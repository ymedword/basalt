package app

// 本文件是 MCP server 只读依赖诊断（颗粒度文档 §3.1.4 / 契约 D-02 / 约束 C-04）：回答
// "这条**已发布**配置按真实启动方式能否定位、env 键是否就绪、是否待重启、有哪些已知
// 边界"。诊断与启动同源——命令构造复用 `tool.NewMCPCommand`（启动用的同一构造器），
// 只读 `cmd.Err` / `cmd.Path` / `os.Stat`，**不** `Start`、**不**连接、**不**写操作行、
// **不**改 published / applied。诊断与测试是两条独立轴：`unconfirmed` 不预判测试结论。
// The read-only MCP dependency diagnosis: the same constructor as launch, stat-only
// checks, zero processes, zero connections, zero operation rows, and no pointer move.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// 三态与原因字面量逐字对齐契约 D-02；`not_applicable` 只在 http（无命令）的内部映射
// 出现，不落 JSON，因此这里不声明。
// State and reason literals mirror contract D-02; not_applicable never reaches JSON.
const (
	mcpDiagStateLocated     = "located"
	mcpDiagStateNotFound    = "not_found"
	mcpDiagStateUnconfirmed = "unconfirmed"

	mcpDiagReasonLookPath     = "lookpath"
	mcpDiagReasonAbsolute     = "absolute"
	mcpDiagReasonNotFound     = "not_found"
	mcpDiagReasonRelativePath = "relative_path"
	mcpDiagReasonScript       = "script"
	mcpDiagReasonPermission   = "permission"
	mcpDiagReasonUnknown      = "unknown"
)

// 诊断 warnings 的固定 code 集合（D-10 的第二套，与目录集合各自固定、**不合并**）。
// 常量是码表下标而不是裸字符串：字面量只在 `mcpDiagnosisWarningCodes` 出现一次，
// append 处只能引用已登记的名字，拼错名字编译不过，集合也不会漂移。
// 前缀必须留 `mcpDiagWarn`：gosec G101 的凭据名正则是
// `(?i)passwd|pass|password|pwd|secret|token|pw|apiKey|bearer|cred`，旧的 `mcpWarn…`
// 含子串 `cpW`（命中 `pw`），配上下面的高熵 code 字面量会被误报成硬编码凭据；
// 改名只是避开误报，code 字面量本身一字未动（契约固定在 D-10）。
// The prefix stays mcpDiagWarn because gosec's G101 name regex contains a bare `pw`
// alternative, which the older mcpWarn… prefix matched by accident.
// The fixed diagnosis warning codes (D-10), separate from the catalog set. Names are
// indices into one table so every literal lives in a single place.
const (
	mcpDiagWarnStdioChildEgress = iota
	mcpDiagWarnRemoteDownstream
	mcpDiagWarnArgsPlaceholders
	mcpDiagWarnPackageEntry
	mcpDiagWarnHTTPAuthHeader
	mcpDiagWarnEnvGateProcessLoc
)

// mcpDiagnosisWarningCodes 是上面六个常量的码表，下标与常量声明一一对应。
// The code table indexed by the constants above.
var mcpDiagnosisWarningCodes = [...]string{
	mcpDiagWarnStdioChildEgress:  "stdio_child_egress_unconstrained",
	mcpDiagWarnRemoteDownstream:  "remote_mcp_downstream_unconstrained",
	mcpDiagWarnArgsPlaceholders:  "args_placeholders_not_resolved",
	mcpDiagWarnPackageEntry:      "package_entry_unverified",
	mcpDiagWarnHTTPAuthHeader:    "http_auth_header_unsupported",
	mcpDiagWarnEnvGateProcessLoc: "env_gate_process_local",
}

// mcpPackageRunnerCommands 是"包 / 脚本运行器"：能定位运行器**不**证明目标包已安装，
// 因此诊断固定追加 `package_entry_unverified`（C-04：不得把 Node 存在推断为包已安装）。
// Package runners: locating one proves nothing about the package, hence the warning.
var mcpPackageRunnerCommands = map[string]bool{
	"node": true, "node.exe": true,
	"npm": true, "npm.cmd": true,
	"npx": true, "npx.cmd": true,
	"python": true, "python.exe": true,
	"python3": true, "python3.exe": true,
	"uv": true, "uv.exe": true,
	"uvx": true, "uvx.exe": true,
	"bun": true, "bun.exe": true,
	"deno": true, "deno.exe": true,
}

// DiagnoseMCPServer 回答一条已发布 MCP server 的只读诊断：读发布指针与发布版本 →
// 解析条目 → 命令三态 / env 布尔 / 待重启 / 固定 warnings，全程零进程、零操作行。
// `actor` 只为与写面端口保持同一签名（管理认证在 web 层完成），诊断不落任何操作行。
// DiagnoseMCPServer answers the read-only diagnosis; actor exists only to keep the port
// signature uniform, and no operation row is ever written here.
func (s *ResourceAdmin) DiagnoseMCPServer(ctx context.Context, actor core.Actor,
	name string,
) (core.MCPServerDiagnosis, error) {
	if err := validateResourceID(name); err != nil {
		return core.MCPServerDiagnosis{}, &core.InvalidInputError{Tool: "diagnostics", Reason: err.Error()}
	}

	unlockResource := s.resourceLocks.lock(resourceLockKey(core.ResourceMCPServer, name))
	defer unlockResource()

	pointer, err := s.activePointer(ctx, core.ResourceMCPServer, name)
	if err != nil {
		return core.MCPServerDiagnosis{}, err
	}
	published, err := s.store.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return core.MCPServerDiagnosis{}, fmt.Errorf("读取 MCP server %s 的发布版本: %w", name, err)
	}
	cfg, err := workspace.ParseMCPServerBlock([]byte(published.DefinitionYAML))
	if err != nil {
		return core.MCPServerDiagnosis{}, fmt.Errorf("读取 MCP server %s 的已发布定义: %w", name, err)
	}
	if cfg.Name != name {
		return core.MCPServerDiagnosis{}, fmt.Errorf(
			"MCP server %s 的发布版本条目名 %q 与资源标识不一致", name, cfg.Name)
	}

	// 引用轴与 `/resources/{kind}/{id}/references` 同源：读失败就报错，不退回"没有
	// 引用"——引用视图是删除闸门的排障证据，缺证据不得伪装成空集。
	// The references axis shares the resource view's source; a read failure is an
	// error, never an empty list.
	references, err := s.agentReferences(core.ResourceMCPServer, name)
	if err != nil {
		return core.MCPServerDiagnosis{}, err
	}
	// 依赖轴只在装配了运行时探测面时回答：nil 表示"没有可核对的运行时目录"，视图
	// 输出 `null`，不猜默认位置、也不把未登记组件答成就绪。
	var dependency *core.MCPDependencyDiagnosis
	if s.mcpDependencies != nil {
		dependency = s.mcpDependencies(cfg)
	}

	transport := mcpTransport(cfg.Transport)
	diagnosis := core.MCPServerDiagnosis{
		Name:               name,
		Transport:          transport,
		PublishedVersionID: pointer.PublishedVersionID,
		AppliedVersionID:   derefVersionID(pointer.AppliedVersionID),
		PendingRestart:     mcpRestartPending(pointer),
		Env:                diagnoseMCPEnv(cfg, os.LookupEnv),
		Dependency:         dependency,
		References:         core.MCPReferencesDiagnosis{Count: len(references), Agents: references},
		Warnings:           mcpDiagnosisWarnings(cfg, transport),
		CheckedAt:          s.now().UTC(),
	}
	// http 没有启动命令：`command` 按 D-02 输出 null，而不是给一个"不适用"的对象。
	if transport == mcpTransportStdio {
		command := s.mcpCommandDiagnosis(cfg)
		diagnosis.Command = &command
	}
	return diagnosis, nil
}

// mcpCommandDiagnosis 取命令三态。装配缺失不得伪装成"已就绪"：三态里只有
// `unconfirmed` 是诚实的答案，同时留一条 WARN 让装配错误可见。
// A missing diagnoser never fakes a positive verdict; it degrades to unconfirmed.
func (s *ResourceAdmin) mcpCommandDiagnosis(cfg core.MCPServerConfig) core.MCPCommandDiagnosis {
	if s.diagnoseMCP == nil {
		s.logger.Warn("MCP 诊断构造器未装配，命令定位无法确认", "resource", cfg.Name)
		return mcpDiagUnconfirmed(mcpDiagReasonUnknown)
	}
	return s.diagnoseMCP(cfg)
}

// diagnoseMCPEnv 只回"键是否就绪"的布尔，判据与装配期 / 保存闸门同口径（
// `resolveMCPEnvSlot`）：读的是**占位符里的变量名**（`${VAR}` 的 `VAR`），不是 env 的
// 键名——两者允许不同名，按键名查会答出与闸门相反的结论。按 key 升序输出（契约 D-02），
// 不回值、不回占位表达式。
// diagnoseMCPEnv reports presence only, sorted by key; it looks up the ${VAR} name,
// exactly like the gate, because the key and the variable may differ.
func diagnoseMCPEnv(cfg core.MCPServerConfig, lookup func(string) (string, bool)) []core.MCPEnvDiagnosis {
	keys := make([]string, 0, len(cfg.Env))
	for key := range cfg.Env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]core.MCPEnvDiagnosis, 0, len(keys))
	for _, key := range keys {
		// 解析器只接受整值 `${VAR}`，`MCPSlotPlaceholder` 理论上恒成；万一存量文件
		// 绕过了校验，退回键名查询而不是报"未就绪"或"就绪"这种未经证明的结论。
		varName, ok := core.MCPSlotPlaceholder(cfg, key)
		if !ok {
			varName = key
		}
		value, found := lookup(varName)
		out = append(out, core.MCPEnvDiagnosis{Key: key, Set: found && value != ""})
	}
	return out
}

// mcpDiagnosisWarnings 按 D-10 的固定顺序追加**按适用**的 code，不拼自由文本：
// stdio 子进程出网无约束 / http 下游与认证头边界 / args 占位不解析 / 包运行器不代表
// 包已装 / 保存期 env 闸门只证明本进程环境。
// The warnings follow D-10's fixed order, filtered by applicability, never free text.
func mcpDiagnosisWarnings(cfg core.MCPServerConfig, transport string) []string {
	warnings := make([]string, 0, 3)
	if transport == mcpTransportStdio {
		warnings = append(warnings, mcpDiagnosisWarningCodes[mcpDiagWarnStdioChildEgress])
	} else {
		warnings = append(warnings, mcpDiagnosisWarningCodes[mcpDiagWarnRemoteDownstream])
	}
	if slices.ContainsFunc(cfg.Args, func(arg string) bool { return strings.Contains(arg, "${") }) {
		warnings = append(warnings, mcpDiagnosisWarningCodes[mcpDiagWarnArgsPlaceholders])
	}
	if mcpPackageRunnerCommands[strings.ToLower(filepath.Base(strings.TrimSpace(cfg.Command)))] {
		warnings = append(warnings, mcpDiagnosisWarningCodes[mcpDiagWarnPackageEntry])
	}
	if transport == mcpTransportHTTP {
		warnings = append(warnings, mcpDiagnosisWarningCodes[mcpDiagWarnHTTPAuthHeader])
	}
	if len(cfg.Env) > 0 {
		warnings = append(warnings, mcpDiagnosisWarningCodes[mcpDiagWarnEnvGateProcessLoc])
	}
	return warnings
}

// diagnoseMCPCommand 是与启动同源的命令诊断（契约 C-04）：`command` 只做形状判定
// （脚本包装 / 相对入口），其余一律交给 `tool.NewMCPCommand`——启动与诊断共用同一
// 构造器，**不**另写近似解析器；本函数只读 `cmd.Err` / `cmd.Path` / `os.Stat`，绝不
// `Start`。三态语义：`located` 只给能证明可启动的情形，`not_found` 只给两条证据
// （构造器 ErrNotFound / os.Stat 明确不存在），其余无法证明的一律 `unconfirmed`。
// diagnoseMCPCommand shares the launch constructor and never starts anything; only
// located and not_found are evidence-backed states, everything else is unconfirmed.
func diagnoseMCPCommand(spec core.MCPServerConfig, root string, envPassthrough []string,
) core.MCPCommandDiagnosis {
	command := strings.TrimSpace(spec.Command)
	if command == "" {
		return mcpDiagUnconfirmed(mcpDiagReasonUnknown)
	}
	// `.cmd` / `.bat` 是脚本包装：直接启动不可证明，**不**默认 `cmd.exe /c`（§2.5.12）。
	if mcpCommandIsScript(command) {
		return mcpDiagUnconfirmed(mcpDiagReasonScript)
	}
	// 含分隔符的相对入口：实际 Start 用 `cmd.Dir` 解析，不启动就无法证明（§2.5.12）。
	if !filepath.IsAbs(command) && strings.ContainsAny(command, `/\`) {
		return mcpDiagUnconfirmed(mcpDiagReasonRelativePath)
	}
	cmd, err := tool.NewMCPCommand(spec, root, envPassthrough)
	if err != nil {
		return mcpDiagUnconfirmed(mcpDiagReasonUnknown)
	}
	if filepath.IsAbs(command) {
		return diagnoseAbsoluteMCPCommand(cmd)
	}
	return diagnoseLookPathMCPCommand(cmd)
}

// diagnoseLookPathMCPCommand 覆盖裸命令名：解析结果由构造器里的 LookPath 给出。
// LookPath 可能把裸名解析成 `.cmd`（Windows `npx` → `npx.cmd`）——能定位不等于能
// 启动，脚本包装仍然只能算 unconfirmed。
// Bare names: the constructor's LookPath decides, but a resolved script shim still
// stays unconfirmed.
func diagnoseLookPathMCPCommand(cmd *exec.Cmd) core.MCPCommandDiagnosis {
	if cmd.Err != nil {
		return classifyMCPCommandError(cmd.Err)
	}
	if mcpCommandIsScript(cmd.Path) {
		return mcpDiagUnconfirmed(mcpDiagReasonScript)
	}
	return mcpDiagLocated(mcpDiagReasonLookPath, cmd.Path)
}

// diagnoseAbsoluteMCPCommand 覆盖绝对路径：存在性 / 权限由 `os.Stat` 只读检查
// （§2.5.12）。绝对路径下构造器**不做**查找（`exec.Command` 只对裸名调 LookPath；
// Windows 的扩展名补全发生在 `Start`），因此 `cmd.Err == nil` 不构成"存在"的证据，
// 反过来 `os.Stat` 明确不存在本身就是 C-04 允许的 `not_found` 证据。Windows 的
// LookPath 会把"目录"折叠成 ErrNotExist——这也是必须落到 os.Stat 再判的原因。
// Absolute paths: exec.Command never looks anything up here, so os.Stat is the only
// evidence, and a clear not-exist is exactly C-04's second allowed not_found proof.
func diagnoseAbsoluteMCPCommand(cmd *exec.Cmd) core.MCPCommandDiagnosis {
	info, statErr := os.Stat(cmd.Path)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrPermission) {
			return mcpDiagUnconfirmed(mcpDiagReasonPermission)
		}
		if errors.Is(statErr, fs.ErrNotExist) {
			return mcpDiagNotFound()
		}
		return mcpDiagUnconfirmed(mcpDiagReasonUnknown)
	}
	switch {
	case info.IsDir():
		// 目录不是可执行文件：存在，但不可能按现行方式启动。
		return mcpDiagUnconfirmed(mcpDiagReasonPermission)
	case !info.Mode().IsRegular():
		return mcpDiagUnconfirmed(mcpDiagReasonUnknown)
	case !mcpCommandStartable(info):
		return mcpDiagUnconfirmed(mcpDiagReasonPermission)
	case cmd.Err != nil:
		// 文件在、构造器却没解析成功（如 Windows 上无扩展名的文件）：无法证明可启动。
		return mcpDiagUnconfirmed(mcpDiagReasonUnknown)
	}
	return mcpDiagLocated(mcpDiagReasonAbsolute, cmd.Path)
}

// classifyMCPCommandError 只按错误类型分类，**不**解析文案：权限不足与"找不到"是
// 两种不同证据，其余一律 unknown（unconfirmed）。
// Classify by error type only; anything else stays unknown.
func classifyMCPCommandError(err error) core.MCPCommandDiagnosis {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return mcpDiagUnconfirmed(mcpDiagReasonPermission)
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return mcpDiagNotFound()
	default:
		return mcpDiagUnconfirmed(mcpDiagReasonUnknown)
	}
}

// mcpCommandStartable 判断一个已存在的普通文件能否按现行方式直接启动：Windows 的
// 可执行性不由 mode 位表达（PE 内容与 ACL 无法只读证明），按卡片 §2.5.12 以"存在且
// 不是目录"为准；Unix 沿用它自己的判据——一个执行位都没有时 Start 必然失败。
// Windows executability is not expressible in FileMode; Unix needs an execute bit.
func mcpCommandStartable(info os.FileInfo) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

// mcpCommandIsScript 判定脚本包装后缀（`.cmd` / `.bat`，大小写不敏感），规则与平台
// 无关：脚本入口只能按 unconfirmed 处理，诊断不替它拼 `cmd.exe /c`。
// Script wrappers are rejected by suffix on every platform.
func mcpCommandIsScript(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".cmd", ".bat":
		return true
	default:
		return false
	}
}

// mcpDiagLocated / mcpDiagUnconfirmed / mcpDiagNotFound 收敛三态拼装，避免各分支
// 各自决定 Path 有无。
// Small constructor helpers keep the three states consistent.
func mcpDiagLocated(reason, path string) core.MCPCommandDiagnosis {
	return core.MCPCommandDiagnosis{State: mcpDiagStateLocated, Path: path, Reason: reason}
}

func mcpDiagUnconfirmed(reason string) core.MCPCommandDiagnosis {
	return core.MCPCommandDiagnosis{State: mcpDiagStateUnconfirmed, Reason: reason}
}

func mcpDiagNotFound() core.MCPCommandDiagnosis {
	return core.MCPCommandDiagnosis{State: mcpDiagStateNotFound, Reason: mcpDiagReasonNotFound}
}

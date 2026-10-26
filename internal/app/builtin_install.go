package app

// 本文件是 14D 的「开箱即用」安装器（设计 §2.5.1 / §4.1 第 2 步，T111–T113）：
//
//  1. **新实例首次启动**：装五个内置 Agent（生命周期由随后的启动基线写成 `disabled`）、
//     生成样例库与 DBHub 只读配置、写入**免凭据**的三条 MCP 条目（docs-readonly /
//     DBHub / fetch）；写下的条目交给紧随其后的启动基线发布（本段在解析
//     `mcp_servers.yaml` 之前跑，因此本次启动即生效）。
//  2. **凭据就绪**：进程环境里出现显式登记的 GitHub 凭据变量（`GITHUB_PERSONAL_ACCESS_TOKEN`
//     或运维常用的 `GITHUB_TOKEN`）时补写 `github` 条目。条目声明的是 env 来源，所以
//     "变量在"就等于"来源可满足"——与保存期 env 闸门同一判据，不会写出一条注定发布
//     失败、或发布后连不上的配置。
//  3. **存量实例回填**：升级上来的工作区没有标记文件时，安装器**不装 Agent**，只在
//     组件真实存在时**追加**缺的 MCP 条目——用户不必为了接入 MCP 重置工作区，也不会
//     被自动塞进五个 Agent 或一条注定起不来的配置。
//
// 三条纪律：
//   - **只追加**：Agent 目录与 MCP 条目已存在即逐字节保留；安装器不是第二条覆盖路径。
//   - **写过就不再写**：`control/builtin-openbox.json` 记录安装器写过哪些条目与 Agent，
//     因此运营删掉的条目**不会**在下次启动被加回来（删除语义归管理面，安装器不越权）。
//   - **存量实例只回填**：Agent 安装不归本段（标记里的 `agents_skipped` 固定这一点）；
//     条目按组件存在性过滤，升级路径的其他变更仍按 Operations 的升级影响清单走显式命令。
//
// The builtin open-box installer prepares the five agents and the credential-free MCP
// entries on a new instance's first start, appends the credential-gated entry once its
// environment variable exists, and backfills ready-only MCP entries on a legacy instance.
// A marker file records what the installer wrote, so an operator's deletion is not undone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/builtin"
)

const (
	// builtinOpenBoxSchema 是标记文件的版本：结构变化时旧标记按"不认识"处理（跳过，
	// 不猜着改写），而不是把未知字段当成当前语义。
	// builtinOpenBoxSchema is the marker's schema version.
	builtinOpenBoxSchema = 1

	// builtinOpenBoxFileName 落在 `<工作区>/control/` 下——与生命周期状态文件同一个受保护
	// 管理目录，不进 `agents/` 也不进 `archive/`（后者会被当成用户内容清扫）。
	// The marker lives under the protected control directory beside the lifecycle files.
	builtinOpenBoxFileName = "builtin-openbox.json"
)

// builtinOpenBoxState 是安装器**已经做过什么**的事实记录。它不记录凭据，也不记录
// 组件的版本——版本在发行布局里，标记只回答"这个名字由本安装器写过"。
// The marker records which agents and entries the installer already wrote.
type builtinOpenBoxState struct {
	SchemaVersion   int      `json:"schema_version"`
	InstalledAt     string   `json:"installed_at,omitempty"`
	AgentsInstalled bool     `json:"agents_installed"`
	// AgentsSkipped 记录"这个存量实例的 Agent 不归安装器管"。没有它时，回填写下的
	// 标记（AgentsInstalled=false）会让下一次启动把五个内置 Agent 补装进老工作区——
	// 升级一次就多出五只 Agent 是明确禁止的副作用。
	// AgentsSkipped keeps a backfilled legacy marker from installing agents on a later start.
	AgentsSkipped bool     `json:"agents_skipped,omitempty"`
	MCPEntries    []string `json:"mcp_entries,omitempty"`
}

// builtinOpenBoxDeps 是一次安装的显式输入（宪法 VII：不塞 context、不读包级变量）。
// builtinOpenBoxDeps carries the installer's explicit inputs.
type builtinOpenBoxDeps struct {
	WorkspaceRoot string
	// RuntimeRoot 是 `<程序目录>/runtime`（设计 §2.1）；空串表示算不出来，此时不写任何
	// MCP 条目——猜一个相对路径写进配置，比缺条目更难查。
	// RuntimeRoot is the program directory's runtime; empty means "do not guess".
	RuntimeRoot string
	// Fresh 是本次启动前"库与实例名册都不存在"的唯一口径（T089）。
	// Fresh is the single new-instance verdict decided before the configuration loads.
	Fresh     bool
	LookupEnv func(string) (string, bool)
	Now       func() time.Time
	Logger    *slog.Logger
}

// builtinOpenBoxReport 汇总本次做了什么；只用于装配日志与测试断言。
// builtinOpenBoxReport summarises one run for the assembly log and tests.
type builtinOpenBoxReport struct {
	AgentsInstalled []string
	AgentsKept      []string
	MCPWritten      []string
	MCPPresent      []string
	Missing         []string
	SampleDB        builtin.SampleDB
	DBHubConfig     string
	// Backfilled 表示本次运行服务的是存量实例：只回填组件已就位的 MCP 条目，
	// Agent 与安装标记的 Agent 段都不动。
	// Backfilled marks a legacy-instance run that only backfills ready MCP entries.
	Backfilled bool
	Skipped    string
}

// builtinOpenBoxControlDir 是工作区根下存放安装标记的目录名。
// builtinOpenBoxControlDir is the control directory under the workspace root.
const builtinOpenBoxControlDir = "control"

// builtinOpenBoxStatePath 返回标记文件路径。
// builtinOpenBoxStatePath returns the marker path.
func builtinOpenBoxStatePath(wsRoot string) string {
	return filepath.Join(wsRoot, builtinOpenBoxControlDir, builtinOpenBoxFileName)
}

// readBuiltinOpenBoxState 读标记；present=false 表示文件不存在。
// 标记损坏或版本不认识时返回 corrupt=true 且**不改写**：宁可本段什么都不做（日志点名），
// 也不按"没写过"处理——那会把运营删掉的条目重新加回文件里。
// A corrupt or unknown-version marker makes the installer a no-op instead of a rewriter.
func readBuiltinOpenBoxState(wsRoot string) (state builtinOpenBoxState, present, corrupt bool, err error) {
	path := builtinOpenBoxStatePath(wsRoot)
	// 读经工作区根句柄：目录名与文件名都是常量，能读到的只能是工作区根下这一份。
	// The read goes through a root handle on the workspace, so only the default path is reachable.
	root, rootErr := os.OpenRoot(wsRoot)
	if errors.Is(rootErr, fs.ErrNotExist) {
		return builtinOpenBoxState{}, false, false, nil
	}
	if rootErr != nil {
		return builtinOpenBoxState{}, false, false, fmt.Errorf("读取安装标记 %s：%w", path, rootErr)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(filepath.Join(builtinOpenBoxControlDir, builtinOpenBoxFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return builtinOpenBoxState{}, false, false, nil
	}
	if err != nil {
		return builtinOpenBoxState{}, false, false, fmt.Errorf("读取安装标记 %s：%w", path, err)
	}
	if unmarshalErr := json.Unmarshal(data, &state); unmarshalErr != nil {
		return builtinOpenBoxState{}, true, true, nil
	}
	if state.SchemaVersion != builtinOpenBoxSchema {
		return builtinOpenBoxState{}, true, true, nil
	}
	return state, true, false, nil
}

// writeBuiltinOpenBoxState 写标记（0600，与工作区里其它系统生成物同档权限）。
// writeBuiltinOpenBoxState writes the marker with the workspace's generated-file mode.
func writeBuiltinOpenBoxState(wsRoot string, state builtinOpenBoxState) error {
	dir := filepath.Dir(builtinOpenBoxStatePath(wsRoot))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("创建安装标记目录 %s：%w", dir, err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化安装标记：%w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(builtinOpenBoxStatePath(wsRoot), data, 0o600); err != nil {
		return fmt.Errorf("写出安装标记 %s：%w", builtinOpenBoxStatePath(wsRoot), err)
	}
	return nil
}

// prepareBuiltinOpenBox 执行一次安装（幂等：缺什么补什么，已有的逐字节保留）。
// 返回的错误会拒绝本次启动——写不动工作区意味着后面的基线同样写不动，提前失败比带着
// 半套定义开服更诚实；样例库缺解释器**不**算错误，它记为 pending 并在报告中如实呈现。
// prepareBuiltinOpenBox is idempotent; only workspace write failures refuse the start.
func prepareBuiltinOpenBox(ctx context.Context, deps builtinOpenBoxDeps) (builtinOpenBoxReport, error) {
	var report builtinOpenBoxReport
	state, present, corrupt, err := readBuiltinOpenBoxState(deps.WorkspaceRoot)
	if err != nil {
		return report, err
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if corrupt {
		report.Skipped = "安装标记损坏或版本不认识"
		logger.Warn("内置安装器跳过：标记文件损坏或版本不认识——删除该文件后下次启动会重新准备",
			"path", builtinOpenBoxStatePath(deps.WorkspaceRoot))
		return report, nil
	}
	// 存量实例（库在、名册不在、没有安装标记）：Agent 不归安装器，但凭据无关的 MCP
	// 条目要在组件就位时回填——用户不该为了接入 MCP 重置整个工作区。回填只追加：
	// 已有条目逐字节保留，运营删掉的条目不会在下次启动被加回来（标记记录写过哪些）。
	// A legacy instance gets a component-gated, append-only MCP backfill; agents are untouched.
	established := !present && !deps.Fresh
	report.Backfilled = established

	// 标记**先落盘再动手**：写到一半崩掉时下次启动仍会进本段（AgentsInstalled=false），
	// 已装好的目录按 kept 保留，缺的补齐——半安装因此可以自愈，而不是永久卡住。
	// The marker lands before the work so a half-finished install heals on the next start.
	state.SchemaVersion = builtinOpenBoxSchema
	if state.InstalledAt == "" {
		state.InstalledAt = deps.Now().UTC().Format(time.RFC3339)
	}
	if established {
		// 回填标记同时声明"Agent 不由安装器接管"：否则下一次启动会把五个内置 Agent
		// 补装进老工作区。
		state.AgentsSkipped = true
	}
	if err := writeBuiltinOpenBoxState(deps.WorkspaceRoot, state); err != nil {
		return report, err
	}

	if !state.AgentsInstalled && !state.AgentsSkipped {
		results, err := builtin.Install(deps.WorkspaceRoot, false)
		if err != nil {
			return report, err
		}
		for _, result := range results {
			if result.State == builtin.StateKept {
				report.AgentsKept = append(report.AgentsKept, result.Name)
				continue
			}
			report.AgentsInstalled = append(report.AgentsInstalled, result.Name)
		}
		state.AgentsInstalled = true
		if err := writeBuiltinOpenBoxState(deps.WorkspaceRoot, state); err != nil {
			return report, err
		}
	}

	if deps.RuntimeRoot == "" {
		report.Skipped = "取不到程序目录，跳过 MCP 条目写入"
		logger.Warn("内置安装器跳过 MCP 条目：取不到程序目录，无法算出运行时布局")
		return report, nil
	}
	paths := builtinRuntimePaths(deps.RuntimeRoot, deps.WorkspaceRoot)
	report.Missing = missingBuiltinComponents(paths)
	// 存量实例只在组件真实存在时才回填：写一条注定起不来的条目，会让一个本来正常的
	// 工作区在每次启动时多一条连接失败。
	requireComponents := state.AgentsSkipped

	// 样例库与 DBHub 只读配置：缺什么补什么。缺 Python 时样例库记 pending——它是业务
	// Demo 的输入，不是服务启动的前提（T115 的启动隔离）。存量实例仅在 DBHub 组件
	// 已就位时准备，否则这两份文件用不上，写进老工作区只是多两处垃圾。
	prepareDemo := !requireComponents || (componentPresent(paths.Node) && componentPresent(paths.DBHubEntry))
	if prepareDemo {
		sample, err := builtin.EnsureSampleDB(ctx, deps.WorkspaceRoot, false)
		if err != nil {
			return report, err
		}
		report.SampleDB = sample
		if sample.State != builtin.StatePending || !requireComponents {
			configPath, _, configErr := builtin.EnsureDBHubConfig(
				deps.WorkspaceRoot, builtin.SampleDBPath(deps.WorkspaceRoot), false)
			if configErr != nil {
				return report, configErr
			}
			report.DBHubConfig = configPath
		}
	}

	entries := builtin.CredentialFreeEntries(paths)
	if requireComponents {
		entries = readyEntries(entries, report.SampleDB)
	}
	result, err := builtin.EnsureMCP(deps.WorkspaceRoot, entries)
	if err != nil {
		return report, err
	}
	report.MCPWritten = append(report.MCPWritten, result.Written...)
	report.MCPPresent = append(report.MCPPresent, result.Present...)
	for _, name := range entryNames(entries) {
		if slices.Contains(result.Present, name) {
			state = recordBuiltinMCPEntry(state, name)
		}
	}

	// 凭据就绪才写需要凭据的条目：条目声明 env 来源，"变量在"即"来源可满足"。
	// 变量名只取显式登记的同义表（本名优先，其次 gh CLI 的 GITHUB_TOKEN），不猜名字。
	github := builtin.GitHubEntry(paths)
	if !slices.Contains(state.MCPEntries, github.Name) {
		if tokenVar, ok := builtin.GitHubTokenSource(deps.LookupEnv); ok {
			github = builtin.GitHubEntryWithToken(paths, tokenVar)
			if !requireComponents || entryComponentsReady(github) {
				githubResult, githubErr := builtin.EnsureMCP(deps.WorkspaceRoot, []builtin.Entry{github})
				if githubErr != nil {
					return report, githubErr
				}
				report.MCPWritten = append(report.MCPWritten, githubResult.Written...)
				report.MCPPresent = append(report.MCPPresent, githubResult.Present...)
				if slices.Contains(githubResult.Present, github.Name) {
					state = recordBuiltinMCPEntry(state, github.Name)
				}
			}
		}
	}

	if err := writeBuiltinOpenBoxState(deps.WorkspaceRoot, state); err != nil {
		return report, err
	}
	if len(report.Missing) > 0 {
		// 缺件**不**改写配置、也**不**拒绝启动：配置里的路径是发行布局的期望值，
		// 缺什么由依赖轴（页面）与这行日志点名。
		logger.Warn("内置组件未在运行时目录就位——对应 MCP 条目仍按发行布局写入，依赖轴会报 missing",
			"missing", report.Missing, "runtime", deps.RuntimeRoot)
	}
	logger.Info("内置安装器完成",
		"agents_installed", len(report.AgentsInstalled), "agents_kept", len(report.AgentsKept),
		"mcp_written", report.MCPWritten, "sample_db", report.SampleDB.State,
		"backfilled", report.Backfilled)
	return report, nil
}

// readyEntries 过滤出**组件已在盘上**的条目（存量实例回填用）。dbhub 另判样例库：
// 配置在、库没生成出来时，DBHub 连上也查不了任何表——宁可少写一条并让依赖轴说话。
func readyEntries(entries []builtin.Entry, sample builtin.SampleDB) []builtin.Entry {
	ready := make([]builtin.Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Name == "dbhub" && sample.State == builtin.StatePending {
			continue
		}
		if !entryComponentsReady(entry) {
			continue
		}
		ready = append(ready, entry)
	}
	return ready
}

// entryComponentsReady 判断条目引用的每个文件路径都已在盘上：命令与参数里的路径
// （解释器、入口脚本、配置文件）逐个 `os.Stat`；不像路径的参数（`--transport`、
// `stdio`）跳过。命令为空一律判未就绪。
func entryComponentsReady(entry builtin.Entry) bool {
	if strings.TrimSpace(entry.Command) == "" {
		return false
	}
	values := append([]string{entry.Command}, entry.Args...)
	for _, value := range values {
		if !looksLikeFilesystemPath(value) {
			continue
		}
		if !componentPresent(value) {
			return false
		}
	}
	return true
}

// looksLikeFilesystemPath 判断一个参数是否在指路径（绝对路径或含分隔符）。
func looksLikeFilesystemPath(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	if filepath.IsAbs(value) {
		return true
	}
	return strings.ContainsAny(value, `/\`)
}

// componentPresent 报告路径非空且存在（存在性只信 `os.Stat`，与依赖轴同口径）。
func componentPresent(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// recordBuiltinMCPEntry 把一条"由安装器写过"的条目记进标记（去重、按写入顺序）。
// recordBuiltinMCPEntry records an installer-written entry once.
func recordBuiltinMCPEntry(state builtinOpenBoxState, name string) builtinOpenBoxState {
	if slices.Contains(state.MCPEntries, name) {
		return state
	}
	state.MCPEntries = append(state.MCPEntries, name)
	return state
}

// entryNames 返回条目名（标记里只记安装器管的那几条）。
// entryNames lists the managed entry names of one batch.
func entryNames(entries []builtin.Entry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

// builtinRuntimePaths 按设计 §2.1 的安装布局算出内置条目的**期望**启动路径。组件版本取
// `mcpRuntimeComponents`（依赖轴同一张清单）——两处各钉一份版本，迟早对不上。
// 每个位置都给"优先候选 + 兼容候选"，取第一个存在的；都不存在时返回首选，让配置与
// 齐全时逐字节相同。
// builtinRuntimePaths computes the expected launch paths of the bundled components.
func builtinRuntimePaths(runtimeRoot, wsRoot string) builtin.Paths {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	componentEntry := func(name string, candidates ...string) string {
		base := filepath.Join(runtimeRoot, "mcp", name, mcpRuntimeComponents[name])
		paths := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			paths = append(paths, filepath.Join(base, candidate))
		}
		return firstExisting(paths...)
	}
	nodeRoot := filepath.Join(runtimeRoot, "node")
	pythonRoot := filepath.Join(runtimeRoot, "python")
	return builtin.Paths{
		DocsReadOnly: componentEntry("docs-readonly", "docs-readonly"+suffix, "docs-readonly"),
		// Node 的两种发行形态：Windows 包放 `runtime/node/node.exe`，官方 Unix tarball 是
		// `bin/node`；`node`（无扩展名）留给手工摊平的安装方式。
		Node: firstExisting(
			filepath.Join(nodeRoot, "node"+suffix),
			filepath.Join(nodeRoot, "bin", "node"),
			filepath.Join(nodeRoot, "node"),
		),
		DBHubEntry:  componentEntry("dbhub", filepath.Join("node_modules", "@bytebase", "dbhub", "dist", "index.js")),
		DBHubConfig: builtin.DBHubConfigPath(wsRoot),
		GitHub:      componentEntry("github-mcp-server", "github-mcp-server"+suffix, "github-mcp-server"),
		// Python 与 Fetch 同属随包布局：Windows 发行包放 `runtime/python/python.exe`，
		// Unix 包（官方 tarball 展开或 python-build-standalone）是 `bin/python3`。
		Python: firstExisting(
			filepath.Join(pythonRoot, "python"+suffix),
			filepath.Join(pythonRoot, "bin", "python3"),
			filepath.Join(pythonRoot, "bin", "python"),
			filepath.Join(pythonRoot, "python"),
		),
		// fetch 的启动脚本落在版本目录里：依赖轴按 `mcp/fetch/<版本>/` 认出组件。
		Fetch: componentEntry("fetch", "run_fetch.py"),
	}
}

// firstExisting 返回第一个存在的候选；都不存在时返回第一个（空输入返回空串）。
// 配置因此只取决于发行布局，不取决于本机恰好装了什么。
// firstExisting returns the first candidate that exists, else the first candidate.
func firstExisting(candidates ...string) string {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0]
}

// missingBuiltinComponents 返回启动路径不在盘上的组件名；只用于日志点名，**不**改变
// 写入内容（改配置会让"缺什么"变成一个第二解释）。
// missingBuiltinComponents names the components whose launch path is absent on disk.
func missingBuiltinComponents(paths builtin.Paths) []string {
	candidates := []struct {
		name string
		path string
	}{
		{"docs-readonly", paths.DocsReadOnly},
		{"node", paths.Node},
		{"dbhub", paths.DBHubEntry},
		{"github-mcp-server", paths.GitHub},
		{"python", paths.Python},
		{"fetch", paths.Fetch},
	}
	missing := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.path == "" {
			missing = append(missing, candidate.name)
			continue
		}
		if _, err := os.Stat(candidate.path); err != nil {
			missing = append(missing, candidate.name)
		}
	}
	return missing
}

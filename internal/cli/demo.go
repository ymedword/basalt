package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/builtin"
)

// demo 命令把仓库自带的五个每日 Demo 装进工作区：复制 Agent 目录与技能、生成
// 样例库、写安装清单，并在收尾逐项列出「还差什么」。
//
// 三条边界（用户 2026-10-26 裁决）：
//  1. **不改 basalt init 的职责**——安装是显式命令，init 仍只建骨架与库；
//  2. **不写模型、不写凭据、不自动放宽白名单**——模型留空、凭据只认环境变量，
//     权限变更只打印待确认的片段，由用户自己写入配置；
//  3. **MCP 依赖按固定版本**——版本写在 mcpDep 表里，命令只提示安装与校验，
//     不在运行时替用户装包（装机动作要用户确认）。
//
// The demo command installs the five bundled demos into a workspace. It never
// writes a model, a credential or a permission grant; it only copies assets,
// generates the sample database and reports what the operator still has to do.
const demoSkillsDirName = "skills"

// 工作区内的落盘权限：目录 0o750 与 internal/workspace 的 dirPerm 同口径（企业私有
// 数据不给"其他用户"留读取面）；文件 0o600 更紧一档——安装器不假设同机的别人需要
// 读这些生成物（AGENT.md / 清单 / 样例库 / 导出源码）。
// Workspace tree permissions: 0750 directories (same as internal/workspace) and
// 0600 files, deliberately stricter than the bootstrap files.

// demoSpec / demoSpecs 直接取 internal/builtin 的清单：**五个内置 Agent 是哪五个、
// 各要什么**只在那一处定义，`basalt demo install` 与启动期自动准备不存在第二份清单。
// demoSpec / demoSpecs alias the single list owned by internal/builtin.
type demoSpec = builtin.Agent

var demoSpecs = builtin.Agents()

// mcpDep 是一条固定版本的 MCP 依赖（版本经 npm / GitHub Releases 实测，2026-10-26）。
// mcpDep pins one MCP dependency; versions were measured on 2026-10-26.
type mcpDep struct {
	ID        string
	Kind      string
	Package   string
	Version   string
	Install   string
	Entry     string
	SourceURL string
	EnvKeys   []string
}

// mcpDeps 是安装报告里打印的固定版本表；两个 GitHub Demo 共用同一条 github 依赖。
// mcpDeps is the pinned dependency table; both GitHub demos share one entry.
var mcpDeps = []mcpDep{
	{
		// Install 是模板：安装器把源码导出到工作区后，用实际路径渲染构建命令。
		// Install is a template; the real workspace paths are filled in at report time.
		ID: "docs-readonly", Kind: "go-build", Package: "docs-readonly", Version: "随本仓库源码（纯标准库）",
		Install:   "go -C %s build -o %s .",
		Entry:     "docs-readonly.exe",
		SourceURL: "examples/shared/mcp/docs-readonly",
	},
	{
		ID: "dbhub", Kind: "npm", Package: "@bytebase/dbhub", Version: "1.5.0",
		Install:   "npm install @bytebase/dbhub@1.5.0",
		Entry:     "node_modules/@bytebase/dbhub/dist/index.js",
		SourceURL: "https://github.com/bytebase/dbhub",
	},
	{
		ID: "brave-search", Kind: "npm", Package: "@brave/brave-search-mcp-server", Version: "2.1.4",
		Install:   "npm install @brave/brave-search-mcp-server@2.1.4",
		Entry:     "node_modules/@brave/brave-search-mcp-server/dist/index.js",
		SourceURL: "https://github.com/brave/brave-search-mcp-server",
		EnvKeys:   []string{"BRAVE_API_KEY"},
	},
	{
		ID: "fetch", Kind: "pip", Package: "mcp-server-fetch", Version: "2026.10.24",
		Install:   "pip install mcp-server-fetch==2026.10.24",
		Entry:     "python -m mcp_server_fetch",
		SourceURL: "https://github.com/modelcontextprotocol/servers/tree/main/src/fetch",
	},
	{
		ID: "github", Kind: "binary", Package: "github-mcp-server", Version: "v2.0.3",
		Install:   "从 Releases 下载 github-mcp-server 并放进 PATH",
		Entry:     "github-mcp-server stdio",
		SourceURL: "https://github.com/github/github-mcp-server/releases",
		EnvKeys:   []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
	},
}

// demoCredentialHint 是一条凭据的获取指引：最小权限、有效期 / 费用口径，以及工作区里
// 讲完整步骤的那份 SETUP 的节号。安装器只打印指引，永远不接收、不回显凭据值。
// demoCredentialHint points at the section of the installed SETUP that carries the
// full acquisition steps; the installer never accepts or echoes a credential value.
type demoCredentialHint struct {
	Key     string
	Need    string
	Section string
}

// demoCredentialHints 按环境变量名索引：只打印本次选中 Demo 真正用到的那几条，
// SETUP 路径取自实际选中的 Demo（不再写死某个 Demo 名）。
// demoCredentialHints is keyed by env var; the SETUP path comes from the selected demo.
var demoCredentialHints = map[string]demoCredentialHint{
	"GITHUB_PERSONAL_ACCESS_TOKEN": {
		Key:     "GITHUB_PERSONAL_ACCESS_TOKEN",
		Need:    "细粒度 PAT：Repository access 选 Public repositories（公开只读即可），默认 30 天到期；启动 github-mcp-server 时加 --read-only",
		Section: "第 2 节",
	},
	"BRAVE_API_KEY": {
		Key:     "BRAVE_API_KEY",
		Need:    "Brave Search API 的 Search 计划（按量计费，额度以官方定价页为准）；本机 2026-10-26 无法访问 brave.com 域名",
		Section: "第 3 节",
	},
}

// demoInstallOptions 是命令参数：选择、覆盖与试运行。
// demoInstallOptions carries the selection, overwrite and dry-run switches.
type demoInstallOptions struct {
	Demos  []string
	Force  bool
	DryRun bool
}

// demoManifest 是写到 <workspace>/demos/install.json 的安装清单：装了什么、装到
// 哪里、还差什么。它只记录事实，不记录任何凭据。
// demoManifest records what was installed and what still needs configuring; it
// never records a credential.
type demoManifest struct {
	SchemaVersion int                `json:"schema_version"`
	InstalledAt   string             `json:"installed_at"`
	Demos         []demoManifestItem `json:"demos"`
	// Interpreter 是写进 AGENT.md 脚本步骤的解释器绝对路径（没找到时为空，占位符保留）；
	// InterpreterNote 记录没找到的原因，报告据此给出可执行的下一步。
	// Interpreter is the absolute interpreter path substituted into AGENT.md; the note
	// explains why it is empty when resolution failed.
	Interpreter     string           `json:"interpreter,omitempty"`
	InterpreterNote string           `json:"interpreter_note,omitempty"`
	SampleDB        *demoSampleDB    `json:"sample_db,omitempty"`
	DBHubConfig     string           `json:"dbhub_config,omitempty"`
	DocsLibrary     *demoDocsLibrary `json:"docs_library,omitempty"`
	DocsReadOnly    *demoSourceAsset `json:"docs_readonly,omitempty"`
}

// demoDocsLibrary 记录样例只读文档库的安装结果（`docs-diff` 的输入）。
// demoDocsLibrary records the sample read-only document library.
type demoDocsLibrary struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

// demoSourceAsset 记录随安装导出的自研 MCP 源码（路径 + 状态 + 生成的 go.mod）。
// demoSourceAsset records the exported docs-readonly source and its generated go.mod.
type demoSourceAsset struct {
	Path  string `json:"path"`
	State string `json:"state"`
	GoMod string `json:"go_mod,omitempty"`
}

// demoManifestItem 是单个 Demo 的安装结果；State 描述 Agent 目录，SkillState 单独
// 记录技能目录（两者可能一个新建、一个保留）。
// demoManifestItem records the agent directory state and, separately, the skill one.
type demoManifestItem struct {
	Name       string   `json:"name"`
	State      string   `json:"state"`
	AgentDir   string   `json:"agent_dir"`
	SkillDir   string   `json:"skill_dir,omitempty"`
	SkillState string   `json:"skill_state,omitempty"`
	Pending    []string `json:"pending"`
}

// demoSampleDB 记录样例库的生成结果；State 取 created / kept / pending。
type demoSampleDB struct {
	Path  string `json:"path"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

// 安装状态常量：新增、保留、待生成。取值直接别名 internal/builtin——报告要比较的
// 正是那边返回的字符串，两处各写一份字面量只会在某次改动后悄悄对不上。
// The state constants alias internal/builtin so the report compares the same values.
const (
	demoStateInstalled = builtin.StateInstalled
	demoStateKept      = builtin.StateKept
	demoStatePending   = builtin.StatePending
)

// newDemoCommand 组装 demo 命令组；当前只交付 install。
// newDemoCommand builds the demo command group; only install ships today.
func newDemoCommand(deps Deps) *cobra.Command {
	var opts demoInstallOptions
	demo := &cobra.Command{
		Use:   "demo",
		Short: "示例 Demo 的安装（素材随二进制内置）",
		Long: "示例 Demo 的安装入口：把内置的五份每日 Demo 复制到工作区，生成样例库，\n" +
			"并逐项提示仍需配置的内容。安装不会写入模型、凭据或权限。\n" +
			"Install the bundled demos into the workspace; models, credentials and " +
			"permissions stay untouched.",
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "把内置 Demo 安装到工作区（模型留空、凭据不落盘）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDemoInstall(cmd.Context(), cmd.OutOrStdout(), deps, opts)
		},
	}
	install.Flags().StringSliceVar(&opts.Demos, "demo", nil, "只安装指定 Demo（可重复；缺省为全部五个）")
	install.Flags().BoolVar(&opts.Force, "force", false, "覆盖已存在的 Agent 目录并重建样例库")
	install.Flags().BoolVar(&opts.DryRun, "dry-run", false, "只打印安装计划，不写任何文件")
	demo.AddCommand(install)
	return demo
}

// runDemoInstall 执行一次安装：校验工作区 → 逐个复制 → 生成样例库 → 写清单 → 报告。
// runDemoInstall performs one install run and prints the follow-up checklist.
//
// 失败路径把错误**上抛**（不吞）：cobra 打印可读文案，装配层记 ERROR 并定退出码；
// 命令面不重复打印同一失败。
// Errors travel up to the assembly layer, which owns logging and the exit code.
func runDemoInstall(ctx context.Context, out io.Writer, deps Deps, opts demoInstallOptions) error {
	wsRoot := filepath.Join(deps.WorkDir, workspaceDirName)
	if _, err := os.Stat(filepath.Join(wsRoot, agentsDirName)); err != nil {
		return fmt.Errorf("工作区未初始化：缺少 %s——先执行 basalt init", filepath.Join(wsRoot, agentsDirName))
	}
	selected, err := selectDemoSpecs(opts.Demos)
	if err != nil {
		return err
	}

	if opts.DryRun {
		writeDemoPlan(out, wsRoot, selected)
		return nil
	}

	// 示例脚本的解释器：随包运行时优先（配置模板登记的就是这一串），源码检出回落 PATH。
	// 找不到也不算安装失败——Agent 仍需人工填模型；报告把"缺解释器"如实列为待配置。
	// The interpreter is resolved once and substituted into every installed AGENT.md.
	interpreter, interpreterErr := resolveDemoInterpreter(ctx)
	if interpreterErr != nil {
		interpreter = ""
	}

	manifest := demoManifest{
		SchemaVersion: 1, InstalledAt: time.Now().UTC().Format(time.RFC3339),
		Interpreter: interpreter, InterpreterNote: demoInterpreterNote(interpreterErr),
	}
	for _, spec := range selected {
		item, err := installDemoBundle(spec, wsRoot, opts.Force, interpreter)
		if err != nil {
			return err
		}
		manifest.Demos = append(manifest.Demos, item)
	}

	for _, spec := range selected {
		if !spec.NeedsSample {
			continue
		}
		sample, err := installDemoSampleDB(ctx, wsRoot, opts.Force)
		if err != nil {
			return err
		}
		manifest.SampleDB = &sample
	}

	if needsDemoSample(selected) {
		configPath, err := writeDemoDBHubConfig(wsRoot, demoDBHubSamplePath(wsRoot))
		if err != nil {
			return err
		}
		manifest.DBHubConfig = configPath
	}

	if needsDemoDocs(selected) {
		library, err := installDemoDocsLibrary(wsRoot, opts.Force)
		if err != nil {
			return err
		}
		manifest.DocsLibrary = &library
	}

	if needsDemoSource(selected) {
		source, err := installDemoSource(wsRoot, opts.Force)
		if err != nil {
			return err
		}
		manifest.DocsReadOnly = &source
	}

	if err := writeDemoManifest(wsRoot, manifest); err != nil {
		return err
	}
	writeDemoReport(out, wsRoot, selected, manifest)
	return nil
}

// selectDemoSpecs 把 --demo 的选择解析成 spec 列表：空选择 = 全部；未知名字报错并
// 列出可选值（不静默忽略拼写错误）。
// selectDemoSpecs resolves the selection; unknown names fail loudly.
func selectDemoSpecs(names []string) ([]demoSpec, error) {
	if len(names) == 0 {
		return append([]demoSpec(nil), demoSpecs...), nil
	}
	byName := make(map[string]demoSpec, len(demoSpecs))
	for _, spec := range demoSpecs {
		byName[spec.Name] = spec
	}
	seen := make(map[string]bool, len(names))
	selected := make([]demoSpec, 0, len(names))
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		spec, ok := byName[trimmed]
		if !ok {
			return nil, fmt.Errorf("未知 Demo %q：可选 %s", trimmed, strings.Join(demoNames(), " / "))
		}
		if seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		selected = append(selected, spec)
	}
	return selected, nil
}

// demoNames 返回全部 Demo 名（报告与错误文案共用一份来源）。
// demoNames lists every bundled demo name.
func demoNames() []string {
	names := make([]string, 0, len(demoSpecs))
	for _, spec := range demoSpecs {
		names = append(names, spec.Name)
	}
	return names
}

// installDemoBundle 复制一份 Demo 的 Agent 目录（技能另装到 <workspace>/skills），
// 把模型留空，并在解释器已知时替换 AGENT.md 的脚本占位符；Agent 目录里的 skills/
// 是内嵌素材的组织方式，不复制进工作区。
// installDemoBundle copies one demo's agent directory (skills go to <workspace>/skills
// separately), blanks the model and substitutes the script interpreter placeholder.
func installDemoBundle(spec demoSpec, wsRoot string, force bool, interpreter string) (demoManifestItem, error) {
	item := demoManifestItem{
		Name:     spec.Name,
		AgentDir: filepath.Join(wsRoot, agentsDirName, spec.Name),
		Pending:  demoPendingItems(spec),
	}
	state, err := copyDemoTree(path.Join("daily", spec.Name), item.AgentDir, force, map[string]bool{"skills": true})
	if err != nil {
		return demoManifestItem{}, fmt.Errorf("安装 %s：%w", spec.Name, err)
	}
	item.State = state
	agentFile := filepath.Join(item.AgentDir, "AGENT.md")
	if state == demoStateInstalled {
		if err := blankDemoModel(agentFile); err != nil {
			return demoManifestItem{}, fmt.Errorf("安装 %s：%w", spec.Name, err)
		}
	}
	// 保留态同样对齐解释器占位符（只动占位符，不碰用户填好的模型绑定）。
	if err := builtin.ResolveAgentPython(agentFile, interpreter); err != nil {
		return demoManifestItem{}, fmt.Errorf("安装 %s：%w", spec.Name, err)
	}
	if spec.Skill == "" {
		return item, nil
	}
	item.SkillDir = filepath.Join(wsRoot, demoSkillsDirName, spec.Skill)
	skillState, err := copyDemoTree(path.Join("daily", spec.Name, "skills", spec.Skill), item.SkillDir, force, nil)
	if err != nil {
		return demoManifestItem{}, fmt.Errorf("安装 %s 技能：%w", spec.Name, err)
	}
	item.SkillState = skillState
	return item, nil
}

// demoPendingItems 计算一份 Demo 安装后仍待配置的项目（模型、MCP、环境变量）。
// demoPendingItems lists what still needs configuring after installation.
func demoPendingItems(spec demoSpec) []string {
	pending := []string{"模型：AGENT.md 的 provider.model 为空，填入后才会注册"}
	for _, server := range spec.MCPServers {
		pending = append(pending, "MCP：mcp_servers.yaml 缺少 "+server)
	}
	for _, key := range spec.EnvKeys {
		pending = append(pending, "环境变量："+key+" 需在 basalt 进程环境里设置")
	}
	return pending
}

// copyDemoTree 从内嵌素材复制一棵子树；目标已存在且不覆盖时返回 kept。
// skip 是目标根下一级的目录名黑名单——Agent 目录里的 skills/ 由安装器单独装到
// <workspace>/skills，复制进 Agent 目录只会让加载器打 WARN 并留下重复副本。
// copyDemoTree copies one embedded subtree; an existing target is kept unless forced.
func copyDemoTree(srcRoot, dstRoot string, force bool, skip map[string]bool) (string, error) {
	return builtin.CopyTree(srcRoot, dstRoot, force, skip)
}

// blankDemoModel 把 frontmatter 里 provider 块的 model 值改成空字符串——空模型
// 让 Agent 装载期按「blank model」拒绝注册，正好表达「装好但等配置」。
// blankDemoModel clears provider.model so the loader refuses to register the
// agent until an operator fills it in.
func blankDemoModel(agentFile string) error {
	return builtin.BlankModel(agentFile)
}

// installDemoSampleDB 用内嵌脚本生成只读样例库；缺 Python 时记 pending 而不失败。
// 生成细节（解释器探测、脚本、夹具路径）全在 internal/builtin：启动期自动准备走同一条。
// installDemoSampleDB generates the sample database through the shared builtin helper;
// a missing interpreter is reported as pending rather than aborting the whole install.
func installDemoSampleDB(ctx context.Context, wsRoot string, force bool) (demoSampleDB, error) {
	sample, err := builtin.EnsureSampleDB(ctx, wsRoot, force)
	if err != nil {
		return demoSampleDB{}, err
	}
	return demoSampleDB{Path: sample.Path, State: sample.State, Note: sample.Note}, nil
}

// demoDBHubSamplePath 是样例库的固定落点（生成、配置、报告共用一份来源）。
// demoDBHubSamplePath is the single source of the sample database location.
func demoDBHubSamplePath(wsRoot string) string {
	return builtin.SampleDBPath(wsRoot)
}

// needsDemoSample 报告本次选择里是否有需要样例库的 Demo。
// needsDemoSample reports whether the selection needs the sample database.
func needsDemoSample(selected []demoSpec) bool {
	for _, spec := range selected {
		if spec.NeedsSample {
			return true
		}
	}
	return false
}

// needsDemoDocs 报告本次选择里是否有需要样例只读文档库的 Demo。
// needsDemoDocs reports whether the selection needs the sample document library.
func needsDemoDocs(selected []demoSpec) bool {
	for _, spec := range selected {
		if spec.NeedsDocs {
			return true
		}
	}
	return false
}

// installDemoDocsLibrary 把共享夹具文档装成样例只读库（`docs-diff` 的输入）：
// 路径与内置只读 server 的 `-root` 取自同一处定义。
// installDemoDocsLibrary installs the sample read-only document library.
func installDemoDocsLibrary(wsRoot string, force bool) (demoDocsLibrary, error) {
	library, err := builtin.EnsureDocsLibrary(wsRoot, force)
	if err != nil {
		return demoDocsLibrary{}, err
	}
	return demoDocsLibrary{Path: library.Path, State: library.State}, nil
}

// resolveDemoInterpreter 取示例脚本用的解释器：随包运行时优先（配置模板登记的正是这一
// 串），源码检出才回落 PATH。取不到自身可执行文件路径时按"没有随包运行时"处理。
// resolveDemoInterpreter prefers the bundled runtime and falls back to PATH.
func resolveDemoInterpreter(ctx context.Context) (string, error) {
	runtimeRoot := ""
	if executable, err := os.Executable(); err == nil {
		runtimeRoot = filepath.Join(filepath.Dir(executable), "runtime")
	}
	return builtin.ResolvePython(ctx, runtimeRoot)
}

// demoInterpreterNote 把解释器探测失败翻成报告里的一句话；成功时为空。
// demoInterpreterNote renders the resolution failure for the report.
func demoInterpreterNote(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// demoSrcDir 是自研 MCP 源码在工作区里的固定落点（报告、构建命令与安装共用一份来源）。
// demoSrcDir is the single source of the exported docs-readonly source location.
func demoSrcDir(wsRoot string) string {
	return filepath.Join(wsRoot, "demos", "src", "docs-readonly")
}

// needsDemoSource 报告本次选择里是否有 Demo 依赖 docs-readonly。
// needsDemoSource reports whether the selection needs the docs-readonly source.
func needsDemoSource(selected []demoSpec) bool {
	for _, spec := range selected {
		for _, server := range spec.MCPServers {
			if server == "docs-readonly" {
				return true
			}
		}
	}
	return false
}

// demoSourceGoModText 是导出目录里的最小 go.mod：源码并回主 module 后自身不再带
// go.mod，导出时补一份，保住 `go -C <dir> build .` 的独立构建体验。
// demoSourceGoModText restores a minimal go.mod so the exported directory builds standalone.
const demoSourceGoModText = `module basalt-docs-readonly

go 1.27
`

// installDemoSource 把内嵌的 docs-readonly 源码导出到工作区，并补一份最小 go.mod。
// installDemoSource exports the embedded source and writes the minimal go.mod.
func installDemoSource(wsRoot string, force bool) (demoSourceAsset, error) {
	dir := demoSrcDir(wsRoot)
	state, err := copyDemoTree("shared/mcp/docs-readonly", dir, force, nil)
	if err != nil {
		return demoSourceAsset{}, fmt.Errorf("导出 docs-readonly 源码：%w", err)
	}
	gomodPath := filepath.Join(dir, "go.mod")
	if state == demoStateInstalled {
		if err := os.WriteFile(gomodPath, []byte(demoSourceGoModText), 0o600); err != nil {
			return demoSourceAsset{}, fmt.Errorf("写出 %s：%w", gomodPath, err)
		}
	}
	depsDir := filepath.Join(wsRoot, "demos", "deps")
	if err := os.MkdirAll(depsDir, 0o750); err != nil {
		return demoSourceAsset{}, fmt.Errorf("创建依赖目录 %s：%w", depsDir, err)
	}
	return demoSourceAsset{Path: dir, State: state, GoMod: gomodPath}, nil
}

// writeDemoDBHubConfig 把只读配置写到 <workspace>/demos/mcp/dbhub.toml。写它不等于
// 接入：本命令不改 mcp_servers.yaml，接入由用户确认（或首次启动的安装器代写）。
// writeDemoDBHubConfig writes the config file; wiring it up is the installer's job.
func writeDemoDBHubConfig(wsRoot, dbPath string) (string, error) {
	target := builtin.DBHubConfigPath(wsRoot)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", fmt.Errorf("创建配置目录 %s：%w", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, []byte(builtin.DBHubConfig(dbPath)), 0o600); err != nil {
		return "", fmt.Errorf("写出 DBHub 配置 %s：%w", target, err)
	}
	return target, nil
}

// findDemoPython 依次尝试 python3 / python / py——与 SETUP 里教用户的口径一致。
// 命中的候选必须**跑一次探针**（版本号 3 且能 import sqlite3）：Windows 的
// `python` 常是 Microsoft Store 的占位程序，LookPath 成功而执行即退出 9009——
// 直接采信 LookPath 会把「没有解释器」误报成「脚本失败」。
// findDemoPython probes the three documented names and requires each candidate to run.
func findDemoPython(ctx context.Context) (string, error) {
	return builtin.FindPython(ctx)
}

// writeDemoManifest 把安装清单写到 <workspace>/demos/install.json（覆盖写）。
// writeDemoManifest writes the manifest to <workspace>/demos/install.json.
func writeDemoManifest(wsRoot string, manifest demoManifest) error {
	dir := filepath.Join(wsRoot, "demos")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("创建清单目录 %s：%w", dir, err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化安装清单：%w", err)
	}
	data = append(data, '\n')
	target := filepath.Join(dir, "install.json")
	if err := os.WriteFile(target, data, 0o600); err != nil {
		return fmt.Errorf("写出安装清单 %s：%w", target, err)
	}
	return nil
}

// writeDemoPlan 是 --dry-run 的输出：只列将写入的路径，不碰磁盘。
// writeDemoPlan prints the would-be writes for --dry-run.
func writeDemoPlan(out io.Writer, wsRoot string, selected []demoSpec) {
	fmt.Fprintf(out, "试运行：工作区 %s\n", wsRoot)
	for _, spec := range selected {
		fmt.Fprintf(out, "  %s → %s\n", spec.Name, filepath.Join(wsRoot, agentsDirName, spec.Name))
		if spec.Skill != "" {
			fmt.Fprintf(out, "  %s 技能 → %s\n", spec.Name, filepath.Join(wsRoot, demoSkillsDirName, spec.Skill))
		}
	}
	if needsDemoSample(selected) {
		fmt.Fprintf(out, "  %s → %s\n", "样例库", demoDBHubSamplePath(wsRoot))
		fmt.Fprintf(out, "  %s → %s\n", "DBHub 只读配置", filepath.Join(wsRoot, "demos", "mcp", "dbhub.toml"))
	}
	if needsDemoDocs(selected) {
		fmt.Fprintf(out, "  %s → %s\n", "样例文档库", builtin.SampleDocsPath(wsRoot))
	}
	if needsDemoSource(selected) {
		fmt.Fprintf(out, "  %s → %s（含生成的 go.mod）\n", "docs-readonly 源码", demoSrcDir(wsRoot))
	}
	fmt.Fprintf(out, "  %s → %s\n", "安装清单", filepath.Join(wsRoot, "demos", "install.json"))
}

// writeDemoReport 打印安装结果与逐项待办：模型、MCP、依赖、凭据、权限。
// writeDemoReport prints the outcome plus the itemised follow-up checklist.
func writeDemoReport(out io.Writer, wsRoot string, selected []demoSpec, manifest demoManifest) {
	installed := make([]string, 0, len(manifest.Demos))
	kept := make([]string, 0, len(manifest.Demos))
	for _, item := range manifest.Demos {
		if item.State == demoStateKept {
			kept = append(kept, item.Name)
			continue
		}
		installed = append(installed, item.Name)
	}
	sort.Strings(installed)
	sort.Strings(kept)

	fmt.Fprintf(out, "工作区 %s\n", wsRoot)
	fmt.Fprintf(out, "安装清单 %s\n", filepath.Join(wsRoot, "demos", "install.json"))
	writeItems(out, "已安装", installed)
	writeItems(out, "已存在，保留", kept)
	for _, item := range manifest.Demos {
		if item.SkillDir == "" {
			continue
		}
		fmt.Fprintf(out, "技能 %s：%s（%s）\n", item.Name, item.SkillDir, demoStateLabel(item.SkillState))
	}
	if manifest.SampleDB != nil {
		fmt.Fprintf(out, "样例库 %s：%s", manifest.SampleDB.Path, demoStateLabel(manifest.SampleDB.State))
		if manifest.SampleDB.Note != "" {
			fmt.Fprintf(out, "（%s）", firstLine(manifest.SampleDB.Note))
		}
		fmt.Fprintln(out)
	}
	if manifest.DBHubConfig != "" {
		fmt.Fprintln(out, "DBHub 配置 "+manifest.DBHubConfig+"（只读 + max_rows=500，dsn 指向样例库）")
	}
	if manifest.DocsReadOnly != nil {
		fmt.Fprintln(out, "docs-readonly 源码 "+manifest.DocsReadOnly.Path+"（"+
			demoStateLabel(manifest.DocsReadOnly.State)+"，"+docsReadOnlyGoModNote(*manifest.DocsReadOnly)+"）")
	}
	if manifest.DocsLibrary != nil {
		fmt.Fprintf(out, "样例文档库 %s：%s（内置 docs-readonly 的 -root 指向这里）\n",
			manifest.DocsLibrary.Path, demoStateLabel(manifest.DocsLibrary.State))
	}
	writeDemoInterpreterLine(out, manifest)
	fmt.Fprintln(out, "待配置（逐项做完才会注册 / 运行）：")
	for _, item := range manifest.Demos {
		fmt.Fprintf(out, "  %s\n", item.Name)
		for _, pending := range item.Pending {
			fmt.Fprintf(out, "    - %s\n", pending)
		}
	}
	writeDemoDepTable(out, wsRoot, selected)
	writeDemoMCPHint(out, wsRoot, selected, manifest)
	writeDemoCredentialHints(out, wsRoot, selected)
	writeDemoPermissionHint(out)
}

// writeDemoInterpreterLine 报告示例脚本的解释器：写入的是什么、以及找不到时下一步做什么。
// 这台机器上"找到"不等于"已放行"——报告必须把登记动作单独说清，否则脚本仍会被拒。
// writeDemoInterpreterLine reports the substituted interpreter and the pending grant.
func writeDemoInterpreterLine(out io.Writer, manifest demoManifest) {
	if manifest.Interpreter != "" {
		fmt.Fprintf(out, "解释器 %s（已写进 AGENT.md 的脚本步骤；需整串登记进 shell.allowed_commands）\n",
			manifest.Interpreter)
		return
	}
	fmt.Fprintln(out, "解释器 未找到：脚本类 Demo 的 AGENT.md 保留 "+builtin.PythonPlaceholder+"——"+
		"按 SETUP 第 6 节登记本机解释器后，把该占位符替换成同一串")
	if manifest.InterpreterNote != "" {
		fmt.Fprintf(out, "  原因：%s\n", firstLine(manifest.InterpreterNote))
	}
}

// docsReadOnlyGoModNote 按实际状态报告 go.mod：只有本次新建（installed）才写入了
// 最小 go.mod；保留既有目录（kept）时不改用户文件，因此不能声称"已生成"——若旧目录
// 本就没有 go.mod，报告里的构建命令会失败，必须当场说清。
// docsReadOnlyGoModNote reports the go.mod fact per state: only a fresh export writes
// one, so a kept directory must not claim "generated" when the file may be absent.
func docsReadOnlyGoModNote(asset demoSourceAsset) string {
	if asset.State == demoStateInstalled {
		return "已生成 go.mod"
	}
	if _, err := os.Stat(asset.GoMod); err == nil {
		return "保留原目录，复用其中已有的 go.mod"
	}
	return "保留原目录，未写 go.mod；构建前需自行补一份（module basalt-docs-readonly + go 1.27）"
}

// writeDemoDepTable 打印固定版本依赖与安装命令；命令只打印，不执行。
// writeDemoDepTable prints the pinned dependencies and their install commands.
func writeDemoDepTable(out io.Writer, wsRoot string, selected []demoSpec) {
	used := make(map[string]bool)
	for _, spec := range selected {
		for _, server := range spec.MCPServers {
			used[server] = true
		}
	}
	fmt.Fprintln(out, "固定版本依赖（命令只打印，不代装）：")
	for _, dep := range mcpDeps {
		if !used[dep.ID] {
			continue
		}
		fmt.Fprintf(out, "  %s %s（%s）\n", dep.ID, dep.Version, dep.SourceURL)
		command := dep.Install
		if dep.Kind == "go-build" {
			// go-build 的命令里带两个 %s：源码目录与产物路径（都按实际工作区渲染）。
			command = fmt.Sprintf(dep.Install, demoSrcDir(wsRoot),
				filepath.Join(wsRoot, "demos", "deps", dep.Entry))
		}
		fmt.Fprintf(out, "    %s\n", command)
		fmt.Fprintf(out, "    入口：%s\n", dep.Entry)
	}
	fmt.Fprintf(out, "  依赖目录建议：%s\n", filepath.Join(wsRoot, "demos", "deps"))
}

// writeDemoMCPHint 打印 MCP 接入片段：只有 dbhub 的配置文件由本命令生成，因此给出
// 可直接粘贴的完整条目；其余 server 的片段留在各自 SETUP.md（随 Demo 一起装进工作区）。
// writeDemoMCPHint prints the paste-ready dbhub entry; other servers point at SETUP.md.
func writeDemoMCPHint(out io.Writer, wsRoot string, selected []demoSpec, manifest demoManifest) {
	if manifest.DBHubConfig == "" {
		return
	}
	dep, ok := demoDepByID("dbhub")
	if !ok {
		return
	}
	entry := filepath.ToSlash(filepath.Join(wsRoot, "demos", "deps", dep.Entry))
	fmt.Fprintln(out, "MCP 接入片段（需你确认后写进 <workspace>/mcp_servers.yaml；本命令不改配置）：")
	fmt.Fprintln(out, "  - name: dbhub")
	fmt.Fprintln(out, "    transport: stdio")
	fmt.Fprintln(out, "    command: node")
	argsLine := fmt.Sprintf("    args: [%q, %q, %q, %q, %q]", entry, "--transport", "stdio", "--config",
		filepath.ToSlash(manifest.DBHubConfig))
	fmt.Fprintln(out, argsLine)
	for _, spec := range selected {
		for _, server := range spec.MCPServers {
			if server == "dbhub" {
				continue
			}
			hint := fmt.Sprintf("  %s 的片段见 %s", server,
				filepath.Join(wsRoot, agentsDirName, spec.Name, "SETUP.md"))
			fmt.Fprintln(out, hint)
		}
	}
}

// demoDepByID 按 id 取固定版本依赖（报告与片段共用一份来源）。
// demoDepByID looks one pinned dependency up by id.
func demoDepByID(id string) (mcpDep, bool) {
	for _, dep := range mcpDeps {
		if dep.ID == id {
			return dep, true
		}
	}
	return mcpDep{}, false
}

// writeDemoCredentialHints 打印凭据获取指引（只打印步骤，不接收也不回显任何值）。
// writeDemoCredentialHints prints how to obtain each key; values never pass through.
func writeDemoCredentialHints(out io.Writer, wsRoot string, selected []demoSpec) {
	seen := make(map[string]bool)
	lines := 0
	for _, spec := range selected {
		for _, key := range spec.EnvKeys {
			hint, ok := demoCredentialHints[key]
			if !ok || seen[key] {
				continue
			}
			seen[key] = true
			if lines == 0 {
				fmt.Fprintln(out, "凭据获取（只在本机输入；本命令不接收任何值）：")
				lines++
			}
			fmt.Fprintln(out, "  "+hint.Key)
			fmt.Fprintln(out, "    "+hint.Need)
			setupPath := filepath.Join(wsRoot, agentsDirName, spec.Name, "SETUP.md")
			fmt.Fprintln(out, "    步骤："+setupPath+" "+hint.Section)
		}
	}
}

// writeDemoPermissionHint 打印需要用户确认的权限变更：本命令不改配置。
// writeDemoPermissionHint prints the permission changes that need consent.
func writeDemoPermissionHint(out io.Writer) {
	fmt.Fprintln(out, "权限变更（需你确认后自行写入 basalt.yaml；本命令不改任何配置）：")
	fmt.Fprintln(out, "  shell.allowed_commands：把上面的解释器路径与 MCP 命令**整串**写进去（Windows 不查 PATH、不补后缀）")
	fmt.Fprintln(out, "  shell.interpreters：把同一串再登记一次即启用受控通道（argv 必须是 [脚本, 参数…]，脚本须在 agents/<name>/scripts 内）")
	fmt.Fprintln(out, "  凭据只从进程环境读取（${VAR} 占位），不要写进任何文件")
}

// demoStateLabel 把状态常量翻成给人看的词。
// demoStateLabel renders a state constant for humans.
func demoStateLabel(state string) string {
	switch state {
	case demoStateInstalled:
		return "已生成"
	case demoStateKept:
		return "已存在，保留"
	default:
		return "待生成"
	}
}

// firstLine 取多行输出的第一行（样例库脚本会打印 JSON 摘要）。
// firstLine keeps the first line of a multi-line tool output.
func firstLine(text string) string {
	return builtin.FirstLine(text)
}

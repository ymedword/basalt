package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/examples"
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
//
// demoPythonProbe 是解释器探针的固定脚本：只打印主版本号，用来识破 Windows 上
// "LookPath 成功但执行即退出"的 Microsoft Store 占位程序。
// demoPythonProbe prints only the major version so the Store stub is detected.
const demoPythonProbe = "import sqlite3, sys; print(sys.version_info[0])"

// demoSpec 是一份 Demo 的安装描述：哪些 MCP server、哪些环境变量、要不要样例库。
// demoSpec describes one bundled demo and what it needs to become runnable.
type demoSpec struct {
	Name        string
	Title       string
	MCPServers  []string
	EnvKeys     []string
	Skill       string
	NeedsSample bool
}

// demoSpecs 是内置 Demo 的唯一清单（顺序即安装顺序，也是报告的打印顺序）。
// demoSpecs is the single list of bundled demos; order drives install and report.
var demoSpecs = []demoSpec{
	{Name: "biz-daily", Title: "业务数据体检", MCPServers: []string{"dbhub"}, NeedsSample: true},
	{Name: "docs-diff", Title: "文档变更", MCPServers: []string{"docs-readonly"}},
	{Name: "github-radar", Title: "GitHub 星标雷达", MCPServers: []string{"github"}, EnvKeys: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"}},
	{Name: "industry-brief", Title: "行业简报", MCPServers: []string{"brave-search", "fetch"}, EnvKeys: []string{"BRAVE_API_KEY"}},
	{Name: "repo-digest", Title: "代码仓库日报", MCPServers: []string{"github"}, EnvKeys: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"}, Skill: "repo-digest"},
}

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
	SampleDB      *demoSampleDB      `json:"sample_db,omitempty"`
	DBHubConfig   string             `json:"dbhub_config,omitempty"`
	DocsReadOnly  *demoSourceAsset   `json:"docs_readonly,omitempty"`
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

// 安装状态常量：新增、保留、待生成。
const (
	demoStateInstalled = "installed"
	demoStateKept      = "kept"
	demoStatePending   = "pending"
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

	manifest := demoManifest{SchemaVersion: 1, InstalledAt: time.Now().UTC().Format(time.RFC3339)}
	for _, spec := range selected {
		item, err := installDemoBundle(spec, wsRoot, opts.Force)
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
// 并把模型留空；Agent 目录里的 skills/ 是内嵌素材的组织方式，不复制进工作区。
// installDemoBundle copies one demo's agent directory (skills go to <workspace>/skills
// separately) and blanks the model.
func installDemoBundle(spec demoSpec, wsRoot string, force bool) (demoManifestItem, error) {
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
	if state == demoStateInstalled {
		if err := blankDemoModel(filepath.Join(item.AgentDir, "AGENT.md")); err != nil {
			return demoManifestItem{}, fmt.Errorf("安装 %s：%w", spec.Name, err)
		}
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
// Names in skip are dropped from the first level below the root.
func copyDemoTree(srcRoot, dstRoot string, force bool, skip map[string]bool) (string, error) {
	if _, err := os.Stat(dstRoot); err == nil {
		if !force {
			return demoStateKept, nil
		}
		if err := os.RemoveAll(dstRoot); err != nil {
			return "", fmt.Errorf("清理旧目录 %s：%w", dstRoot, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("检查目标 %s：%w", dstRoot, err)
	}
	err := fs.WalkDir(examples.Assets, srcRoot, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(filepath.FromSlash(srcRoot), filepath.FromSlash(p))
		if relErr != nil {
			return relErr
		}
		if first := firstPathElement(rel); first != "" && skip[first] {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dstRoot, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		data, readErr := fs.ReadFile(examples.Assets, p)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		return "", err
	}
	return demoStateInstalled, nil
}

// blankDemoModel 把 frontmatter 里 provider 块的 model 值改成空字符串——空模型
// 让 Agent 装载期按「blank model」拒绝注册，正好表达「装好但等配置」。
// blankDemoModel clears provider.model so the loader refuses to register the
// agent until an operator fills it in.
func blankDemoModel(agentFile string) (err error) {
	// 读写都经 os.Root 钉在 AGENT.md 所在目录内：目标是内嵌素材复制出来的固定文件，
	// 路径由安装器拼出来，用根句柄把"只能是这个目录里的这个名字"写进代码而不是注释。
	// Both the read and the write go through an os.Root on the agent directory.
	dir, err := os.OpenRoot(filepath.Dir(agentFile))
	if err != nil {
		return fmt.Errorf("打开 %s 所在目录：%w", agentFile, err)
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("关闭 %s 所在目录：%w", agentFile, closeErr)
		}
	}()
	name := filepath.Base(agentFile)
	source, err := dir.Open(name)
	if err != nil {
		return fmt.Errorf("读取 %s：%w", agentFile, err)
	}
	data, err := io.ReadAll(source)
	if closeErr := source.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("读取 %s：%w", agentFile, err)
	}
	lines := strings.Split(string(data), "\n")
	inFrontmatter, inProvider, done := false, false, false
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		if trimmed == "---" {
			if inFrontmatter {
				break
			}
			inFrontmatter = true
			continue
		}
		if !inFrontmatter {
			continue
		}
		if strings.HasPrefix(trimmed, "provider:") {
			inProvider = true
			continue
		}
		if !inProvider {
			continue
		}
		if strings.HasPrefix(trimmed, "  model:") {
			lines[i] = `  model: ""`
			done = true
			break
		}
		if !strings.HasPrefix(trimmed, "  ") {
			break
		}
	}
	if !done {
		return fmt.Errorf("%s 里找不到 provider.model 行", agentFile)
	}
	target, err := dir.OpenFile(name, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("写入 %s：%w", agentFile, err)
	}
	if _, err := target.WriteString(strings.Join(lines, "\n")); err != nil {
		closeErr := target.Close()
		if closeErr != nil {
			return fmt.Errorf("写入 %s：%w（关闭也失败：%v）", agentFile, err, closeErr)
		}
		return fmt.Errorf("写入 %s：%w", agentFile, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("写入 %s：%w", agentFile, err)
	}
	return nil
}

// installDemoSampleDB 用内嵌脚本生成只读样例库；缺 Python 时记 pending 而不失败。
// installDemoSampleDB generates the sample database; a missing interpreter is
// reported as pending rather than aborting the whole install.
func installDemoSampleDB(ctx context.Context, wsRoot string, force bool) (demoSampleDB, error) {
	dbPath := demoDBHubSamplePath(wsRoot)
	if _, err := os.Stat(dbPath); err == nil && !force {
		return demoSampleDB{Path: dbPath, State: demoStateKept}, nil
	}
	interpreter, err := findDemoPython(ctx)
	if err != nil {
		return demoSampleDB{Path: dbPath, State: demoStatePending, Note: "未找到可用的 Python 3：" + err.Error()}, nil
	}
	scriptDir := filepath.Join(wsRoot, "demos", "scripts")
	if err := os.MkdirAll(scriptDir, 0o750); err != nil {
		return demoSampleDB{}, fmt.Errorf("创建脚本目录 %s：%w", scriptDir, err)
	}
	scriptPath := filepath.Join(scriptDir, "make_sample_db.py")
	script, err := fs.ReadFile(examples.Assets, "shared/scripts/make_sample_db.py")
	if err != nil {
		return demoSampleDB{}, fmt.Errorf("读取样例库脚本：%w", err)
	}
	if err := os.WriteFile(scriptPath, script, 0o600); err != nil {
		return demoSampleDB{}, fmt.Errorf("写出样例库脚本 %s：%w", scriptPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		return demoSampleDB{}, fmt.Errorf("创建样例目录 %s：%w", filepath.Dir(dbPath), err)
	}
	fixtures := filepath.Join(wsRoot, agentsDirName, "biz-daily", "fixtures")
	args := []string{
		scriptPath,
		"--schema", filepath.Join(fixtures, "schema.sql"),
		"--rows", filepath.Join(fixtures, "rows.json"),
		"--out", dbPath,
	}
	if force {
		args = append(args, "--force")
	}
	// #nosec G204 -- 豁免理由（用户 2026-10-26 裁决，沿用 2026-10-17 口径）：
	// ① 可执行名是 findDemoPython 实跑探针通过的 LookPath 结果（候选只有
	// python3 / python / py），argv 是安装器自己拼的脚本路径与内嵌夹具，不经 shell，
	// `--demo` 只影响已校验的 Demo 名，没有把用户输入变成命令行的通道；
	// ② G204 只接受常量实参，运行期的工作区路径静态不可见，无法用代码消除；
	// ③ 改走 env 传参或动态分派只会把审计点藏起来，风险不变、可读性更差。
	// #nosec G204 -- vetted interpreter plus installer-built argv, never via a shell;
	// constant-only argv is impossible because workspace paths are runtime values.
	output, runErr := exec.CommandContext(ctx, interpreter, args...).CombinedOutput()
	if runErr != nil {
		return demoSampleDB{
			Path:  dbPath,
			State: demoStatePending,
			Note:  fmt.Sprintf("生成失败（%v）：%s", runErr, strings.TrimSpace(string(output))),
		}, nil
	}
	return demoSampleDB{Path: dbPath, State: demoStateInstalled, Note: describeSampleDBNote(string(output))}, nil
}

// demoDBHubSamplePath 是样例库的固定落点（生成、配置、报告共用一份来源）。
// demoDBHubSamplePath is the single source of the sample database location.
func demoDBHubSamplePath(wsRoot string) string {
	return filepath.Join(wsRoot, "samples", "biz-sample.db")
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

// firstPathElement 取相对路径的第一段（根自身返回空串），用于跳过指定子目录。
// firstPathElement returns the first segment of a relative path ("" for the root).
func firstPathElement(rel string) string {
	slash := filepath.ToSlash(rel)
	if slash == "." {
		return ""
	}
	if idx := strings.IndexByte(slash, '/'); idx >= 0 {
		return slash[:idx]
	}
	return slash
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

// demoDBHubConfigText 是配置正文模板（%s 处填 DSN）：1.5.0 的只读面与行数上限只能
// 写在配置里，`--readonly` 启动参数在该版本已被移除（2026-10-26 实测）。
// demoDBHubConfigText is the config body; %s is the DSN literal.
const demoDBHubConfigText = `# @bytebase/dbhub 1.5.0 的只读配置（由 basalt demo install 生成）。
# Read-only DBHub config generated by "basalt demo install".
[[sources]]
id = "default"
dsn = %s

[[tools]]
name = "execute_sql"
source = "default"
readonly = true
max_rows = 500
`

// demoDBHubConfig 按样例库路径渲染只读配置（DSN 用 sqlite:/// + 绝对路径）。
// demoDBHubConfig renders the read-only config for the given sample database.
func demoDBHubConfig(dbPath string) string {
	dsn := "sqlite:///" + filepath.ToSlash(dbPath)
	return fmt.Sprintf(demoDBHubConfigText, strconv.Quote(dsn))
}

// writeDemoDBHubConfig 把只读配置写到 <workspace>/demos/mcp/dbhub.toml。写它不等于
// 接入：真正生效还要求用户把它写进 mcp_servers.yaml 的 --config。
// writeDemoDBHubConfig writes the config file; wiring it up stays the operator's job.
func writeDemoDBHubConfig(wsRoot, dbPath string) (string, error) {
	target := filepath.Join(wsRoot, "demos", "mcp", "dbhub.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", fmt.Errorf("创建配置目录 %s：%w", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, []byte(demoDBHubConfig(dbPath)), 0o600); err != nil {
		return "", fmt.Errorf("写出 DBHub 配置 %s：%w", target, err)
	}
	return target, nil
}

// describeSampleDBNote 把生成器的 JSON 摘要压成一行事实（表名 / 行数 / 库哈希前
// 12 位）；摘要不是 JSON 时原样保留——宁可不美化，也不编造读数。
// describeSampleDBNote condenses the generator's JSON summary into one factual line.
func describeSampleDBNote(output string) string {
	var summary struct {
		Table string `json:"table"`
		Rows  struct {
			Count int `json:"count"`
		} `json:"rows"`
		DB struct {
			SHA256 string `json:"sha256"`
		} `json:"db"`
	}
	if err := json.Unmarshal([]byte(output), &summary); err != nil {
		return firstLine(strings.TrimSpace(output))
	}
	hash := summary.DB.SHA256
	if len(hash) > 12 {
		hash = hash[:12] + "…"
	}
	return fmt.Sprintf("%s 表 %d 行，库 sha256 %s", summary.Table, summary.Rows.Count, hash)
}

// findDemoPython 依次尝试 python3 / python / py——与 SETUP 里教用户的口径一致。
// 命中的候选必须**跑一次探针**（版本号 3 且能 import sqlite3）：Windows 的
// `python` 常是 Microsoft Store 的占位程序，LookPath 成功而执行即退出 9009——
// 直接采信 LookPath 会把「没有解释器」误报成「脚本失败」。
// findDemoPython probes the three documented names and requires each candidate to
// actually run: on Windows `python` is often the Store stub, which LookPath finds
// but which cannot execute.
func findDemoPython(ctx context.Context) (string, error) {
	var failures []string
	for _, name := range []string{"python3", "python", "py"} {
		resolved, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		// #nosec G204 -- 豁免理由（用户 2026-10-26 裁决）：可执行名来自本次
		// LookPath，实参全是常量（-c + demoPythonProbe）；探针的目的正是判断这个
		// 候选"能不能跑"，所以名字必然是运行期值，无法写成常量。
		// #nosec G204 -- the name is this loop's LookPath result; all argv are constants.
		output, probeErr := exec.CommandContext(ctx, resolved, "-c", demoPythonProbe).CombinedOutput()
		if probeErr != nil {
			failures = append(failures, fmt.Sprintf("%s 不可执行（%v）", resolved, probeErr))
			continue
		}
		if strings.TrimSpace(string(output)) != "3" {
			failures = append(failures, fmt.Sprintf("%s 不是 Python 3", resolved))
			continue
		}
		return resolved, nil
	}
	if len(failures) == 0 {
		return "", errors.New("PATH 里没有 python3 / python / py")
	}
	return "", errors.New(strings.Join(failures, "；"))
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
	fmt.Fprintln(out, "  shell.allowed_commands：把实际解释器与 MCP 命令整串写进去（Windows 不查 PATH）")
	fmt.Fprintln(out, "  shell.interpreters：把解释器整串再写一遍，脚本限定在 agents/<name>/scripts 内")
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
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		return text[:idx]
	}
	return text
}

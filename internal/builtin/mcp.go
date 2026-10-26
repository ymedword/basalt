package builtin

// 本文件是内置 MCP 条目的开箱写入面（14D 设计 §2.5.1「用户永远不需要手动添加 MCP」）：
// 免凭据的三条（`docs-readonly`、`DBHub`、`fetch`）在首次启动时写入并交给随后的启动
// 基线发布；需要凭据的一条（`github`）只在凭据就绪后才写——这同时满足 US-14 的 env
// 闸门，不需要「先写草稿再补」的第二套生命周期。
//
// 写入口径：**只追加缺失的条目**，已存在的条目不覆盖、不重排、注释不动；一条都不缺时
// 零写入（不碰文件，也就不产生新的待发布差异）。追加后整份候选过与保存路径同一套校验
// 再原子落盘——安装器不是第二条定义写路径。
// The installer only appends missing entries, never rewrites or reorders existing ones,
// and validates the whole candidate through the very same commit used by the save path.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// GitHubTokenEnv 是 GitHub MCP 读取的凭据变量名（与目录配方、示例 SETUP 同一字面量）：
// 启动期安装器据它判断"凭据是否就绪"，因此必须导出，但值永远只以占位符出现。
// GitHubTokenEnv is the credential variable the GitHub MCP server reads.
const GitHubTokenEnv = "GITHUB_PERSONAL_ACCESS_TOKEN"

// GitHubTokenAliases 是 GitHub 凭据**显式登记**的同义变量名（按优先顺序）。部署环境沿用
// gh CLI 的 `GITHUB_TOKEN` 时，条目把该变量写成 `${GITHUB_TOKEN}`，子进程仍按
// `GITHUB_PERSONAL_ACCESS_TOKEN` 取值——两个名字都是写死的事实，**不**按前缀或相似度猜
// 变量名（"看起来像凭据"的变量不止一个，猜错就是把别的凭据递给第三方进程）。
// 值永远只以占位符出现在配置里，不做环境变量 → 托管凭据的隐式回落。
// GitHubTokenAliases lists the explicitly registered credential variable names. The
// placeholder may reference the alias, but the child still receives the canonical name;
// values never appear outside a ${VAR} placeholder.
var GitHubTokenAliases = []string{GitHubTokenEnv, "GITHUB_TOKEN"}

// GitHubTokenSource 按别名表顺序返回第一个非空变量名；都不在时 ok=false。安装器只在
// ok=true 时写 GitHub 条目——这与保存期的 env 闸门同一判据（"变量在"即"来源可满足"）。
// GitHubTokenSource returns the first non-empty variable of the alias table.
func GitHubTokenSource(lookup func(string) (string, bool)) (string, bool) {
	if lookup == nil {
		return "", false
	}
	for _, name := range GitHubTokenAliases {
		if value, ok := lookup(name); ok && strings.TrimSpace(value) != "" {
			return name, true
		}
	}
	return "", false
}

// MCPServersKey 是 `mcp_servers.yaml` 顶层列表键（与 internal/app 的同名字面量同义）。
// MCPServersKey is the top-level list key of mcp_servers.yaml.
const MCPServersKey = "servers"

// Paths 是随包组件在当前安装里的**解析后启动路径**（由装配层按程序目录与运行时目录算好，
// 本包不猜路径）。空字段表示该组件没有解析结果——调用方据此决定写什么、或如实记 WARN。
// Paths carries the resolved launch paths of the bundled components; an empty field
// means the component did not resolve, and the caller decides what to record.
type Paths struct {
	// DocsReadOnly 是自研只读 MCP server 的可执行件绝对路径（完整包随包发布）。
	// DocsReadOnly is the absolute path of the bundled read-only MCP server.
	DocsReadOnly string
	// Node 是 Node 运行时；DBHub 与其它 npm 组件都经它启动。
	// Node is the Node runtime used to launch npm-based components.
	Node string
	// DBHubEntry 是 `@bytebase/dbhub` 的入口脚本绝对路径。
	// DBHubEntry is the absolute path of the dbhub entry script.
	DBHubEntry string
	// DBHubConfig 是 dbhub.toml 的绝对路径（只读样例库的 DSN 写在里面）。
	// DBHubConfig is the absolute path of dbhub.toml.
	DBHubConfig string
	// GitHub 是 github-mcp-server 的可执行件绝对路径。
	// GitHub is the absolute path of the github-mcp-server binary.
	GitHub string
	// Python 是随包 Python 运行时（fetch 组件与示例脚本共用；宿主解释器不在随包布局里）。
	// Python is the bundled Python runtime that launches the fetch component.
	Python string
	// Fetch 是 fetch 组件的启动脚本，落在 `<runtime>/mcp/fetch/<版本>/` 下——依赖轴
	// 按这段路径认出组件与版本，因此它必须是**启动参数里的绝对路径**，不是宿主模块名。
	// Fetch is the launcher script under the runtime component directory; the dependency
	// axis derives the component and version from that path segment.
	Fetch string
}

// Entry 是一条要写进 `mcp_servers.yaml` 的 stdio 条目；Env 的值必须是 `${VAR}` 占位。
// Entry is one stdio entry to write; every Env value must be a ${VAR} placeholder.
type Entry struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
}

// config 把条目映射为渲染器的输入（transport 恒为 stdio：三条内置条目都是本地子进程）。
// config maps the entry onto the renderer input; all built-in entries are stdio.
func (e Entry) config() core.MCPServerConfig {
	return core.MCPServerConfig{
		Name:      e.Name,
		Transport: "stdio",
		Command:   e.Command,
		Args:      e.Args,
		Env:       e.Env,
	}
}

// CredentialFreeEntries 返回**不需要凭据**的三条内置条目：自研只读文档 server、只读
// 样例库的 DBHub 与 fetch。命令恒取**发行布局里的期望路径**（由调用方按
// `<程序目录>/runtime` 算好）——本机缺件时写出的配置与齐全时逐字节相同，缺件由依赖轴
// 报 `missing`；换一份配置只会让"缺什么"变成"配置为什么长得不一样"。
//
// CredentialFreeEntries returns the three entries that need no credential. The commands
// are the expected runtime-layout paths, so a missing component never changes the file.
func CredentialFreeEntries(paths Paths) []Entry {
	return []Entry{
		{Name: "docs-readonly", Command: paths.DocsReadOnly},
		{
			Name:    "dbhub",
			Command: paths.Node,
			Args: []string{
				paths.DBHubEntry, "--transport", "stdio", "--config", paths.DBHubConfig,
			},
		},
		// fetch 经随包 Python 运行启动脚本：命令是解释器、参数是组件路径，两者都在
		// 发行布局里算出来，宿主解释器不参与。
		{Name: "fetch", Command: paths.Python, Args: []string{paths.Fetch}},
	}
}

// GitHubEntry 返回需要凭据的 GitHub 条目：命令与参数取目录配方的实测形态
// （`stdio --read-only`），凭据只以 `${GITHUB_PERSONAL_ACCESS_TOKEN}` 占位出现——
// 值永远不进配置文件、日志与审计。
//
// GitHubEntry returns the credential-gated entry. The credential appears only as a
// ${VAR} placeholder; a value never reaches the configuration file.
func GitHubEntry(paths Paths) Entry {
	return GitHubEntryWithToken(paths, GitHubTokenEnv)
}

// GitHubEntryWithToken 构造 GitHub 条目：子进程的环境键恒为 `GITHUB_PERSONAL_ACCESS_TOKEN`，
// 占位符引用的是**显式选定的来源变量**（`GitHubTokenAliases` 之一）。tokenVar 为空时
// 退回本名，绝不写出空占位。
//
// GitHubEntryWithToken keeps the child's environment key canonical while the placeholder
// references the explicitly selected source variable.
func GitHubEntryWithToken(paths Paths, tokenVar string) Entry {
	if strings.TrimSpace(tokenVar) == "" {
		tokenVar = GitHubTokenEnv
	}
	return Entry{
		Name:    "github",
		Command: paths.GitHub,
		Args:    []string{"stdio", "--read-only"},
		Env:     map[string]string{GitHubTokenEnv: "${" + tokenVar + "}"},
	}
}

// AllowlistedCommands 返回安装器**可能写进配置**的全部命令（按条目顺序去重）：新实例的
// 命令白名单必须原样登记这几条。命令白名单按可执行件整串逐字比对，而随包条目的 command
// 是绝对路径——不登记就会在 spawn 之前被策略拒绝（`shell.allowed_commands` 是显式登记
// 口径，14D §2.2.2：清单为空时自动放行面恒不成立）。由条目构造**派生**而不是手抄清单：
// 条目形态或组件路径变化时，白名单自动跟着变，不会留下"写了条目却没登记命令"的缺口。
//
// AllowlistedCommands derives the commands the installer may write; the new-instance
// allowlist registers exactly these, so a bundled entry can never be denied pre-spawn.
func AllowlistedCommands(paths Paths) []string {
	entries := append(CredentialFreeEntries(paths), GitHubEntry(paths))
	commands := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Command != "" && !slices.Contains(commands, entry.Command) {
			commands = append(commands, entry.Command)
		}
	}
	return commands
}

// MCPResult 是一次 EnsureMCP 的结果：Written 是本次新写的名字（顺序即写入顺序），
// Present 是操作后文件里存在的那几条（原有 ∪ 本次写入）——调用方据此记录"已经准备过
// 哪些"，而不用第二次读文件。
// MCPResult reports what was written and which entries the file now carries.
type MCPResult struct {
	Written []string
	Present []string
}

// EnsureMCP 把 entries 里**还不存在**的条目追加到 `mcp_servers.yaml`，返回写入与现存
// 名字。一条都不缺时零写入（不碰文件，也就不产生新的待发布差异）。
//
// 已存在但形态不同的同名条目不覆盖：那是运营（或用户）写下的定义，覆盖等于悄悄改配置；
// 需要换形态应走管理面的保存路径，那里有预写、版本与回滚。
// EnsureMCP appends only the missing entries and leaves an existing definition of the
// same name untouched; changing its shape belongs to the audited save path.
func EnsureMCP(wsRoot string, entries []Entry) (MCPResult, error) {
	if len(entries) == 0 {
		return MCPResult{}, nil
	}
	path := filepath.Join(wsRoot, mcpServersFileName)
	// 读经工作区根句柄：文件名是常量，能读到的只能是工作区根下这一份（根外的软链接被拒）。
	// The read goes through a root handle on the workspace: only that one file is reachable.
	root, err := os.OpenRoot(wsRoot)
	if err != nil {
		return MCPResult{}, fmt.Errorf("打开工作区根 %s：%w", wsRoot, err)
	}
	defer func() { _ = root.Close() }()
	doc, err := root.ReadFile(mcpServersFileName)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return MCPResult{}, fmt.Errorf("读取 %s：%w", mcpServersFileName, err)
	}
	if errors.Is(err, fs.ErrNotExist) {
		doc = nil
	}

	existing, err := workspace.BlockEntries(doc, MCPServersKey)
	if err != nil {
		return MCPResult{}, fmt.Errorf("解析 %s 的 servers 块：%w", mcpServersFileName, err)
	}
	names := make(map[string]bool, len(existing))
	present := make([]string, 0, len(existing))
	for _, item := range existing {
		names[item.Name] = true
		present = append(present, item.Name)
	}

	written := make([]string, 0, len(entries))
	for _, entry := range entries {
		if names[entry.Name] {
			continue
		}
		if strings.TrimSpace(entry.Command) == "" {
			// 组件路径没解析出来就写一条 command 为空的条目，等于把一个必然失败的
			// 配置装成"已就位"——宁可少写一条并点名，也不发布假配置。
			// A blank command would publish a configuration that cannot start; skip it
			// and let the caller name the missing component instead.
			return MCPResult{Written: written, Present: present},
				fmt.Errorf("内置 MCP 条目 %s 缺少启动路径，拒绝写入空命令条目", entry.Name)
		}
		rendered, renderErr := workspace.RenderMCPServerEntry(entry.config())
		if renderErr != nil {
			return MCPResult{Written: written, Present: present},
				fmt.Errorf("渲染内置 MCP 条目 %s：%w", entry.Name, renderErr)
		}
		doc, err = workspace.AppendEntry(doc, MCPServersKey, rendered)
		if err != nil {
			return MCPResult{Written: written, Present: present},
				fmt.Errorf("追加内置 MCP 条目 %s：%w", entry.Name, err)
		}
		names[entry.Name] = true
		written = append(written, entry.Name)
		present = append(present, entry.Name)
	}
	if len(written) == 0 {
		return MCPResult{Present: present}, nil
	}
	if err := workspace.CommitMCPServersDefinition(path, doc); err != nil {
		return MCPResult{Written: written, Present: present}, fmt.Errorf("写入 %s：%w", mcpServersFileName, err)
	}
	return MCPResult{Written: written, Present: present}, nil
}

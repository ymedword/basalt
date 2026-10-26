// Package builtin 交付五个内置 Agent 的开箱路径（14D 设计 §2.5.1 / §4.1 第 2 步）：
// 定义、技能、样例库与免凭据 MCP 条目在**首次启动**时由系统就位，用户只提供凭据。
//
// 三条纪律：
//  1. **安装即准备，启用才运行**——装进来的一律是 `disabled` 生命周期（状态文件由随后
//     的启动基线写成 disabled），调度器不排期、不产生自动 Run；
//  2. **已存在的一律不动**——用户改过的 Agent 定义不会被覆盖，只有显式 `force` 才重写；
//     这与 `basalt demo install` 的幂等口径同源，两条命令不存在第二套覆盖规则；
//  3. **不写模型与凭据**——`provider.model` 安装时清空，凭据只认进程环境与托管存储；
//     本包不接收、不落盘、不回显任何凭据值。
//
// 本包是"五个内置 Agent 是哪五个、各要什么"的**唯一清单**：`basalt demo install`
// 与启动期自动准备都从这里取，不再各写一份。
// This package is the single list of the five built-in agents shared by the demo
// command and the first-start installer. Existing definitions are never overwritten
// unless force is set, and the installer never writes a model or a credential.
package builtin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ymedword/basalt/examples"
)

const (
	// StateInstalled 表示本次新建；StateKept 表示目标已存在、逐字节未动。
	// StateInstalled means created this run; StateKept means left untouched.
	StateInstalled = "installed"
	StateKept      = "kept"

	// agentsDirName / skillsDirName 与 internal/workspace 的加载布局同源：
	// `.basalt/agents/<name>/AGENT.md` 与 `.basalt/skills/<skill>/SKILL.md`。
	// The directory names mirror the workspace layout the loader scans.
	agentsDirName = "agents"
	skillsDirName = "skills"

	// agentDefinitionFile 是 Agent 的唯一定义入口（契约 §1）。
	// agentDefinitionFile is the single definition entry point of an agent.
	agentDefinitionFile = "AGENT.md"

	// mcpServersFileName 是 MCP 配置文件名（与 internal/app 的同名字面量同义）。
	// mcpServersFileName is the MCP configuration file name.
	mcpServersFileName = "mcp_servers.yaml"
)

// Agent 描述一个内置 Agent：名称、显示标题、依赖的 MCP server、所需环境变量、技能与
// 是否要样例库。字段口径与 `basalt demo install` 的安装报告一致。
// Agent describes one built-in agent: its bundled tree, MCP dependencies, required
// environment variables, its skill and whether it needs the sample database.
type Agent struct {
	Name        string
	Title       string
	MCPServers  []string
	EnvKeys     []string
	Skill       string
	NeedsSample bool
	// NeedsDocs 表示该 Agent 的输入是样例只读文档库（`docs-diff`）：安装器把库装到
	// `samples/docs`，内置只读 server 的 `-root` 指向那里。
	// NeedsDocs marks the agent whose input is the sample read-only document library.
	NeedsDocs bool
}

// agents 是五个内置 Agent 的唯一清单（顺序即安装顺序，也是报告的打印顺序）。
// 名称与 `examples/daily/` 下的目录名逐字一致——目录名就是加载单元。
//
// `industry-brief` 的搜索通道按 2026-10-26 用户裁决改走内置 `web_search`（DeepSeek
// Search API），因此它只声明 `fetch` 一条 MCP，也不再要求 `BRAVE_API_KEY`（设计 §2.5.4）。
// industry-brief searches through the built-in web_search tool, so it declares only the
// Fetch MCP server and no longer requires BRAVE_API_KEY.
var agents = []Agent{
	{Name: "biz-daily", Title: "业务数据体检", MCPServers: []string{"dbhub"}, NeedsSample: true},
	{Name: "docs-diff", Title: "文档变更", MCPServers: []string{"docs-readonly"}, NeedsDocs: true},
	{
		Name: "github-radar", Title: "GitHub 星标雷达", MCPServers: []string{"github"},
		EnvKeys: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"},
	},
	{Name: "industry-brief", Title: "行业简报", MCPServers: []string{"fetch"}},
	{
		Name: "repo-digest", Title: "代码仓库日报", MCPServers: []string{"github"},
		EnvKeys: []string{"GITHUB_PERSONAL_ACCESS_TOKEN"}, Skill: "repo-digest",
	},
}

// Agents 返回内置 Agent 清单的副本：调用方可以放心排序 / 过滤，改不动本包的清单。
// Agents returns a copy so callers cannot mutate the canonical list.
func Agents() []Agent {
	out := make([]Agent, len(agents))
	for i, agent := range agents {
		out[i] = agent
		out[i].MCPServers = append([]string(nil), agent.MCPServers...)
		out[i].EnvKeys = append([]string(nil), agent.EnvKeys...)
	}
	return out
}

// AgentResult 是一只 Agent 的安装结果；技能目录单独记状态（两者可能一个新建、一个保留）。
// AgentResult records the agent directory state and, separately, the skill one.
type AgentResult struct {
	Name       string
	State      string
	Skill      string
	SkillState string
}

// Install 把五只 Agent（与技能）装进工作区：缺失的重建、已存在的逐字节保留。
// 新建的 Agent 目录**清空模型**——空模型让装载期按「blank model」拒绝注册，正好表达
// 「装好但等配置」；状态文件由随后执行的启动基线写成 `disabled`（Bootstrap）。
// interpreter 非空时把 AGENT.md 里的 {{BASALT_PYTHON}} 替换成该绝对路径；为空则保留
// 占位符（安装时没找到解释器），由 SETUP 指引运维自行登记。
//
// Install copies the missing agent trees and skills, blanks the model of every freshly
// created definition and, when an interpreter is known, substitutes the script
// placeholder. Lifecycle state is written by the startup baseline right after.
func Install(wsRoot string, force bool, interpreter string) ([]AgentResult, error) {
	results := make([]AgentResult, 0, len(agents))
	for _, spec := range agents {
		target := filepath.Join(wsRoot, agentsDirName, spec.Name)
		state, err := CopyTree(path.Join("daily", spec.Name), target, force, map[string]bool{"skills": true})
		if err != nil {
			return nil, fmt.Errorf("安装 %s：%w", spec.Name, err)
		}
		agentFile := filepath.Join(target, agentDefinitionFile)
		if state == StateInstalled {
			if err := BlankModel(agentFile); err != nil {
				return nil, fmt.Errorf("安装 %s：%w", spec.Name, err)
			}
		}
		// 保留态也要对齐占位符（只动这一处）：旧版本装出来的目录可能还是 `python` 占位，
		// 升级后第一次安装就该把它们替换成与白名单同一串的绝对路径。
		// Kept installs get only the placeholder aligned; their model bindings stay intact.
		if err := ResolveAgentPython(agentFile, interpreter); err != nil {
			return nil, fmt.Errorf("安装 %s：%w", spec.Name, err)
		}
		result := AgentResult{Name: spec.Name, State: state, Skill: spec.Skill}
		if spec.Skill != "" {
			skillTarget := filepath.Join(wsRoot, skillsDirName, spec.Skill)
			skillState, err := CopyTree(path.Join("daily", spec.Name, "skills", spec.Skill), skillTarget, force, nil)
			if err != nil {
				return nil, fmt.Errorf("安装 %s 技能：%w", spec.Name, err)
			}
			result.SkillState = skillState
		}
		results = append(results, result)
	}
	return results, nil
}

// CopyTree 从内嵌素材复制一棵子树；目标已存在且不覆盖时返回 StateKept。
// skip 是目标根下一级的目录名黑名单——Agent 目录里的 skills/ 由安装器单独装到
// <workspace>/skills，复制进 Agent 目录只会让加载器打 WARN 并留下重复副本。
// CopyTree copies one embedded subtree; an existing target is kept unless forced.
// Names in skip are dropped from the first level below the root.
func CopyTree(srcRoot, dstRoot string, force bool, skip map[string]bool) (string, error) {
	if _, err := os.Stat(dstRoot); err == nil {
		if !force {
			return StateKept, nil
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
	return StateInstalled, nil
}

// BlankModel 把 frontmatter 里 provider 块的 model 值改成空字符串——空模型让 Agent
// 装载期按「blank model」拒绝注册，正好表达「装好但等配置」。
// BlankModel clears provider.model so the loader refuses to register the agent
// until an operator fills it in.
func BlankModel(agentFile string) (err error) {
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
	raw, err := dir.ReadFile(name)
	if err != nil {
		return fmt.Errorf("读取 %s：%w", agentFile, err)
	}
	lines := strings.Split(string(raw), "\n")
	done, inProvider := false, false
	for i, line := range lines {
		// 行尾的 `\r` 单独留下：素材在 Windows 检出上是 CRLF，替换整行时只该改内容，
		// 不该把这一行变成 LF——否则装出来的 AGENT.md 是混合行尾（逐字节保真用例会红，
		// 真实实例上 diff 也会漂）。
		// Keep the CR: replacing the line's content must not rewrite its line ending.
		eol := ""
		trimmed := line
		if strings.HasSuffix(trimmed, "\r") {
			eol = "\r"
			trimmed = strings.TrimSuffix(trimmed, "\r")
		}
		if trimmed == "provider:" {
			inProvider = true
			continue
		}
		if !inProvider {
			continue
		}
		if strings.HasPrefix(trimmed, "  model:") {
			lines[i] = `  model: ""` + eol
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

// firstPathElement 取相对路径的第一段（根自身返回空串），用于跳过指定子目录。
// firstPathElement returns the first segment of a relative path ("" for the root).
func firstPathElement(rel string) string {
	slash := filepath.ToSlash(rel)
	if slash == "." {
		return ""
	}
	if index := strings.IndexByte(slash, '/'); index >= 0 {
		return slash[:index]
	}
	return slash
}

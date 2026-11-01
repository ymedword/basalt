package dbmigrate

// 本文件是**五个内置 Agent 的资产白名单规格**（C-40 / 颗粒度文档 §2.13 图 11 / §3.1.2 /
// R27）：迁移验收不只算数据库表——Agent 定义、脚本、夹具、样例库、文档输入、运行报表与
// 业务状态文件都在工作区里，跑丢任何一类，切换后的实例看起来"装好了但跑不起来"。
//
// 四条纪律：
//  1. 五个 Agent 的名字与技能名**只从 `internal/builtin` 取**——本文件不复制那份唯一
//     清单；新增 / 改名内置 Agent 时这里无需改动。
//  2. 路径一律**相对工作区根** `<WorkDir>/.basalt`、正斜杠；本文件**不**指向仓库
//     `examples/**`（那是模板，不是实例资产），也不声明任何排除项（库 / 锁 / 名册 /
//     密钥 / 日志）。
//  3. 凭据只以**变量名**出现在就绪核对里（`builtin.Agents().EnvKeys` 由迁移报告读取）；
//     资产清单**不得**承接任何凭据值——`AssetSpec` 根本没有承接值的字段。
//  4. `RequiredFor` 标注"缺了它哪个 Agent 不完整"，因此引导文件对五个 Agent 全标；
//     实例级目录（memory / archive / versions）不属于某一只，留空。
//
// This file is the whitelist spec of the five built-in agents' workspace assets. Agent names
// and skill names come from internal/builtin; every path is workspace-relative and never
// points at the repository templates. Credentials appear only as variable names in the
// migration report, never as values in an asset spec.

import (
	"path/filepath"
	"strings"

	"github.com/ymedword/basalt/internal/builtin"
)

// 工作区根的固定目录名与文件（来源：`internal/workspace/templates.go` 的创建清单、
// `lifecycle.go` 的 control 布局、`internal/builtin` 的安装落点）。这里写的是**产品
// 布局字面量**，不复制实现——目录名变了，这几个常量跟着改，测试会盯住五只 Agent 不被漏掉。
// Workspace-level layout literals: bootstrap files and the instance-scoped business dirs.
const (
	assetAgentsDirName = "agents"
	assetSkillsDirName = "skills"

	// assetControlAgentsDir 来自 `internal/workspace/lifecycle.go`：
	// `control/agents/<name>.json` 是生命周期状态文件。
	assetControlAgentsDir = "control/agents"

	// assetDemosDir 是 `basalt demo install` 的落点：安装清单、MCP 配置、样例库脚本与
	// 导出的 docs-readonly 源码（`internal/cli/demo.go`）。
	assetDemosDir = "demos"
)

// builtinBootstrapFiles 是工作区根的引导文件（AGENTS.md / SOUL.md / USER.md /
// mcp_servers.yaml）：内容注入每只 Agent 的提示词与工具池，缺一项五只都算不完整。
// Bootstrap files shared by every agent.
var builtinBootstrapFiles = []string{
	"AGENTS.md",
	"SOUL.md",
	"USER.md",
	"mcp_servers.yaml",
}

// builtinInstanceDirs 是实例级业务目录：全局记忆、归档的 Agent 目录、技能版本快照与
// 生命周期状态目录。它们是**共享**资产，不归属任何单只 Agent，`RequiredFor` 留空。
// Instance-scoped business dirs; none of them belongs to a single agent.
var builtinInstanceDirs = []string{
	"memory",
	"archive",
	"versions",
	"control",
}

// builtinAgentBusinessFiles 是每只内置 Agent 在**自己目录根**下的业务输入 / 状态文件
// （颗粒度文档 §2.13 图 11 的 FS 行）：MCP 工具返回的原文由 Agent 写回这里，脚本再读
// 它们产出 `runs/` 报告。清单只记文件名——`BuiltinAssetSpecs` 负责拼出完整相对路径。
//
// 这里**不要求**与 `builtin.Agents()` 一一对应：缺条目的 Agent 只是没有根级业务文件，
// 它的目录本身仍按 `agents/<name>` 整棵进清单，不会从清单消失。
// Per-agent root-level business files; agents without entries simply have none.
var builtinAgentBusinessFiles = map[string][]string{
	"biz-daily":      {"orders.json"},
	"docs-diff":      {"documents.json", "metadata.json"},
	"github-radar":   {"github_search.json"},
	"industry-brief": {"search.json", "pages.json"},
	"repo-digest":    {"pull_requests.json", "issues.json", "releases.json"},
}

// BuiltinAssetSpecs 返回五个内置 Agent 的资产白名单——`BuildAssetManifest` 的输入。
//
// 覆盖范围（逐项对应颗粒度文档 §2.13 的七项清单）：
//   - 定义：`agents/<name>`（默认整棵，含 AGENT.md）、`skills/<skill>`、引导文件；
//   - 配置：`mcp_servers.yaml`、`demos/mcp`（DBHub 只读配置）；
//   - 资产：`scripts` / `fixtures` / `samples/biz-sample.db` / `samples/docs` / `demos/src`；
//   - 业务状态：`state` / `runs`、Agent 根级业务输入文件、`control/agents/<name>.json`。
//
// **不覆盖**（按设计留给未归类核查）：`sessions/`（骨架目录，当前无写入方）、
// `demos/deps/`（操作者自行投放的依赖，可能含二进制，逐条处置比整目录放行安全）、以及
// 库 / 锁 / 名册 / 密钥 / 日志等排除项（见 `asset.go`）。
//
// 返回的每条路径都相对工作区根、正斜杠；`Kind` 只取 `asset.go` 冻结的七个枚举值。
// BuiltinAssetSpecs returns the whitelist used by BuildAssetManifest for the five built-in
// agents; every path is workspace-relative and the kinds are the frozen enum values.
func BuiltinAssetSpecs() []AssetSpec {
	agents := builtin.Agents()
	allAgents := make([]string, 0, len(agents))
	for _, agent := range agents {
		allAgents = append(allAgents, agent.Name)
	}

	specs := make([]AssetSpec, 0, len(builtinBootstrapFiles)+len(builtinInstanceDirs)+len(agents)*6+6)
	index := make(map[string]int, cap(specs))
	// add 追加一条白名单；同一路径被两个入口声明时合并 RequiredFor（先到的 Kind 保留）。
	// add appends one whitelist entry, merging RequiredFor for a repeated path.
	add := func(rel, kind string, requiredFor ...string) {
		if position, ok := index[rel]; ok {
			specs[position].RequiredFor = mergeRequiredFor(specs[position].RequiredFor, requiredFor)
			return
		}
		index[rel] = len(specs)
		specs = append(specs, AssetSpec{
			Path:        rel,
			Kind:        kind,
			RequiredFor: append([]string(nil), requiredFor...),
		})
	}

	for _, name := range builtinBootstrapFiles {
		add(name, AssetKindAgentDef, allAgents...)
	}
	for _, dir := range builtinInstanceDirs {
		add(dir, AssetKindState)
	}
	// `agents` / `skills` 给**整个目录**一个默认类别：用户自建的 Agent 与技能也是业务资产，
	// 不该因为"不是这五只"就掉进未归类核查。逐 Agent 的细化声明在下面（更长路径优先）。
	add(assetAgentsDirName, AssetKindAgentDef)
	add(assetSkillsDirName, AssetKindAgentDef)

	for _, agent := range agents {
		base := assetAgentsDirName + "/" + agent.Name
		add(base, AssetKindAgentDef, agent.Name)
		add(base+"/scripts", AssetKindScript, agent.Name)
		add(base+"/fixtures", AssetKindFixture, agent.Name)
		add(base+"/state", AssetKindState, agent.Name)
		add(base+"/runs", AssetKindReport, agent.Name)
		for _, rel := range builtinAgentBusinessFiles[agent.Name] {
			add(base+"/"+rel, AssetKindState, agent.Name)
		}
		if agent.Skill != "" {
			add(assetSkillsDirName+"/"+agent.Skill, AssetKindAgentDef, agent.Name)
		}
		add(assetControlAgentsDir+"/"+agent.Name+".json", AssetKindState, agent.Name)
		if agent.NeedsSample {
			add(workspaceRelativePath(builtin.SampleDBPath), AssetKindSampleDB, agent.Name)
		}
		if agent.NeedsDocs {
			add(workspaceRelativePath(builtin.SampleDocsPath), AssetKindDocInput, agent.Name)
		}
	}

	// 样例库的生成链与 DBHub 配置：样例库本身由 `demos/scripts` 里的脚本按 biz-daily 的
	// fixtures 生成，配置缺失则 DBHub 起不来——两者都记在 biz-daily 名下。
	// The sample database and DBHub config chain is required by biz-daily.
	add(assetDemosDir+"/scripts", AssetKindScript, "biz-daily")
	add(workspaceRelativePath(builtin.DBHubConfigPath), AssetKindState, "biz-daily")
	// 安装清单与导出的 docs-readonly 源码不属于某一只 Agent：前者记录装了什么，后者是
	// 运维重建组件的输入。
	add(assetDemosDir+"/install.json", AssetKindState)
	add(assetDemosDir+"/src", AssetKindScript)

	return specs
}

// workspaceRelativePath 把 `internal/builtin` 的固定落点换算成相对工作区根的路径：
// 落点只在该包定义一次，这里不复制字面量（把工作区根设成 "." 再用正斜杠收敛）。
// workspaceRelativePath converts a builtin-fixed location into a workspace-relative path.
func workspaceRelativePath(resolve func(wsRoot string) string) string {
	return filepath.ToSlash(resolve("."))
}

// mergeRequiredFor 合并两份 Agent 名清单（保持先到顺序、去重）；重复声明同一路径时用。
// mergeRequiredFor unions two agent-name lists, preserving first-seen order.
func mergeRequiredFor(existing, added []string) []string {
	if len(added) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(added))
	merged := make([]string, 0, len(existing)+len(added))
	for _, name := range append(append([]string(nil), existing...), added...) {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		merged = append(merged, trimmed)
	}
	return merged
}

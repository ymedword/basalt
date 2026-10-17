package workspace

import (
	"fmt"

	"github.com/goccy/go-yaml"
)

// 目录清单：顺序即创建顺序，也是报告顺序（CLI 输出稳定可比）。
// Directories in creation order; the report keeps the same order so that the
// CLI output stays stable and comparable between runs.
//
// memory/agents/ **不在**清单里：Agent 层记忆在 save_memory 时按需落盘，预建
// 空目录会把"有没有 Agent 层记忆"这个事实抹平（需求 §5.1 产物树注释）。
// memory/agents/ is deliberately absent: it appears on demand.
var directories = []string{
	"agents",
	"skills",
	"memory",
	"logs",
	"archive",
	"sessions",
}

// fileTemplate 是一个引导文件：相对工作区根的路径 + 首次创建时写入的内容。
// fileTemplate is one bootstrap file: a path relative to the workspace root plus
// the content written on first creation.
type fileTemplate struct {
	path    string
	content string
}

// fileTemplates 是引导文件清单（顺序即报告顺序）。已存在的文件一律不写——
// 内容是基线不是约束，工作区主人可以任意改写。
// Bootstrap files in report order; existing files are never rewritten.
var fileTemplates = []fileTemplate{
	{path: "AGENTS.md", content: templateAgentsMD},
	{path: "SOUL.md", content: templateSoulMD},
	{path: "USER.md", content: templateUserMD},
	{path: "mcp_servers.yaml", content: templateMCPServersYAML},
	{path: "memory/MEMORY.md", content: templateMemoryMD},
}

// 模板内容判据（颗粒度文档 §3.1 / §4.2）：MEMORY.md 含「## 核心记忆 / ## 归档
// 记忆」两分区；mcp_servers.yaml 为空表且可被 goccy/go-yaml 解析为空映射。
// 其余正文只描述用途与边界，不写进任何后续 US 会推翻的产品语义。
const (
	templateAgentsMD = `# AGENTS.md —— 项目级行为说明

本文件随工作区创建，是 Bootstrap 三件之一（AGENTS.md / SOUL.md / USER.md），
内容作为上下文注入系统提示词；只写"做什么、不做什么"，人格定义见 SOUL.md。

## 工作区约定

- 工作区根目录为 .basalt/：agents/ 下每个子目录是一个 Agent，skills/ 是公共技能库。
- Agent 的工具池只来自其 AGENT.md 的 tools: 声明，未声明的工具不可用。
- 长期记忆分层：全局层 memory/MEMORY.md，Agent 层 memory/agents/<name>/MEMORY.md（按需创建）。

## 行为约定

- 结论先行：先给可执行的结果，再给必要的依据。
- 涉及删除、覆盖、对外发送的动作，先说清影响面再执行。
- 不确定的事实不编造：无法核实就明说"未核实"。
`

	templateSoulMD = `# SOUL.md —— 人格定义

本文件随工作区创建，是 Bootstrap 三件之一：描述 Agent 的身份与说话方式。
按需改写；basalt init 只在文件缺失时写入，已有内容不会被覆盖。

## 人格基线

- 语气：直接、克制、不客套；对不确定的事如实说明。
- 身份：面向企业内部运维与研发场景的助手。
- 边界：不代替用户做不可逆决定（删除、对外发送、凭证变更）。
`

	templateUserMD = `# USER.md —— 用户偏好（只读不写）

本文件随工作区创建，是 Bootstrap 三件之一：装载为上下文，供 Agent 了解使用者偏好。
Agent 不写本文件；需要记住的事写进长期记忆 memory/MEMORY.md。

## 偏好

- 语言：中文；英文术语保留原文。
- 输出：先给结论与可执行步骤，细节按需展开。
`

	templateMCPServersYAML = `# MCP server 注册表（默认空表）。
# 形态：顶层映射 + servers 列表；条目字段 name / transport / command / env。
# AGENT.md 的 mcp_servers: 声明引用这里的 name。v1 只支持 stdio。
{}
`

	templateMemoryMD = `# MEMORY.md —— 长期记忆 · 全局层

全局层长期记忆：所有 Agent 共享。Agent 层记忆在 memory/agents/<name>/MEMORY.md，
两层不叠加。basalt init 只在本文件缺失时创建，已有内容不会被覆盖。

## 核心记忆

## 归档记忆
`
)

// 占位文本：create 模板里"没配完"的**可见**标记（契约 agent-md.md §2 字段表 + 技术方案
// §11.5：占位模型名是普通文本、天然放行——"没配完"在文件里一眼可见，报错发生在调用模型时）。
// Placeholder texts that make an unfinished template visible in the file itself.
const (
	placeholderProviderName = "请在此填写 provider 名"
	placeholderModelName    = "请在此填写模型名"
)

// agentTemplateDTO 是 profile create 模板的落盘形态：**经 yaml.Marshal 编码**——不得字符串
// 插值（名字可含空格 / 冒号 / 中文，插值会写坏 YAML）；provider.model 恒为占位；tools 显式
// 声明两件记忆工具（US-6 起——出厂即可记忆）；其余列表字段（skills / mcp_servers /
// bootstrap / schedules）省略
// （颗粒度文档 §3.1 CreateAgent 注释 + data-model「profile create 模板」）。
// agentTemplateDTO is the on-disk shape of a created template; names go through the encoder.
type agentTemplateDTO struct {
	Name        string                `yaml:"name"`
	Description string                `yaml:"description"`
	Provider    agentTemplateProvider `yaml:"provider"`
	Tools       []string              `yaml:"tools"`
}

// agentTemplateProvider 是模板里的 provider 段：name 取配置首项（或占位文本）、model 恒为占位。
// agentTemplateProvider is the provider section of a created template.
type agentTemplateProvider struct {
	Name  string `yaml:"name"`
	Model string `yaml:"model"`
}

// agentTemplate 渲染一份**可装载**的 AGENT.md 模板（frontmatter 围栏 + 围栏外正文）。
// providers 为空 → provider.name 写占位文本（命令面另加"模板尚未注册"的说明）。
// agentTemplate renders a loadable AGENT.md template.
func agentTemplate(name string, providers []string) (string, error) {
	providerName := placeholderProviderName
	if len(providers) > 0 {
		providerName = providers[0] // 配置数组首项（§2.3 流程 C）
	}

	dto := agentTemplateDTO{
		Name:        name,
		Description: "一句话说明这个 Agent 的职责（可留空）",
		Provider:    agentTemplateProvider{Name: providerName, Model: placeholderModelName},
		Tools:       []string{"save_memory", "recall_memory"}, // 默认两件记忆工具（US-6；声明即绑定——池外工具名会让本 Agent 整只不注册）
	}

	front, err := yaml.Marshal(dto)
	if err != nil {
		return "", fmt.Errorf("编码 Agent 模板: %w", err)
	}
	return "---\n" + string(front) + "---\n\n" + agentTemplateBody(name) + "\n", nil
}

// agentTemplateBody 是模板的正文（段一素材之一）：只说明"该写什么"，不写进任何后续 US
// 会推翻的产品语义。
// agentTemplateBody is the prose body of a created template.
func agentTemplateBody(name string) string {
	return "你是「" + name + "」。把这一段改成这个 Agent 的职责与行为约定：做什么、不做什么、\n" +
		"输出成什么样。本段正文与 bootstrap: 声明的文件会作为系统提示词的第一段注入。\n"
}

package workspace

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
# 键为 server 名，值为启动配置；AGENT.md 的 mcp_servers: 声明引用这里的名字。
{}
`

	templateMemoryMD = `# MEMORY.md —— 长期记忆 · 全局层

全局层长期记忆：所有 Agent 共享。Agent 层记忆在 memory/agents/<name>/MEMORY.md，
两层不叠加。basalt init 只在本文件缺失时创建，已有内容不会被覆盖。

## 核心记忆

## 归档记忆
`
)

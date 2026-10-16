package core

// 本文件是 Agent 目录的契约面：AGENT.md frontmatter 的解析结果与装载条目。
// 全部为**声明**——零可执行语句（解析与校验的实现进 internal/workspace）。
// 字段名逐字对齐 docs/contracts/agent-md.md §2 字段表；**不渲染序列化标签**——
// 契约层不绑序列化库，映射由 workspace 的私有 DTO 显式完成。
// This file is the contract surface for agent directories: the parsed shape of
// AGENT.md frontmatter and the load entry. Declarations only; parsing and
// validation live in internal/workspace. The structs carry no serialization tags
// on purpose — the contract layer must not bind to a YAML library, so the
// workspace package performs the mapping through its own DTO.

// Profile 是一只 Agent 的定义：AGENT.md frontmatter 的解析结果。
// Profile is the definition of one agent: the parsed AGENT.md frontmatter.
type Profile struct {
	Name        string      // name（必填；等于目录名）
	Description string      // description
	Identity    Identity    // identity.*
	Provider    ProviderRef // provider.*
	Tools       []string    // tools（不声明 = 空池）
	Skills      []string    // skills（声明即绑定；元素 = 技能库目录名）
	MCPServers  []string    // mcp_servers（元素 = mcp_servers.yaml 里的 server 名）
	Bootstrap   []string    // bootstrap（相对工作区根）
	Channels    []Channel   // channels（未声明 → [{name: cli}]；显式 [] → 空）
	Settings    Settings    // settings.*
	Schedules   []Schedule  // schedules（见 §3 契约）
}

// Identity 是人格面：短名进提示词的外观，长提示词拼在系统提示词开头。
// Identity is the persona face: the short name and the leading prompt.
type Identity struct {
	AgentName string // identity.agent_name；缺省 = name（解析后必非空）
	Prompt    string // identity.prompt；缺省空
}

// ProviderRef 是供应商引用——名字必须已注册、模型不得空白；Temperature 用指针区分
// "未声明"（nil = 供应商缺省）与"显式 0"（0 是合法温度）。
// ProviderRef is the provider reference: a registered name and a non-blank model.
// Temperature distinguishes absent (nil, the vendor default) from an explicit zero.
type ProviderRef struct {
	Name        string   // provider.name（必填；必须已注册）
	Model       string   // provider.model（必填；空白即拒；占位符「请在此填写模型名」是普通文本、天然放行）
	Temperature *float64 // provider.temperature；nil = 未声明（供应商缺省）；显式值 0–2 且有限
}

// Channel 是接入渠道声明；Config 是自由映射，内容归渠道实现，本 US 只解析不校验。
// Channel is a channel declaration; Config is a free mapping owned by the channel
// implementation and is parsed but never validated here.
type Channel struct {
	Name   string         // channels[].name
	Config map[string]any // channels[].config（技术方案 §11.2 样例形态；本 US 只解析不校验）
}

// Settings 是运行参数上限；两字段经指针保留"显式 0"（不得写 if v == 0 { v = 10 } 形态）。
// Settings holds the run limits; the pointers preserve an explicit zero.
type Settings struct {
	MaxIterations   int // settings.max_iterations；缺省 10（显式 0 保留为 0）
	MaxHistoryTurns int // settings.max_history_turns；缺省 20（显式 0 保留）
}

// Schedule 是 schedules[] 的一项；Zone 解析后必非空（缺省已填 "UTC"——不得回落进程本地时区）。
// Schedule is one entry of schedules[]; Zone is non-empty after parsing (defaulted to
// UTC and never falling back to the host time zone).
type Schedule struct {
	ID      string // id；单项可缺省（派生 <agent>-1，仅当恰一个任务）；≥2 项每项必须显式
	Cron    string // cron；6 段（秒 分 时 日 月 周）
	Zone    string // zone；缺省 "UTC"；"Local" 显式拒
	Message string // message（必填）
}

// AgentEntry 是一个 Agent 目录的装载结果——注册成功与失败**都有一条**（失败不静默消失）。
// 两段语义互相独立：Profile 非 nil ⇔ frontmatter 解析成功；Registered ⇔ 八条校验全过。
// AgentEntry is the load result of one agent directory; every directory yields one
// entry, registered or not. The two facts are independent: a non-nil Profile means the
// frontmatter parsed, and Registered means every check passed.
type AgentEntry struct {
	Name         string   // 目录名（= Agent 名）
	Registered   bool     // 八条校验全过（+ temperature 字段约束）
	Profile      *Profile // frontmatter 解析成功时非 nil（未注册也可非 nil——供 list / show 展示）
	RejectReason string   // Registered == false 时非空：reason（契约 §2.1 字面量原样）+ detail 的人读合并
}

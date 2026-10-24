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
	Name        string // name（必填；等于目录名）
	Description string // description
	// DisplayName 是管理台展示名（R6）：只影响列表 / 详情 / 面包屑，不参与提示词、
	// 目录名、agent_uid 或运行标识；去空白后为空 = 未声明，展示时回退 Name。
	// DisplayName is display-only; empty means undeclared and views fall back to Name.
	DisplayName string      // display_name
	Identity    Identity    // identity.*
	Provider    ProviderRef // provider.*
	Tools       []string    // tools（不声明 = 空池）
	Skills      []string    // skills（声明即绑定；元素 = 技能库目录名）
	MCPServers  []string    // mcp_servers（元素 = mcp_servers.yaml 里的 server 名）
	Bootstrap   []string    // bootstrap（相对工作区根）
	// NotifyChannels 是允许该 Agent 使用的通知渠道名（AGENT.md 的 notify_channels）。
	// 非空 = 白名单：Run 在准入时冻结，notify 工具只放行集合内的渠道；不声明或空数组
	// = 不限制（兼容既有 Agent）。要完全关闭通知，请从 tools 移除 notify。
	// NotifyChannels is the allowlist of notification channels for this agent. A non-empty
	// declaration is frozen at admission and enforced by the notify tool; absent or empty
	// means unrestricted for backward compatibility.
	NotifyChannels []string
	// NotifyMode 是通知意图三态（v3 起）；未声明时为空串，由准入按兼容规则归一。
	// NotifyMode is the three-state notification intent; empty means undeclared and
	// admission applies the compatibility rule.
	NotifyMode NotifyMode
	// ResourceBindings 是 frontmatter 的 `resource_bindings`（v3 起）：AGENT.md 是唯一
	// 权威，数据库投影可重建。不声明 = 零值（迁移前 Agent / 手工构造的条目）。
	// ResourceBindings mirrors the frontmatter block; the file is authoritative.
	ResourceBindings ResourceBindings
	Channels         []Channel  // channels（未声明 → [{name: cli}]；显式 [] → 空）
	Settings         Settings   // settings.*
	Schedules        []Schedule // schedules（见 §3 契约）
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
	Config map[string]any // channels[].config（技术方案 §3.1.3 样例形态；本 US 只解析不校验）
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
	// DeliverTo 是系统代投目标通知渠道的**资源 UID**（R7）：空串 = 不代投。
	// 只接受 UID、不接受渠道名——同名重建会生成新 UID，按名字查找会把旧任务
	// 送往新机器人。保存时与运行开始时都校验资格，运行开始后固定本次目标。
	// DeliverTo is the notify-channel resource uid the scheduler delivers the final
	// answer to; empty means no system delivery. Names are never accepted here.
	DeliverTo string `json:"deliver_to"`
}

// AgentRejectKind 是装载期拒绝的**机器可判**分类，字面量逐字取契约的 reason 值
// （[agent-md.md](docs/contracts/agent-md.md) / Compatibility §5.1）：英文原样进日志与
// 审计，管理台按常量分支映射中文，**不得**对运行期字符串做前缀匹配。
// AgentRejectKind is the machine-judgeable rejection category; the literals are the
// contract's English reason values and are the only values logs and audits carry.
type AgentRejectKind string

// 装载拒绝的全部分类（闭集）。新增分类必须同时补 web 层的中文投影与测试。
// The closed rejection set; a new kind must also extend the Chinese projection.
const (
	AgentRejectReadFailed              AgentRejectKind = "read AGENT.md failed"
	AgentRejectInvalidFrontmatter      AgentRejectKind = "invalid frontmatter"
	AgentRejectInvalidResourceBindings AgentRejectKind = "invalid resource_bindings"
	AgentRejectNameMismatch            AgentRejectKind = "name mismatch"
	AgentRejectProviderNotRegistered   AgentRejectKind = "provider not registered"
	AgentRejectBlankModel              AgentRejectKind = "blank model"
	AgentRejectInvalidTemperature      AgentRejectKind = "invalid provider.temperature"
	AgentRejectToolNotRegistered       AgentRejectKind = "tool not registered"
	AgentRejectSkillNotFound           AgentRejectKind = "skill not found"
	AgentRejectMCPServerSkipped        AgentRejectKind = "mcp server skipped"
	AgentRejectBootstrapMissing        AgentRejectKind = "bootstrap file missing"
	AgentRejectInvalidSchedule         AgentRejectKind = "invalid schedule"
	AgentRejectInvalidNotifyChannels   AgentRejectKind = "invalid notify_channels"
	AgentRejectInvalidNotifyMode       AgentRejectKind = "invalid notify_mode"
	AgentRejectSkillVersionMissing     AgentRejectKind = "skill version missing"
)

// AgentDefinitionRejectedError 表示一份候选 AGENT.md 未通过装载期校验。Kind 决定
// 管理台文案，Detail 是点名细节；Error() 与装载器的人读合并串逐字一致（英文，
// 只进日志 / 审计），**不得**直接回显给用户。
// AgentDefinitionRejectedError carries the typed rejection out of the candidate gate;
// its Error text is the English diagnostic and must never be echoed to the console.
type AgentDefinitionRejectedError struct {
	Kind   AgentRejectKind
	Detail string
	Cause  error
}

// Error 返回英文诊断串（`reason: detail`），与装载器的 mergedReason 同形。
// Error renders the English diagnostic, matching the loader's merged reason.
func (e *AgentDefinitionRejectedError) Error() string {
	if e.Detail == "" {
		return string(e.Kind)
	}
	return string(e.Kind) + ": " + e.Detail
}

// Unwrap 暴露原因链：绑定块结构非法等哨兵仍可经 errors.Is 判定。
// Unwrap keeps sentinels (such as the bindings sentinel) recognizable.
func (e *AgentDefinitionRejectedError) Unwrap() error { return e.Cause }

// AgentEntry 是一个 Agent 目录的装载结果——注册成功与失败**都有一条**（失败不静默消失）。
// 两段语义互相独立：Profile 非 nil ⇔ frontmatter 解析成功；Registered ⇔ 八条校验全过。
// 第三态 Unverified 只出现在轻路径（不连 MCP、无法判定连接结果）：`Registered == false &&
// Unverified == true` 表示「可能依赖 MCP，需重路径确认」——只改展示与摘要，不进运行面。
// AgentEntry is the load result of one agent directory; every directory yields one
// entry, registered or not. The two facts are independent: a non-nil Profile means the
// frontmatter parsed, and Registered means every check passed. Unverified is the
// light-path third state (Registered == false && Unverified == true): display only.
type AgentEntry struct {
	Name       string   // 目录名（= Agent 名）
	Registered bool     // 八条校验全过（+ temperature 字段约束）
	Unverified bool     // 轻路径待确认（只改展示与摘要；运行面仍只收 Registered）
	Profile    *Profile // frontmatter 解析成功时非 nil（未注册也可非 nil——供 list / show 展示）
	// RejectKind 是拒载的**机器可判**分类（零值 = 未被拒）：管理台文案按它映射成中文，
	// **不得**按 RejectReason 的英文文本做字符串匹配（R8 裁决 1 / SC-033）。
	// RejectDetail 是分类的点名细节（名字 / 相对路径 / 中文说明）；日志与审计照旧
	// 使用 RejectReason 的英文合并串，两者不得互相替代。
	// RejectKind is the machine-judgeable category behind a rejection; the console maps
	// it to Chinese and never matches on the English reason text.
	RejectKind   AgentRejectKind
	RejectDetail string
	RejectReason string // Registered == false 且 Unverified == false 时非空：reason + detail 的人读合并（英文，进日志 / CLI）

	// Definition 是装载 / 候选校验**实际校验过的那一份** AGENT.md 字节：运行快照从它出，
	// **不得**回读磁盘。准入与快照之间若隔着一份尚未发布的磁盘编辑（合法改写或已被
	// watcher 拒绝的坏改写），回读会让清单与实际模型调用分叉，或让坏编辑挡住新运行
	//（人工 review ② 缺陷 1）。手工构造的条目（直接调用方 / 测试）可为 nil——冻结时退回读盘。
	// Definition holds the exact AGENT.md bytes the loader validated; run snapshots freeze
	// from it so an unpublished or rejected disk edit can never split manifest from call.
	Definition []byte
}

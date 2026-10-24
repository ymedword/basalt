package web

// 本文件是 HTTP 响应视图的**唯一**投影面（模块卡 §3.1）：把 core / 端口里的记录转成
// 契约 schema（openapi.yaml）声明的形状。两条固定口径（约束 34）：① 所有数组字段空时
// 输出 `[]` 而不是 `null`；② 时间字段一律 RFC3339 UTC，零值渲染为空串（"不知道时间"
// 不得伪装成 0001 年）。原始原因不在这里出站——`runView` 只经 `publicRunReason` 投影。
// The only projection surface for response views: empty collections marshal to `[]`,
// times are RFC3339 UTC (zero renders empty), and raw run reasons never leave this layer.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// InfoView 是 /info 的响应体（契约 components.schemas.InfoView）。
// InfoView is the /info response body.
type InfoView struct {
	Version       string         `json:"version"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	Agents        int            `json:"agents"` // 已注册数，不是装载总数（约束 25）
	Tools         int            `json:"tools"`
	MCPServers    int            `json:"mcp_servers"` // 已连接数（Options.MCPConnected）
	Providers     []ProviderView `json:"providers"`
}

// ProviderView 是 provider 摘要：**没有**凭证与 base_url 字段（约束 25）。
// ProviderView carries no credential or base_url fields.
type ProviderView struct {
	Name       string   `json:"name"`
	Models     []string `json:"models"`
	Configured bool     `json:"configured"`
}

// ProfileView 是 /profiles 的元素（契约 components.schemas.ProfileView）：
// lifecycle 是状态文件现状（缺失或损坏时 state 为 null、valid=false），runnable 是
// 「已注册 + active + 资源引用全部有已发布版本」的只读判断，不是准入闸门本身
// （真正的准入仍在 core 内按发布指针解析并固定运行时）。
// ProfileView is one /profiles element; lifecycle reflects the state file as-is and
// runnable is a read-only "registered + active + published pointers exist" hint, never
// the admission gate itself.
type ProfileView struct {
	Name string `json:"name"`
	// DisplayName 是展示投影（R6）：AGENT.md 未声明或为空时回退为技术名 Name，
	// 恒非空；它只影响展示，技术名、目录名与 UID 不受影响。
	// DisplayName is the resolved display value and never empty; the technical name
	// remains available in Name and is unaffected.
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	Tools       []string `json:"tools"`
	Skills      []string `json:"skills"`
	// NotifyMode 是通知意图的**有效值**（`none` / `all_eligible` / `allowlist`）：
	// 显式声明逐字投影，未声明按兼容规则推断（与准入同一份 `core.EffectiveNotifyMode`）。
	// 它是契约枚举，**不得**为空、**不得**出现非法字面量——声明非法或没有定义时按
	// 「未声明」的兼容规则取值，真实后果（该 Agent 不可运行）由 `problems` 与
	// `runnable` 说明，不由本字段兜底。
	// NotifyMode is the effective intent; an invalid declaration falls back to the
	// undeclared reading and the real consequence shows in problems / runnable.
	NotifyMode string `json:"notify_mode"`
	// NotifyChannels 是 AGENT.md 的 notify_channels 声明：非空 = 该 Agent 只能使用
	// 这些通知渠道；空 = 不限制。它是**声明**，不是渠道存在性判断。
	// NotifyChannels is the declared allowlist; empty means unrestricted.
	NotifyChannels []string           `json:"notify_channels"`
	Schedules      int                `json:"schedules"`
	Lifecycle      AgentLifecycleView `json:"lifecycle"`
	Registered     bool               `json:"registered"`
	Runnable       bool               `json:"runnable"`
	Problems       []string           `json:"problems"`
}

// ToolView 是 /tools 的元素；Source 枚举 builtin | mcp | code。
// ToolView is one /tools element; Source is builtin, mcp, or code.
type ToolView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
}

// MemoryView 是 /memory 的响应体：只读全局层，两分区带标题、空分区为空串。
// MemoryView is the /memory body: the global layer's two titled partitions.
type MemoryView struct {
	Scope   string `json:"scope"`   // global | agent
	Agent   string `json:"agent"`   // 全局层为空串
	Backend string `json:"backend"` // markdown | sqlite
	Core    string `json:"core"`
	Archive string `json:"archive"`
}

// SandboxView 是 /sandbox 的响应体（契约 components.schemas.SandboxView）：四类白名单
// 原样投影，空集合输出 `[]`（约束 34）。其中 `http_allowed_domains` 是**当前生效**
// 值（管理台可条件写、保存即热生效，R8 裁决 2）；其余三类仍是部署者配置，改配置需
// 重启。`network_allowlist` 给出该列表的生效元数据，页面不靠猜测判断权威来源。
// SandboxView projects the four allowlists; http_allowed_domains is the live value
// (managed and hot-swapped), the other three stay deployment configuration.
type SandboxView struct {
	FileAllowedPaths     []string `json:"file_allowed_paths"`
	ShellAllowedCommands []string `json:"shell_allowed_commands"`
	ShellInterpreters    []string `json:"shell_interpreters"`
	HTTPAllowedDomains   []string `json:"http_allowed_domains"`
	// NetworkAllowlist 是出网域名白名单的生效元数据（R8.2.2）：`etag` 用于条件写
	// 的 If-Match，`revision` 从 1 起（0 = 管理服务未装载），`source` 为
	// config_seed | database，`updated_by` 是脱敏 actor（种子为空串）。
	// NetworkAllowlist carries the live metadata of the managed outbound allowlist.
	NetworkAllowlist NetworkAllowlistView `json:"network_allowlist"`
}

// NetworkAllowlistView 是 `network_allowlist` 的固定投影；`updated_at` 零值渲染为
// 空串（US-9 惯例），不输出 `null`。
// NetworkAllowlistView is the fixed metadata projection; a zero time renders as "".
type NetworkAllowlistView struct {
	ETag      string `json:"etag"`
	Revision  int64  `json:"revision"`
	Source    string `json:"source"`
	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
}

// ConnectionTestView 是资源「最后一次连接测试」的脱敏投影（契约
// components.schemas.ConnectionTestView）：`last_test` 为 **null** 表示从未测过——
// 页面**不得**用 availability（本地配置就绪）冒充它。`tested_at` 是 RFC3339 UTC，
// `detail` 只含固定短语（目标 host、回执码、变量名），不含地址原值与凭据。
// ConnectionTestView projects the last live test; null means never tested, and local
// readiness must never stand in for it.
type ConnectionTestView struct {
	Status   string `json:"status"` // success | failed | unknown
	TestedAt string `json:"tested_at"`
	Detail   string `json:"detail"`
}

// ConnectionTestViewOf 投影一条连接测试记录；nil 保持 nil（JSON null）。
// app 层投影模型连接时复用同一格式，避免两处各写一份时间格式。
// ConnectionTestViewOf projects one record; nil stays nil. The app layer reuses it for
// model connections so the time format exists in exactly one place.
func ConnectionTestViewOf(rec *core.ResourceConnectionTest) *ConnectionTestView {
	if rec == nil {
		return nil
	}
	return &ConnectionTestView{
		Status:   rec.Status,
		TestedAt: rec.TestedAt.UTC().Format(time.RFC3339),
		Detail:   rec.Detail,
	}
}

// NotifyChannelView 是 /notify-channels 的元素（契约 components.schemas.NotifyChannelView）：
// 地址只以 `url_host`（scheme + host）出现，原始 URL 与查询串**没有任何载体**。
// NotifyChannelView carries only the scheme+host projection; the raw URL has no carrier.
type NotifyChannelView struct {
	Name string `json:"name"`
	// ResourceUID 是渠道的稳定身份；同名重建会生成新 UID（旧绑定不自动接管）。
	ResourceUID string `json:"resource_uid"`
	Type        string `json:"type"`
	Description string `json:"description"`
	URLHost     string `json:"url_host"`
	// URLConfigured 表示存储里是否**有地址引用**（非空）。它与 URLHost 是两件事：
	// `${ENV_VAR}` 形态有引用但解析不出 host（URLConfigured=true、URLHost=""），
	// 页面**不得**把这种情况显示成"地址为空"。
	// URLConfigured reports whether a URL reference is stored; a `${VAR}` reference
	// is configured but has no host projection.
	URLConfigured bool `json:"url_configured"`
	// ReferencedBy 是扫描 Agent 文档得到的引用者名单（按名称排序、去重；归档项带
	// `archive:` 前缀）。它只是**文档引用**，不是精确绑定——精确允许范围在 Agent
	// 的 notify_channels 声明。
	// ReferencedBy lists agents whose documents mention the channel; it is a textual
	// reference scan, not the exact binding declared by notify_channels.
	ReferencedBy []string `json:"referenced_by"`
	// AddressStatus 是地址可用性的四值投影；`url_host` 为空**不等于**未配置。
	AddressStatus string `json:"address_status"`
	// 凭据面只出模式、只读引用、状态、版本号与尾号——**没有**任何明文载体。
	CredentialMode    string `json:"credential_mode"`
	CredentialRef     string `json:"credential_ref"`
	CredentialState   string `json:"credential_state"`
	CredentialVersion int    `json:"credential_version"`
	Last4             string `json:"last4"`
	// 范围与条件写：scope / owner_agent / revision / etag 由 app 层的同一份判定给出。
	Scope      string `json:"scope"`
	OwnerAgent string `json:"owner_agent"`
	Revision   int    `json:"revision"`
	ETag       string `json:"etag"`
	// Availability 是本地配置就绪三值（ready / unconfigured / unavailable）；
	// LastTest 是最后一次实测结果（null = 从未测过）。两者分开展示：本地看起来可用
	// **不等于**实测通过，页面不得把 availability 显示成"已验证"。
	// Availability is local readiness; LastTest is the live result, shown separately.
	Availability string              `json:"availability"`
	LastTest     *ConnectionTestView `json:"last_test"`
}

// SessionSummaryView 是会话摘要（契约 components.schemas.SessionSummary）。
// SessionSummaryView is the session summary contract shape.
type SessionSummaryView struct {
	SessionID    string `json:"session_id"`
	ProfileName  string `json:"profile_name"`
	Channel      string `json:"channel"`
	UserID       string `json:"user_id"`
	Status       string `json:"status"`
	MessageCount int    `json:"message_count"`
	CreatedAt    string `json:"created_at"`
	LastActiveAt string `json:"last_active_at"`
}

// MessageView 是历史窗口里的一条消息（契约 components.schemas.MessageView）；
// 推理内容与供应商扩展字段**不出站**。
// MessageView is one history message; reasoning and provider extras never go out.
type MessageView struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []ToolCallView `json:"tool_calls"`
	ToolCallID string         `json:"tool_call_id"`
	CreatedAt  string         `json:"created_at"`
}

// ToolCallView 是 assistant 消息发起的一次调用（id / name / arguments）。
// ToolCallView is one tool call issued by an assistant message.
type ToolCallView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// SessionDetailView 是会话详情：摘要字段内嵌摊平 + 最近 100 条消息。
// SessionDetailView flattens the summary fields and adds the message window.
type SessionDetailView struct {
	SessionSummaryView
	Messages []MessageView `json:"messages"`
}

// RunView 是运行结果（契约 components.schemas.RunView）；trigger 取自 RunRequest
// （scheduler 面为 cron / manual，空值回落 api），reason 只出公开投影文案
// （约束 23 / 注 5 / 注 15）。
// RunView projects one run; trigger comes from the RunRequest (cron or manual on the
// scheduler face, falling back to api when empty) and reason is the public projection.
type RunView struct {
	RunID      string `json:"run_id"`
	SessionID  string `json:"session_id"`
	Agent      string `json:"agent"`
	Channel    string `json:"channel"`
	Trigger    string `json:"trigger"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	FinalText  string `json:"final_text"`
	Iterations int    `json:"iterations"`
	ToolCalls  int    `json:"tool_calls"`
	ConfigHash string `json:"config_hash"`
	// ManifestHash 是准入清单哈希（契约 RunView.manifest_hash）；历史 Run 为空串，
	// 详情里的清单视图同时标 legacy_unrecorded（约束 24）。
	ManifestHash string `json:"manifest_hash"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
}

// RunDetailView 是运行详情：RunView 全字段摊平 + 准入清单视图（契约
// components.schemas.RunDetail 的 allOf：RunView + resource_manifest）。
// RunDetailView flattens RunView and adds the admission manifest view.
type RunDetailView struct {
	RunView
	ResourceManifest *RunResourceManifestView `json:"resource_manifest"`
}

// RunResourceManifestView 是准入清单的 HTTP 视图（契约 components.schemas
// .RunResourceManifestView）：持久结构是唯一哈希对象，本视图只做投影——技能文件
// 扁平化为逻辑工作区路径，技能只留四个哈希；**不得**由视图反推持久结构（约束 23）。
// The manifest view is a pure projection of the persisted structure; it never
// back-derives persisted shapes.
type RunResourceManifestView struct {
	FormatVersion int `json:"format_version"`
	// AgentUID 是 v2 清单固定的 Agent 稳定身份；v1 清单按契约输出空串（不是省略）——
	// 字段集恒定，读的人不必区分「没这一项」与「有但为空」。
	// AgentUID is the v2 agent identity; v1 emits an empty string per the contract.
	AgentUID string `json:"agent_uid"`
	// AgentLifecycleState 在历史 Run 上**省略**（注 17）：不知道就不填 active。
	AgentLifecycleState string `json:"agent_lifecycle_state,omitempty"`
	LegacyUnrecorded    bool   `json:"legacy_unrecorded"`
	// Provider 是 v2 的单 Provider 固定（含模型与凭据引用）；v1 清单为 null。
	Provider *ProviderManifestView `json:"provider"`
	// Providers 是 v1 兼容字段：v1 清单把当时那一个条目放在这里；v2 起恒为 `[]`。
	Providers  []ResourceVersionRefView `json:"providers"`
	MCPServers []ResourceVersionRefView `json:"mcp_servers"`
	Skills     []SkillVersionRefView    `json:"skills"`
	Files      []FileHashView           `json:"files"`
	// Notify 是 v2 起必填的通知授权（模式 + 具体渠道 UID）；v1 清单为 null，
	// 旧语义是「空白名单 = 不限制」。
	Notify *NotifyManifestView `json:"notify"`
}

// ProviderManifestView 是 v2 清单里的单个 Provider 固定（契约 components.schemas
// .ProviderManifestView）：凭据只出引用、版本与目标绑定哈希，**没有**明文字段。
// ProviderManifestView pins the provider without any plaintext credential material.
type ProviderManifestView struct {
	Name              string `json:"name"`
	Model             string `json:"model"`
	ResourceUID       string `json:"resource_uid"`
	VersionID         int64  `json:"version_id"`
	VersionNo         int    `json:"version_no"`
	CredentialRef     string `json:"credential_ref"`
	CredentialVersion int    `json:"credential_version"`
	TargetBinding     string `json:"target_binding"`
}

// NotifyManifestView 是 v2 清单冻结的通知授权（契约 components.schemas
// .NotifyManifestView）：渠道是准入时枚举出来的具体 UID 集合，**没有** `*` 通配。
// NotifyManifestView freezes concrete channel uids; there is no wildcard.
type NotifyManifestView struct {
	Mode     string                 `json:"mode"`
	Channels []NotifyChannelRefView `json:"channels"`
}

// NotifyChannelRefView 是一个冻结的渠道引用：名称只作标签，身份是 UID。
// NotifyChannelRefView carries one frozen channel ref; the uid is the identity.
type NotifyChannelRefView struct {
	Name        string `json:"name"`
	ResourceUID string `json:"resource_uid"`
}

// ResourceVersionRefView 是清单里的一个版本引用；kind 枚举 model_connection /
// mcp_server（契约 components.schemas.ResourceVersionRef）。
// ResourceVersionRefView carries the contract kind enum and version identity.
type ResourceVersionRefView struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// ResourceUID 是资源的稳定身份；v1 清单**没有**记录这一项（那时还没有 UID），
	// 所以历史行输出空串——**不得**回填当前指针的 UID 冒充当时的固定值。
	// ResourceUID stays empty for v1 rows: backfilling the current uid would invent a
	// fact the admission never froze.
	ResourceUID string `json:"resource_uid"`
	VersionID   int64  `json:"version_id"`
	VersionNo   int    `json:"version_no"`
	ContentHash string `json:"content_hash"`
}

// SkillVersionRefView 是技能版本视图（契约 components.schemas.SkillVersionRef）：
// 四个哈希中 skill_md_hash 与 l3_hash 可由持久文件清单精确复算；l1_hash /
// l2_hash 从未持久化，留空串——**不得**回读发布目录补造（I-48）。
// SkillVersionRefView exposes the four skill hashes; l1/l2 were never persisted and
// stay empty rather than being recomputed from the published directory.
type SkillVersionRefView struct {
	Name        string `json:"name"`
	VersionID   int64  `json:"version_id"`
	VersionNo   int    `json:"version_no"`
	ContentHash string `json:"content_hash"`
	SkillMDHash string `json:"skill_md_hash"`
	L1Hash      string `json:"l1_hash"`
	L2Hash      string `json:"l2_hash"`
	L3Hash      string `json:"l3_hash"`
}

// FileHashView 是清单文件项（契约 components.schemas.FileHash）；详情视图里的
// path 是逻辑工作区路径 `.basalt/skills/<name>/<rel>`（约束 23 / 行 601 裁决）。
// FileHashView is one manifest file; detail views carry the logical workspace path.
type FileHashView struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// 清单视图的固定字面量（约束 24 / 出行 601 裁决 + 约束 32 的哈希定义）。
// Manifest view literals: the legacy format version, the skill definition file, and
// the logical workspace prefix.
const (
	// legacyManifestFormatVersion 是**没有清单行**时的合成版本号；v2 是当前持久版本，
	// 两者的差别决定 provider / notify 出 `null` 还是出具体值。
	// The synthetic version for missing rows; v2 decides whether v2-only fields appear.
	legacyManifestFormatVersion = 1
	v2ManifestFormatVersion     = 2
	skillDefinitionFile         = "SKILL.md"
	skillLogicalPrefix          = ".basalt/skills/"
)

// ScheduleView 是 /schedules 的元素（契约 components.schemas.ScheduleView）；id 取
// TaskID，last_run_at 零值渲染空串。
// ScheduleView is one /schedules element; id is the TaskID and a zero last run
// renders as an empty string.
type ScheduleView struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Cron    string `json:"cron"`
	Zone    string `json:"zone"`
	Message string `json:"message"`
	// DeliverTo 是系统代投目标通知渠道的**资源 UID**（R7）；空串 = 不代投。
	// DeliverTo is the resource uid of the system-delivery channel; empty means none.
	DeliverTo  string `json:"deliver_to"`
	Enabled    bool   `json:"enabled"`
	LastRunAt  string `json:"last_run_at"`
	LastStatus string `json:"last_status"`
}

// ExecutionView 是 /schedules/{id}/executions 的元素（契约 components.schemas
// .ExecutionView）；started 行的 run_id / finished_at 为空串，error_message 只出
// 公开投影（约束 34）。
// ExecutionView is one execution-history element; nullable columns render empty and
// error_message is always the public projection.
type ExecutionView struct {
	TaskID       string `json:"task_id"`
	RunID        string `json:"run_id"`
	Status       string `json:"status"`
	Trigger      string `json:"trigger"`
	DurationMS   int64  `json:"duration_ms"`
	ErrorMessage string `json:"error_message"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
	// 系统代投结果（R7）与执行结果**分开**展示：`delivery_status` 为空串表示本行
	// 没有代投记录（任务未绑定投递目标）；有值时取 pending / sent / failed / unknown /
	// skipped。`delivery_channel` 是渠道资源 UID，`delivery_detail` 是脱敏短语。
	// The delivery verdict is projected separately from the execution status; an empty
	// delivery_status means this execution has no delivery record.
	DeliveryStatus  string `json:"delivery_status"`
	DeliveryChannel string `json:"delivery_channel"`
	DeliveryDetail  string `json:"delivery_detail"`
}

// sessionSummaryView 投影一行会话记录。
// sessionSummaryView projects one session row.
func sessionSummaryView(rec core.SessionRecord) SessionSummaryView {
	return SessionSummaryView{
		SessionID:    rec.ID,
		ProfileName:  rec.ProfileName,
		Channel:      rec.Channel,
		UserID:       rec.UserID,
		Status:       rec.Status,
		MessageCount: rec.MessageCount,
		CreatedAt:    formatTime(rec.CreatedAt),
		LastActiveAt: formatTime(rec.LastActiveAt),
	}
}

// messageView 投影一条消息；无工具调用时输出 `[]`（约束 34）。
// messageView projects one message; no tool calls still marshals to `[]`.
func messageView(m core.Message) MessageView {
	calls := make([]ToolCallView, 0, len(m.ToolCalls))
	for _, c := range m.ToolCalls {
		calls = append(calls, ToolCallView{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
	}
	return MessageView{
		Role:       m.Role,
		Content:    m.Content,
		ToolCalls:  calls,
		ToolCallID: m.ToolCallID,
		CreatedAt:  formatTime(m.CreatedAt),
	}
}

// sessionDetailView 组合摘要与消息窗口；空窗口输出 `[]`。
// sessionDetailView combines the summary with the message window; emptiness is `[]`.
func sessionDetailView(rec core.SessionRecord, msgs []core.Message) SessionDetailView {
	out := make([]MessageView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageView(m))
	}
	return SessionDetailView{SessionSummaryView: sessionSummaryView(rec), Messages: out}
}

// sandboxView 投影部署者配置的三类白名单与路径展示名：路径只出相对展示形态——宿主
// 绝对路径是装配层布局，不属于对外契约（任何只读面不得泄露绝对路径）。四个数组空时
// 输出 `[]`。出网域名走 `sandboxView` 方法的生效值覆盖，本函数只提供兜底。
// sandboxView keeps only relative display paths: host layout never leaves the process.
func sandboxView(cfg SandboxConfig) SandboxView {
	views := make([]string, 0, len(cfg.FileAllowedPaths))
	for _, p := range cfg.FileAllowedPaths {
		if p == "" || rootedPath(p) {
			// 防御：装配层已经给的是相对展示名；绝对路径（含 Windows 盘符与 `/`
			// 开头）一律不投影，宁缺毋泄。
			continue
		}
		views = append(views, p)
	}
	return SandboxView{
		FileAllowedPaths:     views,
		ShellAllowedCommands: nonNilStrings(cfg.ShellAllowedCommands),
		ShellInterpreters:    nonNilStrings(cfg.ShellInterpreters),
		HTTPAllowedDomains:   nonNilStrings(cfg.HTTPAllowedDomains),
		NetworkAllowlist:     NetworkAllowlistView{Source: core.NetworkAllowlistSourceConfigSeed},
	}
}

// sandboxView 叠加管理面的**生效**出网白名单：`http_allowed_domains` 取策略实例
// 当前快照（保存后立即变化，不读启动时的配置副本）；`network_allowlist` 同时给出
// ETag / 修订号 / 来源 / 最近写入。未装配管理端口（只读装配 / 测试接缝）时退回
// 配置投影：`revision=0`、空 ETag 如实表示"没有可条件写的权威行"。
// sandboxView overlays the live allowlist; the read-only fallback reports revision 0
// and an empty ETag, which truthfully means "no authoritative row to write against".
func (s *Server) sandboxView() SandboxView {
	view := sandboxView(s.opts.Sandbox)
	if s.opts.NetworkAllowlist == nil {
		return view
	}
	policy := s.opts.NetworkAllowlist.Current()
	view.HTTPAllowedDomains = nonNilStrings(policy.Domains)
	view.NetworkAllowlist = networkAllowlistView(policy)
	return view
}

// networkAllowlistView 把策略行投影为对外元数据；时间固定 RFC3339 UTC。
// networkAllowlistView renders the policy row into the fixed metadata projection.
func networkAllowlistView(policy core.NetworkDomainPolicy) NetworkAllowlistView {
	view := NetworkAllowlistView{
		Revision:  policy.Revision,
		Source:    policy.Source(),
		UpdatedBy: policy.UpdatedBy,
	}
	if policy.Revision > 0 {
		// 未装载（revision 0）时没有可写的权威修订，ETag 留空：页面拿不到一个"看起来
		// 合法"的条件值，PUT 会在形状级被 412 拒绝，而不是预写之后才发现 CAS 落败。
		view.ETag = core.NetworkDomainETag(policy)
	}
	if !policy.UpdatedAt.IsZero() {
		view.UpdatedAt = policy.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return view
}

// nonNilStrings 把 nil 切片归一成空切片（约束 34：空集合输出 `[]`）。
// nonNilStrings normalizes a nil slice to an empty one.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// rootedPath 判定路径是否是宿主的绝对 / 根植形态（`/…`、`\…`、`C:…`）。只看形态、
// 不碰文件系统：判据在两套平台口径一致，测试不会因操作系统而不同。
// rootedPath recognizes host-absolute shapes without touching the filesystem, so the
// judgement is identical on every platform.
func rootedPath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return true
	}
	if len(p) >= 2 && p[1] == ':' {
		c := p[0]
		return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	return false
}

// notifyChannelView 投影一条通知渠道：地址只经 `notifyURLHost` 出站（解析不出回空）。
// notifyChannelView projects one channel; the address leaves only as scheme+host.
func notifyChannelView(ch core.NotifyChannel, referencedBy []string) NotifyChannelView {
	return NotifyChannelView{
		Name:              ch.Name,
		ResourceUID:       string(ch.ResourceUID),
		Type:              ch.Type,
		Description:       ch.Description,
		URLHost:           notifyURLHost(ch.URL),
		URLConfigured:     strings.TrimSpace(ch.URL) != "",
		ReferencedBy:      nonNilStrings(referencedBy),
		AddressStatus:     notifyAddressStatus(ch),
		CredentialMode:    string(channelCredentialMode(ch)),
		CredentialRef:     ch.CredentialID,
		CredentialState:   string(ch.CredentialState),
		CredentialVersion: ch.CredentialVersion,
		Last4:             ch.Last4,
		Scope:             string(channelScope(ch)),
		OwnerAgent:        ch.OwnerAgentName,
		Revision:          ch.Revision,
		ETag:              ch.ETag,
		Availability:      ch.Availability,
		LastTest:          ConnectionTestViewOf(ch.LastTest),
	}
}

// channelCredentialMode 归一化渠道地址模式：空列读成 `env`——`contract`
// 的枚举没有第五个值，而 v3 迁移把既有行一律回填为 `env`（`schema.sql` 注释），
// 所以"空 = env"是同一事实的读法，不是替存储猜。托管渠道由仓储读出 `managed`。
// channelCredentialMode reads an empty column as env, matching the migration backfill.
func channelCredentialMode(ch core.NotifyChannel) core.CredentialMode {
	if ch.CredentialMode == core.CredentialModeManaged {
		return core.CredentialModeManaged
	}
	return core.CredentialModeEnv
}

// channelScope 归一化使用范围：空列读成 `shared`（列默认值与迁移回填值，
// `schema.sql` 的 `resource_pointers` / `notify_channels` 同一口径）。
// channelScope reads an empty column as shared, the documented column default.
func channelScope(ch core.NotifyChannel) core.ResourceScope {
	if ch.Scope == core.ScopeExclusive {
		return core.ScopeExclusive
	}
	return core.ScopeShared
}

// 渠道地址可用性四值（契约 NotifyChannelView.address_status）。`url_host` 为空**不等于**
// 未配置：`${VAR}` 引用与托管密文都有地址，只是解析不出可展示的主机。
// The four address-status literals; an empty url_host never means "not configured".
const (
	addressStatusManagedConfigured  = "managed_configured"
	addressStatusEnv                = "env"
	addressStatusEmpty              = "empty"
	addressStatusManagedUnavailable = "managed_unavailable"
)

// notifyAddressStatus 按模式与凭据身份状态投影地址可用性。它**不**预判主密钥可读性——
// 那是解析期的事实（下一次发送 / 准入报 `CREDENTIAL_UNAVAILABLE`），页面不猜。
// managed 模式下没有 active 身份即 `managed_unavailable`：说不出"可用"就不能说。
// notifyAddressStatus derives the four-value status without predicting whether the
// master key itself is readable; that belongs to resolution time.
func notifyAddressStatus(ch core.NotifyChannel) string {
	if ch.CredentialMode != core.CredentialModeManaged {
		if strings.TrimSpace(ch.URL) != "" {
			return addressStatusEnv
		}
		return addressStatusEmpty
	}
	if ch.CredentialState == core.CredentialIdentityActive {
		return addressStatusManagedConfigured
	}
	return addressStatusManagedUnavailable
}

// profileView 投影一条装载结果（约束 26）：顺序由调用方保持；未注册项的 problems
// **恰含** RejectReason 一项；Profile 为 nil 时派生字段为空、数组输出 `[]`。
// profileView projects one loader entry; an unregistered entry carries exactly its
// reject reason in problems, and a nil profile yields empty derived fields.
func profileView(entry core.AgentEntry) ProfileView {
	v := ProfileView{
		Name:           entry.Name,
		DisplayName:    entry.Name,
		Tools:          []string{},
		Skills:         []string{},
		NotifyMode:     string(effectiveNotifyMode(entry.Profile)),
		NotifyChannels: []string{},
		Problems:       []string{},
		Registered:     entry.Registered,
	}
	if p := entry.Profile; p != nil {
		if p.DisplayName != "" {
			v.DisplayName = p.DisplayName
		}
		v.Description = p.Description
		v.Provider = p.Provider.Name
		v.Model = p.Provider.Model
		v.Tools = stringSlice(p.Tools)
		v.Skills = stringSlice(p.Skills)
		v.NotifyChannels = stringSlice(p.NotifyChannels)
		v.Schedules = len(p.Schedules)
	}
	if !entry.Registered && entry.RejectKind != "" {
		// 未注册且已给分类 → 恰含一项（约束 26）；轻路径的待确认项没有分类，
		// 输出 `[]` 而不是一个空串问题。文案按类型化分类投影为中文，英文原文与
		// 可能携带的路径只留在服务端日志。
		// Exactly one problem item, projected from the typed kind; English stays in logs.
		v.Problems = []string{entryRejectMessage(entry)}
	}
	return v
}

// effectiveNotifyMode 取可投影的通知意图：合法声明逐字返回；未声明**或**非法字面量
// 按兼容规则推断，没有定义文件时就是「未声明且无渠道」的推断结果。视图字段是契约
// 枚举，**不得**留空、**不得**透出非法值——声明非法的真实后果是该 Agent 不可运行，
// 由 `problems` 与 `runnable` 表达，不靠这个字段兜底。
// effectiveNotifyMode never emits an empty or invalid literal: an invalid declaration
// reads as undeclared, and problems / runnable carry the actual consequence.
func effectiveNotifyMode(p *core.Profile) core.NotifyMode {
	if p == nil {
		return core.EffectiveNotifyMode("", nil)
	}
	mode := core.EffectiveNotifyMode(p.NotifyMode, p.NotifyChannels)
	if _, err := core.ParseNotifyMode(string(mode)); err != nil {
		return core.EffectiveNotifyMode("", p.NotifyChannels)
	}
	return mode
}

// toolView 投影一个工具描述符；来源枚举由装配层保证（builtin | mcp | code）。
// toolView projects one tool descriptor; Source is assigned by the assembly layer.
func toolView(d ToolDescriptor) ToolView {
	return ToolView{Name: d.Spec.Name, Description: d.Spec.Description, Source: d.Source}
}

// runView 投影一次运行结果。StartedAt / FinishedAt 由 handler 取装配时钟记录；
// reason 走公开投影，原始原因只留在 runs 表与日志里（约束 23）。
// runView projects one run; the reason is projected, the raw value stays in the runs
// table and logs.
func runView(req core.RunRequest, res core.RunResult, started, finished time.Time) RunView {
	trigger := string(req.Trigger)
	if trigger == "" {
		trigger = triggerAPI
	}
	return RunView{
		RunID:        res.RunID,
		SessionID:    req.SessionID,
		Agent:        req.Agent,
		Channel:      req.Channel,
		Trigger:      trigger,
		Status:       string(res.Status),
		Reason:       publicRunReason(res.Status, res.Reason),
		FinalText:    res.FinalText,
		Iterations:   res.Iterations,
		ToolCalls:    res.ToolCalls,
		ConfigHash:   res.ConfigHash,
		ManifestHash: res.ManifestHash,
		StartedAt:    formatTime(started),
		FinishedAt:   formatTime(finished),
	}
}

// runRecordView 投影一行账本记录（列表与取消响应共用）：trigger 落库时已按 Channel
// 映射，缺值时与建运行同口径回落 api；reason 走公开投影（约束 23）。
// runRecordView projects one ledger row for lists and cancel responses; the trigger
// falls back to api exactly like run creation, and the reason is always projected.
func runRecordView(rec core.RunDetail) RunView {
	trigger := string(rec.Trigger)
	if trigger == "" {
		trigger = triggerAPI
	}
	return RunView{
		RunID:        rec.RunID,
		SessionID:    rec.SessionID,
		Agent:        rec.Agent,
		Channel:      rec.Channel,
		Trigger:      trigger,
		Status:       string(rec.Status),
		Reason:       publicRunReason(rec.Status, rec.Reason),
		FinalText:    rec.FinalText,
		Iterations:   rec.Iterations,
		ToolCalls:    rec.ToolCalls,
		ConfigHash:   rec.ConfigHash,
		ManifestHash: rec.ManifestHash,
		StartedAt:    formatTime(rec.StartedAt),
		FinishedAt:   formatTime(rec.FinishedAt),
	}
}

// runDetailView 组合运行行与准入清单视图。
// runDetailView combines the ledger row with the admission manifest view.
func runDetailView(rec core.RunDetail) RunDetailView {
	return RunDetailView{
		RunView:          runRecordView(rec),
		ResourceManifest: runResourceManifestView(rec.ResourceManifest),
	}
}

// runResourceManifestView 把准入清单投影为契约视图（约束 23 / 24）：
//
//   - nil 或 FormatVersion 为零 = 没有清单行 → legacy 形态：format_version 固定 1、
//     legacy_unrecorded=true、数组一律 `[]`、provider / notify 为 null、**省略**
//     agent_lifecycle_state 与 agent_uid（注 17）；
//   - v1 清单（历史 Run）→ provider 为 null，那一个条目仍走 `providers`（兼容字段）；
//     notify 为 null，旧语义是「空白名单 = 不限制」；
//   - v2 清单 → provider 与 notify 各自投影，`providers` 恒为空数组，并补 agent_uid。
//
// 两代的差别不是展示偏好，而是持久结构本身不同：给 v1 行补一个凭据引用或通知授权，
// 就是在替历史 Run 编造它从未固定过的事实。本函数不读发布目录、不查当前版本——
// 历史 Run 的清单只来自它自己的持久行。
// The v1/v2 split mirrors the persisted shapes; projecting v2 fields onto a v1 row
// would invent facts the run never froze, and the projector never reads published
// directories or current versions.
func runResourceManifestView(m *core.RunResourceManifest) *RunResourceManifestView {
	if m == nil || m.FormatVersion == 0 {
		return &RunResourceManifestView{
			FormatVersion:    legacyManifestFormatVersion,
			LegacyUnrecorded: true,
			Providers:        []ResourceVersionRefView{},
			MCPServers:       []ResourceVersionRefView{},
			Skills:           []SkillVersionRefView{},
			Files:            []FileHashView{},
		}
	}
	mcpServers := make([]ResourceVersionRefView, 0, len(m.MCPServers))
	for _, ref := range m.MCPServers {
		mcpServers = append(mcpServers, resourceVersionRefView(core.ResourceMCPServer, ref))
	}
	skills := make([]SkillVersionRefView, 0, len(m.Skills))
	for _, skill := range m.Skills {
		skills = append(skills, skillVersionRefView(skill))
	}
	view := &RunResourceManifestView{
		FormatVersion:       m.FormatVersion,
		AgentLifecycleState: m.AgentLifecycle,
		Providers:           []ResourceVersionRefView{},
		MCPServers:          mcpServers,
		Skills:              skills,
		Files:               flattenSkillFiles(m.Skills),
	}
	if m.FormatVersion < v2ManifestFormatVersion {
		view.Providers = []ResourceVersionRefView{
			resourceVersionRefView(core.ResourceModelConnection, m.Provider.ResourceVersionRef),
		}
		return view
	}
	view.AgentUID = string(m.AgentUID)
	view.Provider = providerManifestView(m.Provider)
	view.Notify = notifyManifestView(m.Notify)
	return view
}

// providerManifestView 投影 v2 的 Provider 固定；凭据只出引用与绑定哈希。
// providerManifestView projects the v2 provider pin without plaintext material.
func providerManifestView(ref core.ProviderVersionRef) *ProviderManifestView {
	return &ProviderManifestView{
		Name:              ref.Name,
		Model:             ref.Model,
		ResourceUID:       string(ref.ResourceUID),
		VersionID:         ref.VersionID,
		VersionNo:         ref.VersionNo,
		CredentialRef:     ref.CredentialID,
		CredentialVersion: ref.CredentialVersion,
		TargetBinding:     ref.TargetBinding,
	}
}

// notifyManifestView 投影 v2 的通知授权；渠道空集合输出 `[]`，不是 null。
// notifyManifestView projects the v2 grant; an empty channel set is `[]`, not null.
func notifyManifestView(grant core.NotifyManifest) *NotifyManifestView {
	channels := make([]NotifyChannelRefView, 0, len(grant.Channels))
	for _, ref := range grant.Channels {
		channels = append(channels, NotifyChannelRefView{
			Name:        ref.Name,
			ResourceUID: string(ref.ResourceUID),
		})
	}
	return &NotifyManifestView{Mode: string(grant.Mode), Channels: channels}
}

// resourceVersionRefView 给版本引用补上契约枚举的 kind。
// resourceVersionRefView adds the contract kind enum to one version ref.
func resourceVersionRefView(kind core.ResourceKind, ref core.ResourceVersionRef) ResourceVersionRefView {
	return ResourceVersionRefView{
		Kind:        string(kind),
		Name:        ref.Name,
		ResourceUID: string(ref.ResourceUID),
		VersionID:   ref.VersionID,
		VersionNo:   ref.VersionNo,
		ContentHash: ref.ContentHash,
	}
}

// skillVersionRefView 投影一个技能版本：skill_md_hash 直接取文件清单里 SKILL.md 的
// 哈希；l3_hash 按约束 32 对"除 SKILL.md 外的文件清单"复算（与发布时同一字节定义）；
// l1_hash / l2_hash 从未持久化，留空串（I-48）。
// skillVersionRefView recomputes l3 exactly as publishing did; l1/l2 are not
// recoverable from the persisted manifest and stay empty.
func skillVersionRefView(ref core.SkillVersionRef) SkillVersionRefView {
	files := sortedSkillFiles(ref.Files)
	return SkillVersionRefView{
		Name:        ref.Name,
		VersionID:   ref.VersionID,
		VersionNo:   ref.VersionNo,
		ContentHash: ref.ContentHash,
		SkillMDHash: skillMDHash(files),
		L1Hash:      "",
		L2Hash:      "",
		L3Hash:      skillL3Hash(files),
	}
}

// sortedSkillFiles 返回按 path 升序的副本：持久清单已由构造函数规范化，这里再排一次
// 是为了让复算不依赖持久行的字节顺序（约束 32 的哈希定义只认路径序）。
// sortedSkillFiles returns an ascending copy so recomputation never depends on the
// stored byte order.
func sortedSkillFiles(files []core.FileHash) []core.FileHash {
	out := make([]core.FileHash, len(files))
	copy(out, files)
	slices.SortFunc(out, func(a, b core.FileHash) int {
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

// skillMDHash 取技能定义文件的哈希；清单里没有 SKILL.md 时为空串（不补造）。
// skillMDHash returns the SKILL.md hash, or empty when the list has no definition file.
func skillMDHash(files []core.FileHash) string {
	for _, file := range files {
		if file.Path == skillDefinitionFile {
			return file.SHA256
		}
	}
	return ""
}

// skillL3Hash 按约束 32 复算 L3：除 SKILL.md 外的文件清单（path 升序）取
// json.Marshal 的紧凑字节再做 sha256；空清单序列化为 `[]`（与发布路径的
// hashFileList 同一字节定义）。
// skillL3Hash recomputes L3 over the non-SKILL.md list with the same canonical bytes
// the publishing path uses.
func skillL3Hash(files []core.FileHash) string {
	rest := make([]core.FileHash, 0, len(files))
	for _, file := range files {
		if file.Path != skillDefinitionFile {
			rest = append(rest, file)
		}
	}
	raw, err := json.Marshal(rest)
	if err != nil {
		// FileHash 只含字符串与整数，序列化不会失败；真失败时返回空串而非编造哈希。
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// flattenSkillFiles 把各技能的文件清单扁平化为顶层 files[]（约束 23 / 行 601）：
// 路径为逻辑工作区路径 `.basalt/skills/<name>/<rel>`，按 path 升序、按 path 去重。
// flattenSkillFiles flattens per-skill file lists into logical workspace paths,
// ascending and deduplicated.
func flattenSkillFiles(skills []core.SkillVersionRef) []FileHashView {
	out := make([]FileHashView, 0, len(skills))
	for _, skill := range skills {
		for _, file := range skill.Files {
			out = append(out, FileHashView{
				Path:   skillLogicalPrefix + skill.Name + "/" + file.Path,
				SHA256: file.SHA256,
				Size:   file.Size,
			})
		}
	}
	slices.SortFunc(out, func(a, b FileHashView) int {
		return strings.Compare(a.Path, b.Path)
	})
	return slices.CompactFunc(out, func(a, b FileHashView) bool {
		return a.Path == b.Path
	})
}

// SkillView 是 /skills 的元素与 /skills/{name} 详情的基底（契约
// components.schemas.SkillView）：path 是工作副本逻辑路径，etag 由发布版本
// content_hash 生成、格式与内容 ETag 相同（模块卡 §3.1.8）。description 在本 US
// 没有可达来源——只读端口（ResourceReader）只拿得到发布指针与版本行，读不到
// SKILL.md 正文；空串是"未记录"，**不得**用技能名或猜测补造。
// SkillView is the contract shape for one read-only skill; the ETag is derived from
// the published content hash. The description stays empty because the frozen reader
// port has no reachable source for it.
type SkillView struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Path         string   `json:"path"`
	VersionID    int64    `json:"version_id"`
	VersionNo    int      `json:"version_no"`
	ContentHash  string   `json:"content_hash"`
	ETag         string   `json:"etag"`
	FileCount    int      `json:"file_count"`
	ReferencedBy []string `json:"referenced_by"`
}

// ModelConnectionView 是 /model-connections 的元素（契约
// components.schemas.ModelConnectionView）：api_key_env 只回 `${ENV_VAR}` 引用名，
// 未配置或非占位形态为空串——解析后的凭证值**永不**进入本视图（约束 35）。
// ModelConnectionView is the contract shape for one model connection; api_key_env
// carries the reference name only, never the resolved secret.
type ModelConnectionView struct {
	Name string `json:"name"`
	// ResourceUID 是资源的稳定身份；同名重建生成新 UID（旧绑定不自动接管）。
	ResourceUID  string `json:"resource_uid"`
	BaseURL      string `json:"base_url"`
	APIKeyEnv    string `json:"api_key_env"`
	DefaultModel string `json:"default_model"`
	// 凭据面只出模式、只读引用、状态、版本号与尾号——**没有**任何明文载体；
	// `credential_mode` 由「有没有凭据身份」判定，不另存一份模式列。
	CredentialMode    string `json:"credential_mode"`
	CredentialRef     string `json:"credential_ref"`
	CredentialState   string `json:"credential_state"`
	CredentialVersion int    `json:"credential_version"`
	Last4             string `json:"last4"`
	// 范围与条件写：scope / owner_agent / revision / etag 由 app 层的同一份判定给出。
	Scope              string   `json:"scope"`
	OwnerAgent         string   `json:"owner_agent"`
	Revision           int      `json:"revision"`
	PublishedVersionID int64    `json:"published_version_id"`
	PublishedVersionNo int      `json:"published_version_no"`
	AppliedVersionID   int64    `json:"applied_version_id"`
	AppliedVersionNo   int      `json:"applied_version_no"`
	ETag               string   `json:"etag"`
	ReferencedBy       []string `json:"referenced_by"`
	// Availability 是本地配置就绪三值；LastTest 是最后一次实测结果（null = 从未测过）。
	// 两者分开展示：本地看起来可用**不等于**实测通过。
	// Availability is local readiness; LastTest is the live result, shown separately.
	Availability string              `json:"availability"`
	LastTest     *ConnectionTestView `json:"last_test"`
}

// McpServerView 是 /mcp-servers 的元素（契约 components.schemas.McpServerView）：
// 只回可执行名与变量名，不回参数或值（约束 35）；pending_restart 由发布 / 已应用
// 指针实时计算，不写死（约束 29 / D-09）。
// McpServerView is the contract shape for one read-only MCP server; pending_restart is
// always derived from the published/applied pair, never stored.
type McpServerView struct {
	Name               string   `json:"name"`
	Transport          string   `json:"transport"`
	Command            string   `json:"command"`
	EnvKeys            []string `json:"env_keys"`
	Connected          bool     `json:"connected"`
	Tools              []string `json:"tools"`
	PublishedVersionID int64    `json:"published_version_id"`
	PublishedVersionNo int      `json:"published_version_no"`
	AppliedVersionID   int64    `json:"applied_version_id"`
	AppliedVersionNo   int      `json:"applied_version_no"`
	PendingRestart     bool     `json:"pending_restart"`
}

// 需重启四段的出口（约束 29）：MCP 定义由 mcpPendingRestart 投影；沙箱白名单与
// basalt.yaml 的 Provider 段 / http 段没有契约字段，按技术路线⑤只在管理台页面与
// docs/Operations.md 静态标注——**不得**给只读视图加服务端"伪生效"标记。
// Restart hints: only MCP has a contract field; the other three areas are page/doc
// copy and must not gain server-side view fields.

// mcpPendingRestart 按发布 / 已应用指针实时计算"需重启"（约束 29 / D-09）：
// 已发布但运行中的进程仍是旧版本时为真；两个指针相等（含基线同版本）时为假。
// 版本 id 从 1 起，PublishedVersionID 为 0 表示缺失发布指针——此时不报"需重启"，
// 页面另行标问题，不得伪造版本号。connected 独立表达连接状态，不参与本判断。
// mcpPendingRestart derives the restart hint; a missing published pointer (0) is never
// reported as pending, and connection state does not affect the result.
func mcpPendingRestart(ptr core.ResourcePointer) bool {
	if ptr.PublishedVersionID == 0 {
		return false
	}
	return ptr.AppliedVersionID == nil || *ptr.AppliedVersionID != ptr.PublishedVersionID
}

// OperationView 是管理操作列表的元素（契约 components.schemas.OperationView）；
// 操作记录是审计事实，本视图只投影、不改写——时间零值渲染空串（约束 34）。
// OperationView is one management-operation element; times render empty when unset.
type OperationView struct {
	OperationID  string `json:"operation_id"`
	Action       string `json:"action"`
	TargetKind   string `json:"target_kind"`
	TargetID     string `json:"target_id"`
	ActorSubject string `json:"actor_subject"`
	AuthMethod   string `json:"auth_method"`
	Source       string `json:"source"`
	Status       string `json:"status"`
	ResultCode   string `json:"result_code"`
	ErrorMessage string `json:"error_message"`
	CreatedAt    string `json:"created_at"`
	FinishedAt   string `json:"finished_at"`
}

// OperationDetailView 是管理操作详情（契约 components.schemas.OperationDetail 的
// allOf：OperationView + 请求摘要与恢复字段）；detail 是脱敏后的键值对象。
// OperationDetailView adds the request/recovery fields and the redacted detail object.
type OperationDetailView struct {
	OperationView
	RequestHash        string         `json:"request_hash"`
	RequestedVersionID int64          `json:"requested_version_id"`
	ExpectedETag       string         `json:"expected_etag"`
	Detail             map[string]any `json:"detail"`
	RecoveredAt        string         `json:"recovered_at"`
	Verification       string         `json:"verification"`
}

// operationVerificationNotRequired 是契约枚举里"无需恢复查证"的取值：verification
// 列只在启动恢复时写，未恢复的行为 NULL——投影为 not_required 而不是空串。
// Un-recovered rows carry NULL verification, which the contract renders as not_required.
const operationVerificationNotRequired = "not_required"

// operationView 投影一条管理操作记录。
// operationView projects one management-operation record.
func operationView(rec core.OperationRecord) OperationView {
	return OperationView{
		OperationID:  rec.OperationID,
		Action:       rec.Action,
		TargetKind:   rec.TargetKind,
		TargetID:     rec.TargetID,
		ActorSubject: rec.ActorSubject,
		AuthMethod:   rec.AuthMethod,
		Source:       rec.Source,
		Status:       string(rec.Status),
		ResultCode:   rec.ResultCode,
		ErrorMessage: rec.ErrorMessage,
		CreatedAt:    formatTime(rec.CreatedAt),
		FinishedAt:   formatTime(rec.FinishedAt),
	}
}

// operationDetailView 组合操作视图与脱敏详情；detail_json 是唯一需要投影的原始
// 字节，绝不整段出站（模块卡 §3.1.6）。
// operationDetailView combines the operation view with the redacted detail object;
// raw detail_json bytes never leave this layer.
func operationDetailView(rec core.OperationRecord) OperationDetailView {
	verification := rec.Verification
	if verification == "" {
		verification = operationVerificationNotRequired
	}
	return OperationDetailView{
		OperationView:      operationView(rec),
		RequestHash:        rec.RequestHash,
		RequestedVersionID: rec.RequestedVersionID,
		ExpectedETag:       rec.ExpectedETag,
		Detail:             redactOperationDetail(rec.DetailJSON),
		RecoveredAt:        formatTime(rec.RecoveredAt),
		Verification:       verification,
	}
}

// 操作详情白名单（模块卡 §3.1.6）：只保留计数、文件相对路径、版本号与 US-11
// 实际写入的非敏感查证依据（两个文件哈希、生命周期前置条件）。凭证、绝对路径与
// 请求原文一律不投影；未列出的键静默丢弃——白名单是闭集，新增键必须先改这里。
// The detail whitelist is a closed set: counts, relative paths, version numbers, and
// the non-sensitive recovery inputs US-11 actually writes. Everything else is dropped.
func projectOperationDetailValue(key string, value any) (any, bool) {
	switch key {
	// `before_revision` / `after_revision` 是网络白名单的恢复证据（run-and-errors §8）：
	// 修订号前后值决定三态查证，与 `candidate_sha256` 一起构成详情页可核对的依据。
	// Allowlist recovery evidence: the before/after revisions drive the verdict.
	case "providers", "mcp", "skills", "agents", "expected_generation", "version_no",
		"before_revision", "after_revision":
		return detailInteger(value)
	case "candidate_sha256", "previous_sha256":
		text, ok := value.(string)
		if !ok || !isLowerHexSHA256(text) {
			return nil, false
		}
		return text, true
	case "expected_state":
		state, ok := value.(string)
		if !ok {
			return nil, false
		}
		switch core.AgentState(state) {
		case core.AgentActive, core.AgentDisabled, core.AgentArchived:
			return state, true
		default:
			return nil, false
		}
	case "path":
		path, ok := value.(string)
		if !ok || !isRelativeOperationPath(path) {
			return nil, false
		}
		return path, true
	case "files":
		list, ok := value.([]any)
		if !ok {
			return nil, false
		}
		paths := make([]string, 0, len(list))
		for _, item := range list {
			path, ok := item.(string)
			if !ok || !isRelativeOperationPath(path) {
				return nil, false
			}
			paths = append(paths, path)
		}
		return paths, true
	default:
		return nil, false
	}
}

// redactOperationDetail 把 detail_json 投影为白名单键值对象：解析失败、非对象或
// 空输入一律返回 `{}`（宁可少给证据，也不整段出站）。
// redactOperationDetail projects detail_json through the whitelist; unparseable input
// yields an empty object rather than raw bytes.
func redactOperationDetail(raw []byte) map[string]any {
	out := make(map[string]any)
	if len(raw) == 0 {
		return out
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var detail map[string]any
	if err := decoder.Decode(&detail); err != nil || detail == nil {
		return out
	}
	for key, value := range detail {
		if projected, ok := projectOperationDetailValue(key, value); ok {
			out[key] = projected
		}
	}
	return out
}

// detailInteger 只接受非负整数：json.Number 保留原文进制，浮点（2.0）与负数一律
// 判为不可信输入并丢弃。
// detailInteger accepts non-negative integers only; decimals and negatives are dropped.
func detailInteger(value any) (any, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return nil, false
	}
	parsed, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil || parsed < 0 {
		return nil, false
	}
	return parsed, true
}

// isLowerHexSHA256 判定 64 位小写十六进制摘要：与恢复查证对 detail_json 的取值口径
// 同源（internal/app 的同名规则），不猜哈希。
// isLowerHexSHA256 accepts exactly 64 lowercase hex characters.
func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// isRelativeOperationPath 只放行工作区相对路径：拒绝 Unix 绝对路径、Windows 盘符、
// 反斜杠与 `..` 段——绝对路径会暴露宿主目录结构（约束 10 / §3.1.6）。
// isRelativeOperationPath admits workspace-relative logical paths only.
func isRelativeOperationPath(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") {
		return false
	}
	if len(value) >= 2 && value[1] == ':' {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

// scheduleView 投影一条任务定义；时间列经 formatTime（零值渲染空串）。
// scheduleView projects one task definition; times go through formatTime.
func scheduleView(rec core.TaskRecord) ScheduleView {
	return ScheduleView{
		ID:         rec.TaskID,
		Agent:      rec.Agent,
		Cron:       rec.Cron,
		Zone:       rec.Zone,
		Message:    rec.Message,
		DeliverTo:  rec.DeliverTo,
		Enabled:    rec.Enabled,
		LastRunAt:  formatTime(rec.LastRunAt),
		LastStatus: string(rec.LastStatus),
	}
}

// executionView 投影一行执行历史：可空列（run_id / finished_at）渲染空串，
// error_message 只经 publicExecutionMessage 出站（约束 34 / R22）。
// executionView projects one execution row; the raw error never leaves this layer.
func executionView(rec core.ExecutionRecord) ExecutionView {
	return ExecutionView{
		TaskID:       rec.TaskID,
		RunID:        rec.RunID,
		Status:       string(rec.Status),
		Trigger:      string(rec.Trigger),
		DurationMS:   rec.DurationMS,
		ErrorMessage: publicExecutionMessage(rec.Status, rec.ErrorMessage),
		StartedAt:    formatTime(rec.StartedAt),
		FinishedAt:   formatTime(rec.FinishedAt),
		// 投递结果是独立账：待投 / 已投 / 失败 / 结果未知 / 跳过各有自己的状态，
		// 不折进执行状态（"任务完成 / 飞书投递失败"必须能同时看到）。
		// Delivery is its own record and never folds into the execution status.
		DeliveryStatus:  string(rec.Delivery),
		DeliveryChannel: rec.DeliveryChannel,
		DeliveryDetail:  rec.DeliveryDetail,
	}
}

// 执行历史的公开错误文案（约束 34 / D-21）：归档跳过文案由 scheduler 直写
// error_message，其余 skipped 原文（锁失、兄弟任务占会话）一律落回通用文案。
// Public execution messages: the archived-skip literal is written by the scheduler,
// every other skip reason collapses into the generic text.
const (
	executionMessageArchived = "会话已归档，本轮跳过"
	executionMessageSkipped  = "上一轮尚未结束，本轮跳过"
)

// publicExecutionMessage 按 status + 逐字匹配的 raw 做三路投影（约束 34）：
// started / completed → 空串；failed → 运行失败；skipped 且 raw 恰为归档冻结文案
// → 同文案；其余 skipped → 通用跳过文案。**不得**用子串 / 前缀匹配——未识别原文
// （可能含绝对路径、SQL 正文与用户数据）只落库、永不出站。
// publicExecutionMessage projects by status and an exact raw match; unknown text
// always falls back to the generic skip message.
func publicExecutionMessage(status core.ExecutionStatus, raw string) string {
	switch status {
	case core.ExecutionStarted, core.ExecutionCompleted:
		return ""
	case core.ExecutionFailed:
		return publicReasonFailed
	case core.ExecutionSkipped:
		if raw == executionMessageArchived {
			return executionMessageArchived
		}
		return executionMessageSkipped
	default:
		return ""
	}
}

// infoView 汇总进程读面（约束 25）：agents 只数已注册项；MCP 连接数来自装配注入；
// uptime 起点零值时按 0 秒处理，时钟回拨也不出负数。
// infoView assembles the process read face; only registered agents are counted and
// a zero/negative uptime never leaks out.
func infoView(opts Options, now time.Time) InfoView {
	uptime := int64(0)
	if !opts.StartedAt.IsZero() {
		if d := now.Sub(opts.StartedAt); d > 0 {
			uptime = int64(d / time.Second)
		}
	}
	agents := 0
	if opts.Agents != nil {
		for _, entry := range opts.Agents.Profiles() {
			if entry.Registered {
				agents++
			}
		}
	}
	tools := 0
	if opts.Tools != nil {
		tools = len(opts.Tools.Tools())
	}
	providers := make([]ProviderView, 0, len(opts.Providers))
	for _, p := range opts.Providers {
		providers = append(providers, ProviderView{
			Name:       p.Name,
			Models:     stringSlice(p.Models),
			Configured: p.Configured,
		})
	}
	return InfoView{
		Version:       opts.Version,
		UptimeSeconds: uptime,
		Agents:        agents,
		Tools:         tools,
		MCPServers:    opts.MCPConnected,
		Providers:     providers,
	}
}

// AgentLifecycleView 是启停成功后的生命周期状态视图（契约 AgentLifecycleView）：
// 一次成功迁移意味着状态文件刚通过校验，因此 valid 恒为 true、state 非空；
// `updated_at` 取状态文件的 Unix 毫秒并按视图统一口径投影为 RFC3339 UTC。
// `agent_uid` / `uid_source` 是 R4 的稳定身份：v2 状态文件给 `state_file`，合法但
// 未记录身份的 v1 文件给 `legacy_unrecorded`；状态文件缺失 / 损坏时两者都为空串。
// The lifecycle view returned by a successful transition; uid_source is state_file
// for v2 records and legacy_unrecorded for still-valid v1 records.
type AgentLifecycleView struct {
	Agent          string  `json:"agent"`
	AgentUID       string  `json:"agent_uid"`
	UIDSource      string  `json:"uid_source"`
	State          *string `json:"state"`
	Generation     int64   `json:"generation"`
	UpdatedAt      string  `json:"updated_at"`
	OperationID    string  `json:"operation_id"`
	Valid          bool    `json:"valid"`
	LifecycleError string  `json:"lifecycle_error"`
}

const (
	// lifecycleUIDSourceStateFile：身份由状态文件记录（v2 起必填）。
	lifecycleUIDSourceStateFile = "state_file"
	// lifecycleUIDSourceLegacy：状态文件合法但未记录身份（v1 形态，迁移后补写）。
	lifecycleUIDSourceLegacy = "legacy_unrecorded"
)

// agentLifecycleView 投影一条生命周期记录：失败路径没有 data（信封省略），因此本
// 函数只服务成功迁移——缺失 / 损坏的状态用 409 表达，不在这里伪装成 valid=false。
// Only successful transitions reach this projection; missing or corrupt state files
// are expressed as 409 responses, never as a valid=false body.
func agentLifecycleView(rec core.LifecycleRecord) AgentLifecycleView {
	state := string(rec.State)
	source := lifecycleUIDSourceLegacy
	if rec.AgentUID != "" {
		source = lifecycleUIDSourceStateFile
	}
	return AgentLifecycleView{
		Agent:          rec.Agent,
		AgentUID:       string(rec.AgentUID),
		UIDSource:      source,
		State:          &state,
		Generation:     rec.Generation,
		UpdatedAt:      formatTime(time.UnixMilli(rec.UpdatedAt)),
		OperationID:    rec.OperationID,
		Valid:          true,
		LifecycleError: "",
	}
}

// AgentContentView 是 `GET /agents/{name}/content` 的响应体（契约 AgentContent）：
// 原文按字节保留，ETag 与响应头的值逐字一致。
// AgentContentView is the content read response; the ETag matches the header.
type AgentContentView struct {
	Content string `json:"content"`
	ETag    string `json:"etag"`
}

// AgentFileView 是受管文件清单的一项（契约 AgentFilesView.files）：相对 Agent
// 目录的路径（`/` 分隔）与字节数。
// AgentFileView is one managed file entry relative to the agent directory.
type AgentFileView struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// AgentFilesView 是内容写成功的响应体（契约 AgentFilesView）：Agent 名、生命周期
// 摘要、受管文件集合与新 ETag（注 19）。`files` 空时输出 `[]`（约束 34）。
// AgentFilesView is the successful content-write response: agent, lifecycle
// summary, managed files, and the new ETag.
type AgentFilesView struct {
	Agent     string             `json:"agent"`
	Lifecycle AgentLifecycleView `json:"lifecycle"`
	ETag      string             `json:"etag"`
	Files     []AgentFileView    `json:"files"`
	// Changes 是系统在本次保存中自动应用的受控工具增删（R7 / 宪法约束 11）：
	// 只可能是 `notify`；两个数组恒非 nil，页面据此提示"系统已自动加入/移除"。
	// Changes reports the system-applied tool deltas of this save; notify is the only
	// possible name and both arrays are always present.
	Changes AgentToolChangeView `json:"changes"`
}

// AgentToolChangeView 是系统自动工具变更的响应投影（数组恒非 nil，约束 34）。
// AgentToolChangeView is the response shape of the system-applied tool delta.
type AgentToolChangeView struct {
	Added   []string `json:"tools_added"`
	Removed []string `json:"tools_removed"`
}

// agentFilesView 投影一次成功的内容写：ETag 由调用方从随后的读取给出（核心类型
// AgentFiles 不带 ETag 字段——模块卡 §3.1.9 固定了它的形状）。lifecycle 只填
// 核心记录携带的三项（state / valid / lifecycle_error）；generation、
// updated_at 与 operation_id 不在内容写结果里，按零值投影而不是编造，页面以
// 启停响应或生命周期读取为准。
// agentFilesView projects a successful write; the ETag comes from a follow-up
// read because the fixed core shape carries none, and fields the write result
// does not know stay zero rather than being invented.
func agentFilesView(files core.AgentFiles, etag string) AgentFilesView {
	items := make([]AgentFileView, len(files.Files))
	for i, file := range files.Files {
		items[i] = AgentFileView{Path: file.Path, Size: file.Size}
	}
	return AgentFilesView{
		Agent: files.Agent,
		Lifecycle: AgentLifecycleView{
			Agent:          files.Agent,
			State:          files.Lifecycle.State,
			Valid:          files.Lifecycle.Valid,
			LifecycleError: files.Lifecycle.LifecycleError,
		},
		ETag:  etag,
		Files: items,
		Changes: AgentToolChangeView{
			Added:   stringSlice(files.Changes.Added),
			Removed: stringSlice(files.Changes.Removed),
		},
	}
}

// formatTime 是视图时间字段的唯一格式器：RFC3339 UTC；零值渲染空串（约束 34）。
// formatTime is the single time formatter for views; zero renders as empty.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// stringSlice 保证数组字段非 nil——`null` 会让 SDK 反序列化失败（约束 34）。
// stringSlice guarantees a non-nil array; `null` breaks SDK deserialization.
func stringSlice(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

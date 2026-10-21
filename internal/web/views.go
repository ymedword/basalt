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
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Provider    string             `json:"provider"`
	Model       string             `json:"model"`
	Tools       []string           `json:"tools"`
	Skills      []string           `json:"skills"`
	Schedules   int                `json:"schedules"`
	Lifecycle   AgentLifecycleView `json:"lifecycle"`
	Registered  bool               `json:"registered"`
	Runnable    bool               `json:"runnable"`
	Problems    []string           `json:"problems"`
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
// 原样投影，空配置输出 `[]`（约束 34）。修改白名单需改配置并重启，本面只读。
// SandboxView projects the four allowlists; an empty configuration is `[]`.
type SandboxView struct {
	FileAllowedPaths     []string `json:"file_allowed_paths"`
	ShellAllowedCommands []string `json:"shell_allowed_commands"`
	ShellInterpreters    []string `json:"shell_interpreters"`
	HTTPAllowedDomains   []string `json:"http_allowed_domains"`
}

// NotifyChannelView 是 /notify-channels 的元素（契约 components.schemas.NotifyChannelView）：
// 地址只以 `url_host`（scheme + host）出现，原始 URL 与查询串**没有任何载体**。
// NotifyChannelView carries only the scheme+host projection; the raw URL has no carrier.
type NotifyChannelView struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	URLHost     string `json:"url_host"`
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
	// AgentLifecycleState 在历史 Run 上**省略**（注 17）：不知道就不填 active。
	AgentLifecycleState string                   `json:"agent_lifecycle_state,omitempty"`
	LegacyUnrecorded    bool                     `json:"legacy_unrecorded"`
	Providers           []ResourceVersionRefView `json:"providers"`
	MCPServers          []ResourceVersionRefView `json:"mcp_servers"`
	Skills              []SkillVersionRefView    `json:"skills"`
	Files               []FileHashView           `json:"files"`
}

// ResourceVersionRefView 是清单里的一个版本引用；kind 枚举 model_connection /
// mcp_server（契约 components.schemas.ResourceVersionRef）。
// ResourceVersionRefView carries the contract kind enum and version identity.
type ResourceVersionRefView struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
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
	legacyManifestFormatVersion = 1
	skillDefinitionFile         = "SKILL.md"
	skillLogicalPrefix          = ".basalt/skills/"
)

// ScheduleView 是 /schedules 的元素（契约 components.schemas.ScheduleView）；id 取
// TaskID，last_run_at 零值渲染空串。
// ScheduleView is one /schedules element; id is the TaskID and a zero last run
// renders as an empty string.
type ScheduleView struct {
	ID         string `json:"id"`
	Agent      string `json:"agent"`
	Cron       string `json:"cron"`
	Zone       string `json:"zone"`
	Message    string `json:"message"`
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

// sandboxView 投影沙箱白名单：路径只出相对展示形态——宿主绝对路径是装配层布局，
// 不属于对外契约（任何只读面不得泄露绝对路径）。四个数组空时输出 `[]`。
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
	}
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
func notifyChannelView(ch core.NotifyChannel) NotifyChannelView {
	return NotifyChannelView{
		Name:        ch.Name,
		Type:        ch.Type,
		Description: ch.Description,
		URLHost:     notifyURLHost(ch.URL),
	}
}

// profileView 投影一条装载结果（约束 26）：顺序由调用方保持；未注册项的 problems
// **恰含** RejectReason 一项；Profile 为 nil 时派生字段为空、数组输出 `[]`。
// profileView projects one loader entry; an unregistered entry carries exactly its
// reject reason in problems, and a nil profile yields empty derived fields.
func profileView(entry core.AgentEntry) ProfileView {
	v := ProfileView{
		Name:       entry.Name,
		Tools:      []string{},
		Skills:     []string{},
		Problems:   []string{},
		Registered: entry.Registered,
	}
	if p := entry.Profile; p != nil {
		v.Description = p.Description
		v.Provider = p.Provider.Name
		v.Model = p.Provider.Model
		v.Tools = stringSlice(p.Tools)
		v.Skills = stringSlice(p.Skills)
		v.Schedules = len(p.Schedules)
	}
	if !entry.Registered && entry.RejectReason != "" {
		// 未注册且已给拒因 → 恰含一项（约束 26）；轻路径的待确认项没有拒因，
		// 输出 `[]` 而不是一个空串问题。
		v.Problems = []string{entry.RejectReason}
	}
	return v
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
//     legacy_unrecorded=true、数组一律 `[]`、**省略** agent_lifecycle_state（注 17）；
//   - 有清单 → 按持久结构逐项投影：Provider 一项（kind=model_connection）、每个 MCP
//     一项（kind=mcp_server）、技能四个哈希、文件扁平化为逻辑路径。
//
// 本函数不读发布目录、不查当前版本——历史 Run 的清单只来自它自己的持久行。
// The projector never reads published directories or current versions; a historical
// run's manifest comes only from its own persisted row.
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
	providers := []ResourceVersionRefView{
		resourceVersionRefView(core.ResourceModelConnection, m.Provider),
	}
	mcpServers := make([]ResourceVersionRefView, 0, len(m.MCPServers))
	for _, ref := range m.MCPServers {
		mcpServers = append(mcpServers, resourceVersionRefView(core.ResourceMCPServer, ref))
	}
	skills := make([]SkillVersionRefView, 0, len(m.Skills))
	for _, skill := range m.Skills {
		skills = append(skills, skillVersionRefView(skill))
	}
	return &RunResourceManifestView{
		FormatVersion:       m.FormatVersion,
		AgentLifecycleState: m.AgentLifecycle,
		Providers:           providers,
		MCPServers:          mcpServers,
		Skills:              skills,
		Files:               flattenSkillFiles(m.Skills),
	}
}

// resourceVersionRefView 给版本引用补上契约枚举的 kind。
// resourceVersionRefView adds the contract kind enum to one version ref.
func resourceVersionRefView(kind core.ResourceKind, ref core.ResourceVersionRef) ResourceVersionRefView {
	return ResourceVersionRefView{
		Kind:        string(kind),
		Name:        ref.Name,
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
	Name               string   `json:"name"`
	BaseURL            string   `json:"base_url"`
	APIKeyEnv          string   `json:"api_key_env"`
	DefaultModel       string   `json:"default_model"`
	PublishedVersionID int64    `json:"published_version_id"`
	PublishedVersionNo int      `json:"published_version_no"`
	AppliedVersionID   int64    `json:"applied_version_id"`
	AppliedVersionNo   int      `json:"applied_version_no"`
	ETag               string   `json:"etag"`
	ReferencedBy       []string `json:"referenced_by"`
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
	case "providers", "mcp", "skills", "agents", "expected_generation", "version_no":
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
// The lifecycle view returned by a successful transition; the file was just validated,
// so valid is always true and state is never null.
type AgentLifecycleView struct {
	Agent          string  `json:"agent"`
	State          *string `json:"state"`
	Generation     int64   `json:"generation"`
	UpdatedAt      string  `json:"updated_at"`
	OperationID    string  `json:"operation_id"`
	Valid          bool    `json:"valid"`
	LifecycleError string  `json:"lifecycle_error"`
}

// agentLifecycleView 投影一条生命周期记录：失败路径没有 data（信封省略），因此本
// 函数只服务成功迁移——缺失 / 损坏的状态用 409 表达，不在这里伪装成 valid=false。
// Only successful transitions reach this projection; missing or corrupt state files
// are expressed as 409 responses, never as a valid=false body.
func agentLifecycleView(rec core.LifecycleRecord) AgentLifecycleView {
	state := string(rec.State)
	return AgentLifecycleView{
		Agent:          rec.Agent,
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

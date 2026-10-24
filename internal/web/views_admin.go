package web

// 本文件是 US-12 写面响应视图与错误 `data` 的**唯一**投影面（颗粒度文档
// §3.1.8）：凡端口已给出 `core` 记录的（版本行 / 版本详情 / 发布结果 / 引用 /
// 测试结果 / 归档记录 / 草稿），投影都在这里完成；需要 app 层把指针、版本与
// 工作副本正文拼在一起的视图（模型连接 / MCP / 技能详情、工作区）只在这里
// 声明形状，由适配器填充。
// 两条固定口径与 views.go 相同：空数组输出 `[]`；时间字段 RFC3339 UTC、零值
// 渲染空串。错误 `data` 只允许 `agents` / `active_runs` / `run_ids` / `problems`
// 四个契约固定数组、`current_etag` 与 `pending` 之后失败的 `operation_id`，其余键
// （含定义正文、凭证）一律丢弃。
// This file is the only projection surface for US-12 write responses. Empty
// collections marshal to `[]`, times are RFC3339 UTC, and failure data is whitelisted
// to the four contract-fixed string arrays, the conditional-write ETag and the
// post-pending operation id.

import (
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// AgentDraftView 是 `POST /agents/{name}/generate-files` 的响应体（契约
// AgentDraftView）：Agent 名、内存草稿文件与澄清问题（R6.3）。文件与问题互斥——
// 有问题时 `files` 为空数组；`files[].content` 可直接编辑后再走多文件提交。草稿没有
// 磁盘基础版本，所以**没有** ETag 与大小字段。
// AgentDraftView is the in-memory draft response; files and questions are exclusive,
// and no ETag or sizes exist because nothing was written to disk.
type AgentDraftView struct {
	Agent     string               `json:"agent"`
	Files     []AgentDraftFileView `json:"files"`
	Questions []AgentQuestionView  `json:"questions"`
}

// AgentDraftFileView 是草稿文件的一项：相对路径与 UTF-8 正文。
// AgentDraftFileView is one draft file: a relative path and UTF-8 content.
type AgentDraftFileView struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// AgentQuestionView 是作者模型给出的一个澄清问题（R6.3）：只含 id 与问题文本，
// 回答由页面内存保存并随下一次请求提交，服务端不持久化。
// AgentQuestionView is one clarifying question; answers stay in page memory.
type AgentQuestionView struct {
	ID       string `json:"id"`
	Question string `json:"question"`
}

// agentDraftView 投影一次作者草稿：顺序原样保留，两个集合都输出 `[]`（不输出 null）。
// agentDraftView projects a draft, preserving file order and never emitting null.
func agentDraftView(result AgentDraftResult) AgentDraftView {
	items := make([]AgentDraftFileView, len(result.Files))
	for i, file := range result.Files {
		items[i] = AgentDraftFileView{Path: file.Path, Content: file.Content}
	}
	questions := make([]AgentQuestionView, len(result.Questions))
	for i, question := range result.Questions {
		questions[i] = AgentQuestionView{ID: question.ID, Question: question.Question}
	}
	return AgentDraftView{Agent: result.Agent, Files: items, Questions: questions}
}

// ScheduleDraftView 是 `POST /agents/{name}/schedule-draft` 的响应体（契约
// ScheduleDraftView）：候选四字段 + 服务端计算的未来三次触发时间（RFC3339 UTC，
// 不足三次就返回实际数量）。候选**尚未保存**，确认后走既有 `schedules` 条件写。
// ScheduleDraftView carries the unsaved candidate and the server-computed triggers.
type ScheduleDraftView struct {
	Agent    string   `json:"agent"`
	ID       string   `json:"id"`
	Cron     string   `json:"cron"`
	Zone     string   `json:"zone"`
	Message  string   `json:"message"`
	NextRuns []string `json:"next_runs"`
}

// scheduleDraftView 投影定时候选：时间字段在投影处统一格式化，空集合输出 `[]`。
// scheduleDraftView formats the trigger times here; an empty list never marshals to null.
func scheduleDraftView(result ScheduleDraftResult) ScheduleDraftView {
	runs := make([]string, len(result.NextRuns))
	for i, at := range result.NextRuns {
		runs[i] = formatTime(at)
	}
	return ScheduleDraftView{
		Agent:    result.Agent,
		ID:       result.Candidate.ID,
		Cron:     result.Candidate.Cron,
		Zone:     result.Candidate.Zone,
		Message:  result.Candidate.Message,
		NextRuns: runs,
	}
}

// AgentArchiveView 是归档记录（契约 AgentArchiveView / ArchivedAgentView）：
// `archived_at` 由 Unix 毫秒投影为 RFC3339 UTC；`restored_at` 未恢复时为空串；
// `restored_agent_name` 未恢复时为空串（原始值为 NULL 的零值形态）。
// AgentArchiveView is one archive row; archived_at is projected from Unix millis and
// an un-restored row carries empty strings.
type AgentArchiveView struct {
	ArchiveID         string `json:"archive_id"`
	Agent             string `json:"agent"`
	ArchivedAt        string `json:"archived_at"`
	ContentHash       string `json:"content_hash"`
	RestoredAt        string `json:"restored_at"`
	RestoredAgentName string `json:"restored_agent_name"`
}

// agentArchiveView 投影一条归档记录。
// agentArchiveView projects one archive record.
func agentArchiveView(rec core.AgentArchiveRecord) AgentArchiveView {
	view := AgentArchiveView{
		ArchiveID:         rec.ArchiveID,
		Agent:             rec.AgentName,
		ArchivedAt:        formatTime(time.UnixMilli(rec.ArchivedAt)),
		ContentHash:       rec.ContentHash,
		RestoredAgentName: rec.RestoredAgentName,
	}
	if rec.RestoredAt != nil {
		view.RestoredAt = formatTime(time.UnixMilli(*rec.RestoredAt))
	}
	return view
}

// agentArchiveViews 投影归档列表：顺序由调用方保持，空列表输出 `[]`。
// agentArchiveViews projects the archive list; an empty list is never null.
func agentArchiveViews(recs []core.AgentArchiveRecord) []AgentArchiveView {
	views := make([]AgentArchiveView, len(recs))
	for i, rec := range recs {
		views[i] = agentArchiveView(rec)
	}
	return views
}

// ResourceVersionView 是契约 ResourceVersionView：一条不可变版本行 + 指针状态。
// `resource_uid` 是版本所属的稳定身份——同名重建后新旧版本靠它区分（R4.5）；
// `published` / `applied` 不是版本行自身的列，由适配器在同一读取快照里按指针
// 判定；`reverted_from_version_id` 非撤回版本为 null（注 5 的派生口径）。
// ResourceVersionView carries the version row plus pointer-derived flags and the
// derived revert source; resource_uid separates versions across same-name rebuilds.
type ResourceVersionView struct {
	VersionID             int64  `json:"version_id"`
	ResourceKind          string `json:"resource_kind"`
	ResourceID            string `json:"resource_id"`
	ResourceUID           string `json:"resource_uid"`
	VersionNo             int    `json:"version_no"`
	ContentHash           string `json:"content_hash"`
	CreatedAt             string `json:"created_at"`
	OperationID           string `json:"operation_id"`
	Published             bool   `json:"published"`
	Applied               bool   `json:"applied"`
	RevertedFromVersionID *int64 `json:"reverted_from_version_id"`
}

// resourceVersionView 投影一条版本行：created_at 取 Unix 毫秒并统一格式化；
// 三个状态字段由调用方按指针与创建操作给出，**不**在视图层猜测。
// resourceVersionView projects one version row; the three state fields come from the
// caller's pointer/operation snapshot.
func resourceVersionView(v core.ResourceVersion, published, applied bool, revertedFrom *int64) ResourceVersionView {
	return ResourceVersionView{
		VersionID:             v.ID,
		ResourceKind:          string(v.Kind),
		ResourceID:            v.ResourceID,
		ResourceUID:           string(v.ResourceUID),
		VersionNo:             v.VersionNo,
		ContentHash:           v.ContentHash,
		CreatedAt:             formatTime(time.UnixMilli(v.CreatedAt)),
		OperationID:           v.OperationID,
		Published:             published,
		Applied:               applied,
		RevertedFromVersionID: revertedFrom,
	}
}

// ResourceVersionManifestView 是版本详情里 `manifest` 的对象形状：契约把它声明为
// 宽松对象（additionalProperties: true），本仓固定输出 `{"files": [...]}`，让技能
// 快照清单有稳定结构；模型连接 / MCP 的清单为空，输出 `{"files": []}`。
// ResourceVersionManifestView is the stable shape of the loose manifest object.
type ResourceVersionManifestView struct {
	Files []ResourceManifestFileView `json:"files"`
}

// ResourceManifestFileView 是清单中的一个文件（相对路径 / sha256 / 字节数）。
// ResourceManifestFileView is one manifest entry.
type ResourceManifestFileView struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ResourceVersionDetailView 是契约 ResourceVersionDetail（allOf）：版本行字段摊平 +
// `definition_yaml`（模型连接 / MCP 为脱敏后的块原文，技能恒为空串）+ `manifest`。
// 脱敏 fail-closed 由适配器完成——走到这里的内容已经过凭证扫描，本层只投影。
// ResourceVersionDetailView flattens the version fields and adds the redacted
// definition plus the manifest; redaction happens before this point.
type ResourceVersionDetailView struct {
	ResourceVersionView
	DefinitionYAML string                      `json:"definition_yaml"`
	Manifest       ResourceVersionManifestView `json:"manifest"`
}

// resourceVersionDetailView 投影一条版本详情。
// resourceVersionDetailView projects one version detail.
func resourceVersionDetailView(d core.ResourceVersionDetail) ResourceVersionDetailView {
	files := make([]ResourceManifestFileView, len(d.Manifest))
	for i, file := range d.Manifest {
		files[i] = ResourceManifestFileView{Path: file.Path, SHA256: file.SHA256, Size: file.Size}
	}
	return ResourceVersionDetailView{
		ResourceVersionView: resourceVersionView(d.Version, d.Published, d.Applied, d.RevertedFromVersionID),
		DefinitionYAML:      d.DefinitionYAML,
		Manifest:            ResourceVersionManifestView{Files: files},
	}
}

// ResourcePublishResultView 是契约 ResourcePublishResult：发布后的版本 + MCP 是否
// 待重启。`published` 恒为真（响应里的版本就是刚发布的版本）；`applied` 取
// "不待重启"——待重启意味着该版本尚未被运行时接管（约束 9 / 20）。
// `reverted_from_version_id` 只在"以旧版本创建了新版本"时派生自请求版本（注 5）。
// ResourcePublishResultView marks the returned version published; applied is the
// negation of pending_restart, and the revert source is derived from the request.
type ResourcePublishResultView struct {
	Version        ResourceVersionView `json:"version"`
	PendingRestart bool                `json:"pending_restart"`
}

// resourcePublishResultView 投影一次发布结果。请求版本号与新版本号相同说明这是
// 幂等发布（没有创建新版本），撤回来源保持 null；不同则说明按旧版本创建了新版本。
// resourcePublishResultView derives the revert source: an idempotent publish keeps it
// null, while a newly created version points back at the requested one.
func resourcePublishResultView(req core.ResourcePublishRequest, result core.ResourcePublishResult) ResourcePublishResultView {
	var revertedFrom *int64
	if req.VersionID != nil && *req.VersionID != result.Version.ID {
		revertedFrom = req.VersionID
	}
	return ResourcePublishResultView{
		Version:        resourceVersionView(result.Version, true, !result.PendingRestart, revertedFrom),
		PendingRestart: result.PendingRestart,
	}
}

// CredentialRevokeView 是契约 CredentialRevokeResult：只回状态与版本计数，
// `credential_ref` 是**服务端生成**的凭据身份引用，不携带任何凭据材料。
// `credential_state` 在成功路径上恒为 `revoked`（契约枚举只有这一个值）；
// `revoked_versions` 是撤销后该身份的全部版本数（活跃版本永远是 0）。
// `ETag` 只进响应头：契约的 CredentialRevokeResult 没有 etag 字段（与 scope 结果
// 不同），因此用 `json:"-"` 承载，响应体不重复。
// CredentialRevokeView is the contract's revoke result. ETag is header-only because
// the contract schema has no etag field on this one.
type CredentialRevokeView struct {
	ResourceUID     string `json:"resource_uid"`
	CredentialRef   string `json:"credential_ref"`
	CredentialState string `json:"credential_state"`
	RevokedVersions int    `json:"revoked_versions"`
	RevocationEpoch int    `json:"revocation_epoch"`

	ETag string `json:"-"`
}

// ResourceScopeView 是契约 ResourceScopeResult：`etag` 同时在响应体与响应头出现
// （与撤销端点不同，契约在这里声明了字段）。`owner_agent` 在 shared 时为空串；
// `affected_eligible_agents` 是影响提示（`all_eligible` 模式下会受影响但不会被
// 自动修改的 Agent 名），恒为数组——空集合输出 `[]`。
// ResourceScopeView is the contract's scope result; etag appears in both the body and
// the header, owner_agent is empty for shared, and the impact hint is never null.
type ResourceScopeView struct {
	ResourceUID            string   `json:"resource_uid"`
	Scope                  string   `json:"scope"`
	OwnerAgent             string   `json:"owner_agent"`
	Revision               int      `json:"revision"`
	ETag                   string   `json:"etag"`
	AffectedEligibleAgents []string `json:"affected_eligible_agents"`
}

// ResourceBoundAgentView 是契约 ResourceReferencesView.bound_agents 的一项：绑定
// 投影里的精确引用（`agent_uid` 与归档标志都来自投影与归档索引，不按名猜）。
// ResourceBoundAgentView is one exact binding-projection referrer.
type ResourceBoundAgentView struct {
	Agent    string `json:"agent"`
	AgentUID string `json:"agent_uid"`
	Archived bool   `json:"archived"`
}

// ResourceReferencesView 是契约 ResourceReferencesView：资源身份、定义名引用、绑定
// 投影引用与未释放的运行引用，三个列表为空时输出 `[]`——删除闸门与页面据此点名，
// **不得**用"当前没有运行"推断无引用。
// ResourceReferencesView always renders all three reference lists as arrays.
type ResourceReferencesView struct {
	Kind               string                   `json:"kind"`
	ID                 string                   `json:"id"`
	ResourceUID        string                   `json:"resource_uid"`
	PublishedVersionID int64                    `json:"published_version_id"`
	Agents             []string                 `json:"agents"`
	BoundAgents        []ResourceBoundAgentView `json:"bound_agents"`
	ActiveRuns         []string                 `json:"active_runs"`
}

// resourceReferencesView 投影引用联合视图。
// resourceReferencesView projects the joined reference view.
func resourceReferencesView(refs core.ResourceReferences) ResourceReferencesView {
	bound := make([]ResourceBoundAgentView, len(refs.BoundAgents))
	for i, agent := range refs.BoundAgents {
		bound[i] = ResourceBoundAgentView{
			Agent:    agent.Agent,
			AgentUID: string(agent.AgentUID),
			Archived: agent.Archived,
		}
	}
	return ResourceReferencesView{
		Kind:               string(refs.Kind),
		ID:                 refs.ID,
		ResourceUID:        string(refs.ResourceUID),
		PublishedVersionID: refs.PublishedVersionID,
		Agents:             stringSlice(refs.Agents),
		BoundAgents:        bound,
		ActiveRuns:         stringSlice(refs.ActiveRuns),
	}
}

// ModelConnectionTestResultView 是契约 ModelConnectionTestResult：结果脱敏，不含
// 凭证或完整请求；失败时 `ok=false` 且 `error_code` 只放固定短语。
// ModelConnectionTestResultView is the redacted model-connection test result.
type ModelConnectionTestResultView struct {
	OK           bool   `json:"ok"`
	LatencyMS    int64  `json:"latency_ms"`
	Model        string `json:"model"`
	ErrorCode    string `json:"error_code"`
	ErrorMessage string `json:"error_message"`
}

// modelConnectionTestResultView 投影模型连接测试结果（不携带工具清单——契约
// ModelConnectionTestResult 没有 `tools` 字段）。
// modelConnectionTestResultView drops the tool list, which this contract lacks.
func modelConnectionTestResultView(r core.ResourceTestResult) ModelConnectionTestResultView {
	return ModelConnectionTestResultView{
		OK:           r.OK,
		LatencyMS:    r.LatencyMS,
		Model:        r.Model,
		ErrorCode:    r.ErrorCode,
		ErrorMessage: r.ErrorMessage,
	}
}

// NotifyChannelTestResultView 是契约 NotifyChannelTestResult：一次渠道连通性测试的
// 脱敏结果。`status` 三值（success / failed / unknown）——unknown 表示可能已送达，
// 页面**不得**把它显示成失败或成功；`detail` 只含固定短语（目标 host、回执码、变量名）。
// NotifyChannelTestResultView is the redacted connectivity-test outcome; unknown means
// the receipt was ambiguous and must be shown as neither success nor failure.
type NotifyChannelTestResultView struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Detail    string `json:"detail"`
}

// notifyChannelTestResultView 投影渠道连通性测试结果。
// notifyChannelTestResultView projects the connectivity-test result.
func notifyChannelTestResultView(r core.NotifyChannelTestResult) NotifyChannelTestResultView {
	return NotifyChannelTestResultView{
		OK:        r.OK,
		Status:    r.Status,
		LatencyMS: r.LatencyMS,
		Detail:    r.Detail,
	}
}

// McpServerTestResultView 是契约 McpServerTestResult：结果与工具发现清单，均脱敏；
// 空工具清单输出 `[]`。
// McpServerTestResultView is the redacted MCP test result with discovered tools.
type McpServerTestResultView struct {
	OK           bool     `json:"ok"`
	LatencyMS    int64    `json:"latency_ms"`
	Tools        []string `json:"tools"`
	ErrorCode    string   `json:"error_code"`
	ErrorMessage string   `json:"error_message"`
}

// mcpServerTestResultView 投影 MCP 测试结果。
// mcpServerTestResultView projects the MCP test result.
func mcpServerTestResultView(r core.ResourceTestResult) McpServerTestResultView {
	return McpServerTestResultView{
		OK:           r.OK,
		LatencyMS:    r.LatencyMS,
		Tools:        stringSlice(r.Tools),
		ErrorCode:    r.ErrorCode,
		ErrorMessage: r.ErrorMessage,
	}
}

// SkillValidateResultView 是契约 SkillValidateResult：`problems` 为空当且仅当
// `valid=true`，空清单输出 `[]`。
// SkillValidateResultView renders problems as an array; empty means valid.
type SkillValidateResultView struct {
	Valid    bool     `json:"valid"`
	Problems []string `json:"problems"`
}

// skillValidateResultView 投影技能校验结果。
// skillValidateResultView projects the skill validation result.
func skillValidateResultView(r core.SkillValidateResult) SkillValidateResultView {
	// 技能校验问题由 workspace 自产中文（不是装载器拒因），原样投影即可。
	// Skill validation problems are self-authored Chinese; no mapping applies.
	return SkillValidateResultView{Valid: r.Valid, Problems: stringSlice(r.Problems)}
}

// SkillFileView 是契约 SkillFileView：工作副本 / 版本目录中的文本文件（路径 /
// 字节数 / sha256 / 正文）。二进制文件不会进入技能库。
// SkillFileView is one skill file with its hash and text content.
type SkillFileView struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// SkillDetailView 是契约 SkillDetail（allOf SkillView + files）：当前发布版本的
// 文件清单。由适配器填充；`files` 空时仍输出 `[]`（nonNilSkillDetail 兜底）。
// SkillDetailView is the published skill with its full file list.
type SkillDetailView struct {
	SkillView
	Files []SkillFileView `json:"files"`
}

// nonNilSkillDetail 保证技能详情的文件清单非 nil——契约的数组字段不接受 null。
// nonNilSkillDetail guarantees a non-nil file list.
func nonNilSkillDetail(view SkillDetailView) SkillDetailView {
	if view.Files == nil {
		view.Files = []SkillFileView{}
	}
	return view
}

// WorkspaceEntryView 是契约 WorkspaceEntry：相对根目录的路径、类型（file / dir /
// symlink）与文件字节数（目录与软链接为 0）。
// WorkspaceEntryView is one workspace entry; dirs and symlinks report size 0.
type WorkspaceEntryView struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// WorkspaceTreeView 是 `GET /workspace/tree` 的 data：目录条目数组（契约在此端点
// 直接用 WorkspaceEntry 数组，没有单独的 schema 名）。
// WorkspaceTreeView is the workspace tree payload.
type WorkspaceTreeView []WorkspaceEntryView

// nonNilWorkspaceTree 保证目录树非 nil；空目录输出 `[]`。
// nonNilWorkspaceTree guarantees an empty directory marshals to `[]`.
func nonNilWorkspaceTree(tree WorkspaceTreeView) WorkspaceTreeView {
	if tree == nil {
		return WorkspaceTreeView{}
	}
	return tree
}

// WorkspaceFileView 是契约 WorkspaceFile：单文件只读预览。`truncated` 恒为 false——
// 超过 256 KiB 的请求在适配器直接 400，**不**截断（约束 41）。
// WorkspaceFileView is the read-only preview; truncated is always false because
// oversized files are rejected instead of cut.
type WorkspaceFileView struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

// AgentConfigView 是 `GET /agents/{name}/config` 的 data（契约 AgentConfigView）：
// 结构化字段、**一个**与 `/content` 完全一致的强 `content_etag`，以及每个引用的
// 解析状态。**没有** `config_etag`——表单与原文共用同一个条件写基准；正文只在
// `/content` 读取，这里的 `fields` 不携带正文。
// AgentConfigView is the structured form view: one content_etag shared with /content,
// the field values, and the per-reference resolution; the body never appears here.
type AgentConfigView struct {
	Agent            string                     `json:"agent"`
	AgentUID         string                     `json:"agent_uid"`
	ContentETag      string                     `json:"content_etag"`
	Fields           AgentConfigFieldsView      `json:"fields"`
	NotifyMode       string                     `json:"notify_mode"`
	NotifyModeSource string                     `json:"notify_mode_source"`
	References       []AgentConfigReferenceView `json:"references"`
	BindingComplete  bool                       `json:"binding_complete"`
}

// AgentConfigFieldsView 是 `fields` 的结构：键名与 AGENT.md frontmatter 逐字对应，
// 值与原文一致（`temperature` 用指针保留"显式 0"与"未声明"的区别）。
// AgentConfigFieldsView mirrors the frontmatter keys; temperature keeps the explicit
// zero apart from an absent value.
type AgentConfigFieldsView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// DisplayName 是 AGENT.md 的原文值（可为空串）；展示投影另做回退，不在这里补。
	// DisplayName is the raw file value; list / detail views perform the fallback.
	DisplayName      string                    `json:"display_name"`
	Identity         AgentConfigIdentityView   `json:"identity"`
	Provider         AgentConfigProviderView   `json:"provider"`
	Tools            []string                  `json:"tools"`
	Skills           []string                  `json:"skills"`
	MCPServers       []string                  `json:"mcp_servers"`
	Bootstrap        []string                  `json:"bootstrap"`
	NotifyMode       string                    `json:"notify_mode"`
	NotifyChannels   []string                  `json:"notify_channels"`
	ResourceBindings AgentConfigBindingsView   `json:"resource_bindings"`
	Settings         AgentConfigSettingsView   `json:"settings"`
	Schedules        []AgentConfigScheduleView `json:"schedules"`
}

// AgentConfigIdentityView 是 identity 段。
// AgentConfigIdentityView is the identity block.
type AgentConfigIdentityView struct {
	AgentName string `json:"agent_name"`
	Prompt    string `json:"prompt"`
}

// AgentConfigProviderView 是 provider 段；`temperature` 为 nil 时序列化为 null。
// AgentConfigProviderView is the provider block; a nil temperature renders as null.
type AgentConfigProviderView struct {
	Name        string   `json:"name"`
	Model       string   `json:"model"`
	Temperature *float64 `json:"temperature"`
}

// AgentConfigBindingsView 是 `resource_bindings` 段：与 frontmatter 同形的两组
// `[{name, uid}]`，空组输出 `[]`。
// AgentConfigBindingsView is the bindings block in its frontmatter shape.
type AgentConfigBindingsView struct {
	ModelConnections []core.ResourceBinding `json:"model_connections"`
	NotifyChannels   []core.ResourceBinding `json:"notify_channels"`
}

// AgentConfigSettingsView 是 settings 段：缺省值已在装载期填好（10 / 20）。
// AgentConfigSettingsView carries the effective settings after load-time defaults.
type AgentConfigSettingsView struct {
	MaxIterations   int `json:"max_iterations"`
	MaxHistoryTurns int `json:"max_history_turns"`
}

// AgentConfigScheduleView 是一条定时声明（与 `schedules` 页签同一形状）。
// AgentConfigScheduleView is one schedule declaration.
type AgentConfigScheduleView struct {
	ID      string `json:"id"`
	Cron    string `json:"cron"`
	Zone    string `json:"zone"`
	Message string `json:"message"`
	// DeliverTo 是系统代投渠道的资源 UID；空串 = 不代投（R7）。
	DeliverTo string `json:"deliver_to"`
}

// AgentConfigReferenceView 是契约 AgentConfigReference：一条引用（或孤儿绑定）的
// 解析状态。`resource_uid` 为空串 = 未绑定；`resource_name` 是该 UID 的**当前**
// 名称（同名重建后与 `ref_name` 不同，页面据此提示"指向旧资源"）。
// AgentConfigReferenceView is one reference or orphan binding; an empty uid means
// unbound and resource_name is the uid's current name.
type AgentConfigReferenceView struct {
	ResourceKind    string `json:"resource_kind"`
	RefName         string `json:"ref_name"`
	ResourceUID     string `json:"resource_uid"`
	ResourceName    string `json:"resource_name"`
	BindingStatus   string `json:"binding_status"`
	Published       bool   `json:"published"`
	Eligible        bool   `json:"eligible"`
	Deleted         bool   `json:"deleted"`
	CredentialState string `json:"credential_state"`
}

// 绑定解析状态与通知模式来源的固定枚举（契约 AgentConfigReference.binding_status /
// AgentConfigView.notify_mode_source）：适配器填视图时引用这里，**不**各抄一份字面量。
// The fixed literals of the config view; adapters reference these instead of copying
// the strings.
const (
	BindingStatusBound     = "bound"
	BindingStatusMissing   = "missing"
	BindingStatusStaleUID  = "stale_uid"
	BindingStatusOrphan    = "orphan"
	BindingStatusDuplicate = "duplicate"

	NotifyModeSourceExplicit = "explicit"
	NotifyModeSourceLegacy   = "legacy_inferred"
)

// ErrorDataView 是失败信封 `data` 的白名单投影：只允许契约固定的四个字符串数组
// （agents / active_runs / run_ids / problems）、条件写的当前 ETag（current_etag）、
// `pending` 之后失败的 `operation_id`（约束 1a：失败行必须可经
// `GET /operations/{id}` 追查）与 `RESOURCE_UID_MISMATCH` 的当前 `resource_uid`。
// 其余键——定义正文、凭证、内部路径——**不得**经由本结构出站。
// ErrorDataView is the whitelisted failure payload: the four string arrays, the
// current ETag of a conditional write, the operation id of a post-pending failure,
// and the current resource uid of a uid mismatch; definition bodies and credentials
// have no carrier here.
// 四个数组用指针区分"未携带"（nil，字段省略）与"携带空数组"（非 nil 空切片，
// 输出 `[]`）——`omitempty` 会把空切片也省略掉，契约不允许。`current_etag` 与
// `operation_id` 同理：空串视为未携带。
// Pointer fields distinguish "absent" from "present but empty"; omitempty alone would
// drop an empty array, which the contract requires to render as [].
type ErrorDataView struct {
	Agents      *[]string `json:"agents,omitempty"`
	ActiveRuns  *[]string `json:"active_runs,omitempty"`
	RunIDs      *[]string `json:"run_ids,omitempty"`
	Problems    *[]string `json:"problems,omitempty"`
	CurrentETag *string   `json:"current_etag,omitempty"`
	OperationID *string   `json:"operation_id,omitempty"`
	ResourceUID *string   `json:"resource_uid,omitempty"`
}

// errorDataView 从 `core.AdminError` 的 data 里只取四个固定数组、`current_etag`
// 与 `operation_id`；值与契约形状不符的同名键（非 []string / 非非空字符串）按
// "没有该键"处理——宁可不给，也不出站未知形状。第二个返回值为真表示至少带了一个
// 白名单字段——空数组也算携带（输出 `[]`），空串 ETag / 操作标识不算。
// errorDataView extracts only the four contract-fixed arrays, the current ETag and
// the operation id; anything else is dropped.
func errorDataView(err error) (ErrorDataView, bool) {
	raw := core.AdminErrorData(err)
	if len(raw) == 0 {
		return ErrorDataView{}, false
	}
	var view ErrorDataView
	present := false
	if list, ok := stringListField(raw, "agents"); ok {
		view.Agents, present = &list, true
	}
	if list, ok := stringListField(raw, "active_runs"); ok {
		view.ActiveRuns, present = &list, true
	}
	if list, ok := stringListField(raw, "run_ids"); ok {
		view.RunIDs, present = &list, true
	}
	if list, ok := stringListField(raw, "problems"); ok {
		view.Problems, present = &list, true
	}
	if etag, ok := raw["current_etag"].(string); ok && etag != "" {
		view.CurrentETag, present = &etag, true
	}
	if operationID, ok := raw["operation_id"].(string); ok && operationID != "" {
		view.OperationID, present = &operationID, true
	}
	if uid, ok := raw["resource_uid"].(string); ok && uid != "" {
		view.ResourceUID, present = &uid, true
	}
	if !present {
		return ErrorDataView{}, false
	}
	return view, true
}

// stringListField 取一个字符串数组字段并保证非 nil（空数组也带 `[]` 形状）。
// stringListField copies a string-array field and keeps it non-nil.
func stringListField(raw map[string]any, key string) ([]string, bool) {
	value, ok := raw[key]
	if !ok {
		return nil, false
	}
	list, ok := value.([]string)
	if !ok {
		return nil, false
	}
	out := make([]string, len(list))
	copy(out, list)
	return out, true
}

// 带 `data` 的失败信封由写面 handler（T015 `admin_routes.go`）用
// `errorDataView` + `writeEnvelope` 组装：`writeError` 永不设置 data，而这里
// 只负责白名单投影，**不**持有第二个信封序列化出口。
// The data-carrying failure envelope is assembled by the write handlers from
// errorDataView + writeEnvelope; this file only owns the whitelist projection.

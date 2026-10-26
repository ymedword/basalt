package core

import (
	"errors"
	"fmt"
)

// 哨兵错误：跨包的判定锚点。调用方一律用 errors.Is 判卷——不得比对错误文本
// （文案面向用户，措辞可改；哨兵身份不可改）。
//
// Sentinel errors are the cross-package decision anchors. Callers must use
// errors.Is: user-facing wording may change, sentinel identity may not.

var (
	// ErrSchemaTooNew 表示库内版本高于二进制支持的版本——旧二进制不得打开新库。
	// ErrSchemaTooNew means the database is newer than this binary supports;
	// an old binary must never open a newer database.
	ErrSchemaTooNew = errors.New("database schema is newer than the binary supports")

	// ErrSchemaOutdated 表示库内版本旧于目标版本——迁移是持锁重路径的显式动作，
	// 引导（Bootstrap）与只读校验（OpenReadOnly）都在此拒绝；OpenWritable 不返回它。
	// ErrSchemaOutdated means the database is older than the target version.
	// Migration is an explicit, lock-holding action, so bootstrap and read-only
	// verification refuse here while OpenWritable does not.
	ErrSchemaOutdated = errors.New("database schema is older than the target version")

	// ErrSchemaCorrupt 表示 schema_version 表存在但零行——既非新库也非任何已知版本。
	// ErrSchemaCorrupt means the schema_version table exists but holds no row:
	// neither a fresh database nor any known version.
	ErrSchemaCorrupt = errors.New("schema_version table exists but has no version row")

	// ErrDatabaseMissing 表示库文件未就绪（尚未 init）——只读路径不代建。
	// ErrDatabaseMissing means the database file is not ready (not initialized);
	// read-only paths never create it as a side effect.
	ErrDatabaseMissing = errors.New("database file does not exist")

	// ErrWorkspaceNotInitialized 表示工作区未初始化（缺 .basalt/）。
	// ErrWorkspaceNotInitialized means the workspace is not initialized.
	ErrWorkspaceNotInitialized = errors.New("workspace is not initialized")

	// ErrProviderNotConfigured 表示请求的 provider 不在显式映射表内——路由查表不中，
	// 不遍历推断、不取"唯一的那个"当默认（宪法 III）。对外映射为 NOT_CONFIGURED（US-9）。
	// ErrProviderNotConfigured means the requested provider is not in the explicit map;
	// routing never iterates and never falls back to a default.
	ErrProviderNotConfigured = errors.New("provider is not configured")

	// ErrAuthorModelUnavailable 表示作者模型调用已经发出但未得到可用结果（上游拒绝、
	// 不可达或响应不可用）。US-12 R7（问题 8）：HTTP 层据此返回点名修复入口的 INTERNAL，
	// 而不是笼统的"服务器内部错误"；原始原因只进日志与 `llm_calls` 审计，不回显。
	// ErrAuthorModelUnavailable classifies an author-model call that produced no usable
	// result; the HTTP layer surfaces an actionable message instead of a generic 500.
	ErrAuthorModelUnavailable = errors.New("author model unavailable")
)

// —— R4 凭据、绑定与范围哨兵（契约 12 个新错误码的包内身份）——
// R4 sentinels for credentials, bindings and scope; the HTTP layer maps each to the
// contract code with the approved save-time / admission-time status split.
var (
	// ErrAgentResourceBindingMissing 表示引用缺少同名绑定（保存期 400 / 准入 409）。
	ErrAgentResourceBindingMissing = errors.New("agent resource binding is missing")
	// ErrAgentResourceBindingInvalid 表示绑定结构非法、孤儿或 UID 与当前指针不符。
	ErrAgentResourceBindingInvalid = errors.New("agent resource binding is invalid")
	// ErrAgentResourceUnavailable 表示绑定的资源不存在、已删除或未发布。
	ErrAgentResourceUnavailable = errors.New("agent resource is unavailable")
	// ErrResourceNotEligible 表示资源不在该 Agent 的可用范围内（保存期 400 / 准入 409）。
	ErrResourceNotEligible = errors.New("resource is not eligible for this agent")
	// ErrCredentialRevoked 表示绑定的凭据已撤销（保存期 400 / 准入 409）。
	ErrCredentialRevoked = errors.New("credential is revoked")
	// ErrCredentialUnavailable 表示托管凭据无法在当前主密钥下读取（两阶段均 503）。
	ErrCredentialUnavailable = errors.New("credential is unavailable")
	// ErrCredentialModeRequired 表示凭据值形态与模式不符且未显式声明目标模式。
	ErrCredentialModeRequired = errors.New("credential mode is required")
	// ErrCredentialRefReadOnly 表示请求体出现了服务端生成的只读 credential_ref。
	ErrCredentialRefReadOnly = errors.New("credential_ref is read-only")
	// ErrCredentialOwnerMismatch 表示凭据 owner 与目标资源 UID 不一致。
	ErrCredentialOwnerMismatch = errors.New("credential owner does not match the resource")
	// ErrResourceScopeInUse 表示共享转专属时仍被其他 Agent / 活动 Run / 路由引用。
	ErrResourceScopeInUse = errors.New("resource scope is in use")
	// ErrResourceUIDMismatch 表示显式 resource_uid 与名称当前指针不一致。
	ErrResourceUIDMismatch = errors.New("resource uid does not match the current pointer")
	// ErrAgentCopyBlocked 表示复制遇到专属资源且未提供显式映射。
	ErrAgentCopyBlocked = errors.New("agent copy requires an explicit resource mapping")
)

// —— 运行面错误类型（本 US 新增；既有哨兵一个不动）——
// Run-surface error types added by this US; the sentinels above stay untouched.

// SandboxViolationError 是一次沙箱拒绝，文案**四段式**（技术方案 §4.3 逐字）：
// `<规则>：<越界值>。这是安全策略，请勿反复重试；<放行指引：配置键 + 需重启>`。
// 三个字段承载可变量、第三句是固定句（不发散、不改写——它是四方共享的判卷锚点）。
// 拒绝信息**原样进工具结果**：模型看不到"为什么被拒"就不会收敛，故**不得**吞掉、
// **不得**替换为泛化文案；审计侧由 `IsSandboxViolation` 分类为 `sandbox_denied`。
// SandboxViolationError is one sandbox refusal with the frozen four-segment wording.
// Its message travels into the tool result verbatim and must never be swallowed or
// generalized away — a model that cannot see why it was refused cannot self-correct.
type SandboxViolationError struct {
	Rule     string // 段 1 规则：哪条策略拒的（白名单 / 保护规则 / 跨 Agent 授权…）
	Target   string // 段 2 越界值：被拒的具体目标（路径 / 可执行名 / URL）
	Guidance string // 段 4 放行指引：点名配置键 + 需重启
	// Reason 是 `sandbox_denied` 的细分原因（14D 契约 `policy_denial_reason`）：只对
	// 出网面四类填写，其余沙箱拒绝留空（空串 = 不细分，不是第五类）。
	// Reason refines an outbound denial into the four contract reasons; other sandbox
	// refusals leave it empty, which means "not subdivided" rather than a fifth value.
	Reason PolicyDenialReason
}

// Error 拼出四段式文案（段 3 是固定句，不参与拼接决策）。
// Error renders the four-segment message; segment three is a fixed sentence.
func (e *SandboxViolationError) Error() string {
	return fmt.Sprintf("%s：%s。这是安全策略，请勿反复重试；%s", e.Rule, e.Target, e.Guidance)
}

// IsSandboxViolation 判定错误链里是否有沙箱拒绝——`ToolExecutor` 据此把失败分类为
// `sandbox_denied`（**不得**比对错误文本，判据只认类型）。
// IsSandboxViolation reports whether the error chain contains a sandbox refusal; the
// classification decision is by type, never by message text.
func IsSandboxViolation(err error) bool {
	var target *SandboxViolationError
	return errors.As(err, &target)
}

// AgentNotFoundError 表示请求的 Agent 不存在——**调用方问题**：零值 RunResult + 该错误
// 如实返回，**不排队**、不落任何运行痕迹（对外映射 404；US-9）。
// AgentNotFoundError reports an unknown agent: a caller-side error, never queued.
type AgentNotFoundError struct {
	Name string // 请求的 Agent 名（原样带回，便于排障）
}

// Error 返回不带标点的消息（错误文本不大写、不以标点结尾——工程规范 §4）。
// Error renders the message without trailing punctuation.
func (e *AgentNotFoundError) Error() string { return "agent not found: " + e.Name }

// SessionBusyError 表示同一会话已有运行在跑——**调用方问题**：直接拒绝、**不排队**
// （排队会让调用方拿到"后来才失败"的调度语义；对外映射 409；US-9）。
// SessionBusyError reports that the session already has a run in flight: the call is
// refused outright rather than queued.
type SessionBusyError struct {
	SessionID string // 忙的那个会话
}

// Error 返回不带标点的消息。
// Error renders the message without trailing punctuation.
func (e *SessionBusyError) Error() string { return "session is busy: " + e.SessionID }

// InstanceFullError 表示实例并发运行数已达上限——**调用方问题**：直接拒绝、**不排队**
// （对外映射 429；US-9）。上限是实例级配置，不是单次运行的预算。
// InstanceFullError reports that the instance is at its concurrent-run capacity.
type InstanceFullError struct {
	Limit int // 实例并发上限（读数原样带回）
}

// Error 返回不带标点的消息。
// Error renders the message without trailing punctuation.
func (e *InstanceFullError) Error() string {
	return fmt.Sprintf("instance is at capacity (limit %d)", e.Limit)
}

// —— 工具链失败承载（US-5 新增）——
// Tool-chain failure carriers added by US-5.

// ExternalError 是**外部系统错误**的承载（契约 §7.3 `external_error`）：4xx / 5xx 响应与
// DNS 解析失败 / 连接被拒 / TLS 握手失败 / 响应读取失败。**不复用** `provider.CallError`
// （core 只依赖标准库，反向 import 为禁），与 `SandboxViolationError` **平级**。
// 文案纪律（口径 17）：`Error()` 只输出**自产类别摘要**——**不得**带 URL 原文 / query /
// 响应体 / Location 原文（`url.Error` 的原文含整条 URL，直接透传即泄漏）；原始错误只经
// `Unwrap()` 供 `errors.Is` / `errors.As` 分类与排障，**不进文案**。
// ExternalError carries an external-system failure: 4xx/5xx responses and DNS / connect /
// TLS / read failures. Its message is a self-authored summary only; the underlying error
// stays reachable through Unwrap and never enters the text.
type ExternalError struct {
	Target string // 外部目标：状态码与 host 的组合，或 host（不含 path / query / userinfo）
	Reason string // 自产类别摘要（如「连接失败：example.com」「HTTP 404：example.com」）
	Cause  error  // 原因链：只经 Unwrap 供分类与排障，不进文案
}

// Error 返回自产的安全摘要——即 `Reason` 本身（不做任何拼接、不引入外部原文）。
// Error returns the self-authored summary verbatim.
func (e *ExternalError) Error() string { return e.Reason }

// Unwrap 暴露原因链：`errors.Is` / `errors.As` 可穿透到原始错误——两条防线并存，
// 即便未来某处把 ctx 错误经 `%w` 包进 `Cause`，取消 / 截止仍判得出来（约束 20）。
// Unwrap exposes the cause chain so cancellation identity survives even if some future
// site wraps a context error into Cause.
func (e *ExternalError) Unwrap() error { return e.Cause }

// IsExternalError 判定错误链里是否有外部系统错误——`ToolExecutor` 据此分类为
// `external_error`（判据只认类型，**不比文本**）。
// IsExternalError reports whether the chain contains an external-system failure.
func IsExternalError(err error) bool {
	var target *ExternalError
	return errors.As(err, &target)
}

// InvalidInputError 是**入参非法**的承载（契约 §7.3 `invalid_input`）：模型给的参数不合法，
// 不是本仓缺陷（注 11）。两类产出方产出同一类别、分层不重复——执行器入口判「合法 JSON
// 对象 + 必填键」（`checkToolInput`），工具在解码点判「字段类型与取值」。`Reason` 承载
// **完整可读文案**（模型可见）；既有文件工具的失败文本因此逐字不变、只换类型。
// InvalidInputError carries an invalid-argument failure produced by the executor entry
// check or a tool's own decode point: the model's input was invalid, not a defect here.
type InvalidInputError struct {
	Tool   string // 工具名（点名哪件工具的入参）
	Reason string // 可读原因（完整文案，原样进工具结果）
}

// Error 返回可读原因文本。
// Error returns the readable reason verbatim.
func (e *InvalidInputError) Error() string { return e.Reason }

// IsInvalidInput 判定错误链里是否有入参非法——`ToolExecutor` 据此分类为 `invalid_input`
// （判据只认类型，**不比文本**）。
// IsInvalidInput reports whether the chain contains an invalid-input failure.
func IsInvalidInput(err error) bool {
	var target *InvalidInputError
	return errors.As(err, &target)
}

// NotifyDisabledError 是**通知被禁用**的承载（契约 §7.3 `notify_disabled`）：Agent 的
// `notify_mode: none`，或渠道的托管凭据已撤销。它既不是调用方参数错误、也不是外部系统
// 故障——重试不会变好，模型应停止换渠道重试；因此工具在**任何查表与外呼之前**返回它。
// 文案是固定错误：`none` 逐字取 R4.7 的固定句，凭据撤销点名渠道与修复入口。
// NotifyDisabledError carries a disabled notification surface (mode none or a revoked
// channel credential). It is returned before any registry read or outbound call, and
// retrying cannot help.
type NotifyDisabledError struct {
	Reason string // 固定文案（模型可见；不含地址、密钥或内部路径）
}

// Error 返回固定文案。
// Error returns the fixed message.
func (e *NotifyDisabledError) Error() string { return e.Reason }

// IsNotifyDisabled 判定错误链里是否有通知禁用——`ToolExecutor` 据此分类为
// `notify_disabled`（判据只认类型，**不比文本**）。
// IsNotifyDisabled reports whether the chain contains a disabled notification surface.
func IsNotifyDisabled(err error) bool {
	var target *NotifyDisabledError
	return errors.As(err, &target)
}

// —— 调度面错误载体（US-10 新增；既有哨兵与错误类型一个不动）——
// Scheduling-surface error carriers added by US-10; existing sentinels stay untouched.

// SessionArchivedError 表示目标会话已归档——调用方问题：不排队、不重建、不发起运行
// （调度到点映射为执行行 `skipped`，HTTP 映射 400）。
// SessionArchivedError reports an archived session: the call is refused outright, never
// queued, and the session is never recreated.
type SessionArchivedError struct{ SessionID string }

// Error 返回不带标点的消息。
// Error renders the message without trailing punctuation.
func (e *SessionArchivedError) Error() string { return "session is archived: " + e.SessionID }

// SessionNotFoundError 表示要求复用的会话不存在（`RequireExistingSession` 打开时；
// HTTP 映射 404）。自由会话请求不受影响——缺失即按既有逻辑自动创建。
// SessionNotFoundError means the session to reuse does not exist (only when
// RequireExistingSession is set); free-session requests still create one as before.
type SessionNotFoundError struct{ SessionID string }

// Error 返回不带标点的消息。
// Error renders the message without trailing punctuation.
func (e *SessionNotFoundError) Error() string { return "session not found: " + e.SessionID }

// SessionGateUnavailableError 表示装配缺失 `SessionStatusStore`——装配缺陷（HTTP 映射 500），
// 不是调用方问题；`Archive` 在 nil 门禁时返回它，避免把缺陷伪装成业务拒绝。
// SessionGateUnavailableError reports a missing SessionStatusStore: an assembly defect
// mapped to 500, never disguised as a business refusal.
type SessionGateUnavailableError struct{}

// Error 返回不带标点的消息。
// Error renders the message without trailing punctuation.
func (e *SessionGateUnavailableError) Error() string { return "session gate is not configured" }

// ErrTaskOwnerConflict 表示 task_id 已归属其他 Agent——跨 Agent 接管一律拒绝（约束 42；
// retired 行同样算占用）。启动对账撞上它是致命错误，运行期事务撞上它整批回滚。
// ErrTaskOwnerConflict means the task_id already belongs to another agent; retired rows
// count as owned too. Startup treats it as fatal and runtime transactions roll back.
var ErrTaskOwnerConflict = errors.New("scheduler: task id belongs to another agent")

// —— MCP supervisor 判定锚点（US-12 新增）——
// MCP supervisor decision anchor added by US-12.

// ErrPolicyDenied 表示 MCP 连接 / 测试在启动子进程或建立连接之前被沙箱策略拒绝。
// 调用方用 `errors.Is` 判定（不比对文案）；被拒的原始四段式 `*SandboxViolationError`
// 仍在错误链上，两者可同时判出（约束 20：拒绝即不连接）。
// ErrPolicyDenied marks an MCP start/test refused by the sandbox policy before any
// process or connection was created. The four-segment refusal stays in the chain, so
// errors.Is works for both identities.
var ErrPolicyDenied = errors.New("mcp policy denied")

// —— 管理写面错误分类（US-12 新增；既有哨兵一个不动）——
// Management write-surface classifications added by US-12; existing sentinels stay
// untouched.

var (
	// ErrConflict 表示目标已存在或状态冲突（同名资源、归档标识占用）——web 映射 409
	// CONFLICT。它与 ErrPreconditionFailed 的区别是：后者是条件写的前置条件不满足，
	// 前者是请求本身与当前状态不可共存。
	// ErrConflict marks an existing target or an incompatible state (duplicate name,
	// occupied archive id); the web layer maps it to 409 CONFLICT.
	ErrConflict = errors.New("conflict")

	// ErrBusy 表示目标仍被在途工作占用（归档等待超时、Agent 有活动 Run）——web 映射
	// 409 AGENT_BUSY。它不是调用方参数错误，重试可能在占用释放后成功。
	// ErrBusy marks a target still held by in-flight work; the web layer maps it to
	// 409 AGENT_BUSY. Retrying may succeed once the holder releases it.
	ErrBusy = errors.New("busy")

	// ErrImportRejected 表示导入候选未通过边界校验（越界、软链接、非 UTF-8、超限）——
	// web 映射 400 IMPORT_REJECTED。它与 BAD_REQUEST 分开，便于调用方区分「请求格式错」
	// 与「候选内容被安全边界拒绝」。
	// ErrImportRejected marks an import candidate refused by a safety boundary (escape,
	// symlink, non-UTF-8, size/count limits); the web layer maps it to 400
	// IMPORT_REJECTED.
	ErrImportRejected = errors.New("import rejected")

	// ErrPolicyRevisionConflict 表示策略行的修订号已在别处推进（CAS 冲突）：条件写
	// 以失败关闭，不覆盖他人刚写入的值。web 映射 412。
	// ErrPolicyRevisionConflict marks a lost revision CAS; the write fails closed and
	// never overwrites the value that won.
	ErrPolicyRevisionConflict = errors.New("policy revision conflict")

	// ErrExecutionProfileMismatch 表示请求里的 `expected_profile`（客户端渲染确认框时
	// 看到的档位）与库内当前档位不一致：`If-Match` 不能代替它——用户确认的是"从 X 切到
	// Y"这个方向，方向已经不成立就必须停下。web 映射 409 `EXECUTION_PROFILE_MISMATCH`。
	// ErrExecutionProfileMismatch stops a stale confirmation dialog from silently
	// applying a direction the user never saw.
	ErrExecutionProfileMismatch = errors.New("execution profile mismatch")
)

// AdminError 把管理写面分类与结构化 `data` 绑定：错误信封的 `data` 只承载契约固定
// 字段（`current_etag` / `agents` / `active_runs` / `run_ids` / `problems`，以及
// `pending` 之后失败的 `operation_id`），**不得**回显定义正文或凭证。`Unwrap` 返回
// 分类哨兵，调用方一律用 `errors.Is` 判卷。
// AdminError binds a management classification to structured data. The error
// envelope's data carries only contract-fixed fields (including the operation id of a
// post-pending failure), never definition bodies or credentials; Unwrap exposes the
// sentinel so callers judge with errors.Is.
type AdminError struct {
	Kind    error          // 分类哨兵（ErrConflict / ErrBusy / ErrImportRejected 等）
	Message string         // 对外文案（不含凭证与绝对路径）
	Data    map[string]any // 契约固定 data 字段；nil 表示无 data
}

// Error 返回对外文案；Message 为空时退回哨兵文本。
// Error returns the public message, falling back to the sentinel text.
func (e *AdminError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Kind.Error()
}

// Unwrap 暴露分类哨兵，使 errors.Is(err, ErrConflict) 等判定穿透包装。
// Unwrap exposes the classification sentinel to errors.Is.
func (e *AdminError) Unwrap() error { return e.Kind }

// AdminErrorData 取错误链上的结构化 data；没有时返回 nil。
// AdminErrorData returns the structured data carried by the chain, or nil.
func AdminErrorData(err error) map[string]any {
	var target *AdminError
	if errors.As(err, &target) {
		return target.Data
	}
	return nil
}

// AgentBusyError 表示归档等待在途 Run 的有界上限到期（约束 32 ③）：仍有 Run 未退出，
// 已发出的取消信号保留（**不**回滚取消动作）。RunIDs 是等待结束时仍在运行的标识；
// OperationID 非空时随 `data.operation_id` 出站（约束 1a：`pending` 之后的失败必须可追查）。
// 它解包为 `*AdminError{Kind: ErrBusy, Data: run_ids[/operation_id]}`，因此调用方既能用
// `errors.As` 取出 RunIDs，也能沿用 `errors.Is(err, ErrBusy)` 与 `AdminErrorData` 的既有判卷。
// AgentBusyError reports that the bounded wait for in-flight runs expired. It unwraps
// to an AdminError carrying data.run_ids (and the operation id when set), so errors.As,
// errors.Is and AdminErrorData all keep working through the same chain.
type AgentBusyError struct {
	RunIDs      []string
	OperationID string
}

// Error 返回不带 Run 列表的固定文案（列表只经 data.run_ids 对外，避免把标识拼进
// 自由文本后无法机判）。
// Error renders the fixed message; the ids travel through data.run_ids only.
func (e *AgentBusyError) Error() string {
	return "agent is busy: in-flight runs did not finish within the wait limit"
}

// Unwrap 暴露携带 `run_ids`（与可选 `operation_id`）的 AdminError，使 web 的 409
// AGENT_BUSY 映射与 `data` 投影都复用既有通路。
// Unwrap exposes the AdminError that carries run_ids and the optional operation id.
func (e *AgentBusyError) Unwrap() error {
	data := map[string]any{"run_ids": e.RunIDs}
	if e.OperationID != "" {
		data["operation_id"] = e.OperationID
	}
	return &AdminError{
		Kind:    ErrBusy,
		Message: "Agent 仍有在途 Run，等待上限内未退出",
		Data:    data,
	}
}

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
)

// —— 运行面错误类型（本 US 新增；既有哨兵一个不动）——
// Run-surface error types added by this US; the sentinels above stay untouched.

// SandboxViolationError 是一次沙箱拒绝，文案**四段式**（技术方案 §6.5 逐字）：
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

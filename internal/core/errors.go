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

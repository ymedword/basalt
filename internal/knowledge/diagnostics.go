package knowledge

// 本文件声明检索侧的诊断**消费方类型与端口**（US-16 §3.1.6 / C-06）：
// `knowledge` **不得** import `internal/diagnostics`（技术方案 §7.1.2），诊断事件
// 因此在包内以窄结构声明；装配层的适配器把它翻译成冻结契约事件
// （`internal/diagnostics.DiagnosticEvent`），并补齐写入器负责的标识、版本、
// 事件种类与关联结构。翻译规则由 `internal/app/knowledge_adapters_test.go` 逐字
// 锁死：原因码或判定改名时那组用例先红。
//
// 诊断可丢弃：写入**没有返回值**，失败只 WARN——审计才是 fail-closed 的那一轨。
//
// This file declares the retrieval-side diagnostic types and port. TechnicalDesign
// §7.1.2 keeps knowledge away from internal/diagnostics, so the event is a narrow
// local struct; the app adapter maps it to the frozen contract event and fills the
// writer-owned identifiers, version, kind and correlation. Diagnostics are droppable,
// so the port has no return value and failures are logged only.

import (
	"context"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// DiagnosticReason 是检索诊断的稳定原因码，取值与冻结契约 `diagnostic_events`
// 的 `reason_code` 一致（适配器负责逐值翻译）。
// DiagnosticReason carries the retrieval reason codes; the adapter maps each literal
// onto the frozen contract value.
type DiagnosticReason string

const (
	// DiagnosticReasonVectorUnavailable 表示向量通道不可用（降级到关键词）。
	// DiagnosticReasonVectorUnavailable marks the vector path being unavailable.
	DiagnosticReasonVectorUnavailable DiagnosticReason = "vector_unavailable"
	// DiagnosticReasonKeywordUnavailable 表示关键词通道不可用。
	// DiagnosticReasonKeywordUnavailable marks the keyword path being unavailable.
	DiagnosticReasonKeywordUnavailable DiagnosticReason = "keyword_unavailable"
	// DiagnosticReasonPermissionDenied 表示权限拒绝；被拒资料**只**允许计数。
	// DiagnosticReasonPermissionDenied marks a permission rejection; only counts leak.
	DiagnosticReasonPermissionDenied DiagnosticReason = "permission_denied"
	// DiagnosticReasonBudgetTrimmed 表示按预算裁剪掉整条片段。
	// DiagnosticReasonBudgetTrimmed marks whole chunks dropped to fit the budget.
	DiagnosticReasonBudgetTrimmed DiagnosticReason = "budget_trimmed"
	// DiagnosticReasonCitationUnresolved 表示答案里出现了本次上下文**没读过**的引用
	// 标识（V-6 引用复核；契约 `citation_unresolved`）。它只带计数——未解析标识本身
	// 不落诊断（C-08 同理：标识也不外泄）。
	// DiagnosticReasonCitationUnresolved marks citation ids the context never read; only
	// the count is recorded.
	DiagnosticReasonCitationUnresolved DiagnosticReason = "citation_unresolved"
	// DiagnosticReasonQuotaExhausted 表示摄取配额预留被拒（周期额度不足）。
	// DiagnosticReasonQuotaExhausted marks a rejected ingest reservation.
	DiagnosticReasonQuotaExhausted DiagnosticReason = "quota_exhausted"
	// DiagnosticReasonRateLimited 表示调用方速率超限。
	// DiagnosticReasonRateLimited marks a caller rate refusal.
	DiagnosticReasonRateLimited DiagnosticReason = "rate_limited"
	// DiagnosticReasonConcurrencyLimited 表示并发名额用尽（会随在跑请求结束恢复，
	// 与周期配额用尽分开归因）。
	// DiagnosticReasonConcurrencyLimited marks an exhausted concurrency slot.
	DiagnosticReasonConcurrencyLimited DiagnosticReason = "concurrency_limited"
)

// DiagnosticDecision 是诊断判定，取值与冻结契约 `decision` 一致。
// DiagnosticDecision carries the contract decision literals.
type DiagnosticDecision string

const (
	// DiagnosticDecisionAllow 表示放行（配额判定有 allow 态，与检索的降级类判定不同）。
	// DiagnosticDecisionAllow marks an admission; quota verdicts have an allow state.
	DiagnosticDecisionAllow DiagnosticDecision = "allow"
	// DiagnosticDecisionDeny 表示拒绝。
	// DiagnosticDecisionDeny marks a rejection.
	DiagnosticDecisionDeny DiagnosticDecision = "deny"
	// DiagnosticDecisionTrim 表示裁剪。
	// DiagnosticDecisionTrim marks a trim.
	DiagnosticDecisionTrim DiagnosticDecision = "trim"
	// DiagnosticDecisionDegrade 表示降级。
	// DiagnosticDecisionDegrade marks a degradation.
	DiagnosticDecisionDegrade DiagnosticDecision = "degrade"
)

// DiagnosticEvent 是一条待写的检索诊断事件：只含领域侧能确定的部分，写入器
// 负责的标识 / 版本 / 种类 / 关联结构由适配器补齐。
// DiagnosticEvent carries what the domain knows; the adapter adds the writer-owned
// identifier, schema version, kind and correlation.
type DiagnosticEvent struct {
	// OccurredAt 是发生时刻（适配器转成 Unix 毫秒 UTC）。
	OccurredAt time.Time
	// RunID 是主锚点；为空时 `recordEvent` 不写事件（不伪造关联）。
	RunID string
	// StepID 是同一 Run 内的步骤标识。
	StepID string
	// Actor 是谁在执行，取值与 `runs.actor` 同口径。
	Actor core.ActorKind
	// Reason 是稳定原因码。
	Reason DiagnosticReason
	// Decision 是判定结果。
	Decision DiagnosticDecision
	// Attrs 是已脱敏的白名单键值；**不得**含正文、凭证或被拒资料。
	Attrs map[string]any
}

// DiagnosticRecorder 是诊断事件写入端口（§3.1.6 的 `Recorder`）：**无返回值**，
// 失败只 WARN——诊断可丢弃，审计才是 fail-closed 的那一轨。
// DiagnosticRecorder writes diagnostic events; it never fails the business call.
type DiagnosticRecorder interface {
	Record(ctx context.Context, event DiagnosticEvent)
}

// IngestQuotaDiagnostic 是一条**摄取配额判定**事件（V-8）。它与检索事件分开声明，
// 因为主锚点不同：摄取作业用 `ingest_job_id`（冻结契约 §2.9 的关联表逐字写明），
// 检索事件用 `run_id`——把两种锚点塞进同一个结构会让"锚点为空就不写"的规则分叉。
//
// IngestQuotaDiagnostic is one ingest quota decision; it is anchored on ingest_job_id
// rather than run_id, so it does not share the retrieval event struct.
type IngestQuotaDiagnostic struct {
	// IngestJobID 是主锚点；为空时调用方不写事件（不伪造关联）。
	IngestJobID string
	// StepID 标明是作业内的哪一步：`reserve`（字节预留）或 `embed:<seq>`（第 seq 批）。
	StepID string
	// Seq 是同锚点内的本地序号（0 = 字节预留，其余等于批序号）。
	Seq int64
	// Class 是配额类目，取值与实现侧 `RequestClass` 一致（摄取固定 `ingest`）。
	Class string
	// Decision 是判定结果（allow / deny）。
	Decision DiagnosticDecision
	// Reason 只在拒绝时给出；放行时为空。
	Reason DiagnosticReason
}

// IngestQuotaDiagnosticRecorder 是摄取配额判定的写端口：**无返回值**，与检索端口
// 同一口径——诊断可丢弃，配额判定本身的业务语义（拒绝即不发出调用）不受它影响。
// IngestQuotaDiagnosticRecorder writes ingest quota decisions; it never fails the call.
type IngestQuotaDiagnosticRecorder interface {
	RecordIngestQuota(ctx context.Context, event IngestQuotaDiagnostic)
}

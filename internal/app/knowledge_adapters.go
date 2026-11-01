package app

// 本文件把知识库的消费方端口适配到调用方配额与诊断写入器（US-16 C-06）：
// `internal/knowledge` **不得** import `internal/callers` / `internal/diagnostics`
// （技术方案 §7.1.2），值类型、哨兵与原因码的翻译因此落在 `app`——唯一允许
// 「什么都知道」的装配层。翻译一律显式 switch，不做反射或字符串拼接；
// `knowledge_adapters_test.go` 逐值锁死契约字面量，原因码 / 判定 / 哨兵任一改名
// 都会先在那里变红。
//
// 两条错误口径：
//   - 配额哨兵用**双 `%w`** 同时保留实现侧与领域侧标识：知识面按领域哨兵分类
//     `reason_code`，HTTP 面按 `callers.ErrQuotaExhausted` 等映射 429——两边都必须
//     成立（`writeKnowledgeError` 只认后者）；
//   - 诊断可丢弃：未知字面量只 WARN 并丢弃事件；事件标识、契约版本与关联结构由
//     适配器与写入器共同补齐，`knowledge` 侧不碰这些字段。
//
// These adapters bridge the knowledge consumer ports onto the callers limiter and the
// diagnostics recorder. Double-%w wrapping keeps both sentinel spellings matchable, and
// the diagnostic translation is an explicit switch pinned by a contract test.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/knowledge"
)

// knowledgeQuotaAdapter 把摄取的窄配额端口接到 `*callers.Limiter`：类别固定
// `ClassIngest`（知识面只有摄取记账），幂等键原样透传。
// knowledgeQuotaAdapter binds the ingest-only quota port to the callers limiter with
// the request class fixed to ClassIngest.
type knowledgeQuotaAdapter struct {
	limiter *callers.Limiter
}

// newKnowledgeQuotaAdapter 构造配额适配器。装配层传入的是与对话调用同一个
// 限制器实例——另建一个会让同一调用方出现两套账（C-35 的单实例裁决）。
// newKnowledgeQuotaAdapter wraps the one limiter shared with chat calls.
func newKnowledgeQuotaAdapter(limiter *callers.Limiter) knowledge.Quota {
	return &knowledgeQuotaAdapter{limiter: limiter}
}

// AdmitCall 预留一次摄取成本；实现侧哨兵按领域口径翻译（见 knowledgeQuotaError）。
// AdmitCall reserves one ingest cost and translates implementation sentinels.
func (a *knowledgeQuotaAdapter) AdmitCall(ctx context.Context, p core.Principal,
	cost knowledge.QuotaCost, idem string,
) (knowledge.QuotaTicket, error) {
	ticket, err := a.limiter.AdmitCall(ctx, p, callers.ClassIngest, callers.Cost{
		Tasks:  cost.Tasks,
		Tokens: cost.Tokens,
		Bytes:  cost.Bytes,
	}, idem)
	if err != nil {
		return knowledge.QuotaTicket{}, knowledgeQuotaError(err)
	}
	return knowledgeQuotaTicket(ticket), nil
}

// Commit 按实际用量结算；知识面不消费结算视图，因此只回错误。
// Commit settles actual usage; knowledge does not consume the settlement view.
func (a *knowledgeQuotaAdapter) Commit(ctx context.Context, t knowledge.QuotaTicket,
	usage knowledge.QuotaUsage,
) error {
	_, err := a.limiter.Commit(ctx, callerQuotaTicket(t), callers.Usage{
		Tasks:  usage.Tasks,
		Tokens: usage.Tokens,
		Bytes:  usage.Bytes,
	})
	return knowledgeQuotaError(err)
}

// Release 归还「调用证明未发出」的预留；原因字面量逐值翻译，未知值拒绝。
// Release returns a reservation proven unconsumed; an unknown reason is rejected.
func (a *knowledgeQuotaAdapter) Release(ctx context.Context, t knowledge.QuotaTicket,
	reason knowledge.QuotaReleaseReason,
) error {
	var callerReason callers.ReleaseReason
	switch reason {
	case knowledge.QuotaReleaseNotSent:
		callerReason = callers.ReleaseNotSent
	case knowledge.QuotaReleaseAuditFailed:
		callerReason = callers.ReleaseAuditFailed
	default:
		return fmt.Errorf("knowledge: 未知归还原因 %q", string(reason))
	}
	return knowledgeQuotaError(a.limiter.Release(ctx, callerQuotaTicket(t), callerReason))
}

// knowledgeQuotaTicket 把实现侧票据投影成领域票据（字段往返无损，含归属校验位）。
// knowledgeQuotaTicket projects the implementation ticket onto the domain ticket.
func knowledgeQuotaTicket(t callers.Ticket) knowledge.QuotaTicket {
	return knowledge.QuotaTicket{
		ReservationID: t.ReservationID,
		CallerID:      t.CallerID,
		PeriodStart:   t.PeriodStart,
		NoReservation: t.NoReservation,
	}
}

// callerQuotaTicket 是 knowledgeQuotaTicket 的逆投影。
// callerQuotaTicket restores the implementation ticket from the domain ticket.
func callerQuotaTicket(t knowledge.QuotaTicket) callers.Ticket {
	return callers.Ticket{
		ReservationID: t.ReservationID,
		CallerID:      t.CallerID,
		PeriodStart:   t.PeriodStart,
		NoReservation: t.NoReservation,
	}
}

// knowledgeQuotaError 翻译配额错误：领域哨兵与实现哨兵**同时**可被 `errors.Is`
// 命中（双 `%w`），其余错误原样上抛（失败细节留给日志）。
// knowledgeQuotaError wraps domain and implementation sentinels together so both
// spellings match; every other error passes through unchanged.
func knowledgeQuotaError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, callers.ErrQuotaExhausted):
		return fmt.Errorf("%w: %w", knowledge.ErrQuotaExhausted, err)
	case errors.Is(err, callers.ErrRateLimited):
		return fmt.Errorf("%w: %w", knowledge.ErrRateLimited, err)
	case errors.Is(err, callers.ErrConcurrencyLimited):
		return fmt.Errorf("%w: %w", knowledge.ErrConcurrencyLimited, err)
	case errors.Is(err, callers.ErrUnauthorized):
		return fmt.Errorf("%w: %w", core.ErrUnauthorized, err)
	case errors.Is(err, callers.ErrForbidden):
		return fmt.Errorf("%w: %w", core.ErrForbidden, err)
	default:
		return err
	}
}

// knowledgeDiagAdapter 把检索侧窄事件翻成冻结契约事件写进诊断写入器。
// knowledgeDiagAdapter maps the retrieval-side event onto the frozen contract event.
type knowledgeDiagAdapter struct {
	recorder *diagnostics.Recorder
	logger   *slog.Logger
}

// newKnowledgeDiagAdapter 构造诊断适配器；写入器缺装配时仍返回适配器，
// `Recorder.Record` 自己对 nil 写入器空操作（诊断可丢弃，不阻断检索）。
// newKnowledgeDiagAdapter tolerates a nil recorder; the recorder itself no-ops.
func newKnowledgeDiagAdapter(recorder *diagnostics.Recorder, logger *slog.Logger) knowledge.DiagnosticRecorder {
	if logger == nil {
		logger = slog.Default()
	}
	return &knowledgeDiagAdapter{recorder: recorder, logger: logger}
}

// Record 翻译并写出一条检索诊断事件：事件种类固定 `retrieval`，关联只带 Run 主锚点；
// 未知字面量（编程错误）只 WARN 并丢弃，**不得**把未知值写进契约列。
// Record maps one event; an unknown literal is a programming error that is logged and
// dropped rather than written into a frozen column.
func (a *knowledgeDiagAdapter) Record(ctx context.Context, event knowledge.DiagnosticEvent) {
	reason, ok := knowledgeReasonCode(event.Reason)
	if !ok {
		a.drop(ctx, event, "unknown reason code")
		return
	}
	decision, ok := knowledgeDecision(event.Decision)
	if !ok {
		a.drop(ctx, event, "unknown decision")
		return
	}
	var occurredAt int64
	if !event.OccurredAt.IsZero() {
		occurredAt = event.OccurredAt.UTC().UnixMilli()
	}
	a.recorder.Record(ctx, diagnostics.DiagnosticEvent{
		Kind:        diagnostics.KindRetrieval,
		OccurredAt:  occurredAt,
		Correlation: diagnostics.Correlation{RunID: event.RunID},
		Actor:       diagnostics.Actor(event.Actor),
		StepID:      event.StepID,
		ReasonCode:  reason,
		Decision:    decision,
		Attrs:       event.Attrs,
	})
}

// drop 记录事件被丢弃的原因（诊断可丢弃，但丢弃本身要看得见）。
// drop logs why the event was discarded; dropping is visible, never silent.
func (a *knowledgeDiagAdapter) drop(ctx context.Context, event knowledge.DiagnosticEvent, why string) {
	a.logger.WarnContext(ctx, "knowledge: diagnostic event dropped",
		"why", why, "reason", string(event.Reason), "decision", string(event.Decision),
		"run_id", event.RunID, "step_id", event.StepID)
}

// knowledgeReasonCode 逐值翻译原因码；未命中返回 false（调用点丢弃事件）。
// knowledgeReasonCode translates the reason code value by value.
func knowledgeReasonCode(reason knowledge.DiagnosticReason) (diagnostics.ReasonCode, bool) {
	switch reason {
	case knowledge.DiagnosticReasonVectorUnavailable:
		return diagnostics.ReasonVectorUnavailable, true
	case knowledge.DiagnosticReasonKeywordUnavailable:
		return diagnostics.ReasonKeywordUnavailable, true
	case knowledge.DiagnosticReasonPermissionDenied:
		return diagnostics.ReasonPermissionDenied, true
	case knowledge.DiagnosticReasonBudgetTrimmed:
		return diagnostics.ReasonBudgetTrimmed, true
	case knowledge.DiagnosticReasonCitationUnresolved:
		return diagnostics.ReasonCitationUnresolved, true
	// 摄取配额判定的三条原因（V-8）：检索路径不会产生它们，翻译表共用一份是为了
	// 让同一个领域字面量在两条路径上得到同一个契约码。
	// The three ingest quota reasons live here so one literal maps to one code.
	case knowledge.DiagnosticReasonQuotaExhausted:
		return diagnostics.ReasonQuotaExhausted, true
	case knowledge.DiagnosticReasonRateLimited:
		return diagnostics.ReasonRateLimited, true
	case knowledge.DiagnosticReasonConcurrencyLimited:
		return diagnostics.ReasonConcurrencyLimited, true
	default:
		return "", false
	}
}

// knowledgeDecision 逐值翻译判定；未命中返回 false（调用点丢弃事件）。
// knowledgeDecision translates the decision value by value.
func knowledgeDecision(decision knowledge.DiagnosticDecision) (diagnostics.Decision, bool) {
	switch decision {
	// 空串是契约允许的"未给出判定"（`Decision.Valid` 明确接受），不是未知字面量：
	// 引用复核这类**结论性**事件没有 allow / deny 可填，如实留空。
	// The empty decision is the contract's "no decision", not an unknown literal.
	case "":
		return "", true
	case knowledge.DiagnosticDecisionAllow:
		return diagnostics.DecisionAllow, true
	case knowledge.DiagnosticDecisionDeny:
		return diagnostics.DecisionDeny, true
	case knowledge.DiagnosticDecisionTrim:
		return diagnostics.DecisionTrim, true
	case knowledge.DiagnosticDecisionDegrade:
		return diagnostics.DecisionDegrade, true
	default:
		return "", false
	}
}

// knowledgeIngestQuotaAdapter 把摄取配额判定翻成 `quota_decision` 事件（V-8）。
// 与检索适配器分开的两个理由：
//   - **锚点不同**：摄取作业用 `ingest_job_id`（契约 §2.9 关联表），检索事件用
//     `run_id`——混在一个适配器里会让锚点规则分叉；
//   - **判定集不同**：配额判定有 `allow` 态，检索事件没有。
//
// knowledgeIngestQuotaAdapter maps ingest quota verdicts onto quota_decision events;
// it is separate because the anchor and the decision set both differ from retrieval.
type knowledgeIngestQuotaAdapter struct {
	recorder *diagnostics.Recorder
	logger   *slog.Logger
}

// newKnowledgeIngestQuotaAdapter 构造适配器；写入器缺装配时返回 **nil 接口**——
// 摄取端口按"可空"设计（诊断可丢弃），返回 typed nil 会让非空判断失效。
// newKnowledgeIngestQuotaAdapter returns a nil interface when diagnostics are off.
func newKnowledgeIngestQuotaAdapter(
	recorder *diagnostics.Recorder, logger *slog.Logger,
) knowledge.IngestQuotaDiagnosticRecorder {
	if recorder == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &knowledgeIngestQuotaAdapter{recorder: recorder, logger: logger}
}

// RecordIngestQuota 翻译并写出一条配额判定事件：种类固定 `quota_decision`，关联只带
// `ingest_job_id`，`class` 取自领域侧（摄取固定 `ingest`，与实现侧 `RequestClass`
// 同字面量，便于与配额账本对账）。空原因码是合法的放行态，不是未知字面量。
// RecordIngestQuota maps one verdict; an empty reason is the legal allow state.
func (a *knowledgeIngestQuotaAdapter) RecordIngestQuota(
	ctx context.Context, event knowledge.IngestQuotaDiagnostic,
) {
	if event.IngestJobID == "" {
		return
	}
	decision, ok := knowledgeDecision(event.Decision)
	if !ok {
		a.dropIngestQuota(ctx, event, "unknown decision")
		return
	}
	var reason diagnostics.ReasonCode
	if event.Reason != "" {
		translated, ok := knowledgeReasonCode(event.Reason)
		if !ok {
			a.dropIngestQuota(ctx, event, "unknown reason code")
			return
		}
		reason = translated
	}
	a.recorder.Record(ctx, diagnostics.DiagnosticEvent{
		Kind:        diagnostics.KindQuotaDecision,
		Correlation: diagnostics.Correlation{IngestJobID: event.IngestJobID},
		StepID:      event.StepID,
		Seq:         event.Seq,
		Decision:    decision,
		ReasonCode:  reason,
		Attrs:       map[string]any{"class": event.Class},
	})
}

// dropIngestQuota 记录事件被丢弃的原因（诊断可丢弃，但丢弃本身要看得见）。
// dropIngestQuota logs why one verdict was discarded; dropping is never silent.
func (a *knowledgeIngestQuotaAdapter) dropIngestQuota(
	ctx context.Context, event knowledge.IngestQuotaDiagnostic, why string,
) {
	a.logger.WarnContext(ctx, "knowledge: ingest quota diagnostic dropped",
		"why", why, "reason", string(event.Reason), "decision", string(event.Decision),
		"ingest_job_id", event.IngestJobID, "step_id", event.StepID)
}

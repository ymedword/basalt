package knowledge

// 本文件交付知识库侧的操作审计（US-16 FR-007 / T036a）：摄取、重建、删除与检索
// 的每次操作都要回答四个问题——谁（who）、何时（when）、对哪个来源（which
// source）、结果如何（what result）。检索侧的调用点由 T038 接入同一套类型，
// 本文件先交付共用模块。
//
// 为什么权威行不落在 `management_operations`：该表的 action 取值是冻结契约，只
// 登记调用方生命周期与实例密钥两类；新增 `knowledge.*` 属于对外概念变更（软门禁）。
// 知识库操作因此以**各自的业务行**为权威记录：
//   - 摄取 / 重建的终态 → `kb_ingest_jobs`（带 stage / status / error_kind）；
//   - 去重命中 → 命中的 `kb_document_versions` 行（本次没有开作业）；
//   - 删除 → 软删后的 `kb_documents` 或 `kb_collections` 行（软删本身就是留痕）。
//
// 审计模块负责把「谁」与「来源」补进日志，并带出权威行引用（`audit_table` +
// `audit_id`），供诊断事件与排障对号。**已知边界**（tasks.md 附录 R46）：冻结
// schema 的知识库表没有调用方列，内置管理员走无限量配额时连
// `quota_reservations` 都不落行——这类操作的「谁」只在日志里，不在库里；有配额
// 预留时可用 `quota_reservations.caller_id` 交叉核对。
//
// 双轨：失败路径**同时**写权威行终态与 WARN 日志；权威行终态写失败升级为 ERROR
// （工程规范 §4：需要人介入）。准入期拒绝没有权威行可写（零写入是冻结契约的要
// 求），只落 WARN——这是零写入的直接推论，不是遗漏。
//
// Every knowledge operation records who, when, which source and what result. The
// frozen contracts give `management_operations` no knowledge.* action, so each
// operation's own row is the authoritative record; this module adds the actor and
// the source to the log and exposes the row reference. Admission rejections write
// nothing by contract, so they are log-only (see appendix R46 for the remaining
// "who lives only in the log" gap).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/ymedword/basalt/internal/core"
)

// operationKind 是审计里的操作名：与 REST 面向的操作一一对应，**不得**在调用点
// 现拼字符串（取值少一个就是日志聚合里的孤儿）。
// operationKind is the closed set of audited operations.
type operationKind string

const (
	// opIngest 是一次新摄取。
	opIngest operationKind = "ingest"
	// opReindex 是按既有来源重建索引。
	opReindex operationKind = "reindex"
	// opDelete 是软删文档或集合。
	opDelete operationKind = "delete"
	// opRetrieve 是进提示词的检索（T038 接入）。
	opRetrieve operationKind = "retrieve"
	// opSearch 是管理面的检索查询（T038 接入）。
	opSearch operationKind = "search"
	// opSourceView 是来源原文视图（T040 接入）。
	opSourceView operationKind = "source_view"
)

// valid 判定操作名是否已登记；未登记的名称一律拒绝，不落半结构化日志。
// valid reports whether the kind is registered.
func (k operationKind) valid() bool {
	switch k {
	case opIngest, opReindex, opDelete, opRetrieve, opSearch, opSourceView:
		return true
	}
	return false
}

// auditRef 指向一条**权威行**：操作结果真正落库的位置。与诊断契约的
// `audit_ref`（`table` + `id`）同形，`diagnostic_events.audit_ref` 可原样引用。
// auditRef mirrors the diagnostic contract's `audit_ref` shape.
type auditRef struct {
	table string
	id    string
}

// complete 判定引用是否可用；不完整时日志只告警、不编造行号。
// complete reports whether the reference names a row.
func (r auditRef) complete() bool { return r.table != "" && r.id != "" }

// operationTargets 是操作针对的来源对象。字段为空即"本次没有这一层"，日志里整键
// 省略——**不得**用空串冒充"没有调用方 / 没有文档"。
// operationTargets records the objects an operation touched; empty fields are omitted.
type operationTargets struct {
	collectionID string
	documentID   string
	versionID    string
	sourcePath   string
	jobID        string
	// runID 是检索操作的 Run 锚点（T038）：摄取操作没有这一层，日志里整键省略。
	// runID anchors retrieval operations on a run; ingest operations omit the key.
	runID string
}

// merge 合并后续阶段才拿到的标识：已有值优先，空值不覆盖。
// merge fills in identifiers discovered later; empty values never overwrite.
func (t *operationTargets) merge(next operationTargets) {
	if next.collectionID != "" {
		t.collectionID = next.collectionID
	}
	if next.documentID != "" {
		t.documentID = next.documentID
	}
	if next.versionID != "" {
		t.versionID = next.versionID
	}
	if next.sourcePath != "" {
		t.sourcePath = next.sourcePath
	}
	if next.jobID != "" {
		t.jobID = next.jobID
	}
	if next.runID != "" {
		t.runID = next.runID
	}
}

// errAuditUnavailable 表示审计模块没有装配（零值 `Ingestor`）。正常装配路径在
// `ready()` 之后不可能出现；出现即拒绝，**不得**在没有留痕的情况下继续。
// errAuditUnavailable reports a missing audit module; the request is rejected.
var errAuditUnavailable = errors.New("knowledge: operation audit is not configured")

// operationAudit 是审计模块的装配面：一处日志、一处时钟。`*Ingestor` 在构造期
// 建一次，检索侧（T038）用同一构造函数，**不得**各自再拼一套字段。
// operationAudit carries the log and clock shared by every knowledge operation.
type operationAudit struct {
	logger *slog.Logger
	now    func() time.Time
}

// newOperationAudit 装配审计模块；nil 值时回落 `slog.Default()` / `time.Now`
// （与 `Ingestor` 的缺省口径一致）。零值 `*operationAudit` 不可用：所有方法按
// "没有日志面"处理，不往全局默认日志里写。
// newOperationAudit wires the audit module; the zero value is not usable.
func newOperationAudit(logger *slog.Logger, now func() time.Time) *operationAudit {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &operationAudit{logger: logger, now: now}
}

// operationTrace 是一次操作的过程记录：操作名、身份、起点与逐步补齐的目标。
// 由 `begin` 创建，`succeed` / `fail` / `reject` 恰好调用一次收口。
// operationTrace is one operation's in-flight record; exactly one terminal call.
type operationTrace struct {
	audit   *operationAudit
	kind    operationKind
	who     core.Principal
	started time.Time
	targets operationTargets
}

// begin 开一条操作轨迹：先校验操作名与身份，**再**放行任何业务动作。零值身份在
// 这里就拒绝（返回 `core.ErrUnauthorized`），因此它的后续日志不会出现调用方字段。
// begin validates the operation name and the principal before any business step.
func (a *operationAudit) begin(p core.Principal, kind operationKind, started time.Time) (*operationTrace, error) {
	if a == nil {
		return nil, errAuditUnavailable
	}
	if !kind.valid() {
		return nil, fmt.Errorf("knowledge: 未登记的审计操作 %q", string(kind))
	}
	if !p.Valid() {
		return nil, core.ErrUnauthorized
	}
	return &operationTrace{audit: a, kind: kind, who: p, started: started}, nil
}

// rejected 记录**还没有轨迹**的准入期拒绝：装配错误与身份非法发生在 `begin`
// 之前或之内，此时只有日志可写（零写入是契约要求）。
// rejected logs an admission rejection that never reached a trace.
func (a *operationAudit) rejected(ctx context.Context, kind operationKind, p core.Principal, cause error, targets operationTargets) {
	a.rejectedSince(ctx, kind, p, cause, targets, a.nowOr())
}

// rejectedSince 是 `rejected` 的实现面：调用方可以传入更早的起点（`begin` 之后
// 才被拒的登录用户在轨迹里已经带起点）。
func (a *operationAudit) rejectedSince(ctx context.Context, kind operationKind, p core.Principal, cause error, targets operationTargets, started time.Time) {
	if a == nil {
		return
	}
	trace := &operationTrace{audit: a, kind: kind, who: p, started: started, targets: targets}
	outcome := operationOutcome{result: "rejected", reason: reasonCode(cause), cause: cause}
	if errors.Is(cause, ErrIngestUnavailable) || errors.Is(cause, ErrRetrieverUnavailable) ||
		errors.Is(cause, errSourceViewUnavailable) {
		// 装配 / 配置错误要人介入（工程规范 §4 的 ERROR 口径），不是调用方问题。
		a.logger.ErrorContext(ctx, "knowledge operation unavailable", trace.attrs(outcome)...)
		return
	}
	a.logger.WarnContext(ctx, "knowledge operation rejected", trace.attrs(outcome)...)
}

// scope 合并后续阶段才知道的目标标识（集合 → 作业 → 文档 / 版本 → 来源路径）。
// scope records identifiers found later in the pipeline.
func (t *operationTrace) scope(targets operationTargets) {
	if t == nil {
		return
	}
	t.targets.merge(targets)
}

// reject 记录准入期拒绝（身份越权、请求形状非法、路径被拒、配额不足）：动作没有
// 发出，因此没有权威行，只有 WARN。**不得**在拒绝后继续执行。
// reject logs an admission rejection; nothing was written, so there is no row.
func (t *operationTrace) reject(ctx context.Context, cause error) {
	if t == nil {
		return
	}
	t.audit.rejectedSince(ctx, t.kind, t.who, cause, t.targets, t.started)
}

// succeed 写成功终态：先落权威行（`persist`，可为 nil），再记 INFO。引用不完整
// 时降级为 WARN——成功却没有可对号的行，是排障时找不到证据。
// succeed writes the authoritative terminal state first, then the log line.
func (t *operationTrace) succeed(ctx context.Context, ref auditRef, persist func(context.Context) error) {
	if t == nil {
		return
	}
	persistErr := persistTerminal(ctx, persist)
	outcome := operationOutcome{result: "succeeded", ref: ref}
	if ref.complete() {
		t.audit.logger.InfoContext(ctx, "knowledge operation succeeded", t.attrs(outcome)...)
	} else {
		t.audit.logger.WarnContext(ctx, "knowledge operation succeeded without an audit record", t.attrs(outcome)...)
	}
	if persistErr != nil {
		t.audit.logger.ErrorContext(ctx, "knowledge operation terminal write failed",
			t.attrs(operationOutcome{
				result: "succeeded", reason: reasonCode(persistErr), cause: persistErr, ref: ref,
			})...)
	}
}

// succeedRead 记**只读**操作的成功终态：读路径没有本组件写入的权威行（装配记录的
// 标识由写入器分配），因此不套用 `succeed` 的"引用不完整即 WARN"规则——每次检索都
// 告警等于把告警调成没人看。检索的审计面由装配记录（`Retrieve`）与诊断事件两条
// 独立轨道承担，本行只保证日志侧有成功记录。
// succeedRead logs a read-only success: this component writes no authoritative row on
// the read path, so the missing-reference warning of succeed would fire on every query.
func (t *operationTrace) succeedRead(ctx context.Context) {
	if t == nil {
		return
	}
	t.audit.logger.InfoContext(ctx, "knowledge operation succeeded",
		t.attrs(operationOutcome{result: "succeeded"})...)
}

// fail 写失败终态：**先**落 WARN 日志（双轨的另一轨），再写权威行。业务语义
// 不因审计写失败而反转——失败已经发生，这里只保证留痕与告警。
// fail logs the failure first, then writes the authoritative terminal state.
func (t *operationTrace) fail(ctx context.Context, ref auditRef, stage, errorKind string,
	cause error, persist func(context.Context) error,
) {
	if t == nil {
		return
	}
	outcome := operationOutcome{
		result: "failed", stage: stage, errorKind: errorKind,
		reason: reasonCode(cause), cause: cause, ref: ref,
	}
	t.audit.logger.WarnContext(ctx, "knowledge operation failed", t.attrs(outcome)...)
	if err := persistTerminal(ctx, persist); err != nil {
		t.audit.logger.ErrorContext(ctx, "knowledge operation terminal write failed",
			t.attrs(operationOutcome{
				result: "failed", stage: stage, errorKind: errorKind,
				reason: reasonCode(err), cause: err, ref: ref,
			})...)
	}
}

// operationOutcome 是一次操作的终态描述；空字段在日志里整键省略。
// operationOutcome describes a terminal state; empty fields are omitted.
type operationOutcome struct {
	result    string
	stage     string
	errorKind string
	reason    string
	cause     error
	ref       auditRef
}

// attrs 按固定键名组装日志字段（工程规范 §4：键名固定、错误进字段、文案不拼错误）。
// 身份字段只在 `Principal.Valid()` 为真时出现——零值身份不产生假身份。
// attrs builds the fixed-key log fields; identity fields appear only for a valid
// principal, so a rejected zero value never looks like a real caller.
func (t *operationTrace) attrs(o operationOutcome) []any {
	attrs := []any{"operation", string(t.kind), "result", o.result}
	if t.who.Valid() {
		attrs = append(attrs, "principal_kind", string(t.who.Kind))
		attrs = append(attrs, fieldIfSet("caller_id", t.who.CallerID)...)
		attrs = append(attrs, fieldIfSet("key_id", t.who.KeyID)...)
		attrs = append(attrs, fieldIfSet("actor", string(t.who.Actor))...)
		attrs = append(attrs, fieldIfSet("project", t.who.Project)...)
	}
	attrs = append(attrs, fieldIfSet("collection_id", t.targets.collectionID)...)
	attrs = append(attrs, fieldIfSet("document_id", t.targets.documentID)...)
	attrs = append(attrs, fieldIfSet("version_id", t.targets.versionID)...)
	attrs = append(attrs, fieldIfSet("ingest_job_id", t.targets.jobID)...)
	attrs = append(attrs, fieldIfSet("source_path", t.targets.sourcePath)...)
	attrs = append(attrs, fieldIfSet("run_id", t.targets.runID)...)
	attrs = append(attrs, fieldIfSet("stage", o.stage)...)
	attrs = append(attrs, fieldIfSet("error_kind", o.errorKind)...)
	attrs = append(attrs, fieldIfSet("reason_code", o.reason)...)
	if o.cause != nil {
		attrs = append(attrs, "error", o.cause)
	}
	if o.ref.complete() {
		attrs = append(attrs, "audit_table", o.ref.table, "audit_id", sanitizeField(o.ref.id))
	}
	attrs = append(attrs, "duration_ms", t.audit.now().Sub(t.started).Milliseconds())
	return attrs
}

// nowOr 在零值审计器上回落 `time.Now`：拒绝路径可能早于装配检查，仍要能算出耗时。
// nowOr falls back to the wall clock so a rejection can still report a duration.
func (a *operationAudit) nowOr() time.Time {
	if a == nil || a.now == nil {
		return time.Now()
	}
	return a.now()
}

// fieldIfSet 生成一对日志字段；空值返回 nil，调用方用 `...` 展开即整键省略。
// fieldIfSet yields one key/value pair, or nothing for an empty value.
func fieldIfSet(key, value string) []any {
	if value == "" {
		return nil
	}
	return []any{key, sanitizeField(value)}
}

// sanitizeField 去掉控制字符并截断超长值：字段值可能直接来自请求体（路径、集合
// 标识），换行进日志会伪造日志行（工程规范 §4 防注入）；截断保证单条日志有界。
// sanitizeField strips control characters and caps the length: request-supplied
// values must not forge log lines, and one record must stay bounded.
func sanitizeField(value string) string {
	const maxRunes = 256
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	runes := []rune(cleaned)
	if len(runes) <= maxRunes {
		return cleaned
	}
	return string(runes[:maxRunes]) + "..."
}

// detachedWriteTimeout 是脱取消终态写的独立超时（`run-and-errors.md` §4.2）：终态写
// 不得随请求取消而丢，但也不得无限期挂住——数据库连接池卡死时，`WithoutCancel` 会让
// 一次已经发生的操作永久停在半截状态。
// detachedWriteTimeout bounds every cancellation-detached terminal write.
const detachedWriteTimeout = 5 * time.Second

// persistTerminal 在**脱离取消**的上下文里写权威行终态：请求取消不得让操作停在
// 半截状态（与 `credential_ops` 的终态回填同一口径）；同时套独立短超时，脱了取消
// 的写也不会无限期等待。
// persistTerminal writes the terminal state with cancellation detached and bounded.
func persistTerminal(ctx context.Context, persist func(context.Context) error) error {
	if persist == nil {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedWriteTimeout)
	defer cancel()
	return persist(writeCtx)
}

// reasonCode 把错误映射成稳定的 `reason_code`：日志聚合与排障按它分组，**不得**
// 靠错误文本匹配（文案会改，码不会）。未知错误统一 `operation_failed`。配额三类
// 哨兵由本包声明、经适配器从实现侧翻译而来（`quota.go` / C-06）。
// reasonCode maps errors onto a stable code for aggregation; never match on text.
// The three quota sentinels are declared here and translated by the app adapter.
func reasonCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, core.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, core.ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrQuotaExhausted):
		return "quota_exhausted"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrConcurrencyLimited):
		return "concurrency_limited"
	case errors.Is(err, ErrIngestUnavailable), errors.Is(err, ErrRetrieverUnavailable),
		errors.Is(err, errSourceViewUnavailable):
		return "unavailable"
	case errors.Is(err, ErrPathRejected):
		return "path_rejected"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	default:
		return "operation_failed"
	}
}

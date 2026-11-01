package callers

// 本文件实现调用方与配额的**管理面**编排（US-16 §3.1.5 / §3.1.7 / T026）：建调用方、
// 列调用方、签发 / 吊销 Key、查周期用量、核对未知结局的预留、清零 overage。
// 三条规则贯穿全文件：
//
//  1. **先校验、后留痕、再执行**——参数与前置条件不满足时在写任何行之前拒绝，
//     **不**留 `pending` 操作行；预写失败**不得**执行变更（宪法 V）。
//  2. **归属显式**——核对与清零都是跨调用方的管理动作，先按
//     `(caller_id, reservation_id)` 校验归属，**不**只凭预留标识记账。
//  3. **不虚构数字**——没有限额行就报零限额，用量按 (账本, 类别) 如实投影；
//     `overage` 只挂在账本的代表行上，各行相加等于账本总值。
//
// This file is the admin-side orchestration for callers and quotas: validate before
// writing the pending operation, check ownership explicitly, and never invent numbers.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// ErrInvalidRequest 表示管理面请求本身不合法（名称缺失、标识格式不对、结局与
// 实际用量矛盾等）。web 层映射 400；它与存储层错误区分开，**不**依赖错误文案。
// ErrInvalidRequest marks a malformed admin request; the web layer maps it to 400.
var ErrInvalidRequest = errors.New("callers: invalid request")

// InvalidRequestError 承载请求不合法的**可读原因**：`Reason` 是本包自产的中文
// 文案（不含路径、凭据与库原文），web 层原样放进 400 的 `message`；`Unwrap`
// 保留 `ErrInvalidRequest` 的哨兵身份，审计 `result_code` 与既有 `errors.Is`
// 判定都不变。它只解决"把原因说清楚"，**不**新增错误类别。
// InvalidRequestError carries the readable, self-authored reason for a malformed
// request; Unwrap keeps the ErrInvalidRequest identity used by audit projection.
type InvalidRequestError struct {
	// Reason 是给调用方看的完整中文原因。
	Reason string
}

// Error 返回可读原因（不带包名前缀——它直接进公开响应）。
// Error returns the reason verbatim; it is shown to the caller as-is.
func (e *InvalidRequestError) Error() string { return e.Reason }

// Unwrap 让 `errors.Is(err, ErrInvalidRequest)` 继续成立。
// Unwrap keeps the sentinel identity.
func (e *InvalidRequestError) Unwrap() error { return ErrInvalidRequest }

// invalidRequest 构造一条带原因的管理面请求拒绝。
// invalidRequest builds a typed rejection with a self-authored reason.
func invalidRequest(format string, args ...any) error {
	return &InvalidRequestError{Reason: fmt.Sprintf(format, args...)}
}

// errCallerAdminUnconfigured 是装配错误：缺少任一存储端口时拒绝服务，**不得**放行。
// errCallerAdminUnconfigured fails closed when a port is missing.
var errCallerAdminUnconfigured = errors.New("callers: admin service is not configured")

// 管理操作字面量（`docs/contracts/run-and-errors.md` §9.3 逐字）。
// clear-overage 没有独立字面量：它以 `caller.reservation.reconcile` 记账，并在
// detail 里用 `scope=overage` 与预留级核对区分（该缺口记入 run-state R34）。
// Admin action literals copied verbatim from the contract; clear-overage reuses the
// reconcile action with an explicit scope field.
const (
	actionCallerCreate               = "caller.create"
	actionCallerReservationReconcile = "caller.reservation.reconcile"
	resultConflict                   = "CONFLICT"
	resultInvalidRequest             = "INVALID_REQUEST"
	overageScope                     = "overage"
)

// CallerKind 是调用方种类，逐字对齐 openapi `CallerSummary.kind` 的两值。
// 存储层把 `callers.kind` 的 `builtin|service` 投影成这对字面量：`service` 与
// `external` 是同一概念的两套词表，映射只发生在存储边界，**不**泄漏到领域类型。
// CallerKind mirrors CallerSummary.kind; the storage layer maps the DDL's service
// literal onto external at the boundary.
type CallerKind string

const (
	// CallerKindBuiltin 是内置调用方（`local-admin`）。
	// CallerKindBuiltin is the built-in caller.
	CallerKindBuiltin CallerKind = "builtin"
	// CallerKindExternal 是外部调用方。
	// CallerKindExternal is an external caller.
	CallerKindExternal CallerKind = "external"
)

// Valid 判定种类是否在闭集内。
// Valid accepts exactly the two literals.
func (k CallerKind) Valid() bool { return k == CallerKindBuiltin || k == CallerKindExternal }

// CallerStatus 是调用方状态，逐字对齐 `callers.status` 的 CHECK 两值。
// CallerStatus mirrors the two frozen callers.status literals.
type CallerStatus string

const (
	// CallerStatusActive 表示调用方可用。
	// CallerStatusActive means the caller is usable.
	CallerStatusActive CallerStatus = "active"
	// CallerStatusDisabled 表示调用方已停用（其 Key 一律认证失败）。
	// CallerStatusDisabled disables every key of the caller.
	CallerStatusDisabled CallerStatus = "disabled"
)

// Valid 判定状态是否在闭集内。
// Valid accepts exactly the two literals.
func (s CallerStatus) Valid() bool { return s == CallerStatusActive || s == CallerStatusDisabled }

// 账本名称（`caller_usage_periods.kind` 的三值，逐字）。领域与存储共用同一组
// 字面量，避免两处漂移后投影悄悄错位。
// Ledger names shared by the domain and the storage layer.
const (
	// LedgerTasks 是任务次数账本。
	LedgerTasks = "tasks"
	// LedgerTokens 是 Token 账本。
	LedgerTokens = "tokens"
	// LedgerIngestBytes 是摄取字节账本。
	LedgerIngestBytes = "ingest_bytes"
)

// 用量行的对外类别（openapi `UsageLine.kind` 的七值中实际可达的六个）。
// `ingest` 不是账本维度（账本只有 tokens / ingest_bytes），本服务**不**输出它——
// 报一个恒为 0 的类别等于虚构指标。
// The reachable UsageLine kinds; `ingest` is deliberately not emitted because no
// ledger books an "ingest count".
const (
	UsageKindRun             = "run"
	UsageKindChat            = "chat"
	UsageKindCompaction      = "compaction"
	UsageKindRerank          = "rerank"
	UsageKindEmbeddingTokens = "embedding_tokens"
	UsageKindIngestBytes     = "ingest_bytes"
)

// usageLineOrder 是 UsageLine 的固定输出顺序：稳定的顺序让管理台与测试都不依赖
// map 遍历顺序（Go 的 map 序遍历是随机的）。
// usageLineOrder fixes the output order so consumers never depend on map iteration.
var usageLineOrder = []string{
	UsageKindRun,
	UsageKindChat,
	UsageKindCompaction,
	UsageKindRerank,
	UsageKindEmbeddingTokens,
	UsageKindIngestBytes,
}

// CallerRecord 是 `callers` 行的管理面投影，字段对齐 openapi `CallerSummary`。
// CallerRecord is the admin projection of a callers row.
type CallerRecord struct {
	// CallerID 是调用方标识（`cal_` + 26 位 Crockford；内置管理员为 `local-admin`）。
	CallerID string
	// Kind 是调用方种类。
	Kind CallerKind
	// Name 是展示名。
	Name string
	// Project 是所属项目（空串表示未分组）。
	Project string
	// Status 是调用方状态。
	Status CallerStatus
	// CreatedAt 是创建时刻。
	CreatedAt time.Time
	// KeyCount 是未吊销的 Key 数量（`List` 填充；单条查询可不填）。
	KeyCount int
}

// LedgerUsage 是某本账在一个周期上的行投影（`caller_usage_periods` 一行）。
// LedgerUsage is one ledger row of a period.
type LedgerUsage struct {
	// Kind 是账本名。
	Kind string
	// Used 是该周期已结算的实际用量。
	Used int64
	// Reserved 是该周期仍占用的预留量。
	Reserved int64
	// Overage 是该周期不可回滚的超限。
	Overage int64
}

// ClassUsage 是按消耗类别拆开的用量（`quota_reservations` 按 class + 账本聚合）。
// ClassUsage splits one ledger's usage by request class.
type ClassUsage struct {
	// Class 是消耗类别。
	Class RequestClass
	// Ledger 是这次聚合所属的账本（由预留标识前缀解析，零 schema 变更）。
	Ledger string
	// Used 是该类别已结算的实际用量。
	Used int64
	// Reserved 是该类别仍 pending 的预留量。
	Reserved int64
}

// UsageAggregate 是存储层对一个周期的**原始**聚合结果；`CallerAdmin.Usage` 把它
// 投影成契约视图。限额行缺失时 `Limit` 为零值但 `Period` / `Timezone` 已按缺省
// （day / UTC）补齐——零限额表示"没有配置"，**不是**不限量。
// UsageAggregate is the raw storage aggregation; a missing limit row yields zero
// limits with the documented day/UTC period defaults.
type UsageAggregate struct {
	// CallerID 是归属调用方。
	CallerID string
	// PeriodStart 是该周期起点（Unix 毫秒 UTC，已按调用方时区折算）。
	PeriodStart int64
	// Limit 是该调用方的限额（已 Normalize；无行时为零值 + 默认周期 / 时区）。
	Limit Limit
	// Ledger 是三本账中存在的行（可能为空）。
	Ledger []LedgerUsage
	// Classes 是按类别拆开的已结算 / 预留量（可能为空）。
	Classes []ClassUsage
	// PendingReservations 是该调用方**跨周期**的 pending 预留条数。
	PendingReservations int
}

// UsageLine 是 openapi `UsageLine` 的行视图：`used` / `reserved` / `overage`
// 三项均必填，零值也照实输出。
// UsageLine is the contract's usage row; zeros are reported as zeros.
type UsageLine struct {
	// Kind 是类别字面量。
	Kind string
	// Used 是该类别已结算的实际用量。
	Used int64
	// Reserved 是该类别仍占用的预留量。
	Reserved int64
	// Overage 是该类别的超限（只挂在账本的代表行上）。
	Overage int64
}

// CallerUsage 是 `GET /callers/{id}/usage` 的返回视图（openapi `CallerUsageView`）。
// CallerUsage is the usage view returned to the admin console.
type CallerUsage struct {
	// CallerID 是归属调用方。
	CallerID string
	// PeriodStart 是周期起点。
	PeriodStart time.Time
	// PeriodEnd 是下一周期起点（左闭右开）。
	PeriodEnd time.Time
	// Timezone 是周期起算时区。
	Timezone string
	// Limit 是生效限额；零值表示没有配置限额行。
	Limit Limit
	// Lines 是固定顺序的六个类别行。
	Lines []UsageLine
	// PendingReservations 是跨周期的 pending 预留条数。
	PendingReservations int
}

// CreateCallerRequest 是 `POST /callers` 的请求投影。`CallerID` 为空时由服务端
// 生成；给了就必须是 `cal_` 形态。`Limit` 为 nil 表示**不写**限额行。
// CreateCallerRequest carries an optional client-chosen caller id and an optional
// limit; a nil limit leaves the row missing instead of filling defaults.
type CreateCallerRequest struct {
	// CallerID 是可选的自选标识（`cal_` + 26 位 Crockford）。
	CallerID string
	// Name 是展示名（必填）。
	Name string
	// Project 是所属项目（可空）。
	Project string
	// Limit 是可选限额；nil 表示不写 `caller_limits` 行。
	Limit *Limit
}

// ReconcileRequest 是一次预留核对请求。`Actual` 为 nil 表示未给出：`committed`
// 必须给出非负值，`released` 必须**不**给（给了就是与"零消耗"自相矛盾）。
// ReconcileRequest requires actual for committed and forbids it for released.
type ReconcileRequest struct {
	// CallerID 是归属调用方（归属校验的一侧）。
	CallerID string
	// ReservationID 是待核对的预留。
	ReservationID string
	// Outcome 是核对结局。
	Outcome ReconcileOutcome
	// Actual 是核对得到的实际用量；nil 表示未给出。
	Actual *int64
	// Reason 是必填的核对理由（进管理审计）。
	Reason string
}

// ClearOverageRequest 是一次清零 overage 的请求。`PeriodStart` 为零值表示当前
// 周期；非零值会被折算到所属周期的起点。
// ClearOverageRequest zeroes one period's overage; a zero instant means the current
// period.
type ClearOverageRequest struct {
	// CallerID 是归属调用方。
	CallerID string
	// PeriodStart 是目标周期内的任一瞬时（零值 = 当前周期）。
	PeriodStart time.Time
	// Reason 是必填理由（进管理审计）。
	Reason string
}

// CallerAdmin 是调用方 / Key / 配额的管理面服务。它自己不做认证与授权（web 层
// 已解析出 `core.Actor`），只负责「校验 → 留痕 → 执行 → 回填」与视图投影。
// CallerAdmin owns the admin orchestration; authentication and authorisation happen
// in the web layer before the actor reaches this type.
type CallerAdmin struct {
	keys   KeyStore
	quotas QuotaStore
	ops    core.OperationStore
	issuer *KeyIssuer
	logger *slog.Logger
	// now 用于操作标识、审计时间与用量周期折算；测试注入固定时钟。
	now func() time.Time
}

// NewCallerAdmin 构造管理服务。三个端口缺一即拒绝执行（fail-closed）——
// 没有审计就不做管理变更，没有配额存储就没法核对。
// NewCallerAdmin fails closed when any port is missing.
func NewCallerAdmin(keys KeyStore, quotas QuotaStore, ops core.OperationStore) *CallerAdmin {
	return &CallerAdmin{
		keys:   keys,
		quotas: quotas,
		ops:    ops,
		issuer: NewKeyIssuer(keys, ops),
		logger: slog.Default(),
		now:    time.Now,
	}
}

// CreateCaller 新建调用方：校验标识与限额 → 预写 pending → 写调用方行（与可选
// 限额行同事务）→ 回填。重名或主键冲突按 `core.ErrConflict` 返回（web 映射 409）。
// CreateCaller validates, pre-writes the pending operation and creates the caller
// with its optional limit row in one storage transaction.
func (a *CallerAdmin) CreateCaller(ctx context.Context, actor core.Actor, req CreateCallerRequest) (CallerRecord, error) {
	if err := a.ready(); err != nil {
		return CallerRecord{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return CallerRecord{}, invalidRequest("调用方名称不得为空")
	}
	callerID := req.CallerID
	if callerID == "" {
		generated, err := core.NewCallerID()
		if err != nil {
			return CallerRecord{}, err
		}
		callerID = generated
	} else {
		parsed, err := core.ParseCallerID(callerID)
		if err != nil {
			return CallerRecord{}, invalidRequest("%v", err)
		}
		callerID = parsed
	}
	var limit *Limit
	if req.Limit != nil {
		normalized, err := req.Limit.Normalize()
		if err != nil {
			return CallerRecord{}, invalidRequest("%v", err)
		}
		limit = &normalized
	}
	record := CallerRecord{
		CallerID:  callerID,
		Kind:      CallerKindExternal,
		Name:      name,
		Project:   strings.TrimSpace(req.Project),
		Status:    CallerStatusActive,
		CreatedAt: a.now().UTC(),
	}
	detail := callerCreateDetail{
		CallerID: record.CallerID,
		Name:     record.Name,
		Project:  record.Project,
		Limits:   newLimitDetail(limit),
	}
	operationID, err := a.writer().begin(ctx, actor, actionCallerCreate, record.CallerID,
		callerCreateRequestHash(record, limit), detail)
	if err != nil {
		return CallerRecord{}, err
	}
	if createErr := a.keys.CreateCaller(ctx, record, limit); createErr != nil {
		a.finishFailed(ctx, operationID, adminResultCode(createErr), createErr, detail)
		return CallerRecord{}, fmt.Errorf("创建调用方失败: %w", createErr)
	}
	a.writer().finish(ctx, operationID, core.OperationSuccess, resultOK, "", detail)

	// 回读让响应反映真正落库的行；回读失败**不**把创建报成失败（行已经在了），
	// 只记 ERROR 并用本次请求的可信投影作答。
	stored, lookupErr := a.keys.LookupCaller(ctx, record.CallerID)
	if lookupErr == nil {
		return stored, nil
	}
	a.logger.Error("调用方已创建但回读失败，响应使用请求投影",
		"caller_id", record.CallerID, "error", lookupErr)
	return record, nil
}

// ListCallers 返回调用方列表（`caller_id` 升序，内置管理员随建库种子行恒在）。
// 读路径不写管理操作行。
// ListCallers returns the callers without writing an operation row.
func (a *CallerAdmin) ListCallers(ctx context.Context) ([]CallerRecord, error) {
	if a == nil || a.keys == nil {
		return nil, errCallerAdminUnconfigured
	}
	return a.keys.List(ctx)
}

// IssueKey 为指定调用方签发 Key：先确认调用方存在（不存在即 `ErrNotFound`，
// 零写入、无操作行），再交给 `KeyIssuer`（scope 校验与审计都在那里）。
// IssueKey refuses an unknown caller before any row is written and delegates to the
// key issuer, which owns scope validation and the audit record.
func (a *CallerAdmin) IssueKey(ctx context.Context, actor core.Actor, req IssueRequest) (IssuedKey, error) {
	if err := a.ready(); err != nil {
		return IssuedKey{}, err
	}
	if req.CallerID == "" {
		return IssuedKey{}, fmt.Errorf("%w: 调用方标识为空", ErrNotFound)
	}
	if _, err := a.keys.LookupCaller(ctx, req.CallerID); err != nil {
		return IssuedKey{}, err
	}
	return a.issuer.Issue(ctx, actor, req)
}

// RevokeKey 吊销一把 Key 并回读终态行（契约 200 响应要带 `revoked_at`）。
// 重复吊销幂等；回读失败时返回最小投影并记 ERROR——吊销已经生效，不能报成失败。
// RevokeKey revokes and reads the row back so the response carries revoked_at.
func (a *CallerAdmin) RevokeKey(ctx context.Context, actor core.Actor, req RevokeRequest) (KeyRecord, error) {
	if err := a.ready(); err != nil {
		return KeyRecord{}, err
	}
	if err := a.issuer.Revoke(ctx, actor, req); err != nil {
		return KeyRecord{}, err
	}
	record, err := a.keys.LookupByID(ctx, req.CallerID, req.KeyID)
	if err != nil {
		a.logger.Error("调用方 Key 已吊销但回读失败，响应使用最小投影",
			"caller_id", req.CallerID, "key_id", req.KeyID, "error", err)
		return KeyRecord{KeyID: req.KeyID, CallerID: req.CallerID, Status: KeyStatusRevoked}, nil
	}
	return record, nil
}

// Usage 返回指定周期的用量视图；`periodStart` 为 0 表示当前周期。
// Usage returns one period's usage view; zero means the current period.
func (a *CallerAdmin) Usage(ctx context.Context, callerID string, periodStart int64) (CallerUsage, error) {
	if a == nil || a.keys == nil {
		return CallerUsage{}, errCallerAdminUnconfigured
	}
	if callerID == "" {
		return CallerUsage{}, fmt.Errorf("%w: 调用方标识为空", ErrNotFound)
	}
	aggregate, err := a.keys.Usage(ctx, callerID, periodStart, a.now())
	if err != nil {
		return CallerUsage{}, err
	}
	return projectUsage(aggregate)
}

// ReconcileReservation 核对一条结局未知的预留：先按归属读行（不存在或不属于该
// 调用方一律 `ErrNotFound`），已终态按 `ErrReservationSettled`（web 映射 409），
// 再预写 pending 并执行。归属校验发生在预写**之前**，不给错误的调用方留审计行。
// ReconcileReservation checks ownership before pre-writing the operation.
func (a *CallerAdmin) ReconcileReservation(ctx context.Context, actor core.Actor, req ReconcileRequest) (Settlement, error) {
	if err := a.ready(); err != nil {
		return Settlement{}, err
	}
	if req.CallerID == "" || req.ReservationID == "" {
		return Settlement{}, invalidRequest("调用方或预留标识为空")
	}
	if !req.Outcome.Valid() {
		return Settlement{}, invalidRequest("未知核对结局 %q", string(req.Outcome))
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return Settlement{}, invalidRequest("核对理由不得为空")
	}
	switch req.Outcome {
	case ReconcileCommitted:
		if req.Actual == nil || *req.Actual < 0 {
			return Settlement{}, invalidRequest("outcome=committed 必须给出非负 actual")
		}
	case ReconcileReleased:
		if req.Actual != nil {
			return Settlement{}, invalidRequest("outcome=released 不接受 actual")
		}
	}
	current, err := a.quotas.Get(ctx, req.CallerID, req.ReservationID)
	if err != nil {
		return Settlement{}, err
	}
	if current.Status != ReservationPending {
		return Settlement{}, fmt.Errorf("%w: 预留 %s 状态 %s", ErrReservationSettled, req.ReservationID, current.Status)
	}
	actual := int64(0)
	if req.Actual != nil {
		actual = *req.Actual
	}
	detail := reservationReconcileDetail{
		CallerID:      req.CallerID,
		ReservationID: req.ReservationID,
		Outcome:       string(req.Outcome),
		Reason:        reason,
	}
	if req.Actual != nil {
		actualCopy := *req.Actual
		detail.Actual = &actualCopy
	}
	operationID, err := a.writer().begin(ctx, actor, actionCallerReservationReconcile, req.CallerID,
		reservationReconcileRequestHash(req, actual), detail)
	if err != nil {
		return Settlement{}, err
	}
	settlement, reconcileErr := a.quotas.Reconcile(ctx, req.ReservationID, req.Outcome, actual, reason)
	if reconcileErr != nil {
		a.finishFailed(ctx, operationID, adminResultCode(reconcileErr), reconcileErr, detail)
		return Settlement{}, fmt.Errorf("核对预留失败: %w", reconcileErr)
	}
	a.writer().finish(ctx, operationID, core.OperationSuccess, resultOK, "", detail)
	return settlement, nil
}

// ClearOverage 清零一个周期的超限计数并回读该周期的用量视图。调用方不存在按
// `ErrNotFound` 拒绝（404 只表示"没有这个调用方"）；没有限额行时周期按 day / UTC
// 折算——"没有限额"与"没有这个调用方"是两件事。
// ClearOverage zeroes the period's overage and returns the fresh usage view.
func (a *CallerAdmin) ClearOverage(ctx context.Context, actor core.Actor, req ClearOverageRequest) (CallerUsage, error) {
	if err := a.ready(); err != nil {
		return CallerUsage{}, err
	}
	if req.CallerID == "" {
		return CallerUsage{}, fmt.Errorf("%w: 调用方标识为空", ErrNotFound)
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return CallerUsage{}, invalidRequest("清零理由不得为空")
	}
	if _, err := a.keys.LookupCaller(ctx, req.CallerID); err != nil {
		return CallerUsage{}, err
	}
	startMillis := int64(0)
	if !req.PeriodStart.IsZero() {
		startMillis = req.PeriodStart.UnixMilli()
	}
	detail := clearOverageDetail{
		Scope:                 overageScope,
		CallerID:              req.CallerID,
		PeriodStartUnixMillis: startMillis,
		Reason:                reason,
	}
	operationID, err := a.writer().begin(ctx, actor, actionCallerReservationReconcile, req.CallerID,
		operationRequestHash("clear-overage", req.CallerID, fmt.Sprint(startMillis), reason), detail)
	if err != nil {
		return CallerUsage{}, err
	}
	if clearErr := a.quotas.ClearOverage(ctx, req.CallerID, startMillis, reason); clearErr != nil {
		a.finishFailed(ctx, operationID, adminResultCode(clearErr), clearErr, detail)
		return CallerUsage{}, fmt.Errorf("清零超限失败: %w", clearErr)
	}
	a.writer().finish(ctx, operationID, core.OperationSuccess, resultOK, "", detail)
	return a.Usage(ctx, req.CallerID, startMillis)
}

// ready 报告三个端口是否都装配好；缺任何一个都拒绝执行管理动作。
// ready reports whether every port is wired.
func (a *CallerAdmin) ready() error {
	if a == nil || a.keys == nil || a.quotas == nil || a.ops == nil {
		return errCallerAdminUnconfigured
	}
	return nil
}

// writer 用同一组依赖构造共享写法器；主语用调用方管理操作以区分 Key 生命周期。
// writer builds the shared operation writer with the admin log subject.
func (a *CallerAdmin) writer() operationWriter {
	return operationWriter{ops: a.ops, logger: a.logger, now: a.now, subject: subjectCallerOperation}
}

// finishFailed 回填失败终态；错误按分类哨兵投影成 `result_code`。
// finishFailed writes the failed terminal state with a projected result code.
func (a *CallerAdmin) finishFailed(ctx context.Context, operationID, code string, err error, detail any) {
	a.writer().finish(ctx, operationID, core.OperationFailed, code, err.Error(), detail)
}

// adminResultCode 在 Key 生命周期的投影之上补两条管理面分类：冲突与请求非法。
// 认不出的错误仍落到 `INTERNAL`（与 `keyResultCode` 同一兜底）。
// adminResultCode adds the conflict and invalid-request classes to the shared
// projection; anything unrecognised still falls back to INTERNAL.
func adminResultCode(err error) string {
	switch {
	case errors.Is(err, core.ErrConflict):
		return resultConflict
	case errors.Is(err, ErrInvalidRequest):
		return resultInvalidRequest
	case errors.Is(err, ErrReservationSettled):
		return resultConflict
	default:
		return keyResultCode(err)
	}
}

// projectUsage 把存储聚合投影成契约视图：
//   - 已结算 / 预留量按 (账本, 类别) 落到六个类别行上；
//   - `overage` 是**账本级**信号，只挂账本的代表行（tasks→run、tokens→chat、
//     ingest_bytes→ingest_bytes），其余行为 0——保证各行相加等于账本总值；
//   - 出现闭集之外的账本或 (类别, 账本) 组合即报错（存储层与本层字面量漂移的
//     信号，**不**静默丢行）。
//
// projectUsage maps the raw aggregate onto the six frozen usage rows; overage stays
// on each ledger's representative row and unknown pairs are reported, never dropped.
func projectUsage(aggregate UsageAggregate) (CallerUsage, error) {
	if aggregate.PeriodStart <= 0 {
		return CallerUsage{}, errors.New("调用方用量缺少周期起点")
	}
	// 限额行缺失时存储层给的是零值 Limit；周期与时区按 DDL 缺省（day / UTC）
	// 补齐——但限额数字**不**补，零就是零（"没有配置"不是"不限量"）。
	period := aggregate.Limit.Period
	if period == "" {
		period = PeriodDay
	}
	if !period.Valid() {
		return CallerUsage{}, fmt.Errorf("调用方用量投影: 未知周期 %q", string(period))
	}
	timezone := strings.TrimSpace(aggregate.Limit.Timezone)
	if timezone == "" {
		timezone = DefaultTimezone
	}
	start := time.UnixMilli(aggregate.PeriodStart).UTC()
	end, err := NextPeriodStart(period, timezone, start)
	if err != nil {
		return CallerUsage{}, err
	}
	limit := aggregate.Limit
	limit.Period = period
	limit.Timezone = timezone

	lines := make([]UsageLine, 0, len(usageLineOrder))
	index := make(map[string]int, len(usageLineOrder))
	for _, kind := range usageLineOrder {
		index[kind] = len(lines)
		lines = append(lines, UsageLine{Kind: kind})
	}
	seen := make(map[string]struct{}, len(aggregate.Classes))
	for _, class := range aggregate.Classes {
		kind, mapErr := usageKindFor(class.Class, class.Ledger)
		if mapErr != nil {
			return CallerUsage{}, mapErr
		}
		if _, dup := seen[kind]; dup {
			return CallerUsage{}, fmt.Errorf("调用方用量投影: 类别行 %q 重复", kind)
		}
		seen[kind] = struct{}{}
		lines[index[kind]].Used = class.Used
		lines[index[kind]].Reserved = class.Reserved
	}
	for _, ledger := range aggregate.Ledger {
		kind, mapErr := overageLineFor(ledger.Kind)
		if mapErr != nil {
			return CallerUsage{}, mapErr
		}
		lines[index[kind]].Overage = ledger.Overage
	}

	return CallerUsage{
		CallerID:            aggregate.CallerID,
		PeriodStart:         start,
		PeriodEnd:           end,
		Timezone:            timezone,
		Limit:               limit,
		Lines:               lines,
		PendingReservations: aggregate.PendingReservations,
	}, nil
}

// usageKindFor 把 (类别, 账本) 映射成对外类别；表外的组合一律报错。
// usageKindFor maps (class, ledger) onto the frozen usage kinds.
func usageKindFor(class RequestClass, ledger string) (string, error) {
	switch class {
	case ClassRun:
		if ledger == LedgerTasks {
			return UsageKindRun, nil
		}
	case ClassChat:
		if ledger == LedgerTokens {
			return UsageKindChat, nil
		}
	case ClassCompaction:
		if ledger == LedgerTokens {
			return UsageKindCompaction, nil
		}
	case ClassRerank:
		if ledger == LedgerTokens {
			return UsageKindRerank, nil
		}
	case ClassIngest:
		switch ledger {
		case LedgerTokens:
			return UsageKindEmbeddingTokens, nil
		case LedgerIngestBytes:
			return UsageKindIngestBytes, nil
		}
	}
	return "", fmt.Errorf("调用方用量投影: 类别 %q 与账本 %q 不相容", string(class), ledger)
}

// overageLineFor 返回账本的代表行（`overage` 只挂在这里）。
// overageLineFor names the representative usage row of each ledger.
func overageLineFor(ledger string) (string, error) {
	switch ledger {
	case LedgerTasks:
		return UsageKindRun, nil
	case LedgerTokens:
		return UsageKindChat, nil
	case LedgerIngestBytes:
		return UsageKindIngestBytes, nil
	default:
		return "", fmt.Errorf("调用方用量投影: 未知账本 %q", ledger)
	}
}

// callerCreateDetail 是 `caller.create` 的 `detail_json` 固定投影。
// callerCreateDetail is the fixed projection of a caller creation.
type callerCreateDetail struct {
	CallerID string             `json:"caller_id"`
	Name     string             `json:"name"`
	Project  string             `json:"project,omitempty"`
	Limits   *callerLimitDetail `json:"limits,omitempty"`
}

// callerLimitDetail 是限额的审计投影（不写限额行时整体省略）。
// callerLimitDetail is the audited projection of a limit.
type callerLimitDetail struct {
	QPS                  float64 `json:"qps"`
	Burst                int     `json:"burst"`
	MaxConcurrent        int     `json:"max_concurrent"`
	TasksPerPeriod       int64   `json:"tasks_per_period"`
	TokensPerPeriod      int64   `json:"tokens_per_period"`
	IngestBytesPerPeriod int64   `json:"ingest_bytes_per_period"`
	Period               string  `json:"period"`
	Timezone             string  `json:"timezone"`
}

// newLimitDetail 把可选限额转成审计投影；nil 保持 nil（审计里省略该键）。
// newLimitDetail renders the optional limit, keeping nil nil.
func newLimitDetail(limit *Limit) *callerLimitDetail {
	if limit == nil {
		return nil
	}
	return &callerLimitDetail{
		QPS:                  limit.QPS,
		Burst:                limit.Burst,
		MaxConcurrent:        limit.MaxConcurrent,
		TasksPerPeriod:       limit.TasksPerPeriod,
		TokensPerPeriod:      limit.TokensPerPeriod,
		IngestBytesPerPeriod: limit.IngestBytesPerPeriod,
		Period:               string(limit.Period),
		Timezone:             limit.Timezone,
	}
}

// reservationReconcileDetail 是 `caller.reservation.reconcile` 在预留级核对时的
// 投影：`scope` 省略即表示预留级，理由与结局可追溯。
// reservationReconcileDetail records one reservation-level reconcile.
type reservationReconcileDetail struct {
	CallerID      string `json:"caller_id"`
	ReservationID string `json:"reservation_id"`
	Outcome       string `json:"outcome"`
	Actual        *int64 `json:"actual,omitempty"`
	Reason        string `json:"reason"`
}

// clearOverageDetail 是同一动作在**账本级**清零时的投影：`scope=overage` 把它与
// 预留级核对区分开（契约 §9.3 没有为清零单列动作字面量，见 run-state R34）。
// clearOverageDetail marks the ledger-scoped overage reset under the same action.
type clearOverageDetail struct {
	Scope                 string `json:"scope"`
	CallerID              string `json:"caller_id"`
	PeriodStartUnixMillis int64  `json:"period_start_unix_ms,omitempty"`
	Reason                string `json:"reason"`
}

// callerCreateRequestHash 是创建请求的脱敏摘要（限额各字段参与，便于区分同一
// 名称的两次不同配置）。
// callerCreateRequestHash is the redacted digest of a creation request.
func callerCreateRequestHash(record CallerRecord, limit *Limit) string {
	limits := "none"
	if limit != nil {
		limits = fmt.Sprintf("%g|%d|%d|%d|%d|%d|%s|%s",
			limit.QPS, limit.Burst, limit.MaxConcurrent, limit.TasksPerPeriod,
			limit.TokensPerPeriod, limit.IngestBytesPerPeriod, limit.Period, limit.Timezone)
	}
	return operationRequestHash("create", record.CallerID, record.Name, record.Project, limits)
}

// reservationReconcileRequestHash 是核对请求的脱敏摘要；`actual` 未给出时用
// 显式占位符参与，避免"给了 0"与"没给"算出同一摘要。
// reservationReconcileRequestHash keeps "no actual" distinct from "actual 0".
func reservationReconcileRequestHash(req ReconcileRequest, actual int64) string {
	actualText := "-"
	if req.Actual != nil {
		actualText = fmt.Sprint(actual)
	}
	return operationRequestHash("reconcile", req.CallerID, req.ReservationID,
		string(req.Outcome), actualText, strings.TrimSpace(req.Reason))
}

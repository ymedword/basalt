package callers

// 本文件是图 6 [5] 的配额准入与结算（C-29 / C-35 / FR-035 / §2.3「预留 / 结算
// 口径」「记账与状态」两表）：`AdmitCall` 每次**真正发出**的消耗调用一次，只预留、
// 不占 Run 并发名额；`Commit` 用供应商返回的实际用量结算并自动释放余额；`Release`
// 只用于确认零消耗；结果未知时预留保持 `pending` 占额度，由管理面 `Reconcile` 收口。
// 配额端口定义在消费方（三条包纪律之二），PG 实现在 `storage/postgres/quota_store.go`。
// This file owns quota admission and settlement (figure 6 stage [5]): one reservation
// per actually-issued call, commit with real usage, release only when zero consumption
// is proven, and unknown outcomes stay pending until an admin reconciles them.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

var (
	// ErrQuotaExhausted 表示预留失败：周期额度不足、限额缺失 / 为零或 `overage > 0`
	// 未清零。对外映射 429 `QUOTA_EXHAUSTED` + Retry-After（到下一周期起点）。
	// ErrQuotaExhausted covers exhausted, missing and zero limits plus an uncleared
	// overage; the web layer maps it to 429 with Retry-After.
	ErrQuotaExhausted = errors.New("callers: quota exhausted")

	// ErrIdempotencyConflict 表示同一幂等键被换成了不同参数（class / 成本 / 用量），
	// 服务端**不得**静默复用旧预留，也不得新建第二行。对外映射 409。
	// ErrIdempotencyConflict rejects a reused key with different parameters.
	ErrIdempotencyConflict = errors.New("callers: idempotency key reused with different parameters")

	// ErrReservationSettled 表示对已 `released` / `reconciled` 的预留发 `Commit`。
	// 它只服务内部状态机（`committed` 的重复提交走首次结果），**不**新增对外错误码。
	// ErrReservationSettled marks a commit against a terminal non-committed state.
	ErrReservationSettled = errors.New("callers: reservation already settled")
)

// errInvalidRequestClass 表示调用方传入了闭集之外的消耗类别。
// errInvalidRequestClass rejects a class outside the five frozen literals.
var errInvalidRequestClass = errors.New("callers: unknown request class")

// QuotaExhaustedError 在 `ErrQuotaExhausted` 之外带出**恢复时间**：周期额度不足时
// 是下一周期起点（与 `NextPeriodStart` 同一口径，web 层据此写 `Retry-After`）——
// 契约 §8 要求 429 `QUOTA_EXHAUSTED` 给出恢复时间。限额为零、限额行缺失或
// `overage > 0` 未核对时 `RetryAt` 为零值：那些情况等一个周期不会恢复，靠管理员
// 调额或核对，**不编**一个时间。
// QuotaExhaustedError carries the recovery instant for the 429 Retry-After; a zero
// RetryAt means recovery needs an administrator, not the calendar.
type QuotaExhaustedError struct {
	// Reason 是给人看的具体原因（额度不足 / 限额为零 / overage 未清），日志与
	// 响应消息仍以哨兵文案为准。
	Reason string
	// RetryAt 是恢复时刻；零值 = 没有日历上的恢复时间。
	RetryAt time.Time
}

// Error 实现 error；文案沿用哨兵前缀，附加原因。
// Error keeps the sentinel wording and appends the reason.
func (e *QuotaExhaustedError) Error() string {
	switch {
	case e == nil || e.Reason == "":
		return ErrQuotaExhausted.Error()
	default:
		return ErrQuotaExhausted.Error() + ": " + e.Reason
	}
}

// Unwrap 让 `errors.Is(err, ErrQuotaExhausted)` 对携带恢复时间的错误照旧成立——
// 既有调用方（HTTP 映射、诊断原因码、知识适配器）不需要任何改动。
// Unwrap keeps every existing errors.Is check working unchanged.
func (e *QuotaExhaustedError) Unwrap() error { return ErrQuotaExhausted }

// errInvalidCost 表示预留成本不合法：负值、或非恰好一个记账维度非零。
// errInvalidCost rejects a negative or ambiguous reservation cost.
var errInvalidCost = errors.New("callers: reservation cost must set exactly one non-negative dimension")

// errInvalidUsage 表示实际用量不合法：负值，或同时给了多个记账维度。
// errInvalidUsage rejects a negative or ambiguous settled usage.
var errInvalidUsage = errors.New("callers: usage must not be negative or set more than one dimension")

// errMissingIdempotencyKey 表示调用方没给幂等键。键由调用方生成（C-29），末位必须
// 是这一次实际发出的尝试标识；缺失即拒绝，**不得**在服务端临时造一个。
// errMissingIdempotencyKey refuses a reservation without a caller-generated key.
var errMissingIdempotencyKey = errors.New("callers: idempotency key is required")

// errQuotaUnconfigured 是装配错误：缺少配额端口时拒绝请求，**不得**放行。
// errQuotaUnconfigured fails closed when the quota port is missing.
var errQuotaUnconfigured = errors.New("callers: quota store is not configured")

// errInvalidTicket 表示配额票据缺少归属或预留标识，从未被签发，不得结算。
// errInvalidTicket rejects a ticket that was never issued.
var errInvalidTicket = errors.New("callers: quota ticket is missing its caller or reservation")

// errInvalidReleaseReason 表示释放理由是空串或闭集之外的取值——释放必须可追溯。
// errInvalidReleaseReason requires a known, non-empty release reason.
var errInvalidReleaseReason = errors.New("callers: release reason is required")

// errInvalidReconcile 表示核对参数不合法：结局非法、理由为空或实际用量为负。
// errInvalidReconcile rejects an unknown outcome, an empty reason or a negative actual.
var errInvalidReconcile = errors.New("callers: invalid reconcile request")

// RequestClass 是一次消耗调用的类别，取值逐字对齐 openapi `ReservationView.class`
// 的五值枚举（`run` / `chat` / `compaction` / `ingest` / `rerank`）。
// RequestClass mirrors the five frozen class literals of ReservationView.class.
type RequestClass string

const (
	// ClassRun 是 Run 级任务次数：每次逻辑准入一次，不随模型调用次数增加。
	// ClassRun counts run-level task admissions.
	ClassRun RequestClass = "run"
	// ClassChat 是对话模型调用。
	// ClassChat covers chat model calls.
	ClassChat RequestClass = "chat"
	// ClassCompaction 是摘要调用，与 `ClassChat` 分开记账（C-25）。
	// ClassCompaction covers compaction calls, booked apart from chat.
	ClassCompaction RequestClass = "compaction"
	// ClassIngest 是摄取：字节配额与摄取期 Embedding 的 Token 都记在它名下。
	// ClassIngest covers ingest bytes and ingest-time embedding tokens.
	ClassIngest RequestClass = "ingest"
	// ClassRerank 是重排调用；不存在则根本不预留。
	// ClassRerank covers rerank calls; a skipped rerank reserves nothing.
	ClassRerank RequestClass = "rerank"
)

// Valid 判定类别是否在闭集内；空值与未知值一律拒绝。
// Valid accepts exactly the five literals; the zero value and others fail.
func (c RequestClass) Valid() bool {
	switch c {
	case ClassRun, ClassChat, ClassCompaction, ClassIngest, ClassRerank:
		return true
	default:
		return false
	}
}

// Cost 是预留成本：**恰好一个**维度非零，维度本身决定记哪一本账（任务次数 /
// Token / 摄取字节）。`ClassIngest` 同时允许 Token（摄取期 Embedding）与字节，
// 因此类别与账本不是一一对应（R28.1）。
// Cost carries exactly one non-zero dimension, and that dimension picks the ledger;
// ingest is the class that may book either tokens or bytes.
type Cost struct {
	// Tasks 是任务次数（只配 `ClassRun`）。
	// Tasks is the task count and belongs to ClassRun only.
	Tasks int64
	// Tokens 是 Token 数（对话 / 摘要 / 重排，以及摄取期 Embedding）。
	// Tokens covers chat, compaction, rerank and ingest-time embedding.
	Tokens int64
	// Bytes 是摄取字节数（只配 `ClassIngest`）。
	// Bytes is the ingest byte count and belongs to ClassIngest only.
	Bytes int64
}

// validate 判定成本与类别相容：负值、全零、多维度、类别与维度不符都拒绝。
// validate rejects negative, empty, ambiguous or class-incompatible costs.
func (c Cost) validate(class RequestClass) error {
	if c.Tasks < 0 || c.Tokens < 0 || c.Bytes < 0 {
		return fmt.Errorf("%w: 成本不得为负", errInvalidCost)
	}
	dims := 0
	for _, v := range []int64{c.Tasks, c.Tokens, c.Bytes} {
		if v > 0 {
			dims++
		}
	}
	if dims != 1 {
		return fmt.Errorf("%w: 实际设置了 %d 个维度", errInvalidCost, dims)
	}
	if c.Tasks > 0 && class != ClassRun {
		return fmt.Errorf("%w: 任务次数只配 ClassRun，不接受 %q", errInvalidCost, class)
	}
	if c.Tokens > 0 && class == ClassRun {
		return fmt.Errorf("%w: ClassRun 只按任务次数记账", errInvalidCost)
	}
	if c.Bytes > 0 && class != ClassIngest {
		return fmt.Errorf("%w: 字节只配 ClassIngest，不接受 %q", errInvalidCost, class)
	}
	return nil
}

// Usage 是 Commit 的实际用量：只填该预留所属账本的维度，其余为零。全零表示
// 供应商确认零消耗；**结果未知不是零**，那种情况必须保持 `pending`。
// Usage holds the settled amount in the reservation's own dimension; an all-zero
// usage means known zero, never "unknown".
type Usage struct {
	// Tasks 是实际任务次数。
	Tasks int64
	// Tokens 是实际 Token 数。
	Tokens int64
	// Bytes 是实际字节数。
	Bytes int64
}

// validate 拒绝负值与多维度；全零合法（供应商确认零消耗）。
// validate rejects negatives and multiple dimensions; zeros are legal.
func (u Usage) validate() error {
	if u.Tasks < 0 || u.Tokens < 0 || u.Bytes < 0 {
		return fmt.Errorf("%w: 用量不得为负", errInvalidUsage)
	}
	dims := 0
	for _, v := range []int64{u.Tasks, u.Tokens, u.Bytes} {
		if v > 0 {
			dims++
		}
	}
	if dims > 1 {
		return fmt.Errorf("%w: 实际设置了 %d 个维度", errInvalidUsage, dims)
	}
	return nil
}

// ReservationStatus 是预留状态，逐字对齐 openapi `ReservationView.status`。
// 状态只允许 `pending → committed / released / reconciled`，终态不再迁移。
// ReservationStatus mirrors the four frozen status literals; terminal states never
// move again.
type ReservationStatus string

const (
	// ReservationPending 表示已占额度、结局未知。
	// ReservationPending holds quota until the outcome is known.
	ReservationPending ReservationStatus = "pending"
	// ReservationCommitted 表示已按实际用量结算。
	// ReservationCommitted is settled with real usage.
	ReservationCommitted ReservationStatus = "committed"
	// ReservationReleased 表示确认零消耗并归还额度。
	// ReservationReleased returns quota after proving zero consumption.
	ReservationReleased ReservationStatus = "released"
	// ReservationReconciled 表示由管理面核对收口。
	// ReservationReconciled is closed by an admin reconcile.
	ReservationReconciled ReservationStatus = "reconciled"
)

// Valid 判定状态是否在闭集内。
// Valid accepts exactly the four literals.
func (s ReservationStatus) Valid() bool {
	switch s {
	case ReservationPending, ReservationCommitted, ReservationReleased, ReservationReconciled:
		return true
	default:
		return false
	}
}

// ReconcileOutcome 是管理面核对的两个结局，逐字对齐 openapi
// `ReservationReconcileRequest.outcome`。
// ReconcileOutcome mirrors the two frozen reconcile outcomes.
type ReconcileOutcome string

const (
	// ReconcileCommitted 表示核对为「已消耗」，按实际用量记账。
	// ReconcileCommitted books the call as consumed.
	ReconcileCommitted ReconcileOutcome = "committed"
	// ReconcileReleased 表示核对为「未消耗」，全额归还。
	// ReconcileReleased returns the whole reservation.
	ReconcileReleased ReconcileOutcome = "released"
)

// Valid 判定核对结局是否在闭集内。
// Valid accepts exactly the two literals.
func (o ReconcileOutcome) Valid() bool {
	return o == ReconcileCommitted || o == ReconcileReleased
}

// ReleaseReason 是释放原因：释放必须可追溯，空串与未知值一律拒绝。
// ReleaseReason keeps every release traceable; unknown or empty reasons fail.
type ReleaseReason string

const (
	// ReleaseNotSent 表示调用确定未发出（连接失败、请求被本地拒绝）。
	// ReleaseNotSent means the call provably never left the process.
	ReleaseNotSent ReleaseReason = "not_sent"
	// ReleaseAuditFailed 表示审计预写失败——fail-closed 下调用不得发出。
	// ReleaseAuditFailed means the pending audit row failed to write, so no call
	// was issued.
	ReleaseAuditFailed ReleaseReason = "audit_failed"
	// ReleaseZeroUsage 表示调用已发出但确认零消耗。
	// ReleaseZeroUsage means the call ran and consumed nothing.
	ReleaseZeroUsage ReleaseReason = "zero_usage"
)

// Valid 判定释放原因是否在闭集内。
// Valid accepts exactly the three literals.
func (r ReleaseReason) Valid() bool {
	switch r {
	case ReleaseNotSent, ReleaseAuditFailed, ReleaseZeroUsage:
		return true
	default:
		return false
	}
}

// Reservation 是 `Reserve` 的返回行视图（字段随契约评审冻结，R28.6）。
// Reservation is the row view returned by Reserve.
type Reservation struct {
	// ID 是服务端生成的预留标识，对调用方不透明。
	// ID is the server-generated opaque reservation id.
	ID string
	// CallerID 是归属调用方（额度按调用方计，不按 Key 计）。
	// CallerID is the owner; quotas are per caller, never per key.
	CallerID string
	// Class 是消耗类别。
	// Class is the request class.
	Class RequestClass
	// PeriodStart 是创建时的周期起点（Unix 毫秒 UTC）；结算**总是**更新该周期。
	// PeriodStart is the creation period; settlement always updates this period.
	PeriodStart int64
	// Amount 是预留量，单位由类别对应的账本决定。
	// Amount is the reserved quantity in the class's own ledger.
	Amount int64
	// Status 是当前状态。
	// Status is the current reservation state.
	Status ReservationStatus
}

// Settlement 是 `Commit` / `Reconcile` 的返回结果。
// Settlement is the result of Commit and Reconcile.
type Settlement struct {
	// ReservationID 是被结算的预留。
	ReservationID string
	// CallerID 是归属调用方。
	CallerID string
	// Class 是预留的消耗类别（契约 `ReservationView.class` 必填）；核对响应靠它
	// 回显，否则页面无从判断这条预留记的是哪本账。
	Class RequestClass
	// State 是结算后的状态。
	State ReservationStatus
	// PeriodStart 是记账周期（预留创建时的周期，不按当前时间重算）。
	PeriodStart int64
	// Amount 是预留量。
	Amount int64
	// Actual 是实际用量；committed 时非空。
	Actual int64
	// Overage 是已发生且不可回滚的超限：`max(0, actual-amount)`。
	Overage int64
}

// Ticket 是配额票据：只有真正预留成功才签发；`NoReservation` 表示内置管理员的
// 不限量路径——它**不**写预留行，结算方法一律空操作。Run 并发票据是 `RunTicket`，
// 两者**不得**混用。
// Ticket is a quota reservation handle; NoReservation marks the unlimited built-in
// admin path where no row exists and settlement is a no-op.
type Ticket struct {
	// ReservationID 是预留标识。
	ReservationID string
	// CallerID 是归属调用方，结算**必须**带上它做归属校验。
	CallerID string
	// PeriodStart 是预留所属周期起点（Unix 毫秒 UTC）。
	PeriodStart int64
	// NoReservation 表示未写预留行（内置管理员不限量）。
	NoReservation bool
}

// Limit 是调用方限额配置，字段对齐 `caller_limits` 列（openapi
// `CallerLimitView`）。**零值语义按列分工**，以契约注释为准（C-F01，2026-10-31
// 裁决第 5 条）：`qps` / `burst` 的 0 表示"不额外收紧"（入口类别预算仍生效），
// 周期用量与并发名额的 0 按失败关闭处理；**任何一列都不得**读成"不限量"。
// Limit mirrors the caller_limits columns. Zero is per-column: qps/burst zero means
// "no extra tightening" while periodic quotas fail closed; none ever means unlimited.
type Limit struct {
	// QPS 是入口稳态速率**覆盖**（图 6 [1]，C-F01）：0 表示不覆盖，沿用
	// `callers.ingress.*` 的类别预算；非 0 时生效值为 `min(类别, 本值)`——只收紧、
	// 不放宽（契约 `docs/contracts/schema.pg.sql:931` 的 `0 = 不限`）。
	QPS float64
	// Burst 是入口突发额度覆盖，零值与收紧语义同 QPS（契约
	// `docs/contracts/schema.pg.sql:932`）。
	Burst int
	// MaxConcurrent 是并发 Run 名额。
	MaxConcurrent int
	// TasksPerPeriod 是周期任务次数额度。
	TasksPerPeriod int64
	// TokensPerPeriod 是周期 Token 额度。
	TokensPerPeriod int64
	// IngestBytesPerPeriod 是周期摄取字节额度。
	IngestBytesPerPeriod int64
	// Period 是周期类型；空值按 `PeriodDay`。
	Period Period
	// Timezone 是周期起算时区；空值按 `UTC`。
	Timezone string
}

// Normalize 补齐缺省并校验：周期空值取 `day`、时区空值取 `UTC`，负值与非闭集
// 取值一律拒绝。存储层与装配层共用同一份口径，避免两处默认值漂移。
// Normalize fills the documented defaults and rejects negatives or unknown literals
// so the store and the wiring cannot drift apart.
func (l Limit) Normalize() (Limit, error) {
	if l.QPS < 0 || l.Burst < 0 || l.MaxConcurrent < 0 ||
		l.TasksPerPeriod < 0 || l.TokensPerPeriod < 0 || l.IngestBytesPerPeriod < 0 {
		return Limit{}, fmt.Errorf("callers: 限额不得为负")
	}
	if l.Period == "" {
		l.Period = PeriodDay
	}
	if !l.Period.Valid() {
		return Limit{}, fmt.Errorf("callers: 未知配额周期 %q", string(l.Period))
	}
	if strings.TrimSpace(l.Timezone) == "" {
		l.Timezone = DefaultTimezone
	}
	if _, err := loadCallerLocation(l.Timezone); err != nil {
		return Limit{}, err
	}
	return l, nil
}

// QuotaStore 是配额端口（图 6 [5]）：预留、结算、核对与限额管理。接口定义在
// 消费方，实现在 `storage/postgres/quota_store.go`（`postgres.NewQuotaStore`）。
// `Commit` / `Release` **必须**按 `(caller_id, reservation_id)` 定位——只凭
// `reservation_id` 结算会绕过归属校验；`Reconcile` / `ClearOverage` 只由管理面
// 发起，理由写入管理审计。
// QuotaStore is the quota port; commits and releases are always located by
// (caller_id, reservation_id), and reconcile/clear-overage are admin surfaces.
type QuotaStore interface {
	// Reserve 原子预留：额度不足、限额缺失 / 为零或 `overage > 0` 时返回
	// `ErrQuotaExhausted`，**不**产生部分预留；同一幂等键换参数返回
	// `ErrIdempotencyConflict`，参数一致则返回同一预留。
	Reserve(ctx context.Context, callerID string, class RequestClass, cost Cost, idem string) (Reservation, error)

	// Commit 按实际用量结算：`pending → committed`，余额自动释放；重复提交返回
	// 首次结果，用量不一致返回 `ErrIdempotencyConflict`；对 `released` /
	// `reconciled` 返回 `ErrReservationSettled`。
	Commit(ctx context.Context, callerID, reservationID string, usage Usage) (Settlement, error)

	// Release 只用于确认零消耗：`pending → released`；其余状态为空操作。
	Release(ctx context.Context, callerID, reservationID string, reason ReleaseReason) error

	// Reconcile 由管理面把 `pending` 收口为 `reconciled`：`committed` 按 `actual`
	// 记账（大于预留量时记 `overage`，不截断），`released` 按零消耗归还。
	Reconcile(ctx context.Context, reservationID string, outcome ReconcileOutcome, actual int64, reason string) (Settlement, error)

	// ClearOverage 清零某周期的超限计数；`periodStart` 为 0 时按当前周期。
	ClearOverage(ctx context.Context, callerID string, periodStart int64, reason string) error

	// SetLimit 写入调用方限额（同一事务内按需建调用方行，满足外键）。
	SetLimit(ctx context.Context, callerID string, limit Limit) error

	// ListPending 列出该调用方结局未知的预留，供管理面核对。
	ListPending(ctx context.Context, callerID string) ([]Reservation, error)

	// Get 按 `(caller_id, reservation_id)` 返回预留行；行不存在或归属不匹配一律
	// 返回 `ErrNotFound`。管理面核对必须先经它校验归属与当前状态，**不得**只凭
	// `reservation_id` 就写审计记录——否则一条预留可以被记到错误的调用方名下。
	// Get resolves a reservation by owner and id; a mismatch is ErrNotFound.
	Get(ctx context.Context, callerID, reservationID string) (Reservation, error)
}

// AdmitCall 承担图 6 的 [5]：每次**真正发出**的消耗调用一次（Run 级任务次数 /
// Chat / 摘要 / Embedding / 重排 / 摄取字节）。不获取、也不要求 `RunTicket`——
// 同一 Run 的多次调用共用同一个并发名额（C-35）。幂等键由调用方按 C-29 生成，
// 末位**必须**是这一次实际发出的尝试标识。
// AdmitCall is stage [5]: once per actually-issued call, independent of the run
// slot, with a caller-generated idempotency key.
//
// 逆改点：在 Commit 之后仍按同一键再 AdmitCall 一次 → 两次消耗被合成一条
// （`TestQuota_EmbeddingRetryAfterTimeoutGetsNewKey` 红）；把内置管理员也送去
// Reserve → `TestQuota_LocalAdminUnlimitedNoReservation` 断言零预留行时红。
// Mutation: re-reserving with the same key after a commit merges two real calls
// into one row; reserving for the unlimited admin leaves an unexpected row.
func (l *Limiter) AdmitCall(ctx context.Context, p core.Principal, class RequestClass, cost Cost, idem string) (Ticket, error) {
	if l == nil {
		return Ticket{}, errQuotaUnconfigured
	}
	if err := ctx.Err(); err != nil {
		return Ticket{}, err
	}
	if !p.Valid() {
		return Ticket{}, core.ErrUnauthorized
	}
	if !class.Valid() {
		return Ticket{}, fmt.Errorf("%w: %q", errInvalidRequestClass, string(class))
	}
	if err := cost.validate(class); err != nil {
		return Ticket{}, err
	}
	if strings.TrimSpace(idem) == "" {
		return Ticket{}, errMissingIdempotencyKey
	}

	// 内置管理员不限量：不写预留行，结算为空操作；并发、超时与审计照常约束。
	// The unlimited built-in admin reserves nothing; slots, timeouts and audit still apply.
	if p.Kind == core.PrincipalLocalAdmin && l.localAdminUnlimited {
		return Ticket{CallerID: p.CallerID, NoReservation: true}, nil
	}
	if l.quotas == nil {
		return Ticket{}, errQuotaUnconfigured
	}

	reserved, err := l.quotas.Reserve(ctx, p.CallerID, class, cost, idem)
	if err != nil {
		return Ticket{}, err
	}
	return Ticket{
		ReservationID: reserved.ID,
		CallerID:      reserved.CallerID,
		PeriodStart:   reserved.PeriodStart,
	}, nil
}

// Commit 结算实际用量：`pending → committed`，余额自动释放。重复调用返回首次
// 结果；用量未知时**不得**调用（预留保持 `pending`，由 `Reconcile` 收口）。
// `NoReservation` 票据（内置管理员）为空操作。
// Commit settles real usage; unknown usage must stay pending instead.
//
// 本方法**不**检查 ctx.Err()：调用已经发生，用量必须记账（与 `ReleaseRun` 同理），
// 上下文失效会让本次结算失败、预留留在 `pending` 等管理面核对，这是诚实的结局。
// It deliberately ignores ctx.Err(): the call already happened and its usage is owed.
func (l *Limiter) Commit(ctx context.Context, t Ticket, usage Usage) (Settlement, error) {
	if l == nil {
		return Settlement{}, errQuotaUnconfigured
	}
	if t.NoReservation {
		return Settlement{}, nil
	}
	if t.ReservationID == "" || t.CallerID == "" {
		return Settlement{}, errInvalidTicket
	}
	if err := usage.validate(); err != nil {
		return Settlement{}, err
	}
	if l.quotas == nil {
		return Settlement{}, errQuotaUnconfigured
	}
	return l.quotas.Commit(ctx, t.CallerID, t.ReservationID, usage)
}

// Release 归还确认未消耗的预留：只对 `pending` 生效，`committed` 之后为空操作。
// `NoReservation` 票据为空操作。**不得**用它处理「已发出但结果未知」——那条
// 预留保持 `pending` 继续占额度（C-29）。
// Release returns a reservation proven unconsumed; unknown outcomes stay pending.
func (l *Limiter) Release(ctx context.Context, t Ticket, reason ReleaseReason) error {
	if l == nil {
		return errQuotaUnconfigured
	}
	if t.NoReservation {
		return nil
	}
	if t.ReservationID == "" || t.CallerID == "" {
		return errInvalidTicket
	}
	if !reason.Valid() {
		return errInvalidReleaseReason
	}
	if l.quotas == nil {
		return errQuotaUnconfigured
	}
	return l.quotas.Release(ctx, t.CallerID, t.ReservationID, reason)
}

// Reconcile 只由管理面发起：把结局未知的 `pending` 收口为 `reconciled`，理由
// 必填并写入管理审计。它**不**接受 `CallerID`——核对是管理动作，不是调用方自助。
// Reconcile is the admin-only path that closes an unknown reservation.
func (l *Limiter) Reconcile(ctx context.Context, reservationID string, outcome ReconcileOutcome, actual int64, reason string) (Settlement, error) {
	if l == nil {
		return Settlement{}, errQuotaUnconfigured
	}
	if reservationID == "" || !outcome.Valid() || actual < 0 || strings.TrimSpace(reason) == "" {
		return Settlement{}, errInvalidReconcile
	}
	if l.quotas == nil {
		return Settlement{}, errQuotaUnconfigured
	}
	return l.quotas.Reconcile(ctx, reservationID, outcome, actual, reason)
}

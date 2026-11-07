package postgres

// 本文件是 `callers.QuotaStore` 的 PostgreSQL 实现（图 6 [5] / C-29 / §2.3
// 「记账与状态」表 / tasks.md 附录 R28）：预留、结算、释放、核对与限额管理都落在
// `quota_reservations` + `caller_usage_periods` 两张表上，每条路径一个事务、一条
// 带条件的原子语句，**不得**先读后写（并发预留会双双通过）。
//
// 记账维度（kind）没有落库列（R28.1 裁决 C）：服务端把 kind 编进 `reservation_id`
// 前缀 `qres_<tasks|tokens|ingest_bytes>_<26 位 Crockford>`，结算路径从 ID 解析，
// 零 schema / 零 openapi 变更。调用方看到的只是不透明字符串。
// This file implements callers.QuotaStore on PostgreSQL. Every path is one
// transaction with a single conditional statement; the ledger dimension (kind) is
// encoded in the server-generated reservation id because quota_reservations has no
// kind column.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/callers"
)

const (
	// reservationIDPrefix 是预留标识的固定前缀。
	reservationIDPrefix = "qres_"

	// reservationPayloadLength 是 Crockford Base32 随机载荷长度（与 `core` 的身份
	// 载荷同长，26 字符 = 130 位随机）。
	reservationPayloadLength = 26

	// crockfordAlphabet 是 Crockford Base32 字符集，排除 I / L / O / U 四个易混
	// 字符。**本地复制的同一份字面量**：`core` 未导出通用生成器（`core/newUID`
	// 与 `core/splitUID` 均未导出，`core/identity.go:30`）；两处漂移由 R28.1 的
	// 登记约束与本注释共同看守。
	// Local copy of the Crockford alphabet; core exposes no reusable generator.
	crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

	// 账本名称（`caller_usage_periods.kind` 的 CHECK 三值，逐字）。
	usageKindTasks       = "tasks"
	usageKindTokens      = "tokens"
	usageKindIngestBytes = "ingest_bytes"
)

// errQuotaStoreUnconfigured 是装配错误：没有数据库句柄时拒绝服务，**不得**放行。
// errQuotaStoreUnconfigured fails closed when the handle is missing.
var errQuotaStoreUnconfigured = errors.New("postgres: quota store has no database handle")

// errQuotaInvalidArgument 表示调用参数不合法（空调用方、空幂等键、非法类别或成本）。
// errQuotaInvalidArgument rejects empty identities, keys, classes and costs.
var errQuotaInvalidArgument = errors.New("postgres: invalid quota argument")

// errQuotaUsageRowMissing 表示用量行缺失或余额不足以扣减——数据不一致，按错误
// 返回并回滚整个事务，**不得**静默继续。
// errQuotaUsageRowMissing reports an inconsistent ledger row; the transaction aborts.
var errQuotaUsageRowMissing = errors.New("postgres: caller usage row is missing or inconsistent")

// errQuotaInvalidReservationID 表示预留标识不是本服务生成的形态（前缀或载荷非法）。
// errQuotaInvalidReservationID rejects ids this server could not have produced.
var errQuotaInvalidReservationID = errors.New("postgres: invalid reservation id")

// quotaStore 是 `callers.QuotaStore` 的 PG 实现；对外只保证满足接口（R28.2）。
// quotaStore implements callers.QuotaStore; only the interface is guaranteed.
type quotaStore struct {
	db *DB
}

// 编译期断言：实现满足消费方接口。
// The implementation satisfies the consumer-side interface.
var _ callers.QuotaStore = (*quotaStore)(nil)

// NewQuotaStore 构造配额存储；返回类型不导出，调用方按 `callers.QuotaStore` 使用。
// NewQuotaStore builds the store; the concrete type stays unexported.
func NewQuotaStore(db *DB) *quotaStore {
	return &quotaStore{db: db}
}

// Reserve 原子预留一次消耗：先写预留行（pending）、再以条件更新占用额度——
// 条件 `used + reserved + amount <= limit` 与 `overage = 0` 在同一条 UPDATE 里求值，
// 并发预留因此不会双双通过；额度不足时整个事务回滚，**不**留部分预留。
// 同一幂等键换 class / 成本返回 `ErrIdempotencyConflict`，参数一致返回同一预留。
// Reserve inserts the pending row first and then claims quota with one conditional
// update, so concurrent reservations can never both pass the limit check.
//
// 逆改点：把条件累加拆成"先 SELECT 再 UPDATE" → `TestQuota_ReserveIsAtomicUnderConcurrency`
// 出现超发；把幂等键唯一约束换成 `(caller_id, run_id, kind)` → 同 Run 第二次调用冲突。
// Mutation: read-then-write lets concurrent reserves over-allocate.
func (s *quotaStore) Reserve(ctx context.Context, callerID string, class callers.RequestClass, cost callers.Cost, idem string) (callers.Reservation, error) {
	if s == nil || s.db == nil {
		return callers.Reservation{}, errQuotaStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" || strings.TrimSpace(idem) == "" || !class.Valid() {
		return callers.Reservation{}, errQuotaInvalidArgument
	}
	kind, amount, err := ledgerFor(class, cost)
	if err != nil {
		return callers.Reservation{}, err
	}

	var out callers.Reservation
	err = s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.db.clock().UTC()
		limit, periodStart, nextPeriod, err := reservationPeriod(ctx, tx, callerID, kind, now)
		if err != nil {
			return err
		}

		id, err := newReservationID(kind)
		if err != nil {
			return err
		}

		// 先写预留行：命中唯一键说明同一幂等键已经用过，此时**尚未**动过额度，
		// 直接回读旧行比对参数即可，不会留下重复计数。
		var inserted string
		err = tx.QueryRowContext(ctx, `
			INSERT INTO quota_reservations
				(reservation_id, caller_id, idempotency_key, class, period_start, amount, state, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7)
			ON CONFLICT (caller_id, idempotency_key) DO NOTHING
			RETURNING reservation_id`,
			id, callerID, idem, string(class), periodStart, amount, now.UnixMilli(),
		).Scan(&inserted)
		if errors.Is(err, sql.ErrNoRows) {
			existing, readErr := reservationByIdem(ctx, tx, callerID, idem)
			if readErr != nil {
				return readErr
			}
			existingKind, parseErr := parseReservationKind(existing.ID)
			if parseErr != nil {
				return parseErr
			}
			if existing.Class != class || existing.Amount != amount || existingKind != kind {
				return fmt.Errorf("%w: 键 %q 已用于 class=%s amount=%d", callers.ErrIdempotencyConflict, idem, existing.Class, existing.Amount)
			}
			out = existing
			return nil
		}
		if err != nil {
			return fmt.Errorf("写入预留行: %w", err)
		}

		// 惰性建周期行（设计 §2.3 表「重置」行）；旧周期行保留供审计。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO caller_usage_periods
				(caller_id, period_start, kind, used, reserved, overage, updated_at)
			VALUES ($1, $2, $3, 0, 0, 0, $4)
			ON CONFLICT (caller_id, period_start, kind) DO NOTHING`,
			callerID, periodStart, kind, now.UnixMilli(),
		); err != nil {
			return fmt.Errorf("建周期用量行: %w", err)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE caller_usage_periods
			SET reserved = reserved + $4, updated_at = $5
			WHERE caller_id = $1 AND period_start = $2 AND kind = $3
			  AND overage = 0
			  AND used + reserved + $4 <= $6`,
			callerID, periodStart, kind, amount, now.UnixMilli(), limit,
		)
		if err != nil {
			return fmt.Errorf("占用周期额度: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取占用结果: %w", err)
		}
		if affected == 0 {
			// 同一条 UPDATE 条件挡两种拒绝，恢复方向却相反：`overage > 0` 等管理员
			// 核对清零，额度不足等下一周期。回读一次把恢复时间说准——给错方向的
			// `Retry-After` 比不给更误导（契约 §8 只要求后一种给时间）。
			var overage int64
			if err := tx.QueryRowContext(ctx, `
				SELECT overage FROM caller_usage_periods
				WHERE caller_id = $1 AND period_start = $2 AND kind = $3`,
				callerID, periodStart, kind,
			).Scan(&overage); err != nil {
				return fmt.Errorf("读取周期用量行: %w", err)
			}
			if overage > 0 {
				return &callers.QuotaExhaustedError{
					Reason: fmt.Sprintf("账本 %s 有未核对的超额 %d", kind, overage),
				}
			}
			return &callers.QuotaExhaustedError{
				Reason:  fmt.Sprintf("账本 %s 周期额度不足", kind),
				RetryAt: nextPeriod,
			}
		}

		out = callers.Reservation{
			ID:          id,
			CallerID:    callerID,
			Class:       class,
			PeriodStart: periodStart,
			Amount:      amount,
			Status:      callers.ReservationPending,
		}
		return nil
	})
	if err != nil {
		return callers.Reservation{}, err
	}
	return out, nil
}

// Commit 用实际用量结算预留：`pending → committed`，同一条 UPDATE 里释放预留并
// 累加已用；`actual > amount` 时如实记 `overage`（不截断、不回滚调用）。
// 重复提交返回首次结果（用量不一致即 `ErrIdempotencyConflict`）；对 `released` /
// `reconciled` 返回 `ErrReservationSettled`。
// Commit settles with real usage, records overage honestly, and is idempotent.
func (s *quotaStore) Commit(ctx context.Context, callerID, reservationID string, usage callers.Usage) (callers.Settlement, error) {
	if s == nil || s.db == nil {
		return callers.Settlement{}, errQuotaStoreUnconfigured
	}
	if callerID == "" || reservationID == "" {
		return callers.Settlement{}, errQuotaInvalidArgument
	}
	kind, err := parseReservationKind(reservationID)
	if err != nil {
		return callers.Settlement{}, err
	}
	actual, err := actualFor(kind, usage)
	if err != nil {
		return callers.Settlement{}, err
	}

	var out callers.Settlement
	err = s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.db.clock().UTC()
		row, err := lockReservation(ctx, tx, reservationID, callerID)
		if err != nil {
			return err
		}
		switch row.status {
		case callers.ReservationPending:
			// 继续结算。
		case callers.ReservationCommitted:
			if row.actual != actual {
				return fmt.Errorf("%w: 预留 %s 已按 %d 提交", callers.ErrIdempotencyConflict, reservationID, row.actual)
			}
			out = row.settlement()
			return nil
		case callers.ReservationReleased, callers.ReservationReconciled:
			return fmt.Errorf("%w: 预留 %s 状态 %s", callers.ErrReservationSettled, reservationID, row.status)
		default:
			return fmt.Errorf("%w: 预留 %s 状态 %q", errQuotaUsageRowMissing, reservationID, string(row.status))
		}

		overage := overageFor(actual, row.amount)
		if err := applySettlement(ctx, tx, row, kind, actual, overage, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE quota_reservations
			SET state = 'committed', actual = $2, overage = $3, settled_at = $4
			WHERE reservation_id = $1`,
			reservationID, actual, overage, now.UnixMilli(),
		); err != nil {
			return fmt.Errorf("标记预留已结算: %w", err)
		}
		row.status = callers.ReservationCommitted
		row.actual = actual
		row.overage = overage
		out = row.settlement()
		return nil
	})
	if err != nil {
		return callers.Settlement{}, err
	}
	return out, nil
}

// Release 只用于确认零消耗：`pending → released` 并归还全部预留；其余状态为空操作
// （设计「`committed` 后调用为空操作」逐字）。理由落 `reason` 列供审计。
// Release returns a reservation proven unconsumed; other states are a no-op.
func (s *quotaStore) Release(ctx context.Context, callerID, reservationID string, reason callers.ReleaseReason) error {
	if s == nil || s.db == nil {
		return errQuotaStoreUnconfigured
	}
	if callerID == "" || reservationID == "" {
		return errQuotaInvalidArgument
	}
	if !reason.Valid() {
		return fmt.Errorf("%w: 未知释放理由 %q", errQuotaInvalidArgument, string(reason))
	}
	kind, err := parseReservationKind(reservationID)
	if err != nil {
		return err
	}

	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.db.clock().UTC()
		row, err := lockReservation(ctx, tx, reservationID, callerID)
		if err != nil {
			return err
		}
		if row.status != callers.ReservationPending {
			return nil // 空操作：终态不再迁移，也不重复归还额度
		}
		if err := releaseQuota(ctx, tx, row, kind, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE quota_reservations
			SET state = 'released', reason = $2, settled_at = $3
			WHERE reservation_id = $1`,
			reservationID, string(reason), now.UnixMilli(),
		); err != nil {
			return fmt.Errorf("标记预留已释放: %w", err)
		}
		return nil
	})
}

// Reconcile 只由管理面发起：把结局未知的 `pending` 收口为 `reconciled`。
// `committed` 按 `actual` 记账（大于预留量时记 `overage`，不截断），`released`
// 按零消耗归还；理由必填并落 `reason`，管理审计由调用方在预写阶段完成。
// Reconcile closes an unknown reservation from the admin surface only.
func (s *quotaStore) Reconcile(ctx context.Context, reservationID string, outcome callers.ReconcileOutcome, actual int64, reason string) (callers.Settlement, error) {
	if s == nil || s.db == nil {
		return callers.Settlement{}, errQuotaStoreUnconfigured
	}
	if reservationID == "" || !outcome.Valid() || actual < 0 || strings.TrimSpace(reason) == "" {
		return callers.Settlement{}, errQuotaInvalidArgument
	}
	kind, err := parseReservationKind(reservationID)
	if err != nil {
		return callers.Settlement{}, err
	}

	var out callers.Settlement
	err = s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.db.clock().UTC()
		row, err := lockReservation(ctx, tx, reservationID, "")
		if err != nil {
			return err
		}
		if row.status != callers.ReservationPending {
			return fmt.Errorf("%w: 预留 %s 状态 %s", callers.ErrReservationSettled, reservationID, row.status)
		}

		settledActual, overage := int64(0), int64(0)
		if outcome == callers.ReconcileCommitted {
			settledActual = actual
			overage = overageFor(actual, row.amount)
			if err := applySettlement(ctx, tx, row, kind, settledActual, overage, now); err != nil {
				return err
			}
		} else if err := releaseQuota(ctx, tx, row, kind, now); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE quota_reservations
			SET state = 'reconciled', actual = $2, overage = $3, reason = $4, settled_at = $5
			WHERE reservation_id = $1`,
			reservationID, settledActual, overage, reason, now.UnixMilli(),
		); err != nil {
			return fmt.Errorf("标记预留已核对: %w", err)
		}
		row.status = callers.ReservationReconciled
		row.actual = settledActual
		row.overage = overage
		out = row.settlement()
		return nil
	})
	if err != nil {
		return callers.Settlement{}, err
	}
	return out, nil
}

// ClearOverage 清零某周期的超限计数：`overage > 0` 期间新预留一律被拒，管理员核对
// 后调用本方法并写管理审计才恢复。`periodStart` 为 0 表示当前周期，非 0 时把该
// 瞬时折算到所属周期的起点（管理面拿到的是"某个月里的某一刻"，不该要求调用方
// 自己算出本地 00:00）。
// ClearOverage zeroes the period's overage; a zero value means now and any instant is
// snapped to its period start.
func (s *quotaStore) ClearOverage(ctx context.Context, callerID string, periodStart int64, reason string) error {
	if s == nil || s.db == nil {
		return errQuotaStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" || strings.TrimSpace(reason) == "" {
		return errQuotaInvalidArgument
	}

	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.db.clock().UTC()
		at := now
		if periodStart != 0 {
			at = time.UnixMilli(periodStart).UTC()
		}
		start, err := resolveAdminPeriodStart(ctx, tx, callerID, at)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE caller_usage_periods
			SET overage = 0, updated_at = $3
			WHERE caller_id = $1 AND period_start = $2 AND overage > 0`,
			callerID, start, now.UnixMilli(),
		); err != nil {
			return fmt.Errorf("清零超限计数: %w", err)
		}
		return nil
	})
}

// SetLimit 写入调用方限额。`caller_limits.caller_id` 是外键，而管理面可能先配限额
// 再建调用方行，因此同一事务内先 `ON CONFLICT DO NOTHING` 补调用方行（R28.5）。
// SetLimit upserts the limit and, in the same transaction, backfills the caller row
// the foreign key needs.
func (s *quotaStore) SetLimit(ctx context.Context, callerID string, limit callers.Limit) error {
	if s == nil || s.db == nil {
		return errQuotaStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" {
		return errQuotaInvalidArgument
	}
	normalized, err := limit.Normalize()
	if err != nil {
		return err
	}

	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := s.db.clock().UTC().UnixMilli()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO callers (caller_id, kind, name, project, status, created_at)
			VALUES ($1, 'service', $1, '', 'active', $2)
			ON CONFLICT DO NOTHING`,
			callerID, now,
		); err != nil {
			return fmt.Errorf("补齐调用方行: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO caller_limits
				(caller_id, qps, burst, max_concurrent, tasks_per_period, tokens_per_period,
				 ingest_bytes_per_period, period, timezone, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (caller_id) DO UPDATE
			SET qps = EXCLUDED.qps,
			    burst = EXCLUDED.burst,
			    max_concurrent = EXCLUDED.max_concurrent,
			    tasks_per_period = EXCLUDED.tasks_per_period,
			    tokens_per_period = EXCLUDED.tokens_per_period,
			    ingest_bytes_per_period = EXCLUDED.ingest_bytes_per_period,
			    period = EXCLUDED.period,
			    timezone = EXCLUDED.timezone,
			    updated_at = EXCLUDED.updated_at`,
			callerID, normalized.QPS, normalized.Burst, normalized.MaxConcurrent,
			normalized.TasksPerPeriod, normalized.TokensPerPeriod, normalized.IngestBytesPerPeriod,
			string(normalized.Period), normalized.Timezone, now,
		); err != nil {
			return fmt.Errorf("写入调用方限额: %w", err)
		}
		return nil
	})
}

// MaxConcurrent 读该调用方的并发上限投影（图 6 [3] 的限额来源）。仓储只回答
// "有没有这一行、值是多少"：`ok=false` 表示没有 `caller_limits` 行，是否回退到实例
// 级上限由装配层裁决（`app` 的并发上限缓存）；读失败按错误上抛，由调用方失败关闭。
// **不**在这里做缺省填充——把"没配置"当成某个数字会把裁决藏进存储层。
// MaxConcurrent projects caller_limits.max_concurrent for admission stage [3]. A
// missing row reports ok=false; the assembly layer decides the fallback, and storage
// failures surface as errors instead of being defaulted here.
func (s *quotaStore) MaxConcurrent(ctx context.Context, callerID string) (int, bool, error) {
	if s == nil || s.db == nil {
		return 0, false, errQuotaStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" {
		return 0, false, errQuotaInvalidArgument
	}
	var limit int
	err := s.db.pool.QueryRowContext(ctx,
		`SELECT max_concurrent FROM caller_limits WHERE caller_id = $1`, callerID).Scan(&limit)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("读取调用方并发上限: %w", err)
	}
	return limit, true, nil
}

// IngressBudget 读该调用方的入口速率覆盖（`caller_limits.qps` / `burst`，图 6 [1] /
// C-F01）。仓储只回答"有没有生效的覆盖、值是多少"：没有行或两列都为 0 报
// found=false（契约 `schema.pg.sql` 的 `0 = 不限` 指不额外限制），与类别预算如何
// 合并由 `callers` 的 `tightenIngressBudget` 裁决；负值由表上 `CHECK (>= 0)` 挡住，
// 真读到就按数据缺陷报错，**不得**当成 0 或忽略；读失败按错误上抛，由入口判定
// 失败关闭（中间件回 500 而不是 429）。
// IngressBudget projects caller_limits.qps/burst for admission stage [2]. A missing
// row or two zero columns report found=false; negatives are data defects and read
// failures surface as errors so admission fails closed.
func (s *quotaStore) IngressBudget(ctx context.Context, callerID string) (callers.IngressBudget, bool, error) {
	if s == nil || s.db == nil {
		return callers.IngressBudget{}, false, errQuotaStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" {
		return callers.IngressBudget{}, false, errQuotaInvalidArgument
	}
	var (
		qps   float64
		burst int
	)
	err := s.db.pool.QueryRowContext(ctx,
		`SELECT qps::float8, burst FROM caller_limits WHERE caller_id = $1`, callerID).
		Scan(&qps, &burst)
	if errors.Is(err, sql.ErrNoRows) {
		return callers.IngressBudget{}, false, nil
	}
	if err != nil {
		return callers.IngressBudget{}, false, fmt.Errorf("读取调用方入口速率: %w", err)
	}
	if qps < 0 || burst < 0 {
		return callers.IngressBudget{}, false,
			fmt.Errorf("%w: 调用方入口速率不得为负", errQuotaInvalidArgument)
	}
	if qps == 0 && burst == 0 {
		return callers.IngressBudget{}, false, nil
	}
	return callers.IngressBudget{QPS: qps, Burst: burst}, true, nil
}

// ListPending 列出该调用方结局未知的预留，供管理面核对；**不**自动释放、**不**按
// 零结清（设计「未知用量」行）。
// ListPending lists unresolved reservations for the admin surface.
func (s *quotaStore) ListPending(ctx context.Context, callerID string) ([]callers.Reservation, error) {
	if s == nil || s.db == nil {
		return nil, errQuotaStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" {
		return nil, errQuotaInvalidArgument
	}
	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT reservation_id, class, period_start, amount, state
		FROM quota_reservations
		WHERE caller_id = $1 AND state = 'pending'
		ORDER BY created_at, reservation_id`, callerID)
	if err != nil {
		return nil, fmt.Errorf("查询未结算预留: %w", err)
	}
	defer rows.Close()

	var out []callers.Reservation
	for rows.Next() {
		var (
			id      string
			class   string
			start   int64
			amount  int64
			status  string
			pending callers.Reservation
		)
		if err := rows.Scan(&id, &class, &start, &amount, &status); err != nil {
			return nil, fmt.Errorf("读取未结算预留: %w", err)
		}
		pending = callers.Reservation{
			ID:          id,
			CallerID:    callerID,
			Class:       callers.RequestClass(class),
			PeriodStart: start,
			Amount:      amount,
			Status:      callers.ReservationStatus(status),
		}
		out = append(out, pending)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历未结算预留: %w", err)
	}
	return out, nil
}

// Get 按 `(caller_id, reservation_id)` 返回预留行（不锁行）：归属不匹配与不存在
// 一律 `ErrNotFound`。管理面据此在写审计记录**之前**确认归属与当前状态。
// Get resolves a reservation by owner without locking; a mismatch is ErrNotFound.
func (s *quotaStore) Get(ctx context.Context, callerID, reservationID string) (callers.Reservation, error) {
	if s == nil || s.db == nil {
		return callers.Reservation{}, errQuotaStoreUnconfigured
	}
	if strings.TrimSpace(callerID) == "" || strings.TrimSpace(reservationID) == "" {
		return callers.Reservation{}, errQuotaInvalidArgument
	}
	var (
		out    callers.Reservation
		class  string
		status string
	)
	err := s.db.pool.QueryRowContext(ctx, `
		SELECT reservation_id, caller_id, class, period_start, amount, state
		FROM quota_reservations
		WHERE reservation_id = $1 AND caller_id = $2`, reservationID, callerID,
	).Scan(&out.ID, &out.CallerID, &class, &out.PeriodStart, &out.Amount, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return callers.Reservation{}, fmt.Errorf("%w: 调用方 %s 没有预留 %s", callers.ErrNotFound, callerID, reservationID)
	}
	if err != nil {
		return callers.Reservation{}, fmt.Errorf("查询预留: %w", err)
	}
	out.Class = callers.RequestClass(class)
	out.Status = callers.ReservationStatus(status)
	return out, nil
}

// reservationRow 是 `quota_reservations` 的一行锁读结果。
// reservationRow is the locked view of one reservation row.
type reservationRow struct {
	id          string
	callerID    string
	class       callers.RequestClass
	periodStart int64
	amount      int64
	actual      int64
	overage     int64
	status      callers.ReservationStatus
}

// settlement 把锁读行转成对外结算结果。
// settlement converts the locked row into the public settlement view.
func (r reservationRow) settlement() callers.Settlement {
	return callers.Settlement{
		ReservationID: r.id,
		CallerID:      r.callerID,
		Class:         r.class,
		State:         r.status,
		PeriodStart:   r.periodStart,
		Amount:        r.amount,
		Actual:        r.actual,
		Overage:       r.overage,
	}
}

// reservationPeriod 读取该调用方的周期额度与周期起点。限额行缺失、对应账本额度为
// 零一律 `ErrQuotaExhausted`（fail-closed，**不得**退化成不限量）；周期或时区非法
// 按配置缺陷返回错误。
// reservationPeriod resolves the limit and the reservation's period start; a missing
// row or a zero limit fails closed.
// 返回限额、本周期起点（Unix 毫秒）与下一周期起点（零值 = 无法解析，恢复无日历口径）。
// It returns the limit, this period's start and the next period's start; a zero next
// start means the calendar offers no recovery instant.
func reservationPeriod(ctx context.Context, tx *sql.Tx, callerID, kind string, now time.Time) (int64, int64, time.Time, error) {
	var (
		tasks       int64
		tokens      int64
		ingestBytes int64
		period      string
		timezone    string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT tasks_per_period, tokens_per_period, ingest_bytes_per_period, period, timezone
		FROM caller_limits
		WHERE caller_id = $1`, callerID,
	).Scan(&tasks, &tokens, &ingestBytes, &period, &timezone)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, time.Time{}, &callers.QuotaExhaustedError{
			Reason: fmt.Sprintf("调用方 %s 没有限额行", callerID),
		}
	}
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("读取调用方限额: %w", err)
	}

	var limit int64
	switch kind {
	case usageKindTasks:
		limit = tasks
	case usageKindTokens:
		limit = tokens
	case usageKindIngestBytes:
		limit = ingestBytes
	default:
		return 0, 0, time.Time{}, fmt.Errorf("%w: 未知账本 %q", errQuotaInvalidArgument, kind)
	}

	start, err := callers.PeriodStart(callers.Period(period), timezone, now)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	next, err := callers.NextPeriodStart(callers.Period(period), timezone, now)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	if limit <= 0 {
		// 额度为零：等到下一周期也不会恢复，恢复靠管理员调额——RetryAt 保持零值。
		return 0, 0, time.Time{}, &callers.QuotaExhaustedError{
			Reason: fmt.Sprintf("账本 %s 的周期额度为 %d", kind, limit),
		}
	}
	return limit, start.UnixMilli(), next, nil
}

// reservationByIdem 回读同一幂等键的既有预留。
// reservationByIdem reads the reservation already bound to an idempotency key.
func reservationByIdem(ctx context.Context, tx *sql.Tx, callerID, idem string) (callers.Reservation, error) {
	var (
		out    callers.Reservation
		class  string
		status string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT reservation_id, class, period_start, amount, state
		FROM quota_reservations
		WHERE caller_id = $1 AND idempotency_key = $2`,
		callerID, idem,
	).Scan(&out.ID, &class, &out.PeriodStart, &out.Amount, &status)
	if err != nil {
		return callers.Reservation{}, fmt.Errorf("读取同键预留: %w", err)
	}
	out.CallerID = callerID
	out.Class = callers.RequestClass(class)
	out.Status = callers.ReservationStatus(status)
	return out, nil
}

// lockReservation 按 `(reservation_id[, caller_id])` 锁读预留行：`callerID` 为空
// 表示管理面路径（`Reconcile`）。命中不到一律 `ErrNotFound`——归属不匹配与不存在
// 不区分，避免泄露预留是否存在。
// lockReservation locks the row; an empty callerID is the admin path, and a missing
// or foreign row returns ErrNotFound either way.
func lockReservation(ctx context.Context, tx *sql.Tx, reservationID, callerID string) (reservationRow, error) {
	query := `
		SELECT reservation_id, caller_id, class, period_start, amount,
		       COALESCE(actual, 0), overage, state
		FROM quota_reservations
		WHERE reservation_id = $1`
	args := []any{reservationID}
	if callerID != "" {
		query += ` AND caller_id = $2`
		args = append(args, callerID)
	}
	query += ` FOR UPDATE`

	var (
		row    reservationRow
		class  string
		status string
	)
	err := tx.QueryRowContext(ctx, query, args...).Scan(
		&row.id, &row.callerID, &class, &row.periodStart, &row.amount,
		&row.actual, &row.overage, &status,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return reservationRow{}, fmt.Errorf("%w: 预留 %s", callers.ErrNotFound, reservationID)
	}
	if err != nil {
		return reservationRow{}, fmt.Errorf("锁读预留: %w", err)
	}
	row.class = callers.RequestClass(class)
	row.status = callers.ReservationStatus(status)
	return row, nil
}

// applySettlement 在周期行上释放预留并累加已用（含超限），返回行缺失即报错。
// applySettlement moves reserved to used (plus overage) on the period row.
func applySettlement(ctx context.Context, tx *sql.Tx, row reservationRow, kind string, actual, overage int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE caller_usage_periods
		SET reserved = reserved - $4,
		    used = used + $5,
		    overage = overage + $6,
		    updated_at = $7
		WHERE caller_id = $1 AND period_start = $2 AND kind = $3`,
		row.callerID, row.periodStart, kind, row.amount, actual, overage, now.UnixMilli(),
	)
	return affectedOnce(res, err, "结算周期用量")
}

// releaseQuota 只归还预留、不增加已用（确认零消耗）。
// releaseQuota returns the reservation without booking any usage.
func releaseQuota(ctx context.Context, tx *sql.Tx, row reservationRow, kind string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE caller_usage_periods
		SET reserved = reserved - $4, updated_at = $5
		WHERE caller_id = $1 AND period_start = $2 AND kind = $3`,
		row.callerID, row.periodStart, kind, row.amount, now.UnixMilli(),
	)
	return affectedOnce(res, err, "释放周期预留")
}

// affectedOnce 要求语句恰好命中一行：0 行说明账本行缺失或已经被重复扣减，
// 多行说明主键被破坏——两种情况都按数据不一致回滚。
// affectedOnce insists on exactly one row, treating anything else as inconsistent.
func affectedOnce(res sql.Result, err error, action string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: 读取影响行数: %w", action, err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: %s 影响 %d 行", errQuotaUsageRowMissing, action, affected)
	}
	return nil
}

// runTaskAmount 是每次 Run 准入记入 `tasks` 账本的数量：一次逻辑准入算一次任务，
// 与装配层 `runQuotaTasksPerRun` 同值。两个包各持一个字面量是分层使然（`postgres`
// 不得 import `app`），两者都是契约「一次 Run 一次任务」的落地，改动须同批。
// runTaskAmount mirrors app's runQuotaTasksPerRun; changing one requires the other.
const runTaskAmount = 1

// runTaskChargeIdempotencyKey 与装配层 `runQuotaIdempotencyKey` **逐字同形**
// （`run:<run_id>:admit`）：同一 Run 的两条记账路径若用不同键，会被记两次。
// The key must stay byte-identical to the assembly's runQuotaIdempotencyKey.
func runTaskChargeIdempotencyKey(runID string) string { return "run:" + runID + ":admit" }

// chargeRunTaskQuotaTx 在调用方**自己的事务**里完成一次 Run 级任务次数记账
// （预留 + 结算 `pending → committed`；模块卡 §2.3.2 的 Q21 接缝 / T012）：它接收
// `*sql.Tx` 而不是开自己的事务，因此与权威准入、Run 行与 run 租约同生共死——任一步
// 失败整笔回滚，零占用。`core.RunQuota` 的公开接口不变，本路径**不经**它。
// chargeRunTaskQuotaTx reserves and settles one run-level task charge inside the
// caller's transaction, so the charge commits or rolls back with admission itself.
//
// 判据与 `Reserve` + `Commit` 同口径（照抄而非复用，是为了不把"记账"拆成两个事务）：
//   - 限额行缺失 / 额度为零 → `QuotaExhaustedError`（fail-closed，**不**退化成不限量）；
//   - 幂等键 `(caller_id, idempotency_key)` 命中：已 committed 且参数一致 → 返回原
//     预留（重放）；pending → 按"存量未收口"收口（见 `replayRunTaskChargeTx` 的说明，
//     不退款、不重试、只结算一次）；released / reconciled → `ErrReservationSettled`
//     （**不**把已结算的预留当成可再花一次）；
//   - 超额（overage > 0）→ 等管理员核对，`RetryAt` 保持零值；额度不足 → 带下一周期。
//
// The helper mirrors Reserve+Commit's judgement without splitting them into two
// transactions, and it never treats a settled reservation as spendable again.
func chargeRunTaskQuotaTx(ctx context.Context, tx *sql.Tx, callerID, runID string, now time.Time) (string, error) {
	if tx == nil {
		return "", errors.New("postgres: 任务次数记账缺少事务")
	}
	if strings.TrimSpace(callerID) == "" || strings.TrimSpace(runID) == "" {
		return "", fmt.Errorf("%w: 任务次数记账缺少调用方或 Run 标识", errQuotaInvalidArgument)
	}
	limit, periodStart, nextPeriod, err := reservationPeriod(ctx, tx, callerID, usageKindTasks, now)
	if err != nil {
		return "", err
	}
	idem := runTaskChargeIdempotencyKey(runID)
	id, err := newReservationID(usageKindTasks)
	if err != nil {
		return "", err
	}

	var inserted string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO quota_reservations
			(reservation_id, caller_id, idempotency_key, class, period_start, amount, state, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7)
		ON CONFLICT (caller_id, idempotency_key) DO NOTHING
		RETURNING reservation_id`,
		id, callerID, idem, string(callers.ClassRun), periodStart, runTaskAmount, now.UnixMilli(),
	).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		return replayRunTaskChargeTx(ctx, tx, callerID, idem, runID, now)
	}
	if err != nil {
		return "", fmt.Errorf("写入任务次数预留（run_id=%s）: %w", runID, err)
	}

	// 惰性建周期行（与 `Reserve` 同一手法）：旧周期行保留供审计。
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO caller_usage_periods
			(caller_id, period_start, kind, used, reserved, overage, updated_at)
		VALUES ($1, $2, $3, 0, 0, 0, $4)
		ON CONFLICT (caller_id, period_start, kind) DO NOTHING`,
		callerID, periodStart, usageKindTasks, now.UnixMilli(),
	); err != nil {
		return "", fmt.Errorf("建周期用量行（run_id=%s）: %w", runID, err)
	}

	// 条件累加：`overage = 0 AND used + reserved + amount <= limit` 在同一条 UPDATE
	// 里求值，并发准入不会双双通过（与 `Reserve` 同一逆改点）。
	res, err := tx.ExecContext(ctx, `
		UPDATE caller_usage_periods
		SET reserved = reserved + $4, updated_at = $5
		WHERE caller_id = $1 AND period_start = $2 AND kind = $3
		  AND overage = 0
		  AND used + reserved + $4 <= $6`,
		callerID, periodStart, usageKindTasks, runTaskAmount, now.UnixMilli(), limit,
	)
	if err != nil {
		return "", fmt.Errorf("占用任务次数额度（run_id=%s）: %w", runID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("读取任务次数占用结果（run_id=%s）: %w", runID, err)
	}
	if affected == 0 {
		var overage int64
		if err := tx.QueryRowContext(ctx, `
			SELECT overage FROM caller_usage_periods
			WHERE caller_id = $1 AND period_start = $2 AND kind = $3`,
			callerID, periodStart, usageKindTasks,
		).Scan(&overage); err != nil {
			return "", fmt.Errorf("读取任务次数用量行（run_id=%s）: %w", runID, err)
		}
		if overage > 0 {
			return "", &callers.QuotaExhaustedError{
				Reason: fmt.Sprintf("账本 %s 有未核对的超额 %d", usageKindTasks, overage),
			}
		}
		return "", &callers.QuotaExhaustedError{
			Reason:  fmt.Sprintf("账本 %s 周期额度不足", usageKindTasks),
			RetryAt: nextPeriod,
		}
	}

	// 同一事务内立刻结算：先归还原预留、累加已用，再把预留行翻成 committed。
	// 两步都要求恰好命中一行——行缺失或重复扣减按数据不一致回滚（`affectedOnce`）。
	row := reservationRow{
		id:          id,
		callerID:    callerID,
		class:       callers.ClassRun,
		periodStart: periodStart,
		amount:      runTaskAmount,
		status:      callers.ReservationPending,
	}
	if err := applySettlement(ctx, tx, row, usageKindTasks, runTaskAmount, 0, now); err != nil {
		return "", fmt.Errorf("结算任务次数（run_id=%s）: %w", runID, err)
	}
	settled, err := tx.ExecContext(ctx, `
		UPDATE quota_reservations
		SET state = 'committed', actual = $2, overage = 0, settled_at = $3
		WHERE reservation_id = $1 AND state = 'pending'`,
		id, runTaskAmount, now.UnixMilli())
	if err := affectedOnce(settled, err, "提交任务次数预留（run_id="+runID+"）"); err != nil {
		return "", err
	}
	return id, nil
}

// replayRunTaskChargeTx 处理幂等键命中：只有"已 committed 且参数一致"或"存量 pending
// 可收口"两种情形可以返回原预留，其余状态一律失败关闭。`run_id` 只用于错误文案——
// 归属回填由 `backfillReservationRun` 在 Run 行插入后独立校验。
//
// 为什么 pending 不是拒绝而是**就地收口**：本事务内的记账要么提交成 committed、要么
// 整笔回滚、要么让同键的并发准入在 `INSERT … ON CONFLICT` 上等待并在对方提交后读到
// committed——US-24 的路径**产生不出**一条已提交的 pending 行。能观察到它，只有两种
// 来路：Q21 之前"预留事务 + 结算事务"两笔提交留下的存量行，或管理面 / 测试夹具手工
// 种下的行。已接受的崩溃恢复契约要求这类行在**同一准入键**的下一次准入时收敛为
// committed（恰好一次、不退款、不重试），而不是把整个调用方永久卡死。收口只碰它自己
// 的周期行（`period_start` 取自存量行），因此跨周期也不会记错账；预留行先用
// `WHERE state='pending'` 的条件更新上锁，并发收口者只有一个能命中，另一个整笔回滚
// （不产生第二次扣减）。
// A committed pending row cannot come from this path: it is either pre-Q21 legacy or a
// seeded fixture, so the same idempotency key converges by settling it exactly once on its
// own period — never a refund, never a retry, and never a permanent caller lockout.
func replayRunTaskChargeTx(ctx context.Context, tx *sql.Tx, callerID, idem, runID string, now time.Time) (string, error) {
	var (
		existingID  string
		class       string
		amount      int64
		state       string
		periodStart int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT reservation_id, class, amount, state, period_start
		FROM quota_reservations
		WHERE caller_id = $1 AND idempotency_key = $2`,
		callerID, idem,
	).Scan(&existingID, &class, &amount, &state, &periodStart)
	if err != nil {
		return "", fmt.Errorf("读取同键任务预留（run_id=%s）: %w", runID, err)
	}
	if class != string(callers.ClassRun) || amount != runTaskAmount {
		return "", fmt.Errorf("%w: 键 %q 已用于 class=%s amount=%d",
			callers.ErrIdempotencyConflict, idem, class, amount)
	}
	switch callers.ReservationStatus(state) {
	case callers.ReservationCommitted:
		return existingID, nil
	case callers.ReservationPending:
		return settleLegacyRunTaskChargeTx(ctx, tx, callerID, existingID, periodStart, amount, runID, now)
	default:
		return "", fmt.Errorf("%w: 预留 %s 已结算为 %q",
			callers.ErrReservationSettled, existingID, state)
	}
}

// settleLegacyRunTaskChargeTx 把一条存量 pending 预留就地结算为 committed：先按
// `state='pending'` 条件更新预留行（并发收口者在此串行化，落败方整笔回滚），再在它
// **自己的周期行**上做与正常路径完全相同的结算（`reserved - amount`、`used + actual`）。
// 顺序与 `chargeRunTaskQuotaTx` 的收尾一致，用的是同一个 `applySettlement`——两条路径
// 的记账口径因此不会分叉。
// settleLegacyRunTaskChargeTx settles a leftover pending reservation in place, on its own
// period, through the same applySettlement used by the normal path.
func settleLegacyRunTaskChargeTx(ctx context.Context, tx *sql.Tx, callerID, reservationID string,
	periodStart, amount int64, runID string, now time.Time,
) (string, error) {
	settled, err := tx.ExecContext(ctx, `
		UPDATE quota_reservations
		SET state = 'committed', actual = $2, overage = 0, settled_at = $3
		WHERE reservation_id = $1 AND state = 'pending'`,
		reservationID, amount, now.UnixMilli())
	if err := affectedOnce(settled, err, "收口存量任务次数预留（run_id="+runID+"）"); err != nil {
		return "", err
	}
	row := reservationRow{
		id:          reservationID,
		callerID:    callerID,
		class:       callers.ClassRun,
		periodStart: periodStart,
		amount:      amount,
		status:      callers.ReservationPending,
	}
	if err := applySettlement(ctx, tx, row, usageKindTasks, amount, 0, now); err != nil {
		return "", fmt.Errorf("结算存量任务次数（run_id=%s）: %w", runID, err)
	}
	return reservationID, nil
}

// resolveAdminPeriodStart 把管理面给出的瞬时折算成周期起点；限额行缺失时按 DDL
// 缺省（day / UTC）解释，**不**把"没有限额"当成"没有这个调用方"——调用方是否
// 存在由管理面的 `LookupCaller` 先行判定，两者是不同的 404 语义。
// resolveAdminPeriodStart snaps an instant to its period start and tolerates a
// missing limit row with the documented day/UTC defaults.
func resolveAdminPeriodStart(ctx context.Context, tx *sql.Tx, callerID string, at time.Time) (int64, error) {
	period, timezone := callers.PeriodDay, callers.DefaultTimezone
	err := tx.QueryRowContext(ctx, `
		SELECT period, timezone FROM caller_limits WHERE caller_id = $1`, callerID,
	).Scan(&period, &timezone)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("读取调用方周期: %w", err)
	}
	start, err := callers.PeriodStart(period, timezone, at)
	if err != nil {
		return 0, err
	}
	return start.UnixMilli(), nil
}

// ledgerFor 把类别 + 成本映射成账本维度与预留量：**恰好一个**非零维度决定记哪本
// 账；类别与维度不相容一律拒绝（R28.1：ingest 既可记 Token 也可记字节）。
// ledgerFor maps class and cost onto one ledger dimension; it rejects ambiguity.
func ledgerFor(class callers.RequestClass, cost callers.Cost) (string, int64, error) {
	if cost.Tasks < 0 || cost.Tokens < 0 || cost.Bytes < 0 {
		return "", 0, fmt.Errorf("%w: 成本不得为负", errQuotaInvalidArgument)
	}
	set := 0
	for _, v := range []int64{cost.Tasks, cost.Tokens, cost.Bytes} {
		if v > 0 {
			set++
		}
	}
	if set != 1 {
		return "", 0, fmt.Errorf("%w: 成本必须恰好设置一个维度，实际 %d 个", errQuotaInvalidArgument, set)
	}

	switch {
	case cost.Tasks > 0:
		if class != callers.ClassRun {
			return "", 0, fmt.Errorf("%w: 任务次数只配 ClassRun", errQuotaInvalidArgument)
		}
		return usageKindTasks, cost.Tasks, nil
	case cost.Bytes > 0:
		if class != callers.ClassIngest {
			return "", 0, fmt.Errorf("%w: 字节只配 ClassIngest", errQuotaInvalidArgument)
		}
		return usageKindIngestBytes, cost.Bytes, nil
	default:
		if class == callers.ClassRun {
			return "", 0, fmt.Errorf("%w: ClassRun 不按 Token 记账", errQuotaInvalidArgument)
		}
		return usageKindTokens, cost.Tokens, nil
	}
}

// actualFor 取该账本维度的实际用量；其余维度必须为零，否则视为调用方串账。
// actualFor picks the ledger's own dimension and rejects cross-dimension values.
func actualFor(kind string, usage callers.Usage) (int64, error) {
	if usage.Tasks < 0 || usage.Tokens < 0 || usage.Bytes < 0 {
		return 0, fmt.Errorf("%w: 用量不得为负", errQuotaInvalidArgument)
	}
	var actual int64
	switch kind {
	case usageKindTasks:
		actual = usage.Tasks
		if usage.Tokens > 0 || usage.Bytes > 0 {
			return 0, fmt.Errorf("%w: 任务账本只接受 Tasks 维度", errQuotaInvalidArgument)
		}
	case usageKindTokens:
		actual = usage.Tokens
		if usage.Tasks > 0 || usage.Bytes > 0 {
			return 0, fmt.Errorf("%w: Token 账本只接受 Tokens 维度", errQuotaInvalidArgument)
		}
	case usageKindIngestBytes:
		actual = usage.Bytes
		if usage.Tasks > 0 || usage.Tokens > 0 {
			return 0, fmt.Errorf("%w: 字节账本只接受 Bytes 维度", errQuotaInvalidArgument)
		}
	default:
		return 0, fmt.Errorf("%w: 未知账本 %q", errQuotaInvalidArgument, kind)
	}
	return actual, nil
}

// overageFor 计算超预留：只记正向差额，**不得**把实际用量截成预留量。
// overageFor records the positive excess only; actual is never truncated.
func overageFor(actual, amount int64) int64 {
	if actual > amount {
		return actual - amount
	}
	return 0
}

// newReservationID 生成 `qres_<kind>_<26 位 Crockford>`；随机源失败即错误。
// newReservationID mints the opaque reservation id that carries the ledger kind.
func newReservationID(kind string) (string, error) {
	var buf [reservationPayloadLength]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成预留标识随机源: %w", err)
	}
	var b strings.Builder
	b.Grow(len(reservationIDPrefix) + len(kind) + 1 + reservationPayloadLength)
	b.WriteString(reservationIDPrefix)
	b.WriteString(kind)
	b.WriteByte('_')
	for _, v := range buf {
		b.WriteByte(crockfordAlphabet[int(v)%len(crockfordAlphabet)])
	}
	return b.String(), nil
}

// parseReservationKind 从预留标识解析账本维度：前缀、kind 与 26 位载荷逐项校验，
// 任何不符都按非法标识拒绝（**不得**猜一个账本继续结算）。
// parseReservationKind recovers the ledger kind and validates the id shape.
func parseReservationKind(reservationID string) (string, error) {
	for _, kind := range []string{usageKindTasks, usageKindTokens, usageKindIngestBytes} {
		prefix := reservationIDPrefix + kind + "_"
		if !strings.HasPrefix(reservationID, prefix) {
			continue
		}
		if err := validateReservationPayload(reservationID[len(prefix):]); err != nil {
			return "", err
		}
		return kind, nil
	}
	return "", fmt.Errorf("%w: %q", errQuotaInvalidReservationID, reservationID)
}

// validateReservationPayload 校验 26 位 Crockford 载荷；小写按非法处理（服务端
// 只生成大写，大小写混用说明不是本服务签发的标识）。
// validateReservationPayload accepts uppercase Crockford only.
func validateReservationPayload(payload string) error {
	if len(payload) != reservationPayloadLength {
		return fmt.Errorf("%w: 载荷长度 %d，期望 %d", errQuotaInvalidReservationID, len(payload), reservationPayloadLength)
	}
	for i := 0; i < len(payload); i++ {
		if !strings.ContainsRune(crockfordAlphabet, rune(payload[i])) {
			return fmt.Errorf("%w: 载荷含非法字符 %q", errQuotaInvalidReservationID, payload[i])
		}
	}
	return nil
}

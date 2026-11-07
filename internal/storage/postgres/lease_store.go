package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `execution_leases` / `lease_events` 的**唯一 SQL 入口**（US-24 T005）。
// 三条纪律：
//
//  1. **时间只取数据库时钟**。列是 bigint Unix 毫秒，任何比较 / 写入都用
//     `(extract(epoch from clock_timestamp()) * 1000)::bigint`，不拿 Go 时间戳比较——
//     多 Worker 之间的时钟偏差会直接变成"租约提前过期 / 永不续约"。
//  2. **互斥来自行锁**。`Acquire` 与 `FencedTx` 都对同一 `(kind, resource_id)` 行
//     `SELECT … FOR UPDATE`：谁先拿到锁谁先改，后到者重读已提交版本。`EXISTS` 快照
//     在 READ COMMITTED 下取语句开始时的视图，**不能**作互斥依据。
//  3. **续约是行级条件写**。`Renew` 一条 `UPDATE … WHERE owner + epoch + 未过期`，
//     命中 0 行即 `ErrLeaseLost`；**不得**退化成"先读后判"，否则失权与本行者自己的
//     续约之间会出现判定窗口。
//
// This file is the only SQL entry point for execution_leases / lease_events. Lease
// timing always comes from the database clock, mutual exclusion always comes from the
// row lock, and renewal is a single conditional UPDATE (zero rows means lost).

// LeaseStore 是 core.LeaseStore 的 PostgreSQL 实现。
// LeaseStore is the PostgreSQL implementation of the consumer-declared lease port.
type LeaseStore struct{ db *DB }

// 编译期断言：实现满足 core 声明的端口。
var _ core.LeaseStore = (*LeaseStore)(nil)

// NewLeaseStore 绑定一个已打开的写模式库句柄。
func NewLeaseStore(db *DB) *LeaseStore { return &LeaseStore{db: db} }

// acquireAttempts 是"插入撞上并发插入"后的重读次数上限。`ON CONFLICT DO NOTHING`
// 让竞争失败方拿到 0 行，此时必须重新加锁读——已经有一次合法写入在前，读到的
// 多半是未过期租约（→ ErrLeaseHeld）。三次足够覆盖"读空 → 插入 → 撞车 → 再读"
// 的窗口，不把偶发竞争升级成无界重试。
const acquireAttempts = 3

// leaseRow 是加锁读到的一行租约快照。
type leaseRow struct {
	Owner    string
	Epoch    int64
	Expires  int64
	Released sql.NullInt64
}

// readDBMillis 读数据库当前毫秒。**必须在拿到租约行锁之后调用**：加锁前读到的
// "现在"在阻塞等待期间就会过期，用它判 expires 会把新持有者的有效租约误判成可接管。
// readDBMillis must run after the row lock is held; an earlier reading goes stale
// while the statement blocks.
func readDBMillis(ctx context.Context, tx *sql.Tx) (int64, error) {
	var ms int64
	if err := tx.QueryRowContext(ctx,
		`SELECT (extract(epoch from clock_timestamp()) * 1000)::bigint`).Scan(&ms); err != nil {
		return 0, fmt.Errorf("读取数据库时间: %w", err)
	}
	return ms, nil
}

// validateLeaseTarget 做入口校验：kind 闭集、资源标识非空、领取者有身份、ttl 为正。
// 未知 kind 必须在这里拒绝——库里的 CHECK 只是最后一道，走到那里报的是 23514，
// 调用方无从区分"程序缺陷"与"数据损坏"。
func validateLeaseTarget(kind core.LeaseKind, resourceID string) error {
	if _, err := core.ParseLeaseKind(string(kind)); err != nil {
		return err
	}
	if resourceID == "" {
		return fmt.Errorf("postgres: 租约资源标识为空")
	}
	return nil
}

// validateLeaseTTL 要求 ttl 至少 1ms：毫秒以下的值落库为 0，租约一写入即过期。
func validateLeaseTTL(ttl time.Duration) (int64, error) {
	ms := ttl.Milliseconds()
	if ms <= 0 {
		return 0, fmt.Errorf("postgres: 租约 ttl %s 不足 1ms", ttl)
	}
	return ms, nil
}

// lockLeaseRow 对 `(kind, resource_id)` 加行锁并读回当前值；无行返回 found=false。
func lockLeaseRow(ctx context.Context, tx *sql.Tx, kind core.LeaseKind, resourceID string) (leaseRow, bool, error) {
	var row leaseRow
	err := tx.QueryRowContext(ctx, `
SELECT owner_worker_id, ownership_epoch, expires_at, released_at
  FROM execution_leases
 WHERE kind = $1 AND resource_id = $2
   FOR UPDATE`, string(kind), resourceID).
		Scan(&row.Owner, &row.Epoch, &row.Expires, &row.Released)
	if errors.Is(err, sql.ErrNoRows) {
		return leaseRow{}, false, nil
	}
	if err != nil {
		return leaseRow{}, false, fmt.Errorf("锁定租约行: %w", err)
	}
	return row, true, nil
}

// leaseEventAllowed 判租约事件的五个闭集取值。本文件的写入路径产生 claimed /
// takeover / released，以及 keeper 经 `RecordRenewFailure` 落下的 renew_failed；
// reclaimed 由回收器（`reclaimer_store.go`）在收尾同一事务里写。闭集校验放在写入
// helper 里，避免调用方把拼错的字面量带进库（CHECK 会拒绝，但报的是 23514，丢失
// 调用点上下文）。
func leaseEventAllowed(event string) bool {
	switch event {
	case "claimed", "takeover", "released", "renew_failed", "reclaimed":
		return true
	default:
		return false
	}
}

// appendLeaseEvent 记一次**状态转换**（不记每次续约）。previousOwner / previousEpoch
// 是转换前的归属，首次领取时传空 / nil。
func appendLeaseEvent(ctx context.Context, tx *sql.Tx, kind core.LeaseKind, resourceID, event,
	ownerID string, epoch int64, previousOwner string, previousEpoch sql.NullInt64, detail string, nowMS int64,
) error {
	if !leaseEventAllowed(event) {
		return fmt.Errorf("postgres: 未知的租约事件 %q", event)
	}
	var prevOwner any
	if previousOwner != "" {
		prevOwner = previousOwner
	}
	var prevEpoch any
	if previousEpoch.Valid {
		prevEpoch = previousEpoch.Int64
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO lease_events(kind, resource_id, event, owner_worker_id, ownership_epoch,
                         previous_owner_worker_id, previous_epoch, detail, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		string(kind), resourceID, event, ownerID, epoch, prevOwner, prevEpoch, detail, nowMS); err != nil {
		return fmt.Errorf("写租约事件: %w", err)
	}
	return nil
}

// readRunCancelRequested 读 runs.cancel_requested；Run 行不存在或未迁移出该列时
// 视为"未请求取消"。取消请求是**观察量**，读不到不应让续约失败——失败会触发
// keeper 的有界停止，把一次读缺放大成一次停跑。
func readRunCancelRequested(ctx context.Context, tx *sql.Tx, runID string) (bool, error) {
	var cancel bool
	err := tx.QueryRowContext(ctx, `SELECT cancel_requested FROM runs WHERE run_id = $1`, runID).Scan(&cancel)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取 Run 取消标志: %w", err)
	}
	return cancel, nil
}

// leaseView 把一行读数拼成 core.Lease。
func leaseView(kind core.LeaseKind, resourceID, ownerID string, epoch, acquiredAt, renewedAt,
	expiresAt int64, tookOver, cancelRequested bool,
) core.Lease {
	return core.Lease{
		LeaseToken: core.LeaseToken{
			Kind:           kind,
			ResourceID:     resourceID,
			OwnerWorkerID:  ownerID,
			OwnershipEpoch: epoch,
		},
		AcquiredAt:      time.UnixMilli(acquiredAt).UTC(),
		RenewedAt:       time.UnixMilli(renewedAt).UTC(),
		ExpiresAt:       time.UnixMilli(expiresAt).UTC(),
		CancelRequested: cancelRequested,
		TookOver:        tookOver,
	}
}

// Acquire 领取 `(kind, resource_id)` 的执行权。四种结局（模块卡 §2.3.3 图 3）：
//
//	无行                → 新建，epoch = 1
//	已释放              → 正常交棒，epoch = old+1，TookOver = false
//	已过期（未释放）    → 接管，epoch = old+1，TookOver = true
//	未过期且未释放      → ErrLeaseHeld（不等待、不排队）
//
// 全流程在**一个事务**里：行锁 → 读数据库时间 → 判定 → 写入 → 事件。行锁与 FencedTx
// 共用，因此"接管"与"旧属主正在提交"不会交错成双方都成功。
func (s *LeaseStore) Acquire(ctx context.Context, kind core.LeaseKind, resourceID string,
	owner core.WorkerIdentity, ttl time.Duration,
) (core.Lease, error) {
	if err := validateLeaseTarget(kind, resourceID); err != nil {
		return core.Lease{}, err
	}
	if owner.InstanceID == "" {
		return core.Lease{}, fmt.Errorf("postgres: 领取者缺少实例标识")
	}
	ttlMS, err := validateLeaseTTL(ttl)
	if err != nil {
		return core.Lease{}, err
	}

	var lease core.Lease
	_, err = s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for attempt := 0; attempt < acquireAttempts; attempt++ {
			row, found, err := lockLeaseRow(ctx, tx, kind, resourceID)
			if err != nil {
				return err
			}
			nowMS, err := readDBMillis(ctx, tx)
			if err != nil {
				return err
			}

			if !found {
				// 插入用 `ON CONFLICT DO NOTHING`：并发插入的失败方拿到 0 行后重读
				// 已提交的租约，而不是让唯一冲突冒成 500。
				res, err := tx.ExecContext(ctx, `
INSERT INTO execution_leases(kind, resource_id, owner_worker_id, ownership_epoch,
                             acquired_at, renewed_at, expires_at, released_at)
VALUES ($1, $2, $3, 1, $4, $4, $4::bigint + $5::bigint, NULL)
ON CONFLICT (kind, resource_id) DO NOTHING`,
					string(kind), resourceID, owner.InstanceID, nowMS, ttlMS)
				if err != nil {
					return fmt.Errorf("创建租约: %w", err)
				}
				affected, err := res.RowsAffected()
				if err != nil {
					return fmt.Errorf("读取租约插入结果: %w", err)
				}
				if affected == 0 {
					continue // 另一 Worker 抢先插入；回到循环顶部加锁重读
				}
				if err := appendLeaseEvent(ctx, tx, kind, resourceID, "claimed", owner.InstanceID,
					1, "", sql.NullInt64{}, "", nowMS); err != nil {
					return err
				}
				cancel := false
				if kind == core.LeaseRun {
					cancel, err = readRunCancelRequested(ctx, tx, resourceID)
					if err != nil {
						return err
					}
				}
				lease = leaseView(kind, resourceID, owner.InstanceID, 1, nowMS, nowMS, nowMS+ttlMS, false, cancel)
				return nil
			}

			expired := row.Expires <= nowMS
			released := row.Released.Valid
			if !expired && !released {
				return core.ErrLeaseHeld
			}

			// 已释放是**正常交棒**（前手主动交还，Flow 可直接推进）；已过期才是接管
			// （前手可能还活着，Flow 必须先 Recover）。两者都递增代次，旧 token 一律失权。
			tookOver := expired && !released
			newEpoch := row.Epoch + 1
			res, err := tx.ExecContext(ctx, `
UPDATE execution_leases
   SET owner_worker_id = $3, ownership_epoch = $4,
       acquired_at = $5, renewed_at = $5, expires_at = $5::bigint + $6::bigint, released_at = NULL
 WHERE kind = $1 AND resource_id = $2 AND owner_worker_id = $7 AND ownership_epoch = $8`,
				string(kind), resourceID, owner.InstanceID, newEpoch, nowMS, ttlMS, row.Owner, row.Epoch)
			if err != nil {
				return fmt.Errorf("接管租约: %w", err)
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("读取租约接管结果: %w", err)
			}
			if affected != 1 {
				// 持有行锁时不可能被改：走到这里说明锁语义被绕过，必须报错而不是重试。
				return fmt.Errorf("postgres: 持锁期间租约行被修改（kind=%s resource=%s）", kind, resourceID)
			}

			event, detail := "claimed", "released"
			if tookOver {
				event, detail = "takeover", "expired"
			}
			if err := appendLeaseEvent(ctx, tx, kind, resourceID, event, owner.InstanceID, newEpoch,
				row.Owner, sql.NullInt64{Int64: row.Epoch, Valid: true}, detail, nowMS); err != nil {
				return err
			}
			cancel := false
			if kind == core.LeaseRun {
				cancel, err = readRunCancelRequested(ctx, tx, resourceID)
				if err != nil {
					return err
				}
			}
			lease = leaseView(kind, resourceID, owner.InstanceID, newEpoch, nowMS, nowMS, nowMS+ttlMS,
				tookOver, cancel)
			return nil
		}
		return fmt.Errorf("postgres: 租约领取竞争未收敛（kind=%s resource=%s）", kind, resourceID)
	})
	if err != nil {
		return core.Lease{}, err
	}
	return lease, nil
}

// Renew 续约。**一条行级条件写**：owner + epoch + 未释放 + 未过期同时匹配才命中；
// 0 行即 `ErrLeaseLost`（过期后即使尚未被接管，原负责人也不得继续提交）。
// 续约成功会刷新 `renewed_at` 与 `expires_at`，但**不写 lease_events**——事件只记
// 状态转换，记每次续约会把事件表变成心跳日志。
func (s *LeaseStore) Renew(ctx context.Context, token core.LeaseToken, ttl time.Duration) (core.Lease, error) {
	if err := validateLeaseTarget(token.Kind, token.ResourceID); err != nil {
		return core.Lease{}, err
	}
	ttlMS, err := validateLeaseTTL(ttl)
	if err != nil {
		return core.Lease{}, err
	}

	var lease core.Lease
	_, err = s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var acquiredAt, renewedAt, expiresAt int64
		err := tx.QueryRowContext(ctx, `
WITH now AS (SELECT (extract(epoch from clock_timestamp()) * 1000)::bigint AS ms)
UPDATE execution_leases l
   SET renewed_at = now.ms, expires_at = now.ms + $5
  FROM now
 WHERE l.kind = $1 AND l.resource_id = $2
   AND l.owner_worker_id = $3 AND l.ownership_epoch = $4
   AND l.released_at IS NULL AND l.expires_at > now.ms
RETURNING l.acquired_at, l.renewed_at, l.expires_at`,
			string(token.Kind), token.ResourceID, token.OwnerWorkerID, token.OwnershipEpoch, ttlMS).
			Scan(&acquiredAt, &renewedAt, &expiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("续约租约: %w", err)
		}
		cancel := false
		if token.Kind == core.LeaseRun {
			cancel, err = readRunCancelRequested(ctx, tx, token.ResourceID)
			if err != nil {
				return err
			}
		}
		lease = leaseView(token.Kind, token.ResourceID, token.OwnerWorkerID, token.OwnershipEpoch,
			acquiredAt, renewedAt, expiresAt, false, cancel)
		return nil
	})
	if err != nil {
		return core.Lease{}, err
	}
	return lease, nil
}

// Release 释放租约。**幂等**：同一 owner + epoch 已释放 → nil（收尾重试不报错）；
// 不匹配 → ErrLeaseLost（不得释放新负责人的租约）。释放写 `lease_events(released)`。
func (s *LeaseStore) Release(ctx context.Context, token core.LeaseToken) error {
	if err := validateLeaseTarget(token.Kind, token.ResourceID); err != nil {
		return err
	}
	_, err := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
UPDATE execution_leases
   SET released_at = $5
 WHERE kind = $1 AND resource_id = $2 AND owner_worker_id = $3 AND ownership_epoch = $4
   AND released_at IS NULL`,
			string(token.Kind), token.ResourceID, token.OwnerWorkerID, token.OwnershipEpoch, nowMS)
		if err != nil {
			return fmt.Errorf("释放租约: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取租约释放结果: %w", err)
		}
		if affected == 1 {
			return appendLeaseEvent(ctx, tx, token.Kind, token.ResourceID, "released",
				token.OwnerWorkerID, token.OwnershipEpoch, "", sql.NullInt64{}, "", nowMS)
		}

		// 0 行：要么已由本持有者释放过（幂等），要么归属已易主（失权）。
		var row leaseRow
		err = tx.QueryRowContext(ctx, `
SELECT owner_worker_id, ownership_epoch, expires_at, released_at
  FROM execution_leases
 WHERE kind = $1 AND resource_id = $2`, string(token.Kind), token.ResourceID).
			Scan(&row.Owner, &row.Epoch, &row.Expires, &row.Released)
		if errors.Is(err, sql.ErrNoRows) {
			return core.ErrLeaseLost
		}
		if err != nil {
			return fmt.Errorf("读取租约释放状态: %w", err)
		}
		if row.Owner == token.OwnerWorkerID && row.Epoch == token.OwnershipEpoch && row.Released.Valid {
			return nil
		}
		return core.ErrLeaseLost
	})
	return err
}

// RecordRenewFailure 补一行 `lease_events(event='renew_failed')`（US-24 §2.7 /
// 评审 IDX-25）。keeper 只在失权或有界停止两个终态调用，因此这里**不**逐次续约写事件。
//
// 归属列写**调用方当时的** owner / epoch，不读当前行：事件记录的是"谁观察到续约失败"，
// 不是权威归属读数——失权分支里该行可能已被接管，读当前值会把失败记到新属主名下。
// 写入不校验租约仍有效（失败正是要记的事实），也不参与任何受保护写入面；调用方按
// best-effort 处理，写失败只记日志。
// RecordRenewFailure appends the renew_failed event under the caller's original
// owner/epoch; it never re-reads or fences the lease row.
func (s *LeaseStore) RecordRenewFailure(ctx context.Context, token core.LeaseToken, reason string) error {
	if err := validateLeaseTarget(token.Kind, token.ResourceID); err != nil {
		return err
	}
	_, err := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		return appendLeaseEvent(ctx, tx, token.Kind, token.ResourceID, "renew_failed",
			token.OwnerWorkerID, token.OwnershipEpoch, "", sql.NullInt64{}, reason, nowMS)
	})
	return err
}

// 编译期断言：keeper 的可选事件出口由本实现提供（缺它时 keeper 只记日志）。
// The optional renew_failed outlet is implemented here; the keeper degrades to
// logging when a store does not provide it.
var _ core.RenewFailureRecorder = (*LeaseStore)(nil)

// FencedTx 执行一次**受归属保护**的写入。顺序固定（模块卡 §2.3.3）：
//
//	BEGIN → （run 且会话非空）advisory(session) → 租约行 FOR UPDATE
//	      → 校验 owner + epoch + 未释放 + 未过期 → fn(tx) → COMMIT
//
// 互斥证明来自**行锁**：持锁期间 `Acquire` 与另一 `FencedTx` 都阻塞；接管提交后
// 旧持有者重读一定能看到新 epoch。只读 `EXISTS` 子查询在 READ COMMITTED 下取语句
// 开始时的视图，**不能**作为互斥依据。
// The fence is the row lock, never an EXISTS snapshot: blocking on the row followed by
// a re-read is what makes a stale owner fail after a takeover commits.
func (s *LeaseStore) FencedTx(ctx context.Context, token core.LeaseToken,
	fn func(ctx context.Context, tx core.Tx) error,
) error {
	if fn == nil {
		return fmt.Errorf("postgres: FencedTx 的写入闭包为空")
	}
	// `*sql.Tx` 天然满足 `core.Tx`（方法集包含声明的那三个），这里只是把闭包的
	// 参数类型从接口收窄回具体句柄——不装箱、不复制，仍是同一个事务对象。
	return fencedTx(ctx, s.db, token, func(ctx context.Context, tx *sql.Tx) error {
		return fn(ctx, tx)
	})
}

// fencedTx 是**全部受保护写入的同一份实现**：`LeaseStore.FencedTx` 与各仓储的
// `Fenced*` 写入口都经它开事务，因此锁顺序只有一处定义、不可能各自漂移
// （模块卡 §2.3.3 的"唯一写入入口"）。
//
// 顺序固定：BEGIN → （run 且会话非空）advisory(session) → 租约行 FOR UPDATE
// → 校验 owner + epoch + 未释放 + 未过期 → fn(tx) → COMMIT。
// 互斥证明来自**行锁**：持锁期间 `Acquire` 与另一 `fencedTx` 都阻塞；接管提交后
// 旧持有者重读一定能看到新 epoch。只读 `EXISTS` 子查询在 READ COMMITTED 下取语句
// 开始时的视图，**不能**作为互斥依据。
// fencedTx is the single implementation behind every ownership-protected write, so the
// lock order is defined exactly once. The fence is the row lock, never an EXISTS snapshot.
func fencedTx(ctx context.Context, db *DB, token core.LeaseToken,
	fn func(ctx context.Context, tx *sql.Tx) error,
) error {
	return runFencedTx(ctx, db, token, nil, fn)
}

// fencedTxWithPre 是同一外壳的**学习写面形态**（模块卡 §2.3.5 / R7）：`pre` 在租约行锁
// **之前**执行，用来取 agent advisory；此时**不取** session advisory——会话串行已由
// "run 租约未释放"挡住同会话新 Run，多取一把只会扩大锁面。锁序仍为
// advisory(agent) → 租约行 → 业务行，与 B3 恢复写入一致、不得倒置。
// fencedTxWithPre fixes the learning-surface lock order: advisory(agent) → lease row →
// business row, and deliberately takes no session advisory.
func fencedTxWithPre(ctx context.Context, db *DB, token core.LeaseToken,
	pre func(ctx context.Context, tx *sql.Tx) error,
	fn func(ctx context.Context, tx *sql.Tx) error,
) error {
	if pre == nil {
		return fmt.Errorf("postgres: fenced 写入的 pre 步骤为空——学习写面必须显式给出 advisory")
	}
	return runFencedTx(ctx, db, token, pre, fn)
}

// runFencedTx 是两者共用的**唯一事务外壳**：`pre` 为 nil 时按默认形态为 run 类写入取
// session advisory；非 nil 时改用调用方给的 pre（学习写面：agent advisory）。
// runFencedTx is the one transaction shell both entry points share.
func runFencedTx(ctx context.Context, db *DB, token core.LeaseToken,
	pre func(ctx context.Context, tx *sql.Tx) error,
	fn func(ctx context.Context, tx *sql.Tx) error,
) error {
	// 零值 token 不是"无凭证"（那是调用方用 IsZero 分流后的另一条路径），而是
	// 装配缺陷：走到这里说明写入方选错了入口。显式拒绝，不让 ParseLeaseKind 的
	// "未知类型"错误掩盖真正原因。
	if token.IsZero() {
		return fmt.Errorf("postgres: fenced 写入收到零值执行权凭证——调用方必须先判 IsZero")
	}
	if err := validateLeaseTarget(token.Kind, token.ResourceID); err != nil {
		return err
	}
	_, err := db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if pre != nil {
			if err := pre(ctx, tx); err != nil {
				return err
			}
		} else if token.Kind == core.LeaseRun {
			// 会话互斥只对 run 有意义：会话串行已由"run 租约未释放"挡住同会话新 Run，
			// 这里取的 advisory 与 StartRun / A1 恢复共用同一个键。
			var sessionID sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT session_id FROM runs WHERE run_id = $1`,
				token.ResourceID).Scan(&sessionID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("读取 Run 会话: %w", err)
			}
			if sessionID.Valid && sessionID.String != "" {
				if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`,
					sessionAdvisoryKey(sessionID.String)); err != nil {
					return fmt.Errorf("获取会话锁: %w", err)
				}
			}
		}

		row, found, err := lockLeaseRow(ctx, tx, token.Kind, token.ResourceID)
		if err != nil {
			return err
		}
		if !found {
			return core.ErrLeaseLost
		}
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		if row.Owner != token.OwnerWorkerID || row.Epoch != token.OwnershipEpoch ||
			row.Released.Valid || row.Expires <= nowMS {
			return core.ErrLeaseLost
		}
		return fn(ctx, tx)
	})
	return err
}

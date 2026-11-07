package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `schedule_deliveries` 的唯一 SQL 入口（US-12 R7）。这张表是系统代投的
// **权威记录**：发送前先写 pending，终态只回填一次；崩溃恢复按关联 Run 的实际状态
// 把 pending 改判为 unknown（运行已完成、投递是否发生无法证明）或 skipped（运行未完成）。
// **不得**把 unknown 当作失败自动重发。
// This file is the only SQL entry point of schedule_deliveries. A pending row is written
// before sending; recovery turns it into unknown (run completed, send unprovable) or
// skipped (run unfinished) and never resends blindly.

// DeliveryStore 是 core.DeliveryStore 的 SQLite 实现。
// DeliveryStore is the SQLite implementation of core.DeliveryStore.
type DeliveryStore struct{ db *DB }

// 编译期断言：实现满足消费方（core）声明的投递仓储接口。
// Compile-time assertion that the store satisfies the consumer-declared port.
var _ core.DeliveryStore = (*DeliveryStore)(nil)

// NewDeliveryStore 绑定一个已打开的写模式库句柄。
// NewDeliveryStore binds the store to an opened writable database handle.
func NewDeliveryStore(db *DB) *DeliveryStore { return &DeliveryStore{db: db} }

// Prepare 预写一条 pending 投递行；同一 run_id 重复调用是幂等空操作（重试 / 恢复
// 不会产生第二行，也不会覆盖已有终态）。
// Prepare writes the pending row; a repeated run_id is an idempotent no-op and never
// overwrites an existing terminal state.
func (s *DeliveryStore) Prepare(ctx context.Context, rec core.ScheduleDeliveryRecord) error {
	// `execution_id` 与 pending 行同一条 INSERT 落库（US-24 / 0012 新列）：它是回收器
	// B2 判定"这条 Run 创建前的投递是否还有合法执行者"的唯一持久关联。另起一条 UPDATE
	// 会留下"行已预写、归属未关联"的窗口，那个窗口里的崩溃正好无法判定所有权。
	// execution_id is written by the same INSERT as the pending row; a second UPDATE
	// would reopen the very window B2 exists to close.
	_, err := s.db.pool.ExecContext(ctx,
		`INSERT INTO schedule_deliveries
		   (run_id, task_id, agent_name, agent_uid, channel_uid, channel_name,
		    status, detail, execution_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, '', $8, $9)
		 ON CONFLICT(run_id) DO NOTHING`,
		rec.RunID, rec.TaskID, rec.Agent, string(rec.AgentUID), string(rec.ChannelUID),
		rec.ChannelName, string(core.DeliveryPending), nullableInt64(rec.ExecutionID),
		rec.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("预写定时投递记录（run_id=%s）: %w", rec.RunID, err)
	}
	return nil
}

// Finish 回填终态：只允许从 pending 迁移一次，重复回填不影响既有终态（幂等），
// 但未知 run_id 如实报 `fs.ErrNotExist`。
// Finish writes the terminal state once; a repeated call is idempotent and an unknown
// run id reports fs.ErrNotExist.
func (s *DeliveryStore) Finish(ctx context.Context, runID string, status core.DeliveryStatus,
	httpStatus int, receiptCode *int, detail string, attemptedAt, finishedAt time.Time,
) error {
	res, err := s.db.pool.ExecContext(ctx,
		`UPDATE schedule_deliveries
		    SET status = $1, http_status = $2, receipt_code = $3, detail = $4,
		        attempted_at = $5, finished_at = $6
		  WHERE run_id = $7 AND status = $8`,
		string(status), nullableInt(httpStatus), nullableIntPtr(receiptCode), detail,
		nullableTime(attemptedAt), nullableTime(finishedAt), runID, string(core.DeliveryPending))
	if err != nil {
		return fmt.Errorf("回填定时投递记录（run_id=%s）: %w", runID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认定时投递回填（run_id=%s）: %w", runID, err)
	}
	if affected > 0 {
		return nil
	}
	// 0 行可能是「已回填」或「不存在」：只有后者才是错误。
	var exists int
	if err := s.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM schedule_deliveries WHERE run_id = $1`, runID).Scan(&exists); err != nil {
		return fmt.Errorf("确认定时投递记录存在（run_id=%s）: %w", runID, err)
	}
	if exists == 0 {
		return fmt.Errorf("回填定时投递记录（run_id=%s）: %w", runID, fs.ErrNotExist)
	}
	return nil
}

// Get 按 run_id 读取一条投递记录。
// Get reads one delivery row by run id.
func (s *DeliveryStore) Get(ctx context.Context, runID string) (core.ScheduleDeliveryRecord, error) {
	row := s.db.pool.QueryRowContext(ctx,
		`SELECT run_id, task_id, agent_name, agent_uid, channel_uid, channel_name,
		        status, http_status, receipt_code, detail, attempted_at, finished_at,
		        created_at, execution_id
		   FROM schedule_deliveries WHERE run_id = $1`, runID)
	rec, err := scanScheduleDelivery(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return core.ScheduleDeliveryRecord{}, fmt.Errorf("读取定时投递记录（run_id=%s）: %w", runID, fs.ErrNotExist)
		}
		return core.ScheduleDeliveryRecord{}, fmt.Errorf("读取定时投递记录（run_id=%s）: %w", runID, err)
	}
	return rec, nil
}

// ListByTask 返回某任务最近的投递记录（新→旧），最多 limit 条。
// ListByTask returns the newest delivery rows of one task, capped at limit.
func (s *DeliveryStore) ListByTask(ctx context.Context, taskID string, limit int) ([]core.ScheduleDeliveryRecord, error) {
	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT run_id, task_id, agent_name, agent_uid, channel_uid, channel_name,
		        status, http_status, receipt_code, detail, attempted_at, finished_at,
		        created_at, execution_id
		   FROM schedule_deliveries WHERE task_id = $1
		  ORDER BY created_at DESC, run_id DESC LIMIT $2::bigint`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出定时投递记录（task_id=%s）: %w", taskID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.ScheduleDeliveryRecord
	for rows.Next() {
		rec, scanErr := scanScheduleDelivery(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("读取定时投递记录（task_id=%s）: %w", taskID, scanErr)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描定时投递记录（task_id=%s）: %w", taskID, err)
	}
	return out, nil
}

// settleDeliveriesInTx 在调用方的事务里把**指定 Run** 的 pending 投递结算为调用方
// 判定的唯一结论（A1 步骤 ⑤ / B1 步骤 ③ / FR-24-21）。
//
// 结论由调用方给：A1 一律 `skipped`——那条路径写的是 `failed`，投递要么没发生、要么
// 结果无从证明；B1 按 Run 的终态分 CASE（`completed` / `limit_reached` → `unknown`，
// 其余 → `skipped`）。两者分属不同情形，**不混写**，但都**不得重发**——重复通知比
// 少一条通知更糟。
// settleDeliveriesInTx settles one run's pending deliveries to the single verdict the
// caller decided and never resends.
func settleDeliveriesInTx(ctx context.Context, ex sqlExecutor, runID string,
	status core.DeliveryStatus, detail string, now time.Time,
) (int, error) {
	if runID == "" {
		return 0, nil
	}
	res, err := ex.ExecContext(ctx,
		`UPDATE schedule_deliveries SET status = $1, detail = $2, finished_at = $3
		  WHERE run_id = $4 AND status = $5`,
		string(status), detail, now.UnixMilli(), runID, string(core.DeliveryPending))
	if err != nil {
		return 0, fmt.Errorf("收尾遗留投递行（run_id=%s）: %w", runID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("确认遗留投递行收尾（run_id=%s）: %w", runID, err)
	}
	return int(affected), nil
}

// terminalDeliveryOutcome 把 Run 的终态映射为终态残留（B1）里 pending 投递的结论。
//
// `completed` / `limit_reached` → `unknown`：模型已经产出正文，但投递是否发出无从
// 证明，重发就是重复通知；其余终态 → `skipped`：运行没成功，按约定不投递。
// 这条映射与 `DeliveryStore.Recover`（US-12 的全局版本）逐字一致，B1 只是把它收窄
// 到"无有效 run 租约的终态 Run"，不再扫全表。
// terminalDeliveryOutcome maps a terminal run state onto the B1 delivery verdict.
func terminalDeliveryOutcome(runStatus string) (core.DeliveryStatus, string) {
	switch core.RunStatus(runStatus) {
	case core.StatusCompleted, core.StatusLimitReached:
		return core.DeliveryUnknown, "进程在投递完成前退出，投递结果未知"
	default:
		return core.DeliverySkipped, "运行未完成，未投递"
	}
}

// Recover 处理**指定 Run** 崩溃遗留的 pending 行：LEFT JOIN `runs` 取实际终态——
// 已完成 / 达上限 → unknown（模型已产出正文，但投递是否发生无法证明，**不自动重发**）；
// 其余（失败 / 取消 / 仍 running / 无 Run）→ skipped。
//
// 作用域是 US-24 的核心修正（FR-24-21 / C-15）：没有作用域的全局版会把其他 Worker
// 正在收尾的 pending 行一并改判成 skipped，等于替别人宣布"没发出去"。调用方只在
// **已合法领取**该 Run 时传它的 run_id。
// Recover resolves only the named run's pending row; a global scan would rewrite
// another worker's in-flight delivery.
func (s *DeliveryStore) Recover(ctx context.Context, now time.Time, runID string) (int, error) {
	if runID == "" {
		return 0, fmt.Errorf("收尾遗留投递行: run_id 不得为空（恢复必须按被领取的 Run 作用域）")
	}
	recovered := 0
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		recovered = 0
		var runState sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT r.status
			   FROM schedule_deliveries d
			   LEFT JOIN runs r ON r.run_id = d.run_id
			  WHERE d.status = $1 AND d.run_id = $2`,
			string(core.DeliveryPending), runID).Scan(&runState)
		if err == sql.ErrNoRows {
			// 该 Run 没有 pending 投递（从未预写或已回填）：恢复是空操作。
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取遗留的 pending 投递行（run_id=%s）: %w", runID, err)
		}

		status := core.DeliverySkipped
		detail := "运行未完成，未投递"
		if runState.Valid &&
			(runState.String == string(core.StatusCompleted) ||
				runState.String == string(core.StatusLimitReached)) {
			status = core.DeliveryUnknown
			detail = "进程在投递完成前退出，投递结果未知"
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE schedule_deliveries SET status = $1, detail = $2, finished_at = $3
			  WHERE run_id = $4 AND status = $5`,
			string(status), detail, now.UnixMilli(), runID, string(core.DeliveryPending)); err != nil {
			return fmt.Errorf("收尾遗留投递行（run_id=%s）: %w", runID, err)
		}
		recovered++
		return nil
	})
	if err != nil {
		return 0, err
	}
	return recovered, nil
}

// orphanDeliveryCandidates 是 B2 的候选语句（模块卡 §2.3.5 B2 / FR-24-39）：
// 只选"Run 行不存在"**且**"所属 schedule_execution 已失去合法执行者"的 pending 行。
//
// 两个谓词缺一不可：
//   - `NOT EXISTS runs`：没有 Run 行 ⇒ 投递从未尝试，结论只能是 `skipped`。
//     `unknown`（可能已发出）在这里语义上不成立——那条分支归 B1，B2 **不得**读它。
//   - `NOT EXISTS 仍有效的 schedule_execution 租约`：`Prepare` 在 Run 准入**之前**
//     写 pending（`internal/core/scheduler.go`），属主握着执行租约时这行是它正在
//     推进的投递；只按"没有 Run 行"判会把活 Worker 的预写标成 skipped。幂等只保证
//     重复执行无害，不证明第一次执行是对的——所有权必须显式判。
//
// `execution_id` 为空是 0012 迁移遗留行（当时还没有这列）：退回按 `task_id` 下是否
// 仍存在有效执行租约判断，仍有合法执行者就不动。
//
// 返回 `task_id` 与 `execution_id` 是为了逐行**加锁重判**（见
// `lockOrphanDeliveryLeaseRecheck`）：扫描谓词只是快照判断，未提交的续约不可见。
const orphanDeliveryCandidates = `
SELECT d.run_id, d.task_id, d.execution_id
  FROM schedule_deliveries d
 WHERE d.status = $1
   AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.run_id = d.run_id)
   AND NOT EXISTS (
         SELECT 1
           FROM execution_leases l
          WHERE l.kind = 'schedule_execution'
            AND l.released_at IS NULL
            AND l.expires_at > $2
            AND ((d.execution_id IS NOT NULL AND l.resource_id = d.execution_id::text)
              OR (d.execution_id IS NULL
                  AND l.resource_id IN (
                        SELECT e.id::text
                          FROM task_executions e
                         WHERE e.task_id = d.task_id))))
 ORDER BY d.created_at, d.run_id
 LIMIT $3`

// lockOrphanDeliveryLeaseRecheck 在加锁后重判该待结算投递对应的 schedule_execution
// 租约是否仍由活着的执行者持有（并发理由见 `lockLeaseRecheck`）。
//
// 锁哪些行：`execution_id` 有值就锁那条执行租约；迁移遗留行为 NULL 时退回按
// `task_id` 找该任务名下的执行租约（正常至多一条 started；仍按多行遍历，不把唯一
// 索引当成这里正确性的前提）。
// lockOrphanDeliveryLeaseRecheck re-decides lease liveness under the row lock so an
// uncommitted renewal that the candidate scan could not see is never settled away.
func lockOrphanDeliveryLeaseRecheck(ctx context.Context, tx *sql.Tx, taskID string,
	executionID sql.NullInt64,
) (bool, error) {
	if executionID.Valid {
		return lockLeaseRecheck(ctx, tx, core.LeaseScheduleExecution,
			strconv.FormatInt(executionID.Int64, 10))
	}
	rows, err := tx.QueryContext(ctx, `
SELECT l.expires_at, l.released_at
  FROM execution_leases l
  JOIN task_executions e ON e.id::text = l.resource_id
 WHERE l.kind = $1 AND e.task_id = $2
 ORDER BY l.resource_id
   FOR UPDATE OF l`, string(core.LeaseScheduleExecution), taskID)
	if err != nil {
		return false, fmt.Errorf("锁定任务执行租约（task_id=%s）: %w", taskID, err)
	}
	defer func() { _ = rows.Close() }()
	type leaseWindow struct {
		expiresAt  int64
		releasedAt sql.NullInt64
	}
	var windows []leaseWindow
	for rows.Next() {
		var window leaseWindow
		if scanErr := rows.Scan(&window.expiresAt, &window.releasedAt); scanErr != nil {
			return false, fmt.Errorf("读取任务执行租约（task_id=%s）: %w", taskID, scanErr)
		}
		windows = append(windows, window)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return false, fmt.Errorf("遍历任务执行租约（task_id=%s）: %w", taskID, rowsErr)
	}
	nowMS, err := readDBMillis(ctx, tx)
	if err != nil {
		return false, err
	}
	for _, window := range windows {
		if !window.releasedAt.Valid && window.expiresAt > nowMS {
			return true, nil
		}
	}
	return false, nil
}

// orphanDeliveryDetail 是 B2 写进 `detail` 的固定文案：与"运行未完成，未投递"分开，
// 运维据此区分"跑过但没投"与"Run 都没建出来"。
// orphanDeliveryDetail is the fixed skip detail for deliveries whose run never existed.
const orphanDeliveryDetail = "运行未创建，未投递"

// SettleOrphanDeliveries 是回收器的 B2 域（模块卡 §2.3.5 B2 / FR-24-39 / C-29）：
// 把"Run 创建前预写、属主已失联"的 pending 投递结算为 `skipped`。
//
// 不领租约：B 类靠幂等条件更新即互斥（只从 pending 迁出一次）。但幂等只保证重复
// 执行无害，不证明第一次执行是对的——逐行结算前**必须**先在租约行上加锁重判所有权
// （`lockOrphanDeliveryLeaseRecheck`），否则一条未提交的合法续约会从扫描快照里漏过。
// 每轮受 `limit` 约束；没有候选是正常结果（正常路径该计数为 0，>0 说明崩溃落在
// Prepare 与 Run 准入之间）。
// SettleOrphanDeliveries settles pre-run pending deliveries left by a lost execution
// owner; it claims no lease and only ever writes skipped.
func SettleOrphanDeliveries(ctx context.Context, db *DB, now time.Time, limit int) (int, error) {
	if db == nil || db.pool == nil {
		return 0, fmt.Errorf("postgres: 运行创建前投递结算缺少数据库句柄")
	}
	if limit <= 0 {
		return 0, fmt.Errorf("postgres: 运行创建前投递结算的条数上限必须为正（收到 %d）", limit)
	}
	settled := 0
	err := db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		settled = 0
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, orphanDeliveryCandidates,
			string(core.DeliveryPending), nowMS, limit)
		if err != nil {
			return fmt.Errorf("扫描运行创建前的投递行: %w", err)
		}
		type orphanDeliveryRow struct {
			runID       string
			taskID      string
			executionID sql.NullInt64
		}
		var candidates []orphanDeliveryRow
		for rows.Next() {
			var row orphanDeliveryRow
			if scanErr := rows.Scan(&row.runID, &row.taskID, &row.executionID); scanErr != nil {
				_ = rows.Close()
				return fmt.Errorf("读取运行创建前的投递行: %w", scanErr)
			}
			candidates = append(candidates, row)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return fmt.Errorf("遍历运行创建前的投递行: %w", rowsErr)
		}
		if closeErr := rows.Close(); closeErr != nil {
			return fmt.Errorf("关闭运行创建前的投递行游标: %w", closeErr)
		}

		for _, row := range candidates {
			// 锁租约行后重判：扫描快照看不到尚未提交的合法续约；它一旦落库，
			// 这条投递仍归活着的执行者推进，结算它就是替别人宣布"没发出去"。
			live, liveErr := lockOrphanDeliveryLeaseRecheck(ctx, tx, row.taskID, row.executionID)
			if liveErr != nil {
				return liveErr
			}
			if live {
				continue
			}
			// 条件更新：同一行只从 pending 迁出一次；扫描后若被别人合法结算，0 行。
			res, execErr := tx.ExecContext(ctx,
				`UPDATE schedule_deliveries
				    SET status = $1, detail = $2, finished_at = $3
				  WHERE run_id = $4 AND status = $5`,
				string(core.DeliverySkipped), orphanDeliveryDetail, now.UnixMilli(),
				row.runID, string(core.DeliveryPending))
			if execErr != nil {
				return fmt.Errorf("结算运行创建前的投递行（run_id=%s）: %w", row.runID, execErr)
			}
			affected, affErr := res.RowsAffected()
			if affErr != nil {
				return fmt.Errorf("确认运行创建前的投递结算（run_id=%s）: %w", row.runID, affErr)
			}
			settled += int(affected)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return settled, nil
}

// scheduleDeliveryScanner 是 QueryRow / Rows 的共同读取面。
// scheduleDeliveryScanner is the shared read face of QueryRow and Rows.
type scheduleDeliveryScanner interface {
	Scan(dest ...any) error
}

// scanScheduleDelivery 把一行投影成领域记录：可空列用 sql 包类型承接，`receipt_code` 的
// NULL 与 0 保持可区分（0 是飞书成功，NULL 是未取得回执）。
// scanScheduleDelivery projects one row and keeps a NULL receipt code distinct from zero.
func scanScheduleDelivery(scanner scheduleDeliveryScanner) (core.ScheduleDeliveryRecord, error) {
	var (
		rec         core.ScheduleDeliveryRecord
		agentUID    string
		channelUID  string
		status      string
		httpStatus  sql.NullInt64
		receiptCode sql.NullInt64
		attemptedAt sql.NullInt64
		finishedAt  sql.NullInt64
		createdAt   int64
		executionID sql.NullInt64
	)
	if err := scanner.Scan(&rec.RunID, &rec.TaskID, &rec.Agent, &agentUID, &channelUID,
		&rec.ChannelName, &status, &httpStatus, &receiptCode, &rec.Detail,
		&attemptedAt, &finishedAt, &createdAt, &executionID); err != nil {
		return core.ScheduleDeliveryRecord{}, err
	}
	rec.AgentUID = core.AgentUID(agentUID)
	rec.ChannelUID = core.ResourceUID(channelUID)
	rec.Status = core.DeliveryStatus(status)
	rec.HTTPStatus = int(httpStatus.Int64)
	if receiptCode.Valid {
		code := int(receiptCode.Int64)
		rec.ReceiptCode = &code
	}
	// 迁移遗留行的 execution_id 为 NULL：零值表示"没有归属关联"，不是 0 号执行行。
	rec.ExecutionID = executionID.Int64
	rec.AttemptedAt = unixMilliOrZero(attemptedAt)
	rec.FinishedAt = unixMilliOrZero(finishedAt)
	rec.CreatedAt = time.UnixMilli(createdAt).UTC()
	return rec, nil
}

// nullableInt64 把 0 视为"没有关联的执行行"写 NULL（执行行主键从 1 起）。
func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullableInt 把 0 视为「未取得 HTTP 状态」写 NULL。
func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullableIntPtr 把 nil 写 NULL，非 nil 原样写值（0 也是有效值）。
func nullableIntPtr(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullableTime 把零值时间写 NULL。
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

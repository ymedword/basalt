package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是**定时触发的数据库协调面**（模块卡 §2.3.4 图 4 / §3.1.9）：到期扫描、
// 下一次触发时刻的行条件推进（CAS）、一次计划触发的事务内领取，以及错失 / 忙两条
// 跳过路径。触发权在数据库——进程内不再有定时器、互斥表或句柄。
// This file owns the database side of scheduled triggers: the due scan, the CAS
// advance of next_fire_at, the in-transaction claim of one planned instant, and the
// two skip paths (missed grace, task busy). Trigger ownership lives in the database.

// scheduleSkipBusyReason 是"该任务已有在途执行"这条跳过行的固定原因短语
// （模块卡 §3.1 错误/原因表：`task_executions.error_message` 只放固定短语）。
// scheduleSkipBusyReason is the fixed reason phrase of a busy skip row.
const scheduleSkipBusyReason = "task_busy"

// 到期扫描的两条 SQL 是**编译期常量**：全局版与「按 Agent 集合收窄」版只在常量之间
// 选择，运行时不做任何字符串拼接。Agent 谓词因此只能来自源码常量，不可能掺入外部
// 输入（gosec G202 的 SQL 拼接告警从结构上消失，而不是靠抑制注释）。
// The two due-scan queries are compile-time constants; the agent predicate is never
// concatenated at runtime.
const (
	dueTasksSelect = `
SELECT ` + taskRecordColumns + `, next_fire_at
  FROM scheduled_tasks
 WHERE enabled AND retired_at IS NULL`

	dueTasksTail = `
   AND (next_fire_at IS NULL
        OR next_fire_at <= (extract(epoch from clock_timestamp()) * 1000)::bigint)
 ORDER BY next_fire_at ASC NULLS FIRST, task_id ASC
 LIMIT $1::bigint`

	// dueTasksQuery 扫描全部 Agent 的到期行（`$1` 为 limit）。
	dueTasksQuery = dueTasksSelect + dueTasksTail

	// dueTasksLocalQuery 只扫描给定 Agent 集合的到期行（`$1` 为 limit，`$2` 为 text[]）。
	dueTasksLocalQuery = dueTasksSelect + `
   AND agent_name = ANY($2::text[])` + dueTasksTail
)

// dueRowScanner 在 `taskRecordColumns` 之后追加 `next_fire_at` 一列，让到期扫描与
// 全量列表共用同一份行投影与扫描代码（列序只有一处定义）。
// dueRowScanner appends next_fire_at to the shared task projection so the due scan and
// the full list share one column order.
type dueRowScanner struct {
	rows *sql.Rows
	next sql.NullInt64
}

// Scan 先按 `core.TaskRecord` 的列序扫描，再把 `next_fire_at` 读进附加字段。
// Scan reads the task projection and then the extra next_fire_at column.
func (d *dueRowScanner) Scan(dest ...any) error {
	return d.rows.Scan(append(dest, &d.next)...)
}

// ListDispatchable 返回 `limit` 条"enabled、未退休且 next_fire_at 为 NULL 或已到期"的
// 任务。排序固定为 `next_fire_at ASC NULLS FIRST, task_id ASC`：未初始化行优先初始化，
// 其余按到期先后处理——**不**用创建时间或任务名排序，否则积压时新任务会长期饿死。
//
// 扫描本身不加锁：真正互斥在 `ClaimDueTrigger` 的任务行锁 + CAS 上。这里拿行锁只会
// 把整批到期任务串行化，且事务一结束锁就失效，挡不住任何竞争。
// ListDispatchable returns enabled, unretired tasks whose next_fire_at is null or due.
// The scan takes no lock: mutual exclusion lives in ClaimDueTrigger's row lock and CAS.
func (s *TaskStore) ListDispatchable(ctx context.Context, limit int) ([]core.DueTask, error) {
	return s.listDue(ctx, dueTasksQuery, []any{limit}, limit)
}

// ListDispatchableForAgents 是 `ListDispatchable` 的按 Agent 收窄版本（`core.AgentDueLister`）：
// 只返回属于给定 Agent 集合的到期行。多 Worker 共用一张 `scheduled_tasks` 时，全局批次
// 会被其他工作区的到期行占满（它们既不归本进程领取，也不会被本进程推进），本地 Agent
// 因此永远进不了批次——按注册集合先收窄就消除了这种饿死（F6）。
// ListDispatchableForAgents narrows the due scan to the given agents so foreign rows can
// never occupy the whole batch.
func (s *TaskStore) ListDispatchableForAgents(ctx context.Context, agents []string,
	limit int,
) ([]core.DueTask, error) {
	if len(agents) == 0 {
		return nil, nil
	}
	return s.listDue(ctx, dueTasksLocalQuery, []any{limit, agents}, limit)
}

// listDue 是两条到期扫描共用的执行体：查询文本由调用方从上面两个常量里选定
// （参数顺序固定为 limit 在前、agents 在后），本函数不接收也不拼接任何 SQL 片段。
// listDue executes one of the two constant queries; it never concatenates SQL.
func (s *TaskStore) listDue(ctx context.Context, query string, args []any,
	limit int,
) ([]core.DueTask, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.pool.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("扫描到期定时任务: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.DueTask
	for rows.Next() {
		scanner := &dueRowScanner{rows: rows}
		rec, err := scanTaskRecord(scanner)
		if err != nil {
			return nil, fmt.Errorf("读取到期定时任务行: %w", err)
		}
		out = append(out, core.DueTask{Task: rec, NextFireAt: unixMilliOrZero(scanner.next)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历到期定时任务行: %w", err)
	}
	return out, nil
}

// DBNow 读数据库时钟（`core.DBNowReader`）：到期判定、错失宽限与下一次触发的基准
// 时刻必须与领取事务里的 `clock_timestamp()` 同源，否则多 Worker 之间、以及进程与
// 数据库之间的时钟偏差会让"到期 / 未到期"在不同环节得到相反的结论（F8）。
// DBNow returns the database clock so every scheduling decision shares one time basis.
func (s *TaskStore) DBNow(ctx context.Context) (time.Time, error) {
	var ms int64
	if err := s.db.pool.QueryRowContext(ctx,
		`SELECT (extract(epoch from clock_timestamp()) * 1000)::bigint`).Scan(&ms); err != nil {
		return time.Time{}, fmt.Errorf("读取数据库时钟: %w", err)
	}
	return time.UnixMilli(ms).UTC(), nil
}

// SetNextFire 只初始化（expected == nil）或 CAS 推进 next_fire_at，**不**执行任务。
//
// 初始化分支允许覆盖旧值：它只在 `next_fire_at IS NULL` 的迁移遗留行与新行上被调用，
// 覆盖是为了让"扫描到 NULL → 算一次 → 写回"这条路径可重入。推进分支必须带
// `expected`：0 行即 `core.ErrStaleNextFire`（另一 Worker 已推进，或任务已下线）——
// 不区分这两者，调用方对二者都只需放弃本轮。
// SetNextFire only initializes or CAS-advances next_fire_at; it never executes a task.
func (s *TaskStore) SetNextFire(ctx context.Context, taskID string, expected *time.Time, next time.Time) error {
	if taskID == "" {
		return fmt.Errorf("postgres: 推进下一次触发时任务标识为空")
	}
	if next.IsZero() {
		return fmt.Errorf("postgres: 下一次触发时刻为空（task_id=%s）", taskID)
	}
	if expected == nil {
		res, err := s.db.pool.ExecContext(ctx, `
UPDATE scheduled_tasks
   SET next_fire_at = $1, updated_at = (extract(epoch from clock_timestamp()) * 1000)::bigint
 WHERE task_id = $2 AND retired_at IS NULL`, next.UnixMilli(), taskID)
		if err != nil {
			return fmt.Errorf("初始化下一次触发（task_id=%s）: %w", taskID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认下一次触发初始化（task_id=%s）: %w", taskID, err)
		}
		if affected == 0 {
			return fmt.Errorf("初始化下一次触发（task_id=%s）: %w", taskID, fs.ErrNotExist)
		}
		return nil
	}

	if expected.IsZero() {
		return fmt.Errorf("postgres: 期望的下一次触发时刻为空（task_id=%s）", taskID)
	}
	res, err := s.db.pool.ExecContext(ctx, `
UPDATE scheduled_tasks
   SET next_fire_at = $1, updated_at = (extract(epoch from clock_timestamp()) * 1000)::bigint
 WHERE task_id = $2 AND next_fire_at = $3`, next.UnixMilli(), taskID, expected.UnixMilli())
	if err != nil {
		return fmt.Errorf("推进下一次触发（task_id=%s）: %w", taskID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认下一次触发推进（task_id=%s）: %w", taskID, err)
	}
	if affected == 0 {
		return fmt.Errorf("推进下一次触发（task_id=%s）: %w", taskID, core.ErrStaleNextFire)
	}
	return nil
}

// ClaimDueTrigger 在**一个事务**里领取一次计划触发（模块卡 §2.3.4 图 4）：
//
//	任务行 FOR UPDATE（退休即视为不存在）
//	→ 同一 planned_at 已有 cron 执行行 → ErrTriggerAlreadyClaimed
//	→ 同任务已有 started 执行行        → ErrTaskBusy
//	→ CAS 推进 next_fire_at（0 行）    → ErrStaleNextFire
//	→ INSERT started 执行行（planned_at / owner_worker_id / epoch=1）
//	→ INSERT schedule_execution 租约（epoch=1，立即生效）
//
// 互斥来自**任务行锁**：同一任务的领取、错失记录与忙跳过都在这一行上排队，因此
// 上面三次预判读到的都是最新已提交状态；`ux_task_executions_cron_planned` 与
// `ux_task_executions_active` 是最后一道物理防线，不靠它们产出正常结局。
// ClaimDueTrigger claims one planned instant in a single transaction; the task row
// lock serializes every trigger-state transition for that task.
func (s *TaskStore) ClaimDueTrigger(ctx context.Context, taskID string, plannedAt, nextFireAt time.Time,
	owner core.WorkerIdentity, ttl time.Duration,
) (core.DueExecution, error) {
	if owner.InstanceID == "" {
		return core.DueExecution{}, fmt.Errorf("postgres: 领取计划触发缺少领取者身份（task_id=%s）", taskID)
	}
	ttlMS, err := validateLeaseTTL(ttl)
	if err != nil {
		return core.DueExecution{}, err
	}
	if plannedAt.IsZero() || nextFireAt.IsZero() {
		return core.DueExecution{}, fmt.Errorf("postgres: 计划触发时刻为空（task_id=%s）", taskID)
	}

	var claimed core.DueExecution
	outcome, err := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockTaskRow(ctx, tx, taskID)
		if err != nil {
			return err
		}
		// 停用可能提交在到期扫描之后：任务行锁保证这里读到的是最新已提交的 `enabled`。
		// 停用即拒绝领取——不得留下 started 执行行与租约（FR-24-10 的启停语义）。
		// The disable may have committed after the due scan; under the task row lock
		// this recheck sees the latest enabled flag and rejects the claim outright.
		if !task.Enabled {
			return core.ErrTaskDisabled
		}
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		taken, err := executionPlannedExists(ctx, tx, taskID, plannedAt)
		if err != nil {
			return err
		}
		if taken {
			return core.ErrTriggerAlreadyClaimed
		}
		busy, err := executionStartedExists(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if busy {
			return core.ErrTaskBusy
		}

		res, err := tx.ExecContext(ctx, `
UPDATE scheduled_tasks
   SET next_fire_at = $1, updated_at = $2
 WHERE task_id = $3 AND next_fire_at = $4`,
			nextFireAt.UnixMilli(), nowMS, taskID, plannedAt.UnixMilli())
		if err != nil {
			return fmt.Errorf("推进下一次触发（task_id=%s）: %w", taskID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认下一次触发推进（task_id=%s）: %w", taskID, err)
		}
		if affected == 0 {
			return core.ErrStaleNextFire
		}

		// Run ID 在领取事务内分配：`task_executions.run_id` 必须与随后 Run 行同一个值，
		// 而冻结签名没有 RunID 入参（T014 裁决 ①）。时间段用计划时刻——同一次计划
		// 触发的多条线索在日志里可归并，随机段保证唯一。
		runID := core.NewRunID("scheduler", plannedAt.UTC(), core.RunIDSuffix())
		var id int64
		if err := tx.QueryRowContext(ctx, `
INSERT INTO task_executions(task_id, run_id, status, trigger, started_at,
                           planned_at, owner_worker_id, ownership_epoch)
VALUES ($1, $2, $3, $4, $5, $6, $7, 1)
RETURNING id`,
			taskID, runID, string(core.ExecutionStarted), string(core.TriggerCron),
			nowMS, plannedAt.UnixMilli(), owner.InstanceID).Scan(&id); err != nil {
			return fmt.Errorf("预写计划触发执行行（task_id=%s）: %w", taskID, err)
		}
		token, err := insertExecutionLease(ctx, tx, id, owner.InstanceID, nowMS, ttlMS)
		if err != nil {
			return err
		}
		claimed = core.DueExecution{
			Task:        task,
			ExecutionID: id,
			PlannedAt:   plannedAt.UTC(),
			Lease:       token,
			RunID:       runID,
		}
		return nil
	})
	if err != nil {
		return core.DueExecution{}, err
	}
	if outcome != TxCommitted {
		// 领取是否落库未知：**不得**重试、**不得**当未领取。调用方记 ERROR 后由下一轮
		// 到期扫描按真实行状态收敛。
		return core.DueExecution{}, fmt.Errorf("%w: 计划触发领取结局为 %q（task_id=%s）",
			ErrTxOutcomeUnknown, outcome, taskID)
	}
	return claimed, nil
}

// RecordMissedTrigger 为一次"超出宽限"的到期写一条 skipped 行并在**同一事务**里 CAS
// 推进 next_fire_at。
//
// 顺序是 CAS 在前：推进失败的调用方什么都没记录——说明另一 Worker 已处理过这个
// planned_at，记一行 skipped 反而会与真实执行重复。
//
// `missed` 是本次跳过覆盖的计划点数；调度器固定传 1（FR-24-12：不逐点补跑）。它
// **不落库**——契约把 `error_message` 固定为 reason 短语、行数固定一条；这里只做下界
// 校验，避免调用方误以为传 N 会写 N 行。
// RecordMissedTrigger writes one skipped row and CAS-advances next_fire_at in one
// transaction; the CAS runs first so a lost race records nothing.
func (s *TaskStore) RecordMissedTrigger(ctx context.Context, taskID string, plannedAt, nextFireAt time.Time,
	missed int, reason string,
) error {
	if missed < 1 {
		return fmt.Errorf("postgres: 错失记录的点数必须 ≥ 1（task_id=%s, missed=%d）", taskID, missed)
	}
	if reason == "" {
		return fmt.Errorf("postgres: 错失记录缺少原因短语（task_id=%s）", taskID)
	}
	return s.recordSkippedTrigger(ctx, taskID, plannedAt, nextFireAt, reason)
}

// RecordBusyTrigger 为一次"任务已有在途执行"的到期写一条 skipped 行并 CAS 推进
// next_fire_at。**必须**推进：该 planned_at 一旦落下 skipped 行，后续真正领取会永远
// 撞 `(task_id, planned_at)` 唯一约束——跳过行本身就是这个时刻的终态。
// RecordBusyTrigger must advance next_fire_at: the skipped row owns that planned_at.
func (s *TaskStore) RecordBusyTrigger(ctx context.Context, taskID string, plannedAt, nextFireAt time.Time) error {
	return s.recordSkippedTrigger(ctx, taskID, plannedAt, nextFireAt, scheduleSkipBusyReason)
}

// recordSkippedTrigger 是两条跳过路径的同一份实现：锁任务行 → CAS 推进 → 写 skipped
// 行（`started_at` 用计划时刻，`finished_at` 用数据库时刻）。
// recordSkippedTrigger is the single implementation behind both skip paths.
func (s *TaskStore) recordSkippedTrigger(ctx context.Context, taskID string,
	plannedAt, nextFireAt time.Time, reason string,
) error {
	if taskID == "" {
		return fmt.Errorf("postgres: 跳过记录缺少任务标识")
	}
	if plannedAt.IsZero() || nextFireAt.IsZero() {
		return fmt.Errorf("postgres: 跳过记录的时刻为空（task_id=%s）", taskID)
	}
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := lockTaskRow(ctx, tx, taskID); err != nil {
			return err
		}
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
UPDATE scheduled_tasks
   SET next_fire_at = $1, updated_at = $2
 WHERE task_id = $3 AND next_fire_at = $4`,
			nextFireAt.UnixMilli(), nowMS, taskID, plannedAt.UnixMilli())
		if err != nil {
			return fmt.Errorf("推进下一次触发（task_id=%s）: %w", taskID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认下一次触发推进（task_id=%s）: %w", taskID, err)
		}
		if affected == 0 {
			return core.ErrStaleNextFire
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO task_executions(task_id, run_id, status, trigger, duration_ms, error_message,
                            started_at, finished_at, planned_at)
VALUES ($1, NULL, $2, $3, 0, $4, $5, $6, $5)`,
			taskID, string(core.ExecutionSkipped), string(core.TriggerCron), reason,
			plannedAt.UnixMilli(), nowMS); err != nil {
			return fmt.Errorf("记录跳过执行（task_id=%s）: %w", taskID, err)
		}
		return nil
	})
}

// lockTaskRow 对任务行加 `FOR UPDATE` 并读回完整快照（只接受未退休行）。
// 同一任务的领取 / 错失 / 忙跳过 / 手动补跑都先取这把锁，锁序固定在"任务行 → 执行行
// → 租约行"，与 `StartRun` 的"advisory → 租约行 → 业务行"不成环。
// lockTaskRow takes the task row lock and returns the full snapshot; retired rows are
// treated as absent.
func lockTaskRow(ctx context.Context, tx *sql.Tx, taskID string) (core.TaskRecord, error) {
	rec, err := scanTaskRecord(tx.QueryRowContext(ctx,
		`SELECT `+taskRecordColumns+` FROM scheduled_tasks
		  WHERE task_id = $1 AND retired_at IS NULL FOR UPDATE`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return core.TaskRecord{}, fmt.Errorf("锁定定时任务行（task_id=%s）: %w", taskID, fs.ErrNotExist)
	}
	if err != nil {
		return core.TaskRecord{}, fmt.Errorf("锁定定时任务行（task_id=%s）: %w", taskID, err)
	}
	return rec, nil
}

// executionStartedExists 判同任务是否已有 `started` 执行行。读与写在同一个事务里、
// 且任务行锁已持有：这不是"快照猜测"，而是任务级串行化下的权威读。
// executionStartedExists is an authoritative read under the task row lock, not a
// snapshot guess.
func executionStartedExists(ctx context.Context, tx *sql.Tx, taskID string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM task_executions WHERE task_id = $1 AND status = $2)`,
		taskID, string(core.ExecutionStarted)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("检查在途执行（task_id=%s）: %w", taskID, err)
	}
	return exists, nil
}

// executionPlannedExists 判该任务在 plannedAt 这个计划时刻是否已有 cron 执行行
// （无论 started / completed / failed / skipped）——一次计划触发只有一条执行。
// executionPlannedExists reports whether the planned instant already has a cron row.
func executionPlannedExists(ctx context.Context, tx *sql.Tx, taskID string, plannedAt time.Time) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM task_executions
                WHERE task_id = $1 AND trigger = $2 AND planned_at = $3)`,
		taskID, string(core.TriggerCron), plannedAt.UnixMilli()).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("检查计划触发是否已领取（task_id=%s）: %w", taskID, err)
	}
	return exists, nil
}

// insertExecutionLease 为新执行行写 `schedule_execution` 租约（epoch=1）与 claimed 事件。
// 执行行 id 来自 identity 序列、永不复用，因此正常路径不会撞已有租约；`ON CONFLICT
// DO NOTHING` + 0 行 → `core.ErrLeaseHeld` 只是 fail-closed 的最后一道。
// insertExecutionLease writes the schedule_execution lease and its claimed event.
func insertExecutionLease(ctx context.Context, tx *sql.Tx, executionID int64,
	ownerID string, nowMS, ttlMS int64,
) (core.LeaseToken, error) {
	resourceID := strconv.FormatInt(executionID, 10)
	res, err := tx.ExecContext(ctx, `
INSERT INTO execution_leases(kind, resource_id, owner_worker_id, ownership_epoch,
                             acquired_at, renewed_at, expires_at, released_at)
VALUES ($1, $2, $3, 1, $4, $4, $4::bigint + $5::bigint, NULL)
ON CONFLICT (kind, resource_id) DO NOTHING`,
		string(core.LeaseScheduleExecution), resourceID, ownerID, nowMS, ttlMS)
	if err != nil {
		return core.LeaseToken{}, fmt.Errorf("创建执行租约（execution_id=%d）: %w", executionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return core.LeaseToken{}, fmt.Errorf("读取执行租约插入结果（execution_id=%d）: %w", executionID, err)
	}
	if affected == 0 {
		return core.LeaseToken{}, fmt.Errorf("创建执行租约（execution_id=%d）: %w", executionID, core.ErrLeaseHeld)
	}
	if err := appendLeaseEvent(ctx, tx, core.LeaseScheduleExecution, resourceID, "claimed",
		ownerID, 1, "", sql.NullInt64{}, "", nowMS); err != nil {
		return core.LeaseToken{}, err
	}
	return core.LeaseToken{
		Kind:           core.LeaseScheduleExecution,
		ResourceID:     resourceID,
		OwnerWorkerID:  ownerID,
		OwnershipEpoch: 1,
	}, nil
}

// releaseExecutionLease 在收尾事务内释放租约并记 released 事件。调用方必须已经在该
// 事务里通过 `fencedTx` 锁住并校验过同一行——所以这里 0 行是内部矛盾，不是竞争失败。
// releaseExecutionLease releases the lease inside the fenced closeout transaction.
func releaseExecutionLease(ctx context.Context, tx *sql.Tx, token core.LeaseToken, nowMS int64) error {
	res, err := tx.ExecContext(ctx, `
UPDATE execution_leases
   SET released_at = $4
 WHERE kind = $1 AND resource_id = $2 AND owner_worker_id = $3
   AND ownership_epoch = $5 AND released_at IS NULL`,
		string(token.Kind), token.ResourceID, token.OwnerWorkerID, nowMS, token.OwnershipEpoch)
	if err != nil {
		return fmt.Errorf("释放执行租约（resource_id=%s）: %w", token.ResourceID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取执行租约释放结果（resource_id=%s）: %w", token.ResourceID, err)
	}
	if affected != 1 {
		return fmt.Errorf("postgres: 持锁期间执行租约已变化（resource_id=%s）", token.ResourceID)
	}
	return appendLeaseEvent(ctx, tx, token.Kind, token.ResourceID, "released",
		token.OwnerWorkerID, token.OwnershipEpoch, "", sql.NullInt64{}, "", nowMS)
}

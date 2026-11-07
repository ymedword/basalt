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

// 本文件是调度两表（scheduled_tasks / task_executions）的**唯一 SQL 入口**。表 / 列 /
// 状态字面量以 docs/contracts/schema.pg.sql 为准，时间列一律 BIGINT UnixMilli（UTC 毫秒）。
// This file is the only SQL entry point for the scheduling tables.
//
// 四条纪律。① **定义与运行状态分离**：`ApplyAgentTasks` 的 upsert 只写定义列与
// `retired_at`，从不覆盖 `agent_name` / `enabled` / `last_*` / `created_at`——文件对齐是
// 一件事，运行痕迹是另一件事（约束 16）。`next_fire_at` 不是运行痕迹：它是**按当前
// cron / zone 算出的调度游标**，表达式一变就必须置 NULL 交给下一轮到期扫描按新定义
// 重新初始化（否则新 cron 可能被旧游标挡到很远以后，或在非触发点上被领取）。
// ② **归属守卫在 SQL 里**：upsert 的 WHERE 谓词与
// retire 的预检只看 `agent_name`，冲突即拒绝并回滚整个事务（约束 41 / 42）；retired 行同样
// 受保护——下线不释放归属。③ **无 Run 即无引用**：执行行终态写 `run_id = NULL` 的判据是
// "`runs` 表是否真的存在该行"，不是状态取值（P-04）——会话忙 / 准入失败 / 崩溃遗留都不会
// 留下指向不存在 Run 的悬空引用。④ **归属三列只在插入时写**：`caller_id` /
// `authorizing_kind` / `authorizing_key_id` 随定义行建立时固定，冲突分支**不**更新它们
// ——执行期不得改写（C-32；再绑定只经 KeyStore.Rotate 的事务路径）。
// Four rules: definitions never overwrite run state, ownership is guarded inside SQL,
// a terminal execution row only keeps run_id when the runs row exists, and the three
// ownership columns are written only on insert, never rewritten by execution.

// TaskStore 是 core.TaskStore 的 PostgreSQL 实现。
// TaskStore is the PostgreSQL implementation of core.TaskStore.
type TaskStore struct{ db *DB }

// 编译期断言：TaskStore 满足消费方（core）声明的仓储接口。
// Compile-time assertion that TaskStore satisfies the consumer-declared interface.
var _ core.TaskStore = (*TaskStore)(nil)

// NewTaskStore 绑定一个已打开的写模式库句柄。
// NewTaskStore binds the store to an opened writable database handle.
func NewTaskStore(db *DB) *TaskStore { return &TaskStore{db: db} }

// taskRecordColumns 是 `core.TaskRecord` 的投影列；顺序即 `scanTaskRecord` 的扫描顺序。
// `ListTasks`（全量）与 `ListDispatchable`（到期扫描）共用同一份列序，避免两处漂移。
// taskRecordColumns is the projection of core.TaskRecord shared by ListTasks and
// ListDispatchable; the order is the scan order.
const taskRecordColumns = `task_id, agent_name, cron, zone, message, deliver_to, enabled,
	        definition_hash, last_run_at, last_status, retired_at, created_at, updated_at,
	        caller_id, authorizing_kind, authorizing_key_id`

// scanTaskRecord 把一行 `taskRecordColumns` 投影读成 `core.TaskRecord`（`rowScanner`
// 在 run_query.go 声明，单行与多行共用同一次扫描）。
// scanTaskRecord maps one projected row onto core.TaskRecord.
func scanTaskRecord(row rowScanner) (core.TaskRecord, error) {
	var (
		rec        core.TaskRecord
		lastRunAt  sql.NullInt64
		lastStatus sql.NullString
		retiredAt  sql.NullInt64
		createdAt  int64
		updatedAt  int64
		authKind   string
		keyID      sql.NullString
	)
	if err := row.Scan(&rec.TaskID, &rec.Agent, &rec.Cron, &rec.Zone, &rec.Message,
		&rec.DeliverTo, &rec.Enabled, &rec.DefinitionHash, &lastRunAt, &lastStatus, &retiredAt,
		&createdAt, &updatedAt, &rec.CallerID, &authKind, &keyID); err != nil {
		return core.TaskRecord{}, err
	}
	// 归属与授权来源是 NOT NULL 列（schema 的 CHECK 也保证组合成立）：原样读出，
	// **不**做任何"空则补管理员"的兜底——缺值只可能是库被绕过写入，交由调度器
	// 按 fail-closed 拒绝。
	// The ownership columns are NOT NULL and read verbatim; there is no fallback.
	rec.AuthorizingKind = core.AuthorizingKind(authKind)
	rec.AuthorizingKeyID = keyID.String
	rec.LastRunAt = unixMilliOrZero(lastRunAt)
	rec.LastStatus = core.ExecutionStatus(lastStatus.String)
	rec.RetiredAt = unixMilliOrZero(retiredAt)
	rec.CreatedAt = time.UnixMilli(createdAt).UTC()
	rec.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return rec, nil
}

// ListTasks 按 task_id 升序返回**全部**任务行（含 retired 行，由 core 做可见性过滤）。
// ListTasks returns every row ordered by task_id, retired rows included; core filters.
func (s *TaskStore) ListTasks(ctx context.Context) ([]core.TaskRecord, error) {
	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT `+taskRecordColumns+` FROM scheduled_tasks ORDER BY task_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("列出定时任务: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.TaskRecord
	for rows.Next() {
		rec, err := scanTaskRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("读取定时任务行: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描定时任务行: %w", err)
	}
	return out, nil
}

// ApplyAgentTasks 在**同一事务**内应用该 Agent 的全部定义更新与退休：任一条归属冲突或
// 写失败即整体回滚——库要么完整保留旧定义，要么完整落到新定义，不存在部分生效。
// ApplyAgentTasks applies all definition upserts and retirements in one transaction;
// any ownership conflict or write failure rolls the whole batch back.
func (s *TaskStore) ApplyAgentTasks(ctx context.Context, agent string, upserts []core.TaskRecord,
	retireIDs []string, now time.Time,
) error {
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, task := range upserts {
			// 归属三列是 NOT NULL / CHECK 约束列：装配层漏填时在这里就给出可读的
			// 内部错误，而不是让 PostgreSQL 报一条约束违例（错因不可判）。
			// The three ownership columns are NOT NULL with CHECKs; validate here so a
			// missing value fails with a readable internal error.
			if !task.AuthorizingKind.Valid() {
				return fmt.Errorf("定时任务缺少授权来源（task_id=%s）", task.TaskID)
			}
			// 归属谓词在 ON CONFLICT 的 DO UPDATE 上：命中同 owner → 更新定义并清 retired_at；
			// 命中其他 owner → 0 行受影响（不是 SQL 错误），由下面统一转成 ErrTaskOwnerConflict。
			// 不写 agent_name / enabled / last_* / created_at——运行状态只归启停与执行路径；
			// 也不更新归属三列——授权来源只在建立时固定（C-32）。
			res, err := tx.ExecContext(ctx,
				`INSERT INTO scheduled_tasks
				   (task_id, agent_name, cron, zone, message, deliver_to, enabled, definition_hash,
				    last_run_at, last_status, retired_at, created_at, updated_at,
				    caller_id, authorizing_kind, authorizing_key_id)
				 VALUES ($1, $2, $3, $4, $5, $6, true, $7, NULL, NULL, NULL, $8, $9, $10, $11, $12)
				 ON CONFLICT(task_id) DO UPDATE SET
				   cron            = excluded.cron,
				   zone            = excluded.zone,
				   message         = excluded.message,
				   deliver_to      = excluded.deliver_to,
				   definition_hash = excluded.definition_hash,
				   -- 调度游标随表达式重排：cron / zone 变了，旧 next_fire_at 是按旧表达
				   -- 式算出的，既可能落在新表达式的非触发点上，也可能把新定义挡在很远
				   -- 之后。置 NULL 后由下一轮到期扫描按新表达式初始化（只初始化、不执行）。
				   -- Rearm the schedule cursor when the expression changes; NULL makes the
				   -- next due scan initialize it from the new cron without executing.
				   next_fire_at    = CASE
				                       WHEN scheduled_tasks.cron IS DISTINCT FROM excluded.cron
				                         OR scheduled_tasks.zone IS DISTINCT FROM excluded.zone
				                       THEN NULL
				                       ELSE scheduled_tasks.next_fire_at
				                     END,
				   updated_at      = excluded.updated_at,
				   retired_at      = NULL
				 WHERE scheduled_tasks.agent_name = excluded.agent_name`,
				task.TaskID, agent, task.Cron, task.Zone, task.Message, task.DeliverTo,
				task.DefinitionHash, now.UnixMilli(), now.UnixMilli(),
				task.CallerID, string(task.AuthorizingKind), nullIfEmpty(task.AuthorizingKeyID))
			if err != nil {
				return fmt.Errorf("写入定时任务（task_id=%s）: %w", task.TaskID, err)
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("确认定时任务写入（task_id=%s）: %w", task.TaskID, err)
			}
			if affected == 0 {
				return fmt.Errorf("定时任务归属冲突（task_id=%s, agent=%s）: %w",
					task.TaskID, agent, core.ErrTaskOwnerConflict)
			}
		}

		for _, id := range retireIDs {
			// 退休前先判归属：未知 id 静默放行（UPDATE 影响 0 行，幂等）；
			// retired 行同样拒绝其他 Agent——下线不释放归属（约束 17 / 42）。
			var owner string
			err := tx.QueryRowContext(ctx,
				`SELECT agent_name FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&owner)
			switch {
			case err == nil:
				if owner != agent {
					return fmt.Errorf("定时任务归属冲突（task_id=%s 属于 %s）: %w",
						id, owner, core.ErrTaskOwnerConflict)
				}
			case errors.Is(err, sql.ErrNoRows):
				// 未知 id：与 UPDATE 影响 0 行同义，保持幂等。
			default:
				return fmt.Errorf("查询定时任务归属（task_id=%s）: %w", id, err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE scheduled_tasks SET retired_at = $1, updated_at = $2
				  WHERE task_id = $3 AND retired_at IS NULL`,
				now.UnixMilli(), now.UnixMilli(), id); err != nil {
				return fmt.Errorf("退休定时任务（task_id=%s）: %w", id, err)
			}
		}
		return nil
	})
}

// SetTaskEnabled 翻转启停；`AND retired_at IS NULL` 兜底并发下线，0 行 → fs.ErrNotExist。
// SetTaskEnabled flips the enabled flag; zero matched rows means the task vanished or
// was retired concurrently and maps to fs.ErrNotExist.
func (s *TaskStore) SetTaskEnabled(ctx context.Context, taskID string, enabled bool, now time.Time) error {
	res, err := s.db.pool.ExecContext(ctx,
		`UPDATE scheduled_tasks SET enabled = $1, updated_at = $2
		  WHERE task_id = $3 AND retired_at IS NULL`, enabled, now.UnixMilli(), taskID)
	if err != nil {
		return fmt.Errorf("启停定时任务（task_id=%s）: %w", taskID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认启停写入（task_id=%s）: %w", taskID, err)
	}
	if affected == 0 {
		return fmt.Errorf("启停定时任务（task_id=%s）: %w", taskID, fs.ErrNotExist)
	}
	return nil
}

// MarkTaskRun 一次写齐 last_run_at / last_status / updated_at（"本轮已开始"的痕迹）。
// MarkTaskRun records the three run-trace columns in one statement.
func (s *TaskStore) MarkTaskRun(ctx context.Context, taskID string, at time.Time, status core.ExecutionStatus) error {
	if _, err := s.db.pool.ExecContext(ctx,
		`UPDATE scheduled_tasks SET last_run_at = $1, last_status = $2, updated_at = $3
		  WHERE task_id = $4`, at.UnixMilli(), string(status), at.UnixMilli(), taskID); err != nil {
		return fmt.Errorf("记录任务运行痕迹（task_id=%s）: %w", taskID, err)
	}
	return nil
}

// StartExecution 在**同一事务**里写 `status = 'started'` 的执行行与
// `schedule_execution` 租约（手动补跑路径；定时触发走 `ClaimDueTrigger` 的同一套写法）。
//
// 同任务已有 `started` 执行 → `core.ErrTaskBusy`：事务内先判一次给出可区分的哨兵，
// 部分唯一索引 `ux_task_executions_active` 仍是最后一道（并发下两笔同时通过预判时
// 由索引拒绝，错误原样上抛——那说明任务级串行被绕过，不该静默吞掉）。
// StartExecution writes the started row and its lease in one transaction; an existing
// started row for the same task yields core.ErrTaskBusy.
func (s *TaskStore) StartExecution(ctx context.Context, rec core.ExecutionRecord,
	owner core.WorkerIdentity, ttl time.Duration,
) (int64, core.LeaseToken, error) {
	if owner.InstanceID == "" {
		return 0, core.LeaseToken{}, fmt.Errorf("postgres: 起始执行缺少领取者身份（task_id=%s）", rec.TaskID)
	}
	ttlMS, err := validateLeaseTTL(ttl)
	if err != nil {
		return 0, core.LeaseToken{}, err
	}
	if rec.RunID == "" {
		return 0, core.LeaseToken{}, fmt.Errorf("postgres: 起始执行缺少 RunID（task_id=%s）", rec.TaskID)
	}

	var (
		id    int64
		token core.LeaseToken
	)
	_, err = s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 任务行锁把同一任务的全部触发状态转换串起来：并发下线 / 并发补跑都在这里排队，
		// 预判因此是权威读而不是快照猜测。
		if _, err := lockTaskRow(ctx, tx, rec.TaskID); err != nil {
			return err
		}
		busy, err := executionStartedExists(ctx, tx, rec.TaskID)
		if err != nil {
			return err
		}
		if busy {
			return core.ErrTaskBusy
		}
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		startedMS := rec.StartedAt.UnixMilli()
		if rec.StartedAt.IsZero() {
			startedMS = nowMS
		}
		if err := tx.QueryRowContext(ctx, `
INSERT INTO task_executions(task_id, run_id, status, trigger, started_at,
                           planned_at, owner_worker_id, ownership_epoch)
VALUES ($1, $2, $3, $4, $5, NULL, $6, 1)
RETURNING id`,
			rec.TaskID, rec.RunID, string(core.ExecutionStarted), string(rec.Trigger),
			startedMS, owner.InstanceID).Scan(&id); err != nil {
			return fmt.Errorf("预写任务执行行（task_id=%s）: %w", rec.TaskID, err)
		}
		token, err = insertExecutionLease(ctx, tx, id, owner.InstanceID, nowMS, ttlMS)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, core.LeaseToken{}, err
	}
	return id, token, nil
}

// FinishExecution 带 token 做 fencing 写执行行终态：**同一事务**内回填终态、同步
// `scheduled_tasks.last_status`（含 skipped——任何终态都要让任务列表说真话）并释放
// `schedule_execution` 租约。`run_id` 的保留判据是该行在 `runs` 表是否真实存在：
// 会话忙 / 准入失败等未产生 Run 的终态一律置 NULL（P-04）。
//
// 已终态的行幂等返回（重试不重写、不再写事件）；失权由 `fencedTx` 以 `ErrLeaseLost`
// 拒绝——接管提交后旧属主重读一定看到新代次。
// FinishExecution fences on the token, writes the terminal row, syncs last_status and
// releases the lease in one transaction; an already-terminal row is an idempotent no-op.
func (s *TaskStore) FinishExecution(ctx context.Context, id int64, token core.LeaseToken,
	status core.ExecutionStatus, duration time.Duration, errMsg string, at time.Time,
) error {
	if token.Kind != core.LeaseScheduleExecution || token.ResourceID != strconv.FormatInt(id, 10) {
		return fmt.Errorf("postgres: 执行行收尾的执行权凭证不匹配（id=%d, kind=%s）", id, token.Kind)
	}
	return fencedTx(ctx, s.db, token, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE task_executions
			    SET status = $1, duration_ms = $2, error_message = $3, finished_at = $4,
			        run_id = CASE WHEN EXISTS (SELECT 1 FROM runs r WHERE r.run_id = task_executions.run_id)
			                      THEN run_id ELSE NULL END
			  WHERE id = $5 AND status = $6`,
			string(status), duration.Milliseconds(), nullIfEmpty(errMsg), at.UnixMilli(),
			id, string(core.ExecutionStarted))
		if err != nil {
			return fmt.Errorf("回填任务执行终态（id=%d）: %w", id, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认任务执行终态写入（id=%d）: %w", id, err)
		}
		if affected == 0 {
			// 0 行只有两种可能：行已被收尾（幂等）或行不存在（数据损坏）。
			// 失权在第 0 行之前就已由 fencedTx 判掉，不会走到这里。
			var current string
			err := tx.QueryRowContext(ctx,
				`SELECT status FROM task_executions WHERE id = $1`, id).Scan(&current)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("回填任务执行终态（id=%d）: %w", id, fs.ErrNotExist)
			case err != nil:
				return fmt.Errorf("读取任务执行状态（id=%d）: %w", id, err)
			case current == string(core.ExecutionStarted):
				// 条件更新没命中却仍是 started：只可能是并发写绕过了行锁。
				return fmt.Errorf("postgres: 执行行收尾未命中（id=%d）", id)
			default:
				return nil // 已终态：重试幂等
			}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE scheduled_tasks SET last_status = $1
			  WHERE task_id = (SELECT task_id FROM task_executions WHERE id = $2)`,
			string(status), id); err != nil {
			return fmt.Errorf("同步任务最后状态（id=%d）: %w", id, err)
		}
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		return releaseExecutionLease(ctx, tx, token, nowMS)
	})
}

// readExecutionForRecovery 读 A2 需要的执行行读数：本行状态、起始时刻与**关联 Run
// 的当前状态**（LEFT JOIN——预写后尚未建 Run 时 run_id 为 NULL，那是"无 Run"分支，
// 不是缺失数据）。返回 found=false 表示行本身不存在（数据损坏），由调用方决定怎么报。
//
// 读出来的 run 状态只用于**分类**：权威判据仍在 `FinishExecution` 的 fence 里
// （`WHERE id=$5 AND status='started'`），快照过期不会写出错误终态。
// readExecutionForRecovery gathers A2's inputs; the authoritative guard stays inside
// FinishExecution's fence, so a stale snapshot cannot produce a wrong terminal value.
func (s *TaskStore) readExecutionForRecovery(ctx context.Context, id int64) (executionRecoveryRow, bool, error) {
	var row executionRecoveryRow
	err := s.db.pool.QueryRowContext(ctx,
		`SELECT e.status, e.started_at, r.status
		   FROM task_executions e
		   LEFT JOIN runs r ON r.run_id = e.run_id
		  WHERE e.id = $1`, id).Scan(&row.status, &row.startedAt, &row.runStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return executionRecoveryRow{}, false, nil
	}
	if err != nil {
		return executionRecoveryRow{}, false, fmt.Errorf("读取待收尾的执行行（id=%d）: %w", id, err)
	}
	return row, true, nil
}

// RecordSkipped 直接插入零时长的 skipped 终态行（锁失路径）：无 Run、无 started 前驱，
// 因而不触碰 `scheduled_tasks.last_*`——列表里的"上次结果"仍指上一轮真实执行。
// RecordSkipped inserts a zero-duration terminal row for a lock miss; with no Run and no
// started predecessor it deliberately leaves the task's last_* columns untouched.
func (s *TaskStore) RecordSkipped(ctx context.Context, rec core.ExecutionRecord) error {
	at := rec.StartedAt.UnixMilli()
	_, err := s.db.pool.ExecContext(ctx,
		`INSERT INTO task_executions
		   (task_id, run_id, status, trigger, duration_ms, error_message, started_at, finished_at)
		 VALUES ($1, NULL, $2, $3, 0, NULL, $4, $5)`,
		rec.TaskID, string(core.ExecutionSkipped), string(rec.Trigger), at, at)
	if err != nil {
		return fmt.Errorf("记录跳过执行（task_id=%s）: %w", rec.TaskID, err)
	}
	return nil
}

// ListExecutions 按 started_at 降序（同刻按 id 降序）返回某任务的执行历史，最多 limit 条。
// ListExecutions returns the newest execution rows first, capped at limit.
func (s *TaskStore) ListExecutions(ctx context.Context, taskID string, limit int) ([]core.ExecutionRecord, error) {
	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT e.id, e.task_id, e.run_id, e.status, e.trigger, e.duration_ms, e.error_message,
		        d.status, d.channel_uid, d.detail, e.started_at, e.finished_at, e.planned_at
		   FROM task_executions e
		   LEFT JOIN schedule_deliveries d ON d.run_id = e.run_id
		  WHERE e.task_id = $1
		  ORDER BY e.started_at DESC, e.id DESC LIMIT $2::bigint`, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("列出任务执行历史（task_id=%s）: %w", taskID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.ExecutionRecord
	for rows.Next() {
		var (
			rec             core.ExecutionRecord
			runID           sql.NullString
			status          string
			trigger         string
			durationMS      sql.NullInt64
			errMsg          sql.NullString
			deliveryStatus  sql.NullString
			deliveryChannel sql.NullString
			deliveryDetail  sql.NullString
			startedAt       int64
			finishedAt      sql.NullInt64
			plannedAt       sql.NullInt64
		)
		if err := rows.Scan(&rec.ID, &rec.TaskID, &runID, &status, &trigger,
			&durationMS, &errMsg, &deliveryStatus, &deliveryChannel, &deliveryDetail,
			&startedAt, &finishedAt, &plannedAt); err != nil {
			return nil, fmt.Errorf("读取任务执行行: %w", err)
		}
		rec.RunID = runID.String
		rec.Status = core.ExecutionStatus(status)
		rec.Trigger = core.Trigger(trigger)
		rec.DurationMS = durationMS.Int64
		rec.ErrorMessage = errMsg.String
		rec.Delivery = core.DeliveryStatus(deliveryStatus.String)
		rec.DeliveryChannel = deliveryChannel.String
		rec.DeliveryDetail = deliveryDetail.String
		rec.StartedAt = time.UnixMilli(startedAt).UTC()
		rec.FinishedAt = unixMilliOrZero(finishedAt)
		if plannedAt.Valid {
			planned := time.UnixMilli(plannedAt.Int64).UTC()
			rec.PlannedAt = &planned
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描任务执行行: %w", err)
	}
	return out, nil
}

// unixMilliOrZero 把可空毫秒列映射为零值时间：NULL → `time.Time{}`，非空 → UTC 时刻。
// unixMilliOrZero maps a nullable Unix-millisecond column to a zero time when NULL.
func unixMilliOrZero(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64).UTC()
}

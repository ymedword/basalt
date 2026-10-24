package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是调度两表（scheduled_tasks / task_executions）的**唯一 SQL 入口**。表 / 列 /
// 状态字面量以 docs/contracts/schema.sql 为准，时间列一律 INTEGER UnixMilli（UTC 毫秒）。
// This file is the only SQL entry point for the scheduling tables.
//
// 三条纪律。① **定义与运行状态分离**：`ApplyAgentTasks` 的 upsert 只写定义列与
// `retired_at`，从不覆盖 `agent_name` / `enabled` / `last_*` / `created_at`——文件对齐是
// 一件事，运行痕迹是另一件事（约束 16）。② **归属守卫在 SQL 里**：upsert 的 WHERE 谓词与
// retire 的预检只看 `agent_name`，冲突即拒绝并回滚整个事务（约束 41 / 42）；retired 行同样
// 受保护——下线不释放归属。③ **无 Run 即无引用**：执行行终态写 `run_id = NULL` 的判据是
// "`runs` 表是否真的存在该行"，不是状态取值（P-04）——会话忙 / 准入失败 / 崩溃遗留都不会
// 留下指向不存在 Run 的悬空引用。
// Three rules: definitions never overwrite run state, ownership is guarded inside SQL,
// and a terminal execution row only keeps run_id when the runs row actually exists.

// TaskStore 是 core.TaskStore 的 SQLite 实现。
// TaskStore is the SQLite implementation of core.TaskStore.
type TaskStore struct{ db *DB }

// 编译期断言：TaskStore 满足消费方（core）声明的仓储接口。
// Compile-time assertion that TaskStore satisfies the consumer-declared interface.
var _ core.TaskStore = (*TaskStore)(nil)

// NewTaskStore 绑定一个已打开的写模式库句柄。
// NewTaskStore binds the store to an opened writable database handle.
func NewTaskStore(db *DB) *TaskStore { return &TaskStore{db: db} }

// ListTasks 按 task_id 升序返回**全部**任务行（含 retired 行，由 core 做可见性过滤）。
// ListTasks returns every row ordered by task_id, retired rows included; core filters.
func (s *TaskStore) ListTasks(ctx context.Context) ([]core.TaskRecord, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT task_id, agent_name, cron, zone, message, deliver_to, enabled, definition_hash,
		        last_run_at, last_status, retired_at, created_at, updated_at
		   FROM scheduled_tasks ORDER BY task_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("列出定时任务: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.TaskRecord
	for rows.Next() {
		var (
			rec        core.TaskRecord
			lastRunAt  sql.NullInt64
			lastStatus sql.NullString
			retiredAt  sql.NullInt64
			createdAt  int64
			updatedAt  int64
		)
		if err := rows.Scan(&rec.TaskID, &rec.Agent, &rec.Cron, &rec.Zone, &rec.Message,
			&rec.DeliverTo, &rec.Enabled, &rec.DefinitionHash, &lastRunAt, &lastStatus, &retiredAt,
			&createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("读取定时任务行: %w", err)
		}
		rec.LastRunAt = unixMilliOrZero(lastRunAt)
		rec.LastStatus = core.ExecutionStatus(lastStatus.String)
		rec.RetiredAt = unixMilliOrZero(retiredAt)
		rec.CreatedAt = time.UnixMilli(createdAt).UTC()
		rec.UpdatedAt = time.UnixMilli(updatedAt).UTC()
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
	return withWriteLock(ctx, s.db.write, func(ctx context.Context, conn *sql.Conn) error {
		for _, task := range upserts {
			// 归属谓词在 ON CONFLICT 的 DO UPDATE 上：命中同 owner → 更新定义并清 retired_at；
			// 命中其他 owner → 0 行受影响（不是 SQL 错误），由下面统一转成 ErrTaskOwnerConflict。
			// 不写 agent_name / enabled / last_* / created_at——运行状态只归启停与执行路径。
			res, err := conn.ExecContext(ctx,
				`INSERT INTO scheduled_tasks
				   (task_id, agent_name, cron, zone, message, deliver_to, enabled, definition_hash,
				    last_run_at, last_status, retired_at, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, 1, ?, NULL, NULL, NULL, ?, ?)
				 ON CONFLICT(task_id) DO UPDATE SET
				   cron            = excluded.cron,
				   zone            = excluded.zone,
				   message         = excluded.message,
				   deliver_to      = excluded.deliver_to,
				   definition_hash = excluded.definition_hash,
				   updated_at      = excluded.updated_at,
				   retired_at      = NULL
				 WHERE scheduled_tasks.agent_name = excluded.agent_name`,
				task.TaskID, agent, task.Cron, task.Zone, task.Message, task.DeliverTo,
				task.DefinitionHash, now.UnixMilli(), now.UnixMilli())
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
			err := conn.QueryRowContext(ctx,
				`SELECT agent_name FROM scheduled_tasks WHERE task_id = ?`, id).Scan(&owner)
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
			if _, err := conn.ExecContext(ctx,
				`UPDATE scheduled_tasks SET retired_at = ?, updated_at = ?
				  WHERE task_id = ? AND retired_at IS NULL`,
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
	res, err := s.db.write.ExecContext(ctx,
		`UPDATE scheduled_tasks SET enabled = ?, updated_at = ?
		  WHERE task_id = ? AND retired_at IS NULL`, enabled, now.UnixMilli(), taskID)
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
	if _, err := s.db.write.ExecContext(ctx,
		`UPDATE scheduled_tasks SET last_run_at = ?, last_status = ?, updated_at = ?
		  WHERE task_id = ?`, at.UnixMilli(), string(status), at.UnixMilli(), taskID); err != nil {
		return fmt.Errorf("记录任务运行痕迹（task_id=%s）: %w", taskID, err)
	}
	return nil
}

// StartExecution 插入 `status = 'started'` 的执行行并返回自增 id；起始态由 SQL 固定，
// 调用方没有传错的可能（与 StartRun 同一手法）。
// StartExecution inserts the started row and returns its auto-increment id; the status
// is fixed here, not accepted from the caller.
func (s *TaskStore) StartExecution(ctx context.Context, rec core.ExecutionRecord) (int64, error) {
	res, err := s.db.write.ExecContext(ctx,
		`INSERT INTO task_executions(task_id, run_id, status, trigger, started_at)
		 VALUES (?, ?, ?, ?, ?)`,
		rec.TaskID, nullIfEmpty(rec.RunID), string(core.ExecutionStarted),
		string(rec.Trigger), rec.StartedAt.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("预写任务执行行（task_id=%s）: %w", rec.TaskID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("读取任务执行行号（task_id=%s）: %w", rec.TaskID, err)
	}
	return id, nil
}

// FinishExecution 写执行行终态并在**同一事务**内同步 `scheduled_tasks.last_status`
// （含 skipped——任何终态都要让任务列表说真话）。`run_id` 的保留判据是该行在 `runs`
// 表是否真实存在：会话忙 / 准入失败等未产生 Run 的终态一律置 NULL（P-04）。
// FinishExecution writes the terminal row and syncs last_status in the same transaction.
// run_id survives only when the runs row actually exists.
func (s *TaskStore) FinishExecution(ctx context.Context, id int64, status core.ExecutionStatus,
	duration time.Duration, errMsg string, at time.Time,
) error {
	return withWriteLock(ctx, s.db.write, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx,
			`UPDATE task_executions
			    SET status = ?, duration_ms = ?, error_message = ?, finished_at = ?,
			        run_id = CASE WHEN EXISTS (SELECT 1 FROM runs r WHERE r.run_id = task_executions.run_id)
			                      THEN run_id ELSE NULL END
			  WHERE id = ?`,
			string(status), duration.Milliseconds(), nullIfEmpty(errMsg), at.UnixMilli(), id)
		if err != nil {
			return fmt.Errorf("回填任务执行终态（id=%d）: %w", id, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认任务执行终态写入（id=%d）: %w", id, err)
		}
		if affected == 0 {
			// 未知行号：如实报错，且**不**写 last_status——否则会误伤同任务已有状态。
			return fmt.Errorf("回填任务执行终态（id=%d）: %w", id, fs.ErrNotExist)
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE scheduled_tasks SET last_status = ?
			  WHERE task_id = (SELECT task_id FROM task_executions WHERE id = ?)`,
			string(status), id); err != nil {
			return fmt.Errorf("同步任务最后状态（id=%d）: %w", id, err)
		}
		return nil
	})
}

// RecordSkipped 直接插入零时长的 skipped 终态行（锁失路径）：无 Run、无 started 前驱，
// 因而不触碰 `scheduled_tasks.last_*`——列表里的"上次结果"仍指上一轮真实执行。
// RecordSkipped inserts a zero-duration terminal row for a lock miss; with no Run and no
// started predecessor it deliberately leaves the task's last_* columns untouched.
func (s *TaskStore) RecordSkipped(ctx context.Context, rec core.ExecutionRecord) error {
	at := rec.StartedAt.UnixMilli()
	_, err := s.db.write.ExecContext(ctx,
		`INSERT INTO task_executions
		   (task_id, run_id, status, trigger, duration_ms, error_message, started_at, finished_at)
		 VALUES (?, NULL, ?, ?, 0, NULL, ?, ?)`,
		rec.TaskID, string(core.ExecutionSkipped), string(rec.Trigger), at, at)
	if err != nil {
		return fmt.Errorf("记录跳过执行（task_id=%s）: %w", rec.TaskID, err)
	}
	return nil
}

// RecoverStartedExecutions 收尾崩溃遗留的 `started` 行：LEFT JOIN `runs` 取 Run 终态，
// Run 完成 / 达上限 → completed，其余（failed / cancelled / 仍 running / 无 Run）→ failed。
// 无 Run 行同时把 `run_id` 置 NULL（与 FinishExecution 同一条存在性判据，P-04）。
//
// `run_id` 与 `r.status` 都用 `sql.NullString` 接：前者可空、后者在 LEFT JOIN 无匹配时
// 为 NULL。按 `started_at` 正序处理是判据的一部分——同一任务多行遗留时，最后写入的
// `last_status` 必来自最新一行。
// Recovery closes leftover started rows; ordering by started_at ascending is part of the
// contract so the newest row wins last_status.
func (s *TaskStore) RecoverStartedExecutions(ctx context.Context, now time.Time) (int, error) {
	type startedRow struct {
		id       int64
		taskID   string
		runID    sql.NullString
		runState sql.NullString
	}

	var pending []startedRow
	recovered := 0
	err := withWriteLock(ctx, s.db.write, func(ctx context.Context, conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx,
			`SELECT e.id, e.task_id, e.run_id, r.status
			   FROM task_executions e
			   LEFT JOIN runs r ON r.run_id = e.run_id
			  WHERE e.status = ?
			  ORDER BY e.started_at ASC, e.id ASC`, string(core.ExecutionStarted))
		if err != nil {
			return fmt.Errorf("扫描遗留的 started 执行行: %w", err)
		}
		for rows.Next() {
			var row startedRow
			if err := rows.Scan(&row.id, &row.taskID, &row.runID, &row.runState); err != nil {
				_ = rows.Close()
				return fmt.Errorf("读取遗留的 started 执行行: %w", err)
			}
			pending = append(pending, row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("遍历遗留的 started 执行行: %w", err)
		}
		// 必须先关闭游标再写：写与读共用钉住的那一条写连接。
		if err := rows.Close(); err != nil {
			return fmt.Errorf("关闭遗留执行行游标: %w", err)
		}

		for _, row := range pending {
			status := core.ExecutionFailed
			if row.runState.Valid &&
				(row.runState.String == string(core.StatusCompleted) ||
					row.runState.String == string(core.StatusLimitReached)) {
				status = core.ExecutionCompleted
			}
			runID := row.runID
			if !row.runState.Valid {
				runID = sql.NullString{} // 无 Run 行 → 不留悬空引用
			}
			if _, err := conn.ExecContext(ctx,
				`UPDATE task_executions SET status = ?, finished_at = ?, run_id = ?
				  WHERE id = ?`, string(status), now.UnixMilli(), runID, row.id); err != nil {
				return fmt.Errorf("收尾遗留执行行（id=%d）: %w", row.id, err)
			}
			if _, err := conn.ExecContext(ctx,
				`UPDATE scheduled_tasks SET last_status = ? WHERE task_id = ?`,
				string(status), row.taskID); err != nil {
				return fmt.Errorf("同步任务最后状态（task_id=%s）: %w", row.taskID, err)
			}
			recovered++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return recovered, nil
}

// ListExecutions 按 started_at 降序（同刻按 id 降序）返回某任务的执行历史，最多 limit 条。
// ListExecutions returns the newest execution rows first, capped at limit.
func (s *TaskStore) ListExecutions(ctx context.Context, taskID string, limit int) ([]core.ExecutionRecord, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT e.id, e.task_id, e.run_id, e.status, e.trigger, e.duration_ms, e.error_message,
		        d.status, d.channel_uid, d.detail, e.started_at, e.finished_at
		   FROM task_executions e
		   LEFT JOIN schedule_deliveries d ON d.run_id = e.run_id
		  WHERE e.task_id = ?
		  ORDER BY e.started_at DESC, e.id DESC LIMIT ?`, taskID, limit)
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
		)
		if err := rows.Scan(&rec.ID, &rec.TaskID, &runID, &status, &trigger,
			&durationMS, &errMsg, &deliveryStatus, &deliveryChannel, &deliveryDetail,
			&startedAt, &finishedAt); err != nil {
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

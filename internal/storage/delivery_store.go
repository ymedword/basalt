package storage

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
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
	_, err := s.db.write.ExecContext(ctx,
		`INSERT INTO schedule_deliveries
		   (run_id, task_id, agent_name, agent_uid, channel_uid, channel_name,
		    status, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?)
		 ON CONFLICT(run_id) DO NOTHING`,
		rec.RunID, rec.TaskID, rec.Agent, string(rec.AgentUID), string(rec.ChannelUID),
		rec.ChannelName, string(core.DeliveryPending), rec.CreatedAt.UnixMilli())
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
	res, err := s.db.write.ExecContext(ctx,
		`UPDATE schedule_deliveries
		    SET status = ?, http_status = ?, receipt_code = ?, detail = ?,
		        attempted_at = ?, finished_at = ?
		  WHERE run_id = ? AND status = ?`,
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
	if err := s.db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM schedule_deliveries WHERE run_id = ?`, runID).Scan(&exists); err != nil {
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
	row := s.db.read.QueryRowContext(ctx,
		`SELECT run_id, task_id, agent_name, agent_uid, channel_uid, channel_name,
		        status, http_status, receipt_code, detail, attempted_at, finished_at, created_at
		   FROM schedule_deliveries WHERE run_id = ?`, runID)
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
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT run_id, task_id, agent_name, agent_uid, channel_uid, channel_name,
		        status, http_status, receipt_code, detail, attempted_at, finished_at, created_at
		   FROM schedule_deliveries WHERE task_id = ?
		  ORDER BY created_at DESC, run_id DESC LIMIT ?`, taskID, limit)
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

// Recover 处理崩溃遗留的 pending 行：LEFT JOIN `runs` 取实际终态——Run 已完成 /
// 达上限 → unknown（模型已产出正文，但投递是否发生无法证明，**不自动重发**）；
// 其余（失败 / 取消 / 仍 running / 无 Run）→ skipped。
// Recover resolves leftover pending rows from the actual run state without resending.
func (s *DeliveryStore) Recover(ctx context.Context, now time.Time) (int, error) {
	type pendingRow struct {
		runID    string
		runState sql.NullString
	}

	var pending []pendingRow
	recovered := 0
	err := withWriteLock(ctx, s.db.write, func(ctx context.Context, conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx,
			`SELECT d.run_id, r.status
			   FROM schedule_deliveries d
			   LEFT JOIN runs r ON r.run_id = d.run_id
			  WHERE d.status = ?
			  ORDER BY d.created_at ASC`, string(core.DeliveryPending))
		if err != nil {
			return fmt.Errorf("扫描遗留的 pending 投递行: %w", err)
		}
		for rows.Next() {
			var row pendingRow
			if err := rows.Scan(&row.runID, &row.runState); err != nil {
				_ = rows.Close()
				return fmt.Errorf("读取遗留的 pending 投递行: %w", err)
			}
			pending = append(pending, row)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("遍历遗留的 pending 投递行: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("关闭遗留投递行游标: %w", err)
		}

		for _, row := range pending {
			status := core.DeliverySkipped
			detail := "运行未完成，未投递"
			if row.runState.Valid &&
				(row.runState.String == string(core.StatusCompleted) ||
					row.runState.String == string(core.StatusLimitReached)) {
				status = core.DeliveryUnknown
				detail = "进程在投递完成前退出，投递结果未知"
			}
			if _, err := conn.ExecContext(ctx,
				`UPDATE schedule_deliveries SET status = ?, detail = ?, finished_at = ?
				  WHERE run_id = ? AND status = ?`,
				string(status), detail, now.UnixMilli(), row.runID, string(core.DeliveryPending)); err != nil {
				return fmt.Errorf("收尾遗留投递行（run_id=%s）: %w", row.runID, err)
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
	)
	if err := scanner.Scan(&rec.RunID, &rec.TaskID, &rec.Agent, &agentUID, &channelUID,
		&rec.ChannelName, &status, &httpStatus, &receiptCode, &rec.Detail,
		&attemptedAt, &finishedAt, &createdAt); err != nil {
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
	rec.AttemptedAt = unixMilliOrZero(attemptedAt)
	rec.FinishedAt = unixMilliOrZero(finishedAt)
	rec.CreatedAt = time.UnixMilli(createdAt).UTC()
	return rec, nil
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

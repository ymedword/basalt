package storage

// 本文件是通知侧两张表的唯一 SQL 入口（颗粒度文档 R4.8 / T116）：
// `run_notify_refs`（准入冻结的渠道集合）与 `notify_deliveries`（每次发送一条记录）。
//
// 三条纪律：
//   - **通知固定渠道、不固定凭据版本**：Run 准入冻结的是"能发给谁"；每次发送按当时
//     当前 active 版本解析 Key——撤销的凭据立刻停发，而不是继续用准入时的旧版本。
//   - **一次工具调用至多一条投递**：主键是 `tool_invocation_id`，重试同一调用不会
//     写出第二条账（幂等判据在写侧，不靠读侧去重）。
//   - **释放与终态同事务**：`released_at` 由 FinishRun 的 release 路径统一写。
//
// This file is the only SQL entry point for the notify side: channels are frozen at
// admission, credentials are resolved at send time, and a tool invocation has at most
// one delivery row.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// runNotifyColumns 是 run_notify_refs 的读取列序。
// runNotifyColumns is the read order of the notify-ref table.
const runNotifyColumns = `run_id, agent_uid, channel_uid, channel_name, released_at`

// deliveryColumns 是 notify_deliveries 的读取列序。
// deliveryColumns is the read order of the delivery table.
const deliveryColumns = `tool_invocation_id, run_id, agent_uid, channel_uid, channel_name,
	credential_id, credential_version, target_host, resolved_at`

// NotifyDeliveryStore 是通知冻结与投递记录仓储。
// NotifyDeliveryStore covers frozen notify refs and delivery records.
type NotifyDeliveryStore struct{ db *DB }

// NewNotifyDeliveryStore 绑定一个已打开的写模式库句柄。
// NewNotifyDeliveryStore binds the store to an opened writable database handle.
func NewNotifyDeliveryStore(db *DB) *NotifyDeliveryStore { return &NotifyDeliveryStore{db: db} }

// FreezeRef 写入一条渠道冻结（独立调用；StartRun 在自身事务里复用同一 SQL helper）。
// FreezeRef writes one frozen channel; StartRun reuses the same helper in its transaction.
func (s *NotifyDeliveryStore) FreezeRef(ctx context.Context, ref core.RunNotifyRef) error {
	return insertRunNotifyRefTx(ctx, s.db.write, ref)
}

// ReleaseRefs 释放某个 Run 的全部未释放渠道冻结。
// ReleaseRefs stamps released_at on the run's unreleased notify refs.
func (s *NotifyDeliveryStore) ReleaseRefs(ctx context.Context, runID string, at time.Time) (int, error) {
	if at.IsZero() {
		at = time.Now()
	}
	return releaseRunNotifyRefs(ctx, s.db.write, runID, at)
}

// Refs 读某个 Run 冻结的渠道集合，按名称升序。
// Refs reads the frozen channel set of one run.
func (s *NotifyDeliveryStore) Refs(ctx context.Context, runID string) ([]core.RunNotifyRef, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+runNotifyColumns+` FROM run_notify_refs
		  WHERE run_id = ? ORDER BY channel_name ASC, channel_uid ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("列出运行通知冻结（run_id=%s）: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.RunNotifyRef
	for rows.Next() {
		var (
			ref        core.RunNotifyRef
			agentUID   string
			channelUID string
			releasedAt sql.NullInt64
		)
		if scanErr := rows.Scan(&ref.RunID, &agentUID, &channelUID, &ref.ChannelName, &releasedAt); scanErr != nil {
			return nil, fmt.Errorf("扫描运行通知冻结（run_id=%s）: %w", runID, scanErr)
		}
		ref.AgentUID = core.AgentUID(agentUID)
		ref.ChannelUID = core.ResourceUID(channelUID)
		if releasedAt.Valid {
			value := releasedAt.Int64
			ref.ReleasedAt = &value
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历运行通知冻结（run_id=%s）: %w", runID, err)
	}
	return out, nil
}

// UnreleasedRunsByChannel 反查冻结了该渠道且尚未释放的 Run（删除闸门判据）：返回
// Run 标识升序。没有行才等于"没有活动运行还握着它"——**不得**用"当前没有运行"推断。
// UnreleasedRunsByChannel reverse-looks-up the runs that still freeze one channel.
func (s *NotifyDeliveryStore) UnreleasedRunsByChannel(ctx context.Context, uid core.ResourceUID) ([]string, error) {
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT DISTINCT run_id FROM run_notify_refs
		  WHERE channel_uid = ? AND released_at IS NULL ORDER BY run_id ASC`,
		string(canonical))
	if err != nil {
		return nil, fmt.Errorf("反查渠道冻结（channel_uid=%s）: %w", canonical, err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var runID string
		if scanErr := rows.Scan(&runID); scanErr != nil {
			return nil, fmt.Errorf("扫描渠道冻结（channel_uid=%s）: %w", canonical, scanErr)
		}
		out = append(out, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历渠道冻结（channel_uid=%s）: %w", canonical, err)
	}
	return out, nil
}

// Record 写一条投递记录：主键是工具调用 id，已存在即"这次调用已经发过"——
// 返回 `core.ErrConflict` 让调用方按重复投递处理，**不**静默覆盖首条记录。
// Record writes one delivery row; a duplicate invocation is a conflict rather than a
// silent overwrite, because the first row is the audit answer.
func (s *NotifyDeliveryStore) Record(ctx context.Context, rec core.NotifyDeliveryRecord) error {
	if rec.ToolInvocationID <= 0 {
		return errors.New("记录通知投递: tool_invocation_id 必须为正")
	}
	if rec.RunID == "" {
		return errors.New("记录通知投递: run_id 不得为空")
	}
	if _, err := core.ParseAgentUID(string(rec.AgentUID)); err != nil {
		return fmt.Errorf("记录通知投递（run_id=%s）: %w", rec.RunID, err)
	}
	if _, err := core.ParseResourceUID(string(rec.ChannelUID)); err != nil {
		return fmt.Errorf("记录通知投递（run_id=%s）: %w", rec.RunID, err)
	}
	if rec.ChannelName == "" {
		return errors.New("记录通知投递: 渠道名称不得为空")
	}
	if rec.CredentialID != "" {
		if err := core.ValidateCredentialID(rec.CredentialID); err != nil {
			return fmt.Errorf("记录通知投递（run_id=%s）: %w", rec.RunID, err)
		}
		if rec.CredentialVersion < 1 {
			return fmt.Errorf("记录通知投递（run_id=%s）: 托管模式必须带凭据版本", rec.RunID)
		}
	} else if rec.CredentialVersion != 0 {
		return fmt.Errorf("记录通知投递（run_id=%s）: env 模式不得带凭据版本", rec.RunID)
	}
	if rec.ResolvedAt == 0 {
		rec.ResolvedAt = time.Now().UnixMilli()
	}

	if _, err := s.db.write.ExecContext(ctx,
		`INSERT INTO notify_deliveries
		   (tool_invocation_id, run_id, agent_uid, channel_uid, channel_name,
		    credential_id, credential_version, target_host, resolved_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ToolInvocationID, rec.RunID, string(rec.AgentUID), string(rec.ChannelUID),
		rec.ChannelName, nullIfEmpty(rec.CredentialID), nullIfZero(rec.CredentialVersion),
		rec.TargetHost, rec.ResolvedAt); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: 工具调用 %d 已有投递记录", core.ErrConflict, rec.ToolInvocationID)
		}
		return fmt.Errorf("记录通知投递（run_id=%s）: %w", rec.RunID, err)
	}
	return nil
}

// Delivery 按工具调用读投递记录；没有记录 → `ok=false`（"还没发"与"发过但没记"不同，
// 调用方必须能分辨，所以用 ok 而不是 error）。
// Delivery reads the record of one tool invocation.
func (s *NotifyDeliveryStore) Delivery(ctx context.Context, toolInvocationID int64) (core.NotifyDeliveryRecord, bool, error) {
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+deliveryColumns+` FROM notify_deliveries WHERE tool_invocation_id = ?`,
		toolInvocationID)
	rec, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.NotifyDeliveryRecord{}, false, nil
	}
	if err != nil {
		return core.NotifyDeliveryRecord{}, false, fmt.Errorf("读取通知投递（invocation=%d）: %w", toolInvocationID, err)
	}
	return rec, true, nil
}

// Deliveries 读某个 Run 的全部投递记录，按工具调用 id 升序。
// Deliveries reads every delivery record of one run.
func (s *NotifyDeliveryStore) Deliveries(ctx context.Context, runID string) ([]core.NotifyDeliveryRecord, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+deliveryColumns+` FROM notify_deliveries
		  WHERE run_id = ? ORDER BY tool_invocation_id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("列出通知投递（run_id=%s）: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.NotifyDeliveryRecord
	for rows.Next() {
		rec, scanErr := scanDelivery(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("扫描通知投递（run_id=%s）: %w", runID, scanErr)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历通知投递（run_id=%s）: %w", runID, err)
	}
	return out, nil
}

// insertRunNotifyRefTx 是渠道冻结行的唯一 SQL（独立冻结与准入事务共用）。
// insertRunNotifyRefTx is the single notify-ref INSERT.
func insertRunNotifyRefTx(ctx context.Context, ex sqlExecutor, ref core.RunNotifyRef) error {
	if ref.RunID == "" {
		return errors.New("冻结运行通知: run_id 不得为空")
	}
	if _, err := core.ParseAgentUID(string(ref.AgentUID)); err != nil {
		return fmt.Errorf("冻结运行通知（run_id=%s）: %w", ref.RunID, err)
	}
	if _, err := core.ParseResourceUID(string(ref.ChannelUID)); err != nil {
		return fmt.Errorf("冻结运行通知（run_id=%s）: %w", ref.RunID, err)
	}
	if ref.ChannelName == "" {
		return fmt.Errorf("冻结运行通知（run_id=%s）: 渠道名称不得为空", ref.RunID)
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO run_notify_refs (run_id, agent_uid, channel_uid, channel_name, released_at)
		 VALUES (?, ?, ?, ?, NULL)`,
		ref.RunID, string(ref.AgentUID), string(ref.ChannelUID), ref.ChannelName); err != nil {
		return fmt.Errorf("写入运行通知冻结（run_id=%s）: %w", ref.RunID, err)
	}
	return nil
}

// releaseRunNotifyRefs 释放某个 Run 的全部渠道冻结；事务边界由调用方保证。
// releaseRunNotifyRefs releases the run's notify refs; the caller owns the transaction.
func releaseRunNotifyRefs(ctx context.Context, ex sqlExecutor, runID string, at time.Time) (int, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE run_notify_refs SET released_at = ?
		  WHERE run_id = ? AND released_at IS NULL`, at.UnixMilli(), runID)
	if err != nil {
		return 0, fmt.Errorf("释放运行通知冻结（run_id=%s）: %w", runID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("确认运行通知冻结释放（run_id=%s）: %w", runID, err)
	}
	return int(affected), nil
}

// scanDelivery 按 deliveryColumns 的列序扫描一行；空凭据列映射为空值。
// scanDelivery scans one delivery row, mapping NULL credential columns to zero values.
func scanDelivery(row interface{ Scan(...any) error }) (core.NotifyDeliveryRecord, error) {
	var (
		rec        core.NotifyDeliveryRecord
		agentUID   string
		channelUID string
		credID     sql.NullString
		credVer    sql.NullInt64
	)
	err := row.Scan(&rec.ToolInvocationID, &rec.RunID, &agentUID, &channelUID,
		&rec.ChannelName, &credID, &credVer, &rec.TargetHost, &rec.ResolvedAt)
	if err != nil {
		return core.NotifyDeliveryRecord{}, err
	}
	rec.AgentUID = core.AgentUID(agentUID)
	rec.ChannelUID = core.ResourceUID(channelUID)
	rec.CredentialID = credID.String
	if credVer.Valid {
		rec.CredentialVersion = int(credVer.Int64)
	}
	return rec, nil
}

// nullIfZero 把 0 映射为 NULL（`credential_version` 在 env 模式下为空）。
// nullIfZero maps zero to NULL for the nullable credential version column.
func nullIfZero(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

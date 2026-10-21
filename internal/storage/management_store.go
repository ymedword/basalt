package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 management_operations 的**唯一 SQL 入口**（颗粒度文档 §3.1.16）：
// 管理面高权限变更先写 pending、返回后回填终态；启动恢复按查证结论回填并记录
// `recovered_at` / `verification`。管理审计与运行审计（runs / llm_calls /
// tool_invocations）**互不混写**——本文件不碰那三张表，也不写 `run_id`（约束 44）。
// This file is the only SQL entry point for management_operations: pre-write pending,
// then finish; startup recovery records recovered_at and verification. It never touches
// the run ledger tables and never writes run_id (constraint 44).

// operationColumns 是 `management_operations` 的读取列序；scanOperation 按同一顺序扫描。
// 列序只在此处与 scanOperation 成对出现，不另立第二份列清单。
// operationColumns is the single column order shared by every read and scanOperation.
const operationColumns = `operation_id, action, target_kind, target_id, actor_subject,
	auth_method, source, request_hash, requested_version_id, expected_etag, status,
	result_code, error_message, detail_json, created_at, finished_at, recovered_at,
	verification`

// 列表窗口（颗粒度文档 §3.1.16）：缺省 50、上限 200；调用方只给过滤值，排序固定。
// The fixed list window: default 50, hard cap 200.
const (
	defaultOperationLimit = 50
	maxOperationLimit     = 200
)

// ManagementStore 是管理操作仓储（core.OperationStore 的实现）。
// ManagementStore is the management operation repository.
type ManagementStore struct{ db *DB }

// 编译期断言：满足消费方（core）声明的端口。
// Compile-time assertion that the store satisfies the consumer-declared port.
var _ core.OperationStore = (*ManagementStore)(nil)

// NewManagementStore 绑定一个已打开的写模式库句柄。
// NewManagementStore binds the store to an opened writable database handle.
func NewManagementStore(db *DB) *ManagementStore { return &ManagementStore{db: db} }

// BeginOperation 只插入 `status = 'pending'` 的预写行；插入失败 = 调用方**拒绝执行**变更。
// 状态由本函数固定写 pending，调用方传入终态会被拒绝（不存在"预写即成功"的捷径）；
// `detail_json` 为空时存 `{}` 而不是 NULL；可空列（request_hash / requested_version_id /
// expected_etag）的零值写 NULL，与"写了空串"区分开；created_at 存 Unix 毫秒。
// BeginOperation inserts the pending row only; a failure refuses the change. The status
// is a SQL literal here, empty detail becomes {}, and zero-valued nullable columns map
// to NULL.
func (s *ManagementStore) BeginOperation(ctx context.Context, rec core.OperationRecord) error {
	if rec.OperationID == "" {
		return errors.New("预写管理操作: operation_id 不能为空")
	}
	if rec.Status != "" && rec.Status != core.OperationPending {
		return fmt.Errorf("预写管理操作 %s: 只接受 pending 预写，收到 %q", rec.OperationID, rec.Status)
	}
	detail, err := normalizeDetailJSON(rec.DetailJSON)
	if err != nil {
		return fmt.Errorf("预写管理操作 %s: %w", rec.OperationID, err)
	}

	createdAt := rec.CreatedAt
	if createdAt.IsZero() {
		// created_at 是 NOT NULL 且含义为"这一时刻"；零值时间比缺省当前时刻更误导。
		// A zero time would predate 1677 and mislead more than now().
		createdAt = time.Now()
	}

	_, err = s.db.write.ExecContext(ctx,
		`INSERT INTO management_operations (
			operation_id, action, target_kind, target_id, actor_subject, auth_method,
			source, request_hash, requested_version_id, expected_etag, status,
			result_code, error_message, detail_json, created_at, finished_at,
			recovered_at, verification
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', NULL, NULL, ?, ?, NULL, NULL, NULL)`,
		rec.OperationID, rec.Action, rec.TargetKind, rec.TargetID, rec.ActorSubject,
		rec.AuthMethod, rec.Source, nullIfEmpty(rec.RequestHash),
		nullIfZeroInt64(rec.RequestedVersionID), nullIfEmpty(rec.ExpectedETag),
		detail, createdAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("预写管理操作 %s: %w", rec.OperationID, err)
	}
	return nil
}

// FinishOperation 回填终态。`UPDATE` 带 `WHERE operation_id = ? AND status = 'pending'`：
// 影响 0 行时读取当前行——已终态则幂等返回、**不覆盖**；不存在则返回
// core.ErrOperationNotFound。失败原文按 4 KB 截断（契约 §7.4）。
// FinishOperation fills the terminal state under a pending-only guard; terminal rows
// are left untouched and a missing row maps to core.ErrOperationNotFound.
func (s *ManagementStore) FinishOperation(ctx context.Context, operationID string, status core.OperationStatus, resultCode, errorMessage string, detailJSON []byte, now time.Time) error {
	if !isTerminalOperationStatus(status) {
		return fmt.Errorf("回填管理操作 %s: 终态必须是 success / failed / unknown，收到 %q", operationID, status)
	}
	detail, err := normalizeDetailJSON(detailJSON)
	if err != nil {
		return fmt.Errorf("回填管理操作 %s: %w", operationID, err)
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("回填管理操作 %s: 开启事务: %w", operationID, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := finishOperationTx(ctx, tx, operationID, status, resultCode, errorMessage, detail, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("回填管理操作 %s: 提交: %w", operationID, err)
	}
	return nil
}

// finishOperationTx 是终态写入的唯一 SQL 实现（包内可见，US-12 的发布 / 归档事务复用）：
// 只更新仍为 pending 的行；0 行时读当前状态区分"已终态（幂等）"与"不存在"。
// finishOperationTx is the single terminal-write implementation, reused by US-12's
// publish/archive transactions; it updates pending rows only.
func finishOperationTx(ctx context.Context, tx *sql.Tx, operationID string, status core.OperationStatus, resultCode, errorMessage string, detailJSON []byte, now time.Time) error {
	if !isTerminalOperationStatus(status) {
		return fmt.Errorf("回填管理操作 %s: 终态必须是 success / failed / unknown，收到 %q", operationID, status)
	}
	if now.IsZero() {
		now = time.Now()
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE management_operations
		    SET status = ?, result_code = ?, error_message = ?,
		        detail_json = COALESCE(?, detail_json), finished_at = ?
		  WHERE operation_id = ? AND status = 'pending'`,
		string(status), nullIfEmpty(resultCode),
		nullIfEmpty(truncateUTF8(errorMessage, maxErrorMessageBytes)), detailJSON,
		now.UnixMilli(), operationID)
	if err != nil {
		return fmt.Errorf("回填管理操作 %s: %w", operationID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("回填管理操作 %s: 读取影响行数: %w", operationID, err)
	}
	if affected > 0 {
		return nil
	}

	// 0 行有两种含义：并发下已被回填（幂等），或标识不存在（not-found）。
	// Two meanings for zero rows: already terminal (idempotent) or missing.
	var current core.OperationStatus
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM management_operations WHERE operation_id = ?`, operationID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", core.ErrOperationNotFound, operationID)
	}
	if err != nil {
		return fmt.Errorf("回填管理操作 %s: 读取当前状态: %w", operationID, err)
	}
	return nil
}

// RecoverOperation 只由启动恢复调用：把仍为 pending 的行按查证结论写终态，并记录
// `recovered_at` 与 `verification`。三组结论与终态**一一对应**（契约 §9.4）：
// applied → success、not_applied → failed、indeterminate → unknown——错配直接拒绝，
// 避免恢复路径写出自相矛盾的行。已终态的行幂等返回，**不得**覆盖。
// RecoverOperation writes the terminal state plus recovered_at/verification for rows
// still pending; the three verification values map one-to-one onto terminal states.
func (s *ManagementStore) RecoverOperation(ctx context.Context, operationID string, status core.OperationStatus, verification string, detailJSON []byte, now time.Time) error {
	if err := checkRecoveryPair(status, verification); err != nil {
		return fmt.Errorf("恢复管理操作 %s: %w", operationID, err)
	}
	detail, err := normalizeDetailJSON(detailJSON)
	if err != nil {
		return fmt.Errorf("恢复管理操作 %s: %w", operationID, err)
	}
	if now.IsZero() {
		now = time.Now()
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("恢复管理操作 %s: 开启事务: %w", operationID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE management_operations
		    SET status = ?, detail_json = COALESCE(?, detail_json), finished_at = ?,
		        recovered_at = ?, verification = ?
		  WHERE operation_id = ? AND status = 'pending'`,
		string(status), detail, now.UnixMilli(), now.UnixMilli(), verification, operationID)
	if err != nil {
		return fmt.Errorf("恢复管理操作 %s: %w", operationID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("恢复管理操作 %s: 读取影响行数: %w", operationID, err)
	}
	if affected == 0 {
		var current core.OperationStatus
		err = tx.QueryRowContext(ctx,
			`SELECT status FROM management_operations WHERE operation_id = ?`, operationID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", core.ErrOperationNotFound, operationID)
		}
		if err != nil {
			return fmt.Errorf("恢复管理操作 %s: 读取当前状态: %w", operationID, err)
		}
		// 已终态：恢复不覆盖（可能已被正常路径回填，或上一轮恢复已处理）。
		// Already terminal: recovery never overwrites an existing outcome.
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("恢复管理操作 %s: 提交: %w", operationID, err)
	}
	return nil
}

// GetOperation 按标识取一行；不存在返回 core.ErrOperationNotFound。
// GetOperation fetches one operation; a miss maps to core.ErrOperationNotFound.
func (s *ManagementStore) GetOperation(ctx context.Context, operationID string) (core.OperationRecord, error) {
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+operationColumns+` FROM management_operations WHERE operation_id = ?`, operationID)
	rec, err := scanOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.OperationRecord{}, fmt.Errorf("%w: %s", core.ErrOperationNotFound, operationID)
	}
	if err != nil {
		return core.OperationRecord{}, fmt.Errorf("读取管理操作 %s: %w", operationID, err)
	}
	return rec, nil
}

// 四个可选过滤维各占一位，十六种组合都以**编译期常量**给出：运行期不拼 SQL，过滤值
// 只能走占位符；排序与窗口是常量，不开放给调用方。
// Each optional filter owns one bit; all sixteen combinations are compile-time constants,
// so no SQL is assembled at runtime and every filter value binds as a placeholder.
const (
	operationsListSelect = `SELECT ` + operationColumns + ` FROM management_operations`
	operationsListTail   = ` ORDER BY created_at DESC, operation_id DESC LIMIT ?`
)

var operationsListQueries = [16]string{
	operationsListSelect + operationsListTail,
	operationsListSelect + ` WHERE action = ?` + operationsListTail,
	operationsListSelect + ` WHERE target_kind = ?` + operationsListTail,
	operationsListSelect + ` WHERE action = ? AND target_kind = ?` + operationsListTail,
	operationsListSelect + ` WHERE target_id = ?` + operationsListTail,
	operationsListSelect + ` WHERE action = ? AND target_id = ?` + operationsListTail,
	operationsListSelect + ` WHERE target_kind = ? AND target_id = ?` + operationsListTail,
	operationsListSelect + ` WHERE action = ? AND target_kind = ? AND target_id = ?` + operationsListTail,
	operationsListSelect + ` WHERE status = ?` + operationsListTail,
	operationsListSelect + ` WHERE action = ? AND status = ?` + operationsListTail,
	operationsListSelect + ` WHERE target_kind = ? AND status = ?` + operationsListTail,
	operationsListSelect + ` WHERE action = ? AND target_kind = ? AND status = ?` + operationsListTail,
	operationsListSelect + ` WHERE target_id = ? AND status = ?` + operationsListTail,
	operationsListSelect + ` WHERE action = ? AND target_id = ? AND status = ?` + operationsListTail,
	operationsListSelect + ` WHERE target_kind = ? AND target_id = ? AND status = ?` + operationsListTail,
	operationsListSelect + ` WHERE action = ? AND target_kind = ? AND target_id = ? AND status = ?` + operationsListTail,
}

// ListOperations 按过滤条件取固定窗口：`ORDER BY created_at DESC, operation_id DESC`，
// limit 缺省 50、上限 200。过滤值只作为参数传入，排序不开放给调用方。
// ListOperations returns a fixed window ordered by created_at DESC, operation_id DESC.
func (s *ManagementStore) ListOperations(ctx context.Context, filter core.OperationFilter) ([]core.OperationRecord, error) {
	// 位序与 operationsListQueries 的谓词顺序一致：action / target_kind / target_id / status。
	mask := 0
	args := make([]any, 0, 5)
	if filter.Action != "" {
		mask |= 1
		args = append(args, filter.Action)
	}
	if filter.TargetKind != "" {
		mask |= 2
		args = append(args, filter.TargetKind)
	}
	if filter.TargetID != "" {
		mask |= 4
		args = append(args, filter.TargetID)
	}
	if filter.Status != "" {
		mask |= 8
		args = append(args, filter.Status)
	}
	query := operationsListQueries[mask]
	args = append(args, normalizeOperationLimit(filter.Limit))

	rows, err := s.db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("列出管理操作: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []core.OperationRecord
	for rows.Next() {
		rec, err := scanOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描管理操作列表: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历管理操作列表: %w", err)
	}
	return records, nil
}

// PendingOperations 返回全部 pending 行，按 created_at / operation_id 升序——启动恢复
// 最先看到最早的操作，顺序确定、可复核。只查证、不重做（约束 38）。
// PendingOperations returns pending rows in deterministic ascending order for recovery.
func (s *ManagementStore) PendingOperations(ctx context.Context) ([]core.OperationRecord, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+operationColumns+` FROM management_operations
		  WHERE status = 'pending' ORDER BY created_at ASC, operation_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("列出待恢复管理操作: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []core.OperationRecord
	for rows.Next() {
		rec, err := scanOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描待恢复管理操作: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历待恢复管理操作: %w", err)
	}
	return records, nil
}

// scanOperation 按 operationColumns 的列序扫描一行；可空列一律经 sql.Null* / []byte
// 往返，零值哨兵不参与 NULL 判定（§3.1.17 的 NULL 映射口径同源）。
// scanOperation scans one row in operationColumns order; nullable columns go through
// sql.Null* so zero values are never mistaken for NULL.
func scanOperation(row interface{ Scan(...any) error }) (core.OperationRecord, error) {
	var (
		rec                core.OperationRecord
		requestHash        sql.NullString
		requestedVersionID sql.NullInt64
		expectedETag       sql.NullString
		resultCode         sql.NullString
		errorMessage       sql.NullString
		detailJSON         []byte
		createdAt          int64
		finishedAt         sql.NullInt64
		recoveredAt        sql.NullInt64
		verification       sql.NullString
		status             core.OperationStatus
	)
	err := row.Scan(
		&rec.OperationID, &rec.Action, &rec.TargetKind, &rec.TargetID,
		&rec.ActorSubject, &rec.AuthMethod, &rec.Source, &requestHash,
		&requestedVersionID, &expectedETag, &status, &resultCode, &errorMessage,
		&detailJSON, &createdAt, &finishedAt, &recoveredAt, &verification,
	)
	if err != nil {
		return core.OperationRecord{}, err
	}

	rec.RequestHash = requestHash.String
	if requestedVersionID.Valid {
		rec.RequestedVersionID = requestedVersionID.Int64
	}
	rec.ExpectedETag = expectedETag.String
	rec.Status = status
	rec.ResultCode = resultCode.String
	rec.ErrorMessage = errorMessage.String
	rec.DetailJSON = detailJSON
	rec.CreatedAt = time.UnixMilli(createdAt).UTC()
	if finishedAt.Valid {
		rec.FinishedAt = time.UnixMilli(finishedAt.Int64).UTC()
	}
	if recoveredAt.Valid {
		rec.RecoveredAt = time.UnixMilli(recoveredAt.Int64).UTC()
	}
	rec.Verification = verification.String
	return rec, nil
}

// normalizeDetailJSON 统一 detail_json 的持久形态：空输入存 `{}`（不存 NULL，schema
// 注释与 §3.1.16 同口径）；非空输入必须是合法 JSON，否则拒绝写入——视图层按 JSON
// 投影，坏 JSON 不该等到读取时才炸。
// normalizeDetailJSON stores {} for empty input and rejects malformed JSON early.
func normalizeDetailJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("{}"), nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("detail_json 不是合法 JSON")
	}
	return raw, nil
}

// isTerminalOperationStatus 判定终态三值（pending 不是终态）。
// isTerminalOperationStatus reports whether s is one of the three terminal values.
func isTerminalOperationStatus(s core.OperationStatus) bool {
	return s == core.OperationSuccess || s == core.OperationFailed || s == core.OperationUnknown
}

// checkRecoveryPair 校验查证结论与终态的一一对应（契约 §9.4）。
// checkRecoveryPair enforces the one-to-one mapping of verification to terminal state.
func checkRecoveryPair(status core.OperationStatus, verification string) error {
	switch {
	case status == core.OperationSuccess && verification == core.VerificationApplied:
		return nil
	case status == core.OperationFailed && verification == core.VerificationNotApplied:
		return nil
	case status == core.OperationUnknown && verification == core.VerificationIndeterminate:
		return nil
	default:
		return fmt.Errorf("查证结论 %q 与终态 %q 不匹配", verification, status)
	}
}

// normalizeOperationLimit 把 limit 收进 [1, 200]，缺省 50。
// normalizeOperationLimit clamps the limit to [1, 200], defaulting to 50.
func normalizeOperationLimit(limit int) int {
	if limit <= 0 {
		return defaultOperationLimit
	}
	if limit > maxOperationLimit {
		return maxOperationLimit
	}
	return limit
}

// nullIfZeroInt64 把 0 映射为 SQL NULL——`requested_version_id` 的"无目标版本"与
// "版本 0"是两回事（版本从 1 起）。
// nullIfZeroInt64 maps zero to SQL NULL for nullable integer columns.
func nullIfZeroInt64(v int64) any {
	if v == 0 {
		return sql.NullInt64{}
	}
	return v
}

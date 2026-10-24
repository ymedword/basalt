package storage

// 本文件是 `agent_archives` 归档记录的唯一 SQL 入口（颗粒度文档 §3.1.6 / 约束 31、36）：
// 归档行与操作终态**同事务**写入，终态的 SQL 实现只有 `finishOperationTx` 一处——
// 本文件不复制状态机、不另写 UPDATE。
//
// **不提供** `UpsertLifecycle`：`agent_lifecycle` 索引由 US-11 的
// `ResourceStore.PutLifecycleIndex` 写，状态文件由 `workspace.LifecycleStore` 写
// （约束 49）；归档记录存储只管自己那一张表。
// This file is the only SQL entry point for agent_archives. The archive row and the
// operation terminal state share one transaction; the lifecycle index is US-11's.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// archiveColumns 是 agent_archives 的读取列序（scanArchive 按同一顺序扫描）。
const archiveColumns = `archive_id, agent_name, agent_uid, archived_at, content_hash,
	manifest_json, operation_id, restored_at, restored_agent_name`

// AgentAdminStore 是 Agent 归档记录仓储。
// AgentAdminStore is the repository of agent archive rows.
type AgentAdminStore struct{ db *DB }

// NewAgentAdminStore 绑定一个已打开的写模式库句柄。
// NewAgentAdminStore binds the store to an opened writable database handle.
func NewAgentAdminStore(db *DB) *AgentAdminStore { return &AgentAdminStore{db: db} }

// InsertArchive 插入一行归档记录，并在**同一事务**内把归档操作置 success
// （约束 36：归档记录与操作终态同事务，任一步失败整笔回滚）。
// 终态写入只调 `finishOperationTx`——操作行不存在或已终态时它会返回错误，归档行随之
// 回滚（fail-closed：不能留下"有归档记录、操作却查不出结果"的组合）。
// 已恢复的行不允许从这里插入（`RestoredAt` 必须为 nil）：恢复事实只能由 MarkRestored 写。
// InsertArchive writes the archive row and finishes the operation in one transaction.
func (s *AgentAdminStore) InsertArchive(ctx context.Context, rec core.AgentArchiveRecord, opID string) error {
	if err := validateArchiveRecord(rec, opID); err != nil {
		return err
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("写入归档记录 %s: 开启事务: %w", rec.ArchiveID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// uid_source 由身份是否可证明派生（契约 CHECK：`archive` ⇔ agent_uid 非空）：
	// 让调用方各自拼这两列，迟早有一处对不上。
	_, err = tx.ExecContext(ctx,
		`INSERT INTO agent_archives
		   (archive_id, agent_name, agent_uid, uid_source, archived_at, content_hash, manifest_json, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ArchiveID, rec.AgentName, nullIfEmpty(string(rec.AgentUID)),
		archiveUIDSource(rec.AgentUID), rec.ArchivedAt, rec.ContentHash, rec.ManifestJSON, opID)
	if err != nil {
		return fmt.Errorf("写入归档记录 %s: %w", rec.ArchiveID, err)
	}

	// 终态时刻取归档时刻（同一行事实的两种投影，不另取当前时间）。
	if err := finishOperationTx(ctx, tx, opID, core.OperationSuccess, "", "", nil, time.UnixMilli(rec.ArchivedAt)); err != nil {
		return fmt.Errorf("写入归档记录 %s: %w", rec.ArchiveID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("写入归档记录 %s: 提交: %w", rec.ArchiveID, err)
	}
	return nil
}

// MarkRestored 标记一行归档已恢复（写入恢复时刻与目标 Agent 名），并在**同一事务**内
// 把恢复操作置 success。只更新 `restored_at IS NULL` 的行：同一归档不得被恢复两次
// （已恢复 → `core.ErrConflict`；不存在 → `core.ErrResourceNotFound`）。
// 零影响行时**不**写终态就返回——操作行保持 pending，由恢复路径查证后处置。
// MarkRestored marks one archive restored and finishes the operation in one transaction;
// a second restore of the same archive is a conflict, and a missing one is not found.
func (s *AgentAdminStore) MarkRestored(ctx context.Context, archiveID, newName, opID string, at time.Time) error {
	switch {
	case archiveID == "":
		return errors.New("归档标识不得为空")
	case newName == "":
		return fmt.Errorf("恢复目标 Agent 名不得为空（归档 %s）", archiveID)
	case opID == "":
		return fmt.Errorf("恢复操作标识不得为空（归档 %s）", archiveID)
	}
	if at.IsZero() {
		at = time.Now()
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("标记归档 %s 已恢复: 开启事务: %w", archiveID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE agent_archives
		    SET restored_at = ?, restored_agent_name = ?
		  WHERE archive_id = ? AND restored_at IS NULL`,
		at.UnixMilli(), newName, archiveID)
	if err != nil {
		return fmt.Errorf("标记归档 %s 已恢复: %w", archiveID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("标记归档 %s 已恢复: 读取影响行数: %w", archiveID, err)
	}
	if affected == 0 {
		return restoreMissTx(ctx, tx, archiveID)
	}

	if err := finishOperationTx(ctx, tx, opID, core.OperationSuccess, "", "", nil, at); err != nil {
		return fmt.Errorf("标记归档 %s 已恢复: %w", archiveID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("标记归档 %s 已恢复: 提交: %w", archiveID, err)
	}
	return nil
}

// restoreMissTx 区分"归档不存在"与"已经恢复过"：前者 404 语义，后者是状态冲突。
// restoreMissTx distinguishes a missing archive from an already-restored one.
func restoreMissTx(ctx context.Context, tx *sql.Tx, archiveID string) error {
	var restoredAt sql.NullInt64
	err := tx.QueryRowContext(ctx,
		`SELECT restored_at FROM agent_archives WHERE archive_id = ?`, archiveID).Scan(&restoredAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: 归档 %s", core.ErrResourceNotFound, archiveID)
	case err != nil:
		return fmt.Errorf("标记归档 %s 已恢复: 读取当前状态: %w", archiveID, err)
	case restoredAt.Valid:
		return fmt.Errorf("%w: 归档 %s 已恢复（%d）", core.ErrConflict, archiveID, restoredAt.Int64)
	}
	// 行还在但未恢复：UPDATE 的 WHERE 条件与本次读到的状态矛盾，属于并发写者的窗口。
	return fmt.Errorf("%w: 归档 %s 的恢复标记未生效", core.ErrConflict, archiveID)
}

// ArchiveByID 按归档标识读一行；未命中返回 `core.ErrResourceNotFound`（只读面映射 404）。
// ArchiveByID reads one archive row; a miss wraps ErrResourceNotFound.
func (s *AgentAdminStore) ArchiveByID(ctx context.Context, archiveID string) (core.AgentArchiveRecord, error) {
	if archiveID == "" {
		return core.AgentArchiveRecord{}, errors.New("归档标识不得为空")
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+archiveColumns+` FROM agent_archives WHERE archive_id = ?`, archiveID)
	rec, err := scanArchive(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.AgentArchiveRecord{}, fmt.Errorf("%w: 归档 %s", core.ErrResourceNotFound, archiveID)
	}
	if err != nil {
		return core.AgentArchiveRecord{}, fmt.Errorf("读取归档记录 %s: %w", archiveID, err)
	}
	return rec, nil
}

// ListArchives 列出全部归档记录（新的在前；同一毫秒按归档标识倒序——顺序确定可比）。
// 恢复视图与引用扫描共用本清单，不各写一套查询。
// ListArchives lists every archive, newest first, with a deterministic tie-break.
func (s *AgentAdminStore) ListArchives(ctx context.Context) ([]core.AgentArchiveRecord, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+archiveColumns+` FROM agent_archives
		  ORDER BY archived_at DESC, archive_id DESC`)
	if err != nil {
		return nil, fmt.Errorf("列出归档记录: %w", err)
	}
	defer func() { _ = rows.Close() }()

	records := make([]core.AgentArchiveRecord, 0, 8)
	for rows.Next() {
		rec, scanErr := scanArchive(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("列出归档记录: %w", scanErr)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出归档记录: %w", err)
	}
	return records, nil
}

// scanArchive 按 archiveColumns 的列序扫描一行归档记录（nil 恢复时刻 → 零值）。
// scanArchive scans one row in archiveColumns order; a NULL restored_at becomes nil.
func scanArchive(row interface{ Scan(...any) error }) (core.AgentArchiveRecord, error) {
	var (
		rec          core.AgentArchiveRecord
		restoredAt   sql.NullInt64
		restoredName sql.NullString
	)
	var agentUID sql.NullString
	err := row.Scan(&rec.ArchiveID, &rec.AgentName, &agentUID, &rec.ArchivedAt, &rec.ContentHash,
		&rec.ManifestJSON, &rec.OperationID, &restoredAt, &restoredName)
	if err != nil {
		return core.AgentArchiveRecord{}, err
	}
	if agentUID.Valid {
		rec.AgentUID = core.AgentUID(agentUID.String)
	}
	if restoredAt.Valid {
		at := restoredAt.Int64
		rec.RestoredAt = &at
	}
	if restoredName.Valid {
		rec.RestoredAgentName = restoredName.String
	}
	return rec, nil
}

// archiveUIDSource 把"身份是否可证明"映射为 `uid_source` 的固定字面量。
// archiveUIDSource maps provable identity onto the fixed uid_source literals.
func archiveUIDSource(uid core.AgentUID) string {
	if uid == "" {
		return "legacy_unrecorded"
	}
	return "archive"
}

// validateArchiveRecord 是插入前的形状判据：标识 / 名字 / 哈希 / 清单 / 操作行缺一不可，
// 归档时刻必须是正的 Unix 毫秒；`RestoredAt` 非 nil 说明调用方把恢复事实混进了归档插入
// （恢复只能走 MarkRestored）。
// validateArchiveRecord checks the insert shape before any SQL runs.
func validateArchiveRecord(rec core.AgentArchiveRecord, opID string) error {
	switch {
	case rec.ArchiveID == "":
		return errors.New("归档标识不得为空")
	case rec.AgentName == "":
		return fmt.Errorf("归档 %s: Agent 名不得为空", rec.ArchiveID)
	case rec.ArchivedAt <= 0:
		return fmt.Errorf("归档 %s: 归档时刻必须是正的 Unix 毫秒，收到 %d", rec.ArchiveID, rec.ArchivedAt)
	case rec.ContentHash == "":
		return fmt.Errorf("归档 %s: 内容哈希不得为空", rec.ArchiveID)
	case rec.ManifestJSON == "":
		return fmt.Errorf("归档 %s: 目录清单不得为空", rec.ArchiveID)
	case opID == "":
		return fmt.Errorf("归档 %s: 操作标识不得为空", rec.ArchiveID)
	case rec.RestoredAt != nil:
		return fmt.Errorf("归档 %s: 归档插入不得携带恢复事实（恢复只走 MarkRestored）", rec.ArchiveID)
	}
	return nil
}

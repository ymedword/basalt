package storage

// 本文件是 `run_credential_refs` 的唯一 SQL 入口（颗粒度文档 R4.8 / T116）：
// 准入时冻结"这次 Run 用哪一版 Key"，终态时释放。
//
// 三条纪律：
//   - **冻结的是版本号**，不是凭据身份：换 Key 产生新版本，在途 Run 仍用旧版本
//     （直到旧版本被撤销）；不会因为一次发布就把在途 Key 换掉。
//   - **检查不落库**：Run 内每次模型调用前由 app 层读版本状态与身份状态，
//     本文件只提供数据面。
//   - **释放与终态同事务**：由 FinishRun 的 release 路径统一写 `released_at`，
//     行保留作历史——"这次 Run 当时用的是哪版 Key"必须事后可查。
//
// This file is the only SQL entry point for run credential pins: freeze at admission,
// release with the terminal state, rows kept as history.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// runCredentialColumns 是 run_credential_refs 的读取列序。
// runCredentialColumns is the read order of the pin table.
// #nosec G101 -- SQL 列序常量，值不是凭据（变量名命中）。
const runCredentialColumns = `run_id, credential_id, credential_version, resource_kind,
	resource_uid, resource_name, released_at`

// RunCredentialStore 是 Run 凭据固定仓储。
// RunCredentialStore is the repository of run credential pins.
type RunCredentialStore struct{ db *DB }

// NewRunCredentialStore 绑定一个已打开的写模式库句柄。
// NewRunCredentialStore binds the store to an opened writable database handle.
func NewRunCredentialStore(db *DB) *RunCredentialStore { return &RunCredentialStore{db: db} }

// Freeze 写入一条固定（独立调用；StartRun 在自身事务里复用同一 SQL helper）。
// `run_credential_refs` 只记模型连接：通知按发送时解析当前 active 版本，不固定。
// Freeze writes one pin; StartRun reuses the same SQL helper inside its transaction.
func (s *RunCredentialStore) Freeze(ctx context.Context, ref core.RunCredentialRef) error {
	if err := insertRunCredentialRefTx(ctx, s.db.write, ref); err != nil {
		return err
	}
	return nil
}

// Release 释放某个 Run 的全部未释放固定（独立调用；终态路径在 FinishRun 事务内做同一件事）。
// Release stamps released_at on the run's unreleased pins.
func (s *RunCredentialStore) Release(ctx context.Context, runID string, at time.Time) (int, error) {
	if at.IsZero() {
		at = time.Now()
	}
	return releaseRunCredentialRefs(ctx, s.db.write, runID, at)
}

// Pins 读某个 Run 的全部固定，按 `(credential_id, version)` 升序——Run 内检查按此遍历。
// Pins reads every pin of one run in a stable order.
func (s *RunCredentialStore) Pins(ctx context.Context, runID string) ([]core.RunCredentialRef, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+runCredentialColumns+` FROM run_credential_refs
		  WHERE run_id = ? ORDER BY credential_id ASC, credential_version ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("列出运行凭据固定（run_id=%s）: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.RunCredentialRef
	for rows.Next() {
		ref, scanErr := scanRunCredentialRef(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("扫描运行凭据固定（run_id=%s）: %w", runID, scanErr)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历运行凭据固定（run_id=%s）: %w", runID, err)
	}
	return out, nil
}

// insertRunCredentialRefTx 是固定行的唯一 SQL（独立冻结与准入事务共用）。
// insertRunCredentialRefTx is the single pin INSERT.
func insertRunCredentialRefTx(ctx context.Context, ex sqlExecutor, ref core.RunCredentialRef) error {
	if ref.RunID == "" {
		return errors.New("固定运行凭据: run_id 不得为空")
	}
	if err := core.ValidateCredentialID(ref.CredentialID); err != nil {
		return fmt.Errorf("固定运行凭据（run_id=%s）: %w", ref.RunID, err)
	}
	if ref.CredentialVersion < 1 {
		return fmt.Errorf("固定运行凭据（run_id=%s）: 凭据版本必须 >= 1", ref.RunID)
	}
	if ref.ResourceKind != core.BindingModelConnection {
		return fmt.Errorf("固定运行凭据（run_id=%s）: 只固定模型连接，实际 %q", ref.RunID, ref.ResourceKind)
	}
	if _, err := core.ParseResourceUID(string(ref.ResourceUID)); err != nil {
		return fmt.Errorf("固定运行凭据（run_id=%s）: %w", ref.RunID, err)
	}
	if ref.ResourceName == "" {
		return fmt.Errorf("固定运行凭据（run_id=%s）: 资源名称不得为空", ref.RunID)
	}
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO run_credential_refs
		   (run_id, credential_id, credential_version, resource_kind, resource_uid,
		    resource_name, released_at)
		 VALUES (?, ?, ?, ?, ?, ?, NULL)`,
		ref.RunID, ref.CredentialID, ref.CredentialVersion, string(ref.ResourceKind),
		string(ref.ResourceUID), ref.ResourceName); err != nil {
		return fmt.Errorf("写入运行凭据固定（run_id=%s）: %w", ref.RunID, err)
	}
	return nil
}

// releaseRunCredentialRefs 释放某个 Run 的全部固定；事务边界由调用方保证。
// releaseRunCredentialRefs releases the run's pins; the caller owns the transaction.
func releaseRunCredentialRefs(ctx context.Context, ex sqlExecutor, runID string, at time.Time) (int, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE run_credential_refs SET released_at = ?
		  WHERE run_id = ? AND released_at IS NULL`, at.UnixMilli(), runID)
	if err != nil {
		return 0, fmt.Errorf("释放运行凭据固定（run_id=%s）: %w", runID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("确认运行凭据固定释放（run_id=%s）: %w", runID, err)
	}
	return int(affected), nil
}

// scanRunCredentialRef 按 runCredentialColumns 的列序扫描一行。
// scanRunCredentialRef scans one pin row.
func scanRunCredentialRef(row interface{ Scan(...any) error }) (core.RunCredentialRef, error) {
	var (
		ref        core.RunCredentialRef
		kind       string
		resourceID string
		releasedAt sql.NullInt64
	)
	err := row.Scan(&ref.RunID, &ref.CredentialID, &ref.CredentialVersion, &kind,
		&resourceID, &ref.ResourceName, &releasedAt)
	if err != nil {
		return core.RunCredentialRef{}, err
	}
	ref.ResourceKind = core.BindingKind(kind)
	ref.ResourceUID = core.ResourceUID(resourceID)
	if releasedAt.Valid {
		value := releasedAt.Int64
		ref.ReleasedAt = &value
	}
	return ref, nil
}

package storage

// 本文件是 `network_exceptions`（schema v6）的唯一 SQL 入口：网络访问例外的读写面。
// 例外只管**网络**——文件由目录边界决定、命令由白名单 / 安装清单决定，不在这里开口子。
// `note` 必填（放行必须带理由，理由进审计）；`(kind, value)` 唯一，重复登记不覆盖既有理由。
// This file owns the network exception rows. Exceptions only widen network access, the
// note is mandatory, and the (kind, value) pair is unique.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// NetworkExceptionStore 是 core.NetworkExceptionStore 的 SQLite 实现。
// NetworkExceptionStore is the SQLite implementation of the consumer-declared port.
type NetworkExceptionStore struct{ db *DB }

// 编译期断言：实现满足消费方（core）声明的端口。
// Compile-time assertion that the store satisfies the consumer-declared port.
var _ core.NetworkExceptionStore = (*NetworkExceptionStore)(nil)

// NewNetworkExceptionStore 绑定一个已打开的写模式库句柄。
// NewNetworkExceptionStore binds the store to an opened writable database handle.
func NewNetworkExceptionStore(db *DB) *NetworkExceptionStore {
	return &NetworkExceptionStore{db: db}
}

// exceptionColumns 是固定投影；写入者来自审计行的 LEFT JOIN（行缺失时为空串）。
// The fixed projection; the actor is joined from the audit row.
const exceptionColumns = `e.exception_id, e.kind, e.value, e.note, e.revision, e.created_at,
	e.operation_id, COALESCE(op.actor_subject, '')`

// List 读取全部例外，按契约的稳定顺序（`kind` 升序、`value` 升序）返回。
// List returns every exception in the contract's stable order.
func (s *NetworkExceptionStore) List(ctx context.Context) ([]core.NetworkException, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+exceptionColumns+`
		   FROM network_exceptions e
		   LEFT JOIN management_operations op ON op.operation_id = e.operation_id
		  ORDER BY e.kind ASC, e.value ASC`)
	if err != nil {
		return nil, fmt.Errorf("list network exceptions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]core.NetworkException, 0, 8)
	for rows.Next() {
		var (
			exception core.NetworkException
			createdAt int64
		)
		if err := rows.Scan(&exception.ExceptionID, &exception.Kind, &exception.Value,
			&exception.Note, &exception.Revision, &createdAt, &exception.OperationID,
			&exception.CreatedBy); err != nil {
			return nil, fmt.Errorf("scan network exception: %w", err)
		}
		exception.CreatedAt = time.UnixMilli(createdAt).UTC()
		out = append(out, exception)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate network exceptions: %w", err)
	}
	return out, nil
}

// Lookup 按标识取一行；未命中返回 ok=false（无行不是错误）。
// Lookup fetches one row by id; a miss returns ok=false.
func (s *NetworkExceptionStore) Lookup(ctx context.Context, exceptionID string) (core.NetworkException, bool, error) {
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+exceptionColumns+`
		   FROM network_exceptions e
		   LEFT JOIN management_operations op ON op.operation_id = e.operation_id
		  WHERE e.exception_id = ?`, exceptionID)
	var (
		exception core.NetworkException
		createdAt int64
	)
	err := row.Scan(&exception.ExceptionID, &exception.Kind, &exception.Value, &exception.Note,
		&exception.Revision, &createdAt, &exception.OperationID, &exception.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return core.NetworkException{}, false, nil
	}
	if err != nil {
		return core.NetworkException{}, false, fmt.Errorf("lookup network exception %q: %w", exceptionID, err)
	}
	exception.CreatedAt = time.UnixMilli(createdAt).UTC()
	return exception, true, nil
}

// Create 插入一行例外；`(kind, value)` 已被占用时返回 `core.ErrPolicyRevisionConflict`
// 包装的唯一键冲突——**不**覆盖既有理由（覆盖会让原有审计理由悄悄消失）。
// Create inserts one row; a duplicate (kind, value) never overwrites the existing note.
func (s *NetworkExceptionStore) Create(ctx context.Context, exception core.NetworkException) error {
	if _, err := s.db.write.ExecContext(ctx,
		`INSERT INTO network_exceptions
		   (exception_id, kind, value, note, revision, created_at, operation_id)
		 VALUES (?, ?, ?, ?, 1, ?, ?)`,
		exception.ExceptionID, exception.Kind, exception.Value, exception.Note,
		exception.CreatedAt.UnixMilli(), exception.OperationID); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: 例外 %s=%s 已存在", core.ErrPolicyRevisionConflict,
				exception.Kind, exception.Value)
		}
		return fmt.Errorf("create network exception: %w", err)
	}
	return nil
}

// Delete 删除一行例外；行不存在时返回 `core.ErrResourceNotFound`。
// Delete removes one row; a missing row reports ErrResourceNotFound.
func (s *NetworkExceptionStore) Delete(ctx context.Context, exceptionID string) error {
	res, err := s.db.write.ExecContext(ctx,
		`DELETE FROM network_exceptions WHERE exception_id = ?`, exceptionID)
	if err != nil {
		return fmt.Errorf("delete network exception %q: %w", exceptionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect network exception delete result: %w", err)
	}
	if affected == 0 {
		return core.ErrResourceNotFound
	}
	return nil
}

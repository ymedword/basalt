package storage

// 本文件是 `resource_connection_tests` 的唯一 SQL 入口（schema v4）：每个资源只保留
// **最后一次**连接测试结果，供列表页一次读齐。历史明细不在这里——每次测试的权威记录
// 是 `management_operations` 的一行（预写 pending、回填终态）。
//
// 这张表是**投影**，不是审计：upsert 覆盖旧值不违反「版本只追加」——那条纪律约束的是
// 资源版本与操作台账，本表按定义只回答「上回测得怎样」。
// This file is the only SQL entry point of resource_connection_tests. It keeps the last
// test per resource; the authoritative per-test history lives in management_operations.

import (
	"context"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// ConnectionTestStore 是 core.ResourceConnectionTestStore 的 SQLite 实现。
// ConnectionTestStore is the SQLite implementation of the consumer-declared port.
type ConnectionTestStore struct{ db *DB }

// 编译期断言：实现满足消费方（core）声明的端口。
// Compile-time assertion that the store satisfies the consumer-declared port.
var _ core.ResourceConnectionTestStore = (*ConnectionTestStore)(nil)

// NewConnectionTestStore 绑定一个已打开的写模式库句柄。
// NewConnectionTestStore binds the store to an opened writable database handle.
func NewConnectionTestStore(db *DB) *ConnectionTestStore { return &ConnectionTestStore{db: db} }

// connectionTestKinds 是表 CHECK 允许的两个资源种类（模型连接与通知渠道）。
// connectionTestKinds mirrors the table's CHECK constraint.
var connectionTestKinds = map[core.ResourceKind]bool{
	core.ResourceModelConnection: true,
	core.ResourceNotifyChannel:   true,
}

// ConnectionTestStatuses 是表 CHECK 允许的三值状态。
// connectionTestStatuses mirrors the status CHECK constraint.
var connectionTestStatuses = map[string]bool{
	core.ConnectionTestSuccess: true,
	core.ConnectionTestFailed:  true,
	core.ConnectionTestUnknown: true,
}

// RecordConnectionTest 写入 / 覆盖一个资源的最后一次测试结果。非法种类、非法状态与
// 空 UID 在 SQL 之前拒绝——把 CHECK 违例留给驱动只会得到难读的错误，且分不清是装配
// 错误还是数据错误。
// RecordConnectionTest upserts the last result and rejects shapes the schema would reject.
func (s *ConnectionTestStore) RecordConnectionTest(ctx context.Context, rec core.ResourceConnectionTest) error {
	if !connectionTestKinds[rec.Kind] {
		return fmt.Errorf("连接测试记录的资源种类非法：%q", rec.Kind)
	}
	if rec.ResourceUID == "" {
		return fmt.Errorf("连接测试记录缺少资源 UID（%s）", rec.Kind)
	}
	if !connectionTestStatuses[rec.Status] {
		return fmt.Errorf("连接测试记录的状态非法：%q", rec.Status)
	}
	_, err := s.db.write.ExecContext(ctx,
		`INSERT INTO resource_connection_tests
		   (resource_kind, resource_uid, tested_at, status, detail, operation_id)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(resource_kind, resource_uid) DO UPDATE SET
		   tested_at = excluded.tested_at,
		   status = excluded.status,
		   detail = excluded.detail,
		   operation_id = excluded.operation_id`,
		string(rec.Kind), string(rec.ResourceUID), rec.TestedAt.UnixMilli(),
		rec.Status, rec.Detail, rec.OperationID)
	if err != nil {
		return fmt.Errorf("写入连接测试记录（%s/%s）: %w", rec.Kind, rec.ResourceUID, err)
	}
	return nil
}

// ListConnectionTests 返回某一资源种类的全部最后测试结果，按 UID 升序。
// ListConnectionTests returns every last-test row of one kind, ordered by uid.
func (s *ConnectionTestStore) ListConnectionTests(ctx context.Context,
	kind core.ResourceKind,
) ([]core.ResourceConnectionTest, error) {
	if !connectionTestKinds[kind] {
		return nil, fmt.Errorf("连接测试记录的资源种类非法：%q", kind)
	}
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT resource_kind, resource_uid, tested_at, status, detail, operation_id
		   FROM resource_connection_tests
		  WHERE resource_kind = ?
		  ORDER BY resource_uid`, string(kind))
	if err != nil {
		return nil, fmt.Errorf("列出连接测试记录（%s）: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.ResourceConnectionTest
	for rows.Next() {
		var (
			rec      core.ResourceConnectionTest
			kindText string
			uidText  string
			testedAt int64
		)
		if err := rows.Scan(&kindText, &uidText, &testedAt, &rec.Status, &rec.Detail,
			&rec.OperationID); err != nil {
			return nil, fmt.Errorf("读取连接测试记录（%s）: %w", kind, err)
		}
		rec.Kind = core.ResourceKind(kindText)
		rec.ResourceUID = core.ResourceUID(uidText)
		rec.TestedAt = time.UnixMilli(testedAt).UTC()
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取连接测试记录（%s）: %w", kind, err)
	}
	return out, nil
}

package postgres

// 本文件给 `*DB` 补三条查询 / 执行委托（tasks.md 附录 R28.4 / P-14）：`pgtest` 子包与
// `internal/callers` 的集成用例都在**隔离 schema** 上执行裸 SQL（建 / 删 schema、断言
// 用量行），而 Go 的包边界不允许它们触碰私有连接池。三条委托让 `*DB` 直接满足既有
// `Queryer`，不引入第二套句柄、不改任何既有签名，也不在生产 API 里留测试专用后门。
// This file adds the three delegating query methods on *DB so external test packages
// (pgtest and the callers integration suite) can run raw SQL on an isolated schema.
// *DB thereby satisfies the existing Queryer without a second handle type.

import (
	"context"
	"database/sql"
)

// 编译期断言：句柄满足既有查询面（迁移与启动校验共用同一接口）。
// The handle satisfies the existing query surface used by migrations and startup checks.
var _ Queryer = (*DB)(nil)

// QueryContext 在底层连接池上执行一次查询。返回的 `*sql.Rows` 由调用方关闭。
// QueryContext runs one query on the underlying pool; the caller closes the rows.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.pool.QueryContext(ctx, query, args...)
}

// QueryRowContext 在底层连接池上执行一次预期返回单行的查询。
// QueryRowContext runs one single-row query on the underlying pool.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.pool.QueryRowContext(ctx, query, args...)
}

// ExecContext 在底层连接池上执行一条不返回行的语句。
// ExecContext runs one statement that returns no rows on the underlying pool.
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.pool.ExecContext(ctx, query, args...)
}

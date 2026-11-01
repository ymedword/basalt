// Package postgres 是 PostgreSQL 谱系（schema_version 自 1 起）的唯一运行期存储适配层：
// 实现 core 与各领域包声明的消费方窄端口，手写 SQL，不引入 ORM。
// Package postgres is the single runtime storage adapter of the PostgreSQL lineage
// (schema_version starts at 1). It implements the consumer-side narrow ports declared
// by core and the domain packages, with hand-written SQL and no ORM.
//
// 与 internal/storage 的关系：那个包是 SQLite 谱系（v6 冻结），自本 US 起只作迁移来源与
// 回退快照；两个谱系各自版本化，**不得**互相赋值版本号，**不得**在同一进程内同时装配。
// The SQLite package is a migration source only; the two lineages are versioned
// independently and are never assembled side by side.
//
// # 移植来源与落点对照（T120）
// File-group disposition for the port from internal/storage (31 non-test files):
//
//   - 22 个 `*_store.go` 与 6 个非 store 文件（doc / llm_calls / migrate / recovery /
//     run_query / tool_invocations）**同名移植**，行为逐文件保留；
//   - `db.go`（句柄、读写池、版本 / 表名自省、写锁）→ 本包 `connect.go`（Open / Ping /
//     Close / SchemaVersion / TableNames / openPool）+ `tx.go`（`withTx` 取代
//     `withWriteLock`）。SQLite 的只读池与库文件存在性判定是文件后端的形态，PG 不继承
//     ——运行期只有一条连接池（设计 P-01 只点名 Open / Migrate / Tx）；
//   - `bootstrap.go`（init 引导、版本判定的加锁重判）→ `Open` 的失败即拒绝 +
//     `migrate.go` 的 `ensureSchema`（advisory 锁内用同一个 `verifySchemaVersion` 重断言）；
//   - `pragmas.go`（DSN 内 PRAGMA、WAL 切换）→ `connect.go` 的 `normalizeDSN` /
//     `parseConnConfig`（`TimeZone=UTC`、`target_session_attrs=read-write`、
//     `statement_timeout` / `lock_timeout` 逐连接下发）。PG 没有 journal_mode 这类需要
//     运行期切换的文件级开关。
//
// 这三处是后端替换本身的差异，不是漏移植；同名文件之外的任何第二套句柄 / 事务入口都
// **不得**出现（与 `tx.go` 头部的纪律同一条）。
package postgres

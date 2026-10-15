// Package storage 是持久化基座：SQLite 打开、引导与受控前向迁移的实现方。
// Package storage is the persistence base: the implementation side of SQLite
// opening, bootstrap and controlled forward migration.
//
// 表 / 列 / 状态字面量的唯一事实源是 docs/contracts/schema.sql；本包的
// schema.sql 是它的逐字节嵌入副本（由单测强制）。SQL 集中在包内——审计写入
// 只允许出现在 storage，其余包经 core 定义的接口调用（宪法第 9 条）。
//
// The single source of truth for schema literals is docs/contracts/schema.sql;
// the copy embedded here must stay byte-identical. SQL is kept inside this
// package; every other package goes through interfaces declared in core.
package storage

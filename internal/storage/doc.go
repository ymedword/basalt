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
//
// 事务边界纪律：一次写权事务只包住**一次动作内部**的语句——**不得**跨 LLM 调用与工具执行
// 开事务（写池恒一条连接，长事务会把整个实例的写入挡在门外，且回滚粒度会大到连已发生的
// 外部副作用都说不清）。崩溃恢复扫描的执行面按 Run 划分：一个 Run 的组修复与运行收尾在
// **同一短事务**内完成——补齐的消息与收尾的状态要么一起落盘、要么一起回滚，不留"半截组
// 补了、运行还挂着"的中间态。
//
// Transaction boundaries stay short: a transaction never spans an LLM call or a tool
// execution, because the write pool holds exactly one connection and a long transaction
// would block every other writer. In the recovery scan, one Run's group repair and its run
// closeout share a single short transaction, so they commit or roll back together.
package storage

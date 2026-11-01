// Package dbmigrate 把 SQLite v6 实例整体搬到 PostgreSQL：导出、导入、两侧校验与工作区
// 资产清单。它只由 `basalt db` 子命令使用，**不进入** serve 装配，因此不改变运行期包依赖图。
// Package dbmigrate moves a SQLite v6 instance to PostgreSQL: export, import, verification
// on both sides, and the workspace asset manifest. It is used only by the `basalt db`
// subcommands and never appears in the serve wiring, so the runtime dependency graph is
// unaffected.
//
// **迁移是单向的一次性动作，不是同步机制**：导入前先重算文件 / 表 / 行 digest 与清单比对，
// 不符即中止；导入在单个前向事务内完成，显式保留 ID，**不得**归一状态字面量。
// Migration is a one-way, one-shot operation, not replication: digests are recomputed and
// compared with the manifest before import, and any mismatch aborts. Import runs inside a
// single forward transaction, preserves IDs explicitly, and never normalizes status
// literals.
//
// **规范化编码只有一套**：行哈希与 IDSetSHA256 的编码器由导出端与导入端共用，**不得**两端
// 各写一套；复合主键按 schema 声明的主键列序编码并在 testdata 冻结 golden vector。
// One canonical encoder serves both sides: row hashes and IDSetSHA256 share the same
// implementation, and composite primary keys are encoded in declared key order against
// frozen golden vectors.
//
// **失败即停**：导入失败留下空表而不做部分回滚后的自动重试；切换后一旦产生首次外部写，
// 回退窗口即关闭（C-46 / C-47 / C-49）。
// Failures stop the drill: a failed import leaves empty tables with no automatic retry, and
// the rollback window closes at the first external write after cutover.
package dbmigrate

-- Basalt 数据库迁移（PostgreSQL 谱系 v1 → v2）
--
-- 变更：`agent_resource_bindings.resource_kind` 的闭集加入 `knowledge_collection`
-- （设计 D-19 / 契约 agent-md §2.7）。AGENT.md 的 `knowledge:` 块投影成同表的行，
-- `resource_uid` 列存 `col_` 前缀的 `collection_id`（不是 `rs_` 资源身份——按 UID
-- 反查资源引用的闸门不得用它的值，见 core.BindingKnowledgeCollection）。
--
-- 为什么是前向迁移而不是改建表字面量：`docs/contracts/schema.pg.sql` 与
-- `migrations/0001_postgres_v1.sql` 是**已发布的 v1 基线**（二者逐字节相等，由
-- TestPostgresSchemaFile_MatchesContract 锁定），迁移纪律只追加、不得改写。
-- 既有库**实际应用**的是 v1，因此把闭集加宽做成 0002：既有库与新建库走同一条
-- ensureSchema 路径，`SupportedSchemaVersion` 同步前移到 2。
--
-- 可重跑：DROP + ADD 在同一迁移事务内成对执行、幂等；版本记录与脚本同事务提交，
-- 中途失败整体回滚（migrate.go 的既有语义）。
--
-- 只加宽闭集、不新增表：契约 v1 的 52 张表集合不变（TestPostgres_FreshSchemaIsV1
-- 的 `require.Len(tables, 52)` 继续成立）。
--
-- 约束名取 PostgreSQL 为内联列级 CHECK 自动生成的名字
-- `agent_resource_bindings_resource_kind_check`（2026-10-30 在隔离 schema 实测确认；
-- DROP IF EXISTS 让已带同名的库与不带该名的库都安全）。

ALTER TABLE agent_resource_bindings
    DROP CONSTRAINT IF EXISTS agent_resource_bindings_resource_kind_check;

ALTER TABLE agent_resource_bindings
    ADD CONSTRAINT agent_resource_bindings_resource_kind_check
    CHECK (resource_kind IN ('model_connection', 'notify_channel', 'knowledge_collection'));

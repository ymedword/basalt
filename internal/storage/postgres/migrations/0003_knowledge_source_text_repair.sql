-- Basalt 数据库迁移（PostgreSQL 谱系 v1 → v2 → v3）
--
-- 变更：给 `kb_document_versions` 补 `source_text` 列（`text`，可空）。
--
-- 为什么需要一条"补列"迁移：该列本来写在 v1 基线里，但 v1 在处理过程中被**就地
-- 改写**过（R59，2026-10-28：来源视图逐字回读需要原文快照）。改写之前按 v1 建过
-- 库的实例，版本记录停在 1、结构里没有这一列；`ensureSchema` 只应用
-- `current+1..target` 的脚本，**不会**重跑 v1，因此这类库永远拿不到该列——知识库
-- 读取会以 `column "source_text" does not exist` 失败（`LoadVersion` 的 SELECT 与
-- 摄取写入都引用它）。
--
-- 为什么是前向迁移而不是再改 0001：原则 IX 与契约头部「迁移纪律」要求已发布迁移
-- 只追加、不得改写，0001 已作为已发布基线被实际应用（0002 头部同一前提）。基线保持
-- 现状 = 新建库由 0001 建列、0003 空转；既有库由 0003 补列——两条路径收敛到同一结构，
-- 版本链 1→2→3 对两者一致。
--
-- 可重跑：`ADD COLUMN IF NOT EXISTS` 幂等（列已存在时是 no-op，不改类型、不动数据）；
-- 版本记录与脚本在同一迁移事务内提交，中途失败整体回滚（migrate.go 的既有语义）。
--
-- 只补列、不新增表、不碰扩展：契约 v1 的 52 张表集合不变。

ALTER TABLE kb_document_versions
    ADD COLUMN IF NOT EXISTS source_text text;

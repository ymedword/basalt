-- Basalt 数据库迁移（PostgreSQL 谱系 v12 → v13）
--
-- 已发布资产自动分发（US-25C / FR-25C-03）：分发介质是数据库载荷表
-- `agent_bundle_payloads`（内容寻址、同哈希只存一行），配套三处增量列与两处索引：
--   * `worker_nodes.heartbeat_interval_seconds`——实例自报的心跳间隔，供概览按行
--     计算诊断阈值（FR-25C-31）；可空，旧行缺值由控制面的部署默认兜底；
--   * `resource_pointers (resource_kind, updated_at, resource_id)`——增量发现游标的
--     键集索引（`(updated_at, resource_id) > ($1, $2)` 严格前进，同毫秒多行每行只读
--     一次）；
--   * `task_executions.pinned_bundle_hash`——领取事务钉住的 bundle 哈希（FR-25C-08），
--     是 Run 准入"允许使用旧版本"的唯一凭证；可空，历史行与 SQLite 源库没有它。
--
-- 为什么是前向迁移：`docs/contracts/schema.pg.sql` 与 `0001_postgres_v1.sql` 是
-- 已发布的 v1 基线（二者逐字节相等、冻结），迁移纪律只追加、不得回头改写；
-- 既有库实际应用的是 v1..v12，新结构只能以 0013 追加。`schema.pg.sql` 本 US 不动
-- （D-25C-02）。
--
-- 纪律：
--   * 迁移运行器以**无参数** `ExecContext` 执行脚本（internal/storage/postgres/migrate.go），
--     任何 `$1` 占位符都会直接报错——本文件不出现参数占位符。
--   * 全部 DDL 幂等（IF NOT EXISTS / DROP IF EXISTS + ADD），重复执行不报错、不重复建对象。
--   * 时间列一律 bigint Unix 毫秒（UTC），由 Go 在写入时提供；现有表**没有**时间列
--     DEFAULT，本文件不新增先例。`agent_bundle_payloads` 没有回填：载荷只由发布链
--     （先写载荷再移指针）与存量回填（BackfillBundlePayloads，先验目录再编码）写入，
--     迁移本身不伪造任何载荷行。
--   * 载荷表的保留策略如实登记为已知缺口（D-25C-09）：只做内容寻址去重，**不**自动
--     清理；本迁移不建任何清理作业 / 触发器。

-- ============================================================
-- 已发布 bundle 载荷（FR-25C-01 / FR-25C-02 / FR-25C-04）
-- 内容寻址：`bundle_hash` 即 `assets.Manifest.BundleHash`，同哈希只存一行；
-- `payload_sha256` 是归档字节的哈希（与目录内容口径不同，两列都要能独立复核）。
-- ============================================================

CREATE TABLE IF NOT EXISTS agent_bundle_payloads (
    bundle_hash     text    PRIMARY KEY,
    agent           text    NOT NULL,
    agent_uid       text    NOT NULL,
    format_version  integer NOT NULL,
    payload         bytea   NOT NULL,
    payload_sha256  text    NOT NULL,
    size_bytes      bigint  NOT NULL,
    created_at      bigint  NOT NULL
);

-- 按 Agent 诊断（`basalt assets status` 与运维排障）用；键集发现走的是
-- resource_pointers 的索引，不扫本表。
CREATE INDEX IF NOT EXISTS idx_agent_bundle_payloads_agent
    ON agent_bundle_payloads(agent, created_at DESC);

-- ============================================================
-- Worker 心跳间隔登记（FR-25C-31）：概览按行计算 max(3 × 间隔, 30s)，
-- 不拿管理台进程自己的间隔套到所有实例。可空列——旧行留 NULL，由控制面注入的
-- 部署默认 `worker.heartbeat_interval_seconds` 兜底并在详情标注。
-- 约束是最后一道：0 / 负数由应用层拒绝，数据库 CHECK 兜底；0 → NULL 的语义
-- 属于登记层，不属于列层。
-- ============================================================

ALTER TABLE worker_nodes ADD COLUMN IF NOT EXISTS heartbeat_interval_seconds integer;
ALTER TABLE worker_nodes
    DROP CONSTRAINT IF EXISTS worker_nodes_heartbeat_interval_seconds_check;
ALTER TABLE worker_nodes
    ADD CONSTRAINT worker_nodes_heartbeat_interval_seconds_check
    CHECK (heartbeat_interval_seconds IS NULL OR heartbeat_interval_seconds > 0);

-- ============================================================
-- 增量发现游标的键集索引（FR-25C-10）：按 kind 过滤后，
-- `(updated_at, resource_id) > ($1, $2)` 严格前进；用 `>=` 会在同毫秒边界重复
-- 读同一批行，用 `>` 单列会漏掉同毫秒的其余行。全量兜底扫描按 `resource_id`
-- 翻页——它刻意不按 updated_at 排序（迟到提交的行必须仍然被扫到）。
-- ============================================================

CREATE INDEX IF NOT EXISTS idx_resource_pointers_kind_updated_id
    ON resource_pointers(resource_kind, updated_at, resource_id);

-- ============================================================
-- 领取事务的固定版本凭证（FR-25C-08）：只有带钉住的领取路径写这一列，
-- 历史行 / 非调度通道留 NULL（准入走既有当前指针语义）。**不得**回填历史行，
-- 也不得用调用方传入的哈希填充。
-- ============================================================

ALTER TABLE task_executions ADD COLUMN IF NOT EXISTS pinned_bundle_hash text;

-- ============================================================
-- 到期扫描非 NULL 段的键集辅助索引（可选，§2.3.5）：NULL 段按 task_id 走，
-- 非 NULL 段按 (next_fire_at, task_id) 走；部分谓词把 NULL 行排除在这棵树外，
-- 与两段轮转的扫描计划一致。
-- ============================================================

CREATE INDEX IF NOT EXISTS idx_scheduled_tasks_next_fire_task
    ON scheduled_tasks(next_fire_at, task_id) WHERE next_fire_at IS NOT NULL;

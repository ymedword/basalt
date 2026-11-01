# Phase 1 Data Model — US-16

**Feature**: `016-knowledge-memory-history`｜**Date**: 2026-10-28｜**权威**：`docs/us/us16-knowledge-memory-history.md` §3.4（表清单与约束）；本文只做实体化转写

> PostgreSQL 谱系当前为 **v3**（`SupportedSchemaVersion = 3`）：v1 基线 19 张新表 + 既有 33 张表演进；v2 由 `0002_agent_binding_knowledge_kind.sql` 把 `agent_resource_bindings.resource_kind` 闭集加宽出 `knowledge_collection`（D-19）；v3 由 `0003_knowledge_source_text_repair.sql` 给 `kb_document_versions` 补回 R59 就地改写后旧库缺失的 `source_text`（2026-10-31 裁决第 10 条，见 `contracts/README.md` §1）。表集合仍 52 张。SQLite 谱系 **v6** 冻结，只作迁移源。列级 DDL 以 `docs/contracts/schema.pg.sql`（**已冻结，2026-10-28**；2026-10-31 实测当前读数：1195 行 / SHA-256 `e6f8ff0b2f2b10a67151c09a394e24610fb6ec03ebb3c44643d66da47beb14f7`，S-F07 仅注释改写、DDL 零变更，与 `0001_postgres_v1.sql` 逐字节相等；此前的 `0e1aedae…` 与 `eb4a883f…` 保留为历史读数，见 `contracts/README.md` §1 读数勘误）为准；`vector(512)` 随 D-05 冻结，索引代际 1 = 精确扫描（D-06）。R18 重冻结：`purpose` 增 `retrieval` / `compaction` 两处取值，`compaction_entries` 三个消息标识列 `bigint` → `text` 并去掉外键与列间顺序 CHECK（见 `contracts/README.md` §1 与颗粒度文档 R18）。
> **本文与冻结契约冲突时以契约为准**；下表是实体化转写，列名以契约 DDL 逐字核对（2026-10-28 analyze B-18 修订）。

## 1. 知识库

| 实体 | 表 | 关键字段 | 约束 / 校验 |
| --- | --- | --- | --- |
| 集合 | `kb_collections` | `collection_id` / `name` / `project` / `deleted_at` | 集合名在 project 内唯一；软删后不可摄取 |
| 文档 | `kb_documents` | `document_id` / `collection_id` / `source_path` / `content_hash` / `latest_version_id` / `deleted_at` | `content_hash` 相同**不得**产生新版本 |
| 文档版本 | `kb_document_versions` | `version_id` / `document_id` / `content_hash` / `status` / `parser_version` / `chunker_version` / `generation` / `error_kind` | 状态机见 §2 |
| 切块 | `kb_chunks` | `chunk_id` / `version_id` / `seq` / `heading_path` / `char_start` / `char_end` / `text` / `content_hash` / `tsv` | `(version_id, seq)` 唯一；区间必须落在原文范围 |
| 切块向量 | `kb_chunk_embeddings` | `chunk_id` / `generation` / `embedding vector(512)` / `model` / `dimensions` | `(chunk_id, generation)` 唯一；`CHECK (dimensions = 512)`；**不得**改成无维度 `vector`，也**不得**在同一列混写不同维度 |
| 索引代际 | `kb_index_generations` | `generation` / `embedding_model` / `dimensions` / `distance` / `status` / `activated_at` / `retired_at` | 同一时刻至多一个 `active` 代际 |
| 摄取作业 | `kb_ingest_jobs` | `ingest_job_id` / `document_id` / `version_id` / `stage` / `attempt` / `status` / `error_kind` | `attempt` 递增；每次尝试独立一行 |

**关系**：`kb_collections 1─N kb_documents 1─N kb_document_versions 1─N kb_chunks 1─N kb_chunk_embeddings`；`kb_index_generations` 定义代际，切块向量按代际分组。

## 2. 文档版本状态机

```text
pending ──解析/切分成功──> indexed ──代际激活──> active ──新版本激活──> superseded
   │                          │
   └──失败──> failed          └──删除/撤权──> retired
```

- 只有 `active` 版本参与检索；`superseded` / `retired` 仅保留回溯与引用失效判定。
- `failed` 版本**不得**被检索看到，重试**必须**新建版本行，不覆盖旧行。

## 3. 记忆

| 实体 | 表 | 关键字段 | 约束 |
| --- | --- | --- | --- |
| 记忆条目 | `memory_entries`（既有表演进） | 增 `state` / `source` / `valid_from` / `valid_to` / `supersedes_id` / `confirmed_by` / `content_hash` | 六态见下；同一 `supersedes_id` 链**不得**成环 |
| 记忆向量 | `memory_embeddings` | `entry_id` / `generation` / `embedding vector(512)` / `model` / `dimensions` | 换模型建新代际，旧代际保留到回退窗口结束 |
| embedding 审计 | `embedding_calls` | `id` / `run_id` / `ingest_job_id` / `model` / `dimensions` / `input_count` / `prompt_tokens` / `status` / `error_kind` | 两阶段（先 `pending` 后终态），写入路径唯一 |

**记忆六态**：`candidate`（显式提议产生）→ `active`（`save_memory` 直接写入）→ `superseded` / `corrected`；`deleted`（软删，正文不再注入）；`conflicted`（待裁决）。**Run 内注入文本冻结**，但读取面权限每次校验。

## 4. 历史与压缩

| 实体 | 表 | 关键字段 | 约束 |
| --- | --- | --- | --- |
| 消息来源引用 | `message_evidence_refs` | `message_id` / `kind` / `ref_id` / `version_id` / `content_hash` | 与 `messages` 同事务写入；压缩引用集合的**唯一**可信来源 |
| 压缩条目 | `compaction_entries` | `compaction_id` / `scope_session_id` / `scope_run_id` / `chain_epoch` / `covered_from_message_id` / `cut_message_id` / `attempt_id` / `parent_compaction_id` / `status` / `summary` / `source_refs` / `included_refs` | 见下四条 |
| 装配记录 | `context_assembly_records` | `record_id` / `run_id` / `step_id` / `llm_call_id` / `purpose` / `agent` / `prompt_version` / `model_version` / `budget` / `estimator_version` / `candidates` / `final_segments` | 记录能证明「注入了什么」，不能证明「模型据此作答」 |

**压缩条目约束**：

1. `CHECK (num_nonnulls(scope_session_id, scope_run_id) = 1)`——会话范围与无状态范围恰有一个。
2. 两条**部分唯一索引**（`WHERE scope_* IS NOT NULL AND status = 'committed'`）按 `(scope, chain_epoch, covered_from_message_id, cut_message_id)` 去重。
3. `attempt_id` 普通 UNIQUE：一次尝试一行；重提**必须**换新 `attempt_id`。
4. `chain_epoch >= 1`；根条目 `parent_compaction_id IS NULL`；重建时 `chain_epoch = 同 scope 最大值 + 1`；自引用直接拒绝。
5. `summary` 是派生视图，**不建全文索引、不参与历史检索**，任何消息读取路径**不得**返回它。

## 5. 调用方与配额

| 实体 | 表 | 关键字段 | 约束 |
| --- | --- | --- | --- |
| 调用方 | `callers` | `caller_id` / `kind` / `name` / `project` / `status` / `created_at` | `local-admin` 为内置；`status` 控制整体停用 |
| Key | `caller_keys` | `key_id` / `caller_id` / `key_hash` / `key_prefix` / `scopes` / `expires_at` / `revoked_at` | `key_hash` UNIQUE；明文**不落库、不回显** |
| 限额 | `caller_limits` | `caller_id` / `qps` / `burst` / `max_concurrent` / `tasks_per_period` / `tokens_per_period` / `ingest_bytes_per_period` / `period` / `timezone` | 缺省 `UTC`；周期按 timezone 本地 00:00 起算 |
| 周期用量 | `caller_usage_periods` | `caller_id` / `period_start` / `kind` / `used` / `reserved` / `overage` / `updated_at` | `(caller_id, period_start, kind)` UNIQUE |
| 预留 | `quota_reservations` | `reservation_id` / `caller_id` / `idempotency_key` / `class` / `run_id` / `ingest_job_id` / `period_start` / `amount` / **`actual`** / **`overage`** / **`state`** / **`reason`** / `created_at` / **`settled_at`** | `(caller_id, idempotency_key)` UNIQUE（**不是** `(caller_id, run_id, kind)`——同一 Run 可有多条）；`state ∈ {pending, committed, released, reconciled}`；`state = 'committed' ⇒ actual IS NOT NULL`；**不设** `unknown` 列 |

**记账规则**：预留 → 执行 → 核对（用量或退回）；未知结果先核对再决定重试；被拒请求**零占用**。

## 6. 迁移与身份

| 实体 | 表 | 关键字段 | 约束 |
| --- | --- | --- | --- |
| 切换证据 | `storage_cutover` | `cutover_id` / `source_schema_version` / `manifest_sha256` / `cutover_at` / `operator` / `first_write_at` | 一次切换一行；`manifest_sha256` UNIQUE；`first_write_at` 由 `MarkFirstWrite` 写 `at.UTC().UnixMilli()` 的 **BIGINT 毫秒**，写入后只前滚 |

实例身份（名册）与主密钥记录仍由既有文件 + `instance_secrets` 承载，**不**随数据库迁移搬迁；交接见颗粒度文档 §2.12。

## 7. 诊断

| 实体 | 表 | 关键字段 | 约束 |
| --- | --- | --- | --- |
| 诊断事件 | `diagnostic_events` | `event_id`(PK) / `schema_version` / `kind` / `occurred_at` / `request_id` / `run_id` / `task_id` / `operation_id` / `ingest_job_id` / `step_id` / `attempt_id` / `parent_step_id` / `seq` / `attrs` | **局部有序**，不设跨节点全局序号；可采样丢弃 |

**与审计的关系**：审计是 fail-closed 权威记录；诊断**可丢弃**且写失败不阻塞业务。两者**不得**互相替代。

## 8. 既有表演进（提案）

| 表 | 变更 | 说明 |
| --- | --- | --- |
| `sessions` | 增 `caller_id` | 会话归属；迁移遗留 `NULL` 只允许 admintoken 读取 |
| `runs` | 增 `caller_id` / `authorizing_kind` / `key_id` / `reservation_id` / `actor` | 归属与授权来源；逐列迁移规则与 CHECK 对齐 |
| `scheduled_tasks` | 增 `caller_id` / `authorizing_kind` / `authorizing_key_id` | 任务创建时固定归属与授权 Key |
| `memory_entries` | 增 `state` / `source` / `valid_from` / `valid_to` / `supersedes_id` / `confirmed_by` / `content_hash` | 六态与有效头 |
| 其余 29 张表 | 逐表映射 | 见 §3.4.3 迁移清单 |

## 9. 全局校验规则

- 所有时间列 **`bigint` Unix 毫秒 UTC**，**不得**用 `timestamptz`（设计 C-13 / C-14 / §3.4.1；`TestPostgres_TimeColumnsAreUnixMillisUTC` 扫描全库列类型，出现 `timestamptz` 即失败）。两谱系表示一致：SQLite 存 `INTEGER`，PG 存 `bigint`，迁移逐值无损、无需时区换算。
- 所有软删列 `deleted_at` / `revoked_at` 为 `bigint` 毫秒，NULL 表示有效。
- 所有哈希列为 `bytea` 或定长 hex 文本，统一小写。
- 任何「未知结果」**不得**用第三态列表达，靠 `pending` 预写 + 核对收敛。

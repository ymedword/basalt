# US22 数据模型

五张新表（`internal/storage/postgres/migrations/0011_memory_learning.sql`）+ 两个既有 CHECK 闭集扩展，
schema 版本 `10 → 11`，表数 `82 → 87`。时间列一律 bigint Unix 毫秒（UTC），与既有谱系一致。
`docs/contracts/schema.pg.sql` 是冻结基线，**不改**；新表只存在于追加迁移。

## 既有表扩展

- `memory_entries.source`：CHECK 追加 `auto_extract`（原 `save_memory / explicit_propose / legacy_import / migration`）；Go 闭集 `memoryEntrySource` 同步追加，否则「迁移通过、写入被 Go 拒绝」。
- `llm_calls.purpose`：CHECK 追加 `memory_extract`（原 `chat / rerank / summary / embedding / compaction`）。
- `memory_entries` 本 US 起写入 `valid_to`（`memoryRow.validTo`，零值落 NULL）；候选插入路径直接复用包内 `insertMemoryRow`，不调用会用 `s.now()` 覆盖 `created_at` 的 `InsertEntry`。
- 两个 CHECK 用 `DROP CONSTRAINT IF EXISTS` + `ADD CONSTRAINT` **按定义替换**；只 `ADD` 会留下旧约束继续拒绝新取值。实际约束名以实现时 `pg_constraint` 按定义核对为准。

## `memory_extractions`（抽取账本，一行 = 一次触发 Run）

主键 `extraction_id`；`run_id` **UNIQUE**（一个 Run 至多建一行，重复触发在插入处被拒）。同来源后续 Run 可重试同一行（`attempts` 1→3），`last_attempt_run_id` 记录最近一次尝试。

| 分组 | 列 |
| --- | --- |
| 身份 | `agent_name`、`session_id`、`run_id`、`last_attempt_run_id`、`origin_caller_id`、`origin_key_id`（`''` = 本地管理员，`runs.key_id IS NULL`） |
| 窗口 | `window_from_message_id`、`window_to_message_id`、`window_messages`、`dropped_window_messages`、`prior_messages`、`existing_entries` |
| 状态 | `status`、`attempts`、`failure_class`、`error_message`（≤1 KiB，只写分类与供应商错误摘要，不含正文 / 证据 / 凭证） |
| 尝试身份 | `provider`、`model`、`config_hash`（均取最近一次尝试的触发 Run 冻结值） |
| 计数 | `items_total`、`items_accepted`、`items_dropped` |
| 版本 | `prompt_version`、`rule_version` |
| 时刻 | `created_at`、`last_attempt_at`、`finished_at`、`verified_at`（启动恢复查证时刻） |

CHECK：`status IN (pending, success, failed, skipped, unknown)`；`failure_class IS NULL OR IN (model_error, invalid_output, timeout, audit_unavailable, extractor_unavailable, window_gone)`；`error_message` 长度 ≤1024；`window_to_message_id >= window_from_message_id`；`attempts BETWEEN 1 AND 3`。
索引：`(agent_name, created_at)`、`(agent_name, session_id, origin_caller_id, origin_key_id, status)`、`(status)`。

**冻结语义**：`window_from_message_id` / `window_to_message_id` 首次写入后**不再更新**；重试只重读这段，跨来源消息不会因重试被重新贴身份。

**状态机**：

```text
pending ──提交成功（同事务有候选 / 草案 / 来源关系行）──▶ success（finished_at）
pending ──工作段失败分类──▶ failed ──同来源下一 Run（attempts < 3）──▶ pending（attempts+1，同窗口）
failed ──第 3 次失败 / 冻结窗口缺失（window_gone）──▶ skipped（推进游标）
pending ──启动恢复：已有产物 ──▶ success（verified_at 写入，游标推进）
pending ──启动恢复：无产物 ──▶ unknown（verified_at 写入，游标推进，不重放）
```

`Retryable() = status=failed AND attempts < 3`。`pending` 与 `verified_at IS NULL` 的 `unknown` **阻断**同来源新窗口；已验证 `unknown` 保留历史但不再阻断；`success` / `skipped` 是终态。
进程未重启时不做恢复扫描——因此提交未知后同来源新窗口被 `pending` 阻断，而不是「碰巧不重叠」。

## `memory_extraction_cursors`（每 Agent 每会话每来源一行）

主键 `(agent_name, session_id, origin_caller_id, origin_key_id)`；`committed_message_id`（默认 0）、`updated_at`。
推进点：成功提交、`skipped`、`unknown` 恢复，均在**同一事务**内推进；恢复写 `GREATEST(committed_message_id, window_to_message_id)`（成功提交已推进时幂等）。

## `memory_candidate_meta`（与 `memory_entries` 一对一）

主键 / 外键 `entry_id REFERENCES memory_entries(id)`；`extraction_id REFERENCES memory_extractions(extraction_id)`。

| 列 | 语义 |
| --- | --- |
| `kind` | `fact / preference / constraint / inference / temporary`；**不含 method**（method 只写草案表） |
| `certainty` | `asserted / uncertain`，**程序派生**：`fact/preference/constraint/temporary` 且至少一条 `user` 证据 → `asserted`；`inference` 或无 user 证据 → `uncertain` |
| `confidence` | 模型自报，仅展示与排序提示，**永不**作为状态 / 召回 / 确认门槛 |
| `evidence_json` | 证据数组：`message_id`、`run_id`、`role`、`quote`（逐字子串）、`message_at` |
| `source_entry_ids` | 来源条目 id 数组（快照子集） |
| `suggested_supersedes_id` | 建议取代目标；必须来自条目快照，否则提交时清空 |
| `origin_caller_id` / `origin_key_id` | 从抽取行复制，请求路径无写入入口 |
| `evidence_at` | 证据消息的最大 `created_at`（记录时刻，**不是**事实发生时刻），恒 ≥ 1 |
| `valid_to` | 临时约束的绝对未来时刻；空 = 无到期 |
| `time_confidence` | `exact / relative_unknown / unknown` |

CHECK：`kind` 五值闭集；`certainty IN (asserted, uncertain)`；`evidence_at >= 1`；`time_confidence` 三值闭集。索引 `(extraction_id)`。

## `memory_candidate_sources`（派生条目 → 来源条目的有向边）

主键 `(entry_id, source_entry_id)`，两者均 `REFERENCES memory_entries(id)`，`extraction_id REFERENCES memory_extractions`，`created_at`；`CHECK (entry_id <> source_entry_id)`。
索引 `(source_entry_id)`（召回面反向闭包查询）、`(extraction_id)`。
这是召回面闭包复核的**权威边**；无边的条目即显式条目，不受派生过滤影响。

## `memory_method_drafts`（只读文件索引）

主键 `draft_id`；`extraction_id` 外键；`agent_name`、`name`、`content_sha256`、`evidence_json`、`rel_dir`、`created_at`。索引 `(extraction_id)`。
草案正文在文件系统，DB 行只作索引与恢复判据；恢复时「有草案行」即判 `success`。

## 候选条目的生命周期

`memory_entries` 中候选固定 `state='candidate'`、`source='auto_extract'`、`section='archive'`。
晋升只在统一确认事务内发生：`candidate → active`（CAS `WHERE state='candidate'`）；`supersede` 同时把目标 `active → superseded`，两张表一次提交，任一 0 行即回滚。
确认**不移动分区**：晋升后仍是 `archive`，可经 `recall_memory` 召回，但不被注入系统提示。
删除 / 到期 / 替代由既有记忆管理动作处理；本 US 不新增自动纠正或自动删除。

## 派生召回判据（`derivedLiveFilterSQL`）

三个 SQL 入口共用：`RecallByKeyword`、`RecallEntries` 关键词回退、`RecallEntries` 向量路径。
判据形如 `NOT EXISTS (WITH RECURSIVE closure AS (…沿 memory_candidate_sources…) SELECT 1 FROM closure WHERE <来源非 active 或已过期>)`；递归深度上限 8，超限按失效处理（fail-closed）；过滤在 SQL 内、排序与 top-k **之前**；无边的显式条目不受影响。
有效条数不足时结果可少于请求上限，不回填、不换用近似替代。

## 草案文件布局

```text
.basalt/learning/drafts/<draft_id>/
├── DRAFT.md      # frontmatter(draft_id / agent / extraction_id / name / created_at)
│                 # + 适用条件 / 步骤 / 反例 / 原始引用（模型正文，经程序校验）
└── evidence.json # 证据数组（message_id / run_id / role / quote / message_at）
```

目录与文件只读；路径与文件名由程序固定，模型只能提供受校验的 `name`（≤100，字符集与技能名一致）与正文（≤4 KiB）；不写脚本、不进任何 Agent 读写白名单、不执行。
提交事务失败但文件已写：保留文件、行留 `pending`、等恢复查证；不补偿删除、不重做。文件存在但无 DB 行的孤儿目录只在状态页计数，不自动删除。

## 迁移与升级

`0010 → 0011` 前向升级，不回填历史数据；旧库升级后五张表为空、钩子默认关闭。
`connect.go` 的 `SupportedSchemaVersion` 同步改为 11；既有 `TestPostgres_*` / `TestObservability_*` 迁移测试保持通过。

# Data Model: US-11 管理台与运行观测

**Branch**: `011-console-observability` | **Date**: 2026-10-21 | **Source of truth**: [docs/contracts/schema.sql](../../docs/contracts/schema.sql)（v2 段）；本文件只做阅读视图，不新增列 / 表 / 字面量。

## 迁移与版本

| 项 | 值 |
| --- | --- |
| 目标版本 | schema v2（`SupportedSchemaVersion` 1 → 2） |
| 迁移文件 | `internal/storage/migrations/0002_us11_us12_resources.sql`（只追加；已发布迁移不改写） |
| 新库 | 由 `schema.sql` 全量 DDL 直接建到 v2 |
| 旧库 | 按 `schema_version` 前向迁移；库内版本高于二进制支持版本 → 拒绝启动 |
| 时间戳 | INTEGER Unix 毫秒 |
| 幂等 | 全部 `CREATE TABLE / INDEX IF NOT EXISTS` |

## 表：`management_operations`（管理操作留痕）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `operation_id` | TEXT PK | `op-<unix_millis>-<6 位小写字母数字>`，全局唯一 |
| `action` | TEXT NOT NULL | US-11：`agent.content.update` / `agent.disable` / `agent.enable` / `system.baseline_v2` |
| `target_kind` | TEXT NOT NULL | `agent` / `schedule` / `model_connection` / `mcp_server` / `skill` / `notify_channel` / `system` |
| `target_id` | TEXT NOT NULL | 资源名 / Agent 名 / `system` |
| `actor_subject` | TEXT NOT NULL | `token:<sha256 前 16 位>` / `local_trust` / `cli:<user>` / `migration` |
| `auth_method` | TEXT NOT NULL | CHECK：`bearer` / `local_trust` / `cli` / `migration` |
| `source` | TEXT NOT NULL | `<ip>:<port>` / `cli` / `migration`；转发头只在可信代理后用于归因 |
| `request_hash` | TEXT | 脱敏请求摘要（sha256），不含凭证原文 |
| `requested_version_id` | INTEGER | 发布 / 撤回目标版本；US-11 为空 |
| `expected_etag` | TEXT | 条件写期望值；无则空 |
| `status` | TEXT NOT NULL | CHECK：`pending` / `success` / `failed` / `unknown` |
| `result_code` | TEXT | `OK` 或错误码表 code |
| `error_message` | TEXT | 失败原文（截断 4 KB；不得含凭证） |
| `detail_json` | TEXT | 脱敏详情：变更摘要 / 测试结果 / 计数 |
| `created_at` | INTEGER NOT NULL | 预写时刻 |
| `finished_at` | INTEGER | 终态回填时刻 |
| `recovered_at` | INTEGER | 启动恢复查证时刻；正常完成为空 |
| `verification` | TEXT | CHECK：`applied` / `not_applied` / `indeterminate`；仅恢复时写 |

索引：`idx_management_operations_pending(status, created_at) WHERE status='pending'`、`idx_management_operations_target(target_kind, target_id, created_at DESC)`、`idx_management_operations_created(created_at DESC)`。

**状态迁移**：`pending` →（成功）`success` /（失败）`failed`；启动恢复对仍为 `pending` 的行按动作查证 → `success` / `failed` / `unknown`，并写 `recovered_at` 与 `verification`。**不得**物理删除历史行。

## 表：`resource_versions`（不可变发布版本）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `id` | INTEGER PK AUTOINCREMENT | 全局 `version_id` |
| `resource_kind` | TEXT NOT NULL | CHECK：`model_connection` / `mcp_server` / `skill` |
| `resource_id` | TEXT NOT NULL | Provider / MCP server / 技能名 |
| `version_no` | INTEGER NOT NULL | 逐资源从 1 单调递增；CHECK ≥ 1 |
| `content_hash` | TEXT NOT NULL | 小写十六进制 sha256 |
| `definition_yaml` | TEXT | `model_connection` / `mcp_server` 的资源块原文（含 `${ENV_VAR}`，不含解析值） |
| `manifest_json` | TEXT | 技能完整清单 `[{path, sha256, size}]` |
| `storage_path` | TEXT | 技能不可变快照目录（工作区相对路径） |
| `created_at` | INTEGER NOT NULL | — |
| `operation_id` | TEXT NOT NULL FK | 指向 `management_operations` |

约束：`UNIQUE(resource_kind, resource_id, version_no)`；CHECK 保证模型 / MCP 有 `definition_yaml`，技能有 `manifest_json` + `storage_path`。版本**不可变**——撤回 = 用旧内容创建新版本。

## 表：`resource_pointers`（当前发布指针）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `resource_kind` + `resource_id` | 复合 PK | — |
| `published_version_id` | INTEGER NOT NULL FK | 最近一次发布的版本 |
| `applied_version_id` | INTEGER FK | 运行时实际生效版本；MCP 首版需重启前仍指旧版本 |
| `deleted_at` | INTEGER | 删除标记；历史版本不物理删 |
| `updated_at` | INTEGER NOT NULL | — |
| `operation_id` | TEXT NOT NULL FK | 发布操作 |

索引：`idx_resource_pointers_applied(resource_kind, applied_version_id)`。

## 表：`run_resource_manifests`（运行资源清单）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `run_id` | TEXT PK FK → `runs` | 一次运行一行 |
| `format_version` | INTEGER NOT NULL DEFAULT 1 | 清单格式版本 |
| `manifest_hash` | TEXT NOT NULL | `snapshot_json` 规整序列化后的 sha256（稳定：MCP 列表按名字排序等） |
| `agent_lifecycle_state` | TEXT NOT NULL | 准入时状态（只可能为 `active`） |
| `snapshot_json` | TEXT NOT NULL | 完整清单 JSON（Provider 版本 / MCP 生效版本 / 技能版本与 L1–L3 哈希） |
| `created_at` | INTEGER NOT NULL | — |

索引：`idx_run_resource_manifests_hash(manifest_hash)`。**历史运行没有本行** → API 返回 `legacy_unrecorded`，**不得**用当前版本补造。`runs.config_hash` 原义不变。

## 表：`run_resource_refs`（版本引用）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `run_id` + `resource_kind` + `resource_id` | 复合 PK | — |
| `version_id` | INTEGER NOT NULL FK | 指向 `resource_versions` |
| `content_hash` | TEXT NOT NULL | 冗余快照，便于对账 |
| `released_at` | INTEGER | 空 = 仍被引用；终态同事务写入 |

索引：`idx_run_resource_refs_version(version_id, released_at)`、`idx_run_resource_refs_unreleased(released_at) WHERE released_at IS NULL`。行保留作历史证据，**不因释放而删除**。

## 表：`agent_lifecycle`（可重建索引）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `agent_name` | TEXT PK | — |
| `state` | TEXT | CHECK：`active` / `disabled` / `archived` 或 NULL（文件缺失 / 损坏） |
| `generation` | INTEGER NOT NULL DEFAULT 0 | 状态文件中的单调代数；0 = 无有效状态 |
| `state_file_hash` | TEXT | 状态文件校验和（sha256 十六进制） |
| `valid` | INTEGER NOT NULL DEFAULT 0 | CHECK 0/1；1 = 文件存在且校验通过 |
| `error_message` | TEXT | 无效时的可读原因（点名字段 / 相对路径，不含凭证） |
| `updated_at` | INTEGER NOT NULL | — |
| `operation_id` | TEXT FK | 启停 / 基线操作 |

索引：`idx_agent_lifecycle_state(state, agent_name)`。**权威是状态文件**；本表只服务查询与列表，可重建。

## 表：`agent_archives`（归档索引）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `archive_id` | TEXT PK | 归档目录名 `<agent>-YYYYMMDD-HHMMSS` |
| `agent_name` | TEXT NOT NULL | 归档前名字 |
| `archived_at` | INTEGER NOT NULL | — |
| `content_hash` | TEXT NOT NULL | 归档目录清单 sha256 |
| `manifest_json` | TEXT NOT NULL | 归档目录清单 |
| `operation_id` | TEXT NOT NULL FK | 归档操作 |
| `restored_at` | INTEGER | 恢复时刻 |
| `restored_agent_name` | TEXT | 恢复后名字 |

索引：`idx_agent_archives_agent(agent_name, archived_at DESC)`、`idx_agent_archives_restorable(archived_at DESC) WHERE restored_at IS NULL`。US-11 只读面准备；写面归 US-12。

## 领域实体（代码层）

| 实体 | 归属包 | 关键字段 / 规则 |
| --- | --- | --- |
| `ManagementOperation` | `core/management.go` | 操作标识、动作、actor、目标、状态、`recovered_at`、`verification`；预写与回填由 `storage.ManagementStore` 完成 |
| `LifecycleState` / `AgentGate` / `LifecyclePort` | `core/lifecycle.go` | 状态取值、代数、校验和；每 Agent 一把读写锁；准入必须重读 |
| `ResourceGate` / `RunResourceManifest` | `core/resources.go` | 进程级 RWMutex；清单的规整序列化与哈希；持久形态与视图分离 |
| `RunSnapshot`（扩展） | `core/snapshot.go` | 准入固定 Provider / MCP / 技能目录与版本 |
| `AgentRegistry` / 快照 | `internal/app/registry.go` | 不可变集合、原子替换；准入 / 调度 / 管理台共用 |
| `AdminAuthConfig` / `AuthGuard` / `AuthLimiter` | `internal/web/auth*.go` | 模式、允许 Host / Origin、可信代理、窗口计数 |
| `SystemSample` | `internal/web/sampler.go` | `cpu_percent` / `rss_bytes` / `goroutines` / `sampled_at` / `unavailable`；无磁盘与运行时长 |
| `RunView` / `OperationView` / 资源视图 | `internal/web/views.go` | 只投影契约字段；`legacy_unrecorded` 显式标记 |
| 前端视图模型 | `assets/console/src/` | 八路由；token 只在内存；渲染器固定标签集 |

## 状态机

```text
管理操作：  pending ──成功──▶ success
              │ ├──失败──▶ failed
              └─恢复查证──▶ success / failed / unknown
                             （写 recovered_at + verification）

生命周期：  (基线) disabled ──enable──▶ active ──disable──▶ disabled
              │                          │
              └────── 文件缺失/损坏 ──────┴──▶ 拒绝新 Run（不迁移状态）
              archived（US-12 归档写面；US-11 只读展示）

运行：      running ──▶ completed / failed / cancelled / limit_reached
              └─ 取消是信号：循环自行收尾，管理员不写终态

资源指针：  published_version_id 指向新版本；发布失败保留旧指针
              applied_version_id 在重启后追上 published（MCP 首版）
```

## 清单哈希规则

- 清单规整序列化：键按字典序、列表按固定序（MCP 按名字、技能按绑定声明序），不含时间戳与随机值；`manifest_hash = sha256(规整 JSON)`。
- 技能文件哈希：L1 = 整份 `SKILL.md`；L2 = 卡片行之后正文段；L3 = 资源文件逐个哈希后按路径排序；定义见 `docs/contracts/agent-md.md`。
- ETag：原始 `AGENT.md` UTF-8 字节的 sha256，带双引号前缀 `sha256:`。

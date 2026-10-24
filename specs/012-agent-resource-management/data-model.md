# Phase 1 Data Model: US-12 Agent 与公共资源动态管理

**Date**: 2026-10-22 | **Spec**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md)

> **R4 范围修订（2026-10-23）**：schema 升 v3（26 张表）并新增迁移 `0003_credentials_scope.sql`，R3 的「零新增迁移 / schema v2」表述由 [plan.md](./plan.md) 的「R4 范围修订的技术计划」与模块卡 R4 取代。通知渠道仍保留 `?force=true`（字面 `true`）与同名 400 `BAD_REQUEST`（FR-059 / 约束 38）；R4 的 `allowlist` / 冻结集合 / 整词引用检查叠加在强删语义之前。以下为 R3 原文记录。

本 US **零新增迁移**：`internal/storage/migrations/` 仍只有 `0002_us11_us12_resources.sql`，`SupportedSchemaVersion` 保持 2（[migrate.go:21](../../internal/storage/migrate.go#L21)）。下表是「复用哪些列」的边界说明，不是新 DDL；表结构的唯一字面量来源是 [docs/contracts/schema.sql](../../docs/contracts/schema.sql)。

## 1. 持久实体（schema v2 复用）

### 1.1 `management_operations`（写审计与恢复）

| 列 | 本 US 用法 |
| --- | --- |
| `operation_id` | 主键，`op-<unix_millis>-<6 位小写字母数字>`；所有写端点的返回标识 |
| `action` | 契约动作字面量（见 §3） |
| `target_kind` / `target_id` | `agent` / `model_connection` / `mcp_server` / `skill` / `notify_channel` + 资源名 |
| `actor_subject` / `auth_method` / `source` | HTTP：`token:<指纹>` / `bearer` / `<ip>:<port>`；CLI：`cli:<user>` / `cli` / `cli`；启动对账：`local_trust` / `local_trust` / `startup` |
| `request_hash` / `expected_etag` | 条件写的期望 ETag 原值（审计用，不含正文） |
| `status` | `pending` → `success` / `failed` / `unknown` |
| `result_code` / `error_message` / `detail_json` | 固定错误码；失败原文截断 4 KB 且不含凭证；详情只放脱敏摘要 |
| `finished_at` / `recovered_at` / `verification` | 正常完成写 `finished_at`；恢复查证写 `recovered_at` + `applied` / `not_applied` / `indeterminate` |

**不变式**：一行 = 一次执行尝试；`pending` 必先于目标写入；`unknown` 只由恢复路径写，且不得自动补做。

### 1.2 `resource_versions`（只追加的版本账）

| 列 | 本 US 用法 |
| --- | --- |
| `id` | `version_id`，跨资源全局稳定；允许空洞（失败回滚可能消耗自增），只要求逐资源单调 |
| `resource_kind` / `resource_id` | `model_connection` / `mcp_server` / `skill` + 名称 |
| `version_no` | 每资源 1 起递增；同名删除后重建**继续递增** |
| `content_hash` | 发布内容的 sha256（小写十六进制）；资源 ETag 的来源 |
| `definition_yaml` | 模型连接 / MCP 的已发布块原文 |
| `manifest_json` / `storage_path` | 技能的完整清单 `[{path, sha256, size}]` 与不可变快照目录 |
| `operation_id` | 创建该版本的发布操作；「撤回来源版本」由该操作派生 |

**不变式**：只 `INSERT`；`UNIQUE(resource_kind, resource_id, version_no)`；`content_hash` 对存储字节求值。

### 1.3 `resource_pointers`（发布 / 应用指针）

| 列 | 本 US 用法 |
| --- | --- |
| `published_version_id` | 最近一次发布；撤回会指向**新**版本 |
| `applied_version_id` | 运行时实际生效；模型连接 / 技能发布后立即追平，MCP 需重启追平 |
| `deleted_at` / `updated_at` / `operation_id` | 删除置 `deleted_at`（保留行与历史）；每次状态变化更新 |

**派生**：MCP `pending_restart = published_version_id != applied_version_id`；`connected` 来自 supervisor 句柄快照而非本表。

### 1.4 `run_resource_manifests` / `run_resource_refs`（运行固定与引用）

| 表 | 本 US 用法 |
| --- | --- |
| `run_resource_manifests` | Run 准入时固化 Provider 版本、MCP 应用版本、技能版本与文件哈希、生命周期状态；本 US 不改格式，只在读引用时消费 |
| `run_resource_refs` | 版本级引用；`released_at IS NULL` 即活动引用，**是删除闸门的唯一活动运行判据** |

**不变式**：不得用「当前没有运行」推断无引用；引用释放由 Run 终态同事务写，崩溃遗留由启动恢复补齐。

### 1.5 `agent_lifecycle`（可重建索引）

| 列 | 本 US 用法 |
| --- | --- |
| `agent_name` / `state` | `active` / `disabled` / `archived`；`NULL` = 状态文件缺失或损坏 |
| `generation` | 状态文件中的单调代数；归档与恢复都递增 |
| `state_file_hash` / `valid` / `error_message` / `updated_at` / `operation_id` | 校验与可读原因；本 US 只经 US-11 的 `PutLifecycleIndex` 写 |

**不变式**：权威是工作区状态文件，本表可重建；本 US 不新增第二条写路径。

### 1.6 `agent_archives`（归档身份）

| 列 | 本 US 用法 |
| --- | --- |
| `archive_id` | 归档目录名 `<agent>-YYYYMMDD-HHMMSS`（UTC）；主键 |
| `agent_name` / `archived_at` / `content_hash` / `manifest_json` | 归档来源与清单校验；恢复前必须比对 |
| `operation_id` | 归档操作；与操作终态同事务 |
| `restored_at` / `restored_agent_name` | 恢复后写入；未恢复为空（列表过滤条件） |

**不变式**：`archive_id` 冲突 → 409，源目录原样保留，不得自动换名；归档行与操作终态同事务。

### 1.7 `notify_channels`（通知渠道，US-8 已建）

| 列 | 本 US 用法 |
| --- | --- |
| `name` | 主键；同名创建 400 |
| `type` | `webhook` / `feishu`；读写共用同一有序切片 |
| `url` | `${ENV_VAR}` 引用；允许空串（暂不可发送）；对外只回 scheme + host |
| `description` / `created_at` | 原样 |

**不变式**：本 US 新增写面（创建 / 更新 / 删除），不改表结构、不改发送语义。

## 2. 领域实体（`internal/core` 新增 / 扩展）

| 类型 | 字段 / 形态 | 归属 |
| --- | --- | --- |
| `AuthorConfig` | `Provider` / `Model` / `Temperature` | `core/author.go`（新增） |
| `AuthorFile` | `Path`（相对 Agent 目录、`/` 分隔）/ `Content` | `core/author.go` |
| `AuthorReferences` | `Providers` / `Skills` / `MCP` / `Template` | `core/author.go` |
| `AgentSpec` | `Name` / `Description` / `Model` | `core/agent_admin.go`（新增） |
| `AgentFileInfo` | `Path` / `Size` | `core/agent_admin.go` |
| `AgentArchiveRecord` | `ArchiveID` / `AgentName` / `ArchivedAt`（Unix 毫秒 UTC）/ `ContentHash` / `ManifestJSON` / `OperationID` / `RestoredAt *int64` / `RestoredAgentName` | `core/agent_admin.go` |
| `ResourceVersionDetail` | 版本元数据 + `DefinitionYAML` + `Manifest` | `core/resource_admin.go`（新增） |
| `PublishedMCPServer` | 已发布 MCP 配置 + `VersionID` / 已连接标识 | `core/mcp.go`（扩展） |
| `MCPServerConfig` | 追加 `Transport`（`stdio` / `http`）/ `Args []string` / `URL string`；既有 `Name` / `Command` / `Env` 语义不变 | `core/mcp.go`（修改） |

`ParseResourceKind` 只接受三个冻结字面量；`ParseAuthorDraft` 只接受带 `path` / `content` 的对象数组，拒绝绝对路径、`..`、空 `AGENT.md`、非 UTF-8。

## 3. 固定字面量（验收语义，逐字保真）

| 类别 | 取值 |
| --- | --- |
| 动作 | `agent.create`、`agent.files.update`、`agent.generate_files`（**无操作行**）、`agent.archive`、`agent.copy`、`agent.restore`、`model_connection.create/update/delete/publish/test`、`mcp_server.create/update/delete/publish/test`、`skill.create/import/update/publish/delete`、`notify_channel.create/update/delete` |
| 目标类型 | `agent`、`model_connection`、`mcp_server`、`skill`、`notify_channel` |
| 操作标识 | `op-<unix_millis>-<6 位小写字母数字>` |
| ETag | `"sha256:<64 位小写十六进制>"`，强校验；`*`、弱、逗号列表 → 412 |
| 归档标识 | `<agent>-YYYYMMDD-HHMMSS`（UTC） |
| 错误码 | 400 `BAD_REQUEST` / `EDIT_UNSUPPORTED` / `IMPORT_REJECTED`；409 `CONFLICT` / `AGENT_BUSY` / `AGENT_UNAVAILABLE`；412 `PRECONDITION_FAILED`；503 `NOT_CONFIGURED`；504 `TIMEOUT`（其余复用既有） |
| 生命周期 | `active` / `disabled` / `archived` |
| 操作状态 | `pending` / `success` / `failed` / `unknown`；恢复查证 `applied` / `not_applied` / `indeterminate` |

## 4. 状态迁移

### 4.1 Agent 生命周期

```text
(不存在) --create--> disabled (generation=1)
disabled --enable--> active        active --disable--> disabled
active|disabled --archive--> archived（目录移入归档区，生成 agent_archives 行）
archived --restore(原名)--> disabled（状态文件 archived → disabled，generation +1）
archived --restore(异名)--> disabled（新目录 Bootstrap，generation=1；原归档状态文件保持 archived）
```

约束：所有迁移只经 `LifecyclePort` / `workspace.LifecycleStore`；复制与异名恢复走 `Bootstrap`；**不得**在归档等待失败时留下 `archived` 状态。

### 4.2 MCP 运行时

```text
published_version_id = N, applied_version_id = M
M != N  → pending_restart=true, connected=false
重启连接成功 → applied_version_id = N, connected=true, pending_restart=false
连接失败 → applied_version_id 保持 M, connected=false（旧版本继续服务）
```

Agent 引用 MCP 必须满足 `applied == published` 且 `connected`，否则不注册（错误点名）。

### 4.3 管理操作与恢复

```text
pending --成功--> success
pending --业务失败--> failed
pending --崩溃--> (启动恢复) --applied--> success
                              --not_applied--> failed
                              --indeterminate--> unknown
```

恢复**只查证不重做**；`unknown` 在视图里显示为证据缺口，不自动补执行。

### 4.4 删除与重建

```text
发布中版本 v1..vN --delete--> pointers.deleted_at != NULL（版本保留）
同名 create --> vN+1（version_no 继续递增，不重置）
```

### 4.5 运行引用

```text
Run 准入 --> run_resource_refs(released_at=NULL) --> Run 终态（同事务）released_at=now
崩溃遗留 --> 启动恢复按终态 Run 补齐 released_at
```

## 5. 校验规则（与服务层一一对应）

| 规则 | 触发 | 结果 |
| --- | --- | --- |
| Agent 名 | 含 `/`、`..`、空、超长、非法字符 | 400 `BAD_REQUEST` |
| 写面路径 | 绝对路径、`..`、NUL / 控制字符、staging / backup 后缀、大小写重复、非 UTF-8 | 400 `BAD_REQUEST` |
| 请求体 | 超过 `http.max_request_bytes` | 400 `BAD_REQUEST` |
| 技能导入 | 越界、软链接、非普通文件、非 UTF-8、单文件 >256 KiB、总量 >`min(2 MiB, http.max_request_bytes)`、>200 文件 | 400 `IMPORT_REJECTED` |
| 工作区读取 | 单文件 >256 KiB | 400（不截断） |
| Provider 候选 | 缺必填、重名、`base_url` 非 http/https 或含 userinfo、`api_key` 非 `${VAR}` | 400 `BAD_REQUEST` |
| MCP 候选 | stdio 缺 `command` 或多词 command；http 缺 `url` 或 scheme 非法；`env` 非精确占位 | 400 `BAD_REQUEST` |
| 条件写 | 缺 `If-Match`、弱 ETag、`*`、不匹配 | 412 `PRECONDITION_FAILED` |
| 同名 | Agent / 通知渠道 / 技能 / 模型连接 / MCP 创建 | 400 `BAD_REQUEST`（Agent / 通知渠道）；409 `CONFLICT`（技能 / 模型连接 / MCP） |
| 删除闸门 | 被 Agent 或活动 Run 引用 | 400 `BAD_REQUEST` + 点名数组 |
| 归档 | 目标归档标识已存在 | 409 `CONFLICT` |
| 归档等待 | 30 秒内未退出 | 409 `AGENT_BUSY` + `run_ids` |
| 测试端点 | 15 秒未完成 | 504 `TIMEOUT` |
| 作者模型/通知发送 | 未配置 | 503 `NOT_CONFIGURED` |

## 6. 派生视图

| 视图 | 来源 | 脱敏 |
| --- | --- | --- |
| 资源版本列表 | `resource_versions` 按 `version_no DESC` | 无凭证 |
| 资源版本详情 | 版本行 + `definition_yaml` / `manifest` | 凭证值非 `${VAR}` → 500 固定文案，不返回部分内容 |
| 引用视图 | 注册表 + 归档扫描（Agent）+ `run_resource_refs(released_at IS NULL)`（Run） | 只回名字 / run_id |
| 归档列表 | `agent_archives WHERE restored_at IS NULL` | 无 |
| 工作区树 / 文件 | 真实路径投影，跳过 `.staging` / `.backup` | 符号链接只报类型；不跟随 |
| MCP 视图 | supervisor 快照 + 指针 | 只回可执行名 / 环境变量**键名**；错误只含固定短语与 scheme + host |
| 通知渠道列表 | `notify_channels` | `url_host` 只含 scheme + host |

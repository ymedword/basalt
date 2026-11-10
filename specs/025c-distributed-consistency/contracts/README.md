# Contracts: 已发布资产自动分发与跨进程一致性（US-25C）

**口径**：数据结构与 API 的固定字面量以 `docs/contracts/` 为准；本文件只登记本 US 的增量与「不改」项，供 analyze 与契约测试对号。

## 1. `docs/contracts/run-and-errors.md`

| 变更 | 字面量 | 说明 |
| --- | --- | --- |
| 新增错误码 | `POLICY_STALE`（503） | 策略过期或最近一次核对失败时的准入拒绝；既有 503 行之后追加 |
| 复用（不改语义） | `ASSET_UNAVAILABLE`（503） | 指针 / 本地目录缺失；**领取记录或执行租约不符**（`ErrClaimPinInvalid`）也归此码 |
| 复用（不改语义） | `ASSET_VERSION_MISMATCH`（409） | 普通 / 手动 Run 的已安装哈希与指针不符 |
| 复用（不改语义） | `RESOURCE_PROVIDER_RUNTIME` | 资源准备失败 |
| 明确不变 | — | **不新增**资产类错误码；钉住路径不引入新码 |

## 2. `docs/contracts/openapi.yaml`

| 变更 | 位置 | 固定字面量 |
| --- | --- | --- |
| 技能发布响应追加 | 资源发布 / 技能发布响应 schema | `publish: [{agent, status, error}]`（`omitempty`）；`status ∈ {ok, skipped, superseded, missing, failed}` |
| 概览 Worker 视图追加 | `SystemOverview.workers` | `as_of`（数据库时间）、`counts{active, stale, stopped}`（全库聚合）、`total`、`truncated` |
| 概览 Worker 行追加 | 同上，逐行 | `name`、`hostname`、`build_version`、`schema_version`、`stopped_at`（`omitempty`）、`heartbeat_age_seconds`、`heartbeat_interval_seconds`（旧行 `omitempty`）、`stale_after_seconds`、`status`（枚举 `active` / `stale` / `stopped`） |
| 描述 | `SystemOverview.workers.description` | 必须写明「统计对象是进程实例，不是服务器」；`active_leases` 是全库计数、不等于各行之和 |
| 明确不变 | — | 既有字段语义不改；**不新增** HTTP 端点（`assets status` 是 CLI） |

契约测试：`internal/web/contract_test.go` 的 `TestOpenAPI_AdminWriteViewsMatchContract` 必须覆盖上述新增字段；出现未登记字段或未在 OpenAPI 声明的字段即失败。

## 3. `docs/contracts/schema.pg.sql`

**不改写**（D-25C-02）。新表与列只出现在 `internal/storage/postgres/migrations/0013_agent_bundle_payloads.sql`：

- 建 `agent_bundle_payloads`（列与索引见 [data-model.md](../data-model.md) §1.1）。
- `worker_nodes.heartbeat_interval_seconds`（可空 + 正数 CHECK）。
- `task_executions.pinned_bundle_hash`（可空）。
- `resource_pointers (resource_kind, updated_at, resource_id)` 索引；可选 `scheduled_tasks (next_fire_at, task_id)` 部分索引。
- 幂等（`IF NOT EXISTS`），头注释说明「v1 基线未回写、时间列由 Go 写毫秒值」。

契约测试：`TestMigration0013_Idempotent`、`TestMigration0013_Columns`；`schema.pg.sql` 与起点必须空 diff。

## 4. `docs/contracts/agent-md.md`

**不改**：技能绑定仍只由 `AGENT.md` frontmatter 的 `skills:` 声明，不新增 frontmatter 字段；技能物化只影响本机发布版本目录，不改变声明语义。

## 5. 配套文档（非契约，但同批交付）

| 文件 | 增量 |
| --- | --- |
| `docs/Operations.md` | §5.5 发布可见性口径补全；新增「已发布资产同步」运维小节（`assets status`、失败原因、恢复路径、**不要重导种子**） |
| `docs/TechnicalDesign.md` | 追加资产分发与技能物化小节（D-25C-01 / D-25C-07） |
| `docs/Compatibility.md` | US-25 边界 → US-25C 承接记录 |
| `README.md` / `README.zh-CN.md` | 架构描述改为 Compose 多 Worker + 已发布资产自动分发；Demo 指向部署实例；保留范围注 |
| `website/**` | 能力、范围注（列出未交付项）、导航、演示命令、双语图示（12 页不变） |

## 6. 明确不新增的对外概念

- 不新增 HTTP 管理端点、不做 Worker 管理页 / 远程重启按钮、不做拓扑页。
- 不新增 `assets` 触发命令；只加只读 `basalt assets status`。
- 不新增 `retain_payloads` 或其他载荷清理配置键。
- 不新增 frontmatter 字段、配置段或公共接口（模块卡列出的改造点除外）。

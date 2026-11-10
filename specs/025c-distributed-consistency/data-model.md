# Data Model: 已发布资产自动分发与跨进程一致性（US-25C）

**输入**：模块卡 §3.2 / §3.3 / §2.3.4 / §2.3.5 / §2.3.9；契约字面量以 `docs/contracts/` 与模块卡为准。

## 1. 数据库变更（迁移 0013，只追加）

### 1.1 新表 `agent_bundle_payloads`

| 列 | 类型 | 约束 / 说明 |
| --- | --- | --- |
| `bundle_hash` | `text` | `PRIMARY KEY`；与 `assets.Manifest.BundleHash` 同值；内容寻址 |
| `agent` | `text` | `NOT NULL`；Agent 名（诊断与按 Agent 清理） |
| `agent_uid` | `text` | `NOT NULL`；发布时 Agent UID |
| `format_version` | `integer` | `NOT NULL`；bundle 格式版本，拒绝跨格式解码 |
| `payload` | `bytea` | `NOT NULL`；确定性 tar |
| `payload_sha256` | `text` | `NOT NULL`；归档字节哈希（与目录内容哈希不同口径） |
| `size_bytes` | `bigint` | `NOT NULL`；写入时按 `max_bundle_bytes` 校验 |
| `created_at` | `bigint` | `NOT NULL`；Unix 毫秒，**由 Go 写入**，不设数据库 DEFAULT |

索引：`idx_agent_bundle_payloads_agent (agent, created_at DESC)`。

写入语义：`INSERT … ON CONFLICT (bundle_hash) DO NOTHING` 后 `SELECT` 复核 `payload_sha256`；不一致返回明确错误。**没有**清理 / 裁剪方法（D-25C-09）。

### 1.2 既有表增量（同一迁移，`ADD COLUMN IF NOT EXISTS` 幂等）

| 表 | 列 / 索引 | 类型与约束 | 说明 |
| --- | --- | --- | --- |
| `worker_nodes` | `heartbeat_interval_seconds` | `integer NULL`，`CHECK (heartbeat_interval_seconds IS NULL OR heartbeat_interval_seconds > 0)` | 实例登记时自报间隔；旧行 `NULL`；生产登记只收正整数，`0` 写 `NULL`，负数由应用层拒绝、CHECK 兜底 |
| `resource_pointers` | `idx_resource_pointers_kind_updated_id` | `(resource_kind, updated_at, resource_id)` | 支撑 `(updated_at, resource_id)` 键集增量 |
| `task_executions` | `pinned_bundle_hash` | `text NULL` | 领取事务写入的固定哈希，是「允许使用旧版本」的唯一凭证；`NULL` = 非钉住领取，准入走当前指针语义；不回填历史行 |
| `scheduled_tasks` | `idx_scheduled_tasks_next_fire_task`（可选） | `(next_fire_at, task_id)`，带 `next_fire_at IS NOT NULL` 谓词 | 支撑非 NULL 段扫描 |

版本与迁移：

- `SupportedSchemaVersion = 13`；`Open` 增加 `migrate bool`——control 执行迁移，worker 只校验版本（`OpenForWorker` 精确版本、不迁移）。
- 已发布迁移只追加；`docs/contracts/schema.pg.sql` **不改写**（D-25C-02），新表只在迁移 0013 中定义。
- 时间列一律由 Go 写毫秒值，不新增数据库 DEFAULT 先例。

## 2. 领域记录与端口（`internal/core`）

| 记录 / 端口 | 字段 / 签名 | 约束 |
| --- | --- | --- |
| `BundlePayloadRecord` | `BundleHash, Agent, AgentUID, FormatVersion, Payload, PayloadSHA256, SizeBytes, CreatedAt` | 写入后复核哈希 |
| `PublishedBundle` | `Agent, AgentUID, BundleHash, UpdatedAt` | `UpdatedAt` 是存储写入毫秒值 |
| `PublishedBundleCursor` | `UpdatedAt int64, ResourceID string` | 键集游标：`(updated_at, resource_id) > ($1,$2)`，严格前进 |
| `SkillVersionRecord` | `Name, VersionID, StoragePath, Files []FileHash` | 物化按此逐文件核对 |
| `AssetDistributionPort` | `ListPublishedBundlesAfter`、`ListPublishedBundlesPage`、`ReadBundlePayload`、`ReadSkillVersion` | 增量与全量分开；全量按 `resource_id` 排序 |
| `BundlePayloadStore` | `PutBundlePayload` | 内容寻址、幂等；无清理 |
| `PolicyDigestSource` | `RefreshFromStore(ctx) (changed, digest, err)` | 各策略服务实现；锁内应用并自取摘要 |
| `SchedulerOptions.AssetReady` | `func(ctx, agent) error`；nil = 不设门 | 就绪门 |
| `SchedulerOptions.DefinitionAuthority` | `bool` | 仅 control 为 true；false 时 `SetRegistered` / `ReconcileAgent` 失败关闭 |
| `Scheduler.ReplaceEntries` | `func(entries []AgentEntry) error` | `stateMu` 内整体替换，零 I/O；不清空扫描游标与边界 |
| `ClaimDueTriggerPinned` | `(ctx, taskID, plannedAt, nextFireAt, owner, ttl, expectHash) (DueExecution, error)` | 领取事务内锁指针行；失败零写入；成功写 `pinned_bundle_hash` |
| `DueExecution`（追加） | `PinnedBundleHash string` | 既有 `ExecutionID` / `RunID` / `Lease` 不变 |
| `ClaimedBundlePin` | `ExecutionID, BundleHash, Lease` | 只由调度通道填；web / cli / admin 为 nil |
| `ClaimedPinRecord` | `ExecutionID, RunID, Agent, Status, BundleHash, OwnerWorkerID, OwnershipEpoch` | `Agent` 经 `task_executions.task_id` JOIN `scheduled_tasks.agent_name` |
| `ReadClaimedPin` | `(ctx, executionID) (ClaimedPinRecord, bool, error)` | `(零值, false, nil)` = 查无此行 |
| `ErrClaimPinInvalid` | 哨兵错误 | 领取记录或租约任一不符；映射 503 `ASSET_UNAVAILABLE` |
| `SweepBounds` | `Cutoff time.Time, NullUpperTaskID string` | 每圈冻结一次；空串 = 本圈无 NULL 行 |
| `DueScanCursor` | `Sweep, InNullSegment, NullAfterTaskID, AfterNextFireAt *time.Time, AfterTaskID` | 只有 `Start` 清空 |
| `WorkerOverview` | `AsOf, Counts{Active,Stale,Stopped}, Total, Truncated, Nodes` | 单条 SQL 同快照返回 |
| `WorkerNodeStatus` | 内嵌 `WorkerNodeView` + `HeartbeatAgeSeconds`, `HeartbeatIntervalSeconds *int`, `Status` | nil = 迁移前旧行；`Status ∈ {active, stale, stopped}` |
| `WorkerIdentity`（追加） | `HeartbeatIntervalSeconds int` | `0` → 列写 `NULL` |

## 3. 状态机

### 3.1 资产同步四态

```text
pending ──安装成功──> installed ──物化 + Entry 构建 + 注册成功──> ready
   │                     │                                        │
   │                     ├──物化 / 构建失败──> blocked（有界退避，下一轮重试）
   └──失败（载荷缺失 / 哈希不符 / 超限）──> blocked（原因可见）
ready ──指针变化 / 注册刷新失败──> installed / blocked（重新进入处理）
```

- 每轮对 `installed` 无条件重做技能物化与 Entry 构建；`installed ≠ ready`。
- 失败项留在工作集，按 `attemptSeq` 退到队尾；退避 `min(2^(failCount-1) × sync_interval, 5m)`，`failCount` 封顶 6。
- 全量扫描整趟完成且回读确认后才移除缺席项；中断不删任何项。

### 3.2 发布与准入顺序

```text
publish:   写载荷 ──成功──> 指针 CAS ──成功──> 发布可见
                └──失败──> 操作失败可见，指针不变

claim:     就绪门 ──就绪──> 事务：锁任务行 → 锁指针行 FOR SHARE
                │                    ├─ 指针缺失 / 删除 ──> ErrAssetNotReady（零写入）
                │                    ├─ 哈希不符 ──> ErrAssetMoved（零写入）
                │                    └─ 相符 ──> 写 started 执行行 + pinned_bundle_hash + 租约
                └─ 未就绪 ──> 不领取、不推进、不写执行行

admission: StartRun 同事务：生命周期行 → 执行租约 FOR SHARE → 执行行普通读
                ├─ 租约 owner / epoch / 未释放 / 未过期 不符 ──> ErrClaimPinInvalid（503，零写入）
                ├─ 执行行 status / run_id / Agent / 哈希 不符 ──> ErrClaimPinInvalid
                └─ 相符 ──> 用固定 bundle 的 Entry / 定义 / 技能建 Run
```

## 4. 配置键

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `assets.sync_interval` | `30s` | 发现检查频率；`0` = 仅启动时同步一次；**不是**就绪时限 |
| `assets.sync_timeout` | `20s` | 单轮同步总超时 |
| `assets.sync_batch` | `50` | 单轮尝试上限（按 `attemptSeq` 轮转） |
| `assets.max_bundle_bytes` | `16777216` | 载荷上限 |
| `assets.full_scan_every` | `20` | 每 N 轮发起一次全量兜底；`1` = 每轮 |
| `policy.sync_interval` | `30s` | 策略刷新周期 |
| `policy.max_stale` | `300s` | 超过即准入拒绝（`POLICY_STALE`，503） |

不新增 `retain_payloads`、概览阈值、技能修复开关等配置键；技能修复批量 5 / 退避上限 5m / 连续失败 8 次与概览上限 200 均为常量。

## 5. 验证规则

- 载荷：`size_bytes <= max_bundle_bytes`；`payload_sha256` 与写入字节一致；解码后复算 bundle 哈希等于 `wantHash`；拒绝绝对路径、`..`、符号链接、非普通文件、超限。
- 迁移：幂等（重复执行不报错）；版本 13；列可空性、CHECK 与索引与上表一致；`schema.pg.sql` 无 diff。
- 心跳间隔：生产登记只收正整数；`0` → `NULL`；负数被 CHECK 拒绝；旧行 `NULL` 在概览标注「阈值取部署默认」。
- 概览：计数 = 全库聚合；`total` = 三类计数之和；`truncated = total > len(nodes)`；明细上限 200；状态只读。
- 策略：摘要取自实际应用的行；删除 / 同内容不同版本 / 同毫秒变化可检出；读失败才推进过期。

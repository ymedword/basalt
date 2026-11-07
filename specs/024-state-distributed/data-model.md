# US24 数据模型

三张新表（`internal/storage/postgres/migrations/0012_distributed_execution.sql`）+ 四组既有表列扩展 + 两个 `resource_kind` CHECK 闭集扩展；schema 版本 `11 → 12`。时间列一律 bigint Unix 毫秒（UTC），与既有谱系一致；**所有时间判定使用数据库时间**，SQL 侧以 `(extract(epoch from clock_timestamp())*1000)::bigint` 读出 / 比较，不依赖应用进程时钟。
`docs/contracts/schema.pg.sql` 是冻结基线，**不改**；新结构只存在于追加迁移。

## 新表

### `worker_nodes`（进程身份登记，心跳只作诊断）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `instance_id` | text | 主键；进程级唯一（随机 128 bit） |
| `worker_name` | text | 非空；配置或 hostname，**允许重复**，仅诊断 |
| `hostname` | text | 非空 |
| `build_version` | text | 非空 |
| `schema_version` | integer | 非空；进程实际装载的 schema 版本，只在注册时写入 |
| `started_at` | bigint | 非空 |
| `last_heartbeat_at` | bigint | 非空 |
| `stopped_at` | bigint | 可空 |

索引：`idx_worker_nodes_heartbeat (last_heartbeat_at)`。心跳**不构成占有权证明**；存活判定只用租约过期。

### `execution_leases`（执行权唯一权威）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `kind` | text | CHECK `IN ('run','schedule_execution','flow_root')`；未知值 fail-closed |
| `resource_id` | text | 见表末编码契约 |
| `owner_worker_id` | text | 领取者实例标识；`legacy` 不是合法 Worker |
| `ownership_epoch` | bigint | CHECK `>= 0`；每次接管 +1；`0` = 迁移前遗留 |
| `acquired_at` | bigint | 非空 |
| `renewed_at` | bigint | 非空 |
| `expires_at` | bigint | 非空；到期只表示可被接管，不表示已被接管 |
| `released_at` | bigint | 可空；主动释放 |

主键 `(kind, resource_id)`；索引 `idx_execution_leases_expiry (expires_at)`——**不带部分谓词**：回收的两支谓词是"已到期（未释放）"或"已释放但业务未终态"，带 `WHERE released_at IS NULL` 只能覆盖前一支。

**租约生命周期**：

```text
（无行）──Acquire：插入 owner=caller, epoch=1 ──▶ 有效（TookOver=false）
有效 ──Renew（owner+epoch 匹配且未过期）──▶ 有效（renewed_at 前进，epoch 不变）
有效 ──expires_at 过去（无人续约）──▶ 过期（行仍在，可被领取）
有效 ──Release──▶ 已释放（released_at 写入，expires_at 保持）
过期 / 已释放 ──Acquire──▶ 有效（owner=新, epoch+1，TookOver=true）
过期 / 已释放 ──回收领取──▶ 业务收尾 → 行保持可再领取 / 由收尾后释放
```

`Renew` 必须是行级条件写（`UPDATE … WHERE kind/resource_id/owner/epoch AND expires_at > db_now`），命中 0 行 → `ErrLeaseLost`；不得退化为"先读后判"。

### `lease_events`（租约状态转换记录，运维面）

| 列 | 类型 | 约束 / 语义 |
| --- | --- | --- |
| `event_id` | bigint identity | 主键 |
| `kind` | text | 非空 |
| `resource_id` | text | 非空 |
| `event` | text | CHECK `IN ('claimed','takeover','released','renew_failed','reclaimed')` |
| `owner_worker_id` | text | 非空 |
| `ownership_epoch` | bigint | 非空 |
| `previous_owner_worker_id` | text | 可空 |
| `previous_epoch` | bigint | 可空 |
| `detail` | text | 非空，默认 `''` |
| `occurred_at` | bigint | 非空 |

索引 `idx_lease_events_resource (kind, resource_id, occurred_at DESC)`。**不自动清理**；保留运维手工 SQL（Q19）。它**不是**审计结论——审计权威仍是既有 `llm_calls` / 管理操作表。

## 既有表扩展

### `scheduled_tasks`

- 新增 `next_fire_at bigint`（可空 = 未初始化）。注册 / 推进时计算；领取用 CAS 推进。
- 索引 `idx_scheduled_tasks_due (next_fire_at) WHERE enabled AND retired_at IS NULL`。

### `task_executions`

- 新增 `planned_at bigint`（manual 触发为 NULL）、`owner_worker_id text`、`ownership_epoch bigint`。
- `ux_task_executions_cron_planned (task_id, planned_at) WHERE trigger = 'cron' AND planned_at IS NOT NULL`——一次计划触发只有一条执行。
- `ux_task_executions_active (task_id) WHERE status = 'started'`——同任务同时最多一条 `started`。字面量是 `started`（该列只有 `started/completed/failed/skipped`，没有 `running`；写错不报错、只让索引永远匹配不到）。
- `status` 域不变；回收写入只落四值：`limit_reached` → `completed`，`cancelled` → `failed`，无 Run → `failed` + 原因 `worker_lost`。

### `schedule_deliveries`

- 新增 `execution_id bigint`（指向 `task_executions.id`；`Prepare` 与 pending 行同事务写入；迁移遗留 / 历史已结算行为 NULL）。
- 索引 `idx_schedule_deliveries_execution (execution_id) WHERE status = 'pending'`。
- B2 对 NULL 退回按 `task_id` 判定；非 NULL 时必须确认 `execution_id` 对应的 `schedule_execution` 租约已失效才动。

### `runs`

- 新增 `cancel_requested boolean NOT NULL DEFAULT false`——跨 Worker 取消标志，由属主在续约响应 / 检查点观察。
- 索引 `idx_runs_caller_running (caller_id) WHERE status = 'running'`——调用方并发三段计数用。

### `resource_versions` / `resource_pointers`

- `resource_kind` 闭集由 `model_connection / mcp_server / skill` 扩到含 `agent_bundle`（两条 CHECK 都要改，只改列级一条会漏掉复合 CHECK）。
- 复合 CHECK 的 `skill` 支放宽为 `skill` 或 `agent_bundle`（都要求 `manifest_json` 与 `storage_path` 非空）。
- 用 `DROP CONSTRAINT IF EXISTS` + `ADD CONSTRAINT` 按定义替换；**约束名实施前必须在隔离 schema 按 `pg_constraint` 实测**，名字不符改用 `DO` 块按 `pg_get_constraintdef` 定位，不得保留静默空转。

## 回填（迁移 0012）

三类在途行回填 `owner_worker_id='legacy'`、`ownership_epoch=0`、`acquired_at = renewed_at = expires_at = 迁移时刻`（**立即过期**，不是 `now()+ttl`）：

| 类别 | 谓词 |
| --- | --- |
| Run | `runs.status = 'running'` → `kind='run'`, `resource_id=run_id` |
| 定时执行 | `task_executions.status = 'started'` → `kind='schedule_execution'`, `resource_id=id::text` |
| Flow 根 | `flow_tasks` 中 `status IN ('queued','running','blocked') AND cancel_requested = false AND close_intent = 'none' AND parent_task_id IS NULL AND root_task_id = task_id AND depth = 0` |

**不回填**：`cancelling` / `cancelled` / `abandoned` 等关闭态根、带关闭意图的根、已过 deadline 的根——它们由 `sweepClosures` 推进、不领租约，回填只会留下没人碰的死行。

所有回填 `ON CONFLICT (kind, resource_id) DO NOTHING`，迁移幂等。时间值用可执行 SQL `(extract(epoch from now())*1000)::bigint`（同一事务内恒定），**不得**改回 `$1`——迁移运行器以无参数 `ExecContext` 执行脚本。

## `resource_id` 编码契约

| kind | 取值 | 生成方式 |
| --- | --- | --- |
| `run` | `runs.run_id`（text） | 原样 |
| `schedule_execution` | `task_executions.id` 的十进制文本（无前导零 / 符号） | SQL `id::text`；Go `strconv.FormatInt(id, 10)` |
| `flow_root` | 根 `task_id`（text） | 原样 |

两处生成必须逐字相同，否则同一执行会出现两条租约；由 `TestLeaseResourceIDEncoding` 固定。

## 状态与判据

**执行行状态机**：

```text
（无行）──ClaimDueTrigger / StartExecution──▶ started（+ schedule_execution 租约）
started ──正常完成──▶ completed
started ──失败 / 取消──▶ failed
started ──租约过期，回收领取──▶ failed（原因 worker_lost）或 completed（Run 为 limit_reached）
（无行）──错失宽限──▶ skipped（原因 missed）/ skipped（原因 task_busy）
```

**投递结算（回收 B1 / B2）**：

```text
Run 已终态 且 pending ──▶ completed / limit_reached → unknown；failed / cancelled → skipped
Run 不存在 且 pending ──▶ skipped（仅当 execution_id 对应租约已失效；NULL 按 task_id 判）
任何分支都不自动重发
```

**Flow 根三判据（读面，不写租约）**：

| 判据 | 为真含义 | 只放行 |
| --- | --- | --- |
| `HasLiveRunLease` | 子树下仍有有效 run 租约 | 反面 `Isolated=true` 放行 `Recover`（并收敛 `prepared`/`running` → `interrupted`） |
| `HasInFlightNodeAttempt` | 子树下仍有 `prepared` / `running` attempt | 反面 `Joined=true` 放行 `Advance` / 复用已完成节点 |
| `HasUnresolvedAction` | 子树下存在 `outcome IN ('dispatching','unknown')` 动作 | 为真则**不得** `Advance`，必须 `Abandon`（不 `Release`） |

`interrupted` / `unknown` 是查证输入，**不**计入 `Joined`；`CloseState` 仍按 `Joined`，关闭类写入另需 `Isolated` 才能收敛 attempt。

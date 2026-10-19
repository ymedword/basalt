# Data Model: US-10 定时任务（Scheduler）

**Feature**: `010-scheduler` | **Date**: 2026-10-19 | **Plan**: [plan.md](./plan.md) | **Research**: [research.md](./research.md)

**零迁移声明**: 本模块**不新增表、列、索引，不修改 `schema.sql`**。`scheduled_tasks` / `task_executions` / `sessions` / `runs` 四张表均在既有 schema 中定义（[internal/storage/schema.sql](../../internal/storage/schema.sql) 与 [docs/contracts/schema.sql](../../docs/contracts/schema.sql) 当前零 diff）。本文件描述本模块对既有结构的**读写行为**与内存状态，不构成 DDL 变更。

---

## 1. 持久化实体

### 1.1 `scheduled_tasks`（任务定义快照 + 启停状态）

定义真相在 `AGENT.md` 的 `schedules:`，本表是装载后的**运行时快照**（含 `enabled` 与运行痕迹）。字段与 [internal/storage/schema.sql:181](../../internal/storage/schema.sql#L181) 逐字对应：

| 列 | 类型 | 本 US 行为 |
| --- | --- | --- |
| `task_id` | TEXT PK | 装载期规范化派生；首次成功写入的 `agent_name` 固定归属（约束 42）。写路径含 `ON CONFLICT` 归属守卫 |
| `agent_name` | TEXT NOT NULL | 所属 Agent（目录名）。跨 Agent 复用 id 一律拒绝（`ErrTaskOwnerConflict`） |
| `cron` | TEXT NOT NULL | 6 段表达式原文（`秒 分 时 日 月 周`） |
| `zone` | TEXT NOT NULL | IANA 时区；缺省 `UTC`。运行期以 `CRON_TZ=` 前缀交给引擎 |
| `message` | TEXT NOT NULL | 到点后作为用户消息交给 `AgentService.Run` |
| `enabled` | INTEGER NOT NULL DEFAULT 1 | **以库为准**；文件定义变化不翻转。`SetEnabled` 唯一写点 |
| `definition_hash` | TEXT NOT NULL | 规范化定义的哈希；装载与写入门共用同一算法，用于识别「定义未变」 |
| `last_run_at` | INTEGER（可空） | 在 `StartExecution` 成功后、`RunInvoker.Run` 之前写；锁跳过与预写失败不写（D-04） |
| `last_status` | TEXT（可空） | `MarkTaskRun` 置 `started`；`FinishExecution` / 启动恢复在同一事务内同步终态（含 `skipped`）；`RecordSkipped`（锁失）不触碰 |
| `retired_at` | INTEGER（可空） | 下线只置时间戳，**永不物理删除**。同 Agent 同 id 再现 → 清空并保留 `enabled` |
| `created_at` | INTEGER NOT NULL | 首次插入时间（`INSERT` 时写入） |
| `updated_at` | INTEGER NOT NULL | 每次 upsert / `SetEnabled` / `MarkTaskRun` / `FinishExecution` 更新 |

**任务生命周期**（`scheduled_tasks` 行级）：

```text
           ┌───────────────────────────── 同 Agent 定义再现（清 retired_at，保 enabled）
           ▼
      ┌─────────┐   文件定义消失 / Agent 目录删除     ┌──────────┐
      │ active  │ ────────────────────────────────▶ │ retired  │
      │retired_at│                                   │retired_at│
      │  = NULL │ ◀──────────────────────────────── │ 非空     │
      └─────────┘   同 Agent 同 id 重新出现           └──────────┘
           │                                              │
           │ 跨 Agent 复用同一 id                          │ 物理删除
           ▼                                              ▼
      ErrTaskOwnerConflict（整批回滚）             永不发生（历史保留）
```

**校验规则**（装载期与写入门共用 `validateSchedules`）：

- `cron` 必须为 6 段且可被引擎解析；5 段或非法 → 整个 Agent 不注册；
- `zone` 必须是合法 IANA 名；缺省 `UTC`；
- `message` 非空；
- id：恰好一项可省略并派生 `<agent>-1`；≥2 项必须每项显式；显式重复 → 该 Agent 不注册；
- 归属：命中其他 Agent 的 claim 或库内 `agent_name`（含 retired 行）→ 拒绝。

### 1.2 `task_executions`（执行历史）

字段与 [internal/storage/schema.sql:196](../../internal/storage/schema.sql#L196) 逐字对应：

| 列 | 类型 | 本 US 行为 |
| --- | --- | --- |
| `id` | INTEGER PK AUTOINCREMENT | 历史排序的稳定次序（同秒并发时 `id DESC` 兜底） |
| `task_id` | TEXT NOT NULL FK | 关联 `scheduled_tasks(task_id)`；只写已存在任务 |
| `run_id` | TEXT（可空） | 有对应 `runs` 行时写 RunID；**无 Run 的终态行一律 NULL**（会话忙 / 准入失败 / 重启恢复的遗留行；判据是存在性而非 `status`） |
| `status` | TEXT NOT NULL | 四值：`started` / `completed` / `failed` / `skipped` |
| `trigger` | TEXT NOT NULL | 两值：`cron` / `manual` |
| `duration_ms` | INTEGER（可空） | 终态写入毫秒差；`started` 行为空 |
| `error_message` | TEXT（可空） | 内部原因；对外只经三路投影（见 §3.2） |
| `started_at` | INTEGER NOT NULL | `StartExecution` 时写入，毫秒时间戳 |
| `finished_at` | INTEGER（可空） | 终态写入；`started` 行为空 |

**执行状态机**：

```text
                     ┌──────────── StartExecution（Run 之前）────────────┐
                     ▼                                                   │
                ┌─────────┐    Run 终态 completed / limit_reached   ┌───────────┐
   cron 触发 ──▶│ started │ ──────────────────────────────────────▶ │ completed │
   手动补跑 ──▶ └─────────┘                                          └───────────┘
                     │    Run 终态 failed / cancelled / missing
                     ├──────────────────────────────────────────────▶ ┌────────┐
                     │                                                 │ failed │
                     │    会话已归档（准入拒绝，无 Run）                 └────────┘
                     ├──────────────────────────────────────────────▶ ┌─────────┐
                     │    兄弟任务占会话（SessionBusy，无 Run）          │ skipped │
                     └──────────────────────────────────────────────▶ └─────────┘
                                                                           ▲
   锁失（TryLock 失败，cron）── RecordSkipped（独立行，无 Run）───────────┘
   锁失（TryLock 失败，manual）── 返回 ErrTaskBusy，**零执行行**

   启动恢复 RecoverStartedExecutions（崩溃遗留 started 行，约束 19 / FR-018）：
     Run completed / limit_reached → completed；failed / cancelled / 无 Run 行 → failed
     **恢复路径不产生 skipped**
```

**索引使用**：`idx_task_executions_task(task_id, started_at DESC)` 恰好支撑 `GET /schedules/{id}/executions` 的过滤与排序；固定取前 100 条，无分页。历史**不清理**（与「retired 永不物理删」一致）。

### 1.3 `runs`（既有运行账本，只读扩展语义）

`trigger` 列已存在（[schema.sql:84](../../internal/storage/schema.sql#L84)，注释即 `user / cron / manual / api`），本模块**不改表**，只决定写入值：

| 调用来源 | `channel` | `trigger` 写入 | 说明 |
| --- | --- | --- | --- |
| CLI / chat / tool | `cli` | `user` | 既有映射不变 |
| REST 同步调用 | `web` | `api` | 既有映射不变；`RunView.trigger` 取请求值，空回落 `api` |
| cron 到点 | `scheduler` | `cron` | `triggerFor` 显式分支 |
| 手动补跑 | `scheduler` | `manual` | 与 cron 共用会话，靠 `trigger` 区分 |

`explicit != ""` 时只接受 `channel == "scheduler"` 且值属于 `{cron, manual}`；其余组合拒绝落账（防止其他入口伪造调度触发）。`RunID`：调度器预生成 `scheduler-` 前缀并同时写入执行行与 `RunRequest.RunID`；非调度调用在 `RunID` 为空时沿用既有生成公式。

### 1.4 `sessions`（既有会话表，只读 + 归档）

调度会话标识固定：`DeriveSessionID("scheduler", "scheduler", agent)` → 一行 `channel = user_id = scheduler`、`profile_name = agent` 的会话。本模块不改表结构；读写经 `core.SessionStatusStore` 端口：

| 状态 | cron 到点 | 手动补跑 | 归档动作 |
| --- | --- | --- | --- |
| 不存在 | 正常创建并运行（`RequireExistingSession=false` 的调度请求由 `Run` 按既有逻辑创建） | 同左 | `Archive` 对不存在会话返回 `SessionNotFoundError` |
| `active` | 正常运行 | 正常运行 | 在途运行存在 → `SessionBusyError`（409）；否则置 `archived` |
| `archived` | 准入拒绝 → 执行行 `skipped` + 「会话已归档，本轮跳过」，**不重建会话** | 400 + 「会话已归档，不可追加消息」 | 幂等：已归档再归档返回成功 |

## 2. 文件实体：`AGENT.md` 的 `schedules:`

定义唯一来源。frontmatter 中 `schedules:` 为列表，每项：

| 字段 | 必填 | 语义 |
| --- | --- | --- |
| `id` | 条件必填 | 任务标识；单任务可省略（派生 `<agent>-1`），多任务必须显式且唯一 |
| `cron` | 是 | 6 段表达式（秒 分 时 日 月 周） |
| `zone` | 否 | IANA 时区，缺省 `UTC` |
| `message` | 是 | 到点发送的消息正文 |
| `enabled` | 否 | 初始启停；**仅首次插入生效**，之后以库为准 |

写入语义（`manage_schedule` / `AgentLoader.ApplySchedule`）：

- 只替换 `schedules:` 块，其余字节（注释、排版、其他 frontmatter 字段）保持原样；
- 候选内容先经与装载完全相同的 `parseFrontmatter` + `validateSchedules`，失败零写入；
- 临时文件 `O_CREATE|O_EXCL|O_WRONLY` + `os.Root.Rename` 原子替换；`writeMu` 串行化；
- 写后刷新私有 claim 基线；`serve` / `gateway` 下由工具同步调用 `ReconcileAgent`，`chat` / `tool` 下只写盘、下次启动生效。

## 3. 传输实体

### 3.1 `ScheduleView`（`GET /schedules` 元素）

与 [docs/contracts/openapi.yaml:1081](../../docs/contracts/openapi.yaml#L1081) 逐字一致：

| 字段 | 类型 | 来源 / 规则 |
| --- | --- | --- |
| `id` | string | `scheduled_tasks.task_id` |
| `agent` | string | `agent_name` |
| `cron` | string | 库内快照 |
| `zone` | string | 库内快照（缺省 `UTC`） |
| `message` | string | 库内快照 |
| `enabled` | boolean | 库内 `enabled != 0` |
| `last_run_at` | string | 毫秒时间戳 → RFC3339；零值渲染 `""`（US-9 惯例，`oneOf [date-time, const ""]`） |
| `last_status` | string | 库内 `last_status`；零值 `""` |

列表规则：只返回「未 retired 且所属 Agent 已注册」的任务，`task_id` 升序；无分页。

### 3.2 `ExecutionView`（`GET /schedules/{id}/executions` 元素）

与 [docs/contracts/openapi.yaml:1093](../../docs/contracts/openapi.yaml#L1093) 逐字一致：

| 字段 | 类型 | 来源 / 规则 |
| --- | --- | --- |
| `task_id` | string | 执行行 |
| `run_id` | string | 有对应 Run 时为 RunID；无 Run 的终态行为 `""`（库内 NULL 不投影为 `null`） |
| `status` | string enum | `started` / `completed` / `failed` / `skipped` |
| `trigger` | string enum | `cron` / `manual` |
| `duration_ms` | integer | 终态毫秒差；未结束为 0（零值） |
| `error_message` | string | **三路投影**：`failed` → 固定文案「运行失败」；`skipped` 且原文**精确等于**「会话已归档，本轮跳过」→ 原样；其余 `skipped` → 「上一轮尚未结束，本轮跳过」；`started` / `completed` → 空串 |
| `started_at` | string date-time | 必填 |
| `finished_at` | string | 终态 → RFC3339；`started` → `""`（`oneOf [date-time, const ""]`） |

排序：`started_at DESC, id DESC`，固定 100 条，无分页。

### 3.3 `RunView.trigger`（既有响应字段）

取 `RunRequest.Trigger`；空值回落 `api`。因此调度触发的 `POST /schedules/{id}/run` 响应为 `trigger = manual`，定时触发不出现在 REST 响应中但可在 `runs` 表与执行历史中看到 `cron`。

## 4. 内存状态（`core.Scheduler`）

不持久化、进程重启即重建；字段形态只表达职责，签名以颗粒度文档 §2.3 伪代码为准。

| 状态 | 载体 | 生命周期 / 规则 |
| --- | --- | --- |
| 定义快照 | `definitions map[string][]core.Schedule` | 装载后深拷贝；只由 `ApplyAgentTasks` 成功提交后的切换更新 |
| 已注册 Agent | `registry map[string]bool` | 与句柄集合一致；Agent 整只失败时不置位 |
| 每任务锁 | `locks map[string]*sync.Mutex` | 按需创建，随任务保留；`TryLock` 不阻塞 |
| 注册代次 | `gens map[string]uint64` + `nextGen uint64` | 每次 Add / Stop 前自增；旧回调比对失败即返回 |
| 状态互斥 | `stateMu sync.RWMutex` | 保护定义快照、注册表、代次、启停位；锁序：`stateMu` → 每任务锁（运行路径不持 `stateMu` 执行 `Run`） |
| 启动位 | `started bool` | `Start` 成功置位；`Stop` 幂等、未启动为 no-op |
| 在途运行 | `sync.WaitGroup`（或等价计数） | `Stop` 等待上限 70s；超时后 5s 有界清理 |
| 注入时钟 | `Now func() time.Time` | 测试可替换；不读 `time.Local` |

**并发不变量**：

1. `fire(taskID, gen)` 在 `stateMu.RLock` 内先比代次、再取 `ListTasks` 快照；两个动作同一临界区，独立作用域 `defer RUnlock` 释放；
2. 句柄切换（`Add` / `Stop` / `ReconcileAgent`）先写库（单事务提交）后持写锁换句柄并自增代次；
3. 运行路径不持 `stateMu` 执行 `Run`（长运行会阻塞列表 / 启停），只持每任务锁；
4. `SetEnabled` 顺序：可见性检查 → 库写 → 引擎动作；幂等短路；失效句柄修复；
5. 任何路径都不排队；锁失即跳过（cron）或 409（manual）。

## 5. 数据量与保留口径

| 项 | 口径 | 依据 |
| --- | --- | --- |
| 执行历史查询 | 100 条 / 任务 / 次，`started_at DESC, id DESC` | §6.3 第 9 条 |
| 错过触发点枚举 | ≤ 1000 次，超出截断 + 一条 WARN | 约束 18 |
| 执行历史保留 | 永不清理（与任务行同寿命） | 约束 15 / 17 |
| 任务行删除 | 永不物理删除，只置 `retired_at` | 约束 15 |
| 单次运行时限 | 300s（代码常量） | §6.3 第 3 条 |
| 停机宽限 / 清理 | 70s / 5s（代码常量） | §6.3 第 3 条 |

# Research: US-10 定时任务（Scheduler）

**Feature**: `010-scheduler` | **Date**: 2026-10-19 | **Plan**: [plan.md](./plan.md)

本文件回答 plan 的 Technical Context 中的技术未知项。设计面已由颗粒度文档 [docs/us/us10-scheduler.md](../../docs/us/us10-scheduler.md)（R11，人工 review ① 通过）§2.2 约束 1~44 冻结；以下每条只记录「决定 / 理由 / 备选与取舍 / 来源」，不新造设计。

---

## D-01 cron 引擎落位：新建 `internal/adapters/cron`

**决定**: robfig/cron v3.0.1 封装进新包 `internal/adapters/cron`，暴露 `Engine` 类型（`Add` / `Remove` / `Start` / `Stop` / `Occurrences`）；`core` 只声明所需最小接口（`CronEngine` 形态），由其自身或消费方持有，不经 `core` import 该包。装配在 `internal/app` 的 `newRuntime` 完成。

**理由**: 宪法「架构约束」要求 `core` 只依赖标准库。技术方案 §7.1.2 的「cron 调度封装在 `core`」一句与宪法自相矛盾，2026-10-15 经用户确认以宪法为准（`module-dev` 配置区已固化该裁决）。`internal/adapters/` 正是第三条包纪律指定的第三方实现落位。

**备选与取舍**: ① 放 `internal/core` —— 违反宪法，否决；② 放 `internal/app` —— 装配层被迫承载实现细节，且 `web` / `tool` 若需触发点枚举会反向依赖 app，否决；③ 再包一层接口包 —— 本仓已有「接口定义在消费方」惯例，多一层无收益，否决。

**来源**: 宪法架构约束；[docs/TechnicalDesign.md](../../docs/TechnicalDesign.md) §7.1.2；颗粒度文档约束 5 / §3.1。

---

## D-02 调度器本体：`core.Scheduler` + 消费方端口

**决定**: `core/scheduler.go` 定义 `Scheduler`，构造参数为 `TaskStore`（持久化端口）、`RunInvoker`（唯一执行入口，签名对应 `AgentService.Run(ctx, RunRequest)` 所需最小面）、`CronEngine`（引擎端口）、`Logger`、`Now`。`core/task.go` 定义 `TaskRecord` / `ExecutionRecord` / `Trigger` 常量与 `TaskStore` 端口。

**理由**: 调度逻辑（锁、代次、状态机、对账）是纯业务规则，放 `core` 可被单元测试直接覆盖；持久化与第三方 cron 各自隔离。`RunInvoker` 让 `core` 不依赖 `app`，装配顺序由 `wire.go` 决定。

**备选与取舍**: ① 调度器放 `internal/app` —— `core` 的会话门禁测试无法复用，且违反「业务逻辑在 core」的既成结构，否决；② 调度器放 `internal/adapters/cron` —— 适配层承载业务，`core` 的锁与代次不可测，否决。

**来源**: 颗粒度文档约束 1 / 2 / 39；技术方案 §6.3；既有 `core.AgentService` 结构。

---

## D-03 运行不重叠：每任务一把 `sync.Mutex` + `TryLock`

**决定**: `Scheduler` 内 `locks map[string]*sync.Mutex`，运行前 `TryLock`；失败时 cron 触发记 `skipped`（不写 `run_id`、不改 `last_run_at` / `last_status`），手动触发返回 `core.ErrTaskBusy` → HTTP 409 `SESSION_BUSY` + 冻结文案，**零执行行**。不排队、不重试。

**理由**: 主循环是同步执行（宪法第七条），排队会让一次长运行无限堆积后续触发；跳过语义与「300s 运行上限」配套，是最小可判卷行为。

**备选与取舍**: ① 有界队列 —— 需新增容量配置键与丢弃策略，超出已审范围；② 不锁、依赖 `AgentService` 的会话互斥 —— 定时任务与手动补跑共用同一会话，`AgentService` 的在途登记只能覆盖「运行中」而不能表达「跳过记账」，且 cron 需要写 `skipped` 执行行，否决。

**来源**: 颗粒度文档约束 7 / 8 / 10、失败矩阵；技术方案 §6.3。

---

## D-04 旧回调防护：注册代次（generation）

**决定**: `Scheduler` 持 `gens map[string]uint64` 与全局 `nextGen uint64`；每次 `Add` / `Stop` 句柄前先 `gens[taskID] = nextGen++`。cron 回调统一为 `fire(taskID, gen)`：在 `stateMu.RLock` 内先比对 `gens[taskID] == gen`，不等则直接返回（零执行行、零 `Run`、零 `last_status` 变化）。比对与 `ListTasks` 快照在同一读锁临界区内完成，用独立作用域 `defer RUnlock` 释放。

**理由**: robfig/cron 的 `Remove` 不保证已派发回调不执行；仅靠「从 map 删除句柄」不能阻止旧回调使用新定义。代次是进程内单调计数，比回调身份比较（函数值不可比较）与全局代次（跨任务误伤）都更精确，且无需改动引擎。

**备选与取舍**: ① 引擎侧取消（context）—— robfig v3.0.1 的 `Job` 不接收 context，需自行包一层且仍无法阻止已进入 `startJob` 的回调，否决；② 双检锁 + 句柄比对 —— 句柄对象在 Stop 后仍被旧回调持有，无法区分代次，否决；③ 全局代次 —— 任一任务 `Add` 会误杀其他任务的在途回调，否决。

**来源**: 颗粒度文档约束 44；R10 人工裁决第 1 项（HIGH）。

---

## D-05 `Start` 两阶段：先对账落库，再注册句柄

**决定**: `Start` 先对全部 Agent 执行 `ApplyAgentTasks`（任一失败 → 整体失败，**零句柄注册**）；全部成功后统一注册句柄并启动引擎。启动失败为致命错误（`Run` 返回错误、进程 exit 1；若 `runCtx` 已取消则 exit 0）。运行期 `ReconcileAgent` 失败只影响该 Agent：返回聚合错误、记录 WARN，其他任务继续运行。

**理由**: 半注册状态会让「文件里有、库里没有」的任务到点执行并写外键失败；两阶段把「定义真相」与「调度真相」的切换点收敛到一次事务提交之后。启动失败致命是因为调度缺失属于产品能力缺失，不应静默降级。

**备选与取舍**: ① 逐 Agent 尽力而为 —— 部分任务静默不调度，排障成本高，否决；② 运行期失败也致命 —— 单个 Agent 的 `AGENT.md` 语法错误会让整个服务退出，与「失败隔离」冲突，否决。

**来源**: 颗粒度文档约束 16 / 19 / 41、失败矩阵、图 B；R10 人工裁决第 3 项（MEDIUM）。

---

## D-06 `ApplyAgentTasks` 单事务 + 归属守卫

**决定**: 一次事务内对同 Agent 的全部定义执行 `INSERT ... ON CONFLICT(task_id) DO UPDATE SET ... WHERE scheduled_tasks.agent_name = excluded.agent_name`，随后把该 Agent 不再出现的任务 `UPDATE ... SET retired_at = ?`（仅当 `agent_name` 匹配）。语句前先做 owner 预检：`SELECT task_id, agent_name` 命中不属于本 Agent 的 id → `ErrTaskOwnerConflict`，整批回滚。`RowsAffected() == 0` 同样视为冲突。

**理由**: 「upsert + 退休」必须原子，否则崩溃窗口会留下「新定义已写入、旧任务未退休」的双胞胎；`WHERE` 子句把归属守卫下沉到 SQL，避免 TOCTOU。`RowsAffected() == 0` 是 `WHERE` 未命中时的唯一可判信号。

**备选与取舍**: ① 先查后写两条语句 —— 并发写会穿透预检，否决；② 用应用层比较时间戳 —— 需要额外列，超出零迁移约束，否决；③ 分事务逐条写 —— 失败后无法回滚整批，否决。

**来源**: 颗粒度文档约束 17 / 41 / 42；`docs/contracts/schema.sql` 两表定义。

---

## D-07 会话门禁下沉 `core.AgentService`

**决定**: `core.SessionStatusStore` 端口（`GetSession` / `ArchiveSession`）由 `storage` 实现；`ServiceOptions.Sessions` 注入；`AgentService.Run` 在 `mu` 内判定：`RequireExistingSession` 且会话不存在 → `SessionNotFoundError`，会话已归档 → `SessionArchivedError`；`Archive` 在同一把 `mu` 内检查在途运行（`running` 计数），有则 `SessionBusyError`。`SessionID == ""` 的请求完全绕过门禁与在途登记。`web.Server` 的 `admissionMu` / `inflightRuns` / `admitRun` / `releaseRun` 删除，归档 handler 改调 `RunService.Archive`。

**理由**: 门禁必须对**所有**运行入口生效；cron 直接调用 `AgentService.Run`，绕过 web 层，若判定留在 web 就会出现「已归档会话仍被定时任务追加消息」。同一把 `mu` 保证「检查在途 → 归档」与「检查归档 → 登记在途」互斥（同步屏障）。

**备选与取舍**: ① 调度器自行检查会话状态 —— 产生第二套判定，且 REST 与 cron 语义容易漂移，否决；② 两层各查一次 —— 重复锁与重复查询，且 web 层仍需在途登记，否决；③ 用数据库唯一约束 —— 归档与运行不在同一事务，无法表达在途，否决。

**来源**: 颗粒度文档约束 40 / 43、图 E；R10 人工裁决；US-9 既有 `SessionStore` 读面。

---

## D-08 运行请求与账本显式扩展

**决定**: `core.RunRequest` 追加 `RunID string`、`Trigger string`、`RequireExistingSession bool`（零值保持既有行为）；`core.RunRecord` 追加 `Trigger string`；`storage.triggerFor(channel, explicit string)`：`explicit != ""` 时只接受 `channel == "scheduler"` 且值为 `cron` / `manual`，其余组合返回错误拒绝落账；`explicit == ""` 时保持既有四渠道映射。`runView` 取 `req.Trigger`，空则回落 `api`。

**理由**: 四个入口（CLI chat / tool、REST、cron、手动补跑）共享同一账本，触发来源必须显式而不从 channel 推断（`channel` 已有 `user` 含义）。三字段均为可选增量，不破坏既有调用方。

**备选与取舍**: ① 用 `channel` 推断 —— `scheduler` 既是会话身份又可能是触发来源，遇手动补跑无法区分，否决；② 新增独立表 —— 超出零迁移，否决；③ `RunID` 由 `AgentService` 无条件生成 —— 调度器需要 `scheduler-` 前缀以便审计串联，否决。

**来源**: 颗粒度文档约束 12 / 13 / 14 / 15 / 33；`docs/contracts/run-and-errors.md` §5 / §6。

---

## D-09 任务 id 派生只在加载期；`manage_schedule` 的 set 省略 id

**决定**: 任务 id 的派生**只发生在装载期规范化**：原始 `AGENT.md` 缺省 id 且该 Agent 恰有一项时，`workspace` 派生 `<agent>-1` 并**写回** `Profile.Schedules`；两项及以上每项必须显式；调度器注册只读 `Schedule.ID`，运行期不再计算。`manage_schedule` 的 `set` 动作省略 `id` 时，工具先用 `AgentLoader.ReadSchedules` 读回当前磁盘定义，以**编辑前的当前集合**判定：当前恰有一个任务 → 定位该任务的**真实 id**（可能是 `daily-report` 这类显式 id，不假定 `<agent>-1`）；当前为零个或两个及以上 → `invalid_input`。

**理由**: 运行期派生会让「先读后写」的定位与磁盘状态依赖时序；只在加载期派生保证 id 一旦存在就稳定，工具定位以磁盘为准。这也是 agent-md 契约 §5.6 已确认的行为。

**备选与取舍**: ① 工具自行按装载规则再派生一次 —— 需要复用 `workspace` 的派生逻辑，接口会从 `workspace` 泄漏到 `tool`，且「按当前集合定位」与「按装载规范化」是两条规则，混用会让唯一显式 id 的任务被拒，否决；② 省略 id 时新增任务 —— 与「set 是更新」语义冲突，否决。

**来源**: 颗粒度文档约束 6 / 30 / 31；`docs/contracts/agent-md.md` §3 / §5.6（已确认 diff）。

---

## D-10 写入唯一入口：`AgentLoader.ApplySchedule`

**决定**: `workspace/schedule_write.go` 提供 `ApplySchedule(agentName, action, ...)`。流程：`writeMu` 加锁 → `ReadSchedules` 重读磁盘 → 以候选内容调同一套 `validateSchedules` → 临时文件（`O_CREATE|O_EXCL|O_WRONLY`，目录内）→ `os.Root.Rename` 原子替换 → 成功后刷新私有 `claims` 基线。磁盘内容只替换 `schedules:` 块，其余字节保持原样。

**理由**: 单写入门保证「校验 / 写入 / 基线刷新」不可分割；临时文件 + 同目录 rename 是跨平台原子替换的最小方案；`os.Root` 用于防止符号链接逃逸（仓库既有路径纪律）。只替换块级内容避免重排用户注释与格式。

**备选与取舍**: ① 整文件重写 —— 会丢失 frontmatter 外的排版与注释，且 diff 噪声大，否决；② 直接 `os.WriteFile` —— 崩溃窗口会截断文件，否决；③ 先写后校验 —— 校验失败已污染磁盘，否决。

**来源**: 颗粒度文档约束 26~30；`docs/contracts/agent-md.md` §5.6；US-5 的 `os.Root` 惯例。

---

## D-11 归属基线：私有 `claims` 与磁盘预检

**决定**: `AgentLoader` 持 `claims map[string][]core.Schedule`（agent → 最近一次磁盘定义），每次 `LoadAll` / `ApplySchedule` 后刷新。`manage_schedule` 与 `ApplyAgentTasks` 在写盘 / 写库前都先做归属预检：候选任务 id 命中其他 Agent 的 claim → `ErrTaskOwnerConflict`（工具侧 `invalid_input`）。`claims` 不导出，不参与并发判定之外的计算。

**理由**: 磁盘与数据库都可能被外部编辑；归属判定必须有一份进程内基线，否则「文件被手工改过」时无法区分「本 Agent 合法更新」与「跨 Agent 抢占」。

**备选与取舍**: ① 只查数据库 —— 文件已改但库未同步的窗口漏判，否决；② 只查磁盘 —— 库中 retired 历史无法覆盖，否决；③ 公开 `Claims()` 查询方法 —— 扩大公共接口，无消费方，否决。

**来源**: 颗粒度文档约束 30 / 42；技术方案 §6.3 的任务归属规则。

---

## D-12 `manage_schedule` 作为第 10 件内置工具

**决定**: `tool/manageschedule.go` 实现 `Tool` 接口，`Name() == "manage_schedule"`；动作 `set` / `remove` / `list`；身份**只**取 `ToolEnv.AgentName`（模型无法指定 Agent）；`set` 参数含 `id?` / `cron` / `zone?` / `message` / `enabled?`；`remove` 参数 `id`；`list` 无参数。失败分类：参数或调度语法错误 → `invalid_input`；目标 id 不存在 → `not_found`。成功结果回填受影响任务的完整字段。

**理由**: 工具是 Agent 自管任务的唯一入口；身份来自执行环境而非模型参数，是防越权的关键。第 10 件工具进入 `declaredToolNames` 与 `Names()`，与既有九件享受同一注册、审计与错误链路。

**备选与取舍**: ① 由模型传 `agent` 参数 —— 可越权操作其他 Agent 的任务，否决；② 拆成 set/remove/list 三个工具 —— 工具数量与 schema 膨胀，且 `declaredToolNames` 判据面变大，否决；③ 仅暴露 `set` —— 无法删除任务，产品 Demo 不完整，否决。

**来源**: 颗粒度文档约束 25 / 31 / 32；`docs/contracts/agent-md.md` §5.6。

---

## D-13 执行状态与对外原因投影

**决定**: `task_executions.status` 四态：`started` / `completed` / `failed` / `skipped`。`StartExecution` 在 `Run` 之前写入（`started`），终态在返回后更新。`publicExecutionMessage` 三路投影：`failed` → 固定文案「运行失败」；`skipped` 且错误文案**精确等于**「会话已归档，本轮跳过」→ 原样返回；其余 `skipped` → 统一返回「上一轮尚未结束，本轮跳过」；`started` / `completed` → 空串。

**理由**: 执行记录必须能在运行前落库，否则崩溃会留下「有 Run 无执行行」；对外原因只允许三类固定文案（`failed` 的「运行失败」+ skipped 的两条冻结文案），避免内部错误细节泄漏。精确匹配而非包含匹配，防止把其他含相同子串的错误误投影。

**备选与取舍**: ① `skipped` 统一文案 —— 归档与锁失不可区分，排障困难，否决；② 直接透传 `error_message` —— 泄漏内部路径 / SQL 片段，否决；③ 前缀匹配 —— 错误信息拼接顺序变化会误判，否决。

**来源**: 颗粒度文档约束 9 / 10 / 34；`docs/contracts/openapi.yaml` 的 `ExecutionView` 枚举与描述。

---

## D-14 调度会话身份与 RunID 前缀

**决定**: 定时运行统一使用 `DeriveSessionID("scheduler", "scheduler", task.Agent)` 作为会话标识；`RunID` 以 `scheduler-` 前缀开头（由调度器生成，内部仍以 `NewRunID` 形态保证唯一）。手动补跑与 cron 触发共享同一会话。

**理由**: 同一 Agent 的全部定时运行落在一条固定会话内，历史可连续读取；`scheduler-` 前缀让审计与日志能按来源筛选。复用既有 `DeriveSessionID` 避免第二套标识算法。

**备选与取舍**: ① 每任务一个会话 —— 任务重新创建会换会话，历史断裂，否决；② 每触发一次新会话 —— 无法连续对话，且会话表膨胀，否决；③ 手动补跑使用 REST 既有会话 —— 与「补跑走同一链路」的验收冲突，否决。

**来源**: 颗粒度文档约束 12 / 13；`core/sessionid.go` 既有实现（[internal/core/sessionid.go:37](../../internal/core/sessionid.go#L37)）。

---

## D-15 历史窗口与错过触发点枚举上限

**决定**: `GET /schedules/{id}/executions` 返回最多 100 条，按 `started_at` 降序，无分页；重启对账时枚举「上次运行到当前」的错过触发点，上限 1000 次，超出截断并记一条 WARN（含任务 id 与实际错过数）。不补跑任何错过触发点。

**理由**: 固定窗口避免分页参数进入契约；枚举上限防止长时间停机后逐条列举造成启动卡顿。WARN 是停机期间错过触发的唯一可观测信号。

**备选与取舍**: ① 分页 —— 需新增 `limit` / `cursor` 契约字段，超出已确认 openapi diff，否决；② 逐条 WARN —— 日志风暴，否决；③ 完全静默 —— 运维无法得知错过数量，否决。

**来源**: 颗粒度文档约束 18 / 38；`docs/contracts/openapi.yaml` executions 描述。

---

## D-16 停机：70s 宽限 + 5s 有界收尾

**决定**: `Scheduler.Stop` 等待在途运行完成，上限 70s；超时后以 `context.WithoutCancel` 派生 5s 清理上下文，更新执行行终态为 `failed`（原因「停机超时」），然后返回。`heavyRuntime.Close` 顺序：调度器 → MCP → DB → 锁。

**理由**: 与 US-9 的 70s 停机宽限同口径；清理写库不能被已取消的请求上下文打断，因此剥离取消但保留值（追踪）。先停调度器确保没有新运行在 Close 后访问已关闭的 DB / 锁。

**备选与取舍**: ① 直接 `context.Background()` —— 丢失追踪标识，否决；② 不设上限 —— 进程退出被长运行无限阻塞，否决；③ 先关 DB —— 在途运行写终态失败，否决。

**来源**: 颗粒度文档约束 22 / 23；技术方案 §6.3 / §6.5 停机顺序。

---

## D-17 信号上下文只在 `app.Run` 派生一次

**决定**: 删除 `internal/cli/serve.go` 的两处 `signal.NotifyContext`；`app.Run` 用注入的 `notifier`（测试可替换）派生唯一 `runCtx`，向下传给 `newRuntime`、HTTP server 与调度器。`gateway` 只更新提示文案（「调度器已启动；通道族归后续 US」），不启动任何 IM 通道。

**理由**: 两个独立的 `NotifyContext` 会争抢同一个 SIGINT / SIGTERM，导致某一侧先退出而另一侧仍在运行；单一信号源保证「HTTP 关停、调度器停止、MCP 关闭、DB 关闭」按序执行。注入 notifier 让「信号 → 退出码」可测。

**备选与取舍**: ① 保留 cli 侧信号、app 复用 —— 生命周期分裂，否决；② 用 `os/signal` 全局钩子 —— 不可测，否决；③ gateway 启动占位通道 —— 超出 US-10 范围（US-11），否决。

**来源**: 颗粒度文档约束 20 / 24、图 G；US-9 的 `App` 生命周期。

---

## D-18 测试专用 jsonschema 校验器

**决定**: `internal/web/contract_test.go` 使用 `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`（Draft 2020-12），启用 `AssertFormat()`；只在 `*_test.go` 导入。`go.mod` 中仅新增该坐标，传递依赖 `golang.org/x/text v0.14.0` 以 `// indirect` 出现；生产包零导入，发布二进制零增量。

**理由**: `openapi.yaml` 的 `ScheduleView.last_run_at` / `ExecutionView.finished_at` 是 `oneOf [date-time, const ""]`，手写断言无法验证「格式合法」；官方兼容的 Draft 2020-12 校验器是唯一可靠手段。测试专用坐标已写入固定技术栈句并经人工 review ① 批准。

**备选与取舍**: ① 手写字符串格式检查 —— 无法覆盖 date-time 的完整语法，易假绿，否决；② 引入完整 OpenAPI 校验器 —— 依赖更重且不需要，否决；③ 只做结构断言 —— 无法证明 `format` 生效，否决。

**来源**: 颗粒度文档约束 35 / §4.4 `010-web-contract-schema`；`GOPROXY=https://goproxy.cn go list -m -versions github.com/santhosh-tekuri/jsonschema/v6` 实测（v6.0.3 为最新 GA）。

---

## D-19 注入时钟，杜绝真实 sleep

**决定**: `Scheduler` 与涉及时间判定的组件接受 `Now func() time.Time`（来自 `ServiceOptions.Now` 的同一形态）；测试用固定时钟推进。引擎的 `Occurrences` 枚举同样以注入时钟为基准。测试中不出现 `time.Sleep` 等待真实 cron 触发。

**理由**: cron 触发周期最短是秒级，真实等待会让测试不稳定且慢；注入时钟让「到点」「错过」「跨时区」三类行为可确定地驱动。

**备选与取舍**: ① 用真实时钟 + 长超时 —— 不稳定，否决；② 只 mock 引擎、不注入时钟 —— `last_run_at` / 错过枚举无法断言，否决。

**来源**: 颗粒度文档约束 37；`ServiceOptions.Now` 既有形态（[internal/core/service.go:57](../../internal/core/service.go#L57)）。

---

## D-20 零迁移：直接使用既有两表

**决定**: 不新增迁移、不改 `schema.sql`。`scheduled_tasks` / `task_executions` 及其索引按已存在定义直接读写；`task_executions.run_id` 对 skipped 行写 NULL。全部 SQL 只出现在 `internal/storage/task_store.go`。

**理由**: 两表自 US-1 起即在 schema 中定义，且 `docs/contracts/schema.sql` 与 `internal/storage/schema.sql` 当前零 diff；任何迁移都是范围膨胀。

**备选与取舍**: ① 新增 `last_error` 列 —— 契约 diff 未批准，否决；② 新增 `tasks` 视图 —— 无消费方，否决。

**来源**: [internal/storage/schema.sql:175-210](../../internal/storage/schema.sql#L175-L210)；颗粒度文档约束 35。

---

## D-21 四个端点：固定形态与错误映射

**决定**: `GET /schedules`（列表，`task_id` 升序）、`PUT /schedules/{id}`（`{enabled}`，200 / 400 / 404）、`GET /schedules/{id}/executions`（100 条、`started_at` 降序、404）、`POST /schedules/{id}/run`（200 `RunView` / 400 归档 / 404 / 409 在途 / 429 / 504）。`GET /schedules` 的**全部字段取自 `ListTasks` 的库行**（`enabled` / `last_*` 与 `cron` / `zone` / `message` 同源；运行期定义一律以库为准，不回读磁盘）；`retired_at` 非空或所属 Agent 未注册的任务不出现。手动补跑不写 `task_executions` 直到 `StartExecution` 成功。

**理由**: 端点与错误码已在 `docs/contracts/openapi.yaml` 冻结（人工 review ① 确认的 diff）；`enabled` 取库 / 定义取盘是「定义与状态分离」的直接体现。

**备选与取舍**: ① 定义也取库 —— 文件修改后需重启才可见，且违背「文件是定义唯一来源」，否决；② `POST /run` 返回 202 —— 契约固定 200，否决；③ 409 允许排队 —— 与 D-03 的跳过语义冲突，否决。

**来源**: [docs/contracts/openapi.yaml](../../docs/contracts/openapi.yaml) schedules 段（已确认 diff）；颗粒度文档约束 33、图 F。

---

## D-22 定义刷新时机：serve / gateway 立即生效，chat / tool 只写盘

**决定**: `serve` / `gateway` 模式下 `manage_schedule` 写盘后，工具同步调用 `Scheduler.ReconcileAgent`，返回前完成句柄切换；`chat` / `tool` 模式下没有调度器，工具只写盘，改动在下次启动时生效。该语义写进 `agent-md.md` §5.6 与 spec 澄清记录。

**理由**: 调度器只在长驻进程存在；CLI 单次命令中没有可刷新的句柄。让工具在无调度器时静默返回成功，是「定义与运行分离」的一致行为。

**备选与取舍**: ① chat 模式也启动调度器 —— 命令立即退出，句柄无意义，否决；② 无调度器时报错 —— 让 CLI 无法管理任务，否决。

**来源**: 颗粒度文档约束 20 / 31；clarify 记录（effect timing）。

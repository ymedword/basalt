# Tasks: US-10 定时任务（Scheduler）

**Input**: Design documents from `/specs/010-scheduler/`

**Prerequisites**: [plan.md](./plan.md)、[spec.md](./spec.md)、[research.md](./research.md)、[data-model.md](./data-model.md)、[quickstart.md](./quickstart.md)；唯一事实源 = [docs/us/us10-scheduler.md](../../docs/us/us10-scheduler.md) §3 / §4 / §6.3（R11，人工 review ① 2026-10-19 通过）

**Tests**: 本 US 的测试是交付物的一部分（颗粒度文档 §3.2 明列 254 例：新增 8 文件 227 例 + 修改 7 文件 27 例）。测试任务先于或伴随对应实现任务落位；关键回归**红态先行**——先看到 §3.2.1 括注的逐字预期失败，再改实现。

**Organization**: 按 spec 的四个用户故事分阶段（P1 到点自动运行 / P2 重启与停机一致性 / P3 任务管理与手动补跑 / P4 Agent 自管任务）；Phase 2 承载跨故事基础件（领域类型、存储、cron 适配、会话门禁、运行入口扩展）。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 不同文件、无未完成依赖，可并行
- **[Story]**: 所属用户故事（US1~US4）；Setup / Foundational / Polish 无标签
- 每条任务都带精确文件路径；括号内为该任务对应的专属 check ID

## Path Conventions

单 Go module：`internal/` 下的包；测试与被测包同目录。零新增顶层目录、零迁移、零新增配置键；唯一新增坐标是测试专用的 `github.com/santhosh-tekuri/jsonschema/v6`。

---

## Phase 1: Setup（共享准备）

**Purpose**: 基线核对、测试专用坐标与 stage 工件；不产业务代码。

- [x] T001 核对开工基线：`go version`（≥ `toolchain go1.27.2`）、`git branch --show-current` = `010-scheduler`、`GOPROXY=https://goproxy.cn`；复核 `docs/contracts/openapi.yaml`（sha256 `f1f40d1e…`）与 `docs/contracts/agent-md.md`（sha256 `7407c507…`）的 diff 仅含人工 review ① 已确认内容（schedules 四操作、`ScheduleView` / `ExecutionView`、run 的 400 / 429 / 504、PUT 400、时间字段 `oneOf`；`schedules:` 段与 §5.6 实际 id 定位；**不得**出现 `nullable`），读数写入 `specs/010-scheduler/run-state.md` 基线段
- [x] T002 [P] 落测试专用坐标：`GOPROXY=https://goproxy.cn go list -m -versions github.com/santhosh-tekuri/jsonschema/v6` 核实后，在 `go.mod` / `go.sum` 加入 `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3` 与 `go mod tidy` 实测的必要传递依赖（预期 `golang.org/x/text`）；复核 `git diff go.mod` 删除行零命中、新增行仅限这两项，`go list -deps ./cmd/basalt | grep santhosh-tekuri` 零命中，生产代码零导入（check: `010-web-contract-schema` 前置）
- [x] T003 [P] 派生 `harness/stages/us10-scheduler.json`：`inputs` = 颗粒度文档 / `spec.md` / `plan.md` / `tasks.md`；`protect` 按 [颗粒度文档 §4.4](../../docs/us/us10-scheduler.md) 的 stage 派生段逐项枚举（§3.1 既有生产文件、§3.2.2 七个既有测试文件、`internal/app/run_test.go`、8 个新增测试文件、两份契约、`go.mod` / `go.sum`、`harness/acceptance.json`、`Makefile`、11 个 `scripts/**` 具体文件）；`checks` = 13 条专属 check，argv / 超时 / `required_tests` 逐字照 §4.4；`tasks` 映射在 T043 填入

---

## Phase 2: Foundational（阻塞前置，所有用户故事依赖）

**Purpose**: 领域类型与端口、运行入口扩展、会话门禁、两表存储、cron 适配、装载写锁——四条故事共用的地基。

**⚠ CRITICAL**: 本阶段完成前不进入用户故事阶段。

- [x] T004 [P] 落 `internal/core/task.go`：`ExecutionStatus`（`started` / `completed` / `failed` / `skipped`）、`Trigger`（`cron` / `manual`）、`ScheduleAction`（`set` / `remove` / `list`）、`ErrTaskBusy`、`TaskRecord`、`ExecutionRecord`、`TaskStore`（9 方法，含 `ApplyAgentTasks`）、`RunInvoker`、`CronHandle`、`CronEngine`；签名逐字照 §3.1.1，`core` 只依赖标准库（check: `010-core-scheduler` / `010-storage-tasks` 前置）
- [x] T005 [P] 改 `internal/core/errors.go`：新增 `SessionArchivedError`、`SessionNotFoundError`、`SessionGateUnavailableError`、`ErrTaskOwnerConflict`；既有哨兵与错误类型一个不动，文案逐字照 §3.1.11（check: `010-core-sessiongate` 前置）
- [x] T006 [P] 改 `internal/core/run.go` 与 `internal/core/store.go`：`RunRequest` 追加 `RunID` / `Trigger` / `RequireExistingSession`，`RunRecord` 追加 `Trigger`；零值兼容（既有调用方不填 → 行为不变），逐字照 §3.1.9 / §3.1.10
- [x] T007 [P] 改 `internal/core/service_test.go`：新增 9 个函数 / 11 例——`TestAgentService_Run_UsesCallerRunID`、`TestAgentService_Run_DefaultRunIDUnchanged`、`TestAgentService_Run_PassesTriggerToLedger`、`TestAgentService_Archive_RejectsInflightRun`（2）、`TestAgentService_Archive_Idempotent`、`TestAgentService_Run_ArchivedSessionRejected`、`TestAgentService_Run_RequireExistingSession`（2）、`TestAgentService_StatelessRunsNotSerialized`、`TestAgentService_Archive_WithoutSessionStore`；红态先行（逆改点按颗粒度文档 §3.2.2 括注）——覆盖 R27 的 core 面（check: `010-core-sessiongate` / `010-core-runid`）
- [x] T008 改 `internal/core/service.go`：`SessionStatusStore` 端口与 `ServiceOptions.Sessions`；`sessions` 忙集更名 `busy`；`Run` 准入四步（忙集 → 实例满 → `GetSession` 归档 / 缺失判定 → 登记 `busy`）全部在同一 `s.mu` 内，空会话请求绕过会话门禁；新增 `Archive`（nil 门 → `*SessionGateUnavailableError`）；尊重调用方 `RunID`、`Trigger` 写入 `RunRecord`（依赖 T005 / T006 / T007）
- [x] T009 [P] 落 `internal/storage/task_store_test.go`：14 个函数 / 38 例——`TestApplyAgentTasks_UpsertPreservesState`（3）、`TestTaskStore_RetireAndRevive`（2）、`TestApplyAgentTasks_SameOwnerReviveAllowed`、`TestApplyAgentTasks_CrossOwnerRejected`、`TestApplyAgentTasks_CrossOwnerRetiredRejected`、`TestApplyAgentTasks_RollbackOnSecondWrite`（2，SQLite 触发器故障注入）、`TestTaskStore_SetTaskEnabled`（2）、`TestTaskStore_MarkTaskRun`、`TestTaskStore_StartExecutionAndFinish`（2）、`TestTaskStore_FinishExecution`（6）、`TestTaskStore_RecordSkipped`（2）、`TestTaskStore_RecoverStartedExecutions`（8）、`TestTaskStore_ListTasks`（4）、`TestTaskStore_ListExecutions`（3）；真实临时 SQLite，红态先行（check: `010-storage-tasks`）
- [x] T010 落 `internal/storage/task_store.go`：`TaskStore` 9 方法的全部 SQL；`ApplyAgentTasks` 单事务 + 归属谓词（`ON CONFLICT ... WHERE scheduled_tasks.agent_name = excluded.agent_name`，`RowsAffected()==0` → `ErrTaskOwnerConflict`）与 retire 归属预检；`FinishExecution` 以 `runs` 存在性置 NULL `run_id` 并同事务同步 `last_status`（含 `skipped`）；`RecoverStartedExecutions` 用 `sql.NullString` 左连接扫描、按 `started_at` 正序收尾；`ListTasks` / `ListExecutions` 可空列一律 `sql.Null*` 映射零值（依赖 T004 / T009）
- [x] T011 [P] 改 `internal/storage/run_store_test.go`：`TestRunStore_TriggerForScheduler` 5 例（cron / manual 接受；scheduler + 非法值拒绝；`web + manual`、`cli + cron` 拒绝）；红态先行（check: `010-storage-tasks`）
- [x] T012 改 `internal/storage/run_store.go`：`triggerFor(channel, explicit)` 显式值只接受 `channel == "scheduler"` + `cron` / `manual`，空值沿用既有四渠道映射（依赖 T011）
- [x] T013 [P] 落 `internal/adapters/cron/cron_test.go`：9 个函数 / 14 例——`TestEngine_RejectsFiveFieldSpec`、`TestEngine_RejectsMissingCRONTZ`、`TestEngine_ParsesSixFieldWithCRONTZ`、`TestEngine_OccurrencesFromFixedStart`（5，含三契约样例与春令时 / 秋令时独立窗口）、`TestEngine_OccurrencesCapsAtLimit`、`TestEngine_OccurrencesNoMoreTimes`、`TestEngine_AddAndStop`（2）、`TestEngine_StopWaitsForJobs`、`TestEngine_UnknownZoneFails`；固定向量、不得真实等待（check: `010-cron-adapter`）
- [x] T014 落 `internal/adapters/cron/cron.go`：`cron.NewParser(Second|Minute|Hour|Dom|Month|Dow)`；`Add` 拒绝 5 段与缺少 `CRON_TZ=` 前缀的 spec；`Start` / `Stop` / `Occurrences`（`limit` 上限、`to` 截止、解析失败返回空）；句柄化停止（依赖 T004 / T013）
- [x] T015 [P] 改 `internal/workspace/agent.go`：`AgentLoader` 追加 `writeMu sync.Mutex`，`LoadAll` 与后续 `ApplySchedule` 共用；其余装载逻辑零改动（check: `010-workspace-writegate` 前置）

**Checkpoint**: `go build ./...` 通过；`010-core-sessiongate` / `010-core-runid` / `010-cron-adapter` / `010-storage-tasks` 可跑。

---

## Phase 3: User Story 1 - 到点自动运行 (Priority: P1) 🎯 MVP

**Goal**: `AGENT.md` 声明的任务在 `serve` / `gateway` 下按 6 段 cron 到点触发，经同一运行入口执行；执行历史先 `started` 再按结果收尾；单任务互斥、错误被触发边界吸收。

**Independent Test**: `go test -run '^TestScheduler_(Fire_|LogFieldsAndNoBody)' ./internal/core` 全绿；`go test -run '^(TestNeedsSchedules|TestNewScheduler_|TestRun_Schedule|TestScheduleStartExitCode_|TestNewRuntime_ServiceHasSessionGate)' ./internal/app` 全绿；真实到点链路见 [quickstart.md](./quickstart.md) §4.2 场景 1。

### Tests for User Story 1

- [x] T016 [US1] 落 `internal/core/scheduler_test.go` 的 13 个函数 / 19 例——`TestScheduler_Fire_DerivesSessionAndRunID`（2）、`TestScheduler_Fire_ExecutesAndRecords`（2）、`TestScheduler_Fire_ReadsTaskFromStore`（3）、`TestScheduler_Fire_FailureMapping`（2）、`TestScheduler_Fire_AdmissionErrorRecordsFailed`、`TestScheduler_Fire_SessionBusyRecordsSkipped`、`TestScheduler_Fire_StartExecutionFailure`、`TestScheduler_Fire_LockMissRecordsSkipped`、`TestScheduler_Fire_PanicRecovered`、`TestScheduler_Fire_FinishUsesCleanupCtx`（2）、`TestScheduler_Fire_ArchivedSessionSkipped`、`TestScheduler_Fire_ConcurrentDistinctTasks`、`TestScheduler_LogFieldsAndNoBody`；fake `TaskStore` / `RunInvoker` / `CronEngine` + 注入时钟，直接调 `fire`；红态先行（逐条逆改点见 §3.2.1 括注）；覆盖 R5 / R6 / R7 / R8 / R23 / R25 / R27（check: `010-core-scheduler`）

### Implementation for User Story 1

- [x] T017 [US1] 落 `internal/core/scheduler.go` 的 P1 面：`SchedulerOptions` / `Scheduler` / `NewScheduler`（`registry` + `definitions` 只读快照）、`lockFor`（`sync.Map`）、`Start` 两阶段（对账事务 → 统一注册）、`fire`（读锁内代次校验 + `ListTasks` 快照）、`scheduleRun`（`StartExecution → MarkTaskRun → Run → FinishExecution`，清理 ctx）、`Stop` 基础面、启动 ctx watcher、失败 / 跳过日志字段；`List` / `Executions` / `SetEnabled` / `RunNow` / `ReconcileAgent` 留到对应故事（依赖 T004 / T008 / T010 / T014）
- [x] T018 [US1] 落 `internal/app/schedule_test.go` 的 7 个函数 / 14 例——`TestNeedsSchedules`（4）、`TestNewScheduler_Options`（3）、`TestRun_ScheduleStartModes`（3）、`TestRun_ScheduleStartFailureExits`、`TestScheduleStartExitCode_SignalCanceledReturnsZero`、`TestScheduleStartExitCode_FailureReturnsOne`、`TestNewRuntime_ServiceHasSessionGate`；红态先行（check: `010-app-schedule`）
- [x] T019 [US1] 落 `internal/app/schedule.go` 的 P1 面：`scheduleRunTimeout = 300s` / `scheduleStopGrace = 70s`、`newScheduler`、`needsSchedules`（仅 serve / gateway）、`scheduleStartExitCode`、`schedulerRunner`、`startSchedules`（依赖 T017 / T018）
- [x] T020 [US1] 接装配与进程模式：改 `internal/app/wire.go`（`heavyRuntime.scheduler`、构造 `newScheduler`、`Close` 首段 `scheduler.Stop()`）、`internal/app/run.go`（`App.notifyContext` 接缝、`signalContext` 单次派生、消费 `startSchedules` 与 `scheduleStartExitCode`）、`internal/app/web.go`（`webOptions.Schedules = rt.scheduler`）、`internal/cli/serve.go`（serve / gateway 删除自建 `NotifyContext`，gateway 文案改 §6.3 第 22 条、Short / Long 同步）；`runWithRuntime` 签名不变（依赖 T018 / T019）

**Checkpoint**: P1 可独立验证——cron 到点走 `fire → scheduleRun`，`run_id` 前缀 `scheduler-`、会话固定、执行行与 `runs.trigger=cron` 同源；`serve` / `gateway` 启动调度、`chat` 不启动。

---

## Phase 4: User Story 2 - 重启与停机一致性 (Priority: P2)

**Goal**: 重启以文件为定义源、库为状态源；不复活停用任务、不补跑停机错过点；单 Agent 对账全成或全败、提交后才切句柄；旧回调按代次作废；停止有界且幂等。

**Independent Test**: `go test -run '^TestScheduler_(StartRecovers|StartReconciles|StartRegisters|StartTwice|Fire_Missed|Stop|Reconcile|OldCallback|Start_Store|Start_Task|Start_Owner|Start_Second)' ./internal/core` 全绿；`go test -run '^(TestHeavyRuntime_CloseStopsSchedulerFirst|TestApp_SignalContextUsesInjectedNotifier)$' ./internal/app` 全绿；重启 / 停机场景见 [quickstart.md](./quickstart.md) §4.2 场景 7~9。

### Tests for User Story 2

- [x] T021 [US2] 追加 `internal/core/scheduler_test.go` 的 18 个函数 / 34 例——`TestScheduler_StartRecoversExecutions`（4）、`TestScheduler_StartReconcilesDefinitions`（6）、`TestScheduler_StartRegistersEnabledOnly`（2）、`TestScheduler_StartTwice`、`TestScheduler_Fire_MissedTriggerWarnsWithoutRun`（2）、`TestScheduler_StopConcurrent`、`TestScheduler_StopOnContextCancel`、`TestScheduler_StopBeforeStart`、`TestScheduler_StopTimeoutCancelsInflight`、`TestScheduler_ReconcileAgent`（4）、`TestScheduler_ReconcileAgent_StoreFailureKeepsRegistration`、`TestScheduler_Start_StoreFailureFatal`、`TestScheduler_Start_TaskAddFailureStopsEngine`、`TestScheduler_Start_OwnerConflictFatal`（2）、`TestScheduler_Reconcile_SecondWriteFails_KeepsOldDefinition`、`TestScheduler_OldCallbackAfterReconcile_NoRun`（3）、`TestScheduler_ReconcileAgent_AddFailureReturnsError`、`TestScheduler_Start_SecondAgentApplyFails_NoHandles`；红态先行；覆盖 R3 / R4 / R9 / R19 / R24 / R26 / R28 / R29（check: `010-core-scheduler`）

### Implementation for User Story 2

- [x] T022 [US2] 扩展 `internal/core/scheduler.go`：启动先 `RecoverStartedExecutions`、再两阶段对账（任一 `ApplyAgentTasks` 失败 → 零句柄、`Start` 返回错误、不置位）；启动存储失败 / 单任务 `Add` 失败 fatal；`ReconcileAgent`（`stateMu` 写锁内事务提交后切句柄、逐任务自增代次、聚合 `Add` 错误）；`Stop` 70s 宽限 + 5s 有界清理 + `sync.Once` + ctx watcher 恰一次；错过触发点 `Occurrences` 枚举上限 1000 与 WARN；归属冲突 fatal（依赖 T017 / T021）
- [x] T023 [US2] 追加 `internal/app/schedule_test.go` 的 `TestHeavyRuntime_CloseStopsSchedulerFirst` 与 `TestApp_SignalContextUsesInjectedNotifier`；改 `internal/app/wire.go` 的 `Close` 顺序（scheduler → MCP → DB → 实例锁），并验证 T020 已落地的 `App.notifyContext` 注入接缝在信号取消时只触发一次 `Stop`；红态先行（check: `010-app-schedule`）

**Checkpoint**: 重启不复活停用、不补跑、遗留 `started` 行全部收尾；对账失败保留完整旧定义与旧句柄；信号取消触发 `Stop` 恰一次。

---

## Phase 5: User Story 3 - 任务管理与手动补跑 (Priority: P3)

**Goal**: 四个管理端点（列表 / 启停 / 执行历史 / 手动补跑）与错误映射；手动补跑与 cron 共用执行链；锁失 409、归档 400、不可见 404；视图脱敏。

**Independent Test**: `go test -run '^(TestSchedules_|TestRunView_Trigger|TestPostMessage_ArchivedMapsTo400|TestArchiveSession_ServiceBusyMapsTo409)' ./internal/web` 全绿；`go test -run '^TestScheduler_(RunNow_|SetEnabled_|List_|Executions_)' ./internal/core` 全绿。

### Tests for User Story 3

- [x] T024 [US3] 追加 `internal/core/scheduler_test.go` 的 14 个函数 / 24 例——`TestScheduler_RunNow_SameChain`（2）、`TestScheduler_RunNow_BusyReturnsErrTaskBusy`、`TestScheduler_RunNow_HiddenTaskNotFound`（3）、`TestScheduler_RunNow_DisabledTaskStillRuns`、`TestScheduler_RunNow_SessionBusyRecordsSkipped`、`TestScheduler_SetEnabled_TogglesRegistration`（3）、`TestScheduler_SetEnabled_HiddenTaskNotFound`（3）、`TestScheduler_List_FiltersAndOrders`、`TestScheduler_List_ReflectsRunState`、`TestScheduler_Executions_UsesLimit`、`TestScheduler_Executions_HiddenTaskNotFound`（3）、`TestScheduler_SetEnabled_WritesDBFirst`（2）、`TestScheduler_SetEnabled_ConcurrentToggles`、`TestScheduler_RunNow_ArchivedSessionPropagates`；红态先行；覆盖 R10 / R11 / R23 / R25 / R26 / R27（check: `010-core-scheduler`）
- [x] T026 [US3] 落 `internal/web/handlers_schedules_test.go`：12 个函数 / 30 例——`TestSchedules_ListShapeAndOrder`（3）、`TestSchedules_SetEnabled`（4）、`TestSchedules_Executions`（4）、`TestSchedules_RunSuccess`（3）、`TestSchedules_RunConflict`（2）、`TestSchedules_RunNotFound`、`TestSchedules_RunTimeout`（2）、`TestSchedules_ExecutionMessageProjection`（4）、`TestSchedules_MethodNotAllowed404`、`TestSchedules_RunTooManyRuns`、`TestSchedules_NilServiceReturns500`（4）、`TestSchedules_RunArchived400`；红态先行；覆盖 R17 / R22 / R23 / R27（check: `010-web-schedules`）
- [x] T030 [US3] 改 `internal/web/handlers_test.go`：新增 `TestRunView_TriggerFallback`、`TestRunView_TriggerFromRequest`、`TestPostMessage_ArchivedMapsTo400`、`TestArchiveSession_ServiceBusyMapsTo409`（+4 例），并按 §3.5 第 4 条把 `TestSessions_ArchiveSemantics` / `TestSessions_ArchiveRejectedWhileRunInflight` 改为经 fake `RunService.Archive` 驱动（断言口径逐条保留）；改 `internal/web/server_test.go` 给 `slowRunService` / `runFunc` 补 `Archive` 桩（+0 例，编译适配）；红态先行（check: `010-web-schedules`）
- [x] T031 [US3] 改 `internal/web/contract_test.go`：新增 `TestOpenAPI_ScheduleTimeFields` 4 例——加载真实 `docs/contracts/openapi.yaml`，启用 `AssertFormat()` 的 Draft 2020-12 校验器，同一输入分别校验 `ScheduleView.last_run_at` 与 `ExecutionView.finished_at` 的 `oneOf`（合法 RFC3339 / `""` / 非法串 / 数字）；`mustDig` 按路径断言存在；红态先行；覆盖 R31（check: `010-web-contract-schema`）

### Implementation for User Story 3

- [x] T025 [US3] 扩展 `internal/core/scheduler.go`：`List` / `Executions` / `SetEnabled` / `RunNow` 与共用可见性判定（retired / 未注册 Agent 隐藏）、`SetEnabled` 的「判定 → 写库 → 动引擎」写锁临界区与幂等短路、`RunNow` 的 `TryLock` → `ErrTaskBusy`、`SessionBusyError` 原样上抛（依赖 T017 / T022 / T024）
- [x] T027 [US3] 改 `internal/web/views.go`：`ScheduleView` / `ExecutionView` 与 `scheduleView` / `executionView` / `publicExecutionMessage`（三路精确投影）；`runView.Trigger` 改取 `req.Trigger`、空回落 `api`；时间列经既有 `formatTime`（依赖 T004）
- [x] T028 [US3] 落 `internal/web/handlers_schedules.go`：`ScheduleService` 端口、`scheduleHistoryLimit = 100`、`messageTaskBusy`、`setEnabledBody` 与四个 handler；200 / 400 / 404 / 409 / 429 / 504 / 500 映射逐字照 §3.1.7（依赖 T025 / T027）
- [x] T029 [US3] 改 `internal/web/server.go`（`Options.Schedules`、四条方法模式路由、`RunService.Archive`、`SessionStore` 收窄、删除 `admissionMu` / `inflightRuns` / `admitRun` / `releaseRun` / `archiveSession`）与 `internal/web/handlers_sessions.go`（消息发送改由 `Run` 门禁、归档调 `RunService.Archive`、`SessionArchivedError` → 400、删除包内哨兵）（依赖 T008 / T028）

**Checkpoint**: 四端点可独立验证；`grep -rn 'admissionMu\|inflightRuns\|admitRun\|releaseRun' internal/web` 零命中；归档与会话忙的映射只在 `writeRunError` / `Archive` 一处。

---

## Phase 6: User Story 4 - Agent 自管任务 (Priority: P4)

**Goal**: 内置工具 `manage_schedule` 以运行时 Agent 身份读写自己的 `schedules:` 段；唯一写入门、同源校验、原子替换、claim 基线与归属守卫；`serve` / `gateway` 立即生效，`chat` / `tool` 只落盘。

**Independent Test**: `go test -run '^(TestApplySchedule_|TestReadSchedules)' ./internal/workspace`、`go test -run '^TestManageSchedule_' ./internal/tool`、`go test -run '^TestScheduleEditorAdapter_' ./internal/app`、`go test -run '^TestCrossEntry_' ./internal/app` 全绿；Agent 自管场景见 [quickstart.md](./quickstart.md) §4.2 场景 4 / 5 / 14。

### Tests for User Story 4

- [x] T032 [US4] 落 `internal/workspace/schedule_write_test.go`：12 个函数 / 24 例——`TestApplySchedule_SetAddsAndReturnsValidated`（4，含 D-10 省略 id 实际定位）、`TestApplySchedule_Remove`（2）、`TestApplySchedule_PreservesOtherBytes`（3）、`TestApplySchedule_RejectsInvalidCandidate`（4）、`TestApplySchedule_CrossAgentIDRejected`、`TestApplySchedule_RemoveUnknownIDNotFound`、`TestApplySchedule_NoTempResidue`（2）、`TestApplySchedule_AtomicOnRenameFailure`、`TestApplySchedule_ConcurrentSets`、`TestReadSchedules`（2）、`TestApplySchedule_DoesNotTouchOtherAgents`、`TestApplySchedule_ClaimBaseline`（2）；红态先行；覆盖 R12 / R13 / R14 / R15 / R30（check: `010-workspace-writegate`）
- [x] T034 [US4] 落 `internal/tool/manageschedule_test.go`：10 个函数 / 15 例——`TestManageSchedule_NoAgentContextRejected`、`TestManageSchedule_List`（2）、`TestManageSchedule_Set`（3）、`TestManageSchedule_Remove`（2）、`TestManageSchedule_InvalidAction`、`TestManageSchedule_UsesEnvAgentOnly`、`TestManageSchedule_NotFoundClassified`、`TestManageSchedule_InvalidInputClassified`（2）、`TestManageSchedule_UnboundSlotFails`、`TestManageSchedule_ToolSpec`；红态先行；覆盖 R16（check: `010-tool-manageschedule`）
- [x] T036 [US4] 追加 `internal/core/scheduler_test.go` 的 `TestScheduler_CheckOwnership` 2 例（他人 active / retired → `ErrTaskOwnerConflict`；无行或同 owner 的 retired → nil）；红态先行；覆盖 R29（check: `010-core-scheduler`）
- [x] T037 [US4] 追加 `internal/app/schedule_test.go` 的 6 个函数 / 7 例——`TestScheduleEditorAdapter_MapsRejectionToInvalidInput`（2）、`TestScheduleEditorAdapter_ReconcilesAfterWrite`、`TestScheduleEditorAdapter_RefreshesClaimBaseline`、`TestScheduleEditorAdapter_ConcurrentCrossAgentSameID`、`TestScheduleEditorAdapter_ConcurrentWithProfilesRead`、`TestScheduleEditorAdapter_CrossOwnerConflict_NoWrite`；红态先行；覆盖 R12 / R26 / R29（check: `010-app-schedule`）
- [x] T040 [US4] 落 `internal/app/crossentry_test.go`：`TestCrossEntry_CronArchiveCoordination`（2）与 `TestCrossEntry_ManualRerunArchiveCoordination`（2），真实 `AgentService + SQLite + HTTP` 全链路，异步发起用 `PostHTTPAsync` / `WaitRunStarted`；红态先行；覆盖 R27（check: `010-app-crossentry`）

### Implementation for User Story 4

- [x] T033 [US4] 落 `internal/workspace/schedule_write.go`：`ErrScheduleRejected`、`ReadSchedules`、`ApplySchedule`——`set` upsert 语义与 D-10 实际 id 定位、跨 Agent claim 拒绝（`invalid_input`）、同源 `validateSchedules`、`schedules:` 块外科式渲染、临时文件 + `os.Root.Rename` 原子替换、`writeMu` 全临界区（依赖 T015 / T032）
- [x] T035 [US4] 落 `internal/tool/manageschedule.go`：`ScheduleEditor` 接口、`ScheduleEditorSlot`（未绑定显式报错）、`ManageScheduleTool` 的 `Name` / `Description` / `InputSchema` / `Execute`（身份只取 `env.AgentName`，`invalid_input` / `not_found` 分类）（依赖 T004 / T034）
- [x] T038 [US4] 落 `internal/app/schedule.go` 的写入门装配：`scheduleEditorAdapter`（私有 `claims` 基线、`adapter.mu` 全临界区、落盘前 `CheckOwnership`、写后 `ReconcileAgent`、`ErrScheduleRejected` → `*core.InvalidInputError`）、`newScheduleEditorAdapter`、`ReadSchedules` / `ApplySchedule`（依赖 T025 / T033 / T035 / T036 / T037）
- [x] T039 [US4] 接第 10 件工具：改 `internal/app/wire.go`（`tool.NewScheduleEditorSlot()` → `Register(NewManageScheduleTool(slot))`、`declaredToolNames` 扩为 10、slot `Bind` 适配器）、`internal/tool/doc.go`（包注释改真：九件既有 + `manage_schedule`，删除过时句）、`internal/app/web_test.go`（+2：`TestDeclaredToolNames_IncludesManageSchedule`、`TestWebOptions_SchedulesWired`；九名夹具注释同步为"US-10 前十名子集"）、`internal/app/ports_test.go`（`...NineTools` 改名 `...TenTools` 并改精确集 9 → 10）；判据面变更仅此六处已登记项（check: `010-app-tools`）

**Checkpoint**: Agent 只能改自己的任务；写入原子、字节保留、零临时残留；跨 Agent 改 / 删 → `invalid_input` 且零写入；`serve` 下写后立即生效。

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: 格式 / 静态门禁、全量验收、覆盖率、零增量核查与收口证据。

- [x] T041 格式与静态检查：对本轮改动文件执行 `gofumpt -w <files>`（禁手改格式、不得重排无关文件）；`go vet ./...`、staticcheck（`-checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022`）、`gosec -quiet ./...`、`govulncheck ./...` 零命中；复核零增量：`git diff --stat -- internal/storage/schema.sql docs/contracts/schema.sql basalt.yaml.example Makefile .github scripts` 为空、`time.Local` / `.Local()` 零命中、`internal/cli` 零 `signal.NotifyContext`、`internal/app` 恰 1 处（`run.go`）、`santhosh-tekuri` 生产代码零导入
- [x] T042 全量门禁：`make check`（`fmt lint sec test test-integration cover build` 七目标全绿）与 `make cover` 按包判卷（`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%）；`go test -race -count=1 ./internal/core ./internal/adapters/cron ./internal/storage ./internal/workspace ./internal/tool ./internal/web ./internal/app` 干净；逐目标四态读数写入 `run-state.md`
- [x] T043 阶段验收：先按冻结的 tasks.md 填 `harness/stages/us10-scheduler.json` 的 `tasks` 映射，再跑 `.\scripts\check-acceptance.ps1 --stage harness/stages/us10-scheduler.json`；13 条 check 全部 PASS、每个 `required_tests` 真跑（空匹配 / SKIP 不算通过）；逐 check 读数写入 `run-state.md`
- [x] T044 跨模块回归：`go test -count=1 ./...` 与前序模块全部测试绿（含 `internal/workspace/schedule_test.go:22-34` 的 `TestAgentLoader_RejectsFiveFieldCron`、`internal/app/run_test.go:36-49` 的 `TestRun_HeavyCommandSplit`）；`010-race` 的 14 个并发敏感函数全部出现在输出中
- [x] T045 收口准备：更新 `specs/010-scheduler/run-state.md` §4 未验证清单（机器项翻转「已验证」须注日期与证据，人工 Demo 项保持未验证）、清理临时物（探针 / 临时二进制 / 临时消息文件，仓库外临时目录用毕即删）；按模块卡 §6.5 汇总七项证据，准备人工 review ② 收口包；**不 commit / push / tag**

---

## 交付物覆盖核对（37 个代码文件）

| # | 文件 | 任务 | # | 文件 | 任务 |
| --- | --- | --- | --- | --- | --- |
| 1 | `internal/core/task.go` | T004 | 20 | `internal/app/run.go` | T020 / T023 |
| 2 | `internal/core/scheduler.go` | T017 / T022 / T025 / T036 | 21 | `internal/app/web.go` | T020 |
| 3 | `internal/adapters/cron/cron.go` | T014 | 22 | `internal/cli/serve.go` | T020 |
| 4 | `internal/storage/task_store.go` | T010 | 23 | `internal/core/scheduler_test.go` | T016 / T021 / T024 / T036 |
| 5 | `internal/workspace/schedule_write.go` | T033 | 24 | `internal/adapters/cron/cron_test.go` | T013 |
| 6 | `internal/tool/manageschedule.go` | T035 | 25 | `internal/storage/task_store_test.go` | T009 |
| 7 | `internal/web/handlers_schedules.go` | T028 | 26 | `internal/workspace/schedule_write_test.go` | T032 |
| 8 | `internal/app/schedule.go` | T019 / T038 | 27 | `internal/tool/manageschedule_test.go` | T034 |
| 9 | `internal/core/run.go` | T006 | 28 | `internal/web/handlers_schedules_test.go` | T026 |
| 10 | `internal/core/store.go` | T006 | 29 | `internal/app/schedule_test.go` | T018 / T023 / T037 |
| 11 | `internal/core/service.go` | T008 | 30 | `internal/app/crossentry_test.go` | T040 |
| 12 | `internal/core/errors.go` | T005 | 31 | `internal/core/service_test.go` | T007 |
| 13 | `internal/storage/run_store.go` | T012 | 32 | `internal/storage/run_store_test.go` | T011 |
| 14 | `internal/workspace/agent.go` | T015 | 33 | `internal/web/handlers_test.go` | T030 |
| 15 | `internal/tool/doc.go` | T039 | 34 | `internal/web/contract_test.go` | T031 |
| 16 | `internal/web/server.go` | T029 | 35 | `internal/app/web_test.go` | T039 |
| 17 | `internal/web/views.go` | T027 | 36 | `internal/app/ports_test.go` | T039 |
| 18 | `internal/web/handlers_sessions.go` | T029 | 37 | `internal/web/server_test.go` | T030 |
| 19 | `internal/app/wire.go` | T020 / T039 | | | |

**计数核对**：新增生产 8（T004 / T014 / T017+ / T033 / T035 / T028 / T019+ / T010）、修改生产 14、新增测试 8、修改测试 7；配置与迁移各 0。契约两份与 `go.mod` / `go.sum` 为已确认增量，不重复计入 37。

---

## 关键回归 R1~R31 → 任务映射

| R | 任务 | R | 任务 |
| --- | --- | --- | --- |
| R1 | T014（5 段拒绝）+ 既有 workspace 回归 | R17 | T026 / T027 / T028 / T029 |
| R2 | T013 / T014 | R18 | T011 / T012 / T030 |
| R3 | T021 / T022 + T009 / T010 | R19 | T018 / T019 / T020 / T021 / T022 |
| R4 | T021 / T022 + T009 / T010 | R20 | T023 |
| R5 | T016 / T017 | R21 | T002 + T041 / T042 |
| R6 | T016 / T017 / T026 / T028 | R22 | T009 / T010 / T026 / T027 |
| R7 | T016 / T017 | R23 | T016 / T017 / T024 / T025 / T026 |
| R8 | T016 / T017 | R24 | T021 / T022 / T023 |
| R9 | T021 / T022 | R25 | T016 / T017 / T024 / T025 |
| R10 | T024 / T025 / T026 / T028 | R26 | T016 / T017 / T024 / T025 / T037 / T038 |
| R11 | T021 / T022 / T024 / T025 / T026 | R27 | T007 / T008 / T016 / T017 / T024 / T025 / T030 / T040 |
| R12 | T032 / T033 / T037 / T038 | R28 | T009 / T010 / T021 / T022 |
| R13 | T032 / T033 | R29 | T009 / T010 / T021 / T022 / T036 / T037 |
| R14 | T032 / T033 | R30 | T032 / T033 |
| R15 | T032 / T033 | R31 | T002 / T031 |
| R16 | T034 / T035 | | |

---

## 专属 check → 任务映射

| check ID | 任务 | check ID | 任务 |
| --- | --- | --- | --- |
| `010-core-scheduler` | T016 / T017 / T021 / T022 / T024 / T025 / T036 | `010-web-schedules` | T026 / T027 / T028 / T029 / T030 |
| `010-core-sessiongate` | T007 / T008 | `010-web-contract-schema` | T002 / T031 |
| `010-core-runid` | T007 / T008 | `010-app-schedule` | T018 / T019 / T020 / T023 / T037 / T038 |
| `010-cron-adapter` | T013 / T014 | `010-app-tools` | T039 |
| `010-storage-tasks` | T009 / T010 / T011 / T012 | `010-app-crossentry` | T040 |
| `010-workspace-writegate` | T032 / T033 | `010-race` | T042（最终）；各并发用例随所属任务 |
| `010-tool-manageschedule` | T034 / T035 | | |

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: 无依赖，立即开始
- **Foundational (Phase 2)**: 依赖 Setup（T002 坐标、T004 类型）；**阻塞全部用户故事**
- **User Stories (Phase 3~6)**: 均依赖 Foundational；按 P1 → P2 → P3 → P4 顺序执行
- **Polish (Phase 7)**: 依赖全部故事完成

### User Story Dependencies

- **US1 (P1)**: Foundational 后即可开始——MVP；`Start` 的两阶段对账与 `fire` 是后续故事的基础
- **US2 (P2)**: 依赖 US1 的 `Scheduler` 骨架与 `fire`；补齐恢复、对账、代次与停止
- **US3 (P3)**: 依赖 US1 / US2 的调度器方法；web 面只依赖 `ScheduleService` 端口
- **US4 (P4)**: 依赖 US1~US3（`ReconcileAgent`、`CheckOwnership`、`RunService.Archive`）；跨入口测试最后收口

### Within Each User Story

- 测试任务先于对应实现任务（T016 → T017、T021 → T022、T024 → T025、T032 → T033、T034 → T035、T037 → T038）
- 同一文件的任务按 ID 顺序执行（`core/scheduler.go`：T017 → T022 → T025 → T036；`app/schedule.go`：T019 → T038）
- 关键回归先红后绿：红态必须看到逐字预期失败，不得只看退出码

### Parallel Opportunities

- Foundational：T004 / T005 / T006 / T007 / T009 / T011 / T013 / T015 可并行（不同文件）
- US3：T026 / T027 / T030 / T031 可并行（不同文件）
- US4：T032 / T034 可并行；T036 与 T037 可并行
- Polish：T041 / T042 / T043 / T044 有顺序依赖，按序执行

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Phase 1 Setup → Phase 2 Foundational
2. Phase 3 US1（到点触发 + 启动装配）
3. **STOP and VALIDATE**：`go test -run '^TestScheduler_(Fire_|LogFieldsAndNoBody)' ./internal/core` 与 [quickstart.md](./quickstart.md) §4.2 场景 1
4. 可演示真实的 cron 到点运行

### Incremental Delivery

1. Foundational → 类型 / 存储 / cron / 会话门禁可独立验证
2. US1 → 到点自动运行（MVP）
3. US2 → 重启与停机一致性
4. US3 → 管理端点与手动补跑
5. US4 → Agent 自管任务与跨入口收口
6. Polish → 全量门禁、13 条专属 check、覆盖率与收口证据

### Notes

- 每个任务先跑对应包测试，失败即时修复，不累积到任务末尾
- 同一问题修复 3 次不收敛 → 停下走 `/debug`；预算与计数写 `run-state.md`
- 判据面变更仅 §3.5 表的六处（十名工具字面量、九→十重命名、trigger 两例、归档重排、server 编译桩、contract 纯追加）；既有断言零删除、零跳过、零放宽
- 零迁移、零新增配置键、运行时零新增依赖；唯一例外是测试专用坐标及其必要传递依赖
- 所有验收命令带 `GOPROXY=https://goproxy.cn`；`-race` 与 `CGO_ENABLED=0` 发布构建分属不同命令

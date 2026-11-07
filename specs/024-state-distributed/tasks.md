# Tasks: US24 状态外置与分布式执行

**Input**: spec.md / plan.md / research.md / data-model.md / contracts/README.md / quickstart.md；颗粒度文档 `docs/us/us24-state-distributed.md` R7（review ① 已通过，SHA-256 `02e9f941…`，3385 行）。

**Tests**: 已审 Harness 的必跑测试与关键反例必须落地；安全 / 并发 / 审计类红态先行；不删除断言、不无理由跳过、不放宽阈值。测试函数名用模块卡 §3.2 / §4.3 / §4.4 已给出的英文名（`TestXxx_yyy` 形态），中文验收点写进注释。关键回归按模块卡 §4.4 逐条落地并保留逆改点说明。

## Phase 1: Setup

- [x] T001 核对 US-22 收口、台账行 24 与模块卡 R7 的 review ① 依据；确认分支 `024-state-distributed`、起点 `138382f` 与工作树状态，建立 `specs/024-state-distributed/run-state.md` 基线与失败预算；DoD：登记唯一、R7 哈希可复算、spec / plan / tasks 指纹与文件一致、无范围外改动。
- [x] T002 从模块卡 §4.7 派生 `harness/stages/us24-base.json` 与 `harness/stages/us24-fault.json`：公共检查引用 `harness/acceptance.json` 的 check 定义（`fmt` / `lint` / `sec` / `test` / `test-integration` / `test-pg` / `cover` / `build` / `eval-build` / `behavior` / `injection`），专属检查按包拆分（core 租约 / assets / PG 租约与 worker / PG 运行与调度与回收与 Flow / app 装配与故障 / cli / web / callers / console / race），`required_tests` 逐条取自 §4.3 与 §4.4，`protect` 覆盖 §3.1 新增与改造文件及既有回归测试，`tasks` 映射本文件；DoD：两份 JSON 合法、命令在本仓可执行、不复制公共清单、`us24-fault` 固定随机种子且不依赖真实时间窗。

## Phase 2: Foundational

**Purpose**: 迁移与租约原语——所有用户故事的共同前置

- [x] T003 在 `internal/storage/postgres/migrations/0012_distributed_execution.sql` 落三张新表（`worker_nodes` / `execution_leases` / `lease_events`）、四组既有表列（`scheduled_tasks.next_fire_at`、`task_executions.planned_at/owner_worker_id/ownership_epoch`、`schedule_deliveries.execution_id`、`runs.cancel_requested`）、两侧索引与两个 `resource_kind` CHECK 扩展（`agent_bundle`），三类回填 `legacy` / `0` / 已过期与两类关闭态根不回填；在 `internal/storage/postgres/connect.go` 把 `SupportedSchemaVersion` 推进到 12；在 `internal/storage/postgres/migration_0012_test.go` 落 `TestMigration0012_BackfillsInFlightLeases` / `TestMigration0012_ResourceKindDomain` / `TestLeaseResourceIDEncoding`；DoD：约束名先在隔离 schema 按 `pg_constraint` 实测（不符改用 `DO` 块按 `pg_get_constraintdef` 定位，不得静默空转）、`0001`–`0011` 零改写、`schema.pg.sql` 不改、v11 旧库前向可迁移、重复执行幂等、迁移脚本不使用 `$1` 参数、`run` / `schedule_execution` / `flow_root` 三处 `resource_id` 编码逐字一致。
- [x] T004 [P] 在 `internal/core/lease.go` 落 `LeaseKind` 三值常量（`LeaseRun` / `LeaseScheduleExecution` / `LeaseFlowRoot`）与未知值 fail-closed、`WorkerIdentity`（`InstanceID` 为身份，`Name` 允许重复）、`LeaseToken{Kind, ResourceID, OwnerWorkerID, OwnershipEpoch}`（显式参数，**不得** `context.WithValue`）、`Lease`（含 `CancelRequested` 与 `TookOver`）、`ReclaimReport`、哨兵 `ErrLeaseHeld` / `ErrLeaseLost` / `ErrLeaseRenewDeadline` / `ErrCancelRequested` / `ErrCallerConcurrencyLimited` / `ErrTriggerAlreadyClaimed` / `ErrStaleNextFire`，以及 `LeaseStore` / `WorkerRegistry` / `Reclaimer` / `SubtreeGate` 四个消费方接口；在 `internal/core/lease_test.go` 落 `TestLease_UnknownKindRejected` 与 token 相等 / 类型解析用例；DoD：`core` 只依赖标准库、既有字段与调用方兼容、哨兵可按 `errors.Is` 区分、签名与模块卡 §3.1.1 逐字一致。
- [x] T005 在 `internal/storage/postgres/lease_store.go` 落 `Acquire`（行锁读写，返回带 `TookOver` 的 `Lease`）、`Renew`（**行级条件写** `UPDATE … WHERE kind/resource_id/owner/epoch AND expires_at > db_now`，命中 0 行即 `ErrLeaseLost`，不得退化为先读后判）、`Release`、`FencedTx`（同一事务 `SELECT … FOR UPDATE` 锁租约行再校验 owner + epoch + 未过期，**不得**用 `EXISTS` 快照）与 `lease_events` 五值写入；时间一律 `(extract(epoch from clock_timestamp())*1000)::bigint`；在 `internal/storage/postgres/lease_store_test.go` 落 `TestLease_RenewRequiresOwnerAndEpoch` / `TestLease_UnknownKindRejected` / `TestFencedWrites_OldHolderBlocksTakeover` 与领取 / 过期 / 接管 / 代次 / 释放用例；DoD：两种获锁顺序成立、快照读路径被拒、`lease_events` 只记状态转换不记每次续约、既有迁移与测试不回归。

## Phase 3: User Story 1 - Worker 身份、执行租约与归属保护写入 (P1)

**Goal**: 每个进程有唯一身份；一切受归属保护的写入在失权后零写入。

**Independent Test**: 在隔离 schema 构造两个负责人：接管后旧负责人调用消息追加 / 工具结果回填 / 恢复补齐 / 状态推进 / 审计预写 / 学习三写面，全部返回失权错误且零行写入；keeper 在续约持续失败时于预算内取消上下文。

- [x] T006 [US1] 在 `internal/storage/postgres/worker_store.go` 落 `Register` / `Heartbeat` / `Stop`；在 `internal/app/worker_runtime.go` 落 `workerRuntime`、`newInstanceID` 与注册 / 心跳循环，`internal/app/wire.go` / `run.go` 按模块卡图 1 装配启动顺序；在 `internal/cli/config.go` 落 `worker` / `scheduler` / `assets` 三段与装载期校验（§3.3 全部键与全部校验规则逐条落地：`renew_interval < ttl/2`、`reclaim_interval ≤ ttl`、`max_inflight` 范围、`shutdown_grace` 双向范围、`dispatch_batch` 两条上限、`bundle_root` 非空，且删除 `require_bundle`）；在 `internal/storage/postgres/worker_store_test.go` 落 `TestWorkerRegistry_RegisterHeartbeatStop` / `TestWorkerRegistry_DuplicateNameAllowed`，并补配置校验用例；DoD：心跳只作诊断、重名实例互不覆盖、配置非法即启动失败不静默取缺省、`worker_nodes` 只在注册时写 schema 版本。
- [x] T007 [US1] 在 `internal/app/worker_runtime.go` 落 `keepLease`（续约循环，签名与模块卡 §2.3.3 一致）：连续失败累计达到 `ttl - renewInterval` 即以 `ErrLeaseRenewDeadline` 取消执行上下文并停止新动作，瞬时失败不误停；成功续约返回 `Lease.CancelRequested`；在 `internal/core/service.go` 的 `Run` 启动 ReAct 循环前派生并传递该 ctx；在 `internal/core/lease_test.go` 落 `TestRun_RenewFailureCancelsBeforeNextAction`、`TestKeepLease_RenewDeadlineStopsExecution` 与瞬时失败 / 成功续约对照用例；DoD：不得无限重试、不得声称在途外部请求被撤销、`stop` 使用剩余停机预算而非 `WithoutCancel`、goroutine 无泄漏。
- [x] T008 [US1] 把 `FencedTx` 接入消息追加、工具结果回填与审计 `pending` 预写三个写入面（`internal/core/service.go` 的消息追加路径、`internal/storage/postgres` 的会话消息 / 工具调用 / 模型调用存储）；在 `internal/storage/postgres/run_lease_test.go` 落 `TestFencedWrites_RejectAfterTakeover` 的这三个面（接管后旧 owner 全部 `ErrLeaseLost` 且 `llm_calls` / `tool_invocations` 零新行）与 `TestFencedWrites_LateWriteAfterTakeover` / `TestFencedWrites_InterleavedTakeover`（旧写完成 fence 校验后暂停、接管并发提交、再放行旧写，只有行锁方案能正确拒绝）；DoD：同一事务先锁租约行再校验、错误不吞、`核心逻辑`与模块卡图 3 逐块一致、恢复补齐与状态推进入口在 T017 / T022 补齐后完整矩阵全量成立。
- [x] T009 [US1] 在 `internal/core/learning.go` 落 `LearningRunInput.Token`，在 `internal/memory/learning.go` 透传 token，在 `internal/storage/postgres/learning_store.go` 为 `BeginExtraction` / `FailExtraction` / `CommitExtraction` 增 fence（advisory(agent) → 租约行 `FOR UPDATE` → 业务行，fence 对象是**触发 Run**，不得用 DB 的 `run_id` 或 `last_attempt_run_id`），重试 `UPDATE` 增 `attempts` / `last_attempt_run_id` 两个 CAS 条件，`window_gone` 按"当前租约 + 同来源 + 预期旧尝试"受控收尾；在 `internal/storage/postgres/run_lease_test.go` 落 `TestFencedWrites_RetryBindsCurrentRun` / `TestFencedWrites_WindowGoneControlledClose`；DoD：接管后三写面零写入、合法重试不误拒、`window_gone` 不递增 `attempts` 且不改 `last_attempt_run_id`、`WithoutCancel` 的失败回填语义不变。
- [x] T010 [US1] 在 `internal/storage/postgres/run_store.go` 落 `FinishRun`（**只写终态列**，带 token fencing，0 行必须区分"已终态"与"失权"）与 `CompleteRunCloseout`（引用释放 + 投递结算之后，最后释放租约），在 `internal/core/store.go` 改 `RunStore` 签名，在 `internal/core/service.go` 保持"终态 → 学习钩子 → `ReleaseRunRefs` → 投递 → `CompleteRunCloseout`"五步顺序（P-14 钩子仍在事务外）；在 `internal/storage/postgres/run_lease_test.go` 落 `TestRun_FinishRejectsStaleOwnerAfterTakeover`（§4.4.1 原样落地）与 `TestRun_FinishRejectsExpiredLease`（过期未接管同样拒绝）；DoD：接管后旧 owner 终态 `applied=false`、B 的结果不被覆盖、`FinishRun` 不吞审计 / 引用 / 投递、正常路径 B1 命中数为 0。

## Phase 4: User Story 2 - 跨 Worker 准入：会话串行、并发与配额同事务 (P1)

**Goal**: 准入的权威判定全部进数据库并收进同一个事务，被拒零占用。

**Independent Test**: 两个 Scheduler / App 共享同一隔离 schema，并发发起同会话 Run 与同一调用方配额请求；恰好一个成功、另一个 409 且零占用；错误优先级按序返回。

- [x] T011 [US2] 在 `internal/core/store.go` 落 `CallerAdmission{Enabled, MaxConcurrent, ReservedForInteractive, IsFlow}`、`RunRecord.Lifecycle`（`Generation` + `StateFileHash`，零值拒绝）、`RunResourceManifest` 升 v3 增 `AssetBundleHash`（P-11）、`StartRun(rec, manifest, owner WorkerIdentity, admission CallerAdmission, runID string) (LeaseToken, error)` / `LookupAdmittedRun` / `FinishRun` / `CompleteRunCloseout` / `RequestCancel` 端口与 `ErrAdmissionOutcomeUnknown`；在 `internal/core/service.go` 把准入路径改为 `CheckAdmission`（只读预检）→ `StartRun`（事务内权威判定 + 记账）；DoD：`core` 只依赖标准库、v2 清单按兼容路径解析且缺字段读作"未记录"、既有字段语义不变。
- [x] T012 [US2] 在 `internal/storage/postgres/run_store.go` 落 `StartRun` 单事务：调用方 advisory lock + 三段并发计数（total Run / Flow Run / 交互预留，Flow 叶 Run 只算一次）、会话 advisory lock + 活跃 run 租约（含恢复未完成窗口，FR-24-43）、每 Worker 实例上限、发布 / 生命周期拦障、任务次数记账 `pending → committed`、runs / 清单 / 引用 / 租约写入，任一步失败整笔回滚；任务次数记账在 `internal/storage/postgres/quota_store.go` 落**事务内私有 helper**（`pending → committed` 与 `StartRun` 同一 `Tx`，**不经** `ChargeRun`；既有 `Release` 对 committed 仍为 no-op，`quota_store.go:303-304` 语义不变，P-12）；`LookupAdmittedRun` 与 `ErrAdmissionOutcomeUnknown`；在 `internal/storage/postgres/run_lease_test.go` 落 `TestRun_SessionBusyAcrossWorkers`（§4.4.2）、`TestAdmission_BlocksUntilRecoveryCommits`、`TestStartRun_LeaseCreatedInSameTx`、`TestStartRun_TaskQuotaAccountedInSameTx`、`TestRun_CallerConcurrencyAcrossWorkers`、`TestQuota_PeriodBoundaryAcrossWorkers`；DoD：被拒零占用（无 Run 行、不扣配额、不建租约）、提交未知回查不退款不重试、会话锁与租约复查在同一互斥边界。
- [x] T013 [US2] 在 `internal/app` 落准入装配：`internal/app/run.go` 的 `AgentGate` 生命周期期望（P-16）、`internal/app/concurrency_limits.go` 内存名额表降为预检、`internal/app/run_quota.go` 改事务端口（P-12）、`internal/app/wire.go` 接线；`internal/callers/limiter.go` 把 `ErrConcurrencyLimited` 改为 `core.ErrCallerConcurrencyLimited` 别名（既有 `errors.Is` 与 429 映射不变）；在 `internal/app` 落 `TestRun_InstanceCapStaysPerWorker` / `TestAdmission_ErrorPrecedence`，在 `internal/callers/limiter_test.go` 追加 `TestLimiter_QPSRemainsPerWorkerSoftLimit`；DoD：错误优先级 `AUTH_RATE_LIMITED → CALLER_RATE_LIMITED → CALLER_CONCURRENCY_LIMITED → SESSION_BUSY → TOO_MANY_RUNS → QUOTA_EXHAUSTED` 逐项成立、既有准入测试按已批准接口适配不删断言、QPS 明确为每 Worker 软限额。

## Phase 5: User Story 3 - 数据库到期扫描与计划触发去重 (P1)

**Goal**: 调度触发源从进程内定时器替换为数据库到期扫描，同一计划只执行一次。

**Independent Test**: 两个调度循环并发扫描同一到期任务；每个计划时刻恰一行执行；错失窗口只产生一条 skipped；手动补跑在已有 started 时返回任务忙。

- [x] T014 [US3] 在 `internal/core/task.go` 落 `DueTask` / `DueExecution` 与 `ListDispatchable` / `SetNextFire`（CAS） / `ClaimDueTrigger` / `RecordMissedTrigger` / `RecordBusyTrigger` / `StartExecution` / `FinishExecution`，`CronEngine` 增 `Next` 并删除 `Add` / `Start` / `Stop`（进程内定时器 API 删除），在 `internal/adapters/cron/cron.go` 同步把 `Engine` 收敛为 `Next`（删除 `Add` / `Start` / `Stop` 实现，6 段解析与 `CRON_TZ` 守卫逐字保留，P-02）；在 `internal/core/scheduler.go` 落 `dispatchOnce` / `executeClaimed` / `nextFire`，删除 `locks` / `gens` / cron handle，`Start` 只启动到期扫描；`internal/app/schedule.go` 注入 `LeaseStore` / `WorkerIdentity` / `TTL`；DoD：6 段表达式、`CRON_TZ` 前缀、跳过、定义哈希、退休与错失记录语义逐项保留、`RunNow` 走 `StartExecution`。
- [x] T015 [US3] 在 `internal/storage/postgres/schedule_coord.go` 落 `ListDispatchable` / `SetNextFire` / `ClaimDueTrigger` / `RecordMissedTrigger` / `RecordBusyTrigger`（`SKIP LOCKED` + CAS + `(task_id, planned_at)` 唯一 + `ux_task_executions_active`），在 `internal/storage/postgres/task_store.go` 补 `planned_at` / `owner_worker_id` / `ownership_epoch` 读写并删除 `RecoverStartedExecutions`；在 `internal/storage/postgres/schedule_coord_test.go` 落 `TestScheduler_TriggerDedup_OneExecutionPerPlannedTime`（§4.4.3）、`TestScheduler_MissedTriggerRecordsSkippedWithoutReplay`、`TestScheduler_OverlapSkipsAndRunNowBusy`；DoD：重复领取返回 `ErrTriggerAlreadyClaimed` 且不产生第二条执行、下次到期被他人推进返回 `ErrStaleNextFire` 不覆盖、同一任务最多一条 `started`、手动补跑冲突返回既有 `ErrTaskBusy`。
- [x] T016 [US3] 改写 `internal/core/scheduler_test.go` 与适配 `internal/app/schedule_test.go`：把断言"进程内锁 / generation"的用例按模块卡 §4.8 / Q3 **等价替换**为数据库领取断言（6 段 cron / `CRON_TZ` / 跳过 / 定义哈希 / 退休 / 错失记录 / 唯一执行），不得删减验收语义；DoD：替换清单与 Q3 冻结口径逐条对应、`internal/app/schedule_test.go` 的 `TestFlowNodeRunner` / 既有调度用例全绿、红态先以逆改 CAS 或唯一索引复现失败文案。

## Phase 6: User Story 4 - 作用域回收：只碰合法领取的过期 / 已释放租约 (P1)

**Goal**: 回收只作用于合法领取的过期 / 已释放租约，活 Worker 的在跑行与审计事实零改动。

**Independent Test**: 构造活租约与过期租约并存、终态残留与无 Run 投递；回收只动过期 / 已释放者，执行行状态只落四值。

- [x] T017 [US4] 在 `internal/storage/postgres/subtree_gate.go` 落三个判据（`HasLiveRunLease` / `HasInFlightNodeAttempt` / `HasUnresolvedAction`），在 `internal/storage/postgres/reclaimer_store.go` 落 A 类三域 `ClaimExpired`（`SKIP LOCKED` + 代次条件），在 `internal/storage/postgres/recovery.go` 把全局 `Recover` 拆成 `RecoverRun` / `RecoverExecution` / `RecoverFlowRoot` 三个**作用域原语**（P-07），六步算法改为接收 `runIDs` / `token` 并按 run 过滤审计归一化与半截工具组，在 `internal/app/reclaimer.go` 实现 `core.Reclaimer`（组合作用域原语 + `flow.Service.Recover`，不自己判状态）、`internal/app/wire.go` / `internal/app/run.go` 启动改 `ReclaimOnce`（删除全局恢复扫描）；在 `internal/storage/postgres/reclaimer_test.go` 落 `TestReclaimer_DoesNotTouchLiveLease`（§4.4.4）、`TestReclaimer_ClosesExecutionAfterLeaseExpiry`、`TestReclaimer_AuditNormalizationScopedToClaimedRuns`，在 `internal/app` 落 `TestStartup_DoesNotRewriteOtherWorkersRows` / `TestReclaimer_RunsWithoutCronAndIsBounded`；DoD：按心跳 / 启动时间推断死亡的路径被拒、活租约零改动、A3 blocked 根按 reason 分路（`worker_lease_expired` 自重重入 `queued`、`bounded_rework_required` 不动、子 Run 租约仍有效不动作）、并发交错以 `pg_blocking_pids()` 观测且超时前提交 / 回滚，**不得**把 `lock_timeout` 报错当观测手段。
- [x] T018 [US4] 在 `internal/storage/postgres/delivery_store.go` 落 `Recover(ctx, now, runID)` 作用域版、`Prepare` 同事务写 `execution_id`、`SettleOrphans` 拆 B1（Run 已终态：`completed` / `limit_reached` → `unknown`，`failed` / `cancelled` → `skipped`）与 B2（无 Run 只写 `skipped`，`execution_id` 对应租约失效才动，NULL 按 `task_id` 判定），在 `internal/app/delivery.go` 接线；在 `internal/app` 落 `TestReclaimer_TerminalResidueAndPreRunDelivery` / `TestReclaimer_LeavesLiveExecutionContextAlone` / `TestReclaimer_ExecutionStatusStaysInFourValues`，在 `internal/storage/postgres` 落 `TestDelivery_RecoveryScopedToReclaimedRun`；DoD：终态残留三表归零、任何分支都不自动重发、执行行只落 `started` / `completed` / `failed` / `skipped` 四值、B2 不读 B1 的终态分支。
- [x] T019 [US4] 在 `internal/core/learning.go` 落 `RecoverLearningExtraction`（含 `ExpectedLastAttemptRunID`），在 `internal/storage/postgres/learning_store.go` 落 `OrphanPendingExtractions`（返回 `(extractionID, lastAttemptRunID)`）与带预期值重检的 `RecoverExtraction`（事务内 **advisory → 租约行 `FOR UPDATE` → 抽取行 `FOR UPDATE`**，获锁后用数据库时间重判），在 `internal/app/learning_recovery.go` 把"选取待查证行"抽成共享步骤（启动走 `PendingExtractions`，回收器走 `OrphanPendingExtractions`）；改写 `internal/app/learning_recovery_test.go` 落归属保护 / `TestReclaimer_OrphanExtractionVerifiedWithoutRestart` / `TestReclaimer_OrphanExtractionSkipsLegitimateRetry` / `TestReclaimer_OrphanExtractionWaitsForConcurrentRenewal`；DoD：B3 只查证不重放、不重启也能解阻、`last_attempt_run_id` 租约仍有效时跳过、扫描后合法重试本轮零写入、`NOT EXISTS` 快照读逆改点可复现红态。

## Phase 7: User Story 5 - Flow 根租约、接管恢复与两判据推进 (P1)

**Goal**: 正常交棒可直接推进，接管必须先查证；`Isolated` 与 `Joined` 独立取值。

**Independent Test**: 构造子 Run 仍 running 而租约已过期、`prepared` 无 Run、`interrupted` / `unknown` 尝试等状态；两判据独立取值、收敛只按 `Isolated`、终态不伪造。

- [x] T020 [US5] 在 `internal/flow/ports.go` 落 `AdvanceRequest.Lease` / `ResumeRequest.Lease`（`RequestID` 保留）/ `ClaimNode.Lease` / `CommitNode.Lease`、`ReconcileTask{Request, Status, Reason, Joined, Isolated, Now}`（无顶层 `Lease`）、`CancelTask.Isolated`；在 `internal/flow/control.go` 落 `Service.Identity` / `Service.Leases` / `withRootLease` / `releaseRootLease`（2s 超时取自调用方 ctx，不用 `WithoutCancel`）、`Recover` 零 token 自取、`ErrFlowRootBusy` 映射；`ReadyRoots` 增 `released_at IS NULL AND expires_at > db_now` 条件；在 `internal/flow/revision.go` 落 `Revise` 走 `withRootLease`；DoD：签名与模块卡 §3.1.6 逐字一致、关闭类写入不要求根租约但必须携带 `Isolated`、管理面零 token 自取。
- [x] T021 [US5] 在 `internal/storage/postgres/flow_store.go` / `internal/storage/postgres/flow_close_store.go` / `internal/storage/postgres/flow_revision_store.go` 落：`ClaimNode` / `CommitNode` / `Recover` 的租约条件、`updateFlowHead` 租约 `EXISTS` **按写入档分档**（推进类按 `root_task_id` 查父根，关闭类不加）、`Recover` 的 attempt 收敛覆盖整棵子树（`root_task_id=$1`）、`closeFlowTask` / `propagateFlowClose` / `CloseDeadline` 三处收敛由 `joined` 改判 `isolated`（`CloseState` 仍用 `joined`）、`CreateRunForNode`（父根租约 + 父根归属 `root_task_id` + `execution_epoch` + `task_revision`，不符 `ErrFlowAttemptStale`）；DoD：15 个 `updateFlowHead` 调用点逐一按写入档核对、子任务行写入按 `root_task_id` 查父根、既有 `execution_epoch` 条件提交保留只收紧不放宽。
- [x] T022 [US5] 在 `internal/flow/worker.go` 落 `Identity` / `Leases` / `Gate` / `ReleaseCtx` / `LeaseTTL` / `RenewInterval` 与 `TookOver` 分路（`true` → 先 `Recover` 再 `Advance`；`Recover` 出错不释放租约；`false` 可直接推进，但门 ③ 命中必须 `Abandon`），`internal/flow/execution.go` 的节点领取 / 提交带租约，`internal/app/flow.go` 的 `flowNodeRunner.Joined`（子树无 `prepared` / `running` attempt）与 `Isolated`（子树无有效 run 租约），并在 `internal/flow/worker.go` 导出 `StoredPrincipal(task)`（回收器在 `app` 层，不能调包内私有函数；只是可见性变化，P-05）；在 `internal/storage/postgres/flow_lease_test.go` 落 `TestFlow_WorkerClaimsRootLeaseBeforeAdvance` / `TestFlow_TakeoverBumpsExecutionEpochAndRejectsStaleCommit` / `TestFlow_JoinedWaitsForInFlightAttempts` / `TestFlow_LiveLeaseBlocksVerificationOnly` / `TestFlow_RecoverConvergesAttemptsUnderIsolation` / `TestFlow_RecoverConvergesWholeSubtree` / `TestFlow_UnresolvedActionGate`（`succeeded` / `not_issued` / `prepared` 不算未决），在 `internal/app` 落 `TestFlow_LeaseExpiryIsolatesButDoesNotJoin`（§4.4.5）/ `TestFlow_JoinedAfterRecoverResolvesAttempt` / `TestFlow_CompletedRunReusedNotReexecuted` / `TestFlow_TakeoverRecoversBeforeAdvance` / `TestFlow_TransientRecoverErrorRetriesRecoverBeforeAdvance` / `TestFlow_NormalHandoffRefusesUnresolvedAction` / `TestFlow_BlockedRootWithUnknownActionNotQueued` / `TestFlow_CreateRunForNodeRejectsForeignRootOrStaleRevision`；DoD：两判据各自独立取值、`interrupted` / `unknown` 不计入 `Joined`、普通 Worker 以真实 `TookOver=false` 领到未决动作时零新行且不释放租约。
- [x] T023 [US5] 在 `internal/app/flow_http.go` 与 `internal/web/handlers_flow.go` 把 `ErrFlowRootBusy` 映射为 409 `FLOW_ROOT_BUSY`（resume / revise 两条路径，**不得**漏成 500），在 `internal/app` 落 `TestFlow_AdminResumeBusyWhileWorkerHoldsRoot` / `TestReclaimer_FlowRootReentersBlockedRoot` / `TestReclaimer_UnknownActionReleaseKeepsGuard` / `TestFlow_CancelConvergesChildAttemptsUnderIsolation` / `TestFlow_DeadlineCloseConvergesChildAttemptsUnderIsolation`；DoD：租约释放后同一请求成功、`bounded_rework_required` 根不动、子 Run 租约仍有效时根一字未动、deadline 终态是 `stopped` 不是 `cancelled`。

## Phase 8: User Story 6 - 跨 Worker 取消、归档屏障与停机顺序 (P2)

**Goal**: 取消由属主如实收敛、归档先立屏障、停机按固定顺序。

**Independent Test**: 非属主取消后属主在续约响应中观察到标志并收敛；归档等待窗口内新 Run 被拒；停机断言续约持续、释放晚于收尾、连接最后关闭。

- [x] T024 [US6] 在 `internal/storage/postgres/run_store.go` 落 `RequestCancel`（非属主只写 `runs.cancel_requested`、如实返回当前状态），在 `internal/core/service.go` 把 `Cancel` 对非属主改走 `RequestCancel`，属主在续约响应 / 检查点观察 `Lease.CancelRequested` 并自行收敛；在 `internal/storage/postgres/run_lease_test.go` 落取消标志语义用例，在 `internal/app` 落 `TestCancel_RemoteWorkerObservesFlag`（§4.4.6）；DoD：非属主不声称已取消、属主在 ≤ 续约间隔内观察到、取消与终态提交的互斥保持。
- [x] T025 [US6] 在 `internal/app/agent_admin.go` 落归档**准入屏障前置**：先把 `agent_lifecycle.state` 置为 `disabled` 并把原状态写进 `management_operations.detail_json.prior_state`，再发跨 Worker 取消，再有界等待，超时才按原状态回滚并 409 `AGENT_BUSY`；屏障与 `StartRun` 的复查是同一行 `FOR UPDATE` 互斥；在 `internal/app/archive_test.go` 落 `TestArchive_BarrierRejectsNewRunsBeforeWait` / `TestArchive_AdmissionLockOrder`（两种真实获锁顺序）/ `TestArchive_RefusesWhenRemoteLeaseLive`；DoD：等待窗口内新 Run 被拒且不产生 Run 行、远端仍在跑时不移动目录不宣称完成、不新增 `archiving` 状态值、进程崩溃后不得永久停 `disabled`（由 `prior_state` 恢复路径保证）。
- [x] T026 [US6] 在 `internal/app/run.go` 落停机顺序（停止领取 → 排空期间继续续约 → join 执行与收尾 → 释放租约 → 关闭 PG；超过 `worker.shutdown_grace` 直接退出，不 Release、不关库），`internal/app/concurrency_limits.go` / `internal/app/wire.go` 落 `worker.max_inflight` 池化信号量（三条执行路径共用、取槽非阻塞、交互 Run 满额立即 `TOO_MANY_RUNS`、后台阻塞可被 drain ctx 取消），审计终态写用 `WithoutCancel` + 独立短超时；在 `internal/app/shutdown_test.go` 落 `TestShutdown_StopsClaimsReleasesLeasesBeforeClose` / `TestShutdown_RenewsDuringDrainAndJoinsBeforeRelease` / `TestShutdown_InteractiveRunSharesInflightBudget` / `TestShutdown_InteractiveRejectedImmediatelyWhenFull` / `TestShutdown_JoinBoundUsesRemainingGrace`；DoD：停止领取后新领取 0 条、释放晚于收尾退出、`pg_stat_activity` 连接最后消失、join 使用剩余 grace 预算而非再等完整 `cancel_wait`。

## Phase 9: User Story 7 - 内容寻址资产、生命周期代次与准入 fail-closed (P2)

**Goal**: 资产按内容寻址不可变；生命周期代次与实例版本在准入处 fail-closed。

**Independent Test**: bundle 往返与截断拒收；bundle 缺失 / 不符、代次回退、同代次异内容、供应商实例版本不符、MCP 未应用版本、凭据换代的六个准入拒绝 / 重建路径；旧 Run 目录不漂移。

- [x] T027 [P] [US7] 新建 `internal/assets/bundle.go`（`Manifest` / `Installed` / `Build` / `Export` / `Verify` / `Install` / `Resolve`）与 `internal/assets/command.go`（`CommandRoot`）：manifest + 逐文件 SHA-256 + `BundleHash`，安装到 `<bundle_root>/bundles/<agent>/<bundle_hash>` **不可变**目录，已存在只做逐文件比对不重写，截断 / 损坏拒收且不破坏原目录，路径逃逸拒绝；在 `internal/assets/bundle_test.go` 落 `TestAssets_BuildInstallVerifyRoundTrip` / `TestAssets_TruncatedBundleLeavesOriginalIntact` / `TestAssets_OldRunUsesPinnedHashAfterNewInstall` / `TestAssets_InstallResolveAndExecutionUseSamePath`；DoD：`Install` 返回 `Dir`、`Resolve` 返回值、执行期读取路径三处逐字一致且都含 `bundles/` 段、新增包覆盖率按 `core` 口径 ≥70%。
- [x] T028 [US7] 在 `internal/core/resources.go` 与 `internal/storage/postgres/resource_store.go` 落 `agent_bundle` 分支（`validateResourceKind` / `normalizeResourceVersion` 按 kind 分派，`agent_bundle` 走 `core.ParseAgentUID`、`resource_id` 与规范化 AgentUID 同值、复用 skill 支的 manifest + storage_path）与 `PutLifecycleIndex` 同代次仅接受逐项完全一致内容、在 `internal/core/lifecycle.go` 的 `LifecycleIndex` 端口新增 `ReadLifecycleIndex`（代次期望类型同文件）、发布指针读取走 `resource_pointers`（P-19 / P-09）；在 `internal/storage/postgres/resource_store_test.go` 追加 `TestResourceStore_EnsureVersionAgentBundle`（`ag_` UID 经真实仓储 API 插入、二次 `inserted=false`）与 `TestResourceStore_RejectsCrossKindUID`（`agent_bundle` + `rs_`、`skill` + `ag_` 都拒绝），在 `internal/app` 落 `TestLifecycleIndex_SameGenerationOnlyIdenticalWrite` / `TestLifecycleIndex_StaleWorkerCannotRollback`；DoD：旧三类行为不变、代次只增不回退、同代次异内容拒绝且零写入。
- [x] T029 [US7] 在 `internal/app/assets.go` 落 `assetVerifier`（期望 hash 来自 `resource_pointers` + `resource_versions.content_hash`，不是 `config_snapshots`），在 `internal/core/provider.go` / `internal/core/resources.go`（`MCPHandle` / `CredentialPin` 只读访问器）/ `internal/provider/service.go` / `internal/app/resources.go` 落 Provider / MCP 本地实例版本证明（`BuiltVersion()` **两段**：`core.ResourceVersionRef` + `core.CredentialPin`，P-21；Provider 对 `published_version_id`、MCP 对 `applied_version_id`，不一致按同一目标版本重建-换入-再比，仍不符 503 `RESOURCE_VERSION_MISMATCH`；准入**不**隐式执行 MCP apply；事务内按 kind 复查同一列 + 同一份 pin 未漂移），在 `internal/app/baseline.go` 落本地代次超前不写索引并标不可准入（P-17），`internal/app/wire.go` 资产自举改从共享发布记录构建；在 `internal/app/assets_admission_test.go` 落 `TestAdmission_RejectsAssetMismatch` / `TestAdmission_RejectsStaleLifecycle` / `TestAdmission_RejectsStaleProviderInstance` / `TestAdmission_RejectsPublishRace` / `TestAdmission_PinsMCPAppliedVersion` / `TestAdmission_RejectsStaleCredentialPin`，并落 `TestBaseline_LocalGenerationAheadDoesNotWriteIndex` / `TestStartRunRejectsLifecycleExpectationMismatch`；DoD：六个准入拒绝 / 重建路径全绿、旧 Run 固定在原 hash 目录、无 Run 行产生、`asset_bundle_hash` 写入清单。
- [x] T030 [US7] 在 `internal/cli/assets.go` 落 `basalt assets export|install|verify`、`internal/cli/workers.go` 落 `basalt workers list`（只读：instance / name / started / heartbeat / active leases），在 `internal/web` 落运行视图三个只读字段（`worker_instance_id` / `ownership_epoch` / `asset_bundle_hash`）、系统概览 Worker 列表与活跃租约计数、另外 4 个错误码映射（`FLOW_ROOT_BUSY` 已在 T023），在 `assets/console` 落运行详情与概览字段并重建 `dist` 与 `integrity.json`；在 `internal/cli/assets_test.go` 落 export / install / verify 往返、`internal/web` 与 `assets/console/tests` 补对应用例；DoD：只增字段不改既有语义、不新增左侧入口、`npm --prefix assets/console run build` 全绿、CLI 出错非空且不回显文件内容。

## Phase 10: User Story 8 - 停机迁移、升级兼容与依赖纪律 (P3)

**Goal**: v11 库停机迁移后三类回填租约被首轮回收收尾；零新增依赖。

**Independent Test**: v11 隔离 schema 构造三类在途行与两类关闭态根，迁移后断言回填与不回填各就位、版本 12；首轮回收收尾三类行；`go.mod` / `go.sum` 空 diff。

- [x] T031 [US8] 在 `internal/storage/postgres/migration_0012_test.go` 追加 `TestUpgrade_StopTheWorldLeaseBackfill`：构造 v11 库的三类在途行（`running` Run / `started` 执行 / 非终态 Flow 根）与两类关闭态根，停机迁移后启动新回收器首轮 `ReclaimOnce`，断言三类回填租约都被合法领取并收尾、关闭态根零租约；DoD：不模拟在线混跑、重复迁移幂等无重复租约、收尾只作用于回填行。
- [x] T032 [P] [US8] 在诊断事件写入面（`internal/app` 诊断生产者 / `internal/storage/postgres` 诊断存储）为 `diagnostic_events.attrs` 增加 `worker_instance_id` / `ownership_epoch`，只沿用既有 `event_id` / `step_id` / `parent_step_id`，**不得**引入跨节点全局序号或按接收顺序当执行顺序；在 `internal/app` 落 `TestDiagnostics_LeaseAttrsWithoutGlobalSequence`；DoD：既有诊断用例全绿、attrs 只在有租约的 Run / 执行 / Flow 事件上出现、无 `seq` 新字段。
- [x] T033 [P] [US8] 核对依赖与构建纪律：`GOPROXY=https://goproxy.cn go mod tidy` 后 `git diff --exit-code go.mod go.sum` 与起点 `138382f` 空 diff、无新第三方坐标、`CGO_ENABLED=0` 发布构建可产单二进制、`-race` 与发布构建分属不同命令；在 `evals/cases/` 新增 1–2 条行为用例（不调用真实模型、本地桩）验证多 Worker 语义不进入模型评测面；DoD：依赖 diff 为空、评测用例通过 `eval-build` / `behavior`、不注入真实模型凭据。

## Phase 11: Polish & Cross-Cutting Concerns

- [x] T034 在 `internal/app/distributed_integration_test.go` 落跨 Worker 故障装配：两个 App / 两个 Scheduler 共享同一隔离 schema，覆盖接管、失权、会话忙、取消、投递、停机六类场景，作为 `us24-fault` 的主判据；DoD：固定随机种子、不依赖真实时间窗、`-race` 下无泄漏、与 `us24-base` 的单元 / PG 用例不重复定义判据。
- [x] T035 按模块卡 §3.5 更新契约：`docs/contracts/run-and-errors.md` §3.2 / §3.4（归档先立屏障再等待）/ §4.3.1 / §4.3.2 表述与取消标志语义、新增错误码；`docs/contracts/flow.md` 新增 `Isolated` / `Joined` / 未决动作三判据、`TookOver` 分路、`ReconcileTask` / `CancelTask.Isolated`、`CreateRunForNode` 校验；`docs/contracts/agent-md.md` 代次单调守卫；`docs/contracts/openapi.yaml` 只读字段与 5 个错误码；DoD：`git diff --exit-code docs/contracts/schema.pg.sql` 为空、固定字面量与实现逐字一致、只增不改既有分类与状态码。
- [x] T036 按模块卡 §3.5 更新文档：`docs/TechnicalDesign.md` 追加"分布式执行增量（US-24（A））"并修订 §5.2 / §6.5；`docs/Requirements.md` 追加 §2.4.1 五条验收；`docs/Operations.md` 增加多 Worker 前提、Worker / 租约排障、资产导出安装、停机顺序、停机迁移升级与回滚边界；`docs/DevelopmentLoop.md` §7 新增本 US 实测陷阱（至少：租约时间用本地时钟、启动全局恢复、回收越界、资产未校验，每条带"怎么发现"与"怎么复核"）；DoD：逐字按 §2.4 提案、历史记录不改、不把未实现能力写成现状、markdownlint 零诊断。
- [x] T037 跑全量门禁：`make check` 七目标、`sh scripts/check-acceptance.sh --stage us24-base`、`sh scripts/check-acceptance.sh --stage us24-fault`、`test-pg` 与 `-race` 分开命令；DoD：逐目标四态登记、`required_tests` 全部具名 PASS 且无 SKIP / 空匹配、覆盖率不低于底线（`core` / `storage` ≥70%、新增 `internal/assets` ≥70%、`internal/app` ≥50%）、`CGO_ENABLED=0` 发布构建通过、`git diff go.mod go.sum` 为空。
- [x] T038 执行模块卡 §6.1 的 14 项本机双进程人工验证（两个 `serve` 进程、各自工作区、同一 `BASALT_TEST_PG_DSN`）：并发领取 / 崩溃接管 / 暂停恢复 / 同会话恢复期窗口 / 取消路由 / 投递未知 / 资产不符 / Flow 接管 / 停机顺序 / 升级 / 数据库中断 / 时钟变化 / 响应丢失与事件重投 / 网络分区；逐项记录命令、时间与关键查询输出；DoD：十四项逐项"注入方式 / 实际结果 / 是否重放"、真实双宿主与 DB HA 保持 `未测量` / 未授权、不把本机双进程写成真实跨机证据。
- [x] T039 收口三件套：复跑 `/speckit-analyze`（CRITICAL 清零）、独立上下文复核（findings 与未覆盖面逐条处置）、七项证据写入 `specs/024-state-distributed/acceptance.md` 与 run-state；DoD：四态登记、变更总结四段（改动点 / 重点 review 清单 / 判据面变更 / 如何验证）、判据面逐路径核对（Q3 的等价替换给裁决记录，无其他变更记"判据面零变更"）、并发正确性与真实环境项保持 `未测量` / `未验证`、输出人工 review ② 收口包，禁止代签 / 自动提交。

## Check 映射（stage `us24-base` / `us24-fault` 派生依据）

| 任务 | check |
| --- | --- |
| T001 | —（流程证据：run-state 基线） |
| T002 | 全部 check（stage 自身） |
| T003 | `us24-base-pg` |
| T004 | `us24-base-core` |
| T005 | `us24-base-pg`、`us24-base-race` |
| T006 | `us24-base-pg`、`us24-base-cli` |
| T007 | `us24-base-core`、`us24-base-app` |
| T008 | `us24-base-pg`、`us24-base-app` |
| T009 | `us24-base-pg` |
| T010 | `us24-base-pg`、`us24-base-app` |
| T011 | `us24-base-core` |
| T012 | `us24-base-pg` |
| T013 | `us24-base-app`、`us24-base-callers` |
| T014 | `us24-base-core` |
| T015 | `us24-base-pg` |
| T016 | `us24-base-core`、`us24-base-app` |
| T017 | `us24-base-pg`、`us24-base-app` |
| T018 | `us24-base-pg`、`us24-base-app` |
| T019 | `us24-base-pg`、`us24-base-app` |
| T020 | `us24-base-core`、`us24-base-app` |
| T021 | `us24-base-pg` |
| T022 | `us24-base-pg`、`us24-base-app` |
| T023 | `us24-base-pg`、`us24-base-app`、`us24-base-web` |
| T024 | `us24-base-pg`、`us24-base-app` |
| T025 | `us24-base-app` |
| T026 | `us24-base-app`、`us24-base-race` |
| T027 | `us24-base-assets` |
| T028 | `us24-base-pg`、`us24-base-app` |
| T029 | `us24-base-app` |
| T030 | `us24-base-cli`、`us24-base-web`、`us24-base-console-test`、`us24-base-console-build` |
| T031 | `us24-base-pg` |
| T032 | `us24-base-app` |
| T033 | `eval-build`、`behavior` |
| T034 | `us24-fault-pg`、`us24-fault-app`、`us24-base-race` |
| T035 | —（契约修订；`docs/contracts/*` 在 protect 内） |
| T036 | —（文档修订） |
| T037 | 全部 check + `make check` 七目标 |
| T038 | —（人工双进程 G5，不入 stage） |
| T039 | —（收口三件套） |

## 依赖与执行顺序

- Phase 1 → Phase 2 阻塞所有故事：T003 → T004（可并行）→ T005；T003 约束名实测先于 DDL 落地。
- US1 依赖 T005：T006 → T007；T008 与 T009 同存储包不同文件可并行；T010 依赖 T005 / T008。
- US2 依赖 T010 的终态与 `RunStore` 签名：T011 → T012 → T013。
- US3 与 US2 同包（`internal/core` / `postgres`），按 T014 → T015 → T016 顺序推进；T014 的 `CronEngine` 变更先于 `scheduler_test.go` 改写。
- US4 依赖 T005 / T010 / T007：T017 → T018 → T019（T019 依赖 T009）；T017 的 `subtree_gate.go` 同时被 US5 复用。
- US5 依赖 T017 的判据与 T021 的存储条件：T020 → T021 → T022 → T023。
- US6 依赖 T007 的续约观察与 T012 的准入：T024 → T025 → T026。
- US7 与 US5 / US6 不同包，T027 可并行；T028 → T029 → T030（T030 依赖 T023 的 409 映射与 T027 的 CLI 入口）。
- US8 依赖 T003 / T017：T031 → T032 → T033。
- 收尾 T034–T039 依赖全部实现任务；T039 之前不得宣称完成，人工 review ② 之前保持"待人工收口"。

# Implementation Plan: 状态外置与分布式执行（租约归属、fencing、触发去重与跨 Worker 恢复）

**Branch**: `024-state-distributed` | **Date**: 2026-11-06 | **Spec**: [spec.md](spec.md)

**Input**: US-24 模块执行颗粒度文档 R7 定稿（review ① 通过，2026-11-06；3385 行，SHA-256 `02e9f9418b6bca0616ac01c0b941cc4f23864301317396d25ba0fa3fbe934395`）。用户明确「R7 设计评审通过……更新指纹后定稿；不重议 Q1–Q21，不再启动整轮设计复核。并发正确性留到实施阶段用真实 PostgreSQL 用例验证，不记为已验证」。本批准**不代表实现或测试已通过**；并发正确性在取得真实 PG 用例读数前一律 `未验证`。

## Summary

把单机已成立的不变量原样提升到多 Worker：每次执行（交互 / 调度 Run、一条定时执行行、一个 Flow 根）在**同一张 `execution_leases` 表**取得唯一执行权，带负责人、单调归属代次与数据库时间到期；受归属保护的写入统一经 `FencedTx`（同一事务先锁租约行、再校验负责人 + 代次 + 未过期），续约连续失败达到 `ttl - renewInterval` 即有界停止。Run 准入的权威判定（调用方三段并发、同会话活跃租约、实例上限、发布 / 生命周期拦障、任务次数记账）全部收进 `StartRun` 单事务，失败整笔回滚。调度器从进程内 cron 定时器改为数据库到期扫描：`(task_id, planned_at)` 唯一、`next_fire_at` CAS、同任务单 `started` 执行、错失只记一条 `skipped`。回收是独立有界循环，只领取已过期 / 已释放租约，按作用域收尾 A 类三域与 B 类两域，启动不再做全局恢复扫描。Flow 根推进前领取根租约，`TookOver=true` 必须先 `Recover` 再 `Advance`；跨 Worker 用三个独立判据（`Isolated` 放行查证、`Joined` 放行推进、未决动作命中则 Abandon）。取消写 `runs.cancel_requested` 由属主观察；归档先立准入屏障再发取消。资产以内容寻址 bundle 导出 / 安装 / 校验，执行只读固定 hash 目录，准入对 bundle、生命周期代次与 Provider / MCP 实例版本 fail-closed。迁移 `0012` 停机前向追加，三类在途行回填"遗留负责人 / 代次 0 / 已过期"租约。零新增依赖、默认同步、无第二执行路径、模型 / 工具不自动重放。

## Technical Context

**Language/Version**: Go 构建版本由 go.mod toolchain 指令与 CI setup-go 固定（当前 go1.27.3；go 行 go 1.27 仅为最低要求，本机基础安装 1.27.2 不是构建版本，2026-10-27 实测）；单 module；发布 `CGO_ENABLED=0`。

**Primary Dependencies**: 技术栈固定句（逐字照抄，本 US 零新增坐标）：`Go 构建版本由 go.mod toolchain 指令与 CI setup-go 固定（当前 go1.27.3；go 行 go 1.27 仅为最低要求，本机基础安装 1.27.2 不是构建版本，2026-10-27 实测）+ 单 Go module（cmd/basalt + internal/ 分包）+ spf13/cobra v1.10.2 + goccy/go-yaml v1.19.2 + sashabaranov/go-openai v1.43.0（只做协议调用，禁自动工具循环）+ modernc.org/sqlite v1.60.1 + robfig/cron/v3 v3.0.1（6 段：cron.NewParser(Second|Minute|Hour|Dom|Month|Dow)，每任务时区经 CRON_TZ= 前缀）+ modelcontextprotocol/go-sdk v1.8.0 + fsnotify v1.10.1 + 标准库 net/http + ServeMux + log/slog + testify v1.12.1 + [测试专用] github.com/santhosh-tekuri/jsonschema/v6 v6.0.3（仅 *_test.go 导入；必要传递依赖如 golang.org/x/text 以 go mod tidy 实测为准，运行时不进发布二进制）；凭证 ${ENV_VAR} 占位；不用 ORM（手写 SQL + schema.sql 幂等）；CGO_ENABLED=0；本技术栈句与已批准计划内的依赖直接落地、表外新增坐标或版本变更先问用户；GOPROXY=https://goproxy.cn；写坐标前先 go list -m -versions 核实`。本 US 追加：US-16 的 `github.com/jackc/pgx/v5` v5.11.0（`database/sql` stdlib 模式）与 US-11 的 Vue 3 + Vite + TypeScript + Vitest（版本以 `assets/console/package-lock.json` 为准）沿用；**零新增 Go / npm 坐标**——哈希用 `crypto/sha256`、时间用 `time`、随机标识用 `crypto/rand`。`go.mod` / `go.sum` 与本 US 起点（`138382f`）**空 diff**。

**Storage**: PostgreSQL v11 → v12，只追加 `migrations/0012_distributed_execution.sql`（`worker_nodes` / `execution_leases` / `lease_events` 三张新表；`scheduled_tasks.next_fire_at`、`task_executions.planned_at|owner_worker_id|ownership_epoch`、`schedule_deliveries.execution_id`、`runs.cancel_requested` 四组列；`resource_versions` / `resource_pointers` 的 `resource_kind` CHECK 扩到 `agent_bundle`；三类在途行回填"legacy / 0 / 已过期"租约）；`SupportedSchemaVersion = 12`；`docs/contracts/schema.pg.sql` 冻结基线不改。SQLite 仅随 `core.RunStore` 签名（`StartRun` / `FinishRun` / `CompleteRunCloseout`）与既有接口适配，不新增协调语义。

**Testing**: 按模块卡 §3.2 / §4：单元（`internal/core/lease_test.go`、`scheduler_test.go` 改写、`internal/assets/bundle_test.go`）、PG 集成（`postgres/{lease_store, worker_store, run_lease, schedule_coord, reclaimer, flow_lease, resource_store(追加), migration_0012}_test.go`）、装配（`internal/app/{distributed_integration, assets_admission, shutdown, archive, learning_recovery(改写)}_test.go`）、CLI（`internal/cli/assets_test.go`）、限额（`internal/callers/limiter_test.go` 追加）、行为评测（`evals/cases/` 1–2 条）。全部使用本地协议桩与冻结夹具、本机 PostgreSQL 与回环，**不调用真实模型、不注入真实凭据、不连外网**。阶段派生 `harness/stages/us24-base.json` 与 `us24-fault.json`；`make check` 七目标仍是完成的定义。

**Target Platform**: Windows 11 + Git Bash 开发机；Linux/amd64 发布目标；本 US 只验本机双进程 / 双 App 共享同一 PostgreSQL schema，不验真实双宿主。

**Performance Goals**: 无跨节点时延 SLA。接管上界由参数决定：租约到期（默认 TTL 30s）+ 回收轮询（默认 5s，批量 32）+ 领取轮询；续约预算 `ttl - renewInterval`（默认 20s）内必有界停止；停机宽限默认 45s（≥ Flow 停机宽限 + 5）。真实双宿主接管延迟 `未测量`（Q17，归 US-25）。

**Constraints**: 默认同步执行，只新增模块卡冻结的四个有界维护循环（到期扫描 / 续约 keeper / 回收 / 登记心跳），新增第五个须重新裁决（Q1）；不新增执行 / 消息 / 审计路径；不自动重放或补偿未知副作用；QPS 明确为每 Worker 软限额（Q5）；不改写冻结基线或已发布迁移；`go.mod` / `go.sum` 空 diff；台账 / docs 正文为私有面，不入 Git 变更。

**Scale/Scope**: 每 Worker 一份维护循环；回收每轮条数有界（默认 32，总时长有界）；到期扫描每轮批量默认 32 且 ≤ `max_inflight`；总在途 goroutine 默认 64；多 Worker 数量在本 US 只覆盖本机双进程（自动化）与本机双进程（人工 G5）。

## Constitution Check

*GATE: 实施前与设计后各核对一次。*

| 原则 | 本次落实 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct | 不触碰循环体与工具调度；回收 / 到期扫描只做存储与收尾，不执行模型或工具 | 通过 |
| II 客户端只做协议调用；唯一消息仓储 | 消息追加仍由既有路径写入，只是统一经 `FencedTx` 加归属校验；`ReActLoop` 仍是唯一工具调度者；不新增第二套追加路径 | 通过 |
| III Provider 显式映射 | 准入复用 Run 启动时固定的 provider 映射；`StartRun` 事务内只读数据库复查发布 / 生命周期，不遍历推断；Provider / MCP 实例版本按 `published_version_id` / `applied_version_id` 显式比对 | 通过 |
| IV 一目录一 Agent；技能声明式绑定 | 资产 bundle 只固定 Agent 配置 + 技能版本清单（`SkillVersionRef`），不新增绑定机制、不预载正文、不新增 `use_skill`；执行只读 manifest 固定目录 | 通过 |
| V 审计 day-one fail-closed | 审计 `pending` 预写与终态回填都纳入 `FencedTx`；回收的审计归一化只作用于合法领取的 `run_id`；无租约的 `pending` 只报告不改写 | 通过 |
| VI 授权与执行分离 | 不触碰 `Policy` / `CommandRunner`；回收不执行任何命令 | 通过 |
| VII 同步 + 有界后台例外 | 只新增 Q1 冻结的四个有界维护循环（到期扫描 / 续约 keeper / 回收 / 登记心跳），有界批次与停机顺序；不新增持久任务类型；`context.WithValue` 不承载业务状态，租约用显式 `LeaseToken` 参数 | 通过 |
| VIII 单二进制 | 零新增 Go / npm 坐标；`CGO_ENABLED=0`；`-race` 与发布构建分属不同命令 / job；console 只加只读字段 | 通过 |
| IX 手写 SQL、迁移只追加 | 新增 `0012_distributed_execution.sql`；`schema.pg.sql` 冻结基线不改；`0001`–`0011` 不改写；约束名先按隔离 schema 实测再定 | 通过 |
| X 五大能力优先、范围卡紧 | 只交付已批 US-24 范围；数据库 HA、S3 / PVC / NFS、Compose / K8s、端到端 exactly-once、自动重放全部明确不做 | 通过 |

**依赖方向**：`core` 只新增纯类型 / 接口 / 哨兵（`lease.go`；`store.go` / `task.go` / `scheduler.go` 的签名改造），不 import 其他 `internal` 包；`flow` 依赖 `core` 的 `LeaseStore` / `SubtreeGate` 端口；`assets` 只依赖 `core` 与标准库；`storage/postgres` 实现仓储、判据与迁移；`app` 是唯一装配层，组合运行准入、回收器与停机编排；`cli` / `web` 只依 `core` 接口。**新增 `internal/assets` 包**（模块卡 §3.1.11），其余包落位不变；`core` 仍只依赖标准库。

**设计后复核**：Phase 1 产物（`data-model.md` / `contracts/README.md` / `quickstart.md`）未引入新依赖、新公开写端点、新动作字面量或第二套执行 / 审计路径；`ReconcileTask` 不新增顶层 `Lease` 字段（只经 `Request.Lease` 携带）；结论维持「通过」，无 Complexity Tracking 项。

## Project Structure

### Documentation (this feature)

```text
specs/024-state-distributed/
├── plan.md              # 本文件
├── research.md          # Phase 0 输出
├── data-model.md        # Phase 1 输出
├── quickstart.md        # Phase 1 输出
├── contracts/README.md  # Phase 1 输出（指向权威契约）
├── spec.md              # 已审规格（review ① 派生）
├── checklists/requirements.md
├── run-state.md         # 过程状态（非设计工件）
└── tasks.md             # Phase 2 输出（/speckit-tasks 生成）
```

### Source Code (repository root)

```text
internal/core/
├── lease.go              # 新增：LeaseKind / LeaseToken / Lease / ReclaimReport / LeaseStore / WorkerRegistry / Reclaimer / SubtreeGate / 哨兵
├── store.go              # 改造：CallerAdmission；CheckAdmission / StartRun / LookupAdmittedRun / FinishRun / CompleteRunCloseout / RequestCancel
├── task.go               # 改造：DueTask / DueExecution；ListDispatchable / SetNextFire / ClaimDueTrigger / RecordMissedTrigger / RecordBusyTrigger / StartExecution / FinishExecution；CronEngine 去掉进程内定时器
├── scheduler.go          # 改造：dispatchOnce / executeClaimed / nextFire；删除 locks / gens / cron handle
├── service.go            # 改造：CheckAdmission → StartRun；keepLease 派生执行 ctx；消息追加经 FencedTx；Cancel 非属主走 RequestCancel
├── learning.go           # 改造：LearningRunInput.Token；三写面带 token
├── lifecycle.go          # 改造：代次期望（期望代次 + state_file_hash）入准入
├── resources.go          # 改造：内建版本证明（BuiltVersion 两段）
└── legacy/               # 无新增；既有类型随签名适配

internal/flow/
├── ports.go              # 改造：AdvanceRequest / ResumeRequest / ClaimNode / CommitNode 带 Lease；ReconcileTask.Isolated；CancelTask.Isolated
├── control.go            # 改造：Service.Identity / Leases / withRootLease；Recover 零值自取；Cancel / Abandon 填 Isolated
├── worker.go             # 改造：Identity / Leases / Gate / ReleaseCtx / LeaseTTL / RenewInterval；TookOver 分路与门 ③
├── revision.go           # 改造：Revise 走 withRootLease
└── execution.go          # 改造：节点领取 / 提交带租约

internal/assets/          # 新增包
├── bundle.go             # Manifest / Build / Export / Verify / Install / Resolve
└── command.go            # CommandRoot（CLI 入口适配）

internal/storage/postgres/
├── lease_store.go        # 新增：Acquire（含 TookOver）/ Renew（行级条件写）/ Release / FencedTx
├── subtree_gate.go       # 新增：HasLiveRunLease / HasInFlightNodeAttempt / HasUnresolvedAction
├── worker_store.go       # 新增：Register / Heartbeat / Stop
├── schedule_coord.go     # 新增：ListDispatchable / SetNextFire / ClaimDueTrigger / RecordMissedTrigger / RecordBusyTrigger
├── reclaimer_store.go    # 新增：ClaimExpired（A 类三域）/ B 类两域 / B3 OrphanPendingExtractions
├── run_store.go          # 改造：CheckAdmission / StartRun / LookupAdmittedRun / FinishRun / CompleteRunCloseout / RequestCancel
├── task_store.go         # 改造：planned_at / owner / epoch 读写；删除 RecoverStartedExecutions
├── flow_store.go         # 改造：租约条件、CreateRunForNode、整棵子树收敛
├── flow_close_store.go   # 改造：Isolated 收敛、CancelTask.Isolated
├── flow_revision_store.go# 改造：Revise 推进类租约条件
├── delivery_store.go     # 改造：Recover(ctx, now, runID)；Prepare 写 execution_id；SettleOrphans 拆 B1 / B2
├── resource_store.go     # 改造：agent_bundle 分支；PutLifecycleIndex 同代次一致；ReadLifecycleIndex
├── learning_store.go     # 改造：三写面 fence；RecoverExtraction 带预期值；OrphanPendingExtractions
├── recovery.go           # 改造：六步算法接收 runIDs / token
├── connect.go            # 改造：SupportedSchemaVersion = 12
└── migrations/0012_distributed_execution.sql  # 新增

internal/app/
├── worker_runtime.go     # 新增：workerRuntime、keepLease、newInstanceID
├── reclaimer.go          # 新增：core.Reclaimer 组合作用域原语 + flow.Service.Recover + B3
├── assets.go             # 新增：assetVerifier 适配器
├── flow.go               # 改造：flowNodeRunner.Joined / Isolated；advanceRoot 门 ③
├── learning_recovery.go  # 改造：共享查证步骤 + 预期值重检
├── resources.go          # 改造：Provider / MCP 实例版本证明与重建
├── wire.go               # 改造：租约 / Worker / 回收器装配；启动改 ReclaimOnce；资产自举
├── run.go                # 改造：启动顺序与停机顺序
├── schedule.go           # 改造：调度器注入 LeaseStore / WorkerIdentity / TTL
├── delivery.go           # 改造：ExecutionID、作用域 Recover、B2
├── concurrency_limits.go # 改造：内存名额表降为预检
├── run_quota.go          # 改造：任务次数记账进 StartRun 事务
├── agent_admin.go        # 改造：归档先立屏障
├── baseline.go           # 改造：生命周期索引同代次一致写入
└── flow_http.go          # 改造：错误透传（ErrFlowRootBusy 不在此映射；409 单处映射在 web 层，T023 裁决）

internal/callers/limiter.go        # 改造：ErrConcurrencyLimited 别名指向 core 哨兵
internal/memory/learning.go        # 改造：token 透传三写面
internal/provider/{client.go, service.go}  # 改造：客户端 / 服务按 Run 租约透传（不新增协议路径）
internal/cli/{assets.go, workers.go, config.go, worker_config.go}  # 新增两条命令 + 三段配置（解析拆分在 worker_config.go）
internal/web/{views.go, handlers_*.go, envelope.go}  # 只读字段、Worker 列表、错误映射
assets/console/                    # 运行视图 / 概览字段与 dist 重建
docs/contracts/{run-and-errors.md, flow.md, agent-md.md, openapi.yaml}  # 增量；schema.pg.sql 不动
harness/stages/us24-base.json / us24-fault.json
```

**Structure Decision**: 沿用单 Go module + `internal/` 分包；新增 `internal/assets`（模块卡 §3.1.11 已列），其余包落位与台账第 24 行及模块卡 §3.1 一致。`core` 只承载纯类型与接口（消费方声明），实现分属 `flow` / `assets` / `storage` / `app`；`app` 仍是唯一装配层，`core` 不 import 其他 `internal` 包。

## Implementation sequence

1. **迁移与存储地基**：`0012_distributed_execution.sql`（先按隔离 schema 实测三条约束名）+ `SupportedSchemaVersion = 12`；`core/lease.go` 全部类型与端口；`postgres/lease_store.go` / `worker_store.go` / `subtree_gate.go`；`migration_0012_test.go`。判据：回填三类 + 两类不回填、版本 12、幂等、`resource_id` 编码等价。
2. **Run 准入与归属保护写入**：`CheckAdmission` / `StartRun`（调用方 advisory lock + 三段并发、会话锁 + 活跃租约、实例上限、发布 / 生命周期拦障、任务次数记账、runs / 清单 / 引用 / 租约同事务）/ `LookupAdmittedRun`；`FencedTx` 接入消息追加、工具结果、恢复补齐、状态推进、审计预写、学习三写面；`FinishRun`（只写终态）与 `CompleteRunCloseout`（引用释放 + 租约最后释放）。判据：接管后旧 owner 全写入面被拒且零行、两种获锁顺序、合法重试不误拒、错误优先级、提交未知回查。
3. **租约 keeper 与有界停止**：`app/worker_runtime.go` 的 `keepLease`（`ttl - renewInterval` 预算、`ErrLeaseRenewDeadline` / `ErrCancelRequested` 取消原因、`stop` 用剩余停机预算而非 `WithoutCancel`）；`core/lease_test.go`。判据：续约持续失败在预算内取消；瞬时失败不误停。
4. **调度到期扫描与触发去重**：`TaskStore` 新方法 + `schedule_coord.go`；`Scheduler` 删除进程内 cron 定时器、`Start` 不再全局恢复；`RunNow` 走 `StartExecution`；`core/scheduler_test.go` 改写。判据：每个 `planned_at` 恰一行、重复领取报错、错失只一条 skipped、同任务单 `started`、CAS 不覆盖。
5. **作用域回收**：`reclaimer_store.go`（A 类三域 + B 类两域 + B3）、`recovery.go` 作用域化、`app/reclaimer.go`、启动改 `ReclaimOnce`；`delivery_store.go` 的 `Recover(ctx, now, runID)` / `SettleOrphans` 拆分；`learning_recovery.go` 共享查证 + 预期值重检。判据：活租约零改动、审计归一化限定 run 集合、B2 只 skipped、B3 只查证、并发续约交错用 `pg_blocking_pids()` 观测。
6. **Flow 根租约与三判据**：`flow/ports.go` / `control.go` / `worker.go` / `revision.go` / `execution.go`；`flow_store.go` / `flow_close_store.go` / `flow_revision_store.go` 的租约条件、`CreateRunForNode`、整棵子树收敛；`app/flow.go` 的 `Joined` / `Isolated` / 门 ③ 与 `advanceRoot`；`flow_http.go` / `web` 的 409 映射。判据：`TookOver` 分路、接管先 Recover、Recover 出错不释放、未决动作 Abandon、Isolated / Joined 独立取值、管理面 `FLOW_ROOT_BUSY`。
7. **取消、归档与停机**：`RequestCancel` 与非属主语义；`agent_admin.go` 屏障前置 + 有界等待 + 回滚；`concurrency_limits.go` / `run.go` 的 `max_inflight` 池化与停机顺序（停止领取 → 续约 → join → 释放 → 关库）。判据：远端取消如实返回、屏障拒绝新 Run、停机断言续约持续与释放顺序。
8. **资产与生命周期准入**：`internal/assets`（Build / Export / Verify / Install / Resolve / CommandRoot）；`resource_store.go` 的 `agent_bundle` 分支与同代次一致写；`app/assets.go` 期望 hash 来自共享发布记录；`resources.go` 的 Provider / MCP 版本证明与重建；`baseline.go` 启动自举只在代次相等时发生。判据：bundle 往返、截断拒收、旧 Run 不漂移、六个准入拒绝 / 重建路径、路径三处逐字一致。
9. **CLI / Web / 管理台 / 契约与运维文档**：`basalt assets export|install|verify`、`basalt workers list`、`worker` / `scheduler` / `assets` 配置与装载期校验；运行视图与概览只读字段、错误码映射；console 重建 dist 与 integrity；`docs/contracts` 四件增量、`docs/TechnicalDesign.md` / `Requirements.md` / `Operations.md` / `DevelopmentLoop.md` §7 增补；`harness/stages/us24-*.json`。
10. **收尾**：逐任务 stage → 完整 stage + `make check` → 收口三件套（analyze 复跑 → 独立复核 → 七项证据）→ 人工 review ②。

## Complexity Tracking

> 无 Constitution 违规项，无需填写。

| 违反项 | 为什么需要 | 更简单方案被拒的原因 |
| --- | --- | --- |
| 无 | — | — |

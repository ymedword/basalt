# Implementation Plan: US-10 定时任务（Scheduler）

**Branch**: `010-scheduler` | **Date**: 2026-10-19 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/010-scheduler/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 10` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us10-scheduler.md](../../docs/us/us10-scheduler.md)（R11，人工 review ① 2026-10-19 通过）§2 / §3 / §4 为准，冲突时以 [docs/TechnicalDesign.md](../../docs/TechnicalDesign.md) 为准（§1.1 / §1.3 / §3.4.2 / §5.2 / §6.3 / §6.5 / §7.1.1 / §7.1.2 / §8.2）；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准（`openapi.yaml` 的 4 个 schedules 操作与 `ScheduleView` / `ExecutionView`、`run-and-errors.md` §5 / §6、`agent-md.md` §3 / §5.6、`schema.sql` 的 `scheduled_tasks` / `task_executions`）。设计面已冻结；本 plan 只做落位与验证映射，不引入文档外交付物。

## Summary

给 Basalt 装上**钟推第三触发源**：按各 `AGENT.md` 的 `schedules:` 动态注册 cron 任务，到点生成消息并走**同一个**运行入口 `core.AgentService.Run`；同时交付四个管理端点与第 10 个内置工具 `manage_schedule`。调度器只回答「何时、以谁的身份、发什么消息」——准入、Run 账本、审计、ReAct 循环与消息落库全部复用既有链路。

六件可判卷的价值：**① 单一执行链路**（cron / 手动补跑 / CLI / REST 四类触发共享准入与账本，`trigger` 显式区分 `cron` / `manual`）；**② 定义与状态分离**（定义以文件为准、启停以库为准；重启对账不复活停用任务、不补跑错过触发点）；**③ 运行不重叠**（每任务一把锁，cron 锁失记 `skipped`、手动锁失 409，不排队）；**④ 归属不串线**（任务 id 绑定首次写入的 Agent，跨 Agent 复用一律拒绝，写入只经唯一写入门）；**⑤ 旧回调不复活**（每次注册携带进程内代次，切换后已派发的旧回调零副作用）；**⑥ 会话门禁下沉**（归档判定与在途登记收敛到 `core.AgentService`，cron 绕过 web 层也无法运行归档会话）。

技术路线的八个要点：**① 新包 `internal/adapters/cron`**（robfig v3.0.1 封装：6 段解析、`CRON_TZ=` 只由 `workspace.CronSpec` 追加、句柄化启停与触发点枚举；`core` 只留接口与纯业务逻辑）；**② 调度器本体在 `core`**（`Scheduler` + `TaskStore` 端口；`stateMu sync.RWMutex` 保护判定、写库与句柄切换；每任务 `TryLock`）；**③ 注册代次**（`gens map[string]uint64` + `nextGen`；`fire(taskID, gen)` 在读锁内先比代次再取 `ListTasks` 快照，临界区独立作用域 `defer RUnlock`）；**④ 单事务对账**（`ApplyAgentTasks` 一次事务完成同 Agent 全部 upsert 与退休；`ON CONFLICT ... WHERE agent_name = excluded.agent_name` 做归属守卫；提交成功后才切句柄）；**⑤ 会话门禁唯一归属**（`core.SessionStatusStore` 端口 + `ServiceOptions.Sessions`；`Run` 与 `Archive` 同一把 `mu`；无会话请求完全绕过；删除 `web.Server.admissionMu` / `inflightRuns`）；**⑥ 唯一写入门**（`workspace/schedule_write.go`：`writeMu` 串行化、候选重解析 + 同一套 `validateSchedules`、临时文件 + `os.Root.Rename` 原子替换、只替换 `schedules:` 块、写后刷新私有 claim 基线）；**⑦ 第 10 件工具**（`manage_schedule` 身份只取 `ToolEnv.AgentName`；`set` / `remove` / `list`；失败分类 `invalid_input` / `not_found`）；**⑧ 生命周期**（仅 `serve` / `gateway` 启动；信号上下文只在 `app.Run` 派生一次；`Start` 两阶段、启动失败致命、运行期失败聚合上抛；`heavyRuntime.Close` 顺序 调度器 → MCP → DB → 锁）。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）。本轮实测 `go version go1.27.2 windows/amd64`。

**Primary Dependencies**: **运行时零新增坐标**。`github.com/robfig/cron/v3 v3.0.1` 已在 `go.mod:10`，本模块只消费其 `cron.NewParser(cron.Second|cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow)`；**不含** `parser.WithLocation`（v3.0.1 不存在，技术方案已实测），任务时区一律经 `CRON_TZ=` 前缀。**唯一新增是测试专用坐标** `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`（Draft 2020-12、`AssertFormat()`，只被 `*_test.go` 导入）；其自身 `go.mod` 声明 `golang.org/x/text v0.14.0`，为构建测试包可随 `go mod tidy` 以 `// indirect` 进入。判据：`git diff go.mod` 的 `-` 行零命中；`+` 行只属于 `{santhosh-tekuri/jsonschema/v6, golang.org/x/text}`；`go list -deps ./cmd/basalt | grep santhosh-tekuri` 零命中；生产包零导入。

**Storage**: **零迁移、`schema.sql` 零 diff、零新增表 / 列 / 索引**。首次完整读写既有 `scheduled_tasks`（12 列：`task_id` PK / `agent_name` / `cron` / `zone` / `message` / `enabled` / `definition_hash` / `last_run_at` / `last_status` / `retired_at` / `created_at` / `updated_at`）与 `task_executions`（10 列：`id` / `task_id` FK / `run_id` / `status` / `trigger` / `duration_ms` / `error_message` / `started_at` / `finished_at`；索引 `idx_task_executions_task(task_id, started_at DESC)` 已存在）。新增 `internal/storage/task_store.go` 承载全部 9 个方法（`ListTasks` / `GetTask` / `StartExecution` / `FinishExecution` / `RecordSkipped` / `MarkTaskRun` / `ApplyAgentTasks` / `Executions` / `RecoverStartedExecutions`）；任务两表 SQL **只**允许出现在该文件。`run_store.go` 的 `triggerFor` 扩为 `triggerFor(channel, explicit string)`：`explicit != ""` 时只接受 `channel=scheduler` 且值为 `cron` / `manual`，其余组合拒绝落账；空 `explicit` 保持既有四渠道映射。判据：`git diff --stat -- internal/storage/schema.sql docs/contracts/schema.sql` 空输出；`internal/storage/migrations/` 零新增。

**Testing**: 默认层 `go test -race ./...`——**新增 8 个测试文件 120 个函数 / 227 条用例**（`core/scheduler_test.go` 46/79、`adapters/cron/cron_test.go` 9/14、`storage/task_store_test.go` 14/38、`workspace/schedule_write_test.go` 12/24、`tool/manageschedule_test.go` 10/15、`web/handlers_schedules_test.go` 12/30、`app/schedule_test.go` 15/23、`app/crossentry_test.go` 2/4）+ **修改 7 个测试文件 +27 条**（`core/service_test.go` +11、`storage/run_store_test.go` +5、`web/handlers_test.go` +4、`web/contract_test.go` +4、`web/web_test.go` +2、`app/ports_test.go` +1、`web/server_test.go` 仅编译桩）= **254**；§4 判卷并集 **255**（含既有 `TestRun_HeavyCommandSplit` 零改动直接进 check）。**默认层不访问外部网络、不依赖真实时钟**：存储用临时目录 + 真实 SQLite；调度器用 fake engine / fake invoker / fixed clock；跨入口用例用进程内真实 `AgentService` + 真实 SQLite，`Fire` 一律**异步**（`go scheduler.Fire` 或 fake engine 立即返回），HTTP 用 `httptest`。**真实进程 / 真模型 / 真时区 DST / 重启恢复**按颗粒度文档 §6.2 在 Loop 收尾以临时工作区人工执行，Windows 的 SIGTERM 与真实 DST 用例记「未验证」并说明原因。覆盖率：`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%，由既有 `make cover` 判卷（**不改脚本与阈值**）。关键回归 R1~R31 原样落地，函数名英文、中文验收点以注释保留。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物 `CGO_ENABLED=0` 交叉编译；`-race` 与发布构建**分属不同命令 / job**。调度为纯进程内实现，无平台分片；真实信号验收按平台分支。

**Project Type**: CLI + 服务单二进制。新增一个内部包 `internal/adapters/cron`（技术方案 §7.1.2 的强制落位：第三方 cron 实现放 `internal/adapters/`，`core` 只依赖标准库）；不新增其余顶层目录。

**Performance Goals**: 无吞吐 / 延迟指标（后台调度类 feature）；判据是**行为预算**——定时运行 300s 时限、REST 手动补跑 60s 时限、停机宽限 70s + 有界收尾 5s、执行历史窗口 100 条、错过触发点枚举上限 1000 次。调度不排队：锁失即跳过（cron）或立即 409（手动）。

**Constraints**: 三条包纪律——`core` **只依赖标准库**（cron 封装在 `internal/adapters/cron`）；`web` 只依赖 `core` + 标准库；`app` 是唯一装配层。**单一写入路径**：`manage_schedule` 只经 `AgentLoader.ApplySchedule`；任务两表 SQL 只在 `internal/storage/task_store.go`。**单一执行链**：`scheduler.go` 不出现 `ReActLoop` / `AppendMessage` / `StartRun` / `ProviderClient` / `ToolExecutor`。**审计**：调度器不写审计，Run 与工具调用审计仍由既有入口承担；`run_id` 贯穿三处。**同步执行 + context 纪律**：调度触发同步等待 `Run` 返回；`context.WithValue` 零新增；业务状态显式传参。**会话门禁**：`web` 内 `admissionMu` / `inflightRuns` / `admitRun` / `releaseRun` 删除；`Sessions:` 只在 `wire.go` 接线一次。**生命周期**：`signal.NotifyContext` 在 `internal/cli` 零命中、`internal/app` 恰一处；`Start` 两阶段、启动失败致命、运行期失败聚合。**范围零膨胀**：零迁移、零新增配置键、`basalt.yaml.example` 零 diff、`Makefile` / CI / `scripts/**` 零改动、运行时零新增依赖。**静态门禁**：抑制注释零使用；测试函数名英文；格式只许 `gofumpt -w <本轮改动的文件>`。

**Scale/Scope**: **37 个代码文件**——新增生产 8（`core/task.go` / `core/scheduler.go` / `adapters/cron/cron.go` / `storage/task_store.go` / `workspace/schedule_write.go` / `tool/manageschedule.go` / `web/handlers_schedules.go` / `app/schedule.go`）、修改生产 14（`core/run.go` / `core/store.go` / `core/service.go` / `core/errors.go` / `storage/run_store.go` / `workspace/agent.go` / `tool/doc.go` / `web/server.go` / `web/views.go` / `web/handlers_sessions.go` / `app/wire.go` / `app/run.go` / `app/web.go` / `cli/serve.go`）、新增测试 8、修改测试 7；另加 2 个已确认契约文件（`openapi.yaml` / `agent-md.md`）、`go.mod` / `go.sum`、`harness/stages/us10-scheduler.json`。13 个专属 check（`010-core-scheduler` / `010-core-sessiongate` / `010-core-runid` / `010-cron-adapter` / `010-storage-tasks` / `010-workspace-writegate` / `010-tool-manageschedule` / `010-web-schedules` / `010-web-contract-schema` / `010-app-schedule` / `010-app-tools` / `010-app-crossentry` / `010-race`）。前序改造点 16 项；判据面变更仅 §3.5 表中的六处（十名工具字面量、九→十重命名、trigger 两例、归档重排、server 编译桩、contract 纯追加），既有断言零删除、零跳过、零放宽。

**环境实测读数（2026-10-19，本轮当场命令）**: 分支 `010-scheduler`；起始 commit `fc3c59ebba75e53560ed185f298575567ea8a3bf`；工作区已有 `M docs/contracts/agent-md.md`（sha256 `7407c507…`）与 `M docs/contracts/openapi.yaml`（sha256 `f1f40d1e…`）——均经人工 review ① 确认，**保留不覆盖**。核对属实：`RunRequest` 在 [internal/core/run.go:37](../../internal/core/run.go#L37)（无 `RunID` / `Trigger` / `RequireExistingSession`）、`NewRunID` 在 [:133](../../internal/core/run.go#L133)；`ServiceOptions` 在 [internal/core/service.go:57](../../internal/core/service.go#L57)（无 `Sessions`）、`AgentService.Run` 在 [:105](../../internal/core/service.go#L105)、`admit` 在 [:231](../../internal/core/service.go#L231)；`RunRecord` 在 [internal/core/store.go:187](../../internal/core/store.go#L187)、`RunStore` 接口在 [:44](../../internal/core/store.go#L44)；`SessionBusyError` / `InstanceFullError` / `AgentNotFoundError` 在 [internal/core/errors.go:95](../../internal/core/errors.go#L95) / [:106](../../internal/core/errors.go#L106) / [:83](../../internal/core/errors.go#L83)；`DeriveSessionID` 在 [internal/core/sessionid.go:37](../../internal/core/sessionid.go#L37)。storage：`StartRun` / `FinishRun` / `triggerFor(channel)` 在 [internal/storage/run_store.go:54](../../internal/storage/run_store.go#L54) / [:84](../../internal/storage/run_store.go#L84) / [:180](../../internal/storage/run_store.go#L180)；两表与索引在 [internal/storage/schema.sql:181](../../internal/storage/schema.sql#L181) / [:196](../../internal/storage/schema.sql#L196) / [:208](../../internal/storage/schema.sql#L208)。workspace：`NewAgentLoader` / `LoadAll` / `parseFrontmatter` / `validateSchedules` / `validateScheduleEntry` 在 [internal/workspace/agent.go:116](../../internal/workspace/agent.go#L116) / [:139](../../internal/workspace/agent.go#L139) / [:422](../../internal/workspace/agent.go#L422) / [:692](../../internal/workspace/agent.go#L692) / [:729](../../internal/workspace/agent.go#L729)。web：`Options` / `RunService` / `SessionStore` 在 [internal/web/server.go:24](../../internal/web/server.go#L24) / [:63](../../internal/web/server.go#L63) / [:69](../../internal/web/server.go#L69)；`admissionMu` / `inflightRuns` 在 [:120-121](../../internal/web/server.go#L120-L121)，`admitRun` / `releaseRun` / `archiveSession` 在 [:382](../../internal/web/server.go#L382) / [:399](../../internal/web/server.go#L399) / [:413](../../internal/web/server.go#L413)；`runView` 在 [internal/web/views.go:204](../../internal/web/views.go#L204)（`Trigger` 固定 `triggerAPI` 于 [:210](../../internal/web/views.go#L210)）；归档 handler 在 [internal/web/handlers_sessions.go:217](../../internal/web/handlers_sessions.go#L217)。app：`newRuntime` 在 [internal/app/wire.go:134](../../internal/app/wire.go#L134)、`ServiceOptions` 接线在 [:361](../../internal/app/wire.go#L361)、`declaredToolNames` 九项在 [:428](../../internal/app/wire.go#L428)、`heavyRuntime` 在 [:488](../../internal/app/wire.go#L488)、`Close` 在 [:546](../../internal/app/wire.go#L546)（顺序 MCP → DB）；`runWithRuntime` 在 [internal/app/run.go:244](../../internal/app/run.go#L244)、`runtimeCloser` 在 [:236](../../internal/app/run.go#L236)。cli：`signal.NotifyContext` 在 [internal/cli/serve.go:40](../../internal/cli/serve.go#L40) / [:63](../../internal/cli/serve.go#L63)；gateway 占位文案在 [:61](../../internal/cli/serve.go#L61)。tool：`NewRegistry` / `Names` 在 [internal/tool/registry.go:27](../../internal/tool/registry.go#L27) / [:60](../../internal/tool/registry.go#L60)。`internal/adapters/` 目录当前**不存在**；`declaredToolNames` 恰九项。**本轮未复测的项不在此声称**（**未跑** `make check`、**未跑**覆盖率读数、**未跑**真实进程验收）。

### 文档 ↔ 仓库事实差异（实测发现）

**A. 与文档一致、作为落地基点的仓库事实**（逐条已核，不构成差异）：

| # | 文档写法 | 仓库实测 | 落地口径 |
| --- | --- | --- | --- |
| 1 | §3.1：新建 `internal/adapters/cron` | 目录不存在 | 新建；`core` 不 import 该包，接口由 `core` 声明、`app` 装配 |
| 2 | §3.1：`RunRequest` 追加三字段 | [core/run.go:37](../../internal/core/run.go#L37) 无 | 追加 `RunID` / `Trigger` / `RequireExistingSession`，零值保持既有行为 |
| 3 | §3.1：`ServiceOptions.Sessions` | [core/service.go:57](../../internal/core/service.go#L57) 无 | 追加 `Sessions core.SessionStatusStore`；`nil` 时 `Archive` 显式报错 |
| 4 | §3.1：`RunRecord.Trigger` | [core/store.go:187](../../internal/core/store.go#L187) 无 | 追加 `Trigger string`；既有构造点零改动 |
| 5 | §3.1：`triggerFor(channel, explicit)` | [run_store.go:180](../../internal/storage/run_store.go#L180) 单参 | 扩为两参；`explicit == ""` 保持既有映射 |
| 6 | §3.1：`web.Server` 删除准入字段 | [server.go:120-121](../../internal/web/server.go#L120-L121) 有 | 删除 `admissionMu` / `inflightRuns` 与三方法；归档改经 `RunService.Archive` |
| 7 | §3.1：`runView` 取 `req.Trigger` | [views.go:210](../../internal/web/views.go#L210) 固定 `api` | 改为「取 `req.Trigger`，空则回落 `api`」，既有 REST 零行为变化 |
| 8 | §3.1：`declaredToolNames` 十项 | [wire.go:428](../../internal/app/wire.go#L428) 九项 | 追加 `manage_schedule`（排序序位于 `list_dir` 与 `notify` 之间） |
| 9 | §2.2 约束 24：信号只在 `app` 派生 | [cli/serve.go:40,63](../../internal/cli/serve.go#L40) 两处 | 删除两处 `NotifyContext`；`App.signalContext` 在 `app/run.go` 一处派生 |
| 10 | §3.1：`heavyRuntime.Close` 顺序 | [wire.go:546](../../internal/app/wire.go#L546) MCP → DB | 改为 调度器 → MCP → DB → 锁；未启动时 `Stop` 为 no-op |
| 11 | §3.1：gateway 文案更新 | [serve.go:61](../../internal/cli/serve.go#L61) 占位 | 改为「调度器已启动；通道族归后续 US」；不进任何 IM 通道 |

**B. 实测差异**：本轮逐条核对 §3.1 的签名、行号与既有实现，**未发现文档与仓库的事实差异**。实现期若出现新的签名漂移，按颗粒度文档「停止边界」口径停下登记，不静默自决。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 / 约束 | 本模块落地判据 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | `go.mod` 零 Agent 框架坐标；调度器只调 `RunInvoker.Run` | 通过 |
| II LLM 客户端只做协议调用 / 单一消息仓储 | 调度器不追加消息、不连 provider；消息仍由 `ReActLoop` / `AgentService` 唯一写入 | 通过 |
| III Provider 显式映射 | 本模块不触碰 provider 装配 | 通过 |
| IV 一个目录 = 一个 Agent / 声明式绑定 | 任务只来自 `Profile.Schedules`；无软连接；无 `use_skill` 类新机制 | 通过 |
| V 审计 day-one 且 fail-closed | 调度器不写审计；既有 `ToolExecutor` / 模型调用审计路径不变；`run_id` 贯穿三处 | 通过 |
| VI 授权与执行分离 | 监控调度不引入 Policy / CommandRunner 依赖 | 通过 |
| VII 同步执行 + context 纪律 | 触发同步 `Run`；`context.WithValue` 零新增；业务状态显式字段 | 通过 |
| VIII 单二进制纪律 | 零运行时新增依赖；测试专用坐标不进发布二进制；`CGO_ENABLED=0` 与 `-race` 分列 | 通过 |
| IX 不用 ORM | 手写 SQL 集中在 `task_store.go`；零迁移、`schema.sql` 零 diff | 通过 |
| X 范围卡紧 | 交付物与 §3 的 37 文件逐一对应；不做项见 spec「Out of Scope」 | 通过 |
| 架构约束：依赖方向 | `core` 只依赖标准库（cron 在 `internal/adapters/cron`）；`web` → `core`；`app` 唯一装配层 | 通过 |
| 架构约束：接口定义在消费方 | `TaskStore` / `RunInvoker` / `SessionStatusStore` / `ScheduleEditor` 端口均声明在 `core`（编辑器端口在 `tool` 消费方按既有形态） | 通过 |
| 架构约束：第三方实现放消费方包内或 `adapters` | cron 适配在 `internal/adapters/cron`；SQLite 仍在 `storage` | 通过 |

**Post-Design Re-check**: Phase 1 设计（research / data-model / quickstart）完成后复核，结论不变；无违反项需要 Complexity Tracking。

## Project Structure

### Documentation (this feature)

```text
specs/010-scheduler/
├── plan.md              # 本文件
├── spec.md              # 需求规格
├── research.md          # Phase 0 输出
├── data-model.md        # Phase 1 输出
├── quickstart.md        # Phase 1 输出
├── checklists/
│   └── requirements.md  # 规格质量清单
├── run-state.md         # 过程状态（非设计输入）
└── tasks.md             # Phase 2 输出（/speckit-tasks，不由本命令创建）
```

**不新建 `specs/010-scheduler/contracts/`**：固定字面量的唯一来源是 [docs/contracts/](../../docs/contracts/)（`openapi.yaml` / `run-and-errors.md` / `agent-md.md` / `schema.sql`），复制一份必然漂移。

### Source Code (repository root)

```text
internal/
├── core/
│   ├── task.go                    # 新增：TaskRecord / ExecutionRecord / Trigger / TaskStore 端口
│   ├── scheduler.go               # 新增：Scheduler 本体（Start / Stop / List / SetEnabled /
│   │                              #       Executions / RunNow / ReconcileAgent / CheckOwnership /
│   │                              #       fire / scheduleRun / 代次与锁）
│   ├── run.go                     # 修改：RunRequest 追加 RunID / Trigger / RequireExistingSession
│   ├── store.go                   # 修改：RunRecord 追加 Trigger
│   ├── service.go                 # 修改：ServiceOptions.Sessions、AgentService.Archive、
│   │                              #       Run 的会话门禁与在途登记、无会话绕过
│   └── errors.go                  # 修改：SessionArchivedError / SessionNotFoundError /
│                                  #       SessionGateUnavailableError / ErrTaskBusy / ErrTaskOwnerConflict
├── adapters/cron/
│   └── cron.go                    # 新增：6 段解析器、句柄化 Add / Remove / Start / Stop、
│                                  #       Occurrences（触发点枚举）
├── storage/
│   ├── task_store.go              # 新增：两表 9 个方法 + ApplyAgentTasks 单事务与归属守卫
│   └── run_store.go               # 修改：triggerFor(channel, explicit) 显式触发表
├── workspace/
│   ├── schedule_write.go          # 新增：ApplySchedule / ReadSchedules / writeMu / 原子替换
│   └── agent.go                   # 修改：AgentLoader 持有 writeMu，暴露 root 与 claim 所需面
├── tool/
│   ├── manageschedule.go          # 新增：第 10 件内置工具 manage_schedule
│   └── doc.go                     # 修改：工具清单与包注释同步十件
├── web/
│   ├── handlers_schedules.go      # 新增：四个 schedules handler + ScheduleView / ExecutionView 投影
│   ├── server.go                  # 修改：删除 admissionMu / inflightRuns / admitRun / releaseRun；
│   │                              #       Options 增加 Schedules 端口与路由
│   ├── views.go                   # 修改：runView 取 req.Trigger（空 → api）
│   └── handlers_sessions.go       # 修改：归档改经 RunService.Archive
├── app/
│   ├── schedule.go                # 新增：needsSchedules / 调度器构造 / 编辑器适配器 / 生命周期
│   ├── wire.go                    # 修改：TaskStore / cron 引擎 / Scheduler 装配；十名工具；
│   │                              #       ServiceOptions.Sessions；heavyRuntime.Close 顺序
│   ├── run.go                     # 修改：App.signalContext 唯一信号源；调度启动与失败退出码
│   └── web.go                     # 修改：web.Options 注入 schedules 端口与 RunService.Archive
└── cli/
    └── serve.go                   # 修改：删除自建 NotifyContext；gateway 文案更新

harness/stages/
└── us10-scheduler.json            # 新增：13 个专属 check 的 stage 映射

docs/contracts/
├── openapi.yaml                   # 已确认 diff：run 429 / 504、PUT 400、时间字段 oneOf、归档 400
└── agent-md.md                    # 已确认 diff：§5.6 set 省略 id 的实际 id 定位
```

**测试文件**（新增 8 / 修改 7）见 Testing 段与颗粒度文档 §3.2；`app/crossentry_test.go` 的跨入口用例以**异步** `Fire` 发起，阻塞等待通过 fake engine 的 `fireDone` 通道显式控制，不依赖真实时钟。

## Complexity Tracking

无宪法违反项，无额外复杂度需要说明。唯一新增目录 `internal/adapters/cron` 是技术方案 §7.1.2 的强制落位（第三方 cron 实现不得进入 `core`），不构成偏离。

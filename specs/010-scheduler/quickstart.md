# Quickstart: US-10 定时任务（Scheduler）

**Feature**: `010-scheduler` | **Date**: 2026-10-19 | **Plan**: [plan.md](./plan.md)

本文件是**实现完成后的验证指南**：命令可直接复制执行，期望读数是判据。设计细节见 [spec.md](./spec.md) / [plan.md](./plan.md) / [data-model.md](./data-model.md)；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准。实现尚未落地前，本文件中的命令不可运行。

---

## 1. 前置条件

| 项 | 要求 | 复核命令 |
| --- | --- | --- |
| Go 构建版本 | `go1.27.2`（`go.mod` 的 `go 1.27` 是最低要求，准确版本由 `toolchain` 固定） | `go version` |
| 模块代理 | 显式 `GOPROXY=https://goproxy.cn`（本机直连不可达） | `go env GOPROXY` |
| 分支 | `010-scheduler`，工作区 `D:\code\basalt` | `git branch --show-current` |
| 工具链 | `gofumpt` / `staticcheck` / `gosec` / `govulncheck` 可用（`make check` 需要） | `make check` 首次运行会暴露缺失 |
| 真实模型 Demo（可选） | 工作目录下有 `basalt.yaml`（provider 的 `api_key` 用 `${ENV_VAR}`）且环境变量已设置；`.basalt/` 由 `basalt init` 创建 | `basalt status` 三节均正常 |

> 真实模型 Demo **不是**机器门禁的一部分；缺 key / 缺环境时按四态口径记「未验证」，**不得**记为通过。`ANTHROPIC_*` 等 Claude Code 自身变量不等于 Basalt 的 provider 配置。

## 2. 基线快照（开跑与收尾各一次）

```sh
sha256sum docs/us/us10-scheduler.md specs/010-scheduler/spec.md specs/010-scheduler/plan.md
git rev-parse HEAD
git status --short
```

期望：三件输入的 sha256 与 [run-state.md](./run-state.md) 基线段一致；`git status --short` 只落本 US 允许范围（37 个代码文件 + 两份契约 + `go.mod` / `go.sum` + `harness/stages/us10-scheduler.json` + `specs/010-scheduler/**`）。

契约 diff 只允许已确认内容：

```sh
git diff -- docs/contracts/openapi.yaml docs/contracts/agent-md.md
```

期望：`openapi.yaml` 只含 schedules 四个操作、`ScheduleView` / `ExecutionView`、run 的 400（归档）/ 429 / 504、PUT 的 400、时间字段 `oneOf`；`agent-md.md` 只含 `schedules:` 段与 §5.6 实际 id 定位。**不得**出现 `nullable`。

## 3. 机器验收（逐层）

### 3.1 单包测试（快速反馈）

```sh
go test -count=1 ./internal/core ./internal/adapters/cron ./internal/storage \
  ./internal/workspace ./internal/tool ./internal/web ./internal/app ./internal/cli
```

期望：全部 `ok`；新增用例数与颗粒度文档 §3.2 一致（新增 8 文件 120 函数 / 227 例，修改 7 文件 +27 例，合计 254；并集 255）。

关键回归单独跑：

```sh
go test -count=1 -run '^TestScheduler_' ./internal/core
go test -count=1 -run '^TestAgentService_(Archive_RejectsInflightRun|Archive_Idempotent|Run_ArchivedSessionRejected|Run_RequireExistingSession|StatelessRunsNotSerialized|Archive_WithoutSessionStore)$' ./internal/core
go test -count=1 -run '^TestEngine_' ./internal/adapters/cron
go test -count=1 -run '^(TestApplyAgentTasks_|TestTaskStore_|TestRunStore_TriggerForScheduler$)' ./internal/storage
go test -count=1 -run '^(TestApplySchedule_|TestReadSchedules)' ./internal/workspace
go test -count=1 -run '^TestManageSchedule_' ./internal/tool
go test -count=1 -run '^(TestSchedules_|TestRunView_Trigger|TestPostMessage_ArchivedMapsTo400|TestArchiveSession_ServiceBusyMapsTo409)' ./internal/web
go test -count=1 -run '^TestOpenAPI_ScheduleTimeFields$' ./internal/web
go test -count=1 -run '^(TestNeedsSchedules|TestNewScheduler_|TestScheduleEditorAdapter_|TestRun_Schedule|TestHeavyRuntime_CloseStopsSchedulerFirst|TestScheduleStartExitCode_|TestNewRuntime_ServiceHasSessionGate|TestApp_SignalContextUsesInjectedNotifier)' ./internal/app
go test -count=1 -run '^(TestDeclaredToolNames_IncludesManageSchedule|TestWebOptions_SchedulesWired|TestRun_HeavyCommandSplit|TestToolListerPort_HeavyAssemblyRegistersTenTools)$' ./internal/app
go test -count=1 -run '^TestCrossEntry_' ./internal/app
```

竞态（必须与发布构建分属不同命令）：

```sh
go test -race -count=1 ./internal/core ./internal/adapters/cron ./internal/storage \
  ./internal/workspace ./internal/tool ./internal/web ./internal/app
```

### 3.2 阶段验收（13 个专属 check）

```sh
sh scripts/check-acceptance.sh --stage harness/stages/us10-scheduler.json
```

Windows：

```powershell
.\scripts\check-acceptance.ps1 --stage harness/stages/us10-scheduler.json
```

期望：`010-core-scheduler` / `010-core-sessiongate` / `010-core-runid` / `010-cron-adapter` / `010-storage-tasks` / `010-workspace-writegate` / `010-tool-manageschedule` / `010-web-schedules` / `010-web-contract-schema` / `010-app-schedule` / `010-app-tools` / `010-app-crossentry` / `010-race` 全部 PASS；每个 `required_tests` 真跑（空匹配 / SKIP 不算通过）。

### 3.3 全量门禁

```sh
make check
```

期望：`fmt lint sec test test-integration cover build` 七目标全绿；`make cover` 按包判卷——`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%（本 US 不改阈值）。格式判卷**先查退出码再判输出**。

### 3.4 零增量核查

```sh
git diff --stat -- internal/storage/schema.sql docs/contracts/schema.sql basalt.yaml.example Makefile .github scripts
git diff go.mod
go list -deps ./cmd/basalt | grep santhosh-tekuri || echo "OK: 测试专用坐标未进发布依赖"
grep -rn 'santhosh-tekuri' --include='*.go' internal cmd | grep -v '_test.go' || echo "OK: 生产包零导入"
grep -rnE 'time\.Local' --include='*.go' internal/core internal/adapters internal/workspace || echo "OK: 无 time.Local"
grep -rn 'signal.NotifyContext' --include='*.go' internal/cli || echo "OK: cli 无信号源"
grep -rn 'signal.NotifyContext' --include='*.go' internal/app
```

期望：schema / example / Makefile / CI / scripts 的 diff 为空；`go.mod` 删除行零命中、新增行只属于 `{santhosh-tekuri/jsonschema/v6, golang.org/x/text}`；后续 grep 按注释输出 OK；`internal/app` 恰 1 处 `NotifyContext`（`run.go`）。

## 4. 本机 Demo（手动场景）

以下场景在**临时目录**执行（仓库外），用毕即删。工作区约定：`<dir>/.basalt/` 为工作区，`<dir>/basalt.yaml` 为同级配置（`docs/Operations.md` §1.1 / §6）。

### 4.1 准备

```sh
mkdir -p /tmp/us10-demo && cd /tmp/us10-demo
/path/to/basalt init
cp /path/to/basalt.yaml.example basalt.yaml   # 填入 provider / model / ${ENV_VAR}
# 在 .basalt/agents/<agent>/AGENT.md 的 frontmatter 写 schedules:（见 §4.2）
```

`AGENT.md` 示例片段（6 段 cron；`zone` 缺省 UTC）：

```yaml
schedules:
  - id: heartbeat
    cron: "*/10 * * * * *"
    zone: Asia/Shanghai
    message: "定时心跳测试"
```

### 4.2 场景与期望（对应颗粒度文档 §6.2）

| # | 场景 | 命令 / 操作 | 期望观察 | 需要真实模型 |
| --- | --- | --- | --- | --- |
| 1 | 到点真实触发 | `basalt serve`，等到下一个 10 秒点 | 日志出现触发记录；`runs.trigger = cron`；执行行 `started → completed`；`scheduled_tasks.last_status = completed`；消息落 `channel=scheduler` 会话 | 是（要 `completed`）；无 key 可看到触发与 `failed` |
| 2 | 手动补跑 | `curl -s -X POST http://127.0.0.1:8080/api/v1/schedules/heartbeat/run` | 200，`RunView.trigger = manual`；新增 `trigger=manual` 执行行 | 是 |
| 3 | 两表状态 / 历史 | `curl -s http://127.0.0.1:8080/api/v1/schedules`；`curl -s .../heartbeat/executions` | `last_run_at` / `last_status` 与实际一致；历史按 `started_at` 降序、≤100 | 否（读取即可） |
| 4 | Agent 改自己的任务 | `basalt chat --agent <agent> --message "把 heartbeat 改成每分钟"`，让模型调 `manage_schedule` | `AGENT.md` 的 `schedules:` 被改写；`GET /schedules` 随之变化；工具审计 `pending → 终态` 两行 | 是 |
| 5 | Agent 改他人任务被拒 | 对话中让 Agent 操作其他 Agent 的 id | 工具返回 `invalid_input`；目标 `AGENT.md` 字节不变（`sha256sum` 对照） | 是 |
| 6 | 真实时区到点 | `zone: Asia/Shanghai` 与 UTC 任务并排 | 本地时间到点触发；DST 边界**不要求实跑换季日**——以 R2 固定窗口枚举留证 | 否（触发即可见） |
| 7 | 重启对账 | 停用 → 重启；改 cron → 重启；从 `AGENT.md` 删任务 → 重启；删整个 Agent 目录 → 重启 | 停用保持；新 cron 生效且 `enabled` 不翻转；删除任务从列表消失（`retired_at` 非空）且不再触发；`GET /schedules` 不再出现 | 否 |
| 8 | 停机错过不补跑 | 停服跨过触发点后启动 | 启动日志有 WARN；无补跑执行行 | 否 |
| 9 | 崩溃恢复 | 运行中强杀进程 → 重启 | 遗留 `started` 行按 Run 终态收尾；无对应 Run 的置 `failed` + `run_id = NULL` | 否（但需构造在途运行） |
| 10 | 模式边界 | `basalt gateway`；`basalt chat` | gateway 日志「调度器已启动；通道族归后续 US，本阶段阻塞至信号」且无 HTTP 监听；chat 期间无调度触发 | 否 |
| 11 | 并发互斥 | 快速连点两次 `POST .../run` | 第二次 409；`code = SESSION_BUSY`；message 逐字「该任务上一轮尚未结束，请等待完成或稍后重试」；零执行行 | 是（第一轮需持续运行） |
| 12 | 超时 | 构造超过 300s 的任务 | 执行记 `failed`；进程存活 | 是（耗时 ≥ 300s） |
| 13 | 归档与 cron 相遇 | 归档 scheduler 会话后等到点；再手动补跑；再在途时归档 | cron：`skipped` + 「会话已归档，本轮跳过」、无新 Run、会话仍 `archived`；手动：400 + 「会话已归档，不可追加消息」；在途归档：409 | 否（拒绝发生在模型调用前） |
| 14 | 跨 Agent 复用 id | Agent B 声明 A 已存在的 id（含 A 已退休行）→ 启动 | `Start` 返回错误、不注册（fail closed）；B 不得接管 A 的 `enabled` / `last_*` / 历史 | 否 |
| 15 | 真实进程信号停机 | `basalt serve` 下构造在途定时运行，按 Ctrl+C（Windows 无法投递真实 SIGTERM） | 调度器先停并等待在途运行（≤70s），随后 MCP / DB / 实例锁按序释放；退出码 0；无遗留 `started` 行。拿不到真实进程读数时记「未验证」并注平台原因；进程内测试读数不得冒充真实信号读数 | 是（需要跨 70s 的在途运行） |
| 16 | 真实模型 Demo 凭证与配置模板 | 仓库外建 `/tmp/us10-demo/basalt.yaml`，`api_key` 写 `${ENV_VAR}` 占位并注明「需自行填写 API key」；key 只经单次 shell 环境变量从外部 secrets 文件注入；先跑 `basalt status` 三节 | 模板与仓库均无明文 key（`grep -rnE 'sk-[A-Za-z0-9]{16,}'` 排除文档示例后零命中）；模板不入库；演示结束删除 `/tmp/us10-demo` | 是 |

### 4.3 数据库核对（可选）

若本机有 `sqlite3`：

```sh
sqlite3 .basalt/basalt.db \
  "select task_id, agent_name, enabled, last_run_at, last_status, retired_at from scheduled_tasks order by task_id;"
sqlite3 .basalt/basalt.db \
  "select id, task_id, run_id, status, trigger, duration_ms, started_at, finished_at from task_executions order by started_at desc limit 20;"
```

期望与 API 视图一致；无 Run 的终态行 `run_id` 为 NULL。

## 5. 收尾清理

```sh
cd / && rm -rf /tmp/us10-demo
git status --short   # 临时物不得出现在仓库
```

## 6. 读数如何记录

每个场景按 **通过 / 失败 / 跳过 / 未验证** 四态记入 [run-state.md](./run-state.md) §4 未验证清单（翻转「未验证 → 已验证」须注日期与证据）。缺少真实模型 / 外部环境时记「未验证」，**不得**记为通过；`harness` 全绿不等于人工收口通过。

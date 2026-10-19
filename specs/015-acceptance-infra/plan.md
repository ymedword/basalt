# Implementation Plan: US-15 工程验收基础设施

**Branch**: `015-acceptance-infra` | **Date**: 2026-10-19 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/015-acceptance-infra/spec.md`（sha256 `c2ee6157…`，206 行）

**事实源**：模块执行颗粒度文档 `docs/us/us15-acceptance-infra.md`（人审 ① 所审版本 sha256 `19b70290…`，1322 行）§3 交付物清单与 §4 Harness 为唯一设计依据；本 plan 只派生其已审范围，不新增交付物。

## Summary

把验收从"人记、人跑、人贴结果"改成"机器执行、结构化留证、可复跑核对"：

1. 扩展既有 `scripts/acceptance` 执行器：运行后生成 `record.json`（源码指纹 / 基线提交 / 脏标记 / 平台 / Go 版本 / 被测二进制哈希 / 报告哈希 / 授权），报告日志路径投影为仓库相对路径；层级（`source` / `process` / `external`）由 check 声明决定，缺 `binary_sha256=` 标记即基础设施错误（退出 2，不降级）；external 与授权旗标成对，缺一或多给均退出 2。
2. 新增只读 stage lint（`go test -list` 变换）：现行清单失配 fail-closed，历史 stage 失配只 WARN。
3. 新增 `harness/stages/current.json` 聚合 US-8/9/10 全部专属 check（US-8 一个失配项以显式 `superseded_checks` 映射）+ US-15 四个 check，共 33 项；历史三个 stage 按 LF 归一哈希钉住、零改写。
4. 新增 `internal/e2e` 进程级回填夹具（独立 build tag：`e2e`、`e2e && linux`、`e2e && slow`、`e2e && real`）：构建真实二进制、临时工作区、回环假外部服务，把 US-1~10 甲类人工项全部执行为具名测试；已授权乙类走 `us15-real.json`（`external`，仅收口执行）。
5. 记录与同次报告成对发布到 `harness/acceptance/`，台账只追加；三视图状态（历史曾执行 / 指定版本与平台 / 本次回归）由读取时计算，冲突显式保留。
6. CI 在既有 test job 内插入 lint-stages、current、Linux 信号三步；所有运行目录带 `${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}`。
7. 收口口径（`docs/DevelopmentLoop.md` §5.1 第 6 项）整项替换为"AI 默认执行可判定项并留证，未授权项保持未验证"。

## Technical Context

**Language/Version**: Go 1.27.2（构建版本）；`go.mod` `go 1.27` 为最低版本要求，准确版本由 `toolchain go1.27.2` 与 CI `actions/setup-go` 固定

**Primary Dependencies**: 仅标准库（`encoding/json`、`crypto/sha256`、`os/exec`、`path/filepath`、`runtime`、`strings` 等）用于验收工具；`internal/e2e` 测试复用既有 `internal/testutil` 与产品包，不新增坐标；`go.mod` / `go.sum` 零 diff

**Storage**: 无生产存储变更。运行期证据为文件：`.loop/runs/<run>/report.json` + `record.json` + 逐 check 日志；发布面为 `harness/acceptance/records/<record_id>.json`、`records/reports/<record_id>.json`、`ledger.md`；SQLite schema 零迁移（e2e 仅经产品二进制读库）

**Testing**: `go test -json -count=1` + `required_tests` 精确匹配（子测试写 `Parent/sub`）；build tag 分层——`e2e`（平台中立回填）、`e2e && linux`（信号事件序）、`e2e && slow`（300s 超时）、`e2e && real`（已授权真实外发）；`go vet -tags=e2e,slow,real ./internal/e2e`；无 `make check` 七目标变更

**Target Platform**: Windows 11（本机开发）与 Linux（CI `ubuntu-latest`）；进程级回填场景平台中立；Linux 信号场景只在 Ubuntu job 执行，Windows 记未验证（平台限制）

**Project Type**: 工程验收工具 + 进程级 e2e Harness（单 Go module，非产品功能）

**Performance Goals**: check 超时按模块卡 §4.2——`015-acceptance-infra` 300s、`015-invariants` 180s、`015-backfill` 600s、`015-e2e-vet` 180s、`015-signal-linux` 240s、`015-slow-timeout` 600s、`015-real-model` 900s、`015-real-feishu` 180s；首轮 full 后按实测 ×1.5 校准并记录

**Constraints**: 零新增第三方坐标；`CGO_ENABLED=0` 构建被测二进制；发布记录 / 报告零绝对路径、零凭证；历史 stage 零改写；记录只追加、不覆盖；`make check` 七目标与 `harness/acceptance.json` profile 不变；真实凭证只经运行时环境变量注入

**Scale/Scope**: 现行累计 33 个 check（US-8 6 + 替代 1、US-9 9、US-10 13、US-15 4）；四个 stage 文件；`internal/e2e` 17 个文件（`doc.go` + 16 个 `_test.go`，按模块卡 §3.1.1 表逐行）；回填目录项逐项见模块卡 §2.6 与 §4.5

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 / 约束 | 本 plan 的落法 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | 不新增 Agent 框架坐标；`TestRepoInvariants_NoForbiddenAgentFramework` 机器判卷 | 通过 |
| II LLM 客户端只做协议调用 | 不触碰产品调用面；e2e 只启动产品二进制并按历史行为断言 | 通过 |
| III Provider 显式映射 | 不触碰装配；e2e 的假模型 / 真实 provider 均经产品既有映射 | 通过 |
| IV 一个目录 = 一个 Agent | 不新增绑定机制；回填只使用临时工作区 | 通过 |
| V 审计 day-one 且 fail-closed | 不改变审计路径；e2e 断言 pending 预写与终态更新（历史行为） | 通过 |
| VI 授权与执行分离 | 不触碰 `Policy` / `CommandRunner` | 通过 |
| VII 同步执行 + context 纪律 | 验收工具同步执行；`context` 只承载取消 / 截止；无业务态塞入 | 通过 |
| VIII 单二进制纪律 | e2e 与发布构建均 `CGO_ENABLED=0`；`-race` 仍只在 `make test` 的独立命令；零新增坐标 | 通过 |
| IX 不用 ORM，手写 SQL | 零 schema 迁移、零 SQL 变更 | 通过 |
| X 五大能力优先，范围卡紧 | US-15 已登记台账第 15 行；范围仅为已批准验收基础设施 | 通过 |
| 架构 · 依赖方向 | `scripts/acceptance` 独立开发工具包；`internal/e2e` 以测试文件导入 `internal/testutil`，不引入运行时依赖边 | 通过 |
| 架构 · 凭证纪律 | 真实凭证只经 `BASALT_REAL_*` 环境变量；stage、记录、报告只存 `scheme://host` 与授权文本 | 通过 |
| 开发工作流 · 门禁 | `make check` 七目标不变；`current.json` 是叠加层，不替代七目标 | 通过 |

**结论**：无违宪项，无需 Complexity Tracking 例外。Phase 1 设计完成后复核：记录 / 报告字段、层级声明与发布流程均未引入产品依赖或公共契约变更。

## Project Structure

### Documentation (this feature)

```text
specs/015-acceptance-infra/
├── plan.md              # 本文件
├── research.md          # Phase 0：设计与取舍
├── data-model.md        # Phase 1：记录 / 报告 / stage / 目录 / 台账 / 状态视图
├── quickstart.md        # Phase 1：可复跑验证场景
├── checklists/
│   └── requirements.md  # spec 质量检查清单（已完成）
├── run-state.md         # 过程状态（非设计输入）
└── tasks.md             # Phase 2：/speckit-tasks 生成
```

### Source Code (repository root)

```text
scripts/acceptance/            # 开发工具（不进产品二进制）
├── main.go                    # 修改：新旗标、lint 分支、stage-only、写记录
├── execute.go                 # 修改：accept 返回报告、checkTests 返回 PASS/SKIP、路径投影、层级 / 授权
├── config.go                  # 修改：stage.superseded_checks、check.level 与校验
├── fingerprint.go             # 修改：排除 records/ 与 ledger.md
├── record.go                  # 新增：记录构建 / 校验 / 绑定 / 状态视图 / 授权
├── stagelint.go               # 新增：-list 变换与 stage lint
├── acceptance_test.go         # 修改：签名适配 + 两个新用例（既有断言零删除）
├── record_test.go             # 新增
├── stagelint_test.go          # 新增
├── stages_test.go             # 新增：回填目录与冻结全集、current 零漂移、历史钉住、新 check 钉住
├── records_test.go            # 新增：覆盖 / 绑定 / 三视图 / 发布合规 / 台账 / 指纹
└── invariants_test.go         # 新增：六个仓库不变量

internal/e2e/                  # 进程级回填夹具（仅测试）
├── doc.go                     # 包说明（无 build tag）
├── harness_test.go            # TestMain 构建二进制与进程编排
├── fixtures_test.go           # 假模型 / 回环接收器 / MCP helper / SQLite 读助手
├── bootstrap_test.go          # TestBackfill_Bootstrap
├── agentloop_test.go          # TestBackfill_AgentLoop
├── toolsandbox_test.go        # TestBackfill_ToolSandbox
├── memory_test.go             # TestBackfill_Memory
├── mcp_test.go                # TestBackfill_MCP
├── notify_test.go             # TestBackfill_Notify
├── web_test.go                # TestBackfill_Web
├── lock_test.go               # TestBackfill_CrossProcessLock
├── recovery_test.go           # TestBackfill_RestartRecovery
├── scheduler_test.go          # TestBackfill_Scheduler
├── console_test.go            # TestBackfill_ConsolePipe
├── signal_linux_test.go       # //go:build e2e && linux
├── slow_test.go               # //go:build e2e && slow
└── real_test.go               # //go:build e2e && real

harness/
├── acceptance.json            # 不修改（profile 不变）
├── stages/
│   ├── current.json           # 新增：33 个 check 的现行累计清单
│   ├── us15-linux.json        # 新增：015-signal-linux（process）
│   ├── us15-slow.json         # 新增：015-slow-timeout（process）
│   └── us15-real.json         # 新增：015-real-model / 015-real-feishu（external）
├── acceptance/
│   ├── ledger.md              # 新增：记录索引（只追加）
│   └── records/
│       ├── README.md          # 新增：发布步骤与脱敏要求
│       └── reports/           # 新增：与记录同名配对的已发布报告
├── README.md                  # 修改：新旗标、策略、记录字段、发布与路径投影
└── project.md                 # 修改：项目映射补充

.github/workflows/ci.yml       # 修改：三步插入 + 所有 --out 唯一化
docs/DevelopmentLoop.md        # 修改：§5.1 第 6 项整项替换、§9.3 引用句
```

**Structure Decision**: 沿用单 Go module。验收工具保持在 `scripts/acceptance`（现有工具包，非 `internal/` 产品包）；进程级测试单列 `internal/e2e` 包，避免把真实二进制场景混入 `make test` / `test-integration`。不新建 `contracts/`：记录 / 报告 / stage 的固定字面量由模块卡 §3.3 唯一冻结，落地载体是 Go 结构体 + `harness/README.md` 字段表（待决 D-11 明确不新增 JSON Schema）；再写一份契约文件会形成第二套定义并漂移。`spec.md` 的 FR / SC 已承担行为契约角色。

## Complexity Tracking

> 无违宪项或需例外说明的复杂度。

| Violation | Why Needed | Simpler Alternative Rejected Because |
| --- | --- | --- |
| 无 | — | — |

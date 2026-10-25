# Implementation Plan: US-14 MCP 目录与模板 + 五个每日业务 Demo + 发布收尾

**Branch**: `014-templates-demos-release` | **Date**: 2026-10-25 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/014-templates-demos-release/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 14` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us14-templates-demos-release.md](../../docs/us/us14-templates-demos-release.md)（R3 定稿，2,289 行；R2 于 2026-10-25 经人工 review ① 通过，sha256 `ff6594655edc6f669488d744587d14b6d624c994c9bb2b47b2e58e068d284cc6`）§2 / §3 / §4 / §5 为准，冲突时以 [docs/TechnicalDesign.md](../../docs/TechnicalDesign.md) 为准；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准。D-01–D-10 已冻结；本 plan 只做落位与验证映射，不引入模块卡外的新概念。

## Summary

US-14 把 Basalt 从"能跑一个 Agent 的底座"推进到"能被复制、能被验证、能对外交付的一组业务 Agent 与 MCP 配方"，分三片交付、一次登记、分片验收：

1. **14A（P1，产品代码）**：只读 MCP 目录（六条嵌入式配方，编译期快照）、保存 / 发布 / 回滚期的 env 闸门（fail-closed，503 + `operation_id`）、不启动进程的命令依赖诊断、MCP 测试记录补强（被测版本 / 阶段 / 失败分类 / 工具名单）、工具来源与跳过视图，以及管理台目录页 / 预填 / 诊断与测试详情。
2. **14B（P2，示例素材）**：五个每日业务 Demo（github-radar / repo-digest / biz-daily / docs-diff / industry-brief）+ 八个领域模板（六 A、一 A\*、一 B）+ 共享 `docs-readonly` 只读 MCP 素材 + 逐目录计数的示例脚本测试 + README / 官网增量区块（以 US-13 review ② 为前置）。
3. **14C（P3，发布收尾）**：`make example-tests` / `make e2e`、候选二进制的七场景发布 e2e（含新增取消、工具超时、热加载）、干净检出脚本、`us14-release` stage、依赖与许可表、升级 / 恢复演练与双平台走查证据，形成 `v0.1.0` 锚点（tag 由用户手动）。

技术路线的要点：

1. **目录单一事实源**：目录数据是 `internal/catalog/data/mcp_catalog.yaml`，经 `//go:embed` 在启动装配时解析一次；`core` 只持有只读视图类型；HTTP 层只暴露 `GET /api/v1/catalog/mcp`（D-01），读取零进程、零安装、零配置写、零操作行。
2. **诊断与启动同源**：诊断用 `internal/app/mcp_diagnostics.go` 调 `internal/tool.NewMCPCommand`——与真实启动共用同一构造（空命令拒绝、`exec.Command`、`Dir`、`mergeChildEnv`、`configureMCPChildProcess`），只做定位检查、**不 `Start`**；三态 `located` / `not_found` / `unconfirmed` 与原因来自构造结果，不解析 stderr。
3. **env 闸门前置四个入口**：创建 / 修改 / 发布 / 回滚在第一次写文件前调用 `validateMCPEntryEnv`；缺失或空值 → 预写 `pending` 操作行后拒绝（503 + `operation_id`），文件 / 版本 / 指针不变；幂等发布（无变更）不重复校验、不宣称环境已满足；手工改 YAML 的启动期对账仍是 fail-closed。
4. **测试记录只追加字段**：`ResourceTestResult` 追加 `OperationID` / `TestedVersionID` / `Stage` / `FailureClass`；`internal/tool` 新增 `ConnectDetailed` 返回结构化失败（`MCPConnectFailure`），`Connect` 行为与旧语义不变；`MCPSupervisor.Test` 有详细结果时使用，否则回退 `Connect` 并标 `dependency_unconfirmed`；操作状态语义按 D-03 / D-05 固定（连接失败 / 策略拒绝 = success + ok=false；超时 = failed + 504；env 缺失 = failed + 503；预写失败不执行、不留行、不启动子进程）。
5. **工具来源在装配层记录**：`mcpDispositionTable` 在内置工具注册与 MCP `OnConnected` 注册处记录 offered / accepted / skipped（赢家来源 + 原因）；`internal/tool/registry.go` 注册语义与重名 WARN 不变；`McpServerView.Tools` 保持 offered 语义。
6. **示例是可移植基线**：`examples/**` 不含实例 UID / 明文凭据 / 明文密钥，基线 `notify_mode: none`；导入后按每份 `SETUP.md` 覆盖模型绑定、服务名、渠道 UID、`deliver_to` 与解释器路径；固定输入在 `fixtures/`，运行时状态 `state/` 不提交。
7. **只读 MCP 素材独立成 module**：`examples/shared/mcp/docs-readonly/` 是纯标准库嵌套 module（`initialize` / `tools/list` / `tools/call`，拒绝越界 / 绝对路径 / 软链接 / 写操作），根 `go test ./...` 不覆盖它，由 `go -C … test ./...` 与发布 e2e 单独构建。
8. **示例测试堵零发现**：`scripts/run-example-tests.py` 按固定目录清单逐目录跑 `unittest discover`，解析 `Ran N tests` 并断言每目录 ≥3；0 发现 / 低于下限 / 非零退出 = 失败；缺 Python = 失败。
9. **发布 e2e 绑定候选制品**：`internal/e2e/harness_test.go` 的 `TestMain` 识别 `BASALT_E2E_BINARY`，导出绝对路径、重算 SHA-256、打印 `binary_source=candidate`；`make e2e` = `build` + `example-tests` + `go test -tags=e2e`，与 `make check` / `acceptance.json` / CI 分离；七场景中取消（Run 落 `cancelled`）与工具自身超时（审计 `failed` + `timeout`、Run 继续）**分两个用例写、分开 required**。
10. **公开材料只增量**：README 英文单语，加 Templates & demos / MCP catalog / Run a demo / What is verified 四节；官网只在既有 Docs 索引加区块、复用 `templates.mjs` 数据模块；不新增一级路由、不链接私有路径；US-13 review ② 未完成前不改其交付物。
11. **发布收尾可复现**：`scripts/check-release-clean.sh` 要求 `git status --porcelain` 为空、`git archive HEAD` 到临时目录后构建 / 门禁 / e2e；`harness/stages/us14-release.json` 汇总 check 与 `protect`，不注册进 `current.json`、不进 CI；发布证据记录版本 + commit + 实际执行的候选路径与 SHA-256（与分发文件同一份）。
12. **判据面变更如实登记**：本 US 的 D-01–D-10 契约加法、`ResourceTestResult` 字段、e2e `TestMain` 候选分支、新增 stage 属已批准判据面变更（模块卡 §6.10）；实施中不得再改已定字面量或放宽阈值。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行是**最低版本要求** `go 1.27`，准确版本由 `toolchain` 与 CI 固定）；管理台与站点为既有构建期工具链（Node 24.22.0、Vue 3 + Vite + TypeScript + Vitest、VitePress 1.6.4），不进 `go.mod`、不进发布二进制；示例业务脚本为 Python 标准库（本机 Python 3.12，`python` 可能是商店存根，本地复核用 `py`）；`docs-readonly` 为纯标准库嵌套 Go module。

**Primary Dependencies**: Go 侧**零新增坐标**——`go.mod` / `go.sum` 不改；新增生产代码只用既有标准库、`goccy/go-yaml`、MCP SDK 与包内既有设施。控制台与站点使用既有 `package-lock.json` 依赖，**零新增 npm 生产依赖**。示例与 `docs-readonly` 只用 Python / Go 标准库。

**Storage**: **零 schema 迁移**——复用既有 `admin_operations` / `resource_versions` / 资源指针 / `runs` / `llm_calls` / `tool_invocations` / `schedule_deliveries` 等表；目录为编译期内嵌数据，诊断与测试结果走既有操作 detail / 资源版本字段；Demo 产物与 `state/` 落在工作区文件系统（不提交）。

**Testing**: 四层——① Go 单测：`internal/catalog`、`internal/app`（`TestMCPEnvGate*` / `TestMCPDiagnostics*` / `TestMCPTest*` / `TestMCPToolDisposition*` / `TestRegistry_DuplicateSemanticsUnchanged`）、`internal/tool`（`TestMCPPool_ConnectDetailed*` / `TestNewMCPCommand_MatchesLaunch`）、`internal/web`（`TestMCP*` 契约 / handler）；② 控制台：`npm --prefix assets/console test` + `npm --prefix assets/console run build`（重建 `dist` 并核对 `integrity.json`）；③ 示例：`go -C examples/shared/mcp/docs-readonly test -count=1 ./...` + `python scripts/run-example-tests.py`（逐目录 `Ran N tests` ≥3）；④ 发布 e2e：`make e2e`（`//go:build e2e`、候选二进制绝对路径 + SHA-256、`binary_source=candidate`、`-count=1`），七场景 + 五 Demo 夹具 + 八模板质量 + 遗产回归；站点测试与检查沿用 US-13（`npm --prefix website test|build|check`）。`make check` / `scripts/check-acceptance.*` **不调用** Node / npm / Python。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布候选为 `CGO_ENABLED=0` 单文件；14C **Windows 与 Linux 两个平台均必验**（Windows 锁 / 中文控制台 / 进程树取消；Linux 同一候选二进制同一组必跑命令）；管理台面向现代浏览器；官网为静态站点（本 US 不部署）。

**Project Type**: 单 Go module（`cmd/basalt` + `internal/`）+ 内嵌管理台静态资源 + 独立示例目录（含一个嵌套 Go module）+ 静态站点工程；不新增 Go 包之外的产品形态。

**Performance Goals**: 无吞吐承诺；判据是**有界与可观察**——MCP 测试超时 15s（既有 `defaultMCPTestTimeout`），发布 e2e 工具超时场景 45s 内完成审计与 Run 收尾，取消后子进程树清理有时间上限；目录读取为内存快照、无 I/O 放大；不写未测量的耗时或规模数字。

**Constraints**: **零迁移、零新增 Go / npm 生产依赖**；目录 / 诊断只读（零进程、零安装、零配置写、零操作行）；env 闸门 fail-closed 且错误不含值；诊断不预判测试、测试不解析 stderr；`make e2e` / `example-tests` 不进 `make check` / `acceptance.json` / CI；外部服务六项**全部未授权**（只做夹具并标未验证）；公开材料不链接私有路径；`v0.1.0` tag 由用户手动。

**Scale/Scope**: 工作区约 **82–110 个文件**（14A 18–25、14B 55–72、14C 8–12，去重后合计；不承诺，按 tasks 逐文件计数）；五 Demo、八模板、六条目录配方、五个新 Go 生产文件 + 三个 core 类型文件、D-01–D-10 契约、七条发布 e2e 场景、一个 stage。真实外部实跑与双平台走查属人工 / 授权项，不计入文件量。

**环境实测读数（2026-10-25，本轮当场命令）**: 分支 `014-templates-demos-release`；起始 commit `e2e285f`；工作区干净（仅 `?? specs/014-templates-demos-release/` 为本轮产物）；Go 1.27.3、Node 24.22.0（便携安装于 `%TEMP%`，本机 PATH 无 `node`）、Python 3.12；`GOPROXY=https://goproxy.cn`。**本轮未跑** `make check` / `make e2e` / 示例测试；相关项在实现后由 Claude 验收。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 / 约束 | 本模块落地判据 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | `go.mod` 零新增坐标；示例与模板只消费既有循环，不改循环体；`docs-readonly` 是独立素材 module，不是框架 | 通过 |
| II LLM 客户端只做协议调用 / 单一消息仓储 | 不改 LLM 客户端与消息追加路径；Demo 走既有 `AgentService` / `ReActLoop` 链路 | 通过 |
| III Provider 显式映射 | 不改 Provider 目录 / 准入 / 路由；Demo 的模型绑定由 SETUP 交给既有管理面 | 通过 |
| IV 一个目录 = 一个 Agent / 声明式技能绑定 | 五 Demo 与八模板各是一个 Agent 目录；技能只由 `AGENT.md` frontmatter `skills:` 声明；不引入软连接、不新增 `use_skill`；`repo-digest` 的 L1/L2/L3 走既有渐进披露 | 通过 |
| V 审计 day-one 且 fail-closed | env 闸门先写 `pending` 再拒绝；测试预写失败不执行、不留行；目录 / 诊断**不写操作行**（只读不产生管理变更）；Demo 证据靠既有 `runs` / `llm_calls` / `tool_invocations` | 通过 |
| VI 授权与执行分离 | 不改 `Policy` / `CommandRunner`；`NewMCPCommand` 只构造不执行；诊断不越权启动进程 | 通过 |
| VII 同步执行 + context 纪律 | 无新增异步编排；测试 / 诊断同步阻塞；不新增 `context.WithValue` 业务状态 | 通过 |
| VIII 单二进制纪律 | 零新增 Go 坐标；控制台与站点为构建期工具，不进二进制；`-race` 与 `CGO_ENABLED=0` 分属不同命令；`make e2e` 不进 `make check` | 通过 |
| IX 不用 ORM / 只追加迁移 | 零 schema 迁移；手写 SQL 与既有仓储不变 | 通过 |
| X 范围卡紧 | 交付物与模块卡 §3 清单逐项对应；OBS / CHAOS / 打包 / SSO / 多租户明确不做；外部未授权项不写成通过 | 通过 |
| 架构约束：技术栈 | 只用已裁决坐标；Node / VitePress 已在 §1.3；零新增 | 通过 |
| 架构约束：依赖方向与包结构纪律 | 新包 `internal/catalog` 只被 `app` / `web` 装配；`core` 只加纯类型；`app` 记录工具来源，不改 `tool.Registry`；`web` 只消费端口 | 通过 |
| 架构约束：凭证与资源绑定纪律 | env 闸门只回布尔与键名，不回值；目录 `env[].placeholder` 只是展示；示例零凭据、测试哨兵值；不改托管密文路径 | 通过 |
| 架构约束：资源使用范围 / 身份与绑定权威 | 不改资源范围与绑定逻辑；Demo 的 `resource_bindings` 由 SETUP 显式添加，不自动补绑 | 通过 |

**Post-Design Re-check**: Phase 1 设计（research / data-model / quickstart）完成后复核，结论不变；无违反项需要 Complexity Tracking。

## Project Structure

### Documentation (this feature)

```text
specs/014-templates-demos-release/
├── plan.md              # 本文件
├── spec.md              # 需求规格（含 2026-10-25 澄清）
├── research.md          # Phase 0 输出
├── data-model.md        # Phase 1 输出
├── quickstart.md        # Phase 1 输出
├── checklists/
│   └── requirements.md  # 规格质量清单（16/16）
├── run-state.md         # 过程状态（/module-dev 建立；非设计输入）
└── tasks.md             # Phase 2 输出（/speckit-tasks，不由本命令创建）
```

**不新建 `specs/014-templates-demos-release/contracts/`**：D-01–D-10 的固定字面量写入 [docs/contracts/openapi.yaml](../../docs/contracts/openapi.yaml)；复制一份必然漂移。接口契约摘要见 [data-model.md](./data-model.md)，逐字段原文以模块卡 §3.2 与 `openapi.yaml` 为准。

### Source Code (repository root)

```text
internal/catalog/
├── catalog.go                     # 新增：Load(raw) 解析 + 校验
├── data.go                        # 新增：//go:embed data/mcp_catalog.yaml + Builtin()
├── data/mcp_catalog.yaml          # 新增：六条配方
└── catalog_test.go                # 新增：Load / UniqueIDs / EnvPlaceholders / RecipesParse

internal/core/
├── mcp_catalog.go                 # 新增：MCPCatalog / MCPCatalogEntry / MCPCatalogEnv
├── mcp_diagnostics.go             # 新增：MCPServerDiagnosis / MCPCommandDiagnosis / MCPEnvDiagnosis
├── mcp_disposition.go             # 新增：MCPToolDisposition / MCPToolSkip
└── resource_admin.go              # 修改：ResourceTestResult 追加四字段

internal/tool/
├── mcp.go                         # 修改：MCPConnectFailure / ConnectDetailed / NewMCPCommand；Connect 行为不变
└── mcp_test.go                    # 修改：ConnectDetailed 分类 / NewMCPCommand 与启动一致

internal/app/
├── mcp_env.go                     # 新增：validateMCPEntryEnv + mcpEnvGateDetail
├── mcp_diagnostics.go             # 新增：diagnoseMCPCommand / DiagnoseMCPServer
├── mcp_disposition.go             # 新增：mcpDispositionTable
├── resource_admin.go              # 修改：四个入口闸门、测试记录字段、诊断方法、deps
├── mcp_supervisor.go              # 修改：Test 使用 ConnectDetailed 并填 Stage / FailureClass / Tools
├── mcp.go                         # 修改：mcpRuntime.dispositions / ToolDisposition
├── resources.go                   # 修改：mcpServerView 投影 ToolDisposition
├── wire.go                        # 修改：内置 / MCP 注册记录来源、诊断注入、目录注入
├── mcp_env_test.go                # 新增
├── mcp_diagnostics_test.go        # 新增
├── mcp_disposition_test.go        # 新增
└── resource_admin_test.go         # 修改：测试记录 / 旧记录 / 诊断不预判

internal/web/
├── ports_admin.go                 # 修改：ResourceAdminService 追加诊断；MCPCatalogReader
├── server.go                      # 修改：Options.MCPCatalog
├── admin_routes.go                # 修改：两条 GET 路由
├── handlers_admin_resources.go    # 修改：目录 / 诊断 handler；测试新字段与 503 / 504
├── views.go                       # 修改：目录 / 诊断 / 工具来源视图
├── views_admin.go                 # 修改：测试结果视图追加字段
└── *_test.go                      # 修改：D-01–D-10 契约 / handler

assets/console/
├── src/router.ts                  # 修改：catalog/mcp 路由
├── src/api.ts                     # 修改：目录 / 诊断 / 测试类型
├── src/api-catalog.ts             # 新增：getMCPCatalog / getMCPDiagnostics
├── src/views/catalog.ts           # 新增：目录表格 / 筛选 / 详情 / 添加到配置
├── src/views/resources.ts         # 修改：MCP 目录入口
├── src/views/resource-detail.ts   # 修改：诊断区块 / 测试详情 / operation_id
├── src/components/resource-form.ts# 修改：可选 prefill（只预填不提交）
├── tests/catalog.test.ts          # 新增
├── tests/resource-detail.test.ts  # 修改
└── dist/**                        # 重建（完整性清单同步）

docs/contracts/openapi.yaml        # 修改：D-01–D-10
docs/Operations.md                 # 修改：示例依赖 / 出网边界 / 演示依赖（公开）
docs/Requirements.md               # 修改：§2.5 / §7.2 / §7.5（私有）
docs/TechnicalDesign.md            # 修改：§8.1 / §8.1.1 / §8.1.2 / §8.2（私有）
docs/DevelopmentLoop.md            # 修改：§4.5.2 / §5.3（私有）
docs/Roadmap.md                    # 修改：§6 M0 US-14 行（私有）

examples/
├── README.md                      # 新增：总览 / 依赖 / 证据档位
├── daily/{github-radar,repo-digest,biz-daily,docs-diff,industry-brief}/
│   ├── AGENT.md / README.md / SETUP.md
│   ├── scripts/*.py + scripts/test_*.py
│   └── fixtures/**（repo-digest 另有 skills/repo-digest/SKILL.md）
├── templates/{weather-brief,arxiv-tracker,policy-watch,ticket-insights,onboarding-buddy,dependency-watch,web-watch,funding-watch}/
│   └── AGENT.md / README.md / fixtures/**（部分含 skills/）
└── shared/
    ├── mcp/docs-readonly/{go.mod,main.go,main_test.go,README.md}   # 嵌套 module
    ├── fixtures/docs/**
    └── scripts/make_sample_db.py

scripts/run-example-tests.py       # 新增：逐目录 unittest + Ran N ≥3
scripts/check-release-clean.sh     # 新增：干净检出构建 / 门禁 / e2e
Makefile                           # 修改：example-tests / e2e
internal/e2e/us14_demos_test.go    # 新增：TestUS14Demos_Fixture / TestLegacyD1_HttpEndpoints
internal/e2e/us14_release_test.go  # 新增：TestReleaseE2E_Cancel / _ToolTimeout / _HotReload / _SevenScenarioMap
internal/e2e/us14_templates_test.go# 新增：TestTemplates_LoadAndTools / _TierEvidence
internal/e2e/harness_test.go       # 修改：TestMain 候选二进制分支
harness/stages/us14-release.json   # 新增：本 US 专属 stage（不注册进 current.json / CI）

README.md                          # 修改：四节增量（US-13 review ② 后）
website/.vitepress/data/templates.mjs        # 新增（review ② 后）
website/.vitepress/theme/components/TemplateTable.vue  # 新增（review ② 后）
website/docs/index.md / website/zh/docs/index.md        # 修改：区块增量（review ② 后）
website/tests/content.test.mjs / scripts/check-links.mjs# 修改：模板表 / examples 链接（review ② 后）
```

**Structure Decision**: 14A 只加一个新包 `internal/catalog`（纯数据 + 解析，无 I/O），其余改动落在既有包；`core` 只加纯类型，`app` 负责装配与来源记录，`web` 只消费端口，`tool.Registry` 不动。14B 完全在 `examples/` 内，含一个独立嵌套 module，根 module 的测试只通过 `internal/e2e` 读取示例（`go test ./...` 不覆盖嵌套 module）。14C 只加脚本、Makefile 目标、e2e 测试文件与 stage，不改既有 stage / CI / `acceptance.json`。管理台与站点继续走各自既有构建链，产物不进 Go module。

## Complexity Tracking

无 Constitution Check 违反项，无需登记。

## Slice Mapping (task order)

| 切片 | 前置 | 主要任务组 | 完成判据（摘要） |
| --- | --- | --- | --- |
| 14A | US-11 / US-12 已交付 | 契约 D-01–D-10 → `core` 类型 → `internal/catalog` → `tool` 构造 / 详细连接 → `app` 闸门 / 诊断 / 来源 / 测试记录 → `web` 端点与视图 → 控制台 → 文档事实同步 | `mcp-*` 检查全绿；零迁移；目录零副作用；D-xx 字段逐字 |
| 14B | 14A；README / 官网另需 US-13 review ② | `docs-readonly` → 五 Demo → 八模板 → 共享夹具 / 脚本 → 示例测试入口 → e2e 夹具测试 → 公开增量 | 五 Demo 六段 / 业务断言 / 通知计数；模板质量闸门；零凭据；scripts 逐目录计数 |
| 14C | 14A + 14B + US-13 + US-15 设施 | `make e2e` → 取消 / 超时 / 热加载测试 → stage → 干净检出脚本 → 许可 / 依赖 / 平台 / 升级证据 | 候选二进制七场景绿；双平台证据；`v0.1.0` 由用户手动 |

## Phase 0 / Phase 1 产物

- [research.md](./research.md)：关键技术决策与替代方案（目录内嵌与校验、诊断同源构造、env 闸门位置、测试记录字段、来源表位置、示例测试计数、候选二进制绑定、干净检出、公开增量）。
- [data-model.md](./data-model.md)：目录 / 诊断 / 测试记录 / 工具来源 / Demo 证据 / 模板档位 / 发布证据的实体、字段与状态矩阵。
- [quickstart.md](./quickstart.md)：可执行的验证向导（Go / 控制台 / 示例 / e2e / stage / 人工项 / 未授权边界）。

# Tasks: US-7 MCP 集成（stdio 客户端 · 零代码接入 · 故障隔离 · 凭证边界 · 同形可审计）

**Input**: Design documents from `/specs/007-mcp/`

**Prerequisites**: [plan.md](./plan.md)（必需）、[spec.md](./spec.md)（四个用户故事与优先级）、[research.md](./research.md)（D1~D14）、[data-model.md](./data-model.md)、[quickstart.md](./quickstart.md)；唯一事实源 = 颗粒度文档 [docs/us/us7-mcp.md](../../docs/us/us7-mcp.md) §2.2（约束 28 条）/ §2.3（伪代码，签名逐字）/ §3（交付物清单）/ §3.5（前序改造点 9 项 + 后续占位 6 项）/ §4.3（R1a / R1b / R2~R13 共 14 条关键回归、18 个测试函数）/ §6.2（固定口径 25 条）；固定字面量 = [docs/contracts/](../../docs/contracts/)（`agent-md.md` 规则 4 / 6 文案、`run-and-errors.md` 错误分类七值）。

**Tests**: **必须包含**——本仓「harness 先行」是硬纪律：**默认层新增 5 文件 46 条**（`workspace/mcp_test.go` 18 / `tool/mcp_test.go` 18 / `app/wire_mcp_test.go` 7 / `app/mcp_test.go` 1 / `testutil/mcpserver_test.go` 2）+ **集成层新增 1 文件 5 条**（`tool/mcp_integration_test.go`，本仓首个真实子进程集成层）= **6 文件 51 条**；另改动 2 文件追加 **6 条**（`workspace/agent_test.go` +4 / `cli/profile_test.go` +2）+ 夹具改造 **2 点**。**关键回归独立任务**：R1a / R1b / R2~R13 共 14 条、18 个测试函数，各落独立任务并注明**逆改点**；实现与测试同批推进，**不得**先实现后补测。

**Organization**: 任务按 spec 的四个用户故事分组（US1~US4），每个故事可独立验证。**标签口径**：`[US1]`~`[US4]` 是**本 feature 内部**故事编号，**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）的 US-1~US-7；四个故事同属项目 **US-7** 一个模块。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 所属用户故事（US1 / US2 / US3 / US4）；Setup / Foundational / Polish 阶段不带标签
- 每条任务给出精确文件路径与可执行 DoD

## 任务阶段口径：测试准备 / 实现 / 验证（**不得**混记）

每个任务**只承担一个阶段**，DoD 只登记本阶段读数——**不得**把「准备好测试」记成「测试已通过」：

| 阶段 | 承担者 | 本阶段交付与 DoD 读数 |
| --- | --- | --- |
| **测试准备** | 测试类任务（T008 / T010 / T012 / T015 / T017 / T019 / T020 / T024 / T026 / T029 / T031 / T032） | 测试**落地** + 登记**红态读数**（逐字构建错误或逐字失败文案）+ 写明**转绿前提 = T0xx**；**不得**写「转绿 / PASS」 |
| **实现** | 实现类任务（T004~T007 / T009 / T011 / T013 / T016 / T018 / T021 / T022 / T025 / T027 / T030 / T033） | **由红转绿**——绿态读数登记在实现任务的 DoD 里，并点名它让哪几条测试任务由红转绿 |
| **验证** | 故事收尾任务（T014 / T023 / T028 / T034 / T037 / T038） | 本故事**全绿** + 关键回归逐条绿态读数 + **逆改点红态复现**（`go test -overlay` 逆改，探针建在**仓库外**、用毕即删） |

**红态口径**：测试先于实现落地 → 登记自然红态；行为已在更早故事落地（跨故事复验的用例）→ 用 `go test -overlay` 逆改实现取得红态逐字读数，**不得只看退出码**。两种读数都必须能指出**逆改点**（实现怎么改它会红）。

## Path Conventions

单 Go module：`cmd/basalt/`、`internal/`。测试与被测包**同目录**；**测试包归属是判据面事实，动手前逐文件核**（2026-10-17 实测）：

| 测试文件 | 包名 | 说明 |
| --- | --- | --- |
| `internal/workspace/mcp_test.go` | **`workspace_test`** | 外部包——只用 `workspace.ParseMCPServers` 导出面 |
| `internal/tool/mcp_test.go` | **`tool`**（内部包） | 白盒：直接替换非导出 `dial`、直接调 `newMCPToolAdapter` / `listAllTools`（与 `runner_windows_test.go` 同包先例） |
| `internal/tool/mcp_integration_test.go` | **`tool_test`** | 只用导出面 + `testutil`；`//go:build integration` |
| `internal/app/wire_mcp_test.go` / `internal/app/mcp_test.go` | **`app`**（内部包） | 直接设置包内字段 `mcpConnect`、直接调非导出 `resolveMCPEnv` |
| `internal/testutil/mcpserver_test.go` | **`testutil`**（内部包） | 与 `testutil_test.go` 同包 |
| `internal/workspace/agent_test.go` | **`workspace_test`** | 既有文件，追加 4 条 |
| `internal/cli/profile_test.go` | **`cli`**（内部包） | 既有文件，追加 2 条 |

**测试缝纪律**：app 层经 `fx.app()` 取 `*App` 后直接设置包内字段 `mcpConnect`，tool 层白盒直接替换非导出 `dial`——**不得**新增导出测试 API（`SetDialForTest` / `NewMCPToolAdapterForTest` 一类一律不落地）。

`contracts/` **不新建**（固定字面量已有唯一事实源）。每个 `go` 命令显式带 `GOPROXY=https://goproxy.cn`。**记账口径**：代码 新增 **6** + 改动 **6**；测试 新增 **6 文件（默认层 46 + 集成层 5 = 51 条）** + 改动 **2 文件（追加 6 条）** + 夹具改造 **2 点** + `go.mod` / `go.sum` 坐标 **1 条** + `docs/Compatibility.md` **3 行**。

**前序改造点（§3.5 的 9 项）**：逐条已落进下方任务（① T013、② T013、③ T018、④ T005+T021、⑤ T006、⑥ T013+T022+T033、⑦ T013、⑧ T012+T017、⑨ T035）。性质标注：**①③④ = 判据面变更**（① 夹具改写判据不变；③ 注册语义从「配置存在」升级为「已配置且已连接」；④ 新增第三态与展示面，零配置断言逐字节不变）、**⑤ = 等价重构**（既有 runner 用例原样全绿）、**⑨ = 文档同步**。这些都属**软门禁**事项，已随本任务清单在步骤 5 停点**一次性送审**；停点确认后执行阶段不再逐条停车。

---

## Phase 1: Setup（共享前置）

**Purpose**: 环境与事实核实、零变更面基线、依赖坐标——不涉任何用户故事

- [x] T001 写前核实与规范前置就位：`go version` 复核 `go1.27.2`；逐条读 [docs/EngineeringStandards.md](../../docs/EngineeringStandards.md) §1 Go / §2 测试 / §3 数据库 / §4 日志与错误；对照 [research.md](./research.md)「写前核实清单」复核 SDK 读数（`CommandTransport.Connect` 忽略 ctx、进程生命周期由 `exec.Cmd` 持有、`NewInMemoryTransports` 对称要求、`ListTools` / `CallTool` 结果形状、`mcp.Content` 为 sealed interface）。DoD：`go version` 退出码 0 且输出 `go1.27.2 windows/amd64`；规范前置四节全读；复核读数逐条登记；探针（如有）建在仓库外、用毕即删（`git status --short` 零残留）
- [x] T002 [P] 零变更面基线取证：`git diff -- go.mod go.sum`（现状零 `modelcontextprotocol` 坐标）；`git diff -- docs/contracts/schema.sql`（**零迁移**）；`git diff -- basalt.yaml.example`（**零新增配置键**）；`git diff -- Makefile .github/ scripts/`（覆盖率阈值与七目标已在）；`ls -A internal/storage/migrations/`（仅 `.gitkeep`）。DoD：逐条读数粘贴；本 US 期间这些面**零改动**是收口硬判据
- [x] T003 依赖坐标落地：`GOPROXY=https://goproxy.cn go list -m -versions github.com/modelcontextprotocol/go-sdk` 核实 v1.8.0 存在；`GOPROXY=https://goproxy.cn go get github.com/modelcontextprotocol/go-sdk@v1.8.0`；此时尚无 import，`go.mod` 可能标 `// indirect`，**最终归类在 T036 由 `go mod tidy` 定稿**。DoD：`grep -n modelcontextprotocol go.mod` 有且仅有一行 `v1.8.0`；`git diff go.mod` 只多该坐标（表外**直接**坐标零新增）；读数登记

---

## Phase 2: Foundational（阻塞全部用户故事）

**Purpose**: `core` 声明面（MCP 配置类型 + 第三态字段）、子进程环境合并的唯一实现、跨包测试基建——四个故事都建立在它之上

**⚠️ CRITICAL**: 本阶段完成前，任何用户故事都不得开工

- [x] T004 [P] `internal/core/mcp.go`（新增——**纯类型，零 import 内部包**，签名逐字对齐 §3.1）：`type MCPServerConfig struct { Name string; Command string; Env map[string]string }`（无方法、无测试）。**关键注释中英并列**：`Env` 在 `workspace` 产出时是 `${VAR}` 占位形态，经 `app.resolveMCPEnv` 之后才是最终值（同类型两阶段，已决 #2）。DoD：`GOPROXY=https://goproxy.cn go build ./internal/core` 退出码 0；`go list -f '{{.Imports}}' ./internal/core` 只出标准库；`gofumpt -l internal/core/mcp.go` 空输出
- [x] T005 [P] `internal/core/profile.go`（改动——§3.5 改造点 ④ 的类型面）：`AgentEntry` 增 `Unverified bool`（`Registered == false && Unverified == true` = 轻路径待确认；`Profile` 非 nil 的既有语义不变）。DoD：`go build ./...` 退出码 0；既有 `go test ./internal/core ./internal/workspace ./internal/cli` 全绿（零新增用例）
- [x] T006 `internal/tool/runner.go`（改动——§3.5 改造点 ⑤，**等价重构**）：抽出 `func mergeChildEnv(passthrough []string, declared map[string]string) []string`，`LocalRunner.childEnv` 改为调用它；语义逐字节不变——透传白名单 + 声明覆盖同名键、其余清空、结果**永不为 nil**。DoD：`go test -race -count=1 ./internal/tool` 既有 runner / shell 用例**零改动全绿**（透传 / 覆盖 / 空清单三组读数）；`git diff --stat internal/tool/runner.go` 只动该文件；`gofumpt -l` 空输出
- [x] T007 `internal/testutil/mcpserver.go`（新增）+ `internal/testutil/mcpserver_test.go`（新增 2 条，`package testutil`）：**① 测试准备**——先落 `TestMCPTestServer_StandardToolsAreListed`（`echo` / `boom` / `blob` 三件 + schema 非空）与 `TestRunMCPHelper_SkipsWithoutEnv`（未设 `BASALT_MCP_HELPER` → `t.Skip`，理由可读），登记逐字构建错误；**② 实现**——`MCPTestTool` / `NewInMemoryMCPPair`（server 先连、client 后连）/ `StandardMCPTools` / `RunMCPHelper`。DoD：① 红态读数；② `GOPROXY=https://goproxy.cn go test -race -count=1 ./internal/testutil` → `1 PASS + 1 SKIP`、跳过理由含「仅作为 MCP stdio 子进程入口运行」；`gofumpt -l` 空输出

**Checkpoint**: 类型面、第三态字段、环境合并与测试基建就位——US1 可开工

---

## Phase 3: User Story 1 - 零代码接入走通：配置文件 → 连接 → 工具同形可用（Priority: P1）🎯 MVP

**Goal**: 运营者只写 `mcp_servers.yaml` 与 `AGENT.md`，不改代码：启动时解析配置、串行拉起 server、握手列工具、把工具注册进全局池；Agent 声明可解析、模型可调用、结果按固定规则渲染；零 MCP 配置时行为与 US-6 逐字节相同。

**Independent Test**: 以**进程内 MCP 测试对**（`testutil.NewInMemoryMCPPair` + 脚本化工具）驱动真装配：①注册池 = 内置 8 + MCP 2；②声明这些工具的 Agent 注册；③工具名 / 描述 / schema 逐字一致；④一次成功调用按固定规则渲染；⑤启动摘要 `mcp_connected = 1`；⑥重名时内置优先 + `拒绝重复注册的工具` WARN；⑦零配置时既有 `wire_test.go` 全绿且摘要显式 `mcp_connected=0`。审计两阶段由 US4 的 R9 钉住。

### Tests for User Story 1

- [x] T008 [US1] `internal/workspace/mcp_test.go`（新增 **18 条**，`package workspace_test`）——§3.2 全表原样落地：`TestParseMCPServers_MissingOrEmptyYieldsZero`（缺失 / 空 / 纯注释 / `{}` / `servers: null` 五态）· `TestParseMCPServers_ListOrderPreserved` · `TestParseMCPServers_RejectsNonMappingRoot` · `TestParseMCPServers_RejectsServersNotList` · `TestParseMCPServers_RejectsEntryNotMapping` · `TestParseMCPServers_RequiresNameAndCommand` · `TestParseMCPServers_RejectsDuplicateNames` · `TestParseMCPServers_RejectsEnvNotMapping` · **`TestParseMCPServers_RejectsPlaintextEnvWithoutEchoingValue`**（**R1a**）· `TestParseMCPServers_AcceptsExactPlaceholderOnly` · `TestParseMCPServers_TransportDefaultsToStdio` · `TestParseMCPServers_RejectsUnsupportedTransport` · `TestParseMCPServers_IgnoresUnknownEntryKeys` · `TestParseMCPServers_EnvKeysPreserved` · `TestParseMCPServers_ReadsThroughRoot` · `TestParseMCPServers_ParseErrorIsSummarized` · `TestMCPServerNames_PreservesConfigOrder` · `TestIsEnvPlaceholder_Table`。DoD：**测试准备**——`grep -c '^func Test' internal/workspace/mcp_test.go` → 18；登记逐字构建错误（`undefined: workspace.ParseMCPServers` 等）；转绿前提 = T009；**不得**写「通过」
- [x] T009 [US1] `internal/workspace/mcp.go`（新增）：`ParseMCPServers(root string) ([]core.MCPServerConfig, error)` + 非导出 `parseMCPServers` / `mcpServerNames` / `isEnvPlaceholder`；**逐块对照 §2.3 伪代码**：`os.Root` 读取、顶层映射、`servers` 列表、条目字段、重名、`env` 精确 `${VAR}`、`transport` 缺省 stdio / 非 stdio 拒、文件顺序保留、零 server 五态；错误点名文件 / `servers[i]` / server 名与字段，**永不回显 env 值**。DoD：T008 **18 条全 PASS**；`go test -race -count=1 ./internal/workspace` 全绿；`go vet` 退出码 0；`gofumpt -l` 空输出；`go list -f '{{.Imports}}' ./internal/workspace` 零 `internal/tool'
- [x] T010 [US1] `internal/tool/mcp_test.go`（新增首批 **6 条**，`package tool`）：`TestMCPPool_ConnectReturnsConnectionsInSpecOrder` · `TestMCPPool_ListToolsPaginatesToEmptyCursor`（**R7 前半**）· `TestMCPToolAdapter_RendersTextBlocksJoined` · `TestMCPToolAdapter_RendersNonTextAsPlaceholderNoBase64` · `TestMCPToolAdapter_FallsBackToStructuredContentAndNoOutput` · `TestMCPToolAdapter_InputObjectAndEmptyInput`；同批落辅助件 `captureLogger` / `connectTestSession` / `testAdapter`（§4.3 辅助件表）。DoD：**测试准备**——6 条落地；逐字构建错误（`undefined: NewMCPPool` 等）；转绿前提 = T011；**不得**写「通过」
- [x] T011 [US1] `internal/tool/mcp.go` + `internal/tool/mcptools.go`（新增）：`MCPPool` / `MCPPoolOptions` / `NewMCPPool` / `Connect`（**成功路径**：逐 server 串行、30s 预算只罩 initialize + tools/list、dial 不接收 ctx）/ `dialCommand`（`exec.Command`、`Dir` = 工作区根、`Env` = `mergeChildEnv`）/ `listAllTools`（游标到空 + 重复游标守卫）/ `Close`（基础形态）；`mcpToolAdapter` 四方法 + `mcpCallArguments`（空 / `null` → `{}`，其余原样 `json.RawMessage`）+ `renderCallToolResult` / `mcpContentPlaceholder`。**本任务只做成功路径与渲染**：失败吸收在 T016、错误边界在 T027 / T030（US1 独立验收不含故障隔离与错误分类）。DoD：**由红转绿**——T010 **6 条全 PASS**；`go test -race -count=1 ./internal/tool` 全绿；`go vet` / `gofumpt -l` 退出码 0
- [x] T012 [US1] `internal/app/wire_mcp_test.go`（新增首批 **3 条**，`package app`）：`TestWire_MCPToolsRegisteredAfterEqualityGate`（**R3 前半**）· `TestWire_DuplicateMCPToolKeepsBuiltin`（**R8**）· `TestWire_StartupSummaryCountsConnectedServers`（FR-027 / 约束 23，见 Notes ①）；同批落夹具 `fakeMCPConnector` / `fakeMCPTool` / `writeMCPServersYAML`（§3.5 改造点 ⑧）。DoD：**测试准备**——3 条落地；逐字红态（装配后 `registry.Names()` 无 `search_docs` / 摘要无 `mcp_connected`）；转绿前提 = T013
- [x] T013 [US1] `internal/app/mcp.go`（新增：`mcpConnector` 接口 + `mcpToolNames`）+ `internal/app/wire.go`（改动：等值门禁**之后**插入「建池 → `Connect` → 注册 MCP 工具」，重名 keep-first + WARN `拒绝重复注册的工具`，摘要增 `mcp_connected`，装载器注入 `MCPServers`）+ `internal/workspace/agent.go`（改动：`AgentLoaderOptions` 增 `MCPServers` / `MCPConnected` 两字段并在构造期 `slices.Clone`；`LoadAll` 改用 `parseMCPServers` 取名字集、删除 `readMCPServerNames`）+ `internal/workspace/templates.go`（**只改注释**）——§3.5 改造点 ①②⑥⑦。**夹具改造 ①**：`agent_test.go` 的 `writeMCPServers` 改为 `servers:` 列表形态（判据不变，`ghost-mcp` 仍不在配置里）。DoD：**由红转绿**——T012 **3 条全 PASS**；既有 `go test -race -count=1 ./internal/app` 全绿（零配置工作区 `{}` → 零 server，整段跳过）；`go test -race -count=1 ./internal/workspace` 全绿；`gofumpt -l` 空输出
- [x] T014 [US1] **US1 验证**：复跑 `GOPROXY=https://goproxy.cn go test -race -count=1 ./internal/workspace ./internal/tool ./internal/app`；对 R3 前半 / R8 做**逆改点红态复现**（`go test -overlay`：把 MCP 注册挪到等值门禁前 → R3 红；注册循环改成后到者覆盖 → R8 红），探针建在仓库外、用毕即删。DoD：三包全绿；R3 前半 / R8 红→绿读数各一行；零配置摘要 `mcp_connected=0` 读数；`git status --short` 无探针残留

**Checkpoint**: US1 独立可验——零代码接入正向链路走通，零配置不回归

---

## Phase 4: User Story 2 - 故障隔离与准入一致：坏 server 不阻断启动、依赖它的 Agent 整只不注册（Priority: P2）

**Goal**: 任一 server 的 dial / initialize / list 失败只影响自己：WARN 点名并跳过、其余照常、启动完成；依赖失败 server 的 Agent 因规则 4（工具不在池）或规则 6（server 未连接）整只不注册，规则 4 先于规则 6、各按声明序报首个；轻路径不连接 MCP，对可能依赖 MCP 的 Agent 给第三态 `unverified`，确定性规则仍先拒。

**Independent Test**: 假连接器注入「一个失败 + 一个成功」：①启动成功、WARN 含固定键 `server` / `reason` / `detail`、成功项工具可用；②声明失败工具 → `tool not registered: <tool>`；③只声明失败 server → `mcp server skipped: <server>`；④同时声明先报规则 4；⑤轻路径 `unverified` 不进运行面；⑥确定性规则先拒。

### Tests for User Story 2

- [x] T015 [US2] `internal/tool/mcp_test.go`（追加 **3 条**）：**`TestMCPPool_ConnectSkipsFailedServerAndKeepsOthers`**（**R2**：dial 失败 → WARN 固定键 + 其余连接保留 + 无错返回）· `TestMCPPool_ConnectFailureClosesPartialSession`（dial 成功但 initialize 失败 → 半截会话关闭）· **`TestMCPPool_ConnectFailureLogOmitsSensitiveDetail`**（**R12 前半**：dial 错误含哨兵 → 日志只有固定短语）。DoD：**测试准备**——3 条落地；登记逐字红态（当前 `Connect` 返回错误、无 WARN 行 / 日志含哨兵）；转绿前提 = T016
- [x] T016 [US2] `internal/tool/mcp.go`（改动）：`Connect` 吸收三类失败——WARN `MCP server 未连接——已跳过`，固定键 `server` / `reason`（`启动失败` / `初始化失败` / `列工具失败`）/ `detail`（`safeErrorSummary` 固定短语集：`可执行文件不存在` / `权限不足` / `超时` / `已取消` / `子进程退出码 N` / `外部交互失败`）；失败时关闭半截会话并继续；只有整体 ctx 取消 / 截止返回错误。DoD：**由红转绿**——T015 **3 条全 PASS**；`go test -race -count=1 ./internal/tool` 全绿；`safeErrorSummary` 的固定短语集逐字对照 §2.3
- [x] T017 [US2] `internal/workspace/agent_test.go`（追加 **4 条 + 夹具改造 ②**，`package workspace_test`）：`TestAgentLoader_DeclaredServerConnectedRegistersAgent` · **`TestAgentLoader_ConfiguredButNotConnectedRejects`**（**R4**，§4.3 代码原样）· `TestAgentLoader_LightPathMarksMCPAgentsUnverified` · `TestAgentLoader_NoMCPConfigKeepsTwoStateBehavior`；**夹具改造 ②**：`newTestLoader` / `newTestLoaderWithTools` 扩 `MCPServers` / `MCPConnected` 参数（随已批准接口适配）；`TestAgentLoader_ParsesAllFrontmatterFields` 的构造改为注入 `MCPServers: []string{"search"}` + `MCPConnected: []string{"search"}`（§3.5 改造点 ③ 的**判据面变更**：语义从「配置存在」升级为「已配置且已连接」）。DoD：**测试准备**——4 条落地；逐字红态（规则 6 未查连接 → 拒载转收；第三态未实现 → `Unverified` 为 false）；转绿前提 = T018
- [x] T018 [US2] `internal/workspace/agent.go`（改动——§3.5 改造点 ③）：`loadOne` 增 `mcpConnected` 参数；规则 4 用**注册后**名字集并加待确认分支（`mcpConnected == nil && len(mcpNames) > 0` 且工具非内置 / server 已配置 → defer，不立即拒）；规则 6 **先判已配置、再判已连接**；两条规则各按声明序报首个、规则 4 先于 6；收尾 `defer && 无拒绝 → Unverified=true, Registered=false, RejectReason=""` + INFO `Agent 注册待确认`。DoD：**由红转绿**——T017 **4 条全 PASS**；`go test -race -count=1 ./internal/workspace ./internal/app` 全绿
- [x] T019 [US2] `internal/app/wire_mcp_test.go`（追加 **3 条**）：`TestWire_AgentDeclaringFailedMCPToolIsNotRegistered`（**R3 后半**，§4.3 代码原样）· `TestWire_AgentDeclaringFailedServerOnlyIsNotRegistered`（规则 6）· `TestWire_ConnectCanceledAbortsAndClosesPool`（连接器返回 ctx 错误 → 装配失败 + 池 `Close` 被调用）。DoD：**测试准备**——3 条落地；逐字红态（日志无 `tool not registered` / 取消后池未关闭）；转绿前提 = T022
- [x] T020 [US2] `internal/cli/profile_test.go`（追加 **2 条**，`package cli`）：`TestRenderProfileList_UnverifiedLine`（逐字断言，行首两个空格，正文 = `<name>：registered=unverified——MCP 工具与连接需重路径确认`）· `TestRenderProfileList_ZeroUnverifiedKeepsSummaryBytes`（零计数汇总与 US-3 逐字节相同；>0 时追加 `/ 未验证 %d`，斜杠前一个空格）。DoD：**测试准备**——2 条落地；逐字红态（无 unverified 分支 → 行文不符）；转绿前提 = T021
- [x] T021 [US2] `internal/cli/profile.go`（改动——§3.5 改造点 ④ 的展示面）：`renderProfileLine` 增 unverified 分支、`renderProfileList` 汇总在**未验证数 > 0** 时追加后缀；`registeredCount` 语义不变（只数 `Registered`）；**签名以仓库为准**（`(out io.Writer, …)`、无返回值，见 Notes ②）。DoD：**由红转绿**——T020 **2 条全 PASS**；`go test -race -count=1 ./internal/cli` 全绿（零 MCP 配置断言逐字节不变）
- [x] T022 [US2] `internal/app/wire.go`（改动——§3.5 改造点 ⑥）：装载器注入 `MCPConnected`（实际连接数对应的名字集）；池创建后的失败路径统一走本地 `fail` 闭包（先 `pool.Close()` 再 `abortRuntime`）。DoD：**由红转绿**——T019 **3 条全 PASS**；`go test -race -count=1 ./internal/app` 全绿
- [x] T023 [US2] **US2 验证**：复跑 `./internal/tool ./internal/workspace ./internal/app ./internal/cli`；对 R2 / R3 后半 / R4 做逆改点红态复现（`-overlay`：Connect 失败改回返回错误 → R2 红；装载器传 nil `MCPConnected` → R4 第二组与 R3 后半红）。DoD：四包全绿；三条逆改红读数各一行；探针用毕即删

**Checkpoint**: US2 独立可验——坏 server 不阻断、准入结论与轻路径一致

---

## Phase 5: User Story 3 - 凭证与子进程边界：配置 fail-closed、env 只允许占位、子进程环境可预测（Priority: P3）

**Goal**: 配置校验全部拒绝式且点名；`${VAR}` 在装配期解析，未设置或空值 → 拒绝启动；错误与日志永不回显配置值；子进程环境 = 透传白名单 + 声明覆盖、cwd = 工作区根、command 只按空白切分、不经 shell。

**Independent Test**: ①负向配置逐条拒 + 错误不含被拒值（R1a，T008 已落）；②未设置 / 空值变量拒 + 点名 server 与变量（R1b）；③子进程环境恰为「白名单 + 声明」、声明覆盖、空透传非 nil 空集；④cwd = 根；⑤引号不被解释、无 `sh -c`、无 `cmd /c` 包裹。

### Tests for User Story 3

- [x] T024 [US3] `internal/app/mcp_test.go`（新增 **1 条**，`package app`）：**`TestResolveMCPEnv_MissingOrEmptyFailsClosed`**（**R1b**，§4.3 代码原样：lookup 注入——未设置与空值同档拒绝、错误点名 `server <name>` + `env.<key>` + 变量名、不含值；`"tok-1"` 通过并取到解析值）。DoD：**测试准备**——1 条落地；逐字构建错误（`undefined: resolveMCPEnv`）；转绿前提 = T025
- [x] T025 [US3] `internal/app/mcp.go` + `internal/app/wire.go`（改动——§3.5 改造点 ⑥）：`resolveMCPEnv(specs, lookup)`（生产 `os.LookupEnv`；未设置**或空串** → 拒绝、点名、不回显值；解析值不进错误与日志）；wire 在 `Connect` 之前接入。DoD：**由红转绿**——T024 **1 条 PASS**；`go test -race -count=1 ./internal/app` 全绿
- [x] T026 [US3] `internal/tool/mcp_test.go`（追加 **2 条**）：`TestMCPPool_DialCommandEnvAndDir`（透传 + 声明覆盖 + `Dir` = 根 + `cmd.Cancel == nil`（非 `CommandContext` 创建）；不真起进程——检查构造出的 `exec.Cmd`）· **`TestMCPToolAdapter_ProtocolErrorOmitsSensitiveDetail`**（**R12 后半**：server handler 错误哨兵不进 `err.Error()`）。DoD：**测试准备**——2 条落地；登记红态读数（dial 未合环境 / 协议错误原文外泄，按实测逐字）；转绿前提 = T027
- [x] T027 [US3] `internal/tool/mcp.go` + `internal/tool/mcptools.go`（改动）：`dialCommand` 按 §2.3 落地（`strings.Fields` 切分、`exec.Command`、`Dir` / `Env`、`mcp.CommandTransport`，**不得**自动包裹 `cmd /c`、**不得** `sh -c`）；协议 / 传输错误只把原始错误放进 `*core.ExternalError` 的 `Cause`，`Reason` 用固定文案。DoD：**由红转绿**——T026 **2 条全 PASS**；`go test -race -count=1 ./internal/tool` 全绿
- [x] T028 [US3] **US3 验证**：复跑 R1a（T008）、R1b（T024）、R12 前后半（T015 / T026）；对 R1a / R1b / R12 做逆改点红态复现（`-overlay`：`isEnvPlaceholder` 放行明文 → R1a 红；`resolveMCPEnv` 改回静默空串 → R1b 红；日志 / Reason 拼接原始错误 → R12 红）。DoD：三处红→绿读数各一行；错误文本**逐字**核对不含被拒值；探针用毕即删

**Checkpoint**: US3 独立可验——配置与子进程边界 fail-closed

---

## Phase 6: User Story 4 - 同形、可审计与生命周期：来源透明、两阶段审计、预算与进程分离、停机有序（Priority: P4）

**Goal**: 适配器与内置工具完全同形；调用经执行器两阶段审计；`IsError` / 协议错误 / 取消三档分类与正文边界固定；分页重复游标即失败；预算释放后子进程存活到池关闭；停机顺序 MCP → 库 → 锁、关闭幂等。

**Independent Test**: ①阻塞 server 期间审计已见 `pending` 行；②三档错误分类逐字；③分页与重复游标终止；④真实子进程预算释放后仍可调用（集成层）；⑤`Close` 幂等 + 停机顺序以库版本读数验证；⑥改配置不重建连接（重启生效）。

### Tests for User Story 4

- [x] T029 [US4] `internal/tool/mcp_test.go`（追加 **7 条**，补齐 18）：**`TestMCPToolAdapter_MapsIsErrorToExternalError`**（**R5**：server 文本含哨兵 → Reason 固定 `MCP 工具返回错误：<server>/<tool>`、文本不回显）· **`TestMCPToolAdapter_ContextIdentityWinsOverExternalError`**（**R6**）· **`TestMCPToolAdapter_CallIsAuditedBeforeExecute`**（**R9**：阻塞期间 `pending` 已落库、放行后同一行回填终态）· **`TestListAllTools_RepeatedCursorFails`**（**R7 后半**）· **`TestMCPPool_CloseIsIdempotent`**（**R10 前半**：先断言连接数 = 1、池内 `sessions` 非空；两次 `Close` 均无错；`Close` 后在 **5s 限定**内观察 `session.Wait()` 返回；逆改点 = Close 完全不关闭会话 → Wait 超时红）· **`TestMCPCallArguments_PreservesLargeIntegers`**（**R13**：`9007199254740993` 原样序列化）· `TestMCPPool_ConnectReturnsErrorOnlyOnContextCancellation`；同批落 `fakeToolLister` / `recordingAudit`。DoD：**测试准备**——7 条落地（`grep -c '^func Test'` → 18）；登记红态读数（`IsError` 当成功返回 / 取消被包成外部错误 / 审计无 pending 等，逐字）；转绿前提 = T030
- [x] T030 [US4] `internal/tool/mcp.go` + `internal/tool/mcptools.go`（改动）：`Execute` 错误边界按 §2.3 收口——`ctxPreferred` 先判 → ctx 原错误；`IsError=true` → `*core.ExternalError{Target: "mcp:<server>/<tool>", Reason: "MCP 工具返回错误：<server>/<tool>"}`（server 文本整段丢弃，不进 Cause）；传输 / 协议错误 → `Reason: "MCP 调用失败：<server>/<tool>"`、原始错误只进 `Cause`；非法入参 → `*core.InvalidInputError{Tool: <tool>, Reason:"MCP 工具入参必须是 JSON 对象"}`；`Connect` 只在 ctx 取消 / 截止返回错误；`Close` 幂等（互斥取走会话、置空、逐步 `errors.Join`）；`listAllTools` 重复游标 → 失败。DoD：**由红转绿**——T029 **7 条全 PASS**（`./internal/tool` 18 条全绿）；`go vet` / `gofumpt -l` 退出码 0
- [x] T031 [US4] `internal/tool/mcp_integration_test.go`（新增 **5 条 + 辅助入口**，`package tool_test`，`//go:build integration`）：`TestMCPIntegration_ConnectListAndCallOverStdio` · **`TestMCPIntegration_CallSucceedsAfterConnectBudgetReleased`**（**R11**）· `TestMCPIntegration_BadCommandIsSkipped`（R2 真进程版）· `TestMCPIntegration_PaginationOverStdio` · `TestMCPIntegration_ServerDiesMidCall`；辅助入口 `TestMCPHelperProcess`（调 `testutil.RunMCPHelper`）与 `helperCommand`。路径含空白 → `t.Skip("测试二进制路径含空白——command 按空白切分，无法表达")`，跳过如实登记。DoD：`GOPROXY=https://goproxy.cn go test -tags=integration ./internal/tool -run 'TestMCPIntegration|TestMCPHelperProcess' -v` → 5 PASS + 顶层 helper SKIP；若跳过条件命中，登记「跳过 + 依据」，**不得**写通过
- [x] T032 [US4] `internal/app/wire_mcp_test.go`（追加 **1 条**）：**`TestWire_CloseClosesMCPBeforeDatabase`**（**R10 后半**：假连接器的 `Close` 里 `rt.db.SchemaVersion` 仍成功——顺序错了这里红）。DoD：**测试准备**——1 条落地；逐字红态（`heavyRuntime.Close` 尚未持池 → onClose 未触发）；转绿前提 = T033
- [x] T033 [US4] `internal/app/wire.go`（改动——§3.5 改造点 ⑥）：`heavyRuntime` 增 `mcp mcpConnector`，`Close` 顺序改为 **MCP → 数据库 → 实例锁**；失败路径池关闭沿用 T022 的 `fail` 闭包；关闭错误 `errors.Join`、**不得**静默。DoD：**由红转绿**——T032 **1 条 PASS**；`go test -race -count=1 ./internal/app` 全绿
- [x] T034 [US4] **US4 验证**：复跑 `./internal/tool`（含 `-tags=integration`）**与 `./internal/app`**（R10 后半的测试 `TestWire_CloseClosesMCPBeforeDatabase` 位于 `./internal/app`，须单跑一次：`GOPROXY=https://goproxy.cn go test -race -count=1 ./internal/app -run '^TestWire_CloseClosesMCPBeforeDatabase$' -v`）；对 R5 / R6 / R9 / R10 / R11 / R13 做逆改点红态复现（`-overlay`：`IsError` 当成功 → R5 红；ctx 判序后置 → R6 红；`BeginInvocation` 挪到执行后 → R9 红；**R10a** `Close` 完全不关闭会话 → `session.Wait()` 超时红；**R10b** `Close` 顺序改回库先 → onClose 里 `rt.db.SchemaVersion` 返回错误、`rt.Close()` 经 `errors.Join` 上抛 → `TestWire_CloseClosesMCPBeforeDatabase` 红；`exec.Command` 换 `exec.CommandContext` → R11 红；入参先 `json.Unmarshal` 到 map → R13 红）。DoD：七处红→绿读数各一行（R10 拆 a/b 两条）；集成层读数粘贴；**须记录 `./internal/app` 单跑时该测试实际失败的逐字读数**（而非只看退出码）；探针用毕即删

**Checkpoint**: US4 独立可验——同形、审计与生命周期闭环

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: 上游文档同步、依赖与零变更面收口、全量门禁、交付物逐项核对

- [x] T035 [P] `docs/Compatibility.md` §3（**增补 3 行有意差异**——§3.5 改造点 ⑨，文档同步）：非 stdio → 拒绝启动；非文本内容 → 类型化占位（不回显 base64）；MCP 子进程环境 = 白名单 + 声明覆盖。DoD：三行逐条对照 §3.3 送审；`git diff -- docs/Compatibility.md` 只有 `+` 行、无历史行改写；措辞不含「MCP 工具受沙箱约束」类表述（FR-025）
- [x] T036 依赖与零变更面收口：`GOPROXY=https://goproxy.cn go mod tidy`；核对 `git diff go.mod` **只多** `github.com/modelcontextprotocol/go-sdk v1.8.0` 一条**直接**坐标、无表外直接坐标；`git diff -- docs/contracts/schema.sql basalt.yaml.example Makefile .github/ scripts/` 全空；`git status --short internal/storage/migrations/` 无新文件。DoD：逐条读数粘贴；`go mod tidy` 后再跑一次 `go build ./...`
- [x] T037 全量门禁：`make check` 七目标（`fmt lint sec test test-integration cover build`）；**先查退出码再判输出**（gofmt / gofumpt 陷阱）；`-race` 与 `CGO_ENABLED=0` 分属不同目标；`test-integration` **首次实跑**（本仓首个集成层）；覆盖率 `core` / `tool` / `memory` / `storage` ≥ 70%、其余 ≥ 50%。DoD：七目标逐条四态登记（通过 / 失败 / 跳过 / 未验证，跳过给依据）；覆盖率读数逐包粘贴
- [x] T038 交付物逐项核对 + 收口证据归集：以 `git status --porcelain` + 显式路径清单核对 **6 新增代码 + 6 改动代码 + 6 新增测试（默认层 46 + 集成层 5 = 51 条）+ 2 改动测试（6 条）+ 夹具 2 点 + go.mod 1 坐标 + Compatibility 3 行**；`grep -rln 'modelcontextprotocol' --include='*.go' internal` 只命中 `internal/tool` 与 `internal/testutil`；`go list -f '{{.Imports}}' ./internal/core` 只出标准库；抑制注释零命中。DoD：逐项存在性读数；与 [plan.md](./plan.md)「文档 ↔ 仓库事实差异」B1 / B2 对照，记录未偏离；本任务不产码、不改冻结文档

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1（Setup）**: 无依赖，先做 T001~T003
- **Phase 2（Foundational）**: 依赖 Phase 1；T004~T007 完成后 US1 才可开工
- **Phase 3（US1）**: 依赖 Phase 2；T008 → T009；T010 → T011；T012 → T013；T014 收尾
- **Phase 4（US2）**: 依赖 US1（连接链路与装配段）；T015 → T016；T017 → T018；T019 / T020 → T021 / T022；T023 收尾
- **Phase 5（US3）**: 依赖 US1（解析与装配）与 US2（连接失败日志）；T024 → T025；T026 → T027；T028 收尾
- **Phase 6（US4）**: 依赖 US1~US3；T029 → T030；T031 集成层；T032 → T033；T034 收尾
- **Phase 7（Polish）**: 依赖全部故事；T035 / T036 可并行；T037 最后跑；T038 归集证据

### Story Completion Order

`US1（P1）→ US2（P2）→ US3（P3）→ US4（P4）`。故事内测试任务先于对应实现任务（或与新增文件同批的两段式 DoD）；跨故事复验的用例在验证任务里用 `-overlay` 取得逆改红态。

### Parallel Opportunities

- T004（`core/mcp.go`）与 T005（`core/profile.go`）不同文件，可并行
- T008（workspace 测试）与 T010（tool 测试）在各自包内独立，不依赖彼此实现，可并行
- T012（app 测试）与 T013（装配实现）之间必须按测试先行顺序；但 `docs/Compatibility.md`（T035）与代码任务无依赖，可随时并行
- 测试文件跨包（workspace / tool / app / cli / testutil）互不冲突，可并行编写；同一文件内的追加任务（T015 / T026 / T029）必须按顺序

---

## Implementation Strategy

### MVP First（仅 US1）

1. 完成 Phase 1 + Phase 2（T001~T007）
2. 完成 US1（T008~T014）——零代码接入正向链路可演示（Demo 三的自动化半面）
3. **停点自检**：零配置逐字节不回归、R3 前半 / R8 绿、US1 独立测试全绿

### Incremental Delivery

1. US1 → 独立验收（连接 + 注册 + 同形可用）
2. US2 → 独立验收（故障隔离 + 规则 4/6 + 第三态）
3. US3 → 独立验收（配置 fail-closed + 子进程边界）
4. US4 → 独立验收（审计 + 错误边界 + 生命周期）
5. Polish（T035~T038）→ 门禁全绿 + 交付物核对

### Notes

1. **R3 标签漂移**：§3.2 把 `TestWire_StartupSummaryCountsConnectedServers` 标为「R3 后半」，但 §4.3 的 R3 只有两个函数（`TestWire_AgentDeclaringFailedMCPToolIsNotRegistered` 与 `TestWire_MCPToolsRegisteredAfterEqualityGate`）。本清单保留该用例，按 **FR-027 / 约束 23**（启动摘要 `mcp_connected`）归类，挂在 T012；**不**把它算作 R3 的锚点。R3 的锚点 = T012 + T019。
2. **CLI 渲染函数签名以仓库为准**：§3.1 写 `renderProfileList(entries []core.AgentEntry) string` / `renderProfileLine(entry core.AgentEntry) string`，仓库实际是 `func renderProfileList(out io.Writer, entries []core.AgentEntry)` / `func renderProfileLine(out io.Writer, entry core.AgentEntry)`（`internal/cli/profile.go:296/306`，无返回值）。T021 **保持仓库签名**，只改函数体；不按文档签名重构。
3. **SDK 包边界（约束 25 的解释）**：`modelcontextprotocol/go-sdk` 出现在 `internal/tool`（生产）与 `internal/testutil`（测试基建）两处；约束 25 的「只出现在工具包」按**生产包**解释，`testutil` 是跨包测试基建、不参与运行时装配。判据：`grep -rln 'modelcontextprotocol' --include='*.go' internal` 恰好命中这两处（T038 复核）。
4. **故事标签**：`[US1]`~`[US4]` 是 spec 内部编号，不是项目台账的 US-1~US-7；四个故事同属项目 US-7（见 [spec.md](./spec.md) 标签口径段）。
5. **前序改造点软门禁**：①③④⑤⑨ 的性质已在本文件头部标注，随步骤 5 停点一次性送审；停点确认后执行阶段不再逐条停车。
6. **零新增交付物**：本清单不含颗粒度文档 §3 之外的导出类型 / 配置键 / 数据表 / REST 路径 / frontmatter 字段；30s 预算与全部文案是代码常量。

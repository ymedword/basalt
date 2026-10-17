# Implementation Plan: US-7 MCP 集成（stdio 客户端 + 工具适配 + 故障隔离）

**Branch**: `007-mcp` | **Date**: 2026-10-17 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/007-mcp/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 7` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us7-mcp.md](../../docs/us/us7-mcp.md) §2 / §3 为准，冲突时以 [docs/TechnicalSolution.md](../../docs/TechnicalSolution.md) 为准（§1.1 决策 1 允许协议 SDK / §1.2 栈表 MCP 坐标 U-5 / §6.3 零代码扩展 / §6.4 注册池与准入 / §6.5 授权矩阵与信任边界 / §8.8 `${VAR}` 装配期解析 / §10.1 包结构 / §11.5 热加载范围 / §13 US-7 行）；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准（`agent-md.md` 的 `mcp_servers` 字段与规则 4 / 6 文案、`run-and-errors.md` 的错误分类七值）。

## Summary

给 Basalt 装上**零代码接入外部能力**的路：运营者只写两处配置——工作区根部的 `mcp_servers.yaml`（登记现成 MCP server）与 `AGENT.md` 的 `tools:` / `mcp_servers:` 声明——不改一行代码，外部工具就进入工具清单、可被模型调用、可审计。四件可判卷的价值：**① 零代码扩展走通**（编程指南 Demo 三：一个配置文件 + 一个现成 server）；**② 故障隔离**（某个 server 起不来 → WARN 点名、关闭半截会话、继续下一个，启动照常完成；依赖它的 Agent 因工具解析不到而整只不注册）；**③ 来源透明**（MCP 工具与内置工具在注册、声明、调度、审计、失败分类上完全同形——循环与执行器看不到"这是 MCP 工具"）；**④ 信任边界写在明面上**（server 是独立进程，其文件 / 网络行为不经 Policy 是**有意接受的边界**，可治理面 = 管理员登记的那一行配置）。

技术路线的四个要点：**① 配置面——一个解析器、全量 fail-closed**——`internal/workspace/mcp.go` 的 `ParseMCPServers` 读「顶层映射 + `servers:` 列表」，**替换** US-1~US-6 的临时占位 `readMCPServerNames`（顶层键名集，位于 [internal/workspace/agent.go:368](../../internal/workspace/agent.go#L368)）；形态、必填、重名、`env` 精确 `${VAR}`、`transport` 取值域逐条拒绝启动并点名；缺失 / 空 / `{}` / 无 `servers` → 零 server、整段跳过；`env` **只校验形态、不读环境**（解析责任在装配层）。**② 连接面——串行、预算只罩握手、进程不绑预算**——`internal/tool/mcp.go` 的 `MCPPool` 持单 `mcp.Client`、多会话；每 server 依次 dial（`exec.Command` 起 `mcp.CommandTransport`，**不接收 ctx**）→ initialize → tools/list，两步共享**一个 30 秒代码常量预算**（`ConnectTimeout == 0` → 30s）；列工具按 `NextCursor` 分页到空、重复游标即失败；任一 server 失败 → WARN（固定键 `server` / `reason` / `detail`=固定短语）→ 关闭半截会话 → 继续下一个；`Connect` 只在整体 ctx 取消 / 截止时返回错误；`Close` 幂等、逐会话收尾子进程。**③ 适配与注册面——同形、原样、错误边界自产文案**——`internal/tool/mcptools.go` 的 `mcpToolAdapter` 实现 `core.Tool` 四方法；名称 / 描述 / `InputSchema` 原样；入参经 `mcpCallArguments` 返回**原始字节**（空 / `null` → `{}`，**不得**经 `float64`）；结果渲染固定（文本换行连接 / 非文本类型化占位 / 结构化兜底 / 空输出固定文案）；`IsError` → `*core.ExternalError`（Reason 固定、**server 文本整段丢弃**）；传输 / 协议错误 → `*core.ExternalError`（Reason 固定、原文只进 `Cause`）；ctx 取消 / 截止经 `ctxPreferred` **身份优先**；注册顺序 = 内置 8 → MCP（文件序 → 工具序），重名 keep-first + WARN，**注册发生在等值门禁之后**（门禁在 [internal/app/wire.go:248](../../internal/app/wire.go#L248)，只比对八件内置工具）。**④ 装配与停机面——显式注入、两路径一致、顺序可观察**——`internal/app/mcp.go` 的 `resolveMCPEnv`（lookup 可注入）与 `mcpConnector` 测试夹层；`wire.go` 在门禁后插入「解析 → env 解析 → 建池 → Connect → 注册 → 装载器注入 `MCPServers` / `MCPConnected` → 摘要 `mcp_connected`」；轻路径不连接，对可能依赖 MCP 的 Agent 给第三态 `unverified`（只改展示与摘要，不改运行面）；`heavyRuntime.Close` 顺序改为**池 → 库 → 锁**。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）

**Primary Dependencies**: **+1 直接坐标** `github.com/modelcontextprotocol/go-sdk v1.8.0`（宪法架构约束技术栈表 U-5 已批准；2026-10-17 经模块代理核实为最新 GA——写前按纪律复跑 `GOPROXY=https://goproxy.cn go list -m -versions github.com/modelcontextprotocol/go-sdk`）。其余全部既有：标准库（`context` / `encoding/json` / `errors` / `fmt` / `log/slog` / `os` / `os/exec` / `slices` / `strings` / `sync` / `time`）+ 既有 `goccy/go-yaml v1.19.2`（`workspace` 解析，沿用）+ 既有 `stretchr/testify v1.12.1`（测试）。SDK 的**传递依赖**随 `go mod tidy` 落地；`go.mod` 的直接依赖区**只许多这一条**（判据 `git diff go.mod`）。零 `import _` 副作用注册。

**Storage**: **零迁移、零表 / 列 / 索引改动**。MCP 会话、工具清单、连接状态全在内存；审计继续落既有 `tool_invocations`（`tool` 列 = 工具名原样，`error_kind` 取七值之一 `external_error` / `timeout` / `invalid_input` 等），两阶段写仍由 `core.ToolExecutor` + `storage.ToolInvocationRepo` 承载，**适配器自身零写入**。判据：`git diff -- internal/storage/schema.sql docs/contracts/schema.sql` 为空；`internal/storage/migrations/` 零新增文件。`mcp_servers.yaml` 是**工作区文件**（`<workspace>/mcp_servers.yaml`），不是库表、不占 `basalt.yaml` 的键。

**Testing**: 默认层 `go test -race ./...`——**新增 6 个测试文件共 51 条**（`workspace/mcp_test.go` **18** / `tool/mcp_test.go` **18** / `tool/mcp_integration_test.go` **5**（`//go:build integration`，**默认层不执行**）/ `app/wire_mcp_test.go` **7** / `app/mcp_test.go` **1** / `testutil/mcpserver_test.go` **2**）+ **改动 2 个测试文件追加 6 条**（`workspace/agent_test.go` +4，现有 24 → **28**；`cli/profile_test.go` +2，现有 8 → **10**）+ **夹具改造 2 点**（`writeMCPServers` 从顶层键名集改为 `servers:` 列表；`newTestLoader` / `newTestLoaderWithTools` 扩 `MCPServers` / `MCPConnected` 两个参数或加变体）。默认层合计新增 **52** 条可执行用例（51 中减去集成层 5，再加改动追加 6），集成层 **5** 条由 `-tags=integration` 单独执行。**默认层不访问真实网络、不打真实模型、不起真实子进程**：MCP 用 SDK 的 `mcp.NewInMemoryTransports()`（进程内管道，协议编解码为真、进程边界为假）与 app 层假连接器（`fakeMCPConnector`）；真实文件系统（`t.TempDir()`）与真实 SQLite 库文件允许（与 US-4~US-6 同口径）。**集成层是本仓第一个起真实子进程的集成层**：测试二进制自举（`os.Args[0] -test.run=^TestMCPHelperProcess$` + 子进程 env `BASALT_MCP_HELPER=1`），`os.Args[0]` 含空白时 `t.Skip("测试二进制路径含空白——command 按空白切分，无法表达")` 并在收口报告如实登记「跳过 + 依据」（陷阱 55，不得写成通过）。**测试缝纪律**：app 层用例经 `fx.app()` 取 `*App` 后直接设置包内字段 `mcpConnect`；tool 层白盒用例直接替换非导出字段 `dial`（`mcp_test.go` = `package tool`，与 `runner_windows_test.go` 同包先例）；**不得**新增导出测试 API（`SetDialForTest` / `NewMCPToolAdapterForTest` 一类）。并发面由 `-race` 覆盖（`Close` 与在途调用交错、会话列表互斥、两阶段审计），`require` 一律在主 goroutine。关键回归 **R1a / R1b / R2~R13（14 条守卫、18 个测试函数）** 原样落地，函数名英文、文档中文验收点以注释保留；每条带逆改点（见颗粒度文档 §4.3）。覆盖率：`tool`（新主体）/ `core`（新增纯类型）≥ 70%，`workspace` / `app` / `cli` / `testutil` ≥ 50%，由既有 `make cover` 按包判失败（**不改脚本与阈值**）。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物 `CGO_ENABLED=0` 交叉编译（四目标编译矩阵是人工项，不进 `make check`）；`-race` 与发布构建**分属不同命令 / job**。集成层的子进程自举在 Linux CI 同样成立（`os.Args[0]` 无空白）；Windows 路径含空白时显式跳过。命令解析无 shell 语义（`strings.Fields`），Windows 上 `.cmd` 需显式写解释器——这是**已登记的已知边界**，不是本 US 修复项。

**Project Type**: CLI + 服务单二进制；本 US **不新增内部包、不新增顶层目录、不新增命令**——全部落在既有 `internal/{core,workspace,tool,app,cli,testutil}` 内。

**Performance Goals**: 无吞吐 / 延迟指标（底座零件类 feature）；判据是**行为顺序与可解释边界**——连接**串行**（不做并发连接）；每 server 握手预算 **30 秒代码常量**且只罩 initialize + tools/list；列工具分页到空游标且必须有终点；调用入参大整数（> 2^53）逐字节保真；`mcp_servers.yaml` **不热加载**（改文件 → 重启生效，零文件监听）。启动期的成本上界 = `server 数 × 30s`（最坏情形如实登记，不包装成承诺）。

**Constraints**: 三条包纪律——`core` **只依赖标准库**（`internal/core/mcp.go` 纯类型，零 import 内部包）；`workspace` **不得** import `tool`（解析产出 `[]core.MCPServerConfig`，名字集以 `[]string` 注入装载器）；`tool` **不得** import `app` / `cli`；`app` 是唯一装配层；MCP SDK **只出现在生产工具包 `internal/tool`**，`internal/testutil` 的 SDK 使用仅限测试基建（进程内传输与工具夹具；见「文档 ↔ 仓库事实」§3 的解释与判据）。**接口定义在消费方**：`mcpConnector` 由 `app` 声明（`Connect` / `Close` 两方法）、`toolLister` 由 `tool` 本包声明（`ListTools` 单方法）、`core.Tool` 由 `core` 声明。**审计零改动**：适配器不落审计、不写消息、不记日志；两阶段写仍只在既有执行器链路（`grep -rn '\.BeginCall(' --include='*.go' internal/core` 零命中）。**同步执行**：`Connect` / `Execute` 均为同步调用；`context.WithValue` 零使用（收尾逐处确认）；不建 goroutine、不引入异步编排。**进程生命周期与握手预算分离**：dial 用 `exec.Command` 而**非** `exec.CommandContext`；预算只传给 `client.Connect` 与 `listAllTools` 的 `cctx`。**命令解析**：`strings.Fields`，无引号语义、不包 `cmd /c`、不引入 shell。**安全边界**：外部正文只进 `Cause`，永不进日志 / 审计 / 模型上下文；`IsError` 的 server 文本连 `Cause` 也不放；连接失败 `detail` 只用固定短语集。**范围零膨胀**：零新增配置键、零迁移、`basalt.yaml.example` / `Makefile` / CI 零改动、`schema.sql` 零 diff；30s 预算与全部文案是代码常量。**静态门禁**：抑制注释（`//nolint` / `#nosec` / `//lint:ignore`）**零使用**；测试函数名英文 `TestXxx_yyy`。**测试缝**：只通过包内字段（`mcpConnect` / `dial`）注入，不新增导出测试 API。

**Scale/Scope**: 代码 **新增 6 文件 + 改动 6 文件 + `go.mod` / `go.sum` +1 直接坐标**（`core` 1 新 / `workspace` 1 新 2 改 / `tool` 2 新 1 改 / `app` 1 新 1 改 / `cli` 1 改；`testutil` 1 新）；测试 **新增 6 文件 51 条 + 改动 2 文件追加 6 条 + 夹具改造 2 点**；`docs/Compatibility.md` §3 **增补 3 行有意差异**（非 stdio 拒启动 / 非文本类型化占位 / 子进程环境白名单，§3.5 改造点 ⑨）；前序改造点 **9 项**（逐条属软门禁——§3.5，其中 ①③④ 改变既有测试的判据面、⑨ 是上游文档同步）+ 后续占位 **6 项**（只留位不实现）。

**环境实测读数（2026-10-17，本轮当场命令）**: 分支 `007-mcp`，HEAD `8d9bdfb8727933cc10f27ef6cad62e180648dd7d`；`go.mod` 对 `modelcontextprotocol` **零命中**；全仓 `grep -rn 'MCPPool\|MCPServerConfig\|mcpToolAdapter\|ParseMCPServers' --include='*.go' internal cmd` **零命中**（交付面全新增）。改造点逐条核对属实——`readMCPServerNames` 在 [internal/workspace/agent.go:368](../../internal/workspace/agent.go#L368)（`LoadAll` 于 `:166` 调用；`AgentLoaderOptions` 三字段在 `:76`；规则 4 在 `:270`；规则 6 在 `:278`，注释逐字「存在性半条；「连接成功」半条随 US-7」）；等值门禁在 [internal/app/wire.go:248](../../internal/app/wire.go#L248)（装载器在 `:256`，三字段注入）；`childEnv` 在 [internal/tool/runner.go:266](../../internal/tool/runner.go#L266)（透传 + 声明覆盖 + 非 nil 空切片语义属实）；`heavyRuntime` 在 `wire.go:396`（六字段，无 MCP），`Close` 在 `:426` 现行顺序 **db → lock**；`templates.go` 的 `templateMCPServersYAML` 在 `:89`（注释「键为 server 名，值为启动配置」+ `{}` 字节，US-1 判据在 `:44-45`）；轻路径装载器在 [internal/app/ports.go:101](../../internal/app/ports.go#L101)（只传 `Providers` / `Tools: declaredToolNames` / `Logger` → 新字段零值 = 轻路径的第三态入口）。既有测试条数当场读取——`workspace/agent_test.go` **24**（`writeMCPServers` 在 `:150`、`newTestLoader` 在 `:206`、`newTestLoaderWithTools` 在 `:216`、`TestAgentLoader_RejectsUnconfiguredMCPServer` 在 `:528`、`TestAgentLoader_ParsesAllFrontmatterFields` 在 `:641`）、`cli/profile_test.go` **8**、`app/wire_test.go` **8**、`app/ports_test.go` **10**、`tool/runner_test.go` **12**；测试包归属实测——`workspace/agent_test.go` = `package workspace_test`、`app/wire_test.go` = `package app`、`cli/profile_test.go` = `package cli`、`tool/mcp_test.go` 落 `package tool`（`runner_windows_test.go` / `httptools_test.go` / `shelltools_test.go` 均为 `package tool`；`policy_test.go` / `filetools_test.go` / `memorytools_test.go` / `registry_test.go` / `runner_test.go` 为 `package tool_test`）。`internal/cli/profile.go` 现签名实测：`renderProfileList(out io.Writer, entries []core.AgentEntry)` / `renderProfileLine(out io.Writer, entry core.AgentEntry)`（无返回值）。**本轮未复测的项不在此声称**（**未跑** `make check`、**未跑**四目标编译、**未读**覆盖率读数）。工具链读数沿用 US-1 / US-4 / US-5 plan。

### 文档 ↔ 仓库事实差异（实测发现）

**A. 与文档一致、作为落地基点的仓库事实**（逐条已核，不构成差异）：

| # | 文档写法 | 仓库实测 | 落地口径 |
| --- | --- | --- | --- |
| 1 | §2.2 约束 1：`readMCPServerNames` 是 US-1~US-6 的临时占位，「连接成功」半条随 US-7 | [agent.go:368-398](../../internal/workspace/agent.go#L368-L398) 顶层键名集解析器在案；`:278` 注释逐字预告随 US-7 | **替换而非叠加**：删除 `readMCPServerNames`，`LoadAll` 改调 `parseMCPServers(base)`（`MCPServers == nil` 时）；同一文件只留一份解析器 |
| 2 | §2.2 约束 9：等值门禁在 `wire.go:248`，必须先于 MCP 注册 | [wire.go:248](../../internal/app/wire.go#L248) `registry.Names() == declaredToolNames` 在案；装载器 `:256` 紧随其后 | MCP 注册段插在 `:252`（门禁之后）与 `:256`（装载器之前）；门禁表达式与比较对象**零改动** |
| 3 | §2.2 约束 17：`childEnv` 抽出 `mergeChildEnv` 与 MCP dial 共用 | [runner.go:266](../../internal/tool/runner.go#L266) 现函数含透传 + 声明覆盖 + 非 nil 空切片语义 | **等价重构**：抽 `mergeChildEnv(passthrough, declared)`，`childEnv` 改为调用它；既有 runner 用例原样全绿（零判据变化） |
| 4 | §2.2 约束 20：`heavyRuntime.Close` 改为池 → 库 → 锁 | [wire.go:426-435](../../internal/app/wire.go#L426) 现行 **db → lock**，注释「先关库…再放实例锁」 | 改为 `mcp → db → lock`，`errors.Join` 保留；`heavyRuntime` 增 `mcp mcpConnector` 字段；注释同步为三步理由 |
| 5 | §3.1：`templates.go` 模板**只改注释**、`{}` 字节不动 | [templates.go:89-92](../../internal/workspace/templates.go#L89) 注释为顶层键名集形态；`:44-45` 判据「空表且可解析为空映射」 | 注释改为 `servers:` 列表形态与字段说明；`{}` 字节逐字不动（`init_test.go` 零改动） |
| 6 | §4.1：`tool/mcp_test.go` 必须落 `package tool`（要替换非导出 `dial`） | `runner_windows_test.go` / `httptools_test.go` / `shelltools_test.go` 实测均为 `package tool` | `mcp_test.go` = `package tool`；`mcp_integration_test.go` 可落 `package tool_test`（只用导出面 + `testutil`） |

**B. 实测差异（两条；**不静默改颗粒度文档**，按仓库事实落地并在步骤 5 停点报告）**：

| # | 颗粒度文档写法 | 仓库实测 | 落地口径 |
| --- | --- | --- | --- |
| 1 | §3.1 给出 `func renderProfileList(entries []core.AgentEntry) string` / `func renderProfileLine(entry core.AgentEntry) string`（并注「签名不变」） | 实际签名为 `func renderProfileList(out io.Writer, entries []core.AgentEntry)` / `func renderProfileLine(out io.Writer, entry core.AgentEntry)`，**无返回值**（[profile.go:296](../../internal/cli/profile.go#L296) / [:306](../../internal/cli/profile.go#L306)） | 「**签名不变**」是操作约束、优先于文档里抄写的签名形态：保持 `(out io.Writer, entry)` 实签名，只改函数体（unverified 分支 + 汇总后缀）；新用例经 `renderProfileList(&buf, entries)` 直接断言字节（`package cli` 包内测试可调非导出函数） |
| 2 | §3.2 把 `TestWire_StartupSummaryCountsConnectedServers` 标为「**R3 后半**」 | §4.3 的 R3 只有两个函数（`TestWire_AgentDeclaringFailedMCPToolIsNotRegistered` / `TestWire_MCPToolsRegisteredAfterEqualityGate`），摘要计数不在其中；spec.md 锚点表同 §4.3（R3 = 等值门禁先于 MCP 注册） | 用例**照写**（FR-027 / 约束 23 的可判据），但其守卫类别登记为**启动摘要键**（`mcp_connected` = 实际连接数），不并入 R3；tasks 中单独成条，步骤 5 停点如实报告标签漂移 |

**C. 文档内部口径的落地解释（一条，提请步骤 5 一并确认）**：约束 25 写「`go-sdk` 只出现在 `internal/tool`」，而 §3.1 / §4.1 明确要求 `internal/testutil/mcpserver.go` 用 `mcp.Transport` / `mcp.ToolHandler` 构造进程内测试对。两者并置时按「**生产包**只出现在 `internal/tool`」解释：`internal/testutil` 是测试基建（只被测试 import，不进任何生产装配路径），其 SDK 使用仅限协议编解码夹具。机器判据：`grep -rln 'modelcontextprotocol' --include='*.go' internal` 命中集必须恰为 `internal/tool` + `internal/testutil` 两处；`internal/{core,workspace,app,cli,storage,memory,provider,channel}` 零命中。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

对照 [.specify/memory/constitution.md](../../.specify/memory/constitution.md) 十原则 + 三条架构约束 + 开发工作流（GATE 判定：通过 / 不适用 / 需说明）：

| # | 宪法条目 | 本 feature 关系 | GATE |
| --- | --- | --- | --- |
| I | 自实现 ReAct 循环 | **适用（强形态）**：`go.mod` 只加协议 SDK（U-5 已批准），**零 Agent 框架坐标**（收尾 `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中）；SDK **不接管循环**——`mcp.Client` 只用于 initialize / tools/list / CallTool，工具调度唯一入口仍是 `core.ToolExecutor`；循环体零改动、不按来源分支（约束 7） | 通过 |
| II | LLM 客户端只做协议调用 | **适用**：不启用 SDK 的自动工具循环 / 托管能力（`mcp.NewClient(impl, nil)`，无工具自动执行选项）；会话历史仍由**一条消息仓储**承载——适配器**不写消息**、不落审计、不记日志；模型响应与工具结果仍由 `ReActLoop` 统一追加 | 通过 |
| III | Provider 必须显式映射 | **不适用**（本 US 不触碰 provider 路由与映射表；`resolveMCPEnv` / `mcpConnector` 是 MCP 自己的装配面，不遍历推断） | 不适用 |
| IV | 一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露 | **适用（弱形态）**：规则 4 用**注册后**名字集（含 MCP 工具）、规则 6 补「连接成功」半条——`AGENT.md` 字段集与技能面零改动；**不得**新增 `use_skill`、**不得**预载技能正文；MCP 工具进入全局注册池后，装载判定与既有八条规则同形 | 通过 |
| V | 审计 day-one 且 fail-closed | **适用（零改动 + 一条新判据）**：MCP 调用经既有 `ToolExecutor` 两阶段写（pending 预写失败 → 不发出；R9 在 server 阻塞期间观察 pending 行）；适配器自身零写入；审计 SQL 仍只在 `internal/storage` | 通过 |
| VI | 授权与执行分离（两接口） | **适用（边界形态）**：MCP 工具**不持有** `Policy` / `CommandRunner`；MCP server 的**进程内**文件 / 网络行为不经 Policy 是**有意接受的信任边界**（技术方案 §6.5 + 文档如实登记，FR-028）；可治理面 = 管理员登记的配置行——**不得**宣称 MCP 工具受沙箱约束 | 通过 |
| VII | 同步执行 + context 只承载取消 / 截止 / 追踪 | **适用（强形态）**：`Connect` / `Execute` 同步；每 server 串行连接、不做并发编排；`context.WithValue` **零使用**；Profile / Session 等业务状态走显式类型化参数（`core.ToolEnv`）；无包级变量模拟 ThreadLocal；30s 预算只用 `context.WithTimeout` 的取消 / 截止语义 | 通过 |
| VIII | 单二进制纪律 + 依赖最小化 | **适用**：`CGO_ENABLED=0` 发布构建；`-race` 与发布构建分属不同命令 / job；依赖最小化——只加**表内已批准**的 `go-sdk v1.8.0`（直接坐标），传递依赖随 tidy；无新构建步骤 / 新 profile | 通过 |
| IX | 不用 ORM，手写 SQL | **不适用（零 SQL 增改）**：MCP 无持久化状态；`schema.sql` / 迁移零 diff；审计沿用既有三表 | 不适用 |
| X | 五大能力优先，范围卡紧 | **适用**：交付面对照颗粒度文档 §5 逐项边界（SSE / HTTP transport / 容器隔离 / resources / prompts / sampling / 重连 / 热加载 / 管理台页 / 命名空间 / server 级开关 / 工具级超时 / 二进制落盘**全不做**）；对外概念零发明（导出签名逐字以 §3.1 为准）；信任边界与「启动成本上界 = 连接数 × 30s」如实登记，不包装成承诺 | 通过 |

**架构约束**：依赖方向 `channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 唯一装配层——本 US 的落法：`core` 新增 `mcp.go`（纯配置类型，**零 import 内部包**，判据机器可判）；`workspace` 新增 `mcp.go` import `core` + 标准库 + 既有 `goccy/go-yaml`（**不** import `tool`）；`tool` 新增 `mcp.go` / `mcptools.go` import `core` + 标准库 + **MCP SDK**（**不** import `app` / `cli`，不 import `workspace`）；`tool/runner.go` 抽出 `mergeChildEnv`（标准库）；`app` 新增 `mcp.go` import `core` + `tool` + 标准库（连接器接口与 env 解析）；`cli` 只改 `profile.go` 的展示分支（不 import `tool` / MCP SDK）；`testutil` 新增 MCP 夹具（仅测试 import）。**SDK 边界**：生产包只出现在 `internal/tool`；`internal/testutil` 属测试基建（见「文档 ↔ 仓库事实」C 的解释与 grep 判据）。**接口定义在消费方**：`core.Tool`（内建接口）由适配器实现；`tool.toolLister` 与 `app.mcpConnector` 均为**消费方本地声明**，实现方（`mcp.ClientSession` / `MCPPool`）不知情——编译器钉住方向。**第三方实现放消费方包内**：MCP SDK 封装在 `internal/tool`（与 SQLite 封装在 `storage` 同构），`testutil` 的测试夹具属测试侧。无环由 `go list` 复核（现有 10 包不变）。

**开发工作流**：门禁一条命令 `make check`（七目标；`Makefile` / CI / 覆盖率脚本**零改动**）；三条实测纪律适用（格式判卷**先退出码后输出** / `-race` 与 `CGO_ENABLED=0` **分离** / **集成层首增**：`test-integration` 从空跑变实跑，必须单独执行 `go test -tags=integration ./...`）；覆盖率不可豁免（`tool` / `core` ≥70%，其余 ≥50%）；提交纪律（AI **不自动** commit / push / tag）。

**plan review 必查（宪法 §开发工作流）逐条**：

| 必查项 | 本 plan 的答复 |
| --- | --- |
| 依赖方向无环 | `core` 新文件只 import 标准库（收尾 `GOPROXY=https://goproxy.cn go list -f '{{.Imports}}' ./internal/core` 复跑）；`workspace/mcp.go` 不 import `tool`；`tool` 不 import `app` / `cli` / `workspace`；`app` 装配全部；`cmd/basalt` 不动 |
| 审计写入路径唯一 | 适配器不写审计（四方法签名无审计存储参数）；两阶段写仍只在 `core.ToolExecutor` → `storage.ToolInvocationRepo`；`grep -rn '\.BeginCall(' --include='*.go' internal/core` 零命中（不变量 11）复跑；`grep -rn 'INSERT INTO tool_invocations' --include='*.go' internal` 只在 `internal/storage` |
| 无框架坐标 | `go.mod` 只多 `github.com/modelcontextprotocol/go-sdk v1.8.0`（判据 `git diff go.mod` 直接依赖区只此一条）；框架 grep 零命中同上 |
| 无 `context.WithValue` 业务态 | 收尾 `grep -rn "context.WithValue" --include='*.go' cmd internal` 每处命中人工确认；MCP 的连接结果 / 名字集全部走显式字段与参数 |
| 单二进制构建通过 | 本 US 无新平台分片文件（SDK 纯 Go，`CGO_ENABLED=0` 可用；`os/exec` 三平台一致）；`build` 目标 `CGO_ENABLED=0 go build`；`-race` 只在 `test` 目标；四目标交叉编译是**人工项**，**不进** `make check` |
| SDK 包边界 | `grep -rln 'modelcontextprotocol' --include='*.go' internal` 命中集 = `internal/tool` + `internal/testutil`；`internal/{core,workspace,app,cli,storage,memory,provider,channel}` 零命中 |
| 无自动工具循环 | `mcp.NewClient` 的第二参数（`ClientOptions`）为 `nil`；全仓无 `mcp.ClientOptions` 的工具自动执行配置；工具调度 grep 只在 `core.ToolExecutor` |

**Phase 1 后复评（post-design）**：设计产物（[research.md](./research.md) / [data-model.md](./data-model.md) / [quickstart.md](./quickstart.md)）完成后重跑本表——结论同前，无新增违规、无 Complexity Tracking 条目（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/007-mcp/
├── spec.md              # /speckit-specify 输出（含 Clarifications 会话 2026-10-17：三条待决落案 + 两条收敛项）
├── plan.md              # 本文件（/speckit-plan 输出）
├── research.md          # Phase 0 输出（D1~D12 决策与写前核实清单）
├── data-model.md        # Phase 1 输出（实体、字段、校验矩阵与状态流转）
├── quickstart.md        # Phase 1 输出（可复制执行的验证步骤）
├── run-state.md         # 过程状态（module-dev 步骤 0 建；入库、收口不删）
├── checklists/
│   └── requirements.md  # 需求质量清单（/speckit-specify 输出；16/16）
└── tasks.md             # /speckit-tasks 输出（下一步生成）
```

**`contracts/` 本 feature 不新建**（与 US-1~US-6 同口径）：本 US 的固定字面量**已有唯一事实源**——`mcp_servers.yaml` 形态与 `AGENT.md` 的 `mcp_servers` 字段 / 规则 4 / 6 文案在 [docs/contracts/agent-md.md](../../docs/contracts/agent-md.md)；失败分类七值与审计形状在 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md)；导出签名逐字来源在颗粒度文档 §3.1、固定口径 25 条在 §6.2。再抄一份到 `specs/` 就是**第二份字面量**——抄一份必漂移一处。plan 的契约节只**引用**，不复制。

### Source Code (repository root)

```text
internal/
├── core/
│   └── mcp.go                 # 新增：MCPServerConfig 纯配置类型（Name / Command / Env map[string]string）——
│                              #       两阶段同类型：workspace 产出时 Env 是 ${VAR} 占位，resolveMCPEnv 之后是最终值；
│                              #       零方法、零 import 内部包（core 只依赖标准库）
├── workspace/
│   ├── mcp.go                 # 新增：ParseMCPServers(root) + 非导出 parseMCPServers(base *os.Root) /
│   │                          #       mcpServerNames / isEnvPlaceholder——顶层映射 + servers 列表、八类形态校验、
│   │                          #       重名拒载、精确 ${VAR}、transport 取值域、顺序保留、零 server 语义；
│   │                          #       **不读环境变量**；缺失 / 空 / {} / null / 无 servers → 零 server
│   ├── agent.go               # 改造：AgentLoaderOptions 增 MCPServers / MCPConnected（构造期 slices.Clone）；
│   │                          #       LoadAll 在 MCPServers == nil 时调 parseMCPServers(base)；
│   │                          #       删除 readMCPServerNames；loadOne 增 mcpConnected 参数，规则 4 / 6 按 §2.3 改写
│   │                          #       （规则 4 用注册后名字集；规则 6 先判已配置再判已连接；轻路径 unverified 分支）
│   └── templates.go           # 改造：templateMCPServersYAML **只改注释**（servers 列表形态与字段）；{} 字节不动
├── tool/
│   ├── mcp.go                 # 新增：MCPPool（单 mcp.Client、多会话）——NewMCPPool(opts) / Connect(ctx, specs) /
│   │                          #       Close()（幂等）；串行连接、每 server 30s 握手预算（只罩 initialize + tools/list）、
│   │                          #       分页到空 + 重复游标守卫、失败 WARN（server / reason / detail=safeErrorSummary）
│   │                          #       + 关闭半截会话 + 继续；dialCommand（exec.Command + CommandTransport，Dir = Root，
│   │                          #       Env = mergeChildEnv；不得 CommandContext）；非导出 dial 注入点（白盒测试缝）
│   ├── mcptools.go            # 新增：mcpToolAdapter 实现 core.Tool 四方法（Name / Description / InputSchema / Execute）——
│   │                          #       名称 / 描述 / schema 原样；mcpCallArguments（空 / null → {}，其余原始字节）；
│   │                          #       renderCallToolResult（文本换行连接 / 非文本类型化占位 / 结构化兜底 / 空输出文案）；
│   │                          #       IsError → ExternalError（server 文本丢弃）；协议错误 → ExternalError（原文只进 Cause）；
│   │                          #       先 ctxPreferred 判序；不落审计、不写消息、不记日志
│   └── runner.go              # 改造：抽出 mergeChildEnv(passthrough, declared) []string（等价重构）；
│                              #       LocalRunner.childEnv 改为调用它（行为逐字节不变）
├── app/
│   ├── mcp.go                 # 新增：mcpConnector 接口（Connect / Close——App 的测试夹层）、
│   │                          #       resolveMCPEnv(specs, lookup)（未设置 / 空值 → 拒绝启动，点名 server + env.key + 变量）、
│   │                          #       mcpToolNames(conns)
│   └── wire.go                # 改造：① App 增 mcpConnect mcpConnector；② 等值门禁之后插入「ParseMCPServers →
│                              #       resolveMCPEnv → NewMCPPool → Connect → 注册 MCP 工具（keep-first）」；③ 装载器注入
│                              #       MCPServers / MCPConnected；④ 摘要日志增 mcp_connected；⑤ 池创建后的失败路径先
│                              #       pool.Close() 再 abortRuntime；⑥ heavyRuntime 增 mcp 字段，Close 改为池 → 库 → 锁
├── cli/
│   └── profile.go             # 改造：renderProfileLine 增 unverified 分支（逐字「registered=unverified——MCP 工具与
│                              #       连接需重路径确认」）；renderProfileList 在未验证数 > 0 时追加「/ 未验证 %d」；
│                              #       零计数时汇总行逐字节不变；registeredCount 语义不变
└── testutil/
    └── mcpserver.go           # 新增（非 _test.go，仅供测试 import）：MCPTestTool / NewInMemoryMCPPair（可设 PageSize）/
                               #       StandardMCPTools（echo / boom / blob）/ RunMCPHelper（BASALT_MCP_HELPER 未设 → t.Skip）

go.mod / go.sum                      # +1 直接坐标 github.com/modelcontextprotocol/go-sdk v1.8.0（传递依赖随 tidy）
basalt.yaml.example / Makefile / CI  # 零改动（30s 预算是代码常量；门禁七目标与覆盖率判卷已覆盖本 US 的包）
internal/storage/schema.sql          # 零 diff（零迁移、零新表 / 列 / 索引）
docs/contracts/**                    # 零改动（固定字面量引用不复制）
docs/Compatibility.md                # 增补 §3 三行有意差异（非 stdio 拒启动 / 非文本类型化占位 / 子进程环境白名单）
```

**Structure Decision**: 单 Go module + `internal/` 分包；本 US **新增实现文件 6 个、改造 6 个、新增测试文件 6 个 + 追加 2 个 + 夹具改造 2 点**，**不新增内部包、不新增顶层目录、不新增命令**。与台账「模块落位（摘要）」列一致：`internal/tool`（MCP 客户端池 + 工具适配器）、`internal/workspace`（`mcp_servers.yaml` 解析与形态校验）、`internal/app`（装配接线：env 解析、连接、注册、装载器注入、停机）、`internal/core`（`MCPServerConfig` 纯类型）、`internal/testutil`（测试基建）、`internal/cli`（轻路径第三态展示面）；`go.mod` 新增直接坐标 `github.com/modelcontextprotocol/go-sdk v1.8.0`（栈表已批准）。前序改造点 9 项（§3.5 表）中，①③④ 触及**既有测试的判据面**（`writeMCPServers` 夹具改列表形态；`TestAgentLoader_ParsesAllFrontmatterFields` 从「配置存在即注册」升级为「已配置且已连接」；`profile_test.go` 零配置断言不变、新增 unverified 分支），⑤ 是等价重构（零判据变化），⑧ 属随已批准接口适配，⑨ 是上游文档同步——**逐条在步骤 5 停点报告**。后续占位 6 项（SSE / HTTP transport、resources / prompts / sampling、重连 / 健康检查、容器隔离与工具级 Policy、管理台 MCP 页、命名空间与 server 级开关）只留位与注释，**不实现**。

**分层落位（三条包纪律的逐条落地）**：

| 纪律 | 本 US 的落法 |
| --- | --- |
| 依赖方向无环 | `core` 新文件只 import 标准库（判据 `go list`）；`workspace` import `core` + 标准库 + 既有 yaml（**不** import `tool`——解析只产出纯类型，名字集以 `[]string` 给装载器）；`tool` import `core` + 标准库 + SDK（**不** import `app` / `cli` / `workspace`）；`app` 装配全部；`cli` 不 import `tool` / SDK；`testutil` 仅测试 import |
| 接口定义在消费方 | `core.Tool` 由适配器实现（编译期 `var _ core.Tool = (*mcpToolAdapter)(nil)`）；`tool.toolLister`（`ListTools` 单方法）由 `tool` 本包声明，`*mcp.ClientSession` 隐式满足；`app.mcpConnector`（`Connect` / `Close`）由 `app` 声明，生产实现是 `*tool.MCPPool`、测试实现是 `fakeMCPConnector`——编译器钉住方向 |
| 第三方实现放消费方包内 | MCP SDK 封装在 `internal/tool`（与 SQLite 封装在 `storage` 同构）；`internal/testutil` 的进程内传输与工具夹具是测试侧用法（见「文档 ↔ 仓库事实」C）；`core` 不认识 SDK、`workspace` 不认识 SDK |

**标签口径（`[US1]`~`[US4]` 不混淆）**：`tasks.md` 中的 `[US1]` / `[US2]` / `[US3]` / `[US4]` 是 Spec-Kit 对**本 feature 内部**用户故事的编号（对应 [spec.md](./spec.md) 的四个故事：零代码接入走通 / 故障隔离与准入一致 / 凭证与子进程边界 / 同形可审计与生命周期），**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）里的 US-1 ~ US-7——本 feature 的全部故事都落在项目 **US-7（MCP 集成）** 一个模块内。故事 ↔ 测试文件面的分组是本 plan 自己的记账口径，**不改动**颗粒度文档任何内容。

## Complexity Tracking

> 仅当 Constitution Check 存在需辩护的违规时填写。

无违规——本 feature 无 Constitution Check 不通过项，故无复杂度辩护条目。

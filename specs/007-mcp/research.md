# Research: US-7 MCP 集成 — Phase 0 决策

**Feature**: `007-mcp` | **Date**: 2026-10-17 | **Plan**: [plan.md](./plan.md)

## 0. 结论先行

- **NEEDS CLARIFICATION：零**——[spec.md](./spec.md) 的 Clarifications（2026-10-17）已把颗粒度文档的三条待决全部落案为**方案 A**（transport 非 stdio 拒启动 / 轻路径第三态 `unverified` / 子进程环境复用 `shell.env_passthrough`），两条就地收敛（零工具 server 计入 `mcp_connected`；规则 6 先判已配置再判已连接、声明序报首个失败、规则 4 先于规则 6）。设计面冻结，本文件不重新裁决任何口径。
- 本文件记的是**实现路径选择**（Decision / Rationale / Alternatives）与**写前核实读数**；每条决策都能指回颗粒度文档 [docs/us/us7-mcp.md](../../docs/us/us7-mcp.md) 或技术方案的节号。
- 下方 SDK 事实全部来自**本地模块缓存中的 v1.8.0 源码**（本轮当场读取，行号为该文件内实测）；不是凭记忆，也不依赖教程摘要。

## 1. 写前核实读数（2026-10-17，本轮当场命令）

| # | 检查项 | 命令 / 文件 | 读数 |
| --- | --- | --- | --- |
| 1 | Go 工具链 | `GOPROXY=https://goproxy.cn go version` | `go version go1.27.2 windows/amd64`（**与 `toolchain go1.27.2` 一致**） |
| 2 | SDK 版本线 | `GOPROXY=https://goproxy.cn go list -m -versions github.com/modelcontextprotocol/go-sdk` | 版本列表到 `v1.8.0`；`v1.8.0` 是**最新 GA**（`v1.8.0-pre.1` / `v1.8.0-pre.2` 是里程碑版，不用） |
| 3 | `CommandTransport.Connect` 是否使用 ctx | `mcp/cmd.go:29-47` | **不使用**：直接 `StdoutPipe` / `StdinPipe` / `t.Command.Start()`；`ctx` 参数在整个方法体内零引用 |
| 4 | 进程生命周期归属 | `mcp/cmd.go:49-56` | `pipeRWC` 持有 `cmd *exec.Cmd`；`Close` 走 stdin 关闭 → 等待 → 终止 → 强杀（`TerminateDuration`，缺省值在 `cmd.go:42-45`） |
| 5 | `NewInMemoryTransports` | `mcp/transport.go:183` | 存在，返回 `(*InMemoryTransport, *InMemoryTransport)` 一对——默认层测试用 |
| 6 | `NewClient` / `Client.Connect` | `mcp/client.go:51` / `:308` | `NewClient(impl *Implementation, options *ClientOptions) *Client`；`Connect(ctx, t Transport, opts *ClientSessionOptions)` |
| 7 | `ListTools` / `ListToolsParams` / `ListToolsResult` | `mcp/client.go:1263` / `mcp/protocol.go:1310` / `:1326` | `ListTools(ctx, *ListToolsParams{Cursor string})`；结果 `Tools []*Tool` + `NextCursor string` |
| 8 | `CallTool` / `CallToolParams` | `mcp/client.go:1284` / `mcp/protocol.go:219` | `CallTool(ctx, *CallToolParams{Name string; Arguments any})` |
| 9 | `CallToolResult` | `mcp/protocol.go:266` | `Content []Content` / `StructuredContent any` / `IsError bool` |
| 10 | `Tool` | `mcp/protocol.go:1895` | `Name string` / `Description string` / `InputSchema any`（**任意可 JSON 序列化的 schema**） |
| 11 | `Content` 是否为密封接口 | `mcp/content.go:22-25` | `Content` 含**未导出方法** `fromWire(*wireContent)` → **包外无法实现**；渲染只能对 `*TextContent` / `*ImageContent` / `*AudioContent` / `*ResourceLink` / `*EmbeddedResource` 做类型分支 |
| 12 | server 侧 API（测试基建用） | `mcp/server.go:211` / `:315` / `:1354`；`mcp/transport.go:128`；`mcp/tool.go:30` | `NewServer(impl, opts)`、`AddTool(*Tool, ToolHandler)`、`Run(ctx, Transport)`、`StdioTransport`、`ToolHandler func(context.Context, *CallToolRequest) (*CallToolResult, error)` |
| 13 | `Implementation` | `mcp/protocol.go:2211` | `Name` / `Title` / `Description` / `Version`——`MCPPoolOptions.Version` 直接落 `Version` 字段 |
| 14 | 仓库现状 | `grep modelcontextprotocol go.mod`；`grep 'MCPPool\|MCPServerConfig\|mcpToolAdapter\|ParseMCPServers' internal cmd` | 均**零命中**——本 US 交付面全新增 |
| 15 | 改造点行号 | 见 plan「文档 ↔ 仓库事实差异」§A | `readMCPServerNames` = `internal/workspace/agent.go:368`；等值门禁 = `internal/app/wire.go:248`；`childEnv` = `internal/tool/runner.go:266`；`heavyRuntime.Close` = `wire.go:426`（db → lock） |

> 本轮**未复测**：`make check` 七目标、四目标交叉编译、覆盖率读数。这些是步骤 6 / 步骤 7 的当场读数，不在此声称。

## 2. 实现决策

### D1 协议库与包边界

- **Decision**：`github.com/modelcontextprotocol/go-sdk v1.8.0` 作为**唯一新增直接坐标**；生产代码只在 `internal/tool` 使用 SDK；`internal/testutil` 的 SDK 用法属测试基建（进程内传输 + 工具夹具）。`core` / `workspace` / `app` / `cli` 不认识 SDK。
- **Rationale**：宪法架构约束技术栈表 U-5 已批准；SDK 只做协议编解码（宪法 I / II）；`workspace` 只产出 `[]core.MCPServerConfig`，`app` 只依赖自己声明的 `mcpConnector` 接口——依赖倒置让 SDK 不可能渗进装配层。`testutil` 的例外是**交付物本身的要求**（§3.1 明确规定 `MCPTestTool.Handler mcp.ToolHandler` / `NewInMemoryMCPPair` 返回 `mcp.Transport`），且该包只被测试 import（plan「文档 ↔ 仓库事实」C）。
- **Alternatives considered**：① 手写 JSON-RPC over stdio（拒绝：协议版本、分页、错误语义都要自维护，且没有社区一致性测试）；② 把 SDK 隔离到 `internal/adapters/mcp` 新包（拒绝：技术方案 §10.1 明确「MCP SDK 封装在 `tool`」，新建适配包属文档外概念）；③ 用 SSE / HTTP transport（明确不做，扩展阶段）。

### D2 配置文件形态

- **Decision**：`<workspace>/mcp_servers.yaml` = 顶层映射 + `servers:` **列表**；条目字段 `name` / `transport` / `command` / `env`；列表顺序原样保留（连接序 = 注册序）；未知键忽略；缺失 / 空文件 / `{}` / `null` / 无 `servers` → 零 server。**删除** `readMCPServerNames`（顶层键名集临时占位），同一文件只留 `parseMCPServers` 一份解析器。
- **Rationale**：Compatibility §2.1「文件内容零改动」是 NivroOS 迁移承诺；NivroOS 基线即「注释 + 顶层 `servers:` 列表」（颗粒度文档头部事实源 6）。`{}` 是 US-1 模板字节，两种形态下都表示零 server（注 1 / 注 7）。
- **Alternatives considered**：① 保留顶层键名集并兼容双形态（拒绝：两套语义、两份错误文案、`{}` 与 `servers:` 混用时判据不清）；② 每 server 一个文件（拒绝：一个文件 = 一个注册表，顺序与重名判定需要跨文件排序，超出 v1 范围）。

### D3 加载期 fail-closed 校验

- **Decision**：九类形态问题**拒绝启动**——顶层非映射 / `servers` 非列表 / 条目非映射 / 缺 `name`（空串）/ 缺 `command`（纯空白同档）/ server 名重复 / `env` 非映射 / `env` 值不是精确 `${VAR}` / `transport` 非空且非 `stdio`。错误点名文件、条目序号或 server 名与字段；**错误文案不回显配置值**（值可能就是凭证）。
- **Rationale**：需求 §5.13「格式非法明确报错、拒绝启动」；技术方案 §8.8；颗粒度文档约束 2 / 3。`transport` 空串按缺省 `stdio` 处理（`TestParseMCPServers_TransportDefaultsToStdio`）。
- **Alternatives considered**：① 坏条目 WARN 跳过（拒绝：配置错误被静默吃掉，运维以为 server 生效）；② 明文 env 值警告后使用（拒绝：凭证落日志 + 落子进程环境，直接违反凭证纪律）。

### D4 装配期 env 解析

- **Decision**：`workspace` **只校验形态、不读环境**；`app.resolveMCPEnv(specs, lookup)` 在装配时解析，`lookup` 可注入（生产 = `os.LookupEnv`）；未设置**或值为空** → 拒绝启动，点名 `server <name> 的 env.<key>` 与变量名；解析值不进错误、不进日志。
- **Rationale**：技术方案 §8.8「装配前统一解析，未解析残留即拒」；把环境读取留在装配层才能让轻路径（`profile list`）不依赖运行环境、让 R1b 用注入的 lookup 判卷。`MCPServerConfig` 两阶段同类型（先占位、后最终值）由颗粒度文档 §3.1 固定。
- **Alternatives considered**：① 解析放在 `workspace`（拒绝：轻路径会被环境缺失拖死，且 `workspace` 单测需要污染进程环境）；② 解析后把值写回配置文件（拒绝：凭证落盘）；③ 未设置时用空串继续（明确禁止，FR-005）。

### D5 串行连接与 30 秒握手预算；进程生命周期不绑预算

- **Decision**：逐 server **串行** dial → `client.Connect` → `listAllTools`；dial 用 `exec.Command`（**不是** `exec.CommandContext`）且不接收 ctx；30 秒预算是一个 `context.WithTimeout`（`MCPPoolOptions.ConnectTimeout == 0` → 30s），**只**传给 `client.Connect` 与 `listAllTools`，两步完成即 `cancel()`；`Connect` 只在整体 ctx 取消 / 截止时返回错误，其余失败 WARN + 关闭半截会话 + 继续。
- **Rationale**：核实读数 #3 —— `CommandTransport.Connect` 忽略 ctx、进程由 `exec.Cmd` 持有；若用 `CommandContext`，`cancel()` 会杀掉子进程，启动看似成功、第一次调用即失败（颗粒度文档注 11，R11 钉住）。串行保证「文件序 → 工具序」的注册顺序确定（FR-009）。
- **Alternatives considered**：① `exec.CommandContext(cctx, ...)`（拒绝：见上）；② 并发连接（拒绝：注册顺序不确定、日志交错、失败隔离的判据难写）；③ 把 `cctx` 存进会话供后续调用（拒绝：预算释放即失效）。

### D6 列工具分页

- **Decision**：`listAllTools` 以 `NextCursor` 循环到空串；同一游标第二次出现 → 返回错误（该 server 视为列工具失败、WARN + 跳过）。**不得**无限循环。
- **Rationale**：FR-011；核实读数 #7 的 `NextCursor` 形态；重复游标是唯一能区分「服务端有 bug」与「还有下一页」的确定性判据。
- **Alternatives considered**：① 只取第一页（拒绝：工具静默丢失，`AGENT.md` 声明会莫名 `tool not registered`）；② 最大页数上限（拒绝：上限内重复游标仍会空转，且引入新常量）。

### D7 适配器与入参

- **Decision**：`mcpToolAdapter` 实现 `core.Tool` 四方法；`Name` / `Description` / `InputSchema` **原样**；`mcpCallArguments`：空串 / `null` → `map[string]any{}`，其余先 `json.Unmarshal` 到 `map[string]json.RawMessage` 做形态校验，再返回**原始 `json.RawMessage`**；非法 → `*core.InvalidInputError{Tool, Reason:"MCP 工具入参必须是 JSON 对象"}`。
- **Rationale**：核实读数 #10 的 `InputSchema any` 允许原样透传；`Arguments any` 由 `encoding/json` 序列化，`json.RawMessage` 实现 `Marshaler` → 原始字节直通，>2^53 的整数不经过 `float64`（FR-012 / R13）。形态校验用 `map[string]json.RawMessage` 只校验顶层是对象，不触碰值。
- **Alternatives considered**：① `map[string]any` 往返（拒绝：大整数失真，且 `null` / 空串语义要另写分支）；② 传 `string(input)`（拒绝：SDK 会再套一层 JSON 字符串）；③ 不做形态校验（拒绝：非法入参应在适配器前就被拒，但执行器已保证合法 JSON 对象——适配器仍防白盒直调）。

### D8 结果渲染

- **Decision**：优先级固定——文本块（`*mcp.TextContent`）按序 `\n` 连接 → 无文本时 `StructuredContent` 紧凑 JSON → 非文本块类型化占位 `[非文本内容：<type> <mime 或 uri>]`（type ∈ image / audio / resource_link / resource / 其他）→ 全空 `(MCP 工具无输出)`。**不输出 base64**。
- **Rationale**：FR-013 / 口径 11；核实读数 #11 —— `Content` 接口有未导出方法，包外只能用**类型分支**，这正好与"类型化占位"的设计同构；base64 进模型上下文既烧预算又会在会话历史留二进制副本（注 3）。
- **Alternatives considered**：① 所有块 JSON 序列化（拒绝：图片块会把 base64 原样带进模型上下文）；② 非文本块直接丢弃（拒绝：模型看不到「有内容但不是文本」，容易误判工具无输出）。

### D9 错误边界

- **Decision**：判序 = `ctxPreferred(ctx, err)` **先判**（命中 → 返回 ctx 原错误，归 `timeout`）→ `res.IsError` → `*core.ExternalError{Target: "mcp:<server>/<tool>", Reason: "MCP 工具返回错误：<server>/<tool>"}`，**server 文本整段丢弃（连 `Cause` 也不放）** → 传输 / 协议错误 → `*core.ExternalError{Target, Reason: "MCP 调用失败：<server>/<tool>", Cause: err}`。原始错误只经 `Unwrap` 可见，不进任何可观测面。
- **Rationale**：FR-014 / 015 / 016 / 028；`ExternalError.Error()` 只返回 Reason（US-5 既有），所以执行器写审计 / 日志 / 模型上下文时天然只见固定文案；R5 / R6 / R12 分别钉住三条分支。注 4：MCP 适配器不会产出 `fs.ErrNotExist`，`not_found` 对 MCP 工具不可达——这是判序的自然结果，测试不得断言 `not_found`。
- **Alternatives considered**：① 把 server 文本截断 / 单行化后放进 Reason（明确禁止：截断不是脱敏，令牌仍可能完整保留）；② 把 server 文本放进 `Cause`（拒绝：颗粒度文档明确"整段丢弃"，且未来任何打印 Cause 的代码都会泄漏）；③ 先判 `IsError` 再判 ctx（拒绝：R6 要求取消身份优先，与 HTTP 工具同一判序函数）。

### D10 注册与等值门禁

- **Decision**：注册顺序 = 内置 8 件 → MCP 工具（server 文件序 → 工具列表序）；重名 **keep-first** + WARN「拒绝重复注册的工具」；等值门禁 `registry.Names() == declaredToolNames` **必须**在 MCP 注册**之前**执行（`wire.go:248`，门禁表达式与比较对象零改动）。
- **Rationale**：FR-009 / 010；门禁的语义是「内置注册池 == 声明的内置工具名集」，MCP 工具注册后池变大，门禁必然失败——顺序是硬约束（约束 9 / R3）。keep-first 保证模型与既有 `AGENT.md` 对内置名的语义不漂移。
- **Alternatives considered**：① MCP 重名覆盖内置（拒绝：模型看到的 `read_file` 语义被外部进程替换）；② 门禁改为集合包含（拒绝：改变既有判据面，且放过"内置工具没注册全"的缺陷）；③ 在注册阶段做命名空间前缀（明确不做，注 9）。

### D11 审计

- **Decision**：**零改动**——MCP 调用经既有 `core.ToolExecutor` 两阶段审计（执行前 `pending`、执行后更新同一行、失败 WARN）；适配器自身不落审计、不写消息、不记日志。
- **Rationale**：宪法 V；FR-017；技术方案 §6.5「不经 Policy ≠ 不审计」。R9 用阻塞的 server handler + 记录型审计假件观察 pending 行；生产路径即 SQLite 仓储，不需要任何新 SQL。
- **Alternatives considered**：① 适配器内写审计（拒绝：第二套追加路径，违反宪法 II / V）；② MCP 调用绕过执行器直接调（拒绝：调度唯一入口被破坏）。

### D12 子进程环境、cwd 与命令切分

- **Decision**：从 `LocalRunner.childEnv`（`runner.go:266`）抽出 `mergeChildEnv(passthrough, declared)`——透传清单优先、声明覆盖同名键、其余清空、结果**永不为 nil**；MCP dial 与 shell 工具共用同一函数。`cmd.Dir = Root`（工作区根）。`command` 只按 `strings.Fields` 切分，首 token 为可执行文件；无引号语义、不包 `cmd /c`、不引入 shell。
- **Rationale**：FR-018 / 019 / 020；待决 #3 已裁决复用 `shell.env_passthrough`；`childEnv` 现有语义逐字保留（等价重构，零判据变化）。MCP server 是实例级资源，cwd 必须可预测（注 6 / 已决 #9）。
- **Alternatives considered**：① 继承完整父进程环境（拒绝：把 `BASALT_*`、云凭证、代理设置一并送出）；② `cwd` = 进程当前目录（拒绝：从不同目录启动指向不同文件）；③ 支持 shell 引号 / `sh -c`（拒绝：配置字面量变成可注入命令行）。

### D13 停机顺序与池关闭

- **Decision**：`heavyRuntime.Close` 顺序 **MCP 池 → 数据库 → 实例锁**；`MCPPool.Close` 幂等（第二次 sessions 为空直接返回）；逐步 `errors.Join`，不静默。`heavyRuntime` 增 `mcp mcpConnector` 字段；池创建后的所有失败路径先 `pool.Close()` 再 `abortRuntime`。
- **Rationale**：FR-021；Operations §5.3 step 4~6；若先关库，在途 MCP 调用收尾时可能写审计失败。握手预算释放不杀进程，进程在此步才收（读数 #4 的 SDK Close 路径）。
- **Alternatives considered**：① 先关库（拒绝：见上）；② 用 `context` 超时强关池（拒绝：v1 无停机超时配置键，SDK 自己已有终止梯度）。

### D14 轻路径第三态

- **Decision**：`core.AgentEntry` 增 `Unverified bool`；`AgentLoaderOptions` 增 `MCPServers` / `MCPConnected` 两字段（构造期 `slices.Clone`）。重路径注入「已配置 + 已连接」；轻路径两字段为 nil，装载器自读一次 `parseMCPServers` 取已配置名字集（`mcpNames`），对**可能依赖 MCP** 的 Agent 置 `Unverified`（`Registered == false`、`RejectReason == ""`），确定性规则仍先拒。展示面只在 `cli/profile.go`：`registered=unverified——MCP 工具与连接需重路径确认`；汇总在未验证数 > 0 时追加 `/ 未验证 %d`，零计数逐字节不变。
- **Rationale**：FR-023；待决 #2 裁决 A；注 5。运行面（`newAgentSource`）仍只收 `Registered`，轻路径不改变任何运行行为；零 MCP 配置时与 US-6 逐字节相同。
- **Alternatives considered**：① 轻路径也连 MCP（拒绝：把毫秒级只读命令变成网络 / 进程操作）；② 保持两态并接受误报（待决 #2 的方案 B，已被否决）。

## 3. 留给实现期的写前核实清单

以下三点在写代码前**当场核实**，结果不进本文件、进任务 DoD：

| # | 核实点 | 方法 | 失败时的处置 |
| --- | --- | --- | --- |
| 1 | `go mod tidy` 后直接依赖区只有 `go-sdk` 一条新增 | `git diff go.mod` | 出现表外坐标 → 软门禁停下 |
| 2 | 集成层子进程自举在当前 go test 版本下可用（`-test.run=^TestMCPHelperProcess$` + env） | 先写 R11 用例并实跑 | 不可用 → 停下来讨论，不改判据 |
| 3 | `go test -overlay` 逆改探针可产出逐字失败文案（R2 / R5 / R9 各一次） | 探针副本放仓库外，用毕即删 | 探针不可用 → 用等价逆改（临时改源码后还原），结论进 run-state |

## 4. 被否决方案的保留记录（证据纪律：失败路径不删）

| # | 被否决方案 | 否决依据 | 若将来重开的条件 |
| --- | --- | --- | --- |
| 1 | 双形态兼容（顶层键名集 + `servers:` 列表） | 注 1：迁移承诺是文件内容零改动，不是"两种都认"；双解析器 = 双错误文案 | 若不接受 NivroOS 配置迁移，先改 Compatibility §2.1 |
| 2 | `exec.CommandContext` 起子进程 | 注 11 / R11：预算释放杀进程 | SDK 未来改变进程生命周期模型时重评（R11 会先红） |
| 3 | 并发连接多 server | 注册顺序不确定、失败隔离判据复杂 | 出现"启动时间被串行连接拖垮"的实测指标时 |
| 4 | 非文本内容输出 base64 | 注 3：烧预算、留二进制副本 | 引入附件落盘 / 引用机制（扩展阶段） |
| 5 | 断线重连 / 健康检查 | §5：失败即跳过、重启才重连 | 扩展阶段 |

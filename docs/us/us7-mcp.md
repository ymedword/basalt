# US-7 MCP — 模块执行颗粒度文档

> **文档状态**：**R4**（2026-10-17；R1 由 `/module-doc-gen 7` 产出，R2 按评审回合 6 项发现修正后于步骤 5 停点批准并实施；R3 为收口复核修正——非字符串 `transport` 拒载、握手期取消 / 截止为池级错误、SC-001 描述与 schema 保真断言；R4 为文档口径修订——统一「`shell.env_passthrough` 代码无内置默认白名单」表述，实现与配置键零改动；逐条见文末「收口补登」）。已登记于 `docs/us/REGISTRY.md`（US-7）。设计面**待决事项 3 条已于 2026-10-17 裁决为方案 A**：① 非 stdio `transport` 拒绝启动；② 轻路径第三态 `unverified`；③ MCP 子进程环境复用 `shell.env_passthrough`。正文按裁决书写。
>
> **事实主源顺位（供 `/us-register 7` 派生登记）**
>
> 1. **`docs/TechnicalSolution.md`**（文档之间最权威）——§1.1 原则 1（MCP SDK **只做协议编解码**，工具调度唯一归属 `ToolExecutor`，SDK **不得**接管循环或自动执行工具）/ §1.2 技术栈表（`github.com/modelcontextprotocol/go-sdk v1.8.0`，**已定 U-5**，2026-10-15）/ §6.3 三档扩展（**零代码档（主推）= `AGENT.md` + 现成 MCP server**：配置 `mcp_servers.yaml` + 声明 `mcp_servers:`）/ §6.4 ToolRegistry（`tools map[string]Tool` = **内置 + MCP 适配 + 代码内扩展**；加载期解析不到 → 该 Agent **整只不注册**；运行期工具池 = 声明子集，**不声明 = 空池**；**不得**反射扫描隐式注册）/ §6.5 MCP 行为边界（MCP server 是**独立进程**，其文件读写与网络访问**不经过** `Policy`；信任边界 = 管理员在 `mcp_servers.yaml` 里显式登记的那一行，`command` 来自配置、**非模型可控**；v1 = 经管理员评审，容器隔离属扩展阶段；登记时需写明该 server 会碰什么）/ §6.5 子进程环境变量白名单（只透传 `shell.env_passthrough` + 工具显式声明，其余清空）/ §8.8 配置占位解析（所有 `${VAR}` 在装配前统一解析，未解析残留 → **点名报错、拒绝启动**；校验失败 exit code 非零）/ §10.1 包结构与依赖方向（`tool/` = 内置工具 + **MCP Client** + Registry；MCP SDK 封装在 `tool`；`core` 只依赖标准库；现有十包是起点不是冻结清单）/ §11.5 配置生效时机（`mcp_servers.yaml` 变更**不热加载**）/ §13 实施节奏（US-7 行：任务 = MCP；涉及包 = `tool` / `app` / `workspace`；可演示成果 = **Demo 三**）
> 2. **`docs/DemandAnalysis.md`**——§5.13 配置与密钥（`mcp_servers.yaml`：**name / transport / command / env**；env 值同样只允许 `${VAR}` 占位；加载期校验失败**明确报错、拒绝启动**，不静默取默认）/ §9.1 启动加载（`注册内置工具 → 连 MCP server（失败跳过 + WARN）→ 扫描 agents/`；启动摘要日志含**已注册 Agent 数 / 工具数 / MCP 连接数**）/ §13.1 功能验收（**Demo 三**：Agent 目录 + 现成 MCP server，全程**不改代码**）/ §12.1 风险表（MCP SDK 行为差异与版本演进 → **核心链路有真实子进程协议测试（自研最小 server）钉住**；SDK 升级先跑该测试）/ §12.2 U-5（SDK 选型已定）/ §5.14 管理台（MCP 观测页归 US-12）
> 3. **`.specify/memory/constitution.md`**（原则级最高）——I（自实现 ReAct 循环；`go.mod` **不得**出现 Agent 框架坐标；协议 SDK **不得**接管循环或自动执行工具）/ II（LLM 客户端只做协议调用；工具调度唯一归属 `ToolExecutor`）/ IV（一个目录 = 一个 Agent；声明即绑定；**声明名无实体 → 整只不注册**）/ V（审计 day-one 且 **fail-closed**——MCP 工具调用同样经 `ToolExecutor` 两阶段写，适配器**不得**绕开）/ VI（授权与执行分离两接口——MCP server 的**内部**行为不经 `Policy`，边界见技术方案 §6.5，**不是**对宪法 VI 的豁免，而是"授权对象是 Basalt 自己的动作"的边界声明）/ VII（同步执行；`context` 只承载取消 / 截止 / 追踪）/ VIII（单二进制纪律；依赖最小化——本 US **恰好一个**表内坐标 `go-sdk v1.8.0`）/ IX（不用 ORM；**零迁移**）/ X（范围卡紧）
> 4. **`docs/contracts/`**（**固定字面量**）——`agent-md.md` 字段表（`mcp_servers` `[]string`，否，缺省 `[]`；元素 = `mcp_servers.yaml` 里的 server 名）/ §2.1 规则 4（`tools` 里每个名字都能解析——内置十件 / MCP 适配 / 代码内注册；缺一 → WARN `tool not registered: <name>`）/ §2.1 规则 6（`mcp_servers` 里每个名字在 `mcp_servers.yaml` 里**且连接成功**；缺一 → WARN `mcp server skipped`）；`run-and-errors.md` §7.2 两阶段写 / §7.3 `error_kind` 七值（MCP 工具失败归 `external_error`；取消 / 截止归 `timeout`）
> 5. **既有实现事实源（US-1~US-6 已交付面；复用不重造）**——`internal/workspace/agent.go`：`mcpServersFile = "mcp_servers.yaml"`、`AgentLoaderOptions{Providers, Tools, Logger}`（切片构造期 `slices.Clone`）、`LoadAll` 打开 `os.OpenRoot` 后调用 `readMCPServerNames(base)`（注入进 `loadOne`，注释逐字：「规则 6（存在性半条；「连接成功」半条随 US-7）」）、`reject{reason, detail}` + `mergedReason()`、规则 4 报**声明序**首个缺名、规则 6 报 `mcp server skipped: <name>`；`internal/core/profile.go:76` `AgentEntry{Name, Registered bool; Profile *Profile; RejectReason string}`（注释：`Registered ⇔ all checks passed`）；`internal/app/wire.go`：装配 ①~⑦（⑤ 八件内置工具 → **等值门禁** `registry.Names() == declaredToolNames` → ⑥ 装载器 → ⑦ 运行入口）、`App` 的 `providerOverride` 测试夹层先例、`abortRuntime(err, db, lock)` 11 个调用点、`heavyRuntime.Close`（先库后锁）、`newAgentSource` 只收 `Registered` 条目；`internal/app/ports.go:100-110`：轻路径装载器 `Tools: declaredToolNames`；`internal/cli/profile.go:296-331`：`汇总：%d 个 Agent 目录（已注册 %d）` 与 `%s：registered=false——%s` / `%s：registered=true（provider %s / 模型 %s）`（两者行首各有两个空格；注释：措辞 = 固定口径，改它即改判据）；`internal/tool/registry.go`：`Register` 重名拒载**保留原注册** + WARN `拒绝重复注册的工具`、`Resolve` 只查表、`Names` 排序、`Specs` 保持传入序；`internal/tool/runner.go:260-292` `childEnv`（声明 `KEY=VALUE` 映射 + 透传清单，结果**永不为 nil**——空切片 = 一个都不继承）；`internal/tool/httptools.go:538-553` `ctxPreferred(ctx, err)`（`ctx.Err()` 优先，否则裸 `context.Canceled` / `context.DeadlineExceeded`，否则 nil）；`internal/core/tool_exec.go`：`Execute(ctx, env, toolSet, call)` **不返回 error**、审计预写 fail-closed、单工具超时在此建立、失败 → `classifyToolError` + WARN + 回填、`truncateToolOutput`；`internal/workspace/templates.go:89-92` `templateMCPServersYAML`（注释写的是"顶层键名集"，字节恒 `{}`）；`internal/workspace/init_test.go:56-69`（模板可解析为**空映射**）；`internal/workspace/agent_test.go:147-157` `writeMCPServers`（顶层 map 形态夹具）、`:528-542` `TestAgentLoader_RejectsUnconfiguredMCPServer`、`:637-680` `TestAgentLoader_ParsesAllFrontmatterFields`（配置 `search` 后断言 `Registered == true`）；`internal/testutil/`（`httpserver.go` / `provider.go` / `workspace.go` + 自证用例 `testutil_test.go`）
> 6. **附加事实源**——`docs/Compatibility.md` §2.1（工作区内部布局**保持一致**：`agents/ skills/ memory/ mcp_servers.yaml logs/ ...`；工作区根改名后**文件内容零改动**）/ §2.1 AGENT.md 字段集**逐字相同**（含 `mcp_servers`）/ §2.4 运行行为（热加载范围**保持一致**：agents 与 skills 目录感知，`mcp_servers.yaml` 变更**需重启**）；`docs/Operations.md` §1.2（MCP server 连不上 → 工具不注册 → 声明该工具的 Agent 整只不注册，WARN 点名）/ §5.3 停机顺序（step 4 **关 MCP**：关闭客户端与子进程；step 5 关库；step 6 退出）；`docs/AiProgrammingGuide.md` §4.7（US-7 完整定义：核心目标、task 大类、颗粒度约 10~12 个文件、**硬依赖 US-4**、验收 Demo 三）/ §7 陷阱表（与本 US 相关：#55 未验证清单）；`docs/EngineeringStandards.md` §1 Go / §2 测试（判据面单一权威枚举）/ §4 日志与错误；**NivroOS 基线（只读参考，不回写）**：`.nivroos/mcp_servers.yaml` 形态 = 注释 + 顶层 `servers:` **列表**，元素 `{name, transport: stdio, command: "<完整命令行>", env: {KEY: ${VAR}}}`；`McpClientService` 每 server 串行连接、请求超时 30s、失败 WARN 跳过、ListTools 分页到空游标
> 7. **落地参考（非权威）**——`docs/Roadmap.md` §4（US 开放序列）
>
> **固定裁决句**：本文与最新技术方案冲突时，**以技术方案为准**；本仓配套工件（spec / plan / 已批准 US 清单）已锁定的引用在不与前者冲突时沿用，不另行改判。**契约字面量一律服从契约**——`AGENT.md` 的 `mcp_servers` 字段类型、规则 4 / 6 的 WARN 文案、`error_kind` 七值，本文明文冻结的部分只在契约留白处生效。**spec 锚点现状（核对于 2026-10-17）**：`specs/` 下现有 `001-foundation/` ~ `006-memory/`（US-1~US-6 已完成验收的历史设计，**只读**）；US-7 的 spec 目录由 `/module-dev 7` 新建。
>
> **登记派生信息**：技术方案章节 = §1.1 / §1.2 / §6.3 / §6.4 / §6.5 / §8.8 / §10.1 / §11.5 / §13；需求文档章节 = §5.13 / §9.1 / §13.1 / §12.1；模块落位（摘要）= `internal/tool`（MCP 客户端池 + 工具适配器——技术方案 §10.1「`tool/` = 内置工具 + MCP Client + Registry」/ §13 US-7「涉及包」列）/ `internal/workspace`（`mcp_servers.yaml` 解析与形态校验——§13 同列）/ `internal/app`（装配接线：env 解析、连接、注册、装载器注入、停机——§13 同列）+ `internal/core`（`MCPServerConfig` 纯类型，§3.1）+ `internal/testutil`（测试基建，§3.2）+ `internal/cli`（轻路径第三态的展示面，§3.5，随待决 #2）；`go.mod` **新增一个直接坐标** `github.com/modelcontextprotocol/go-sdk v1.8.0`（技术方案 §1.2 栈表已批准）。
>
> **台账状态**：**未登记**（`docs/us/REGISTRY.md` 尚无 US-7 行；登记由 `/us-register 7` 完成，本段「登记派生信息」即该行的派生源）。

## 1. 模块概述

**定位**：让 Basalt 用**零代码**方式接入外部能力——读 `mcp_servers.yaml` 起 **MCP client（stdio）**，连上官方 `go-sdk` 的 server 子进程，把 server 的工具**适配成 `core.Tool` 注册进全局注册池**；ReAct 循环**不感知工具来源**。单个 server 连不上只 WARN 跳过，**不中断启动**；依赖它的 Agent 因"工具解析不到"**整只不注册**（沿用 US-4 已交付的加载期门禁）。

**价值（WHAT / WHY）**：US-7 交付四件可判卷的事：① **零代码扩展路径走通**——编程指南 §4.7 的验收 Demo（**Demo 三**）：一个 Agent 目录 + 一个现成 MCP server，全程不改代码就能用上新工具；② **故障隔离**——`mcp_servers.yaml` 里某个 server 的 `command` 写错，启动**照常完成**、WARN 点名、其余 server 与其余 Agent 不受影响；③ **来源透明**——MCP 工具与内置工具在注册池、`tools:` 声明、运行时工具池、审计、失败分类上**同形**，模型与循环都看不到"这是 MCP 工具"；④ **信任边界写在明面上**——MCP server 是独立进程，其内部文件 / 网络行为**不经** `Policy`（技术方案 §6.5）；能被治理的是"管理员登记了哪一行配置"。规模参考：编程指南 §4.7 约 10~12 个文件；本 US 实际交付面见 §3（代码 新增 6 文件 + 改动 6 文件；测试 新增 6 文件 + 改动 2 文件）。**硬依赖 US-4（已交付）**：`Tool` 接口 / `ToolExecutor` / 注册池 / 加载期八条校验 / 失败分类；不消费其他未交付能力（管理台 MCP 页归 US-12、技能加载归 US-11、`notify` / `manage_schedule` 归 US-8 / US-10）。

## 2. 设计要点与约束

### 2.1 职责边界表

| 维度 | 本 US 职责（做） | 非本 US 职责（不做，归属） |
| --- | --- | --- |
| `mcp_servers.yaml` 解析 | `internal/workspace/mcp.go`：读文件（经 `os.Root`）、形态校验（顶层 / `servers` / 条目 / 必填 / 重名 / `env` 形态 / `transport`）、产出 `[]core.MCPServerConfig`；缺失 / 空 / `{}` / 无 `servers` → **零 server**；文件顺序**原样保留** | `basalt init` 的模板创建（US-1 已交付，本 US **只改注释**、字节不动）；工作区根改名（Compatibility 已定） |
| MCP 协议客户端 | `internal/tool/mcp.go`：`MCPPool`——串行连接、初始化、列工具（分页）、保留会话、关闭；`CommandTransport` 起子进程（stdio）；单 server 失败 WARN + 跳过 | SDK 自身的协议实现；SSE / Streamable HTTP（扩展阶段）；重连 / 健康检查（扩展阶段） |
| 工具适配 | `internal/tool/mcptools.go`：`mcpToolAdapter` 实现 `core.Tool`（名称原样、schema 透传、`CallTool` 结果与错误映射） | 工具调度 / 归属校验 / 审计两阶段（`ToolExecutor`，US-4 既有，**零改动**）；工具清单命令（US-5 已交付，注册池变大后自动多行） |
| 装配与失败隔离 | `internal/app/wire.go` + `internal/app/mcp.go`：`${VAR}` 解析、建池 / 连接、把 MCP 工具注册进池、把"已配置 / 已连接"名字注入装载器、摘要日志 `mcp_connected`、停机顺序接 MCP | provider 映射表（US-2 既有）；记忆门面（US-6 既有）；实例锁 / 库 / 恢复（US-1 / US-4 既有） |
| Agent 准入门禁 | `internal/workspace/agent.go`：规则 4 用**注册后**名字集；规则 6 补「连接成功」半条；轻路径第三态（待决 #2） | 其余七条规则（US-3 已交付，零改动）；`AGENT.md` 字段集（契约，零改动） |
| 可观测性 | 连接期 INFO / WARN（`server` / `reason` / `detail` 固定键）、启动摘要 `mcp_connected`、工具失败沿用执行器 WARN | 审计链路本体（US-4 已交付）；管理台 MCP 观测页（US-12） |
| 文档同步 | `docs/Compatibility.md` 增补与 NivroOS 的**有意差异**行（非 stdio 处置、非文本渲染、子进程环境）——见 §3.5 改造点 ⑨ | 技术方案 / 需求文档（本 US **不改**；有冲突走 §2.4 裁决注） |

### 2.2 关键约束清单

1. **`mcp_servers.yaml` 形态 = 顶层映射 + `servers:` 列表**（`Compatibility.md` §2.1「工作区内部布局保持一致 / 文件内容零改动」+ NivroOS 基线）：元素字段 `name` / `transport` / `command` / `env`；**顺序原样保留**（= 连接序与工具注册序）；未知字段**忽略**（前向兼容）。**不得**把本 US 的解析器改成"顶层键名集"形态——US-1~US-6 的 `readMCPServerNames` 是**临时占位**（注释逐字「「连接成功」半条随 US-7」），NivroOS 迁移承诺要求的是**内容零改动**。症状：按顶层 map 解析，`mv .nivroos .basalt` 后所有 server 静默消失，声明它们的 Agent 全部 `mcp server skipped`。
2. **加载期校验 fail-closed**（需求 §5.13「明确报错、拒绝启动，不静默取默认」）：顶层非映射 / `servers` 非列表 / 条目非映射 / `name` 缺失 / `command` 缺失或纯空白 / **server 名重复** / `env` 非映射 / `env` 值不是 `${VAR}` 精确形态 / **`transport` 字段非字符串** / `transport` 非 stdio（已决方案 A）→ **拒绝启动**，错误点名 `mcp_servers.yaml`、条目序号或 server 名与字段。症状：某条配置写错被静默跳过，运维只能从"工具怎么少了"倒推；非字符串 `transport` 若被强转成空串，还会**悄悄按缺省 stdio 获得执行含义**。
3. **`env` 值只允许 `${VAR}` 占位**（需求 §5.13 逐字；技术方案 §8.8）：精确形态 `^${[A-Za-z_][A-Za-z0-9_]*}$`；明文 / 半截占位（`${A}${B}`、`${A}x`、`$A`）→ 拒绝启动。**错误文案不得回显配置值**（值可能正是凭证）。判据载体：`workspace/mcp_test.go` 的 `NotContains(err, secret)`。症状：`GITHUB_TOKEN: ghp_xxx` 被接受并原样落日志 / 进子进程环境。
4. **`env` 在装配期解析**（技术方案 §8.8「所有 `${VAR}` 装配前统一解析」）：解析责任在 `app`（`resolveMCPEnv`），`workspace` **只校验形态、不读环境**（`workspace` 的输入是工作区文件，环境变量是进程装配面）。未设置**或值为空** → 拒绝启动，点名 `server <name> 的 env.<key>` 与变量名；**不得**静默用空串。症状：token 未注入时 MCP server 以空凭证启动，外部返回 401，排障方向错到 server 身上。
5. **连接串行、每个 server 30s 握手预算、失败 WARN + 跳过**（需求 §9.1「连 MCP server（失败跳过 + WARN）」）：`Connect` 对每个 server 依次执行 **dial（起子进程）→ initialize → tools/list**；**30s 预算只罩握手与列工具两步**（`client.Connect` + `listAllTools` 共享一个 `cctx`；常量，零新增配置键；NivroOS `REQUEST_TIMEOUT = 30s` 先例），dial **不接收 ctx**、进程生命周期由 `exec.Cmd` 持有（约束 27）。任一步失败 → WARN（固定键 `server` / `reason` / `detail`；`detail` = `safeErrorSummary` 固定短语，约束 28）+ 关闭半截会话 + **继续下一个**；`Connect` 只在 **ctx 取消 / 截止**时返回错误（其余失败都被吸收为跳过）。症状：一个坏 server 让整个 `serve` 起不来，或反向——失败被吞得连 WARN 都没有，**或**预算释放时把 server 子进程一并杀掉（R11）。
6. **连接失败的二阶后果 = 依赖它的 Agent 整只不注册**（契约 `agent-md.md` §2.1 规则 4 / 6；编程指南 §4.7）：server 失败 → 它的工具**不进注册池** → 声明该工具的 Agent 命中规则 4（`tool not registered: <tool>`）；只声明了 server 的 Agent 命中规则 6（`mcp server skipped: <server>`）。**两条 WARN 的分工是判据**：规则 4 先判（声明序），规则 4 命中时规则 6 不会再报；失败原因靠启动期 `MCP server 未连接——已跳过` 的 WARN 关联。症状：把"连接失败"当成 Agent 自己的配置错误去报，运维改 `AGENT.md` 改不到点上。
7. **工具适配后与内置工具同形，ReAct 循环不感知来源**（技术方案 §6.1 / §6.4）：适配器实现 `core.Tool` 四方法；工具名**原样使用**（不加 `mcp_` 前缀、不做命名空间）；`InputSchema` 把 server 给的 `map[string]any` 原样序列化；`Description` 原样透传。**不得**让循环 / 执行器按"来源"分支。症状：循环里出现 `if isMCP` 的分叉，工具调度的唯一归属被破坏（宪法 II）。
8. **注册顺序 = 内置 8 件 → MCP（server 文件序 → 工具列表序）**；**重名 keep-first + WARN**（复用 `Registry.Register` 既有语义：拒绝重复、保留原注册、WARN `拒绝重复注册的工具`）。内置优先：模型与 `AGENT.md` 对内置名的既有语义不因接入 MCP 而漂移。症状：某个 MCP server 提供了一个叫 `read_file` 的工具，静默覆盖内置文件工具 —— 审计里看到的 `read_file` 不再是沙箱内那个。
9. **注册池等值门禁必须在 MCP 注册之前**（`wire.go:248` 既有判据）：`registry.Names() == declaredToolNames` 比对的是**八件内置工具**；MCP 工具随后注册，池随之变大。**不得**把 MCP 注册挪到门禁之前——门禁会因"池里多了 MCP 工具"而拒绝启动。判据：`TestWire_MCPToolsRegisteredAfterEqualityGate`（把注册挪前 → 装配失败红）。
10. **列工具分页到空游标，重复游标即失败**：`tools/list` 以 `NextCursor` 驱动，直到空串；**同一游标第二次出现** → 该 server 视为列工具失败（WARN + 跳过），**不得**无限循环。症状：行为异常的 server 让启动卡死在一个 for 循环里，且没有任何超时（分页循环本身不受 30s 之外的保护——30s 预算覆盖整个 Connect，循环在预算内结束）。
11. **调用入参原样透传，不经 `float64`**：`ToolExecutor.checkToolInput` 已保证合法 JSON 对象；`mcpCallArguments` 对空串 / `null` 返回空对象 `map[string]any{}`（**不得**传 nil，避免 server 侧把 `null` 当非法），其余返回**原始 `json.RawMessage`** 交给 `CallToolParams.Arguments`（SDK 序列化时原样输出字节）。**不得**先 `json.Unmarshal` 成 `map[string]any`——`float64` 会把 `9007199254740993` 舍成 `9007199254740992`，且无任何报错。非法 / 非对象（理论上被执行器挡掉）→ `*core.InvalidInputError`。判据 R13。症状：server 收到的大整数 ID / 时间戳与模型给的值不一致，双方都认为自己是原始值。
12. **结果渲染固定**：文本块按序以 `\n` 连接；非文本块（image / audio / resource_link / resource / 未知）渲染为**类型化占位符**（`[非文本内容：image image/png]` 一类），**不得**把 base64 原文塞进模型上下文（有意差异，注 3）；无文本块时用 `StructuredContent` 的 JSON；全空 → `(MCP 工具无输出)`。占位符与兜底文案见 §6.2 口径 11。
13. **`IsError = true` → `*core.ExternalError`（server 文本整段丢弃）**：`Reason` = 固定文案 `MCP 工具返回错误：<server>/<tool>`（口径 12）；server 返回的内容块**不进入** `Reason` / `Cause` / 日志 / 审计 / 模型上下文——那是纯外部正文，可能含凭证或文件内容。**不得**把 `IsError` 结果当成功返回——模型会把"工具报错"读成"工具说没事"；也**不得**把 server 文本单行化 / 截断后塞进 Reason——截断不是脱敏（约束 28）。判据 R5（含敏感值负向）。
14. **传输 / 协议错误 → `*core.ExternalError`（自产摘要）**：`Target = mcp:<server>/<tool>`；`Reason` = 固定文案 `MCP 调用失败：<server>/<tool>`（口径 13）；原始错误只进 `Cause`（`ExternalError.Error()` 只返回 `Reason`，`Cause` 只经 `Unwrap` 供分类与进程内排障）。**不得**把 SDK 原文（可能含 server stderr / 路径 / 环境细节 / 凭证）写进 Reason / 日志 / 审计 / 模型上下文。判据 R12 后半。症状：server 崩溃时把它的 stack trace 或 token 回填给模型。
15. **取消 / 截止的身份优先于外部错误**（`classifyToolError` 判序：沙箱 → 入参 → **取消 / 截止** → 外部系统）：适配器先调 `ctxPreferred(ctx, err)`（与 HTTP 工具**同一个**判序函数，同包复用）；`ctx.Err() != nil` → 返回 **ctx 原错误**（→ `timeout`），**不得**包成 `ExternalError`。逆改点见 R6：判 `!core.IsExternalError(err)`。
16. **MCP 工具调用经执行器完整两阶段审计**（宪法 V；技术方案 §6.5 的"不经 Policy"**不**等于"不审计"）：适配器自身**不落审计、不写消息、不记日志**；`ToolExecutor` 在执行前写 `pending`、执行后更新同一行为终态，失败再记 WARN。判据 R9：server 处理函数阻塞期间，审计存储里已能看到 `pending` 行（测试用记录型假件观察；生产即 SQLite 仓储）。
17. **MCP 子进程环境 = 透传白名单 + 声明 env，其余清空**（技术方案 §6.5 子进程环境白名单；**已决方案 A**）：透传清单复用 `shell.env_passthrough`（只透传**显式登记**的变量；示例配置 `basalt.yaml.example` 登记 `PATH` / `HOME` / `LANG` / `TZ`；**代码无内置默认白名单**，省略或配置为空列表时**不透传**父进程环境变量）；`mcp_servers.yaml` 的 `env` 在透传之后覆盖同名键；结果**永不为 nil**（空切片 = 一个都不继承）。**不得**让 MCP server 继承完整父进程环境（`BASALT_*` / 云凭证 / 代理设置会一并送出去）。实现落点：`tool/runner.go` 抽出 `mergeChildEnv(passthrough, declared)`，`LocalRunner.childEnv` 与 MCP dial **共用同一函数**（同一语义只有一处实现）。
18. **MCP 子进程 cwd = 工作区根**：与 `shell` 工具的"有 Agent 上下文 → 该 Agent 目录"不同——MCP server 是**实例级**资源、不绑定单个 Agent，相对路径按工作区根解释才可预测。**不得**用进程默认 CWD（随启动目录漂移）。症状：从不同目录启动 `basalt`，同一个 `command` 里的相对路径指向不同文件。
19. **`command` 只按空白切分**（`strings.Fields`，首 token = 可执行文件；NivroOS 基线一致）：**无 shell 引号语义**——带空格的路径需要用绝对路径且路径本身不得含空白；`.cmd` / `.bat` **不自动**包 `cmd /c`（Windows 上需显式写解释器，如 `cmd /c tool.cmd`，而 `cmd` 必须是首 token）。**不得**引入 shell（`sh -c`）——那会把配置字面量变成可注入的命令行。症状：`command: "C:\Program Files\node\node.exe server.js"` 被切成 4 段，启动失败且原因难读。
20. **停机顺序：MCP → 数据库 → 实例锁**（`Operations.md` §5.3 step 4~6）：`heavyRuntime.Close` 先关池（逐会话 `Close`：stdin → 等待 → 终止 → 强杀，SDK 负责子进程收尾；子进程存活到这一步才被收——握手预算释放不杀进程，约束 27），再关库，最后放锁；三处失败 `errors.Join`，**不得**静默。池 `Close` **幂等**（重复调用不报错）。
21. **`mcp_servers.yaml` 变更不热加载**（`Compatibility.md` §2.4 热加载范围「保持一致」/ 技术方案 §11.5）：改文件后**必须重启**；本 US 不接 fsnotify、不做连接池重建。症状：文档承诺"改完即生效"而实现没有，运维在线上反复试。
22. **轻 / 重两条路对同一份 `AGENT.md` 给出一致结论**（`wire.go` 等值门禁的既有理由）：重路径注入"已配置 + 已连接"；轻路径不连 MCP（毫秒级、只读文件系统——见需求 §5.12 两条路径），因此对**可能依赖 MCP** 的 Agent 给第三态 `unverified`（**待决 #2**，正文按"采纳"书写）。零 MCP 配置时轻路径行为与 US-6 **逐字节相同**。症状：`profile list` 把用 MCP 工具的 Agent 全部标成 `tool not registered`，而 `chat` 能正常跑。
23. **启动摘要含 `mcp_connected`**（需求 §9.1 逐字「MCP 连接数」）：`重路径装配完成` 一条 INFO 的键 = `agents` / `tools` / `mcp_connected`；`mcp_connected` = **实际连上**的 server 数（不是配置数）。零 MCP 配置时该键为 `0`，**不得**省略（结构摘要的键集稳定才可机器读）。
24. **信任边界如实登记**（技术方案 §6.5）：MCP server 的文件 / 网络行为**不经** `Policy`；`mcp_servers.yaml` 的 `command` **来自配置、非模型可控**；v1 定位 = 经管理员评审的 server。**不得**在文档或日志里宣称"MCP 工具受沙箱白名单约束"。症状：安全评审据文档以为 MCP server 受路径白名单管，实际没有。
25. **MCP SDK 只做协议编解码**（宪法 I / II；技术方案 §1.1 原则 1）：`go-sdk` 只出现在 `internal/tool`；**不得**启用 SDK 的自动工具循环 / 托管能力；工具调度的唯一入口仍是 `ToolExecutor`。`workspace` **不得** import `tool`（装载器拿的是名字集），`tool` **不得** import `app` / `cli`。症状：SDK 的 agent 抽象接管循环，宪法 I 的"自实现 ReAct"名存实亡。
26. **零新增配置键、零迁移、一个表内直接坐标**：`go.mod` 只加 `github.com/modelcontextprotocol/go-sdk v1.8.0`（技术方案 §1.2 栈表已定 U-5）；30s 连接预算、超时、渲染文案都是**代码常量**；`schema.sql` 零 diff；`basalt.yaml.example` 零改动（MCP 配置在工作区的独立文件里）。
27. **进程生命周期与握手预算分离**：`dial`（起子进程）**不接收 ctx**，用 `exec.Command` 而**不是** `exec.CommandContext`；30s 预算只罩 `initialize` + `tools/list`（传给 `client.Connect` 与 `listAllTools` 的 `cctx`），两步完成即 `cancel()`，会话与子进程继续存活到 `pool.Close`。SDK 事实（v1.8.0）：`CommandTransport.Connect` 忽略 ctx、进程生命周期由 `exec.Cmd` 持有；`jsonrpc2.NewConnection` 对未实现 `propagateCancellation` 的传输包 `notDone`，故取消预算 ctx 不关闭连接。**不得**把进程绑到预算 ctx 上（`CommandContext` / 把 `cctx` 存进会话）。症状：`Connect` 返回后第一次工具调用即报连接关闭——启动看似成功、工具全部不可用；空载时测不出来。判据 R11（真实子进程，预算释放 + 等待后仍可调用）。
28. **错误正文的安全边界（凭证与外部正文不泄漏）**：`ExternalError.Reason`（= `Error()`，唯一进日志 / 审计 / 模型上下文的文本）**必须**是自产固定文案——协议 / 传输错误 = `MCP 调用失败：<server>/<tool>`，`IsError` = `MCP 工具返回错误：<server>/<tool>`；原始错误只进 `Cause`（只经 `Unwrap`、不进文案），`IsError` 的 server 文本**整段丢弃**（连 `Cause` 也不放）；连接 WARN 的 `detail` 用 `safeErrorSummary` 固定短语集，**不得**用 `err.Error()` 单行化 / 截断充数——单行化与截断都不是脱敏。边界一句话：**外部正文可以进 `Cause` 供进程内排障，但永不进任何可观测面**（日志 / 审计 / 模型上下文）。诊断代价：server 侧细节需查该 server 自己的日志——这是有意的取舍（注 12）。症状：server 把 token / 文件正文放进错误文本，Basalt 原样写进 `tool_invocations.error_message` 与模型上下文。判据 R5 / R12（三条敏感值负向：连接失败、协议错误、`IsError`）。

### 2.3 核心逻辑

#### 流程图

```text
【A 启动装配与失败分支】重路径 newRuntime（serve / chat）
  │
  ├─ ①~④ 实例锁 / 开库 / 迁移 / 崩溃恢复            （US-1~US-6 既有）
  ├─ ⑤ 策略 / 注册池 / 执行器 + 八件内置工具         （US-1~US-6 既有）
  ├─ 等值门禁：registry.Names() == declaredToolNames
  │      └─ 不等 → 拒绝启动（此时 MCP 工具**尚未**注册——顺序是硬约束，约束 9）
  │
  ├─ 读 mcp_servers.yaml（workspace.ParseMCPServers，经 os.Root）
  │      ├─ 结构非法（顶层 / servers / 条目 / name / command / 重名 / env 形态 / transport）
  │      │        └─ 拒绝启动：点名文件、条目序号或 server、字段（约束 2）
  │      ├─ 零 server（缺失 / 空 / {} / 无 servers）→ 整段跳过：不建池、不发日志
  │      └─ N 个 server（列表序）
  │             ├─ env 解析（${VAR} → 值；未设置 / 空 → 拒绝启动，点名 server + 变量，约束 4）
  │             └─ pool.Connect（串行；每 server 一个 30s 预算，只罩握手 + 列工具，约束 5 / 27）
  │                    ├─ dial（起子进程，不接收 ctx、exec.Command——进程不绑预算）
  │                    ├─ initialize / tools/list 失败
  │                    │        └─ WARN「MCP server 未连接——已跳过」（server / reason / detail=safeErrorSummary）
  │                    │           + 关闭半截会话 / 子进程 + 继续下一个（不中断启动，约束 28）
  │                    ├─ 握手完成 → cancel() 预算，会话与子进程继续存活（约束 27 / R11）
  │                    └─ 成功 → INFO「MCP server 已连接」（server / tools）+ 保留会话
  │
  ├─ 注册 MCP 工具（server 文件序 → 工具列表序；重名 keep-first + WARN，约束 8）
  ├─ ⑥ 装载 Agent（Tools = **注册后**名字集；MCPServers / MCPConnected 注入）
  │      ├─ 规则 4：声明了未注册工具（含失败 server 的工具）→ tool not registered（约束 6）
  │      ├─ 规则 6：声明了未配置 / 未连接 server → mcp server skipped（约束 6）
  │      └─ 其余 Agent 照常注册
  ├─ ⑦ 运行入口（provider 映射表一次构建，宪法 III）
  └─ 摘要日志：agents / tools / mcp_connected（约束 23）→ heavyRuntime{lock, db, mcp, service, …}（`mcp` 类型 = `mcpConnector`）
```

```text
【B 一次 MCP 工具调用】模型 → ReActLoop → ToolExecutor → 适配器 → server
  模型 tool_calls
        │
        ▼
  ToolExecutor.Execute(ctx, env, toolSet, call)
    ├─ ① Resolve(注册池)        适配器与内置工具同形，循环不感知来源（约束 7）
    ├─ ② 归属校验（本 Agent tools: 声明）
    ├─ ②' 入参校验（合法 JSON 对象 + 必填键，执行器既有）
    ├─ ③ 审计 pending（fail-closed；预写失败 → 不执行，宪法 V）
    ├─ ④ tool.Execute（单工具超时在这里建立）
    │       └─ mcpToolAdapter.Execute
    │            ├─ input → mcpCallArguments（空 / null → {}；其余原样 RawMessage，约束 11）
    │            ├─ session.CallTool(ctx, {Name, Arguments})
    │            │       └─ 子进程 stdio：SDK 只做协议编解码（约束 25）
    │            ├─ err != nil
    │            │     ├─ ctxPreferred(ctx, err) != nil → 返回 ctx 原错误 → timeout（约束 15）
    │            │     └─ 否则 → *core.ExternalError{Reason: 固定文案, Cause: err}
    │            │              → external_error（原文只进 Cause，约束 14 / 28）
    │            ├─ result.IsError → *core.ExternalError{Reason: 固定文案}（server 文本丢弃）
    │            │              → external_error（约束 13 / 28）
    │            └─ 成功 → 文本连接 / 非文本占位 / 结构化兜底（约束 12）
    ├─ ⑤ classifyToolError（沙箱 → 入参 → 取消 → 外部 → 文件 → 兜底）
    ├─ ⑥ truncateToolOutput + 终态回填**同一行** + 失败 WARN
    └─ ⑦ Result 回填会话（下一轮模型可见）
```

```text
【C Agent 准入：规则 4 / 规则 6 + 轻路径第三态】（待决 #2 按"采纳"书写）
                        连接结果已知？（MCPConnected != nil）
                        │ 是（重路径）                    │ 否（轻路径）
                        ▼                                  ▼
       规则 4：tool ∈ 注册池？                     workspace 配置了 MCP？
         否 → tool not registered                  否 → 规则 4 照旧（池 = 声明八件）
       规则 6：server ∈ 已配置？                    是 → tool ∈ 内置八件？
         否 → mcp server skipped                       是 → 通过
         server ∈ 已连接？                             否 → **待确认**（unverified）
           否 → mcp server skipped                   规则 6：server ∈ 已配置？
           是 → 通过                                    否 → mcp server skipped
                                                         是 → **待确认**（unverified）
       两条规则都按**声明序**报首个失败名；             确定性规则（1/2/3/5/7/8）照旧先判，
       规则 4 先于规则 6（约束 6）                     被拒的仍拒（不因待确认而放行）
```

```text
【D 停机】heavyRuntime.Close（Operations §5.3 step 4~6）
  ├─ ① mcp.Close()       逐会话 Close：stdin 关闭 → 等待 → 终止 → 强杀（SDK 收尾子进程）
  │                     幂等；多会话错误 errors.Join（约束 20）
  ├─ ② db.Close()        写池最后一批使用者收尾
  └─ ③ lock.Release()    所有写入停止后才放锁
  任一步失败 → 错误链返回，不静默
```

#### 伪代码（签名与 §3.1 逐字一致）

```text
# 配置解析（workspace）—— 事实源：Compatibility §2.1 + 需求 §5.13 + 技术方案 §6.3
ParseMCPServers(root) ([]core.MCPServerConfig, error):
    base, err := os.OpenRoot(root)                 # 工作区根句柄；软连接越界在根上被挡
    err != nil → return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
    defer base.Close()
    return parseMCPServers(base)

parseMCPServers(base *os.Root) ([]core.MCPServerConfig, error):
    raw, err := base.ReadFile("mcp_servers.yaml")
      errors.Is(err, fs.ErrNotExist) → return nil, nil          # 缺失 = 零 server
      err != nil                     → return nil, fmt.Errorf("读取 mcp_servers.yaml: %w", err)
    TrimSpace(raw) == ""             → return nil, nil          # 空文件
    tree, err := yaml.Unmarshal(raw)                            # goccy/go-yaml
      err != nil → return nil, fmt.Errorf("解析 mcp_servers.yaml: %s", frontmatterErrorSummary(err))
    tree == nil → return nil, nil                               # `null` 文档 / 纯注释（注释不产生节点）
    m, ok := tree.(map[string]any)
      !ok → return nil, fmt.Errorf("mcp_servers.yaml 的顶层必须是映射（servers: 列表），实际是 %T", tree)
    rawList, present := m["servers"]
      !present || rawList == nil → return nil, nil              # 兼容 US-1 模板 `{}`
    items, ok := rawList.([]any)
      !ok → return nil, fmt.Errorf("mcp_servers.yaml 的 servers 必须是列表，实际是 %T", rawList)
    out := make([]core.MCPServerConfig, 0, len(items))
    seen := map[string]bool{}
    for i, item := range items:
        e, ok := item.(map[string]any)
          !ok → return nil, fmt.Errorf("mcp_servers.yaml 的 servers[%d] 必须是映射，实际是 %T", i, item)
        name := stringField(e, "name")
          name == "" → return nil, fmt.Errorf("mcp_servers.yaml 的 servers[%d] 缺少 name", i)
        seen[name] → return nil, fmt.Errorf("mcp_servers.yaml 的 server 名重复: %s", name)
        seen[name] = true
        transport := ""
        if rawTransport, present := e["transport"]; present && rawTransport != nil:
            typed, ok := rawTransport.(string)
              !ok → return nil, fmt.Errorf("mcp_servers.yaml 的 server %s 的 transport 必须是字符串，实际是 %T", name, rawTransport)
            transport = typed
          transport != "" && transport != "stdio"
              → return nil, fmt.Errorf("mcp_servers.yaml 的 server %s 的 transport 不受支持（%s）——v1 只支持 stdio", name, transport)
        command := stringField(e, "command")
          strings.TrimSpace(command) == ""
              → return nil, fmt.Errorf("mcp_servers.yaml 的 server %s 缺少 command", name)
        env := map[string]string{}
        if v, present := e["env"]; present && v != nil:
            em, ok := v.(map[string]any)
              !ok → return nil, fmt.Errorf("mcp_servers.yaml 的 server %s 的 env 必须是映射", name)
            for key, val := range em:
                s, ok := val.(string)
                !ok || !isEnvPlaceholder(s)
                    → return nil, fmt.Errorf("mcp_servers.yaml 的 server %s 的 env.%s 必须是 ${VAR} 占位（明文凭证不得入库）", name, key)
                env[key] = s
        out = append(out, core.MCPServerConfig{Name: name, Command: command, Env: env})
    return out, nil

# 装配期 env 解析（app）—— 技术方案 §8.8：装配前统一解析，未解析残留即拒
resolveMCPEnv(specs []core.MCPServerConfig, lookup func(string) (string, bool)) ([]core.MCPServerConfig, error):
    for i := range specs:
        resolved := make(map[string]string, len(specs[i].Env))
        for key, ref := range specs[i].Env:
            varName := strings.TrimSuffix(strings.TrimPrefix(ref, "${"), "}")
            value, ok := lookup(varName)
            !ok || value == ""
                → return nil, fmt.Errorf("mcp_servers.yaml 的 server %s 的 env.%s 引用的 %s 未设置或为空", specs[i].Name, key, varName)
            resolved[key] = value                      # 值不进错误、不进日志
        specs[i].Env = resolved
    return specs, nil

# 连接（tool）—— 串行；30s 预算只罩握手 + 列工具，不罩进程生命周期（约束 27）
MCPPool.Connect(ctx, specs) ([]MCPConnection, error):
    for _, spec := range specs:
        if ctx.Err() != nil: return conns, ctx.Err()          # 只有取消 / 截止是池级错误
        transport, err := p.dial(spec)                        # 只起子进程：不接收 ctx、exec.Command
        if err != nil:
            p.logger.Warn("MCP server 未连接——已跳过", "server", spec.Name,
                          "reason", "启动失败", "detail", safeErrorSummary(err))
            continue
        cctx, cancel := context.WithTimeout(ctx, p.connectTimeout)   # 缺省 30s：只罩下面两步
        session, err := p.client.Connect(cctx, transport, nil)
        if err != nil:
            # SDK 在 Connect 失败路径已 Close 会话（stdin 关闭 → 等待 → 终止 → 强杀）；
            # 传输层失败（Start 失败）根本没有进程——两条路径都不需要再关
            p.logger.Warn("MCP server 未连接——已跳过", "server", spec.Name,
                          "reason", "初始化失败", "detail", safeErrorSummary(err))
            cancel(); continue
        tools, err := listAllTools(cctx, session)
        if err != nil:
            _ = session.Close()                               # 收起半截会话（连带子进程）
            p.logger.Warn("MCP server 未连接——已跳过", "server", spec.Name,
                          "reason", "列工具失败", "detail", safeErrorSummary(err))
            cancel(); continue
        cancel()                                              # 预算到此为止；会话 / 子进程存活到 Close（R11 钉住）
        ads := make([]core.Tool, 0, len(tools))
        for _, t := range tools: ads = append(ads, newMCPToolAdapter(session, spec.Name, t))
        p.mu.Lock(); p.sessions = append(p.sessions, session); p.mu.Unlock()
        conns = append(conns, MCPConnection{Server: spec.Name, Tools: ads})
        p.logger.Info("MCP server 已连接", "server", spec.Name, "tools", len(ads))
    return conns, nil

# 连接失败的安全摘要（tool）—— 固定短语集，永不返回 err.Error()（约束 28）
safeErrorSummary(err) string:
    errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) → return "可执行文件不存在"
    errors.Is(err, fs.ErrPermission)                                   → return "权限不足"
    errors.Is(err, context.DeadlineExceeded)                           → return "超时"
    errors.Is(err, context.Canceled)                                   → return "已取消"
    var exit *exec.ExitError; errors.As(err, &exit)                    → return "子进程退出码 N"
    default                                                            → return "外部交互失败"
    # 命令名 / server 消息 / 路径等原文一律不出现——它们可能含凭证（R12 前半）

listAllTools(ctx, lister) ([]*mcp.Tool, error):
    var all []*mcp.Tool; cursor := ""; seen := map[string]bool{}
    for:
        page, err := lister.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
          err != nil → return nil, err
        all = append(all, page.Tools...)
        cursor = page.NextCursor
        cursor == "" → return all, nil
        seen[cursor] → return nil, errors.New("tools/list 游标重复——终止分页")
        seen[cursor] = true

MCPPool.Close() error:
    p.mu.Lock(); sessions := p.sessions; p.sessions = nil; p.mu.Unlock()
    errs := []error{}
    for _, s := range sessions: errs = append(errs, s.Close())   # 幂等：第二次 sessions 为空
    return errors.Join(errs...)

MCPPool.dialCommand(spec) (mcp.Transport, error):     # 不接收 ctx——进程不绑预算（约束 27）
    argv := strings.Fields(spec.Command)              # 无引号语义（约束 19）
    argv == [] → return nil, errors.New("command 为空")
    cmd := exec.Command(argv[0], argv[1:]...)         # 不得用 exec.CommandContext
    cmd.Dir = p.root                                  # 工作区根（约束 18）
    cmd.Env = mergeChildEnv(p.envPassthrough, spec.Env)        # 透传 + 声明（约束 17）
    // stderr 不接进程默认：由 CommandTransport 的默认行为丢弃（避免把 server stderr 混进终端）
    return &mcp.CommandTransport{Command: cmd}, nil

# 入参转换（tool）—— 原样字节透传，大整数不经 float64（约束 11）
mcpCallArguments(input json.RawMessage) (any, error):
    s := strings.TrimSpace(string(input))
    s == "" || s == "null" → return map[string]any{}, nil         # null 在 server 侧可能被当非法
    var obj map[string]json.RawMessage
    json.Unmarshal(input, &obj)
      err != nil → return nil, err                                # 只做形态校验：值仍是 RawMessage
    return json.RawMessage(input), nil                            # 原始字节交给 SDK 序列化

# 适配器（tool）—— Reason 一律自产固定文案，外部正文不进可观测面（约束 28）
mcpToolAdapter.Execute(ctx, env, input):
    args, err := mcpCallArguments(input)
    err != nil → return Result{}, &core.InvalidInputError{Tool: a.Name(), Reason: "MCP 工具入参必须是 JSON 对象"}
    res, err := a.session.CallTool(ctx, &mcp.CallToolParams{Name: a.name, Arguments: args})
    if err != nil:
        if ce := ctxPreferred(ctx, err); ce != nil: return Result{}, ce      # → timeout（约束 15）
        return Result{}, &core.ExternalError{
            Target: "mcp:" + a.server + "/" + a.name,
            Reason: "MCP 调用失败：" + a.server + "/" + a.name,             # 固定文案；原文只进 Cause
            Cause:  err,
        }                                                                     # → external_error（约束 14 / 28）
    if res.IsError:
        return Result{}, &core.ExternalError{
            Target: "mcp:" + a.server + "/" + a.name,
            Reason: "MCP 工具返回错误：" + a.server + "/" + a.name,         # server 文本整段丢弃（约束 13）
        }                                                                     # → external_error
    return Result{Content: renderCallToolResult(res)}, nil                    # Retryable 恒 false

renderCallToolResult(res) string:
    texts := [c.Text for c in res.Content where type(c) == *mcp.TextContent]
    len(texts) > 0 → return strings.Join(texts, "\n")
    res.StructuredContent != nil → if b, err := json.Marshal(res.StructuredContent); err == nil: return string(b)
    placeholders := [placeholder(c) for c in res.Content]     # 非文本：不回显 base64（约束 12）
    len(placeholders) > 0 → return strings.Join(placeholders, "\n")
    return "(MCP 工具无输出)"

# 装载器（workspace）—— 规则 4 / 6 的 US-7 半条
AgentLoader.LoadAll(root):
    ...                                                        # US-1~US-6 既有读取 / 排序 / 技能根
    mcpNames := l.mcpServers
    if mcpNames == nil:                                        # 轻路径：自读一次（文件只读一遍，陷阱 39）
        specs, err := parseMCPServers(base)
        err != nil → return nil, err
        mcpNames = mcpServerNames(specs)
    for _, d := range dirs: ... loadOne(base, lib, d.Name(), mcpNames, l.mcpConnected, claim)

loadOne 规则 4:
    for _, toolName := range profile.Tools:
        if slices.Contains(l.tools, toolName): continue
        if l.mcpConnected == nil && len(mcpNames) > 0: defer = true; continue   # 轻路径待确认
        return reject{reason: "tool not registered: " + toolName}

loadOne 规则 6:
    for _, m := range profile.MCPServers:
        if !slices.Contains(mcpNames, m):
            return reject{reason: "mcp server skipped", detail: m}
        if l.mcpConnected == nil && len(mcpNames) > 0: defer = true; continue
        if !slices.Contains(l.mcpConnected, m):
            return reject{reason: "mcp server skipped", detail: m}

loadOne 收尾:
    if defer && rej.reason == "":
        entry.Unverified = true                               # 只标记，不放行、不参与运行（待决 #2）
        l.logger.Info("Agent 注册待确认", "agent", name)
```

### 2.4 差异裁决注

1. **注 1（配置文件形态以 Compatibility 为准）**：技术方案 §6.3 只说"配置 `mcp_servers.yaml`"，未规定形态；`DemandAnalysis` §5.13 只列字段（name / transport / command / env）。形态的裁决来自 `Compatibility.md` §2.1——工作区布局保持一致、**文件内容零改动**即可 `mv`——故取 NivroOS 的「顶层映射 + `servers:` 列表」。US-1~US-6 的 `readMCPServerNames` 按「顶层键名集」解析是**临时占位**（源码注释逐字写明"规则 6 的「连接成功」半条随 US-7"），本次**替换**而非叠加：同一文件只留一份解析器。`{}`（US-1 模板字节）在两种形态下都表示零 server，模板**字节不动**、只改注释（注 7）。
2. **注 2（非 stdio transport 与 NivroOS 的差异）**：NivroOS 对非 stdio 条目 **WARN 后跳过**；本文按**待决 #1 的默认建议**取**拒绝启动**（fail-closed，与需求 §5.13「格式非法明确报错、拒绝启动」同向）。代价：一份含 `transport: sse` 的 NivroOS 配置在 Basalt 上**起不来**，而 NivroOS 只是少一个 server——这与"文件内容零改动"的承诺在**无效配置**上产生张力，故列为待决而非既定。改判落点见待决 #1 的换向点。
3. **注 3（非文本内容渲染是有意差异）**：NivroOS 对 MCP 内容块走 `toString()`（图片即 base64 文本）。本文取**类型化占位符**，不回显 base64：base64 进模型上下文既烧预算又无工具价值，且会把二进制内容写进会话历史（审计之外的又一份副本）。这是**有意改变**，§3.5 改造点 ⑨ 把它登记进 `Compatibility.md`。
4. **注 4（`not_found` 对 MCP 工具不可达）**：`classifyToolError` 的 `not_found` 依据 `errors.Is(err, fs.ErrNotExist)`；MCP 适配器只会返回 `*core.InvalidInputError`、ctx 错误或 `*core.ExternalError`，**不会**产出 `fs.ErrNotExist`。结论：MCP 工具失败只可能落在 `invalid_input` / `timeout` / `external_error` 三档。这是判序的自然结果，不是缺口；测试**不得**写"MCP 工具失败 → not_found"的断言。
5. **注 5（轻 / 重两条路的第三态）**：轻路径不连 MCP（需求 §5.12：毫秒级、只读文件系统），因此无法知道"某名字是不是某个 MCP server 的工具"、"某个 server 连没连上"。US-6 之前这不是问题（工具池 = 固定的八件声明名）；US-7 之后，**规则 4 / 6 的判据依赖连接结果**，两态模型（registered / rejected）会把所有用 MCP 的 Agent 误报为拒载。**待决 #2** 建议引入第三态 `unverified`：只改变**展示与摘要**，不改变运行面（`newAgentSource` 仍只收 `Registered` 条目；重路径不产生 `Unverified`）。若裁决不采纳，则退化为"轻路径按 US-6 两态、接受误报"，落点见该待决的换向点。
6. **注 6（MCP 子进程的环境与 cwd 是本 US 的设计选择）**：技术方案 §6.5 的「子进程环境变量白名单」写在 `shell` 工具条目下；MCP server 同样是子进程、同样由配置启动，**没有理由**给它更宽的环境面。故复用 `shell.env_passthrough`（**待决 #3**）。`cwd = 工作区根` 是本文的**补充选择**：技术方案未规定，本文按"实例级资源不绑定单 Agent"给出可预测的默认（已决 #9）。两者都写进 §6.2 固定口径，实现错即红（R2 / 子进程环境用例）。
7. **注 7（US-1 模板注释的修正）**：`templates.go:89-92` 的注释写「键为 server 名，值为启动配置」，描述的是 US-1~US-6 的临时顶层-map 形态，与 Compatibility 固定的 `servers:` 列表不符。本 US **只改注释**，`{}` 字节保持不变——`init_test.go:56-69` 断言的是"可解析为空映射"，不是逐字节；模板的**可解析结果**零变化。
8. **注 8（30s 连接预算是常量、零配置键）**：技术方案未把 MCP 超时列为配置项，NivroOS 的 `REQUEST_TIMEOUT = 30s` 是现成先例。本 US 取同一数值为**代码常量**（`MCPPoolOptions.ConnectTimeout` 仅供测试注入，配置面不暴露）；**不得**顺手新增 `mcp.connect_timeout` 之类的键（宪法 X / 本 US 约束 26）。
9. **注 9（工具名不加命名空间）**：v1 原样使用 server 的工具名（NivroOS 一致）。重名靠"内置优先 + 先连上的 server 优先 + WARN"收敛；这保留 `AGENT.md` 的 `tools:` 与 `mcp_servers:` 两段声明各自独立可读。命名空间（`server.tool`）留到确有多 server 重名需求时再议——它会给所有 `AGENT.md` 带来一次性迁移。
10. **注 10（`command` 空白切分与 Windows）**：NivroOS 基线按 `\s+` 切分、首 token 为可执行文件；本文沿用（`strings.Fields`）。由此派生的已知边界：含空白的路径无法表达；`.cmd` / `.bat` 需显式 `cmd /c`。**不得**为了让 Windows 好用以"检测扩展名就自动包 `cmd`"的方式暗中改变 argv 语义——那是隐式 shell，与"argv 直执"的纪律冲突。集成测试在测试二进制路径含空白时**显式跳过并写明理由**（§4.1）。
11. **注 11（进程生命周期与握手预算分离——SDK 事实）**：R1 伪代码把 `cctx` 传给 `dial` 并用 `exec.CommandContext` 起进程，`Connect` 末尾 `cancel()` 时子进程被一并杀掉——启动看似成功，第一次工具调用即报连接关闭。v1.8.0 源码事实：`CommandTransport.Connect(ctx, ...)` **忽略 ctx**（进程生命周期由 `t.Command` 的 `exec.Cmd` 持有，`Close` 才走 stdin → 等待 → SIGTERM → SIGKILL）；`jsonrpc2.NewConnection` 对未实现 `propagateCancellation` 的传输把 ctx 包成 `notDone`（`Done()` 恒 nil），因此取消预算 ctx **不会**关闭连接。裁决：`dial` 去掉 ctx、用 `exec.Command`；预算只罩 `client.Connect` + `listAllTools`；预算释放后进程存活到 `pool.Close`（约束 27）。SDK 升级时 R11 就是这条事实的钉子——若未来版本开始传播取消，R11 立即红（需求 §12.1 的升级前置）。
12. **注 12（安全摘要与外部错误正文的边界）**：三个面分开处置——① `Reason`（`Error()` 的全文，也是 `tool_invocations.error_message`、失败 WARN 的 `detail`、回填模型 content 的唯一来源）**只放自产固定文案**；② `Cause` 保留原始错误，仅经 `Unwrap` 供 `errors.Is/As` 与进程内排障，`ExternalError.Error()` 不拼接它（`errors.go` 既有纪律："原因链只经 Unwrap，不进文案"）；③ `IsError` 的 server 文本是**纯外部正文**（不是传输错误），没有进程内用途，整段丢弃——连 `Cause` 也不放。同包先例：HTTP 工具对 `Location` 原文的处理（"只进原因链、不进文案"）。诊断代价如实登记：server 侧细节只能查该 server 自己的日志，Basalt 侧只保留"哪个 server / 哪个阶段 / 哪类失败"（§6.1 人工项 10）。

## 3. 交付物清单

### 3.1 代码交付物

#### 新增 6 文件

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `internal/core/mcp.go` | MCP server 的**纯配置类型**（`core` 只依赖标准库）：`MCPServerConfig`——`workspace` 产出时 `Env` 是 `${VAR}` 占位形态，`app` 的 `resolveMCPEnv` 之后才是最终值（同类型两阶段，已决 #2） | `type MCPServerConfig struct { Name string; Command string; Env map[string]string }`（无方法） |
| `internal/workspace/mcp.go` | `mcp_servers.yaml` 解析与形态校验：顶层映射 + `servers:` 列表、条目字段校验、重名拒载、`env` 占位形态（**精确 `${VAR}`**）、`transport` 取值域、文件顺序保留；**不读环境变量**；缺失 / 空 / `{}` / 无 `servers` → 零 server | `func ParseMCPServers(root string) ([]core.MCPServerConfig, error)`；非导出：`func parseMCPServers(base *os.Root) ([]core.MCPServerConfig, error)` / `func mcpServerNames(specs []core.MCPServerConfig) []string` / `func isEnvPlaceholder(s string) bool` |
| `internal/tool/mcp.go` | `MCPPool`：单 `mcp.Client`（多会话）、串行连接、每 server 30s 预算（**只罩 initialize + tools/list**；dial 不接收 ctx）、分页到空游标 + 重复游标守卫、失败 WARN 跳过（`detail` = `safeErrorSummary`）、会话保留与幂等 `Close`；`dialCommand` 用 `exec.Command` 起 `mcp.CommandTransport`（`Dir` = 工作区根，`Env` = `mergeChildEnv`；**不得**用 `exec.CommandContext`，约束 27）；非导出 `dial` 注入点供**包内白盒测试**替换传输 | `type MCPConnection struct { Server string; Tools []core.Tool }`；`type MCPPoolOptions struct { Logger *slog.Logger; EnvPassthrough []string; ConnectTimeout time.Duration; Version string; Root string }`（`ConnectTimeout == 0` → 30s）；`func NewMCPPool(opts MCPPoolOptions) *MCPPool`；`func (p *MCPPool) Connect(ctx context.Context, specs []core.MCPServerConfig) ([]MCPConnection, error)`；`func (p *MCPPool) Close() error`；非导出：`func (p *MCPPool) dialCommand(spec core.MCPServerConfig) (mcp.Transport, error)` / `func listAllTools(ctx context.Context, lister toolLister) ([]*mcp.Tool, error)` / `func safeErrorSummary(err error) string` / `type toolLister interface { ListTools(ctx context.Context, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error) }`；字段 `dial func(core.MCPServerConfig) (mcp.Transport, error)` |
| `internal/tool/mcptools.go` | `mcpToolAdapter`：实现 `core.Tool`；名称 / 描述 / schema 原样；`Execute` 的入参（原样 `json.RawMessage` 透传，约束 11）、结果渲染、错误映射（约束 12~16 / 28）；**不落审计、不写消息、不记日志**；非文本占位与入参转换辅助 | `type mcpToolAdapter struct{ ... }`；`func newMCPToolAdapter(session *mcp.ClientSession, server string, t *mcp.Tool) *mcpToolAdapter`；`func (a *mcpToolAdapter) Name() string` / `Description() string` / `InputSchema() json.RawMessage` / `Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error)`；非导出：`func mcpCallArguments(input json.RawMessage) (any, error)` / `func renderCallToolResult(res *mcp.CallToolResult) string` / `func mcpContentPlaceholder(c mcp.Content) string` |
| `internal/app/mcp.go` | 装配侧 MCP 辅助：`${VAR}` 解析（注入 `lookup`，生产 = `os.LookupEnv`）、连接器接口（`App` 的测试夹层，沿 `providerOverride` 先例）、连接结果 → 工具名集 | `type mcpConnector interface { Connect(ctx context.Context, specs []core.MCPServerConfig) ([]tool.MCPConnection, error); Close() error }`；`func resolveMCPEnv(specs []core.MCPServerConfig, lookup func(string) (string, bool)) ([]core.MCPServerConfig, error)`；`func mcpToolNames(conns []tool.MCPConnection) []string` |
| `internal/testutil/mcpserver.go` | 测试基建（**非 `_test.go`**，供跨包测试 import）：进程内 in-memory server 夹具（可设 `PageSize`），标准测试工具集（`echo` / `boom` / `blob`），以及集成层子进程入口 `RunMCPHelper`（`BASALT_MCP_HELPER` 未设置时 `t.Skip` 并写明理由） | `type MCPTestTool struct { Name, Description string; Schema any; Handler mcp.ToolHandler }`；`func NewInMemoryMCPPair(t *testing.T, pageSize int, tools ...MCPTestTool) mcp.Transport`；`func StandardMCPTools() []MCPTestTool`；`func RunMCPHelper(t *testing.T)` |

#### 改动 6 文件

| 文件 | 改什么 | 关键方法签名（改动面） |
| --- | --- | --- |
| `internal/workspace/agent.go` | ① `AgentLoaderOptions` 增 `MCPServers` / `MCPConnected`（nil 的各自身语义见 §2.3）；② `AgentLoader` 增两字段并在构造期 `slices.Clone`；③ `LoadAll` 用 `parseMCPServers` 取名字集（`MCPServers == nil` 时），删除 `readMCPServerNames`；④ `loadOne` 增 `mcpConnected` 参数，规则 4 / 6 按 §2.3 改写（含待确认分支）；⑤ 拒绝信息与 WARN 键不变 | `type AgentLoaderOptions struct { Providers []string; Tools []string; MCPServers []string; MCPConnected []string; Logger *slog.Logger }`；`func NewAgentLoader(opts AgentLoaderOptions) *AgentLoader`（签名不变）；`func (l *AgentLoader) LoadAll(root string) ([]core.AgentEntry, error)`（签名不变） |
| `internal/workspace/templates.go` | `templateMCPServersYAML` **只改注释**：描述 `servers:` 列表形态与字段；`{}` 字节不动（注 7） | — |
| `internal/core/profile.go` | `AgentEntry` 增 `Unverified bool`（**待决 #2**）：`Registered == false && Unverified == true` = 轻路径待确认；`Profile` 非 nil 的既有语义不变 | `type AgentEntry struct { Name string; Registered bool; Unverified bool; Profile *Profile; RejectReason string }` |
| `internal/cli/profile.go` | 轻路径第三态的展示（**待决 #2**）：`renderProfileLine` 增 unverified 分支、`renderProfileList` 的汇总在**未验证数 > 0** 时追加后缀（零计数时逐字节不变）；`registeredCount` 语义不变（只数 `Registered`） | `func renderProfileList(entries []core.AgentEntry) string` / `func renderProfileLine(entry core.AgentEntry) string`（签名不变） |
| `internal/tool/runner.go` | 抽出 `mergeChildEnv(passthrough []string, declared map[string]string) []string`：`LocalRunner.childEnv` 改为调用它（行为**逐字节不变**——既有 runner 用例是回归判据）；MCP dial 复用同一函数 | `func mergeChildEnv(passthrough []string, declared map[string]string) []string`；`func (r *LocalRunner) childEnv(cmd core.Command) ([]string, error)`（签名不变） |
| `internal/app/wire.go` | ① `App` 增 `mcpConnect mcpConnector`（测试夹层；nil → 生产 `tool.NewMCPPool`）；② 等值门禁之后插入「解析 → env 解析 → 建池 → `Connect` → 注册 MCP 工具」；③ 装载器注入 `MCPServers` / `MCPConnected`；④ 摘要日志增 `mcp_connected`；⑤ 池创建后的失败路径统一走本地 `fail` 闭包（先 `pool.Close()` 再 `abortRuntime`）；⑥ `heavyRuntime` 增 `mcp mcpConnector`，`Close` 顺序改为 **池 → 库 → 锁** | （无新增导出签名；`type App struct { ...; mcpConnect mcpConnector }`；`func (r *heavyRuntime) Close() error`（签名不变）） |

#### 依赖坐标

| 文件 | 改动 | 核实 |
| --- | --- | --- |
| `go.mod` / `go.sum` | 新增**直接**坐标 `github.com/modelcontextprotocol/go-sdk v1.8.0`（技术方案 §1.2 栈表已定 U-5）；`go-sdk` 自身的传递依赖随 `go mod tidy` 落地 | 写前 `GOPROXY=https://goproxy.cn go list -m -versions github.com/modelcontextprotocol/go-sdk` 核实版本存在（本轮已核实 v1.8.0 在 Central 与模块缓存中）；若出现**表外直接坐标**（非传递依赖）→ 停下问用户 |

### 3.2 测试交付物

#### 新增 6 文件 51 条

> 收口复核后实测：默认层 50 条 + 集成层 5 条 = **55 条**（本轮新增 4 个函数，见文末「收口补登」；下表为批准版设计基线，计数不动）。

| 文件 | 条数 | 用例（函数名英文；中文验收点以注释保留） |
| --- | --- | --- |
| `internal/workspace/mcp_test.go` | 18 | **`package workspace_test`**。`TestParseMCPServers_MissingOrEmptyYieldsZero`（缺失 / 空文件 / 纯注释 / `{}` / `servers: null` 五态）· `TestParseMCPServers_ListOrderPreserved`（**不排序**——连接序与注册序的判据）· `TestParseMCPServers_RejectsNonMappingRoot` · `TestParseMCPServers_RejectsServersNotList` · `TestParseMCPServers_RejectsEntryNotMapping` · `TestParseMCPServers_RequiresNameAndCommand`（缺 name / 空 command / 纯空白 command 三态）· `TestParseMCPServers_RejectsDuplicateNames` · `TestParseMCPServers_RejectsEnvNotMapping` · **`TestParseMCPServers_RejectsPlaintextEnvWithoutEchoingValue`**（**R1a**）· `TestParseMCPServers_AcceptsExactPlaceholderOnly`（`${A}` 通过；`${A}${B}` / `${A}x` / `$A` / `${}` / `${1A}` 拒）· `TestParseMCPServers_TransportDefaultsToStdio` · `TestParseMCPServers_RejectsUnsupportedTransport`（待决 #1 默认）· `TestParseMCPServers_IgnoresUnknownEntryKeys` · `TestParseMCPServers_EnvKeysPreserved` · `TestParseMCPServers_ReadsThroughRoot`（工作区外不可达 / 软连接越界被挡）· `TestParseMCPServers_ParseErrorIsSummarized`（库文案不外传）· `TestMCPServerNames_PreservesConfigOrder` · `TestIsEnvPlaceholder_Table` |
| `internal/tool/mcp_test.go` | 18 | **`package tool`**（白盒：替换非导出 `dial`，与 `runner_windows_test.go` 同包先例）。`TestMCPPool_ConnectReturnsConnectionsInSpecOrder` · **`TestMCPPool_ConnectSkipsFailedServerAndKeepsOthers`**（**R2**）· `TestMCPPool_ConnectFailureClosesPartialSession`（dial 成功但 initialize 失败）· `TestMCPPool_ListToolsPaginatesToEmptyCursor`（**R7** 前半：`PageSize = 1` + 3 工具）· **`TestListAllTools_RepeatedCursorFails`**（**R7** 后半：重复游标 → error，不死循环）· `TestMCPPool_ConnectReturnsErrorOnlyOnContextCancellation` · `TestMCPPool_CloseIsIdempotent`（**R10** 前半：两次 Close 均无错、会话已关）· `TestMCPPool_DialCommandEnvAndDir`（`mergeChildEnv` 透传 + 声明覆盖 + `Dir` = 根 + `cmd.Cancel == nil`（非 `CommandContext` 创建）；**不真起进程**——检查构造出的 `exec.Cmd`）· **`TestMCPPool_ConnectFailureLogOmitsSensitiveDetail`**（**R12** 前半：dial 错误含哨兵 → 日志只有安全短语）· **`TestMCPToolAdapter_MapsIsErrorToExternalError`**（**R5**：server 文本含哨兵 → Reason 固定、文本不回显）· **`TestMCPToolAdapter_ProtocolErrorOmitsSensitiveDetail`**（**R12** 后半：server handler 错误的哨兵不进 `err.Error()`）· **`TestMCPToolAdapter_ContextIdentityWinsOverExternalError`**（**R6**）· **`TestMCPToolAdapter_CallIsAuditedBeforeExecute`**（**R9**：执行器两阶段审计——阻塞期间 `pending` 已落库、放行后同一行回填终态）· `TestMCPToolAdapter_RendersTextBlocksJoined` · `TestMCPToolAdapter_RendersNonTextAsPlaceholderNoBase64` · `TestMCPToolAdapter_FallsBackToStructuredContentAndNoOutput` · `TestMCPToolAdapter_InputObjectAndEmptyInput` · **`TestMCPCallArguments_PreservesLargeIntegers`**（**R13**：`9007199254740993` 原样序列化） |
| `internal/tool/mcp_integration_test.go` | 5 | **`//go:build integration`**。本仓**首个起真实子进程**的集成层（需求 §12.1 明示要求）。`TestMCPIntegration_ConnectListAndCallOverStdio`（`os.Args[0] -test.run=^TestMCPHelperProcess$` + `BASALT_MCP_HELPER=1`；连接 → 列工具 → 调 `echo` 拿回文本）· **`TestMCPIntegration_CallSucceedsAfterConnectBudgetReleased`**（**R11**：`Connect` 返回、预算已释放并等待后，`echo` 仍可调用——进程生命周期与预算分离）· `TestMCPIntegration_BadCommandIsSkipped`（`command: basalt-mcp-no-such-binary` → `Connect` 无错、零连接、WARN 含 server 名）· `TestMCPIntegration_PaginationOverStdio`（`BASALT_MCP_PAGE_SIZE=1` + 3 工具全列出）· `TestMCPIntegration_ServerDiesMidCall`（`boom` 工具让 server 退出 → 调用返回 `external_error`，不是 `timeout`）。**辅助入口**：`func TestMCPHelperProcess(t *testing.T) { testutil.RunMCPHelper(t) }`（未设 `BASALT_MCP_HELPER` → `t.Skip("仅作为 MCP stdio 子进程入口运行")`——默认层跑它时是跳过，不是失败）；辅助件 `helperCommand(t)` 见 §4.3 |
| `internal/app/wire_mcp_test.go` | 7 | **`package app`**。`TestWire_MCPToolsRegisteredAfterEqualityGate`（**R3 前半**：池含 MCP 工具；把注册挪到门禁前 → 装配失败红）· `TestWire_AgentDeclaringFailedMCPToolIsNotRegistered`（**R3**：日志含 `Agent 未注册` 且 `reason=tool not registered: <tool>`；装配本身成功）· `TestWire_AgentDeclaringFailedServerOnlyIsNotRegistered`（规则 6：`mcp server skipped`）· `TestWire_DuplicateMCPToolKeepsBuiltin`（**R8**：MCP 也提供 `read_file` → `Resolve("read_file")` 仍是内置工具 + WARN `拒绝重复注册的工具`）· `TestWire_StartupSummaryCountsConnectedServers`（**R3 后半**：`mcp_connected` = 实际连接数，不是配置数）· `TestWire_ConnectCanceledAbortsAndClosesPool`（连接器返回 ctx 错误 → 装配失败 + 池 `Close` 被调用）· **`TestWire_CloseClosesMCPBeforeDatabase`**（**R10** 后半：写入一条 MCP 配置建池；假连接器的 `Close` 里 `rt.db.SchemaVersion` 仍成功——顺序错了这里红） |
| `internal/app/mcp_test.go` | 1 | **`package app`**（直接调非导出 `resolveMCPEnv`）。**`TestResolveMCPEnv_MissingOrEmptyFailsClosed`**（**R1b**：lookup 注入——未设置与空值同档拒绝，错误点名 server 与变量名、不含值；`"tok-1"` 通过并取到解析值） |
| `internal/testutil/mcpserver_test.go` | 2 | `TestMCPTestServer_StandardToolsAreListed`（`echo` / `boom` / `blob` 三件 + schema 非空）· `TestRunMCPHelper_SkipsWithoutEnv`（未设 `BASALT_MCP_HELPER` → `t.Skip`，跳过理由可读） |

#### 改动 2 文件，追加 6 条 + 夹具改造 2 点

| 文件 | 追加条数 | 用例 / 改动 |
| --- | --- | --- |
| `internal/workspace/agent_test.go` | 4 + **夹具改造 2 点** | `TestAgentLoader_DeclaredServerConnectedRegistersAgent`（`MCPServers` / `MCPConnected` 均注入 → 注册）· **`TestAgentLoader_ConfiguredButNotConnectedRejects`**（**R4**：配置有、`MCPConnected` 为空 → `mcp server skipped`；守卫"接线忘了传连接结果"）· `TestAgentLoader_LightPathMarksMCPAgentsUnverified`（**待决 #2**：`MCPConnected == nil` 且配置非空 → `Unverified`、`Registered == false`、`RejectReason == ""`；确定性规则仍先拒）· `TestAgentLoader_NoMCPConfigKeepsTwoStateBehavior`（零配置 → 与 US-6 逐字节同判）。**夹具改造**：① `writeMCPServers` 从顶层 map 改为 `servers:` 列表（`name` + `command`）；② `TestAgentLoader_ParsesAllFrontmatterFields` 的装载器构造改为注入 `MCPServers: []string{"search"}` 与 `MCPConnected: []string{"search"}`——旧断言的语义从"文件里存在"升级为"已配置**且已连接**"（US-7 的规则 6 全条） |
| `internal/cli/profile_test.go` | 2 | `TestRenderProfileList_UnverifiedLine`（逐字 `%s：registered=unverified——MCP 工具与连接需重路径确认`，行首两个空格）· `TestRenderProfileList_ZeroUnverifiedKeepsSummaryBytes`（未验证数 = 0 → 汇总行与 US-3 逐字节相同；> 0 → 追加 `/ 未验证 %d`，斜杠前一个空格） |

> **测试包归属（判据面事实）**：`internal/workspace/agent_test.go` 是 `package workspace_test`，`internal/app/wire_test.go` 是 `package app`，`internal/cli/profile_test.go` 是 `package cli`（包内测试，理由见该文件头注释）；`internal/tool` 同时存在 `package tool`（`runner_windows_test.go` / `httptools_test.go` / `shelltools_test.go`）与 `package tool_test`（`policy_test.go` / `filetools_test.go`）——`mcp_test.go` 必须落 **`package tool`**（要直接替换非导出 `dial` 字段、直接调非导出 `newMCPToolAdapter` / `listAllTools`），`mcp_integration_test.go` 可落 `package tool_test`（只用导出面 + `testutil`）。改动前先按目录实测复核。

### 3.3 配置与工程

| 项 | 结论 | 核对动作（module-dev 登记为核对任务，非改动任务） |
| --- | --- | --- |
| `go.mod` / `go.sum` | **+1 直接坐标**：`github.com/modelcontextprotocol/go-sdk v1.8.0`（栈表已批准，U-5）；传递依赖随 tidy 落地 | 写前 `GOPROXY=https://goproxy.cn go list -m -versions github.com/modelcontextprotocol/go-sdk`；写后 `git diff go.mod` 只多这一条直接依赖（间接依赖随 SDK）；`go mod tidy` 不得引入表外**直接**坐标 |
| `basalt.yaml.example` | **零改动** | MCP 配置在 `<workspace>/mcp_servers.yaml`（独立文件），不占 `basalt.yaml` 的键 |
| 新增配置键 | **零新增** | 30s 连接预算是代码常量；`shell.env_passthrough` 是既有键（复用，不新增） |
| `Makefile` / CI | **零改动** | 门禁七目标照跑；`test-integration` 已存在，本 US 首次让它有真实子进程用例；覆盖率底线不变（`tool` ≥ 70%、`core` ≥ 70%，其余 ≥ 50%——`scripts/check-coverage.sh` 头注释） |
| 迁移脚本 / `schema.sql` | **零迁移** | `git diff -- internal/storage/schema.sql` 为空；MCP 无持久化状态（会话只在内存） |
| `docs/Compatibility.md` | **增补 3 行**（§3 有意改变）：非 stdio → 拒绝启动（待决 #1 定稿后写）；非文本内容 → 类型化占位（注 3）；MCP 子进程环境 = 白名单（注 6） | 改动随 §3.5 改造点 ⑨ 送审；§2.1「文件内容零改动」在有**有效**配置时仍成立 |

### 3.4 表与列级口径

**本 US 零表改动、零列改动、零索引改动。** MCP 的配置在 `mcp_servers.yaml`（工作区文件），会话与工具清单只在内存；审计与消息继续落既有 `tool_invocations` / `llm_calls` / `messages` 三处（US-4 已交付的形状），**不新增列**——MCP 调用的 `tool` 列就是工具名，`error_kind` 取七值之一（`external_error` / `timeout` / `invalid_input`），与内置工具同一张表、同一套两阶段写。

| 项 | 结论 | 核对 |
| --- | --- | --- |
| `schema.sql` | 零 diff | `git diff -- internal/storage/schema.sql` 为空 |
| 迁移脚本 | 零新增 | `internal/storage/migrations/` 无新文件（已发布迁移不得改写） |
| 新表 / 新列 / 新索引 | 零 | 全文无 `CREATE TABLE` / `ALTER TABLE` |

### 3.5 前序改造点与后续占位

**前序改造点 9 项**（触及前序交付契约——**逐条属软门禁**，动手前与用户确认；其中 ①③④ 触及既有测试的判据面，⑨ 是上游文档同步）

| # | 改造点 | 前序契约面 | 影响既有测试 |
| --- | --- | --- | --- |
| 1 | `mcp_servers.yaml` 解析从"顶层键名集"改为"`servers:` 列表 + 全量校验"（`readMCPServerNames` 删除） | US-3 交付的规则 6 存在性半条（源码注释已预告"随 US-7"）；US-1 的模板注释 | `writeMCPServers` 夹具改写成列表形态；`TestAgentLoader_RejectsUnconfiguredMCPServer` 断言不变（`ghost-mcp` 仍不在配置里）——**夹具变更，判据不变**；`init_test.go` 零改动（`{}` 可解析为空映射） |
| 2 | `AgentLoaderOptions` 增 `MCPServers` / `MCPConnected` | US-4 的装载器显式依赖面（构造期克隆） | 既有调用点（`app/ports.go` / `wire.go`）不传 → nil；`ports.go` 的轻路径行为由第三态决定（待决 #2）；`agent_test.go` 的 `newTestLoader` / `newTestLoaderWithTools` 两夹具需扩 `MCPServers` / `MCPConnected` 两个参数或加变体（随已批准接口适配） |
| 3 | 规则 6 补「连接成功」半条；规则 4 用注册后名字集 | 契约 `agent-md.md` §2.1 规则 6；US-4 的判据面 | **判据面变更**：`TestAgentLoader_ParsesAllFrontmatterFields` 从"配置存在即注册"变为"已配置**且已连接**才注册"（夹具注入两名单）；新增 R4 守"配置有、连接没有 → 拒" |
| 4 | `AgentEntry` 增 `Unverified`；`cli/profile.go` 增 unverified 行与汇总后缀 | US-3 的 `AgentEntry` 与 `cli` 固定措辞（源码注释：措辞 = 固定口径） | **判据面变更**（待决 #2）：既有 `profile_test.go` 断言（`:163` / `:207`）在零 MCP 配置时**不变**；新增两条守第三态与零计数逐字节 |
| 5 | `tool/runner.go` 抽出 `mergeChildEnv`（`LocalRunner.childEnv` 改为调用） | US-5 的子进程环境实现 | **零判据变化**：既有 runner 用例（透传 / 覆盖 / 空清单）必须**原样全绿**——等价重构，不是新语义 |
| 6 | `app` 增 `mcpConnect` 测试夹层；`wire.go` 装配增 MCP 段 | US-6 完成态的装配序（①~⑦）与 `heavyRuntime` | 既有 `wire_test.go` 零改动（夹具工作区是 `{}` → 零 server → 整段跳过）；`heavyRuntime.Close` 的调用点行为扩为三步 |
| 7 | `templates.go` 模板注释修正 | US-1 模板 | 零测试改动（断言可解析性，非字节） |
| 8 | 测试夹具扩展：`agent_test.go` 的 `writeMCPServers` / `newTestLoader` / `newTestLoaderWithTools`；`wire_mcp_test.go` 的 `fakeMCPConnector` / `fakeMCPTool` / `writeMCPServersYAML` | US-3 / US-6 测试夹具 | 属"随已批准接口适配"；新增夹具不改既有断言 |
| 9 | **上游文档同步**：`docs/Compatibility.md` §3 增三行有意差异（非 stdio / 非文本渲染 / 子进程环境） | Compatibility 是配套工件，冲突以技术方案为准；差异**如实登记**而非静默 | 无 Go 测试改动；文档 diff 送审 |

**后续占位 6 项**（本 US **只留位不实现**）

| # | 占位 | 归属 |
| --- | --- | --- |
| 1 | SSE / Streamable HTTP transport（`transport` 取值域扩展） | 扩展阶段（待决 #1 若改判为 WARN-skip，仍不实现这两种 transport） |
| 2 | MCP resources / prompts / sampling | 扩展阶段；v1 只用 tools 一面 |
| 3 | 断线重连 / 健康检查 / 连接池重建 | 扩展阶段；v1 失败即跳过，重启才重连 |
| 4 | MCP server 的容器隔离与工具级 Policy | 扩展阶段（技术方案 §6.5：v1 信任边界 = 配置行） |
| 5 | 管理台 MCP 观测页与"测试连接" | US-12 |
| 6 | 工具名命名空间（`server.tool`）与 server 级启停开关 | 扩展阶段（注 9） |

## 4. 验收测试（Harness）

### 4.1 测试分层标准

- **默认层**（`make test`，进 `make check`）：**不访问真实网络、不打真实模型、不起真实子进程**。MCP 的默认层用 **SDK 的 `mcp.NewInMemoryTransports()`**（进程内管道，server 与 client 在同一测试进程）与 **假连接器**（`app` 层）——协议编解码是真的，进程边界是假的。连接器替身遵守一条纪律：`MCPConnected` 永远是**显式**的，缺省 nil 只在"测轻路径"时使用。
- **集成层**（`-tags=integration`）：**本仓第一个起真实子进程的集成层**（需求 §12.1 明示要求：核心链路有真实子进程协议测试，自研最小 server 钉住 SDK 行为）。实现 = **测试二进制自举**：父进程用 `os.Args[0] -test.run=^TestMCPHelperProcess$` 拉起自己，子进程在 `TestMCPHelperProcess` 里调 `testutil.RunMCPHelper`，以 `mcp.StdioTransport` 服务标准测试工具集。**路径含空白即跳过**（`t.Skip("测试二进制路径含空白——command 按空白切分，无法表达")`，与注 10 的已知边界一致）；跳过在收口报告里如实登记（不把"跳过"写成"通过"）。
- **并发面**：`MCPPool.Close` 幂等 / 会话列表的互斥、执行器两阶段写与适配器调用的并发由 `-race` 覆盖；`Connect` 本身串行，不做并发连接（已决 #3）。
- **覆盖率**：`tool`（新主体）≥ 70%、`core`（新增纯类型）≥ 70%；`workspace` / `app` / `cli` / `testutil` ≥ 50%——由 `make cover` 判卷。`testutil/mcpserver.go` 是**非测试包**，同样计入判卷，故配 `mcpserver_test.go` 自证（`NewInMemoryMCPPair` 被同包用例调用）。

### 4.2 测试类 → 验收点映射表

| 测试类（文件） | 覆盖的验收点 / 约束 | 依据（事实源） |
| --- | --- | --- |
| `workspace/mcp_test.go` | 文件形态与零 server 兼容（约束 1 / 注 1）· 八类结构校验（约束 2）· **env 占位精确形态 + 不回显值**（约束 3 / **R1a**）· 顺序保留（约束 1）· transport 取值域与**字段类型**（非字符串拒载；已决 #1 方案 A）· 未知字段忽略 | 需求 §5.13；Compatibility §2.1；技术方案 §6.3 / §8.8 |
| `tool/mcp_test.go` | 连接成功 / 失败跳过（约束 5 / **R2**）· 分页与重复游标（约束 10 / **R7**）· 会话保留与幂等关闭（约束 20 / **R10**）· 子进程 env 与 cwd、非 `CommandContext`（约束 17 / 18 / 27）· **IsError 固定文案 + server 文本不回显**（约束 13 / 28 / **R5**）· **协议错误安全摘要**（约束 14 / 28 / **R12** 后半）· **连接失败日志安全摘要**（约束 28 / **R12** 前半）· **ctx 身份优先**（约束 15 / **R6**）· **执行器两阶段审计**（约束 16 / **R9**）· 结果渲染 / 占位 / 兜底（约束 12）· 入参原样透传与大整数精度（约束 11 / **R13**）· **描述与 schema 保真**（**SC-001**：SDK 列工具 → 适配器 → 注册池 → `Specs`）· **握手期取消 / 截止为池级错误**（约束 5；预算超时仍跳过） | 技术方案 §6.5 / §1.1；契约 `run-and-errors.md` §7.3；`errors.go` 文案纪律 |
| `tool/mcp_integration_test.go` | **真实子进程协议链路**（需求 §12.1）· **预算释放后进程仍存活可调用**（约束 27 / **R11**）· 坏 command 跳过（约束 5 / **R2** 真进程版）· 分页真链路（**R7**）· server 中途退出 → `external_error`（约束 14） | 需求 §12.1 / §13.1；技术方案 §6.3 |
| `app/wire_mcp_test.go` | **门禁先于 MCP 注册**（约束 9 / **R3**）· 失败 server 的 Agent 拒载（约束 6 / **R3**）· 重名 keep-first（约束 8 / **R8**）· 摘要 `mcp_connected`（约束 23）· 连接取消的装配失败与池关闭（约束 5）· **停机顺序**（约束 20 / **R10**）· **配置修改需重启**（FR-011 不热加载） | 技术方案 §6.4 / §13；需求 §9.1；Operations §5.3 |
| `app/mcp_test.go` | **env 装配期解析 fail-closed**（约束 4 / 口径 3 / **R1b**）· 未设置与空值同档、点名不吐值 | 技术方案 §8.8；需求 §5.13 |
| `workspace/agent_test.go`（追加） | 连接成功才注册（**R4** 对照组）· **配置有连接无 → 拒**（约束 6 / **R4**）· 轻路径第三态（约束 22 / 待决 #2）· 零配置两态不变 | 契约 §2.1 规则 4 / 6；Compatibility §2.4 |
| `cli/profile_test.go`（追加） | unverified 行逐字（待决 #2）· 零计数汇总逐字节不变 | US-3 固定措辞；本 US §6.2 口径 9 |
| `testutil/mcpserver_test.go` | 测试基建自证（标准工具集可列出）· 子进程入口未设环境时**跳过带理由** | `internal/testutil/testutil_test.go` 先例；陷阱 55 |

### 4.3 关键回归测试写出代码

以下 R1a / R1b / R2~R13 共 **14 条关键回归、18 个测试函数**（R3 / R7 / R10 / R12 各含两条）——**必须**成为独立任务且原样落地；每条标注**逆改点**（实现怎么改它会红）——写不出逆改点的用例按"对号入座但无判别力"处理。夹具按 US-4/US-5/US-6 既有形状（`wireFixture` / `newTestLoader`）。**测试缝纪律**：app 层用例经 `fx.app()` 取 `*App` 后直接设置包内字段 `mcpConnect`，tool 层白盒用例直接替换非导出 `dial`——**不得**为此新增导出测试 API（`SetDialForTest` / `NewMCPToolAdapterForTest` 一类一律不落地）。

**本 US 新增的测试辅助件**（落在 §3.2 对应文件内；下方直接引用，不再重复定义）：

| 辅助件 | 落位 | 职责 |
| --- | --- | --- |
| `MCPTestTool` / `NewInMemoryMCPPair` / `StandardMCPTools` | `testutil/mcpserver.go` | in-memory server 与 client 传输的配对夹具；标准工具集 = `echo`（回显 `text` 参数）/ `boom`（`IsError` + 固定文本）/ `blob`（图片内容块，判非文本渲染）。server **先连**、client 后连（SDK `NewInMemoryTransports` 的对称要求） |
| `fakeMCPConnector` | `app/wire_mcp_test.go` | 实现 `mcpConnector`：记录 `specs`、返回构造好的 `[]tool.MCPConnection` 或注入错误、`Close` 记录调用并可选执行回调（`onClose`）——R3 / R8 / R10 的判别力来源 |
| `fakeMCPTool` | `app/wire_mcp_test.go` | `core.Tool` 替身：`*fakeMCPTool{name, content string}`（**指针类型**，方法用指针接收者）；`Execute` 返回固定 `content`。R8 用类型断言 `got.(*fakeMCPTool)` 判"谁赢了"——**不执行**内置工具（空 `ToolEnv` 下执行会被沙箱合法拒绝，测的就不是注册语义了） |
| `writeMCPServersYAML(t, root, names)` | `app/wire_mcp_test.go` | 在工作区根写 `servers:` 列表形态的最小配置（app 测试不能复用 `workspace_test` 的包内夹具） |
| `fakeToolLister` | `tool/mcp_test.go` | 实现 `toolLister`：按页返回 `ListToolsResult`（R7 后半的重复游标由它构造） |
| `captureLogger(t)` | `tool/mcp_test.go`（沿 US-6 同名约定） | 内存缓冲 `*slog.Logger` + 取文本方法（R2 判 WARN 半轨） |
| `connectTestSession(t, transport)` | `tool/mcp_test.go` | 以 `mcp.NewClient(&mcp.Implementation{Name:"basalt-test"}, nil)` 连上给定传输并注册 `t.Cleanup` 关闭（`package tool` 白盒用例共用） |
| `testAdapter(session, server, name)` | `tool/mcp_test.go` | 用最小 `*mcp.Tool{Name: name, InputSchema: map[string]any{"type":"object"}}` 调**非导出** `newMCPToolAdapter`（不新增导出测试缝） |
| `recordingAudit` | `tool/mcp_test.go` | 实现 `core.ToolInvocationStore` 的记录型假件（与 `core/tool_exec_test.go` 的 `execAudit` 同型）：`BeginInvocation` 追加一行 `pending`，`FinishInvocation` 回填同一行；配 `count()` / `lastStatus()` 两个观察方法——R9 的观察面 |
| `helperCommand(t)` | `tool/mcp_integration_test.go` | 组 `os.Args[0] + " -test.run=^TestMCPHelperProcess$"` 命令串（子进程环境另经 `spec.Env` 传 `BASALT_MCP_HELPER=1`）；`os.Args[0]` 含空白 → `t.Skip("测试二进制路径含空白——command 按空白切分，无法表达")`（注 10） |

**测试包归属**：`tool/mcp_test.go` = `package tool`（可用非导出 `dial` / `listAllTools`）；`tool/mcp_integration_test.go` = `package tool_test`；`app/wire_mcp_test.go` = `package app`；`workspace/mcp_test.go` = `package workspace_test`。

**并发用例的纪律**：`require` 一律在**主 goroutine**（Wait / 通道收尾之后）断言——子 goroutine 里只收集 error。

#### R1a — env 明文与错误形态在解析期即拒，且不回显值（workspace 层）

```go
// R1a —— env 值只允许精确 ${VAR}；明文 / 半截占位拒载；错误文案不回显配置值。
// 逆改点：把检查放宽成 HasPrefix("${") && HasSuffix("}")
//         → 第二组（${A}${B}）由拒转收，红；
//        把 val 拼进错误文案 → 第三组 NotContains 红（明文凭证进日志即泄密）。
func TestParseMCPServers_RejectsPlaintextEnvWithoutEchoingValue(t *testing.T) {
    root := t.TempDir()
    secret := "ghp_plaintext_must_not_leak"
    writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: gh
    command: gh-mcp
    env:
      GITHUB_TOKEN: `+secret+`
`)
    _, err := workspace.ParseMCPServers(root)
    require.Error(t, err)
    require.Contains(t, err.Error(), "gh", "错误必须点名 server")
    require.Contains(t, err.Error(), "env.GITHUB_TOKEN", "错误必须点名字段")
    require.NotContains(t, err.Error(), secret, "错误文案不得回显配置值")

    writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: gh
    command: gh-mcp
    env:
      TOKEN: ${A}${B}
`)
    _, err = workspace.ParseMCPServers(root)
    require.Error(t, err, "非精确 ${VAR} 形态必须拒（无 HasPrefix/HasSuffix 放行口）")
    require.NotContains(t, err.Error(), "${A}${B}", "占位原文同样不回显")
}
```

#### R1b — env 未设置或为空 = 拒绝启动，点名变量（app 层）

```go
// R1b —— 装配期解析：未设置 / 空值同档拒绝；错误点名 server 与变量，不进入值。
// 逆改点：把 !ok 改成 value := os.Getenv(name) 继续装配
//         → 两组 require.Error 都红（空凭证被送给 server）。
func TestResolveMCPEnv_MissingOrEmptyFailsClosed(t *testing.T) {
    specs := []core.MCPServerConfig{{
        Name: "gh", Command: "gh-mcp",
        Env: map[string]string{"GITHUB_TOKEN": "${GH_TOKEN}"},
    }}
    _, err := resolveMCPEnv(specs, func(string) (string, bool) { return "", false })
    require.Error(t, err)
    require.Contains(t, err.Error(), "gh")
    require.Contains(t, err.Error(), "GH_TOKEN")

    _, err = resolveMCPEnv(specs, func(string) (string, bool) { return "", true })
    require.Error(t, err, "空值与未设置同档（空凭证不可用）")

    resolved, err := resolveMCPEnv(specs, func(string) (string, bool) { return "tok-1", true })
    require.NoError(t, err)
    require.Equal(t, "tok-1", resolved[0].Env["GITHUB_TOKEN"])
}
```

#### R2 — 单 server 连接失败 = WARN + 跳过，其余照常（tool 层）

```go
// R2 —— 失败隔离：坏 server 不产生池级错误，好 server 的工具照常返回。
// 逆改点：把 per-server 失败改成 return nil, err → conns 断言红；
//        把失败静默吞掉（无 WARN）→ logs 断言红（可观测性双轨）。
func TestMCPPool_ConnectSkipsFailedServerAndKeepsOthers(t *testing.T) {
    logger, logs := captureLogger(t)
    pool := NewMCPPool(MCPPoolOptions{Logger: logger, Root: t.TempDir()})
    transport := testutil.NewInMemoryMCPPair(t, 0, testutil.StandardMCPTools()...)
    pool.dial = func(spec core.MCPServerConfig) (mcp.Transport, error) { // 无 ctx：进程不绑预算（约束 27）
        if spec.Name == "broken" {
            return nil, errors.New("exec: \"basalt-mcp-no-such-binary\": executable file not found")
        }
        return transport, nil
    }

    conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{
        {Name: "broken", Command: "basalt-mcp-no-such-binary"},
        {Name: "healthy", Command: "unused"},
    })
    require.NoError(t, err, "单 server 失败不是池级错误")
    require.Len(t, conns, 1)
    require.Equal(t, "healthy", conns[0].Server)
    require.ElementsMatch(t, []string{"blob", "boom", "echo"},
        []string{conns[0].Tools[0].Name(), conns[0].Tools[1].Name(), conns[0].Tools[2].Name()})

    require.Contains(t, logs.String(), "level=WARN")
    require.Contains(t, logs.String(), "broken")
    require.Contains(t, logs.String(), "MCP server 未连接——已跳过")
}
```

#### R3 — 失败 server 的工具不进池，依赖它的 Agent 整只不注册（app 层）

```go
// R3 —— 装配面：门禁先于 MCP 注册（装配成功即证），失败 server 的 Agent 拒载且点名。
// 逆改点：把 MCP 工具注册挪到等值门禁之前 → newRuntime 直接失败，第一步红；
//        装载器忘了注入 MCPConnected（传 nil）→ 失败 server 的 Agent 变成 unverified，
//        日志里没有 "tool not registered"，第二步红。
func TestWire_AgentDeclaringFailedMCPToolIsNotRegistered(t *testing.T) {
    fx := newWireFixture(t)
    writeMCPServersYAML(t, fx.root, []string{"broken"}) // servers: [{name: broken, command: ...}]
    fx.writeAgent(t, "scribe", "broken_probe")          // 声明一个只可能来自 broken 的工具
    connector := &fakeMCPConnector{conns: nil}          // broken 未连上 → 零连接

    app := fx.app()
    app.mcpConnect = connector
    rt, err := app.newRuntime(context.Background())
    require.NoError(t, err, "连接失败不得中断启动")
    t.Cleanup(func() { require.NoError(t, rt.Close()) })

    require.NotContains(t, rt.registry.Names(), "broken_probe")
    require.Contains(t, fx.logs.String(), `"reason":"tool not registered: broken_probe"`)
    require.Contains(t, fx.logs.String(), "Agent 未注册")
}

func TestWire_MCPToolsRegisteredAfterEqualityGate(t *testing.T) {
    fx := newWireFixture(t)
    writeMCPServersYAML(t, fx.root, []string{"search"})
    connector := &fakeMCPConnector{conns: []tool.MCPConnection{{
        Server: "search",
        Tools:  []core.Tool{&fakeMCPTool{name: "search_docs", content: "hit"}},
    }}}
    app := fx.app()
    app.mcpConnect = connector
    rt, err := app.newRuntime(context.Background())
    require.NoError(t, err, "等值门禁必须只比对八件内置工具；MCP 注册在门禁之后")
    t.Cleanup(func() { require.NoError(t, rt.Close()) })
    require.Contains(t, rt.registry.Names(), "search_docs")
}
```

#### R4 — 配置有、连接没有 → `mcp server skipped`（workspace 层）

```go
// R4 —— 规则 6 的「连接成功」半条：存在但未连接的 server 与不存在的 server 同判。
// 逆改点：只查 mcpNames 不查 mcpConnected → 第一组由拒转收，红；
//        守卫"接线忘了传连接结果"：传 nil 会走 unverified 分支，第二组红。
func TestAgentLoader_ConfiguredButNotConnectedRejects(t *testing.T) {
    root := t.TempDir()
    writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: search
    command: search-mcp
`)
    writeAgentFrontmatter(t, root, "scout", `
name: scout
provider: {name: probe, model: probe-model}
mcp_servers: [search]
`, "")

    entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
        Providers:    []string{"probe"},
        MCPServers:   []string{"search"},
        MCPConnected: []string{}, // 已配置、未连上
    }).LoadAll(root)
    require.NoError(t, err)
    require.False(t, entries[0].Registered)
    require.False(t, entries[0].Unverified)
    require.Contains(t, entries[0].RejectReason, "mcp server skipped")
    require.Contains(t, entries[0].RejectReason, "search", "detail 点名 server")
}
```

#### R5 — `IsError` → `external_error`，server 文本整段丢弃（tool 层，敏感值负向）

```go
// R5 —— MCP 侧报错不是成功；且 server 文本是外部正文，不得进 Reason / Cause / 日志。
// 逆改点：忽略 res.IsError 直接渲染内容 → Error 断言红（调用"成功"了）；
//        把 server 文本（单行化 / 截断后）放进 Reason → NotContains(secret) 红；
//        把文本挂进 Cause → Cause == nil 断言红（Cause 也会经 %+v / 排障通道外泄）。
func TestMCPToolAdapter_MapsIsErrorToExternalError(t *testing.T) {
    const secret = "ghp_iserror_must_not_leak"
    leaky := testutil.MCPTestTool{
        Name: "boom", Description: "IsError + 敏感值",
        Schema: map[string]any{"type": "object"},
        Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
            return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
                &mcp.TextContent{Text: "server-side failure token=" + secret},
            }}, nil
        },
    }
    transport := testutil.NewInMemoryMCPPair(t, 0, leaky)
    session := connectTestSession(t, transport)
    adapter := testAdapter(session, "probe", "boom")

    _, err := adapter.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{}`))
    require.Error(t, err)
    require.True(t, core.IsExternalError(err))
    require.Equal(t, "MCP 工具返回错误：probe/boom", err.Error(), "Reason 必须是自产固定文案")
    require.NotContains(t, err.Error(), secret, "server 文本不得回显")
    require.NotContains(t, err.Error(), "server-side failure", "整段外部正文都不回显")
    var ee *core.ExternalError
    require.ErrorAs(t, err, &ee)
    require.Nil(t, ee.Cause, "IsError 的 server 文本连 Cause 也不放")
    require.False(t, core.IsInvalidInput(err))
}
```

#### R6 — 取消 / 截止的身份优先于外部错误（tool 层）

```go
// R6 —— ctx 取消 → 返回 ctx 原错误（→ timeout），不得包成 ExternalError。
// 用「进入适配器前已取消」的 ctx：结果确定（SDK 在发送前就查 ctx.Done），不赌 echo 的耗时。
// 逆改点：把所有 err 统一包成 &ExternalError{Cause: err}
//         → errors.Is 仍真（Unwrap 穿透），但 !IsExternalError 断言红——
//        这个断言守的正是「取消的身份优先于错误来自哪个系统」。
func TestMCPToolAdapter_ContextIdentityWinsOverExternalError(t *testing.T) {
    transport := testutil.NewInMemoryMCPPair(t, 0, testutil.StandardMCPTools()...)
    session := connectTestSession(t, transport)
    adapter := testAdapter(session, "probe", "echo")

    ctx, cancel := context.WithCancel(context.Background())
    cancel()
    _, err := adapter.Execute(ctx, core.ToolEnv{}, json.RawMessage(`{"text":"late"}`))
    require.Error(t, err)
    require.True(t, errors.Is(err, context.Canceled))
    require.False(t, core.IsExternalError(err), "ctx 身份必须保留为裸 sentinel")
}
```

#### R7 — 分页到空游标；重复游标即失败（tool 层）

```go
// R7 —— 两个面：真分页全列出；异常游标不死循环。
// 逆改点：只取第一页 → 第一组工具数断言红（PageSize=1 只剩 1 件）；
//        删掉 seen 守卫 → 第二组不返回（用例超时判红）。
func TestMCPPool_ListToolsPaginatesToEmptyCursor(t *testing.T) {
    transport := testutil.NewInMemoryMCPPair(t, 1, testutil.StandardMCPTools()...) // 每页 1 件
    pool := NewMCPPool(MCPPoolOptions{Logger: slog.New(slog.DiscardHandler)})
    pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
        return transport, nil
    }
    conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{Name: "paged", Command: "unused"}})
    require.NoError(t, err)
    require.Len(t, conns[0].Tools, 3, "三页必须全部列出")
}

func TestListAllTools_RepeatedCursorFails(t *testing.T) {
    lister := &fakeToolLister{pages: []*mcp.ListToolsResult{
        {NextCursor: "c1", Tools: []*mcp.Tool{{Name: "a"}}},
        {NextCursor: "c1", Tools: []*mcp.Tool{{Name: "b"}}}, // 游标不前进
    }}
    _, err := listAllTools(context.Background(), lister)
    require.Error(t, err)
    require.Contains(t, err.Error(), "游标重复")
}
```

#### R8 — 重名 keep-first：内置优先，其余按文件序（app 层）

```go
// R8 —— 注册池的重名语义在 MCP 接入后仍成立，且可观测到"谁赢了"。
// 判别方式 = 身份断言（类型断言），不执行内置工具：空 ToolEnv 下 read_file 读不存在的 x
// 会被沙箱**合法拒绝**——那测到的是沙箱，不是"重名谁赢"。
// 逆改点：改成后注册覆盖 → got 变成 *fakeMCPTool，isFake 断言红；
//        改成"重名即拒绝注册整个 MCP server" → Names 断言红（工具变少）。
func TestWire_DuplicateMCPToolKeepsBuiltin(t *testing.T) {
    fx := newWireFixture(t)
    writeMCPServersYAML(t, fx.root, []string{"shadow"})
    connector := &fakeMCPConnector{conns: []tool.MCPConnection{{
        Server: "shadow",
        Tools: []core.Tool{
            &fakeMCPTool{name: "read_file", content: "shadowed"},
            &fakeMCPTool{name: "search_docs", content: "hit"},
        },
    }}}
    app := fx.app()
    app.mcpConnect = connector
    rt, err := app.newRuntime(context.Background())
    require.NoError(t, err)
    t.Cleanup(func() { require.NoError(t, rt.Close()) })

    got, ok := rt.registry.Resolve("read_file")
    require.True(t, ok)
    require.Equal(t, "read_file", got.Name())
    _, isFake := got.(*fakeMCPTool)
    require.False(t, isFake, "内置 read_file 必须保留（keep-first）——MCP 替身赢了这里就红")
    require.Contains(t, rt.registry.Names(), "search_docs", "其余 MCP 工具照常注册")
    require.Contains(t, fx.logs.String(), "拒绝重复注册的工具")
}
```

#### R9 — MCP 调用经执行器两阶段审计（core + tool 联测）

```go
// R9 —— 宪法 V 在 MCP 工具上不打折：执行前 pending 已落审计存储（生产 = SQLite 仓储），
//        执行后同一行回填终态。**所有等待都有界**：断言失败时用例给出可读原因，不挂死。
// 逆改点：适配器自己执行、绕过 ToolExecutor（例如循环直接调 adapter）
//         → 阻塞期间的 pending 查询红（没有行）；
//        执行后再插入第二行 → 终态行数断言红（一次调用两行）。
func TestMCPToolAdapter_CallIsAuditedBeforeExecute(t *testing.T) {
    entered := make(chan struct{})
    release := make(chan struct{})
    var releaseOnce sync.Once
    unblock := func() { releaseOnce.Do(func() { close(release) }) }
    t.Cleanup(unblock) // 失败路径也放行 handler：会话关闭不被阻塞的调用拖住

    blocking := testutil.MCPTestTool{
        Name: "slow", Description: "阻塞到放行",
        Schema: map[string]any{"type": "object"},
        Handler: func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
            close(entered)
            select {
            case <-release:
            case <-ctx.Done(): // 调用被取消 / 截止时同样退出（否则 handler 永远挂着）
                return nil, ctx.Err()
            }
            return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
        },
    }
    transport := testutil.NewInMemoryMCPPair(t, 0, blocking)
    adapter := testAdapter(connectTestSession(t, transport), "probe", "slow")

    audit := &recordingAudit{} // 与 core/tool_exec_test.go 的 execAudit 同型
    reg := NewRegistry(slog.New(slog.DiscardHandler))
    require.NoError(t, reg.Register(adapter))
    exec := core.NewToolExecutor(reg, audit, slog.New(slog.DiscardHandler))
    done := make(chan core.Result, 1) // 有缓冲：主 goroutine 提前失败时发送方不阻塞
    go func() {
        done <- exec.Execute(context.Background(),
            core.ToolEnv{RunID: "run-1", SessionID: "ses-1", Timeout: 5 * time.Second},
            []string{"slow"}, core.ToolCall{Name: "slow", Arguments: `{}`})
    }()

    select {
    case <-entered:
    case <-time.After(5 * time.Second):
        t.Fatal("工具调用未在预算内进入 server 处理函数")
    }
    require.Equal(t, 1, audit.count(), "执行期间审计行已存在")
    require.Equal(t, core.LLMCallPending, audit.lastStatus(),
        "工具执行期间审计行必须是 pending（fail-closed 的可见形态）")

    unblock()
    select {
    case res := <-done:
        require.Equal(t, "done", res.Content)
    case <-time.After(5 * time.Second):
        t.Fatal("工具调用未在预算内返回")
    }
    require.Equal(t, 1, audit.count(), "一次调用一行")
    require.Equal(t, core.LLMCallSuccess, audit.lastStatus(), "同一行回填终态")
}
```

#### R10 — 池关闭幂等；停机顺序 MCP → 库 → 锁（tool + app 层）

```go
// R10a —— Close 幂等，且**确实关闭了会话**（tool 层）。
// 逆改点：Close 完全不关闭会话（直接 return nil）→ 限定时间内 Wait() 不返回，第三条断言红。
// 注：SDK 自身的 ClientSession.Close 是幂等的，因此"去掉会话清空 → 第二次 Close 红"不成立；
//     幂等断言保留，但判别力由"关闭可观察"承担。
func TestMCPPool_CloseIsIdempotent(t *testing.T) {
    transport := testutil.NewInMemoryMCPPair(t, 0, testutil.StandardMCPTools()...)
    pool := NewMCPPool(MCPPoolOptions{Logger: slog.New(slog.DiscardHandler)})
    pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
        return transport, nil
    }
    conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{Name: "s", Command: "unused"}})
    require.NoError(t, err)
    require.Len(t, conns, 1, "连接成功是本用例的前提")

    pool.mu.Lock()
    sessions := slices.Clone(pool.sessions)
    pool.mu.Unlock()
    require.Len(t, sessions, 1, "池必须持有已连接会话")

    require.NoError(t, pool.Close())
    require.NoError(t, pool.Close(), "重复关闭必须无错")

    closed := make(chan error, 1)
    go func() { closed <- sessions[0].Wait() }()
    select {
    case <-closed:
    case <-time.After(5 * time.Second):
        t.Fatal("Close 后会话未在限定时间内结束——Close 没有真正关闭会话")
    }
}

// R10b —— 停机顺序（app 层）：池先于库关闭。
// 前提：工作区必须有一条 MCP 配置——零 server 时装配不建池，Close 自然不会碰连接器。
// 库存活性用真实 API 观察：rt.db.SchemaVersion(ctx)（storage.DB 没有 PingContext）。
// 逆改点：把 Close 顺序改成「库 → 池」→ 连接器 Close 里 SchemaVersion 返回错误，
//        rt.Close() 经 errors.Join 上抛，NoError 断言红；忘了关池 → closed 断言红。
func TestWire_CloseClosesMCPBeforeDatabase(t *testing.T) {
    fx := newWireFixture(t)
    writeMCPServersYAML(t, fx.root, []string{"probe"}) // 建池前提：至少一个 server
    var rt *heavyRuntime
    connector := &fakeMCPConnector{onClose: func() error {
        _, err := rt.db.SchemaVersion(context.Background()) // 库还活着 = 顺序正确
        return err
    }}
    app := fx.app()
    app.mcpConnect = connector
    got, err := app.newRuntime(context.Background())
    require.NoError(t, err)
    rt = got
    require.NoError(t, rt.Close())
    require.True(t, connector.closed, "heavyRuntime.Close 必须先关闭 MCP 池")
}
```

#### R11 — 预算释放后 server 子进程仍存活、工具仍可调用（集成层，真实子进程）

```go
// R11 —— 进程生命周期与握手 / 列工具预算分离。
// 逆改点：dialCommand 改用 exec.CommandContext(握手 ctx)（或把 cctx 存进会话）
//         → Connect 末尾 cancel() 即杀子进程，等待后的调用报连接关闭，红。
func TestMCPIntegration_CallSucceedsAfterConnectBudgetReleased(t *testing.T) {
    var logs bytes.Buffer
    pool := tool.NewMCPPool(tool.MCPPoolOptions{
        Logger:         slog.New(slog.NewTextHandler(&logs, nil)),
        Root:           t.TempDir(),
        EnvPassthrough: []string{"PATH", "SYSTEMROOT", "TEMP", "TMP", "HOME"},
        ConnectTimeout: 5 * time.Second,
        Version:        "us7-test",
    })
    conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{
        Name: "helper", Command: helperCommand(t),
        Env: map[string]string{"BASALT_MCP_HELPER": "1"}, // 已解析形态：值直接传给子进程
    }})
    require.NoError(t, err)
    require.Len(t, conns, 1, "helper 子进程必须连上：%s", logs.String())
    t.Cleanup(func() { require.NoError(t, pool.Close()) })

    // 预算 ctx 已在 Connect 末尾 cancel()；进程若被预算绑死，这里已经死透。
    time.Sleep(300 * time.Millisecond)
    var echo core.Tool
    for _, tl := range conns[0].Tools {
        if tl.Name() == "echo" {
            echo = tl
        }
    }
    require.NotNil(t, echo)
    res, err := echo.Execute(context.Background(), core.ToolEnv{Timeout: 5 * time.Second},
        json.RawMessage(`{"text":"after-budget"}`))
    require.NoError(t, err, "Connect 返回后的调用不得因预算释放而失败")
    require.Contains(t, res.Content, "after-budget")
}
```

#### R12 — 敏感值负向：连接失败日志与协议错误原文都不回显（tool 层）

```go
// R12 前半 —— 连接失败日志只含安全摘要；命令名里的敏感值不进日志。
// 逆改点：把 detail 从 safeErrorSummary(err) 换回 err.Error() → NotContains(secret) 红（单行化与截断都不算脱敏）。
func TestMCPPool_ConnectFailureLogOmitsSensitiveDetail(t *testing.T) {
    const secret = "ghp_dial_must_not_leak"
    logger, logs := captureLogger(t)
    pool := NewMCPPool(MCPPoolOptions{Logger: logger, Root: t.TempDir()})
    pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
        return nil, &exec.Error{Name: "mcp-" + secret, Err: exec.ErrNotFound}
    }
    conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{Name: "gh", Command: "unused"}})
    require.NoError(t, err)
    require.Empty(t, conns)
    require.Contains(t, logs.String(), "可执行文件不存在", "detail 必须是固定安全短语")
    require.NotContains(t, logs.String(), secret, "命令名里的敏感值不得进日志")
    require.NotContains(t, logs.String(), "exec:", "exec 原文不得进日志")
}

// R12 后半 —— 协议错误（server 以 JSON-RPC 错误答复）→ Reason 固定文案；
// server 错误消息里的敏感值不得出现在 err.Error()。
// 逆改点：把 err.Error() 拼进 Reason → NotContains(secret) 红；
//        丢掉 Cause → Error(t, ee.Cause) 红（进程内排障通道被切断）。
func TestMCPToolAdapter_ProtocolErrorOmitsSensitiveDetail(t *testing.T) {
    const secret = "ghp_protocol_must_not_leak"
    failing := testutil.MCPTestTool{
        Name: "leak", Description: "handler 返回错误",
        Schema: map[string]any{"type": "object"},
        Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
            return nil, errors.New("server exploded: token=" + secret)
        },
    }
    transport := testutil.NewInMemoryMCPPair(t, 0, failing)
    adapter := testAdapter(connectTestSession(t, transport), "probe", "leak")

    _, err := adapter.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{}`))
    require.Error(t, err)
    require.True(t, core.IsExternalError(err))
    require.Equal(t, "MCP 调用失败：probe/leak", err.Error(), "Reason 必须是自产固定文案")
    require.NotContains(t, err.Error(), secret, "协议错误原文不得回显")
    var ee *core.ExternalError
    require.ErrorAs(t, err, &ee)
    require.Error(t, ee.Cause, "原始错误保留在 Cause（只经 Unwrap，不进文案）")
}
```

#### R13 — 入参原样透传，大整数不丢精度（tool 层）

```go
// R13 —— 大于 2^53 的整数不得经 float64 舍入。
// 逆改点：mcpCallArguments 改为 json.Unmarshal 到 map[string]any
//         → 序列化输出 9007199254740992，NotContains 与 Contains 双双红。
func TestMCPCallArguments_PreservesLargeIntegers(t *testing.T) {
    args, err := mcpCallArguments(json.RawMessage(`{"n":9007199254740993}`))
    require.NoError(t, err)
    wire, err := json.Marshal(&mcp.CallToolParams{Name: "probe", Arguments: args})
    require.NoError(t, err)
    require.Contains(t, string(wire), "9007199254740993", "原始字节必须透传")
    require.NotContains(t, string(wire), "9007199254740992", "不得出现 float64 舍入后的值")

    empty, err := mcpCallArguments(json.RawMessage(`null`))
    require.NoError(t, err)
    require.Equal(t, map[string]any{}, empty, "null → 空对象，不传 nil")
}
```

## 5. 范围边界

### 5.1 核心阶段不做项（依据：需求文档 §6 / 技术方案"核心阶段不做"）

| 不做项 | 依据 |
| --- | --- |
| SSE / Streamable HTTP transport | 技术方案 §6.3 零代码档以 stdio 为主；扩展阶段 |
| 对不可信 MCP server 的容器化隔离 | 技术方案 §6.5「v1 定位：经管理员评审的脚本与 MCP server；对不可信 MCP server 的隔离（容器化）属扩展阶段」 |
| MCP server 内部行为的工具级 Policy | 技术方案 §6.5「白名单管的是 Basalt 自己的工具，管不了 MCP server 内部做什么」——**不是遗漏，是有意接受的边界** |
| MCP resources / prompts / sampling | 扩展阶段；v1 只用 tools 一面 |
| MCP 工具调用审核 / 内容过滤 | 企业级治理归扩展阶段（宪法 X） |

### 5.2 本 US 不做（归后续）

| 不做项 | 归属 |
| --- | --- |
| 断线重连 / 健康检查 / 连接池重建 | 扩展阶段；v1 失败即跳过、重启才重连 |
| `mcp_servers.yaml` 热加载 | 技术方案 §11.5 / Compatibility §2.4：**需重启** |
| 管理台 MCP 观测页与"测试连接" | US-12 |
| 工具名命名空间（`server.tool`） | 扩展阶段（注 9）；v1 原样名 + keep-first |
| MCP server 级启停开关 / 优先级配置 | 扩展阶段；v1 以文件顺序为唯一优先级 |
| 工具级超时覆盖（每 server 单独设超时） | 零新增配置键（约束 26）；单工具超时沿用 `ToolTimeout` 预算 |
| 非文本内容的二进制落盘 / 附件引用 | 扩展阶段；v1 只渲染类型化占位符（注 3） |

## 6. 验证与验收

### 6.1 剩余人工项

1. **门禁七目标与覆盖率读数**：`make check` 七个目标逐条读数（`fmt` / `lint` / `sec` / `test` / `test-integration` / `cover` / `build`）；`tool` / `core` 覆盖率实测数值写入 DoD。
2. **Demo 三（零代码接入）**：真模型 + 一个现成 MCP server（如文件系统或 git 类 server）——只写 `AGENT.md` 与 `mcp_servers.yaml`，**全程不改代码**；让 Agent 完成一次 PR digest（或选定的真实小任务），核对：工具出现在 `basalt tool list`、模型确实调用了它、`tool_invocations` 有对应行（`status=succeeded`）、启动摘要 `mcp_connected` = 1。
3. **负向 Demo（故意写错 `command`）**：把某个 server 的 `command` 改成不存在的可执行文件 → 启动**不中断**；日志出现 `MCP server 未连接——已跳过`（点名 server）；依赖它的 Agent 出现 `Agent 未注册`（`tool not registered` 或 `mcp server skipped`）；其余 Agent 正常可聊。
4. **两档展示面的一致性抽验**：重路径（`chat`）与轻路径（`profile list`）对同一份工作区——零 MCP 配置时读数不变（US-3 基线）；有 MCP 配置且未验证时 `profile list` 显示 `registered=unverified`（待决 #2 定稿后核对）。
5. **`tool list` 冒烟**：八件内置 + N 件 MCP 工具；MCP 工具的描述与参数 JSON 与 server 声明一致（抽一件比对）。
6. **`env` 占位与明文负向**：`mcp_servers.yaml` 写明文 token → 启动拒绝，错误与日志**不含该值**；写 `${MISSING_VAR}` → 拒绝启动，错误点名 server 与变量名。
7. **停机子进程检查**：`serve` / `chat` 退出后，用任务管理器 / `ps` 确认 MCP server 子进程**不残留**（Windows 任务管理器 + `tasklist` 双读数）；池关闭错误若有，日志与退出码如实反映。
8. **SDK 升级前置**：记录集成层（真实子进程）读数为基线，**含 R11 生命周期用例**（预算释放后仍可调用）；将来升级 `go-sdk` 先跑该测试（需求 §12.1 的缓解措施）——若新版开始把上下文取消传播到传输，R11 立即红。
9. **Windows 真控制台**：中文工具描述 / 错误文案的渲染（无 BOM、多行不交错）；`command` 含中文路径时按空白切分的实际行为如实记录（已知边界，注 10）。
10. **凭证与敏感面复核**：日志与错误不打印 `env` 值、不打印 server stderr 全文、不打印工具结果正文、不打印 `IsError` 的 server 文本与协议错误原文（只打 `server` / `reason` / `detail` 安全短语 / 长度）；抽查 `tool_invocations.error_message` 与回填模型的 content，确认与 `ExternalError.Reason` 一致（无外部正文）；`mcp_servers.yaml` 在仓库外或已脱敏。
11. **变更范围核对**：`git status --short` 与 §3.1 / §3.2 清单逐项对齐；`schema.sql` / `basalt.yaml.example` / `Makefile` 零 diff；`go.mod` 只多 `go-sdk` 一条直接依赖。
12. **四目标编译矩阵**（不只 `make build` 宿主档）：

    ```sh
    for target in linux/amd64 linux/arm64 windows/amd64 darwin/arm64; do
      os=${target%/*}; arch=${target#*/}
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X main.version=us7check" -o bin/basalt-$os-$arch$([ $os = windows ] && echo .exe) ./cmd/basalt
    done
    ```

13. **并发抽验**：并发触发多个含 MCP 工具的会话 → `-race` 无告警；`Close` 与在途调用交错时不 panic（人工复核 + `-race` 读数）。

### 6.2 本文固定口径（编号冻结，只追加）

| # | 口径 | 依据 / 备注 |
| --- | --- | --- |
| 1 | `mcp_servers.yaml` = 顶层映射 + `servers:` 列表；元素字段 `name` / `transport` / `command` / `env`；缺失 / 空 / 纯注释 / `{}` / 无 `servers` / `servers: null` → **零 server**；列表顺序**不排序**、原样保留；未知字段忽略 | Compatibility §2.1；需求 §5.13；注 1 |
| 2 | `env` 值精确形态 `^${[A-Za-z_][A-Za-z0-9_]*}$`；其余一律拒载；**错误与日志不回显值** | 需求 §5.13；技术方案 §8.8；R1a |
| 3 | `env` 在**装配期**解析（`app.resolveMCPEnv`，lookup 可注入）；未设置**或空值** → 拒绝启动，点名 `server <name> 的 env.<key>` 与变量名 | 技术方案 §8.8；R1b |
| 4 | 连接**串行**；每 server 一个 **30s** 预算，**只罩 initialize + tools/list**（dial 不接收 ctx，进程生命周期不绑预算，约束 27）；失败 = WARN + 关闭半截会话 + 跳过；`Connect` 只在 ctx 取消 / 截止时返回错误 | 需求 §9.1；NivroOS 30s 先例；已决 #3 / #8 / #13；R11 |
| 5 | 连接期日志固定键 `server` / `reason` / `detail`；`reason` ∈ {`启动失败`, `初始化失败`, `列工具失败`}；`detail` = `safeErrorSummary` 固定短语（`可执行文件不存在` / `权限不足` / `超时` / `已取消` / `子进程退出码 N` / `外部交互失败`），**不得**放 `err.Error()` 原文；成功日志 `MCP server 已连接` + `server` / `tools` | 可观测性双轨；约束 28；R2 / R12 |
| 6 | 工具名**原样**（不加前缀、不做命名空间）；`Description` / `InputSchema` 原样透传；`InputSchema` 为 nil 或序列化失败 → `{"type":"object","properties":{}}` | 技术方案 §6.1 / §6.4；注 9 |
| 7 | 注册顺序 = 内置 8 件 → MCP（server 文件序 → 工具列表序）；重名 **keep-first** + WARN `拒绝重复注册的工具`；内置优先 | 技术方案 §6.4；`registry.go` 既有语义；约束 8 / R8 |
| 8 | 等值门禁（`registry.Names() == declaredToolNames`）**先于** MCP 注册；MCP 注册失败（重名）**不**中断装配 | `wire.go:248`；约束 9 / R3 |
| 9 | Agent 准入：规则 4 用**注册后**名字集；规则 6 = **已配置且已连接**；两规则按声明序报首个失败；规则 4 先于规则 6；轻路径第三态 `unverified`（待决 #2；零 MCP 配置时两态不变） | 契约 §2.1 规则 4 / 6；约束 6 / 22 |
| 10 | 调用入参：原始 `json.RawMessage` **原样**交给 `CallToolParams.Arguments`（空串 / `null` → `map[string]any{}`）；**不得**解成 `map[string]any`（大整数经 float64 丢精度）；非法 / 非对象 → `*core.InvalidInputError` | 约束 11；R13 |
| 11 | 结果渲染：文本块按序 `\n` 连接；非文本块占位格式 `[非文本内容：<type> <mime 或 uri>]`（type ∈ image / audio / resource_link / resource / 其他）；无文本 → `StructuredContent` 的紧凑 JSON；全空 → `(MCP 工具无输出)`；**不输出 base64** | 注 3；约束 12 |
| 12 | `IsError = true` → `*core.ExternalError`；`Reason` = 固定文案 `MCP 工具返回错误：<server>/<tool>`；server 内容块**整段丢弃**（不进 Reason / Cause / 日志 / 审计 / 模型上下文，无文本时同一文案） | 契约 §7.3 `external_error`；约束 13 / 28；R5 |
| 13 | 传输 / 协议错误 → `*core.ExternalError`，`Target = mcp:<server>/<tool>`，`Reason` = 固定文案 `MCP 调用失败：<server>/<tool>`；原始错误只进 `Cause`（只经 `Unwrap`，不进文案） | 契约 §7.3；`errors.go` 文案纪律；约束 14 / 28；R12 |
| 14 | ctx 取消 / 截止 → 返回 **ctx 原错误**（`ctxPreferred` 判序），**不得**包成 `ExternalError`；分类为 `timeout` | `tool_exec.go` 判序；R6 |
| 15 | `Result.Retryable` 恒 `false`（运行时不自动重试、恢复不自动重放） | Compatibility §2.4 外部副作用重试边界 |
| 16 | 适配器**不落审计、不写消息、不记日志**；执行器两阶段写与失败 WARN 是唯一留痕路径 | 宪法 II / V；约束 16；R9 |
| 17 | MCP 子进程环境 = `shell.env_passthrough` + 声明 `env`（声明覆盖透传）；结果**永不为 nil**；其余清空 | 技术方案 §6.5；待决 #3；约束 17 |
| 18 | MCP 子进程 cwd = **工作区根** | 已决 #9；约束 18 |
| 19 | `command` 按空白切分（首 token = 可执行文件）；无引号语义；`.cmd` / `.bat` 不自动包 shell | NivroOS 基线；注 10；约束 19 |
| 20 | 停机顺序 = MCP 池 → 数据库 → 实例锁；池 `Close` 幂等；错误 `errors.Join` 不静默 | Operations §5.3；约束 20；R10 |
| 21 | `mcp_servers.yaml` 变更**需重启**，不热加载 | 技术方案 §11.5；Compatibility §2.4 |
| 22 | `transport` 缺省 / `stdio` 通过；**字段非字符串**或显式非 stdio → **拒绝启动**（已决方案 A；若改判为 WARN-skip，只改一处判据与 R4 的姊妹用例） | 需求 §5.13；注 2 |
| 23 | 启动摘要日志 `重路径装配完成` 的键 = `agents` / `tools` / `mcp_connected`；`mcp_connected` = 实际连接数（零也输出） | 需求 §9.1；约束 23 |
| 24 | MCP server 的**内部**文件 / 网络行为不经 `Policy`；信任边界 = `mcp_servers.yaml` 的登记行；`command` 非模型可控 | 技术方案 §6.5；约束 24 |
| 25 | 日志与错误**不打印** `env` 值、server stderr 全文、工具结果正文、`IsError` 的 server 文本、协议错误原文；`detail` 只放 `safeErrorSummary` 固定短语（约束 28：单行化 / 截断不是脱敏） | 凭证纪律；约束 3 / 14 / 28；R5 / R12 |

### 评审回合裁决落案表

**R1 评审（2026-10-17）**：用户以本机 SDK v1.8.0 源码与临时探针核对，报 6 项（P1×2 / P2×4）；前两项为派生 spec / tasks 的**前置阻断**。全部采纳，逐条落案：

| 编号 | 评审发现 | 裁决 | 落案位置 |
| --- | --- | --- | --- |
| P1-1 | 连接成功后预算 `cancel()` 杀掉 server：R1 用 `exec.CommandContext(cctx)` 起进程，列完工具立即 `cancel()`——启动看似成功、首次调用即连接关闭（标准库探针已复现） | 采纳。`dial` 去掉 ctx、改用 `exec.Command`；30s 预算只罩 `client.Connect` + `listAllTools`；预算释放后进程存活到 `pool.Close`。新增约束 27 与 R11（真实子进程：预算释放 + 等待后仍可调用） | 约束 5 / 20 / 27；注 11；§2.3 流程 A / 伪代码 `Connect` / `dialCommand`；§3.1；§4.3 R11；口径 4 / 5 |
| P1-2 | 错误处理未满足凭证与正文不泄漏的承诺：`err.Error()` 进 `ExternalError.Reason`，执行器把 Reason 同时写日志、审计、模型上下文；`IsError` 透传 server 文本；单行化 / 截断不算脱敏 | 采纳。`Reason` 一律自产固定文案；原始错误只进 `Cause`（`ExternalError.Error()` 只返回 Reason）；`IsError` 的 server 文本整段丢弃；连接 `detail` 用 `safeErrorSummary` 固定短语。新增约束 28、注 12、R12（连接失败 / 协议错误两条敏感值负向），并把 R5 改写为 IsError 敏感值负向 | 约束 13 / 14 / 28；注 12；§2.3 流程 B / 伪代码 `Execute` / `safeErrorSummary`；§3.1 / §3.2；§4.3 R5 / R12；口径 5 / 12 / 13 / 25；§6.1 人工项 10 |
| P2-3 | 参数转换损失大整数精度：`json.Unmarshal` 到 `map[string]any` 得 float64，`9007199254740993 → 9007199254740992` | 采纳。`mcpCallArguments` 对非空入参返回原始 `json.RawMessage`（`map[string]json.RawMessage` 只做形态校验），空 / `null` → `map[string]any{}`；新增 R13 | 约束 11；§2.3 流程 B / 伪代码；§3.1 / §3.2；§4.3 R13；口径 10 |
| P2-4 | R8 的正确实现也会红：空 `ToolEnv` 下执行内置 `read_file` 读不存在的 `x` 被沙箱合法拒绝，测到的是沙箱不是注册语义 | 采纳。R8 改为**身份断言**：`fakeMCPTool` 改指针类型，`got.(*fakeMCPTool)` 必须为假；不再执行内置工具 | §4.3 R8；§4.3 辅助件表 `fakeMCPTool` |
| P2-5 | R10 含不存在的 API（`rt.db` 无 `PingContext`）且缺建池前提（零 server → 不建池 → 不调连接器 `Close`） | 采纳。库存活性改用 `rt.db.SchemaVersion(ctx)`；`writeMCPServersYAML` 写入一条配置作为建池前提 | §4.3 R10b |
| P2-6 | R9 失败路径可能挂死：handler 裸等 `release`、主测试裸等 `entered` | 采纳。handler 用 `select` 同时等 `release` 与 `ctx.Done()`；主测试两处等待改为带 `time.After(5s)` 的有界 `select` + `t.Fatal`；`release` 经 `sync.Once` + `t.Cleanup` 释放；结果通道带缓冲 | §4.3 R9 |
| P2-7 | R10a 样例无判别力：只断言两次 `Close()` 无错，不确认连接数、不观察会话关闭；`Close` 改成直接 `return nil` 仍能通过（SDK 自身关闭幂等，"去掉会话清空 → 第二次红"不成立） | 采纳。`Connect` 后断言 `len(conns)==1` 与池内 `sessions` 非空；`Close` 后在 **5s 限定**内观察 `session.Wait()` 返回；逆改点改为"**Close 完全不关闭会话** → Wait 不返回，第三条断言红"；幂等断言保留 | §4.3 R10a；tasks T029 / T030 / T034 |

### 收口补登

**收口复核（2026-10-17）**：用户复跑五个包的 `-race` 与五条 stdio 集成用例（均通过）后，以额外探针报 2 处行为问题 + 1 处验收缺口（P2×3）与 3 条建议；全部采纳并复验。读数与证据见 [run-state.md](../../specs/007-mcp/run-state.md) §2 / §5。

| # | 发现 | 落案 |
| --- | --- | --- |
| 1 | 最后一个 server 的握手期间整体取消被吞成 nil（复现：断言 `context.Canceled`，实得 nil） | `MCPPool.Connect` 在 dial / initialize / 列工具三个失败分支及返回前复核父 `ctx.Err()`；单 server 预算超时（父 ctx 未结束）仍按跳过。新增 `TestMCPPool_ConnectCancellationDuringHandshake`（取消 / 截止 / 拨号期取消 / 预算超时仍跳过四态），红态读数 `Expected error with "context canceled" in chain but got nil` |
| 2 | 非字符串 `transport` 被转成空串后按缺省 stdio 放行（复现：`1` / `true` / `[]` / `{}` 全部解析成功） | 解析器先做类型检查再查取值（`null` 视同缺省，与 env 同口径）；新增 `TestParseMCPServers_RejectsNonStringTransport`（四态），红态读数 `An error is expected but got nil`；约束 2、§2.3 伪代码、§4.2 / §6.2 口径 22 同步修订 |
| 3 | SC-001 的描述 / schema 保真无有效断言（overlay 清空描述 + 空 schema 后相关默认层测试仍全绿） | 新增 `TestMCPPool_ConnectPreservesToolDescriptionAndSchema`：经「SDK 列工具 → 适配器 → 注册池 → `Specs`」全链路比对描述与 schema。overlay 逆改验证判别力：清空描述 → `expected: "查询仓库提交历史（SC-001 保真探针）" / actual: ""`；强制兜底 schema → `Should not be: "{\"type\":\"object\",\"properties\":{}}"`。**SC-001 的状态从"需求已建立映射"改为"要求已验证"** |
| 4 | FR-011 缺回归（建议） | 新增 `TestWire_MCPConfigChangesNeedRestart`：运行中改 `mcp_servers.yaml` 不影响当前 runtime，重建后才读新配置 |
| 5 | 集成层直调适配器未设期限（建议） | 集成用例改经 `callDirect`，以 `context.WithTimeout` 承载 5s 期限——适配器不消费 `ToolEnv.Timeout` |

**测试计数（实测）**：默认层 46 → **50** 条（`workspace/mcp_test.go` 19、`tool/mcp_test.go` 20、`app/wire_mcp_test.go` 8），集成层 5 条不变，合计 **55** 条。R1a / R1b / R2~R13 的 14 条关键回归与 18 个函数不变。

**待决三条裁决（2026-10-17，均为方案 A）**：① 非 stdio `transport` 拒绝启动；② 轻路径第三态 `unverified`；③ MCP 子进程环境复用 `shell.env_passthrough`。

**遗留（人工项）**：§6.1 的 13 项与 quickstart §5 的 S1~S5 由用户执行；真模型 Demo 与跨平台编译矩阵保持**未验证**。

**R4 文档口径修订（2026-10-17 用户裁决）**：`env_passthrough` 的「缺省 `PATH` / `HOME` / `LANG` / `TZ`」表述与实际实现不符——代码层没有内置默认白名单（`internal/cli/config.go` 只声明字段、无默认赋值；`mergeChildEnv` 对空清单产出非 nil 空集），四个变量只出现在示例配置 `basalt.yaml.example`。用户裁决统一为：「只透传 `shell.env_passthrough` **显式登记**的变量；示例配置登记 `PATH` / `HOME` / `LANG` / `TZ`。**代码无内置默认白名单**，省略或配置为空列表时，**不透传**父进程环境变量；工具显式声明的变量仍按规则传入。」同步修订 [TechnicalSolution.md](../../docs/TechnicalSolution.md) §6.5、[Operations.md](../../docs/Operations.md) §3、[spec.md](../../specs/007-mcp/spec.md) FR-022、[data-model.md](../../specs/007-mcp/data-model.md) §3.3。**实现与配置键零改动**；受影响的冻结指纹（本文件 / spec.md / data-model.md）已在 [run-state.md](../../specs/007-mcp/run-state.md) §1.4 原地更新并保留旧值修订记录。

### 已决事项

| # | 已决（本 US 设计面内自决，无待裁项） | 落点 |
| --- | --- | --- |
| 1 | 包落位：`workspace` 解析配置（yaml + 形态校验）、`tool` 封装 SDK（池 + 适配器）、`app` 解析 env 并装配、`core` 只放纯类型、`testutil` 放测试基建 | 技术方案 §10.1；§3.1 |
| 2 | 一个 `core.MCPServerConfig` 类型两阶段：`workspace` 产出时 `Env` 是占位，`app.resolveMCPEnv` 之后是终值（只有装配层允许改） | §3.1；约束 4 |
| 3 | 连接**串行**、不并发连接：失败隔离语义与 NivroOS 一致，启动时延有界（N × 30s 最坏），实现与测试都简单 | §2.3；§6.2 口径 4 |
| 4 | 一个 `mcp.Client` 承载多会话（每 server 一个 `ClientSession`），池持有会话列表 | §3.1 `MCPPool` |
| 5 | `tool.MCPPool` 的 `dial` 是**非导出字段**注入点：生产用 `dialCommand`，包内白盒测试替换传输——不把 SDK 类型暴露成导出配置面 | §3.1；§4.3 辅助件 |
| 6 | `app` 增非导出 `mcpConnector` 接口 + `App.mcpConnect` 字段（沿 `providerOverride` 先例）；生产 nil → `tool.NewMCPPool` | §3.1；R3 / R10 |
| 7 | US-1 模板**只改注释**、字节仍 `{}`（零 server 的合法形态） | 注 7；§3.5 改造点 7 |
| 8 | 30s 连接预算是**代码常量**（`MCPPoolOptions.ConnectTimeout` 仅测试注入）；零新增配置键 | 注 8；约束 26 |
| 9 | MCP 子进程 cwd = 工作区根（实例级资源不绑定单 Agent） | 注 6；§6.2 口径 18 |
| 10 | 工具名 v1 不命名空间化；重名靠 keep-first + WARN | 注 9；§6.2 口径 6 / 7 |
| 11 | 规则 4 的报错保持契约字面量 `tool not registered: <name>`，规则 6 保持 `mcp server skipped` + detail = server 名（**不**把连接失败原因塞进 RejectReason） | 契约 §2.1；约束 6 |
| 12 | `testutil/mcpserver.go` 是**非测试文件**（跨包 import 需要），配 `mcpserver_test.go` 自证以过覆盖率判卷 | §3.2；§4.1 |
| 13 | 进程生命周期与握手预算分离：`dial` 不接收 ctx、用 `exec.Command`；30s 预算只罩 `client.Connect` + `listAllTools`，释放后会话存活到 `pool.Close`（SDK v1.8.0 事实见注 11） | 约束 27；R11；§6.2 口径 4 |
| 14 | 错误正文边界：`Reason` = 自产固定文案（进日志 / 审计 / 模型）；`Cause` = 原始错误（只经 `Unwrap`，不进文案）；`IsError` 的 server 文本整段丢弃；连接 `detail` = `safeErrorSummary` 固定短语 | 约束 28；注 12；R5 / R12；§6.2 口径 12 / 13 / 25 |
| 15 | 入参原样透传：非空入参以 `json.RawMessage` 交给 SDK（不经 float64），空 / `null` → `map[string]any{}` | 约束 11；R13；§6.2 口径 10 |

### 待决事项（**3 条**）

| # | 待决 | 选项 | 推荐与理由 | 改判时的换向点（留痕） |
| --- | --- | --- | --- | --- |
| 1 | **非 stdio `transport` 的处置** | A. **拒绝启动**（fail-closed，点名 server 与取值）· B. WARN 后跳过该条目（NivroOS 行为） | **A**。需求 §5.13 的加载期纪律是"格式非法明确报错、拒绝启动，不静默取默认"；`sse` / `http` 在 v1 **没有实现**，跳过等于把"配了但不生效"静默化。反向论据（B）：Compatibility 承诺"文件内容零改动"——一份含非 stdio 条目的 NivroOS 配置在 Basalt 上会整体起不来（NivroOS 只是少一个 server）。**取舍**：A 更符合 fail-closed；B 更符合迁移平滑。本文按 A 书写 | 改 B 的落点：`workspace/mcp.go` 的 transport 校验从 return error 改为 WARN + `continue`（需要把 logger 传进解析器，或在 `app` 过滤）；§6.2 口径 22、约束 2、`TestParseMCPServers_RejectsUnsupportedTransport` 改为"跳过且其余照常"；`Compatibility.md` 的差异行改写 |
| 2 | **轻路径第三态 `unverified`** | A. **引入第三态**：`AgentEntry.Unverified` + `profile list` 新行 + 汇总后缀（未验证数 > 0 时）· B. 保持两态：轻路径按 US-6 判（配置了 MCP 的工作区里，用 MCP 的 Agent 会显示拒载） | **A**。B 会让 `profile list` 对**所有** MCP 工具使用者误报 `tool not registered`，而同一份工作区 `chat` 能正常跑——两套判定相反，正是 `wire.go` 等值门禁注释里最想避免的情形。A 只改展示面：运行面（`newAgentSource`）仍只收 `Registered`，重路径不产生 `Unverified`，零 MCP 配置时行为逐字节不变 | 改 B 的落点：`core.AgentEntry` 不加字段、`cli/profile.go` 不改、§3.2 的 4 条 workspace 用例减为 2 条、`cli/profile_test.go` 两条删除；**同时**要在文档里如实写明"轻路径会把 MCP 工具声明误报为未注册"（把已知缺陷写进 §5.2 而不是假装没有） |
| 3 | **MCP 子进程的环境变量白名单** | A. **复用 `shell.env_passthrough` + 声明 `env`**（声明覆盖透传）· B. 固定只给 `PATH` / `HOME` / `LANG` / `TZ`（不受 `shell` 配置影响）· C. 继承完整父进程环境（NivroOS Java SDK 行为） | **A**。技术方案 §6.5 的"子进程环境白名单"理由（不把 `BASALT_*` / 云凭证 / 代理设置送给任意脚本）对 MCP server **同样成立**；复用同一配置键比再造一个 `mcp.env_passthrough` 少一个键、少一处漂移。B 更严格但让运维为同一类子进程维护两套心智。C 明确违反技术方案的环境白名单纪律（虽然 NivroOS 如此） | 改 B：`MCPPoolOptions` 去掉 `EnvPassthrough`，`dialCommand` 用固定常量表；§6.2 口径 17、约束 17、注 6 改写。改 C：需要用户**明确裁决**（与 §6.5 冲突，属宪法 VIII 相邻面），并在 `Compatibility.md` §3 记为"有意保持一致"；本文不建议 |

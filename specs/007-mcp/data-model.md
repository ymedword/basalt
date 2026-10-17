# Data Model: US-7 MCP 集成

**Feature**: `007-mcp` | **Date**: 2026-10-17 | **Plan**: [plan.md](./plan.md) | **Research**: [research.md](./research.md)

**结论先行**：本 feature **零持久化模型变更**——没有新表、没有新列、没有迁移；数据只有两类：**一个工作区文件**（`mcp_servers.yaml`）与**内存中的配置 / 连接 / 适配器对象**。审计复用既有 `tool_invocations` 表（`tool` 列即 MCP 工具名原样）。字段名与签名逐字取自颗粒度文档 §3.1；语义取自 §2.2 约束与 §6.2 固定口径 25 条。

## 1. 数据流总览

```text
<workspace>/mcp_servers.yaml
   │  workspace.ParseMCPServers(root) → parseMCPServers(base *os.Root)
   ▼
[]core.MCPServerConfig           阶段 1：Env 值 = ${VAR} 占位（只校验形态，不读环境）
   │  app.resolveMCPEnv(specs, lookup)            ← 失败：拒绝启动（点名 server / env.key / 变量）
   ▼
[]core.MCPServerConfig           阶段 2：Env 值 = 终值（永不进错误 / 日志）
   │  tool.MCPPool.Connect(ctx, specs)           ← 串行；单 server 失败 → WARN + 跳过 + 继续
   ▼
[]tool.MCPConnection { Server string; Tools []core.Tool }
   │  app.wire：注册进 core.ToolRegistry（内置 8 先，MCP 后；重名 keep-first）
   ▼
注册池（名字集）+ AgentLoaderOptions{MCPServers, MCPConnected}
   │  workspace.AgentLoader.LoadAll → 规则 4 / 6 / 第三态
   ▼
[]core.AgentEntry { Registered | Unverified | Rejected }
   │
   ▼
运行：ReActLoop → core.ToolExecutor → mcpToolAdapter → mcp.ClientSession（子进程 stdio）
停机：heavyRuntime.Close → MCPPool.Close → db.Close → lock.Release
```

## 2. 外部输入：`mcp_servers.yaml`

**位置**：`<workspace>/mcp_servers.yaml`（工作区根，经 `os.Root` 读取——软连接越界在根上被挡）。

**形态**（顶层映射 + `servers:` 列表；列表顺序 = 连接顺序 = 工具注册顺序）：

```yaml
servers:
  - name: filesystem
    transport: stdio
    command: node /opt/mcp/filesystem-server.js
    env:
      GITHUB_TOKEN: ${GITHUB_TOKEN}
```

| 字段 | 类型 | 必填 | 语义与约束 |
| --- | --- | --- | --- |
| `name` | string | 是 | server 名；空串 = 缺失。唯一（重名拒绝启动）；用于 `AGENT.md` 的 `mcp_servers:` 引用、日志 `server` 键、错误 `Target` 的 `mcp:<server>/<tool>` |
| `transport` | string | 否 | 空串或缺省 → `stdio`；**只接受 `stdio`**，其余值拒绝启动（待决 #1 方案 A） |
| `command` | string | 是 | 缺失或纯空白 → 拒绝启动；运行时按 `strings.Fields` 切分，首 token = 可执行文件；无引号语义、不包 `cmd /c`、不引入 shell |
| `env` | mapping<string,string> | 否 | 值必须是**精确** `${VAR}` 形态（`^${[A-Za-z_][A-Za-z0-9_]*}$`）；明文 / 半截占位 → 拒绝启动，错误不回显值 |
| 其他键 | 任意 | 否 | **忽略**（前向兼容；`TestParseMCPServers_IgnoresUnknownEntryKeys`） |

**零 server 的判定**（任一满足即零 server、整段跳过——不建池、不发日志）：

| 输入状态 | 判据 |
| --- | --- |
| 文件缺失 | `os.Root.ReadFile` 返回 `fs.ErrNotExist` |
| 空文件 / 纯空白 | `strings.TrimSpace(raw) == ""` |
| 纯注释 / `null` 文档 | YAML 解析结果为 nil（注释不产生节点） |
| `{}` | 顶层映射但无 `servers` 键（US-1 模板字节，注 7） |
| `servers:` 存在但值为 null | `rawList == nil` |

**错误定位口径**：错误文本点名 `mcp_servers.yaml` + `servers[%d]`（条目序号）**或** `server %s`（已取到名字时）+ 字段名；**永不**包含配置值 / 环境值 / 外部正文。

## 3. 内存实体

### 3.1 `core.MCPServerConfig`（`internal/core/mcp.go`）

```go
type MCPServerConfig struct {
    Name    string
    Command string
    Env     map[string]string
}
```

**无方法**；`core` 只依赖标准库。**同类型两阶段**：

| 阶段 | 产出方 | `Env` 的形态 | 允许谁改 |
| --- | --- | --- | --- |
| 1 解析后 | `workspace.parseMCPServers` | 每个值 = 精确 `${VAR}` 占位 | 只读传给 `app` |
| 2 解析后 | `app.resolveMCPEnv` | 每个值 = 环境终值（非空） | 只有装配层允许改（plan 不变量）；终值永不进错误 / 日志 |

### 3.2 装载器输入与准入结果（`internal/workspace/agent.go` / `internal/core/profile.go`）

```go
type AgentLoaderOptions struct {
    Providers    []string
    Tools        []string   // 注册后的工具名字集（含 MCP 工具；重路径）
    MCPServers   []string   // 已配置的 server 名（文件序）；nil = 轻路径自读
    MCPConnected []string   // 已连接的 server 名；nil = 轻路径（第三态）
    Logger       *slog.Logger
}

type AgentEntry struct {
    Name         string
    Registered   bool
    Unverified   bool     // 第三态：Registered == false && Unverified == true
    Profile      *Profile
    RejectReason string
}
```

**准入判定（规则 4 先于规则 6；各自按 `AGENT.md` 声明序报首个失败名）**：

| # | 路径 | 判据 | 结果 |
| --- | --- | --- | --- |
| 1 | 重路径（`MCPConnected != nil`） | 声明的 tool ∉ `Tools` | 拒载 `tool not registered: <tool>` |
| 2 | 轻路径（`MCPConnected == nil`）且 `len(MCPServers) > 0` | 声明的 tool ∉ 内置工具集 | **defer**（暂记待确认，继续判后续声明） |
| 3 | 轻路径且 `len(MCPServers) == 0` | 声明的 tool ∉ 内置工具集 | 拒载 `tool not registered: <tool>`（与 US-6 逐字节相同） |
| 4 | 重路径 | 声明的 server ∉ `MCPServers` | 拒载 `mcp server skipped`（detail = server 名） |
| 5 | 重路径 | server ∈ `MCPServers` 但 ∉ `MCPConnected` | 拒载 `mcp server skipped`（detail = server 名） |
| 6 | 轻路径且 `len(MCPServers) > 0` | 声明的 server ∉ `MCPServers` | 拒载 `mcp server skipped`（确定性规则先拒） |
| 7 | 轻路径且 `len(MCPServers) > 0` | server ∈ `MCPServers` | **defer** |
| 8 | 任意 | `defer == true` 且无任何拒载 | `Unverified = true`、`Registered = false`、`RejectReason = ""`；INFO `Agent 注册待确认` |
| 9 | 任意 | 既有确定性规则（路径 / frontmatter / 技能等）失败 | 照旧拒载——**不因 defer 放行** |

**不变量**：重路径**不产生** `Unverified`；`newAgentSource` 只收 `Registered` 条目（第三态只影响展示与摘要）；零 MCP 配置时轻路径与 US-6 逐字节相同。

### 3.3 连接池（`internal/tool/mcp.go`）

```go
type MCPConnection struct {
    Server string
    Tools  []core.Tool   // 适配器实例；顺序 = server 的 tools/list 顺序
}

type MCPPoolOptions struct {
    Logger         *slog.Logger
    EnvPassthrough []string      // 来自 shell.env_passthrough（显式登记；代码无内置默认，空 = 不透传）
    ConnectTimeout time.Duration // 0 → 30s（代码常量，零配置键）
    Version        string        // 填入 mcp.Implementation.Version
    Root           string        // 工作区根（子进程 cwd）
}
```

**`MCPPool` 内部状态**（非导出）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `logger` | `*slog.Logger` | 连接期 INFO / WARN |
| `client` | `*mcp.Client` | 单客户端（`NewClient(&Implementation{Name:"basalt", Version: ...}, nil)`） |
| `dial` | `func(core.MCPServerConfig) (mcp.Transport, error)` | 非导出测试缝；生产 = `dialCommand` |
| `root` / `envPassthrough` / `connectTimeout` | — | 来自 options |
| `mu` | `sync.Mutex` | 只保护 `sessions` 切片 |
| `sessions` | `[]*mcp.ClientSession` | 已连接会话；`Close` 后置 nil（幂等） |

**每 server 的状态机**：

```text
未尝试
  ├─ dial 失败（可执行文件不存在 / 权限不足 …）
  │     → WARN「MCP server 未连接——已跳过」(reason=启动失败) → 跳过（无会话可关）
  ├─ client.Connect（initialize）失败
  │     → SDK 已收尾传输 → WARN(reason=初始化失败) → 跳过
  ├─ listAllTools 失败（含重复游标）
  │     → session.Close() 收起半截会话 → WARN(reason=列工具失败) → 跳过
  └─ 成功 → cancel() 握手预算 → 会话进 sessions → INFO「MCP server 已连接」→ 产出 MCPConnection
```

**池级状态**：`新建 → Connect(...)`（可返回部分连接）`→ Close()`；`Connect` 只在整体 ctx 取消 / 截止时返回错误（返回已建立的连接 + ctx 错误；装配层随后关闭池）；`Close` 幂等，逐会话关闭并 `errors.Join`。

### 3.4 工具适配器（`internal/tool/mcptools.go`）

```go
type mcpToolAdapter struct {
    session     *mcp.ClientSession
    server      string
    name        string
    description string
    inputSchema any
}
```

| 方法 | 行为 |
| --- | --- |
| `Name() string` | `a.name`（**原样**，无前缀 / 无命名空间） |
| `Description() string` | `a.description`（原样） |
| `InputSchema() json.RawMessage` | `a.inputSchema` 原样序列化（`Tool.InputSchema` 为 `any`，见 research 读数 #10） |
| `Execute(ctx, env, input) (core.Result, error)` | 见 §5 / §6；不落审计、不写消息、不记日志 |

编译期断言：`var _ core.Tool = (*mcpToolAdapter)(nil)`。

### 3.5 装配层对象（`internal/app/mcp.go` / `wire.go`）

```go
type mcpConnector interface {
    Connect(ctx context.Context, specs []core.MCPServerConfig) ([]tool.MCPConnection, error)
    Close() error
}
```

- `App.mcpConnect mcpConnector`：**测试夹层**（沿 `providerOverride` 先例）；nil → 生产 `tool.NewMCPPool`。
- `heavyRuntime.mcp mcpConnector`：`Close` 顺序 **`mcp` → `db` → `lock`**。
- `resolveMCPEnv(specs, lookup)`：返回**新切片 / 新 map**语义按伪代码就地在 `specs[i].Env` 上替换（入参由装配层独占）；`lookup` 生产 = `os.LookupEnv`。
- `mcpToolNames(conns []tool.MCPConnection) []string`：按连接顺序收集工具名，供注册日志 / 测试对号。

## 4. 加载期校验矩阵（`parseMCPServers`）

| # | 条件 | 结果 | 错误点名 |
| --- | --- | --- | --- |
| 1 | 顶层不是映射 | 拒绝启动 | 文件 + 实际类型 |
| 2 | `servers` 存在但不是列表 | 拒绝启动 | 文件 + `servers` + 实际类型 |
| 3 | 条目不是映射 | 拒绝启动 | 文件 + `servers[%d]` |
| 4 | `name` 缺失 / 空串 | 拒绝启动 | 文件 + `servers[%d]` + `name` |
| 5 | `name` 重复 | 拒绝启动 | 文件 + server 名 |
| 6 | `transport` 非空且 ≠ `stdio` | 拒绝启动 | 文件 + server 名 + `transport` + 值（transport 值不是凭证，可回显） |
| 7 | `command` 缺失 / 纯空白 | 拒绝启动 | 文件 + server 名 + `command` |
| 8 | `env` 存在但不是映射 | 拒绝启动 | 文件 + server 名 + `env` |
| 9 | `env` 值不是 string 或不是精确 `${VAR}` | 拒绝启动 | 文件 + server 名 + `env.<key>`（**不回显值**） |
| 10 | `transport` 空串 | 通过（按 `stdio`） | — |
| 11 | 未知键 | 忽略 | — |
| 12 | 上述零 server 五态 | 零 server | — |

**装配期校验（`resolveMCPEnv`）**：

| 条件 | 结果 | 错误点名 |
| --- | --- | --- |
| `lookup(varName)` 未设置 | 拒绝启动 | `server <name> 的 env.<key>` + 变量名 |
| `lookup` 返回值是空串 | 拒绝启动（同档） | 同上 |
| 正常 | 值写入 `Env` | 值不进任何输出 |

## 5. 错误映射（`mcpToolAdapter.Execute`）

| 条件 | 返回 | `classifyToolError` 档位 | 可观察文本 |
| --- | --- | --- | --- |
| 入参非合法 JSON 对象 | `*core.InvalidInputError{Tool, Reason: "MCP 工具入参必须是 JSON 对象"}` | `invalid_input` | Reason（自产） |
| ctx 已取消 / 截止（判序最先） | ctx 原错误 | `timeout` | ctx 错误文本 |
| `CallTool` 返回传输 / 协议错误 | `*core.ExternalError{Target: "mcp:<server>/<tool>", Reason: "MCP 调用失败：<server>/<tool>", Cause: err}` | `external_error` | Reason（自产）；原文只经 `Unwrap` |
| `CallTool` 成功但 `IsError == true` | `*core.ExternalError{Target: "mcp:<server>/<tool>", Reason: "MCP 工具返回错误：<server>/<tool>"}`（**无 Cause**） | `external_error` | Reason（自产）；**server 文本整段丢弃** |
| 成功 | `core.Result{Content: renderCallToolResult(res)}` | — | 见 §6 |

**注**：MCP 适配器不会产出 `fs.ErrNotExist`，故 `not_found` 档对 MCP 工具**不可达**（注 4）；测试不得写 `not_found` 断言。`Retryable` 恒 `false`。

## 6. 结果渲染（`renderCallToolResult`）

优先级（先命中先返回）：

| 顺序 | 条件 | 输出 |
| --- | --- | --- |
| 1 | `res.Content` 中存在 `*mcp.TextContent` | 所有文本块按序以 `\n` 连接（非文本块在此情形被忽略） |
| 2 | `res.StructuredContent != nil` | 其紧凑 JSON（`json.Marshal` 成功时） |
| 3 | 存在非文本块 | 逐个 `[非文本内容：<type> <mime 或 uri>]`，`\n` 连接；`type` ∈ `image` / `audio` / `resource_link` / `resource` / `其他` |
| 4 | 其余（`Content` 为空且 `StructuredContent` 为 nil / 序列化失败） | `(MCP 工具无输出)` |

**禁止**：base64 原文进入输出（`blob` 工具负向用例守）。**实现约束**：`mcp.Content` 是密封接口（未导出方法），只能对 SDK 具体类型做类型分支（research 读数 #11）。

## 7. 可观测面形状（日志 / 摘要 / 展示）

| 事件 | 级别 | 键 / 文案 |
| --- | --- | --- |
| server 连接成功 | INFO | `MCP server 已连接`；`server`、`tools`（工具数） |
| server 连接失败 | WARN | `MCP server 未连接——已跳过`；`server`、`reason` ∈ {`启动失败`, `初始化失败`, `列工具失败`}、`detail` = `safeErrorSummary` 固定短语（`可执行文件不存在` / `权限不足` / `超时` / `已取消` / `子进程退出码 N` / `外部交互失败`） |
| 工具重名 | WARN | `拒绝重复注册的工具`；保留先注册者（内置优先） |
| 装配完成摘要 | INFO | `重路径装配完成`；`agents`、`tools`、`mcp_connected`（实际连接数；零也输出） |
| Agent 待确认 | INFO | `Agent 注册待确认`；`agent` |
| Agent 拒载 | 既有 | `Agent 未注册`；`agent`、`reason`（规则 4 / 6 的固定文案） |
| `profile list` 待确认行 | 展示 | `<name>：registered=unverified——MCP 工具与连接需重路径确认`（行首两个空格） |
| `profile list` 汇总后缀 | 展示 | 未验证数 > 0 → 追加「空格 + `/ 未验证 %d`」；零计数**逐字节不变** |

## 8. 持久化

| 项 | 状态 |
| --- | --- |
| 新表 / 新列 / 索引 | **无**；`schema.sql` 零 diff |
| 迁移 | **无**；`internal/storage/migrations/` 零新增 |
| MCP 调用审计 | 复用 `tool_invocations`：`tool` = 工具名原样；`status` / `error_kind` / `error_message` 由既有执行器写；`error_kind` 只可能取 `invalid_input` / `timeout` / `external_error` |
| MCP 配置 / 连接状态 | **仅内存**；进程重启即重连（不热加载，FR-022） |

## 9. 机器可判不变量（收尾自查）

| # | 不变量 | 复核命令 / 方式 |
| --- | --- | --- |
| I-1 | SDK 只出现在 `internal/tool`（生产）与 `internal/testutil`（测试基建） | `grep -rln 'modelcontextprotocol' --include='*.go' internal` |
| I-2 | 适配器不写审计 / 消息 / 日志 | 代码审查：`mcpToolAdapter` 四方法签名不含仓储 / 日志参数；`grep -rn 'BeginInvocation\|INSERT INTO tool_invocations' internal/tool/mcptools.go` 零命中 |
| I-3 | `core` 只依赖标准库 | `GOPROXY=https://goproxy.cn go list -f '{{.Imports}}' ./internal/core` |
| I-4 | 子进程不经 shell、不绑预算 | `TestMCPPool_DialCommandEnvAndDir`（`cmd.Cancel == nil`、argv 切分、`Env` / `Dir` 断言） |
| I-5 | 子进程环境永不为 nil | `mergeChildEnv` 单测（`runner_test.go` 既有 + MCP 语义复用） |
| I-6 | 错误正文只出固定文案 | R5 / R12 三条敏感值负向用例 |
| I-7 | 重名 keep-first、门禁先于 MCP 注册 | R8 / R3 |
| I-8 | 停机顺序池 → 库 → 锁 | R10 后半（假连接器 `Close` 里读 `db.SchemaVersion` 成功） |
| I-9 | 入参原始字节、大整数保真 | R13 |
| I-10 | 零配置时行为与 US-6 逐字节相同 | `TestAgentLoader_NoMCPConfigKeepsTwoStateBehavior` + `TestRenderProfileList_ZeroUnverifiedKeepsSummaryBytes` |

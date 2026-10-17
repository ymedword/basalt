# Quickstart: US-7 MCP 集成 — 验证指南

**Feature**: `007-mcp` | **Date**: 2026-10-17 | **Plan**: [plan.md](./plan.md)

本文件是**验证 / 运行指南**，不是实现说明：每条命令可直接复制执行，并给出预期读数。实现细节见 [plan.md](./plan.md)、实体与判据见 [data-model.md](./data-model.md)、决策与 SDK 事实见 [research.md](./research.md)。

## 0. 前置

```sh
cd /d/code/basalt
GOPROXY=https://goproxy.cn go version          # 预期：go version go1.27.2 windows/amd64
git branch --show-current                      # 预期：007-mcp
git log -1 --format=%h                         # 实现前基线：8d9bdfb
```

- 所有 `go` 命令**显式带** `GOPROXY=https://goproxy.cn`（本机直连不可达）。
- 工作区根 = `<当前目录>/.basalt`（`basalt init` 创建）；MCP 配置文件 = `.basalt/mcp_servers.yaml`。
- 默认层**不访问真实网络、不打真实模型、不起真实子进程**；集成层才起真实子进程（本仓首个）。

## 1. 默认层单元验证（毫秒级，无外部依赖）

```sh
GOPROXY=https://goproxy.cn go test -race ./internal/workspace \
  -run 'TestParseMCPServers|TestMCPServerNames|TestIsEnvPlaceholder|TestAgentLoader' -v

GOPROXY=https://goproxy.cn go test -race ./internal/tool \
  -run 'TestMCPPool|TestListAllTools|TestMCPToolAdapter|TestMCPCallArguments' -v

GOPROXY=https://goproxy.cn go test -race ./internal/app \
  -run 'TestWire_|TestResolveMCPEnv' -v

GOPROXY=https://goproxy.cn go test -race ./internal/cli \
  -run 'TestRenderProfileList' -v

GOPROXY=https://goproxy.cn go test -race ./internal/testutil \
  -run 'TestMCPTestServer|TestRunMCPHelper' -v
```

**预期**：

| 包 | 读数 | 说明 |
| --- | --- | --- |
| `workspace` | 新增 18 条 + 装载器 4 条全 PASS | 解析（八类校验 / 零 server 五态 / 顺序 / `os.Root`）、R1a、R4、第三态、零配置两态 |
| `tool` | 18 条全 PASS | R2 / R5 / R6 / R7 / R9 / R10 / R12 / R13 + 渲染 + env/cwd |
| `app` | 7 + 1 条 PASS | R3 / R8 / R10 后半 / 摘要计数 / 池关闭 / R1b |
| `cli` | 2 条 PASS | unverified 行逐字、零计数汇总逐字节 |
| `testutil` | 1 PASS + 1 **SKIP** | `TestRunMCPHelper_SkipsWithoutEnv` 的跳过是**预期**（未设 `BASALT_MCP_HELPER`），跳过理由必须可读 |

**注意**：`internal/tool/mcp_integration_test.go` 带 `//go:build integration`，**默认层不编译、不执行**——上面命令里看不到它的任何用例是正常的。

## 2. 集成层（真实子进程 stdio 链路）

```sh
GOPROXY=https://goproxy.cn go test -tags=integration ./internal/tool \
  -run 'TestMCPIntegration|TestMCPHelperProcess' -v
```

**预期**：5 条 `TestMCPIntegration_*` PASS；`TestMCPHelperProcess` 在顶层运行时报 **SKIP**（它只在子进程里带 `BASALT_MCP_HELPER=1` 时有意义）。

**已知跳过条件**：测试二进制路径（`os.Args[0]`）含空白时，5 条集成用例应以 `t.Skip("测试二进制路径含空白——command 按空白切分，无法表达")` 跳过。出现该跳过时：本轮集成层计「**跳过 + 依据**」，**不得**写成通过；换一个无空白路径的仓库副本重跑即可拿到真实读数。

## 3. 全量门禁

```sh
make check
```

**预期**：`fmt lint sec test test-integration cover build` 七目标全绿。其中：

- `test-integration` 在本 US 之前是**空跑即绿**（本仓无集成用例）；本 US 后它**首次实跑**——该目标必须能看到集成层输出；
- `cover` 按包判失败：`core` / `tool` ≥ 70%，`workspace` / `app` / `cli` / `testutil` ≥ 50%（阈值**零改动**）；
- `-race` 只在 `test` 目标；`build` 目标 `CGO_ENABLED=0`——两者**不得**合并成一条命令。

## 4. 包边界与全局不变量（可复制）

```sh
# SDK 只出现在生产工具包 + 测试基建
grep -rln 'modelcontextprotocol' --include='*.go' internal
# 预期：internal/tool/... 与 internal/testutil/mcpserver.go（且仅此两处）

# core 只依赖标准库
GOPROXY=https://goproxy.cn go list -f '{{.Imports}}' ./internal/core

# 无 Agent 框架坐标 / 无 ORM
grep -nE "eino|langchaingo|adk-go|trpc-agent-go|gorm|entgo|ent\.io|xorm" go.mod
# 预期：零命中

# context.WithValue 逐处人工确认（只允许追踪标识）
grep -rn "context.WithValue" --include='*.go' cmd internal

# 抑制注释零使用
grep -rnE "//nolint|#nosec|//lint:ignore" --include='*.go' internal
# 预期：零命中

# 格式检查只读（先查退出码、再判输出）
gofumpt -l . ; echo "exit=$?"
```

**预期**：`grep modelcontextprotocol` 只命中 `internal/tool` 与 `internal/testutil`；`go list ./internal/core` 只出标准库；框架 / ORM / 抑制注释零命中；`gofumpt -l` 输出为空且退出码 0。

## 5. 手工验收场景（对应颗粒度文档 §6.1）

以下场景需要**真实模型**或**真实 MCP server**，由用户人工执行；harness 只判卷自动化部分。

### S1 零代码接入（Demo 三，§6.1 第 2 项）

```sh
mkdir -p /tmp/basalt-demo && cd /tmp/basalt-demo
basalt init
# 编辑 .basalt/mcp_servers.yaml（写一个现成 server）：
#   servers:
#     - name: fs
#       transport: stdio
#       command: node /opt/mcp/filesystem-server.js
#       env:
#         GITHUB_TOKEN: ${GITHUB_TOKEN}
# 编辑 .basalt/agents/<agent>/AGENT.md：tools 与 mcp_servers 声明该 server 的工具与名字
export GITHUB_TOKEN=...
basalt chat          # 或 basalt serve
basalt tool list     # 工具清单应出现该 MCP 工具
```

**核对**：启动摘要 `重路径装配完成` 含 `mcp_connected=1`；模型确实调用了该工具；`.basalt/basalt.db` 的 `tool_invocations` 有对应行（`status=succeeded`）；**全程不改代码**。

### S2 故障隔离（§6.1 第 3 项）

把上面 `command` 改成不存在的可执行文件（如 `basalt-mcp-no-such-binary`），再启动：

**核对**：启动**不中断**；日志出现 `MCP server 未连接——已跳过`，`server` 点名、`detail` 为固定短语（如 `可执行文件不存在`）；声明该工具 / server 的 Agent 整只不注册（`tool not registered: <tool>` 或 `mcp server skipped`）；其余 Agent 正常可聊；摘要 `mcp_connected=0`。

### S3 env fail-closed（§6.1 第 6 项）

```sh
env -u GITHUB_TOKEN basalt chat
```

**核对**：拒绝启动、退出码非零；错误点名 `server fs 的 env.GITHUB_TOKEN 引用的 GITHUB_TOKEN 未设置或为空`；错误文本**不含**任何环境值。把变量设为空串再试，同样拒绝。

### S4 两档展示面（§6.1 第 4 项）

零 MCP 配置的工作区：`basalt profile list` 的输出与 US-6 基线**逐字节相同**。配置了 MCP（未设 env / 未连接）时：用 MCP 工具的 Agent 显示：

```text
  <name>：registered=unverified——MCP 工具与连接需重路径确认
```

汇总行在未验证数 > 0 时追加 ` / 未验证 <n>`；未验证数为 0 时逐字节不变。

### S5 停机无残留子进程（§6.1 第 7 项）

启动 `serve` / `chat` 后正常退出：

```sh
tasklist | grep -i node        # 或 server 可执行文件名
```

**核对**：MCP server 子进程**零残留**；池关闭错误（若有）出现在日志与退出码里。

## 6. 关键回归 → 命令映射

| 锚点 | 测试函数（逐一可 `-run`） | 所在包 |
| --- | --- | --- |
| R1a | `TestParseMCPServers_RejectsPlaintextEnvWithoutEchoingValue` | `workspace` |
| R1b | `TestResolveMCPEnv_MissingOrEmptyFailsClosed` | `app` |
| R2 | `TestMCPPool_ConnectSkipsFailedServerAndKeepsOthers`（+ 集成层 `BadCommandIsSkipped`） | `tool` |
| R3 | `TestWire_MCPToolsRegisteredAfterEqualityGate` / `TestWire_AgentDeclaringFailedMCPToolIsNotRegistered` | `app` |
| R4 | `TestAgentLoader_ConfiguredButNotConnectedRejects` | `workspace` |
| R5 | `TestMCPToolAdapter_MapsIsErrorToExternalError` | `tool` |
| R6 | `TestMCPToolAdapter_ContextIdentityWinsOverExternalError` | `tool` |
| R7 | `TestMCPPool_ListToolsPaginatesToEmptyCursor` / `TestListAllTools_RepeatedCursorFails` | `tool` |
| R8 | `TestWire_DuplicateMCPToolKeepsBuiltin` | `app` |
| R9 | `TestMCPToolAdapter_CallIsAuditedBeforeExecute` | `tool` |
| R10 | `TestMCPPool_CloseIsIdempotent` / `TestWire_CloseClosesMCPBeforeDatabase` | `tool` / `app` |
| R11 | `TestMCPIntegration_CallSucceedsAfterConnectBudgetReleased` | `tool`（集成层） |
| R12 | `TestMCPPool_ConnectFailureLogOmitsSensitiveDetail` / `TestMCPToolAdapter_ProtocolErrorOmitsSensitiveDetail` | `tool` |
| R13 | `TestMCPCallArguments_PreservesLargeIntegers` | `tool` |

单条复跑示例：

```sh
GOPROXY=https://goproxy.cn go test -race ./internal/tool -run '^TestMCPPool_ConnectSkipsFailedServerAndKeepsOthers$' -v
```

## 7. 本指南**不**覆盖的项

- 真模型 / 真 server 的行为质量（S1/S2 属人工项，自动化只到协议链路）；
- 四目标交叉编译矩阵（人工项，`make check` 只构建本机目标）；
- `docs/Compatibility.md` §3 增补三行的措辞评审（文档同步项，随 §3.5 改造点 ⑨ 送审）；
- markdownlint 判卷（本机无 Node；仓库根 `.markdownlint.json` 只供有 Node 的环境/CI 使用）。

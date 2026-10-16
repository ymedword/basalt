# Implementation Plan: US-4 ReAct 循环与首个工具闭环

**Branch**: `004-react-loop` | **Date**: 2026-10-16 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/004-react-loop/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 4` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us4-react-loop.md](../../docs/us/us4-react-loop.md) §2 / §3 为准，冲突时以 [docs/TechnicalSolution.md](../../docs/TechnicalSolution.md) 为准。

## Summary

让 Basalt 第一次自己跑起来：一个**唯一运行入口**驱动**本仓自实现**的 ReAct 循环完成「模型 → 工具调用 → 工具结果回填 → 再问模型」闭环，沙箱从条文变成句柄，`basalt chat` 在真控制台上多轮对话、会话历史跨重启可续。

技术路线的三个要点：**① 唯一运行入口 + 自实现循环**——`AgentService.Run(ctx, req)` 是唯一入口（准入：未知 Agent / 同会话忙 / 实例满**不排队**、直接返错 → 冻结并落库配置快照、失败即**拒绝启动** → `runs` 落 `running` → `ReActLoop.run(ctx, runID, env)`）；循环体是本仓代码，与技术方案 §4.1 伪代码**逐块同构**，六处必要差异冻结在颗粒度文档 §2.4 注 12（`run` 显式带 `runID`；`env.Session.CompleteToolGroup(...)`；`env.Session.AppendToolResult(call, result)`；`ToolExecutor.Execute(ctx, env, toolSet, call)` **返回结果、不写消息**；`env.ToolEnv(ctx, runID)` 显式构造）。消息追加**三条路径一个出口**——循环追加模型响应与工具结果、运行入口写用户消息、`Session.CompleteToolGroup` 补半截组，全部经同一条写透接口。**② 沙箱从条文变成句柄**——`WhitelistPolicy.Check(ctx, action)` 只看「谁、要碰什么」（`Action` **不含 argv**；跨 Agent 读**必须显式授权**、保护规则**先于**一般放行、`bash` / `sh` 硬拒、四段式拒绝文案）+ `LocalRunner.Run(ctx, cmd)` 受控子进程（环境变量白名单、超时**杀整棵进程树**、输出**读取中**限流、解释器 argv 形态与脚本真实路径校验属**执行侧**）；Windows 侧进程树走 Job Object 的**挂起—关联**顺序（`CreateJobObject(KILL_ON_JOB_CLOSE)` → `CREATE_SUSPENDED` 创建 → `AssignProcessToJobObject` → `ResumeThread`，收尾顺序固定），Unix 侧进程组随 exec 原子生效。三件内置文件工具（`read_file` / `write_file` / `list_dir`）首次让闭环真跑通；ShellTools / HttpTools 归 US-5（**沙箱本身是本 US 正式实现，不是桩**）。**③ 一次运行三处留痕 + 崩溃恢复 + 首个真控制台命令面**——审计两条链 fail-closed（工具 `pending` 预写失败即**拒执行** + 回填「审计不可用」且 **Run 继续**；模型调用预写失败即**拒发** + **Run failed**），消息写入失败同样 fail-closed（Run `failed`）；启动时（取锁后、开跑前）六步恢复扫描把 `pending` 归一化为 `unknown`、按**位置**判半截组并按五分支补齐（**只进消息、不进审计**）、把**仍为** `running` 的运行标 `failed`，组修复与运行收尾在**同一短事务**内且幂等；命令面新增 `chat`（真控制台经**控制台 API** 读写、管道 / 重定向两侧显式 UTF-8；Ctrl-C → `cancelled` + 退出码 0）与 `session list`（**只读**：不取锁、不恢复、校验 `schema_version`）；评测基座 `evals/`（YAML 用例集 + 可执行断言 + 资源用量读数，**不换算金额**）落在仓库根新顶层目录。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）

**Primary Dependencies**: **零新增坐标**（宪法 VIII + 依赖最小化）。既有坐标不动（`spf13/cobra` v1.10.2 / `goccy/go-yaml` v1.19.2 / `sashabaranov/go-openai` v1.43.0 / `modernc.org/sqlite` v1.60.1 / `robfig/cron/v3` v3.0.1 / `stretchr/testify` v1.12.1）；`golang.org/x/sys` v0.48.0 自 US-3 起已是**直接依赖**，本 US 复用其能力面——`windows`（Job Object：`CreateJobObject` / `AssignProcessToJobObject` / `SetInformationJobObject` / `TerminateJobObject` + 常量 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` / `JobObjectExtendedLimitInformation`；控制台：`GetConsoleMode` / `SetConsoleMode` / `ReadConsole` / `WriteConsole` / `GetConsoleOutputCP` / `SetConsoleOutputCP` / `GetStdHandle`）与 `unix`（`IoctlGetTermios` / `IoctlSetTermios` + 平台常量 `TCGETS`（Linux）/ `TIOCGETA`（Darwin））。**不引入** `golang.org/x/term`（不在模块缓存、不在技术栈句内——`IsTerminal` 自实现于 `x/sys` 之上）；**不提升** `github.com/mattn/go-isatty` v0.0.24（保持 indirect）。标准库：`crypto/rand`（RunID 随机段）、`crypto/sha256` + `encoding/hex`（会话标识派生与快照哈希）、`encoding/json`（`ProviderExtra` 往返保真用 `UseNumber`）、`os.Root`（工作区钉根读写；新文件写入经**相对根句柄**）、`os/exec`（受控子进程）、`net/url`（网络目标校验）、`log/slog`、`time`、`testing`。无新增 `import _` 副作用（SQLite 驱动注册沿用 US-1）。

**Storage**: SQLite——**六表既有 DDL 零迁移**（`sessions` / `messages` / `runs` / `config_snapshots` / `llm_calls` / `tool_invocations` 由 US-1 建齐；已发布迁移**只追加、不得改写**，宪法 IX）。本 US 首次**读写全六表**（`llm_calls` 仍只经 `provider` 的两阶段入口）。列级口径（逐项核对既有 DDL，本 US **不得**改写）：`runs` **无 `final_text` 列**（最终回答只存在 `RunResult.FinalText` 与会话的 assistant 消息里——`FinishRun` 不接收 `finalText` 参数）；`tool_invocations` **无 `round`、无 `truncated` 列**（截断标记写在既有 `result_json` 表示内 `{"content":…,"retryable":…,"truncated":true}`）；`config_snapshots` 主键 `id` + 唯一键 `(agent_name, hash)`，`content` = **AGENT.md 全文**（**不能只存哈希**），落库走 insert-if-absent（冲突即 no-op）；`messages.run_id` 是恢复的判据来源，故追加接口**显式承载 `runID`**；`llm_calls.round` = 该 Run 内第几轮（**1 起**）；时间戳 UNIX 毫秒。事务边界：恢复扫描的**组修复与运行收尾在同一短事务**内，**不得**跨 LLM 调用与工具执行开事务（`storage/doc.go` 事务纪律段更新——既有改造点 §3.5 第 7 项）。

**Testing**: `go test -race ./...` 默认层——**21 个测试文件 185 条新增**（19 个新文件 + 2 个追加：`workspace/agent_test.go` 4 条、`provider/client_test.go` 4 条），逐文件条数与颗粒度文档 §3.2 对齐（收口核对 `grep -c '^func Test'`）；**不访问真实网络与真实模型**（`provider` 侧以 fake client 注入、实现 `core.ProviderClient`；无 `net.Listen`）；夹具 `t.TempDir()` 内联构造，**不扩 `internal/testutil` 公开面**；时间一律显式传入（`now time.Time` / 时钟注入）；测试函数名**英文**（`TestXxx_yyy`），文档 harness 的中文验收点以注释保留。**集成层本 US 无新增**（`make test-integration` 跑既有用例）；评测层 `go run ./evals --cases evals/cases` 默认 fake 后端、真模型**显式门控**（命令行指定 + 环境变量凭证）；人工层见 §6.1 十一项。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物 `CGO_ENABLED=0` 交叉编译三平台（`console_windows.go` / `runner_windows.go` 在本机执行；`console_linux.go` / `console_darwin.go` / `runner_unix.go` 由 `CGO_ENABLED=0 GOOS=… go build ./...` 编译到，运行分支在 CI / 真机——**不得**因 CI 绿宣称 Windows 分支已验，反之亦然）；`-race` 与发布构建**分属不同命令 / job**。

**Project Type**: CLI + 服务单二进制（本 US 交付命令面第 4 / 5 组：`chat` 与 `session`；REST 面归 US-9）+ 仓库根**新顶层目录** `evals/`（`package main` 评测运行器；`internal/**` 是 Go 内部包，模块根下任意包——含 `evals/`——均可导入）

**Performance Goals**: 无显式吞吐 / 延迟指标（底座零件类 feature）；判据是**行为顺序**与**资源上限可解释**——每次工具执行**之前**先查取消、再查预算（**取消优先于预算**，工具预算耗尽**不**禁止「不使用工具的模型响应」）；每 Run 迭代上限 10（Profile 可覆盖）、工具调用上限 50、单工具超时 30 秒（`min(30s 缺省, 剩余 Run 预算, 更小的工具请求值)` 并**实际建立**超时 context——**不新增** Profile 覆盖键）、输出上限 256 KB、上下文预算缺省 32768（另有 `reserve_tokens` 1024 余量）、CLI 每次 Run **60 秒** deadline（调用方已有更早截止时取更早者；Ctrl-C **不能**替代资源时限）；达上限**不是错误**（`limit_reached` + 已得部分结果）。

**Constraints**: 三条包纪律（依赖方向 / 接口定义在消费方 / 第三方实现放消费方包内）——本 US 的落法：**`core` 只依赖标准库，且自本 US 起首次出现可执行语句**（判据 = `go list -f '{{.Imports}}' ./internal/core` 只出标准库包名，**不得**出现第三方路径与 `github.com/ymedword/basalt/internal/...`）；`cli` **不得** import `workspace`（端口定义在消费方、适配器在 `app`）；`workspace` **不得**反向 import `tool`（注册池名字集由 `app` 装配期经 `AgentLoaderOptions.Tools` 注入）；工具执行**串行**、无异步编排；`context` 只承载取消 / 截止 / 追踪（`runID` 与 `ToolEnv` **显式传参**，`context.WithValue` 零使用，**不得**用包级变量模拟 ThreadLocal）；审计两阶段 fail-closed **两条链**（模型调用 / 工具执行）；错误不吞（`err` 必落日志 / 审计或上抛，**不得** `_ =`）+ **可观测性双轨**（失败路径审计落库与 WARN 日志**两者都要**）；日志与错误文案**不得**回显文件全文或配置原文。覆盖率：`core` / `tool`（沙箱）/ `storage` ≥70%（本 US 起**首次**判卷）、`channel` 与其余 ≥50%，由既有 `make cover` 按包判失败。

**Scale/Scope**: 新增实现文件 34 个 + 改造 10 个（`core/errors.go`、`storage/doc.go`、`workspace/{context_loader,agent}.go`、`provider/client.go`、`cli/{ports,root,config}.go`、`app/{ports,run}.go`）+ 新增测试文件 19 个与 2 个测试文件追加（21 文件 185 条）+ `basalt.yaml` / `basalt.yaml.example` 的 `http.allowed_domains` 补键 + `evals/`（`README.md` / `main.go` / `cases/*.yaml` / `injection/*.yaml`）；**零新增坐标**、**零迁移脚本**、**`Makefile` 零改动**；前序改造点 8 项（逐条软门禁——§3.5）+ 后续占位 7 项

**环境实测读数（2026-10-16，本轮当场命令）**: `go version` → `go1.27.2 windows/amd64`（与 `toolchain go1.27.2` 一致，退出码 0）；`go.mod` 三行读取：`go 1.27` / `toolchain go1.27.2` / `golang.org/x/sys v0.48.0`（位于**直接依赖**块内、**无** `// indirect` 标记——US-3 的「迁直接依赖」结论复验通过；同轮读到 `github.com/mattn/go-isatty v0.0.24 // indirect` 仍在 indirect 块，**不提升**）；`GOMODCACHE` = `C:\Users\76562\go\pkg\mod`，`golang.org/x/sys@v0.48.0` 就地在缓存（H3 门禁可离线核实第三方 API）；`scripts/check-coverage.sh` 存在（8147 字节），第 146 行阈值口径 `core | tool | memory | storage) threshold=70`、其余 `50`（「无需改脚本 / Makefile」的结论复验通过）；`internal/` 现有包目录 11 个（`app` / `channel` / `cli` / `core` / `memory` / `provider` / `storage` / `testutil` / `tool` / `web` / `workspace`）——**本 US 不新增内部包**（`evals/` 在仓库根，非 `internal/` 包）。第三方 API 存在性读数逐条登记在 [research.md](./research.md)：`windows` 侧 Job Object 四函数 + 控制台面六函数 + `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE = 0x00002000` / `JobObjectExtendedLimitInformation = 9` / `JOBOBJECT_EXTENDED_LIMIT_INFORMATION` 结构体；`unix` 侧 `IoctlGetTermios` / `IoctlSetTermios` + `TCGETS = 0x5401`（linux/amd64）/ `TIOCGETA = 0x40487413`（darwin/amd64）。工具链与静态检查工具就绪读数沿用 US-1 plan；本轮**未复测**的项不在此声称（**未跑** `make check`、**未跑**三平台编译命令）。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

对照 [.specify/memory/constitution.md](../../.specify/memory/constitution.md) 十原则 + 三条架构约束 + 开发工作流（GATE 判定：通过 / 不适用 / 需说明）：

| # | 宪法条目 | 本 feature 关系 | GATE |
| --- | --- | --- | --- |
| I | 自实现 ReAct 循环 | **适用（首个完整落地面）**：循环体 = `internal/core/loop.go` 本仓代码，与技术方案 §4.1 逐块同构；`go.mod` **零新增坐标**（无任何 Agent 框架——收尾 grep 零命中）；协议 SDK 只做编解码、**不接管循环、不自动执行工具** | 通过 |
| II | LLM 客户端只做协议调用 | **适用（强形态）**：**不得**启用任何自动工具循环；工具调度唯一归属 `ToolExecutor`；会话历史**三条路径一个出口**（循环追加模型响应与工具结果 / 运行入口写用户消息 / `Session.CompleteToolGroup` 补半截组，均经同一写透接口），`ToolExecutor` **只返回结果、不写消息**；`provider` 侧只追加出站 `tools` 键的编解码 | 通过 |
| III | Provider 必须显式映射 | **适用**：映射表在装配处（`app/wire.go`）一次构建为「名称 → 客户端」映射表，运行时只查表（`RunEnv` 经注入的 client 读取）——**不得**遍历推断 | 通过 |
| IV | 一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露 | **部分适用**：本 US 把**工具注册池名字集**注入装载器，US-3 的规则 4 由占位转为**执行**（声明名不在池 → `tool not registered: <name>` 整只不注册）；技能面不动——**不得**预载正文、**不得**新增 `use_skill` 工具（收尾 grep 零命中） | 通过 |
| V | 审计 day-one 且 fail-closed | **适用（两条链全落地）**：工具执行先落 `pending` 再执行（预写失败 → **拒绝执行** + 回填「审计不可用」+ Run 继续）；模型调用沿用前序两阶段入口（预写失败 → **拒绝发出** + Run failed）；消息写入失败 = fail-closed（Run failed——本 US 补记，与宪法 V 同源纪律）；审计终态回填失败**不在** fail-closed 之列（记 ERROR、行留 `pending`、**不覆盖**真实结果） | 通过 |
| VI | 授权与执行分离（两接口） | **适用（唯一实现落地）**：`Policy.Check(ctx, action) error` 与 `CommandRunner.Run(ctx, cmd) (CommandResult, error)` 两接口在 `core` **声明**，`WhitelistPolicy` / `LocalRunner` 在 `tool` **实现**；文件工具只引用 `core.Policy`（编译期判据——`filetools.go` 不 import `tool` 具体类型）；调用方**不得**依赖具体实现 | 通过 |
| VII | 同步执行 + context 只承载取消 / 截止 / 追踪 | **适用（强形态）**：工具**串行**执行、无异步编排；`runID` 与 `ToolEnv` 一律**显式传参**（`ToolExecutor.Execute(ctx, env ToolEnv, toolSet []string, call ToolCall)`——裁决 ①）；`context.WithValue` **零使用**；收尾补齐用**独立有限时长** context（父上下文已取消仍必须写得进去，常量 `finishTimeout = 5 * time.Second`） | 通过 |
| VIII | 单二进制纪律 + 依赖最小化 | **适用**：**零新增坐标**（`x/sys` 复用、不引 `x/term`、不提升 `go-isatty`）；平台差异经构建约束分文件（`console_{windows,linux,darwin}.go` / `runner_{windows,unix}.go`）；发布 `CGO_ENABLED=0`；`-race` 与发布构建**分属不同命令 / job** | 通过 |
| IX | 不用 ORM，手写 SQL | **适用（本 US 首次大规模手写 SQL）**：`runs` / `messages` / `config_snapshots` / `tool_invocations` 四表读写全部手写 SQL、SQL 只出现在 `internal/storage`；**零迁移脚本**（六表 US-1 已建齐）；已发布迁移**只追加、不得改写** | 通过 |
| X | 五大能力优先，范围卡紧 | 通过——交付面对照颗粒度文档 §5 逐项边界（并行调用 / 幂等重试 / 流式 / 排队 / 总结压缩 / 容器化沙箱 / 分布式锁全不做；`shell` / `http_*` / 记忆 / MCP / 通知 / REST / 定时 / 热加载归后续 US）；对外概念零发明（导出类型与签名字面量以 §3.1 为准） | 通过 |

**架构约束**：依赖方向 `channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 是唯一允许「什么都知道」的装配层——本 US 的落法：`core` 新增 11 文件只 import 标准库与同包；`tool` 依赖 `core`（+ `x/sys`，仅在两个平台文件内）；`channel` 自声明 `Channel` 接口（消费方定义）、`app` 装配适配；`storage` 依赖 `core` + `modernc.org/sqlite`；`workspace` 依赖 `core`（**不**依赖 `tool`——门禁靠名字集注入）；`cli` 消费 `core` 与自声明端口（**不** import `workspace`）；`app/wire.go` 装配全部（取锁 → 写模式打开 → 显式迁移 → 恢复扫描 → 建注册表 → 名字集注入装载器 → 构造 `AgentService`）。无环由 CI 用 `go list` 复核。**`core` 引擎化的边界（防漂移）**：技术方案 §10.1 把 ReActLoop / PromptBuilder / ToolExecutor / AgentService 定在「核心抽象与引擎」——本 US 是该包**首次出现可执行语句**的时点，纪律实质（只依赖标准库）不变、判据由「零语句」改为「`go list` 只出标准库」（§2.4 注 11 / 已决事项 1）；第三方实现仍一律不进 `core`（SQLite 封装在 `storage`、控制台与进程在 `channel` / `tool`、cron 适配归 US-10 的独立内部包）。`evals/` 是仓库根下的又一处**消费方**（经 `app` 装配消费 `AgentService`），不构成新依赖方向。

**开发工作流**：门禁一条命令 `make check`（七目标，**`Makefile` 零改动**——`cover` 已按包名判 `core` / `tool` / `storage` ≥70%，本 US 起这三个包**首次**进入判卷；`test-integration` 无新增用例）；三条实测纪律适用（格式判卷**先退出码后输出** / `-race` 与 `CGO_ENABLED=0` **分离** / 本 US **无集成层新增**，`test-integration` 跑既有）；覆盖率不可豁免（**有可执行语句但无测试文件的包计 0%**）；提交纪律（AI **不自动** commit / push / tag）。

**plan review 必查（宪法 §开发工作流）逐条**：

| 必查项 | 本 plan 的答复 |
| --- | --- |
| 依赖方向无环 | `core` 新增 11 文件 = 标准库 + 同包（收尾 `go list -f '{{.Imports}}' ./internal/core` 判据）；`tool` / `storage` / `workspace` / `provider` 只向下依赖 `core`；`channel` 自声明接口；`cli` **不** import `workspace`（既有 `cli/ports_test.go` 禁止清单自动覆盖新命令）；`workspace` **不** import `tool`（名字集由 `app` 注入）；`app` 装配全部；`cmd/basalt` 不动 |
| 审计写入路径唯一 | 模型调用经 `provider.ProviderService`（`core` 只**声明** `LLMCallStore`；不变量 11：`grep -rn '\.BeginCall(' --include='*.go' internal/core` 零命中）；工具审计经 `storage.ToolInvocationRepo`（`core` 只声明 `ToolInvocationStore` / `ToolInvocationReader`）；审计 SQL 只出现在 `internal/storage` |
| 无框架坐标 | `go.mod` **零新增**；收尾 `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中；`use_skill` 收尾 grep 零命中 |
| 无 `context.WithValue` 业务态 | 本 US 新增 44 个实现文件逐处审查（收尾 grep 每处命中人工确认）；`runID` / `ToolEnv` / `Profile` / `Session` 一律显式类型化参数；`finishTimeout` 是常量而非包级可变状态 |
| 单二进制构建通过 | 平台分支经 build tag 分文件；`build` 目标 `CGO_ENABLED=0 go build`；`-race` 只在 `test` 目标；三平台编译命令是**人工项**（§6.1 第 5 项，**不进** `make check`） |

**Phase 1 后复评（post-design）**：设计产物（[research.md](./research.md) / [data-model.md](./data-model.md) / [quickstart.md](./quickstart.md)）完成后重跑本表——结论同前，无新增违规、无 Complexity Tracking 条目（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/004-react-loop/
├── spec.md              # /speckit-specify 输出（需求规格，含 Clarifications 会话 2026-10-16）
├── plan.md              # 本文件（/speckit-plan 输出）
├── research.md          # Phase 0 输出（决策 + 写前必须实测核实清单）
├── data-model.md        # Phase 1 输出（实体、状态机与不变量）
├── quickstart.md        # Phase 1 输出（可复制执行的验证步骤）
├── checklists/
│   └── requirements.md  # 需求质量清单（/speckit-specify 输出）
└── tasks.md             # /speckit-tasks 输出（下一步生成）
```

**`contracts/` 本 feature 不新建**（与 US-1 的差异，理由同 US-2 / US-3）：本 US 的固定字面量**已有唯一事实源**——运行面与状态机在 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md)（§1.1 请求 / 结果字段、§1.2 返回语义、§2 状态机五值、§3 并发、§4.3.1 锁、§4.3.2 六步恢复、§4.4 五分支合成文本、§5 会话标识派生、§7.2 审计四值 / §7.3 七类失败）、AGENT.md 契约在 [docs/contracts/agent-md.md](../../docs/contracts/agent-md.md)、列级事实以 [docs/contracts/schema.sql](../../docs/contracts/schema.sql) 为准；导出签名逐字来源在颗粒度文档 §3.1、「本文固定口径」九条（会话标识派生 / RunID 形态 / chat 终止语义 / 渲染模板 / `session list` 渲染 / 日志键集 / 截断标记 / `trigger` 映射 / 估算器版本与余量）在 §6.2。再抄一份到 `specs/` 就是**第二份字面量**——抄一份必漂移一处（颗粒度文档 §3.3 的同一理由：契约文件不新建，只引用）。plan 的契约节只**引用**，不复制。

### Source Code (repository root)

```text
internal/
├── core/                      # 引擎包——本 US 起首次有可执行语句（只依赖标准库）
│   ├── run.go                 # 新增：RunRequest / RunResult / RunStatus / RunEnv / ToolEnv / Budget /
│   │                          #       RunRecord / SessionRecord + NewRunID + RunEnv 结局构造与助手消息组装
│   ├── errors.go              # 改造：增 SandboxViolationError（四段式文案字段）/ AgentNotFoundError /
│   │                          #       SessionBusyError / InstanceFullError + IsSandboxViolation（不动既有哨兵）
│   ├── session.go             # 新增：Session（内存历史 + 写透仓储）+ AppendMessage / AppendToolResult /
│   │                          #       AppendSynthesizedToolMessage / CompleteToolGroup / MissingToolResults（导出）
│   ├── store.go               # 新增：消费方接口声明——MessageStore（追加接口显式承载 runID）/ RunStore /
│   │                          #       ToolInvocationStore / ToolInvocationReader / SnapshotStore / ToolResolver /
│   │                          #       Policy / CommandRunner + ToolInvocation(Result) 结构
│   ├── action.go              # 新增：Action / ActionType / Command / CommandResult（技术方案 §6.5 逐字）
│   ├── snapshot.go            # 新增：ConfigSnapshot / ManifestEntry（workspace 构造、storage 落库）
│   ├── tool.go                # 新增：Tool 四方法接口 + Result + Role 常量
│   ├── prompt.go              # 新增：Prompt / ToolSpec + PromptBuilder（四段组装 / 两级裁剪 / 准入 / 估算）
│   ├── loop.go                # 新增：ReActLoop + run(ctx, runID, env) + completeGroup（补齐失败只记 ERROR）
│   ├── tool_exec.go           # 新增：ToolExecutor + Execute(ctx, env ToolEnv, toolSet []string, call ToolCall)
│   ├── service.go             # 新增：AgentService（唯一运行入口：准入 / 会话锁 / 实例计数 / 快照 / runs 落库）
│   └── synthesis.go           # 新增：SynthesizeMissingResult + 四段补齐文本与 Reason* / estimateVersion /
│                              #       finishTimeout 常量 + TruncationMarker（补齐口径唯一份）
├── tool/                      # 沙箱与内置工具——本 US 起进入 ≥70% 覆盖率判卷
│   ├── registry.go            # 新增：Registry（显式注册 / 重名拒绝 / Names 排序 / Specs 保序 / Resolve）
│   ├── policy.go              # 新增：WhitelistPolicy.Check（授权矩阵 / 跨 Agent 授权 / 四段式文案）
│   ├── pathguard.go           # 新增：resolveRealPath / withinDir（软连接感知；不可解析 → 失败关闭）
│   ├── runner.go              # 新增：LocalRunner.Run（环境白名单 / 杀进程树 / 读取中限流 / 解释器 argv 校验）
│   ├── runner_windows.go      # 新增：Windows 分支——Job Object 挂起—关联顺序 + killTree 收尾
│   ├── runner_unix.go         # 新增：Unix 分支——进程组（SysProcAttr{Setpgid} + kill(-pgid)）
│   ├── filetools.go           # 新增：ReadFileTool / WriteFileTool / ListDirTool（写入经相对根句柄）
│   └── doc.go                 # 新增：包说明（授权矩阵与执行控制的面向实现者摘要 + 出处）
├── channel/                   # 真控制台 vs 管道分流——本 US 最大的坑
│   ├── channel.go             # 新增：Channel 接口 + Message / Reply（消费方接口）
│   ├── cli.go                 # 新增：CliChannel（交互循环 / EOF / --message 单条 / Ctrl-C）
│   ├── console_windows.go     # 新增：GetConsoleMode 判定 + ReadConsole / WriteConsole（build tag windows）
│   ├── console_linux.go       # 新增：IoctlGetTermios(TCGETS) 判定（build tag linux）
│   ├── console_darwin.go      # 新增：TIOCGETA（**独立文件**，不得与 Linux 合并写 TCGETS）
│   └── doc.go                 # 新增：包说明（编码纪律 §8.4 与平台差异登记）
├── storage/                   # 仓储与恢复——本 US 起 ≥70%
│   ├── session_store.go       # 新增：sessions / messages 仓储（seq 自增、run_id 显式入列）
│   ├── run_store.go           # 新增：runs 仓储（建 running / 终态更新全部列 / 扫 running；无 final_text）
│   ├── tool_invocations.go    # 新增：两阶段写（BeginInvocation 写 pending / FinishInvocation 更新同行）
│   ├── snapshot_store.go      # 新增：config_snapshots（insert-if-absent；UNIQUE(agent_name, hash) 冲突 no-op）
│   ├── recovery.go            # 新增：Recover 六步 ①②③④⑥（同一短事务；按位置判组调 core.MissingToolResults）
│   └── doc.go                 # 改造：事务边界纪律段（不得跨 LLM 调用与工具执行）
├── workspace/                 # 前序包改造（§3.5 第 1 / 3 项，逐条软门禁）
│   ├── context_loader.go      # 改造：增 Snapshot（同次读取解析 Profile + 段一 + 原文；哈希不含时间）
│   ├── agent.go               # 改造：AgentLoaderOptions 增 Tools []string（规则 4 由占位转执行）
│   └── recovery.go            # 新增：RecoverWorkspaceResidue（恢复步骤 ⑤：.backup / .staging 残留）
├── provider/
│   └── client.go              # 改造：出站请求体输出 tools 键（空池不带键 = 旧行为逐字不变）
├── cli/
│   ├── chat.go                # 新增：chat 命令（多轮 / --message / 60s deadline / 渲染模板 / deriveSessionID）
│   ├── session.go             # 新增：session list（只读：不取锁、不恢复、校验 schema_version）
│   ├── ports.go               # 改造：Deps 增运行面端口（RunService / SessionLister）
│   ├── root.go                # 改造：注册 chat / session 命令组
│   └── config.go              # 改造：Sandbox / Shell / HTTP 由 map[string]any 落结构化字段（键名逐字沿用）
├── app/
│   ├── wire.go                # 新增：重路径接线（取锁 → 打开 → 迁移 → 恢复 → 注册表 → 名字集 → AgentService）
│   ├── ports.go               # 改造：新增 RunService / SessionLister 适配器
│   └── run.go                 # 改造：chat / serve 走重路径；session list 走只读路径
└── （其余包不动：memory / web / testutil / cmd/basalt）

evals/                         # 仓库根新顶层目录（评测基座）
├── README.md                  # 用例格式、判定口径、运行方法、资源用量字段说明（不换算金额）
├── main.go                    # 运行器：加载 YAML → 经 AgentService 跑 → 可执行断言逐例 PASS/FAIL → 结果文件
├── cases/*.yaml               # 用例集：输入 / 夹具 / 预算 / 断言
└── injection/*.yaml           # 注入类安全用例（越权必须四段式拒绝 + 审计 failed + sandbox_denied）

basalt.yaml / basalt.yaml.example   # http 段补 allowed_domains（沿用既有键名；消费归 US-5）
Makefile                            # 零改动（cover 已按包判 core / tool / storage ≥70%）
scripts/run-evals.sh                # 可选包装脚本（go run ./evals …）；门禁化不在本 US
go.mod / go.sum                     # 零改动（本 US 零新增坐标）
```

**Structure Decision**: 单 Go module + `internal/` 分包（无多模块、无 `src/` 层）；本 US 新增实现文件 34 个、改造 10 个、新增测试文件 19 个 + 追加 2 个（21 文件 185 条），另加仓库根新顶层目录 `evals/`；**不新增 `internal/` 包**（`channel` / `tool` / `storage` 三个包 US-1 已建骨架，本 US 首次填入实现）。与台账「模块落位（摘要）」列一致：`internal/{core,tool,channel,cli,storage}`（引擎 / 沙箱与内置工具 / 终端通道 / 命令面 / 仓储与恢复）、`internal/{workspace,app,provider}`（快照构造 / 装配与端口适配 / 出站 `tools` 编解码——既有面改造）、`evals/`（评测基座）；`go.mod` 零新增坐标。前序改造点 8 项（§3.5 表）全部为**加字段 / 新增方法 / 新增文件 / 只增文字 / 键名沿用**形态——**不得**删除或改写既有字段、签名与行为；§3.5「后续占位」7 项只留装配位与文本占位，**不实现**。

**分层落位（三条包纪律的逐条落地）**：

| 纪律 | 本 US 的落法 |
| --- | --- |
| 依赖方向无环 | `core` 新增文件全部只 import 标准库（本 US 起可执行语句，判据改为 `go list`）；`tool` / `storage` / `workspace` / `provider` 只向下依赖 `core`；`channel` 自声明 `Channel` 接口；`cli` **不** import `workspace`（端口在消费方、适配器在 `app`）；`workspace` **不** import `tool`（名字集注入）；`app` 装配全部；`evals/` 经 `app` 装配消费，不形成新方向 |
| 接口定义在消费方 | `core` 声明 `MessageStore` / `RunStore` / `ToolInvocationStore` / `ToolInvocationReader` / `SnapshotStore` / `ToolResolver` / `Policy` / `CommandRunner` / `LLMCallStore`；实现分别在 `storage` / `tool` / `provider`；`channel.Channel` 与 `cli` 的运行面端口由各自消费方声明，`app` 出适配器（编译期断言） |
| 第三方实现放消费方包内 | `modernc.org/sqlite` 只在 `internal/storage`；`x/sys` 只在 `tool/runner_{windows,unix}.go` 与 `channel/console_{windows,linux,darwin}.go`；`sashabaranov/go-openai` 只在 `internal/provider`；`core` 零第三方 |

**标签口径（`[US1]` 不混淆）**：`tasks.md` 中的 `[US1]` / `[US2]` / `[US3]` / `[US4]` 是 Spec-Kit 对**本 feature 内部**用户故事的编号（对应 [spec.md](./spec.md) 的 User Story），**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）里的 US-1 / US-2 / US-3 / US-4——本 feature 的全部故事都落在项目 **US-4（ReAct 循环与首个工具闭环）** 一个模块内。四个故事 ↔ 颗粒度文档 §4.2 的十七组测试类（21 个测试文件、185 条用例，全部归属这四个故事、无剩余）的分组是本 plan 自己的记账口径，**不改动**颗粒度文档任何内容。标签按工具产出保留，不另造编号。

## Complexity Tracking

> 仅当 Constitution Check 存在需辩护的违规时填写。

无违规——本 feature 无 Constitution Check 不通过项，故无复杂度辩护条目。

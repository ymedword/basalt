# US-4 ReAct 循环与首个工具闭环 — 模块执行颗粒度文档

> **文档状态**：草案 **R1**（2026-10-16 生成；同日按评审回合修订——8 项待决**全部落案**、6 组 P1 语义冲突修正、3 项 P2 补齐；**待决事项 0 项**，见文末「评审回合裁决落案表」）。`/us-register 4` 登记、`/module-dev 4` 消费。
>
> **事实主源顺位（供 `/us-register 4` 派生登记）**
>
> 1. **`docs/TechnicalSolution.md`**（文档之间最权威）——§3.1 设计（`ProviderClient` / `ProviderService` 形状）/ §3.2 显式映射与 `basalt.yaml` 逐字样例 / §4.1 算法（核心循环**伪代码**——本文 §2.3 流程 B 逐块同构）/ §4.2 四个部件（`ReActLoop` / `PromptBuilder` / `ToolExecutor` / `AgentService`；四段提示词；运行级快照冻结）/ §4.3 边界行为（上下文超长按组丢弃、保留系统提示词与工具定义完整）/ §4.4 完整消息模型（三条硬约束；裁剪最小单位 = 完整工具交互组）/ §4.5 Run 状态机 / §4.6 并发、取消与崩溃恢复 / §4.7 执行预算与准入控制（`MaxOutputBytes` **读取中**截断）/ §6.1 工具接口（四方法**逐字**）/ §6.2 内置十工具 / §6.4 ToolRegistry（显式注册）/ §6.5 授权与执行（`Policy` / `CommandRunner` / `Action` 逐字；**授权矩阵**——跨 Agent 读取 `sandbox.cross_agent_read`、`shell.allowed_commands` 整串逐字、**不得给 `bash`/`sh` 放行**、解释器受控通道、`shell.env_passthrough`；`LocalRunner` 执行控制；四段式拒绝；`SandboxViolationError`）/ §8.4 Channel 与 CLI 交互（**编码纪律**——真控制台 vs 管道分流，平台差异 build tag）/ §8.7 CLI 命令（12 个；轻 / 重两条路；重路径接线 = 取锁 → 写模式打开 → 显式迁移 → 恢复）/ §9.1 SQLite 装配 / §9.4 审计模型 / §10.1 包结构与依赖方向（`core` = 核心抽象与引擎）/ §11.5 热加载不可变快照（"读即所记"、运行内冻结、清单哈希）/ §13 实施节奏（US-4 行：任务、可演示成果、涉及包）
> 2. **`docs/DemandAnalysis.md`**——§1.2 五大核心能力 / §5.2 一个目录 = 一个 Agent / §5.4 能力二：ReAct 循环 / §5.6 能力四：Tool 体系 / §5.7 Channel（核心阶段：CLI）/ §5.9 Session 管理 / §5.11 三种运行模式 / §5.12 命令行工具（`chat` / `session list`）/ §9.2 一次对话（含工具）/ §10.1 SQLite 表 / §10.2 文件系统数据 / §13.1 功能验收 / §13.5 运行时契约验收
> 3. **`.specify/memory/constitution.md`**（原则级最高）——I（自实现 ReAct 循环，`go.mod` **不得**出现 Agent 框架坐标）/ II（LLM 客户端只做协议调用；**不得**自动工具循环；工具调度唯一归属 `ToolExecutor`；会话历史由**一条消息仓储**承载——模型响应与工具结果由 `ReActLoop` 统一追加、用户输入由 `AgentService` 写入、崩溃 / 取消后的半截工具组由恢复路径补齐；**不得**出现第二套追加路径、**不得**重复追加）/ III（Provider 显式映射，**不得**遍历推断）/ IV（一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露）/ V（审计 day-one 且 **fail-closed**——先写 `pending` 再发调用，预写失败**不得**发出）/ VI（授权与执行分离两接口——调用方**不得**依赖具体实现）/ VII（同步执行；context 只承载取消 / 截止 / 追踪；业务状态**必须**显式传递）/ VIII（单二进制纪律；`-race` 与 `CGO_ENABLED=0` 分属不同命令）/ IX（不用 ORM，手写 SQL；已发布迁移只追加）/ X（五大能力优先，范围卡紧）+ 架构约束 + 开发工作流
> 4. **`docs/contracts/`**（**固定字面量**）——`run-and-errors.md` **全**：§1.1 `RunRequest` / `RunResult` / `RunEnv` / `ToolEnv` 字段字（**唯一事实源**）、§1.2 返回语义（**error 只用于调用方问题**）、§2 状态机、§3 并发（不排队）、§3.3 无重试无重放、§4.1 取消传播链路（**取消优先于预算判定**）、§4.2 审计与取消解耦、§4.3.1 实例锁、§4.3.2 崩溃恢复六步（步骤 3：**若仍是 `running`** 才标 `failed`）、§4.4 半截工具组补齐表（**Option A**）、§5 触发源与 RunID 规则（`<channel>-<unix_millis>-<随机 6 位>`；CLI 的 `SessionID` 由 `channel+user+profile` 派生）/ §6 五项预算 + 上下文预算 + 准入；§7.2 fail-closed 与残余窗口表；**§7.3 `error_kind` 七字面量**；§7.4 存 / 不存；`agent-md.md` §4 完整消息模型 + 裁剪样例、§5.1 FileTools 三件、§5.2 shell 与解释器受控通道、§5.3 `http_get` / `http_post`；`schema.sql` 六表 DDL（`runs` **无 `final_text` 列**；`tool_invocations` **无 `round` / `truncated` 列**，截断标记在 `result_json` 内；`config_snapshots` 主键 `id` + 唯一键 `(agent_name, hash)`，`hash = manifest 规整序列化的 sha256`，`content = AGENT.md 全文`）+ 迁移纪律（**事务不得跨 LLM 调用与工具执行**）
> 5. **既有实现事实源（本仓 US-1~US-3 已交付面；**复用不重造**）**——`internal/core/provider.go`：`Message` / `ToolCall` / `Usage` / `ChatRequest` / `ChatResponse` / `ProviderClient` / `LLMCallStatus`（pending / success / failed / unknown）/ `ErrorKind`（七值）/ `LLMCall` / `LLMCallResult` / `LLMCallStore` **原样复用**；`internal/core/errors.go` 既有哨兵；`internal/provider/service.go`：`ProviderService.Chat` 已是"路由 + 两阶段审计"的**唯一 LLM 调用入口**（三道拒绝在预写之前；`finish()` 是终态回填唯一出口——回填失败记 ERROR、行留 `pending`、**不覆盖**调用结果）；`internal/workspace/{agent,context_loader}.go`：`AgentLoader` 与 `SectionOne`（其注释已登记"快照冻结与清单哈希归 US-4""注入内容与哈希必须共享同次读取产物"）；`internal/cli/config.go` 的 `budget` 四键与 `sandbox` / `shell` 段；`basalt.yaml.example` 与 `docs/Operations.md` §2.2（配置键与"改完需重启"清单）
> 6. **附加事实源**——`docs/Compatibility.md` §5.1 共享验收样例（10 个工具声明与六条判据——判据 2 在本 US 由 Registry 注入名集兑现）/ §5.3 判据 3（`reasoning_content` 清空后**必须**复现 400）；`docs/EngineeringStandards.md` §1 Go / §2 测试 / §4 日志与错误 / §5 提交；`docs/AiProgrammingGuide.md` §4.4（US-4 完整定义：**沙箱正式实现非桩**、涉及包、规模参考、硬依赖 US-3）/ §4.5（US-5 = ShellTools / HttpTools / `tool list`，**不含**沙箱实现）/ §7 陷阱表（#22 / #23 / #24 / #25 / #27 / #29 / #30 / #31 / #33 / #34 / #35 / #37 / #39）/ §8.3 治理
> 7. **落地参考（非权威）**——`docs/Roadmap.md` §4（US 开放序列）；`docs/GettingStarted.md` §2（工具链读数）
>
> **固定裁决句**：本文与最新技术方案冲突时，**以技术方案为准**；本仓配套工件（spec / plan / 已批准 US 清单）已锁定的引用在不与前者冲突时沿用，不另行改判。**契约字面量一律服从契约**——审计状态集、`error_kind` 七值、Option A 五条合成文本、四段式拒绝格式，本文明文冻结的部分只在契约留白处生效。**spec 锚点现状（核对于 2026-10-16）**：`specs/` 下现有 `001-foundation/` / `002-provider/` / `003-agent-profile/`（US-1~US-3 已完成验收的历史设计，**只读**）；US-4 的 spec 目录由 `/module-dev 4` 新建。
>
> **登记派生信息**：技术方案章节 = §3.1 / §3.2 / §4.1 / §4.2 / §4.3 / §4.4 / §4.5 / §4.6 / §4.7 / §6.1 / §6.2 / §6.4 / §6.5 / §8.4 / §8.7 / §9.1 / §9.4 / §10.1 / §11.5 / §13；需求文档章节 = §1.2 / §5.2 / §5.4 / §5.6 / §5.7 / §5.9 / §5.11 / §5.12 / §9.2 / §10.1 / §10.2 / §13.1 / §13.5；模块落位（摘要）= `internal/{core,tool,channel,cli,storage}`（技术方案 §13 US-4「涉及包」列）+ `internal/workspace`（快照构造与工作区残留判定——§2.4 注 2）+ `internal/app`（装配与端口适配器改造）+ `internal/provider`（出站 `tools` 编解码改造）+ `evals/`（评测基座，**仓库根新顶层目录**——§2.4 注 10）；`go.mod` **零新增坐标**（§3.3）。
>
> **台账状态**：登记时 `docs/us/REGISTRY.md` 查无 US-4 行——本章节对照由本 skill 自定位，供 `/us-register 4` 派生。

## 1. 模块概述

**定位**：让 Basalt 第一次**自己跑起来**——`AgentService.Run(ctx, RunRequest)` 作为唯一运行入口，驱动自实现的 `ReActLoop` 完成"模型 → 工具调用 → 工具结果回填 → 再问模型"的闭环；工具经 `ToolExecutor` 调度、由 `WhitelistPolicy`（授权，看"谁要碰什么"）与 `LocalRunner`（执行，受控子进程）把关；每一步 LLM 调用与每一次工具执行**先落 `pending` 审计再发出去**；`basalt chat` 在真控制台上多轮对话、会话历史落 SQLite 跨重启可续。

**价值（WHAT / WHY）**：US-4 是全项目**唯一保留的大 US**（再拆就会出现"有循环没工具"或"有工具没人调"的半截状态，编程指南 §4.4），一次交付七件可判卷的事：① **ReAct 循环本体**——宪法 I 要求循环体是本仓代码，本 US 就是那段代码（`go.mod` 全程零 Agent 框架坐标）；② **首个工具闭环**——`read_file` 真的能读工作区文件并把内容喂回模型；③ **沙箱从条文变成句柄**——`WhitelistPolicy` + `LocalRunner` 是**正式实现**（不是桩；ShellTools / HttpTools 归 US-5）；④ **审计 fail-closed 落地**——LLM 与工具两条审计链各自两阶段写、预写失败即拒发（宪法 V 从条文变成可测断言）；⑤ **消息模型全链保真**——`tool_call_id` / `reasoning_content` / `ProviderExtra` 三条硬约束贯通到出站编码；⑥ **会话持久化与崩溃恢复**——重启后会话可续、半截工具组按位置补齐（契约 §4.4 Option A："合成结果只进 `messages` 不进审计"）；⑦ **行为评测基座**——`evals/` 首批固定用例集跑通，工具选择 / 业务结果 / 越权尝试 / 资源用量四类读数落到结果文件。规模参考：约 25~30 个文件（编程指南 §4.4 的规模参考；本 US 实际交付面见 §3，略多于该参考——沙箱与评测基座同批交付所致）。**硬依赖 US-3（已交付）**——`AgentLoader` 产出的 `Profile` 与 `ContextLoader`（段一）是本 US 的输入面；本 US 不消费其他未交付能力（记忆注入归 US-6、MCP 归 US-7、ShellTools / HttpTools 归 US-5）。

## 2. 设计要点与约束

### 2.1 职责边界表

| 维度 | 本 US 职责（做） | 非本 US 职责（不做，归属） |
| --- | --- | --- |
| 运行入口 | `core.AgentService`：**唯一入口** `Run(ctx, RunRequest) (RunResult, error)`；准入（同会话串行 / 实例级上限）与运行级编排；用户输入消息由它写入仓储 | REST / 管理台补跑入口与 409 / 429 的 HTTP 面（US-9）；定时触发（US-10） |
| 循环 | `core.ReActLoop`：迭代 → 组装 → 调用 → 落库 → 有工具则执行 → 无工具则 `complete`；迭代 / 预算 / 取消三类终止的返回语义 | 异步编排（宪法 VII 明禁）；流式响应（核心阶段不做）；Agent 间委托（核心阶段不做） |
| 提示词 | `core.PromptBuilder`：四段组装（段 1 = 运行级快照的段一正文；段 2 记忆注入**留空占位**；段 3 对话历史，按**轮数** + **完整工具交互组**两级裁剪；段 4 工具列表）；上下文准入（不可裁部分超限 → 拒绝启动） | 记忆召回注入（US-6）；技能 L2 / L3 正文注入（由模型经 `read_file` 自取） |
| 工具调度 | `core.ToolExecutor`：工具名解析 + **运行时 ToolSet 归属校验** → 审计 `pending` 预写（**fail-closed**）→ 工具执行 → 审计终态回填 → 结果包装与失败回填；`Session.CompleteToolGroup` 半截组补齐 | 工具本体其余七件（US-5 / US-7 / US-8 / US-10）；MCP 客户端（US-7） |
| 沙箱 | `tool.WhitelistPolicy`（授权判定 + 四段式文案）+ `tool.LocalRunner`（受控子进程：环境变量白名单、超时杀进程树、输出读取中限流） | 容器化隔离（扩展阶段 F）；HTTP 逐跳校验的**驱动**（US-5 的 `http_get` / `http_post`） |
| 内置工具 | `tool.FileTools` 三件：`read_file` / `write_file` / `list_dir`（契约 §5.1 参数与失败类别逐字） | `shell` / `http_get` / `http_post` / `notify` / `manage_schedule`（US-5 / US-7 / US-8 / US-10）；`use_skill` 宪法 IV **永久禁** |
| 消息与会话 | 既有 `core.Message` 完整模型（复用）；`core.Session` 内存历史 + 写透仓储；`storage` 的 `sessions` / `messages` 仓储；`session list` 命令 | 会话删除 / 归档（US-9 / US-12）；记忆压缩 / 总结（核心阶段不做） |
| 审计 | 工具链两阶段写（`tool_invocations` 仓储）+ `runs` 自本 US 起写入 + `config_snapshots` 落库；LLM 链**调用既有** `ProviderService.Chat`（不另写一套） | 审计查询端点与界面（US-9 / US-12） |
| 渠道 | `channel.CliChannel`：真控制台经**控制台 API**、管道 / 重定向两侧显式 UTF-8；`IsTerminal` 判定（平台分别适配） | `web` 渠道（US-9）；其他渠道（扩展阶段） |
| 崩溃恢复 | 六步扫描（①②③④⑥ 在 `storage`、⑤ 工作区残留判定在 `workspace`），`app` 取锁后编排；半截组**按位置**判组、补齐与 Run 收尾**同一短事务** | 热加载 watcher 与原子发布（US-11）；`.staging` 多文件提交（US-12，本 US 只处理残留） |
| 行为评测 | `evals/` 用例集 + 运行器（默认 fake 可重复；真模型显式门控；可执行断言逐例判卷；结果文件含 RunID 与审计关联） | 评测的 CI 门禁化与历史基线对比（本 US 只要求跑通并留读数） |
| 包与装配 | `internal/core` **首次出现可执行语句**（引擎包）；`internal/tool` / `internal/channel` 新实现；`internal/app` 装配与端口适配 | `channel` / `web` 反向依赖 `core`（违 §10.1 依赖方向）；`core` import 任何第三方包 |

### 2.2 关键约束清单（每条对应宪法条款 / 契约，带症状或判据）

1. **自实现循环 + 无框架坐标**（宪法 I）：`ReActLoop` 是**本仓库代码**；`go.mod` **不得**出现 Agent 框架坐标（`eino` / `langchaingo` / `adk-go` / `trpc-agent-go` 等）；协议 SDK（`go-openai`）**不得**接管循环或自动执行工具——它只做协议调用（宪法 II）。判据可执行：收尾 `grep -nE 'eino|langchaingo|adk-go|trpc-agent-go' go.mod` 零命中。**注意**：`go-openai` 在本项目**只用于协议类型与已知字段编解码**，HTTP 往返自建（`net/http`，一次 POST、**不重试**、**不跟随重定向**——约束 20）。
2. **工具调度唯一归属 `ToolExecutor`**（宪法 II）：工具**只能**由 `ToolExecutor` 调度；`provider` 层与 `ReActLoop` **不得**直接执行工具；`go-openai` 的任何"自动工具循环"能力**不得**启用。症状：循环里出现"模型返回工具调用 → 循环自己 exec"的分支 = 违约束。
3. **消息追加唯一路径**（宪法 II）：正常运行中**模型响应与工具结果由 `ReActLoop` 统一追加**、**用户输入由 `AgentService` 写入**、崩溃 / 取消后的半截工具组由**补齐路径**（`ReActLoop → Session.CompleteToolGroup` / 恢复扫描）写入——三者都经 `Session` 的写透接口，**不得**出现第二个消息出口。**`ToolExecutor.Execute` 只返回 `Result`，不写消息**（同处禁止第二套追加路径）；`complete` 只组装 `RunResult`、**不写库**（assistant 消息已在追加点落过一次）。**留痕顺序**：assistant 消息（带 `tool_calls`）**先**落库、**再**执行工具（契约 §4.3.2 的半截组判据依赖这个顺序）。
4. **LLM 调用的审计归属 `ProviderService.Chat`**（宪法 II/V + 既有实现事实）：LLM 链的"路由查表 + 两阶段审计 + 终态回填"**已由 `ProviderService` 交付**（US-2），`ReActLoop` **必须经 `core.ProviderClient` 接口调用它**——**不得**在循环里自己写 `pending` / 回填终态（第二套审计 = 重复行 / 双写冲突）。**回填语义沿用既有唯一出口**：预写失败 → `ProviderService` 拒绝发出（该错误上抛 → Run 以 `failed` 收场）；调用发生后的终态回填失败 → 记 ERROR、行留 `pending`、**不覆盖**真实调用结果。判据可执行：`grep -rn 'BeginCall' internal/core` 零命中（core 不得直呼审计仓储）。
5. **审计口径冻结：状态四值 + 失败七类**（契约 §7.2 / §7.3，**服从契约、不得自造**）：`llm_calls.status` 与 `tool_invocations.status` ∈ `pending` / `success` / `failed` / `unknown`——**没有 `cancelled`**；`error_kind` ∈ `sandbox_denied` / `timeout` / `not_found` / `invalid_input` / `external_error` / `runtime_error` / `unknown`（七类**逐字**，取值口径见 §3.4）。**Run 状态是另一套**（`running` / `completed` / `limit_reached` / `cancelled` / `failed`，契约 §2）——两套字面量**不得混用**。**Run 可以 `cancelled`，审计仍按既有口径记 `failed` 及分类**（ctx 错误归 `timeout`——与 `ProviderService.classifyError` 同口径）。
6. **授权与执行分离（两接口）**（宪法 VI + 技术方案 §6.5）：`Policy.Check(ctx, action) error` 与 `CommandRunner.Run(ctx, cmd) (CommandResult, error)` 两接口；**调用方不得依赖 `WhitelistPolicy` / `LocalRunner` 等具体实现**——接口定义在消费方（`core`）、实现在 `tool`、装配在 `app`。**调用点在工具内部**（§2.4 注 1 已裁决）：工具解析入参后构造 `Action` / `Command` 并调用接口；**文件工具只需 `Policy`**（读 / 写不启子进程），**命令执行才需 `CommandRunner`**；解释器 argv 形态校验属**执行侧**（`LocalRunner`）。`Action{Type, Target, Agent}` 语义**逐字**——"授权只看它（谁、要碰什么），**不看打算怎么做**"：`Action` **没有 argv**，`Policy` **不得**检查 `arguments[0]` 之类的入参形状。
7. **Provider 显式映射**（宪法 III）：模型客户端实例在装配处一次构建为 `provider name → 客户端实例` 映射表；运行时只查表，**不得**遍历推断。本 US 经运行级快照的 provider 名查表取客户端（查表动作在 `ProviderService` 内），**不得**按模型名猜 provider。
8. **同步执行 + context 纪律**（宪法 VII）：**不得**引入异步编排（无 goroutine 池、无 channel 编排）；`context.WithValue` **只允许**追踪标识（本 US 的追踪标识 = `run_id`）；`Profile` / `Session` / `ToolEnv` / `now time.Time` 等业务状态**必须**显式传递（`RunEnv` 是它们的载体）；**不得**用包级变量模拟 ThreadLocal。`now time.Time` 显式传入，**不得**在函数内取真实当前时间（唯一例外：`main` 入口取一次并逐层下传）。判据可执行：收尾 `grep -rn 'context.WithValue' --include='*.go' cmd internal` 命中数只落在追踪标识处。
9. **单二进制与依赖最小化**（宪法 VIII）：`go.mod` **本 US 零新增坐标**（§3.3 逐条核实）；`core` **不得** import 任何第三方包（`go list -f '{{.Imports}}' ./internal/core` 只出标准库——本 US 起 `core` 有可执行语句，判据从"零语句"升级为"语句只依赖标准库"）；`-race` 与 `CGO_ENABLED=0` **分属不同命令 / job**。
10. **不用 ORM，手写 SQL**（宪法 IX）：四个新仓储全部手写 SQL；**已发布迁移只追加、不得改写**（本 US **零迁移**——六表 US-1 已建齐，§3.4）；`schema_version` 打开库前必校验；**事务不得跨 LLM 调用与工具执行**（契约迁移纪律第 7 条：`pending` 预写、LLM 往返、终态回填是**三个独立短事务**——单事务跨越会让写池（max 1）在整个模型往返期间被独占）。
11. **范围卡紧**（宪法 X）：核心阶段范围以需求文档 §6 与已批准 US 清单为准；本 US 的"不做"逐条登记在 §5（工具并行调用 / 幂等键 / 流式 / 准入排队 / 上下文总结压缩均在契约 §3.3 / 技术方案 §7.5 的"核心阶段不做"内）。
12. **运行级快照冻结 + 读即所记**（技术方案 §4.2 / §11.5 + 契约 DDL）：配置快照**每次 Run 冻结**（Run 内不变，中途改盘不换配置）；**Profile 与段一正文从同一份原始 `AGENT.md` 解析**（**不得**混用加载期的 `entry.Profile` 与新读的正文 / 哈希——三者必须同源同次）；原始 `AGENT.md` 全文保留供 `config_snapshots.content`；`manifest` 覆盖段一注入源（AGENT.md + bootstrap + 绑定技能 `SKILL.md`），`hash = manifest 规整序列化的 sha256`（排序确定、**当前时间不得混入**——内容相同即同一版本，`now` 只影响 `created_at` 列）；任一清单文件读失败 → **该次 Run 拒绝启动**（不得降级为部分清单）；快照**在 Run 开跑前先落 `config_snapshots`**（insert-if-absent），落库失败 → 拒绝启动（零 LLM 调用、零工具执行）。L2 / L3 技能正文与 `scripts/` **不在**冻结范围。**与 US-3 的接口**：`SectionOne` 的"每次调用重读"说的是**组装器无缓存**；本 US 的快照是**运行级冻结**——一次 Run 内读一次并冻结，Run 之间各自重建（热加载归 US-11，本 US **不得**引入 watcher）。
13. **四段提示词与工具池**（技术方案 §4.2 + 契约 §2）：段 1 系统提示词（快照的段一正文，末尾含当前日期时间）/ 段 2 记忆注入（**本 US 恒空**，US-6 接入）/ 段 3 对话历史 / 段 4 工具列表（Function Calling 格式）。**历史裁剪两级**：① 轮数——保留最近 `max_history_turns` **轮**（轮 = 以 user 消息为界；缺省 20，Profile.settings 可覆盖），**不得**按消息条数计算；② token——仍超预算则按**完整工具交互组**从最老处继续丢弃（可跨轮，某轮可能只剩 user 消息），**不得**拆散任何一组。工具池 = 快照 Profile 的 `tools:` 声明，**不声明 = 空池**（陷阱 #9：**不得**把"未声明"当作"全部可用"）。
14. **上下文准入与 token 估算**（契约 §6 上下文预算）：**不可裁剪部分** = 系统提示词（段 1 + 段 2）+ 工具定义（段 4）+ **当前消息**；裁剪到只剩不可裁部分仍超 `budget.context_tokens` → **拒绝启动**（Run `failed` + Reason 点名 `budget.context_tokens` + **零 LLM 调用**）。估算器**版本明确**（`EstimateVersion` 常量）、**覆盖完整出站字段**（段 1 / 段 2 / 历史各字段含 `tool_calls` 与 `reasoning_content` 与 `provider_extra` / 段 4 的 name + description + JSON Schema），并用**中英文、代码、工具 schema、工具参数、扩展字段**五类夹具标定（§4.3）；**不得**以"固定比率即保守上界"的说法宣称不会低估——估值口径见 §2.4 注 9。
15. **完整消息模型三条硬约束**（技术方案 §4.4 + 契约 `agent-md.md` §4）：① `tool_call_id` **原样回传**（不得改写 / 重编号）；② `reasoning_content` **必须回传**（否则供应商 400 且会话不可恢复——Compatibility §5.3 判据 3 钉住该形态）；③ `ProviderExtra` **原样透传**（既有类型 `map[string]any`，**不改类型**；经两跳保真，`UseNumber` 保大整数）。`ToolCall{ID, Name, Arguments}` 的 `Arguments` 是 **JSON 字符串原样保存**（**不得**解析后重序列化——重序列化会改键序与数字形状）。**兜底写清**：带工具调用的响应 `content` **可能为空**、`usage` **可能为 null**——循环与回填逻辑**必须**容忍；`usage` 缺失时 token 计数记 0，**不猜**。
16. **工具接口四方法冻结**（技术方案 §6.1，**逐字**）：`Name() string` / `Description() string` / `InputSchema() json.RawMessage` / `Execute(ctx context.Context, env ToolEnv, input json.RawMessage) (Result, error)`；`Result{Content string; Retryable bool}`；`env` 是**值传递必填**参数；无 Agent 上下文时 `AgentName` / `AgentDir` 是**空串**（不是异常）。**字段字唯一事实源是契约 §1.1 的 `ToolEnv`**——**不得**加字段（加字段 = 改契约，须先裁决）。
17. **沙箱授权矩阵与执行控制**（技术方案 §6.5 + 契约 §5.2）：**文件读** = 当前 Agent 目录（含 `scripts/`）+ `.basalt/skills`；**跨 Agent 读取默认拒**，须 `sandbox.cross_agent_read` **显式列出**目标 Agent 名（缺省空 = 一个都不许；拒绝文案点名该键）；目标**先投影真实路径**（软连接感知），不可解析 → **失败关闭**。**文件写** = 当前 Agent **自有目录**，且**保护规则先于一般放行**（`AGENT.md` 与受保护配置、`.basalt/skills`、`scripts/` 逐条先拒）；`memory/` **不在**白名单；**不得**放宽为 `.basalt` 整根；**父目录必须已存在**（**不**自动创建）。**命令** = `executable` 与 `shell.allowed_commands` **整串逐字**比对、多词串**必拒**、**`bash` / `sh` 一律硬拒（无放行分支）**；解释器受控通道（`shell.interpreters`）由**执行侧**校验：argv 必须 `[<脚本路径>, 参数…]`、`arguments[0]` **不得**以 `-` 开头、脚本真实路径**必须**落在该 Agent 的 `scripts/` 或 `.basalt/skills/` 内。**HTTP** = 域名白名单（**键名 `http.allowed_domains`**，大小写不敏感）+ scheme ∈ {http, https}（代码固定）+ **逐跳**校验（客户端**不得**自行跟随重定向）+ 跳数上限 **5（代码常量，不得放宽、不提供配置键）** + IP 字面量直接拒 + **不读环境代理变量**（`Transport.Proxy` **恒 nil**）。**执行控制**：子进程环境变量**白名单**（不继承全环境，只透传 `shell.env_passthrough` 列出的变量 + 工具显式声明的）、超时**杀进程树**、输出**读取中限流**（`io.LimitReader`，尊重更小的配置上限）。
18. **单工具超时与 Run 超时分离**（契约 §6 + 本回合裁决 ⑤）：`ToolEnv.Timeout = min(30s 缺省, 剩余 Run 预算, 工具请求的更小值)`，并**实际建立超时 context**；**单工具超时 → 该工具 `failed` + `error_kind=timeout`，Run 继续**（模型看得到失败，可换路）；**Run 自身取消或截止 → `cancelled`**。**本 US 不新增 Profile 键**（契约 §2 字段表无 `tool_timeout`——不发明）。CLI 的 Run 时限 = **60s**（调用方已有更早 deadline 时取更早者；Ctrl-C 是取消路径，**不能替代**资源时限）。
19. **四段式拒绝是模型可见契约**（技术方案 §6.5，**逐字**）：`<规则>：<越界值>。这是安全策略，请勿反复重试；<放行指引：配置键 + 需重启>`；沙箱拒绝抛 `SandboxViolationError`（专用错误类型）→ 走 `ToolExecutor` 既有失败审计路径（`failed` + `error_kind=sandbox_denied`）；**拒绝信息原样进工具结果**（**不得**吞掉、**不得**替换为泛化文案——模型看不到"为什么被拒"就不会收敛）。**`sandbox.cross_agent_read` 的拒绝文案与白名单拒绝文案分别点名各自的配置键**（Operations §2.2 的"改完需重启"清单）。
20. **出站 HTTP 纪律**（技术方案 §3.1 + 集成约束）：一次 POST、**不重试**、**不跟随重定向**（`CheckRedirect` 返回 `http.ErrUseLastResponse`）；凭证只从 `${ENV_VAR}` 解析后的内存值取；**不得**把凭证写进日志 / 审计 / 错误文案。
21. **CLI 编码纪律（本 US 最大的坑）**（技术方案 §8.4）：真控制台经**控制台 API**（Windows `ReadConsoleW` / `WriteConsoleW`；类 Unix UTF-8 直读），管道 / 重定向两侧**一律显式 UTF-8**；判定用 `IsTerminal`，**不得**按终端名或环境变量猜；实现归 `internal/channel`，平台差异 build tag **按平台分别适配**：Windows `windows.GetConsoleMode`；**Linux `unix.TCGETS`、Darwin `unix.TIOCGETA`（常量不同，不得统一写一个）**；非终端分支一律 UTF-8 直读 / 直写。**目标平台编译检查必做**（§3.3 命令：三个 `GOOS` 各交叉编译一次）。症状：中文输入回显乱码 / 管道输入被当作控制台 / Darwin 上 `IsTerminal` 恒假 = 违约束。
22. **包与依赖方向**（宪法·架构约束 + 技术方案 §10.1）：`channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 是**唯一**装配层；**接口定义在消费方**。**`core` 只依赖标准库**。**本 US 是 `internal/core` 从"零可执行语句"变为"引擎包"的时点** → `core` 自本 US 起进入覆盖率 ≥70% 判卷（`scripts/check-coverage.sh` 已按包名判，**无需**改脚本）；`tool` 同时首次进入 ≥70%，`storage` ≥70%，`channel` ≥50%。
23. **崩溃恢复六步（全幂等）**（契约 §4.3.2 + §4.4）：① `pending` → `unknown`（两表都扫）；② 半截组扫描**按位置**（assistant 带 `tool_calls` + 同一会话 `seq` **紧邻连续**的 tool 段——**不得**全会话按 `tool_call_id` 查找，否则会把"跨 Run 同 id 调用"误配）；③ 组修复与 Run 状态修复**同一短事务**（Run **若仍是 `running`** 则标 `failed` + `reason = '进程中断（启动时发现未完成的运行）'` + 写 `finished_at`——**不得覆盖已结束状态**）；④ 补组后**另扫** `runs` 收尾其余仍是 `running` 的行（**组看消息、Run 看表**）；⑤ 工作区替换残留（`.backup` 判定，回滚 / 前滚）；⑥ INFO 日志 + 启动摘要。**只有持工作区实例锁的进程做恢复**；只读命令（`init` / `profile list` / `profile show` / `session list`）**不取锁、不做恢复、不得清理锁文件**，打开库前**必须**校验 `schema_version`。
24. **半截组补齐的三条纪律**（契约 §4.4）：① **先判缺失**——判据是"assistant 的 `tool_calls` 里每个 id 是否已有结果"，**落在消息上**（`core.MissingToolResults` 单实现，取消路径与恢复路径共用），**再**按审计行分类合成文本（分类逻辑同样单实现：`core.SynthesizeMissingResult`）；② **不得把执行过的写成"未执行"**——有终态行 → 还原真实结果；`pending` / `unknown` 行 → "结果未知"文本（**逐字**取契约）；无行且取消 → "已取消"文本；无行且预算耗尽 → "未执行（工具调用次数超限）"；③ **补齐与 Run 收尾使用独立收尾 context**（`context.WithoutCancel` + 有限超时，缺省 5s）——调用时 `ctx` 往往已取消，收尾动作**不得**依赖它；`Session.CompleteToolGroup` **必须返回 error**（消息写失败要能传达：调用方记 ERROR + Run 照常落终态，半截组留给下次启动的恢复扫描）。
25. **轻 / 重路径接线**（技术方案 §8.7）：重命令（涉及注册表或数据库）走完整装配——**取锁 → 写模式打开 → 显式迁移 → 恢复**；`chat` 属**写模式**命令（取实例锁）。轻命令（`init` / `profile list` / `profile show` / `session list`）**不取锁**。
26. **可观测性双轨 + 文案脱敏**（工程规范 §4）：审计落库 ≠ 运行日志——**失败路径两者都要**（审计落 `failed` + WARN 日志，固定键）；工具执行失败 WARN、审计写失败 ERROR、恢复动作 INFO；日志**不得**打印消息全文 / 工具结果全文（只记长度 / 截断标记 / `run_id` / 工具名）；**不得**回显 `${ENV_VAR}` 解析后的明文。

### 2.3 核心逻辑

本 US 核心逻辑为**七条流程**：A 运行编排（`AgentService.Run`）、B 循环（`ReActLoop.run`，与技术方案 §4.1 伪代码**逐块同构**）、C 提示词组装（`PromptBuilder.Build`）、D 工具调度（`ToolExecutor.Execute`）、E 半截组补齐（`Session.CompleteToolGroup`，取消路径与崩溃路径**共用同一张文本表**）、F 授权判定（`WhitelistPolicy.Check`）、G 崩溃恢复扫描（六步）。伪代码方法名与 §3 交付物清单**逐字一致**。

#### 流程 A — 运行编排（`internal/core`，`AgentService.Run`）

```text
AgentService.Run(ctx, req)                  # 契约 §1.1 的 RunRequest（Agent / SessionID / Message / Channel / UserID / Budget / ConfigHash）
  │
  ├─ ① 准入判定（**调用方问题 → 返回 error**，契约 §1.2）
  │     ├─ 未知 Agent 名 → 错误（映射 404）
  │     ├─ 会话锁（进程内 sync.Mutex，键 = session_id）被占 → 错误（**不排队**，映射 409）
  │     └─ 实例级在跑数 ≥ max_concurrent_runs（缺省 32）→ 错误（**不排队**，映射 429）
  │     ※ session_id 不在本函数派生：命令面按规范元组编码算好再传入（§6 固定口径 1）；
  │       req.SessionID 为空 = **无状态调用**——仅内存临时会话、不落会话历史，但**仍落 Run 与审计**（契约 §1.1 / §5）
  │
  ├─ ② 取运行级快照：workspace.Snapshot(root, entry, now)
  │     ├─ 同一份原始 AGENT.md → 解析 Profile + 段一正文 + 保留原文（内容寻址）
  │     ├─ 任一清单文件读失败 → 拒绝启动（status=failed、reason 点名路径、零 LLM 调用、零工具执行）
  │     └─ 先落 config_snapshots（insert-if-absent；UNIQUE(agent_name, hash)）——**落库失败同样拒绝启动**
  │
  ├─ ③ 取会话：有 session_id → 按 id 载入历史（不存在则建行）；无 session_id → 仅内存会话
  ├─ ④ 组装 RunEnv{Req, Snapshot, Session, Budget, ToolSet}（ToolSet = **Snapshot.Profile.Tools**）
  │     └─ Budget = RunRequest.Budget 与 Profile.settings.max_iterations 合并后的实际预算（Profile 只能收紧迭代上限）
  │
  ├─ ⑤ 预写 runs 行：status=running、started_at=now、trigger=按 Channel 映射（§6 固定口径 8）、config_hash=Snapshot.Hash
  │     └─ **写失败 → 直接 error**（运行账本不落下就不开跑——与 fail-closed 同源纪律）
  ├─ ⑥ 用户输入消息写进消息仓储（role=user、run_id=本次 Run；**无状态会话跳过**）
  │     └─ **写失败 → 终态 failed + 错误返回**（宪法 II：用户输入的唯一写入点是这里）
  │
  ├─ ⑦ result, _ = ReActLoop.run(ctx, runID, env)         # 流程 B（TS §4.1 的签名 + 显式 runID——§2.4 注 12；error 恒 nil，运行期失败归 Status）
  ├─ ⑧ 终态落库：runs.status / reason / iterations / tool_calls / finished_at
  │     └─ 写失败 → 记 ERROR（**不覆盖**已得结果：结果如实返回，行停 running 待下次启动收尾）
  └─ ⑨ 返回 RunResult{RunID, Status, FinalText, Iterations, ToolCalls, ConfigHash, Reason}
        ※ **`runs` 表没有 final_text 列**——最终回答只存在 RunResult 与会话里的 assistant 消息中
```

#### 流程 B — 循环（`internal/core`，`ReActLoop.run`；与技术方案 §4.1 伪代码逐块同构，差异逐处列在 §2.4 注 12）

```text
ReActLoop.run(ctx, runID, env) → (RunResult, error)   # env *RunEnv（Snapshot / Session / Budget / ToolSet）
  │
  ├─ 每轮（round = 1 … env.Budget.MaxIterations）：
  │     ├─ ① 轮首查取消：ctx.Err() != nil → cancelled（Reason = ctx 原因）
  │     ├─ ② **轮内**组装完整请求：prompt, err = PromptBuilder.Build(env)    # 流程 C
  │     │      ├─ 配置快照每 Run 冻结；**每轮的请求都重新组装**（上一轮追加的工具结果必须进本轮 history）
  │     │      └─ 不可裁部分仍超 context_tokens → err → Run failed（点名 budget.context_tokens，零 LLM 调用）
  │     ├─ ③ resp, err = client.Chat(ctx, env.llmEnv(runID, round, prompt))
  │     │      ※ 路由 + 两阶段审计（pending 预写 / 终态回填）在 ProviderService 内（约束 4）——循环不碰审计仓储
  │     │      ├─ err != nil：**先看 ctx.Err()**——非 nil → cancelled；nil → failed（Reason = 错误摘要）
  │     ├─ ④ env.Session.AppendMessage(ctx, runID, env.assistantMessage(resp))   # **先落 assistant**
  │     │      └─ 写失败 → failed（不得先执行工具再补记——恢复扫描依赖此顺序）
  │     ├─ ⑤ resp.ToolCalls 为空 → **complete**（Status=completed，Iterations=round，FinalText=resp.Content）
  │     │      ※ 首次纯文本完成时 round=1 → **Iterations = 1**；complete 只组装 RunResult，**不写库**
  │     └─ ⑥ 逐个（**串行**）执行工具调用——**每次执行之前**查取消、查工具预算：
  │            ├─ ctx.Err() != nil → Session.CompleteToolGroup(本轮整组, 取消) → cancelled → 返回
  │            ├─ env.Budget.ToolCallsExhausted(toolCalls) → Session.CompleteToolGroup(预算) → limit_reached → 返回
  │            ├─ result = ToolExecutor.Execute(ctx, env.ToolEnv(ctx, runID), env.ToolSet, call)  # 流程 D（**返回结果，不写消息**）
  │            └─ env.Session.AppendToolResult(ctx, runID, call, result)        # **Loop 是唯一追加者**
  │            ※ 工具预算耗尽**不**禁止"不使用工具的模型响应"——预算只在"要执行工具之前"判（见 ⑤）
  └─ 循环外：Session.CompleteToolGroup（防御性 no-op——正常路径每组都已回填）→ limit_reached（"超过最大迭代次数"）
        ※ 迭代耗尽发生在**所有工具结果都已追加之后**，故无半截组可补；Runtime 上限见契约 §2 状态机
```

```text
func (l *ReActLoop) run(ctx context.Context, runID string, env *RunEnv) (RunResult, error):
    toolCalls := 0
    for round := 1; round <= env.Budget.MaxIterations; round++ {          # round 从 1 起（契约 §7.1 的 round 语义）
        if ctxErr := ctx.Err(); ctxErr != nil {
            return env.cancelled(ctxErr, round-1, toolCalls), nil         # 轮首取消：本轮的模型调用未发出，不计
        }
        prompt, err := l.builder.Build(env)                               # 轮内组装：冻结快照 + 裁剪后的历史
        if err != nil {
            return env.fail(err, round-1, toolCalls), nil                 # 不可裁部分超限 → 零 LLM 调用
        }
        resp, err := env.client().Chat(ctx, env.llmEnv(runID, round, prompt))  # 路由 + 两阶段审计在 ProviderService 内
        if err != nil {
            if ctxErr := ctx.Err(); ctxErr != nil {                       # **先看 ctx.Err()**：取消不是失败
                return env.cancelled(ctxErr, round-1, toolCalls), nil
            }
            return env.fail(err, round-1, toolCalls), nil
        }
        if err := env.Session.AppendMessage(ctx, runID, env.assistantMessage(resp)); err != nil {
            return env.fail(err, round, toolCalls), nil                   # **先落 assistant 再执行工具**
        }
        if len(resp.ToolCalls) == 0 {
            return env.complete(resp.Content, round, toolCalls), nil      # 首次完成 = 1 轮；只组装、不写库
        }
        for _, call := range resp.ToolCalls {                             # 串行；**每次执行之前**两步检查
            if ctxErr := ctx.Err(); ctxErr != nil {
                l.completeGroup(ctx, runID, env, resp.ToolCalls, ReasonCancelled)   # 先补齐（独立收尾 context）再落终态
                return env.cancelled(ctxErr, round, toolCalls), nil
            }
            if env.Budget.ToolCallsExhausted(toolCalls) {
                l.completeGroup(ctx, runID, env, resp.ToolCalls, ReasonToolBudget)
                return env.limitReached(ReasonToolBudget, round, toolCalls), nil
            }
            result := l.executor.Execute(ctx, env.ToolEnv(ctx, runID), env.ToolSet, call)  # 返回 Result；**不追加消息**
            if err := env.Session.AppendToolResult(ctx, runID, call, result); err != nil {
                l.completeGroup(ctx, runID, env, resp.ToolCalls, ReasonRunFailed)
                return env.fail(err, round, toolCalls), nil               # 消息追加失败 → Run failed，半截组交恢复扫描
            }
            toolCalls++
        }
        if ctxErr := ctx.Err(); ctxErr != nil {                           # 组返回后复查取消：**取消优先于预算**
            l.completeGroup(ctx, runID, env, resp.ToolCalls, ReasonCancelled)  # 组内未执行的调用取"已取消"文本
            return env.cancelled(ctxErr, round, toolCalls), nil
        }
    }
    return env.limitReached(ReasonIterationLimit, env.Budget.MaxIterations, toolCalls), nil

func (l *ReActLoop) completeGroup(ctx context.Context, runID string, env *RunEnv, calls []ToolCall, reason string):
    if err := env.Session.CompleteToolGroup(ctx, runID, calls, reason); err != nil {
        l.logger.Error("半截工具组补齐失败", "run_id", runID, "reason", reason, "error", err)
        # 只记 ERROR：Run 照常收场，半截组留给下次启动的恢复扫描（约束 24 ③）
    }
```

**边界行为补记（技术方案未画该分支处，本文口径）**：① `Build` 返回 error 的分支在技术方案 §4.1 伪代码里未画——它是契约 §6 上下文预算的"拒绝启动"出口，故循环必须在**首次调用之前**处理；② `CompleteToolGroup` 在解析失败 / 追加失败路径上是**幂等兜底**（无缺失即 no-op），不改变"人人有结果"的底线；③ `iterations` 语义 = **已收到模型响应的轮次数**（= 完成时的 `round`；轮首取消时不计当前轮）——首次纯文本完成即 1。

#### 流程 C — 提示词组装（`internal/core`，`PromptBuilder.Build`）

```text
PromptBuilder.Build(env) (Prompt, error)
  │
  ├─ 段 1 system = env.Snapshot.SectionOne        # 快照冻结的段一正文；**不重读**（读即所记）
  ├─ 段 2 记忆注入 —— **本 US 恒空**（US-6 占位；装配位保留、不产生文本）
  ├─ 段 4 tools = 对本轮 ToolSet 逐名经 ToolResolver.Specs 取 {Name, Description, Parameters}
  │     └─ ToolSet 为空 → Tools = nil（**空池**：请求体不带 tools 键）
  ├─ 段 3 history = env.Session.Messages() 两级裁剪：
  │     ├─ ① 轮数：保留最近 max_history_turns 轮（缺省 20；轮 = 以 user 消息为界）
  │     ├─ ② token：仍超预算 → 按**完整工具交互组**从最老处继续丢（组 = assistant(带 tool_calls) + 其后紧邻 tool 段）
  │     ├─ **原子单位 = 完整组**：任何裁剪不得拆散一组（拆半分 → 供应商 400）
  │     └─ **不可裁**：系统提示词（段 1 + 段 2）、工具定义（段 4）、**当前消息**
  ├─ 估算：tokens = estimateTokens(estimateVersion, 完整出站字段)     # 口径见 §2.4 注 9
  └─ 准入：不可裁部分估算 > context_tokens − 余量 → **error（拒绝启动，零 LLM 调用）**
```

#### 流程 D — 工具调度（`internal/core`，`ToolExecutor.Execute`）

```text
ToolExecutor.Execute(ctx, env ToolEnv, toolSet []string, tc ToolCall) → Result   # **返回结果；不写消息、不返回 error**
  │
  ├─ ① 工具名解析 + **运行时归属校验**（两道，均在**执行之前**——不产生审计行、不终止 Run）
  │     ├─ ToolResolver.Resolve(tc.Name) 不中（注册池里没有）→ 回填"tool not registered: <name>"（契约既有字面量）
  │     ├─ tc.Name ∉ toolSet（全局注册但**本 Agent 未声明**——加载期门禁挡不住池被替换 / 快照与加载不同源）
  │     │     → 回填四段式拒绝：工具未在本 Agent 的 tools 声明中 + 放行指引点名 `AGENT.md` 的 `tools:`（需重启）
  │     └─ 两道都过 → tool
  │     ※ 为什么拒绝也不写审计行：`tool_invocations` 记的是"一次工具执行"（有入参、有结果）；沙箱拒绝发生在
  │       **执行之内**（工具已启动、Policy 判拒 → 有行、failed + sandbox_denied），名字与归属拒绝发生在
  │       **执行之前**（没有任何工具被启动）。尝试本身已由 assistant 消息（含 tool_calls）+ 会话回填文本完整留痕
  │
  ├─ ② 入参校验：json.Valid + 经 json.Decoder UseNumber 能解出**对象**
  │     └─ 非法 → **仍写审计**（工具确实被调用了）：failed + error_kind=invalid_input → 回填失败文本 → Run 继续
  ├─ ③ 审计**先写 pending**（tool_invocations：env.RunID / env.SessionID / tc.ID / tc.Name / input_json 原样）
  │     └─ **写失败 → 拒绝执行**（fail-closed）+ 回填"审计不可用"失败文本 → **Run 继续**（契约 §7.2 工具侧口径）
  ├─ ④ ToolEnv 由**循环侧**构造（`env.ToolEnv(ctx, runID)`，值传递）——本流程只消费：
  │     Timeout = min(30s 缺省, 剩余 Run 预算)          # 工具内部要更短 → 自行 context.WithTimeout 收窄（约束 18）
  │     MaxOutputBytes = min(256 KB 缺省, 配置 budget.max_output_bytes)
  ├─ ⑤ res, err = tool.Execute(ctx, env, tc.Arguments)      # 工具内部经 Policy.Check（文件工具）或 Policy.Check + Runner.Run（命令）
  ├─ ⑥ 终态审计回填（更新**同一行**）：
  │     ├─ SandboxViolationError → failed + sandbox_denied
  │     ├─ 超时（deadline / ctx 错误）→ failed + timeout（**与 ProviderService.classifyError 同口径**）
  │     ├─ 未找到（文件 / 脚本不存在）→ failed + not_found；入参必填缺失 → failed + invalid_input
  │     ├─ 其余工具内部错误 → failed + runtime_error；外部系统错误（US-5 起）→ failed + external_error
  │     └─ 正常 → success + 耗时 + result_json（含 truncated 标记）
  │     └─ **回填本身失败 → 记 ERROR，行留 pending，不覆盖真实结果**（结果照常返回给模型）
  ├─ ⑦ 包装为 Result（失败信息**必须回填**：content = 错误文本；沙箱拒绝文案原样；截断标记在 content 尾部）
  └─ ⑧ 返回 Result；**Run 不因工具失败而失败**（工具失败是模型要看到的信息，不是运行级故障）
```

#### 流程 E — 半截工具组补齐（`internal/core`，`Session.CompleteToolGroup`；契约 §4.4 Option A）

```text
Session.CompleteToolGroup(ctx, runID, calls []ToolCall, reason string) error   # **返回 error**：消息写失败要能传达
  ├─ 收尾 context：finCtx = context.WithTimeout(context.WithoutCancel(ctx), 5s)
  ├─ 判据**落在消息上**（先判缺失，再分类——顺序不得颠倒）：
  │     missing = MissingToolResults(calls, s.Messages())     # = calls 里在 assistant 的 tool_calls 中没有对应 tool 消息的 id
  ├─ 对每个 missing 的调用，按**审计行**分类合成文本（五分支，逐字取自契约 §4.4）：
  │     ├─ 有 success / failed 终态行 → **还原真实结果**（由 result_json 重建 content）
  │     │     ※ 执行过但回填失败（仍有 pending 行）**不得**写成"未执行"——归下一分支
  │     ├─ 有 pending 或 unknown 行 → "结果未知（进程中断，无法确定是否已产生外部副作用）"
  │     │     ※ 同时记 WARN 点名该行（终态回填曾失败——可观测性双轨）
  │     ├─ 无行 + reason=取消 → "已取消（运行被取消，该调用未执行）"
  │     ├─ 无行 + reason=预算 → "未执行（工具调用次数超限）"（迭代耗尽走流程 B 的循环外分支，无半截组）
  │     └─ 无行 + 崩溃路径（恢复扫描）→ "未执行（进程中断）"
  ├─ 逐个 AppendSynthesizedToolMessage（**只进 messages，不进审计**——审计只记真实发生的事）
  └─ 幂等：判据在消息上，重复调用不重复插入；补齐失败 → 返回 error（调用方记 ERROR，Run 照常落终态）
```

```text
func (s *Session) CompleteToolGroup(ctx context.Context, runID string, calls []ToolCall, reason string) error:
    finCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)   # 5s
    defer cancel()
    var errs []error
    for _, call := range MissingToolResults(calls, s.Messages()) {   # **先判缺失**（判据在消息上）
        content, err := SynthesizeMissingResult(finCtx, s.audit, runID, call, reason)  # **再分类**（五分支）
        if err != nil {
            errs = append(errs, err)                                # 查审计失败不吞——记 ERROR 后仍写兜底文本
            content = TextResultUnknown
        }
        if err := s.AppendSynthesizedToolMessage(finCtx, runID, call.ID, content); err != nil {
            errs = append(errs, err)
        }
    }
    return errors.Join(errs...)
```

#### 流程 F — 授权判定（`internal/tool`，`WhitelistPolicy.Check`；技术方案 §6.5 授权矩阵）

```text
Check(ctx, action Action{Type, Target, Agent}) → error        # 任一拒绝 → *SandboxViolationError（四段式文案）
  │
  ├─ 基准：工作区根经 os.OpenRoot 钉住（越出工作区的窗口由 os.Root 语义兜住）；agent 目录由 action.Agent 定位
  ├─ FILE_READ：
  │     ① real = 投影真实路径（软连接感知）→ 不可解析（断链 / 成环 / 无权限）→ **失败关闭**（拒）
  │     ② 放行序：real 在 **当前 Agent 目录**（含 scripts/）内 → 放行；real 在 `.basalt/skills/` 内 → 放行
  │     ③ **跨 Agent 授权分支**：real 落在 **`sandbox.cross_agent_read` 显式列出的** Agent 目录内 → 放行
  │     ④ 其余（未授权 Agent 目录 / memory/ / 工作区外）→ 拒；**未授权跨 Agent 的文案点名 `sandbox.cross_agent_read`**
  ├─ FILE_WRITE：
  │     ① **保护规则先行**：目标（投影后）是 `AGENT.md` 或受保护配置文件 → 拒；在 `scripts/` 内 → 拒；
  │        在 `.basalt/skills/` 内 → 拒（代码常量，技能库不可写）
  │     ② 一般放行：目标在当前 Agent **自有目录**内、且**父目录存在且为目录** → 放行（父目录不存在**不**自动创建）
  │     ③ 其余（`memory/` / 其他 Agent 目录 / 工作区外 / `.basalt` 其它位置）→ 拒；**不得**放宽为 `.basalt` 整根
  │     ※ 写入的**实际约束**在工具侧：经 `os.OpenRoot(agentDir)` 的相对根句柄创建 / 打开文件——"先检查路径、
  │       随后普通 open"证明不了不存在软链接竞争窗口，句柄约束才是保证（§3.1 `filetools.go`）
  ├─ SHELL_COMMAND（只看 Target = executable，**不看 argv**——Action 没有 argv 字段）：
  │     ① Target 含空白（多词串）→ **必拒**
  │     ② Target **逐字**比对 `shell.allowed_commands`（大小写敏感；Windows 上按整串比较，不做扩展名推测）
  │     ③ Target 是 **`bash` / `sh`（含 `*bash*` / `*sh` 的完整路径形态）→ 一律硬拒**（**无放行分支**，契约 §6.5）
  │     ④ Target 在 `shell.interpreters` 内 → 放行**到执行侧**（argv 形态与脚本真实路径由 LocalRunner 校验，见 §3.1）
  │     ⑤ 其余 → 拒
  ├─ HTTP_REQUEST：
  │     ① URL 解析失败 / host 是 IP 字面量 → 拒
  │     ② scheme ∉ {http, https} → 拒（**代码固定**，不可配置放行）
  │     ③ host 不在 `http.allowed_domains`（**域名匹配、大小写不敏感**）→ 拒
  │     ④ 放行（**逐跳校验由调用方在每一跳发送前再次 Check**；跳数上限 5 为代码常量；`Transport.Proxy` 恒 nil——US-5 驱动）
  └─ 任一拒 → return &SandboxViolationError{四段式：<规则>：<越界值>。这是安全策略，请勿反复重试；<放行指引：配置键 + 需重启>}
```

#### 流程 G — 崩溃恢复扫描（`internal/storage`，六步；`app` 在取锁后调用）

```text
Recover(ctx, db, logger, now)（幂等；只在**持锁进程**内启动时跑一次）
  ① pending → unknown        ：llm_calls 与 tool_invocations 全表扫（WHERE status='pending'），error_kind 写 'unknown'
  ② 半截组扫描（**按位置**）：找 assistant(带 tool_calls) 且其后 seq 紧邻连续的 tool 段**缺行**的组
        （按 seq 分组，**不得**按 tool_call_id 全会话查——跨 Run 同 id 会误配）
  ③ 同一短事务：组修复（走流程 E 的五分支文本，missing 先判）+ Run 状态修复
        （**若仍是 running** → status='failed'、reason='进程中断（启动时发现未完成的运行）'、finished_at=now）
        ※ **不得覆盖已结束状态**（completed / limit_reached / cancelled / failed 的行保持原样）
  ④ 另扫 runs：仍为 running 且**不在**上一步事务内的行 → 同样收尾（**组看消息、Run 看表**）
  ⑤ 工作区残留（`internal/workspace`）：<agent>/.backup 且正式目录缺失 → 回滚；正式目录完整
        且 .backup/.staging 尚在 → 前滚删除（本 US 无 staged 写入方，此步属**休眠分支**，单测直接构造）
  ⑥ INFO 日志（每步计数）+ 启动摘要（未知数 / 修复组数 / 收尾 Run 数）
```

### 2.4 差异裁决注

1. **`Policy` / `CommandRunner` 的调用点：工具内部**（**已裁决 ①：通过**）。技术方案 §6.5 写"调用方（`ToolExecutor`）零改动"，但 §6.1 的 `Tool` 四方法与契约 §1.1 的 `ToolEnv` **已冻结**，而 `Action.Target`（路径 / 可执行名 / URL）与 `Command`（argv）**只能由解析入参的那一方构造**；由 `ToolExecutor` 构造就得**再解析一遍**入参（第二套解析路径——checker 与 runner 的解析分歧正是越权漏洞的经典来源）。**落案口径**：工具内部依次 `Policy.Check` / `CommandRunner.Run`（两接口经构造注入，装配在 `app`；`tool` 包内**不得**引用 `WhitelistPolicy` / `LocalRunner` 的具体类型）；**文件工具只持有 `core.Policy`**（读 / 写不启子进程），命令执行的工具才持 `core.CommandRunner`；解释器 argv 形态校验属**执行侧**（`LocalRunner`）。技术方案"`ToolExecutor` 零改动"的**实质**（换实现只动装配处）保持不变。
2. **`workspace` 不在编程指南 §4.4 的「涉及包」列表内，但本 US 必须动它**（**前序改造点，软门禁**）。两条前序约束把它拉进来：US-3 §2.2 约束 7 明写"运行级快照冻结与清单哈希归 US-4"；同一约束的"**注入内容与哈希必须共享同次读取产物**"只有**读取方**能满足（在 `core` / `app` 里二次读盘算哈希直接违反该约束）。**落案口径**：`workspace` 增 `Snapshot(root, entry, now)`（同一次读取产出 Profile + 段一 + 原文 + manifest 哈希；既有 `SectionOne` 委托给它、**签名不变**）+ `recovery.go`（步骤 ⑤）。**不动**编程指南的包列表文字（历史工件不改写）。
3. **`ChatRequest.Tools` 与 provider 出站 `tools` 字段：US-2 已登记占位，本 US 兑现**。**落案口径**：`core.ChatRequest` 增 `Tools []ToolSpec`（占位名逐字沿用）+ `core.ToolSpec{Name, Description string; Parameters json.RawMessage}`；`provider` 在 `len(req.Tools) > 0` 时输出**单个** `tools` 键（OpenAI 形态 `[{"type":"function","function":{...}}]`），为空**不带该键**（空池 = 客户端行为逐字不变）。**新增回归**：出站 tools 编解码逐字断言（§3.2 provider 4 条）。
4. **加载期工具门禁：名字集显式注入，不引 `workspace → tool` 依赖**（**已裁决**：方案成立）。`AgentLoaderOptions` 增 `Tools []string`（注册池名字快照），`workspace` **不**读 `tool` 包；`app` 先建 `tool.Registry` → 取名字集 → 传 `AgentLoader`。**US-3 既有用例（软门禁，须用户确认模式）**：`TestAgentLoader_LoadsCompatSampleAgent` 改造为"**注入 10 个名字** → 注册成功 + `tool not registered` 零命中"（兼容判据是"加载器对**已注册**名字集不误拒"，不是"这 10 件都实现了"）+ **新增**负例"注入 3 个名字 → 同一目录被拒且 reason 逐字 `tool not registered: shell`"。**不得**改样例 `AGENT.md`、**不得**放宽门禁；`AgentLoaderOptions.Tools` 为空 → **空池**（未声明 `tools:` 的 Agent 仍注册；声明了 `tools:` 的 Agent 整只不注册）。
5. **`config_snapshots` 落库**（**已裁决 ③：通过，本 US 落库**）。在**关联 Run 开跑前**保存（流程 A 步骤 ② 与 runs 预写同批），失败则**拒绝启动**；按既有 DDL 写**原始 `AGENT.md` 全文**（`content`）与 `manifest_json`（`[{path, sha256}]`）——**不能只存哈希**；内容寻址：`hash` 由 manifest 规整序列化派生，`UNIQUE(agent_name, hash)` + insert-if-absent（幂等，重复不覆盖）。US-11 只是改变**发布时机与版本保留策略**，**不改内容寻址算法**（`hash` 的派生规则一旦发布即冻结）。
6. **三段沙箱配置的形状：沿用已有契约键，不另造**（**已裁决 ②：原建议不通过**）。既有键面（`basalt.yaml.example` + `docs/Operations.md` §2.2）：
   - `sandbox.cross_agent_read`（列表；缺省空 = 一个都不许）——**没有** `sandbox.allowed_domains` 这个键；
   - 域名白名单键名 = **`http.allowed_domains`**（Operations §2.2 已列；本 US 在样例文件中补该键一行，**沿用键名不新造**）；
   - `shell.allowed_commands` / `shell.interpreters` / `shell.env_passthrough`（样例文件已有）；
   - 重定向跳数上限 = **代码常量 5**（**不得放宽**，不给配置键；技术方案 §6.5 的"上限 5"是硬边界）。
   **配置结构**：`internal/cli/config.go` 的 `Sandbox` / `Shell` / `HTTP` 由 `map[string]any` 落为**结构化字段**（键名逐字沿用上表）——登记为前序改造点（§3.5 第 4 项）。
7. **CLI 的 Run 时限 = 60s**（**已裁决 ④**）：契约 §6 只给 REST 60s / 定时 300s；CLI 采用 **60s**（与 REST 同档），由命令面为每次 Run 建 deadline；**调用方已有更早 deadline 时取更早者**（context 语义天然如此）；Ctrl-C 走取消路径，**不能替代**资源时限。
8. **`ToolTimeout` 的 Profile 覆盖键：本 US 不新增**（**已裁决 ⑤**）：契约 §6 说 Profile 可覆盖，但 §2 字段表无对应键 → 本 US 生效值 = `min(30s 缺省, 剩余 Run 预算, 工具请求的更小值)`（模型只能缩短，不能放大），**实际建立超时 context**；覆盖分支留待字段表扩展（另案）。**单工具超时 → 该工具失败、Run 继续**；**Run 自身取消或截止 → `cancelled`**。
9. **token 估算口径**（**已裁决 ⑥：原建议不通过**"字节数 / 2 是保守上界"的说法）。**落案口径**：估算器**版本明确**（`core.EstimateVersion = "v1"`，写进日志与评测结果文件，**不落库**——无对应列）；**覆盖完整出站字段**：段 1 / 段 2 正文、历史每条的 `role` + `content` + `tool_calls`（`name` + `arguments`）+ `tool_call_id` + `reasoning_content` + `provider_extra` 的 JSON 文本、段 4 每个工具的 `name` + `description` + `parameters`（JSON Schema 文本），外加每条消息固定开销与全局余量（用于吸收协议包装）。**标定**：五类夹具（中文 / 英文 / 代码 / 工具 schema / 工具参数与扩展字段）在单测中钉住**确定性**（同输入同读数）与**相对偏差**（以夹具内记录的参考读数为准——参考读数由人工用目标模型分词器实测后写入夹具注释，标注测量日期与模型）；**不承诺**"永不低估"——超发风险由余量与按组裁剪共同吸收，真超发由供应商报错按 `external_error` 记失败并如实返回（人工项 §6 第 8 项复测）。
10. **`evals/` 是仓库根新顶层目录**（**已裁决 ⑦**）：`evals/` 进仓库根（不进 `internal/` 包结构、不进 `cmd/` 发布面），以 `package main` 形态经 `go run ./evals` 运行；**不新增评测表**（读数落结果文件 `evals/results/<时间戳>.json`，关联 RunID 与审计行）。判据与形态见 §3.1。
11. **`core` 从"零可执行语句"升级为"引擎包"，非违反包纪律**：US-1~US-3 的颗粒度文档反复出现"`core` 零可执行语句"——那是**当时**的事实。技术方案 §10.1 明确把 ReActLoop / PromptBuilder / ToolExecutor / AgentService 放在"核心抽象与引擎"，故 `core` 自本 US 起有可执行语句是**设计使然**。纪律的**实质**（`core` 只依赖标准库）不变，判据从"零语句"改为"`go list -f '{{.Imports}}' ./internal/core` 只出标准库"。**历史工件不回改**。
12. **与 TS §4.1 伪代码的逐处差异（TS 未画或与契约字面量冲突处，本文冻结）**。TS §4.1 是循环的**算法主源**，但其伪代码省略了类型来源与返回值细节；契约 §1.1 是 `RunRequest` / `RunResult` / `RunEnv` / `ToolEnv` 字段字的**唯一事实源**（"技术方案与实现**不得**另抄字段清单"）。两者相互约束后，本文与 TS 伪代码存在六处**有据**差异，逐处列明（其余逐块同构）：
    1. **`run` 增 `runID` 显式参数**：`RunEnv` 的字段字是 `{Req, Snapshot, Session, Budget, ToolSet}`——**没有** RunID 字段，而 `ToolEnv.RunID` / `ChatRequest.RunID` / `messages.run_id` / `runs.run_id` 都必须由它填充（TS §4.1 的 `env.ToolEnv()` / `env.llmEnv(i, prompt)` 未画出该来源）。**不增契约字段、不落包级变量**（宪法 VII 显式传递）——由 `AgentService.Run` 生成后逐层传参（TS §4.2 表："`AgentService`——生成 `run_id`"）。
    2. **`Execute` 增 `toolSet` 参数**：TS 写作 `l.executor.Execute(ctx, env.ToolEnv(), call)`；裁决 ④ 要求的**运行时 ToolSet 归属校验**需要 Agent 声明的工具名集，而 `ToolEnv` 字段字**不得**扩——故 `ToolEnv` 参数原样保留、`toolSet` 单列一个参数。
    3. **`round` 从 1 起**：TS 的循环变量是 `i := 0`；契约 `llm_calls.round` 的语义是"该 Run 内第几轮（**1 起**）"——取契约口径（裁决 ③）。
    4. **结局构造器显式收计数**：TS 写作 `env.complete(resp.Content)` / `env.cancelled()`；`RunEnv` 是**值语义**（契约"值传递"）、字段字不含计数器，故 `iterations` / `toolCalls` 由循环显式传入（`env.complete(text, iterations, toolCalls)`），`cancelled` 另收 `cause error`（`RunResult.Reason` 需要可读原因）。
    5. **`Build` 的错误分支**：TS 未画——契约 §6 的"不可裁部分超限即拒绝启动"必须有出口，故在**首次调用之前**处理（零 LLM 调用）。
    6. **`Budget.ToolCallsExhausted(n)` 取值入参**：TS 写作 `env.Budget.toolCallsExhausted()`（隐含 env 可变状态）；本仓 `Budget` 是纯值结构，计数由循环持有——语义等价，签名带参。
    **两处与 TS 完全一致、不得改动**：`Session.CompleteToolGroup(calls, reason)` 归 **Session**（TS §4.1 第 245 / 249 行即 `env.Session.CompleteToolGroup(...)`）；`Env.Session.AppendToolResult(call, result)` 由**循环**调用（TS 第 253 行）。

## 3. 交付物清单

### 3.1 代码

**既有类型：复用，不重造**（本 US 一行定义都不写）：`core.Message` / `core.ToolCall` / `core.Usage` / `core.ChatRequest`（**改造**：增 `Tools` 字段，见 §3.5 第 2 项）/ `core.ChatResponse` / `core.ProviderClient` / `core.LLMCallStatus` / `core.ErrorKind` / `core.LLMCall` / `core.LLMCallResult` / `core.LLMCallStore`。`ProviderExtra` 保持 `map[string]any`（**不改类型**）。

**`internal/core`（引擎包——本 US 起有可执行语句；只依赖标准库）**

| 文件 | 内容 | 关键方法签名（与 §2.3 逐字一致） |
| --- | --- | --- |
| `run.go`（新增） | 契约 §1.1 的运行面（**字段字逐字，不增不改**）：`RunRequest` / `RunResult` / `RunStatus` / `RunEnv` / `ToolEnv` / `Budget` / `RunRecord` / `SessionRecord`；RunEnv 结局构造与助手消息组装；`RunID` 生成 | `func NewRunID(channel string, now time.Time, rand6 string) string`；`func (b Budget) ToolCallsExhausted(n int) bool`；`func (e *RunEnv) llmEnv(runID string, round int, p Prompt) ChatRequest`；`func (e *RunEnv) assistantMessage(resp ChatResponse) Message`；`func (e *RunEnv) ToolEnv(ctx context.Context, runID string) ToolEnv`（**runID 显式传参**——§2.4 注 12 第 1 条；`Timeout = min(30s 缺省, 剩余 Run 预算)`，读 ctx 截止时间）；`func (e *RunEnv) client() ProviderClient`；`func (e *RunEnv) complete(text string, iterations, toolCalls int) RunResult`；`func (e *RunEnv) limitReached(reason string, iterations, toolCalls int) RunResult`；`func (e *RunEnv) cancelled(cause error, iterations, toolCalls int) RunResult`；`func (e *RunEnv) fail(err error, iterations, toolCalls int) RunResult` |
| `errors.go`（改造） | 增运行面错误类型与哨兵（**不动既有哨兵**）：`SandboxViolationError`（含四段式文案字段）、`AgentNotFoundError` / `SessionBusyError` / `InstanceFullError` | `func (e *SandboxViolationError) Error() string`；`func IsSandboxViolation(err error) bool` |
| `session.go`（新增） | `Session`：内存历史 + 写透仓储；无状态（空 session_id）仅内存；**半截组补齐（流程 E）** | `func NewSession(sessionID string, store MessageStore, audit ToolInvocationReader) *Session`；`func (s *Session) Load(ctx context.Context) error`；`func (s *Session) AppendMessage(ctx context.Context, runID string, m Message) error`；`func (s *Session) AppendToolResult(ctx context.Context, runID string, call ToolCall, r Result) error`；`func (s *Session) AppendSynthesizedToolMessage(ctx context.Context, runID, toolCallID, content string) error`；`func (s *Session) CompleteToolGroup(ctx context.Context, runID string, calls []ToolCall, reason string) error`（**TS §4.1 的调用形状 + 裁决 ⑤ 的 error 返回**）；`func (s *Session) Messages() []Message`；`func MissingToolResults(calls []ToolCall, msgs []Message) []ToolCall`（**导出：取消路径与恢复路径共用同一判据实现**） |
| `store.go`（新增） | 消费方接口声明：`MessageStore`（**追加接口显式承载 `runID`**——`Message` 本身没有该字段，恢复却依赖 `messages.run_id`）、`RunStore`、`ToolInvocationStore`、`SnapshotStore`、`ToolResolver`、`Policy`、`CommandRunner`；工具审计的预写 / 终态结构与 `SessionRecord` / `RunRecord` | `type MessageStore interface { AppendMessage(ctx context.Context, sessionID, runID string, m Message) error; Messages(ctx, sessionID string) ([]Message, error); UpsertSession(ctx, SessionRecord) error; ListSessions(ctx) ([]SessionRecord, error) }`；`type ToolInvocationStore interface { BeginInvocation(ctx, ToolInvocation) (int64, error); FinishInvocation(ctx, id int64, ToolInvocationResult) error }`；`type ToolInvocation struct { RunID, SessionID, ToolCallID, ToolName string; InputJSON json.RawMessage; CreatedAt time.Time }`；`type ToolInvocationResult struct { Status LLMCallStatus; ResultJSON json.RawMessage; ErrorMessage string; ErrorKind ErrorKind; Duration time.Duration }`；`type ToolInvocationReader interface { Lookup(ctx context.Context, runID, toolCallID string) (ToolInvocationView, bool, error) }`（**补齐路径专用读接口**：`Session` 只读不写审计，`storage` 的同一仓储实现它）；`type SnapshotStore interface { SaveSnapshot(ctx context.Context, snap *ConfigSnapshot) error }`；`type ToolResolver interface { Resolve(name string) (Tool, bool); Specs(names []string) []ToolSpec }` |
| `action.go`（新增） | `Action` / `ActionType` / `Command` / `CommandResult`（技术方案 §6.5 逐字） | `Action{Type ActionType; Target string; Agent string}`；`ActionFileRead/ActionFileWrite/ActionShellCommand/ActionHTTPRequest = "FILE_READ"/"FILE_WRITE"/"SHELL_COMMAND"/"HTTP_REQUEST"` |
| `snapshot.go`（新增） | `ConfigSnapshot` / `ManifestEntry`（技术方案 §11.5；由 `workspace` 构造、`storage` 落库） | `ConfigSnapshot{AgentName string; Hash string; Content string /* AGENT.md 全文 */; Manifest []ManifestEntry; SectionOne string; Profile *Profile}`；`ManifestEntry{Path, SHA256 string}` |
| `tool.go`（新增） | 工具四方法接口 + `Result`（技术方案 §6.1 逐字）+ 角色常量 | `type Tool interface { Name() string; Description() string; InputSchema() json.RawMessage; Execute(ctx context.Context, env ToolEnv, input json.RawMessage) (Result, error) }`；`type Result struct { Content string; Retryable bool }`；`const RoleUser/RoleAssistant/RoleTool = "user"/"assistant"/"tool"` |
| `prompt.go`（新增） | `Prompt` / `ToolSpec` + `PromptBuilder`：四段组装、两级裁剪、上下文准入、token 估算 | `func NewPromptBuilder(resolver ToolResolver) *PromptBuilder`；`func (b *PromptBuilder) Build(env *RunEnv) (Prompt, error)`；`func estimateTokens(p Prompt) int`；`const EstimateVersion = "v1"`；`const ReserveTokens = 1024`（**2026-10-16 补登**：原稿写未导出 `estimateVersion`，实现按裁决取导出名——评测跨包直接引用同一份常量，见「收口补登」F1） |
| `loop.go`（新增） | `ReActLoop`：§2.3 流程 B | `func NewReActLoop(builder *PromptBuilder, executor *ToolExecutor, logger *slog.Logger) *ReActLoop`；`func (l *ReActLoop) run(ctx context.Context, runID string, env *RunEnv) (RunResult, error)`（**TS §4.1 签名 + 显式 runID**——§2.4 注 12 第 1 条；error 恒 nil：运行期失败一律归 `Status`，契约 §1.2）；`func (l *ReActLoop) completeGroup(ctx context.Context, runID string, env *RunEnv, calls []ToolCall, reason string)`（**补齐失败只记 ERROR**——Run 照常收场，半截组留给恢复扫描） |
| `tool_exec.go`（新增） | `ToolExecutor`：§2.3 流程 D | `func NewToolExecutor(resolver ToolResolver, audit ToolInvocationStore, logger *slog.Logger) *ToolExecutor`；`func (e *ToolExecutor) Execute(ctx context.Context, env ToolEnv, toolSet []string, call ToolCall) Result`（**返回结果，不写消息**；`toolSet` 承载裁决 ④ 的运行时归属校验——§2.4 注 12 第 2 条） |
| `service.go`（新增） | `AgentService`：**唯一运行入口**；准入 / 会话锁 / 实例计数 / 快照取用与落库 / runs 落库 | `func NewAgentService(opts ServiceOptions) *AgentService`；`func (s *AgentService) Run(ctx context.Context, req RunRequest) (RunResult, error)` |
| `synthesis.go`（新增） | 补齐分类**唯一份**（五分支四文本，逐字取自契约 §4.4）+ `Reason*` 常量 + 截断标记 | `func SynthesizeMissingResult(ctx context.Context, audit ToolInvocationReader, runID string, call ToolCall, reason string) (string, error)`（**取消路径与恢复路径共用**：先查审计行——终态行 → 由 `ResultJSON` 重建 content；`pending` / `unknown` → `TextResultUnknown` + WARN；无行 → 按 `reason` 取取消 / 预算 / 中断文本）；`const TextToolCancelled/TextToolBudget/TextResultUnknown/TextToolInterrupted = "已取消（运行被取消，该调用未执行）"/"未执行（工具调用次数超限）"/"结果未知（进程中断，无法确定是否已产生外部副作用）"/"未执行（进程中断）"`；`const ReasonCancelled/ReasonToolBudget/ReasonIterationLimit/ReasonRunFailed = "运行被取消"/"工具调用次数超限"/"超过最大迭代次数"/"运行内部错误"`；`const finishTimeout = 5 * time.Second`；`func TruncationMarker(maxBytes int) string` |

**`internal/tool`（沙箱与内置工具——本 US 起进入 ≥70% 覆盖率判卷）**

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `registry.go` | `Registry`：显式注册 + 名字集导出（装配期喂 `AgentLoader`） | `func NewRegistry(logger *slog.Logger) *Registry`；`func (r *Registry) Register(t core.Tool) error`（重名报错）；`func (r *Registry) Resolve(name string) (core.Tool, bool)`；`func (r *Registry) Names() []string`（排序）；`func (r *Registry) Specs(names []string) []core.ToolSpec` |
| `policy.go` | `WhitelistPolicy`：§2.3 流程 F；跨 Agent 授权分支；四段式文案生成 | `func NewWhitelistPolicy(opts PolicyOptions) *WhitelistPolicy`；`func (p *WhitelistPolicy) Check(ctx context.Context, action core.Action) error` |
| `pathguard.go` | 真实路径投影与归属判定（软连接感知；不可解析 → 失败关闭） | `func resolveRealPath(root, target string) (string, error)`；`func withinDir(real, dir string) bool` |
| `runner.go` | `LocalRunner`：受控子进程（环境变量白名单、超时杀进程树、输出**读取中**限流、**解释器 argv 形态与脚本真实路径校验**——执行侧裁决点） | `func NewLocalRunner(opts RunnerOptions) *LocalRunner`；`func (r *LocalRunner) Run(ctx context.Context, cmd core.Command) (core.CommandResult, error)` |
| `runner_windows.go` | 进程树生命周期（Job Object）——见下方**落地顺序** | `func spawnGuarded(...)`；`func killTree(job uintptr) error` |
| `runner_unix.go` | 进程组终止（`SysProcAttr{Setpgid: true}` + `kill(-pgid)`；Unix 侧无"关联窗口"——组随 exec 原子生效） | `func spawnGuarded(...)`；`func killTree(pid int) error` |
| `filetools.go` | `ReadFileTool` / `WriteFileTool` / `ListDirTool`（契约 §5.1 参数与失败类别逐字）；**写入经 `os.OpenRoot(agentDir)` 的相对根句柄**（约束 17 注） | `func NewReadFileTool(policy core.Policy, root string) *ReadFileTool`；`func NewWriteFileTool(policy core.Policy, root, agentDir string) *WriteFileTool`；`func NewListDirTool(policy core.Policy, root string) *ListDirTool`；`InputSchema() json.RawMessage` 返回**内联 JSON Schema 常量**（测试钉住） |
| `doc.go` | 包说明：授权矩阵与执行控制的**面向实现者**摘要 + 引用出处 | — |

**`runner_windows.go` — 进程树生命周期（Job Object；P2 补齐项）**

```text
① CreateJobObject → SetInformationJobObject(JobObjectExtendedLimitInformation,
     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE)      # 关句柄即杀全树 = 兜底不漏孙进程
② 以 CREATE_SUSPENDED 创建子进程（**先挂起**）  # 消除"启动与关联之间"的窗口：子进程在关联完成前不跑用户代码
③ AssignProcessToJobObject(job, process)     # 关联；失败 → TerminateJobObject(兜底) → 关进程/线程句柄 → 关 Job → 返回错误
④ ResumeThread(thread)                        # 放行；失败 → TerminateJobObject → 同上清理顺序 → 返回错误
⑤ 超时 / 取消 → TerminateJobObject(job)       # 整树终止（不是 TerminateProcess 单杀）
⑥ 收尾顺序固定：Wait 回收子进程 → 关进程 / 线程句柄 → 关 Job 句柄 → 错误 Join 上抛
   ※ 只有 `killTree(pid)` 不足以描述保证：**约束来自关联关系**，故 ②③ 的挂起—关联顺序是设计的核心
```

**`internal/channel`（真控制台 vs 管道分流——US-4 最大的坑）**

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `channel.go` | `Channel` 接口 + `Message` / `Reply` 形态（消费方接口） | `type Channel interface { Name() string; Send(ctx context.Context, r Reply) error; Receive(ctx context.Context) (Message, error) }` |
| `cli.go` | `CliChannel`：交互循环、终止语义（EOF / `--message` 单条 / Ctrl-C） | `func NewCliChannel(opts CliOptions) *CliChannel` |
| `console_windows.go` | `IsTerminal` 经 `windows.GetConsoleMode`；真控制台经 `ReadConsoleW` / `WriteConsoleW` | `func isTerminal(f *os.File) bool`；`func newConsole(in, out *os.File) console` |
| `console_linux.go` | `IsTerminal` 经 `unix.IoctlGetTermios(fd, unix.TCGETS)`；非终端按 UTF-8 直读 / 直写 | 同上（build tag `linux`） |
| `console_darwin.go` | **独立文件**：Darwin 的常量是 `unix.TIOCGETA`（**不得**与 Linux 合并写 `TCGETS`） | 同上（build tag `darwin`） |
| `doc.go` | 包说明：编码纪律（技术方案 §8.4）与平台差异登记 | — |

**`internal/storage`（仓储与恢复；本 US 起 ≥70%）**

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `session_store.go` | `sessions` / `messages` 仓储（`core.MessageStore` 实现）：`seq` 库内自增、**`run_id` 显式入列**、按会话取历史、会话行 upsert / 列表 | `func (s *SessionStore) AppendMessage(ctx context.Context, sessionID, runID string, m core.Message) error`；`func (s *SessionStore) Messages(ctx context.Context, sessionID string) ([]core.Message, error)`；`func (s *SessionStore) UpsertSession(ctx context.Context, rec core.SessionRecord) error`；`func (s *SessionStore) ListSessions(ctx context.Context) ([]core.SessionRecord, error)` |
| `run_store.go` | `runs` 仓储（`core.RunStore`）：建 `running` 行、终态更新、扫 `running`（**无 `final_text` 列**——`FinishRun` 不接受 finalText 参数） | `func (s *RunStore) StartRun(ctx context.Context, rec core.RunRecord) error`；`func (s *RunStore) FinishRun(ctx context.Context, runID string, status core.RunStatus, reason string, iterations, toolCalls int, now time.Time) error` |
| `tool_invocations.go` | `tool_invocations` **两阶段写**（`core.ToolInvocationStore` 实现）：`BeginInvocation` 写 `pending` / `FinishInvocation` 更新同一行（含 `result_json` 与截断标记） | `func (s *ToolInvocationRepo) BeginInvocation(ctx context.Context, inv core.ToolInvocation) (int64, error)`；`func (s *ToolInvocationRepo) FinishInvocation(ctx context.Context, id int64, res core.ToolInvocationResult) error` |
| `snapshot_store.go` | `config_snapshots` 落库（**insert-if-absent**；`UNIQUE(agent_name, hash)` 冲突即 no-op） | `func (s *SnapshotStore) SaveSnapshot(ctx context.Context, snap *core.ConfigSnapshot) error` |
| `recovery.go` | 六步恢复 ①②③④⑥（§2.3 流程 G；③ 在**同一短事务**内、**只改仍为 `running` 的行**；② 按位置判组、调 `core.MissingToolResults`） | `func Recover(ctx context.Context, db *sql.DB, logger *slog.Logger, now time.Time) (RecoveryReport, error)`；`type RecoveryReport struct { Unknowns, RepairedGroups, ClosedRuns int }` |
| `doc.go`（改造） | 包说明：本 US 起仓储面 + 恢复扫描的**事务边界**纪律（不得跨 LLM 调用与工具执行） | — |

**`internal/workspace`（前序包改造——§2.4 注 2 / 注 4，逐条软门禁）**

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `context_loader.go`（改造） | 增 `Snapshot`：**同一份原始 AGENT.md** 解析 Profile + 段一正文 + 保留原文；manifest 规整序列化定哈希（不含时间）；`SectionOne` 委托给它、**签名不变** | `func (c *ContextLoader) Snapshot(root string, entry core.AgentEntry, now time.Time) (*core.ConfigSnapshot, error)` |
| `agent.go`（改造） | `AgentLoaderOptions` 增 `Tools []string`（注册池名字集）；规则 4 由占位转为**执行**：名字不在池 → `tool not registered: <name>` 整只不注册 | `type AgentLoaderOptions struct { Providers []string; Tools []string; Logger *slog.Logger }` |
| `recovery.go`（新增） | 恢复步骤 ⑤：`.backup` / `.staging` 残留判定（回滚 / 前滚） | `func RecoverWorkspaceResidue(root string, logger *slog.Logger) (ResidueReport, error)` |

**`internal/provider`（前序包改造——§2.4 注 3）**

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `client.go`（改造） | 出站请求体输出 `tools` 键（Function Calling 形态；**空池不带键 = 旧行为逐字不变**） | `func buildRequestBody(model string, msgs []core.Message, tools []core.ToolSpec) ([]byte, error)` |

**`internal/cli`（命令面）**

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `chat.go` | `chat` 命令：多轮交互 + `--message` 单条；**`session_id` 由本层派生**（规范元组编码，§6 固定口径 1，**在准入之前算好**）；每次 Run 建 60s deadline；渲染模板见 §6 固定口径 3 / 4 | `func newChatCmd(deps Deps) *cobra.Command`；`func deriveSessionID(channel, user, profile string) string` |
| `session.go` | `session list`：**只读命令**（不取锁、不恢复、校验 `schema_version`） | `func newSessionCmd(deps Deps) *cobra.Command` |
| `ports.go` / `root.go`（改造） | `Deps` 增运行面端口（`RunService` / `SessionLister`）；注册 `chat` / `session` 命令组 | — |

**`internal/app`（装配——唯一装配层）**

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `ports.go`（改造） | 新端口适配器（`RunService` ← `core.AgentService`；`SessionLister` ← `storage.SessionStore`） | — |
| `wire.go`（新增） | 重路径接线：**取锁 → 写模式打开 → 显式迁移 → 恢复扫描** → 建 `tool.Registry` → 名字集喂 `AgentLoader` → 构造 `AgentService`（`Policy` / `Runner` / 四个仓储 / 解析器 / 时钟注入） | `func (a *App) newRuntime(...) (*runtime, error)` |
| `run.go`（改造） | `chat` / `serve` 走重路径；`session list` 走只读路径 | — |

**`evals/`（行为评测基座——仓库根新顶层目录，§2.4 注 10）**

| 文件 | 内容 |
| --- | --- |
| `evals/README.md` | 用例格式、判定口径、运行方法、资源用量读数字段说明（**不换算金额**） |
| `evals/main.go` | 运行器（`package main`，`go run ./evals --cases evals/cases`）：加载 YAML 用例 → 经 `AgentService` 跑 → **可执行断言逐例判 PASS/FAIL** → 结果文件 `evals/results/<时间戳>.json`（每例：用例 id / RunID / 状态 / 工具序列 / token 用量 / 调用次数 / **计量完整性**标记） |
| `evals/cases/*.yaml` | 用例集：**输入**（message）/ **夹具**（临时工作区的 AGENT.md 与文件）/ **预算**（max_iterations / max_tool_calls / context_tokens）/ **断言**（期望工具序列、期望文本包含、期望终态）/ 越权尝试项 |
| `evals/injection/*.yaml` | 注入类安全用例：越权尝试必须被四段式拒绝 + 审计落 `failed` + `sandbox_denied` |
| `Makefile` / `scripts/run-evals.sh` | 可选包装脚本（`go run ./evals …`）；**门禁化（进 `make check`）不在本 US**（属构建步骤变更，走 `/us-register 4` 的判断题） |

**评测两种后端（裁决 ⑦）**：**默认 fake**（进程内 fake `ProviderClient`，脚本化响应序列——**可重复运行**，CI 与日常回归用）；**真模型显式门控**（`--provider <name>` + 环境变量凭证，人工执行、结果如实登记）。业务结果的**主观项**（如"回答是否专业"）留人工签核，可执行断言部分自动判卷。

### 3.2 测试

> 用例总数 **185 条**（收口核对：`grep -c '^func Test'` 逐文件比对；`http_get` / `http_post` 已按契约更名，无 `http_request` 残留）。
>
> **2026-10-16 implement 期记账修订**（用户裁决）：`core/loop_test.go` 实测 17 条（原记 16）——A 清单点名的 12 条全部落地，另含 §4.3 三条原样落地中的 `TestReActLoop_NoGoroutineLeakOnCancel` 与四条失败路径 / 准入用例（模型失败 → `failed` / 取消后模型错误 → 已取消 / 不可裁超限 → 拒绝且零模型调用 / `RunID` 形态）；`usage` 空记 0 由 ProviderService 用例承担，本文件只留口径注释。故 US1 61 → 62、总数 184 → 185。**不得**为凑数删测试。

| 测试文件 | 条数 | 覆盖点（对应 §4.2 映射表编号） |
| --- | --- | --- |
| `core/loop_test.go` | 17 | A1~A17：迭代上限 / 首次完成 iterations=1 / round 从 1 起 / 工具串行 / **轮内重组装**（第二轮请求含上一轮工具结果）/ 取消优先于预算 / 轮首取消不计当前轮 / 工具预算耗尽**不**禁止纯文本完成 / 每条提前返回路径都经 `Session.CompleteToolGroup` / 空 content 容忍 / `usage` 空记 0（由 ProviderService 用例承担，本文件只留口径注释）/ 追加唯一性（assistant 恰 2 条 + tool 恰 N 条）/ 模型失败 → 运行 `failed` / 取消后模型错误 → 已取消 / 不可裁部分超限 → 拒绝且零模型调用 / `RunID` 形态（2026-10-16 implement 期记账修订，见下方注） |
| `core/tool_exec_test.go` | 10 | B1~B10（逐条对号，§4.2 按号引用）：B1 **运行时 ToolSet 校验**（全局注册但不属本 Agent → 不执行、无审计行、四段式回填、Run 继续）；B2 名字不在注册池 → 无审计行 + 四段式回填；B3 审计 `pending` 预写失败 → 拒执行且 **Run 继续**；B4 终态回填失败 → 只记 ERROR、保留 `pending`、不覆盖真实结果；B5 沙箱拒绝 → `failed` + `sandbox_denied`；B6 单工具超时 → `failed` + `timeout`（Run 继续）；B7 非法入参 → `failed` + `invalid_input`（**仍写审计行**）；B8 目标不存在 → `not_found`、进程启动失败 → `runtime_error`；B9 `tool_call_id` 与 `reasoning_content` 落审计详情（§4.3 硬约束）；B10 截断标记与结果包装（**失败信息必须回填**） |
| `core/prompt_test.go` | 12 | C1~C12：四段顺序 / 空池不带 tools / **轮数**裁剪（20 轮，不按条数）/ 组不拆散 / token 超限按组继续裁 / 不可裁部分超限 → 拒绝（error）/ 当前消息不裁 / 系统提示词与工具定义不裁 / **token 估算五类夹具**（中英文 / 代码 / 工具 schema / 参数与扩展字段）+ 确定性 + `EstimateVersion` |
| `core/service_test.go` | 10 | D1~D10：唯一入口 / 未知 Agent 报错 / 会话忙不排队 / 实例满不排队 / **空 SessionID 无状态**（不落会话历史、仍落 Run 与审计）/ runs 先落 running / 用户消息写入点唯一 / 终态落库列正确（**无 final_text**）/ RunID 形态 / 快照先落库再开跑 |
| `core/session_test.go` | 8 | E1~E8（**流程 E 全落此文件**——`CompleteToolGroup` 是 `Session` 方法，不挂 `tool_exec_test.go`）：E1 `AppendMessage` 透传 `runID`；E2 `MissingToolResults` 只认消息判据（按位置判组）；E3 五分支逐条（含"**执行过但回填失败不得写成『未执行』**"）；E4 补齐幂等（重复调用不重复插入、组已完整为 no-op）；E5 **先判缺失再分类**（顺序断言）；E6 补齐失败 → **返回 error**（调用方记 ERROR，Run 照常收尾）；E7 收尾 context 独立（父 ctx 已取消仍能写、限时收尾）；E8 无状态会话仅内存（不落库） |
| `tool/policy_test.go` | 18 | F1~F18：读白名单（含 skills 库）/ **`cross_agent_read` 授权正例** / **未授权跨 Agent 拒且点名该键** / 软连接逃逸拒 / 不可解析失败关闭 / 写 `scripts` 拒 / `AGENT.md` 拒 / skills 库拒 / `memory/` 拒 / **父目录不存在拒** / 保护规则先于一般放行 / 多词串拒 / **`bash` / `sh` 硬拒（含 `bash -c` 与 `bash script.sh`）** / 整串逐字比对 / 域名大小写不敏感 / IP 字面量拒 / scheme 拒 / 四段式文案逐字 |
| `tool/runner_test.go` | 10 | G1~G10：环境变量白名单 / 超时杀进程树（子进程一并死）/ **读取中限流** / **更小的配置上限被尊重** / 截断标记**不含原始长度** / 退出码与 stderr 捕获 / 不继承代理变量 / **解释器 argv 形态校验**（`-c` 拒 / 脚本真实路径越界拒）/ Job 关联失败清理顺序（Windows 读数） |
| `tool/registry_test.go` | 6 | H1~H6：显式注册 / 重名拒 / 名字集排序稳定 / `Specs` 保序 / 未命中 `ok=false` / 与 `ToolResolver` 接口编译期断言 |
| `tool/filetools_test.go` | 14 | I1~I14：三件工具入参 / 出参 / 失败类别逐字（`not_found` / `sandbox_denied` / `runtime_error`）；`InputSchema` 与常量逐字；`write_file` 父目录不存在；**写入经相对根句柄**（把父目录换成指向工作区外的软连接后写入仍失败）/ `list_dir` 不递归、不跟随软连接 |
| `storage/tool_invocations_test.go` | 8 | J1~J8：两阶段写 / `pending` → 终态同账一行 / `error_kind` 只取七值 / `result_json` **截断标记在值内**（不写不存在的列）/ 失败原文截断 4 KB / 入参原样 / 回填失败保留 pending / 无 `round` 列（列清单断言） |
| `storage/run_store_test.go` | 6 | K1~K6：建 `running` 行 / 终态更新全部列 / **只把仍为 running 的改 failed**（已结束状态不被覆盖）/ 无 `final_text` 列（列清单断言）/ 时间 UNIX 毫秒 / `trigger` 取值 |
| `storage/snapshot_store_test.go` | 5 | L1~L5：insert-if-absent 幂等 / `UNIQUE(agent_name, hash)` 冲突不覆盖 / `content` 存 AGENT.md 全文 / `manifest_json` 往返 / 写失败如实上抛 |
| `storage/recovery_test.go` | 12 | M1~M12：六步逐条 / **按位置判组（不按 id）** / 同一短事务 / 幂等（连跑两次零变化）/ **只有旧 Run 审计行 → "未执行（进程中断）"** / **当前 Run 有 pending 行 → "结果未知"** / pending → unknown 两表 / 只改仍 running 的行 / 组看消息·Run 看表 / 无状态 Run 也收尾 |
| `storage/session_store_test.go` | 8 | N1~N8：`seq` 自增单调 / `run_id` 落列 / 跨重启读回 / tool 消息保 `tool_call_id` / `ProviderExtra` 往返保真（`UseNumber`）/ 会话行 upsert / `ListSessions` 排序 / 无状态调用不写 `messages` |
| `workspace/snapshot_test.go` | 8 | O1~O8：**同一份原始 AGENT.md** 解析 Profile 与正文（改盘后快照用新盘内容，不与旧 `entry.Profile` 混用）/ `content` = 原文 / manifest 覆盖四类文件 / **同内容不同 `now` → 同 hash**（时间不入哈希）/ 文件读失败 → 拒绝启动 / `SectionOne` 签名与既有行为不变 / L2/L3 与 `scripts` 不进清单 / `manifest_json` 规整序列化稳定 |
| `workspace/agent_test.go`（追加） | 4 | P1~P4：注入 10 名字 → 兼容样例注册成功 + `tool not registered` 零命中；注入 3 名字 → 拒且 reason 逐字；`Tools` 为空 + 未声明 `tools:` → 仍注册；`Tools` 为空 + 声明了 `tools:` → 整只不注册 |
| `channel/cli_test.go` | 8 | Q1~Q8：真控制台 UTF-8 往返（本机读数）/ 管道输入按 UTF-8 读 / 管道输出按 UTF-8 写 / EOF 退出 / Ctrl-C → cancelled / `IsTerminal` 不猜环境变量 / 平台分支编译（见 §3.3 三平台命令） |
| `cli/chat_test.go` | 8 | R1~R8：`--message` 单条 / 多轮渲染模板 / 退出码 / Ctrl-C → cancelled 且退出码 0 / **`deriveSessionID` 无碰撞**（`user=a-b,profile=c` ≠ `user=a,profile=b-c`）/ 派生结果字符集安全 / 派生在准入之前（同参数两次调用同 id）/ 60s deadline 生效 |
| `cli/session_test.go` | 4 | S1~S4：`session list` 渲染 / 空库文案 / 排序 / 持锁时仍可用（只读命令不取锁） |
| `app/wire_test.go` | 5 | T1~T5：重路径顺序（取锁 → 打开 → 迁移 → 恢复）/ 注册池名字集流入装载器 / 端口注入 / 轻路径零装配 / 快照先落库再开跑 |
| `provider/client_test.go`（追加） | 4 | U1~U4：**带 tools 的出站请求体逐字编解码** / **空池不带 `tools` 键（旧行为不变）** / 入站 `tool_calls` 解析 / `ProviderExtra` 两跳往返 |

**测试分层与纪律**：全部默认层（`go test -race`），**不访问真实网络与真实模型**——`provider` 侧以 fake client 注入（实现 `core.ProviderClient`）；真实模型调用只出现在 `evals/` 的真模型门控档与 §6 人工项。时间一律显式传入（`now time.Time` / `Clock` 注入）。夹具内联构造（`t.TempDir()`），不扩 `testutil` 公开面。**测试函数名英文（`TestXxx_yyy`），中文验收点以注释保留。**

### 3.3 配置与工程

- **`go.mod`：零新增坐标**（宪法 VIII + 依赖最小化）。逐条核实（2026-10-16）：
  - `golang.org/x/sys v0.48.0` —— **已是直接依赖**（US-3 引入），本 US 复用：`windows`（Job Object：`CreateJobObject` / `AssignProcessToJobObject` / `SetInformationJobObject` / `TerminateJobObject` / `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`；控制台：`GetConsoleMode` / `ReadConsole` / `WriteConsole` / `SetConsoleMode`）与 `unix`（`IoctlGetTermios` / `IoctlSetTermios` + 平台常量 `TCGETS`（Linux）/ `TIOCGETA`（Darwin）」——**模块缓存离线核实通过**；
  - `golang.org/x/term` —— **不在**模块缓存、且不在技术栈句内 → **不引入**；`IsTerminal` 自实现于 `x/sys` 之上；
  - `github.com/mattn/go-isatty v0.0.24` —— 现为 **indirect**（cobra 传递），**不提升**为直接依赖；
  - `modernc.org/sqlite` / `sashabaranov/go-openai` / `testify` —— 既有点名坐标，沿用。
- **`basalt.yaml` / `basalt.yaml.example`**：`http` 段补 `allowed_domains`（**沿用 Operations §2.2 已列键名**，注释点名 US-5 的 `http_get` / `http_post` 消费）；`sandbox.cross_agent_read` / `shell.*` / `budget.*` 沿用既有键面；**不新增任何键名**（§2.4 注 6）。
- **`Makefile`：零改动**（`cover` 已按包名判 `core` / `tool` / `storage` ≥70%、`channel` ≥50%；`test-integration` 无新增用例）。**目标平台编译检查**（人工项 / CI 可选，**本 US 不进 `make check`**）：

  ```bash
  CGO_ENABLED=0 GOOS=linux   go build ./...    # Linux 控制台分支（TCGETS）+ runner_unix.go
  CGO_ENABLED=0 GOOS=darwin  go build ./...    # Darwin 控制台分支（TIOCGETA）
  CGO_ENABLED=0 GOOS=windows go build ./...    # Windows 控制台分支 + Job Object
  ```

  若要把它纳入门禁，属**构建步骤变更**——走 `/us-register 4` 的第 2 项判断题（由用户裁决）。
- **`scripts/run-evals.sh`**：评测包装脚本（可选；`go run ./evals` 即可）。
- **credential**：本 US 出站请求复用 US-2 的 `${ENV_VAR}` 解析面；**凭证不进日志 / 审计 / 错误文案**。

### 3.4 表

**零迁移脚本**——六表（`sessions` / `messages` / `runs` / `config_snapshots` / `llm_calls` / `tool_invocations`）US-1 已建齐；本 US 只**读写**（已发布迁移只追加、不得改写——宪法 IX）。

**列级口径（逐项核对既有 DDL，本文不得改写）**：

- `runs`：**无 `final_text` 列**。最终回答只存在 `RunResult.FinalText` 与 `messages` 的 assistant 行中——`FinishRun` 不接收 `finalText` 参数。
- `tool_invocations`：**无 `round`、无 `truncated` 列**。截断标记放在既有 `result_json` 表示内（`{"content": …, "retryable": …, "truncated": true}`），不新造列。
- `config_snapshots`：**主键 `id`（自增）、唯一键 `(agent_name, hash)`**；`hash` = `manifest_json` 规整序列化的 sha256；`content` = **AGENT.md 全文**；落库走 insert-if-absent（§2.4 注 5）。
- `messages`：`run_id` 列是恢复的判据来源，故追加接口**显式承载 `runID`**（§3.1 `MessageStore`）。
- `llm_calls`：`round` 语义 = 该 Run 内第几轮（**1 起**），由 `ReActLoop` 传入（轮号 = 循环的 `round`）。

**状态与分类字面量（**两套不得混用**）**：

| 集合 | 取值 | 出处 |
| --- | --- | --- |
| 审计 `status`（两表） | `pending` / `success` / `failed` / `unknown` | 契约 §7.2 + `schema.sql` 列注释（**无 `cancelled`**） |
| `error_kind`（七类） | `sandbox_denied` / `timeout` / `not_found` / `invalid_input` / `external_error` / `runtime_error` / `unknown` | 契约 §7.3（**逐字，不得增删**） |
| Run `status` | `running` / `completed` / `limit_reached` / `cancelled` / `failed` | 契约 §2 状态机 |

**`error_kind` 取值口径**（实现与测试对齐；**服从契约 §7.3 的例子列**）：

| 情形 | `status` | `error_kind` |
| --- | --- | --- |
| 沙箱拒绝（`SandboxViolationError`） | `failed` | `sandbox_denied` |
| 文件 / 脚本目标不存在 | `failed` | `not_found` |
| 入参不是合法 JSON 对象 / 必填缺失 | `failed` | `invalid_input` |
| 单工具超时、上下文取消或截止（含 Run 级取消——**与 `ProviderService.classifyError` 同口径**：ctx 错误一律归 `timeout`） | `failed` | `timeout` |
| HTTP 5xx / 供应商 4xx-5xx（US-5 起） | `failed` | `external_error` |
| 进程启动失败 / 编码错误 / 其余本仓内部错误 | `failed` | `runtime_error` |
| 崩溃恢复把 `pending` 归一化 | `unknown` | `unknown` |

### 3.5 前序改造点与后续占位

**前序改造点（逐条软门禁——须用户确认后方可动手；**原有行为与回归断言一律保留**）**

| # | 改造点 | 触及面 | 理由与依据 | 影响既有测试 |
| --- | --- | --- | --- | --- |
| 1 | `workspace.AgentLoaderOptions` 增 `Tools []string` | US-3 已交付的导出结构体（**加字段，不删不改既有字段**） | 技术方案 §8.2 规则 4 门禁在本 US 接入（§2.4 注 4） | `TestAgentLoader_LoadsCompatSampleAgent` **改造为注入 10 名字** + 新增正负例；其余 8 条校验用例不受影响 |
| 2 | `core.ChatRequest` 增 `Tools []ToolSpec`；`provider.buildRequestBody` 输出 `tools` 键；**`ChatRequest.ConfigHash` 由"恒空"变为运行期填充** | US-2 已交付的请求结构（**加字段**；`ConfigHash` 是**注释与语义**变更——字段已在，`llm_calls.config_hash` 自此有值） | 技术方案 §4.2 段 4 + US-2 文档"类型与组装归 US-4"的占位登记 | `provider` 既有出站用例**全部保留**（空池不带键 = 旧行为逐字不变），**追加** 4 条（§3.2 U1~U4） |
| 3 | `workspace.ContextLoader` 增 `Snapshot`（`SectionOne` 委托，**签名不变**） | US-3 已交付的导出方法（**新增方法**） | US-3 §2.2 约束 6/7 明写"快照冻结与清单哈希归 US-4" | `context_loader_test.go` 既有 7 条**全部保留**；新增 `snapshot_test.go` 8 条 |
| 4 | **配置结构扩展**：`internal/cli/config.go` 的 `Sandbox` / `Shell` / `HTTP` 由 `map[string]any` 落为**结构化字段**（键名逐字沿用既有面：`sandbox.cross_agent_read` / `shell.allowed_commands` / `shell.interpreters` / `shell.env_passthrough` / `http.allowed_domains`） | US-1 已交付的配置装载面（**结构化，不改键名、不删键**） | §2.4 注 6（裁决 ②：沿用已有契约键） | `cli/config_test.go` 既有用例保留；**新增**三段配置的解析与缺省用例 |
| 5 | `internal/cli/Deps` 与 `root.go` 增 `chat` / `session` 注册 | US-1 / US-3 已交付的命令面（**加命令组**） | 技术方案 §8.7（12 命令） | `cli/ports_test.go` 的"`cli` 不得 import `workspace`"判据自动覆盖新命令 |
| 6 | `internal/app` 增 `wire.go` 重路径接线 | US-1 已交付的装配层（**新增文件**） | 技术方案 §8.7 + US-3 留给 US-4 | `app/ports_test.go` 既有判据沿用 |
| 7 | `internal/storage/doc.go` 事务纪律段更新 | US-1 已交付的包说明（**只增文字**） | 契约迁移纪律第 7 条 | 无 |
| 8 | **既有类型复用**（不重造）：`core.Message` / `ToolCall` / `Usage` / `ChatResponse` / `ProviderClient` / `LLMCallStatus` / `ErrorKind` / `LLMCall` / `LLMCallResult` / `LLMCallStore` 原样复用；`ProviderExtra` 保持 `map[string]any` | US-2 已交付的声明面（**零改动**——本行是**禁止事项**的登记：动词是"不得重造"） | 裁决要求：不重造既有类型 | 无（不动即无影响） |

#### 后续占位（本 US 只留装配位与文本占位，不实现）

| # | 占位 | 接入模块 |
| --- | --- | --- |
| 1 | 段 2（记忆注入）在 `PromptBuilder` 的装配位**恒空** | US-6 |
| 2 | `ToolRegistry` 只注册 `read_file` / `write_file` / `list_dir` 三件；`shell` / `http_get` / `http_post` / `notify` / `manage_schedule` 归后续 | US-5 / US-8 / US-10（`use_skill` 宪法 IV **永久禁**） |
| 3 | HTTP 逐跳校验的**驱动**（`http_get` / `http_post` 工具本体）不在本 US；`WhitelistPolicy` 的 `HTTP_REQUEST` 分支本 US 全量实现 + 单测覆盖 | US-5 |
| 4 | 热加载 watcher 与快照版本保留策略 | US-11（**不改内容寻址算法**） |
| 5 | REST 端点 / 409 / 429 的 HTTP 面 / 认证 / 多租户 | US-9 |
| 6 | 管理台的运行视图 / 审计查询 / 多文件提交（`.staging` 的**产生方**） | US-12（本 US 只处理**残留**） |
| 7 | 定时触发与 `RunTimeout=300s` 一条路径 | US-10 |

## 4. 验收测试（Harness）

### 4.1 测试分层标准

| 层 | 是否访问真实网络 / 模型 | 内容 | 命令 |
| --- | --- | --- | --- |
| 默认层（本 US 全部自动化用例） | **否**（fake provider client；无 `net.Listen`；无外网） | §3.2 全部 **185** 条 | `make test`（= `go test -race ./...`） |
| 集成层 | 否（仍不访问网络） | 本 US **无新增** | `make test-integration`（跑既有） |
| 评测层（`evals/`） | 默认 **否**（fake 后端）；真模型档**显式门控** | 用例集 + 可执行断言 | `go run ./evals --cases evals/cases` |
| 人工层（§6） | **是**（真实模型 / 真控制台 / 真二进制） | 验收 Demo + 评测重跑 + 平台读数 | 见 §6 |

**判卷纪律**：① 格式判卷**先查退出码、再判输出**；② `-race` 与 `CGO_ENABLED=0` **分属不同命令**；③ 覆盖率 `core` / `tool` / `storage` ≥70%、其余 ≥50%（`make cover` 按包判失败）。

### 4.2 测试类 → 验收点映射表

| FR / 验收点（来源） | 测试编号 | 判定 |
| --- | --- | --- |
| 需求 §5.4 循环终止三态（completed / limit_reached / cancelled） | A1 A3 A7 A12 | 状态码与 `Reason` 逐条断言 |
| 契约 §2 状态机（终态不回退；达上限非错误） | A1 A2 K3 M6 | 转义表断言 + 行值不变断言 |
| 技术方案 §4.1 循环逐块同构 | A2 A4 A5 A6 A8 A9 A11 | 每块至少一条用例（**轮内 Build / 先落 assistant / 执行前查取消与预算 / 取消优先**） |
| 宪法 II 追加唯一性 | A11 D7 | `SELECT count(*)` 级断言 + 内存历史比对 |
| 宪法 II/V LLM 审计归属既有 `ProviderService` | D10 + 收尾 grep（`BeginCall` 在 `internal/core` 零命中） | 单一入口断言 |
| 技术方案 §4.4 三条硬约束 | B9 N4 N5 U3 U4 | `tool_call_id` / `reasoning_content` / `ProviderExtra` 往返保真 |
| Compatibility §5.3 判据 3（清空 `reasoning_content` → 400） | U3 + §6 人工项 3 | 出站请求体逐字断言"含 `reasoning_content`"（负例属人工项） |
| 契约 §7.2 审计两阶段写 + fail-closed | B2 B3 B7 J1 J2 | `pending` 行先于出站动作；**LLM 预写失败 → Run failed；工具预写失败 → 拒执行 + 回填 + Run 继续** |
| 契约 §7.3 七 `error_kind` + §3.4 取值口径 | B5 B6 B7 B8 J3 | 取值逐条 + 字面量集合断言 |
| 契约 §4.4 Option A 五分支 | E3 E4 E5 E6 M3 M4 M5 M6 | 逐条 + 幂等 |
| 契约 §4.3.2 六步恢复 | M1~M12 | 逐步断言 + 连跑两次零变化 |
| 技术方案 §6.1 工具四方法 | H6 I1~I4 | 编译期签名钉住 + `InputSchema` 逐字 |
| 技术方案 §6.5 授权矩阵 | F1~F18 | 每格一条用例（**含跨 Agent 授权与硬拒 `bash`/`sh`**） |
| 技术方案 §6.5 执行控制 | G1~G10 | 环境白名单 / 杀进程树 / 读取中限流 |
| 技术方案 §6.5 四段式拒绝 | F18 B1 B2 | 文案**逐字**比对 + `sandbox_denied` |
| 宪法 VI 授权与执行分离 | H6 + `wire_test` T3 | 编译期：`filetools.go` 只引用 `core.Policy` 接口 |
| 宪法 III 显式映射 | D1 T2 | 装配置映射表；运行时查表 |
| 技术方案 §11.5 + 契约 DDL 读即所记 | O1 O3 O4 O7 O8 | 同次读取（改盘后不得混用旧 Profile）+ 同内容同 hash |
| 技术方案 §4.2 段 1~4 | C1~C5 | 顺序 / 空池 / 两级裁剪 |
| 契约 §6 上下文预算 | C5 C6 C7 C8 | 按组继续裁 / 不可裁部分超限 → 拒绝启动（零 LLM 调用） |
| 宪法 VIII `core` 只依赖标准库 | 收尾 grep | `go list -f '{{.Imports}}' ./internal/core` 只出标准库 |
| 技术方案 §8.4 CLI 编码纪律 | Q1~Q7 + §3.3 三平台编译命令 | 真控制台读数（本机）+ 管道 UTF-8 + 平台分别适配 |
| 技术方案 §8.7 轻 / 重路径 | S4 T1 T4 | 取锁顺序 + 只读命令不取锁 |
| 需求 §5.9 会话跨重启 | N3 N7 | 重开库读回 |
| Compatibility §5.1 判据 2（10 名字全命中） | P1 P2 | 注入名字集 → 注册成功 + 零 `tool not registered` |
| 契约 §3 并发（同会话串行 / 跨会话并行；不排队） | D3 D4 | 忙回错不排队；并发泄漏见 §4.3 |
| 契约 §1.2 返回语义（error 只用于调用方问题） | D1 D2 D3 D4 | 调用方问题 → error；运行期失败 → Status |
| 契约 §5 SessionID 派生（CLI） | R5 R6 R7 | 无碰撞 + 字符集安全 + 准入前确定 |
| 契约 §1.1 无状态语义（空 SessionID） | D5 N8 M10 | 不落会话历史；Run 与审计仍落 |

### 4.3 关键回归测试写出代码

> 测试函数名英文（`TestXxx_yyy`），中文验收点以注释保留。以下六条是"单请求测试测不出、并发或跨进程才暴露"的守点（易漏维度核对表：并发泄漏类回归 / 留痕顺序 / 失败信息回填 / 越权面）。

```go
// TestReActLoop_AppendsAssistantBeforeExecutingTools 守"留痕顺序"：
// 验收点（契约 §4.3.2 半截组判据的前提）——assistant 消息（带 tool_calls）必须**先**落库、
// **再**执行工具。若反转，崩溃后恢复扫描找不到 assistant 行，半截组无从判定。
// 同时钉住"最终回答不丢"：一次运行 = 2 条 assistant（带工具调用的一条 + 最终回答的一条）。
func TestReActLoop_AppendsAssistantBeforeExecutingTools(t *testing.T) {
    h := newLoopHarness(t)                    // fake provider：第 1 次返回 tool_call，第 2 次返回纯文本
    h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"notes.txt"}`))
    h.provider.onCall(1, textResponse("done"))
    // 工具执行当时：assistant 行已存在，且此刻**只有 1 条**（工具尚未产生任何 assistant）
    h.tools.onExecute = func(call int) {
        require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleAssistant),
            "执行工具前 assistant 消息必须已落库（留痕顺序）")
    }
    res, err := h.service.Run(ctx, h.req("读一下 notes.txt"))
    require.NoError(t, err)
    require.Equal(t, core.StatusCompleted, res.Status)
    require.Equal(t, "done", res.FinalText)
    // 运行结束：**2 条** assistant（工具轮 1 条 + 最终回答 1 条）——断言 1 条会把"丢最终回答"判成通过
    require.Equal(t, 2, h.store.countByRole(t, h.sessionID, core.RoleAssistant),
        "最终回答必须落库：一次工具轮 + 一次纯文本完成 = 2 条 assistant")
    require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleTool))
}

// TestReActLoop_CancelBeforeBudgetCheck 守"取消优先于预算判定"：
// 验收点（契约 §4.1 / §4.4）——取消与预算同时成立时判 cancelled 而非 limit_reached，
// 组内未执行调用取"已取消"合成文本（**不是**预算文本）。
func TestReActLoop_CancelBeforeBudgetCheck(t *testing.T) {
    h := newLoopHarness(t)
    h.budget.MaxToolCalls = 1
    h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`,
        toolCallResponse("c2", "read_file", `{"path":"b.txt"}`)))   // 一轮两个调用
    h.tools.onExecute = func(call int) { h.cancel() }               // 第一个执行完即取消
    res, err := h.service.Run(ctx, h.req("读两个文件"))
    require.NoError(t, err)
    require.Equal(t, core.StatusCancelled, res.Status, "取消优先于预算：不得记为 limit_reached")
    require.Equal(t, core.TextToolCancelled, h.store.lastToolContent(t, h.sessionID),
        "组内未执行调用取『已取消』合成文本")
}

// TestToolExecutor_PreWriteFailureRefusesExecution 守"审计 fail-closed"（宪法 V + 契约 §7.2 工具侧）：
// 验收点——审计 pending 预写失败时，工具**一次都不得执行**；但**Run 不因此失败**：
// 失败结果必须回填模型并说明审计不可用（模型看到失败 -> 下一轮收敛为最终回答）。
func TestToolExecutor_PreWriteFailureRefusesExecution(t *testing.T) {
    h := newLoopHarness(t)
    h.audit.failStart = errors.New("disk full")                    // 注入预写失败
    h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
    h.provider.onCall(1, textResponse("工具不可用，我直接回答"))
    res, err := h.service.Run(ctx, h.req("读文件"))
    require.NoError(t, err)
    require.Equal(t, core.StatusCompleted, res.Status, "工具审计不可用不判 Run 失败（契约 §7.2）")
    require.Zero(t, h.tools.executed, "预写失败时工具不得执行（fail-closed）")
    require.Zero(t, h.store.countToolInvocations(t))
    require.Contains(t, h.store.toolContent(t, h.sessionID, "c1"), "审计不可用",
        "失败信息必须回填进对话上下文（模型看不到结果就不会收敛）")
}

// TestToolExecutor_RejectsToolOutsideAgentToolSet 守"运行时 ToolSet 归属校验"：
// 验收点——模型返回"全局已注册、该 Agent 未声明"的工具名时，不得执行、不得产生审计行，
// 且回填四段式拒绝文案（加载期验证挡不住注册池与快照不同源的情形）。
func TestToolExecutor_RejectsToolOutsideAgentToolSet(t *testing.T) {
    h := newLoopHarness(t)
    h.registry.register(writeFileToolSpy(h))       // 全局注册，但**不在**本 Agent 的 ToolSet
    h.toolSet = []string{"read_file"}
    h.provider.onCall(0, toolCallResponse("c1", "write_file", `{"path":"x","content":"y"}`))
    h.provider.onCall(1, textResponse("改用其它方式"))
    res, err := h.service.Run(ctx, h.req("写个文件"))
    require.NoError(t, err)
    require.Equal(t, core.StatusCompleted, res.Status)
    require.Zero(t, h.spies.writeFileExecuted, "未声明的工具不得执行")
    require.Zero(t, h.store.countToolInvocations(t), "执行之前的拒绝不产生审计行")
    require.Contains(t, h.store.toolContent(t, h.sessionID, "c1"), "tools",
        "拒绝文案点名 AGENT.md 的 tools 声明（四段式放行指引）")
}

// TestRecovery_RepairsHalfGroupByPosition 守"按位置判组 + 修复同事务 + 分类按审计行"：
// 验收点（契约 §4.3.2 步骤 ②③ + §4.4）——半截组按 seq 位置判定（同会话里另一 Run 用同一个
// tool_call_id 的"陈旧"行不得被误配）；只有旧 Run 的审计行 → 当前调用是"未执行（进程中断）"；
// 当前 Run 有 pending 行 → "结果未知（进程中断…）"；Run 修复只覆盖**仍为 running** 的行。
func TestRecovery_RepairsHalfGroupByPosition(t *testing.T) {
    db := newTestDB(t)
    seedRun(t, db, "run-1", "running")                  // 当前 Run
    seedRun(t, db, "run-alone", "completed")            // 已结束的 Run——不得被覆盖
    seedMessage(t, db, "s1", 1, "user", "读文件", "run-1")
    seedMessage(t, db, "s1", 2, "assistant", "", "run-1")            // 本轮组：assistant(c1) → 缺 tool 行
    seedToolInvocation(t, db, "run-0", "c1", "success", "【陈旧】上一次运行的同 id 结果")
    report, err := storage.Recover(ctx, db, testLogger(t), fixedNow)  // 参数与 §3 声明一致
    require.NoError(t, err)
    require.Equal(t, 1, report.RepairedGroups)
    require.Equal(t, 1, report.ClosedRuns)
    got := loadMessages(t, db, "s1")
    require.Equal(t, "未执行（进程中断）", got[2].Content,
        "只有其它 Run 的审计行 = 本 Run 没有行 = 确定没执行（按位置判组，不得误配陈旧行）")
    require.Equal(t, "completed", loadRunStatus(t, db, "run-alone"), "已结束的 Run 不得被覆盖")

    // ② 当前 Run 有 pending 行 → 同 id 的调用必须写"结果未知"（不得写成"未执行"）
    seedRun(t, db, "run-2", "running")
    seedMessage(t, db, "s1", 3, "user", "再读一次", "run-2")
    seedMessage(t, db, "s1", 4, "assistant", "", "run-2")
    seedToolInvocation(t, db, "run-2", "c2", "pending", "")
    _, err = storage.Recover(ctx, db, testLogger(t), fixedNow)
    require.NoError(t, err)
    got = loadMessages(t, db, "s1")
    require.Equal(t, "结果未知（进程中断，无法确定是否已产生外部副作用）", got[4].Content,
        "执行过但终态未落 → 结果未知，不得写成『未执行』")

    // ③ 幂等：连跑第二次零变化
    before := dumpMessages(t, db, "s1")
    _, err = storage.Recover(ctx, db, testLogger(t), fixedNow)
    require.NoError(t, err)
    require.Equal(t, before, dumpMessages(t, db, "s1"))
}

// TestReActLoop_NoGoroutineLeakOnCancel 守"并发泄漏类回归"：
// 验收点（宪法 VII 同步执行）——取消后**拥有的资源**（工具子进程、收尾写入）全部退出；
// 必须**真正触发取消**（等工具已启动的信号，而不是"发完就取消"），并限时等待 Run 返回；
// goroutine 总数只作**辅助观察**（它会把测试框架自身的 goroutine 波动算进来，不能当主判据）。
func TestReActLoop_NoGoroutineLeakOnCancel(t *testing.T) {
    h := newLoopHarness(t)
    started := make(chan struct{})
    h.tools.blockUntilCancel = true
    h.tools.onStarted = func() { close(started) }        // 工具已启动的信号
    before := runtime.NumGoroutine()
    done := make(chan struct{})
    var res core.RunResult
    var err error
    go func() { res, err = h.service.Run(ctx, h.req("跑一个慢工具")); close(done) }()
    <-started                                            // 等"工具已启动"再取消——否则测不到取消路径
    h.cancel()
    select {
    case <-done:
    case <-time.After(3 * time.Second):
        t.Fatal("取消后 Run 未在限时内返回（取消传播链路断了）")
    }
    require.NoError(t, err)
    require.Equal(t, core.StatusCancelled, res.Status, "Run 自身取消 → cancelled")
    // 限时等待"所拥有的资源"退出：工具子进程已回收、收尾写入已结束（用 harness 的等待点，不比 goroutine 总数）
    h.tools.waitExited(t, 2*time.Second)                 // 工具执行体已返回
    h.audit.waitIdle(t, 2*time.Second)                   // 收尾审计写入已结束
    // 辅助观察：goroutine 数不高于基线 + 少量余量（辅助判据，不作为唯一断言）
    require.LessOrEqual(t, runtime.NumGoroutine(), before+2, "辅助观察：取消后不得残留成片 goroutine")
}
```

## 5. 范围边界

**核心阶段不做**（依据：技术方案"核心阶段不做"各节 / 需求 §6 / 契约 §3.3）：

| 不做项 | 依据 |
| --- | --- |
| 工具**并行调用**（一轮内多工具并发执行） | 技术方案 §4.1（串行执行）；契约 §3.3 |
| 幂等键 / 重试 / 重放 | 契约 §3.3（重传 = 新 Run）；技术方案 §3.3 |
| 流式响应（SSE / 假流式） | 技术方案 §3.3 / §7.5；陷阱 #37 |
| 准入**排队**（同会话忙时不排队、实例满时不排队） | 契约 §3 / §4.3；技术方案 §4.6 |
| 上下文**总结压缩**（超限只裁剪，不生成摘要） | 技术方案 §4.7（裁剪最小单位 = 完整工具交互组） |
| 会话记忆压缩 / 归档 | 需求 §5.5（记忆归 US-6）；技术方案 §4.7 |
| 容器化沙箱（Docker / 命名空间隔离） | 技术方案 §6.5（v1 唯一实现 `WhitelistPolicy` + `LocalRunner`）；扩展阶段 F |
| 网络文件系统工作区（NFS / 共享盘） | 技术方案 §9.3（真实路径与锁语义在 NFS 上不成立） |
| 多副本部署 / 分布式锁 | 扩展阶段；契约 §4.3.1（实例锁是**单实例**语义） |
| Bearer Token 认证 / 多租户隔离 | 技术方案 §7.5；扩展阶段 |

**本 US 不做**（归后续 US，接入点见 §3.5 占位表）：

| 不做项 | 归属 |
| --- | --- |
| `shell` / `http_get` / `http_post` 工具本体 / `tool list` | US-5（沙箱**已在本 US 正式实现**，US-5 只接工具本体） |
| 记忆召回注入（段 2）与记忆写入 | US-6 |
| MCP 客户端与 `mcp_servers` 连接 | US-7 |
| `notify` 工具与通知渠道 | US-8 |
| REST 端点 / 409 / 429 / 认证 / 管理台 | US-9 / US-12 |
| `manage_schedule` 与定时触发 / `RunTimeout=300s` 路径 | US-10 |
| 热加载 watcher 与配置快照的版本保留策略 | US-11 |
| `use_skill` 工具 | **永久不做**（宪法 IV） |
| `AGENT.md` 内 `${ENV_VAR}` 解析 | 无文档依据，不引入（US-3 已定案） |

## 6. 验证与验收

### 6.1 剩余人工项

执行人 = 用户；每项须附**本轮当场**命令与输出：

1. **门禁七目标全绿**：`make check` 退出码 0（`fmt lint sec test test-integration cover build`）；覆盖率读数须含 `internal/core` / `internal/tool` / `internal/storage` **首次** ≥70% 的实测输出行；`internal/channel` ≥50%；
2. **验收 Demo 七项**（需求 §13.1 / 编程指南 §4.4，真模型 + 真控制台）：
   - ① `basalt chat` 多轮对话，AGENT.md 身份生效（自称与正文一致）；
   - ② 模型调 `read_file` 读工作区文件并**在回答里用上内容**；
   - ③ 越界路径（如 `../../etc/passwd`）→ **四段式拒绝**可见 + 审计落 `failed` + `error_kind=sandbox_denied`（`sqlite3` 查库读数）；
   - ④ 思考模式模型连续多轮工具调用**不报 400**（`reasoning_content` 回传有效）；
   - ⑤ 重启进程后 `basalt chat` 同一会话历史仍在并可继续；
   - ⑥ `evals/` 首批用例集跑通：**可执行断言逐例 PASS**，结果文件含每例 RunID 与审计关联，资源用量（token / 调用次数）齐全；
   - ⑦ 注入用例：越权尝试被四段式拒绝且审计落 `failed`；
3. **Compatibility §5.3 判据 3 的负例**：人工把 `reasoning_content` 清空后重发 → **必须**复现供应商 400（证明该字段回传是**必要**而非冗余）；
4. **Windows 控制台编码读数**：真控制台中文输入 / 输出无乱码；`echo 中文 | basalt chat --message …` 管道侧按 UTF-8 正确解析（**本机读数**，CI 不覆盖）；
5. **三平台编译检查**：§3.3 三条 `CGO_ENABLED=0 GOOS=… go build ./...` 全部退出码 0（Linux `TCGETS` / Darwin `TIOCGETA` / Windows 控制台与 Job Object 分支各被编译到一次）；
6. **进程树强杀读数**：起一个会派生子进程的命令，超时后**父子进程都消失**（Windows Job Object 生效证据——`tasklist` 前后对比；关联失败路径的清理顺序由单测判卷）；
7. **装配确认**：`chat` / `session list` 已注册、`--help` 文案可用、重路径顺序（锁 → 打开 → 迁移 → 恢复）在日志里可见；
8. **token 估算参考读数**：用目标模型分词器对五类夹具各测一次，把读数写回夹具注释（标注日期与模型名）；与估算值的相对偏差记入本项输出（**不承诺上界**——§2.4 注 9）；
9. **变动范围核对**：`git status --short` 与 §3 交付物清单逐项对照、**无探针残留**（探针建在仓库外、本 US 收口时删除）；
10. **凭证复核**：`grep -rn 'api_key' --include='*.go' internal` 的命中处**无明文**，只走 `${ENV_VAR}` 解析面；
11. **文档未验证项如实登记**：markdownlint 本机不可用（从 VSCode markdown 诊断自查）；`-race` 与 `CGO_ENABLED=0` 分属不同 job 已由 Makefile 天然分离。

### 6.2 本文固定口径（无权威字面量，逐字冻结；仿 US-3 先例，供实现与测试对齐）

1. **`session_id` 派生**（契约 §5 只说"由 `channel+user+profile` 派生"）：采用**规范元组编码 + 稳定哈希**——`sha256("basalt/session/v1" ‖ 0x1F ‖ <len(channel)>:channel ‖ 0x1F ‖ <len(user)>:user ‖ 0x1F ‖ <len(profile)>:profile)` 取十六进制前 16 位，前缀渠道名：`<channel>-<hex16>`（如 `cli-3f9a1c07b2d84e55`）。**长度前缀消除拼接歧义**（`user=a-b, profile=c` 与 `user=a, profile=b-c` 必然不同 id——连字符拼接会碰撞）；**由命令面在准入之前算好**（`AgentService` **不派生**，`req.SessionID` 为空即**无状态**，契约 §1.1 语义保留）；CLI 的 `user` = 当前 OS 用户名（取不到 → `unknown`）；profile 名取快照 `AgentName`。
2. **`RunID` 形态**：`<channel>-<unix_millis>-<6 位 base36 小写>`（契约 §5 定"随机 6 位"，取值域本文冻结：`crypto/rand` 取 6 字符 `0-9a-z`）。
3. **`chat` 交互循环终止语义**：stdin EOF → 正常退出（退出码 0）；`--message` → 单条即退；Ctrl-C → 该次 Run `cancelled`（打印 `已取消`）→ **进程退出**（退出码 0——取消是预期动作而非错误）；每次 Run 建 **60s** deadline（§2.4 注 7）。
4. **`chat` 渲染**：单轮回复直接打正文；工具调用期间打印一行 `⚙ 调用工具 <name>…`（**不打印入参与结果全文**——脱敏纪律约束 26）；失败打印 `✗ <四段式/错误文本>`；`limit_reached` / `cancelled` 打印醒目提示 + 已得部分结果（契约 §1.3）。
5. **`session list` 渲染**：一行一会话 `<id>：<消息数> 条（最后活动 <2006-01-02 15:04:05>）`；末尾汇总 `共 N 个会话`；空 → `未发现会话`。时刻一律按 **UTC** 呈现（`.UTC()`，**不得**对时间值调本地时区方法——`.Local()` 与 `time.Local` 是同一条禁令的两种写法，见[工程规范 §1.6](EngineeringStandards.md)；2026-10-16 用户裁决），命令帮助文案与本节一并注明 UTC。
6. **日志键**（固定）：工具执行 WARN = `run_id` / `session_id` / `tool` / `reason` / `detail`；审计写失败 ERROR = `run_id` / `store`（`llm_calls` / `tool_invocations`）/ `op`（`start` / `finish`）；恢复 INFO = `unknowns` / `repaired_groups` / `closed_runs`；快照 INFO/Debug = `agent` / `hash` / `files`（清单条数）。
7. **截断标记形态**：结果超（生效的）`MaxOutputBytes` → content 尾部追加 `\n[已截断：输出超过 <N> 字节上限，此处不是完整输出]`（N = 生效上限）；审计侧在既有 `result_json` 内写 `"truncated": true`。**不承诺**"原始长度 N 字节"——读取中限流意味着从未读完整输出，报不出精确原始长度。
8. **`runs.trigger` 映射**（契约 §1.1 的 `RunRequest` 无 `Trigger` 字段，且**不得**加字段）：按 `Channel` 映射——`cli` → `user`；`web` → `api`；`scheduler` → `cron`；`admin` → `manual`。
9. **`estimateVersion` 与余量**：估算器版本常量 `v1`（口径见 §2.4 注 9）；上下文准入保留**余量** `reserve_tokens`（缺省 1024，代码常量）用于吸收协议包装开销；余量与估算器版本一并写进 `evals` 结果文件与快照 INFO 日志。

## 评审回合裁决落案表（2026-10-16）

**八项待决的裁决与落位**（原「待决事项」清单已按此表落案，文末待决为空）：

| # | 原待决项 | 裁决 | 落位 |
| --- | --- | --- | --- |
| 1 | `Policy` / `CommandRunner` 调用点 | **通过：放工具内部**；文件工具只需 `Policy`，命令执行才需 `Runner`；解释器 argv 校验属**执行侧** | §2.4 注 1；§2.2 约束 6 / 17；流程 D ⑤、流程 F ⑤ |
| 2 | 三段配置 YAML 形状 | **原建议不通过**：沿用已有契约键——域名键是 **`http.allowed_domains`**（**不得**另造 `sandbox.allowed_domains`），保留 `sandbox.cross_agent_read` / `shell.allowed_commands` / `shell.interpreters` / `shell.env_passthrough` 与已有 HTTP 配置；**重定向不得放宽超过 5 跳**（代码常量） | §2.4 注 6；§3.3；§3.5 第 4 项（配置结构扩展） |
| 3 | `config_snapshots` 是否落库 | **通过：本 US 落库**——关联 Run 开跑**前**保存，失败**拒绝启动**；按既有 DDL 写**原始 AGENT.md 全文**与 `manifest_json`（**不能只存哈希**） | §2.4 注 5；流程 A ②；§3.1 `snapshot_store.go` |
| 4 | CLI 的 Run 时限 | **60s**（调用方已有更早 deadline 时取更早者；Ctrl-C 不能替代资源时限） | §2.4 注 7；§6 固定口径 3 |
| 5 | `ToolTimeout` 的 Profile 覆盖键 | **本 US 不新增键**：`min(30s, 剩余 Run 预算, 更小的工具请求值)` 并**实际建立超时 context**；**单工具超时 → Run 继续**；Run 自身取消 / 超时 → `cancelled` | §2.4 注 8；§2.2 约束 18；流程 D ④ |
| 6 | token 估算口径 | **原建议不通过**"字节数 / 2 是保守上界"：改用**版本明确、覆盖完整出站字段**的估算器 + 五类夹具标定；**不宣称不会低估** | §2.4 注 9；§2.2 约束 14；§4.3 `prompt_test` 夹具；§6 人工项 8 |
| 7 | `evals` 形态 | **通过：YAML 用例集 + 执行判据**——每例含输入 / 夹具 / 预算 / **可执行断言**；默认 fake 可重复、真模型显式门控；复用 `AgentService`；输出结果文件（RunID + 审计关联）；**不新增评测表** | §2.4 注 10；§3.1 `evals/`；§4.1 评测层 |
| 8 | 成本读数口径 | **通过：token 用量 + 调用次数**，称**资源用量**、**不换算金额**；usage 缺失 / `pending` / `unknown` 标**计量不完整**，**不得**把库中默认 0 解释为零成本 | §1 价值 ⑦；§3.1 `evals/main.go` 结果字段 |

**六组 P1 修正的落位**：① 审计枚举 → §2.2 约束 5 + §3.4 字面量表（七类 + 状态四值，Run 状态单列）；② LLM 审计与消息追加职责 → §2.2 约束 3 / 4 + 流程 B（调用 `ProviderService`；Executor 返回结果、Loop 追加；回填失败记 ERROR 保留 pending）；③ 循环同构 → §2.3 流程 B 全块（Build 在轮内、快照每 Run 冻结、round 从 1 起、首次完成 iterations=1、执行工具前查取消与预算、预算不禁止纯文本完成、轮数裁剪、token 按组继续裁）；④ ToolSet 运行时校验与授权矩阵 → 流程 D ① + 流程 F（`cross_agent_read` 分支、保护规则先行、`bash` / `sh` 硬拒、`Action` 无 argv、父目录与相对根句柄写入）；⑤ 半截组与收尾 → 流程 E（error 返回、收尾 context、先判缺失、不把执行过的写成"未执行"）+ 流程 G ③（只改仍 `running`）；⑥ 交付物列与类型 → §3.1 既有类型复用行 + §3.4 列级口径（无 `final_text` / 无 `round` / 无 `truncated` 列；快照主键 `id` + 唯一键 `(agent_name, hash)`；`MessageStore` 承载 `run_id`）。

**三项 P2 补齐的落位**：跨平台终端 → §2.2 约束 21 + §3.1 `console_linux.go` / `console_darwin.go` + §3.3 三平台编译命令；进程树生命周期 → §3.1 `runner_windows.go` 的挂起—关联顺序与收尾顺序；输出与测试清单 → §6 固定口径 7（不承诺原始长度）+ §3.2 新增 provider tools 编解码 4 条、`RunStore` 6 条、`SnapshotStore` 5 条、ToolSet 校验与收尾 context 用例，**用例总数重统计为 184**（该次重统计的读数于 implement 期修订为 **185**——`core/loop_test.go` 实测 17 条，见 §3.2 注）。

## 收口补登（2026-10-16 用户裁决，T077 发现 F1~F5）

T077 收口时发现 §3.1 签名表之外**实际存在**的符号面五项（F1~F5）。用户裁决：**全部确认补登**——按实际符号、用途、调用方与回归证据登记；**不为了匹配旧清单撤销合理实现**。各行的「裁决边界」= 用户裁决原语句的落位口径。

| # | 实际符号 | 用途 | 调用方 | 回归证据 | 裁决边界 |
| --- | --- | --- | --- | --- | --- |
| F1 | `core.EstimateVersion = "v1"` / `core.ReserveTokens = 1024`（**导出**常量，`prompt.go:65-67`；§3.1 签名表原写未导出 `estimateVersion`，已同步） | token 估算器版本与上下文准入余量（§2.4 注 9 / §6.2 口径 9：写进日志与评测结果文件，**不落库**） | `core` 内部（`prompt.go:105` 准入、`:149` 生效上限）；`evals/main.go:383-384` 跨包直接引用**同一份常量**写进结果文件 | `TestPromptBuilder_EstimatorVersionReadable`（`prompt_test.go:362`：断言 `"v1"` / `1024` / 生效上限 = 预算 − 余量 / 零值兜底）；`prompt_test` 五类夹具 + 确定性 | 允许导出；评测**直接引用同一份常量**（不得复制字面量） |
| F2 | `app.App.providerOverride`（`wire.go:85-90`、`:484`）+ `app.Options.ProviderClient`（注入面） | 模型客户端的**进程内替身**（评测基座默认后端；生产路径恒 nil） | `evals/main.go:815-821`（fake 档以脚本回放客户端替换）、`app/wire_test.go:369`（包外装配注入） | `app/providers.go:71-83`：替身路**照建全表**——每个配置名各一条、统一包进 `provider.NewProviderService`（单条目表 = 绕开路由，被实现注释明确拒绝）；`TestWire_PortsReachRunEntryAndSessionStore` 走替身路把 `read_file` 闭环跑到落库并断言审计行 | 允许替换模型客户端，但**必须继续经过 ProviderService 的路由和审计** |
| F3 | `storage.DB` 的薄封装：`*DB.Migrate`（`migrate.go:103`）与 `*DB.Recover`（`recovery.go:144`） | 让装配层经**同一句柄**调迁移与崩溃恢复 | `internal/app/wire.go:160`（显式迁移）、`:168`（崩溃恢复）——重路径顺序内 | 只读句柄（write 为 nil）**显式拒绝**：`storage: 只读句柄没有写池，不得迁移…` / `…不得做恢复扫描…`（不是 nil 解引用崩溃）；表与迁移机制零改动（`git diff --numstat -- '*.sql'` 空） | 允许；复用既有迁移、恢复机制，**不暴露写池**，只读句柄**明确拒绝** |
| F4 | `cli.Deps.In` / `Deps.Out` / `Deps.Signals`（`deps.go:63-65`） | 命令面读写的**介质**与**中断来源**（真控制台或管道，分流由渠道层按句柄判定；三者为空回落进程标准流与渠道自建中断源） | `internal/cli` 全部命令；测试经 `newChatFixture`（`chat_test.go:84`）与逐次运行覆写 `deps.In/Out`（`chat_test.go:129`） | `TestChat_InterruptCancelsRunAndExitsZero`（`chat_test.go:460`，经 `Signals` 注入中断判"运行已取消 + 退出码 0"）；`TestChat_MessageFlagRunsOnceAndExits` 等经 In/Out 判 I/O 与 EOF | 允许，**仅服务输入输出和中断测试**；**不引入全局业务状态**（字段只承载介质与信号通道） |
| F5 | `workspace.skillCard.relativePath` / `raw`（`skill.go:50-51`） | 段一清单 path 列与该条 sha256 的来源——**同一次读取**的两个产物（`skill.go:152-163`） | `context_loader.go:179-180`（`Path: cards[i].relativePath`、`SHA256: sha256Hex(cards[i].raw)`） | `TestSnapshot_ManifestCoversEveryInjectedFile`（`snapshot_test.go:97`）：清单含 `skills/digest-format/SKILL.md`，且每条 sha256 = 盘上该文件内容哈希；`TestSnapshot_ManifestExcludesBodiesAndScripts` 守注入面 | 允许保存**同次读取**的路径与原始字节；**不扩大技能正文的注入范围**（注入面仍是 L1 三要素 name + description + 技能库真路径；`raw` 只喂 frontmatter 解析与哈希） |

F6~F9 三项收口发现不在补登范围：F6（`_ =` 写法）**不收录为陷阱**（逐处代码审查结论见 T077 行）；F7（`sec` 止于 gosec）与 F8（不变量 7 判据红）经修复后复跑清零；F9（quickstart 状态句）已更新为"已执行 + 两段读数并列"。

## 已决事项（无需裁决，本文直接落案）

1. **引擎落 `core`**：技术方案 §10.1 把 ReActLoop / PromptBuilder / ToolExecutor / AgentService 定在"核心抽象与引擎"→ 本 US 是 `core` 出现可执行语句的时点；纪律实质（只依赖标准库）不变，判据改为 `go list` 检查（§2.4 注 11）；
2. **覆盖率判卷已就位**：`scripts/check-coverage.sh` 按包名判 `core` / `tool` / `storage` ≥70%，**无需**改脚本或 Makefile；
3. **零新增依赖坐标**：`x/sys` 复用（已直接依赖 + 模块缓存离线核实）；**不**引入 `x/term`、**不**提升 `mattn/go-isatty`（§3.3）；
4. **恢复扫描的归属**：步骤 ①②③④⑥ 在 `storage`（同一短事务）、⑤ 在 `workspace`、`app` 取锁后编排（流程 G）；
5. **门禁靠名字集注入**：`workspace` 不引 `tool` 依赖（兄弟包都指向 `core`），`app` 装配期传送（§2.4 注 4）；
6. **`session list` 是只读命令**：不取锁、不做恢复、不清理锁文件，打开库前校验 `schema_version`（契约 §4.3.1）；
7. **消息写入失败 = fail-closed**：用户消息 / assistant 消息 / 工具结果消息写入失败 → Run `failed`（技术方案 §4.1 伪代码未画该分支，本文补记——与宪法 V 同源纪律）；**审计终态回填失败不在此列**（记 ERROR、保留 `pending`、不覆盖真实结果）；
8. **合成文本单源**：`core/synthesis.go` 是四段补齐文本（**逐字取自契约 §4.4**）与 `Reason*` 常量的**唯一**定义处，循环与恢复共同消费（避免"两处各写一份、措辞漂移"）；
9. **工具名与归属的两级拒绝不写审计行**：`tool_invocations` 记的是"一次工具执行"；执行**之前**的拒绝（名字不在注册池 / 不在本 Agent ToolSet）无入参也无结果可言，尝试本身已由 assistant 消息与回填文本留痕——与"沙箱拒绝在执行**之内**、有行有分类"区分（流程 D ①）；
10. **`runs.trigger` 由 Channel 派生**：`RunRequest` 无该字段且不得加字段（§6 固定口径 8）。

## 待决事项

**无**。本回合八项待决全部落案（见「评审回合裁决落案表」）；新增的技术方案留白（决策点）已按权威顺位逐条裁决并落位：`Build` 错误分支（§2.3 补记）、`iterations` 语义（§2.3 补记 ③）、会话派生编码（§6 固定口径 1）、`estimateVersion` 与余量（§6 固定口径 9）、`trigger` 映射（§6 固定口径 8）。若实施期间出现与本文冲突的事实，按固定裁决句处理：契约字面量服从契约，其余以技术方案为准，并在 §2.4 追加裁决注。

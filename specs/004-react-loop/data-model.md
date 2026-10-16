# Phase 1 数据模型：US-4 ReAct 循环与首个工具闭环

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-16

本文件是设计面实体清单：**载体 / 成员 / 判据 / 状态机 / 不变量**。字段名、字面量与列级口径**不在此重新定义**——唯一事实源是 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md)（§1.1 请求 / 结果、§1.2 返回语义、§1.3 部分结果、§2 状态机、§3 并发、§4.3.1 锁、§4.3.2 六步恢复、§4.4 五分支合成文本、§5 会话标识派生、§7.1~§7.3 审计）、[docs/contracts/schema.sql](../../docs/contracts/schema.sql)（六表 DDL 列级事实）、[docs/TechnicalSolution.md](../../docs/TechnicalSolution.md)（§4.1 循环、§4.2 提示词四段、§4.7 裁剪、§6.1 工具接口、§6.5 授权与执行控制、§11.5 快照）与颗粒度文档 [docs/us/us4-react-loop.md](../../docs/us/us4-react-loop.md) §3（交付物清单）与 §6.2（本文固定口径九条）；本文件只做**汇集与引用**，不复制整表（第二份字面量必漂移）。本 US **零迁移脚本**（六表 US-1 已建齐，§3.4）。

## 实体关系总览

```text
一次运行（AgentService.Run）                              持久化（六表，US-1 已建）
──────────────────────────                               ─────────────────────────
RunRequest ──准入──▶ RunEnv{runs 行=running} ──────────▶ runs（无 final_text 列）
   │    │                    │
   │    │                    ├─ 快照冻结 ──────────────▶ config_snapshots（content 全文 + manifest）
   │    │                    │
   │    │                    └─ ReActLoop.run(ctx, runID, env)
   │    │                          │  每轮：PromptBuilder.Build → ProviderService（两阶段审计）
   │    │                          │                          └────────────────▶ llm_calls（pending→终态）
   │    │                          ├─ 有工具 → ToolExecutor.Execute(ctx, ToolEnv, toolSet, call)
   │    │                          │        │  Policy.Check → 工具体（含受控子进程）
   │    │                          │        └─ 两阶段审计 ─────────────────────▶ tool_invocations（pending→终态）
   │    │                          ├─ Session.AppendMessage（助手消息，含 tool_calls）
   │    │                          └─ Session.AppendToolResult（工具结果消息）
   │    │
   │    └─ 半截组补齐（Session.CompleteToolGroup；判据先落消息、再查审计）
   └─ RunResult{running→completed / limit_reached / cancelled / failed} ────────▶ 终态更新同一 runs 行

崩溃恢复（app 取锁后、开跑前）
────────────────────────────
Recover（storage ①②③④⑥ 同一短事务）+ RecoverWorkspaceResidue（workspace ⑤）
  → pending 审计行归一化为 unknown；按位置判半截组并补齐；仍为 running 的运行标 failed；幂等
```

## 实体 1：运行面契约（`RunRequest` / `RunResult` / `RunStatus` / `Budget`）

- **载体**：内存（`core/run.go`；`RunRequest` 与 `RunResult` 字段**逐字**取自契约 §1.1，**不增不改**）；`runs` 表的行是其持久化投影（**无 `final_text` 列**——最终回答只存在 `RunResult.FinalText` 与会话的 assistant 消息里）。
- **成员**：`RunRequest{AgentName, SessionID, Message, Channel, User, Budget}`（`ConfigHash` 由运行时填充）；`RunResult{RunID, Status, FinalText, Iterations, ToolCalls, ConfigHash, Reason}`；`Budget{MaxIterations, MaxToolCalls, ToolTimeout, MaxOutputBytes, ContextTokens}`；`RunStatus` 五值。
- **状态机**（出处：契约 §2；**终态不回退**）：

| 起始 | 事件 | 终态 | `Reason` 字面量 |
| --- | --- | --- | --- |
| `running` | 无工具调用的模型响应 | `completed` | 空 |
| `running` | 达迭代上限 或 工具预算上限 | `limit_reached` | `超过最大迭代次数` / `工具调用次数超限`（`Reason*` 常量） |
| `running` | 取消 / 截止 | `cancelled` | `运行被取消` |
| `running` | 运行期失败（含上下文准入拒绝、消息写入失败、模型预写失败） | `failed` | `运行内部错误`（或点名字面量，如点名上下文预算键） |

- **判据**：**达上限不是错误**（`limit_reached` 照常带部分结果，契约 §1.3）；函数级 `error` 只用于**调用方问题**（请求非法 / Agent 不存在 / 同会话忙 / 实例数超限——**均不排队**，契约 §1.2 / §3）；`iterations` 语义 = 已**发出**的模型调用轮数（轮首取消不计当前轮）；`toolCalls` = 实际执行过的工具调用数。
- **不变量**：`RunEnv` **不得**承载 Profile / Session 等业务状态之外的东西；`context.WithValue` 零使用（`runID` 显式传参——`ToolEnv(ctx, runID)`）。

## 实体 2：审计两表（`llm_calls` / `tool_invocations`）

- **载体**：SQLite 两表（DDL 由 US-1 建齐）；写入路径**唯一**——模型调用经 `internal/provider` 的 `ProviderService`（`core` 只声明 `LLMCallStore`，收尾 grep `.BeginCall(` 在 `internal/core` **零命中**），工具调用经 `internal/storage.ToolInvocationRepo`。
- **成员（工具侧）**：`ToolInvocation{RunID, SessionID, ToolCallID, ToolName, InputJSON, CreatedAt}` + `ToolInvocationResult{Status, ResultJSON, ErrorMessage, ErrorKind, Duration}`；模型侧沿用 US-2 既有结构。
- **状态机（两阶段写，两者同形）**：`BeginInvocation`（或 `BeginCall`）写 `pending` → 出站动作 → `FinishInvocation`（或终态回填）更新**同一行**。终态取值 = 审计 `status` 四值 `pending` / `success` / `failed` / `unknown`（**无** `cancelled`）。
- **列级口径**（出处：§3.4；**本 US 不得改写既有 DDL**）：

| 表 | 口径 |
| --- | --- |
| `runs` | **无 `final_text` 列**；`FinishRun` 不接收 `finalText` 参数；`trigger` 由 `Channel` 映射（`cli`→`user` / `web`→`api` / `scheduler`→`cron` / `admin`→`manual`）；时间戳 UNIX 毫秒 |
| `tool_invocations` | **无 `round` 列**、**无 `truncated` 列**；截断标记写在既有 `result_json` 表示内（`{"content":…,"retryable":…,"truncated":true}`）；失败原文截断 4 KB；`input_json` 原样 |
| `config_snapshots` | 主键 `id`（自增）+ 唯一键 `(agent_name, hash)`；`content` = AGENT.md 全文；insert-if-absent（冲突即 no-op） |
| `messages` | `run_id` 列是恢复的**判据来源**；`seq` 库内自增；`ProviderExtra` 往返保真（`UseNumber`） |
| `llm_calls` | `round` = 该 Run 内第几轮（**1 起**）；`config_hash` 自本 US 起由运行期填充（前序恒空——改造点 2） |

- **判据（fail-closed，两条链同纪律）**：**预写失败即拒发 / 拒执行**——模型链该错误上抛即 Run `failed`；工具链拒执行 + 回填「审计不可用」+ **Run 继续**。**终态回填失败不在 fail-closed 之列**：记 ERROR（固定键 `run_id` / `store` / `op`）、行留 `pending`、**不覆盖**真实结果。`error_kind` 七值 `sandbox_denied` / `timeout` / `not_found` / `invalid_input` / `external_error` / `runtime_error` / `unknown`（取值情形表见 §3.4；其**不属**于任何合法情形时才用 `unknown`）。
- **不变量**：审计 SQL **只出现在 `internal/storage`**；**执行之前的拒绝不写审计行**（名字不在注册池 / 不在本 Agent `toolSet`——两级拒绝，已决事项 9）；补齐合成结果**只进消息、不进审计**。

## 实体 3：消息模型与会话（`Message` / `ToolCall` / `Session`）

- **载体**：`core.Message` 内存历史 + `messages` 表写透（`Session` = 内存 + 仓储的组合）；无状态会话（**空 `session_id`**）只在内存——**不落会话历史**，但仍落 Run 与审计。
- **成员**：`Message`（角色 / 内容 / `ToolCalls` / `ToolCallID` / `ProviderExtra`）/ `ToolCall`（id / 名字 / 参数）/ `Session`。
- **追加唯一路径**（宪法 II）：正常运行中**模型响应与工具结果由循环统一追加**（`AppendMessage` / `AppendToolResult`）、**用户输入由运行入口写入**、崩溃 / 取消后的半截组由 **`Session.CompleteToolGroup`** 写入——三者都经同一写透接口，**不得**出现第二个出口；`ToolExecutor` **只返回结果、不写消息**。
- **追加接口显式承载 `runID`**：恢复按 `messages.run_id` 定位半截组，而 `Message` 本身没有该字段（契约 DDL 事实）。
- **判据**：模型响应 `content` 可为空（有工具调用时）——**必须容忍**；`usage` 为空记 0 不做推断；`ProviderExtra` 保持原始 JSON 形态（`map[string]any`，**不改类型**），在**入站解析 / 会话落库 / 出站编码**三处逐字不丢。
- **不变量**：**消息写入失败 = fail-closed**（用户消息 / assistant 消息 / 工具结果消息任一处写入失败 → Run `failed`）——**不得**出现「执行了但历史里没有」的调用。
- **会话标识**（出处：§6.2 固定口径 1 + 契约 §5）：由命令面在**准入之前**派生（`sha256` 规范元组编码取 hex 前 16 位，前缀渠道名 `<channel>-<hex16>`；长度前缀消除拼接歧义）；`AgentService` **不派生**——空即无状态（契约语义保留）。

## 实体 4：运行级配置快照（`ConfigSnapshot` / `ManifestEntry`）

- **载体**：`config_snapshots` 表（`workspace.ContextLoader.Snapshot` 构造、`storage.SnapshotStore` 落库）。
- **成员**：`ConfigSnapshot{AgentName, Hash, Content, Manifest, SectionOne, Profile}` + `ManifestEntry{Path, SHA256}`。
- **判据**（出处：技术方案 §11.5 + 裁决 3 + §3.4）：
  - **同一次读取**解析 `Profile` 与段一正文并保留 AGENT.md 原文——**不得**与旧 `entry.Profile` 混用；
  - `Hash` = `manifest_json` **规整序列化**的 sha256，**当前时间不得混入**（同内容不同时刻同哈希）；
  - `content` = **AGENT.md 全文**（不能只存哈希）；manifest 覆盖四类文件，L2/L3 正文与 `scripts` **不进清单**；
  - **Run 开跑之前**保存；**失败即拒绝启动**（零模型调用）；
  - 落库 insert-if-absent（`UNIQUE(agent_name, hash)` 冲突即 no-op）。
- **状态机**：无运行态——构造即不可变；生命周期 = 每次运行一份（**不缓存、不复用**）。
- **不变量**：`SectionOne(root, entry, now)` 既有签名与行为**不变**（`Snapshot` 委托给它）；日志只记 `agent` / `hash` / `files`（清单条数），**不得**回显文件全文。

## 实体 5：提示词（`Prompt` / `PromptBuilder`）

- **载体**：内存（`core/prompt.go`）；**每轮重新组装**（上一轮工具结果必须进本轮历史）。
- **成员（四段）**（出处：技术方案 §4.2）：

| 段 | 内容 | 本 US 口径 |
| --- | --- | --- |
| 1 | 系统提示词 | = 运行级快照冻结的段一正文（**不重读**盘） |
| 2 | 记忆注入 | **恒空占位**（归 US-6） |
| 3 | 对话历史 | 经两级裁剪后的消息序列 |
| 4 | 工具列表 | 工具池为空则**请求体不带 `tools` 键**（旧行为逐字不变） |

- **两级裁剪与准入**（出处：技术方案 §4.7 + 契约 §6）：先按**轮数**保留最近若干轮（**不得**按消息条数）→ 仍超预算则按**完整工具交互组**从最老处继续丢弃（**不得**拆散任何一组）；不可裁部分（系统提示词 / 工具定义 / 当前消息）估算超限 → **拒绝启动**（运行 `failed`、点名上下文预算键、**零模型调用**）。
- **估算器**：版本常量 `estimateVersion = "v1"`，覆盖**完整出站字段**（消息内容 / 工具调用参数 / 工具 schema / 扩展字段）；准入另保留 `reserve_tokens`（缺省 1024）余量吸收协议包装开销；**不宣称**「不会低估」或「保守上界」（裁决 6）——用五类夹具标定 + 人工项 8 与真分词器对拍。
- **不变量**：估算**确定性**（同输入同读数）；版本与余量一并写进 `evals` 结果文件与快照 INFO 日志。

## 实体 6：工具与执行环境（`Tool` / `Result` / `ToolEnv`）

- **载体**：`core/tool.go` 四方法接口（技术方案 §6.1 逐字）+ `core.RunEnv.ToolEnv(ctx, runID)` 构造的 `ToolEnv`；实现三件在 `internal/tool/filetools.go`。
- **成员**：`Tool{Name, Description, InputSchema, Execute}`；`Result{Content, Retryable}`；`ToolEnv`（`Timeout` = `min(30s 缺省, 剩余 Run 预算)`，读 ctx 截止时间）；工具三件 `read_file` / `write_file` / `list_dir`（参数与失败类别逐字对齐契约；`InputSchema` 是**内联 JSON Schema 常量**、由测试钉住）。
- **判据**：
  - **串行**执行（一轮内多工具依次，无并发编排）；
  - 单工具超时 = `min(30s, 剩余 Run 预算, 更小的工具请求值)` 且**实际建立**超时 context；**超时 → Run 继续**；
  - 输出上限 256 KB，**读取中**限流（内存不随输出膨胀）且尊重更小的配置上限；截断标记 `\n[已截断：输出超过 <N> 字节上限，此处不是完整输出]`（N = **生效**上限）——**不承诺**原始长度（§6.2 第 7 条）；
  - 失败信息**必须回填**给模型（模型看不到结果就不会收敛）；
  - `write_file` 新文件的父目录**必须已存在**（不存在即拒），写入经**相对根句柄**（防「校验与写入之间被换成越界软连接」的竞争窗口）；
  - `list_dir` 不递归、不跟随越界软连接。
- **不变量**：`ToolExecutor.Execute(ctx, env ToolEnv, toolSet []string, call ToolCall) Result` —— **返回结果、不写消息**（D2 ④）；工具失败**不判运行失败**。

## 实体 7：授权判定与受控执行（`Action` / `Command` / `WhitelistPolicy` / `LocalRunner`）

- **载体**：`core/action.go`（`Action{Type, Target, Agent}` / `ActionType` 四值 `FILE_READ` / `FILE_WRITE` / `SHELL_COMMAND` / `HTTP_REQUEST` / `Command` / `CommandResult`）+ `internal/tool` 的两个实现（技术方案 §6.5 逐字）。
- **判据（授权面）**：只看「谁、要碰什么」，**不看打算怎么做**（`Action` **不含 argv**）；读白名单含技能库与工作区；**跨 Agent 读取必须显式授权**（未授权即拒且**点名该配置键** `sandbox.cross_agent_read`）；**保护规则**（配置文件 / 技能库 / 记忆目录 / 脚本目录的写）**先于**一般放行判定；命令行匹配**整串逐字**（多词串拒）；**不得**给 `bash` / `sh` 放行（含 `-c` 形态）；网络目标域名大小写不敏感、纯 IP 字面量拒、只允许 http/https、**跳转不得放宽到超过 5 跳**（代码常量）；**软连接不可解析 → 失败关闭**。
- **判据（执行面）**：环境变量**白名单**（不继承代理与凭证类变量）；超时 / 取消 → **杀整棵进程树**（Windows Job Object`KILL_ON_JOB_CLOSE` + 挂起—关联顺序；Unix 进程组）；退出码与 stderr 捕获；**解释器形态校验**（`-c` 拒、脚本真实路径越界拒）属**执行侧**。
- **拒绝文案**：**四段式**（违规说明 / 具体目标 / 规则出处 / 放行指引）——逐字来源 = 技术方案 §6.5，测试 `tool/policy_test.go` F18 与 `core/tool_exec_test.go` B1 / B2 **逐字比对**；审计侧落 `failed` + `sandbox_denied`。
- **不变量**：两接口（`Policy.Check` / `CommandRunner.Run`）由 `core` **声明**、`tool` **实现**；调用方**不得**依赖具体实现（文件工具只引用 `core.Policy`——编译期判据）；**调用点在工具内部**（文件工具只需 `Policy`，命令执行才需 `Runner`——裁决 1）。

## 实体 8：半截工具组与合成结果（`CompleteToolGroup` / `SynthesizeMissingResult`）

- **载体**：`core/session.go`（补齐动作）+ `core/synthesis.go`（**唯一**文本与 `Reason*` 常量定义处——两侧共同消费）。
- **成员**：四段文本常量（逐字取自契约 §4.4）：

| 常量 | 文本 |
| --- | --- |
| `TextResultUnknown` | `结果未知（进程中断，无法确定是否已产生外部副作用）` |
| `TextToolCancelled` | `已取消（运行被取消，该调用未执行）` |
| `TextToolBudget` | `未执行（工具调用次数超限）` |
| `TextToolInterrupted` | `未执行（进程中断）` |

- **判据（顺序不得颠倒）**：先判「哪些工具调用**没有对应结果消息**」（`MissingToolResults(calls, msgs)` 导出、取消路径与恢复路径**共用同一实现**），**再**按审计行分类：

| 分支 | 条件 | 合成内容 | 留痕 |
| --- | --- | --- | --- |
| 有终态审计行 | `status` ∈ `success` / `failed` | 由 `ResultJSON` **重建真实结果** | 无新增 |
| 执行过但终态回填失败 | 行仍 `pending` / `unknown` | `TextResultUnknown` | **WARN** |
| 无行 + 取消 | `reason` = 取消 | `TextToolCancelled` | 无审计行 |
| 无行 + 预算 | `reason` = 工具预算 | `TextToolBudget` | 无审计行 |
| 无行 + 崩溃 | `reason` = 中断 | `TextToolInterrupted` | 无审计行 |

- **不变量**：补齐**只进消息、不进审计**（审计只记真实发生的事）；**幂等**（重复调用不重复插入；组已完整即 no-op）；收尾用**独立有限时长** context（`finishTimeout = 5 * time.Second`——父上下文已取消仍必须写得进去）；补齐**失败返回 error**（调用方记 ERROR，Run 照常收场）；「执行过但回填失败」**不得**写成「未执行」（前者可能有外部副作用）。

## 实体 9：崩溃恢复（`Recover` / `RecoveryReport` / `ResidueReport`）

- **载体**：`internal/storage/recovery.go`（六步 ①②③④⑥，同一短事务）+ `internal/workspace/recovery.go`（⑤ 残留）+ `internal/app/wire.go` 编排（取锁后、开跑前）。
- **成员**：`Recover(ctx, db, logger, now) (RecoveryReport, error)`；`RecoveryReport{Unknowns, RepairedGroups, ClosedRuns}`；`RecoverWorkspaceResidue(root, logger) (ResidueReport, error)`。
- **判据**：① `pending` 审计行 → `unknown`；② 按**位置**（**不按 id**）判半截组并按实体 8 五分支补齐；③ 组修复与运行收尾在**同一短事务**内；④ 只把**仍为** `running` 的运行标 `failed`（已结束状态**不得**被覆盖）；⑤ `.backup` / `.staging` 残留判定（回滚 / 前滚）；⑥ **幂等**（连跑两次零变化）。
- **不变量**：**不得**跨 LLM 调用与工具执行开事务（`storage/doc.go` 事务纪律段）；无状态运行（空 `session_id`）**也收尾**（组看消息、Run 看表）；留痕固定键 INFO `unknowns` / `repaired_groups` / `closed_runs`。

## 实体 10：命令面（`chat` / `session list`）

- **载体**：`internal/cli/chat.go` / `internal/cli/session.go` + `internal/channel`（真控制台 vs 管道）。
- **终止语义**（出处：§6.2 固定口径 3）：stdin EOF → 正常退出（退出码 0）；`--message` → 单条即退；Ctrl-C → 该次 Run `cancelled`（打印取消提示）→ **进程退出码 0**（**取消是预期动作而非错误**）；每次 Run 建 **60 秒** deadline（调用方已有更早 deadline 取更早者；Ctrl-C **不能**替代资源时限）。
- **渲染口径**（固定，§6.2 第 4 / 5 条）：单轮回复直接打正文；工具调用期间一行 `⚙ 调用工具 <name>…`（**不打印入参与结果全文**——脱敏纪律）；失败 `✗ <四段式 / 错误文本>`；`limit_reached` / `cancelled` 打醒目提示 + **已得部分结果**；`session list` 一行一会话 `<id>：<消息数> 条（最后活动 <2006-01-02 15:04:05>）`、末尾 `共 N 个会话`、空库 `未发现会话`。
- **判据**：`chat` / `session list` 走**重路径** vs **轻路径**的划分——`session list` **只读**：不取实例锁、不做恢复、不清理锁文件，但**打开库前校验 `schema_version`**（不匹配即拒绝）。
- **不变量**：`session_id` 由本层在**准入之前**算好；`IsTerminal` 经控制台 API 判定（**不猜环境变量**）；**`cli` 不得 import `workspace`**。

## 实体 11：评测用例与结果文件（`evals/`）

- **载体**：`evals/cases/*.yaml` 与 `evals/injection/*.yaml`（用例集）；`evals/results/<时间戳>.json`（结果文件）；`evals/main.go`（`package main` 运行器）。
- **成员（用例）**：输入（`message`）/ 夹具（临时工作区的 AGENT.md 与文件）/ 预算（`max_iterations` / `max_tool_calls` / `context_tokens`）/ **可执行断言**（期望工具序列、期望文本包含、期望终态）；注入类用例断言「四段式拒绝 + 审计 `failed` + `sandbox_denied`」。
- **成员（结果）**：每例 = 用例 id / **RunID** / 状态 / 工具序列 / **资源用量**（token 用量 + 调用次数）/ **计量完整性**标记；另含 `estimateVersion` 与 `reserve_tokens`。
- **判据**：**复用**唯一运行入口与既有审计链（**不新建**入口、**不新增评测表**）；默认进程内 fake `ProviderClient`（**可重复**，进日常回归）；真模型**显式门控**（命令行指定 + 环境变量凭证）。
- **不变量**：资源用量**不换算金额**；usage 缺失 / 悬空 / 未知时标「**计量不完整**」——**不得**把库中默认零解释为零成本；主观项（如「回答是否专业」）留人工签核，可执行断言部分自动判卷。

## 全局不变量

1. **唯一运行入口**：`AgentService.Run` 是唯一入口；`ReActLoop.run` **不导出**（外部不可直调）；`error` 只用于调用方问题，运行期失败一律体现在 `RunResult.Status`。
2. **消息追加唯一路径**：循环 / 运行入口 / 补齐路径三条写入方，**同一个**写透接口；`ToolExecutor` 不写消息。
3. **审计两条链 fail-closed**：预写失败即拒发 / 拒执行；**回填失败保留 `pending` 不覆盖**；补齐**只进消息不进审计**；审计 SQL 只在 `internal/storage`。
4. **`core` 只依赖标准库**（自本 US 起可执行语句，判据 = `go list -f '{{.Imports}}' ./internal/core` 只出标准库）；`cli` 不 import `workspace`；`workspace` 不 import `tool`。
5. **time 一律显式传入**（`now time.Time` / 时钟注入）；禁用词 `time.Local` / `context.WithValue` / `math/rand`（收尾 grep 零命中）与 `use_skill`（**永久禁**，宪法 IV）。
6. **凭证零明文**：出站复用 US-2 的 `${ENV_VAR}` 解析面；**凭证不进日志 / 审计 / 错误文案**；日志与错误文案不回显文件全文与配置原文。
7. **可观测性双轨**：失败路径**审计落库 + WARN 日志两者都要**（固定键 `run_id` / `session_id` / `tool` / `reason` / `detail`）；审计写失败记 ERROR（`run_id` / `store` / `op`）；恢复记 INFO（`unknowns` / `repaired_groups` / `closed_runs`）；快照记 INFO/Debug（`agent` / `hash` / `files`）。
8. **预算与上限**：迭代 10（Profile 可覆盖）/ 工具调用 50 / 单工具超时 30s / 输出 256 KB / 上下文 32768（+ `reserve_tokens` 1024）/ CLI Run 60s——**达上限不是错误**。
9. **平台差异经构建约束分文件**（`console_{windows,linux,darwin}.go` / `runner_{windows,unix}.go`）；`CGO_ENABLED=0` 与 `-race` 分属不同命令。

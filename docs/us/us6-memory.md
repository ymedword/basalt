# US-6 Memory — 模块执行颗粒度文档

> **文档状态**：草案 **R4**（2026-10-17；由 `/module-doc-gen 6` 产出，已由 `/us-register 6` 登记，台账行见 [REGISTRY.md](REGISTRY.md)）。**R4 = analyze C1 处置**（实施前阻断检查，用户裁决）：FR-038 / 口径 26 要求「记忆包生产导入面」判据**必须**由**包级架构测试**承载——落定为**新增** `internal/memory/import_test.go`（1 条；测试面 40 → **41 条**）；逐条见文末「评审回合裁决落案表（续：R3 → analyze C1 处置）」。**R3 = module-dev 步骤 5 停点修订**（用户裁决）：**P1-a** 任务三阶段口径 + **P1-b** 条目正文表示统一（写入入口 `TrimSpace` 一次；补首尾空白边界用例）+ **P2-b** per-scope 锁按完整 `Scope` 分档，另顺手修正两处（Path Conventions 收窄 / T052 用 `git status --porcelain`）——逐条见文末「评审回合裁决落案表（续：R2 → R3）」。**R2 = 评审回合修订**：**P1** 三条（读取面错误处理分档 / R4b 预算算术 / R3·R4a·R5·R7 逆改点守不住）+ **P2** 三条（条目边界与配额口径 / 检索语义的上游澄清 / 样例接口与夹具接线）全部落案，另补小修七处——逐条见文末「评审回合裁决落案表」。原「待决事项」**4 条已清零**（选项与依据保留在原处作取舍记录）。
>
> **事实主源顺位（供 `/us-register 6` 派生登记）**
>
> 1. **`docs/TechnicalSolution.md`**（文档之间最权威）——§5.1 长期记忆（**两层作用域** `Scope` / 全局回退 / **两层不叠加**；**`LongTermMemoryStore` 三方法逐字**；**行为契约 6 条**：不缓存（每次运行重建快照、运行内冻结，见 §11.5）· 核心区不截断但有界（单条 ≤ 4 KB、每 Agent ≤ 32 KB，超限可读拒绝）· 写入分区由 Agent 显式指定（参数名 **`scope`**，取值 `CORE` / `ARCHIVAL`，落 `memory_entries.section`）· 召回是**关键词检索**（整串匹配，不得分词 / 扩展 / 向量化）· **载体隔离**（`memory/` 不在文件工具白名单）· **同作用域写入串行化**（进程内 per-scope 锁，「读-检查-写」不得交错；sqlite 档配额检查与追加**同一事务**、markdown 档**临时文件 + rename** 原子替换））/ §5.2 分层回退（无 Agent 上下文 = **全局层**，正常取值非异常；Agent 层无内容 → 回退全局层；存量全局载体**不得**迁移删除）/ §5.3 载体分档（本 US 交付 **markdown 与 sqlite** 两档；mem0 按 U-8 裁决）/ §5.4 两个记忆工具（`save_memory` / `recall_memory` 门面与工具分工；`USER.md` 与 `MEMORY.md` 的分工注）/ §4.2 提示词四段（**[2] 记忆注入**（长期记忆：Agent 层有则注、无则回退全局层）/ [1] 系统提示词 / [3] 对话历史 / [4] 工具列表）/ §4.7 上下文预算（**不可裁剪内容 = 系统提示词 + 核心记忆 + 当前消息**——超限拒绝启动、点名声明的预算键、零 LLM 调用；手工编辑超配额**不按写拒绝**：注入时 WARN + **完整**注入，若因此超预算走拒绝启动）/ §11.5 快照（`ConfigSnapshot` manifest 逐字**不含 memory**；每次运行重建配置与**内存快照**）/ §3 配置面（`memory.backend`）/ §10.1 包结构与依赖方向 / §13 实施节奏（US-6 行：任务 = Memory；可演示成果 = **Demo 二：跨对话记偏好**；涉及包 = `memory` / `storage` / `core` / `workspace`）
> 2. **`docs/DemandAnalysis.md`**——§1.2 五大核心能力（能力三：记忆系统）/ §5.5 长期记忆（两层作用域、回退、载体档、配额与召回语义）/ §5.6 能力四：Tool 体系（记忆两工具在工具表中的位置）/ §9.3 记忆写入与召回链（`save_memory → 记忆门面 → 按 memory.backend 选载体 → 写入`；`recall_memory（单个关键词）→ 记忆门面 → 按载体检索归档区 → 命中返回`；`提示词组装：Agent 层有内容 → 注 Agent 层；否则 → 注全局层；两层不叠加`）/ §5.1 工作区目录树（`memory/` 的位置与 `MEMORY.md`）/ §10.2 模板默认工具（`tools: [save_memory, recall_memory]` 逐字）/ §13.1 功能验收 / §13.4 质量门禁验收
> 3. **`.specify/memory/constitution.md`**（原则级最高）——II（工具调度唯一归属 `ToolExecutor`；会话历史由**一条消息仓储**承载——工具只返回结果、**不写消息**）/ IV（一个目录 = 一个 Agent；技能声明式绑定——记忆工具属**内置工具池**、与技能绑定无关）/ V（审计 day-one 且 **fail-closed**——记忆工具**不落审计**，审计面本 US 零改动）/ VI（授权与执行分离两接口——本 US 的工具**不去触碰**沙箱面，见 §2.4 注 3）/ VII（同步执行；context 只承载取消 / 截止 / 追踪，Profile / Session 等业务状态**必须**显式传递——记忆文本经 `RunEnv` 显式注入，**不得**走 `context.WithValue`）/ VIII（单二进制纪律；依赖最小化——本 US **零新增坐标**）/ IX（不用 ORM，手写 SQL；已发布迁移只追加——本 US **零迁移**）/ X（范围卡紧）
> 4. **`docs/contracts/`**（**固定字面量**）——`agent-md.md` **§5.4**（`save_memory`：`content` 是 / 必填，`scope` 否 / 省略 = `ARCHIVAL`；有 Agent 上下文 → `memory/agents/<name>/MEMORY.md`，无 → 全局 `memory/MEMORY.md`；返回写入确认含作用域；失败 `invalid_input` / `runtime_error`；超配额返回**可读拒绝提示改写归档区**。`recall_memory`：`query` 是 / 必填、**单个关键词**；**返回命中的归档区条目（核心区不参与检索——它每轮已完整注入）**；**无命中返回成功 + 空结果**；按整串 contains——**查询词以连续子串出现才命中**，分散词不自动组合（2026-10-17 上游澄清：原文「多词短语必不命中」已同步改写，见 §2.4 注 6）。**§5.4 注「两个 scope」**：工具 `scope` 参数 = 写入分区 `CORE` / `ARCHIVAL` → `memory_entries.section` 列；表 `memory_entries.scope` 列 = **分层** `agent` / `global`。**§4.2**：裁剪顺序从最早的组开始整组丢弃，直到满足 `max_history_turns` 与 token 预算；**「核心记忆区永不裁剪」** 逐字）/ `run-and-errors.md` §6 预算表（`ToolTimeout` 30 秒 / `MaxOutputBytes` 256 KB / 上下文 32768；**模型只能缩短**；不可裁剪内容超限 → 拒绝启动）/ §7.3 `error_kind` **七值**（`invalid_input` = 「入参非法」、`runtime_error` = 「运行时错误」、`not_found` / `external_error` 等）/ §7.2 两阶段写 / `schema.sql` **§`memory_entries`**（六列 + `idx_memory_scope`，US-1 建齐 → 本 US **零迁移**）
> 5. **既有实现事实源（US-1~US-5 已交付面；复用不重造）**——`internal/core/prompt.go`：`PromptBuilder.Build(env *RunEnv) (Prompt, error)` 组 `Prompt{System, History, Tools}`，`System` 注释**逐字**写着「段 1 系统提示词（快照冻结的段一正文）+ **段 2 记忆注入（本 US 恒空——装配位保留、不产生文本）**；故两者合成一个字段，注入接上时无需改结构」，`estimateTokens(p.System)` 整体估算，唯一 error 点名 `budget.context_tokens`；`internal/core/run.go`：`RunEnv` 五导出（`Req` / `Snapshot` / `Session` / `Budget` / `ToolSet`）+ **两个非导出注入位**（`llmClient ProviderClient` / `agentDir string`——注释逐字「冻结签名下的注入位……不是契约字段，也不进任何出站形态」），`llmEnv` 里 `Message{Role: roleSystem, Content: p.System}` 是唯一段 1+2 落点，`ToolEnv{RunID, SessionID, AgentName, AgentDir, Timeout, MaxOutputBytes}`；`internal/core/service.go`：`Run` 步骤 ②③ 的失败形状（`RunResult{RunID, Status: StatusFailed, Reason: err.Error()}, nil`——**零 LLM 调用**）、步骤 ④ 组装 `RunEnv{Budget: mergeBudget(...), ToolSet: snapshotTools(...), llmClient: ..., agentDir: ...}`、`ServiceOptions` 字段表；`internal/core/tool.go`：`Tool` 四方法 + `Result{Content, Retryable}`（冻结）；`internal/core/errors.go`：`InvalidInputError{Tool, Reason}`（`Error()` 返回 `Reason` 原文，模型可见）/ `ExternalError` / `IsInvalidInput` / `IsExternalError` / `IsSandboxViolation`；`internal/core/tool_exec.go`：`classifyToolError` **判序**（沙箱 → 入参非法 → 取消 / 截止 → 外部系统 → 文件不存在 → 兜底；判据走 `errors.Is` / `errors.As`，**不得**比对错误文本）、`checkToolInput`（合法 JSON 对象 + 必填键）、`truncateToolOutput`；`internal/tool/filetools.go`：`decodeFileToolInput` + `fileToolInput.check` 的「解码失败与取值失败同归 `*core.InvalidInputError`」范式、内联 JSON Schema 常量、`os.OpenRoot` 根句柄读写；`internal/tool/policy.go`：memory 目录规则（`policy.go:202` `case withinDir(real, filepath.Join(p.root, memoryDirName))`）与指引常量 `guidanceMemoryNotWritable`（`policy.go:49` 逐字「记忆的写入归记忆工具，文件工具不改写 memory/（代码常量），没有配置键可以加。」）；`internal/tool/registry.go`：`Register` / `Names` / `Specs`；`internal/workspace/templates.go`：`fileTemplates` 含 `memory/MEMORY.md`（`templateMemoryMD` = 标题 `# MEMORY.md —— 长期记忆 · 全局层` + 两分区标题 `## 核心记忆` / `## 归档记忆`）、`directories` 含 `memory`（**`memory/agents` 有意不预建**，注释逐字）、`agentTemplate` 的 `Tools: []string{}`（注释 `// tools: []（记忆工具到 US-6 才在池）`）、`templateAgentsMD` 已写明「长期记忆分层：全局层 memory/MEMORY.md，Agent 层 `memory/agents/<name>/MEMORY.md`（按需创建）」；`internal/storage/db.go`：`withWriteLock(ctx, db, fn)`（钉一条连接 + `BEGIN IMMEDIATE`）——sqlite 档「配额检查 + 追加同事务」的现成原语；`internal/storage/{run,session,snapshot}_store.go`：`NewXxxStore(db *DB)` 构造范式与 `pendingInvocation` 的落库范式；`internal/app/wire.go`：重路径装配 ①②③④⑤⑥⑦（⑤ 注册池**六件**、注释逐字「`notify` / `manage_schedule` 与 US-6 / US-7 / US-10 各件仍为占位」）、`ServiceOptions` 构造点、`outputBudget(cfg)`；`internal/app/wire_test.go`：`scriptedProviderClient`（含 `requests()`）+ `newWireFixture` / `fx.assemble` + 包外 `OpenRuntime` 装配面闭环例；`internal/cli/config.go`：`Config.Memory map[string]any`（注释「键集尚未落位的段（memory / author）以原始结构保留」）+ `validateConfig`（取值域与必填校验）；`basalt.yaml.example:53-54`：`memory:` 与 `backend: markdown` 两行（**键已存在**，本 US 零改动样例）；`internal/cli/ports_test.go`：依赖方向判卷（禁 import 清单 = storage / workspace / provider / tool）——本 US 之前**唯一**的依赖方向机器判据；本 US 落第二处同型判据（`internal/memory/import_test.go`，口径 26）
> 6. **附加事实源**——`docs/Compatibility.md` §2.2（记忆分层 / 长期记忆门面工具行）/ §3（快照语义为**有意改变**——记忆文本不进快照）/ §4 #2（mem0 载体**延后**到扩展阶段）/ §2.4（驱动映射：#41 **长期记忆**记忆/召回——**只追加、删除留缺口**）/ §5 共享验收样例；`docs/Operations.md` §2.2（「改完需重启」清单——载体切换 = 重启生效）/ §3（备份面含工作区 `memory/` 与库文件两处）；`docs/AiProgrammingGuide.md` §4.6（US-6 完整定义：核心目标「长期记忆两层作用域（Agent 层 / 全局回退层，不叠加）+ 三档载体接口（本 US 交付 markdown 与 sqlite；mem0 按 U-8 裁决）+ save / recall 两个记忆工具」/ 涉及包 = **`memory` / `storage`（memory_entries）/ `core`（记忆接口）/ `workspace`（memory 路径约定）** / task 大类 / 颗粒度「约 12~15 个文件」/ 硬依赖 US-4）/ §7 陷阱表（与本 US 直接相关：#20 模板默认工具需随本 US 更新、#41 记忆/召回只追加、#55 未验证清单）；`docs/EngineeringStandards.md` §1 Go / §2 测试（判据面单一权威枚举）/ §3 数据库（手写 SQL、迁移只追加）/ §4 日志与错误
> 7. **落地参考（非权威）**——`docs/Roadmap.md` §4（US 开放序列）
>
> **固定裁决句**：本文与最新技术方案冲突时，**以技术方案为准**；本仓配套工件（spec / plan / 已批准 US 清单）已锁定的引用在不与前者冲突时沿用，不另行改判。**契约字面量一律服从契约**——`LongTermMemoryStore` 三方法签名、两工具的参数表与失败类别、`error_kind` 七值、`memory_entries` 六列，本文明文冻结的部分只在契约留白处生效。**spec 锚点现状（核对于 2026-10-17）**：`specs/` 下现有 `001-foundation/` / `002-provider/` / `003-agent-profile/` / `004-react-loop/` / `005-shell-http-tools/`（US-1~US-5 已完成验收的历史设计，**只读**）；US-6 的 spec 目录由 `/module-dev 6` 新建。
>
> **登记派生信息**：技术方案章节 = §5.1 / §5.2 / §5.3 / §5.4 / §4.2 / §4.7 / §11.5 / §3 / §10.1 / §13；需求文档章节 = §1.2 / §5.5 / §5.6 / §9.3 / §5.1 / §10.2 / §13.1；模块落位（摘要）= `internal/memory`（门面 + markdown 载体 + per-scope 锁——技术方案 §13 US-6「涉及包」列）/ `internal/storage`（sqlite 载体 `memory_store.go`——同列「storage（memory_entries）」，零迁移）/ `internal/core`（记忆接口与端口 + 段 2 注入——同列「core（记忆接口）」）/ `internal/workspace`（memory 路径约定 + 模板默认工具——同列「workspace（memory 路径约定）」）+ `internal/tool`（两件记忆工具，§3.1）+ `internal/app`（装配接线，§3.5）+ `internal/cli`（memory 段类型化，§3.5）；`go.mod` **零新增坐标**（§3.3）。
>
> **台账状态**：**已登记**（2026-10-17，`/us-register 6` 写入 `docs/us/REGISTRY.md` 登记表末行）；本段「登记派生信息」即该行「技术方案章节 / 需求文档章节 / 模块落位」的派生源。

## 1. 模块概述

**定位**：给底座装上**长期记忆**——两层作用域（**Agent 层** / **全局回退层**，两层**不叠加**）+ 三档载体接口（本 US 交付 **markdown 与 sqlite** 两档，换档只改一行配置）+ `save_memory` / `recall_memory` 两件记忆工具；记忆在**每次运行开始时**读成文本注入系统提示词的**段 2**，运行内冻结。

**价值（WHAT / WHY）**：US-6 交付三件可判卷的事：① **跨对话记偏好**——编程指南 §4.6 的验收 Demo（**Demo 二**）：会话 A 里告诉 Agent「以后回答简洁点」，进程重启，会话 B 仍按该偏好输出；② **两层作用域与回退**——同一份 Agent 资产可被多个 Agent 复用（Agent 层记住该 Agent 的专有事实），全局层承载跨 Agent 共享的偏好；没有 Agent 上下文时（如运维直连会话）落全局层且**不报错**——「无 Agent 上下文 = 全局层」是**正常取值**，不是异常；③ **载体可换**——markdown 档人可读可手改、sqlite 档可并发可查询，两档**同一套语义断言**（`memory/` 目录对文件工具**不可写**，记忆只能经记忆工具进出——载体隔离是审计可信度的前提）。规模参考：约 12~15 个文件（编程指南 §4.6）；本 US 实际交付面见 §3（代码 新增 6 文件 + 改动 7 文件；测试 新增 6 文件 41 条 + 改动 7 文件 17 条 + 判据扩 1 处——含 R4 处置新增的包级架构测试 1 条）。**硬依赖 US-4（已交付）**——`ToolExecutor` / `Tool` 接口 / `PromptBuilder` / `RunEnv` 注入位范式；本 US 不消费其他未交付能力（MCP 归 US-7、`notify` 归 US-8、`manage_schedule` 归 US-10、管理台与记忆查询端点归 US-9 / US-12）。

## 2. 设计要点与约束

### 2.1 职责边界表

| 维度 | 本 US 职责（做） | 非本 US 职责（不做，归属） |
| --- | --- | --- |
| 记忆接口与端口 | `internal/core/memory.go` 新增：`Scope` / `GlobalScope` / `Section`（`SectionCore` / `SectionArchive`）/ `LongTermMemoryStore`（三方法**逐字**）/ `MemoryReader` 端口（提示词侧唯一消费面）/ 配额常量 / `MemoryQuotaError` | 载体实现（`memory` / `storage` 两包）；管理台读取端点（US-12）；`GET /api/v1/memory`（US-9） |
| 记忆门面 | `internal/memory`：`Service`（两档共用的**门面**）——作用域解析、生效层回退、核心区解析、**per-scope 锁**、注入期超配额 WARN；`Options{Backend, Logger}` | 载体本体（下游两处）；技能 / 会话状态（US-2 / US-4 既有面） |
| 载体（markdown） | `internal/memory/markdown.go`：`MarkdownStore`——文件读写（`os.OpenRoot` 根句柄）、两分区解析与拼接、**临时文件 + rename** 原子替换、归档区整串检索、配额求和 | `basalt init` 的模板与目录创建（US-1 已交付，本 US **不**预建 `memory/agents/`） |
| 载体（sqlite） | `internal/storage/memory_store.go`：`MemoryStore`——六列读写、`octet_length` 配额求和、**`withWriteLock` 单事务**「配额检查 + INSERT」、归档区 `instr` 检索、规范文本渲染 | 表结构与迁移（US-1 已建齐，**零迁移**）；跨表事务编排（US-4 既有面） |
| 段 2 注入 | `core.ServiceOptions.Memory`（可空端口）+ `AgentService.Run` 步骤 ④ **每运行读一次**（运行内冻结）+ `RunEnv` 第三个非导出注入位 + `PromptBuilder.Build` 拼段 2 | 段 1 / 段 3 / 段 4 的组装（US-4 既有）；快照结构（`ConfigSnapshot` **不得**加记忆字段） |
| 记忆工具 | `internal/tool/memorytools.go`：`save_memory` / `recall_memory` 两件（schema、解码与取值校验、作用域与分区解析、配额拒绝映射、确认 / 召回文案） | 工具调度与失败分类的执行器侧（US-4 既有）；审计（本 US **零改动**）；工具清单命令（US-5 已交付，池从 6 → 8 后自动多两行） |
| 装配与配置 | `wire.go` 注册池 6 → 8、记忆门面构建与两条接线（工具 + `ServiceOptions.Memory`）；`cli` 的 `memory` 段类型化与取值域校验；`workspace` 导出 `MemoryFilePath`；模板默认工具 | 热加载装配（US-11）；`serve` / `gateway` 的 REST 装配（US-9） |
| 可观测性 | 门面在**注入期超配额**（手工编辑所致）时记 **WARN**（可观测性双轨：此路径无审计行，日志是唯一留痕） | 审计链路（两阶段写全在 `ToolExecutor`，本 US 零改动）；`runs` / `llm_calls` / `tool_invocations` 面的任何改动 |

### 2.2 关键约束清单

1. **三方法签名逐字**（技术方案 §5.1）：`Append(ctx, scope, section, content) error` / `Load(ctx, scope) (string, error)` / `RecallByKeyword(ctx, scope, query) ([]string, error)`——**不得**增删方法名或参数、**不得**在门面与载体之间另立第二套读写接口（门面 `Service` 与两档载体都实现它，`var _ core.LongTermMemoryStore = (*X)(nil)` 一对一断言）。症状：加一个 `LoadCore(scope)` 让三方法接口名存实亡，换档适配面随之分叉。
2. **记忆接口与端口落 `internal/core`**（编程指南 §4.6「涉及包 = core（记忆接口）」/ 技术方案 §10.1 依赖方向）：`core` **只依赖标准库**——`Scope` / `Section` / 接口 / 端口 / 配额常量 / `MemoryQuotaError` 全是纯类型与纯逻辑，**不得**在 `core` 里出现文件或 SQL。
3. **`Scope` 与全局层是一等值**（技术方案 §5.2）：`type Scope struct { Agent string }` + `var GlobalScope = Scope{}`——**不得**用 `nil`、空串另行顶替，**不得**把「无 Agent 上下文」当异常（`env.AgentName == ""` 是正常取值 → 全局层）。
4. **两层作用域：回退但不叠加**（技术方案 §5.2 / 需求 §9.3 逐字）：生效层解析 = ① 有 Agent 上下文且该层**有内容** → Agent 层；② 否则 → 全局层。**不得**把两层内容拼接注入（Agent 层命中时全局层**一个字都不进**系统提示词）。症状：Agent 层与全局层同时注入，两份偏好冲突时模型无所适从，且总量不受任一层配额约束。
5. **运行级快照**（技术方案 §5.1 契约 1 / §11.5）：记忆文本**每次运行开始读一次**、运行内冻结；`PromptBuilder` 每轮重组提示词但**不得**重读载体；`ConfigSnapshot` **不得**新增记忆字段（manifest = AGENT.md + bootstrap 三件 + 绑定技能 `SKILL.md`，逐字不含 memory）。症状：中途改了 `MEMORY.md`，同一运行的第 2 轮读到新内容——同一运行内两套段 2。
6. **核心区永不截断**（契约 `agent-md.md` §4.2「核心记忆区永不裁剪」/ 技术方案 §5.1 契约 2 / §4.7）：段 2 注入的核心区文本**完整**进系统提示词；超限的处置是**拒绝启动**（约束 11），**不得**静默截断、**不得**按条丢弃。
7. **核心区有写配额（有界）**（技术方案 §5.1 契约 2）：单条 ≤ **4 KB**、每层核心区总量 ≤ **32 KB**（按 **UTF-8 字节**——实测 `length('中文') = 2` 而 `octet_length('中文') = 6`，故两档一律按字节）；**只有 CORE 写受单条限制**，归档区无配额（无界是归档区的设计前提：它不进提示词）。超限 → **可读拒绝**（约束 16 / 口径 8）。
8. **写入分区由调用方显式指定**（技术方案 §5.1 契约 3）：工具参数名是 **`scope`**（取值 `CORE` / `ARCHIVAL`），落 `memory_entries.section` 列（`core` / `archive`）；**不得**把参数名改成 `section`——那样会与 `memory_entries.scope` 列（**分层** `agent` / `global`）撞名（契约 §5.4 注「两个 scope」）。症状：改名后本 US 的两处 `scope` 语义在代码与文档里彻底混淆。
9. **召回只查归档区、整串、单关键词**（契约 §5.4 / 技术方案 §5.1 契约 4）：`recall_memory` 的 `query` 是**单个关键词**，按**整串 contains** 匹配，只搜**归档区**（核心区每轮已完整注入，再检索一遍是冗余）；**不得**分词、**不得**做同义词扩展、**不得**引入向量检索。**查询词的命中条件是"连续子串"**——条目原文里含该连续短语（如条目写着 `SQLite 方案 评估`）**会命中**，而分散在条目各处、只是"这几个词都出现过"的查询（`SQLite 方案 评估` 打在只分别含 `SQLite` 与 `评估` 的条目上）**不会自动组合命中**。契约原文「传多词短语**必不命中**」的旧表述与实现不符（`strings.Contains` / `instr` 对连续短语必然命中），已按本口径**澄清并同步上游四处**（2026-10-17，见 §2.4 注 6）。实现口径：sqlite 用 `instr(content, ?) > 0`（**区分大小写**，实测读数）、markdown 用条目级 `strings.Contains`。症状：引入 `LIKE '%a%b%'` 式模糊匹配后，"不许 改 配置" 这类碎片查询会命中无关条目。
10. **无命中不是错误**（契约 §5.4 逐字）：`RecallByKeyword` 返回**空切片 + nil error**，工具把成功结果渲染成 `(无匹配条目)`；**不得**把未命中映射成 `not_found` 或 `runtime_error`。症状：模型对"没想起什么"反复重试、或反过来向用户报故障。
11. **手工编辑超配额不按写拒绝**（技术方案 §4.7 逐字）：载体里已有超限内容（人手改的文件）时——注入期**记 WARN 并完整注入**（**不得**静默截断），若因此超上下文预算 → 走**拒绝启动**路径（`failed` + `Reason` 点名 `budget.context_tokens`，零 LLM 调用）。症状：把「写配额」与「注入处理」混成一件事，用户手改的文件让整个 Agent 静默变哑。
12. **配额判定与追加必须原子**（技术方案 §5.1 契约 6）：sqlite 档「求和 + INSERT」在**同一事务**（复用 `storage.withWriteLock`——钉一条连接 + `BEGIN IMMEDIATE`）；markdown 档「解析 + 求和 + 判 + 替换」在**门面 per-scope 锁**内。**替换失败时的不变式**：旧文件**逐字节不变**、零临时文件残留——"临时文件已清理"**不足以**证明安全（替换中途失败若把目标截断/清空，记忆就静默丢了），判据必须**同时**比对新旧字节。症状：两笔并发各查各的都在限内 → 双双落盘，总量超限且再无法自然收敛。
13. **同作用域写入串行化 = 进程内 per-scope 锁**（技术方案 §5.1 契约 6；**P2-b 裁决 2026-10-17**）：锁的粒度是**完整作用域**——`core.Scope` 的**每个不同取值**一把（`Scope{Agent: "alice"}` 一把、`Scope{Agent: "bob"}` 一把、`GlobalScope` 一把），实现形态 = `map[core.Scope]*sync.Mutex` + 查表小锁；归**门面**（`internal/memory`）；跨进程 / 多实例不承诺（单实例由实例锁兜底——`run-and-errors.md` §4.3.1 既有口径）。**不得**用全仓一把大锁（把无关 Agent 的写入串成一队）；也**不得**按「层」只建两把（Agent 层一把会被**所有** Agent 共用，同样把无关 Agent 串成一队）。判据：同 `Scope` 的第二笔必须等首笔完成、不同 Agent 的写入**不得**互相阻塞（`TestService_AppendLocksAreKeyedByScope`，`blockingStore` 闸门夹具）。
14. **载体隔离**（技术方案 §5.1 契约 5 / 需求 §5.5）：`memory/` 目录**不在**文件工具白名单——`write_file` / `save_memory` 的边界就在这条线上：前者拒（`sandbox_denied` + `guidanceMemoryNotWritable` 指引逐字「记忆的写入归记忆工具，文件工具不改写 memory/（代码常量），没有配置键可以加。」），后者是**唯一**写入面。**该规则 US-4 已交付，本 US 零改动**（只新增负向用例守住它）。
15. **载体路径经工作区根句柄**（契约 §5.4 路径约定）：两档一律相对**工作区根**（`.basalt/`）定位：Agent 层 `memory/agents/<name>/MEMORY.md`、全局层 `memory/MEMORY.md`；markdown 档全部读写经 `os.OpenRoot(root)` 相对根句柄（沿 `filetools.go` 既有范式），**不得**拼绝对路径、**不得** `..` 逃逸。Agent 层文件与目录**按需创建**（`memory/agents` 有意不预建——`templates.go:13` 注释逐字）。
16. **可读拒绝**（契约 §5.4「超配额返回可读拒绝提示改写归档区」）：拒绝文本必须**点名触发的上限**并给出**可执行的下一步**（改写归档区 / 压缩内容），见口径 8；**不得**只说"失败"。归类见口径 18（**已决 #4：`invalid_input`**）。
17. **两档同语义**（技术方案 §5.3 / 需求 §5.5）：markdown 与 sqlite 两档跑**同一张场景表**（契约测试）；`Load` 文本**不承诺逐字节相同**，只承诺「同一解析器可切出同一分区结构」。换档 = 改 `memory.backend` 一行 + 重启（配置生效时机 = 重启，Operations §2.2 口径）。
18. **后端取值 fail-closed**（Compatibility §4 #2）：`memory.backend` 取值域 = `markdown`（缺省）/ `sqlite`；`mem0` 与一切未知取值 → **拒绝启动**并点名键与取值（**不得**静默降级到 markdown——那会让"我配了 mem0"变成"记忆写进了文件"）。**零新增配置键**（`memory.backend` 是既有键，`basalt.yaml.example:53-54` 零改动）。
19. **工具不落审计、不写消息、不记日志**（宪法 V / II / 技术方案 §5.4）：记忆工具只返回 `core.Result`；两阶段审计与失败分类归 `ToolExecutor`（US-4 既有）；记忆工具**不得**自行 `AppendMessage`——回填由循环统一承担。失败分类沿用执行器判序（沙箱 → 入参非法 → 取消 / 截止 → 外部系统 → 文件不存在 → 兜底）：记忆工具**只新增两类自产错误**——`*core.InvalidInputError`（入参 / 取值 / 配额）与载体 I/O 错误（原样上抛 → `runtime_error`）。
20. **空输入在工具层拒**（契约 §5.4 参数语义 + 实测；**P1-b 裁决 2026-10-17** 补规范化面）：空 `content` / 空 `query` → `invalid_input`，**零写入 / 零检索**。判据是**规范化（`TrimSpace`）后为空**——纯空白（`"   "` / `"\n\t"`）与空串**同档**；工具侧先 `args.Content = strings.TrimSpace(args.Content)`（`args.Query` 同）再 `check`，**传给载体的也是规范化文本**（约束 29）。实测依据：sqlite `instr(content, '')` 空针 → **匹配全部**——不校验的「空词召回」会把整个归档区倒给模型。症状：`{"query":""}`（或 `{"query":" "}`）返回全部归档条目。
21. **作用域只在工具层解析**（`run.go` `ToolEnv.AgentName` 是唯一来源）：`env.AgentName != ""` → `core.Scope{Agent: env.AgentName}`；`== ""` → `core.GlobalScope`。**不得**让模型传 `agent_name`、**不得**从路径反推 Agent 名（那是第二套作用域解析）。
22. **段 2 拼接形态固定**（§2.3 流程 A / 口径 3）：`Prompt.System = 段一正文 + "\n\n" + 核心区文本`；核心区为空 → **不产生任何文本**（`Prompt.System` 与 US-4 **逐字节相同**——这是本 US 最重要的回归判据，见 §4.3 R1）。记忆文本经 `RunEnv` 的**非导出注入位**传入（沿 `llmClient` / `agentDir` 先例），**不得**走 `context.WithValue`（宪法 VII）。
23. **读取失败 = 拒绝启动**（与段一读取失败同口径，`service.go` ②③ 既有形状）：门面返回 error → `RunResult{RunID, Status: StatusFailed, Reason: …点名载体…}, nil`，**零 LLM 调用**；**不得**吞错继续（fail-closed）。**"读取失败"的三档切分见约束 27**——只有 I/O 错误与非空损坏算失败，"文件不存在 / 合法空层"是**正常取值**（回退照常、首次写入即创建），**不得**把它们也当失败拒启动。
24. **两工具不持有沙箱面**（§2.4 注 3）：记忆工具**不**接受 `core.Policy` / `core.CommandRunner`——它的目标不是"工作区外的资源"，而是**固定路径上的记忆载体**；写入面由「路径约定（约束 15）+ 配额（约束 7）+ 载体隔离（约束 14）」三件封闭。症状：给记忆工具加一层 Policy，会凭空多出一个"给记忆写操作配白名单"的配置面。
25. **时间显式传入**（工程纪律）：`memory_entries.created_at` 由载体**构造器显式传入的时钟**写入（与 `RunStore.StartRun`「时间随记录传入」同源口径；此处接口签名冻结、无时间参数，故时钟走构造器），**不得**在方法体里散落 `time.Now()`。
26. **模板默认工具随本 US 落位**（需求 §10.2 逐字 / 编程指南陷阱 #20）：`agentTemplate` 的 `Tools: []string{}` → `[]string{"save_memory", "recall_memory"}`，并同步刷新其上方注释；注入池判据（`wire_test.go` 的「声明池外工具名 → 整只不注册」）随之从 6 件扩到 8 件。症状：模板落了新工具名而池未扩——新建的 Agent **整只不注册**（宪法 IV 的连带效应）。
27. **读取面错误分三档，不得用一个 `ok` 抹平**（**P1 裁决 2026-10-17**）：`text, _ := root.ReadFile(rel)` / `if ok {…}` 这类写法**一律禁止**——权限错误会被当成"空层"，运行在"没有记忆"的状态下继续，下一次写入还会把手上还有的内容整段覆盖。三档：
    - **文件不存在 / 合法空层**（不存在、零字节、或只有文件标题 + 两分区标题而无任何条目）→ **空层**：读取面返回空文本 + `nil` error（生效层回退照常走）；写入面**首次创建**（Agent 层目录 `MkdirAll` 按需建，骨架 = 与 `templateMemoryMD` 同形：文件标题 + 两分区标题，**不带说明段之外的内容**）。
    - **其他读取错误**（权限 / I/O / 句柄异常等）→ **返回错误**：注入面按约束 23 **拒绝启动**（零 LLM）；写入面**不触碰原文件**、原样上抛（→ `runtime_error`）。
    - **非空文件但结构损坏**（缺 `## 核心记忆` 或 `## 归档记忆`、同一标题出现两次、`## 归档记忆` 出现在 `## 核心记忆` 之前）→ **拒绝使用、拒绝覆盖、保留原文件逐字节不变**：`Load` / `LoadEffective` 返回点名文件与症状（缺哪个 / 重复哪个 / 次序错）的错误；`Append` **先解析再写**，损坏即拒。**不得**把"重建文件"当修复手段（那会静默删掉人手写的内容），也**不得**当空层继续。
    - **合法扩展不算损坏**：人写的多余标题（`### 小节`）、裸段落、条目正文里的任何行都按条目收集（约束 28）；判据只看三条结构事实。症状：把"合法扩展"也判成损坏 → 人手加个小节标题整个 Agent 就起不来。
28. **markdown 条目形态与分区边界**（**P2 裁决 2026-10-17**）——载体写入与解析同一套规则，两档共用一份解析器：
    - **结构行**只认**零缩进**的：文件标题（`# …`，恒为第 1 行）、分区标题（`## 核心记忆` / `## 归档记忆`）。**缩进 ≥ 2 空格的行永远只是条目正文**——条目正文里的 `## 核心记忆`（或任何看起来像标题的行）因此**不会变成核心区结构**。
    - **条目书写形态** = 零缩进列表项（`"- "` 前缀）+ 正文首行；正文**其余各行（含空行）统一加两空格缩进**（空行写成空行，无行尾空格）。故「首行 + 空行 + 缩进段落 + 缩进代码围栏」这一串是**一条**，不会被拆成多条；一条正文里出现 `"- "`、`## 归档记忆`、代码围栏都只当字符看。
    - **解析与重建**：条目 = 分区内一个零缩进行及其后的所有缩进行（空行**不**切块；下一个零缩进行才开新块）。重建 = 首行去 `"- "` 前缀、其余行去两空格缩进、两端 `TrimSpace`——写入与重建因此**无损可逆**（`Append(c)` 之后重建即得 `TrimSpace(c)`）。
    - **人为手写的裸段落**（零缩进、非 `"- "` 开头）也按条目收集并原样重建（重建会补上 `"- "` 与缩进——**注入面读的是重建结果**，文件本身不被改写）。
29. **条目正文的规范表示与配额求和口径**（**P2 裁决 2026-10-17**；**P1-b 裁决 2026-10-17** 统一表示面，**废止**本条原末句「sqlite 侧存量 `content` 列是原样存的用户输入，与重建口径天然一致」——该断言不成立，见下方首尾空白反例）：
    - **规范表示 = `strings.TrimSpace(原文)`，在写入入口规范化一次**：markdown 载体在两条配额检查**之前** `added := normalizeEntry(content)`；sqlite 载体在单条检查与 `INSERT` **之前** `c := strings.TrimSpace(content)`。两处是**同一个标准库函数**（不存在"两套算法"的分叉空间）；载体各自入口的这次调用是防线本身，重复调用幂等。
    - **写入、单条检查、总量求和、存储、重建渲染、召回（含查询词 `query`）一律作用于规范化文本**——六个面同一表示。**不得**出现"检查用原始入参、存储用另一份"的错位。
    - **求和单位** = 条目正文自身的 UTF-8 字节（**包含用户内容内部的空白与换行**；首尾空白已由规范化去掉、不计入），**只排除载体添加物**——分区标题、文件标题、条目前缀 `"- "`、续行缩进、条目之间的空行。
    - 两档同一算法（markdown 走重建文本求和、sqlite 走 `SUM(octet_length(content))`）→ **8 条恰 4096 字节的条目在两档都恰好达到 32768 并放行，第 9 笔被拒**（R6 / 契约表）。
    - **首尾空白边界必须有用例**（`TestCarriers_QuotaBoundaryExactLimit` 的子断言；markdown 侧另在 `TestMarkdownStore_QuotaCountsEntryBytesExcludingScaffold` 断言首尾空白不计入）：八条**首尾各带一个空格、原长 4096 字节**（规范化后 4094）的条目——**不规范化时** markdown 合计 32752 而 sqlite 合计 32768，紧接着写一条 `"x"` 两档判定分叉（32753 放行 / 32769 拒，"换档只改一行"在边界上失效）；**规范化后**两档**都**合计 32752、**都**放行 `"x"`。

### 2.3 核心逻辑

#### 流程图

```text
【A 注入面】每次 Run 开始一次（读一次，运行内冻结）
                          ┌──────────────────────────────────────────────┐
   memory/agents/<n>/     │ 门面 Service.LoadEffective(ctx, agent)        │
     MEMORY.md ──────────→│  ① 读候选层（agent 非空 → Agent 层；否则全局层）│
   memory/MEMORY.md       │       载体 error（I/O / 结构损坏）→ 上抛      │
     （回退）             │  ② 该层任一分区正文非空？                     │
                          │       是 → 生效层 = 该层（不叠加）            │
                          │       否 → 读全局层（缺失 / 空层 = ""，不报错）│
                          │  ③ 切出核心区文本（含 "## 核心记忆" 标题行）  │
                          │  ④ 总量 > 32KB（手工编辑）→ WARN，仍完整返回  │
                          └───────────────────────┬──────────────────────┘
                                                  │ env.memory（非导出注入位）
   ReAct 每轮：PromptBuilder.Build(env)           ↓
     Prompt.System = 段一正文 + "\n\n" + 核心区文本   ← 空核心区则不加任何字符
     （llmEnv → Message{role: system}）

【B 写入面】save_memory
   模型 → 执行器（必填校验 / 审计两阶段 / 分类）→ 工具
        → 入参规范化（TrimSpace；纯空白与空串同档，约束 20）
        → 解析作用域（Agent 层 / 全局层）与分区（CORE / ARCHIVAL）
        → 门面 Append：per-scope 锁（按**完整 Scope** 分档）→ [CORE] 单条 ≤ 4KB 判 → 载体 Append
             载体：markdown = 读（三档，约束 27）→ 解析（损坏即拒，不覆盖）
                            → 写入入口规范化（约束 29）→ 求和（≤ 32KB）
                            → 唯一名临时文件 + Rename（原子）
                   sqlite  = 写入入口同一规范化 → withWriteLock 单事务（求和 + INSERT）
        → 超限：*MemoryQuotaError → 可读拒绝（invalid_input）
        → 成功：确认文案（含作用域与分区）；下一次运行才可见

【C 召回面】recall_memory
   query 规范化后为空 → invalid_input（零检索）
   门面 RecallByKeyword → 载体：仅归档区、整串 contains（作用于规范化后的条目正文）、写入顺序
   命中 → 条目正文按空行分隔；未命中 → "(无匹配条目)"（成功）
```

#### 伪代码（签名与 §3.1 逐字一致）

```text
# 运行侧（core）：每运行读一次、运行内冻结
AgentService.Run(ctx, req):
    ② snap, err := s.opts.Agents.Snapshot(ctx, req.Agent)      # US-4 既有
       err → return RunResult{RunID, StatusFailed, Reason: err.Error()}, nil   # 零 LLM
    ④ env := &RunEnv{ Req: req, Snapshot: snap, Session: ...,
                      Budget: mergeBudget(req.Budget, snap.Profile),
                      ToolSet: snapshotTools(snap), llmClient: s.opts.Client,
                      agentDir: s.opts.Agents.Dir(req.Agent) }
       if s.opts.Memory != nil:                                # 可空端口：nil = 段 2 恒空
           text, err := s.opts.Memory.LoadEffective(ctx, snap.AgentName)
           err → return RunResult{RunID, StatusFailed, Reason: 点名载体}, nil  # 零 LLM（约束 27 / 已决 #3）
           env.memory = text                                   # ← 本运行的冻结值

PromptBuilder.Build(env):
    system := env.Snapshot.SectionOne                          # 段 1
    if env.memory != "": system = system + "\n\n" + env.memory # 段 2（空则不拼）
    return Prompt{System: system, History: trimByTurns(...), Tools: b.toolSpecs(env)}, nil

# 门面侧（memory）：作用域解析 + 生效层回退 + 锁
Service.LoadEffective(ctx, agent) (string, error):
    text, err := s.backend.Load(ctx, scopeOf(agent))           # 载体读取：I/O 失败 / 结构损坏 → err
    err != nil → return "", err                                # 上抛 → 运行拒绝启动（约束 23）
    if layerNonEmpty(text):                                    # 任一分区正文非空
        return coreSectionOf(text), nil                        # 切出核心区（含标题行）
    text, err = s.backend.Load(ctx, core.GlobalScope)           # 回退全局层（不叠加）
    err != nil → return "", err
    return coreSectionOf(text), nil                            # 空层 → ""（段 2 不产生文本）

Service.Append(ctx, scope, section, content) error:
    unlock := s.locks.lock(scope)                              # 进程内 per-scope（按完整 Scope 分档）
    defer unlock()
    return s.backend.Append(ctx, scope, section, content)      # 配额判在载体内（与追加同原子域）

# 载体侧（markdown）—— 读取面三档见约束 27；条目形态与配额口径见约束 28 / 29
MarkdownStore.Append(ctx, scope, section, content) error:
    rel := workspace.MemoryFilePath(scope.Agent)               # 路径约定唯一事实源
    root := os.OpenRoot(s.root)                                # 相对根句柄
    added := normalizeEntry(content)                           # 写入入口规范化一次（约束 29）：
                                                               #     TrimSpace；续行加两空格（约束 28）
    text, err := root.ReadFile(rel)
      errors.Is(err, fs.ErrNotExist) → text = ""               # ① 缺失 = 空层：写入时创建骨架
      err != nil                     → return err              # ② 权限 / I/O：不动原文件，原样上抛
    coreEntries, archiveEntries, err := parseSections(text)    # ③ 非空且结构损坏 → err
    err != nil → return err                                    #    拒绝覆盖：原文件逐字节不变
    if section == core.SectionCore && len([]byte(added)) > core.MaxCoreEntryBytes:
        return &core.MemoryQuotaError{Kind: QuotaKindEntry, Limit: core.MaxCoreEntryBytes}
    if section == core.SectionCore && sectionBytes(coreEntries) + len([]byte(added)) > core.MaxCoreSectionBytes:
        return &core.MemoryQuotaError{Kind: QuotaKindSection, Limit: core.MaxCoreSectionBytes}
    newText := render(fileTitle,                            # 重建；非本分区条目原样保留
                      appendIf(section == core.SectionCore, coreEntries, added),
                      appendIf(section == core.SectionArchive, archiveEntries, added))
    dir, base := path.Split(rel)
    tmp := dir + base + ".tmp-" + hex(crypto/rand 8 字节)          # os.Root **没有** CreateTemp
    f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
    err != nil → return err
    f.Write(newText) → f.Close() → err = root.Rename(tmp, rel)     # 唯一名 + 原子替换
    任一步失败：root.Remove(tmp)（清理临时文件，忽略清理错误）后原样上抛
               —— 目标未被触碰：旧文件逐字节不变（约束 12 的不变式）

# 载体侧（sqlite）—— 写入入口与 markdown 同一规范化（约束 29）
MemoryStore.Append(ctx, scope, section, content):
    c := strings.TrimSpace(content)                            # 检查与存储同一表示（幂等）
    if section == core.SectionCore:
        if len([]byte(c)) > core.MaxCoreEntryBytes → MemoryQuotaError{entry}
    return s.db.withWriteLock(ctx, func(ctx, conn):            # 单事务：检查 + 追加
        if section == core.SectionCore:
            total := SELECT COALESCE(SUM(octet_length(content)),0) FROM memory_entries
                     WHERE scope = ? AND agent_name IS ? AND section = 'core'
            total + len([]byte(c)) > core.MaxCoreSectionBytes → MemoryQuotaError{section}
        INSERT INTO memory_entries(scope, agent_name, section, content, created_at)
        VALUES (?, ?, ?, ?, s.now()))                          # content 列存的就是 c

# 工具侧（tool）
SaveMemoryTool.Execute(ctx, env, input):
    args := json.Unmarshal(input, &saveMemoryInput)            # 失败 → *core.InvalidInputError
    args.Content = strings.TrimSpace(args.Content)             # 先规范化再判空（约束 20 / 29）
    args.check(t.Name())                                       # 规范化后为空 → *core.InvalidInputError
    section := core.SectionArchive                             # 省略 / null → ARCHIVAL
    if args.Scope == "CORE": section = core.SectionCore
    else if args.Scope != nil && *args.Scope != "ARCHIVAL":    # 严格大小写
        return Result{}, &core.InvalidInputError{...}          # 零写入
    scope := core.GlobalScope; if env.AgentName != "": scope = core.Scope{Agent: env.AgentName}
    err := t.store.Append(ctx, scope, section, args.Content)
    errors.As(err, &quota) → return Result{}, &core.InvalidInputError{Tool: t.Name(), Reason: 口径 8 文案}
    err != nil → return Result{}, err                          # runtime_error
    return Result{Content: "已记住（" + 层名(scope) + " · " + 分区名(section) + "）"}, nil

RecallMemoryTool.Execute(ctx, env, input):
    args := json.Unmarshal(input, &recallMemoryInput)
    args.Query = strings.TrimSpace(args.Query)                                # 纯空白与空串同档（约束 20）
    args.check(t.Name())                                                      # 规范化后为空 → invalid_input
    hits, err := t.store.RecallByKeyword(ctx, scopeOf(env), args.Query)       # 只查归档区
    err != nil → return Result{}, err
    len(hits) == 0 → return Result{Content: "(无匹配条目)"}, nil              # 成功
    return Result{Content: strings.Join(hits, "\n\n")}, nil
```

### 2.4 差异裁决注

1. **注 1（注入面：核心区 vs 整层）**：技术方案 §5.1 的 `Load` 注释写「读取指定层的完整记忆（核心区 + 归档区，核心区永不截断）」——**读取**面确实是整层；但同一节的行为契约 2 把写配额只放在**核心区**，契约 `agent-md.md` §5.4 又逐字写「核心区**不参与检索**——它每轮已完整注入」，§4.2 的裁剪纪律写「**核心记忆区**永不裁剪」，§4.7 的不可裁剪内容列也只列「**核心记忆**」。本 US 取**核心区进段 2、归档区只经 `recall_memory` 取回**（口径 1）——两条不冲突：`Load` 是**读取**接口（管理与巡检要能看到全层），段 2 是**注入**面（必须有界）。整层全量注入的替代案与影响面见文末「待决事项」的原第 1 条（**已决 D1 = 只注核心区**，该表只作取舍留痕）；若改判，受影响处：§2.3 流程 A、§3.1 的 `MemoryReader` 注释、§4.2 的注入映射行、§6.2 口径 1 / 6 / 7 与 R1。
2. **注 2（载体分档与包落位）**：编程指南 §4.6 的「涉及包」列把 `memory` 与 `storage（memory_entries）` **并列**——本 US 据此把 **sqlite 载体落 `internal/storage`**（复用 `withWriteLock` 单事务原语与既有 store 范式），**markdown 载体与门面落 `internal/memory`**。「三档载体归门面」的口径不因此改变：**门面**（作用域、回退、锁、注入期 WARN）只有一份在 `memory`，载体是可替换的**实现**（`var _ core.LongTermMemoryStore`）。
3. **注 3（记忆工具不持有 Policy / CommandRunner）**：宪法 VI 的「授权与执行分离两接口」针对的是**沙箱面**（工作区外的路径、命令、域名）。记忆工具的目标是**固定路径上的记忆载体**，不构成"另一个可被模型指向的位置"：作用域由 `env.AgentName` 决定（模型只能给分区）、路径由工作区约定拼出、写入面由配额封闭、`memory/` 对文件工具不可写。故记忆工具**接受 `core.LongTermMemoryStore`，不接受 `Policy`**——这是**契约留白处的设计选择**，不是对宪法 VI 的豁免：它没有"执行外部进程 / 访问越界路径"的能力可供授权。
4. **注 4（工作区路径字面量的第三处）**：`memory/` 与 `memory/agents/<name>/MEMORY.md` 字面量现有两处——`workspace/templates.go`（模板与目录）与 `tool/policy.go`（`memoryDirName = "memory"` + 目录规则）。本 US 新增 `workspace.MemoryFilePath` 作为**唯一事实源**供 `memory` 包取用；**不顺手改写** `policy.go` 的既有本地常量（范围外重构）。漂移由 §4.3 **R7** 守住：`write_file` 写 `memory/<任意路径>` 必须被拒——`policy.go` 的字面量一漂，R7 当场红。**R2 补强**：R7 断言的不再只是"被拒了"，而是**类型化字段逐字**（`*core.SandboxViolationError` 的 `Rule` = `记忆目录不在可写范围`、`Guidance` = 固定指引、`Target` = 工作区相对形态）——只断言 `IsSandboxViolation` 会被**通用越界规则**冒充通过（评审实测：删掉 memory 专属分支，`ruleWriteOutside` 顶上，旧断言照样绿）。
5. **注 5（提示词四段表的 [2] 与实现）**：技术方案 §4.2 的四段表把 [2] 写作「记忆注入（长期记忆：……）」、[3] 写作「对话历史（持久化部分）」；§5.4 又注明「`USER.md` vs `MEMORY.md`：前者用户手写、底座只读不写；后者 Agent 通过 `save_memory` 写入。**两者都进系统提示词**」。本 US 的落点是：`USER.md` 已在 **US-4 的段 1**（Bootstrap 三件与 AGENT.md 正文同为 `Snapshot.SectionOne`）里进提示词，**本 US 不移动它的位置**；段 2 的新增面**只有** `MEMORY.md` 的生效层核心区。四段表的编号与实现的对应关系照实登记，**不擅自改文档**（口径以技术方案为准，差异留在本注）。
6. **注 6（检索语义的上游澄清——登记 + 同步，非单点改 US-6）**：契约 `agent-md.md` §5.4 原文写「传多词短语（`"SQLite 方案 评估"`）**必不命中**——这是已知行为，不是缺陷」，但实现语义是整串 `contains`（`strings.Contains` / `instr`）：**只要条目原文里含该连续短语就必然命中**，"必不命中"与实现不符。**2026-10-17 用户裁决**：整串子串匹配；不分词、不扩展；**连续短语可以命中**，分散出现的词不会自动组合命中。该句是**上游表述错误**，故同步四处上游（**不止改 US-6**）。**同步已完成（2026-10-17）**：
   - `docs/contracts/agent-md.md` §5.4（源头，固定字面量）——改写为"连续子串命中 / 分散词不自动组合"，并加 2026-10-17 澄清块指明其余三处已同步；`query` 仍要求**单个关键词**（参数表不变）。
   - `docs/Compatibility.md` §2.2 对照行「长期记忆门面工具」的备注（原写"含多词短语必不命中这条已知行为"，现写"整串子串匹配——连续短语命中、分散词不自动组合"，**对照结论"无需动作"不变**）。
   - `docs/AiProgrammingGuide.md` §7 陷阱 #11（症状与复核口径同步为**两态**；陷阱**编号不动**）。
   - `docs/Roadmap.md` B 方向两处 + M2 里程碑一处（同一句改写：B 触发信号、B 卡点「承诺契约」的括注、M2 可演示成果）。
   技术方案 §5.1 契约 4（"归档区按整串匹配；不得分词 / 扩展 / 向量化"）**本就兼容**，零改动。**测试面禁止钉住错误行为**：不得写"多词短语必不命中"的断言，改钉**两态**——条目原文含连续短语 → 命中；词分散出现 → 不命中（R5 附近，见 §4.3）。

## 3. 交付物清单

### 3.1 代码交付物

#### 新增 6 文件

| 文件 | 内容 | 关键方法签名 |
| --- | --- | --- |
| `internal/core/memory.go` | 记忆接口与端口（纯类型 + 常量）：`Scope` / `GlobalScope` / `Section` / 两常量 / `LongTermMemoryStore`（三方法**逐字**）/ `MemoryReader` / 配额常量 / `MemoryQuotaError`；`core` 只依赖标准库 | `type Scope struct { Agent string }`；`var GlobalScope = Scope{}`；`type Section string`；`const (SectionCore Section = "core"; SectionArchive Section = "archive")`；`const (MaxCoreEntryBytes = 4096; MaxCoreSectionBytes = 32768)`；`const (QuotaKindEntry = "entry"; QuotaKindSection = "section")`；`type LongTermMemoryStore interface { Append(ctx context.Context, scope Scope, section Section, content string) error; Load(ctx context.Context, scope Scope) (string, error); RecallByKeyword(ctx context.Context, scope Scope, query string) ([]string, error) }`；`type MemoryReader interface { LoadEffective(ctx context.Context, agent string) (string, error) }`；`type MemoryQuotaError struct { Kind string; Limit int }`；`func (e *MemoryQuotaError) Error() string` |
| `internal/memory/memory.go` | 记忆**门面**：作用域 → 分层读取、生效层回退（不叠加）、核心区解析（**一份解析器两档共用**）、**per-scope 锁（按完整 `core.Scope` 分档——`map[core.Scope]*sync.Mutex` + 查表小锁；每个 Agent 一把 + 全局层一把）**、注入期超配额 **WARN**；`var _ core.LongTermMemoryStore = (*Service)(nil)` / `var _ core.MemoryReader = (*Service)(nil)` | `type Options struct { Backend core.LongTermMemoryStore; Logger *slog.Logger }`；`func NewService(opts Options) (*Service, error)`（`Backend == nil` → 错误，fail-closed）；`func (s *Service) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error`；`func (s *Service) Load(ctx context.Context, scope core.Scope) (string, error)`；`func (s *Service) RecallByKeyword(ctx context.Context, scope core.Scope, query string) ([]string, error)`；`func (s *Service) LoadEffective(ctx context.Context, agent string) (string, error)` |
| `internal/memory/markdown.go` | markdown 载体：`os.OpenRoot` 根句柄读写、两分区解析与渲染（**条目形态与结构行判据见约束 28**）、**写入入口规范化**（`normalizeEntry` = `TrimSpace`，先于两条配额检查与重建——约束 29）、**唯一名临时文件 + rename** 原子替换（失败清理且**旧文件逐字节不变**）、归档区条目级整串检索、配额求和（**条目正文 UTF-8 字节**——约束 29）、Agent 层文件**按需创建**；**读取面三档**（缺失 / I/O / 损坏）见约束 27；路径取 `workspace.MemoryFilePath` | `type MarkdownStore struct{ ... }`；`func NewMarkdownStore(root string) *MarkdownStore`；`func (m *MarkdownStore) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error`；`func (m *MarkdownStore) Load(ctx context.Context, scope core.Scope) (string, error)`；`func (m *MarkdownStore) RecallByKeyword(ctx context.Context, scope core.Scope, query string) ([]string, error)`；非导出：`parseSections(text string) (coreEntries, archiveEntries []string, err error)` / `renderMemory(title string, core, archive []string) string` / `normalizeEntry(content string) string`（首个与 §2.3 伪代码逐字同名） |
| `internal/storage/memory_store.go` | sqlite 载体：六列读写、**写入入口 `strings.TrimSpace` 规范化**（与 markdown 同一个标准库函数，先于单条检查与 `INSERT`——约束 29）、`octet_length` 求和、**`withWriteLock` 单事务**「配额检查 + INSERT」、归档区 `instr` 检索、规范文本渲染（同形两分区）；`var _ core.LongTermMemoryStore = (*MemoryStore)(nil)`；**零迁移** | `type MemoryStore struct{ ... }`；`func NewMemoryStore(db *DB, now func() time.Time) *MemoryStore`；`func (s *MemoryStore) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error`；`func (s *MemoryStore) Load(ctx context.Context, scope core.Scope) (string, error)`；`func (s *MemoryStore) RecallByKeyword(ctx context.Context, scope core.Scope, query string) ([]string, error)` |
| `internal/tool/memorytools.go` | 两件工具：内联 JSON Schema（`save_memory`：`content` 必填 + `scope` 枚举 `CORE` / `ARCHIVAL`；`recall_memory`：`query` 必填）、`decode + 规范化 + check` 范式（先 `TrimSpace` 再判空——纯空白与空串同档；失败归 `*core.InvalidInputError`）、作用域解析（`ToolEnv.AgentName`）、配额拒绝映射、确认 / 召回文案；`var _ core.Tool = (*SaveMemoryTool)(nil)` / `var _ core.Tool = (*RecallMemoryTool)(nil)` | `type SaveMemoryTool struct{ ... }`；`func NewSaveMemoryTool(store core.LongTermMemoryStore) *SaveMemoryTool`；`func (t *SaveMemoryTool) Name() string` / `Description() string` / `InputSchema() json.RawMessage` / `Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error)`；`type RecallMemoryTool struct{ ... }`；`func NewRecallMemoryTool(store core.LongTermMemoryStore) *RecallMemoryTool` + 同四方法；非导出：`saveMemoryInput` / `recallMemoryInput`（各带 `check(toolName string) error`）、`saveMemoryInputSchema` / `recallMemoryInputSchema` |
| `internal/workspace/memory.go` | 工作区记忆路径约定（**唯一事实源**，注 4）：相对工作区根 | `func MemoryFilePath(agent string) string`（`agent == ""` → `memory/MEMORY.md`；否则 `memory/agents/<name>/MEMORY.md`） |

#### 改动 7 文件

| 文件 | 改什么 | 关键方法签名（改动面） |
| --- | --- | --- |
| `internal/core/run.go` | `RunEnv` 增**第三个非导出注入位** `memory string`（沿 `llmClient` / `agentDir` 先例；注释声明「不是契约字段、不进任何出站形态」）；`ToolEnv` 零改动 | （无导出签名变化） |
| `internal/core/prompt.go` | `Build` 拼段 2：`system = env.Snapshot.SectionOne + "\n\n" + env.memory`（`env.memory == ""` → 与 US-4 **逐字节相同**）；刷新 `Prompt.System` 上方那段「段 2 恒空」的注释 | `func (b *PromptBuilder) Build(env *RunEnv) (Prompt, error)`（签名不变） |
| `internal/core/service.go` | `ServiceOptions` 增 `Memory MemoryReader`（**可空**：nil = 段 2 恒空）；步骤 ④ 读一次生效记忆（失败 → 既有 `StatusFailed` 形状，零 LLM）并写入 `env.memory` | `type ServiceOptions struct { ...; Memory MemoryReader; ... }` |
| `internal/memory/doc.go` | 包注释改写：从骨架占位改为本包职责（门面 / markdown 载体 / 两档共用的分区解析 / per-scope 锁 / 注入期 WARN） | — |
| `internal/workspace/templates.go` | `agentTemplate` 的默认工具 `[]string{}` → `[]string{"save_memory", "recall_memory"}`，同步刷新其上方注释（`// tools: []（记忆工具到 US-6 才在池）` → 声明即默认池内两件） | — |
| `internal/cli/config.go` | `Config.Memory map[string]any` → 类型化 `Memory MemoryConfig`；`validateConfig` 增取值域校验（缺省 `markdown`；`mem0` / 未知 → 拒绝启动并点名键与取值）；`basalt.yaml.example` **零改动**（键已存在） | `type MemoryConfig struct { Backend string }`（yaml 键 `backend`）；`func LoadConfig(path string, lookupEnv func(string) (string, bool)) (Config, error)`（签名不变） |
| `internal/app/wire.go` | 装配：④ 与 ⑤ 之间构建记忆门面（按 `cfg.Memory.Backend` 选载体：`markdown` → `memory.NewMarkdownStore(root)`；`sqlite` → `storage.NewMemoryStore(db, now)`；default → 错误，fail-closed 双保险）；⑤ 注册池 **6 → 8**（两件记忆工具）并刷新占位注释；⑦ `ServiceOptions.Memory` 接线 | （无新增导出签名；内部 helper `newMemoryService(...)` 由实现侧定名） |

### 3.2 测试交付物

#### 新增 6 文件 41 条（**P1-b / P2-b 裁决后：原 39 条 + `memory_test.go` 的锁分档 1 条；R4 / analyze C1 处置后：+ `import_test.go` 架构测试 1 条**）

| 文件 | 条数 | 用例（函数名英文；中文验收点以注释保留） |
| --- | --- | --- |
| `internal/memory/memory_test.go` | 10 | `TestService_AppendResolvesScopeFromAgentName`（有 Agent → `memory/agents/<name>/MEMORY.md`；无 → `memory/MEMORY.md`）· `TestService_LoadEffective_TwoLayerFallbackWithoutStacking`（**R3**：全局层回退 / Agent 层不叠加 / **Agent 层只有归档区时也不回退全局核心区** / 无上下文 = 全局层 / 归档区不进注入面，五步同一函数）· `TestService_LoadEffective_ReturnsCoreSectionOnly`（归档区不进返回值）· `TestService_LoadEffective_ErrorTiersDoNotCollapse`（**R9**：三档不塌成一档——缺失档照常回退 / I/O 档上抛且不回退 / 损坏档拒绝使用与覆盖且**原文件逐字节不变**）· `TestService_RecallByKeyword_WholeStringCaseSensitiveArchiveOnly` · `TestService_ConcurrentAppendSameScopeSerialized`（`-race`）· **`TestService_AppendLocksAreKeyedByScope`**（**P2-b**：锁按**完整 Scope** 分档——`blockingStore` 闸门夹具下 ① 同 `Scope` 的第二笔在首笔放行前**不得**进入载体（串行）；② `alice` 阻塞期间 `bob` 的追加**必须**进入载体（不同 Agent 不互相阻塞）。**逆改点**：把 `map[core.Scope]*sync.Mutex` 换成全仓一把（或按层的两把）→ ② 等不到 `bob` 的进入信号而超时红）· `TestService_AppendQuotaRejectionIsTyped`（`*core.MemoryQuotaError` 两档 `Kind`）· `TestService_RequiresBackend`（`Options.Backend == nil` → 构造失败）· `TestOversizedManualMemory_WarnsAndInjectsFully`（**R4a**：手工堆过 32 KB → WARN + 注入完整不截断） |
| `internal/memory/markdown_test.go` | 8 | `TestMarkdownStore_AppendCreatesAgentLayerOnDemand`（含骨架 = 同形两分区标题）· `TestMarkdownStore_AppendWritesToRequestedSection` · `TestMarkdownStore_AppendReplacesAtomicallyWithoutResidue`（成功 / 失败两态：无临时文件残留 **+ 失败时旧文件逐字节不变**——约束 12）· `TestMarkdownStore_AppendRefusesCorruptedFileKeepsOriginalBytes`（**缺分区标题的文件：读报错、写拒、原文件零改动**——约束 27 第三档）· `TestMarkdownStore_LoadReturnsFileVerbatim` · `TestMarkdownStore_RecallMatchesWholeStringWithinEntries`（区分大小写；未命中 = 空 + nil；**连续短语命中 / 分散词不命中两态**——注 6）· `TestMarkdownStore_QuotaCountsEntryBytesExcludingScaffold`（中文 3 字节 / 字符；标题、`"- "` 前缀与缩进不计入；**首尾空白同样不计入**——写入入口 `normalizeEntry` 先 `TrimSpace`，同一函数内子断言，约束 29）· `TestMarkdownStore_ParseToleratesExtraHeadingsAndRejectsBrokenShape` |
| `internal/memory/carrier_contract_test.go` | 7 | **同一张场景表跑两档**（`package memory_test`，test-only import `internal/storage`，注 2）：`TestCarriers_ScenarioTable` · `TestCarriers_QuotaBoundaryExactLimit`（恰 4096 / 32768 放行；超 1 字节拒——**8 条恰 4096 字节的条目在两档都恰好到顶、第 9 笔拒**；**P1-b 首尾空白档**：八条首尾各带一空格、原长 4096 字节（规范化后 4094）的条目 → 两档**都**合计 32752 且**都**放行紧接的 `"x"`（不规范化时两档会分叉成 32752 / 32768——约束 29 末条））· `TestCarriers_EntryBoundariesSurviveRoundTrip`（**同一张边界场景表**：条目含空行 / 代码围栏 / `## 核心记忆` / `## 归档记忆` / `"- "` 开头行 → 两档都**一条进一条出**、归档正文里的标题不变成核心区结构、重建后逐字节等于原输入——约束 28）· `TestCarriers_RecallPhraseHitsOnlyWhenContiguous`（连续短语命中 / 分散词不命中两态——注 6）· `TestCarriers_ConcurrentAppendKeepsEveryEntry`（两档 `-race`）· `TestCarriers_LoadShapeIsCarrierNeutral`（同一解析器切出同一分区结构）· `TestCarriers_ScopeIsolationAgentVsGlobal` |
| `internal/storage/memory_store_test.go` | 6 | `TestMemoryStore_AppendAndLoadRoundTrip` … |
| `internal/memory/import_test.go` | 1 | **包级架构测试**（口径 26 / FR-038，R4 处置新增）：`TestDependencyDirection_MemoryImportsOnlyCoreWorkspaceStdlib` ——`go list -f '{{join .Imports " "}}'` 读 `internal/memory` 的**生产导入面**（`{{.Imports}}` 是**非测试**导入集），对每个导入项断言「标准库（路径首段不含点）**或**白名单内（`core` / `workspace`）」——**正向白名单**（比 `cli/ports_test.go` 的禁 import 清单更强的形态：越界依赖不靠枚举穷举）；`package memory_test`，沿 `cli/ports_test.go` 先例（`go list` 不可用即判失败）。**逆改点**：给任一生产文件加 `_ "github.com/ymedword/basalt/internal/storage"` → 断言红；test-only 导入（`carrier_contract_test.go` 那份）**不受影响**（不在 `.Imports` 里） |
| `internal/tool/memorytools_test.go` | 9 | **`package tool_test`**（外部测试包，与 `policy_test.go` / `filetools_test.go` 同形——R7 断言的就是 `policy.go` 的规则，构造子带 `tool.` 前缀；`go list` 判据只读**非测试**导入集，测试侧转调真实现包是既有纪律）：`TestSaveMemoryTool_InputSchemaContract`（参数名 / 必填 / 枚举逐字）· `TestRecallMemoryTool_InputSchemaContract` · `TestSaveMemoryTool_DefaultsScopeToArchival`（省略与 `null` 两态）· `TestSaveMemoryTool_RejectsUnknownScopeCaseSensitive`（`core` / `Core` / `ARCHIVE` → `invalid_input`、零写入）· `TestMemoryTools_RejectEmptyInputs`（**R8**：**四态**——`content` 空串与纯空白（`"   \n\t "`）、`query` 空串与纯空白（`" "`）都必须拒；`countingStore` 证"零写入 / 零检索"；**逆改点**：只判 `== ""` 不规范化 → 纯空白两态红）· `TestMemoryDirIsolation`（**R7**：文件工具写 `memory/` 被拒——**类型化字段逐字 + 文件逐字节不变** + 记忆工具可写，同函数正反对照）· `TestRecallMemoryTool_NoHitReturnsSuccessPlaceholder` · `TestSaveMemoryTool_MapsQuotaToReadableInvalidInput`（文案点名上限 + 改写归档区指引）· `TestMemoryTools_ConfirmationNamesScopeAndSection` |

> `internal/storage/memory_store_test.go` 的 6 条：`TestMemoryStore_AppendAndLoadRoundTrip`（含 `created_at` 递增 + **首尾空白入参存的是规范化文本**——读回无首尾空白，同一函数内子断言，约束 29）· `TestMemoryStore_ConcurrentAppendNeverExceedsCoreQuota`（**R6**：配额检查与追加同事务——单条越限 `Kind = entry`、总量边界 8 笔、并发 12 笔）· `TestMemoryStore_RecallUsesInstrOnArchiveOrderedByID` · `TestMemoryStore_GlobalScopeUsesNullAgentName`（`agent_name IS NULL`）· `TestMemoryStore_InsertFailureLeavesNoRow` · `TestMemoryStore_RenderedTextIsParseableBySharedParser`（渲染文本可被共享解析器切分——两档同形）。

#### 改动 7 文件，追加 17 条 + 判据扩 1 处 + 夹具扩展 2 点

| 文件 | 追加条数 | 用例 / 改动 |
| --- | --- | --- |
| `internal/core/loop_test.go` | 0（**夹具扩展**） | 本文件是 **`package core_test`**（外部测试包）；`loopHarness` 增 `memory core.MemoryReader` 字段 + `type harnessOption func(*loopHarness)` 与 `withMemory(r core.MemoryReader) harnessOption`，`newLoopHarness(t *testing.T, opts ...harnessOption)` **在构造 `core.NewAgentService(...)` 之前应用 opts**——`ServiceOptions` 是构造时装配的，**构造后再赋 `h.memory` 不会接线**（默认 nil = 既有用例零影响，既有调用点保持 `newLoopHarness(t)` 不破）；另增两个测试替身：`fakeMemoryReader{text string}` 与 `scriptedMemoryReader`（逐次返回不同文本，暴露"每轮重读"） |
| `internal/core/prompt_test.go` | 3 | `TestPromptBuilder_Build_InjectsMemorySegment` · `TestPromptBuilder_Build_EmptyMemoryIsByteIdenticalToSectionOne` · `TestPromptBuilder_Build_MemoryCountsTowardTokenBudget`（段 2 计入 `estimateTokens(p.System)`，超限 error 点名 `budget.context_tokens`） |
| `internal/core/service_test.go` | 5 | `TestAgentService_InjectsCoreMemorySegment`（**R1**：段 2 注入形态 + 空记忆逐字节回归）· `TestAgentService_MemoryReadOncePerRun`（**R2**，配合 `scriptedMemoryReader`）· `TestAgentService_MemoryReadFailureFailsRunBeforeModelCall` · `TestAgentService_NilMemoryLeavesSegmentTwoEmpty` · `TestAgentService_OversizedMemoryFailsRunBeforeModelCall`（**R4b**：注入后超上下文预算 → 拒绝启动、零 LLM、Reason 点名 `budget.context_tokens`）；`assemble(maxConcurrent)` 的构造点同批接线 |
| `internal/workspace/templates_test.go` | 2 | `TestTemplates_AgentTemplateDeclaresMemoryTools`（`tools: [save_memory, recall_memory]` 逐字）· `TestMemoryFilePath_Convention`（两路径 + 无前导斜杠） |
| `internal/workspace/agent_test.go` | 1 | `TestAgentLoader_MemoryToolsInPoolRegisterAgent`（池含两件记忆工具时，声明它们的 Agent 正常注册——与"池外工具名 → 整只不注册"成对） |
| `internal/cli/config_test.go` | 3 | `TestLoadConfig_MemoryBackendDefaultsToMarkdown` · `TestLoadConfig_AcceptsSqliteBackend` · `TestLoadConfig_RejectsUnknownMemoryBackend`（`mem0` 与未知取值 → 拒绝启动并点名） |
| `internal/app/wire_test.go` | 3 + 判据扩 1 处 | `TestWire_MemorySegmentReachesModelRequest`（段 2 进模型入站 system 消息）· `TestWire_SaveMemoryThenSecondRunRecalls`（Demo 二最小闭环：真装配 + 真工具写 → 第二次运行段 2 含该条目；两档各跑一次）· `TestWire_SqliteBackendLeavesWorkspaceFileUntouched`（sqlite 档：`memory/MEMORY.md` 保持模板原文）；**判据扩 1 处**：`TestWire_RegistryNamesReachAgentLoader` 的池名集从 6 → 8 件（含 `save_memory` / `recall_memory`） |

### 3.3 配置与工程

| 项 | 结论 | 核对动作（module-dev 登记为核对任务，非改动任务） |
| --- | --- | --- |
| `go.mod` / `go.sum` | **零新增坐标** | 全部依赖：标准库（`os` / `path/filepath` / `strings` / `sync` / `errors` / `fmt` / `context` / `log/slog` / `encoding/json` / `time`）+ 既有 `modernc.org/sqlite`（sqlite 载体）+ 既有 `stretchr/testify`（测试）。写出后 `git diff --stat go.mod go.sum` 为空 |
| `basalt.yaml.example` | **零改动** | `memory.backend` 键已存在（`:53-54`），本 US 只把它的**取值域**写进 `validateConfig` |
| 新增配置键 | **零新增** | 配额（4 KB / 32 KB）是**代码常量**（技术方案 §5.1 契约 2 未列为配置项）；载体切换只用既有键；Profile 无配额覆盖权 |
| `Makefile` / CI | **零改动** | 门禁七目标照跑；覆盖率底线不变（脚本 `scripts/check-coverage.sh` 的头注释逐字：**`core` / `tool` / `memory` / `storage` ≥ 70%**，其余 ≥ 50%）——`tool` 本来就是 70% 档（R2 更正：R1 把它误列为 50%），`memory` 是本 US 从骨架变主体的包，同按 70% 判 |
| 迁移脚本 | **零迁移** | `memory_entries` 与 `idx_memory_scope` 由 US-1 建齐（§3.4）；`schema.sql` 零 diff |

### 3.4 表与列级口径

`memory_entries`（`internal/storage/schema.sql:230-239`，US-1 已建齐——**本 US 零迁移，逐字引用**）：

| 列 | 类型 | 口径 |
| --- | --- | --- |
| `id` | `INTEGER PRIMARY KEY AUTOINCREMENT` | 写入顺序的事实源（召回按 `id` 升序） |
| `scope` | `TEXT NOT NULL` | **分层**：`agent` / `global`（≠ 工具参数 `scope`——那是分区，注见约束 8） |
| `agent_name` | `TEXT` | `scope = 'agent'` → Agent 名；`scope = 'global'` → **NULL**（查询恒用 `IS NULL`，**不得**用空串） |
| `section` | `TEXT NOT NULL` | **分区**：`core` / `archive`（工具参数 `CORE` / `ARCHIVAL` 落此列） |
| `content` | `TEXT NOT NULL` | 条目原文（**非空**——空 `content` 在工具层即拒，约束 20）；配额按 `SUM(octet_length(content))` 求和——存的就是"条目正文自身的字节"，与 markdown 档的重建口径（约束 29）逐字节同源 |
| `created_at` | `INTEGER NOT NULL` | 载体时钟（构造器显式传入，约束 25） |

索引 `idx_memory_scope(scope, agent_name, section, id)` 覆盖三类查询：分层核心区求和（配额）、分层全量读（`Load`）、分层归档区检索（`instr`）。查询形状固定：Agent 层 `WHERE scope = 'agent' AND agent_name = ?`；全局层 `WHERE scope = 'global' AND agent_name IS NULL`。

### 3.5 前序改造点与后续占位

**前序改造点 9 项**（触及前序交付契约——**逐条属软门禁**，动手前与用户确认；其中 ①②③ 改变既有测试的判据面，⑨ 是上游文档同步）

| # | 改造点 | 前序契约面 | 影响既有测试 |
| --- | --- | --- | --- |
| 1 | `cli/config.go`：`Memory map[string]any` → 类型化 `MemoryConfig{Backend string}` + 取值域校验 | `Config` 结构（US-3 配置面）；`basalt.yaml.example` 已含该键 | `cli/config_test.go`：既有断言若依赖 map 形态需**等价重构**（取值不变）；`mem0` 从"可解析"变"拒绝启动"——**判据面变更**，随 `TestLoadConfig_RejectsUnknownMemoryBackend` 一并送审 |
| 2 | `workspace/templates.go`：`agentTemplate` 默认工具 `[]` → `["save_memory", "recall_memory"]` | 模板内容判据（US-1）；需求 §10.2 逐字要求 | `workspace/templates_test.go` / `agent_test.go`：既有 `tools` 为空的断言需同步（**判据面变更**） |
| 3 | `wire.go`：注册池 6 → 8 + 占位注释刷新 | US-5 交付的六件池与注解 | `app/wire_test.go`：`TestWire_RegistryNamesReachAgentLoader` 判据扩 1 处（6 → 8） |
| 4 | `core.ServiceOptions.Memory`（新增**可空**字段）+ `RunEnv` 第三非导出注入位 + `Build` 拼段 2 | US-4 的 `ServiceOptions` / `RunEnv` / `Prompt` 结构 | `core` 既有用例：`Memory == nil` → 行为与 US-4 **逐字节相同**，故**零改动**；仅夹具扩展（`loopHarness` 增字段） |
| 5 | `internal/workspace/memory.go` 新增 `MemoryFilePath` | 工作区路径约定的**新增**导出面 | 无（新面） |
| 6 | `internal/storage` 新增 `memory_store.go` | storage 既有 store 范式；`withWriteLock` 为**非导出**原语（本 US 在包内复用，**不导出**） | 无（新文件） |
| 7 | `internal/memory/doc.go` 改写 + 包主体新增 | US-1 的骨架占位 | 无（既有文件只有一行骨架注释） |
| 8 | 测试夹具扩展：`loopHarness.memory` + `withMemory` 选项与 `service_test.go` 的构造点接线 | core 测试夹具 | 属"随已批准接口适配"（新增字段与选项、默认 nil）；**不改变任何既有判据** |
| 9 | **上游文档同步（检索语义澄清，注 6）——已完成 2026-10-17**：`docs/contracts/agent-md.md` §5.4（含 2026-10-17 澄清块）、`docs/Compatibility.md` §2.2、`docs/AiProgrammingGuide.md` §7 陷阱 #11（编号不动）、`docs/Roadmap.md` B 方向两处 + M2 一处 | 契约 §5.4 固定字面量（"多词短语必不命中"与实现不符）；三处文档转述同一句 | 无 Go 测试改动；**测试面禁止钉住旧表述**——不得写"多词短语必不命中"的断言，改钉连续短语 / 分散词**两态**（§4.3 与 `memory_test.go`） |

**后续占位 5 项**（本 US **只留位不实现**）

| # | 占位 | 归属 |
| --- | --- | --- |
| 1 | `notify` / `manage_schedule` 两件工具（注册池第 9 / 10 件） | US-8 / US-10（`wire.go` 注释保持占位） |
| 2 | `mem0` 载体档（`memory.backend: mem0`） | 扩展阶段（Compatibility §4 #2 / U-8 裁决）；本 US 取值域外 = **拒绝启动** |
| 3 | 记忆管理读取端点（`GET /api/v1/memory`，管理台记忆页） | US-9 / US-12；现成读取面 = 门面 `Load`（整层）与 `RecallByKeyword` |
| 4 | 归档区裁剪 / 压缩 / 摘要（归档区无界，长期只增） | 扩展阶段；Compatibility §2.4 #41「记忆 / 召回**只追加、删除留缺口**」是当前承诺 |
| 5 | Agent 删除时的记忆清理 | 核心阶段语义：**存量记忆不迁移、不删除**（技术方案 §5.2 逐字）；动态删除归 Agent 动态管理交付物，本 US 不改 |

## 4. 验收测试（Harness）

### 4.1 测试分层标准

- **默认层**（`make test`，进 `make check`）：**不访问真实网络、不打真实模型、不起真实子进程**。本 US 的"真实"只到**真实文件系统**（`t.TempDir()` 下的工作区根与 `MEMORY.md`）与**真实 SQLite 库文件**（`t.TempDir()/basalt.db`，与 `internal/storage` 既有夹具同形）——载体是本地资源，不引入外部依赖。
- **集成层**（`-tags=integration`）：本 US **无新增**（记忆全链路可在默认层闭合：`OpenRuntime` + `scriptedProviderClient` 已是现成的真装配夹具）；既有集成冒烟照跑。
- **并发面**：涉及共享状态的 5 条（两档并发写、单事务配额、运行内冻结、**锁按完整作用域分档**）必须由 `go test -race` 覆盖（`-race` 与发布构建分属不同命令，宪法 VIII 陷阱）。
- **覆盖率**：`memory`（新主体）≥ 70% 底线，其余按包既有底线——由 `make cover` 判卷。

### 4.2 测试类 → 验收点映射表

| 测试类（文件） | 覆盖的验收点 / 约束 | 依据（事实源） |
| --- | --- | --- |
| `memory_test.go` | 作用域解析（约束 3 / 21）· 回退不叠加 **+ 只有归档区的层不回退**（约束 4 / 口径 2）· 生效层 = 核心区（口径 1 / 2）· **读取面三档不塌成一档**（约束 27 / **R9**）· 手工超配额 WARN 且**完整注入**（约束 11 的注入侧 / **R4a**）· 整串区分大小写召回（约束 9）· per-scope 串行 + **锁按完整作用域分档**（同 Scope 串行、不同 Agent 不互相阻塞；约束 13 / **P2-b**）· 配额类型化（约束 7 / 16）· fail-closed 构造（约束 18 同族） | 技术方案 §5.1 契约 1~6 / §5.2；契约 §5.4；需求 §9.3 |
| `markdown_test.go` | 路径与按需创建（约束 15）· 分区写入（约束 8）· 原子替换无残留 **+ 失败时旧文件逐字节不变**（约束 12）· **损坏文件读报错 / 写拒绝且零改动**（约束 27）· `Load` 原文（口径 12）· 归档区条目级检索与两态召回（约束 9 / 注 6）· 条目正文字节口径（约束 29）· 解析器容错与损坏判据（口径 21 / 约束 28） | 技术方案 §5.1 契约 2 / 6 / §5.3；契约 §5.4 |
| `carrier_contract_test.go` | **两档同语义**（约束 17）· 配额边界（8 × 4096 恰 32768）与并发（约束 7 / 12 / 29）· **条目边界往返**（约束 28）· 召回两态与未命中（约束 9 / 10 / 注 6）· 分层隔离（约束 4）· 解析器中立（口径 12） | 技术方案 §5.3；编程指南 §4.6「三档载体接口」 |
| `import_test.go`（**包级架构测试**） | **生产导入面**只有 `core` / `workspace` / 标准库（口径 26 / FR-038）——包级架构测试承载，**不得**往全局不变量清单加全仓 `grep` 判据；test-only 引 `internal/storage` 不在判据面内 | 技术方案 §10.1 三条包纪律；口径 26；`cli/ports_test.go` 先例 |
| `storage/memory_store_test.go` | 六列读写与 `created_at`（§3.4）· **单事务配额**（约束 12）· `instr` + `id` 升序（约束 9）· `IS NULL` 全局层（§3.4）· 失败不留半行（约束 12）· 渲染可解析（口径 12） | 契约 `schema.sql`；`run-and-errors` §7.4 |
| `tool/memorytools_test.go` | schema 契约（口径 9）· 默认分区（契约 §5.4）· 非法取值（约束 20）· 空输入零写入零检索（约束 20 / **R8**）· 未命中成功（约束 10）· 配额可读拒绝（约束 16）· 确认文案（口径 11） | 契约 §5.4；`run-and-errors` §7.3 |
| `core/prompt_test.go`（追加） | 段 2 拼接形态（约束 22）· **空记忆逐字节回归** | 技术方案 §4.2 / §4.7；`prompt.go` 既有注释 |
| `core/service_test.go`（追加） | 读取失败拒绝启动（约束 23）· nil 端口容错（约束 22）· **运行内冻结**（约束 5） | 技术方案 §5.1 契约 1 / §11.5；`service.go` ②③ 既有形状 |
| `workspace/*_test.go`（追加） | 模板默认工具（约束 26）· 路径约定（约束 15）· 池内注册（宪法 IV 连带） | 需求 §10.2；编程指南陷阱 #20 |
| `cli/config_test.go`（追加） | 缺省 / 取值域 / fail-closed（约束 18） | 技术方案 §3；Compatibility §4 #2 |
| `app/wire_test.go`（追加） | 段 2 到模型入站（约束 22 端到端）· **Demo 二闭环**（两档）· 载体隔离的装配面（约束 14）· 池 6 → 8 | 技术方案 §13 US-6；需求 §9.3 |
| `tool/memorytools_test.go`（**R7 / R8** 同文件，`package tool_test`） | 载体隔离负向（约束 14 / **R7**：**规则名与指引逐字**断言 + **原文件逐字节不变** + 记忆工具正向对照）· 空输入零写入零检索（约束 20 / **R8**） | 技术方案 §5.1 契约 5；`policy.go` 的 `ruleMemoryNotWritable` / `guidanceMemoryNotWritable`；`run-and-errors` §7.3 |

### 4.3 关键回归测试写出代码

以下 R1~R9 共 **10 条函数**（R4 跨两包，拆 R4a / R4b）——**必须**成为独立任务且原样落地；每条标注**逆改点**（实现怎么改它会红）——写不出逆改点的用例按"对号入座但无判别力"处理。夹具按 US-4/US-5 既有形状（`loopHarness` / `fakeProvider` / `spyTool` / `fakeAgentSource` / `policyFixture` / `fileToolFixture`）。

**本 US 新增 / 扩展的测试辅助件**（落在 §3.2 对应测试文件内；下方 R1~R9 直接引用，不再重复定义）：

| 辅助件 | 落位 | 职责 |
| --- | --- | --- |
| `fakeMemoryReader` | `core/loop_test.go` | 静态返回一段文本的 `core.MemoryReader` 替身（`{text string}`；R1 / R4b） |
| `scriptedMemoryReader` | `core/loop_test.go` | 逐次返回不同文本（`{texts []string}`）并按次计数（`readCount()`）的替身——第 2 次读出「新偏好」即暴露"每轮重读"（R2） |
| `withMemory(r core.MemoryReader) harnessOption` | `core/loop_test.go` | 把记忆端口**在构造期**置入 `core.ServiceOptions{Memory: r}` 的函数式选项（`newLoopHarness(t, opts ...harnessOption)`）。**构造顺序是硬约束**：`newLoopHarness` 内部**就**构造好了 `AgentService`，构造之后再写 `h.memory` 对已建好的 service **无效**（端口在构造期定格）——两条注入用例（R1 / R4b）**必须**经 `withMemory` 传入，否则"有记忆"那组实际跑的是空端口、断言恒绿（**实现错它也不红**） |
| `carriersForTest(t)` | `memory/carrier_contract_test.go` | 返回两档载体用例：`{name: "markdown", store: NewMarkdownStore(root)}` 与 `{name: "sqlite", store: storage.NewMemoryStore(db, now)}`——两档跑同一断言的唯一入口（R3 / R5）。**注意**：该文件是 `package memory_test`（外部测试包），拿不到 `internal/storage` 的内测夹具，须用**导出面**自建真库：`storage.OpenWritable(ctx, filepath.Join(t.TempDir(), "basalt.db"), storage.SupportedSchemaVersion)` + `t.Cleanup(db.Close)` |
| `archiveEntriesForTest(t, text)` | `memory/carrier_contract_test.go` | 按**约束 28 的文档化形态**提取归档区条目的测试侧提取器（`package memory_test` 拿不到未导出的 `parseSections`，且**不得**引用实现常量——形态漂移时它先红，而不是跟着实现一起漂）；提取规则之外的任何行 → `require.Failf` 直接失败（R5） |
| `captureLogger(t)` / `captureWriter.logs(t)` | `memory/memory_test.go` | 写入内存缓冲的 `*slog.Logger` 与取文本的方法（R4a 判 WARN 半轨） |
| `errorStore{err}` | `memory/memory_test.go` | `Load` 恒返回注入错误的 `core.LongTermMemoryStore` 桩——真文件造权限错误无法跨平台复现，I/O 档用桩注入（**与结构性损坏档分开**：后者用真 markdown 文件造；R9） |
| `blockingStore{entered, release}` | `memory/memory_test.go` | **闸门式** `core.LongTermMemoryStore` 替身：`Append` 一进入就把本次 `core.Scope` 发到 `entered`，然后**阻塞**到 `release` 收到一个值才返回（`Load` / `RecallByKeyword` 恒空）。判「锁按完整 Scope 分档」的判别力来源——`TestService_AppendLocksAreKeyedByScope` 用它证明 ① 同 Scope 第二笔在首笔放行前不进入（串行）、② `alice` 阻塞期间 `bob` 能进入（不同 Agent 不互相阻塞）。**逆改点**：把分档 map 换成一把大锁 → ② 等不到 `bob` 的进入信号（超时红） |
| `openTempDB(t)` | `storage/llm_calls_test.go:41`（**既有，直接复用**） | `OpenWritable(ctx, t.TempDir()/basalt.db, SupportedSchemaVersion)` + `t.Cleanup(Close)` 的 `*storage.DB`——`storage/memory_store_test.go`（内测包）与 `storage.NewMemoryStore` 同包同型，不再新造夹具 |
| `testLogger(t)` | `memory` 测试包（沿 `storage/recovery_test.go:279` 的同名约定） | 丢弃级 `*slog.Logger`（本仓按包各自定义，不跨包共享）；R4a 用 `captureLogger` 替它——**只有判 WARN 的那条需要留痕** |
| `countingStore` | `tool/memorytools_test.go` | 记录 `Append` / `RecallByKeyword` 调用次数的 `core.LongTermMemoryStore` 替身（R8 判"零写入 / 零检索"） |
| `toolEnvWithAgent(name)` | `tool/memorytools_test.go` | 构造带 Agent 上下文的 `core.ToolEnv`（`AgentName` 置位——记忆工具的作用域只由此取值，沿 US-5 `fileToolFixture.toolEnv()` / `httpToolEnv()` 的"显式环境"形状）；R7 的**文件工具**一侧不新造环境，直接用 `newFileToolFixture(t, nil)` 的 `f.toolEnv()` |

**测试包归属**（判据面事实，改动前先核）：`core/loop_test.go` 与 `core/service_test.go` 是 `package core_test`；`internal/tool/{filetools,policy,registry,runner}_test.go` 是 `package tool_test`——`tool/memorytools_test.go` 落在 **`package tool_test`**（与已持有 `memory/` 拒绝用例的 `policy_test.go` 同包同型，`cli/ports_test.go` 的 import 面判据只看非测试导入，不受影响）。

**并发用例的纪律**：`require` 一律在**主 goroutine**（Wait 之后）断言——子 goroutine 里只收集 error（`require` 内部的 `FailNow` 要求调用方是测试 goroutine，在子 goroutine 里调用行为未定义）。

#### R1 — 段 2 注入与空记忆逐字节回归（core 层）

```go
// R1 —— 段 2 注入 + 空记忆逐字节回归（core 层）。
// 逆改点：Build 给 System 加任何固定前后缀（哪怕一个换行）→ 第三组断言红；
// 把段 2 塞进 History / Tools 而不是 System → 第一组断言红。
func TestAgentService_InjectsCoreMemorySegment(t *testing.T) {
    const coreText = "## 核心记忆\n\n- 用户偏好简洁回答"

    // 基线：不接记忆端口（nil）——US-4 的既有形状。
    base := newLoopHarness(t)
    base.provider.onCall(0, textResponse("好"))
    _, err := base.service.Run(ctx, base.req("你好"))
    require.NoError(t, err)
    baseline := base.provider.request(0).Messages[0].Content

    // 有记忆：段一 + 两换行 + 核心区文本（端口**只能在构造期**经 withMemory 置入）。
    mem := newLoopHarness(t, withMemory(fakeMemoryReader{text: coreText}))
    mem.provider.onCall(0, textResponse("好"))
    _, err = mem.service.Run(ctx, mem.req("你好"))
    require.NoError(t, err)
    require.Equal(t, baseline+"\n\n"+coreText, mem.provider.request(0).Messages[0].Content)

    // 空记忆：与基线**逐字节**相同（段 2 不产生任何文本）。
    empty := newLoopHarness(t, withMemory(fakeMemoryReader{text: ""}))
    empty.provider.onCall(0, textResponse("好"))
    _, err = empty.service.Run(ctx, empty.req("你好"))
    require.NoError(t, err)
    require.Equal(t, baseline, empty.provider.request(0).Messages[0].Content,
        "空记忆不得在系统提示词里留下任何痕迹")
}

```

#### R2 — 运行内冻结：整次运行只读一次记忆（core 层）

```go
// R2 —— 运行内冻结：整次运行只读一次记忆，第 2 轮不得看到"改后"文本（core 层）。
// 逆改点：把读取挪进每轮组装（Build 内重读）→ 第 2 轮入站 system 变成"- 新偏好"，断言红。
func TestAgentService_MemoryReadOncePerRun(t *testing.T) {
    reader := &scriptedMemoryReader{texts: []string{"- 旧偏好", "- 新偏好"}}
    h := newLoopHarness(t, withMemory(reader)) // 端口在构造期置入
    h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"AGENT.md"}`))
    h.provider.onCall(1, textResponse("好"))

    _, err := h.service.Run(ctx, h.req("你好"))
    require.NoError(t, err)
    require.Equal(t, 1, reader.readCount(), "每次运行只读一次（运行内冻结）")
    for i := 0; i < 2; i++ {
        require.Contains(t, h.provider.request(i).Messages[0].Content, "- 旧偏好")
        require.NotContains(t, h.provider.request(i).Messages[0].Content, "- 新偏好")
    }
}

```

#### R3 — 两层回退且不叠加（memory 层，两档同一断言）

```go
// R3 —— 两层回退且不叠加（memory 层，两档同一断言）。
// 逆改点：把两层内容拼接（叠加）→ ② 的"全局偏好"与"alice 偏好"同时出现，断言红；
// 生效层判据写成"**核心区**非空"（而非契约 §5.2 字面的"层非空"）→ ③ 里 bob 的层**只写了归档**
// 时判据会去回退全局层核心区、返回非空文本，断言红——这一步就是口径 2 的逆改点
// （原 R3 缺它：只有"层非空"与"核心区非空"**分叉**的场景才判得出这个差别）。
func TestService_LoadEffective_TwoLayerFallbackWithoutStacking(t *testing.T) {
    for _, c := range carriersForTest(t) { // markdown（t.TempDir 根）/ sqlite（storage.MemoryStore + 真实库文件）
        t.Run(c.name, func(t *testing.T) {
            svc := memory.NewService(memory.Options{Backend: c.store, Logger: testLogger(t)})
            agentScope := core.Scope{Agent: "alice"}

            // ① 全局层有内容、Agent 层空 → 生效层 = 全局层。
            require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "- 全局偏好"))
            got, err := svc.LoadEffective(ctx, "alice")
            require.NoError(t, err)
            require.Contains(t, got, "- 全局偏好")

            // ② Agent 层有核心条目 → 只注 Agent 层，全局层不叠加。
            require.NoError(t, svc.Append(ctx, agentScope, core.SectionCore, "- alice 偏好"))
            got, err = svc.LoadEffective(ctx, "alice")
            require.NoError(t, err)
            require.Contains(t, got, "- alice 偏好")
            require.NotContains(t, got, "- 全局偏好", "两层不叠加")

            // ③ 另一个 Agent 的层里**只有归档条目**：层非空 → 生效层仍是它自己这一层；
            //    核心区切出为空（段 2 不产生文本），**不得**因此回退去注全局层核心区。
            require.NoError(t, svc.Append(ctx, core.Scope{Agent: "bob"}, core.SectionArchive, "- bob 归档条目"))
            got, err = svc.LoadEffective(ctx, "bob")
            require.NoError(t, err)
            require.Empty(t, got, "Agent 层只有归档区 → 段 2 空（层非空即生效层，不回退全局层核心区）")

            // ④ 归档区不进注入面（口径 1）：alice 的归档条目同样不出现。
            require.NoError(t, svc.Append(ctx, agentScope, core.SectionArchive, "- alice 归档条目"))
            got, err = svc.LoadEffective(ctx, "alice")
            require.NoError(t, err)
            require.NotContains(t, got, "- alice 归档条目", "归档区不进系统提示词")

            // ⑤ 无 Agent 上下文（空串）= 全局层，是正常取值不是异常。
            got, err = svc.LoadEffective(ctx, "")
            require.NoError(t, err)
            require.Contains(t, got, "- 全局偏好")
        })
    }
}

```

#### R4a — 手工编辑超配额：注入侧不截断 + WARN（memory 层）

R4 **跨两个包**——门面侧与循环侧无法同函数断言（`newLoopHarness` 是 `package core_test` 的未导出夹具，跨目录用不了），故**落成两条**（R4a / R4b，各为独立任务）：R4a 在 `memory/memory_test.go`，R4b 在 `core/service_test.go`。

```go
// R4a —— 手工编辑超配额：**完整**注入 + WARN（memory 层，可观测性双轨的日志半轨）。
// 逆改点：注入期静默截断（"只取前 N 字节"）→ 与全量期望文本的相等断言红——**末尾哨兵**最先消失，
// 断言失败信息直接指到截断点；删掉超限判定 → 日志里没有 WARN，断言红。
// 判据形态（P1-3 修正）：不比 Contains（"条目在不在"在截断实现下照样能过），
// 而是**逐字节等于完整期望文本** + 尾部哨兵——截断、丢条、多拼接都会红。
func TestOversizedManualMemory_WarnsAndInjectsFully(t *testing.T) {
    root := t.TempDir()
    store := memory.NewMarkdownStore(root)
    svc := memory.NewService(memory.Options{Backend: store, Logger: captureLogger(t)})

    // 3000 条 24 字节条目 + 末尾哨兵 ≈ 72 KB > 32768：手工编辑不归工具配额管。
    entries := make([]string, 0, 3001)
    for i := 0; i < 3000; i++ {
        entries = append(entries, "- 很长的一条偏好")
    }
    entries = append(entries, "- 末尾哨兵：本条必须在注入结果里出现")

    // 人手写的文件 = 载体重建形态（零缩进列表项 + 条目间空行，约束 28）。
    manual := "# MEMORY.md —— 长期记忆 · 全局层\n\n## 核心记忆\n\n" +
        strings.Join(entries, "\n\n") + "\n\n## 归档记忆\n"
    require.NoError(t, os.WriteFile(filepath.Join(root, "memory", "MEMORY.md"), []byte(manual), 0o600))

    got, err := svc.LoadEffective(ctx, "")
    require.NoError(t, err)
    require.Equal(t, "## 核心记忆\n\n"+strings.Join(entries, "\n\n"), got,
        "注入面 = 核心区全文（不截断、不含文件标题、不含归档区）")

    logs := captureWriter.logs(t)
    require.Contains(t, logs, "WARN")
    require.Contains(t, logs, "超") // 日志点名超配额事实；**不得**打印记忆正文
    require.NotContains(t, logs, "- 很长的一条偏好", "日志不打记忆正文")
    require.NotContains(t, logs, "末尾哨兵", "日志不打记忆正文")
}
```

#### R4b — 超上下文预算拒绝启动、零 LLM（core 层）

```go
// R4b —— 注入后超上下文预算 → 拒绝启动、Reason 点名预算键、零 LLM（core 层）。
// 预算与记忆两端**都取小值**（P1-2 修正）：旧值 `Repeat("- 偏好\n", 3000)` 只有 27 KB、
// 估不出 32768 的缺省预算——用例恒绿、实现再错也不红，等于没判。现在把预算压到 8192
// （可裁面 = 8192 - core.ReserveTokens 1024 = 7168），并加**对照组**：同一预算、不接记忆必须放行
// ——"闸门是不是真的接在记忆上"由此钉死，而不是靠一个可能本来就超限的预算蒙对。
// 逆改点：把超限改成"截断后继续跑" → 实验组 Status 非 failed 且 provider 被调用，两条断言皆红；
// 把记忆漏接进 Build（`env.memory` 没喂给段 2）→ 实验组反而跑通，同样红。
func TestAgentService_OversizedMemoryFailsRunBeforeModelCall(t *testing.T) {
    const contextTokens = 8192

    // 对照组：同一预算、不接记忆端口（nil）→ 正常跑完（本预算本身不构成超限）。
    control := newLoopHarness(t)
    control.budget.ContextTokens = contextTokens
    control.provider.onCall(0, textResponse("好"))
    res, err := control.service.Run(ctx, control.req("你好"))
    require.NoError(t, err)
    require.Equal(t, core.StatusCompleted, res.Status, "对照组：同预算、无记忆必须放行")
    require.Equal(t, 1, control.provider.callCount())

    // 实验组：同一预算 + 90000 字节记忆（estimateText 下 ≥ 22000 tokens，见 prompt.go）→
    // 拒绝启动、Reason 点名预算键、零模型调用。
    over := newLoopHarness(t, withMemory(fakeMemoryReader{text: strings.Repeat("- 偏好\n", 10000)}))
    over.budget.ContextTokens = contextTokens
    res, err = over.service.Run(ctx, over.req("你好"))
    require.NoError(t, err, "运行期失败归 Status，不是 Go error")
    require.Equal(t, core.StatusFailed, res.Status)
    require.Contains(t, res.Reason, "budget.context_tokens", "原因必须点名上下文预算键")
    require.Zero(t, over.provider.callCount(), "拒绝启动 = 零模型调用")
}
```

#### R5 — 并发写同一作用域一条不丢（memory 层，两档，`-race`）

```go
// R5 —— 并发写同一作用域：一条不丢、不互相覆盖（memory 层，两档，-race）。
// 逆改点：去掉门面 per-scope 锁 → markdown 档"读-改-写"交错丢条目，条数断言红。
// 判据形态（P1-3 修正）：**不得**用 `Contains(loaded, "条目 1")`——"条目 10"能替它过；
// 探针实测：只落 15 条时，16 条 Contains 断言仍全绿。改为按约束 28 的形态提取条目、
// 逐条比对**多重集**（ElementsMatch）：丢条、重复、条目串被截半都会红。
func TestCarriers_ConcurrentAppendKeepsEveryEntry(t *testing.T) {
    for _, c := range carriersForTest(t) {
        t.Run(c.name, func(t *testing.T) {
            svc := memory.NewService(memory.Options{Backend: c.store, Logger: testLogger(t)})
            const n = 16
            errs := make([]error, n)
            var wg sync.WaitGroup
            for i := 0; i < n; i++ {
                wg.Add(1)
                go func(i int) {
                    defer wg.Done()
                    errs[i] = svc.Append(ctx, core.GlobalScope, core.SectionArchive,
                        fmt.Sprintf("条目 %d", i))
                }(i)
            }
            wg.Wait()
            for i, err := range errs {
                require.NoError(t, err, "第 %d 笔并发追加", i) // 断言一律回主 goroutine
            }
            loaded, err := svc.Load(ctx, core.GlobalScope)
            require.NoError(t, err)

            want := make([]string, 0, n)
            for i := 0; i < n; i++ {
                want = append(want, fmt.Sprintf("条目 %d", i))
            }
            got := archiveEntriesForTest(t, loaded)
            require.Len(t, got, n, "并发追加不得丢条目（也不得多出条目）")
            require.ElementsMatch(t, want, got, "16 条逐字齐全且两两不同（多重集比较：重复即红）")
        })
    }
}
```

`archiveEntriesForTest` 是**测试侧**的形态提取器（落在 `carrier_contract_test.go`；`package memory_test` 拿不到未导出的 `parseSections`，也**不得**引用实现常量——形态漂移时它先红，而不是跟着实现一起漂）：

```go
// archiveEntriesForTest 按约束 28 的**文档化形态**从载体文本里取归档区条目：结构行只有
// 零缩进的文件标题与分区标题；条目 = 零缩进 `"- "` 起行（本用例的条目都是单行，缩进续行
// 归入上一条的正文、不单独成条）。这三类之外的任何行 → 直接失败（形态漂移必须可见）。
// The documented shape only; anything else fails rather than being swallowed silently.
func archiveEntriesForTest(t *testing.T, text string) []string {
    t.Helper()
    var entries []string
    inArchive := false
    for _, line := range strings.Split(text, "\n") {
        switch {
        case strings.HasPrefix(line, "## "): // 分区标题（含其后的正文归属切换）
            inArchive = line == "## 归档记忆"
        case strings.HasPrefix(line, "# "): // 文件标题
            inArchive = false
        case line == "" || strings.HasPrefix(line, "  "): // 条目间空行 / 缩进续行
        case inArchive && strings.HasPrefix(line, "- "):
            entries = append(entries, strings.TrimPrefix(line, "- "))
        default:
            require.Failf(t, "载体形态漂移", "行 %q 既不是结构行也不是条目行", line)
        }
    }
    return entries
}

```

#### R6 — sqlite 档配额检查与追加同事务（storage 层）

```go
// R6 —— sqlite 档：配额检查与追加同事务，并发也不得穿透上限（storage 层）。
// 逆改点：把 SUM(octet_length) 查询挪到 withWriteLock 事务之外 → 两笔同时通过检查 → 总量超限，断言红。
func TestMemoryStore_ConcurrentAppendNeverExceedsCoreQuota(t *testing.T) {
    db := openTempDB(t) // 既有夹具（llm_calls_test.go:41）：真库 + 全量迁移 + Cleanup 关闭
    store := NewMemoryStore(db, func() time.Time { return time.Unix(0, 0) }) // 内测包：同包直呼

    // 单条 20000 字节 > MaxCoreEntryBytes（4096）：单条判据先于总量判据，Kind = entry。
    var quota *core.MemoryQuotaError
    err := store.Append(ctx, core.GlobalScope, core.SectionCore, strings.Repeat("a", 20000))
    require.ErrorAs(t, err, &quota)
    require.Equal(t, core.QuotaKindEntry, quota.Kind)

    // 边界用 4096 字节条目：上限 32768 / 4096 = 恰 8 笔可成功；并发 12 笔里必须恰好 8 笔通过。
    // （若"求和"在事务之外，两笔并发会各查各的都在限内 → 双双落盘 → 穿透上限。）
    entry := strings.Repeat("b", 4096) // 恰在上限内（口径 6：恰在上限 == 放行）
    errs := make([]error, 12)
    var wg sync.WaitGroup
    for i := 0; i < 12; i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            errs[i] = store.Append(ctx, core.GlobalScope, core.SectionCore, entry)
        }(i)
    }
    wg.Wait()

    succeeded := 0
    for i, err := range errs {
        if err == nil {
            succeeded++
            continue
        }
        require.ErrorAs(t, err, &quota, "第 %d 笔应被总量判据拒绝", i)
        require.Equal(t, core.QuotaKindSection, quota.Kind)
    }
    require.Equal(t, 8, succeeded, "并发 12 笔里只允许恰好 8 笔落盘，多一笔即穿透上限")
    // 到顶之后 1 字节都进不去——总额恰为 32768，未被任何并发穿透。
    require.ErrorAs(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "x"), &quota)
}

```

#### R7 — 载体隔离负向：文件工具拒写 `memory/`、记忆工具可写（tool 层）

`memory/` 的**规则级**拒绝面在 US-4 已由 `policy_test.go` 的 F9（`requireRefused(..., "记忆目录不在可写范围")`，两条 memory 路径）钉住；R7 多钉三件 F9 钉不住的事：**工具侧**确实走到那条规则（而不是被通用越界规则拦下）、拒绝后**文件逐字节不变**、以及**正向对照**（记忆工具在同一个根上写成功）。

```go
// R7 —— 载体隔离负向：文件工具写 memory/ 被拒、记忆工具可写（tool 层，真策略 Fixture）。
// 拒绝面的两条字面量**独立抄一份**（沿 filetools_test.go 的 fileReadLimit / wantWriteFileSchema 先例）：
// policy.go 只导出 NewWhitelistPolicy，测试侧引用实现常量会让两边一起漂、钉不住任何东西。
const (
    memoryNotWritableRule     = "记忆目录不在可写范围"
    memoryNotWritableGuidance = "记忆的写入归记忆工具，文件工具不改写 memory/（代码常量），没有配置键可以加。"
)

// TestMemoryDirIsolation 覆盖契约 §5.1 的载体隔离：文件工具够不到 memory/，记忆工具够得到。
// 逆改点：① 把 memory 分支从 checkFileWrite 里删掉 → 工具侧落回通用规则（ruleWriteOutside），
// 规则名断言红——**这是本用例唯一的判别力来源**：只判 `IsSandboxViolation` 时，
// 删掉 memory 分支它照样绿（探针实测：agent 上下文 + 相对 `memory/…` 的入参解析到
// `agents/<name>/memory/…`，通用规则会顶上来，文案却完全不提记忆）；
// ② 给记忆工具也接上文件工具的那条路径 → 正向对照红。
func TestMemoryDirIsolation(t *testing.T) {
    f := newFileToolFixture(t, nil) // 最小工作区骨架（含 memory/MEMORY.md 占位文件）+ 真策略
    before, err := os.ReadFile(f.path(filepath.Join("memory", "MEMORY.md")))
    require.NoError(t, err)

    // ① 相对 Agent 自有目录**爬出去**打到 memory/：解析后落在 `<root>/memory/…`，
    //    因此判的必须正是记忆目录那条规则（相对入参 `memory/…` 会落在自有目录里
    //    `agents/<name>/memory/…` → 走通用规则，判不出记忆分支）。
    for _, tc := range []struct{ target, wantTarget string }{
        {filepath.Join("..", "..", "memory", "MEMORY.md"), filepath.Join("memory", "MEMORY.md")},
        {filepath.Join("..", "..", "memory", "agents", policyAgent, "MEMORY.md"),
            filepath.Join("memory", "agents", policyAgent, "MEMORY.md")},
    } {
        _, err := runTool(t, f.write, f.toolEnv(),
            `{"path":`+strconv.Quote(tc.target)+`,"content":"越界写入"}`)
        var violation *core.SandboxViolationError
        require.ErrorAs(t, err, &violation, "文件工具必须拒写 memory/：%s", tc.target)
        require.Equal(t, memoryNotWritableRule, violation.Rule, "拒绝必须出自记忆目录规则：%s", tc.target)
        require.Equal(t, memoryNotWritableGuidance, violation.Guidance, "指引逐字固定：%s", tc.target)
        require.Equal(t, tc.wantTarget, violation.Target,
            "Target 给工作区相对形态（拒绝文案不回显绝对路径）")
    }

    // ② 拒绝之后原文件**逐字节不变**（拒绝不是"清空重写"，也不是"改完再报错"）。
    after, err := os.ReadFile(f.path(filepath.Join("memory", "MEMORY.md")))
    require.NoError(t, err)
    require.Equal(t, string(before), string(after), "被拒的写入不得改动原文件一个字节")

    // ③ 正向对照：同一 root、同一 Agent 上下文下，记忆工具写入成功——隔离是"通道分离"，
    //    不是"记忆不可写"（否则 ① 用"所有 memory/ 写入都拒"的错实现也能过）。
    store := memory.NewMarkdownStore(f.root)
    _, err = tool.NewSaveMemoryTool(store).Execute(ctx, toolEnvWithAgent(policyAgent),
        json.RawMessage(`{"content":"用户偏好简洁回答","scope":"CORE"}`))
    require.NoError(t, err)
    written, err := os.ReadFile(f.path(filepath.Join("memory", "agents", policyAgent, "MEMORY.md")))
    require.NoError(t, err)
    require.Contains(t, string(written), "用户偏好简洁回答", "记忆工具必须真的写进去")
}

```

#### R8 — 空 `query` / 空 `content` 在工具层即拒（tool 层）

```go
// R8 —— 空 query / 空 content 在工具层即拒（零写入、零检索，`package tool_test`）。
// 判据面 = **规范化（TrimSpace）后为空**：空串与纯空白同档（约束 20 / 29，P1-b 裁决）。
// 逆改点：① 删掉 check 里的空值分支 → sqlite 档 instr(content,'') 命中全部 →
//   "空词召回倒出整个归档区"；② 判据只写 `args.Content == ""` 而不先 TrimSpace →
//   纯空白两态（P1-b 新增）红。
func TestMemoryTools_RejectEmptyInputs(t *testing.T) {
    store := &countingStore{} // 记录 Append / RecallByKeyword 调用次数的替身（外部测试包内自建）
    save := tool.NewSaveMemoryTool(store)
    recall := tool.NewRecallMemoryTool(store)

    // 空串与纯空白四态：规范化后都为空 → 全部必须拒（只判 `== ""` 时后两条红）。
    for _, in := range []string{`{"content":""}`, `{"content":"   \n\t "}`} {
        _, err := save.Execute(ctx, toolEnvWithAgent("alice"), json.RawMessage(in))
        require.Error(t, err, "规范化后为空的 content 必须拒：%s", in)
        require.True(t, core.IsInvalidInput(err))
    }
    for _, in := range []string{`{"query":""}`, `{"query":" "}`} {
        _, err := recall.Execute(ctx, toolEnvWithAgent("alice"), json.RawMessage(in))
        require.Error(t, err, "规范化后为空的 query 必须拒：%s", in)
        require.True(t, core.IsInvalidInput(err))
    }

    require.Zero(t, store.appends, "规范化后为空 → 零写入")
    require.Zero(t, store.recalls, "规范化后为空 → 零检索")
}
```

#### R9 — 读取面三档不得塌成一档（memory 层）

P1-1 裁决的判别面：**缺失 / 合法空层**（正常取值，回退照常）· **I/O 失败**（上抛 → 注入面拒绝启动）· **非空但结构损坏**（拒绝使用、拒绝覆盖、原文件逐字节不变）。三档的共同逆改点就是 `text, _ := root.ReadFile(rel)` 或 `if ok {…}` 那类写法——它把三档一次抹平成"空层"。

```go
// R9 —— 读取面三档不得塌成一档（memory 层，无 Agent 上下文侧）。
// 逆改点：把 `text, _ := root.ReadFile(rel)`（或 `if ok {…}`）写回来 → ② 拿到 nil error
// 并回退全局层、③ 拿到 nil error 且把损坏文件当空层覆盖，三条断言皆红。
// 判据面分档：损坏形态用**真 markdown 文件**造（`parseSections` 的三条结构事实靠它判）；
// I/O 失败用**桩载体**注入（真文件造权限错误无法跨平台复现）。
func TestService_LoadEffective_ErrorTiersDoNotCollapse(t *testing.T) {
    root := t.TempDir()
    store := memory.NewMarkdownStore(root)
    svc := memory.NewService(memory.Options{Backend: store, Logger: testLogger(t)})
    require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "- 全局偏好"))

    // ① 文件不存在 = 空层（**不是**失败）：生效层回退到全局层核心区。
    got, err := svc.LoadEffective(ctx, "alice") // memory/agents/alice/MEMORY.md 尚不存在
    require.NoError(t, err, "文件不存在是正常取值，不是读取失败")
    require.Contains(t, got, "- 全局偏好", "缺失档照常回退全局层")

    // ② I/O 失败 = 读取失败：上抛，**不得**回退（注入面据此拒绝启动，约束 23）。
    failing := memory.NewService(memory.Options{Backend: errorStore{err: fs.ErrPermission}, Logger: testLogger(t)})
    got, err = failing.LoadEffective(ctx, "alice")
    require.Error(t, err, "I/O 失败必须上抛（不得当空层回退继续跑）")
    require.Empty(t, got, "失败时不得返回任何可注入文本")

    // ③ 非空但结构损坏（缺 `## 归档记忆`）：拒绝使用、拒绝覆盖、原文件逐字节不变。
    manual := "# MEMORY.md —— 长期记忆 · 全局层\n\n## 核心记忆\n\n- 手写的偏好\n"
    file := filepath.Join(root, "memory", "agents", "alice", "MEMORY.md")
    require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o750))
    require.NoError(t, os.WriteFile(file, []byte(manual), 0o600))

    got, err = svc.LoadEffective(ctx, "alice")
    require.Error(t, err, "结构损坏拒绝使用（不得当空层回退去注全局层）")
    require.Empty(t, got)
    require.Contains(t, err.Error(), "归档记忆", "错误文案点名声称与症状（缺哪个标题）")

    require.Error(t, svc.Append(ctx, core.Scope{Agent: "alice"}, core.SectionCore, "- 新条目"),
        "损坏文件拒绝覆盖：重建文件会静默删掉人手写的内容")
    after, readErr := os.ReadFile(file)
    require.NoError(t, readErr)
    require.Equal(t, manual, string(after), "被拒之后原文件逐字节不变")
}
```

**三档的补充分布**（同一判别面，落点不同，不重复）：缺分区标题 / 标题重复 / 次序颠倒三种畸形形态的**解析器级**判据在 `markdown_test.go` 的 `TestMarkdownStore_ParseToleratesExtraHeadingsAndRejectsBrokenShape` 与 `TestMarkdownStore_AppendRefusesCorruptedFileKeepsOriginalBytes`；"合法扩展不算损坏"（人手加 `### 小节`、裸段落）同在其中。

## 5. 范围边界

### 5.1 核心阶段不做项（依据：需求文档 §6 / 技术方案"核心阶段不做"）

| 不做项 | 依据 |
| --- | --- |
| 向量检索 / 语义召回 / 知识库 | 技术方案 §5.1 契约 4（召回是关键词检索）；扩展阶段 |
| `mem0` 载体档 | Compatibility §4 #2（延后）；U-8 裁决前不实现——本 US 取值域外即**拒绝启动** |
| 多租户记忆隔离 | 企业级治理归扩展阶段（宪法 X） |
| 记忆写入审核 / 策略引擎 / 敏感信息过滤 | 扩展阶段；本 US 的写入面只有配额一道 |

### 5.2 本 US 不做（归后续）

| 不做项 | 归属 |
| --- | --- |
| `notify` / `manage_schedule` 工具 | US-8 / US-10（占位见 §3.5） |
| MCP 工具与技能加载 | US-7（记忆与技能**无绑定关系**——记忆是内置工具池成员） |
| 记忆管理端点（`GET /api/v1/memory`）与界面 | US-9 / US-12 |
| 归档区裁剪 / 压缩 / 迁移 | 扩展阶段；承诺是**只追加、删除留缺口**（Compatibility §2.4 #41） |
| 归档区的容量边界与检索性能保证 | **归档区无配额**（口径 6：只有核心区有 32 KB 上限）·**每次运行整层 `Load` 一次**（口径 4：运行内冻结，不做增量/分页读）。由此派生的已知边界：归档区可以长到任意大，单次运行的记忆读取开销与归档区体量同阶（markdown 档是整文件读，sqlite 档是整层 `SELECT`）。**本 US 不设上限、不做索引优化、不做分页**——批量裁剪与检索加速归扩展阶段的记忆治理；在此之前该边界写进运维熟悉的口径（[Operations](../Operations.md) 的备份/容量节 + 本文 §6.1 人工项 13），**不得**当作"已解决" |
| `USER.md` 注入面调整 | 已在 US-4 的段 1（§2.4 注 5），本 US 不移动 |
| 记忆配额做成配置键 | 本 US **零新增配置键**（配额是代码常量） |
| Agent 删除时清理记忆 | 技术方案 §5.2（存量不迁移不删除），本 US 不改 |

## 6. 验证与验收

### 6.1 剩余人工项

1. **门禁七目标与覆盖率读数**：`make check` 七个目标逐条读数（`fmt` / `lint` / `sec` / `test` / `test-integration` / `cover` / `build`）；`memory` 包覆盖率 ≥ 70% 的实测数值写入 DoD。
2. **Demo 二（跨对话记偏好）**：真模型下——会话 A 说「以后回答简洁点」→ Agent 调 `save_memory`，**且必须落进核心区**（模型显式带 `scope: "CORE"`；缺省落归档区时**本 Demo 不成立**——归档区不进注入面，第二次运行看不到它）→ 进程重启 → 会话 B（新会话）**两条都要验**：① 观察回答风格；② 检查 `memory/agents/<name>/MEMORY.md` 的 `## 核心记忆` 区**可见该条目**（全局层才在 `memory/MEMORY.md`——Agent 层条目写在 Agent 自己的文件里，路径按 `workspace.MemoryFilePath`）。只验风格不验文件不算通过：风格是主观读数，文件里的条目才是**可复核**的证据。
3. **两档各跑一遍 Demo 二**：`memory.backend: markdown` 与 `sqlite`（改一行配置 + 重启）；sqlite 档核对工作区 `MEMORY.md` **未被改动**（写入落库）。
4. **手工编辑生效时机**：运行中手改 `MEMORY.md` → 本次运行**不变**（快照冻结），下一次运行生效。
5. **手工编辑超配额**：把核心区堆过 32 KB → 日志出现 WARN、注入完整；再把总量推过上下文预算 → 该次运行 `failed` 且 Reason 点名 `budget.context_tokens`。
6. **Windows 真控制台渲染**：中文偏好 + 两分区标题在真控制台与文件里的观感（换行、无 BOM）。
7. **并发抽验**：并发触发多次 `save_memory`（同一作用域）→ 无条目丢失、总量不超限（人工复核 + `-race` 读数）。
8. **变更范围核对**：`git status --short` 与 §3.1 / §3.2 清单逐项对齐；`go.mod` / `go.sum` / `basalt.yaml.example` / `schema.sql` 零 diff。
9. **凭证与敏感面复核**：记忆内容可能含用户数据——确认工具确认文案与错误文案**不回显**内容、日志不打印记忆正文（只打长度与作用域）；仓库内零明文凭证。
10. **`gosec` 与三平台编译**：`make sec` 读数照记；编译面**不能只看 `make build`**（它只编**宿主**一档）——按 CI 的四目标矩阵逐条跑，读数一并入库：

    ```sh
    for target in linux/amd64 linux/arm64 windows/amd64 darwin/arm64; do
      os=${target%/*}; arch=${target#*/}
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
        -ldflags "-s -w -X main.version=us6check" -o bin/basalt-$os-$arch$([ $os = windows ] && echo .exe) ./cmd/basalt
    done
    ```

    只跑 `GOOS=windows GOARCH=amd64 go vet ./...` 这类"单档抽查"**不算**三平台编译（本 US 触到的 `os.Root` / `OpenFile` / `Rename` 都是跨平台 API，风险不高，但判据要按矩阵给）。
11. **`tool list` 冒烟**：池从 6 → 8 后 `basalt tool list` 显示八件（含两件记忆工具）——US-5 的读数基线随之更新。
12. **部署侧复核**：备份面（Operations §3）确认工作区 `memory/` 与库文件两处都在备份范围内；切档说明写进运维熟悉的口径（改键 + 重启）。
13. **归档区体量边界的登记**（§5.2 最后一行）：把一个几百 KB 的归档区灌进某一层，实测"每次运行整层 `Load` 一次"的耗时读数（markdown 整文件读 / sqlite 整层 `SELECT`），把读数与"无配额、无分页"的已知边界写进运维口径——**本 US 不解决它，只把它变成有读数的已知量**，不得在报告里写成"性能已验证达标"（无 SLO 即无"达标"一说）。

### 6.2 本文固定口径（编号冻结，只追加）

| # | 口径 | 依据 / 备注 |
| --- | --- | --- |
| 1 | 段 2 注入面 = 生效层的**核心区**（完整、含 `## 核心记忆` 标题行、不含文档标题）；归档区**不进**系统提示词，只经 `recall_memory` 取回 | agent-md §5.4「核心区不参与检索」+ §4.2「核心记忆区永不裁剪」+ §4.7 不可裁剪列；**已决 #1（2026-10-17）** |
| 2 | 生效层判据 = 该层载体**任一分区正文非空**（标题行与模板说明**不算内容**）；Agent 层有内容 → **不回退、不叠加**；Agent 层只有归档条目时，生效层仍是它、段 2 得空文本 | §5.2 字面；**已决 #2**；R3 第 ③ 步守 |
| 3 | `Prompt.System = 段一正文 + "\n\n" + 核心区文本`；核心区为空 → **不产生任何文本**（与 US-4 逐字节相同） | §4.2 / `prompt.go` 既有注释；R1 守 |
| 4 | 读取时机 = **每次运行开始读一次**、运行内冻结；`ConfigSnapshot` **不含**记忆字段 | §5.1 契约 1 / §11.5 manifest 逐字 |
| 5 | 读取失败 → 拒绝启动（`failed` + 点名载体、零 LLM 调用）——**只限三档里的后两档**（I/O 错误 / 非空损坏）；**文件不存在与合法空层是正常取值**，照常回退、首次写入即创建 | 与段一读取失败同口径；三档切分见约束 27；**已决 #3** |
| 6 | 配额按 **UTF-8 字节**：单条 ≤ 4096（**仅 CORE 写**）、每层核心区总量 ≤ 32768；**求和单位 = 条目正文自身的字节**（口径 23） | §5.1 契约 2；实测 `octet_length` 口径 |
| 7 | 配额判与追加**同原子域**：sqlite 同事务、markdown 门面锁内 | §5.1 契约 6 |
| 8 | 配额拒绝文案——单条：`核心记忆单条超出上限（4096 字节），请压缩内容或改写归档区`；总量：`核心记忆超出总量上限（32768 字节），请改写归档区` | 契约 §5.4「可读拒绝提示改写归档区」；`error_kind` 见口径 18（**已决 #4**） |
| 9 | `scope` 取值：省略 / `null` → `ARCHIVAL`；`CORE` / `ARCHIVAL` **严格区分大小写**；其余 → `invalid_input` | 契约 §5.4 参数表 |
| 10 | 空 `content` / 空 `query` → `invalid_input`，**零写入 / 零检索**；判据 = **规范化（`TrimSpace`）后为空**——纯空白与空串同档（P1-b 裁决；R8 守四态） | 契约 §5.4「单个关键词」+ 实测空针匹配全部 |
| 11 | save 确认 = `已记住（作用域：全局层 或 Agent 层 <name>；分区：核心记忆 或 归档记忆）`（**不回显内容**）；recall 命中 = 条目原文按写入顺序以**空行**分隔；未命中 = `(无匹配条目)` | 契约 §5.4「含作用域」；沿 `(空目录)` 文案先例 |
| 12 | 检索 = 归档区条目级**整串子串**匹配、**区分大小写**、按写入顺序（sqlite 按 `id`）；**不分词、不扩词、不做词形归一**——连续短语（`"用户 偏好"` 逐字连排）**能命中**，分散在条目里的几个词**不会自动组合**成一次命中；`Load` 两档文本**不承诺逐字节相同**（只承诺同解析器同结构） | §5.1 契约 4；实测 `instr` 区分大小写；**语义澄清**见注 6（旧表述"多词短语必不命中"与实现、与本口径均不符，已登记并同步上游四处） |
| 13 | `memory/` 不在文件工具白名单（规则与指引 US-4 已交付、**零改动**）；记忆工具**不持有** `Policy` | §5.1 契约 5；§2.4 注 3 |
| 14 | markdown 写入 = 同目录**临时文件 + rename**；全部读写经 `os.OpenRoot(root)`；Agent 层**按需创建**；失败清理临时文件，且**旧文件逐字节不变**（口径 24） | §5.1 契约 6；`templates.go:13` 注释 |
| 15 | `memory.backend` ∈ {`markdown`（缺省）, `sqlite`}；`mem0` 与未知 → **拒绝启动**点名；**零新增配置键** | Compatibility §4 #2；Operations §2.2 重启口径 |
| 16 | 模板默认工具 = `tools: [save_memory, recall_memory]`；注册池 6 → 8 | 需求 §10.2；编程指南陷阱 #20 |
| 17 | 记忆工具**不落审计、不写消息、不记日志**；失败分类沿用执行器判序；自产错误仅 `*core.InvalidInputError` 与载体 I/O（→ `runtime_error`） | 宪法 V / II；`tool_exec.go` 判序 |
| 18 | 配额拒绝归类 = `invalid_input` | 与「入参非法」同类、判序先于 `runtime_error`；**已决 #4** |
| 19 | 并发口径 = **进程内** per-scope 串行；**锁按完整 `Scope` 分档**（每个 Agent 一把 + 全局层一把——**同 Scope 串行、不同 Agent 不互相阻塞**；按「层」只建两把会把所有 Agent 串成一队，**P2-b 裁决**） | §5.1 契约 6；`run-and-errors` §4.3.1；`TestService_AppendLocksAreKeyedByScope` 守 |
| 20 | `created_at` 由载体**构造器传入的时钟**写入 | 时间显式传入纪律 |
| 21 | markdown 结构损坏 = 三条结构事实之一（缺 `## 核心记忆` / 缺 `## 归档记忆`、同一分区标题出现两次、`## 归档记忆` 早于 `## 核心记忆`）；**合法扩展不算损坏**（多余的 `### 小节`、裸段落、条目正文里的任何行） | 约束 27；缺 / 重复 / 倒序三态的解析器级判据在 `markdown_test.go` |
| 22 | 条目形态 = 零缩进结构行（文件标题恒为第 1 行 + 两个分区标题）+ 零缩进 `"- "` 起行的条目首行 + **缩进两空格**的续行（含空行）；写入与重建**无损可逆**，注入面读的是**重建结果**（文件本身不被改写） | 约束 28；`carrier_contract_test.go` 的条目边界往返用例守 |
| 23 | 条目正文的**规范表示 = `strings.TrimSpace(原文)`，写入入口规范化一次**（markdown 在两条配额检查前 `normalizeEntry`；sqlite 在单条检查与 `INSERT` 前 `strings.TrimSpace`——同一个标准库函数）；**写入 / 单条检查 / 总量求和 / 存储 / 重建 / 召回六面同表示**；配额求和 = **规范化后条目正文自身的 UTF-8 字节**（**含**用户内容内部的空白与换行；首尾空白不计入），**只排除**载体添加物（文件标题 / 分区标题 / `"- "` 前缀 / 续行缩进 / 条目间空行）；两档同一算法 → 8 条恰 4096 字节的条目在两档都恰好到顶，**首尾空白档两档读数也必须一致**（P1-b 裁决——不统一时 markdown 32752 / sqlite 32768，第 9 笔判定分叉） | 约束 29；R6 与 `TestCarriers_QuotaBoundaryExactLimit` 的首尾空白子断言守 |
| 24 | 原子替换失败的不变式 = **目标文件逐字节不变**（"临时文件已清理"是附带结果，**不是**判据）；失败路径 = 清理临时文件后原样上抛（→ `runtime_error`） | 约束 12；R7 的"被拒后原文件不变"与 `markdown_test.go` 的失败态用例守 |
| 25 | **归档区无配额**（只有核心区有 32 KB 上限）+ **每次运行整层 `Load` 一次**：单次运行的记忆读取开销与归档区体量同阶。**已知边界，本 US 不解决**——无分页、无增量读、无索引优化，登记进运维口径（§5.2 末行 + §6.1 人工项 13 的读数） | §5.1 契约 2（配额只覆盖核心区）+ 口径 4（运行内冻结）；扩展阶段的记忆治理 |
| 26 | `internal/memory` 的**生产导入面**只有 `core` / `workspace` / 标准库（test-only 可 import `internal/storage`）；该约束由**包级架构测试**承载（import 面 `go list` 断言，沿 `cli/ports_test.go` 的既有做法）——本 US 的判据载体 = `internal/memory/import_test.go`（1 条，R4 / analyze C1 处置落定，见 §3.2 / §4.2），**不得**因为本 US 往 `module-dev` 的全局不变量清单里新加一条全仓 `grep` 判据 | 注 2；全局不变量清单只收**跨包**的机器可判项，包内导入面归包自己的测试 |

### 评审回合裁决落案表

评审回合：**2026-10-17**（R1 → R2）。共 **10 项裁决**（P1 三条 / P2 三条 / D1~D4）+ **7 处小修**，全部落案；**待决事项清零**。编号不复用，后续评审回合续编。

| 编号 | 裁决 | 落案位置 | 状态 |
| --- | --- | --- | --- |
| **P1-1** | 读取面错误**分三档**、不得用一个 `ok` 抹平：缺失 / 合法空层 = 正常取值（回退照常、首次写入即创建）；其他 I/O = 上抛（注入面拒绝启动、零 LLM）；非空且结构损坏 = 拒绝使用、拒绝覆盖、原文件逐字节不变。伪代码里 `LoadEffective` 的 `s.read(...)` 与 `Append` 的两处 error 线一并补齐（不再吞 `root.ReadFile` 错误、不再吞 `parseSections` 的 `ok`）；原先的"标题破坏 → WARN + 当空层回退"断言**随之废止** | 约束 27 · §2.3 流程 A / 伪代码 · 口径 5 / 21 · §4.3 R9 · §3.2 `memory_test.go` / `markdown_test.go` | 已落案 |
| **P1-2** | R4b 的旧值算不出超限（`"- 偏好\n" × 3000` ≈ 27 KB ≈ 8 k tokens，够不到缺省 `ContextTokens = 32768`）：改为**显式小预算 8192 + 对照组**（同预算、无记忆必须放行），实验组用 90 KB 记忆 | §4.3 R4b · 口径 4 | 已落案 |
| **P1-3** | 四处逆改点补强：**R3** 增「Agent 层**只有归档区**」一步（判据写成"核心区非空"即红）；**R4a** 改**全量期望文本相等** + 尾部哨兵（弃 `Contains`，截断照样能过）；**R5** 改**条目提取 + 多重集比较**（弃 `Contains("条目 1")`——"条目 10"能替它过；探针实测：只落 15 条时 16 条断言全绿）；**R7** 断言**规则名与指引逐字 + 原文件逐字节不变 + 记忆工具正向对照**（弃只判 `IsSandboxViolation`——删掉 memory 分支后通用规则会顶上来，文案却完全不提记忆） | §4.3 R3 / R4a / R5（含新增 `archiveEntriesForTest`）/ R7 · §3.2 用例清单 | 已落案 |
| **P2-4** | markdown 条目边界与配额口径写实：结构行只认零缩进、条目 = `"- "` 首行 + 两空格缩进续行（含空行）、写入与重建**无损可逆**、注入面读**重建结果**（归档正文里的标题不成为结构、多行条目不拆条）；配额求和 = **条目正文自身字节**（排除载体添加物），**8 × 4096 恰 32768、两档一致**；原子替换失败的判据是**旧文件逐字节不变**，不是"临时文件已清理" | 约束 28 / 29 · 口径 22 / 23 / 24 · §3.2 `carrier_contract_test.go` / `markdown_test.go` | 已落案 |
| **P2-5** | 检索语义与上游表述矛盾 → 裁决为**整串子串匹配、不分词不扩词不归一词形**：连续短语**能**命中、分散词**不**自动组合。旧表述"多词短语必不命中"与实现不符，属**上游契约澄清**——登记后同步上游四处，**不得**只改本文档；测试面**禁止**钉住旧表述，改钉"连续短语命中 / 分散词不命中"**两态** | 注 6 · 约束 9 · 口径 12 · §3.5 改造点 ⑨ · §4.3 R5 与两档召回用例 | 已落案，**上游四处已同步**（2026-10-17：`agent-md.md` §5.4 含澄清块 / `Compatibility.md` §2.2 对照行 / `AiProgrammingGuide.md` §7 陷阱 #11（编号不动）/ `Roadmap.md` B 方向两处 + M2 一处） |
| **P2-6** | 接口与夹具事实修正：`memory.NewService` 返回 `(*Service, error)`（全部调用点按两返回写）；`tool.PolicyOptions` **无** `Logger` 字段；`withMemory` **必须**在构造期置端口（`newLoopHarness` 内部即构造 `AgentService`，构造后再赋 `h.memory` 不接线）；`os.Root` **无** `CreateTemp`（唯一名 `OpenFile(O_CREATE\|O_EXCL)` + `Rename`）；loop harness 是 `package core_test`，`tool/memorytools_test.go` 落 `package tool_test` | §4.3 辅助件表 / R1 / R4b / R7 / R8 · §3.1 `markdown.go` 行 · §2.3 伪代码 | 已落案 |
| **D1** | 段 2 注入面 = **只注生效层核心区**（归档区只经 `recall_memory` 取回） | 口径 1 | 已决 |
| **D2** | 生效层判据 = 该层载体**任一分区正文非空**（分区标题与模板说明**不算内容**），并补「仅归档」用例 | 口径 2 · R3 ③ | 已决 |
| **D3** | 读取失败 = **拒绝启动、零 LLM**；缺失与合法空层单独处理（不并入失败档） | 口径 5 · 约束 27 · R9 ② | 已决 |
| **D4** | 配额拒绝归类 = **`invalid_input`**，保留可读纠正指引（改写归档区） | 口径 8 / 18 · 约束 16 | 已决 |
| **小修** | ① 覆盖率表 `tool` 由 ≥50% 更正为 **≥70%**（与 `scripts/check-coverage.sh` 头注释一致）；② **Demo 二必须写明核心区**（缺省落归档区则 Demo 不成立）并核对 `memory/agents/<name>/MEMORY.md`；③ 三平台编译给出**四目标显式命令**（`make build` 只编宿主）；④ 文头「待登记」同步为**已登记**；⑤ 登记**归档区无配额 + 每次运行整层 `Load`** 的性能边界；⑥ `internal/memory` 导入面判据归**包级架构测试**、不往全局清单加 `grep`；⑦ 上游同步四处逐条点名（并入 P2-5 的同步清单） | §4.1 / §6.1 2 · 10 · 13 / §3.3 / 文头 / §5.2 末行 · 口径 25 / 26 / §3.5 ⑨ | 已落案 |

### 评审回合裁决落案表（续：R2 → R3，2026-10-17 module-dev 步骤 5 停点）

评审回合：**2026-10-17**（R2 → R3）。共 **3 项裁决**（用户裁决：P1-a / P1-b / P2-b）+ **2 处顺手修正**，全部落案；编号**续编不复用**。范围声明（用户逐字）：「本次确认的范围是：按仓库实测新建 `templates_test.go`、同步名集与行为两处承载点，以及报告列出的五项判据变更」——本节各项**不含**范围外概念。

| 编号 | 裁决 | 落案位置 | 状态 |
| --- | --- | --- | --- |
| **P1-a** | 任务清单**三阶段口径**：测试任务只承担「**测试准备**」（测试落地 + **登记红态读数** + 写明转绿前提），实现任务承担「**由红转绿**」，故事收尾才做「**验证**」；**不得**把"准备好测试"记成"测试已通过"；**不得**让后置实现任务成为测试任务的**编译前提**（T042 的「编译前提 = T046」改为「**转绿前提 = T046**」——WARN 实现不该是编译条件）；「纯类型面声明（只增字段 / 注入位、零行为）」不算实现先行 | `specs/006-memory/tasks.md` 阶段口径块 + 全部测试任务 DoD + `run-state.md` §1.4 批准记录 | 已落案 |
| **P1-b** | **条目正文表示统一**：写入入口 `TrimSpace` **一次**（markdown `normalizeEntry` / sqlite `strings.TrimSpace`，同一标准库函数），**单条检查 / 总量求和 / 存储 / 重建 / 召回**一律作用于规范化文本；工具侧先规范化再判空（纯空白与空串同档）；补**首尾空白边界用例**；原「sqlite 存量与重建口径天然一致」断言**废止**（反例：八条首尾各带一空格、原长 4096 → 两档 32752 / 32768 分叉） | 约束 20 / 29 · §2.3 流程图与三处伪代码 · §3.1 四行 · §3.2 用例（`carrier_contract` / `markdown_test` / `memory_store_test` / `memorytools_test`）· 口径 10 / 23 · §4.3 R8 · `spec.md` FR-022 / FR-029 · `data-model.md` 不变量 5 / 8 · `research.md` **D-15**（新增决策） | 已落案 |
| **P2-b** | per-scope 锁**按完整 `Scope` 分档**（每个 Agent 一把 + 全局层一把；`map[core.Scope]*sync.Mutex` + 查表小锁）——按「层」只建两把会让**所有** Agent 共用一把锁；补「**同 Scope 串行、不同 Agent 不互相阻塞**」的验证 | 约束 13 · §3.1 `memory.go` 行 · §4.1 并发面 4 → 5 条 · §4.2 映射行 · §4.3 辅助件表（`blockingStore`）· §3.2 `memory_test.go`（9 → 10）· 口径 19 · `spec.md` FR-017 · `data-model.md` §3.1 · `research.md` D-07（**旧决策被本条推翻，已改写并写明依据**）· `plan.md` | 已落案 |
| **小修 ①** | Path Conventions 的 `internal/tool/*_test.go = tool_test` **过宽**（实测：`httptools_test.go` / `shelltools_test.go` / `runner_windows_test.go` 是 **`package tool`** 内测包）→ 收窄为「**新增 `memorytools_test.go` = `package tool_test`**」 | `specs/006-memory/tasks.md` Path Conventions 表 | 已落案 |
| **小修 ②** | T052 的文件范围核对改用 **`git status --porcelain`**（`git diff --stat` **不含未跟踪的新文件**——本 US 恰有 6 个新文件）+ 显式路径清单 | `specs/006-memory/tasks.md` T052 | 已落案 |

### 评审回合裁决落案表（续：R3 → analyze C1 处置，2026-10-17 实施前阻断检查）

`/speckit-analyze` 实施前阻断检查（2026-10-17）报 **C1（HIGH，覆盖缺口）**：FR-038 / 口径 26 / `plan.md` 三处都要求「记忆包生产导入面」判据由**包级架构测试**承载，但 tasks 的测试任务里**没有任何一条**创建它——T023 只是一次性 `go list` 读数（重跑一次就没了，不是持久判据），且 CI 与 `Makefile` 实测无该判据。用户裁决：**增 1 条架构测试（纯扩面、不放宽既有判据）**。编号**续编不复用**。

| 编号 | 裁决 | 落案位置 | 状态 |
| --- | --- | --- | --- |
| **C1-1** | 增**一条**包级架构测试落地口径 26：`internal/memory/import_test.go`（`package memory_test`）的 `TestDependencyDirection_MemoryImportsOnlyCoreWorkspaceStdlib`——**正向白名单**断言（每个生产导入项 = 标准库 **或** `core` / `workspace`），`go list` 读生产导入面；**纯扩面**——不放宽任何既有判据、不改 `cli/ports_test.go`、不新增第三方坐标与配置键 | §3.2（新增行）· §4.2（映射行）· 口径 26（判据载体落点）· `specs/006-memory/tasks.md` **T053**（测试 42 → 43 条；27 → 28 个文件）· `plan.md`（Testing / Scale）· `quickstart.md` 场景 A3（6 → 7 文件）· `run-state.md` §1.3 续第 10 行 | 已落案 |

### 收口补登

（按 US-4 的格式预留；本 US 收口时补登实施期变更、读数与遗留。）

### 已决事项

| # | 已决（本 US 设计面内自决，无待裁项） | 落点 |
| --- | --- | --- |
| 1 | 记忆文本的承载方式 = `RunEnv` **第三个非导出注入位**（沿 `llmClient` / `agentDir` 先例）；`ConfigSnapshot` 零改动 | §3.1 · 约束 5 / 22 |
| 2 | 记忆接口与端口落 `internal/core`（`LongTermMemoryStore` 三方法 + `MemoryReader`） | §3.1 · 注 2 |
| 3 | 段 2 的文本在**门面**用**一份解析器**从载体规范文本里切出（两档共用；sqlite 档渲染同形文本） | §3.1 · 口径 12 |
| 4 | 配额判在**载体**（sqlite 事务内 / markdown 门面锁内）；**per-scope 锁在门面** | §2.3 流程 D · 约束 12 / 13 |
| 5 | markdown 载体与门面落 `internal/memory`；sqlite 载体落 `internal/storage` | 注 2 |
| 6 | 工作区记忆路径字面量的唯一事实源 = `workspace.MemoryFilePath`（`policy.go` 既有常量不动，R7 守） | 注 4 · 约束 15 |
| 7 | 记忆工具不持有 `Policy` / `CommandRunner`（目标不是沙箱面） | 注 3 · 约束 24 |
| 8 | 双 `scope` 命名照契约 §5.4 注执行（参数 `scope` = 分区；表列 `scope` = 分层） | 约束 8 · §3.4 |
| 9 | `memory` 段的类型化在 `cli` 完成、取值域 fail-closed；wire 侧 default 分支再兜一次 | §3.1 · 约束 18 |
| 10 | 跨档契约表测试落 `internal/memory`（`package memory_test`），test-only import `internal/storage`——生产导入面仍只有 `core` / `workspace` / 标准库 | 注 2 · §3.2 |

### 待决事项（**0 条**）

2026-10-17 评审回合把原 4 条**全部裁决**（D1~D4，见上方落案表），正文按裁决口径书写。下表保留**原选项与换向点**作为取舍留痕——它是"当时为什么这么选"的证据，**不得**据此擅自改判；将来确需换向，须走**新裁决**并在落案表追加一行（编号不复用）。

| # | 原待决 | 原选项 | 裁决与依据 | 改判时的换向点（留痕） |
| --- | --- | --- | --- | --- |
| 1 | **段 2 的注入面** | A. 只注生效层**核心区**（归档区经 `recall_memory` 取回）· B. 整层全量注入（核心区 + 归档区都进系统提示词） | **A（D1）**。依据：agent-md §5.4「核心区不参与检索——它每轮**已完整注入**」、§4.2「核心记忆区**永不裁剪**」、§4.7 不可裁剪列只列「核心记忆」；且**只有核心区有写配额**（配额的意义就在于它会被注入），归档区**无界**——无界内容不可能进"必须有界"的系统提示词 | B 需连带：给归档区补配额与裁剪规则（契约的「永不裁剪」只覆盖核心区）、段 2 尺寸与拒绝启动概率显著上升、`MemoryReader` 语义改写；受影响处：§2.3 流程 A、§3.1、§4.2、口径 1 / 6 / 11 / 25、R1 / R3 |
| 2 | **生效层判据** | A. 该层**任一分区正文非空**（字面）· B. 该层**核心区**非空 | **A（D2）**。依据：契约 §5.2 与需求 §9.3 的字面是「Agent 层有内容」——未限定分区；且分区标题与模板说明**不算内容** | 改 B 的行为差异：Agent 层**只写了归档**时，段 2 会回退去注全局层核心区（当前为段 2 空）；换向点 = 门面一处判据 + R3 第 ③ 步（就该步红） |
| 3 | **记忆读取（I/O）失败处置** | A. **拒绝启动**（`failed` + Reason 点名载体，零 LLM）· B. 记 WARN 后段 2 恒空、运行继续 | **A（D3）**。与段一（`AGENT.md` / 快照）读取失败**同口径**（`service.go` ②③ 既有形状），且合 fail-closed 的审计纪律：记忆读不出来时，Agent 会在"不知道用户偏好"的状态下继续对外说话 | 改 B 受影响处：§2.3 流程 A 的失败分支、约束 23 / 27 第二档、R9 ②（该断言改判 WARN + 继续）；"缺失 / 合法空层"档**不随之改判**（它是正常取值，不是失败） |
| 4 | **配额拒绝的 `error_kind`** | A. `invalid_input` · B. `runtime_error` | **A（D4）**。契约 §5.4 把它与「入参非法」并列归入可读拒绝；执行器判序里 `InvalidInput` 先于 `runtime_error`，且模型看到的应是"改写归档区"这类**可自我纠正**的指令 | 改 B 一处代码点（映射类型）+ 口径 8 / 18 与 `tool/memorytools_test.go` 的一条断言 |

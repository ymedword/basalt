# Implementation Plan: US-6 Memory 长期记忆（两层作用域 + 两档载体 + 两件记忆工具）

**Branch**: `006-memory` | **Date**: 2026-10-17 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/006-memory/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 6` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us6-memory.md](../../docs/us/us6-memory.md) §2 / §3 为准，冲突时以 [docs/TechnicalSolution.md](../../docs/TechnicalSolution.md) 为准（§5.1 统一门面与行为契约 / §5.2 两层作用域与回退 / §5.3 载体分档 / §5.4 MEMORY.md 形态 / §4.2 提示词四段 / §4.7 上下文预算与不可裁剪 / §11.5 快照语义 / §3 配置面 / §10.1 包结构与依赖方向）。

## Summary

给底座装上**长期记忆**：Agent 第一次能跨对话携带事实。交付面三件可判卷的事——**段 2 注入**（每次运行开始时把生效层核心区读成文本拼进系统提示词，运行内冻结）、**两层作用域**（Agent 层优先、全局层回退、**绝不叠加**；无 Agent 上下文 = 全局层，是正常取值不是异常）、**两档载体**（markdown 人可读可手改 / sqlite 可并发可查询，换档 = 改 `memory.backend` 一行 + 重启，两档跑同一张场景表）；出口是 `save_memory` / `recall_memory` 两件工具，`memory/` 目录对文件工具仍不可写（US-5 交付面，本 US 只加负向用例守住它）。

技术路线的四个要点：**① 注入链——读一次、拼一处、运行内冻结**——`core` 侧只增**纯类型面**（`LongTermMemoryStore` 三方法逐字 / `MemoryReader` 单方法读面 / `Scope` + `GlobalScope` / `Section` 两常量 / 配额常量 / `MemoryQuotaError`），实现一律在 `memory` 门面；`AgentService.Run` 在**步骤 ④**（组装 `RunEnv` 时）读**一次**生效记忆，经 `RunEnv` 的**第三个非导出注入位**（沿 `llmClient` / `agentDir` 先例，**不得**走 `context.WithValue`）传给 `PromptBuilder.Build`，后者拼成 `Prompt.System = 段一正文 + "\n\n" + 核心区文本`——**核心区为空则不产生任何文本**，与 US-4 逐字节相同（R1 的空记忆侧就是这条逐字节回归）；每轮重组提示词**不得**重读载体（`scriptedMemoryReader` 逐次返回不同文本，第 2 轮看到它即红——R2）。**② 门面与两档同一语义**——`memory.Service` 是唯一门面（`LoadEffective` 负责回退不叠加；两档共用**一份**分区解析器 `parseSections`）；`markdown` 档 `MarkdownStore` 全部读写经 `os.OpenRoot(root)` 根句柄、**唯一名临时文件 + rename** 原子替换（失败清理且旧文件逐字节不变）；`sqlite` 档 `MemoryStore` 的「配额求和 + INSERT」在**同一事务**内（包内复用 `withWriteLock`——**非导出**原语，不导出）；同一作用域写入串行化 = 门面 **per-scope 锁（按完整 `core.Scope` 分档**：`map[core.Scope]*sync.Mutex` + 查表小锁——每个 Agent 一把 + 全局层一把；同 Scope 串行、**不同 Agent 不互相阻塞**），跨进程不承诺。**③ 读取面三档，不得塌成一档**——① 文件不存在 / 合法空层 = 空层（空文本 + nil error，回退照常，首次写入即创建）② 其他 I/O 错误 = 返回错误（注入面**拒绝启动**、零 LLM；写入面不触碰原文件、原样上抛）③ 非空但结构损坏（缺分区标题 / 标题重复 / 归档早于核心）= 拒绝使用、拒绝覆盖、**原文件逐字节不变**，错误点名文件与症状（「标题破坏 → WARN + 当空层回退」的旧断言**已废止**，上游四处已同步）。配额两条同时进：单条 ≤ 4096 / 每层核心区 ≤ 32768（**代码常量**，不是配置键）；**手工编辑超配额不按写拒绝**——注入期记 WARN 并**完整注入**（不截断、不按条丢弃），因此超上下文预算时走拒绝启动（failed + Reason 点名 `budget.context_tokens`，零 LLM 调用）。**④ 工具面与装配收口**——两件记忆工具**不持** `Policy` / `CommandRunner`（不进沙箱面）、不落审计、不写消息、不记日志（失败分类沿用既有 `ToolExecutor` 判序；双轨由执行器承载）；作用域**只在工具层解析**（`env.AgentName != ""` → Agent 层；否则全局层——模型**不得**传 `agent_name`、不得从路径反推）；工具参数名是 `scope`（分区 `CORE` / `ARCHIVAL` → 落 `memory_entries.section` 列，**不得**改名 `section`——表列 `scope` 是**分层**，两处语义不得混淆）；装配池 **6 → 8** 件、模板默认工具 `[]` → 两件记忆工具、`cli.Config.Memory` 从 `map[string]any` 变类型化并 fail-closed 校验取值域。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）

**Primary Dependencies**: **零新增坐标**（宪法 VIII）。全部落在标准库（`context` / `errors` / `fmt` / `log/slog` / `os`（含 `os.OpenRoot`）/ `path/filepath` / `strings` / `sync` / `time` / `encoding/json`）+ 既有 `modernc.org/sqlite` v1.60.1（sqlite 载体，零新增）+ 既有 `stretchr/testify` v1.12.1（测试）。无新增 `import _` 副作用。

**Storage**: SQLite——`memory_entries` 六列（`id` / `scope` / `agent_name` / `section` / `content` / `created_at`）+ `idx_memory_scope(scope, agent_name, section, id)` 由 US-1 建齐，本 US **零迁移、零列变化**（判据：`git diff docs/contracts/schema.sql` 为空）。列级口径逐条重申：`scope` = **分层**（`agent` / `global`）与工具参数 `scope`（**分区**）**不是一回事**；`agent_name` 全局层恒 **NULL**（查询用 `IS NULL`，**不得**用空串）；`section` = **分区**（`core` / `archive`）；`content` 非空（空 `content` 在工具层即拒）；配额按 `SUM(octet_length(content))` 求和——与 markdown 档重建口径（条目正文 UTF-8 字节）**逐字节同源**；`created_at` 由载体构造器显式传入的时钟写入（`NewMemoryStore(db, now)`），**不**在 SQL 里取当前时间。三类查询形状固定、全部命中 `idx_memory_scope`：Agent 层 `WHERE scope='agent' AND agent_name=?`；全局层 `WHERE scope='global' AND agent_name IS NULL`；归档检索 `instr(content, ?)` 按 `id` 升序。审计面**零改动**：两件记忆工具**不落审计**（`tool_invocations` 只记经 `ToolExecutor` 的工具调用——记忆工具同样是工具，仍走执行器；「不落审计」指**工具本体不写**第二套审计行）、不写消息；本 US 不写任何审计 SQL。

**Testing**: `go test -race ./...` 默认层——**新增 7 个测试文件 43 条**（`memory/memory_test.go` **10** / `memory/markdown_test.go` 8 / `memory/carrier_contract_test.go` 7（`package memory_test`，test-only 引 `internal/storage`）/ `memory/import_test.go` **1**（**包级架构测试**：`TestDependencyDirection_MemoryImportsOnlyCoreWorkspaceStdlib`——`go list` 读 `internal/memory` 的**生产导入面**、正向白名单断言；**analyze C1 处置新增**，见下段）/ `storage/memory_store_test.go` 6 / `tool/memorytools_test.go` 9（**`package tool_test`**）/ `workspace/templates_test.go` 2）+ **改动 6 个测试文件追加 15 条**（`core/prompt_test.go` 3 / `core/service_test.go` 5 / `workspace/agent_test.go` 1 / `cli/config_test.go` 3 / `app/wire_test.go` 3）+ **夹具扩展 1 处**（`core/loop_test.go` 0 条：`loopHarness` 增 `memory core.MemoryReader` 字段 + `harnessOption` / `withMemory` 选项，**构造前**应用——`ServiceOptions` 构造时装配，构造后再赋值不接线）+ **既有判据同步 3 处**（见下「文档 ↔ 仓库事实差异」）。`memory_test.go` 由 9 → **10** 条 = R3 裁决 P2-b 新增 `TestService_AppendLocksAreKeyedByScope`（锁按**完整 `Scope`** 分档 + `blockingStore` 闸门夹具）；`memory/import_test.go` 的 1 条 = **analyze C1 处置**（FR-038 / 口径 26 的「记忆包生产 import 面」判据**必须**由包级架构测试承载——原 tasks 无一条任务承载它）。条数合计与颗粒度文档 §3.2 对齐（**43 + 15 = 41 + 17**：差异只在文件归属分类，用例数一条不差）；收口逐文件 `grep -c '^func Test'` 核对。**不访问真实网络、不打真实模型、不起真实子进程**；真实文件系统（`t.TempDir()`）与真实本地 SQLite 库文件**允许**（与 US-4 / US-5 同口径）。判卷纪律：4 条并发面用例（`memory_test` / `carrier_contract_test` / `memory_store_test` / `tool` 面）全部在 `-race` 下跑且夹具带锁；R3 的两档同一断言经**同一张场景表**（`carrier_contract_test.go`）跑；R4b 含**同预算无记忆的对照组**（证明拒绝是记忆撑爆的，不是预算本身）；R7 双向对照（文件工具拒写 `memory/` + 记忆工具可写）在同一函数内。**集成层本 US 无新增**（`make test-integration` 跑既有；集成面无真模型 / 真 webhook 增量）。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物 `CGO_ENABLED=0` 交叉编译三平台（人工项）；本 US **无新平台分片文件**；`-race` 与发布构建**分属不同命令 / job**。

**Project Type**: CLI + 服务单二进制（本 US 新增两个包内文件与一个工作区路径约定函数；**不新增内部包**、不新增顶层目录、不新增命令）

**Performance Goals**: 无显式吞吐 / 延迟指标（底座零件类 feature）；判据是**行为顺序与可解释边界**——每运行读**一次**（R2：第 2 轮不得看到改后文本）、运行内冻结；核心区注入**完整不截断**（R4a：逐字节等于完整期望文本 + 尾部哨兵）；超上下文预算的处置是**拒绝启动**（R4b：零 LLM 调用、Reason 点名 `budget.context_tokens`）而**不是**静默截断；配额边界**恰好**可判（8 条恰 4096 字节条目 → 32768 恰放行、第 9 笔拒，两档同读数）；归档区**无配额、无索引优化、无分页**——每次运行**整层 `Load` 一次**是**已登记的已知量**（如实写进文档与验收报告，**不得**写成「性能已验证达标」）；sqlite 档并发写 R5 / R6（12 笔并发恰 8 笔通过、一条不丢，多重集比较）。

**Constraints**: 三条包纪律的落法——`core` **只依赖标准库**（新增 `memory.go` 纯类型面；既有判据 `GOPROXY=https://goproxy.cn go list -f '{{.Imports}}' ./internal/core` 只出标准库）；`memory` 包**生产 import 面 = `core` + `workspace` + 标准库**（`core` 侧接口由 `memory` 实现，方向 `memory → core` 不反向；test-only 引 `internal/storage` 由包级架构测试承载——**实指 `internal/memory/import_test.go`**（1 条，正向白名单断言，沿 `cli/ports_test.go` 先例），**不得**往全局不变量清单加全仓 grep 判据）；`tool` **不得** import `cli` / `app`（既有判据）；`app` 是唯一装配层（注册池 +2、记忆门面构建、`ServiceOptions.Memory` 接线）。**状态传递纪律**：记忆文本经 `RunEnv` 第三个**非导出注入位**（`memory string`）传入——`context.WithValue` 零使用（收尾逐处确认）；`ConfigSnapshot` **不得**新增记忆字段（快照语义 US-4 已定，记忆是**运行级**输入不是配置级；判据 `grep` 快照结构零改动）。**可观测性双轨**：工具本体**不记日志**、不写消息；失败路径的审计落库与 WARN 固定键仍由既有 `ToolExecutor` 承载（键集逐字不变）；注入面的 WARN 只有一处——手工编辑超配额（R4a），**日志不得打记忆正文**（正文是用户 / 模型内容）。错误不吞（`err` 必回填 / 上抛；**不得**用 `_ =` 静默丢弃）。敏感面：错误文案与日志**不得**含记忆正文；配额拒绝文案**点名触发的上限**并给出可执行的下一步（改写归档区 / 压缩内容）。配置面：`memory.backend ∈ {markdown（缺省）, sqlite}`，`mem0` 与未知取值 = 拒绝启动并点名键与取值（fail-closed，**取值域外即拒**）；**零新增配置键**（配额是代码常量）。覆盖率：`memory` / `core` / `tool` / `storage` ≥70%、其余 ≥50%，由既有 `make cover` 按包判失败（**不改**脚本与阈值；`memory` 本 US 从骨架变主体，同按 70% 判）。静态门禁：抑制注释（`//nolint` / `#nosec` / `//lint:ignore`）**零使用**。

**Scale/Scope**: 代码 **新增 6 文件 + 改动 7 文件**（`core` 1 新 3 改 / `memory` 2 新 1 改 / `storage` 1 新 / `tool` 1 新 / `workspace` 1 新 1 改 / `cli` 1 改 / `app` 1 改）+ 测试 **新增 7 文件 43 条 + 改动 6 文件追加 15 条 + 夹具扩展 1 处 + 既有判据同步 3 处**；`go.mod` / `go.sum` / `basalt.yaml.example` / `Makefile` / CI / `docs/contracts/` **零改动**；**零迁移脚本**；前序改造点 9 项（逐条属软门禁——§3.5）+ 后续占位 5 项（只留装配位，**不实现**）。

**环境实测读数（2026-10-17，本轮当场命令）**: `go version` → `go1.27.2 windows/amd64`（与 `toolchain go1.27.2` 一致）；包清单逐目录读取——`internal/memory` **只有 `doc.go`**（骨架占位属实）、`internal/core` 22 文件（`run.go` / `prompt.go` / `service.go` / `snapshot.go` 均在改造面）、`internal/tool` 17 文件（**无** `memorytools.go`）、`internal/workspace` 21 文件（**无** `memory.go` / **无** `templates_test.go`）、`internal/cli` 21 文件、`internal/storage` 27 文件（**无** `memory_store.go`）、`internal/app` 11 文件；零既有记忆符号——`grep -rn "LongTermMemoryStore\|MemoryReader\|MemoryFilePath\|MemoryConfig\|LoadEffective" --include='*.go' cmd internal` **零命中**（交付面全新增）；改造点逐条核对——`core/prompt.go:37` 注释「段 2 记忆注入（本 US 恒空…）」与 `:94` 行内占位**属实**（本 US 刷新）、`core/run.go` 的 `RunEnv` 现为五导出字段 + **两个**非导出注入位（`llmClient` / `agentDir`——第三个位置属实）、`core/snapshot.go` 的 `ConfigSnapshot` **无**记忆字段（FR-007 判据的基点）、`core/service.go` 的 `ServiceOptions` 十字段（`Agents` / `Client` / `Messages` / `Runs` / `Snapshots` / `Audit` / `Loop` / `Logger` / `Now` / `MaxConcurrent`）与九步 `Run` 的步骤 ④ 位置**属实**、`tool/policy.go:22` `memoryDirName = "memory"` + `:48-49` 规则常量与指引文案 + `:203` deny 调用**在案**（本 US 零改动，只加负向用例）、`storage/db.go:276` `withWriteLock` 为**非导出**原语**属实**、`workspace/templates.go:41`（`memory/MEMORY.md` 属 `fileTemplates`）+ `:94-102`（`templateMemoryMD` 含两分区标题）+ `:145`（`tools: []` 占位注释）**属实**、`cli/config.go:33`（`Memory map[string]any`）+ `:356`（`validateConfig`）**属实**、`app/wire.go` 七步装配（② `OpenWritable` ③ `Migrate` ④ `Recover` ⑤ 策略 / 注册表 / runner / 六件工具循环 ⑥ `NewAgentLoader{Tools: registry.Names()}` ⑦ 三仓储 + 循环 + 服务）**属实**；既有测试条数当场读取——`core/prompt_test.go` 12 / `core/service_test.go` 10 / `core/loop_test.go` 17 / `workspace/agent_test.go` 23 / `cli/config_test.go` 17 / `app/wire_test.go` 5。**本轮未复测的项不在此声称**（**未跑** `make check`、**未跑**三平台编译、**未读**覆盖率读数）。工具链与静态检查工具就绪读数沿用 US-1 / US-4 / US-5 plan。

### 文档 ↔ 仓库事实差异（实测发现，两条；**不静默改颗粒度文档**，按仓库事实落地并如实登记）

| # | 颗粒度文档写法 | 仓库实测 | 落地口径 |
| --- | --- | --- | --- |
| 1 | §3.2 把 `internal/workspace/templates_test.go`（2 条）列为**改动**文件；§3.5 第 2 项说「`workspace/templates_test.go` / `agent_test.go`：既有 `tools` 为空的断言需同步」 | `templates_test.go` **不存在**（`ls internal/workspace/` 实测）；「模板 `tools` 为空」的既有断言实际在 **`internal/workspace/agent_files_test.go`** 两处：`TestCreateAgent_WritesLoadableTemplate`（`:68` 装载器**不注入** `Tools` → 模板声明两件工具后 `Registered` 变 false（`:74` 断言破）；`:78` `require.Empty(t, Profile.Tools)` 同破——注释逐字写着「tools: []（记忆工具到 US-6 才在池）」）与 `TestCreateAgent_EncodesNamesThroughYAML`（`:147` 同形装载器 + `:151` `require.True(Registered)` 破） | ① 2 条**新用例**按文档点名的文件名**新建** `internal/workspace/templates_test.go`（文件不存在 → 分类从「改动」变「**新增**」，语义不变，文件名不擅自改）；② 判据面变更落在**实测承载文件** `agent_files_test.go`：两个装载器注入 `Tools: []string{"save_memory", "recall_memory"}`（模板声明的就是这两件，装载器池必须与之对应）、`:78` 的 `require.Empty` → 等值断言（**判据面变更**，随 tasks 送审） |
| 2 | §3.2 / §3.5 第 3 项把「池名集 6 → 8 件」的判据扩面写在 `app/wire_test.go` 的 `TestWire_RegistryNamesReachAgentLoader` | 该函数（`wire_test.go:277`）判的是**行为**（声明池内名 `read_file` → 注册成功；池外名 → 整只不注册），**无**「恰好六件」断言——池扩到 8 它照旧通过。**恰好六件**的机器判据实际在 **`internal/app/ports_test.go:304`** `TestToolListerPort_HeavyAssemblyRegistersSixTools`（`:313-315` `require.Equal` 六名单集 + `:317` 三件 `Resolve` 循环） | ① 真判据扩面落在实测承载点 `app/ports_test.go`（六名单集 → 八件、`Resolve` 循环增两件记忆工具、函数名同步为事实形态）；② `app/wire_test.go` 按文档意图在本 US 3 条新用例之上补一条记忆工具**经装配链注册成功**的行为断言（池名集判据的第二个面）——两边都不落空，差异在步骤 5 停点随 tasks 一并报告 |

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

对照 [.specify/memory/constitution.md](../../.specify/memory/constitution.md) 十原则 + 三条架构约束 + 开发工作流（GATE 判定：通过 / 不适用 / 需说明）：

| # | 宪法条目 | 本 feature 关系 | GATE |
| --- | --- | --- | --- |
| I | 自实现 ReAct 循环 | **适用**：`go.mod` **零新增坐标**（收尾 `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中）；本 US 不触碰循环体——记忆是**运行级输入**（运行开始时读一次），不是循环内行为 | 通过 |
| II | LLM 客户端只做协议调用 | **适用**：会话历史仍**一条消息仓储**承载——记忆**不写消息**、不追加第二条路径；模型响应与工具结果仍由 `ReActLoop` 统一追加；两件记忆工具经既有 `ToolExecutor` 调度（工具本体不写消息、不落审计） | 通过 |
| III | Provider 必须显式映射 | **不适用**（本 US 不触碰 provider 路由与装配映射表） | 不适用 |
| IV | 一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露 | **适用（弱形态）**：注册池 6 → 8 件后装载器的名字集规则继续生效（池内名字更新即生效）；**模板默认工具随之落位**（`tools: [save_memory, recall_memory]` + 注册池 8 件的**一致性**是本 US 的连带判据——`agent_files_test.go` 的两处同步正是这条约束的机器判据）；技能面零改动（**不得**新增 `use_skill`、**不得**预载正文） | 通过 |
| V | 审计 day-one 且 fail-closed | **适用（零改动面 + 一条新判据）**：记忆工具不写审计（审计两阶段写仍只在既有执行器链路）；**拒绝启动的两种新档**（读取面 I/O / 非空损坏；注入后超上下文预算）**必须发生在任何 LLM 调用之前**——判据是零 LLM 调用（R4b 含对照组） | 通过 |
| VI | 授权与执行分离（两接口） | **适用（负向形态）**：两件记忆工具**不持有** `Policy` / `CommandRunner`（不进沙箱面，FR-024）；`memory/` 目录的不可写规则仍由既有 `WhitelistPolicy` 判（本 US 零改动，只加 R7 负向用例守住）——**不得**让记忆工具绕开或替代沙箱判定 | 通过 |
| VII | 同步执行 + context 只承载取消 / 截止 / 追踪 | **适用（强形态）**：记忆读取是**同步**调用（`LoadEffective` 运行开始时一次）；`context.WithValue` **零使用**（记忆文本经 `RunEnv` 非导出注入位显式传递——收尾 grep 逐处确认）；per-scope 锁只做**进程内串行化**，不引入异步编排、不起后台 goroutine；跨进程并发**不承诺**（如实登记） | 通过 |
| VIII | 单二进制纪律 + 依赖最小化 | **适用**：**零新增坐标**（标准库 + 既有 SQLite 驱动）；`os.OpenRoot` 是标准库（Go 1.24+）；无新平台分片文件；发布 `CGO_ENABLED=0`；`-race` 与发布构建分属不同命令 / job | 通过 |
| IX | 不用 ORM，手写 SQL | **适用（强形态）**：sqlite 载体手写 SQL（`SELECT SUM(octet_length(content))` / `INSERT` / `instr` / `ORDER BY id`），**零迁移脚本**（表与索引 US-1 已建齐，只追加不改写）；「配额检查 + 追加」在**同一事务**内（`withWriteLock`） | 通过 |
| X | 五大能力优先，范围卡紧 | **适用**：交付面对照颗粒度文档 §5 逐项边界（向量检索 / `mem0` / 多租户隔离 / 写入审核 / 归档区裁剪压缩 / 记忆管理端点与界面 / 配额配置键 / Agent 删除清理**全不做**）；对外概念零发明（导出签名逐字以 §3.1 为准）；归档区无配额的**边界如实登记为已知量**，不包装成承诺 | 通过 |

**架构约束**：依赖方向 `channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 是唯一装配层——本 US 的落法：`core` 新增 `memory.go`（纯类型 + 常量 + 一个错误类型，**零 import 内部包**，判据机器可判）；`memory` 新增两文件 import `core` + `workspace`（路径取 `MemoryFilePath`——**唯一事实源**，不复制字面量）+ 标准库；`storage` 新增 `memory_store.go` import `core` + 同包 `withWriteLock`（**不导出**）；`tool` 新增 `memorytools.go` import `core` + 标准库（**不** import `cli` / `app`，也不 import `memory`——只认 `core.LongTermMemoryStore` 接口）；`workspace` 新增 `memory.go` 零新依赖（标准库）；`cli` 只改 `config.go`（类型化 + 取值域校验，**不** import `memory`）；`app` 在 ④⑤ 之间构建门面并接线 `ServiceOptions.Memory`；`cmd/basalt` 不动。无环由 `go list` 复核——本 US 落**第二处包级架构测试** `internal/memory/import_test.go`（口径 26 / FR-038，沿 `cli/ports_test.go` 先例）：对 `internal/memory` 的**生产导入面**（`go list -f '{{join .Imports " "}}'`，`{{.Imports}}` 是非测试导入集）逐项断言「标准库（路径首段不含点）**或**白名单（`core` / `workspace`）」——正向白名单比 `cli/ports_test.go` 的禁 import 清单更强（清单外的越界依赖也红）；`carrier_contract_test.go` 的 test-only `storage` 导入**不受影响**。接口定义在消费方：`core.LongTermMemoryStore`（三方法）与 `core.MemoryReader`（单方法 `LoadEffective`）由 `core` 声明、实现放 `memory` / `storage`——同一份实现用编译期断言双向证明（`var _ core.LongTermMemoryStore = (*Service)(nil)` 等 4 处）。第三方实现不进 `core`（本 US 无新第三方）。

**开发工作流**：门禁一条命令 `make check`（七目标，**`Makefile` / CI / 覆盖率脚本零改动**——`memory` 已在判卷内且本 US 起有可执行语句，按 70% 档判）；三条实测纪律适用（格式判卷**先退出码后输出** / `-race` 与 `CGO_ENABLED=0` **分离** / **无集成层新增**，`test-integration` 跑既有）；覆盖率不可豁免（`memory` / `core` / `tool` / `storage` ≥70%，其余 ≥50%）；提交纪律（AI **不自动** commit / push / tag）。

**plan review 必查（宪法 §开发工作流）逐条**：

| 必查项 | 本 plan 的答复 |
| --- | --- |
| 依赖方向无环 | `core` 新文件只 import 标准库（收尾 `go list -f '{{.Imports}}' ./internal/core` 复跑）；`memory` 生产 import 面 = `core` + `workspace` + 标准库（test-only 引 `storage` 由包级架构测试承载 = `internal/memory/import_test.go` 的 `TestDependencyDirection_MemoryImportsOnlyCoreWorkspaceStdlib`——本 US **持久判据**载体）；`tool` 不 import `cli` / `app`（既有判据）；`workspace` 零新依赖；`app` 装配全部；`cmd/basalt` 不动 |
| 审计写入路径唯一 | 两件记忆工具**不落审计**（签名无关联标识参数——`Execute(ctx, env, input)` 形态不变）；`tool_invocations` 两阶段写仍只在 `storage.ToolInvocationRepo`、由 `core.ToolExecutor` 调用（记忆工具调用同样经它——**不新增**审计写入方）；`grep -rn '\.BeginCall(' --include='*.go' internal/core` 零命中（不变量 11）复跑 |
| 无框架坐标 | `go.mod` **零新增**（判据 `git diff go.mod go.sum` 为空）；收尾 grep 零命中同上 |
| 无 `context.WithValue` 业务态 | 记忆文本走 `RunEnv` **第三个非导出注入位**（沿 `llmClient` / `agentDir` 先例）；收尾 `grep -rn "context.WithValue" --include='*.go' cmd internal` 每处命中人工确认；`LoadEffective` 的 `agent` 走显式字符串参数、不进 ctx |
| 单二进制构建通过 | 本 US 无新平台分片文件（无 build tag 增量）；`build` 目标 `CGO_ENABLED=0 go build`；`-race` 只在 `test` 目标；`os.OpenRoot` 在 Windows / Linux / macOS 三平台均有实现（三平台编译是**人工项**，**不进** `make check`） |

**Phase 1 后复评（post-design）**：设计产物（[research.md](./research.md) / [data-model.md](./data-model.md) / [quickstart.md](./quickstart.md)）完成后重跑本表——结论同前，无新增违规、无 Complexity Tracking 条目（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/006-memory/
├── spec.md              # /speckit-specify 输出（含 Clarifications 会话 2026-10-17：三条收敛项）
├── plan.md              # 本文件（/speckit-plan 输出）
├── research.md          # Phase 0 输出（决策与写前核实清单）
├── data-model.md        # Phase 1 输出（实体、字段与不变量）
├── quickstart.md        # Phase 1 输出（可复制执行的验证步骤）
├── run-state.md         # 过程状态（module-dev 步骤 0 建；入库、收口不删）
├── checklists/
│   └── requirements.md  # 需求质量清单（/speckit-specify 输出；16/16）
└── tasks.md             # /speckit-tasks 输出（下一步生成）
```

**`contracts/` 本 feature 不新建**（与 US-1~US-5 同口径）：本 US 的固定字面量**已有唯一事实源**——工具参数表与检索语义在 [docs/contracts/agent-md.md](../../docs/contracts/agent-md.md) §5.4（含 2026-10-17 澄清块）、裁剪顺序「核心记忆区永不裁剪」在 §4.2；预算表与拒绝形状在 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md) §6 / §7.2 / §7.3；列级事实以 [docs/contracts/schema.sql](../../docs/contracts/schema.sql) 的 `memory_entries` 六列 + `idx_memory_scope` 为准；导出签名逐字来源在颗粒度文档 §3.1、固定口径 26 条在 §6.2。再抄一份到 `specs/` 就是**第二份字面量**——抄一份必漂移一处。plan 的契约节只**引用**，不复制。

### Source Code (repository root)

```text
internal/
├── core/                      # 只依赖标准库（既有判据机器可判）
│   ├── memory.go              # 新增：Scope + GlobalScope / Section（core · archive）/ MaxCoreEntryBytes=4096 ·
│   │                          #       MaxCoreSectionBytes=32768 / QuotaKindEntry · QuotaKindSection /
│   │                          #       LongTermMemoryStore（三方法逐字）/ MemoryReader（LoadEffective）/
│   │                          #       MemoryQuotaError{Kind, Limit}——纯类型与常量，零 import 内部包
│   ├── run.go                 # 改造：RunEnv 增**第三个非导出注入位** memory string（沿 llmClient / agentDir
│   │                          #       先例；注释声明「不是契约字段、不进任何出站形态」）；导出字段零变化
│   ├── prompt.go              # 改造：Build 拼段 2——system = SectionOne + "\n\n" + env.memory；
│   │                          #       env.memory == "" → 与 US-4 逐字节相同；刷新「段 2 恒空」的过期注释
│   └── service.go             # 改造：ServiceOptions 增**可空** Memory MemoryReader；步骤 ④ 读一次生效记忆
│                              #       （失败 → 既有 StatusFailed 形状、零 LLM）并写入 env.memory
├── memory/                    # ≥70% 覆盖率判卷（生产 import 面 = core + workspace + 标准库）
│   ├── doc.go                 # 改造：骨架占位 → 包职责正稿（门面 / 两档共用解析 / per-scope 锁 / 注入期 WARN）
│   ├── memory.go              # 新增：Service 门面——NewService(Options{Backend, Logger})（Backend == nil 即拒）+
│   │                          #       Append / Load / RecallByKeyword / LoadEffective（回退不叠加）+
│   │                          #       per-scope 锁（**按完整 core.Scope 分档**：map[core.Scope]*sync.Mutex +
│   │                          #       查表小锁；每个 Agent 一把 + 全局层一把）+ 两档共享的配额与解析调用面
│   └── markdown.go            # 新增：MarkdownStore——NewMarkdownStore(root)（os.OpenRoot 根句柄）+
│   │                          #       parseSections / renderMemory / normalizeEntry（与 §2.3 伪代码同名同构）+
│   │                          #       唯一名临时文件 + rename 原子替换（失败清理、旧文件逐字节不变）+
│   │                          #       normalizeEntry **先于两条配额检查**（六面同表示，D-15）+
│   │                          #       条目正文 UTF-8 字节配额求和 + 归档区条目级整串检索 + 读取面三档
├── storage/
│   └── memory_store.go        # 新增：MemoryStore——NewMemoryStore(db *DB, now func() time.Time) +
│                              #       写入入口 strings.TrimSpace 一次（单条检查 + INSERT 都用规范文本，D-15）+
│                              #       「SUM(octet_length(content)) 配额检查 + INSERT」同事务（包内复用
│                              #       withWriteLock，**不导出**）+ instr 召回按 id 升序 + 规范文本渲染 + 零迁移
├── tool/
│   └── memorytools.go         # 新增：SaveMemoryTool / RecallMemoryTool——NewSaveMemoryTool(store) /
│                              #       NewRecallMemoryTool(store) + Name / Description / InputSchema / Execute；
│                              #       内联 JSON Schema（save：content 必填 + scope 枚举 CORE·ARCHIVAL；
│                              #       recall：query 必填）；decode + **规范化** + check 范式（入参先 TrimSpace
│                              #       再判空——纯空白与空串同档，D-15；失败归 *core.InvalidInputError）；
│                              #       作用域只在工具层解析（ToolEnv.AgentName）；不持 Policy / CommandRunner
├── workspace/
│   ├── memory.go              # 新增：MemoryFilePath(agent string) string（唯一事实源——agent == "" →
│   │                          #       memory/MEMORY.md；否则 memory/agents/<name>/MEMORY.md；无前导斜杠）
│   └── templates.go           # 改造：agentTemplate 默认工具 []string{} → []string{"save_memory", "recall_memory"}
│                              #       + 刷新其上方过期注释（「记忆工具到 US-6 才在池」）
├── cli/
│   └── config.go              # 改造：Config.Memory map[string]any → 类型化 Memory MemoryConfig；
│                              #       validateConfig 增取值域（缺省 markdown；sqlite 合法；mem0 / 未知 → 拒绝启动
│                              #       并点名键与取值）；LoadConfig 签名不变
└── app/
    └── wire.go                # 改造：④ 与 ⑤ 之间构建记忆门面（按 cfg.Memory.Backend 选载体，default 分支
                               #       fail-closed 双保险）；⑤ 注册池 6 → 8（两件记忆工具）+ 占位注释刷新；
                               #       ⑦ ServiceOptions.Memory 接线

go.mod / go.sum                      # 零改动（零新增坐标）
basalt.yaml.example / Makefile / CI  # 零改动（memory.backend 键已存在；门禁与覆盖率判卷已覆盖本 US 的包）
docs/contracts/**                    # 零改动（固定字面量引用不复制）
```

**Structure Decision**: 单 Go module + `internal/` 分包；本 US 新增实现文件 6 个、改造 7 个、新增测试文件 **7** 个 + 追加 6 个（+ 夹具扩展 1 处 + 既有判据同步 3 处），**不新增内部包、不新增顶层目录**。与台账「模块落位（摘要）」列一致：`internal/memory`（门面 + markdown 载体 + per-scope 锁）、`internal/storage`（sqlite 载体、零迁移）、`internal/core`（记忆接口与端口 + 段 2 注入）、`internal/workspace`（`MemoryFilePath` 路径约定 + 模板默认工具）、`internal/tool`（两件记忆工具）、`internal/app`（装配接线：门面构建 + 注册池 6 → 8）、`internal/cli`（memory 段类型化与取值域校验）；`go.mod` 零新增坐标。前序改造点 9 项（§3.5 表）全部为**加文件 / 加字段 / 加类型化与校验 / 改注释**形态——**不得**删除或改写既有字段、签名与行为；其中 ①②③ 改变既有测试的**判据面**（见上「文档 ↔ 仓库事实差异」与 §3.5），⑧ 属随已批准接口适配（新增字段与选项、默认 nil，**不改任何既有判据**），⑨ 上游文档同步**已完成**（2026-10-17）。后续占位 5 项（`notify` / `manage_schedule` / `mem0` / 记忆管理端点 / 归档区裁剪）只留装配位与注释，**不实现**。

**分层落位（三条包纪律的逐条落地）**：

| 纪律 | 本 US 的落法 |
| --- | --- |
| 依赖方向无环 | `core` 新文件只 import 标准库（判据 `go list`）；`memory` 生产 import = `core` + `workspace` + 标准库（`workspace` 只取 `MemoryFilePath` 一个函数——路径约定集中一处）；`storage` 只 import `core` + 同包；`tool` 只 import `core` + 标准库（**不** import `memory` / `cli` / `app`——只认接口）；`app` 装配全部 |
| 接口定义在消费方 | `core.LongTermMemoryStore` / `core.MemoryReader` 由 `core` 声明（消费方），`memory.Service` 与 `storage.MemoryStore` 实现——四个编译期断言（`var _ core.LongTermMemoryStore = (*Service)(nil)` / `= (*MemoryStore)(nil)`，`var _ core.MemoryReader = (*Service)(nil)` + 工具侧 `var _ core.Tool = (*SaveMemoryTool)(nil)` / `(*RecallMemoryTool)(nil)`）把方向钉在编译期 |
| 第三方实现放消费方包内 | 本 US 无新第三方；SQLite 驱动封装仍在 `storage`（既有）；`os.OpenRoot` 是标准库，`markdown` 载体实现直接在 `memory` 包内 |

**标签口径（`[US1]` 不混淆）**：`tasks.md` 中的 `[US1]` / `[US2]` / `[US3]` / `[US4]` 是 Spec-Kit 对**本 feature 内部**用户故事的编号（对应 [spec.md](./spec.md) 的四个故事：段 2 注入 / 两层作用域 / 载体两档与隔离 / 治理可判卷），**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）里的 US-1 / US-2 / US-3 / US-4——本 feature 的全部故事都落在项目 **US-6（Memory）** 一个模块内。故事 ↔ 测试文件面的分组是本 plan 自己的记账口径，**不改动**颗粒度文档任何内容（颗粒度文档按包分组、未按故事分组——spec.md 的 Notes 已声明）。

## Complexity Tracking

> 仅当 Constitution Check 存在需辩护的违规时填写。

无违规——本 feature 无 Constitution Check 不通过项，故无复杂度辩护条目。

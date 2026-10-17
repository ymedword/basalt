<!--
Sync Impact Report
==================
版本变化：1.1.0 → 1.2.0（2026-10-18；用户明确要求的开发工作流修订）
修改的原则：无；十大原则、架构约束、七目标与覆盖率底线保持原文。
修改的章节：开发工作流，将 tasks 固定人工停点替换为模块卡生成后与 US 收口时的
两处人工 review；中间由 AI 派生、分析、实施、验收与修复。plan review 由 AI 完成。
补充两层 Harness：公共集中维护，专属随 US 文档；机器通过不替代人工收口决定。
版本判定：MINOR，实质扩展工作流而未增删或重新定义原则。
配套同步：CLAUDE.md、DevelopmentLoop.md、TechnicalDesign.md、Roadmap.md、
harness/README.md 与 stage 模板、module-doc-gen / us-register / module-dev、
speckit-clarify / analyze / implement 的自动推进模式及续跑提醒 hook。
模板核对：constitution-template 解析成功，保留本仓现有原则 / 架构 / 工作流 / 治理结构。
待办：真实后续 US 的完整实现与两处人工 review，未声称已发生。

历史同步记录（保留）
==================
版本变化：1.0.0 → 1.1.0（2026-10-15 修订；用户裁决的四处修正 + 两处门禁补充）
修改的原则（措辞澄清与范围澄清，原则条数不变）：
- II 结果回填责任分界：原文「会话历史的追加只发生在 ReActLoop」范围过大——用户消息由
  AgentService 写入、崩溃 / 取消恢复必须补半截工具组，照原文执行会把两处既有正确行为
  判成违宪 → 改为「正常运行中由 ReActLoop 统一追加模型响应与工具结果；用户输入与恢复
  修复走各自入口；三处复用同一消息仓储，不得重复追加」
- VIII 依赖确认：补「技术栈表与已批准计划内的依赖可直接落地；表外新增坐标或版本变更先
  经用户确认」——原「新增第三方坐标先问用户」字面偏严，会把已裁决依赖重复询问
- X 核心阶段范围：原文「只交付运行时内核五大能力」过窄——管理台与观测、Agent 动态管理、
  每日 Demo、发布收尾已在既定 US 清单内，照原文判卷会把既定交付物判成越界 → 改为
  「以需求文档 §6 与已批准的 US 清单（台账）为准」
修改的章节：
- 架构约束 · 技术栈：Go 版本语义澄清——`go` 行是**最低版本要求**，准确构建版本由
  `toolchain` 指令与 CI 工具链配置固定（原文「go.mod 锁 go 1.27」易被读成准确版本锁定）
- 开发工作流：①覆盖率底线补可执行判据（US-1 交付按包阈值判失败的检查脚本；无语句包与
  无测试文件包的判定口径）；②原型五链路验证改为**分阶段执行**（与起步手册 §4 已决
  「口径丙」同步：US-1 小探针验链路 5，US-4 验链路 1/3/4/5，链路 2 的 HTTP 面到 US-9；
  契约在各链路验证前保持待验证设计草案，不构成 US-2 / US-3 的开工阻断）；③analyze 明确
  为**实施前阻断检查**（CRITICAL 未清零不进 implement，收口再跑一次）
新增原则 / 章节：无；删减原则 / 章节：无
配套同步（同批）：CLAUDE.md、docs/AiProgrammingGuide.md §3.2 / §3.3 / §3.4、
docs/GettingStarted.md §2.2 / §4 / §5.4 / §5.5 / §8、docs/TechnicalSolution.md §1.2、
docs/Roadmap.md §4.3（历史快照纪律按状态区分）、.claude/skills 三个编排 skill
（module-dev / us-register / init-foundation）

———— 首次采纳记录（1.0.0，保留） ————
版本变化：首次采纳 —— Version 1.0.0（Ratified 2026-10-15）
新增原则：I~X 十条（逐条对齐 docs/TechnicalSolution.md §1.1「关键决策」，编号与顺序一致）
修改的原则：无（首版）
新增章节：前言、架构约束（技术栈 / 依赖方向与包结构纪律 / 凭证纪律）、开发工作流、Governance
删减章节：无
开工前工件（已于 2026-10-15 全部落地）：
- CLAUDE.md（项目根，运行时指引）
- 四个编排 skill：/module-doc-gen、/us-register、/module-dev、/init-foundation
- US 台账 docs/us/REGISTRY.md（登记表零行，行随各 US 颗粒度文档逐行追加）
- speckit 层（.specify/ 与 10 个 speckit-* skill）
- docs/GettingStarted.md（起步手册：开发机环境 → git init → 逐 US 操作）
配套文档裁决状态同步（2026-10-15 先行落盘）：
- docs/DemandAnalysis.md §12.2：U-1 / U-3 / U-4 / U-5 / U-6 / U-8 六行「建议」→「已定（2026-10-15）」
- docs/TechnicalSolution.md：五处「（未决 U-N）」→「（已定 U-N）」
- docs/AiProgrammingGuide.md §3.1：补第 9 条「不用 ORM」、编号归一（Demo 行移出编号表）；§3.3 第 1 项勾选
复核方式：以上文件逐个 grep 复核（读数见本批交付报告）
-->

# Basalt Constitution

Basalt 是用 Go 实现的面向企业场景的 Agent OS：自托管单 Go 二进制，作为统一底座运行多个业务 Agent，共享渠道接入、模型路由、工具调用、记忆系统与沙箱执行。工作区及持久化数据由企业自托管；接入外部模型或工具时，相应请求数据按配置发送到服务端。

本宪法规定十条不可违背的核心原则与三条架构约束；每条都带「违反时的症状」与「复核方式（可验证）」，可在代码与命令层面判卷。所有实现（人类或 AI）动工前**必须**逐条对照。

## Core Principles

### I. 自实现 ReAct 循环

**Basalt 主程序不得将 Agent 框架作为直接或间接依赖引入。** 允许使用协议 SDK（如 MCP SDK），但 SDK 不得接管 ReAct 循环或自动执行工具。核心循环由本仓库代码实现（`ReActLoop`）。

- **违反时的症状**：框架接管循环，工具被调两次，或行为不可控、不可复现
- **复核方式（可验证）**：`go.mod` 不出现框架坐标（eino / langchaingo / adk-go / trpc-agent-go 等）；`ReActLoop` 循环体为仓库内代码
- **理由**：完全可控、可审计、可定制；框架会接管循环并引入版本与行为耦合

### II. LLM 客户端只做协议调用

**不得**启用 LLM 客户端的任何自动工具循环能力；调用面只到 Chat 请求 / 响应结构（含工具 schema 序列化）。工具调度的唯一归属是 `ToolExecutor`。结果回填与历史写入的责任分界：**ReActLoop 组织轮次，并在正常运行中统一追加模型响应与工具结果；ToolExecutor 执行工具、记录审计并返回结构化结果（不写历史）；用户输入由 `AgentService` 写入；崩溃 / 取消后的半截工具组由恢复路径补齐。上述入口必须复用同一消息仓储（`core` 定义的消息写入接口），不得出现第二套追加路径，不得重复追加同一消息。**

- **违反时的症状**：工具被客户端与执行器各调一次；或同一消息被两处重复追加；或半截工具组无人补齐（会话从此哑掉）
- **复核方式（可验证）**：调用面只有 Chat 请求 / 响应结构；装配处无 agent / runner 类 API；消息写入点收敛在一条仓储通道上——正常运行追加只发生在 `ReActLoop`、用户输入只发生在 `AgentService`、恢复只补半截工具组（清点消息写入接口的调用点，无第四处；`ToolExecutor` 不写历史）
- **理由**：工具被调两次是全项目最容易踩的坑；写入入口少而明确（一条仓储通道 + 三种各司其职的入口）才能既避免重复追加、又让崩溃恢复有唯一补写点

### III. Provider 必须显式映射

`provider name → 客户端实例` 的映射表**必须**在装配处一次构建；路由只查表。**不得**遍历推断、**不得**"取第一个"。

- **违反时的症状**：多 Provider 时路由错乱；Go map 遍历顺序**随机**，行为不可复现
- **复核方式（可验证）**：装配处无 `range` 选择逻辑；路由只查 map
- **理由**：多 Provider 并存时类型相同，扫描推断必然歧义

### IV. 一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露

`AGENT.md` frontmatter 即运行配置（含 `skills:` 声明），正文为任务指令。**一个目录 = 一个 Agent**。技能绑定**必须**由 frontmatter 的 `skills:` 列表声明（元素 = 公共技能库目录名，声明即绑定，各平台一致）；**不得**引入软连接 / 本地目录类绑定机制。技能按 L1（name + description + 技能库路径）→ L2（`read_file` 读 `SKILL.md` 正文）→ L3（附属资源按需再取）渐进披露；**不得**预载正文、**不得**新增 `use_skill` 工具。声明名在技能库中无实体 → **该 Agent 整只不注册**（WARN 点名）。

- **违反时的症状**：Agent 目录被当 Tool 注册，执行时报错；或技能卡片不出现而其它一切正常
- **复核方式（可验证）**：加载器有 `AGENT.md` 专项测试；`skills:` 缺实体 → 该 Agent 不注册
- **理由**：业务方零代码上线；杜绝平台差异（软连接类绑定机制在 Windows 不可行）

### V. 审计 day-one 且 fail-closed

审计表在 US-1 随 schema 建齐；写入自对应 US 起——`runs` 自 US-4（首次运行）、`llm_calls` 自 US-2（首次 LLM 调用）、`tool_invocations` 自 US-4（首次工具执行）。**模型调用与工具执行均必须先写入 pending 审计记录；预写失败不得发出调用。调用返回后更新终态，终态写入失败与崩溃恢复遵循运行时契约 §7.2。** **不得**以"日志够了"为由跳过落库。

- **违反时的症状**：扩展阶段审计无从落地；或"跑都跑了却没记录"
- **复核方式（可验证）**：任一 Demo 跑完查 SQL 有记录；审计表预写失败时调用**不得**发出（删审计写权限后调用应被拒而非照跑）
- **理由**：可审计是差异化能力，不是扩展阶段功能；两阶段写是 fail-closed 的实现方式（与运行时契约 §7.2 逐字一致）

### VI. 授权与执行分离（两接口）

**授权与受控命令执行分别通过 `Policy` 与 `CommandRunner` 接口完成；调用方不得直接依赖 `WhitelistPolicy`、`LocalRunner` 等具体实现。核心阶段由装配层显式选择这两个实现。** 签名以技术方案 §4.3 为准：`Policy.Check(ctx, action) error`；`CommandRunner.Run(ctx, cmd) (CommandResult, error)`。`CommandRunner` 负责 cwd、环境变量、输出上限、取消与进程树清理。升级（容器 / microVM）= 两接口各新增实现 + 装配切换，调用方零改动。**不得**把本条读成"所有工具都必须经过 `CommandRunner`"——它只承担受控命令执行（`SHELL_COMMAND`）；文件 / HTTP / 通知等动作经 `Policy` 授权后在各自执行路径落地。

- **违反时的症状**：升级要改多处；授权与执行职责糊在一起，无法单独替换
- **复核方式（可验证）**：调用方代码 grep 不到 `WhitelistPolicy` / `LocalRunner`（只在装配处与实现文件出现）；装配处显式选择实现；容器档 = 两接口各加实现 + 装配切换
- **理由**：升级承诺是"两个接口各新增实现 + 装配切换，调用方零改动"；"单接口不变"的旧承诺已作废

### VII. 同步执行 + context 只承载取消 / 截止 / 追踪

全程同步阻塞，goroutine-per-request；**不得**引入异步编排。`context.Context` **只承载取消、截止时间与追踪标识**；**逐处审查业务代码中的 `context.WithValue` 使用，只允许追踪标识；Profile、Session、RunEnv、ToolEnv 等业务状态必须显式传递。** **不得**用包级变量模拟 ThreadLocal。

- **违反时的症状**：隐式耦合、取消失效、并发串号（Go 无 ThreadLocal，塞进 context 等于把隐式依赖合法化）
- **复核方式（可验证）**：逐处审查业务代码中的 `context.WithValue`——只允许追踪标识；业务状态一律走显式类型化参数；grep 无包级可变状态存 Profile / Session；请求路径无自建 worker pool
- **理由**：Go 无 ThreadLocal，"显式传递"的载体是类型化参数而不是 context

### VIII. 单二进制纪律

**发布产物必须使用 CGO_ENABLED=0 构建，运行时代码和管理台静态资源随单 Go 二进制交付。核心底座无需外部数据库、缓存或消息队列；模型服务、显式配置的 MCP Server 及其运行环境属于按配置接入的业务能力，依赖条件应单独说明。竞态测试与发布构建分开执行；测试 job 可按 -race 要求启用 CGO。** 依赖最小化：**本表（架构约束 · 技术栈）与已批准计划（spec / plan / 模块执行颗粒度文档）内的依赖可直接落地**；表外的**新增第三方坐标或版本变更**先经用户确认。

- **违反时的症状**：交叉编译失败 / 目标机缺动态库；或 `-race` 与 `CGO_ENABLED=0` 互相打架
- **复核方式（可验证）**：`CGO_ENABLED=0 go build` 通过；发布产物按**各目标平台**分别验证——Linux 用 `ldd` / `file` 断言静态，macOS / Windows 验证无额外依赖即可启动；`-race` 只在测试命令 / job 中启用
- **理由**：交付形态是本项目的核心卖点；破例即是产品破位

### IX. 不用 ORM，手写 SQL

**不得引入 ORM，不得由 ORM 自动推断或修改数据库结构。允许启动时按 `schema_version` 执行已审查、版本化的显式 SQL 迁移；已发布迁移不得改写，只允许追加。** 建表用 schema 幂等脚本；repository 接口定义在 `core`、实现在 `storage`。

- **违反时的症状**：SQLite 迁移能力弱，ORM 自动迁移反复出坑；SQL 不可审计、不可优化
- **复核方式（可验证）**：`go.mod` 无 ORM 坐标；建表与迁移只在显式 SQL 脚本与 `schema_version` 记录里——复核**必须**覆盖建表脚本**与升级迁移脚本**，并逐版审查 `schema_version` 记录
- **理由**：显式 SQL 可审计、可优化；"只追加不改写"保证已发布版本的升级路径可推演

### X. 五大能力优先，范围卡紧

核心阶段范围**以需求文档 §7.1 与已批准的 US 清单（台账 `docs/us/REGISTRY.md`）为准**——运行时内核五大能力（Provider / ReAct 循环 / Memory / Tool 体系 / Web Service）之外，管理台与观测、Agent 动态管理、每日 Demo、发布收尾等**已登记在 US 清单内的交付物同样属于核心阶段**；企业级治理层（认证、多租户、审计查询、Tool Policy、SSO）放扩展阶段。完不成的功能挪到扩展阶段，**不得**砍验收。

- **违反时的症状**：功能蔓延，内核迟迟不完整；或反向误判——把已批准 US 清单里的交付物当成"超出五大能力"而拒做
- **复核方式（可验证）**：每 US 范围对照需求文档 §7.1 边界**与台账已登记行**；清单外功能一律进扩展阶段（Roadmap 或新增 US，先经用户裁决）；**不得**以"超出五大能力"为由判已批准的 US 交付物越界
- **理由**：先交付最小完整内核，企业级治理在真实使用数据验证后再做

## 架构约束（Architecture Constraints）

### 技术栈（2026-10-15 已全部裁决；版本以 `go.mod` / `go.sum` 为唯一事实源）

| 组件 | 选型 |
| --- | --- |
| 语言 / 工具链 | Go 1.27.2（构建版本）；`go.mod` 的 `go` 行写最低版本要求 `go 1.27` |
| 工程结构 | 单 Go module：`cmd/basalt` + `internal/` 分包（包数为起点，纪律在依赖方向与无环） |
| CLI | `spf13/cobra` |
| YAML / frontmatter | `goccy/go-yaml`（已定 U-3） |
| LLM 客户端 | `sashabaranov/go-openai`（已定 U-4；只做协议调用） |
| HTTP 服务 | 标准库 `net/http` + ServeMux 方法路由（已定 U-6） |
| 持久化 | `modernc.org/sqlite`（纯 Go、无 CGO）+ `database/sql` |
| 调度 | `robfig/cron/v3`（6 段制式；每任务时区经 `CRON_TZ=` 前缀） |
| MCP | `modelcontextprotocol/go-sdk`（已定 U-5） |
| 文件监听 | `fsnotify` |
| 日志 | 标准库 `log/slog`（JSON handler） |
| 测试 | 标准库 `testing` + `testify` |
| 记忆后端 | 核心阶段 markdown + sqlite 两档；mem0 接口预留（已定 U-8） |

（裁决明细与理由见技术方案 §1.2 与需求文档 §7.6；U-1~U-10 已于 2026-10-15 全部裁决。）

**Go 版本语义**（Go 1.21 起，[官方工具链文档](https://go.dev/doc/toolchain)、[go 指令语义](https://go.dev/ref/mod#go-mod-file-go)）：`go` 行声明的是**最低版本要求**——低于它的工具链会被拒绝，**它不是准确版本锁定**；准确构建版本由 **`toolchain` 指令**（`toolchain go1.27.2`）与 **CI 工具链配置**（`actions/setup-go` 的 `go-version: 1.27.2`）共同表达。本地工具链低于 `toolchain` 行时按 `GOTOOLCHAIN=auto` 切换 / 下载，高于它则照用。**不得**把 1.27.2 表述为"`go.mod` 锁定"。

### 依赖方向与包结构纪律

- 依赖方向无环：`channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 是唯一装配层
- `core` 只依赖标准库与内部接口；接口**必须**定义在消费方
- cron 等第三方库的适配**必须**放在独立内部包；`core` 保留接口与纯业务逻辑，由 `app` 完成装配
- 同类语义**不得**出现第二套实现（解析 / 注册 / 审计写入路径唯一）

### 凭证纪律

- 凭证（API key、webhook 地址等）一律 `${ENV_VAR}` 占位；明文**不得**落盘 / 落日志 / 进对话
- 通知渠道地址在库中只存 `${ENV_VAR}` 引用、发送时解析（解析失败点名环境变量、不静默）
- 凭证类操作由人执行

## 开发工作流（Development Workflow）

- **固定人工 review 仅两处**：AI 生成 US 颗粒度文档、自检与独立 AI review 后交人审核；通过后 AI 自动执行 `/us-register N` 与 `/module-dev N` 的 spec / clarify / plan / tasks、任务比对、analyze、Loop 实施 / 验收 / 修复，不设置登记、tasks 或实现前的固定人工确认。收口由 AI 复跑 analyze、独立复核并汇总证据，再交人审核 US 收口；未收到实际决定保持待审核。**`/speckit-analyze` 仍是实施前阻断检查**，CRITICAL 清零前不得实施。已审范围内的派生或实现问题由 AI 修复，实质范围 / 公共契约 / 权限 / 验收变更返回模块卡评审修订，不能伪造批准
- **两层 Harness**：公共规则与门禁集中维护、各轮复用，US 专属行为、故障与边界验收写入每份颗粒度文档，并由 AI 派生执行映射。公共门禁与专属验收均须通过，独立复核是审核方式，不另列第三层；人工收口与机器检查的结论分别记录
- **交付口径**：每个 US 收口以「有可演示 Demo + 门禁全绿」为准（**不**列为原则，是本节的工作流定义）
- **门禁**：`make check`（= `fmt lint sec test test-integration cover build` 七目标聚合）全绿是完成的定义。三条实测纪律：① `gofmt -l` / `gofumpt -l` **必须先查退出码、再判输出**（语法错误 → 非 0 退出；格式差异 → 退出 0 但输出非空）；② `-race` 需 CGO 开启，与 `CGO_ENABLED=0` 发布构建**分属不同命令 / job**；③ `-tags=integration` 默认不跑，**必须**单列
- **覆盖率底线（必须有可执行判据）**：`core` / `tool`（沙箱）/ `memory` / `storage` ≥ 70%，其余 ≥ 50%。`go test -cover` 只打印百分比、**不会**因低于阈值而失败——**必须**由 US-1 交付的覆盖率检查脚本（`make cover`，纳入 `make check`）按包解析结果并判失败。判定口径：**有可执行语句但无测试文件的包**（`no test files`）计 0%，未达标即失败，**不得**豁免；**无可执行语句的包**（`coverage: [no statements]`）跳过（无从测量，不计通过也不计失败）。安全关键路径（沙箱三档、凭证红线、审计落库）**必须**有专项测试，不靠覆盖率兜底
- **plan review 由 AI 执行，必查**：依赖方向无环 / 审计写入路径唯一 / 无框架坐标 / 无 `context.WithValue` 业务态 / 单二进制构建通过
- **原型五链路验证（2026-10-15 修订：分阶段执行，**不**阻塞后续 US 开工；裁决全文见起步手册 §4「已决（口径丙）」）**：①`go.mod` 坐标经 goproxy.cn 实测（US-1 内完成）；②链路 5（崩溃恢复 + 实例锁）由 US-1 小探针先验（不入库，内存 SQLite）；③链路 1 / 3 / 4 在 US-4 交付后立即验证（含链路 5 的真实实现复验）；④链路 2 的 HTTP 准入面（409 / 429）到 US-9 补齐。**契约状态**：运行时契约四件套在**对应链路**验证通过前均为**待验证设计草案**（状态头见各契约文件）；US-2 / US-3 在草案状态下开工属**预期安排**，**不构成硬前置阻断**。**最终冻结条件**：五链路全部通过 + 实测偏差回写 `contracts/`
- **提交纪律**：AI **不得**自动 commit / push / tag，提交时机由用户决定；提交信息 = Conventional Commits 标题 + 四段正文（Problem / Design / Testing / Impact）

## Governance

- 原则约束以经采纳的宪法为准；实现设计在四份权威文档之间以技术方案为准；数据结构与 API 固定字面量以对应契约为准。冲突涉及原则变更时，**必须**走宪法修订程序
- 宪法正式文本的修订**必须**经用户确认——AI **不得**自行修订；修订走 `/speckit-constitution`
- 版本策略：SemVer（MAJOR = 原则增删 / 重新定义；MINOR = 新增或实质扩展；PATCH = 措辞澄清）；每次修订同步更新顶部 Sync Impact Report
- 合规审查：每个 US 收口逐条自查十大原则与架构约束；`/speckit-analyze` 将宪法冲突判为 CRITICAL——**必须**调整 spec / plan / tasks，**不得**稀释原则
- 文档纪律：四份权威文档各司其职，**不得**互相"修正"；有冲突先裁决（技术方案最权威）再动手

**Version**: 1.2.0 | **Ratified**: 2026-10-15 | **Last Amended**: 2026-10-18

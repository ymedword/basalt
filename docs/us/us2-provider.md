# US-2 Provider 对接（能力一）— 模块执行颗粒度文档

> **文档状态**：草案（2026-10-16 生成；同日按用户评审修订 **R1**；同日按用户复核裁决修订 **R2**——两项待决全部落案，见「已决事项」#8 / #9；`/us-register 2` 登记、`/module-dev 2` 消费）
>
> **R1 修订要点（用户 2026-10-16 评审；逐项落案见文末「已决事项」）**：① 未知字段往返**不再依赖**库的 `ExtraFields`——v1.43.0 无此机制（已读源码确认，见 §3.1 尾注），改为客户端自建薄传输、两端保真，验收改**两跳**；② 调用错误在**客户端边界**先脱敏再交服务记录，假服务回显合成 key 并断言三路（返回错误 / 日志 / `error_message`）不含；③ 取消用例去掉 `time.Sleep`，改假 Provider 信号同步，另补「调用前 ctx 已取消」与「取消后终态写失败仍保留取消链」；④ 伪代码与类型定义对齐（预写态由仓储固定、token 三列显式填、模型解析空分支入伪代码并点名用例）；⑤ US-4 的协议面口径改为「**方法签名**沿用；请求字段与协议适配按已登记范围扩展并跑原有回归」；⑥ 真实验收的 token 判据按供应商是否返回 usage 分支，补 `base_url` 必填的独立用例、显式执行开关与截止时间。
>
> **R2 修订要点（用户 2026-10-16 复核裁决；两项原「待决事项」全部落案）**：① **SDK 角色收窄获批**——保持坐标与版本，自建 HTTP 层限定在 `provider` 内承担请求 / 响应 / 扩展字段透传；技术方案 §4.4 的透传承诺**保留**（只补实现方式），「唯一路径」措辞改为「**本 US 选定路线**」；② **`base_url` 改为装载期明确拒绝** userinfo / query / fragment——**不采用**"接受后静默丢弃"，新增第 8 类 fail-closed 规则（§3.5 改造点 1）+ 装载层与构造层两层拒绝及"文案不回显 URL"的用例（§3.2 / §4.3），发送层保留防御校验，正常 scheme / host / path 原样保留。**待决事项已清空**。
>
> **事实主源顺位（供 `/us-register 2` 派生登记）**
>
> 1. **`docs/TechnicalSolution.md`**（文档之间最权威）——§3.1 设计（`ProviderClient` / `ProviderService` 结构体逐字）/ §3.2 显式映射（硬约束 + `providers` 段样例）/ §3.3 核心阶段不做 / §4.1 ReAct 循环中的调用形状（`l.provider.Chat(ctx, env.llmEnv(i, prompt))`——本 US 的 `Chat` 签名依据）/ §4.4 完整消息模型（`Message` / `ToolCall` 逐字）/ §8.7 CLI 命令与轻·重路径 / §9.1 SQLite 装配（写池恒 1、事务不跨 LLM 调用）/ §9.2 表清单 / §9.4 审计模型 / §10.1 包结构与依赖方向 / §13 实施节奏（US-2 行：任务、可演示成果「一次真实 LLM 调用 + `llm_calls` 记录」、涉及包 core / provider / storage / app / cli）
> 2. **`docs/DemandAnalysis.md`**——§1.5 实施节奏（US-2 定位与依赖序）/ §5.3 能力一：对接 LLM / §5.12 命令行工具（`basalt provider list  # 列 Provider 及配置状态`）/ §5.13 配置与密钥 / §13.4 质量门禁验收
> 3. **`.specify/memory/constitution.md`**（原则级最高）——原则 I（自实现循环，框架不得接管）/ II（LLM 客户端只做协议调用）/ III（Provider 显式映射）/ V（审计 day-one 且 fail-closed，`llm_calls` 自 US-2 起）/ VII（context 只承载取消 / 截止 / 追踪）+ 架构约束（技术栈与依赖最小化、包与依赖方向、凭证）+ 开发工作流（门禁、覆盖率判据）
> 4. **`docs/contracts/`**——`schema.sql` 的 `llm_calls` DDL（表 / 列 / 状态字面量唯一事实源；本 US 首次写入）；`run-and-errors.md` §7.1 三表分工 / §7.2 审计 fail-closed 两阶段写与三个残余窗口 / §7.3 `error_kind` 七类 / §7.4 存什么不存什么（`error_message` 截断 4 KB）/ §4.2 审计写入与取消解耦（`context.WithoutCancel` + 独立短超时）/ §8 错误码表（`NOT_CONFIGURED` 的语义位）
> 5. **附加事实源**——`docs/Compatibility.md` §2.4 运行行为（审计写入策略相对基线为**有意改变**：NivroOS best-effort → Basalt fail-closed）/ §2.8 包结构对照（`internal/provider` ↔ nivroos-provider）；`docs/EngineeringStandards.md` §1 Go / §2 测试 / §3 数据库（审计两阶段写三分支）/ §4 日志与错误（级别约定：预写失败 = ERROR、失败路径 = WARN）
> 6. **落地参考（非权威；冲突以技术方案为准）**——`docs/AiProgrammingGuide.md` §4.2 US-2 拆解（核心目标 / 涉及包 / task 大类 / 验收 Demo）/ §5 七项完成定义 / §7 陷阱表（#23 丢 `tool_calls` / `reasoning_content` → 400；#25 先执行后写审计；#29 事务跨 LLM 调用；#40 YAML 库错误文案脱敏）/ §8.2 纪律 / §8.3 工程治理八项（第 2 项：固定假 Provider 随 US-2 接口落位；第 5 项契约同批更新；第 6 项 CI 与审查排序）
>
> **固定裁决句**：本文与最新技术方案冲突时，**以技术方案为准**；本仓配套工件（spec / plan / 已批准 US 清单）已锁定的引用在不与前者冲突时沿用，不另行改判。**spec 锚点现状（核对于 2026-10-16）**：`specs/` 下现有 `001-foundation/`（US-1 已完成验收的历史设计，**只读**——本规格不得回改它）；US-2 的 spec 目录由 `/module-dev 2` 新建。与本 US **无冲突**，且有两条正向引用：① `specs/001-foundation/data-model.md` 对 `providers` 段写明「**解析并保留**（结构进 `*Config`），语义校验随消费 US（**US-2**）」——正是本文 §3.5 前序改造点 1 的授权；② 同目录 `plan.md` 记「provider 包本 US 仅 `doc.go` 占位（**无语句，覆盖率跳过**）」——正是本文 §2.2 约束 10 的原始读数来源。
>
> **登记派生信息**：技术方案章节 = §3.1 / §3.2 / §3.3 / §4.1 / §4.4 / §8.7 / §9.1 / §9.2 / §9.4 / §10.1 / §13；需求文档章节 = §1.5 / §5.3 / §5.12 / §5.13 / §13.4；模块落位（摘要）= `internal/{core,provider,storage,app,cli,testutil}`——技术方案 §13 的 US-2「涉及包」列为 core / provider / storage / app / cli，**`testutil` 为该列之外的测试基础设施落点**（`FakeProvider` 随本 US 接口落位，编程指南 §8.3 第 2 项；与 US-1 登记行含 `testutil` 同口径）；`go.mod` 新增坐标一处（go-openai v1.43.0）。
>
> **台账状态**：登记时 `docs/us/REGISTRY.md` 查无 US-2 行——本章节对照由本 skill 自定位，供 `/us-register 2` 派生。

## 1. 模块概述

**定位**：把 Basalt 与 LLM 供应商之间的一次真实调用打通——配置装载（US-1 已交付）→ 装配处**显式映射表**一次构建 → 一次 Chat 调用（协议翻译 + usage 解析）→ 调用**成败都写** `llm_calls` 审计（fail-closed 两阶段写）；并交付 `basalt provider list`（列 Provider 及配置状态）。

**价值（WHAT / WHY）**：US-2 是五大能力里**第一个真实外部依赖面**（需求 §1.5：`US-1 → US-2 → …`，每个 US 只消费左侧已交付的能力）。它一次交付三件可判卷的事：① 显式映射表落地（宪法 III 从条文变成可 grep 的结构与装配函数）；② 审计从"有表"变成"有写入"——`llm_calls` 自本 US 起（宪法 V 的时序定义、编程指南 §4.2），两阶段写把 fail-closed 从契约文本变成可单测的执行顺序；③ 供应商错误第一次有了类别（`error_kind`），审计从"有记录"迈向"能解释失败"（技术方案 §9.4）。同时它是**循环之前的最后一个底座零件**——US-4 的 `ReActLoop` 消费本 US 的 `ProviderService.Chat`（技术方案 §4.1 的调用形状即本 US 的**方法签名**，签名沿用）；**请求字段与协议适配不冻结**：`Tools` / `Temperature` 等按 US-4 已登记范围扩展，并跑本 US 全部回归（§3.5 占位 3）。规模参考：约 8~10 个实现文件 + 对应测试（编程指南 §4.2「接口 + 一个实现 + 一个命令」）。**硬依赖 US-1**；本 US 不消费 US-1 之外的任何未交付能力。

## 2. 设计要点与约束

### 2.1 职责边界表

| 维度 | 本 US 职责（做） | 非本 US 职责（不做，归属） |
| --- | --- | --- |
| Provider 抽象 | `core` 声明 `ProviderClient` 接口 + 统一请求 / 响应 / 消息模型（§3.1） | 工具 schema 与工具列表组装（US-4 / US-5）；`ToolCalls` 的执行（US-4） |
| OpenAI 兼容实现 | `provider.OpenAIClient`：协议翻译（发出）+ 响应解析（usage 可空兜底）+ 错误归类 | 流式 / embedding / 图片 / 语音（技术方案 §3.3）；Anthropic 原生协议适配（扩展阶段） |
| 显式映射 | `app.newProviderClients` **逐条**构造 `name → 客户端`，`app.newProviderService` 装配服务 | 映射的热重载（改为重启生效；热加载语义归 US-11） |
| 调用与审计 | `ProviderService.Chat`：查表路由 → **先插 `pending`** → 发出 → 终态回填 + `error_kind` | Run 级预算 / 并发准入 / 取消判定（US-4 的 `AgentService` + `ReActLoop`）；`config_hash` 填充（US-4 / US-11） |
| llm_calls 仓储 | `storage.LLMCallRepo`：`BeginCall` / `FinishCall` 两条手写 SQL（表 US-1 已建齐） | 恢复扫描（`pending` → `unknown`，契约 §4.3.2；US-4）；审计查询面（扩展阶段） |
| 命令面 | `basalt provider list`（轻路径：只读配置、不开库、不取实例锁） | `chat` / `serve` / `gateway`（US-4 / US-9 / US-10）；其余 9 条命令 |
| 测试基础设施 | `testutil.FakeProvider`（`core.ProviderClient` 假实现，编程指南 §8.3 第 2 项「随 US-2 接口落位」）+ `testutil.FakeOpenAIServer`（OpenAI 兼容假服务，回环） | MCP 测试服务（US-7）；mock 模型 e2e 发布闸门（US-14，`//go:build e2e`） |
| 真实调用验收 | 仓库内 `//go:build integration` 用例（env 门控，缺凭证即 `t.Skip` 写明理由），人执行 | 每日 Demo（US-4 起有 `chat` 后才成立）；发布前 mock e2e（US-14） |

### 2.2 关键约束清单（每条对应宪法条款 / 契约，带症状或判据）

1. **Provider 必须显式映射**（宪法 III）：装配处**逐条**由配置构造 `map[string]ProviderClient`，路由**只查表**（`clients[req.Provider]`）；**不得**遍历（Go map 遍历顺序随机 → 同一进程不同请求指到不同供应商）、**不得**推断、**不得**把"唯一的那个"当默认。配置里 `providers[].name` 重复 → **装载期拒绝**（否则 map 静默保留最后一个，缺失条目无人发现）。
2. **LLM 客户端只做协议调用**（宪法 I / II）：`go-openai` 的角色收窄为**协议类型与已知字段的编解码**（请求信封基座 / 消息 / `ToolCall` / `Usage` / 错误体），HTTP 往返由 `provider` 自建薄传输（`net/http` + `encoding/json`）——原因见 §3.1 尾注（源码核实：库的编解码只处理已知字段，未知字段**两端都会被丢**；库亦未开放 chat 的原始 JSON 通道）。**不得**启用任何自动工具循环、**不得**让库接管会话历史或工具调度；`ProviderService.Chat` 是**一次**调用，不做重试、不做 fallback（技术方案 §3.3）。契约 §3.3 第 1 条的"SDK 传输层重试"例外**本 US 不被触发**（自建传输只发一次 POST：该条是"允许"不是"要求"）；重定向**不得**跟随（`CheckRedirect` 直接返回 `http.ErrUseLastResponse`，3xx 按 `external_error` 记审计——避免凭证随重定向跨主机转发，也不把一次调用变成两次请求）。本仓**不得**在应用层加隐式重试。响应里的 `ToolCalls` **只透传**给调用方，执行权归 US-4 的 `ToolExecutor`。
3. **审计 day-one 且 fail-closed**（宪法 V + 契约 §7.2）：`llm_calls` **自本 US 起写入**——调用**前**先插 `status=pending`，**插入失败即拒绝发出该次调用**；调用返回后更新为终态（`success` / `failed`）。**终态更新失败**：记 ERROR、行**保留 `pending`**、**不得**伪造终态，由下次启动的恢复扫描标 `unknown`（契约 §7.2 残余窗口；恢复扫描本体归 US-4）。**两次写入都在解耦上下文里做**（`context.WithoutCancel` + 独立 5s——第 7 条）：被取消的运行同样要留下"这次调用存在过"的证据。症状：删掉审计写权限后调用照常发出 = 违反。
4. **审计关联标识传导**（易漏维度）：`run_id` / `session_id` / `round` 随 `ChatRequest` **显式**传入并落 `llm_calls`；`run_id` 为空或 `round < 1` → **拒绝发出且不写审计行**（用法错误，非调用失败）。`run_id` 由 US-4 的 `AgentService` 生成；本 US 由调用方（测试 / 演示）显式提供（§2.4 裁决注 4）。
5. **留痕顺序不得颠倒**：`BeginCall` 必须早于对客户端的发出。判据可执行——假 Provider 在 `Chat` 内**当场**查询库中是否存在该次调用的 `pending` 行（`TestProviderService_AuditPendingBeforeCall`）。
6. **「无行 = 确定没执行」**（契约 §4.4 崩溃窗口表）：路由失败 / 关联缺失发生在预写之前 → **不写审计行**；反之，只要调用被发出过，就一定有 `pending` 行（第 3 条保证）。**不得**为"未发出的调用"合成审计行——那会污染恢复判据。
7. **context 纪律**（宪法 VII）：`ctx` 只承载取消 / 截止 / 追踪标识；关联字段、客户端映射、模型映射、仓储、日志器一律**显式类型化参数**；`context.WithValue` 零使用（收尾 grep）。**审计的两处写入（预写与终态回填）一律用 `context.WithoutCancel(ctx)` + 独立短超时（缺省 5 秒）**（契约 §4.2 原文主语是"审计写入"**整体**，不止终态；数值 5 秒亦取自该条）。两条理由：① 被取消的调用同样要落审计——"为什么停了"正是最需要证据的时刻；② 预写解耦消掉一个歧义——**预写失败必须意味着"审计真的不可用"，而不是"ctx 恰好已取消"**，否则取消会伪装成 fail-closed 拒发。取消路径上的审计失败只记 ERROR，**不得**反向覆盖调用结果。
8. **凭证纪律**（宪法·架构约束 + 需求 §5.13）：`api_key` 只从**已解析的配置**（US-1 的 `cli.LoadConfig`，`${VAR}` 占位经注入的 `lookupEnv`）取；凭证值**不得**出现在错误文案、日志、`provider list` 输出、审计行的任何列里。`llm_calls` **不存**凭证（契约 §7.4：存模型与供应商、token、耗时、失败原文，不存 key）。**脱敏责任落在客户端边界**——它是唯一持有 key 的地方：错误文案离开 `OpenAIClient` 前先抹除 key 回显（`ReplaceAll`）、截断到 4 KB、**不携带请求 URL 与响应正文全文**（只带供应商名、HTTP 状态与服务器给的 `error.message`）；服务照原样记录它拿到的错误（服务不持有 key，无从脱敏——所以脱敏**必须**在内层做完）。地址面同理：`base_url` 含 userinfo / query / fragment 在**装载期即被拒绝**（不静默裁剪——用户配置的含义不得被改写，§2.4 注 9），装载层与构造层的文案都只点名 `providers[i].base_url` 与不支持的成分，**不回显原 URL、也不回显其中的值**（`url.Parse` 的原始错误自带 URL，**不得**直传）；`provider list` 展示层再按"只留 scheme://host/path"渲染（纵深防御，不依赖上一层已拦干净）。**判据可执行**：假服务**回显合成 key** → 返回错误、日志缓冲、`llm_calls.error_message` 三路 `NotContains`（`TestProviderService_ProviderErrorEchoingKeyNeverLandsInAuditOrLogs`）；`base_url` 三类成分 → 装载期拒绝且文案不含原 URL（`TestConfig_RejectsProviderBaseURLWithUserinfoQueryFragment`）。
9. **依赖最小化 + 坐标先核实**（宪法 VIII）：本 US 新增坐标**唯一一个**——`github.com/sashabaranov/go-openai v1.43.0`（在固定技术栈句与已批准计划内，**直接落地**，不重复询问）。**本轮已核实**（2026-10-16）：`GOPROXY=https://goproxy.cn go list -m -versions github.com/sashabaranov/go-openai` 输出末位 `v1.43.0`、退出码 0。无 `import _` 副作用注册；`go.mod` / `go.sum` 落地后为唯一事实源。
10. **`core` 只放声明——首要理由是职责划分，覆盖率门槛是它的后果**（技术方案 §10.1 三条包纪律 + 宪法·开发工作流）：`core` 是**契约层**——依赖倒置后各包共用的类型、接口、常量、哨兵，即"谁需要看见谁"的最小面。实现不进 `core` 是包纪律本身的要求（`core` 不得 import 任何 internal 包与第三方，它放不下实现——放得下就没有倒置可言了），不是为躲门槛挑的落点。覆盖率门槛是同一件事的第二重体现：阈值 `core` / `storage` ≥ 70%、`provider` / `app` / `cli` ≥ 50%；`core` 一旦出现可执行语句即纳入 70% 判卷，而"只做转发的 `core` 实现"会**同时**违背依赖方向与判卷口径。因此本 US 的落法：**类型 / 接口 / 常量 / 哨兵进 `core`（零语句），一切逻辑进 `provider` / `storage`**（各自 ≥ 50% / ≥ 70%，专项测试充足）。**本轮实测（2026-10-16 复跑，读数见 §6）**：`internal/core` 与 `internal/provider` 当前记「无可执行语句」；本 US 给 `provider` 加实现后它**纳入 ≥ 50% 判卷**。若后续 US 必须在 `core` 放实现，**必须同批**补 `internal/core` 的测试文件（有语句无测试文件 = 计 0% 判失败，不得豁免）。
11. **错误分类齐全**（契约 §7.3）：每次**失败**的调用都必须落 `error_kind`（七类之一），**不得**只留自由文本；"为什么失败"与"失败了"是两回事。本 US 的取值面**三类**：`external_error`（供应商 4xx/5xx、网络不可达 / DNS / TLS、响应不可解析、3xx 未跟随）、`timeout`（调用被取消或截止——**归并**口径见「已决事项」**原待决** #1：`context.Canceled` 与 `DeadlineExceeded` 合并记 `timeout`，但两者的**错误链区分保留**，`errors.Is` 仍可判）、`unknown`（**兜底**：既非客户端产出的安全错误、错误链上也无 ctx 错误的异常错误——例如第三方 `ProviderClient` 实现抛回的裸错误）。**`unknown` 不是恢复路径专属**（契约 §7.3 举的例子只是它的主要来源）；反过来，恢复扫描（US-4）把孤儿 `pending` 行标 `unknown`，与本条的兜底**同值不同因**——审计要分开这两类时按 `status` 判（`failed` vs `unknown`），不靠 `error_kind` 单独判。
12. **失败原文截断**（契约 §7.4）：`error_message` 落库前截断 **4 KB**；截断**不得**切断半个 UTF-8 字符（否则库内出现非法文本、管理台与导出全线踩雷）。截断责任在 `storage`（列的家）——单点、可单测。
13. **可观测性双轨**（EngineeringStandards §4）：审计轨与日志轨**都要**。级别：**预写失败** = ERROR（服务降级，需人介入）、**终态更新失败** = ERROR（残余窗口）、**调用失败** = WARN（失败路径降级）。日志字段固定键（`provider` / `model` / `run_id` / `error` / `error_kind`），**不得**把错误文本拼进消息主体、**不得**记凭证、**不得**记全量 URL。
14. **命令面轻路径**（技术方案 §8.7 + 需求 §5.12）：`provider list` 只读配置文件——**不开库、不取实例锁、不做恢复、不构造客户端**；不涉及注册表或数据库故属轻命令。退出码口径沿用 US-1：成功 0 / 失败 1，原因由文案区分。`providers` 段为空或缺配置文件 → **失败**（§2.4 裁决注 2）。
15. **包与依赖方向**（宪法·架构约束 + 技术方案 §10.1）：`core` 不 import 其他 internal 包、不 import 第三方（`go-openai` 只在 `provider`、SQL 只在 `storage`）；接口定义在消费方——`core.ProviderClient` 由 `provider` 实现并由 `provider.ProviderService` 消费，`core.LLMCallStore` 由 `provider` 消费、`storage` 实现（本仓既有口径：仓储接口 `core` 定义 / `storage` 实现）；无环由 CI 用 `go list` 复核。
16. **消息模型不可裁**（技术方案 §4.4 + 编程指南 §7 陷阱 23）：`Message` / `ToolCall` 的字段与 JSON 名**逐字**保真（`tool_calls` / `tool_call_id` / `reasoning_content` / 供应商扩展透传位）——本 US 做的是协议**往返**，任何字段在第一跳就丢掉，US-4 的思考模式会话会持续 400 且不可恢复。**"丢"发生在库的编解码里，不在我们的类型里**：v1.43.0 的消息类型只 marshal / unmarshal 已知字段，`ProviderExtra` 若经它进出发送即静默消失——故出站由 `encodeMessages` 平铺未知键、入站由 `parseChatResponse` 收集未知键（§3.1）。**验收必须走两跳**：假服务返回未知字段 → 客户端解析进 `ProviderExtra` → **下一次请求体仍含该字段**；仅对 `core.Message` 做一次 JSON 往返**检不出库丢字段**，不构成该条的验收。

### 2.3 核心逻辑

本 US 核心逻辑为**四条流程**：A 装配（配置 → 显式映射 → 服务）、B 一次调用（两阶段写）、C `basalt provider list`、D 失败与边界矩阵。伪代码方法名与 §3 交付物清单**逐字一致**。

#### 流程 A — 装配（`internal/app`）

```text
app.newProviderService(cfg, store, logger)
  │
  ├─ newProviderClients(cfg)                       # 宪法 III：逐条显式构造，不遍历推断
  │    for i, entry := range cfg.Providers:
  │        client, err := provider.NewOpenAIClient(provider.Options{
  │            Name: entry.Name, BaseURL: entry.BaseURL, APIKey: entry.APIKey })
  │        clients[entry.Name] = client            # 键 = 配置条目名（重名已在装载期被拒——§3.5 改造点）
  │        models[entry.Name]  = entry.DefaultModel
  │
  └─ provider.NewProviderService(clients, models, store, logger)
```

```go
// 伪代码（方法名与 §3 逐字一致）
func app.newProviderClients(cfg *cli.Config) (map[string]core.ProviderClient, map[string]string, error):
    clients := make(map[string]core.ProviderClient, len(cfg.Providers))
    models  := make(map[string]string, len(cfg.Providers))
    for i, entry := range cfg.Providers:                   // 遍历的是**配置数组**（有序、显式）
        client, err := provider.NewOpenAIClient(provider.Options{...})
        if err != nil: return nil, nil, err                // 点名 providers[i]
        clients[entry.Name] = client                       // 路由只查表（宪法 III）
        models[entry.Name] = entry.DefaultModel
    return clients, models, nil

func app.newProviderService(cfg *cli.Config, store core.LLMCallStore, logger *slog.Logger) (*provider.ProviderService, error):
    clients, models, err := app.newProviderClients(cfg)
    if err != nil: return nil, err
    return provider.NewProviderService(clients, models, store, logger), nil
```

> **消费方时序**：本 US 的装配函数**无生产调用方**——它的第一个生产消费者是 US-4 的 `serve` / `chat` 装配（占位点见 §3.5）。本 US 的验收演示经 `//go:build integration` 用例调用同一条装配路径（配置 → 映射 → 调用 → 审计），保证"交付即可用"，而不是"到 US-4 才第一次被编译进来"。

#### 流程 B — 一次调用（两阶段写；伪代码方法名与 §3 逐字一致）

```text
ProviderService.Chat(ctx, req)
  │
  ├─ 1. 路由（查表，不遍历）
  │       client, ok := s.clients[req.Provider]
  │       !ok → 点名错误 core.ErrProviderNotConfigured（含 provider 名）        # 无审计行（「无行 = 确定没执行」）
  │
  ├─ 2. 关联与模型解析（三道拒绝都在预写之前 → 无审计行）
  │       req.RunID == "" 或 req.Round < 1 → 点名错误                            # 无审计行
  │       req.Model = s.resolveModel(req)      # 请求模型 > 供应商缺省模型
  │       req.Model 仍为空 → 点名错误（点名 provider）                             # 无审计行
  │
  ├─ 3. 预写（留痕先于发出；契约 §7.2 第 1 步）
  │       beginCtx, cancelBegin := auditContext(ctx)        # WithoutCancel + 独立 5s——契约 §4.2
  │       callID, err := s.store.BeginCall(beginCtx, core.LLMCall{run_id, session_id, round,
  │                                                              provider, model, config_hash, created_at})
  │         # 状态列由仓储**固定写 'pending'**——预写形态不带状态字段（不由调用方给）
  │       err != nil → ERROR 日志 + **拒绝发出** + 返回错误（审计不可用比调用不可用更严重）
  │
  ├─ 4. 发出（一次 POST、不重试、不跟随重定向）
  │       start := time.Now()
  │       resp, chatErr := client.Chat(ctx, req)                # 协议翻译 + 错误脱敏都在客户端内
  │       duration := time.Since(start)
  │
  ├─ 5. 终态回填（同一把解耦上下文——取消路径也要落终态）
  │       chatErr == nil → FinishCall(callID, {Status: success, 三 token 列, Duration})
  │       chatErr != nil → kind := classifyError(chatErr, ctx.Err())
  │                        FinishCall(callID, {Status: failed, ErrorMessage: chatErr.Error(),
  │                                              ErrorKind: kind, Duration})
  │       FinishCall 失败 → ERROR 日志 + 行**保留 pending**（不伪造终态；恢复标 unknown 归 US-4）
  │
  └─ 6. 返回 resp / chatErr（不吞错、不二次包装消费方语义——Run 状态判定归 US-4 的循环）
```

```go
// 伪代码（方法名与 §3 逐字一致）
func (s *ProviderService) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error):
    client, ok := s.clients[req.Provider]                        // 只查表
    if !ok: return core.ChatResponse{}, fmt.Errorf("%w: %s", core.ErrProviderNotConfigured, req.Provider)
    if req.RunID == "" || req.Round < 1: return …, 关联标识错误   // 无行：调用未发出
    req.Model = s.resolveModel(req)                              // 请求模型 > 供应商缺省模型
    if req.Model == "": return …, 模型缺失错误                    // 无行：调用未发出（点名 provider）
    beginCtx, cancelBegin := auditContext(ctx)                   // 契约 §4.2：审计写入与取消解耦
    callID, err := s.store.BeginCall(beginCtx, core.LLMCall{…, CreatedAt: time.Now()})
    cancelBegin()                                               // 预写形态不含状态列——仓储固定写 'pending'
    if err != nil: s.logger.Error(…); return core.ChatResponse{}, err       // 拒绝发出
    start := time.Now()
    resp, chatErr := client.Chat(ctx, req)                       // 客户端已脱敏（约束 8）
    auditCtx, cancel := auditContext(ctx)
    defer cancel()
    if chatErr == nil:
        s.finish(auditCtx, callID, core.LLMCallResult{Status: core.LLMCallSuccess,
            PromptTokens:     resp.Usage.PromptTokens,              // 三列**显式**填：列是平的，
            CompletionTokens: resp.Usage.CompletionTokens,          // 结构体也平——不嵌 Usage
            TotalTokens:      resp.Usage.TotalTokens,               // （可空兜底见约束 11 / §3.1）
            Duration:         time.Since(start)})
    else:
        s.finish(auditCtx, callID, core.LLMCallResult{Status: core.LLMCallFailed,
            ErrorMessage: chatErr.Error(),                          // 原样记录——脱敏在内层做完
            ErrorKind:    classifyError(chatErr, ctx.Err()),
            Duration:     time.Since(start)})
    return resp, chatErr

// auditContext 是审计写入的上下文（契约 §4.2：WithoutCancel + 独立短超时）——
// 预写与终态回填**共用**，单点定义，避免两处各写一份而漂移。
func auditContext(ctx context.Context) (context.Context, context.CancelFunc):
    return context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)   // 缺省 5 秒

// finish：终态回填的唯一出口——失败只记 ERROR，绝不 panic、绝不覆盖调用结果
func (s *ProviderService) finish(ctx context.Context, id int64, result core.LLMCallResult):
    if err := s.store.FinishCall(ctx, id, result); err != nil:
        s.logger.Error("审计终态回填失败——行保留 pending，待启动恢复标 unknown", "id", id, "error", err)
```

#### 流程 C — `basalt provider list`（轻路径）

```text
basalt provider list（不取实例锁、不做恢复、不开库——技术方案 §8.7 轻命令口径）
  │
  ├─ cli.LoadConfig(<WorkDir>/basalt.yaml, os.LookupEnv)     # US-1 已交付：占位解析 + 六类 fail-closed
  │     ├─ 文件缺失 → 「未发现配置（路径明示）——先创建 basalt.yaml 并配置 providers 段」→ exit 1
  │     ├─ 配置无效 → 原错误上抛（已点名键路径与变量名，不含凭证值）        → exit 1
  │     └─ providers 段为空（零条目）→ 「providers 段为空——至少配置一家 Provider」→ exit 1
  │
  ├─ 逐条打印：name / redactedBaseURL(base_url) / default_model + 凭证状态文案
  │     # redactedBaseURL：只留 scheme://host/path（§2.4 裁决注 9）
  │     # 纵深防御：含 userinfo / query / fragment 的配置在装载期已被拒绝，
  │     #           展示层仍按收窄规则渲染——不依赖上一层的拦截（本层可被直接注入构造）
  │     # 凭证状态文案只报"已配置 / 未上送"，**值不回显**（约束 8）
  └─ 汇总行 + exit 0
```

#### 流程 D — 失败与边界矩阵（判据表，harness 逐行对号）

| 情形 | `llm_calls` 行 | `status` | `error_kind` | `Chat` 返回 | 日志 |
| --- | --- | --- | --- | --- | --- |
| 未注册的 provider（查表不中） | **无**（未发出） | — | — | 错误（点名 provider） | WARN |
| `run_id` 为空 / `round < 1` | **无**（未发出） | — | — | 错误（点名字段） | WARN |
| 模型无法解析（请求与缺省模型都空） | **无**（未发出） | — | — | 错误（点名 provider） | WARN |
| `providers[]` 条目未过装载期校验（缺 `base_url` / 重名 / `base_url` 含不支持成分） | **不适用**（客户端未构造，`Chat` 不可达——失败点在使用之前） | — | — | — | — |
| 预写失败（审计不可用） | 无（插入失败） | — | — | 错误 | **ERROR** |
| 调用成功 | 有 | `success` | NULL | 响应 + nil | — |
| 供应商 4xx / 5xx | 有 | `failed` | `external_error` | 错误（不吞） | WARN |
| 网络不可达 / DNS / TLS 失败 | 有 | `failed` | `external_error` | 错误 | WARN |
| 供应商回 3xx | 有 | `failed` | `external_error` | 错误（**不跟随**重定向——约束 2） | WARN |
| 响应不可解析（畸形 JSON） | 有 | `failed` | `external_error` | 错误 | WARN |
| 供应商错误正文**回显了 key** | 有 | `failed` | `external_error` | 错误（**已脱敏**：错误 / 日志 / `error_message` 三路均不含 key） | WARN（字段同样不含） |
| 调用被取消 / 截止时间到 | 有 | `failed` | `timeout`（归并；`Canceled` / `DeadlineExceeded` 链保留） | 错误（`errors.Is` 可判具体 ctx 错误） | WARN |
| **调用前** ctx 已取消 | 有（预写已与取消解耦，行照落） | `failed` | `timeout` | 错误（链保留 `context.Canceled`） | WARN |
| 错误无法归类（异常错误 / 第三方客户端裸错误） | 有 | `failed` | `unknown`（**兜底**，约束 11） | 错误 | WARN |
| 供应商未返回 usage | 有 | `success` | NULL | 响应（三 token 列 0） | — |
| 终态回填失败 | 有（**留 `pending`**） | — | — | 按调用结果返回（**取消路径亦然——不覆盖取消链**） | **ERROR** |

### 2.4 差异裁决注（技术方案未逐字段规定处的解读与出处；不引入文档之外的概念）

1. **`ChatRequest` 的字段集与"谁读哪些字段"**：技术方案只给出调用形状（§4.1 `l.provider.Chat(ctx, env.llmEnv(i, prompt))`——**一个**请求值）与接口签名（§3.1 `Chat(ctx, req ChatRequest)`），未逐字段列出 `ChatRequest`。本文按该形状把它分成**两组**：路由 / 审计关联组（`Provider` / `RunID` / `SessionID` / `Round` / `ConfigHash`——`ProviderService` 消费）与协议内容组（`Model` / `Messages`——`ProviderClient` 消费）。`ProviderClient` 实现**只读协议组**；两组同处一值是 §4.1 单参数调用形状的直接结果，不是新概念。`Tools`（工具列表）**不在此列**：它的类型与组装归 US-4（§3.5 占位 1）。
2. **`provider list` 的失败语义与 `status` 的容忍语义刻意不同**：US-1 已决「`status` 无配置不计失败」（诊断报告必须能在裸工作区出结果）。`provider list` 的语义是"列出**已配置**的 Provider"——无配置即无输出对象，属命令目的未达成：**缺配置文件 / `providers` 段为空都计失败（exit 1）**，文案点名路径与段名。两处口径不一致是**有意的**，写在此处防"顺手统一"。
3. **`config_hash` 在本 US 恒为空**：列随 schema 建齐（US-1），但内容寻址的配置快照机制属 US-4（运行内冻结）/ US-11（热加载）。本 US 预写时该列写 NULL（**不是空串**），待快照机制落位后填充；审计行"哪版配置"的完整性由 US-4 收口。
4. **本 US 的 `run_id` 由调用方显式提供**：`llm_calls.run_id` 为 `NOT NULL`（契约 schema.sql），而 `runs` 表的第一行要到 US-4 才写入——本 US 的集成演示用**合成 run_id**（形如 `demo-<UTC 时间戳>`），明示为演示数据；真实 `run_id` 由 US-4 的 `AgentService` 生成后经 `RunEnv.llmEnv` 注入。`llm_calls` 对 `runs` **无外键**（契约 DDL 如实），故合成 id 不破坏引用完整性。
5. **`core` 只放声明，实现进 `provider`**：见约束 10——**首先**是职责划分（`core` 是契约层：各包共用的类型 / 接口 / 常量 / 哨兵；实现属实现包，这也是包纪律的直接结果），覆盖率门槛只是同一件事的后果（`core` ≥ 70% 的门槛在 `core` 出现第一条语句时即生效）。分类函数、协议翻译、URL 处理一律落 `provider`。
6. **`ProviderService` 结构体比技术方案 §3.1 的字面多两个装配期字段**（软门禁项，在此报备——module-dev 步骤 1 会逐条停下报告）：§3.1 的结构体字面只列 `clients` / `models` 两个字段，而**同一节的职责 3**（"计量与审计：解析 usage……**成败都写** `llm_calls`"）要求服务持有审计仓储与日志器。本 US 补 `store core.LLMCallStore` 与 `logger *slog.Logger`，两者均**装配期注入**（宪法 VII：依赖显式传递，不经 context）；`clients` / `models` 的字段名与顺序与 §3.1 逐字一致。这是"职责 3 的落地所需"，不是对 §3.1 的改写——**用户已裁决（2026-10-16）：同意，属既定审计职责的显式依赖**。

7. **客户端自建薄传输：SDK 角色收窄**（软门禁**报备项**——角色变更，非新增坐标、非版本变更；**用户已裁决 2026-10-16：同意**，见「已决事项」）：技术方案 §3.1 / §10.1 只规定 `provider` 放"OpenAI 兼容客户端"，技术栈句规定 go-openai"只做协议调用"；本 US **选定的路线**是**用它的协议类型与已知字段编解码，HTTP 往返自己做**（`net/http` + `encoding/json`，`CheckRedirect` 拒绝跟随）——**这是路线选择，不是技术必然**（备选路线见本节末的"备选评估"，同样成立，只是代价不同）。依据是**源码核实**（v1.43.0，见 §3.1 尾注）：① `ChatCompletionMessage` 没有通用扩展字段，`MarshalJSON` / `UnmarshalJSON` 只覆盖已知键——未知字段**两个方向都会被丢**，技术方案 §4.4 约束 3（供应商扩展字段透传）在库的 chat API 上**不可实现**；② 库未导出 `sendRequestRaw` / `newRequest` / `withExtraBody`（后者仅 embedding 路径使用），chat 没有原始 JSON 通道。**备选评估**：自定义 `RoundTripper` 注入 / 截获未知字段——与库内部编解码耦合（要按 `messages` 下标配对、并吞掉库自身的解析与错误语义），复杂度更高、收益相同，**不采用**。代价如实登记：库的传输层便利（代理环境变量、连接池调参）改由标准库缺省承担（`http.Transport` 同样读 `HTTP_PROXY` / `NO_PROXY` 等）。**坐标系不变**（仍是 v1.43.0，不新增坐标、不变版本），协议字段（`model` / `messages` / `tool_calls` / `reasoning_content` / `usage`）是契约面，不随库版本变。**技术方案 §4.4 的透传承诺不变**——本 US 换的是实现路线、补的是实现方式，不是承诺本身；自建 HTTP 层**限定在 `provider` 内**，承担请求、响应与扩展字段透传，文档既有的取消传播 / 禁止重定向 / 错误脱敏 / 两跳透传用例一并沿用（「唯一路径」一类措辞已按用户 2026-10-16 裁决改为「本 US 选定路线」）。

8. **审计预写与终态回填共用同一把解耦上下文**：契约 §4.2 的主语是"审计写入"（整体），不止终态回填——故本 US 把两处都放进 `auditContext(ctx)`（`WithoutCancel` + 5 秒，数值取自契约"缺省 5 秒"，**不新增配置键**）。可直接观察的后果：**调用前 ctx 已取消时，仍先落 `pending` 行**，再发出（客户端立刻以 `context.Canceled` 失败），终态 `failed` + `timeout`。另一条路（预写用原 ctx）会让"取消"伪装成"审计不可用"的 fail-closed 拒发，与 §4.2 首句的理由（被取消的运行也必须落审计）相反。

9. **`base_url` 的支持面与拒绝规则**（用户 2026-10-16 裁决：**装载期明确拒绝**，不采用"接受后静默丢弃"）：只支持 `scheme`（`http` / `https`）+ `host` + `path` 三部分，**原样保留**（含 base path，如 `/v1`；`path` **可为空**——`basalt.yaml.example` 的 `https://api.deepseek.com` 就是合法形态，客户端在其后接 `/chat/completions`）；含 **userinfo / query / fragment** 的取值在**装载期拒绝**（`validateConfig`，第 8 类 fail-closed，§3.5 改造点 1）。裁决理由：展示脱敏与"配置是否合法"是两件事——静默删掉这些部分会**改写用户配置的含义**，把认证 / 路由错误推迟成难定位的运行时故障（query 里可能带着网关路由参数）。**文案纪律**：只点名 `providers[i].base_url` 与不支持的成分名（`userinfo` / `query` / `fragment`），**不回显原 URL，也不回显其中的值**（userinfo 里可能就是凭证）；`url.Parse` 的原始错误自带 URL，**不得**直传。**发送层保留防御校验**：`provider.NewOpenAIClient` 构造期按同一口径再判一次（客户端可被直接构造，不依赖装载层已拦）——两层判定同一条口径、同一文案纪律，实现各一小段；**不得**为复用而让 `cli` import `provider`（依赖方向红线，`ports_test.go` 判卷）或把实现塞进 `core`（约束 10）。缺失 scheme 视为非法——**不代补** `https`（不做隐式推断，与"不静默改配置"同一原则）。展示层 `redactedBaseURL` 保留（**纵深防御**：配置面已拒绝，展示层仍按收窄规则渲染，不依赖上一层）。将来若需支持 query，**先显式定义支持范围与语义**再放开，不做"白名单之外一律丢弃"的宽松解析。

## 3. 交付物清单

### 3.1 代码（类级粒度 + 关键方法签名；不写方法体）

```text
internal/core/
├── provider.go      # 新增：ProviderClient 接口 + 消息模型 + 审计接口与数据类型（**纯声明，零语句**）
└── errors.go        # 修改：新增哨兵 ErrProviderNotConfigured（前序改造点，§3.5）

internal/provider/
├── doc.go           # 修改：占位句改为实装说明（约束仍在：无自动工具循环 / 工具调度归 ToolExecutor）
├── client.go        # 新增：OpenAIClient（OpenAI 兼容协议翻译 + 响应解析 + 错误归类）
└── service.go       # 新增：ProviderService（显式映射路由 + 两阶段审计写）

internal/storage/
└── llm_calls.go     # 新增：LLMCallRepo（llm_calls 两阶段写的唯一 SQL 入口）

internal/app/
└── providers.go     # 新增：装配函数（配置 → 显式映射 → ProviderService）

internal/cli/
├── provider.go      # 新增：provider 命令 + list 子命令（轻路径）
└── root.go          # 修改：注册 provider 命令组（前序改造点，§3.5）

internal/testutil/
└── provider.go      # 新增：FakeProvider（core.ProviderClient 假实现）+ FakeOpenAIServer（回环假服务）
```

```go
// internal/core/provider.go —— 消息模型逐字取自技术方案 §4.4（编程指南 §7 陷阱 23）
type Message struct {
    Role             string         `json:"role"`
    Content          string         `json:"content"`
    ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
    ToolCallID       string         `json:"tool_call_id,omitempty"`
    ReasoningContent string         `json:"reasoning_content,omitempty"`
    ProviderExtra    map[string]any `json:"provider_extra,omitempty"`
}

type ToolCall struct {
    ID        string `json:"id"`
    Name      string `json:"name"`
    Arguments string `json:"arguments"`
}

// Usage 是 token 计量的载体——**可空**：供应商未返回时为零值，落库为三列 0（不判失败）。
type Usage struct {
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
}

// ChatRequest 是一次调用的完整入参（技术方案 §3.1 + §4.1 的单参数调用形状）。
// 两组字段的读者不同：路由与审计关联组由 ProviderService 消费；协议内容组由 ProviderClient 消费。
type ChatRequest struct {
    // —— 路由与审计关联（ProviderService 消费）——
    Provider   string // 显式映射表的键；查表不中 → 点名错误（不遍历、不推断——宪法 III）
    RunID      string // 审计关联单位（契约 §7.1）；空 = 拒绝发出（NOT NULL 列的语义位）
    SessionID  string // 可空（列允许 NULL）
    Round      int    // 该 Run 内第几轮（1 起）
    ConfigHash string // 配置快照 hash；本 US 恒空（§2.4 裁决注 3）

    // —— 协议内容（ProviderClient 消费）——
    Model    string    // 空 = 用供应商缺省模型（models 映射）
    Messages []Message // 完整消息模型（含 tool_calls / reasoning_content，原样发出）
}

// ChatResponse 是一次调用的返回（usage 可空；tool_calls 只透传，执行权归 US-4 的 ToolExecutor）。
type ChatResponse struct {
    Content          string
    ToolCalls        []ToolCall
    ReasoningContent string         // 供应商扩展：思考过程——**必须在会话历史里回传**（US-4）
    ProviderExtra    map[string]any // 协议未知字段的透传位（出站平铺 / 入站收集，经**两跳**断言——§3.1）
    Usage            Usage
}

// ProviderClient 抽象单个供应商的调用能力（技术方案 §3.1 逐字）。
type ProviderClient interface {
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// LLMCallStatus 是 llm_calls.status 的字面量（契约 schema.sql；pending/success/failed/unknown）。
type LLMCallStatus string

const (
    LLMCallPending LLMCallStatus = "pending" // 预写态；插入失败即拒绝发出（fail-closed）
    LLMCallSuccess LLMCallStatus = "success"
    LLMCallFailed  LLMCallStatus = "failed"
    LLMCallUnknown LLMCallStatus = "unknown" // 恢复扫描写入（US-4）；本 US 不产生
)

// ErrorKind 是 llm_calls.error_kind 的七类字面量（契约 §7.3）。
type ErrorKind string

const (
    ErrorKindSandboxDenied ErrorKind = "sandbox_denied"
    ErrorKindTimeout       ErrorKind = "timeout"
    ErrorKindNotFound      ErrorKind = "not_found"
    ErrorKindInvalidInput  ErrorKind = "invalid_input"
    ErrorKindExternalError ErrorKind = "external_error"
    ErrorKindRuntimeError  ErrorKind = "runtime_error"
    ErrorKindUnknown       ErrorKind = "unknown"
)

// LLMCall 是审计行的**预写形态**：只含调用前可知的字段（关联键 + 供应商 + 模型 + 快照）。
// **不带状态字段**——预写态恒为 pending，由仓储的 SQL 固定写（不由调用方给，
// 也就没有"传错状态"的可能；终态才由 LLMCallResult 表达）。
type LLMCall struct {
    RunID      string
    SessionID  string
    Round      int
    Provider   string
    Model      string    // 解析后的**生效**模型（请求模型 > 供应商缺省模型）
    ConfigHash string    // 空 → 列写 NULL
    CreatedAt  time.Time // 列存 Unix 毫秒 UTC（契约迁移纪律 4）
}

// LLMCallResult 是审计行的**终态回填形态**：成败与计量都在这——成败只由 Status 表达，不设布尔列。
// 三 token 列**显式平铺**（不嵌 core.Usage）：审计表的列是平的，回填结构与列同构；
// 可空兜底（供应商未返回 usage → 三列 0）由调用方在填之前完成——嵌套一层会让
// "谁负责兜底"变含糊。
type LLMCallResult struct {
    Status           LLMCallStatus
    PromptTokens     int
    CompletionTokens int
    TotalTokens      int
    Duration         time.Duration
    ErrorMessage     string    // 截断 4 KB 的责任在 storage（§3.4）
    ErrorKind        ErrorKind // 失败必填（契约 §7.3）；成功留空 → 列写 NULL
}

// LLMCallStore 是 ProviderService 消费的审计仓储接口（接口定义在消费方 core，实现放 storage）。
type LLMCallStore interface {
    // BeginCall 插入 status=pending 的审计行并返回行 id；失败 = 调用方**拒绝发出**该次调用。
    BeginCall(ctx context.Context, call LLMCall) (int64, error)

    // FinishCall 把该行更新为终态；调用方以独立短超时 + WithoutCancel 调用（契约 §4.2）。
    FinishCall(ctx context.Context, id int64, result LLMCallResult) error
}
```

```go
// internal/core/errors.go（新增哨兵；既有哨兵不动）
// ErrProviderNotConfigured 表示请求的 provider 不在显式映射表内——路由查表不中，
// 不遍历推断、不取"唯一的那个"当默认（宪法 III）。对外映射为 NOT_CONFIGURED（US-9）。
var ErrProviderNotConfigured = errors.New("provider is not configured")

// internal/provider/client.go
// Options 是单个供应商客户端的构造入参——逐条来自配置（app 装配处），不来自遍历推断。
type Options struct {
    Name    string // 诊断用（错误文案点名供应商）
    BaseURL string // OpenAI 兼容端点；只接受 scheme + host + path——空 / 非法 / 含 userinfo/query/fragment
                   // → 构造失败（防御校验，点名 providers[i]；装载期已拒，§2.4 注 9；文案不回显 URL）
    APIKey  string // **已解析**的真实值；只进客户端，不进日志 / 错误 / 审计
}

// OpenAIClient 是 OpenAI 兼容协议的实现：**自建薄传输**（一次 POST——不重试、不跟随
// 重定向），未知字段两端保真（§2.4 裁决注 7）；只做协议调用与响应解析，不接管循环（宪法 I / II）。
type OpenAIClient struct {
    name     string
    endpoint string       // chat/completions 端点：scheme + host + path **原样保留**（不裁剪——不支持的
                          // 成分在 parseBaseURL 处已被拒，见 §2.4 注 9）
    apiKey   string
    http     *http.Client // 不设超时：截止时间由调用方 ctx 承载（「已决事项」**原待决** #3）
}

func NewOpenAIClient(opts Options) (*OpenAIClient, error)
func (c *OpenAIClient) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error)
// 编译期断言：var _ core.ProviderClient = (*OpenAIClient)(nil)

// CallError 是客户端边界产出的**安全调用错误**（约束 8 的落点）：文案已脱敏——抹除 key
// 回显、截断 4 KB、不含请求 URL 与响应正文全文；类别已判好；原始链经 Unwrap 保留——
// 取消 / 截止用 errors.Is(err, context.Canceled / DeadlineExceeded) 仍可判。
type CallError struct {
    Provider string         // 供应商名（点名用）
    Status   int            // HTTP 状态码；0 = 传输层失败（DNS / 连接 / TLS）
    Kind     core.ErrorKind // 客户端的判定面：external_error / timeout
    Message  string         // 已脱敏、已截断的文案
    err      error          // 原始错误链（ctx 错误经此保留）
}

func (e *CallError) Error() string
func (e *CallError) Unwrap() error

// 内部函数（同为交付面，逐项可核）
func (c *OpenAIClient) buildRequestBody(model string, msgs []core.Message) ([]byte, error)
func encodeMessages(msgs []core.Message) ([]map[string]any, error) // 已知字段 + ProviderExtra **平铺**
func parseChatResponse(body []byte) (core.ChatResponse, error)     // 已知键 → 库类型；未知键 → ProviderExtra
func sanitizeMessage(text, apiKey string) string                   // 抹 key 回显 + 4 KB UTF-8 安全截断
func parseBaseURL(raw string) (endpoint string, err error)         // 构造期防御校验：只放行 scheme+host+path（§2.4 注 9）

// internal/provider/service.go
// ProviderService 持有 name → 客户端的显式映射（宪法 III）——装配时一次构建，之后只读（并发安全）。
type ProviderService struct { /* clients map[string]core.ProviderClient; models map[string]string; store core.LLMCallStore; logger *slog.Logger */ }

func NewProviderService(clients map[string]core.ProviderClient, models map[string]string,
    store core.LLMCallStore, logger *slog.Logger) *ProviderService
func (s *ProviderService) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error)
// 编译期断言：var _ core.ProviderClient = (*ProviderService)(nil)（路由面与单供应商面同形——§4.1 的调用形状）

// 内部函数（同为交付面，逐项可核）
func (s *ProviderService) resolveModel(req core.ChatRequest) string
func (s *ProviderService) finish(ctx context.Context, id int64, result core.LLMCallResult)
func classifyError(err error, ctxErr error) core.ErrorKind // 包内函数：错误 → error_kind 七类

// internal/storage/llm_calls.go
// LLMCallRepo 是 llm_calls 的唯一 SQL 入口（审计 SQL 只允许出现在 internal/storage——全局不变量 3）。
type LLMCallRepo struct{ /* db *DB */ }

func NewLLMCallRepo(db *DB) *LLMCallRepo
func (r *LLMCallRepo) BeginCall(ctx context.Context, call core.LLMCall) (int64, error)
func (r *LLMCallRepo) FinishCall(ctx context.Context, id int64, result core.LLMCallResult) error
// 编译期断言：var _ core.LLMCallStore = (*LLMCallRepo)(nil)

// internal/app/providers.go
func newProviderClients(cfg *cli.Config) (map[string]core.ProviderClient, map[string]string, error)
func newProviderService(cfg *cli.Config, store core.LLMCallStore, logger *slog.Logger) (*provider.ProviderService, error)

// internal/cli/provider.go
func newProviderCommand(deps Deps) *cobra.Command // 命令组：provider list
func runProviderList(out io.Writer, deps Deps) error
func redactedBaseURL(raw string) string // 展示层收窄：只留 scheme://host/path（纵深防御，§2.4 注 9）

// internal/testutil/provider.go
// FakeProvider 是 core.ProviderClient 的固定假实现：按脚本回放响应 / 错误、记录收到的请求、
// 支持阻塞到 ctx 取消（取消路径用例）。编程指南 §8.3 第 2 项：假 Provider 随 US-2 接口落位、后续 US 复用。
type FakeStep struct {
    Response core.ChatResponse
    Err      error
    Block    bool // 阻塞直到 ctx.Done()（取消用例）
}

func NewFakeProvider(steps ...FakeStep) *FakeProvider
func (f *FakeProvider) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error)
func (f *FakeProvider) Requests() []core.ChatRequest // 收到的请求（顺序回放；并发安全）
func (f *FakeProvider) CallCount() int

// Started 返回第 n 次调用（n 从 1 起）**进入时**关闭的信号通道——取消 / 并发用例的
// 同步基元：测试等它而不是 time.Sleep（Sleep 会在"预写尚未完成"时取消，用例走成
// 另一条分支；见 §4.3 的取消用例注释）。
func (f *FakeProvider) Started(n int) <-chan struct{}

// FakeOpenAIServer 是 OpenAI 兼容协议的回环假服务：按脚本回放 HTTP 状态与响应体，
// 并**记录收到的请求体**（编程指南 §8.4 第 3 项的双向断言口径——遇预期之外的请求即失败，
// 专防"库记录齐了、发给模型的协议错了"）。
type FakeOpenAIResponse struct {
    StatusCode int    // 0 → 200
    Body       string // 原样响应体（含畸形 JSON 用例）
}

func NewFakeOpenAIServer(t *testing.T, script ...FakeOpenAIResponse) *FakeOpenAIServer
func (s *FakeOpenAIServer) URL() string                 // 供 Options.BaseURL 使用
func (s *FakeOpenAIServer) Requests() []map[string]any  // 收到的请求体（已解码）
```

> **信封的构造方式（刻意不靠 Go 的字段仲裁）**：`buildRequestBody` 先把 SDK 请求类型的已知字段 marshal 成 `map[string]json.RawMessage`（如 `{"model": …}`），再**显式覆盖** `messages` 键为 `encodeMessages` 构造的消息数组，最后 marshal 整个 map。**不**用"外层结构体嵌入 SDK 类型 + 同名字段遮蔽"的写法——那要让出站体只有一个 `messages` 键这件事依赖字段仲裁规则；map 覆盖是构造出来的，`TestOpenAIClient_RequestEnvelopeKeepsSingleMessagesKey` 直接断言出站体里 `"messages":` 只出现一次。US-4 扩展请求字段（`tools` / `temperature`）时，改的就是这个已知字段基座（§3.5 占位 3）。
>
> **`OpenAIClient.Chat` 的返回路径（四条，全部从 `CallError` 出口）**：① 2xx 且正文可解析 → `(resp, nil)`；② 2xx 但正文畸形 → `CallError{Kind: external_error, Status: 2xx, Message: "响应不可解析…"}`（**不带正文**）；③ 非 2xx（含 3xx——不跟随重定向）→ 解 `openai.ErrorResponse` 取 `error.message` → `CallError{Kind: external_error, Status: 状态码, Message: sanitizeMessage(服务器文案, key)}`；④ 传输失败 / ctx 取消或截止 → `CallError{Kind: timeout（ctx 错误）/ external_error（DNS / 连接 / TLS）, Status: 0, err: 原始链}`。**四条都不得把 key、请求 URL 或响应正文全文带出去**——`sanitizeMessage` 是出口前的最后一道（约束 8）。
>
> **库 API 与协议事实（源码核实，2026-10-16；读本地模块缓存 `github.com/sashabaranov/go-openai@v1.43.0`）**：
>
> | 事实 | 读数 | 对本设计的作用 |
> | --- | --- | --- |
> | 无通用扩展字段 | 全仓 `ExtraFields` **零命中** | `ProviderExtra` 不能经库往返——§2.4 裁决注 7 的直接依据 |
> | 消息编解码只认已知键 | `ChatCompletionMessage` 的 `MarshalJSON` / `UnmarshalJSON` 只处理 `role` / `content` / `name` / `function_call` / `tool_calls` / `tool_call_id` / `refusal` / `reasoning_content` 等已知字段 | 出站平铺、入站收集必须我们自己做过（两跳验收） |
> | chat 无原始 JSON 通道 | `sendRequestRaw` / `newRequest` 未导出；`withExtraBody` 未导出且仅 embedding 路径使用 | "让库替我们透传"无路可走 |
> | 无内置重试 | 库不含重试逻辑（自建传输后此行仅作记录） | 契约 §3.3 的 SDK 重试例外**不被触发**（约束 2） |
> | 传输与错误文案会泄露 | `sendRequest` 返回裸 `*url.Error`（含 URL）；`RequestError.Error()` 含**完整原始响应体**；`APIError.Error()` 含服务器消息 | 错误文案**不得**直接取自库错误的 `Error()`——`CallError` 自建文案（约束 8） |
> | 错误体类型可复用 | `ErrorResponse` / `APIError` 可解 OpenAI 标准错误信封 | 非 2xx 走它取 `error.message`，再经 `sanitizeMessage` |
>
> **仍标「以实测为准（写前核实）」的残余项（写实现时逐项 `go doc` / 源码核对——module-dev 硬门禁 H3）**：① 出站 `stream` 键的形态（库的 tag 是否带 `omitempty`——判据写成"`stream` **不得为 true**"，两种形态都成立）；② `ChatCompletionRequest` 的 `Model` / `Stream` 与 `ChatCompletionMessage` 已知字段集随版本变化的可能（本 US 只用这几个结构体，升级库时重跑 §3.2 的 client 用例即覆盖）。**协议字段映射本身**（`model` / `messages` / `tool_calls` / `reasoning_content` / `usage`）是契约面（技术方案 §4.4 + 契约 §7.4），不随库版本变。

### 3.2 测试（file → cases；函数名英文，中文验收点以注释保留）

| 文件 | 用例 | 守什么 |
| --- | --- | --- |
| `internal/provider/service_test.go` | `TestProviderService_RoutesByExplicitMap` | 宪法 III：路由只查表，键集 = 装配传入的映射 |
| | `TestProviderService_UnknownProviderRejectedWithoutAuditRow` | 查表不中 → 点名错误、**零调用**、**零审计行**（「无行 = 确定没执行」）；日志轨收到 WARN（FR-010——路由失败记日志） |
| | `TestProviderService_RejectsMissingRunBinding` | `run_id` 空 / `round < 1` → 拒绝、零调用、零审计行 |
| | `TestProviderService_UnresolvableModelRejectedWithoutAuditRow` | **模型解析为空**（请求模型与供应商缺省都空）→ 点名 provider 拒绝、零调用、零审计行（流程 B 步骤 2 的第三道拒绝） |
| | `TestProviderService_DefaultModelFromMapAndOverride` | 模型解析顺序（请求 > 供应商缺省）；生效模型落 `llm_calls.model` |
| | `TestProviderService_AuditPendingBeforeCall` | **留痕顺序**：假 Provider 在 `Chat` 内当场断言该次 `pending` 行已存在 |
| | `TestProviderService_WritesSuccessRowWithUsage` | 成功：`status=success` + 三 token 列 + `duration_ms >= 0` + `error_kind` 为 NULL |
| | `TestProviderService_UsageAbsentIsZero` | `usage` 可空兜底：三列 0、不判失败、不 panic |
| | `TestProviderService_ErrorResponseRecordedWithKind` | **错误模型响应**：供应商 5xx → `failed` + `external_error` + 原文截断；日志轨收到 WARN（FR-010） |
| | `TestProviderService_ProviderErrorEchoingKeyNeverLandsInAuditOrLogs` | **凭证三路**（约束 8）：假服务在错误正文里**回显合成 key** → 返回错误 / 日志缓冲 / `llm_calls.error_message` 三处均 `NotContains` |
| | `TestProviderService_RefusesCallWhenAuditPreWriteFails` | **审计写入失败**：预写失败 → 客户端 **0 次调用**（fail-closed 的判据） |
| | `TestProviderService_TerminalWriteFailureKeepsPendingRowAndLogsERROR` | 残余窗口：终态写失败 → 行留 `pending`、ERROR 日志、**不伪造终态** |
| | `TestProviderService_CanceledCallTerminalWriteFailurePreservesCancelChain` | **取消 + 终态写失败**：行留 `pending`、ERROR 日志，**返回的仍是取消错误**（终态写失败不得覆盖调用结果——约束 7 的可执行判据） |
| | `TestProviderService_CancelDuringCallRecordsTimeoutKind` | **取消**：`Block` 假 Provider，**经 `Started(1)` 信号确认调用已进入**才取消（**无 `time.Sleep`**）→ 返回错误（链保留 `context.Canceled`）、行落 `failed` + `timeout` |
| | `TestProviderService_PreCanceledContextStillAuditsFailedCall` | **调用前 ctx 已取消**：仍先落 `pending`（预写与取消解耦——§2.4 注 8）、终态 `failed` + `timeout`、错误链保留 `context.Canceled` |
| | `TestProviderService_ConcurrentCallsAreRaceFree` | **并发**：多 goroutine 并发调用（含取消路径）→ `-race` 无告警、无 goroutine 泄漏 |
| | `TestProviderService_ResponseFieldsPassThrough` | 透传：`tool_calls` / `reasoning_content` / `provider_extra` 原样回到调用方 |
| | `TestProviderService_UnclassifiedErrorRecordedAsUnknownKind` | **「未知」兜底**（FR-007 第三类 / 契约 §7.3）：假客户端抛回**裸错误**（非 `CallError`、错误链上无 ctx 错误）→ 行落 `failed` + `unknown`——该值**不是**恢复路径专属（恢复扫描以 `status=unknown` 判定，与本兜底同值不同因） |
| | `TestProviderService_CancelDuringResponseReadRecordsTimeoutKind` | **读取阶段的取消**（实现评审 P1 回归）：响应头已返回后取消 → 行落 `failed` + `timeout`（曾误记 `external_error`）、失败原文含「未取完响应」、日志 `error_kind=timeout` |
| | `TestProviderService_MalformedResponseRecordsFailedAudit` | **坏响应不得记 success**（实现评审 P2 回归）：200 + `{"choices":[{}]}` → 行落 `failed` + `external_error`（曾按零值响应记 `success`） |
| `internal/provider/client_test.go` | `TestOpenAIClient_TranslatesRequestToOpenAIProtocol` | 假服务断言收到的请求：`model` / `messages` 顺序与内容 / `stream` **不得为 true**（缺省形态以实测为准——尾注残余项 ①） |
| | `TestOpenAIClient_RequestEnvelopeKeepsSingleMessagesKey` | 信封构造：出站体里 `"messages":` **只出现一次**，条数与顺序 = 传入消息（防"已知字段基座把我们的 `messages` 顶掉"） |
| | `TestOpenAIClient_HTTPErrorMapsToExternalError` | **错误模型响应**：4xx / 5xx / 3xx（不跟随）/ 畸形 JSON → `external_error`（不 panic；3xx 断言**未**跟到 `Location`） |
| | `TestOpenAIClient_ContextCancelStopsCall` | **取消**：慢响应 + ctx 取消 → 立即返回，`errors.Is(err, context.Canceled)` 成立（链未断） |
| | `TestOpenAIClient_NoCredentialInErrorMessages` | 凭证纪律：构造失败 / 传输失败 / **服务器回显 key** 的错误文案均不含 key 值，且不含请求 URL 的 query |
| | `TestOpenAIClient_RejectsBaseURLWithUnsupportedParts` | §2.4 裁决注 9 的**构造期防御校验**：userinfo / query / fragment / 缺 scheme / 非 `http(s)` 方案 → 构造失败；文案点名键路径与成分，**不含原 URL 与其中的值**（`url.Parse` 的错误不得直传）；**空 fragment / 空 query 分隔符同拒**（实现评审 P2 回归——尾随 `#` / `?` 解析后成分字段是空串，只看成分字段会漏，而 `…/v1#` 会让拼接端点整段落进 fragment） |
| | `TestOpenAIClient_RoundTripsUnknownFieldsTwoHops` | 技术方案 §4.4 约束 3 的**两跳验收**：假服务返回未知字段 → `ProviderExtra` 捕获 → **下一次请求体仍含该字段**（仅对 `core.Message` 做一次 JSON 往返检不出库丢字段，故不构成判据） |
| | `TestOpenAIClient_CancelOrDeadlineDuringResponseReadStaysTimeout` | **读取阶段的取消 / 截止**（实现评审 P1 回归）：响应头已返回后取消 / 截止 → `timeout` + 链保留 `Canceled` / `DeadlineExceeded`；判据含 `Status == 200`（钉死"确在读取阶段"）与文案「未取完响应」 |
| | `TestOpenAIClient_RoundTripsBigIntegersTwoHops` | **大整数两跳保真**（实现评审 P1 回归）：未知字段里的 2^53+1 整数（顶层 / 嵌套对象 / 数组三处）**判据取原始出站 JSON**——解码判卷会再次 float64 化，掩盖同一缺陷 |
| | `TestOpenAIClient_RejectsResponseBodyOverLimit` | **响应体读取上限**（实现评审优化项）：成功体与错误体各发 8 MiB（上限 4 MiB 的两倍）→ 安全失败、文案点名上限、正文一个字不外带 |
| | `TestOpenAIClient_RejectsResponseWithoutChoiceMessage` | **缺 message 的响应判死**（实现评审 P2 回归）：`{"choices":[{}]}` / `message:null` / `message` 非对象 → `external_error`；正例不连坐——`content:null` + `tool_calls` 仍判成功 |
| | `TestOpenAIClient_AcceptsEncodedHashInPath` | **空 fragment 拒绝的正例守卫**（实现评审 P2 回归）：路径里的 `%23` 原样上线（判据取服务端 `EscapedPath()`）——被拒的是字面 `#` 分隔符，合法编码不连坐 |
| `internal/storage/llm_calls_test.go` | `TestLLMCallRepo_BeginCallInsertsPendingRow` | 预写行的列值（`status=pending`、`duration_ms=0`、`config_hash` NULL、`created_at` 毫秒 UTC） |
| | `TestLLMCallRepo_FinishCallUpdatesTerminalFields` | 终态回填的列值（成败、token、耗时、`error_kind`） |
| | `TestLLMCallRepo_FinishCallTruncatesErrorMessageAt4KBWithoutBreakingRunes` | 契约 §7.4：截断 4 KB 且**不切断 UTF-8 字符** |
| | `TestLLMCallRepo_FinishCallRejectsNonPendingRow` | 终态只写一次：非 `pending` 行 → 报错（点名 id），不覆盖 |
| | `TestLLMCallRepo_CrashWindowLeavesPendingForRecovery` | **崩溃恢复窗口**：崩溃 = 进程消失后留下的持久化形态就是 `pending` 行（恢复归一化 `pending → unknown` 归 US-4，本用例只判窗口形态：无半写、无终态伪造） |
| | `TestLLMCallRepo_ConcurrentBeginCall` | **并发**：多 goroutine 预写（写池 1 连接串行）→ `-race` 无告警、id 唯一 |
| `internal/app/providers_test.go` | `TestNewProviderClients_ExplicitMapMatchesConfig` | 宪法 III：映射键集 **逐字** = 配置条目名集；无"默认供应商" |
| | `TestNewProviderClients_InvalidBaseURLRejected` | 非法 `base_url` → 构造失败并点名 `providers[i]` |
| | `TestNewProviderService_ConfigToAuditRowChain` | 端到端（假客户端 + 真库）：配置 → 映射 → 调用 → `llm_calls` 行 |
| `internal/cli/provider_test.go` | `TestProviderList_ListsConfiguredProviders` | 输出含每家 name / **脱敏后的 base_url** / default_model + 汇总；exit 0 |
| | `TestProviderList_RedactsBaseURLUserinfoAndQuery` | §2.4 裁决注 9 的**展示层防御**：直接注入含 userinfo / query / fragment 的 provider 条目（**绕过装载期**——配置面已拒绝，展示层不得依赖这一点）→ `https://u:p@h/v1?key=x` 渲染为 `https://h/v1`，三部分都不出现在输出里 |
| | `TestProviderList_NoCredentialLeakInOutput` | 凭证三路不泄露（stdout / stderr / logger 缓冲均 `NotContains`——与 US-1 同口径） |
| | `TestProviderList_MissingConfigFailsWithPath` | §2.4 裁决注 2：缺配置 → 失败并点名路径（**不**沿用 status 的容忍语义） |
| | `TestProviderList_EmptyProvidersSectionFails` | `providers` 段为空 → 失败并点名段名 |
| | `TestProviderList_InvalidConfigFailsAndNamesKeyPath` | 配置无效 → 原错误上抛（点名键路径与变量名、不含凭证值） |
| `internal/cli/config_test.go`（追加） | `TestConfig_RejectsDuplicateProviderName` | 前序改造点 1：重名 → 装载期拒绝（点名两条目下标） |
| | `TestConfig_RejectsMissingProviderBaseURL` | 前序改造点 1：`base_url` 缺失 → 拒绝并点名键路径（**独立用例**——必填是新增规则，不复用其他必填用例的判据） |
| | `TestConfig_RejectsProviderBaseURLWithUserinfoQueryFragment` | 前序改造点 1 的**第 8 类规则**：子例覆盖 userinfo / query / fragment / **缺 scheme**（缺 scheme 对号 spec.md US3 验收场景 2 的「地址缺 scheme」例；非 `http(s)` 方案同判）→ 装载期拒绝；文案含键路径与成分名，**不含原 URL 与其中的值**（userinfo 里可能就是凭证） |
| `internal/cli/ports_test.go`（修改） | `TestDependencyDirection_CliDoesNotImportImplementations`（**扩展**禁止清单：加入 `internal/provider`） | 依赖方向红线：命令面不得为复用脱敏而 import 实现包——`redactedBaseURL` 自实现的理由由 CI 判卷（analysis 阶段发现：既有清单只拦 `storage` / `workspace`） |
| `internal/app/realcall_integration_test.go`（`//go:build integration`） | `TestProviderService_RealCallWritesAudit` | 验收 Demo 正例：**显式执行开关 + 凭证 env 门控**（缺一即 `t.Skip` 并写明理由）→ 成功行；token 判据 = **审计行与响应三列一致**——**不得**由 `total_tokens > 0` 反推"供应商返回了 usage"（该推断强于既定判据，实现评审优化项已撤除；usage 缺失与"usage 在但两列皆 0"总数上不可分）；入口 `context.WithTimeout` **显式设截止**（「已决事项」**原待决** #3） |
| | `TestProviderService_UnreachableEndpointRecordsFailedAudit` | 验收 Demo 反例：**不可达端点**失败也落审计（`failed` + `external_error`）；**不需要凭证**，故不因缺凭证跳过——它是 fail-closed 的正面判据 |
| `internal/testutil/provider_test.go` | `TestFakeProvider_ReplaysScriptedResponses` | 基础设施自检（与 US-1 的 `TestTestutil_HTTPServer` 同口径） |
| | `TestFakeProvider_SignalsCallStartForDeterministicCancelSynchronization` | `Started(n)` 的信号语义：调用进入即关通道——取消用例的同步基元**自己先被判卷**（它若坏掉，表现为用例挂起或时紧时松，属最难查的一类） |
| | `TestFakeOpenAIServer_RecordsRequestsAndServesCannedResponse` | 同上——假服务本身必须先被判卷 |

### 3.3 配置与工程文件

| 项 | 内容 |
| --- | --- |
| `go.mod` / `go.sum` | 新增**唯一**坐标 `github.com/sashabaranov/go-openai v1.43.0`（固定技术栈句内，直接落地；本轮已核实见约束 9）。其余坐标不动 |
| `Makefile` | **无改动**（七目标不变；`provider` / `core` 的覆盖率判卷自动纳入 `make cover`） |
| CI（`.github/workflows/ci.yml`） | **无改动**（两 job 与必需检查不变；新增测试随既有 `go test` 目标执行） |
| 测试环境变量（只被集成层读取，**不落配置、不入 `basalt.yaml`**） | `BASALT_REAL_CALL=1`（显式执行开关，见 §4.1 / §6）与 `BASALT_DEMO_DIR`（演示工作区路径）；凭证 env 由 `basalt.yaml` 的 `${VAR}` 占位声明（US-1 已交付机制），本 US 不新增凭证键 |

### 3.4 表

| 表 | 状态 | 说明 |
| --- | --- | --- |
| `llm_calls` | US-1 已建齐；**本 US 首次写入** | 列 / 状态字面量唯一事实源 = [docs/contracts/schema.sql](../contracts/schema.sql)（**本文不另抄列清单**——抄一份必漂移一处）。写入路径唯一：`internal/storage/llm_calls.go`（审计 SQL 只出现在 storage——全局不变量 3）。本 US 常量：`error_message` 截断 4 KB（UTF-8 安全）、`config_hash` 写 NULL（§2.4 裁决注 3） |

### 3.5 前序改造点与依赖副作用

**前序改造点**（触及 US-1 已交付契约，逐条为软门禁——开工前报告）：

1. **`internal/cli/config.go`**：① `validateConfig` 新增 `providers[].name` **重名拒绝**（第 7 类 fail-closed；症状：map 静默保留最后一条，映射表缺条目无人知晓）；② `providers[].base_url` 提为**必填**（缺 → 点名拒绝；无 base_url 无法构造客户端）；③ `providers[].base_url` **拒绝含 userinfo / query / fragment 的取值**（第 8 类 fail-closed；只支持 scheme + host + path，缺失 scheme 不代补——§2.4 注 9）。③ 的文案只点名键路径与不支持的成分名，**不回显原 URL 或其中的值**（`url.Parse` 的错误不得直传）。三条都**不**改动既有六类校验的语义与文案风格。
2. **`internal/core/errors.go`**：新增哨兵 `ErrProviderNotConfigured`（既有哨兵不动、`errors.Is` 语义不变）。
3. **`internal/cli/root.go`**：`root.AddCommand(...)` 增注册 `newProviderCommand(deps)`（命令面 2 → 3 条，与需求 §5.12 的 12 条命令表一致推进）。
4. **`internal/provider/doc.go`**：占位句（"实现随 US-2 落位"）改为实装说明——约束原文保留（无自动工具循环 / 工具调度归 `ToolExecutor` / 显式映射）。

**依赖初始化副作用**：

- `go-openai` 经正常 import 使用（**只用其协议类型与已知字段编解码**，不再经其 `Client` 发请求——§2.4 注 7）：**无** `import _` 副作用注册、无 `init()`、不启动后台 goroutine。
- HTTP 往返由 `provider` 自建：`http.Client` 由客户端实例持有，**不设自有超时**（截止时间由调用方 `ctx` 承载——契约 §6 `RunTimeout`；「已决事项」**原待决** #3），`CheckRedirect` 拒绝跟随（约束 2）；代理等环境按标准库缺省（`HTTP_PROXY` / `NO_PROXY`）。
- `testutil.FakeOpenAIServer` **只监听回环**（`httptest`）——默认层无一例访问真实网络（沿用 US-1 §4.1 的分层口径）。

**后续模块能力占位**（下游接入点，逐条写明）：

1. `ChatRequest.Tools []ToolSpec` 字段与工具列表组装 → **US-4**（工具 schema 归 `ToolRegistry`；本 US **不预造**该字段，见 §2.4 裁决注 1）。
2. `config_hash` 填充 → **US-4**（运行内配置快照）/ **US-11**（热加载）；本 US 写 NULL。
3. `ProviderService.Chat` 的生产调用方 → **US-4** 的 `ReActLoop`：**方法签名沿用**（技术方案 §4.1 的调用形状与本 US 签名逐字一致）；**请求字段与协议适配按 US-4 已登记范围扩展**——`Tools` / `Temperature` 等落在 `buildRequestBody` 的已知字段基座上（§3.1 尾注），扩展时**必须**跑本 US 全部回归（两跳用例是这一步的护栏）。**本 US 不实现工具注册**（工具 schema 归 US-4 的 `ToolRegistry`）。
4. `FakeProvider` / `FakeOpenAIServer` → **US-4** 循环与工具用例复用；**US-14** mock 模型 e2e 的假服务形态评估（须用独立 build tag `e2e` + 产品二进制，编程指南 §8.4）。
5. `pending` → `unknown` 的恢复归一化 → **US-4**（契约 §4.3.2 第 1 步）；本 US 只保证窗口形态可被恢复流程识别（`pending` 行 + `created_at`）。
6. `error_kind` 的分类器 → **US-4 / US-5** 的工具侧沿用同一套七类字面量（`core.ErrorKind`）。

## 4. 验收测试（Harness）

### 4.1 测试分层标准

| 层 | 是否访问真实网络 | 内容 | 执行时机 |
| --- | --- | --- | --- |
| 默认层（不入 tag） | **不访问**——`testutil.NewFakeOpenAIServer` 只监听回环；`FakeProvider` 纯进程内 | §3.2 的 **54** 条用例（service 20 + client 12 + storage 6 + app 3 + cli 10 + testutil 3；含实现评审回合新增的 8 条：client 5 + service 2 + cli 1） | `make test`（`-race`）与 `make cover` |
| 集成层（`//go:build integration`） | **由人决定**：**显式执行开关 + 凭证 env 门控**（缺一即 `t.Skip` 写明理由）；反例用不可达端点（无真实出网、**不需要凭证**）；用**独立临时库**（临时工作区，不复用开发库） | `TestProviderService_RealCallWritesAudit` / `TestProviderService_UnreachableEndpointRecordsFailedAudit` | `make test-integration`（本 US 起有实质用例，不再空跑）。**跳过 ≠ 通过**：缺凭证的 `t.Skip` 只在日志里留痕，**不构成**"真实调用验收通过"（§6 收口时按此判） |
| 发布闸门（`//go:build e2e`） | 假模型服务 + 发布候选二进制 | **不属本 US**（US-14；编程指南 §8.4 六条） | 发布流程 |

### 4.2 测试类 → 验收点映射

| 测试类（§3.2 文件） | 覆盖的验收点 |
| --- | --- |
| `service_test.go` | 宪法 III（显式映射）/ 宪法 V（两阶段写、fail-closed）/ 契约 §7.2（含残余窗口）/ §7.3（三类取值——`unknown` 兜底由 `TestProviderService_UnclassifiedErrorRecordedAsUnknownKind` 单独判卷）/ §4.2（**预写与终态都解耦**——含"调用前已取消"）/ 技术方案 §4.4（透传）；约束 8（凭证三路）；五类重点中的**审计写入失败**、**取消**、**并发**；实现评审回归：**读取阶段的取消归 `timeout`**（审计面）、**坏响应不得记 `success`** |
| `client_test.go` | 技术方案 §3.1 协议翻译 / §3.3（不重试、不 fallback、不跟随重定向——一次性错误直接失败）/ §4.4 约束 3（扩展字段**两跳**透传）；约束 8（错误文案脱敏）；§2.4 裁决注 9（构造期防御校验——不支持的地址成分一律拒绝）；五类重点中的**错误模型响应**、**取消**；实现评审回归：读取阶段取消 / 截止的归并、大整数保真（原始出站 JSON 判卷）、响应体读取上限、缺 `message` 的响应判死、编码 `%23` 正例 |
| `llm_calls_test.go` | 契约 §7.2（预写 / 终态）/ §7.4（截断 4 KB）/ schema.sql 列语义；五类重点中的**崩溃恢复**（窗口形态层）、**并发** |
| `providers_test.go`（app） | 编程指南 §4.2 验收 Demo 全链（配置 → 显式映射 → 调用 → `llm_calls` 落库） |
| `provider_test.go`（cli） | 需求 §5.12（`provider list`）/ §5.13 + 宪法·架构约束（凭证零泄露）/ §2.4 裁决注 2（失败语义）/ 裁决注 9（展示层**防御性**脱敏——配置面已拒绝，展示层不依赖上一层） |
| `config_test.go`（追加） | 前序改造点 1 的**三条**新规则（重名拒绝、`base_url` 必填、`base_url` 成分拒绝——各一例；依赖方向由 `internal/cli/ports_test.go` 扩展禁止清单判卷（`cli` 不 import `provider`——analysis 阶段补入）；后者含"文案不回显原 URL 与其中的值"断言） |
| `realcall_integration_test.go` | 编程指南 §4.2 验收 Demo 的正 / 反两例（真实调用 + 不可达端点也落审计）；正例含 usage 分支判据与显式截止时间 |
| `testutil/provider_test.go` | 编程指南 §8.3 第 2 项（测试基础设施自判卷） |

**治理五关键词的 grep 判据**（编程指南 §8.3 第 2 项）：

```bash
grep -rn "Cancel" internal/provider/*_test.go        # 取消
grep -rn "Concurrent" internal/provider/*_test.go internal/storage/llm_calls_test.go   # 并发
grep -rn "CrashWindow" internal/storage/llm_calls_test.go    # 崩溃恢复（窗口形态；全量恢复归 US-4）
grep -rn "ErrorResponse\|HTTPError" internal/provider/*_test.go   # 错误模型响应
grep -rn "AuditPreWriteFails\|TerminalWriteFailure" internal/provider/*_test.go   # 审计写入失败（含取消路径的终态写失败）
grep -rn "TwoHops" internal/provider/*_test.go    # 未知字段**两跳**往返（P1-1 的验收判据）
grep -rn "EchoingKey\|RedactsBaseURL" internal/provider/*_test.go internal/cli/*_test.go   # 脱敏（错误面 / 展示面两侧）
grep -rn "RejectsProviderBaseURL\|RejectsBaseURLWithUnsupportedParts" internal/provider/*_test.go internal/cli/*_test.go   # base_url 拒绝（装载层 / 构造层两层）
grep -rn "ResponseRead\|OverLimit\|EncodedHashInPath\|BigIntegers" internal/provider/*_test.go internal/cli/*_test.go   # 实现评审回合（P1×2 / P2×2 / 优化×2）的回归
```

> **崩溃恢复的诚实边界**：本 US **无**恢复扫描（契约 §4.3.2 归 US-4）——本 US 判的是"崩溃会留下什么"（`pending` 行、无半写），不是"重启后怎么收口"。US-4 落位恢复后，`TestLLMCallRepo_CrashWindowLeavesPendingForRecovery` 的期望值不变（行仍是 `pending` 形态，归一化发生在恢复时）。

### 4.3 关键回归测试写出代码（断言逐条对应 FR）

```go
// TestProviderService_AuditPendingBeforeCall —— 留痕顺序（契约 §7.2：预写先于发出）。
// 判据不靠时序猜测：假 Provider 在 **Chat 内部**当场查库，此时该次调用的 pending 行
// 必须已经存在——"先发出、后补记"的实现会在这里判红。
func TestProviderService_AuditPendingBeforeCall(t *testing.T) {
    ctx := context.Background()
    db := openTempDB(t)                       // 真库（temp dir + storage.OpenWritable）
    repo := storage.NewLLMCallRepo(db)

    var pendingRows int
    probe := fakeProviderFunc(func(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
        // 此刻调用尚未返回；预写必须已经落库。
        pendingRows = countRows(t, db, "SELECT count(*) FROM llm_calls WHERE status = 'pending'")
        return core.ChatResponse{Content: "ok"}, nil
    })

    svc := provider.NewProviderService(
        map[string]core.ProviderClient{"fake": probe}, map[string]string{"fake": "m"},
        repo, slog.New(slog.NewTextHandler(io.Discard, nil)))

    _, err := svc.Chat(ctx, core.ChatRequest{
        Provider: "fake", RunID: "run-test", Round: 1, Messages: []core.Message{{Role: "user", Content: "hi"}},
    })
    require.NoError(t, err)
    require.Equal(t, 1, pendingRows, "发出之前必须已存在 pending 行（fail-closed 预写）")
}

// fakeProviderFunc 把函数适配成 core.ProviderClient——判"留痕顺序"需要在 Chat **内部**查库，
// 固定脚本的 testutil.FakeProvider 给不了这个探针位。其余用例一律用 testutil.FakeProvider。
type fakeProviderFunc func(context.Context, core.ChatRequest) (core.ChatResponse, error)

func (f fakeProviderFunc) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
    return f(ctx, req)
}
```

```go
// TestProviderService_RefusesCallWhenAuditPreWriteFails —— 审计写入失败（宪法 V 的可执行判据）。
// 断言的是**客户端未被调用**：审计不可用时"拒绝发出"，而不是"照发再抱怨"。
func TestProviderService_RefusesCallWhenAuditPreWriteFails(t *testing.T) {
    ctx := context.Background()
    fakeP := testutil.NewFakeProvider(testutil.FakeStep{Response: core.ChatResponse{Content: "never"}})
    svc := provider.NewProviderService(
        map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"},
        failingStore{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

    _, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-test", Round: 1})

    require.Error(t, err, "预写失败必须返回错误")
    require.Equal(t, 0, fakeP.CallCount(), "预写失败时**不得**发出调用（fail-closed）")
}

// failingStore 是 core.LLMCallStore 的失败注入实现：预写必失败。
type failingStore struct{}

func (failingStore) BeginCall(context.Context, core.LLMCall) (int64, error) {
    return 0, errors.New("audit unavailable")
}
func (failingStore) FinishCall(context.Context, int64, core.LLMCallResult) error { return nil }
```

```go
// TestLLMCallRepo_FinishCallTruncatesErrorMessageAt4KBWithoutBreakingRunes —— 契约 §7.4。
// 断言：长度上界是字节 4 KB；且末尾是合法 UTF-8（按 rune 边界回退，不切断半个汉字）。
func TestLLMCallRepo_FinishCallTruncatesErrorMessageAt4KBWithoutBreakingRunes(t *testing.T) {
    ctx := context.Background()
    repo := storage.NewLLMCallRepo(openTempDB(t))
    id, err := repo.BeginCall(ctx, core.LLMCall{RunID: "run-test", Round: 1, Provider: "fake", Model: "m"})
    require.NoError(t, err)

    long := strings.Repeat("超长错误", 4000)            // 远超 4 KB，且是多字节字符
    require.NoError(t, repo.FinishCall(ctx, id, core.LLMCallResult{
        Status: core.LLMCallFailed, ErrorMessage: long, ErrorKind: core.ErrorKindExternalError,
    }))

    got := readErrorMessage(t, repo, id)
    require.LessOrEqual(t, len(got), 4*1024, "截断上界 = 4 KB")
    require.True(t, utf8.ValidString(got), "截断不得切断 UTF-8 字符")
    require.NotEqual(t, long, got, "超长原文必须被截断")
}
```

```go
// TestProviderService_CancelDuringCallRecordsTimeoutKind —— 取消路径（契约 §4.2 + §7.3）。
// 同步方式：假 Provider 进入 Chat 后**关信号通道**，测试收到信号才取消——不用 time.Sleep
// 猜时序。Sleep 的坏处是具体的：取消可能落在预写完成之前，用例会走成 fail-closed 拒发那条
// 分支（判的就不是取消路径），而且时紧时松。
func TestProviderService_CancelDuringCallRecordsTimeoutKind(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    svc, repo, fakeP := newServiceWithRepo(t, testutil.FakeStep{Block: true})

    go func() {
        <-fakeP.Started(1)      // 调用确已进入（此时预写必然已完成）
        cancel()
    }()
    _, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-test", Round: 1})

    require.ErrorIs(t, err, context.Canceled, "取消错误链必须保留（US-4 据此判 cancelled）")
    row := readCallRow(t, repo)
    require.Equal(t, core.LLMCallFailed, row.Status, "取消也不得留下 pending 行")
    require.Equal(t, core.ErrorKindTimeout, row.ErrorKind)
    require.Equal(t, 1, fakeP.CallCount())
}
```

```go
// TestProviderService_PreCanceledContextStillAuditsFailedCall —— 调用前 ctx 已取消（§2.4 注 8）。
// 预写与取消解耦：行照落 pending → 发出（客户端立刻以 context.Canceled 失败）→ 终态
// failed + timeout。另一条路（预写用原 ctx）会让"取消"伪装成"审计不可用"的 fail-closed
// 拒发——与契约 §4.2 首句的理由（被取消的调用也必须落审计）相反。
func TestProviderService_PreCanceledContextStillAuditsFailedCall(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())
    cancel()                                     // 调用前就取消
    svc, repo, fakeP := newServiceWithRepo(t, testutil.FakeStep{Err: context.Canceled})

    _, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-test", Round: 1})

    require.ErrorIs(t, err, context.Canceled)
    row := readCallRow(t, repo)
    require.Equal(t, core.LLMCallFailed, row.Status, "预写已解耦——取消不改变'这次调用存在过'")
    require.Equal(t, core.ErrorKindTimeout, row.ErrorKind)
    require.Equal(t, 1, fakeP.CallCount(), "已取消不改变调用路径：仍走'预写→发出→终态'")
}

// TestProviderService_CanceledCallTerminalWriteFailurePreservesCancelChain —— 取消 + 终态写失败。
// 两条纪律一次判完：① 行留 pending（不伪造终态）；② 返回的**仍是取消错误**——终态写失败
// 只记 ERROR，不得覆盖调用结果（约束 7；契约 §4.2 第二句）。
func TestProviderService_CanceledCallTerminalWriteFailurePreservesCancelChain(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    db := openTempDB(t)
    repo := storage.NewLLMCallRepo(db)
    var logs bytes.Buffer
    fakeP := testutil.NewFakeProvider(testutil.FakeStep{Block: true})
    svc := provider.NewProviderService(
        map[string]core.ProviderClient{"fake": fakeP}, map[string]string{"fake": "m"},
        finishFailingStore{repo},                       // 预写真写、终态必失败
        slog.New(slog.NewTextHandler(&logs, nil)))

    go func() { <-fakeP.Started(1); cancel() }()
    _, err := svc.Chat(ctx, core.ChatRequest{Provider: "fake", RunID: "run-test", Round: 1})

    require.ErrorIs(t, err, context.Canceled, "终态写失败不得覆盖取消错误链")
    require.Contains(t, logs.String(), "审计终态回填失败", "残余窗口必须落 ERROR 日志")
    require.Equal(t, core.LLMCallPending, readCallRow(t, repo).Status, "行留在 pending，待启动恢复标 unknown")
}

// finishFailingStore 包一层真仓储：预写真写、终态必失败——残余窗口的注入点。
// 嵌入接口只覆盖 FinishCall，其余方法原样转发（注入面保持最小）。
type finishFailingStore struct{ core.LLMCallStore }

func (finishFailingStore) FinishCall(context.Context, int64, core.LLMCallResult) error {
    return errors.New("audit write failed")
}
```

```go
// TestProviderService_ProviderErrorEchoingKeyNeverLandsInAuditOrLogs —— 凭证三路（约束 8）。
// 假服务把 Authorization 里的 key **原样回显**进错误正文（现实里供应商确实会这么干）。
// 客户端必须在出口前抹掉它——判据是"三处都不含"，不是"我们记得别打印"。
func TestProviderService_ProviderErrorEchoingKeyNeverLandsInAuditOrLogs(t *testing.T) {
    const key = "sk-synthetic-echo-probe"        // 合成值：非真凭证，也不触发仓库的密钥扫描模式
    ctx := context.Background()
    db := openTempDB(t)
    repo := storage.NewLLMCallRepo(db)

    srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
        StatusCode: http.StatusUnauthorized,
        Body:       `{"error":{"message":"invalid key: ` + key + `","type":"invalid_request_error"}}`,
    })
    client, err := provider.NewOpenAIClient(provider.Options{Name: "echo", BaseURL: srv.URL(), APIKey: key})
    require.NoError(t, err)

    var logs bytes.Buffer
    svc := provider.NewProviderService(map[string]core.ProviderClient{"echo": client},
        map[string]string{"echo": "m"}, repo, slog.New(slog.NewTextHandler(&logs, nil)))

    _, chatErr := svc.Chat(ctx, core.ChatRequest{Provider: "echo", RunID: "run-test", Round: 1,
        Messages: []core.Message{{Role: "user", Content: "hi"}}})

    require.Error(t, chatErr, "401 必须失败")
    require.NotContains(t, chatErr.Error(), key, "返回错误不得含 key")
    require.NotContains(t, logs.String(), key, "日志缓冲不得含 key")
    require.NotContains(t, readCallRow(t, repo).ErrorMessage, key, "审计 error_message 不得含 key")
}
```

```go
// TestOpenAIClient_RoundTripsUnknownFieldsTwoHops —— 技术方案 §4.4 约束 3 的验收（两跳，P1-1）。
// 缺一不可：① 假服务在响应消息里放一个库不认识的字段；② 把解析结果交回客户端发第二次调用，
// 假服务收到的第二个请求体**必须仍含该字段**。只对 core.Message 做一次 JSON 往返检不出
// "库的编解码把未知键丢了"——那正是本条要防的失效。
func TestOpenAIClient_RoundTripsUnknownFieldsTwoHops(t *testing.T) {
    ctx := context.Background()
    const unknown = "vendor_thought_signature"
    srv := testutil.NewFakeOpenAIServer(t,
        testutil.FakeOpenAIResponse{Body: `{"choices":[{"message":{"role":"assistant","content":"ok",` +
            `"reasoning_content":"think","` + unknown + `":"sig-1"}}],"usage":{"total_tokens":3}}`},
        testutil.FakeOpenAIResponse{Body: `{"choices":[{"message":{"role":"assistant","content":"ok2"}}]}`})

    client, err := provider.NewOpenAIClient(provider.Options{Name: "vendor", BaseURL: srv.URL(), APIKey: "k"})
    require.NoError(t, err)

    // 第一跳：未知字段进 ProviderExtra；reasoning_content 是已知字段，走显式列。
    resp, err := client.Chat(ctx, core.ChatRequest{Model: "m",
        Messages: []core.Message{{Role: "user", Content: "hi"}}})
    require.NoError(t, err)
    require.Equal(t, "think", resp.ReasoningContent)
    require.Equal(t, "sig-1", resp.ProviderExtra[unknown])

    // 第二跳：把该消息交回——请求体里必须仍带这个键（原名、原值）。
    _, err = client.Chat(ctx, core.ChatRequest{Model: "m", Messages: []core.Message{
        {Role: "assistant", Content: "ok", ReasoningContent: resp.ReasoningContent,
            ProviderExtra: resp.ProviderExtra},
    }})
    require.NoError(t, err)

    bodies := srv.Requests()
    require.Len(t, bodies, 2)
    sent := bodies[1]["messages"].([]any)[0].(map[string]any)
    require.Equal(t, "sig-1", sent[unknown], "未知字段必须在第二次请求里原样回发")
    require.Equal(t, "think", sent["reasoning_content"], "已知扩展字段（思考内容）同样不得丢")
}
```

```go
// TestConfig_RejectsProviderBaseURLWithUserinfoQueryFragment —— 第 8 类 fail-closed（§2.4 注 9）。
// 判决两个对象：① **拒绝**——含这些成分的地址不得生效（静默裁剪会让配置在运行时"变味"，
// 认证 / 路由错误由此变成难定位的故障）；② **文案纪律**——只点名键路径与成分名，不回显原 URL、
// 也不回显其中的值（userinfo 里可能正是一份凭证）。子用例各带一个"敏感片段"，逐例断言它
// 不出现在错误文案里（与 US-1 的"明文不回显"同口径）。
func TestConfig_RejectsProviderBaseURLWithUserinfoQueryFragment(t *testing.T) {
    cases := []struct{ name, baseURL, sensitive string }{
        {"userinfo", "https://user-x9:secret-7f@api.example.com/v1", "secret-7f"},
        {"query", "https://api.example.com/v1?route=9f3a", "route=9f3a"},
        {"fragment", "https://api.example.com/v1#alt-2b", "alt-2b"},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            path := writeConfig(t, providerConfigWithBaseURL(tc.baseURL)) // US-1 既有夹具
            _, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_KEY": "resolved-secret-value"}))

            require.Error(t, err, "含 %s 的 base_url 必须被拒绝", tc.name)
            require.Contains(t, err.Error(), "providers[0].base_url", "必须点名键路径")
            require.Contains(t, err.Error(), tc.name, "必须点出不支持的成分")
            require.NotContains(t, err.Error(), tc.baseURL, "不得回显原 URL")
            require.NotContains(t, err.Error(), tc.sensitive, "不得回显其中的值")
        })
    }
}

// providerConfigWithBaseURL 以 US-1 的 baseConfig 为底，只替换 base_url 一行——
// 子用例之间的唯一变量就是被拒的那个成分。
func providerConfigWithBaseURL(baseURL string) string {
    return strings.Replace(baseConfig, "base_url: http://${DEMO_HOST}:8080/v1", "base_url: "+baseURL, 1)
}
```

> 断言写法上仍标「以实测为准（写前核实）」的两处：① 出站 `stream` 键的形态（库 tag 是否带 `omitempty`）——判据写成"**不得为 true**"，两种形态都成立；② `url.Parse` 对异常输入的宽容度（判据是**两层都拒绝**：装载期的第 8 类规则由上方用例判卷、构造期的防御校验由 `TestOpenAIClient_RejectsBaseURLWithUnsupportedParts` 判卷——宽容度只决定要不要补显式校验，不影响判据本身）。**已撤销的第三处**：未知字段经 `ExtraFields` 往返——源码核实后该路径在 v1.43.0 不存在，改由上方两跳用例判卷。

## 5. 范围边界（核心阶段不做项，逐项标注依据）

| 不做项 | 依据 |
| --- | --- |
| ReAct 循环、ToolExecutor、工具注册与执行（`ToolCalls` 只透传） | 技术方案 §3.3 + §4；编程指南 §4.4（US-4） |
| 调用重试、fallback 链（A 失败切 B）、流式 | 技术方案 §3.3（**运行时不会自动重试、崩溃恢复不会自动重放**——契约 §3.3 承诺边界） |
| embedding / 图片 / 语音（只做 Chat Completions） | 技术方案 §3.3 |
| Anthropic 原生协议适配（新增 `ProviderClient` 实现的场景） | 技术方案 §3.2（扩展阶段） |
| Agent 加载、Profile（`provider` / `model` / `temperature` 的来源）、`profile` 四命令 | 编程指南 §4.3（US-3）；DA §5.13 |
| Run 级预算 / 并发准入 / 取消判定 / `run_id` 生成 / 会话历史写入 | 技术方案 §4.1 / §4.5 / §4.6；契约 §6（US-4） |
| 配置快照与 `config_hash` 填充（本 US 写 NULL） | 技术方案 §11.5；§2.4 裁决注 3（US-4 / US-11） |
| 启动恢复扫描（`pending` → `unknown`、`running` → `failed`、半截组补齐） | 契约 §4.3.2；编程指南 §4.4（US-4） |
| 实例锁与写模式命令准入（`provider list` 是只读轻命令，不涉锁） | 契约 §4.3.1；US-1 §6 小探针读数（实现归 US-4） |
| HTTP 端点与错误码映射（`NOT_CONFIGURED` 只是本 US 哨兵的对外映射位） | 技术方案 §7；契约 §8（US-9） |
| `chat` / `serve` / `gateway` 命令（12 条命令中的其余 9 条） | 技术方案 §8.7；本 US 只交付 `provider list` |
| 监控指标实装（`MetricsRegistry` 仍为 no-op） | US-1 范围边界；观测属扩展阶段（US-11） |
| 审计查询面 / 审计导出（`llm_calls` 只写不查） | 需求 §6 扩展功能表；宪法 X |

## 6. 验证与验收（剩余人工项）

| 项 | 做法与判据 |
| --- | --- |
| 门禁全绿 | 干净检出 + 完整前置（同 US-1 §3.3）：`make check` 七目标全绿（本轮当场输出为准——完成证据纪律，编程指南 §8.2 第 6 条）。**本 US 的覆盖率读数变化**：`internal/core` 与 `internal/provider` 由此纳入判卷（`core` ≥ 70% 门槛生效；**已按 §2.2 约束 10 设计为"core 只放声明"**） |
| **覆盖率现状实测（2026-10-16 复跑 `sh scripts/check-coverage.sh`）** | 退出码 0，末行 `check-coverage: 全部包达标`；两条判卷行为 `skip  github.com/ymedword/basalt/internal/core  无可执行语句` 与 `skip  github.com/ymedword/basalt/internal/provider  无可执行语句`（模块路径逐字，中间空白已压缩）；其余包达标，关键三条：`internal/storage 76.26% (>= 70%)`、`internal/cli 77.06% (>= 50%)`、`internal/app 81.13% (>= 50%)`。**这是设计约束 10 的原始读数**，US-2 落地后必须重测——若 `core` 出现语句而无测试文件，判卷会以 0% 判失败 |
| 验收 Demo（编程指南 §4.2，**人执行**；AI 给核对清单） | 前置：① `basalt init` 得工作区（集成用例另用**独立临时库**，不复用开发库）；② `basalt.yaml` 配**两家** provider（`api_key` 一律 `${VAR}` 占位）；③ 真实凭证经环境变量注入（**不得**落盘 / 落日志 / 进对话）；④ **显式执行开关** `BASALT_REAL_CALL=1`（与凭证 env 同时门控，缺一即 `t.Skip` 写明理由）。执行：`BASALT_REAL_CALL=1 BASALT_DEMO_DIR=<dir> go test -tags=integration -count=1 -run TestProviderService ./internal/app/`。判据：① 真实调用成功一次（用例入口 `context.WithTimeout` **显式设截止**——60 秒测试常量，不是配置键）；② 库中可见 `SELECT provider, model, total_tokens FROM llm_calls`：`status=success`，**token 三列与响应一致**——`usage` 可空不是失败（三列 0 仍判成功）；**不得**把 `token > 0` 写成无条件判据，也**不得**由 `total_tokens > 0` 反推"供应商返回了 usage"（已决事项 #16——「是否返回 usage」以对端原始响应为准，不以审计列反推）；③ 把 `base_url` 指向不可达地址重跑（**不需要凭证**）→ 失败也落审计（`failed` + `error_kind` 非空）；④ `basalt provider list` 列出两家，且**不含凭证值**（地址按 scheme://host/path 渲染——正常配置的 `base_url` 已不含 userinfo / query，见⑤；展示层的收窄渲染由 `TestProviderList_RedactsBaseURLUserinfoAndQuery` 判卷）；⑤ **负向一例（不需要凭证）**：把某家的 `base_url` 改成含 query / userinfo 的地址 → **装载期拒绝**，文案点名 `providers[i].base_url` 与成分且**不含该 URL**（`provider list` 与一切启动路径同判——§2.4 注 9）；⑥ **`t.Skip` 不算通过**：缺凭证时用例会 SKIP，收口报告必须区分 `--- PASS` 与 `--- SKIP`——跳过的那次**不构成**"真实调用验收通过" |
| 凭证复核 | 范围收窄为**运行对象**（同 US-1 口径）：`basalt.yaml.example` 与测试夹具零明文；`provider list` 与失败文案的三路输出（stdout / stderr / logger 缓冲）带 `NotContains` 断言；实例配置按 Operations §6 原式核（`grep -rnE "sk-\|api_key: [^$]" <实例目录>/basalt.yaml` 零命中）。**全仓 grep 会命中文档示例，不作为判据** |
| 装配确认（人工走查） | `go.mod` 无 Agent 框架 / ORM 坐标（宪法 I / IX）；业务代码 `context.WithValue` 零使用（宪法 VII）；审计 SQL 只在 `internal/storage`（全局不变量 3）；`core` 未 import 其他 internal 包与第三方（`go list -deps` 复核）；映射构建处无任何"取第一个 / 取唯一一个"形态 |
| 真实调用的环境独立性 | 集成用例只用临时工作区 + 环境变量；**零真实凭证**是 US-14 mock e2e 的口径，本 US 的真实调用**必须**由人执行、凭证**必须**经环境变量注入 |
| CI 与分支保护 | 沿用 US-1 已就位的双 job + 必需检查（**无需改动**）；本轮新增用例在既有 `test` job 内执行 |

## 已决事项

### 本 US 评审与复核裁决（用户，2026-10-16——R1 修订 + R2 复核；原「待决事项」#1~#5 与 R1 后新提的两项全部在列）

| # | 事项 | 裁决 |
| --- | --- | --- |
| 1 | `ProviderService` 的 `store` / `logger` 两字段（§2.4 注 6） | **同意**——属既定审计职责的显式依赖 |
| 2 | 推导符号集（原待决 #5） | **名称整体同意**；但**先修正类型与伪代码的不一致**（R1 已改：`LLMCall` 不带状态列、`LLMCallResult` 三 token 列显式平铺、模型解析为空的分支入伪代码并点名用例） |
| 3 | `testutil` 落位（技术方案 §13「涉及包」之外） | **同意**——编程指南 §8.3 第 2 项有明确依据 |
| 4 | 被取消 / 截止调用的 `error_kind`（原待决 #1） | **暂归 `timeout`**，明示这是**分类归并**；`context.Canceled` / `DeadlineExceeded` 的**错误链区分保留**。附：契约的 `unknown` **并非只允许恢复路径产生**（R1 已改约束 11 与流程 D：`unknown` 兜底 + 恢复扫描同值不同因） |
| 5 | `provider list` 列集（原待决 #2） | **同意首版文本列集**（name / base_url / default_model + 凭证状态 + 汇总）；URL 按敏感信息规则处理（去 userinfo / query——R1 落为 §2.4 注 9） |
| 6 | 客户端超时（原待决 #3） | **同意不设自有超时**（由 `ctx` 承载）；**真实验收入口必须显式设截止时间**（R1 已写进 §6 判据①） |
| 7 | 真实调用验收的执行形态（原待决 #4） | **同意 integration + env 门控**；**增加显式执行开关**、**使用独立临时库**；**缺凭证的跳过不得记为"真实调用验收通过"**（R1 已写进 §4.1 与 §6 判据⑥——R2 在 §6 插入新的判据⑤后顺延） |
| 8 | SDK 角色收窄（R1 后新提 #1；§2.4 注 7） | **同意**——保持坐标与版本；自建 HTTP 层**限定在 `provider` 内**，承担请求 / 响应 / 扩展字段透传；取消传播、禁止重定向、错误脱敏、两跳透传测试一并沿用。**技术方案 §4.4 的透传承诺保留**，只补实现方式；「唯一路径」措辞改为「**本 US 选定路线**」（不把实现选择写成技术必然） |
| 9 | `base_url` 的地址成分处置（R1 后新提 #2；§2.4 注 9） | **改为装载期明确拒绝**：userinfo / query 拒绝，**fragment 一并明确拒绝**——**不采用**"接受后静默丢弃"（静默删改会改变用户配置的含义，把认证 / 路由错误变成难定位的运行时故障）。新增一条规则 + 对应测试（第 8 类 fail-closed）；报错**只点名 `providers[…].base_url` 与不支持的成分**，不回显原 URL 或其中的值；**发送层保留防御校验**；正常 scheme / host / path 原样保留；将来需要 query 时再显式定义支持范围。Harness 已补拒绝与脱敏断言（§3.2 / §4.3） |

### module-dev 步骤 5 任务比对裁决（用户，2026-10-16）

| # | 事项 | 裁决 |
| --- | --- | --- |
| 10 | FR-007「未知」兜底类别的用例落位（R2 复核携带的实际问题：§3.2 的 17 条服务层用例里没有独立用例名，§4.2 只在测试类一级记录"三类取值 + `unknown` 兜底"） | **新增独立用例** `TestProviderService_UnclassifiedErrorRecordedAsUnknownKind`（服务层；裸错误 → `failed` + `unknown`）；默认层计数 **45 → 46**（service **17 → 18**），§3.2 / §4.1 / §4.2 与 spec / plan / tasks 的计数同步更新。**不**采用"挂进既有用例作子用例"（映射句没有可对号的用例名，会弱化核对） |

### 实现评审回合裁决（用户，2026-10-16——外部代码评审 2×P1 + 2×P2 + 2 优化，用户以临时探针逐条复现；修完再终验）

| # | 事项 | 裁决 |
| --- | --- | --- |
| 11 | **P1**：读取响应体期间取消被误记为 `external_error`（`client.go` 读取失败统一归外部错误，`service.go` 直接采信该类别——客户端类别与终态回填类别同时错） | **修**：读取阶段补取消 / 截止分类——`timeout` + 错误链保留（罕见形态下 `errors.Join` 把 ctx 错误补进链）；回归两条（客户端 + 审计面），判据含 `Status == 200` 与文案「未取完响应」（钉死"确在读取阶段"，与传输阶段「未取得响应」分野） |
| 12 | **P1**：未知字段中的大整数无法原样透传（解进 `any` 变 float64，`9007199254740993` 两跳后回发成 `…992`；嵌套结构同受影响） | **修**：未知键解码改 `json.Decoder` + `UseNumber()`（`json.Number` 原样保留字面量）；**判据取原始出站 JSON**——假服务的解码视图也会 float64 化，用它判卷会掩盖同一缺陷。透传位类型（`map[string]any`）不变，契约层字面量零改动 |
| 13 | **P2**：尾随空 `#` 绕过校验并改变请求路径（`base_url=http://host/v1#` 被接受，拼接端点整段落进 fragment，服务器只见 `/v1`） | **修**：两层（装载层 + 构造层）同判**字面 `#`**——`url.URL` 没有 `ForceFragment`（query 侧才有 `ForceQuery`），空分隔符只能从原始串上找；正例守卫：路径里的 `%23` 是合法编码，不连坐 |
| 14 | **P2**：缺少 `message` 的响应被当成成功（`{"choices":[{}]}` 与 `message:null` 均返回零值响应 + `error=nil`，服务随后写 `success`） | **修**：首条 choice 必须有 `message` 对象（缺失 / `null` / 非对象一律判死）；正例不连坐——`content:null` + `tool_calls` 仍判成功（FR-006） |
| 15 | **优化**：响应体读取无大小上限（`io.ReadAll` 无条件读到 EOF；4 KB 是**读完之后**的文案截断，管不住读取时内存） | **修**：`io.LimitReader(上限 + 1)`，成功体与错误体同一条读路径同判；超限安全失败且正文一个字不外带。上限取**内部常量** 4 MiB——**不**做配置键（不得新建表外对外概念）；用例按同值字面量钉住，上限被调大而判据未同步时立刻判红 |
| 16 | **优化**：真实调用测试的 token 断言过强（由 `total_tokens > 0` 反推"供应商返回了 usage"，强于「审计用量与响应一致」的既定判据） | **修**：撤除该推断，只留三列 **Equal**（与响应一致）的既定判据；总数在"usage 缺失"与"usage 在但两列皆 0"之间不可分，拿它当分支条件等于用不可靠读数收窄判据。quickstart §场景 4 与 §3.2 / §6 的判据文案同步改口径 |
| 17 | 收口报告口径 | 「**需求映射覆盖 100%**」= US 需求 → 用例的**映射**覆盖率，与**行为验证结果**（哪些用例 PASS / SKIP、哪些人工项未执行）分开陈述；不得用映射覆盖代替行为结论 |

### 自 US-1 带入，本 US 沿用

- **契约状态**：运行时契约四件套在对应链路验证通过前为**待验证设计草案**——**不构成** US-2 开工阻断；本 US 引用的是 `llm_calls` DDL（表结构字面量）与 §7.2 / §7.3 / §4.2 三条纪律，均属已交付事实。
- **五链路原型分阶段执行（口径丙）**：链路 1 / 3 / 4 在 US-4 交付后立即验证；本 US 的"真实模型调用往返"是**契约最终冻结条件之一**（契约文件头部的附加条件），随本 US 验收 Demo 落地，**不**改五链路的分批安排。
- **退出码口径**：成功 0 / 失败 1，原因由文案区分（US-1 已决；`provider list` 沿用）。
- **依赖方向裁决（D1，2026-10-15）**：接口定义在消费方、实现由装配层注入——本 US 同口径落地（`core.ProviderClient` / `core.LLMCallStore` 声明在 `core`，实现在 `provider` / `storage`，装配在 `app`）。
- **Windows 锁手段**（US-1 探针已决）：与 `provider list`（只读轻命令）无关；本 US 不触碰锁文件。

## 待决事项

> **当前为空。** 原待决 #1~#5（2026-10-16 R1 评审）与 R1 后新提出的两项（SDK 角色收窄、`base_url` 拒绝规则；同日 R2 复核）均已裁决并落案，见「已决事项」第 1~9 行。设计面已无待决项——实现期间若冒出新的未定口径，回到本节登记后再动手，不静默自决（module-doc-gen 规则 6）。

## 附录 A：易漏维度核对（US-2 适用性）

| 维度 | US-2 处理 |
| --- | --- |
| 审计关联标识传导 | **适用（本 US 的核心面）**：`run_id` / `session_id` / `round` 随 `ChatRequest` 显式传入 → `BeginCall` 落列；`run_id` 空或 `round < 1` → 拒绝发出且不写行（§2.2 约束 4、流程 B 步骤 2）。`run_id` 的产生归 US-4（§2.4 裁决注 4） |
| 上下文对象传递机制 | **适用**：映射表 / 模型表 / 仓储 / 日志器 / 关联字段一律显式类型化参数；`ctx` 只承载取消 / 截止；**审计的两处写入（预写与终态回填）**都经 `auditContext`（`context.WithoutCancel` + 独立 5s——契约 §4.2，§2.4 注 8）；收尾 grep `context.WithValue` 零使用 |
| 前序改造点清单 | **适用**：§3.5 四条（cli 配置校验两处 / core 哨兵 / cli 注册 / provider doc.go），逐条为软门禁 |
| 后续模块能力占位 | **适用**：§3.5 六条（`Tools` 字段 → US-4；`config_hash` → US-4/11；`Chat` 的生产调用方 → US-4；假 Provider 复用 → US-4/14；`pending → unknown` → US-4；`ErrorKind` 共用 → US-4/5） |
| 缓存语义 | **适用**：客户端与映射表在装配时**一次构建、进程内只读**（并发安全的前提）；配置改动**重启生效**（热加载归 US-11）；`ProviderService` 不做响应缓存（无缓存语义——每次调用都真发） |
| 边界行为返回语义（技术方案未规定项） | **适用**：被取消调用的 `error_kind`、`provider list` 列集、客户端超时、验收执行形态 → 均已在「已决事项·本 US 评审与复核裁决」中落案（原待决 #1~#4，加 R2 的 #8 SDK 角色收窄、#9 `base_url` 拒绝规则）；**待决事项已清空**。已有出处的边界写在流程 D 矩阵（未注册 / 关联缺失 / 模型不可解析 / 装载期校验不过 / usage 可空 / 截断 / 3xx / 取消） |
| 并发泄漏类回归 | **适用**：`TestProviderService_ConcurrentCallsAreRaceFree`（含取消路径）与 `TestLLMCallRepo_ConcurrentBeginCall`；`make test` 全量 `-race`；假 Provider / 假服务自身并发安全（用例判卷）；客户端连接随响应释放（取消用例判连接不悬挂） |
| 留痕顺序 | **适用（本 US 的核心判据）**：`BeginCall` 早于发出——由 `TestProviderService_AuditPendingBeforeCall`（在假客户端内部当场查库）判卷；终态回填失败**不得**伪造终态（只记 ERROR、留 `pending`） |
| 配置占位符解析语义 | **适用（承接 US-1，不新造）**：`api_key` 的 `${VAR}` 解析、缺失 / 空值 / 明文 / 残留四类拒绝全部在 US-1 的 `cli.LoadConfig`；本 US 只消费解析结果，**不**新增解析面、**不**新增凭证键；`provider list` 的输出与错误文案均不含凭证值 |
| LLM 响应字段可空性 | **适用**：`usage` 可空（三列 0、不判失败——验收判据同口径，§6）；带工具调用的响应 `content` 可为空（只透传，不判失败）；`reasoning_content` 可空但**不得丢**（透传 + 两跳断言）；`provider_extra` 承载**协议未在 `core.Message` 显式建模的未知字段**（出站平铺 / 入站收集都由客户端自己做，不经库的编解码——§2.4 注 7） |
| 失败信息回填闭环 | **不适用（US-4 起）**：本 US 无工具循环，"回填进对话历史"由 US-4 的循环负责；本 US 的对应面是失败原文落 `error_message`（截断 4 KB）并上抛错误给调用方（不吞） |
| 依赖初始化副作用 | **适用**：`go-openai v1.43.0`（只用协议类型——无 `import _`、无 `init()`、无后台 goroutine）与自建 `http.Client`（无后台 goroutine、代理按标准库缺省）；坐标先核实（本轮读数见 §2.2 约束 9）；详见 §3.5 依赖副作用 |
| 可观测性双轨 | **适用**：审计轨（`llm_calls` 两阶段写）+ 日志轨（预写失败 / 终态写失败 = ERROR；路由与调用失败 = WARN；字段固定键、不记凭证、不记全量 URL）。**脱敏三路同判**：返回错误 / 日志缓冲 / `error_message` 在"供应商回显 key"的用例里一起断言（约束 8） |

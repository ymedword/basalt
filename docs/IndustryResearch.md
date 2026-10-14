# Basalt 行业调研：Agent OS 版图、Go 生态位与 Basalt 定位

> 本文档是 Basalt 四份权威文档中的**调研篇（Why）**，回答"为什么要有 Basalt、它站在哪个位置"。需求篇（What）、技术方案（How）、AI 编程指南（How 落地）分别见同目录其余三份文档（`DemandAnalysis.md`、`TechnicalSolution.md`、`AiProgrammingGuide.md`）。四份文档各司其职，细节不一致时以技术方案为准。**配套工件**（三件套）：[Compatibility.md](Compatibility.md)（与 NivroOS 的逐项三态对照）、[contracts/](contracts/)（运行时契约四件）、[Operations.md](Operations.md)（装、看、备份、升级、凭证）。
>
> Basalt 是基于 **Go** 实现、面向企业场景的 **Agent OS**：装在企业自己的服务器或 K8s 上，作为统一底座运行多个业务 Agent（运维助手、客服助手、HR 助手、销售助手、知识管理助手等），共享渠道接入、模型路由、工具调用、记忆系统与沙箱执行能力；**单一静态二进制、零外部依赖**，数据完全留在企业自己的基础设施。
>
> Basalt（玄武岩）取"基岩"之意——坚硬、致密、分布最广的火山岩，是立东西的那层石头。项目定位同样如此：不是某个业务 Agent，而是让任意 Agent 可靠运行的**底座**。
>
> 本项目的设计血脉来自兄弟项目 **NivroOS**（同设计、Java 实现，第一阶段已交付：`mvn clean verify` 667 测试 0 失败）。NivroOS 用 Java 技术栈验证了"企业内自托管 Agent 运行时底座"的设计可行性，Basalt 用 Go 重新实现它，并把 NivroOS 实践沉淀出的全部经验与教训作为 day-one 设计输入。两个仓库各自独立演进，不复刻彼此的模块划分。
>
> 本文所有版本号、星数、活跃度均为 **2026-10-14 实测**（核实方式见附录 B），随时点变化。例外：§4.2 的 PicoClaw / GoClaw 两条**星数与活跃度**以**检索摘要**为准（github.com 直取被本机网络策略拦，未做 API 实测），已在附录 B 逐条标注；**GoClaw 的许可不属例外**——已按仓库 main 分支 LICENSE 文件核对为 CC BY-NC 4.0（2026-10-14），以仓库内文件为准，不采信 GitHub 元数据。

---

## 一、什么是 Agent OS

### 1.1 让一个 Agent "能干活"需要什么

LLM 本身只会"说话"。要让 LLM 变成能真正干活的 Agent，一个执行内核至少要提供五件事：

1. **对接 LLM**——把各家大模型的 API 差异吸收掉，让上层不感知调的是哪家
2. **推理循环**——模型想一步、调一个工具、看结果、再想下一步，循环到任务完成（ReAct）
3. **工具执行**——文件读写、执行命令、发 HTTP、调企业内部系统，且要能扩展
4. **上下文与记忆**——会话历史、跨会话的长期记忆，每轮组装进提示词
5. **受控执行**——工具不能为所欲为：路径白名单、命令白名单、域名白名单

这五件事构成一个 **agent runtime**（Agent 运行时）。

### 1.2 从 runtime 到 Agent OS

企业里从来不是"跑一个 Agent"，而是"一群 Agent、很多人、很多系统"。这时 runtime 之上还需要 OS 级的能力：

- **多个 Agent 的并存管理**——一个实例上同时跑多个业务 Agent，各自配置、互不干扰
- **统一的接入与出口**——业务系统通过一套 REST API 接入所有 Agent；人通过 CLI / IM 接入
- **共享的记忆与知识**——同一用户的偏好、同一项目的约定，跨 Agent、跨会话沉淀
- **主动触发的调度**——定时任务到点自动跑（每日日报类场景），不只被动等人来问
- **可审计**——每一次 LLM 调用和工具执行都留痕，回答"它当时为什么那样做"

借操作系统类比：**runtime 像单个进程的执行环境，Agent OS 像管理一群进程、提供共享服务与治理的那层**。一句话——runtime 让一个 Agent 跑起来，**Agent OS 让一群 Agent 在企业里被管起来**。

### 1.3 与相邻概念的区别

| 概念 | 代表 | 解决什么 | 不解决什么 |
| --- | --- | --- | --- |
| **LLM 客户端/SDK** | openai-go、anthropic-sdk-go | 一家模型的 API 调用 | 循环、工具、记忆、治理一概不做 |
| **Agent 开发框架** | eino、adk-go、langchaingo、trpc-agent-go | "写一个 Agent 程序"的脚手架：编排、组件、抽象 | 不负责托管运行、不提供企业治理（它产出的是代码，不是底座） |
| **编排平台** | Dify、n8n、Coze | 用图/流程把 LLM 调用串起来，偏可视化低代码 | 流程是预先编排的，Agent 的自主循环只是其中一个节点；运行形态是平台，不是可嵌入的底座 |
| **K8s 原生 Agent 运行时** | kagent、agent-sandbox | 在已有 K8s 集群里以 CRD/Pod 形态跑 Agent | 前提是你已经有一整套 K8s；对"一台服务器装一个二进制"的场景过重 |
| **托管 Agent 中台** | 各家云厂商 Agent 平台 | 云上开箱即用 | 数据出域、生态锁定，与严监管企业诉求相反 |
| **Agent OS** | OpenClaw、Hermes Agent、**Basalt** | 自托管、多 Agent 并存、共享五大能力、可审计的底座 | 不做业务流程编排，不替业务方决定 Agent 干什么 |

### 1.4 部署形态

Agent OS 的部署形态决定了它服务谁：

- **云托管**——厂商替你运行，数据经过厂商。个人和轻量团队友好，严监管企业不可用
- **自托管单机/单集群**——装在企业自己的服务器或 K8s 上，**实例即边界**，数据不出域。这是企业场景的事实选择，也是 Basalt 的部署形态
- **嵌入式**——作为库被业务系统直接引入。Go 的静态链接让这条路在 Basalt 里天然可行（扩展阶段考虑）

---

## 二、业界两个最具代表性的开源项目

### 2.1 OpenClaw（TypeScript，个人级 Agent 的爆发样本）

2025 年 11 月发布、作者 Peter Steinberger，到 2026-10-14 已 **391,943 星**（实测），是历史上增长最快的开源 Agent 项目之一。它把"个人 AI 助手"做到极致：任意平台、任意 IM 接入、大量现成 Skill、本地执行。

它的成功验证了两件事：**Agent 自主循环 + 工具执行 + 本地记忆**这条技术路线成立；**markdown 描述 + 现成工具生态**这种低门槛扩展方式成立。

它的教训同样深刻：

- **安全债**：CVE-2026-25253（`gatewayUrl` 参数未校验，页面加载即把网关令牌发给攻击者服务器，形成一键 RCE，CVSS 8.8，v2026.1.29 才修复）；同类还有 CVE-2026-26319（webhook 鉴权缺失）、CVE-2026-27488（定时任务 webhook SSRF）；2026 年 1 月的一次审计中发现 **512 个漏洞**；公开测绘显示约 **42,665 个实例暴露在互联网上**
- **供应链风险**：ClawHavoc 事件中 1,184 个恶意 Skill 进入其技能市场——"零代码扩展生态"如果不带治理，就是攻击面
- **定位**：个人场景优先，多租户、审计、企业 IT 集成不在设计中心

### 2.2 Hermes Agent（Python，NousResearch）

2026 年 2 月发布，到 2026-10-14 已 **253,638 星**（实测）。主打"会成长的 Agent"：从对话中自动提炼记忆、持续进化，记忆系统设计是它的核心卖点。

它验证了**长期记忆是 Agent 区别于 chatbot 的关键体验**——用户用一段时间后，Agent 自然记住偏好与背景，不用反复解释。这一点被 Basalt 直接吸收为核心能力之三。

### 2.3 两个项目共同留下的空白

1. **企业治理空白**——两者都偏个人/小团队。多 Agent 并存管理、审计落库、最小权限沙箱、统一 REST 集成，都不在其设计中心
2. **安全教训空白**——爆发式增长换来的安全债（一键 RCE、恶意技能、暴露实例）说明：Agent 这类"能执行动作"的系统，**安全必须 day one 设计，不能事后补**
3. **企业自托管位未见同口径者（但不得表述为"这一位空着"）**——Agent OS 层已被 Node.js（OpenClaw）与 Python（Hermes）各占一个生态；Go 侧也已有项目（PicoClaw 走极简单二进制个人助手、GoClaw 走多 Agent 团队平台，见 §4.2），**"Go 这层是空的"这个说法已不成立**——差异化不立在语言空白或星数上，而立在**可验证行为**（见 §4.2 末的能力对照）

---

## 三、企业为什么需要私有可控的 Agent 底座

### 3.1 需求是真实的，且集中在大中型组织

Agent 从演示走向生产是 2025–2026 年的主线：头部机构的调研反复显示，企业采用率高但**进入生产环节的比例明显偏低**，卡点集中在可靠性、安全与合规，而不是"模型不够聪明"。企业缺的不是又一个 Agent 框架，而是**能把 Agent 安全托管起来的底座**。

### 3.2 严监管企业的铁律

银行、政府、能源等行业只有一条路：**数据不出域**。这意味着：

- 云托管 Agent 平台不可用；模型调用要么走私有推理（vLLM/Ollama），要么通过企业网关出网
- 每一个动作要能回答"谁、何时、对什么、做了什么、结果如何"——**审计是采购红线，不是加分项**
- 工具权限要能收敛：读哪个目录、跑哪些命令、访问哪些域名，必须可白名单、可关闭

### 3.3 价值锚点是知识沉淀，不是自动化率

企业真正长钱的不是"少雇几个人"，而是**经验留在系统里**：故障处理记录、客户背景、项目约定——这些是它的真实资产。Agent 的长期记忆与管理层可见的审计记录，让知识从员工个人流向组织。这是"自托管底座 + 长期记忆 + 审计表"三件事绑在一起的价值主张。

---

## 四、Go 生态在这件事上的位置

### 4.1 分层盘点（2026-10-14 实测）

| 层 | 代表项目 | 星数 | 语言 | 定位 |
| --- | --- | --- | --- | --- |
| Agent 开发框架 | cloudwego/eino | 13,305 | Go | "The ultimate LLM/AI application development framework in Go"（字节系） |
| Agent 开发框架 | tmc/langchaingo | 9,723 | Go | LangChain 的 Go 移植，v0.1.x 仍是预 1.0 |
| Agent 开发框架 | google/adk-go | 8,872 | Go | Google Agent Development Kit 的 Go 版 |
| Agent 开发框架 | trpc-group/trpc-agent-go | 1,862 | Go | 腾讯 tRPC 生态的 Agent 框架 |
| K8s 原生运行时 | kubernetes-sigs/agent-sandbox | 4,248 | Go | 在 K8s 上管理隔离、有状态的 Agent 工作负载 |
| K8s 原生运行时 | kagent-dev/kagent | 4,018 | Go | CNCF 生态（topics 含 cncf），"Cloud Native Agentic AI" |
| 多 Agent 协作 OS | agentscope-ai/AgentTeams | 5,747 | Go | 阿里 AgentScope 系，基于 Matrix 房间的多 Agent 协作 |
| MCP 客户端/服务端 | mark3labs/mcp-go | 9,161 | Go | 社区 MCP 实现 |
| MCP 客户端/服务端 | modelcontextprotocol/go-sdk | 5,216 | Go | 官方 Go SDK（v1.8.0），与 Google 协作维护 |

各行均处于活跃状态（上述仓库最近推送均在 2026-10 内，实测）。

### 4.2 空白确认（"空白"到底还剩什么）

先纠正一条早先的说法：**"Go 这层是空的"不成立**。Go 侧已有两个体量项目（下节对照），语言空白论应当废弃。Basalt 的主张由此改立在**可验证差异**上，不立在"这一位没人占"上——四条事实：

1. **框架层已经被覆盖且会继续被覆盖**（eino/adk-go/langchaingo/trpc-agent-go）——这层解决"**写** Agent 程序"，框架会接管循环与编排。Basalt 不在这里竞争
2. **K8s 原生运行时是给"已有集群"的企业用的**（kagent/agent-sandbox）——重、与集群绑定；恰好漏掉"一台服务器 + 一个二进制装好就跑"的长尾现实（大量中小银行、政府科室、制造业子公司就是这种环境）
3. **Go 侧的单体 Agent 运行时已有项目，与 Basalt 的距离要按版本与部署形态分开量**：**PicoClaw**（sipeed，MIT）把自己明确定位成"极简单二进制个人助手"——目标是 `$10` 板卡与 `<10MB` 内存，会话落成扁平 JSONL 文件，文档自述 v1.0 前不建议生产使用，这一档与 Basalt 不同位；**GoClaw**（nextlevelbuilder）定位"OpenClaw rebuilt in Go"，做的是**多 Agent 团队平台**——多租户隔离、三层记忆、8 阶段流水线，**服务模式**要求 PostgreSQL 18 + pgvector（对照此档，Basalt 的差异是"零外部服务单二进制"），**另有 SQLite 形态的「Lite」部署档**（对照此档，"零外部依赖"不再构成差异，差异改立在审计 day-one 落库 + 白名单沙箱 + 目录协议可互换这些**可验证行为**上）。两者都不与 Basalt 同口径，但原因各不相同——PicoClaw 是**定位不在**（个人极简助手），GoClaw 是**许可与口径**：main 分支 LICENSE 为 CC BY-NC 4.0（公开许可不授权商业使用、商用需另获许可，见下节对照），多租户 / 5 层安全均为自家声明而非可复核证据
4. **多 Agent 协作 OS 的最大同名物（AgentTeams）是云厂商的协作产品**，与"装在客户服务器上的单二进制底座"不同位；GitHub 检索 `"agent os" language:go` 也未见同定位的体量项目

**能力对照**（只列可验证维度，不列星数排名）：

| 维度 | PicoClaw | GoClaw | Basalt |
| --- | --- | --- | --- |
| 形态定位 | 个人助手（`$10` 板卡、`<10MB` 内存） | 多 Agent 团队平台（服务模式 / SQLite「Lite」两档） | 企业自托管 Agent OS（单二进制） |
| 外部依赖 | 单二进制；会话为 JSONL 文件 | 服务模式需 PostgreSQL 18 + pgvector；**SQLite「Lite」档此差异消失** | 单二进制 + 嵌入式 SQLite，**零外部服务** |
| 多租户 | 无（个人） | 有（PostgreSQL 行级安全） | 核心阶段无（内网单实例，扩展阶段再做） |
| 安全 | 未见白名单沙箱口径 | 声称 5 层安全 | **白名单三档 + 真实路径投影 + argv 直执 + fail-closed**，授权与执行两接口拆分（Policy / Runner） |
| 审计 | 未见审计表 | 未见 day-one 审计口径 | `runs` / `llm_calls` / `tool_invocations` **day-one 落库**，失败类别可解释 |
| 协议/互操作 | MCP | MCP | MCP + **`AGENT.md` 目录协议与 NivroOS 公共子集互操作**（见 [Compatibility.md](Compatibility.md)） |
| 许可 | MIT | **CC BY-NC 4.0**（main 分支 LICENSE 实测 2026-10-14，以仓库内文件为准）：公开许可**不授权商业使用**，商用需另获许可（CC 允许权利人另行提供商业授权），这会增加企业采购的评估成本 | **待定**（发布前定；"企业可商用"是**目标**而非既成事实——Basalt 自己的许可还没选） |

一句话：**Node.js 有 OpenClaw，Python 有 Hermes Agent，Go 有 PicoClaw 与 GoClaw——"Go 这层没人做"的说法早就不成立。**GoClaw 与 Basalt 的距离**按版本与部署形态分开量**：对它的 PostgreSQL 服务模式，Basalt 的差异是"零外部服务单二进制"；对它的 SQLite「Lite」档，这条差异消失，差异改立在**可验证行为**（day-one 审计落库、白名单沙箱、`AGENT.md` 目录协议互操作）与**许可**（对方是 CC BY-NC 4.0，商用需另获许可）两条上——Basalt 的目标位是"企业可商用、行为可复核"，不是"这一位没人占"；其中"企业可商用"是**目标**而非既成事实（Basalt 自身许可待定，见上表）。

### 4.3 Go 作为底座语言的六个技术支点

1. **单一静态二进制**——`CGO_ENABLED=0` 构建出的产物无运行时依赖，`scp` 一个文件即可部署；对比 Java 需要 JVM、Node/Python 需要运行时与包管理
2. **毫秒级启动**——CLI 命令无"启动 JVM 几秒"之痛；常驻服务冷启动可忽略，与 Python/Java 的秒级启动形成体验差
3. **交叉编译**——一条命令产出 linux/amd64、linux/arm64、windows、darwin 各平台产物；企业内网分发不需要目标机器装任何工具链
4. **内存与并发模型**——goroutine 天然适配"每请求一线程"的同步模型；同样负载下的常驻内存是 JVM 的数分之一，与"装在客户服务器上"的定位匹配
5. **云原生生态事实标准**——K8s、Docker、Terraform、kagent 皆 Go；企业运维工具链天然认识它，二次开发与交付阻力最小
6. **静态门禁生态完整**——`go vet`、staticcheck、gosec、govulncheck、`-race`、golangci-lint 全部成熟，可组成与 Java 侧 Spotless/SpotBugs/OWASP 对等的质量门禁

### 4.4 MCP：Go 生态已经就位

MCP（Model Context Protocol）是 Agent 工具接入的开放标准。Go 侧两条成熟路线同时存在：官方 `modelcontextprotocol/go-sdk`（v1.8.0，与 Google 协作维护）与社区 `mark3labs/mcp-go`（v1.2.1）。**这意味着 Basalt 的工具扩展不需要自造协议层**——轻代码扩展直接写 MCP server（任何语言），Basalt 作为 MCP Client 接入即可。

### 4.5 与 NivroOS 的血脉关系

为什么不直接用 NivroOS、而要再立一个 Go 仓库：

1. **部署现实**——NivroOS 交付的是 fat JAR + JVM；目标客户里有一大批只有"一台服务器、一个 systemd"的场景，单二进制是刚需
2. **经验蒸馏**——NivroOS 的第一阶段（US-1~US-7，667 测试 0 失败）已经把这类系统的坑几乎踩了一遍：审计表必须 day-one 落库、沙箱必须真实路径投影、进程树必须整棵杀、cron 字段制式必须显式裁决、凭证绝不能回显…… Basalt 从第一行代码起就带着这些结论，**不是重新摸索，是带着答案重修一遍**
3. **生态互补**——Java 生态（Spring 全家桶、企业中间件）与 Go 生态（云原生、单机部署）各服务一批企业。同一个设计长出两种部署形态，两边文档互相引用，互为验证

两个仓库是一次性、单向的蒸馏关系：Basalt 的创始文档吸收 NivroOS 的结论，此后各自独立演进，不回写。**逐项差异与共同验收样例**（`AGENT.md` 样例、cron 公共子集对拍、工具调用样例，基线 commit 与三态标记）见 [Compatibility.md](Compatibility.md)——"可互换"的范围以那份清单为准，不以本文的印象为准。

---

## 五、Basalt 定位与愿景

### 5.1 Basalt 是什么

**基于 Go 实现、面向企业场景的 Agent OS**。装在企业自己的服务器或 K8s 上，作为统一底座运行多个业务 Agent，共享渠道接入、模型路由、工具调用、记忆系统、沙箱执行与定时触发能力。单一静态二进制，数据不出域，不锁任何云生态。

### 5.2 锚在需求，不锚在概念

四个词：**统一、私有、易接入、可观测**。

| 目标 | 说明（Go 版） |
| --- | --- |
| **统一** | 企业内多个业务 Agent 共享同一套底座；上一个新 Agent = 放一个目录（一份 `AGENT.md`），不写后端代码 |
| **私有** | 数据完全留在企业基础设施；单二进制部署，无运行时依赖；不收集任何数据 |
| **易接入** | REST API 是集成唯一通道；Go 静态二进制 + 交叉编译让"装机"变成 `scp` + `systemctl start`；运维工具链复用云原生生态 |
| **可观测** | 结构化日志（log/slog JSON）、健康检查、审计三表 day-one 落库（`runs` 父表 + `llm_calls` + `tool_invocations`，按 `run_id` 可关联、失败可分类解释） |

### 5.3 边界：做什么，不做什么

- **做运行时，不做编排**——Basalt 不解析任务步骤、不做工作流引擎。任务怎么拆解执行是模型在 ReAct 循环里自己决定的
- **不做 Agent 开发框架的竞品**——eino/adk-go 解决"写 Agent 程序"，Basalt 解决"托管 Agent 运行"。用框架写的组件可经协议（MCP / HTTP）接入为 Basalt 的工具（进程外对接，本体不引入框架依赖），生态互补不竞争
- **不做 K8s Operator**（核心阶段）——单实例自托管优先；集群化是扩展阶段的信号驱动升级
- **不做云服务**——没有 SaaS 形态，不提供托管版

### 5.4 安全怎么不重蹈开路者的覆辙

OpenClaw 的教训（第 2.1 节）给出的直接结论，写进 Basalt 的 day-one 设计：

| 教训 | Basalt 的 day-one 对策 |
| --- | --- |
| 凭证泄漏（令牌经 URL 参数泄漏 → 一键 RCE） | 凭证只从环境变量注入；通知渠道地址**不得**出现在任何响应/日志/错误文案（对外只回 scheme+host），且**库里也只存 `${ENV_VAR}` 引用、发送时解析**（见 [Operations.md](Operations.md) §6） |
| 工具权限失控 | 应用层白名单沙箱三档（路径/命令/域名），真实路径投影、argv 直执、失败关闭；拒绝信息是给模型看的可读契约 |
| 生态即攻击面（1,184 个恶意 Skill） | 技能库只读（Agent 不得写库）；工具三档扩展里"能执行什么"由白名单收敛；审计表 day-one 落库让动作可回放 |
| 暴露实例（42,665 个） | **默认只监听 `127.0.0.1:8080`**，远程访问必须显式改配置（见 [Operations.md](Operations.md) §2）；文档明确"内网假设"；认证/接入网关在扩展阶段以"反向代理优先"出方案 |

### 5.5 跟业界项目的关系

| 项目/生态 | 关系 |
| --- | --- |
| OpenClaw / Hermes Agent | 同为 Agent OS 形态，占据个人/小团队生态位；Basalt 占企业自托管位。设计上互相学习（记忆系统、Skill 渐进披露形态借自业界事实标准） |
| PicoClaw（Go） | 同为 Go 单二进制，但定位是**个人极简助手**（`$10` 板卡、`<10MB` 内存、会话存 JSONL）；Basalt 定位是企业多 Agent 底座，两者目标场景不重叠，不构成竞品关系 |
| GoClaw（Go） | **位置最近的一个**：同样"Go 重写"、同样做多租户平台；对照必须**按版本与部署形态分开**——服务模式（PostgreSQL 18 + pgvector）下差异是"零外部服务单二进制"，SQLite「Lite」档下这条差异消失，差异改立在可验证行为（白名单沙箱 + 三张审计表 + 与 NivroOS 的目录协议互操作，见 [Compatibility.md](Compatibility.md)）与**许可**两条上——main 分支 LICENSE 实测为 **CC BY-NC 4.0**（2026-10-14，以仓库内文件为准）：公开许可**不授权商业使用**，商用需**另获许可**（CC 允许权利人另行提供商业授权），这会增加企业采购的评估成本。多租户 / 5 层安全是它的自家口径，Basalt 的口径是行为可复核 |
| eino / adk-go / langchaingo / trpc-agent-go | 互补：框架负责"写"，Basalt 负责"托管"。Basalt 的重代码工具档可封装它们写的组件 |
| kagent / agent-sandbox | 互补：集群场景用它们，单机/小集群场景用 Basalt；未来 Basalt 可出 K8s 部署形态（扩展阶段） |
| MCP 生态 | 直接采纳：轻代码扩展写 MCP server，Basalt 作 MCP Client；官方 Go SDK 已就位 |
| NivroOS | 血脉：同设计、Java 先行验证、Go 重新实现；文档互为参照 |

---

## 六、未来方向：从单机到分布式

按信号驱动升级，不做超前设计：

| 阶段 | 形态 | 升级信号 |
| --- | --- | --- |
| **第一阶段（核心）** | 单实例单二进制：五能力 + CLI + REST + 定时 + 审计 | — |
| **第二阶段（扩展）** | 加上企业治理层：认证（API Key/JWT）、多租户、SSO、完整审计查询、Tool Policy、管理台、多 IM Channel | 出现真实生产部署，开始有多人共用与合规要求 |
| **第三阶段（分布式）** | 多节点协同：会话与记忆的外部存储后端、任务分派、节点故障迁移 | 单节点撑不住或需要高可用 |

每一阶段都按"接口先行"预留：授权与执行是**两个接口**（`Policy.Check` / `CommandRunner.Run`），Memory 后端、Channel / Provider 也是接口 + 实现的结构。升级 = 加一个实现 + 装配层换一行，**调用方零改动**——但接口本身会随档位需要扩展（容器档正是"两接口各加一个实现 + 装配切换"），**不承诺"接口不变"**。

---

## 附录 A：术语

| 术语 | 定义 |
| --- | --- |
| **Agent（智能体）** | 一个具象的业务智能体，定义本体是一个目录 `.basalt/agents/<name>/`：`AGENT.md`（frontmatter 是运行配置、正文是任务指令）加可选 `scripts/`、`REFERENCE.md`。一个目录就是一个完整可用的业务 Agent |
| **Profile（配置）** | 底座内部的运行时配置对象，由 `AGENT.md` 的 frontmatter 派生（`deriveProfile`），决定这个 Agent 用什么 Provider、可用哪些 Tool、绑定哪些 Skill、有没有定时 |
| **Provider（供应商）** | LLM API 服务的抽象。Basalt 通过 OpenAI 兼容协议对接各家（DeepSeek/通义/Kimi/智谱/本地 vLLM 等），维护显式的 provider name → 客户端映射 |
| **ReAct 循环** | Agent 的核心工作机制：Reason + Act。模型思考是否调工具、调用后看结果、再决定下一步，直到给出最终响应 |
| **Tool（工具）** | Agent 可调用的外部能力。内置工具（文件/命令/HTTP/记忆/通知）+ Plugin 三档扩展（Agent 目录加 MCP / 自写 MCP server / 在 Basalt 代码内实现接口） |
| **Memory（记忆）** | Agent 跨对话保留的状态。核心阶段两层：会话记忆（SQLite 持久化）与长期记忆（`MEMORY.md` 与 `memory_entries` 表，按 Agent 分层 + 全局回退层） |
| **Skill（技能）** | 公共实体存 `.basalt/skills/<name>/SKILL.md`；Agent 在 `AGENT.md` frontmatter 的 `skills:` 里**声明式绑定**（声明即绑定，各平台一致）。每轮只注入 name/description/路径（L1），模型按需 `read_file` 读正文（L2）、取附属资源（L3）。Skill 不是 Tool，不进工具注册表 |
| **Channel（渠道）** | Agent 对外的消息入口。核心阶段内置 CLI；IM 渠道在扩展阶段 |
| **Session（会话）** | 用户与 Agent 一次对话的上下文容器，按 channel+user+profile 联合标识，持久化到 SQLite |
| **Sandbox（沙箱）** | 工具执行的受控边界，拆成授权与执行两个接口：`Policy.Check`（能不能做）+ `CommandRunner.Run`（怎么做）。核心阶段唯一组合 = `WhitelistPolicy`（应用层白名单：路径/命令/域名）+ `LocalRunner`（进程内本地执行） |
| **Bootstrap（引导文件）** | 注入系统提示词的上下文文件：`AGENTS.md`（项目级行为说明）、`SOUL.md`（人格）、`USER.md`（用户偏好，只读不写） |
| **Workspace（工作区）** | Basalt 实例的工作目录 `.basalt/`，包含 Agent 目录、技能库、记忆、配置、日志与 SQLite 数据库 |
| **MCP** | Model Context Protocol，Agent 工具接入的开放标准；Basalt 作 Client 连接外部 MCP server |

## 附录 B：参考资料与核实记录

**事实核实方式（2026-10-14）**：Go 工具链版本取自 `go.dev/VERSION?m=text`；Go 模块版本取自 `goproxy.cn`（proxy.golang.org 自本机网络不可达，等效镜像）的 `@latest`；仓库星数与活跃度取自 GitHub REST API（`api.github.com/repos/...` 与 search 端点）逐仓库实测；CVE 细节取自 NVD 与 GitHub Advisory 并交叉核对。

- Go 官方版本端点：[https://go.dev/VERSION?m=text](https://go.dev/VERSION?m=text)（实测 `go1.27.2`，2026-10-02 发布）
- OpenClaw：`openclaw/openclaw`（391,943★ 实测）；CVE-2026-25253：[NVD](https://nvd.nist.gov/vuln/detail/cve-2026-25253)、[GitHub Advisory GHSA-g8p2-7wf7-98mq](https://github.com/advisories/ghsa-g8p2-7wf7-98mq)
- Hermes Agent：`NousResearch/hermes-agent`（253,638★ 实测）
- Go Agent 框架：`cloudwego/eino`（13,305★）、`tmc/langchaingo`（9,723★）、`google/adk-go`（8,872★）、`trpc-group/trpc-agent-go`（1,862★）
- K8s 原生运行时：`kagent-dev/kagent`（4,018★，CNCF 生态，[kagent.dev](https://kagent.dev)）、`kubernetes-sigs/agent-sandbox`（4,248★）
- 多 Agent 协作 OS：`agentscope-ai/AgentTeams`（5,747★，阿里 AgentScope 系）
- Go 侧单体 Agent 运行时（**核实方式不同：github.com 直取被本机网络策略拦，以下两条星数 / 活跃度以检索摘要为准，未经 API 实测**）：`sipeed/picoclaw`（MIT，定位个人极简助手，官网 [picoclaw.io](https://picoclaw.io)，v0.2.x）、`nextlevelbuilder/goclaw`（"OpenClaw rebuilt in Go"，多 Agent 团队平台，服务模式 PostgreSQL 18 + pgvector、另有 SQLite「Lite」档，官网 [goclaw.sh](https://goclaw.sh)；**许可 = CC BY-NC 4.0**——以仓库 main 分支 LICENSE 文件实测为准（2026-10-14），GitHub 元数据仍显示 `NOASSERTION`，**两处矛盾时以仓库内 LICENSE 文件为准**）
- MCP Go SDK：`modelcontextprotocol/go-sdk`（5,216★，官方，v1.8.0）、`mark3labs/mcp-go`（9,161★，v1.2.1）
- 兄弟项目：NivroOS（Java 实现，第一阶段已交付 667 测试 0 失败，见其仓库 `README.md` 与 `docs/`）

> 本附录中所有星数为 2026-10-14 快照；开源项目的星数与定位会随时间变化，引用时以实测日为准。

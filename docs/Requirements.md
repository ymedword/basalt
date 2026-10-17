# Basalt 需求文档：用户任务、运行承诺与交付范围

Basalt 的产品单位是一次可追溯的 Agent 运行：业务人员定义任务，平台维护者控制可执行动作，使用者得到结果和可解释的失败。本文件按用户任务、资产生命周期和对外承诺组织需求，作为 US 立项与验收的输入。

阅读顺序：先确认 §1 的适用边界，再从 §2 找到业务结果；§3～§5 定义产品行为，§6 定义质量证据，§7 管理交付范围。正文列出的是需求，交付与验证状态以模块记录为准。

实现设计由 [技术方案](TechnicalDesign.md) 裁决；API、状态与字段的固定字面量由 [公共契约](contracts/) 定义；部署程序见 [Operations.md](Operations.md)。历史文档名与旧章节号见 [引用索引](Compatibility.md#document-reference-index)。

<a id="basalt-at-a-glance"></a>

## Basalt 是什么

**一个底座二进制**：用一个 Go 程序承载多个业务 Agent，实例数据由部署方自托管。
**一个工作区**：Agent 目录、技能、配置、记忆与 SQLite 状态保存在可管理的工作区。
**一条统一执行链**：CLI、REST 和定时触发按对应 US 交付后，复用相同的运行、工具与审计规则。

| 层次 | 在 Basalt 中承担什么 |
| --- | --- |
| Agent runtime | 推进一次 Run：模型调用、推理循环、上下文、工具、预算、终止和恢复 |
| Agent OS / 管理底座 | 管理多个 Agent 的目录资产、共享能力、接入、调度、配置和观测 |

五大核心能力共同构成这一交付物，下面的详细条款再规定它们的边界：

| 能力 | 一句话说明 |
| --- | --- |
| Provider | 接入模型服务，显式选择供应商并记录调用结果 |
| ReAct | 在预算内推进模型与工具交互，明确何时完成、失败或停止 |
| Memory | 保存会话与跨会话背景，按作用域和配额供后续运行使用 |
| Tool | 用明确权限接文件、命令、HTTP、MCP 与已规划的业务工具 |
| Web Service | 提供业务系统接入和管理所需的 REST / 展示面，与 CLI 共用运行能力 |

以上是既定交付范围的导览，不表示所有模块已经实现；当前状态仍查 US 记录。模型、MCP 和通知等外部依赖按配置接入，自托管不会自动消除出站数据流。

<a id="product-boundary"></a>

## 1. 使用者、部署边界与成功条件

<a id="product-users"></a>

### 1.1 谁负责定义、运行和治理任务

| 使用者 | 要完成的工作 | 产品应提供的支持 |
| --- | --- | --- |
| Agent 维护者 | 定义指令、工具、技能和触发计划 | 可版本化的目录、加载期校验、草稿与人工编辑 |
| 调用者 / 业务系统 | 发起任务、继续会话、获取结果 | CLI 与 REST 接入、明确的运行状态和失败结果 |
| 实例运维者 | 安装、配额、凭证、备份和故障恢复 | 单二进制、本地工作区、受控配置和可关联审计 |

首版面向由同一管理方控制的自托管实例。工作区和 SQLite 由部署方管理；模型 API、MCP Server、通知目的地和脚本解释器仍是显式配置的外部依赖。部署在内网不等于请求不会出网，实际数据流由这些配置共同决定。

底座运行不要求额外部署数据库、缓存或消息队列。D3 示例脚本需要 Python，部分 MCP Server 有自己的运行依赖，前端与官网构建需要 Node；这些要求分别登记，不转化为底座进程的通用运行前置。

<a id="product-success"></a>

### 1.2 验收从结果、权限和证据三方面判断

| 目标 | 含义 | 验收视角 |
| --- | --- | --- |
| **统一** | 企业内多个业务 Agent 共享一个底座；上一个新 Agent = 放一个目录，不写后端代码 | 新 Agent 从创建到可调用 ≤ 3 分钟（页面操作） |
| **私有** | 实例数据自托管、无遥测；模型与工具出站按配置开放；单二进制交付 | 断网环境（除模型端点外）可完整运行；`ss -tlnp` 无预期外监听 |
| **易接入** | REST 是集成唯一通道；技能 / MCP / 通知全是标准协议 | 外部系统不看源码，只按契约文档即可对接 |
| **可观测** | 结构化日志 + 审计三表 + 健康检查 | 任一回答可回放：按 `run_id` 串起 `llm_calls` + `tool_invocations` 还原全过程 |

**安全是地基，不是补丁**（贯穿全部功能的横切目标）：工具来源管控、最小权限、强制沙箱、凭证走环境变量、审计 day-one 落库。任何功能与安全冲突时，安全优先、功能挪扩展阶段。

<a id="product-capabilities"></a>

### 1.3 产品能力如何服务这些工作

| 需求对象 | 首版承诺 | 扩展边界 |
| --- | --- | --- |
| 任务定义 | `AGENT.md` 声明运行配置、指令、工具、技能和计划 | 生成草稿须经人编辑，不让模型自行授予权限 |
| 一次 Run | 统一接入，冻结配置，执行推理与工具交互，保存终态 | 同步执行；工具顺序调用，不加入并行编排 |
| 持续使用 | 会话持久化；长期记忆按 Agent 分层并提供全局回退 | 首版 markdown / SQLite；向量检索与 mem0 真服务属于扩展 |
| 外部动作 | 内置十工具、MCP 和代码实现三档接入 | 本地白名单与外部 MCP 的约束范围分别说明 |
| 服务交付 | REST、定时、通知、观测、Agent 管理与五套每日样例 | 多租户、SSO、RBAC、审批与强隔离需另行立项 |

<a id="product-scope-changes"></a>

### 1.4 范围变更如何获得依据

产品需求、技术设计与公共契约以本仓文档为依据；功能能否使用以对应 US 的实现和验收证据为准。迁移与兼容性记录见 [Compatibility.md](Compatibility.md)，不作为业务 Agent 的使用前提。

<a id="user-outcomes"></a>

## 2. 用户任务与业务验收样例

<a id="scenario-1"></a>

### 2.1 现场排查与自动巡检

值班人员维护一个 `ops-agent`，在受管实例中处理两种工作：

- 白天工程师在终端 `basalt chat` 问"订单服务昨天日志里有什么异常"，它读日志目录、归纳后回答
- 每天早上 8 点定时自动跑一次巡检，把结论通过通知渠道推到运维群
- 所有操作限于白名单目录与白名单命令；推送到群的地址是运维在管理台登记的，凭证不落明文

<a id="scenario-2"></a>

### 2.2 在新会话继续使用已保存的偏好

某工程师在 `ops-agent` 的会话里说"我们报表统一用万元为单位"。一周后同一 Agent 的**新会话**（跨会话）仍按这个偏好输出——长期记忆写在该 Agent 自己的层。若某次调用**没有 Agent 上下文**（如 CLI 顶层），写下的记忆落在全局回退层，对全部 Agent 生效。注入规则：Agent 层有内容就只注入自己的、不回退全局层，**两层不叠加**。

<a id="scenario-3"></a>

### 2.3 接入外部工具形成资料摘要

维护者为仓库资料汇总建立一个 Agent，接入流程需要满足以下条件：

1. 把现成 MCP server（GitHub）配置进 `mcp_servers.yaml`
2. 写一个 `AGENT.md`：声明 provider、工具、技能、定时（每天 9 点）
3. 完成后推送到部门群

全程不写代码；配置变更**热加载**（保存即下一次运行生效），涉及 `mcp_servers.yaml` 的变更**需重启**——上线成本是一条目录 + 一份通知渠道登记。

<a id="feature-examples"></a>

### 2.4 单项功能的可演示结果

| Demo | 场景 | 通过口径 |
| --- | --- | --- |
| Demo 一 | `basalt chat` 查天气并给出穿衣建议 | 多轮对话流畅；调用 HTTP 工具；审计三表有记录且按 `run_id` 关联 |
| Demo 二 | 跨对话记偏好 | 新会话仍按偏好输出；`MEMORY.md` 有对应条目 |
| Demo 三 | 零代码 PR digest | Agent 目录 + 现成 MCP server，全程不改代码 |
| Demo 四 | REST 同步调用 | 外部脚本用 curl 完成创建会话 → 发消息 → 取历史 |
| Demo 五 | 多端点联动 | `/profiles` + `/tools` + `/info` + `/memory` 串起来看运行时全貌 |

<a id="daily-workflows"></a>

### 2.5 五套每日工作流 D1–D5

本组与 §2.4 的功能 Demo 编号独立；由 US-14 交付可复制素材与验收证据。

| 编号 / 场景 | Agent 目录 | 每日时间（Asia/Shanghai） | 输入与业务结果 | 能力覆盖 |
| --- | --- | --- | --- | --- |
| D1 接口可用性巡检 | `service-watch` | `0 10 8 * * *` | 读取测试服务的健康快照，列出正常 / 异常 / 不可达及观测时间；一次通知汇总 | HTTP + 定时 + 通知 |
| D2 交接待办整理 | `handoff-review` | `0 25 8 * * *` | 读取虚构交接记录，按技能规则提取负责人、截止日与阻塞；缺字段标“待确认” | Skills L1/L2/L3 + 文件读取 |
| D3 CSV 数据质量检查 | `data-quality` | `0 40 8 * * *` | 标准库 Python 脚本检查样例 CSV 的缺失、重复和非法值；模型解释脚本统计，不重新猜数 | Shell + argv / cwd + 输出边界 |
| D4 项目风险跟踪 | `risk-watch` | `0 55 8 * * *` | 对比两天的虚构风险快照与归档记忆，列出新增、持续、解除项；首次运行明确没有历史基线；一次通知汇总 | Memory + 跨运行对照 |
| D5 知识库变更摘要 | `knowledge-diff` | `0 10 9 * * *` | 经只读 MCP 获取样例文档目录的版本 / 摘要，与本 Agent 的基线比较；无变化也发通知明确报告 | MCP + 受控读写 + 差异摘要 |

五个场景均需：①真实自动触发与一次人工补跑分别留证；②固定输入的业务结果可判定；③空输入 / 无变化及依赖失败不编造结果；④`task_executions` 关联 Run，模型与工具审计可追溯；⑤D1 / D4 / D5 经测试通知接收器确认实际送达，D2 / D3 不产生通知；⑥重复执行、跨触发点重叠和停机错过按既有调度契约处理。

素材位置、最小权限、脚本 / MCP 的独立依赖、记忆基线、时钟和证据格式见 [TechnicalDesign.md](TechnicalDesign.md) §8.1。发布验收区分“固定替身自动化”“真模型实跑”“每日计划现场核对”，任一必验项未完成则发布验收不记完成。

<a id="agent-assets"></a>

## 3. Agent 资产从创建到下线

<a id="workspace-init"></a>

### 3.1 准备实例工作区

`basalt init` 在当前目录创建 `.basalt/` 工作区，幂等——已存在的文件**不覆盖**：

```text
.basalt/
├── agents/             # 每个子目录 = 一个 Agent（AGENT.md + scripts/ + REFERENCE.md）
├── skills/             # 公共技能库（每个子目录 = 一个 SKILL.md 实体）
├── memory/
│   ├── MEMORY.md       # 长期记忆 · 全局层（## 核心记忆 / ## 归档记忆 两分区）
│   └── agents/<name>/  # 长期记忆 · Agent 层（缺省不存在，save_memory 时落盘）
├── mcp_servers.yaml    # MCP server 配置（默认空表）
├── sessions/           # 会话备用目录（数据在 SQLite 里）
├── logs/               # 结构化日志
├── archive/            # 归档区（Agent 归档删除的目标目录）
├── AGENTS.md           # Bootstrap：项目级行为说明
├── SOUL.md             # Bootstrap：人格定义
├── USER.md             # Bootstrap：用户偏好（只读不写）
└── basalt.db           # SQLite 数据库
```

<a id="agent-definition"></a>

### 3.2 声明任务与加载所需资源

`AGENT.md` 是 Agent 的唯一定义入口：

- **frontmatter（运行配置）**：`name`（必须等于目录名）、`description`、`identity`（人格短名 + prompt）、`provider`（name / model / temperature）、`tools`（工具名列表，**不声明 = 空池**）、`skills`（技能绑定列表，**声明即绑定**）、`mcp_servers`、`channels`、`bootstrap`、`settings`（`max_iterations` / `max_history_turns`）、`schedules`（定时任务）
- **正文（任务指令）**：注入系统提示词；可引用通知渠道名、相对路径脚本等
- **校验规则**：`name` 必须等于目录名（不一致整只不注册）；声明的 provider / tool / bootstrap / skill / mcp_server **必须都能解析**，任一解析不到该 Agent **整只不注册**并给点名 WARN；`model` 空白被拒（脚手架会写可见占位符「请在此填写模型名」）
- **技能绑定**：`skills:` 声明公共技能库目录名。L1 每轮注入 `name + description + 技能库真路径`；模型命中后 `read_file` 读 SKILL.md 正文（L2）；附属参考 / 脚本按需再取（L3）。不预载正文、不新增 `use_skill` 工具
- **`scripts/`**：Agent 的附属脚本。带 Agent 上下文执行时，子进程工作目录 = 该 Agent 自己的目录（相对路径按 Agent 目录解析）

<a id="agent-management"></a>

### 3.3 浏览、编辑、生成草稿和归档

**管理台（观测）**：内嵌在服务进程里的 Web 页面（无外部资源、无 CDN）：

- 观测页：Agent 列表 / 详情、工具、技能、MCP、通知渠道、沙箱白名单、会话、记忆、定时任务
- 一个 Agent 的运行全貌（用什么模型、能用什么工具、绑了什么技能、定时如何）一页可见
- Agent 配置在线编辑（写 `AGENT.md`，保存即生效——**下一次运行**使用新配置，无需重启；正在跑的运行用开始时的配置快照）
- 页面对模型产出的任何文本**不得**用 `innerHTML` / `v-html` 类方式渲染（存储型 XSS 防线）

**Agent 动态管理**：

- **一句话生成草稿**：输入名字 + 一句描述 → 模型起草 `AGENT.md`（及可能的附属文件）→ 人就地编辑（尤其定时任务、工具清单这类敏感项）→ 创建
- **模板脚手架创建**：名字 + 描述 → 写出最小可加载的 Agent 目录（默认工具含两个记忆工具）
- **归档删除**：注销定时 → 移出注册 → **取消该 Agent 在跑的 Run 并等待退出（有界等待；超时报错并恢复注册）** → 目录移入 `.basalt/archive/`（时间戳唯一化）；**会话 / 记忆 / 审计数据不动**（配置下线 ≠ 数据下线）；归档失败恢复注册（不留半残态）
- **工作区只读浏览**：页面钻进 Agent 目录看实际文件（真实路径投影校验，越界拒绝；单文件预览上限 256KB；**只读**，不提供写）
- 三条写通道（创建 / 提交文件 / 归档）与页面编辑器、目录热加载共用**同一段注册写入门**，没有第二套解析路径

<a id="file-ownership"></a>

### 3.4 文件归属与可达范围

| 路径 | 内容 | 变更方式 | Agent 文件工具可达 |
| --- | --- | --- | --- |
| `.basalt/agents/<name>/` | Agent 目录（配置 + 脚本 + 参考文档） | 页面 / 命令 / 手工编辑（**下一次运行**生效） | 读写**自己的**目录；读别的 Agent 目录需授权 |
| `.basalt/skills/` | 公共技能库 | 手工 / 页面（Agent **只读**） | 只读（技能 L2 必需） |
| `.basalt/memory/` | 长期记忆（全局层 + Agent 层） | 只经记忆工具 / 页面查看 | **不可达**（记忆只能由记忆工具写） |
| `.basalt/archive/` | 归档的 Agent 目录 | 归档删除 | **不可达** |
| `.basalt/mcp_servers.yaml` | MCP server 配置 | 手工编辑（**需重启**） | **不可达** |
| `.basalt/logs/` | 结构化日志 | 进程写 | **不可达** |
| `.basalt/basalt.db` | SQLite 数据库（会话 / 消息 / 审计 / 快照 / 任务状态） | 进程写（**不提供手工改库路径**） | **不可达** |
| `.basalt/AGENTS.md` / `SOUL.md` / `USER.md` | Bootstrap 文件 | 手工编辑（下一次运行生效） | **不可达** |

**可达范围即授权范围**：Agent 的文件工具只碰得到前两行，且写范围收窄为**当前 Agent 自己的目录**——`AGENT.md` 与技能库在**只读区**（写了也拒）；记忆载体留在可达范围之外，是"记忆只能由记忆工具写"这条性质的实现方式（详见技术方案 §4.3 授权矩阵）。**跨 Agent 读取默认被拒**，需管理员在 `sandbox.cross_agent_read` 显式授权（缺省空 = 一个都不许）。反向不成立：运维的页面编辑器与命令行**不受**这层白名单约束（它们动的是运维自己的配置），但各自的写入对象另有硬边界（只能改已注册的 Agent 名）。

<a id="startup-outcome"></a>

### 3.5 启动时的可见结果

```text
basalt serve
  → 读 basalt.yaml（校验 + 环境变量解析）→ 初始化工作区（缺省则建）→ 打开 SQLite（幂等建表）
  → 构建 Provider 映射表 → 注册内置工具 → 连 MCP server（失败跳过 + WARN）
  → 扫描 agents/ 逐个加载校验（失格整只跳过 + 点名 WARN）→ 注册定时任务
  → 启动 HTTP 服务 → 结构启动摘要日志（已注册 Agent 数 / 工具数 / MCP 连接数）
```

<a id="skill-disclosure"></a>

### 3.6 按需读取技能资源

```text
L1（每轮，系统提示词）   name + description + 技能库真路径
L2（模型命中后自主读）   read_file 读 SKILL.md 正文
L3（按需）               附属 REFERENCE.md / scripts 再取
```

<a id="run-promises"></a>

## 4. 一次执行对调用者的承诺

<a id="run-progress"></a>

### 4.1 推进任务、调用工具与停止

一次消息处理的固定链路：

1. 消息进 `AgentService`（统一入口），追加到会话历史
2. 组装提示词四部分：系统提示词（AGENT.md 正文 + Bootstrap + 技能卡片 + 当前时间）/ 记忆注入（会话历史 + 长期记忆）/ 最近 N 轮对话 / 可用工具列表
3. 调 Provider（写 `llm_calls`）
4. 无工具调用 → 返回最终响应
5. 有工具调用 → 逐个执行（沙箱校验 → 执行 → 写 `tool_invocations`），结果回填对话历史
6. 回到第 2 步，最多 `max_iterations` 轮（默认 10，Profile 可覆盖）

**行为契约**：工具调用一次响应里多个时**顺序执行**；上下文超长按**完整工具交互组**从最早整组丢弃（不做总结压缩）；全程同步阻塞（不引入异步编排）；工具失败的错误信息必须回填进对话上下文（模型看得到失败才能收敛）；达 `max_iterations` = **`limit_reached` 正常终止 + 部分结果**（HTTP 200，**不是错误**）。完整消息模型（`tool_calls` / `tool_call_id` / `reasoning_content` 必须原样回传）见技术方案 §2.3。

<a id="provider-routing"></a>

### 4.2 选择模型与解释调用失败

- 配置在应用配置文件（`basalt.yaml`）的 `providers` 段：每个 provider 一个条目（name / base_url / api_key / models 提示）
- **API key 一律 `${ENV_VAR}` 占位**，启动时解析；缺失或未解析给清晰报错，不静默失败
- **显式映射**：`name → 客户端实例` 的映射表在装配处一次构建；请求路由只查表，不猜、不扫
- 新增一家供应商 = 配置加一段（OpenAI 兼容协议内）；协议不兼容的供应商 = 新增一个协议适配实现（扩展阶段）
- 每次调用（成功 / 失败）落 `llm_calls` 审计

<a id="session-history"></a>

### 4.3 延续会话和保留历史

- `session_id` 由 `channel + user + profile` 联合生成（确定性：同一组合 = 同一会话）
- 会话元数据存 `sessions` 表、消息**逐条追加**进 `messages` 表（承载完整工具调用结构）；**进程重启后会话可继续**
- `status`：`active` / `archived`；归档不删数据
- 定时任务的会话：channel 与 user 都固定 `scheduler`，同一 Agent 历次触发复用同一会话
- **并发契约**：同一会话同时只允许一次运行（忙即 409，**不排队**）；不同会话并行；实例级并发有上限（超限 429）
- **取消与崩溃**：取消经 `ctx` 传播（含杀掉正在跑的进程树），被取消的运行照常落审计；进程崩溃后重启，未完成的运行标 `failed`、未定状态的调用标 `unknown`（**不猜测是否已产生副作用**）；**会话保持可继续**——未完成的工具交互组在取消 / 预算耗尽 / 崩溃后补齐为显式结果（**已取消 / 未执行（工具调用次数超限）/ 结果未知 / 未执行（进程中断），四种都不进审计**——合成只进会话历史，审计永远只记真实发生的事）；取消**优先于**预算判定（最后一轮最后一个工具执行中被取消，仍以 `cancelled` 收场，不得先落 `limit_reached`），同会话的下一条消息按正常路径完成；崩溃恢复只由**持工作区实例锁**（操作系统级独占文件锁——心跳只作诊断，**不得**据心跳过期抢锁）的启动进程执行（活实例在跑时，命令行与服务的写模式命令拒绝启动，只读命令不触发恢复）；恢复的工具组修复以**未完成交互组**为准绳（不只查 `running` 的 Run），且组的判定**按位置**——`tool_calls` 非空的 assistant 消息 + 其后**紧邻的连续 tool 消息段**，组内 id 未全覆盖即半截，**不得**全会话按 `tool_call_id` 全局查找（旧 Run 的同 id 结果行会造成误判），Run 状态修复与消息补齐**同事务提交**；补组后另扫一次 `runs`，把**没有半截组可补**的 `running` Run 也标 `failed`（等首次 LLM 响应 / 最终回答已落库而终态未写 / 无状态 invoke 三种崩溃正落在这一类）——恢复过程中再次崩溃不会留下永久半截组，也不会留下永远 `running` 的 Run；有外部副作用的调用（`http_post` / `notify`）**运行时不会自动重试、恢复不会自动重放**（承诺边界逐字见 [contracts/run-and-errors.md](contracts/run-and-errors.md) §3.3）

<a id="long-term-memory"></a>

### 4.4 长期记忆的读写与回退

**两层记忆**：

| 层 | 载体 | 生命周期 | 读写方 |
| --- | --- | --- | --- |
| **会话记忆** | SQLite `messages` 表（逐条追加，承载完整工具调用结构） | 随会话；重启不丢 | 底座自动读写 |
| **长期记忆** | `.basalt/memory/`（markdown 档）或 SQLite 表 | 永久 | 仅记忆工具（`save_memory` / `recall_memory`）可写 |

**长期记忆的行为契约**（全部为硬约束）：

- **按 Agent 分层**：Agent 层 `.basalt/memory/agents/<name>/MEMORY.md` + 全局回退层 `.basalt/memory/MEMORY.md`；Agent 层有内容不再注入全局层（**两层不叠加**，同一句话不出现两遍）；无 Agent 上下文（CLI 顶层调用）= 全局层
- **写入分区**：`save_memory` 由 Agent 显式指定写核心区还是归档区
- **核心区永远完整但有界**：截断与检索只作用于归档区；核心区有写配额（单条 ≤ 4 KB、每 Agent 总量 ≤ 32 KB），超限返回可读拒绝——"永不截断"的前提是有界
- **召回是关键词检索**：不分词、不扩展、不向量化（query 传单个关键词）
- **运行级快照**：每次**运行**开始重建配置与记忆快照、运行内冻结；手工编辑 `MEMORY.md` 在下一次运行生效
- **载体隔离**：`memory/` 目录**不在** Agent 文件工具的白名单里——记忆只能由记忆工具写
- **换后端 = 改一行配置**（`memory.backend`），作用域语义三档完全一致

<a id="conversation-events"></a>

### 4.5 对话处理的事件顺序

```text
消息 → AgentService.Run（统一入口：生成 run_id → 取配置快照 → 加载 Session → 准入检查）
  → ReAct 循环（预算有界）：组提示词 → 调 LLM（审计，带 run_id + round）→ 解析响应
      → 有工具调用：Policy 校验 → Runner 执行 → 审计（两阶段写）→ 结果回填 → 下一轮
      → 无工具调用：返回最终响应（completed）
      → 达迭代/调用次数上限：limit_reached（部分结果，HTTP 200）
      → 被取消：cancelled
  → 会话更新（消息逐条追加）→ Run 终态落库 → 响应返回渠道（CLI / REST）
```

<a id="memory-events"></a>

### 4.6 记忆更新的可观察行为

```text
save_memory（内容 + 分区 + 作用域=当前 Agent）
  → 记忆门面 → 按 memory.backend 选载体 → 写入（Agent 层或全局层）

recall_memory（单个关键词）
  → 记忆门面 → 按载体检索归档区 → 命中返回（找不到核心区）

提示词组装时的注入：
  → Agent 层有内容 → 注 Agent 层；否则 → 注全局层；两层不叠加
```

<a id="access-and-actions"></a>

## 5. 接入方式、动作权限与配置

<a id="tool-permissions"></a>

### 5.1 可调用工具与执行限制

**内置十工具**：

| Tool | 能力 | 沙箱档 |
| --- | --- | --- |
| `read_file` / `write_file` / `list_dir` | 文件读写列目录 | 路径白名单（真实路径投影，软连接感知）；**写范围 = 当前 Agent 自己的目录**；跨 Agent 读取需显式授权 |
| `shell` | 执行一个可执行文件 | 可执行名整串白名单；**argv 直执、不经 shell**；子进程**环境变量白名单**；超时 / 中断杀**进程树** |
| `http_get` / `http_post` | HTTP 请求 | 域名白名单（大小写不敏感）+ scheme 必须 http/https |
| `save_memory` / `recall_memory` | 长期记忆存取 | 不经文件沙箱（走记忆门面）；核心区写配额 |
| `notify` | 推送到通知渠道注册表 | 发送前过 HTTP 域名白名单 |
| `manage_schedule` | 改**当前 Agent 自己**的定时任务 | 与页面编辑器同一道注册写入门 |

**三档扩展**（选择原则：能低就不高）：

| 档 | 门槛 | 形态 |
| --- | --- | --- |
| 零代码 | 最低（主推） | 写 `AGENT.md` + 复用现成 MCP server |
| 轻代码 | 中 | 任意语言写 MCP server，登记进 `mcp_servers.yaml` |
| 重代码 | 高 | 在 Basalt 代码内实现工具接口，编译进二进制 |

**授权与执行分离**：`Policy.Check`（这个动作允许吗）+ `CommandRunner.Run`（怎么受控地执行）。核心阶段唯一实现 = 白名单授权 + 本地执行，四类动作（文件读 / 文件写 / 命令 / HTTP）：

- 文件**读**：当前 Agent 自己的目录 + `.basalt/skills`（技能 L2 必需）；**跨 Agent 读取默认被拒**（`sandbox.cross_agent_read` 显式列出才放行）
- 文件**写**：**收窄为当前 Agent 自己的目录**；技能库由代码常量拒绝（技能是共享库，一个 Agent 写坏全体受影响）；`AGENT.md` 与受保护配置文件拒写；`memory/` 不在白名单
- 命令：可执行文件名**整串逐字**比对；**不得**给 `bash` / `sh` 放行；**不得**继承调用进程完整环境（只透传白名单变量）
- HTTP：域名通配白名单；scheme 代码固定
- 目标与白名单根都先**投影为真实路径**（软连接感知）再比前缀；不可解析失败关闭
- **MCP server 的行为不受这套白名单约束**（独立进程，管理员在 `mcp_servers.yaml` 登记即信任）——v1 定位是"经管理员评审的脚本与 MCP server"，容器隔离属扩展阶段

**拒绝信息是模型可见契约**：四段式（规则 / 越界值 / 这是安全策略请勿反复重试 / 放行指引点名配置键 + 需重启）。失败同样落审计（`error_kind=sandbox_denied`）。

**通知渠道**：注册表（`notify_channels` 表）管理 `name / type / url / description`；类型 → 适配器是装配层显式映射；核心阶段两个适配器——通用 `webhook`（企微 / 钉钉 / 任意 HTTP 端点）与 `feishu`（只认响应体 `code == 0`，投递失败也回 HTTP 200）。**渠道地址是凭证**：对外（响应 / 日志 / 错误文案）只出现 scheme + host。

<a id="configuration"></a>

### 5.2 配置有效性和凭证

**应用配置**（`basalt.yaml`）与环境变量两级：

- 服务配置：监听地址（缺省 `127.0.0.1:8080`）、工作区路径、日志级别、沙箱白名单（路径 / 命令 / 域名 / 允许的跨 Agent 读取 / 环境变量透传清单）、记忆后端
- Provider 配置：见 §4.2；**API key 只从环境变量解析**，配置文件里只有 `${ENV_VAR}` 占位
- `mcp_servers.yaml`：MCP server（name / transport / command / env）；env 值同样只允许 `${VAR}` 占位
- 通知渠道地址同样**只以 `${ENV_VAR}` 引用形态入库**（`notify_channels.url` 列），发送时解析——凭证零明文落盘
- 加载期校验：必填与格式非法**明确报错、拒绝启动**，不静默取默认值

<a id="human-channel"></a>

### 5.3 人的交互入口

- `basalt chat` 交互式多轮对话：真实终端下按控制台编码收发（中文不乱码），管道 / 重定向下按 UTF-8
- `basalt chat --message "..."` 单条发送后退出（脚本化 / 冒烟用）
- CLI 显示工具的调用过程（谁被调了、成功与否），让"Agent 在干活"可见
- IM 渠道属扩展阶段；Channel 抽象（入站消息 → Agent、出站响应 → 渠道）核心阶段就立好

<a id="http-access"></a>

### 5.4 业务系统的 HTTP 接入

`basalt serve` 启动 REST 服务（默认 **`127.0.0.1:8080`**），统一前缀 `/api/v1`，10 个核心端点：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/sessions` | 创建会话 |
| `POST` | `/sessions/{id}/messages` | 发消息（触发 ReAct 循环） |
| `GET` | `/sessions/{id}` | 查会话历史 |
| `DELETE` | `/sessions/{id}` | 归档会话 |
| `POST` | `/agents/{name}/invoke` | 无状态调用 Agent |
| `GET` | `/profiles` | 列所有 Agent 配置 |
| `GET` | `/memory` | 查长期记忆（全局层） |
| `GET` | `/tools` | 列可用工具 |
| `GET` | `/health` | 健康检查 |
| `GET` | `/info` | 运行信息 + Provider 状态 |

**统一信封**：`{code, message, data, timestamp}`；错误码集中定义，异常统一翻译。**限制**：单条消息 ≤ 32KB；会话历史返回最近 100 条；Agent 调用 60 秒超时回 504；**并发准入**——同会话串行（忙回 409 `SESSION_BUSY`）、实例级上限（超回 429 `TOO_MANY_RUNS`），**不排队**。**核心阶段不做**：认证（内网假设）、SSE、WebSocket、限流（**429 是准入不是限流**——准入保护正在跑的 Run，限流防的是单位时间请求数；机制不同不混称）。

**监听默认 `127.0.0.1:8080`**——核心阶段无认证，默认只对本机开放；远程访问**必须显式改配置**并自行保证网络隔离。

管理台在核心阶段追加只读观测端点与管理端点（§3.3），不动上述 10 个核心端点。

<a id="scheduled-work"></a>

### 5.5 定时计划的产品语义

- 定义源：`AGENT.md` 的 `schedules` 段（cron + zone + message）；两张表（`scheduled_tasks` / `task_executions`）只存状态与历史，重启时从文件重新注册
- **cron 制式：6 段（秒 分 时 日 月 周）**；5 段表达式**加载期即拒**（该 Agent 整只不注册）
- **支持语法明确**：`*` / 数字 / 范围 / 列表 / 步长 / 月周缩写可用；星期取 0–6（Sunday = `0`），不支持 `7` / `L` / `W` / `#`。日与星期同时受限时解析器取 OR；公开样例至少一项为裸 `*`。固定起点验证未来 10 次触发时间，时区与 DST 单独实测；跨实现迁移边界见 Compatibility
- **zone 缺省 UTC**（不取服务器本机时区）；要"北京时间早上 8 点"必须显式 `Asia/Shanghai`（Basalt 侧经 `CRON_TZ=` 前缀实现）
- 任务 `id`：**单项任务可缺省**（派生 `<agent>-1`），**两项及以上每项必须显式声明**（否则列表重排会静默改变任务身份）；全局唯一，显式撞车 = 该 Agent 不注册；给派生任务补显式 `id` = 换身份（旧状态不跟随）
- 并发控制：每个任务一把锁——上一次没跑完、下一次到点则跳过（不排队、不并行）
- 失败只记日志不崩调度器；失败调用照常落审计
- **停机期间错过的触发点不补跑**（启动时记 WARN；要补跑走管理台 / 手动端点，与"不自动重试、恢复不重放"同一纪律）
- **重启对账**：`enabled` 以库为准、定义以文件为准（文件改动不回翻停用）；Agent 归档删除时其任务**置下线标记、不物理删**，历史执行记录保留
- 人可手动补跑一次，走同一条链路

<a id="schedule-events"></a>

### 5.6 到点触发时发生什么

```text
注册：读各 AGENT.md 的 schedules → 校验（6 段 / zone 合法 / id 全局唯一）→ 逐个登记
触发：到点 → 取锁（拿不到 = 上一轮未完成，跳过）→ 构造消息（channel=user=scheduler）
  → 走 9.2 完整链路 → 写 task_executions → 释放锁
```

<a id="process-modes"></a>

### 5.7 进程运行模式

| 模式 | 命令 | 用途 |
| --- | --- | --- |
| 交互模式 | `basalt chat` | 人在终端与单个 Agent 多轮对话 |
| 服务模式 | `basalt serve` | 常驻提供 REST + 管理台 + 定时器 |
| 守护模式 | `basalt gateway` | 常驻多 Channel 接入（核心阶段 = CLI + 定时；扩展阶段加 IM） |

<a id="cli-surface"></a>

### 5.8 命令入口清单

```bash
basalt init                      # 初始化 .basalt/ 工作区
basalt status                    # 查看配置与运行状态
basalt chat [--profile <name>]   # 交互式对话（--message 单条后退出）
basalt serve                     # 启动 HTTP API 服务
basalt gateway                   # 守护进程模式（多 Channel）
basalt profile list              # 列 Agent 配置
basalt profile create <name>     # 生成最小 AGENT.md 模板
basalt profile show <name>       # 查看单个配置
basalt profile delete <name>     # 删除配置
basalt provider list             # 列 Provider 及配置状态
basalt tool list                 # 列可用工具
basalt session list              # 列会话
```

> 版本查询用 `basalt --version`（全局选项，不计入 12 个命令）。

**两条路径**：轻命令（`init`、`profile list` / `profile show`）只读文件系统状态，毫秒级返回；重命令（涉及容器注册表或数据库）走完整装配；**`profile create` / `profile delete` 属写模式命令**——无装配但取工作区实例锁，锁被活实例持有时拒绝启动（指引改用 REST / 管理台，保存即生效）。**不得**为同一语义做两份实现（轻路径不许有写死的假数据）。

<a id="quality-evidence"></a>

## 6. 质量、运维与发布需要的证据

<a id="public-site"></a>

### 6.1 对外站点和快速开始的真实性

- **定位**：独立于四份权威文档的对外展示站点——讲清楚 Basalt 是什么、五大核心能力、架构与怎么快速开始；与核心代码同期发布，作为 `v0.1.0` 对外亮相的一部分
- **形态**：静态站点生成器（示例方案 vitepress，见技术方案 §1.3）；**仅构建期依赖**，不进单二进制；页面集合：首页（展示型落地页）/ 架构 / 快速开始；插图随仓库入库，页面无外部 CDN 资源
- **内容纪律**：页面与 README 每条能力必须对应已交付实现，**不得**写"即将支持"；README 快速开始与站点 quick-start 页同源，不写第二份口径
- **部署**：静态站点发布工作流随仓库入库；发布时机由人决定

<a id="nonfunctional"></a>

### 6.2 非功能目标

| 类别 | 要求 |
| --- | --- |
| **性能** | 验收目标：无 MCP、无外部探测的本地启动 < 1 秒；单实例 10 并发对话互不串号；REST 简单端点本地 P95 < 50ms（不含 LLM 时延）。记录 OS、CPU、工作区规模与样本数。MCP 串行握手预算另计，不能据此承诺所有配置都在 1 秒内启动 |
| **可靠性** | 单进程崩溃不丢数据（SQLite WAL + 会话落库）；定时任务失败不崩调度器；工具超时被杀进程树不留孤儿；崩溃后未完成的运行与工具调用有诚实标记（`failed` / `unknown`） |
| **执行预算（有界性）** | 一次运行五项预算缺一不可并有缺省值：最大迭代次数（10）/ 运行总超时（60s REST、300s 定时）/ 单工具超时（30s）/ 工具调用次数（50）/ 单工具输出（256 KB 截断）；同会话串行（409）+ 实例级并发上限（429），**不排队**；核心记忆写配额（单条 ≤ 4 KB、每 Agent ≤ 32 KB） |
| **兼容** | 交叉编译覆盖 linux/amd64、linux/arm64、windows、darwin；非法配置明确拒绝；公共子集内的资产按 [Compatibility.md](Compatibility.md) §5 的样例可互换加载，互操作范围与判据以该节为准 |
| **安全** | 凭证零明文（配置 / 日志 / 响应三处复核，含通知渠道地址以 `${ENV_VAR}` 引用入库）；白名单失败关闭；**文件写范围 = 当前 Agent 自己的目录**、技能库只读、跨 Agent 读取需显式授权；子进程不继承完整环境；通知地址对外只回 host；MCP server 的行为边界如实写明（不受白名单约束） |
| **可观测** | 结构化日志（JSON，含 session / trace 标识）；审计三表按 `run_id` 关联、day-one 写入、失败可分类（`error_kind`）；`/health` + `/info` |
| **可维护** | 质量门禁全绿（格式 / 静态检查 / 竞态检测 / 安全扫描 / 依赖漏洞扫描）；单测覆盖率有底线并逐步提高 |

<a id="operations-acceptance"></a>

### 6.3 部署、日志、备份与审计验收

- 目标机器不安装开发工具链也能部署（单二进制实测）
- 交叉编译出 linux/amd64 与 linux/arm64 两平台产物并在两种机器上启动通过
- 结构化日志可读（JSON 行）、`/health` 200、`/info` 显示 Provider 状态
- 默认只监听 `127.0.0.1:8080`；远程可访问须显式改配置（安全默认，见 [Operations.md](Operations.md)）
- 备份与恢复按 [Operations.md](Operations.md) 走通一遍（含**恢复验证**：从备份起一个实例并读到既有会话）
- 凭证：全仓 grep 无明文 key；通知渠道 `url` 列存 `${ENV_VAR}` 引用（发送时解析，解析失败点名环境变量）
- 审计：任一 Demo 跑完后，`runs` 有一条终态记录，`llm_calls` / `tool_invocations` 按 `run_id` 挂得上、失败项 `error_kind` 非空且归类正确

<a id="quality-gates"></a>

### 6.4 工程门禁与安全回归

- 格式 / 静态检查 / 竞态检测（`go test -race`）/ 安全扫描（gosec）/ 依赖漏洞扫描（govulncheck）全绿
- 单测覆盖率有底线并记录趋势；关键安全路径（沙箱三档 / 凭证红线 / 审计落库）有专项测试

<a id="runtime-proofs"></a>

### 6.5 运行可靠性必须证明的五条链路

以下五条链路是**底座可靠性的前置判据**，按所需实现的可用时点分阶段验证；不要求所有能力尚未落地时就全部跑通。执行时序与冻结条件见 [DevelopmentLoop.md](DevelopmentLoop.md) §5.2 / §10.4；每个阶段的实测不代替后续真实实现复验。

| 链路 | 通过口径 |
| --- | --- |
| 多轮工具调用 | 一轮内多个工具调用按顺序执行、结果按 `tool_call_id` 回填；思考模式模型连续三轮带工具调用**不报 400**（`reasoning_content` 全程回传） |
| 同会话并发 | 同一会话的第二个运行被拒（409 `SESSION_BUSY`、不排队）；跨会话并行互不阻塞；超实例并发上限回 429 |
| 执行取消 | 取消后子进程树被杀、Run 落 `cancelled`、审计有终态（`context.WithoutCancel` 兜住落库）；未完成工具组补齐为「已取消」合成结果；**取消后在同一会话再发一条消息，能正常跑完**（会话可继续） |
| 运行中热加载 | 运行进行中改 `AGENT.md`：本次运行以旧快照跑完、**下一次运行**用新快照——不出现"旧 Profile × 新正文"的混搭 |
| 崩溃后恢复 | `kill -9` 后重启（**只由持工作区实例锁的进程**做恢复；锁为**操作系统级独占文件锁**，心跳只作诊断、不据以抢锁）：`running` 的 Run 置 `failed`、`llm_calls` 与 `tool_invocations` 的 `pending` 行置 `unknown`，未完成工具组以消息为入口**按位置**扫出（assistant 组 + 其后紧邻的连续 tool 段，不得全会话按 `tool_call_id` 全局查找）并按审计补齐、补组后再扫 `runs` 收尾无半截组可补的 `running` Run（终态行还原真实结果 / `pending` 或 `unknown` 或缺失补「结果未知」「未执行」）；**同一会话的下一条消息能正常跑完**；库里不留"永远 running"的记录、也不留半截工具组；**恢复执行到一半再 `kill -9`，下次启动仍能补齐同一批组**（恢复可重入）；活实例在跑时写模式命令拒绝启动、被 `SIGSTOP` 暂停的实例同样**不得**被第二实例抢锁 |

<a id="stored-data"></a>

### 6.6 持久数据清单

**表 / 列 / 状态字面量的唯一事实源是 [contracts/schema.sql](contracts/schema.sql)**——本节只列表名与用途，**不得**在此另抄列清单（抄一份必漂移一处）。建表以 DDL 的实际执行结果为准；库结构与基线的差异登记在 [Compatibility.md](Compatibility.md) §2.5。

| 表 | 用途 |
| --- | --- |
| `sessions` | 会话元数据（消息不在本表——拆到 `messages`） |
| `messages` | 对话历史**逐条追加**（完整承载工具调用结构与供应商扩展字段） |
| `runs` | 一次运行 = 执行与审计单位（状态机 / 触发源 / 配置快照指针） |
| `llm_calls` | 模型调用审计（两阶段写） |
| `tool_invocations` | 工具执行审计（两阶段写，**成败只由 `status` 列表达**） |
| `config_snapshots` | 配置快照（**内容寻址**；`runs.config_hash` 指向它） |
| `scheduled_tasks` / `task_executions` | 定时任务登记与每次执行历史 |
| `notify_channels` | 通知渠道注册表（`url` 存 `${ENV_VAR}` 引用、发送时解析） |
| `memory_entries` | SQLite 记忆档载体（`scope` = 分层 / `section` = 分区，两回事） |
| `schema_version` | 迁移版本记录 |

迁移纪律见 schema.sql 头部：`IF NOT EXISTS` 幂等 + 受控前向迁移 + **已发布迁移不得改写**；**不得**依赖 ORM 自动 DDL。

<a id="product-risks"></a>

### 6.7 需要持续跟踪的产品风险

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| Go Agent 生态快速演进（框架层的 eino / adk-go 等） | 存在"直接用框架"的诱惑，偏离"自实现内核"的差异化 | 宪法级原则：不引 Agent 框架（技术方案 §1.1）；生态复用走协议对接——重代码工具档实现本项目 Tool 接口，带框架的组件经 MCP / HTTP 进程外接入；框架不得作为直接或间接依赖引入 |
| MCP SDK 行为差异 / 版本演进 | 工具接入出现兼容性问题 | 核心链路有真实子进程协议测试（自研最小 server）钉住；SDK 升级先跑该测试 |
| 调度边界行为（DST / 跨时区 / 长任务超时） | 错过或重复触发、停机后误补跑、通知重复 | 6 段 cron + 显式 zone + 每任务独占锁；固定时钟验证下一次触发、DST 和错过触发点；不补跑、不自动重放外部副作用；人工补跑单独留证 |
| 管理台前端引入 XSS / 供应链面 | 存储型 XSS 或新依赖风险 | 页面渲染模型产出文本走安全子集渲染器；前端依赖最小化并锁版本；CI 断言零 `innerHTML` |
| 单机 SQLite 的并发上限 | 高并发写锁竞争 | 核心阶段定位单机 / 小团队（WAL + 短事务）；扩展阶段换外部后端（接口预留） |

<a id="delivery-scope"></a>

## 7. 交付切片、依赖与范围裁决

<a id="release-scope"></a>

### 7.1 核心交付与扩展边界

- **核心阶段（本版交付范围）**：运行时内核（五能力）+ 支撑模块 + 五个每日 Demo + 管理台与 Agent 动态管理。目标是"一个人、一个周末、一台服务器，装出一个可演示、可试用、可审计的 Agent 底座"
- **扩展阶段**：企业级治理层（§7.4）。进入条件：核心阶段有真实生产部署、出现多人与合规诉求

**范围纪律**：核心阶段做不完的功能**挪到扩展阶段，不砍验收**；核心阶段的每一项在 §2.4、§6 都有可演示的验收口径。

<a id="delivery-slices"></a>

### 7.2 US 的依赖与可演示切片

拆解为 **14 个 US**，递进式依赖：每个 US 只消费它左边已交付的能力，不出现"先做桩、后面替换"式的返工。

```text
US-1 → US-2 → US-3 → US-4 ─→ US-5 ─→ US-9 → US-10 → US-11 → US-12 → US-13 → US-14
                            ├─→ US-6（Memory，只依赖 US-4）
                            ├─→ US-7（MCP，只依赖 US-4）
                            └─→ US-8（通知渠道，只依赖 US-4）
```

| US | 名称 | 交付一句话 |
| --- | --- | --- |
| US-1 | 工程地基 | 骨架 / 门禁 / 配置 / 日志 / 工作区 / 持久化全部就位 |
| US-2 | Provider 对接 | 一次 LLM 调用打通，显式映射表落地，调用落审计 |
| US-3 | Agent 目录与 Profile 加载 | `AGENT.md` 契约落地，加载期校验，`profile` 四个子命令 |
| US-4 | ReAct 循环与首个工具闭环 | `basalt chat` 多轮对话，能调工具把结果用于回答 |
| US-5 | Shell 与 HTTP 工具 | **Demo 一**：问天气给出穿衣建议 |
| US-6 | Memory | **Demo 二**：跨对话记偏好 |
| US-7 | MCP 集成 | **Demo 三**：零代码 PR digest |
| US-8 | 通知渠道 | 消息经渠道注册表推送（webhook / 飞书） |
| US-9 | Web Service | **Demo 四 / 五**：10 端点 curl 全链路 |
| US-10 | 定时任务 | 钟推到点触发（五个每日 Demo 的底座） |
| US-11 | 观测与管理台 | 浏览器里看清一个 Agent 全貌；改配置即生效 |
| US-12 | Agent 动态管理 | 页面三分钟上新一个 Agent |
| US-13 | 官网展示主页 | 站点与 README 快速开始就位，对外门面立起来 |
| US-14 | 发布收尾 | 五个每日 Demo 真跑 + `v0.1.0` 版本锚点 |

US-5 ~ US-8 只依赖 US-4、彼此独立，可并行或调序。每个 US 结束有可演示成果、跑一次一致性检查、按已有授权提交。节奏可调，**依赖序不可调**。

<a id="milestones"></a>

### 7.3 里程碑作为排期参考

按 5 周节奏（可调；US 已按粒度拆细，单周并行多个小 US），依赖序固定：

| 周次 | US | 核心任务 | 可演示成果 |
| --- | --- | --- | --- |
| 第一周 | US-1 + US-2 | 工程地基 + Provider 显式映射 | 干净检出 `make check` 全绿；一次真实 LLM 调用 + `llm_calls` 记录 |
| 第二周 | US-3 + US-4 | Agent 目录加载 + ReAct 循环与首闭环 | `basalt chat` 多轮对话，会话跨重启恢复 |
| 第三周 | US-5 + US-6 + US-7 | 天气 / 记忆 / MCP 三条独立枝 | Demo 一 / 二 / 三 |
| 第四周 | US-8 + US-9 + US-10 | 通知渠道 + Web Service + 定时任务 | Web 10 端点 curl 全链路；定时到点触发 |
| 第五周 | US-11 + US-12 + US-13 + US-14 | 管理台 + Agent 动态管理 + 官网展示主页 + 发布收尾 | 页面三分钟上新一个 Agent；官网与 README 可对外亮相；五个每日 Demo 自动跑 |

**范围纪律（重申）**：任一周完不成，把该 US 内**可独立演示的子集**先交付、其余挪扩展阶段，不砍已交付的验收口径。

<a id="extension-options"></a>

### 7.4 候选扩展能力

| 方向 | 内容 | 触发信号 |
| --- | --- | --- |
| **认证与接入** | API Key / JWT、登录、反向代理样例 | 出现非内网部署诉求 |
| **多租户与权限** | 租户隔离、RBAC、SSO | 多人多团队共用同一实例 |
| **完整审计** | 审计查询接口与页面、导出、留存策略 | 合规验收 |
| **Tool Policy** | 按 Agent / 按用户的工具策略、审批流 | 工具权限需要分级 |
| **IM Channel** | 企微 / 飞书 / 钉钉 / Slack 双向 | "在群里直接用" |
| **沙箱升级** | 容器沙箱 → microVM（`Policy` / `CommandRunner` 两接口各新增实现 + 装配切换） | 需要执行不可信代码 |
| **记忆进阶** | 向量检索、mem0 真实后端联调 | 记忆量大、语义召回诉求 |
| **环境适配** | K8s 部署清单、Helm chart、多副本 | 集群交付要求 |
| **可观测度量** | Prometheus 指标、分布式追踪 | 平台化运维要求 |

<a id="contribution-surface"></a>

### 7.5 贡献入口

- **协议开放**：MCP（工具）、OpenAI 兼容（LLM）、agentskills.io 形态（技能）——不发明私有标准
- **素材资产**：五个每日 Demo 的 Agent 目录、自研最小 MCP server（纯标准库、零依赖）作为可复制模板入仓库
- **贡献路径**：技能库与 MCP server 是最容易贡献的形态（不需要懂 Go）；核心代码贡献走 issue 讨论
- **文档即产品**：四份权威文档 + README 快速开始（干净检出 → 编译 → 起服务 → 跑通一次对话）

<a id="scope-decisions"></a>

### 7.6 已确定的范围与选型依据

| 编号 | 事项 | 裁决（含理由） | 影响面 |
| --- | --- | --- | --- |
| U-1 | 首版是否含管理台与 Agent 动态管理（US-11 / US-12） | **已定（2026-10-15）：含**——"不碰文件系统、页面上三分钟上新一个 Agent"是定位承诺，首版就要能演示；14 个 US 全量保留（US-11 / US-12 不砍批次） | 里程碑排期 |
| U-2 | cron 字段制式 | **已定：6 段（秒 分 时 日 月 周）+ zone 缺省 UTC**；5 段拒载。星期 0–6，`7` / `L` / `W` / `#` 不支持；日与周同时受限按 OR 语义，固定起点验证未来十次触发 | `AGENT.md` 契约、调度实现 |
| U-3 | YAML 解析库 | **已定（2026-10-15）：`goccy/go-yaml` v1.19.2**（活跃维护 + 报错可读，加载期校验需要）；备选 `go.yaml.in/yaml/v3`（v3.0.5，YAML 官方组织的维护分支；原生态默认 `gopkg.in/yaml.v3` 2025-04 已归档，**不建议**新项目采用） | 配置 / frontmatter 解析 |
| U-4 | LLM 客户端库 | **已定（2026-10-15）：`sashabaranov/go-openai` v1.43.0**（活跃；自定义 base URL 成熟）；备选 `openai/openai-go`（官方） | Provider 层实现 |
| U-5 | MCP SDK | **已定（2026-10-15）：官方 `modelcontextprotocol/go-sdk` v1.8.0**（与 Google 协作、协议对齐快）；备选 `mark3labs/mcp-go`（v1.2.1，社区体量大） | MCP Client 实现 |
| U-6 | HTTP 框架 | **已定（2026-10-15）：标准库 `net/http` + ServeMux 方法路由**（零依赖）；备选 `chi`（v5.4.0，如中间件需求变复杂） | Web 层实现 |
| U-7 | 调度库 | **已定：`robfig/cron/v3` v3.0.1**。实测结论（v3.0.1 源码）：**没有 `parser.WithLocation`**——六段制式写作 `cron.NewParser(cron.Second｜cron.Minute｜cron.Hour｜cron.Dom｜cron.Month｜cron.Dow)`；时区是**调度器级** `cron.WithLocation` 或**任务级** `TZ=` / `CRON_TZ=` 前缀（本项目取前缀，与 `AGENT.md` 的 `zone` 字段对接）；`L` / `W` / `#` 不支持、星期合法域 0–6（`7` 无效）。备选 `gocron/v2` | 定时器实现 |
| U-8 | mem0 第三档记忆后端 | **已定（2026-10-15）：核心阶段只交付 markdown + sqlite 两档**；mem0 接口预留、实现与真服务联调放扩展阶段 | 记忆范围 |
| U-9 | 管理台前端方案 | **已定：Vue + Vite 构建产物 `go:embed`**；Node 仅构建期依赖，运行期单二进制；产物入库，模型文本经安全子集渲染器，不引入路由库 / Markdown 库 / CDN 外链 | 前端工程与交付形态 |
| U-10 | OpenAPI 契约 | **已定：不引入代码生成**；冻结契约以**手工维护的静态** [contracts/openapi.yaml](contracts/openapi.yaml) 为准（不是运行时生成的产物），与技术方案的实现约束冲突时以技术方案为准 | 文档与集成方式 |

<a id="terms"></a>

## 8. 术语与对象边界

| 术语 | 定义 |
| --- | --- |
| **Agent（智能体）** | 一个业务智能体。定义本体 = 一个目录 `.basalt/agents/<name>/`（`AGENT.md` + 可选 `scripts/`、`REFERENCE.md`）。一个目录就是一个完整可用的 Agent |
| **AGENT.md** | Agent 的定义文件：frontmatter（运行配置）+ 正文（任务指令，注入系统提示词）。frontmatter 里没有 `notify_channels`——通知渠道是全局注册表管理的 |
| **Profile（配置）** | 底座内部的运行时配置对象，由 `AGENT.md` frontmatter 派生，决定 Provider、工具池、技能绑定、定时、限额 |
| **Provider（供应商）** | LLM API 服务。通过 OpenAI 兼容协议对接多家，维护显式 `name → 客户端` 映射 |
| **ReAct 循环** | Agent 的核心工作机制：Reason（想）+ Act（调工具）。工具结果回填后再进入下一轮，直到给出最终响应或达上限 |
| **Tool（工具）** | Agent 可调用的外部能力。**工具名必须在 `AGENT.md` 的 `tools:` 里显式声明，否则本轮工具池为空** |
| **Skill（技能）** | 公共实体存 `.basalt/skills/<name>/SKILL.md`；在 `AGENT.md` frontmatter 的 `skills:` 列表里**声明即绑定**。按需渐进披露：L1 元数据卡片 → L2 正文 → L3 附属资源。Skill 不是 Tool，不进工具注册表 |
| **Memory（记忆）** | 跨对话保留的状态。两层：会话记忆（SQLite 持久化、随会话走）与长期记忆（按 Agent 分层，Agent 层无内容回退全局层，两层不叠加） |
| **Bootstrap（引导文件）** | 注入系统提示词的上下文文件：`AGENTS.md`（项目级行为）、`SOUL.md`（人格）、`USER.md`（用户偏好，只读不写） |
| **Channel（渠道）** | Agent 对外的接入面。核心阶段内置 CLI；IM 渠道在扩展阶段 |
| **Session（会话）** | 一次对话的上下文容器，按 `channel+user+profile` 联合标识，持久化到 SQLite，重启不丢 |
| **Sandbox（沙箱）** | 工具执行的受控边界。核心阶段唯一实现为白名单沙箱（路径 / 命令 / 域名三档） |
| **Workspace（工作区）** | 实例工作目录 `.basalt/`：Agent 目录、技能库、记忆、配置、日志、数据库 |
| **MCP** | Model Context Protocol，工具接入的开放标准。Basalt 作 Client 连接外部 MCP server |
| **审计三表** | `runs`（一次运行，父）+ `llm_calls`（每次模型调用）+ `tool_invocations`（每次工具执行）；后两表各带 `run_id` 与轮次，失败带分类。**核心阶段就写入**（fail-closed），是"可审计"卖点的实现 |

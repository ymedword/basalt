# AGENT.md 与工具契约

> 本文档是 Basalt 运行时契约的**配置与工具**篇：`AGENT.md` frontmatter 的完整字段规范，以及全部内置工具的模型可见契约（名称 / 参数 JSON Schema / 返回与失败语义）。
>
> 与 [技术方案](../TechnicalSolution.md) §6（Tool 体系）、§11（工作区与 Agent 目录）冲突时以技术方案为准。本文档中的字段名、工具名、参数名、返回文本是固定字面量，**不得**在实现中改写。
>
> **文档状态**：待验证设计草案（冻结条件：五链路原型验证通过 + 双底座样例加载实测，见 [AiProgrammingGuide.md](../AiProgrammingGuide.md) §3.4）。
>
> **参数兼容口径**（与 NivroOS 基线 commit `50e1b30` 对照，见 [Compatibility.md](../Compatibility.md) §2.2）：两底座**共有**的工具，其**共有参数名与必填性逐字一致**（含 `save_memory` 的 `scope`、`http_post` 的 `body` 必填）；Basalt 新增的参数一律标注「**扩展**」，不改变共有参数的含义。NivroOS 的 Agent 迁移过来时，`tools:` 声明与调用语法不需改动。

---

## 1. AGENT.md 结构

`AGENT.md` = **frontmatter**（运行配置，派生成 `Profile`）+ **正文**（任务指令，注入系统提示词）。

```markdown
---
name: ops-agent
description: 运维助手
identity:
  agent_name: 运维小欧
  prompt: 你是一个专业的运维助手……
provider:
  name: deepseek
  model: deepseek-chat
  temperature: 0.7
tools:
  - read_file
  - shell
  - save_memory
skills:
  - digest-format
mcp_servers:
  - github-mcp
bootstrap:
  - AGENTS.md
  - SOUL.md
  - USER.md
settings:
  max_iterations: 10
  max_history_turns: 20
schedules:
  - id: daily-weather
    cron: "0 0 8 * * *"
    zone: Asia/Shanghai
    message: "查一下今天的天气并给出穿衣建议"
---

你是一个专业的运维助手。被触发时……（正文，注入系统提示词）
```

**一个目录 = 一个 Agent**：`.basalt/agents/<name>/` 内是 `AGENT.md` + 可选 `scripts/`、`REFERENCE.md`。目录名即 Agent 名，**必须**与 frontmatter 的 `name` 逐字一致（不一致整只不注册）。

---

## 2. frontmatter 字段规范

| 字段 | 类型 | 必填 | 缺省 | 说明 |
| --- | --- | --- | --- | --- |
| `name` | string | **是** | — | 必须等于目录名（改名 = 新建 + 归档旧的） |
| `description` | string | 否 | 空 | 一句话描述，管理台展示用 |
| `identity.agent_name` | string | 否 | `name` | 人格短名，注入提示词的自我称谓 |
| `identity.prompt` | string | 否 | 空 | 人格提示词，拼在系统提示词开头 |
| `provider.name` | string | **是** | — | 必须是 `basalt.yaml` 里已注册的 provider 名 |
| `provider.model` | string | **是** | — | 模型名；**空白即拒**（脚手架填占位符「请在此填写模型名」） |
| `provider.temperature` | float | 否 | 供应商缺省 | 0–2 |
| `tools` | []string | 否 | `[]` | **不声明 = 空池**：模型一行工具都看不到 |
| `skills` | []string | 否 | `[]` | 元素 = 公共技能库目录名；**声明即绑定**；库里没有该名字 → 整只不注册 |
| `mcp_servers` | []string | 否 | `[]` | 元素 = `mcp_servers.yaml` 里的 server 名 |
| `channels` | []object | 否 | `[{name: cli}]` | 接入渠道（核心阶段只有 `cli`） |
| `bootstrap` | []string | 否 | `[]` | 注入提示词的引导文件（相对工作区根） |
| `settings.max_iterations` | int | 否 | 10 | ReAct 最大轮次；达上限 = `limit_reached`（见 [run-and-errors.md](run-and-errors.md) §2） |
| `settings.max_history_turns` | int | 否 | 20 | 送进提示词的对话轮数上限 |
| `schedules` | []object | 否 | `[]` | 定时任务（见 §3） |

### 2.1 加载期校验（任一不过 = 该 Agent 整只不注册 + 点名 WARN）

| # | 规则 | 不通过时 |
| --- | --- | --- |
| 1 | `name` 等于目录名 | WARN `name mismatch` |
| 2 | `provider.name` 在已注册 provider 表里 | WARN `provider not registered: <name>` |
| 3 | `provider.model` 非空白 | WARN `blank model` |
| 4 | `tools` 里每个名字都能解析（内置 10 个 / MCP / 代码内注册） | WARN `tool not registered: <name>` |
| 5 | `skills` 里每个名字在 `.basalt/skills/<name>/SKILL.md` 有实体 | WARN `skill not found: <name>` |
| 6 | `mcp_servers` 里每个名字在 `mcp_servers.yaml` 里且连接成功 | WARN `mcp server skipped` |
| 7 | `bootstrap` 里每个文件存在 | WARN `bootstrap file missing` |
| 8 | `schedules[].cron` 为 6 段且合法、`zone` 合法、`id` 全局唯一（**≥2 项时每项 `id` 必须显式声明**，见 §3） | WARN 并跳过该 Agent |

**校验失败即整只不注册**（不是"跳过那一条继续"）——半可用的 Agent 比明确不可用更危险。

> 遗留形态备案：Agent 本地曾用 `skills/` 目录下的相对软连接表达技能绑定。该机制**已废弃**（Windows 下不可行），本地 `skills/` 目录被无视并记一条 WARN，绑定一律以 frontmatter 的 `skills:` 声明为准。

---

## 3. schedules 段（定时任务）

| 字段 | 类型 | 必填 | 缺省 | 说明 |
| --- | --- | --- | --- | --- |
| `id` | string | 否（单项可缺省） | `<agent>-1`（仅当该 Agent 恰有一个任务时派生） | **两项及以上每项必须显式声明**（否则列表重排会静默改变任务身份）；全局唯一，显式撞车 = 该 Agent 不注册；给派生任务补显式 `id` = 换身份（旧状态不跟随） |
| `cron` | string | **是** | — | **6 段**：秒 分 时 日 月 周（`0 0 8 * * *` = 每天 8 点）；5 段拒 |
| `zone` | string | 否 | `UTC` | IANA 时区名；**不得**回落服务器本地时区 |
| `message` | string | **是** | — | 到点发给该 Agent 的消息（等价于人推的一条用户消息） |

### 3.1 cron 公共子集（与 NivroOS 可互换的范围）

**6 段制式不代表语法等价**。两底座可互换的表达式限于**公共子集**：

| 语法 | 公共子集内 | 说明 |
| --- | --- | --- |
| `*` 通配 | 是 | |
| 数字 `5` | 是 | |
| 范围 `1-5` | 是 | |
| 列表 `1,3,5` | 是 | |
| 步长 `*/5`、`1-30/5` | 是 | |
| 月/周缩写 `JAN`、`MON` | 是 | 两侧均支持 |
| 星期 `7` | **否** | Spring 认 7 = Sunday；robfig/cron 合法域 0–6 → **用 0** |
| `L`（最后一天） | **否** | Spring 支持，robfig 不支持 |
| `W`（最近工作日） | **否** | 同上 |
| `#`（第 N 个星期几） | **否** | 同上 |
| **`dom` 与 `dow` 同时被限定** | **否** | Spring 取**交集**（AND，两者都满足才触发）；robfig 取**并集**（OR，任一满足即触发）——**硬性规则：至少一个是裸 `*`** |

**每任务时区**：Basalt 侧经 `CRON_TZ=<zone>` 前缀实现（`robfig/cron` v3.0.1 无 `parser.WithLocation`，时区是**调度器级** `cron.WithLocation` 或**任务级** `TZ=`/`CRON_TZ=` 前缀）。装载时把 `zone` 字段转成前缀，Agent 配置形态与 NivroOS 逐字相同。

**验收样例**（两个底座各从**同一固定起点**——2026-10-01T00:00:00Z——跑一遍，未来 10 次触发时间必须逐条相同；**不得**用"当前时间"作起点）：

```text
cron = "0 0 8 * * *"     zone = Asia/Shanghai   → 每天 08:00:00 +08:00
cron = "0 30 23 * * 0"   zone = UTC             → 每周日 23:30:00 UTC
cron = "0 0 */6 * * *"   zone = Asia/Shanghai   → 每 6 小时整点
```

**已知差异例（不得用于对拍、不得写进样例 `AGENT.md`）**：`0 15 9 1-7 * 1`（Asia/Shanghai）——`dom` 与 `dow` 同时受限，恰好落在两底座语义不同的地方：以固定起点 2026-10-01（北京）实测，Spring（AND）第一次触发为 **2026-10-05 09:15 +08:00**；robfig（OR）预期为 2026-10-01 09:15 +08:00（待原型期实跑确认）。详细读数与对拍程序见 [Compatibility.md](../Compatibility.md) §5.2。

DST 边界例（`America/New_York`，2027-03-14 跳 02:00–03:00）：`0 30 2 * * *` 在该日的行为**必须实测、不得推断**——两底座**各自测出什么就记什么**；若两侧不同，登记为已知差异并移出公共子集（同 Compatibility.md §5.2）。

---

## 4. 完整消息模型

会话历史**不是** `(role, content)` 二元组——工具调用轮次的完整结构必须原样保留并回传，否则供应商侧会直接报错。

### 4.1 消息结构

```go
type Message struct {
    Role              string          `json:"role"`                          // system / user / assistant / tool
    Content           string          `json:"content"`
    ToolCalls         []ToolCall      `json:"tool_calls,omitempty"`          // assistant 发起：每个含 ID / Name / Arguments
    ToolCallID        string          `json:"tool_call_id,omitempty"`        // tool 角色：回填哪次调用
    ReasoningContent  string          `json:"reasoning_content,omitempty"`   // 供应商扩展：思考过程
    ProviderExtra     map[string]any  `json:"provider_extra,omitempty"`      // 供应商扩展字段的透传位
}

type ToolCall struct {
    ID        string `json:"id"`         // 模型生成的调用标识，必须原样回传
    Name      string `json:"name"`
    Arguments string `json:"arguments"`  // JSON 字符串（原样保存，不做二次序列化）
}
```

**三条硬约束**：

1. **`tool_call_id` 必须原样回传**：一个 assistant 消息里可能并列多个工具调用，结果必须以对应 ID 回填，否则供应商无法匹配
2. **`reasoning_content` 必须回传**（DeepSeek 等思考模式模型）：带工具调用的轮次若丢掉该字段，供应商返回 **HTTP 400**；且历史被"洗干净"后**整个会话持续 400**——一旦发生，会话不可恢复
3. **`ProviderExtra` 原样透传**：不认识但供应商发来的字段不能在序列化往返中丢失

### 4.2 历史裁剪：按完整工具交互组

裁剪的**最小单位是"完整工具交互组"**（一个 assistant 消息 + 其全部 tool 结果消息），**不得**只丢一半：

```text
正确：丢掉 [assistant(tool_calls=[A,B])] + [tool(A)] + [tool(B)]
错误：只丢 [tool(A)]，留下 [assistant(tool_calls=[A,B])] + [tool(B)]
      → 供应商报"tool_call 无对应结果"，HTTP 400
```

裁剪顺序：从最早的组开始整组丢弃，直到满足 `max_history_turns` 与 token 预算。**核心记忆区永不裁剪**（见技术方案 §5）。

---

## 5. 内置工具契约（10 个）

工具名与参数名是**模型可见契约**：改字面量等于改调用方式。

**扩展标注约定**：表格「必填」列带 **扩展** 标记的行是 Basalt 相对 NivroOS 基线**新增**的参数——基线没有它，模型不传也能跑（有缺省值）；共有参数**不带**标记，其名称 / 类型 / 必填性与基线逐字一致。**不得**把扩展参数加进共有参数的含义里（例如不得让扩展的 `timeout_seconds` 改变 `arguments` 的传递方式）。

### 5.1 FileTools（3 个）

#### read_file

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 是 | 文件路径（绝对或相对工作区根） |

返回：文件内容（UTF-8 宽容解码：非法字节以替换字符呈现，**不**因编码拒绝读取）。
失败：`not_found`（文件不存在）/ `sandbox_denied`（路径越界）/ `runtime_error`（不可读）。
上限：单文件 256 KB，超出返回可读错误（提示用 `shell` 工具处理大文件）。

**读范围**：当前 Agent 自己的目录 + `.basalt/skills`（技能 L2 必需）。**跨 Agent 读取默认被拒**——读别的 Agent 目录需管理员在 `sandbox.cross_agent_read` 显式列出该 Agent 名（缺省空 = 一个都不许），技术方案 §6.5。

#### write_file

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 是 | 目标路径 |
| `content` | string | 是 | 写入内容（覆盖写） |

返回：写入字节数。
失败：`sandbox_denied`（越界、或目标是 `AGENT.md` / 技能库与 `scripts/` **只读区**）。
约束：写入范围 = **当前 Agent 自己的目录**（详见技术方案 §6.5），其中 `scripts/` 子目录**只读**（代码常量拒绝——脚本受控通道的前提是模型改不动脚本，见 §5.2）；父目录不存在时**不**自动创建。

#### list_dir

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 是 | 目录路径 |

返回：条目列表（每行：类型 / 大小 / 名字）；不递归、不跟随软连接。
失败：`not_found` / `sandbox_denied`。

### 5.2 ShellTools（1 个）

#### shell

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `executable` | string | 是 | **可执行文件名**，与 `shell.allowed_commands` **整串逐字**比对（`"git status"` 这类多词串必拒） |
| `arguments` | []string | 否 | 参数数组，逐项原样传递 |
| `timeout_seconds` | int | 否（**扩展**） | 超时（缺省 30 秒）；超时杀**进程树**。基线无此参数，缺省行为与基线一致 |

返回：退出码 + stdout + stderr（合并截断到 256 KB）。
失败：`sandbox_denied`（可执行名不在白名单、解释器形态不合法、或脚本路径不在允许区）/ `timeout` / `runtime_error`（启动失败）。

**argv 契约（不得违反）**：`executable` 与 `arguments` **分开传**，进程 argv 直执、**不经任何 shell 解释**。**不得**传 `bash -c "……"`，也**不得**把参数拼进可执行名——`git status; curl evil.com` 这类串接在 argv 契约下不可能执行。
工作目录：有 Agent 上下文且目录存在 → 该 Agent 自己的目录；无上下文 → 进程默认 CWD。
环境变量：只透传白名单变量（见技术方案 §6.5），**不**继承调用进程完整环境。

**解释器受控通道（`shell.interpreters`）**：解释器（`python3` / `node` 等）**可以**被放行，但形态受硬约束——argv **必须**是 `[<脚本路径>, <参数…>]`：`arguments[0]` 是脚本路径、**不得**以 `-` 开头（`-c` / `-m` / `-e` 这类内联代码形态一律拒），脚本的真实路径（软连接投影后）**必须**落在该 Agent 的 `scripts/` 或 `.basalt/skills/` 内。`scripts/` 对模型 `write_file` **只读**（代码常量拒绝）——脚本约束的前提是模型改不动脚本。`bash` / `sh` **不得**放行（等于把解释层请回来）。解释器进程拥有**宿主同等权限**，容器化隔离属扩展阶段（见 Roadmap.md F 方向）。

### 5.3 HttpTools（2 个）

#### http_get

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `url` | string | 是 | 目标 URL |
| `headers` | object | 否（**扩展**） | 附加请求头 |

返回：状态码 + 响应体（截断到 256 KB）。
失败：`sandbox_denied`（域名不在 `http.allowed_domains` / scheme 非 http/https / **重定向目标越界** / **跳数超过 5**）/ `timeout` / `external_error`（4xx/5xx）。

**重定向与目标形态**（两个 http 工具共有）：目标地址按**逐跳**校验——每一跳在**发送前**过域名白名单与 scheme 判定，HTTP 客户端**不得**自行跟随；最多 5 跳，超限或越界一律判 `sandbox_denied`（不是 `external_error`）。白名单条目与目标 host **必须是域名**：IP 字面量目标直接拒（白名单按域名匹配，放行 IP 等于绕过它）。

#### http_post

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `url` | string | 是 | 目标 URL |
| `body` | string | 是 | 请求体（**必填，与基线一致**） |
| `content_type` | string | 否（**扩展**） | 缺省 `application/json` |
| `headers` | object | 否（**扩展**） | 附加请求头 |

返回 / 失败：同 `http_get`。
注意：`http_post` 是**有外部副作用**的调用。承诺边界**逐字**见 [run-and-errors.md](run-and-errors.md) §3.3——运行时不自动重试、崩溃恢复不自动重放；但**客户端**（curl / 浏览器 / 调用方程序）在超时后重发同一请求，服务端**无法识别**是重试还是新意图，会照常执行（v0.1 不做幂等键）。

### 5.4 MemoryTools（2 个）

#### save_memory

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `content` | string | 是 | 要记住的内容 |
| `scope` | string | 否 | `CORE`（核心记忆）/ `ARCHIVAL`（归档记忆）；**省略 = `ARCHIVAL`**。与基线逐字一致 |

返回：写入确认（含作用域：Agent 层 / 全局层）。
失败：`invalid_input`（scope 非法，合法取值仅 `CORE` / `ARCHIVAL`）/ `runtime_error`；**超配额返回可读拒绝**（单条 ≤ 4 KB、每 Agent 核心区 ≤ 32 KB），提示改写归档区。
作用域：有 Agent 上下文 → 该 Agent 的 `memory/agents/<name>/MEMORY.md`；无上下文 → 全局 `memory/MEMORY.md`。

> **两个 `scope` 不是一回事（防混淆，必须看清）**：本工具的 `scope` 参数取值是 `CORE` / `ARCHIVAL`，指的是**记忆分区**（落 `memory_entries.section` 列，值 `core` / `archive`）；而 `memory_entries.scope` **列**指的是**记忆分层**（`agent` / `global`）。二者是两条正交维度：分区由模型选，分层由运行时按上下文决定（有 Agent 上下文 = Agent 层，无 = 全局层）。列名以 [schema.sql](schema.sql) 为唯一事实源。

#### recall_memory

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `query` | string | 是 | **单个关键词** |

返回：命中的归档区条目（核心区不参与检索——它每轮已完整注入）。
失败：无命中返回成功 + 空结果（不报错）。
**契约语义**：按整串 `contains` 匹配，**不分词、不扩展、不向量化**。命中条件是**连续子串**：条目原文里含该连续短语（条目写着 `SQLite 方案 评估`）**会命中**；几个词分散在条目各处、只是"都出现过"（该条分别含 `SQLite` 与 `评估`，但从不连排）**不会**自动组合命中——这是已知行为，不是缺陷。参数仍要求**单个关键词**。

> **2026-10-17 澄清**：本节此前写作"传多词短语**必不命中**"，与 `contains` 语义不符（连续短语在 `strings.Contains` / `instr` 下必然命中）。语义以本段为准：**整串子串匹配、不分词不扩词**；连续短语能命中、分散词不自动组合。同一句在 [Compatibility.md](../Compatibility.md) §2.2、[AiProgrammingGuide.md](../AiProgrammingGuide.md) §7 陷阱 11、[Roadmap.md](../Roadmap.md) B 方向与 M2 已同步改写；渲染测试**不得**钉"多词必不命中"，改钉"连续短语命中 / 分散词不命中"两态。

### 5.5 NotifyTools（1 个）

#### notify

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `channel` | string | 否 | 通知渠道名（`notify_channels` 表里的 `name`）。**缺省即失败**（不猜"默认渠道"——渠道由全局注册表管理，无默认概念），失败文案回填可用渠道名。必填性之所以标"否"：与基线一致，让模型不传时拿到可读失败而不是被供应商侧的 schema 校验拦下 |
| `content` | string | 是 | 推送内容（≤ 20 KB，超限**发送前**失败） |

返回：投递确认。
失败：`not_found`（渠道不存在）/ `sandbox_denied`（渠道域名不在白名单）/ `external_error`（投递失败）。
**投递判定**：按渠道类型分派适配器——`webhook` 看 HTTP 状态码；`feishu` **只认响应体 `code == 0`**（投递失败也回 HTTP 200，只看状态码会把"没送达"记成成功）。回执解析不出来同样判失败（"结果未知"**不得**冒充成功）。

### 5.6 ManageScheduleTools（1 个）

#### manage_schedule

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `action` | string | 是 | `set` / `remove` / `list` |
| `id` | string | 否 | 任务 id；`remove` 必填；`set` 省略时仅当该 Agent 恰有一个任务时可派生 `<agent>-1`（其余情况**必须**显式给 id，与 §3 规则一致） |
| `cron` | string | 否 | 6 段表达式；`set` 必填 |
| `zone` | string | 否 | 缺省 `UTC` |
| `message` | string | 否 | 到点发给自己的消息；`set` 必填 |

返回：`list` 返回全部任务；`set` / `remove` 返回确认（含 id）。
失败：`invalid_input`（action 非法、必填缺失、id 不属于自己）/ `not_found`。

**作用域硬边界**：只能改**当前 Agent 自己**的 `schedules:` 段（身份取自运行时上下文，**不**来自入参）；无 Agent 上下文一律拒绝。改动经与页面编辑器、脚手架创建**同一道**注册写入门（先验后写），不另立解析路径。

> 该工具是相对 NivroOS 基线的**补齐项**：NivroOS 已有此工具（第 10 个），Basalt v0.1 同步交付——否则声明它的 Agent 在 Basalt 上会因 `tool not registered` **整只不注册**，破坏 `AGENT.md` 可互换这条核心承诺。

---

## 6. 拒绝信息格式（模型可见契约）

沙箱拒绝的返回文本是**四段式**，逐字保真：

```text
<规则>：<越界值>。这是安全策略，请勿反复重试；<放行指引>
```

例：

```text
域名不在白名单：evil.example.com。这是安全策略，请勿反复重试；如需访问，请联系运维把该域名加入配置键 http.allowed_domains 并重启服务。
```

指引**必须**点名配置键（该档确实有键可加时）+ 说明需要重启——模型据此停止重试并转告人类。文件档的指引分两种：

- **跨 Agent 读取被拒** → 点名 `sandbox.cross_agent_read`（授权后需重启）
- **工作区之外的路径** → 可读根由工作区约定固定（当前 Agent 自己的目录 + `.basalt/skills`），**没有配置键可以加**；指引如实说明，**不得**点名不存在的配置键

这条文案与普通错误（面向运维）的读者不同，**不得**合并。

---

## 7. 跨文档指引

| 主题 | 文档 |
| --- | --- |
| Run 状态机 / 并发 / 取消 / 崩溃恢复 / 错误码 | [run-and-errors.md](run-and-errors.md) |
| 数据库结构（会话 / 消息 / 审计 / 快照） | [schema.sql](schema.sql) |
| REST 端点与信封 | [openapi.yaml](openapi.yaml) |
| 与 NivroOS 的逐项差异 | [Compatibility.md](../Compatibility.md) |
| 沙箱与授权边界（Policy / Runner） | [技术方案](../TechnicalSolution.md) §6.5 |

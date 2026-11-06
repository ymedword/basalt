# 记忆学习契约（US-22）

> 本文档是 US-22 受控自我进化的**固定字面量与语义**篇：抽取账本状态机、窗口与证据规则、确认处置、启动恢复与只读查询面。它是[技术方案](../TechnicalDesign.md) §1.1 原则 2 / 7 与[需求文档](../Requirements.md)记忆候选承诺的**契约细化**，两者冲突时以技术方案为准；颗粒度文档见 [US-22 模块卡](../us/us22-controlled-evolution.md) §2.4.5。
>
> 本文档**只**写本 US 新增或修订的固定事实。记忆六态、召回与注入守点、`save_memory` 语义仍以 [run-and-errors.md](run-and-errors.md) §9.3 与既有记忆契约为准；技能准入状态机、来源声明与提议入口仍以 [skill-admission.md](skill-admission.md) 为准。本文档**不**复制、不改写这两份契约。
>
> 契约中的表名、列名、状态字面量、失败分类、配置键、端点路径与 `detail_json` 键是**模型与外部系统可见的固定字面量**，实现必须逐字保真。实现事实源：`internal/storage/postgres/migrations/0011_memory_learning.sql`、`internal/core/learning.go`、`internal/memory/learning.go`、`internal/web/handlers_learning.go`。

---

## 1. 范围与不变式

本契约覆盖：Run 终态之后的**同步、有界**候选抽取，候选的人工确认晋升，方法草案的只读落盘与人工转交，以及启动恢复与只读查询面。

四条不变式（违反即缺陷，不是「建议」）：

1. **候选永不直接生效**：自动抽取只写 `memory_entries.state='candidate'`、`source='auto_extract'`；进入 `active` 的唯一路径是人工确认（§7）。候选不参与召回与注入。
2. **唯一触发点**：抽取只能由 Run 终态钩子同步触发；**不得**新增后台 Worker、定时扫描或第二套消息追加路径（宪法 VII）。
3. **钩子不能改变 Run**：`LearningHook.AfterRun` 没有返回值；钩子 panic、超时、失败都不改变 Run 的终态、返回或错误（§2.3）。
4. **只查证、不重放**：启动恢复对 `pending` 抽取行只做查证与落库，**不得**重读消息、重调模型或删除草案文件（§8）。

出厂默认**关闭**：`learning.auto_extract` 缺省 `false`。未启用时零写入、零模型调用、零审计行。

## 2. 启用与触发

### 2.1 配置键（`learning` 段）

| 键 | 类型 | 语义 |
| --- | --- | --- |
| `learning.auto_extract` | bool | 唯一启用通道；缺省 `false` |
| `learning.agents` | string[] | 抽取名单；启用时必须显式列出至少一个 Agent |

装载期校验（任一不满足即启动报错，**不得**降级为 WARN）：

- `auto_extract: true` 但 `agents` 为空 / 缺省 → 报错（空名单**不等于**「全部 Agent」）
- 名单条目为空串、重复，或不符合 `^[a-z0-9][a-z0-9_-]*$` → 报错
- 名单内 Agent 在工作区不存在 → 记 WARN、不抽取、**不阻止启动**；状态页 `unknown_agents` 列出

### 2.2 触发条件（全部满足才抽取）

| 条件 | 不满足时 |
| --- | --- |
| `FinishRun` 返回 `applied=true`（本次真正完成 `running` → 终态转换） | 跳过：写失败或幂等幂等 0 行分支 |
| Run 终态为 `completed` | 跳过：`failed` / `cancelled` / `limit_reached` 不抽取 |
| `RunRequest.Channel` 为 `web` 或 `cli` | 跳过：`scheduler` / `admin` 没有「用户陈述」 |
| `RunRequest.Flow == nil` 且 `RunRequest.FixedTool == nil` | 跳过：Flow 与固定工具有各自的受控执行路径 |
| Agent 在启用的 `learning.agents` 名单内 | 跳过（未列名只记 Debug 日志，不读库、不调用） |
| 本次 Run 准入时冻结的客户端实现了 `ChatMemoryExtract` | 记 ERROR 并放弃；**不得**换用装配期全局客户端（原则 III） |

### 2.3 收尾顺序（正常路径）

```text
FinishRun（终态落库，5s 独立预算，WithoutCancel）
  → 学习抽取钩子（同步；总截止 ≤20s；无返回值）
  → ReleaseRunRefs（无条件 defer；失败只记 ERROR，由启动 releaseTerminalRefs 补齐）
```

- 终态落库失败**不覆盖**已得结果：返回照常，行留 `running` 待恢复扫描。
- 释放**无条件**执行：钩子 panic、超时、跳过都不影响；顺序由 defer 注册点保证。
- 钩子端到端预算：工作段 13s + 审计回填 5s + 失败回填 2s，总截止 20s；**不得**起 goroutine 逃逸。
- 本顺序是 [run-and-errors.md](run-and-errors.md) §2 的修订面：**正常收尾**拆为终态 → 抽取 → 释放；**崩溃恢复**关闭 `running` 行仍保持状态与释放同事务（§2.4.6 的替换关系）。

## 3. 抽取账本状态机

### 3.1 状态（`memory_extractions.status`）

| 状态 | 含义 | 进入方式 |
| --- | --- | --- |
| `pending` | 已预写、调用结果未知 | `BeginExtraction` 预写；提交 / 失败回填未完成时停留 |
| `success` | 本次抽取已提交 | `CommitExtraction`（候选 / 草案行与状态同事务） |
| `failed` | 本次尝试失败，可重试 | `FailExtraction`；`attempts < 3` 时同来源下一 Run 重试冻结窗口；重试前重读窗口发现缺失 → `skipped` / `window_gone`（不递增 `attempts`） |
| `skipped` | 放弃，不重试 | `window_gone`：冻结窗口消息被保留策略清理（实际条数 < 冻结条数） |
| `unknown` | 结果不可知 | 启动恢复查证无候选元数据也无草案行（§8）；保留历史，`verified_at` 非空后不再阻断 |

状态**只前进一次**：`pending` → 任一终态；两条受控例外——重试路径把 `failed` 改回 `pending`（`BeginExtraction`，`attempts` +1），重试前发现冻结窗口缺失则把 `failed` 直接终结为 `skipped`（`FailExtraction`，C-33，**不递增** `attempts`）。除此之外，非 `pending` 行上的恢复写入是幂等空操作（**不得**把 `unknown` 补成 `success`，也**不得**用 `failed` 覆盖可能已提交的 `success`）。

### 3.2 失败分类（`memory_extractions.failure_class`）

| 字面量 | 含义 | 终态 |
| --- | --- | --- |
| `model_error` | 模型调用失败（非审计、非截止、非装配） | `failed` |
| `invalid_output` | 严格解析或整次校验失败 | `failed` |
| `timeout` | 工作段截止耗尽 | `failed` |
| `audit_unavailable` | 两级预写审计失败 | `failed` |
| `extractor_unavailable` | 装配缺陷 / 抽取器不可用 | `failed` |
| `window_gone` | 冻结窗口不完整 | `skipped` |

`failure_class` 为 NULL 仅出现在 `pending` / `success` / `unknown` 行。`error_message` ≤1024 字节；命中凭证扫描时整体抑制为 `<class>: details suppressed`，**不得**落正文、证据引文或凭证。

### 3.3 尝试与重试

- 每次尝试只调用模型**一次**：不重试、不 fallback、不换模型。
- 重试条件：同来源（Agent + 会话 + 来源调用方 + 来源 Key）存在 `failed` 且 `attempts < 3` 的行 → 下一 Run 重试**同一冻结窗口**，身份随本次 Run 更新（`last_attempt_run_id` / `last_attempt_at` / provider / model / config_hash）。
- 第 3 次仍失败 → 不建新行、不调用；该来源的后续窗口因游标未推进继续报同一失败直至放弃。
- `pending` 行与未查证 `unknown`（`verified_at` 为空）**阻断**同来源新窗口（不重试、不新开）。

### 3.4 预算

每 Agent 每滚动 24 小时最多 **200** 次抽取尝试（代码常量，不是配置键）。预算判定与 `pending` 行写入 / `attempts` 递增在**同一事务**内并取 Agent 锁；耗尽时**不建行、不调用**（`ErrLearningBudgetExhausted`，记 WARN 并跳过）。状态页 `attempts_used` / `attempt_limit` / `exhausted` / `budget_window_seconds` 如实投影。

## 4. 窗口与证据

### 4.1 窗口规则

| 规则 | 固定值 / 语义 |
| --- | --- |
| 窗口消息 | 同来源 Run 的消息，角色仅 `user` / `assistant` |
| 窗口上限 | 最多 **40** 条 / **8192** 字节，保留最近；超出部分丢弃并记 `dropped_window_messages` |
| 前文 | 最多 **6** 条 / **2048** 字节；只进提示词，**永不**作为证据 |
| 冻结 | 首次尝试成功后窗口按 `window_from_message_id` / `window_to_message_id` 冻结；重试只重读该区间 |
| 窗口缺失 | 冻结区间实到条数 < `window_messages` → `skipped` / `window_gone` 并推进游标，不用残缺集合抽取 |
| 游标 | `memory_extraction_cursors` 按 `(agent_name, session_id, origin_caller_id, origin_key_id)`；终态（成功 / 跳过 / 查证）推进到冻结窗口末端 |
| 既有条目快照 | 最多 **20** 条 / **8192** 字节；来源子集与建议取代目标只能落在快照内 |

来源身份（`origin_caller_id` / `origin_key_id`）由存储层从触发 Run 的 `runs` 行**复制**；服务层不接触 `Principal`，重试不重贴当前请求身份。

### 4.2 证据规则（逐项程序校验）

| 规则 | 判定 |
| --- | --- |
| 条数 | 每项 1–**3** 条证据 |
| 在窗口内 | `message_id` 必须属于冻结窗口的**本次**消息集；前文与窗口外消息一律丢弃该条 |
| 角色 | 消息角色必须是 `user` / `assistant`，且与模型自报 `role` 一致 |
| 逐字 | `quote` 非空且是消息正文的逐字子串（`strings.Contains`） |
| 凭证 | 正文或引文命中凭证扫描 → 丢弃该条 |
| 用户证据 | `fact` / `preference` / `constraint` / `temporary` 至少一条 `role=user` 证据，否则**降级**为 `inference`（不丢弃） |
| `evidence_at` | 证据消息的最大 `created_at`，是**记录时刻不是事实发生时刻**；不可求证（< 1ms）即丢弃该条 |
| `certainty` | 程序派生：`asserted`（未降级的四类）/ `uncertain`（`inference` / `method`） |
| `confidence` | 模型自报，夹到 `[0,1]`，**只作展示**，不作状态、召回或确认门槛 |
| `valid_to` | 仅 `temporary` 必需：RFC3339 绝对时刻且晚于当前；相对日期不解析、不猜 |
| `time_confidence` | `exact`（temporary 的绝对到期）/ `unknown`（其余）；`relative_unknown` 保留在闭集中、当前不产生 |

### 4.3 模型输出形状（不可信数据）

顶层唯一形状 `{"items":[...]}`，条目字段固定为 `kind` / `content` / `name` / `confidence` / `evidence` / `source_entry_ids` / `suggested_supersedes_id` / `valid_to`。**没有** `state` / `source` / `entry_id` 之类的字段——模型无法指定落库状态、来源或目标主键。

整次解析失败（`invalid_output`）的判据（任一命中）：非 UTF-8、超过 **65536** 字节、顶层不是 JSON 对象、未知键、尾随内容、`items` 超过 **5** 条。解析错误一律固定文案哨兵，**不得**把模型输出片段拼进错误串。

逐项校验（丢弃只影响该条，不失败整次）：

| 规则 | 处置 |
| --- | --- |
| `kind` 不在闭集 | 丢弃 |
| `content` 空或 > **4096** 字节 | 丢弃 |
| 与快照内归档区 `candidate` / `active` 正文同文，或同批重复 | 丢弃 |
| `source_entry_ids` 超过 **5** 个或含快照外 ID | 丢弃该条 |
| `suggested_supersedes_id` 在快照外 | 清空建议，条目保留 |
| `method` 的 `name` 非法（空 / 超限 / 含路径分隔） | 丢弃 |

`kind` 闭集：`fact` / `preference` / `constraint` / `inference` / `temporary` / `method`。其中 `method` **只**写方法草案（§10），不落记忆候选；`memory_candidate_meta.kind` 的 DB 闭集因此只有前五个。

## 5. 两级预写审计

`internal/memory/learning.go` 的入口顺序固定：

1. `BeginExtraction`：在**单事务**内重判未决阻断、尝试上限与预算，写 `memory_extractions` 的 `pending` 行（含冻结窗口与来源复制），并递增 `attempts`。预写失败 → **不得**发出调用。
2. `ChatMemoryExtract`：模型调用唯一入口，内部先写 `llm_calls` 的 `pending` 行（`purpose='memory_extract'`、`round=0`），再发调用。预写失败 → **不得**发出调用。
3. 调用返回后更新 `llm_calls` 终态，再由 `CommitExtraction` / `FailExtraction` 回填抽取行终态。

`llm_calls.purpose` 闭集（迁移 `0011` 扩展）：`chat` / `rerank` / `summary` / `embedding` / `compaction` / `memory_extract`。`memory_entries.source` 闭集（迁移 `0011` 扩展）：`save_memory` / `explicit_propose` / `legacy_import` / `migration` / `auto_extract`。

**没有新增动作字面量**：抽取不是管理操作，不写 `management_operations`；它由 `memory_extractions` 与 `llm_calls` 两类账本承载。

## 6. 确认处置（`memory.confirm` 审计明细）

自动候选的确认是 `memory.confirm` 的既有动作，**不新增**动作名。`detail_json` 在既有键集上**追加**两个可选键（旧路径不写，旧操作行与旧客户端照常可读）：

| 键 | 类型 | 语义 |
| --- | --- | --- |
| `resolution` | string | `keep_both` / `supersede`；空 body 归一化为 `keep_both` 后**照写** |
| `supersedes_id` | int64 | supersede 成功后被退役的旧条目主键；未替代时省略 |

既有键保持不变：`entry_id` / `replaced_id` / `section` / `agent` / `state_before` / `state_after` / `content_sha256`。正文、证据引文**不得**进入 `detail_json`。

## 7. 确认是唯一晋升路径

### 7.1 请求形状

`POST /api/v1/memory/entries/{id}/confirm`，请求体可选：

```json
{"resolution": "keep_both", "supersedes_entry_id": 5}
```

- 整个 body 可省略；空 body 等价 `{"resolution":"keep_both"}`，但**不**跳过 §7.2 的任何门禁。
- `resolution` 闭集：`keep_both` / `supersede`。空串归一化为 `keep_both`。`keep_both` 携带 `supersedes_entry_id` → 409（两种处置被拼在一起）。
- `supersede` 必须带正整数 `supersedes_entry_id`，且必须与候选元数据的 `suggested_supersedes_id` **逐字相等**。

### 7.2 门禁（任一不满足 → **409 `CONFLICT`** 且状态不变）

| 门禁 | 判据 |
| --- | --- |
| 候选状态 CAS | 目标行必须仍为 `candidate`（事务内用 CAS 再判一次） |
| 候选到期 | `valid_to` 为空或晚于当前 |
| 候选元数据 | 自动候选必须有 `memory_candidate_meta` 行；缺失即 fail-closed，**不得**按 `keep_both` 放行 |
| 来源闭包 | 沿 `memory_candidate_sources` 递归取候选的来源闭包（深度上限 8，超限按失效处理，fail-closed），与元数据声明的直接来源取并集；闭包内全部条目存在、`state='active'` 且未过期 |
| 来源 Key | `origin_key_id` 未撤销、未过期、属于 `origin_caller_id`；空串表示本地管理员（无 Key 可查） |
| 来源调用方 | `origin_caller_id` 存在且 `status='active'` |
| 替代目标 | supersede 时：存在、`state='active'`、未过期，且与建议逐字相等 |

并发确认只有一个赢家：CAS 失败的一方 409，候选与目标不变。

### 7.3 事务与审计

1. 纯读复核（预写**之前**）：候选存在、自动候选的元数据、候选到期、替代目标与建议一致且仍 active、直接来源仍 active。任一失败 → 409 且**不留操作行**。直接来源只是快速路径，**闭包复核只在事务内**（来源闭包、到期与撤权必须在加锁后判定）。
2. 预写 `memory.confirm` `pending`（`detail_json` 见 §6）。
3. 单事务确认：存储侧**再执行一遍**全部门禁（来源到期、Key 撤销 / 到期、调用方停用、并发竞争只有事务内可见）→ 候选转 `active`（写 `supersedes_id` / `confirmed_by`）→ supersede 时目标转 `superseded`。
4. 成功 → 回填审计 `success`；失败 → 回填 `failed` 并返回 409，**不得**留下 `pending`。
5. best-effort 补索引：失败只记 WARN，确认保持 `active`，缺口由启动回填补齐；**不得**把补索引失败写成确认失败。

### 7.4 回执

响应体为既有 `MemoryEntryReceipt`：`entry_id`（确认的目标行）、`state='active'`、`superseded_id`（本次被退役的旧条目；0 = 未替代）、`replaced_id=0`、`merged=false`。

## 8. 启动恢复（只查证、不重放）

启动时**持工作区实例锁**、在开放服务**之前**扫描 `memory_extractions.status='pending'` 的行，逐条查证：

| 查证对象 | 结论 |
| --- | --- |
| `memory_candidate_meta` 存在该 `extraction_id` 的行，或 `memory_method_drafts` 存在该 `extraction_id` 的行 | `success` |
| 两者都不存在（可证明提交事务未写出任何结果） | `unknown` |
| 读库 / 计数失败 | 中止启动（fail-closed），**不得**带着结果未知的账本开始服务 |

两种结论都在**同一事务**内写 `status` + `verified_at`，并把来源游标推进到冻结窗口末端。

- 未查证的 `unknown`（`verified_at IS NULL`）**阻断**同来源新窗口；查证之后保留历史但**不再阻断**。
- 恢复**不得**重读消息、重调模型、删除草案目录、补插候选行或把 `pending` 补成 `success`。
- 没有 `pending` 行时是零写入空操作。

## 9. 只读查询面

### 9.1 HTTP（五个 GET，无写端点）

| 路径 | 语义 |
| --- | --- |
| `GET /api/v1/learning/status` | 状态与预算读模型（`LearningStatusView`，13 字段；与 CLI `learning status --json` 逐项同源） |
| `GET /api/v1/learning/extractions?agent=&state=&limit=` | 抽取账本列表，`created_at` 倒序，不展开候选与草案 |
| `GET /api/v1/learning/extractions/{id}` | 抽取详情：账本 + 候选复核投影 + 草案元数据 |
| `GET /api/v1/learning/drafts?agent=&limit=` | 草案元数据列表（不含正文） |
| `GET /api/v1/learning/drafts/{id}` | 草案详情：元数据 + 正文 + 证据 |

- `state` 只接受五字面量：`pending` / `success` / `failed` / `skipped` / `unknown`；非法 → 400，不触达存储。
- `limit` 缺省 50，必须 1–200；越界 → 400。
- 存储失败 → 500，**不得**用空列表或全零状态冒充正常读数。端口未装配 → 500 `INTERNAL`（**不得**伪装成「未启用」）。

### 9.2 CLI（只读；`handoff` 另计）

| 命令 | 语义 |
| --- | --- |
| `learning status [--json]` | 状态与预算 |
| `learning extractions list [--agent] [--state] [--limit] [--json]` | 账本列表 |
| `learning extractions show <extraction-id> [--json]` | 账本详情 |
| `learning drafts list [--agent] [--limit] [--json]` | 草案元数据列表 |
| `learning drafts show <draft-id> [--json]` | 草案正文的唯一 CLI 出口 |
| `learning handoff <draft-id> --name <skill> --from-dir <dir> --source-file <json>` | 薄转交（§10） |

**管理台**并入既有「记忆」页三个页签（记忆库 / 待确认 / 学习记录），无新入口、无新写端点；确认与删除复用既有 `POST /memory/entries/{id}/confirm` 与 `DELETE /memory/entries/{id}`（按**条目主键**，不按分层路径）。

## 10. 方法草案与 handoff

### 10.1 草案落盘（只读、固定布局）

```text
<workspace>/.basalt/learning/drafts/<draft_id>/
├── DRAFT.md      # frontmatter + 模型正文；权限 0444
└── evidence.json # 证据数组；权限 0444
```

- `draft_id` = `dft_` + 26 位小写 base32（128 位随机）；目录与文件名由程序固定，模型只能提供受校验的 `name` 与正文。
- 草案**不**写记忆条目、**不**执行脚本、**不**进任何 Agent 读写白名单（US-20 隔离），也不进隔离执行挂载。
- 写盘失败：清理该目录（不留下「有目录无数据库行」的孤儿）；其余条目照常提交，该条计入 dropped。

### 10.2 handoff 纪律

`learning handoff` 是**薄适配**，只调用既有 `AdmissionAdmin.Propose`：

1. 主体必须是可信本机 CLI（`cli:<OS user>`，非 unknown）——复用准入的同一判据。
2. 来源声明严格解码，`confirmation` 必须是 `user_confirmed`。
3. 程序强制 `kind=model_proposal` 与 `ref=learning/draft/<draft-id>`；管理员文件里的这两个字段被覆盖，不参与判定。
4. 取**实例锁**后才读草案、调用 `Propose`；草案不存在即拒绝（`ErrLearningDraftNotFound`），不凭手写 ID 造来源引用。
5. **不**直接评估、批准、发布或撤回，也**不**写自己的审计——这些全部仍归 US-21 准入（`skill-admission.md`）。

## 11. 召回面派生复核

召回（关键词真实入口、无向量关键词回退、向量路径）**三个 SQL 入口共用同一段判据**：

```text
WITH RECURSIVE derived_closure(entry_id, source_id, depth) AS (
    种子: 本次召回分层内、memory_candidate_sources 中作为派生条目的 entry_id
    递归: 沿 memory_candidate_sources 向「来源的来源」展开，depth < 8
)
行级判据: NOT EXISTS（闭包内任一来源缺失 / 非 active / 已过期，或在 depth >= 8 时仍有更深的来源）
```

- 判据写在 `WHERE` 里，先于 `ORDER BY` 与 top-k。
- 深度上限 **8**，超深按失效处理（fail-closed），**不得**截断后照常返回。
- 闭包为空的显式条目判据恒真——**显式条目不受影响**。
- 只做**排除**，不扩张召回面；候选仍不可召回，注入守点不放宽。

## 12. 迁移与版本

- 新增迁移 `internal/storage/postgres/migrations/0011_memory_learning.sql`，只追加：两条 CHECK 闭集扩展（`memory_entries.source`、`llm_calls.purpose`）+ 五张新表（`memory_extractions`、`memory_extraction_cursors`、`memory_candidate_meta`、`memory_candidate_sources`、`memory_method_drafts`）。不改既有列与既有行。
- `SupportedSchemaVersion`：10 → **11**（`internal/storage/postgres/connect.go`）。打开库前校验版本；高于二进制支持版本 → 拒绝并提示升级二进制。
- `docs/contracts/schema.pg.sql` 是 v1 基线，**冻结不改**；v11 只由迁移 `0011` 表达。SQLite 谱系（`schema_version=6`）不变。
- 本 US 零新增 Go / npm 依赖坐标。

## 13. 不变式速查

| # | 约束（不得 / 必须） | 违反时的症状 | 复核方式 |
| --- | --- | --- | --- |
| 1 | 自动抽取**不得**产生 `active` 条目 | 未经确认的候选进入召回 | 抽取后 `memory_entries` 只多出 `candidate` / `auto_extract` 行（PG 测试） |
| 2 | 候选**不得**参与召回与注入 | 模型看到未确认内容 | 候选未确认前召回结果为空（`TestUS22_*Recall*`） |
| 3 | 未启用时**不得**写行、**不得**调用模型 | 默认部署出现抽取账本或 `llm_calls` | 默认关闭零写入零调用用例 |
| 4 | 两级预写任一失败**不得**发出调用 | 无审计的模型调用 | 预写失败用例断言零调用 |
| 5 | 空 body 确认**不得**跳过门禁 | 空 body 直接晋升自动候选 | 空 body 用例仍走全门禁并 409 |
| 6 | 恢复**不得**重放、**不得**补写结果 | 重启后出现第二次模型调用或新候选 | 恢复用例断言零调用、只写查证结论 |
| 7 | 草案**不得**进入任一 Agent 读写白名单 | 抽取产物被 Agent 当输入 | 文件访问用例与白名单断言 |
| 8 | `schema.pg.sql` **不得**被本 US 改写 | 冻结基线漂移 | `git diff docs/contracts/schema.pg.sql` 为空 |

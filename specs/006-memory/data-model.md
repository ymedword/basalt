# Phase 1 Data Model: US-6 Memory 长期记忆

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-17

**总览**：本 feature **零迁移、零列变化**（判据：`git diff docs/contracts/schema.sql` 为空）——sqlite 档用的是 US-1 已建齐的 `memory_entries` 六列 + `idx_memory_scope`，本 US 只是它的**首个写入方**；数据面的其余变化全在**文件实体**（markdown 档的 `MEMORY.md` 形态）与**内存实体**（`core` 记忆类型面 + 门面 + 两件工具的输入结构）。下表的既有实体形态**已逐字对源核实**（`internal/core/{run.go,tool.go,errors.go}`，2026-10-17 本轮读取）。

## 1. 既有实体（只读引用，本 feature 的改动面以标注为准）

### 1.1 `memory_entries` 表 + `idx_memory_scope`（[docs/contracts/schema.sql](../../docs/contracts/schema.sql)，**零改动**）

| 列 | 类型 | 语义与约束 |
| --- | --- | --- |
| `id` | `INTEGER PRIMARY KEY AUTOINCREMENT` | 写入顺序的事实源（召回按 `id` **升序**） |
| `scope` | `TEXT NOT NULL` | **分层**：`agent` / `global`——与工具参数 `scope`（**分区**）**不是一回事**，两处语义**不得**混淆 |
| `agent_name` | `TEXT` | `scope='agent'` → Agent 名；`scope='global'` → **NULL**（查询恒用 `IS NULL`，**不得**用空串顶替） |
| `section` | `TEXT NOT NULL` | **分区**：`core` / `archive`（工具参数 `CORE` / `ARCHIVAL` 落此列） |
| `content` | `TEXT NOT NULL` | 条目正文的**规范表示**（`TEXT NOT NULL`；**非空**——规范化后为空在工具层即拒）；写入前 MUST 经 `strings.TrimSpace` 规范化（与 markdown 档同一个标准库函数）；配额按 `SUM(octet_length(content))` 求和（作用于规范化文本） |
| `created_at` | `INTEGER NOT NULL` | 载体时钟（构造器显式传入 `now func() time.Time`，**不**在 SQL 里取当前时间） |

索引 `idx_memory_scope(scope, agent_name, section, id)` 覆盖三类查询：分层核心区求和（配额）· 分层全量读（`Load`）· 分层归档区检索（`instr`）。查询形状固定：Agent 层 `WHERE scope='agent' AND agent_name=?`；全局层 `WHERE scope='global' AND agent_name IS NULL`；归档检索 `WHERE … AND section='archive' AND instr(content, ?) > 0 ORDER BY id`。

### 1.2 `core.ToolEnv`（[internal/core/run.go:112-119](../../internal/core/run.go#L112-L119)，**零改动**）

`{RunID, SessionID, AgentName, AgentDir, Timeout, MaxOutputBytes}`——两件记忆工具的消费面只有 `AgentName`：**空串 = 无 Agent 上下文 → 全局层**（注释逐字：「**不是**异常」）。工具**不得**从 `AgentDir` 反推 Agent 名（路径反推 = 第二处事实源）。

### 1.3 `core.Tool` 与 `core.Result`（[internal/core/tool.go:29-42](../../internal/core/tool.go#L29-L42)，**零改动**）

- `Tool`：`Name() / Description() / InputSchema() json.RawMessage / Execute(ctx, env, input) (Result, error)`——两件记忆工具实现它，签名不变；
- `Result` **只有两个字段** `{Content string, Retryable bool}`——确认文案与召回结果都走 `Content`；**不得**自造第三字段。

### 1.4 `core.InvalidInputError`（[internal/core/errors.go:158-165](../../internal/core/errors.go#L158-L165)，**既有承载，本 US 复用**）

`{Tool string, Reason string}` + `IsInvalidInput(err) bool`——两件记忆工具的自产错误**只有两类**：`*core.InvalidInputError`（空输入 / 取值域外 / 配额拒绝映射）与载体 I/O（→ `runtime_error`）；失败分类沿用既有 `ToolExecutor` 判序（工具本体不写审计、不记日志）。

### 1.5 `core.RunEnv`（[internal/core/run.go:85-94](../../internal/core/run.go#L85-L94)，**本 US 增第三非导出注入位**）

| 字段 | 类型 | 本 US |
| --- | --- | --- |
| `Req` / `Snapshot` / `Session` / `Budget` / `ToolSet` | 导出五件 | 不变 |
| `llmClient` | `ProviderClient`（非导出） | 不变（既有注入位先例 ①） |
| `agentDir` | `string`（非导出） | 不变（既有注入位先例 ②） |
| `memory` | `string`（**非导出**） | **新增**：本次运行生效的核心区文本（运行开始时读一次、运行内冻结）——非导出 = 不是契约字段、不进任何出站形态 |

### 1.6 `core.ServiceOptions`（[internal/core/service.go](../../internal/core/service.go)，**本 US 增 1 字段**）

`{Agents, Client, Messages, Runs, Snapshots, Audit, Loop, Logger, Now, MaxConcurrent}` → 增 `Memory MemoryReader`（**可空**：nil = 段 2 恒空、行为与 US-4 逐字节相同——既有 `core` 用例因此零改动）。

### 1.7 `core.ConfigSnapshot`（[internal/core/snapshot.go](../../internal/core/snapshot.go)，**零改动**）

`{Manifest []ManifestEntry, SectionOne string, Hash string}`——**不得**新增记忆字段：快照是**配置级**事实（落库 + 参与哈希），记忆是**运行级**输入（每运行可变、不参与快照哈希）。

## 2. 新增实体：`core` 记忆类型面（[internal/core/memory.go](../../internal/core/memory.go)，纯类型 + 常量，零 import 内部包）

### 2.1 `Scope` 与 `GlobalScope`

| 实体 | 形态 | 约束 |
| --- | --- | --- |
| `Scope` | `type Scope struct { Agent string }` | `Agent != ""` → Agent 层；`Agent == ""` 与 `GlobalScope` **同值**（`Scope{}`） |
| `GlobalScope` | `var GlobalScope = Scope{}` | 全局层的**一等值**——**不得**用 `nil` 或空串顶替；「无 Agent 上下文 = 全局层」是正常取值 |

### 2.2 `Section` 与两常量

`type Section string`；`SectionCore Section = "core"` / `SectionArchive Section = "archive"`——这是**分区**维度（工具参数 `CORE` / `ARCHIVAL` 的落库形态），与 `memory_entries.scope` 列（**分层**）不可混淆。

### 2.3 配额常量（**代码常量，不是配置键**）

| 常量 | 值 | 语义 |
| --- | --- | --- |
| `MaxCoreEntryBytes` | `4096` | **单条**上限——**只限 CORE 写**（归档区单条无配额） |
| `MaxCoreSectionBytes` | `32768` | **每层核心区总量**上限（Agent 层、全局层**各算各的**） |

### 2.4 两个接口（`core` 声明、实现放各自包——依赖倒置）

| 接口 | 方法 | 实现方 |
| --- | --- | --- |
| `LongTermMemoryStore` | `Append(ctx context.Context, scope Scope, section Section, content string) error` / `Load(ctx context.Context, scope Scope) (string, error)` / `RecallByKeyword(ctx context.Context, scope Scope, query string) ([]string, error)` | `memory.Service`（门面）· `memory.MarkdownStore` · `storage.MemoryStore` |
| `MemoryReader` | `LoadEffective(ctx context.Context, agent string) (string, error)` | `memory.Service`（门面——回退语义的唯一判定点） |

**方法名与参数逐字冻结**（颗粒度文档 §3.1）：门面与载体之间**不得**另立第二套读写接口。

### 2.5 `MemoryQuotaError`

`type MemoryQuotaError struct { Kind string; Limit int }`；`Kind` 取 `QuotaKindEntry = "entry"` / `QuotaKindSection = "section"`——**判类型不比文本**（`errors.As` 形态），文案由工具层映射为可读拒绝（点名触发的上限 + 可执行下一步：改写归档区 / 压缩内容）。

## 3. 新增实体：实现侧（门面 / 两档载体 / 工具输入）

### 3.1 `memory.Service` 与 `Options`（门面——唯一对外实现面）

| 项 | 形态 | 约束 |
| --- | --- | --- |
| `Options` | `{Backend core.LongTermMemoryStore; Logger *slog.Logger}` | `Backend == nil` → `NewService` 返回错误（**fail-closed**：没有载体就没有记忆） |
| `NewService` | `func NewService(opts Options) (*Service, error)` | 编译期断言：`var _ core.LongTermMemoryStore = (*Service)(nil)` + `var _ core.MemoryReader = (*Service)(nil)` |
| per-scope 锁 | 按**完整 `core.Scope` 分档**：`map[core.Scope]*sync.Mutex` + 查表小锁（每个 Agent 一把 + 全局层一把） | 只覆盖写入临界区（解析 + 判定 + 替换）；**同 Scope 串行、不同 Agent 互不阻塞**；跨进程**不承诺**。按「层」只建两把 → 所有 Agent 共用一把（被 `TestService_AppendLocksAreKeyedByScope` 判红） |

**`LoadEffective` 的路由表（回退判定——唯一判定点）**：

| 输入 | 分支 | 输出 |
| --- | --- | --- |
| `agent != ""` 且 Agent 层**有内容**（核心区或归档区**任一非空**） | 命中 Agent 层 | Agent 层核心区文本（全局层**一个字都不进**） |
| `agent != ""` 且 Agent 层为空层（不存在 / 两区皆空） | 回退 | 全局层核心区文本 |
| `agent == ""` | 全局层 | 全局层核心区文本 |
| 载体返回错误（I/O 档 / 非空损坏档） | **原样上抛** | `("", err)`——注入面拒绝启动（零 LLM） |

### 3.2 markdown 档：`MarkdownStore` 与 `MEMORY.md` 文件形态

`func NewMarkdownStore(root string) *MarkdownStore`——全部读写经 `os.OpenRoot(root)` 根句柄（本轮 `go doc os.Root` 实测：`MkdirAll` / `Rename` / `ReadFile` / `WriteFile` / `OpenFile` / `Remove` / `Stat` 齐备）。

**文件实体形态（两分区 + 条目）**：

| 结构行 | 判据 |
| --- | --- |
| 文件标题 | **恒为第 1 行**（模板产物是 `# 记忆`——US-1 的 `templateMemoryMD`） |
| 分区标题 | `## 核心记忆` / `## 归档记忆`——**只认零缩进**；两标题各**恰好一次**，且核心区**必须先于**归档区 |
| 条目 | 零缩进 `"- "` 首行 + **两空格**缩进的后续行（含空行） |
| 合法扩展 | 多余的 `###` 小节、裸段落（人为手写）——按条目收集并**原样重建**，**不算损坏** |

**损坏判据**（拒绝使用、拒绝覆盖、**原文件逐字节不变**，错误点名文件与症状）：缺任一分区标题 · 同一分区标题出现两次 · 归档区早于核心区。

**写入序列（原子替换）**：**写入入口规范化一次**（`normalizeEntry(content)` = `TrimSpace`，在任一步之前）→ 读原文 → `parseSections` → 配额求和（规范化后条目正文 UTF-8 字节，排除标题 / `"- "` 前缀 / 缩进 / 条目间空行）→ 判定（超限 → `*core.MemoryQuotaError`）→ `renderMemory`（渲染作用于规范化文本）→ 写**唯一名临时文件**（同目录）→ `os.Root.Rename` 覆盖 → 失败删临时文件且**旧文件逐字节不变**。

**读取面三档（不得塌成一档）**：

| 档 | 条件 | 返回值 | 下游 |
| --- | --- | --- | --- |
| ① 空层 | 文件不存在（`fs.ErrNotExist`）/ 合法空层 | `("", nil)` | 回退照常；首次写入即创建（含 `MkdirAll` 建 Agent 层目录） |
| ② I/O | 权限 / 读失败 | `("", err)` | 注入面拒绝启动；写入面不触碰原文件、原样上抛 |
| ③ 损坏 | 非空但结构损坏 | `("", err)`（点名文件与症状） | 拒绝使用、拒绝覆盖 |

### 3.3 sqlite 档：`storage.MemoryStore`

`func NewMemoryStore(db *DB, now func() time.Time) *MemoryStore`——「**写入入口 `strings.TrimSpace` 规范化**（先于单条检查与 `INSERT`；与 markdown 档同一个标准库函数）+ `SUM(octet_length(content))` 配额求和 + `INSERT`」在**同一事务**内（包内复用非导出原语 `withWriteLock`，**不导出**）；`Load` 渲染**规范文本**（同形两分区——与 markdown 档同一解析器可切出同一分区结构；**不承诺**逐字节相同）；`RecallByKeyword` 用 `instr` 且按 `id` 升序。跨进程并发**不承诺**。

### 3.4 两件工具的输入结构（`internal/tool/memorytools.go`）

| 工具 | 输入字段 | Schema 约束 | 默认与拒绝 |
| --- | --- | --- | --- |
| `save_memory` | `content`（必填，`string`）· `scope`（可选，枚举 `CORE` / `ARCHIVAL`） | 枚举**区分大小写**（`core` / `Core` / `ARCHIVE` → `invalid_input`、零写入） | 省略 / `null` → 归档区（缺省档）；`content` **规范化（`TrimSpace`）后为空** → `invalid_input`、零写入（纯空白与空串同档）；入库的是**规范化文本**；超配额 → 映射为可读 `invalid_input`（判 `*core.MemoryQuotaError`） |
| `recall_memory` | `query`（必填，`string`） | 整串、区分大小写 | `query` **规范化后为空** → `invalid_input`、**零检索**（纯空白与空串同档）；检索作用于**规范化后的**查询词与条目正文；未命中 → `(无匹配条目)` + **成功**（不映射 `not_found` / `runtime_error`） |

**作用域解析（只此一处）**：`env.AgentName != ""` → `core.Scope{Agent: env.AgentName}`；`== ""` → `core.GlobalScope`——模型**不得**传 `agent_name`（越权面：一个 Agent 的会话不得写另一个 Agent 的记忆）。

### 3.5 `workspace.MemoryFilePath`（路径约定的**唯一事实源**）

| 输入 | 返回 |
| --- | --- |
| `""`（全局层） | `memory/MEMORY.md` |
| `<name>`（Agent 层） | `memory/agents/<name>/MEMORY.md` |

两条路径均**相对工作区根**、**无前导斜杠**；`memory/agents/` 目录**不预建**（`Init` 有意不建，首次写入时 `MkdirAll` 按需创建——US-1 既有语义）。`memory/MEMORY.md` 由 `Init` 的 `fileTemplates` 落模板原文（含两分区标题）。

## 4. 状态与转移

**层（一个作用域 = 一份存储）的状态机**：

```text
不存在 ──首次 Append（按需建目录与文件）──▶ 空层（两分区标题齐备、零条目）
   │                                              │
   └──────────── Load：档 ①（空层）──────────────┘
                                                  │ Append（配额内）
                                                  ▼
                                            有内容（≥1 条目）
                                                  │
                                     ┌────────────┴────────────┐
                                     │ 人工编辑破坏结构          │ Append / Load
                                     ▼                          ▼
                              损坏（档 ③）── 拒绝使用 / 拒绝覆盖，原文件逐字节不变
```

**注入面的运行级转移**：每次运行开始 → `LoadEffective` 读一次 → 得到文本（或错误）→ **运行内冻结**（循环每轮重组提示词时复用同一份文本，**不重读**）→ 运行结束丢弃（**不缓存**跨运行）。手工编辑的生效时机 = **下一次运行**（运行内冻结的直接推论）。

**保留语义**：归档区**只追加**——删除留缺口（Compatibility §2.4 #41 的当前承诺）；本 US **不做**裁剪 / 压缩 / 迁移。

## 5. 不变量（逐条带判据）

| # | 不变量 | 判据（机器可判 / 用例） |
| --- | --- | --- |
| 1 | 两层**回退不叠加** | `TestService_LoadEffective_TwoLayerFallbackWithoutStacking`（含「Agent 层只有归档」档）· 两档同一断言 |
| 2 | 运行内**冻结** | `TestAgentService_MemoryReadOncePerRun`（`scriptedMemoryReader` 第 2 轮返回不同文本，循环不得看到） |
| 3 | 核心区**永不截断** | `TestOversizedManualMemory_WarnsAndInjectsFully`（逐字节等于完整期望文本 + 尾部哨兵）；超预算的处置是拒绝启动（`TestAgentService_OversizedMemoryFailsRunBeforeModelCall`，含对照组） |
| 4 | 段 2 空记忆 = **US-4 逐字节** | `TestPromptBuilder_Build_EmptyMemoryIsByteIdenticalToSectionOne` |
| 5 | 两档配额**同一算法**（含**同一规范化表示**） | `TestCarriers_QuotaBoundaryExactLimit`（8 条恰 4096 字节 → 两档都恰 32768 放行、第 9 笔拒；**首尾空白档**：8 条首尾各带一空格、原长 4096 → 两档都合计 32752 且都放行紧接的 `"x"`）· `TestMarkdownStore_QuotaCountsEntryBytesExcludingScaffold`（首尾空白不计入） |
| 6 | 写入**原子**（配额判定 + 追加） | `TestMemoryStore_ConcurrentAppendNeverExceedsCoreQuota`（12 笔并发恰 8 笔通过）· `TestMarkdownStore_AppendReplacesAtomicallyWithoutResidue`（失败时旧文件逐字节不变） |
| 7 | 同作用域并发写**一条不丢**；锁**按完整作用域分档**（同 Scope 串行、不同 Agent 不互相阻塞） | `TestService_ConcurrentAppendSameScopeSerialized` / `TestCarriers_ConcurrentAppendKeepsEveryEntry`（多重集比较，`-race`）· `TestService_AppendLocksAreKeyedByScope`（`blockingStore` 闸门夹具） |
| 8 | **规范化后为空**的输入**工具层即拒** | `TestMemoryTools_RejectEmptyInputs`（四态：空串 + 纯空白；`countingStore` 证零写入 / 零检索） |
| 9 | 载体**隔离** | `TestMemoryDirIsolation`（文件工具拒写 `memory/` 且规则名与指引逐字 + 原文件逐字节不变 + 记忆工具正向对照） |
| 10 | 读取面**三档不塌** | `TestService_LoadEffective_ErrorTiersDoNotCollapse` · `TestMarkdownStore_AppendRefusesCorruptedFileKeepsOriginalBytes` |
| 11 | 零迁移 / 零新增配置键 / 零新增坐标 | `git diff docs/contracts/schema.sql` 空 · `basalt.yaml.example` 零 diff · `git diff go.mod go.sum` 空 |
| 12 | 快照**零记忆字段** | `grep` `ConfigSnapshot` 结构零改动（FR-007） |

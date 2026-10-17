# Phase 0 Research: US-6 Memory 长期记忆

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-17

**背景**：本 feature 的设计面已冻结（颗粒度文档 [docs/us/us6-memory.md](../../docs/us/us6-memory.md)「待决事项」已清零——4 条全部裁决落案 2026-10-17；澄清会话零提问，两处上游一致面已就地收敛）。Technical Context **无 NEEDS CLARIFICATION**。本文件不重新设计，而是把已冻结的裁决整理为可执行的**决策记录**（Decision / Rationale / Alternatives considered）+ **写前核实清单**，供 `/speckit-tasks` 与 implement 逐步引用。

## D-01：段 2 注入的传递路径——`RunEnv` 第三个非导出注入位

- **Decision**：记忆文本经 `core.RunEnv` 新增的**非导出字段** `memory string` 传给 `PromptBuilder.Build`；`Build` 拼成 `Prompt.System = env.Snapshot.SectionOne + "\n\n" + env.memory`（`env.memory == ""` → 与 US-4 **逐字节相同**）。
- **Rationale**：`RunEnv` 已有两个同形态先例（`llmClient ProviderClient` / `agentDir string`——非导出 = 不是契约字段、不进任何出站形态、不参与序列化）；记忆文本的消费方就是 `Build`，注入位与消费点同层。宪法 VII 明令 `context.WithValue` **只允许追踪标识**——记忆正文是业务状态，**不得**走 ctx。
- **Alternatives considered**：
  - *`context.WithValue`*：拒——违宪法 VII（业务状态不得进 ctx），且 `Build` 会拿到一个不可静态检查的隐式输入。
  - *`ConfigSnapshot` 加记忆字段*：拒——FR-007 明禁；快照是**配置级**事实（落库、参与哈希），记忆是**运行级**输入（每次运行可变、不参与快照哈希），两者语义不同层。
  - *`Prompt` 结构加一个独立字段（段 2 单列）*：拒——下游（provider 适配）只认 `Prompt.System` 一个字符串；加字段会让「段一 + 段二」的合成点分裂到消费方，且 `estimateTokens(p.System)` 的预算判据（R4b）会漏算段 2。
  - *`PromptBuilder` 直接持 `MemoryReader`*：拒——`Build` 每轮被循环调用，直读载体 = 每轮重读，R2（运行内冻结）必红。

## D-02：读取时机与失败处置——运行开始时读一次、失败即拒绝启动

- **Decision**：`AgentService.Run` 的**步骤 ④**（组装 `RunEnv` 时）调一次 `opts.Memory.LoadEffective(ctx, req.Agent)`；失败（I/O 档或非空损坏档）→ 既有 `StatusFailed` 形状（`RunResult{RunID, Status: StatusFailed, Reason: err.Error()}, nil`）、**零 LLM 调用**；结果写入 `env.memory`。
- **Rationale**：FR-005 / FR-023——「每运行读一次、运行内冻结」把读取点钉死在唯一一处（步骤 ④ 是 `RunEnv` 的唯一组装点）；失败处置与「段一读取失败」同口径（US-4 既有形状），复用同一失败面不新建错误语义。
- **Alternatives considered**：*循环内首轮读取*：拒——读取点会落进循环体，与「循环不值一提地薄」的分层相悖，且首轮失败与后续轮失败的语义会分叉；*后台预读*：拒——宪法 VII 禁异步编排。
- **落地注意**：`ServiceOptions.Memory` 是**可空**字段（nil = 段 2 恒空，行为与 US-4 逐字节相同）——既有 `core` 用例因此**零改动**（§3.5 第 4 项）。

## D-03：两层回退的判据归口——门面 `LoadEffective`，回退**不叠加**

- **Decision**：`memory.Service.LoadEffective(ctx, agent)`：`agent != ""` → 先读 Agent 层；**该层有内容**（核心区或归档区**任一非空**）即命中、直接返回其核心区文本；否则回退全局层；`agent == ""` → 全局层。命中 Agent 层时全局层**一个字都不进系统提示词**。
- **Rationale**：FR-004——回退的判据是「Agent 层**有内容**」而不是「Agent 层核心区非空」：Agent 层只有归档条目时**也不回退**（R3 的就地档：「Agent 层只有归档」不得把全局核心区拉进来）。这条档位差异只有一处能判，判定归口必须在门面（载体不知道"有另一层"）。
- **Alternatives considered**：*每层各返回核心区、由调用方决定用哪个*：拒——调用方（`service.go`）会变成第二个回退判定点，两处判必漂移；*叠加（段 2 = 全局核心区 + Agent 核心区）*：拒——技术方案 §5.2 明定「不叠加」，叠加会让 Agent 层无法"覆盖"全局偏好。

## D-04：读取面三档的判定归口——载体判，门面只做回退决策

- **Decision**：三档由**载体**产出——① 文件不存在（`fs.ErrNotExist`）/ 合法空层 → `("", nil)`；② 其他 I/O 错误 → `("", err)`（原样上抛）；③ 非空但结构损坏 → `("", err)` 且**错误点名文件与症状**。门面只做一件事：`err == nil && 文本为空` → 回退。写入面同理：② / ③ 档**不触碰原文件**，原样上抛。
- **Rationale**：FR-027 明禁「用一个 ok 抹平三档」——档位的**事实来源是文件系统与文件内容**，只有载体能区分 `ErrNotExist`（正常）与 `EACCES`（异常）。门面若自己 `Stat` 一遍就是第二处判定。
- **Alternatives considered**：*门面先 `Stat` 再决定*：拒——第二处判定 + TOCTOU；*把损坏降级为 WARN 当空层*（上游旧表述）：**已废止**（2026-10-17）——那样会静默吞掉用户手写坏的文件，且后续 `Append` 会把损坏内容覆盖掉。

## D-05：配额求和的单一算法——两档**逐字节同源**

- **Decision**：求和单位 = **条目正文自身的 UTF-8 字节**（重建后、`TrimSpace` 后）；排除载体添加物（文件标题 / 分区标题 / `"- "` 前缀 / 续行缩进 / 条目间空行）。markdown 档在门面 per-scope 锁内解析 → 求和 → 判 → 替换；sqlite 档 `SUM(octet_length(content))`。单条 ≤ 4096（只限 CORE 写）、每层核心区总量 ≤ 32768。
- **Rationale**：FR-029——两档必须是**同一个算法**，否则"换档只改一行配置"这句承诺在配额边界上失效。可判读数：**8 条恰 4096 字节的条目在两档都恰好到 32768 并放行、第 9 笔被拒**（`carrier_contract_test.go` 的 `TestCarriers_QuotaBoundaryExactLimit` 就是这条断言的机器形态）。
- **Alternatives considered**：*markdown 档按整文件字节求和*：拒——会把标题 / 前缀 / 缩进算进去，两档读数必分叉；*按 rune 数求和*：拒——中文 3 字节 / 字符，按 rune 会让"4 KB"名不副实（FR-007 逐字要求按 UTF-8 字节）。
- **落地注意**：`octet_length` 是 SQLite 3.43+ 的内建函数——**写前核实项 2**（若驱动捆绑版本不支持，fallback `length(CAST(content AS BLOB))`，读数不变）。

## D-06：sqlite 原子性——包内复用 `withWriteLock`，不自建事务

- **Decision**：`MemoryStore` 的「配额求和 + INSERT」在**同一事务**内完成，经 `storage` 包内**非导出**原语 `withWriteLock(ctx, db, fn)`；跨进程并发**不承诺**。
- **Rationale**：FR-012——先查后写不原子时，N 个并发写会一起通过配额检查（R6：并发 12 笔必须**恰 8 笔**通过）。`withWriteLock` 是既有原语（`storage/db.go:276`，`BEGIN IMMEDIATE` + 钉连接），复用它是「依赖方向无环 + 单一实现」的选择；`withWriteLock` **不导出**（本 US 在包内使用，导出会变成第二个公开入口）。
- **Alternatives considered**：*自建 `db.ExecContext("BEGIN IMMEDIATE")`*：拒——第二套事务路径，锁策略会漂移；*乐观重试（写入后复查配额再删）*：拒——多一步删除、且在"只追加、删除留缺口"的承诺下语义可疑。

## D-07：同作用域串行化——门面 per-scope 锁（**按完整 `Scope` 分档**），不设全局锁

- **Decision**（**2026-10-17 R3 裁决 P2-b 修订**）：门面按**完整 `core.Scope` 分档**持锁——`map[core.Scope]*sync.Mutex` + 查表小锁：`Scope{Agent: "alice"}` 一把、`Scope{Agent: "bob"}` 一把、`GlobalScope` 一把；锁只覆盖「解析 + 判定 + 替换」这一写入临界区。
- **Rationale**：FR-013 / FR-017——markdown 档的「读改写」需要串行化（sqlite 档由事务兜底，门面锁作为统一语义仍持）。**按完整作用域分档**才是 "per-scope" 的字面含义：不同 Agent 的写入彼此独立、不得互相阻塞；跨进程不承诺（技术方案 §5.3 的既有边界，如实登记）。判据 = `TestService_AppendLocksAreKeyedByScope`（`blockingStore` 闸门夹具：① 同 `Scope` 第二笔在首笔放行前不进入载体；② `alice` 阻塞期间 `bob` 必须能进入）。
- **修订依据（本决策的上一版被推翻，留痕不删）**：上一版（2026-10-17 评审回合 R2）为「门面持**两把** `sync.Mutex`（Agent 层 / 全局层各一把）」，且 Alternatives 明确**拒掉了**"按 scope 值建 map 锁表"。该版被推翻的直接理由：按「层」分的两把锁中，Agent 层那**一把**会被**所有** Agent 共用——`alice` 的写入阻塞 `bob`，与「per-scope 串行」承诺、乃至上一版 Rationale 自己写的"不同 Agent 层的写入互不阻塞"**自相矛盾**（同一层内多 Agent 恰是最常见形态；越权面之外，这是纯粹的性能与耦合问题）。上一版"动态锁表是没必要的复杂度"的判断也站不住：map 的并发面由查表小锁 + 只增不删的条目管理解决，实现量约十行，而"每个 Agent 一把"才是该契约的最小正确实现。
- **Alternatives considered**：*一把全局互斥锁*：拒——无关作用域的写入被串行化，且掩盖「per-scope」这一契约事实（P2-b 判据 ② 直接红）；*按层两把*（**上一版决策，已推翻**）：拒，理由见上。

## D-08：作用域只在工具层解析——模型不传"层"

- **Decision**：两件工具的 `Execute` 里：`env.AgentName != ""` → `core.Scope{Agent: env.AgentName}`；`== ""` → `core.GlobalScope`。模型**不得**传 `agent_name`、不得从路径反推；工具参数 `scope` 的取值 `CORE` / `ARCHIVAL` 是**分区**（落 `memory_entries.section`），参数名**不得**改成 `section`。
- **Rationale**：FR-008 / FR-021——「哪个 Agent 在说话」是**运行上下文事实**（`ToolEnv.AgentName` 由运行侧填），不是模型可伪造的输入；让模型传 = 越权面（一个 Agent 的会话能写另一个 Agent 的记忆）。双 `scope` 命名照契约 §5.4 注（**两个 `scope`**：参数 = 分区、表列 = 分层）——改名会与 `memory_entries.scope` 列撞名。
- **Alternatives considered**：*参数叫 `section` 更"清楚"*：拒——对外字面量已冻结（契约 §5.4 参数表 + `AGENT.md` 文档），改名 = 改已定字面量（软门禁）；*工具从 `env.AgentDir` 反推 Agent 名*：拒——路径反推是第二处事实源，且无 Agent 上下文时 `AgentDir` 为空串，反推会退化成空名。

## D-09：文案权威面——确认文案取 §6.2 口径 11，`§2.3` 伪代码是占位形态

- **Decision**：`save_memory` 成功文案逐字 = 颗粒度文档 §6.2 口径 11（点名**作用域**与**分区**）；`recall_memory` 未命中 = `(无匹配条目)`（**成功**返回，不是错误）。
- **Rationale**：FR-010 / §6.2——§2.3 伪代码里的 `层名(scope)` / `分区名(section)` 是**占位符调用**（函数形态），不是第二套字面量；口径 11 是权威面。未命中判成功是刻意的：「没有这条记忆」是正常结果，映射成 `not_found` / `runtime_error` 会让模型把"没记住"当成故障去重试。
- **Alternatives considered**：*未命中返回错误*：拒——FR-010 明禁；*返回空串*：拒——模型看不到任何反馈会以为工具坏了，`(无匹配条目)` 是显式的"检索过、没有"。

## D-10：检索语义——整串 `contains`、区分大小写、按写入顺序

- **Decision**：召回 = 归档区条目里做**整串子串匹配**（sqlite：`instr(content, ?)`；markdown：`strings.Contains`），区分大小写，按写入顺序（`ORDER BY id` / 文件条目序）返回。**连续短语命中、分散词不自动组合**；不分词、不扩展、不向量化。空 `query` 在**工具层**即拒（`invalid_input`，零检索）。
- **Rationale**：FR-009——语义面在 2026-10-17 澄清会话收敛（旧表述「多词短语必不命中」与 `contains` 实现不符，已同步上游四处）。空 `query` 必须工具层拒：`instr(content, '')` 的空针会**匹配全部条目**（这正是「分散词不自动组合」的反面陷阱——按词拆分会让单个常见词召回一片）。
- **Alternatives considered**：*分词 + 逐词 AND*：拒——超出冻结语义（且把「连续短语」的判据弄成不可判）；*`LIKE '%' || ? || '%'`*：拒——`LIKE` 需转义 `%` / `_`，`instr` 无此问题；*大小写不敏感（`lower()` / `NOCASE`）*：拒——契约字面量是区分大小写。

## D-11：配置面的类型化与取值域——fail-closed，零新增键

- **Decision**：`cli.Config.Memory` 从 `map[string]any` 改为 `MemoryConfig{Backend string}`（yaml 键 `backend`）；`validateConfig` 判取值域：缺省（空串）→ `markdown`；`markdown` / `sqlite` 合法；`mem0` 与任何未知值 → **拒绝启动**并点名键与取值。`basalt.yaml.example` **零改动**（键已存在）。
- **Rationale**：FR-018——`mem0` 是**已登记的延后档**（Compatibility §4 #2），"取值域外即拒绝"比"静默回退 markdown"诚实：静默回退会让用户以为 mem0 生效了。类型化是取值域校验的前提（`map[string]any` 判不了拼写错误）。
- **Alternatives considered**：*保留 map + 运行时取值断言*：拒——错拼的键名（`backends:`）会静默通过；*未知值回退 markdown + WARN*：拒——fail-closed 是本项目配置面的一致口径（providers 段已如此）。

## D-12：模板默认工具与注册池的一致性——连带判据

- **Decision**：`workspace/templates.go` 的 `agentTemplate` 默认工具 `[]string{}` → `[]string{"save_memory", "recall_memory"}`；`app/wire.go` 注册池 6 → 8；两者**必须同时落地**——装载器的名字集规则（宪法 IV）会让"模板声明了池外工具"直接使新建 Agent **整只不注册**。
- **Rationale**：FR-026 + 需求 §10.2 逐字要求。连带影响（实测，见 plan「文档 ↔ 仓库事实差异」）：`agent_files_test.go` 两处装载器需注入这两个名字（模板声明的就是它们）、`ports_test.go:304` 的「恰好六件」断言扩为八件。
- **Alternatives considered**：*模板保持 `tools: []`、用户手填*：拒——需求 §10.2 逐字要求默认可用；*池不加、模板不加，等到 US-7*：拒——US-6 的全部交付价值（Demo 二）要求新装的 Agent 开箱能记忆。

## D-13：markdown 原子替换——唯一名临时文件 + `os.Root.Rename`

- **Decision**：写入路径 = 读原文 → 解析 → 判定 → 渲染 → 写**唯一名临时文件**（同目录）→ `os.Root.Rename` 覆盖 → 失败时删临时文件且**旧文件逐字节不变**；全部经 `os.OpenRoot(root)` 根句柄（含 `MkdirAll` 按需建 Agent 层目录）。
- **Rationale**：FR-012 / 约束 12——`rename` 是同文件系统内的原子替换；唯一名避免并发写者互相覆盖临时文件。`os.Root` 的句柄语义把路径逃逸挡在 API 层（`..` / 绝对路径 / 符号链接逃逸）——这是「载体路径经工作区根句柄」纪律（FR-015）的实现面。
- **本轮实测（`go doc os.Root`，2026-10-17）**：`os.OpenRoot(name string) (*Root, error)`；`*Root` 具备 `MkdirAll` / `Rename` / `ReadFile` / `WriteFile` / `OpenFile` / `Remove` / `Stat` / `Lstat` / `OpenRoot`（含 `RemoveAll`）——本 feature 需要的全部操作在 `*Root` 上都存在，**无需**退回 `os` 全局函数（那会重新打开路径逃逸面）。
- **Alternatives considered**：*直接用 `os.WriteFile`（不经 Root）*：拒——路径逃逸面回归，且与 FR-015 逐字冲突；*写后 `os.Remove` 原文件再 rename*：拒——删了再写之间原文件不存在，中途失败会丢数据；*加锁文件 / 写锁*：拒——跨进程不承诺（D-07），进程内已有 per-scope 锁。

## D-14：损坏判定的边界——合法扩展不算损坏

- **Decision**：**损坏**（拒绝使用、拒绝覆盖、原文件逐字节不变）= 缺 `## 核心记忆` 或 `## 归档记忆`、同一分区标题出现两次、归档记忆早于核心记忆；**合法扩展**（多余的 `###` 小节、人为手写的裸段落）= 不算损坏，裸段落按条目收集并原样重建。
- **Rationale**：FR-027 / FR-028——`MEMORY.md` 是人可读可手改的载体（markdown 档的卖点），把"多写了一个小节"判成损坏等于拒绝用户自然的编辑方式；反过来，结构缺失或标题重复会让解析器给出**不确定**的分区归属（哪一段算核心区？），必须拒绝而不是猜。
- **Alternatives considered**：*宽容解析：缺标题就按"无核心区"处理*：拒——后续 `Append` 会写进一个结构不明的文件，"只追加"承诺落空；*只认严格骨架、多余内容一律拒*：拒——与 markdown 档的交付价值冲突。

**另**：结构行只认**零缩进**（文件标题恒为第 1 行 + 两个分区标题）——条目 = 零缩进 `"- "` 首行 + 两空格缩进的后续行（含空行）；写入与重建**无损可逆**（`TestCarriers_EntryBoundariesSurviveRoundTrip`：条目内含 `## 核心记忆` 字样、`"- "` 开头行等边界值，两档都一条进一条出）。

## D-15：条目正文的**规范表示统一**——`TrimSpace` 在写入入口做一次，六面同表示

- **Decision**（**2026-10-17 R3 裁决 P1-b 新增**）：条目正文与召回查询词的**唯一规范表示 = `strings.TrimSpace(原文)`**；规范化只在**写入入口**执行一次——markdown 档 `added := normalizeEntry(content)`（在**两条配额检查之前**）、sqlite 档 `c := strings.TrimSpace(content)`（在**单条检查与 `INSERT` 之前**，与 `normalizeEntry` 是同一个标准库函数）；工具侧 `args.Content = strings.TrimSpace(args.Content)`（`args.Query` 同）后**再**判空与调用。**写入、单条配额检查、总量配额求和、存储、重建渲染、召回（含查询词）六面 MUST 作用于同一表示**。
- **Rationale**：颗粒度文档 §2.2 约束 29 / FR-022——规范化若只发生在「重建渲染」一侧，两档的**配额读数会分叉**，同一份输入在 markdown 档与 sqlite 档得到不同的准入判定（两档同语义是 FR-017 的核心承诺）。用户给的反例是机器可判的：写入**八条首尾各带一个空格、原长 4096 字节**的内容——只在渲染侧 `TrimSpace` 时 markdown 档合计 32752（放行）、sqlite 档按原样 `octet_length` 求和得 32768（也放行，但**紧接**一笔 `"x"` 的判定相反：32753 放行 / 32769 拒）；写入侧一次规范化后两档**都**得 32752、判定一致。首尾空白在语义上是噪音（用户粘贴带入），规范表示把它消掉；条目**内部**的空白与换行**保留**（FR-029：求和含用户内容里的空白与换行）。
- **首尾空白边界必须有用例**（MUST）：`TestCarriers_QuotaBoundaryExactLimit` 加首尾空白档（两档读数与判定必须一致）；R8 四态用例覆盖 `{"content":"   \n\t "}` 与 `{"query":" "}`（规范化后为空 = 与空串同档）。
- **废止的旧断言**（留痕，不删）：颗粒度文档原末句「sqlite 侧存量 `content` 列是原样存的用户输入，与重建口径天然一致」——**该断言不成立**：原样存储不等于两侧求和作用于同一表示，正是分叉的来源。R3 裁决后该句作废，改以上述六面同表示口径为准。
- **Alternatives considered**：*只在渲染侧 `TrimSpace`（原版）*：拒——两档配额读数分叉（上述反例）；*两侧都不规范化、原样存储*：拒——markdown 档重建后必然掉落首尾空白，"存储 == 重建 == 召回"三面不一致，且用户手改文件时的自然写法（行尾空格）会与工具写入产生两种表示；*入库时规范化 + 检查时再规范化一次*：拒——同一次规范化重复执行无意义，入口一次是全流程不变量的最小实现（`TrimSpace` 幂等，重复不会更错，但会掩盖入口遗漏）。
- **落案位置**：颗粒度文档 §2.2 约束 29（子项 1~5）、§2.3 流程 B/C 与三块伪代码、§3.2 `markdown.go` / `memory_store.go` / `memorytools.go` 行、§6.2 口径 23；`spec.md` FR-022 / FR-029；`data-model.md` 不变量 5 / 8。

## 写前核实清单（H3——implement 起步前逐项执行并记录读数）

| # | 核实项 | 手段 | 若不符合预期的处置 |
| --- | --- | --- | --- |
| 1 | `os.Root` 的方法面（`MkdirAll` / `Rename` / `ReadFile` / `WriteFile` / `Remove` / `Stat`） | `go doc os.Root`——**本轮已实测**，全部存在（见 D-13） | —（已验证） |
| 2 | SQLite 驱动捆绑版本的 `octet_length` 支持 | 仓库外一次性探针模块（`modernc.org/sqlite v1.60.1` 已在模块缓存）：建内存库 → `SELECT octet_length('中文')` 读回 6 → 用毕即删 | 换 `length(CAST(content AS BLOB))`，读数与两档同源判据不变（须记入 run-state） |
| 3 | `instr(X, '')` 空针语义 | 同一探针：`SELECT instr('abc','')` 读数 | 不影响（空 `query` 在工具层已拒）；读数仅作常识登记 |
| 4 | `core/loop_test.go` 现行 `newLoopHarness` 形态（是否有选项模式） | 读文件 + `grep -n "func newLoopHarness" -A 30` | 若已有 options 形态 → `withMemory` 并入既有选项类型；若无（§3.2 的假设）→ 新增 `harnessOption`，默认零值 = 既有行为 |
| 5 | `cli/config_test.go` 现有 `Memory` 断言形态 | `grep -n "Memory" internal/cli/config_test.go` | 依赖 map 形态的断言按**等价重构**（取值不变）；`mem0` 那条是判据面变更（随 tasks 送审） |
| 6 | `app/wire.go` 六件工具循环的现行写法 | 读 ⑤ 段 | 照形追加两件（构造子 `NewSaveMemoryTool` / `NewRecallMemoryTool` 持门面） |
| 7 | `storage` 既有 store 构造范式（`*DB` + 时钟的传入形态） | 读 `storage/session.go` / `tool_invocation.go` 的构造子与 `DB` 方法面 | `NewMemoryStore(db *DB, now func() time.Time)` 照形；若既有范式用 `DB.Tx` 而非 `withWriteLock` → 停下报告（不得自建第二套事务路径，D-06） |
| 8 | 上下文预算估算的现状（段 1 是否已计入 `estimateTokens`） | 读 `core/prompt.go` 的预算段 | R4b 的判据（Reason 点名 `budget.context_tokens`、零 LLM）依赖它把 `p.System` 整串计入——若只估段一，则本 US 顺带修正为整串（**属实现修正，不是判据放宽**，须在 run-state 记录） |
| 9 | `slog` 用法与 `Logger` 空值兜底范式 | 读既有 `memory` 相邻包（如 `workspace`）的 logger 处理 | 照形；注入期 WARN **不得**打记忆正文 |
| 10 | `core.ToolEnv` 字段面（`AgentName` / `AgentDir` / `Timeout` 现读） | 读 `core/tool.go`（或等效文件） | 记忆工具只需 `AgentName`；若字段名不符 → 以实测为准并停下报告（§3.1 逐字签名的前提） |

## 冻结来源索引（本文件每条决策的出处）

| 决策 | 颗粒度文档 | 技术方案 / 契约 |
| --- | --- | --- |
| D-01 / D-02 | §2.3 流程 B、§3.1（`core/prompt.go` / `core/service.go` 改动面）、§3.5 第 4 项 | 技术方案 §4.2（段 2 记忆注入）、§11.5（快照语义） |
| D-03 / D-04 | §2.2 约束 27、§2.3 流程 A、§4.3 R3 / R9 | 技术方案 §5.2（两层作用域与回退） |
| D-05 / D-06 | §2.2 约束 7 / 29、§3.4 列级口径 | 技术方案 §5.1 契约 2、契约 §6 预算表 |
| D-07 | §2.2 约束 13 / 19、§3.2 `memory_test.go` 行（10 条）、R2→R3 裁决表 P2-b 行 | 技术方案 §5.3（载体分档） |
| D-08 | §2.2 约束 8 / 21、§6.2 口径 8 / 9 | 契约 `agent-md.md` §5.4（两个 `scope` 注） |
| D-09 / D-10 | §6.2 口径 11、§2.3 伪代码、注 6 | 契约 `agent-md.md` §5.4 检索语义澄清块 |
| D-11 | §3.3 配置面、§3.1（`cli/config.go`） | 技术方案 §3 配置面 |
| D-12 | §3.5 第 2 / 3 项、§3.2 测试交付物 | 需求文档 §10.2；宪法 IV |
| D-13 / D-14 | §2.2 约束 12 / 27 / 28 | 技术方案 §5.4（MEMORY.md 形态） |
| D-15 | §2.2 约束 29（子项 1~5）、§2.3 流程 B / C、§6.2 口径 23、R2→R3 裁决表 P1-b 行 | 技术方案 §5.1 契约 3 / §5.3（载体分档）、`spec.md` FR-022 / FR-029 |

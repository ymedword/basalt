# run-state — US-6 Memory（feature 006-memory）

> **性质声明（三声明，本文件常驻头部）**
>
> 1. 本文件是**过程状态，非设计工件**——speckit 命令**不得**把它当设计输入；
> 2. 失败台账**只追加**：一行一次尝试，不改旧行（可 diff 审计）；
> 3. 唯一允许原地翻转的是「未验证清单」的「未验证 → 已验证」，翻转必须注日期与证据。
>
> 本文件**入库 git、收口不删**——它是长任务续跑与验收可信度的过程载体，**不是**探针 / 诊断临时物（「临时物收口即删」惯例**不适用**）。

## 1. 基线

**写入时点**：步骤 0 分流后（2026-10-17）。

| 项 | 值 |
| --- | --- |
| US | US-6（Memory）——台账 `docs/us/REGISTRY.md` 类型列：开发类 → 走完整流程步骤 1~7 |
| feature 目录 | `specs/006-memory/`（glob `specs/006-*/` 单命中；`.specify/feature.json` 作交叉核对） |
| 分支 | `006-memory`（步骤 1.3 手动建；仓库未登记 `before_specify` hook） |
| 起始 commit SHA | `4069fb021d30ebaf0cd85d79c29f705a52feb794`（短：`4069fb0`，`chore: harden loop development mechanism with run-state and hooks`） |
| worktree | **无**——单工作区 `D:/code/basalt`，无并行 US 交错 |

### 1.1 开跑时已有未提交改动（**保留，不得 stash / checkout 覆盖**）

| # | 路径 | 状态 | 归属 |
| --- | --- | --- | --- |
| 1 | `.claude/skills/module-doc-gen/SKILL.md` | M | 流程机制补强轮（规则 7 核心逻辑无条件必备 + 规则 8 harness 判别力） |
| 2 | `docs/AiProgrammingGuide.md` | M | 同轮：§5 七项证据四态口径 / §8.2 回执纪律 / §7 陷阱 #11 检索语义 |
| 3 | `docs/Compatibility.md` | M | US-6 设计轮：§2.2 检索语义同步 |
| 4 | `docs/Roadmap.md` | M | US-6 设计轮：B 方向两处 + M2 检索语义同步 |
| 5 | `docs/contracts/agent-md.md` | M | US-6 设计轮：§5.4 两工具参数表 + 检索语义澄清块 |
| 6 | `docs/us/REGISTRY.md` | M | US-6 登记（`/us-register 6`） |
| 7 | `docs/us/us6-memory.md` | ?? | US-6 颗粒度文档本体（未入库） |

以上 7 项均为**本 US 开工前的既有改动**，非本运行产生；收口报告「变更总结」需按 `git status --short` 实测区分本运行改动与这 7 项。

### 1.2 本 US 允许触碰的文件范围（颗粒度文档 §3 交付物清单）

#### 代码（新增 6 + 改动 7，共 13 文件）

| 类 | 路径 |
| --- | --- |
| 新增 | `internal/core/memory.go`、`internal/memory/memory.go`、`internal/memory/markdown.go`、`internal/storage/memory_store.go`、`internal/tool/memorytools.go`、`internal/workspace/memory.go` |
| 改动 | `internal/core/run.go`、`internal/core/prompt.go`、`internal/core/service.go`、`internal/memory/doc.go`、`internal/workspace/templates.go`、`internal/cli/config.go`、`internal/app/wire.go` |

#### 测试（新增 7 文件 43 条 + 改动 6 文件追加 15 条 + 夹具扩展 2 点 + 既有判据同步 3 处）

> **条数合计与颗粒度文档 §3.2 一致**（**43 + 15 = 41 + 17**）——差异只在文件归属分类，见下第 1 行与 §1.3。`memory_test.go` 由 9 → **10**：R3 裁决 **P2-b** 新增 `TestService_AppendLocksAreKeyedByScope`（锁按**完整 `Scope`** 分档 + `blockingStore` 闸门夹具）；新增测试文件由 6 → **7**：**analyze C1 处置**新增 `internal/memory/import_test.go`（1 条包级架构测试，见 §1.3 续第 10 行）。

| 类 | 路径（条数） |
| --- | --- |
| 新增（7 文件 43 条） | `internal/memory/memory_test.go`（**10**）、`internal/memory/markdown_test.go`（8）、`internal/memory/carrier_contract_test.go`（7，`package memory_test`，test-only 引 `internal/storage`）、`internal/memory/import_test.go`（**1**——**包级架构测试**，**analyze C1 处置**新增：口径 26 / FR-038 的「记忆包生产导入面」判据，沿 `cli/ports_test.go` 先例、正向白名单断言、**纯扩面**）、`internal/storage/memory_store_test.go`（6）、`internal/tool/memorytools_test.go`（9，**`package tool_test`**）、`internal/workspace/templates_test.go`（2——**2026-10-17 核对修正**：该文件在仓库中**不存在**，§3.2 列为「改动」不实；实测按文档点名的文件名**新建**，分类从改动变新增，用例数与验收语义不变） |
| 改动（追加，6 文件 15 条） | `internal/core/prompt_test.go`（3）、`internal/core/service_test.go`（5）、`internal/workspace/agent_test.go`（1）、`internal/cli/config_test.go`（3）、`internal/app/wire_test.go`（3）、`internal/core/loop_test.go`（0 条，**夹具扩展**） |
| 判据扩 1 处 | **实测承载点 = `internal/app/ports_test.go:304`**（「恰好六件」名集断言）——§3.2 / §3.5 第 3 项把它记在 `app/wire_test.go` 的 `TestWire_RegistryNamesReachAgentLoader`，实测该函数判的是**行为**（池内名注册 / 池外名整只不注册），无六名集断言；`app/wire_test.go` 另按文档意图补一条记忆工具经装配链注册成功的行为断言（两面都不落空） |
| 夹具扩展 2 点 | `core/loop_test.go`：`loopHarness` 增 `memory` 字段 + `withMemory` 选项（**构造期**置端口）；增 `fakeMemoryReader` / `scriptedMemoryReader` 两个替身 |

**零改动核对面（列出为核对任务，非改动任务）**：`go.mod` / `go.sum`（第三方坐标零新增）、`basalt.yaml.example`（配置键零新增）、`schema.sql` 与迁移（表结构零改动）、`internal/tool/policy.go` 的 `memory/` 规则与指引文案（US-5 已交付面）、审计面（`internal/storage` 审计路径 + `internal/core` 的 `LLMCallStore` 声明）。

**范围外**：四份权威文档与 `docs/contracts/` 正文、`docs/us/*` 其他 US 文档、`.specify/memory/constitution.md`、`specs/005-*/` 及更早 feature 目录。

### 1.3 已知判据面变更（开工前送审清单，来源：颗粒度文档 §3.5 第 1/2/3/8 项；**第 1/2/3 行按 2026-10-17 仓库实测修正**）

| # | 文件 | 变更 | 归属 |
| --- | --- | --- | --- |
| 1 | `internal/cli/config_test.go` | `mem0` 从「可解析」变「拒绝启动」——既有断言的验收语义改变 | **判据面变更**（随 `TestLoadConfig_RejectsUnknownMemoryBackend` 一并送审） |
| 2 | `internal/workspace/agent_files_test.go`（**非** §3.5 所写的 `templates_test.go`——该文件不存在） | 两处装载器**不注入** `Tools`：`TestCreateAgent_WritesLoadableTemplate`（`:68` 装载器 → `:74` `Registered` 断言破）与 `TestCreateAgent_EncodesNamesThroughYAML`（`:147` / `:151` 同形）——模板声明两件工具后空池使 Agent **整只不注册**；另 `:78` 的 `require.Empty(t, Profile.Tools)`（注释逐字「tools: []（记忆工具到 US-6 才在池）」）同破 | **判据面变更**（模板默认工具落位的必然连带；修法 = 两个装载器注入 `["save_memory", "recall_memory"]` + `require.Empty` → 等值断言） |
| 3 | `internal/app/ports_test.go:304`（**非** §3.5 所写的 `wire_test.go`——该文件无六名集断言） | `TestToolListerPort_HeavyAssemblyRegistersSixTools` 的 `require.Equal` **恰好六件**名集（`:313-315`）+ `Resolve` 循环（`:317`） | **判据面变更**（池 6 → 8；扩集合两件 + `Resolve` 循环增两件 + 函数名同步为事实形态）；`app/wire_test.go` 另按文档意图补一条记忆工具经装配链注册成功的行为断言 |
| 4 | `internal/app/wire_test.go` | 3 条新用例（段 2 进模型入站 / Demo 二最小闭环两档 / sqlite 档不碰文件） | **新增用例**（判据扩面） |
| 5 | `internal/core/loop_test.go` 等夹具 | 新增字段与选项、默认 nil——既有调用点零影响 | **随已批准接口适配**（不改任何既有判据） |

以上 5 项在步骤 5 的固定停点随 tasks 一并送审；批准记录写进本段。第 2/3 行为**颗粒度文档写法与仓库实测不符**之处——不静默改文档，按仓库事实落地并在 [specs/006-memory/plan.md](../006-memory/plan.md) 的「文档 ↔ 仓库事实差异」节留同一份记录。

### 1.3 续：R3 裁决轮追加（2026-10-17，**追加行，不改上表**）

上表 5 项在停点被确认；用户在**同一停点**对 tasks 提出三处修正 + 两处顺手修正（**修正后再作为批准版执行**）。修正本身对既有判据面的影响如下（前五项**不变**，本表只记新增变化）：

| # | 文件 | 变更 | 归属 |
| --- | --- | --- | --- |
| 6 | `internal/memory/memory_test.go`（**新增用例 1 条**，9 → 10） | `TestService_AppendLocksAreKeyedByScope` + `blockingStore` 闸门夹具（P2-b：锁按**完整 `Scope`** 分档；同 Scope 串行、不同 Agent 不互相阻塞） | **判据扩面**（新增用例，不放宽任何既有断言）；同时是 FR-017 的措辞精确化（「Agent 层与全局层各一把」→「按完整作用域分档」） |
| 7 | `internal/tool/memorytools_test.go` 的 R8 用例 | 两态 → **四态**（`content` / `query` 各含空串与**纯空白**；判据 = 规范化（`TrimSpace`）后为空） | **判据扩面**（原「空串」断言保留，新增纯空白态；P1-b） |
| 8 | `internal/memory/markdown_test.go` / `internal/memory/carrier_contract_test.go` / `internal/storage/memory_store_test.go` | 各加**首尾空白**子断言（求和口径 / 两档配额边界读数与判定必须一致 / 入库存规范化文本） | **判据扩面**（P1-b）；其中 `carrier_contract` 的「两档读数一致」是本轮新增的**跨档一致性**判据 |
| 9 | 颗粒度文档 §2.2 约束 29 的末句 | 原「sqlite 侧存量 `content` 列是原样存的用户输入，与重建口径天然一致」**废止**（该断言不成立：原样存储 ≠ 两侧求和作用于同一表示） | **文档断言废止**（不改任何测试判据；反例与依据见 `research.md` D-15） |

#### 1.3 续（二）：analyze C1 处置轮（追加行，不改上两表；2026-10-17 实施前阻断检查）

| # | 文件 | 变更 | 归属 |
| --- | --- | --- | --- |
| 10 | `internal/memory/import_test.go`（**新增文件 1 条**；tasks **T053**） | `TestDependencyDirection_MemoryImportsOnlyCoreWorkspaceStdlib`——以 `go list -f '{{join .Imports " "}}' github.com/ymedword/basalt/internal/memory` 读**生产导入面**，对每个导入项断言「标准库（路径首段不含点）**或**白名单（`internal/core` / `internal/workspace`）」（`exec.Command` 沿 `cli/ports_test.go:38-40` 同形）；`{{.Imports}}` 是**非测试**导入集 → `carrier_contract_test.go` 的 test-only `internal/storage` 引用**不受影响**；**逆改点**：生产文件加 `_ ".../internal/storage"` → 断言红 | **判据扩面**（新增文件 1 条；**不放宽任何既有判据**、不新增第三方坐标与配置键）——**analyze C1 处置**：FR-038 / SC-008 与口径 26 要求「记忆包生产 import 面」判据**必须**由包级架构测试承载，analyze 实测本版 42 条无一条是它（CI / Makefile / scripts 也无该判据）→ 用户 2026-10-17 裁决「**增 1 条架构测试**」；级联：42 → **43 条**、新增测试文件 6 → **7**、全量 27 → **28 个文件**、`tasks.md` 新增 **T053**、`quickstart.md` A3 / 场景 F8 同步 |
| 11 | `specs/006-memory/tasks.md` T037 的 DoD 记账串（**analyze I1，LOW**） | 末项「2」→「1」：原值是 `templates_test.go` 的**文件合计**，其中第 1 条归 T005 / Phase 2——本故事测试增量 = **19 条**（4 + 1(R7) + 6 + 3 + 3 + 1 + 1） | **记账口径修正**（只改 DoD 里的核对串与 `quickstart.md` A3 的读数串；**不动任何用例、断言与验收语义**，非判据面变更） |

**不受影响面**（修正后仍零变化）：第 1~5 项送审清单、`internal/tool/policy.go`、`go.mod` / `go.sum`、`basalt.yaml.example`、`schema.sql` 与迁移、审计面、`make check` 七目标与覆盖率阈值。

### 1.3 续（三）：T027 落地轮实测修正（2026-10-17，**追加行，不改上两表**）

| # | 文件 | 变更 | 归属 |
| --- | --- | --- | --- |
| 12 | 上表**第 1 行**（`internal/cli/config_test.go` 的 `mem0` 判据面变更） | **该「既有断言」在仓库中不存在**——`config_test.go` 全文零 `memory` / `mem0` 断言（460 行、19 个既有用例逐行核过），`grep -rn "mem0" internal/` 只命中 `cli/config.go:133` 的注释与 `memory/doc.go:11` 的说明句。实测（overlay 探针，仓库外、用毕已删）：缺省 `Memory` 段类型 `map[string]interface{}(nil)`；`backend: mem0` 与 `backend: sqlite` **都 err=nil**、取值原样进 map（`map[string]interface{}{...}`）——当前**零取值域校验、零缺省归一**，故不存在「原判据从可解析变拒绝启动」这回事 | **判据面变更 = 零**（T027 实为**纯新增** 3 条：`git diff --numstat` = `70 0`，`-` 行 **0**；既有用例逐行未动）。上表第 1 行按此口径读作「**修正记录**」而非「变更送审项」；`basalt.yaml.example` 与 `schema.sql` 的零改动面不受影响 |

### 1.3 续（四）：T036 装配落地轮实测登记（2026-10-17，**追加行，不改上三表**）

| # | 文件 | 变更 | 归属 |
| --- | --- | --- | --- |
| 13 | 颗粒度文档 §3.5 第 **3** 项（装配面改造点写的是 `internal/app/wire.go` **一个文件**） | 实测**另需 `internal/app/ports.go` 一处**：T035 让模板声明两件记忆工具后，**轻路径**（`profile list` / `profile show` 经 `agentsPort`，`run.go:86` 构造、无注册池）此前给装载器传 nil `Tools` = **空池** ⇒ 空池语义（US-4 已判卷）把每个声明工具的 Agent 判成**整只不注册**（逐字 `tool not registered: save_memory`），与重路径结论相反。落地：装配层新增 `declaredToolNames`（八名有序清单，`wire.go:336`）+ 重路径装配后 `slices.Equal(registry.Names(), declaredToolNames)` **fail-closed 校验**（`wire.go:242-252`）+ 端口注入该名集（`ports.go:103`） | **实现落点登记**（**交付物清单之外的第三处「文档 ↔ 仓库事实差异」**——前两处见本文件 §1.2 与 [plan.md](../006-memory/plan.md) 差异表）；**判据面零变更**：未动任何测试文件与断言，新增的是一处**校验**（扩面）。plan.md 差异表**暂仍两条**——该节属已批准设计输入，Mid-implement 追加会动其 sha256 基线并触发总则 2 第 7 项（输入变化 = 变更），故先登记于 T036 行与本表，**收口交用户裁决是否回填**；逆改探针双向读数（A 档重路径校验红 / B 档轻路径判定红）见 tasks.md T036 行 |

### 1.3 续（五）：第二轮复核修复轮追加文件与用例（2026-10-17，**追加行，不改上四表**）

| # | 文件 | 变更 | 归属 |
| --- | --- | --- | --- |
| 14 | `internal/memory/markdown_fault_test.go`（**新增文件 1 条，含 3 子用例**） | `TestMarkdownStore_ReplaceFailuresLeaveOriginalAndNoResidue`——临时文件写入 / 关闭 / rename 三类失败各一子用例；断言错误原样上抛（`errors.Is`）、原文件逐字节不变、零 `.tmp-` 残留；配套生产代码**未导出**注入点 `openTemp` / `rename` + `tempFile` 最小接口（生产恒为 nil，走真实 `os.Root`） | **判据扩面**（F2-4：补 FR-014 的失败路径判据；既有配额拒绝档测试原样保留，**未删任何断言**） |
| 15 | `internal/memory/markdown_test.go`（**新增用例 1 条**，8 → 9） | `TestMarkdownStore_AcceptsCRLFFromManualEdit`——CRLF 手工文件：`Load` 逐字节返回原文、`LoadEffective` 输出 LF 重建文本、`Append` 后重建为 LF 且条目完整（正文空行不被拆条） | **判据扩面**（F2-2；既有 LRU 换行用例与断言未动） |
| 16 | `internal/memory/memory_test.go`（**新增用例 1 条**，10 → 11） | `TestService_LoadEffective_SingleOversizedManualEntryWarns`——单条 4097 字节手工核心记忆完整注入且 WARN 点名单条上限与字节数、日志不打正文；恰 4096 边界不告警 | **判据扩面**（F2-3；总量 WARN 的既有用例未动） |

**合计**：新增测试文件 7 → **8**、测试函数 +3（其中故障注入为 1 函数 3 子用例）；生产代码改动仅 `internal/memory/markdown.go`（`splitLines` + 两个未导出注入点）与 `internal/memory/memory.go`（`layerNonEmpty` 共用 `splitLines` + 单条 WARN）。三件输入（颗粒度文档 / `spec.md` / `plan.md`）的 sha256 **基线不变**——本轮问题是实现与冻结规格（FR-014 / FR-021 / FR-025）的偏差，不构成输入变更。颗粒度文档 §2.3 伪代码只列总量 WARN、未列单条检查：以 `spec.md` FR-021 / FR-025 为准补齐，冻结文档不静默改写。

### 1.3 续（六）：复合失败路径补强（2026-10-17，**追加行，不改上五表**）

| # | 文件 | 变更 | 归属 |
| --- | --- | --- | --- |
| 17 | `internal/memory/markdown_fault_test.go`（**子用例 3 → 4**） | 新增「写入与关闭同时失败」子用例：`tempFailure` 必须在关闭失败后**仍尝试**删除临时文件，并把两个失败都写进错误文本；断言 `errors.Is` 判因（写入错误）、错误文本点名「临时文件关闭失败」、原文件逐字节不变、零 `.tmp-` 残留 | **判据扩面 + 实现修正**（F2-5：原实现关闭失败即提前返回，复合失败会留残留；测试函数总数不变，仅子用例 +1） |

### 1.4 三件输入的 sha256 与 tasks 指纹（**现行基线，2026-10-17 二次写入**）

> **修订历史（追加式保留旧值供审计；后一次写入不得覆盖前一次的记录）**：

| 轮次 | 触发 | 颗粒度文档 | spec.md | plan.md | tasks.md |
| --- | --- | --- | --- | --- | --- |
| 首版批准（R3 裁决轮） | 步骤 5 停点确认 | `62671f467e9f736e805b811e05392a9aeff57e85bfaceaab080a228cd52669e2`（969 行） | `ac944bc42fec72a946170451d1caaeb40467428877dcbbd83e40779100a2cee8` | `39e775757e267262001325dc7484a0884f60077e3bb69cb47f9fd16cf89aada2` | `cfe2df7cf8b7bb18593ed9161b6d1b3bf17023321ecbef8c911b852676011715`（238 行） |
| **现行（C1 处置轮）** | analyze 阻断检查 → 用户裁决增 1 条包级架构测试 → 六文件级联修订 | `d774ab95e5a3b2e8de91cf120d308f0f452eb9b6f08634ac7725c433c2788e4d`（979 行） | `1d6a55fb748ac8019e05f2a61aed814b99ed75ae36ec4b1236e6de8f0c67ef10`（272 行） | `484cc6fb5695f0c1aa338f49f1d3943017081eff3ada7f682c2c282345112c2b`（171 行） | `b7fb1b0de89ba8afe953251395a4a2c125b13e5cd9eff3b704588ce2e063e133`（242 行，新增 T053） |
| 实施轮（T007~T023 勾选与逐任务读数登记） | implement 逐任务执行（T020 / T021 / T022 / T023 本批完成；**判据面零变更**） | 未变 | 未变 | 未变 | `1e0553f94f41fc31ec027850b9bb1d9ba87bfbfa9cedfd2b2c5323bc78db3e56`（251 行，勾选 23 / 未勾选 30——勾选与读数登记属 implement 的预期改动，**三件输入**仍以批准版为基线） |

| 输入 | 路径 | sha256（现行） | 写入时点 |
| --- | --- | --- | --- |
| 颗粒度文档 | `docs/us/us6-memory.md` | `d774ab95e5a3b2e8de91cf120d308f0f452eb9b6f08634ac7725c433c2788e4d` | 2026-10-17（C1 处置级联修订后） |
| 规格 | `specs/006-memory/spec.md` | `1d6a55fb748ac8019e05f2a61aed814b99ed75ae36ec4b1236e6de8f0c67ef10` | 同上 |
| 计划 | `specs/006-memory/plan.md` | `484cc6fb5695f0c1aa338f49f1d3943017081eff3ada7f682c2c282345112c2b` | 同上 |
| **tasks 指纹** | `specs/006-memory/tasks.md` | `b7fb1b0de89ba8afe953251395a4a2c125b13e5cd9eff3b704588ce2e063e133` | 同上（**现行版**，242 行） |

**读数**：颗粒度文档 979 行、`spec.md` 272 行、`plan.md` 171 行、`tasks.md` 242 行、`quickstart.md` 148 行（`wc -l`，2026-10-17 实测）。**辅助产物**（非三件输入，供续跑交叉核对）：`research.md` = `0a94dd239904928a66d83f56a8768cd036050c55496c95522dc024fd7a240e63`（本轮未变）、`data-model.md` = `e67d132c00c1fd18c321ad97ee9b12ffdfba775e47b6293895fd0337095e914d`（本轮未变）、`quickstart.md` = `5be2c2c3345159ceced62f3165d006ab43347ec879e99459dc63a0dc93a3e2f6`（**C1 处置轮已刷新**——A3 文件清单与条数串、场景 F8；原 `4a738c8b25da4efd2bcba0d061eb62a86ead2940fd98600d5f1dfca9573b0b85`）。

**批准记录**（步骤 5 固定停点）：

| 项 | 记录 |
| --- | --- |
| 送审 | 2026-10-17：tasks 首版 + 三件输入 + 两处「文档 ↔ 仓库事实差异」+ 五项判据面变更（§1.3 表）一并送审 |
| 裁决 | ① 确认两处事实差异与五项判据面变更；② tasks 需修正三处——P1-a 三阶段口径（测试准备 / 实现 / 验证，**不得**把「准备好测试」记成「测试已通过」）、P1-b 两档规范化与配额计量统一（`TrimSpace` 六面同表示）、P2-b per-scope 锁按**完整 `Scope`** 分档——外加两处顺手修正（Path Conventions 收窄到实际新增文件、T052 改用 `git status --porcelain`）；**修正后即作为批准版执行** |
| 范围声明（用户逐字） | 「本次确认的范围是：按仓库实测新建 `templates_test.go`、同步名集与行为两处承载点，以及报告列出的五项判据变更。**无需重新询问这些事项**。」 |
| 落案 | 修正已按 §1.3 续逐条落地（`research.md` 的 D-07 改判 + D-15 新增 → 颗粒度文档 P1-b/P2-b 行与陈旧计数 → `plan.md` / `quickstart.md` / `tasks.md` 全量同步 → 本文件 §1.2）；用户裁决时明记「本轮未修改文件或写入批准记录」——本批准记录于修正落地后补写 |
| **C1 处置裁决** | 2026-10-17（实施前阻断检查）：`/speckit-analyze` 报 **C1（HIGH）**——FR-038 / SC-008 与口径 26 要求「记忆包生产 import 面」判据**必须**由包级架构测试承载，而本版 42 条无一条是它（CI / Makefile / `scripts/` 亦无该判据）。用户裁决「**增 1 条架构测试（推荐）**」：沿 `internal/cli/ports_test.go` 先例在 `internal/memory/` 增包级导入面测试（`.../internal/memory` 生产导入面 ⊆ 标准库 ∪ {`core`, `workspace`}；`{{.Imports}}` 为非测试导入集 → test-only 引 `storage` 不受影响）。级联修订六文件：颗粒度文档（§3.2 表头与行 / §4.2 / 口径 26 / 评审回合裁决落案表续）、`spec.md`（计数句 + 锚点表行 + Clarifications 修订记录 ⑤）、`plan.md`（Testing / Constraints / 架构约束 / 必查表 / Structure Decision）、`tasks.md`（**新增 T053** + 计数 42→43、文件 27→28 + T023 指针 + **analyze I1 修正** + Notes）、`quickstart.md`（A3 + 场景 F8）、本文件（§1.2 / §1.3 续（二）/ §1.4）。**纯扩面**：不放宽任何既有判据、不新增第三方坐标与配置键。修订后**重跑 `/speckit-analyze`**——CRITICAL 清零方可进 implement |
| 用途 | 上表（**现行基线四行**）是 `/module-dev` 续跑「输入一致性核对」的**基线**：任何一行与实测不符 → 按总则 2 软门禁停车（列差异 → 追加式修订），**不得**静默继续 |

## 2. 失败台账（只追加）

| 日期 | 问题（测试名或首错位置 + 症状短语） | 假设 | 结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-10-17 | T015 逆改探针（R1）：`TestAgentService_InjectsCoreMemorySegment` 注释称「Build 加固定前后缀 → 第三组断言红」，实测**不红**（overlay 加前缀后仍绿） | 假设一：逆改位置没生效（overlay 未命中） | 不成立——同 overlay 下 `NilMemoryLeavesSegmentTwoEmpty` 逐字红（`service_test.go:546: actual "[逆改固定前缀]段一：你是 demo。"`），证明 overlay 已命中；真因是 R1 三组用**相对基线**（nil 端口组现场读数），加前缀令基线与实验组同步平移 | 1（已收敛，无需 `/debug`） |
| 2026-10-17 | T015 逆改探针（R2）：overlay 三文件后首跑 `build failed`——`internal\core\prompt_test.go:378:6: env.memory undefined` | 假设一：`RunEnv.memory` 改名会断包内测试编译 | 成立——`prompt_test.go` 是 `package core` 内测、直接引用该非导出字段；探针改为「保留原字段 + **新增** `memoryReader` 注入位」（只破坏行为、不动字段名），编译通过、R2 红态取到（`expected: 1 actual: 2`） | 1（探针手法问题，非实现缺陷） |
| 2026-10-17 | T023 验证轮：`coreSectionOf` 注入文本**不渲染 `-` 前缀**——与 §4.3 R4a 的**全文相等**期望（`"## 核心记忆\n\n"+strings.Join(entries, "\n\n")`，entries 带 `-` 前缀）不符；已落地 13 条全用 `Contains` / `Empty`，判不出（`"- - x"` 也含 `"- x"`） | 假设一：注入面读的是**重建结果**（约束 28 / 口径 22），`parseSections` 切正文、注入面须按文档化形态渲染回来 | 成立——overlay 探针缩样（`zz_probe_r4a_test.go`，仓库外、用毕即删）对当前实现逐字红：`expected: "## 核心记忆\n\n- 很长的一条偏好…"` vs `actual: "## 核心记忆\n\n很长的一条偏好…"`；修 `coreSectionOf`（改走同一 `renderEntry`）→ 探针转绿、`-race` 全包绿（`ok .../memory 2.606s` / `ok .../storage 8.010s`） | 1（当场收敛；修实现在 T020 落地范围内，判据面**零变更**） |
| 2026-10-17 | T026 逆改探针：首版「sqlite 档按**未规范化**长度判单条配额」变异**不红**——`TestCarriers_QuotaBoundaryExactLimit` 首尾空白档仍绿 | 假设一：变异没生效（overlay 未命中依赖包）；假设二：常量口径记错（我按 `4096` 当单条上限、实际判据应更小） | 假设一不成立——诊断探针（`E` 把 `Append` 改名 → `build failed`；`F` 恒拒 `len==4096` → 逐字红）证明 overlay 已命中 `internal/storage`；假设二不成立但**指向真因**——实测 `MaxCoreEntryBytes = 4096`（`core/memory.go:58`），故 `len(content)=4096` 既不触单条上限、也与 `len(c)=4094` 在总量上同量级：**该变异与正解等价**，是探针本身不具判别力。**修法**：改用「整条去掉 `TrimSpace`」的变异（量 `content` + 存 `content`）→ 两档分叉、逐字红；**同批加固判据**（配额边界②追加读回正文合计 = 32752） | 1（当场收敛；实现无误，属探针设计缺陷） |
| 2026-10-17 | T037 逆改探针：三条探针**首跑全部** `[setup failed]`——`open D:\c\Users\76562\AppData\Local\Temp\basalt-t037-probe\policy_probe.go: The system cannot find the path specified.` | 假设一：探针文件没写出来 | 不成立——文件在（`wc -l` 三条都读得到）、`grep` 核对变异也确实落在探针副本里；真因是 **overlay JSON 里的路径写成 MSYS 形态 `/c/Users/...`**：`go` 按 Windows 语义把 `/c/...` 当**相对**路径、拼到当前盘（`D:\`）下 ⇒ `D:\c\Users\...`。**修法**：overlay 路径一律写 `C:/Users/...` 形态（探针 A/B/C 三条随之取到逐字红态，见 tasks.md T037 行） | 1（当场收敛；探针包装问题，非实现缺陷。**可复用口径**：`-overlay` 的键与值都必须是**盘符形态的绝对路径**） |
| 2026-10-17 | T038 首跑 `TestService_RecallByKeyword_WholeStringCaseSensitiveArchiveOnly` 红：`expected: []string{"部署流程见 RUNBOOK", …}` / `actual: []string{"- 部署流程见 RUNBOOK", …}` | 假设一：召回丢了前缀（实现把条目正文与载体装饰混在一起了） | 不成立——真因在**用例入参**：首版把 `"- 部署流程见 RUNBOOK"`（自带 `"- "`）当 content 写入，而 `normalizeEntry` **有意只做 `TrimSpace`**、不剥 `"- "`（那是用户内容，不是载体添加物，约束 29），故载体重建后正文原样带前缀、召回也原样取回。**修法**：入参改为**条目正文**（与 `carrier_contract_test.go` 的 R5 / 两态用例同形），断言语义一字未变；改后 `--- PASS … (0.03s)`（判据形态自纠，非实现缺陷） | 1（当场收敛） |
| 2026-10-17 | T039 首跑 `TestMarkdownStore_RecallMatchesWholeStringWithinEntries` 红：`expected: []string{"结论：SQLite 方案\n评估已完成"}` / `actual: []string(nil)`（`markdown_test.go:196`） | 假设一：召回没做整串匹配（实现按分词拼词命中） | 不成立——真因在**用例入参**：首版把连续短语写成了**跨换行**的形态（首行 `结论：SQLite 方案`、续行 `评估已完成`），而查询串 `SQLite 方案 评估` 因此**不是**任何一条正文的连续子串（实现把续行按 `"\n"` 拼回整条正文，换行也是正文字符，约束 28 / 口径 12）。**修法**：第一条正文改为**首行含连续短语** `结论：SQLite 方案 评估` + 续行 `已完成，细节另见附录`，断言语义一字未变；改后 `--- PASS … (0.04s)`（判据形态自纠，非实现缺陷） | 1（当场收敛） |
| 2026-10-17 | T040 逆改探针 B（配额错误原样上抛）：变异后测试**不红**——读数 `ok github.com/ymedword/basalt/internal/tool 1.124s` | 假设一：用例不具判别力（`IsInvalidInput` 断言漏判）；假设二：变异没落到探针副本 | **假设二成立**：perl 模式把内层 `if errors.As` 的缩进写成 **3 个制表符**（源码实际为 **2 个**），替换未命中 ⇒ 探针副本与原文件逐字节相同，绿是**假绿**。**修法**：按 2 个制表符重做，并**先取证变异落地**（`grep -n 'InvalidInputError'` 不再命中该映射行 + `sed -n '96,104p'` 显示该分支已是 `return core.Result{}, err`）再取红态——两子测试逐字红（`memorytools_test.go:294: Should be true` + `Messages: 归类 = invalid_input（约束 16 / 口径 18）…`）。**可复用口径**：变异探针取红态前必须先证明变异在副本里生效（grep / `diff`），否则「绿」无法区分「用例没判别力」与「变异没生效」 | 1（当场收敛；探针包装问题，非实现 / 用例缺陷） |
| 2026-10-17 | 收口独立复核 F-01（MEDIUM）：`internal/storage/memory_store.go:63` 的 `created_at` 用 `.Unix()` 写**秒**，违反契约 `docs/contracts/schema.sql` 迁移纪律 4「时间统一存 INTEGER（Unix 毫秒，UTC）」；同库 `run_store.go:68` / `session_store.go:126` / `llm_calls.go:91` / `tool_invocations.go:66` 均为 `.UnixMilli()`；`memory_store_test.go:59` 把秒固化进断言 | 假设一：契约的毫秒纪律只约束审计表、不约束记忆表 | 不成立——迁移纪律第 4 条是**表级通则**（「时间统一存」），`memory_entries.created_at` 同为 INTEGER 时间列、无豁免条款。用户 2026-10-17 裁决「改毫秒 + 同步断言」。**修法**：`.UnixMilli()` + 断言同步（**判据面变更 1 条**：期望值由秒改毫秒，验收语义「`created_at` 来自注入时钟」不变）→ `TestMemoryStore_AppendAndLoadRoundTrip` PASS、`go test -race ./internal/storage/ ./internal/core/` 双包 ok、`make check` exit 0（2026-10-17 复跑） | 1（当场收敛；用户裁决记录见 §5） |
| 2026-10-17 | 收口独立复核 F-02（LOW）：`internal/core/run.go:154` 注释「段 1（+ 段 2 恒空）」与 `prompt.go:95-100` 的段 2 注入矛盾 | 假设一：注释是 US-4 遗留、未随 US-6 更新 | 成立——纯注释，无行为影响；修为「段 1 + 段 2（记忆核心区；空记忆时与段 1 逐字节相同）」；`gofumpt` 干净、`make check` exit 0 | 1（当场收敛） |
| 2026-10-17 | 收口独立复核 F-03（LOW，信息性）：`internal/memory/markdown.go:89-99` 对非 CORE 的任意 `section` 值 fail-open 落归档；颗粒度文档 §2.3 伪代码为 `appendIf(section == core.SectionArchive, ...)`（域外值不落任何分区）；sqlite 档对域外值原样入库，两档在域外输入上分歧 | 假设一：这是可达的生产缺陷 | 不成立——生产唯一调用方为工具层（`scope` 仅 `CORE` / `ARCHIVAL`，非法值在工具层即拒），载体收到的域外值不可达；契约场景表不含域外输入。**处置**：不改代码（不扩大已批准范围），记录为已知边界；若未来新增管理面调用方，须在门禁层补显式拒绝 | 0（信息性，无修复尝试） |
| 2026-10-17 | 第二轮复核 F2-1（P1，验收结论）：首轮收口报告写「本 US 可收口」，与编程指南 §5 第 6 项「验收 Demo 实跑通过」及四态口径「必验项处于未验证 → 该项验收保持未完成」不符——真模型 Demo、U-1 / U-2 人工项均未执行 | 假设一：自动化层全绿即可宣布 US 完成 | 不成立——完成定义要求 Demo 实跑；必验项未验证时验收保持未完成。**处置**：§5 结论改判为「**实现与已执行自动化验收完成，整体验收待完成**」；U-1 / U-2 保持未验证，**不**翻转 | 0（口径纠正，无代码修复） |
| 2026-10-17 | 第二轮复核 F2-2（P2，CRLF）：Windows 记事本手工保存的 CRLF 记忆文件被 `parseSections` 判成「结构损坏——缺少分区标题：## 核心记忆」并拒绝启动——按 `\n` 切行后直接与标题比较，行尾 `\r` 未去（`markdown.go:196`） | 假设一：CRLF 文件不可能来自支持的用户路径 | 不成立——Windows 手工编辑是文档化用法（Demo 二场景 ③），记事本默认 CRLF；且正文空行在 CRLF 下会变成 `"\r"`，还会被误当零缩进行而把一条拆成两条。**修法**：新增 `splitLines`（按 `\n` 切、去行尾 `\r`），解析面与 `layerNonEmpty` 共用；`Load` 仍逐字节返回原文（CRLF 保留），重建产物用文档化 LF。**红态**：新用例逐字 `memory: memory/MEMORY.md: memory: 结构损坏——缺少分区标题：## 核心记忆`；修复后 PASS（含正文空行不被拆条、CRLF 文件追加后重建为 LF） | 1（当场收敛） |
| 2026-10-17 | 第二轮复核 F2-3（P2，注入期单条超配额漏 WARN）：`coreSectionOf` 只判核心区总量 `> 32768`；手工加入一条 4097 字节的核心记忆时完整注入但日志零 WARN（`memory.go:185`） | 假设一：FR-025 的「超配额」只指总量上限 | 不成立——FR-021 定义两条写入配额（单条 4096 / 总量 32768），FR-025 对「手工编辑导致的超配额」未限定只总量。**修法**：注入期逐条检查单条上限并记 WARN（字段 path / entry / bytes / limit，**不打正文**），总量检查保留；恰 4096 字节不告警。**红态**：`"" does not contain "WARN"`；修复后 PASS | 1（当场收敛） |
| 2026-10-17 | 第二轮复核 F2-4（P2，FR-014 判据不完整）：`TestMarkdownStore_AppendReplacesAtomicallyWithoutResidue` 的失败态是配额拒绝（发生在临时文件创建之前），无法证明写入 / 关闭 / rename 失败后的清理与原文件不变；测试注释已自认该边界（`markdown_test.go:234-236`） | 假设一：真实文件系统可在跨平台确定性制造三类失败 | 不成立——Windows 访问位与 POSIX 不同形。**修法**：生产代码加两个**未导出**注入点（`openTemp` / `rename`，生产恒为 nil），同包白盒测试注入三类失败；每条断言错误原样上抛、原文件逐字节不变、零临时文件残留。**判别力探针**：仓库外 overlay 删除 `root.Remove(tmp)` → 三条子用例逐字红（残留路径指名 `.tmp-...`），用毕即删。修复后 `-race` 全包绿 | 1（当场收敛） |
| 2026-10-17 | 第二轮复核补强 F2-5（FR-014 复合失败路径）：`tempFailure` 在「关闭临时文件失败」时**提前返回**，不再尝试 `root.Remove`——写入与关闭同时失败时会留下 `.tmp-` 残留，与 FR-014「替换失败零临时文件残留」不符 | 假设一：关闭失败后删除必然也失败、无需尝试 | 不成立——应当**总是尽力清理**并把两个错误都写进错误文本；删除是否成功由返回值如实报告，不能因未尝试而制造残留。**修法**：`tempFailure` 改为「关闭（可选）→ **无条件尝试删除** → 合并错误文本」，cause 仍是 `errors.Is` 判据；故障注入新增第 4 子用例「写入与关闭同时失败」，断言残留为零且错误文本点名关闭失败。**判别力探针**：overlay 还原旧「提前返回」实现 → 该子用例逐字红（`Should be empty, but was [...MEMORY.md.tmp-15bbfece15efc291]`）；恢复后 PASS。 | 1（当场收敛） |

## 3. 停止预算与计数

| 预算项 | 阈值 | 当前计数 |
| --- | --- | --- |
| 同一问题修复次数 | 3 次不收敛 → 停下走 `/debug` | 0 |
| 同一问题 debug 内假设不成立数 | 3 个 → 停下讨论是否设计 / 架构问题 | 0 |
| 本 US `/debug` 轮次 | ≤ 2 | 0 |
| 无进展判据 | 相邻两次尝试无新读数 / 新假设 / 新定位 → 立即停，不等计数 | 未触发 |
| 预算到期处置 | **停下报告请裁**（继续 / 降档 / 换方案）——不得草率收尾、不得自动放弃 | — |

## 4. 未验证清单

> 与 `docs/AiProgrammingGuide.md` §5 第 6 项、陷阱表第 55 条是**同一概念**，本文件是其**持久载体**（其余处一律引用，不各写一份）。条目格式：`编号 | 项 | 状态 | 依据 / 证据`。唯一允许原地翻转的段。

| # | 项 | 状态 | 依据 / 证据 |
| --- | --- | --- | --- |
| U-1 | quickstart 场景 E 人工项 E1~E8（Demo 二 markdown/sqlite 两档 / 手工编辑生效时机 / 超配额 WARN 与拒绝启动 / 两层作用域现场 / `basalt tool list` 八件 / 三平台编译矩阵 / 损坏文件现场） | **未验证**（harness 之外，执行人 = 用户） | [quickstart.md](./quickstart.md) §场景 E；机器侧等价判据已覆盖（R1~R9 / T051 门禁全绿），但真模型与真控制台读数不在自动化层 |
| U-2 | spec.md「人工验收项」13 条（含 Demo 二两条都验、`tool list` 八件冒烟、四目标编译矩阵、归档区体量边界读数） | **未验证**（执行人 = 用户） | [spec.md](./spec.md) 同名节；T051 已给出七目标四态，剩余为人工面 |
| U-3 | analyze 收口轮 LOW 发现 I1（spec.md:11 计数句「新增 6 文件 + 改动 7 文件」为 C1 前旧值）与 I2（spec.md:199「含一条判据面变更」已被 T027 实测取代） | **未验证 / 待裁决** | 不改冻结输入（改 spec.md 即触发总则 2 第 7 项）；两条均不影响任何 FR/SC 判据与已执行读数；收口请用户裁决：保留原样 / 刷新基线并一行更正 |
| U-4 | plan.md「文档 ↔ 仓库事实差异」第三处（`internal/app/ports.go` 轻路径名集注入）是否回填 | **未验证 / 待裁决** | run-state §1.3 续（四）第 13 行；实现已落地并经逆改探针双向取证（T036 行），未回填原因 = 冻结基线 |

## 5. 收口结论

**写入时点**：步骤 7（收口三件套 + 七项证据 DoD）。四态汇总的指针，细节进验收报告。

| 项 | 值 |
| --- | --- |
| `/speckit-analyze` 复跑 | **完成**（2026-10-17 收口轮，只读）：CRITICAL **0** / HIGH **0** / MEDIUM **0** / LOW **2**（I1 spec.md:11 C1 前旧计数、I2 spec.md:199 已被 T027 实测取代的「判据面变更」括注——均不改任何 FR/SC 判据，登记于 §4 U-3，待用户裁决是否刷新基线更正）；FR 40 条 / SC 8 条覆盖 **100%**；已知项（五项判据面变更 / R3 三处修正 / 两处事实差异 + 第三处已登记）未重复计 |
| 独立复核 | **完成**（2026-10-17，独立上下文子代理；输入 = 冻结规格 + 判据面 diff，不读作者叙述）：12 项审查面全部通过；findings **F-01 MEDIUM**（`created_at` Unix 秒 vs 契约毫秒）/ **F-02 LOW**（`run.go:154` 陈旧注释）/ **F-03 LOW 信息性**（markdown 载体域外 `section` fail-open，生产不可达）；处置：F-01 经用户裁决修复（`.UnixMilli()` + 断言同步，判据面变更 1 条）、F-02 已修、F-03 记录为已知边界不改代码；修复后 `make check` exit 0。总结论：**通过**（修复后条件满足） |
| 第二轮复核（2026-10-17，用户提交 findings） | **完成处置**：**F2-1（P1）** 验收结论改判——自动化绿不等于 US 完成，Demo 实跑与必验人工项未验证时整体验收保持未完成；**F2-2（P2）** CRLF 记忆文件误判损坏——`splitLines` 兼容结构行换行，`Load` 原文不变，红→绿；**F2-3（P2）** 手工单条超配额漏 WARN——注入期补单条 4096 检查（不打正文），红→绿；**F2-4（P2）** FR-014 判据不完整——新增三类原子替换失败故障注入（`markdown_fault_test.go`），判别力探针红态取证。四条均落 §2 台账；自动化复验读数见本轮收口报告。**判据面：纯扩面**（新增用例与未导出测试注入点；既有断言未删、未放宽） |
| 第二轮复核补强（2026-10-17，F2-5） | **完成**：`tempFailure` 在关闭失败时提前返回、复合失败会留 `.tmp-` 残留（FR-014）——改为关闭可选、**无条件尝试删除**、两错并入错误文本；故障注入扩至 **4 子用例**（新增「写入与关闭同时失败」），overlay 还原旧实现时逐字红、修复后绿。见 §1.3 续（六）与 §2 F2-5 |
| 七项证据 DoD | **完成**（证据 1 门禁七目标 / 2 harness 逐条 / 3 交付物 / 4 前序回归 / 5 全局不变量 / 6 剩余人工项 U-1·U-2 / 7 变更总结四段——读数见收口报告与 tasks.md 各任务「2026-10-17 读数」）；第二轮复核后复跑 `make check` **exit 0**（fmt / lint / sec / test / test-integration / cover / build 全绿；`internal/memory 91.91%`（F2-5 新增的「删除也失败」两个分支未覆盖，仍 ≥ 70%），core 85.35% / storage 79.54% / tool 85.25% 均 ≥ 70%；`scripts` 包另以 `-count=1` 强制复跑 exit 0，**未复现**评分报告所称 Win32 error 5） |
| 结论 | **实现与已执行自动化验收完成，整体验收待完成**——按编程指南 §5 第 6 项与四态口径，必验项（真模型 Demo 二两档、U-1 quickstart E1~E8、U-2 spec.md 人工验收 13 条）处于**未验证**，该项验收保持未完成，**不得**宣布本 US 已收口；U-3 / U-4 保留冻结输入原样、作为已知 LOW / 文档差异登记（如需刷新基线须用户另行确认） |

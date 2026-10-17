# run-state — US-7 MCP 集成（feature 007-mcp）

> **性质声明（三声明，本文件常驻头部）**
>
> 1. 本文件是**过程状态，非设计工件**——speckit 命令**不得**把它当设计输入；
> 2. 失败台账**只追加**：一行一次尝试，不改旧行（可 diff 审计）；
> 3. 唯一允许原地翻转的是「未验证清单」的「未验证 → 已验证」，翻转必须注日期与证据。
>
> 本文件**入库 git、收口不删**——它是长任务续跑与验收可信度的过程载体，**不是**探针 / 诊断临时物（「临时物收口即删」惯例**不适用**）。

## 1. 基线

**写入时点**：步骤 5 固定停点（用户批准 tasks 后，2026-10-17）。

| 项 | 值 |
| --- | --- |
| US | US-7（MCP 集成）——台账 `docs/us/REGISTRY.md` 类型列：开发类 → 走完整流程步骤 1~7 |
| feature 目录 | `specs/007-mcp/`（glob `specs/007-*/` 单命中） |
| 分支 | `007-mcp`（步骤 1.3 手动建；仓库未登记 `before_specify` hook） |
| 起始 commit SHA | `8d9bdfb8727933cc10f27ef6cad62e180648dd7d`（短：`8d9bdfb`，`feat: deliver US-6 long-term memory with scoped fallback`） |
| worktree | **无**——单工作区 `D:/code/basalt`，无并行 US 交错 |

### 1.1 开跑时已有未提交改动（**保留，不得 stash / checkout 覆盖**）

| # | 路径 | 状态 | 归属 |
| --- | --- | --- | --- |
| 1 | `docs/us/REGISTRY.md` | M | US-7 登记（`/us-register 7`） |
| 2 | `.markdownlint.json` | ?? | 仓库根 markdownlint 配置（`{"MD013": false}`，供有 Node 的环境 / CI 使用） |
| 3 | `docs/us/us7-mcp.md` | ?? | US-7 颗粒度文档本体（未入库） |
| 4 | `specs/007-mcp/` | ?? | US-7 speckit 工件目录（spec / plan / research / data-model / quickstart / tasks / checklists） |

以上 4 项均为**本 US 开工前的既有改动**，非本运行产生；收口报告「变更总结」需按 `git status --short` 实测区分本运行改动与这 4 项。

### 1.2 本 US 允许触碰的文件范围（颗粒度文档 §3 交付物清单）

#### 代码（新增 6 + 改动 6，共 12 文件）

| 类 | 路径 |
| --- | --- |
| 新增 | `internal/core/mcp.go`、`internal/workspace/mcp.go`、`internal/tool/mcp.go`、`internal/tool/mcptools.go`、`internal/app/mcp.go`、`internal/testutil/mcpserver.go` |
| 改动 | `internal/workspace/agent.go`、`internal/workspace/templates.go`、`internal/core/profile.go`、`internal/cli/profile.go`、`internal/tool/runner.go`、`internal/app/wire.go` |

#### 测试（新增 6 文件 51 条 + 改动 2 文件追加 6 条 + 夹具改造 2 点）

| 类 | 路径（条数） |
| --- | --- |
| 新增（6 文件 51 条 = 默认层 46 + 集成层 5） | `internal/workspace/mcp_test.go`（18，`package workspace_test`）、`internal/tool/mcp_test.go`（18，`package tool`）、`internal/tool/mcp_integration_test.go`（5，`package tool_test`，`//go:build integration`）、`internal/app/wire_mcp_test.go`（7，`package app`）、`internal/app/mcp_test.go`（1，`package app`）、`internal/testutil/mcpserver_test.go`（2，`package testutil`） |
| 改动（追加，2 文件 6 条） | `internal/workspace/agent_test.go`（4）、`internal/cli/profile_test.go`（2） |
| 夹具改造 2 点 | `workspace/agent_test.go` 的 `writeMCPServers` 改为 `servers:` 列表形态；同文件按新形态补夹具（详见颗粒度文档 §3.2） |

#### 配置 / 文档 / 依赖

| 类 | 路径 | 说明 |
| --- | --- | --- |
| 依赖 | `go.mod` | **只新增**一条直接坐标 `github.com/modelcontextprotocol/go-sdk v1.8.0`（技术方案 §1.2 栈表已批准） |
| 文档 | `docs/Compatibility.md` §3 | 增补 3 行有意差异（非 stdio 拒启动 / 非文本类型化占位 / 子进程环境白名单）——§3.5 改造点 ⑨ |

**零改动核对面（列出为核对任务，非改动任务）**：`basalt.yaml.example`（配置键零新增）、`docs/contracts/schema.sql` 与迁移（零迁移）、`Makefile`、`.github/`、`scripts/`、审计面（`internal/storage` 审计路径 + `internal/core` 的 `LLMCallStore` 声明）、`docs/contracts/` 正文。

**范围外**：四份权威文档正文（`Compatibility.md` §3 三行增补除外）、`.specify/memory/constitution.md`、`docs/us/*` 其他 US 文档、`specs/006-*/` 及更早 feature 目录。

### 1.3 已知判据面变更（开工前送审清单，来源：颗粒度文档 §3.5 前序改造点）

| # | 文件 | 变更 | 归属 |
| --- | --- | --- | --- |
| 1 | `internal/workspace/agent_test.go` | `writeMCPServers` 夹具由旧占位形态改为 `servers:` 列表形态（§3.5 改造点 ①，T013） | **判据面变更**（夹具形态变化，送审项） |
| 2 | `internal/workspace/agent_test.go` | `TestAgentLoader_ParsesAllFrontmatterFields` 语义从「配置存在」升级为「已配置**且已连接**」（§3.5 改造点 ③，T018） | **判据面变更**（验收语义升级，送审项） |
| 3 | `internal/cli/profile_test.go` | 新增 `unverified` 分支用例（第三态）；**零 MCP 配置的逐字节断言原样保留**（§3.5 改造点 ④，T005 + T021） | **判据面变更**（新增分支；既有零配置判据不动，送审项） |
| 4 | `internal/tool/runner.go` | `childEnv` 抽取为 `mergeChildEnv`（§3.5 改造点 ⑤，T006） | **等价重构**（零判据变化） |
| 5 | `docs/Compatibility.md` §3 | 增补 3 行有意差异（§3.5 改造点 ⑨，T035） | **文档同步**（非代码判据） |

以上 5 项在步骤 5 的固定停点随 tasks 一并送审并获确认；完整九项映射（①~⑨ 逐条对应任务）见 [tasks.md](./tasks.md) 的「§3.5 前序改造点映射」表。

### 1.4 三件输入的 sha256 与 tasks 指纹（**现行基线，2026-10-17 写入；R4 修订同日重算**）

| 输入 | 路径 | sha256（现行） | 行数 | 写入时点 |
| --- | --- | --- | --- | --- |
| 颗粒度文档 | `docs/us/us7-mcp.md` | `31416a0420ebef5e27c56e7923da5ad28df375123f16fca98455d1f607aa847a` | 1177 | 2026-10-17（R4 文档口径修订后重算） |
| 规格 | `specs/007-mcp/spec.md` | `b6c46c44f140b8c85c23186a923f45ae7711fc808947467f32e13c3b20a2bb3d` | 266 | 2026-10-17（R4：FR-022 口径修订后重算） |
| 计划 | `specs/007-mcp/plan.md` | `d2f90b9af16ed29b46719908297efca3cde115d6382f675d90851ce45c666a82` | 185 | 2026-10-17（R4 未变） |
| **tasks 指纹** | `specs/007-mcp/tasks.md` | `725d613bdeb82a5e72e1f9787ec6a176ce583190a104fd57c357756d09cb5430` | 214 | **批准版**指纹不变（38 条任务；勾选翻转属过程状态，不回写指纹） |

**辅助产物**（非三件输入，供续跑交叉核对）：`research.md` = `963deaabd07bd7473b4ab8e9ec790bfeb1d1790a7737d97d7a6ac698e1d90fa3`、`data-model.md` = `67ca9cbcdb39e80088f31a17b2d215e8bf217a4ba9349a83b4f7bb06eea154e6`（285 行；R4 修订 + MD038 等价修正后重算）、`quickstart.md` = `05d63301989acc0e40a711617992f30bbd681f334b8efb8fb7e5f5e71e11f063`（189 行），research / quickstart 未变。

**R4 修订记录（2026-10-17 用户裁决）**：统一 `env_passthrough` 口径为「只透传显式登记变量；示例配置登记 `PATH` / `HOME` / `LANG` / `TZ`；代码无内置默认白名单，省略或空列表时不透传父进程环境变量」后，`us7-mcp.md` / `spec.md` / `data-model.md` 变更，上表对应值原地重算。**旧值留档**：颗粒度文档 `bbe772ce8e16501c496293b3ea9bd2af8e6a1a9c760c8715c2aea3b2e210597d`（1155 行）、规格 `09f5a09ca5f9e2462e024b607b2c1527340f28b2e5a3749b699098f8a89ce969`（266 行）、data-model `01df69f06ab7ead0e82476c63b39bf8fe5de66990f6d46d645bd00bebd05d63e`。修订依据与裁决原文见 [us7-mcp.md](../../docs/us/us7-mcp.md)「收口补登」R4 段；**实现与配置键零改动**。

**批准记录**（步骤 5 固定停点，2026-10-17）：

| 项 | 记录 |
| --- | --- |
| 送审 | 38 条任务 + 三件输入 + §1.3 五项判据 / 重构 / 文档同步项 + 三个已裁决选项（① transport 非 stdio = 拒绝启动；② 轻路径第三态 `unverified`；③ MCP 子进程环境复用 `shell.env_passthrough`）一并送审 |
| 裁决 | 覆盖映射齐、38 条任务规模合理；要求修正 **3 处 P2** + **2 处文字同步**后作为批准版执行，**保留 38 条任务及三个已决选项** |
| P2-1（R10a 判别力） | R10a 样例原只断言两次 `Close()` 无错，不确认连接数、不观察会话关闭；SDK 自身关闭幂等，原逆改点不成立。**修正**：样例重写——`Connect` 后断言 `len(conns)==1` 与池内 `sessions` 非空；`Close` 后在 5s 限定内观察 `session.Wait()` 返回；逆改点改为「Close 完全不关闭会话 → Wait 超时红」。处理记 [us7-mcp.md](../../docs/us/us7-mcp.md) 评审台账 P2-7，任务侧同步到 T029 / T030 / T034 |
| P2-2（T034 漏包） | 「数据库先关 → R10 红」的测试位于 `./internal/app`，原 T034 只要求复跑 `./internal/tool`。**修正**：T034 增 `./internal/app` 单跑 `TestWire_CloseClosesMCPBeforeDatabase`、R10 逆改拆 a/b 两条、要求记录该测试实际失败的逐字读数 |
| P2-3（T038 命令无效） | `go list ./internal/core` 只输出包路径、不显示依赖。**修正**：改为 T004 同款 `go list -f '{{.Imports}}' ./internal/core`，核对输出均为标准库 |
| 文字同步 1（T037） | 覆盖率口径补全：`core` / `tool` / `memory` / `storage` ≥ 70%，其余 ≥ 50%（原文漏 `memory` / `storage`） |
| 文字同步 2（测试计数） | 统一为「默认层新增 46 条 + 集成层新增 5 条 = 51 条，另追加 6 条」——避免把集成层 5 条重复计算 |
| 范围声明（用户逐字） | 「这些修正可以保留 38 条任务及三个已决选项。修正后再写批准记录和冻结哈希即可。」 |
| 落案 | 上述修正全部落地（`tasks.md` T029 / T034 / T037 / T038 + 头部 Tests 段与记账口径；`us7-mcp.md` §4.3 R10a 样例与评审台账 P2-7）；修正后重算三件输入与 tasks 的 sha256 → 上表新值即批准版基线 |
| 用途 | 上表是 `/module-dev` 续跑「输入一致性核对」的**基线**：任何一行与实测不符 → 按总则 2 软门禁停车（列差异 → 追加式修订），**不得**静默继续 |

## 2. 失败台账（只追加）

| 日期 | 问题（测试名或首错位置 + 症状短语） | 假设 | 结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-10-17 | T037 `make check` → `sec` 目标失败：gosec G204 `internal/tool/mcp.go:193`（配置驱动 argv 的 `exec.Command`，`Issues : 1, Nosec : 3`） | 按 H2「修实现不改判据」评估：G204 只接受常量实参，配置驱动 argv 静态不可见；改走 `os.StartProcess` / 手工拼 `*exec.Cmd` 等于掩盖审计点、偏离颗粒度文档 §2.3，均更差（陷阱 #56） | 软门禁停车请裁；用户 2026-10-17 裁决 = 加一行定向 `#nosec G204`（三点理由逐字落地，同步陷阱 #47 豁免清单 4 行 → 5 行）→ 重跑 `make check` exit 0（七目标全绿，读数见 T037） | 1 |
| 2026-10-17 | 收口复核：用户复跑五个包 `-race` 与五条 stdio 集成（均通过）后，以额外探针报 2 处行为问题 + 1 处验收缺口（P2×3）与 3 条建议——① 最后一个 server 握手期间整体取消被吞成 nil（断言 `context.Canceled`，实得 nil）；② 非字符串 `transport` 强转空串后按缺省 stdio 放行（`1` / `true` / `[]` / `{}` 全部解析成功）；③ SC-001 描述 / schema 保真无有效断言（overlay 清空描述 + 空 schema 后默认层仍全绿） | 按 H2「修实现不改判据」：① 三个失败分支与返回前复核父 `ctx.Err()`，预算超时仍跳过；② 解析器先类型检查再查取值（`null` 视同缺省）；③ 新增全链路保真断言并以 overlay 逆改证明判别力；另补 FR-011 回归与集成直调期限 | 全部修复并复验：新增 4 个测试函数（workspace +1 / tool +2 / app +1），红态逐字留住（`An error is expected but got nil` / `Expected error with "context canceled" in chain but got nil`）；五包 `-race` 全绿（workspace 5.212s / tool 16.055s / app 5.149s / cli 4.224s / testutil 1.696s）、集成 5/5 PASS、`make check` exit 0（读数见 §5） | 1 |

本段在基线时点**为空**；实施后每次修复尝试逐条追加（一行一次，不改旧行）。

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
| U-1 | 实施前阻断检查 `/speckit-analyze`（CRITICAL 必须清零后才进 implement） | **已验证**（2026-10-17：CRITICAL = 0，放行 implement） | 步骤 6 执行结论：37 条要求（30 FR + 7 SC）覆盖 36 条（97.3%）；14 条关键回归 / 18 个函数逐条点名；宪法十条 + 三条架构约束零冲突。findings = 3 MEDIUM（I1 `spec.md:11` 测试计数双算 / G1 FR-011 热加载无验证任务 / G2 SC-001 schema·描述逐字无具名断言）+ 3 LOW（G3 FR-024 引号语义无断言 / I2 T035 引用 FR-025 应为 FR-028 / I3 plan·quickstart 覆盖率枚举仅列本 US 包）；均属冻结输入的追加式修订，待用户裁决，不阻断实施 |
| U-2 | 颗粒度文档 §6.1 人工验收 13 项（门禁七目标读数 / Demo 三零代码 PR digest / 负向 Demo / 两档展示面抽验 / `tool list` 冒烟 / env 负向 / 停机子进程检查 / SDK 升级基线 / Windows 真控制台 / 凭证面复核 / 变更范围核对 / 四目标编译矩阵 / 并发抽验） | **未验证**（harness 之外，执行人 = 用户） | [us7-mcp.md](../../docs/us/us7-mcp.md) §6.1；自动化层由 §4 harness 承载，真模型 / 真 console 读数不在自动化层 |
| U-3 | [quickstart.md](./quickstart.md) §5 手工验收场景 S1~S5（零代码接入 / 故障隔离 / env fail-closed / 两档展示面 / 停机无残留子进程） | **未验证**（执行人 = 用户） | quickstart §5 已写明各场景核对项；S2 / S3 / S5 可部分以本机命令复核，仍属人工面 |

## 5. 收口结论

**写入时点**：步骤 7（收口三件套 + 七项证据 DoD）。四态汇总的指针，细节进验收报告。

| 项 | 值 |
| --- | --- |
| `/speckit-analyze` 复跑 | **已执行（2026-10-17）**：CRITICAL = 0，无新增 findings；固定字面量与术语逐字核对通过；既有 6 条 findings（3 MEDIUM + 3 LOW）仍属冻结输入的追加式修订、待用户裁决，不阻断 |
| 独立复核 | **已执行（2026-10-17，独立子代理，只读）**：结论 **通过**；0 CRITICAL / 0 HIGH / 0 MEDIUM / **5 LOW**（F-01~F-05）；5 条逐条验证与处置见验收报告「独立复核回执」；复核者未运行构建 / 测试（只读约束），执行面证据由作者侧门禁承担 |
| 七项证据 DoD | **全部满足**：门禁七目标 exit 0（收口复核后 2026-10-17 复跑仍 exit 0：fmt / gofumpt 干净、staticcheck 零输出、gosec + govulncheck `No vulnerabilities found`、test 与 test-integration 全 ok、cover 全部包达标、`CGO_ENABLED=0` build 成功）；18/18 关键回归本轮 PASS；收口复核新增 4 个测试函数并取得红态与 overlay 判别力读数（SC-001 从"已映射"升级为"已验证"）；交付物 12+6+2 逐项存在；前序模块全量回归绿；全局不变量 11/11 符合；U-2（13 项）/ U-3（S1~S5）仍为**未验证**（执行人 = 用户） |
| 结论 | **通过（带人工项）**——自动化层、静态复核与门禁全部完成；收口复核的 3 处 P2 与 3 条建议已修复并复验；剩余人工项 U-2 / U-3 待用户执行；commit / push / tag 待用户决定 |

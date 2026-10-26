# US-14 运行状态（run-state）

> 本文件是**过程状态，非设计工件**——speckit 命令不得把它当设计输入。
>
> 失败台账**只追加**：一行一次尝试，不改旧行（可 diff 审计）。
>
> 唯一允许原地翻转的是「未验证清单」的「未验证 → 已验证」，翻转必须注日期与证据。

## 1. 基线（2026-10-25 建立）

| 项 | 值 |
| --- | --- |
| US | US-14 MCP 目录与模板 + 五个每日业务 Demo + 发布收尾（14A / 14B / 14C） |
| 类型 | 开发类（台账第 15 条数据行，`docs/us/REGISTRY.md`） |
| 分支 | `014-templates-demos-release`（自 `main` `e2e285f` 新建；AI 不 commit / push / tag） |
| 起始 commit | `e2e285f`（`test: guard and record the Pages auto-deployment`） |
| 开跑时未提交改动 | 无（`git status --porcelain` 为空） |
| 前序交付 | US-1～US-13 已交付；US-15 验收基础设施已落地（`harness/stages/current.json`、`scripts/acceptance/`） |
| 模块卡 | `docs/us/us14-templates-demos-release.md`（**R3 定稿**，2,289 行，本地私有未入库） |
| 模块卡 SHA-256 | `ff6594655edc6f669488d744587d14b6d624c994c9bb2b47b2e58e068d284cc6`（R2 送审版经 review ① 通过；R3 只做三处一致性文字，未改已审设计） |
| 人审依据 | 人工 review ① 通过（用户，2026-10-25）：R2 全部设计；其余 J 项（J-01 / J-02 / J-04–J-09）与 Q 项（Q-01–Q-22）按 R2 建议采纳；**D-01–D-10 按 §2.4 修正后的状态规则冻结** |
| 台账状态 | 已登记（`/us-register 14`，2026-10-25；文档未入库） |
| worktree | `D:\code\basalt`（主工作区，单 US 串行，未开并行 worktree） |
| 启动环境 | Go 1.27.3（≥ `toolchain` 要求）；Node 24.22.0；Python 3.12（本机 `python` 可能为 Store 存根，本地复核用 `py`） |

### 1.1 三件输入指纹

| 输入 | 路径 | SHA-256 |
| --- | --- | --- |
| 颗粒度文档 | `docs/us/us14-templates-demos-release.md` | `ff6594655edc6f669488d744587d14b6d624c994c9bb2b47b2e58e068d284cc6` |
| `spec.md` | `specs/014-templates-demos-release/spec.md` | `a2559dd6a442bed268429bf04d07b3e5f5212b590f73a3410174ab87379a3e14` |
| `plan.md` | `specs/014-templates-demos-release/plan.md` | `50f33cc232b24613eabe9c2fbae6bab7cd908c804fbca9e3c6bf343d97d0ec75` |
| `tasks.md` 文件哈希 | `specs/014-templates-demos-release/tasks.md` | `efa371ddbd9723bea597f171e8f253adf5ac396ae03434a01993793bb0a1c8c3`（2026-10-25 阻塞解除后重算；勾选 16 项。冻结值 `786eb5c8…` 见 §1.4） |
| `tasks.md` 正文指纹（勾选位归一） | 同上（`- [x]` / `- [X]` → `- [ ]` 后哈希） | `cbe006f34f8ff0fb8e30af061b2709882269d2c10260fef1ed84bec6561a2cad`（勾选位归一，勾选数 16；2026-10-25 阻塞解除后重算） |
| `quickstart.md` | `specs/014-templates-demos-release/quickstart.md` | `4869a9ae2ca6933984246013b45a0ab2ec9781a7ebb81c2d87648fe558d33134` |
| `research.md` | `specs/014-templates-demos-release/research.md` | `746d6ddd98c5fa67dc4d134c3ff3356e61a48537d7a0722652305613577a7bf2` |

2026-10-25 实施前 analyze 修订记录：按 analyze findings A1–A6 修正 `tasks.md` T017 / T059 / T060 / T062 / T068 与 `quickstart.md` §2.3 / §3.1、`research.md` R-11（候选二进制 / temp 回退语义对齐模块卡 §4.3 与 §6.11 第 7 条）；**未改** `spec.md`、`plan.md`、模块卡、公共契约或验收语义。任务总数仍 70（T001–T070，ID 集合逐项核对无增删），勾选数 0，未重编号。

2026-10-26 指纹重算（收口 analyze 复跑的输入谱系，只追加，不改上表历史值）：

| 输入 | 冻结值（2026-10-25） | 当前值（2026-10-26） | 变更来源 |
| --- | --- | --- | --- |
| 颗粒度文档 | `ff659465…`（2,289 行，R3） | `c3a1694c5b509da0cd0d7cffaf73adf1f3e35f726b6038181e1cec1e571d8a55`（2,326 行） | T071–T078 的已批准裁决（DBHub 1.5.0 实测对齐、`docs-readonly` 并回主 module、安装器与发行面）在收口修复时按用户裁决回写模块卡；R3 的 `ff659465…` 仍是 review ① 所审版本，历史引用不改 |
| `spec.md` | `a2559dd6…` | `cc25b1c38cab71bb9d774d2ee3f633c31114f466618688fb6983d7a8afaa3041`（179 行） | T075 裁决后 FR-018 / FR-026 同步为「主 module 内 `package main` + 安装器实际导出源码并生成最小 `go.mod`」（commit `4511c4e` / `452678c`） |
| `plan.md` | `50f33cc2…` | `eced7af33a71d5cbf1bb2bbc4325de1ba9f719e0836f548fd78fc58d2b070610`（210 行） | 表头哈希谱系 + Project Type / Scale-Scope / Project Structure / Structure Decision 补齐安装器与 `examples/embed.go` 落位（本轮 analyze 修复） |
| `tasks.md` 文件哈希 | `efa371dd…` | `f9be75980574606dbee40d45b584651131ae824b7dc9cc3d09bcfb6d9022990b` | 追加 T071–T078 与后续勾选；冻结值只作历史 |
| `tasks.md` 正文指纹（勾选位归一） | `cbe006f3…`（70 任务 / 16 勾选） | `f482a7ebe0e108a0a0b516e0490e20a02d946a504c95459fb1f7bd4a6613c80d`（78 任务 / 77 勾选，余 T060） | 同上；正文指纹变化来自**追加任务**，未删改任何既有任务行（`git diff` 对 `tasks.md` 仅 8 增 8 删，全部是勾选位翻转） |
| `quickstart.md` | `4869a9ae…` | `e0298ca1a6d79b4da3fc5f1f1e8066c72dcd21a4bcea0f70575ad7c66e7ff4a0`（170 行） | stage 检查清单里的 `acceptance` 换成 2026-10-25 裁决后的四项、US-13 门解除说明、候选绑定口径（本轮 analyze 修复） |
| `research.md` | `746d6ddd…` | `23b21bff0c75a7b572ed9f60eabc86b8d8dc390432fb4969d018f90ba32e6456` | R-11 / R-12 随 `docs-readonly` 并回主 module 与安装器发行面同步 |

**冻结正文的过时表述（不改，只登记）**：`tasks.md` 的 T036 / T037 / T061 与 Phase 4 「Independent Test」仍写嵌套 module 与 `go -C examples/shared/mcp/docs-readonly`。三项都是**已勾选的冻结正文**，module-dev 步骤 0 的自检 ① 禁止修改勾选行正文；语义已由 T075 与 §2.2 #22 取代，实际执行面以 `Makefile` / stage / 模块卡当前值为准。收口包把这三处列为 errata（与 F-13 同处），由用户决定是否出 R4。

### 1.1.1 AI 任务比对与冻结（module-dev 步骤 5，2026-10-25）

| 项 | 结论 |
| --- | --- |
| 任务总数 | 70（T001–T070；14A = T001–T035，14B = T036–T060，14C = T061–T067，收口 = T068–T070） |
| 交付物覆盖 | 模块卡 §3.1–§3.10 逐项有任务：契约 D-01–D-10（T001）、core 四文件（T002–T005）、catalog 四文件（T006–T009）、tool 构造 / 详细连接（T010–T012）、app 九文件与测试（T013–T025）、web 七文件与测试（T017 / T026–T029）、控制台七文件（T030–T034）、五 Demo（T041–T045）、八模板（T046–T053）、共享素材（T036–T040）、三个 e2e 文件（T054 / T055 / T063）、Makefile / 脚本 / stage（T056 / T061–T065）、发布证据（T066–T067）、文档同步（T035 / T057 / T058 / T059） |
| 多余 / 缺失 | 无多余任务；补入 `DevelopmentLoop.md` §5.3 与 `internal/cli/public_docs_test.go` 两处映射后无缺失 |
| 顺序 | 测试任务先于或伴随实现（T006 先于 T007–T009；T010 先于 T011–T012；T013–T017 先于 T018–T029；T054–T056 伴随示例）；14A → 14B → 14C 依赖链一致 |
| Check 可追溯 | 每个任务标注模块卡 §4.3 check id 或公共 `acceptance.json`；映射表见 `tasks.md` Task → Check Mapping |
| 边界来源 | 「明确不做」与冻结路径取自模块卡 §2 / §5；`go.mod` / `go.sum` / `schema.sql` / `acceptance.json` / 既有 stage / CI 均不在任务允许面 |
| 阻塞项 | T060（外部授权）、T066（Linux / 许可 / 升级演练）按模块卡 §5.4 登记为未完成条件，不伪装通过。T058 / T059 的 US-13 review ② 门已于 2026-10-25 解除（见 §1.4） |
| 结论 | AI 比对通过，任务清单冻结；无需用户对 tasks 另行批准（module-dev 总则 0） |

### 1.1.2 实施前 `/speckit-analyze`（module-dev 步骤 6 阻断检查，2026-10-25）

| 轮次 | 范围 | 结论 |
| --- | --- | --- |
| 第 1 轮 | `spec.md`（FR-001–FR-025 / SC-001–SC-012）/ `plan.md` / `tasks.md` / 模块卡 R3 / 宪法十条 | **CRITICAL = 0**；findings A1（HIGH）–A6（LOW），全部落在已审范围内的派生不一致 |
| 第 2 轮（修订后复跑） | 同上 + 修订后的 `tasks.md` / `quickstart.md` / `research.md` | findings 全部关闭；**CRITICAL = 0**，允许进入 implement |

| ID | 严重度 | 位置 | 问题 | 处置 |
| --- | --- | --- | --- | --- |
| A1 | HIGH | `tasks.md` T062 / `quickstart.md` §3.1 / `research.md` R-11 | 原写“`BASALT_E2E_BINARY` 缺失 = 失败”，与模块卡 §4.3（`demo-fixtures` / `legacy-d1` / `template-quality` 直接 `go test -tags=e2e` 不设该变量）及 §6.11 第 7 条（temp 回退）冲突 | 已改为：变量存在 = 候选（`candidate`，不重建）；缺失 = temp 回退（`temp`）；仅 `make e2e` / stage 断言 `candidate` |
| A2 | MEDIUM | `tasks.md` T017 | D-02 未知 server 的 404（`NOT_FOUND`）无契约断言 | 已在 T017 增补 |
| A3 | MEDIUM | `tasks.md` T059 | SC-011（不改 US-13 已验收视觉文件）无保护断言 | 已在 T059 列出冻结视觉文件与 `website/public/**`、`website/diagrams/**`，收口以哈希核对 |
| A4 | MEDIUM | `tasks.md` T068 | FR-025 零迁移 / 零依赖 / 不改门禁与 CI 无显式收口断言 | 已在 T068 增补 `git diff --stat` 核对路径 |
| A5 | LOW | `tasks.md` T060 | 人工外部实跑任务标注了自动 check `demo-fixtures` | 已改为“manual，不绑定自动 check” |
| A6 | LOW | `quickstart.md` §2.3 | 直接 `go test -tags=e2e` 未说明使用 temp 二进制 | 已增加说明，避免与发布证据混淆 |

覆盖率：FR-001–FR-025 与 SC-001–SC-012 均有 ≥1 个任务映射（映射表见 `tasks.md` Task → Check Mapping）；无未映射任务。宪法对齐：零违反（零新增坐标 / 零迁移 / 单二进制 / 目录与诊断只读 / fail-closed 预写 / `catalog` 只依赖 `core`）。六个派生工件 markdownlint 全绿（0 issues，退出码 0）。

**stage 派生时补查的锚点缺口（A7–A11，2026-10-25，同属 analyze 覆盖面，已修）**：按模块卡 §4.5 / §4.4 逐条核对 `required_tests` 时发现五处任务侧缺漏，均在已审范围内按模块卡逐字补名，未改范围、契约或断言。

| ID | 严重度 | 位置 | 问题 | 处置 |
| --- | --- | --- | --- | --- |
| A7 | MEDIUM | `tasks.md` T017 | §4.5.4 `TestMCPCatalog_ReadOnly` 与 §4.5.5 `TestMCPDiagnostics_DoesNotStartProcess` 两个 web 层只读断言未列入任务 | 已补入 T017（含六条 / 零写 / 零操作行 / 零 `CommandRunner` / 不启动进程） |
| A8 | MEDIUM | `tasks.md` T016 | §4.5.12 / §4.5.13 的规范名 `TestMCPTest_NoOperationOnPrewriteFailure` / `TestMCPTest_EmptyToolList` 与任务里旧名（`_PreflightWriteFailureNoRow` / `_EmptyToolsOK`）不一致 | 已改为 §4.5 逐字名，避免 stage `required_tests` 与实现分叉 |
| A9 | MEDIUM | `tasks.md` T016 | §4.5.9 `TestNotifyMode_DeliverToRemovesNotify`（`deliver_to` 与 `notify` 互斥）无任务承载 | 已补入 T016（`internal/app/notify_mode_test.go`），并新增 stage check `notify-exclusion` |
| A10 | MEDIUM | `tasks.md` T010 | §4.4 `TestRegistry_DuplicateSemanticsUnchanged`（注册语义回归）无任务承载 | 已补入 T010；既有 `TestRegistry_RefusesDuplicateNameAndKeepsOriginal` 保持不变，两者互补不替代 |
| A11 | LOW | `tasks.md`（无对应任务） | §4.4 另列 `TestScheduleDelivery_OnlySuccessfulRun` / `_NoAutoResend` / `_SkippedOnNonSuccess` 三个名字在仓库中**不存在**；等价语义由既有 `TestDeliveryService_*` 与 D1 夹具承载 | **不新增同名测试、不改既有测试**：stage approval 记录该名字到既有覆盖的映射，人工 review ② 时核对；若判为必须新写，回到 review ① 追加任务 |

### 1.1.3 stage 派生（module-dev 步骤 6.1，2026-10-25）

| 项 | 值 |
| --- | --- |
| 路径 | `harness/stages/us14-release.json`（新文件，未入库；`git status` 显示 `??`） |
| SHA-256 | `e7be194372a522d99cefe50daad3de229dec9d75d3063e2a1a499182ec32a39e`（演进：初稿 `4be48aee…` → `52b62f57…`（补 tasks 哈希 / 增 `go-build` / `contract-yaml`）→ `e9daa482…`（approval 内 tasks 哈希更正）→ `e7be1943…`（T010–T012 绑定修订，见下）→ `ddbf8cb8…` → `2b537efa…`（2026-10-25 阻塞解除修订，见下） |
| id / version | `us14-release-r1` / `1` |
| 来源 | 模块卡 §3.8（建议 check 集 + protect 建议）、§4.3（check 归属）、§4.4 / §4.5（`required_tests` 逐字名）、§5.3（允许 / 禁止面）、§6.11 第 7 条（候选二进制） |
| inputs | 13 条，全部存在（模块卡、`spec` / `plan` / `tasks`、`harness/acceptance.json`、两份契约、`Operations.md`、台账、`go.mod` / `go.sum`、`Makefile`、`ci.yml`） |
| protect | 分类四组：公共门禁面（`internal`、契约、四份正文、台账、`go.mod` / `go.sum`、`Makefile`、`acceptance.json`、两个 `check-acceptance` 脚本、`ci.yml`）、既有 stage（`current.json` + 三个 `us15-*` + `us13-website-readme.json`）、US-13 冻结视觉面（九个视觉文件 + `website/public` + `website/diagrams` + `docs/us/us13-*` + `specs/013-*` 的 run-state）。**只列已存在路径**：`loadStage` 对每条 `Lstat`，新建的 Go / Python / 前端 / 站点文件在 T065 再补入 |
| checks | 25 条：`go-build` / `contract-yaml` / `mcp-catalog` / `mcp-env-gate` / `mcp-diagnostics` / `mcp-test-record` / `mcp-connect` / `mcp-disposition` / `mcp-contracts` / `notify-exclusion` / `mcp-admin` / `console-test` / `console-build` / `demo-fixtures` / `legacy-d1` / `template-quality` / `docs-readonly-tests` / `script-tests` / `e2e` / `e2e-required` / `website-test` / `website-build` / `website-check` / `acceptance` / `clean-checkout` |
| tasks | 70 条（T001–T070，逐项核对 ID 集合无增删），每条 ≥1 check；`mcp-admin` 为**未绑定**的跨任务聚合（`main.go` 明确支持未绑定阶段断言），只在 full 运行 |
| 验证（已做） | JSON 解析通过；`inputs` + `protect` 59 条路径全部存在；check / task ID 唯一；任务 ID 恰为 `T001..T070`；所有 `go test` check 均带 `-json` / `-count=1` 与 `required_tests`；`--stage ... --task ZZ` 走完 `loadConfiguration` + `loadStage` 至 `unknown stage task`（exit 2，未执行任何 check）→ **stage 可加载** |
| 验证（未做） | `--lint-stages` 现必红（被测包与测试尚未实现），T065 实现完成后按实际 `go test -list` 逐条复核 `required_tests`；`make e2e` / `acceptance` / `clean-checkout` 均未执行 |

派生规则（不弱化断言，逐条留痕）：

1. **候选 / temp 双 check**：`e2e`（`make e2e`，`level=process`）供运行器抽取唯一 `binary_sha256` 标记；`e2e-required` 用 `go test -tags=e2e -json` 按名断言 §4.3 的 13 个必跑测试（temp 回退下可执行，仅功能验收，不作发布证据）。两者互补，`make e2e` 仍是唯一的发布候选证据入口。
2. **新增两个 check**：`mcp-connect`（§4.5.6 tool 层 `ConnectDetailed` / 旧语义 / 命令构造同源）与 `notify-exclusion`（§4.5.9）在 §3.8 建议集里无承载，按 §4.3 归属补为具名 check —— 只加断言，不删任何建议项。
3. **合并不重复跑**：§3.8 的 `example-tests` 折入 `docs-readonly-tests` + `script-tests`；`templates` 折入 `template-quality` + `make e2e` —— 同一命令、同一断言面，避免一次 full 运行跑两遍。
4. **Python 启动器**：本机 `python` 是 Store 存根，`script-tests` 用 `py -3`；缺解释器仍是失败，不转跳过。`Makefile` 的 `example-tests` 保持 `PYTHON ?= python` 可由环境覆盖（T061 落地）。
5. **网站三 check**：`website-test` / `-build` / `-check` 的 US-13 review ② 门已于 2026-10-25 解除（§1.4）；解除只拿掉"不许动"，T059 的冻结面（九个视觉文件、`website/public/**`、`website/diagrams/**`）照旧，未执行前不宣称通过、不勾选。
6. **不碰门禁面**：`inputs` 只读登记；stage 不进 `current.json` / CI，不改 `harness/acceptance.json` 与 `us15-*.json`；`approval` 记录真实 review ① 决定与三件输入 / 模块卡哈希，不写模板占位。
7. **Phase 1 的判据可满足性**：T001–T005 只有类型 / 契约文档，语义断言在 T006 / T015 / T016 / T017 的测试任务里才存在。故 T001 绑 `contract-yaml`（`TestOpenAPI_ViewEnumsMatchGoLiterals`：文档可解析 + 既有枚举未漂移）、T002–T005 绑 `go-build`（整树编译）；新 schema 的良构性由 T017 的 `TestMCPContracts_*` 用 `compileObjectSchema` 断言，断言不减少，只是落到能判它的任务上。
8. **中间红窗口（预期，不是回归）**：T001 写入 `tool_disposition` 与测试结果四字段后，`TestOpenAPI_McpServerViewPropertiesMatchContract` / `TestOpenAPI_AdminWriteViewsMatchContract` 在 T029 补齐视图前保持红。该窗口内**不得**把 `make check` / `acceptance` 判为通过；`acceptance` 只绑在 T035 及之后的任务，落在窗口之外。

2026-10-25 修订：`approval` 中的 `tasks.md` 冻结哈希由 `fa37323b…` 更正为 `786eb5c8…`（A7–A11 补锚后的实际值），文件其余内容不变；更正后文件哈希为 `e9daa482…`（见上表），并已重新通过 `loadStage` 加载验证。

2026-10-25 修订（二，T010–T012 绑定）：三条任务的 check 绑定由「本任务 check + 后续阶段 check」收窄为 `["mcp-connect"]`。原因：`mcp-disposition` 的 `required_tests` 含 `TestMCPToolDisposition_*`（T015 / T023–T025 才落地）、`mcp-test-record` 含 `TestMCPTest_*`（T016 / T021–T022）、`mcp-diagnostics` 含 `TestMCPDiagnostics_*`（T014 / T020）——原绑定会让这三个任务在自身实现完成后仍无法验收，与「任务只绑判它自己输出的 check」的既有 US-11 / US-12 口径相悖（T004 已按同一理由绑 `go-build`）。**未删改任何 check、命令、`required_tests` 或断言**：25 条 check 全部保留且每条仍 ≥1 个任务绑定，full 运行面不变；三条任务仍由 `mcp-connect` 判卷。修订后哈希 `e7be194372a522d99cefe50daad3de229dec9d75d3063e2a1a499182ec32a39e`，已重跑 `loadStage` 加载验证。

2026-10-25 修订（三，阻塞解除联动，§1.4）：`approval` 删除「website-test/-build/-check 直到 US-13 review ② 完成前阻塞」的过期表述，改为记录 2026-10-25 解除及解除范围；同时追加本节第一条。**未改** `inputs` / `protect` / `checks` / `tasks` / `required_tests`——判据面零变更，只是审批文字与实际状态对齐。修订后哈希 `2b537efa50f778fa7058d3ea4422e4830e0119edaf88ad942af3ccf9312ee2bc`（中间值 `ddbf8cb8…` 因 tasks.md 再补一处 Phase 4 依赖表述而重算，最终值以此为准）；JSON 解析通过，`--stage ... --task ZZ` 仍走完加载并以 `unknown stage task`（exit 2）结束，未执行任何 check。

### 1.2 允许触碰范围（以模块卡 §5.3 为准，按切片）

- **14A 新增**：`internal/catalog/**`（含 `data/mcp_catalog.yaml`）；`internal/core/mcp_catalog.go` / `mcp_diagnostics.go` / `mcp_disposition.go`；`internal/app/mcp_env.go` / `mcp_diagnostics.go` / `mcp_disposition.go` 及测试；`internal/tool/mcp.go` 的 `NewMCPCommand` / `ConnectDetailed` / `MCPConnectFailure`；`internal/web` 目录 / 诊断 handler、视图、端口、测试；`assets/console/src/views/catalog.ts`、`api.ts` 目录类型、`api-catalog.ts`、相关测试；`docs/contracts/openapi.yaml` 的 D-01–D-10。
- **14A 修改**：`internal/app/resource_admin.go`（env 闸门 / 测试记录 / 诊断方法 / deps 字段）、`internal/app/mcp_supervisor.go`（Test 分类 / 字段）、`internal/app/mcp.go` / `resources.go` / `wire.go`、`internal/core/resource_admin.go`（`ResourceTestResult` 追加字段）、`internal/web/server.go` / `ports_admin.go` / `admin_routes.go` / `handlers_admin_resources.go` / `views.go` / `views_admin.go`、`assets/console/src/{router,api,views/resources,views/resource-detail,components/resource-form}.ts` 与测试、`assets/console/dist/**`（重建）、四份正文与 `docs/Operations.md`（事实同步）。
- **14B 新增**：`examples/**`（五 Demo + 八模板 + 共享 `docs-readonly` 嵌套 module + Python 测试 + fixtures）、`internal/e2e/us14_*.go`（测试）、`internal/testutil` 测试夹具、`website/.vitepress/data/templates.mjs` 与 `TemplateTable.vue`（US-13 已收口，可执行）。
- **14C 新增**：`harness/stages/us14-release.json`、`scripts/check-release-clean.sh`、发布证据文件；修改 `internal/e2e/harness_test.go` 的 `TestMain` 候选分支。
- **不得触碰**：`go.mod` / `go.sum`（零新增坐标）、`docs/contracts/schema.sql` 与迁移、`harness/acceptance.json`、`harness/stages/current.json` / `us15-*.json`、`.github/workflows/ci.yml`、`internal/tool/registry.go` 注册语义、`internal/core/resources.go` 的 `RunResourceManifest`、US-13 已验收视觉文件与 `website/public/**` / `website/diagrams/**`、`docs/us/us13-*`、`specs/013-*`、`career/**`、`docs/interview/**`、`CLAUDE.local.md`。
- **README / 官网增量**：US-13 review ② 门已于 2026-10-25 解除，可执行；但仍须守住 T059 的冻结面与英文单语、不新增一级路由、不链接私有路径（§1.4）。
- **追加范围（2026-10-26，T071–T078 经用户裁决）**：`examples/embed.go`、`internal/cli/demo.go` / `demo_test.go` / `root.go`、`internal/e2e/us14_demo_install_test.go`、`internal/e2e/us14_examples_test.go`、`internal/catalog/data/mcp_catalog.yaml`、`examples/shared/mcp/docs-readonly/`（删除其 `go.mod`）；stage `protect` 已增 `examples`。上列 14B 行中的「嵌套 module」为 2026-10-25 形态，2026-10-26 起已撤回（§2.2 #22、§4 对应条目）。

### 1.3 外部授权登记（模块卡 §4.6，全部未授权）

| 外部项 | 授权状态 | 缺失时处置 |
| --- | --- | --- |
| 真实模型 Provider | **未授权** | 14B 不完成；夹具不能替代 |
| GitHub PAT（D1 / D2） | **未授权** | D1 / D2 真实外部不完成 |
| Brave API key / Fetch 目标网页（D5） | **未授权** | D5 不完成；不得用 HN / 内置 HTTP 替代 |
| MCP 二进制 / Node / Python / Playwright / DBHub 安装 | **未授权** | 由用户执行；未装 = 未验证 |
| 真实通知接收方 | **未授权** | 本地回环接收器做夹具；真实送达未验证 |
| `v0.1.0` tag / 发布 | **未授权** | 用户手动；AI 不代打 |

授权 ref 按 US 唯一发放（`user-YYYY-MM-DD-<slug>`），不继承其他 US；未授权项一律进未验证清单，不写成通过。

### 1.4 阻塞解除：US-13 review ②（2026-10-25，用户指令）

条件原文在 `spec.md` Q-17、模块卡 C-22 / §3.9、`plan.md` 第 28 行：US-13 review ② 完成前不改其交付物。条件已满足，按用户 2026-10-25 指令解除：

| 项 | 证据 |
| --- | --- |
| 最终 review ② 通过 | US-13 run-state §13.1：用户原话「暂时没问题了……us-13 可以收口」与再次确认「us13 收口吧」，记为最终通过 |
| 收口基线 | US-13 run-state §13.5：公开基线 commit `24de00e`，`origin/main` 与 `origin/docs/readme-license` 均指向它，该 commit 的 CI 四个 job 全绿 |
| 更晚的发布验证 | `a58a832` / `e2e285f` 的 Pages 自动发布经运行 `40115518583` 验证（US-13 run-state §15.3）；US-14 起点 `e2e285f` 晚于收口基线 |

解除只拿掉「不改 US-13 交付物」这道门，**不是放开**：

- T059 仍**不得**触碰 SC-011 冻结的九个视觉文件（`Layout.vue` / `HomeHero.vue` / `CapabilityGrid.vue` / `CliDemo.vue` / `DiagramFigure.vue` / `LanguageSwitch.vue` / `SiteFooter.vue` / `custom.css` / `index.ts`）与 `website/public/**` / `website/diagrams/**`，收口时以哈希核对；
- README 仍英文单语，不新增一级路由，不链接私有路径；
- T060 的阻塞是外部授权（真实模型 / GitHub PAT / Brave / Fetch / DBHub / 通知接收方），与 US-13 无关，解除后 14B 依旧不会因此完成。

连带修订：`tasks.md` 移除 T058 / T059 的「（阻塞：US-13 review ②）」标记、阻塞表整行、Phase 4 注意段与依赖段的同义表述（任务数仍 70，ID / 勾选不变、无断言变更）；§1.1 两个 `tasks.md` 指纹随之重算（文件 `efa371dd…`、归一 `cbe006f3…`，冻结值 `786eb5c8…` 作废）。`harness/stages/us14-release.json` 的 `approval` 追加本次修订说明并重算哈希（§1.1.3）。

## 2. 失败台账

| # | 日期 | 阶段 | 问题（测试名 / 首错位置 + 症状短语） | 假设 | 结果 | 同题计数 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 2026-10-25 | T010（实现前红态 → 首次运行） | `TestMCPPool_ConnectDetailed/握手超时` 挂起：`go test ./internal/tool` 3 分钟未结束，`-timeout 90s` 后 panic 栈显示阻塞在 `mcp_test.go:1200` 的 `time.Sleep(1h)` → `handshakeBlockingTransport.Connect` | 测试夹具在 `onConnect` 回调里睡满一小时，睡眠发生在检查 `ctx.Done()` 之前，预算对这层无效——是实现期夹具缺陷，不是连接池行为问题 | 改为空回调，让传输直接阻塞在 `ctx.Done()`；重跑该用例 0.05s 通过，整包 5.087s 全绿 | 1 |
| 2 | 2026-10-25 | T013（回归运行，实现后） | `TestMCPTest_MissingEnvNotConfigured`（US-12 既有用例）红：`resource_admin_test.go:2382` 期望保存成功，实际返回 `NOT_CONFIGURED` | 该用例的前提（"保存一条引用缺失变量的条目"）与 US-14 FR5 / 模块卡 §2.3 图 2-2 的创建期闸门直接冲突——是**已批准接口变化**导致的前提失效，不是实现错误 | 只改**前置形态**、断言逐条保留：先在变量就绪时保存，再把变量置空后调测试端点（"保存时满足、测试时不满足"）。见 §2.2 判据面变更记录 | 1 |
| 3 | 2026-10-25 | T020（诊断首次实现后跑 T014 判据） | `TestMCPDiagnostics_NotFound/absolute` 红：期望 `not_found`，实际 `unconfirmed` | 实现里假设"绝对路径下 `cmd.Err == nil` 意味着构造器声称解析成功"，于是把 os.Stat 明确不存在降级为 unconfirmed；实际 `exec.Command` 只对**裸名**调 LookPath（Windows 扩展名补全发生在 `Start`），绝对路径的 `cmd.Err` 恒为 nil、不携带存在性信息 | 修**实现**不改判据：绝对路径按 C-04 的第二种证据——`os.Stat` 明确不存在即 `not_found`；十条测试 3.34s 全绿 | 1 |
| 4 | 2026-10-25 | T035（`make acceptance` 首轮，报告 `accept-20261024T210756.509378300`） | `lint` exit=2：`internal\catalog\catalog.go:1:1` 与 `internal\catalog\data.go:1:1` 两条 ST1000「at least one file in a package should have a package comment」 | 包注释放错位置——`catalog.go` 的 `// Package catalog …` 写在 `const` 块之前而非紧贴 `package catalog`；`data.go` 的文件注释又被 staticcheck 当成了候选包注释 | 修实现不改判据：包注释移到 `package catalog` 正上方、`data.go` 的说明降为包声明之后的普通文件注释；`staticcheck -checks=ST1000 ./internal/catalog` 与全量显式 `-checks` 复跑均 exit 0 | 1 |
| 5 | 2026-10-25 | T035（同轮 `sec` exit=2，gosec G101 ×4） | `internal\app\mcp_diagnostics.go:48/49/52/53` G101「Potential hardcoded credentials」；改成 iota 下标 + 码表后仍余 1 条（落在 `[...]string` 字面量上） | 实测根因：gosec v2.29.0 的凭据名正则是 `(?i)passwd\|pass\|password\|pwd\|secret\|token\|pw\|apiKey\|bearer\|cred`，`mcpWarn…` 前缀含子串 `cpW` 命中裸 `pw` 分支（仓库外探针程序逐名验证：六个旧名全部 match=true、新名全部 false）；再配上 zxcvbn 判定为高熵的 code 字面量（截断 16 字符读数 68.74 / 50.76 / 60.31 / 51.08 bit；另两条 43.20 / 46.19 未达标）即触发 | 修实现不改判据：常量族改名 `mcpDiagWarn…`（18 处、单文件），code 字面量一字未动（D-10 固定）；**未用** `#nosec`（`grep -rn "#nosec"` 在 `internal/app/mcp_diagnostics.go` 与 `internal/catalog/` 零命中）；`gosec -quiet ./...` exit 0；注释写明该前缀不得改回的原因 | 1 |
| 6 | 2026-10-25 | T036（首次 stage 判卷，报告 `accept-20261024T214646.194629200`） | `docs-readonly-tests` 失败：`failed docs-readonly-tests (exit=0) required test skipped a subtest: TestDocsReadOnly_RejectsSymlink/real_symlink`——子用例用 `t.Skipf` 处理"本机无 symlink 权限"，控制侧按「SKIP ≠ pass」判红（判据本身没错：跳过的用例不构成证据） | ① 保留跳过、把该子用例从 required 测试里摘掉 → **否决**（那是削判据面换绿态）；② 换机制：Windows 无 `SeCreateSymbolicLinkPrivilege` 时改用 junction（`mklink /J` 不需特权），Go 1.27 的 `Lstat` 把它报成 `ModeIrregular` 而非 `ModeSymlink`、`WalkDir` 不进入——服务端在"某一层不是目录"处挡住，逃逸被真实断言 | 假设 ② 成立：`real_symlink` 改造为 `real_filesystem`，先试 `os.Symlink`，失败且平台为 Windows 时退 `exec.Command("cmd","/c","mklink","/J",link,target)`；两者都失败按 `t.Fatalf` 处理（缺前置 = 失败，**不再有** `t.Skip`）。首跑仍红：`软链接与 junction 都无法创建（exit status 1；文件名、目录名或卷标语法不正确。）`——根因是 `cmd /c` 收到一整个含引号的字符串，Go 用反斜杠转义内部引号、`cmd.exe` 不认这套转义；改为分参数传递后 `真实重解析点机制：junction（escape）` 且 `TestDocsReadOnly_RejectsSymlink` 整体 PASS；全模块 `-json` 读数 `pass`×10 / `skip`×0 / `fail`×0。`grep -rn "t.Skip" examples/shared/mcp/docs-readonly/` 零命中 | 2 |
| 7 | 2026-10-25 | T043 单测 `test_naive_clock_rejected`（实现期首跑） | `AssertionError: ScriptError not raised`——用例直接调 `biz_daily.daily(..., now="2026-10-25T09:00:00")` 期望抛错，但 `daily()` 把 `now` 当**标签字符串**原样写进 `observed_at`，时区校验只在 CLI 的 `parse_clock` 里 | 不是实现缺陷（纯函数不校验标签、CLI 校验时钟，分工成立），也不是放宽断言：改成从**真实入口**判——`biz_daily.main([... "--now", "2026-10-25T09:00:00"])` 断言 exit 1 且输出/父目录都不创建，另补一条 `parse_clock` 直断言；判据意图（无偏移的时钟必须被拒）保持不变 | 改测试入口后 `Ran 19 tests … OK`；红态由仓库外探针逆改独立证明（见 T043 读数） | 1 |
| 8 | 2026-10-25 | T054（`TestUS14Demos_Fixture` 首跑 / `repo_digest` + `industry_brief`） | 任务 `last_status = failed`，`runs` / `tool_invocations` **零行**；`task_executions.error_message` = `resource unavailable: repo-digest (agent resource bindings are missing, invalid or ineligible; …)` | 夹具给 `notify_mode: none` + `notify_channels: []` 的 D2 / D5 也写了 `notify_channel` 绑定；`CheckAgentBindings` → `ValidateCorrespondence` 要求绑定名集合与声明引用**双向精确相等**，多出的那一条即 invalid（D2 / D5 走系统代投，渠道只该出现在任务 `deliver_to`） | 修夹具不改判据：两处 `demoOptions` 不再传 `channelUID`（`deliverTo` 保留）；`demoBindings` 其余路径不变 | 1 |
| 9 | 2026-10-25 | T054（同轮 / `docs_diff`） | `limit_reached`：`runs.iterations = 10`，剧本 14 步「还剩 4 步」 | `AGENT.md` 的 `settings.max_iterations: 20` **不可能生效**——契约 §6 / 技术方案 §2.5.1 规定 Profile 只能收紧运营上限 `budget.max_iterations`（缺省 10），而夹具 `basalt.yaml` 没有 `budget:` 段；D2 / D5 的 12 / 30 同样被压到 10 | 修夹具：`demoFixture.config()` 写 `budget.max_iterations: 30`（五 Demo 声明值的最大者），与示例 SETUP 要求的运维侧调高是同一链路；是否仍有越界待重跑读数 | 1 |
| 10 | 2026-10-25 | T054（同轮 / `biz_daily`） | `缺失字段 amount = 0，期望 1`；但落盘 `runs/*/daily.json` 里 `by_field.amount = 1`、输入哈希与固定输入一致 | 脚本 stdout 是**摘要视图**（`quality` 只有 total / truncated / counts，`metrics` 只有 computed / currency / net_amount），四类坏行明细与完整指标只在 `daily.json`；测试把明细断言与 `daily.md` 渲染都架在 stdout 上 | 修测试判卷对象：明细 / 指标断言与日报渲染改用 `read_file` 读到的 `daily.json`（新增 `decodeD3Artifact`），stdout 摘要改为交叉核对（message / observed_at / input / report / 四类计数必须一致）——断言强度提高，未放宽 | 1 |
| 11 | 2026-10-25 | T054（第二轮 / `repo_digest`） | 日报表格渲染成 `\| PR \| 6 \| 6 \| 1 \| 0 \|`，测试期望 `\| PR \| 6 \| 6 \| 1 \| 1 \|`；同轮「待确认」段却列出 3 条、结论句也写「待确认 3 项」 | 夹具渲染器 `d2DigestReport` 把「待确认字段」当成 `sources.<源>` 的**顶层**字段解，脚本实际把它放在 `sources.<源>.counts` 里（与桶计数同表，`repo_digest.py` 第 504 行）——解不到就静默得 0（`testDemoRepoDigest` 里读 `counts` 的那条断言一直是通过的） | 修渲染器不改判据：删掉顶层字段、改读 `entry.Counts["with_unconfirmed_fields"]`，表格期望值逐字保留 | 1 |
| 12 | 2026-10-25 | T054（第三轮 / `repo_digest`） | `首轮 read_file 3 次，期望 2 次（L2 + L3）`；第 3 次是 `runs\<时间戳>\digest.json` | 夹具第 10 步是「回读本轮产物再写日报」（L2 模板要求数字照抄产物，与 D3 / D5 同构），但断言还停在「两次读取」的旧形状 | 修判据形状不放宽强度：总数改 3，并把第 3 次**钉死**在产物路径（`filepath.ToSlash` 归一后判 `/digest.json`）；前两次仍必须落在发布目录（SKILL.md / REFERENCE.md 同目录） | 1 |
| 13 | 2026-10-25 | T057（stage 判卷，报告 `accept-20261025T024106.882051900`） | `acceptance` exit=2：`sh scripts/check-acceptance.sh --profile full` → `exclusive harness lock unavailable; ensure no other run is active` → `make: *** [Makefile:22: acceptance] Error 2` | stage 的 `acceptance` 检查用 `make acceptance` 包装了同一验收入口；`scripts/acceptance` 每次启动都取独占 `.loop/lock`（`main.go:90`），外层任务运行已持锁 → 自锁 | 不是断言失败而是**设施自调用**：删除包装检查，改绑任务级具体锚点（`docs-consistency` / `stage-lint` / `release-records`）；full profile 仍由 stage 整体运行（默认 full + 全部 stage / 任务检查）执行，是 `make acceptance` 的严格超集。详见 §2.2 #8 | 1 |
| 14 | 2026-10-25 | T057（full profile 首轮，报告 `accept-20261025T024818.525174400`，`sec` 失败后人工中止） | `sec` exit=2：`internal\testutil\mcpserver.go:181` G703（CWE-22，taint 分析，HIGH）+ G304「Potential file inclusion via variable」——新增的 `BASALT_MCP_HELPER_SPEC` 规格读取用裸 `os.ReadFile(path)`，路径来自父进程环境变量 | 与 `internal/cli` 的 `readConfigFile`（T136）同型：裸变量路径必被判文件包含，而抑制手段（`#nosec`）默认禁用；正确修法是 `os.OpenRoot(filepath.Dir(path))` + `root.ReadFile(filepath.Base(path))`，既消除判定又把越界软连接挡在门外 | 修实现不改判据：`readMCPSpec` 改用 `os.Root` 钉住父目录（注释写明与 `readConfigFile` 同口径）；`make sec` 复跑 `No vulnerabilities found.`、`gofumpt -l` 空、e2e 夹具两用例（`TestUS14Demos_Fixture` 与 `TestTemplates_`）100.493s ok；被中止的 `accept-20261025T024818.525174400` 报告停留 `running`，**不作证据**；另清理遗留临时资源：`bin/basalt serve`（PID 17924，2026-10-24 12:38 起）已终止、中止运行留下的 `.loop/lock`（pid=37820 已不存在）已按文档检查后删除 | 1 |
| 15 | 2026-10-25 | T058（控制侧首跑，报告 `accept-20261025T025422.928140100`） | `website-test` error exit=2：`executable unavailable: npm` | 环境前置缺失而非判据失败：本机 user / machine PATH 都没有 Node（`(Get-Command npm).Source` 为空），US-11 / T030 的便携 Node 只在调用时以 `PATH=<便携目录>:$PATH` 前缀注入，未进持久配置；验收入口按调用进程的 PATH 解析 `npm` | 修前置不碰判据：同一命令加 `PATH=/c/Users/76562/AppData/Local/Temp/basalt-node/node-v24.22.0-win-x64:$PATH` 重跑（报告 `accept-20261025T025437.896988400`）= `passed fmt` + `passed website-test`。收口证据须注明该前置（与 T030 补充读数同口径） | 1 |

| 16 | 2026-10-25 | T061（控制侧首跑，报告 `accept-20261025T031540.131143200`） | `fmt` passed、`e2e` passed（213.4s）之后验收工具报 `e2e: level process requires exactly one binary_sha256 marker, found 0` → `acceptance task/us14-release-r1/T061: error exit=2` | `go test` 在包通过且未带 `-v` / `-json` 时会吞掉包级 stdout，`TestMain` 的 `binary_sha256=` 标记随之消失；模块卡 §3.8 / §4.3（第 1430 行）写的本就是 `go test -json -tags=e2e`，T061 任务正文把 `-json` 简写掉了 | 修实现不碰判据：`e2e` 目标补 `-json`，Makefile 注释写明原因；实测 `BASALT_E2E_BINARY=… go test -json -tags=e2e -run '^$'` 恰好 1 条 `binary_sha256=c12aa4cd…04a1`；失败报告停留 `error`，不作证据 | 1 |
| 17 | 2026-10-25 | T063（控制侧首跑，报告 `accept-20261025T034205.292993100`） | 三项检查全 `passed`（`fmt` 1.3s / `e2e` 236.4s / `e2e-required` 214.8s），工具终态却是 `source_changed` exit=2 | 我在启动验收时把日志重定向到仓库内的 `.tmp-us14/accept-T063.log`。`sourceFingerprint` 取 `git ls-files --cached --others --exclude-standard`，`.tmp-us14/` 未被 git 忽略；`ignored()` 只按**目录前缀**豁免 `bin` / `.loop` / `.git` / `.basalt` 等、按**基名**豁免 `.tmp-*`，`accept-T063.log` 的基名不匹配 → 该文件计入指纹，运行中又被持续追加 → 前后指纹不同 | 不是实现缺陷，是**证据污染**：删掉仓库内日志、改把输出落 `$LOCALAPPDATA/Temp/` 重跑 → `accept-20261025T035216.740018600` 全绿（§2.1 T063 行）；stage、检查命令与任何测试均未改动 | 1 |

| 18 | 2026-10-25 | T064（干净克隆机制验证，克隆 `$LOCALAPPDATA/Temp/t64clone/basalt` @ `e2e285f`，日志 `t64clone/run.log`） | 干净克隆里 `make acceptance` 走到 `test` 失败：`--- FAIL: TestAccept_RejectsSourceChangedDuringChecks (0.49s)` → `acceptance full: failed exit=1`（此前 `fmt` / `lint` / `sec` 均 passed） | 旧版脚本用 `GIT_DIR=<repo>/.git GIT_WORK_TREE=<tmp>` 给「解档树无 `.git`」借身份，这两个变量被 `go test` 继承：`scripts/acceptance` 的夹具 `git init` / `git config` / `git add` 全部打到**被劫持的仓库**，临时仓库的变更因此不可见，`source_changed` 判据不触发。实证：克隆 `.git/config` 事后残留 `core.worktree=C:/…/tmp.myX5HefTG3`（已删除的临时树），说明夹具把配置写进了宿主仓 | 修实现不碰判据：临时树改 `git worktree add --detach <tmp> HEAD`（自带 `.git` 文件、**零环境变量**），并新增 `git archive HEAD` 解档树 + `diff -r --exclude=.git` **逐文件等价核验**（保留 C-18 的 archive 复现证据）。克隆内机制快检全绿（worktree status 0 行 / `DIFF_EMPTY=yes` / 1054 文件 / 清理正常）；主仓 `core.worktree` 复核为**未设置**（未受污染）。端到端复跑见 T064 补充读数 | 1 |

| 19 | 2026-10-25 | T069 前端探针（真实 API 实跑：本地临时候选实例 `ghost` / `relcmd` 两条配置） | `POST /api/v1/mcp-servers/ghost/test` 返回 `stage=spawn` + `failure_class=dependency_unconfirmed`，而颗粒度文档 §2.4.3 第 668 行要求「启动程序不存在 → `command_not_found`」；`relcmd`（`scripts\missing.cmd`）同样落 `dependency_unconfirmed` | 根因：`classifyInitializeFailure` 只在 `cmd.Process == nil` 时直接返回 `dependency_unconfirmed`，**从未**检查真实启动错误的类型；`dialCommand` 不返回 `cmd.Err`，且既有表驱动用例把 `*exec.Error{ErrNotFound}` 注入在 **dial 接缝**（`classifyDialFailure` 分支），真实 `cmd.Start()` 失败路径无任何断言 | 修实现不碰判据：`classifyInitializeFailure` 的 spawn 分支先认 `exec.ErrNotFound` → `command_not_found`；`fs.ErrNotExist` 须经 `commandFileMissing`（`os.Stat` 亲看命令文件缺失、且工作目录存在，相对入口按 `cmd.Dir` 解析）才归 `command_not_found`；`fs.ErrPermission` → `permission_denied`；其余仍 `dependency_unconfirmed`。红态先行（逐字）：`expected: "command_not_found" actual: "dependency_unconfirmed"`，两条子用例（裸名 / 绝对路径）同时红；修后 `go test ./internal/tool` 5.318s ok、`internal/app ^TestMCPTest` 8.438s ok、`internal/web ^TestMCP` 1.879s ok | 1 |
| 20 | 2026-10-25 | T069 浏览器走查（阶段 1，真实实例 `basalt-console/ws2`） | 每个 MCP 详情页的「修改MCP 服务」表单渲染 `Cannot read properties of undefined (reading 'join')`；`GET /api/v1/mcp-servers/{name}` 载荷无 `args` / `url` | 契约 `McpServerDetail`（US-11 起声明 = 视图 + `args` + `url`）与实现不一致：`internal/web/views.go` 的 `McpServerView` 无这两字段、app 详情端口只回视图；控制台 `assets/console/src/components/resource-form.ts:189` 直接 `view.args.join('\n')`（US-12 交付）即崩。单测假件自带 `args`（US-14 run-state T033 行「fixture 补 args / url」）因而从未暴露；openapi 的 `args` / `url` 未标 `required`，schema 校验也放行 | 用户裁决按契约补齐（2026-10-25，`AskUserQuestion` 选项 A）。修实现不改判据强度：`web.McpServerDetail` 增 `Args` / `URL`，handler 出站 `McpServerDetailView`（嵌入视图 + 两字段，`args` 恒 `[]`、ETag 仍只走响应头）；app 侧 `mcpServerView` 改为 `projectMCPServer` 返回投影结构（视图 + ETag + 发布定义），详情投影 `args` / `url`，列表视图不变；app 的 `resourceMCPDefinition` 补 `args` / `url` 解析。红态先行（逐字）：`TestResources_ViewShapesMatchContract/MCP_详情` → `+ (string) (len=4) "args"` / `+ (string) (len=3) "url"` 字段集差；修后 `go test ./internal/web` 4.643s ok、`go test ./internal/app` 36.670s ok。判据面变更见 §2.2 #11 | 1 |
| 21 | 2026-10-25 | T069 浏览器走查（阶段 2，真实实例 `basalt-console/ws2`，重建后的最终二进制） | `fake-empty-applied` 红：详情页「工具注册」期望 `已提供 0，已注册 0`、实际 `未连接，暂无注册记录`，而同页「连接」轴为「已连接」；同一时刻 `GET /api/v1/mcp-servers/fake-empty` 的 `connected=true`、`tool_disposition=null` | `mcpRuntime.ToolDisposition` 只查装配期来源表 `mcpDispositionTable.byServer`，而该表仅由 `recordMCP` **逐工具**写入——`tools/list` 成功但一个工具都没有时表里没有条目，查询返回 nil，视图按 D-06 的「未连接 / 未知」分支渲染成"未连接" | 修实现不改判据：`internal/app/mcp.go` 的 `ToolDisposition` 对**已连接**但无条目的 server 返回零计数对象（`Skipped: []core.MCPToolSkip{}`），未连接 / 未知 / 无来源表仍返回 nil。依据 D-06 原文（模块卡第 1184 行「已连接 ⇒ `offered = len(tools)`」、第 1686 行「已连接 server 必须有 `tool_disposition`」）——不是新增判据。新增 `TestMCPToolDisposition_ConnectedWithNoTools`；红态先行（`go test -overlay` 逆改旧实现，探针副本建在仓库外、用毕已删）：`--- FAIL: TestMCPToolDisposition_ConnectedWithNoTools` / `mcp_disposition_test.go:54: Expected value not to be nil.`，exit 1。修后读数：`go test -count=1 -run 'TestMCPToolDisposition' ./internal/app` → ok 2.261s；`go test -count=1 ./internal/app ./internal/web` → 36.682s / 4.090s ok；实跑 `/api/v1/mcp-servers` 的出站：`fake-empty` `{"offered":0,"accepted":0,"skipped":[]}`、`docs-readonly` `{"offered":3,"accepted":3,"skipped":[]}`；走查重跑 phase 1 14/14、phase 2 5/5 全 PASS（见 §2.1 T069 补充读数） | 1 |
| 22 | 2026-10-25 | T064 / T068（发布闸门 r1，报告 `.loop/runs/us14-release-gate`，后台 shell） | `error console-test (exit=2) executable unavailable: npm`；此前公共九项与本 stage `mcp-*` 等已 passed，工作区干净、候选 `11ef243` | 后台命令的 PATH 不含 Node——本机 Node 24.22.0 是**便携安装**（`%LOCALAPPDATA%/Temp/basalt-node/node-v24.22.0-win-x64`），前台历史运行手动带 PATH，后台 shell 未继承；不是实现缺陷，也不是 stage 判据问题 | 环境问题修前置再重跑，不改判据：同一 PATH 下先单独预检五个 npm 检查（console test 234 用例 / console build / website test / build / check 全绿，树保持干净），再以 `PATH=<便携 Node>:$PATH` 重跑全量 stage → `us14-release-gate-r2` 37/37 passed（§2.1）；r1 目录保留为失败证据，不发布记录 | 1 |
| 23 | 2026-10-26 | 发布闸门 r3（`.loop/runs/us14-release-gate-r3`，候选 `7c2595d`，37 项检查） | `failed e2e (exit=2)`：`TestUS14Demos_Fixture/industry_brief` 红，`us14_demos_test.go:4280` → `接收端累计收到 0 次投递，期望 1 次`；同轮其余 36 项 passed（含 `e2e-required`、`clean-checkout`） | 调度器先写执行行终态（`finishRun`，`internal/core/scheduler.go:443-446`）再发投递（`settleDelivery`，同文件 500-518），而 `waitCronRun` 只看任务 `last_status`（`internal/e2e/scheduler_test.go:923-939`）；测试在两者之间的窗口里数接收端 = 赌窗口已关闭 | 窗口真实且常被命中：给等待助手加「首读 ≠ `sent` 才打」的日志后，本地 3 轮 D5 有 1 轮首读为 `"pending"`（`Run scheduler-1792952461001-ez16g1 的代投行首次读数 "pending"（期望 "sent"）`）。修法：D2 / D5 三处计数前加有界等待（§2.2 #20），断言值一字未改。复跑 `--task T038` → `passed fmt` + `passed demo-fixtures`（7 条子测试全 PASS；最终源码版报告 `.loop/runs/us14-demo-fixtures-sync2`，签名简化前的一版 `…-sync` 同结论）；另单跑 D2+D5 三轮 6/6 PASS | 1 |

| 24 | 2026-10-26 | 发布闸门 r5（`.loop/runs/accept-20261025T215532.249140700`，候选 `452678c`，28 项检查） | `failed lint (exit=2)`：`examples/embed.go:18:1 SA9009`（`// go:embed …` 起头的注释被当成写错空格的编译指令）；`failed sec (exit=2)`：`gosec` 18 条——`internal/cli/demo.go` G301×6 / G306×6 / G304×1 / G703×1 / G204×2，`examples/shared/mcp/docs-readonly/main.go` G703×1 / G304×1 | ① 安装器（`4511c4e`）与 docs-readonly 并回主 module（2026-10-26 裁决）都发生在 r4 记录之后，这批代码从未跑过全量 `sec`；② G301/G306 是 0o755 / 0o644 字面量越过阈值（`internal/workspace` 早已用 0o750，因此从未被报）；③ G304/G703 是「拼出绝对路径再 `os.ReadFile` / `os.WriteFile`」这一形态，gosec 的 autofix 与仓库既有 `workspace.Init` 都指向 `os.Root`；④ G204 是运行期 argv（解释器路径与工作区脚本路径） | 修实现不改判据：① 注释改写为不以 `go:embed` 开头；② 目录 0o750 / 文件 0o600（与 `internal/workspace` 同口径或更紧）；③ `blankDemoModel` 与 docs-readonly 读路径改经 `os.OpenRoot` + `Root.Open` / `Root.OpenFile`（`serve` 持有根句柄，`resolveRoot` 用 `OpenRoot` 兼做目录校验），相应单测改为传根句柄；④ 解释器探针脚本提为常量 `demoPythonProbe`，仅剩两条无法用常量消除的 `#nosec G204`（用户 2026-10-26 裁决批准，理由逐条写进注释）。实测：`gosec -quiet ./...` exit 0（`#nosec` 25 → 27）、`go vet` / 显式 `-checks` staticcheck / `check-format.sh` / `govulncheck` 全零告警；`./internal/cli`、docs-readonly 单测与 `TestUS14DemoInstall_Artifact`（temp 二进制，`binary_sha256=14725fb2…`）全绿 | 1 |
| 25 | 2026-10-26 | 发布闸门 r5 启动（首次尝试） | 启动即 `exclusive harness lock unavailable`：`.loop/lock` 记 `pid=41968 started=2026-10-25T20:50:57Z`，对应 `accept-20261025T205057.050956100` | 上一轮 stage 进程被中断时未清理锁；PID 已不存在、也没有 acceptance 进程在跑 | 按 runner 自带提示核对 PID 与进程后删除过期 `.loop/lock` 再启动；被中断的 run 目录保留为失败证据，收口时清理 | 1 |

| 26 | 2026-10-26 | 发布闸门 r6 首次尝试（`.loop/runs/accept-20261025T220816.327122600`，候选 `4641c7c`，28 项检查） | `error console-test (exit=2) executable unavailable: npm`；此前 21 项 passed（fmt / lint / sec / test / test-integration / cover / build / eval-build / behavior / injection / go-build / contract-yaml / `mcp-*`×6 / notify-exclusion / mcp-admin），工作区干净 | 与 #22 同一根因：本机 Node 24.22.0 是便携安装（`%LOCALAPPDATA%/Temp/basalt-node/node-v24.22.0-win-x64`），不在 PATH；我这次用后台 shell 启动 stage 时又漏了 `PATH=<便携 Node>:$PATH`。不是实现缺陷，也不是判据问题——r4 记录里 `console-test` 是 passed（exit 0，32.4 s），证明带 PATH 时该死检查（含 console-build / website-test/build/check 五项 npm 检查）本就通得过 | 环境问题修前置再重跑，不改判据：核对便携 Node 仍在（`node --version` = v24.22.0、`npm --version` = 11.19.1，PowerShell 解析到 `npm.ps1`），`which npm` 在裸 PATH 下为空，随后以 `PATH=<便携 Node>:$PATH` 重跑全量 stage（`/tmp/us14-gate-r6.log`）；被中断的 run 目录保留为失败证据 | 2 |

**注（#22 / #26 复发，2026-10-26）**：#22 当时的处置是「同一 PATH 下重跑」，只修了那一次调用，没有把 PATH 写进 stage 启动模板，因此 #26 原样复发。后续所有 stage 启动命令一律写成 `PATH=<便携 Node>:$PATH powershell … check-acceptance.ps1 --stage …`，不再依赖调用时临时想起。

**注（#13 处置修正，2026-10-25）**：上表 #13 的「结果」写得像已执行改写，实际**未采用**——拟删除 stage 的 `acceptance` 包装属判据面变更，按 [EngineeringStandards §2](../../docs/EngineeringStandards.md) 单一口径须用户裁决；自动模式分类器亦按此拦截了改写后的任务判卷。`harness/stages/us14-release.json` 已恢复已审原文（复核：`approval` 长度 4511、检查尾部 `website-check / acceptance / clean-checkout`、T035 / T057 / T065–T068 / T070 仍绑 `acceptance`）。T057 改按 T035 同口径：以 `--profile full` 独立运行（与 `make acceptance` 同一 full profile、同一十项）作为证据。**2026-10-25 用户裁决采纳 §2.2 #8 方案**（记录于 §2.2 #10）：stage 内删除 `acceptance` 包装检查，改绑 `docs-consistency` / `stage-lint` / `release-records`，full profile 由 stage 整体运行执行。

### 2.1 任务验收读数（只追加；报告与日志在 `.loop/runs/<run_id>/`）

| 任务 | 日期 | run_id | 结论 | 本轮读数 |
| --- | --- | --- | --- | --- |
| T001 | 2026-10-25 | `accept-20261024T191720.717616400` | passed | `contract-yaml` |
| T002–T005 | 2026-10-25 | `accept-20261024T191912.457987500` / `…191920.583790000` / `…191926.846115600` / `…191933.155815600` | passed | `go-build`（逐任务各一次） |
| T006–T009 | 2026-10-25 | `accept-20261024T192213.404188300` / `…192218.713303000` / `…192224.260079900` / `…192229.859172300` | passed | `mcp-catalog`（四用例逐条 PASS） |
| T010 | 2026-10-25 | `accept-20261024T193841.610761900` | passed | `fmt` + `mcp-connect`；19 条具名测试 PASS（`TestMCPPool_ConnectDetailed` 15 子测试 + `TestMCPPool_Connect_LegacySemantics` + `TestNewMCPCommand_MatchesLaunch` 2 条） |
| T011 | 2026-10-25 | `accept-20261024T193850.685167600` | passed | `fmt` + `mcp-connect`；实现期红态：`pool.ConnectDetailed undefined` ×5 |
| T012 | 2026-10-25 | `accept-20261024T193857.473181900` | passed | `fmt` + `mcp-connect`；实现期红态：`undefined: NewMCPCommand` ×2 |
| T013 | 2026-10-25 | `accept-20261024T194706.178830500` | passed | `fmt` + `mcp-env-gate`；六条具名测试 PASS；实现前红态：5 条 `An error is expected but got nil` + 1 条类型不符（已先修正夹具，再取红态） |
| T014 | 2026-10-25 | `accept-20261024T195450.783680400` | passed | `fmt` + `mcp-diagnostics`；10 条具名测试 + 7 条子测试全部 PASS（零 SKIP）；实现前红态（编译级，逐字）：`mcp_diagnostics_test.go:69:30: env.admin.DiagnoseMCPServer undefined`、`:109:22` 同、`resource_admin_test.go:267:3: unknown field DiagnoseMCP in struct literal`、`:268:11: undefined: diagnoseMCPCommand`、`[build failed]`；实现后首轮 1 条红（见失败台账 #3），修实现后全绿 |
| T015 | 2026-10-25 | `accept-20261024T195709.170919600` | passed | `fmt` + `mcp-disposition`；5 条具名测试 PASS（`TestMCPToolDisposition_BuiltinWins` / `_TwoServers` / `_InvalidTool` / `_NilWhenUnknown` + `TestRegistry_DuplicateSemanticsUnchanged`，零 SKIP）；实现前红态（逐字）：`internal\app\mcp_disposition_test.go:25:32: undefined: mcpDispositionTable`；同轮完成 T023（`internal/app/mcp_disposition.go`）与 T024 的 `mcpRuntime.ToolDisposition` 半边，`mcpServerView` 投影仍未做（随 T024/T029 落地） |
| T016 | 2026-10-25 | `accept-20261024T202408.783107600` | passed | `fmt` + `mcp-test-record` + `notify-exclusion`；6 条 `TestMCPTest_*`（含 5 条 `_FailureClassification` 子测试）+ `TestNotifyMode_DeliverToRemovesNotify` 全 PASS，零 SKIP |
| T021 | 2026-10-25 | `accept-20261024T202431.454576900` | passed | `fmt` + `mcp-test-record`；`connectDetailed` 优先 `ConnectDetailed`、否则回退 `Connect` 并标 `spawn`/`dependency_unconfirmed`；超时按 `err` / `testCtx.Err()` / `failures[0].Class` 三路判定，stage 取结构化 failure |
| T022 | 2026-10-25 | `accept-20261024T202444.447285900` | passed | `fmt` + `mcp-test-record`；`RequestedVersionID` 在 `BeginOperation` 前设置；D-07 七字段恒定（`tools` 空为 `[]`）；超时与 env 缺失**不返回结果对象** |
| T017 | 2026-10-25 | `accept-20261024T204857.406056300` | passed | `fmt` + `mcp-contracts`；九个 `TestMCP*` 全 PASS（含 `TestMCPContracts_OperationTestDetail` 2 子测试），零 SKIP；`mcp-contracts` 的 8 条 `required_tests` 逐名命中 |
| T018 | 2026-10-25 | `accept-20261024T204906.509882300` | passed | `fmt` + `mcp-env-gate`；六条 `TestMCPEnvGate_*` 全 PASS |
| T019 | 2026-10-25 | `accept-20261024T204916.599593400` | passed | `fmt` + `mcp-env-gate`（同检查，覆盖四入口接线 + 幂等短路） |
| T020 | 2026-10-25 | `accept-20261024T204924.691694100` | passed | `fmt` + `mcp-diagnostics`；十条 `TestMCPDiagnostics_*` 全 PASS；`DiagnoseMCP` 在 `wire.go` 以 `diagnoseMCPCommand(spec, root, cfg.Shell.EnvPassthrough)` 注入 |
| T024 | 2026-10-25 | `accept-20261024T204935.194149700` | passed | `fmt` + `mcp-disposition`；`mcpServerView` 投影 `ToolDisposition`，`Tools` 保持 offered |
| T025 | 2026-10-25 | `accept-20261024T204943.014076700` | passed | `fmt` + `mcp-disposition` + `mcp-catalog`；三处接线（`recordBuiltin` / `recordMCP` / `mcpRT.dispositions`）+ `ResourceAdminDeps.DiagnoseMCP` + `web.Options.MCPCatalog`（`mcpcat.Builtin()` 解析一次、失败 fail-closed） |
| T026 | 2026-10-25 | `accept-20261024T204952.784320700` | passed | `fmt` + `mcp-contracts`；`DiagnoseMCPServer` 进端口、`MCPCatalogReader` 独立成接口、`Options.MCPCatalog` nil → 固定 500 |
| T027 | 2026-10-25 | `accept-20261024T205001.817923900` | passed | `fmt` + `mcp-contracts`；路由表 35 → 37 行（第 36 / 37 条，均 GET、无条件写），闭集与受保护标记不变 |
| T028 | 2026-10-25 | `accept-20261024T205008.066411000` | passed | `fmt` + `mcp-contracts`；两个只读 handler 零操作行；测试端点 503 / 504 带 `operation_id` 且不返回结果对象 |
| T029 | 2026-10-25 | `accept-20261024T205014.487716100` | passed | `fmt` + `mcp-contracts`；D-03 四字段、D-07 七键白名单、http `command=null`、空数组 `[]` |
| T030 | 2026-10-25 | `accept-20261024T205524.013717000` | passed | `fmt` + `console-test`；`api.ts` 新增 `MCPCatalog` / `MCPCatalogEntry` / `MCPCatalogEnv` / `McpServerDiagnostics` / `McpToolDisposition` 及其子类型，`McpServerView.tool_disposition` 可为 null、`McpServerTestResult` 追加 D-03 四字段；`api-catalog.ts` 两个只读 GET 仍走 `ApiClient` |
| T031 | 2026-10-25 | `accept-20261024T205747.112481600` | passed | `fmt` + `console-test`；`catalog-mcp` 路由（`catalog/mcp`，内部路由）+ `mcpCatalogHref()` + `views/catalog.ts`（七列表格 / 四筛选 / 配方详情 / 预填不提交 / warning code 文案表带未知 code 原样回显） |
| T032 | 2026-10-25 | `accept-20261024T210550.341697100` | passed | `fmt` + `console-test`；详情页四轴（配置 / 连接 / 上次测试 / 依赖诊断）各占一行、`待重启` 独立、`工具（已提供）` 与 `工具注册` 分列；测试详情以 `/operations/{id}` 为权威（POST 响应仅作标注兜底）；列表「未配置 → —」不写成「未连接」 |
| T033 | 2026-10-25 | `accept-20261024T210602.788256200` | passed | `fmt` + `console-test`；新增 `tests/catalog.test.ts` 六条（配方状态 / 筛选 / 详情边界 / 预填零写 / 名单读失败不称未配置 / warning 文案）与 `tests/resource-detail.test.ts` MCP 七条（四轴分离 / 诊断不预判 / 无被测版本 / 字段不全 / 版本过期 / 诊断布尔 / 未配置） |
| T033 | 2026-10-25 | `accept-20261024T210707.950363100` | passed | 复跑（导入修正后重取证据）：`McpServerDetail` 改从 `api-admin` 导入、fixture 补 `args` / `url`；`Test Files 21 passed` / `Tests 234 passed` |
| T034 | 2026-10-25 | `accept-20261024T210656.898702300` | passed | `fmt` + `console-build`；`vue-tsc` + `vite build` + `emit-integrity`（3 个文件）；连跑两次逐文件 sha256 一致（重建确定性）；`git status --porcelain -- assets/console/dist` 为 3 条待提交变更（`index-szB35Sqj.js` 删除、`index-C8YvltG4.js` 新增、`index.html` / `integrity.json` 修改），无 dist 之外的越界文件——"为空"只在提交后成立，本行如实记录 |
| T035 | 2026-10-25 | `accept-20261024T212832.299760100` | passed | `acceptance`（full profile）十项全 passed：`fmt / lint / sec / test / test-integration / cover / build / eval-build / behavior / injection`；同口径首轮 `accept-20261024T210756.509378300` 红在 `lint` / `sec`（失败台账 #4 / #5），修实现后复跑全绿；本轮改 `docs/TechnicalDesign.md` §8.1 / §8.1.1 / §8.1.2 与 §8.2 US-14 行、`docs/Requirements.md` §7.2 US-14 行、`docs/Roadmap.md` §6 M0 行，章节编号未动 |
| T036 | 2026-10-25 | `accept-20261024T214837.692827600` | passed | `fmt` + `docs-readonly-tests`；同口径首轮 `accept-20261024T214646.194629200` 红在 `docs-readonly-tests`——`required test skipped a subtest: TestDocsReadOnly_RejectsSymlink/real_symlink`（见失败台账 #6）；修订后 `go -C examples/shared/mcp/docs-readonly test -count=1 -json ./...` → 10 × `pass` / **0 × `skip`** / 0 × `fail`，退出 0 |
| T037 | 2026-10-25 | `accept-20261024T214847.433150500` | passed | `fmt` + `docs-readonly-tests`；七条用例 `TestDocsReadOnly_ToolNames` / `_ToolResults` / `_RejectsEscape` / `_RejectsAbsolutePath` / `_RejectsSymlink`（`component_check` + `real_filesystem`）/ `_ReadOnly` / `_ProtocolErrors` 全 PASS；`real_filesystem` 在本机走 junction 分支（`t.Logf` 读数：`真实重解析点机制：junction（escape）`） |
| T054 | 2026-10-25 | `accept-20261025T021243.062641900` | passed | `fmt` + `demo-fixtures` + `legacy-d1` 三项 `exit=0`；`demo-fixtures` 耗时 96.90s，`passed_tests` = 五个子测试 + 父测试（`github` / `repo_digest` / `biz_daily` / `docs_diff` / `industry_brief`），`skipped_tests` 为空；`legacy-d1` 耗时 10.99s，`TestLegacyD1_HttpEndpoints` PASS。本轮逐轮读数：① 首跑四红（失败台账 #8–#10）；② 第二轮仅 `repo_digest` 红在日报取值（#11）；③ 第三轮 `repo_digest` 红在 read_file 计数（#12），单跑复验 `--- PASS: TestUS14Demos_Fixture/repo_digest (19.26s)`；④ 本行 stage 全量复跑五子测试全绿 |
| T038 | 2026-10-25 | `accept-20261025T022553.577884500` | passed | `fmt` + `demo-fixtures`（`TestUS14Demos_Fixture` 及五子测试全 PASS，零 SKIP）；补跑取代早前一条"预期红态"读数（见补充读数） |
| T041 | 2026-10-25 | `accept-20261025T022734.100341600` | passed | `fmt` + `demo-fixtures` |
| T042 | 2026-10-25 | `accept-20261025T022915.118698700` | passed | `fmt` + `demo-fixtures` |
| T043 | 2026-10-25 | `accept-20261025T023056.046823100` | passed | `fmt` + `demo-fixtures` |
| T044 | 2026-10-25 | `accept-20261025T023237.071625600` | passed | `fmt` + `demo-fixtures`；首轮 `source_changed` exit=2（T055 新文件落在其运行窗口内），冻结源码后整套重跑 |
| T045 | 2026-10-25 | `accept-20261025T022401.295997100` | passed | `fmt` + `demo-fixtures`（`demo-fixtures` 96.65s） |
| T055 | 2026-10-25 | `accept-20261025T023417.134969000` | passed | `fmt` + `template-quality`；`required_tests` 逐名命中（`TestTemplates_LoadAndTools` / `TestTemplates_TierEvidence`），零 SKIP |
| T040 | 2026-10-25 | `accept-20261025T023435.485015000` | passed | `fmt` + `template-quality` |
| T046 | 2026-10-25 | `accept-20261025T023448.281647600` | passed | `fmt` + `template-quality` |
| T047 | 2026-10-25 | `accept-20261025T023501.205989800` | passed | `fmt` + `template-quality` |
| T048 | 2026-10-25 | `accept-20261025T023513.942941700` | passed | `fmt` + `template-quality` |
| T049 | 2026-10-25 | `accept-20261025T023526.476998500` | passed | `fmt` + `template-quality` |
| T050 | 2026-10-25 | `accept-20261025T023538.981918200` | passed | `fmt` + `template-quality` |
| T051 | 2026-10-25 | `accept-20261025T023551.687943600` | passed | `fmt` + `template-quality` |
| T052 | 2026-10-25 | `accept-20261025T023604.503340200` | passed | `fmt` + `template-quality` |
| T053 | 2026-10-25 | `accept-20261025T023616.802745300` | passed | `fmt` + `template-quality` |
| T056 | 2026-10-25 | `accept-20261025T023816.747519900` | passed | `fmt` + `script-tests`（五目录累计 87 个用例：9 / 13 / 19 / 22 / 24） |
| T039 | 2026-10-25 | `accept-20261025T023820.567343100` | passed | `fmt` + `script-tests`（生成器不在固定清单内，判据由 T054 的夹具测试调起生成器承担；本行只证 runner 建成后该检查可执行且绿） |
| T057 | 2026-10-25 | `accept-20261025T025527.099439300` | passed | `acceptance`（full profile）十项全 passed：`fmt` / `lint` / `sec` / `test` / `test-integration` / `cover` / `build` / `eval-build` / `behavior` / `injection`；耗时读数 1195 / 7040 / 8339 / 107360 / 46114 / 598166 / 554 / 2246 / 1823 / 546 ms（`cover` 占 598.2s，总 ≈773s） |
| T058 | 2026-10-25 | `accept-20261025T025437.896988400` | passed | `fmt` + `website-test`（`npm --prefix website test`：19 tests / 19 pass / 0 fail / 0 skipped，duration 3958ms；前置：便携 Node 以 `PATH=…basalt-node/node-v24.22.0-win-x64:$PATH` 注入，见失败台账 #15） |
| T059 | 2026-10-25 | `accept-20261025T031412.184590600` | passed | `fmt` + `website-test` + `website-build` + `website-check`（1219 / 4185 / 2576 / 700 ms；`npm --prefix website test` 20 tests / 20 pass / 0 fail / 0 skipped；同轮两报告指纹相同 `39adde24…40ac`，证明红态探查后源码逐字节复原，`accept-20261025T031306.313496200` 亦有效） |
| T061 | 2026-10-25 | `accept-20261025T031949.713326000` | passed | `fmt` + `e2e`（1239 / 207055 ms；`tested_binary_sha256=c12aa4cd…204a1`；指纹 `92ca219e…77ae2`；首跑失败与修复见失败台账 #16，失败报告不作证据） |
| T062 | 2026-10-25 | `accept-20261025T032637.263656300` | passed | `fmt` + `e2e`（1246 / 202216 ms；`tested_binary_sha256=55bb0e04…1eb32`；指纹 `521d5aa2…f9b5`；`e2e.log` 内 `binary_source=candidate` 恰好 1 条、`binary_sha256=55bb0e04…1eb32` 恰好 1 条，152 条具名测试 PASS；`make build` 产物与 `e2e` 检查用的是同一份 `bin/basalt.exe`） |
| T063 | 2026-10-25 | `accept-20261025T035216.740018600` | passed | `fmt` + `e2e` + `e2e-required`（1277 / 236883 / 214085 ms；第 3 项 81 条具名用例全 PASS，含新增 `TestReleaseE2E_Cancel` / `TestReleaseE2E_ToolTimeout` / `TestReleaseE2E_HotReload` / `TestReleaseE2E_SevenScenarioMap`；`tested_binary_sha256=55bb0e04…1eb32`；指纹 `dbdc0117…3e57d`；首跑 `source_changed` 见失败台账 #17，该报告不作证据） |
| T069 | 2026-10-25 | `accept-20261025T040225.441350800` | passed（**只判机器前置**） | `fmt` + `console-test`（Vitest 21 文件 / 234 用例全过，2.79 s；前置：便携 Node 注入 `PATH`）；任务正文的**浏览器人工走查**未做，见 T069 轮补充读数——`console-test` 是它的机器前置，不是它本身 |
| T065 | 2026-10-25 | `accept-20261025T123143.451474300` | passed | `fmt` + `stage-lint`（`exit=0`；`fmt` 1263 ms、`stage-lint` 497769 ms——超时值由 300 提到 1800 后通过，见 T065 轮补充读数）；`status=passed`、指纹 `d8bd8c6160cb0eca772cba67872e80f9d70dfbeb788e4771c9c9b8515fd1da5a`、`base_commit=e2e285fa45e67b6a595b29980e84634abf94f0b0`、`dirty=true`（本轮源码未再改动，脏来自此前已交付未提交的工作树） |
| T015 | 2026-10-25 | `accept-20261025T131045.329353500` | passed | 复跑（失败台账 #21 修复、源码变化后重取证据）：`fmt` + `mcp-disposition`，含新增 `TestMCPToolDisposition_ConnectedWithNoTools` |
| T024 | 2026-10-25 | `accept-20261025T131055.894841300` | passed | 复跑（同上）：`fmt` + `mcp-disposition` |
| T025 | 2026-10-25 | `accept-20261025T131103.183988000` | passed | 复跑（同上）：`fmt` + `mcp-disposition` + `mcp-catalog` |
| T069 | 2026-10-25 | `accept-20261025T131115.438925100` | passed（含浏览器走查） | 复跑机器前置：`fmt` + `console-test`；任务本体（浏览器自动化走查）读数见本段「T069 浏览器走查」补充读数——setup 7/7、phase 1 14/14、phase 2 5/5、三段 `console_errors` 均为 `[]`。本行取代早前「只判机器前置」那行的结论，tasks.md 已勾选 T069 |

补充读数（T061 轮，非 stage 判卷）：`Makefile` 增 `PYTHON ?= py -3`（本机 `python` / `python3` 是 Microsoft Store 占位符，`python --version` 之外一律 exit 49；`py -3` 实测 3.12.10）与三个目标：`.PHONY` 增 `example-tests` / `e2e`；`example-tests` = `go -C examples/shared/mcp/docs-readonly test -count=1 ./...` && `$(PYTHON) scripts/run-example-tests.py`；`e2e` = 依赖 `build example-tests`，再 `BASALT_E2E_BINARY=$(CURDIR)/bin/basalt go test -json -tags=e2e -count=1 ./internal/e2e`。三者（`example-tests` / `e2e` / `PYTHON`）都**不**进 `check` / `acceptance` / `acceptance.json` / CI。首跑实测：`make example-tests` 退出 0，嵌套 module 0.736s，五目录 87 个用例（9 / 13 / 19 / 22 / 24），无 `__pycache__` 残留。首轮 stage 验收失败读数与修复见失败台账 #16（`e2e` 检查报 `level process requires exactly one binary_sha256 marker, found 0`；`e2e` 本身 213.4s 已通过、`fmt` 通过）。修复后复跑 `accept-20261025T031949.713326000` 全绿：`fmt` 1239 ms、`e2e` 207055 ms，`tested_binary_sha256=c12aa4cd…204a1`，指纹 `92ca219e…77ae2`。**注意这条证据仍是 temp 构建态**：`make e2e` 已导出 `BASALT_E2E_BINARY`，但 T062 前 `TestMain` 尚不读它（仍走临时构建）——`binary_source=candidate` 断言属 T062 之后与 stage 判卷，本行不冒充。

补充读数（T062 轮，非 stage 判卷）：`TestMain` 增候选分支（`candidateBinaryEnv = "BASALT_E2E_BINARY"` / `candidateSHA256Env = "BASALT_E2E_SHA256"` / `binarySourceCandidate` / `binarySourceTemp` 四个常量）：`resolveBinaryUnderTest` 有候选环境时**不**调 `buildBinary`，直接 `checkCandidateBinary`（拒绝非绝对路径、缺失、非常规文件；重算 SHA-256，声明值与实际不符即失败）并打印 `binary_source=candidate` / `binary_path` / `binary_sha256`；无候选时保留原临时构建并打印 `binary_source=temp`。`expectedVersionFor` 按来源取期望值：temp = `us15-e2e`（原注入常量），candidate = 仓库内 `git describe --tags --always`（失败或空输出 fail-closed，**不**新增版本环境变量、**不**跳过该断言）。**实测命令与读数**：① `make build` → `bin/basalt.exe`（`go env GOEXE` = `.exe`），`./bin/basalt.exe --version` = `basalt version e2e285f`，与 `git describe --tags --always` = `e2e285f` 逐字一致；② `BASALT_E2E_BINARY="$(pwd)/bin/basalt.exe" go test -json -tags=e2e -run '^TestBackfill_Bootstrap$/^version_injection$'` → `binary_source=candidate`、`binary_path=D:/code/basalt/bin/basalt.exe`、`binary_sha256=55bb0e04…1eb32`、`--- PASS: TestBackfill_Bootstrap/version_injection (0.03s)`；③ temp 回退仍绿且哈希稳定 `c12aa4cd…204a1`（与 T061 同值，证明临时构建可复现）；④ 三条 fail-closed 红态逐字读数（均非零退出）：相对路径 → `e2e: 解析被测二进制失败: BASALT_E2E_BINARY 必须是绝对路径（实际 "bin/basalt.exe"）`；哈希声明不符 → `e2e: 解析被测二进制失败: 候选二进制哈希不符：BASALT_E2E_SHA256 声明 0000…，实际 55bb0e04…1eb32`；文件缺失 → `e2e: 解析被测二进制失败: 候选二进制不可用（D:/code/basalt/bin/nope.exe）: GetFileAttributesEx … The system cannot find the file specified.`。**两处 Windows 适配（实现细节，非判据面变更）**：ⓐ `Makefile` 用 `EXE ?= $(shell go env GOEXE)` + `BIN ?= bin/basalt$(EXE)`，`build` / `e2e` 都引用 `$(BIN)`——Windows 的 `os/exec` 只按 PATHEXT 解析带扩展名的目标，无扩展名的 PE 文件即使可执行也会报 `executable file not found in %PATH%`（本轮实测首跑即此错，`ls` 与 `file` 均证明 `bin/basalt` 是合法 PE32+）；Linux 上 `GOEXE` 为空，产物名仍是 `bin/basalt`，与模块卡 §3.8 原文一致。ⓑ `e2e` 目标改为把 `go test -json` 输出落临时文件：失败时原样 `cat` 后按原退出码退出，成功时 `cat` 后 `grep -q 'binary_source=candidate'`，缺失即以非零退出并说明"发布证据必须跑构建产物"——这条断言是模块卡 §6.11 第 7 条在 `make e2e` 侧的落地（工作区里 `acceptance` 工具只解析 `binary_sha256`，不认 `binary_source`，故断言放在目标里；`make -n e2e` 已核对展开，配方全 ASCII——本机 ezwinports make 按 ANSI 代码页读 Makefile，中文只能出现在注释里）。

补充读数（T058 轮，非 stage 判卷）：README 新增四节 `Templates & demos` / `MCP catalog` / `Run a demo` / `What is verified`，链接 `examples/README.md`（真实存在，`os.Stat` 判）与五个 `examples/daily/*/` 目录；`internal/cli/public_docs_test.go` 增两条子用例（`readme-examples` 正向、`readme-private-links` 拒绝 `career/` / `specs/` / `harness/` / `docs/us/` / `docs/interview/` / `.specify/` / `.claude/` / `CLAUDE*.md` / `.gitmessage` / 六份私有正文），`go test -count=1 ./internal/cli -run '^TestPublicDocs_UseAgentFlag$'` 五个子用例全 PASS（1.173s）。红态逐字读数（临时向 README 追加一条私有链接后跑该子用例）：`public_docs_test.go:76: README.md links to private document Requirements.md` → `--- FAIL: TestPublicDocs_UseAgentFlag/readme-private-links`；恢复后 README SHA-256 与备份一致（`45c23c1e…b7b6b0`）。markdownlint-cli2 v0.23.3：`Summary: 0 issues in 0 files`。网站在 T058 阶段未改（`website-test` 全绿证明 README 与既有内容规则不冲突）。

补充读数（T059 轮，非 stage 判卷）：新增 `website/.vitepress/data/templates.mjs`（`DEMOS` 5 / `TEMPLATES` 8 / `CATALOG` 6 / `TIERS` A·A\*·B / `TABLE_LABELS` 双语）与 `website/.vitepress/theme/components/TemplateTable.vue`（三块表 + 档位图例 + 未实测状态注；`repoLink` 先剥尾斜杠再拼 `https://github.com/ymedword/basalt/tree/main/<path>`）。两页 docs 索引各加 `## Templates & demos {#templates-demos}` 与 `## MCP catalog {#mcp-catalog}`（中英同名锚点），组件经页内 `<script setup>` 局部 import——`theme/index.ts` 属 US-13 已验收视觉文件不得改，故不做全局注册；两页 MD033 `allowed_elements` 各加 `TemplateTable`；未新增一级路由（`ROUTES` 仍 4 条、`ROUTES.length*2 === 8` 不变）。`content.test.mjs`：`CONTENT_FILES` 增两个新文件（文本 / 链接 / 时长 / 私有路径规则同时覆盖它们）；`checkPublicContent` 每语种新增 `missing-template-table-{demos,templates,catalog}` 规则；新增一致性测试（ids 与 `examples/daily|templates` 下含 `AGENT.md` 的目录逐条相等、`CATALOG` ids 与 `internal/catalog/data/mcp_catalog.yaml` 的 `- id:` 逐条相等、`path` 必须等于 `examples/<bucket>/<id>/`、每项 path 存在且 `isForbiddenPath` 为假、双语字段非空、档位恰好 6 A / 1 A\* / 1 B 且都在 `TIERS` 内）。`checks.test.mjs`：夹具新增「`examples/daily/github-radar` 的仓库 tree 链接不判错」，拒绝项新增 `docs/us/…`（`private-path`）与 `examples/../career/private.md`（`private-path`）。**`website/scripts/check-links.mjs` 生产代码零改动（有意，登记给 review ②）**：现状 `isForbiddenPath` 已允许 `examples/`、拒绝 `docs/`（除 Operations / contracts / assets）、`career|specs|harness|.specify|.claude` 与 `../`，`allowedExternal` 只放行 `github.com/ymedword/basalt*`——US-14 需要的三条行为本就成立，再写规则只会是死代码或改动既有页面判据面；故「允许 `examples/`、拒绝私有路径」由新增夹具锁定（新增测试，非判据面变更）。红态逐字读数：删掉 `website/zh/docs/index.md` 的 `<TemplateTable section="catalog" />` 后 `node --test website/tests/content.test.mjs` → `✖ public content passes all shared content and command checks`，`actual: [ { file: 'zh', reason: 'missing-template-table-catalog' } ]`；恢复后该页 SHA-256 前 16 位回到 `30a5f560f57c9108`，20 tests 全绿。另一条真实红态：新一致性测试首次运行即 `AssertionError: weather-brief: missing bodyEn`（模板条目本无 body 字段），说明断言在判数据形态，随后按"每类条目各自必填字段"收窄（Demo 补 body / evidence，模板补 domain），不是放宽到不判。渲染核对（`npm run build` 后 dist）：`docs/index.html` 含 `GitHub radar`、`https://github.com/ymedword/basalt/tree/main/examples/daily/github-radar`、`template-tiers`、`Recipe not exercised on this machine` ×1；`zh/docs/index.html` 含 `网页监测`、`该配方尚未在本机实测` ×1。SC-011 核对：`git status --porcelain -- website/.vitepress/theme/ website/public website/diagrams` 唯一命中是新增的 `TemplateTable.vue`；US-13 九个已验收视觉文件零差异（sha256 前 16 位 Layout `0be27db9c79aab26`、index.ts `6c06561b7b84f3b2`、HomeHero `a16650634c2d2bfb`、CapabilityGrid `cc665ce785fc6339`、CliDemo `a3921548ae6ff0ba`、DiagramFigure `b0fe7f5e7247fc32`、LanguageSwitch `8311a190e0800122`、SiteFooter `1ea4df7f7a9db175`、custom.css `ac86e1c493c0ba25`），`website/public/**` 与 `website/diagrams/**` 零改动。markdownlint-cli2 v0.23.3：两页 `Summary: 0 issues in 0 files`。**另记一条环境坑**：本机 `python` 在本会话中一律 exit 49 且无输出（`python --version` 之外的任何调用皆然），红态探针改用 `node -e` 完成——凡需改文件文本的探针不要用本机 `python`，T064 的脚本判卷若调 Python 须先复核该前置。

补充读数（同一轮，非 stage 判卷）：`go vet ./internal/tool` 退出 0；`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/tool` 退出 0；`gofumpt -l` 三个改动文件无输出。`tasks.md` 勾选 T001–T012（12 条），勾选位归一的正文指纹仍为 `786eb5c8…`（冻结输入未变）。

补充读数（T013–T014 轮，非 stage 判卷）：`go vet ./internal/app` 退出 0（实现后）；`gofumpt -w` 只作用于 `mcp_diagnostics.go` / `resource_admin.go` 两个本轮改动文件。`tasks.md` 勾选 T001–T014（14 条），勾选位归一后正文指纹复算仍为 `786eb5c8…`（冻结输入未变）。

补充读数（T015 轮，非 stage 判卷）：`go vet ./internal/app` 退出 0（实现后）；`gofumpt -w` 只作用于 `mcp_disposition.go` / `mcp.go` / `mcp_disposition_test.go`。`tasks.md` 勾选 T001–T015 + T023（16 条），勾选位归一的正文指纹复算仍为 `786eb5c8…`（冻结输入未变）。

补充读数（T016 / T021 / T022 轮，非 stage 判卷）：实现前红态（逐字，T016 先跑）：`resource_admin_test.go` 五条 `TestMCPTest_*` 断言失败——`stage` 期望 `spawn` 实得空串、`requested_version_id` 期望 9 实得 0、504 分支期望"无结果对象"实际仍返回 `core.ResourceTestResult` 对象；`go vet ./internal/app` 实现后退出 0；`gofumpt -w` 只作用于 `mcp_supervisor.go` / `resource_admin.go` / `mcp_supervisor_test.go` / `resource_admin_test.go` / `notify_mode_test.go`，`gofumpt -l` 五个文件无输出；`go test -count=1 ./internal/app/...` → `ok … 44.158s`。`tasks.md` 勾选 T001–T016 + T021–T023（19 条），勾选位归一后正文指纹复算为 `cbe006f3…`（与 §1.1 修订后的冻结值一致，冻结输入未变）。**注**：上面 T012 / T013–T014 / T015 三条补充读数引用的 `786eb5c8…` 是 §1.4 修订前的旧值，已由第 151 行作废；现行归一指纹为 `cbe006f3…`，文件哈希随勾选变为 `d1eb375a…`（基线快照 `efa371dd…` 保留不改）。

补充读数（T030 轮，非 stage 判卷）：**环境恢复**——本机 PATH 无 `node` / `npm`（US-11 的便携安装位于系统 Temp，已被清理），按 US-11 I-08 口径重新下载官方便携 Node v24.22.0 + npm 11.19.1 到 `$LOCALAPPDATA/Temp/basalt-node/`（仓库外、不入仓库、不入 PATH 持久配置），zip SHA-256 `f9683a19…f80c3` 与官方 `SHASUMS256.txt` 及 US-11 记录值三方一致。判卷时以 `PATH=<便携目录>:$PATH` 前缀执行，`console-test` 因此可解析 `npm`。读数：`npm --prefix assets/console run typecheck`（`vue-tsc --noEmit`）exit 0 无输出；`npm --prefix assets/console test` → `Test Files 20 passed (20)` / `Tests 221 passed (221)` / `Duration 20.86s`。`tasks.md` 勾选 T001–T030（30 条）；勾选位归一后正文指纹复算仍为 `cbe006f3…`（冻结输入未变）。

补充读数（T017–T020 / T024–T029 轮，非 stage 判卷）：`go build ./...` 退出 0；`go test -count=1 ./internal/web` → `ok … 4.906s`；`go test -count=1 ./internal/app` → `ok … 40.219s`；`go test -count=1 ./internal/catalog` → `ok … 0.602s`；`gofumpt -w` 只作用于 `internal/web/views.go` 与 `internal/web/handlers_admin_mcp_contracts_test.go`（`gofumpt -l` 本轮全部改动文件无输出，退出 0）。`tasks.md` 勾选 T001–T020 + T021–T029（29 条）；勾选位归一后正文指纹复算仍为 `cbe006f3…`（冻结输入未变），文件哈希随勾选变为 `96f60fea…`（基线快照 `efa371dd…` 保留不改）。

补充读数（T035 轮，非 stage 判卷）：`go vet ./...` 退出 0；`staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...` 退出 0；`gosec -quiet ./...` 退出 0（零命中）；`go build ./...` 退出 0；`go test -count=1 ./internal/app/ -run 'TestMCP'` → `ok … 8.429s`；`gofumpt -w` 只作用于 `internal/app/mcp_diagnostics.go` / `internal/catalog/catalog.go` / `internal/catalog/data.go` 三个本轮改动文件。`tasks.md` 勾选 T001–T035（35 条），勾选位归一后正文指纹复算仍为 `cbe006f3…`（冻结输入未变），文件哈希随勾选变为 `e7dce06e…`（基线快照 `efa371dd…` 保留不改）。

补充读数（T036–T037 轮，非 stage 判卷）：`go -C examples/shared/mcp/docs-readonly vet ./...` 退出 0；`gofumpt -w` 只作用于本轮新建的 `examples/shared/mcp/docs-readonly/main.go` / `main_test.go`（`gofumpt -l examples/shared/mcp/docs-readonly/` 无输出，退出 0）；嵌套 module 零依赖（`go.mod` 只有 `module` + `go 1.27` 两行，无 `require`，`go.sum` 不存在）；根 `go test ./...` 不覆盖它（嵌套 module，须单列 `go -C … test`）。**未用**任何抑制手段：`grep -rn "t\.Skip\|//nolint\|#nosec" examples/shared/mcp/docs-readonly/` 仅 1 条命中，且是 `main_test.go:365` 的一句注释（"不靠 t.Skip 换取绿态"），无任何实际 `t.Skip` 调用；`gofumpt -l` 无输出。`tasks.md` 勾选 T001–T037（37 条），勾选位归一后正文指纹复算仍为 `cbe006f3…`（冻结输入未变），文件哈希随勾选变为 `48e56afe…`（基线快照 `efa371dd…` 保留不改）。

补充读数（T038 轮，非 stage 判卷）：新增 `examples/shared/fixtures/docs/` 四份脱敏 Markdown——`handbook.md`（38 行）、`security-basics.md`（40 行）、`tooling.md`（32 行）、`teams/data-access.md`（32 行，落子目录以验证递归与 `/` 路径拼接），共 142 行；四份文件首行都标注"内容为虚构，不含真实人员、账号或凭据"。脱敏自查：`grep -rnE '@|\+?[0-9]{7,}|password|passwd|token|secret|api[_-]?key|sk-[A-Za-z0-9]|ghp_|Bearer' examples/shared/fixtures/docs/` 零命中（退出 1）。真实 server 读数（`go run . -root examples/shared/fixtures/docs`，stdin 送 5 条 JSON-RPC）：`list_documents` 返回 4 条且按 path 升序（`handbook.md` / `security-basics.md` / `teams/data-access.md` / `tooling.md`，子目录路径用 `/`）；`document_metadata teams/data-access.md` 返回 `size=1641` 与 `sha256=a3341425…`；`read_document ../README.md` 被拒（`path 不得包含 ..（越界拒绝）`）。固定哈希（供 T054 在 e2e 内钉死）：`handbook.md=0f2f9584…`、`security-basics.md=8b28568a…`、`tooling.md=51c0ff04…`、`teams/data-access.md=a3341425…`。**T038 本轮不勾选**：其 check `demo-fixtures` 绑定 `TestUS14Demos_Fixture`（T054 交付），现读数 `accept-20261024T215049.789390700` = `failed demo-fixtures (exit=0) required test did not execute and pass: TestUS14Demos_Fixture`——这是**预期红态**（断言面已冻结、实现未到），按「未通过不勾选」留到 T054 落地后补跑再勾。同类绑定的 T041–T045 亦按此处理。

补充读数（T041 轮，非 stage 判卷）：新增 D1 `examples/daily/github-radar/` 五个文件——`AGENT.md`、`README.md`、`SETUP.md`、`scripts/github_radar.py`、`scripts/test_github_radar.py`，加固定输入 `fixtures/search.json`（六条虚构仓库，sha256 `7117da7b…`）。业务脚本零第三方依赖（`argparse` / `json` / `hashlib` / `tempfile` / `datetime` / `pathlib`）；写序先落报告、成功后才原子替换状态（`tempfile.mkstemp` 同目录 + `os.replace`）；输入非法时非零退出且**一个字节都不写**。读数：① `cd examples/daily/github-radar/scripts && py -3 -m unittest` → `Ran 9 tests … OK`（首三个用例是模块卡 §4.5.10 原文，断言逻辑逐条保真）；② 夹具复跑（`--now 2026-10-25T08:30:00+08:00`，输出到仓库外临时目录）→ `baseline_missing=true`、无任何 `star_delta`、`counts {candidates:6, deduped:0, trimmed:1, recommended:5}`、五个推荐按星数降序、`lumen-labs/tiny-loader` 被裁剪但仍在 `state.observed` 里、`message=本次推荐 5 个仓库（候选 6，去重 0，裁剪 1）`、`orbitworks/fleet-scheduler` 的 `readme_url` 原样保留 `trunk` 分支；产物 SHA-256 输入 `7117da7b…` / 报告 `6d2ab04b…` / 状态 `001ad6f5…`。③ 红态（仓库外探针副本三处逆改，逐字读数）：去掉窗口去重 → `AssertionError: 'old/repo' unexpectedly found in ['old/repo', 'fringe/repo']`（连带 `{'candidates': 3, 'deduped': 0, 'trimmed': 1, 'recommended': 2} != {… 'deduped': 1, 'trimmed': 0 …}`）；首跑编造星差 → `AssertionError: 'star_delta' unexpectedly found in {… 'baseline_stars': 0, 'star_delta': 100}`；非法输入静默补默认 → `AssertionError: ScriptError not raised`——三只互不相同的逆改点各自被独立捕获，未逆改副本 9/9 绿。脱敏自查：`grep -rniE 'password|passwd|secret|access_token|api[_-]?key|bearer' examples/daily/github-radar/` 7 条命中，逐条核为环境变量**名**与 `${VAR}` / `$VAR` 占位（`GITHUB_PERSONAL_ACCESS_TOKEN`、`$BASALT_ADMIN_TOKEN`），零明文值；20 位以上裸长字符串扫描零命中；单测产生的 `scripts/__pycache__/` 已删除，仓库内零 `.pyc`。

**刻意决定（T041，与模块卡清单的差异与理由）**：① `tools:` 含 `shell`——卡里候选清单没有它，但图 2-8 主链写的是 `shell: python scripts/github_radar.py`，缺它 Demo 跑不起来（README 已写明来源与理由）；② `notify_mode: none` 时 `tools:` **不含** `notify`（§2.6 装配规则），SETUP 写明切 `allowlist` 后由保存路径补上；③ `resource_bindings` 两个数组均为空——模型连接 UID 只能由管理台保存路径写入，README / SETUP 都**禁止**手填；④ 测试 helper 固定 `top=2`：首跑必须裁掉一条"有基线但未被推荐"的仓库，第二次运行才存在同时"在窗口外"与"带基线"的条目，去重与星差因此在同一组输入里都可判（helper docstring 已写理由）；⑤ `runs/<时间戳>/` 由**脚本**创建（`write_file` 不建父目录，模型也拿不到自己的 `run_id`），与审计 Run 的关联走审计表而非目录名。

**T041 本轮不勾选**：其 check `demo-fixtures` 绑定 `TestUS14Demos_Fixture`（T054 交付），与 T038 同理按预期红态留待补跑。`AGENT.md` 的真实装载核验（`workspace.AgentLoader.LoadAll` + 注入 `Providers` / `Tools` / `MCPServers` / `MCPConnected`）一并并入 T054 的 `TestUS14Demos_Fixture`，本轮只做静态自查（frontmatter 逐项对照 `docs/contracts/agent-md.md`、`tools` 六项均为已知内置名或标注待实测的 MCP 名、`mcp_servers: [github]` 与 SETUP 的 `name` 逐字一致）。

**给 T056 的实现备注**：单测运行会在示例目录生成 `scripts/__pycache__/`（本轮已删）；`.gitignore` 无相应条目，而 T064 的 `check-release-clean.sh` 要求工作区干净——`scripts/run-example-tests.py` **必须**以 `-B`（或子进程环境 `PYTHONDONTWRITEBYTECODE=1`）运行，不靠 `.gitignore` 兜底。

补充读数（T042 轮，非 stage 判卷）：新增 D2 `examples/daily/repo-digest/` 八个文件——`AGENT.md`、`README.md`、`SETUP.md`、`scripts/repo_digest.py`、`scripts/test_repo_digest.py`、`skills/repo-digest/SKILL.md`（L2）、`skills/repo-digest/REFERENCE.md`（L3），加固定输入 `fixtures/pull_requests.json` / `fixtures/issues.json` / `fixtures/releases.json`（虚构仓库 `northwind-labs/mcp-bridge`；sha256 `b17ad886…` / `c1688f9c…` / `c2b012ef…`）。业务脚本零第三方依赖（`argparse` / `json` / `hashlib` / `tempfile` / `datetime` / `pathlib` / `os` / `sys`）；写序先落 `digest.json`、成功后才原子替换游标；输入或状态不合规时非零退出且**一个字节都不写**。读数：① `cd examples/daily/repo-digest/scripts && py -3 -m unittest` → `Ran 13 tests … OK`；② 夹具复跑（固定时钟 `2026-10-25T08:45:00+08:00`，输出到仓库外临时目录）→ 三源 `processed` 6 / 5 / 4、`held_back` 全空；PR `counts {open:2, draft_open:1, merged:1, closed_unmerged:1, unknown:1, with_unconfirmed_fields:1}`、Issue `{open:4, closed:1, with_unconfirmed_fields:1}`、Release `{draft:1, prerelease:1, published:1, unknown:1, with_unconfirmed_fields:1}`；`followups` 依序 PR #2 `stale_open`（15 天）→ Issue #13 `long_open`（54 天）→ Issue #12 `unanswered_open`（10 天）；`unconfirmed` 为 PR #6 `merged_at`、Issue #15 `comments`、Release #1004 `published_at` 三处 `missing`；`message=本次处理 PR 6 / Issue 5 / Release 4 项，待跟进 3 项，待确认 3 项`；`cursor.next` 三源 `2026-10-24T14:00:00+00:00` / `13:30:00+00:00` / `11:00:00+00:00`；产物 SHA-256 报告 `9773ebaf15701e6fe94036631aaa283330c6f4aa31c6b7468b2e0a67a6c8b836` / 状态 `8218ba5e176e4deaa7aba44e262d60289770972296dcc201fc640cdc92ca5072`（两次独立重跑逐字节一致）；③ 第二次同输入运行 → 三源 `processed=0`、`message=本次无新增：三个来源都没有晚于游标的新条目`、游标文件字节与上次一致（`advanced` 全 `false`）；④ 红态三例（仓库外探针副本逐字读数）：输入文件不存在 → `repo_digest: pull_requests 输入 不存在：…`、形状错误 → `repo_digest: pull_requests 必须是 JSON 数组，或 {"items": [...]} 形态的列表响应`、`schema_version=99` → `repo_digest: 状态文件 schema_version 必须是 1，实际 99`；三者均退出 1 且不创建输出目录、不改写游标。脱敏自查：`grep -rniE 'password|passwd|secret|access_token|api[_-]?key|bearer|ghp_' examples/daily/repo-digest/` 仅命中 `GITHUB_PERSONAL_ACCESS_TOKEN` 的环境变量**名**与 `${VAR}` 占位，零明文值；单测产生的 `scripts/__pycache__/`（早于 `-B` 的一轮）已删除，仓库内零 `.pyc`。

**刻意决定（T042，与模块卡清单的差异与理由）**：① `tools:` 含 `shell`——模块卡 §2.3 与 §916 要求脚本算数，缺它 Demo 跑不起来；② `notify_mode: none` 时 `tools:` **不含** `notify`（§2.6 装配规则），`deliver_to` 的代投路径整体推迟到 `SETUP.md` 第 6 节，按「建渠道 → 声明并绑定 → 任务选 `deliver_to`」三步写死（控制台下拉只列**已声明且已绑定**的渠道，顺序反了选不到）；③ `resource_bindings` 两个数组均为空——模型连接 UID 只能由管理台保存路径写入，README / SETUP 均**禁止**手填；④ 三份原始响应落在 Agent 根目录而非子目录——`write_file` **不创建父目录**，`digest.md` 才写在脚本刚建好的 `runs/<时间戳>/` 里；⑤ 日报正文由模型写、机器可读结果由脚本写，两者同目录但**权限与来源分离**（`scripts/` 对模型只读）；⑥ 技能 L1 卡片路径写 `.basalt/skills/repo-digest/SKILL.md`（运行时发布副本路径），不是示例目录里的源文件。

**T042 本轮不勾选**：其 check `demo-fixtures` 绑定 `TestUS14Demos_Fixture`（T054 交付），与 T038 / T041 同理按预期红态留待补跑。`AGENT.md` 的真实装载核验（`workspace.AgentLoader.LoadAll` + 技能声明实体与已发布版本校验）与技能 L1 卡片 / `read_file` 审计一并并入 T054 的 `TestUS14Demos_Fixture/repo_digest`（需要真实模型，本轮未跑）。

补充读数（T039 轮，非 stage 判卷）：新增 `examples/shared/scripts/make_sample_db.py`（150 行，纯标准库 `sqlite3`；`--schema` / `--rows` / `--out` / `--force`）。行为：校验 `{"table","columns","rows"}` 形态、拒绝未声明列、目标已存在且无 `--force` 时拒写、`executescript` 后验证表已建、缺键按 SQL NULL 入库、验证入库行数，最后打印 db / schema / rows 三项路径与 SHA-256 的 JSON 摘要。实测（`$LOCALAPPDATA/Temp/biz-db/`，仓库外）：24 行全部入库；`biz-sample.db` sha256 `de3a167d37c828ed12d3c79486853184c9901f289b4be360ebbb0f96795ba235`；同一命令第二次生成到 `biz2.db` **逐字节一致**（同 sha256，本机）；无 `--force` 重跑同路径 → exit 1、`make_sample_db: 目标库已存在（加 --force 覆盖）: …`、原库字节不变。SQL 直查（证明三类异常确实入库、没被约束或清洗掉）：`region IS NULL` 1 行；`typeof(amount)` 呈 `[('text', 'N/A'), ('real', -300.0), ('null', None)]` 三态；重复单号 `SO-1002` ×2 / `SO-1003` ×2；`status` 含空串 1 行；`order_date` 保留 `'2026-13-40'`。输入指纹：`fixtures/schema.sql` = `2136d7c0589f4fcb9a3f39f6f4e3c583221ab520343b8836b9e02f64d590ef53`、`fixtures/rows.json` = `d64d446df5c4ccf4044fc01999ab05e71de37a0f97cff817d439ac7d4f6cfaf6`。**覆盖落点**：`script-tests` 的固定目录清单是五个 `examples/daily/*/scripts`（T056 的任务正文不动），生成器不在其中；它的通过性由 T054 的 `TestUS14Demos_Fixture` 调起生成器并断言库内容承担（已写入 T054 的实现备注）。

**刻意决定（T039）**：① 生成器放 `examples/shared/scripts/` 而不是某个 Demo 内——D3 与模板 `ticket-insights` 共用同一份 schema + rows 形态（模块卡 §3.7）；② `.db` 不提交：样例库是生成物，源是 `fixtures/schema.sql` + `fixtures/rows.json`；③ **不做数据清洗**——三类异常就是被测对象，清洗掉等于把 Demo 的证据面抹平；④ 跨机器字节一致性**不承诺**（SQLite 版本 / 平台差异未验证），README 与证据口径都写「引用你自己生成的库哈希」。

补充读数（T043 轮，非 stage 判卷）：新增 D3 `examples/daily/biz-daily/` 七个文件——`AGENT.md`（110 行）、`README.md`（302 行）、`SETUP.md`（181 行）、`scripts/biz_daily.py`（464 行）、`scripts/test_biz_daily.py`（219 行），加固定输入 `fixtures/schema.sql` / `fixtures/rows.json`。脚本零第三方依赖（`argparse` / `hashlib` / `json` / `os` / `sys` / `tempfile` / `datetime` / `decimal` / `pathlib`）；金额全程 `Decimal`、落盘两位小数；四类互斥且优先级固定 `missing > duplicate > invalid`；`computed=false` ⇒ 金额 `null`（**不是** `0.00`）；写序为同目录临时文件 + `os.replace`，输入非法时一个字节都不写、连输出父目录都不创建；显式 `--now` 无时区偏移 → exit 1。读数：① `py -3 -B -m unittest discover -s scripts -p 'test_*.py'` → `Ran 19 tests in 0.025s … OK`，零 `t.Skip`；② 夹具复跑（固定时钟 `2026-10-25T09:00:00+08:00`，`--orders fixtures/rows.json`）→ `quality.total=24`、`counts {valid:15, missing:3, duplicate:2, invalid:4}`（和 == 24）；`missing.by_field {amount:1, region:1, status:1}`；`duplicate.order_ids ["SO-1002","SO-1003"]`（首次分别第 2 / 3 行）；`invalid.by_reason {amount_not_numeric:1, amount_negative:1, status_unknown:1, order_date_unparseable:1}`；`metrics`：`status_counts {paid:9, refunded:3, pending:3}`、毛额 `11890.5` / 退款 `1190.0` / 挂起 `2170.0` / 净额 `10700.5` / 客单价 `1321.17`（CNY）；`by_region` 华东 6 单 / 3740.0、华北 5 单 / 4290.5、华南 2 单 / 1990.0、西南 2 单 / 680.0（分区净额之和 == 总净额）；`message=本次 24 行中有效 15 行（缺失 3 / 重复 2 / 非法 4），净额 10700.50 CNY`；报告 sha256 `67814bc79325d095ae812c74fa8788c209b664cdf82b8baebba226fc4a61c876`（同一 `input.path` 与时钟下两次独立重跑逐字节一致；**换 `--orders` 路径会改哈希**，因 `input.path` 进报告）；③ `--max-examples 1` → `quality.truncated=true`、`truncation {missing:{shown:1,total:3}, duplicate:{shown:1,total:2}, invalid:{shown:1,total:4}}`，`counts` 不变；④ 红态三例（仓库外探针副本逆改，逐字读数）：缺失补 0 → `KeyError: 'amount_not_numeric'`；关闭去重 → `- []` vs `+ ['SO-1002', 'SO-1003']`；静默截断不标记 → `AssertionError: False is not true`——三只互不相同的逆改点各自被独立捕获，未逆改副本 19/19 绿；⑤ 失败样例逐字读数：`biz_daily: 读取查询结果失败: missing.json: [Errno 2] No such file or directory: 'missing.json'`、`biz_daily: 查询结果必须是行数组，或 {"rows": [...]} / {"items": [...]} 形态的列表响应`、`biz_daily: 第 1 行不是对象，实际 str`、`biz_daily: --now 必须是带时区偏移的 ISO 8601 时间戳: '2026-10-25T09:00:00'`、`biz_daily: --max-examples 必须 ≥ 0，实际 -1`；均 exit 1 且不创建输出目录。脱敏自查：`grep -rniE 'password|passwd|secret|access_token|api[_-]?key|bearer|ghp_' examples/daily/biz-daily/` 仅命中 `SETUP.md` 的 `$BASALT_ADMIN_TOKEN` 环境变量名（调用示例，无值），零明文凭据；无 `__pycache__` / `.pyc`。

**刻意决定（T043，与模块卡清单的差异与理由）**：① `tools:` 用候选 DBHub 工具名 `run_query` / `list_tables`（`verified:false`，README / SETUP 都写明连上后按 `tools/list` 校正）+ 内置 `read_file` / `write_file` / `shell`；`shell` 缺了 Demo 就跑不起来（数字必须由脚本算）；② `notify_mode: none` 时 `tools:` **不含** `notify`（§2.6 装配规则）——零通知是本 Demo 的**完成判据**，SETUP §7 明确「不要建渠道、不要改 notify_mode」；③ `resource_bindings` 两个数组为空——模型连接 UID 只能由管理台保存路径写入，README / SETUP 均**禁止**手填；④ 查询结果落 Agent 根目录 `orders.json` 而非子目录——`write_file` **不创建父目录**；`daily.md` 写在脚本刚建好的 `runs/<时间戳>/` 里；⑤ 固定 SQL 写进 `AGENT.md` 并写明理由（加 `WHERE` / 只取正常列会让三类异常直接消失，检查就永远报 0）；⑥ D3 没有技能，日报模板直接写在 `AGENT.md` 步骤 5（D2 靠技能 L2）——模板不依赖未发布的技能也能用；⑦ `SETUP.md` 的 `allowed_commands` 只放 `node`：stdio MCP 启动前同样过命令授权（`internal/app/mcp_supervisor.go` 的 `authorize` 按 `SHELL_COMMAND` 判 `cfg.Command`），漏放的表现是 `mcp server skipped: dbhub`。

**T043 本轮不勾选**：其 check `demo-fixtures` 绑定 `TestUS14Demos_Fixture`（T054 交付），与 T038 / T041 / T042 同理按预期红态留待补跑。`AGENT.md` 的真实装载核验、`run_query` 实际调用与零通知取证一并并入 T054 / 14B 人工验收；本轮只做静态自查（frontmatter 逐项对照 `docs/contracts/agent-md.md`、`mcp_servers: [dbhub]` 与 SETUP 的 `name` 逐字一致、`tools` 五项均为已知内置名或标注未实测的 DBHub 名）。

补充读数（T044 轮，非 stage 判卷）：新增 D4 `examples/daily/docs-diff/` 五个文件——`AGENT.md`（127 行）、`README.md`（204 行）、`SETUP.md`（206 行）、`scripts/docs_diff.py`（477 行）、`scripts/test_docs_diff.py`（317 行），加单测夹具 `fixtures/docs/a.md`（719 B，sha256 `8826189d…`）/ `b.md`（565 B，`3dd24d0a…`）；`.gitignore` 追加 `examples/daily/docs-diff/state/` 与 `examples/daily/docs-diff/runs/`。脚本零第三方依赖（`argparse` / `hashlib` / `json` / `os` / `sys` / `tempfile` / `datetime` / `pathlib`）；变更判据**只认内容哈希**，`size` / `modified_at` 只做 list 与 metadata 两份快照的同源核对、不参与判定；首跑只记基线（**无** `changes_detected` 键、四类集合全空）；无变化也写报告；写序先报告后状态、同目录 `mkstemp` + `os.replace`，输入非法时一个字节都不写、不建输出父目录。读数：① `py -3 -B -m unittest discover -s examples/daily/docs-diff/scripts -p 'test_*.py'` → `Ran 22 tests in 0.042s … OK`，零 `t.Skip`；② 真实 `docs-readonly`（`go -C examples/shared/mcp/docs-readonly build` 到仓库外）经 stdio 驱动三轮：首跑 `mode=baseline_created`、4 份全部 `baseline`、无 `changes_detected`、报告 sha256 `584b6638…`、状态 `fa0b484f…`；同内容第二轮 `mode=compared`、未变 4、`changes_detected=false`、报告 `7a345be3…`、状态 `08b3185c…`；变更演练（追加 `tooling.md`、删 `teams/data-access.md`、加 `teams/faq.md`）→ 新增 `["teams/faq.md"]`、修改 `["tooling.md"]`、删除 `["teams/data-access.md"]`、未变 `["handbook.md","security-basics.md"]`、报告 `c4671cc8…`、状态 `d68755c8…`；`document_metadata` 的 sha256 与磁盘 `sha256sum` 逐份相等；③ 红态四例（仓库外探针副本逆改，逐字读数）：首跑编造变更 → `AssertionError: {'added': ['a.md', 'b.md'], 'modified': [], …} != {'added': [], …}`；改按 `size` 判变更 → `AssertionError: False is not true`（同字节数内容变化被漏判）；无变化不写报告 → `FileNotFoundError: [Errno 2] No such file or directory: '…\runs\second\changes.json'`；报告携带输入 → `AssertionError: 'inputs' unexpectedly found in {…}`——四只互不相同的逆改点各自被独立捕获，未逆改副本 22/22 绿；④ 失败样例逐字读数（`rc=1`、零写入）：`docs_diff: metadata 缺少文档的元信息：…`、`… metadata 多出清单外的文档：…`、`… 文档 handbook.md 的 size 在 list 与 metadata 之间不一致：1717 != 1718 …`、`… sha256 不是 64 位小写十六进制：'ABC'`、`… 状态文件 schema_version 必须是 1，实际 2`、`… list_documents 第 1 项的 path 含空段 / . / ..：'../a.md'`；只读 MCP 工具级拒收（`isError=true`）：`path 不得包含 ..（越界拒绝）: ../outside.md`、`文档不存在: missing.md`。脱敏自查：`grep -rniE 'password|passwd|secret|access_token|api[_-]?key|bearer|ghp_' examples/daily/docs-diff/` 仅 1 条命中，是 `SETUP.md` 调用示例里的环境变量**名** `$BASALT_ADMIN_TOKEN`（无值）；探针目录已删除，仓库内零 `__pycache__` / `.pyc` / 临时二进制。**T044 本轮不勾选**：其 check `demo-fixtures` 绑定 `TestUS14Demos_Fixture`（T054 交付），与 T038 / T041–T043 同理按预期红态留待补跑；`AGENT.md` 的真实装载核验与 `docs-readonly` 真实连接、（零历史会话下的）`recall_memory` / `save_memory` 取证一并并入 T054。

**刻意决定（T044，与模块卡清单的差异与理由）**：① `tools:` **不含** `notify`——基线 `notify_mode: none` 时 `internal/app/notify_assembly.go` 的 `AssembleNotifyTool` 会把它从工具面移除（§2.6 装配规则），手写进去反而与装配结果不一致；`SETUP.md` §7 写明切 `allowlist` 后由保存路径经 `changes.tools_added` 加回，验收口径「每次运行含无变化恰好一条」落在开启之后；② 跨运行基线 `state/baseline.json` 由脚本写、`.gitignore` 忽略，`SETUP.md` 明确复制 Agent 目录时**不带** `state/` 与 `runs/`，否则首跑不再是 `baseline_created`；③ 零历史会话验证做成可执行步骤：清 `messages` 而保留 `memory_entries` 与 `state/`，再跑一轮要求 `recall_memory` 成功且报告「记忆基线」引用上一轮 `save_memory` 原文——调度 Run 共用按 agent 派生的持久会话（`core/scheduler.go` 的 `DeriveSessionID("scheduler","scheduler",task.Agent)`），只看「新会话」构造不出来；④ 记忆条目固定以 `docs-diff 基线 <observed_at>` 开头并写计数 / 集合 / `state.sha256` / 报告路径——`recall_memory` 是**大小写敏感的子串匹配且只查 ARCHIVAL**（CORE 不参与检索），query 固定 `docs-diff`，SETUP 故障表写明「写成 CORE 就永远查不到」；⑤ `max_iterations: 20`：每份文档一次 `document_metadata` + 取数 + 读数 + 写报告 + 通知，默认 10 不够；⑥ 只对新增 / 修改的文档 `read_document`，删除只列路径与旧哈希——少读无关正文，也避开「文档已不在还去读」的失败路径；⑦ 新增 `test_same_size_change_is_still_modified`（末尾换行换成空格：字节数不变、内容不同，必须判修改），把 README 里「只改一个空白也算修改」的承诺变成机器判据；属**新增**判据，不是放宽（旧 21 条全保留）。

补充读数（T045 轮，非 stage 判卷）：新增 D5 `examples/daily/industry-brief/` 六个文件——`AGENT.md`（157 行）、`README.md`（288 行）、`SETUP.md`（217 行）、`scripts/industry_brief.py`（439 行 / 18582 B）、`scripts/test_industry_brief.py`（322 行 / 15620 B），加两份单测夹具 `fixtures/search.json`（3522 B，sha256 `277a7a8b2e68fad7adfcd6ddb950b90df355a6ece8db86c9923a2b5726b4f039`）/ `fixtures/pages.json`（5027 B，`e4196b4b350834064609a04f8abeb936d85d727aca8cc6684409c8bf2bbed7c7`）；`.gitignore` 追加 `examples/daily/industry-brief/runs/`。脚本零第三方依赖（`argparse` / `hashlib` / `json` / `os` / `re` / `sys` / `tempfile` / `datetime` / `pathlib` / `urllib.parse`）；可引用性**只认正文落盘**（`status=fetched` 且正文非空 → `citable=true`；未抓 / 抓失败 / 空正文 → `lead`，只作线索），`--verify-report` 只允 `[来源: <url>]` 指向 `citable_urls`，URL 去重按去掉 `#` 片段后比较。读数：① `py -3 -B -m unittest discover -s examples/daily/industry-brief/scripts -p 'test_*.py'` → `Ran 24 tests in 0.039s … OK`，零 `t.Skip`；② CLI（夹具 + `--now 2026-10-25T09:30:00+08:00` + `--topic 企业级 AI Agent 平台`）：`counts={queries:3, results:10, sources:8, duplicates:2, fetched:5, leads:3, fetch_failures:1, missing_dates:1}`，`message` 原文「本次检索 3 个查询，命中 10 条结果（去重 2 条），来源 8 个：已抓正文 5 个、仅线索 3 个」，`citable_urls` 5 条（analysis/audit-trail-design、blog/runtime-cost-controls、blog/self-hosted-agent-runtime、guides/tool-sandbox-patterns、reports/agent-platform-observability-2026），`lead_urls` 3 条（news/enterprise-agent-adoption、news/sandbox-incident-review、vendor/agent-os-comparison），`report.sha256` = `86dbe1dd26960353a2b890aab0d4ec585d30ef3d59349923081c96847f2db170`（跨两次会话独立重跑同值）；同输入同 `--now` 两次逐字节相同、换 `--now` 即不同（`test_report_is_reproducible`）；③ 引用自检三次读数：合法引用 → `rc=0`、stderr 空；引用线索 → `rc=1`、`industry_brief: 引用了未抓正文的 URL（只能作线索）：https://example.com/news/enterprise-agent-adoption`；全篇无引用 → `rc=1`、`industry_brief: 简报没有任何「[来源: <url>]」标注：结论必须逐条可追溯到已抓正文的来源`（自检失败仍先原子写出 `brief.json`，只 stdout 为空）；④ 输入拒收逐字读数（`rc=1`、五例均零写入、不建输出父目录）：`…pages 第 1 项的 URL 不在检索结果里：https://example.com/other（混入了别的 Run？）`、`…pages 里同一 URL 出现多次：https://example.com/a`、`…查询 'q1' 的第 1 条结果的 url 必须是 http(s) 绝对地址：'file:///etc/passwd'`、`…查询 'q1' 的第 1 条结果的 url 不得包含用户名或密码：'https://user:secret@example.com/a'`、`…Brave 检索结果缺少 queries 数组`；单测另逐字断言 `…查询 'q1' 缺少 results 数组`、`抓取失败：HTTP 404 Not Found`、`抓取失败：正文为空`；⑤ 红态三例（仓库外探针副本逆改，逐字读数）：`citable` 恒真 → 5 条失败（`test_unfetched_urls_are_leads_not_citable` / `test_fetch_failure_stays_a_lead` / `test_empty_body_is_a_fetch_failure` / `test_verify_report_rejects_lead_citation` / `test_cli_verify_failure_exits_nonzero_after_writing_brief`）；取消跨查询去重 → 5 条失败（`test_fixture_counts_are_conserved` 报 `AssertionError: 10 != 8`，另 `test_dedupe_across_queries_merges_sources` / `test_dedupe_ignores_fragment` / `test_fetched_sources_are_citable_with_access_time` / `test_cli_writes_brief_json_and_prints_summary`）；自检放过线索 → 2 条失败（`test_verify_report_rejects_lead_citation` 报 `AssertionError: 0 != 1`、`test_cli_verify_failure_exits_nonzero_after_writing_brief`）——三只互不相同的逆改点各自被独立捕获，未逆改副本 24/24 绿；⑥ 脱敏自查：`examples/daily/industry-brief/` 仅变量名 / 占位符（`BRAVE_API_KEY`、`${BRAVE_API_KEY}`、`$BASALT_ADMIN_TOKEN`）与拒收用例里的伪造凭据（`https://user:secret@example.com/a`、`file:///etc/passwd`），零真实值；探针目录（`%LOCALAPPDATA%/Temp/ib-probe*`）已删净，仓库内零 `__pycache__` / `.pyc` / `runs/`；⑦ 三份 markdown 经 `markdownlint-cli2 v0.23.3`（便携 Node 24.22.0 + 仓库 `.markdownlint.json`）→ `Summary: 0 issues in 0 files`、`exit=0`。

**刻意决定（T045，与模块卡清单的差异与理由）**：① `tools:` **不含** `notify`——基线 `notify_mode: none` 时装配会剔除（§2.6），`deliver_to` 的代投路径整体写进 `SETUP.md` §6 三步（建渠道 → 声明并绑定 → 任务选代投），下拉只列已声明且已绑定的渠道，顺序反了选不到；② 便携基线**不写** `deliver_to`（D2 先例，FR17：基线不含真实 UID；`deliver_to` 的合法值是 `rs_` 前缀的 `resource_uid`，不写渠道名）；③ `max_iterations: 30`——三条 Brave 查询 + 逐 URL Fetch + 落盘 + 脚本 + 读回 + 写简报 + 自检，每段至少一轮，D1–D4 的 12–20 不够；④ `SETUP.md` 写明 Fetch 配方的 `-m` 是**服务端入口方式**、不走 `shell` 解释器通道（`shell.interpreters` 只约束 `shell` 工具的 argv，必须 `[<脚本路径>, …]`），MCP stdio 启动只受 `shell.allowed_commands` 约束——两者混谈会把排障引向错误方向；⑤ Microsoft Store 的 `python` 是占位程序，提示在 **MCP `command`** 与 **`shell.interpreters`** 两处都写（改 `py` 时两处必须同一串）；⑥ 自检复用同一 `--now` 与 `--topic`——`observed_at` 进 `brief.json`，换 `--now` 会破坏「同输入同输出」的重写一致性；⑦ 把 `[来源: <url>]` 写法收进反引号 / 代码块（`AGENT.md` 第 9 步、`SETUP.md` §11 表格）——MD033 会把裸 `<url>` 当内联 HTML 拒绝；属格式化修正，不改语义。

**T045 本轮不勾选**：其 check `demo-fixtures` 绑定 `TestUS14Demos_Fixture`（T054 交付），与 T038 / T041–T044 同理按预期红态留待补跑；真实 Brave / Fetch 连接（含按 `tools/list` 校正候选工具名）与真实模型 Run 的六段证据一并并入 14B 人工验收（外部服务全未授权），`AGENT.md` 的装载核验并入 T054。

补充读数（T040 轮，非 stage 判卷）：`examples/README.md`（111 行，sha256 `a4ab212b…`）为英文总览（公开面随根 README 的英文单语口径），含五 Demo 表（产物 / 外部服务 / 仓内证据）、八模板表（领域 / 档位 / 依赖）、A / A\* / B 档位定义、运行前置、如何跑一次、已验 / 未验两类主张、共享素材、出网边界、`make example-tests` / `make e2e` 两个新入口。本轮对初稿做了三处**事实对齐**（不是新增判据）：① 前置表把 Fetch 从「Node + npm」行拆出——`mcp_catalog.yaml` 的 `fetch` 配方是 `python -m mcp_server_fetch`（Python 包，不是 npm 包）；② 删掉 `make_sample_db.py`「原子写」的说法——实现是 `unlink` 后直接 `sqlite3.connect(out)` 建库（[make_sample_db.py:81-121](examples/shared/scripts/make_sample_db.py)），只承诺「已存在必须 `--force` 才覆盖」；③ Demo 证据列收窄到夹具真能证明的范围——`docs-diff` 去掉「记忆基线」（属真实 Run 证据）、`industry-brief` 的「两个 MCP 都被调用」改标 release e2e（T054 断言）。核对：链接全部指向 `examples/**` 内已存在路径（`templates/**` 由 T046–T053 落地后补齐）；`grep -nE 'docs/us|docs/interview|career/|specs/|harness/|CLAUDE'` 零命中；demo 脚本零网络调用（`grep urlopen|requests\\.|http\\.client` 仅命中 docstring 里的 `pull_requests.json`）；`.markdownlint.json` 判卷 `Summary: 0 issues in 0 files`、`exit=0`。

**刻意决定（T040）**：① 英文——本文件与根 README 同属公开面，五个 Demo 的 `README.md` / `SETUP.md` 仍是中文工程文档（面向复制后自查的使用者）；② 不重复各 Demo 的 14 项固定内容，只给总览与入口，细节由各自 `README.md` / `SETUP.md` 承担；③ 「已验 / 未验」两类主张分开写，并写明「连接成功」既不算夹具复跑也不算真实运行（模块卡 C-14 / §1.6 的档位纪律）；④ 「仓内证据」列只写**仓内已经能证明**的项，真实外部（GitHub PAT / Brave key / 通知接收方）一律落「未验证」，不写进 Demo 表的证据列。

**T040 本轮不勾选**：其 check `template-quality` 由 `TestTemplates_LoadAndTools` / `TestTemplates_TierEvidence`（T055 交付）判卷，当前为预期红态；`TestTemplates_TierEvidence` 将同时核对本文件「八模板 × 档位」与各模板 README 一致（含 A\* 不转 B）。

**诚实偏差登记（T017–T029 轮，不构成判据面变更）**：① **层拆分**——`TestMCPCatalog_ReadOnly` / `TestMCPDiagnostics_DoesNotStartProcess` 在 web 层无法断言"零 `CommandRunner` 调用 / 哨兵文件未创建"（那需要 app 层的连接器与 sentinel）；web 侧改为断"只触达只读端口（调用计数 + 未装方法即报错）+ 生产文件无 `os/exec` 导入的结构扫描（`scanned != 0`）"，进程级判据仍由 `internal/app/mcp_diagnostics_test.go:337` 的 `TestMCPDiagnostics_NoProcessNoOperation` 承担，两道合起来覆盖 §4.5.4 / §4.5.5 的原始断言面。② **`error_code` 无契约枚举**（`{type: string}`）→ 只按"有界、无控制字符"校验；`stage` / `failure_class` 契约有枚举 → 与契约逐值对齐（`failure_class` 的空串是合法值）。③ **新增第九个用例** `TestMCPContracts_OperationTestDetail`：D-07 的详情白名单若不加此用例，`-run ^TestMCP` 永远覆盖不到；属**新增**判据，不是放宽。④ 模块卡 §4.5.4 写作"零 CommandRunner"，web 层无从观测；见 ①。

**诚实偏差登记（不构成判据面变更，逐条说明）**：① `TestNotifyMode_DeliverToRemovesNotify` **无红态**——抑制逻辑（`core.AgentService` 的 `SuppressNotify` → `notify` 工具剔除）在 US-14 之前已存在，新测试首跑即绿；该测试是既有能力的守点，不是本轮新增行为的驱动测试，红灯由 `TestMCPTest_*` 六条承担。② §4.5.13 的 `connectedCount()` 观测点在一过性 `Test` 路径上不可达（假连接器不触发 `OnConnected`），`TestMCPTest_UnconfirmedEntryStillAttempts` 改用 `createdCount()==1` + `startedProcess()`——仍逐条断"诊断 unconfirmed 也必须真实尝试"，强度不降（见 §2.2 第 2 行）。③ §4.5.12 的 `beginFailStore` 以既有 `failingBeginStore` 落地（同语义，仅类型名不同）。④ §4.5.9 伪代码写 `tool.Function.Name`，本仓模型请求实际是 `core.ChatRequest.Tools []core.ToolSpec`，落为 `core.ToolSpec.Name`（同语义）。

**补充读数（T046 轮，非 stage 判卷）**：`examples/templates/weather-brief/` 文件清单与实测 sha256 / 行数——`AGENT.md`（112 行，`75c86f9add823c6fd32112fd876e43413cd9d9e2fb6dd31f5b4cd4a245a5ee98`）、`README.md`（102 行，`1893ccfb8da26afb130cd0114cdbd33a862056c3f6b68ab6a81c876570ea3eae`）、`fixtures/city.json`（23 行，`9a64e38022687dd43e9bec5680ee9b3cef31c6c6f552d673f23a3ebf57985bc0`）。无技能、无 MCP（§3.6 清单内最小形态）。`fixtures/city.json` 含 3 天预报（降水 0.0 / 6.4 / 0.2 mm，最高温 24.1 / 27.6 / 33.2 ℃）与偏好阈值 `umbrella_mm 0.5`、`heat_c 32.0`，按 AGENT.md 规则复算得到 通风 / 带伞 / 防暑 三条结论，与 README 期望表逐项一致。

**补充读数（T047 轮，非 stage 判卷）**：`examples/templates/arxiv-tracker/`——`AGENT.md`（104 行，`41d39093979950b6b4357b1e961a0e7aae1f776e3c1ee939098f9a3eef291de1`）、`README.md`（103 行，`0e7bf748ef6e228a3e9fd344d3e6baa586ef36be02640406f77b3f6999d2f05c`）、`fixtures/response.xml`（66 行，`48c9fcf9f283970504cd507c8e28db2e31f2e290720c3b97ac964d51fd1f1d42`）。XML 实测：`totalResults=6` 与 6 个 `<entry>` 一致；按 README 分类规则复算得 3 条新论文 / 2 条更新 / 1 条不变，与期望表一致。

**补充读数（T048 轮，非 stage 判卷）**：`examples/templates/policy-watch/`——`AGENT.md`（93 行，`9f6a6bf2bf181637e1576c1aac4d50d71a662c3490cf7f2e3c5b1b010f0e9c3c`）、`README.md`（104 行，`450260dd975af734e928fbf8e684abf15514bda7f742da9987afaaf8216a5ba6`）、`fixtures/announcements.json`（75 行，`a94cea650af693a5c2555d9dc1bf563eaefeafd0ca7f731e918e99a403dedac0`）、`skills/policy-watch/SKILL.md`（76 行，`5941400258580b356a909eb625526ce73d2590d79a72cd6b4cb088dd82e16922`）。`snapshot.json` 写入路径已统一到 **Agent 根目录**（`write_file` 不建父目录，`state/` 子目录写法会直接失败）——`AGENT.md` 第 6 步、README 文件范围行、SKILL.md §4 三处口径一致。

**补充读数（T049 轮，非 stage 判卷）**：`examples/templates/ticket-insights/`——`AGENT.md`（82 行，`137f48d7bf624b1104b656c33e9df951ffaf7d2bea7385fae0fcdd8b09ea665b`）、`README.md`（133 行，`013b870c5ac1744019b8c8fc21540afc8fe9e3172fad5d37c950543eaaa9e5fb`）、`fixtures/schema.sql`（21 行，`0769635ac698182ce1aa27d80f9672165de1ff3ef4cfab20a260ff6a2e53a5f4`）、`fixtures/rows.json`（16 行，`c9595de1fbf664d03aa95760a95646afbb90c7bf3c2ef786e63ffa9c5b61963e`）、`skills/ticket-insights/SKILL.md`（89 行，`3c7b03b2746a39b59126001bed3aedec3e210b3b4160c11d7cef61203b8906f3`）。10 条虚构工单 TK-2001…TK-2010 按技能关键词表（登录 → 支付 → 配送 → 未分类，首命中即停）复算：登录 4（open 2）/ 支付 3（open 3）/ 配送 2（open 1）/ 未分类 1（open 0），合计 10 / open 6；`TK-2010` 的 `priority: urgent` 不在枚举内，进「待确认」1 条——与 README 期望表逐项一致。dbhub 配方写明 `--readonly` 是 A 档前提。

**补充读数（T050 轮，非 stage 判卷）**：`examples/templates/onboarding-buddy/`——`AGENT.md`（85 行，`852858809e2a87e06eaa0262322dafeead0a46df36c634dda4951f3778bffcac`）、`README.md`（137 行，`d08a1d2c531c2558fa0dabcf2568cbdacdde2ce05b0fd0fe9c357fb3b3ba36e5`）、`skills/onboarding/SKILL.md`（101 行，`05bfcaab662f45e28638885265c87c7006b8940ce604a675adb344973037102c`）、`fixtures/docs/handbook.md`（35 行，`89ea1f1058d3487e08a16fb7334d8ff6d41c4292fbd412ada97b450449c1ed6b`）、`fixtures/docs/security-basics.md`（27 行，`e9852381a18afefb8558bc60913c893f176250c682cc331cfee7516324c6b296`）、`fixtures/docs/teams/data-access.md`（31 行，`684757b01e01baab4f9d61eb80453311b88e92d5b236624e8fd4f4208bae4809`）、`fixtures/docs/archive/2025-policy.md`（24 行，`692110e98870001df41c3e22c8272ab698d0e257deef50e9f696e96c2e5e96fd`）。**真实探针读数**（临时目录构建嵌套 module、JSON-RPC 走 stdio，探针已删）：`tools/list` 恰为 `list_documents` / `read_document` / `document_metadata` 三个，无写工具；`list_documents` 返回 4 个文档、按路径升序**且 `archive/2025-policy.md` 排第一**——这正是 README 要求「先按桶、桶内再按路径」而不能直接沿用全局路径序的原因；`document_metadata` 对 `teams/data-access.md` 返回的 sha256 与本地实测一致；`../escape.md` 被拒绝。README 另记一条替代根 `examples/shared/fixtures/docs/`（必读 3 + 按角色 1 + 参考 0）。

**补充读数（T051 轮，非 stage 判卷）**：`examples/templates/dependency-watch/`——`AGENT.md`（141 行，`711d5a56137cd5757a4ca5751dae5467a201da816e45d4df3ec4730bb68c3be0`）、`README.md`（112 行，`5470ad4b8b8218e5e6de5e20392cb2d4f50c24df93d51307e7df84ad3e87dcf2`）、`fixtures/dependencies.txt`（9 行，`c2331d38989001e211c68c571755388e42a7e880c7303b6961ec130146477fe9`）、`fixtures/osv.json`（41 行，`c3d716b6b57a7dd8871cc147a10c9398dca48b9bb14fcd71e90b84369c448798`）。按 AGENT.md 规则对夹具复算：新增 `OSV-2026-1002` / `OSV-2026-1003`、仍然存在 `OSV-2026-1001`、已修复（待核实）`OSV-2026-0999`、干净 4 个包——与期望表一致。**修正本轮实测发现**：非法行是**第 9 行** `npm dotenv`（只有两个字段），初稿误写「第 8 行 / 共 8 行」，已改为「9 行清单（2 行注释 + 6 行有效 + 1 行格式非法）」，`AGENT.md` 夹具复跑段与 README 期望段同步。

**补充读数（T052 轮，非 stage 判卷）**：`examples/templates/web-watch/`——`AGENT.md`（110 行，`4247fe71ad4e9956954ed2c190f57885b2eb762e5fbc0d792ab2ec7cb3c5b132`）、`README.md`（137 行，`2f8ce0d1b9701eb78e8b9e7200dbc8b45fcd1e73bb213d3a0a78f77c4dd76620`）、`fixtures/pages.json`（33 行，`02f0f5079a3ead62d8de9efcb9259955f4b6cff5a1d7b7c9ccdf0cb68c64baca`）。归一化（去行尾空白 + 丢空行）后静态页无变化；动态页快照对 `previous_snapshot` 做多重集合差得新增 2 行、删去 1 行——与 README 一致。档位 `A*（浏览器待就绪）`，README 明写不得为了收工降成 B。

**补充读数（T053 轮，非 stage 判卷）**：`examples/templates/funding-watch/`——`AGENT.md`（86 行，`5637723bc6c848919ea45ccf35274639b4fbd2f8288784c208dad0368f57e913`）、`README.md`（131 行，`db6cdc2c826ba9fbdcae35ef0f988da5e17e5f62116b14905ce463c980c10311`）。B 档只交两份文件（无 fixtures / 无 skills）；两文件都含字面量「**未验证，不要当可运行示例**」；`mcp_servers.yaml` 配方给出 `brave-search` / `fetch` 两条，`${BRAVE_API_KEY}` 为引用占位（全仓模板扫描仅此一处 `${...}`）。

**共同核对（T046–T053 轮，非 stage 判卷）**：① **八份 frontmatter 全部可解析**——用 `goccy/go-yaml v1.19.2` 在仓外临时 module 逐份 `Unmarshal`，8/8 通过，`tools` / `mcp_servers` / `skills` 条数与设计一致（临时 module 已删）；② 全部 JSON 夹具可解析、`arxiv` 的 XML 与 `totalResults` 自洽；③ **markdownlint**：23 个模板文件 + `examples/README.md` 共 24 个，`Summary: 0 issues in 0 files`、`exit=0`；④ **密钥扫描**：模板树内除 `${BRAVE_API_KEY}` 无任何 key / token / 密码字面量；⑤ 四个用 MCP 的模板 README 都补了「声明的 MCP server 连不上 → **整只 Agent 不注册**（`docs/contracts/agent-md.md` §2.1 第 6 条，WARN `mcp server skipped: <name>`）」已知限制——这条同时是 `web-watch` 停在 A\* 而非 B 的理由。

**故意差异登记（T046–T053，与模块卡 §3.6 之外的形态修正，不构成判据面变更）**：① **档位行改纯文本**——机器核对的档位字面量由 `**档位：A（全量）**` 改为 `档位：A（全量）`：单独成段的整行加粗会被 markdownlint 判为 **MD036**（「强调被当作标题」），`weather-brief` / `arxiv-tracker` / `policy-watch` 已改，新文件一开始就用纯文本形态。对 T055 的影响：`TestTemplates_TierEvidence` 按纯文本 `档位：…` 行解析，不按加粗标记解析；`## 档位` 节正文里的加粗解说不变。② **嵌套代码围栏**——`web-watch/AGENT.md` 的报告模板内层含以三反引号标注 `text` 的围栏，外层若也用三反引号会被内层提前闭合（实测 6 条告警：MD031 ×3 / MD033 / MD022 / MD040），改外层 `~~~text` 后归零；这是格式修正，模板语义未变。

**诚实偏差登记（T040 轮读数被本轮修订，不构成判据面变更）**：T040 记录的 `examples/README.md` 读数（111 行，sha256 `a4ab212b…`）**已被本轮修订取代**——A 档定义原写「templates 带 `SETUP.md`」，与模块卡 §3.6（模板文件清单只有 `AGENT.md` / `README.md` / fixtures / 按需 `skills/`；`SETUP.md` 属 Demo）冲突，属事实对齐修正。现行读数：`examples/README.md`（111 行，sha256 `3c93c80e90b77c8a1864fb0f1ea555a96dd8456ab7be7ef801b2ec80c9ddc667`），A 档定义改为「complete agent directory with fixtures, expected results and a README (templates carry no `SETUP.md`; that file belongs to the demos)…」。这是**同一文件的事实修正**，不是判据放宽：`TestTemplates_TierEvidence` 仍按八模板 × 档位交叉核对。

**T046–T053 本轮不勾选**：其 check `template-quality` 绑定 `TestTemplates_LoadAndTools` / `TestTemplates_TierEvidence`（T055 交付），与 T038 / T040–T045 同理按预期红态留待补跑；装载与工具子集核验需要 `dbhub` / `docs-readonly` / `playwright` / `brave-search` / `fetch` 五个同名假 MCP server 的 `tools/list`，随 T055 一并落地。真实外部实连（Brave / Fetch / Playwright / DBHub / OSV / arXiv / 天气 / 政策站点）与真实模型 Run 全部并入 14B 人工验收（外部服务未授权）。

**补跑读数（T038 / T041–T045，取代各自的"本轮不勾选"结论）**：`TestUS14Demos_Fixture`（T054 交付）落地后，上述六条按 stage 逐任务复跑并全部 `passed`，报告逐条见 §2.1 新增行；早前三条「本轮不勾选（预期红态）」的备注是**当时的**状态记录，按只追加纪律保留不动，其效力由本段取代。**T044 首轮 `source_changed` exit=2** 是验收工具的正确行为：我在 T044 的运行窗口内创建了 `internal/e2e/us14_templates_test.go`（T055 交付物），源码指纹变化使旧证据失效——不是 T044 的实现有问题。处置：等 T044 运行结束后冻结源码，把 T038 / T041 / T042 / T043 / T044 / T055 六条**整套**重跑，六份报告对应的都是同一棵现行树。`tasks.md` 勾选 T001–T055 中的已验条目（勾选 45 条），勾选位归一后正文指纹复算仍为 `cbe006f3…`（冻结输入未变），文件哈希随勾选变为 `7ccb4a22…`（基线快照 `efa371dd…` 保留不改）。

**补充读数（T055 轮，非 stage 判卷）**：新增 `internal/e2e/us14_templates_test.go`（`//go:build e2e`），两条判据——`TestTemplates_LoadAndTools` 走**真实二进制**取内置工具集（`basalt tool list`，解析 `名称：描述` 行，得到 10 个内置名）而不是复制 `declaredToolNames`（未导出，复制必然漂移）；MCP 侧用 `tools.NewMCPPool(...).ConnectDetailed` 起五个同名 server 取 `tools/list`：`docs-readonly` 是**真实构建**的交付物（`buildDocsReadOnlyBinary`，与 T054 的 demo 夹具**同一份**构建代码——两处各写一遍必然漂移而它判的是同一个交付物），面钉死为 `document_metadata` / `list_documents` / `read_document` 三个；`brave-search` / `fetch` / `dbhub` / `playwright` 用 `internal/testutil` 的 spec helper 复现 README 记录的**候选**工具名，代码注释写明真实服务未授权、该候选表若日后修改属判据面变更。`TestTemplates_TierEvidence` 断八份 README 的 `档位：` 行与 §2.4.6 一致（6 A / 1 A\* / 1 B）、九个固定章节齐全、无 `**已验证**` 徽章、funding-watch 含「未验证，不要当可运行示例」；A / A\* 模板的 README **不得**出现其它档位的 `档位：` 行——这就是「A 不通过不得降 B」的机器判据。**红态三例**（逐字，探针已删、改动文件已按 SHA-256 还原）：① 把 `web-watch/README.md` 的档位行改成 `档位：B（配方）` → `us14_templates_test.go:315: web-watch 出现了非本档位的档位行：档位：B（配方）`；② 把 `ticket-insights/README.md` 的 MCP 工具表里加一个 `run_sql` → `us14_templates_test.go:229: ticket-insights 表内 MCP 工具 run_sql 不属于任何声明的 server`；③ 往 `funding-watch/README.md` 写一个 `ghp_` 前缀字面量 → `us14_templates_test.go:244: funding-watch/README.md：GitHub token`。另两条判别力自检内建在测试里：`subset_check_discriminates`（面里不存在的工具名 ⇒ 零个 owner）与 `secret_scanner_discriminates`（临时文件植入合成 token 必须被扫出）。绿态复跑：`TestTemplates_LoadAndTools` + `TestTemplates_TierEvidence` PASS，合计 8.0s，`binary_sha256=c12aa4cd195563feb3d03235e1a65f6cbabcc2867976345938701363fff204a1`；`go vet -tags=e2e ./internal/e2e` 退出 0，`gofumpt -l` 两个改动文件无输出。**同轮重构**：`internal/e2e/us14_demos_test.go` 的 `(*demoFixture).buildDocsReadOnly` 提为包级 `buildDocsReadOnlyBinary(t, repo)`，D4 夹具与模板闸门共用。

**补充读数（T056 轮，非 stage 判卷）**：新增 `scripts/run-example-tests.py`（约 190 行，纯标准库）。判据形状：**固定清单**五个 `examples/daily/*/scripts`（不用通配——通配会把"某个 Demo 的测试目录整体消失"变成静默通过）；逐目录 `<python> -B -m unittest discover -v -s . -p test_*.py -t .`，解析 `Ran N tests` 并断言 `N >= 3`；`-B` + 子进程 `PYTHONDONTWRITEBYTECODE=1`，跑完**再扫一遍** `__pycache__`，真出现就按失败报出（不靠 `.gitignore` 兜底 T064 的干净检出）；输出统一 `reconfigure(encoding="utf-8")`，`.loop/runs` 日志不再按 cp936 落盘。绿态读数：`PASS` ×5，用例数 `9 / 13 / 19 / 22 / 24`（累计 87），`exit=0`，`find examples -name __pycache__` 零命中。**红态探针**（仓库外 `$LOCALAPPDATA/Temp/t56probe/`，已删）：同一份 runner + 五个构造目录，四类失败各自命中，逐字——① 只有 2 个用例 → `FAIL examples/daily/repo-digest/scripts: Ran 2 tests` / `- 仅 2 个用例，低于每目录下限 3`；② 一条断言失败 → `- unittest 退出码 1（非零即失败）`；③ 空目录 → `Ran 0 tests` + `- unittest 退出码 5（非零即失败）` + `- 发现 0 个用例（退出码随 Python 版本而异，必须独立拦住）`；④ 目录不存在 → `FAIL …: 无法解析用例数` / `- examples/daily/industry-brief/scripts 不是目录（清单写死，改名即失败）`；整轮 `4/5 个目录未通过`、`exit=1`，未构造缺陷的目录仍 `PASS`（说明失败不是整轮短路造成）。**实测纠正一条草稿事实**：初稿写"`unittest discover` 零发现时退出码仍为 0"——本机 Python 3.12.10 实测为 **5**；已改为"随版本而异（3.12 起为 5，更早版本为 0）"，因此计数下限仍是独立判据、不能只信退出码。「缺 Python = 失败」由调用方承担：`py -3` 不存在时进程根本起不来，stage 按非零退出判失败；脚本内另查 `sys.executable` 与 `>= 3.8`。

**补充读数（T063 轮，非 stage 判卷）**：新增 `internal/e2e/us14_release_test.go`（`//go:build e2e`，四条）。设计要点：① 取消用例的 run_id 取自**管理读面** `GET /api/v1/runs?session_id=…&status=running`，不在运行期直读 SQLite；② 工具超时用例按模块卡「生产代码无测试开关」**不传** `timeout_seconds`（判的就是缺省 `core.defaultToolTimeout` 30 s），用**有界 45 s** 轮询 `tool_invocations` 进入终态——这是四条里唯一一处运行期读库，实测第二连接在 WAL 下读取成功（`sql.ErrNoRows` 与瞬时忙在窗口内重试，超时把最后一次查询错误一并报出），不是"等 Run 收场再读"；③ 热加载用例用 `block` 工具开窗，同步点取 watcher 的 `Agent 配置已热加载` 日志（`internal/app/watcher.go:129`），`rewriteAgentBody` 只换正文、frontmatter（含夹具绑定块）逐字保留，故不触发运行期建库写入；判据比卡上的"模型屏障"更严——本 Run 的**两次**模型请求都必须带旧正文（每轮重读 `AGENT.md` 即红），下一 Run 第 1 次请求带新正文且不得残留旧正文；④ 映射自检不读 `harness/` / `docs/`（干净检出可跑），用 `-test.list` 校验 8 行 / 9 条去重用例真实存在，且「取消」「工具自身超时」各占一行、不是同一条用例。绿态读数：四条一次性 PASS（1.51 / 1.13 / 31.12 / 0.01 s，合计 35.7 s），`binary_source=candidate`、`binary_sha256=55bb0e04…1eb32`；候选分支的 `expectedVersionFor` 在仓库内有 `.git`，`git describe --tags --always` 返回 `e2e285f`。**红态三例（逆改）**：探针文件与 `-overlay` 全在仓库外 `$LOCALAPPDATA/Temp/t63probe/`（用毕已删），源码树零改动，三例各用 `go build -overlay` 单独构建探针二进制（sha `da979494…` / `87e5f97c…` / `de366db0…`），`BASALT_E2E_BINARY` 指向探针——① 只杀直接子进程 + 去掉 `KILL_ON_JOB_CLOSE` 兜底（`internal/tool/runner_windows.go`）→ `取消后：孙进程仍在写心跳（5 → 50 字节），整棵树都要回收`；② `classifyToolError` 把 ctx 错误改判 `runtime_error`（`internal/core/tool_exec.go`）→ `工具自身超时应落 failed + timeout，实际 "failed" + "runtime_error"`；③ 跳过 `registry.Replace`（`internal/app/watcher.go`，缓存不失效）→ `下一次运行应取新正文：输出缺少 "NEW-PROMPT-发布热加载"`。三条红态都落在**语义断言**上（不是构造/编译失败），事后 `bin/basalt.exe` 与全部源码逐字节未动。

**补充读数（T069 轮，非 stage 判卷；**T069 未勾选**）**：`console-test` 绿（`Test Files 21 passed (21)` / `Tests 234 passed (234)` / 2.79 s / exit 0），`console-build` 复跑绿（`npm --prefix assets/console run build` → `dist/assets/index-C8YvltG4.js` 303.17 kB / `index-BZZL5gRG.css` 34.07 kB / `integrity.json: 3 个文件` / 174 ms / exit 0），构建后 `git status --porcelain -- assets/console/dist` 仍只有本轮既有改动（旧 `index-szB35Sqj.js` 删除、`index.html` / `integrity.json` 修改、新 `index-C8YvltG4.js` 未跟踪），说明 dist 与当前源码一致、重建可复现。产物文案核对（对 `assets/console/dist/assets/index-C8YvltG4.js` 直接计数）：`配方未实测` ×3、`配方已实测` ×2、`服务端未暴露工具` ×3、`四条轴互不预判` ×1、`添加到配置` ×3；`已验证` 只出现在**否定句**里（`…也不等于已验证：命令…` / `…会显示成已验证。` / `…不代表已验证。` / `…从不代表已验证。`），**没有**把它做成徽章的字符串——与 `assets/console/src/views/catalog.ts:41` 的两值徽章（`配方已实测` / `配方未实测`）一致。视图级行为由 Vitest 承担（`assets/console/tests/catalog.test.ts` 等 21 个文件）。**未做且不冒充**：浏览器人工走查（目录页实际打开与观感、添加到配置的预填交互、缺 env 保存 503 后页面能否打开操作详情、诊断时用任务管理器核对不启动进程、`.cmd` 显示未确认、`pending_restart` 与连接状态并存、`tool_disposition` 显示内置赢家）——本会话**没有浏览器自动化工具**，`console-test` 只是该任务的机器前置而不是任务本身；这些条目按模块卡 §4.7「14A 人工」留待用户 review ②，T069 保持 `- [ ]`，未验证清单同步登记。

**补充读数（T064 轮，非 stage 判卷；**T064 未勾选**）**：`scripts/check-release-clean.sh` 两条路径分别取证。① **脏工作区红态（主仓实测）**：`sh scripts/check-release-clean.sh` → `clean-checkout: 工作区不干净——发布证据只能绑定已提交的候选提交` + 40 行 `git status --porcelain`，exit 1；这是当前主仓的**正确**读数，T064 的真实验收因此必须等用户提交候选提交（用户 2026-10-25 裁决：由用户提交，AI 等待）。② **干净路径机制验证（克隆 `$LOCALAPPDATA/Temp/t64clone/basalt` @ `e2e285f`，非候选提交，仅验机制）**：首版脚本用 `GIT_DIR` / `GIT_WORK_TREE` 给解档树借身份 → `make acceptance` 在 `test` 步骤红（失败台账 #18：变量泄漏劫持验收夹具，且把 `core.worktree` 写进了宿主仓配置）。改版为 `git worktree add --detach` + `git archive HEAD` 解档树逐文件比对：机制快检 `status_lines=0` / `DIFF_EMPTY=yes` / `files=1054` / 清理正常；端到端复跑 `make acceptance` **十项全绿**（`fmt` / `lint` / `sec` / `test` / `test-integration` / `cover` / `build` / `eval-build` / `behavior` / `injection`，报告 `accept-20261025T040744.374896400` 位于临时树内、随清理删除），随后 `make e2e` 报 `No rule to make target 'e2e'`——克隆停在 `e2e285f`，`e2e` 目标属 US-14 未提交改动，脚本按 fail-closed 退出 2。**结论**：脚本两条路径（脏 → 拒绝；干净 → 档案等价核验 + full profile + e2e）机制成立，但**不得**把克隆读数当发布证据；真实 T064 判卷需在用户提交的候选提交上重跑。主仓 `core.worktree` 复核为未设置；克隆已修复（`git config --unset-all core.worktree`）且 `git worktree list` 仅剩自身。

**补充读数（T039–T053 / T055 的路由证据）**：`template-quality` 在 stage 里绑 T040、T046–T053、T055（本条与模块卡 §4.3 的映射一致）；`demo-fixtures` 绑 T038、T041–T045、T054。因此一个模板文件改错会一次性打红九条任务——这是**有意**的共同判据面，不是重复计分：检查只有一份，失败时逐任务重跑只为让未通过的任务各自留证。

**补充读数（T069 浏览器走查，2026-10-25，用户裁决的验证方式；取代该任务早前「无浏览器自动化工具」的结论）**：

- **方式**：仓库外一次性走查工作区 `C:\Users\76562\AppData\Local\Temp\basalt-console\`（`bin/` 候选二进制、`docs/` 只读文档夹具、`fake/` 六个最小 MCP 子进程、`ws2/` 真实实例 `127.0.0.1:18431` 与独立 `.basalt`），Node 24.22.0 + Playwright 驱动 Chromium；脚本 `.loop/demo-us14-console/{lib.mjs,setup.mjs,run.mjs,run2.mjs}`。断言读**真实页面 DOM 与真实 API**，`console_errors` 收集 `console.error` 与 `pageerror`。
- **进程生命周期（实测，决定走查顺序）**：经管理 API 创建的 server 一律 `pending_restart=true / connected=false`，连接只在**进程启动**时发生（`internal/app/baseline.go` 的 `scanResource` 在重启时把 `applied` 追平 `published`，不看连接成败——契约 run-and-errors §9.6）。故走查分三段：`serve`（全新 `.basalt`）→ setup → **重启 1** → phase 1（末尾把 docs-readonly 发布为 v2）→ **重启 2** → phase 2。
- **读数**：setup 7/7（六个 server 建成 + 列表，`yaml_sha256_after_setup=af5fd232…d426`、`server_count=6`）；phase 1 **14/14**（`login` / `catalog-list` / `catalog-filters` / `catalog-detail` / `prefill-no-autosave` / `env-gate-503` / `operations-ledger` / `docsreadonly-detail` / `diagnosis-readonly-no-side-effects` / `test-docsreadonly` / `ghost-command-not-found` / `relcmd-unconfirmed-still-attempts` / `fake-empty-no-tools` / `publish-v2-pending-restart`），`create_503_operation_id=op-1792933533569-hnqis6`、`yaml_sha256_after_phase1=abcc19e1…9d96`、`server_count_after_phase1=6`；phase 2 **5/5**（`docsreadonly-v2-applied` / `stale-test-then-retest-v2` / `fake-empty-applied` / `fake-collide-disposition` / `ghost-applied-unconnected`）。三段 `console_errors` 均为 `[]`（零前端异常）。
- **落盘证据**：`.loop/demo-us14-console/shots/01-overview.png` … `19-ghost-applied-unconnected.png`（19 张，与上述步骤一一对应）；结果文件 `result-setup.json` / `result-phase1.json` / `result-phase2.json`（逐步 `PASS` 与上下文读数）。修复前失败运行的截图另存 `shots/earlier-failed-runs/`，只作 #20 / #21 的现场记录，**不代表**最终树。
- **本轮覆盖的客观项**（逐条对模块卡 §4.7「14A 人工」）：① 目录页可打开、六条配方、四条筛选、详情与「添加到配置」预填（`prefill-no-autosave` 断预填后**未**产生任何写请求、`mcp_servers.yaml` 哈希不变）；② 缺 env 保存 → 503 + 操作详情入口（`env-gate-503`：操作行 `error_message` = `保存所需环境变量未配置`、`detail.error_message` 点名 `BRAVE_API_KEY`，文件与发布指针零变化）；③ 诊断三态（`ghost` `command_not_found` / `relcmd.cmd` `unconfirmed` 且测试仍真实尝试 / 正常 server `located`）与「诊断不启动进程」（`diagnosis-readonly-no-side-effects` 断诊断前后完整子进程集合与 `mcp_servers.yaml` 哈希不变）；④ 测试记录的被测版本 / 阶段 / 失败分类，以及 v2 重启后旧测试标「版本已过期」、重测后转通过；⑤ `pending_restart` 与连接状态并存（`publish-v2-pending-restart` / `14-pending-restart`）；⑥ `tool_disposition` 内置赢家（`fake-collide`：`read_file` 被 `builtin` 抢先，`tools` 仍是全量 offered）；⑦ 空工具清单（`fake-empty`：`已提供 0，已注册 0`，修 #21 后成立）；⑧ `ghost` 重启后 `已应用 · v1 / 待重启 否 / 连接 未连接`（契约 §9.6 语义）。
- **免验与不冒充**：纯主观视觉评价（间距、配色、文案语气）**用户裁决免验，不阻断收口**，本节**不**记为「人工验证通过」。**单列、不视为通过**的客观项：诊断点击瞬间的进程树人工旁证（自动化只对点击前后的完整子进程集合取差，抓不到瞬时 fork）；真实 `.cmd` 在无 `PATHEXT` 解析环境下的行为（只能夹具模拟）；真实 GitHub / Brave / Fetch / Playwright / DBHub（走查里都是同名假 server，外部服务未授权）。
- **同轮发现**：两处实现缺陷由此暴露并修复——失败台账 #20（详情契约缺 `args` / `url`）与 #21（已连接零工具时 `tool_disposition` 为 null），均有逐字红态与复跑读数。走查工作区仍在系统 Temp，收口前删除（§4）。

**补充读数（T068 四道门禁，2026-10-25）**：① `make check` exit=0（本轮实测；下列逐目标读数取自**同一棵树的**全量 stage 运行日志 `.loop/runs/accept-20261025T134558.780851700/`，指纹均为 `9a3a70cc…`）——`fmt`：`check-format: gofmt / gofumpt 均干净`；`lint`：vet + staticcheck 静默；`sec`：`gosec -quiet ./... && govulncheck ./...` → `No vulnerabilities found.`；`test`（`-race`）19 个包全 `ok`、零 `FAIL`（`internal/app` 108.6 s）；`test-integration`（`-tags=integration`）19 个包全 `ok`；`cover`：`check-coverage: 全部包达标`——`core` 85.04%（≥70）、`memory` 91.32%（≥70）、`storage` 71.02%（≥70）、`tool` 85.34%（≥70），其余 ≥50%（最低 `internal/testutil` 50.92%）；`build`（`CGO_ENABLED=0`）ok。② `make example-tests` exit=0——嵌套 module 与五目录 87 个用例（9 / 13 / 19 / 22 / 24），零 `__pycache__`（本 stage 运行中由 `docs-readonly-tests` + `script-tests` 重跑确认）。③ `make acceptance`（full profile）十项全 passed，报告 `accept-20261025T132805.648468600`。④ `make e2e` exit=0、233.083 s，`binary_source=candidate`、`binary_path=D:/code/basalt/bin/basalt.exe`、`binary_sha256=34406c4f7958d5f0e21da54050b275f9301459f3dd6801bdcdffd883728c56b8`（修复 #21 后的新产物；上表 T063 行的 `55bb0e04…` 是修复前读数，已被取代）。⑤ FR-025 核对：`git diff --stat -- go.mod go.sum docs/contracts/schema.sql harness/acceptance.json harness/stages/current.json .github/workflows/ci.yml` 为空（零迁移 / 零新增 Go 坐标 / 不改门禁与 CI）。以上 ①②③④ 在本轮全量 stage 中由同一棵树的对应 check 再次覆盖（见下条）。

**补充读数（T068 全量 stage，2026-10-25，非逐任务判卷）**：`sh scripts/check-acceptance.sh --stage harness/stages/us14-release.json`（不带 `--task`，即公共十项 + 本 stage 二十七项）→ 报告 `.loop/runs/accept-20261025T134558.780851700/report.json`，`scope=full/us14-release-r1`、`base_commit=e2e285f`、`fingerprint=9a3a70cc85a6eb21e7f184db8fe09771122752eda422a033bc373c5c868dce1c`、`dirty=true`、`go1.27.3` / `windows/amd64`、`tested_binary_sha256=34406c4f7958d5f0e21da54050b275f9301459f3dd6801bdcdffd883728c56b8`（与 `make e2e` 同一次 `make build` 产物的 SHA-256 一致）；**37 项中 36 项 passed、1 项 failed**，累计机器耗时 1903.8 s（约 31.7 min，21:45:58 → 22:17:43 本地）。逐项：公共 `fmt` / `lint` / `sec` / `test` / `test-integration` / `cover` / `build` / `eval-build` / `behavior` / `injection` 十项全 passed（`cover` 644.3 s、`test` 119.3 s、`test-integration` 49.0 s）；本 stage `go-build` / `contract-yaml` / `mcp-catalog` / `mcp-env-gate` / `mcp-diagnostics` / `mcp-test-record` / `mcp-connect` / `mcp-disposition` / `mcp-contracts` / `notify-exclusion` / `mcp-admin` / `console-test` / `console-build` / `demo-fixtures`（98.4 s）/ `legacy-d1` / `template-quality` / `docs-readonly-tests` / `script-tests` / `e2e`（235.8 s）/ `e2e-required`（214.4 s）/ `website-test` / `website-build` / `website-check` / `docs-consistency` / `stage-lint`（422.1 s）/ `release-records` 二十六项 passed。**唯一红项 `clean-checkout`**：exit=1、耗时 245 ms，逐字输出 `clean-checkout: 工作区不干净——发布证据只能绑定已提交的候选提交` 后列出 40 行 `git status --porcelain`——这是脚本设计上的**预期红**（工作区尚未提交，用户 2026-10-25 裁决由用户提交），**不作** T064 / T068 的通过证据，也不据此判定实现缺陷。两点旁证：① 报告状态是 `failed` 而非 `source_changed`（[execute.go:414-420](scripts/acceptance/execute.go#L414-L420)），说明整轮运行期间源码指纹未漂移，36 项读数对同一棵树成立；② `clean-checkout` 在 245 ms 内返回，说明它**先**判工作区干净再进档案解档，未因长任务掩盖脏态。

**补充读数（T066 依赖与许可表，2026-10-25）**：**只统计随产物分发的依赖**。Go 侧取 `go version -m bin/basalt.exe` 的 `dep` 行（发布候选、与 `make build` 同一次 `CGO_ENABLED=0` argv），许可逐个读 `GOMODCACHE/<module>@<version>/LICENSE*` 后归类；Node 侧读已安装 `package.json` 的 `license` 字段；Python 侧做全量 import 扫描；MCP 侧对照 `internal/catalog/data/mcp_catalog.yaml` 的 `verified` 状态。

**Go：24 个链接进二进制的模块**（Apache-2.0 4 / BSD-3-Clause 12 / MIT 8；**链接集合内无 GPL / AGPL / LGPL / MPL**）：

| 模块 | 版本 | 许可 |
| --- | --- | --- |
| `github.com/inconshreveable/mousetrap` | v1.1.0 | Apache-2.0 |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | Apache-2.0 |
| `github.com/sashabaranov/go-openai` | v1.43.0 | Apache-2.0 |
| `github.com/spf13/cobra` | v1.10.2 | Apache-2.0 |
| `github.com/fsnotify/fsnotify` | v1.10.1 | BSD-3-Clause |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause |
| `github.com/spf13/pflag` | v1.0.9 | BSD-3-Clause |
| `github.com/yosida95/uritemplate/v3` | v3.0.2 | BSD-3-Clause |
| `golang.org/x/oauth2` | v0.35.0 | BSD-3-Clause |
| `golang.org/x/sync` | v0.23.0 | BSD-3-Clause |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause |
| `golang.org/x/time` | v0.15.0 | BSD-3-Clause |
| `modernc.org/libc` | v1.77.1 | BSD-3-Clause |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause |
| `modernc.org/memory` | v1.12.1 | BSD-3-Clause |
| `modernc.org/sqlite` | v1.60.1 | BSD-3-Clause |
| `github.com/dustin/go-humanize` | v1.0.1 | MIT |
| `github.com/goccy/go-yaml` | v1.19.2 | MIT |
| `github.com/google/jsonschema-go` | v0.4.3 | MIT |
| `github.com/mattn/go-isatty` | v0.0.24 | MIT |
| `github.com/ncruces/go-strftime` | v1.0.0 | MIT |
| `github.com/robfig/cron/v3` | v3.0.1 | MIT |
| `github.com/segmentio/asm` | v1.1.3 | MIT |
| `github.com/segmentio/encoding` | v0.5.4 | MIT |

**三条须随表一起说明的事实**：① `modelcontextprotocol/go-sdk` 的 `LICENSE` 自带 MIT→Apache-2.0 过渡声明——新增贡献按 Apache-2.0，未同意再许可的原 MIT 贡献仍按 MIT，两者都在宽松范围内；② `modernc.org/sqlite` 带 `LICENSE-3RD-PARTY.md`（74,309 字符），其第 1 节「链接进产物」内的组件为 MIT / BSD-3-Clause / public domain（含转译进 `lib/` 的 SQLite 3.53.4 = public domain、`vec/` 的 sqlite-vec v0.1.9 = MIT）；**唯一一处 MPL-2.0** 是 `github.com/hashicorp/golang-lru/v2 v2.0.7`（双许可 MPL-2.0 / BSD-3-Clause），上游把它列在第 3 节「只在模块图、未编译进任何产物」——`go.sum` 里能看到，但**不在** `go version -m` 的 24 条里；③ 主模块时间戳当前是 `github.com/ymedword/basalt v0.0.0-20261024172006-e2e285fa45e6+dirty`——`+dirty` 来自未提交工作区，**候选提交后必须重算**，发布记录不得沿用本行。

**Node（构建期工具链；只有 Vue 随控制台 `dist` 进二进制）**：控制台生产依赖仅 `vue 3.5.44`（MIT），经 `go:embed` 进 `assets/console/dist` 后随二进制分发（当前产物 `assets/console/dist/assets/index-C8YvltG4.js`，303,176 B，sha256 `35d81757d7e3f2672ebccd7b680856548d0a5090f158515ecedff4b992f2322c`）；控制台开发依赖 7 个 MIT（`@vitejs/plugin-vue 6.0.10`、`@vue/test-utils 2.5.1`、`jsdom 30.1.3`、`vite 8.3.4`、`vitest 5.0.5`、`vue-tsc 3.3.13`）+ `typescript 5.9.3`（Apache-2.0），不进产物；站点仅 `vitepress 1.6.4`（MIT），产物不进产品二进制。两个 `package.json` 的依赖树 `missing=[]`（无缺失 `license` 字段的包）。

**Python（Demo / 示例脚本）**：**零第三方依赖**。对 `examples/` 与 `scripts/` 下全部 `*.py` 做 `import` / `from` 首段扫描，只出现标准库（`argparse` / `contextlib` / `datetime` / `decimal` / `hashlib` / `io` / `json` / `os` / `pathlib` / `re` / `sqlite3` / `subprocess` / `sys` / `tempfile` / `unittest` / `urllib` / `__future__`）与五个 Demo 自己的模块名（`biz_daily` / `docs_diff` / `github_radar` / `industry_brief` / `repo_digest`）。运行只需 Python 3 解释器本身（本机是 `py -3`）。

**仓内自研素材**：`examples/shared/mcp/docs-readonly` 是独立嵌套 module，`go.mod` 只有 `module` 与 `go 1.27` 两行、**零第三方 require**，与主仓同许可；样例 SQLite 由 `examples/shared/scripts/make_sample_db.py` 生成、不提交二进制。

**MCP 配方包（六条）**：

| 配方 id | 包 / 入口 | 版本 | 许可 | 状态 |
| --- | --- | --- | --- | --- |
| `brave-search` | `brave-search-mcp-server`（npm） | 未核实 | 未核实 | 未安装；`verified: false` + `package_entry_unverified` |
| `dbhub` | `dbhub`（npm） | 未核实 | 未核实 | 同上 |
| `fetch` | `mcp-server-fetch`（PyPI，`python -m`） | 未核实 | 未核实 | 同上 |
| `filesystem-readonly` | `@modelcontextprotocol/server-filesystem`（npm） | 未核实 | 未核实 | 同上 |
| `github` | `github-mcp-server`（预编译二进制，需在 PATH） | 未核实 | 未核实 | 同上 |
| `playwright` | `@playwright/mcp`（npm） | 未核实 | 未核实 | 同上 |

六条均 `verified: false`，目录**只读、不安装任何包**；本 US 外部授权为零，故不写推断的版本或许可——「未核实」是事实状态而非遗漏，发布记录同一行必须原样保留。

**补充读数（T066 升级 / 恢复演练，2026-10-25）**：依据 `docs/Operations.md` §5.1（停 → 备份 → 换二进制 → 启动并迁移 → 验证 → 观察）、§5.2（回滚 = 备份 + 旧二进制）、§4.3（恢复验证三隔离 + 五判据）；全部在仓库外一次性工作区 `%LOCALAPPDATA%/Temp/t66drill/` 执行，不改仓库、不改发布脚本。**版本对**：候选 = `bin/basalt.exe` 同一份产物（`34406c4f7958d5f0e21da54050b275f9301459f3dd6801bdcdffd883728c56b8`，即 `make e2e` 与全量 stage 的 `tested_binary_sha256`）；旧 = `git archive HEAD`（`e2e285f`，US-14 改动之前）以同一 `CGO_ENABLED=0` 口径重建（`12a8b5bed22f369f7186548bc26bb2883819dbe3c38a9e36f1a58fb87f52ff64`）。**不外呼**：模型用回环假服务（`127.0.0.1:18461`，OpenAI 兼容、固定回复、不带工具调用），全程 `requests.jsonl` 仅 3 条请求、路径全为 `/v1/chat/completions`；配置 `http.allowed_domains: []`（`http_get` / `http_post` / 通知投递一律失败关闭）、`shell.allowed_commands: []`，通知渠道 0 个。**步骤与读数**：① 建实例（候选）并 `chat` 一轮 → 会话 `cli-1c5aafb256301130` 2 条、`schema v5、29 张表`、10 件内置工具、`profile list` 1 个已注册；备份 `backup-20261025T1422/` 共 **12 个文件**（含 `basalt.db` / `-wal` / `-shm` / `basalt.yaml`），`basalt.db` sha256 `0f929100f4c5deca3899dc22e275a01fe3c769a3dfd08ccf5fe8e3e2168f35b2`、`basalt.yaml` `825c427acd356aca7cbc77568653c8b07dd78a817f4eb394c2c0eab5322b243d`。② **旧二进制读新数据**（回滚前置）：旧二进制对候选写出的实例 `status` 全部通过（`schema v5、29 张表`）、`session list` 同一会话 2 条；另在 live 工作区的**副本**上复验（`old-read-check`）→ `status` 通过、`session list` 4 条、`profile list` 1 个已注册，三条命令 `exit=0`——**只读路径成立，写路径未演练**。③ 换二进制：`deploy/basalt.exe` = 候选，旧文件留作 `deploy/basalt.bak-prev`（sha256 与 `basalt-prev.exe` 一致）。④ 启动并迁移：候选启动同一工作区，数据不变、`profile list` 仍注册；**无 schema 迁移可跑**（v5 → v5，`v0.1.0` 是首个发布，本 US 零迁移）。⑤ 升级后写入：再 `chat` 一轮 → `messages 4`、`runs 2`（均 `completed`）、`llm_calls 2`、`tool_invocations 0`、`management_operations 3`（备份点即为 3，升级本身不新增管理操作）。⑥ 恢复验证（§4.3）：备份复制到 `restore-check/`（12 个文件；`basalt.yaml` 与备份逐字节相同，sha256 同上、`diff` 为空），三隔离逐条——`scheduled_tasks` 0（**表名核对：本仓不是 `schedules`**）、`notify_channels` 0 且通知环境变量未设、`http.allowed_domains: []` 落库为 `network_domain_policy ('http.allowed_domains','',1,1792937992812,'')`；五判据：`status` 全部通过 / `session list` **2 条**（备份点计数，live 为 4 条——恢复的是快照而非当前态）/ `profile list` 1 个 / `tool list` 10 件 / `chat` 返回 `drill-ok: 演练实例已连通（假模型，非真实模型）` 并留下审计行（恢复库随后 `messages 4 / runs 2 / llm_calls 2`）。⑦ 三库互不覆盖：备份 `0f9291…f35b2`、live `fc3954…362b3`、恢复 `bc64e8…f0d3f` 三个不同哈希。**结论**：§5.1 六步、§5.2 回滚前置与 §4.3 恢复验证在本机（Windows）走通。**不覆盖**：Linux / 跨机器 / 真实模型与通知接收方 / schema 变更迁移（无迁移可演）/ 观察段（指标）/ 回滚后的写路径；备份取自无 `serve` 进程运行的窗口（文件集复制，含 WAL / SHM），**在线备份未演练**。

**补充读数（T066 Linux 平台走查，2026-10-25）**：平台 = WSL2 Ubuntu 26.04 LTS（`Linux 6.6.114.1-microsoft-standard-WSL2 x86_64`，Python 3.14.4），二进制按发布同一 argv 交叉编译（`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always)"`）→ `da3bb7f9ea3f09c9d073751bee13a450ad7d4173b60a2b177795a6085c17784c`（20,050,080 B）、`basalt version e2e285f`；工作区 `~/t66linux-walk/ws`，回环假模型 `127.0.0.1:18462`，实例 `127.0.0.1:18463`，全程零外呼。逐项读数：① `init` 新建 11 项骨架 + 库（`schema v5`）、`profile create linux-agent` 生成脚手架（`provider.model` 占位「请在此填写模型名」）；② `serve` 起后 `/api/v1/health` → `200 {"status":"ok"}`；`GET /` → `302 → /console/`；`GET /console/` → `200`（771 B，入口脚本 `assets/index-C8YvltG4.js`）；`GET /console/assets/index-C8YvltG4.js` → `200`，**303,176 B**——与 Windows 侧 `assets/console/dist/assets/index-C8YvltG4.js` 字节数一致（同一份 `go:embed` 产物）；③ 管理面鉴权：`GET /api/v1/model-connections` 无凭据 `401`、带 bearer `200`（`linux-fake / rs_5WWZAADRJECJ34P7VYG97PDDYW / linux-model`）；④ 绑定与启用：改 `provider.model: linux-model` 并写 `resource_bindings.model_connection`，`POST /api/v1/agents/linux-agent/enable` → `200`（`state=active`、`generation=2`、`agent_uid=ag_XDDPCJA5X7K2BWCD3N0P6NHFAD`、`operation_id=op-1792938589263-a7zqdw`）；⑤ `status` 全部通过（`schema v5，29 张表`）、`tool list` 10 件、`chat --agent linux-agent --message …` → `drill-ok: 演练实例已连通（假模型，非真实模型）`、`exit=0`；⑥ `session list` 1 个会话 2 条、`profile list` 显示 `模型 linux-model`（证明绑定改写生效）、假模型收到 1 条 `/v1/chat/completions`（`model=linux-model`）；⑦ 落库计数 `messages 2 / runs 1（completed）/ llm_calls 1（success）/ tool_invocations 0 / management_operations 3 / scheduled_tasks 0 / notify_channels 0`。**不覆盖**：Linux 上的浏览器交互走查（WSL 无浏览器，只验到 `/console/` 与入口脚本 `200` 且字节数与 Windows 产物一致）、真实外部服务、systemd 常驻与包安装（属 US-17 拟）。**候选提交后版本戳会变**（`git describe` 返回新 SHA），Linux 二进制需按候选重建后才能作为发布证据。

**补充读数（T066 升级 / 恢复演练——候选提交后重跑，2026-10-25）**：口径与候选前那次相同，本次全部绑定**已提交候选 `11ef243`**。**版本对**：新 = `bin/basalt.exe`（`make build` 产物）= `fb022a01d94e155a3c4682527827e715c9d7829b7dba906b40ba1402d10d909f`（20,377,600 B，`basalt version 11ef243`）；旧 = pre-US-14 二进制 `12a8b5bed22f369f7186548bc26bb2883819dbe3c38a9e36f1a58fb87f52ff64`（`basalt version dev`）。**环境**：仓库外一次性工作区 `%LOCALAPPDATA%/Temp/t66drill2/ws`，回环假模型 `127.0.0.1:18461`，`http.allowed_domains: []`、`shell.allowed_commands: []`、管理令牌取 `${BASALT_ADMIN_TOKEN}`，全程零外呼。**步骤与读数**：① `init` 新建 11 项骨架 + `schema v5`；`profile create drill-agent`；serve#1 取 `resource_uid=rs_6TDVTSRS9W1S14TR9A1NET7QJT`；改写 `provider.model` 与 `resource_bindings` 后 serve#2 `enable` → `200`；`chat` #1 → `drill-ok: 演练实例已连通（假模型，非真实模型）`。② 停机备份 `backup-20261025T2249`：**10 个文件**（`.basalt/` 下 9 + `basalt.yaml`），**无 `basalt.db-wal` / `-shm`**——chat #1 的 CLI 连接已关闭，SQLite 在最后一个连接关闭时 checkpoint 并删除 WAL，复制按当时实际存在的文件集执行；`basalt.db` sha256 `6e7a4f502724ba72cf3a3dcda2169b5a582fd61b309f11998b2be20868545a15`、`basalt.yaml` `ff1fe3a0ce5885a7d975c0ab92834b7308de1818c3c34a04e9371e7100000ed2`。备份点计数：`messages 2 / runs 1 / llm_calls 1 / tool_invocations 0 / management_operations 3 / scheduled_tasks 0 / notify_channels 0`；`runs.completed=1`、`llm_calls.success=1`；`sqlite_master` 表数 30（`status` 报受管表 29 张）。③ **旧二进制读新数据（回滚前置）**：对 live 工作区与副本（`old-read-check`，复制于 chat #2 前）各跑 `status` / `session list` / `profile list` → 全部 `exit=0`；`status` 全通过（`schema v5 / 29 张表`）、会话 2 条、`drill-agent：registered=true`。**只读路径成立，写路径未演练**。④ 换二进制：`deploy/basalt.exe` = 候选，`deploy/basalt.bak-prev` = 旧文件（sha256 与 `basalt-prev.exe` 一致）。⑤ 升级后：`status` / `session list` / `profile list` / `tool list`（10 件）全通过；`chat` #2 成功 → 计数 4/2/2/0/3（`runs.completed=2`、`llm_calls.success=2`）；**无迁移可跑**（v5 → v5）。⑥ 恢复验证（§4.3）：`restore-check` 复制备份 → **10 个文件**、`basalt.yaml` 与备份逐字节一致、复制后 DB 哈希与备份相同（`6e7a4f50…`，字节级一致）；三隔离：`scheduled_tasks=0`、通知环境变量未设且 `notify_channels=0`、`network_domain_policy` 行 = `('http.allowed_domains','',1,1792939764346,'')`（列名 `policy_scope / domains / revision / updated_at / operation_id`）——**配置键不灌注、以数据库为准**；副本 `messages=2` = 备份点计数。五判据：`status` 全通过 / `session list` 2 条 / `profile list` 1 个 / `tool list` 10 件 / `chat` 成功 → 恢复副本计数 4/2/2/0/3。⑦ 三库互不覆盖：备份 `6e7a4f50…5a15`、live（chat #2 后）`4e6500786a85830db3927a1f97f4fc2ef1efbaba9adc79e5f2e2a4ddef9c678c`、恢复副本（chat #3 后）`de6e14a843eda49b83dd5f1710d23f755951576d95842473cf5d6563ef47ea67`；旧二进制对恢复副本 `status` 全通过、`session list` 4 条、`exit=0`。⑧ 假模型收到 3 条请求，路径全为 `/v1/chat/completions`、模型 `drill-model`。**结论**：Operations §5.1 六步 / §5.2 回滚前置 / §4.3 三隔离五判据在**已提交候选**上重跑通过。**不覆盖**：Linux 上的升级 / 恢复（Linux 走查另见下条，只验单二进制单工作区）、真实模型与通知接收方、schema 变更迁移（本 US 零迁移，无迁移可演）、指标观察段、**回滚写路径**（旧二进制只读）、**在线备份**（仅停机复制）。

**补充读数（T066 Linux 平台走查——候选提交后重跑，2026-10-25）**：二进制按发布同一 argv 以 `11ef243` 重建 → `3de73e446486dc0bc8ce4a9b9d5648c9c1b933dd0be5182e0660b2a6f5e6a847`（20,050,080 B）、`basalt version 11ef243`；平台 = WSL2 Ubuntu 26.04 LTS（`Linux 6.6.114.1-microsoft-standard-WSL2 x86_64`，Python 3.14.4），工作区 `~/t66linux-walk/ws`，回环假模型 `127.0.0.1:18462`、实例 `127.0.0.1:18463`，全程零外呼。逐项读数：① `init` 新建 11 项骨架 + `schema v5`；`profile create linux-agent` 生成脚手架（`provider.model` 占位「请在此填写模型名」）；② `serve` → `/api/v1/health` `200 {"status":"ok"}`；`GET /` `302 → /console/`；`GET /console/` `200`（771 B，入口脚本 `assets/index-C8YvltG4.js`）；`GET /console/assets/index-C8YvltG4.js` `200`，**303,176 B**——与 Windows 侧 `assets/console/dist/assets/index-C8YvltG4.js` 字节数一致（同一份 `go:embed` 产物）；③ 管理面鉴权：无凭据 `401`、bearer `200`（`linux-fake / rs_JRVT3JBVDYGXMRB1G047ZQ9PA1 / linux-model`）；④ 绑定与启用：改写 `provider.model: linux-model` 并写 `resource_bindings.model_connection`，`POST /api/v1/agents/linux-agent/enable` → `200`（`state=active`、`generation=2`、`agent_uid=ag_F55NE2MB4E5KBS98EAR7TDJX8S`、`operation_id=op-1792939873436-ymlf5s`）；⑤ `status` 全通过（`schema v5，29 张表`）、`tool list` 10 件、`chat --agent linux-agent --message …` → `drill-ok: 演练实例已连通（假模型，非真实模型）`、`exit=0`；⑥ `session list` 1 个会话 2 条、`profile list` 显示 `linux-agent：registered=true（provider linux-fake / 模型 linux-model）`；假模型收到 1 条 `/v1/chat/completions`（`model=linux-model`）；⑦ 落库计数 `messages 2 / runs 1（completed）/ llm_calls 1（success）/ tool_invocations 0 / management_operations 3 / scheduled_tasks 0 / notify_channels 0`。**不覆盖**：Linux 上的浏览器交互走查（WSL 无浏览器；只验到 `/console/` 与入口脚本 `200` 且字节数与 Windows 产物一致）、真实外部服务、systemd 常驻与包安装（属 US-17 拟）。

**补充读数（T064 / T066 / T068 全量 stage——候选提交后，2026-10-26）**：`PATH=<便携 Node 24.22.0>:$PATH sh scripts/check-acceptance.sh --stage harness/stages/us14-release.json --out .loop/runs/us14-release-gate-r2 --executor ai --run-kind original` → `scope=full/us14-release-r1`、`base_commit=11ef2437083adc7228f4658079447567c9434398`、`dirty=false`、`source_fingerprint=8deb0dfdf613d8f195de9bddc82b769dfc44a2ed5eb8e3558f8c6379c4d3e0ac`、`go1.27.3 / windows/amd64`、`started_at=2026-10-25T15:13:20Z`、`finished_at=2026-10-25T16:07:04Z`；**37/37 passed**（公共十项 + 本 stage 二十七项），累计机器耗时 3223.2 s（53.7 min）。慢项：`clean-checkout` 1203.5 s、`cover` 674.2 s、`stage-lint` 444.5 s、`e2e` 259.1 s、`e2e-required` 219.9 s、`test` 134.3 s。三项关键读数：① `clean-checkout` 逐字 `clean-checkout: passed (commit=11ef2437083adc7228f4658079447567c9434398 version=11ef243)`，内部 `make acceptance` 与 `make e2e` 全绿（内层 e2e `ok … 242.996s`），**T064 通过**；② `e2e` 日志 `binary_source=candidate` / `binary_path=D:/code/basalt/bin/basalt.exe` / `binary_sha256=fb022a01d94e155a3c4682527827e715c9d7829b7dba906b40ba1402d10d909f`，与分发候选同一文件；③ 发布前 `release-records` passed，发布后 `--task T066`（`accept-20261025T160737.716742700`）`fmt` + `release-records` passed。**发布记录（T066）**：record + 同轮 report 已按 `harness/acceptance/records/README.md` 顺序发布为 `harness/acceptance/records/us14-release-gate-r2.json` 与 `records/reports/us14-release-gate-r2.json`（report sha256 `8921b2e72927138d8354d5c95aa67f4f1d5d5840ef85430c5cc5d82cf91ad342`），`ledger.md` 追加一行（scope `us14-release-r1` / original / ai / passed / 2026-10-25）；记录字段 `code.dirty=false`、`code.tested_binary_sha256=fb022a01…`、`criteria.inputs` 含三件输入与 stage / acceptance 哈希。**说明**：r1（`.loop/runs/us14-release-gate`）因后台 shell 缺 `npm` 在 `console-test` 报 `executable unavailable: npm`（失败台账 #22），保留为失败证据、不发布；本记录只认同一棵树上的 37/37 绿态。

**补充读数（T067 条件式文档更新核对，2026-10-26）**：两次演练（候选前 + 候选提交后）均按 `docs/Operations.md` §5.1 / §5.2 / §4.3 原文执行，未发现需要修订流程语义的缺口；`git diff e2e285f 11ef243 -- docs/Operations.md` 只有两处**事实同步**——§1 依赖表（Python / Node / Go 行重写，点名五个 Demo 与首批 MCP 配方）与新增 §7.1「示例 / 模板依赖与出网边界」（素材依赖 / 出网 / 凭据逐项），§4.3 / §5.1 / §5.2 **零改动**。**结论：T067 无缺口需修，Operations 变更属 FR-024 事实同步，不是演练驱动的流程改写。**

**补充读数（T068 quickstart 逐节四态，2026-10-26）**：基线与证据 = 上一条 gate r2（commit `11ef243`，dirty=false）+ 已发布记录；`git diff --stat e2e285f 11ef243 -- go.mod go.sum docs/contracts/schema.sql harness/acceptance.json harness/stages/current.json .github/workflows/ci.yml` **为空**（FR-025 零迁移 / 零依赖 / 不改门禁与 CI）。

| quickstart 小节 | 四态 | 本轮读数 / 证据 |
| --- | --- | --- |
| 0 前置条件 | 通过 | Go 1.27.3、`GOPROXY=https://goproxy.cn`、Python 3.12.10；Node 24.22.0 为**便携安装**，必须显式加入 PATH 才有 `npm`（失败台账 #22） |
| 1.1 包级测试 | 通过 | `mcp-catalog` / `mcp-env-gate` / `mcp-diagnostics` / `mcp-test-record` / `mcp-connect` / `mcp-disposition` / `mcp-contracts` / `notify-exclusion` / `mcp-admin` 均 passed（`required_tests` 判卷，空匹配 / SKIP 不算通过） |
| 1.2 契约测试 | 通过 | `mcp-contracts`（`TestMCPContracts_*`，含 D-07 详情白名单第九例）+ `contract-yaml` passed |
| 1.3 全量门禁 | 通过 | 公共十项 `fmt` / `lint` / `sec` / `test`（134.3 s）/ `test-integration` / `cover`（674.2 s）/ `build` / `eval-build` / `behavior` / `injection` 全 passed（`make check` 的七目标全在其中） |
| 1.4 控制台 | 通过 | `console-test` 234 用例、`console-build` 产出 `assets/console/dist/assets/index-C8YvltG4.js`（303,176 B）与 `integrity.json` 3 文件；构建后工作区仍干净 |
| 1.5 管理台人工走查 | 通过（客观项）/ 主观免验 | T069 浏览器自动化 19 张截图、setup 7/7 + phase 1 14/14 + phase 2 5/5、`console_errors=[]`；纯主观视觉记「用户裁决免验，不阻断收口」；真实外部服务与诊断瞬间进程树旁证**未验证** |
| 2.1 嵌套 module | 通过 | `make e2e` 前置 `go -C examples/shared/mcp/docs-readonly test ./...` → `ok … 0.667s`；`docs-readonly-tests` passed（10 个子用例，0 skip） |
| 2.2 示例脚本测试 | 通过 | `script-tests`：5 个目录、`Ran 9/13/19/22/24`，累计 87 个用例（每目录 ≥3 下限） |
| 2.3 五 Demo 与八模板 | 通过（夹具层） | `demo-fixtures`（`TestUS14Demos_Fixture*`，含 D1 二次去重与星差）与 `template-quality`、`e2e-required` 内的 `TestTemplates_LoadAndTools` / `TestTemplates_TierEvidence` 全 passed；**真实外部实跑未授权、未验证** |
| 2.4 公开增量 | 通过 | `website-test` / `website-build` / `website-check`（Links / Diagrams 无 finding）；`docs-consistency` passed |
| 3.1 发布专用目标 | 通过 | `e2e` 259.1 s：`binary_source=candidate` + `binary_sha256=fb022a01…`；七场景（含取消 / 工具超时两独立用例）全绿 |
| 3.2 stage 与干净检出 | 通过 | 本 stage 37/37；`clean-checkout` 1203.5 s passed（内层 acceptance + e2e 全绿）。注：stage 内 `acceptance` 包装检查已按用户裁决（§2.2 #10）移除，公共十项由 stage 整体运行承载 |
| 3.3 发布证据 | 通过（tag 除外） | 记录已发布（上一条）；双平台走查、依赖与许可表、升级 / 恢复演练读数在 §2.1；`v0.1.0` tag **未验证**（用户手动） |
| 4 边界与失败语义 | 遵循 | 外部服务零实跑；SKIP / 空匹配不判通过；判据面变更均停下裁决（§2.2 12 条） |
| 5 未验证项 | 已更新 | 见 §4（外部服务、真实自动触发、tag、MCP 包信息、Linux 浏览器交互、诊断进程树旁证仍为未验证） |

**补充读数（收口独立复核 F1–F6 处置，2026-10-26）**：首轮独立复核结论 = **带条件通过**，条件为 F1（B8 的具名证据缺失）。逐条核实后的处置与读数：

| id | 严重度 | 核实结论（复核者原判） | 处置 | 本轮证据 |
| --- | --- | --- | --- | --- |
| F1 | MEDIUM | 模块卡 C-13 / C-19 与 §6.3 B8 点名的 `TestExamples_PortableBaseline` 在仓库中不存在；只能靠人工读文件，不能机器判卷 | **已修复**：新增 `internal/e2e/us14_examples_test.go` 的同名测试（无 UID / 凭据、`notify_mode: none`、`resource_bindings` 空、`deliver_to` 不在 frontmatter、五份 SETUP 六个编辑项、无自动导入入口），并带 UID / 凭据 / 占位域三组植入样本逆改点 | 绿态：`go test -tags=e2e ./internal/e2e -run '^(TestUS14Demos_Fixture\|TestExamples_PortableBaseline)$'` → `ok … 101.328s`；红态见下 |
| F2 | LOW | C-20 点名的 `TestMCPEgress_Documentation` 不存在；13 份 README 的段落标题两种写法（「MCP 自身出网范围」/「出网边界」） | **已修复**：同名测试按**内容**判卷（有 MCP 必须写「不受/管不到/不代管」，无 MCP 必须写「受白名单约束」或 `sandbox_denied`），另查 Operations §2.2 四行走廊与 `shell.allowed_commands`；植入缺段 README 必红。对 4 份声明 `\| MCP \| 无` 的模板（weather-brief / arxiv-tracker / policy-watch / dependency-watch）判「出网边界」段写明内置工具受白名单约束（`sandbox_denied`）；9 份含 MCP 的 README（五 Demo + onboarding-buddy / ticket-insights / web-watch / funding-watch）仍必须写「不受 / 管不到 / 不代管」——这是 C-20「每份 MCP 段」在无 MCP 模板上的语义解释，不是取消该段 | 绿态：`go test -tags=e2e ./internal/e2e -run '^(TestTemplates_\|TestMCPEgress_Documentation)$'` → `ok … 5.749s`；红态见下 |
| F3 | LOW | 模块卡 / §4.4 点名的 `TestScheduleDelivery_*` / `TestDeliveryFailure_NoAutoResend` 不存在 | **等价证据，不改测试文件**：同语义断言在 `internal/app/delivery_test.go` 的 `TestDeliveryService_DeliverWritesTerminalState`（:360）/ `_DeliverUnknownIsRecordedNotRetried`（:410）/ `_SkipKeepsRow`（:453）；本轮三条复跑全 passed（`ok … 3.156s`）。名称差异记入收口说明，不新造同义测试 | 绿态：三条具名 PASS（2026-10-26 复跑） |
| F4 | LOW | `scripts/check-release-clean.sh:12` 声称「逐文件比对（内容 / 权限 / 相对路径）」，实际 `diff -r` 不比较权限位 | **已修复注释**：改为「内容与相对路径」，并写明权限位不在本判据内、两棵树同出同一 Git 树对象 | 脚本行为零变更（仅注释） |
| F5 | LOW | `classifyInitializeFailure` 先判 `*url.Error` 再判 deadline；`http.Client` 超时把 `context.DeadlineExceeded` 包在外壳里，会被误报 `http_unreachable`，与 §2.4.3「超时 → timeout」相悖 | **已修复**：deadline 判据前移，新增 `TestClassifyInitializeFailure_DeadlineBeforeURLShell` 四分支 | 绿态：`go test ./internal/tool -run '^TestClassifyInitializeFailure_DeadlineBeforeURLShell$'` → PASS（4/4 子用例）；红态见下 |
| F6 | LOW | `docs-readonly` 的 `resolveDocument` 逐段 lstat 后再 `os.Open`，检查与打开之间可被换文件（TOCTOU） | **已修复**：`resolveDocument` 返回末段 lstat 的 `fs.FileInfo`；`readBounded(path, checked)` 用打开句柄的 `fstat` 做 `os.SameFile` 比对，元信息也取 fstat；新增 `TestDocsReadOnly_RejectsSwappedFile` | 绿态：`go -C examples/shared/mcp/docs-readonly test ./... -count=1` → 8 个用例全 PASS（含新增例）；红态见下 |

**逆改红态（逐字读数，2026-10-26）**：两处均用仓库外临时副本 + `go test -overlay`，逆改后即删（`%TEMP%/us14-redstate` 已确认不存在）。① F5：把 deadline 分支改成 `false && (…)`（等价恢复旧判别）→ `classifyInitializeFailure() = (initialize, http_unreachable)，期望 (initialize, timeout)`；`budget_deadline_with_dial_error` 同步变红 `(initialize, handshake_failed)`。② F6：把 `os.SameFile` 守卫改成 `false && checked != nil && …` → `main_test.go:455: 检查后文件被替换时必须拒绝，实际读取成功`。

**判据面影响**：F1/F2 是新增测试（允许）；F5/F6 是缺陷修复 + 新增回归测试；F4 仅注释；唯一收窄处是 URL 凭据扫描对 RFC 2606 文档域的豁免（§2.2 #13，待用户裁决）。**证据链影响**：本轮源码 / 测试 / stage 均有改动，`11ef243` 上的 gate r2 与候选二进制 `fb022a01…` 不再对应当前树——收口前**必须**有新的用户授权候选提交并重跑全量 stage（r3），否则发布记录与分发产物对不上。

**第二轮独立复核（修复闭环，2026-10-26）**：结论 = **阻断**（F-01 HIGH 使 stage 必失败，另有 F-02–F-06 / F-12 判据与证据缺口）。逐条处置与读数如下；F-12 属收口证据且需用户授权新的候选提交，**未完成**；F-13 转 errata，**不改**模块卡。

| id | 严重度 | 复核发现（复核者原判） | 处置 | 本轮证据 |
| --- | --- | --- | --- | --- |
| F-01 | HIGH | `template-quality` 的 `-run "^(TestTemplates_\|TestMCPEgress_Documentation)$"` 把 `TestTemplates_` 锚成字面量，只执行 `TestMCPEgress_Documentation`；`required_tests` 仍要求两条 `TestTemplates_*`，runner 必报必须失败的测试未执行 | **已修复**：改为 `^(TestTemplates_.*\|TestMCPEgress_Documentation)$`；stage `approval` 追加修订说明并更正原先「未移除任何 `-run` 分支」的错误表述 | `sh scripts/check-acceptance.sh --stage harness/stages/us14-release.json --task T040` → `passed template-quality (exit=0)`；报告 `accept-20261025T171121.163048600` 的 `passed_tests` 含 `TestTemplates_LoadAndTools`（8 条目录用例 + 2 条判别力用例）、`TestTemplates_TierEvidence`（8 条目录用例）与 `TestMCPEgress_Documentation` |
| F-02 | MEDIUM | 无 MCP 分支用 `strings.Contains(…, "受")` 判「受白名单约束」；「不受」同样含「受」，方向写反仍绿 | **已修复**：改为正向三条件（含「受」+「白名单」+「约束」，且不含「不受」），否则必须出现 `sandbox_denied`；新增 `plantedEgressSection(remoteConstrained)` 正反两版植入文本 | 定向绿态：`go test -tags=e2e ./internal/e2e -run '^(TestExamples_PortableBaseline\|TestMCPEgress_Documentation)$'` → `--- PASS` 两条、`ok … 5.722s`；反版植入断言产出 finding |
| F-03 | MEDIUM | 植入树只有 1 份 README 且无 Operations，findings 必然 ≥13 条（12 份「读不到」+ Operations 缺失），`len(findings)==0` 永不成立——缺段判据不被单独证明 | **已修复**：逆改点改为直接调纯函数 `readmeEgressFindings` / `operationsEgressFindings`，断言**精确**结论（缺段样本 → 恰好 1 条且点名该文件；合格样本 → 0 条） | 同上绿态运行：两条精确断言均通过 |
| F-04 | MEDIUM | `docs/Operations.md` §2.2 只有 4 行，缺「远程 MCP 下游」与「stdio 启动程序受 `shell.allowed_commands`」两行走廊；测试把 4 个标签写成固定清单，冻结了缺口 | **已修复**：Operations §2.2 补两行，标题改为「出网：按通道分开看」；测试改判**六行**走廊 | 同上绿态运行：六行逐行判极性通过；`shell.allowed_commands` 另要求落在启动行 |
| F-05 | MEDIUM | `resource_bindings` 只匹配内联 `[...]`（契约示例 `docs/contracts/agent-md.md:40-44` 本身是块式）；UID 只匹配大写——块式 + 小写可同时绕过 | **已修复**：frontmatter 用既有坐标 `github.com/goccy/go-yaml`（非新增依赖）结构化解析，`nestedEntryCount` 递归判 `resource_bindings` / `notify_channels` 是否为空；UID 正则加 `(?i)`，依据 `internal/core/identity.go` 的 `ParseAgentUID` 接受小写并规范化 | 同上绿态运行：块式非空绑定样本、小写 UID 样本均断言命中 |
| F-06 | MEDIUM | SETUP 判据缺「服务名」，用 `allowed_commands` 代「解释器路径」，删段留 token 仍绿 | **已修复**：六个独立判据——不自动导入声明、模型绑定、MCP 服务名（`mcp_servers.yaml` 且含具体 `- name: <id>` 行）、渠道 `UID`、`deliver_to`、解释器路径（「解释器」与「路径」同在） | 同上绿态运行：五份 SETUP 逐项通过；缺解释器路径的植入样本断言命中 |
| F-07 | LOW | 只查 `examples/` 顶层 `import*` / `install*` 名称与 README 一句；嵌套脚本改名或脚本内放宽白名单不会被发现 | **已修复**：任意深度扫脚本（`.py/.sh/.ps1/.mjs/.js/.bat/.cmd`），顶层再判非脚本同名项；脚本内容扫 `mcp_servers.yaml` / `allowed_commands` / `allowed_domains` / `shell.interpreters` 四个禁用 token；README 必须出现 `imported automatically` 或「不会自动导入」 | 同上绿态运行：嵌套 `import-demo.sh` 与缺声明 README 样本断言命中 |
| F-08 | LOW | 逆改点只覆盖 UID / 凭据 / URL 域；基线字段、SETUP、不自动导入没有逆改点 | **已修复**：每个判据各配植入样本（`notify_mode` / `deliver_to` / `notify_channels` 嵌套 / 块式绑定 / SETUP 六项 / 嵌套导入脚本 / README 缺声明 / Operations 逐行极性） | 同上绿态运行；**未**额外做 overlay 逆改——红态由植入样本在测试内自证（判据失效即断言失败） |
| F-09 | LOW | `TestDocsReadOnly_RejectsSwappedFile` 会随 `^TestDocsReadOnly_` 执行，但不在 `required_tests`；改名 / 移除时六项仍绿 | **已修复**：`docs-readonly-tests.required_tests` 增该测试 | `--task T037` → `passed docs-readonly-tests (exit=0)`；报告 `accept-20261025T171140.672483000` 的 `passed_tests` 10 项含 `TestDocsReadOnly_RejectsSwappedFile` |
| F-10 | LOW | `SameFile` 错误把服务器绝对路径回给 MCP 客户端 | **已修复**：`displayPath`（root 相对 posix 路径，`filepath.Rel` 失败回退 `filepath.Base`）+ `ioFailureDetail`（`*fs.PathError` 只取 `.Err`）；`readBounded(path, display, checked)` 全部错误路径改用 display | 同上 `docs-readonly-tests` 10 项全 passed |
| F-11 | LOW | §2.2 只查标签存在 + 全节关键词，逐行极性（是 / 否）未判；行内容写反也能绿 | **已修复**：六行逐行判第三列前缀（`**是**` / `是` / `**否**` / `否`）；`plantedEgressSection` 提供「远程下游写成受限」与「写成不受限」两版，断言前者红、后者绿 | 同上绿态运行 |
| F-12 | MEDIUM | gate r2 记录绑定修复前的 stage（`e656e01f…`）与 tasks（`8f8c00…`），当前 stage `8212c75c…` / tasks `9e0bcb…` 均不同；修复后无新记录，T064 / T066 / T067 / T068 勾选缺匹配证据 | **未完成（收口前置）**：需用户在第二个本地候选提交上授权重跑全量 stage 并发布 r3 记录（r2 保留为历史行）；本轮只做逐任务复跑与定向 e2e，不构成发布证据 | T037 / T038 / T040 三条 `--task` 复跑 passed（`accept-20261025T171140` / `T171222` / `T171121`）；全量 r3 未运行 |
| F-13 | LOW | 模块卡 §2.8.4 的 `TestExamples_NoSecrets` 在仓库中不存在；§4.4 未列 `TestExamples_PortableBaseline` / `TestMCPEgress_Documentation` | **转 errata，不改模块卡**：卡是 review ① 已批准版本，`sha256 ff659465…` 已冻结在 stage `approval`，改卡会使该声明与实物不符；在 review ② 包中作为 errata 列出，由用户决定是否出 R4 | 本节与 §5 记录；阶段 `approval` 未改哈希 |

**复核者列出的未覆盖面中与本轮相关的三条**：① URL 豁免的用户裁决仍缺（§2.2 #13）；② 完整 stage / `clean-checkout` 未执行（即 F-12）；③ `os.SameFile` / `os.Rename` 的替换测试只在本机 Windows 通过，Linux 未验证（已进 §4 未验证清单）。

### 2.1.1 补充读数：候选提交 `57784fe` 上的双平台走查重跑（2026-10-26，非 stage 判卷）

**为什么重跑**：gate r4 钉在 `57784fe`，而此前的升级 / 恢复演练与 Linux 走查绑的是 `11ef243` 的 `fb022a01…` / `3de73e44…`。`git diff 11ef243 57784fe` 实测改动面 = `internal/tool/mcp.go`（`classifyInitializeFailure` 的 deadline 分支前移，19 行）、`examples/shared/mcp/docs-readonly/main.go`（112 行）、`scripts/check-release-clean.sh`（5 行）与各自测试——都是产品面改动，旧二进制证据不能代表发布候选。

#### Windows（`deploy/basalt.exe` = `bin-new/basalt.exe`，sha256 `946a6b801f78ec15a75119aa57995d6101280de2c9f82b6d31227ac70b435a99`，`basalt version 57784fe`）

- **升级 §5.1 六步**：停 → 备份 → 换二进制（旧文件留作 `deploy/basalt.bak-prev`，sha256 `857c8de2846fc3ec74f2539357f82c7ae28a135877d9e75deb9831876a168eba`，`basalt version e2e285f-pre-us14`）→ 起服 → 验证 → 观察。
- **备份时点读数**：`backup-20261025T1932` 10 个文件（`basalt.db` `24ec4e41aab327258ae16cc4bc8291564b844944d4163394a1603c96c6d9239a`、`basalt.yaml` `1573953c…`、无 WAL / SHM）；计数 `{"messages":2,"runs":1,"runs_completed":1,"llm_calls":1,"llm_calls_success":1,"tool_invocations":0,"management_operations":3,"scheduled_tasks":0,"notify_channels":0,"sqlite_master_tables":30,"schema_version":5}`。旧二进制在换二进制前读过新数据：`status` / `session list` / `profile list` 全 exit 0。
- **换后起服**：`health=200`、`/console/=200`；**无迁移**（日志逐字 `v3 迁移：库已是目标版本，跳过` `schema_version=5`）；`status` 汇总全部通过（schema v5、29 张表）、`session list` `cli-1c5aafb256301130` 2 条、`profile list` 1 个（`registered=true`）、`tool list` **10 件**。
- **chat #2**：exit 0，回复 `drill-ok: 演练实例已连通（假模型，非真实模型）`；计数 `messages 4 / runs 2 / runs_completed 2 / llm_calls 2 / llm_calls_success 2 / tool_invocations 0 / management_operations 3`，库 `4a470380d65dec199574bd1eb3d2857d99b7d0764b14106cb7bf61e8987d8572`；假模型日志 2 行，第 2 次请求带 4 条消息（历史确实跨进程落库读回）。
- **实例锁（候选二进制，逐字）**：`serve` 持锁时 `chat` exit 1 —— `取得实例锁: 工作区实例锁已被占用：pid=3640，开始于 2026-10-25T19:36:55Z——另一个活实例正在运行，写模式命令拒绝启动（服务在跑时请改用 REST / 管理台）`。
- **中文控制台**：`status` / `profile list` / `session list` / `chat` 输出均为中文（Git Bash 里显示为乱码属终端编码，不是程序输出问题）。
- **MCP 目录（14A 端点）**：`GET /api/v1/catalog/mcp` 无 auth **401** / 带 auth **200** / 6 条、按 id 升序（`brave-search`、`dbhub`、`fetch`、`filesystem-readonly`、`github`、`playwright`），字段集 = `args,category,command,display_name,env,id,requires,source_url,summary,transport,url,verification_note,verified,verified_at,verified_platforms,warnings`。

#### 恢复演练 §4.3（CLI 路径：全程不起 `serve`）

- 独立目录 `restore-check/`：`cp -a` 备份的 `.basalt` + `basalt.yaml`；恢复后库 sha256 仍 `24ec4e41…`（与备份逐字节一致）、`basalt.yaml` diff 为空。
- **三隔离**：① `scheduled_tasks=0` 且备份的 `AGENT.md` 无 `schedules` 键；② `notify_channels=0` 且环境无任何 notify / webhook / feishu / lark 变量（未覆盖也未 unset，纯粹不存在）；③ `network_domain_policy` = `('http.allowed_domains','',1,1792956662609,'')`——**空白名单**（最严格收口；配置键不生效，以库为准）。
- **五判据**：`status` 汇总全部通过 / `session list` 2 条 / `profile list` 1 个 / `tool list` 10 件 / `chat` exit 0 走完整链路并回复同一句。**审计增量**：`messages 2→4`、`runs 1→2`、`llm_calls 1→2`（`management_operations` 仍 3）；跑完 chat 的库 `889a15aea68deb002375053bae2c130b8259353301970dbdce65fd415ed35782`。
- **旧二进制读恢复副本**：`status` / `session list` / `profile list` 全 exit 0，且**未改动库**（执行前后 hash 都是 `24ec4e41…`）——回滚可读得到证。
- **三个互不相同的库哈希**：live `4a470380…`、备份 / 恢复副本 `24ec4e41…`、恢复副本跑完 chat `889a15ae…`。

#### Linux（WSL2 Ubuntu，kernel 6.6.114.1-microsoft-standard-WSL2，python3 3.14.4）

- **同一发布 argv 交叉编译**：`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=57784fe" -o basalt-linux-amd64 ./cmd/basalt` → sha256 `0c62c6573eb0d4dfbbba1ade333ebf5c82478298e678551e0263ded2d6a8d58a`（ELF 64-bit LSB，statically linked，stripped），`basalt version 57784fe`。
- **回环**：假模型 Linux 侧 `127.0.0.1:18470`，实例 `127.0.0.1:18471`，`http.allowed_domains: []`。
- **序列**：`init`（11 项 / schema v5）→ `serve` → `health=200` / `/console/=200` / `catalog` 401→200（6 条、同 id 升序）→ `POST /agents`（带 `model`）→ `enable` → 停服 → `status` 全部通过 / `profile list` 1 个（provider `drill-fake` / 模型 `drill-model`）/ `tool list` 10 件 / `chat` exit 0 / `session list` `cli-b3bbc22b4a2ec58f` 2 条。
- **启用代数**：首次 `expected_generation=0` → **409 CONFLICT**，`expected_generation=1` → **200**（`ag_F6YK24Z5EGXXRZ5A6204KXSTVE`，`generation=2`），与 Windows 同形。
- **落库**：`messages 2 / runs 1 / runs_completed 1 / llm_calls 1（success 1）/ tool_invocations 0 / management_operations 3（system.baseline_v2 + agent.create + agent.enable）/ scheduled_tasks 0 / notify_channels 0 / 30 表 / schema v5`；库 `86218c4eb2fd35982a053621a311c404a3a7189ce093bf07427c7359e913cbcc`。
- **零外呼**：假模型请求日志恰好 1 行（`model=drill-model`、`messages=2`）；策略行空 → 除回环外没有任何通道被打开。

**顺带补验（此前只在本机 Windows 验过）**：`examples/shared/mcp/docs-readonly` 嵌套 module 在 **Linux** 上跑通——`go test -count=1 -v ./...` → 8 个顶层用例 + 2 个子用例全 PASS、零 SKIP，含 `TestDocsReadOnly_RejectsSwappedFile`（`os.SameFile` / `os.Rename` 替换守卫）与 `TestDocsReadOnly_RejectsSymlink/real_filesystem`，`ok … 0.004s`。**环境事实**：WSL 自带 go 1.26.0，低于该 module 的 `go 1.27`，故经 `GOPROXY=https://goproxy.cn` 由 `GOTOOLCHAIN=auto` 拉取 go1.27.0 执行（Windows 本仓构建仍用 toolchain go1.27.2）。

**新增观察（不是缺陷，已记 C-050）**：创建后**立即**启用会在注册表快照刷新前返回 404。探针（仓库外 `race_probe.py`，200 ms 轮询）读数：t+77 ms → 404、t+280 ms → 404、t+483 ms → 409 CONFLICT，与 `internal/workspace/watcher.go:32` 的 `watcherDebounce = 300 * time.Millisecond` 吻合。控制台有人工延迟，走查脚本必须自带「404 重试 / 409 读代数重试」。

**本轮未覆盖、不据此宣称通过**：14A 的 `diagnostics` / `test` 端点（需先有已发布 MCP server，两平台走查均未构造）；14B 五 Demo 的真实自动触发与八模板夹具复跑（外部服务零授权）；浏览器交互走查（沿用 T069 轮读数，不在本轮重跑）；真实 `.cmd` / GitHub / Brave / Fetch / Playwright / DBHub 行为。

### 2.2 判据面变更记录（实施期间，只追加）

| # | 日期 | 文件 / 用例 | 旧形态 | 新形态 | 依据（已审输入） | 断言面变化 |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | 2026-10-25 | `internal/app/resource_admin_test.go` / `TestMCPTest_MissingEnvNotConfigured`（US-12 既有） | 先把变量置空，再保存一条引用它的条目（保存必须成功），随后调测试端点 | 先在变量就绪时保存（闸门放行），再把变量置空后调测试端点 | US-14 FR5 + 模块卡 §2.3 图 2-2 / §4.5.1（review ① 2026-10-25 通过）：创建期必须 fail-closed，旧前提已被批准取消 | **零变更**：不拨号 / 零连接器、`failed` + `NOT_CONFIGURED`、503 + `operation_id`、点名变量、不回显占位、恰一行台账，逐条保留 |
| 2 | 2026-10-25 | `internal/app/resource_admin_test.go` / `TestMCPTest_UnconfirmedEntryStillAttempts` | 模块卡 §4.5.13 伪代码用 `connectedCount()` 断"真实尝试" | 改断 `createdCount()==1` 且 `startedProcess()` | 一过性 `Test` 路径的假连接器不调 `OnConnected`，`connectedCount()` 恒为 0，无法判别 | **观测点变更、强度不降**：仍要求"诊断状态为 `unconfirmed` 时连接器被真实调用一次、子进程被启动"；不放松为"只断言没短路" |
| 3 | 2026-10-25 | `internal/web/admin_routes_test.go` / 路由闭集三处（`TestAdminRoutes_*`） | 35 行路由 / 24 条非条件写 / 资源 19 条 | 37 行 / 26 条 / 资源 21 条 | US-14 D-01 + D-02 新增两条只读 GET（T027）；`wantConditionalRouteKeys` **不变**（11 条） | **零变更**：闭集仍是"表内每一行都被断言到"；只是把新增行纳入同一闭集，不减少任何既有行的判据 |
| 4 | 2026-10-25 | `internal/web/views_admin_test.go` / `TestAdminViews_EmptyCollections` | `McpServerTestResultView` 期望 5 键 | 期望 9 键（追加 D-03 四字段） | US-14 D-03（测试记录追加 `operation_id` / `tested_version_id` / `stage` / `failure_class`） | **零变更**：仍是"零值也出现、不做 omitempty"的全键判据，只是键集随契约扩大 |
| 5 | 2026-10-25 | `internal/web/contract_test.go` / `TestOpenAPI_AdminWriteViewsMatchContract` 的 `McpServerTestResult` 样例 | 样例用零值 `Stage`（空串） | 样例带 `Stage: "complete"` | D-03 把 `stage` 定成契约枚举；真实适配器成功路径恒填 `complete` | **零变更**：校验仍是"视图必须通过契约 schema"，改的是样例数据而非校验强度（空串会掩盖枚举判定） |
| 6 | 2026-10-25 | `internal/e2e/us14_demos_test.go` / `testDemoRepoDigest` 日报表格 | 夹具渲染器从 `sources.<源>` 顶层解 `with_unconfirmed_fields`（脚本不在此处发该键，恒得 0） | 从 `sources.<源>.counts` 解同一键 | 脚本契约：`repo_digest.py` 第 504 行把该计数与桶计数写在同一个 `counts` 表；模块卡 §4.5.4 的期望结果 `\| PR \| 6 \| 6 \| 1 \| 1 \|` 逐字保留 | **零变更**：期望值一字未动，只修"从哪读"（原写法恒红，不构成判据） |
| 7 | 2026-10-25 | `internal/e2e/us14_demos_test.go` / `testDemoRepoDigest` 读取次数 | `read_file` 必须恰好 2 次（L2 + L3） | 恰好 3 次，且第 3 次钉死在 `runs/<时间戳>/digest.json` | 同一测试的第 10 步已是「回读产物再写日报」（与 D3 / D5 同构，L2 模板要求数字照抄产物）；旧断言停留在夹具加入该步骤之前 | **强度不降**：前两次仍必须落在同一发布目录（`SKILL.md` / `REFERENCE.md`），第 3 次必须命中产物路径；既不允许任意多次读取，也不允许顺序颠倒 |
| 8 | 2026-10-25 | `harness/stages/us14-release.json` / `acceptance` 检查与 T035 / T057 / T065–T068 / T070 绑定 | 一个 `acceptance` 检查 = `make acceptance`，上述任务全部绑它 | 删除该包装检查；T035 / T057 / T067 → `docs-consistency`（`go test ./internal/cli -run '^TestPublicDocs_UseAgentFlag$'`，`required_tests` 同名）；T065 → `stage-lint`（`go run ./scripts/acceptance --lint-stages --stage …`，取锁前返回）；T066 → `release-records`（`TestRecords_PublishedConform` + `TestRecords_NoSecretsOrAbsolutePaths`）；T068 → `stage-lint` + `e2e` + `clean-checkout`（后两项不变）；T070 → `stage-lint` + `release-records` | 模块卡 §4.2「公共层以 `harness/acceptance.json` 为准，本卡不复制公共清单」；`loadStage` 禁止任务绑 full profile id（`config.go:222-226`）；`make acceptance` 在同一 stage 内自锁（失败台账 #13，`main.go:90`） | **不缩减验收面**：`make acceptance` 执行的就是 full profile 十项；stage 整体运行 = 默认 full profile + 全部 stage 检查 + 全部任务检查（`main.go:121-132`），是它的严格超集，由 T068 与收口运行执行；未删任何测试 / 阈值 / tag / `-run` 过滤 / `required_tests` 条目 |
| 9 | 2026-10-25 | `harness/stages/us14-release.json`（#8 **未采用**） | 拟删除 `acceptance` 包装检查并改绑 `docs-consistency` / `stage-lint` / `release-records` | **已恢复已审原文**：`acceptance` = `make acceptance`；T035 / T057 / T065–T068 / T070 绑定不变 | 判据面变更须用户裁决（EngineeringStandards §2 单一追问句：改后原本红的任务会变绿）；自动模式分类器已拦截改写后的任务判卷 | **零变化**：未改任何检查、绑定、测试或阈值；stage 内自锁问题留待裁决（失败台账 #13） |
| 10 | 2026-10-25 | `harness/stages/us14-release.json`（#8 **已采纳**，用户裁决） | `acceptance` = `make acceptance`，T035 / T057 / T065–T068 / T070 绑它 → 在 stage 判卷下必然自锁（#13） | 删除 `acceptance` 检查；新增 `docs-consistency`（`go test -json -count=1 ./internal/cli -run '^TestPublicDocs_UseAgentFlag$'`，`required_tests` 同名）、`stage-lint`（`go run ./scripts/acceptance --lint-stages --stage harness/stages/us14-release.json`，取锁前返回）、`release-records`（`go test -json -count=1 ./scripts/acceptance -run '^TestRecords_(PublishedConform\|NoSecretsOrAbsolutePaths)$'`，两条 `required_tests`）；T035 / T057 / T067 → `docs-consistency`，T065 → `stage-lint`，T066 → `release-records`，T068 → `stage-lint` + `e2e` + `clean-checkout`，T070 → `stage-lint` + `release-records` | 用户 2026-10-25 裁决采纳 §2.2 #8；`main.go` 的 `--lint-stages` 在配置加载与独占锁**之前**返回（只读，不产报告），三个检查引用的测试 / 旗标均已核实存在（`internal/cli/public_docs_test.go:14`、`scripts/acceptance/records_test.go:525/577`、`scripts/acceptance/main.go:30`） | **不缩减验收面**：`make acceptance` 执行的就是 full profile 十项；stage 整体运行 = 默认 full profile + 全部 stage 检查 + 全部任务检查（`main.go:121-132`），是它的严格超集，由 T068 与收口运行执行；未删任何测试 / 阈值 / tag / `-run` 过滤 / `required_tests` 条目。stage `approval` 字段追加修订说明（长度 4511 → 5277） |

| 11 | 2026-10-25 | `harness/stages/us14-release.json` / `stage-lint` 的 `timeout_seconds` | 300 秒 | 1800 秒 | 实测读数：`--lint-stages` 扫描全部 11 个 stage（160 检查 / 约 120 条唯一 `go test -list`）在本机耗时 **441 秒**（冷缓存 + 并发负载下测得，且该轮因中途编译错误提前中止，完整扫描只会更久）；300 秒会让该检查在正常机器上**必然**超时失败。超时是资源边界而非判据：检查命令、`required_tests` 语义、失败条件一个未改 | **零变更**：不涉及断言 / 阈值 / `-run` 过滤 / 跳过；1800 秒与同 stage 内 `e2e` / `demo-fixtures` / `clean-checkout` 的重型检查同级 |
| 12 | 2026-10-25 | `internal/web/handlers_resources_test.go` / `TestResources_GetMCPServer` + `TestResources_ViewShapesMatchContract`（US-11 既有） | 详情响应体断言「逐字 `McpServerView`」；`TestResources_ViewShapesMatchContract` 用 `McpServerView` 的字段集比对 MCP 详情载荷 | 断言「详情体 = 契约 `McpServerDetail`（视图 + `args` + `url`），ETag 仍只经响应头出站、无 `etag` 字段、无 env 值」；字段集改用 `McpServerDetail` 比对，无指针条目 `args` 出站为 `[]` | 用户 2026-10-25 裁决：按契约补齐（openapi `McpServerDetail` 自 US-11 起即声明 `args` / `url`；US-11 模块卡约束 23「回显 args / url 时按契约 McpServerDetail」）。原「逐字视图」是 US-11 review ② 决定 #5 的落地形态，与开放契约本身冲突，详情页「修改」表单因此崩溃（失败台账 #20） | **强度不降**：字段集仍与契约**完全一致**（既不多也不少）、schema 校验保留、ETag 只经响应头、无 env 值与命令原文；新增 `args` / `url` 的正向断言与无指针时 `args=[]` 的形态断言。改变的只是"契约的哪一份 schema 是判定基准"（`McpServerView` → `McpServerDetail`），即把实现拉回已冻结契约，不减少任何原有判据 |

| 13 | 2026-10-26 | `internal/e2e/us14_templates_test.go` / `templateScanSecrets` 的 URL 内嵌凭据探针 | `https?://user:pass@host` 一律命中（含 RFC 2606 文档域） | 拆出 `templateURLCredentialPattern`；`example.com` / `example.net` / `example.org` 及其子域豁免，其余主机仍命中 | 复核 F1：`examples/daily/industry-brief/README.md:236` 与 `scripts/test_industry_brief.py:223` 是「必须拒绝带凭据 URL」的反例夹具（`u:p@example.com` / `user:secret@example.com`），不是可用凭据；RFC 2606 明定这些域只用于文档 | **收窄一处误报、真实凭据判据不降**：非占位域由植入样本 `user:secret@intranet.corp` 证明仍必红；GitHub / OpenAI / AWS / Slack / 私钥五类探针未动。**待用户裁决**：这是本轮唯一一处让原本会红的输入变绿的判据语义调整 |
| 14 | 2026-10-26 | `harness/stages/us14-release.json` / `demo-fixtures` 与 `template-quality` | `demo-fixtures` 只跑 `^TestUS14Demos_Fixture`；`template-quality` 只跑 `^TestTemplates_` | `demo-fixtures` 的 `-run` 增 `TestExamples_PortableBaseline` 并加入 `required_tests`；`template-quality` 增 `TestMCPEgress_Documentation` 并加入 `required_tests` | 复核 F1/F2：模块卡 C-13 / C-19 / C-20 点名的证据必须有机器判卷入口；新增测试本身是允许的，绑定只增不减 | **只增不减**：原有 `-run` 分支、`required_tests` 条目、超时与其余 35 项检查均未改；stage `approval` 追加修订说明 |
| 15 | 2026-10-26 | `harness/stages/us14-release.json` / `template-quality` 的 `-run`（#14 **自身缺陷**的更正） | `^(TestTemplates_\|TestMCPEgress_Documentation)$`——`$` 把 `TestTemplates_` 锚成字面量，实测只匹配 `TestMCPEgress_Documentation`，`required_tests` 里的两条 `TestTemplates_*` 永不执行、runner 必失败 | `^(TestTemplates_.*\|TestMCPEgress_Documentation)$` | 复核 F-01；`go test -list` 与 `-json` 实测两版匹配面 | **恢复原判据**：`-run` 原意就是跑全部 `TestTemplates_*`；`required_tests`、超时与其余检查一字未改。更正 #14 与 stage `approval` 中「未移除任何 `-run` 分支」的错误陈述 |
| 16 | 2026-10-26 | `harness/stages/us14-release.json` / `docs-readonly-tests.required_tests` | 9 条 | 10 条（增 `TestDocsReadOnly_RejectsSwappedFile`） | 复核 F-09：新增的 TOCTOU 守卫必须有控制侧锚点，改名或移除时不得静默变绿 | **只增不减** |
| 17 | 2026-10-26 | `internal/e2e/us14_examples_test.go` / `TestExamples_PortableBaseline`（C-13 / C-19，B8） | 上一轮新增版本：内联 `resource_bindings` 正则、仅大写 UID、SETUP 四个 token 代理、只扫顶层 `import*` 名称 | 结构化 YAML 解析判空（含块式与 `notify_channels`）、大小写不敏感 UID、SETUP 六项独立判据（含具体 `- name:` 服务名行与「解释器路径」）、任意深度脚本 + 四个禁用 token、README 声明必判；每个判据配植入样本 | 复核 F-05 / F-06 / F-07 / F-08；`docs/contracts/agent-md.md:40-44`（块式示例）；`internal/core/identity.go`（`ParseAgentUID` 接受小写） | **判据加强**：旧版对块式绑定与小写 UID 漏检（会绿），新版必红；测试本身是本轮新增（原本不存在），不属于放宽 |
| 18 | 2026-10-26 | `internal/e2e/us14_examples_test.go` / `TestMCPEgress_Documentation` + `docs/Operations.md` §2.2 | 只查 §2.2 的 4 个行标签、全节「白名单」与「管不到 / 不受」；`shell.allowed_commands` 只查全文出现 | 六行走廊逐行判第三列极性（是 / 否 前缀），启动行另须点名 `shell.allowed_commands`；Operations §2.2 补「远程 MCP 下游」「stdio 启动程序」两行，标题改「按通道分开看」 | 复核 F-02 / F-03 / F-04 / F-11；模块卡 §2.8.3 七行表与 C-20 | **判据加强**：4 行 → 6 行、极性逐行判，方向写反必红；Operations 增两行属文档补齐，不改已批准范围 |
| 19 | 2026-10-26 | §2.2 #13 的收窄（URL 内嵌凭据对 RFC 2606 文档域豁免） | #13 记录状态为「**待用户裁决**」 | **用户裁决：采纳豁免**；stage `approval` 同步记为已裁决（`example.com` / `example.net` / `example.org` 及子域豁免，其他主机仍命中） | 用户 2026-10-26 在 US-14 收口对话中明确选择「采纳豁免」 | **无新增判据变化**：#13 所述收窄即最终形态；非占位主机由植入样本 `user:secret@intranet.corp` 持续证明必红 |
| 20 | 2026-10-26 | `internal/e2e/us14_demos_test.go` / `testDemoRepoDigest`（两处）与 `d5RequireRun`（一处）的代投计数 | 运行收尾后**读一次**就断言：`demoDeliveryStatus` 必须已经是 `sent`、接收端累计数必须等于期望 | 先调 `waitDeliveryStatus(t, h, runID, "sent", 30s)` 等代投行离开 `pending`，**再**跑原样的 `requireNotifyCount` + 原样的 `demoDeliveryStatus == "sent"` 断言（原断言一行未删） | 失败台账 #23：调度器 `finishRun` → `settleDelivery` 有窗口，`waitCronRun` 的判据只到 `last_status`；本地 3 轮 D5 命中 1 次首读 `pending`（gate r3 即在此窗口误判） | **判据零削弱**：期望值与终态集合一字未改，仍要求「恰好 1 条代投、状态 `sent`、接收端恰好 N 条」；改动只把「读一次」换成「有界等终态再读」，且 `failed` / `unknown` / `skipped` 仍立即判红（比超时更早、读数更具体）。属同步缺陷修复 + 等价重构，不是阈值放宽 |
| 21 | 2026-10-26 | `internal/e2e/us14_demos_test.go`（D3 `execute_sql` 夹具与 `demoDBHubEnvelope`）、`internal/e2e/us14_templates_test.go`（`templateCandidateFaces["dbhub"]`）、`examples/daily/biz-daily/**`、`examples/templates/ticket-insights/**`、`internal/catalog/data/mcp_catalog.yaml` | D3 夹具按旧配方假想：`execute_sql` 返回自由文本统计结果；dbhub 只读工具面记“候选未测”；素材与目录配方写 `--readonly`（该版本已移除） | 按 2026-10-26 本机实测 @bytebase/dbhub 1.5.0 对齐：`tools/list` 只报 `execute_sql`（入参 `{sql}`）；成功返回 `{"success":true,"data":{"statements":[{sql,rows,count}],"source_id":"default"}}`；写操作返回 `isError=true` + `code=READONLY_VIOLATION`。夹具改为该信封（`demoDBHubEnvelope` 由固定行集构造），`templateCandidateFaces["dbhub"] = {"execute_sql"}` 记已测；素材 / 配方改 `--transport stdio --config`，只读由 `dbhub.toml` 的 `[[tools]] readonly=true max_rows=500` 表达 | 用户 2026-10-26 裁决（`AskUserQuestion`）：“批准现在一并修正，固定使用已实测的 DBHub 1.5.0……不只替换工具名，还要按真实 tools/list 和调用结果对齐参数、返回结构；只读模式使用该版本实际支持的 dbhub.toml 配置，并验证写操作确实被拒绝，不能仅靠提示词限制。” | **判据面变更**：D3 夹具由自由文本改为真实信封；`d3FixtureHash` 更名 `d3InputHash` 并按实测重钉 `84f64efe…`；业务断言（15 / 3 / 2 / 4、净额 10700.50、零通知）与只读拒绝断言一字未删；模板候选面 dbhub 由“候选”升为“已测”，brave / fetch / playwright 仍为候选。依据为本机真实调用证据（2026-10-26），非提示词推断 |
| 22 | 2026-10-26 | `examples/shared/mcp/docs-readonly/`（布局与发行方式）、`internal/cli/demo.go` / `demo_test.go`、`internal/e2e/us14_demo_install_test.go`（新增）、`harness/stages/us14-release.json`、FR-018 / C-16 / README / SETUP / 权威正文 | 嵌套 module（自带 `go.mod`，不进根 `go test ./...`，安装器只打印源码路径） | 主 module 内 `package main`（纯标准库、零第三方坐标、不被产品二进制导入、随根 `go test ./...` 测试）；安装器把 `go:embed` 的源码**实际导出**到 `<ws>/demos/src/docs-readonly` 并生成最小 `go.mod`（`module basalt-docs-readonly` + `go 1.27`），报告里的 `go -C … build -o … .` 按实际路径渲染；仓库不保留第二份源码副本 | 用户 2026-10-26 裁决（`AskUserQuestion`，选“并回主 module”）：嵌套 module 使 `go:embed` 无法收录源码，发行包里缺少素材。实测 `go list -f '{{range .EmbedFiles}}{{println .}}{{end}}' ./examples`：嵌套形态 42 个文件、`docs-readonly` 占 0 个；并回主 module 后 44 个，含 `shared/mcp/docs-readonly/main.go` 与 `main_test.go` | **只增不减**：新增 `demo-installer` 检查（`./internal/cli -run ^TestDemoInstall_`，10 条 `required_tests` 含四条新锚点）；`demo-fixtures` 的 `-run` 与 `required_tests` 增 `TestUS14DemoInstall_Artifact`（制品级：只用构建后二进制 + 已声明 Go 工具链，执行报告打印的构建命令、真实 MCP 连接、工具发现、一次文档读取）；`protect` 增 `examples`；T071–T078 绑定第二轮工作。未删任何测试 / 阈值 / `-run` 分支 / `required_tests` 条目；`docs-readonly` 用例集合与断言逐条不变，只是改由根 module 执行 |

| 23 | 2026-10-26 | `internal/app/ports_test.go` / `TestToolListerPort_HeavyAssemblyRegistersTenTools` → `…ElevenTools`、`internal/e2e/bootstrap_test.go` / `testBootstrapToolList`、`internal/app/wire.go` 的 `declaredToolNames`、`harness/stages/current.json` | 重路径装配池**精确集 10 件**；`basalt tool list` 冒烟按 10 个名字；启动不变式按 10 比 | **11 件**：插入 `web_search`（字典序位于 `shell` 与 `write_file` 之间）；用例改名 `…ElevenTools`；`tool list` 冒烟与 `declaredToolNames == registry.Names()` 不变式按 11 比 | 用户 2026-10-26 §2.5.4 裁决（D5 检索从 Brave MCP 改为内置 `web_search`）+ 契约 `docs/contracts/agent-md.md` §5「内置工具契约（11 个）」 | **只增不减**：原有十个内置名与注册顺序语义逐条保留，第十一件必须同时出现在注册池、`declaredToolNames`、`tool list` 输出三处；改名同步进 `harness/stages/current.json`（已更新）。历史 stage（`us8-notify-r1` / `us10-scheduler`）保留旧名被 `--lint-stages` 只告警——历史不静默改写 |
| 24 | 2026-10-26 | `internal/e2e/us14_demos_test.go` / `testDemoIndustryBrief`（D5）与夹具的 Brave 规格位 | 「Brave Search MCP + Fetch 双 MCP」：夹具提供 brave 规格替身，剧本消费 `brave_web_search` 结果并断言来源表非空、引用自检通过 | 内置 `web_search` + Fetch；端点 `https://api.deepseek.com/v1/tools/search` 是**编译期常量**、`http.proxy` 会因 TLS 校验失败、build tag 覆写会改变产物——进程面没有可替换的成功路径。夹具把 `api.deepseek.com` 写进 `http.denied_domains`，进程级只判「检索不可达」：`search.json` = `{"queries":[]}`、`pages.json` = `{"pages":[]}`、零来源零引用零 URL、失败行逐字点名禁止域名与 `api.deepseek.com`、`web_search` 1 次 / `fetch` 0 次、Run `completed` 且失败工具行恰 1 条、代投恰 1 条 | 用户 2026-10-26 §2.5.4 裁决（改内置工具）+ `industry_brief.py` 的零来源契约（`build_brief` 第 343 行 / `verify_report_text` 第 360 行）；成功一侧由 `TestWebSearch_*`（11 只）与 industry-brief 的 24 只脚本单测覆盖 | **成功路径从进程级移出、失败路径逐条新增**：不删任何断言，改的是「进程级能观测到哪一侧」。此前该子用例因 T125–T129 的示例改动而**真红**（读 `brave_web_search` 报「工具未在本 Agent 的 tools 声明中」并超时），本轮修的是过时剧本，不是放宽阈值 |

| 25 | 2026-10-26 | `harness/stages/current.json`、`scripts/acceptance/stages_test.go`（`pinnedStageChecks` / `pinnedRequiredTests` / 两处断言） | `current.json` 的 `superseded_checks` 为空，历史检查 `008-ports-nine` 被直接改名为 `008-ports-eleven`；历史检查 `010-app-tools` 被**就地**改写成 11 件工具的参数与 `required_tests` | 两条显式 supersession：`008-ports-nine → 008-ports-eleven`、`010-app-tools → 010-app-tools-eleven`；替换 id 各自绑定 `stages_test.go` 里冻结的 `required_tests`（新增 `010-app-tools-eleven` 的 pin），替换断言改为查 pin 表而非写死单测名单 | §2.2 #23（同一批工具改名）+ 仓库既有机制 `superseded_checks`（`scripts/acceptance/stages_test.go` 的冻结历史 stage 不变式）+ US-10 的检查更名前例；本轮 `go test ./scripts/acceptance -run '^TestStages_'` 先红后绿（红态读数：替换 id 缺 pin、历史 id 被逐字复制） | **零削弱**：历史检查仍必须被覆盖、旧 id 仍不得逐字复制、替换必须恰好绑定其冻结 `required_tests`（新增"替换 id 必须有 pin"这一条守卫）；改的是登记方式，不是任何被断言的场景或阈值 |
| 26 | 2026-10-26 | `internal/tool/execution_profile_program_dir_test.go`（D3 探针夹具） | 探针子进程**不带任何环境变量**；`go test -cover` 插桩形态下 `all_three_conditions_admit` / `empty_manifest_does_not_block_explicit_targets` 真红（期望退出码 0，实得 2） | 新增 `probeCommandEnv(t)`，探针经 `core.Command.Env` 得到 `TEMP` / `TMP` / `TMPDIR` 三个指向临时目录的键；退出码断言与 `require.NoError` 一字未改 | 实测根因：执行器只透传声明过的环境变量（`mergeChildEnv`），而插桩测试二进制退出时 `testing/internal/testdeps.coverTearDown`（`GOROOT/src/testing/internal/testdeps/deps.go:236`）在 `-test.gocoverdir` 缺省时调 `os.MkdirTemp("", "gocoverdir")`，无 `%TEMP%` 则回落到 `C:\WINDOWS` 并因权限失败退出 2 | **零变更**：仍然要求探针退出码 0；只给插桩形态一个可写临时目录，未插桩时无副作用（无 D3 判定涉及环境变量） |
| 27 | 2026-10-26 | `internal/e2e/mcp_test.go` / `testMCPEnvNegative`（`unset` / `empty` 两个子用例） | 引用未设置 / 空值变量 → 整个 `serve` 拒绝启动（`ExitCode != 0`），逐字断言 `mcp_servers.yaml 的 server helper 的 env.BASALT_MCP_HELPER 引用的 BASALT_E2E_MCP_UNSET 未设置或为空` | 退出码 0；只跳过该 server：WARN「MCP server 凭据不可用，本次启动跳过该 server」+ `server=helper` + 「槽位 BASALT_MCP_HELPER 引用的 BASALT_E2E_MCP_UNSET 未设置或为空」；被跳过者不注册工具（`requireAbsent("回显 text 参数")`）、内置工具仍 11 件；`plaintext` 子用例保持失败关闭、逐字未改 | 14D §2.5.4 E3（review ① 2026-10-26 通过）+ `internal/app/mcp.go` 跳过路径；U-2 第 6 项原意是「占位失败不得拿字面量起进程」，E3 把失败面从"整进程"收窄到"该 server" | **判据随已批准语义迁移，不缩减**：旧两条（整进程失败 + 点名变量）在新语义下互斥；新断言把「点名变量、不回显值、不建连接、不注册工具」逐条保留，另加「其余 server / 内置工具不受影响」与「恰好 11 件」；明文 env 仍必红 |
| 28 | 2026-10-26 | `internal/e2e/notify_test.go` / `testNotifySentinelRedaction`、`testNotifyDomainDenied`；`internal/e2e/toolsandbox_test.go` / `testToolSandboxDenied`、`testToolSandboxPortRedaction` | 四只用例的配置只写 `http.allowed_domains`，依赖"白名单是外呼前置"这一默认 | 四处配置显式加 `execution:\n  profile: strict`（新实例默认 standard，standard 下白名单不是外呼前置） | 14D §2.5.4 G1/G2（review ① 2026-10-26 通过）；`internal/cli/config.go` 的 `execution.profile` 只对无行库播种，之后以库为准 | **前置形态适配、断言逐字保留**：四只用例判的分别是「哨兵脱敏」「域名拒绝」「四档拒绝」「端口档先于域名档」；不钉 strict 时拒绝理由会换成基础防护或真实连接失败。`testToolSandboxPortRedaction` 的第 ④ 档（显式 `https:443` 放行到域名档）同样要求 strict |
| 29 | 2026-10-26 | `internal/e2e/bootstrap_test.go` / `testBootstrapInitIdempotent` + `testBootstrapStatusNoConfig`、`internal/e2e/v3_test.go` / `requireFreshSchema`、`internal/e2e/web_test.go` / `testWebToolCatalog` | schema v5 / 29 张用户表；`tool list` 十件；`/tools` 目录 13 项 | schema v6 / 33 张用户表（v1 十一 + v2 七 + v3 八 + v4 两 + v5 一 + v6 四）；`tool list` 十一件；`/tools` 14 项（11 内置 + docs-readonly 三件） | 14D schema v6 四张新表（§6.4 实施记录）+ §2.2 #23 的 `web_search`；与 `internal/storage/migrate_test.go` 的 `contractTables` 同口径 | **只增不减**：都是"当前基线的事实数"随已批准迁移 / 工具扩充同步；断言仍是精确相等（不是包含），表数与工具数写死在判据里 |
| 30 | 2026-10-26 | `internal/e2e/us14_demos_test.go` / `TestLegacyD1_HttpEndpoints`（第三档读数）与夹具 | 第 2、3 次 `http_get` 都归 `external_error`（`HTTP <code>：<host>` / `DNS 解析失败：<target>`），用一次循环断两档 | 第 2 次单独断 `external_error` + `HTTP 500：localhost`；第 3 次改断**策略拒绝**：`failed` + `sandbox_denied` + `目标域名无法解析` + `demo-unreachable.invalid`；夹具另落两行 `ip_port` 访问例外（`127.0.0.1:80` / `[::1]:80`），让旧 D1 的回环走查在基础防护下仍可达 | 14D §2.5.4 基础防护（2026-10-26 口径）：DNS 解析发生在**建连之前**，解析失败即拒且与被禁止不作区分（`internal/tool/netguard.go` 冻结文案）；例外表只匹配**已解析**地址，恢复不了「不可解析域名走外部错误」这条老路径 | **改判而非放宽**：三档仍可区分（200 / 5xx 外部错误 / 策略拒绝），第 3 档另逐字点名 `demo-unreachable.invalid`；旧期望在新语义下不可达（解析先于建连），保留只会永远红 |
| 31 | 2026-10-26 | `internal/e2e/toolsandbox_test.go` / `testToolSandboxPortRedaction` 的哨兵串；`assets/console/tests/sandbox-allowlist.test.ts` 两处顺序判据 | 口令哨兵用裸词 `secret`；控制台两处 `calls.map(methodOf)` 的精确序列断言在 14D 新增 `/api/v1/execution-policy` GET 后失效，改为计数式（`writes == ['PUT']` / `PUT 次数 == 1`） | 哨兵改为唯一串 `port-e2e-secret-4d1c`；控制台恢复**有序**判据并加路径维度——过滤到 `/api/v1/sandbox` 与 `/api/v1/sandbox/network-domains` 后断 `['GET /api/v1/sandbox', 'PUT /api/v1/sandbox/network-domains', 'GET /api/v1/sandbox']`（412 用例）与 `['GET /api/v1/sandbox', 'PUT /api/v1/sandbox/network-domains']`（400 用例） | 14D 实例加密日志新增 `step=secrets` 字段（裸词误伤，读数见 §2.1）；控制台新增档位 / 禁止域名 / 例外表读取（T119） | **判据加强**：唯一哨兵只可能来自被测 URL，比裸词更强；控制台从"次数"回到"次序 + 路径"，多写 / 少读 / 顺序颠倒 / 换端点都会红，覆盖范围超过原 `['GET','PUT','GET']`（原式不含路径） |
| 32 | 2026-10-26 | `harness/stages/us14-14d.json`（`approval` 追加 ADDITIVE REVISION 4） | 17 项检查 | 20 项：新增 `script-tests`（`py -3 scripts/run-example-tests.py`，600s）、`template-quality`（`-run "^(TestTemplates_.*\|TestMCPEgress_Documentation)$"`，required 三条）、`legacy-d1`（`-run ^TestLegacyD1_HttpEndpoints$`，required 同名）；`args` / `required_tests` 逐字取自 `us14-release.json` | 14D 自身 stage 对三处被本增量改动的面没有锚点：产业简报 24 只业务计数脚本用例、出网边界文档测试（本轮真的抓到 industry-brief README 标题回退）、旧 D1 回环走查（访问例外）。三项都是**新增检查**，不改既有检查 | **只增不减**：未删任何检查、`required_tests`、阈值、`-run` 分支或任务映射；`approval` 追加修订说明（含理由与时间）。T124 运行前以 `--lint-stages` 校验 20 项均可加载 |

> 该变更不是删断言或放宽阈值（断言逐字保留），而是把前置形态调整到已批准接口允许的唯一路径；旧形态在新契约下已不可达。

## 3. 停止预算与计数（跨会话从本段恢复）

| 项 | 预算 | 已用 |
| --- | --- | --- |
| 同一问题修复次数（不收敛即 `/debug`） | 3 | 0 |
| `/debug` 内同一问题假设数 | 3 | 0 |
| 本 US 的 `/debug` 轮次 | 2 | 0 |
| 无进展判据 | 相邻两次尝试无新读数 / 新假设 / 新定位 → 立即停 | — |

## 4. 未验证清单（模块卡 §6.7 基线，随实测更新；「未验证 → 已验证」须注日期与证据）

- 全部真实外部服务（模型、GitHub、Brave、Fetch、Playwright、DBHub）**未授权、未实跑**。
- 五个 Demo 的真实自动触发 / 每日计划现场核对**未执行**。
- 八个模板的「可复跑（夹具）」**未执行**；A / A\* / B 为计划档位。
- Windows 与 Linux 平台走查——**已验证（2026-10-25）**：**Windows** 侧 = 候选二进制 `34406c4f…56b8` 上的升级 / 恢复演练与 T069 浏览器自动化走查（19 张截图），读数见 §2.1 对应补充读数；**Linux** 侧 = WSL2 Ubuntu 26.04 LTS 上用发布同一 argv 交叉编译的二进制 `da3bb7f9…784c` 走完 `init` → `profile create` → `serve`（`/` 302 → `/console/` 200、入口脚本 303,176 B 与 Windows 产物同字节数）→ 鉴权 401/200 → 绑定并 `enable` → `status` 全通过 → `tool list` 10 件 → `chat` → `session list` / `profile list` → 落库计数，全程零外呼，详见 §2.1「T066 Linux 平台走查」。**不覆盖、且不据此宣称通过**：Linux 上的浏览器交互走查（WSL 无浏览器，只验到静态资源出站与字节一致）；两端均未触碰真实外部服务；**Linux 二进制与发布记录须在用户提交候选提交后按新 `git describe` 重建**（SHA-256 会变）。
- `v0.1.0` tag / 发布**未执行**（tag 归用户手动，AI 不代打）；发布候选 SHA-256——**部分产生（2026-10-25）**：Windows 候选 `bin/basalt.exe` = `34406c4f7958d5f0e21da54050b275f9301459f3dd6801bdcdffd883728c56b8`、Linux 交叉编译 `da3bb7f9ea3f09c9d073751bee13a450ad7d4173b60a2b177795a6085c17784c`，**两者都携带 `+dirty` 版本戳**（主模块 stamp `v0.0.0-20261024172006-e2e285fa45e6+dirty`）——**候选提交后必须重建并重算**，发布记录不得引用这两个哈希。
- 官网 / README 增量**未执行**（US-13 review ② 门已于 2026-10-25 解除；待 T058 / T059 执行并留证）。
- MCP 包版本 / 许可 / 入口路径 / 工具名**未核实**；目录 `verified` 全为 false。——**依赖与许可表的其余四列已验证（2026-10-25）**：Go 24 个链接模块（4 Apache-2.0 / 12 BSD-3-Clause / 8 MIT，链接集合内无 GPL / AGPL / LGPL / MPL）、Node（Vue 3.5.44 MIT 随 `dist` 进二进制；其余开发 / 站点依赖不进产物）、Python（Demo 零第三方）、仓内自研素材（`docs-readonly` 零 require）——逐项读数见 §2.1「T066 依赖与许可表」；**MCP 六条仍是「未核实」**（未安装、零外部授权），发布记录须原样保留该状态。
- `args` 占位符**不支持**（代码事实）；凭据 DSN 配方不可用。
- 教育学习 / 公开市场信息两类能力**未覆盖**。
- 技能 L1 每轮注入的 per-Run 持久证据**不足**。
- e2e 热加载 / 取消 / 工具超时新测试**未实现**；`make e2e` / `us14-release` stage **未创建**。——**已验证（2026-10-25）**：`internal/e2e/us14_release_test.go` 四条落地，`make e2e` 在 `accept-20261025T035216.740018600` 中 passed（236883 ms，81 条具名用例），热加载 / 取消 / 工具超时三条另有逆改红态（同报告补充读数）；`harness/stages/us14-release.json` 已创建并加载为 `us14-release-r1`。
- 管理台**浏览器走查**（目录页 / 预填交互 / 503 后打开操作详情 / 诊断不启动进程 / `.cmd` 未确认 / `pending_restart` 与连接状态并存 / `tool_disposition` 内置赢家 / 空工具清单）——**已验证（2026-10-25）**：用户裁决的验证方式 = 浏览器自动化实跑（真实实例 `ws2`），setup 7/7、phase 1 14/14、phase 2 5/5，三段 `console_errors` 均为 `[]`；19 张截图（`shots/01-overview.png` … `19-ghost-applied-unconnected.png`）与三份 `result-*.json` 见 §2.1「T069 浏览器走查」补充读数；同轮发现并修复两处实现缺陷（失败台账 #20 / #21，均有逐字红态）。**纯主观视觉评价**记「用户裁决免验，不阻断收口」，**不**记为人工验证通过；**仍不构成通过、单列**：诊断点击瞬间的进程树人工旁证（自动化只取点击前后子进程集合差）、真实 `.cmd` 与真实 GitHub / Brave / Fetch / Playwright / DBHub 行为（走查用同名假 server，外部服务未授权）。

- **`docs-readonly` 的文件替换守卫——平台覆盖**（2026-10-26 复核未覆盖面）：`TestDocsReadOnly_RejectsSwappedFile` 依赖 `os.Rename` 覆盖已打开文件与 `os.SameFile` 的身份比对，本机 Windows passed。**已验证（2026-10-26）**：在 WSL2 Ubuntu 上跑该嵌套 module 全量用例 → 8 个顶层 + 2 个子用例全 PASS、零 SKIP，`TestDocsReadOnly_RejectsSwappedFile` 与 `TestDocsReadOnly_RejectsSymlink/real_filesystem` 均在内（读数见 §2.1.1）；macOS 仍未执行。
- **`-tags=e2e` 的文件不在 `make lint` 判卷面（2026-10-26，判卷面事实）**：`make lint` 的 staticcheck 不带构建标签，`internal/e2e` 里带 `//go:build e2e` 的文件因此从静态检查中整体缺席；本轮用 `GOFLAGS=-tags=e2e staticcheck -checks=inherit,ST1000,…` 单独扫了一次，报出 22 处既有 `U1000`（未使用的测试助手，`fixtures_test.go` / `harness_test.go` / `us14_demos_test.go` 的旧函数），**本轮新增的 `waitDeliveryStatus` 不在其中**。未动这些旧助手（不属本 US 改动面）。是否把 e2e 标签纳入门禁属判据面提案，留 review ② 裁决。
- **候选提交后重跑，取代上文两处 dirty 哈希（2026-10-26）**：Windows 候选 = `bin/basalt.exe` `fb022a01d94e155a3c4682527827e715c9d7829b7dba906b40ba1402d10d909f`（20,377,600 B，`basalt version 11ef243`），升级 / 恢复演练已在该文件上重跑（§2.1「候选提交后重跑」）；Linux = `3de73e446486dc0bc8ce4a9b9d5648c9c1b933dd0be5182e0660b2a6f5e6a847`（20,050,080 B，`basalt version 11ef243`），WSL2 走查已重跑（同节）；`34406c4f…` / `da3bb7f9…` 两个 dirty 戳哈希作废，不得写入发布记录。
- **再取代：发布候选 = `57784fe`（2026-10-26，gate r4 所在提交）**：Windows 候选 `946a6b801f78ec15a75119aa57995d6101280de2c9f82b6d31227ac70b435a99`（`deploy/basalt.exe`，`basalt version 57784fe`，与 gate r4 记录里的 `tested_binary_sha256` 同一文件）、Linux 交叉编译 `0c62c6573eb0d4dfbbba1ade333ebf5c82478298e678551e0263ded2d6a8d58a`（ELF 64-bit LSB、静态链接、stripped）。升级 / 恢复演练与 WSL2 走查**都已在这两个文件上重跑**，读数见 §2.1.1；`fb022a01…` / `3de73e44…` 随 `11ef243` 一并作废，发布记录只引用 `946a6b80…` / `0c62c657…`。

- **证据绑定缺口：已发布的 gate r5 绑定 `4641c7c`，而双平台走查与升级 / 恢复演练仍绑 `57784fe`（2026-10-26，收口 analyze 复跑发现，**未验证**）**：`records/us14-release-gate-r5.json` 的 `tested_binary_sha256` 与 `4641c7c` 对应，但 §2.1.1 全部读数（Windows `946a6b80…`、Linux `0c62c657…`、升级六步、恢复三隔离五判据）来自 `57784fe` 的二进制。`git diff --stat 57784fe 4641c7c` 实测为**产品面改动**：`internal/cli/demo.go` +960 行、`examples/shared/mcp/docs-readonly/main.go` 42 行、`examples/embed.go` +22 行、`internal/catalog/data/mcp_catalog.yaml` 15 行、`harness/stages/us14-release.json` 97 行、`internal/e2e/**` 与 Makefile 等共 37 个文件。按 §2.1.1 自己的口径（「旧二进制证据不能代表发布候选」），`57784fe` 上的读数**不能**为 `4641c7c` 候选背书。**处置（二选一，需用户裁决）**：① 在 `4641c7c`（或用户最终指定的发布候选）上重跑双平台走查与升级 / 恢复演练后翻转本项；② 用户明确裁决以 r5 记录 + `4641c7c` 的 `make check` / stage 绿态作为发布证据，双平台读数保留为 `57784fe` 的历史读数并在发布记录里注明绑定关系。在裁决或重跑之前，FR-022 的「双平台均必验」与发布证据绑定的同一性**保持未完成**。

- **`docs-readonly` 布局改为主 module 内 `package main`（2026-10-26，用户裁决）**：上一条 WSL2 读数来自嵌套 module 形态（源码内容未变，用例集合与断言逐条相同），现由根 module 执行：`go test ./examples/shared/mcp/docs-readonly` 与 `go test ./...` 均覆盖；`go list -deps ./cmd/basalt` 不含该包（不被产品二进制导入），`go list ./examples/shared/mcp/docs-readonly` 有输出。导出与构建由 `demo-installer` / `TestUS14DemoInstall_Artifact` 判卷（源码完整性、生成的 `go.mod`、报告命令可执行、真实连接与一次读取）。

- **构建命令未加引号的边界（2026-10-26 复核 R-06，已知局限，未验证）**：安装报告里的 `go -C <源码目录> build -o <产物> .` 与依赖表模板不对路径加引号；工作区路径含空格时该命令不能直接复制执行，制品测试的 `\S+` 正则同样假设无空格，因此**测不出**这个边界。本机工作区路径 `D:\code\…` 无空格，全部实测通过；含空格路径**未验证**。处置建议（未实施，属实现变更，不在本次裁决范围）：渲染时按平台加引号，并让制品测试覆盖含空格的工作区。

- **管理台登录页文案的 `dist` 尚未重建（2026-10-26，T093，「未验证」）**：F3 的四件固定文案已落在 `assets/console/src/views/login.ts` / `app.ts` 源码，Go 侧 `TestAdminLogin_FixedCopy` 只判源码；本机**无 Node**，`console-test` / `console-build` 未运行，`assets/console/dist` 仍是旧文案。**T119–T121 必须重建 dist 并提交**，在那之前不得声称管理台页面已显示新文案。——**「dist 已重建」这一半已验证（2026-10-26，T120 轮）**：本机已有 Node 24.22.0 便携副本，`npm --prefix assets/console run build` 连跑两次产物同一 SHA-256（`index-3A2vPR85.js`），`integrity.json` 三文件哈希同步更新；对该产物直接计数，四件固定文案均在（`登录管理台` / `访问密码` / `请输入部署者提供的访问密码` / `登录`）。**仍需用户动作**：提交（`git status --porcelain -- assets/console/dist` 现为「删 `index-C8YvltG4.js` / 增 `index-3A2vPR85.js` + `index.html` + `integrity.json`」，提交时机归用户）；T121 的 `admin-login` check 与登录页人工走查仍未执行。

- **发布证据链**（`clean-checkout` 真实验收、发布记录与依赖许可表、双平台走查、`v0.1.0` tag）**等待用户提交候选提交**——**部分验证（2026-10-25）**：全量 stage `accept-20261025T134558.780851700` 的 37 项中 36 项 passed（公共十项 + 本 stage 二十六项，读数见 §2.1「T068 全量 stage」），`clean-checkout` 在 245 ms 内以 `clean-checkout: 工作区不干净——发布证据只能绑定已提交的候选提交` 退出，属脚本设计上的预期红、**不**构成 T064 通过；工作区干净路径的机制已在 `e2e285f` 克隆上验证（§2.1「T064 轮」），但克隆不是候选提交，不作发布证据。T064 / T066 / T067 / T068 / T070 在用户提交后于候选提交上重跑。——**不依赖提交的那一半已完成（2026-10-25）**：依赖与许可表（Go 24 行 + Node / Python / 自研素材 / MCP 六条状态）、升级与恢复演练（Operations §5.1 六步 + §5.2 回滚前置 + §4.3 三隔离五判据）、双平台走查（Windows 演练 + 浏览器自动化、Linux WSL2 全链路）三项读数见 §2.1 三块补充读数；**仍阻塞在用户提交后**：T064 真实 `clean-checkout`、T066 发布记录（版本 + commit + 候选路径 / SHA-256 须与分发产物同一文件，且 `+dirty` 戳与两个二进制哈希都要重算）、T067 条件式文档更新、T068 绿态全量 stage、T070 收口三件套与 review ② 包。**临时资源归属**：`%LOCALAPPDATA%/Temp/t66drill/`（演练工作区、假模型、备份 / 恢复副本、许可扫描产物）与 `%LOCALAPPDATA%/Temp/t66linux/`（Linux 二进制与两份脚本 / 日志）+ WSL `~/t66linux-walk` 全部在仓库外，**保留到候选提交后的重跑与收口，收口前整体删除**；仓库内零临时探针（T056 / T063 探针均建在 Temp 且已删）。**——候选提交后已完成（2026-10-26）**：全量 stage `us14-release-gate-r2` 37/37 passed（`clean-checkout` 1203.5 s passed，commit `11ef243`，dirty=false）；发布记录已发布（`records/us14-release-gate-r2.json` + `records/reports/us14-release-gate-r2.json` + `ledger.md` 一行），发布后 `--task T066` 复跑 passed；T064 / T066 / T067 / T068 四态读数见 §2.1 两条补充读数。**仍为未验证**：T060 全部真实外部实跑（零授权）、`v0.1.0` tag（用户手动）、review ② 人工决定。**临时物已清理（2026-10-26）**：`%LOCALAPPDATA%/Temp/t66drill/`、`t66drill2/`、`t66linux/`、`us14-closeout/`、两份 US-14 临时日志与 WSL `~/t66linux-walk`（20 M）已整体删除，仓库外无残留；证据读数已固化在 §2.1 / §2.2，删除不影响复核。

## 5. 收口结论

**待人工 review ②**（2026-10-26 第三次更新）。AI 侧进展与未完成项：

- 首轮独立复核 = 带条件通过（条件 F1）；F1–F6 已逐条处置并留绿态 / 逆改红态读数（§2.1「收口独立复核 F1–F6 处置」）。
- **第二轮独立复核（修复闭环）= 阻断**（F-01–F-13）。除 F-12（需新候选提交后的全量 gate 记录）与 F-13（转 errata）外均已处置，逐条读数见 §2.1「第二轮独立复核（修复闭环）」。
- **用户裁决（2026-10-26）**：① §2.2 #13 的判据收窄（RFC 2606 文档域豁免）**采纳**（记 §2.2 #19，stage `approval` 同步）；② **授权第二个本地候选提交**，并在该提交上重跑全量 stage（r3），发布 r3 记录取代 r2（r2 保留为历史行）。授权仅限本仓 `git commit`；push / tag / 部署仍归用户。
- **gate r3 结果（2026-10-26）**：36/37 passed，`e2e` exit=2——`TestUS14Demos_Fixture/industry_brief` 的接收端计数在代投落库前读了一次（失败台账 #23），是测试同步缺陷而非实现缺陷；判定窗口真实存在（本地 3 轮命中 1 次首读 `pending`）。修法 = D2 / D5 三处计数前加有界等待（§2.2 #20），**断言值一字未改**；复跑 `--task T038` passed（7 条子测试全 PASS）。r3 报告与日志保留在 `.loop/runs/us14-release-gate-r3/`，**不发布记录**。
- **gate r4 结果与发布（2026-10-26，用户授权第三个本地候选提交）**：候选提交 `57784fe`，全量 stage **37/37 passed**，`dirty=false`，`tested_binary_sha256=946a6b80…`，`source_fingerprint=a32bb07e…`，起止 `2026-10-25T18:28:35Z → 19:24:49Z`。记录已按 `harness/acceptance/records/README.md` 发布：`records/us14-release-gate-r4.json`（`5b085a19…`）+ `records/reports/us14-release-gate-r4.json`（`05f1bbf7…`，与记录 `evidence.report_sha256` 及 `.loop/runs/us14-release-gate-r4/report.json` 逐字节相同）+ `ledger.md` 追加一行（日期取 UTC `2026-10-25`，与 `finished_at` 对齐）；发布后 `--task T066` 复跑 = `fmt` + `release-records` passed，证明 `TestRecords_PublishedConform` / `TestRecords_NoSecretsOrAbsolutePaths` 对本次发布判绿。
- **双平台候选二进制已在 r4 候选上重建（2026-10-26）**：Windows `946a6b80…`（与记录里的 `tested_binary_sha256` 同一文件）、Linux `0c62c657…`；升级 / 恢复演练与 WSL2 走查在这两个文件上重跑完毕，读数见 §2.1.1。
- **gate r5 结果与发布（2026-10-26）**：候选 `4641c7c`（工作区干净），全量 stage **38/38 passed**（含 `console-test` / `demo-installer` / `e2e` / `e2e-required` / `release-records` / `clean-checkout`），起止 `2026-10-25T22:26:33Z → 23:26:32Z`。发布 `records/us14-release-gate-r5.json` + `records/reports/us14-release-gate-r5.json`（报告 sha256 `c92bbd5d…`，与记录 `evidence.report_sha256` 及 `.loop/runs/us14-release-gate-r5/report.json` 逐字节一致）+ `ledger.md` 追加一行（日期取 UTC `2026-10-25`）；发布后 `go test ./scripts/acceptance -run '^TestRecords_(PublishedConform|NoSecretsOrAbsolutePaths)$'` 两用例 passed。T071–T078 已勾选（77/78，余 T060 待外部授权）。注：§2 台账里「发布闸门 r5 / r6」是**尝试序号**，与已发布记录 id `us14-release-gate-r5` 不属同一编号体系。
- **收口前置剩余项**：③ **已完成（2026-10-26）**——七项 DoD（逐项四态 + 本轮读数）与四段变更总结已在本轮对话输出（module-dev 步骤 7 不另开文件）；④ 模块卡 errata（F-13：§2.8.4 的 `TestExamples_NoSecrets`、§4.4 缺两条测试映射 + 本轮新增的 T036 / T037 / T061 / Phase 4 冻结正文过时表述）随 review ② 包提交，由用户决定是否出 R4；⑤ **证据绑定缺口（阻断项，2026-10-26 收口 analyze 复跑发现）**——已发布 gate r5 绑 `4641c7c`，但双平台走查与升级 / 恢复演练绑 `57784fe`，两者间是产品面改动（详见 §4 同名条目）；需用户裁决「重跑」或「接受绑定关系并注明」，在此之前 FR-022 的发布证据同一性保持未完成。
- **临时资源已清空（2026-10-26，收口前）**：`$LOCALAPPDATA/Temp/t66drill3/`（演练工作区 / 假模型与 `requests.jsonl` / 备份与恢复副本 / 双平台二进制 / token / `race_probe.py`）、`$LOCALAPPDATA/Temp/us14-validate-rec.py`、`$LOCALAPPDATA/Temp/us14-commit-msg-3.txt`、WSL `/tmp/us14-linux-drill/`（测试假模型进程 pid 535 先停后删）全部删除，逐项复查不存在；仓库根无遗留构建产物。
- **第三轮独立复核（2026-10-26，docs-readonly 单一事实源裁决）= 带条件通过**（新上下文子代理，只读）：七个判点 A–G 中 A/B/C/D/E 全部 CONFIRMED、F 主体成立、G 断言有区分力；findings R-01–R-07 全为 minor，无 blocker / major。已处置：R-01 模块卡 §3.10 / §3.1.8 的检查映射与用例清单同步到 `demo-installer`；R-02 `Makefile` 注释、R-03 `internal/e2e/us14_demos_test.go` 助手注释、R-04 `.claude/skills/module-dev/SKILL.md` 的命令口径均已改正；R-05「保留态谎称已生成 go.mod」已修（`docsReadOnlyGoModNote` 按状态与文件实况报告）并新增 `TestDemoInstall_ReportsKeptSourceWithoutGoMod`（进 stage `demo-installer` 的 `required_tests`）；R-07 断言已加强（导出物逐字节等于仓库源码、生成的 go.mod 零 `require`、构建产物必须落在 `<ws>/demos/deps`）。R-06（构建命令未加引号，含空格路径不可用）记为已知边界，见 §4。复核未覆盖面：#1 候选二进制模式的 e2e 未由复核者实跑（由 stage `e2e` 覆盖）、#2 未跑 `make check` / `make acceptance`（改用只读等价命令）、#3 仅 Windows、#4 未跑 markdownlint（本轮由控制侧补跑，18 份文档 0 issues）、#5 嵌套形态的 42 个内嵌文件读数为历史记录（当前 44 已实测）、#6 未做全仓 `gofumpt -l .`、#7 其他 stage 的 4 条既有 WARN 未判。
- **仍为未验证**：T060 全部真实外部实跑（零授权）、五个 Demo 真实自动触发 / 八模板夹具复跑（外部依赖）、`v0.1.0` tag（用户手动）、纯主观视觉（用户裁决免验，不阻断收口）、macOS 上 `os.SameFile` / `os.Rename` 的替换行为（Windows 与 Linux 均已验，macOS 未跑）。
- 未收到人工决定前，本段不得写成人工接受；commit / push / tag 仍归用户。

## 6. 14D 增量基线（2026-10-26，用户显式范围增量；只追加）

**用户决定**：本轮范围并入 **US-14 增量交付「开箱即用增强」**，变更包标签 **14D**。14D 是增量标签，**不是**新的正式 US 编号；不执行 `/us-register`（US-14 已登记，14A / 14B / 14C 已存在），不新增 US，US-16 起的拟编号与后续文档不因本决定重编号。

**批准依据**：用户 2026-10-26 消息「四、US 归属：采用 US-14 增量交付，不打乱后续编号」原文——「这是我对 US-14 范围的显式增量调整，请记录批准依据，并同步修正草案 §8。原 US-14 的基线、候选、验收报告和历史结论保留。14D 单独记录新增范围、任务、验收和提交，不把新能力追认为旧验收内容。保护当前 US-14 尚未完成的收口现场；如实现会影响原候选验证，使用独立分支承载增量，集成后另留证据。」

**设计基线工件**：`docs/us/us-draft-openbox.md`（模块卡送审稿，1104 行，sha256 `6b9667d919e2e3773b151d841818dc14db7a9f98572b81648dda987009bd8811`，markdownlint 0 issues）。该文件是私有未跟踪资产，不入 Git，变更以本行哈希核对。

**与既有基线的关系**：

- US-14 原基线（`specs/014-templates-demos-release/` 的 spec / plan / tasks / 既有任务勾选 / 失败台账旧行 / gate r5 记录）**不改写、不追认**；14D 的范围、任务、验收追加记录，任务与验收带 14D 标识。
- 当前 US-14 收口现场（待 review ②、证据绑定缺口待裁决）**不因 14D 改变状态**；14D 设计稿不是收口证据。
- 若 14D 实现触及 US-14 已交付代码路径（目录视图 / env 闸门 / 诊断），在独立分支承载增量，集成后另留证据。

**状态**：设计基线**已完成定点修订与源码核查**，等用户对 14D 增量基线做**一次确认**（review ①）。未确认前不进入派生、分析与实施；本段不写成人工已接受。

**本轮源码核查（读代码，非运行读数）**：`internal/tool/mcp.go:145-153` / `:391-402` / `:577-582`（客户端与 `mcpStatusTransport.base` 接缝）、`go-sdk@v1.8.0 mcp/streamable.go:1968` / `:2048`（`HTTPClient *http.Client`）、`internal/app/resource_admin.go:837`（保存即发布）、`internal/workspace/agent.go:44-60`（`schedules[].zone` 唯一时区来源，cron 内禁 `CRON_TZ=`）、`internal/app/mcp.go:287`（`resolveMCPEnv` 只解析 env）、`internal/core/service.go:34` / `:776-793` 与 `internal/e2e/us14_demos_test.go:964`（缺省 10 与内置 Agent 12/12/12/20/30 冲突）、`go.mod:16`（`golang.org/x/sys` 已是直接依赖）。

**仍未验证**：任何构建、迁移、二进制、完整包安装运行、传输层测试均**未执行**；模块卡 §6.2 M1–M8 全部标未验证。

**2026-10-26 review ① 确认记录**（本段为追加，不覆盖上一段的状态行）：

- **确认**：按 US-14 增量交付「14D 开箱即用增强」推进；不新增 US、不执行 `/us-register`、不调整 US-16 起编号；保留原 spec / 任务勾选 / 历史证据，追加 14D 内容与**独立 stage**。
- **Q4**：接受窄化修订——仅为真正新实例且目标文件不存在时生成最小可用 `basalt.yaml`；已有文件不得覆盖；**存量无配置实例不得被当成新实例**放宽策略；创建须有并发不覆盖保证。
- **§6.5**：接受六项判据调整（稿中现为**七项**，第七项「访问密码来源扩展」为本次确认后新增），只改与调整直接相关的断言，不得减少既有覆盖。
- **P0**：授权修订宪法——允许系统在可证明的新实例中自动生成独立随机主密钥；保留存量密钥缺失 / 损坏 / 身份不匹配时拒绝替换；网页录入凭据仍是用户操作，Agent 不得管理凭据。修订原文与差异并入同一份 review ① 记录。
- **实施前补齐的四处定点设计**（已落稿）：① 硬链接发布残留的识别与恢复（`SameFile` 证明同一 inode 才清理，否则进恢复态，不放宽 `nlink > 1` 检查）；② Windows 权限先于写入（建目录 → 设受阻 DACL → 读回校验 → 才建文件）；③ 首次配置对本次启动生效（生成早于 `cli.LoadConfig`，见 `internal/app/run.go:81`）；④ 访问密码不无条件打印（非 TTY 只提示位置；文件移到工作区外的实例私有目录）。
- **平台**：Windows 原生 + WSL2 Ubuntu；WSL2 须跑 Linux 构建产物并在 Linux 文件系统内验证权限 / 锁 / 进程行为。
- **原 US-14 证据绑定**：保留 `4641c7c` / `57784fe` 记录，核对差异影响，在最终候选上重跑受影响走查；不口头豁免。
- **O1**：先用可复现 v5 夹具覆盖，真实实例升级前再跑只读检查；不得改当前实例凑证据。
- **未验证项**：设计阶段未跑构建 / 迁移 / 完整包属正常，继续标未验证，不再单列成待裁决事项。

**新基线工件**：`docs/us/us-draft-openbox.md`，**1157 行**，sha256 `39d9b048a1cd992152a1b6553d76febebdee06de91fecf642b284858df11cc60`，markdownlint 0 issues、单 H1。中间态 `0ae39e74…`（确认记录写入时的版本）与上一段的 `6b9667d9…`（确认前版本）均保留作历史，**以本行哈希为准**。

### 6.1 P0 宪法修订记录（2026-10-26，`/speckit-constitution`；同一份 review ① 的依据）

**执行方式**：用户 2026-10-26 集中确认中「授权完成 P0 的必要修订」；按 Governance 走 `/speckit-constitution`，未由 AI 自批。模板解析 `constitution-template` 成功；只改两条款 + Sync Impact Report + 版本头，其余条款零改动。

**版本**：`3.0.0` → **`4.0.0`**，**MAJOR**——第 14 条由「全部由人执行」改为「唯一允许的自动化例外」，属规范语义反转（与 3.0.0 对第 11 条的处理同口径）。`Last Amended` 2026-10-26。

**修订后原文（逐字）**：

```text
6. **启动依赖判定**：……纯 env 或仅通知托管实例**不得**因缺少主密钥被阻断；**不得**静默回落 env、在**已有密文**的实例上自动生成新密钥或跳过；**新建实例的首次初始化**生成主密钥不属此禁（该实例没有既有密文可失效，范围见第 14 条）。

14. **凭据操作由人执行；主密钥初始化例外**：凭据的录入、替换、撤销**必须**由人执行。**主密钥初始化可以由用户启动的安装 / 实例初始化流程自动完成**，但只限**可证明的新实例**——目标密钥文件不存在，且该实例既无托管密文、也无主密钥历史记录（含未完成初始化的残留）。存量密钥缺失、损坏或 `key_id` 身份不匹配时**必须**拒绝替换并进入恢复处置，**不得**据此生成新密钥；初始化中断后只恢复能证明来源的残留，无法证明来源时进入恢复状态而**不得**删除既有文件。运行中的 Agent 任务**不得**管理或获取主密钥，只能使用凭据；Agent 运行时**不得**创建、读取明文或修改凭据。
```

**差异**：`git diff` = 36 insertions / 6 deletions；blob `b8155ab` → `a7adda0`；文件 351 → 375 行，sha256 `6e3b77430fc3a7c1dee12f0f0885f75f3add443fc688ed0dc774d85226431b32`；markdownlint 0 errors、无占位 token。改动面**仅**：第 6 条正文 + 其症状行、第 14 条标题 / 正文 / 症状 / 复核、顶部新 Sync Impact Report 块、`**Version**` 行。

**不在本次范围（实施时同步，已登记为 14D 影响清单项）**：`docs/Requirements.md:419` 与 `docs/TechnicalDesign.md:751` 仍原样复述旧第 6 / 14 条；`docs/Operations.md:550-555` 的 `secrets init` 口径与访问密码文件位置待改；契约与 14D 卡 / spec / plan / tasks 随实施更新。**代码无需改动**：`internal/app/credential_crypto.go:15`、`credential_resolver.go:12`、`ports.go:502` 的「运行时不自动生成」仍成立——本次放宽的只是用户启动的初始化流程。

### 6.2 14D 派生记录（2026-10-26，review ① 通过后自动推进）

**用户授权**：2026-10-26「既然设计已经敲定。后续所有决策，按你推荐的来。我在 review 2 人工验证时再介入。」——在已确认范围内由 AI 决策并推进；不改变 commit / push / tag 仍归用户，也不代替 review ②。

| 工件 | 变更 | 状态 |
| --- | --- | --- |
| `spec.md` | 追加 14D 段：US4 / US5、FR-027–FR-044、SC-014–SC-020、边界；FR-025 的「零 schema 迁移」由 FR-031 取代 | 已写，既有 FR / SC 未改 |
| `plan.md` | 追加 14D 段：技术基线、宪法逐条核对、P0–P7 顺序、代码落位、stage 判据、风险、测试策略 | 已写，既有内容未改 |
| `tasks.md` | 追加 Phase 9–13：T079–T124（46 项），既有 T001–T078 与勾选未动 | 已写 |
| `harness/stages/us14-14d.json` | 新建独立 stage `us14-14d-r1`：15 个 check、46 个任务映射 | `--lint-stages` 通过 |

**stage 设计说明**：14D 的锚点测试尚不存在，`required_tests` 若现在写入会让 stage lint 失败且无法运行任何 check，因此本轮**暂不列**，锚点名称冻结在 stage 的 `approval` 与 `tasks.md` 中，由落地该测试的同一任务以**追加式修订**补入——不删断言，只把判定推迟到能判定它的任务。

**演示实例**：用户 2026-10-26 指定演示实例的访问密码并固定；该值不写入仓库、测试、公开文档或日志。8080 实例已按新值重启并验证（`/api/v1/info`：新值 200、旧值 401、无认证 401）。

### 6.3 14D 实施前 `/speckit-analyze`（2026-10-26，阻塞检查）

**范围**：`spec.md` 14D 段（FR-027–FR-044、SC-014–SC-020）× `plan.md` 14D 段 × `tasks.md` Phase 9–13 × 宪法 v4.0.0 × 设计正本。**只读分析后按本仓自动推进模式在已审范围内修复并复跑。**

| ID | 类别 | 级别 | 结论与处置 |
| --- | --- | --- | --- |
| C-01 | 宪法对齐 | 高 | 访问密码以明文文件承载，是否违反第 1 条「凭据两种模式」——**已在 FR-030 显式界定**：它是实例引导凭据，不属资源凭据两种模式的管辖对象，仍受 F5–F9 限权；无 CRITICAL |
| C-02 | 宪法对齐 | — | 第 14 条新例外只覆盖**用户启动的初始化流程**；FR-027 的存量实例不自动初始化与之逐条一致 |
| A-01 | 一致性 | 高 | `docs/TechnicalDesign.md:751`、`docs/Requirements.md:419` 仍是旧第 6 / 14 条镜像——**已由 FR-043 / T123 覆盖**，实施时同步；当前不算 CRITICAL（文本滞后，尚无实现与之矛盾） |
| A-02 | 覆盖 | 中 | FR-044（零新增坐标）无独立任务——**由 T079 与 T124 的 `go-build` / `stage-lint` 承担**，`go.mod` / `go.sum` 在 protect 列表内 |
| A-03 | 任务可判定性 | 中 | T118（WSL2 完整包）无法由自动 check 判定——**保持人工项**：`full-package` 只跑 Windows 冒烟；T118 勾选必须等 Linux 实机证据，单平台不得代替 |
| A-04 | 重复 | 低 | FR-035 与 FR-036 都涉及域名——**保留**：前者定档位授权范围，后者定禁止域名与例外的生效语义，不合并 |
| A-05 | 歧义 | 低 | FR-041「本轮声明支持的平台」——由 §三裁决固定为 Windows 原生 + WSL2 Ubuntu，plan 已写明 |
| A-06 | 歧义 | 低 | 五个内置 Agent 名称只在 T111 出现——**冻结在 T111**，spec 不重复列名以免两处漂移 |

**覆盖统计**：14D 需求 25 项（FR 18 + SC 7），有任务覆盖 25 项（100%）；任务 46 项（T079–T124），全部绑定 check；CRITICAL = **0**，允许进入实施。

**复跑结论**：上述修复落地后重跑同一分析，无新增 CRITICAL / 高。

### 6.4 14D 实施记录（2026-10-26 起）

#### A. 用户纠正：`required_tests` 不再推迟（**推翻** §6.2「stage 设计说明」整段）

**用户原文要点**（2026-10-26）：暂不填写的处理需要纠正；用户实际执行 `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T079`，输出 `schema-v6: stage Go test check requires named required_tests`、`exit status 2`；现有加载逻辑（`scripts/acceptance/config.go:195`）要求 stage 内 Go 测试检查必须填写 `required_tests`。因此 `--lint-stages` 通过**不代表** stage 可执行。正确处理是**现在就填入已冻结的完整测试名**；实施前测试不存在、存在性检查呈红态属正常；**不得**为让 lint 提前通过而省略机器可检查的约束。

**处置**：15 个 check 全部写入 `required_tests`（T079–T129 的冻结名，逐字来自模块卡 §6.1 的 A1–A12 / B1–B8 / C1–C8 / D1–D12 / E1–E11 / F1–F9 与 plan 的测试策略）；§6.2 中「锚点名称冻结在 approval 与 tasks.md、由落地任务追加式补入」的做法**作废**，保留为失误记录。stage 的 `approval` 已写明这是**故意的红态基线**。

**实测印证**：T082 当前 `contracts-14d (exit=0) required test did not execute and pass: TestContracts14D_EnumsMatchGoLiterals`——`-run` 选择空集时 `go test` 退出码为 0，控制侧按「具名测试未执行且通过」判红，说明具名锚点确实在判卷，而不是靠退出码。

**追加式修订 1**（同日，同一授权）：T081 增 `policy-scope` 检查（`TestNetworkPolicyStore_ScopesIndependent`）——`schema-v6` 只选 `^TestMigrate`，两 scope 读写分离无人判；`admin-login` 的 `-run` 补 `^TestInstanceSecrets_AdminTokenSeparateFile`（该测试住在 instance-secrets 文件里，却判定登录密码来源）。

**追加式修订 2**（同日，T082 期间）：**T100 / T103 / T110 原本没有自己的锚点**——T100（诊断 `dependency` / `references` 轴）只有 B1–B8 的凭据名，T103（`execution_policy` 存储 + `PUT` If-Match）无任何存储锚点，T110（`GET /sandbox` 扩展 + 禁止域名 / 例外端点）无 API 锚点且 `TestSandbox_ViewShapeMatchesContract` 不在任何 `required_tests` 里——三个任务会靠别人的测试转绿。只**增加**名称与包：`mcp-credential` 增 `TestMCPDiagnostics_DependencyAxis` / `TestMCPDiagnostics_ReferencesAxis` / `TestMCPContracts_DiagnosticsResponse` 与 `^TestMCPDiagnostics|^TestMCPContracts_DiagnosticsResponse`；`execution-profile` 增 `TestExecutionProfile_StorageIsRuntimeSource` / `_UpdateCAS` / `_DeniedDomainsAPI` / `_NetworkExceptionsAPI` / `TestSandbox_ViewShapeMatchesContract`，包加 `./internal/web`；T089 另映射 `builtin-agent`（其交付物由 E7/E9/E10 判）。无删除、无弱化。

**Phase 14 追加（FR-045）**：`tasks.md` 追加 T125–T129、stage 新增 `web-search` 检查——`industry-brief` 的搜索由 Brave MCP 改为内置 `web_search` 调 DeepSeek Search API，复用该 Agent 的 DeepSeek 模型凭据；不属 MCP 验证证据，不再需要 `BRAVE_API_KEY`，正文仍走 Fetch；仅 408 / 429 / 5xx 可重试（最多 2 次）。T111 的 `industry-brief` 已同步为 `web_search` + Fetch。**官方 schema 逐字段核对归 T129**，核对前不写死契约字面量。

#### B. 逐任务记录

| 任务 | 日期 | 报告 | 结果 | 覆盖 |
| --- | --- | --- | --- | --- |
| T079 | 2026-10-26 | `accept-20261026T025139.245018300` | passed | `fmt` + `schema-v6`（3 个具名测试） |
| T080 | 2026-10-26 | `accept-20261026T025152.945620900` | passed | `fmt` + `schema-v6` |
| T081 | 2026-10-26 | `accept-20261026T025143.507231600` | passed | `fmt` + `schema-v6` + `policy-scope` |
| T082 | 2026-10-26 | `accept-20261026T025200.034358200` | **failed（预期）** | 契约已落地；`contracts-14d` 红在 `TestContracts14D_EnumsMatchGoLiterals` 未执行——该测试由 **T084** 交付 |
| T082 | 2026-10-26 | `accept-20261026T031118.690590200` | passed | 复跑：`fmt` + `contracts-14d`（两个具名测试均执行并通过；上一行保留为红态基线证据） |
| T083 | 2026-10-26 | `accept-20261026T031135.653001100` | passed | `fmt` + `contracts-14d` |
| T084 | 2026-10-26 | `accept-20261026T031110.284214200` | passed | `fmt` + `contracts-14d`（`TestContracts14D_EnumsMatchGoLiterals` / `TestContracts14D_SchemaMatchesMigration`） |
| T085 | 2026-10-26 | `accept-20261026T032257.700672800` | passed | `fmt` + `secrets-init`（加密三态 `uninitialized` / `ready` / `recovery_required` 与六个恢复原因） |
| T086 | 2026-10-26 | `accept-20261026T032309.642131400` | passed | `fmt` + `secrets-init`（原子发布：受限目录 → `O_EXCL` 链接 → 读回；残留回收） |
| T087 | 2026-10-26 | `accept-20261026T032316.775978000` | passed | `fmt` + `secrets-init`（`instance_secrets` 记录、`key_id` 校验与 adopt） |
| T088 | 2026-10-26 | `accept-20261026T032324.057777200` | passed | `fmt` + `secrets-init`（平台权限先于写入：Unix `0700` / Windows 受限 DACL） |
| T092 | 2026-10-26 | `accept-20261026T032331.166547700` | passed | `fmt` + `secrets-init`（A1–A12 十二个具名测试） |
| T090 | 2026-10-26 | `accept-20261026T033900.988892700` / `…T033911.368897400` | failed（两次） | `admin-login` 已绿；`fmt` 红在 `internal/cli/config.go` 与 `internal/app/run.go` 的 gofumpt 差异（本轮改动未格式化，与判据无关） |
| T090 | 2026-10-26 | `accept-20261026T033932.301205900` | passed | `fmt` + `admin-login`（复跑；上两行保留为红态记录） |
| T091 | 2026-10-26 | `accept-20261026T033951.936032100` | passed | `fmt` + `secrets-init`（`auto_init: false` 纯 env、CLI 状态三态投影） |
| T093 | 2026-10-26 | `accept-20261026T033943.207714200` | passed | `fmt` + `admin-login`（七个具名测试全执行并通过） |
| T089 | 2026-10-26 | `accept-20261026T034001.790199000` | **failed（预期，部分绿）** | `fmt` + `secrets-init` 通过；`builtin-agent` 红在 `TestUS14D_AgentsDisabledAfterInstall` 未执行——E1–E10 的 e2e 由 **T111–T115** 交付，T089 的两个检查全绿后才勾选 |
| T101 | 2026-10-26 | `accept-20261026T042744.640985800` | failed | `mcp-credential` 红在 `fmt`——新增测试文件未过 gofumpt（判据无关，属本轮改动未格式化）；保留为红态记录 |
| T101 | 2026-10-26 | `accept-20261026T042902.786913000` | passed | `fmt` + `mcp-credential`（B1–B8 八个具名测试全执行并通过） |
| T094 | 2026-10-26 | `accept-20261026T042917.118891300` | passed | `fmt` + `mcp-credential` |
| T095 | 2026-10-26 | `accept-20261026T042924.698086400` | passed | `fmt` + `mcp-credential`（来源显式 B7 三分支） |
| T096 | 2026-10-26 | `accept-20261026T042932.215958400` | passed | `fmt` + `mcp-credential`（headers 闸门 B5、args 拒绝 B6） |
| T097 | 2026-10-26 | `accept-20261026T042939.620090700` | passed | `fmt` + `mcp-credential`（Bearer 装配 B8：线上请求头、跨主机重定向零携带） |
| T100 | 2026-10-26 | `accept-20261026T042946.849370500` | passed | `fmt` + `mcp-credential`（`TestMCPDiagnostics_DependencyAxis` / `_ReferencesAxis` / `TestMCPContracts_DiagnosticsResponse` 转绿，§6.4.D 两行之一清账） |
| T098 | 2026-10-26 | `accept-20261026T051039.511851900` | failed | `mcp-apply` 已绿；`fmt` 红在 `internal/app/mcp_supervisor.go` / `resource_state.go` / `internal/web/views.go` 的 gofumpt 差异（本轮改动未格式化，与判据无关）；保留为红态记录 |
| T098 | 2026-10-26 | `accept-20261026T051054.411413100` | passed | `fmt` + `mcp-apply`（八条冻结 C1–C8 全执行并通过；复跑） |
| T099 | 2026-10-26 | `accept-20261026T051106.388828200` | passed | `fmt` + `mcp-apply` |
| T102 | 2026-10-26 | `accept-20261026T051112.727751800` | passed | `fmt` + `mcp-apply`（含新增的恢复查证用例 `TestMCPApply_PendingRowVerifiedNotReplayed`） |
| T103 | 2026-10-26 | `accept-20261026T053157.024757000` | **failed（预期，部分绿）** | `fmt` 通过；`execution-profile` 的 18 个冻结名中当时只有 `TestExecutionProfile_StorageIsRuntimeSource` / `_UpdateCAS` 与六个本轮新增的 web 用例执行通过，其余十六个归 T104–T110；check 全绿后才勾选 |
| T104 | 2026-10-26 | `accept-20261026T054349.596162800` | **failed（预期，部分绿）** | `fmt` 通过；`execution-profile` 中 `TestNetGuard_DialMatchesResolution`（8 子用例）与 `TestNetGuard_NoEnvProxy`（2 子用例）连续两次执行并通过，T104 判据面全绿；check 整体仍红在未落地的 `TestExecutionProfile_*` 十个名（T105–T109）与 `TestSandbox_ViewShapeMatchesContract`（T110）；check 全绿后才勾选 |
| T105 | 2026-10-26 | `accept-20261026T054622.781991900` | **failed（预期，部分绿）** | `fmt` 通过；`TestExecutionProfile_ShellNameSet`（`policy_standard` / `policy_strict` / `runner_refuses_before_spawn` 三个子用例）通过，`symlink_rename_is_refused` **跳过并附理由**（本机 Windows 未授予创建软链接的权限，`os.Symlink` 报 `A required privilege is not held by the client`；该分支在 CI 的 Ubuntu 作业上执行）；其余冻结名仍归 T106–T110 |
| T106 | 2026-10-26 | `accept-20261026T055013.260242400` | **failed（预期，部分绿）** | `fmt` 通过；`TestExecutionProfile_AuthorizationParity`（4 子用例）、`_ProgramDirRequiresThree`（5 子用例）、`_CmdBatNotExempt`（3 子用例）全部通过；其余冻结名仍归 T107–T110 |
| T107 | 2026-10-26 | `accept-20261026T060538.059204100` | **failed（预期，部分绿）** | `fmt` 通过；T107 的四个冻结名全部执行并通过：`TestExecutionProfile_DeniedDomain`（standard / strict × 2 子用例）、`_NetworkException`（6 子用例）、`_DeniedDomainsAPI`（4 子用例）、`_NetworkExceptionsAPI`（6 子用例）；`execution-profile` 唯一红项 `TestSandbox_ViewShapeMatchesContract`（缺 `execution_policy` / `execution_profile`）归 **T110**；check 全绿后才勾选 |
| T108 | 2026-10-26 | `accept-20261026T060829.228950700` | **failed（预期，部分绿）** | `fmt` 通过；T108 的冻结名 `TestExecutionProfile_MigrationStrict` 执行并通过（六段 ①–⑥：迁移行抗配置覆盖 / 两组待确认项 / 未确认双面拒绝 / 保持拒绝写审计 / 加例外只放行一条 / shell 轴确认且修订号不变）；`execution-profile` 唯一红项仍是 `TestSandbox_ViewShapeMatchesContract`，归 **T110**；check 全绿后才勾选 |
| T109 | 2026-10-26 | `accept-20261026T061306.476114300` | **failed（预期，部分绿）** | `fmt` 通过；T109 的四个冻结名全部执行并通过：`TestExecutionProfile_StandardBaseline`（D1 七条：公网放行 / IP 字面量三种 / 端口档三种 / 私有段 / 元数据含例外 / 公网域名解析私有 / 重定向不跟随）、`_BothProfilesSameBaseline`（D8 四子用例：私有解析 / 元数据重定向 / 环境变量代理 / 授权分叉）、`_FileWriteDenied`（D4 两档 × 九类写读面）、`_WhitelistResolvedProtected`（D11 判定面：默认拒 / 加例外放行 / 例外只覆盖点名域名 / 删除即失效 / 标准档同结论）；`execution-profile` 唯一红项 `TestSandbox_ViewShapeMatchesContract` 归 **T110**；`TestExecutionProfile_ShellNameSet/symlink_rename_is_refused` 仍按 T105 记录跳过并附理由；check 全绿后才勾选 |
| T110 | 2026-10-26 | `accept-20261026T061954.307018200` | **passed** | `fmt` + `execution-profile` 双绿：19 个冻结名全部执行并通过（含 T110 转绿的 `TestSandbox_ViewShapeMatchesContract` 与本轮新增的 `TestSandbox_ExecutionProfileProjection`），`execution-profile` 自 T103 起第一次整条通过。报告里唯一的跳过是顶层用例 `TestExecutionProfile_ShellNameSymlink`（本机无软链接创建权限，见下方裁决），不是任何必跑用例的子测试；勾选依据即本次报告 |

**同一份报告触发的勾选波次**：`execution-profile` 是 T103–T110 共用的检查，T110 让整条转绿后，T103–T109 各自"check 全绿后才勾选"的条件同时满足，故一并勾选——这是同一份证据（`accept-20261026T061954.307018200`）的机械结果，不是补记。

**T082–T084 交付内容**：`openapi.yaml` 落 9 个新端点（含 T083 期间补的 `POST /execution-policy/confirmations` 与 `PolicyConfirmationWrite` / `PolicyConfirmationResult`——GET 的待确认项原本只写了"可处置"却没有出口，属契约自洽补齐）与 4 处扩展；`run-and-errors.md` 落四个新错误码、`policy_denial_reason` 四值表、八个新动作、三个新 target_kind、七行恢复口径与四条 §9.5 前提。Go 侧新增 `internal/core/execution_policy.go` / `instance_secrets.go` / `mcp_credential.go` 三个纯字面量文件（本轮补 `SecretRecoveryReason` 六值、`PendingConfirmationAxis` / `PendingConfirmationReason` 各两值、`NetworkAllowlistSource` 的 `migration` / `instance_init`），每个闭集配表驱动测试。契约测试两处：`internal/web/contracts14d_test.go::TestContracts14D_EnumsMatchGoLiterals`（十一个闭集 + 两套 MCP 失败分类的双向判卷 + 解析 run-and-errors §9.3 的 action / target_kind 字面量表与 openapi 交叉核对）与 `internal/storage/schema_v6_contract_test.go::TestContracts14D_SchemaMatchesMigration`（全量 DDL 与 v5→v6 迁移在 v6 五张表上的结构指纹等价：列 / 外键 / 索引形状，忽略自动索引名——`ALTER TABLE … RENAME` 会保留旧表名；另钉住唯一键形状、`execution_policy` 的"迁移种 strict / 全量 DDL 不种"与 `binding_key` CHECK 行为）。`tasks.md` T079–T084 已按报告勾选。

**T079–T081 交付内容**：迁移 `internal/storage/migrations/0006_execution_policy_and_mcp_credentials.sql` 与 `docs/contracts/schema.sql` 同步；`SUPPORTED_SCHEMA_VERSION` 5 → 6；`credentials` 重建（`owner_kind` 加 `mcp_server`、新增 `binding_key`、`CHECK` 约束绑定键与属主一一对应、唯一约束改 `(owner_kind, owner_uid, binding_key)`）；新表 `execution_policy`（scope `global`，迁移种 `strict`，`operation_id='system.migrate_v6'`）、`network_exceptions`（`exception_id` 前缀 **`exc_`** + 26 Crockford Base32，`note` 非空，`UNIQUE(kind, value)`）、`instance_secrets`（scope `master_key`）、`mcp_credential_sources`（PK `(server_uid, binding_key)`）；`NetworkDomainPolicyStore` 改为按 `policy_scope` 读写，`http.allowed_domains` 行行为不变。

**T085–T093 交付内容**：实例加密三态（`core.SecretState` / `SecretRecoveryReason`）与 `instance_secrets` 记录、`InstanceSecretsService.Ensure`（原子发布 + 残留回收 + adopt）、平台权限先于写入；`internal/app/instance_identity.go`（`<工作区>/.basalt/instance.json` 名册、平台数据根、缺省私有目录 / 主密钥 / 访问密码路径、`resolveMasterKeyFile` 的"显式 → 推导 → 纯 env"次序）、`internal/app/instance_bootstrap.go`（T089：库与名册都不存在才算新实例；配置先写、名册后写；`O_CREAT|O_EXCL` 并发唯一胜出）、`internal/app/instance_admin_token.go`（T090：第三来源、三处全缺的固定错误、新实例才生成、非 TTY 只出路径、交互仅一次）；`cli.SecretsConfig.AutoInit` 与 `validateSecretsSection` 放宽为"缺省主密钥路径可自动推导"，启动期 token 校验从 `cli.LoadConfig` 移到装配层（`web.ValidateAdminAuth` 仍拒绝空值，拒绝理由改由 `missingAdminTokenError` 点名三处）。`wire.go` 启动步骤增加 `secrets`（位于 `migrate` 之后、`recover` 之前，十步顺序的判卷已更新）；`SecretsStatus` 增 `State` / `Reason` 并投影到 CLI 文本。测试：`internal/app/instance_admin_token_test.go`（F2 / F5–F9）、`internal/web/login_acceptance_test.go`（F1 / F3 / F4）、`TestInstanceSecrets_AdminTokenSeparateFile`（F9：分文件、同目录、权限同级、主密钥恢复态不阻塞登录）。登录页四件文案已在 `assets/console/src/views/login.ts` 落地（标题「登录管理台」/ 字段「访问密码」/ 提示「请输入部署者提供的访问密码」/ 按钮「登录」），`app.ts` 路由标题同步。

**未验证（本轮环境）**：本机无 Node，`console-test` / `console-build` 未运行，`assets/console/dist` **未**按新源码重建——T119–T121 执行时必须重建并提交 dist；在那之前管理台产物仍是旧文案，F3 的 Go 侧判卷只覆盖源码。

**T094–T101 交付内容**：`internal/app/mcp_credential_test.go`（约 905 行，八条冻结测试逐条对应设计 §6.1 的 B1–B8）：B1 无明文（定义文件 / 版本表 / 操作 `detail_json` / 日志 / 视图投影 / `hashMCPCredentialDecls` 只出 `slot=mode`）、B2 按键独立版本（alpha 升 v2 后 beta 仍 v1，两者 `credential_id` 不同）、B3 撤销不回落（进程环境埋了同名陷阱变量仍报 `ErrCredentialRevoked`、`ciphertext` 为 NULL、epoch 0→1）、B4 普通编辑保托管值且不追加版本、B5 `http.headers` 与 env 同一闸门（缺变量 → 503 + `operation_id`，文件 / 指针 / 版本零写入；编辑路径文件字节不变）、B6 `args` 含 `${` 拒绝（创建点名 `args[3]`、编辑点名 `args[0]`，无操作行；存量条目仍可加载并带 `args_placeholders_not_resolved` 诊断）、B7 来源显式（managed 缺失不读进程环境、`env` 槽只读进程环境）、B8 `Authorization` 装配为 `Bearer <value>` 且不推断不查重（值里已含 `Bearer` 则双前缀）、其他头逐字透传、跨主机重定向丢弃凭据。判卷用真实路径：真实保存链路 + 真实 SQLite + 真实 `CredentialOps.Revoke` + `tool.NewMCPPool` 对进程内 `mcp.NewStreamableHTTPHandler` 服务端（回环 handler 记录收到的请求头）。

**T098–T102 交付内容**：`internal/app/mcp_apply_test.go`（约 1150 行，八条冻结测试对应设计 §6.1 的 C1–C8，加一条非冻结的恢复查证用例）：C1 只换目标（指针 / 注册池 / 来源表 / 连接器生命周期；`DrainTimeoutSeconds=30`、`DrainDeadline` 按基线时钟）、C2 失败保 `applied`（应用层与 supervisor 层各一条；旧连接仅在 `OldLegal` 为真时保留）、C3 排空窗口（在途调用被覆盖；到点未完成 → `drain_timeout`，之后新调用按 `mcpLinkClosedReason` 拒绝）、C4 旧 Run 快照不被改写（钉住实例仍指向旧代际，换名后的新实例是另一个对象）、C5 预检拒绝（依赖 `dependency_unconfirmed` / 托管凭据已撤销 `credential_unavailable`，均在建连之前拒绝）、C6 并发 `superseded`（发布 / 另一次 apply / 撤销三个方向，候选连接与注册撤回、操作行 `MCP_APPLY_SUPERSEDED`）、C7 CAS 回滚（提交段注入写失败 → 与建连失败同一条回滚路径）、C8 撤销即时（拒绝发生在执行之前、连接立即回收、`applied` 与指针不变、撤销路径不伪造 apply 操作行）。夹具用真仓储 + 假连接器，接缝只有设计允许的 `NewConnector` / `ReplaceTools` / `MarkApplied`；C6 的并发钩子复用指针行已有的 `operation_id`（`resource_pointers.operation_id` 外键指向 `management_operations`，自造 id 会撞 787）。

**T098/T099 的三处实现裁决（2026-10-26，按设计就近解释，不改判据）**：① **预写 detail 由 `{}` 改为冻结投影**——设计 §2.3.3 第 2 步要求"冻结定义版本 + 每个占位符的凭据版本，写进操作行 detail"，而崩溃后恢复只能依据这份 detail 判三态；原实现只在终态回填写 detail，pending 行是 `{}`，恢复会全落 `unknown`。改为 `BeginOperation` 时即写 7 键投影（`outcome` 为空 = 尚无结论，`applied_version_id` 记操作前的值），终态回填照旧覆盖；终态行仍是固定 7 键。② **C8 的"运行记录标明使用了已撤销版本"落在真实执行器审计行**：拒绝以 `*core.ExternalError`（固定短语「MCP 凭据已撤销，连接已关闭：该调用未发出」）返回，执行器记 `failed` + `external_error` + 原文，Run 记录据此可事后追溯；不新增 schema 列，也不把"已撤销"再抄一份到运行记录（凭据版本状态是唯一事实源）。③ **撤销后的状态投影没有 `stale` / `pending_restart` 字面量**：设计 §2.3.3 的"把 `applied` 标为 stale"在实现里由 `Connected=false` + `Error=mcpLinkRevokedReason` + `applied_version_id` 保留三者共同表达——`applied` 指针保持不变，页面据 `Error` 与凭据状态显示"已撤销"；另建一个与 `pending_restart` 并列的字面量会与"版本事实不变"互相矛盾。

**本轮发现并修复的三件事**（均属实现 / 夹具缺陷，不改判据）：① **共享夹具缺陷**——`resource_admin_test.go` 的主密钥原本写在 `<工作区根>/secrets`，而 `LoadMasterKey` 按纪律拒绝工作区内的密钥；此前的用例从没封存过托管值，所以一直没暴露，T101 第一个 managed 用例即 `credential is unavailable: 主密钥文件不得放在工作区内`。修法：密钥改写到与工作区同级的第二份 `t.TempDir()`（生产部署也是这条纪律）。② **判卷空转**——B1 的日志扫描起初在 MCP 保存成功路径上读日志，而该路径只在失败时写日志，缓冲区为空等于断言恒真；改为先经同一 logger 打一条探针并断言探针可见，再扫描无明文。③ **T100 实现补齐**——`diagnoseMCPEnv` 原先把占位符字面量（`${VAR}`）当成变量名去查环境，改为按 `core.MCPSlotPlaceholder` 取变量名；`McpServerDiagnosis` 补 `dependency` / `references` 两键后 `TestMCPContracts_DiagnosticsResponse` 转绿，§6.4.D 的对应行已清账（`TestSandbox_ViewShapeMatchesContract` 仍红，归 T110）。

**T103 交付内容**：`internal/app/execution_policy.go`（`ExecutionPolicyService`：`EnsureSeeded` 以「表内无行」识别新实例、种子来源 `config` 非法时回落 `standard` 并记 `system.instance_init`；`Update` 在同一把锁内依次判 `If-Match` → `expected_profile` → 修订号 CAS，预写失败即拒绝执行；`PendingConfirmations` 两轴只读扫描（出网条目按 `core.ClassifyAddress` 判受保护段、命令条目按 `core.IsShellName` 判名称上限），已加例外或已记录「保持拒绝」的条目不再列出；`Confirm` 只追加处置行、不改档位与修订号）、`internal/web/handlers_execution_policy.go`（GET / PUT / confirmations 三端点、六条固定中文文案、失败按类型映射为 400 / 409 / 409 / 412 / 500）、`internal/web/views.go` 三个视图、`internal/app/baseline.go` 的 `verifyExecutionPolicy`（`profile` 与 `revision` 对上前 / 后两侧，第三值记 `unknown`，**不**重放）与 `RecoveryDeps.ExecutionPolicy`、`wire.go` 的装配（`EnsureSeeded` 排在 `mcpSupervisor.ApplyPublished` 之前，首个 MCP 决策即用权威档位）。web 层**不**自行比较 ETag，也不把一次档位切换顺带写成白名单或例外变更。

**T103 的四处裁决**（就近解释，不改判据）：

| 事项 | 裁决 |
| --- | --- |
| 四类拒绝原因怎么落到具体分支 | `denied_domain` = 命中 `http.denied_domains`（两档同判，先查）；`profile_denied` = 严格档白名单未命中、或标准档因档位降级到程序目录；`protected_address` = 云元数据地址与解析失败；`exception_missing` = 本可经例外放行的受保护地址但没有生效例外。shell 名称上限 / 文件范围 / 命令形状 / 端口拒绝**不**套第五类，`Reason` 留空——四项都是能力上限声明，不是"策略拒绝"的一种 |
| URL 目标是 IP 字面量 | 两档都拒；`ip_port` 例外只在**拨号时**按已解析地址匹配，不给 URL 目标开旁路 |
| `current_profile` 进 `ErrorDataView` | `run-and-errors.md` §8 要求 `EXECUTION_PROFILE_MISMATCH`「给出当前档位」，而 `ErrorDataView` 是失败载荷的白名单结构——不加字段就只能吞掉契约要求的数据。只加这一个键，仍不透出定义正文 / 凭据 / 内部路径 |
| 路由表计数 37 → 41（非条件行 26 → 29、条件行 11 → 12） | `wantAdminRouteKeys` 原本已漏登记 `POST /api/v1/mcp-servers/{name}/apply`（T098 落地时未同步）；本轮按已批准接口补齐四项并更新计数，逐行断言循环未动，属接口适配而非判据变更 |

**判据面记录**：T101 只**新增**具名测试（八个测试名在 stage 中已冻结），无删除、无弱化；`MCPServerRequest.Credentials` 字段对既有测试调用点的改动是按已批准接口做的机械适配，不改变任何断言语义——按 14D 裁决口径记为接口适配。T103 同口径：只新增 web 层六个用例（`TestExecutionProfile_ReadView` / `_ReadEmptyPendingList` / `_ReadFailsClosed` / `_WriteRequiresIfMatch` / `_WriteBodyValidation` / `_WritePreconditionFailures` / `_WriteSuccess` / `_WriteErrorMapping` / `_Confirmations`），未删除、未放宽任何既有断言。T104 同口径：两个冻结名 `TestNetGuard_DialMatchesResolution` / `TestNetGuard_NoEnvProxy` 只新增；另在既有表驱动 `TestClassifyInitializeFailure_DeadlineBeforeURLShell` 上**追加**一条 `guard_refusal_inside_url_error_is_policy_denied`，未改任何既有用例的断言。

**T104 交付内容（受控拨号）**：`internal/tool/netguard.go` 的原实现已冻结在本轮开工前（解析 → 分类 → 只拨校验过的地址；双栈任一受保护段即整域拒绝；`Host` / SNI 保持原域名；云元数据地址即便有例外也拒；解析失败即拒；`Transport()` 每消费者独立克隆且不回读环境代理），本轮补齐**装配与出口**：

1. `internal/cli/config.go` 新增 `http.proxy` 键与 `validateHTTPProxy`：空 = 直连；只接受 `http(s)://[userinfo@]host:port`；形态错误**拒绝启动**（不静默直连——那等于换了一个出口）；错误文案不回显取值（userinfo 可能含凭证）。
2. `internal/app/netguard.go`：`installOutboundGuard` 在 `app.Run` 内、**任何 HTTP 客户端构造之前**装配进程级守门器；`networkExceptionSnapshot` 为进程级活快照（`sync.RWMutex` + 每次拨号现读），由 `wire.go` 在迁移完成后经 `refreshNetworkExceptions` 从 `network_exceptions` 表装载——读失败 WARN 并**保留上一份快照**（受保护地址仍一律拒绝，fail-closed）。装配点选 `app.Run` 而非 `newRuntime`：包内测试直连回环 httptest MCP，若在 `newRuntime` 装真解析器会引入顺序依赖。
3. `internal/tool/notify_adapters.go`：webhook / feishu 切到新的 `newNotifyHTTPClient()`（不经过守门器、显式 `Proxy=nil`、不跟随 3xx）。理由：通知目标是**管理员绑定**的渠道（企业内网网关 / 回环中转），不是模型自选 URL；域名授权仍走 `Policy.Check`，沙箱的受保护段规则不适用于该出口。
4. `internal/tool/mcp.go::classifyInitializeFailure` 增补：`core.IsSandboxViolation(err)` → `(initialize, policy_denied)`，且**排在 `*url.Error` 之前**——`http.Client.Do` 把拨号错误必然包进 `*url.Error`，后判会把策略拒绝误报成 `http_unreachable`（D-13 / FR10 的具名约束）。
5. `basalt.yaml.example` 只补**已生效**的 `http.proxy`；`denied_domains` / `execution.profile` 两键尚未被消费（种子在 T107 / 档位读写已交付但示例同步归文档任务），留 T123 一并补，避免示例声明不存在的键。

**T104 两处待复核裁决**（按用户「设计已敲定，后续按你推荐的来」的授权口径自行裁定，收口时供 review ② 复核）：① 契约三份视图（`ExecutionPolicyView` / `SandboxView` / `NetworkAllowlistView`）**不新增** proxy 字段——代理来源是实例配置而非可写资源，没有端点能改它，只读字段会制造"可在页面配置"的错觉；执行环境页保留静态说明文案（T119 落地）。② 通知出口**不接管门器**（见上文第 3 条）——若后续把通知目标扩到模型可自选地址，必须先改回受控拨号，否则受保护段规则在通知出口上有缺口。

**T107 交付内容（禁止域名表 + 内网访问例外）**：

1. **禁止表复用白名单行**：`network_domain_policy` 新增 `http.denied_domains` 行的读写，`NetworkAllowlist` 服务加 `Scope` 依赖与 `scope()` / `action()` / `targetKind()` / `label()` 四个投影——同一套 ETag / CAS / 预写 / 热替换，只换作用域、审计动作（`network_denylist.update`）、目标类型（`network_denylist`）与生效字段（`SetDeniedDomains`）。判定面：`core.DomainEntryMatches` 保证 `*.example.com` **不**覆盖裸域名；两档都**先**查禁止表再判白名单。
2. **例外服务**：`internal/app/network_exception.go`（`EnsureLoaded` / `List` / `ETag` / `Create` / `Delete`）——理由必填（1..500 rune，按 rune 计）且进审计 detail；云元数据地址任何档位都拒绝登记（`core.ClassifyAddress` 的 metadata 位）；重复 `(kind,value)` → 409 且**不覆盖**既有理由；删除以**集合 ETag** 为唯一条件（`If-Match`），行不存在 → 404 且零操作行；预写 `pending` 失败即拒绝执行。一次 `apply` 同时写三处消费面：`policy.SetNetworkExceptions` / 进程级 `processExceptions.Replace` / 服务自身快照，删除后**下一次**判定即失效。
3. **Web 面**：`PUT /api/v1/sandbox/denied-domains`（If-Match 必需，成功回全量 `SandboxView`）、`GET /api/v1/network-exceptions`（`{etag, exceptions}`，空集也有稳定 ETag）、`POST`（三字段指针体，note 必填）、`DELETE /{id}`（If-Match = 集合 ETag）。白名单与禁止表共用 `writeNetworkDomainPolicyError`，状态码映射不再有两份；`GET` 时端口缺失返回 500 而非空列表——空列表会让页面与实际拨号判定分叉。`SandboxView` 增 `denied_domains` / `network_denylist` 两键（基准投影 + 活快照覆盖）。
4. **恢复查证（契约 §9.4）**：`RecoveryDeps.NetworkExceptions`；`verifyNetworkAllowlist` 按 `TargetKind` 选作用域行（两行同表，选错行等于对比错摘要）；新增 `verifyNetworkExceptionCreate` / `_Delete` 与 `exceptionValueVerdict`——同 `(kind,value)` 被**别的**操作占用 / 重新出现时一律 `unknown`，不猜 `applied` / `not_applied`。

| 事项 | 裁决 |
| --- | --- |
| 白名单运维文案 | `NetworkAllowlist` 共用实现后，日志 / 错误文案按 `label()` 分流：白名单**逐字保持**原 `allowlist` 文案（既有两个断言不动），禁止表说 `denylist` |
| `network_exceptions` 的审计 detail | `kind` / `value` / `note` / `before_etag` 四键；理由本身属审计内容（FR 要求"写审计"），凭据类禁令不适用 |
| 例外服务端口缺失时的 GET | 500 而非空集合（fail-closed，见上） |

**本轮发现并修复**：共用实现初期把白名单四条日志与两条错误文案统一改成 `policy` 措辞，`go test ./internal/app` 报 `TestNetworkAllowlist_SeedsOnceFromConfig` / `_SeedOverLimitRefusesStart` 两个既有断言红（期望原文仍是 `allowlist`）。属**实现改名的回归**，不是判据变更：加 `label()` 后白名单路径恢复原文，两测试复绿；未改任何断言。

**T108 交付内容（存量升级的升级后行为）**：`internal/app/execution_policy_migration_test.go`（新文件，单条冻结名 `TestExecutionProfile_MigrationStrict`）。迁移脚本本身由 storage 层 `TestMigrate_V5ToV6` 判（`schema-v6` check：v5 → v6 一律 `strict`，白名单为空亦然），本文件判的是**那行落到运行期之后**的后果——① 按 0006 的写法种出 `strict` / `system.migrate_v6` 行后，`EnsureSeeded("standard")` 不得覆盖（配置写 `standard` 是最坏情形；`Source()` 仍 `migration`、修订号仍 1、运行期策略实例同为 `strict`），空白名单在最宽松解释下也拒公网域名；② `SetAllowedDomains` 装载 v5 存量条目后 `PendingConfirmations` 列出四项：两条解析到 RFC1918 的白名单条目（`ResolvedProtected` 各自给出地址）与两条命中 shell 名称的命令条目，公网条目不列；③ 未确认则双面拒绝——拨号面（注入解析器 + 记录型拨号器，`exception_missing`、零拨出）与授权面（`bash` / `cmd.exe` 两档皆拒）；④ `Confirm`（保持拒绝）写 `execution_policy.confirm` 审计行、`detail` 带 `"axis":"network_allowlist"`，条目出列而拨号仍拒；⑤ `NetworkExceptionService.Create` 对该条目写 `network_exception.create` 行（理由进审计），条目出列、该主机真的拨到 `10.20.30.41:443`，未登记的那条继续拒；⑥ shell 轴确认后条目出列、`cmd.exe` 仍在列且仍拒，末段断言档位与修订号都未变（"确认与例外都不改档位与修订号"）。

**T108 判据口径**：未删任何既有断言、未放宽任何阈值；解析器与拨号器均为注入替身（不打真实 DNS、不开套接字），因此判据在无网环境与 CI 上同结论。

**T109 交付内容（D1 / D4 / D8 / D11 的判定面判据）**：三个新文件，均落在 `internal/tool` 的**包内**测试（`package tool`）——`newHTTPClient` 与进程级 `SetNetGuard` 不导出，而 D1 的"重定向到私有地址拒"与 D8 的"环境变量代理"必须判**真实组合**（客户端 + 守门器），在 `tool_test` 里只能重写一份客户端，等于判测试自己的副本。

1. `execution_profile_baseline_test.go`（`TestExecutionProfile_StandardBaseline` = D1；`_BothProfilesSameBaseline` = D8）。D1 七条逐条落地：公网域名放行（标准档不查白名单）→ IP 字面量三种（公网 / 私有 / 元数据，域名面同一条规则拒）→ 端口档三种（`http:8080` 与两向交叉档）→ 私有段（域名面放行、拨号面 `exception_missing`）→ 云元数据（**带 `ip_port` 例外也拒**）→ 公网域名解析到私有 → 公网域名真的拨到已校验 IP → 回环服务 302 指向内网域名时 3xx 原样返回、第二跳零解析零拨号。D8 四子用例把"同一结论"落在具体读数上：私有解析（两档域名面都放行 → 两档拨号面都拒、零拨出）、元数据重定向（两档都只拨第一跳，记录两次相同地址）、环境变量代理（`HTTP_PROXY` 等六个变量都指向死端口，两档仍直连回环并成功）、授权分叉（标准档放行未列名公网域名 / 严格档 `域名不在白名单` / 严格档命中白名单后放行）。
2. `execution_profile_file_test.go`（`TestExecutionProfile_FileWriteDenied` = D4）：工作区骨架 + 工作区外的两点（主密钥文件、程序目录），两档各跑一遍——自有目录普通写入放行（禁区不是"什么都拒"）、两个 Agent 的 `AGENT.md`、控制目录（读写**同一句**规则）、`scripts/`、技能库、记忆目录、程序目录、主密钥逐条拒绝，最后判读面不随档位扩大（工作区根 `README.md` 两档同拒）且自有目录读仍可用。
3. `execution_profile_resolved_protected_test.go`（`TestExecutionProfile_WhitelistResolvedProtected` = D11 判定面）：活例外快照闭包——默认 `exception_missing` 且零拨出 → 登记 host 例外后真的拨到 `10.20.30.40:443` → 同段另一个域名仍拒（host 例外按域名匹配，不做地址段级放行）→ 删除例外后下一次判定即失效 → 标准档同结论。

**T110 交付内容（`GET /sandbox` 的执行档位投影）**：`internal/web/views.go` 的 `SandboxView` 增 `execution_profile`（枚举 `standard|strict`）与 `execution_policy`（与 `network_allowlist` 同形的五键元数据 `etag` / `revision` / `source` / `updated_at` / `updated_by`）。`(*Server).sandboxView()` 在装配了 `ExecutionPolicy` 端口、且活行修订号 > 0 时投影**活行**：档位取行上的值，元数据走新抽出的 `executionPolicyMetadata`，与 `GET /execution-policy` 共用同一份 ETag 材料——页面两处看到的强 ETag 不可能分叉。`handlers_execution_policy.go` 的 `executionPolicyView` 改为复用同一投影函数，删掉重复的 ETag / 时间渲染。新用例 `TestSandbox_ExecutionProfileProjection` 判两个方向：种子行（`standard` / revision 3 / `op-seed`）→ 投影 `standard` 与同一 ETag、`source=database`、`updated_at` / `updated_by` 逐字；未装载（revision 0）→ `strict` / `config_seed` / 空 ETag / 空修订号。

**T110 的三处就近裁决**：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| 没有权威行时的档位 | 投影 `strict`，不投影空串，也不投影 `standard` | 策略实例在没有档位行时实际按 `strict` 判定（首版默认即严格档）；投影必须与判定是同一事实。空串会违反契约枚举，`standard` 会宣称一个比实际更宽的档位 |
| 没有权威行时的来源 | `config_seed` + 修订号 0 + 空 ETag | 与既有 `network_allowlist` 只读缝同一口径；编造 `migration` / `instance_init` 等于在页面上宣称一次没发生过的种子动作，而第 5 条纪律要求证据可回溯 |
| `admin_routes_test.go` 的期望集合 | 端点 41 → 45、条件写 12 → 14、非条件行 29 → 31 | T105 / T106 已按契约落了四个新端点（`PUT /sandbox/denied-domains`、`GET /network-exceptions`、`POST /network-exceptions`、`DELETE /network-exceptions/{id}`），判卷表当时只改了注释没改清单。这是"随已批准接口适配"：四条都在 `docs/contracts/openapi.yaml`，断言是**扩大**（新端点逐条必须在表、条件写集合必须含 `DELETE /network-exceptions/{id}`），没有删除任何既有断言 |

**T110 期间的一处判据结构调整（不改断言）**：`TestExecutionProfile_ShellNameSet/symlink_rename_is_refused` 移为顶层用例 `TestExecutionProfile_ShellNameSymlink`。原因：创建软链接在 Windows 上需要 `SeCreateSymbolicLinkPrivilege`，而验收执行器（`scripts/acceptance/execute.go` 的 `checkTests`）把"必跑用例的子测试被跳过"判为失败——于是一个平台能力问题把 D1–D12 整族判据一起拖红（T105–T109 五次运行都带着这一项）。拆成顶层后：子句与断言逐字不变；有权限的环境（Linux CI、或开了开发者模式的 Windows）照常执行；本机跳过记在报告的 `skipped_tests`（`execution-profile` 的唯一一项），**不**算通过。`TestExecutionProfile_ShellNameSet` 保留 `policy_standard` / `policy_strict` / `runner_refuses_before_spawn` 三个子用例作必跑锚点。stage 已追加 "ADDITIVE REVISION 3" 记录（含新投影用例此前不在任何 `-run` 选择集里，现已同时进 `-run` 与 `required_tests`）。

**T109 判据口径与两处就近裁决**：① **不判 `Policy.Check` 的两条合成 URL**（回环 `httptest` 的随机端口 + `127.0.0.1` 字面量）——策略面的端口档与"出网目标不是域名"会先拒，那与"3xx 不跟随 / 环境变量代理不生效"是两件事；替代断言是活档位确实切到本档（`policy.ExecutionProfile()`），再判客户端结论。这两处**没有**放宽任何既有断言——它们本来就是新用例。② 主密钥与程序目录在 D4 里都落到 `写入目标超出当前 Agent 的可写范围`：主密钥按纪律必须在工作区之外，程序目录是安装清单的只读运行时，两者都不在任何 Agent 的可写范围内，因此由同一条规则挡下；测试把这条结论钉住，防止将来给工作区开旁路时顺带放进去。

#### C. T082 派生字面量裁决（供 T083 / T084 / T094–T110 对齐）

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| `EXECUTION_PROFILE_MISMATCH` | `PUT /execution-policy` 的 `expected_profile` CAS 不符 → 409（与 revision 409 分开：前者是档位语义冲突，后者是并发写冲突） | 用户口径「改档位要有前置断言」，避免两个管理员互相覆盖档位 |
| `SECRETS_RECOVERY_REQUIRED` | 实例 secrets 处 `recovery_required` 时拒绝任何以密文为前提的操作（409）；与运行期 `CREDENTIAL_UNAVAILABLE` 分属实例级与资源级 | 模块卡 §2.4 状态机 |
| `MCP_APPLY_FAILED` / `MCP_APPLY_SUPERSEDED` | 500 / 409；前者带 `failure_class`（复用十个值 + `drain_timeout`），后者带 `superseded_by` | 模块卡 C2 / C6 |
| 例外标识前缀 | `exc_` + 26 Crockford Base32（**不是**设计稿早稿的 `nx_`；以迁移 `0006` 为准），路径参数带 `pattern` | 迁移已冻结 |
| 例外集合 ETag | 以包装对象 `NetworkExceptionListView{etag, exceptions}` 返回，不用响应头 | 与既有 GET 的「对象载荷」口径一致 |
| 凭据种类枚举 | 撤销用 `CredentialOwnerKind`（含 `mcp_server`），`/scope` 仍用 `CredentialResourceKind`（不含）；`binding_key` 仅 `kind=mcp_server` 必填、其余必须为空 | 主密钥作用域不按占位符切片 |
| `NetworkAllowlistView.source` | 扩为 `config_seed` / `database` / `migration` / `instance_init`；白名单与禁止域名只出前两者 | 否则 `execution_policy` 的来源会被误标 `database` |
| `McpDependencyDiagnosis` | 可空（`oneOf [..., null]`） | 自定义命令无注册运行时组件，与 `command` 的可空性同口径 |
| `OperationView.target_kind` | 扩到 11 值（新增 `network_denylist` / `execution_policy` / `network_exception`） | 写入行各由同名动作产生，读侧必须能按同一字面量过滤 |
| `OperationDetail.detail` | 固定键集：`mcp_server.apply`（= apply 结果去掉 `server`）/ `execution_policy.update`（前后档位与前后修订）/ 两处域名写（前后修订 + 候选 hash）；除 `last4` 外禁止凭据 | 审计详情不得成为泄漏面 |
| apply 的 503 | 内联描述（`NOT_CONFIGURED` / `CREDENTIAL_UNAVAILABLE` / `SECRETS_RECOVERY_REQUIRED`，必须带 `data.operation_id`），**不复用** `NotConfigured` | 后者共享描述写死「一次 LLM 调用都不会发出」，与 apply 语义不符 |

#### D. 契约落地后的瞬态红（按用户裁决保留，已绑定归属任务）

| 测试 | 现象 | 归属 |
| --- | --- | --- |
| `TestMCPContracts_DiagnosticsResponse`（`stdio` 带命令对象 / `http` 的命令对象为 `null`） | `McpServerDiagnosis` 缺 `dependency` / `references` 两键 | **T100**（现为 `mcp-credential` 的具名锚点） |
| `TestSandbox_ViewShapeMatchesContract` | ~~缺 `denied_domains` / `network_denylist`~~（T107 已补）；~~余缺 `execution_policy` / `execution_profile`~~（**T110 已补，用例转绿**） | **T110**（现为 `execution-profile` 的具名锚点） |

**为什么不在 T082 先补字段转绿**：契约要求 `execution_profile` 是 `standard` / `strict` 枚举、`execution_policy` / `network_denylist` 是实体对象。当前既无 `execution_policy` 读取路径、也无禁止域名数据源，先填 `""` / 空对象会在执行环境页**伪造**一个不存在的策略；`dependency` / `references` 同理，`references.count=0` 等于在诊断面断言「没有 Agent 引用它」而实际未统计。按用户「红态属正常」的口径保留红，收口前由 T100 / T110 转绿。

**已转绿的两条**：`TestMCPContracts_ToolDisposition` 与 `TestResources_ViewShapesMatchContract/MCP 详情`——`projectMCPServer`（`internal/app/resources.go:856`）初始化 `HeaderKeys` / `CredentialKeys` 为空集合（载荷出 `[]` 不出 `null`）；填充值分别是 T096（`http.headers`）与 T094（`mcp_credential_sources` 投影）的工作，故此时空集合是**如实**的。

**常量对账两条已转绿**（2026-10-26，T093 期间发现并清理）：`TestContract_ErrorCodes` 与 `TestContractAdmin_NewErrorCodes` 曾因 §8 已并入 14D 四行（`SECRETS_RECOVERY_REQUIRED` / `EXECUTION_PROFILE_MISMATCH` / `MCP_APPLY_FAILED` / `MCP_APPLY_SUPERSEDED`）而 `envelope.go` 尚未声明对应常量而红。按契约固定字面量补四个常量声明，期望集合 30 → 34（**扩大**而非删除；产码仍由 T094 / T103 / T106 等实现任务落地）。验证：`go test ./internal/web -run '^TestContract_ErrorCodes$|^TestContractAdmin_NewErrorCodes$'` → `ok`。`TestMCPContracts_DiagnosticsResponse` / `TestSandbox_ViewShapeMatchesContract` 两条按上表继续保留红态，归 T100 / T110。

#### E. T114 启用门禁与预算兼容（2026-10-26）

**交付内容**：新增只读端点 `GET /api/v1/agents/{name}/readiness` 与共享评估器（`internal/web/readiness.go`），启用端点（`POST /agents/{name}/enable`）的前置校验改为调用**同一个** `agentReadiness`——页面说"可启用"、按钮却 409 的分叉在结构上不可能出现。

- `AgentReadinessView{agent, ready, pending, notices}` + `AgentReadinessItem{kind, item, detail, fix}`（`internal/web/views.go`），契约 `docs/contracts/openapi.yaml` 新增路径与两个 schema。
- pending（**阻断**）：`model_connection`（未发布 / `availability=unavailable`）、`credential`（MCP 槽位 `managed_revoked` / `missing` / 未知值）、`resource`（MCP 条目或技能无已发布版本、未注册条目的装载拒因）。
- notices（**只提示**）：`notify_channel`、`budget`（`settings.max_iterations` 超实例生效上限，点名两边数字与配置键 `budget.max_iterations`）、`concurrency`（启用且未退役计划涉及的**不同 Agent 数**超 `server.max_concurrent_runs`）。
- 409 文案：`Agent「x」当前不可启用：缺少必需配置：<item>（<原因>，前往 <控制台路径>）、…`；装载期拒绝路径的文案保持不变（名字 / frontmatter 类问题**不是**"缺少配置"）。
- 实例上限：`core.DefaultMaxIterations`（10）/ `core.DefaultMaxConcurrentRuns`（32）改为导出；`web.Options.InstanceLimits` 由 `App.webOptions` 从 `cfg.Budget.MaxIterations` / `cfg.Server.MaxConcurrentRuns` 投影，零值由 `web.NewServer` 补同一对常量。装配层不抄第二份缺省。
- 路由表 45 → 46 行，非条件行 31 → 32；`wantAdminRouteKeys` 与契约路径同步（**扩大**，未删断言）。

**T114 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T114-R1 新增只读就绪端点 | 设计 §2.6.4 的三段视图与 E8 的"点名"都需要机器可读的判定；`/profiles` 的 `problems` 只在 active 时计算（`profileRunnable` 先判状态），无法回答"停用的 Agent 差什么" | 设计 §2.6.4 要求三段共存于 Agent 页；§3.2 的九端点表未列该端点，属派生遗漏 |
| T114-R2 `env` 来源槽位按就绪处理 | 保存 / 发布期的 env 闸门已在发布那一刻证明变量存在；事后移除在运行期以分类失败暴露（§2.5.1 末段） | 不在启用闸门重复猜进程环境快照；`sampleMcpServerView` 的 `CredentialKeys` 为空也据此不阻断 |
| T114-R3 空 `Provider.Name` 在评估层不判 | 装载规则 2 / 3 已把未注册 provider 与空模型整只拒载（`Registered=false`），启用路径走不到该分支；与 `missingPublishedResources` 同口径 | 若在此再判一次，会与 `/profiles` 的 `runnable` 分叉，且只影响人工构造的条目 |
| 并发提示的计数口径 | 数"启用且未退役计划涉及的**不同** Agent 数"，与 `server.max_concurrent_runs` 对照，文案写"可能"（429 TOO_MANY_RUNS） | 不解析 cron 求交集：精确推断要枚举时间点，代价远超收益；E8 只要求"提示可能" |
| 预算提示不改配置 | 只在 `notices` 点名两边数字与配置键，`fix` 为空串（配置键没有对应页面，**不**伪造跳转） | E8 明确"只点名不改配置" |
| `notify_channel` 进 notices 而非 pending | E6："通知渠道缺失**不**阻止启用" | 与 §2.5.1 的必需项列表区分 |
| 装载拒因的跳转落点 | 缺模型连接 → 模型连接页，缺技能 → 技能页，MCP 不可用 → MCP 页，其余（名字 / frontmatter / 绑定 / 计划 / 空模型）→ Agent 配置页 | 跳转必须点得通且指得对；控制台没有的路由不写 |
| 就绪视图读失败 | `GET` → 500 `INTERNAL` + `resourceViewsProblem`；启用 → 409 同一文案 | 与既有启用闸门的失败关闭口径一致，**不**降级成"就绪" |

**验证读数**（2026-10-26）：

- `go test ./internal/web/ -count=1` → `ok`（新增 `readiness_test.go`：阻断四类 / 凭据四态 / 三条提示 / 零值归一 / 未注册条目 / 跳转路由 / 端点 200·404·500 / 契约字段与 kind 闭集 / 409 文案拼装）。
- `go test ./internal/app/ ./internal/core/ -count=1` → `ok`（新增 `TestWebOptions` 的实例上限投影子例：12 / 4 逐键投影、零值原样投影）。
- `staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/web/ ./internal/app/ ./internal/core/` → exit 0。顺带清掉 `internal/app/mcp_credentials.go` 的 `mcpCredentialDeclare` 未引用（`Prepare` 里显式写缺省动作，不再靠 iota 零值含义）。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T114` → `passed fmt (exit=0)`；`failed builtin-agent (exit=0) required test did not execute and pass: TestUS14D_AgentsDisabledAfterInstall`。红态符合用户 2026-10-26 的裁决（冻结名先于实现），转绿归 T116 的勾选波次。

**未验证**：E6 / E8 的进程级证据（`TestUS14D_EnableBlockedWithoutConfig` / `TestUS14D_BudgetCompatNotices`）尚未存在（T116）；控制台三段视图未接线（T121），"跳转点得通"目前只由对齐 `assets/console/src/router.ts` 的既有路由常量保证，没有浏览器级证据。

#### F. T115 启动隔离（2026-10-26）

**交付内容**：`publishedMCPServers`（`internal/app/mcp.go`）在装配期遇到**某一条 server 自己的凭据取不到值**时跳过该 server（WARN 点名 server 与变量名、不带值），不建连接、不注册工具，其余照常装配；新增 `mcpCredentialStateFailure` 标记与 `mcpCredentialUnavailable` 分类器（`internal/app/mcp_credentials.go`）。E3 的另一半「一个 MCP server 连不上」无需改动——`MCPSupervisor.ApplyPublished`（`internal/app/mcp_supervisor.go:170-187`）已把单条连接失败吸收为 `connected=false` 并继续其余 server。

- 被标记为「该 server 的凭据当前不可用」（**可跳过**）：env 变量未设置 / 为空（既有 `mcpEnvSlotFailure`）；托管凭据没有值、已撤销、当前版本非 active（新增标记）。
- **不**标记、仍失败关闭：主密钥不可装载 / 损坏 / key_id 不匹配、凭据存储读取失败、密文解密失败、目标绑定不符、槽位未声明来源、发布版本不可读 / 归属不符 / 定义非法 / 条目名不一致。
- 主密钥错误补上 server / 槽位上下文（`MCP server x 的槽位 TOKEN: <原因>`），但仍不套状态标记。
- 缺少凭据的 server 在页面上走既有可见性路径：资源视图的 `CredentialKeys` 显示 `missing` / `managed_revoked`，引用它的 Agent 按装载规则 6 未注册、就绪视图给出装载拒因（T114 的 `agentReadiness` 已覆盖该分支）。**不新增端点、不新增视图字段。**

**T115 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T115-R1 跳过而不是带占位符启动 | 跳过 = 不建进程、不注册工具；带 `${VAR}` 字面量起子进程正是 FR5 禁止的失败形态，且子进程可能产生副作用 | 设计 §2.5.1 末段"MCP 连接失败被吸收为 connected=false，不阻断 serve 启动" |
| T115-R2 跳过依据用分类而非 `errors.Is(ErrCredentialUnavailable)` | 主密钥失败也包装同一个哨兵；一把抓会把"整个实例的托管凭据都解不开"伪装成一次缺值，启动看似成功、所有托管条目同时失效 | 设计 §2.4 要求存量密钥缺失 / 损坏显式进入恢复态，不得静默降级 |
| T115-R3 模型连接目录仍失败关闭 | E3 的机制段只写 MCP；provider 目录是运行期路由表（宪法 III），跳过会让"已发布"与"可路由"分叉；`VerifyStartup` 只校验 **active 且已注册** Agent 绑定的托管模型连接，14D 新实例五个 Agent 安装即 `disabled`，不触发该路径 | 宪法 III / 设计 §2.5.1 机制段的范围 |
| T115-R4 定义与版本完整性失败不跳过 | 版本不可读、归属 / 条目名不符、定义非法、槽位未声明来源都说明存储与定义分叉；跳过会让 Agent 静默引用一张错表 | "坏数据伪装成没配好"与启动隔离要解决的不是同一类问题 |
| T115-R5 不新增端点与页面字段 | 既有资源视图的槽位状态 + 装载规则 6 + T114 就绪视图已经能回答"该 server / 该 Agent 未就绪" | 设计 §2.6 未要求新的"跳过"视图 |
| T115-R6 连接失败路径不改 | `ApplyPublished` 已逐条吸收失败并记 ERROR，`connected=false` 由 supervisor 状态承载 | 源码实测（`mcp_supervisor.go:170-187`），无需第二次改造 |

**验证读数**（2026-10-26）：

- 新增 `internal/app/mcp_startup_isolation_test.go`（5 例）：env 事后清空只跳过该条且日志不含值；托管撤销只跳过该条；主密钥不可用仍拒绝启动并点名 server（同一份库换回正常主密钥仍装配成功，证明失败只来自主密钥）；定义与资源分叉仍拒绝启动；分类器只对两类 per-server 错误返回 true。
- `go test ./internal/app/ ./internal/core/ ./internal/web/ ./internal/storage/ -count=1` → 四包全 `ok`（app 40.7s）。
- `staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./internal/app/ ./internal/web/ ./internal/core/ ./internal/storage/` → exit 0。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T115` → `passed fmt (exit=0)`；`failed builtin-agent (exit=0) required test did not execute and pass: TestUS14D_AgentsDisabledAfterInstall`。红态同 T114，转绿归 T116。

**未验证**：进程级 `TestUS14D_StartupIsolation`（真实 `serve` 进程 + 缺凭据的已发布 server）尚未存在（T116）；控制台对"被跳过"的 server 仍只看既有槽位徽标，没有专门的页面证据（T121 接线）。

#### G. T116 内置 Agent 的 E1–E10 进程级判据（2026-10-26）

**交付内容**：`internal/e2e/us14_builtin_test.go`（`//go:build e2e`，package `e2e`）。九个用例已落地并全绿；第十个 `TestUS14D_FixtureEndToEnd`（E4「夹具端到端」）**尚未写**——它判的链路经过内置 `web_search` 与 Fetch 两个工具面，而 `web_search` 归 T125–T129，先写只能把断言写成对未落地行为的猜测。全部用例跑真实二进制（`TestMain` 构建一次，`binary_source=temp`），不 import `internal` 包，判据只用管理面契约与库内读数。

| 用例 | 判什么 | 本轮耗时 |
| --- | --- | --- |
| `AgentsDisabledAfterInstall`（E1） | 首次启动装五只 Agent 且全部 `registered=false` / 问题恰为「未在 AGENT.md 中声明默认模型」/ 生命周期 `disabled` 代数 1；免凭据两条 MCP 条目本次启动即发布（`published_version_id > 0`），github 条目**不在** | 1.72s |
| `MCPAutoPublishedOnCredential`（E2） | 无凭据的首次启动不写 github 条目；凭据就绪的下一次启动补写并当次发布，占位符为 `${GITHUB_PERSONAL_ACCESS_TOKEN}`、明文值不落盘；五只 Agent 的定义与生命周期状态**逐字节不变**；零 `mcp_server.*` 管理操作行 | 1.49s |
| `StartupIsolation`（E3） | `broken` 条目只拒载引用它的 `bad`（拒因逐字 `MCP 服务不可用（未配置或未连接）：broken`），`good` 照常注册，进程不退出 | 1.62s |
| `DisabledNotScheduled`（E5） | 停用态跨两个触发点后 `runs`=0 / `task_executions`=0 / `last_run_at` 为 NULL（任务行仍在）；同进程启用后下一次到点跑出恰一次 `cron` + `scheduler` + `completed` 的 Run | 16.25s（含既有判据要求的 11s 静默窗口） |
| `EnableBlockedWithoutConfig`（E6） | 缺模型连接 / 缺 MCP 各返回 409 `AGENT_UNAVAILABLE` 且逐字点名与 provider / server 名；就绪视图同一句话，Kind 与 Fix 分别为 `model_connection` + `/console/resources/model-connections`、`resource` + `/console/resources/mcp-servers`；就绪的 `plain` `ready=true`、待补为空、通知渠道缺失只进 notices 且启用成功 | 0.73s |
| `FreshInstanceBudgetPreset`（E7） | 新实例生成配置含 `addr: 127.0.0.1:8080` / `mode: token` / `max_concurrent_runs: 32` / `max_iterations: 30` 且**无** `token:` 行，名册匹配 `^inst_[0-9a-f]{16}$`，日志含 `新实例：最小配置与实例名册已就绪` + `config_created=true`；已有配置（`max_iterations: 7`）启动前后逐字节不变，名册仍生成 | 1.79s（两子用例 0.89 / 0.91） |
| `BudgetCompatNotices`（E8） | 声明 40 超上限 10、2 只计划超并发 1 各出提示并点明两边数字与后果（`429 TOO_MANY_RUNS` / `budget.max_iterations`），`ready` 仍为真；`basalt.yaml` 与两只 AGENT.md 一字节不改 | 0.63s |
| `ConfigEffectiveThisStart`（E9） | 同一进程内用自动生成的访问密码（实例私有目录第三来源）发布模型连接、运行期写入声明 40 的 Agent，就绪视图立刻点名「声明 40 超过实例上限 30」且 `ready=true`，配置仍为 30 | 1.30s |
| `ExistingNoConfigUntouched`（E10） | `init` 建库后的工作区按存量实例处理：`serve` 非零退出，输出含 `管理认证配置校验失败` / `http.admin_auth.mode 缺失或非法` / `存量实例`，且不写 `basalt.yaml`、不写名册、不建实例私有目录 | 0.11s |

**T116 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T116-R1 「已发布」按读到 `published_version_id > 0` 判，不按文件存在判 | T112 / T113 的交付物是「写入**并发布**」；只看文件无法区分「写进去了但发布失败」，那正是免凭据条目要排除的形态 | 任务卡 T112 原文 + 设计 §2.5.4 |
| T116-R2 E2 快照取在首次启动**健康检查之后** | 生命周期状态文件由启动基线在首次启动时创建，之前根本不存在；提前取只能读到 ENOENT | 源码实测（`internal/app/baseline.go` `applyAgentWork`） |
| T116-R3 E5 / E6 / E8 必须先 `initWorkspace` 再写夹具 Agent | `writeLifecycleState` 会为状态文件的 `op-...` 补 `management_operations` 行，库不存在时**跳过**插入，随后索引写入撞外键失败（WARN），Agent 丢身份——判据会红在夹具而不是被测行为 | 源码实测（`internal/e2e/harness_test.go` `seedLifecycleOperation`） |
| T116-R4 E6 的 409 断言用**不带**`缺少必需配置：`前缀的形态 | 该前缀只出现在「已注册但未发布」的 `readinessPendingMessage` 路径；未注册条目的 409 直接回 `entryRejectMessage`。两种形态各自对得上代码，混用会互相掩盖 | 源码实测（`internal/web/handlers_lifecycle.go` + `internal/web/readiness.go`） |
| T116-R5 通知渠道提示只从 GET 就绪视图断言 | `notifyNotice` 只在 `handleAdminAgentReadiness` 里追加，409 响应体里没有；从 409 断言会永远红 | 源码实测（`internal/web/readiness.go`） |
| T116-R6 E9 的「本次生效」以就绪提示为主证据、配置文件为副证据 | `budget.max_iterations` 没有直接的读回端点，能观测到它生效的只有「超上限提示」这条派生读数；配置文件同时保持不变，两头对得上才能排除「提示写死」 | 设计 §2.6.4 + T114 的提示实现 |
| T116-R7 E7 / E9 / E10 绑定模板写死的 8080，不以空闲端口替代 | `http.addr` **没有**环境变量覆盖；换成空闲端口就换掉了被测对象（新实例模板的默认值）。`us14dRequireFreePort` 等 20s 后带可执行处置判红，不静默改端口 | 源码实测（`internal/cli/config.go` 无 addr 覆盖）+ 任务卡 E7 |
| T116-R8 本文件不用 `t.Parallel` | 三个用例争同一个固定端口；并行会让失败取决于调度顺序，读数不可复现 | 同 T116-R7 |
| T116-R9 E2 的环境里把 `GITHUB_PERSONAL_ACCESS_TOKEN` 置空串 | 夹具的 `registerSecret` 跳过空值，且只脱敏 `_KEY`/`_TOKEN`/`_SECRET`/`_PASSWORD`/`_URL` 结尾的变量名；置空是让「无凭据」可复现，不是泄露 | 源码实测（`internal/e2e/harness_test.go` `registerSecret`） |
| T116-R10 全部用例的数据根隔离到用例自己的临时目录 | `LOCALAPPDATA` / `APPDATA` / `HOME` / `XDG_DATA_HOME` 一起改，三平台的数据根都避开真实用户目录；否则用例会读写本机真实 admin-token，并把「本机已有一把钥匙」误当成产品行为 | 源码实测（`internal/app/instance_identity.go` 三平台分支） |
| T116-R11 `-run ^TestUS14D` 会顺带选中两只既有 Demo 用例 | `TestUS14DemoInstall_Artifact` / `TestUS14Demos_Fixture` 与 E1–E10 同前缀；冻结命令不改（它是超集不改变判据语义，且两只用例归 `demo-fixtures` 且已绿） | stage 冻结命令 + 本轮实测 11 例全绿 |

**验证读数**（2026-10-26）：

- `go test -tags=e2e -count=1 -v -timeout 900s ./internal/e2e -run '^TestUS14D_'` → 九例全 PASS，包级 `ok 30.138s`；`binary_source=temp`，`binary_sha256=0ec42af60fef17a010223fa8c1861a70c6298513f0b1b0b45c55898ddac3d239`。
- 冻结命令原样复跑 `go test -tags=e2e -count=1 -timeout 900s ./internal/e2e -run '^TestUS14D'` → 11 PASS / 0 FAIL / 0 SKIP，退出 0。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T116` → `passed fmt (exit=0)`；`failed builtin-agent (exit=0) required test did not execute and pass: TestUS14D_FixtureEndToEnd`（报告 `accept-20261026T083640.743865000`）。红态**只**剩这一条缺失名，符合用户 2026-10-26 的裁决（冻结名先于实现，逐项转绿）。
- 同次发现并修复：`internal/e2e/harness_test.go` 的 `lifecycleRecord` 字面量对齐漂移（上一轮夹具写入引入），`gofumpt -w` 后 `sh scripts/check-format.sh` → exit 0、`gofmt` / `gofumpt` 均干净。首跑 `fmt` 因此红（`accept-20261026T083410.515661300`），修的是格式不是判据。

**勾选状态**：T111–T116 一律**不勾**。六者的 check 同为 `builtin-agent`，该检查要等 E4 落地后才整条转绿；按用户口径，冻结名未落地期间的红态属正常，但「check 全绿后才勾选」的条件尚未满足。

**未验证**：E4 端到端（随 T125–T129）；T118 的 WSL2 Linux 实机（人工项）；控制台的目录 / 诊断 / 测试 / 执行环境页（T119–T121，本机无 Node）。

### 6.4 H「T117 完整包构建与 Windows 实际安装验证」（2026-10-26）

**交付物**：`internal/e2e/us14_fullpackage_test.go` 两只用例 + 本轮发现的三处产品缺陷的修复与回归面。

| 用例 | 判据 | 读数 |
| --- | --- | --- |
| `TestFullPackage_WindowsInstallSmoke` | 从候选二进制装包（二进制 + 可离线构建的 `runtime/` 组件）→ 解压到**含空格**的程序目录（`…/Program Files/Basalt 0.1.0`）→ 首次启动生成最小配置与名册 → 用自动生成的访问密码登录 → 错密码 401 / `/console` 200 → `docs-readonly` 连上（`connected=true`）→ dbhub 已发布但未连接（node 未随包） | PASS，3.99s（报告 `accept-20261026T085323.073609600` 的 `elapsed`；修复前 63.24s 全花在等 `connected=true` 超时） |
| `TestFullPackage_ReportsMeasuredSize` | 体积实测：二进制字节数 == 磁盘文件大小，`二进制 + runtime == 总量`，`0 < 压缩包 < 总量`，zip 条目数 == 计数 | PASS，`binary_bytes=30596096 runtime_bytes=4667904 raw_bytes=35264000 archive_bytes=19464953 files=2 ratio=0.55` |

**包内容口径**：只装本机**可离线构建**的组件（`docs-readonly` 纯标准库、随主 module 构建）。Node / Python / npm 组件要经包仓库下载，属 stage approval 原文的未授权外部源——不下载、不内置，冒烟如实记录它们缺席（`missing="[node dbhub github-mcp-server]"`、dbhub 发布但 `connected=false`），**不**用一个装了半截的包冒充完整包，也**不**把设计 §2.5.3 的 150 MB 估算当成实测。

**本轮发现的三处产品缺陷**（全部为真实缺陷，不是夹具问题）：

| # | 缺陷 | 症状（现场读数） | 修复 | 回归面 |
| --- | --- | --- | --- | --- |
| D-1 | MCP 定义校验按空白一刀切拆 `command` | `重路径装配失败 … command 必须是单个可执行名`——解压到含空格路径后连 serve 都起不来 | `internal/workspace/mcp.go`：整串是已存在的文件 → 路径；首词是已存在文件且整串不是 → 旧格式命令行；其余整串保留 | `TestValidateMCPServerCandidate_AbsolutePathWithSpaces`、`TestParseMCPServers_SpaceyAbsolutePathNotSplit` |
| D-2 | 同上修复的**第一版**把旧格式「绝对路径 + 参数」也一律保留 | E3 红：`helper` / `broken` 双双 `decision=deny reason=policy_denied`（夹具 `command` 是 `<test 二进制> -test.run=…`），`good` 因 `tool not registered: echo` 永不注册 | 改为**正面证据**判据（见 D-1 终版）；Operations §5.4 的读取兼容因此保住 | `TestParseMCPServers_LegacyAbsoluteMultiWordStillSplit` |
| D-3 | 命令白名单在**查表之前**先拒含空白的 target | `MCP 授权审计 … decision=deny reason=policy_denied`（docs-readonly / dbhub 两条随包条目），开箱即用走不到「依赖就位」 | `internal/tool/policy.go`：交互式 shell 名判定上移；`filepath.IsAbs(target)` 且整串逐字登记在白名单 / 解释器集合时放行；相对多词串照旧先拒 | `TestWhitelistPolicy_AllowsAllowlistedAbsolutePathWithSpaces`（含「同目录未登记 → 仍拒」「相对多词串 → 仍拒」「含空格目录里的 `cmd.exe` → 能力上限仍拒」三态） |
| D-4 | 新实例生成的配置**没有** `shell.allowed_commands`，即使 D-3 修好也会拒 | 同上（D-3 与 D-4 叠加，先修 D-3 仍红才暴露 D-4） | `internal/app/instance_bootstrap.go`：模板按发行布局登记随包命令，与安装器**同源**（`builtinRuntimePaths` → `builtin.AllowlistedCommands`，由条目构造派生而非手抄）；取不到程序目录时整节省略（回落显式登记） | `TestInstanceConfigTemplate_RegistersBundledCommands`（模板 → `cli.LoadConfig` 解析 → 与条目命令逐条比对）、`TestInstanceConfigTemplate_NoRuntimeRootSkipsAllowlist` |

**T117 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T117-R1 随包运行时命令由**新实例配置模板**登记，不由安装器改写配置 | 配置模板只在「可证明的新实例」写一次，安装器不碰 `basalt.yaml`（同文件头「已有配置文件一律不改写」）；两者共用 `builtinRuntimePaths`，白名单与条目命令同源 | 设计 §2.5.1 + `instance_bootstrap.go` 文件头 |
| T117-R2 清单（安装清单）继续留给 US-17 拟，本增量只做显式登记 | wire.go §⑤ 的装配注释已定：清单产者属安装器（US-17 拟），本增量「程序目录里的可执行件一律要显式登记」 | `internal/app/wire.go` 装配注释 |
| T117-R3 多词 `command` 的拆分判据改为**正面证据**，不以绝对性一刀切 | 绝对路径里的空格是路径的一部分（设计 §4.1 第 1 步），旧格式读取兼容又必须保住（约束 8 / Operations §5.4）；只有「整串是文件 / 首词是文件」能同时区分两者，且两种结果都不是校验失败 | 实测 D-1 / D-2 + Operations §5.4 |
| T117-R4 `filepath.IsAbs` 豁免只到「绝对 + 整串逐字登记」为止 | 相对多词串即便写进白名单也不放行（既有 FR1 判据不变）；交互式 shell 名按基名先判（含空格目录里的 `cmd.exe` 仍拒） | 既有 `TestShellTool_MultiWordExecutableNotSplitByTool` / `TestWhitelistPolicy_RefusesBashAndShHard` 保持全绿 |
| T117-R5 `github` 条目（凭据就绪才写）的命令在首启就登记 | 否则补上凭据的那次启动仍会被拒；白名单是「可执行件登记」不是「条目授权」，多登记一条命令不构成对未写条目的放行 | `builtin.AllowlistedCommands` 派生自 `CredentialFreeEntries` + `GitHubEntry` |
| T117-R6 E3 失败信息附带被测进程输出 | 只报 profiles 读数时拒因不可见（本轮实测：真实拒因 `policy_denied` 与「读数是空的」走了两个来回报才能定位）；只增强失败信息，不改断言 | 本轮实测 |

**验证读数**（2026-10-26）：

- `go test -tags=e2e -count=1 -v -timeout 1800s -run '^TestFullPackage' ./internal/e2e/` → 2 PASS；同一代码在报告 `accept-20261026T085323.073609600` 中两只均 passed，包级 `ok 9.962s`（`TestFullPackage_WindowsInstallSmoke` elapsed 3.99s / `TestFullPackage_ReportsMeasuredSize` 1.24s）；`full-package-install program_dir=…\Program Files\Basalt 0.1.0 binary_bytes=30596096 runtime_bytes=4667904 files=2`；`full-package-size … ratio=0.55`。**读数时效**：体积随 14D 后续代码增长，报告 `accept-20261026T125328.674965300` 的同一用例为 `binary_bytes=30721536 raw_bytes=35389440 archive_bytes=19520870 ratio=0.55`；最终读数见 §6.11，本节保留 H 时刻报告的原值。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T117` → `passed fmt (exit=0)`、`passed full-package (exit=0)`（报告 `accept-20261026T085323.073609600`）。
- 受影响单测：`go test ./internal/workspace/ ./internal/tool/ ./internal/app/` 全绿（14.9s / 8.0s / 40.0s）。
- E7 增补断言（生成的配置必须登记随包运行时命令）后 E3 + 完整包同跑 `ok 13.570s`；T116 复跑见下条。

**未验证**：T118 的 WSL2 Ubuntu 实机安装（人工项，单平台读数不代替）；完整包的 Node / Python / npm 组件与其余人工项 M1–M8；`internal/builtin` 目前无包内测试文件，覆盖率门禁（≥50%）需在 T124 前补齐或如实登记。

**T116 复跑**（2026-10-26，`check builtin-agent`，报告 `accept-20261026T090144.949470300`）：`passed fmt (exit=0)`；`failed builtin-agent` 唯一红项 = `required test did not execute and pass: TestUS14D_FixtureEndToEnd`——E4 依赖 `web_search` 内置工具（Phase 14 / T125–T129）尚未落地，按「实施前测试不存在、存在性检查呈红态属于正常情况」的裁决保留红态，不写占位测试凑绿。其余 9 只具名测试与全部子测试 PASS（`TestUS14D_AgentsDisabledAfterInstall` / `MCPAutoPublishedOnCredential` / `StartupIsolation` / `DisabledNotScheduled` / `EnableBlockedWithoutConfig` / `FreshInstanceBudgetPreset`（含两个子测试）/ `BudgetCompatNotices` / `ConfigEffectiveThisStart` / `ExistingNoConfigUntouched`）。T117 修复前的 T116 读数作废，以本条为准。

### 6.4 I「T118 完整包 WSL2 Ubuntu 实际安装验证」（2026-10-26）

**执行环境（实测读数，本地路径已脱敏）**：WSL2，发行版 **Ubuntu 26.04 LTS**，内核 `6.6.114.1-microsoft-standard-WSL2`，Go `go1.27.3 linux/amd64`，安装根所在文件系统 `ext4`（`/dev/sdd`），执行用户 `<user>`。本段读数中的本机绝对路径统一写成 `<user>` / `<repo>` 占位（`specs/**` 是公开跟踪路径，不落本机用户名与目录），只替换路径前缀与用户名，读数本身逐字保留。这条读数只说明"WSL2 Ubuntu 上跑通了"，**不**扩大为"所有 Linux 部署形态均已验证"（设计 §9 平台口径）。

**判据落地**：新增 `TestFullPackage_LinuxInstallSmoke`（`internal/e2e/us14_fullpackage_test.go`），只在 `linux` 上执行、Windows 上带理由跳过。它判四件事：

| # | 判据 | 读数 |
| --- | --- | --- |
| 1 | **Linux 产物 + Linux 文件系统**：安装根放用户主目录（`/home/<user>`），路径含空格（`Basalt Install/Basalt 0.1.0`）；落在 `/mnt/**`（drvfs）直接判红并点名——那里跑出的绿态没有证据价值 | `program_dir=/home/<user>/basalt-linux-install-3411706903/Basalt Install/Basalt 0.1.0` |
| 2 | 解压 → 首次启动 → 登录 → 依赖就位（与 Windows 半同口径：配置监听 8080、生成的访问密码可登录、错密码 401、五只内置 Agent 在册、`docs-readonly` `connected=true`、dbhub 已发布未连接） | `binary_bytes=30250655 runtime_bytes=4541558 files=2`，`binary_sha256=75811a1c7e4e19ad…`（WSL 内构建的 Linux 产物，与 Windows 产物不是同一文件，各自独立构建） |
| 3 | **权限位**：实例私有目录 `0700`、`master.key` `0600`、`admin-token` `0600` | 三项全绿（Windows 半无法判这一面） |
| 4 | **实例锁与进程行为**：同一工作区的第二个 `serve` 被拒（输出点名「实例锁已被占用」）且 `basalt.yaml` 一字未改；`SIGINT` 优雅停机退出码 0，停机后 `/proc` 中无进程再引用随包 `docs-readonly` | 全绿 |

**命令与结果**（2026-10-26）：

```text
wsl.exe -e bash -lc 'cd <repo> && GOPROXY=https://goproxy.cn go test -tags=e2e \
  -count=1 -v -timeout 900s -run "^TestFullPackage_LinuxInstallSmoke$" ./internal/e2e/'
→ PASS (2.50s)，包级 ok 21.364s
```

Windows 侧复发同一 command（`-run '^TestFullPackage'`）→ `ok 9.936s`（Linux 用例按理由跳过，不影响 `full-package` 的 pinned required_tests）。

**T118 就近裁决**：T118-R1 `TestFullPackage_LinuxInstallSmoke` **不**加入 stage `full-package` 的 `required_tests`。理由：阶段在 Windows 上执行，验收器把 required test 的 skip 判红（`execute.go`「required test was skipped」），加进去等于让 Windows 阶段永远红；Linux 半的证据 = 本条记录的命令 + 读数，将来 Linux CI job 可直接复用同一命令。T118-R2 用例在 `/mnt/**` 上直接判红而不是跳过：跳过会让"在 Linux 文件系统内验证"这一条悄悄消失。

**未验证**：真实 Linux 服务化部署（systemd 单元 / 非 root 服务账户 / 发行版包管理）；WSL2 以外的发行版（Debian / RHEL 系）；完整包的 Node / Python / npm 组件在 Linux 上的安装（本轮未授权下载）。

**T118 证据落盘（2026-10-26，T124 收口轮；此前只有正文读数，无机器可查日志）**：在当前工作区（`4641c7c` + 未提交 14D 改动）上重跑同一命令，日志存 `.loop/runs/us14-14d-t118-linux/linux-install.log`（exit 0）：`--- PASS: TestFullPackage_LinuxInstallSmoke (2.88s)`、包级 `ok 12.252s`；`binary_source=temp`、`binary_sha256=7bd999c9fccf7a30…`（Linux 侧独立构建，与 Windows 候选不是同一文件）、`binary_bytes=30373280 runtime_bytes=4541558 files=2 instance_id=inst_11a532ea4707cf2a`、`program_dir=/home/<user>/basalt-linux-install-329536278/Basalt Install/Basalt 0.1.0`（路径已脱敏，原值是同一形态）。体积随 14D 代码增长（此前 30250655），判据未变。

### 6.4 J「T119 执行环境页：档位切换 / 禁止域名 / 例外表 / 待确认项」（2026-10-26）

**交付物**：`assets/console/src/api.ts`（读面）、`api-admin.ts`（写面）、`views/workspace.ts`（`SandboxView` 重写）、三个测试文件；`assets/console/dist` 与 `integrity.json` 重建。

| 面 | 落地 |
| --- | --- |
| 读面 | `SandboxView` 扩 `execution_profile` / `execution_policy` / `denied_domains` / `network_denylist`；新增 `ExecutionPolicyView`（档位、例外条数、受保护地址段、shell 名、待确认项）、`NetworkExceptionListView`；`NetworkAllowlistView.source` 收为四值联合（`config_seed` / `database` / `migration` / `instance_init`） |
| 写面 | `PUT /sandbox/denied-domains`（禁止域名全集 + 禁用表 ETag）、`PUT /execution-policy`（`profile` + `expected_profile` + 策略 ETag）、`POST /execution-policy/confirmations`（无 If-Match，只写 `keep_denied`）、`POST /network-exceptions`、`DELETE /network-exceptions/{id}`（用**集合** ETag） |
| 页面 | 摘要六行 → 待确认卡片 → 处置反馈 → 例外表 → 禁止域名 → 白名单 → 技术细节；档位切换走确认框，方向在打开时冻结 |
| 测试 | 新增 `tests/sandbox-policy.test.ts`（10 例 / 5 组）；`sandbox-allowlist.test.ts` 夹具补四字段；`workspace.test.ts` 补三端点桩 |

**T119 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T119-R1 「运行环境」行只写可证明事实：受控解释器条数 + 指向 MCP「依赖」轴，**不**写 Node / Python 版本 | 本仓没有运行时清点端点（`internal/app/mcp_runtime.go` 的诊断按 MCP server 分轴），页面上任何版本号都只能是编造；用户场景要的是「缺什么」而不是清单 | 源码实测 + FR6/FR7 的分轴口径 |
| T119-R2 白名单空态文案**按档位**分叉，并判定契约描述有误 | `openapi.yaml` 的 `http_allowed_domains` 描述写「`[]` 表示出站请求全部拒绝」，在 `standard` 档为假（标准档放行未命中禁止域名的公网）。控制台写两种真话；`openapi.yaml` 属固定字面量，**不**在实施中顺手改，转 T123 统一裁决 | `internal/tool/policy.go:hostAuthorized` 实测两档行为 + 契约原文 |
| T119-R3 处置反馈渲染在待确认卡片**之外** | 最后一条待确认项被处置后卡片整体消失，反馈若挂在卡片里会跟着消失（本轮两例测试先红后修） | 本轮实测（`pendingFeedback` 提到页级） |
| T119-R4 ETag / 修订号只进「技术细节」折叠区 | 主界面要回答「现在是什么、缺什么」，ETag 是排障信息；摘要行只显示「修订 N」与来源 | 设计 §4.5 信息分层 + A15 判据 |
| T119-R5 三个写面失败一律「重新读取 + 不自动重发」，草稿保留 | 现有条件写判据（412 / 409）在档位、禁止域名、例外删除上同构；自动重发会绕过用户确认的方向 | 既有 `sandbox-allowlist` 判据 + T103 / T107 契约 |

**验证读数**（2026-10-26）：

- `npm --prefix assets/console test` → `Test Files 22 passed (22)` / `Tests 244 passed (244)`（3.08s，Node 24.22.0 便携副本）。
- `npm --prefix assets/console run build` → `vue-tsc --noEmit` 通过；`dist/assets/index-D5LejwwN.js` 317.80 kB（gzip 103.50 kB）、`index-BZZL5gRG.css` 34.07 kB；`integrity.json: 3 个文件`。旧的 `index-C8YvltG4.js` 被替换（`git status` 中一删一增）。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T119` → `passed fmt (exit=0)` / `passed console-test (exit=0)` / `passed console-build (exit=0)`（报告 `accept-20261026T091734.777103500`）。

**未验证**：浏览器人工走查（A15 的可打开 / 可读性属 review ② 人工项）；`dist` 重建产物未提交（提交时机归用户）。

### 6.4 K「T120 MCP 六轴与凭据表单」（2026-10-26）

**交付物**：`assets/console/src/views/resource-detail.ts`（六轴 + 占位符凭据区 + 应用区）、`components/resource-form.ts`（逐占位符来源卡片、请求头重填、保存并应用、`createdApplyNotice`）、`components/credential-field.ts`（`valueVisible`）、`views/resources.ts`（凭据列 + 创建提示）、`views/catalog.ts`（创建提示 + 依赖措辞）；四个测试文件。后端契约无改动——`header_keys` / `credential_keys` / 逐槽位撤销 / apply 端点与结果字段在先前任务已冻结并实现。

| 面 | 落地 |
| --- | --- |
| 六轴 | 依赖 / 配置 / 连接 / 测试 / 工具 / 引用各自成行；配置轴按 `credential_keys` + 未声明槽位判定，连接轴 `pending_restart` → 待应用并写「运行 vX，发布 vY」 |
| 凭据区 | 逐槽位表格：占位符、来源、状态、尾号、撤销；未声明槽位单列「未声明来源」；撤销走确认框，`binding_key` 为槽位名本身 |
| 表单 | 每个占位符一张来源卡片（默认加密托管，env 为高级选项），托管值 write-only；请求头文本域与「清空请求头占位」互斥；「创建并应用 / 保存并应用」为第二动作 |
| 应用区 | 成功读 `applied/previous/target` 与排空窗口；失败只认操作台账（`operation_id` 查行），不合成结论；`superseded` 显示「被取代（未切换）」 |

**T120 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T120-R1 依赖轴 `dependency=null` 记「不适用」，不记「缺失」 | 探测只覆盖程序目录 `runtime/mcp/<组件>/<版本>/…`；`null` 表示命令不指向已登记组件，写「缺失」等于把「不检测」说成「缺」 | 契约 `McpDependencyDiagnosis` + `internal/app/mcp_dependency.go` 实测 |
| T120-R2 未声明来源短路为「校验失败」，不并入「缺凭据」 | `credential_keys` 只列已声明槽位；`(env_keys ∪ header_keys) − credential_keys` 是配置错误，比就绪判定更靠前，且下一次保存会被拒 | 契约 `McpCredentialKeyView` 注释 + `internal/core/mcp_credential.go` |
| T120-R3 env 来源下不渲染值输入框 | 映射表里的 `KEY=${VAR}` 就是声明；env + 值会被 400 拒绝，留一个「填了也不生效」的框等于骗用户 | `indexMCPCredentialInputs` / `normaliseCredentialValue` 实测 |
| T120-R4 修改态请求头必须重填或显式清空 | `UpsertMCPServer` 以 `req.Headers` 整体替换条目，与 env 同构；静默丢失占位符会让已发布定义悄悄降级 | `internal/app/resource_admin.go` 实测 |
| T120-R5 修改态槽位草稿取自服务端已有声明，不默认「加密托管」 | 默认托管会把既有 env 槽位改成 managed，且无值 → 503；草稿必须从 `credential_keys` 回填 | 表单实测（本轮修复） |
| T120-R6 应用结果四态（未点组合按钮 / applied / 失败 / 被取代）分开说，未切换按 warn | 表单 emit 即被父页面卸载，它自己的提示随之消失，结论必须由 `createdApplyNotice` 重述；保存成功不等于运行版本切换 | 本轮实测（父页面提示为空的问题） |
| T120-R7 列表「凭据」列按四档计数、未声明来源单独标红；列名改「占位符键」 | http 的请求头名不是环境变量，原名会误导；列表不写尾号，逐槽位明细留详情页（设计 §2.6.3「不加新表格」） | 设计 §2.6.3 + 契约 `header_keys` |
| T120-R8 目录详情的依赖标为「依赖（配方要求）」 | 目录只写「需要什么」；新建态没有诊断（诊断只对已发布 server），设计 §2.6.3 要求的「依赖状态徽标」在新建表单无法诚实给出，改为文案指向保存后的「依赖」轴 | 设计 §2.3.4 / 真实端点面 |

**验证读数**（2026-10-26）：

- `npm --prefix assets/console run typecheck` → `npm info ok`（vue-tsc 3.3.13）。
- `npm --prefix assets/console test` → `Test Files 22 passed (22)` / `Tests 253 passed (253)`（3.52s，Node 24.22.0 便携副本）。本任务新增 4 例：`catalog.test.ts` 两例（保存并应用成功 / 409 被取代时目录页重述）、`resource-list.test.ts` 两例（四档计数 + 未声明来源 / 列表无尾号）。
- `npm --prefix assets/console run build` → `dist/assets/index-3A2vPR85.js` 333.74 kB（gzip 108.61 kB）、`index-BZZL5gRG.css` 34.07 kB；`integrity.json: 3 个文件`。连跑两次同一 SHA-256（构建可重放）。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T120` → `passed fmt (exit=0)` / `passed console-test (exit=0)` / `passed console-build (exit=0)`（报告 `accept-20261026T094204.538959100`）。

**证据复跑**：`views/resources.ts` 文件头注释补「凭据档位」后（纯注释，不影响产物）复跑 `--task T120` → fmt / console-test / console-build 三项 exit=0（报告 `accept-20261026T094305.467157500`），本节读数对应当前工作区。

**未验证**：浏览器人工走查（六轴可读性、应用失败的操作台账入口属 review ② 人工项）；`dist` 重建产物未提交——`git status --porcelain -- assets/console/dist` 现为「删 HEAD 的 `index-C8YvltG4.js` / 增 `index-3A2vPR85.js` + `index.html` + `integrity.json`」，提交时机归用户。

### 6.5 L「T121 Agent 三段视图 / 实例加密两行 / 登录页文案」（2026-10-26）

**交付物**：`assets/console/src/instance-secrets.ts`（状态 / 原因 / 汇总 / 形状守卫的单一文案源）、`components/agent-readiness.ts`（需要你提供 / 系统已准备 / 未启用原因三段）、`views/agent-detail.ts`（四路只读加载：就绪、MCP 列表、/info、工作区树）、`views/overview.ts`（实例与凭据两行 + 技术详情）、`app.ts`（恢复态全站横幅 + `basalt:instance-secrets-changed` 刷新）、`components/credential-field.ts`（`managedDisabled` / `managedReason`）、`components/resource-form.ts`（实例状态行、禁用托管、就地初始化与确认框）、`views/login.ts`（共享访问密码固定句）、`workspace.css`（`.ready-*` 网格）。九个测试文件：新增 `instance-secrets.test.ts`（8 例）、`agent-readiness.test.ts`（5 例），扩展 `workspace.test.ts`（4 例）、`resource-form.test.ts`（3 例）、`agent-operations.test.ts`（1 例）。

| 面 | 落地 |
| --- | --- |
| 三段视图 | 阻断项只来自 `/agents/{name}/readiness.pending`；`fix` 直链控制台既有页面；未启用原因写「还差 N 项」+ 首个修复入口 |
| 系统已准备 | 服务版本、按声明逐个核对 MCP（未发布 / 待重启双版本 / 已连接工具数）、脚本与样例库目录；读不到写「暂不可读」 |
| 实例加密 | 总览两行（状态 + 汇总）+ 详情；恢复态顶栏下全站横幅；未初始化可在模型连接表单内初始化，成功后就地翻绿并广播事件 |
| 登录页 | 共享访问密码固定句 + 身份来源说明；Go 断言的四件文案保持不变 |

**T121 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T121-R1 提示项分段：`notify_channel` 进「需要你提供」（状态词「提示」、不计入「还差 N 项」），`budget` / `concurrency` 只进「未启用原因」 | 通知是页面能给的修复入口，预算与并发是运行限制而非待补配置；三者都不得冒充阻断项 | 契约 `AgentReadinessItem.kind` 注释（前三个只进 pending，后三个只进 notices）+ `internal/web/readiness.go` 实测 |
| T121-R2 「系统已准备」只列可读事实；服务版本写「运行时组件随安装包提供」 | `/info` 只暴露服务版本与计数，控制台读不到各运行时组件版本；编一个或空着都不如说清边界 | 契约 `InfoView` + 本轮实现 |
| T121-R3 脚本 / 样例库目录缺失按中性「无（目录不存在，自定义 Agent 不要求）」显示 | 控制台无法区分内置 Demo（缺失是缺陷）与自定义 Agent（本就不要求），也没有文件缺失的修复页面；判红会制造无法消除的告警 | 设计 §2.5.1 措辞 + 工作区端点面 |
| T121-R4 只有显式 `uninitialized` / `recovery_required` 禁用「加密托管」；读取失败保留可提交并说明 | 读不到状态不等于服务不可用；服务端仍是最终闸门（503 / 409），页面只把不确定性说清楚。环境变量来源在任何状态都可用 | 契约 `InstanceSecretsView.state` + `internal/web/instance_secrets.go` 拒绝语义 |
| T121-R5 未知响应形状按「未知」处理：总览「读取中 / 待确认」、不挂横幅、不出现「就绪」 | 把读不懂的响应说成安全比不显示更糟；横幅只在能证明恢复态时出现 | `isInstanceSecretsView` 守卫 + 本轮用例 |
| T121-R6 既有用例的判据面按原意维护：挂载期实例加密探测由公共夹具应答，原「零写入 / 不重新 GET 列表」断言改用 `resourceCalls()` 排除该已知只读 | 新探测是页面自身行为，不是资源读写；排除一条显式命名的只读请求，不白名单吞掉其他未预期请求，断言强度不变 | `resource-form.test.ts` / `agent-config.test.ts` 实测（修复前 30 例红） |

**验证读数**（2026-10-26）：

- `npm --prefix assets/console run typecheck` → `npm info ok`（vue-tsc）。
- `npm --prefix assets/console test` → `Test Files 24 passed (24)` / `Tests 274 passed (274)`（3.29s，Node 24.22.0 便携副本）。新增 21 例：`instance-secrets.test.ts` 8、`agent-readiness.test.ts` 5、`workspace.test.ts` 4（总览两行 / 全站横幅 / 未知形状 / 登录页共享密码句）、`resource-form.test.ts` 3（未初始化就地初始化 / 恢复态禁用 / 读取失败保留可提交）、`agent-operations.test.ts` 1（Agent 页三段接线）。
- 红态留证：首次 `--task T121` 先暴露 `fmt` exit=2（`internal/app/web.go`、`internal/web/instance_secrets.go`、`internal/web/instance_secrets_test.go` 未格式化）与 `executable unavailable: npm`（PATH 缺 Node 便携副本）；前者 `gofumpt -w` 修三文件，后者在命令内显式导出 PATH 后复跑。该次报告 `accept-20261026T101313.686940200`（status=error）保留。
- `npm --prefix assets/console run build` → `dist/assets/index-BuQHQzBe.js` 348.93 kB（gzip 113.55 kB）、`index-Bs2SzWxe.css` 34.66 kB；`integrity.json: 3 个文件`。连跑两次 SHA-256 一致（js `abbfa776…`、css `e0faea1c…`，构建可重放）。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T121` → `passed fmt` / `passed console-test` / `passed console-build` / `passed admin-login`（exit=0，报告 `accept-20261026T101332.743187600`）。

**未验证**：浏览器人工走查（三段面板可读性、横幅视觉、初始化对话框属 review ② 人工项）；`dist` 重建产物未提交——`git status --porcelain -- assets/console/dist` 现为「删 `index-C8YvltG4.js` + `index-BZZL5gRG.css`，增 `index-BuQHQzBe.js` + `index-Bs2SzWxe.css`，改 `index.html` / `integrity.json`」，提交时机归用户。

### 6.6 M「T122 O1 升级影响清单」（2026-10-26）

**交付物**：`docs/Operations.md` §5.4 末尾新增 `#### 升级影响清单（受保护地址与 shell 名称）`（77 行，不重编号 §5.5），把 `schema_version` 5 → 6 的两轴待确认项、两个显式动作、只读检查命令、dry run 顺序与回退口径写在同一处；`internal/cli/docs_14d_test.go` 新增 `TestDocs14D_FactsMatchImplementation`（subtest `upgrade-impact-list`）+ `repoRoot` 助手，把文档字面量与契约 / 路由 / 控制台对齐。

| 文档声明 | 可核对来源（测试断言） |
| --- | --- |
| 两轴 `network_allowlist` / `shell_command` | `core.PendingConfirmationAxis`；`workspace.ts` 的 `PENDING_AXIS_LABELS` |
| 「加例外」→ `POST /api/v1/network-exceptions` | `openapi.yaml` 路径块 + `admin_routes.go:167` 注册行 |
| 「保持拒绝」→ `POST /api/v1/execution-policy/confirmations` | `openapi.yaml` 路径块 + `admin_routes.go:187`；契约 `PolicyConfirmationWrite.decision` 唯一取值 `keep_denied` |
| 「不改授权结论」「未确认不放行」 | `workspace.ts:453/504/505` 文案 + `internal/app/execution_policy.go` 的 keep-denied 实现 |
| 迁移夹具可复现 | `TestMigrate_V5ToV6`（`seedV5Database` → `Migrate(...,6)`，`internal/storage/migration_v6_test.go:192`）与 `TestExecutionProfile_MigrationStrict`（`internal/app/execution_policy_migration_test.go:128`，含受保护地址解析与 `bash` / `cmd.exe` 命中）——既有覆盖，本轮不新增夹具 |

**T122 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T122-R1 文档只给只读查询 + 备份副本上 dry run，不给改当前实例的快捷命令 | 任务要求「不得改当前实例凑证据」；清单是只读派生，dry run 不需要动生产目录 | 任务卡 T122 原文 + `PendingConfirmations` 无写入路径 |
| T122-R2 shell 轴只列「保持拒绝」，域名轴才有「加例外」 | 契约 `decision` 当前唯一取值 `keep_denied`，放行走 `POST /network-exceptions`；文档不描述不存在的动作 | `openapi.yaml` `/execution-policy/confirmations` 描述 |
| T122-R3 断言取代码 / 契约里既有字面量，不改文案凑测试 | 事实同步方向固定为「实现 → 文档」，测试反向锁住；首次红态即 `**不得**为了让清单变短`（文档带加粗标记）——改断言不改文档 | 首次运行 `docs_14d_test.go:52` 失败读数 |
| T122-R4 新测试放 `internal/cli` 并与 `TestPublicDocs_*` 共用 `-run` 正则，T123 继续追加 subtest | stage `docs-consistency` 的 argv 已冻结为该正则，新增 subtest 无需改 stage 判据面 | `harness/stages/us14-14d.json` |

**验证读数**（2026-10-26）：

- `go test ./internal/cli -run '^TestDocs14D'` → 首次红（`missing "不得为了让清单变短"`），改断言后 `ok`；与公共文档测试合并跑 `-run '^TestDocs14D|^TestPublicDocs_'` → `ok`（1.175s）。
- `gofumpt -l` / `gofmt -l` 对新文件均无输出。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T122` → `passed fmt` / `passed docs-consistency`（exit=0，报告 `accept-20261026T101713.856717600`）。

**未验证**：T123 的其余 14D 事实（新端点全表、枚举、错误码、`secrets` 子命令、两档、访问密码文件、出网边界）尚未写进 `TestDocs14D_FactsMatchImplementation`；真实实例升级前的清单核对属 review ② 人工项（本轮只在备份副本 dry run 口径上落文档，未对演示实例执行升级）。

### 6.7 N「T123 事实同步」（2026-10-26）

**交付物**：三份权威正文与运维手册按实现同步，并在 `internal/cli/docs_14d_test.go` 追加两个 subtest 反向锁住。

| 文档 | 本轮改动 |
| --- | --- |
| `docs/TechnicalDesign.md` | §3.7 管理面认证补第三来源与「新实例首次初始化 / 存量实例恢复态」；§4.3.1 命令与 HTTP 行改为按档位描述；新增 `#### 4.3.4 执行档位与出网边界（14D）`（两档、共享基础防护、访问例外、拒绝细分、待确认项）；§5.4 新增 `execution_policy` / `network_exceptions` / `instance_secrets` / `credentials`（v6 重建）四行并把 `schema_version` 改为 v6；§6.1.2 命令块换成真实命令集（含 `secrets` / `demo install`）；§6.2.4 补 14D 端点全表；§6.2.6 补访问密码三处来源 |
| `docs/Requirements.md` | §5.1 `shell` / `http_get` / `http_post` 行按 shell 名称集与档位改写，新增「执行档位（14D，两档）」「访问例外（仅网络）」两段；§5.2 MCP 逐键来源与 503 env 闸门、主密钥新实例初始化与实例加密可见性；§5.3 登录页共享密码固定句；§5.4 允许省略 `token`；§5.8 命令块补 `secrets` / `demo install`；§6.6 表补四行并改 `schema_version` v6；§7.1 补 14D 范围增量 |
| `docs/Compatibility.md` | §2.3 管理台追加端点与认证来源、§2.5 新增表补至 v6（含 `credentials` 重建的 `binding_key` 空串迁移影响）；§3 新增 45–50 六行（两档与迁移落 `strict`、共享出网防护、访问例外、实例加密初始化放宽与可见、MCP 逐键凭据与 env 闸门、访问密码第三来源） |
| `docs/Operations.md` | §2.1 管理认证行 / 必须段 / 复核句补三处来源与「新建实例自动生成、存量实例不生成」；§2.1.1 新增第三来源段（分文件、0600、非交互只打印路径、轮换口径）；§2.2 出网表头改「受不受出网策略」、MCP http 与 stdio 启动行按档位改写，新增 `执行档位与出网基础防护（14D）` 六条块，热生效清单补档位 / 禁止域名 / 访问例外；§6 凭据表补 `http.admin_auth.token` 新边界与 MCP 逐槽行；§6.1 `生成` 改 `secrets init` / JSON 文件格式 / `auto_init` 与「已有实例绝不自动生成」，补「就绪视图」与 MCP 撤销带 `binding_key`，`secrets init` 行与典型时序按自动初始化改写 |

**T123 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T123-R1 私有正文的断言放进同一测试、按缺席跳过 | 三份正文在 `.git/info/exclude` 内，干净检出与 CI 不存在；沿用 `requirements-local` 的 `t.Skip` 理由（本地漂移保护），不伪造通过，也不把断言语义降级成可选项 | `internal/cli/public_docs_test.go:81-87` 既有口径 |
| T123-R2 「写了 `token` 键却解析为空」不写「改读密码文件」 | 装载层把 `http.admin_auth.token` 当凭证键：占位变量缺失 / 解析为空 / 残留 `${` 都是装载期报错，不会落到密码文件——文档若写「回退到文件」会诱导部署者留着半截配置 | `internal/cli/config.go:996-1018`（`resolveString` 凭证分支）+ `validateAdminAuth` 注释 |
| T123-R3 `secrets status` 仍只统计模型连接与通知渠道，MCP 槽位只进 `credential_summary` | 命令面实现只数 `managedDependencyCounts` 的两类；API 只读视图四档计数才含 MCP 槽位。文档按各自真实口径写，不把两者混成一句 | `internal/app/credential_ops.go:1140-1169` + `internal/web/instance_secrets.go:257-323` |
| T123-R4 「密钥文件已存在、缺初始化记录」写成下次 `serve` 自动 adopt，而非再次 init | 判定表在有文件无记录时给 `SecretsActionAdopt`，`Ensure` 在启动时执行；`Init` 明确拒绝并要求改用 adopt。写成「再跑一次 init」会与实现矛盾 | `internal/app/instance_secrets.go:150-160/300-340` |
| T123-R5 出网表头由「受不受白名单」改为「受不受出网策略」 | MCP http 连接与 stdio 启动都走 `Policy.Check`，`standard` 档下前者按禁止域名表、后者按安装清单程序目录放行；沿用旧表头会在标准档下失真 | `internal/app/mcp_supervisor.go:604-612` + `internal/tool/policy.go:335-340/507-550` |
| T123-R6 云元数据「即使加例外也拒」写进运维手册与权威正文 | 判序在例外之**前**（`classify` 先判 metadata 再查例外），且文档若暗示「加例外可放行元数据」会变成安全误导 | `internal/tool/netguard.go:175-190` |

**验证读数**（2026-10-26）：

- `gofumpt -w internal/cli/docs_14d_test.go` 后 `go test ./internal/cli -run '^TestDocs14D' -count=1 -v` → `PASS`：`upgrade-impact-list`、`operations-sync`、`authoritative-sync` 及三份私有正文 subtest 全绿（本机三份文件在位，断言实际执行而非跳过）。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T123` → `passed fmt (exit=0)` / `passed docs-consistency (exit=0)`，报告 `.loop/runs/accept-20261026T102519.935329400/report.json`。
- 断言字面量在写入前逐条 grep 核对过实现源（`core.Denial*`、`core.SecretState` / `SecretRecoveryReason`、`openapi.yaml` 两个路径块、`admin_routes.go:196-197`、`internal/cli/secrets.go` 五个 `Use:` 字面量）。

**未验证**：本轮未对 `docs/Operations.md` 跑 Markdown linter（本机 Node 需便携副本，且公共门禁无 markdown 检查项）；新增段落的长表格与跨行 blockquote 属 review ② 的阅读项。真实实例升级前的清单核对仍未对本机演示实例执行（T122 已记，只在副本上 dry run 的口径写进文档）。

### 6.8 O「T125–T129 内置 `web_search`：D5 检索通道从 Brave MCP 换成 DeepSeek Search API」（2026-10-26）

**交付物**：

| 文件 | 改动 |
| --- | --- |
| `internal/tool/websearch.go` | 新增。纯标准库 `net/http`（零新增 Go 坐标）：端点 `https://api.deepseek.com/v1/tools/search` 为编译期常量；入参 `{query, count?}`（`count` 缺省 10、区间 1–50、超界判 `invalid_input`）；请求体固定 `{query, count, need_content:false}`；响应消费 `rerank_score` / `title` / `url` / `snippet` / `summary` / `date` / `authority` 七字段（官方 `date` 为 `null` 时写空串）；无标题、URL 非绝对 http(s)、URL 带 userinfo 的条目丢弃；仅 408 / 429 / 5xx 重发（最多 2 次，200 ms 起逐次翻倍，sleep 可注入且受 ctx 约束）；非 2xx 只出状态码与 host，401 / 402 / 403 / 404 / 408·429 / 5xx 各带固定指引；响应体上限 1 MiB、超限按 `external_error`；凭据每次调用重新解析、不缓存 |
| `internal/app/search_credential.go` | 新增 `webSearchCredentialSlot`：Agent → `provider.name` → 已发布模型连接指针 → 发布版本条目 → 既有凭据解析（与 provider 目录同一条路）；两段式绑定（注册表装配后 `Bind`），未绑定即报装配错误而不是拿别的连接顶上；**不校验连接是否 DeepSeek**（T126-R1） |
| `internal/app/wire.go` | 注册池插入 `tool.NewWebSearchTool(policy, webSearchSlot)`（内置先注册先占名，`dispositions.recordBuiltin` 照记）；`declaredToolNames` 扩到 11 个（`web_search` 按字典序位于 `shell` 与 `write_file` 之间），与 `registry.Names()` 相等的不变式照旧拒绝启动 |
| `internal/tool/websearch_test.go` | 新增 11 只顶层用例：7 只冻结名（`_ParsesResults` / `_RunsWithoutBraveEnv` / `_Retry429Bounded` / `_NoRetry401` / `_NoSecretInAudit` / `_CountsBudget` / `_FailureLeavesNoSources`）+ `_InputValidation` / `_PolicyDeniedNoSend` / `_MissingCredentialFailsClosed` / `_TransportErrors` / `_BackoffHonoursContext`；`_Retry429Bounded` 覆盖「第二次成功」「第三次成功」「始终 429/500/504 即停」三组子用例 |
| `docs/contracts/agent-md.md` | §5 标题改「内置工具契约（11 个）」；新增 `### 5.7 SearchTools（1 个）`：参数表、返回七字段恒定、四类失败、凭据与判定四条硬约束、固定请求体、有界重试、一调用一审计行；末段 blockquote 指认官方核对记录并写明**契约不声明 QPS 数值** |
| `docs/Operations.md` | §2.2 新增「搜索出网（`web_search` → `https://api.deepseek.com/v1/tools/search`）」行（受同一套档位判定、每次外呼各判一次、`strict` 必须放行 `api.deepseek.com`、`standard` 未命中禁止域名即放行）；§7 依赖段与 §7.1 D5 行改为「内置 `web_search` + Fetch、无专用变量」 |
| `examples/daily/industry-brief/{AGENT.md,README.md,SETUP.md,scripts/industry_brief.py}` | D5 改「内置 `web_search` 检索 + Fetch 抓正文」：`tools: [web_search, fetch, read_file, write_file, shell]`、`mcp_servers: [fetch]`；`search.json` 契约不变（`{queries:[{query,results:[{title,url,description,page_age}]}]}`，`snippet`→`description`、`date`→`page_age`，`date` 空串则省 `page_age`，不补齐日期）；SETUP 只配 Fetch MCP 并单列「放行 `web_search` 出网域名」；脚本只改文案与 argparse 帮助（判定面零改动） |
| `examples/README.md`、`README.md`、`website/.vitepress/data/templates.mjs` | D5 依赖与证据口径、凭据指引（唯一凭据 = GitHub PAT）、已验证列表（DeepSeek Search）同步；Brave 段保留但标注「无 Demo 使用」 |
| `docs/Requirements.md`、`docs/TechnicalDesign.md`、`docs/Compatibility.md` | 私有正文：内置十一工具、D5 双通道行、§5.1 `web_search` 行、§4.2 `SearchTools` 行与「为什么不做成 MCP server」、§8.1 D 规则；兼容稿补「第 11 个内置工具不在互换承诺内、基线侧会 `tool not registered`」三处 |
| `internal/cli/docs_14d_test.go` | 新增 `web-search-sync` subtest（契约 §5.7 五处字面量、`websearch.go` 四处 JSON 名、运维手册三处、`industry-brief/AGENT.md` 声明 `web_search` 且禁止 `brave_web_search` 回潮）；`authoritative-sync` 三份私有正文各追加 web_search 事实 |

**官方 schema 逐字段核对（2026-10-26，`curl -sS -L` 直读；未发任何真实计费请求）**：

来源两页：`https://api-docs.deepseek.com/api/search`（Search API Reference）与 `https://api-docs.deepseek.com/guides/search`（Search & Fetch 指南：错误 / 计费 / 限流）。站内 sitemap 未列这两页（`/api/search`、`/guides/search` 直接 200 且含 OpenAPI 渲染正文），核对以页面实际内容为准。

| 项 | 官方口径 | 本仓落地 |
| --- | --- | --- |
| 端点 / 认证 | `POST https://api.deepseek.com/v1/tools/search`；`Authorization: Bearer <key>` + `Content-Type: application/json` | 端点编译期常量；Bearer 每次调用经既有凭据解析面取；`Content-Type` 固定 |
| `query` | 必填；1–128 Unicode 字符；不得全为空白；运算符原样转发（`site:` / `-site:` / `-` / `-"..."` / `"..."` / `lang:` / `after:` / `before:`） | 逐字一致（工具不解析运算符） |
| `count` | 可选；1–50；缺省 10 | 一致；**超界在本地判 `invalid_input`、零外呼**（官方给 400，本地前置只是省一次外呼，语义不变） |
| `ori_question` / `localization` | 可选：原始问题（1–128）/ 本地化 IP（缺省 `""`）；未知字段被忽略 | **不发送**——工具面没有「原始问题」这一独立输入，本地化属部署偏好；请求体固定三键 |
| `need_content` | 可选；缺省 false；false 时 `content` 恒为空串 | 恒 false（正文走 Fetch 或 `http_get`） |
| 响应 `results[]` | `rerank_score`（0–3，≥1.5 高质量）/ `title` / `url` / `snippet`（UI 用，100–200 字）/ `summary`（模型用，600–800 字）/ `content` / `site_name`（可 null）/ `site_icon`（可 null）/ `date`（可 null，RFC 3339 UTC）/ `authority`（0–4，-1 未知） | 消费 7 个：`title` / `url` / `snippet` / `summary` / `date` / `rerank_score` / `authority`；`content` / `site_name` / `site_icon` 不消费 |
| 空结果 | 无命中返回**空数组**、HTTP 200 | 如实 `results: []`，不判失败、不改写、不重查 |
| 错误码与重试 | 400 请求头 / 体非法；401 凭据缺失或无效；402 余额不足；403（Fetch）被安全策略拒绝；404（Fetch）取不到可用内容；408 读请求体超时；413 请求体过大；429 QPS 超限；500 内部错误；504 下游超时。官方指引：408 / 429 / 5xx 等待后重试，402 充值后再试，其余 4xx 改请求再试 | 重发集合 = 408 / 429 / ≥500，最多 2 次；401 / 402 / 403 / 404 带固定指引且不重发；响应体一律不回显；413 走未分类的 `external_error`（只出状态码） |
| 计费 | $0.75 / 1000 次**成功**请求（Search 与 Fetch 同价）；仅 HTTP 200 且产结果才计费（Search 需非空 `results`），失败 / 空结果 / 超时不计费；`count` 不影响单价 | 金额不写进契约与正文（官方商业条款可变），运维手册只写「按官方计费口径」；空结果不计费这一条与「不重查」的取舍一致 |
| 限流 | 账户级：Search **100 QPS**、Fetch **25 QPS**，两者独立；Fetch 另有按域名 QPS；超额 429 | **设计稿 §2.5.4 的「5 QPS」与官方 100 QPS 不符**——不落实任何 QPS 常量，契约明写不声明数值，重试只按 429 退避 |
| 免计费期说明 | 经 DeepSeek Harness / Claude Code / Codex 的服务端工具调用 2026-11-01（北京时间）前免费 | 与本仓直连 API 无关，不写入任何文档 |

**核对后的三处取舍**：

1. **字段面比设计稿宽**：`ori_question` / `localization` 是设计时未记录的官方可选字段。本工具不发送（工具面没有独立的「原始问题」输入、本地化属部署偏好），请求体保持三键固定；官方明确忽略未知字段，不发也不违规。
2. **QPS 分歧不落码**：设计稿 5 QPS 与官方 100 QPS 冲突，按「限流以官方文档为准、契约不声明数值」处置。stage、代码、契约都没有 QPS 常量——写死任何一个都会在官方调价 / 调限后变成假事实。
3. **403 / 404 的实际语义**：官方错误表这两行是 **Fetch 专用**（安全策略拒绝 / 取不到可用内容）。对 Search，403 按「被官方安全策略拒绝」、404 按「接口路径不存在」给指引——是同一状态码在 Search 语境下最合理的解释，文案不冒充官方原文。

**T125–T129 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| T125-R1 `count` 超界在本地拒绝而非夹取 | 官方区间是 1–50，夹取会悄悄改变模型意图；本地判 `invalid_input` 还省一次外呼（零凭据解析、零审计行之外的开销） | `internal/tool/websearch.go` 的 `decodeWebSearchInput`；`TestWebSearch_InputValidation` |
| T126-R1 不校验模型连接是否 DeepSeek | 兼容代理（OpenAI 兼容端点转发到 DeepSeek）是合法部署；连接配错的症状是官方 401，如实上报即可，多一层厂商判断反而挡住合法部署 | `internal/app/search_credential.go:51-57`；`TestWebSearch_MissingCredentialFailsClosed` |
| T126-R2 判定先于凭据解析 | 被 `Policy` 拒绝时零解密、零外呼——拒绝路径不该先碰主密钥 | `internal/tool/websearch.go` `Execute` 的 ①→② 顺序；`TestWebSearch_PolicyDeniedNoSend` |
| T126-R3 每次调用解析一次凭据、重发复用同一个 Key | 重发是同一逻辑调用的重试，重新解密只会放大主密钥使用面；撤销 / 轮换在下一次调用生效 | `Execute` 的 ② 在重发循环之外；`TestWebSearch_Retry429Bounded` |
| T127-R1 脚本文案去 Brave 化、判定面零改动 | `industry_brief.py` 只改 docstring / argparse 帮助 / 错误文案里的「Brave」，`parse_search` 与 `--verify-report` 逻辑一字未动，24 只单测原样通过 | `py -3 -B -m unittest discover -s scripts` → `Ran 24 tests … OK` |
| T127-R2 工具 `Description` 改为渠道中立 | 「正文须用 `http_get` 或 Fetch 另行抓取」——工具面里哪个能抓就用哪个，工具描述不预设绑定了哪个 MCP | `internal/tool/websearch.go` 的 `Description`；无测试断言该字符串 |
| T127-R3 `examples/README.md` 保留 Brave 段但标注「无 Demo 使用」 | `funding-watch` 模板（tier B）仍是 Brave 配方；删掉会让模板失去凭据指引，留着不标注又像 Demo 依赖 | `examples/README.md`「Brave Search API key（可选；仅 `funding-watch` 模板）」 |
| T128-R1 重发不另计审计行，重发耗尽也只记一行 | 一次工具调用 = 一行审计；把重发拆行会让「工具调用次数」与预算计次对不上 | `TestWebSearch_CountsBudget` 两个子用例（重发后成功 / 重发耗尽失败） |
| T129-R1 `Compatibility.md` §5.1 样例 `AGENT.md` 保持 10 个工具 | 那份样例是**双平台互换样例**，声明 `web_search` 会让它在基线侧整体不注册，与样例用途矛盾 | `docs/Compatibility.md` §2.2 表后「表外新增」段 |
| T129-R2 文档同步靠新增 `web-search-sync` subtest 锁住，而不是只改字 | 只改文档的话，下一次实现或文档任一侧漂移都没有判据；subtest 把契约、实现、运维手册、示例目录四处字面量绑在一起 | `internal/cli/docs_14d_test.go` 的 `web-search-sync`；`check docs-consistency` |

**验证读数**（2026-10-26）：

- `check web-search`（T125）→ `passed fmt (exit=0)` / `passed web-search (exit=0)`，报告 `accept-20261026T104912.327808100`；同一检查另为 T126（`accept-20261026T104922.036619100`）与 T128（`accept-20261026T104927.934507600`）各跑一次，读数相同——三个任务共用一个 check id，勾选依据是同一份判据。
- `check docs-consistency`（T129）→ `passed fmt (exit=0)` / `passed docs-consistency (exit=0)`，报告 `accept-20261026T104842.744802000`；`^TestPublicDocs_|^TestDocs14D` 全绿，含新增的 `web-search-sync` 与本机在位执行（非跳过）的三份私有正文 subtest。
- `go test ./internal/tool -run '^TestWebSearch' -count=1` 覆盖 11 只顶层用例（7 只冻结名 + 4 只非冻结）；冻结名逐字命中 stage 的 `required_tests`。
- `py -3 -B -m unittest discover -s scripts` → `Ran 24 tests … OK`（T127 文案改动后原样通过）。
- 契约 §5.7 的「契约不声明 QPS 数值」一句原本跨行断开，`web-search-sync` 首次运行即红（`§5.7 is missing "不声明 QPS 数值"`）；改判据为「句子单行」而不是删断言——文档的可检索性本就是断言的一部分。

**未验证**：

- **真实一次搜索未执行**（DeepSeek 凭据未授权）：字段名、错误码、计费与限流全部来自官方文档页，未用真实 Key 打一次计费请求；报告不给 `web_search` 贴「真实实跑」标签。
- **设计稿 §2.5.4 的 5 QPS 与官方 100 QPS 冲突**留作 review ② 可见事实：本仓两边都不写死，只记录分歧。
- **E4 端到端（T116 `TestUS14D_FixtureEndToEnd`）仍未落地**：`builtin-agent` 红在缺这只用例，`web-search` 转绿不代表 D5 全链路有进程级判据（下一节记）。
- 文档层未跑 Markdown linter（同 §6.7 口径）；`docs/Compatibility.md` 与契约 §5.7 的长表格属 review ② 的阅读项。

### 6.9 P「T116 E4 落地：装出来的五只 Demo 端到端 + D5 进程级口径改判」（2026-10-26）

**交付物**：

| 文件 | 改动 |
| --- | --- |
| `internal/e2e/us14_builtin_test.go` | 新增 `TestUS14D_FixtureEndToEnd`（E4）。正文只有一句 `runDemoScenarios(t, newInstalledDemoFixture)`——五套剧本与断言与 `TestUS14Demos_Fixture` **同一份**，两处唯一变量是 Demo 的来源；文件头注释从「九个」改为「十个」 |
| `internal/e2e/us14_demos_test.go` | ① 新增 `installedDemoNames` 与 `(*demoFixture).requireInstallArtifacts`（安装清单五条 + `state=installed` + 五份 `model: ""` + docs-readonly 源码导出），在 `demo install` 之后、任何字段改写之前调用；② D5 剧本重写为内置 `web_search` + Fetch（§2.2 #24）；③ 夹具去掉 Brave 规格位：`specs` 三项、`env` 一项、`writeSpec` 一次、`writeMCPServers` 的 `brave-search` 行，以及 `demoBraveSpecEnv` 常量 |
| `specs/014-templates-demos-release/tasks.md` | 勾选 T111–T116、T127（依据同一个 `builtin-agent` 报告） |

**T116 E4 与 D5 就近裁决**（review ② 复核项）：

| 事项 | 裁决 | 依据 |
| --- | --- | --- |
| E4-R1 五套剧本只有一份实现 | `runDemoScenarios(t, demoFixtureFor)` 接两个构造器（`newDemoFixture` / `newInstalledDemoFixture`）；E4 判的是「装出来的那一份能跑」，不是另写一套判据 | 副本必然漂移；`demo-fixtures` 与 `builtin-agent` 两个检查因此判的是同一批断言 |
| E4-R2 安装器制品只在字段改写**之前**读一次 | 五份 `model: ""` 会被 `installAgent` 改成真实模型名；晚读只会读到夹具自己的改写 | `newDemoFixtureMode` 中 `demo install` 之后立刻 `requireInstallArtifacts` |
| E4-R3 清单断言「五条且 `state=installed`」 | 全新工作区；`kept` 属重装场景，不应出现 | `internal/cli/demo.go` 的 `demoManifest` / `builtin.StateInstalled` |
| E4-R4 不重复安装器单测 | `internal/cli/demo_test.go` 的 17 只判安装器**内部行为**（清单字段、空模型单行改写、技能不入 Agent 目录、不写凭据），`us14_demo_install_test.go` 判制品级构建；E4 判的是安装后的**运行**证据，重复断言只会让同一缺陷红两处 | 既有两只测试文件的覆盖面 |
| D5-R1 进程级只判失败路径 | 端点编译期常量、`http.proxy` 会因 TLS 校验失败、build tag 覆写会改变产物——进程面没有可替换的成功路径；成功侧由 `TestWebSearch_*` 与 24 只脚本单测承担，报告不给它贴「真实实跑」标签 | §6.8 官方核对 + `internal/tool/websearch.go` |
| D5-R2 禁止域名先于档位与端口 | `WhitelistPolicy.hostAuthorized` 与 `checkHTTPRequest` 都在档位判定**之前**先查禁止域名，同一断言在 strict / standard 两档都成立 | `internal/tool/policy.go` |
| D5-R3 失败也要落成可持久核对的审计行 | `core/tool_exec.go` 把失败写进 `result_json`（`content` = 错误文本），`runs.tool_calls` 无条件计数，故「一次失败调用」可从库里读回 | `internal/core/tool_exec.go`、`internal/core/loop.go:108` |

**验证读数**（2026-10-26）：

- `go test -tags=e2e -count=1 -v ./internal/e2e -run '^TestUS14D_FixtureEndToEnd$'` → PASS 92.76s，五个子用例 github 19.90s / repo_digest 17.79s / biz_daily 17.94s / docs_diff 18.44s / industry_brief 18.69s；`binary_source=temp`，`binary_sha256=ec70180755b32ade08f3e3bd1edf81a70eba539a0fdb67706ec09b01b9bf27bd`。
- `go run ./scripts/acceptance --stage harness/stages/us14-14d.json --task T116 --executor ai --run-kind rerun` → `passed fmt (exit=0)` / **`passed builtin-agent (exit=0)`**，报告 `accept-20261026T111035.193463300`，duration 210416 ms；10 条 `required_tests` 全部出现在 `passed_tests`（含 `TestUS14D_FixtureEndToEnd` 与五个子用例），`skipped_tests` 为空。
- 夹具移除 Brave 规格位后，`TestUS14Demos_Fixture` 仍全绿（91.67s，上一轮读数）——说明规格位不是其余四套剧本的隐含前置。
- `go vet -tags=e2e ./internal/e2e` 退出 0；`gofumpt -l internal/e2e/us14_builtin_test.go internal/e2e/us14_demos_test.go` 无输出。

**勾选状态**：T111–T116、T127 已勾。§6.4 G 末段「T111–T116 一律不勾」是当时读数，按只追加口径保留；以此段为准。

**未验证**：

- 真实一次检索仍未执行（DeepSeek 凭据未授权，同 §6.8）。
- `make e2e`（全量 `./internal/e2e`，含 `TestBootstrap*` 的 11 工具冒烟）本轮未复跑，安排在 T124 收口。
- 控制台页面（本机无 Node）与 WSL2 Linux 实机仍是人工项，同 §6.4 G。

### 6.10 Q「T124 14D 收口：全量 stage 首跑红态（lint / sec）与处置」（2026-10-26）

**首跑红态**（报告 `.loop/runs/accept-20261026T120956.435967700`，27 项检查）：`failed lint (exit=2)` 6 条 + `failed sec (exit=2)` 24 条，其余 25 项 passed。红态不是既有代码回退——是这批 14D 代码**第一次**走到全量 `lint` / `sec`（此前只在任务级 `--task` 跑过若干检查，`make lint` / `make sec` 未单独跑过）。

| 发现 | 位置 | 分类与处置 |
| --- | --- | --- |
| ST1005 ×5 | `internal/app/search_credential.go:70,74,78`、`internal/tool/websearch.go:252,255` | 五条错误文案以大写 `Agent` 加空格起头。逐条 grep 全仓确认**没有任何测试断言这五条文案**（`grep -rn "不在注册表中\|未声明 provider.name\|没有可用的已发布版本\|搜索凭据不可用\|搜索凭据解析为空" --include=*.go` 只命中定义处），改为中文短语起头、`Agent` 留在句中：`无法定位 Agent %s 的模型连接：不在注册表中` / `无法确定 Agent %s 的模型连接：未声明 provider.name` / `取不到 Agent %s 的模型连接 %s 的已发布版本` / `取不到 Agent %s 的搜索凭据` / `解析到空的搜索凭据：Agent %s` |
| U1000 ×1 | `internal/cli/demo.go:464-469` | `describeSampleDBNote` 是 CLI 侧包装，**无调用方**（`builtin.DescribeSampleDBNote` 由 `internal/builtin/sample.go:185` 直接调用，`builtin_test.go` 判卷）。**删除而非豁免**——死代码不该留豁免 |
| G101 ×18 | `web/readiness.go:57,60,61,62`、`web/instance_secrets.go:84-91`、`storage/mcp_credential_source_store.go:26`、`core/resource_admin.go:61`、`core/instance_secrets.go:52,54`、`app/mcp_env.go:32`、`app/mcp_admin_apply.go:33` | 全部是**变量名**命中规则（名字里含 `credential` / `Secret`），值分别是界面提示、SQL 列序、操作名、原因码、闸门分类、业务码，**无一是材料**。按仓库既有口径（`storage/credential_store.go:33`、`web/handlers_credentials.go`）逐条 `#nosec G101 -- <理由>`；两处成块的固定文案（`instance_secrets.go` 8 条、`readiness.go` 4 条）用块级指令覆盖同一份声明，理由写在块注释末行。改走重命名属于「躲扫描器」（开发操作 §7 陷阱 #56 明禁），不做 |
| G304 ×6 | `builtin/mcp.go:154`、`app/builtin_install.go:104`、`app/instance_identity.go:87`、`tool/program_dir.go:121`、`app/instance_admin_token.go:138,159` | 前四条是「工作区根 / 工作目录 / 程序目录推导出的固定文件名」：按规则的**本意修法**改成 `os.OpenRoot` 根句柄（`root.ReadFile` / `root.Open`），顺带把根外软链接挡住；`program_dir.go` 的哈希读取改为 `<程序目录>/runtime` 根句柄 + 清单相对路径 `rel`（该 `rel` 在第 68 行算出、第 72 行已与清单键逐字比对过），`hashProgram` 签名因此由 `(real, info)` 变为 `(real, rel, info)`。后两条（管理面密码文件的读与 `O_CREATE\|O_EXCL` 写）路径由实例私有目录推导、与主密钥读取同档，按 `credential_crypto.go:269` 的既有裁决写 `#nosec G304` 并逐条写明来源 |

**绿态读数**：

- `make lint` → exit 0（`go vet ./... && staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...` 零输出）。
- `make sec` → `govulncheck`: `No vulnerabilities found.`；`gosec` 汇总 `Files 269 / Lines 93355 / Nosec 39 / Issues 0`（本轮新增 10 条指令行；`gosec` 的 `Nosec` 计的是被抑制的告警数，不是评论行数）。
- 受影响七个包单测全绿：`tool` 9.952s / `builtin` 1.041s / `cli` 9.638s / `web` 6.015s / `core` 1.732s / `storage` 13.023s / `app` 39.964s。
- `go run ./scripts/acceptance --lint-stages --stage harness/stages/us14-14d.json` → `stage lint: ok`，exit 0；4 条 WARN 全属历史 stage（`us11-console-observability` / `us15-linux` ×2 / `us8-notify-r1` 的改名测试），按「只有当前 stage fail-closed」口径不阻断。

**全量 stage 绿态**（报告 `.loop/runs/accept-20261026T125328.674965300`）：

- `acceptance full/us14-14d-r1: passed exit=0`，**27/27 项 passed**；`base_commit=4641c7c`、`dirty=true`（14D 未提交，符合预期）、`fingerprint=51031911984c4960…`；`environment` = windows/amd64、go1.27.3；总耗时 1,767,396 ms（≈29 分 27 秒）。
- 单检查耗时（ms）：`cover` 792,297（最慢，含 `internal/storage` 五个新测试文件的判卷）/ `stage-lint` 474,283（对**每个** stage 的 `required_tests` 逐个 `go test -list`）/ `builtin-agent` 220,087 / `test` 134,580 / `test-integration` 53,204 / 其余均 < 11,000。
- 具名测试计数：`execution-profile` 117、`web-search` 47、`mcp-credential` 44、`builtin-agent` 24、`secrets-init` 21、`mcp-apply` 20、`admin-login` 18、`schema-v6` 17、`docs-consistency` 14、`policy-scope` 9、`contracts-14d` 2、`full-package` 2、`contract-yaml` 1；`skipped_tests` 合计 2 条，两条都在代码里写明理由：`TestExecutionProfile_ShellNameSymlink`（Windows 无软链接权限，`t.Skipf` 带平台原因）、`TestFullPackage_LinuxInstallSmoke`（`runtime.GOOS != "linux"` 时跳过并指向 Windows 半侧用例）。
- `console-test` / `console-build` passed（`PATH=/tmp/node24/node-v24.22.0-win-x64:$PATH`，本机便携 Node 24.22.0 仍在）。

**判据面（仅限本节 lint / sec 处置）：零变更**——本节没有删改任何测试、断言、阈值、`-run` 分支或 `required_tests` 条目。五条文案在改动前已确认无断言；`describeSampleDBNote` 无调用方；18 条 G101 与 2 条 G304 是抑制误报（理由逐条写在源码里）；4 条 G304 是把读取收窄到根句柄（`os.Root` 拒绝根外软链接，是**加强**而非放宽）。故本节不新增 §2.2 行。**边界**：这不覆盖收口期间其余判据面改动——e2e 适配、stage 追加三项检查、控制台顺序断言分别见 §2.2 #27–#31。

**未验证（截至本段）**：`make e2e` 全量复跑与独立复核见下段；`PATH` 前置仍是本机约定（不写进 stage，同 §2.2 #26 注）。

### 6.11 R「T124 14D 收口：30 项全量 stage 绿态、独立复核与未覆盖面」（2026-10-26）

**stage lint（T124 的第一道判据）**：`go run ./scripts/acceptance --lint-stages --stage harness/stages/us14-14d.json` → `stage lint: ok`，exit 0。20 项检查全部可加载；5 条 WARN 全属历史 stage（`us10-scheduler` 的 `010-app-tools` 改名测试、`us11-console-observability`、`us15-linux` ×2、`us8-notify-r1`），按「只有当前 stage fail-closed」口径不阻断。

**全量 stage 绿态**（报告 `.loop/runs/accept-20261026T135820.559606700`，`--executor ai --run-kind rerun`，21:58 起跑）：

- `acceptance full/us14-14d-r1: passed exit=0`，**30/30 项 passed**——10 项公共 profile（fmt / lint / sec / test / test-integration / cover / build / eval-build / behavior / injection）+ 20 项 stage（go-build / contract-yaml / schema-v6 / policy-scope / contracts-14d / secrets-init / admin-login / mcp-credential / mcp-apply / execution-profile / builtin-agent / full-package / web-search / console-test / console-build / docs-consistency / script-tests / template-quality / legacy-d1 / stage-lint）。
- `base_commit=4641c7c`、`dirty=true`（14D 未提交，符合预期）、`fingerprint=f48fab0234f3eb5a…`；`environment` = windows/amd64、go1.27.3；`started_at=2026-10-26T13:58:22Z`；检查耗时合计 1,787,939 ms（≈29 分 48 秒）。
- 最慢五项：`cover` 796,243 ms / `stage-lint` 471,479 ms / `builtin-agent` 214,464 ms / `test` 127,271 ms / `test-integration` 52,138 ms；其余均 < 13,000 ms。
- 具名测试：execution-profile 117、web-search 47、mcp-credential 44、builtin-agent 24、secrets-init 21、template-quality 21、mcp-apply 20、admin-login 18、schema-v6 17、docs-consistency 14、policy-scope 9、contracts-14d 2、full-package 2、contract-yaml 1、legacy-d1 1。`skipped_tests` 恰 2 条，两条都在代码里写明理由：`TestExecutionProfile_ShellNameSymlink`（Windows 无软链接权限）、`TestFullPackage_LinuxInstallSmoke`（非 linux 平台）。
- 新增三项检查的读数：`script-tests` = `py -3 scripts/run-example-tests.py` 五目录 90 用例全 PASS（9 / 13 / 22 / 22 / 24）；`template-quality` 21 条具名测试（`TestTemplates_*` 与 `TestMCPEgress_Documentation`）；`legacy-d1` 1 条。
- `full-package`：`binary_bytes=30721536 runtime_bytes=4667904 raw_bytes=35389440 archive_bytes=19520878 files=2 ratio=0.55`（与 12:53 报告同一二进制；压缩包差 8 字节属 zip 时间戳，非代码变化）。
- `console-test` / `console-build` passed（`PATH=/tmp/node24/node-v24.22.0-win-x64:$PATH`，本机便携 Node 24.22.0）。

**`make e2e` 全量复跑（候选二进制，2026-10-26）**：

- 命令 `make e2e`（依赖 `build` + `example-tests`）：`BASALT_E2E_BINARY=<repo>/bin/basalt.exe go test -json -tags=e2e -count=1 ./internal/e2e`。
- `ok github.com/ymedword/basalt/internal/e2e **360.375s**`，**44 只顶层用例 passed、0 failed、2 skipped**。两条 skip 均非判据缺口：`TestMCPHelperProcess`（子进程助手，本就不是独立用例）、`TestFullPackage_LinuxInstallSmoke`（非 linux 平台，T118 人工项）。
- `binary_source=candidate`、`binary_sha256=e8fdd4a678c3182111f5f79d752f96f62e8925268915d34e2d9a0cd8f7dc5726`（`bin/basalt.exe`，21,646,336 B，`basalt version 4641c7c`，stamp `v0.0.0-20261025220807-4641c7ce1401+dirty`——**未提交，不构成发布候选**）。
- 前置同轮 passed：`go test ./examples/shared/mcp/docs-readonly` 与 `py -3 scripts/run-example-tests.py`（五目录 90 用例）。
- 与上一轮（发现 10 处红态）的关系：六处判据面适配 + 一处文档回退 + 一处哨兵撞词全部处置后，**同一套全量 e2e 0 failed**。

**本轮 e2e 红态与处置**（判据面逐条见 §2.2 #27–#31）：

- 六处判据面适配：E3 启动隔离（`mcp_test.go`）、G1/G2 档位置 `strict`（`notify_test.go` ×2、`toolsandbox_test.go` ×2）、schema v6 / 33 张表与第 11 件内置工具（`bootstrap_test.go`、`v3_test.go`、`web_test.go`）、旧 D1 第三档改判（`us14_demos_test.go`）。
- 一处真实文档回退（不是测试问题）：`examples/daily/industry-brief/README.md` 的标题 `## 出网判定（…)` 被 `TestMCPEgress_Documentation` 判红——该测试要求 §2.2 口径的「六行走廊」标题用「边界」而非「判定」；改为 `## 出网边界（…)` 后通过。这是判据抓到文档与本轮口径漂移的实例，不是放宽测试。
- `testToolSandboxPortRedaction` 的裸词哨兵 `secret` 与 14D 新增日志字段 `step=secrets` 撞词，改为唯一串 `port-e2e-secret-4d1c`（判据加强，§2.2 #31）。

**独立复核（review ② 复核项的取证，非人工 review ②）**：

| 复核 | 结论 | 处置 |
| --- | --- | --- |
| A（判据面 / 覆盖） | 1 条 HIGH + 1 条 LOW。HIGH：D5 成功路径从进程级移出后，14D stage 没有机器锚点能证明「产业简报 24 只业务计数用例、出网边界文档、旧 D1 回环走查」仍被判卷 | 已处置：stage 追加 `script-tests` / `template-quality` / `legacy-d1`（§2.2 #32），本轮 30 项全绿即这三项的机器锚点 |
| A（同轮 LOW） | 控制台 `sandbox-allowlist.test.ts` 的顺序断言在 14D 期间退化为计数式 | 已处置：两处回补为 `method + path` 精确有序断言（§2.2 #31），`vitest` 4/4 通过 |
| B（安全 / 路径审计） | **无 CRITICAL / HIGH / MEDIUM**：全仓无 PAT 模式；任务指定的演示口令字面量 0 命中（含忽略目录与二进制）；新增路径面一律 `os.Root`；44 条 `#nosec`（新增 17、删除 4，基线 31）逐条有理由；示例 / 模板数据无真实 UID 或明文凭据 | 2 条 LOW：① `run-state.md` 的 WSL 读数带本机用户名与绝对路径 → **已脱敏**（§6.4 I 统一写 `<user>` / `<repo>`）；② 新增可提交文件引用私有文档路径（stage 文件、`us14-release-gate-r5.json`、`internal/cli/docs_14d_test.go`、`specs/**`、`constitution.md:23`）→ **待用户统一裁决**，基线已有同类先例，不宜只在 14D 单独改 |

**未覆盖面（14D stage **不**判的，逐项列出，不冒充通过）**：

1. 真实外部服务：DeepSeek Search 真实检索、GitHub API、Brave、Fetch、Playwright、DBHub 真实包、真实通知接收方——全部未授权，仅夹具。
2. `internal/e2e` 全量：backfill / notify / toolsandbox / bootstrap / web / v3 等用例不在 14D stage 的 `-run` 面内，由 `make e2e` 与 US-14 release stage 的 `e2e` 检查承担（本节 `make e2e` 段 44 只顶层用例全 passed）。
3. Linux 平台：WSL2 读数（§6.4 I，本轮已重跑并落 `.loop/runs/us14-14d-t118-linux/linux-install.log`），仍不算 14D stage 证据——stage 只在 Windows 执行。
4. 控制台人工走查（US-14 §4.7 M1–M8）、五 Demo 的自动触发 / 每日计划现场核对、八模板夹具复跑、依赖与许可表、双平台走查、`v0.1.0` tag——人工项或用户项。
5. 真实模型端到端：全部模型交互走假模型服务器。

**US-14 review ② 与 14D 证据的分界**：本节与 §6.4–§6.10 是 **14D 增量**证据（stage `us14-14d-r1`）；US-14 review ② 的证据是 release stage `us14-release-r1` 的全量七场景、`make acceptance`、干净检出与双平台走查记录（§4 / §5 已登记）。二者**分开记录**，本节不替代 review ② 的任何一项。

**收口物与演示实例（review ② 用）**：

- 候选二进制：`bin/basalt.exe` 与 `bin/basalt-demo.exe` 现在是**同一文件**（SHA-256 `e8fdd4a678c3182111f5f79d752f96f62e8925268915d34e2d9a0cd8f7dc5726`，21,646,336 B，`basalt version 4641c7c`）——即 `make e2e` 判卷的那一份；二者都带 `+dirty` 戳（14D 未提交），**不是发布候选**。
- 演示实例已用该二进制启动：`127.0.0.1:8080` LISTENING（工作区 `D:\code\basalt-demo`，配置 / 密码在仓库外，`run-demo.sh` 从 `.env.local` 读入）。启动读数：`agents=5`、五只 Demo 均在册但**未注册**（`provider not registered: local-model`——模型归用户配置，不是缺陷）、内置工具 11 件、`mcp_connected=0`；`execution profile loaded from database; profile=strict source=migration`——该库是存量实例，G1/G2 的「存量即严格档」在真实库上可见。
- `assets/console/dist` 在 stage 的 `console-build` 中重建（22:19，新产物 `assets/index-Bs2SzWxe.css` / `assets/index-BuQHQzBe.js`，旧两份删除，`index.html` / `integrity.json` 更新）；**未提交**，提交时机归用户。
- 临时物清理：`basalt-base` 临时 worktree 已 `git worktree remove`；本轮日志固化到 `.loop/runs/us14-14d-r1/`（`stage-console.log` / `e2e-candidate.log`）与 `.loop/runs/us14-14d-t118-linux/linux-install.log`；`/tmp` 下本轮探针与日志已删；便携 Node 24.22.0（`/tmp/node24`）**保留**——`console-test` / `console-build` 依赖它，属本机工具而非本轮临时物。

## 7. review ② 退回后的整改（2026-10-27 起，只追加）

### 7.0 依据与边界

用户 2026-10-27 的退回意见分四段（三处裁决 / 执行环境页整改 / 8080 实例三个阻塞缺陷 / 完整包交付）加执行与验收要求；整改并入 US-14 14D，**不新增 US、不重开已裁决方向、不进入 review ②**。任务判据口径不变：缺陷修复必须给出「原因 + 改动位置」「测试 + 真实实例验证」「源码 commit / 制品哈希 / run_id」「未验证项」四类证据，并分开记录「代码存在 / 配置已写入 / 连接成功 / 业务运行成功」。提交只做本地检查点，**不 push / 不合并 / 不打 tag**；凭据、运行库、本机环境与私有正文不进提交。

### 7.1 阻塞缺陷 1：存量实例的网页加密初始化（已验）

**原因**：存量演示库从未做过主密钥初始化（`instance_secrets` 空），网页保存 API Key 走加密写入时缺少可用密钥；旧流程要求先手工走初始化，普通用户「填 Key → 保存」走不通。

**改动（代码在 14D 既有提交 + 本轮工作区）**：保存模型凭据时折叠初始化——仅在「可证明从未托管」的状态下自动生成主密钥，随后同一次请求内加密写入；已有密文的实例不重新生成（宪法架构约束第 6 条口径，版本 4.0.0 已按用户 2026-10-26 裁决修订）。

**真实实例证据**（`D:\code\basalt-demo`，同一进程，读取 `basalt.db`）：

| 读数 | 值 |
| --- | --- |
| `instance_secrets` | `scope=master_key`、`key_id=k_KJR2BH7VMETM16MYVXD15APSTD`、`fingerprint=96009e84a6b5d627…`、`initialized_at=1793029276739`（2026-10-26 23:41:16.739 +08） |
| 初始化操作 | `op-1793029276724-a5ahvz` / `system.secrets_init` / `success` / `OK`，`actor_subject=token:…`（管理认证） |
| 凭据写入操作 | `op-1793029276745-86msq4` / `model_connection.credential.write` / `success`，同一 `key_id`（加密生效），`credential_versions` 仅有密文与非空 nonce，无明文列 |
| 连接测试 | `op-1793029287632-wfxym0` / `model_connection.test` / `success`，`resource_connection_tests.detail=模型调用成功（424 ms）`（2026-10-26 23:41:28） |

**结论**：「存量实例 → 网页填写 API Key → 点击保存 → 系统完成必要初始化并加密 → 同进程连接测试成功」在本机真实库上成立；凭据值未落盘、未出现在日志与本记录。未覆盖面：企业级主密钥轮换 / 多节点共享主密钥不在本 US。

### 7.2 阻塞缺陷 2：GitHub PAT 与 MCP 凭据通路（已验）

**原因**：演示实例的 `github` MCP 需要 `GITHUB_PERSONAL_ACCESS_TOKEN`，原先没有把进程环境里的 `GITHUB_TOKEN` 映射到该键；`docs-readonly` / `dbhub` / `fetch` 三个系统自带服务需要不依赖任何用户凭据即可准备。

**改动**：MCP 凭据来源登记走管理面 `mcp_credential_sources`（`mode=env`，把 `GITHUB_TOKEN` 映射到 `GITHUB_PERSONAL_ACCESS_TOKEN`）；系统自带三项由启动基线自动准备，不要求用户填任何密钥。**凭据值只在实例环境的 `.env.local`（仓库外）**，仓库、示例、测试与本文均无该值。

**真实实例证据**：

| 读数 | 值 |
| --- | --- |
| 基线重写 | `op-1793030025017-2rfyli` / `system.baseline_v2` / `success`（2026-10-26 23:53:45） |
| 凭据来源 | `mcp_credential_sources`：`server_uid=rs_1N9D4M0YA0P9PM51QH7581FG57`、`binding_key=GITHUB_PERSONAL_ACCESS_TOKEN`、`mode=env`、`revision=1` |
| 首次批量测试 | 23:54:12 四条 `mcp_server.test` 全部 `ok=false` / `failure_class=policy_denied` / `stage=preflight`（当时执行档位未放行 MCP 启动，属预期红态，照实保留） |
| 放行后批量测试 | 23:55:17–23:55:22 四条全部 `success` + `ok=true` + `stage=complete`：`docs-readonly` 7 ms / 3 工具（`list_documents`、`read_document`、`document_metadata`）、`dbhub` 597 ms / `execute_sql`、`fetch` 872 ms / `fetch`、`github` 111 ms / 27 工具 |
| 进程启动读数 | `serve-r2-20261027.log`（2026-10-27 00:52:32 起）：`providers=1 mcp=4 agents=5`；四台 MCP 全部 `decision=allow` 并连接（docs-readonly=3、dbhub=1、fetch=1、github=27 件工具）；`profile=standard source=database revision=4` |

**结论**：「代码存在 / 配置已写入 / 连接成功 / 工具发现」四档分开：MCP 测试与启动日志证明「连接成功 + 工具发现」；GitHub 工具的真实业务调用仍未在本轮证明（D1 未跑）。未覆盖面：GitHub API 侧的真实读写（需用户授权范围）不在本轮。

### 7.3 阻塞缺陷 3：D5 `industry-brief` 真实检索与运行时钟（已验）

**改动**：演示实例的 `industry-brief` 定义由旧 Brave 方案点迁到内置 `web_search` + Fetch（Agent 根目录备份旧定义后迁移，保留其余配置与业务状态）；`AGENT.md` 增加「每一轮都是全新简报，不得复用历史 `search.json` / `pages.json` / `--now`」；脚本把运行时钟收归自己——省略 `--now` 时只采一次本机时钟（秒级），`runs/<t0>/` 与 `observed_at` 同源，显式 `--now` 晚于本机时钟 5 分钟以上直接拒收并提示删掉重跑。**动机是实测**：模型曾在 00:52 的手动补跑里自行编造 `--now 03:20:00`，把「访问时间」写成未发生的时间点；更早的 00:42 全量轮（`d11gzj`）也被确认产物目录/`observed_at` 为 `02:20:00`（同样是编造值）。

**真实实例证据（干净轮，`scheduler-1793033843142-lzne2c`，手动补跑）**：

- `trigger=manual`、`status=completed`、`iterations=14`、`tool_calls=22`，2026-10-26T16:57:23Z → 17:00:05Z（00:57:23 → 01:00:05 +08）。
- 工具行：`web_search` ×3（`count=5`，三条查询各不相同）→ `write_file search.json` → `fetch` ×10 全成功 → `write_file pages.json` → `shell` ×4 → `read_file brief.json` → `write_file brief.md` → `shell`（引用自检）。
- **时钟强制的完整往返**：`tool_invocations` id 295 模型仍传 `--now 2026-10-27T03:50:00+08:00`，脚本退出码 1、`stderr` =「--now 晚于本机时钟约 171 分钟：运行时钟不能是未来时间；删掉 --now 重跑…」；id 296 去掉 `--now` 重跑但输入不合规（`pages 第 5 项的 status 必须是整数`）；模型按提示改 `pages.json` 后 id 298 成功，`observed_at=2026-10-27T00:59:24+08:00`（与真实跑数时间一致）；id 301 以 `--now 00:59:24` 做引用自检，退出码 0，同一份 `brief.json` 重写后哈希不变。
- 产物与哈希（`/tmp/us14-r2-evidence/lzne2c/` 快照）：`search.json` `6b923e4a2aad592ee928ef36c41ca3ba5e20c5ba5aec48bd527a20c309c272fa`；`pages.json` `d00e0aa74385bb2d34e50b981635c43f401d265939ab6317d744b6b30c93b41f`；`runs/20261027T005924/brief.json` `76049a811277225f4a968dc2f66ddf7f9f322463c0a94acf8ccc3e97c2a7fbdf`；`brief.md` `cfe23f4e67fd6f688e0a57ae934c11b205ebab7e5d0b3afa25caaaa4b113096c`。快照三份哈希与脚本 `stdout` 的 `input.search.sha256` / `input.pages.sha256` / `report.sha256` 逐字节一致。
- 业务读数：`counts.queries=3`、`results=14`、`duplicates=0`、`sources=14`、`fetched=10`、`leads=4`、`fetch_failures=0`、`missing_dates=3`；来源表 10 行 + 线索表 4 行 = 14，与 `sources` 守恒；`citable_urls=10`。
- 诚实性读数：36氪中文页与 Kagi News 页抓到的是反爬提示 / 站点导航（正文非空，脚本按 fetched 计），模型**未**将其写入结论、在「失败」段如实说明；未抓正文的 4 个 URL 只进线索表。

**结论**：「检索 → 真实抓取 → 脚本算来源表 → 只看已抓正文写结论 → 引用自检」在本机真实实例上跑通，且运行时钟不再由模型推测。未覆盖面：`deliver_to` 送达需要真实通知渠道（用户配置）；自动触发轮的时钟取证仍以 `d11gzj` 的产物为历史（该轮 `observed_at` 为编造值，正是本轮修复的动机，不作为干净证据）。

### 7.4 手动补跑的时限缺陷（已修）

**原因**：`POST /schedules/{id}/run` 复用了 Web 会话的 60 秒 `RunTimeout`，而定时执行链的预算是 300 秒（`scheduleRunTimeout`）。真实业务轮（14 次迭代、22 次工具调用）远超 60 秒，手动补跑会被 504 截断——恰好是 review ② 人工点「立即运行」时要用的入口。

**改动**：`internal/web/server.go` 增加 `Options.ScheduleRunTimeout`（缺省 300 s）并让 `writeTimeout = read + max(RunTimeout, ScheduleRunTimeout) + margin`（60+300+10=370 s）；`internal/web/handlers_schedules.go` 的手动补跑按该预算取 context，504 文案点名实际生效秒数；`internal/app/web.go` 与定时触发共用 `scheduleRunTimeout` 常量；`docs/contracts/run-and-errors.md` 的 RunTimeout 行改为「60 秒（REST 单次调用）/ 300 秒（定时任务与手动补跑）」。测试同步：`server_test.go` 断言缺省值与 370 s 写预算、`handlers_schedules_test.go` 两个超时子测试改用显式小预算。

**验证**：`go test ./internal/web -run 'TestServer_|TestSchedules_RunTimeout'` 通过（见 7.8 汇总）；真实实例 `lzne2c` 手动补跑 162 秒完成，未被截断。

**实例副作用登记**：为取证曾在演示实例把该任务的 cron 临时改为每 2 分钟，期间产生了 00:04–00:46 的一批定时轮（含 `policy_denied` 与模型编造时钟的轮次）；cron 已恢复 `0 30 9 * * *`（热加载日志 `next=2026-10-27T09:30:00.000+08:00`），演示库与配置未重置。

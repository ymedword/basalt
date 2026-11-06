# US22 收口复跑一致性分析

2026-11-06；只读上下文对已审模块卡 R6（`docs/us/us22-controlled-evolution.md`，SHA-256
`289b5e70712974002d9addd9ab63893144ff7c6de332de73d6a1dd95c41ddfe9`，2039 行）、
`spec.md`（`06c5e121…`）/ `plan.md`（`3415e3c9…`）/ `tasks.md`（勾选位归一
`8753916…`）、实现源码与契约重新比对；不把 `run-state.md` 当证据，不运行测试
（绿态由 stage 与七项证据承载）。实施前 analyze（2026-11-06，CRITICAL 0）见
[run-state](run-state.md) §1.2。

分析时任务状态：34 个任务，32 已勾选；T032（全量门禁）与 T033（收口三件套）在本报告
落盘时仍在执行。

## 1. Findings 与处置

| ID | 类别 | 严重度 | 位置 | 发现 | 处置（2026-11-06） |
| --- | --- | --- | --- | --- | --- |
| F-01 | plan ↔ 实现（文件落位） | MEDIUM | plan.md:119；`internal/web/server.go` `routeTable()`；`internal/web/contracts_learning_test.go` | plan 列 `internal/web/admin_routes.go` 与 `contract_test.go` 为改动文件；实际两条落点均未改：五条只读路由注册在 `server.go` 的路由表（US-21 只读面先例，管理面 67 行写清单不含只读模块面），契约测试落在新文件 `contracts_learning_test.go` | **收口登记**：T026 已记录（`tasks.md` 正文不改写）；判据面不变，`us22-base-web` 的 11 个必跑用例全部经 `server.go` 路由表 → handler 生效 |
| F-02 | plan ↔ 实现（文件删除） | LOW | plan.md:111；`git status` → `D internal/app/admission_secret.go`；`internal/core/secret_scan.go` | plan 写 `admission_secret.go 改造：委托 core 扫描器`；实际按 T007 把扫描器**下沉**到 `core` 后删除该文件（app 侧调用直接改 `core.*`） | **收口登记**：T007 已记录；US-21 凭证回归与 `TestUS22_SecretScanner_SharedWithAdmission` 通过，规则仍只有一份 |
| F-03 | plan ↔ 实现（未改文件） | LOW | plan.md:113；`internal/app/run.go` 未在 `git status` 改动面 | plan 列 `resources.go / run.go / web.go 既有启动兜底接线`；实际接线落在 `wire.go` 与 `web.go`，`run.go` 无需改动 | **收口登记**：无障碍；启动恢复与装配由 `wire.go` 单点承载，T024 启动次序用例覆盖 |
| F-04 | plan ↔ 契约文档清单 | LOW | plan.md:128；`docs/contracts/schema.pg.sql`（`git status` 零改动） | plan 的文档交付行把 `schema.pg.sql` 与增量契约并列；模块卡 §2.4.5 与 T029 DoD 明确冻结基线**不改写** | **以卡为准**：实现未改 `schema.pg.sql`（已核对），plan 行按「提及文件」读；不改已审卡 |
| F-05 | plan ↔ 文档清单遗漏 | LOW | plan.md:129；`docs/Compatibility.md`（`git status` 不可见，属 `.git/info/exclude` 私有正文） | plan 的文档交付行只列 TechnicalDesign / Requirements / Operations；T030 还按任务正文更新了 `docs/Compatibility.md`（US-22 兼容增量） | **收口登记**：T030 已落地并实测（§757 `US-22 受控自我进化兼容增量`），属任务正文要求，plan 清单为摘要遗漏 |
| F-06 | tasks 顺序 | LOW | tasks.md:76（T034 位于 T033 之后） | T034 为 T024 阶段检查发现的派生补遗，按追加式修订落在 Phase 10 末行；文档序上排在收口任务 T033 之后 | **无功能影响**：T034 已完成并勾选；续跑规则取「文档序首个未勾选」= T032，不会误取 T033 之后的任务 |

严重度计数：**CRITICAL 0 / HIGH 0 / MEDIUM 1（F-01，已登记且判据面不变）/ LOW 5**。
无未处置 blocking 项；F-01 的落点差异在验收报告变更总结中复述。

## 2. FR 覆盖与测试存在性

13 条 FR 均有承载任务与已存在的具名测试；stage `us22-base.json` 的 `required_tests`
共 207 条（200 个唯一名，7 个并发 / 泄漏用例同时列在功能 check 与 race check），
逐名静态核对 `internal/` 中 `func TestUS22_*` 定义：**missing = 0**。

| FR | 承载任务 | 具名测试（节选） |
| --- | --- | --- |
| FR-22-01 默认关闭 / 名单校验 | T011、T013、T023、T031 | `Disabled_NoWritesNoCalls`、`Config_DefaultsOff` / `_EnabledRequiresAgents` / `_DuplicateRejected` / `_UnknownAgentWarn`、`E2E_DisabledNoSideEffects` |
| FR-22-02 唯一同步触发点与生命周期顺序 | T004、T005、T006、T012、T031 | `Hook_OnlyAfterTerminalApplied`、`Hook_OncePerRun`、`FinishRunKeepsTerminalWhenReleaseFails`（SQLite / PG）、`Extractor_DecoratedReachable`、`Router_NotExtractor`、`E2E_ExtractConfirmRecall` |
| FR-22-03 有界窗口与冻结 | T009、T011、T016、T017 | `Window_FirstAttemptFreezes`、`Window_FrozenSameOriginOnly`、`Store_MessagesSameOrigin`、`Store_MessagesForFrozenWindow`、`Window_PartialGoneSkips` / `_FullGoneSkipsAndAdvances` |
| FR-22-04 两级预写与尝试身份 | T009、T010、T018 | `Audit_ExtractionPendingBeforeCall`、`Audit_LLMPendingBeforeCall`、`Audit_PreWriteFailureNoCall`、`Retry_AttemptBindsRunModel`、`Store_RetryUpdatesAttemptIdentity` |
| FR-22-05 不可信输出校验 | T008、T011、T021 | `Parse_StrictRejectsUnknownAndTrailing`、`Validate_*`（证据逐字 / 角色 / `valid_to` / 去重 / 来源子集 / 凭证丢弃）、`Method_DraftNotMemory` |
| FR-22-06 候选落库与不可见 | T011、T013 | `Candidate_InsertedAsCandidate`、`Source_IsAutoExtract`、`Scope_AgentOnly` / `_NoGlobalWrite`、`Candidate_NotRecalledBeforeConfirm` / `_NotInjected`、`Compat_SaveMemoryStaysActive` |
| FR-22-07 确认门禁与并发 | T014、T015 | `Confirm_BodyOptional`、`Confirm_ExpiredRejected`、`Confirm_OriginRevokedRejected` / `_KeyExpiredRejected` / `_CallerDisabledRejected`、`Confirm_ConcurrentOneWinner` / `_ConcurrentRevokeFailsClosed`、`Confirm_ResolutionReceipt` |
| FR-22-08 草案只读与 handoff | T021、T022、T023 | `Draft_FixedLayout` / `_ReadOnly` / `_NotAgentReadable` / `_NoScripts`、`Handoff_TakesLock` / `_ForcesSourceFields` / `_RequiresUserConfirmed` / `_CallsProposeOnly`、`CLI_Handoff*` |
| FR-22-09 恢复只查证 | T024、T025 | `Recovery_CandidatesMeanSuccess` / `_DraftsMeanSuccess` / `_UnknownAdvancesNoReplay` / `_UnknownNotRewritten` / `_RecoveredUnknownUnblocks`、`Unknown_NoRestartNextRunNoReplay`、`Store_RecoverSetsVerifiedAt` |
| FR-22-10 预算 / 重试 / 失败分类 / 时延 | T009、T016、T018 | `Budget_ExhaustedNoCallNoRow`、`Store_BudgetAtomic`、`Retry_*`、`Failure_TimeoutRecordedAfterDeadline`、`Latency_TotalBudget` / `_SlowModelAuditSuccess` / `_WorkDeadlineFailureStillAudited` / `_AuditPreWriteCrossDeadline`、`Hook_BoundedAndNoGoroutineLeak` |
| FR-22-11 只读查询面 | T023、T026、T027、T028 | `LearningStatus_ReadOnly`、`LearningExtractions_ListAndDetail`、`LearningDrafts_ListAndDetail`、`NoWriteEndpoints`、`CLI_Status` / `_ListShow`、`Console_MemoryTabs` / `_LearningRecordsReadOnly` / `_DeepLinksPreserveAgent` |
| FR-22-12 迁移只追加 | T003、T029、T030 | `Migration_SourcePurposeExtended` / `_FailureClassWindowGone` / `_ForwardFromV10` / `_CandidateSourcesTable`、`TestPostgres_FreshSchemaIsV1`（表数 87 / 版本 11）、`Contracts_NoNewActionLiterals` |
| FR-22-13 召回派生复核 | T019、T020 | `Recall_DerivedSourceDeleted_Keyword` / `_LegacyKeyword` / `_Semantic`、`Recall_DerivedChainTransitive`、`Recall_DerivedSourceExpired`、`Recall_DepthLimitFailsClosed`、`Recall_ExplicitEntryUnaffected`、`Recall_ToolKeywordStopsDerived` |

## 3. Success Criteria 覆盖

| SC | 承载任务 | 说明 |
| --- | --- | --- |
| SC-001 完整链路可重复 + 每动作有审计 | T011–T015、T018、T031 | E2E 六用例走真实二进制；审计序由 `Audit_*` 三用例断言 |
| SC-002 拒绝错误候选不召回 | T014、T031 | `E2E_RejectWrongCandidate` + 不产生 `active` 行 |
| SC-003 来源失效停止召回 | T019、T020、T031 | 四链路（关键词真实入口 / 旧入口 / 语义 / 工具入口）+ 传递链 |
| SC-004 确认门禁与并发 | T014、T015 | 21 个 `Confirm_*` + 并发单赢家 / 撤权竞态 |
| SC-005 审计 fail-closed 与恢复 | T018、T024、T025 | 预写失败零调用；恢复只 success / unknown、不重放 |
| SC-006 时延有界 | T018 | 20s = 13s + 5s + 2s 分段；慢模型与工作段耗尽两场景 |
| SC-007 默认关闭零副作用 / 零新坐标 | T011、T031、T032 | 零写入零调用；`go.mod` / `go.sum` / `package.json` 零 diff（T032 实测） |
| SC-008 迁移前向升级 | T003 | 空库与 v10 旧库各一次；既有迁移测试保留 |
| SC-009 只读面可验证 | T026、T027、T028 | 五 GET + CLI 只读；写方法 404；存储错误 500 不空列表 |
| SC-010 效果类结论 `未测量` | T033 | 本报告与验收报告只断言确定性行为，不转写效果提升 |

SC-010 不派生构建任务属设计使然（它是报告纪律，不是可交付能力），由 T033 的收口
口径承载。

## 4. 未映射任务

| 任务 | 说明 |
| --- | --- |
| T001、T002 | 流程与 stage 自举（run-state 基线 / 12 check 派生），不产产品代码，无 required_test 归属 |
| T030 | 文档修订（私有正文与契约叙述），由 markdownlint 与文档比对承载，映射「—」 |
| T033 | 收口三件套（本报告、独立复核、七项证据），映射「—」 |
| T029 | 契约叙述与 `memory.confirm` 明细键；check 映射 `us22-base-web`（`OpenAPIContract` / `NoNewActionLiterals`） |

其余 27 个任务全部至少映射一个含 `TestUS22_*` 的 check，与 `tasks.md` 末表一致。

## 5. 宪法对齐

| 原则 | 本次落实证据 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct | `go.mod` / `go.sum` 零 diff；抽取带空 `Tools`，不执行脚本 | 通过 |
| II 只做协议调用 / 唯一消息仓储 | `ChatMemoryExtract` 一次协议调用；`MessagesForExtraction` / `MessagesForFrozenWindow` 只读；`TestUS22_Messages_ReadOnly` 断言抽取前后消息逐字节不变 | 通过 |
| III Provider 显式映射 | 抽取取 Run 冻结视图（`Pinned()` 携带预写面）；路由器不实现 `MemoryExtractor`（注释 + 断言），两个生产装饰器实现 | 通过 |
| IV 一目录一 Agent / 技能声明式 | 草案固定布局、不进白名单（`Draft_NotAgentReadable`）；不新增 `AGENT.md` 字段 | 通过 |
| V 审计 day-one fail-closed | 抽取行 `pending` → `llm_calls` `pending`（`round=0`）→ 调用；预写失败零调用；确认先写 `memory.confirm` `pending` | 通过 |
| VI 授权与执行分离 | 不触碰 `Policy` / `CommandRunner`；handoff 只调 `AdmissionAdmin.Propose` | 通过 |
| VII 同步 + 有界后台例外 | 单钩子、无 goroutine、无 Worker / 持久任务；`Hook_BoundedAndNoGoroutineLeak`；`context.WithValue` 未新增业务状态 | 通过 |
| VIII 单二进制 | 零新增 Go / npm 坐标；`CGO_ENABLED=0` 由 T032 复验；`-race` 独立命令 | 通过（构建待 T032 读数） |
| IX 手写 SQL / 迁移只追加 | `0011_memory_learning.sql` 新增，0001–0010 与 `schema.pg.sql` 零改写；`SupportedSchemaVersion=11` | 通过 |
| X 范围卡紧 | 无后台 Worker / 管理台开关 / 自动晋升 / 新写端点；效果实验归 D-2（拟） | 通过 |

## 6. 总结论

**CRITICAL 0、HIGH 0**，可进入收口；F-01（MEDIUM）已在 T026 记录且判据面不变，
其余 5 条 LOW 均属 plan / tasks 文件清单与实现落点的措辞差异，不改写已勾选任务
正文与已审模块卡。实现与 spec 的 13 条 FR、10 条 SC 逐条可追溯，stage 必跑测试
200 个唯一名零缺失。

## 7. 指标

| 项 | 值 |
| --- | --- |
| 需求总数 | 23（13 FR + 10 SC） |
| 任务总数 | 34 |
| 覆盖率（FR / SC 有承载任务） | 13/13、10/10 |
| stage check | 12 |
| `required_tests` | 本报告快照 207 条 / 200 唯一名；收口复跑实测 217 条 / 210 唯一名（见 §8）；定义缺失 0 |
| CRITICAL / HIGH / MEDIUM / LOW | 0 / 0 / 1 / 5 |
| 歧义 / 重复计数 | 0 / 0（spec 与 plan 无未决占位符） |

## 8. 2026-11-06 收口复跑增量（独立复核二轮后）

本报告 §2 / §7 的 207/200 快照之后，控制侧按独立复核两轮的发现修复实现并补回归，
分析结论更新如下（只读复跑，不运行测试；绿态由 T032 门禁与验收报告承载）：

- **R-01（MEDIUM，已修复）**：`ConfirmCandidate` 原先只 `FOR SHARE` 元数据直接来源，
  未沿 `memory_candidate_sources` 递归核对来源闭包，与 C-16「来源闭包全部 active 且
  未过期」不符；契约 `memory-learning.md` §7.2 同步复述为「直接来源」。修复：单事务内
  递归闭包（深度上限 8，超限 fail-closed）与直接来源取并集、`ORDER BY id` 固定锁序，
  任一来源缺失 / 非 active / 过期即 409；契约 §7.2 / §7.3 更正为闭包口径并注明闭包
  复核只在事务内（app 预检是直接来源快速路径，事务为权威判定点）。新增
  `TestUS22_Confirm_ClosureInvalidatedRejected`（红态逐字
  `Expected error with "conflict" in chain but got nil`）。
- **R-02（MEDIUM，已修复）**：`buildPrompt` 的既有条目快照未做凭证过滤（窗口 / 前文
  已过滤），§3.1.13「提示词里不放凭证」可被既有条目绕过。修复：命中凭证的既有条目
  不进提示词（仍保留在冻结窗口、证据校验面与来源子集判定）。新增
  `TestUS22_Prompt_EntriesCredentialFiltered`（红态逐字显示提示词含条目 44 与凭证值）。
- **R-03（LOW，已修复）**：契约 §7.1 写 keep_both 携带替代目标 → 409，app 实际返回
  400。修复：app 改走 `memoryConfirmReject`（409），OpenAPI 与契约字面量不变。新增
  `TestUS22_Confirm_MixedResolutionRejected`（红态 `Target error should be in err chain`）。
- **stage 指标对账**：`harness/stages/us22-base.json` 实测 **217 条 / 210 唯一名**
  （`py -3` 直接统计；12 个 check：core 8、memory 69、provider 11、app 60、pg 39、
  web 11、cli 6、e2e 6、console-test 0、console-build 0、race 6、memory-race 1）。
  相对 207/200 快照 +10 / +10：F 修复轮补入 `TestUS22_LearningPort_ReadCommandsReachProductionPort`、
  `TestUS22_LearningPort_HandoffReachesRepository`、`TestUS22_LoadDraft_RejectsDotIDs`、
  `TestUS22_ReleaseRunRefsSkipsWhileRunning`（pg），R 修复轮补入
  `TestUS22_Confirm_ClosureInvalidatedRejected`、`TestUS22_Prompt_EntriesCredentialFiltered`、
  `TestUS22_Confirm_MixedResolutionRejected`；其余 3 条为 F 轮把既有具名用例补入
  `required_tests` 的 additive 调整（不改 argv、不删项、不弱化断言）。
- **重判**：R-01 / R-02 / R-03 均属实现与已审契约不符，按冻结判据修实现、补测试，
  不涉及范围、权限、公共契约字面量或验收语义变更；CRITICAL 仍为 **0**，HIGH 仍为
  **0**，可继续收口。F-07 / F-08 / F-09 维持「已知限制」登记（见验收报告 §6）。

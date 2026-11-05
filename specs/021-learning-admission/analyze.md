# US21 收口复跑一致性分析

2026-11-05；独立只读上下文对冻结模块卡 R2（`docs/us/us21-learning-admission.md`，SHA-256
`14a1b4f7a19c9d8fbf4232fd385db7520ad312372933a39a05a84fe7d4fe5e78`，2697 行）、
`spec.md` / `plan.md` / `tasks.md`、实现源码与 `docs/contracts/skill-admission.md` 重新比对；
不把 `run-state.md` 当证据，不运行测试（绿态由 stage 与七项证据承载）。实施前 analyze
（2026-11-04，CRITICAL 0）见 run-state §1.2。

## 1. Findings 与处置

| ID | 类别 | 严重度 | 位置 | 发现 | 处置（2026-11-05） |
| --- | --- | --- | --- | --- | --- |
| F-01 | 卡 / tasks ↔ 实现 | MEDIUM | 模块卡 :188、:1456；tasks.md:23；workspace/admission_store.go:131-139 | 卡与 T007 写「staging + 两次 rename」，实现为一次 `base.Rename` 就位；可观察契约（原子就位、残留 / 已存在拒绝、置只读）不变 | **收口登记**：acceptance.md 记解释性偏差；T007 已勾选，正文不改写 |
| F-02 | plan ↔ 实现 | MEDIUM | plan.md:57；web/server.go 路由表；admin_routes.go 未改 | plan/T021 列 `admin_routes.go` 为改动文件，两条只读路由实际落在 `server.go`；卡 §3.1.11 允许二选一 | **已处置**：plan.md Project Structure 更正并加收口注；T021 正文不改写，差异在 acceptance.md 登记 |
| F-03 | plan ↔ 实现 | MEDIUM | plan.md:54-57；app/admission_port.go；app/run.go、web.go、web/handlers_operations.go、storage/postgres/resource_store.go | plan 列 `app/ports.go` 为改动点（实际未改），未列实际改动的 run.go / web.go / handlers_operations.go / resource_store.go 与新增 admission_port.go | **已处置**：plan.md 文件清单按实测更新 |
| F-04 | 卡 ↔ 实现（接口契约） | MEDIUM | 模块卡 :1834-1846（9 方法）；cli/ports.go:833-905（12 方法） | 卡 §3.1.10 列 9 个业务方法；实现 12 个，新增 `ImportFiles` / `ReadSourceFile` / `ReadDecisionFile`；9 个业务签名逐字一致，增补理由是 cli 不得 import workspace | **review ② 决策项**：acceptance.md §6 显式登记；不静默改写已批准卡 |
| F-05 | 测试完整性 | HIGH | workspace/admission_store_test.go:323-326、:342-345、:474-477（原行号） | 三个 stage 必跑测试在 `os.Symlink` 失败时 `t.Logf` 后直接 `return`，无符号链接权限的 Windows 上显示 PASS 但拒绝断言未执行 | **已处置**：新增 `createSymlinkOrJunction`——真实符号链接优先，Windows 回退目录联接；实测 junction 被 Go 报告为 `ModeIrregular`（`?rw-rw-rw-`，symlink=false，2026-11-05），走「非普通文件 / 非目录」拒绝分支，故期望消息片段按链接类型参数化，断言仍是「拒绝 + 该对象类别」；三个用例在 Windows 实际执行并通过，gosec / vet / staticcheck 干净。真实符号链接分支（「符号链接」消息）本机无权限创建，平台覆盖在 acceptance.md 注明 |
| F-06 | 卡 ↔ 实现（文件落位） | LOW | 模块卡 :1797；app/admission_admin.go:1302；baseline.go:1020；tasks.md:45 | 卡 §3.1.9 命名 `internal/app/admission_recovery.go` 未创建；`VerifyOperation` 实际在 `admission_admin.go`，`baseline.go` 做分派与目标类型守门 | **收口登记**：acceptance.md 记文件落位偏差；T019 正文不改写 |
| F-07 | tasks 描述准确性 | LOW | tasks.md:15；core/admission.go:168-474 | T004 称「八类结构体」，实际 20 个 struct + 7 个字符串枚举类型；「23 个哨兵」与实现逐条一致 | **收口登记**：acceptance.md 记措辞口径差异；任务正文不改写 |
| F-08 | Harness 保护面 | LOW | harness/stages/us21-base.json protect | 原 protect 含两个未改动文件，缺本次核心新增文件（app/cli/web 生产文件与 US-21 测试） | **已处置**：protect 增补全部 US-21 新增文件与既有技能发布 / 策略 / 操作视图测试（card §3.1.14），仅追加、checks 与 required_tests 零变更，approval 字段留处置注 |
| F-09 | Harness 任务映射粒度 | LOW | harness/stages/us21-base.json tasks | T021 映射的 `us21-base-web` 必跑 T022 的 `ConsoleReadOnly` 与 T023 的 `OpenAPIContract`，任务级验收存在跨任务依赖 | **收口登记**：后续 stage 可按任务拆分 required 子集 |
| F-10 | 任务未映射 / 未完成 | LOW | tasks.md:55-56；stage tasks | T001 / T002 / T024 未映射（setup / docs，设计使然）；T026 / T027 未勾选 | **收口执行**：T024 内容已在 TechnicalDesign.md:1733、Requirements.md:859、Operations.md:1566、Compatibility.md:745 核对存在；T026 / T027 在完整 stage 与七项证据齐备后勾选 |

严重度计数：CRITICAL 0 / HIGH 1 → 0（F-05 已处置）/ MEDIUM 4（F-01/F-02/F-03 已登记或更正，
F-04 进 review ② 决策项）/ LOW 5（F-08 已处置，其余收口登记）。

## 2. FR 覆盖与测试存在性

15 条 FR 均有承载任务与已存在的具名测试；stage `required_tests` 逐包静态核对
core 16/16、workspace 13/13、app 52/52、postgres 14/14（含 3 个既有迁移测试）、web 5/5、
cli 5/5、e2e 5/5，无缺失。`*` 曾标注 F-05 的静默早退，已随 F-05 处置消除。

| FR | 承载任务 | 具名测试（节选，全表见 analyze 报告） |
| --- | --- | --- |
| FR-21-01 | T004、T005、T007、T011 | `Propose_FreezeAndHash`、`Propose_RejectsSymlinkSource`、`Propose_RejectsTraversal`、`CandidateDirReadOnly`、`AdmissionCandidate_RoundTrip` / `TamperDetected` / `MissingArtifacts` |
| FR-21-02 | T004、T005、T010 | `Evaluate_Dimensions` / `_Aggregate` / `_RiskTier` / `_RetentionExpired`、`EvaluationHash_BindsContentAndNormalisesTime` |
| FR-21-03 | T010、T013 | `NotEvaluable_IsNotPass`、`Approve_AcknowledgmentMismatch_Rejected`、`Approve_FailedEvaluationRejected`、`AdmissionBlockedDimensions_InconsistentRow` |
| FR-21-04 | T012、T013、T021、T023 | `WriteActions_RejectUntrustedActor`、`Approve_BearerRejected`、`Approve_UnknownCLIRejected`、`NoWriteEndpoints`、`Admission_UntrustedActorRejectedBeforeState` |
| FR-21-05 | T010、T013 | `HighRisk_SelfApproval_Rejected`、`HighRisk_SeparationPasses` |
| FR-21-06 | T010、T013、T015 | `Publish_StaleBaseline_Rejected` / `_StalePolicy_Rejected` / `_ContentChangedAfterApproval_Rejected` / `_SourceHashMismatch_Rejected`、`Approve_ContentChangedAfterEvaluation_Rejected` / `_SourceChangedAfterEvaluation_Rejected` / `_RetentionExpired_Rejected`、`Publish_RetentionExpired_Rejected`、`Policy_DomainTableRevisionChangesHash` |
| FR-21-07 | T010、T014、T015 | `Approve_ExpiryWindow` / `_ExpiredPublicationRejected`、`State_ExpiredIsDerived`、`StateMachine_DuplicateApprove_OneActive`、`AdmissionList_DerivedExpired` |
| FR-21-08 | T006、T015、T016 | `Publish_AtomicOnFailure` / `_UsesSharedPrimitives` / `_PointerCAS` / `_GateLocked` / `_FailureKeepsApproved` / `_RetryRechecks`、`ConcurrentPublish_OneWinner`、`Store_ConcurrentPublishSingleWinner`、`Store_PublishRejectsStaleBaseline` |
| FR-21-09 | T018、T020 | `Withdraw_RestoresPreviousVersion` / `_SupersededDoesNotMovePointer` / `_DeletedSkill` / `_FirstVersionRejected` / `_KeepsEvidence`、`Store_PublishThenWithdrawRestoresPrevious` |
| FR-21-10 | T011–T019 | `Audit_PreWriteBeforeMutation` / `_DetailJSONEmpty`、`Recovery_PublishNotApplied_NotReplayed` / `_UnknownNotRewritten` / `_DispatchCoversSevenActions` / `_VerdictOperationRows` |
| FR-21-11 | T006、T010、T015、T016 | `Publish_UsesSharedPrimitives`、`Policy_AdmissionsDirDenied` |
| FR-21-12 | T021、T022、T023 | `ConsoleReadOnly`、`OpenAPIContract`、`OperationTargetKind`、`AdmissionDetail_Projection`、`AdmissionList_DerivedExpired` |
| FR-21-13 | T010、T013 | `Evaluate_NoProviderOrNetwork` / `_ScriptsNotExecuted`、`Approve_ScriptsRequireStaticReview` |
| FR-21-14 | T005、T008、T013 | `Source_StrictJSON_*`、`Decision_StrictJSON_*`、`Secrets_NotEchoed`、`Errors_DoNotEchoSecrets`、`ReadAdmissionInputFiles_Bounds` |
| FR-21-15 | T003、T006、T013、T015 | `Propose_ActiveCandidateExists`、`Publish_DuplicateRejected`、`Migration_PartialUniqueIndex`、`StateMachine_DuplicateApprove_OneActive`、`Store_CreateCandidateEnforcesActiveName` |

卡 §4.2 写 `TestUS21_Source_StrictJSON` / `TestUS21_Decision_StrictJSON`，实现为带后缀的细分
用例（语义覆盖更细，非缺口）；其余测试名与卡逐字一致。

## 3. 未映射任务

- 未映射到 stage `tasks`：T001（setup 核对）、T002（stage 派生本身）、T024（文档增量）、
  T026（七目标 / 完整验收）、T027（analyze 复跑与收口证据）。
- T024 无自动化判据，四份文档内容已核对存在；口径确认记录在 acceptance.md。
- T026 / T027 在完整 stage 与七项证据齐备后勾选，之前不得宣称完成。

## 4. 宪法对齐

逐条静态核对宪法 v6.0.0 十条与三条架构约束，未发现违反：

1. **I 自实现 ReAct**：未触碰循环 / 工具调度；`go.mod` / `go.sum` 无改动，无 Agent 框架坐标。
2. **II 客户端只做协议调用**：评估器无模型调用，`Evaluate_NoProviderOrNetwork` 钉住。
3. **III Provider 显式映射**：未触碰映射表；准入只在既有发布事务后由基线绑定。
4. **IV 一目录一 Agent / 声明式技能**：准入复用 `ValidateSkillDraft` 与既有版本机制；
   `.basalt/admissions` 不在白名单（`Policy_AdmissionsDirDenied`）。
5. **V 审计 day-one fail-closed**：七个写动作 `BeginOperation` 均先于任何变更
   （admission_admin.go:279 / :351 / :675 / :762 / :832 / :965 / :1117）；恢复经
   `VerifyOperation`（:1302）与 `baseline.go:1020` 分派、`target_kind` 守门，只查证不重放。
6. **VI 授权与执行分离**：未新增 Policy / CommandRunner 接口或实现；评估不执行脚本。
7. **VII 同步 + 有界后台例外**：admission 生产文件无 `go func`、无 ticker、无业务
   `context.WithValue`；全部同步本机命令。
8. **VIII 单二进制**：零新增 Go / npm 坐标；console 只增只读页；`CGO_ENABLED=0` 不受影响。
9. **IX 手写 SQL / 迁移只追加**：新增 `0010_skill_admission.sql`，`SupportedSchemaVersion = 10`；
   `schema.pg.sql` 未改，0001–0009 未改写。
10. **X 范围卡紧**：无 SSO / RBAC / 自动蒸馏 / 真实模型 / 脚本执行 / 后台过期扫描。

## 5. 总结论

- **CRITICAL 计数 0；HIGH 在 F-05 处置后为 0**；15 条 FR 全覆盖，stage 必跑测试名与真实函数
  逐包对得上；契约字面量（7 存储状态 + 派生 `expired`、7 动作、`target_kind=skill_admission`、
  `detail_json={}`、恢复三态）与实现一致。
- **允许进入人工 review ②**，附三项：F-04 端口方法增补作为决策项显式裁决；F-05 的平台覆盖
  说明（本机 junction 走「特殊文件」分支，真实符号链接分支待 Linux CI / 有权限环境）；T026 /
  T027 以完整 stage 与七项证据勾选。
- 本分析不运行测试，不替代 stage 绿态、独立复核或人工收口。

## 6. review ② 修复轮复跑（2026-11-05，只读增量）

review ② 退回后按用户裁决重做（R-03 修改、F-04 接受、四项必解发现 + 修复轮独立复核三处残留），
本轮对**变更后**的工件重新比对：

- 模块卡：`docs/us/us21-learning-admission.md` SHA-256 `43ed5df7…`，2713 行（§7 追加两行修订记录，
  已审 R2 正文未改）；契约 `docs/contracts/skill-admission.md` `79d4dbe8…`，162 行（`policy_hash`
  纳入 `isolation.profiles.*`；脚本判定补「末尾点视同无扩展名」）；stage `4dd96738…`，
  `required_tests` 122 → 125；`plan.md` 评估器一行按 R-03 补 isolation 档位来源。
- 实现 ↔ spec：FR-21-06 的「策略哈希」在实现中覆盖 `baseline + backend + tools + 配置档位名 +
  数据库档位 + revision + isolation.profiles.*`，比 spec 文字更严，不冲突；评估 / 批准 / 发布三处
  共用同一 `BuildAdmissionPolicy`，漂移均走 `ErrAdmissionStalePolicy`。
- 实现 ↔ tasks：未新增 / 未重编号任务，勾选位未变（T001–T027）；新增的 7 条具名回归
  （`Policy_IsolationProfileChangesHash`、`Policy_IsolationProfileDriftRejectsApproval`、
  `Evaluate_ExtensionlessScript`、`Approve_ExtensionlessScriptRequiresReview`、
  `Propose_CommitUnknown_KeepsSnapshot`、`Withdraw_SupersededCommitUnknownRecorded`、
  `Evaluate_TrailingDotScript`）落在既有 T010 / T013 / T014 / T015 / T018 / T019 的 DoD 内，
  stage 以追加方式引用（无删改条目）。
- F-04：用户 2026-11-05 明确「F-04 接受」——12 方法（9 业务 + 3 只读辅助）保留，文件校验唯一实现
  不变，接口偏差已在 acceptance.md 登记为**已接受**，不再作为待裁决项。
- 宪法对齐：本轮改动未触碰循环 / 工具调度 / 映射表 / 迁移（仅 `0010` 为新增）；审计仍先写
  `pending`；提交未知统一 `ErrAdmissionCommitUnknown` 且只在恢复路径查证，未引入重放。
- 结论：**CRITICAL 0、HIGH 0**；无范围、权限、公共契约或验收语义的未授权变更；允许提交
  人工 review ② 复验。

## 7. review ② 修复轮第二轮复跑（2026-11-05，只读增量）

修复轮第二轮（F-01 工作副本不确定、F-02 冻结目录已就位、F-03 路径段首尾空白、F-04 `policy_hash` json tag）
落地后，对**变更后**的工件重新比对：

- 模块卡：`docs/us/us21-learning-admission.md` SHA-256 `751cb573e5dfd8230609726365083641ae3154ba31ef074b7f904dfb9a8ceb4e`，
  2715 行（§7 再追加一行，已审 R2 正文未改）；契约 `docs/contracts/skill-admission.md`
  `05b68e9a3ee5932fee1c7968d8f299fe8cfbc29f4bb265d8ac71350834a9b2c7`，162 行（脚本判定补「路径段首尾空白一律拒绝」，
  恢复补「工作副本交换 + 回滚双失败记 `unknown`」）；stage `4dd96738…`、`tasks.md` `f1edf284…`、`spec.md` `6f76656a…`、
  `plan.md` `94fe63b9…` 均未变。
- 实现 ↔ spec：F-01 / F-02 的不确定态与既有 `core.ErrAdmissionCommitUnknown` 同口径（预写 pending → 终态
  `unknown` → 恢复只查证不重放），未新增第二套执行 / 审计路径；F-03 在导入入口拒绝歧义路径，不改变 FR-21-13
  「不执行候选脚本」边界；F-04 只把 `policy_hash` 输入的 JSON 字段名固定为契约字面量，现有哈希断言均为相对比较，
  无固定向量受影响。
- 实现 ↔ tasks：无任务增删 / 重编号，勾选位未变（T001–T027）；新增用例落在既有 T007 / T010 / T011 / T015 / T018
  的 DoD 内；stage `required_tests` 保持 125 条不变。
- 宪法对齐：未触碰循环 / 工具调度 / 映射表 / 迁移（仅 `0010` 为新增）；审计仍先写 `pending`；未知态不触发重放；
  单二进制与零新增依赖不变。
- 结论：**CRITICAL 0、HIGH 0**；无范围、权限、公共契约或验收语义的未授权变更；允许重跑门禁并提交人工 review ② 复验。

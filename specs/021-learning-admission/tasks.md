# Tasks: US21 学习准入

**Input**: spec.md / plan.md / research.md / data-model.md / contracts；颗粒度文档 `docs/us/us21-learning-admission.md` R2（SHA-256 `14a1b4f7a19c9d8fbf4232fd385db7520ad312372933a39a05a84fe7d4fe5e78`）。

**Tests**: 已审 Harness 的必跑测试与关键反例必须落地，安全 / 并发 / 审计红态先行；不删除断言或无理由跳过。

## Phase 1: Setup

- [x] T001 核对 US-20 收口、登记行与颗粒度文档哈希；建立 `specs/021-learning-admission/run-state.md` 基线与派生工件指纹；DoD：登记唯一、R2 哈希可复算、analyze 无 CRITICAL。
- [x] T002 从具名判据派生 `harness/stages/us21-base.json`；DoD：`required_tests` 与命令匹配，保护路径覆盖新代码与既有技能发布 / 策略 / 操作测试，缺环境不冒充通过。

## Phase 2: Foundational

- [x] T003 在 `internal/storage/postgres/migrations/0010_skill_admission.sql`、`internal/storage/postgres/connect.go` 落四表 DDL 并把 `SupportedSchemaVersion` 推进到 10；DoD：既有 0001–0009 零改写、幂等重放、v9 旧库前向可迁移。
- [x] T004 在 `internal/core/admission.go` 落状态 / 风险档 / 维度 / 聚合 / 动作 / 目标类型常量、八类结构体与 23 个哨兵；DoD：`core` 仍只依赖标准库，既有常量无重复定义。
- [x] T005 在 `internal/core/admission_json.go` 与 `admission_json_test.go` 落严格 JSON 解析（未知键、体积、字段取值、保留期、决策绑定）与哈希 / 规范化函数；DoD：拒绝路径逐条有反例，`AdmissionScriptReviewJSON(nil)` 返回 `[]`。
- [x] T006 在 `internal/storage/postgres/admission_store.go` 与 `admission_store_test.go` 落候选 / 评估 / 决策 / 发布记录仓储与 `publishTx` 可选期望扩展；DoD：唯一冲突、状态 CAS、`Latest*` 按 seq、并发发布单赢家、既有资源发布测试不变。

## Phase 3: User Story 1 - 提议、冻结与确定性评估 (P1)

独立判据：合法目录逐字冻结可复算；非法目录与重复活跃候选拒绝且零写入；七维评估固定可复算。

- [x] T007 [US1] 在 `internal/workspace/admission_store.go` 与 `admission_store_test.go` 落 `.basalt/admissions/candidates/<id>/` 冻结读写（staging + 两次 rename + 只读）；DoD：软链接 / 穿越 / 超限 / 哈希不符 / 残留目录逐条拒绝，读取时重跑 `ValidateSkillDraft`。
- [x] T008 [US1] 在 `internal/app/admission_secret.go` 与测试落凭证扫描（含 `${ENV_VAR}` 放行、只回路径）；DoD：命中直接阻止批准，错误与日志不回显凭证。
- [x] T009 [US1] 在 `internal/app/admission_refs.go` 与测试落 `scanSkillReferences`（活动 + 归档 `AGENT.md`，解析失败 fail-closed）；DoD：unparseable 计入可能引用，归档名口径正确。
- [x] T010 [US1] 在 `internal/app/admission_eval.go` 与 `admission_eval_test.go` 落 `BuildAdmissionPolicy` / `BuildAdmissionBaseline` / 七维判定 / 风险档 / `admissionExpiryWithin24h`；DoD：域名表修订变化改哈希、`not_evaluable` 不计通过、相同输入同结果、`.basalt/admissions` 不在任何 Agent 读写白名单（策略断言用例）。
- [x] T011 [US1] 在 `internal/app/admission_admin.go` 落 `Propose` / `Evaluate` / `List` / `Get`（含 `ImportFiles` 与工作副本复用）；DoD：重复活跃候选 409 零写入、评估落库与操作终态同事务、磁盘成功库失败记 `unknown` 不自动删孤儿。
- [x] T012 [US1] 在 `internal/cli/admission.go`、`ports.go`、`deps.go`、`root.go` 与 `internal/app` 接线落 `propose|list|show|evaluate`；DoD：写命令取实例锁、`cli:unknown` 在开库前拒绝、读命令不取锁。

## Phase 4: User Story 2 - 本机批准与受控发布 (P1)

独立判据：身份 / 哈希 / 基线 / 策略 / 风险档 / 保留期任一漂移即拒绝；合法发布单事务且并发单赢家。

- [x] T013 [US2] 在 `internal/app/admission_admin.go` 与测试落 `checkTrustedActor` / `checkApprovalSeparation` / `checkAdmissionApprovalBoundary` / `checkScriptReview` 与 `Approve` / `Reject`；DoD：确认集合精确匹配、阻止维度不可确认、高风险自批拒绝且无决策行、重复批准只有一条活跃。
- [x] T014 [US2] 在 `internal/app/admission_admin.go` 与测试落 `Cancel` 与批准过期语义；DoD：取消 supersede 活跃批准、过期只在展示层派生、发布拒绝过期。
- [x] T015 [US2] 在 `internal/app/admission_admin.go` 与测试落 `Publish`（工作副本、快照、gate、事务、补偿 `abortSkillCommit`）；DoD：内容 / 来源 / 基线 / 策略 / 风险档不一致逐条拒绝，事务失败整体回滚候选保持 `approved`。
- [x] T016 [US2] 在 `internal/app/resource_admin.go` 抽取 `abortSkillCommit` 并让 `publishTx` 期望参数贯通；DoD：既有技能发布 / 撤回行为零变化，前序回归全绿。
- [x] T017 [US2] 在 `internal/cli/admission.go` 落 `approve|reject|cancel|publish` 与实例锁；DoD：决策文件路径 / 版本冲突映射稳定，`--approver` 之类旁路不存在。

## Phase 5: User Story 3 - 撤回、审计恢复与只读观测 (P2)

独立判据：撤回三分类正确且不删证据；恢复只查证；管理面无写入口。

- [x] T018 [US3] 在 `internal/app/admission_admin.go` 与测试落 `Withdraw` 与 `classifyAdmissionWithdrawal`；DoD：恢复上一版内容建新版本、首版拒绝、被超越 / 已删除只记撤回，快照失败记 `failed` / `unknown` 且候选保持 `published`。
- [x] T019 [US3] 在 `internal/app/baseline.go`、`wire.go` 落 `VerifyOperation` 各动作查证与 `RecoveryDeps.Admission`；DoD：应用 / 未应用 / 不确定三态与终态一一对应，从不重放。
- [x] T020 [US3] 在 `internal/cli/admission.go` 落 `withdraw`；DoD：与发布同一锁序，原因先校验后幂等返回。
- [x] T021 [US3] 在 `internal/web/handlers_admissions.go`、`views.go`、`admin_routes.go`、`server.go` 与契约测试落只读列表 / 详情与 `target_kind=skill_admission`；DoD：缺端口 500 非空列表，POST / PUT / PATCH / DELETE 均非成功。
- [x] T022 [US3] 在 `assets/console/src/api.ts`、`router.ts`、`app.ts`、`views/admissions.ts`、`views/admission-detail.ts`、`tests/admissions.test.ts` 落只读页面并重建 dist；DoD：无写控件、无 token 存储，console-test 与 build 通过。

## Phase 6: 契约、文档与收口

- [x] T023 新建 `docs/contracts/skill-admission.md` 并在 `run-and-errors.md` / `openapi.yaml` 追加七动作、目标类型、两个只读端点与恢复行；DoD：固定字面量与实现逐字一致，`schema.pg.sql` 不改。
- [x] T024 在 `docs/TechnicalDesign.md`、`Requirements.md`、`Operations.md`、`Compatibility.md` 追加「学习准入增量（US-21）」与 PG v10 / 备份 / 运维口径；DoD：无提前把未实现能力写成现状。
- [x] T025 在 `internal/e2e/us21_admission_test.go` 落全链路、并发、恢复、撤回与只读面 e2e；DoD：固定时钟、本地夹具、无真实模型 / 网络 / 脚本执行。
- [x] T026 跑 `make check` 七目标与 `.\scripts\check-acceptance.ps1 --stage us21-base`，补 `go test -race` 单列命令；DoD：全部必验通过，覆盖率不低于底线，`CGO_ENABLED=0` 发布构建通过，`git diff go.mod go.sum` 为空。
- [x] T027 复跑 analyze、独立 AI 复核并逐条处置，完成七项证据至 `acceptance.md` 与 run-state；DoD：无未处置阻断，输出人工 review ② 收口包，禁止代签 / 自动提交。

# Tasks: US-19 持久流程与有界协作

**Input**: spec.md、plan.md、正式 Flow 契约；模块卡 R3 已审，用户2026-11-03全文批准。测试必需，G-FT写分支专关保留。

## Phase 1: Setup

- [x] T001 治理镜像、D07、登记与派生工件核对（P20；FR-023）：.specify/memory/constitution.md、docs/TechnicalDesign.md、docs/Requirements.md、docs/Compatibility.md、docs/us/REGISTRY.md；记录原 R3 hash、宪法 exact-match、模板及文档 lint。
- [x] T002 冻结 spec／plan／tasks／F01–F19 映射与实施前 analyze（P19/P20；FR-001–025）：specs/019-flow-collaboration、harness/stages/us19-*.json；不以空匹配／SKIP 过门禁。

## Phase 2: Foundational

- [x] T003 实现并测试 core Flow 窄类型／Schema 端口与 tool 严格子集适配（P07/P08；FR-018；F12）：internal/core/flow.go、schema.go、internal/tool/schema.go／schema_test.go；重复键／精确整数／超深／未知关键字发出前拒绝。
- [x] T004 实现并测试版本化定义／契约／目录加载／发布验证（P01；FR-001/002/014；F01）：internal/flow/definition.go、contract.go、loader.go／definition_test.go；终止性、必需验证、可信分支及路径安全。
- [x] T005 实现并测试 Task／revision／状态／预算／证据／结论／验证纯领域（P02/P04–P06；FR-005/008/011–014/017/020；F05/F07–09/F14）：internal/flow/{state,budget,evidence,claims,verification,revision,summary}.go 及对应测试。
- [x] T006 新增 PG v8 DDL／仓储／条件事务与查证（P09/P10；FR-003/005/020/023；F02/F03/F08/F14/F17）：internal/storage/postgres/flow_store.go、flow_evidence_store.go、migrations/0008_flow.sql、docs/contracts/schema.pg.sql；真实 PG 竞争／非法意图／引用保护／升级测试。
- [x] T007 G-FT 设计／隔离探针独立复核材料（P21；FR-006/007/011；F04/F12/F17）：specs/019-flow-collaboration/gft-design.md、internal/core 与 app 临时探针；记录实际 Claude 结论，尚未通过不得实现 FixedTool 写分支。

## Phase 3: User Story 1 - 发布执行与验收

- [x] T008 [US1] Service.Start／Publisher 的 current active／幂等／审计／持久受理（P01/P02；FR-001/003/004；F01/F02）：internal/flow/service.go、publisher.go 及 web／postgres 定点测试。
- [x] T009 [US1] core 共享工具组助手和结构化执行结局（P07/P21；FR-006/007/011/018；F04/F12/F17）：internal/core/{loop,tool_exec,service}.go 与兼容回归；已派发计数不因消息追加失败丢失，不生成第二消息／调用审计入口。
- [x] T010 [US1] 通过 G-FT 设计后实现 FixedTool 与 Flow 动作／预算接缝（P07/P11/P21；FR-006/007/011；F04）：internal/core/flow.go、internal/app/flow.go；共享 RunQuota／工具次数、零 Chat／prompt检索／压缩、冻结输入；完成行为关口才交付写节点。
- [x] T011 [US1] 实现受控节点执行／输出验证与结果／引用原子接受（P02/P06/P11；FR-005/006/012/014/020；F03/F04/F09/F14）：internal/flow/service.go、internal/app/flow.go 及真实 PG 接线测试。

## Phase 4: User Story 2 - 取消与恢复

- [x] T012 [US2] 实现取消／放弃／deadline 关闭意图及 join（P02/P03；FR-008/015/020；F05/F14/F16）：internal/flow/{service,state,worker}.go、cancel_test.go、app/flow_lifecycle_test.go；取消优先、unknown不清、终态不复活。
- [x] T013 [US2] 实现检查点格式／完成证据复用／动作查证与安全恢复（P02/P11；FR-007/015/023；F10/F17）：internal/flow/recovery.go、internal/app/flow.go；先既有恢复、无可信证据则 blocked，未知零重放。
- [x] T014 [US2] 真实进程／PG 故障窗口与身份／调用次数／预算核对（P17；FR-005/007/008/015/020；F04/F10/F14）：internal/e2e/us19_action_test.go、us19_recovery_test.go；每窗 fault-fired，不能只用接口错误代替 kill。

## Phase 5: User Story 3 - 有界协作与证据

- [x] T015 [US3] 顺序子流程栈／独立会话／原执行权限与版本钉住（P04/P11；FR-009/010；F06）：internal/flow/delegation.go、internal/app/flow_authorization_test.go；固定UID／hash、原Key撤销拒绝，同名不重绑。
- [x] T016 [US3] 祖先预算与共享准入交互预留／公平重排（P04/P11；FR-011/022；F07/F16）：internal/flow/budget.go、internal/app/run_admission.go、internal/callers 适配；原共享临界区原子检查、根不占叶子票据。
- [x] T017 [US3] 循环／进展持久窗口与合法分页（P07；FR-016/017；F11）：internal/core/progress.go、internal/flow/revision.go／对应测试；3重复／6交替／6无进展阈值与跨revision不清零。
- [x] T018 [US3] 来源有效性／依赖闭包／版本验证／上下文摘要（P05/P06/P11；FR-012–014/017/019；F08/F09/F13）：internal/flow/{evidence,claims,verification,summary}.go、internal/app/flow_context_test.go；硬约束不可裁剪，来源与任务摘要不混。

## Phase 6: User Story 4 - 运维与管理

- [x] T019 [US4] 固定有界 Worker、轮询领取／容量／排空与服务装配（P03/P11；FR-022；F16）：internal/flow/worker.go、internal/app/{flow,wire,run}.go；serve专用、构造不启动，实际join前PG不关。
- [x] T020 [US4] API／scope 闭集／归属／ETag／操作幂等／安全投影（P12；FR-004/008/010/021；F02/F06/F15）：internal/web/{ports_flow,routes_flow,handlers_flow,views_flow}.go、internal/callers、docs/contracts/openapi.yaml。
- [x] T021 [US4] CLI 服务客户端与离线校验（P13；FR-021；F15）：internal/cli/flow.go、internal/app/flow_cli.go／测试；token不进argv／日志、不起第二写实例。
- [x] T022 [US4] 诊断关联／OBS／清理引用保护（P15；FR-021/023；F15/F17）：internal/{observability,diagnostics}、internal/storage/postgres；真实锚点、无高基数指标／正文泄漏。
- [x] T023 [US4] 管理台流程／任务操作及状态错误测试（P14/P18；FR-024；F18）：assets/console/src/api-flow.ts、views/flows.ts、views/flow-task.ts、tests/flow-*.test.ts；轮询、旧响应、重复提交、unknown与关闭说明。
- [x] T024 [US4] 配置／运维／公开说明／图源同步（P20；FR-021/023/025；F17/F19）：basalt.yaml.example、docker配置模板、docs/Operations.md、README*、website及现有图源；不改历史US18证据、不宣称已交付未来能力。

## Phase 7: Polish & cross-cutting

- [x] T025 冻结12任务／资料／定义／验收器与单Agent基线并运行对照（P17；FR-025；F19）：testdata/flow、evals/flow、internal/e2e/us19_demo_test.go；12/12、错误成功0、非法调用0，真实模型未授权单列。
- [x] T026 200次容量／公平／32 open恢复／生命周期测量与真实浏览器（P17/P18；FR-022/024/025；F16/F18/F19）：internal/e2e/us19_lifecycle_test.go、.loop/runs/证据；spec SC-002–004逐项判卷。
- [x] T027 完整公共七目标、全部US19 exact stage、race与受影响全回归（P16/P19；FR-001–025；F01–F19）：harness/stages/us19-*.json；源码稳定并绑定指纹，失败历史保留，必验缺失不算通过。
- [x] T028 收口 analyze→独立复核处置→七项证据→用户review②（P20/P21；FR-001–025）：specs/019-flow-collaboration/run-state.md、验收报告；不自动批准／提交／推送／合并。

## Dependencies & Execution Order

T001→T002；基础 T003／T004／T005→T006。T007 G-FT设计可与纯领域／PG工作交错；不依赖写节点的任务不因该局部前置停工。T010必须在T007实际Claude通过后，且写节点交付前F04/F12/F17行为门禁通过。US1形成闭环后分别验证US2／US3／US4独立故事；共享文件按任务序串行，最后T025–T028汇总，不在任务边界等待用户继续。

## Parallel Example

仅说明无文件冲突的逻辑机会：纯Schema测试与定义夹具、前端错误态测试与运维说明、只读独立review可并行；当前主Agent按依赖推进，不自动派遣实现子Agent。真实执行／PG故障／资源清理与同工作区 mutation 不并行。

## Implementation Strategy

先判据与红态、再实现及定向回归，控制侧验收勾选；所有F组及P交付物映射见plan，不删除或降级设计行为。每任务写run-state与下一动作。只完成US1不构成US19收尾；完整公共＋全部US专属证据齐后才交人工review②。Q5真实模型未授权不执行，单列限制，不伪称效果通过。

## Phase 8: R4 工作流体验与业务演示（用户追加授权）

本增量依赖原 R3 已通过事实；T028 仍是最终人工收口，必须等待 T029–T036 完成。新增任务不改原已勾正文，原 Q5 未授权是历史记录，当前有界授权见 experience-design §5。

- [x] T029 补充设计／派生契约／F20–F24 stage／analyze 与独立只读设计复核（FR-026–031；P22）：experience-design.md、contracts、harness/stages/us19-experience.json；无新增执行语义／依赖。
- [x] T030 管理 Agent 目录与固定版本定义读取（FR-028；F21；P23）：internal/web、internal/app、OpenAPI；拒绝非 admin，零模型／工具外发，无 prompt／凭据，旧接口兼容。
- [x] T031 纯编辑模型、顺序画布与输入输出表单（FR-027/029；F22；P24）：assets/console/src/workflow-editor.ts、views/workflow-editor.ts；上游引用检查、稳定 ID、原文保真与高级模式，单元及组件反例。
- [x] T032 统一工作流导航、本机草稿、已发布版本编辑、发布／任务旅程（FR-026/027/029；F20/F22；P25）：app/router、flows、api-flow及测试；原 unknown 写锁／版本／审计不变。
- [x] T033 公开资料溯源／知识库与确认记忆／四角色 Agent 与业务工作流（FR-030；F23；P26）：业务演示夹具、配置与说明；无虚构资质／自动投标／经验覆盖事实。
- [x] T034 按授权完成真实模型长任务与三组对照（FR-031；F23；P27）：evals/flow、.loop证据；明确成本／调用／usage，未确认消费不按零，真实结果不预设专业分工优胜。
- [x] T035 新管理台真实 Chrome 全旅程、错误态与文案／排版修复（FR-026–030；F20–F23；P25/P26）：真实候选、截图与走查步骤；不以 mock 冒充真模型效果。
- [x] T036 最终稳定新树公共 full、base／process／experience、受影响回归及独立复核／文档同步（FR-001–031；F01–F24；P27）：所有报告绑定当前指纹，原历史保留后交 T028 人工 review ②。

2026-11-04 用户追加测试约束：T036 的 CI／基础／回归验收及后续 US 禁止真实模型，
由 `019-offline-model-policy` 验证执行前拒绝和子进程凭据剥离；T034 只在本次明确
授权的累计预算内进行隔离业务演示。T033／T036 补充无知识绑定叶子真实 PG 回归
`TestFlow_NoKnowledgeLeafKeepsSharedRunChain`，复用原 Worker／Run／审计链和本地模型
协议夹具；不得把该离线回归代替 T034 的真实业务结果。

## Phase 9: 随包默认采购工作流（用户2026-11-04追加）

- [x] T037 默认示例打包与一次性准备、升级保留、CLI、模板入口与离线验收（FR-032/033；F25；P28）：examples/defaults、builtin、app、cli、console；零自动计费/启用/发布，归档与用户配置保留。

T028 的收口前置追加 T037。T034 最后一轮真实模型改由用户人工运行；原对照/失败/预算不改写，AI停止自动真模型请求，不将人工待验项勾为通过。

T037 后按 FR-001–033／F01–25 重冻结并重跑 T036；T037 之前的最终报告不能代替新增范围的稳定树验收。

2026-11-04 用户人工review②已明确通过，并要求仅补两处显示后快速交由Claude提交和跟踪CI。
T034最后完整真模型链／T035人工页面走查据用户回执闭合，历史失败与三组对照的实际结果不改写。
T028本地交接以最后增量的定点回归＋构建为证据；用户明确免除本轮全量重跑，
T036先前final5共同指纹仍只是当时证据，提交后CI由Claude跟踪。详见run-state最新追加记录。
T037仍只代表已实现的4角色默认模板；bid-general与额外对照模板随包增量留在交接待办，不冒充完成。

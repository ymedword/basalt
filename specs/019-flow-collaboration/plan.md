# Implementation Plan: US-19 持久流程与有界协作

**Branch**: `019-flow-collaboration` | **Date**: 2026-11-03 | **Spec**: [spec.md](spec.md)

**Input**: Approved R3 derived specification; no further tasks approval gate.

## Summary

单机 PG 持久顺序任务；定义／契约不可变，CAS／短事务／动作身份／祖先预算／证据验证共同约束；叶子复用 app 共享准入及 core 唯一执行链。普通消息同步不变。G-FT 设计与行为两关分别保留；未通过设计关口不实现写分支。

## Technical Context

**Language/Version**: Go 1.27.3（go1.27 最低要求），TypeScript／既有控制台。

**Primary Dependencies**: 原 go.mod 坐标及版本不变；goccy/go-yaml v1.19.2、pgx/v5 v5.11.0、jsonschema/v6 v6.0.3；后者从测试扩到 tool 生产适配器。

**Storage**: PostgreSQL 16／17 + pgvector，当前 v7，追加 0008_flow.sql／v8；SQL 手写，旧迁移不改。

**Testing**: 标准 testing／testify，tag integration／e2e；真实临时 PG、回环 mock、真进程故障、race、Vitest、浏览器、完整公共 Harness。

**Target Platform**: 原 Windows／Linux 单机；其他发布目标保持既有静态交叉构建；不引入分布式执行。

**Project Type**: 单 Go module／内嵌管理台／CLI 服务客户端。

**Performance Goals**: spec SC-002／003；不把 mock 延迟当生产 SLO。

**Constraints**: 正式 Flow 契约的全部硬限；未知副作用零重放；原身份／权限交集；先 join 再关共享 PG。

**Scale/Scope**: 默认4 active／32 open／10000 stored；单根最多256推进、深度3、累计16子任务；12项冻结对照。

## Constitution Check

| 原则 | 实施前与设计后检查 |
| --- | --- |
| I／II | 不引入框架；只经 AgentService／ReActLoop／ToolExecutor／Provider，抽取同一消息组助手，不复制写入路径；FixedTool 需要结构化真实结局 |
| III／IV | 固定 Agent UID／配置／资源 hash；名称只是标签；不更改目录技能绑定协议 |
| V | 任务／动作与调用 pending 先于外发，管理发布／退役／放弃先审计；终态写失败保留不确定，不解析文本当成功 |
| VI | 每叶原 caller 当前权限交集；flow 不直接依赖具体 Policy／Runner |
| VII | 正式 v6.0.0 与当前镜像已同步；普通 Run 同步，固定有界 Worker 仅 serve 启用；未知写停止，单任务无并行 |
| VIII | core 仅标准库，适配器使用已有坐标；CGO_ENABLED=0 发布与 race 分开 |
| IX | 消费方窄端口，postgres 实现；短事务、CAS、CHECK／复合FK；旧迁移零改写 |
| X | 范围对齐已批准 R3；不偷加 A2A／分布式／代码执行／通用审批或发布 |

实施前与设计后未发现原则冲突；G-FT 未完成是显式执行前置，不记作已通过。Q5 缺真实授权只影响 external 效果证据，其他独立工作继续。

## Project Structure

### Documentation (this feature)

`specs/019-flow-collaboration/` 包含 spec、plan、research、data-model、contracts、quickstart、tasks、run-state、analysis；正式接口唯一源 `docs/contracts/flow.md`，OpenAPI／PG字面量随实现三向核对。

### Source Code (repository root)

文件、组件与关键方法落位

以下路径是**待评审交付计划**，不声称文件已存在。新增方法和类型的候选签名统一见 §6.6.2；这里按职责逐项引用，避免第二套接口定义。每一行必须在后续 tasks 比对中有落实任务；路径合并可记录派生原因，不能删行为。

| ID | 路径／组件 | 类级职责／关键方法位置 | 验收 |
| --- | --- | --- | --- |
| P01 | `internal/flow/definition.go`、`contract.go`、`loader.go` | 不可变定义／契约、目录加载与终止性；候选 Loader.Load／Publisher.Publish | F01 |
| P02 | `internal/flow/state.go`、`service.go`、`ports.go` | 任务状态与转移、Service.Start／Advance／Cancel／Abandon／Recover／Revise；Repository 对应条件写完整列于 §6.6.2 | F02–F05／F10／F16 |
| P03 | `internal/flow/worker.go` | Q1-A 的服务生命周期、存储轮询、容量与停止；Worker.Run | F05／F16 |
| P04 | `internal/flow/delegation.go`、`budget.go` | 顺序委托栈、Agent／子流程固定版本、祖先预算预留与结算、子会话边界；交互名额预留在共享准入完成 | F06／F07／F16 |
| P05 | `internal/flow/evidence.go`、`claims.go`、`revision.go` | 槽位／观察、依赖闭包、修订接受、失效与复用；Service.Revise | F08／F09 |
| P06 | `internal/flow/verification.go`、`summary.go` | VerificationService.Verify、任务摘要及硬约束输入，禁止来源摘要混写 | F09／F13 |
| P07 | `internal/core/flow.go`、`progress.go`、`schema.go` | 标准库纯类型／窄端口：关联、授权上限、祖先预算、进展停止、Schema 校验 | F07／F11／F12 |
| P08 | `internal/tool/schema.go` | JSON Schema 编译适配器；现有坐标从测试用途扩展到生产，TechnicalDesign §1.3 同步；core 不引第三方库 | F12 |
| P09 | `internal/storage/postgres/flow_store.go`、`flow_evidence_store.go` | 手写 SQL、短事务、条件写、幂等与提交查证、同任务关联约束 | F03／F14／F17 |
| P10 | `internal/storage/postgres/migrations/<next>_flow.sql` | §6.6.3 候选表／索引／外键；更新 SupportedSchemaVersion 与 schema.pg.sql | F17 |
| P11 | `internal/app/flow.go`、既有 `wire.go`／`run.go`／`run_admission.go` | NodeRunner 经共享准入适配；B 有效性／callers／诊断端口接线；启动核对及退出顺序 | F04／F05／F16 |
| P12 | `internal/web/ports_flow.go`、`routes_flow.go`、`handlers_flow.go`、`views_flow.go` | 新业务／管理面分离，条件操作、幂等、分页与安全投影；§6.6.4 | F02／F06／F15 |
| P13 | `internal/cli/flow.go`、`internal/app/flow_cli.go` | 通过服务 API 的任务命令；离线只读定义校验；不能绕实例锁开第二写进程 | F15 |
| P14 | `assets/console/src/api-flow.ts`、`views/flows.ts`、`views/flow-task.ts`、现有路由／导航 | 定义与任务操作闭环、节点／Run／产物链接、轮询／未知态／错误态 | F18 |
| P15 | `internal/observability`、`internal/diagnostics`、现有清理适配 | 复用 OBS 时间线／字段白名单／脱敏，新真实关联而非假定旧 Task 锚点可直接复用；Flow 引用保护 | F15／F17 |
| P16 | `internal/flow/*_test.go`、core／tool／postgres／app／web 对应测试 | 单元、真实 PG、接线、竞争与负向断言；§4 对号 | F01–F17 |
| P17 | `internal/e2e/us19_*_test.go`、`testdata/flow/`、`evals/flow/` | 真实二进制、受控故障、固定本地资料与 mock；固定任务单 Agent 对照 | F04／F10／F16／F19 |
| P18 | `assets/console/tests/flow-*.test.ts`、重建 dist／integrity | 测试真实操作与路由；构建后嵌入单二进制 | F18 |
| P19 | `harness/stages/us19-{base,linux,local,external}.json`（按适用项派生） | 公共层引用、exact required_tests、任务映射／保护；不预填批准 | §4.4／§6.3 |
| P20 | 正式文档／配置／公开图源／兼容记录 | §1.3 八项影响清单同步；PG 升级、恢复处置及声明范围 | F17／F19／人工 |
| P21 | 本卡 §6.6.2 FixedTool 独立设计说明；必要时隔离探针与结果记录 | 写路径实施前审查消息／工具组共享点与 II／V 反例；该门禁不能由功能已实现替代 | F04／F12／F17；门禁 G-FT |

## Phases

1. 配套治理与登记／规格、任务比对、只读 analyze；先冻结全部 F01–F19 判据。
2. 纯类型／Schema／定义／状态／预算／证据与验证，真实 PG 仓储和迁移。
3. G-FT 独立设计与隔离探针交实际 Claude；共享 core 助手及 read-only Flow 接线可独立验证，写分支等待设计关口。
4. 顺序 Worker、取消／安全恢复、共享准入／上下文／权限，真实 API／CLI／前端闭环。
5. 真实故障矩阵、race、容量／12项对照、完整公共并集、独立复核，最终交用户 review ②。

## Test Traceability

FR、测试文件与反例映射

FR 是本卡待批功能判据编号，不冒充已生成 spec 的 FR。后续 spec 可扩展编号映射但不能漏项。每个 F 行列出的具名测试均为计划交付；此处不报告实跑结果。

| F／FR | 测试文件与 required_tests 候选 | 可判定结果及逆改点 |
| --- | --- | --- |
| F01／FR01 定义版本；FR02 契约与终止性 | `flow/definition_test.go`：`TestFlow_DefinitionRejectsUnsupported`、`TestFlow_DefinitionPublishFreezesVersion`、`TestFlow_DefinitionRejectsUnboundedRoute`、`TestFlow_RequiredSlotMustHaveValidator`、`TestFlow_BranchReadsTrustedSlotState` | 并行、无界跳转、未知关键字／必需槽位无规则拒绝；judgment 缺校准／人工要求拒绝；branch 只读 C 可信槽位；删覆盖校验或信任模型槽位字符串即红 |
| F02／FR03 持久接受与请求幂等；FR04 查询 | `web/handlers_flow_test.go`、`postgres/flow_store_integration_test.go`：`TestFlow_StartIdempotency`、`TestFlow_StartAckRequiresCommit`、`TestFlow_StatusReadHasNoSideEffect`、`TestFlow_StartOnlyCurrentDefinition`、`TestFlow_DefinitionPublicMetadataNotTaskAccess`、`TestFlow_IdempotencyAfterDefinitionSwitch`、`TestFlow_AbandonRequiresAdminAudit` | 同键同请求一个任务；切版／退役后重试仍回原任务，旧版新键拒绝；公共定义不赋 Agent／来源权；abandon 无管理审计零关闭／零调用；删固定版本核对或把 Key 当管理凭据即红 |
| F03／FR05 合法条件转移 | `postgres/flow_store_integration_test.go`：`TestFlow_ConcurrentAdvanceSingleAttempt`、`TestFlow_StaleRevisionCannotCommit` | 双推进只有一组真实尝试；旧 revision／代次不能接受结果；移除 CAS 即出现第二调用／旧产物 |
| F04／FR06 唯一执行入口与审计；FR07 动作身份 | `app/flow_integration_test.go`、`e2e/us19_action_test.go`：`TestFlow_PrewriteFailureNoCall`、`TestFlow_ToolNodeUsesCorePipeline`、`TestFlow_ActionIdentityAcrossAttempts`、`TestFlow_FixedToolGateAndOrdinaryMessagePath`、`TestFlow_AgentNodeCannotWrite`、`TestFlow_FixedToolUsesRunAndToolBudgets`、`TestFlow_WriteInputFrozenBeforeDispatch` | G-FT 前写节点不能发布／执行；伪造 FixedTool／Agent 写请求零调用；pending 失败零调用、历史唯一且组完整；固定 Tool 真实执行计 Run／父工具次数和 caller Run 配额、无模型调用不造 Token；上游派生参数须先冻结，dispatch 后改参零再调用；旁路／免计／换参重放即红 |
| F05／FR08 取消、完成和有界收尾 | `flow/cancel_test.go`、`app/flow_lifecycle_test.go`：`TestFlow_CancelWinsBeforeNextDispatch`、`TestFlow_CompleteCancelRace`、`TestFlow_CancelReleasesChild`、`TestFlow_BlockedCanCloseWithoutReplay`、`TestFlow_CancelIntentNeverRequeues`、`TestFlow_AbandonWaitsForLocalJoin` | 取消／放弃后恢复零排队；deadline 不得覆盖取消为 stopped；已有用户取消且已 join 优先 cancelled，无 caller Key 的管理救援须保留取消位；unknown／未知用量原样保留；清位、期限覆盖取消或未 join 提前关闭即红 |
| F06／FR09 委托边界；FR10 原归属授权 | `flow/delegation_test.go`、`app/flow_authorization_test.go`：`TestFlow_DelegationBoundsAndSessions`、`TestFlow_DelegationCannotExpandAuthority`、`TestFlow_CrossCallerInvisible`、`TestFlow_AuthorizingKeyRevokedBlocksResume` | 深度／子数／同会话递归拒绝；父子 CallerID 一致；专属资源、来源和父未授权目标零调用；同名新 UID 拒绝；跨调用方 404 |
| F07／FR11 祖先预算与配额 | `flow/budget_test.go`、`app/flow_quota_integration_test.go`：`TestFlow_ChildBudgetNeverResets`、`TestFlow_UnknownUsageKeepsReservation`、`TestFlow_ParentDoesNotHoldLeafRunSlot` | 跨子 Run／Key／revision 仍共用父预算；未知不释放；max concurrent runs=1 时顺序叶子能完成；删祖先扣减／父占 Run 票据即红 |
| F08／FR12 版本化证据；FR13 闭包失效 | `flow/evidence_test.go`、`postgres/flow_evidence_integration_test.go`：`TestFlow_EvidenceInvalidationClosure`、`TestFlow_ClaimDependencyCycleRejected`、`TestFlow_RecoveryRechecksSourceAuthority` | 纠正／删除／换版／撤权传递到间接依赖；无关结果保留；核对报错阻断复用；仅失效直接结论或跨域关系放行即红 |
| F09／FR14 独立验收与防伪造 | `flow/verification_test.go`：`TestFlow_ModelCannotChangeContract`、`TestFlow_CallerCannotWeakenContract`、`TestFlow_ToolSuccessIsNotTaskAcceptance`、`TestFlow_RepeatedSourceIsNotIndependentEvidence`、`TestFlow_VerificationBindsTargetVersion` | 调用方与模型都不能删槽位／弱化验收／抬高预算；success 但证据不足不得成功；重复来源只计一份；input 覆盖模板控制字段或旧版 pass 沿用即红 |
| F10／FR15 检查点与安全恢复 | `e2e/us19_recovery_test.go`：`TestFlow_CrashWindowMatrix`、`TestFlow_UnknownActionNeverReplayed`、`TestFlow_CompletedNodeReusedAfterRestart`、`TestFlow_UnknownCheckpointFormatBlocks` | 按 §4.3 窗口 kill，观察动作 ID 集合／次数及结果；unknown 零重放、已保存完成节点零重跑；删完成核对／任意 retry 即红 |
| F11／FR16 循环检测；FR17 有限修订 | `core/progress_test.go`、`flow/revision_test.go`：`TestFlow_RepetitionAndAlternationStop`、`TestFlow_DeclaredPaginationMakesProgress`、`TestFlow_NoProgressAcrossRevision`、`TestFlow_RevisionCrashKeepsSingleHead` | A-A-A／A-B-A-B-A-B 停止；有效新游标／新证据不误杀；换措辞／噪声 hash 不当进展；revision 前后退出只一头且预算不重置 |
| F12／FR18 强参数校验与拒绝 | `core/tool_exec_test.go`、`tool/schema_test.go`：`TestFlow_NestedSchemaViolationZeroDispatch`、`TestFlow_UnknownSchemaKeywordRejected`、`TestFlow_SchemaAdapterFailureFailsClosed` | 嵌套错误、范围／枚举／额外字段／重复键零真实调用；审计／日志及模型错误回填可查；把校验挪到 Tool.Execute 内即红 |
| F13／FR19 长任务上下文与摘要 | `app/flow_context_test.go`：`TestFlow_SummaryKeepsHardConstraints`、`TestFlow_SummaryFailureKeepsUnresolved`、`TestFlow_ContextOverflowZeroModelCalls` | 摘要绑定 revision；任务／来源摘要不混；丢硬约束、核对失败后沿旧摘要、超限仍调用即红 |
| F14／FR20 原子提交／结局查证 | `postgres/flow_store_integration_test.go`：`TestFlow_CommitOutcomeReconciled`、`TestFlow_ResultAndDependenciesAtomic`、`TestFlow_InvalidCloseIntentCombination` | 断开提交回执后按事务事实查证；结果／依赖／预算／转移全有或全无；真实 PG 对 none／cancel／abandon／deadline×取消位共八种组合校验，四种非法组合触发 CHECK、零状态／转移／预算改变；移除 CHECK／部分提交即红 |
| F15／FR21 CLI／API／诊断与隐私 | `web/handlers_flow_test.go`、`app/flow_cli_test.go`：`TestFlow_ManagementCallerBoundary`、`TestFlow_CLIRoutesThroughService`、`TestFlow_DiagnosticsDoNotAdvanceTask`、`TestFlow_SensitiveDataProjection` | Caller Key 无管理权；CLI 不绕实例锁；查询零执行；敏感值不进响应／日志／导出／指标；伪造锚点即红 |
| F16／FR22 容量、背压和停机 | `e2e/us19_lifecycle_test.go`：`TestFlow_DurableCapacityLimit`、`TestFlow_ClosedBlockedReleasesOpenCapacity`、`TestFlow_DeadlineIncludesBlockedTime`、`TestFlow_ChildrenCountAcrossAllDepths`、`TestFlow_InteractiveReserveUnderFlowLoad`、`TestFlow_SingleRunSlotRequiresExplicitOptIn`、`TestFlow_ShutdownJoinsBeforeDBClose`、`TestFlow_LoadAndLatency` | unknown 关闭后 open 可回收但未知账仍占；blocked 不暂停 1800s；累计子数跨深度／revision；Flow 满载时实例与同 caller 普通请求仍获预留；去共享原子预留／重启清计数／提前关 PG 即红 |
| F17／FR23 迁移／保留与兼容 | `postgres/flow_migration_integration_test.go`：`TestFlow_MigrateFromPreviousVersion`、`TestFlow_RetentionProtectsRecoveryReferences`、`TestFlow_ClosedUnknownPinsDefinitionAndContract`、`TestFlow_ExistingRunSemanticsUnchanged` | 新建／升级一致；活动与关闭后的 unknown 仍保护定义／契约／调用依据；旧消息同步／忙即拒／审计五链路不变；终态即解保护或普通入口重复追加即红 |
| F18／FR24 前端操作闭环 | `console/tests/flow-actions.test.ts`、`flow-task.test.ts`：测试名冻结在派生 stage | 定义发布、任务启动、节点及产物可达、取消与恢复提交、错误不假成功；过期响应不回滚新态、重复点击不重发、隐藏停轮询 |
| F19／FR25 固定任务对照与交付 | `e2e/us19_demo_test.go`、`evals/flow/`：`TestFlow_DemoContractAndArtifacts`、`TestFlow_EvaluationCorpusIsFrozen` | 12 项任务输入／规则／基线版本固定；正常、空证据、冲突、取消、预算、unknown 都有预期；脚本不能改评分器，报告区分 mock 与真模型 |

## Frozen Bounds and Evaluation

候选配置、硬限与质量阈值

候选 `basalt.yaml` 新增 `flows` 段，默认 `enabled: false`，只有 `serve` 配置启用 Q1-A worker；`chat`／`gateway` 不暗中启动 Flow Worker。定义发布、任务数据与逻辑预算在 PG；配置只限定部署上限。有效额度 = 部署上限 ∩ 契约额度 ∩ 所有祖先剩余额度 ∩ 既有 Run／调用方额度，输入不许放大上限，未配置的关键预算拒绝而非不限量。

| 候选项 | 建议默认／硬限 | 验收含义 |
| --- | --- | --- |
| `flows.max_active` | 4（部署上限可降，首批最高 8） | 固定数量顺序 Worker；不同任务可并发，单任务无并行叶子 |
| `flows.interactive_reserved_runs`／`interactive_reserved_per_caller` | 各 1；仅显式配置可降为 0 | 原共享总名额之外给 Flow 加原子子上限；预留 1 而总名额≤1 时 Flow 无容量；0 不能宣称保留了交互能力 |
| `flows.max_open_tasks`／`max_stored_tasks` | 32／10000，含所有层次子任务；open 包含 queued／running／cancelling／blocked | 仅完成本地 join 后 cancelled／abandoned／其它终态释放 open；关闭 unknown 不释放其未知配额、不删证据；stored 不随关闭减少 |
| `flows.poll_interval_ms` | 250，范围 100–5000 | 持久存储才是队列权威；唤醒信号可丢但任务不可丢 |
| `flows.max_duration_seconds` | 每根任务 1800，含 queued／blocked／等人／停机墙钟；恢复不延后 | 子任务继承根截止，只能收紧；过期只核对／关闭，不续跑或续期 |
| `flows.max_nodes`／展开后推进总数 | 64／256 | 定义可静态证明有限终止；超过拒绝 |
| `flows.max_depth`／`max_children` | 3／16（root 深度 0；16 是根任务全生命周期、所有深度累计创建子任务总数，不含 root） | 完成／失败／关闭仍计累计；新 revision／重启不归零；同一子 ID 幂等回读不重复加，新子 ID 必须扣 |
| `flows.max_attempts_per_node`／`max_revisions` | 3／2（初始 revision 0，最多两次接受修订） | 只读重试与返工各受限制，root 总预算另扣；不允许 retry forever |
| `flows.max_total_tokens`／`max_tool_calls` | 200000／200；金额不是本阶段结算维度 | 未知 usage 保守占预留；实际超量时禁止新分配，不能已发生后报未执行 |
| `flows.cancel_wait_seconds` | 30；调用级审计终态回填仍用原独立 5s 收尾约束 | 30s 是操作有界等待，5s 是取消 context 外的最终写预算，不是 HTTP 停机期限 |
| `flows.shutdown_grace_seconds` | 35，须≥cancel_wait＋5s 且≤实际 HTTP ShutdownGrace（当前默认 70s） | Worker 停领／取消／join 独立与 HTTP drain 并行；共享 PG 必须等两者实际结束才关；超时如实报失败，不虚假完成 |
| 定义／契约／单输出／单产物 | 各 1 MiB；单任务产物总量 8 MiB | 读取中限制，拒绝超大，不静默截断使 JSON 看似可验收 |
| Schema 嵌套／属性／数组长度 | 深度 16／总属性 1024／数组最多 1024 元素 | 即便 schema 未设 tighter 限制也有部署硬限，避免校验耗尽 |
| 证据＋结论关系／失效遍历 | 每任务 1024 条／最多 2048 次边访问 | 添加前检查，超过阻断，不只失效部分后继续成功 |
| 循环窗口／连续重复／交替／无进展 | 16 次调用／3 次同签名／6 次 A-B 交替／6 次无验证进展 | 每节点持久记录有界摘要；参数 hash 规范化输入最多 1 MiB；结果只留 hash／引用 |

阈值只能通过受控配置或契约收紧；需要扩大首批硬限时先评估容量和验收影响。loop 的停止次数不是额外调用预算：达到任一原 Run／父／调用方上限立即按已有优先级停止。

**容量／公平性执行策略：**实例 Flow 叶子上限为 `max(0, MaxConcurrentRuns - interactive_reserved_runs)`，同时受 max_active 限制；每 caller Flow 叶子上限为 `max(0, effective_caller_max_concurrent - interactive_reserved_per_caller)`，effective 值必须由现有准入配置解析器确定，不另解释数据库零值／缺省。两项都在原总名额计数临界区检查，失败／取消／panic 共用原释放路径；Flow 不能因为另一个计数器尚有票据就突破共享总上限。root Worker 及准备节点不占叶子票据。启动时 flows.enabled 且实例无 Flow 可用槽则明确配置失败；caller 有效子上限为 0 时新任务接受拒绝 FLOW_INTERACTIVE_RESERVE。已接受任务遇额度调低只排队／退避至固定截止，不能换 Key／主体执行。

普通请求保留原“忙即拒”，不引入交互排队或抢占。Flow 等不到叶子名额时不占 Worker 睡眠等子任务，释放 root active 槽并持久记录 next_eligible_at／有界退避；未开始业务的容量拒绝不消耗节点执行 attempt。就绪根任务按 caller 轮转、同 caller 按接受顺序推进；仅承诺 Flow 不吃掉预留，不承诺普通请求在其它普通／调度负载已占满时也成功，也不保证交互持续满载时 Flow 无等待。

**单名额实例／caller 的显式取舍：**若确需总名额 1 下跑 Flow，操作者须显式把相应交互预留设为 0；此时一个 Flow 可占唯一名额，普通请求按原规则 429，手册说明无并发公平性保证。F07 的单 Run 槽反自锁测试使用该明确配置，不能与默认交互预留测试混用。F16 在默认预留模式下同时饱和 Flow 与发送普通请求，核对实例／caller 两种拒绝、名额释放及切换配置行为。

**I8 停机时钟分责：**单次审计终态回填 5s、用户取消等待 30s、Worker 排空 35s、HTTP 默认优雅停机 70s 是不同预算，不逐任务串行相加。服务停止同时停止 Flow 领取并取消叶子、开始 HTTP drain；35s 内完成实际 join 后才标 Worker 已退出。若超时仍有本地执行，返回／记录停机失败且不标任务取消完成，PG 不提前关闭；沿既有安全优先路径继续等待实际退出，必要时由部署层外部停机上限强制终止进程。US-18 当前外部兜底 90s 不是正常排空成功，也不证明任务未发生；下次启动先核对。这个超时失败分支须由 F16 验证，不能把“有界尝试等待”写成任何条件下都能成功排空。

性能候选基准：记录真实 CPU／内存／OS／PG／二进制与配置 hash，以 4 active、32 open、每任务 20 节点、mock 固定 50ms 调用、200 次独立任务提交测量；接受接口 p95≤2s，状态读 p95≤1s，可运行任务从可领取到开始的 p95≤1s（无活跃槽排队时），资源无持续增长。报告 p50／p95／p99、失败率、峰值内存、PG 连接／锁等待和队列等待；外部模型时延与协调开销分列，不能把这些建议写成已测生产 SLO。

恢复候选目标：在隔离基准 32 个 open 任务／有界历史内，依赖健康且可证明安全任务的启动核对≤30s；unknown 判为 blocked 也算核对完成，不能算业务恢复成功。已接受状态以 PG 持久化为权威；RPO 依赖实际数据库持久化与备份设置，本文不承诺基础设施故障零丢失。取消有界响应与实际动作停止分别测量，不声称取消能撤回已发送调用。

效果候选集：12 项脱敏固定资料任务，涵盖报告链、择一分支、有界双 Agent 交接、空证据、冲突版本、撤权、无进展、错误输出、取消、预算耗尽、已完成检查点复用、unknown 写动作。固定输入／资料版本／验收器／定义和单 Agent 基线版本，模型不改评分器；报告可交付率、必需槽位完整率、引用支持、错误成功率、Token／时间／协调开销。安全／停止样例按各自预期评价，不能为了提高“完成率”把应停止样例写成功。

本地 deterministic mock 的硬门槛为 12／12 预期行为正确、错误成功 0、非法调用 0；仅证明控制协议。开放质量另取同一批适用资料任务，至少三次配对运行并报告逐题结果、样本数／不可评和人工校准，真模型依 Q5；不预先承诺 Flow 质量或费用优于单 Agent，不以少量样本宣称统计显著。judge 缺失、校准不足或验证错误不能记 pass。

## Complexity Tracking

无宪法豁免。治理 MAJOR 已获用户全文批准并落盘，非实现者自行解释；G-FT 不通过不得以旁路／只读偷偷替换获批范围。新辅助结果类型用于表达现有工具真实结局，放 core 消费侧，不创造另一执行器。

## R4 实施增量（2026-11-03）

用户授权与具体边界见 experience-design.md。不新增执行引擎、依赖、数据库表或迁移。加性可选 FlowAuthoringBackend／两个只读 admin GET 端点，app 复用当前注册、生命周期与资源快照，原 FlowBackend 兼容。前端纯编辑模型＋Vue/SVG 顺序画布、映射表单、本机草稿、高级原文与发布确认共用原写入口。业务演示走原 Agent 配置、知识／记忆与 Run／Flow 链；公开原件只留本机并记录 hash／页码；真实模型预算独立账本，既有凭据不进入产物。

T029–T036 对应 P22：增量设计／契约／判据；P23：作者目录／完整版本读取；P24：编辑模型／画布／表单；P25：统一导航／草稿／发布旅程；P26：业务资料／Agent／工作流；P27：真实对照及完整最终验证。F20–F24 与原 F01–F19 并集判卷。用户最终 T028 必须在 T036 后，旧已勾任务正文保留。

### P28 默认采购工作流随产品交付

用户2026-11-04追加；映射FR-032/033、F25、T037。净化嵌入素材和builtin共享原子安装器；现有开箱标记可选字段防复活；显式CLI取实例锁；管理台模板用当前目录生成草稿，不新增执行器、API或模型供应商依赖。知识摄取/记忆确认/任务执行保持显式。

CLI 与启动共用工作流准备回执；CLI 不改写旧开箱标记，不改变 legacy 接管判定。
在首次启动发布身份之前删除的角色也不会被自动补回；损坏或未知已有回执保守保留。

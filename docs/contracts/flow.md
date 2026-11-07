# Flow 持久任务契约

> 状态：2026-11-03 已批准设计转录，待实现与验收；普通 Run 语义不变。本文冻结类型／API／配置与安全边界，不宣称 G-FT 已通过。

## 1. 领域对象、状态与目录协议

**领域名：**`Definition`／`TaskContract` 为不可变版本；`Task` 是跨 Run 的工作单元；`TaskRevision` 是当前受控输入／路线／槽位／摘要／预算视图；`NodeAttempt` 为一次节点尝试；`LogicalAction` 与 `ActionAttempt` 区分意图和发送尝试；`Observation` 指向原始工具／模型结果；`EvidenceRef` 为版本化来源定位；`ClaimRevision` 为结论版本；`ArtifactVersion` 为产物版本；`Verification` 记录验证目标与规则。**不复用 `core.TaskRecord`**，它仍是定时计划。

| 对象 | 枚举／合法转移 | 接受条件 |
| --- | --- | --- |
| Task | 初始 queued；`queued → running / cancelled / abandoned / stopped`；`running → succeeded / failed / stopped / blocked / cancelling`；`cancelling → cancelled / abandoned / blocked`；`blocked → queued` 仅限无关闭意图、未过期且恢复核对通过；`blocked → cancelled / abandoned / stopped` 仅按明确关闭／截止规则 | cancelling 不得转 stopped；其 abandoned 分支仅限下文无可用 caller Key 的管理救援。succeeded／failed／stopped／cancelled／abandoned 为不可复活终态；所有终态须本地执行已退出，unknown 动作独立保留 |
| 节点尝试 | `prepared → running → succeeded / failed / cancelled / interrupted / unknown` | 尝试记录不重写历史；unknown 动作阻止节点被当可重试 |
| 动作结果 | `prepared → dispatching → succeeded / failed / unknown`；prepared 取消可记 `not_issued` | dispatching 必须在外部发出之前持久化；缺记录不能视为 not_issued；失败是否已生效另列，不由 failed 推导零副作用 |
| 槽位 | `missing / found / conflict / unverifiable / verified` | found 仅找到材料；verified 要有适用的有效验证；必需槽位未验证不成功 |
| 结论 | `proposed / accepted / stale / superseded / rejected` | accepted 依赖与验证目标版本匹配；stale 不进入新最终产物 |
| 验证 | `pass / fail / inconclusive`，类型 `deterministic / evidence / judgment` | inconclusive 不作 pass；必需 judgment 无校准评分器／人工记录即未通过 |

首批结论生产绑定：作者的 `acceptance[].target_kind="claim"` 以 `target_id` 明确引用既有 `agent/tool` 叶子节点 ID，一节点对应一份结构化结论。该节点通过固定 `output_schema` 的整份 canonical 输出只是低信任提议；服务器分配 task、claim revision、状态和验证身份，模型不能写入权威状态。必需结论的生产节点须位于每条可成功终结的路线；产物仅引用当前有效、依赖完整且对应版本验证通过的结论。此绑定补齐已批 `ClaimRevision/VerifyRequest` 语义，不引入全局保留 `claims` 字段、ClaimSpec、新 REST 或任意多结论提议协议。

内部 NodeOutput 检查点携带服务器从实际输入映射派生的确切 ClaimRef 闭包，穿过没有 claim 规则的中间节点也不能丢边。可选提议未通过时保留其结论／验证事实，但该输出不能作为受信上游；与它无关的路线仍可推进。产物的 claim 依赖必须等于实际生产输出的闭包。跨 revision 保留的上游证明绑定原输出 revision／attempt／规则版本，并即时重查来源；不要求无关上游重新推理，也不把旧证明套给新输出。验证暂不可用造成的 blocked 以 interrupted Flow attempt 保留原完成 Run，恢复只重验原字节；新验收事实另用状态版本绑定的内部操作 ID，不覆盖旧事实。

内置确定性来源验收器 `validator_id="basalt-source-value-v1"` 仅证明“确切版本的整份来源数据等于该输出”：JSON 原文按严格 canonical 值比较，非 JSON 原文与 JSON 字符串输出逐字比较；不通过子串命中证明自由文本推断。原文按既有 SourceView 在原身份、冻结集合和当前 ACL 下读取，截断、旧版本、未知位置、权限变动均不通过；独立资料按 source UID 去重，作者要求的各份完整资料值不一致记冲突。该验收器不需要模型，也不证明一般业务判断；一般语义判断仍须独立、有校准记录的可信评分器，缺失时保持 inconclusive／blocked。没有作者点名这一验收语义时不能自动套用。

Task 的 `blocked` 是恢复或外部结果不确定，不等于授权了重试；`stopped` 保留原因与部分产物。Run 内循环守卫触发时仍用原 `limit_reached`，增加原因 `repeated_call / alternating_calls / no_progress`；Flow 根据验证决定 stopped，**不把 Run 的 partial 当作通过**。取消／完成／恢复的 Task 条件写见 §2.3E／F。

**B1／B2 的判定顺序固定：**先检查终态，再检查持久 `cancel_requested`／`close_intent`，再检查 deadline，最后才检查恢复资格。`cancel_requested` 一旦 true 不得清回 false；取消等待超时只能保留 cancelling 或带原取消位的 blocked。此类 blocked 即使原动作后来查证成功，也只能完成关闭／对账，永不 queued。`close_intent` 为 `none / cancel / abandon / deadline`；存储与条件写共同维护不变量 **`cancel_requested == (close_intent == "cancel" || close_intent == "abandon")`**，二者同事务更新，不允许一边成功另一边失败。deadline 只可在尚无取消／放弃意图时置 close_intent=deadline，取消位保持 false；不得用 deadline 覆盖 cancel／abandon。服务停机仅记中断原因，不擅自设置用户取消／放弃意图。

另有状态约束：status=cancelling 必须同时满足 cancel_requested=true 且 close_intent 属于 cancel／abandon；deadline 的执行 context 被取消不代表设置用户取消位。CAS／DDL CHECK 与对应负向用例共同约束该关系。非终态 deadline 与用户取消竞争时，必须重新核对旧版本和意图；已提交终态先返回原结果，不反向重写历史终态。

**终态关闭优先级：**已有用户取消（close_intent=cancel）且本地执行已收束时，优先 CAS cancelled；管理员晚到也不能把这个可收束取消改为 deadline 或 abandoned。`cancelling → abandoned` 仅允许在本地尚未收束、调用方已无可用任务关闭 Key 时，由管理员以独立、带理由的救援操作持久记录依据并接管关闭意图为 abandon；原 cancel_requested=true 必须保留，实际 join 后才可 abandoned。没有该条件的取消继续走 cancelled／blocked。普通管理放弃非取消中的任务仍按既有 abandon 规则：有叶子先记带 abandon 意图的 blocked 并等待退出，实际 join 后 abandoned，不借此扩大 cancelling 的例外。以上路径均不得重放业务动作或释放未知配额。

原调用方以 `flows:write`（统一替代 R0 的 flow:cancel）可取消自己 blocked 的任务；管理员可在 Key 已撤销时以独立管理操作发起 abandon，理由必填。若没有本地叶子存活且执行许可已收束，可直接 CAS cancelled／abandoned；若仍活，先持久关闭意图并传播取消，202 返回后仍占执行／open 名额，直到 join 或启动时证明原进程已退出。不能拿“超时”充当退出证明。根任务关闭向其有界子树传播同一禁止推进意图，子任务逐一按同样条件关闭；未收束子任务仍计容量。

取消只要求本次有效身份有 flows:write 且属于任务 CallerID，不额外要求原执行 Key／目标 Agent 调用权仍有效，因为它不发出业务动作；同 caller 的另一枚有效 Key 可以关任务，不能因此替换原执行授权去 resume。没有任何有效 caller Key 时由管理 abandon 收口。

**截止覆盖全部墙钟时间**：从根任务首次持久接受开始的 1800s，包含 queued、blocked、等待人处理、服务停机和重启间隔；不暂停、不重置，子任务继承根截止并可再收紧。无用户取消／管理放弃意图时，deadline 禁止新的 dispatch 许可；本地执行已收束的 queued／running／blocked 可关闭为 stopped（deadline），unknown 动作及预留仍原样保留。running 的叶子尚未退出时先保留 running 或 blocked、写 close_intent=deadline 并取消其执行 context，等待实际 join；**不能进入专属取消／放弃意图的 cancelling 再转 stopped**。已有取消／放弃意图则只完成该关闭，不被 deadline 覆盖。resume 不续期；需要新目标／更长时间应另建任务并先处理潜在重复风险。

文件协议：`.basalt/flows/<name>/FLOW.yaml`＋`TASK.json`（任务契约模板）。只支持普通文件，拒绝符号链接／重解析点／绝对引用／`..`／大小写同名碰撞；Flow 文件与产物目录不加入 Agent 可写白名单。发布版本按规范化内容 hash 固定；引用 Agent UID＋版本、子流程 UID＋版本，名称只作展示。发布内容保存至 PG，文件更新须再次发布，不为在途任务热替换。

定义字段：`format_version: 1`、`name`、`entry`、`nodes`、`limits`、`task_contract`；node 闭集为 `agent / tool / branch / subflow`。每节点有稳定 `id`、输入映射、输出 schema、成功下一步、失败策略与可选有界返工目标；所有路线可展开到有界步数并最终终结。branch 可读两类可信值：已通过对应校验的结构化节点输出，以及 **C 自有的槽位／证据需求状态、剩余预算和有界尝试计数**。由此支持 `missing/conflict → 补查`；不是让模型自报 missing 就改变任务状态。只允许已声明字段的布尔／枚举／数值与字面量比较，类型不符拒绝，不 eval／不生成代码；引用未知字段、任意来源正文或模型状态字符串的 branch 发布拒绝。

Tool 节点必须指定合法 Agent UID／版本和该 Agent 已声明的工具；不能借“系统节点”获得管理员身份。**Flow Agent 节点只提供可信只读工具子集**；业务写只能由已定义、参数在 dispatch 前冻结并通过验证的受控 Tool 节点执行。参数可由定义中的常量、获准调用方参数及**已验证上游输出**按已发布映射派生；不是要求作者预先写死所有业务输入。Prepare 时固定规范化参数、来源／上游版本及 input hash，BeforeDispatch 再核对；dispatch 后模型不得重选 action／改参数，同逻辑动作的重试／查证使用原冻结输入与身份。未知副作用工具不进入 Agent 节点可用集合，伪造请求仍由 core／Policy 在调用前拒绝；这是运行权限求交，不修改 AGENT.md 的工具声明。协作仍由顺序节点／子流程名单决定，模型不能任意创建子 Agent 或自由递归。

任务契约字段：`contract_version`、`goal`、`scope`、`input_refs`（含来源版本）、`constraints`、`required_slots`、`output_schema`、`artifact_spec`、`acceptance`（规则版本及必需性）、`budget`、`stop_conditions`、`escalation`。模板实例化后产生该任务的不可变契约版本；模型提议不能改 acceptance。首批不支持运行中改验收契约；需要改变目标／验收时停止并由获准管理流程创建新契约／新任务，旧动作事实仍保留。

**契约实例化边界：**发布作者固定 required_slots、acceptance、artifact_spec、output_schema、stop_conditions、授权／目标范围和预算上限；goal／constraints 中可参数化的位置也由定义显式声明。调用方只提交通过 input schema 的参数、获准类型的 input_refs 和可选 `budget_cap`（只能收紧）。禁止 input 与模板控制字段做任意深度 merge，禁止通过同名键、空值／空数组覆盖必需槽位／验收，禁止额外字段隐式提升授权或抬高预算。实例化后的确切契约／有效预算由服务端保存并纳入 hash；模型与调用方都没有削弱验收的通道。

**发布验证硬条件：**每个 required slot 必须绑定至少一条适用的验证规则，规则引用的槽位／目标／输入类型可解析，必需性和 pass 判据明确；不能仅配置一个无规则的 verified 字符串。必需 judgment 必须绑定有版本、有校准记录的可信评分器，或显式要求一条受控人工验证记录，否则拒绝定义。首批没有通用人工验证提交／审批服务：要求人工记录的定义可以结构校验通过，发布结果必须明确标注必需人工能力尚未提供；执行到该关口只能 blocked（human_verification_required），不能转 pass／自动续跑。首批可成功验收的固定任务不能依赖这种未提供能力，不能静默删掉该规则。

`escalation` 只表示“停止并向人展示未解项／处理建议”；不建立审批单、不自发外部通知、不等待 H 未来自动批准后恢复，不赋予人修改验收契约的隐含入口。现有 resume 只核对已知状态，不能替代缺少的人工验证记录。

`TaskRevision` 只允许在既定路线内改输入、选择获准分支、补查证据与有限返工；不可重新命名已发出动作绕过 unknown。任务摘要携带 goal／hard_constraints／progress／unresolved／accepted_artifacts／basis_refs／task_revision；这是 C 状态的有界派生视图，不是第二个状态推进者。

## 2. 关键类型与方法签名

以下为接口级设计，不写方法体。辅助请求／结果类型在同一文件中定义并由正式契约冻结字段；不得留下未定名词直接开工。所有写方法含身份及关联参数，所有预算／Profile／任务上下文都显式传递。

```go
// internal/flow；仅依赖 core 的纯类型、标准库和本包消费端口。
type TaskVersion struct { Revision, StateVersion, ExecutionEpoch int64 }
type StartRequest struct {
    Principal core.Principal
    RequestID, IdempotencyKey, DefinitionUID string
    DefinitionVersion int64 // 0=服务端解析当前版本；正值=必须等于当前版本
    Input json.RawMessage
    BudgetCap *BudgetCap // 只收紧；解析后的有效预算进入契约和幂等 hash
}
type AdvanceRequest struct {
    Principal core.Principal
    TaskID string
    Expected TaskVersion
}
type CancelRequest struct {
    Principal core.Principal
    TaskID, RequestID, OperationID, Reason string
    Expected TaskVersion
}
type ResumeRequest struct {
    Principal core.Principal
    TaskID, RequestID, OperationID, Reason string
    Expected TaskVersion
}
type AbandonRequest struct {
    Principal core.Principal // 必须为最小管理认证身份；不能用 caller key
    TaskID, RequestID, OperationID, Reason string
    Expected TaskVersion
}
type RevisionRequest struct {
    Principal core.Principal
    TaskID, OperationID string
    Expected TaskVersion
    Proposal json.RawMessage // 必须按已固定修订 schema 校验，不是任意 patch
}
// ErrTaskClosed：领域错误，用于取消／放弃／终态的恢复拒绝；HTTP 为 FLOW_CLOSED。
func (l *Loader) Load(ctx context.Context, dir string) (Definition, error)
func (v *ContractValidator) ValidateDefinition(ctx context.Context, def Definition) error
func (p *Publisher) Publish(ctx context.Context, req PublishRequest) (DefinitionVersion, error)
func (p *Publisher) Retire(ctx context.Context, req RetireRequest) (DefinitionVersion, error)
func (s *Service) Start(ctx context.Context, req StartRequest) (Task, error)
func (s *Service) Advance(ctx context.Context, req AdvanceRequest) (Task, error)
func (s *Service) Cancel(ctx context.Context, req CancelRequest) (Task, error)
func (s *Service) Abandon(ctx context.Context, req AbandonRequest) (Task, error)
func (s *Service) Recover(ctx context.Context, req ResumeRequest) (Task, error)
func (s *Service) Revise(ctx context.Context, req RevisionRequest) (Task, error)
func (w *Worker) Run(ctx context.Context) error
func (v *VerificationService) Verify(ctx context.Context, req VerifyRequest) (Verification, error)

type Repository interface {
    Create(ctx context.Context, req CreateTask) (Task, error)
    Claim(ctx context.Context, req ClaimNode) (NodeAttempt, error)
    Commit(ctx context.Context, req CommitNode) (Task, error)
    Cancel(ctx context.Context, req CancelTask) (Task, error)
    Abandon(ctx context.Context, req AbandonTask) (Task, error)
    Recover(ctx context.Context, req ReconcileTask) (Task, error)
    Revise(ctx context.Context, req AcceptRevision) (Task, error)
    Read(ctx context.Context, principal core.Principal, taskID string) (Task, error)
}
type NodeRunner interface {
    Run(ctx context.Context, req NodeRunRequest) (NodeRunResult, error)
}
```

`PublishRequest` 含管理 Principal／OperationID／RequestID／预期发布版本／已校验字节及 hash；`RetireRequest` 含管理身份／OperationID／RequestID／目标 UID＋版本／预期指针与非空理由，走图 A 同一审计／条件指针路径，不删除内容。`CreateTask` 含 StartRequest、解析后当前定义／契约／授权／有效预算及 hash；`ClaimNode` 含 task／version／node invocation／attempt／预先生成的 RunID／预算分配；`CommitNode` 含上述身份、目标版本、输出 hash、证据／结论／产物／验证引用与实际／未知用量。

`CancelTask`／`AbandonTask` 均含对应请求身份、task、Expected、操作 ID／理由、拟写入的单调关闭意图及本地执行收束证明；`ReconcileTask` 含原授权、Expected、检查点／动作／截止核对证据与拟转移，**禁止清取消位**；`AcceptRevision` 含原身份、Expected、旧／新 revision、固定契约版本、提议校验／依赖闭包和预算不增证明。每个 Repository 写方法原子校验旧状态／state_version／revision／epoch／关闭意图，写目标状态＋转移＋操作事实并处理提交查证；不能 Read 后无条件 UPDATE。关闭根任务对有界子树的意图写入有一致锁序及故障测试，不通过每次查询触发补关闭。

`VerifyRequest` 必含原 Principal、task／revision、node attempt、目标结论或产物版本、契约与规则版本、原始观察定位；`Verification` 含验证者类别与标识、时间、pass／fail／inconclusive 及依据。来源有效性在 flow 消费端声明窄端口，由 app 调既有 `knowledge.SourceViewer.Check(ctx, principal, ref)` 适配；flow 不 import knowledge／storage／app。Read 方法只查询，列表／分页在消费端另列只读端口，不让 Worker 借 GET 执行业务。

```go
// internal/core：新增；core 循环／ToolExecutor 是下列预算／动作端口的消费方。
// 接口在 core；具体实现位于其它内部包，经 app 适配接线；analyze 核对依赖无环。
type FlowRunContext struct {
    TaskID, RootTaskID, ParentTaskID, ParentRunID, NodeInvocationID, NodeAttemptID string
    TaskRevision int64
    AgentUID, AgentConfigHash, ResourceManifestHash string
    Authority FlowAuthority
    Budget AncestorBudget
    Actions FlowActionLedger
    Progress ProgressGuard
}
type SchemaValidator interface {
    Validate(ctx context.Context, schemaHash string, input json.RawMessage) error
}
type ProgressGuard interface {
    BeforeCall(ctx context.Context, call ToolCall) error
    AfterCall(ctx context.Context, call ToolCall, result Result, outcome ActionOutcome) error
}
// RunRequest / RunEnv / ToolEnv 新增 Flow *FlowRunContext；非 Flow 为 nil。
// RunRequest 新增 FixedTool *ToolCall：仅可信 app NodeRunner 构造，不进入通用 Run HTTP DTO。
// ToolSet 的收紧由 FlowAuthority 在 core 准入时求交，不能从提示词取授权。
```

`FlowAuthority` 是纯数据：原调用方归属／授权来源＋本任务接受时的 scope／目标 Agent／工具／知识集合上限。每个叶子按既有 Principal 当前授权与目标 Agent 当前资格再次求交；服务进程不能把 caller-key 改为 admin-builtin。存储中保留原 `authorizing_kind`／Key ID，后台 actor 使用现有合法执行者类别，不凭后台运行就补管理员。ParentRunID 只有真实父 Run 时才填，根任务与静态子流程没有父 Run 就留空，不伪造关联。

固定 AgentUID／AgentConfigHash／ResourceManifestHash 在 Run 准入与快照构建时核对，不能先查当前注册表再假称使用旧版。当前已发布输入仍与冻结清单一致时正常准入；若无法从受控版本安全构造旧运行环境，则 blocked，需经获准输入修订明确接受新版本，并重新验证权限／依赖。不能按名称自动绑定重建 Agent、资源或换版凭据；休眠任务持有的恢复引用也纳入 F17 清理保护。

`AncestorBudget` 提供 `Reserve(ctx, runID, actionID string, estimate UsageEstimate) (BudgetTicket, error)`、`Settle(ctx, ticket BudgetTicket, actual UsageActual) error`、`KeepUnknown(ctx, ticket BudgetTicket) error`。未知量保留预留；已知超量形成占用并阻止后续新分配。Flow 祖先预算与调用方周期配额是两道约束：同一次模型调用各扣各自维度，但不在 callers 同一维度重复预留。执行名额只在叶子 Run 期间获取，任务接受额度另计；Q1-A 的持久名额由数据库任务状态计算，不能重启丢失。

`FlowActionLedger` 提供 `Prepare(ctx, req ActionRequest) (ActionTicket, error)`、`BeforeDispatch(ctx, ticket ActionTicket, auditID int64) error`、`RecordOutcome(ctx, ticket ActionTicket, outcome ActionOutcome) error`。ActionRequest 含 root task／node invocation／逻辑序号、工具／适配器版本、规范化参数 hash、恢复类别；ActionTicket 带稳定 action ID 和独立 attempt ID。`BeforeDispatch` 必须以执行代次、取消位与预算再次授权，将原工具审计 ID 绑定同一动作尝试并成功持久标 dispatching 后才允许外部调用；先写 action／调用 pending，任一步失败零调用。调用终态写入仍唯一归现有 Provider／ToolExecutor，Flow 仅关联，不另建模型／工具审计表。此接口细化来自 2026-11-03 实际 Claude GFT-01／02／03 条件回执；ActionOutcome 的 AuditID 使用既有审计 int64，携 Dispatched／AuditConfirmed 与结构化结局，观察持久化失败不得调用进展更新。

固定 Tool 节点经 app 同一 `runAdmission.run` 后进入 `AgentService.Run` 的受控 FixedTool 分支，完成相同身份／快照／Run 准入；由 core 循环共享的消息与工具组助手生成唯一 assistant 工具调用、执行并回填，Flow 不直接写 messages。该模式不调用模型，不绕过 ToolExecutor。实现时提取共享助手而非复制循环／追加逻辑；新增调用点审查证明正常消息写入仍唯一归 core 循环。若该改动被 review 判定超出可控范围，必须回 Q2 调整，不能用 flow 直调工具凑功能。

**FixedTool 不是免费内部调用。** 每个新 FixedTool Run 经原 RunQuota 按准入记 1 个 caller 任务次数（run:<run_id>:admit），并占同一实例／caller Run 并发名额；祖先任务和 Run 的 max_tool_calls 在发出前共同约束实际工具尝试。一次正常执行且结果已回填的固定工具调用，RunResult.ToolCalls=1，根／祖先工具额度也只记一次；重试形成新 Run 时仍按原 Run 配额收费，不能因 action ID 不变豁免新 Run，亦不能在同一计量维度重复扣费。失败／取消按原共享路径收尾，已发出结果未知的动作和预算保持占用，消息回填失败不能使实际执行免计。

无模型调用的固定节点 RunResult.Iterations=0，不写虚假 llm_calls、不预留／结算一次不存在的 Chat Token；Run 用量仍如实报告工具次数、耗时／状态及其真实审计。若工具内部访问第三方服务，只能声明本次 Basalt 没有 Chat 调用，不能推断第三方内部免费或伪造 Token。以上计量口径必须进入 spec 与 F04，检验 max_tool_calls 耗尽时零工具发出、caller 任务额度耗尽时零执行、重试计费与幂等不重复记账。

**G-FT 硬门禁与独立设计说明：**写 Tool 路径实施前，先将以下调用边界及必要隔离探针结果交独立 reviewer（用户指定 Claude）核对；当前尚未通过该门禁，不将 R1 作者自检当通过。允许先完成独立设计／临时探针和共享助手提取验证；禁止在门禁未闭合时交付或启用写节点。

| 阶段 | 固定 Tool 设计 | 普通 Agent 的必须保持项／负向证据 |
| --- | --- | --- |
| 输入与准入 | FixedTool 只由 app 已验证的节点构造；HTTP DTO／模型不能设置；相同 Principal、Agent UID／快照、祖先预算及共享 runAdmission | 缺身份／越权／伪造 FixedTool 零工具调用；普通 Run 不因新增字段改变同步与忙即拒 |
| 消息追加 | 抽取 core 循环已拥有的同一组装／追加 helper；固定节点调用一次 helper 生成 assistant(tool_calls)，再用同 helper 回填唯一 tool 结果 | 普通 Agent 仍由模型响应驱动同一 helper；成功／失败／空 content／多工具／取消各只有一条相应消息，不出现 app／flow 第四写入口 |
| 执行与审计 | 只有 ToolExecutor 发出调用；动作许可与调用 pending 均已成功，才执行；Flow 不直接调用 Tool.Execute | pending／动作许可失败调用为 0；一调用一审计行；取消与崩溃半截组由原合成路径补齐，不能产生无 assistant 的孤儿 tool |
| 结束与恢复 | Run、配额、消息终态按原入口收束；node 产物经独立验证后 CAS；unknown 不重放 | 断开完成记录故障下，实际调用集合和历史条数不增长；非 Flow 调用与固定 Tool 的权威路径相同 |

G-FT 分两步闭合：**设计关口**在写分支实施前完成独立设计／探针复核；**行为关口**在共享助手代码完成、写节点可交付前以 F04／F12／F17 的真实负向回归和普通 Run 兼容证据通过。探针只用临时工作区／mock／真实临时 PG，不外发业务写。门禁失败则停止写节点并返回 Q2 评审只读 Tool 回退与写动作后移；只读不是原则 II／V 豁免，若共享助手仍不能守住唯一追加／预写审计，则只读 Tool 也不能旁路上线。不能通过删测试或把写操作藏进 Agent／子流程绕过门禁。

## 3.  PG 记录与一致性

以下是逻辑／约束级 schema 提案，正式列类型与 DDL 在 Q3 通过后写入 `schema.pg.sql` 和新迁移；不改 SQLite 谱系。所有时间沿用 BIGINT Unix ms，IDs 沿用本仓 text 身份风格，版本／计数使用有上界整数，JSONB 必须带 format_version 并经程序校验。

| 表 | 最小记录／键 | 必须有的约束 |
| --- | --- | --- |
| `flow_definitions`、`flow_pointers` | definition_uid／name／version／content_hash／content；按 UID 的当前发布指针和 retired 状态 | 部署级公共定义，不设 caller owner／ACL；UID＋version 唯一、不可变；退役不删旧版，指针条件更新，新任务只取 current active |
| `flow_task_contracts` | task_id＋contract_version；内容／hash／definition_uid＋version／可信 scope | 不可变；与任务／revision 同作用域；删除任务不能自动连锁删恢复证据 |
| `flow_tasks` | task_id、root／parent、CallerID／授权来源／Key ID、状态／state_version／epoch／current_revision、单调 cancel_requested／close_intent、截止、请求键／解析后 hash | `(caller_id, idempotency_key)` 唯一；条件更新与意图 CHECK；终态无活跃本地执行；parent 同 caller；根与子都计容量 |
| `flow_task_revisions` | task_id＋revision、契约／定义版本、当前节点、输入版本／hash、槽位、摘要、剩余预算视图、修订原因 | 一任务一当前头；指针 FK 可延迟但提交时必须完整；旧 revision 不改写；剩余预算不作独立第二账本 |
| `flow_transitions` | task_id＋seq、旧／新 state_version／revision、operation_id、原因与时间 | 同 task 单调 seq；operation_id 幂等；状态与转移同事务 |
| `flow_node_attempts` | task／node_invocation／attempt、revision／epoch、run_id／session_id、状态、输出／hash、Observation 引用、开始／终止 | 同一 node invocation 同时只有一个活动尝试；RunID 预先持久化；非执行分支没有伪 Run |
| `flow_actions` | 稳定 action_id、task／node invocation／logical slot、工具／输入 hash／恢复分类、结果 | 同 task＋invocation＋logical slot 唯一；同 action 不得换输入；unknown 阻断该逻辑槽重放 |
| `flow_action_attempts` | action_id＋attempt、run_id／tool_invocation_id／llm_call_id、dispatch 及结果／查证依据 | 与实际调用一一关联；独立尝试不改变逻辑 ID；一次尝试不能同时映射两条实际调用 |
| `flow_evidence_refs` | task／evidence_id、source kind／UID／版本／位置／hash、获得它的 Run／node／attempt | 来源定位完整，来源内容不复制；同来源转述不当独立证据；每次使用重查当前权 |
| `flow_claims` | task／claim_id／claim_revision、text 或受控内容引用、状态／目标 task_revision | 结论版本不可静默改；接受必须具备完整依赖与适用验证 |
| `flow_claim_evidence`、`flow_claim_dependencies` | task／claim revision→evidence 或上游 claim revision | 复合 FK 限定同 task／scope；禁止自环及有向环；上限与遍历预算先验 |
| `flow_artifacts`、`flow_artifact_claims` | task／artifact_id／version、内容 hash／格式／内容或安全引用、关联 claim revisions | 产物版本不可变，终结输出接受时重验依赖；首批小产物存 PG，不新增对象存储 |
| `flow_verifications` | task／verification_id、目标 kind／ID／version、规则与验证器版本、验证者、类型、结果、时间、依据 | 目标版本精确匹配；旧通过不自动转移；验证事实 append-only |
| `flow_budget_allocations` | task／ancestor allocation／ticket、维度／预留／实际／未知、对应 Run／action | 幂等票据；父预留与子分配原子收紧；未知不释放；与 callers 配额表各自分责 |

TaskRevision JSON 内的槽位／摘要与 NodeAttempt 的 Observation 仍按正式格式验证，不因未各建一表而省掉版本、权限与关联。输出和其证据／结论／验证引用在一份 C 短事务内提交；B 内容不在此事务改写。状态查询索引至少为 `(caller_id, status, updated_at, task_id)`、待领取状态／时间、task／node／attempt、动作未决与全部反向依赖；以真实 EXPLAIN／容量数据验证，不虚报索引收益。

定义／契约版本是恢复依据：本 US 不自动清理任何已发布定义或已接受契约版本。当前发布指针、活动任务、检查点、原幂等接受记录、unknown 动作或未决配额引用的版本全部受 FK／清理保护；任务 cancelled／abandoned 不解保护。退役只禁止新任务接受，不破坏已接受任务的旧输入核对；原授权和资源资格仍逐次复议。管理面只有退役／发布，没有硬删定义版本或清除 unknown 的入口；旧版保留无时间到期自动删除。

新迁移版本由实施基线当前版本＋1 决定：若仍为 v7，则 `0008_flow.sql`／v8；若其他在途迁移先合入则顺延，并同步配置与测试。迁移前备份，旧二进制遇新 schema 拒绝启动；回退仅恢复升级前数据库／工作区／实例私有目录配对快照，不能裸降级。新建／升级 DDL 对账，已发布迁移哈希保持。

## 4.  API、CLI 与界面

**R1 统一 scope 为 `flows:read / flows:write`**，对齐现有 `sessions:read/write`、`knowledge:read/write` 的命名。R0 的 `flow:read` 对应 flows:read，`flow:run/cancel/resume` 统一对应 flows:write；这些从未成为正式 scope，不提供旧别名。read 用于公共定义／自己的任务读取；write 用于自己任务 start／cancel／resume，**不**获得定义发布／退役或管理 abandon 权。现有 `agents:invoke:*` 等目标 Agent 权限仍须同时满足。更新 scope 闭集、发 Key／编辑选项及相关契约，不给旧 Key 自动加权；local-admin 不能冒充原 caller-key 续跑。

**B3 版本与可见性绑定：**定义是部署级公共资源，所有持有 flows:read 的调用方都能看到当前 active 定义的安全元数据／input schema／公开契约说明，不新增调用方定义 ACL，也不暴露 Agent 正文、资源／来源敏感值。公开可见不代表获准执行定义引用的 Agent／工具／知识；Start 和每叶子另查原身份当前授权。任务、输入、来源与产物依旧按 caller 隔离，跨 caller 404。

新幂等键的 Start 只允许**当前 active 发布版本**：DefinitionVersion=0／省略由服务器解析当前版本，正值必须等于它；负值 400。未知 UID 或未知版本 404（FLOW_DEFINITION_NOT_FOUND／FLOW_DEFINITION_VERSION_NOT_FOUND）；已知退役定义／版本 409 FLOW_DEFINITION_RETIRED；保留但非当前的旧版本 409 FLOW_DEFINITION_NOT_CURRENT，拒绝时零新任务／预算／业务调用。解析发布指针与接受任务在同一短事务中条件固定，切版竞争只接受一个确切 current 事实。

**幂等例外是回读旧接受事实，不是允许新任务钉旧版。** 先按 `(CallerID, Idempotency-Key)` 查接受记录；已有键时，省略版本绑定首次接受版本，显式版本必须与首次版本一致。用首次冻结的定义 UID／版本／契约模板和输入参数、有效预算、执行相关选项计算规范 hash，匹配则回原任务（即使定义已切版／退役／任务已关闭），不重新准入执行或预留。不同输入／显式版本冲突则 409 FLOW_IDEMPOTENCY_CONFLICT。无键记录才解析当前指针；并发首次创建由唯一键收敛，败者按胜者接受版本核对。hash 基于**服务端解析后的语义版本／有效预算**，不直接 hash 原始请求或用当前新版去重解释旧输入。

| 接口 | 权限／输入 | 响应语义 |
| --- | --- | --- |
| `GET /api/v1/flows`、`GET /api/v1/flows/{name}` | flows:read；部署级公共 current active 定义的安全投影 | 有界分页；退役／未发布不列，by-name 未命中 404；不附任务或来源正文 |
| `POST /api/v1/flow-tasks` | flows:write；Idempotency-Key；definition UID／version＋input／budget_cap | Q1-A 持久接受 202，原任务幂等回读 200；按上文版本／hash 规则，零无确认接受 |
| `GET /api/v1/flow-tasks`、`GET /api/v1/flow-tasks/{id}` | flows:read＋归属 | 跨 caller 404；含 task/state/revision、预算、关闭意图／unknown 和证据缺口 |
| `GET /api/v1/flow-tasks/{id}/artifacts`、`.../verifications` | 同归属＋当前证据访问权 | 产物版本／hash／验收状态；来源撤权不回显旧敏感正文 |
| `POST /api/v1/flow-tasks/{id}/cancel` | flows:write＋归属＋If-Match＋操作 ID | blocked 可取消；已无本地执行则 200 cancelled；仍在途 202，保留意图；unknown 不改 |
| `POST /api/v1/flow-tasks/{id}/resume` | flows:write＋原授权有效＋If-Match＋理由／操作 ID | 先拒取消／放弃／终态／过期；unknown 无查证阻断，不重放 |
| `GET /api/v1/admin/flows`、`POST .../validate`、`POST .../publish`、`POST .../{uid}/retire` | admin token／local_trust；拒 caller key | 校验只读；发布／退役 pending 管理操作＋条件写；退役保留旧版本 |
| `GET /api/v1/admin/flow-tasks`、`GET .../{id}` | 最小管理认证；安全管理投影 | 跨任务运维查看不等于获得原调用方来源阅读权；正文按可证明权限返回，否则仅状态 |
| `POST /api/v1/admin/flow-tasks/{id}/abandon` | 管理身份＋If-Match＋操作 ID＋非空理由；原 Key 可已撤销 | 持久禁止推进，仍活返回 202；执行收束后 200 abandoned；不重放／不伪造成功／不解未知预留 |

所有响应复用现有 envelope、RequestID、Host／Origin／body 限制。除上文定义版本错误外，错误为 400 `FLOW_DEFINITION_INVALID / FLOW_SCHEMA_UNSUPPORTED`，409 `FLOW_STATE_CONFLICT / FLOW_ACTION_UNKNOWN / FLOW_CHECKPOINT_UNSUPPORTED / FLOW_IDEMPOTENCY_CONFLICT / FLOW_CLOSED / FLOW_DEADLINE_EXPIRED / FLOW_ROOT_BUSY`，412 `PRECONDITION_FAILED`，429 `FLOW_CAPACITY_EXHAUSTED / FLOW_INTERACTIVE_RESERVE`；认证／归属／限流／配额／存储错误沿用原分类。`FLOW_ROOT_BUSY` 只由 `resume` / `revise` 在需要把根推进（写 `queued`）而 `flow_root` 租约已被执行者持有时返回，**不重试、不降级成无租约写入**（见文末「US-24 分布式执行补充」）。任务内停止原因在 data 呈现。If-Match 为完整条件版本 opaque ETag，缺失／过期 412 零写入。

列表使用稳定游标＋limit（默认 50，上限 100），不以 offset 越来越慢或无界响应代替；任务展开的节点／产物／验证分别分页并显示截断。重新读取不触发自动恢复。取消／恢复必须具有操作幂等身份，网络超时后 UI 只查证原操作，不能换键再提交。发布操作必须说明影响新任务，不能静默替换在途版本。

发布／在线校验的 `definition` 为完整 bundle：`FLOW.yaml` 的定义字段加上严格解析 `TASK.json` 得到的 `contract`；不接收服务端文件路径，不在服务端读取任意宿主目录。离线文件解析仍拒绝在 `FLOW.yaml` 内嵌 `contract`。管理任务读取只投影身份、状态、版本、关闭意图、unknown 与原节点尝试关联，不附任务输入、目标正文、来源、产物正文或原授权 Key；未提供的预算不伪装为零。

CLI ：`basalt flow validate <dir>` 只做离线结构校验；`basalt flow list / start / status / cancel / resume` 为服务 API 客户端，用 `--server` 指向活实例，不通过全量装配取得第二写锁。token 从受控文件或交互输入取得，不进 argv／日志；start 的幂等键可由 CLI 生成并先向用户显示，超时重试复用同键。退出码沿用 CLI 现有成功／调用错误风格，在正式契约冻结时逐项映射，不把任务 stopped 当业务成功。

管理台新增「流程」与「流程任务」入口：定义列表→导入／只读校验→查看差异和版本→发布；任务列表→选择定义及结构化输入→提交→详情。详情显示任务契约／revision、顺序节点／子 Run、未解槽位、已接受／失效结论、预算、验证结果及产物，链接复用 OBS／Run 查询。取消／恢复显示动作依据与后果；无依据的 unknown 只给运维处置说明，不提供强制成功或重放。

blocked 自有任务提供“取消并关闭”，管理投影提供“放弃任务”并要求理由、展示剩余 unknown／未知预留；执行未退出时展示“正在停止，尚未释放执行名额”，不能显示已关闭。定义管理提供退役确认，明确禁止新任务、保留历史版本。任务关闭后仍展示未知动作状态和人工对账说明；不出现强制成功按钮。

轮询复用现有可见性／focus／离开／登出清理规则：活跃任务页建议 2s、终态停止、隐藏停止；同时仅一个在途请求，过期响应不能覆盖新 revision。网络错误保留最后数据及更新时间，未知显示未知；按钮重复点击、确认取消、403／404／409／412／429／5xx、空列表／未记录／query_failed 均需操作测试。

## 5. 动作安全、Schema 与循环规则

**动作安全分类只由受信注册／适配器声明，模型、Flow 作者或 tool 文本不能自报“安全”。** 默认未知工具按可能写／不可查证处理。首批只读动作可按次数／预算重试；业务写只经 G-FT 后的受控 Tool 节点及可信适配器。幂等／查证能力须有真实适配器与同请求键测试；未知写不能仅因下游声称幂等就自动重发，必须先查证到已完成或有证明未产生副作用。查不明则 blocked，不进行恢复性业务写；查证本身为受限只读动作，同样受授权／审计／预算约束。HTTP 2xx、工具 Retryable、方法叫 GET、模型说“没成功”均不是通用副作用证明。

动作稳定身份由服务器在首次执行前分配，并持久关联 root task＋node invocation＋逻辑槽位＋输入 hash。attempt、Run、revision 不进下游幂等键；重试只换 attempt。同一逻辑槽输入变化必须先证明不是未决写动作；unknown 永远不能通过新 revision／新 action ID 绕开。明确新业务意图需新任务并告知可能重复，由人裁决，不自动执行。

**Agent 节点不能重推理以复现写动作。** 当前正常 Agent 节点仅允许可信只读工具；若旧检查点／异常审计显示它曾发出可能写动作，则按违规／未知阻断，不套只读重试。中断节点只有所有已发生动作均为可信只读且预算允许时才重跑；已完成节点可复用。受控 Tool 节点只使用 dispatch 前冻结且已 hash 的输入（允许由已验证上游派生）、原动作身份，在可信查证解除未知后依规则推进，不让模型重新决定 action 序列。新状态“关闭”不是查证，不解除动作未知。

Schema 子集：`type` 单类型（object／array／string／integer／number／boolean／null）、`properties`、`required`、`additionalProperties` 布尔、`items` 单一 schema、`enum`、`const`、`minimum / maximum / exclusiveMinimum / exclusiveMaximum`、`minLength / maxLength / minItems / maxItems`；`title / description` 仅注释。每个 object 显式声明 additionalProperties 策略，首批任务／输出对象推荐 false。禁止远程引用与 `$ref`、递归、组合关键字、pattern／format 执行及其它未实现关键字；不支持时发布拒绝而非忽略。

原始 JSON 拒绝重复键、非有限数、超深、超大及尾随第二文档；用 `json.Number` 或精确比较保留大整数与数值边界，不经 float64 舍入。错误给安全字段路径／规则，不回显敏感值。输入／输出／模型观察三面都校验；工具 schema 在固定版本注册／Flow 发布时编译，使用 hash 缓存只读编译结果；修改工具 schema 不让旧任务悄悄使用新规则。校验器缺失／编译失败在外部动作发出前失败关闭。

循环规则：每个 root task 的每个节点有固定窗口，工具名＋规范化参数 hash 识别重复／交替；结果 hash 是辅助证据，**不单凭字节变化认为有进展**。进展来自经程序验证的新来源／槽位状态／结论状态，或可信工具的单调分页游标／处理位置；随机时间戳、重复来源、模型改写不算。合法轮询必须在定义中声明次数／总时间／退避与可验证进度；没有进度时仍受 no-progress 上限。节点结束可以结束该节点窗口，但 root 总预算／累计返工与已见证据不清零；同节点返工／新 revision 恢复同一历史。

BeforeCall 在发出新工具前检测，AfterCall 在结果与观察保存后更新；连续重复／交替阈值按调用计数，达到阈值的该次在发出前拒绝（3 次重复最多实际发出前两次，6 次交替最多前五次）；无进展按完成观察计数，连续第 6 次无进展后停止下一调用。触发时回填明确停止说明并补齐工具组，Run limit_reached，Flow stopped／blocked 按真实原因区分，留下窗口中调用身份与未解槽位。受保护的原始参数不进日志或导出，诊断仅给工具名／hash／reason／边界计数。

## 6. 配置、硬限与质量阈值

 `basalt.yaml` 新增 `flows` 段，默认 `enabled: false`，只有 `serve` 配置启用 Q1-A worker；`chat`／`gateway` 不暗中启动 Flow Worker。定义发布、任务数据与逻辑预算在 PG；配置只限定部署上限。有效额度 = 部署上限 ∩ 契约额度 ∩ 所有祖先剩余额度 ∩ 既有 Run／调用方额度，输入不许放大上限，未配置的关键预算拒绝而非不限量。

| 项 | 建议默认／硬限 | 验收含义 |
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

性能基准：记录真实 CPU／内存／OS／PG／二进制与配置 hash，以 4 active、32 open、每任务 20 节点、mock 固定 50ms 调用、200 次独立任务提交测量；接受接口 p95≤2s，状态读 p95≤1s，可运行任务从可领取到开始的 p95≤1s（无活跃槽排队时），资源无持续增长。报告 p50／p95／p99、失败率、峰值内存、PG 连接／锁等待和队列等待；外部模型时延与协调开销分列，不能把这些建议写成已测生产 SLO。

恢复目标：在隔离基准 32 个 open 任务／有界历史内，依赖健康且可证明安全任务的启动核对≤30s；unknown 判为 blocked 也算核对完成，不能算业务恢复成功。已接受状态以 PG 持久化为权威；RPO 依赖实际数据库持久化与备份设置，本文不承诺基础设施故障零丢失。取消有界响应与实际动作停止分别测量，不声称取消能撤回已发送调用。

效果集：12 项脱敏固定资料任务，涵盖报告链、择一分支、有界双 Agent 交接、空证据、冲突版本、撤权、无进展、错误输出、取消、预算耗尽、已完成检查点复用、unknown 写动作。固定输入／资料版本／验收器／定义和单 Agent 基线版本，模型不改评分器；报告可交付率、必需槽位完整率、引用支持、错误成功率、Token／时间／协调开销。安全／停止样例按各自预期评价，不能为了提高“完成率”把应停止样例写成功。

本地 deterministic mock 的硬门槛为 12／12 预期行为正确、错误成功 0、非法调用 0；仅证明控制协议。开放质量另取同一批适用资料任务，至少三次配对运行并报告逐题结果、样本数／不可评和人工校准，真模型依 Q5；不预先承诺 Flow 质量或费用优于单 Agent，不以少量样本宣称统计显著。judge 缺失、校准不足或验证错误不能记 pass。

## 产物只读投影的原因分类

产物 `state=accepted` 表示该版本已记录，不单独承诺整个任务验收通过。正文仍须通过当前来源授权及原验收尝试的依据核对；任一项不满足即返回 `readable=false`、省略 `content`。当前来源不可读或版本无法核对用 `current_source_access_required`；当前授权可读、但原节点验收依据尚未确认用 `artifact_basis_unconfirmed`，不得把缺少核验依据误称为调用方无权限。任务最终结果仍以 Task.status 及全部必需验证为准。

## R4 工作流作者读取面

用户于2026-11-03将可视化编排／业务演示纳入US19。加性 `FlowAuthoringBackend` 与原 `FlowBackend` 分离，由同一 app 适配实现；缺失端口失败关闭。

- `GET /api/v1/admin/flow-agents?cursor=<name>&limit=50`：仅 admin；limit 1–100，默认50；cursor为上一页末项名称（最长128字节）。按名称排序，最多读取当前页100项的当前生命周期与已发布快照；不执行Agent／工具／模型／MCP连接。`FlowAgentPage` 为 `{items: FlowAgentView[], next_cursor?: string}`。每项 `{name, display_name, description, eligible, reason, agent?: FlowAgentRef, knowledge_collections: string[], memory_scope: agent_only|global_and_agent|legacy_default, readonly_tools: string[]}`。reason闭集为 `ready`／`agent_unregistered`／`agent_disabled`／`resources_unavailable`；仅ready返回当前不可变引用。memory_scope仅提示核心记忆层选择配置，不证明内容存在／本次注入或归档召回可用。提示不授予权限，发布与执行仍重新核对。
- `GET /api/v1/admin/flows/{uid}/versions/{version}`：仅admin，version为正整数、精确到固定版本；回`FlowAuthoringVersion`：`{definition_uid, version, content_hash, retired, published_at, bundle_json: string}`；`bundle_json`承载原版本完整规范化作者bundle（含contract），不经JS Number中转，无任务输入／产物／检查点。找不到固定版本404，不回退current。包含历史退役版本供导出，返回该版本原事实。

两路均沿原认证、Host／Origin及读取规则；403不包含作者内容。现有业务 `/flows` 仍仅安全投影，不加完整定义字段。流程草稿只在用户明确下载时写到浏览器下载目录，服务端不新增表／文件写入口／草稿权威。编辑器不支持的分支／子流程／精确数字保持高级原文，禁止经JS数值转换后静默重发。发布仍走原校验、pending审计、版本CAS与operation_id；未知写锁不变。

### 人工验收后的任务检索补充（2026-11-04）

任务列表与详情的安全元数据增加 `definition_name`，取任务创建时固定的定义版本，
配合 `definition_version` 展示用户工作流编号（如 `bid-readiness-matrix-v1 · v1`）；
`definition_uid` 仍保留作为内部唯一标识，不用目标文本替代工作流编号。
普通与管理任务列表支持可选 `definition_name` 精确筛选，空值代表全部版本／全部工作流。
归属检查、工作流筛选、`updated_at DESC, task_id DESC` 排序都在 SQL 分页前执行；
游标携带更新时间和任务 ID，切换筛选应清空游标，旧 ID 单字段游标应从首页重读。
这是实时分页，不承诺跨页快照；刷新任务回到最新第一页，不触发执行或恢复。

## US-24 分布式执行补充

多 Worker 部署下 Flow 的推进权由数据库租约承担（`execution_leases.kind = 'flow_root'`，资源为根任务行）。`Service.Leases.Acquire` 返回 `TookOver`：**接管**（上一持有者已失联、`ownership_epoch` 已推进）必须**先 `Recover` 再 `Advance`**；只有**正常交棒**（`TookOver = false`）才可直接推进。`Recover` 出错时**不释放租约**——释放会让下一轮继续以正常交棒领到同一根、直接 `Advance` 绕开查证；不释放则租约到期后必为 `TookOver = true`，重跑 `Recover`。接管递增 `execution_epoch`，旧代次的提交按既有条件提交被拒。

**三道门各挡一种"还有人在动"的情形，不得互相顶替**：

| 门 | 判据（命中 = 仍有活动） | 命中时 |
| --- | --- | --- |
| ① 活租约 `HasLiveRunLease`（对应 `Isolated` 的反面） | 子树仍有未释放且未过期的 `run` 租约 | 本轮不查证也不推进，交还租约（稳定结论） |
| ② 在途 attempt `HasInFlightNodeAttempt`（对应 `Joined` 的反面） | 子树仍有 `prepared` / `running` attempt；`interrupted` / `unknown` **不计入** | 不 `Advance`；释放后根停在 `blocked`，下一轮由回收器的 blocked 分支接住再做一次 `Recover` |
| ③ 未决动作 `HasUnresolvedAction` | 子树仍有 `outcome ∈ dispatching / unknown` 的动作 | **不得** `Advance`，必须 `Abandon`（**不** Release），把下一轮逼成 `TookOver = true` |

- **不得用 TTL 到期推断子 Run 已退出**：租约判据是 `released_at IS NULL AND expires_at > <db_ms>`，不按时间推测；已完成节点按运行证据复用而不重跑。
- 普通 Worker 以**真实** `TookOver = false` 领到含未决动作的根时，同样不派发新 Run / 动作、**不写任何审计新行**（含 `status = 'pending'` 的预写）——门 ③ 在 `Advance` 之前判决。
- 子 Run 创建（`CreateRunForNode`）在同一事务内校验：① 父根的 `flow_root` 租约未释放未过期（否则 `core.ErrLeaseLost`）；② attempt 归属的父根就是本次固定的 `root_task_id`（持根 A 的租约不得为根 B 下的 attempt 建 Run）；③ attempt 的 `execution_epoch` 与 `task_revision` 等于任务当前值、任务仍 `running`、未关闭 / 未取消、未过 deadline（否则 `flow.ErrFlowAttemptStale`）。`ErrFlowAttemptStale` 是**内部信号**，不进对外错误码表；唯一写入仍是那条带 EXISTS 兜底的单条 `INSERT`。

**两个判据分开，各有收敛职责。** `ReconcileTask.Isolated`、`CancelTask.Isolated`、`AbandonTask.Isolated` 语义逐字相同：`Isolated = true` 时 `Recover` 把**整棵子树**的 `prepared|running → interrupted`；`Isolated = false` 时存储层 **attempt 一字不动**（仍有活执行，fail-closed）。`closeFlowTask` / `propagateFlowClose` / `CloseDeadline` 三处收敛由 `joined` 改判 `isolated`，范围覆盖整棵子树；`CloseState` 仍用 `joined`。deadline 的终态是 `stopped`，**不是** `cancelled`。

**管理面推进类入口。** `resume`（`Service.Recover`）与 `revise`（`Service.Revise`）的请求里没有租约；需要把根写回 `queued` 时由 `withRootLease` 自取一个 `flow_root` 租约。`Acquire` 返回 `core.ErrLeaseHeld` 映射 `flow.ErrFlowRootBusy` → HTTP **409 `FLOW_ROOT_BUSY`**，**不重试、不降级成无租约写入**，`flow_tasks` 一行不改；租约释放后重放同一请求才开始成功。`Blocked` 写入不是推进类、不需要租约，管理面照常可用。

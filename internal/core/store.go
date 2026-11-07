package core

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// 本文件是运行面的**消费方接口声明**：core 需要什么就声明什么，实现分别落在
// internal/storage（四个仓储）与 internal/tool（Resolver / Policy / CommandRunner）。
// 接口定义在消费方是依赖倒置的落点——引擎不 import 任何实现包（宪法 VIII）。
// This file declares the consumer-side interfaces of the run surface. Implementations
// live in internal/storage and internal/tool; the engine imports no implementation
// package (constitution VIII).

// MessageStore 是会话历史的写透接口：内存里的 Session 经它落库。
// **追加接口显式承载 `runID`**——Message 本身没有 Run 字段（契约 DDL 事实），
// 而崩溃恢复要按 `messages.run_id` 定位半截组；缺了它，恢复无从判定。
// MessageStore is the write-through interface for session history. AppendMessage takes
// runID explicitly: Message carries no run field, yet recovery locates half-groups by
// messages.run_id — without it recovery cannot decide anything.
type MessageStore interface {
	// AppendMessage 追加一条消息，`runID` 落 `run_id` 列（system 注入类可为空串 → 列写 NULL），
	// 并返回该行的持久化标识（`messages.id` 的十进制串）供调用方回填 `Message.ID`——
	// 装配记录的 `FirstDroppedMessageID` / `CutMessageID` 要的是**真实**主键，不得事后猜。
	// 实现**不得**在拿不到标识时返回空串加 nil：那种"看起来成功"的写入会让历史缺身份。
	// AppendMessage appends one message and returns its persisted id (the messages.id
	// decimal string) so the caller can stamp Message.ID. An empty id with a nil error is
	// forbidden: that would leave history without identities while looking successful.
	AppendMessage(ctx context.Context, sessionID, runID string, m Message) (string, error)

	// Messages 按 `seq` 升序取回整个会话历史。
	// Messages returns the full session history in ascending seq order.
	Messages(ctx context.Context, sessionID string) ([]Message, error)

	// UpsertSession 写入或刷新会话行（不存在即建、存在则刷新最后活动时间）。
	// UpsertSession inserts or refreshes the session row.
	UpsertSession(ctx context.Context, rec SessionRecord) error

	// ListSessions 列出会话（`session list` 与 `GET /sessions` 的共用读面；排序口径由
	// 实现固定）。空过滤器 = 既有全量口径——命令面走的就是这一支。
	// ListSessions lists sessions; an empty filter keeps the command surface's
	// unfiltered behaviour, and ordering stays fixed by the implementation.
	ListSessions(ctx context.Context, filter SessionFilter) ([]SessionRecord, error)
}

// ErrRunNotFound 表示按标识查不到账本行——web 映射 404（约束 21 / §3.1.18），
// 恢复路径不得把它当成"已收尾"。
// ErrRunNotFound means no ledger row exists for the id; the web layer maps it to 404.
var ErrRunNotFound = errors.New("run not found")

// ErrAdmissionOutcomeUnknown 表示准入事务的**提交结果未知**（连接中断 / 超时）：
// 事务可能已提交、也可能没有。调用方**只能**按 Run 标识回查（`LookupAdmittedRun`），
// 查得到就按成功继续、查不到就按失败返回——**不得**退款、**不得**重试，重试会在
// "其实已提交"的分支上多记一次任务次数、多建一条 Run。
// ErrAdmissionOutcomeUnknown marks an admission commit whose outcome is unknown. The
// caller may only look the run up; refunding or retrying would double-book.
var ErrAdmissionOutcomeUnknown = errors.New("core: admission outcome unknown")

// RunFilter 是运行列表的三个过滤参数（约束 21）：只有会话 / Agent / 状态三个过滤，
// 排序与窗口由仓储固定——**不得**随请求派生 ORDER BY 或任意分页参数。
// RunFilter carries exactly the three allowed list filters; ordering and window are fixed
// by the repository and never derived from the request.
type RunFilter struct {
	SessionID string
	Agent     string
	Status    string
	Limit     int
}

// RunDetail 是运行行的读侧投影：账本起始行 + 终态列 + 资源清单摘要。
// `FinalText` **不是**账本列（`runs` 表没有它，约束 22）：由该 Run 最近一条 assistant
// 消息派生；`ResourceManifest` 只在详情读取时填充，列表只带 `ManifestHash`。
// RunDetail is the read-side projection of one run: the starting row plus terminal columns.
// FinalText derives from the latest assistant message, never from a column; the full
// manifest is filled on detail reads only, while lists carry just its hash.
type RunDetail struct {
	RunRecord
	Status           RunStatus
	Reason           string
	Iterations       int
	ToolCalls        int
	FinalText        string
	ManifestHash     string
	FinishedAt       time.Time
	ResourceManifest *RunResourceManifest
	// WorkerInstanceID 是该 Run 的 run 租约**最后一个**负责人（未释放 / 已回收的
	// 行都读得到）；迁移前的历史 Run 没有租约行，为空串。它只是诊断读数——归属
	// 判定由 `LeaseStore` 负责，本字段不参与任何写入校验。
	// WorkerInstanceID is the last owner of this run's lease; legacy runs have none.
	// It is a diagnostic reading and never feeds an ownership check.
	WorkerInstanceID string
	// OwnershipEpoch 是该租约的归属代次（每接管一次加一）；无租约行时为 0，
	// 与迁移回填的 `owner='legacy', epoch=0` 同形。
	// OwnershipEpoch is the lease epoch, 0 when there is no lease row.
	OwnershipEpoch int64
	// AssetBundleHash 是准入时固定的资产包哈希（v3 清单起；更早的清单为空 =
	// 未记录）。它只在详情读取时填充——列表不解析清单正文。
	// AssetBundleHash is the frozen bundle hash from the v3 manifest; detail reads
	// only, because lists never parse the manifest body.
	AssetBundleHash string
}

// CallerAdmission 是调用方并发的判定输入，由装配层按既有 `caller_limits` 投影解析
// （行 > 0 用行值；行 0 或无行回落实例上限；读失败 fail-closed）。`Enabled=false` 只表示
// "不占调用方名额桶"（内置不限量管理员），**不**表示跳过实例上限、会话串行或审计。
//
// `ReservedForInteractive` 是**交互预留**：从 `MaxConcurrent` 中扣掉后，剩下的是 Flow
// 叶 Run 的上限。它是"给交互留的"，不是"给 Flow 留的"——字段名反过来会让 Flow 上限算反。
// 第二条限制只对 `IsFlow=true` 的请求生效；非 Flow 请求判了它，交互预留就会反过来拒绝
// 本该被预留保护的交互 Run（与 `callers.Limiter.acquire` 的 `flow &&` 门控同形）。
// 预检与事务内权威复查**必须**传同一个值。
// CallerAdmission is the caller-concurrency input resolved by the assembly. The reserved
// slice protects interactive runs, so the second limit applies to flow requests only.
// Precheck and the authoritative recheck must receive the same value.
type CallerAdmission struct {
	Enabled                bool
	MaxConcurrent          int
	ReservedForInteractive int
	IsFlow                 bool
}

// CallerAdmissionResolver 把调用方身份投影为一次准入的判定输入。接口声明在消费方
// （`core`），实现落在 `app`——解析要读 `caller_limits` 行并回落实例上限，那是装配层
// 才知道的事实。`Principal` 不合法时返回 `ErrUnauthorized`（fail-closed，不得回落成
// `Enabled=false` 的"不限量"）。
// CallerAdmissionResolver projects a principal into admission input; the assembly owns
// the implementation. An invalid principal is refused rather than treated as unlimited.
type CallerAdmissionResolver interface {
	For(p Principal) (CallerAdmission, error)
}

// LifecycleExpectation 定义在 lifecycle.go（与 `LifecycleIndex` 端口同一文件）。
// LifecycleExpectation lives in lifecycle.go, next to the index port that feeds it.

// RunStore 是运行账本的读写接口。**不接受 finalText 参数**——最终回答只存在
// RunResult 与会话的 assistant 消息里（§3.4：`runs` 表无 `final_text` 列）。
// RunStore is the read/write interface of the run ledger. FinishRun takes no finalText:
// the final answer lives in RunResult and the assistant message only.
type RunStore interface {
	// CheckAdmission 是**只读预检**：调用方并发计数在前、同会话活跃租约在后，与
	// openapi 的错误优先级一致（429 CALLER_CONCURRENCY_LIMITED → 409 SESSION_BUSY）。
	// 结论可能被 `StartRun` 的权威复查推翻；本方法**不写任何行**、不产生任何占用。
	// 会话与实例两段按既有语义就地判定（预检不需要 advisory 锁）。
	// CheckAdmission is the read-only precheck; its verdict may be overturned by the
	// authoritative recheck inside StartRun, and it never writes a row.
	CheckAdmission(ctx context.Context, sessionID string, p Principal, admission CallerAdmission) error

	// StartRun 建 `status=running` 的账本行（`trigger` 由 Channel 按契约 §5 映射），并在
	// **同一事务**内完成权威准入、任务次数记账、清单与全部资源引用、run 租约；任一步
	// 失败整笔回滚（零占用，FR-24-03 / FR-24-33）。
	// `manifest.FormatVersion == 0` 表示装配尚未接线：只写运行行、**不伪造**清单——这类
	// 运行在读取面如实呈现为"未记录"（legacy_unrecorded），不得补造。
	// 返回的 token 从这一次起就是本 Run 的执行权凭证：调用方把它填进 RunEnv 并在终态、
	// 收尾与受保护写入面带上它。`rec.Lifecycle` 的零值表示调用方未提供期望值——准入
	// **拒绝**而不是跳过最终复查（P-16）。提交结果未知返回 ErrAdmissionOutcomeUnknown。
	// StartRun admits, accounts, writes the row/manifest/refs and creates the run lease in
	// one transaction. The returned token is this run's credential from then on; a zero
	// Lifecycle expectation is refused, never skipped. An unknown commit outcome returns
	// ErrAdmissionOutcomeUnknown.
	StartRun(ctx context.Context, rec RunRecord, manifest RunResourceManifest,
		owner WorkerIdentity, admission CallerAdmission, runID string) (LeaseToken, error)

	// LookupAdmittedRun 是提交结果未知后的唯一回查面：按 runID 读 Run 行与其 run 租约，
	// 读得到就返回该租约的 token（调用方按成功继续），读不到返回 ErrRunNotFound。
	// **不得**用它判断"是否该再试一次"——未知结果不重试。
	// LookupAdmittedRun is the only lookup after an unknown commit; it never authorises
	// a retry.
	LookupAdmittedRun(ctx context.Context, runID string) (LeaseToken, error)

	// FinishRun 把终态写回同一行（status / reason / iterations / tool_calls / finished_at），
	// 并返回**本次调用是否真正完成了状态转换**（`applied`）：目标是终态（已终态 / 已删除）
	// 时命中 0 行、返回 false。写失败**不得**覆盖已得结果：调用方如实返回 RunResult，
	// 行留给下次启动的恢复扫描收尾。`applied` 是记忆抽取钩子的唯一触发判据——写失败或
	// 幂等未转换一律不抽取（C-05）。
	//
	// token 非零时必须做归属 fencing（US-24 FR-24-36）：同一事务先锁租约行再校验
	// 负责人 + 归属代次 + 未过期，不符返回 `ErrLeaseLost`；**只写终态列**，不释放引用、
	// 不结算投递、不释放租约——收尾留给 `CompleteRunCloseout`。零值 token 表示该装配
	// 没有租约（SQLite 谱系 / 轻装配 / 零值单测），走无 fencing 的纯条件更新。
	// FinishRun writes the terminal state and reports whether this call performed the
	// transition (applied). Zero rows — already terminal or deleted — returns false. A
	// write failure must not overwrite an obtained result; recovery closes the row later.
	// applied is the extraction hook's only trigger condition. A non-zero token fences the
	// write on owner + epoch + not-expired and never releases refs, deliveries or the lease.
	FinishRun(ctx context.Context, runID string, token LeaseToken, status RunStatus, reason string, iterations, toolCalls int, now time.Time) (applied bool, err error)

	// CompleteRunCloseout 是 Run 收尾的**最后一步**：引用释放（资源 / 凭据 / 通知三类，
	// 幂等）与 run 租约释放在**同一事务**内提交，token 非零时同样做归属 fencing
	// （US-24 FR-24-36 / 模块卡 §2.3.5）。调用时机固定在学习钩子与投递都结束之后；
	// "该 run 无有效租约"精确等价于"收尾已完成，或属主已经不在了"——回收器的 B1
	// 谓词正好用这一条。已释放的租约重放返回 nil（幂等），不属于本人的 token 返回
	// `ErrLeaseLost`。
	// CompleteRunCloseout releases refs and the run lease in one transaction. It runs last,
	// after the learning hook and delivery settlement; a replay is idempotent.
	CompleteRunCloseout(ctx context.Context, runID string, token LeaseToken, now time.Time) error

	// RequestCancel 写 `runs.cancel_requested`：运行不在本进程时由**属主**在续约 /
	// 检查点观察并自行收尾。非属主只写标志、如实返回当前状态——**不得**假装取消成功，
	// 也不得代替属主提交终态（FR-24-22）。返回写后的运行详情；行不存在返回 ErrRunNotFound。
	// RequestCancel writes the cancel flag; a non-owner only writes it and reports the
	// current state truthfully. It never fakes a successful cancel.
	RequestCancel(ctx context.Context, runID string) (RunDetail, error)

	// ReleaseRunRefs 释放该 Run 的资源 / 凭据 / 通知三类引用（幂等，已释放的行跳过）。
	// 调用时机固定在终态落库与抽取钩子**之后**、无条件执行：钩子要读的运行期客户端
	// 依赖这些引用仍然有效。释放失败只记 ERROR，已终态未释放的行由下次启动的
	// `releaseTerminalRefs` 兜底补齐（C-05）。
	// ReleaseRunRefs releases the run's resource/credential/notification refs. It runs
	// unconditionally after the terminal write and the extraction hook, because the hook
	// needs those refs alive. A failure is logged and closed by startup recovery.
	ReleaseRunRefs(ctx context.Context, runID string, now time.Time) error

	// GetRun 取单条运行详情（含准入清单）；不存在返回 ErrRunNotFound。
	// GetRun returns one run detail including its admission manifest, or ErrRunNotFound.
	GetRun(ctx context.Context, runID string) (RunDetail, error)

	// ListRuns 按固定排序（started_at DESC, run_id DESC）与固定窗口返回运行列表。
	// 列表只带清单哈希、不带完整清单——与 openapi 的 RunView 一致（详情才含清单）。
	// ListRuns returns runs in the fixed order and window, carrying only the manifest hash.
	ListRuns(ctx context.Context, filter RunFilter) ([]RunDetail, error)
}

// ToolInvocationStore 是工具审计的两阶段写接口（契约 §7.2，fail-closed）：
// 先写 `pending` 再执行；**预写失败即拒绝执行**；返回后回填终态同一行。
// ToolInvocationStore is the two-phase write interface of tool audit (contract §7.2):
// a failed pre-write means the execution is refused; the terminal state updates the same row.
type ToolInvocationStore interface {
	// BeginInvocation 写 `pending` 行并返回行 id；失败 = 调用方**拒绝执行**该次调用。
	// BeginInvocation inserts the pending row and returns its id; a failure here means
	// the caller must refuse to execute.
	BeginInvocation(ctx context.Context, inv ToolInvocation) (int64, error)

	// FinishInvocation 把该行更新为终态；回填失败**不得**覆盖真实结果（记 ERROR、行留 pending）。
	// FinishInvocation writes the terminal state; a failed write keeps the row pending and
	// must never fake a terminal value.
	FinishInvocation(ctx context.Context, id int64, res ToolInvocationResult) error
}

// ToolInvocationReader 是**补齐路径专用的只读**审计接口：Session 只读不写审计，
// 仓储的同一实现满足它（写路径经 ToolInvocationStore，读写合成一个实现、两个消费面）。
// ToolInvocationReader is the read-only audit interface used by the synthesis path:
// Session reads audit rows but never writes them.
type ToolInvocationReader interface {
	// Lookup 按 (runID, toolCallID) 取审计行；未命中返回 ok=false（无行 ≠ 错误）。
	// Lookup fetches the row by (runID, toolCallID); a miss returns ok=false, not an error.
	Lookup(ctx context.Context, runID, toolCallID string) (ToolInvocationView, bool, error)
}

// SnapshotStore 是配置快照的落库接口（insert-if-absent；`UNIQUE(agent_name, hash)` 冲突即 no-op）。
// SnapshotStore persists the config snapshot; a unique-key conflict is a no-op.
type SnapshotStore interface {
	// SaveSnapshot 保存快照；失败 = 该次运行**拒绝启动**（零模型调用）。
	// SaveSnapshot persists the snapshot; a failure refuses the run (zero model calls).
	SaveSnapshot(ctx context.Context, snap *ConfigSnapshot) error
}

// ToolResolver 按名解析工具与取规格——名字集由装配期从注册表导出（宪法 III 的显式面）。
// ToolResolver resolves tools by name and returns their specs; the name set is derived
// explicitly at assembly time (constitution III).
type ToolResolver interface {
	// Resolve 查注册池；未命中返回 ok=false（不遍历、不推断）。
	// Resolve looks the pool up by name; a miss returns ok=false.
	Resolve(name string) (Tool, bool)

	// Specs 按传入名字的**顺序**逐名取规格；未命中的名字跳过（运行时归属校验已先行把关）。
	// Specs returns specs in the order the names were given; unknown names are skipped.
	Specs(names []string) []ToolSpec
}

// ToolPinner 是准入时**钉住工具实例**的解析面（14D / C4）：注册池可以在运行期被受控
// 重建（`mcp_server.apply`）整组换掉，但一次 Run 手里的工具实例必须在准入时固定——
// 否则在途 Run 会被悄悄换到新连接，与"旧 Run 的快照不被改写"相悖。
// 它是 `ToolResolver` 之外的**第二个**接口而不是给它加方法：按名解析与实例快照是两件
// 事，只做前者的实现（既有测试替身）不必凭空实现后者。
// ToolPinner pins tool instances at admission; a controlled apply may swap the registry,
// and only a pinned set keeps an in-flight run on the connection it admitted with.
type ToolPinner interface {
	// Pin 按名取出一组工具实例与它们的规格（顺序同入参名字，未命中的名字跳过）。
	// Pin snapshots instances and specs in the caller's name order, skipping misses.
	Pin(names []string) (map[string]Tool, []ToolSpec)
}

// Policy 是授权判定接口（技术方案 §4.3 逐字）：回答"这个动作允许吗"，不负责"怎么做"。
// 调用点在**工具内部**（文件工具只需 Policy，命令执行才需 CommandRunner）——调用方
// 只依赖本接口，**不得**依赖 WhitelistPolicy 等具体实现。
// Policy answers "is this action allowed?"; the call site lives inside each tool and
// callers depend on the interface only, never on a concrete implementation.
type Policy interface {
	// Check 判定失败返回拒绝错误（`*SandboxViolationError`，四段式文案，模型可见）。
	// Check returns a SandboxViolationError on refusal; the message is model-visible.
	Check(ctx context.Context, action Action) error
}

// CommandRunner 是受控子进程执行接口（技术方案 §4.3 逐字）：回答"怎么受控地执行"——
// cwd、环境变量白名单、输出上限、取消与进程树回收都在实现侧。
// CommandRunner executes a command under control: cwd, env allowlist, output cap,
// cancellation and process-tree cleanup all live in the implementation.
type CommandRunner interface {
	// Run 执行一次受控命令；超时 / 取消经 ctx 表达，实现必须回收整棵进程树。
	// Run executes one controlled command; the implementation must reap the whole tree.
	Run(ctx context.Context, cmd Command) (CommandResult, error)
}

// ToolInvocation 是工具审计行的**预写形态**：只含执行前可知的字段。
// **不带状态字段**——预写态恒为 `pending`，由仓储 SQL 固定写（调用方给不出错状态）。
// 入参 `InputJSON` 原样保存（模型给的 arguments 是 JSON 字符串，不重编码）。
// ToolInvocation is the pre-write shape of a tool audit row: fields known before
// execution only. It carries no status field on purpose — the repo writes pending in SQL.
type ToolInvocation struct {
	RunID      string
	SessionID  string
	ToolCallID string
	ToolName   string
	InputJSON  json.RawMessage
	CreatedAt  time.Time // 列存 Unix 毫秒 UTC（契约迁移纪律 4）
}

// ToolInvocationResult 是工具审计行的**终态回填形态**：成败只由 Status 表达，不设布尔列。
// `ResultJSON` 是结果表示（`{"content":…,"retryable":…,"truncated":true}`）——截断标记
// 写在值内，**不**新造列；`ErrorMessage` 截断 4 KB 的责任在 storage。
// ToolInvocationResult is the terminal shape of a tool audit row: success or failure is
// expressed by Status alone; the truncation flag lives inside ResultJSON, never in a column.
type ToolInvocationResult struct {
	Status       LLMCallStatus
	ResultJSON   json.RawMessage
	ErrorMessage string
	ErrorKind    ErrorKind // 失败必填（契约 §7.3 七值）；成功留空 → 列写 NULL
	Duration     time.Duration
}

// ToolInvocationView 是补齐路径读到的审计行视图：终态行按 `ResultJSON` **重建真实结果**，
// `pending` / `unknown` 行取「结果未知」文案（契约 §4.4 五分支）——故只读这三样，
// 不把整行搬进 core（审计列面归 storage）。
// ToolInvocationView is what the synthesis path reads: a terminal row rebuilds the real
// result from ResultJSON, a pending/unknown row yields the unknown-result text.
type ToolInvocationView struct {
	Status       LLMCallStatus
	ResultJSON   json.RawMessage
	ErrorMessage string
	ErrorKind    ErrorKind
}

// SessionRecord 是会话行的载体（`sessions` 表的读写面）：
// 读侧供 `session list` 渲染（标识 / 消息数 / 最后活动），写侧建行与刷新。
// SessionRecord carries a session row: the read side feeds `session list`, the write
// side inserts or refreshes the row.
type SessionRecord struct {
	ID          string // 由命令面按「渠道 + 用户 + Agent 画像」派生（契约 §5）
	ProfileName string
	Channel     string
	UserID      string
	// CallerID 是会话归属调用方（`sessions.caller_id`）：空串写 NULL——迁移遗留的
	// 无归属会话只允许管理面读取，**不得**用空串冒充某个调用方。
	// CallerID is the owning caller; an empty value writes NULL (migration legacy).
	CallerID     string
	MessageCount int
	CreatedAt    time.Time
	LastActiveAt time.Time
	// Status 是会话状态（active | archived）；GetSession 读 `status` 列，
	// ListSessions 按 `archived_at` 是否为空派生（契约 SessionSummary 的口径）。
	Status string
	// ArchivedAt 是可空归档时间；仅 GetSession / ArchiveSession 填充。
	ArchivedAt *time.Time
}

// SessionFilter 是会话列表的过滤条件（模块卡 §3.1.26 / 约束 21）：`agent` / `status`
// 两维，空字段 = 该维不过滤。`Status` 取 `active` / `archived`——**不是**任意字符串：
// 调用方（HTTP 层）先校验取值，非法值不得落到 SQL。
//
// `CallerID` 是**归属强制过滤**（US-16 / T056），与上面两维不同：它不是可选的收窄
// 条件，而是"调用方只能看自己的行"这条越权边界的载体。空 = 不过滤，只有管理员 /
// 命令面可以这样用；调用方身份在 web 层先判空再填，退化成全量即越权。
// SessionFilter narrows the list by agent and status, where an empty field filters
// nothing. CallerID is different: it carries the ownership boundary, and only the
// admin or the CLI may leave it empty.
type SessionFilter struct {
	Agent    string // 画像名（sessions.profile_name 精确匹配）
	Status   string // active | archived
	CallerID string // 归属调用方；空 = 不过滤（仅管理员 / 命令面）
}

// RunRecord 是运行账本的**起始行载体**：只含开跑前可知的字段。
// **不含状态与终态列**——起始态恒为 `running`、终态经 FinishRun 写入（同预写纪律：
// 状态不由调用方给，也就没有传错的可能）。`SessionID` 空串 = 无状态调用 → 列写 NULL。
// RunRecord is the starting-row shape of the run ledger: fields known before the run.
// Status is deliberately absent — the row starts as running and only FinishRun writes
// a terminal state. An empty SessionID means a stateless run and writes NULL.
type RunRecord struct {
	RunID string
	// RequestID 只保留入口实际记录的请求标识；空值不补造历史关联。
	// RequestID preserves a recorded ingress identity without inventing one.
	RequestID string
	SessionID string
	Agent     string
	Channel   string
	UserID    string
	// Principal 是本次运行的调用方身份（必填）：`caller_id` / `authorizing_kind` /
	// `key_id` / `actor` 四列都由它派生，**不另设**重复的 CallerID 字段（P-04）。
	// Principal derives the four ownership columns; no duplicated CallerID field.
	Principal Principal
	// ReservationID 是准入预留标识（可空；`runs.reservation_id text`）。
	// ReservationID is the optional quota reservation id.
	ReservationID string
	Trigger       Trigger // 落库触发方式：scheduler 显式填 cron / manual；空 = 由 storage 按 Channel 映射（cli→user / web→api / scheduler→cron / admin→manual）
	ConfigHash    string  // 本次运行使用的配置快照版本（关联 config_snapshots.hash）
	StartedAt     time.Time
	// Lifecycle 是准入 [1] 读到的生命周期期望值（P-16）：`StartRun` 在事务内与共享
	// 索引比对，发布 / 归档竞争因此不能在"已固定版本、未落库"的窗口里插进来。
	// 零值 = 调用方未提供 → **拒绝**（缺期望值就无法做最终复查）。
	// Lifecycle carries the admission-time expectation rechecked inside StartRun's
	// transaction; a zero value is refused rather than skipped.
	Lifecycle LifecycleExpectation
	// Flow associates an admitted Run with the already durable node attempt.
	Flow *FlowRunContext
}

// SandboxExecutionStore 是生命周期写入端口；审计仍由 ToolExecutor 独占。
// SandboxExecutionStore persists lifecycle facts, not tool audit conclusions.
type SandboxExecutionStore interface {
	BeginSandboxExecution(context.Context, SandboxExecution) error
	MarkSandboxRunning(context.Context, int64, time.Time) error
	FinishSandboxExecution(context.Context, SandboxExecutionResult) error
	CountActiveSandboxExecutions(context.Context, string) (int, error)
	MarkSandboxCleaned(context.Context, int64, SandboxState, CleanupState, string, time.Time) error
}

// SandboxExecutionReader 是恢复与运行详情的窄读端口。
// SandboxExecutionReader serves recovery and run detail projections.
type SandboxExecutionReader interface {
	PendingSandboxExecutions(context.Context, string) ([]SandboxExecution, error)
	SandboxExecutionsByRun(context.Context, string) ([]SandboxExecutionView, error)
}

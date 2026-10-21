package core

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// 本文件是管理面的领域类型与**消费方端口**（颗粒度文档 §3.1.9）：管理操作记录、
// 操作仓储、actor 字面量、Agent 内容读写请求与 `AgentAdmin` 端口。
// 字面量取自 docs/contracts/schema.sql 的 `management_operations` 段与
// docs/contracts/run-and-errors.md §9.3；实现在 internal/storage（操作仓储）与
// internal/app（组合适配器），core 只声明、不依赖任何实现包（宪法 VIII）。
// This file declares the management-surface domain types and consumer-side ports:
// operation records, the operation store, actor literals, content-write request
// shapes and the AgentAdmin port. Literals come from the schema contract and
// run-and-errors §9.3; implementations live in internal/storage and internal/app.

// OperationStatus 是管理操作的四个状态字面量（schema.sql CHECK 子句逐字）。
// OperationStatus is the four-value literal set of management operation states.
type OperationStatus string

const (
	// OperationPending 是预写态：变更**之前**落库，预写失败即拒绝执行（宪法 V）。
	// OperationPending is the pre-write state; a failed pre-write refuses the change.
	OperationPending OperationStatus = "pending"
	// OperationSuccess 是变更成功并已回填的终态。
	// OperationSuccess is the terminal state written after a successful change.
	OperationSuccess OperationStatus = "success"
	// OperationFailed 是变更失败或被恢复路径查证为未生效的终态。
	// OperationFailed is the terminal state of a failed or verified-not-applied change.
	OperationFailed OperationStatus = "failed"
	// OperationUnknown 是恢复路径查证不出结论时保留的状态——**不得**盲目重做。
	// OperationUnknown is kept when startup recovery cannot determine the outcome;
	// such an operation is never replayed.
	OperationUnknown OperationStatus = "unknown"
)

// 管理操作的 auth_method 四值（schema.sql CHECK 子句逐字）。
// The four auth_method literals of a management operation.
const (
	// AuthMethodBearer 是浏览器 / 客户端持有的管理 token。
	AuthMethodBearer = "bearer"
	// AuthMethodLocalTrust 是直接回环连接 + 本机信任头。
	AuthMethodLocalTrust = "local_trust"
	// AuthMethodCLI 是本机 CLI 调用。
	AuthMethodCLI = "cli"
	// AuthMethodMigration 是启动基线 / 迁移流程。
	AuthMethodMigration = "migration"
)

// actor_subject 的四种固定形态（run-and-errors §9.3）：前两种带前缀，后两种是
// 整体字面量。token 只落指纹，明文**不得**进入任何持久面。
// The four fixed actor_subject shapes; the token form carries only a fingerprint.
const (
	// ActorSubjectTokenPrefix 后接 sha256(token) 的前 16 位小写十六进制。
	ActorSubjectTokenPrefix = "token:"
	// ActorSubjectLocalTrust 表示"未验证个人身份"的本机信任。
	ActorSubjectLocalTrust = "local_trust"
	// ActorSubjectCLIPrefix 后接本机用户名。
	ActorSubjectCLIPrefix = "cli:"
	// ActorSubjectMigration 表示启动基线 / 迁移流程。
	ActorSubjectMigration = "migration"
)

// 管理操作 `verification` 列的三值（schema.sql CHECK 子句逐字）：仅启动恢复查证时写。
// The three verification literals, written only by startup recovery.
const (
	// VerificationApplied 表示查证确认变更已生效 → 操作回填 `success`。
	VerificationApplied = "applied"
	// VerificationNotApplied 表示查证确认变更未生效 → 操作回填 `failed`。
	VerificationNotApplied = "not_applied"
	// VerificationIndeterminate 表示查证无结论 → 操作保持 / 置为 `unknown`。
	VerificationIndeterminate = "indeterminate"
)

// Actor 是一次管理请求的认证主体：谁、用哪种方式、从哪里发起。
// 三个字段逐列对应 `management_operations` 的 actor_subject / auth_method / source；
// 显式参数传递（不写入 context——宪法 VII）。
// Actor identifies who performed a management request, by which method, from where.
type Actor struct {
	// Subject 是 actor_subject 固定形态之一（见上方常量）。
	Subject string
	// Method 是 auth_method 四值之一。
	Method string
	// Source 是来源归因：`<ip>:<port>` / `cli` / `migration`。
	Source string
}

// OperationRecord 是一条管理操作记录（`management_operations` 行的领域形态）。
// 可空列用零值表达"未设置"：`RequestedVersionID` 0 / `ExpectedETag` 空串 /
// `FinishedAt` / `RecoveredAt` 零时间 / `Verification` 空串——storage 负责映射 NULL。
// OperationRecord is the domain shape of one management operation row; zero values
// mark the nullable columns, and storage owns the NULL mapping.
type OperationRecord struct {
	OperationID        string
	Action             string // 点号分隔的 `<resource>.<verb>` 字面量
	TargetKind         string // agent / schedule / model_connection / mcp_server / skill / notify_channel / system
	TargetID           string
	ActorSubject       string
	AuthMethod         string
	Source             string
	RequestHash        string // 脱敏请求摘要（sha256）；不含凭证原文
	RequestedVersionID int64  // 0 = 无目标版本
	ExpectedETag       string // 空 = 无条件写期望
	Status             OperationStatus
	ResultCode         string
	ErrorMessage       string
	DetailJSON         []byte
	CreatedAt          time.Time
	FinishedAt         time.Time
	RecoveredAt        time.Time
	Verification       string
}

// OperationFilter 是管理操作列表的过滤条件；空字段表示不过滤。
// 列表的排序与窗口由仓储固定（`created_at DESC, operation_id DESC`，缺省 50 / 上限 200），
// 调用方只给过滤值——排序不是参数，避免第二套口径。
// OperationFilter carries the list filters; ordering and window stay fixed in storage.
type OperationFilter struct {
	Action     string
	TargetKind string
	TargetID   string
	Status     string
	Limit      int
}

// ErrOperationNotFound 表示按标识查不到管理操作记录——web 映射 404，恢复路径不掩盖。
// ErrOperationNotFound is returned when an operation id has no row; the web layer
// maps it to 404 and startup recovery never hides it.
var ErrOperationNotFound = errors.New("management operation not found")

// ErrPreconditionFailed 表示条件写的前置条件不满足：`If-Match` 缺失、格式非法、
// 弱 ETag（`W/"..."`）、`*` 或与当前值不等。调用方必须返回 412 且磁盘零写入、
// **不写管理操作行**（约束 25）——先在比较里失败，就还没有"要留痕的变更"。
// ErrPreconditionFailed marks a failed conditional-write precondition; the caller
// answers 412 without touching the disk or writing an operation row.
var ErrPreconditionFailed = errors.New("precondition failed")

// ErrEditUnsupported 是 core 边界上的"补丁无法安全保留原文"分类（块内注释 / 锚点 /
// 流式集合 / 未知字段）：调用方必须返回 400 `EDIT_UNSUPPORTED` 并提示改用整份原文，
// **不得**静默重排或丢注释（约束 26）。workspace 的同名哨兵在内容写出口映射到这里，
// web 因此无需依赖 workspace。
// ErrEditUnsupported is the core-side classification of a patch that cannot preserve
// the original bytes; the caller answers 400 EDIT_UNSUPPORTED.
var ErrEditUnsupported = errors.New("edit unsupported")

// OperationStore 是管理操作仓储的消费方端口（实现在 internal/storage）：
// **先写 pending、返回后回填终态**，与调用审计同一两阶段纪律（宪法 V）。
// OperationStore is the consumer-side port of the operation repository: pre-write
// pending first, then finish with the terminal state.
type OperationStore interface {
	// BeginOperation 只接受 `pending` 预写；失败 = 调用方**拒绝执行**该变更。
	// BeginOperation pre-writes a pending row; a failure refuses the change.
	BeginOperation(ctx context.Context, rec OperationRecord) error

	// FinishOperation 回填终态；对已终态的行幂等返回，**不得**覆盖。
	// FinishOperation writes the terminal state; an already-terminal row is left untouched.
	FinishOperation(ctx context.Context, operationID string, status OperationStatus, resultCode, errorMessage string, detailJSON []byte, now time.Time) error

	// GetOperation 按标识取一行；未命中由实现映射为 not-found 错误。
	// GetOperation fetches one row by id; a miss maps to a not-found error.
	GetOperation(ctx context.Context, operationID string) (OperationRecord, error)

	// ListOperations 按过滤条件取固定窗口的列表。
	// ListOperations returns a fixed-window list under the filter.
	ListOperations(ctx context.Context, filter OperationFilter) ([]OperationRecord, error)

	// PendingOperations 返回全部 `pending` 行——启动恢复的输入，**只查证、不重做**。
	// PendingOperations feeds startup recovery, which verifies but never replays.
	PendingOperations(ctx context.Context) ([]OperationRecord, error)

	// RecoverOperation 只由启动恢复调用（约束 38 / 契约 §9.4）：把仍为 pending 的行
	// 按查证结论写终态，同时记录 `recovered_at` 与 `verification`；已终态的行幂等返回，
	// **不得**覆盖。正常完成路径**不得**调用本方法。
	// RecoverOperation is startup-recovery only: it writes the terminal state together
	// with recovered_at and verification, and leaves already-terminal rows untouched.
	RecoverOperation(ctx context.Context, operationID string, status OperationStatus, verification string, detailJSON []byte, now time.Time) error
}

// NewOperationID 生成操作标识 `op-<unix_millis>-<6 位小写字母数字>`（契约 §9.3）。
// 时间显式传入（可复现）；随机段与 Run ID 同源（base36 小写），取不到熵时以全零段
// 收场——唯一性仍由毫秒段与仓储主键冲突重试兜底，**不**退化成可预测计数器。
// NewOperationID builds `op-<unix_millis>-<six lowercase alphanumerics>`; the clock is
// explicit and the entropy failure mode never degrades into a predictable counter.
func NewOperationID(now time.Time) string {
	return "op-" + strconv.FormatInt(now.UnixMilli(), 10) + "-" + runSuffix()
}

// FileEntry 是 Agent 受管文件清单的一项：**Agent 目录内**相对路径（`/` 分隔，注 19：
// 只含该目录根下的普通文件，不含技能工作副本与控制目录）与字节数。
// FileEntry is one managed file: a path relative to the Agent directory plus its size.
type FileEntry struct {
	Path string
	Size int64
}

// AgentContent 是 `GET /agents/{name}/content` 的读取结果：原文 + 强 ETag。
// 原文按字节保留（JSON 不加 base64、不去 BOM）；ETag 与磁盘字节一一对应。
// AgentContent is the content read result: the raw text plus its strong ETag.
type AgentContent struct {
	Content string
	ETag    string
}

// AgentLifecycleView 是 Agent 生命周期状态的观测视图：`State` 为 nil 表示状态文件
// 缺失或损坏（与 `Valid=false` 同时出现），调用方**不得**把 nil 读成 active。
// AgentLifecycleView is the observable lifecycle state; a nil State means the state
// file is missing or corrupt and must never be read as active.
type AgentLifecycleView struct {
	State          *string
	Valid          bool
	LifecycleError string
}

// AgentFiles 是内容写成功后的返回视图：Agent、生命周期与受管文件集合。
// AgentFiles is the response view after a successful content write.
type AgentFiles struct {
	Agent     string
	Lifecycle AgentLifecycleView
	Files     []FileEntry
}

// AgentContentUpdate 是一次内容写请求：整份替换与结构化补丁**恰给一个**。
// AgentContentUpdate carries exactly one of a whole-file replacement or a patch.
type AgentContentUpdate struct {
	Content string
	Changes *AgentContentChanges
}

// AgentContentChanges 是 AGENT.md frontmatter 的结构化补丁：指针字段为 nil 表示不改；
// `Tools` / `Skills` / `MCPServers` / `Bootstrap` / `Schedules` 用 `*[]T` 区分
// "不改"（nil）与"改成空列表"（非 nil 空切片）。
// AgentContentChanges patches the frontmatter; pointer fields distinguish "leave
// unchanged" from "set to empty".
type AgentContentChanges struct {
	Description *string
	Identity    *AgentIdentityChanges
	Provider    *AgentProviderChanges
	Tools       *[]string
	Skills      *[]string
	MCPServers  *[]string
	Bootstrap   *[]string
	Settings    *AgentSettingsChanges
	Schedules   *[]Schedule
}

// AgentIdentityChanges 是 identity 段的结构化补丁。
// AgentIdentityChanges patches the identity block.
type AgentIdentityChanges struct {
	AgentName *string
	Prompt    *string
}

// AgentProviderChanges 是 provider 段的结构化补丁；Temperature 指针保留"显式 0"。
// AgentProviderChanges patches the provider block; the temperature pointer keeps an
// explicit zero distinct from "unset".
type AgentProviderChanges struct {
	Name        *string
	Model       *string
	Temperature *float64
}

// AgentSettingsChanges 是 settings 段的结构化补丁。
// AgentSettingsChanges patches the settings block.
type AgentSettingsChanges struct {
	MaxIterations   *int
	MaxHistoryTurns *int
}

// AgentLifecycleTransition 是启停请求的前置条件：期望状态与期望代数必须同时匹配，
// 否则调用方收到冲突错误——这是"页面看到的版本"与"服务端当前版本"之间的乐观锁。
// AgentLifecycleTransition is the optimistic precondition of a lifecycle change.
type AgentLifecycleTransition struct {
	ExpectedState      AgentState
	ExpectedGeneration int64
}

// AgentAdmin 是管理台读写 Agent 内容的消费方端口（实现 = internal/app 的组合适配器）。
// web 只依赖本接口——**不得**直接依赖 workspace 的具体实现（宪法 VI 的同型纪律）。
// AgentAdmin is the consumer-side port of the console's agent read/write surface.
type AgentAdmin interface {
	// ReadContent 读取 AGENT.md 原文与强 ETag。
	// ReadContent reads the raw AGENT.md and its strong ETag.
	ReadContent(ctx context.Context, agent string) (AgentContent, error)

	// UpdateContent 在有条件写保护下保存内容；`ifMatch` 与磁盘当前 ETag 不符即拒绝，
	// 磁盘零写入。成功返回该 Agent 的受管文件视图。
	// UpdateContent saves content under conditional-write protection; a mismatched
	// If-Match refuses with zero disk writes.
	UpdateContent(ctx context.Context, agent string, update AgentContentUpdate, ifMatch string, actor Actor) (AgentFiles, error)

	// Transition 执行一次启停迁移：先写 pending 操作记录，再动状态文件。
	// Transition performs one lifecycle change, pre-writing the pending operation first.
	Transition(ctx context.Context, agent string, target AgentState, expected AgentLifecycleTransition, actor Actor) (LifecycleRecord, error)
}

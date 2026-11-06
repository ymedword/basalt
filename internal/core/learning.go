package core

import (
	"context"
	"crypto/rand"
	"errors"
	"time"
)

// 本文件是 US-22 受控自我进化的**声明面**：抽取触发事实、钩子端口、状态与失败分类、
// 窗口与证据模型、候选 / 草稿与提交载荷、只读学习查询面。全部为纯类型与接口——
// 零可执行语句；实现落在 internal/memory（服务与校验）与 internal/storage/postgres
// （账本与事务）。核心不变式：抽取只能由 Run 终态钩子同步触发，候选永不直接生效。
// The declaration surface of US-22 controlled evolution: run facts, the hook port,
// extraction states and failure classes, window/evidence models, candidate and draft
// payloads, and the read-only learning admin port. Declarations only; implementations
// live in internal/memory and internal/storage/postgres.

// LearningRunInput 是一次已完成 Run 触发抽取时携带的冻结事实；全部字段来自 RunEnv 与
// RunRequest，不从 context、包级变量或模型输出反推。**不携带 Principal**：来源身份由
// BeginExtraction 从该 Run 的 runs 行（caller_id / key_id）复制，跨 Run 重试因此不会
// 把旧窗口重新贴成当前请求的身份。
// LearningRunInput carries the frozen facts of one completed run. It deliberately omits
// the Principal: the origin identity is copied from the runs row inside BeginExtraction,
// so a retry can never relabel an old window with the current request's identity.
type LearningRunInput struct {
	RunID      string
	Agent      string
	SessionID  string
	Channel    string
	Provider   string
	Model      string
	ConfigHash string
	// CompletedAt 是终态落库时刻（钩子内的时间基准）。
	CompletedAt time.Time
	// Extractor 是本次 Run **准入时冻结**的模型调用视图（`env.ProviderClient()` 的类型
	// 断言结果），不是装配期另拿的全局客户端：抽取调用只查该 Run 固定的路由表（原则 III），
	// 同时仍走两阶段审计。nil = 装配缺陷；抽取服务记 ERROR 并放弃本次抽取，不换用别的客户端。
	// Extractor is the model-call view frozen at admission for this run, not a global client
	// picked up at assembly time. Nil is an assembly defect: the service logs and gives up
	// rather than switching clients.
	Extractor MemoryExtractor
}

// LearningHook 是 Run 终态之后唯一允许的同步回调；没有返回值，从类型上保证它不能
// 改变 Run 的结果、状态或错误。
// LearningHook is the one synchronous callback allowed after a run's terminal state. It
// returns nothing, so it cannot change the run's result, status or error.
type LearningHook interface {
	AfterRun(ctx context.Context, in LearningRunInput, res RunResult)
}

// MemoryExtractor 是抽取调用面；唯一实现是 ProviderService.ChatMemoryExtract（两阶段审计）。
// MemoryExtractor is the extraction call surface; its only implementation is
// ProviderService.ChatMemoryExtract, which performs the two-phase audit.
type MemoryExtractor interface {
	ChatMemoryExtract(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// NewLearningExtractionID 生成一个抽取标识：`lrn_` 前缀 + 26 位小写 base32（`crypto/rand.Text`，
// 128 位熵）。只由发起抽取的服务在**首次尝试**时生成一次；重试沿用既有行，不换标识。
// NewLearningExtractionID builds an extraction id: an lrn_ prefix plus 128 bits of
// randomness. It is generated once when the first attempt starts; retries reuse it.
func NewLearningExtractionID() string {
	return "lrn_" + rand.Text()
}

// LearningExtractionState 是抽取账本的状态（与 memory_extractions.status 的 CHECK 同源）。
// LearningExtractionState mirrors the memory_extractions.status CHECK.
type LearningExtractionState string

const (
	LearningExtractionPending LearningExtractionState = "pending"
	LearningExtractionSuccess LearningExtractionState = "success"
	LearningExtractionFailed  LearningExtractionState = "failed"
	LearningExtractionSkipped LearningExtractionState = "skipped"
	LearningExtractionUnknown LearningExtractionState = "unknown"
)

// LearningFailureClass 是失败分类闭集（与 memory_extractions.failure_class 的 CHECK 同源）。
// LearningFailureClass mirrors the failure_class CHECK.
type LearningFailureClass string

// 六字面量与 memory_extractions.failure_class 的 CHECK 闭集逐字同源。
// The six literals are verbatim the CHECK set of memory_extractions.failure_class.
const (
	LearningFailureModel         LearningFailureClass = "model_error"
	LearningFailureInvalidOutput LearningFailureClass = "invalid_output"
	LearningFailureTimeout       LearningFailureClass = "timeout"
	LearningFailureAudit         LearningFailureClass = "audit_unavailable"
	// LearningFailureUnavailable 表示装配缺陷：内层客户端不实现 MemoryExtractor，
	// 调用根本没有发出——**不得**归入 model_error（那会把装配问题报成供应商故障）。
	LearningFailureUnavailable LearningFailureClass = "extractor_unavailable"
	// LearningFailureWindowGone 表示重读冻结窗口时消息已被保留策略清理（全部或部分）：
	// 该行直接转 skipped 并在同事务推进游标，不再重试（C-33）。
	LearningFailureWindowGone LearningFailureClass = "window_gone"
)

// LearningItemKind 是候选条目类型闭集（method 只进草案，不写记忆条目）。
// LearningItemKind is the candidate kind set; method only produces drafts.
type LearningItemKind string

const (
	LearningKindFact       LearningItemKind = "fact"
	LearningKindPreference LearningItemKind = "preference"
	LearningKindConstraint LearningItemKind = "constraint"
	LearningKindInference  LearningItemKind = "inference"
	LearningKindTemporary  LearningItemKind = "temporary"
	LearningKindMethod     LearningItemKind = "method"
)

// 只读面与 handoff 的"查不到"哨兵：web 映射 404，**不**与状态冲突（409）混用。
// Not-found and budget sentinels for the read surface and handoff; callers match with
// errors.Is, never by comparing text.
var (
	ErrLearningNotFound      = errors.New("learning: extraction not found")
	ErrLearningDraftNotFound = errors.New("learning: draft not found")
	// ErrLearningBudgetExhausted 由 BeginExtraction 在事务内判定后返回；调用方据此
	// 记 WARN 并放弃，**不得**再尝试写行或发调用。
	ErrLearningBudgetExhausted = errors.New("learning: extraction budget exhausted")
	// ErrLearningUnresolved 表示同来源已有 pending 或未查证的 unknown：新窗口被阻断，
	// 既不建行也不发调用；恢复查证后自然解除（§3.1.4）。
	ErrLearningUnresolved = errors.New("learning: unresolved extraction for origin")
	// ErrLearningExtractorUnavailable 表示内层客户端不支持抽取（装配缺陷）。
	ErrLearningExtractorUnavailable = errors.New("learning: extractor not assembled")
)

// LearningCursor 是 (agent, session, 来源) 维度的处理位置：同一会话被不同 caller / key
// 先后使用时各自推进，互不覆盖。CommittedMessageID 只在成功、终态跳过或恢复查证时推进；
// 失败不推进。OriginKeyID 为空串表示本地管理员（runs.key_id IS NULL）。
// LearningCursor is the per-origin processing position. It advances only on success,
// terminal skip or recovery verification, never on failure.
type LearningCursor struct {
	Agent              string
	SessionID          string
	OriginCallerID     string
	OriginKeyID        string
	CommittedMessageID int64
	UpdatedAt          time.Time
}

// LearningExtraction 是一行抽取账本的领域形态（与 memory_extractions 同构）。
// 一行对应一次触发 Run（RunID）；重试不新建行，只递增 Attempts 并更新
// LastAttemptRunID / LastAttemptAt，窗口主键 WindowFrom/ToMessageID 保持不变。
// LearningExtraction is the domain shape of one extraction row; retries increment
// Attempts and refresh the attempt identity but never move the frozen window.
type LearningExtraction struct {
	ExtractionID          string
	Agent                 string
	SessionID             string
	RunID                 string
	LastAttemptRunID      string
	OriginCallerID        string
	OriginKeyID           string // 空串 = 本地管理员
	Status                LearningExtractionState
	WindowFromMessageID   int64
	WindowToMessageID     int64
	WindowMessages        int
	DroppedWindowMessages int
	PriorMessages         int
	Attempts              int
	ExistingEntries       int
	ItemsTotal            int
	ItemsAccepted         int
	ItemsDropped          int
	PromptVersion         string
	RuleVersion           string
	Provider              string // 最近一次尝试的 provider（重试时随触发 Run 更新）
	Model                 string // 最近一次尝试的 model
	ConfigHash            string // 最近一次尝试的 Run 配置哈希（空 = 该 Run 未填）
	FailureClass          LearningFailureClass
	ErrorMessage          string
	CreatedAt             time.Time
	LastAttemptAt         time.Time
	FinishedAt            time.Time
	VerifiedAt            time.Time
}

// Retryable 表示这一行是「可重试的失败」：同来源下一 Run 只重读冻结窗口。
// pending 与未查证 unknown（verified_at 为零值）阻断（不可重试）；已查证 unknown
// 保留历史但不再阻断。skipped / success 是终态。
// Retryable reports whether the next run from the same origin may retry the frozen
// window. Pending and unverified unknown block instead of retrying.
func (e LearningExtraction) Retryable() bool {
	return e.Status == LearningExtractionFailed && e.Attempts < 3
}

// LearningEvidence 是一条候选所依据的消息证据。Quote 必须是 MessageID 对应消息正文的
// 逐字子串；MessageAt 来自 messages.created_at；RunID 是该消息所属 Run（证据可追溯到
// 具体一次运行，而不是「会话里某条」）。
// LearningEvidence is one verbatim quote from a message; MessageAt is the record time
// and RunID ties the evidence to the exact run that produced the message.
type LearningEvidence struct {
	MessageID int64
	RunID     string
	Role      string
	Quote     string
	MessageAt time.Time
}

// LearningMessage 是窗口/前文的一条消息投影；不携带工具调用与 provider 附加字段。
// LearningMessage is a message projection for the window and prior context.
type LearningMessage struct {
	MessageID int64
	RunID     string
	Role      string
	Content   string
	CreatedAt time.Time
}

// LearningWindow 是消息读面的返回值。FromMessageID / ToMessageID 是**冻结边界**：
// 首次尝试即写入抽取行，重试只重读这一段，不重新计算。
// LearningWindow carries the frozen message boundaries; retries reread exactly this run.
type LearningWindow struct {
	New           []LearningMessage
	Prior         []LearningMessage
	FromMessageID int64
	ToMessageID   int64
	Dropped       int
}

// LearningCandidate 是待提交的一条候选（只会以 candidate 状态落库）。
// LearningCandidate is one candidate to commit; it is only ever stored as candidate.
type LearningCandidate struct {
	Scope                 Scope
	Section               Section
	Content               string
	Kind                  LearningItemKind
	Certainty             string // asserted / uncertain（程序派生）
	Confidence            float64
	Evidence              []LearningEvidence
	SourceEntryIDs        []int64
	SuggestedSupersedesID int64
	// EvidenceAt 是证据消息的最大 created_at：**记录时刻，不是事实发生时刻**。
	EvidenceAt     time.Time
	ValidTo        time.Time // 零值 = 无到期
	TimeConfidence string    // exact / relative_unknown / unknown
}

// LearningDraft 是一份只读方法草案（固定布局，不进记忆条目）。
// LearningDraft is a read-only method draft; it never becomes a memory entry.
type LearningDraft struct {
	DraftID       string
	ExtractionID  string
	Agent         string
	Name          string
	Content       string
	ContentSHA256 string
	Evidence      []LearningEvidence
	RelDir        string
	CreatedAt     time.Time
}

// CommitLearningExtraction 不携带来源身份与游标位置：提交事务从 memory_extractions
// 的同一行读取 origin_caller_id / origin_key_id、window_to_message_id 与 attempts，
// 候选元数据与游标推进都以此为准，调用方无法在提交时重贴身份。
// 来源关系行由每条候选的 SourceEntryIDs 在事务内展开；窗口外或非快照子集已在
// 校验阶段丢弃。
// CommitLearningExtraction carries no origin identity or cursor position: the commit
// transaction reads both from the extraction row, so a caller cannot relabel them.
type CommitLearningExtraction struct {
	ExtractionID  string
	Candidates    []LearningCandidate
	Drafts        []LearningDraft
	ItemsTotal    int
	ItemsAccepted int
	ItemsDropped  int
	CompletedAt   time.Time
}

// LearningCommitResult 是提交事务的回执（新候选主键与草案标识）。
// LearningCommitResult reports the committed candidate entry ids and draft ids.
type LearningCommitResult struct {
	CandidateEntryIDs []int64
	DraftIDs          []string
}

// LearningExtractionFilter 是抽取列表的过滤条件；Limit <= 0 取缺省。
// LearningExtractionFilter carries the extraction list filters.
type LearningExtractionFilter struct {
	Agent string
	State string
	Limit int
}

// LearningDraftFilter 是草案列表的过滤条件；Limit <= 0 取缺省。
// LearningDraftFilter carries the draft list filters.
type LearningDraftFilter struct {
	Agent string
	Limit int
}

// LearningCandidateView 是候选的只读投影（证据与来源关系可复核）。
// LearningCandidateView is the read-only candidate projection used by the console.
type LearningCandidateView struct {
	EntryID               int64
	State                 string
	Content               string
	Kind                  string
	Certainty             string
	Confidence            float64
	Evidence              []LearningEvidence
	SourceEntryIDs        []int64
	SuggestedSupersedesID int64
	OriginCallerID        string
	OriginKeyID           string
	EvidenceAt            time.Time
	ValidTo               time.Time
	TimeConfidence        string
	CreatedAt             time.Time
}

// LearningExtractionView 是一次抽取的读模型：账本行 + 候选 + 草案。
// LearningExtractionView is the read model of one extraction with its candidates and drafts.
type LearningExtractionView struct {
	Extraction LearningExtraction
	Candidates []LearningCandidateView
	Drafts     []LearningDraftView
}

// LearningDraftView 是草案的只读投影。
// LearningDraftView is the read-only draft projection.
type LearningDraftView struct {
	DraftID       string
	ExtractionID  string
	Agent         string
	Name          string
	Content       string
	ContentSHA256 string
	Evidence      []LearningEvidence
	RelDir        string
	CreatedAt     time.Time
}

// LearningStatus 是学习面的状态读模型（预算、待决与未知计数、版本）。
// LearningStatus is the status read model: budget, pending/unknown counts and versions.
type LearningStatus struct {
	Enabled            bool
	Agents             []string
	UnknownAgents      []string
	RuleVersion        string
	PromptVersion      string
	AttemptsUsed       int
	AttemptLimit       int
	Exhausted          bool // AttemptsUsed >= AttemptLimit；状态页与 CLI 直接展示
	BudgetWindow       time.Duration
	PendingExtractions int
	UnknownExtractions int
	OrphanDrafts       int
	LastExtractionAt   time.Time
}

// LearningAdmin 是只读学习查询面（web / CLI 消费）。**没有写方法**：确认与删除复用
// 既有记忆管理动作，本端口不提供晋升、重试或发布。
// LearningAdmin is the read-only learning surface. It has no write methods: confirm and
// delete stay with the existing memory admin actions.
type LearningAdmin interface {
	Status(ctx context.Context) (LearningStatus, error)
	ListExtractions(ctx context.Context, filter LearningExtractionFilter) ([]LearningExtractionView, error)
	LoadExtraction(ctx context.Context, extractionID string) (LearningExtractionView, error)
	ListDrafts(ctx context.Context, filter LearningDraftFilter) ([]LearningDraftView, error)
	LoadDraft(ctx context.Context, draftID string) (LearningDraftView, error)
}

// LearningHandoffRequest 是一次受控转交请求（`learning handoff`，C-19）：
// 草案标识、技能名、技能目录文件与来源声明。类型放在 core 而不是装配层，是因为
// 命令面（cli）只认 core 类型——app 的薄适配实现同一形状，契约与实现在类型上分离。
// Files 与 SourceJSON 由命令面经有界读取器取得后原样转交；严格解码与字段强制在
// 薄适配里做（单一份实现）。
// LearningHandoffRequest is one controlled handoff request. It lives in core so the
// command surface can name the contract without importing the assembly layer; the
// strict decoding and forced fields stay in the thin adapter.
type LearningHandoffRequest struct {
	Actor      Actor
	DraftID    string
	SkillName  string
	Files      []AuthorFile
	SourceJSON []byte
}

// LearningHandoffResult 只含草案元数据与 propose 回执——评估 / 批准 / 发布仍在
// US-21 的准入命令里进行，handoff 不返回、也不触发任何后续状态。
// The result carries the draft metadata and the propose receipt, and nothing else:
// evaluation, approval and publication stay with the admission commands.
type LearningHandoffResult struct {
	Draft     LearningDraftView
	Candidate AdmissionCandidate
}

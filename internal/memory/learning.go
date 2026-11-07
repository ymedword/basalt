package memory

// 本文件承载 US-22 受控自我进化的**抽取服务**（颗粒度文档 §2.3.1 / §3.1.4 / §3.1.5）：
// Run 终态之后唯一的同步抽取入口。流程与 §2.3.1 伪代码逐块一致——启用判定 → 未决阻断 →
// 窗口 / 前文 / 条目快照 → BeginExtraction（预算与来源复制在同一事务）→ 提示词 →
// ChatMemoryExtract（两级预写审计、一次调用）→ 严格解析与逐项校验 → 只读草案 → 候选 →
// CommitExtraction。服务没有返回值，因此**不可能**改变 Run 的终态、返回或错误（原则 VII）。
//
// 三条不可越过的边界：
//  1. 模型输出是不可信数据：解析在 extract_output.go，逐项校验在 extract_validate.go；
//     本文件只负责把两者的结果送进提交事务，不自行放宽规则。
//  2. 来源身份只由存储层从 runs 行复制（BeginExtraction / CommitExtraction），服务
//     不接触 Principal，也不在重试时重贴身份（§2.3.3）。
//  3. 日志与失败消息只含身份与计数（C-30）：正文、证据引文、凭证一律不落日志、不进
//     memory_extractions.error_message。
//
// The synchronous extraction service behind the single run-terminal hook. The flow is
// the card's §2.3.1 pseudo-code block by block; the model output is untrusted and its
// parsing/validation lives in the sibling files, and origin identity never passes
// through this service.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

// SourceAutoExtract 是自动抽取候选的写入来源（追加到 memory_entries.source 闭集）。
// 只有 CommitExtraction 的受信任路径写入；普通 Append / Propose 没有来源入参，调用方
// 无法把别的写入伪装成自动抽取。storage 侧的 Go 闭集与迁移 0011 的 DB CHECK 同源。
// SourceAutoExtract is the source literal written only by the trusted commit path.
const SourceAutoExtract = "auto_extract"

// 版本常量是代码常量，不是配置键；改规则必须同时改版本号，旧抽取行不复用新规则解释。
// Prompt/rule versions are code constants; changing a rule requires bumping its version.
const (
	learningPromptVersion = "memory-extract-v1"
	learningRuleVersion   = "memory-rules-v1"
)

// 草案文件名与目录形态由程序固定（C-18）：模型只能提供受校验的 name 与正文。
// Draft file names and the directory shape are fixed by the program.
const (
	learningDraftFile  = "DRAFT.md"
	learningEvidence   = "evidence.json"
	learningDraftRoot  = ".basalt/learning/drafts"
	learningDraftIDPfx = "dft_"
)

// 以下三个导出别名服务装配层（状态页版本字段与孤儿草案计数的目录前缀）：
// 字面量与判定只保留在这一处，app **不得**复制一份——复制出来的第二份会在改规则
// 或改前缀时与抽取行漂移。
// Exported aliases for the assembly layer: the version literals and the draft
// directory prefix stay defined here only, so no second copy can drift.
const (
	LearningPromptVersion = learningPromptVersion
	LearningRuleVersion   = learningRuleVersion
	LearningDraftIDPrefix = learningDraftIDPfx
)

// failureWriteTimeout 是失败终态回填的独立预算（§3.1.12 的内部常量），并从钩子总截止
// 截断：模型耗尽工作段、Provider 再花至多 5s 回填审计之后，失败行仍在 20s 总预算内。
// The failure terminal write gets its own 2s budget, capped by the hook's total deadline.
const learningFailureWriteTimeout = 2 * time.Second

// LearningStore 是抽取服务消费的窄写端口；实现落在 storage/postgres。
// LearningEntrySnapshot 由 extract_validate.go 定义（提示词与校验共用），本文件直接复用。
// 契约细节见颗粒度文档 §3.1.4：UnresolvedForOrigin 只返回需要阻断或重试的行；
// BeginExtraction 在单事务内重判未决与尝试上限并复制来源；CommitExtraction 在单事务内
// 写候选 / 元数据 / 来源关系 / 草案行 / 成功状态并推进游标。
//
// 三个写方法都收**触发 Run** 的执行权凭证（US-24 §2.3.5 / FR-24-36）：非零 token 时
// 存储层必须先锁该 Run 的租约行并校验 owner + epoch + 未释放 + 未过期，再进业务写入；
// 零值只出现在未接执行权的装配（SQLite 谱系 / 轻装配），存储层退化为无 fence 路径。
// fence 对象恒为 token.ResourceID（= 本次 Run），**不得**改用抽取行上的 run_id 或
// last_attempt_run_id。FailExtraction 另多收两个预期值供 window_gone 受控收尾使用
// （pending 分支忽略它们）。
// The three write methods carry the triggering run's ownership credential; a non-zero
// token must be fenced on that run's lease before any business write.
type LearningStore interface {
	UnresolvedForOrigin(ctx context.Context, agent, sessionID, runID string) (*core.LearningExtraction, error)
	LoadCursor(ctx context.Context, agent, sessionID, runID string) (core.LearningCursor, bool, error)
	EntriesForExtraction(ctx context.Context, scope core.Scope, limit, maxBytes int) ([]LearningEntrySnapshot, error)
	BeginExtraction(ctx context.Context, row core.LearningExtraction, token core.LeaseToken) (core.LearningExtraction, error)
	FailExtraction(ctx context.Context, extractionID, triggerRunID string, token core.LeaseToken,
		expectedPrev string, expectedAttempts int,
		class core.LearningFailureClass, message string, failedAt time.Time) error
	CommitExtraction(ctx context.Context, req core.CommitLearningExtraction, token core.LeaseToken) (core.LearningCommitResult, error)
}

// LearningMessageStore 是消息读面的专用端口：只读、带游标与双字节上限；不新增追加路径。
// LearningMessageStore is the read-only message-window port; it never appends.
type LearningMessageStore interface {
	MessagesForExtraction(ctx context.Context, sessionID, runID string, afterMessageID int64,
		priorLimit, newLimit, priorBytes, newBytes int) (core.LearningWindow, error)
	MessagesForFrozenWindow(ctx context.Context, sessionID, originCallerID, originKeyID string,
		fromMessageID, toMessageID int64, priorLimit, priorBytes int) (core.LearningWindow, error)
}

// LearningAdminStore 是只读查询与恢复查证的端口（app 消费；实现在 storage/postgres）。
// 它**没有**写学习结果的业务方法：恢复只查证不重放，候选晋升仍走既有记忆管理动作。
// Recovery 两个入口只处理 pending 行；List / Load 是管理台与 CLI 的只读投影。
//
// 两个 pending 读面共享**同一归属判据**（P-18）：最近一次尝试的 Run（last_attempt_run_id）
// 已无有效 run 租约才入选——属主仍活着时多 Worker 下由它自己收尾，别人不得抢先查证。
// 区别只在用途：PendingExtractions 供启动扫描（全量），OrphanPendingExtractions 供回收器
// B3（每轮有界）。RecoverExtraction 收预期值并在事务内重检，结论过期时零写入跳过。
// Both pending readers share the same ownership predicate; RecoverExtraction re-checks
// the expectation inside its transaction and skips with zero writes when it went stale.
type LearningAdminStore interface {
	PendingExtractions(ctx context.Context) ([]core.LearningExtraction, error)
	OrphanPendingExtractions(ctx context.Context, limit int) ([]core.LearningExtraction, error)
	ExtractionCandidateCount(ctx context.Context, extractionID string) (int, error)
	ExtractionDraftCount(ctx context.Context, extractionID string) (int, error)
	RecoverExtraction(ctx context.Context, req core.RecoverLearningExtraction) (bool, error)
	ListExtractions(ctx context.Context, filter core.LearningExtractionFilter) ([]core.LearningExtractionView, error)
	LoadExtraction(ctx context.Context, extractionID string) (core.LearningExtractionView, bool, error)
	ListDrafts(ctx context.Context, filter core.LearningDraftFilter) ([]core.LearningDraftView, error)
	LoadDraft(ctx context.Context, draftID string) (core.LearningDraftView, bool, error)
	LearningCounts(ctx context.Context, since time.Time) (LearningCounts, error)
	LastExtractionAt(ctx context.Context) (time.Time, bool, error)
}

// 确认处置的闭集字面量（§3.1.8）：空串（无 body）等价 keep_both，**不是**第三种处置。
// 字面量只在这里定义一份，web 的请求校验与管理面共用，不得各写一个副本。
// The closed set of confirmation resolutions; an empty value means keep_both.
const (
	ResolutionKeepBoth  = "keep_both"
	ResolutionSupersede = "supersede"
)

// CandidateConfirmStore 是确认处置的窄事务端口；只有**自动候选**进入该事务，显式提议
// 候选仍走既有 Confirm。MemoryStore 不必实现它，管理面在需要时做类型断言，未实现即
// 返回「未装配」而不是退化成 keep_both。
// ConfirmCandidate 在同一事务内按固定顺序校验：候选行 FOR UPDATE 且 state='candidate'
// CAS → 候选 valid_to 未过 → supersede 时校验目标与记忆中的建议逐字相等且仍 active →
// 来源条目 FOR SHARE 全部 active 且未过期 → 来源 Key 未撤销未过期 → 来源调用方未停用。
// CandidateConfirmStore is the narrow promotion transaction consumed only by the admin
// surface for auto-extracted candidates; the memory store may or may not implement it.
type CandidateConfirmStore interface {
	LoadCandidateMeta(ctx context.Context, entryID int64) (core.MemoryCandidateMeta, bool, error)
	ConfirmCandidate(ctx context.Context, req ConfirmCandidateRequest) (ConfirmCandidateResult, error)
}

// ConfirmCandidateRequest 是一次确认事务的全部入参。`Now` 由管理面传入（与审计同一个
// 时钟），事务内的时间判据都用它，**不得**在 SQL 里另取 now 导致判据不可复现。
// ConfirmCandidateRequest carries every input of one promotion transaction.
type ConfirmCandidateRequest struct {
	EntryID           int64
	Resolution        string
	SupersedesEntryID int64
	ConfirmedBy       string
	Now               time.Time
}

// ConfirmCandidateResult 是事务提交后的回执：晋升的候选与被替代的旧条（0 = 不适用）。
// ConfirmCandidateResult reports the promoted row and the replaced one, if any.
type ConfirmCandidateResult struct {
	EntryID      int64
	SupersededID int64
}

// LearningCounts 是状态页的计数投影：滚动窗口内的尝试数、待决与未查证数。
// LearningCounts is the status projection: rolling attempts, pending and unverified.
type LearningCounts struct {
	Attempts int
	Pending  int
	Unknown  int
}

// LearningLimits 是窗口、预算与输出校验的固定上限（§3.1.12）。它们是代码常量，不是
// 配置键；零值字段由 normaliseLimits 补默认值。
// LearningLimits carries the fixed limits of §3.1.12; zero fields take the defaults.
type LearningLimits struct {
	WindowMessages      int
	WindowBytes         int
	PriorMessages       int
	PriorBytes          int
	EntrySnapshot       int
	EntrySnapshotBytes  int
	AttemptsPerAgent    int
	BudgetWindow        time.Duration
	TotalTimeout        time.Duration
	Timeout             time.Duration
	failureWriteTimeout time.Duration
	MaxItems            int
	MaxContentBytes     int
	MaxEvidence         int
	MaxSourceEntries    int
	MaxOutputBytes      int
	FailuresBeforeSkip  int
}

// LearningDeps 是抽取服务的装配面。模型调用客户端**不在**这里——每次调用取
// LearningRunInput.Extractor（本次 Run 准入时冻结的审计视图），否则抽取会绕过准入
// 固定表（原则 III）。
// LearningDeps is the assembly surface; the model client is deliberately absent.
type LearningDeps struct {
	Store     LearningStore
	Messages  LearningMessageStore
	Enabled   bool
	Agents    []string
	DraftRoot string // 绝对路径：<workspace>/.basalt/learning/drafts
	Limits    LearningLimits
	Logger    *slog.Logger
	Now       func() time.Time
}

// LearningService 是 core.LearningHook 的唯一实现。
// LearningService is the only core.LearningHook implementation.
type LearningService struct {
	store    LearningStore
	messages LearningMessageStore
	enabled  bool
	agents   map[string]struct{}
	// draftRoot 为空 = 草案不可写：method 项按写盘失败丢弃（不写记忆条目）。
	draftRoot string

	limits LearningLimits
	rules  extractRules
	logger *slog.Logger
	now    func() time.Time

	initErr error
}

var _ core.LearningHook = (*LearningService)(nil)

// DefaultLearningLimits 返回 §3.1.12 的缺省上限。
// DefaultLearningLimits returns the defaults from module card §3.1.12.
func DefaultLearningLimits() LearningLimits {
	return LearningLimits{
		WindowMessages:      40,
		WindowBytes:         8192,
		PriorMessages:       6,
		PriorBytes:          2048,
		EntrySnapshot:       20,
		EntrySnapshotBytes:  8192,
		AttemptsPerAgent:    200,
		BudgetWindow:        24 * time.Hour,
		TotalTimeout:        20 * time.Second,
		Timeout:             13 * time.Second,
		failureWriteTimeout: learningFailureWriteTimeout,
		MaxItems:            5,
		MaxContentBytes:     4096,
		MaxEvidence:         3,
		MaxSourceEntries:    5,
		MaxOutputBytes:      65536,
		FailuresBeforeSkip:  3,
	}
}

// normaliseLimits 在缺省值之上覆盖调用方显式给出的字段；保持「零值 = 缺省」的语义，
// 避免部分字段为零时把窗口或输出上限判成 0。
// normaliseLimits overlays explicit fields on the defaults so a zero field never
// silently becomes a zero cap.
func normaliseLimits(limits LearningLimits) LearningLimits {
	out := DefaultLearningLimits()
	if limits.WindowMessages > 0 {
		out.WindowMessages = limits.WindowMessages
	}
	if limits.WindowBytes > 0 {
		out.WindowBytes = limits.WindowBytes
	}
	if limits.PriorMessages > 0 {
		out.PriorMessages = limits.PriorMessages
	}
	if limits.PriorBytes > 0 {
		out.PriorBytes = limits.PriorBytes
	}
	if limits.EntrySnapshot > 0 {
		out.EntrySnapshot = limits.EntrySnapshot
	}
	if limits.EntrySnapshotBytes > 0 {
		out.EntrySnapshotBytes = limits.EntrySnapshotBytes
	}
	if limits.AttemptsPerAgent > 0 {
		out.AttemptsPerAgent = limits.AttemptsPerAgent
	}
	if limits.BudgetWindow > 0 {
		out.BudgetWindow = limits.BudgetWindow
	}
	if limits.TotalTimeout > 0 {
		out.TotalTimeout = limits.TotalTimeout
	}
	if limits.Timeout > 0 {
		out.Timeout = limits.Timeout
	}
	if limits.failureWriteTimeout > 0 {
		out.failureWriteTimeout = limits.failureWriteTimeout
	}
	if limits.MaxItems > 0 {
		out.MaxItems = limits.MaxItems
	}
	if limits.MaxContentBytes > 0 {
		out.MaxContentBytes = limits.MaxContentBytes
	}
	if limits.MaxEvidence > 0 {
		out.MaxEvidence = limits.MaxEvidence
	}
	if limits.MaxSourceEntries > 0 {
		out.MaxSourceEntries = limits.MaxSourceEntries
	}
	if limits.MaxOutputBytes > 0 {
		out.MaxOutputBytes = limits.MaxOutputBytes
	}
	if limits.FailuresBeforeSkip > 0 {
		out.FailuresBeforeSkip = limits.FailuresBeforeSkip
	}
	return out
}

// NewLearningService 组装抽取服务。启用但缺少存储 / 消息端口时记下装配错误：每次
// AfterRun 记 ERROR 并放弃（fail-closed），**不得**静默跳过。
// NewLearningService assembles the service; an enabled service without its ports fails
// closed on every call instead of silently skipping.
func NewLearningService(deps LearningDeps) *LearningService {
	limits := normaliseLimits(deps.Limits)
	s := &LearningService{
		store:     deps.Store,
		messages:  deps.Messages,
		enabled:   deps.Enabled,
		agents:    make(map[string]struct{}, len(deps.Agents)),
		draftRoot: deps.DraftRoot,
		limits:    limits,
		rules: extractRules{
			MaxItems:         limits.MaxItems,
			MaxContentBytes:  limits.MaxContentBytes,
			MaxEvidence:      limits.MaxEvidence,
			MaxSourceEntries: limits.MaxSourceEntries,
			MaxOutputBytes:   limits.MaxOutputBytes,
		},
		logger: deps.Logger,
		now:    deps.Now,
	}
	for _, agent := range deps.Agents {
		s.agents[agent] = struct{}{}
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if deps.Enabled && (len(deps.Agents) == 0 || deps.Store == nil || deps.Messages == nil) {
		s.initErr = fmt.Errorf("memory: learning service is enabled without its required ports")
	}
	return s
}

// enabledFor 是启用判定的唯一点：开关、Agent 名单与渠道三者同时满足才抽取。渠道不属于
// {web, cli} 的 Run（定时 / Flow 等）永不触发抽取。
// enabledFor is the single enablement predicate: switch, agent list and channel.
func (s *LearningService) enabledFor(agent, channel string) bool {
	if !s.enabled {
		return false
	}
	if channel != "web" && channel != "cli" {
		return false
	}
	_, ok := s.agents[agent]
	return ok
}

// AfterRun 与 §2.3.1 伪代码逐块一致；无返回值，因此不可能改变 Run 结果。
// AfterRun follows the §2.3.1 pseudo-code block by block and returns nothing.
func (s *LearningService) AfterRun(ctx context.Context, in core.LearningRunInput, res core.RunResult) {
	if res.Status != core.StatusCompleted {
		return
	}
	if !s.enabled {
		return
	}
	if !s.enabledFor(in.Agent, in.Channel) {
		if _, ok := s.agents[in.Agent]; !ok {
			// 未列名的 Agent 只留 Debug（C-01 / 失败矩阵）：不读库、不调用。
			s.logger.DebugContext(ctx, "学习抽取：Agent 不在启用名单", "agent", in.Agent)
		}
		return
	}
	if s.initErr != nil {
		s.logger.ErrorContext(ctx, "学习抽取：服务未完整装配，放弃本次抽取",
			"agent", in.Agent, "run_id", in.RunID)
		return
	}
	if in.Extractor == nil {
		s.logger.ErrorContext(ctx, "学习抽取：Runner 未装配冻结的抽取客户端，放弃",
			"agent", in.Agent, "run_id", in.RunID)
		return
	}

	totalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.limits.TotalTimeout)
	defer cancel()
	workCtx, cancelWork := context.WithTimeout(totalCtx, s.limits.Timeout)
	defer cancelWork()
	defer s.recoverPanic(totalCtx, in)

	// UnresolvedForOrigin 只返回需要处理的行：pending、failed(attempts<3)、或
	// verified_at IS NULL 的 unknown；已查证 unknown 与其他终态行返回 nil。
	unresolved, err := s.store.UnresolvedForOrigin(workCtx, in.Agent, in.SessionID, in.RunID)
	if err != nil {
		s.logger.WarnContext(workCtx, "学习抽取：读取未决抽取失败", "agent", in.Agent, "error", err)
		return
	}
	// 防御：存储层若误返回已查证 unknown，也不得阻断或把它当 failed 重试。
	if unresolved != nil && unresolved.Status == core.LearningExtractionUnknown && !unresolved.VerifiedAt.IsZero() {
		unresolved = nil
	}
	if unresolved != nil && !unresolved.Retryable() { // pending，或尚未查证的 unknown：阻断
		s.logger.WarnContext(workCtx, "学习抽取：同来源存在未决窗口，阻断",
			"agent", in.Agent, "extraction_id", unresolved.ExtractionID, "status", unresolved.Status)
		return
	}

	var (
		window core.LearningWindow
		row    core.LearningExtraction
	)
	if unresolved != nil { // failed 且 attempts < 3：只重试冻结窗口，身份随本次 Run 更新
		window, err = s.messages.MessagesForFrozenWindow(workCtx, unresolved.SessionID,
			unresolved.OriginCallerID, unresolved.OriginKeyID,
			unresolved.WindowFromMessageID, unresolved.WindowToMessageID,
			s.limits.PriorMessages, s.limits.PriorBytes)
		row = *unresolved
		row.LastAttemptRunID = in.RunID // 第 2/3 次尝试：绑定本次触发 Run
		row.LastAttemptAt = in.CompletedAt
		row.Provider, row.Model, row.ConfigHash = in.Provider, in.Model, in.ConfigHash
		row.PromptVersion, row.RuleVersion = learningPromptVersion, learningRuleVersion
		// 消息可被保留策略清理：实际条数少于冻结条数即视为窗口缺失，
		// 记 skipped / window_gone 并推进游标，不用残缺集合抽取（C-33）。
		if err == nil && len(window.New) != row.WindowMessages {
			// 预期值取**扫描时**的行值（unresolved），不是 row——row 的
			// LastAttemptRunID 已被改写成本次触发 Run，而这次 Run 并没有真正尝试。
			s.fail(totalCtx, in, row, unresolved.LastAttemptRunID, unresolved.Attempts,
				core.LearningFailureWindowGone,
				fmt.Errorf("frozen window incomplete: want %d messages, got %d", row.WindowMessages, len(window.New)))
			return
		}
	} else {
		cursor, _, cerr := s.store.LoadCursor(workCtx, in.Agent, in.SessionID, in.RunID)
		if cerr != nil {
			s.logger.WarnContext(workCtx, "学习抽取：读取游标失败", "agent", in.Agent, "error", cerr)
			return
		}
		window, err = s.messages.MessagesForExtraction(workCtx, in.SessionID, in.RunID,
			cursor.CommittedMessageID, s.limits.PriorMessages, s.limits.WindowMessages,
			s.limits.PriorBytes, s.limits.WindowBytes)
		row = core.LearningExtraction{
			ExtractionID:          core.NewLearningExtractionID(),
			Agent:                 in.Agent,
			SessionID:             in.SessionID,
			RunID:                 in.RunID,
			LastAttemptRunID:      in.RunID,
			LastAttemptAt:         in.CompletedAt,
			WindowFromMessageID:   window.FromMessageID,
			WindowToMessageID:     window.ToMessageID,
			WindowMessages:        len(window.New),
			DroppedWindowMessages: window.Dropped,
			PriorMessages:         len(window.Prior),
			PromptVersion:         learningPromptVersion,
			RuleVersion:           learningRuleVersion,
			Provider:              in.Provider,
			Model:                 in.Model,
			ConfigHash:            in.ConfigHash,
			CreatedAt:             in.CompletedAt,
		}
	}
	if err != nil || len(window.New) == 0 {
		if err != nil {
			s.logger.WarnContext(workCtx, "学习抽取：读取消息窗口失败", "agent", in.Agent, "error", err)
		}
		return
	}

	entries, err := s.store.EntriesForExtraction(workCtx, core.Scope{Agent: in.Agent},
		s.limits.EntrySnapshot, s.limits.EntrySnapshotBytes)
	if err != nil {
		s.logger.WarnContext(workCtx, "学习抽取：读取既有条目失败", "agent", in.Agent, "error", err)
		return
	}
	row.ExistingEntries = len(entries)
	// 预算、未决阻断与来源复制都在同一事务内；origin 由 runs 行复制，不取当前 Principal。
	// 重试时本事务同时写入 last_attempt_run_id / last_attempt_at 与本次尝试的
	// provider / model / config_hash；返回行即本次尝试的调用参数，不得改用旧行值。
	row, err = s.store.BeginExtraction(workCtx, row, in.Token)
	if err != nil {
		if errors.Is(err, core.ErrLearningBudgetExhausted) {
			s.logger.WarnContext(workCtx, "学习抽取：尝试次数上限已耗尽，跳过",
				"agent", in.Agent, "limit", s.limits.AttemptsPerAgent, "window", s.limits.BudgetWindow)
		} else {
			s.logger.WarnContext(workCtx, "学习抽取：预写抽取行失败，拒绝发出",
				"agent", in.Agent, "error", err)
		}
		return
	}
	prompt := s.buildPrompt(row, window, entries)
	// row.Provider / row.Model / row.ConfigHash 是本次尝试的身份（重试时已随本次 Run 更新）；
	// 预写在 ChatMemoryExtract 内受 workCtx 截止约束，不得外扩独立 5s。
	reply, err := in.Extractor.ChatMemoryExtract(workCtx, core.ChatRequest{
		RunID:        in.RunID,
		SessionID:    in.SessionID,
		Round:        0,
		Provider:     row.Provider,
		Model:        row.Model,
		ConfigHash:   row.ConfigHash,
		Messages:     prompt,
		HookDeadline: s.hookDeadline(totalCtx),
	})
	if err != nil {
		// 本轮已 Begin，pending 分支按本轮绑定的触发 Run 断言，预期值不适用。
		s.fail(totalCtx, in, row, "", 0, s.classifyFailure(err), err) // 独立 2s，从总预算起点计
		return
	}
	items, dropped, err := s.parseAndValidate(workCtx, row, reply.Content, window, entries)
	if err != nil {
		s.fail(totalCtx, in, row, "", 0, core.LearningFailureInvalidOutput, err)
		return
	}
	drafts, draftDropped := s.writeDrafts(workCtx, row, items)
	candidates := s.buildCandidates(row, items)
	dropped += draftDropped
	if _, err := s.store.CommitExtraction(workCtx, core.CommitLearningExtraction{
		ExtractionID:  row.ExtractionID,
		Candidates:    candidates,
		Drafts:        drafts,
		ItemsTotal:    len(items),
		ItemsAccepted: len(candidates) + len(drafts),
		ItemsDropped:  dropped,
		CompletedAt:   s.now(),
	}, in.Token); err != nil {
		s.logger.ErrorContext(workCtx, "学习抽取：提交结果未知，留 pending 交恢复查证",
			"extraction_id", row.ExtractionID, "agent", in.Agent, "error", err)
		return
	}
	s.logger.InfoContext(workCtx, "学习抽取完成",
		"extraction_id", row.ExtractionID, "agent", in.Agent,
		"accepted", len(candidates)+len(drafts), "dropped", dropped)
}

// learningPromptPayload 是程序序列化的用户消息：窗口、前文与既有条目快照。字段顺序与
// 结构固定，相同输入必须产生逐字节相同的提示词（确定性版本化）。
// learningPromptPayload is the deterministic user message; identical input must produce
// byte-identical output.
type learningPromptPayload struct {
	PromptVersion string                  `json:"prompt_version"`
	RuleVersion   string                  `json:"rule_version"`
	Prior         []learningPromptMessage `json:"prior"`
	Window        []learningPromptMessage `json:"window"`
	Entries       []learningPromptEntry   `json:"entries"`
}

type learningPromptMessage struct {
	MessageID int64  `json:"message_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

type learningPromptEntry struct {
	EntryID int64  `json:"entry_id"`
	Section string `json:"section"`
	State   string `json:"state"`
	Content string `json:"content"`
}

// learningSystemPrompt 是固定系统提示词（§3.1.13）：只输出 JSON、不解释、不引用窗口外
// 消息、不请求工具、证据逐字。内容不得包含凭证或配置。
// learningSystemPrompt is the fixed system prompt from §3.1.13.
const learningSystemPrompt = `你是 Basalt 的记忆抽取器。只输出一个 JSON 对象：{"items":[...]}。
不得输出解释、Markdown 代码块或任何 JSON 之外的内容。不得引用窗口之外的消息，不得请求或假设任何工具。
每个条目字段固定为：kind、content、name、confidence、evidence、source_entry_ids、suggested_supersedes_id、valid_to。
- kind 只能是 fact、preference、constraint、inference、temporary、method。
- content 不超过 4096 字节；为 method 时用「适用条件 / 步骤 / 反例」三节组织正文，并提供 name（不超过 100 字符）。
- evidence 为 1–3 条，每条含 message_id、role、quote；quote 必须是窗口内该消息正文的逐字子串，role 必须与消息角色一致。
- fact、preference、constraint、temporary 至少需要一条 role=user 的证据，否则会被降级为 inference；inference 允许只依据 assistant 消息。
- temporary 的 valid_to 必须是未来的 RFC3339 绝对时刻；相对日期不要猜测。
- source_entry_ids 只能引用 entries 快照中的 entry_id，最多 5 个；suggested_supersedes_id 只能引用快照中的 entry_id。
- 不得输出任何密钥、令牌或凭证。
- items 最多 5 条；没有值得长期保留的内容时输出 {"items":[]}。
只依据 window 中的消息判断事实；prior 只补足指代，不是证据来源。`

// buildPrompt 组装系统提示词与程序序列化的用户消息。窗口之外的正文（工具输出、system
// 消息、其他来源）在读取面已被剔除；凭证命中的消息**与既有条目**都不进入提示词
// （§3.1.13「提示词里不放凭证」），但仍保留在冻结窗口、证据校验面与来源子集判定里。
// buildPrompt assembles the fixed system prompt and a deterministic user payload.
func (s *LearningService) buildPrompt(row core.LearningExtraction, window core.LearningWindow, entries []LearningEntrySnapshot) []core.Message {
	payload := learningPromptPayload{
		PromptVersion: learningPromptVersion,
		RuleVersion:   learningRuleVersion,
		Prior:         make([]learningPromptMessage, 0, len(window.Prior)),
		Window:        make([]learningPromptMessage, 0, len(window.New)),
		Entries:       make([]learningPromptEntry, 0, len(entries)),
	}
	for _, message := range window.Prior {
		if TextHasCredential(message.Content) {
			continue
		}
		payload.Prior = append(payload.Prior, learningPromptMessage{
			MessageID: message.MessageID, Role: message.Role, Content: message.Content,
		})
	}
	for _, message := range window.New {
		if TextHasCredential(message.Content) {
			continue
		}
		payload.Window = append(payload.Window, learningPromptMessage{
			MessageID: message.MessageID, Role: message.Role, Content: message.Content,
		})
	}
	for _, entry := range entries {
		if TextHasCredential(entry.Content) {
			continue
		}
		payload.Entries = append(payload.Entries, learningPromptEntry{
			EntryID: entry.EntryID, Section: string(entry.Section), State: entry.State, Content: entry.Content,
		})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// 结构体字段全部可序列化；这里失败属于编排缺陷，返回空消息让调用失败关闭。
		encoded = []byte(`{"prompt_version":"` + learningPromptVersion + `","window":[],"prior":[],"entries":[]}`)
	}
	return []core.Message{
		{Role: "system", Content: learningSystemPrompt},
		{Role: "user", Content: string(encoded)},
	}
}

// parseAndValidate 委托严格解析与逐项校验；ctx 由调用链保留（固定签名），本函数自身的
// 工作不受其影响。丢弃数是本次调用的局部值，**不得**存进服务字段。
// parseAndValidate delegates to the strict parser and per-item validator.
func (s *LearningService) parseAndValidate(ctx context.Context, row core.LearningExtraction, out string, window core.LearningWindow, entries []LearningEntrySnapshot) ([]validatedItem, int, error) {
	parsed, err := parseExtractOutput(out, s.rules)
	if err != nil {
		return nil, 0, err
	}
	items, dropped := validateExtractItems(parsed, window, entries, s.now(), s.rules)
	return items, dropped, nil
}

// buildCandidates 把校验通过的普通条目转成候选：Agent 层、归档区、source 由提交事务
// 固定为 auto_extract。method 只进草案，**不得**出现在这里。
// buildCandidates maps accepted non-method items to agent-scoped archive candidates.
func (s *LearningService) buildCandidates(row core.LearningExtraction, items []validatedItem) []core.LearningCandidate {
	candidates := make([]core.LearningCandidate, 0, len(items))
	for _, item := range items {
		if item.Kind == core.LearningKindMethod {
			continue
		}
		candidates = append(candidates, core.LearningCandidate{
			Scope:                 core.Scope{Agent: row.Agent},
			Section:               core.SectionArchive,
			Content:               item.Content,
			Kind:                  item.Kind,
			Certainty:             item.Certainty,
			Confidence:            item.Confidence,
			Evidence:              item.Evidence,
			SourceEntryIDs:        item.SourceEntryIDs,
			SuggestedSupersedesID: item.SuggestedSupersedesID,
			EvidenceAt:            item.EvidenceAt,
			ValidTo:               item.ValidTo,
			TimeConfidence:        item.TimeConfidence,
		})
	}
	return candidates
}

// writeDrafts 把 method 条目写成固定布局的只读草案；写盘失败的条目计入 dropped，其余
// 条目照常提交。草案不写记忆条目、不执行脚本、不进任何 Agent 白名单。
// writeDrafts writes method items as read-only drafts; a failed write drops that item.
func (s *LearningService) writeDrafts(ctx context.Context, row core.LearningExtraction, items []validatedItem) ([]core.LearningDraft, int) {
	drafts := make([]core.LearningDraft, 0, len(items))
	dropped := 0
	for _, item := range items {
		if item.Kind != core.LearningKindMethod {
			continue
		}
		if ctx.Err() != nil {
			dropped++
			continue
		}
		draft, err := s.writeDraft(row, item)
		if err != nil {
			// C-30：失败日志只含身份与计数，不带路径、正文或错误原文。
			s.logger.WarnContext(ctx, "学习抽取：草案写入失败，丢弃该条",
				"extraction_id", row.ExtractionID, "agent", row.Agent)
			dropped++
			continue
		}
		drafts = append(drafts, draft)
	}
	return drafts, dropped
}

// writeDraft 写一份固定布局草案：DRAFT.md（frontmatter + 模型正文）与 evidence.json。
// 目录与文件名由程序固定，模型不能指定路径或文件名。
// writeDraft writes one draft with the fixed program-controlled layout.
func (s *LearningService) writeDraft(row core.LearningExtraction, item validatedItem) (core.LearningDraft, error) {
	if s.draftRoot == "" {
		return core.LearningDraft{}, fmt.Errorf("learning: draft root is not assembled")
	}
	draftID := learningDraftIDPfx + rand.Text()
	createdAt := s.now().UTC()
	relDir := path.Join(learningDraftRoot, draftID)
	targetDir := filepath.Join(s.draftRoot, draftID)
	// 目录先按属主可写建（0750），内容落盘后再与文件一起收紧到 0444 / 0555 的只读布局。
	// Create the directory writable by the owner first, then harden it with the files.
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		return core.LearningDraft{}, err
	}
	cleanup := func() {
		// 写盘失败不留下"有目录无数据库行"的孤儿；清理失败由恢复扫描报告。
		_ = os.RemoveAll(targetDir)
	}
	draftContent := renderDraft(row, item, draftID, createdAt)
	digest := sha256.Sum256([]byte(item.Content))
	evidenceJSON, err := json.Marshal(renderDraftEvidence(item.Evidence))
	if err != nil {
		cleanup()
		return core.LearningDraft{}, err
	}
	// 写入阶段先按属主可写（0600）落盘，收尾再统一收紧到只读；直接以 0444 创建会让
	// 写失败后的清理同样受限。最终布局仍是 0444 文件 + 0555 目录（FR-22-08）。
	// Write writable first and harden at the end; the final layout stays 0444 / 0555.
	draftPath := filepath.Join(targetDir, learningDraftFile)
	if err := os.WriteFile(draftPath, []byte(draftContent), 0o600); err != nil {
		cleanup()
		return core.LearningDraft{}, err
	}
	evidencePath := filepath.Join(targetDir, learningEvidence)
	if err := os.WriteFile(evidencePath, evidenceJSON, 0o600); err != nil {
		cleanup()
		return core.LearningDraft{}, err
	}
	// 内容落盘后再收紧为只读布局（先建内容再收权限，避免自己写不进去）；任一收紧失败
	// 都按写盘失败处理——草案必须以 0444 / 0555 落地，调用方把该条记为丢弃，抽取整体
	// 仍可成功，与其它写盘失败同一处置。注释同时纠正一处易误读点：草案**不在**任何
	// Agent 的文件白名单里，读取与写入都被沙箱策略拒绝（test: Draft_NotAgentReadable），
	// 0444 / 0555 是纵深防御、防进程内其它路径误写，不是"允许 Agent 读"。
	// gosec 的 G302 按普通数据文件 0600 判；0444 / 0555 是已审设计（FR-22-08），按设计
	// 保留并在验收报告登记豁免。
	// Harden only after the content is written; any chmod failure is a write failure. The
	// drafts are NOT readable by agents (the sandbox denies both read and write); the
	// read-only layout is defence in depth against accidental in-process rewrites.
	if err := os.Chmod(draftPath, 0o444); err != nil { // #nosec G302 -- 已审设计：草案文件固定 0444
		cleanup()
		return core.LearningDraft{}, fmt.Errorf("收紧草案文件只读布局（%s）：%w", relDir, err)
	}
	if err := os.Chmod(evidencePath, 0o444); err != nil { // #nosec G302 -- 已审设计：证据文件固定 0444
		cleanup()
		return core.LearningDraft{}, fmt.Errorf("收紧证据文件只读布局（%s）：%w", relDir, err)
	}
	if err := os.Chmod(targetDir, 0o555); err != nil { // #nosec G302 -- 已审设计：草案目录固定 0555
		cleanup()
		return core.LearningDraft{}, fmt.Errorf("收紧草案目录只读布局（%s）：%w", relDir, err)
	}
	return core.LearningDraft{
		DraftID:       draftID,
		ExtractionID:  row.ExtractionID,
		Agent:         row.Agent,
		Name:          item.Name,
		Content:       item.Content,
		ContentSHA256: hex.EncodeToString(digest[:]),
		Evidence:      item.Evidence,
		RelDir:        relDir,
		CreatedAt:     createdAt,
	}, nil
}

// renderDraft 生成 DRAFT.md：frontmatter 的五个键固定，正文是模型输出（已经过程序校验）。
// 字符串值一律用双引号转义，避免名称里的 YAML 元字符破坏 frontmatter。
// renderDraft renders DRAFT.md; every frontmatter string is quoted.
func renderDraft(row core.LearningExtraction, item validatedItem, draftID string, createdAt time.Time) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("draft_id: " + strconv.Quote(draftID) + "\n")
	b.WriteString("agent: " + strconv.Quote(row.Agent) + "\n")
	b.WriteString("extraction_id: " + strconv.Quote(row.ExtractionID) + "\n")
	b.WriteString("name: " + strconv.Quote(item.Name) + "\n")
	b.WriteString("created_at: " + strconv.Quote(createdAt.Format(time.RFC3339Nano)) + "\n")
	b.WriteString("---\n\n")
	b.WriteString("# " + item.Name + "\n\n")
	b.WriteString(item.Content)
	b.WriteString("\n")
	return b.String()
}

// draftEvidence 是 evidence.json 的固定形状；时刻用 RFC3339Nano（UTC）。
// draftEvidence is the fixed evidence.json shape.
type draftEvidence struct {
	MessageID int64  `json:"message_id"`
	RunID     string `json:"run_id"`
	Role      string `json:"role"`
	Quote     string `json:"quote"`
	MessageAt string `json:"message_at"`
}

func renderDraftEvidence(evidence []core.LearningEvidence) []draftEvidence {
	out := make([]draftEvidence, 0, len(evidence))
	for _, item := range evidence {
		out = append(out, draftEvidence{
			MessageID: item.MessageID,
			RunID:     item.RunID,
			Role:      item.Role,
			Quote:     item.Quote,
			MessageAt: item.MessageAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return out
}

// classifyFailure 把模型调用错误映射到闭集（§3.1.5）。解析 / 校验错误由调用方直接给
// invalid_output；冻结窗口完整性复核直接给 window_gone（终态 skipped）。
// classifyFailure maps call errors onto the fixed failure-class set.
func (s *LearningService) classifyFailure(cause error) core.LearningFailureClass {
	switch {
	case errors.Is(cause, core.ErrAuditWrite):
		return core.LearningFailureAudit
	case errors.Is(cause, context.DeadlineExceeded):
		return core.LearningFailureTimeout
	case errors.Is(cause, core.ErrLearningExtractorUnavailable):
		return core.LearningFailureUnavailable
	default:
		return core.LearningFailureModel
	}
}

// hookDeadline 返回钩子总截止；无截止时返回零值。只把外边界交给 Provider，5s 回填
// 预算由 Provider 在**回填开始时**与这个边界取较小值。
// hookDeadline returns the hook's absolute outer bound, or the zero time.
func (s *LearningService) hookDeadline(totalCtx context.Context) time.Time {
	if deadline, ok := totalCtx.Deadline(); ok {
		return deadline
	}
	return time.Time{}
}

// fail 用 context.WithoutCancel + 2s 的独立上下文写终态，并把截止截到钩子总截止；
// window_gone 走同一入口但由 FailExtraction 置 skipped 并推进游标。日志与错误消息
// 都不得包含正文、证据引文或凭证（C-30）。
//
// WithoutCancel 只是"取消后仍能写完"的时间预算，**不是**执行权证明（§2.3.5）：真正
// 的归属保护由 FailExtraction 在事务内按 in.Token 锁租约行完成。expectedPrev /
// expectedAttempts 只对 window_gone 受控收尾有意义——本轮没有 Begin，必须传**扫描时
// 读到的** last_attempt_run_id / attempts；pending 分支忽略它们，改按本轮绑定的
// 触发 Run 断言。
// fail writes the terminal state in its own bounded context, capped by the hook deadline.
// WithoutCancel is a time budget, not proof of ownership: the fence lives in
// FailExtraction. The expected values matter only on the pre-Begin window_gone path.
func (s *LearningService) fail(ctx context.Context, in core.LearningRunInput, row core.LearningExtraction,
	expectedPrev string, expectedAttempts int, class core.LearningFailureClass, cause error,
) {
	deadline := time.Now().Add(s.limits.failureWriteTimeout)
	if hookDeadline, ok := ctx.Deadline(); ok && hookDeadline.Before(deadline) {
		deadline = hookDeadline
	}
	failCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancel()
	// window_gone 的终态是 skipped（窗口缺失不是坏结果），日志与落库状态保持一致。
	status := "failed"
	if class == core.LearningFailureWindowGone {
		status = "skipped"
	}
	s.logger.WarnContext(failCtx, "学习抽取失败",
		"extraction_id", row.ExtractionID, "agent", row.Agent, "failure_class", string(class), "status", status)
	if err := s.store.FailExtraction(failCtx, row.ExtractionID, in.RunID, in.Token,
		expectedPrev, expectedAttempts, class, s.failureMessage(class, cause), s.now()); err != nil {
		s.logger.ErrorContext(failCtx, "学习抽取：失败终态写回失败，留待恢复",
			"extraction_id", row.ExtractionID, "agent", row.Agent, "failure_class", string(class), "error", err)
	}
}

// failureMessage 生成 ≤1 KiB 的失败摘要：凭证命中时整体抑制，超长按 UTF-8 边界截断。
// failureMessage builds a ≤1 KiB summary and suppresses credential-bearing text.
func (s *LearningService) failureMessage(class core.LearningFailureClass, cause error) string {
	if cause == nil {
		return string(class)
	}
	text := strings.TrimSpace(cause.Error())
	if text == "" {
		return string(class)
	}
	if core.TextHasSecret(text) {
		return string(class) + ": details suppressed"
	}
	const limit = 1024
	if len(text) > limit {
		text = truncateUTF8(text, limit)
	}
	return text
}

// truncateUTF8 在不超过 limit 字节的前提下截断，保证结果仍是合法 UTF-8。
// truncateUTF8 cuts at a UTF-8 boundary within the byte limit.
func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// recoverPanic 兜底钩子内的 panic：记 ERROR 后放弃本次抽取，Run 的终态与返回值不受影响。
// recoverPanic contains any panic inside the hook; the run result is unaffected.
func (s *LearningService) recoverPanic(ctx context.Context, in core.LearningRunInput) {
	if recovered := recover(); recovered != nil {
		// panic 值可能携带正文；日志只记事实，不回显值（C-30）。
		s.logger.ErrorContext(ctx, "学习抽取：钩子 panic，已回收（不改变 Run 结果）",
			"agent", in.Agent, "run_id", in.RunID)
	}
}

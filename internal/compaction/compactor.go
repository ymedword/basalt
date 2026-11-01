package compaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是**压缩引擎本体**（T083 主流程 / T086 链分流 / T087 降级）：预算判定 → 定范围 →
// 找切点 → 过滤与组摘要 → 调摘要 → 追加 → 按六态分流。它**不**组装提示词、**不**写消息仓储、
// **不**生成任务状态。
// The engine itself: budget decision, scope, cut, filter, summarize, append, and the
// six-outcome dispatch. It never assembles prompts, writes messages or authors task state.

// sessionPageSize 是会话消息的分页大小（`MessageReader.ListBySession` 一次取多少条）。
// 分页只避免单次全表读，**不**改变顺序与语义：读到的仍然是 `(session_id, seq)` 全序。
// sessionPageSize is only a batching size; reading stays a full ordered scan.
const sessionPageSize = 500

// sessionPageLimit 是单次压缩最多读的页数：超过即按"会话过大"报错而不是静默截断
// （静默截断会让覆盖起点悄悄前移，压缩范围与审计对不上）。
// sessionPageLimit bounds one compaction's session scan; exceeding it is an error, never a
// silent truncation.
const sessionPageLimit = 40

// Policy 是压缩引擎的**策略数值**，全部来自 `compaction.*` 配置键（设计 §3.3）。
// `core.CompactRequest` 已冻结、按契约不携带策略数值，故由 `app` 在构造时从配置一次性填入；
// 任一项为零值即视为**未接线**，`Compact` 直接拒绝而不是猜默认值（D-24：配置阈值不得写死在代码里）。
// Policy holds the compaction.* numbers. The frozen CompactRequest carries no policy, so the
// wiring layer fills this once; any zero value means "not wired" and compaction refuses.
type Policy struct {
	TriggerRatio      float64       // `compaction.trigger_ratio`（0 < r < 1）
	SafetyMarginRatio float64       // `compaction.safety_margin_ratio`
	KeepRecentGroups  int           // `compaction.keep_recent_groups`
	SummaryMaxTokens  int           // `compaction.summary_max_tokens`（摘要请求自身 max_tokens）
	SummaryInputBytes int           // `compaction.summary_input_tokens` 的字节近似（0 = 不限）
	SummaryTimeout    time.Duration // `compaction.timeout_seconds`
	StrategyVersion   string        // 切点 / 预算策略版本
	PromptVersion     string        // 摘要模板版本
	SummaryModel      string        // 摘要用的模型名（冻结映射里查表）
}

// valid 判定策略是否接线完整：缺一项都算未接线，**不得**用零值兜底跑起来。
// valid reports whether every policy number was wired; a missing one is a refusal, not a default.
func (p Policy) valid() bool {
	return p.TriggerRatio > 0 && p.TriggerRatio < 1 && p.SafetyMarginRatio >= 0 &&
		p.KeepRecentGroups >= 1 && p.SummaryMaxTokens > 0 && p.SummaryTimeout > 0 &&
		p.StrategyVersion != "" && p.PromptVersion != "" && p.SummaryModel != ""
}

// CompactorDeps 是压缩引擎的全部依赖（设计 §3.1.11）：消息端口、条目存储、摘要器、
// 来源校验器、估算器、时钟与策略。全部为接口，便于单测注入。
// CompactorDeps holds every collaborator of the engine, all interfaces so tests can inject.
type CompactorDeps struct {
	Messages   MessageReader
	Store      CompactionStore
	Summarizer Summarizer
	Validator  ValidityChecker
	Estimator  Estimator
	Clock      func() time.Time
	Policy     Policy
}

// Compactor 实现 `core.ContextCompactor`。它是**有状态**的，但状态只有两类、都只在本 Run
// 内有效：① `no_injectable_chain` 的粘性记忆（本 Run 余下轮次不再调用摘要）；
// ② 已生成但结局未定的摘要正文（同进程重提，不再发第二次调用）。
// 两者都**不**落库——它们不是业务事实，崩溃后由 `FindCommitted` 重建。
// Compactor keeps exactly two pieces of run-scoped memory: the sticky no_injectable_chain
// verdict and the not-yet-confirmed summary text. Neither is persisted.
type Compactor struct {
	deps CompactorDeps

	mu        sync.Mutex
	exhausted map[string]string // runID → reason_code
	rebuilt   map[string]bool   // runID → 本 Run 已尝试过换代重建
	pending   map[string]*preparedSummary
}

// NewCompactor 构造压缩引擎。`Estimator` 缺省用 `MessageEstimator`（与装配记录同源），
// 其余依赖缺省即拒绝——存储 / 摘要器 / 校验器没有安全默认值。
// NewCompactor builds the engine; only the estimator has a safe default.
func NewCompactor(deps CompactorDeps) *Compactor {
	if deps.Estimator == nil {
		deps.Estimator = MessageEstimator{}
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	return &Compactor{
		deps:      deps,
		exhausted: make(map[string]string),
		rebuilt:   make(map[string]bool),
		pending:   make(map[string]*preparedSummary),
	}
}

// Compact 实现 `core.ContextCompactor`：一轮模型调用前判断是否需要压缩，需要则生成条目。
//
// 返回值的三种形态：
//   - `Entry != nil, Degraded == false`：当前可注入的条目（可能是本轮的既有头，也可能是新生成的）；
//   - `Entry == nil, Degraded == false`：不需要压缩且没有可注入条目（与现状一致，零副作用）；
//   - `Entry == nil, Degraded == true`：压缩失败 / 不可注入，`ReasonCode` 必非空。
//
// 错误只用于**不可判定**与**未接线**：`ErrCompactionPending`（结局读不到，冻结前滚）、
// `ErrScopeInvalid`（请求或策略未接线）、`core.ErrUnauthorized`（身份零值）、
// `context.Canceled`（取消传播）。摘要失败、超限、无切点都是**可分类的降级**，走返回值。
// Errors are reserved for undecidable outcomes and unwired inputs; ordinary summarizer
// failures come back as a degradation with a reason code.
func (c *Compactor) Compact(ctx context.Context, req core.CompactRequest) (core.CompactResult, error) {
	if !req.Principal.Valid() {
		return core.CompactResult{}, core.ErrUnauthorized
	}
	if err := c.validate(req); err != nil {
		return core.CompactResult{}, err
	}
	// 链作用域**恰有一个**身份：会话范围只认 session_id，无状态范围才用 run_id。
	// `req.RunID` 始终承载触发记录与审计关联（trigger_run_id），不得混进链身份——
	// PG 落库的 `num_nonnulls(scope_session_id, scope_run_id) = 1` 会直接拒绝两者同填。
	// Exactly one identity: a session range is keyed by session_id only; run_id is
	// provenance, never part of the chain identity.
	scope := Scope{RunID: req.RunID}
	if req.SessionID != "" {
		scope = Scope{SessionID: req.SessionID}
	}

	if reason, ok := c.exhaustedReason(req.RunID); ok {
		// 重建后仍不可注入：本 Run 余下轮次不再调用摘要，直接降级（不得每轮计费重试）。
		return degraded(reason), nil
	}

	messages, err := c.loadMessages(ctx, req)
	if err != nil {
		return core.CompactResult{}, err
	}
	messages = AssignToolGroups(messages)
	if len(messages) == 0 {
		return core.CompactResult{}, nil
	}

	chain, err := c.deps.Store.LatestCommittedChain(ctx, scope)
	if err != nil {
		return core.CompactResult{}, err
	}
	head, err := c.qualifyChain(ctx, req.Principal, chain)
	if err != nil {
		return core.CompactResult{}, err
	}
	start := 0
	if head != nil {
		idx := indexOfMessage(messages, head.FirstKeptMessageID)
		if idx < 0 {
			// 头指向的保留起点不在本次窗口里（会话被裁剪 / 头来自另一段历史）：
			// 按断链处理，不沿用它的摘要——沿用等于把无从校验的内容注入进去。
			head = nil
		} else {
			start = idx
		}
	}

	limit := req.Budget.Limit()
	if limit <= 0 {
		return core.CompactResult{}, fmt.Errorf("%w: budget.limit=%d 非正", ErrScopeInvalid, limit)
	}
	safety := int(math.Ceil(c.deps.Policy.SafetyMarginRatio * float64(limit)))
	usable := limit - req.Budget.OutputReserve - safety
	if usable <= 0 {
		return core.CompactResult{}, fmt.Errorf("%w: usable_input=%d 非正（context_tokens 与预留 / 余量矛盾）",
			ErrScopeInvalid, usable)
	}

	projected := c.projected(req, messages[start:], head)
	if !c.triggered(projected, usable) {
		if head == nil {
			// 不触发且没有可注入条目：与未装配压缩的行为一致。
			return core.CompactResult{}, nil
		}
		return core.CompactResult{Entry: summaryOf(*head, core.SummaryValid)}, nil
	}

	plan := plannedAttempt{
		scope:       scope,
		epoch:       1,
		coveredFrom: messages[0].MessageID,
		runID:       req.RunID,
		baseAttempt: req.AttemptID,
	}
	if head != nil {
		plan.epoch = clampEpoch(head.ChainEpoch)
		plan.parentID = head.CompactionID
		plan.coveredFrom = head.FirstKeptMessageID
		plan.parentAudit = head.SourceRefs
		plan.parentIncluded = head.IncludedRefs
		plan.previous = head.Summary
	}

	rebaseLeft, rebuildLeft, retryLeft := 1, 1, 1
	for {
		coveredStart := indexOfMessage(messages, plan.coveredFrom)
		if coveredStart < 0 {
			return degraded(ReasonStale), nil
		}
		window := messages[coveredStart:]
		cutRel, ok := FindCut(window, c.deps.Policy.KeepRecentGroups)
		if !ok {
			// 没有可压缩的完整工具组：不触发"压缩全部"，按失败降级。
			if projected > usable {
				return degraded(ReasonCompactionOverLimit), nil
			}
			return degraded(ReasonCompactionFailed), nil
		}
		covered := window[:cutRel]
		cutID := covered[len(covered)-1].MessageID
		firstKept := window[cutRel].MessageID
		tokensBefore := c.projected(req, window, summaryEntry(plan.previous))

		// 崩溃 / 重启恢复：同一覆盖范围已有**链资格通过**的条目 → 直接复用，零摘要调用。
		existing, hit, err := c.deps.Store.FindCommitted(ctx, scope, plan.coveredFrom, cutID)
		if err != nil {
			return core.CompactResult{}, err
		}
		if hit {
			ok, _, err := c.refsInjectable(ctx, req.Principal, existing.IncludedRefs)
			if err != nil {
				return core.CompactResult{}, err
			}
			if ok {
				c.forgetPending(pendingKey(scope, plan.coveredFrom, cutID))
				return core.CompactResult{Entry: summaryOf(existing, core.SummaryValid)}, nil
			}
		}

		prepared, err := c.prepare(ctx, req, plan, covered, window[cutRel:], cutID, firstKept, tokensBefore)
		if err != nil {
			return c.degradeOrReturn(ctx, err)
		}

		entry := c.buildEntry(prepared, plan)
		res, err := c.deps.Store.Append(ctx, entry)
		if err != nil {
			return core.CompactResult{}, err
		}
		switch res.Outcome {
		case AppendCommitted:
			c.forgetPending(pendingKey(scope, plan.coveredFrom, cutID))
			c.clearExhausted(req.RunID)
			committed := res.Entry
			if committed.CompactionID == "" {
				committed = entry
			}
			return core.CompactResult{Entry: summaryOf(committed, core.SummaryValid)}, nil

		case AppendRetryable:
			// 原事务**确认未落**：换 attempt_id 重提同一条摘要，不再发摘要调用、不再预留。
			if retryLeft == 0 {
				return degraded(ReasonAppendPending), nil
			}
			retryLeft--
			plan.attempt++
			continue

		case AppendSuperseded:
			// 头已前进或切点已变：按新头重算范围并**重新生成**摘要，旧摘要不得重提。
			if rebaseLeft == 0 || res.Head == nil {
				return degraded(ReasonStale), nil
			}
			rebaseLeft--
			next := *res.Head
			idx := indexOfMessage(messages, next.FirstKeptMessageID)
			if idx < 0 || idx >= coveredStart+cutRel {
				// 新头的保留起点已经越过本次切点：没有可压缩的增量。
				return degraded(ReasonStale), nil
			}
			plan.epoch = clampEpoch(next.ChainEpoch)
			plan.parentID = next.CompactionID
			plan.coveredFrom = next.FirstKeptMessageID
			plan.parentAudit = next.SourceRefs
			plan.parentIncluded = next.IncludedRefs
			plan.previous = next.Summary
			continue

		case AppendRebuildRoot:
			// 无有效头：以 `chain_epoch+1`、`parent` 为空、覆盖范围起点重建新根，**每 Run 至多一次**。
			if rebuildLeft == 0 || c.rebuildUsed(req.RunID) {
				c.markExhausted(req.RunID, ReasonNoInjectableChain)
				return degraded(ReasonNoInjectableChain), nil
			}
			rebuildLeft--
			c.markRebuilt(req.RunID)
			plan.epoch = maxEpoch(chain) + 1
			plan.parentID = ""
			plan.coveredFrom = messages[0].MessageID
			plan.parentAudit = chainAudit(chain)
			plan.parentIncluded = nil
			plan.previous = ""
			continue

		case AppendPending, AppendInconsistent:
			// 结局读不到 / 与业务行矛盾：冻结前滚、告警，**不重试**（也不降级成"再压一次"）。
			return core.CompactResult{}, ErrCompactionPending

		default:
			return core.CompactResult{}, fmt.Errorf("%w: 追加结局 %q 不在六值之内", ErrScopeInvalid, res.Outcome)
		}
	}
}

// validate 校验请求与策略接线：任一缺失即拒绝（**无默认值**）。
// validate refuses an unwired request or policy; there are no defaults to fall back on.
func (c *Compactor) validate(req core.CompactRequest) error {
	if !c.deps.Policy.valid() {
		return fmt.Errorf("%w: compaction.* 策略数值未接线", ErrScopeInvalid)
	}
	if c.deps.Store == nil || c.deps.Summarizer == nil || c.deps.Validator == nil {
		return fmt.Errorf("%w: 压缩引擎依赖未装配", ErrScopeInvalid)
	}
	if req.RunID == "" || req.AttemptID == "" {
		return fmt.Errorf("%w: run_id / attempt_id 为空", ErrScopeInvalid)
	}
	if req.History == nil {
		return fmt.Errorf("%w: req.History 缺失（无状态范围的唯一消息来源）", ErrScopeInvalid)
	}
	if req.SessionID != "" && c.deps.Messages == nil {
		// 会话范围必须能读库；无状态范围不读库，缺 reader 不算错。
		return fmt.Errorf("%w: 会话范围缺少 MessageReader", ErrScopeInvalid)
	}
	if req.Budget.ContextTokens <= 0 || req.Budget.OutputReserve <= 0 {
		return fmt.Errorf("%w: budget.context_tokens / compaction.output_reserve_tokens 未接线",
			ErrScopeInvalid)
	}
	if req.Fixed.Total <= 0 {
		return fmt.Errorf("%w: Fixed.Total 为零（不可裁段落未接线，压缩不得猜固定开销）", ErrScopeInvalid)
	}
	if req.EstimatorVersion != "" && req.Budget.EstimatorVersion != "" &&
		req.EstimatorVersion != req.Budget.EstimatorVersion {
		return fmt.Errorf("%w: 估算器版本不一致（%q / %q）",
			ErrScopeInvalid, req.EstimatorVersion, req.Budget.EstimatorVersion)
	}
	return nil
}

// loadMessages 按范围取消息：会话走 `MessageReader`（持久化行，跨此前 Run），无状态走
// `req.History`（内存，唯一来源）。两条路径**不得**互相回落——按 Run 查库只能得到空集。
// loadMessages reads a session's persisted rows or a stateless run's in-memory history;
// the two paths never fall back to each other.
func (c *Compactor) loadMessages(ctx context.Context, req core.CompactRequest) ([]StoredMessage, error) {
	if req.SessionID == "" {
		msgs := req.History.Messages()
		out := make([]StoredMessage, 0, len(msgs))
		for i, m := range msgs {
			out = append(out, StoredMessage{
				MessageID:        StatelessMessageID(req.RunID, i+1),
				Seq:              int64(i + 1),
				RunID:            req.RunID,
				Role:             m.Role,
				Content:          m.Content,
				ToolCallID:       m.ToolCallID,
				ToolCalls:        m.ToolCalls,
				ReasoningContent: m.ReasoningContent,
				SourceRefs:       m.EvidenceRefs,
			})
		}
		return out, nil
	}
	var out []StoredMessage
	after := int64(0)
	for page := 0; page < sessionPageLimit; page++ {
		batch, err := c.deps.Messages.ListBySession(ctx, req.SessionID, after, sessionPageSize)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < sessionPageSize {
			return out, nil
		}
		after = batch[len(batch)-1].Seq
	}
	return nil, fmt.Errorf("%w: 会话 %s 超过 %d 条消息，单次压缩不读取更长历史",
		ErrScopeInvalid, req.SessionID, sessionPageSize*sessionPageLimit)
}

// qualifyChain 逐条校验链资格与注入资格，返回**最后一条可注入条目**（无则 nil）。
//
// 失效从**最早**不满足资格的那一条起标：`MarkStale` 由 store 在同一次事务里级联后代，
// 标完仍要问"上游还有没有合格祖先"——有就回退（不重建、零摘要调用），没有才重建。
// **不得**从叶开始标，也**不得**跳过坏祖先取后代摘要。
// qualifyChain validates the chain link by link and returns the last injectable entry,
// marking the earliest failing entry stale (the store cascades to its descendants).
func (c *Compactor) qualifyChain(ctx context.Context, p core.Principal, chain []CompactionEntry) (*CompactionEntry, error) {
	if len(chain) == 0 {
		return nil, nil
	}
	if err := ValidateChain(chain); err != nil {
		// 断链 / 分叉 / 跨 epoch：不选头。下一次 Append 会返回 AppendRebuildRoot。
		return nil, nil
	}
	for i := range chain {
		ok, reason, err := c.refsInjectable(ctx, p, chain[i].IncludedRefs)
		if err != nil {
			return nil, err
		}
		if ok {
			continue
		}
		if err := c.deps.Store.MarkStale(ctx, chain[i].CompactionID, reason); err != nil {
			return nil, err
		}
		if i == 0 {
			return nil, nil
		}
		ancestor := chain[i-1]
		return &ancestor, nil
	}
	head := chain[len(chain)-1]
	return &head, nil
}

// projected 估算本轮 prompt 的输入量：不可裁段落 + 滚动摘要段 + 保留段消息。
// 估算器与装配记录同源（C-31），**不得**另用一套口径。
// projected estimates this round's input: fixed parts, the rolling summary and the kept tail.
func (c *Compactor) projected(req core.CompactRequest, kept []StoredMessage, head *CompactionEntry) int {
	total := req.Fixed.Total + c.deps.Estimator.Estimate(kept)
	if head != nil {
		total += estimateTextTokens(head.Summary)
	}
	return total
}

// triggered 判定是否压缩：比例线只让压缩**更早**触发，强制线决定"压不下去即拒绝"。
// triggered applies the ratio line and the hard line from the single budget chain.
func (c *Compactor) triggered(projected, usable int) bool {
	if projected > usable {
		return true
	}
	return float64(projected) > c.deps.Policy.TriggerRatio*float64(usable)
}

// estimateTextTokens 估算一段纯文本的 token 数（用与消息同一个估算器，含固定包裹开销）。
// estimateTextTokens estimates a plain text segment with the very same estimator.
func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	return core.EstimateMessage(core.Message{Role: roleSummarySystem, Content: text})
}

// plannedAttempt 是一次追加尝试的**定位信息**：它决定条目的 epoch / 父节点 / 覆盖起点与
// 父链的两个引用集合。重提时只换 `attempt`，定位不变。
// plannedAttempt pins an append attempt's epoch, parent and coverage start.
type plannedAttempt struct {
	scope          Scope
	epoch          int
	parentID       string
	coveredFrom    string
	parentAudit    []core.EvidenceRef
	parentIncluded []core.EvidenceRef
	previous       string
	// runID 是触发本次压缩的 Run（进 `compaction_entries.trigger_run_id`）。
	runID string
	// baseAttempt 是调用方给的 `attempt_id`：首次追加用它，重提时加后缀区分。
	baseAttempt string
	attempt     int
}

// preparedSummary 是一条**已生成、尚未确认落库**的摘要：它的正文与两个引用集合。
// 同一覆盖范围重提时直接复用（不再发摘要调用、不再预留），落库确认后即丢弃。
// preparedSummary is a generated-but-unconfirmed summary, reused verbatim on re-submit.
type preparedSummary struct {
	text         string
	model        string
	usage        core.Usage
	audit        []core.EvidenceRef
	included     []core.EvidenceRef
	cutID        string
	firstKept    string
	tokensBefore int
	tokensAfter  int
}

// buildEntry 把准备好的摘要与定位信息组装成一条条目（标识与时间由本函数生成：
// 重提同一条摘要时**换标识与 `attempt_id`**，内容与覆盖范围不变）。
// buildEntry assembles a prepared summary into the store-shaped entry; a re-submit keeps
// the coverage and the text but always gets a fresh id and attempt id.
func (c *Compactor) buildEntry(p *preparedSummary, plan plannedAttempt) CompactionEntry {
	return CompactionEntry{
		CompactionID:         newCompactionID(),
		Scope:                plan.scope,
		ChainEpoch:           clampEpoch(plan.epoch),
		ParentCompactionID:   plan.parentID,
		TriggerRunID:         plan.runID,
		AttemptID:            attemptID(plan),
		CoveredFromMessageID: plan.coveredFrom,
		CutMessageID:         p.cutID,
		FirstKeptMessageID:   p.firstKept,
		StrategyVersion:      c.deps.Policy.StrategyVersion,
		PromptVersion:        c.deps.Policy.PromptVersion,
		Model:                p.model,
		Usage:                p.usage,
		TokensBefore:         p.tokensBefore,
		TokensAfter:          p.tokensAfter,
		Status:               StatusCommitted,
		SourceRefs:           p.audit,
		IncludedRefs:         p.included,
		Summary:              p.text,
		CreatedAt:            c.deps.Clock(),
	}
}

// prepare 过滤覆盖段、算引用集合并调用摘要器；同一覆盖范围的未确认摘要在内存里复用。
// prepare filters the covered segment, computes both reference sets and calls the summarizer,
// reusing an unconfirmed summary for the very same coverage range.
func (c *Compactor) prepare(ctx context.Context, req core.CompactRequest, plan plannedAttempt,
	covered, keptTail []StoredMessage, cutID, firstKept string, tokensBefore int,
) (*preparedSummary, error) {
	kept := make([]bool, len(covered))
	inputs := make([]SummaryMessage, len(covered))
	for i, m := range covered {
		ok, _, err := c.refsInjectable(ctx, req.Principal, m.SourceRefs)
		if err != nil {
			return nil, err
		}
		kept[i] = ok
		inputs[i] = SummaryMessage{
			MessageID:  m.MessageID,
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			ToolNames:  toolNamesOf(m),
			Truncated:  m.Truncated,
			Redacted:   !ok,
		}
	}
	audit, included := SourceRefsOf(covered, plan.parentAudit, plan.parentIncluded, kept)

	// 同一覆盖范围已经生成过摘要（重提 / 换代重建）：只复用**模型正文**，两个引用集合
	// 与过滤结论按当前父链重算——重建根的父链变了，照搬旧集合会把已剔除的来源带进来。
	key := pendingKey(plan.scope, plan.coveredFrom, cutID)
	var body SummaryOutput
	if cached, ok := c.pendingSummary(key); ok {
		body = SummaryOutput{Text: cached.text, Usage: cached.usage, Model: cached.model}
	} else {
		in := SummaryInput{
			Scope:           plan.scope,
			RunID:           req.RunID,
			SessionID:       req.SessionID,
			AttemptID:       req.AttemptID,
			Model:           c.deps.Policy.SummaryModel,
			PromptVersion:   c.deps.Policy.PromptVersion,
			StrategyVersion: c.deps.Policy.StrategyVersion,
			Previous:        plan.previous,
			Covered:         inputs,
			MaxOutputTokens: c.deps.Policy.SummaryMaxTokens,
			MaxInputBytes:   c.deps.Policy.SummaryInputBytes,
		}
		out, err := c.summarizeWithin(ctx, in)
		if err != nil {
			return nil, err
		}
		if err := ValidateSummaryText(out.Text); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSummaryFailed, err)
		}
		body = out
		c.rememberPending(key, &preparedSummary{text: out.Text, usage: out.Usage, model: out.Model})
	}

	text := body.Text
	if carried := CarryCodeBlocks(plan.previous); carried != "" {
		// 代码保真段由代码追加，模型无权删除；两节**不**参与上限裁剪（它们不许被裁掉）。
		text = text + "\n\n" + carried
	}
	tokensAfter := estimateTextTokens(text) + c.deps.Estimator.Estimate(keptTail)

	return &preparedSummary{
		text:         text,
		model:        body.Model,
		usage:        body.Usage,
		audit:        audit,
		included:     included,
		cutID:        cutID,
		firstKept:    firstKept,
		tokensBefore: tokensBefore,
		tokensAfter:  tokensAfter,
	}, nil
}

// summarizeWithin 在策略超时内调用摘要器，并对**输出超限**做一次重新生成：
// 超限**不得**静默截断（截断会先丢"硬约束 / 未决项"两节），两次仍超限即失败。
// summarizeWithin applies the policy timeout and one regeneration on an over-limit output;
// the text is never silently truncated.
func (c *Compactor) summarizeWithin(ctx context.Context, in SummaryInput) (SummaryOutput, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.deps.Policy.SummaryTimeout)
	defer cancel()
	var out SummaryOutput
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		out, err = c.deps.Summarizer.Summarize(callCtx, in)
		if err != nil {
			return SummaryOutput{}, err
		}
		if estimateTextTokens(out.Text) <= c.deps.Policy.SummaryMaxTokens {
			return out, nil
		}
		// 超限：按模板缩小叙事段后重新生成（第二次仍是同一条输入，覆盖段不变）。
	}
	return SummaryOutput{}, fmt.Errorf("%w: 两次生平均超过 %d token", ErrSummaryOverLimit, c.deps.Policy.SummaryMaxTokens)
}

// degradeOrReturn 把分类错误映射成降级结果；取消与结局不可判定**不**降级，按错误上抛。
// degradeOrReturn maps classified failures to a degradation; cancellation and undecidable
// outcomes are returned as errors instead.
func (c *Compactor) degradeOrReturn(ctx context.Context, err error) (core.CompactResult, error) {
	switch {
	case ctx.Err() != nil:
		// 取消传播：历史保持原样，审计记取消终态，**不得**降级成"压缩失败"继续跑。
		return core.CompactResult{}, ctx.Err()
	case errors.Is(err, ErrSummaryTimeout):
		return degraded(ReasonCompactionTimeout), nil
	case errors.Is(err, ErrSummaryOverLimit):
		return degraded(ReasonCompactionOverLimit), nil
	case errors.Is(err, ErrNoValidCut):
		return degraded(ReasonCompactionFailed), nil
	case errors.Is(err, ErrCompactionPending):
		return core.CompactResult{}, ErrCompactionPending
	default:
		return degraded(ReasonCompactionFailed), nil
	}
}

// —— 引擎内的小工具 ——

// summaryOf 把存储条目投影成 core 侧的摘要视图。
// summaryOf projects a stored entry into the core-side summary view.
func summaryOf(e CompactionEntry, v core.SummaryValidity) *core.CompactionSummary {
	return &core.CompactionSummary{
		CompactionID:         e.CompactionID,
		ChainEpoch:           e.ChainEpoch,
		TriggerRunID:         e.TriggerRunID,
		CoveredFromMessageID: e.CoveredFromMessageID,
		CutMessageID:         e.CutMessageID,
		FirstKeptMessageID:   e.FirstKeptMessageID,
		Text:                 e.Summary,
		SourceRefs:           e.SourceRefs,
		IncludedRefs:         e.IncludedRefs,
		Validity:             v,
		StrategyVersion:      e.StrategyVersion,
		PromptVersion:        e.PromptVersion,
	}
}

// summaryEntry 把"上一份摘要正文"包成条目形状，仅用于估算其 token 占用。
// summaryEntry wraps the previous summary text so the estimator can size it.
func summaryEntry(text string) *CompactionEntry {
	if text == "" {
		return nil
	}
	return &CompactionEntry{Summary: text}
}

// toolNamesOf 取一条 assistant(tool_calls) 消息要了哪些工具（只给摘要器读，不进估算）。
// toolNamesOf lists the tool names requested by an assistant tool-call message.
func toolNamesOf(m StoredMessage) []string {
	if len(m.ToolCalls) == 0 {
		return nil
	}
	names := make([]string, 0, len(m.ToolCalls))
	for _, tc := range m.ToolCalls {
		names = append(names, tc.Name)
	}
	return names
}

// indexOfMessage 返回消息 ID 在有序消息里的下标；找不到返回 -1（**不**猜位置）。
// indexOfMessage returns the index of a message id, or -1.
func indexOfMessage(msgs []StoredMessage, id string) int {
	for i, m := range msgs {
		if m.MessageID == id {
			return i
		}
	}
	return -1
}

// chainAudit 求整条链的审计集并集：换代重建时旧链只作审计，其来源仍须可追溯。
// 当前依赖集**不**继承——重建根不得把失效链的依赖带进正文。
// chainAudit unions the audit sets of the old chain for a rebuild root; the dependency set
// is never inherited.
func chainAudit(chain []CompactionEntry) []core.EvidenceRef {
	sets := make([][]core.EvidenceRef, 0, len(chain))
	for _, e := range chain {
		if len(e.SourceRefs) > 0 {
			sets = append(sets, e.SourceRefs)
		}
	}
	return mergeRefs(sets...)
}

// maxEpoch 返回链上最大的 `chain_epoch`（空链为 0，重建根因此从 1 起）。
// maxEpoch returns the largest epoch in the chain, zero for an empty chain.
func maxEpoch(chain []CompactionEntry) int {
	best := 0
	for _, e := range chain {
		if e.ChainEpoch > best {
			best = e.ChainEpoch
		}
	}
	return best
}

// clampEpoch 把 epoch 归到「从 1 起」：无头时用 1，有头时沿用头的代次。
// clampEpoch normalises an epoch to the 1-based scale.
func clampEpoch(epoch int) int {
	if epoch < 1 {
		return 1
	}
	return epoch
}

// attemptID 生成本次追加的幂等身份：首次用调用方给的 `attempt_id`，重提追加后缀。
// attemptID renders the idempotency identity of one append attempt.
func attemptID(plan plannedAttempt) string {
	base := plan.baseAttempt
	if base == "" {
		base = plan.scope.Canonical()
	}
	if plan.attempt == 0 {
		return base
	}
	return fmt.Sprintf("%s-re%d", base, plan.attempt)
}

// newCompactionID 生成条目标识（随机 16 字节十六进制）。标识**不**参与幂等——幂等靠
// 覆盖范围唯一键与 `attempt_id`，随机 ID 只避免并发下撞主键。
// newCompactionID mints an entry id; idempotency never depends on it.
func newCompactionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 随机源失效时退化为时间戳：标识只是主键，不能因为熵源故障阻断压缩。
		return fmt.Sprintf("cmp-%d", time.Now().UnixNano())
	}
	return "cmp-" + hex.EncodeToString(buf[:])
}

// degraded 构造降级结果（`ReasonCode` 必非空）。
// degraded builds a degradation result.
func degraded(reason string) core.CompactResult {
	return core.CompactResult{Degraded: true, ReasonCode: reason}
}

// —— 本 Run 的粘性状态 ——

// exhaustedReason 读本 Run 的 no_injectable_chain 结论。
// exhaustedReason reports the sticky no_injectable_chain verdict for this run.
func (c *Compactor) exhaustedReason(runID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reason, ok := c.exhausted[runID]
	return reason, ok
}

// markExhausted 记录本 Run 不再生成摘要。
// markExhausted stops further regeneration for the rest of the run.
func (c *Compactor) markExhausted(runID, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exhausted[runID] = reason
}

// clearExhausted 在成功提交后清掉标记（下一轮是新的事实）。
// clearExhausted drops the sticky verdict after a successful commit.
func (c *Compactor) clearExhausted(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.exhausted, runID)
}

// rebuildUsed 报告本 Run 是否已尝试过换代重建（每 Run 至多一次）。
// rebuildUsed reports whether this run already attempted a root rebuild.
func (c *Compactor) rebuildUsed(runID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rebuilt[runID]
}

// markRebuilt 记下本 Run 已用掉重建机会。
// markRebuilt records that this run spent its single rebuild.
func (c *Compactor) markRebuilt(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rebuilt[runID] = true
}

// pendingKey 是未确认摘要的键：同一 scope 的同一覆盖范围才算同一条摘要。
// pendingKey identifies an unconfirmed summary by scope and coverage.
func pendingKey(scope Scope, coveredFrom, cutID string) string {
	return scope.Canonical() + "|" + coveredFrom + "|" + cutID
}

// pendingSummary 读未确认摘要。
// pendingSummary returns the cached unconfirmed summary.
func (c *Compactor) pendingSummary(key string) (*preparedSummary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[key]
	return p, ok
}

// rememberPending 记住刚生成的摘要（结局未定前不丢弃）。
// rememberPending caches a freshly generated summary until its append is confirmed.
func (c *Compactor) rememberPending(key string, p *preparedSummary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[key] = p
}

// forgetPending 在落库确认（或发现已有合格条目）后丢弃缓存。
// forgetPending drops the cache once the append is confirmed.
func (c *Compactor) forgetPending(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, key)
}

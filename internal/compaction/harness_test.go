package compaction_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件是压缩引擎测试的**唯一夹具**（设计卡 §4.4 R19～R22 冻结的 `newCompactionHarness`）：
// 内存消息表 + 可编程存储 / 摘要器 / 来源校验器 / 消息读取器。全部依赖用替身，不连 PG、
// 不发真实模型调用——本文件里没有一次外部 I/O。
// The single fixture for the engine tests: in-memory message table plus programmable
// store, summarizer, validity checker and message reader. No external I/O anywhere here.

// group 是一条**完整轮次**的三条消息：用户提问、assistant 发起一次工具调用、工具结果。
// 三条消息预置同一个 `ToolGroupID`（= 分组名）：这样"按完整工具组切分"的保留组数按
// **轮次**计，而不是把 user 单独算一组——否则 keepRecent=1 只留 assistant+结果，
// 冻结断言「最近一组必须保留」（`groupOfMessage(FirstKeptMessageID) == "g3"`）不成立。
// 分组名同时编码进工具调用 ID（`<name>-call`），`withSourceRefs` 包一层也不丢名字。
// group is one complete turn; all three messages carry the same pre-assigned tool group
// id so the retained-group count is per turn, not per message.
func group(name, prompt, result string) []compaction.StoredMessage {
	callID := name + "-call"
	return []compaction.StoredMessage{
		{Role: core.RoleUser, Content: prompt, ToolGroupID: name},
		{Role: core.RoleAssistant, ToolGroupID: name, ToolCalls: []core.ToolCall{{ID: callID, Name: "source_view", Arguments: `{"query":"` + prompt + `"}`}}},
		{Role: core.RoleTool, ToolCallID: callID, Content: result, ToolGroupID: name},
	}
}

// u 是一条用户消息，a 是一条 assistant 文本消息。
// u and a build a plain user / assistant message.
func u(content string) compaction.StoredMessage {
	return compaction.StoredMessage{Role: core.RoleUser, Content: content}
}

func a(content string) compaction.StoredMessage {
	return compaction.StoredMessage{Role: core.RoleAssistant, Content: content}
}

// asstCall 是一条只发起工具调用的 assistant 消息（content 可为空——契约事实）。
// asstCall is an assistant message that only requests a tool; its content may be empty.
func asstCall(callID, name string) compaction.StoredMessage {
	return compaction.StoredMessage{
		Role:      core.RoleAssistant,
		ToolCalls: []core.ToolCall{{ID: callID, Name: name, Arguments: "{}"}},
	}
}

// toolResult 是一条工具结果消息，与被回填的调用一一对应。
// toolResult answers the named tool call.
func toolResult(callID, content string) compaction.StoredMessage {
	return compaction.StoredMessage{Role: core.RoleTool, ToolCallID: callID, Content: content}
}

// ref 构造一条来源引用（kind 固定 knowledge——测试不关心种类）。
// ref builds an evidence reference; the tests never care about the kind.
func ref(id, version string) core.EvidenceRef {
	return core.EvidenceRef{Kind: "knowledge", ID: id, VersionID: version}
}

// withSourceRefs 给一组消息挂上同一批来源引用（一轮的来源是整轮共享的）。
// withSourceRefs attaches the same references to every message of a group.
func withSourceRefs(msgs []compaction.StoredMessage, refs ...core.EvidenceRef) []compaction.StoredMessage {
	out := make([]compaction.StoredMessage, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].SourceRefs = append(append([]core.EvidenceRef{}, out[i].SourceRefs...), refs...)
	}
	return out
}

// compactionHarness 是一份完整的压缩测试环境。
// compactionHarness is one complete compaction test environment.
type compactionHarness struct {
	t   *testing.T
	ctx context.Context

	compactor *compaction.Compactor
	provider  *fakeProvider
	store     *fakeStore
	validity  *fakeValidity
	reader    *fakeReader
	hist      *fakeHistory

	principal  core.Principal
	budgetCfg  core.ContextBudget
	fixedTotal int
	policy     compaction.Policy

	msgs     []compaction.StoredMessage
	runs     map[string]string // messageID → runID
	sessions map[string]string // messageID → sessionID
	seq      int64
}

// newCompactionHarness 构造夹具；阈值全部由用例随后用 `setLimits` 显式给定。
// newCompactionHarness wires the fixture; limits are always set explicitly afterwards.
func newCompactionHarness(t *testing.T) *compactionHarness {
	t.Helper()
	h := &compactionHarness{
		t:   t,
		ctx: context.Background(),
		principal: core.Principal{
			Kind:     core.PrincipalCaller,
			CallerID: "caller-1",
			KeyID:    "key-1",
			Scopes:   []string{core.ScopeSessionsRead, core.ScopeSessionsWrite},
			Actor:    core.ActorCaller,
		},
		budgetCfg:  core.ContextBudget{ContextTokens: 300, OutputReserve: 50, EstimatorVersion: core.EstimateVersion},
		fixedTotal: 30,
		policy: compaction.Policy{
			TriggerRatio:      0.2,
			SafetyMarginRatio: 0.1,
			KeepRecentGroups:  1,
			SummaryMaxTokens:  100,
			SummaryTimeout:    5 * time.Second,
			StrategyVersion:   "cut-v1",
			PromptVersion:     "summary-v1",
			SummaryModel:      "summary-model",
		},
		runs:     map[string]string{},
		sessions: map[string]string{},
	}
	h.provider = &fakeProvider{}
	h.validity = &fakeValidity{revoked: map[core.EvidenceRef]string{}}
	h.store = &fakeStore{h: h, stale: map[string]string{}}
	h.reader = &fakeReader{h: h}
	h.hist = &fakeHistory{}
	h.compactor = h.newCompactor()
	return h
}

// newCompactor 按当前策略接线一台引擎（`setLimits` 改了数值后必须重建，策略是构造时冻结的）。
// newCompactor wires an engine with the current policy, which is frozen at construction.
func (h *compactionHarness) newCompactor() *compaction.Compactor {
	return compaction.NewCompactor(compaction.CompactorDeps{
		Messages:   h.reader,
		Store:      h.store,
		Summarizer: h.provider,
		Validator:  h.validity,
		Estimator:  compaction.MessageEstimator{},
		Clock:      func() time.Time { return time.Unix(1760000000, 0).UTC() },
		Policy:     h.policy,
	})
}

// restart 模拟进程重启：内存里的未确认摘要与 `no_injectable_chain` 结论都消失，
// 只剩存储里已提交的条目。
// restart drops every piece of in-memory state, as a process restart would.
func (h *compactionHarness) restart() *compaction.Compactor {
	h.compactor = h.newCompactor()
	return h.compactor
}

// setLimits 显式给定预算 / 保留组数 / 摘要上限（不读配置缺省）。
// setLimits pins the three numbers the tests depend on.
func (h *compactionHarness) setLimits(budget, keepRecentGroups, summaryMax int) {
	h.budgetCfg.ContextTokens = budget
	h.policy.KeepRecentGroups = keepRecentGroups
	h.policy.SummaryMaxTokens = summaryMax
	h.compactor = h.newCompactor()
}

// budget 覆盖 `context_tokens` 与 `compaction.output_reserve_tokens`。
// budget overrides the two budget numbers.
func (h *compactionHarness) budget(contextTokens, outputReserve int) {
	h.budgetCfg.ContextTokens = contextTokens
	h.budgetCfg.OutputReserve = outputReserve
}

// fixed 覆盖不可裁段落的 token 估算。
// fixed overrides the untrimmable-segment estimate.
func (h *compactionHarness) fixed(total int) { h.fixedTotal = total }

// seedRun 追加一个 Run 的消息：ID 按会话内顺序分配（m1、m2……），seq 单调递增。
// seedRun appends one run's messages, assigning m1..mN ids in session order.
func (h *compactionHarness) seedRun(t *testing.T, sessionID, runID string, msgs ...compaction.StoredMessage) {
	t.Helper()
	h.seed(sessionID, runID, msgs)
}

// seedGroups 追加若干**测试分组**（每组三条消息，见 `group`）。
// seedGroups appends whole groups; each group is three messages.
func (h *compactionHarness) seedGroups(t *testing.T, sessionID, runID string, groups ...[]compaction.StoredMessage) {
	t.Helper()
	for _, g := range groups {
		h.seed(sessionID, runID, g)
	}
}

func (h *compactionHarness) seed(sessionID, runID string, msgs []compaction.StoredMessage) {
	for _, m := range msgs {
		h.seq++
		m.MessageID = fmt.Sprintf("m%d", h.seq)
		m.Seq = h.seq
		m.RunID = runID
		h.msgs = append(h.msgs, m)
		h.runs[m.MessageID] = runID
		h.sessions[m.MessageID] = sessionID
	}
}

// history 设置无状态 Run 的内存历史（`CompactRequest.History` 的唯一来源）。
// history seeds the in-memory history of a stateless run.
func (h *compactionHarness) history(msgs ...compaction.StoredMessage) {
	out := make([]core.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.AsMessage())
	}
	h.hist.msgs = out
}

// request 构造一次压缩请求。
// request builds one compaction request.
func (h *compactionHarness) request(sessionID, runID string) core.CompactRequest {
	return core.CompactRequest{
		Principal:        h.principal,
		RunID:            runID,
		SessionID:        sessionID,
		AttemptID:        "a1",
		Budget:           h.budgetCfg,
		Fixed:            core.FixedParts{Total: h.fixedTotal},
		History:          h.hist,
		EstimatorVersion: core.EstimateVersion,
	}
}

// renameMessage 给一条消息换标识（用于构造"标识顺序与 seq 顺序相反"的夹具：
// 引擎**不得**按标识或创建时间排序）。
// renameMessage relabels a message so ids can deliberately disagree with seq order.
func (h *compactionHarness) renameMessage(t *testing.T, oldID, newID string) {
	t.Helper()
	for i := range h.msgs {
		if h.msgs[i].MessageID != oldID {
			continue
		}
		h.msgs[i].MessageID = newID
		h.runs[newID] = h.runs[oldID]
		delete(h.runs, oldID)
		h.sessions[newID] = h.sessions[oldID]
		delete(h.sessions, oldID)
		return
	}
	t.Fatalf("未知消息 %s", oldID)
}

// indexOf 返回某条消息在会话顺序里的下标（覆盖起点是否单调前移的判据）。
// indexOf returns the session-order position of a message.
func (h *compactionHarness) indexOf(t *testing.T, messageID string) int {
	t.Helper()
	for i, m := range h.msgs {
		if m.MessageID == messageID {
			return i
		}
	}
	t.Fatalf("未知消息 %s", messageID)
	return -1
}

// messageCount 返回会话消息表的总条数（"摘要不得写回消息表"的判据）。
// messageCount is the size of the message table.
func (h *compactionHarness) messageCount(t *testing.T) int {
	t.Helper()
	return len(h.msgs)
}

// runOfMessage 返回某条消息所属的 Run。
// runOfMessage reports which run a message belongs to.
func (h *compactionHarness) runOfMessage(t *testing.T, messageID string) string {
	t.Helper()
	run, ok := h.runs[messageID]
	require.True(t, ok, "未知消息 %s", messageID)
	return run
}

// groupOfMessage 返回某条消息所属的测试分组名：预置 `ToolGroupID` 优先（`group` 建的
// 整轮三件套），否则工具调用消息与它的结果同组，其余消息自成一组——与 `AssignToolGroups`
// 同一口径。
// groupOfMessage resolves the test group name of a message, honouring a pre-assigned group id.
func (h *compactionHarness) groupOfMessage(t *testing.T, messageID string) string {
	t.Helper()
	for _, m := range h.msgs {
		if m.MessageID != messageID {
			continue
		}
		if m.ToolGroupID != "" {
			return m.ToolGroupID
		}
		if m.Role == core.RoleAssistant && len(m.ToolCalls) > 0 {
			return strings.TrimSuffix(m.ToolCalls[0].ID, "-call")
		}
		if m.ToolCallID != "" {
			return strings.TrimSuffix(m.ToolCallID, "-call")
		}
		return m.MessageID
	}
	t.Fatalf("未知消息 %s", messageID)
	return ""
}

// messagesBetween 取 `[from, to]` 闭区间的消息（按会话顺序）。
// messagesBetween returns the inclusive message range.
func (h *compactionHarness) messagesBetween(t *testing.T, from, to string) []compaction.StoredMessage {
	t.Helper()
	start, end := -1, -1
	for i, m := range h.msgs {
		if m.MessageID == from {
			start = i
		}
		if m.MessageID == to {
			end = i
		}
	}
	require.True(t, start >= 0 && end >= start, "范围非法：%s → %s", from, to)
	return append([]compaction.StoredMessage{}, h.msgs[start:end+1]...)
}

// summaryInputContains 断言某段原文出现在摘要输入里（按真实渲染口径，含占位符替换）。
// summaryInputContains checks the rendered summary input for a substring.
func (h *compactionHarness) summaryInputContains(t *testing.T, want string) bool {
	t.Helper()
	for _, in := range h.provider.recorded() {
		if strings.Contains(renderSummaryInput(in), want) {
			return true
		}
	}
	return false
}

// summaryInputExcludes 断言某段原文**没有**出现在任何一次摘要输入里。
// summaryInputExcludes checks that a substring never reached the summarizer.
func (h *compactionHarness) summaryInputExcludes(t *testing.T, unwanted string) bool {
	t.Helper()
	for _, in := range h.provider.recorded() {
		if strings.Contains(renderSummaryInput(in), unwanted) {
			return false
		}
	}
	return true
}

// renderSummaryInput 复刻 `ModelSummarizer` 的渲染口径：上一份摘要 + 结构化状态 + 覆盖段。
// renderSummaryInput mirrors how the real summarizer renders its user message.
func renderSummaryInput(in compaction.SummaryInput) string {
	var b strings.Builder
	b.WriteString(in.Previous)
	b.WriteString(in.StateBlock)
	b.WriteString(compaction.RenderCovered(in.Covered, in.MaxInputBytes))
	return b.String()
}

// assertWholeGroups 校验一段连续消息没有把工具组拆开：每个工具调用都在段内找到结果，
// 每个结果都能在段内找到调用方。
// assertWholeGroups rejects a slice that splits an assistant tool call from its results.
func assertWholeGroups(msgs []compaction.StoredMessage) error {
	for _, m := range msgs {
		if m.Role == core.RoleAssistant {
			for _, tc := range m.ToolCalls {
				if !hasToolResult(msgs, tc.ID) {
					return fmt.Errorf("切点拆散了工具组：%s 的调用 %s 没有结果", m.MessageID, tc.ID)
				}
			}
		}
		if m.Role == core.RoleTool && m.ToolCallID != "" && !hasToolCall(msgs, m.ToolCallID) {
			return fmt.Errorf("切点拆散了工具组：%s 的结果没有对应的调用", m.MessageID)
		}
	}
	return nil
}

func hasToolResult(msgs []compaction.StoredMessage, callID string) bool {
	for _, m := range msgs {
		if m.Role == core.RoleTool && m.ToolCallID == callID {
			return true
		}
	}
	return false
}

func hasToolCall(msgs []compaction.StoredMessage, callID string) bool {
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.ID == callID {
				return true
			}
		}
	}
	return false
}

// —— 摘要器替身 ——

type fakeProvider struct {
	mu     sync.Mutex
	callsN int
	texts  []string
	inputs []compaction.SummaryInput
	fail   error
	onCall func()
}

func (p *fakeProvider) Summarize(ctx context.Context, in compaction.SummaryInput) (compaction.SummaryOutput, error) {
	p.mu.Lock()
	p.callsN++
	idx := p.callsN - 1
	p.inputs = append(p.inputs, in)
	fail, hook, texts := p.fail, p.onCall, p.texts
	p.mu.Unlock()

	if hook != nil {
		hook()
	}
	if fail != nil {
		return compaction.SummaryOutput{}, fail
	}
	text := "摘要正文"
	if len(texts) > 0 {
		if idx >= len(texts) {
			idx = len(texts) - 1
		}
		text = texts[idx]
	}
	return compaction.SummaryOutput{
		Text:  text,
		Usage: core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		Model: in.Model,
	}, nil
}

// respond 脚本化按调用次序返回的摘要正文（用尽后重复最后一条）。
// respond scripts the summary bodies by call order, repeating the last one.
func (p *fakeProvider) respond(texts ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.texts = append([]string{}, texts...)
}

// failWith 让每次摘要调用都失败。
// failWith makes every summary call fail.
func (p *fakeProvider) failWith(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = err
}

// onEachCall 注册每次调用时执行的回调（用于取消传播的场景）。
// onEachCall runs a hook on every call, used for cancellation.
func (p *fakeProvider) onEachCall(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onCall = fn
}

// calls 返回摘要调用次数。
// calls is the number of summary calls.
func (p *fakeProvider) calls(t *testing.T) int {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.callsN
}

// recorded 返回每次调用的完整输入。
// recorded returns every call's input.
func (p *fakeProvider) recorded() []compaction.SummaryInput {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]compaction.SummaryInput{}, p.inputs...)
}

// —— 存储替身 ——

type fakeStore struct {
	h *compactionHarness

	seeds    []compaction.CompactionEntry
	appended []compaction.CompactionEntry
	stale    map[string]string

	script        []compaction.AppendOutcome
	supersededTo  *compaction.CompactionEntry
	supersededOK  bool
	rebuildReject bool

	appendCalls int
	staleCallsV int
}

// Append 实现 `CompactionStore.Append`：先消费脚本化结局，再按"锁内校验有效头"的
// 真实语义分流——头存在时 epoch / 父节点 / 覆盖起点必须与头对齐，否则被超越；
// 无有效头时必须是 `chain_epoch+1` 且无父节点，否则返回重建根。
// `AppendSuperseded` 的脚本会在**这一刻**把"并发追加"的新头补进已提交链：头必须在
// 读链之后、追加之前才落，否则引擎读链时就已看见它，测不出"按新头重算"。
// Append mirrors the real in-lock head check. The scripted superseded outcome promotes the
// concurrent head only at append time, because a head that already existed when the chain
// was read proves nothing about rebasing.
func (s *fakeStore) Append(_ context.Context, entry compaction.CompactionEntry) (compaction.AppendResult, error) {
	s.appendCalls++
	if len(s.script) > 0 {
		outcome := s.script[0]
		s.script = s.script[1:]
		switch outcome {
		case compaction.AppendCommitted:
			// 脚本只说"这次一定成功"：仍要真的落库，否则"复用已提交条目"无从验证。
			return s.commit(entry), nil
		case compaction.AppendSuperseded:
			s.promoteSuperseded()
			head := *s.supersededTo
			return compaction.AppendResult{Outcome: outcome, Head: &head}, nil
		case compaction.AppendRebuildRoot:
			return compaction.AppendResult{Outcome: outcome, ReasonCode: compaction.ReasonChainBroken}, nil
		default:
			return compaction.AppendResult{Outcome: outcome, ReasonCode: compaction.ReasonAppendLockTimeout}, nil
		}
	}
	return s.commit(entry), nil
}

// commit 是"无脚本干预"时的真实追加：锁内重读有效头并校验，通过才落库。
// commit is the unscripted append: re-read the head, verify it, then insert.
func (s *fakeStore) commit(entry compaction.CompactionEntry) compaction.AppendResult {
	if s.rebuildReject {
		return compaction.AppendResult{Outcome: compaction.AppendRebuildRoot, ReasonCode: compaction.ReasonEpochRaced}
	}
	head, ok := s.head(entry.Scope)
	if ok {
		if entry.ChainEpoch != head.ChainEpoch || entry.ParentCompactionID != head.CompactionID ||
			entry.CoveredFromMessageID != head.FirstKeptMessageID {
			headCopy := head
			return compaction.AppendResult{Outcome: compaction.AppendSuperseded, Head: &headCopy}
		}
	} else if entry.ParentCompactionID != "" || entry.ChainEpoch != s.maxEpoch(entry.Scope)+1 {
		return compaction.AppendResult{Outcome: compaction.AppendRebuildRoot, ReasonCode: compaction.ReasonEpochRaced}
	}
	if existing, found, _ := s.find(entry.Scope, entry.CoveredFromMessageID, entry.CutMessageID); found {
		return compaction.AppendResult{Entry: existing, Outcome: compaction.AppendCommitted}
	}
	entry.Status = compaction.StatusCommitted
	s.appended = append(s.appended, entry)
	return compaction.AppendResult{Entry: entry, Outcome: compaction.AppendCommitted}
}

// promoteSuperseded 把"并发追加的新头"补进已提交链（只补一次）。
// promoteSuperseded makes the concurrently-appended head visible exactly once.
func (s *fakeStore) promoteSuperseded() {
	if s.supersededTo == nil || s.supersededOK {
		return
	}
	s.seeds = append(s.seeds, *s.supersededTo)
	s.supersededOK = true
}

// FindCommitted 按覆盖范围回读**已提交且未失效**的条目（祖先失效按未命中处理）。
// FindCommitted returns a committed, non-stale entry for the exact coverage range.
func (s *fakeStore) FindCommitted(_ context.Context, scope compaction.Scope, coveredFrom, cut string) (compaction.CompactionEntry, bool, error) {
	return s.find(scope, coveredFrom, cut)
}

// LatestCommittedChain 返回当前 `chain_epoch` 的已提交链（父到子）。
// LatestCommittedChain returns the current epoch's committed chain, parent first.
func (s *fakeStore) LatestCommittedChain(_ context.Context, scope compaction.Scope) ([]compaction.CompactionEntry, error) {
	all := s.committed(scope)
	if len(all) == 0 {
		return nil, nil
	}
	epoch := 0
	for _, e := range all {
		if e.ChainEpoch > epoch {
			epoch = e.ChainEpoch
		}
	}
	out := make([]compaction.CompactionEntry, 0, len(all))
	for _, e := range all {
		if e.ChainEpoch == epoch {
			out = append(out, e)
		}
	}
	return out, nil
}

// MarkStale 在同一把 scope 锁内把目标与全部后代置 `stale`（级联）。
// MarkStale marks the target and every descendant stale, as the real store does.
func (s *fakeStore) MarkStale(_ context.Context, compactionID, reason string) error {
	s.staleCallsV++
	if _, ok := s.stale[compactionID]; ok {
		return nil
	}
	s.stale[compactionID] = reason
	for {
		grew := false
		for _, e := range append(append([]compaction.CompactionEntry{}, s.seeds...), s.appended...) {
			if _, done := s.stale[e.CompactionID]; done {
				continue
			}
			if _, parentStale := s.stale[e.ParentCompactionID]; e.ParentCompactionID != "" && parentStale {
				s.stale[e.CompactionID] = reason
				grew = true
			}
		}
		if !grew {
			return nil
		}
	}
}

// all 按 **scope 规范串**取条目：会话范围的身份是 `session:<id>`，条目里的 `RunID`
// 只是"哪次 Run 触发的"注记，**不参与**匹配——否则同一会话的两个 Run 永远成不了一条链
// （L2 滚动增量直接失效）。真实适配层同样只能按 `Scope.Canonical()` 建键。
// all matches on the canonical scope: a session entry's RunID is provenance, not identity.
func (s *fakeStore) all(scope compaction.Scope) []compaction.CompactionEntry {
	out := make([]compaction.CompactionEntry, 0, len(s.seeds)+len(s.appended))
	for _, e := range append(append([]compaction.CompactionEntry{}, s.seeds...), s.appended...) {
		if e.Scope.Canonical() == scope.Canonical() {
			out = append(out, e)
		}
	}
	return out
}

func (s *fakeStore) committed(scope compaction.Scope) []compaction.CompactionEntry {
	out := make([]compaction.CompactionEntry, 0, len(s.seeds)+len(s.appended))
	for _, e := range s.all(scope) {
		if e.Status != compaction.StatusCommitted {
			continue
		}
		if _, stale := s.stale[e.CompactionID]; stale {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (s *fakeStore) head(scope compaction.Scope) (compaction.CompactionEntry, bool) {
	chain, _ := s.LatestCommittedChain(context.Background(), scope)
	if len(chain) == 0 {
		return compaction.CompactionEntry{}, false
	}
	return chain[len(chain)-1], true
}

func (s *fakeStore) find(scope compaction.Scope, coveredFrom, cut string) (compaction.CompactionEntry, bool, error) {
	for _, e := range s.committed(scope) {
		if e.CoveredFromMessageID == coveredFrom && e.CutMessageID == cut {
			return e, true, nil
		}
	}
	return compaction.CompactionEntry{}, false, nil
}

func (s *fakeStore) maxEpoch(scope compaction.Scope) int {
	best := 0
	for _, e := range s.all(scope) {
		if e.ChainEpoch > best {
			best = e.ChainEpoch
		}
	}
	return best
}

// seedChain 预置一条已提交链（缺省补 `status=committed`、`chain_epoch=1`）。
// seedChain seeds a committed chain with the status and epoch defaults filled in.
func (s *fakeStore) seedChain(_ *testing.T, entries ...compaction.CompactionEntry) {
	for _, e := range entries {
		s.seeds = append(s.seeds, normalizeEntry(e))
	}
}

// seedParentEntry 预置一份既有摘要（只有标识与两个引用集合，覆盖范围取整个范围起点）。
// seedParentEntry seeds an existing summary entry; its coverage starts at m1 by default.
func (s *fakeStore) seedParentEntry(entry compaction.CompactionEntry) {
	if entry.CoveredFromMessageID == "" {
		entry.CoveredFromMessageID = "m1"
	}
	if entry.CutMessageID == "" {
		entry.CutMessageID = "m1"
	}
	if entry.FirstKeptMessageID == "" {
		entry.FirstKeptMessageID = "m1"
	}
	s.seeds = append(s.seeds, normalizeEntry(entry))
}

func normalizeEntry(e compaction.CompactionEntry) compaction.CompactionEntry {
	if e.Status == "" {
		e.Status = compaction.StatusCommitted
	}
	if e.ChainEpoch == 0 {
		e.ChainEpoch = 1
	}
	if e.Scope == (compaction.Scope{}) {
		e.Scope = compaction.Scope{SessionID: "s1"}
	}
	return e
}

// appendOutcome 让接下来的 N 次 Append 返回指定结局（一次性）。
// appendOutcome scripts the next append outcomes.
func (s *fakeStore) appendOutcome(outcomes ...compaction.AppendOutcome) {
	s.script = append(s.script, outcomes...)
}

// appendSupersededOnce 让下一次 Append 返回"被超越"：新头在**那次追加的时刻**才进链
// （真实场景里头由并发的另一次追加推上去，引擎读链时还看不到它）。
// appendSupersededOnce makes the next append report a concurrent head that only becomes
// visible at append time.
func (s *fakeStore) appendSupersededOnce(head compaction.CompactionEntry) {
	head = normalizeEntry(head)
	s.supersededTo = &head
	s.script = append(s.script, compaction.AppendSuperseded)
}

// rebuildRejected 让每一次 Append 都返回"无有效头"，模拟并发再次失效 / 断链。
// rebuildRejected makes every append report "no valid head".
func (s *fakeStore) rebuildRejected() { s.rebuildReject = true }

// count 返回**由 Append 写入**的已提交条目数（预置链不计）。
// count is the number of rows written by Append; seeded entries never count.
func (s *fakeStore) count(t *testing.T) int {
	t.Helper()
	return len(s.appended)
}

// staleCount 返回当前处于 stale 的条目数。
// staleCount is the number of stale entries after cascading.
func (s *fakeStore) staleCount(t *testing.T) int {
	t.Helper()
	return len(s.stale)
}

// staleCalls 返回 MarkStale 被调用的次数（"只标最早失效的那一条"的判据）。
// staleCalls is the number of MarkStale invocations.
func (s *fakeStore) staleCalls(t *testing.T) int {
	t.Helper()
	return s.staleCallsV
}

// committedMessagesWith 返回正文包含该文本的消息条数（摘要不得写回消息表）。
// committedMessagesWith counts message rows containing the text.
func (s *fakeStore) committedMessagesWith(t *testing.T, text string) int {
	t.Helper()
	n := 0
	for _, m := range s.h.msgs {
		if strings.Contains(m.Content, text) {
			n++
		}
	}
	return n
}

// entryByID 在预置链与已写入条目里按标识查找。
// entryByID finds an entry by id across seeds and appends.
func (s *fakeStore) entryByID(t *testing.T, id string) (compaction.CompactionEntry, bool) {
	t.Helper()
	for _, e := range append(append([]compaction.CompactionEntry{}, s.seeds...), s.appended...) {
		if e.CompactionID == id {
			return e, true
		}
	}
	return compaction.CompactionEntry{}, false
}

// —— 来源校验替身 ——

type fakeValidity struct {
	revoked map[core.EvidenceRef]string
	err     error
	checks  int
}

// Check 按可编程失效集合返回结论；未登记即视为有效。
// Check returns the programmed verdict; anything unregistered is valid.
func (v *fakeValidity) Check(_ context.Context, p core.Principal, r core.EvidenceRef) (compaction.Validity, error) {
	v.checks++
	if v.err != nil {
		return compaction.Validity{}, v.err
	}
	if !p.Valid() {
		return compaction.Validity{}, core.ErrUnauthorized
	}
	if reason, ok := v.revoked[r]; ok {
		return compaction.Validity{Reason: reason}, nil
	}
	return compaction.Validity{Valid: true}, nil
}

// revoke 让指定引用在校验时失效。
// revoke marks a reference as no longer injectable.
func (v *fakeValidity) revoke(r core.EvidenceRef) {
	v.revoked[r] = compaction.ValidityRevoked
}

// —— 消息读取器替身 ——

type fakeReader struct {
	h    *compactionHarness
	n    int
	fail bool
	why  string
}

func (r *fakeReader) ListBySession(_ context.Context, sessionID string, afterSeq int64, limit int) ([]compaction.StoredMessage, error) {
	r.n++
	if r.fail {
		r.h.t.Fatalf("这条路径不该读 PG：%s", r.why)
	}
	// 真实实现是 `ORDER BY (session_id, seq)`：顺序由读取端口保证，引擎不得另按标识排序。
	ordered := append([]compaction.StoredMessage{}, r.h.msgs...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })
	out := make([]compaction.StoredMessage, 0, len(ordered))
	for _, m := range ordered {
		if r.h.sessions[m.MessageID] != sessionID || m.Seq <= afterSeq {
			continue
		}
		out = append(out, m)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (r *fakeReader) calls(t *testing.T) int {
	t.Helper()
	return r.n
}

func (r *fakeReader) failIfCalled(t *testing.T, why string) {
	t.Helper()
	r.fail = true
	r.why = why
}

// —— 无状态历史替身 ——

type fakeHistory struct{ msgs []core.Message }

func (f *fakeHistory) Messages() []core.Message {
	return append([]core.Message{}, f.msgs...)
}

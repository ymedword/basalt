package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件是压缩接线的**真实 Run 驱动**验收（颗粒度文档 §4.3 `us16-wiring`，R21 / R23 的落点）：
// 走真实的 `core.AgentService.Run` → `core.ReActLoop` → `PromptBuilder`，只把模型、消息仓储、
// 来源校验器、压缩器换成假端口，**不连 PG、不打网络**。
//
// 判的是**接线**而不是引擎：压缩引擎的算法由 `internal/compaction` 自带用例判卷；这里要证的
// 是"循环交给压缩器的输入正确、压缩器交回的摘要正确注入、注入与降级两条路径都过来源闸门、
// 身份按 Run 冻结且不外泄"。因此压缩器是**录制用的假实现**——它记录每一次 `CompactRequest`
// 并按用例要求返回"无摘要 / 可注入摘要 / 降级"三种结局之一。
//
// 假端口全部 `cmpw` 前缀（compaction wiring），与本包既有夹具不重名；`newWiringHarness`
// 一次装配出可跑的 `*core.AgentService`。**不得**复用 `run_test.go` 的夹具——那个文件带
// `//go:build integration`，默认构建集里根本不存在。
//
// This file drives the compaction wiring through a real run. The compactor itself is a
// recorder: the algorithm is judged elsewhere, what is judged here is the wiring.

const (
	// cmpwOutputReserve 是接线时给的回复预留（`compaction.output_reserve_tokens` 的替身）。
	// 刻意取一个**不等于** `core.ReserveTokens` 的值：两者相等时"用协议余量顶替输出预留"
	// 的逆改点会静默通过。
	cmpwOutputReserve = 700
	// cmpwAgentUID / cmpwResourceUID 是合法的 UID 字面量（前缀 + 26 位 Crockford Base32）。
	cmpwAgentUID    = "ag_0123456789ABCDEFGHJKMNPQRS"
	cmpwResourceUID = "rs_0123456789ABCDEFGHJKMNPQRS"
	// cmpwSectionOne 是快照冻结的段一正文：非空才有不可裁开销，`FixedParts` 才非零。
	cmpwSectionOne = "你是 Basalt 测试 Agent。"
	// cmpwPlaceholder 与 `compaction.SourceGate` 的失效占位符逐字一致（契约判据）。
	cmpwPlaceholder = "[内容不可用：来源已失效]"
)

// errCmpwUnsupported 是夹具对"本用例不该走到的生命周期写方法"的答复。
// The fixture answers lifecycle writes it is never supposed to receive.
var errCmpwUnsupported = errors.New("cmpw: 本夹具只支持生命周期读取")

// —— 用例夹具 ——

// wiringHarness 是一套可直接跑 `AgentService.Run` 的接线夹具。
//
// `messages` 与 `store` **指向同一个 ledger**：颗粒度文档把两者写成两个名字，是因为
// `h.messages` 读的是消息面（历史 / 正文 / 角色）、`h.store` 读的是仓储面（落库引用与
// 追加次数）。同一份实现同时满足 `core.MessageStore` 与 `compaction.MessageReader`，
// 两处逐字代码都不必改，而"引用随消息同事务落库"这条判据正是在同一处观测的。
//
// messages and store name the same ledger: the message face and the persistence face are
// two views of one implementation, which is exactly what makes the same-transaction
// assertion meaningful.
type wiringHarness struct {
	t         *testing.T
	ledger    *cmpwLedger
	messages  *cmpwLedger
	store     *cmpwLedger
	validity  *cmpwValidity
	chat      *cmpwChat
	recorder  *cmpwRecorder
	assembly  *cmpwAssembly
	wiring    *compactionWiring
	filter    core.SourceFilter
	svc       *core.AgentService
	principal core.Principal
	agent     string
	session   string
	message   string
	runID     string
	budget    core.Budget
}

// newWiringHarness 装配一套完整的运行链路：资源解析 → 生成快照 → 真实循环 → 压缩接线 → 提示词。
// 只有"来源校验器 / 消息仓储 / 模型 / 压缩器"是假的，其余全是生产代码。
// The harness assembles the production graph and replaces only four collaborators.
func newWiringHarness(t *testing.T) *wiringHarness {
	t.Helper()
	logger, _ := newCapturedLogger()
	ledger := newCmpwLedger()
	h := &wiringHarness{
		t:         t,
		ledger:    ledger,
		messages:  ledger,
		store:     ledger,
		validity:  newCmpwValidity(),
		chat:      newCmpwChat(),
		recorder:  &cmpwRecorder{},
		assembly:  &cmpwAssembly{},
		principal: cmpwPrincipal("caller-1", "key-1"),
		agent:     "agent-a",
		session:   "sess-1",
		message:   "查 A 文档",
	}

	resolver := &cmpwResolver{tool: &cmpwSourceTool{}}
	wiring, err := newCompactionWiring(compactionWiringDeps{
		Enabled: true,
		Policy: compaction.Policy{
			TriggerRatio:      0.8,
			SafetyMarginRatio: 0.1,
			KeepRecentGroups:  1,
			SummaryMaxTokens:  512,
			SummaryInputBytes: 8192,
			SummaryTimeout:    5 * time.Second,
			StrategyVersion:   "cmpw-strategy-v1",
			PromptVersion:     "cmpw-prompt-v1",
		},
		OutputReserve: cmpwOutputReserve,
		Messages:      ledger,
		Store:         &cmpwCompactionStore{},
		Validity:      h.validity,
		Audit:         &cmpwAuditPort{},
		Quota:         &cmpwQuotaPort{},
		Now:           cmpwClock,
		Logger:        logger,
	})
	require.NoError(t, err, "接线依赖齐全时不得拒绝装配")
	h.wiring = wiring
	h.filter = wiring.SourceFilter()
	require.NotNil(t, h.filter, "来源闸门必须接线：降级回退的原始历史同样不得漏出失效来源")

	builder := core.NewPromptBuilder(resolver).WithSourceFilter(h.filter)
	executor := core.NewToolExecutor(resolver, &cmpwInvocations{}, logger)
	loop := core.NewReActLoop(builder, executor, logger).
		WithCompaction(wiring.OutputReserve(), func(env *core.RunEnv) core.ContextCompactor {
			// 记录装配可见的事实：摘要客户端是否真的从准入映射表接上（`Assemble` 为空即
			// "本次 Run 不压缩"，压缩根本不会发生——那是 T090 要挡住的第一类逆改）。
			h.recorder.observeAssembly(wiring.Assemble(env) != nil)
			return h.recorder
		}).
		// 对话装配记录（V-7）：本夹具判"压缩结论随记录交出去"，与生产 `wire.go` 的
		// 接线形状一致（真压缩接线 + 唯一装配写入器）。
		WithAssemblyRecorder(h.assembly)

	runtime := &cmpwRuntime{client: h.chat}
	h.svc = core.NewAgentService(core.ServiceOptions{
		Agents:        &cmpwAgents{snapshot: cmpwSnapshot()},
		Client:        h.chat,
		Messages:      ledger,
		Runs:          &cmpwRunStore{},
		Snapshots:     &cmpwSnapshotStore{},
		Loop:          loop,
		Logger:        logger,
		Now:           cmpwClock,
		MaxConcurrent: 8,
		Sessions:      &cmpwSessions{},
		Lifecycle:     &cmpwLifecycle{},
		Resources: &cmpwResources{
			runtime: runtime,
			// 摘要客户端与对话客户端来自同一张固定映射表（原则 III）：这里让它们同源，
			// 正是准入冻结语义的替身。
			summary: h.chat,
		},
		RunSnapshots: &cmpwSnapshotBuilder{snapshot: cmpwSnapshot()},
	})
	return h
}

// cmpwClock 是本文件的固定时钟（`ServiceOptions.Now` / `LifecycleRecord.UpdatedAt` 共用）。
// cmpwClock is the fixed clock shared by the service options and the lifecycle record.
func cmpwClock() time.Time { return baselineClock() }

// model 设置默认模型脚本（对所有会话生效），`callTool` 挂到紧邻的上一个 `step` 上。
// model scripts the fake provider for every session; callTool attaches to the previous step.
func (h *wiringHarness) model(args ...cmpwStepArg) { h.modelFor("", args...) }

// modelFor 为指定会话设置脚本（并发用例按会话区分响应）。
// modelFor scripts one session so concurrent runs never read each other's replies.
func (h *wiringHarness) modelFor(sessionID string, args ...cmpwStepArg) {
	h.chat.script(sessionID, args...)
}

// run 以夹具默认身份跑一次并断言收口在 `completed`。
// run performs one run with the fixture principal and requires a completed outcome.
func (h *wiringHarness) run(t *testing.T) error {
	t.Helper()
	return h.runFor(t, h.session, h.principal, h.message)
}

// runFor 以显式身份 / 会话跑一次（并发与多身份用例用）。
// runFor performs one run with an explicit principal and session.
func (h *wiringHarness) runFor(t *testing.T, sessionID string, p core.Principal, message string) error {
	t.Helper()
	res, err := h.svc.Run(context.Background(), core.RunRequest{
		Principal: p,
		RunID:     h.runID,
		Agent:     h.agent,
		SessionID: sessionID,
		Message:   message,
		Channel:   "cli",
		Budget:    h.budget,
	})
	if err != nil {
		return err
	}
	require.Equal(t, core.StatusCompleted, res.Status, "运行应当正常收口（reason=%q）", res.Reason)
	return nil
}

// lastPrompt 取"模型这一轮实际看到的内容"。
//
// `System` / `History` / `Tools` 逐字来自假模型收到的 `ChatRequest`；`EvidenceRefs` **只能**
// 经 assistant 消息观测——`Prompt.EvidenceRefs` 不进 `ChatRequest`，它的唯一出口是循环把它
// 赋给 `assistant.EvidenceRefs` 之后随消息落库。实现正确时两者是同一个值，而 R23 的逆改点
// 恰恰是"这条链被改坏"，故这里如实按该等价关系取数（注释即判据，不另造接口）。
//
// The prompt's evidence set has exactly one exit — the assistant message the loop stamps it
// onto — so the view reads it there and documents the equivalence.
func (h *wiringHarness) lastPrompt(t *testing.T) cmpwPromptView {
	t.Helper()
	req, ok := h.chat.lastRequest()
	require.True(t, ok, "假模型至少应收到一次调用")
	view := cmpwPromptView{System: req.Messages[0].Content, History: req.Messages[1:], Tools: req.Tools}
	if assistant, ok := h.ledger.lastWithRole("assistant"); ok {
		view.EvidenceRefs = assistant.EvidenceRefs
	}
	return view
}

// cmpwPromptView 是一次组装的读面视图（字段来源见 `lastPrompt`）。
// cmpwPromptView is the read-side view of one assembly; see lastPrompt for provenance.
type cmpwPromptView struct {
	System       string
	History      []core.Message
	Tools        []core.ToolSpec
	EvidenceRefs []core.EvidenceRef
}

// text 返回模型这一轮实际读到的全部正文（系统段 + 历史），供"原文不得出现"类断言使用。
// text joins everything the model actually read this round.
func (v cmpwPromptView) text() string {
	var b strings.Builder
	b.WriteString(v.System)
	for _, m := range v.History {
		b.WriteString("\n")
		b.WriteString(m.Content)
	}
	return b.String()
}

// byID 在**过滤返回值**里按消息身份取那一条（`SourceGate` 契约要求保留位置与 ID）。
// byID picks one message out of a filter result by identity.
func byID(t *testing.T, msgs []core.Message, id string) core.Message {
	t.Helper()
	for _, m := range msgs {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("过滤结果里没有消息 %q（失效消息必须整条换占位符，不得从结果里删除）", id)
	return core.Message{}
}

// ref 构造一条来源引用。`Kind` 只用于人读与日志，`Valid()` 只要求 ID 与 VersionID。
// ref builds one evidence reference.
func ref(id, version string) core.EvidenceRef {
	return core.EvidenceRef{Kind: "knowledge", ID: id, VersionID: version}
}

// cmpwPrincipal 构造合法的调用方身份（`Valid()` 要求非内置、非管理员、非空 scope 集）。
// cmpwPrincipal builds a valid caller principal.
func cmpwPrincipal(callerID, keyID string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		KeyID:    keyID,
		Scopes:   []string{core.ScopeSessionsWrite},
		Actor:    core.ActorCaller,
	}
}

// cmpwSnapshot 是冻结的配置快照：段一非空（否则 `FixedParts` 全零）、Profile 带模型与
// 工具集、`Settings` 显式写满（`MaxHistoryTurns` 显式 0 会被保留成 0 轮，历史全被裁掉）。
// cmpwSnapshot is the frozen snapshot every fake path agrees on.
func cmpwSnapshot() *core.ConfigSnapshot {
	return &core.ConfigSnapshot{
		AgentName:  "agent-a",
		Hash:       "cfg-cmpw",
		Content:    "cmpw",
		SectionOne: cmpwSectionOne,
		Profile: &core.Profile{
			Name: "agent-a",
			Provider: core.ProviderRef{
				Name:  "prov-a",
				Model: "model-a",
			},
			Tools:    []string{"source_view"},
			Settings: core.Settings{MaxIterations: 10, MaxHistoryTurns: 20},
		},
	}
}

// —— 脚本化假模型 ——

// cmpwStep 是一轮模型响应的脚本：正文 + 本轮发起的工具调用。
// cmpwStep is one scripted model turn.
type cmpwStep struct {
	text  string
	calls []core.ToolCall
}

// cmpwStepArg 让 `h.model(...)` 读起来与颗粒度文档逐字一致：`step` 追加一轮，
// `callTool` 把调用挂到**上一轮**（同一轮里可以有多个工具调用）。
// cmpwStepArg keeps the call sites reading exactly like the design document.
type cmpwStepArg func(*[]cmpwStep)

// step 追加一轮模型响应（正文）。
// step appends one model turn carrying plain text.
func step(text string) cmpwStepArg {
	return func(steps *[]cmpwStep) {
		*steps = append(*steps, cmpwStep{text: text})
	}
}

// callTool 把一次工具调用挂到紧邻的上一个 `step` 上；入参里带上要回传的来源引用，
// 假工具据此产出 `Result.EvidenceRefs`（C-26：来源依赖必须随转述传播）。
// callTool attaches a tool call to the previous step, carrying the reference the fake
// tool echoes back as the result's evidence.
func callTool(name string, r core.EvidenceRef) cmpwStepArg {
	return func(steps *[]cmpwStep) {
		if len(*steps) == 0 {
			*steps = append(*steps, cmpwStep{})
		}
		last := &(*steps)[len(*steps)-1]
		last.calls = append(last.calls, core.ToolCall{
			ID:        fmt.Sprintf("call-%d-%d", len(*steps), len(last.calls)+1),
			Name:      name,
			Arguments: fmt.Sprintf(`{"evidence_id":%q,"version_id":%q}`, r.ID, r.VersionID),
		})
	}
}

// cmpwChat 是按会话分流的假模型：脚本按 `(SessionID)` 各自推进，并发用例因此不会互相
// 抢脚本；同时记录每一次 `ChatRequest`（提示词视图的唯一来源）。
// cmpwChat scripts responses per session so concurrent runs never steal each other's turns.
type cmpwChat struct {
	mu      sync.Mutex
	scripts map[string][]cmpwStep
	next    map[string]int
	reqs    []core.ChatRequest
}

// newCmpwChat 建一个空脚本的假模型。
// newCmpwChat builds a fake provider with no script.
func newCmpwChat() *cmpwChat {
	return &cmpwChat{scripts: map[string][]cmpwStep{}, next: map[string]int{}}
}

// script 登记（或替换）某个会话的响应脚本；空串键是"所有会话的缺省脚本"。
// script registers the reply script of one session; the empty key is the default.
func (c *cmpwChat) script(sessionID string, args ...cmpwStepArg) {
	steps := make([]cmpwStep, 0, len(args))
	for _, arg := range args {
		arg(&steps)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.scripts[sessionID] = steps
	c.next[sessionID] = 0
}

// Chat 实现 `core.ProviderClient`：按会话取下一轮脚本；脚本用尽返回固定文本，
// 让用例因**断言不符**失败而不是因"多出一轮"意外通过。
// Chat implements core.ProviderClient; an exhausted script yields a fixed text so a run
// still converges and the failure shows up on an assertion.
func (c *cmpwChat) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	steps, ok := c.scripts[req.SessionID]
	if !ok {
		steps = c.scripts[""]
	}
	idx := c.next[req.SessionID]
	c.next[req.SessionID] = idx + 1
	if idx >= len(steps) {
		return core.ChatResponse{Content: "（cmpw 脚本已用尽）"}, nil
	}
	s := steps[idx]
	return core.ChatResponse{
		Content:   s.text,
		ToolCalls: s.calls,
		Usage:     core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

// lastRequest 返回最后一次调用；无调用时 ok=false。
// lastRequest returns the most recent request.
func (c *cmpwChat) lastRequest() (core.ChatRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		return core.ChatRequest{}, false
	}
	return c.reqs[len(c.reqs)-1], true
}

// lastRequestFor 返回指定会话的最后一次调用。
// lastRequestFor returns the latest request of one session.
func (c *cmpwChat) lastRequestFor(sessionID string) (core.ChatRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.reqs) - 1; i >= 0; i-- {
		if c.reqs[i].SessionID == sessionID {
			return c.reqs[i], true
		}
	}
	return core.ChatRequest{}, false
}

// calls 返回累计调用次数。
// calls returns the number of model calls so far.
func (c *cmpwChat) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// promptTextFor 返回指定会话最后一次调用里模型读到的全部正文。
// promptTextFor joins everything the model read in that session's latest call.
func (c *cmpwChat) promptTextFor(sessionID string) string {
	req, ok := c.lastRequestFor(sessionID)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// —— 录制用的假压缩器 ——

// cmpwRecorder 实现 `core.ContextCompactor`：记录每一次 `CompactRequest`，并按用例要求
// 返回三种结局之一（无摘要 / 可注入摘要 / 降级）。压缩**算法**不在这里判——那归
// `internal/compaction` 自己的用例；这里判的是循环交给它的输入对不对。
// cmpwRecorder records every CompactRequest and returns the outcome the test asked for.
type cmpwRecorder struct {
	mu        sync.Mutex
	reqs      []core.CompactRequest
	history   []cmpwHistoryView
	assembly  []bool
	summary   *core.CompactionSummary
	degraded  bool
	reasonFor string
}

// cmpwAssembly 实现 `core.AssemblyRecorder`：录下循环交出的每一条对话装配记录。
// 端口无返回值（记录可丢弃），所以这里只做取值，不模拟失败。
// cmpwAssembly records every chat assembly record the loop hands over.
type cmpwAssembly struct {
	mu      sync.Mutex
	records []core.AssemblyRecord
}

// Record 实现 `core.AssemblyRecorder`。
func (r *cmpwAssembly) Record(_ context.Context, record core.AssemblyRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
}

// snapshot 返回录到的记录副本。
func (r *cmpwAssembly) snapshot() []core.AssemblyRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.AssemblyRecord(nil), r.records...)
}

// cmpwHistoryView 是压缩请求里历史端口的**调用时刻**读数（不是事后快照）：
// `nil` 与"空切片"是两种接线事实，必须分开记。
// cmpwHistoryView captures what the history port looked like at call time.
type cmpwHistoryView struct {
	nilPort bool
	ids     []string
}

// observeAssembly 记录一次装配：`withSummary` 为真表示 `Assemble` 给出了压缩器。
// observeAssembly records whether the wiring produced a compactor for this run.
func (r *cmpwRecorder) observeAssembly(withSummary bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assembly = append(r.assembly, withSummary)
}

// Compact 实现 `core.ContextCompactor`。
// Compact implements core.ContextCompactor.
func (r *cmpwRecorder) Compact(ctx context.Context, req core.CompactRequest) (core.CompactResult, error) {
	view := cmpwHistoryView{nilPort: req.History == nil}
	if req.History != nil {
		for _, m := range req.History.Messages() {
			view.ids = append(view.ids, m.ID)
		}
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.history = append(r.history, view)
	summary, degraded, reason := r.summary, r.degraded, r.reasonFor
	r.mu.Unlock()

	switch {
	case degraded:
		return core.CompactResult{Degraded: true, ReasonCode: reason}, nil
	case summary != nil:
		return core.CompactResult{Entry: summary}, nil
	default:
		return core.CompactResult{}, nil
	}
}

// invocations 返回累计压缩判定次数。
// invocations returns how many compaction decisions were made.
func (r *cmpwRecorder) invocations() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// lastRequest 返回最后一次压缩请求（无请求时 fail）。
// lastRequest returns the latest compaction request.
func (r *cmpwRecorder) lastRequest(t *testing.T) core.CompactRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.reqs, "循环必须在每轮模型调用前判一次压缩")
	return r.reqs[len(r.reqs)-1]
}

// lastHistory 返回最后一次压缩请求在**调用时刻**看到的历史读数。
// lastHistory returns the history view taken at the latest compaction call.
func (r *cmpwRecorder) lastHistory(t *testing.T) cmpwHistoryView {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.history, "循环必须在每轮模型调用前判一次压缩")
	return r.history[len(r.history)-1]
}

// assembledWithSummary 报告是否每一次装配都拿到了压缩器。
// assembledWithSummary reports whether every assembly produced a compactor.
func (r *cmpwRecorder) assembledWithSummary() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.assembly) > 0 && !slices.Contains(r.assembly, false)
}

// seedSummary 预置一份"可注入摘要"：此后每一次压缩判定都返回它（R23 的注入路径）。
// seedSummary pre-loads an injectable summary returned by every compaction decision.
func (h *wiringHarness) seedSummary(s core.CompactionSummary) {
	h.recorder.mu.Lock()
	defer h.recorder.mu.Unlock()
	h.recorder.summary = &s
}

// seedDegraded 让压缩判定返回降级（R23 之外的"回退原始历史"路径）。
// seedDegraded makes every compaction decision degrade to the raw history.
func (h *wiringHarness) seedDegraded(reason string) {
	h.recorder.mu.Lock()
	defer h.recorder.mu.Unlock()
	h.recorder.degraded = true
	h.recorder.reasonFor = reason
}

// —— 假消息仓储 ——

// cmpwLedger 同时实现 `core.MessageStore`（四方法）与 `compaction.MessageReader`：
// 一份实现、两个消费面。`appendCalls` 与 `refs` 是"同事务落库"判据的观测点。
// cmpwLedger serves both the message store and the compaction reader; the per-message
// append count and evidence set are what the same-transaction assertions read.
type cmpwLedger struct {
	mu      sync.Mutex
	seq     int64
	byID    map[string]*cmpwStored
	order   []*cmpwStored
	appends map[string]int
	refs    map[string][]core.EvidenceRef
}

// newCmpwLedger 建一个空账本。
// newCmpwLedger builds an empty ledger.
func newCmpwLedger() *cmpwLedger {
	return &cmpwLedger{
		byID:    map[string]*cmpwStored{},
		appends: map[string]int{},
		refs:    map[string][]core.EvidenceRef{},
	}
}

// cmpwStored 是账本里的一行：消息本体 + 会话 + 运行 + 序号。
// cmpwStored is one ledger row.
type cmpwStored struct {
	id        string
	sessionID string
	runID     string
	seq       int64
	msg       core.Message
}

// AppendMessage 实现 `core.MessageStore`：分配身份、记账引用、累计追加次数。
// AppendMessage implements core.MessageStore and is the only write path in this fixture.
func (l *cmpwLedger) AppendMessage(ctx context.Context, sessionID, runID string, m core.Message) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	id := fmt.Sprintf("m%d", l.seq)
	m.ID = id
	l.byID[id] = &cmpwStored{id: id, sessionID: sessionID, runID: runID, seq: l.seq, msg: m}
	l.order = append(l.order, l.byID[id])
	l.appends[id]++
	l.refs[id] = append([]core.EvidenceRef(nil), m.EvidenceRefs...)
	return id, nil
}

// Messages 实现 `core.MessageStore`：按追加顺序返回（含身份与来源引用）。
// Messages implements core.MessageStore in append order.
func (l *cmpwLedger) Messages(ctx context.Context, sessionID string) ([]core.Message, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]core.Message, 0, len(l.order))
	for _, row := range l.order {
		if row.sessionID != sessionID {
			continue
		}
		out = append(out, row.msg)
	}
	return out, nil
}

// UpsertSession 实现 `core.MessageStore`（本夹具不判会话行）。
// UpsertSession implements core.MessageStore; session rows are out of scope here.
func (l *cmpwLedger) UpsertSession(ctx context.Context, rec core.SessionRecord) error {
	return nil
}

// ListSessions 实现 `core.MessageStore`（本夹具不判会话列表）。
// ListSessions implements core.MessageStore; session listing is out of scope here.
func (l *cmpwLedger) ListSessions(ctx context.Context, filter core.SessionFilter) ([]core.SessionRecord, error) {
	return nil, nil
}

// ListBySession 实现 `compaction.MessageReader`。
// ListBySession implements compaction.MessageReader.
func (l *cmpwLedger) ListBySession(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]compaction.StoredMessage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []compaction.StoredMessage
	for _, row := range l.order {
		if row.sessionID != sessionID || row.seq <= afterSeq {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, compaction.StoredMessage{
			MessageID:  row.id,
			Seq:        row.seq,
			RunID:      row.runID,
			Role:       row.msg.Role,
			Content:    row.msg.Content,
			ToolCallID: row.msg.ToolCallID,
			ToolCalls:  row.msg.ToolCalls,
			SourceRefs: append([]core.EvidenceRef(nil), row.msg.EvidenceRefs...),
		})
	}
	return out, nil
}

// seedAssistant 预置一条**尚未归属会话**的历史转述（带来源引用），返回其消息身份；
// 归属由随后的 `seedSession` 一次绑定。
// seedAssistant pre-loads one historical assistant message with evidence refs; the session
// binding follows through seedSession.
func (l *cmpwLedger) seedAssistant(t *testing.T, text string, refs ...core.EvidenceRef) string {
	t.Helper()
	return l.seedAssistantIn(t, "", text, refs...)
}

// seedAssistantIn 把历史转述直接预置到指定会话（多会话用例各备一份历史）。
// seedAssistantIn seeds a historical message straight into one session.
func (l *cmpwLedger) seedAssistantIn(t *testing.T, sessionID, text string, refs ...core.EvidenceRef) string {
	t.Helper()
	id, err := l.AppendMessage(context.Background(), sessionID, "", core.Message{
		Role:         core.RoleAssistant,
		Content:      text,
		EvidenceRefs: refs,
	})
	require.NoError(t, err)
	return id
}

// seedSession 把**尚未归属会话**的预置消息绑定到指定会话；已归属的行不受影响
// （把所有行一律改绑会抹掉别会话的历史）。
// seedSession binds only the rows that carry no session yet; rows already bound keep
// their session, otherwise one call would erase another session's history.
func (l *cmpwLedger) seedSession(t *testing.T, sessionID string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, row := range l.order {
		if row.sessionID == "" {
			row.sessionID = sessionID
		}
	}
}

// refsOf 返回该消息追加时记录的来源引用集合。
// refsOf returns the evidence refs recorded when the message was appended.
func (l *cmpwLedger) refsOf(t *testing.T, id string) []core.EvidenceRef {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]core.EvidenceRef(nil), l.refs[id]...)
}

// appendCallsFor 返回该消息**被追加的次数**（>1 即出现第二套追加路径）。
// appendCallsFor counts how many times that message was appended.
func (l *cmpwLedger) appendCallsFor(t *testing.T, id string) int {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appends[id]
}

// contentOf 返回仓储里该消息的正文（用于对照"过滤不改写存储"）。
// contentOf reads the stored body, proving the injection filter never rewrites storage.
func (l *cmpwLedger) contentOf(t *testing.T, id string) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.byID[id]
	require.True(t, ok, "账本里没有消息 %q", id)
	return row.msg.Content
}

// all 返回全量历史（含身份），供 `SourceFilter` 直接驱动。
// all returns the whole history with identities so the filter can be driven directly.
func (l *cmpwLedger) all(t *testing.T) []core.Message {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]core.Message, 0, len(l.order))
	for _, row := range l.order {
		out = append(out, row.msg)
	}
	return out
}

// lastWithRole 返回最后一条指定角色的消息。
// lastWithRole returns the latest message of that role.
func (l *cmpwLedger) lastWithRole(role string) (core.Message, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.order) - 1; i >= 0; i-- {
		if l.order[i].msg.Role == role {
			return l.order[i].msg, true
		}
	}
	return core.Message{}, false
}

// lastWithRole 是 fail 版本（用例里"应当存在"是判据的一部分）。
// lastWithRole asserts existence; a missing message fails the test.
func (l *cmpwLedger) lastWithRoleT(t *testing.T, role string) core.Message {
	t.Helper()
	m, ok := l.lastWithRole(role)
	require.True(t, ok, "历史里应当至少有一条 %s 消息", role)
	return m
}

// —— 假来源校验器 ——

// cmpwValidity 实现 `compaction.ValidityChecker`：按 (Principal.KeyID, ref) 判定有效性，
// 并记录每一次带身份调用的 Principal——"过滤器拿到的是本次 Run 的身份"由此可判。
// cmpwValidity decides validity per (key, ref) and records the principal of every call.
type cmpwValidity struct {
	mu       sync.Mutex
	revoked  map[string]string            // refKey → reason（对所有身份生效）
	perKey   map[string]map[string]string // keyID → refKey → reason（只对该 Key 生效）
	observed []cmpwValidityCall
}

// cmpwValidityCall 是一次校验的可观测事实。
// cmpwValidityCall records one check.
type cmpwValidityCall struct {
	Principal core.Principal
	Ref       core.EvidenceRef
}

// newCmpwValidity 建一个"一切有效"的校验器。
// newCmpwValidity starts with everything valid.
func newCmpwValidity() *cmpwValidity {
	return &cmpwValidity{revoked: map[string]string{}, perKey: map[string]map[string]string{}}
}

// revoke 让某条引用对所有身份失效。
// revoke invalidates one reference for every principal.
func (v *cmpwValidity) revoke(r core.EvidenceRef) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.revoked[cmpwRefKey(r)] = "revoked"
}

// denyForKey 让某条引用只对指定 Key 失效（同调用方另一把 Key 仍可见）。
// denyForKey invalidates one reference for one key only.
func (v *cmpwValidity) denyForKey(keyID string, r core.EvidenceRef) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.perKey[keyID] == nil {
		v.perKey[keyID] = map[string]string{}
	}
	v.perKey[keyID][cmpwRefKey(r)] = "revoked"
}

// Check 实现 `compaction.ValidityChecker`。
// Check implements compaction.ValidityChecker.
func (v *cmpwValidity) Check(ctx context.Context, p core.Principal, r core.EvidenceRef) (compaction.Validity, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.observed = append(v.observed, cmpwValidityCall{Principal: p, Ref: r})
	key := cmpwRefKey(r)
	if reasons, ok := v.perKey[p.KeyID]; ok {
		if reason, hit := reasons[key]; hit {
			return compaction.Validity{Valid: false, Reason: reason}, nil
		}
	}
	if reason, hit := v.revoked[key]; hit {
		return compaction.Validity{Valid: false, Reason: reason}, nil
	}
	return compaction.Validity{Valid: true}, nil
}

// principalKeys 返回被观测到的身份 Key 集合（去重、保序）。
// principalKeys returns the observed key ids in first-seen order.
func (v *cmpwValidity) principalKeys() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	for _, call := range v.observed {
		if !slices.Contains(out, call.Principal.KeyID) {
			out = append(out, call.Principal.KeyID)
		}
	}
	return out
}

// cmpwRefKey 是引用在校验器内部的键（Kind 不参与语义）。
// cmpwRefKey is the internal lookup key of a reference.
func cmpwRefKey(r core.EvidenceRef) string { return r.ID + "@" + r.VersionID }

// —— 假工具与解析面 ——

// cmpwSourceTool 是 `source_view` 的替身：把入参里的来源引用原样作为 `Result.EvidenceRefs`
// 返回——这正是"工具结果的来源依赖必须随转述传播"（C-26）的输入端。
// cmpwSourceTool echoes its input reference back as the result's evidence, which is the
// input end of the provenance chain.
type cmpwSourceTool struct{}

// cmpwSourceArgs 是假工具的入参形状。
// cmpwSourceArgs is the fake tool's argument shape.
type cmpwSourceArgs struct {
	EvidenceID string `json:"evidence_id"`
	VersionID  string `json:"version_id"`
}

// Name 实现 `core.Tool`。
// Name implements core.Tool.
func (t *cmpwSourceTool) Name() string { return "source_view" }

// Description 实现 `core.Tool`。
// Description implements core.Tool.
func (t *cmpwSourceTool) Description() string { return "读取一条知识库来源的正文" }

// InputSchema 实现 `core.Tool`（必填字段参与入参校验，故与入参一一对应）。
// InputSchema implements core.Tool; the required fields match the arguments.
func (t *cmpwSourceTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","required":["evidence_id","version_id"]}`)
}

// Execute 实现 `core.Tool`。
// Execute implements core.Tool.
func (t *cmpwSourceTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	var args cmpwSourceArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return core.Result{Content: "入参非法"}, nil
	}
	r := core.EvidenceRef{Kind: "knowledge", ID: args.EvidenceID, VersionID: args.VersionID}
	return core.Result{
		Content:      fmt.Sprintf("来源 %s@%s 的正文", args.EvidenceID, args.VersionID),
		EvidenceRefs: []core.EvidenceRef{r},
	}, nil
}

// cmpwResolver 实现 `core.ToolResolver`：只有一件工具，规格按名字顺序取。
// cmpwResolver implements core.ToolResolver over a single tool.
type cmpwResolver struct{ tool core.Tool }

// Resolve 实现 `core.ToolResolver`。
// Resolve implements core.ToolResolver.
func (r *cmpwResolver) Resolve(name string) (core.Tool, bool) {
	if name == r.tool.Name() {
		return r.tool, true
	}
	return nil, false
}

// Specs 实现 `core.ToolResolver`。
// Specs implements core.ToolResolver.
func (r *cmpwResolver) Specs(names []string) []core.ToolSpec {
	var out []core.ToolSpec
	for _, name := range names {
		if name != r.tool.Name() {
			continue
		}
		out = append(out, core.ToolSpec{
			Name:        r.tool.Name(),
			Description: r.tool.Description(),
			Parameters:  r.tool.InputSchema(),
		})
	}
	return out
}

// cmpwInvocations 实现 `core.ToolInvocationStore`：审计可用（否则执行器 fail-closed 拒执行）。
// cmpwInvocations implements core.ToolInvocationStore so the executor may run.
type cmpwInvocations struct {
	mu  sync.Mutex
	seq int64
}

// BeginInvocation 实现 `core.ToolInvocationStore`。
// BeginInvocation implements core.ToolInvocationStore.
func (s *cmpwInvocations) BeginInvocation(ctx context.Context, inv core.ToolInvocation) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq, nil
}

// FinishInvocation 实现 `core.ToolInvocationStore`。
// FinishInvocation implements core.ToolInvocationStore.
func (s *cmpwInvocations) FinishInvocation(ctx context.Context, id int64, res core.ToolInvocationResult) error {
	return nil
}

// —— 假装配端口 ——

// cmpwAgents 实现 `core.AgentSource`（查找 / 目录 / 快照）。
// cmpwAgents implements core.AgentSource.
type cmpwAgents struct{ snapshot *core.ConfigSnapshot }

// Lookup 实现 `core.AgentSource`。
// Lookup implements core.AgentSource.
func (a *cmpwAgents) Lookup(name string) (core.AgentEntry, bool) {
	return core.AgentEntry{Name: name, Registered: true, Profile: a.snapshot.Profile}, true
}

// Dir 实现 `core.AgentSource`。
// Dir implements core.AgentSource.
func (a *cmpwAgents) Dir(name string) string { return "" }

// Snapshot 实现 `core.AgentSource`（接了 `RunSnapshots` 时不会被调用）。
// Snapshot implements core.AgentSource; it is unused once RunSnapshots is wired.
func (a *cmpwAgents) Snapshot(ctx context.Context, entry core.AgentEntry, now time.Time) (*core.ConfigSnapshot, error) {
	return a.snapshot, nil
}

// cmpwSnapshotBuilder 实现 `core.RunSnapshotBuilder`。
// cmpwSnapshotBuilder implements core.RunSnapshotBuilder.
type cmpwSnapshotBuilder struct{ snapshot *core.ConfigSnapshot }

// Build 实现 `core.RunSnapshotBuilder`。
// Build implements core.RunSnapshotBuilder.
func (b *cmpwSnapshotBuilder) Build(ctx context.Context, entry core.AgentEntry, resolved core.ResolvedResources) (core.ConfigSnapshot, error) {
	return *b.snapshot, nil
}

// cmpwResources 实现 `core.RunResources`：返回一份固定的发布版本集合。
// cmpwResources implements core.RunResources with one fixed published set.
type cmpwResources struct {
	runtime *cmpwRuntime
	summary core.ProviderClient
}

// Resolve 实现 `core.RunResources`。
// Resolve implements core.RunResources.
func (r *cmpwResources) Resolve(ctx context.Context, entry core.AgentEntry) (core.ResolvedResources, error) {
	return core.ResolvedResources{
		Provider: core.ResourceVersionRef{
			Name:        "prov-a",
			ResourceUID: cmpwResourceUID,
			VersionID:   3,
			VersionNo:   1,
			ContentHash: strings.Repeat("a", 64),
		},
		Runtime:       r.runtime,
		ProviderModel: "model-a",
		ProviderCredential: core.CredentialPin{
			Mode:          core.CredentialModeEnv,
			TargetBinding: strings.Repeat("b", 64),
		},
		Notify:          core.NotifyManifest{Mode: core.NotifyModeNone},
		SummaryProvider: r.summary,
	}, nil
}

// ResolvePinned 与 Resolve 同解：本文件的装配判据只用普通 Run 路径。
// ResolvePinned mirrors Resolve because the wiring judges here never pin a bundle.
func (r *cmpwResources) ResolvePinned(ctx context.Context, entry core.AgentEntry,
	_ string,
) (core.ResolvedResources, error) {
	return r.Resolve(ctx, entry)
}

// cmpwRuntime 实现 `core.RunRuntime`：模型映射表里只有一条目（不遍历、不推断）。
// cmpwRuntime implements core.RunRuntime with exactly one mapped provider.
type cmpwRuntime struct {
	client core.ProviderClient
	mu     sync.Mutex
	closed bool
}

// Provider 实现 `core.RunRuntime`。
// Provider implements core.RunRuntime.
func (r *cmpwRuntime) Provider(name string) (core.ProviderClient, bool) {
	if name != "prov-a" {
		return nil, false
	}
	return r.client, true
}

// MCP 实现 `core.RunRuntime`（本用例不接 MCP）。
// MCP implements core.RunRuntime; no MCP servers here.
func (r *cmpwRuntime) MCP(name string) (core.MCPHandle, bool) { return nil, false }

// ReleaseAdmission 实现 `core.RunRuntime`。
// ReleaseAdmission implements core.RunRuntime.
func (r *cmpwRuntime) ReleaseAdmission() {}

// Release 实现 `core.RunRuntime`。
// Release implements core.RunRuntime.
func (r *cmpwRuntime) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

// cmpwLifecycle 实现 `core.LifecyclePort`：只读面返回 active；写方法一律报"不支持"，
// 让"准入顺手写了状态文件"这类错误路径立刻可见。
// cmpwLifecycle implements core.LifecyclePort read-only; the write methods refuse loudly.
type cmpwLifecycle struct{}

// Read 实现 `core.LifecyclePort`。
// Read implements core.LifecyclePort.
func (l *cmpwLifecycle) Read(ctx context.Context, agent string) (core.LifecycleRecord, bool, error) {
	return core.LifecycleRecord{
		SchemaVersion: 2,
		Agent:         agent,
		AgentUID:      core.AgentUID(cmpwAgentUID),
		State:         core.AgentActive,
		Generation:    1,
		UpdatedAt:     cmpwClock().UnixMilli(),
		OperationID:   "cmpw-op",
		Checksum:      strings.Repeat("c", 64),
	}, true, nil
}

// Transition 实现 `core.LifecyclePort`（本用例不该走到）。
// Transition implements core.LifecyclePort and refuses, as this fixture never transitions.
func (l *cmpwLifecycle) Transition(ctx context.Context, agent string, expected core.AgentState, generation int64, target core.AgentState, operationID string) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errCmpwUnsupported
}

// Bootstrap 实现 `core.LifecyclePort`（本用例不该走到）。
// Bootstrap implements core.LifecyclePort and refuses.
func (l *cmpwLifecycle) Bootstrap(ctx context.Context, agent, operationID string) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errCmpwUnsupported
}

// BootstrapIdentity 实现 `core.LifecyclePort`（本用例不该走到）。
// BootstrapIdentity implements core.LifecyclePort and refuses.
func (l *cmpwLifecycle) BootstrapIdentity(ctx context.Context, agent string, uid core.AgentUID, operationID string) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errCmpwUnsupported
}

// Discard 实现 `core.LifecyclePort`（本用例不该走到）。
// Discard implements core.LifecyclePort and refuses.
func (l *cmpwLifecycle) Discard(ctx context.Context, agent, operationID string) error {
	return errCmpwUnsupported
}

// cmpwSessions 实现 `core.SessionStatusStore`：会话一律"不存在"，由 `UpsertSession` 建行。
// cmpwSessions implements core.SessionStatusStore: every session is absent until upserted.
type cmpwSessions struct{}

// GetSession 实现 `core.SessionStatusStore`。
// GetSession implements core.SessionStatusStore.
func (s *cmpwSessions) GetSession(ctx context.Context, id string) (core.SessionRecord, error) {
	return core.SessionRecord{}, fs.ErrNotExist
}

// ArchiveSession 实现 `core.SessionStatusStore`。
// ArchiveSession implements core.SessionStatusStore.
func (s *cmpwSessions) ArchiveSession(ctx context.Context, id string, at time.Time) error {
	return fs.ErrNotExist
}

// cmpwRunStore 实现 `core.RunStore`：只记账本写入，不判读面。
// cmpwRunStore implements core.RunStore for the write side only.
type cmpwRunStore struct {
	mu      sync.Mutex
	started []string
	status  map[string]core.RunStatus
}

// StartRun 实现 `core.RunStore`（本用例不持有租约，返回零值 token）。
// StartRun implements core.RunStore; this fixture holds no leases.
func (s *cmpwRunStore) StartRun(ctx context.Context, rec core.RunRecord, manifest core.RunResourceManifest,
	owner core.WorkerIdentity, admission core.CallerAdmission, runID string,
) (core.LeaseToken, error) {
	if !owner.IsZero() {
		return core.LeaseToken{}, core.ErrLeaseLost
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		s.status = map[string]core.RunStatus{}
	}
	s.started = append(s.started, rec.RunID)
	s.status[rec.RunID] = core.StatusRunning
	return core.LeaseToken{}, nil
}

// CheckAdmission 实现 `core.RunStore`（本用例不判准入）。
// CheckAdmission implements core.RunStore; this fixture never refuses.
func (s *cmpwRunStore) CheckAdmission(context.Context, string, core.Principal, core.CallerAdmission) error {
	return nil
}

// LookupAdmittedRun 实现 `core.RunStore`（本用例不会有未知提交结果）。
// LookupAdmittedRun implements core.RunStore; no unknown commit can occur here.
func (s *cmpwRunStore) LookupAdmittedRun(context.Context, string) (core.LeaseToken, error) {
	return core.LeaseToken{}, core.ErrRunNotFound
}

// RequestCancel 实现 `core.RunStore`（本用例不跨 Worker 取消）。
// RequestCancel implements core.RunStore; nothing to cancel here.
func (s *cmpwRunStore) RequestCancel(context.Context, string) (core.RunDetail, error) {
	return core.RunDetail{}, core.ErrRunNotFound
}

// FinishRun 实现 `core.RunStore`。
// FinishRun implements core.RunStore.
func (s *cmpwRunStore) FinishRun(ctx context.Context, runID string, token core.LeaseToken, status core.RunStatus, reason string, iterations, toolCalls int, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		s.status = map[string]core.RunStatus{}
	}
	s.status[runID] = status
	return true, nil
}

// CompleteRunCloseout 实现 `core.RunStore`（本用例不持有租约与引用）。
// CompleteRunCloseout implements core.RunStore; this fixture holds neither leases nor refs.
func (s *cmpwRunStore) CompleteRunCloseout(context.Context, string, core.LeaseToken, time.Time) error {
	return nil
}

// ReleaseRunRefs 实现 `core.RunStore`（本用例不持有引用）。
// ReleaseRunRefs implements core.RunStore; this fixture holds no references.
func (s *cmpwRunStore) ReleaseRunRefs(context.Context, string, time.Time) error {
	return nil
}

// GetRun 实现 `core.RunStore`（本用例不读账本）。
// GetRun implements core.RunStore; this fixture never reads it back.
func (s *cmpwRunStore) GetRun(ctx context.Context, runID string) (core.RunDetail, error) {
	return core.RunDetail{}, core.ErrRunNotFound
}

// ListRuns 实现 `core.RunStore`（本用例不读账本）。
// ListRuns implements core.RunStore; this fixture never reads it back.
func (s *cmpwRunStore) ListRuns(ctx context.Context, filter core.RunFilter) ([]core.RunDetail, error) {
	return nil, nil
}

// cmpwSnapshotStore 实现 `core.SnapshotStore`。
// cmpwSnapshotStore implements core.SnapshotStore.
type cmpwSnapshotStore struct{}

// SaveSnapshot 实现 `core.SnapshotStore`。
// SaveSnapshot implements core.SnapshotStore.
func (s *cmpwSnapshotStore) SaveSnapshot(ctx context.Context, snap *core.ConfigSnapshot) error {
	return nil
}

// cmpwCompactionStore 实现 `compaction.CompactionStore`：本用例的压缩器是录制用的假实现，
// 条目存储一次也不会被读到——它的存在只为让接线五项必填清单成立。
// cmpwCompactionStore satisfies the mandatory wiring list; the recording compactor never
// touches it.
type cmpwCompactionStore struct{}

// Append 实现 `compaction.CompactionStore`。
// Append implements compaction.CompactionStore.
func (s *cmpwCompactionStore) Append(ctx context.Context, entry compaction.CompactionEntry) (compaction.AppendResult, error) {
	return compaction.AppendResult{Entry: entry}, nil
}

// FindCommitted 实现 `compaction.CompactionStore`。
// FindCommitted implements compaction.CompactionStore.
func (s *cmpwCompactionStore) FindCommitted(ctx context.Context, scope compaction.Scope, coveredFromMessageID, cutMessageID string) (compaction.CompactionEntry, bool, error) {
	return compaction.CompactionEntry{}, false, nil
}

// LatestCommittedChain 实现 `compaction.CompactionStore`。
// LatestCommittedChain implements compaction.CompactionStore.
func (s *cmpwCompactionStore) LatestCommittedChain(ctx context.Context, scope compaction.Scope) ([]compaction.CompactionEntry, error) {
	return nil, nil
}

// MarkStale 实现 `compaction.CompactionStore`。
// MarkStale implements compaction.CompactionStore.
func (s *cmpwCompactionStore) MarkStale(ctx context.Context, compactionID, reason string) error {
	return nil
}

// cmpwAuditPort 实现 `compactionAuditPort`（摘要审计端口）：本用例的假压缩器不发摘要调用，
// 端口的存在只为让接线五项必填清单成立。
// cmpwAuditPort satisfies the mandatory wiring list; no summary call is ever emitted.
type cmpwAuditPort struct {
	mu  sync.Mutex
	seq int64
}

// BeginCompactionCall 实现 `compactionAuditPort`。
// BeginCompactionCall implements compactionAuditPort.
func (p *cmpwAuditPort) BeginCompactionCall(ctx context.Context, call core.LLMCall) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return p.seq, nil
}

// FinishCall 实现 `compactionAuditPort`。
// FinishCall implements compactionAuditPort.
func (p *cmpwAuditPort) FinishCall(ctx context.Context, id int64, result core.LLMCallResult) error {
	return nil
}

// cmpwQuotaPort 实现 `compactionQuotaPort`（摘要配额端口）。
// cmpwQuotaPort satisfies the mandatory quota port of the summary gate.
type cmpwQuotaPort struct{}

// AdmitCall 实现 `compactionQuotaPort`。
// AdmitCall implements compactionQuotaPort.
func (p *cmpwQuotaPort) AdmitCall(ctx context.Context, principal core.Principal, class callers.RequestClass, cost callers.Cost, idem string) (callers.Ticket, error) {
	return callers.Ticket{ReservationID: "cmpw-reservation/" + idem}, nil
}

// Commit 实现 `compactionQuotaPort`。
// Commit implements compactionQuotaPort.
func (p *cmpwQuotaPort) Commit(ctx context.Context, ticket callers.Ticket, usage callers.Usage) (callers.Settlement, error) {
	return callers.Settlement{}, nil
}

// Release 实现 `compactionQuotaPort`。
// Release implements compactionQuotaPort.
func (p *cmpwQuotaPort) Release(ctx context.Context, ticket callers.Ticket, reason callers.ReleaseReason) error {
	return nil
}

// —— 用例 ——

// TestCompactionWiring_StatelessRunReadsRunHistory 守"无状态 Run 的压缩输入取自本次 Run 的
// 内存历史"：`ListBySession` 对无状态范围只能查到空集，历史必须由循环经 `CompactRequest.History`
// 显式交进去。
//
// 逆改点：循环没把 `History` 填进 `CompactRequest` → 无状态 Run 压缩直接 `ErrScopeInvalid`
// 或读到空集（`history.nilPort` 为真 / `ids` 为空）。
func TestCompactionWiring_StatelessRunReadsRunHistory(t *testing.T) {
	h := newWiringHarness(t)
	h.session = ""
	h.runID = "r-stateless"
	h.model(step("好的"))

	require.NoError(t, h.run(t))

	req := h.recorder.lastRequest(t)
	require.Empty(t, req.SessionID, "无状态 Run 的范围标识必须是空会话")
	require.Equal(t, "r-stateless", req.RunID, "压缩请求必须指向本次 Run")
	view := h.recorder.lastHistory(t)
	require.False(t, view.nilPort, "历史端口**必填**：缺失即 ErrScopeInvalid，不得留 nil 让引擎猜")
	require.Equal(t, []string{core.StatelessMessageID("r-stateless", 1)}, view.ids,
		"无状态范围的历史是本次 Run 的内存消息（不发一条库查询）")
	require.Equal(t, 1, h.recorder.invocations(), "脚本只有一轮，压缩恰好判一次")
}

// TestCompactionWiring_BuildUsesBudgetSnapshot 守"组装与压缩共用同一份预算 / 固定开销口径"：
// 循环必须把 `PromptBuilder` 现算的 `FixedParts` 与配置里的输出预留一并交进 `CompactRequest`，
// 而不是拿协议余量顶替、也不是留零值。
//
// 逆改点：`PromptBuilder` 丢掉 `FixedParts`（`Fixed.Total == 0`，真实引擎会拒绝）/
// 用 `ReserveTokens` 当输出预留（`Budget.OutputReserve` 变成 1024）→ 两条判定口径分叉。
func TestCompactionWiring_BuildUsesBudgetSnapshot(t *testing.T) {
	h := newWiringHarness(t)
	h.budget = core.Budget{ContextTokens: 4096}
	h.model(step("好的"))

	require.NoError(t, h.run(t))

	req := h.recorder.lastRequest(t)
	require.Equal(t, 4096, req.Budget.ContextTokens, "预算取请求值（Profile 只收不迭代 / 上下文上限）")
	require.Equal(t, core.ReserveTokens, req.Budget.ProtocolReserve, "协议余量是常量，不是配置键")
	require.Equal(t, cmpwOutputReserve, req.Budget.OutputReserve,
		"输出预留只来自 compaction.output_reserve_tokens")
	require.NotEqual(t, core.ReserveTokens, req.Budget.OutputReserve,
		"不得用协议余量顶替输出预留（两者相等时该逆改点会静默通过）")
	require.Equal(t, core.EstimateVersion, req.Budget.EstimatorVersion, "估算器版本必须随预算一并交底")
	require.Equal(t, req.Budget.EstimatorVersion, req.EstimatorVersion, "两处版本不一致即接线错误")

	// 固定开销由 PromptBuilder 现算：段一非空 + 工具集非空 ⇒ 两项都为正；无记忆时段二为零，
	// 于是 Total 与 System+Tools 精确相等（分隔符不产生字符）。
	require.Positive(t, req.Fixed.System, "段一正文是快照冻结的，估算必须为正")
	require.Positive(t, req.Fixed.Tools, "工具定义含每个工具的包裹开销")
	require.Zero(t, req.Fixed.Memory, "本次运行没有记忆，段二不产生任何字符")
	require.Positive(t, req.Fixed.Total, "零值 = 未接线，真实引擎必须拒绝")
	require.Equal(t, req.Fixed.System+req.Fixed.Tools+req.Fixed.Memory, req.Fixed.Total,
		"Total 是各段之和（无记忆时分隔符不加字符）")

	require.Equal(t, h.principal, req.Principal, "压缩请求携带本次 Run 准入时冻结的身份")
	require.True(t, h.recorder.assembledWithSummary(),
		"摘要客户端必须从准入固定的映射表接上——接不上就不会压缩")
}

// TestCompactionWiring_AssemblyRecordCarriesCompressedVerdict 守"真压缩接线下装配记录
// 如实携带压缩结论"：摘要可注入时记录必须写 `decision=compressed` 与条目引用字段——
// 只写 `enabled` 不写引用，等于把"按 `compaction_id` 回查条目"的诊断路径断在记录层。
//
// 逆改点：循环在 `record.Compression` 并入之前交出记录 → `decision` 退回 `none` 且
// `compaction_id` 为空；接线层漏填条目字段（只填 decision）→ 引用断言红。
func TestCompactionWiring_AssemblyRecordCarriesCompressedVerdict(t *testing.T) {
	h := newWiringHarness(t)
	h.seedSummary(core.CompactionSummary{
		CompactionID:         "c-a",
		ChainEpoch:           1,
		CoveredFromMessageID: "m1",
		CutMessageID:         "m4",
		FirstKeptMessageID:   "m5",
		Text:                 "早前对话摘要",
		Validity:             core.SummaryValid,
		StrategyVersion:      "cmpw-strategy-v1",
		PromptVersion:        "cmpw-prompt-v1",
	})
	h.model(step("好的"))

	require.NoError(t, h.run(t))

	records := h.assembly.snapshot()
	require.Len(t, records, 1, "一轮对话 = 一条装配记录")
	record := records[0]
	require.Equal(t, core.PurposeChat, record.Purpose)
	require.Equal(t, h.agent, record.Agent, "Agent 名取自冻结快照")
	require.Equal(t, "model-a", record.ModelVersion, "模型取快照里的 Provider 绑定")
	require.True(t, record.Compression.Enabled, "压缩已装配：记录必须如实标 enabled")
	require.Equal(t, core.DecisionCompressed, record.Compression.Decision)
	require.Equal(t, "c-a", record.Compression.CompactionID, "记录按 compaction_id 可回查条目")
	require.Equal(t, "m1", record.Compression.CoveredFromMessageID)
	require.Equal(t, "m4", record.Compression.CutMessageID)
	require.Equal(t, "m5", record.Compression.FirstKeptMessageID)
	require.Equal(t, "cmpw-strategy-v1", record.Compression.StrategyVersion)
	require.Equal(t, "cmpw-prompt-v1", record.Compression.PromptVersion)

	segments := make([]string, 0, len(record.FinalSegments))
	for _, segment := range record.FinalSegments {
		segments = append(segments, segment.ID)
	}
	require.Contains(t, segments, "summary", "注入的摘要段必须出现在本轮装配记录里")
}

// TestCompactionWiring_AssemblyRecordCarriesDegradedVerdict 守"降级原因码随记录落账"：
// 降级不是失败（Run 照常完成），但原因码必须进装配记录——只留在日志里，事后就查不出
// "这一轮为什么没注入摘要"。
//
// 逆改点：循环把降级当失败（Run 不 completed）/ 降级分支不填 ReasonCode → 断言红。
func TestCompactionWiring_AssemblyRecordCarriesDegradedVerdict(t *testing.T) {
	h := newWiringHarness(t)
	h.seedDegraded("compaction_timeout")
	h.model(step("好的"))

	require.NoError(t, h.run(t))

	records := h.assembly.snapshot()
	require.Len(t, records, 1)
	require.True(t, records[0].Compression.Enabled)
	require.Equal(t, core.DecisionDegraded, records[0].Compression.Decision)
	require.Equal(t, "compaction_timeout", records[0].Compression.ReasonCode)
	require.Empty(t, records[0].Compression.CompactionID, "降级没有条目可引用")
}

// TestCompactionWiring_FallbackFiltersRevokedSources 守"降级回退的原始历史走同一个来源闸门"：
// 压缩不可用不等于来源规则可以失效。
//
// 逆改点：`SourceFilter` 未接线或只作用于摘要路径 → 降级后 prompt 里出现已撤权原文。
func TestCompactionWiring_FallbackFiltersRevokedSources(t *testing.T) {
	h := newWiringHarness(t)
	control := h.messages.seedAssistant(t, "早前引用 B 的回复", ref("kb-b", "v1"))
	h.messages.seedSession(t, h.session)
	h.validity.revoke(ref("kb-b", "v1"))
	h.seedDegraded("summary_unavailable")
	h.model(step("好的"))

	require.NoError(t, h.run(t))

	require.NotNil(t, h.filter, "来源闸门必须在装配时就接上（降级路径共用它）")
	prompt := h.lastPrompt(t)
	require.NotContains(t, prompt.text(), "早前引用 B 的回复",
		"降级回退的原始历史同样不得把失效来源原文送进模型输入")
	require.Contains(t, prompt.text(), cmpwPlaceholder, "失效消息整条换成占位符，位置保留")
	require.Equal(t, "早前引用 B 的回复", h.messages.contentOf(t, control),
		"过滤只作用于注入视图，仓储正文保持不变")
}

// TestCompactionWiring_FilterUsesRunPrincipal 守"过滤器拿到的是本次 Run 的身份"：
// 同一调用方的两把 Key 权限不同时，受限 Key **不得**看到另一把 Key 才能读的正文。
//
// 逆改点：`Filter` 不接收 `Principal`（改从 ctx / 单例取）→ 同一调用方两把不同 scope 的 Key
// 得到同一份 prompt，受限 Key 看到无权限正文。
func TestCompactionWiring_FilterUsesRunPrincipal(t *testing.T) {
	h := newWiringHarness(t)
	// 两个会话各备一份**同一来源**的历史：唯一区别是跑它们的那把 Key。
	h.messages.seedAssistantIn(t, "sess-full", "只有 key-1 能读的正文", ref("kb-a", "v1"))
	h.messages.seedAssistantIn(t, "sess-limited", "只有 key-1 能读的正文", ref("kb-a", "v1"))
	h.validity.denyForKey("key-2", ref("kb-a", "v1"))

	// 两次运行串行跑，脚本按会话分别推进。
	h.modelFor("sess-full", step("好的"))
	h.modelFor("sess-limited", step("好的"))
	require.NoError(t, h.runFor(t, "sess-full", cmpwPrincipal("caller-1", "key-1"), "查 A 文档"))
	require.NoError(t, h.runFor(t, "sess-limited", cmpwPrincipal("caller-1", "key-2"), "查 A 文档"))

	full := h.chat.promptTextFor("sess-full")
	limited := h.chat.promptTextFor("sess-limited")
	require.Contains(t, full, "只有 key-1 能读的正文", "持有该来源权限的 Key 正常读到正文")
	require.NotContains(t, limited, "只有 key-1 能读的正文",
		"受限 Key 不得读到另一把 Key 才有权读的正文")
	require.Contains(t, limited, cmpwPlaceholder, "受限判定同样以占位符替换，不删除消息")

	require.ElementsMatch(t, []string{"key-1", "key-2"}, h.validity.principalKeys(),
		"校验器每次拿到的都必须是**本次 Run** 的身份，不得共用一份")
}

// TestCompactionWiring_FilterRejectsZeroPrincipal 守"零值身份即拒绝"：过滤器**不得**把
// 缺失身份当成管理员放行，运行入口也必须在任何副作用之前拒绝。
//
// 逆改点：零值 Principal 被放行或回退成管理员 → 过滤器按全权限校验，未授权内容进入 prompt。
func TestCompactionWiring_FilterRejectsZeroPrincipal(t *testing.T) {
	h := newWiringHarness(t)
	h.messages.seedAssistant(t, "受保护正文", ref("kb-a", "v1"))
	h.messages.seedSession(t, h.session)

	kept, err := h.filter.Filter(context.Background(), core.Principal{}, h.messages.all(t))
	require.ErrorIs(t, err, core.ErrUnauthorized, "零值身份一律拒绝，不得补默认值后继续")
	require.Nil(t, kept, "拒绝时不得返回任何正文")

	// 运行入口侧：同一份非法身份在任何副作用之前被挡下——不发模型调用、不写消息。
	h.model(step("好的"))
	_, err = h.svc.Run(context.Background(), core.RunRequest{
		Principal: core.Principal{},
		Agent:     h.agent,
		SessionID: h.session,
		Message:   "查 A 文档",
		Channel:   "cli",
	})
	require.ErrorIs(t, err, core.ErrUnauthorized, "运行入口的身份门禁先于一切副作用")
	require.Zero(t, h.chat.calls(), "非法身份不得发出任何模型调用")
	require.Zero(t, h.recorder.invocations(), "非法身份不得进入循环，压缩判定一次都不该发生")
}

// TestCompactionWiring_ConcurrentRunsKeepPrincipal 守"身份随 Run 冻结、不落到包级状态"：
// 两个并发 Run 各自的身份只影响自己的注入视图。
//
// 逆改点：身份放进包级变量 / 闭包捕获 → 并发下 A 的 Run 用 B 的 Principal 校验
// （`-race` 或交叉断言红）。
func TestCompactionWiring_ConcurrentRunsKeepPrincipal(t *testing.T) {
	h := newWiringHarness(t)
	// 两个会话各自的历史：甲依赖 kb-a、乙依赖 kb-b。
	h.messages.seedAssistantIn(t, "sess-A", "甲 的回复", ref("kb-a", "v1"))
	h.messages.seedAssistantIn(t, "sess-B", "乙 的回复", ref("kb-b", "v1"))
	// key-A 只看得到 kb-a，key-B 只看得到 kb-b。
	h.validity.denyForKey("key-A", ref("kb-b", "v1"))
	h.validity.denyForKey("key-B", ref("kb-a", "v1"))

	// 会话缺省脚本对两个会话都生效（各自独立推进）。
	h.model(step("好的"))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, run := range []struct {
		session string
		keyID   string
	}{
		{session: "sess-A", keyID: "key-A"},
		{session: "sess-B", keyID: "key-B"},
	} {
		wg.Add(1)
		go func(idx int, session, keyID string) {
			defer wg.Done()
			_, errs[idx] = h.svc.Run(context.Background(), core.RunRequest{
				Principal: cmpwPrincipal("caller-1", keyID),
				Agent:     h.agent,
				SessionID: session,
				Message:   "查资料",
				Channel:   "cli",
			})
		}(i, run.session, run.keyID)
	}
	wg.Wait()
	for i := range errs {
		require.NoError(t, errs[i], "并发运行不得互相干扰")
	}

	// 两个会话各自只含自己那条历史，故唯一能区分身份的观测是"自己那条有没有被保住"：
	// 身份被串到另一把 Key 上，甲（key-B 视角）或乙（key-A 视角）就会被换成占位符。
	textA := h.chat.promptTextFor("sess-A")
	textB := h.chat.promptTextFor("sess-B")
	require.Contains(t, textA, "甲 的回复", "sess-A 的 Run 必须用 key-A 校验，kb-a 可见")
	require.Contains(t, textB, "乙 的回复", "sess-B 的 Run 必须用 key-B 校验，kb-b 可见")
	require.NotContains(t, textA, cmpwPlaceholder, "身份被串走时甲会整条换成占位符")
	require.NotContains(t, textB, cmpwPlaceholder, "身份被串走时乙会整条换成占位符")

	require.ElementsMatch(t, []string{"key-A", "key-B"}, h.validity.principalKeys(),
		"两个并发 Run 各自用自己的身份校验（包级状态会让其中一把 Key 消失）")
}

// TestCompactionWiring_AssistantDerivedRefsPersisted 守"assistant 转述继承本次模型输入的
// 来源集合，并随消息同事务落库"（颗粒度文档 R21 逐字）。
//
// 逆改点：assistant 消息只拷工具引用或不落 `message_evidence_refs` → `refsOf(assistant)`
// 为空，撤权后转述漏过滤、压缩引用集合偏窄；另起事务补写 → `appendCallsFor` > 1。
func TestCompactionWiring_AssistantDerivedRefsPersisted(t *testing.T) {
	h := newWiringHarness(t)
	h.model(step("查 A 文档"), callTool("source_view", ref("kb-a", "v1")), step("A 的内容是……"))
	require.NoError(t, h.run(t))

	assistant := h.messages.lastWithRoleT(t, "assistant")
	require.ElementsMatch(t, []core.EvidenceRef{ref("kb-a", "v1")}, h.store.refsOf(t, assistant.ID),
		"assistant 转述必须带上本次模型输入实际包含的来源，并随消息同事务落库")
	require.Equal(t, 1, h.store.appendCallsFor(t, assistant.ID),
		"assistant 消息仍走唯一追加路径（原则 II：不得出现第二套追加）")
}

// TestCompactionWiring_RevokedSourceNotSentAsToolOrParaphrase 守"撤权后工具原文与 assistant
// 转述同时失效"：只过滤 `tool` 消息等于放行转述，转述会把已撤权内容带进下一次 prompt。
//
// 逆改点：只过滤 `tool` 消息 → 撤权后 assistant 转述仍原样进入下一次 prompt 与摘要请求。
func TestCompactionWiring_RevokedSourceNotSentAsToolOrParaphrase(t *testing.T) {
	h := newWiringHarness(t)
	h.model(
		step("查 B 文档"), callTool("source_view", ref("kb-b", "v1")), step("B 的回复转述"),
		step("好的"),
	)
	require.NoError(t, h.run(t))
	require.Contains(t, h.chat.promptTextFor(h.session), "来源 kb-b@v1 的正文",
		"前提：撤权前工具原文确实进过一次 prompt")
	paraphrase := h.messages.lastWithRoleT(t, "assistant")
	require.Equal(t, "B 的回复转述", paraphrase.Content, "前提：第一轮以带来源依赖的转述收口")

	h.validity.revoke(ref("kb-b", "v1"))
	require.NoError(t, h.run(t))

	second := h.chat.promptTextFor(h.session)
	require.NotContains(t, second, "来源 kb-b@v1 的正文", "工具原文不得再次进入 prompt")
	require.NotContains(t, second, "B 的回复转述",
		"assistant 转述依赖同一来源，撤权后同样不得进入 prompt")
	require.Contains(t, second, cmpwPlaceholder, "两条都整条换成占位符（不删除、不改写存储）")
	require.Equal(t, "B 的回复转述", h.messages.contentOf(t, paraphrase.ID),
		"过滤只作用于注入视图：第一轮的转述在仓储里逐字保留")
}

// TestCompactionWiring_AuditRefsDoNotPropagateToAssistant 守"摘要审计集不传播成 assistant 的
// 当前依赖"（颗粒度文档 R23 逐字）：审计集里的已撤权引用一旦随回复落库，下一轮过滤器会把
// 这条**本身安全**的回复整条换掉。
//
// 逆改点：`PromptBuilder` 把摘要 `SourceRefs` 抄进 `Prompt.EvidenceRefs` → assistant 引用
// 集合里出现已撤权的 kb-b，下一轮 `SourceFilter` 把这条安全回复整条换成占位符；
// 或过滤器把失效消息**删掉**而不是换成占位符 → `byID` 取不到该条；
// 或过滤顺手改写仓储正文 → `contentOf` 对照失败。
func TestCompactionWiring_AuditRefsDoNotPropagateToAssistant(t *testing.T) {
	h := newWiringHarness(t)
	h.principal = cmpwPrincipal("caller-1", "key-1")
	// 摘要：审计集 {kb-a, kb-b}，当前依赖仅 {kb-a}；kb-b 已撤权。
	h.seedSummary(core.CompactionSummary{
		CompactionID: "c-a", ChainEpoch: 1, Text: "早前对话摘要",
		SourceRefs:   []core.EvidenceRef{ref("kb-a", "v1"), ref("kb-b", "v1")},
		IncludedRefs: []core.EvidenceRef{ref("kb-a", "v1")},
		Validity:     core.SummaryValid,
	})
	// 正对照：历史里另有一条**确实依赖** kb-b 的转述，它必须被替换，证明过滤器本身在工作。
	control := h.messages.seedAssistant(t, "早前引用 B 的回复", ref("kb-b", "v1"))
	h.messages.seedSession(t, h.session)
	h.validity.revoke(ref("kb-b", "v1"))
	h.model(step("A 的内容是……"))

	require.NoError(t, h.run(t))

	require.ElementsMatch(t, []core.EvidenceRef{ref("kb-a", "v1")}, h.lastPrompt(t).EvidenceRefs,
		"本次模型输入的来源集合只算实际注入的 IncludedRefs，审计集不入列")
	assistant := h.messages.lastWithRoleT(t, "assistant")
	require.ElementsMatch(t, []core.EvidenceRef{ref("kb-a", "v1")}, h.store.refsOf(t, assistant.ID),
		"落库的回复依赖同样不含审计集里的失效项")

	// 下一轮：同一份消息历史过 SourceFilter（Build → assistant 落库 → 过滤，三步走通）。
	kept, err := h.filter.Filter(context.Background(), h.principal, h.messages.all(t))
	require.NoError(t, err)

	require.Equal(t, assistant.Content, byID(t, kept, assistant.ID).Content,
		"安全回复正文保留——不得因审计集里的旧引用被换掉")
	keptControl := byID(t, kept, control)
	require.Equal(t, cmpwPlaceholder, keptControl.Content,
		"正对照：真正依赖 kb-b 的转述整条换成占位符（位置与 ID 保留）")
	require.NotContains(t, keptControl.Content, "早前引用 B 的回复", "失效来源的原文不得出现在过滤结果里")
	require.Equal(t, "早前引用 B 的回复", h.messages.contentOf(t, control),
		"过滤只作用于注入视图，仓储正文保持不变")
}

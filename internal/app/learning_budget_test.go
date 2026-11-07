package app

// 本文件是 US-22 T018 的**装配层**验收面（无库）：判两件事——
//
//  1. **两级预写都先于发出**：抽取账本行（`extraction-pending`）和模型审计行
//     （`llm-pending`）都必须在模型调用之前落库；任一预写失败时调用**不得**发出，
//     且钩子按 `audit` 类别写失败终态（颗粒度文档 C-07 / FR-22-05、宪法 V）。
//  2. **钩子有界且同步**：模型耗尽工作段后，钩子在总预算内返回并发完失败终态；
//     整个过程不起 goroutine（返回时失败行已落库，协程数回到基线）。
//
// 用到的都是本地协议桩与记账桩；没有真实模型、没有数据库。
// Unit-level audit and boundedness cases for the extraction hook: both pre-writes precede
// the call, a failed pre-write suppresses the call, and the hook returns synchronously
// inside its total budget without leaking goroutines.

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/provider"
)

// us22BudgetEvents 是跨桩共享的事件序列：用**单一时间线**给"谁先谁后"判卷。
// us22BudgetEvents is the shared timeline used by every stub in this file.
type us22BudgetEvents struct {
	list []string
}

func (e *us22BudgetEvents) add(name string) { e.list = append(e.list, name) }

func (e *us22BudgetEvents) snapshot() []string { return append([]string(nil), e.list...) }

// us22ExtractStore 是 LearningStore 的记账桩：预写置 pending，失败 / 提交各记事件。
// us22ExtractStore records the ledger pre-write, failure and commit on one timeline.
type us22ExtractStore struct {
	events   *us22BudgetEvents
	beginErr error

	pending bool

	beginCalls   int
	failCalls    int
	commitCalls  int
	failClass    core.LearningFailureClass
	failCtxErr   error
	failDeadline time.Time
}

func (s *us22ExtractStore) UnresolvedForOrigin(context.Context, string, string, string) (*core.LearningExtraction, error) {
	return nil, nil
}

func (s *us22ExtractStore) LoadCursor(context.Context, string, string, string) (core.LearningCursor, bool, error) {
	return core.LearningCursor{}, false, nil
}

func (s *us22ExtractStore) EntriesForExtraction(context.Context, core.Scope, int, int) ([]memory.LearningEntrySnapshot, error) {
	return nil, nil
}

func (s *us22ExtractStore) BeginExtraction(_ context.Context, row core.LearningExtraction, _ core.LeaseToken) (core.LearningExtraction, error) {
	s.beginCalls++
	if s.events != nil {
		s.events.add("extraction-pending")
	}
	if s.beginErr != nil {
		return core.LearningExtraction{}, s.beginErr
	}
	s.pending = true
	row.Attempts++
	return row, nil
}

func (s *us22ExtractStore) FailExtraction(ctx context.Context, _, _ string, _ core.LeaseToken,
	_ string, _ int, class core.LearningFailureClass, _ string, _ time.Time,
) error {
	s.failCalls++
	s.failClass = class
	s.failCtxErr = ctx.Err()
	s.failDeadline, _ = ctx.Deadline()
	if s.events != nil {
		s.events.add("failed")
	}
	return nil
}

func (s *us22ExtractStore) CommitExtraction(context.Context, core.CommitLearningExtraction, core.LeaseToken) (core.LearningCommitResult, error) {
	s.commitCalls++
	if s.events != nil {
		s.events.add("commit")
	}
	return core.LearningCommitResult{}, nil
}

// us22BudgetMessages 返回固定的两消息窗口（同来源 Run），足够触发一次真实抽取路径。
// us22BudgetMessages returns a fixed two-message window from one origin run.
type us22BudgetMessages struct{}

func (m *us22BudgetMessages) MessagesForExtraction(context.Context, string, string, int64, int, int, int, int) (core.LearningWindow, error) {
	return us22BudgetWindow(), nil
}

func (m *us22BudgetMessages) MessagesForFrozenWindow(context.Context, string, string, string, int64, int64, int, int) (core.LearningWindow, error) {
	return us22BudgetWindow(), nil
}

func us22BudgetWindow() core.LearningWindow {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	return core.LearningWindow{
		New: []core.LearningMessage{
			{MessageID: 11, RunID: "run-1", Role: "user", Content: "我偏好用中文写日报", CreatedAt: at},
			{MessageID: 12, RunID: "run-1", Role: "assistant", Content: "好的。", CreatedAt: at.Add(time.Second)},
		},
		FromMessageID: 11,
		ToMessageID:   12,
	}
}

// us22BudgetExtractor 是抽取面桩：记录调用时账本行是否已 pending，并可阻塞到截止。
// us22BudgetExtractor records whether the ledger row was pending when the call was made.
type us22BudgetExtractor struct {
	store  *us22ExtractStore
	events *us22BudgetEvents

	reply string
	err   error
	block bool

	calls         int
	pendingAtCall bool
}

func (e *us22BudgetExtractor) ChatMemoryExtract(ctx context.Context, _ core.ChatRequest) (core.ChatResponse, error) {
	e.calls++
	e.pendingAtCall = e.store.pending
	if e.events != nil {
		e.events.add("extraction-call")
	}
	if e.err != nil {
		return core.ChatResponse{}, e.err
	}
	if e.block {
		<-ctx.Done()
		return core.ChatResponse{}, ctx.Err()
	}
	return core.ChatResponse{Content: e.reply}, nil
}

// us22OrderAuditStore 是同时覆盖聊天 / 抽取两条审计面的有序桩；预写失败可按入口预设。
// us22OrderAuditStore orders both audit surfaces on the shared timeline.
type us22OrderAuditStore struct {
	events          *us22BudgetEvents
	beginExtractErr error

	chatBegins    int
	extractBegins int
	finishCalls   int
	lastExtract   core.LLMCall
}

func (s *us22OrderAuditStore) BeginCall(context.Context, core.LLMCall) (int64, error) {
	s.chatBegins++
	if s.events != nil {
		s.events.add("chat-pending")
	}
	return 1, nil
}

func (s *us22OrderAuditStore) BeginMemoryExtractCall(_ context.Context, call core.LLMCall) (int64, error) {
	s.extractBegins++
	s.lastExtract = call
	if s.events != nil {
		s.events.add("llm-pending")
	}
	if s.beginExtractErr != nil {
		return 0, s.beginExtractErr
	}
	return 2, nil
}

func (s *us22OrderAuditStore) FinishCall(context.Context, int64, core.LLMCallResult) error {
	s.finishCalls++
	if s.events != nil {
		s.events.add("llm-finish")
	}
	return nil
}

// us22OrderClient 是协议桩：调用记入同一时间线。
// us22OrderClient records the protocol call on the shared timeline.
type us22OrderClient struct {
	events *us22BudgetEvents
	resp   core.ChatResponse
	calls  int
}

func (c *us22OrderClient) Chat(context.Context, core.ChatRequest) (core.ChatResponse, error) {
	c.calls++
	if c.events != nil {
		c.events.add("model-call")
	}
	return c.resp, nil
}

// us22BudgetInput 构造一次 web 渠道的完成 Run。
// us22BudgetInput builds one completed web run.
func us22BudgetInput(extractor core.MemoryExtractor) core.LearningRunInput {
	return core.LearningRunInput{
		RunID:       "run-1",
		Agent:       "demo",
		SessionID:   "sess-1",
		Channel:     "web",
		Provider:    "prov",
		Model:       "model",
		ConfigHash:  "cfg-1",
		CompletedAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		Extractor:   extractor,
	}
}

// us22BudgetService 用记账桩装配抽取服务；上限取缺省值（除非调用方另给）。
// us22BudgetService assembles the extraction service over the recording stubs.
func us22BudgetService(t *testing.T, store memory.LearningStore, limits memory.LearningLimits) *memory.LearningService {
	t.Helper()
	return memory.NewLearningService(memory.LearningDeps{
		Store:     store,
		Messages:  &us22BudgetMessages{},
		Enabled:   true,
		Agents:    []string{"demo"},
		DraftRoot: filepath.Join(t.TempDir(), "drafts"),
		Limits:    limits,
		Logger:    us22DiscardLogger(),
		Now:       func() time.Time { return time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC) },
	})
}

// TestUS22_Audit_ExtractionPendingBeforeCall 守 C-07 / FR-22-05：模型调用发出时，抽取
// 账本行必须已经是 pending；事件序列是「账本预写 → 调用 → 提交」。
// 逆改点：把 `BeginExtraction` 挪到调用之后 → `pendingAtCall` 为假且事件顺序颠倒。
// The ledger row must be pending before the call; the timeline pins the order.
func TestUS22_Audit_ExtractionPendingBeforeCall(t *testing.T) {
	events := &us22BudgetEvents{}
	store := &us22ExtractStore{events: events}
	extractor := &us22BudgetExtractor{store: store, events: events, reply: `{"items":[]}`}
	service := us22BudgetService(t, store, memory.LearningLimits{})

	service.AfterRun(context.Background(), us22BudgetInput(extractor),
		core.RunResult{RunID: "run-1", Status: core.StatusCompleted})

	require.Equal(t, 1, extractor.calls)
	require.True(t, extractor.pendingAtCall, "模型调用发出时抽取账本行必须已经是 pending")
	require.Equal(t, []string{"extraction-pending", "extraction-call", "commit"}, events.snapshot())
	require.Equal(t, 1, store.beginCalls)
	require.Equal(t, 1, store.commitCalls)
}

// TestUS22_Audit_LLMPendingBeforeCall 守 C-07 / FR-22-05：模型审计的 pending 行必须先于
// 唯一一次协议调用落库，终态回填在调用之后；抽取入口不得退回聊天预写。
// 逆改点：把 `BeginMemoryExtractCall` 挪到 `client.Chat` 之后 → 事件顺序颠倒而红。
// The memory_extract audit row must be pending before the single protocol call.
func TestUS22_Audit_LLMPendingBeforeCall(t *testing.T) {
	events := &us22BudgetEvents{}
	audit := &us22OrderAuditStore{events: events}
	wire := &us22OrderClient{events: events, resp: core.ChatResponse{Content: "ok"}}
	service := provider.NewProviderService(
		map[string]core.ProviderClient{"prov": wire}, map[string]string{"prov": "model"},
		audit, us22DiscardLogger()).WithMemoryExtractStore(audit)

	resp, err := service.ChatMemoryExtract(context.Background(), core.ChatRequest{
		Provider: "prov", RunID: "run-1", Round: 0,
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Content)
	require.Equal(t, []string{"llm-pending", "model-call", "llm-finish"}, events.snapshot())
	require.Equal(t, 1, wire.calls)
	require.Equal(t, 1, audit.extractBegins)
	require.Zero(t, audit.chatBegins, "抽取预写不得落到聊天入口")
	require.Equal(t, 0, audit.lastExtract.Round, "抽取调用的审计轮次固定为 0")
	require.Equal(t, "run-1", audit.lastExtract.RunID)
}

// TestUS22_Audit_PreWriteFailureNoCall 守 C-07 / FR-22-05 / 宪法 V fail-closed：
// ① 审计预写失败时 `ChatMemoryExtract` 返回 ErrAuditWrite，协议调用零次、终态零次；
// ② 组合到钩子时，整次抽取按 `audit` 类别写失败终态，不提交、不发调用。
// 逆改点：预写失败后仍发出调用 → 协议桩计数非零；钩子把 ErrAuditWrite 归到 model_error
// → 失败类别断言红。
// A failed pre-write suppresses the call; the hook records an audit-class failure.
func TestUS22_Audit_PreWriteFailureNoCall(t *testing.T) {
	t.Run("provider-refuses-without-call", func(t *testing.T) {
		events := &us22BudgetEvents{}
		audit := &us22OrderAuditStore{events: events, beginExtractErr: errors.New("audit unavailable")}
		wire := &us22OrderClient{events: events, resp: core.ChatResponse{Content: "ok"}}
		service := provider.NewProviderService(
			map[string]core.ProviderClient{"prov": wire}, map[string]string{"prov": "model"},
			audit, us22DiscardLogger()).WithMemoryExtractStore(audit)

		_, err := service.ChatMemoryExtract(context.Background(), core.ChatRequest{
			Provider: "prov", RunID: "run-1", Round: 0,
		})
		require.ErrorIs(t, err, core.ErrAuditWrite)
		require.Equal(t, []string{"llm-pending"}, events.snapshot(),
			"预写失败后不得出现模型调用或终态回填")
		require.Zero(t, wire.calls, "预写失败必须拒绝发出调用")
		require.Zero(t, audit.finishCalls)
		require.Zero(t, audit.chatBegins)
	})

	t.Run("hook-records-audit-failure", func(t *testing.T) {
		events := &us22BudgetEvents{}
		audit := &us22OrderAuditStore{events: events, beginExtractErr: errors.New("audit unavailable")}
		wire := &us22OrderClient{events: events, resp: core.ChatResponse{Content: "ok"}}
		extractor := provider.NewProviderService(
			map[string]core.ProviderClient{"prov": wire}, map[string]string{"prov": "model"},
			audit, us22DiscardLogger()).WithMemoryExtractStore(audit)
		store := &us22ExtractStore{events: events}
		service := us22BudgetService(t, store, memory.LearningLimits{})

		service.AfterRun(context.Background(), us22BudgetInput(extractor),
			core.RunResult{RunID: "run-1", Status: core.StatusCompleted})

		require.Equal(t, 1, store.failCalls)
		require.Equal(t, core.LearningFailureAudit, store.failClass,
			"审计预写失败必须归 audit 类别")
		require.NoError(t, store.failCtxErr)
		require.Zero(t, store.commitCalls, "预写失败不得提交候选")
		require.Zero(t, wire.calls, "预写失败不得发出模型调用")
		require.Zero(t, audit.finishCalls)
	})
}

// TestUS22_Hook_BoundedAndNoGoroutineLeak 守 C-06 / FR-22-10：模型耗尽工作段后，钩子在
// 总预算内**同步**返回——返回时失败终态已落库（不是交给后台协程），失败回填上下文未过期
// 且截止被总预算截断，协程数回到基线。
// 逆改点：给钩子加 `go func()` 写终态 → 返回时 failCalls 为零或协程数高于基线而红；
// 失败回填继承已到期的工作段上下文 → failCtxErr 非 nil 而红。
// The hook returns synchronously inside its total budget and leaks no goroutines.
func TestUS22_Hook_BoundedAndNoGoroutineLeak(t *testing.T) {
	limits := memory.LearningLimits{TotalTimeout: 600 * time.Millisecond, Timeout: 250 * time.Millisecond}
	events := &us22BudgetEvents{}
	store := &us22ExtractStore{events: events}
	extractor := &us22BudgetExtractor{store: store, events: events, block: true}
	service := us22BudgetService(t, store, limits)

	baseline := runtime.NumGoroutine()
	start := time.Now()
	service.AfterRun(context.Background(), us22BudgetInput(extractor),
		core.RunResult{RunID: "run-1", Status: core.StatusCompleted})
	elapsed := time.Since(start)

	require.Equal(t, 1, extractor.calls)
	require.Equal(t, 1, store.failCalls, "钩子返回时失败终态必须已同步落库")
	require.Equal(t, core.LearningFailureTimeout, store.failClass)
	require.NoError(t, store.failCtxErr, "失败回填不得继承已到期的工作段上下文")
	require.GreaterOrEqual(t, elapsed, 200*time.Millisecond, "钩子确实等满了工作段")
	require.Less(t, elapsed, limits.TotalTimeout+300*time.Millisecond,
		"钩子必须在总预算附近返回，不得叠加独立预算")
	require.False(t, store.failDeadline.IsZero())
	require.WithinDuration(t, start.Add(limits.TotalTimeout), store.failDeadline, 150*time.Millisecond,
		"失败回填截止从总预算截断")

	// 不能用 testify.Eventually：它在**新协程**里跑条件，读数恒为 baseline+1。
	// 这里在测试协程内轮询，读数才与基线可比。
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	require.LessOrEqual(t, runtime.NumGoroutine(), baseline,
		"钩子不得留下后台协程（baseline=%d）", baseline)
}

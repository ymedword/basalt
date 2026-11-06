package provider_test

// 本文件承载 US-22 抽取调用面（`ProviderService.ChatMemoryExtract`）的验收测试：
// 用途固定为 memory_extract、round 恒为 0、拒绝 Flow 与工具定义、一次调用不重试、
// 冻结视图只路由这一家、未知 provider 点名失败、预写失败拒绝发出、失败也回填终态、
// `Pinned` 复制抽取预写仓储、回填截止在**开始回填时**计算（FR-22-04 / FR-22-10，
// 颗粒度文档 §3.1.6 / §4.2）。
// Acceptance tests for the extraction call surface of ProviderService.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
)

// memoryAuditStore 是抽取路径的判卷桩：分别计数聊天预写与抽取预写、记录事件顺序、
// 终态回填的上下文截止与取消状态。既实现 `core.LLMCallStore` 又实现
// `core.MemoryExtractCallStore`——抽取若误走聊天预写，计数立刻可见。
// memoryAuditStore is the assertion double for both pre-write surfaces; it records event
// order and the finalize context so a wrong path cannot hide.
type memoryAuditStore struct {
	mu sync.Mutex

	chatBegins   int
	memoryBegins int
	events       []string

	beginMemoryErr error
	beginCtxErr    error
	beginDeadline  time.Time
	beginHasDeadln bool

	finishes  []finishRecord
	finishErr error
}

// finishRecord 是一次终态回填的判卷读数。
type finishRecord struct {
	id          int64
	result      core.LLMCallResult
	deadline    time.Time
	hasDeadline bool
	ctxErr      error
}

func (s *memoryAuditStore) BeginCall(context.Context, core.LLMCall) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatBegins++
	s.events = append(s.events, "chat_begin")
	return 1, nil
}

func (s *memoryAuditStore) BeginMemoryExtractCall(ctx context.Context, _ core.LLMCall) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memoryBegins++
	s.events = append(s.events, "memory_begin")
	s.beginCtxErr = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		s.beginDeadline, s.beginHasDeadln = deadline, true
	}
	// 真实仓储在已取消 / 超时的上下文上会失败；桩必须同形，否则「预写继承工作段截止」
	// 这条判据看不见 WithoutCancel 的逆改。
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.beginMemoryErr != nil {
		return 0, s.beginMemoryErr
	}
	return 7, nil
}

func (s *memoryAuditStore) FinishCall(ctx context.Context, id int64, result core.LLMCallResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := finishRecord{id: id, result: result, ctxErr: ctx.Err()}
	if deadline, ok := ctx.Deadline(); ok {
		record.deadline, record.hasDeadline = deadline, true
	}
	s.finishes = append(s.finishes, record)
	return s.finishErr
}

func (s *memoryAuditStore) eventLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func (s *memoryAuditStore) beginCounts() (chat, memory int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chatBegins, s.memoryBegins
}

func (s *memoryAuditStore) finishRecords() []finishRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]finishRecord(nil), s.finishes...)
}

// recordingClient 是抽取调用的脚本客户端：记录调用次数与到达 / 返回时刻，按需延时，
// 并把「模型调用」事件写进同一份事件序列（判「预写先于调用」）。
// recordingClient scripts the model call and shares the event log with the store.
type recordingClient struct {
	mu    sync.Mutex
	store *memoryAuditStore

	delay time.Duration
	resp  core.ChatResponse
	err   error

	calls      int
	requests   []core.ChatRequest
	returnedAt time.Time
}

func (c *recordingClient) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	c.mu.Lock()
	c.calls++
	c.requests = append(c.requests, req)
	if c.store != nil {
		c.store.mu.Lock()
		c.store.events = append(c.store.events, "chat_call")
		c.store.mu.Unlock()
	}
	delay := c.delay
	resp, err := c.resp, c.err
	c.mu.Unlock()

	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return core.ChatResponse{}, ctx.Err()
		}
	}

	c.mu.Lock()
	c.returnedAt = time.Now()
	c.mu.Unlock()
	return resp, err
}

func (c *recordingClient) snapshot() (int, []core.ChatRequest, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls, append([]core.ChatRequest(nil), c.requests...), c.returnedAt
}

// newExtractService 装配被测服务：同一桩同时扮演聊天审计与抽取预写仓储。
// newExtractService wires the service under test with the shared assertion double.
func newExtractService(t *testing.T, client core.ProviderClient, store *memoryAuditStore) *provider.ProviderService {
	t.Helper()
	base := provider.NewProviderService(
		map[string]core.ProviderClient{"fake": client},
		map[string]string{"fake": "m-default"},
		store,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return base.WithMemoryExtractStore(store)
}

// TestUS22_Provider_PurposeMemoryExtract —— FR-22-04：抽取预写只有一个入口，用途由仓储
// 固定为 memory_extract；聊天预写**不得**被使用，且 pending 先于模型调用。
func TestUS22_Provider_PurposeMemoryExtract(t *testing.T) {
	client := &recordingClient{resp: core.ChatResponse{Content: `{"items":[]}`}}
	store := &memoryAuditStore{}
	client.store = store
	svc := newExtractService(t, client, store)

	resp, err := svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", SessionID: "s1", Provider: "fake", Round: 0,
	})
	require.NoError(t, err)
	require.Equal(t, `{"items":[]}`, resp.Content)

	chatBegins, memoryBegins := store.beginCounts()
	require.Equal(t, 1, memoryBegins, "抽取必须走 BeginMemoryExtractCall")
	require.Zero(t, chatBegins, "抽取不得回落到聊天预写（那会写成 purpose='chat'）")
	require.Equal(t, []string{"memory_begin", "chat_call"}, store.eventLog(),
		"审计 pending 必须先于模型调用")
	require.True(t, store.beginHasDeadln, "预写必须处在有截止的上下文中")

	finishes := store.finishRecords()
	require.Len(t, finishes, 1)
	require.Equal(t, int64(7), finishes[0].id)
	require.Equal(t, core.LLMCallSuccess, finishes[0].result.Status)
}

// TestUS22_Provider_RoundZero —— FR-22-04：抽取固定 round=0；round>=1 与空 run_id
// 都在预写之前拒绝，不建审计行、不发出调用。
func TestUS22_Provider_RoundZero(t *testing.T) {
	client := &recordingClient{resp: core.ChatResponse{Content: `{}`}}
	store := &memoryAuditStore{}
	svc := newExtractService(t, client, store)

	_, err := svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "round")

	_, err = svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		Provider: "fake", Round: 0,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "run_id")

	_, memoryBegins := store.beginCounts()
	require.Zero(t, memoryBegins, "被拒绝的请求不得留下审计行")
	calls, _, _ := client.snapshot()
	require.Zero(t, calls, "被拒绝的请求不得发出调用")

	_, err = svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_2", Provider: "fake", Round: 0,
	})
	require.NoError(t, err)
	calls, _, _ = client.snapshot()
	require.Equal(t, 1, calls)
}

// TestUS22_Provider_RejectsTools —— FR-22-04：抽取调用不带工具定义；携带即拒绝，
// 且拒绝发生在预写之前。
func TestUS22_Provider_RejectsTools(t *testing.T) {
	client := &recordingClient{resp: core.ChatResponse{Content: `{}`}}
	store := &memoryAuditStore{}
	svc := newExtractService(t, client, store)

	_, err := svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 0,
		Tools: []core.ToolSpec{{Name: "read_file"}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "工具")

	chatBegins, memoryBegins := store.beginCounts()
	require.Zero(t, chatBegins)
	require.Zero(t, memoryBegins)
	calls, _, _ := client.snapshot()
	require.Zero(t, calls)
}

// TestUS22_Provider_RejectsFlow —— FR-22-04：抽取调用不是对话轮，不携带 Flow；
// 携带即拒绝，且拒绝发生在预写之前。
func TestUS22_Provider_RejectsFlow(t *testing.T) {
	client := &recordingClient{resp: core.ChatResponse{Content: `{}`}}
	store := &memoryAuditStore{}
	svc := newExtractService(t, client, store)

	_, err := svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 0, Flow: &core.FlowRunContext{},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Flow")

	chatBegins, memoryBegins := store.beginCounts()
	require.Zero(t, chatBegins)
	require.Zero(t, memoryBegins)
	calls, _, _ := client.snapshot()
	require.Zero(t, calls)
}

// TestUS22_Provider_OneCallNoRetry —— FR-22-04 / C-08：每次尝试只发一次调用；模型失败
// 不重试、不 fallback，也不建第二条审计行。
func TestUS22_Provider_OneCallNoRetry(t *testing.T) {
	client := &recordingClient{err: errors.New("model down")}
	store := &memoryAuditStore{}
	svc := newExtractService(t, client, store)

	_, err := svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 0,
	})
	require.Error(t, err)

	calls, _, _ := client.snapshot()
	require.Equal(t, 1, calls, "同一次尝试只允许一次模型调用")
	_, memoryBegins := store.beginCounts()
	require.Equal(t, 1, memoryBegins, "重试不发生在 provider 内")
	require.Len(t, store.finishRecords(), 1)
}

// TestUS22_Provider_PinnedRunModel —— FR-22-04 / C-08：抽取只走 Run 准入时冻结的
// 一行视图；请求携带的冻结模型原样发出，视图外的 provider 点名拒绝。
func TestUS22_Provider_PinnedRunModel(t *testing.T) {
	client := &recordingClient{resp: core.ChatResponse{Content: `{}`}}
	store := &memoryAuditStore{}
	base := newExtractService(t, client, store)

	pinned, ok := base.Pinned("fake")
	require.True(t, ok)
	_, err := pinned.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 0, Model: "m-run-frozen",
	})
	require.NoError(t, err)
	_, requests, _ := client.snapshot()
	require.Len(t, requests, 1)
	require.Equal(t, "m-run-frozen", requests[0].Model, "冻结模型必须原样发出")

	_, err = pinned.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "other", Round: 0,
	})
	require.ErrorIs(t, err, core.ErrProviderNotConfigured,
		"冻结视图不得路由到映射表外的供应商")
}

// TestUS22_Provider_UnknownProviderFailed —— §4.3 负向表：provider 不在映射表 →
// 点名错误、调用未发出、没有审计行。
func TestUS22_Provider_UnknownProviderFailed(t *testing.T) {
	client := &recordingClient{resp: core.ChatResponse{Content: `{}`}}
	store := &memoryAuditStore{}
	svc := newExtractService(t, client, store)

	_, err := svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "nope", Round: 0,
	})
	require.ErrorIs(t, err, core.ErrProviderNotConfigured)

	chatBegins, memoryBegins := store.beginCounts()
	require.Zero(t, chatBegins)
	require.Zero(t, memoryBegins)
	calls, _, _ := client.snapshot()
	require.Zero(t, calls)
}

// TestUS22_Provider_AuditPreWriteFailureNoCall —— 预写失败 = 确定没执行：返回
// ErrAuditWrite（保留底层错误），不发调用、不写终态。
func TestUS22_Provider_AuditPreWriteFailureNoCall(t *testing.T) {
	client := &recordingClient{resp: core.ChatResponse{Content: `{}`}}
	store := &memoryAuditStore{beginMemoryErr: errors.New("audit down")}
	svc := newExtractService(t, client, store)

	_, err := svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 0,
	})
	require.ErrorIs(t, err, core.ErrAuditWrite)
	require.Contains(t, err.Error(), "audit down", "底层错误必须保留以便排障")

	calls, _, _ := client.snapshot()
	require.Zero(t, calls, "预写失败不得发出模型调用")
	require.Empty(t, store.finishRecords(), "没有 pending 行就没有终态回填")
}

// TestUS22_Provider_FinishOnError —— 模型失败也必须回填终态（取消 / 超时路径同理）：
// 行不留 pending，错误分类与原文落库。
func TestUS22_Provider_FinishOnError(t *testing.T) {
	client := &recordingClient{err: errors.New("boom")}
	store := &memoryAuditStore{}
	svc := newExtractService(t, client, store)

	_, err := svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 0,
	})
	require.Error(t, err)

	finishes := store.finishRecords()
	require.Len(t, finishes, 1)
	require.Equal(t, core.LLMCallFailed, finishes[0].result.Status)
	require.Equal(t, core.ErrorKindUnknown, finishes[0].result.ErrorKind)
	require.Contains(t, finishes[0].result.ErrorMessage, "boom")
	require.NoError(t, finishes[0].ctxErr, "回填上下文不得提前过期")
}

// TestUS22_Provider_PinnedCarriesMemoryStore —— 冻结视图必须携带抽取预写仓储；未装配时
// 在预写处 fail-closed（装配错误），不退回聊天预写、不发出调用。
func TestUS22_Provider_PinnedCarriesMemoryStore(t *testing.T) {
	client := &recordingClient{resp: core.ChatResponse{Content: `{}`}}
	store := &memoryAuditStore{}
	base := newExtractService(t, client, store)

	pinned, ok := base.Pinned("fake")
	require.True(t, ok)
	_, err := pinned.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 0,
	})
	require.NoError(t, err, "Pinned 必须复制 memoryStore")
	_, memoryBegins := store.beginCounts()
	require.Equal(t, 1, memoryBegins)

	// 未装配抽取仓储的视图：装配错误、零调用、零审计行。
	bare := provider.NewProviderService(
		map[string]core.ProviderClient{"fake": client},
		map[string]string{"fake": "m-default"},
		store, slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	barePinned, ok := bare.Pinned("fake")
	require.True(t, ok)
	_, err = barePinned.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_2", Provider: "fake", Round: 0,
	})
	require.ErrorIs(t, err, core.ErrLearningExtractorUnavailable)
	_, memoryBegins = store.beginCounts()
	require.Equal(t, 1, memoryBegins, "未装配时不得写任何预写行")
	calls, _, _ := client.snapshot()
	require.Equal(t, 1, calls, "未装配时不得发出调用")
}

// TestUS22_Provider_FinalizeDeadlineUsesStartTime —— FR-22-10 时延：回填截止在**开始
// 回填时**计算 min(now+5s, 钩子总截止)，不是在调用开始时预先算死。逆改（调用开始就
// 固定）会让慢模型正常返回时回填截止已经过期，成功审计落不了库。
func TestUS22_Provider_FinalizeDeadlineUsesStartTime(t *testing.T) {
	const auditWindow = 5 * time.Second

	client := &recordingClient{
		resp:  core.ChatResponse{Content: `{"items":[]}`},
		delay: 30 * time.Millisecond,
	}
	store := &memoryAuditStore{}
	svc := newExtractService(t, client, store)

	start := time.Now()
	// 钩子总截止略大于 5s：正确实现取「回填开始时刻 + 5s」（此刻仍是它更小）；
	// 在调用开始时固定则恰好等于 start+5s，两者可区分。
	hookDeadline := start.Add(auditWindow + 200*time.Millisecond)
	_, err := svc.ChatMemoryExtract(context.Background(), core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 0, HookDeadline: hookDeadline,
	})
	require.NoError(t, err)

	finishes := store.finishRecords()
	require.Len(t, finishes, 1)
	require.True(t, finishes[0].hasDeadline, "回填上下文必须带截止")
	require.True(t, finishes[0].deadline.After(start.Add(auditWindow)),
		"回填截止必须晚于调用开始 + 5s——预先算死的实现会恰好等于它")
	require.NoError(t, finishes[0].ctxErr, "慢模型正常返回后，成功审计仍须落库")
	require.Equal(t, core.LLMCallSuccess, finishes[0].result.Status)

	_, _, returnedAt := client.snapshot()
	require.WithinDuration(t, returnedAt.Add(auditWindow), finishes[0].deadline, time.Second,
		"截止应以回填开始时刻为基准")
}

// TestUS22_Provider_PreWriteInheritsWorkDeadline —— FR-22-10 时延：抽取预写继承工作段
// 截止（`context.WithTimeout`），**不得**用 `WithoutCancel` 外扩独立 5s。父上下文已取消
// 时，预写必须看到取消并拒绝发出。
func TestUS22_Provider_PreWriteInheritsWorkDeadline(t *testing.T) {
	client := &recordingClient{resp: core.ChatResponse{Content: `{}`}}
	store := &memoryAuditStore{}
	svc := newExtractService(t, client, store)

	parent, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.ChatMemoryExtract(parent, core.ChatRequest{
		RunID: "run_1", Provider: "fake", Round: 0,
	})
	require.ErrorIs(t, err, core.ErrAuditWrite)
	require.ErrorIs(t, store.beginCtxErr, context.Canceled,
		"预写上下文必须继承工作段的取消")
	calls, _, _ := client.snapshot()
	require.Zero(t, calls)
}

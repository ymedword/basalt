package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是循环面（US1）的验收测试与共享 harness：进程内假模型驱动唯一运行入口，
// 断言留痕顺序 / 三类终止 / 逐轮重组装 / 提前返回路径都经半截组补齐。
// This file holds the loop-surface acceptance tests and the shared harness: an in-process
// fake model drives the single run entry point; assertions cover write ordering, the three
// terminal states, per-round reassembly and the half-group completion on every exit path.

// ctx 是 harness 为每个用例建立的可取消上下文（用例内串行执行，构造时覆盖包级变量）。
// 之所以暴露为包级变量，是为了让颗粒度文档 §4.3 的回归代码原样落地：h.cancel() 取消的
// 正是传进 h.service.Run 的那个 ctx。
// ctx is the cancellable context the harness creates per test case; exposing it at package
// level keeps the §4.3 regression code verbatim — h.cancel() cancels exactly the context
// handed to h.service.Run.
var ctx context.Context

// sectionOneStatic 是假快照的段一正文（断言"段一取自快照、不重读盘"的锚点）。
// sectionOneStatic is the fake snapshot's section-one text.
const sectionOneStatic = "段一：你是 demo。"

// loopSessionID 是 harness 的会话标识（形态与 §6.2 固定口径 1 的派生结果同构）。
const loopSessionID = "cli-3f9a1c07b2d84e55"

// loopAgentName 是 harness 的 Agent 名（假装载器里唯一注册的一只）。
const loopAgentName = "demo"

// serialProbe 是串行断言用例里放大并发窗口的延时：顺序实现与它无关（必过），
// 并发实现会在这个窗口里撞上 in-flight 计数。
const serialProbe = 2 * time.Millisecond

// errNoScript 是"脚本没给这次调用准备响应"的内部记号——只用于让用例快速失败。
var errNoScript = errors.New("fake provider: no scripted response")

// loopHarness 是循环面测试的共享夹具：假模型 + 假仓储 + 假审计 + 假工具注册池，
// 经 **唯一运行入口**（core.AgentService）驱动——测试不直接调 ReActLoop。
// loopHarness is the shared fixture of the loop surface: fake model, stores, audit and
// tool pool, driven through the single run entry point rather than through ReActLoop.
type loopHarness struct {
	t *testing.T

	service  *core.AgentService
	provider *fakeProvider
	store    *fakeStore
	audit    *fakeAudit
	registry *fakeResolver
	tools    *spyTool
	agents   *fakeAgentSource
	logs     *bytes.Buffer

	toolSet   []string
	budget    core.Budget
	sessionID string
	agentsDir string

	now       time.Time
	cancelRun context.CancelFunc
}

// newLoopHarness 组装一套独立夹具（用例之间零共享状态）。
// newLoopHarness assembles an isolated fixture; test cases share no state.
func newLoopHarness(t *testing.T) *loopHarness {
	t.Helper()
	h := &loopHarness{
		t:         t,
		store:     newFakeStore(),
		registry:  newFakeResolver(),
		logs:      &bytes.Buffer{},
		toolSet:   []string{"read_file"},
		sessionID: loopSessionID,
		agentsDir: filepath.Join(t.TempDir(), loopAgentName),
		now:       time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC),
		budget: core.Budget{
			MaxIterations:  10,
			MaxToolCalls:   50,
			ToolTimeout:    30 * time.Second,
			MaxOutputBytes: 256 * 1024,
			ContextTokens:  32768,
		},
	}
	h.tools = &spyTool{name: "read_file"}
	h.registry.register(h.tools)
	h.audit = &fakeAudit{store: h.store}
	h.provider = newFakeProvider(t)
	h.agents = &fakeAgentSource{h: h}

	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.service = core.NewAgentService(core.ServiceOptions{
		Agents:    h.agents,
		Client:    h.provider,
		Messages:  h.store,
		Runs:      h.store,
		Snapshots: h.store,
		Audit:     h.audit,
		Loop: core.NewReActLoop(
			core.NewPromptBuilder(h.registry),
			core.NewToolExecutor(h.registry, h.audit, logger),
			logger,
		),
		Logger: logger,
		Now:    func() time.Time { return h.now },
	})

	runCtx, cancel := context.WithCancel(context.Background())
	h.cancelRun = cancel
	ctx = runCtx
	t.Cleanup(cancel)
	return h
}

// req 组装一次运行请求（预算与渠道取自 harness 的当前状态）。
// req builds one run request from the harness's current state.
func (h *loopHarness) req(msg string) core.RunRequest {
	return core.RunRequest{
		Agent:     loopAgentName,
		SessionID: h.sessionID,
		Message:   msg,
		Channel:   "cli",
		UserID:    "tester",
		Budget:    h.budget,
	}
}

// cancel 取消本次运行（§4.3 的回归代码按此形状调用）。
// cancel cancels the current run.
func (h *loopHarness) cancel() { h.cancelRun() }

// —— 假模型 ——

// fakeProvider 是按调用序脚本化的假 ProviderClient：第 i 次调用取 resp[i] / errs[i]。
// fakeProvider is a scripted in-process ProviderClient: call i takes resp[i] or errs[i].
type fakeProvider struct {
	t *testing.T

	mu    sync.Mutex
	resp  map[int]core.ChatResponse
	errs  map[int]error
	calls []core.ChatRequest
	hook  func(call int) // 调用入口钩子（用例用它在中途取消）
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	return &fakeProvider{
		t:    t,
		resp: map[int]core.ChatResponse{},
		errs: map[int]error{},
	}
}

// onCall 给第 i 次（0 起）调用挂一条响应。
func (p *fakeProvider) onCall(i int, resp core.ChatResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resp[i] = resp
}

// onError 给第 i 次（0 起）调用挂一个错误。
func (p *fakeProvider) onError(i int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs[i] = err
}

// Chat 发出一条脚本响应；没脚本的调用即用例失败。
func (p *fakeProvider) Chat(_ context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	p.mu.Lock()
	i := len(p.calls)
	p.calls = append(p.calls, req)
	hook := p.hook
	err := p.errs[i]
	resp, scripted := p.resp[i]
	p.mu.Unlock()

	if hook != nil {
		hook(i)
	}
	if err != nil {
		return core.ChatResponse{}, err
	}
	if !scripted {
		p.t.Errorf("fake provider: 第 %d 次调用没有脚本响应（脚本只备了 %d 条）", i, len(p.resp))
		return core.ChatResponse{}, errNoScript
	}
	return resp, nil
}

// callCount 是已发生的调用次数。
func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// request 取第 i 次调用的入参（断言轮号 / 历史 / 工具定义）。
func (p *fakeProvider) request(i int) core.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Less(p.t, i, len(p.calls), "第 %d 次调用不存在（实际 %d 次）", i, len(p.calls))
	return p.calls[i]
}

// toolCallResponse 组装一次"模型请求调用工具"的响应（单次调用）。
func toolCallResponse(id, name, args string) core.ChatResponse {
	return core.ChatResponse{ToolCalls: []core.ToolCall{{ID: id, Name: name, Arguments: args}}}
}

// toolCallResponses 把多次"请求调用工具"合并为**同一轮**的一次响应（一轮多调用夹具）。
func toolCallResponses(resps ...core.ChatResponse) core.ChatResponse {
	merged := core.ChatResponse{}
	for _, r := range resps {
		merged.ToolCalls = append(merged.ToolCalls, r.ToolCalls...)
	}
	return merged
}

// textResponse 组装一次纯文本完成响应。
func textResponse(text string) core.ChatResponse {
	return core.ChatResponse{Content: text}
}

// —— 假会话 / 运行 / 快照仓储 ——

// storedMessage 是库里的消息行：消息本体 + 落库时显式承载的 run_id 列。
type storedMessage struct {
	msg   core.Message
	runID string
}

// storedInvocation 是库里的工具审计行（两阶段：预写 pending → 终态回填）。
type storedInvocation struct {
	inv      core.ToolInvocation
	status   core.LLMCallStatus
	result   core.ToolInvocationResult
	finished bool
}

// fakeStore 是一套内存仓储后端，实现 core.MessageStore / core.RunStore / core.SnapshotStore，
// 并为审计面（fakeAudit）提供同一份数据——审计行与消息行在同一个"库"里，便于跨面断言。
type fakeStore struct {
	mu sync.Mutex

	msgs     map[string][]storedMessage
	sessions map[string]core.SessionRecord
	runs     map[string]core.RunRecord
	status   map[string]core.RunStatus
	reason   map[string]string

	invocations []storedInvocation
	nextInvID   int64

	snapshots []core.ConfigSnapshot

	// failToolAppendOnce 注入"工具结果追加失败"（提前返回路径用例）——**一次性**：
	// 只失败第一次，之后的补齐写入必须能落（否则测的是"存储全坏"，不是"提前返回路径"）。
	failToolAppendOnce bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		msgs:     map[string][]storedMessage{},
		sessions: map[string]core.SessionRecord{},
		runs:     map[string]core.RunRecord{},
		status:   map[string]core.RunStatus{},
		reason:   map[string]string{},
	}
}

// AppendMessage 追加一条消息（`sessionID` 为空 = 无状态会话，不落历史）。
func (s *fakeStore) AppendMessage(_ context.Context, sessionID, runID string, m core.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID == "" {
		return nil
	}
	if m.Role == core.RoleTool && s.failToolAppendOnce {
		s.failToolAppendOnce = false
		return errors.New("injected: message store unavailable")
	}
	s.msgs[sessionID] = append(s.msgs[sessionID], storedMessage{msg: m, runID: runID})
	return nil
}

// Messages 取整段会话历史（升序）。
func (s *fakeStore) Messages(_ context.Context, sessionID string) ([]core.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messagesLocked(sessionID), nil
}

// UpsertSession 建行 / 刷新最后活动时间。
func (s *fakeStore) UpsertSession(_ context.Context, rec core.SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[rec.ID] = rec
	return nil
}

// ListSessions 列出会话（排序口径：标识升序）。
func (s *fakeStore) ListSessions(_ context.Context) ([]core.SessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.SessionRecord, 0, len(s.sessions))
	for _, rec := range s.sessions {
		out = append(out, rec)
	}
	return out, nil
}

// StartRun 建 `running` 行。
func (s *fakeStore) StartRun(_ context.Context, rec core.RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[rec.RunID] = rec
	s.status[rec.RunID] = core.StatusRunning
	return nil
}

// FinishRun 把终态写回同一行（不接收 finalText——列面没有它）。
func (s *fakeStore) FinishRun(_ context.Context, runID string, status core.RunStatus, reason string, _, _ int, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[runID] = status
	s.reason[runID] = reason
	return nil
}

// SaveSnapshot 落配置快照（insert-if-absent 语义：同 hash 不覆盖）。
func (s *fakeStore) SaveSnapshot(_ context.Context, snap *core.ConfigSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshots = append(s.snapshots, *snap)
	return nil
}

// countByRole 数某个会话里某角色的消息条数（§4.3 的 `SELECT count(*)` 级断言）。
func (s *fakeStore) countByRole(t *testing.T, sessionID, role string) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.msgs[sessionID] {
		if m.msg.Role == role {
			n++
		}
	}
	return n
}

// lastToolContent 取会话里最后一条 tool 消息的正文。
func (s *fakeStore) lastToolContent(t *testing.T, sessionID string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs := s.messagesLocked(sessionID)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == core.RoleTool {
			return msgs[i].Content
		}
	}
	t.Fatalf("会话 %s 里没有 tool 消息", sessionID)
	return ""
}

// toolContent 按 tool_call_id 取回填文本。
func (s *fakeStore) toolContent(t *testing.T, sessionID, toolCallID string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.msgs[sessionID] {
		if m.msg.Role == core.RoleTool && m.msg.ToolCallID == toolCallID {
			return m.msg.Content
		}
	}
	t.Fatalf("会话 %s 里没有 tool_call_id=%s 的回填消息", sessionID, toolCallID)
	return ""
}

// countToolInvocations 数审计行条数（工具执行面）。
func (s *fakeStore) countToolInvocations(t *testing.T) int {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.invocations)
}

// messages 取会话消息副本。
func (s *fakeStore) messages(t *testing.T, sessionID string) []core.Message {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messagesLocked(sessionID)
}

// runStatus 取运行终态（断言终态落库）。
func (s *fakeStore) runStatus(t *testing.T, runID string) core.RunStatus {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status[runID]
}

// runReason 取运行终态原因。
func (s *fakeStore) runReason(t *testing.T, runID string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason[runID]
}

// missingResults 是"人人有结果"的判据面：把会话里所有 assistant 的 tool_calls 与本会话
// 的 tool 回填逐一比对（与恢复路径共用 core.MissingToolResults——同一份判据实现）。
func (s *fakeStore) missingResults(t *testing.T, sessionID string) []core.ToolCall {
	t.Helper()
	msgs := s.messages(t, sessionID)
	var calls []core.ToolCall
	for _, m := range msgs {
		if m.Role == core.RoleAssistant {
			calls = append(calls, m.ToolCalls...)
		}
	}
	return core.MissingToolResults(calls, msgs)
}

// messagesLocked 是内部的取历史实现（调用方持锁）。
func (s *fakeStore) messagesLocked(sessionID string) []core.Message {
	out := make([]core.Message, 0, len(s.msgs[sessionID]))
	for _, m := range s.msgs[sessionID] {
		out = append(out, m.msg)
	}
	return out
}

// —— 假审计（工具侧两阶段写 + 补齐路径只读面）——

// fakeAudit 实现 core.ToolInvocationStore 与 core.ToolInvocationReader，
// 支持注入预写失败（fail-closed 回归）与终态回填失败。
type fakeAudit struct {
	store *fakeStore

	failStart  error // 预写失败注入
	failFinish error // 终态回填失败注入
}

// BeginInvocation 写 `pending` 行并返回行 id；注入失败时**不写行**。
func (a *fakeAudit) BeginInvocation(_ context.Context, inv core.ToolInvocation) (int64, error) {
	if a.failStart != nil {
		return 0, a.failStart
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	a.store.nextInvID++
	id := a.store.nextInvID
	a.store.invocations = append(a.store.invocations, storedInvocation{
		inv:    inv,
		status: core.LLMCallPending,
	})
	return id, nil
}

// FinishInvocation 把同一行更新为终态；注入失败时行保持 `pending`。
func (a *fakeAudit) FinishInvocation(_ context.Context, id int64, res core.ToolInvocationResult) error {
	if a.failFinish != nil {
		return a.failFinish
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for i := range a.store.invocations {
		if int64(i+1) != id {
			continue
		}
		a.store.invocations[i].status = res.Status
		a.store.invocations[i].result = res
		a.store.invocations[i].finished = true
		return nil
	}
	return fmt.Errorf("tool invocation row %d not found", id)
}

// Lookup 按 (runID, toolCallID) 取审计行（补齐路径专用只读面）。
func (a *fakeAudit) Lookup(_ context.Context, runID, toolCallID string) (core.ToolInvocationView, bool, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for _, row := range a.store.invocations {
		if row.inv.RunID != runID || row.inv.ToolCallID != toolCallID {
			continue
		}
		return core.ToolInvocationView{
			Status:       row.status,
			ResultJSON:   row.result.ResultJSON,
			ErrorMessage: row.result.ErrorMessage,
			ErrorKind:    row.result.ErrorKind,
		}, true, nil
	}
	return core.ToolInvocationView{}, false, nil
}

// waitIdle 限时等待"收尾审计写入已结束"：每条已预写的行都拿到了终态。
// 同步执行模型下它通常在首次检查即满足；保留为等待点是为了让取消回归不依赖
// "Run 返回时恰好已无在途写入"的时序侥幸。
func (a *fakeAudit) waitIdle(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		a.store.mu.Lock()
		open := 0
		for _, row := range a.store.invocations {
			if !row.finished {
				open++
			}
		}
		a.store.mu.Unlock()
		if open == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("限时 %s 内收尾审计写入未结束（仍有 %d 行停在 pending）", d, open)
		}
		time.Sleep(time.Millisecond)
	}
}

// —— 假工具注册池与假工具 ——

// fakeResolver 实现 core.ToolResolver（显式注册 + 保序取规格）。
type fakeResolver struct {
	mu    sync.Mutex
	tools map[string]core.Tool
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{tools: map[string]core.Tool{}}
}

// register 注册一件工具（重名覆盖——测试夹具足够）。
func (r *fakeResolver) register(tool core.Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

// Resolve 查池。
func (r *fakeResolver) Resolve(name string) (core.Tool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tool, ok := r.tools[name]
	return tool, ok
}

// Specs 按传入顺序取规格。
func (r *fakeResolver) Specs(names []string) []core.ToolSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]core.ToolSpec, 0, len(names))
	for _, name := range names {
		tool, ok := r.tools[name]
		if !ok {
			continue
		}
		out = append(out, core.ToolSpec{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  tool.InputSchema(),
		})
	}
	return out
}

// spyTool 是假的必需工具实现：记录执行次数、可阻塞到取消（取消传播回归），
// 并暴露用例可设置的钩子。
type spyTool struct {
	name string

	mu        sync.Mutex
	executed  int
	active    int
	maxActive int
	finished  chan struct{} // 最近一次执行体返回时关闭

	// 用例钩子。
	onExecute        func(call int)
	onStarted        func()
	blockUntilCancel bool
	execDelay        time.Duration
}

// Name 是注册名。
func (s *spyTool) Name() string { return s.name }

// Description 是段 4 用的描述。
func (s *spyTool) Description() string { return "测试用假工具 " + s.name }

// InputSchema 是段 4 用的参数 schema。
func (s *spyTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
}

// Execute 记一次执行；`blockUntilCancel` 时等 ctx 被取消再返回（取消传播用例的锚点）。
func (s *spyTool) Execute(execCtx context.Context, _ core.ToolEnv, _ json.RawMessage) (core.Result, error) {
	s.mu.Lock()
	call := s.executed
	s.executed++
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	done := make(chan struct{})
	s.finished = done
	hook, onStarted, block, delay := s.onExecute, s.onStarted, s.blockUntilCancel, s.execDelay
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
		close(done)
	}()

	if hook != nil {
		hook(call)
	}
	if block {
		if onStarted != nil {
			onStarted()
		}
		<-execCtx.Done()
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	return core.Result{Content: fmt.Sprintf("【%s 第 %d 次执行结果】", s.name, call)}, nil
}

// maxConcurrent 是执行体的最大同时在跑数（串行断言用：恒 ≤ 1）。
func (s *spyTool) maxConcurrent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxActive
}

// waitExited 限时等待最近一次执行体返回。
func (s *spyTool) waitExited(t *testing.T, d time.Duration) {
	t.Helper()
	s.mu.Lock()
	done := s.finished
	s.mu.Unlock()
	if done == nil {
		t.Fatal("假工具从未被执行，无从等待退出")
	}
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("限时 %s 内工具执行体未返回（取消未传播到工具）", d)
	}
}

// —— 假 Agent 面（准入 + 快照）——

// fakeAgentSource 实现 core.AgentSource：只有一只注册 Agent，快照随 harness 的工具集
// 实时变化（用例改 h.toolSet 后立即生效——模拟"注册池与快照不同源"）。
type fakeAgentSource struct {
	h *loopHarness

	failSnapshot error // 注入"任一清单文件读失败"
	profileIter  int   // Profile.settings.max_iterations（0 表示用装载器缺省 10）
}

// Exists 判定 Agent 是否可运行。
func (a *fakeAgentSource) Exists(name string) bool { return name == loopAgentName }

// Dir 返回 Agent 目录绝对路径。
func (a *fakeAgentSource) Dir(name string) string {
	if name != loopAgentName {
		return ""
	}
	return a.h.agentsDir
}

// Snapshot 冻结一份运行级快照：段一正文、Profile（含工具集）与哈希。
func (a *fakeAgentSource) Snapshot(_ context.Context, name string, _ time.Time) (*core.ConfigSnapshot, error) {
	if a.failSnapshot != nil {
		return nil, a.failSnapshot
	}
	if name != loopAgentName {
		return nil, &core.AgentNotFoundError{Name: name}
	}
	tools := append([]string(nil), a.h.toolSet...)
	iterations := a.profileIter
	if iterations == 0 {
		iterations = 10
	}
	profile := &core.Profile{
		Name:     name,
		Provider: core.ProviderRef{Name: "fake", Model: "fake-model"},
		Tools:    tools,
		Settings: core.Settings{MaxIterations: iterations, MaxHistoryTurns: 20},
	}
	return &core.ConfigSnapshot{
		AgentName:  name,
		Hash:       "1f0d7c9a4b2e5f60",
		Content:    "---\nname: demo\n---\n" + sectionOneStatic + "\n",
		Manifest:   []core.ManifestEntry{{Path: "AGENT.md", SHA256: "d41d8cd98f00b204"}},
		SectionOne: sectionOneStatic,
		Profile:    profile,
	}, nil
}

// —— A1~A16 ——

// TestReActLoop_IterationLimitReached 守"迭代上限是预期终止、不是错误"：
// 验收点（契约 §2 状态机 / §1.3）——到顶返回 limit_reached，函数级 error 恒 nil。
func TestReActLoop_IterationLimitReached(t *testing.T) {
	h := newLoopHarness(t)
	h.budget.MaxIterations = 2
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, toolCallResponse("c2", "read_file", `{"path":"b.txt"}`))

	res, err := h.service.Run(ctx, h.req("一直读文件"))
	require.NoError(t, err, "达上限是预期终止，不是调用方错误")
	require.Equal(t, core.StatusLimitReached, res.Status)
	require.Equal(t, core.ReasonIterationLimit, res.Reason)
	require.Equal(t, 2, res.Iterations, "iterations = 已收到模型响应的轮次数")
	require.Equal(t, 2, res.ToolCalls)
	require.Equal(t, core.StatusLimitReached, h.store.runStatus(t, res.RunID))
	require.Zero(t, len(h.store.missingResults(t, h.sessionID)), "人人有结果：循环外返回不产生半截组")
}

// TestReActLoop_FirstTextCompletionIterationsOne 守"首次纯文本完成"：
// 验收点（契约 §2）——无工具调用即完成，iterations = 1，最终回答只落一次。
func TestReActLoop_FirstTextCompletionIterationsOne(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, textResponse("你好，我在"))

	res, err := h.service.Run(ctx, h.req("在吗"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, "你好，我在", res.FinalText)
	require.Equal(t, 1, res.Iterations)
	require.Zero(t, res.ToolCalls)
	require.Equal(t, 1, h.provider.callCount(), "完成即停：不再发起第二轮调用")
	require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleAssistant))
	require.Zero(t, h.store.countByRole(t, h.sessionID, core.RoleTool))
	require.Equal(t, core.StatusCompleted, h.store.runStatus(t, res.RunID))
}

// TestReActLoop_ToolBudgetExhausted 守"工具预算耗尽 = limit_reached + 点名原因"：
// 验收点（契约 §2 / §4.4 五分支）——组内未执行的调用取"未执行（工具调用次数超限）"。
func TestReActLoop_ToolBudgetExhausted(t *testing.T) {
	h := newLoopHarness(t)
	h.budget.MaxToolCalls = 1
	h.provider.onCall(0, toolCallResponses(
		toolCallResponse("c1", "read_file", `{"path":"a.txt"}`),
		toolCallResponse("c2", "read_file", `{"path":"b.txt"}`),
	))

	res, err := h.service.Run(ctx, h.req("读两个文件"))
	require.NoError(t, err)
	require.Equal(t, core.StatusLimitReached, res.Status, "达上限不是失败")
	require.Equal(t, core.ReasonToolBudget, res.Reason)
	require.Equal(t, 1, res.ToolCalls)
	require.Equal(t, 1, h.provider.callCount())
	require.Equal(t, core.TextToolBudget, h.store.toolContent(t, h.sessionID, "c2"),
		"未执行的调用必须回填预算文本（模型看不到结果就不会收敛）")
}

// TestReActLoop_RoundNumbersStartAtOne 守"轮号从 1 起且逐轮递增"：
// 验收点（契约 §7.1 `round` 语义）——llm_calls.round 与 Run 内调用序一致。
func TestReActLoop_RoundNumbersStartAtOne(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("done"))

	res, err := h.service.Run(ctx, h.req("读一下"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, 1, h.provider.request(0).Round, "第一轮必须是 1（不是 0）")
	require.Equal(t, 2, h.provider.request(1).Round)
	require.Equal(t, h.provider.request(0).RunID, h.provider.request(1).RunID, "同一 Run 内轮次共用 RunID")
	require.Equal(t, res.RunID, h.provider.request(0).RunID, "调用审计关联的 RunID 与返回值一致")
	require.Equal(t, "1f0d7c9a4b2e5f60", h.provider.request(0).ConfigHash, "出站带快照哈希")
}

// TestReActLoop_ToolsExecuteSerially 守"工具串行、无并发执行"：
// 验收点（宪法 VII 同步执行）——一轮多调用时执行区间不重叠（in-flight 计数恒 ≤ 1）。
func TestReActLoop_ToolsExecuteSerially(t *testing.T) {
	h := newLoopHarness(t)
	h.tools.execDelay = serialProbe // 放大并发窗口（顺序实现与它无关）
	h.provider.onCall(0, toolCallResponses(
		toolCallResponse("c1", "read_file", `{"path":"a.txt"}`),
		toolCallResponse("c2", "read_file", `{"path":"b.txt"}`),
		toolCallResponse("c3", "read_file", `{"path":"c.txt"}`),
	))
	h.provider.onCall(1, textResponse("done"))

	res, err := h.service.Run(ctx, h.req("读三个文件"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, 3, h.tools.executed)
	require.LessOrEqual(t, h.tools.maxConcurrent(), 1, "工具执行必须串行（同时在跑数不得 > 1）")
	require.Equal(t, 3, h.store.countByRole(t, h.sessionID, core.RoleTool))
}

// TestReActLoop_RebuildsPromptEveryRound 守"每轮重组装"：
// 验收点（FR-004）——段一取自快照（不重读）、上一轮工具结果必须进本轮历史。
func TestReActLoop_RebuildsPromptEveryRound(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"notes.txt"}`))
	h.provider.onCall(1, textResponse("总结完成"))

	res, err := h.service.Run(ctx, h.req("读一下 notes.txt 并总结"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	first := h.provider.request(0)
	require.Equal(t, "system", first.Messages[0].Role, "段 1 是系统提示词")
	require.Equal(t, sectionOneStatic, first.Messages[0].Content, "段 1 正文取自快照（不重读盘）")
	require.Equal(t, "读一下 notes.txt 并总结", first.Messages[len(first.Messages)-1].Content, "当前用户消息在末位")
	require.Len(t, first.Tools, 1, "段 4 带本 Agent 的工具定义")
	require.Equal(t, "read_file", first.Tools[0].Name)

	second := h.provider.request(1)
	var found bool
	for _, m := range second.Messages {
		if m.Role == core.RoleTool && m.ToolCallID == "c1" {
			found = true
			require.Contains(t, m.Content, "read_file 第 0 次执行结果", "本轮历史必须带上上一轮的工具结果")
		}
	}
	require.True(t, found, "第二轮请求的历史里必须有第一轮工具结果的回填消息")
	require.Contains(t, second.Messages[len(second.Messages)-1].Content, "【read_file", "工具结果在末位（其后才是模型的新响应）")
}

// TestReActLoop_CancelAtRoundStartSkipsModelCall 守"轮首取消不计当前轮"：
// 验收点（契约 §2 状态机）——已取消的上下文下不发模型调用，iterations = 0。
func TestReActLoop_CancelAtRoundStartSkipsModelCall(t *testing.T) {
	h := newLoopHarness(t)
	h.cancel() // 开跑前就取消

	res, err := h.service.Run(ctx, h.req("喂"))
	require.NoError(t, err, "取消是预期动作，不是调用方错误")
	require.Equal(t, core.StatusCancelled, res.Status)
	require.Zero(t, res.Iterations, "轮首取消：那次的模型调用未发出，不计入轮次")
	require.Zero(t, h.provider.callCount(), "取消后不得发起模型调用")
	require.Equal(t, core.StatusCancelled, h.store.runStatus(t, res.RunID), "取消也要落终态")
}

// TestReActLoop_ToolBudgetDoesNotBlockTextResponse 守"工具预算耗尽不禁止纯文本模型响应"：
// 验收点（FR-003）——预算只在"要执行工具之前"判，模型给出无工具的响应即完成。
func TestReActLoop_ToolBudgetDoesNotBlockTextResponse(t *testing.T) {
	h := newLoopHarness(t)
	h.budget.MaxToolCalls = 1
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("够了，我直接回答"))

	res, err := h.service.Run(ctx, h.req("读一下再答"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status,
		"预算已耗尽仍是 completed——预算不禁止不使用工具的模型响应")
	require.Equal(t, "够了，我直接回答", res.FinalText)
	require.Equal(t, 2, res.Iterations)
	require.Equal(t, 1, res.ToolCalls)
}

// TestReActLoop_EveryEarlyReturnCompletesToolGroup 守"每条提前返回路径都经补齐、无旁路"：
// 验收点（FR-016 / 流程 B）——取消 / 预算 / 追加失败三条路径结束后，
// 仍满足"每个 tool_call 都有对应结果消息"（判据与恢复扫描共用同一实现）。
func TestReActLoop_EveryEarlyReturnCompletesToolGroup(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *loopHarness)
		want  core.RunStatus
	}{
		{
			name: "取消路径",
			setup: func(h *loopHarness) {
				h.tools.onExecute = func(int) { h.cancel() }
			},
			want: core.StatusCancelled,
		},
		{
			name: "预算路径",
			setup: func(h *loopHarness) {
				h.budget.MaxToolCalls = 1
			},
			want: core.StatusLimitReached,
		},
		{
			name: "工具结果追加失败路径",
			setup: func(h *loopHarness) {
				h.store.failToolAppendOnce = true
			},
			want: core.StatusFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoopHarness(t)
			h.provider.onCall(0, toolCallResponses(
				toolCallResponse("c1", "read_file", `{"path":"a.txt"}`),
				toolCallResponse("c2", "read_file", `{"path":"b.txt"}`),
			))
			tc.setup(h)

			res, err := h.service.Run(ctx, h.req("读两个文件"))
			require.NoError(t, err)
			require.Equal(t, tc.want, res.Status)
			require.Zero(t, len(h.store.missingResults(t, h.sessionID)),
				"提前返回的每条路径都必须补齐半截组（不留悬空调用）")
			require.Equal(t, 2, h.store.countByRole(t, h.sessionID, core.RoleTool), "两个调用都要有结果消息")
		})
	}
}

// TestReActLoop_AppendsAssistantBeforeExecutingTools 守"留痕顺序"：
// 验收点（契约 §4.3.2 半截组判据的前提）——assistant 消息（带 tool_calls）必须**先**落库、
// **再**执行工具。若反转，崩溃后恢复扫描找不到 assistant 行，半截组无从判定。
// 同时钉住"最终回答不丢"：一次运行 = 2 条 assistant（带工具调用的一条 + 最终回答的一条）。
func TestReActLoop_AppendsAssistantBeforeExecutingTools(t *testing.T) {
	h := newLoopHarness(t) // fake provider：第 1 次返回 tool_call，第 2 次返回纯文本
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"notes.txt"}`))
	h.provider.onCall(1, textResponse("done"))
	// 工具执行当时：assistant 行已存在，且此刻**只有 1 条**（工具尚未产生任何 assistant）
	h.tools.onExecute = func(call int) {
		require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleAssistant),
			"执行工具前 assistant 消息必须已落库（留痕顺序）")
	}
	res, err := h.service.Run(ctx, h.req("读一下 notes.txt"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, "done", res.FinalText)
	// 运行结束：**2 条** assistant（工具轮 1 条 + 最终回答 1 条）——断言 1 条会把"丢最终回答"判成通过
	require.Equal(t, 2, h.store.countByRole(t, h.sessionID, core.RoleAssistant),
		"最终回答必须落库：一次工具轮 + 一次纯文本完成 = 2 条 assistant")
	require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleTool))
}

// TestReActLoop_ModelFailureIsRunFailed 守"运行期失败归状态、不归 error"：
// 验收点（契约 §1.2）——模型调用失败（非取消）→ failed + 可读原因，函数级 error 仍为 nil。
func TestReActLoop_ModelFailureIsRunFailed(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onError(0, errors.New("connection reset by peer"))

	res, err := h.service.Run(ctx, h.req("喂"))
	require.NoError(t, err, "运行期失败经 Status 表达，不当成调用方错误")
	require.Equal(t, core.StatusFailed, res.Status)
	require.Contains(t, res.Reason, "connection reset by peer", "原因必须带回错误摘要")
	require.Equal(t, core.StatusFailed, h.store.runStatus(t, res.RunID))
}

// TestReActLoop_ModelErrorAfterCancelIsCancelled 守"取消不是失败"：
// 验收点（流程 B ③）——调用返回错误时**先看 ctx.Err()**：已取消判 cancelled，不判 failed。
func TestReActLoop_ModelErrorAfterCancelIsCancelled(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("不会走到这里"))
	h.provider.hook = func(call int) {
		if call == 1 {
			h.cancel() // 取消发生在模型调用期间
		}
	}
	h.provider.onError(1, context.Canceled)

	res, err := h.service.Run(ctx, h.req("读一下"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, res.Status, "取消优先：不得记为 failed")
	require.NotEmpty(t, res.Reason, "取消也要写明原因（文案归实现，这里只钉非空）")
}

// TestReActLoop_CancelBeforeBudgetCheck 守"取消优先于预算判定"：
// 验收点（契约 §4.1 / §4.4）——取消与预算同时成立时判 cancelled 而非 limit_reached，
// 组内未执行调用取"已取消"合成文本（**不是**预算文本）。
func TestReActLoop_CancelBeforeBudgetCheck(t *testing.T) {
	h := newLoopHarness(t)
	h.budget.MaxToolCalls = 1
	h.provider.onCall(0, toolCallResponses(
		toolCallResponse("c1", "read_file", `{"path":"a.txt"}`),
		toolCallResponse("c2", "read_file", `{"path":"b.txt"}`),
	)) // 一轮两个调用
	h.tools.onExecute = func(call int) { h.cancel() } // 第一个执行完即取消
	res, err := h.service.Run(ctx, h.req("读两个文件"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, res.Status, "取消优先于预算：不得记为 limit_reached")
	require.Equal(t, core.TextToolCancelled, h.store.lastToolContent(t, h.sessionID),
		"组内未执行调用取『已取消』合成文本")
}

// TestReActLoop_NoGoroutineLeakOnCancel 守"并发泄漏类回归"：
// 验收点（宪法 VII 同步执行）——取消后**拥有的资源**（工具子进程、收尾写入）全部退出；
// 必须**真正触发取消**（等工具已启动的信号，而不是"发完就取消"），并限时等待 Run 返回；
// goroutine 总数只作**辅助观察**（它会把测试框架自身的 goroutine 波动算进来，不能当主判据）。
func TestReActLoop_NoGoroutineLeakOnCancel(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	started := make(chan struct{})
	h.tools.blockUntilCancel = true
	h.tools.onStarted = func() { close(started) } // 工具已启动的信号
	before := runtime.NumGoroutine()
	done := make(chan struct{})
	var res core.RunResult
	var err error
	go func() { res, err = h.service.Run(ctx, h.req("跑一个慢工具")); close(done) }()
	<-started // 等"工具已启动"再取消——否则测不到取消路径
	h.cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("取消后 Run 未在限时内返回（取消传播链路断了）")
	}
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, res.Status, "Run 自身取消 → cancelled")
	// 限时等待"所拥有的资源"退出：工具子进程已回收、收尾写入已结束（用 harness 的等待点，不比 goroutine 总数）
	h.tools.waitExited(t, 2*time.Second) // 工具执行体已返回
	h.audit.waitIdle(t, 2*time.Second)   // 收尾审计写入已结束
	// 辅助观察：goroutine 数不高于基线 + 少量余量（辅助判据，不作为唯一断言）
	require.LessOrEqual(t, runtime.NumGoroutine(), before+2, "辅助观察：取消后不得残留成片 goroutine")
}

// TestReActLoop_AdmissionRejectionFailsRun 守"不可裁部分超限 = 拒绝启动"：
// 验收点（契约 §6 上下文预算）——超限返回 failed + 点名预算键，**零模型调用**、零工具执行。
func TestReActLoop_AdmissionRejectionFailsRun(t *testing.T) {
	h := newLoopHarness(t)
	h.budget.ContextTokens = 1 // 不可裁部分（段一 + 工具定义 + 当前消息）必然超限

	res, err := h.service.Run(ctx, h.req("喂"))
	require.NoError(t, err, "运行期失败归 Status")
	require.Equal(t, core.StatusFailed, res.Status)
	require.Contains(t, res.Reason, "budget.context_tokens", "原因必须点名上下文预算键")
	require.Zero(t, h.provider.callCount(), "拒绝启动 = 零模型调用")
	require.Zero(t, h.tools.executed, "拒绝启动 = 零工具执行")
}

// TestReActLoop_ToleratesEmptyContentWithToolCalls 守"空 content 带工具调用是常态"：
// 验收点（契约事实）——content 为空且有 tool_calls 必须被容忍并原样落库；
// usage 缺失（零值）不得判失败（审计侧的三列 0 由 ProviderService 用例覆盖）。
func TestReActLoop_ToleratesEmptyContentWithToolCalls(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, core.ChatResponse{ToolCalls: []core.ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"a.txt"}`}}})
	h.provider.onCall(1, textResponse("done"))

	res, err := h.service.Run(ctx, h.req("读一下"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	msgs := h.store.messages(t, h.sessionID)
	// user + assistant(带工具调用) + tool + assistant(最终回答)——**4** 条不是 3 条：
	// 最终回答也是助手消息，断言 3 条会把"丢了最终回答"判成通过（§4.3 回归点）。
	require.Len(t, msgs, 4, "user + assistant(带工具调用) + tool + assistant(最终回答)")
	require.Equal(t, core.RoleAssistant, msgs[1].Role)
	require.Empty(t, msgs[1].Content, "空 content 必须原样保留（不得补默认文本）")
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Equal(t, "c1", msgs[1].ToolCalls[0].ID, "工具调用 id 全链保真")
	require.Equal(t, core.RoleTool, msgs[2].Role)
	require.Equal(t, "c1", msgs[2].ToolCallID)
	require.Equal(t, core.RoleAssistant, msgs[3].Role)
	require.Equal(t, "done", msgs[3].Content, "最终回答必须落库")
}

// TestReActLoop_RunIDShape 守"运行标识形态"：
// 验收点（§6.2 固定口径 2）——`<渠道>-<毫秒>-<6 位 base36 小写>`，毫秒段取自注入的时钟。
func TestReActLoop_RunIDShape(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, textResponse("ok"))

	res, err := h.service.Run(ctx, h.req("喂"))
	require.NoError(t, err)
	pattern := regexp.MustCompile(`^cli-\d+-[0-9a-z]{6}$`)
	require.Regexp(t, pattern, res.RunID)
	require.Contains(t, res.RunID, fmt.Sprintf("cli-%d-", h.now.UnixMilli()),
		"毫秒段取自本次运行的时间（时钟注入、时间显式传入）")
}

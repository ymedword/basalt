package core_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是运行入口（US1 / 流程 A）的验收测试：准入不排队、证据先行（快照 → 账本 → 用户消息）、
// 终态列面、无状态会话。夹具复用 loop_test.go 的 harness，另加一个"记录终态列"的仓储包装
// （harness 自带的假仓储只记 status 与 reason，而 D8 要逐列断言）。
// This file is the acceptance test of the run entry point (flow A): admission never queues,
// evidence is written before the model call, and terminal columns are asserted one by one.

// recordingStore 包装 harness 的假仓储，额外记录 `FinishRun` 的列面与快照落库失败注入。
// 终态列的断言因此能落到"落库时到底传了什么"，而不是只看内存里的状态位。
// recordingStore wraps the harness store to record the FinishRun columns and to inject a
// snapshot-persist failure; assertions then cover what was actually written.
type recordingStore struct {
	*fakeStore

	mu    sync.Mutex
	iters map[string]int
	calls map[string]int
	at    map[string]time.Time

	failSnapshot error // 注入"快照落库失败"（拒绝启动用例）
}

// newRecordingStore 组装包装仓储（嵌入假仓储，三张记录表各自初始化）。
// newRecordingStore wraps a fresh fake store and initializes its column tables.
func newRecordingStore() *recordingStore {
	return &recordingStore{
		fakeStore: newFakeStore(),
		iters:     map[string]int{},
		calls:     map[string]int{},
		at:        map[string]time.Time{},
	}
}

// FinishRun 先记录列面再落库。
// FinishRun records the column surface before delegating to the wrapped store.
func (s *recordingStore) FinishRun(ctx context.Context, runID string, status core.RunStatus, reason string, iterations, toolCalls int, now time.Time) error {
	s.mu.Lock()
	s.iters[runID] = iterations
	s.calls[runID] = toolCalls
	s.at[runID] = now
	s.mu.Unlock()
	return s.fakeStore.FinishRun(ctx, runID, status, reason, iterations, toolCalls, now)
}

// SaveSnapshot 落快照（可注入失败）。
// SaveSnapshot persists the snapshot and can be made to fail on demand.
func (s *recordingStore) SaveSnapshot(ctx context.Context, snap *core.ConfigSnapshot) error {
	if s.failSnapshot != nil {
		return s.failSnapshot
	}
	return s.fakeStore.SaveSnapshot(ctx, snap)
}

// columns 取终态三列（iterations / tool_calls / finished_at）。
// columns returns the recorded terminal columns.
func (s *recordingStore) columns(runID string) (int, int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.iters[runID], s.calls[runID], s.at[runID]
}

// assemble 用 harness 的假件另装一个运行入口（`maxConcurrent <= 0` 用缺省 32），
// 并返回记录终态列的包装仓储。harness 主入口（h.service）仍供其它用例使用。
// assemble builds a separate entry point over the harness fakes, returning a store that
// records the terminal columns.
func (h *loopHarness) assemble(maxConcurrent int) (*core.AgentService, *recordingStore) {
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := newRecordingStore()
	svc := core.NewAgentService(core.ServiceOptions{
		Agents:        h.agents,
		Client:        h.provider,
		Messages:      store,
		Runs:          store,
		Snapshots:     store,
		Audit:         h.audit,
		Loop:          core.NewReActLoop(core.NewPromptBuilder(h.registry), core.NewToolExecutor(h.registry, h.audit, logger), logger),
		Logger:        logger,
		Now:           func() time.Time { return h.now },
		MaxConcurrent: maxConcurrent,
	})
	return svc, store
}

// sessionsOf 数会话行条数（无状态调用不得建行）。
// sessionsOf counts session rows; a stateless run must create none.
func (s *fakeStore) sessionsOf() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// snapshotsOf 数已落库的快照条数。
// snapshotsOf counts persisted snapshots.
func (s *fakeStore) snapshotsOf() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.snapshots)
}

// runsOf 数账本行条数。
// runsOf counts ledger rows.
func (s *fakeStore) runsOf() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// runRow 取账本起始行（断言落库的渠道 / 会话 / 配置哈希）。
// runRow returns the ledger's starting row.
func (s *fakeStore) runRow(t *testing.T, runID string) core.RunRecord {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.runs[runID]
	require.True(t, ok, "账本里没有 %s 行", runID)
	return rec
}

// runIDsOfUser 取某个会话里全部用户消息的落库 run_id 列（断言写入点唯一与关联正确）。
// runIDsOfUser returns the persisted run_id column of every user message in a session.
func (s *fakeStore) runIDsOfUser(sessionID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, m := range s.msgs[sessionID] {
		if m.msg.Role == core.RoleUser {
			out = append(out, m.runID)
		}
	}
	return out
}

// startBlockedRun 起一次"卡在工具里"的运行（不排队用例的在途运行），
// 返回"工具已启动"与"运行已返回"两个信号。
// startBlockedRun launches a run that blocks inside a tool and returns the started/done
// signals; the in-flight run is the fixture for the no-queueing cases.
func startBlockedRun(t *testing.T, h *loopHarness, svc *core.AgentService, sessionID string) (<-chan struct{}, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{})
	done := make(chan struct{})
	h.tools.onStarted = func() { close(started) }
	h.tools.blockUntilCancel = true
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))

	req := h.req("第一条")
	req.SessionID = sessionID
	go func() {
		defer close(done)
		_, _ = svc.Run(ctx, req)
	}()
	return started, done
}

// waitSignal 限时等待一个信号（不排队的判据是"立刻返回"，卡住即失败）。
// waitSignal waits for a signal with a deadline; a queued call would stall and fail here.
func waitSignal(t *testing.T, ch <-chan struct{}, d time.Duration, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatal(msg)
	}
}

// TestAgentService_IsTheSingleRunEntryPoint 守"唯一入口"：
// 验收点（D1）——一次运行的全部落库痕迹（快照 / 账本 / 消息 / 审计）都产生自
// `AgentService.Run` 这一条路径；测试不直接调用循环、执行器或任何仓储写入面。
func TestAgentService_IsTheSingleRunEntryPoint(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("done"))

	res, err := h.service.Run(ctx, h.req("读一下"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	require.Equal(t, 1, h.store.snapshotsOf(), "快照落库恰一份")
	require.Equal(t, 1, h.store.runsOf(), "账本恰一行")
	require.Equal(t, core.StatusCompleted, h.store.runStatus(t, res.RunID))
	require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleUser), "用户消息恰一条")
	require.Equal(t, 2, h.store.countByRole(t, h.sessionID, core.RoleAssistant), "助手消息恰两条（工具轮 + 最终回答）")
	require.Equal(t, 1, h.store.countToolInvocations(t), "工具审计恰一行")
	require.Equal(t, 2, h.provider.callCount())
}

// TestAgentService_UnknownAgentIsCallerError 守"未知 Agent 是调用方问题"：
// 验收点（D2）——返回 `*AgentNotFoundError`（**不是**运行状态 failed），且**不产生任何落痕**：
// 无快照、无账本、无会话、无消息、零模型调用、零工具执行。
func TestAgentService_UnknownAgentIsCallerError(t *testing.T) {
	h := newLoopHarness(t)

	req := h.req("喂")
	req.Agent = "ghost"
	res, err := h.service.Run(ctx, req)

	var notFound *core.AgentNotFoundError
	require.ErrorAs(t, err, &notFound, "未知 Agent 走函数级 error")
	require.Equal(t, "ghost", notFound.Name)
	require.Equal(t, core.RunResult{}, res, "调用方问题不产生运行结果")
	require.Zero(t, h.store.snapshotsOf(), "零快照")
	require.Zero(t, h.store.runsOf(), "零账本行（不产生半截落库）")
	require.Zero(t, h.store.sessionsOf(), "零会话行")
	require.Empty(t, h.store.messages(t, h.sessionID), "零消息")
	require.Zero(t, h.provider.callCount(), "零模型调用")
	require.Zero(t, h.tools.executed, "零工具执行")
}

// TestAgentService_BusySessionRejectedWithoutQueueing 守"同会话忙不排队"：
// 验收点（D3）——同一会话已有运行在跑时直接返回 `*SessionBusyError`，
// **限时**返回（排队会让调用方等到前一次跑完），且不产生任何落痕。
func TestAgentService_BusySessionRejectedWithoutQueueing(t *testing.T) {
	h := newLoopHarness(t)
	started, done := startBlockedRun(t, h, h.service, h.sessionID)
	waitSignal(t, started, 2*time.Second, "在途运行的工具未启动（夹具没就位）")
	defer func() {
		h.cancel()
		waitSignal(t, done, 3*time.Second, "取消后在途运行未返回")
	}()

	type outcome struct {
		res core.RunResult
		err error
	}
	second := make(chan outcome, 1)
	go func() {
		res, err := h.service.Run(ctx, h.req("第二条"))
		second <- outcome{res: res, err: err}
	}()

	select {
	case out := <-second:
		var busy *core.SessionBusyError
		require.ErrorAs(t, out.err, &busy, "同会话忙 = 调用方问题")
		require.Equal(t, h.sessionID, busy.SessionID)
		require.Equal(t, core.RunResult{}, out.res)
	case <-time.After(2 * time.Second):
		t.Fatal("同会话忙时 Run 未立即拒绝——这是排队，不是拒绝")
	}

	require.Equal(t, 1, h.store.runsOf(), "被拒的那次不得留下账本行")
	// 在途运行此刻已落了"提问 + 助手工具调用"，故判据是"被拒那条提问从未落库"：
	// 用户消息仍只有一条，且没有一条消息带着被拒的正文。
	require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleUser), "被拒的提问不得落库")
	for _, m := range h.store.messages(t, h.sessionID) {
		require.NotEqual(t, "第二条", m.Content, "被拒的正文不得出现在历史里")
	}
}

// TestAgentService_FullInstanceRejectedWithoutQueueing 守"实例满不排队"：
// 验收点（D4）——实例级并发上限（缺省 32，可配）达顶时直接返回 `*InstanceFullError`，
// **别的会话也拒**（会话锁管不到这条），且不产生任何落痕。
func TestAgentService_FullInstanceRejectedWithoutQueueing(t *testing.T) {
	h := newLoopHarness(t)
	svc, store := h.assemble(1) // 上限 1：在途一跑即满
	started, done := startBlockedRun(t, h, svc, "cli-1111111111111111")
	waitSignal(t, started, 2*time.Second, "在途运行的工具未启动（夹具没就位）")
	defer func() {
		h.cancel()
		waitSignal(t, done, 3*time.Second, "取消后在途运行未返回")
	}()

	type outcome struct {
		res core.RunResult
		err error
	}
	second := make(chan outcome, 1)
	go func() {
		req := h.req("另一条会话")
		req.SessionID = "cli-2222222222222222" // 不同会话：唯一会被拒的理由是实例满
		res, err := svc.Run(ctx, req)
		second <- outcome{res: res, err: err}
	}()

	select {
	case out := <-second:
		var full *core.InstanceFullError
		require.ErrorAs(t, out.err, &full, "实例满 = 调用方问题")
		require.Equal(t, 1, full.Limit, "上限读数原样带回")
		require.Equal(t, core.RunResult{}, out.res)
	case <-time.After(2 * time.Second):
		t.Fatal("实例满时 Run 未立即拒绝——这是排队，不是拒绝")
	}

	require.Equal(t, 1, store.runsOf(), "被拒的那次不得留下账本行")
	require.Empty(t, store.messages(t, "cli-2222222222222222"), "被拒的会话不得落消息")
}

// TestAgentService_EmptySessionIsStateless 守"空会话标识 = 无状态"：
// 验收点（D5）——不落会话历史、不建会话行，但该次运行的**账本与工具审计照常落库**
// （无状态说的是会话，不是证据）。
func TestAgentService_EmptySessionIsStateless(t *testing.T) {
	h := newLoopHarness(t)
	h.sessionID = "" // 无状态调用
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("done"))

	res, err := h.service.Run(ctx, h.req("一次性问答"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	require.Empty(t, h.store.messages(t, ""), "无状态会话不落历史")
	require.Zero(t, h.store.sessionsOf(), "无状态调用不建会话行")
	require.Equal(t, 1, h.store.runsOf(), "账本照常落库")
	require.Equal(t, core.StatusCompleted, h.store.runStatus(t, res.RunID))
	require.Equal(t, 1, h.store.countToolInvocations(t), "工具审计照常落库")
}

// TestAgentService_LedgerStartsBeforeModelCall 守"账本先落 running"：
// 验收点（D6）——第一次模型调用发生时，账本行已存在且为 `running`
// （先留痕再发出：调用已发出而账本没行，恢复扫描就无从收尾）。
func TestAgentService_LedgerStartsBeforeModelCall(t *testing.T) {
	h := newLoopHarness(t)
	var (
		rows     int
		statuses []core.RunStatus
	)
	h.provider.hook = func(int) {
		h.store.mu.Lock()
		defer h.store.mu.Unlock()
		rows = len(h.store.runs)
		for _, st := range h.store.status {
			statuses = append(statuses, st)
		}
	}
	h.provider.onCall(0, textResponse("马上好"))

	res, err := h.service.Run(ctx, h.req("在吗"))
	require.NoError(t, err)
	require.Equal(t, 1, rows, "模型调用发生时账本行已就位")
	require.Equal(t, []core.RunStatus{core.StatusRunning}, statuses, "此刻状态是 running")
	require.Equal(t, core.StatusCompleted, h.store.runStatus(t, res.RunID), "返回前已收口为终态")
}

// TestAgentService_UserMessageAppendPointIsUnique 守"用户消息写入点唯一"：
// 验收点（D7）——一次运行只写一条用户消息，落库 `run_id` 列指向本次运行，
// 且它在第一次模型调用之前就已落库（写失败即 fail-closed，不给模型看到没留痕的提问）。
func TestAgentService_UserMessageAppendPointIsUnique(t *testing.T) {
	h := newLoopHarness(t)
	var usersAtCall []string
	h.provider.hook = func(int) {
		usersAtCall = h.store.runIDsOfUser(h.sessionID)
	}
	h.provider.onCall(0, textResponse("好"))

	res, err := h.service.Run(ctx, h.req("问一句"))
	require.NoError(t, err)

	require.Len(t, usersAtCall, 1, "模型调用时用户消息已落库且只有一条")
	require.Equal(t, res.RunID, usersAtCall[0], "落库 run_id 指向本次运行")
	require.Equal(t, []string{res.RunID}, h.store.runIDsOfUser(h.sessionID), "收尾后仍只有一条")
}

// TestAgentService_TerminalColumnsOnLedger 守"终态落库列正确"：
// 验收点（D8）——终态写回 status / reason / iterations / tool_calls / finished_at，
// 且**没有** final_text 列：最终回答只存在于返回结果与会话的助手消息里。
func TestAgentService_TerminalColumnsOnLedger(t *testing.T) {
	h := newLoopHarness(t)
	svc, store := h.assemble(0)
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("读完了，是 a 文件"))

	res, err := svc.Run(ctx, h.req("读一下"))
	require.NoError(t, err)

	iterations, toolCalls, at := store.columns(res.RunID)
	require.Equal(t, 2, iterations, "两轮：工具轮 + 最终回答轮")
	require.Equal(t, 1, toolCalls)
	require.Equal(t, h.now, at, "finished_at 取注入时钟")
	require.Equal(t, core.StatusCompleted, store.runStatus(t, res.RunID))
	require.Empty(t, store.runReason(t, res.RunID), "正常完成不填原因")

	// 列面没有 final_text：账本行里不含最终文本。
	row := store.runRow(t, res.RunID)
	require.NotContains(t, fmt.Sprintf("%+v", row), "读完了，是 a 文件", "最终文本不进账本")
	// 它落在会话的助手消息里（与返回结果同源）。
	msgs := store.messages(t, h.sessionID)
	require.Equal(t, "读完了，是 a 文件", msgs[len(msgs)-1].Content)
	require.Equal(t, res.FinalText, msgs[len(msgs)-1].Content)
}

// TestAgentService_RunIDShapeAndFrozenClock 守"运行标识形态"：
// 验收点（D9）——`<渠道>-<毫秒时间戳>-<6 位 base36 小写>`；时间戳取**注入时钟**
// （业务时间显式传入，测试因此可复现）；账本键与返回的标识一致。
func TestAgentService_RunIDShapeAndFrozenClock(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, textResponse("好"))

	req := h.req("在吗")
	req.Channel = "scheduler"
	res, err := h.service.Run(ctx, req)
	require.NoError(t, err)

	pattern := regexp.MustCompile(`^scheduler-\d{13}-[0-9a-z]{6}$`)
	require.Regexp(t, pattern, res.RunID, "形态：渠道-毫秒-6 位 base36 小写")
	require.Contains(t, res.RunID, fmt.Sprintf("-%d-", h.now.UnixMilli()), "毫秒段取注入时钟")
	require.Equal(t, "scheduler", h.store.runRow(t, res.RunID).Channel, "账本同键落行")
}

// TestAgentService_SnapshotPersistedBeforeRunStart 守"快照先落库再开跑"：
// 验收点（D10）——第一次模型调用时快照已落库；快照**读失败**或**落库失败**都拒绝启动：
// Run failed + 点名错误、零模型调用、零工具执行、零账本行（连账本都还没轮到）。
func TestAgentService_SnapshotPersistedBeforeRunStart(t *testing.T) {
	t.Run("persisted_first", func(t *testing.T) {
		h := newLoopHarness(t)
		snapsAtCall := -1
		h.provider.hook = func(int) { snapsAtCall = h.store.snapshotsOf() }
		h.provider.onCall(0, textResponse("好"))

		_, err := h.service.Run(ctx, h.req("在吗"))
		require.NoError(t, err)
		require.Equal(t, 1, snapsAtCall, "模型调用发生时快照已落库")
	})

	t.Run("save_failure_refuses", func(t *testing.T) {
		h := newLoopHarness(t)
		svc, store := h.assemble(0)
		store.failSnapshot = errors.New("injected: snapshot store unavailable")

		res, err := svc.Run(ctx, h.req("在吗"))
		require.NoError(t, err, "运行期失败归 Status")
		require.Equal(t, core.StatusFailed, res.Status)
		require.Contains(t, res.Reason, "snapshot store unavailable", "原因如实带回")
		require.NotEmpty(t, res.RunID, "失败结果也带运行标识")
		require.Zero(t, h.provider.callCount(), "拒绝启动 = 零模型调用")
		require.Zero(t, h.tools.executed, "拒绝启动 = 零工具执行")
		require.Zero(t, store.runsOf(), "拒绝启动 = 零账本行")
	})

	t.Run("read_failure_refuses", func(t *testing.T) {
		h := newLoopHarness(t)
		h.agents.failSnapshot = errors.New("injected: AGENT.md unreadable")

		res, err := h.service.Run(ctx, h.req("在吗"))
		require.NoError(t, err)
		require.Equal(t, core.StatusFailed, res.Status)
		require.Contains(t, res.Reason, "AGENT.md unreadable")
		require.Zero(t, h.provider.callCount(), "零模型调用")
		require.Zero(t, h.store.snapshotsOf(), "读失败不落半份快照")
	})
}

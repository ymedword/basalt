package core_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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
	return h.assembleWith(maxConcurrent, nil)
}

// assembleWith 是 assemble 的可选会话门禁版本：sessions 在构造期并入 ServiceOptions；
// assemble(n) 等价于 assembleWith(n, nil)——既有签名与全部既有调用不变（等价重构）。
// assembleWith is the session-gate variant of assemble; assemble(n) equals
// assembleWith(n, nil), so existing signatures and call sites stay unchanged.
func (h *loopHarness) assembleWith(maxConcurrent int, sessions core.SessionStatusStore) (*core.AgentService, *recordingStore) {
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := newRecordingStore()
	svc := core.NewAgentService(core.ServiceOptions{
		Agents:        h.agents,
		Client:        h.provider,
		Messages:      store,
		Runs:          store,
		Snapshots:     store,
		Audit:         h.audit,
		Memory:        h.memory, // 随已批准接口适配（默认 nil = 段 2 恒空；判据不变）
		Loop:          core.NewReActLoop(core.NewPromptBuilder(h.registry), core.NewToolExecutor(h.registry, h.audit, logger), logger),
		Logger:        logger,
		Now:           func() time.Time { return h.now },
		MaxConcurrent: maxConcurrent,
		Sessions:      sessions,
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

// —— 段 2 记忆注入（US-6）——

// failingMemoryReader 是 `Load` 恒失败的记忆端口替身——真文件造"读取失败"无法跨平台
// 稳定复现（权限 / I/O 各平台形态不同），故用桩注入（与 memory 包的 `errorStore` 同理由）。
// failingMemoryReader always fails; a real I/O failure cannot be reproduced portably.
type failingMemoryReader struct {
	err error
}

// LoadEffective 恒返回注入错误。
// LoadEffective always returns the injected error.
func (r failingMemoryReader) LoadEffective(context.Context, string) (string, error) {
	return "", r.err
}

// R1 —— 段 2 注入 + 空记忆逐字节回归（core 层）。
// 逆改点：把段 2 塞进 History / Tools 而不是 System → 第一组断言红。
// 注：三组用的是**相对基线**（nil 端口组的现场读数），故"Build 给 System 加固定前后缀"会让
// 基线与实验组同步平移、**本测试不红**——那条逆改由 TestAgentService_NilMemoryLeavesSegmentTwoEmpty
// 的**绝对字面量**断言（sectionOneStatic）承载；两者经传递性合起来仍钉死"空记忆零痕迹"
// （T015 逆改实测：该条逐字红 `expected: "段一：你是 demo。" actual: "[逆改固定前缀]段一：你是 demo。"`）。
func TestAgentService_InjectsCoreMemorySegment(t *testing.T) {
	const coreText = "## 核心记忆\n\n- 用户偏好简洁回答"

	// 基线：不接记忆端口（nil）——US-4 的既有形状。
	base := newLoopHarness(t)
	base.provider.onCall(0, textResponse("好"))
	_, err := base.service.Run(ctx, base.req("你好"))
	require.NoError(t, err)
	baseline := base.provider.request(0).Messages[0].Content

	// 有记忆：段一 + 两换行 + 核心区文本（端口**只能在构造期**经 withMemory 置入）。
	mem := newLoopHarness(t, withMemory(fakeMemoryReader{text: coreText}))
	mem.provider.onCall(0, textResponse("好"))
	_, err = mem.service.Run(ctx, mem.req("你好"))
	require.NoError(t, err)
	require.Equal(t, baseline+"\n\n"+coreText, mem.provider.request(0).Messages[0].Content)

	// 空记忆：与基线**逐字节**相同（段 2 不产生任何文本）。
	empty := newLoopHarness(t, withMemory(fakeMemoryReader{text: ""}))
	empty.provider.onCall(0, textResponse("好"))
	_, err = empty.service.Run(ctx, empty.req("你好"))
	require.NoError(t, err)
	require.Equal(t, baseline, empty.provider.request(0).Messages[0].Content,
		"空记忆不得在系统提示词里留下任何痕迹")
}

// R2 —— 运行内冻结：整次运行只读一次记忆，第 2 轮不得看到"改后"文本（core 层）。
// 逆改点：把读取挪进每轮组装（Build 内重读）→ 第 2 轮入站 system 变成"- 新偏好"，断言红。
func TestAgentService_MemoryReadOncePerRun(t *testing.T) {
	reader := &scriptedMemoryReader{texts: []string{"- 旧偏好", "- 新偏好"}}
	h := newLoopHarness(t, withMemory(reader)) // 端口在构造期置入
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"AGENT.md"}`))
	h.provider.onCall(1, textResponse("好"))

	_, err := h.service.Run(ctx, h.req("你好"))
	require.NoError(t, err)
	require.Equal(t, 1, reader.readCount(), "每次运行只读一次（运行内冻结）")
	for i := 0; i < 2; i++ {
		require.Contains(t, h.provider.request(i).Messages[0].Content, "- 旧偏好")
		require.NotContains(t, h.provider.request(i).Messages[0].Content, "- 新偏好")
	}
}

// TestAgentService_MemoryReadFailureFailsRunBeforeModelCall 守"读取失败 = 拒绝启动"：
// 验收点（约束 23）——与段一读取失败同口径：`StatusFailed` + 零 LLM 调用 + 零工具执行，
// 失败形状沿 US-4 既有 `StatusFailed`（错误文本如实带回 Reason，不另立一套形状）。
func TestAgentService_MemoryReadFailureFailsRunBeforeModelCall(t *testing.T) {
	h := newLoopHarness(t, withMemory(failingMemoryReader{err: errors.New("injected: memory unreadable")}))

	res, err := h.service.Run(ctx, h.req("在吗"))
	require.NoError(t, err, "运行期失败归 Status，不是 Go error")
	require.Equal(t, core.StatusFailed, res.Status)
	require.Contains(t, res.Reason, "memory unreadable", "原因如实带回")
	require.NotEmpty(t, res.RunID, "失败结果也带运行标识")
	require.Zero(t, h.provider.callCount(), "拒绝启动 = 零模型调用")
	require.Zero(t, h.tools.executed, "拒绝启动 = 零工具执行")
}

// TestAgentService_NilMemoryLeavesSegmentTwoEmpty 守"nil 端口容错"：
// 验收点（约束 22）——`ServiceOptions.Memory == nil` 是**正常装配**（可空端口）：运行照常
// 完成，系统提示词与无记忆时逐字节相同（既有的 US-4 行为基线由此再钉一次）。
func TestAgentService_NilMemoryLeavesSegmentTwoEmpty(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, textResponse("好"))

	res, err := h.service.Run(ctx, h.req("你好"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, sectionOneStatic, h.provider.request(0).Messages[0].Content,
		"nil 端口 = 段 2 不产生任何文本（与 US-4 逐字节相同）")
}

// R4b —— 注入后超上下文预算 → 拒绝启动、Reason 点名预算键、零 LLM（core 层）。
// 预算与记忆两端**都取小值**（P1-2 修正）：旧值 `Repeat("- 偏好\n", 3000)` 只有 27 KB、
// 估不出 32768 的缺省预算——用例恒绿、实现再错也不红，等于没判。现在把预算压到 8192
// （可裁面 = 8192 - core.ReserveTokens 1024 = 7168），并加**对照组**：同一预算、不接记忆必须放行
// ——"闸门是不是真的接在记忆上"由此钉死，而不是靠一个可能本来就超限的预算蒙对。
// 逆改点：把超限改成"截断后继续跑" → 实验组 Status 非 failed 且 provider 被调用，两条断言皆红；
// 把记忆漏接进 Build（`env.memory` 没喂给段 2）→ 实验组反而跑通，同样红。
func TestAgentService_OversizedMemoryFailsRunBeforeModelCall(t *testing.T) {
	const contextTokens = 8192

	// 对照组：同一预算、不接记忆端口（nil）→ 正常跑完（本预算本身不构成超限）。
	control := newLoopHarness(t)
	control.budget.ContextTokens = contextTokens
	control.provider.onCall(0, textResponse("好"))
	res, err := control.service.Run(ctx, control.req("你好"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "对照组：同预算、无记忆必须放行")
	require.Equal(t, 1, control.provider.callCount())

	// 实验组：同一预算 + 90000 字节记忆（estimateText 下 ≥ 22000 tokens，见 prompt.go）→
	// 拒绝启动、Reason 点名预算键、零模型调用。
	over := newLoopHarness(t, withMemory(fakeMemoryReader{text: strings.Repeat("- 偏好\n", 10000)}))
	over.budget.ContextTokens = contextTokens
	res, err = over.service.Run(ctx, over.req("你好"))
	require.NoError(t, err, "运行期失败归 Status，不是 Go error")
	require.Equal(t, core.StatusFailed, res.Status)
	require.Contains(t, res.Reason, "budget.context_tokens", "原因必须点名上下文预算键")
	require.Zero(t, over.provider.callCount(), "拒绝启动 = 零模型调用")
}

// —— 会话门禁与 Run ID / Trigger 传导（US-10）——

// fakeSessionStatusStore 是 `core.SessionStatusStore` 的内存实现：会话行按 id 存，
// 归档幂等（已归档再归档保留首次 `archived_at`），未命中返回 fs.ErrNotExist。
// fakeSessionStatusStore is the in-memory SessionStatusStore: archive is idempotent and
// keeps the first archived_at; a miss returns fs.ErrNotExist.
type fakeSessionStatusStore struct {
	mu   sync.Mutex
	rows map[string]core.SessionRecord
}

// newFakeSessionStatusStore 组装空的门禁替身。
func newFakeSessionStatusStore() *fakeSessionStatusStore {
	return &fakeSessionStatusStore{rows: map[string]core.SessionRecord{}}
}

// put 预置一行会话（状态由调用方给）。
func (f *fakeSessionStatusStore) put(rec core.SessionRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[rec.ID] = rec
}

// GetSession 取会话行（未命中 fs.ErrNotExist）。
func (f *fakeSessionStatusStore) GetSession(_ context.Context, id string) (core.SessionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.rows[id]
	if !ok {
		return core.SessionRecord{}, fs.ErrNotExist
	}
	return rec, nil
}

// ArchiveSession 置归档（未命中 fs.ErrNotExist；已归档保留首次时间）。
// ArchiveSession marks the row archived; archiving twice keeps the first timestamp.
func (f *fakeSessionStatusStore) ArchiveSession(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.rows[id]
	if !ok {
		return fs.ErrNotExist
	}
	if rec.Status != "archived" {
		rec.Status = "archived"
		stamp := at
		rec.ArchivedAt = &stamp
	}
	f.rows[id] = rec
	return nil
}

// statusOf 取会话状态字。
func (f *fakeSessionStatusStore) statusOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id].Status
}

// archivedAtOf 取归档时间戳（未归档为 nil）。
func (f *fakeSessionStatusStore) archivedAtOf(id string) *time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[id].ArchivedAt
}

// TestAgentService_Run_UsesCallerRunID 守"调用方 RunID 原样使用"（约束 14）：
// 调度器预生成的 `scheduler-` 标识经 RunRequest 传入后不得被再次生成覆盖。
// 逆改点：沿用 `NewRunID(...)` 无条件生成 → 账本行键与调用方值不符，断言红。
func TestAgentService_Run_UsesCallerRunID(t *testing.T) {
	h := newLoopHarness(t)
	svc, store := h.assemble(0)
	h.provider.onCall(0, textResponse("好"))

	const callerRunID = "scheduler-1760000000000-abc123"
	req := h.req("在吗")
	req.Channel = "scheduler"
	req.RunID = callerRunID
	res, err := svc.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, callerRunID, res.RunID, "非空 RunID 必须原样使用")
	require.Equal(t, callerRunID, store.runRow(t, callerRunID).RunID, "账本行以调用方 RunID 为键")
}

// TestAgentService_Run_DefaultRunIDUnchanged 守"缺省生成规则不变"（约束 14 回归）：
// `RunID` 为空时仍由 AgentService 按既有 `渠道-毫秒-base36` 形态生成。
// 逆改点：把"空值 → 固定前缀"或另换形态 → 正则断言红。
func TestAgentService_Run_DefaultRunIDUnchanged(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, textResponse("好"))

	res, err := h.service.Run(ctx, h.req("在吗"))
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^cli-\d{13}-[0-9a-z]{6}$`), res.RunID,
		"空 RunID 沿用既有生成形态（渠道-毫秒-6 位 base36 小写）")
	require.Contains(t, res.RunID, fmt.Sprintf("-%d-", h.now.UnixMilli()), "毫秒段取注入时钟")
}

// TestAgentService_Run_PassesTriggerToLedger 守"触发方式进账本"（约束 13 / 14）：
// `RunRequest.Trigger` 原样写入 `RunRecord`，仓储据此落 `runs.trigger`。
// 逆改点：StartRun 不传 Trigger → 账本行 Trigger 为空，断言红。
func TestAgentService_Run_PassesTriggerToLedger(t *testing.T) {
	h := newLoopHarness(t)
	svc, store := h.assemble(0)
	h.provider.onCall(0, textResponse("好"))

	const callerRunID = "scheduler-1760000000001-manual"
	req := h.req("补跑")
	req.Channel = "scheduler"
	req.RunID = callerRunID
	req.Trigger = core.TriggerManual
	_, err := svc.Run(ctx, req)
	require.NoError(t, err)
	require.Equal(t, core.TriggerManual, store.runRow(t, callerRunID).Trigger,
		"显式 Trigger 必须进入 RunRecord")
}

// TestAgentService_Archive_RejectsInflightRun 守"归档与在途运行互斥"（约束 40 / HIGH-1）：
// ① 运行在途时归档 → *SessionBusyError，会话状态不变；② 运行结束后归档成功、状态落 archived。
// 逆改点：Archive 不查 busy 集合 → 在途时返回 nil，断言红。
func TestAgentService_Archive_RejectsInflightRun(t *testing.T) {
	h := newLoopHarness(t)
	sessions := newFakeSessionStatusStore()
	sessions.put(core.SessionRecord{ID: h.sessionID, Status: "active"})
	svc, _ := h.assembleWith(0, sessions)
	started, done := startBlockedRun(t, h, svc, h.sessionID)

	waitSignal(t, started, 2*time.Second, "在途运行未启动")
	var busy *core.SessionBusyError
	require.ErrorAs(t, svc.Archive(context.Background(), h.sessionID), &busy, "在途运行期间归档必须被拒")
	require.Equal(t, "active", sessions.statusOf(h.sessionID), "被拒的归档不得改状态")

	h.cancelRun() // 工具阻塞在 ctx 取消上，运行随之收尾
	waitSignal(t, done, 2*time.Second, "取消后在途运行未返回")
	require.NoError(t, svc.Archive(context.Background(), h.sessionID), "运行结束后归档应成功")
	require.Equal(t, "archived", sessions.statusOf(h.sessionID))
}

// TestAgentService_Archive_Idempotent 守"重复归档幂等"（US-9 既有语义换承载点）：
// 第二次归档返回 nil，`archived_at` 保留首次值。
// 逆改点：每次都覆盖时间戳 → 时间断言红；第二次报错 → 错误断言红。
func TestAgentService_Archive_Idempotent(t *testing.T) {
	h := newLoopHarness(t)
	sessions := newFakeSessionStatusStore()
	sessions.put(core.SessionRecord{ID: h.sessionID, Status: "active"})
	svc, _ := h.assembleWith(0, sessions)

	require.NoError(t, svc.Archive(context.Background(), h.sessionID))
	first := sessions.archivedAtOf(h.sessionID)
	require.NotNil(t, first, "首次归档必须落 archived_at")

	h.now = h.now.Add(time.Minute) // 第二次归档使用不同时刻
	require.NoError(t, svc.Archive(context.Background(), h.sessionID), "重复归档幂等")
	require.NotNil(t, sessions.archivedAtOf(h.sessionID))
	require.True(t, first.Equal(*sessions.archivedAtOf(h.sessionID)), "archived_at 保留首次值")
}

// TestAgentService_Run_ArchivedSessionRejected 守"归档判定在服务层"（约束 40）：
// 已归档会话的 Run 一律 *SessionArchivedError，**不论** RequireExistingSession 取值——
// cron / 手动触发因此不依赖 HTTP 层也能被拒（零模型调用、零账本行）。
// 逆改点：归档判定只在 HTTP 层 → 本用例红。
func TestAgentService_Run_ArchivedSessionRejected(t *testing.T) {
	h := newLoopHarness(t)
	sessions := newFakeSessionStatusStore()
	sessions.put(core.SessionRecord{ID: h.sessionID, Status: "archived"})
	svc, store := h.assembleWith(0, sessions)

	var archived *core.SessionArchivedError
	_, err := svc.Run(ctx, h.req("在吗"))
	require.ErrorAs(t, err, &archived, "归档会话必须被服务层拒绝")
	require.Equal(t, h.sessionID, archived.SessionID)
	require.Zero(t, h.provider.callCount(), "拒绝发生在模型调用之前")
	require.Zero(t, store.runsOf(), "拒绝不落账本行")
}

// TestAgentService_Run_RequireExistingSession 守"要求既有会话"的两分支：
// ① 缺失 + 要求既有 → *SessionNotFoundError（零模型调用）；② 缺失 + 未要求 → 自动创建并跑通。
// 逆改点：缺会话一律拒绝 → ② 红；要求既有被忽略 → ① 红。
func TestAgentService_Run_RequireExistingSession(t *testing.T) {
	t.Run("missing_and_required_rejected", func(t *testing.T) {
		h := newLoopHarness(t)
		sessions := newFakeSessionStatusStore()
		svc, _ := h.assembleWith(0, sessions)

		req := h.req("在吗")
		req.RequireExistingSession = true
		var notFound *core.SessionNotFoundError
		_, err := svc.Run(ctx, req)
		require.ErrorAs(t, err, &notFound, "缺失且要求既有 → 调用方错误")
		require.Equal(t, h.sessionID, notFound.SessionID)
		require.Zero(t, h.provider.callCount(), "拒绝发生在模型调用之前")
	})

	t.Run("missing_and_free_created", func(t *testing.T) {
		h := newLoopHarness(t)
		sessions := newFakeSessionStatusStore()
		svc, store := h.assembleWith(0, sessions)
		h.provider.onCall(0, textResponse("好"))

		res, err := svc.Run(ctx, h.req("在吗"))
		require.NoError(t, err, "未要求既有时会话缺失不构成调用方错误")
		require.Equal(t, core.StatusCompleted, res.Status)
		require.Equal(t, 1, store.sessionsOf(), "会话行由 Run 自动创建")
	})
}

// TestAgentService_StatelessRunsNotSerialized 守"无状态调用不互相串行"（约束 40 / R10-M9）：
// 两条并发 `SessionID == ""` 的运行都必须跑通、零 *SessionBusyError。
// 逆改点：门禁不判空会话 → 第二次准入看到 busy[""]，本用例红。
//
// 判别力来自模型调用钩子：第一次运行进入模型调用后（准入已完成）才发起第二次，
// 第二次**必须**能顺利通过准入并完成——串行化实现会当场拒绝它。
func TestAgentService_StatelessRunsNotSerialized(t *testing.T) {
	h := newLoopHarness(t)
	sessions := newFakeSessionStatusStore()
	svc, _ := h.assembleWith(0, sessions)

	firstCall := make(chan struct{})
	release := make(chan struct{})
	var hookOnce sync.Once
	h.provider.hook = func(i int) {
		if i == 0 {
			hookOnce.Do(func() { close(firstCall) })
			<-release
		}
	}
	h.provider.onCall(0, textResponse("一"))
	h.provider.onCall(1, textResponse("二"))

	var wg sync.WaitGroup
	var errFirst, errSecond error
	wg.Add(2)
	go func() {
		defer wg.Done()
		req := h.req("无状态一")
		req.SessionID = ""
		_, errFirst = svc.Run(context.Background(), req)
	}()
	go func() {
		defer wg.Done()
		<-firstCall // 第一次运行已通过准入并进入模型调用
		req := h.req("无状态二")
		req.SessionID = ""
		_, errSecond = svc.Run(context.Background(), req)
		close(release) // 第二次准入已完成，放行第一次
	}()
	wg.Wait()

	require.NoError(t, errFirst)
	require.NoError(t, errSecond, "空会话调用不得因忙集互斥被拒")
	var busy *core.SessionBusyError
	require.False(t, errors.As(errSecond, &busy), "零 SessionBusyError")
}

// TestAgentService_Archive_WithoutSessionStore 守"未装配门禁是装配缺陷"（约束 40 / R10-H1）：
// `ServiceOptions.Sessions == nil` 时 Archive 返回 *SessionGateUnavailableError——
// 不 nil 解引用、不静默成功。
// 逆改点：直接调 opts.Sessions.ArchiveSession → panic，测试红。
func TestAgentService_Archive_WithoutSessionStore(t *testing.T) {
	h := newLoopHarness(t)
	svc, _ := h.assemble(0)

	var unavailable *core.SessionGateUnavailableError
	require.ErrorAs(t, svc.Archive(context.Background(), h.sessionID), &unavailable)
}

// —— US-11：生命周期门禁（图 F ③，约束 15）——
// Lifecycle gate at admission (flow F ③, constraint 15).

// fakeLifecycle 是状态文件端口的测试替身：读面可脚本化（active / disabled / archived /
// 缺失 / 损坏），写面一律返回冲突——T017 只落读面，测试若意外走写路径应立刻显形。
// fakeLifecycle scripts the read face (four states plus missing and corrupt) and refuses
// every write: this task lands the read face only.
type fakeLifecycle struct {
	mu      sync.Mutex
	state   core.AgentState
	exists  bool
	readErr error
	reads   int
	onRead  func()
}

// newFakeLifecycle 造一个"存在且为给定状态"的替身。
// newFakeLifecycle builds an existing state file in the given state.
func newFakeLifecycle(state core.AgentState) *fakeLifecycle {
	return &fakeLifecycle{state: state, exists: true}
}

// setState 模拟另一次迁移改写状态文件（调用方负责持写锁）。
// setState mimics another transition rewriting the file under the write lock.
func (f *fakeLifecycle) setState(state core.AgentState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
}

// readCount 是已发生的读取次数。
// readCount reports how many reads happened.
func (f *fakeLifecycle) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// Read 按脚本返回状态；onRead 在返回前调用（用于探测与写锁的重叠）。
// Read returns the scripted state; onRead runs before returning so tests can probe
// overlap with the write lock.
func (f *fakeLifecycle) Read(_ context.Context, agent string) (core.LifecycleRecord, bool, error) {
	f.mu.Lock()
	f.reads++
	state, exists, err, hook := f.state, f.exists, f.readErr, f.onRead
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return core.LifecycleRecord{}, false, err
	}
	if !exists {
		return core.LifecycleRecord{}, false, nil
	}
	return core.LifecycleRecord{
		SchemaVersion: 2,
		Agent:         agent,
		AgentUID:      "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
		State:         state,
		Generation:    1,
	}, true, nil
}

// Transition 不在读面范围内：返回冲突，避免测试掩盖"误走写路径"。
// Transition is out of scope for the read face and reports a conflict.
func (f *fakeLifecycle) Transition(context.Context, string, core.AgentState, int64, core.AgentState, string) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, core.ErrLifecycleConflict
}

// Bootstrap 不在读面范围内：返回冲突。
// Bootstrap is out of scope for the read face and reports a conflict.
func (f *fakeLifecycle) Bootstrap(context.Context, string, string) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, core.ErrLifecycleConflict
}

// BootstrapIdentity 不在读面范围内：返回冲突（归档恢复路径由 app 层用例覆盖）。
// BootstrapIdentity is out of scope for the read face and reports a conflict.
func (f *fakeLifecycle) BootstrapIdentity(context.Context, string, core.AgentUID, string) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, core.ErrLifecycleConflict
}

// Discard 不在读面范围内：返回冲突。
// Discard is out of scope for the read face and reports a conflict.
func (f *fakeLifecycle) Discard(context.Context, string, string) error {
	return core.ErrLifecycleConflict
}

// assembleWithLifecycle 在与既有用例相同的假件上装一个带生命周期门禁的运行入口。
// assembleWithLifecycle builds a second entry point over the shared fakes, wired with the
// lifecycle port and the agent gate.
func (h *loopHarness) assembleWithLifecycle(lifecycle core.LifecyclePort, gate *core.AgentGate) (*core.AgentService, *recordingStore) {
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := newRecordingStore()
	svc := core.NewAgentService(core.ServiceOptions{
		Agents:    h.agents,
		Client:    h.provider,
		Messages:  store,
		Runs:      store,
		Snapshots: store,
		Audit:     h.audit,
		Memory:    h.memory,
		Loop:      core.NewReActLoop(core.NewPromptBuilder(h.registry), core.NewToolExecutor(h.registry, h.audit, logger), logger),
		Logger:    logger,
		Now:       func() time.Time { return h.now },
		Lifecycle: lifecycle,
		Gate:      gate,
	})
	return svc, store
}

// TestAgentService_Lifecycle_ActiveAdmitsRun 守"只有 active 放行"：状态为 active 时一次
// 运行正常完成，且状态文件在准入时被读了恰一次（不是从目录 / 索引推断）。
// 逆改点：跳过门禁 → 本用例仍绿（无判别力），由其余五例提供反证；此处守正向路径。
func TestAgentService_Lifecycle_ActiveAdmitsRun(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, textResponse("好"))
	lifecycle := newFakeLifecycle(core.AgentActive)
	svc, store := h.assembleWithLifecycle(lifecycle, core.NewAgentGate())

	res, err := svc.Run(ctx, h.req("在吗"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, 1, lifecycle.readCount(), "准入必须在现场读状态文件一次")
	require.Equal(t, 1, store.runsOf(), "放行后账本照常预写")
	require.Equal(t, 1, h.provider.callCount())
}

// TestAgentService_Lifecycle_DisabledRejected 守"停用是硬门"：状态为 disabled 时运行被
// 拒绝（409 AGENT_UNAVAILABLE），且发生在任何落痕之前——零快照、零账本、零模型调用。
// 逆改点：把状态缓存成 active → 本用例红（运行开始落账）。
func TestAgentService_Lifecycle_DisabledRejected(t *testing.T) {
	h := newLoopHarness(t)
	lifecycle := newFakeLifecycle(core.AgentDisabled)
	svc, store := h.assembleWithLifecycle(lifecycle, core.NewAgentGate())

	_, err := svc.Run(ctx, h.req("在吗"))
	var unavailable *core.AgentUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, core.AgentDisabled, unavailable.State)
	require.Contains(t, err.Error(), "disabled")
	require.Contains(t, err.Error(), "enable", "文案必须给出修复入口")

	require.Zero(t, store.snapshotsOf(), "拒绝必须发生在快照落库之前")
	require.Zero(t, store.runsOf(), "拒绝必须发生在账本预写之前")
	require.Zero(t, h.provider.callCount(), "拒绝必须发生在模型调用之前")
}

// TestAgentService_Lifecycle_ArchivedRejected 守"归档同样是硬门"：任何入口（HTTP / CLI /
// 调度）都经同一准入判定，归档态拒绝新运行。
// 逆改点：只判 disabled → 本用例红。
func TestAgentService_Lifecycle_ArchivedRejected(t *testing.T) {
	h := newLoopHarness(t)
	lifecycle := newFakeLifecycle(core.AgentArchived)
	svc, store := h.assembleWithLifecycle(lifecycle, core.NewAgentGate())

	_, err := svc.Run(ctx, h.req("在吗"))
	var unavailable *core.AgentUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, core.AgentArchived, unavailable.State)
	require.Contains(t, err.Error(), "archived")
	require.Zero(t, store.runsOf())
}

// TestAgentService_Lifecycle_MissingStateRejected 守"缺失 = 失败关闭"：状态文件不存在时
// 拒绝新运行、同时记 ERROR；`errors.Is(err, fs.ErrNotExist)` 经 Unwrap 仍可判定（底层错误
// 不进文案）。
// 逆改点：把缺失读成 active（目录存在即放行）→ 本用例红。
func TestAgentService_Lifecycle_MissingStateRejected(t *testing.T) {
	h := newLoopHarness(t)
	lifecycle := &fakeLifecycle{exists: false, readErr: fs.ErrNotExist}
	svc, store := h.assembleWithLifecycle(lifecycle, core.NewAgentGate())

	_, err := svc.Run(ctx, h.req("在吗"))
	var unavailable *core.AgentUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Empty(t, unavailable.State, "缺失时没有已知状态")
	require.True(t, errors.Is(err, fs.ErrNotExist), "底层错误必须经 Unwrap 保留")
	require.Contains(t, err.Error(), "missing")
	require.Contains(t, h.logs.String(), "level=ERROR", "缺失必须同时记 ERROR")
	require.Contains(t, h.logs.String(), "生命周期状态文件缺失")
	require.Zero(t, store.runsOf())
}

// TestAgentService_Lifecycle_CorruptStateRejected 守"损坏 = 失败关闭"：读取失败（校验和不符
// 等）同样拒绝，不得回退为 active；损坏与缺失在文案上可区分。
// 逆改点：吞掉读取错误按 active 放行 → 本用例红。
func TestAgentService_Lifecycle_CorruptStateRejected(t *testing.T) {
	h := newLoopHarness(t)
	corrupt := errors.New("lifecycle checksum mismatch")
	lifecycle := &fakeLifecycle{exists: false, readErr: corrupt}
	svc, store := h.assembleWithLifecycle(lifecycle, core.NewAgentGate())

	_, err := svc.Run(ctx, h.req("在吗"))
	var unavailable *core.AgentUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Contains(t, err.Error(), "corrupt")
	require.False(t, errors.Is(err, fs.ErrNotExist), "损坏不得被当成缺失")
	require.True(t, errors.Is(err, corrupt), "原因链必须保留")
	require.Contains(t, h.logs.String(), "level=ERROR")
	require.Contains(t, h.logs.String(), "生命周期状态文件损坏")
	require.Zero(t, store.runsOf())
}

// TestAgentService_Lifecycle_DisableDuringAdmission 守"准入读锁与启停写锁互斥"（011-race
// 必需用例）：写者持写锁把状态改成 disabled 时，准入必须等锁后再读——读到的永远是完整
// 的旧状态或新状态，不会与迁移交叠。
// 逆改点：准入不取 AgentGate 读锁 → 读在写者临界区内发生，interleaved 置位，用例红。
func TestAgentService_Lifecycle_DisableDuringAdmission(t *testing.T) {
	h := newLoopHarness(t)
	gate := core.NewAgentGate()
	lifecycle := newFakeLifecycle(core.AgentActive)

	var writerInside, interleaved atomic.Bool
	lifecycle.onRead = func() {
		if writerInside.Load() {
			interleaved.Store(true)
		}
	}
	svc, store := h.assembleWithLifecycle(lifecycle, gate)

	locked := make(chan struct{})
	release := make(chan struct{})
	go func() {
		unlock := gate.Lock(loopAgentName)
		writerInside.Store(true)
		lifecycle.setState(core.AgentDisabled)
		close(locked)
		<-release
		writerInside.Store(false)
		unlock()
	}()
	<-locked

	runErr := make(chan error, 1)
	go func() {
		_, err := svc.Run(ctx, h.req("在吗"))
		runErr <- err
	}()

	select {
	case err := <-runErr:
		t.Fatalf("停用写锁未释放时准入不得完成（err=%v）", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	err := <-runErr
	var unavailable *core.AgentUnavailableError
	require.ErrorAs(t, err, &unavailable)
	require.Equal(t, core.AgentDisabled, unavailable.State, "等锁后必须读到迁移后的状态")
	require.False(t, interleaved.Load(), "准入读状态不得与启停写锁交叠")
	require.Equal(t, 1, lifecycle.readCount())
	require.Zero(t, store.runsOf())
}

// —— 准入资源固定（T026：图 F ③ 的解析 → 清单 → 传仓储 → 释放）——

// fakeRuntime 是准入固定的运行时视图：记释放次数（约束 46 的判据），并按名字发放
// 准入固定的模型客户端（人工 review ② 缺陷 2：运行时必须真的参与路由）。
// fakeRuntime records releases and hands out the pinned model client for its one name.
type fakeRuntime struct {
	mu                sync.Mutex
	releases          int
	admissions        int                 // 准入读锁的归还次数（ReleaseAdmission 或 Release 的连带归还）
	admissionReturned bool                // 与生产实现的 sync.Once 同口径：读锁只能归还一次
	provider          core.ProviderClient // 准入固定的客户端；nil = 该运行时没有可发放的入口
	providerName      string              // 只对这一条名字发放（路由只查固定表，不推断）
}

// Provider 只对自己固定的名字发放客户端；其余一律 false（失败关闭的判据面）。
func (r *fakeRuntime) Provider(name string) (core.ProviderClient, bool) {
	if r.provider == nil || name != r.providerName {
		return nil, false
	}
	return r.provider, true
}

// MCP 不在本用例的判据面内。
func (r *fakeRuntime) MCP(string) (core.MCPHandle, bool) { return nil, false }

// ReleaseAdmission 记一次准入读锁归还（与 Release 的连带归一分列两个计数面）。
// 与生产实现同口径幂等：读锁已归还时不再计数（否则重复 RUnlock 会 panic）。
func (r *fakeRuntime) ReleaseAdmission() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.admissionReturned {
		return
	}
	r.admissionReturned = true
	r.admissions++
}

// Release 幂等计数（同一实例被释放两次即可见）；按生产口径连带归还准入读锁。
func (r *fakeRuntime) Release() {
	r.mu.Lock()
	r.releases++
	r.mu.Unlock()
	r.ReleaseAdmission()
}

// releaseCount 是已发生的释放次数。
func (r *fakeRuntime) releaseCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releases
}

// admissionReleaseCount 是已发生的准入读锁归还次数（ReleaseAdmission + Release 连带）。
func (r *fakeRuntime) admissionReleaseCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.admissions
}

// fakeRunResources 是准入解析端口的假件：按脚本返回固定结果或错误，并把运行时挂进结果。
// fakeRunResources scripts the resolution outcome and attaches the counting runtime.
type fakeRunResources struct {
	mu       sync.Mutex
	resolved core.ResolvedResources
	err      error
	runtime  *fakeRuntime
	calls    int
}

// Resolve 返回脚本结果；runtime 非 nil 时挂进结果（调用方负责释放）。
func (f *fakeRunResources) Resolve(_ context.Context, _ core.AgentEntry) (core.ResolvedResources, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return core.ResolvedResources{}, f.err
	}
	resolved := f.resolved
	if f.runtime != nil {
		resolved.Runtime = f.runtime
	}
	return resolved, nil
}

// callCount 是已发生的解析次数。
func (f *fakeRunResources) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeSnapshotBuilder 是发布版本快照构建的假件：默认返回一份可跑的快照，可注入错误。
// fakeSnapshotBuilder returns a runnable published-version snapshot unless an error is set.
type fakeSnapshotBuilder struct {
	mu    sync.Mutex
	calls int
	err   error
	// notifyChannels 注入快照 Profile 的通知渠道白名单（空 = 不限制）。
	notifyChannels []string
}

// Build 返回固定快照（哈希与工作副本不同——用例据此分辨"哪条来源胜出"）。
func (b *fakeSnapshotBuilder) Build(_ context.Context, entry core.AgentEntry, _ core.ResolvedResources) (core.ConfigSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.err != nil {
		return core.ConfigSnapshot{}, b.err
	}
	return core.ConfigSnapshot{
		AgentName:  entry.Name,
		Hash:       "published-snapshot-hash",
		Content:    "---\nname: demo\n---\n" + sectionOneStatic + "\n",
		Manifest:   []core.ManifestEntry{{Path: "AGENT.md", SHA256: "d41d8cd98f00b204"}},
		SectionOne: sectionOneStatic,
		Profile: &core.Profile{
			Name:           entry.Name,
			Provider:       core.ProviderRef{Name: "fake", Model: "fake-model"},
			Tools:          []string{"read_file"},
			NotifyChannels: append([]string(nil), b.notifyChannels...),
			Settings:       core.Settings{MaxIterations: 10, MaxHistoryTurns: 20},
		},
	}, nil
}

// callCount 是已发生的构建次数。
func (b *fakeSnapshotBuilder) callCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// sampleResolvedRefs 造一份最小解析结果（Provider + 一条 MCP），供清单构造用。
// sampleResolvedRefs builds a minimal resolution for manifest construction.
func sampleResolvedRefs() core.ResolvedResources {
	return core.ResolvedResources{
		Provider:      core.ResourceVersionRef{Name: "fake", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", VersionID: 1, VersionNo: 1, ContentHash: "aaa"},
		ProviderModel: "fake-model",
		ProviderCredential: core.CredentialPin{
			Mode:          core.CredentialModeEnv,
			TargetBinding: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		Notify:     core.NotifyManifest{Mode: core.NotifyModeAllEligible, Channels: []core.NotifyChannelRef{}},
		MCPServers: []core.ResourceVersionRef{{Name: "files", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B", VersionID: 2, VersionNo: 1, ContentHash: "bbb"}},
	}
}

// assembleWithResources 在与既有用例相同的假件上装一个资源接线齐全的运行入口。
// assembleWithResources wires all three resource ports over the shared fakes.
func (h *loopHarness) assembleWithResources(resources core.RunResources, snapshots core.RunSnapshotBuilder) (*core.AgentService, *recordingStore) {
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := newRecordingStore()
	svc := core.NewAgentService(core.ServiceOptions{
		Agents:       h.agents,
		Client:       h.provider,
		Messages:     store,
		Runs:         store,
		Snapshots:    store,
		Audit:        h.audit,
		Loop:         core.NewReActLoop(core.NewPromptBuilder(h.registry), core.NewToolExecutor(h.registry, h.audit, logger), logger),
		Logger:       logger,
		Now:          func() time.Time { return h.now },
		Lifecycle:    newFakeLifecycle(core.AgentActive),
		Gate:         core.NewAgentGate(),
		Resources:    resources,
		RunSnapshots: snapshots,
	})
	return svc, store
}

// TestAgentService_Resources_ManifestPassesThroughAndRuntimeReleased：资源接线后，准入固定的
// 清单随 StartRun 交给仓储（FormatVersion 非零、状态取门禁读数、版本冻结），运行结束后
// 运行时恰好释放一次（约束 46）。
// TestAgentService_Resources_ManifestPassesThroughAndRuntimeReleased pins the manifest
// hand-off and the single runtime release after FinishRun.
func TestAgentService_Resources_ManifestPassesThroughAndRuntimeReleased(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, textResponse("好"))
	runtime := &fakeRuntime{provider: h.provider, providerName: "fake"}
	resources := &fakeRunResources{resolved: sampleResolvedRefs(), runtime: runtime}
	builder := &fakeSnapshotBuilder{}
	svc, store := h.assembleWithResources(resources, builder)

	res, err := svc.Run(ctx, h.req("在吗"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, "published-snapshot-hash", res.ConfigHash, "资源接线后快照必须走发布版本构建")
	require.Equal(t, 1, resources.callCount())
	require.Equal(t, 1, builder.callCount())

	manifest := store.lastManifest
	require.Equal(t, 2, manifest.FormatVersion, "清单必须是正式形态 v2（零值 = 未接线）")
	require.Equal(t, string(core.AgentActive), manifest.AgentLifecycle, "清单状态取准入门禁的同一次读数")
	require.Equal(t, "fake", manifest.Provider.Name)
	require.Equal(t, "aaa", manifest.Provider.ContentHash)
	require.NotEmpty(t, manifest.Hash, "清单必须带哈希")
	require.Equal(t, manifest.Hash, res.ManifestHash, "RunResult 必须带准入清单哈希（契约 §1.1）")
	require.Equal(t, 1, runtime.releaseCount(), "在途结束后运行时必须恰好释放一次")
}

// TestAgentService_Resources_AdmissionLeaseSpansStartRun（人工 review ② US-12 交接裁决）：
// 准入读锁的租约必须延续到 **StartRun 成功之后**——账本与资源引用落库时必须仍持读锁，
// 发布才插不进"版本已固定、引用未落库"的窗口；落库成功后由 ReleaseAdmission 归还，
// 运行时本身活到运行结束。两个观察点夹出归还窗口：StartRun 记账前**未**归还（0），
// 第一次模型调用时**已**归还（1）。逆改点：在 StartRun 之前归还 → 记账前读数非 0；
// 拖到运行结束随 Release 一起归还 → 首次模型调用时读数仍为 0。两条各自变红。
// The admission lease still held while StartRun persists the ledger (0 at StartRun), already
// returned by the first model call (1) — the two observations bracket the release window.
func TestAgentService_Resources_AdmissionLeaseSpansStartRun(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, textResponse("好"))
	runtime := &fakeRuntime{provider: h.provider, providerName: "fake"}
	resources := &fakeRunResources{resolved: sampleResolvedRefs(), runtime: runtime}
	svc, store := h.assembleWithResources(resources, &fakeSnapshotBuilder{})

	var observedAtStartRun, observedAtFirstChat int
	store.fakeStore.onStartRun = func() { observedAtStartRun = runtime.admissionReleaseCount() }
	h.provider.hook = func(call int) {
		if call == 0 {
			observedAtFirstChat = runtime.admissionReleaseCount()
		}
	}

	res, err := svc.Run(ctx, h.req("在吗"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	require.Zero(t, observedAtStartRun, "StartRun 落库时准入读锁必须仍被租用（发布不得插进这个窗口）")
	require.Equal(t, 1, observedAtFirstChat, "第一次模型调用时读锁必须已归还——不得拖到运行结束才随 Release 归还")
	require.Equal(t, 1, runtime.admissionReleaseCount(), "StartRun 成功后读锁恰好归还一次")
	require.Equal(t, 1, runtime.releaseCount(), "运行结束时运行时恰好释放一次（读锁不得重复归还）")
}

// TestAgentService_Resources_AdmissionLeaseReleasedOnStartRunFailure：StartRun 失败 →
// 准入租约与运行时由 defer 的 Release **一并**归还（失败路径只有一个收尾入口），
// 且失败按函数级 error 上报、不得静默开跑。
// A failed StartRun returns the lease and the runtime through the same Release.
func TestAgentService_Resources_AdmissionLeaseReleasedOnStartRunFailure(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, textResponse("不该被调用"))
	runtime := &fakeRuntime{provider: h.provider, providerName: "fake"}
	resources := &fakeRunResources{resolved: sampleResolvedRefs(), runtime: runtime}
	svc, store := h.assembleWithResources(resources, &fakeSnapshotBuilder{})
	store.fakeStore.startRunErr = errors.New("injected: ledger unavailable")

	_, err := svc.Run(ctx, h.req("在吗"))
	require.Error(t, err)
	require.ErrorContains(t, err, "start run ledger row")

	require.Equal(t, 1, runtime.admissionReleaseCount(), "失败路径必须归还准入读锁（经 Release）")
	require.Equal(t, 1, runtime.releaseCount(), "失败路径必须归还运行时")
	require.Zero(t, h.provider.callCount(), "账本失败不得开始模型调用")
}

// TestAgentService_Resources_PinnedClientServesModelCalls（人工 review ② 缺陷 2）：资源接线
// 后模型调用**只走准入固定的客户端**——装配期的全局客户端一次都不得被调用。此前 Resolve
// 返回的运行时只被释放、从未参与路由（复现读数 final="GLOBAL" global_calls=1
// pinned_calls=0）。逆改点：env.llmClient 改回 s.opts.Client → 本用例红。
// The pinned client serves every model call once resources are wired; the global
// assembly-time client must never be consulted (human review ② defect 2).
func TestAgentService_Resources_PinnedClientServesModelCalls(t *testing.T) {
	h := newLoopHarness(t)
	pinned := newFakeProvider(t)
	pinned.onCall(0, textResponse("来自固定客户端"))
	runtime := &fakeRuntime{provider: pinned, providerName: "fake"}
	resources := &fakeRunResources{resolved: sampleResolvedRefs(), runtime: runtime}
	builder := &fakeSnapshotBuilder{}
	svc, _ := h.assembleWithResources(resources, builder)

	res, err := svc.Run(ctx, h.req("在吗"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, "来自固定客户端", res.FinalText, "响应必须来自固定客户端")
	require.Equal(t, 1, pinned.callCount(), "模型调用必须走准入固定的客户端")
	require.Zero(t, h.provider.callCount(), "不得退回装配期的全局客户端（审计外的第二入口）")
	require.Equal(t, "fake", pinned.request(0).Provider, "出站路由名取快照冻结的 Provider")
}

// TestAgentService_Resources_ToolEnvCarriesBoundSkillDirs（人工 review ② F1）：准入固定的技能
// 发布目录必须随每次工具调用进入 ToolEnv——文件工具只按这份绑定放行读，策略层不自行推断
// 版本。断言**排序后**的切片：map 遍历顺序随机，绑定面必须是可复核的稳定输入。逆改点：
// `RunEnv.ToolEnv` 不填 SkillDirs → 本用例红（文件工具随即读不到发布的 L2/L3）。
// The run-bound skill directories reach every tool call in a deterministic order.
func TestAgentService_Resources_ToolEnvCarriesBoundSkillDirs(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":".basalt/versions/skills/pdf/2/SKILL.md"}`))
	h.provider.onCall(1, textResponse("done"))
	runtime := &fakeRuntime{provider: h.provider, providerName: "fake"}
	resolved := sampleResolvedRefs()
	resolved.SkillDirs = map[string]string{
		"writer": ".basalt/versions/skills/writer/1",
		"pdf":    ".basalt/versions/skills/pdf/2",
	}
	resources := &fakeRunResources{resolved: resolved, runtime: runtime}
	svc, _ := h.assembleWithResources(resources, &fakeSnapshotBuilder{})

	res, err := svc.Run(ctx, h.req("读技能"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)
	require.Equal(t, []string{
		".basalt/versions/skills/pdf/2",
		".basalt/versions/skills/writer/1",
	}, h.tools.executedEnv().SkillDirs, "绑定目录必须排序后原样进入 ToolEnv（map 遍历顺序随机）")
	require.True(t, h.tools.executedEnv().RunBound,
		"循环构造的 ToolEnv 必须显式标记 Run 上下文（F-04 B1：不从 RunID / 绑定推断）")
}

// TestAgentService_Resources_ToolEnvCarriesFrozenNotifyChannels：通知授权在准入时按
// **资源清单**（`ResolvedResources.Notify`）冻结成具体渠道 UID，随每次工具调用进入
// ToolEnv；工具层只读这份副本（模式 + UID 集合），不重新读文件、不按名字猜。
// 快照 Profile 的声明名只服务于未接资源面的兼容路径——资源接线时**不得**胜出。
// The admission-time grant is frozen as concrete channel uids from the resolved manifest
// and reaches every tool call through ToolEnv; the snapshot's declared names never win
// once the resource face is wired.
func TestAgentService_Resources_ToolEnvCarriesFrozenNotifyChannels(t *testing.T) {
	h := newLoopHarness(t)
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"AGENT.md"}`))
	h.provider.onCall(1, textResponse("done"))
	runtime := &fakeRuntime{provider: h.provider, providerName: "fake"}
	resolved := sampleResolvedRefs()
	resolved.Notify = core.NotifyManifest{
		Mode: core.NotifyModeAllowlist,
		Channels: []core.NotifyChannelRef{
			{Name: "ops-alerts", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z4C"},
			{Name: "night-ops", ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z4D"},
		},
	}
	resources := &fakeRunResources{resolved: resolved, runtime: runtime}
	builder := &fakeSnapshotBuilder{notifyChannels: []string{"stale-declared-name"}}
	svc, _ := h.assembleWithResources(resources, builder)

	res, err := svc.Run(ctx, h.req("读"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	env := h.tools.executedEnv()
	require.Equal(t, []string{
		"rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z4C",
		"rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z4D",
	}, env.NotifyChannels, "ToolEnv 携带的是准入冻结的具体渠道 UID")
	require.NotContains(t, env.NotifyChannels, "stale-declared-name",
		"资源接线后快照里的声明名不得进入运行面")
	require.Equal(t, core.NotifyModeAllowlist, env.NotifyMode, "冻结模式随 UID 集合一起进入 ToolEnv")
	require.True(t, env.RunBound, "运行内调用必须带 RunBound 标记：工具按 UID 判定而不是按名字")
}

// TestAgentService_Resources_MissingPinnedClientFailsClosed：准入固定的映射表里没有该
// Provider → 500 分类、零快照、零账本、零模型调用——不得退回全局客户端"照样能跑"
// （人工 review ② 缺陷 2 的失败关闭面）。
// A missing pinned client fails closed before any trace or model call.
func TestAgentService_Resources_MissingPinnedClientFailsClosed(t *testing.T) {
	h := newLoopHarness(t)
	runtime := &fakeRuntime{} // 没有可发放的客户端
	resources := &fakeRunResources{resolved: sampleResolvedRefs(), runtime: runtime}
	builder := &fakeSnapshotBuilder{}
	svc, store := h.assembleWithResources(resources, builder)

	_, err := svc.Run(ctx, h.req("在吗"))
	var unavailable *core.ResourceUnavailableError
	require.ErrorAs(t, err, &unavailable, "固定表缺条目必须走资源不可用分类")
	require.Contains(t, err.Error(), "republish", "文案必须给出修复入口")
	require.Zero(t, builder.callCount(), "拒绝必须发生在快照构建之前")
	require.Zero(t, store.snapshotsOf(), "拒绝必须发生在快照落库之前")
	require.Zero(t, store.runsOf(), "拒绝必须发生在账本预写之前")
	require.Zero(t, h.provider.callCount(), "拒绝必须发生在模型调用之前")
	require.Equal(t, 1, runtime.releaseCount(), "固定成功后的失败分支必须归还运行时")
}

// TestAgentService_Resources_ResolveFailureIsResourceUnavailable：解析失败 → 500 分类
// （*ResourceUnavailableError，文案给修复入口），且发生在任何落痕之前；没有固定成功的
// 运行时，因此也没有释放。
// TestAgentService_Resources_ResolveFailureIsResourceUnavailable maps a resolution failure
// to the fixed 500 category before any trace is written.
func TestAgentService_Resources_ResolveFailureIsResourceUnavailable(t *testing.T) {
	h := newLoopHarness(t)
	runtime := &fakeRuntime{}
	resources := &fakeRunResources{err: errors.New("version 7 of provider openai-main not found"), runtime: runtime}
	builder := &fakeSnapshotBuilder{}
	svc, store := h.assembleWithResources(resources, builder)

	_, err := svc.Run(ctx, h.req("在吗"))
	var unavailable *core.ResourceUnavailableError
	require.ErrorAs(t, err, &unavailable, "资源不可用必须走固定分类，不得混进调用方错误")
	require.Contains(t, err.Error(), "republish", "文案必须给出修复入口")
	require.NotContains(t, err.Error(), "C:\\", "底层错误原文（可能含路径）不得进响应文案")

	require.Zero(t, store.snapshotsOf(), "拒绝必须发生在快照落库之前")
	require.Zero(t, store.runsOf(), "拒绝必须发生在账本预写之前")
	require.Zero(t, h.provider.callCount(), "拒绝必须发生在模型调用之前")
	require.Zero(t, runtime.releaseCount(), "解析失败没有固定出运行时，不得凭空释放")
	require.Zero(t, builder.callCount())
}

// TestAgentService_Resources_PartialWiringFailsClosed：只有解析端口、缺快照构建或生命周期
// 端口 → 装配错误（函数级 error），零落痕——不得用半套来源拼出一份看似完整的账。
// TestAgentService_Resources_PartialWiringFailsClosed refuses a partial resource wiring.
func TestAgentService_Resources_PartialWiringFailsClosed(t *testing.T) {
	h := newLoopHarness(t)
	runtime := &fakeRuntime{}
	resources := &fakeRunResources{resolved: sampleResolvedRefs(), runtime: runtime}

	// 缺快照构建：Lifecycle 有、RunSnapshots 无（半套接线必须失败关闭）。
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := newRecordingStore()
	svc := core.NewAgentService(core.ServiceOptions{
		Agents:    h.agents,
		Client:    h.provider,
		Messages:  store,
		Runs:      store,
		Snapshots: store,
		Loop:      core.NewReActLoop(core.NewPromptBuilder(h.registry), core.NewToolExecutor(h.registry, h.audit, logger), logger),
		Logger:    logger,
		Now:       func() time.Time { return h.now },
		Lifecycle: newFakeLifecycle(core.AgentActive),
		Resources: resources,
	})

	_, err := svc.Run(ctx, h.req("在吗"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "run resources are configured without", "装配错误必须点名缺的那一半")
	require.Zero(t, resources.callCount(), "接线不全时不得开始解析")
	require.Zero(t, store.runsOf())
	require.Zero(t, runtime.releaseCount())
}

// TestAgentService_Resources_FixedRuntimeReleasedOnSnapshotFailure：解析成功后快照构建失败 →
// Run failed（既有快照失败口径），**固定出的运行时仍须释放**——失败分支不得漏运行时。
// TestAgentService_Resources_FixedRuntimeReleasedOnSnapshotFailure still releases the
// pinned runtime when the published snapshot build fails.
func TestAgentService_Resources_FixedRuntimeReleasedOnSnapshotFailure(t *testing.T) {
	h := newLoopHarness(t)
	runtime := &fakeRuntime{provider: h.provider, providerName: "fake"}
	resources := &fakeRunResources{resolved: sampleResolvedRefs(), runtime: runtime}
	builder := &fakeSnapshotBuilder{err: errors.New("published skill SKILL.md hash mismatch")}
	svc, store := h.assembleWithResources(resources, builder)

	res, err := svc.Run(ctx, h.req("在吗"))
	require.NoError(t, err, "运行期失败归 RunResult.Status，不归函数级 error")
	require.Equal(t, core.StatusFailed, res.Status)
	require.Contains(t, res.Reason, "hash mismatch")
	require.Equal(t, 1, runtime.releaseCount(), "固定成功后的失败分支必须归还运行时")
	require.Zero(t, store.runsOf(), "快照失败发生在账本预写之前")
	require.Zero(t, h.provider.callCount())
}

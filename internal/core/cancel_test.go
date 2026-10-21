package core_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是取消语义（约束 20 / 图 G）的验收测试：取消**只发信号**，终态由循环经
// FinishRun 写入；等待有界（生产 30 秒，用例压到毫秒级）；重复取消返回当前状态。
// This file is the acceptance suite of cancel semantics (constraint 20 / graph G): cancel
// only signals, the loop writes the terminal state, the wait is bounded, and a repeated
// cancel reports the current state.

// newCancelHarness 组装取消用例的夹具：等待上限压到 2 秒——正常路径毫秒级返回，
// 出问题时也不会真等 30 秒。
// newCancelHarness builds the fixture with a 2-second wait bound: the normal path returns
// in milliseconds, and a broken one never really waits 30 seconds.
func newCancelHarness(t *testing.T) *loopHarness {
	t.Helper()
	h := newLoopHarness(t)
	core.SetCancelWaitForTest(h.service, 2*time.Second)
	return h
}

// singleRunID 取账本里唯一一行的 run_id（在途运行的标识由 Run 内部生成）。
// singleRunID returns the only run id in the ledger; the in-flight run's id is generated
// inside Run.
func singleRunID(t *testing.T, s *fakeStore) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Len(t, s.runs, 1, "账本里应恰有一行")
	for id := range s.runs {
		return id
	}
	return ""
}

// finishCount 是终态写入次数（取消路径**不得**调用 FinishRun）。
// finishCount reports how many times the terminal state was written.
func (s *fakeStore) finishCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishCalls
}

// seedRunRow 直接种一条运行行（不经 Run——取消用例要的是"库里已有这一行"）。
// seedRunRow seeds one ledger row directly instead of going through Run.
func seedRunRow(s *fakeStore, runID string, status core.RunStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[runID] = core.RunRecord{RunID: runID, Agent: loopAgentName}
	s.status[runID] = status
}

// waitRunResult 限时等待运行返回（卡死即失败）。
// waitRunResult waits for the run to return within the deadline.
func waitRunResult(t *testing.T, ch <-chan core.RunResult, d time.Duration, msg string) core.RunResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(d):
		t.Fatal(msg)
		return core.RunResult{}
	}
}

// startProviderBlockedRun 起一次卡在**模型调用**里的运行：假模型不理会 ctx，模拟
// "取消信号已发出、循环还没收场"的窗口（超时用例的锚点）。返回释放函数与运行返回信号。
// startProviderBlockedRun launches a run blocked inside the model call; the fake provider
// ignores ctx, which is exactly the window the timeout cases need.
func startProviderBlockedRun(t *testing.T, h *loopHarness, runID string) (func(), <-chan core.RunResult) {
	t.Helper()
	release := make(chan struct{})
	chatStarted := make(chan struct{}, 1)
	h.provider.hook = func(int) {
		chatStarted <- struct{}{}
		<-release
	}
	h.provider.onCall(0, textResponse("迟到的回答"))

	done := make(chan core.RunResult, 1)
	req := h.req("卡在模型调用里")
	req.RunID = runID
	go func() {
		res, _ := h.service.Run(ctx, req)
		done <- res
	}()
	waitSignal(t, chatStarted, 2*time.Second, "模型调用未开始（在途登记或准入未完成）")

	var once sync.Once
	return func() { once.Do(func() { close(release) }) }, done
}

// TestAgentService_Cancel_SignalsActiveRun 守"取消是信号、终态由循环写入"：
// 验收点（约束 20 / 图 G ①）——取消传播到阻塞中的工具，循环自行落 cancelled；
// 取消路径零 FinishRun。
func TestAgentService_Cancel_SignalsActiveRun(t *testing.T) {
	h := newCancelHarness(t)
	started, runDone := startBlockedRun(t, h, h.service, h.sessionID)
	waitSignal(t, started, 2*time.Second, "工具未启动")
	runID := singleRunID(t, h.store)

	detail, err := h.service.Cancel(context.Background(), runID)
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, detail.Status, "取消后的详情必须是循环写下的 cancelled")

	waitSignal(t, runDone, 2*time.Second, "取消后 Run 未返回")
	require.Equal(t, core.StatusCancelled, h.store.runStatus(t, runID))
	require.Equal(t, 1, h.store.finishCount(), "终态只由循环写一次，取消路径不得写")
	h.tools.waitExited(t, 2*time.Second) // 取消已传播到工具执行体
}

// TestAgentService_Cancel_DoesNotOverwriteTerminal 守"已写终态不被取消覆盖"：
// 验收点（约束 19 / 20，模块卡 §4.3.5）——终态原样返回、零终态写；未知 run_id → 404 分类。
func TestAgentService_Cancel_DoesNotOverwriteTerminal(t *testing.T) {
	h := newCancelHarness(t)
	seedRunRow(h.store, "run-done", core.StatusCompleted)

	got, err := h.service.Cancel(context.Background(), "run-done")
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, got.Status, "已完成的 Run 必须返回当前终态")
	require.Equal(t, 0, h.store.finishCount(), "取消不得写终态")

	if _, err := h.service.Cancel(context.Background(), "run-missing"); !errors.Is(err, core.ErrRunNotFound) {
		t.Fatalf("未知 run_id 的错误分类 = %v，期望 ErrRunNotFound", err)
	}
}

// TestAgentService_Cancel_TimeoutReturnsCurrentState 守"等待超时不强写"：
// 验收点（约束 20 / 注 15）——等不到收场就如实返回 running 并记 WARN；终态随后仍由
// 循环自己写入，取消路径零终态写。
func TestAgentService_Cancel_TimeoutReturnsCurrentState(t *testing.T) {
	h := newCancelHarness(t)
	core.SetCancelWaitForTest(h.service, 50*time.Millisecond)
	release, runDone := startProviderBlockedRun(t, h, "run-blocked")

	start := time.Now()
	detail, err := h.service.Cancel(context.Background(), "run-blocked")
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, core.StatusRunning, detail.Status, "超时后返回当前状态，不伪造 cancelled")
	require.Less(t, elapsed, time.Second, "等待必须在上限附近收住")
	require.Equal(t, core.StatusRunning, h.store.runStatus(t, "run-blocked"), "取消路径不得写终态")
	require.Equal(t, 0, h.store.finishCount())

	release()
	waitRunResult(t, runDone, 2*time.Second, "释放后 Run 未返回")
	require.NotEqual(t, core.StatusRunning, h.store.runStatus(t, "run-blocked"), "终态仍须由循环写入")
	require.Contains(t, h.logs.String(), "尚未结束", "等待超时必须记 WARN")
}

// TestAgentService_Cancel_StopsWaitingOnCallerCancel 守"调用方中止等待不拖住请求"：
// 验收点（注 15 的等待边界）——调用方的上下文已取消时，Cancel 立即返回当前状态，
// 不越过自己的等待上限；读当前状态用不随调用方取消的派生上下文。
func TestAgentService_Cancel_StopsWaitingOnCallerCancel(t *testing.T) {
	h := newCancelHarness(t)
	core.SetCancelWaitForTest(h.service, 5*time.Second) // 上限放大，证明确实是被调用方唤醒
	release, runDone := startProviderBlockedRun(t, h, "run-caller")

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	cancelCaller() // 调用方已放弃等待

	start := time.Now()
	detail, err := h.service.Cancel(callerCtx, "run-caller")
	elapsed := time.Since(start)
	require.NoError(t, err)
	require.Equal(t, core.StatusRunning, detail.Status)
	require.Less(t, elapsed, 2*time.Second, "调用方取消后必须立即返回，不得等满 5 秒上限")

	release()
	waitRunResult(t, runDone, 2*time.Second, "释放后 Run 未返回")
}

// TestAgentService_Cancel_DatabaseRunningWithoutProcessEntry 守"运行不在本进程"：
// 验收点（注 15）——库里 running 但内存无登记时如实返回当前状态并记 ERROR，不假装取消成功。
func TestAgentService_Cancel_DatabaseRunningWithoutProcessEntry(t *testing.T) {
	h := newCancelHarness(t)
	seedRunRow(h.store, "run-ghost", core.StatusRunning)

	detail, err := h.service.Cancel(context.Background(), "run-ghost")
	require.NoError(t, err)
	require.Equal(t, core.StatusRunning, detail.Status)
	require.Equal(t, 0, h.store.finishCount(), "幽灵行同样不得被取消路径改写")
	require.Contains(t, h.logs.String(), "运行不在本进程")
}

// TestAgentService_Cancel_RepeatedReturnsCurrentState 守"重复取消幂等"：
// 验收点（约束 20）——第二次取消返回当前终态，不再产生终态写。
func TestAgentService_Cancel_RepeatedReturnsCurrentState(t *testing.T) {
	h := newCancelHarness(t)
	started, runDone := startBlockedRun(t, h, h.service, h.sessionID)
	waitSignal(t, started, 2*time.Second, "工具未启动")
	runID := singleRunID(t, h.store)

	first, err := h.service.Cancel(context.Background(), runID)
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, first.Status)
	waitSignal(t, runDone, 2*time.Second, "取消后 Run 未返回")

	second, err := h.service.Cancel(context.Background(), runID)
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, second.Status, "重复取消返回当前状态")
	require.Equal(t, 1, h.store.finishCount(), "重复取消不得再写一次终态")
}

// TestAgentService_Cancel_Concurrent 守"并发取消同一运行"（011-race 必需名）：
// 验收点（约束 20）——多个取消并发到达，全部成功返回，终态只写一次。
func TestAgentService_Cancel_Concurrent(t *testing.T) {
	h := newCancelHarness(t)
	started, runDone := startBlockedRun(t, h, h.service, h.sessionID)
	waitSignal(t, started, 2*time.Second, "工具未启动")
	runID := singleRunID(t, h.store)

	const callers = 8
	var wg sync.WaitGroup
	details := make([]core.RunDetail, callers)
	errs := make([]error, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			details[i], errs[i] = h.service.Cancel(context.Background(), runID)
		}(i)
	}
	wg.Wait()
	waitSignal(t, runDone, 2*time.Second, "并发取消后 Run 未返回")

	for i := 0; i < callers; i++ {
		require.NoError(t, errs[i])
		require.Equal(t, core.StatusCancelled, details[i].Status, "第 %d 个取消必须看到终态", i)
	}
	require.Equal(t, 1, h.store.finishCount(), "并发取消不得产生第二次终态写")
}

// TestAgentService_Cancel_UnknownRun 守"未知运行 404 分类"：
// 验收点（约束 20）——账本无行 → `ErrRunNotFound`（web 层据此映射 404）。
func TestAgentService_Cancel_UnknownRun(t *testing.T) {
	h := newCancelHarness(t)
	_, err := h.service.Cancel(context.Background(), "run-missing")
	require.ErrorIs(t, err, core.ErrRunNotFound)
}

// TestAgentService_Cancel_EmptyRunID 守"空标识按未命中处理"：
// 验收点（约束 20）——空 run_id 不得命中任何登记项，也不得被当作"取消成功"。
func TestAgentService_Cancel_EmptyRunID(t *testing.T) {
	h := newCancelHarness(t)
	_, err := h.service.Cancel(context.Background(), "")
	require.ErrorIs(t, err, core.ErrRunNotFound)
}

// TestAgentService_Cancel_FreesAdmissionSlot 守"取消后准入恢复"：
// 验收点（约束 20 的收场链）——Run 返回即摘除登记并释放会话锁，同一会话可再次运行。
func TestAgentService_Cancel_FreesAdmissionSlot(t *testing.T) {
	h := newCancelHarness(t)
	started, runDone := startBlockedRun(t, h, h.service, h.sessionID)
	waitSignal(t, started, 2*time.Second, "工具未启动")
	runID := singleRunID(t, h.store)
	h.provider.onCall(1, textResponse("第二次完成"))

	_, err := h.service.Cancel(context.Background(), runID)
	require.NoError(t, err)
	waitSignal(t, runDone, 2*time.Second, "取消后 Run 未返回")

	var lastErr error
	require.Eventually(t, func() bool {
		res, err := h.service.Run(ctx, h.req("第二条"))
		lastErr = err
		return err == nil && res.Status == core.StatusCompleted
	}, 2*time.Second, 10*time.Millisecond, "取消后同一会话必须能重新准入")
	require.NoError(t, lastErr)
}

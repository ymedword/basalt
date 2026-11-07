package core

// US-22 受控自我进化：终态 → 抽取钩子 → 引用释放的收尾顺序与失败隔离。
// US-22 controlled self-evolution: the finish order (terminal → extraction hook →
// reference release) and its failure isolation at the run-collection point.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// learningHookProbe 记录钩子调用与顺序；panicIt 用来验证兜底 recover。
// learningHookProbe records calls and order; panicIt exercises the outer recover.
type learningHookProbe struct {
	events  *[]string
	calls   int
	inputs  []LearningRunInput
	results []RunResult
	panicIt bool
}

func (p *learningHookProbe) AfterRun(_ context.Context, in LearningRunInput, res RunResult) {
	p.calls++
	p.inputs = append(p.inputs, in)
	p.results = append(p.results, res)
	if p.events != nil {
		*p.events = append(*p.events, "hook")
	}
	if p.panicIt {
		panic("hook probe panic")
	}
}

// finishOrderProbe 把终态写回与引用释放写进同一事件序列，并可按用例制造失败。
// finishOrderProbe records the terminal write and the release in one event sequence and
// can inject failures per case.
type finishOrderProbe struct {
	RunStore
	events           *[]string
	finishErr        error
	notApplied       bool
	releaseErr       error
	releaseN         int
	finishCtxErr     error
	finishDeadline   time.Time
	releaseCtxErr    error
	releaseDeadline  time.Time
	closeoutErr      error
	closeoutN        int
	closeoutCtxErr   error
	closeoutDeadline time.Time
}

func (p *finishOrderProbe) FinishRun(ctx context.Context, _ string, _ LeaseToken, _ RunStatus, _ string, _, _ int, _ time.Time) (bool, error) {
	if p.events != nil {
		*p.events = append(*p.events, "finish")
	}
	p.finishCtxErr = ctx.Err()
	p.finishDeadline, _ = ctx.Deadline()
	if p.finishErr != nil {
		return false, p.finishErr
	}
	return !p.notApplied, nil
}

// CompleteRunCloseout 记入同一事件序列：US-24 的五步收尾要求它排在释放**之后**，
// 且系统代投的 Run 不在这里出现（投递在 Run 返回之后结算，由调用方补这一笔）。
func (p *finishOrderProbe) CompleteRunCloseout(ctx context.Context, _ string, _ LeaseToken, _ time.Time) error {
	p.closeoutN++
	p.closeoutCtxErr = ctx.Err()
	p.closeoutDeadline, _ = ctx.Deadline()
	if p.events != nil {
		*p.events = append(*p.events, "closeout")
	}
	return p.closeoutErr
}

func (p *finishOrderProbe) ReleaseRunRefs(ctx context.Context, _ string, _ time.Time) error {
	p.releaseN++
	p.releaseCtxErr = ctx.Err()
	p.releaseDeadline, _ = ctx.Deadline()
	if p.events != nil {
		*p.events = append(*p.events, "release")
	}
	return p.releaseErr
}

func newLearningHookService(store RunStore, hook LearningHook) *AgentService {
	return &AgentService{opts: ServiceOptions{
		Runs:     store,
		Learning: hook,
		Now:      time.Now,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
}

// completedLearningEnv 构造一个满足抽取门的普通 Web 完成运行环境。
// completedLearningEnv is an ordinary completed web run environment that passes the gate.
func completedLearningEnv() *RunEnv {
	return &RunEnv{Req: RunRequest{Agent: "ops", SessionID: "sess-1", Channel: "web"}}
}

// TestUS22_Hook_OnlyAfterTerminalApplied 守 C-05：只有本次调用真正完成终态转换才抽取，
// 且顺序固定为 终态 → 钩子 → 释放 → 收尾（US-24 起，投递结算之后才是收尾末笔；
// 本方法内的收尾排在释放之后，系统代投的 Run 由调用方在投递落库后补）。
// Only a run whose terminal state was really applied extracts, in the fixed US-24 order
// terminal → hook → release → closeout.
func TestUS22_Hook_OnlyAfterTerminalApplied(t *testing.T) {
	t.Run("completed-web-runs-once", func(t *testing.T) {
		var events []string
		store := &finishOrderProbe{events: &events}
		hook := &learningHookProbe{events: &events}
		result := newLearningHookService(store, hook).finish(
			t.Context(), "run_1", completedLearningEnv(), RunResult{Status: StatusCompleted})
		require.Equal(t, []string{"finish", "hook", "release", "closeout"}, events)
		require.Equal(t, 1, hook.calls)
		require.Equal(t, 1, store.closeoutN)
		require.Equal(t, StatusCompleted, hook.results[0].Status)
		require.Equal(t, "run_1", result.RunID)
		require.NoError(t, store.finishCtxErr, "终态写回必须用不受调用方取消影响的上下文")
		require.False(t, store.finishDeadline.IsZero())
		require.WithinDuration(t, time.Now().Add(5*time.Second), store.finishDeadline, 150*time.Millisecond,
			"FinishRun 的独立预算 ≤5s（C-05/card §3.1.12）")
		require.NoError(t, store.closeoutCtxErr, "收尾必须用不受调用方取消影响的上下文")
		require.False(t, store.closeoutDeadline.IsZero())
		require.WithinDuration(t, time.Now().Add(5*time.Second), store.closeoutDeadline, 150*time.Millisecond,
			"CompleteRunCloseout 的独立预算 ≤5s（card §2.3.5）")
	})
	t.Run("system-delivery-closeout-deferred-to-caller", func(t *testing.T) {
		var events []string
		store := &finishOrderProbe{events: &events}
		hook := &learningHookProbe{events: &events}
		env := completedLearningEnv()
		env.Req.SuppressNotify = true
		newLearningHookService(store, hook).finish(
			t.Context(), "run_1", env, RunResult{Status: StatusCompleted})
		require.Equal(t, []string{"finish", "hook", "release"}, events,
			"系统代投的 Run 在投递落库前不得收尾——否则 B1 会把在途投递误判成 unknown")
		require.Equal(t, 0, store.closeoutN)
	})
	t.Run("idempotent-not-applied", func(t *testing.T) {
		var events []string
		store := &finishOrderProbe{events: &events, notApplied: true}
		hook := &learningHookProbe{events: &events}
		newLearningHookService(store, hook).finish(
			t.Context(), "run_1", completedLearningEnv(), RunResult{Status: StatusCompleted})
		require.Equal(t, 0, hook.calls, "命中 0 行幂等分支不得抽取")
		require.Equal(t, []string{"finish", "release", "closeout"}, events)
	})
	t.Run("terminal-not-completed", func(t *testing.T) {
		var events []string
		store := &finishOrderProbe{events: &events}
		hook := &learningHookProbe{events: &events}
		newLearningHookService(store, hook).finish(
			t.Context(), "run_1", completedLearningEnv(), RunResult{Status: StatusCancelled})
		require.Equal(t, 0, hook.calls)
		require.Equal(t, []string{"finish", "release", "closeout"}, events)
	})
	t.Run("finish-write-error", func(t *testing.T) {
		var events []string
		store := &finishOrderProbe{events: &events, finishErr: errors.New("ledger unavailable")}
		hook := &learningHookProbe{events: &events}
		newLearningHookService(store, hook).finish(
			t.Context(), "run_1", completedLearningEnv(), RunResult{Status: StatusCompleted})
		require.Equal(t, 0, hook.calls)
		require.Equal(t, []string{"finish", "release", "closeout"}, events)
	})
}

// TestUS22_FinishWriteFailure_SkipsExtraction 守 B-10 / C-05：终态写失败时不得把内存里的
// completed 当成已落库，不抽取；释放照常尝试（终态未落库时由存储实现内部 no-op）。
// A failed terminal write must not be read as "completed in memory": no extraction, and
// the release is still attempted.
func TestUS22_FinishWriteFailure_SkipsExtraction(t *testing.T) {
	var events []string
	store := &finishOrderProbe{events: &events, finishErr: errors.New("ledger unavailable")}
	hook := &learningHookProbe{events: &events}
	input := RunResult{Status: StatusCompleted, FinalText: "answer", Iterations: 3, ToolCalls: 2, ConfigHash: "cfg-1"}
	result := newLearningHookService(store, hook).finish(t.Context(), "run_1", completedLearningEnv(), input)
	require.Equal(t, 0, hook.calls)
	require.Equal(t, []string{"finish", "release", "closeout"}, events)
	require.Equal(t, input.FinalText, result.FinalText)
	require.Equal(t, input.Status, result.Status)
	require.Equal(t, input.ConfigHash, result.ConfigHash)
	require.Equal(t, 1, store.releaseN)
}

// TestUS22_Hook_SkipsFlowFixedToolScheduler 守 C-05：Flow、固定工具与调度器渠道都不触发抽取。
// Flow runs, fixed-tool runs and scheduler runs never trigger extraction.
func TestUS22_Hook_SkipsFlowFixedToolScheduler(t *testing.T) {
	cases := map[string]func(*RunEnv){
		"flow": func(env *RunEnv) { env.Req.Flow = &FlowRunContext{} },
		"fixed-tool": func(env *RunEnv) {
			env.Req.FixedTool = &ToolCall{Name: "save_memory"}
		},
		"scheduler": func(env *RunEnv) { env.Req.Channel = "scheduler" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var events []string
			store := &finishOrderProbe{events: &events}
			hook := &learningHookProbe{events: &events}
			env := completedLearningEnv()
			mutate(env)
			newLearningHookService(store, hook).finish(
				t.Context(), "run_1", env, RunResult{Status: StatusCompleted})
			require.Equal(t, 0, hook.calls)
			require.Equal(t, []string{"finish", "release", "closeout"}, events)
		})
	}
}

// TestUS22_Hook_FailureDoesNotChangeRun 守 C-27：钩子失败（此处以 panic 表示）不得改变
// Run 的状态、返回文本、原因或计数。
// Hook failure (a panic here) must not change the run's status, text, reason or counters.
func TestUS22_Hook_FailureDoesNotChangeRun(t *testing.T) {
	var events []string
	store := &finishOrderProbe{events: &events}
	hook := &learningHookProbe{events: &events, panicIt: true}
	input := RunResult{
		Status: StatusCompleted, FinalText: "the answer", Reason: "", Iterations: 4,
		ToolCalls: 2, ConfigHash: "cfg-1", ManifestHash: "man-1",
	}
	result := newLearningHookService(store, hook).finish(t.Context(), "run_1", completedLearningEnv(), input)
	require.Equal(t, 1, hook.calls)
	require.Equal(t, input.Status, result.Status)
	require.Equal(t, input.FinalText, result.FinalText)
	require.Equal(t, input.Reason, result.Reason)
	require.Equal(t, input.Iterations, result.Iterations)
	require.Equal(t, input.ToolCalls, result.ToolCalls)
	require.Equal(t, input.ConfigHash, result.ConfigHash)
	require.Equal(t, input.ManifestHash, result.ManifestHash)
}

// TestUS22_Hook_PanicRecovered 守 C-27：钩子内的 panic 在收尾路径被兜底恢复，
// 不穿透到 Run 的调用方；释放仍在恢复后照常执行。
// A hook panic is recovered at the finish path, never reaches the run's caller, and the
// release still runs after recovery.
func TestUS22_Hook_PanicRecovered(t *testing.T) {
	var events []string
	store := &finishOrderProbe{events: &events}
	hook := &learningHookProbe{events: &events, panicIt: true}
	require.NotPanics(t, func() {
		result := newLearningHookService(store, hook).finish(
			t.Context(), "run_1", completedLearningEnv(), RunResult{Status: StatusCompleted})
		require.Equal(t, "run_1", result.RunID)
	})
	require.Equal(t, []string{"finish", "hook", "release", "closeout"}, events)
	require.Equal(t, 1, store.releaseN)
}

// TestUS22_Release_RunsAfterHookFailure 守 C-32：钩子 panic 之后释放仍执行，顺序不变。
// The release still runs after a hook panic, in the fixed order.
func TestUS22_Release_RunsAfterHookFailure(t *testing.T) {
	var events []string
	store := &finishOrderProbe{events: &events}
	hook := &learningHookProbe{events: &events, panicIt: true}
	newLearningHookService(store, hook).finish(
		t.Context(), "run_1", completedLearningEnv(), RunResult{Status: StatusCompleted})
	require.Equal(t, []string{"finish", "hook", "release", "closeout"}, events)
	require.NoError(t, store.releaseCtxErr, "释放必须用不受调用方取消影响的上下文")
	require.WithinDuration(t, time.Now().Add(5*time.Second), store.releaseDeadline, 100*time.Millisecond)
}

// TestUS22_Release_RunsWhenHookSkipped 守 C-32：钩子被跳过（未完成转换）时释放照样执行。
// The release runs even when the hook is skipped because no transition was applied.
func TestUS22_Release_RunsWhenHookSkipped(t *testing.T) {
	var events []string
	store := &finishOrderProbe{events: &events, notApplied: true}
	hook := &learningHookProbe{events: &events}
	newLearningHookService(store, hook).finish(
		t.Context(), "run_1", completedLearningEnv(), RunResult{Status: StatusCompleted})
	require.Equal(t, 0, hook.calls)
	require.Equal(t, []string{"finish", "release", "closeout"}, events)
}

// TestUS22_ReleaseFailureKeepsTerminal 守 C-32：释放失败只记错误，不改变已落库的终态，
// 也不改变 Run 的返回结果。
// A release failure only logs: the applied terminal state and the run result stay intact.
func TestUS22_ReleaseFailureKeepsTerminal(t *testing.T) {
	var events []string
	store := &finishOrderProbe{events: &events, releaseErr: errors.New("release unavailable")}
	hook := &learningHookProbe{events: &events}
	input := RunResult{Status: StatusCompleted, FinalText: "done"}
	result := newLearningHookService(store, hook).finish(t.Context(), "run_1", completedLearningEnv(), input)
	require.Equal(t, []string{"finish", "hook", "release", "closeout"}, events)
	require.Equal(t, 1, hook.calls, "释放失败发生在钩子之后，不得反过来阻断抽取")
	require.Equal(t, input.Status, result.Status)
	require.Equal(t, input.FinalText, result.FinalText)
	require.Equal(t, 1, store.releaseN)
}

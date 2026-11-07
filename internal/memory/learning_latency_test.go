package memory

// 本文件是 US-22 T018 的**分段时延**验收面（颗粒度文档 §3.1.12 / §4.3 B-12、
// C-06 / FR-22-10）。判据全部落在「传给下游的截止」与「钩子在截止内收场」两件事上：
//
//	StageBounds                    —— 20s = 工作段 13s + 审计回填 5s + 失败回填 2s
//	TotalBudget                    —— 模型耗尽工作段后，钩子仍在总预算内落 failed
//	AuditPreWriteCrossDeadline     —— 预写即使给自己 5s，也不能越过工作段截止
//	SlowModelAuditSuccess          —— 慢模型（仍在工作段内）正常返回 → success 照常提交
//	WorkDeadlineFailureStillAudited—— 工作段耗尽后失败回填仍拿到**新的**有界上下文
//	Failure_TimeoutRecordedAfterDeadline —— 超时归类 timeout，失败行在截止之后写回
//
// 为避免用例真的等 20 秒，这里注入缩短的 `LearningLimits`；断言的是**同一套截止结构**
// （工作段与总预算的差值、失败回填从总截止截断），不是复用生产字面量。生产字面量由
// `TestUS22_Latency_StageBounds` 的常量段单独钉住。
//
// The segmented-latency cases assert the deadline structure with shortened limits; only
// StageBounds pins the production literals. The provider half of the audit window is
// covered by TestUS22_Provider_FinalizeDeadlineUsesStartTime in that package.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// learningLatencyStore 在记账桩之上记录失败回填收到的上下文状态：失败终态写回必须
// 拿到**未过期**的独立上下文（WithoutCancel + 有界截止），而不是工作段那个已到期的。
// learningLatencyStore records the context the failure write receives.
type learningLatencyStore struct {
	learningFakeStore
	failCtxErr   error
	failDeadline time.Time
}

func (s *learningLatencyStore) FailExtraction(ctx context.Context, extractionID, triggerRunID string,
	token core.LeaseToken, expectedPrev string, expectedAttempts int,
	class core.LearningFailureClass, message string, failedAt time.Time,
) error {
	s.failCtxErr = ctx.Err()
	s.failDeadline, _ = ctx.Deadline()
	return s.learningFakeStore.FailExtraction(ctx, extractionID, triggerRunID, token,
		expectedPrev, expectedAttempts, class, message, failedAt)
}

// learningLatencyExtractor 是可控的模型桩：可延迟、可阻塞到工作段截止、可在调用内
// 模拟一次"晚到且慢"的审计预写。它记录拿到的两个截止，断言据此判断预算结构。
type learningLatencyExtractor struct {
	reply         string
	err           error
	delay         time.Duration
	block         bool
	prewriteDelay time.Duration

	calls                int
	workDeadline         time.Time
	hookDeadline         time.Time
	hookDeadlineAtReturn time.Time
	prewriteDeadline     time.Time
	prewriteErr          error
	deadlineErrAtReturn  error
}

func (e *learningLatencyExtractor) ChatMemoryExtract(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	e.calls++
	e.workDeadline, _ = ctx.Deadline()
	e.hookDeadline = req.HookDeadline
	if e.delay > 0 {
		timer := time.NewTimer(e.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			e.deadlineErrAtReturn = ctx.Err()
			return core.ChatResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	if e.prewriteDelay > 0 {
		// 预写给自己 5s：子上下文仍被父的工作段截止钳住——这正是"不得外扩 5 秒"
		// 在钩子侧的判据。Provider 侧同一判据由 TestUS22_Provider_PreWriteInheritsWorkDeadline
		// 判卷（那边才能在真实预写入口上逆改）。
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		e.prewriteDeadline, _ = pctx.Deadline()
		timer := time.NewTimer(e.prewriteDelay)
		defer timer.Stop()
		select {
		case <-pctx.Done():
			e.prewriteErr = pctx.Err()
			cancel()
			return core.ChatResponse{}, pctx.Err()
		case <-timer.C:
		}
		cancel()
	}
	if e.block {
		<-ctx.Done()
		e.deadlineErrAtReturn = ctx.Err()
		return core.ChatResponse{}, ctx.Err()
	}
	e.hookDeadlineAtReturn = req.HookDeadline
	return core.ChatResponse{Content: e.reply}, e.err
}

// newLearningLatencyRig 用给定上限装配服务；`normaliseLimits` 保证只覆盖显式字段。
// newLearningLatencyRig assembles the service with the given limits.
func newLearningLatencyRig(t *testing.T, limits LearningLimits) (*LearningService, *learningLatencyStore) {
	t.Helper()
	store := &learningLatencyStore{learningFakeStore: learningFakeStore{entries: learningEntriesFixture()}}
	service := NewLearningService(LearningDeps{
		Store:     store,
		Messages:  &learningFakeMessages{window: learningWindowFixture()},
		Enabled:   true,
		Agents:    []string{"demo"},
		DraftRoot: filepath.Join(t.TempDir(), "drafts"),
		Limits:    limits,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       func() time.Time { return learningTestNow },
	})
	return service, store
}

// latencyInput 构造一次 web 渠道的完成 Run。
// latencyInput builds one completed web run.
func latencyInput(extractor core.MemoryExtractor) core.LearningRunInput {
	in := learningRunInput(extractor)
	in.RunID = "run_latency"
	return in
}

// assertDeadlineWithin 断言截止落在 want 附近的宽松窗口内（Windows 定时器精度与 CI
// 调度抖动都要容得下）；差值断言用 assertGapWithin 单独做更紧的判卷。
// assertDeadlineWithin allows scheduler jitter around the expected absolute deadline.
func assertDeadlineWithin(t *testing.T, want, got time.Time, slack time.Duration) {
	t.Helper()
	require.False(t, got.IsZero(), "必须拿到截止：无截止就无从判“不超过预算”")
	require.WithinDuration(t, want, got, slack)
}

// assertGapWithin 断言两个截止的差值接近 want：判的是预算**分段**，与机器快慢无关。
// assertGapWithin pins the budget split independent of machine speed.
func assertGapWithin(t *testing.T, want time.Duration, from, to time.Time) {
	t.Helper()
	require.WithinDuration(t, from.Add(want), to, 50*time.Millisecond)
}

// TestUS22_Latency_StageBounds 守 C-06 / B-12：生产预算固定为总 20s = 工作 13s +
// 审计回填 5s + 失败回填 2s；注入缩短上限后，工作段与总预算的差值、以及两个绝对截止
// 仍按同一结构计算（不是各写各的字面量）。
// The production split is 20 = 13 + 5 + 2; the shortened run keeps the same structure.
func TestUS22_Latency_StageBounds(t *testing.T) {
	limits := DefaultLearningLimits()
	require.Equal(t, 20*time.Second, limits.TotalTimeout, "钩子端到端总预算")
	require.Equal(t, 13*time.Second, limits.Timeout, "工作段：读窗口 / 预算 / 模型 / 校验 / 提交")
	require.Equal(t, 2*time.Second, limits.failureWriteTimeout, "失败终态回填的独立预算")
	require.Equal(t, 5*time.Second,
		limits.TotalTimeout-limits.Timeout-limits.failureWriteTimeout,
		"剩余 5s 是 Provider 审计终态回填预算；三段之和必须等于总预算")

	short := LearningLimits{TotalTimeout: 900 * time.Millisecond, Timeout: 400 * time.Millisecond}
	service, _ := newLearningLatencyRig(t, short)
	extractor := &learningLatencyExtractor{reply: `{"items":[]}`}
	start := time.Now()
	service.AfterRun(context.Background(), latencyInput(extractor),
		core.RunResult{RunID: "run_latency", Status: core.StatusCompleted})

	require.Equal(t, 1, extractor.calls)
	assertDeadlineWithin(t, start.Add(400*time.Millisecond), extractor.workDeadline, 150*time.Millisecond)
	assertDeadlineWithin(t, start.Add(900*time.Millisecond), extractor.hookDeadline, 150*time.Millisecond)
	assertGapWithin(t, 500*time.Millisecond, extractor.workDeadline, extractor.hookDeadline)
	require.True(t, extractor.hookDeadline.After(extractor.workDeadline),
		"审计回填窗口必须晚于工作段：供应商在模型返回后才有时间回填")
}

// TestUS22_Latency_TotalBudget 守 C-06：模型耗尽工作段时，钩子仍在总预算内返回，失败
// 终态在总截止之前写回（失败回填从总截止截断，不叠加第二段预算）。
// When the model exhausts the work segment, the hook still finishes inside the total
// budget and the failure write is capped by the hook deadline.
func TestUS22_Latency_TotalBudget(t *testing.T) {
	limits := LearningLimits{TotalTimeout: 700 * time.Millisecond, Timeout: 300 * time.Millisecond}
	service, store := newLearningLatencyRig(t, limits)
	extractor := &learningLatencyExtractor{block: true}
	start := time.Now()

	service.AfterRun(context.Background(), latencyInput(extractor),
		core.RunResult{RunID: "run_latency", Status: core.StatusCompleted})
	elapsed := time.Since(start)

	require.Equal(t, 1, extractor.calls)
	require.ErrorIs(t, extractor.deadlineErrAtReturn, context.DeadlineExceeded,
		"模型必须真的耗到工作段截止，而不是提前返回")
	require.GreaterOrEqual(t, elapsed, 250*time.Millisecond, "钩子确实等满了工作段")
	require.Less(t, elapsed, limits.TotalTimeout+300*time.Millisecond,
		"端到端必须落在总预算附近（含失败回填），不得叠加独立预算")
	require.Equal(t, 1, store.failCalls, "工作段耗尽仍要写失败终态")
	require.Equal(t, core.LearningFailureTimeout, store.failClass)
	require.NoError(t, store.failCtxErr, "失败回填必须拿到未过期的独立上下文")
	require.False(t, store.failDeadline.IsZero())
	require.False(t, store.failDeadline.After(extractor.hookDeadline),
		"失败回填截止不得越过钩子总截止")
	require.Zero(t, store.commitCalls)
}

// TestUS22_Latency_AuditPreWriteCrossDeadline 守 C-06 / C-07：晚到的工作段里，审计
// 预写即使给自己 5s，也只能被父上下文钳在工作段截止——预写不能把 5s 叠到 13s 之上，
// 端到端因此仍受 20s 总预算约束。
// A late pre-write asking for 5s cannot cross the work-segment deadline; the 5s is not
// stacked on top of the 13s segment.
func TestUS22_Latency_AuditPreWriteCrossDeadline(t *testing.T) {
	limits := LearningLimits{TotalTimeout: 900 * time.Millisecond, Timeout: 350 * time.Millisecond}
	service, store := newLearningLatencyRig(t, limits)
	extractor := &learningLatencyExtractor{reply: `{"items":[]}`, delay: 250 * time.Millisecond, prewriteDelay: 400 * time.Millisecond}
	start := time.Now()

	service.AfterRun(context.Background(), latencyInput(extractor),
		core.RunResult{RunID: "run_latency", Status: core.StatusCompleted})
	elapsed := time.Since(start)

	require.Equal(t, 1, extractor.calls)
	require.ErrorIs(t, extractor.prewriteErr, context.DeadlineExceeded,
		"预写必须被工作段截止截断，而不是自己再拖 5s")
	require.False(t, extractor.prewriteDeadline.After(extractor.workDeadline),
		"预写截止不得晚于工作段截止")
	require.Less(t, elapsed, limits.TotalTimeout+300*time.Millisecond,
		"预写被工作段截断后，端到端仍应在总预算内")
	require.Equal(t, 1, store.failCalls)
	require.Equal(t, core.LearningFailureTimeout, store.failClass,
		"预写超时归 timeout（Provider 侧把 ErrAuditWrite 归类在包装层，这里得到的是 ctx 截止）")
	require.Zero(t, store.commitCalls)
}

// TestUS22_Latency_SlowModelAuditSuccess 守 B-12 场景 A：慢模型（仍在工作段内）正常
// 返回时，终态提交照常成功；此刻钩子总截止仍在未来，审计回填有它的 5s 窗口。
// A slow but in-budget model still commits success, and the hook deadline is still in the
// future so the provider has room to finalize its audit row.
func TestUS22_Latency_SlowModelAuditSuccess(t *testing.T) {
	limits := LearningLimits{TotalTimeout: 1000 * time.Millisecond, Timeout: 600 * time.Millisecond}
	service, store := newLearningLatencyRig(t, limits)
	extractor := &learningLatencyExtractor{reply: `{"items":[]}`, delay: 450 * time.Millisecond}

	service.AfterRun(context.Background(), latencyInput(extractor),
		core.RunResult{RunID: "run_latency", Status: core.StatusCompleted})

	require.Equal(t, 1, extractor.calls)
	require.NoError(t, extractor.deadlineErrAtReturn, "慢调用仍在工作段内，不得提前超时")
	require.Equal(t, 1, store.commitCalls, "慢模型正常返回仍提交 success")
	require.Zero(t, store.failCalls)
	require.True(t, extractor.hookDeadlineAtReturn.After(time.Now()),
		"返回时总截止仍未来：Provider 据此在回填开始时还能算出正的 5s 窗口")
	require.True(t, extractor.hookDeadlineAtReturn.After(extractor.workDeadline))
}

// TestUS22_Latency_WorkDeadlineFailureStillAudited 守 C-06 / B-12 场景 B：工作段耗尽后，
// 失败终态仍写得进去——失败回填用 WithoutCancel 的新上下文，截止从总预算截断；这正
// 是"模型超时仍留审计痕迹"的钩子侧判据。
// After the work segment is exhausted, the failure write still gets a fresh bounded
// context: the row is audited even though the work context is already done.
func TestUS22_Latency_WorkDeadlineFailureStillAudited(t *testing.T) {
	limits := LearningLimits{TotalTimeout: 800 * time.Millisecond, Timeout: 300 * time.Millisecond}
	service, store := newLearningLatencyRig(t, limits)
	extractor := &learningLatencyExtractor{block: true}
	start := time.Now()

	service.AfterRun(context.Background(), latencyInput(extractor),
		core.RunResult{RunID: "run_latency", Status: core.StatusCompleted})

	require.Equal(t, 1, store.failCalls, "工作段到期不是“没有失败可写”")
	require.NoError(t, store.failCtxErr, "失败回填上下文不得继承已到期的工作段")
	require.False(t, store.failDeadline.IsZero())
	require.True(t, store.failDeadline.After(extractor.workDeadline),
		"失败回填截止晚于工作段截止：它拿的是总预算里的独立一段")
	require.False(t, store.failDeadline.After(extractor.hookDeadline),
		"但仍被钩子总截止截断")
	assertDeadlineWithin(t, start.Add(800*time.Millisecond), store.failDeadline, 150*time.Millisecond)
	require.Equal(t, core.LearningFailureTimeout, store.failClass)
}

// TestUS22_Failure_TimeoutRecordedAfterDeadline 守 FR-22-10 / 失败矩阵：模型返回
// context.DeadlineExceeded 归 `timeout`；失败行在截止之后写回，失败回填上下文仍有界
// 且未过期，提交不发生。
// A deadline error is classified as timeout; the failure row lands after the deadline in
// a fresh bounded context and nothing is committed.
func TestUS22_Failure_TimeoutRecordedAfterDeadline(t *testing.T) {
	limits := LearningLimits{TotalTimeout: 600 * time.Millisecond, Timeout: 200 * time.Millisecond}
	service, store := newLearningLatencyRig(t, limits)
	extractor := &learningLatencyExtractor{err: context.DeadlineExceeded}
	start := time.Now()

	service.AfterRun(context.Background(), latencyInput(extractor),
		core.RunResult{RunID: "run_latency", Status: core.StatusCompleted})

	require.Equal(t, 1, extractor.calls)
	require.Equal(t, 1, store.failCalls)
	require.Equal(t, core.LearningFailureTimeout, store.failClass,
		"context.DeadlineExceeded 必须归 timeout，不得落到 model_error")
	require.NoError(t, store.failCtxErr, "失败回填在截止之后仍要有可用上下文")
	require.True(t, store.failDeadline.After(start.Add(100*time.Millisecond)),
		"失败回填截止是新的 2s 段（被总截止截断），不是模型工作段的旧截止")
	require.False(t, store.failDeadline.After(extractor.hookDeadline))
	require.Zero(t, store.commitCalls)
	require.Contains(t, store.failMessage, "deadline exceeded",
		"失败摘要保留原因类型，但不含窗口正文")
	require.False(t, errors.Is(store.failCtxErr, context.Canceled))
}

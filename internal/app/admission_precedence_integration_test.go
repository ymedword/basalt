//go:build integration

package app

// 本文件落 US-24 T013 的准入判据（模块卡 §3.1.2 / §4）：
//
//  1. `TestAdmission_ErrorPrecedence`——错误优先级逐项成立
//     `AUTH_RATE_LIMITED → CALLER_RATE_LIMITED → CALLER_CONCURRENCY_LIMITED →
//     SESSION_BUSY → TOO_MANY_RUNS → QUOTA_EXHAUSTED`（FR-24-34），且被拒请求
//     **零占用**（不占名额、不写 Run 行、不留半截任务预留）；
//  2. `TestRun_InstanceCapStaysPerWorker`——实例并发上限是**每个 Worker** 的
//     （FR-24-08 / FR-24-32 的同一组边界）：第二个 Worker 用自己的实例计数继续接
//     Run，第一个 Worker 满额只拒自己；
//  3. `TestRun_CallerConcurrencyPrecedesLocalSessionBusy` /
//     `TestRunAdmission_CallerConcurrencyPrecedesInflightCapacity`——§4.5 的合并负向
//     用例：跨 Worker 并发超限与本地会话忙 / 在途名额池满**同时成立**时，只返回
//     `CALLER_CONCURRENCY_LIMITED`。两条分别钉住 `core` 的 [4] 顺序与入口层
//     「预检先于容量类拒绝」的顺序（FR-24-34 / FR-24-40）。
//
// 入口的三段按生产顺序接线：认证闸门用**真实** `web.NewAuthGuard`，入口速率用
// **真实** `callers.Limiter.AdmitIngress`，运行入口直接调 `h.service.Run`
// （`NewRunService`，含 `runAdmission` 名额表）。判卷只看错误归类——`errors.Is` /
// `errors.As` 命中哪个哨兵；哨兵到 HTTP 码与 `code` 字面量的唯一映射在
// `internal/web`（`writeRunError` 与 run-and-errors §8 契约测试），本文件不复制
// 那份映射，`admissionCodeOf` 只为失败信息可读。
// The two cases pin FR-24-34's precedence and the per-worker instance cap. The entry
// chain uses the real auth guard and the real ingress limiter; classification is by
// sentinel, the HTTP mapping stays owned by internal/web.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/web"
)

// admissionCodeOf 把一次入口拒绝归类成契约 §8 的错误码字面量。它只做**归类**：
// 判定依据是 core / callers 的哨兵，映射结果用于断言与失败信息。
// admissionCodeOf classifies a rejection by sentinel; the web layer owns the mapping.
func admissionCodeOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, callers.ErrRateLimited):
		return "CALLER_RATE_LIMITED"
	case errors.Is(err, callers.ErrConcurrencyLimited):
		return "CALLER_CONCURRENCY_LIMITED"
	case errors.Is(err, callers.ErrQuotaExhausted):
		return "QUOTA_EXHAUSTED"
	}
	var busy *core.SessionBusyError
	if errors.As(err, &busy) {
		return "SESSION_BUSY"
	}
	var full *core.InstanceFullError
	if errors.As(err, &full) {
		return "TOO_MANY_RUNS"
	}
	return "UNEXPECTED: " + err.Error()
}

// TestAdmission_ErrorPrecedence 逐项验证六段错误优先级。
//
// 判据面：每一段都让**其余条件同时成立**，只留一个"更靠前"的判据——被命中的必须
// 是更靠前的那一个，且被拒请求零占用。逆改点：把 `AdmitIngress` 挪到运行入口之后
// （或让认证限速发生在凭据校验之后），第 1/2 段立刻变红。
// Each step arms every condition and asserts the earliest one wins.
func TestAdmission_ErrorPrecedence(t *testing.T) {
	const (
		caller      = runHarnessCaller
		adminToken  = "precedence-admin-token"
		allowedHost = "127.0.0.1:8080"
		sessionKey  = "X-Test-Session"
		busySession = "sess-busy"
		// 认证失败来源与后续正常来源用不同的 IP：认证限速按来源 IP（不是端口）计数，
		// 同一 IP 换个端口仍共享窗口。
		authSource = "203.0.113.9:40000"
		okSource   = "198.51.100.7:40000"
	)

	// 实例上限压到 1：`TOO_MANY_RUNS` 才可能真的发生在名额与会话门禁之后。
	h := newRunHarnessWith(t, runHarnessOptions{MaxConcurrent: 1})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	guard, err := web.NewAuthGuard(web.AdminAuthConfig{
		Mode:         "token",
		Token:        adminToken,
		AllowedHosts: []string{allowedHost},
	}, logger, nil)
	require.NoError(t, err)

	// downstream 记录**下游**（认证之后）这次请求的结局；nil 表示下游从未被调用。
	// 每次请求前重置，因此"认证闸门拒绝"会以 nil 留下可判读的痕迹。
	var (
		downstream  error
		handlerHits int
	)
	handler := guard.Wrap(func(w http.ResponseWriter, r *http.Request, _ core.Actor) {
		handlerHits++
		sessionID := r.Header.Get(sessionKey)
		err := h.limiter.AdmitIngress(r.Context(), h.principal(caller), callers.IngressRun)
		if err == nil {
			req := h.requestFor(caller)
			req.SessionID = sessionID
			_, err = h.service.Run(r.Context(), req)
		}
		downstream = err
		if err != nil {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	do := func(source, token, session string) *httptest.ResponseRecorder {
		t.Helper()
		downstream = nil
		req := httptest.NewRequest(http.MethodPost, "http://"+allowedHost+"/api/v1/chat", http.NoBody)
		req.Host = allowedHost
		req.RemoteAddr = source
		req.Header.Set("Authorization", "Bearer "+token)
		if session != "" {
			req.Header.Set(sessionKey, session)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// —— 准备：一个已完成的 Run（证明整条链路是通的），并把入口速率收紧到只剩突发一发 ——
	// 0.01 QPS 的下一发令牌要 100 秒后才回填：第 2 段判的是"桶已空"，不依赖用例时长。
	h.setIngressQPS(t, caller, callers.IngressRun, 0.01)
	first, err := h.postChat(context.Background(), caller)
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, first.Status, "准备用的 Run 必须真实执行完成")
	require.Equal(t, 1, h.countRuns(t, caller))

	// —— [1] AUTH_RATE_LIMITED：9 次凭据失败后，第 10 次在凭据校验**之前**被拒 ——
	for i := 1; i <= 9; i++ {
		rec := do(authSource, "wrong-token", "")
		require.Equalf(t, http.StatusUnauthorized, rec.Code, "第 %d 次凭据失败应为 401", i)
		require.Emptyf(t, rec.Header().Get("Retry-After"), "第 %d 次凭据失败不应带 Retry-After", i)
	}
	hitsBefore := handlerHits
	limited := do(authSource, adminToken, "")
	require.Equal(t, http.StatusTooManyRequests, limited.Code, "认证限速后即使凭据正确也必须 429")
	require.NotEmpty(t, limited.Header().Get("Retry-After"), "认证限速必须带 Retry-After")
	require.Equal(t, hitsBefore, handlerHits, "认证限速的请求不得进入下游")
	require.Nil(t, downstream, "下游一次都不许被调用：AUTH_RATE_LIMITED 先于一切准入判定")
	var authEnvelope struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(limited.Body.Bytes(), &authEnvelope))
	require.Equal(t, "AUTH_RATE_LIMITED", authEnvelope.Code)
	require.Equal(t, 1, h.countRuns(t, caller), "认证拒绝不得写 Run 行")
	require.Zero(t, h.concurrencyInUse(t, caller), "认证拒绝不得占名额")

	// —— [2] CALLER_RATE_LIMITED：桶已空，入口速率先于名额与会话判据 ——
	rec := do(okSource, adminToken, "")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "CALLER_RATE_LIMITED", admissionCodeOf(downstream),
		"桶已空时必须由入口速率拒绝，不得落到名额 / 会话判据")
	require.Equal(t, 1, h.ingressRejections(t, caller))
	require.Zero(t, h.concurrencyInUse(t, caller), "入口速率拒绝不得占名额")
	require.Equal(t, 1, h.countRuns(t, caller), "入口速率拒绝不得写 Run 行")

	// —— 占位：一个真实在跑的 Run（持名额、持会话、占实例计数） ——
	type runOutcome struct {
		res core.RunResult
		err error
	}
	h.provider.blockUntilCanceled()
	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	t.Cleanup(cancelBlocked)
	blockedDone := make(chan runOutcome, 1)
	go func() {
		res, err := h.service.Run(blockedCtx, h.requestForSession(caller, busySession))
		blockedDone <- runOutcome{res: res, err: err}
	}()
	require.Eventually(t, func() bool { return h.provider.calls() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"被占住的 Run 必须已经进入模型调用")
	require.Equal(t, 1, h.concurrencyInUse(t, caller), "占位 Run 必须持有一个名额")
	runsBefore := h.countRuns(t, caller)
	require.Equal(t, 2, runsBefore, "此时账本里应有：准备好的 Run + 占位 Run")

	// 名额上限压到 1，入口桶放宽——后续每一段都只留一个更靠前的判据。
	h.setMaxConcurrent(t, caller, 1)
	h.tickLimitCache()
	h.setIngressQPS(t, caller, callers.IngressRun, 1000)

	// —— [3] CALLER_CONCURRENCY_LIMITED：名额已满（接口面首判） ——
	rec = do(okSource, adminToken, "")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "CALLER_CONCURRENCY_LIMITED", admissionCodeOf(downstream),
		"名额满时必须由调用方并发拒绝，不得落到会话 / 实例判据")
	require.Equal(t, 1, h.concurrencyInUse(t, caller), "名额拒绝不得多占名额")
	require.Equal(t, runsBefore, h.countRuns(t, caller), "名额拒绝不得写 Run 行")

	// —— [4] SESSION_BUSY：名额放开、会话仍被占 ——
	h.setMaxConcurrent(t, caller, 8)
	h.tickLimitCache()
	rec = do(okSource, adminToken, busySession)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "SESSION_BUSY", admissionCodeOf(downstream),
		"会话被占时必须由会话门禁拒绝，不得落到实例上限")
	require.Equal(t, 1, h.concurrencyInUse(t, caller))
	require.Equal(t, runsBefore, h.countRuns(t, caller))

	// —— [5] TOO_MANY_RUNS：换一个空会话，实例计数已满 ——
	rec = do(okSource, adminToken, "")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "TOO_MANY_RUNS", admissionCodeOf(downstream),
		"实例满时必须由实例上限拒绝，不得落到配额")
	require.Equal(t, 1, h.concurrencyInUse(t, caller))
	require.Equal(t, runsBefore, h.countRuns(t, caller))

	// —— [6] QUOTA_EXHAUSTED：腾出名额与会话，只留"任务次数额度 = 0" ——
	h.provider.respondWithText("precedence-done")
	cancelBlocked()
	outcome := <-blockedDone
	require.NoError(t, outcome.err, "取消是运行期终止，走 RunResult")
	require.Equal(t, core.StatusCancelled, outcome.res.Status)
	require.Eventually(t, func() bool { return h.concurrencyInUse(t, caller) == 0 }, 5*time.Second, 10*time.Millisecond,
		"取消路径必须归还名额")

	h.setTaskLimit(t, caller, 0)
	rec = do(okSource, adminToken, "")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "QUOTA_EXHAUSTED", admissionCodeOf(downstream),
		"额度为零时必须在权威准入事务里失败关闭")
	require.Zero(t, h.concurrencyInUse(t, caller), "额度拒绝不得占名额")
	require.Equal(t, runsBefore, h.countRuns(t, caller), "额度拒绝不得写 Run 行")
	require.Equal(t, map[string]int{"committed": 2}, h.taskReservations(t, caller),
		"被拒的准入不得留下半截任务预留（整笔事务回滚）")
}

// TestRun_InstanceCapStaysPerWorker 判 FR-24-08：`max_concurrent_runs` 是**每个
// Worker** 的实例上限，不是全局上限。
//
// 判据面：同一套 PostgreSQL 端口上派生两个 Worker，各自上限 1。A 占满自己的实例
// 计数后，B 必须仍能开始自己的 Run；A 的第三次请求被 `*core.InstanceFullError`
// 拒绝（读数带本 Worker 的 1），且不写 Run 行。逆改点：把实例计数改成全局（例如
// 落库计数或包级共享），B 的 Run 起不来，`calls() >= 2` 超时变红。
// Two workers share one database but keep independent instance counters.
func TestRun_InstanceCapStaysPerWorker(t *testing.T) {
	h := newRunHarness(t)
	const caller = runHarnessCaller

	workerA := h.workerService(1)
	workerB := h.workerService(1)

	type runOutcome struct {
		res core.RunResult
		err error
	}

	h.provider.blockUntilCanceled()

	ctxA, cancelA := context.WithCancel(context.Background())
	t.Cleanup(cancelA)
	doneA := make(chan runOutcome, 1)
	go func() {
		res, err := workerA.Run(ctxA, h.requestFor(caller))
		doneA <- runOutcome{res: res, err: err}
	}()
	require.Eventually(t, func() bool { return h.provider.calls() >= 1 }, 5*time.Second, 10*time.Millisecond,
		"Worker A 的 Run 必须进入模型调用并占住 A 的实例计数")

	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	doneB := make(chan runOutcome, 1)
	go func() {
		res, err := workerB.Run(ctxB, h.requestFor(caller))
		doneB <- runOutcome{res: res, err: err}
	}()
	require.Eventually(t, func() bool { return h.provider.calls() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"第二个 Worker 必须能开始自己的 Run：实例上限不是全局的")

	// A 自己已满：第三次请求在 A 的准入处被拒，不排队、不落账。
	runsBefore := h.countRuns(t, caller)
	require.Equal(t, 2, runsBefore, "两个 Worker 各自写了一条 Run 行")
	_, err := workerA.Run(context.Background(), h.requestFor(caller))
	var full *core.InstanceFullError
	require.ErrorAs(t, err, &full)
	require.Equal(t, 1, full.Limit, "拒绝必须带回本 Worker 的实例上限读数")
	require.Equal(t, runsBefore, h.countRuns(t, caller), "被实例上限拒绝的请求不得写 Run 行")

	// 收尾：两个在跑的 Run 都以取消结束，名额与会话全部归还。
	cancelA()
	cancelB()
	gotA := <-doneA
	require.NoError(t, gotA.err, "取消是运行期终止，走 RunResult")
	require.Equal(t, core.StatusCancelled, gotA.res.Status)
	gotB := <-doneB
	require.NoError(t, gotB.err, "取消是运行期终止，走 RunResult")
	require.Equal(t, core.StatusCancelled, gotB.res.Status)
	require.Zero(t, h.concurrencyInUse(t, caller))
}

// TestRun_CallerConcurrencyPrecedesLocalSessionBusy 判模块卡 §4.5 的合并负向用例：
// **跨 Worker 并发超限 + 本地会话忙**同时成立时只返回 `CALLER_CONCURRENCY_LIMITED`。
//
// 判据面：两个 Worker 共用一套 PostgreSQL，调用方上限 2。A、B 各跑一条后共享库的
// `runs(status='running')` 计数已到 2；此时再向 A 发同一会话的请求——A 的进程内忙集
// 确实命中了该会话，但本地忙集只看得见本进程，权威判据是库里的并发计数。
// 逆改点：把 `core.AgentService.Run` 的 [4] 预检挪回 `s.admit` 之后（或让本地会话判定
// 先跑），返回值变成 `*core.SessionBusyError`——与 FR-24-34 及图 2 的顺序相反。
// The cross-worker caller-concurrency verdict must outrank the process-local busy set.
func TestRun_CallerConcurrencyPrecedesLocalSessionBusy(t *testing.T) {
	h := newRunHarness(t)
	const caller = runHarnessCaller

	// 调用方上限 2：两个 Worker 各占一条即满额。
	h.setMaxConcurrent(t, caller, 2)
	h.tickLimitCache()

	workerA := h.workerService(4)
	workerB := h.workerService(4)

	type runOutcome struct {
		res core.RunResult
		err error
	}
	h.provider.blockUntilCanceled()

	ctxA, cancelA := context.WithCancel(context.Background())
	t.Cleanup(cancelA)
	doneA := make(chan runOutcome, 1)
	go func() {
		res, err := workerA.Run(ctxA, h.requestForSession(caller, "sess-local"))
		doneA <- runOutcome{res: res, err: err}
	}()
	require.Eventually(t, func() bool { return h.provider.calls() >= 1 }, 5*time.Second, 10*time.Millisecond,
		"Worker A 的 Run 必须进入模型调用并占住本进程的会话忙集")

	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	doneB := make(chan runOutcome, 1)
	go func() {
		res, err := workerB.Run(ctxB, h.requestForSession(caller, "sess-remote"))
		doneB <- runOutcome{res: res, err: err}
	}()
	require.Eventually(t, func() bool { return h.provider.calls() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"Worker B 的 Run 必须进入模型调用：共享库里的调用方名额由此满额")

	runsBefore := h.countRuns(t, caller)
	require.Equal(t, 2, runsBefore, "两个 Worker 各写了一条 running 行")

	// 同一会话再向 A 发起：本地忙集与跨 Worker 并发同时命中。
	_, err := workerA.Run(context.Background(), h.requestForSession(caller, "sess-local"))
	require.ErrorIs(t, err, core.ErrCallerConcurrencyLimited,
		"并发超限与会话忙同时成立时只返回 CALLER_CONCURRENCY_LIMITED（FR-24-34）")
	var busy *core.SessionBusyError
	require.False(t, errors.As(err, &busy),
		"本地会话忙集不得抢在跨 Worker 的并发判定之前（图 2 的顺序）")
	require.Equal(t, runsBefore, h.countRuns(t, caller), "被拒的请求零占用，不写 Run 行")

	// 收尾：两条在跑的 Run 以取消结束，名额全部归还。
	cancelA()
	cancelB()
	gotA := <-doneA
	require.NoError(t, gotA.err, "取消是运行期终止，走 RunResult")
	require.Equal(t, core.StatusCancelled, gotA.res.Status)
	gotB := <-doneB
	require.NoError(t, gotB.err, "取消是运行期终止，走 RunResult")
	require.Equal(t, core.StatusCancelled, gotB.res.Status)
}

// TestRunAdmission_CallerConcurrencyPrecedesInflightCapacity 判入口层的同一顺序：
// `worker.max_inflight` 满额与实例上限同码（`TOO_MANY_RUNS`），不得抢在跨 Worker 的
// `CALLER_CONCURRENCY_LIMITED` 之前（FR-24-34 / FR-24-40）。
//
// 判据面：共享库的调用方名额已满；入口名额池只有 1 格且被后台执行占住。请求必须由
// **调用方并发**拒绝——`runAdmission.run` 在取在途名额之前先做 [4] 预检。
// 逆改点：把 `precheckAdmission` 挪到 `acquireInflight` 之后（或删掉），返回的是
// `*core.InstanceFullError`；把 `run` 的 next 真的调起来同样红。
// The pooled slot is a capacity verdict and must not outrank the caller-concurrency one.
func TestRunAdmission_CallerConcurrencyPrecedesInflightCapacity(t *testing.T) {
	h := newRunHarness(t)
	const caller = runHarnessCaller

	h.setMaxConcurrent(t, caller, 2)
	h.tickLimitCache()

	// 两个 Worker 各跑一条，把共享库里的调用方名额占满。它们直接走 core 入口，
	// 不经过本用例要判的入口名额池。
	workerA := h.workerService(4)
	workerB := h.workerService(4)

	type runOutcome struct {
		res core.RunResult
		err error
	}
	h.provider.blockUntilCanceled()

	ctxA, cancelA := context.WithCancel(context.Background())
	t.Cleanup(cancelA)
	doneA := make(chan runOutcome, 1)
	go func() {
		res, err := workerA.Run(ctxA, h.requestForSession(caller, "sess-a"))
		doneA <- runOutcome{res: res, err: err}
	}()
	require.Eventually(t, func() bool { return h.provider.calls() >= 1 }, 5*time.Second, 10*time.Millisecond,
		"Worker A 的 Run 必须进入模型调用")

	ctxB, cancelB := context.WithCancel(context.Background())
	t.Cleanup(cancelB)
	doneB := make(chan runOutcome, 1)
	go func() {
		res, err := workerB.Run(ctxB, h.requestForSession(caller, "sess-b"))
		doneB <- runOutcome{res: res, err: err}
	}()
	require.Eventually(t, func() bool { return h.provider.calls() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"Worker B 的 Run 必须进入模型调用：共享库里的调用方名额由此满额")

	// 入口名额池：唯一一格已被后台执行占住。交互请求若先判容量，会得到 TOO_MANY_RUNS。
	budget := newInflightLimiter(1)
	hold, holdErr := budget.acquireBackground(context.Background())
	require.NoError(t, holdErr)
	defer hold()

	admission := &runAdmission{
		limiter:   h.limiter,
		inflight:  budget,
		runs:      h.serviceOptions.Runs,
		admission: h.serviceOptions.Admission,
	}
	_, err := admission.run(context.Background(), h.requestForSession(caller, "sess-new"),
		func(context.Context, core.RunRequest) (core.RunResult, error) {
			t.Fatal("预检必须拒绝：被拒的请求不得进入 Run")
			return core.RunResult{}, nil
		})
	require.ErrorIs(t, err, core.ErrCallerConcurrencyLimited,
		"在途名额满与并发超限同时成立时只返回 CALLER_CONCURRENCY_LIMITED（FR-24-34）")
	var full *core.InstanceFullError
	require.False(t, errors.As(err, &full),
		"容量类拒绝不得抢在调用方并发判定之前（FR-24-40 与图 2 的顺序）")

	// 收尾：两条在跑的 Run 以取消结束。
	cancelA()
	cancelB()
	gotA := <-doneA
	require.NoError(t, gotA.err, "取消是运行期终止，走 RunResult")
	require.Equal(t, core.StatusCancelled, gotA.res.Status)
	gotB := <-doneB
	require.NoError(t, gotB.err, "取消是运行期终止，走 RunResult")
	require.Equal(t, core.StatusCancelled, gotB.res.Status)
}

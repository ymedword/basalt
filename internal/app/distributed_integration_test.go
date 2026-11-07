//go:build integration

package app

// 本文件承载 US-24 的**跨 Worker 故障装配**（`harness/stages/us24-fault.json` 的
// `us24-fault-app` 主判据，模块卡 §4 的接管 / 失权 / 会话忙 / 取消 / 投递 / 停机
// 六类场景）。装配形态刻意贴近生产：两个 `core.AgentService` 实例各自持独立的
// Worker 身份与进程内会话表、共享同一套隔离 PG schema；租约、准入、续约与回收全部
// 走真实实现，测试只做种行与断言，不代为领取、不代为提交。
//
// 判据设计的三条纪律：
//   - **不用真实时间窗**：过期由 `expireLeaseForTest` 直接改到过去，取消由
//     `RequestCancel` 写标志，接管由 `ClaimExpired` 显式领取——没有一条依赖
//     "等 TTL 自然到期"。
//   - **不依赖 sleep 顺序**：跨 goroutine 的等待一律走通道（`done` / `start`），
//     唯一的时间上界是防挂死的 20s 兜底。
//   - **判可观察结果，不判内部时序**：并发回收后只断言最终状态与"收尾时刻不再
//     被改写"，不断言哪一轮先提交（SKIP LOCKED 的胜者本就不可预测）。
//
// This file is the cross-worker fault rig: two services with distinct worker
// identities sharing one isolated PG schema, exercising the six scenario classes
// from the module card with real leases, admission, renewal and reclamation.

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

const (
	// distributedLeaseTTL 是测试用租约时长：10s 足够覆盖一次同步准入 + 首个模型调用，
	// 又让"失权"用例能在毫秒级以显式过期推进，不用等真实 TTL。
	// distributedLeaseTTL keeps leases alive across a synchronous admission while
	// letting the ownership-loss case expire explicitly.
	distributedLeaseTTL = 10 * time.Second
	// distributedRenewEvery 是续约周期：失权与远端取消都靠它在 100ms 量级被观察到。
	// distributedRenewEvery lets ownership loss and remote cancel be observed quickly.
	distributedRenewEvery = 100 * time.Millisecond
)

// distributedWorkerIdentity 构造一个合法 Worker 身份（注册要求 InstanceID 与
// SchemaVersion）。Name 与 InstanceID 同值：FR-24-30 允许重名，身份只认 InstanceID。
// distributedWorkerIdentity builds a valid identity; the name may repeat, the id is the identity.
func distributedWorkerIdentity(instanceID string) core.WorkerIdentity {
	return core.WorkerIdentity{
		InstanceID:    instanceID,
		Name:          instanceID,
		Version:       "test",
		SchemaVersion: postgres.SupportedSchemaVersion,
	}
}

// distributedReclaimer 用指定实例身份装配真实回收器（与 `appNewReclaimer` 同一组
// 依赖，只有 Worker 身份不同——接管必须由**另一个** Worker 完成才叫接管）。
// distributedReclaimer builds the real reclaimer under the given identity.
func distributedReclaimer(t *testing.T, db *postgres.DB, instanceID string) *leaseReclaimer {
	t.Helper()

	reclaimer, err := newLeaseReclaimer(reclaimerDeps{
		DB:       db,
		Flows:    &flow.Service{},
		Leases:   postgres.NewLeaseStore(db),
		Worker:   distributedWorkerIdentity(instanceID),
		LeaseTTL: startupReclaimTTL,
		Learning: postgres.NewLearningRepo(db),
	})
	require.NoError(t, err)
	return reclaimer
}

// distributedOwnedService 从夹具派生一个**接了执行权**的服务实例：Worker 身份、
// 租约端口与续约参数一起给齐（`Run` 里三者缺一即装配错误），Run 仓储换成测试用
// 短 TTL 的版本。会话表每实例一份——两个 Worker 是两个进程，除数据库外没有共享
// 内存；共用同一张 map 会把"跨 Worker 串行"悄悄降级成"同进程互斥"。
// distributedOwnedService derives a lease-wired service instance with its own
// in-process session table; the database is the only shared boundary.
func distributedOwnedService(h *runHarness, worker core.WorkerIdentity) *core.AgentService {
	opts := h.serviceOptions
	opts.Worker = worker
	opts.Leases = postgres.NewLeaseStore(h.db)
	opts.Runs = postgres.NewRunStoreWithLeaseTTL(h.db, distributedLeaseTTL)
	opts.RenewInterval = distributedRenewEvery
	opts.LeaseTTL = distributedLeaseTTL
	opts.Sessions = &runHarnessSessions{busy: make(map[string]bool)}
	return core.NewAgentService(opts)
}

// seedDistributedCallerKey 给夹具调用方补一把仍有效的调用方 Key（含该 Agent 的调用
// scope）。真实准入由 Key 授权，恢复器在收尾前会复核**那一把** Key；不补的话接管
// 收尾会如实记成 `owner_unauthorized`（授权已不在），而不是本用例要判的
// `worker_lost`——那是另一条判据，不能在这里顶替。
// seedDistributedCallerKey seeds the still-valid caller key that recovery re-checks
// before closing a taken-over run.
func seedDistributedCallerKey(t *testing.T, h *runHarness) {
	t.Helper()

	now := time.Now().UTC().UnixMilli()
	_, err := h.db.ExecContext(t.Context(), `
INSERT INTO callers (caller_id, kind, name, project, status, created_at)
VALUES ($1, 'service', $1, '', 'active', $2)
ON CONFLICT (caller_id) DO NOTHING`, runHarnessCaller, now)
	require.NoError(t, err)
	_, err = h.db.ExecContext(t.Context(), `
INSERT INTO caller_keys (key_id, caller_id, key_hash, key_prefix, scopes, created_at)
VALUES ($1, $2, $3, $4, ARRAY[$5]::text[], $6)
ON CONFLICT (key_id) DO NOTHING`,
		runHarnessCaller+"-key", runHarnessCaller, "hash-"+runHarnessCaller,
		"bk_"+runHarnessCaller, core.AgentInvokeScopePrefix+h.agent, now)
	require.NoError(t, err)
}

// distributedRunResult 是一次异步运行的返回值。运行期取消 / 失败走 `RunResult`，
// 只有准入与装配错误经 error——判据据此区分"拒绝"与"跑完"。
type distributedRunResult struct {
	res core.RunResult
	err error
}

// runDistributedAsync 在后台跑一次运行，用独立 context（取消必须由租约 / 标志驱动，
// 不能由测试的清理阶段顺带触发）。
// runDistributedAsync runs one request in the background on its own context.
func runDistributedAsync(svc *core.AgentService, req core.RunRequest) <-chan distributedRunResult {
	done := make(chan distributedRunResult, 1)
	go func() {
		res, err := svc.Run(context.Background(), req)
		done <- distributedRunResult{res: res, err: err}
	}()
	return done
}

// awaitDistributedRun 等待后台运行结束。20s 只是防挂死兜底，不承担任何时序判定。
// awaitDistributedRun waits for the background run; the 20s bound only prevents a hang.
func awaitDistributedRun(t *testing.T, done <-chan distributedRunResult) core.RunResult {
	t.Helper()

	select {
	case out := <-done:
		require.NoError(t, out.err, "运行期取消 / 失败走 RunResult，不经 error")
		return out.res
	case <-time.After(20 * time.Second):
		t.Fatal("等待跨 Worker 运行结束超时")
		return core.RunResult{}
	}
}

// waitFirstProviderCall 等"运行确实抵达模型调用"。它同时盯运行本身：模型调用之前
// 就结束的运行不是本用例要判的时序，直接带出原因失败，而不是把等待变成挂死。
// waitFirstProviderCall fails fast when the run ends before its first model call.
func waitFirstProviderCall(t *testing.T, h *runHarness, done <-chan distributedRunResult) {
	t.Helper()

	select {
	case <-h.provider.firstCall:
	case out := <-done:
		t.Fatalf("运行在首次模型调用前结束：err=%v status=%s reason=%s",
			out.err, out.res.Status, out.res.Reason)
	case <-time.After(30 * time.Second):
		t.Fatal("等待首次模型调用超时")
	}
}

// distributedCancelRequested 读跨 Worker 取消标志。
func distributedCancelRequested(t *testing.T, db *postgres.DB, runID string) bool {
	t.Helper()

	var requested bool
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT cancel_requested FROM runs WHERE run_id = $1`, runID).Scan(&requested))
	return requested
}

// TestDistributed_TakeoverAfterCrash 判接管与隔离（FR-24-02 / 15 / 16 / 39）：
// 崩溃 Worker 的过期租约被合法领取并按既有恢复规则收尾；同一轮里活 Worker 的在跑
// 行、审计与租约**一字不动**；第二轮不再命中任何东西。
// TestDistributed_TakeoverAfterCrash closes only the legally claimed expired lease
// and leaves a live worker's running row untouched.
func TestDistributed_TakeoverAfterCrash(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := t.Context()

	// A 崩溃：running 行 + 两条 pending 审计 + 已过期未释放的租约。
	appSeedRun(t, db, "run-crash", "sess-crash", "worker-a")
	appSeedPendingAudit(t, db, "run-crash")
	expireLeaseForTest(t, db, core.LeaseRun, "run-crash")

	// B 活着：同形 running 行，租约未过期。
	appSeedRun(t, db, "run-live", "sess-live", "worker-b")
	appSeedPendingAudit(t, db, "run-live")

	reclaimer := distributedReclaimer(t, db, "worker-b")
	report, err := reclaimer.ReclaimOnce(ctx, 8)
	require.NoError(t, err)
	require.Equal(t, 1, report.Runs, "只收尾合法领取的过期租约（启动不做全局扫描）")
	require.Equal(t, 2, report.PendingAudit, "崩溃 Run 的两条 pending 审计都必须归一化")
	require.Zero(t, report.Executions)
	require.Zero(t, report.Errors)

	require.Equal(t, string(core.StatusFailed), appRunStatus(t, db, "run-crash"))
	require.Equal(t, "worker_lost", appRunReason(t, db, "run-crash"))
	require.Equal(t, string(core.LLMCallUnknown), appAuditStatus(t, db, "llm_calls", "run-crash"))
	require.Equal(t, string(core.LLMCallUnknown), appAuditStatus(t, db, "tool_invocations", "run-crash"))

	// 活 Worker 的在跑行不得被"顺手"改写成失败（FR-24-02）。
	require.Equal(t, string(core.StatusRunning), appRunStatus(t, db, "run-live"))
	require.Empty(t, appRunReason(t, db, "run-live"))
	require.Equal(t, string(core.LLMCallPending), appAuditStatus(t, db, "llm_calls", "run-live"))
	require.Equal(t, string(core.LLMCallPending), appAuditStatus(t, db, "tool_invocations", "run-live"))
	requireLeaseUntouched(t, db, core.LeaseRun, "run-live", "worker-b")

	// 第二轮：正常路径命中数为 0，活行仍不受影响。
	second, err := reclaimer.ReclaimOnce(ctx, 8)
	require.NoError(t, err)
	require.Zero(t, second.Runs)
	require.Zero(t, second.PendingAudit)
	require.Zero(t, second.Executions)
	requireLeaseUntouched(t, db, core.LeaseRun, "run-live", "worker-b")
}

// TestDistributed_StaleOwnerWritesRejected 判失权（FR-24-04 / 06 / 36）：租约过期后
// 即使尚未被接管，旧属主也不得提交终态；B 合法接管并按作用域收尾后，A 恢复重试仍被
// `ErrLeaseLost` 拒绝，不得覆盖 B 的结果。
// TestDistributed_StaleOwnerWritesRejected fences the old owner out even before
// takeover and keeps the stale write rejected after the new owner closes the run.
func TestDistributed_StaleOwnerWritesRejected(t *testing.T) {
	h := newRunHarness(t)
	ctx := t.Context()
	svcA := distributedOwnedService(h, distributedWorkerIdentity("worker-a"))
	seedDistributedCallerKey(t, h)

	h.provider.blockUntilCanceled()
	req := h.requestForSession(runHarnessCaller, "sess-stale")
	req.RunID = "run-stale-owner"
	done := runDistributedAsync(svcA, req)
	waitFirstProviderCall(t, h, done)

	// 模拟 A 暂停超过租约时长：租约过期，但**还没人接管**。
	expireLeaseForTest(t, h.db, core.LeaseRun, "run-stale-owner")

	res := awaitDistributedRun(t, done)
	require.Equal(t, core.StatusCancelled, res.Status, "失权必须让属主尽快停手")
	require.Equal(t, string(core.StatusRunning), appRunStatus(t, h.db, "run-stale-owner"),
		"失权后的终态提交不得落库")

	lease := readDistributedLease(t, h.db, core.LeaseRun, "run-stale-owner")
	require.Equal(t, "worker-a", lease.Owner)
	require.False(t, lease.Released, "失权不等于交还：租约保持已过期未释放，交给回收器接管")
	stale := core.LeaseToken{
		Kind:           core.LeaseRun,
		ResourceID:     "run-stale-owner",
		OwnerWorkerID:  lease.Owner,
		OwnershipEpoch: lease.Epoch,
	}

	runs := postgres.NewRunStoreWithLeaseTTL(h.db, distributedLeaseTTL)
	applied, err := runs.FinishRun(ctx, "run-stale-owner", stale,
		core.StatusCompleted, "stale-owner-overwrite", 1, 0, time.Now().UTC())
	require.ErrorIs(t, err, core.ErrLeaseLost, "过期后即使未被接管，旧属主也不得提交")
	require.False(t, applied)
	require.Equal(t, string(core.StatusRunning), appRunStatus(t, h.db, "run-stale-owner"))

	// B 以合法领取接管：过期未释放的租约必须报告为真正的接管（TookOver=true）。
	claimed, err := postgres.NewReclaimerStore(h.db).ClaimExpired(
		ctx, core.LeaseRun, distributedWorkerIdentity("worker-b"), 4, startupReclaimTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.True(t, claimed[0].TookOver)

	report, err := postgres.RecoverRun(ctx, h.db, distributeLogger(), claimed[0].LeaseToken,
		time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, 1, report.ClosedRuns)
	require.Equal(t, string(core.StatusFailed), appRunStatus(t, h.db, "run-stale-owner"))
	require.Equal(t, "worker_lost", appRunReason(t, h.db, "run-stale-owner"))
	require.Zero(t, appPendingAuditCount(t, h.db, "run-stale-owner"),
		"接管收尾必须把 pending 审计归一化")

	// A 恢复后再试一次：仍被拒，B 的结果不得被覆盖。
	applied, err = runs.FinishRun(ctx, "run-stale-owner", stale,
		core.StatusCompleted, "stale-owner-overwrite", 1, 0, time.Now().UTC())
	require.ErrorIs(t, err, core.ErrLeaseLost)
	require.False(t, applied)
	require.Equal(t, string(core.StatusFailed), appRunStatus(t, h.db, "run-stale-owner"))
	require.Equal(t, "worker_lost", appRunReason(t, h.db, "run-stale-owner"))
}

// TestDistributed_SessionBusyAcrossWorkers 判跨 Worker 会话串行（FR-24-07 / 34 /
// 43）：A 在共享会话上运行期间，B 的准入在数据库内被拒（409 SESSION_BUSY），且被拒
// 请求**零占用**；A 正常收尾释放租约后，B 的同会话重试必须成功。
// TestDistributed_SessionBusyAcrossWorkers rejects the second worker's admission
// on a busy session with zero occupancy and lets the retry succeed after closeout.
func TestDistributed_SessionBusyAcrossWorkers(t *testing.T) {
	h := newRunHarness(t)
	ctx := t.Context()
	svcA := distributedOwnedService(h, distributedWorkerIdentity("session-a"))
	svcB := distributedOwnedService(h, distributedWorkerIdentity("session-b"))

	h.provider.blockUntilCanceled()
	reqA := h.requestForSession(runHarnessCaller, "sess-shared")
	reqA.RunID = "run-busy-a"
	doneA := runDistributedAsync(svcA, reqA)
	waitFirstProviderCall(t, h, doneA)

	reqB := h.requestForSession(runHarnessCaller, "sess-shared")
	reqB.RunID = "run-busy-b"
	_, err := svcB.Run(ctx, reqB)
	require.Error(t, err, "同一会话跨 Worker 不得并行")
	var busy *core.SessionBusyError
	require.ErrorAs(t, err, &busy)
	require.ErrorIs(t, err, core.ErrConflict)
	require.Equal(t, "sess-shared", busy.SessionID)
	require.Equal(t, 1, h.countRuns(t, runHarnessCaller),
		"被拒的准入必须零占用：不得留下第二个 Run 行")

	detail, err := svcA.Cancel(ctx, "run-busy-a")
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, detail.Status)
	require.Equal(t, core.StatusCancelled, awaitDistributedRun(t, doneA).Status)

	h.provider.respondWithText("done")
	resB, err := svcB.Run(ctx, h.requestForSession(runHarnessCaller, "sess-shared"))
	require.NoError(t, err, "上一 Run 收尾并释放租约后，同会话必须可以重新准入")
	require.Equal(t, core.StatusCompleted, resB.Status)
	require.Equal(t, 2, h.countRuns(t, runHarnessCaller))
}

// TestDistributed_RemoteCancelObserved 判跨 Worker 取消（FR-24-22）：非属主只写
// `runs.cancel_requested` 并如实返回当前状态（仍是 running），不假装取消成功；
// 属主在续约 / 检查点观察后收敛为 cancelled；重复取消幂等。
// TestDistributed_RemoteCancelObserved writes the flag without faking success, and
// the owner converges to cancelled; a second cancel is idempotent.
func TestDistributed_RemoteCancelObserved(t *testing.T) {
	h := newRunHarness(t)
	ctx := t.Context()
	svcA := distributedOwnedService(h, distributedWorkerIdentity("cancel-a"))
	svcB := distributedOwnedService(h, distributedWorkerIdentity("cancel-b"))

	h.provider.blockUntilCanceled()
	reqA := h.requestForSession(runHarnessCaller, "sess-remote-cancel")
	reqA.RunID = "run-remote-cancel"
	doneA := runDistributedAsync(svcA, reqA)
	waitFirstProviderCall(t, h, doneA)

	// B 不是属主：只写标志，返回写后的真实状态。
	detail, err := svcB.Cancel(ctx, "run-remote-cancel")
	require.NoError(t, err)
	require.Equal(t, core.StatusRunning, detail.Status,
		"非属主不得把「请求已写入」伪装成「已取消」")
	require.True(t, distributedCancelRequested(t, h.db, "run-remote-cancel"))
	require.Equal(t, string(core.StatusRunning), appRunStatus(t, h.db, "run-remote-cancel"))

	// 属主观察到标志后收敛为 cancelled。
	require.Equal(t, core.StatusCancelled, awaitDistributedRun(t, doneA).Status)
	require.Equal(t, string(core.StatusCancelled), appRunStatus(t, h.db, "run-remote-cancel"))
	var finishedAt sql.NullInt64
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT finished_at FROM runs WHERE run_id = $1`, "run-remote-cancel").Scan(&finishedAt))
	require.True(t, finishedAt.Valid, "取消后的终态必须落收尾时刻")

	again, err := svcB.Cancel(ctx, "run-remote-cancel")
	require.NoError(t, err)
	require.Equal(t, core.StatusCancelled, again.Status, "重复取消幂等")
}

// TestDistributed_DeliverySettledOnce 判投递结算（FR-24-21 / 39）：两个回收器并发
// 跑同一 schema，终态 Run 的 pending 投递 → unknown、无 Run 的前置投递 → skipped，
// 两者都**永不重发**（attempted_at 为空）；执行行收为 failed + worker_lost；
// 第三轮不再改写任何状态与收尾时刻——结算恰好一次。
// TestDistributed_DeliverySettledOnce lets two reclaimers race and proves the
// deliveries settle once, never resend, and are not rewritten afterwards.
func TestDistributed_DeliverySettledOnce(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := t.Context()

	// ① 终态 Run 的 pending 投递：模型已出正文，投递是否发出无从证明 → unknown。
	appSeedTerminalRun(t, db, "run-delivery-done", core.StatusCompleted)
	appSeedScheduledTask(t, db, "task-delivery-done")
	appSeedPendingDelivery(t, db, "run-delivery-done", "task-delivery-done", 0)

	// ② Run 创建前的 pending 投递：属主失联、执行租约过期 → skipped。
	appSeedScheduledTask(t, db, "task-delivery-pre")
	preExec := appSeedStartedExecution(t, db, "task-delivery-pre", sql.NullString{}, "worker-gone")
	appSeedPendingDelivery(t, db, "run-delivery-pre", "task-delivery-pre", preExec)
	expireLeaseForTest(t, db, core.LeaseScheduleExecution, strconv.FormatInt(preExec, 10))

	// 两个回收器同时起跑：SKIP LOCKED 决定谁领到执行租约，判据只看最终状态。
	reclaimers := []*leaseReclaimer{
		distributedReclaimer(t, db, "worker-b"),
		distributedReclaimer(t, db, "worker-c"),
	}
	start := make(chan struct{})
	errs := make(chan error, len(reclaimers))
	var wg sync.WaitGroup
	for _, reclaimer := range reclaimers {
		wg.Add(1)
		go func(reclaimer *leaseReclaimer) {
			defer wg.Done()
			<-start
			_, err := reclaimer.ReclaimOnce(ctx, 8)
			errs <- err
		}(reclaimer)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "并发回收轮次不得报错（单条失败只计数）")
	}

	pre := appDeliveryRecord(t, db, "run-delivery-pre")
	require.Equal(t, core.DeliverySkipped, pre.Status)
	require.Equal(t, "运行未创建，未投递", pre.Detail)
	require.True(t, pre.AttemptedAt.IsZero(), "B2 永不重发")

	done := appDeliveryRecord(t, db, "run-delivery-done")
	require.Equal(t, core.DeliveryUnknown, done.Status)
	require.Equal(t, "进程在投递完成前退出，投递结果未知", done.Detail)
	require.True(t, done.AttemptedAt.IsZero(), "B1 永不重发")

	require.Equal(t, string(core.ExecutionFailed), appExecutionStatus(t, db, preExec))
	require.Equal(t, "worker_lost", appExecutionError(t, db, preExec))

	// 第三轮：没有可修的残留，且已结算行的状态、明细与收尾时刻一字不改。
	third, err := appNewReclaimer(t, db).ReclaimOnce(ctx, 8)
	require.NoError(t, err)
	require.Zero(t, third.Runs)
	require.Zero(t, third.Executions)
	require.Zero(t, third.PendingAudit)

	preAgain := appDeliveryRecord(t, db, "run-delivery-pre")
	require.Equal(t, pre.Status, preAgain.Status)
	require.Equal(t, pre.Detail, preAgain.Detail)
	require.True(t, preAgain.FinishedAt.Equal(pre.FinishedAt),
		"结算恰好一次：收尾时刻不得被后续轮次改写")
	doneAgain := appDeliveryRecord(t, db, "run-delivery-done")
	require.Equal(t, done.Status, doneAgain.Status)
	require.Equal(t, done.Detail, doneAgain.Detail)
	require.True(t, doneAgain.FinishedAt.Equal(done.FinishedAt),
		"结算恰好一次：收尾时刻不得被后续轮次改写")
}

// TestDistributed_ShutdownOrder 判停机（FR-24-27 / 40）：两个 Worker 各自登记、
// 各自心跳；先停的那个写下线标记、拒绝继续心跳，另一个**不受牵连**；已下线实例的
// 名册行保留（可读），停机可以重复调用。
// TestDistributed_ShutdownOrder stops one worker without touching the other and
// keeps the registry row readable.
func TestDistributed_ShutdownOrder(t *testing.T) {
	db := pgtest.NewTestDB(t)
	ctx := t.Context()

	identityA := distributedWorkerIdentity("shutdown-a")
	identityB := distributedWorkerIdentity("shutdown-b")
	appInstance := &App{logger: distributeLogger()}

	runtimeA, err := appInstance.startWorkerRuntime(ctx, db, identityA,
		distributedReclaimer(t, db, identityA.InstanceID))
	require.NoError(t, err)
	runtimeB, err := appInstance.startWorkerRuntime(ctx, db, identityB,
		distributedReclaimer(t, db, identityB.InstanceID))
	require.NoError(t, err)
	t.Cleanup(func() {
		// Stop 幂等：用例内已停过的实例再停一次不得报错（判据本身也是重启路径的前提）。
		require.NoError(t, runtimeA.Stop(context.Background()))
		require.NoError(t, runtimeB.Stop(context.Background()))
	})

	registry := postgres.NewWorkerStore(db)
	require.Equal(t, 1, appWorkerNodeCount(t, db, identityA.InstanceID))
	require.Equal(t, 1, appWorkerNodeCount(t, db, identityB.InstanceID))
	require.NoError(t, registry.Heartbeat(ctx, identityA.InstanceID))
	require.NoError(t, registry.Heartbeat(ctx, identityB.InstanceID))

	// 先停 A：下线标记落库，B 继续在线并继续心跳。
	require.NoError(t, runtimeA.Stop(context.Background()))
	require.Zero(t, appWorkerNodeCount(t, db, identityA.InstanceID), "停机必须写下线标记")
	require.Equal(t, 1, appWorkerNodeCount(t, db, identityB.InstanceID),
		"另一个 Worker 不得被连带下线")
	require.Error(t, registry.Heartbeat(ctx, identityA.InstanceID), "已下线实例拒绝继续心跳")
	require.NoError(t, registry.Heartbeat(ctx, identityB.InstanceID))

	// 名册保留两条：下线行只是 stopped_at 有值，仍然可读（FR-24-01）。
	nodes, err := registry.ListNodes(ctx)
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	stoppedAt := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		stoppedAt[node.InstanceID] = node.StoppedAt != nil
	}
	require.True(t, stoppedAt[identityA.InstanceID], "已下线行的 stopped_at 必须可读")
	require.False(t, stoppedAt[identityB.InstanceID], "在线行不得有下线标记")

	require.NoError(t, runtimeB.Stop(context.Background()))
	require.Zero(t, appWorkerNodeCount(t, db, identityB.InstanceID))
}

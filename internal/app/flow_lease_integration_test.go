//go:build integration

package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// 本文件落地模块卡 §4.4.5 / §4.4.6 的跨 Worker 执行权用例（T022）。断言只读两件事：
// `postgres.SubtreeGate` 的三个持久判据给出的取值，以及 `flow.Worker` 实际走过的调用
// 顺序与落库事实。用例不重新实现任何判据，也不绕开执行权——需要推进的地方不是经
// `advanceUnderFlowRoot`（显式持根租约），就是经真实 `flow.Worker` 领取。
//
// These cases cover the cross-worker execution-right suite. Every predicate is the
// production one: the three SubtreeGate queries, flow.Service.Recover/Advance, and the
// real flow.Worker acquire path.

const (
	// distributedWorkerTTL / distributedRenewInterval 让"手工制造租约过期"保持确定性：
	// 一轮推进在毫秒级结束，30s 的续约周期绝不会先写一次续约，因此 expire* 之后
	// 不会出现"被 keeper 悄悄续活"的竞态。
	// A long renew interval keeps manual expiry deterministic inside one round.
	distributedWorkerTTL     = 2 * time.Minute
	distributedRenewInterval = 30 * time.Second
)

// crashedOwnerIdentity 是被中断的属主留下的领取者身份：它领过租约，之后再没续约。
func crashedOwnerIdentity() core.WorkerIdentity {
	return core.WorkerIdentity{InstanceID: "crashed-owner", Name: "crashed-owner"}
}

// gateLive / gateInFlight / gateUnresolved 是三个判据的断言包装。三者一一对应
// FR-24-20 的 Isolated 反面、Joined 反面与 R7 的门 ③，**不得**互相推导。
func gateLive(t *testing.T, gate core.SubtreeGate, rootTaskID string) bool {
	t.Helper()
	live, err := gate.HasLiveRunLease(t.Context(), rootTaskID)
	require.NoError(t, err)
	return live
}

func gateInFlight(t *testing.T, gate core.SubtreeGate, rootTaskID string) bool {
	t.Helper()
	inflight, err := gate.HasInFlightNodeAttempt(t.Context(), rootTaskID)
	require.NoError(t, err)
	return inflight
}

func gateUnresolved(t *testing.T, gate core.SubtreeGate, rootTaskID string) bool {
	t.Helper()
	pending, err := gate.HasUnresolvedAction(t.Context(), rootTaskID)
	require.NoError(t, err)
	return pending
}

// TestFlow_LeaseExpiryIsolatesButDoesNotJoin 对应模块卡 §4.4.5 的
// `TestFlow_LeaseExpiryIsolatesButDoesNotJoin`：Isolated（子树已无有效 run 租约）
// 与 Joined（子树已无 prepared / running attempt）必须**各自独立**取值。
func TestFlow_LeaseExpiryIsolatesButDoesNotJoin(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-isolated")
	task := startDistributedTask(t, service, definition, "isolated-root")
	attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-isolated", "run-isolated", "session-isolated", 1)
	// 终态行 + 未释放的 run 租约 = 属主写完终态就崩溃的现场：结论已落库，执行权没交还。
	seedDistributedRun(t, db, running, attempt, "completed")
	acquireDistributedLease(t, db, core.LeaseRun, attempt.RunID, crashedOwnerIdentity(), 5*time.Minute)
	gate := postgres.NewSubtreeGate(db)

	require.True(t, gateLive(t, gate, task.RootTaskID))
	require.True(t, gateInFlight(t, gate, task.RootTaskID))
	require.False(t, gateUnresolved(t, gate, task.RootTaskID))
	isolated, err := runner.Isolated(t.Context(), running)
	require.NoError(t, err)
	require.False(t, isolated, "活 run 租约下子树未隔离，Recover 不得收敛 attempt")
	joined, err := runner.Joined(t.Context(), running)
	require.NoError(t, err)
	require.False(t, joined)

	// 租约过期：Isolated 翻真，Joined 仍为假——两个判据各自取值，谁也不能推谁。
	expireDistributedLease(t, db, core.LeaseRun, attempt.RunID)
	require.False(t, gateLive(t, gate, task.RootTaskID))
	isolated, err = runner.Isolated(t.Context(), running)
	require.NoError(t, err)
	require.True(t, isolated, "租约过期即隔离成立，Recover 才能动子树")
	require.True(t, gateInFlight(t, gate, task.RootTaskID))
	joined, err = runner.Joined(t.Context(), running)
	require.NoError(t, err)
	require.False(t, joined, "在途 attempt 未收敛前不得放行 Advance")

	// interrupted / unknown 是复用 / 查证输入，**不**计入 Joined（FR-24-20）：把它们
	// 计入就等于永远等不到放行，只能由 Recover 清理。
	for _, state := range []string{"interrupted", "unknown"} {
		setDistributedAttemptState(t, db, attempt.ID, state)
		require.False(t, gateInFlight(t, gate, task.RootTaskID), "state=%s", state)
		joined, err = runner.Joined(t.Context(), running)
		require.NoError(t, err)
		require.True(t, joined, "state=%s 不计入 Joined", state)
	}

	// 门 ③ 只认 outcome ∈ dispatching / unknown；prepared 与四个已决值都不算未决。
	seedUnresolvedAction(t, db, running, attempt, "act-isolated", "prepared")
	require.False(t, gateUnresolved(t, gate, task.RootTaskID))
	for _, outcome := range []string{"dispatching", "unknown"} {
		setDistributedActionOutcome(t, db, "act-isolated", outcome)
		require.True(t, gateUnresolved(t, gate, task.RootTaskID), "outcome=%s", outcome)
	}
	for _, outcome := range []string{"succeeded", "failed", "not_issued"} {
		setDistributedActionOutcome(t, db, "act-isolated", outcome)
		require.False(t, gateUnresolved(t, gate, task.RootTaskID), "outcome=%s", outcome)
	}
}

// TestFlow_JoinedAfterRecoverResolvesAttempt 验证两轮收敛：第一轮 Isolated=true 把
// 子树 prepared / running → interrupted，结论仍是 Blocked；第二轮没有在途 attempt
// 才允许写 Queued（推进类结论要根租约），随后按运行证据复用已完成的叶子。
func TestFlow_JoinedAfterRecoverResolvesAttempt(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-joined")
	task := startDistributedTask(t, service, definition, "joined-root")
	attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-joined", "run-joined", "session-joined", 1)
	seedDistributedRun(t, db, running, attempt, "completed")
	acquireDistributedLease(t, db, core.LeaseRun, attempt.RunID, crashedOwnerIdentity(), 5*time.Minute)
	expireDistributedLease(t, db, core.LeaseRun, attempt.RunID)

	blocked, err := service.Recover(t.Context(), flow.ResumeRequest{
		Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: running.Version,
		OperationID: "recover-joined-1", Reason: "worker_lease_expired",
	})
	require.NoError(t, err)
	require.Equal(t, flow.Blocked, blocked.Status)
	require.Equal(t, "interrupted", distributedAttemptStateOf(db, attempt.ID), "Isolated=true 必须收敛整棵子树")

	queued, err := service.Recover(t.Context(), flow.ResumeRequest{
		Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: blocked.Version,
		OperationID: "recover-joined-2", Reason: "worker_lease_expired",
	})
	require.NoError(t, err)
	require.Equal(t, flow.Queued, queued.Status, "没有在途 attempt 才允许写 Queued")

	finished, err := advanceUnderFlowRoot(t, db, service, task.RootTaskID, flow.AdvanceRequest{
		Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: queued.Version,
	})
	require.NoError(t, err)
	require.Equal(t, flow.Succeeded, finished.Status)
	require.Zero(t, runner.calls.Load(), "已完成的节点按运行证据复用，不得重跑")
	require.Equal(t, 1, countDistributedAttempts(t, db, task.ID), "复用不新增 attempt")
	require.Equal(t, 1, countDistributedNodeRuns(t, db, task.RootTaskID), "复用不新增 Run")
}

// TestFlow_CompletedRunReusedNotReexecuted 对照两个方向：终态完成 → 复用（零新
// attempt / 零次执行）；终态失败 → 重新领取（新 attempt + 一次真实执行）。
func TestFlow_CompletedRunReusedNotReexecuted(t *testing.T) {
	t.Run("completed-run-reused", func(t *testing.T) {
		db := pgtest.NewTestDB(t)
		runner := newDistributedRunner(db)
		service := newDistributedService(db, runner)
		store := postgres.NewFlowStore(db)
		definition := publishDistributedDefinition(t, db, "probe-reuse")
		task := startDistributedTask(t, service, definition, "reuse-root")
		attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-reuse", "run-reuse", "session-reuse", 1)
		seedDistributedRun(t, db, running, attempt, "completed")
		// 旧代次执行已确定退出（interrupted 只能由 Recover 写入），推进方因此可以直接复用。
		setDistributedAttemptState(t, db, attempt.ID, "interrupted")

		finished, err := advanceUnderFlowRoot(t, db, service, task.RootTaskID, flow.AdvanceRequest{
			Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: running.Version,
		})
		require.NoError(t, err)
		require.Equal(t, flow.Succeeded, finished.Status)
		require.Zero(t, runner.calls.Load(), "复用路径不得派发新 Run")
		require.Equal(t, 1, countDistributedAttempts(t, db, task.ID))
		require.Equal(t, 1, countDistributedNodeRuns(t, db, task.RootTaskID))
	})

	t.Run("failed-run-reexecuted", func(t *testing.T) {
		db := pgtest.NewTestDB(t)
		runner := newDistributedRunner(db)
		service := newDistributedService(db, runner)
		store := postgres.NewFlowStore(db)
		// 节点失败上限抬到 2：生产 Claim 按合同预算判上限，但"可重试一次"才让失败
		// 结论有意义——第一次失败了，第二次领取要有据可依。
		definition := distributedDefinition()
		definition.Nodes[0].Failure.MaxAttempts = 2
		published := publishDistributedDefinitionWith(t, db, "probe-reexecute", definition)
		task := startDistributedTask(t, service, published, "reexecute-root")
		attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-reexecute", "run-reexecute", "session-reexecute", 1)
		seedDistributedRun(t, db, running, attempt, "failed")
		setDistributedAttemptState(t, db, attempt.ID, "failed")

		finished, err := advanceUnderFlowRoot(t, db, service, task.RootTaskID, flow.AdvanceRequest{
			Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: running.Version,
		})
		require.NoError(t, err)
		require.Equal(t, flow.Succeeded, finished.Status)
		require.EqualValues(t, 1, runner.calls.Load(), "失败的叶子必须重新执行一次")
		require.Equal(t, 2, countDistributedAttempts(t, db, task.ID))
	})
}

// TestFlow_TakeoverRecoversBeforeAdvance 验证接管分路：属主崩溃后，接管者必须先
// Recover（第一轮结论 Blocked、attempt 收敛、租约**不**释放），再由回收器 A3 按
// reason 重入 Queued，最后以正常交棒推进到 Succeeded。
func TestFlow_TakeoverRecoversBeforeAdvance(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-takeover")
	task := startDistributedTask(t, service, definition, "takeover-root")
	attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-takeover", "run-takeover", "session-takeover", 1)
	seedDistributedRun(t, db, running, attempt, "running")
	acquireDistributedLease(t, db, core.LeaseRun, attempt.RunID, crashedOwnerIdentity(), 5*time.Minute)
	expireDistributedLease(t, db, core.LeaseRun, attempt.RunID)
	crashedRoot := acquireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID, crashedOwnerIdentity(), 5*time.Minute)
	expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)

	repo := newDistributedRepository(store, task.RootTaskID, nil)
	workerService := newDistributedService(db, runner)
	workerService.Repository = repo // Service 与 Worker 必须看同一个装饰器，否则调用顺序观察不到
	identity := core.WorkerIdentity{InstanceID: "worker-takeover", Name: "worker-takeover"}
	stop := startDistributedWorker(t, workerService, repo, db, identity, distributedWorkerTTL, distributedRenewInterval)
	t.Cleanup(stop)

	require.Eventually(t, func() bool {
		return distributedAttemptStateOf(db, attempt.ID) == "interrupted"
	}, 15*time.Second, 20*time.Millisecond, "接管者必须先 Recover 并收敛子树")
	require.Equal(t, []string{"recover"}, repo.events())
	blocked := readTaskForDistributedTest(t, store, task.ID)
	require.Equal(t, flow.Blocked, blocked.Status)
	require.Equal(t, "worker_lease_expired", blocked.Reason)
	row := readDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)
	require.False(t, row.Released, "Advance 被 Blocked 拒绝后必须 Abandon，不得释放租约")
	require.EqualValues(t, crashedRoot.OwnershipEpoch+1, row.Epoch)
	require.Zero(t, runner.calls.Load(), "接管轮只查证，不派发新 Run")

	// 回收器 A3（§2.3.6 / FR-24-37）：按 reason=worker_lease_expired 重入 queued。
	// 它自己领取根租约（零 token 走 withRootLease），用完释放——不是 Worker 的接管轮。
	expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)
	reclaimer := newDistributedService(db, runner)
	reclaimer.Identity = core.WorkerIdentity{InstanceID: "flow-reclaimer", Name: "flow-reclaimer"}
	requeued, err := reclaimer.Recover(t.Context(), flow.ResumeRequest{
		Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: blocked.Version,
		OperationID: "reclaim-requeue", Reason: "worker_lease_expired",
	})
	require.NoError(t, err)
	require.Equal(t, flow.Queued, requeued.Status)

	require.Eventually(t, func() bool {
		current, err := store.ReadForWorker(context.Background(), task.ID)
		return err == nil && current.Status == flow.Succeeded
	}, 20*time.Second, 20*time.Millisecond, "重入 queued 之后 Worker 以正常交棒推进")
	require.Equal(t, []string{"recover", "claim"}, repo.events(), "接管先 Recover、重入后才领取")
	// 被中断的叶子**没有**完成证据（Run 停在 running），因此重入后必须重新执行一次；
	// "已完成按运行证据复用"由 `TestFlow_CompletedRunReusedNotReexecuted` 单独覆盖。
	// The interrupted leaf has no completion evidence, so re-execution is correct.
	require.EqualValues(t, 1, runner.calls.Load(), "被中断的叶子必须重新执行一次")
	require.Equal(t, 2, countDistributedAttempts(t, db, task.ID))
}

// TestFlow_TransientRecoverErrorRetriesRecoverBeforeAdvance 验证"Recover 出错不释放
// 租约"：第一轮接管 Recover 失败 → Abandon（租约仍在），下一轮仍是 TookOver=true，
// 必须先 Recover 才谈得上推进——整轮不得出现任何一次 claim。
func TestFlow_TransientRecoverErrorRetriesRecoverBeforeAdvance(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-transient")
	task := startDistributedTask(t, service, definition, "transient-root")
	attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-transient", "run-transient", "session-transient", 1)
	seedDistributedRun(t, db, running, attempt, "running")
	acquireDistributedLease(t, db, core.LeaseRun, attempt.RunID, crashedOwnerIdentity(), 5*time.Minute)
	expireDistributedLease(t, db, core.LeaseRun, attempt.RunID)
	crashedRoot := acquireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID, crashedOwnerIdentity(), 5*time.Minute)
	expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)

	// 注入的失败取 `core.ErrPreconditionFailed`：它正是 `Service.Recover` 在版本被
	// 并发推进时返回的错误，Worker 循环按契约**不** ParkTask（并发的写者会继续推进），
	// 因此这里观察到的仍是"同一根被重新以接管方式处理"，而不是被 park 成 advance_unavailable。
	// The injected failure is the version-conflict sentinel Service.Recover really returns.
	repo := newDistributedRepository(store, task.RootTaskID, core.ErrPreconditionFailed)
	workerService := newDistributedService(db, runner)
	workerService.Repository = repo
	identity := core.WorkerIdentity{InstanceID: "worker-transient", Name: "worker-transient"}
	stop := startDistributedWorker(t, workerService, repo, db, identity, distributedWorkerTTL, distributedRenewInterval)
	t.Cleanup(stop)

	require.Eventually(t, func() bool { return repo.recoverCalls() >= 1 }, 15*time.Second, 20*time.Millisecond)
	require.Equal(t, []string{"recover"}, repo.events())
	row := readDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)
	require.False(t, row.Released, "Recover 出错结论未定，不得交还租约")
	require.EqualValues(t, crashedRoot.OwnershipEpoch+1, row.Epoch)
	require.Equal(t, "running", distributedAttemptStateOf(db, attempt.ID), "失败的 Recover 不得收敛 attempt")
	current := readTaskForDistributedTest(t, store, task.ID)
	require.Equal(t, flow.Running, current.Status, "失败的 Recover 不得改写任务头")
	require.EqualValues(t, 1, distributedTakeovers(db, core.LeaseFlowRoot, task.RootTaskID))

	// 租约到期 → 下一轮仍是接管：必须再走一次 Recover，不得直接 Advance。
	expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)
	require.Eventually(t, func() bool { return repo.recoverCalls() >= 2 }, 20*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		return distributedAttemptStateOf(db, attempt.ID) == "interrupted"
	}, 15*time.Second, 20*time.Millisecond)
	require.Equal(t, []string{"recover", "recover"}, repo.events(), "整轮都在 Recover 分支")
	require.EqualValues(t, 2, distributedTakeovers(db, core.LeaseFlowRoot, task.RootTaskID))
	require.NotContains(t, repo.events(), "claim")
}

// TestFlow_NormalHandoffRefusesUnresolvedAction 验证门 ③：普通 Worker 以真实
// TookOver=false 领到含未决动作的根时，既不派发新 Run / 动作、也不写审计新行，且
// **不释放**租约——把下一轮逼成 TookOver=true 走 Recover 查证。
func TestFlow_NormalHandoffRefusesUnresolvedAction(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-pending")
	task := startDistributedTask(t, service, definition, "pending-root")
	attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-pending", "run-pending", "session-pending", 1)
	seedDistributedRun(t, db, running, attempt, "running")
	// 旧执行已退出、run 租约也已过期，但根租约在 claim 后被正常释放——下一轮是正常交棒。
	acquireDistributedLease(t, db, core.LeaseRun, attempt.RunID, crashedOwnerIdentity(), 5*time.Minute)
	expireDistributedLease(t, db, core.LeaseRun, attempt.RunID)
	setDistributedAttemptState(t, db, attempt.ID, "interrupted")
	seedUnresolvedAction(t, db, running, attempt, "act-pending", "dispatching")
	before := countDistributedRows(t, db, task.RootTaskID)

	repo := newDistributedRepository(store, task.RootTaskID, nil)
	workerService := newDistributedService(db, runner)
	workerService.Repository = repo
	identity := core.WorkerIdentity{InstanceID: "worker-pending", Name: "worker-pending"}
	stop := startDistributedWorker(t, workerService, repo, db, identity, distributedWorkerTTL, distributedRenewInterval)
	t.Cleanup(stop)

	require.Eventually(t, func() bool {
		return distributedLeaseSnapshot(db, core.LeaseFlowRoot, task.RootTaskID).Owner == identity.InstanceID
	}, 15*time.Second, 20*time.Millisecond, "Worker 必须先领到根租约")
	require.Zero(t, distributedTakeovers(db, core.LeaseFlowRoot, task.RootTaskID), "已释放的租约是正常交棒，不是接管")
	require.Empty(t, repo.events(), "TookOver=false 不走 Recover 分支")
	require.False(t, readDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID).Released,
		"门 ③ 命中必须 Abandon：释放会让下一轮永远绕开 Recover")
	require.Equal(t, before, countDistributedRows(t, db, task.RootTaskID), "未决动作之上不得写任何新行")
	require.Zero(t, runner.calls.Load())
	require.Equal(t, "dispatching", distributedActionOutcome(db, "act-pending"))

	// 租约到期后下一轮必为接管：先经 Recover 的未决动作查证，仍不推进。
	expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)
	require.Eventually(t, func() bool {
		return readDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID).Released == false &&
			distributedTakeovers(db, core.LeaseFlowRoot, task.RootTaskID) >= 1
	}, 20*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { return repo.unknownCalls() >= 1 }, 15*time.Second, 20*time.Millisecond,
		"接管必须先做未决动作查证")
	require.NotContains(t, repo.events(), "claim")
	require.Equal(t, before, countDistributedRows(t, db, task.RootTaskID))
	require.Equal(t, "dispatching", distributedActionOutcome(db, "act-pending"))
}

// TestFlow_InFlightAttemptWithUnresolvedActionAbandons 验证 C-13 在**崩溃中途**的
// 现场成立：属主在模型调用在途时崩溃，同时留下 `running` attempt 与 `dispatching`
// 动作。`Service.Recover` 在未决动作上写库前就返回，这个 attempt 因此永远不会被
// 收敛——若门 ② 先判，它每轮都给出 Release，下一轮就是正常交棒、永远绕开 Recover，
// 根被锁在 running 并按轮询频率反复领取 / 释放（T038 双进程演练实测 2s 内 477 条
// 租约事件）。门 ③ 必须先判：命中即 Abandon（released_at IS NULL），过期后下一轮
// 必为 TookOver=true → 重跑 Recover。
func TestFlow_InFlightAttemptWithUnresolvedActionAbandons(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-crash-midcall")
	task := startDistributedTask(t, service, definition, "crash-midcall-root")
	attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-midcall", "run-midcall", "session-midcall", 1)
	seedDistributedRun(t, db, running, attempt, "running")
	// 崩溃现场：run 租约过期，attempt 仍是 running（Recover 因未决动作不会收敛它）。
	acquireDistributedLease(t, db, core.LeaseRun, attempt.RunID, crashedOwnerIdentity(), 5*time.Minute)
	expireDistributedLease(t, db, core.LeaseRun, attempt.RunID)
	seedUnresolvedAction(t, db, running, attempt, "act-midcall", "dispatching")
	before := countDistributedRows(t, db, task.RootTaskID)
	gate := postgres.NewSubtreeGate(db)
	require.False(t, gateLive(t, gate, task.RootTaskID))
	require.True(t, gateInFlight(t, gate, task.RootTaskID))
	require.True(t, gateUnresolved(t, gate, task.RootTaskID))

	repo := newDistributedRepository(store, task.RootTaskID, nil)
	workerService := newDistributedService(db, runner)
	workerService.Repository = repo
	identity := core.WorkerIdentity{InstanceID: "worker-midcall", Name: "worker-midcall"}
	stop := startDistributedWorker(t, workerService, repo, db, identity, distributedWorkerTTL, distributedRenewInterval)
	t.Cleanup(stop)

	require.Eventually(t, func() bool {
		return distributedLeaseSnapshot(db, core.LeaseFlowRoot, task.RootTaskID).Owner == identity.InstanceID
	}, 15*time.Second, 20*time.Millisecond, "Worker 必须先领到根租约")
	// 门 ③ 必须先于门 ②：attempt 仍在途也不能 Release——一旦释放，下一轮就是正常交棒。
	require.Never(t, func() bool {
		return readDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID).Released
	}, time.Second, 20*time.Millisecond, "未决动作命中必须 Abandon：释放会让下一轮永远绕开 Recover")
	require.Empty(t, repo.events(), "TookOver=false 不走 Recover 分支")
	require.Equal(t, before, countDistributedRows(t, db, task.RootTaskID), "未决动作之上不得写任何新行")
	require.Equal(t, "dispatching", distributedActionOutcome(db, "act-midcall"))

	// 过期后下一轮必为接管：即使 attempt 仍 running，也必须进 Recover 查证。
	expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)
	require.Eventually(t, func() bool { return repo.unknownCalls() >= 1 }, 20*time.Second, 20*time.Millisecond,
		"接管必须先做未决动作查证")
	require.EqualValues(t, 1, distributedTakeovers(db, core.LeaseFlowRoot, task.RootTaskID))
	require.NotContains(t, repo.events(), "claim")
	require.Equal(t, before, countDistributedRows(t, db, task.RootTaskID))
	require.Equal(t, "dispatching", distributedActionOutcome(db, "act-midcall"))
	require.Equal(t, "running", distributedAttemptStateOf(db, attempt.ID),
		"Recover 在写库前返回，attempt 必须保持原样（由未决动作的查证结果决定）")
}

// TestFlow_BlockedRootWithUnknownActionNotQueued 验证回收分路（FR-24-37）：带未决
// 动作的 blocked 根**不是**就绪根，回收器不得私自重入 queued；同形状但
// reason=bounded_rework_required 的根才是就绪根。
func TestFlow_BlockedRootWithUnknownActionNotQueued(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-ready")
	task := startDistributedTask(t, service, definition, "ready-root")
	attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-ready", "run-ready", "session-ready", 1)
	seedDistributedRun(t, db, running, attempt, "running")
	acquireDistributedLease(t, db, core.LeaseRun, attempt.RunID, crashedOwnerIdentity(), 5*time.Minute)
	expireDistributedLease(t, db, core.LeaseRun, attempt.RunID)

	// 先做出"接管轮已把根停成 blocked"的现场，再追加一条未决动作：`Service.Recover`
	// 在 `HasUnknownActions` 为真时**一行都不写**（R5 口径：结论是"有未决动作"，不是
	// "本轮已置 blocked"），所以未决动作不能在 Recover 之前就位。
	blocked, err := service.Recover(t.Context(), flow.ResumeRequest{
		Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: running.Version,
		OperationID: "recover-ready", Reason: "worker_lease_expired",
	})
	require.NoError(t, err)
	require.Equal(t, flow.Blocked, blocked.Status)
	require.Equal(t, "worker_lease_expired", blocked.Reason, "reason 是 A3 唯一的分路依据")
	seedUnresolvedAction(t, db, running, attempt, "act-ready", "unknown")
	require.True(t, gateUnresolved(t, postgres.NewSubtreeGate(db), task.RootTaskID),
		"未决动作必须留在库里，回收器据此拒绝重入")

	// `now` 必须在**每次**扫描前现取：`flow_tasks.next_eligible_at` 由 Start 写成创建
	// 时刻，用更早的 `now` 会把后建出来的根判成"还没到可领取时间"。
	// Capture now fresh per scan: next_eligible_at is the creation timestamp.
	readyIDs := func() []string {
		ready, err := store.ReadyRoots(t.Context(), time.Now().UnixMilli(), 32)
		require.NoError(t, err)
		return distributedIDs(ready)
	}
	require.NotContains(t, readyIDs(), task.ID, "blocked + worker_lease_expired 不是就绪根")
	open, err := store.OpenRoots(t.Context(), 32)
	require.NoError(t, err)
	require.Contains(t, distributedIDs(open), task.ID, "关闭扫描仍须看到该根")

	bounded := startDistributedTask(t, service, definition, "ready-bounded")
	_, err = db.ExecContext(t.Context(), `UPDATE flow_tasks SET status='blocked', reason='bounded_rework_required' WHERE task_id=$1`, bounded.ID)
	require.NoError(t, err)
	ready := readyIDs()
	require.Contains(t, ready, bounded.ID, "受控返工的 blocked 根必须可再领取")
	require.NotContains(t, ready, task.ID)
}

// TestFlow_CreateRunForNodeRejectsForeignRootOrStaleRevision 验证子 Run 创建的三条
// 前置（FR-24-37 / P-05）：持根 A 的执行者不得为根 B 下的 attempt 建 Run，旧
// revision 的 attempt 也不得建——两种都在写入前返回 ErrFlowAttemptStale。
func TestFlow_CreateRunForNodeRejectsForeignRootOrStaleRevision(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definitionA := publishDistributedDefinition(t, db, "probe-run-a")
	definitionB := publishDistributedDefinition(t, db, "probe-run-b")
	rootA := startDistributedTask(t, service, definitionA, "run-node-a")
	rootB := startDistributedTask(t, service, definitionB, "run-node-b")
	attempt, running := claimDistributedAttempt(t, db, store, rootB, "attempt-run-b", "run-b", "session-b", 1)
	// 执行者此刻合法持有根 B 的租约：拒绝必须来自归属 / 代次判定，而不是"没租约"。
	token := acquireFlowRootForTest(t, db, rootB.RootTaskID)
	t.Cleanup(func() { releaseFlowRootForTest(t, db, token) })
	tokenA := acquireFlowRootForTest(t, db, rootA.RootTaskID)
	t.Cleanup(func() { releaseFlowRootForTest(t, db, tokenA) })
	runs := postgres.NewRunStore(db)
	record := func(runID string, flowCtx *core.FlowRunContext) core.RunRecord {
		return core.RunRecord{
			RunID: runID, SessionID: "", Agent: "writer", Channel: "cli",
			UserID: "flow:" + rootB.CallerID, Principal: core.LocalAdminCLIPrincipal(),
			ConfigHash: distributedConfigHash, StartedAt: time.Now(), Flow: flowCtx,
		}
	}
	base := &core.FlowRunContext{
		TaskID: rootB.ID, RootTaskID: rootB.RootTaskID, NodeInvocationID: attempt.NodeInvocationID,
		NodeAttemptID: attempt.ID, TaskRevision: running.Version.Revision,
		Lease:    token,
		AgentUID: distributedAgentUID, AgentConfigHash: distributedConfigHash, ResourceManifestHash: distributedManifestHash,
	}
	runRows := func(runID string) int {
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs WHERE run_id=$1`, runID).Scan(&count))
		return count
	}

	// 持根 A 的租约（rootToken.ResourceID=根 A）为根 B 下的 attempt 建 Run：
	// 父根归属判定必须拒绝（§2.3.6 CreateRunForNode 第 ③ 步）。
	foreign := *base
	foreign.RootTaskID = rootA.RootTaskID
	foreign.Lease = tokenA
	_, err := runs.StartRun(t.Context(), record("run-foreign-root", &foreign), core.RunResourceManifest{},
		core.WorkerIdentity{}, core.CallerAdmission{}, "run-foreign-root")
	require.ErrorIs(t, err, flow.ErrFlowAttemptStale, "持根 A 不得为根 B 下的 attempt 建 Run")
	require.Zero(t, runRows("run-foreign-root"))

	stale := *base
	stale.TaskRevision = running.Version.Revision + 1
	_, err = runs.StartRun(t.Context(), record("run-stale-revision", &stale), core.RunResourceManifest{},
		core.WorkerIdentity{}, core.CallerAdmission{}, "run-stale-revision")
	require.ErrorIs(t, err, flow.ErrFlowAttemptStale, "旧 revision 的 attempt 必须在写入前被拒")
	require.Zero(t, runRows("run-stale-revision"))

	// 租约已被另一 Worker 持有（owner 不符）→ ErrLeaseLost，子 Run 不得建成。
	// A live lease owned by another worker must surface as ErrLeaseLost.
	otherOwner := *base
	otherOwner.Lease.OwnerWorkerID = "another-worker"
	_, err = runs.StartRun(t.Context(), record("run-other-owner", &otherOwner), core.RunResourceManifest{},
		core.WorkerIdentity{}, core.CallerAdmission{}, "run-other-owner")
	require.ErrorIs(t, err, core.ErrLeaseLost, "owner 不符的凭证不得创建子 Run")
	require.Zero(t, runRows("run-other-owner"))

	// 同一 owner 的旧代次（接管前取得、接管后 ownership_epoch 已递增）→ ErrLeaseLost：
	// 只比 owner 不比代次会放行旧持有者的残留凭证。
	// A pre-takeover epoch must fail even when the owner name matches the live row.
	oldEpoch := *base
	oldEpoch.Lease.OwnershipEpoch = token.OwnershipEpoch - 1
	_, err = runs.StartRun(t.Context(), record("run-old-epoch", &oldEpoch), core.RunResourceManifest{},
		core.WorkerIdentity{}, core.CallerAdmission{}, "run-old-epoch")
	require.ErrorIs(t, err, core.ErrLeaseLost, "旧代次的凭证不得创建子 Run")
	require.Zero(t, runRows("run-old-epoch"))
}

// readTaskForDistributedTest 以 Worker 视角重读任务头（不经过管理面权限）。
func readTaskForDistributedTest(t *testing.T, store *postgres.FlowStore, taskID string) flow.Task {
	t.Helper()
	task, err := store.ReadForWorker(t.Context(), taskID)
	require.NoError(t, err)
	return task
}

// distributedIDs 取任务 ID 列表，便于对就绪 / 关闭扫描做包含性断言。
func distributedIDs(tasks []flow.Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ID)
	}
	return ids
}

// 以下三个读取器**不使用 require**：它们只在 `require.Eventually` 的条件函数里跑，
// 而条件函数在独立 goroutine 中执行，`t.FailNow` 在那里不成立。取不到时返回零值，
// 由调用方的断言给出结论。
// These readers avoid require: Eventually runs its condition in another goroutine.

func distributedLeaseSnapshot(db *postgres.DB, kind core.LeaseKind, resourceID string) distributedLeaseRow {
	var row distributedLeaseRow
	err := db.QueryRowContext(context.Background(), `SELECT owner_worker_id, ownership_epoch, released_at IS NOT NULL
  FROM execution_leases WHERE kind=$1 AND resource_id=$2`, string(kind), resourceID).
		Scan(&row.Owner, &row.Epoch, &row.Released)
	if err != nil {
		return distributedLeaseRow{}
	}
	return row
}

func distributedAttemptStateOf(db *postgres.DB, attemptID string) string {
	var state string
	if err := db.QueryRowContext(context.Background(),
		`SELECT state FROM flow_node_attempts WHERE node_attempt_id=$1`, attemptID).Scan(&state); err != nil {
		return ""
	}
	return state
}

func distributedTakeovers(db *postgres.DB, kind core.LeaseKind, resourceID string) int {
	var count int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM lease_events WHERE kind=$1 AND resource_id=$2 AND event='takeover'`,
		string(kind), resourceID).Scan(&count); err != nil {
		return -1
	}
	return count
}

func distributedActionOutcome(db *postgres.DB, actionID string) string {
	var outcome string
	if err := db.QueryRowContext(context.Background(),
		`SELECT outcome FROM flow_actions WHERE action_id=$1`, actionID).Scan(&outcome); err != nil {
		return ""
	}
	return outcome
}

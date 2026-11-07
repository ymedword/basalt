//go:build integration

package app

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// 本文件落地 T023 的五个用例（模块卡 §4.4.7 / FR-24-37 / FR-24-38）：
//
//   - 管理面 resume 撞上执行者的活根租约 → `ErrFlowRootBusy`，一行不写，租约释放后同一请求成功；
//   - 回收器 A3 对 blocked 根按 reason 分路（worker_lease_expired 重入 queued、
//     bounded_rework_required 不动、子 Run 租约仍有效不动作）；
//   - 未决动作命中时 A3 每轮"领取 → 查证 → 交还"，保护不依赖租约；
//   - cancel / deadline 两条关闭路径在 Isolated 时收敛整棵子树的 attempt，
//     且 deadline 的终态是 stopped 不是 cancelled。
//
// These cases pin the A3 routing and the close-path convergence. Every predicate is the
// production one (`Service.Recover` / `Service.Cancel` / `flow.Worker` / `leaseReclaimer`);
// the fixtures only seed persisted facts they cannot obtain through a legal write path.

// liveRootHolderIdentity 是"另一个 Worker 正持有根租约"的现场身份。
// A non-zero identity is what makes Acquire write the lease row.
func liveRootHolderIdentity() core.WorkerIdentity {
	return core.WorkerIdentity{InstanceID: "worker-live-root", Name: "worker-live-root"}
}

// newFlowReclaimer 组装一个真正覆盖 A3 的回收器：`Flows` 必须是完整的 Flow 服务，
// 不能像 `appNewReclaimer` 那样只放一个空壳（那批用例的库里没有 flow_root 租约）。
// repo 非空时替换服务的仓储，用于观察 `HasUnknownActions` 是否被调用。
func newFlowReclaimer(t *testing.T, db *postgres.DB, service *flow.Service, repo flow.Repository) *leaseReclaimer {
	t.Helper()
	if repo != nil {
		service.Repository = repo
	}
	reclaimer, err := newLeaseReclaimer(reclaimerDeps{
		DB:       db,
		Flows:    service,
		Leases:   postgres.NewLeaseStore(db),
		Worker:   appReclaimerIdentity(),
		LeaseTTL: startupReclaimTTL,
		Learning: postgres.NewLearningRepo(db),
	})
	require.NoError(t, err)
	return reclaimer
}

// seedDistributedChild 造一个真实的子任务：整行从父根复制（契约、修订、截止时间），
// 只把 task_id / parent / root / depth / 幂等键换成子任务自己的，再补一条 running attempt。
//
// 为什么复制而不是造最小行：`scanFlowTaskData` 会逐项核对契约哈希、输入哈希、
// `revision.task_id` 与 attempts 等字段；自造最小行只能靠关掉校验才能被读出来，
// 那会把"子任务与根同样是一等任务"这条前提从用例里抹掉。
// The child row is a faithful copy: the contract and revision checks stay active.
func seedDistributedChild(t *testing.T, db *postgres.DB, parent flow.Task,
	childID, attemptID, runID, sessionID string,
) (flow.Task, flow.NodeAttempt) {
	t.Helper()
	now := time.Now().UnixMilli()
	invocationID := childID + ":" + distributedNodeID
	input := []byte(`{"answer":"input"}`)
	_, err := db.Tx(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_tasks(task_id,root_task_id,parent_task_id,caller_id,authorizing_kind,
   authorizing_key_id,definition_uid,definition_version,contract_version,status,state_version,execution_epoch,
   current_revision,cancel_requested,close_intent,deadline,created_at,updated_at,next_eligible_at,
   idempotency_key,request_hash,reason,depth)
SELECT $1,t.root_task_id,t.task_id,t.caller_id,t.authorizing_kind,t.authorizing_key_id,
       t.definition_uid,t.definition_version,t.contract_version,'running',1,1,
       0,false,'none',t.deadline,t.created_at,t.updated_at,t.next_eligible_at,
       $2,$3,'',t.depth+1
  FROM flow_tasks t WHERE t.task_id=$4`, childID, "child-"+childID, distributedHashOf([]byte(childID)), parent.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_task_contracts(task_id,contract_version,definition_uid,
   definition_version,content,content_hash)
SELECT $1,c.contract_version,c.definition_uid,c.definition_version,c.content,c.content_hash
  FROM flow_task_contracts c WHERE c.task_id=$2`, childID, parent.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_task_revisions(task_id,revision,contract_version,checkpoint,created_at)
SELECT $1,r.revision,r.contract_version,r.checkpoint || jsonb_build_object('task_id',$1::text),r.created_at
  FROM flow_task_revisions r WHERE r.task_id=$2`, childID, parent.ID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO flow_node_attempts(task_id,node_attempt_id,node_id,node_invocation_id,
   attempt,task_revision,execution_epoch,run_id,session_id,state,input,input_hash,started_at)
VALUES($1,$2,$3,$4,1,0,1,$5,$6,'running',$7,$8,$9)`,
			childID, attemptID, distributedNodeID, invocationID, runID, sessionID, string(input), distributedHashOf(input), now)
		return err
	})
	require.NoError(t, err)
	child, err := postgres.NewFlowStore(db).ReadForWorker(t.Context(), childID)
	require.NoError(t, err)
	return child, flow.NodeAttempt{
		ID:               attemptID,
		TaskID:           childID,
		NodeID:           distributedNodeID,
		NodeInvocationID: invocationID,
		Attempt:          1,
		TaskRevision:     child.Version.Revision,
		ExecutionEpoch:   child.Version.ExecutionEpoch,
		RunID:            runID,
		SessionID:        sessionID,
		Input:            input,
		InputHash:        distributedHashOf(input),
	}
}

// flowTaskSnapshot 是 `flow_tasks` 行的只读投影。A3 的"一字未动"必须整行比较：
// 只看 status 会放过 execution_epoch / reason / updated_at 的改写。
type flowTaskSnapshot struct {
	Status         string
	StateVersion   int64
	ExecutionEpoch int64
	Revision       int64
	CloseIntent    string
	Cancel         bool
	Reason         string
	UpdatedAt      int64
	NextEligibleAt int64
}

func readFlowTaskSnapshot(t *testing.T, db *postgres.DB, taskID string) flowTaskSnapshot {
	t.Helper()
	var snapshot flowTaskSnapshot
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT status,state_version,execution_epoch,
   current_revision,close_intent,cancel_requested,reason,updated_at,next_eligible_at
  FROM flow_tasks WHERE task_id=$1`, taskID).
		Scan(&snapshot.Status, &snapshot.StateVersion, &snapshot.ExecutionEpoch, &snapshot.Revision,
			&snapshot.CloseIntent, &snapshot.Cancel, &snapshot.Reason, &snapshot.UpdatedAt, &snapshot.NextEligibleAt))
	return snapshot
}

// distributedTaskStatus 不使用 require：它只在 `require.Eventually` 的条件函数里跑。
// It avoids require because Eventually runs the condition in another goroutine.
func distributedTaskStatus(db *postgres.DB, taskID string) (flow.TaskStatus, string) {
	var status, intent string
	if err := db.QueryRowContext(context.Background(),
		`SELECT status,close_intent FROM flow_tasks WHERE task_id=$1`, taskID).Scan(&status, &intent); err != nil {
		return "", ""
	}
	return flow.TaskStatus(status), intent
}

// blockRootWithReason 把根推进到"上一轮 Recover 已停住"的现场：先 claim 出一条
// running attempt，再以给定 reason 调一次真实的 `Service.Recover`（joined=false →
// Blocked + Isolated 收敛 attempt）。reason 是 A3 唯一的分路依据，必须由服务写入。
func blockRootWithReason(t *testing.T, service *flow.Service, task flow.Task,
	running flow.Task, reason, operationID string,
) flow.Task {
	t.Helper()
	blocked, err := service.Recover(t.Context(), flow.ResumeRequest{
		Principal:   core.LocalAdminCLIPrincipal(),
		TaskID:      task.ID,
		Expected:    running.Version,
		OperationID: operationID,
		Reason:      reason,
	})
	require.NoError(t, err)
	require.Equal(t, flow.Blocked, blocked.Status)
	require.Equal(t, reason, blocked.Reason)
	return blocked
}

// TestFlow_AdminResumeBusyWhileWorkerHoldsRoot 对应模块卡 §4.4.7 的管理面并发用例：
// 执行者持有根租约时 resume **不得**降级成无租约写入（不重试、不写库），而要在
// 租约释放后让同一请求成功。
func TestFlow_AdminResumeBusyWhileWorkerHoldsRoot(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-admin-busy")
	task := startDistributedTask(t, service, definition, "admin-busy-root")
	attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-admin-busy", "run-admin-busy", "session-admin-busy", 1)
	blocked := blockRootWithReason(t, service, task, running, "worker_lease_expired", "recover-admin-busy")
	require.Equal(t, "interrupted", distributedAttemptStateOf(db, attempt.ID))
	before := readFlowTaskSnapshot(t, db, task.ID)

	held := acquireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID, liveRootHolderIdentity(), 5*time.Minute)
	busy, err := service.Recover(t.Context(), flow.ResumeRequest{
		Principal:   core.LocalAdminCLIPrincipal(),
		TaskID:      task.ID,
		Expected:    blocked.Version,
		OperationID: "admin-resume-busy",
		Reason:      "worker_lease_expired",
	})
	require.ErrorIs(t, err, flow.ErrFlowRootBusy, "执行者持租约时必须回应冲突，不得无租约写入")
	require.Equal(t, flow.Blocked, busy.Status)
	require.Equal(t, before, readFlowTaskSnapshot(t, db, task.ID), "领取根租约失败时 flow_tasks 一行未改")

	releaseFlowRootForTest(t, db, held)
	queued, err := service.Recover(t.Context(), flow.ResumeRequest{
		Principal:   core.LocalAdminCLIPrincipal(),
		TaskID:      task.ID,
		Expected:    blocked.Version,
		OperationID: "admin-resume-busy",
		Reason:      "worker_lease_expired",
	})
	require.NoError(t, err, "租约释放后同一请求必须成功")
	require.Equal(t, flow.Queued, queued.Status)
}

// TestReclaimer_FlowRootReentersBlockedRoot 对应模块卡 §4.4.7 的 A3 分路用例：
// 三条判据各自决定"动作 / 不动 / 不动作"，且三者都不推进、不派发新 Run。
func TestReclaimer_FlowRootReentersBlockedRoot(t *testing.T) {
	t.Run("worker-lease-expired-requeued", func(t *testing.T) {
		db := pgtest.NewTestDB(t)
		runner := newDistributedRunner(db)
		service := newDistributedService(db, runner)
		store := postgres.NewFlowStore(db)
		definition := publishDistributedDefinition(t, db, "probe-reclaim-requeue")
		task := startDistributedTask(t, service, definition, "reclaim-requeue-root")
		_, running := claimDistributedAttempt(t, db, store, task, "attempt-requeue", "run-requeue", "session-requeue", 1)
		blocked := blockRootWithReason(t, service, task, running, "worker_lease_expired", "recover-requeue")

		// 属主崩溃留下的根租约：领取谓词只认"已过期或已释放"，不按心跳推断死亡。
		acquireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID, crashedOwnerIdentity(), 5*time.Minute)
		expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)

		report, err := newFlowReclaimer(t, db, service, nil).ReclaimOnce(t.Context(), 32)
		require.NoError(t, err)
		require.Equal(t, 1, report.FlowRoots)
		require.Zero(t, report.Errors)

		requeued := readTaskForDistributedTest(t, store, task.ID)
		require.Equal(t, flow.Queued, requeued.Status, "worker_lease_expired 由 A3 自己重入 queued")
		require.Equal(t, "worker_lease_expired", requeued.Reason)
		require.Greater(t, requeued.Version.ExecutionEpoch, blocked.Version.ExecutionEpoch)
		require.True(t, readDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID).Released)
		require.Zero(t, countDistributedNodeRuns(t, db, task.RootTaskID), "A3 不推进、不派发新 Run")
		require.Zero(t, runner.calls.Load())
	})

	t.Run("bounded-rework-left-alone", func(t *testing.T) {
		db := pgtest.NewTestDB(t)
		runner := newDistributedRunner(db)
		service := newDistributedService(db, runner)
		store := postgres.NewFlowStore(db)
		definition := publishDistributedDefinition(t, db, "probe-reclaim-rework")
		task := startDistributedTask(t, service, definition, "reclaim-rework-root")
		_, running := claimDistributedAttempt(t, db, store, task, "attempt-rework", "run-rework", "session-rework", 1)
		blockRootWithReason(t, service, task, running, "bounded_rework_required", "recover-rework")

		acquireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID, crashedOwnerIdentity(), 5*time.Minute)
		expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)
		before := readFlowTaskSnapshot(t, db, task.ID)

		report, err := newFlowReclaimer(t, db, service, nil).ReclaimOnce(t.Context(), 32)
		require.NoError(t, err)
		require.Equal(t, 1, report.FlowRoots)
		require.Zero(t, report.Errors)

		require.Equal(t, before, readFlowTaskSnapshot(t, db, task.ID),
			"bounded_rework_required 归 Flow Worker 重新排队，A3 不得插手")
		require.True(t, readDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID).Released)
		require.Zero(t, countDistributedNodeRuns(t, db, task.RootTaskID))
		require.Zero(t, runner.calls.Load())
	})

	t.Run("live-child-run-lease-untouched", func(t *testing.T) {
		db := pgtest.NewTestDB(t)
		runner := newDistributedRunner(db)
		service := newDistributedService(db, runner)
		store := postgres.NewFlowStore(db)
		definition := publishDistributedDefinition(t, db, "probe-reclaim-live")
		task := startDistributedTask(t, service, definition, "reclaim-live-root")
		rootAttempt, running := claimDistributedAttempt(t, db, store, task, "attempt-live-root", "run-live-root", "session-live-root", 1)
		child, childAttempt := seedDistributedChild(t, db, running, "ft_reclaim_live_child", "attempt-live-child", "run-live-child", "session-live-child")
		seedDistributedRun(t, db, child, childAttempt, "running")
		// 子 Run 的租约**仍然有效**：TTL 到期不等于子 Run 已退出，A3 必须不动作。
		acquireDistributedLease(t, db, core.LeaseRun, childAttempt.RunID, liveRootHolderIdentity(), 5*time.Minute)

		blocked := blockRootWithReason(t, service, task, running, "worker_lease_expired", "recover-live")
		require.Equal(t, "running", distributedAttemptStateOf(db, rootAttempt.ID),
			"Isolated=false 时 Recover 不得收敛根 attempt")
		require.Equal(t, "running", distributedAttemptStateOf(db, childAttempt.ID))
		before := readFlowTaskSnapshot(t, db, task.ID)

		acquireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID, crashedOwnerIdentity(), 5*time.Minute)
		expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)

		report, err := newFlowReclaimer(t, db, service, nil).ReclaimOnce(t.Context(), 32)
		require.NoError(t, err)
		require.Equal(t, 1, report.FlowRoots)
		require.Zero(t, report.Errors)

		require.Equal(t, before, readFlowTaskSnapshot(t, db, task.ID), "子 Run 租约仍有效时根一字未动")
		require.Equal(t, flow.Blocked, blocked.Status)
		require.Equal(t, "running", distributedAttemptStateOf(db, rootAttempt.ID))
		require.Equal(t, "running", distributedAttemptStateOf(db, childAttempt.ID))
		require.True(t, readDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID).Released,
			"结论是「仍有在跑工作」：只交还租约，不改写根")
		require.Zero(t, runner.calls.Load())
	})
}

// TestReclaimer_UnknownActionReleaseKeepsGuard 对应模块卡 §4.4.7 的未决动作分路：
// A3 每轮都"领取 → 查证 → 交还"，下一轮仍能看见同一条未决动作；保护来自持久行，
// 不依赖租约状态。
func TestReclaimer_UnknownActionReleaseKeepsGuard(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-reclaim-unknown")
	task := startDistributedTask(t, service, definition, "reclaim-unknown-root")
	attempt, running := claimDistributedAttempt(t, db, store, task, "attempt-unknown", "run-unknown", "session-unknown", 1)
	// 旧执行已退出（attempt 收敛）但没有完成证据：Joined=true，可以进入 A3 的查证分支。
	setDistributedAttemptState(t, db, attempt.ID, "interrupted")
	blockRootWithReason(t, service, task, running, "worker_lease_expired", "recover-unknown")
	seedUnresolvedAction(t, db, running, attempt, "act-unknown", "dispatching")

	before := readFlowTaskSnapshot(t, db, task.ID)
	beforeRows := countDistributedRows(t, db, task.RootTaskID)
	repo := newDistributedRepository(store, task.RootTaskID, nil)
	reclaimer := newFlowReclaimer(t, db, service, repo)

	for round := 1; round <= 2; round++ {
		acquireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID, crashedOwnerIdentity(), 5*time.Minute)
		expireDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID)
		report, err := reclaimer.ReclaimOnce(t.Context(), 32)
		require.NoError(t, err)
		require.Equal(t, 1, report.FlowRoots, "round=%d", round)
		require.Zero(t, report.Errors, "round=%d", round)
		require.Equal(t, round, repo.unknownCalls(), "每轮都必须真正查证未决动作")
		require.Zero(t, repo.recoverCalls(), "查证为真时不得写 ReconcileTask")
		require.Empty(t, repo.events())
		require.Equal(t, before, readFlowTaskSnapshot(t, db, task.ID), "round=%d 根一字未动", round)
		require.Equal(t, "dispatching", distributedActionOutcome(db, "act-unknown"))
		require.Equal(t, beforeRows, countDistributedRows(t, db, task.RootTaskID), "round=%d 零新行", round)
		require.True(t, readDistributedLease(t, db, core.LeaseFlowRoot, task.RootTaskID).Released,
			"round=%d 结论确定后交还租约，下一轮仍会命中", round)
	}
	require.Zero(t, runner.calls.Load())
}

// TestFlow_CancelConvergesChildAttemptsUnderIsolation 对应模块卡 §4.4.7 的取消用例：
// Isolated=true 时关闭路径把**整棵子树**的 prepared / running attempt 收敛为
// interrupted；第二轮 Joined 才成立，根与子任务都落到 cancelled。
func TestFlow_CancelConvergesChildAttemptsUnderIsolation(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-cancel-isolated")
	task := startDistributedTask(t, service, definition, "cancel-isolated-root")
	rootAttempt, running := claimDistributedAttempt(t, db, store, task, "attempt-cancel-root", "run-cancel-root", "session-cancel-root", 1)
	child, childAttempt := seedDistributedChild(t, db, running, "ft_cancel_child", "attempt-cancel-child", "run-cancel-child", "session-cancel-child")

	cancelling, err := service.Cancel(t.Context(), flow.CancelRequest{
		Principal:   core.LocalAdminCLIPrincipal(),
		TaskID:      task.ID,
		Expected:    running.Version,
		OperationID: "cancel-child-root-1",
		Reason:      "user cancelled",
	})
	require.NoError(t, err)
	require.Equal(t, flow.Cancelling, cancelling.Status)
	require.True(t, cancelling.CancelRequested)
	require.Equal(t, flow.CloseCancel, cancelling.CloseIntent)
	childCancelling := readTaskForDistributedTest(t, store, child.ID)
	require.Equal(t, flow.Cancelling, childCancelling.Status, "关闭意图必须传播到整棵子树")
	require.True(t, childCancelling.CancelRequested)
	require.Equal(t, "interrupted", distributedAttemptStateOf(db, rootAttempt.ID))
	require.Equal(t, "interrupted", distributedAttemptStateOf(db, childAttempt.ID),
		"Isolated=true 时子任务的 attempt 同样必须收敛")

	cancelled, err := service.Cancel(t.Context(), flow.CancelRequest{
		Principal:   core.LocalAdminCLIPrincipal(),
		TaskID:      task.ID,
		Expected:    cancelling.Version,
		OperationID: "cancel-child-root-2",
		Reason:      "user cancelled again",
	})
	require.NoError(t, err)
	require.Equal(t, flow.Cancelled, cancelled.Status)
	childCancelled := readTaskForDistributedTest(t, store, child.ID)
	require.Equal(t, flow.Cancelled, childCancelled.Status)
	require.Equal(t, flow.CloseCancel, childCancelled.CloseIntent)
}

// TestFlow_DeadlineCloseConvergesChildAttemptsUnderIsolation 对应模块卡 §4.4.7 的
// deadline 用例：第一轮 Isolated 收敛整棵子树但仍是 blocked，第二轮 Joined 才写终态；
// 终态是 **stopped**，不是 cancelled。
func TestFlow_DeadlineCloseConvergesChildAttemptsUnderIsolation(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := newDistributedRunner(db)
	service := newDistributedService(db, runner)
	store := postgres.NewFlowStore(db)
	definition := publishDistributedDefinition(t, db, "probe-deadline-isolated")
	task := startDistributedTask(t, service, definition, "deadline-isolated-root")
	rootAttempt, running := claimDistributedAttempt(t, db, store, task, "attempt-deadline-root", "run-deadline-root", "session-deadline-root", 1)
	child, childAttempt := seedDistributedChild(t, db, running, "ft_deadline_child", "attempt-deadline-child", "run-deadline-child", "session-deadline-child")
	// 把整棵子树的墙钟截止时间挪到过去：外层 CHECK 要求 deadline >= created_at，
	// 因此取 created_at 本身——`now >= deadline` 对任何后续读数都成立。
	_, err := db.ExecContext(t.Context(), `UPDATE flow_tasks SET deadline=created_at WHERE root_task_id=$1`, task.RootTaskID)
	require.NoError(t, err)

	repo := newDistributedRepository(store, task.RootTaskID, nil)
	stop := startDistributedWorker(t, service, repo, db, flowLeaseFixtureIdentity(), distributedWorkerTTL, distributedRenewInterval)
	t.Cleanup(stop)

	// `sweepClosures` 不导出，只有真实 `flow.Worker` 循环会驱动它（worker.go 的每轮首步）。
	require.Eventually(t, func() bool {
		status, intent := distributedTaskStatus(db, task.ID)
		return status == flow.Stopped && intent == "deadline"
	}, 20*time.Second, 50*time.Millisecond, "deadline 关闭必须由 Worker 循环推进到 stopped")

	final := readTaskForDistributedTest(t, store, task.ID)
	require.Equal(t, flow.Stopped, final.Status)
	require.NotEqual(t, flow.Cancelled, final.Status, "deadline 终态是 stopped，不是 cancelled")
	require.Equal(t, flow.CloseDeadline, final.CloseIntent)
	require.False(t, final.CancelRequested)
	childFinal := readTaskForDistributedTest(t, store, child.ID)
	require.Equal(t, flow.Stopped, childFinal.Status)
	require.Equal(t, flow.CloseDeadline, childFinal.CloseIntent)
	require.Equal(t, "interrupted", distributedAttemptStateOf(db, rootAttempt.ID))
	require.Equal(t, "interrupted", distributedAttemptStateOf(db, childAttempt.ID))
	require.Zero(t, runner.calls.Load(), "关闭路径不执行任何节点")
}

//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// 本文件是 US-24 T022 的存储层验收：Flow 根推进的执行权（flow_root 租约）、
// 接管后的代次组合，以及 Isolated / Joined / 未决动作三个**互相独立**的判据。
// 全部使用真实 PostgreSQL 与真实 LeaseStore，不用替身模拟租约。

// flowGateRunner 把两个跨 Worker 判据接到真实子树上（与 app 层 flowNodeRunner
// 的同构最小实现）：Joined = 无 prepared/running attempt，Isolated = 无有效 run 租约。
// 用它是为了让 Service.Recover 在存储层测试里也能拿到真实判据而不是"端口缺失→false"。
// flowGateRunner wires the two subtree criteria to the real gate.
type flowGateRunner struct {
	gate   core.SubtreeGate
	joined bool
	// isolate / isolateSet 用来把 Isolated 固定成指定读数（隔离收敛与"活工作不动"
	// 两个方向都要能构造）；isolateSet=false 时才查真实 gate。
	isolate    bool
	isolateSet bool
	run        func(context.Context, flow.NodeRunRequest) (flow.NodeRunResult, error)
}

func (r *flowGateRunner) Run(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	if r.run == nil {
		return flow.NodeRunResult{Run: core.RunResult{Status: core.StatusFailed}}, errors.New("flowGateRunner: 未装配 Run")
	}
	return r.run(ctx, req)
}

func (r *flowGateRunner) Joined(ctx context.Context, task flow.Task) (bool, error) {
	if r.joined {
		return true, nil
	}
	inflight, err := r.gate.HasInFlightNodeAttempt(ctx, task.RootTaskID)
	if err != nil {
		return false, err
	}
	return !inflight, nil
}

func (r *flowGateRunner) Isolated(ctx context.Context, task flow.Task) (bool, error) {
	if r.isolateSet {
		return r.isolate, nil
	}
	live, err := r.gate.HasLiveRunLease(ctx, task.RootTaskID)
	if err != nil {
		return false, err
	}
	return !live, nil
}

// flowLeaseProbeRunner 在**派发节点的这一刻**读一次 flow_root 租约：
// "推进前必须先持有根租约"（FR-24-18）只有在这里观测才是真判据——事后再查
// 只能证明"有过租约"，证明不了"推进时已持有"。
// The probe reads the root lease at dispatch time, which is the only moment that
// proves the claim happened before the advance.
type flowLeaseProbeRunner struct {
	inner *flowAdvanceRunner
	db    *DB
	mu    sync.Mutex
	owner string
	epoch int64
	live  bool
	err   error
	seen  bool
}

func (r *flowLeaseProbeRunner) Run(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	var owner string
	var epoch int64
	var released sql.NullInt64
	var expires int64
	err := r.db.QueryRowContext(ctx, `SELECT owner_worker_id, ownership_epoch, released_at, expires_at
  FROM execution_leases WHERE kind='flow_root' AND resource_id=$1`, req.Task.RootTaskID).Scan(&owner, &epoch, &released, &expires)
	r.mu.Lock()
	r.seen = true
	r.owner, r.epoch, r.err = owner, epoch, err
	r.live = err == nil && !released.Valid
	r.mu.Unlock()
	return r.inner.Run(ctx, req)
}

func (r *flowLeaseProbeRunner) Joined(context.Context, flow.Task) (bool, error) { return true, nil }

// readFlowRootLeaseRow 读回一条 flow_root 租约行（复用 lease_store.go 的行快照类型）。
func readFlowRootLeaseRow(t *testing.T, db *DB, resource string) leaseRow {
	t.Helper()
	var row leaseRow
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT owner_worker_id, ownership_epoch, expires_at, released_at
  FROM execution_leases WHERE kind='flow_root' AND resource_id=$1`, resource).Scan(&row.Owner, &row.Epoch, &row.Expires, &row.Released))
	return row
}

func TestFlow_WorkerClaimsRootLeaseBeforeAdvance(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	probe := &flowLeaseProbeRunner{inner: &flowAdvanceRunner{store: s, output: json.RawMessage(`{"answer":"lease ok"}`)}, db: s.db}
	service.Runner = probe
	// Worker.Run 的取值闸门是 100..5000，低于 100 会直接判配置非法退出，
	// 用例要的是"尽快轮询"而不是绕过闸门。
	service.Config.PollIntervalMS = 100
	task, err := service.Start(t.Context(), startFlowRequest(def, "worker-claims-root"))
	require.NoError(t, err)
	worker := flowWorkerForStore(s, service, func(context.Context) error { return nil })
	worker.Identity = core.WorkerIdentity{InstanceID: "flow-worker-fixture", Name: "worker"}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.After(10 * time.Second)
	for {
		current, err := s.ReadForWorker(t.Context(), task.ID)
		require.NoError(t, err)
		if current.Status == flow.Succeeded {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("worker exited early: %v", err)
		case <-deadline:
			t.Fatal("worker did not advance the queued root")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	require.NoError(t, <-done)

	probe.mu.Lock()
	seen, owner, epoch, live, probeErr := probe.seen, probe.owner, probe.epoch, probe.live, probe.err
	probe.mu.Unlock()
	require.True(t, seen, "节点必须被派发过")
	require.NoError(t, probeErr)
	require.Equal(t, "flow-worker-fixture", owner, "派发时根租约必须已由本 Worker 领取")
	require.EqualValues(t, 1, epoch)
	require.True(t, live, "派发时租约未释放也未过期")

	// 正常交棒：推进完成且无未决动作时 `stop(ReleaseCtx())` 主动释放，下一轮别的
	// Worker 拿到的是 TookOver=false 的正常交棒（FR-24-18）。
	released := readFlowRootLeaseRow(t, s.db, task.ID)
	require.True(t, released.Released.Valid, "正常推进结束后必须交还根租约")
	require.EqualValues(t, 1, released.Epoch)

	var claimed, releasedEvents int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM lease_events WHERE kind='flow_root' AND resource_id=$1 AND event='claimed'`, task.ID).Scan(&claimed))
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM lease_events WHERE kind='flow_root' AND resource_id=$1 AND event='released'`, task.ID).Scan(&releasedEvents))
	require.Equal(t, 1, claimed)
	require.Equal(t, 1, releasedEvents)
}

func TestFlow_TakeoverBumpsExecutionEpochAndRejectsStaleCommit(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	runner := &flowAdvanceRunner{store: s, output: json.RawMessage(`{"answer":"takeover"}`)}
	service.Runner = &flowGateRunner{gate: NewSubtreeGate(s.db), run: runner.Run}
	task, err := service.Start(t.Context(), startFlowRequest(def, "takeover-epoch"))
	require.NoError(t, err)

	// A 领取根租约并推进到 running（attempt execution_epoch = 1）。
	leases := NewLeaseStore(s.db)
	workerA := core.WorkerIdentity{InstanceID: "worker-a", Name: "a"}
	workerB := core.WorkerIdentity{InstanceID: "worker-b", Name: "b"}
	heldA, err := leases.Acquire(t.Context(), core.LeaseFlowRoot, task.ID, workerA, time.Minute)
	require.NoError(t, err)
	require.False(t, heldA.TookOver)
	current, err := s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	claimed, err := s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: current.Version, Lease: heldA.LeaseToken, Now: time.Now().UnixMilli(), Attempt: flow.NodeAttempt{ID: "attempt-a", TaskID: task.ID, NodeID: "draft", NodeInvocationID: task.ID + ":draft", Attempt: 1, TaskRevision: current.Version.Revision, ExecutionEpoch: current.Version.ExecutionEpoch, Input: current.Revision.Input, InputHash: current.Revision.InputHash, RunID: "run-a"}})
	require.NoError(t, err)
	running, err := s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	staleVersion := running.Version

	// A 的租约过期但未释放 → B 合法接管，代次 +1，TookOver=true。
	expireLease(t, leases, core.LeaseFlowRoot, task.ID)
	heldB, err := leases.Acquire(t.Context(), core.LeaseFlowRoot, task.ID, workerB, time.Minute)
	require.NoError(t, err)
	require.True(t, heldB.TookOver, "过期未释放必须是接管而不是正常交棒")
	require.EqualValues(t, 2, heldB.OwnershipEpoch)

	// 接管方先 Recover：子树已隔离（没有活 run 租约）→ 收敛 attempt 并递增任务代次。
	recovered, err := service.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: running.Version, OperationID: "takeover-recover", Reason: "worker_lease_expired", Lease: heldB.LeaseToken})
	require.NoError(t, err)
	require.Equal(t, flow.Blocked, recovered.Status)
	require.EqualValues(t, 2, recovered.Version.ExecutionEpoch, "接管递增 Flow 执行代次")
	var state string
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT state FROM flow_node_attempts WHERE node_attempt_id='attempt-a'`).Scan(&state))
	require.Equal(t, "interrupted", state)

	// 旧属主 A 的迟到提交必须被拒：execution_epoch / state_version 条件提交保留。
	_, err = s.Commit(t.Context(), flow.CommitNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, AttemptID: claimed.ID, OperationID: "stale-commit", Expected: staleVersion, Revision: running.Revision, Outcome: flow.Succeeded, AttemptState: "succeeded", Output: json.RawMessage(`{"answer":"overwrite"}`), OutputHash: flowDataHash(json.RawMessage(`{"answer":"overwrite"}`)), Lease: heldA.LeaseToken, Now: time.Now().UnixMilli()})
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrPreconditionFailed, "旧代次提交必须被条件提交拒绝")

	after, err := s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, recovered.Version, after.Version, "B 的结果不得被 A 覆盖")
	require.EqualValues(t, 2, after.Version.ExecutionEpoch)
	var output sql.NullString
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT output FROM flow_node_attempts WHERE node_attempt_id='attempt-a'`).Scan(&output))
	require.False(t, output.Valid, "被拒的提交不得写入任何结果列")
}

func TestFlow_JoinedWaitsForInFlightAttempts(t *testing.T) {
	s, def := newFlowStoreTest(t)
	gate := NewSubtreeGate(s.db)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "joined-waits", "joined-waits"))
	require.NoError(t, err)
	acquireFlowRootLease(t, s.db, task.ID)

	inflight, err := gate.HasInFlightNodeAttempt(t.Context(), task.ID)
	require.NoError(t, err)
	require.False(t, inflight, "尚无 attempt 时 Joined 成立")

	claimed, err := s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Now: time.Now().UnixMilli(), Attempt: flow.NodeAttempt{ID: "attempt-inflight", TaskID: task.ID, NodeID: "draft", NodeInvocationID: task.ID + ":draft", Attempt: 1, TaskRevision: task.Version.Revision, ExecutionEpoch: task.Version.ExecutionEpoch, Input: task.Revision.Input, InputHash: task.Revision.InputHash, RunID: "run-inflight"}})
	require.NoError(t, err)
	inflight, err = gate.HasInFlightNodeAttempt(t.Context(), task.ID)
	require.NoError(t, err)
	require.True(t, inflight, "running attempt 必须让 Joined 等待")

	// interrupted 不计入：它是 Recover 的产物，计入会形成"等自己收敛"的死锁。
	_, err = s.db.ExecContext(t.Context(), `UPDATE flow_node_attempts SET state='interrupted' WHERE node_attempt_id=$1`, claimed.ID)
	require.NoError(t, err)
	inflight, err = gate.HasInFlightNodeAttempt(t.Context(), task.ID)
	require.NoError(t, err)
	require.False(t, inflight, "interrupted 不得计入 Joined")

	// prepared 同样计入：它是"已领取但尚未开跑"的在途态。
	_, err = s.db.ExecContext(t.Context(), `UPDATE flow_node_attempts SET state='prepared' WHERE node_attempt_id=$1`, claimed.ID)
	require.NoError(t, err)
	inflight, err = gate.HasInFlightNodeAttempt(t.Context(), task.ID)
	require.NoError(t, err)
	require.True(t, inflight, "prepared 必须计入 Joined")
}

func TestFlow_LiveLeaseBlocksVerificationOnly(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	gate := NewSubtreeGate(s.db)
	// 夹具按真实 Worker 身份建 Run：本用例断言"Run 在跑 → Isolated=false"，
	// 而这条判据读的是 run 租约，无租约的 Run 会让断言假绿。
	runner := &flowAdvanceRunner{store: s, output: json.RawMessage(`{"answer":"blocked"}`), block: make(chan struct{}), release: make(chan struct{}), owner: core.WorkerIdentity{InstanceID: "flow-live-lease", Name: "live"}}
	service.Runner = runner
	task, err := service.Start(t.Context(), startFlowRequest(def, "live-lease"))
	require.NoError(t, err)
	// 凭证必须原样下传：叶子 Run 创建（createRunForNode）以它证明父根归属。
	root := flowRootLeaseFor(t, s.db, task.ID)

	runCtx, stopRun := context.WithCancel(t.Context())
	advance := make(chan error, 1)
	go func() {
		_, err := service.Advance(runCtx, flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Lease: root})
		advance <- err
	}()
	select {
	case <-runner.block:
	case <-time.After(10 * time.Second):
		t.Fatal("节点未派发")
	}
	// 这一刻：Run 仍在跑（活 run 租约），attempt 仍是 running。
	live, err := gate.HasLiveRunLease(t.Context(), task.ID)
	require.NoError(t, err)
	require.True(t, live, "在跑的 Run 必须让 Isolated=false")
	inflight, err := gate.HasInFlightNodeAttempt(t.Context(), task.ID)
	require.NoError(t, err)
	require.True(t, inflight)

	// 把 attempt 收敛掉（模拟上一轮 Recover 的产物）后两个判据**各自独立取值**：
	// 租约仍活 → 不得推进；但 attempt 已不在途 → 不再等它。用租约顶替 Joined 会
	// 把在跑的工作当成可推进，用 attempt 顶替 Isolated 会覆盖别人正在写的结果。
	_, err = s.db.ExecContext(t.Context(), `UPDATE flow_node_attempts SET state='interrupted' WHERE task_id=$1 AND state='running'`, task.ID)
	require.NoError(t, err)
	live, err = gate.HasLiveRunLease(t.Context(), task.ID)
	require.NoError(t, err)
	require.True(t, live, "Isolated 仍是 false")
	inflight, err = gate.HasInFlightNodeAttempt(t.Context(), task.ID)
	require.NoError(t, err)
	require.False(t, inflight, "Joined 已是 true——两个判据必须独立取值")

	// 结束在跑的叶子：夹具在 ctx 取消后写终态并返回。这里不判 Advance 的返回值——
	// 本用例的判据已在上面两条独立读数上完成，后面这条路径的终态由别的用例覆盖。
	stopRun()
	close(runner.release)
	select {
	case <-advance:
	case <-time.After(15 * time.Second):
		t.Fatal("advance did not return")
	}
}

func TestFlow_RecoverConvergesAttemptsUnderIsolation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		isolated bool
		expected string
	}{
		{"isolated-converges", true, "interrupted"},
		{"live-work-not-touched", false, "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, def := newFlowStoreTest(t)
			service := flowServiceForStore(s)
			gate := NewSubtreeGate(s.db)
			service.Runner = &flowGateRunner{gate: gate, isolate: tc.isolated, isolateSet: true}
			task, err := service.Start(t.Context(), startFlowRequest(def, "recover-"+tc.name))
			require.NoError(t, err)
			root := flowRootLeaseFor(t, s.db, task.ID)
			current, err := s.ReadForWorker(t.Context(), task.ID)
			require.NoError(t, err)
			claimed, err := s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: current.Version, Lease: root, Now: time.Now().UnixMilli(), Attempt: flow.NodeAttempt{ID: "attempt-" + tc.name, TaskID: task.ID, NodeID: "draft", NodeInvocationID: task.ID + ":draft", Attempt: 1, TaskRevision: current.Version.Revision, ExecutionEpoch: current.Version.ExecutionEpoch, Input: current.Revision.Input, InputHash: current.Revision.InputHash, RunID: "run-" + tc.name}})
			require.NoError(t, err)
			running, err := s.ReadForWorker(t.Context(), task.ID)
			require.NoError(t, err)

			recovered, err := service.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: running.Version, OperationID: "recover-" + tc.name, Reason: "worker_lease_expired", Lease: root})
			require.NoError(t, err)
			require.Equal(t, flow.Blocked, recovered.Status)
			var state string
			require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT state FROM flow_node_attempts WHERE node_attempt_id=$1`, claimed.ID).Scan(&state))
			require.Equal(t, tc.expected, state)
			require.NoError(t, NewLeaseStore(s.db).Release(t.Context(), root))
		})
	}
}

func TestFlow_RecoverConvergesWholeSubtree(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	gate := NewSubtreeGate(s.db)
	service.Runner = &flowGateRunner{gate: gate, isolate: true, isolateSet: true}
	parent, err := service.Start(t.Context(), startFlowRequest(def, "whole-subtree"))
	require.NoError(t, err)
	childRequest := flowCreateRequest(def, "whole-subtree-child", "whole-subtree-child")
	childRequest.RootTaskID = parent.ID
	childRequest.ParentTaskID = parent.ID
	childRequest.Depth = 1
	// 子任务的 deadline 由父任务给出（`Create` 拒绝 `req.Deadline > parent.Deadline`）；
	// 辅助函数按"此刻 + 预算"新算，晚于父任务几毫秒就会被拒成 state conflict。
	// A child inherits the parent's deadline; recomputing it would exceed the parent's.
	childRequest.Deadline = parent.Deadline
	child, err := s.Create(t.Context(), childRequest)
	require.NoError(t, err)
	require.Equal(t, parent.ID, child.RootTaskID)

	root := flowRootLeaseFor(t, s.db, parent.ID)
	// 根与子各留一条 running attempt：只收敛根（R4 的缺陷）会让 Joined 永远为 false。
	for _, item := range []flow.Task{parent, child} {
		current, err := s.ReadForWorker(t.Context(), item.ID)
		require.NoError(t, err)
		_, err = s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: item.ID, Expected: current.Version, Lease: root, Now: time.Now().UnixMilli(), Attempt: flow.NodeAttempt{ID: "attempt-" + item.ID, TaskID: item.ID, NodeID: "draft", NodeInvocationID: item.ID + ":draft", Attempt: 1, TaskRevision: current.Version.Revision, ExecutionEpoch: current.Version.ExecutionEpoch, Input: current.Revision.Input, InputHash: current.Revision.InputHash, RunID: "run-" + item.ID}})
		require.NoError(t, err)
	}
	running, err := s.ReadForWorker(t.Context(), parent.ID)
	require.NoError(t, err)
	_, err = service.Recover(t.Context(), flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: parent.ID, Expected: running.Version, OperationID: "recover-whole-subtree", Reason: "worker_lease_expired", Lease: root})
	require.NoError(t, err)
	var remaining int
	require.NoError(t, s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_node_attempts a JOIN flow_tasks t ON t.task_id=a.task_id WHERE t.root_task_id=$1 AND a.state IN ('prepared','running')`, parent.ID).Scan(&remaining))
	require.Zero(t, remaining, "收敛范围必须是整棵子树，不是根自己")
	require.NoError(t, NewLeaseStore(s.db).Release(t.Context(), root))
}

func TestFlow_UnresolvedActionGate(t *testing.T) {
	s, def := newFlowStoreTest(t)
	gate := NewSubtreeGate(s.db)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "unresolved-gate", "unresolved-gate"))
	require.NoError(t, err)
	acquireFlowRootLease(t, s.db, task.ID)

	unresolved, err := gate.HasUnresolvedAction(t.Context(), task.ID)
	require.NoError(t, err)
	require.False(t, unresolved, "无动作时门 ③ 不命中")

	insert := func(actionID, outcome string) {
		_, err := s.db.ExecContext(t.Context(), `INSERT INTO flow_actions(task_id,action_id,node_invocation_id,logical_slot,tool_name,adapter_version,schema_hash,input,input_hash,recovery_class,outcome,created_at)
VALUES($1,$2,$3,$4,'read_file','v1','schema','{}','hash','readonly',$5,$6)`, task.ID, actionID, task.ID+":"+actionID, actionID, outcome, time.Now().UnixMilli())
		require.NoError(t, err)
	}
	// succeeded / not_issued / prepared 都不是未决：只有"可能已经发出去了"才算。
	for _, outcome := range []string{"succeeded", "not_issued", "prepared"} {
		insert("act-"+outcome, outcome)
		unresolved, err := gate.HasUnresolvedAction(t.Context(), task.ID)
		require.NoError(t, err)
		require.False(t, unresolved, "outcome=%s 不得算未决动作", outcome)
	}
	for _, outcome := range []string{"dispatching", "unknown"} {
		insert("act-"+outcome, outcome)
		unresolved, err := gate.HasUnresolvedAction(t.Context(), task.ID)
		require.NoError(t, err)
		require.True(t, unresolved, "outcome=%s 必须命中门 ③", outcome)
	}
}

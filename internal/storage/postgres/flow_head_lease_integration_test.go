//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// TestFlow_AdvanceClassHeadWriteRequiresLiveRootLease 验证 US-24 §3.1.6 / FR-24-18
// 的写入档分档：写 running 的推进类写入必须有活跃 flow_root 租约，租约释放后同一
// 写入被拒；重新领取后恢复。子任务行没有自己的 flow_root 键，按行内 root_task_id
// 查父根，这里用子任务证明该解析。
// Advance-class head writes (running) require a live flow_root lease resolved from
// the row's own root_task_id; close-class writes do not.
func TestFlow_AdvanceClassHeadWriteRequiresLiveRootLease(t *testing.T) {
	s, def := newFlowStoreTest(t)
	root, err := s.Create(t.Context(), flowCreateRequest(def, "task-lease-root", "key-lease-root"))
	require.NoError(t, err)
	childReq := flowCreateRequest(def, "task-lease-child", "key-lease-child")
	childReq.RootTaskID, childReq.ParentTaskID, childReq.Depth, childReq.Deadline = root.ID, root.ID, 1, root.Deadline
	child, err := s.Create(t.Context(), childReq)
	require.NoError(t, err)
	claim := flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: child.ID, Expected: child.Version, Now: child.CreatedAt + 1, Attempt: flow.NodeAttempt{ID: "lease-attempt", TaskID: child.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: child.Version.ExecutionEpoch, Input: child.Revision.Input, InputHash: child.Revision.InputHash}}
	_, err = s.Claim(t.Context(), claim)
	require.ErrorIs(t, err, core.ErrPreconditionFailed, "无根租约的推进类写入必须被拒")
	var attempts int
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_node_attempts WHERE task_id=$1`, child.ID).Scan(&attempts))
	require.Zero(t, attempts, "被拒的领取不得留下 attempt 行")

	lease := flowRootLeaseFor(t, s.db, child.RootTaskID)
	_, err = s.Claim(t.Context(), claim)
	require.NoError(t, err, "持有根租约时子任务可按父根解析领取")
	require.NoError(t, NewLeaseStore(s.db).Release(t.Context(), lease))

	current, err := s.ReadForWorker(t.Context(), child.ID)
	require.NoError(t, err)
	require.Equal(t, flow.Running, current.Status)
	// ParkTask 在 queued 行上先写 running（推进类）：租约已释放时必须失权；
	// 重新领取后同一停住请求成功（第二次写 blocked 属关闭类，本就不要求租约）。
	queued, err := s.Create(t.Context(), flowCreateRequest(def, "task-lease-park", "key-lease-park"))
	require.NoError(t, err)
	require.ErrorIs(t, s.ParkTask(t.Context(), core.LocalAdminCLIPrincipal(), queued.ID, queued.Version, "verification_missing"), core.ErrPreconditionFailed, "租约释放后推进类写入必须失权")
	lease = flowRootLeaseFor(t, s.db, queued.RootTaskID)
	require.NoError(t, s.ParkTask(t.Context(), core.LocalAdminCLIPrincipal(), queued.ID, queued.Version, "verification_missing"))
	require.NoError(t, NewLeaseStore(s.db).Release(t.Context(), lease))
}

// TestFlow_CloseClassHeadWriteNeedsNoRootLease 关闭类写入（cancelling / cancelled）
// 不要求根租约：关闭正是失权执行者也要能完成的收尾（US-24 §2.3.6 / P-04）。
// Close-class writes must stay possible after the executor lost the lease.
func TestFlow_CloseClassHeadWriteNeedsNoRootLease(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-close-lease", "key-close-lease"))
	require.NoError(t, err)
	lease := flowRootLeaseFor(t, s.db, task.RootTaskID)
	_, err = s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Now: task.CreatedAt + 1, Attempt: flow.NodeAttempt{ID: "close-attempt", TaskID: task.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: task.Version.ExecutionEpoch, Input: task.Revision.Input, InputHash: task.Revision.InputHash}})
	require.NoError(t, err)
	require.NoError(t, NewLeaseStore(s.db).Release(t.Context(), lease))
	running, err := s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	closed, err := s.Cancel(t.Context(), flow.CancelTask{Request: flow.CancelRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: running.Version, OperationID: "close-without-lease"}, Joined: true, Now: task.CreatedAt + 2})
	require.NoError(t, err, "无根租约时关闭类写入必须获准")
	require.Equal(t, flow.Cancelled, closed.Status)
}

// TestFlow_RecoverConvergesAttemptsOnlyUnderIsolation 验证 US-24 §3.1.7 / FR-24-38：
// Recover 只在 Isolated=true 时把整棵子树（按 root_task_id）的 prepared / running
// attempt 收敛为 interrupted；Isolated=false 时 attempt 一字不动。
// Recover converges subtree attempts only when the old execution is isolated.
func TestFlow_RecoverConvergesAttemptsOnlyUnderIsolation(t *testing.T) {
	s, def := newFlowStoreTest(t)
	root, err := s.Create(t.Context(), flowCreateRequest(def, "task-recover-root", "key-recover-root"))
	require.NoError(t, err)
	childReq := flowCreateRequest(def, "task-recover-child", "key-recover-child")
	childReq.RootTaskID, childReq.ParentTaskID, childReq.Depth, childReq.Deadline = root.ID, root.ID, 1, root.Deadline
	child, err := s.Create(t.Context(), childReq)
	require.NoError(t, err)
	lease := flowRootLeaseFor(t, s.db, root.ID)
	// 建子任务会推进根行的 children_created，Create 返回的版本已过期，先重读。
	root, err = s.ReadForWorker(t.Context(), root.ID)
	require.NoError(t, err)
	// 先把根停到 blocked：Recover 写 Blocked 是"停住"，看的是 Running→Blocked 路径；
	// 根仍为 queued 时 Queued→Blocked 不是合法转换。
	require.NoError(t, s.ParkTask(t.Context(), core.LocalAdminCLIPrincipal(), root.ID, root.Version, "verification_missing"))
	_, err = s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: child.ID, Expected: child.Version, Now: child.CreatedAt + 1, Attempt: flow.NodeAttempt{ID: "recover-attempt", TaskID: child.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: child.Version.ExecutionEpoch, Input: child.Revision.Input, InputHash: child.Revision.InputHash}})
	require.NoError(t, err)
	require.NoError(t, NewLeaseStore(s.db).Release(t.Context(), lease))
	root, err = s.ReadForWorker(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, flow.Blocked, root.Status)

	state := func() string {
		t.Helper()
		var value string
		require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT state FROM flow_node_attempts WHERE task_id=$1`, child.ID).Scan(&value))
		return value
	}
	reconcile := func(expected flow.TaskVersion, isolated bool, op string, now int64) flow.Task {
		t.Helper()
		recovered, err := s.Recover(t.Context(), flow.ReconcileTask{Request: flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: root.ID, OperationID: op, Reason: "lease reclaimed", Expected: expected}, Status: flow.Blocked, Joined: true, Isolated: isolated, Now: now})
		require.NoError(t, err)
		return recovered
	}

	first := reconcile(root.Version, false, "recover-not-isolated", root.CreatedAt+2)
	require.Equal(t, "running", state(), "Isolated=false 时 attempt 一字不动")
	current := reconcileMustRead(t, s, root.ID)
	require.Equal(t, first.Version, current.Version)
	reconcile(current.Version, true, "recover-isolated", root.CreatedAt+3)
	require.Equal(t, "interrupted", state(), "Isolated=true 时整棵子树的 attempt 收敛为 interrupted")
}

// TestFlow_RecoverRechecksIsolationUnderRunLeaseLock 验证 US-24 §2.3.6 注记 / 评审 IDX-13：
// Recover 不得直接采信事务外算出的 Isolated。对账事务先锁子树现存 run 租约行，与
// "过期前已开始、正在提交"的那一笔串行化，锁后按数据库时钟重读判据；并发那一笔提交后
// 仍能看到有效租约时，根写 Blocked、attempt 一字不动——即使调用方传来
// Status=Queued、Isolated=true。
// A pre-transaction Isolated snapshot that is contradicted after the row locks must fall
// back to Blocked without converging attempts, never Queue.
func TestFlow_RecoverRechecksIsolationUnderRunLeaseLock(t *testing.T) {
	s, def := newFlowStoreTest(t)
	root, err := s.Create(t.Context(), flowCreateRequest(def, "task-recheck-root", "key-recheck-root"))
	require.NoError(t, err)
	childReq := flowCreateRequest(def, "task-recheck-child", "key-recheck-child")
	childReq.RootTaskID, childReq.ParentTaskID, childReq.Depth, childReq.Deadline = root.ID, root.ID, 1, root.Deadline
	child, err := s.Create(t.Context(), childReq)
	require.NoError(t, err)
	lease := flowRootLeaseFor(t, s.db, root.ID)
	// 建子任务推进了根行的 children_created，Create 返回的版本已过期，先重读。
	root, err = s.ReadForWorker(t.Context(), root.ID)
	require.NoError(t, err)
	// 根先停到 blocked：Recover 的 Queued 分支只接受 blocked 根（FR-24-37）。
	require.NoError(t, s.ParkTask(t.Context(), core.LocalAdminCLIPrincipal(), root.ID, root.Version, "verification_missing"))
	claimed, err := s.Claim(t.Context(), flow.ClaimNode{
		Principal: core.LocalAdminCLIPrincipal(), TaskID: child.ID, Expected: child.Version, Now: child.CreatedAt + 1,
		Attempt: flow.NodeAttempt{ID: "recheck-attempt", TaskID: child.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, TaskRevision: child.Version.Revision, ExecutionEpoch: child.Version.ExecutionEpoch, RunID: "recheck-run", SessionID: "recheck-session", Input: child.Revision.Input, InputHash: child.Revision.InputHash},
	})
	require.NoError(t, err)
	// 真实 StartRun：Run 行与 run 租约同一事务建成（FR-24-03）。
	require.NoError(t, startRunOwnedTest(NewRunStore(s.db), t.Context(), core.RunRecord{
		RunID: "recheck-run", SessionID: "recheck-session", Agent: "writer", Channel: "cli",
		UserID: "flow-probe", Principal: core.LocalAdminCLIPrincipal(), StartedAt: time.Now(),
		Flow: &core.FlowRunContext{TaskID: child.ID, RootTaskID: root.ID, NodeAttemptID: claimed.ID, TaskRevision: claimed.TaskRevision, Lease: lease},
	}, core.RunResourceManifest{}, core.WorkerIdentity{InstanceID: "recheck-owner", Name: "recheck-owner"}))
	// 调用方读到 Isolated=true 的那一刻：run 租约已过期（属主崩溃现场）。
	_, err = s.db.ExecContext(t.Context(), `UPDATE execution_leases SET expires_at=$2 WHERE kind='run' AND resource_id=$1`, "recheck-run", time.Now().Add(-time.Minute).UnixMilli())
	require.NoError(t, err)

	// 并发的那一笔：先持租约行锁，再把有效期延长到未来，提交发生在 Recover 的对账事务
	// 持锁等待期间。未提交前 Recover 必须停在锁上，不得按旧快照收敛。
	held, err := s.db.pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = held.Rollback() }()
	var one int
	require.NoError(t, held.QueryRowContext(t.Context(), `SELECT 1 FROM execution_leases WHERE kind='run' AND resource_id=$1 FOR UPDATE`, "recheck-run").Scan(&one))
	_, err = held.ExecContext(t.Context(), `UPDATE execution_leases SET expires_at=$2 WHERE kind='run' AND resource_id=$1`, "recheck-run", time.Now().Add(time.Hour).UnixMilli())
	require.NoError(t, err)

	current, err := s.ReadForWorker(t.Context(), root.ID)
	require.NoError(t, err)
	type outcome struct {
		task flow.Task
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		// WithoutCancel：测试结束时 ctx 才会取消，这里必须活到 held 提交之后。
		task, err := s.Recover(context.WithoutCancel(t.Context()), flow.ReconcileTask{
			Request: flow.ResumeRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: root.ID, OperationID: "recover-recheck", Reason: "worker_lease_expired", Expected: current.Version},
			Status:  flow.Queued, Joined: true, Isolated: true, Now: time.Now().UnixMilli(),
		})
		done <- outcome{task: task, err: err}
	}()
	// Recover 第一步已取得根 advisory 锁（TRY 失败即证明），说明它正走向租约行锁并阻塞。
	// 根互斥是 advisory xact lock（lockFlowRoot），不是 flow_tasks 行锁。
	rootKey := advisoryKey("basalt/flow/root/" + root.ID)
	heldByRecover := func() bool {
		tx, err := s.db.pool.BeginTx(context.Background(), nil)
		if err != nil {
			return false
		}
		var got bool
		err = tx.QueryRowContext(context.Background(), `SELECT pg_try_advisory_xact_lock($1)`, rootKey).Scan(&got)
		_ = tx.Rollback()
		return err == nil && !got
	}
	require.Eventually(t, heldByRecover, 5*time.Second, 10*time.Millisecond, "Recover 事务必须先取得根 advisory 锁")
	select {
	case got := <-done:
		t.Fatalf("Recover 在租约行锁被持有期间不得返回：%+v", got)
	default:
	}
	require.NoError(t, held.Commit())

	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.Equal(t, flow.Blocked, got.task.Status, "锁后仍能看到有效 run 租约：必须写 Blocked，不得排队推进")
		require.EqualValues(t, current.Version.ExecutionEpoch+1, got.task.Version.ExecutionEpoch)
	case <-time.After(10 * time.Second):
		t.Fatal("租约行锁释放后 Recover 未返回")
	}
	var state string
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT state FROM flow_node_attempts WHERE node_attempt_id=$1`, claimed.ID).Scan(&state))
	require.Equal(t, "running", state, "Isolated 被推翻时 attempt 一字不动")
	stored, err := s.ReadForWorker(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, flow.Blocked, stored.Status)
	require.NoError(t, NewLeaseStore(s.db).Release(t.Context(), lease))
}

// reconcileMustRead 读取当前根行，供下一次 Recover 取 Expected 版本。
func reconcileMustRead(t *testing.T, s *FlowStore, id string) flow.Task {
	t.Helper()
	task, err := s.ReadForWorker(t.Context(), id)
	require.NoError(t, err)
	return task
}

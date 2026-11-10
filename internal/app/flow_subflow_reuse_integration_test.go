//go:build integration

package app

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// 本文件固定 review ② 第 7 条（子 Flow 恢复）的反例：**父 subflow attempt 处于
// running** 时，子任务里"Run 已完成、节点尚未提交"的崩溃现场还能不能被复用。
//
// 缺陷形态。`advanceSubflow` 先给父节点 `Claim` 一条 running attempt，再进入子任务
// 循环；子任务里的 `advanceOne` 命中完成证据后调用 `reuseCompleted`，而后者用
// `flowNodeRunner.Joined` 判"子树是否在途"，该判据按 `task.RootTaskID` 扫**整棵根
// 子树**——于是刚建出来的父 attempt 自己把复用挡住：`joined=false` →
// `ErrCapacity` → `advanceSubflow` 原样上抛 → `flow.Worker.handleJobError` 把
// `ErrCapacity` 当作可竞争错误吞掉 → 下一轮同样先 Claim 再复用 → 永远推进不了。
//
// 判据分界。`Joined` 的语义是"**这次推进的目标任务**子树里没有 prepared / running
// attempt"（FR-24-20 / FR-24-38）；父 subflow attempt 是**调用方自己**在同一轮里刚
// 建的，既不是"别人的在途执行"，也不是复用候选。把它计入就等于让 subflow 的复用
// 路径自我阻塞。根任务自己的推进不受影响，`HasInFlightNodeAttempt(root)` 的取值
// 语义也不变。
//
// 逆改点（证明用例确实在考生产代码）：
//   - `flowNodeRunner.Joined` 对非根任务也扫整棵根子树 → 第 ③ 步拿到
//     `flow.ErrCapacity`，用例红；
//   - 让 `reuseCompleted` 直接跳过 `Joined`（不做任务作用域收窄）→ 第 ③ 步绿，
//     但"同任务子树仍有在途 attempt 时不得复用"失去保护，由
//     `TestFlow_TaskScopedInFlightAttemptCoversDescendantAttempts` 单独守。
//
// This file pins the sub-flow recovery counterexample: a running parent subflow
// attempt must not block reuse of an already completed child Run.

// subflowRefusalRunner 把"容量不足"与"复用观察"分开计数：
// `Run` 前 N 次返回 `flow.ErrCapacity`（造出"没有 Run 行"的崩溃前半段），
// `ObserveCompleted` 单独计数——复用路径必须走到它，且**不得**再调用 `Run`。
type subflowRefusalRunner struct {
	*distributedNodeRunner
	refusals atomic.Int64
	observes atomic.Int64
}

var _ flow.NodeRunner = (*subflowRefusalRunner)(nil)

func (r *subflowRefusalRunner) Run(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	if r.refusals.Load() > 0 {
		r.refusals.Add(-1)
		r.calls.Add(1)
		return flow.NodeRunResult{}, flow.ErrCapacity
	}
	return r.distributedNodeRunner.Run(ctx, req)
}

func (r *subflowRefusalRunner) ObserveCompleted(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	r.observes.Add(1)
	return r.distributedNodeRunner.ObserveCompleted(ctx, req)
}

// testFlowClock 是夹具用的可控服务时钟：容量推迟窗口按 `PollIntervalMS` 从服务
// 时钟起算，真实时钟下这个窗口可能在 CI 慢机上一次 advance 的中途就过期——生产
// 随后合法地重试叶子，夹具形状便随负载漂移（2026-11-10 CI 上 `middle` attempt
// 偶发 succeeded 即此形态）。用例显式推进时钟：窗口不会在单次推进内部过期，
// 需要跨推进等待时由用例赋值。
// testFlowClock gives fixtures a controllable service clock so the capacity
// deferral window cannot expire halfway through one advance on slow machines.
type testFlowClock struct {
	at time.Time
}

func (c *testFlowClock) Now() time.Time { return c.at }

// TestFlow_SubflowChildCompletedRunReusedUnderRunningParentAttempt 走一遍生产时序：
// 父 subflow 节点领取 → 建子任务 → 子叶子容量拒绝（父 attempt 留在 running、子
// attempt 收敛为 interrupted、无 Run 行）→ 补上"崩溃前子 Run 已 completed"的证据 →
// 过了容量推迟窗口再推进一次，必须复用而不是再次派发 / 再次失败。
func TestFlow_SubflowChildCompletedRunReusedUnderRunningParentAttempt(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := &subflowRefusalRunner{distributedNodeRunner: newDistributedRunner(db)}
	service := newDistributedService(db, runner)
	clock := &testFlowClock{at: time.Now()}
	service.Now = clock.Now

	child := publishDistributedDefinition(t, db, "probe-subflow-child")
	definition := distributedDefinition()
	definition.Entry = "delegate"
	definition.Nodes = []flow.Node{{
		ID:           "delegate",
		Kind:         "subflow",
		Subflow:      &flow.DefinitionRef{UID: child.DefinitionUID, Version: child.Version},
		OutputSchema: child.Definition.Contract.OutputSchema,
		Failure:      flow.FailurePolicy{MaxAttempts: 1},
	}}
	parent := publishDistributedDefinitionWith(t, db, "probe-subflow-parent", definition)
	root := startDistributedTask(t, service, parent, "subflow-reuse-root")

	// ① 容量拒绝：父 attempt 保持在途，子 attempt 退还为 interrupted，没有 Run 行。
	runner.refusals.Store(1)
	deferred, err := advanceUnderFlowRoot(t, db, service, root.RootTaskID, flow.AdvanceRequest{
		Principal: core.LocalAdminCLIPrincipal(), TaskID: root.ID, Expected: root.Version,
	})
	require.NoError(t, err, "容量拒绝只推迟根区间，不得作为推进错误返回")
	require.EqualValues(t, 1, runner.calls.Load(), "第一次推进确实尝试执行了子叶子")

	childTask := readChildTaskForSubflowTest(t, db, root.ID)
	require.Equal(t, 1, childTask.Depth)
	childAttempt := readLatestAttemptForSubflowTest(t, db, childTask.ID)
	require.Equal(t, "interrupted", childAttempt.State, "容量拒绝必须退还子 attempt")
	require.Equal(t, "running", readLatestAttemptForSubflowTest(t, db, root.ID).State,
		"父 subflow attempt 在这一轮里保持 running")

	// ② 崩溃前子 Run 已经写完终态：补上运行证据，它才是复用候选。
	seedDistributedRun(t, db, childTask, childAttempt, "completed")

	// ③ 过了容量推迟窗口，在同一父 attempt 下再推进：子 Run 已完成 → 必须复用。
	// 此刻**根子树确实还有在途 attempt**（父 subflow 节点那条），复用的判据是
	// "目标子任务自己的子树在途吗"，不是"整棵根子树在途吗"——两者取值必须分开。
	require.True(t, gateInFlight(t, postgres.NewSubtreeGate(db), root.RootTaskID),
		"根子树口径：父 subflow attempt 仍在途")
	// 把服务时钟推进到推迟点之后，等价于真实等待窗口过去，但不受 CI 机器速度影响。
	clock.at = time.UnixMilli(childTask.NextEligibleAt).Add(time.Millisecond)
	final, err := advanceUnderFlowRoot(t, db, service, root.RootTaskID, flow.AdvanceRequest{
		Principal: core.LocalAdminCLIPrincipal(), TaskID: root.ID, Expected: deferred.Version,
	})
	require.NoError(t, err, "子 Run 已完成时，父 subflow attempt 的在途状态不得挡住复用")
	require.Equal(t, flow.Succeeded, final.Status)
	require.EqualValues(t, 1, runner.calls.Load(), "复用路径不得派发新 Run")
	require.GreaterOrEqual(t, runner.observes.Load(), int64(1), "复用必须经 ObserveCompleted 观察原 Run")
	require.Equal(t, 1, countDistributedAttempts(t, db, childTask.ID), "复用不新增 attempt")
	require.Equal(t, 1, countDistributedNodeRuns(t, db, root.RootTaskID), "复用不新增 Run")
}

// TestFlow_TaskScopedInFlightAttemptCoversDescendantAttempts 守 `Joined` 收窄后
// **没有变松**：锚点任务自己的 attempt 已收敛（interrupted）时，只要它**后代**还有
// running attempt，`HasInFlightNodeAttemptUnder` 仍必须为 true。
//
// 三层真实形状（根 → 子 → 孙）由生产 `advanceSubflow` 自己建：容量拒绝把孙任务叶子
// 退还成 interrupted，而父 / 祖父 subflow 节点的 attempt 保持 running。随后把子任务
// 自己的 attempt 手工收敛成 interrupted，再用真实 `Claim` 给孙任务领一条 running
// attempt——此时唯一"在途"的只剩后代那一条。
//
// 逆改点：把 `taskInFlightAttemptQuery` 的递归项删掉（等价于只查 `n.task_id=$1`）→
// 本用例第二次断言转红，而主用例仍绿：那正是"收窄过头"的形态。
func TestFlow_TaskScopedInFlightAttemptCoversDescendantAttempts(t *testing.T) {
	db := pgtest.NewTestDB(t)
	runner := &subflowRefusalRunner{distributedNodeRunner: newDistributedRunner(db)}
	service := newDistributedService(db, runner)
	// 夹具阶段只做一次推进：冻结服务时钟，容量推迟窗口不会在推进中途过期，
	// "middle 保持 running、孙任务叶子退还为 interrupted"的形状与机器速度无关。
	clock := &testFlowClock{at: time.Now()}
	service.Now = clock.Now
	store := postgres.NewFlowStore(db)

	grandchildDefinition := publishDistributedDefinition(t, db, "probe-subflow-descendant")
	childDefinition := distributedDefinition()
	childDefinition.Entry = "delegate"
	childDefinition.Nodes = []flow.Node{{
		ID:           "delegate",
		Kind:         "subflow",
		Subflow:      &flow.DefinitionRef{UID: grandchildDefinition.DefinitionUID, Version: grandchildDefinition.Version},
		OutputSchema: grandchildDefinition.Definition.Contract.OutputSchema,
		Failure:      flow.FailurePolicy{MaxAttempts: 1},
	}}
	childPublished := publishDistributedDefinitionWith(t, db, "probe-subflow-middle", childDefinition)
	topDefinition := distributedDefinition()
	topDefinition.Entry = "delegate"
	topDefinition.Nodes = []flow.Node{{
		ID:           "delegate",
		Kind:         "subflow",
		Subflow:      &flow.DefinitionRef{UID: childPublished.DefinitionUID, Version: childPublished.Version},
		OutputSchema: childPublished.Definition.Contract.OutputSchema,
		Failure:      flow.FailurePolicy{MaxAttempts: 1},
	}}
	topPublished := publishDistributedDefinitionWith(t, db, "probe-subflow-top", topDefinition)
	root := startDistributedTask(t, service, topPublished, "subflow-descendant-root")

	runner.refusals.Store(1)
	_, err := advanceUnderFlowRoot(t, db, service, root.RootTaskID, flow.AdvanceRequest{
		Principal: core.LocalAdminCLIPrincipal(), TaskID: root.ID, Expected: root.Version,
	})
	require.NoError(t, err)

	middle := readChildTaskForSubflowTest(t, db, root.ID)
	descendant := readChildTaskForSubflowTest(t, db, middle.ID)
	require.Equal(t, 2, descendant.Depth)
	middleAttempt := readLatestAttemptForSubflowTest(t, db, middle.ID)
	require.Equal(t, "running", middleAttempt.State)
	descendantAttempt := readLatestAttemptForSubflowTest(t, db, descendant.ID)
	require.Equal(t, "interrupted", descendantAttempt.State)

	// 锚点自己的 attempt 收敛后，后代没有在途 → 放行。
	setDistributedAttemptState(t, db, middleAttempt.ID, "interrupted")
	inflight, err := store.HasInFlightNodeAttemptUnder(t.Context(), middle.ID)
	require.NoError(t, err)
	require.False(t, inflight)

	// 后代领一条 running attempt → 锚点子树重新在途（只看自己的 attempt 会漏判）。
	claimed, _ := claimDistributedAttempt(t, db, store, descendant, "attempt-descendant", "run-descendant", "session-descendant", 1)
	inflight, err = store.HasInFlightNodeAttemptUnder(t.Context(), middle.ID)
	require.NoError(t, err)
	require.True(t, inflight, "后代的在途 attempt 必须计入锚点子树")

	// 与根口径对照：同一时刻整棵根子树当然也是在途的。
	require.True(t, gateInFlight(t, postgres.NewSubtreeGate(db), root.RootTaskID))

	setDistributedAttemptState(t, db, claimed.ID, "interrupted")
	inflight, err = store.HasInFlightNodeAttemptUnder(t.Context(), middle.ID)
	require.NoError(t, err)
	require.False(t, inflight, "interrupted 不计入，与根口径一致")
}

// readChildTaskForSubflowTest 读父任务唯一的静态子任务。
func readChildTaskForSubflowTest(t *testing.T, db *postgres.DB, parentID string) flow.Task {
	t.Helper()
	var id string
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT task_id FROM flow_tasks WHERE parent_task_id=$1`, parentID).Scan(&id))
	task, err := postgres.NewFlowStore(db).ReadForWorker(t.Context(), id)
	require.NoError(t, err)
	return task
}

// readLatestAttemptForSubflowTest 读某个任务自己的最近一条节点尝试（不扩到子树）。
func readLatestAttemptForSubflowTest(t *testing.T, db *postgres.DB, taskID string) flow.NodeAttempt {
	t.Helper()
	attempt := flow.NodeAttempt{TaskID: taskID}
	var input string
	require.NoError(t, db.QueryRowContext(t.Context(),
		`SELECT node_attempt_id,node_id,node_invocation_id,attempt,task_revision,execution_epoch,
		        COALESCE(run_id,''),COALESCE(session_id,''),state,input,input_hash,started_at
		   FROM flow_node_attempts WHERE task_id=$1
		  ORDER BY started_at DESC, node_attempt_id DESC LIMIT 1`, taskID).
		Scan(&attempt.ID, &attempt.NodeID, &attempt.NodeInvocationID, &attempt.Attempt,
			&attempt.TaskRevision, &attempt.ExecutionEpoch, &attempt.RunID, &attempt.SessionID,
			&attempt.State, &input, &attempt.InputHash, &attempt.StartedAt))
	attempt.Input = json.RawMessage(input)
	return attempt
}

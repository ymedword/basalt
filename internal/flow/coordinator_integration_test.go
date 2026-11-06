//go:build integration

package flow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
	"github.com/ymedword/basalt/internal/tool"
)

// 本领域接缝测真实持久协调与原Run账本；叶子是脚本，不替代app真执行器验收。
// These public-domain seam tests use real PG and original Run rows, with scripted leaves.
type coordinatorAuthority struct{}

func (coordinatorAuthority) Current(context.Context, flow.Task) (core.Principal, error) {
	return core.LocalAdminCLIPrincipal(), nil
}

func (coordinatorAuthority) CheckStart(context.Context, core.Principal, flow.DefinitionVersion) error {
	return nil
}

func (coordinatorAuthority) CheckAgent(_ context.Context, ref flow.AgentRef, _ string, _ bool) error {
	if ref.UID != "ag_coordinator" {
		return core.ErrUnauthorized
	}
	return nil
}

func (coordinatorAuthority) Freeze(_ context.Context, p core.Principal, _ flow.DefinitionVersion) (core.FlowAuthority, error) {
	return core.FlowAuthority{CallerID: p.CallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin, Scopes: []string{core.ScopeFlowsWrite}, AgentUIDs: []string{"ag_coordinator"}}, nil
}

func (coordinatorAuthority) NoAvailableCallerKey(context.Context, string) (bool, error) {
	return false, nil
}

type coordinatorResolver struct {
	coordinatorAuthority
	store *postgres.FlowStore
}

func (r coordinatorResolver) ReadDefinition(ctx context.Context, ref flow.DefinitionRef) (flow.DefinitionVersion, error) {
	return r.store.ReadDefinition(ctx, ref)
}

type coordinatorOutputs struct{}

func (coordinatorOutputs) CheckOutput(_ context.Context, _ core.Principal, _ flow.Task, out flow.NodeOutput) error {
	if !out.Verified {
		return flow.ErrStateConflict
	}
	return nil
}

type coordinatorLeaf struct {
	db                     *postgres.DB
	calls, active, maximum atomic.Int64
	bad, capacity          bool
	entered, release       chan struct{}
}

func (r *coordinatorLeaf) Joined(context.Context, flow.Task) (bool, error) {
	return r.active.Load() == 0, nil
}

func (r *coordinatorLeaf) Run(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	r.calls.Add(1)
	if r.capacity {
		return flow.NodeRunResult{}, flow.ErrCapacity
	}
	n := r.active.Add(1)
	defer r.active.Add(-1)
	for old := r.maximum.Load(); n > old && !r.maximum.CompareAndSwap(old, n); old = r.maximum.Load() {
	}
	runs := postgres.NewRunStore(r.db)
	err := runs.StartRun(ctx, core.RunRecord{RunID: req.Attempt.RunID, SessionID: req.Attempt.SessionID, Agent: req.Node.Agent.Name, Channel: "cli", UserID: "coordinator-seam", Principal: req.Principal, StartedAt: time.Now(), Flow: req.Flow}, core.RunResourceManifest{})
	if err != nil {
		return flow.NodeRunResult{}, err
	}
	status := core.StatusCompleted
	if r.entered != nil {
		close(r.entered)
		<-ctx.Done()
		<-r.release
		status = core.StatusCancelled
	}
	// 1s → 15s（2026-11-06）：这是夹具写回终态的上限，不是断言——写完立即返回，
	// 正常路径零成本。cover 轮 20 核并行下 1s 预算被 PG 争用打穿时，叶子返回错误，
	// 产品按 fail-closed 把任务停为 blocked（leaf_execution_unconfirmed），表现为
	// 本文件状态断言「expected succeeded / actual blocked」。已用 1ns 上限诱导红态
	// 复现同一症状（回填运行终态: 开启事务: context deadline exceeded），机制同源。
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if _, err := runs.FinishRun(finish, req.Attempt.RunID, status, "", 1, 0, time.Now()); err != nil {
		return flow.NodeRunResult{}, err
	}
	output := json.RawMessage(`{"answer":"coordinator result"}`)
	if r.bad {
		output = json.RawMessage(`{"answer":7}`)
	}
	return flow.NodeRunResult{Run: core.RunResult{RunID: req.Attempt.RunID, Status: status, FinalText: string(output), Iterations: 1}, Output: output, Retryable: r.bad}, nil
}

type coordinatorFixture struct {
	db         *postgres.DB
	store      *postgres.FlowStore
	service    *flow.Service
	publisher  *flow.Publisher
	definition flow.Definition
	leaf       *coordinatorLeaf
}

func newCoordinatorFixture(t *testing.T) *coordinatorFixture {
	t.Helper()
	db, err := postgres.Open(t.Context(), postgres.Config{DSN: pgtest.NewTestSchema(t), MaxConns: 8, MinConns: 8})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	store := postgres.NewFlowStore(db)
	schemas := tool.NewSchemaCompiler()
	authority := coordinatorResolver{store: store}
	config := flow.DefaultConfig()
	config.Enabled = true
	leaf := &coordinatorLeaf{db: db}
	service := &flow.Service{Repository: store, Definitions: store, Schemas: schemas, Authority: authority, Runner: leaf, Outputs: coordinatorOutputs{}, Verification: &flow.VerificationService{Schemas: schemas}, Config: config, Operations: postgres.NewManagementStore(db)}
	publisher := &flow.Publisher{Definitions: store, Operations: service.Operations, Validator: &flow.ContractValidator{Schemas: schemas, Resolver: authority}}
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	budget := config.Budget()
	d := flow.Definition{FormatVersion: 1, Name: "coordinator", TaskContract: "TASK.json", Entry: "draft", Limits: budget, Nodes: []flow.Node{{ID: "draft", Kind: "agent", Agent: &flow.AgentRef{UID: "ag_coordinator", Name: "writer", Version: 1, ConfigHash: "config", ManifestHash: "manifest"}, OutputSchema: schema, Failure: flow.FailurePolicy{MaxAttempts: 1}}}, Contract: flow.TaskContract{ContractVersion: 1, Goal: "return a checked result", Constraints: []string{"never infer successful acceptance"}, InputSchema: schema, OutputSchema: schema, Budget: budget, StopConditions: []string{"budget", "deadline", "unknown"}, Escalation: "show unresolved", ArtifactSpec: []flow.ArtifactSpec{{ID: "report", Format: "json", Required: true}}, Acceptance: []flow.AcceptanceRule{{ID: "shape", Version: 1, Kind: "deterministic", TargetKind: "artifact", TargetID: "report", Required: true, Schema: schema}}}}
	return &coordinatorFixture{db: db, store: store, service: service, publisher: publisher, definition: d, leaf: leaf}
}

func (f *coordinatorFixture) publish(t *testing.T, d flow.Definition, op string) flow.DefinitionVersion {
	t.Helper()
	v, err := f.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: op, Definition: d})
	require.NoError(t, err)
	return v
}

func (f *coordinatorFixture) start(t *testing.T, v flow.DefinitionVersion, key string) flow.Task {
	t.Helper()
	task, err := f.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: v.DefinitionUID, IdempotencyKey: key, Input: json.RawMessage(`{"answer":"input"}`)})
	require.NoError(t, err)
	return task
}

func (f *coordinatorFixture) advance(t *testing.T, task flow.Task) flow.Task {
	t.Helper()
	next, err := f.service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version})
	require.NoError(t, err)
	return next
}

func TestFlow_CoordinatorOriginalLedgerLifecycle(t *testing.T) {
	for _, mode := range []string{"chain", "retired-receipt", "capacity", "invalid-output", "missing-human", "cancel-queued", "revision", "subflow"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoordinatorFixture(t)
			d := f.definition
			if mode == "chain" {
				for i := range 2 {
					node := d.Nodes[0]
					node.ID = fmt.Sprint("next-", i)
					node.Next = ""
					node.Inputs = []flow.InputMapping{{Target: "answer", Source: "outputs." + d.Nodes[len(d.Nodes)-1].ID + ".answer"}}
					d.Nodes[len(d.Nodes)-1].Next = node.ID
					d.Nodes = append(d.Nodes, node)
				}
			}
			if mode == "missing-human" {
				d.Contract.Acceptance[0] = flow.AcceptanceRule{ID: "human", Version: 1, Kind: "judgment", TargetKind: "artifact", TargetID: "report", Required: true, HumanRequired: true}
			}
			if mode == "revision" {
				d.Nodes[0].Failure = flow.FailurePolicy{MaxAttempts: 1, MaxReworks: 1, ReworkTarget: "draft"}
			}
			if mode == "subflow" {
				child := d
				child.Name = "child"
				v := f.publish(t, child, "publish-child")
				d.Nodes = []flow.Node{{ID: "delegate", Kind: "subflow", Subflow: &flow.DefinitionRef{UID: v.DefinitionUID, Version: v.Version}, OutputSchema: child.Contract.OutputSchema, Failure: flow.FailurePolicy{MaxAttempts: 1}}}
				d.Entry = "delegate"
			}
			v := f.publish(t, d, "publish-root")
			task := f.start(t, v, "start")
			require.Equal(t, task.ID, f.start(t, v, "start").ID)
			if mode == "retired-receipt" {
				_, err := f.publisher.Retire(t.Context(), flow.RetireRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, DefinitionUID: v.DefinitionUID, Version: v.Version, ExpectedVersion: v.Version, OperationID: "retire", Reason: "retire future acceptance"})
				require.NoError(t, err)
				require.Equal(t, task.ID, f.start(t, v, "start").ID)
				_, err = f.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: v.DefinitionUID, IdempotencyKey: "new", Input: task.Revision.Input})
				require.ErrorIs(t, err, flow.ErrDefinitionRetired)
				return
			}
			if mode == "cancel-queued" {
				closed, err := f.service.Cancel(t.Context(), flow.CancelRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "cancel", Reason: "close before run"})
				require.NoError(t, err)
				require.Equal(t, flow.Cancelled, closed.Status)
				require.Zero(t, f.leaf.calls.Load())
				return
			}
			if mode == "capacity" {
				f.leaf.capacity = true
				task = f.advance(t, task)
				require.Zero(t, task.Revision.Attempts["draft"])
				require.Equal(t, flow.Running, task.Status)
				f.leaf.capacity = false
			}
			f.leaf.bad = mode == "invalid-output" || mode == "revision"
			task = f.advance(t, task)
			if mode == "revision" {
				require.Equal(t, flow.Blocked, task.Status)
				next, err := f.service.Revise(t.Context(), flow.RevisionRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "revise", Proposal: json.RawMessage(`{"reason":"correct output","node_id":"draft"}`)})
				require.NoError(t, err)
				require.EqualValues(t, 1, next.Version.Revision)
				f.leaf.bad = false
				task = f.advance(t, next)
			}
			for n := 0; n < 8 && task.Status == flow.Running; n++ {
				task = f.advance(t, task)
			}
			want := flow.Succeeded
			if mode == "invalid-output" {
				want = flow.Failed
			}
			if mode == "missing-human" {
				want = flow.Blocked
			}
			// 断言不变；补 reason 只为下次失败能当场看到产品侧的停因（如
			// leaf_execution_unconfirmed），不改变判据。
			require.Equal(t, want, task.Status, "reason=%q", task.Reason)
			graph, err := f.store.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
			require.NoError(t, err)
			if want == flow.Succeeded {
				require.Len(t, graph.Artifacts, 1)
				require.Equal(t, "accepted", graph.Artifacts[0].State)
			}
			if mode == "chain" {
				require.EqualValues(t, 3, f.leaf.calls.Load())
				var count int
				require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT count(DISTINCT r.session_id) FROM runs r JOIN flow_tasks t ON t.task_id=r.flow_task_id WHERE t.root_task_id=$1`, task.ID).Scan(&count))
				require.Equal(t, 3, count)
			}
		})
	}
}

func TestFlow_CoordinatorWorkerNeverStartsAtConstruction(t *testing.T) {
	f := newCoordinatorFixture(t)
	v := f.publish(t, f.definition, "publish")
	var tasks []flow.Task
	for i := range 5 {
		tasks = append(tasks, f.start(t, v, fmt.Sprint("worker-", i)))
	}
	f.service.Config.MaxActive = 2
	f.service.Config.PollIntervalMS = 100
	worker := &flow.Worker{Service: f.service, Repository: f.store, Startup: func(context.Context) error { return nil }}
	require.Zero(t, f.leaf.calls.Load())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	require.Eventually(t, func() bool {
		var count int
		return f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_tasks WHERE status='succeeded'`).Scan(&count) == nil && count == 5
	}, 8*time.Second, 20*time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		require.ErrorIs(t, err, context.Canceled)
	}
	require.EqualValues(t, 5, f.leaf.calls.Load())
	for _, original := range tasks {
		finished, err := f.store.Read(t.Context(), core.LocalAdminCLIPrincipal(), original.ID)
		require.NoError(t, err)
		require.Equal(t, flow.Succeeded, finished.Status, "every accepted task must finish")
	}
	require.LessOrEqual(t, f.leaf.maximum.Load(), int64(2))
	require.Zero(t, f.leaf.active.Load())
}

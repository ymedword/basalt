//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// This seam probe uses the original Run ledger but a mock leaf; it is not proof
// of the complete app/AgentService/model/tool wiring or of model output quality.
type flowAdvanceRunner struct {
	store    *FlowStore
	calls    atomic.Int64
	output   json.RawMessage
	block    chan struct{}
	release  chan struct{}
	capacity bool
}

func (r *flowAdvanceRunner) Run(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	r.calls.Add(1)
	if r.capacity {
		return flow.NodeRunResult{}, flow.ErrCapacity
	}
	runs := NewRunStore(r.store.db)
	if err := runs.StartRun(ctx, core.RunRecord{RunID: req.Attempt.RunID, SessionID: req.Attempt.SessionID, Agent: req.Node.Agent.Name, Channel: "cli", UserID: "flow-probe", Principal: req.Principal, StartedAt: time.Now(), Flow: req.Flow}, core.RunResourceManifest{}); err != nil {
		return flow.NodeRunResult{}, err
	}
	if r.block != nil {
		close(r.block)
		<-ctx.Done()
		if r.release != nil {
			<-r.release
		}
		// 1s → 15s（2026-11-06）：夹具写回终态的上限而非断言，正常路径写完即返回。
		// 与 internal/flow 同族夹具同步抬高，避免并行判卷下 PG 争用把上限打穿。
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if _, err := runs.FinishRun(finish, req.Attempt.RunID, core.StatusCancelled, "cancelled", 0, 0, time.Now()); err != nil {
			return flow.NodeRunResult{}, err
		}
		return flow.NodeRunResult{Run: core.RunResult{RunID: req.Attempt.RunID, Status: core.StatusCancelled}}, nil
	}
	if _, err := runs.FinishRun(ctx, req.Attempt.RunID, core.StatusCompleted, "", 1, 0, time.Now()); err != nil {
		return flow.NodeRunResult{}, err
	}
	return flow.NodeRunResult{Run: core.RunResult{RunID: req.Attempt.RunID, Status: core.StatusCompleted, FinalText: string(r.output), Iterations: 1}, Output: r.output}, nil
}
func (r *flowAdvanceRunner) Joined(context.Context, flow.Task) (bool, error) { return true, nil }

func TestFlow_AdvanceVerifiesBeforeAtomicSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		status flow.TaskStatus
	}{
		{"valid", `{"answer":"verified structure"}`, flow.Succeeded},
		{"bad-shape", `{"answer":7}`, flow.Failed},
		{"malformed", `not-json`, flow.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, def := newFlowStoreTest(t)
			service := flowServiceForStore(s)
			runner := &flowAdvanceRunner{store: s, output: json.RawMessage(tc.output)}
			service.Runner = runner
			task, err := service.Start(t.Context(), startFlowRequest(def, "advance"))
			require.NoError(t, err)
			finished, err := service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version})
			require.NoError(t, err)
			require.Equal(t, tc.status, finished.Status)
			require.EqualValues(t, 1, runner.calls.Load())
			graph, err := s.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
			require.NoError(t, err)
			if tc.status == flow.Succeeded {
				require.Len(t, graph.Artifacts, 1)
				require.Equal(t, "accepted", graph.Artifacts[0].State)
				var count int
				require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_verifications WHERE task_id=$1 AND outcome='pass'`, task.ID).Scan(&count))
				require.Equal(t, 1, count)
			} else {
				require.Empty(t, graph.Artifacts)
			}
			_, err = service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: finished.Version})
			require.ErrorIs(t, err, flow.ErrTaskClosed)
			require.EqualValues(t, 1, runner.calls.Load())
		})
	}
}

func TestFlow_AdvanceCapacityYieldsWithoutRun(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	runner := &flowAdvanceRunner{store: s, capacity: true}
	service.Runner = runner
	task, err := service.Start(t.Context(), startFlowRequest(def, "defer"))
	require.NoError(t, err)
	deferred, err := service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version})
	require.NoError(t, err)
	require.Equal(t, flow.Running, deferred.Status)
	require.Zero(t, deferred.Revision.Attempts["draft"])
	require.Greater(t, deferred.NextEligibleAt, deferred.UpdatedAt)
	var count int
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&count))
	require.Zero(t, count)
	runner.capacity = false
	runner.output = json.RawMessage(`{"answer":"ready"}`)
	finished, err := service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: deferred.Version})
	require.NoError(t, err)
	require.Equal(t, flow.Succeeded, finished.Status)
	require.Equal(t, 1, finished.Revision.Attempts["draft"])
}

func TestFlow_CancelRequiresRealLeafJoin(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	runner := &flowAdvanceRunner{store: s, block: make(chan struct{}), release: make(chan struct{})}
	service.Runner = runner
	task, err := service.Start(t.Context(), startFlowRequest(def, "cancel-join"))
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() {
		_, err := service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version})
		result <- err
	}()
	select {
	case <-runner.block:
	case <-time.After(3 * time.Second):
		t.Fatal("leaf did not start")
	}
	task, err = s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	cancelCtx, cancel := context.WithCancel(t.Context())
	cancelled := make(chan flow.Task, 1)
	failures := make(chan error, 1)
	go func() {
		v, err := service.Cancel(cancelCtx, flow.CancelRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "cancel-active", Reason: "user stopped"})
		cancelled <- v
		failures <- err
	}()
	deadline := time.After(3 * time.Second)
	for {
		current, err := s.ReadForWorker(t.Context(), task.ID)
		require.NoError(t, err)
		if current.Status == flow.Cancelling {
			break
		}
		select {
		case <-deadline:
			t.Fatal("close intent not persisted")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel() // Returning to the caller is not proof the leaf has joined.
	current := <-cancelled
	require.NoError(t, <-failures)
	require.Equal(t, flow.Cancelling, current.Status)
	stored, err := s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, flow.Cancelling, stored.Status)
	close(runner.release)
	require.NoError(t, <-result)
	stored, err = s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, flow.Cancelled, stored.Status)
	require.True(t, stored.CancelRequested)
	graph, err := s.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
	require.NoError(t, err)
	require.Empty(t, graph.Artifacts)
}

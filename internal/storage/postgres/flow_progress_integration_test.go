//go:build integration

package postgres

import (
	"encoding/json"
	"fmt"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func TestFlow_ProgressWindowSurvivesRevision(t *testing.T) {
	p := newPGFlowActionProbe(t)
	guard := &FlowProgress{Store: p.store, Principal: p.env.Principal, TaskID: p.task.ID, NodeAttemptID: "node-action"}
	p.execution.Flow.Progress = guard
	for i := range 3 {
		p.execution.Request.LogicalSlot = fmt.Sprintf("slot-%d", i)
		_, out, err := p.executor.ExecuteFlowStep(t.Context(), p.env, []string{p.call.Name}, p.call, core.ToolStep{StepID: "1", Seq: 1}, p.execution)
		if i == 2 {
			var limit *core.ProgressLimitError
			require.ErrorAs(t, err, &limit)
			require.Equal(t, "repeated_call", limit.Reason)
			require.False(t, out.Dispatched)
		} else {
			require.NoError(t, err)
			require.True(t, out.Dispatched)
			require.True(t, out.AuditConfirmed)
		}
	}
	require.EqualValues(t, 2, p.received.Load())
	task, err := p.store.ReadForWorker(t.Context(), p.task.ID)
	require.NoError(t, err)
	window := task.Revision.Progress[task.ID+":draft"]
	require.Len(t, window.Calls, 2)
	require.Equal(t, 2, window.NoProgress)
	output := json.RawMessage(`{}`)
	task, err = p.store.Commit(t.Context(), flow.CommitNode{Principal: p.env.Principal, TaskID: task.ID, AttemptID: "node-action", OperationID: "progress-block", Expected: task.Version, Revision: task.Revision, Outcome: flow.Blocked, Reason: "verification unavailable", AttemptState: "failed", Output: output, OutputHash: flowDataHash(output), Now: task.UpdatedAt + 1})
	require.NoError(t, err)
	next := task.Revision
	next.Revision++
	next.Reworks = maps.Clone(task.Revision.Reworks)
	next.Reworks[task.Revision.NodeID]++
	next.Summary.TaskRevision = next.Revision
	reset := next
	reset.Progress = nil
	_, err = p.store.Revise(t.Context(), flow.AcceptRevision{Request: flow.RevisionRequest{Principal: p.env.Principal, TaskID: task.ID, OperationID: "illegal-reset", Expected: task.Version}, Revision: reset, Now: task.UpdatedAt + 1})
	require.Error(t, err)
	task, err = p.store.Revise(t.Context(), flow.AcceptRevision{Request: flow.RevisionRequest{Principal: p.env.Principal, TaskID: task.ID, OperationID: "progress-revise", Expected: task.Version}, Revision: next, Now: task.UpdatedAt + 1})
	require.NoError(t, err)
	_, err = p.store.Claim(t.Context(), flow.ClaimNode{Principal: p.env.Principal, TaskID: task.ID, Expected: task.Version, Now: task.UpdatedAt + 1, Attempt: flow.NodeAttempt{ID: "node-after-revision", TaskID: task.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 2, TaskRevision: task.Version.Revision, ExecutionEpoch: task.Version.ExecutionEpoch, Input: task.Revision.Input, InputHash: task.Revision.InputHash}})
	require.NoError(t, err)
	guard.NodeAttemptID = "node-after-revision"
	var limit *core.ProgressLimitError
	require.ErrorAs(t, guard.BeforeCall(t.Context(), p.call), &limit)
	require.Equal(t, "repeated_call", limit.Reason)
	require.EqualValues(t, 2, p.received.Load(), "revision must not enable a third dispatch")
}

func TestFlow_ProgressObservationFailureBlocksNextDispatch(t *testing.T) {
	p := newPGFlowActionProbe(t)
	p.execution.Flow.Progress = &FlowProgress{Store: p.store, Principal: p.env.Principal, TaskID: p.task.ID, NodeAttemptID: "node-action"}
	_, out, err := p.run(t.Context())
	require.NoError(t, err)
	require.True(t, out.Dispatched)
	installFlowFault(t, p.store, "flow_task_revisions", "UPDATE", "")
	p.execution.Request.LogicalSlot = "second-call"
	_, out, err = p.run(t.Context())
	require.Error(t, err)
	require.True(t, out.Dispatched)
	require.True(t, out.AuditConfirmed)
	var fired bool
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT is_called FROM flow_probe_fault_seq`).Scan(&fired))
	require.True(t, fired)
	_, err = p.store.db.pool.ExecContext(t.Context(), `DROP TRIGGER flow_probe_fault_trigger ON flow_task_revisions`)
	require.NoError(t, err)
	// Discard all guard/store memory, as a restart or a new node attempt would.
	p.execution.Flow.Progress = &FlowProgress{Store: NewFlowStore(p.store.db), Principal: p.env.Principal, TaskID: p.task.ID, NodeAttemptID: "node-action"}
	p.execution.Request.LogicalSlot = "third-call"
	_, out, err = p.run(t.Context())
	require.ErrorIs(t, err, flow.ErrStateConflict)
	require.False(t, out.Dispatched)
	require.EqualValues(t, 2, p.received.Load())
}

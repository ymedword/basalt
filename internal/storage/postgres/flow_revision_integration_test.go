//go:build integration

package postgres

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func TestFlow_RevisionClosedProposalAndImmutableLimits(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	service.Runner = &flowAdvanceRunner{store: s}
	task, err := service.Start(t.Context(), startFlowRequest(def, "revise"))
	require.NoError(t, err)
	require.NoError(t, s.ParkTask(t.Context(), core.LocalAdminCLIPrincipal(), task.ID, task.Version, "verification_missing"))
	task, err = s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	req := flow.RevisionRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, OperationID: "revise-1", Expected: task.Version}
	for _, proposal := range []string{
		`{"reason":"fix","node_id":"draft","budget":{"max_total_tokens":99999999}}`,
		`{"reason":"fix","node_id":"draft","constraints":[]}`,
		`{"reason":"fix","node_id":"draft","nodes":[]}`,
		`{"reason":"fix","node_id":"unknown"}`,
		`{"reason":"fix","node_id":"draft","input":{"answer":3}}`,
		`{"reason":"fix","node_id":"draft","node_id":"draft"}`,
	} {
		req.Proposal = json.RawMessage(proposal)
		_, err := service.Revise(t.Context(), req)
		require.Error(t, err, proposal)
	}
	req.Proposal = json.RawMessage(`{"reason":"correct bounded parameter","node_id":"draft","input":{"answer":"corrected"}}`)
	revised, err := service.Revise(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, flow.Queued, revised.Status)
	require.EqualValues(t, 1, revised.Version.Revision)
	require.Equal(t, task.Contract, revised.Contract)
	require.Equal(t, task.Deadline, revised.Deadline)
	require.Equal(t, task.Revision.Attempts, revised.Revision.Attempts)
	require.Equal(t, task.Revision.Progress, revised.Revision.Progress)
	require.Equal(t, task.Revision.ExpandedSteps, revised.Revision.ExpandedSteps)
	require.JSONEq(t, `{"answer":"corrected"}`, string(revised.Revision.Input))
	again, err := service.Revise(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, revised.Version, again.Version)
	req.Proposal = json.RawMessage(`{"reason":"changed same key","node_id":"draft"}`)
	_, err = service.Revise(t.Context(), req)
	require.ErrorIs(t, err, flow.ErrIdempotencyConflict)
	var oldInput string
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT checkpoint::jsonb->>'input' FROM flow_task_revisions WHERE task_id=$1 AND revision=0`, task.ID).Scan(&oldInput))
	require.JSONEq(t, `{"answer":"input"}`, oldInput)
	// Two revisions are a task ceiling, not a fresh allowance per parked epoch.
	for i := range 2 {
		require.NoError(t, s.ParkTask(t.Context(), core.LocalAdminCLIPrincipal(), task.ID, revised.Version, "verification_missing"))
		parked, err := s.ReadForWorker(t.Context(), task.ID)
		require.NoError(t, err)
		req.Expected = parked.Version
		req.OperationID = []string{"revise-2", "revise-3"}[i]
		req.Proposal = json.RawMessage(`{"reason":"bounded retry","node_id":"draft"}`)
		next, err := service.Revise(t.Context(), req)
		if i == 0 {
			require.NoError(t, err)
			revised = next
		} else {
			require.ErrorIs(t, err, flow.ErrBudget)
		}
	}
}

func TestFlow_RevisionCheckpointFaultIsAtomic(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	service.Runner = &flowAdvanceRunner{store: s}
	task, err := service.Start(t.Context(), startFlowRequest(def, "revision-fault"))
	require.NoError(t, err)
	require.NoError(t, s.ParkTask(t.Context(), core.LocalAdminCLIPrincipal(), task.ID, task.Version, "verification_missing"))
	task, err = s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	req := flow.RevisionRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "revise-fault", Proposal: json.RawMessage(`{"reason":"retry","node_id":"draft"}`)}
	installFlowFault(t, s, "flow_task_revisions", "INSERT", "")
	_, err = service.Revise(t.Context(), req)
	require.Error(t, err)
	var fired bool
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT is_called FROM flow_probe_fault_seq`).Scan(&fired))
	require.True(t, fired)
	after, err := s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	require.Equal(t, task.Version, after.Version)
	var count int
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_task_revisions WHERE task_id=$1`, task.ID).Scan(&count))
	require.Equal(t, 1, count)
	_, err = s.db.pool.ExecContext(t.Context(), `DROP TRIGGER flow_probe_fault_trigger ON flow_task_revisions`)
	require.NoError(t, err)
	revised, err := service.Revise(t.Context(), req)
	require.NoError(t, err)
	require.EqualValues(t, 1, revised.Version.Revision)
}

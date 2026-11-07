//go:build integration

package postgres

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func TestFlow_UnadmittedCapacityDoesNotConsumeAttempt(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-yield", "yield-key"))
	require.NoError(t, err)
	task = claimFlowForTest(t, s, task)
	eligible := task.UpdatedAt + 250
	// YieldUnadmitted 写回 running + next_eligible_at，属推进类：执行期间持根租约，
	// 写完即释放，让下面的就绪扫描能看见这颗空闲根。
	lease := flowRootLeaseFor(t, s.db, task.RootTaskID)
	require.NoError(t, s.YieldUnadmitted(t.Context(), core.LocalAdminCLIPrincipal(), task.ID, "attempt-"+task.ID, task.Version, eligible))
	require.NoError(t, s.YieldUnadmitted(t.Context(), core.LocalAdminCLIPrincipal(), task.ID, "attempt-"+task.ID, task.Version, eligible), "same refusal fact is idempotent")
	require.NoError(t, NewLeaseStore(s.db).Release(t.Context(), lease))
	task, err = s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	require.Zero(t, task.Revision.Attempts["draft"])
	require.Zero(t, task.Revision.ExpandedSteps)
	require.Equal(t, flow.Running, task.Status)
	require.Equal(t, eligible, task.NextEligibleAt)
	ready, err := s.ReadyRoots(t.Context(), eligible-1, 32)
	require.NoError(t, err)
	require.Empty(t, ready)
	ready, err = s.ReadyRoots(t.Context(), eligible, 32)
	require.NoError(t, err)
	require.Len(t, ready, 1)
	acquireFlowRootLease(t, s.db, task.RootTaskID)
	_, err = s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Now: eligible, Attempt: flow.NodeAttempt{ID: "yield-retry", TaskID: task.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: task.Version.ExecutionEpoch, Input: task.Revision.Input, InputHash: task.Revision.InputHash}})
	require.NoError(t, err)
	// A separate admitted Run cannot be refunded even if it made no tool call.
	p := newPGFlowActionProbe(t)
	require.ErrorIs(t, p.store.YieldUnadmitted(t.Context(), p.env.Principal, p.task.ID, "node-action", p.task.Version, p.task.UpdatedAt+250), flow.ErrStateConflict)
}

func TestFlow_TaskPagesAndOutputProvenance(t *testing.T) {
	s, def := newFlowStoreTest(t)
	for _, id := range []string{"task-page-a", "task-page-b", "task-page-c"} {
		_, err := s.Create(t.Context(), flowCreateRequest(def, id, id))
		require.NoError(t, err)
	}
	p := core.LocalAdminCLIPrincipal()
	first, cursor, err := s.ListTasks(t.Context(), p, "", 2, false, "")
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.NotEmpty(t, cursor)
	last, end, err := s.ListTasks(t.Context(), p, cursor, 2, false, "")
	require.NoError(t, err)
	require.Len(t, last, 1)
	require.Empty(t, end)
	require.NotEqual(t, first[1].ID, last[0].ID)
	_, _, err = s.ListTasks(t.Context(), p, "%%%", 2, false, "")
	require.Error(t, err)
	_, _, err = s.ListTasks(t.Context(), p, "", 101, false, "")
	require.Error(t, err)
	task := claimFlowForTest(t, s, first[0])
	commit := flowCommitForTest(task)
	commit.Revision.Outputs["draft"] = flow.NodeOutput{Value: commit.Output, Hash: commit.OutputHash, NodeAttemptID: commit.AttemptID, Revision: 0, Verified: true}
	task, err = s.Commit(t.Context(), commit)
	require.NoError(t, err)
	refs, err := s.OutputEvidence(t.Context(), p, task.ID, task.Revision.Outputs["draft"])
	require.NoError(t, err)
	require.Empty(t, refs)
	bad := task.Revision.Outputs["draft"]
	bad.Value = json.RawMessage(`{"answer":"changed"}`)
	_, err = s.OutputEvidence(t.Context(), p, task.ID, bad)
	require.Error(t, err)
	bad = task.Revision.Outputs["draft"]
	bad.NodeAttemptID = "missing"
	_, err = s.OutputEvidence(t.Context(), p, task.ID, bad)
	require.Error(t, err)
	attempts, next, err := s.ListAttempts(t.Context(), p, task.ID, "", 1)
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	require.Empty(t, next)
	require.Equal(t, commit.AttemptID, attempts[0].ID)
	left, err := s.Remaining(t.Context(), p, task.ID)
	require.NoError(t, err)
	require.EqualValues(t, task.Contract.Budget.MaxTotalTokens, left.Tokens)
}

func TestFlow_TaskRecentOrderAndWorkflowFilter(t *testing.T) {
	s, def := newFlowStoreTest(t)
	other := flowStoreDefinition()
	other.Name = "other-workflow"
	require.NoError(t, NewManagementStore(s.db).BeginOperation(t.Context(), core.OperationRecord{OperationID: "publish-other", Action: "flow.publish", TargetKind: "flow_definition", TargetID: "fd_other", ActorSubject: "admin", AuthMethod: core.AuthMethodCLI, Source: "cli", RequestedVersionID: 1}))
	otherVersion, err := s.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: "fd_other", OperationID: "publish-other", Definition: other})
	require.NoError(t, err)
	for _, entry := range []struct {
		id         string
		at         int64
		definition flow.DefinitionVersion
	}{{"z-old", 10, def}, {"a-new", 30, def}, {"b-new", 30, def}, {"c-other", 40, otherVersion}} {
		_, err = s.Create(t.Context(), flowCreateRequest(entry.definition, entry.id, entry.id))
		require.NoError(t, err)
		_, err = s.db.ExecContext(t.Context(), `UPDATE flow_tasks SET updated_at=$1 WHERE task_id=$2`, entry.at, entry.id)
		require.NoError(t, err)
	}
	p := core.LocalAdminCLIPrincipal()
	all, _, err := s.ListTasks(t.Context(), p, "", 2, false, "")
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "c-other", all[0].ID)
	first, cursor, err := s.ListTasks(t.Context(), p, "", 2, false, "report")
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, "b-new", first[0].ID)
	require.Equal(t, "a-new", first[1].ID)
	require.Equal(t, "report", first[0].AcceptedVersion.Definition.Name)
	last, end, err := s.ListTasks(t.Context(), p, cursor, 2, false, "report")
	require.NoError(t, err)
	require.Len(t, last, 1)
	require.Equal(t, "z-old", last[0].ID)
	require.Empty(t, end)
	missing, _, err := s.ListTasks(t.Context(), p, "", 2, false, "missing")
	require.NoError(t, err)
	require.Empty(t, missing)
	foreignPrincipal := core.Principal{Kind: core.PrincipalCaller, CallerID: "other-owner", KeyID: "other-key", Scopes: []string{core.ScopeFlowsRead}, Actor: core.ActorCaller}
	foreign, _, err := s.ListTasks(t.Context(), foreignPrincipal, "", 2, false, "report")
	require.NoError(t, err)
	require.Empty(t, foreign, "workflow filter must not bypass ownership")
	admin, _, err := s.ListTasks(t.Context(), core.LocalAdminPrincipal(), "", 2, true, "other-workflow")
	require.NoError(t, err)
	require.Len(t, admin, 1)
	require.Equal(t, "c-other", admin[0].ID)
}

func TestFlow_AttemptChronologicalPagination(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "chronological-task", "chronological-key"))
	require.NoError(t, err)
	for i, entry := range []struct {
		id string
		at int64
	}{{"b-tie", 30}, {"z-early", 10}, {"a-tie", 30}} {
		_, err = s.db.ExecContext(t.Context(), `INSERT INTO flow_node_attempts(task_id,node_attempt_id,node_id,node_invocation_id,attempt,task_revision,execution_epoch,state,input,input_hash,started_at,finished_at) VALUES($1,$2,'draft','stable-invocation',$3,0,1,'failed',$4,$5,$6,$6)`, task.ID, entry.id, i+1, string(task.Revision.Input), task.Revision.InputHash, entry.at)
		require.NoError(t, err)
	}
	first, next, err := s.ListAttempts(t.Context(), core.LocalAdminPrincipal(), task.ID, "", 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, "z-early", first[0].ID)
	require.Equal(t, "a-tie", first[1].ID)
	require.NotEmpty(t, next)
	last, end, err := s.ListAttempts(t.Context(), core.LocalAdminPrincipal(), task.ID, next, 2)
	require.NoError(t, err)
	require.Len(t, last, 1)
	require.Equal(t, "b-tie", last[0].ID)
	require.Empty(t, end)
	_, _, err = s.ListAttempts(t.Context(), core.LocalAdminPrincipal(), task.ID, "bm90LWEtdGltZQ", 2)
	require.Error(t, err, "opaque cursor must bind time and tie-break identity")
}

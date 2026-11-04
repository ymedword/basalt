//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func flowStoreDefinition() flow.Definition {
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	cap := flow.BudgetCap{MaxTotalTokens: 1000, MaxToolCalls: 10, MaxDurationSeconds: 60, MaxDepth: 3, MaxChildren: 16, MaxAttemptsPerNode: 3, MaxRevisions: 2}
	return flow.Definition{FormatVersion: 1, Name: "report", Entry: "draft", TaskContract: "TASK.json", Limits: cap, Nodes: []flow.Node{{ID: "draft", Kind: "agent", Agent: &flow.AgentRef{UID: "ag_one", Name: "writer", Version: 1, ConfigHash: "config", ManifestHash: "manifest"}, OutputSchema: schema, Failure: flow.FailurePolicy{MaxAttempts: 1}}}, Contract: flow.TaskContract{ContractVersion: 1, Goal: "answer", Constraints: []string{"keep evidence"}, InputSchema: schema, OutputSchema: schema, Budget: cap, StopConditions: []string{"budget", "deadline", "unknown"}, Escalation: "show unresolved", ArtifactSpec: []flow.ArtifactSpec{{ID: "report", Format: "json", Required: true}}, Acceptance: []flow.AcceptanceRule{{ID: "shape", Version: 1, Kind: "deterministic", TargetKind: "artifact", TargetID: "report", Required: true, Schema: schema}}}}
}

func publishFlowStoreDefinition(t *testing.T, s *FlowStore, def flow.Definition, expected int64) flow.DefinitionVersion {
	t.Helper()
	version := expected + 1
	op := fmt.Sprintf("publish-%d", version)
	require.NoError(t, NewManagementStore(s.db).BeginOperation(t.Context(), core.OperationRecord{OperationID: op, Action: "flow.publish", TargetKind: "flow_definition", TargetID: "fd_test", ActorSubject: "admin", AuthMethod: core.AuthMethodCLI, Source: "cli", RequestedVersionID: version, CreatedAt: time.Now()}))
	result, err := s.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: "fd_test", ExpectedVersion: expected, OperationID: op, Definition: def})
	require.NoError(t, err)
	return result
}

func flowCreateRequest(def flow.DefinitionVersion, id, key string) flow.CreateTask {
	input := json.RawMessage(`{"answer":"input"}`)
	now := time.Now().UnixMilli()
	return flow.CreateTask{Request: flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: def.DefinitionUID, IdempotencyKey: key}, TaskID: id, Definition: def, Contract: def.Definition.Contract, Input: input, InputHash: flowDataHash(input), RequestHash: "semantic-original-input-and-budget", CreatedAt: now, Deadline: now + 60000, Config: flow.DefaultConfig()}
}

func newFlowStoreTest(t *testing.T) (*FlowStore, flow.DefinitionVersion) {
	t.Helper()
	s := NewFlowStore(newTxTestDB(t))
	return s, publishFlowStoreDefinition(t, s, flowStoreDefinition(), 0)
}

func TestFlow_DefinitionPublishFreezesVersion(t *testing.T) {
	s, first := newFlowStoreTest(t)
	def := flowStoreDefinition()
	def.Contract.Goal = "updated goal"
	second := publishFlowStoreDefinition(t, s, def, 1)
	require.EqualValues(t, 2, second.Version)
	require.NotEqual(t, first.ContentHash, second.ContentHash)
	old, err := s.ReadDefinition(t.Context(), flow.DefinitionRef{UID: first.DefinitionUID, Version: 1})
	require.NoError(t, err)
	require.Equal(t, "answer", old.Definition.Contract.Goal)
	require.Equal(t, first.ContentHash, old.ContentHash)
	_, err = s.ResolveCurrent(t.Context(), first.DefinitionUID, 1)
	require.ErrorIs(t, err, flow.ErrDefinitionNotCurrent)
	var status string
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT status FROM management_operations WHERE operation_id='publish-2'`).Scan(&status))
	require.Equal(t, "success", status, "pointer/content and management terminal must commit together")
}

func TestFlow_StartIdempotency(t *testing.T) {
	s, def := newFlowStoreTest(t)
	const concurrent = 12
	results := make(chan flow.Task, concurrent)
	failures := make(chan error, concurrent)
	var group sync.WaitGroup
	for i := range concurrent {
		group.Go(func() {
			req := flowCreateRequest(def, fmt.Sprintf("task-%d", i), "same-key")
			task, err := s.Create(t.Context(), req)
			if err != nil {
				failures <- err
			} else {
				results <- task
			}
		})
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	winner := ""
	count := 0
	for task := range results {
		count++
		if winner == "" {
			winner = task.ID
		}
		require.Equal(t, winner, task.ID)
		require.EqualValues(t, 1, task.Version.StateVersion)
	}
	require.Equal(t, concurrent, count)
	var rows int
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_tasks`).Scan(&rows))
	require.Equal(t, 1, rows)
	changed := flowCreateRequest(def, "different-task", "same-key")
	changed.RequestHash = "changed"
	_, err := s.Create(t.Context(), changed)
	require.ErrorIs(t, err, flow.ErrIdempotencyConflict)
}

func TestFlow_IdempotencyAfterDefinitionSwitch(t *testing.T) {
	s, def := newFlowStoreTest(t)
	req := flowCreateRequest(def, "task-switch", "key-switch")
	first, err := s.Create(t.Context(), req)
	require.NoError(t, err)
	updated := flowStoreDefinition()
	updated.Contract.Goal = "new template"
	publishFlowStoreDefinition(t, s, updated, 1)
	again, err := s.Create(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	require.EqualValues(t, 1, again.DefinitionVersion)
	require.Equal(t, "answer", again.AcceptedVersion.Definition.Contract.Goal)
	req.TaskID = "task-new-key"
	req.Request.IdempotencyKey = "new-key"
	_, err = s.Create(t.Context(), req)
	require.ErrorIs(t, err, flow.ErrDefinitionNotCurrent)
}

func TestFlow_ConcurrentAdvanceSingleAttempt(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-claim", "key-claim"))
	require.NoError(t, err)
	const concurrent = 8
	errorsSeen := make(chan error, concurrent)
	var group sync.WaitGroup
	for i := range concurrent {
		group.Go(func() {
			_, err := s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Now: task.CreatedAt + 1, Attempt: flow.NodeAttempt{ID: fmt.Sprintf("attempt-%d", i), TaskID: task.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, TaskRevision: 0, ExecutionEpoch: 1, Input: task.Revision.Input, InputHash: task.Revision.InputHash, RunID: fmt.Sprintf("run-%d", i)}})
			errorsSeen <- err
		})
	}
	group.Wait()
	close(errorsSeen)
	won := 0
	for err := range errorsSeen {
		if err == nil {
			won++
		} else {
			require.ErrorIs(t, err, core.ErrPreconditionFailed)
		}
	}
	require.Equal(t, 1, won, "CAS must allow exactly one claimed attempt")
	var count int
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_node_attempts`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestFlow_InvalidCloseIntentCombination(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-check", "key-check"))
	require.NoError(t, err)
	for _, pair := range []struct {
		intent string
		cancel bool
	}{{"none", true}, {"deadline", true}, {"cancel", false}, {"abandon", false}} {
		_, err := s.db.pool.ExecContext(t.Context(), `UPDATE flow_tasks SET close_intent=$2,cancel_requested=$3 WHERE task_id=$1`, task.ID, pair.intent, pair.cancel)
		var pgError *pgconn.PgError
		require.ErrorAs(t, err, &pgError, "the real DB CHECK must reject the illegal pair")
		require.Equal(t, "23514", pgError.Code)
		require.Equal(t, "flow_close_intent_matches_cancel", pgError.ConstraintName)
	}
	_, err = s.db.pool.ExecContext(t.Context(), `UPDATE flow_tasks SET status='cancelling' WHERE task_id=$1`, task.ID)
	require.Error(t, err)
}

func TestFlow_CancelIntentNeverRequeues(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-cancel", "key-cancel"))
	require.NoError(t, err)
	_, err = s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Now: task.CreatedAt + 1, Attempt: flow.NodeAttempt{ID: "attempt-cancel", TaskID: task.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: 1, Input: task.Revision.Input, InputHash: task.Revision.InputHash}})
	require.NoError(t, err)
	task, err = s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	r := flow.CancelRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, OperationID: "cancel-operation", Expected: task.Version, Reason: "user cancelled"}
	closing, err := s.Cancel(t.Context(), flow.CancelTask{Request: r, Now: task.CreatedAt + 2})
	require.NoError(t, err)
	require.Equal(t, flow.Cancelling, closing.Status)
	_, err = s.Recover(t.Context(), flow.ReconcileTask{Request: flow.ResumeRequest{Principal: r.Principal, TaskID: task.ID, OperationID: "resume-operation", Reason: "check", Expected: closing.Version}, Status: flow.Queued, Joined: false, Now: task.CreatedAt + 3})
	require.ErrorIs(t, err, flow.ErrStateConflict, "recovery cannot reopen a task carrying cancellation")
	closed, err := s.Cancel(t.Context(), flow.CancelTask{Request: r, Joined: true, Now: task.CreatedAt + 4})
	require.NoError(t, err)
	require.Equal(t, flow.Cancelled, closed.Status)
	require.True(t, closed.CancelRequested)
	again, err := s.Cancel(t.Context(), flow.CancelTask{Request: r, Joined: true, Now: task.CreatedAt + 5})
	require.NoError(t, err)
	require.Equal(t, closed.Version, again.Version, "operation retry must not advance the head")
	_, err = s.Recover(context.Background(), flow.ReconcileTask{Request: flow.ResumeRequest{Principal: r.Principal, TaskID: task.ID, OperationID: "late-resume", Reason: "check", Expected: closed.Version}, Status: flow.Queued, Joined: true, Now: task.CreatedAt + 6})
	require.True(t, errors.Is(err, flow.ErrTaskClosed))
}

func TestFlow_CrossCallerMalformedCheckpointHidden(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-private", "private-key"))
	require.NoError(t, err)
	_, err = s.db.pool.ExecContext(t.Context(), `UPDATE flow_task_revisions SET checkpoint=jsonb_set(checkpoint,'{input_hash}','"wrong"') WHERE task_id=$1`, task.ID)
	require.NoError(t, err)
	other := core.Principal{Kind: core.PrincipalCaller, CallerID: "c1", KeyID: "key-c1", Scopes: []string{"flows:read", "flows:write"}, Actor: core.ActorCaller}
	require.True(t, other.Valid())
	_, err = s.Read(t.Context(), other, task.ID)
	require.ErrorIs(t, err, flow.ErrNotFound, "foreign invalid data must remain indistinguishable from absent data")
	_, err = s.Claim(t.Context(), flow.ClaimNode{Principal: other, TaskID: task.ID, Expected: task.Version, Attempt: flow.NodeAttempt{ID: "foreign-attempt", TaskID: task.ID, NodeInvocationID: "invocation", Input: json.RawMessage(`{}`), InputHash: flowDataHash([]byte(`{}`))}})
	require.ErrorIs(t, err, flow.ErrNotFound)
	_, err = s.ReadForWorker(t.Context(), task.ID)
	require.ErrorIs(t, err, flow.ErrCheckpointUnsupported, "internal owner integrity check must actually detect the fault")
}

func TestFlow_TaskContractHashMismatchBlocksWorker(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-contract", "contract-key"))
	require.NoError(t, err)
	changed, err := s.db.pool.ExecContext(t.Context(), `UPDATE flow_task_contracts SET content=replace(content,'"goal":"answer"','"goal":"changed"') WHERE task_id=$1`, task.ID)
	require.NoError(t, err)
	n, err := changed.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = s.ReadForWorker(t.Context(), task.ID)
	require.ErrorIs(t, err, flow.ErrCheckpointUnsupported)
}

func TestFlow_CheckpointRequiredKeysEnforcedByPG(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-format", "format-key"))
	require.NoError(t, err)
	for _, key := range []string{"format_version", "task_id", "revision"} {
		_, err := s.db.pool.ExecContext(t.Context(), `UPDATE flow_task_revisions SET checkpoint=checkpoint-$2 WHERE task_id=$1`, task.ID, key)
		var pgError *pgconn.PgError
		require.ErrorAs(t, err, &pgError, "missing required JSON key must fail the actual CHECK")
		require.Equal(t, "23514", pgError.Code)
	}
}

func claimFlowForTest(t *testing.T, s *FlowStore, task flow.Task) flow.Task {
	t.Helper()
	_, err := s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Now: task.CreatedAt + 1, Attempt: flow.NodeAttempt{ID: "attempt-" + task.ID, TaskID: task.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: 1, Input: task.Revision.Input, InputHash: task.Revision.InputHash}})
	require.NoError(t, err)
	claimed, err := s.ReadForWorker(t.Context(), task.ID)
	require.NoError(t, err)
	return claimed
}

func flowCommitForTest(task flow.Task) flow.CommitNode {
	output := json.RawMessage(`{"answer":"draft"}`)
	return flow.CommitNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, AttemptID: "attempt-" + task.ID, OperationID: "commit-" + task.ID, Expected: task.Version, Revision: task.Revision, Outcome: flow.Blocked, Reason: "needs evidence", AttemptState: "succeeded", Output: output, OutputHash: flowDataHash(output), Now: task.CreatedAt + 2, Artifacts: []flow.ArtifactVersion{{TaskID: task.ID, ID: "report", Version: 1, TaskRevision: 0, Format: "json", Content: output, ContentHash: flowDataHash(output), State: "proposed"}}}
}

func TestFlow_ResultAndTransitionCommitAtomically(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-atomic", "key-atomic"))
	require.NoError(t, err)
	task = claimFlowForTest(t, s, task)
	_, err = s.db.pool.ExecContext(t.Context(), `CREATE FUNCTION flow_test_fail_transition() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected final transition failure'; END $$; CREATE TRIGGER flow_test_fault BEFORE INSERT ON flow_transitions FOR EACH ROW WHEN (NEW.phase='commit') EXECUTE FUNCTION flow_test_fail_transition()`)
	require.NoError(t, err)
	_, err = s.Commit(t.Context(), flowCommitForTest(task))
	require.ErrorContains(t, err, "injected final transition failure", "the late fault must fire after result inserts")
	var artifacts int
	var state string
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_artifacts WHERE task_id=$1`, task.ID).Scan(&artifacts))
	require.Zero(t, artifacts, "late failure must roll back outputs, evidence and transition together")
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT state FROM flow_node_attempts WHERE task_id=$1`, task.ID).Scan(&state))
	require.Equal(t, "running", state)
	_, err = s.db.pool.ExecContext(t.Context(), `DROP TRIGGER flow_test_fault ON flow_transitions`)
	require.NoError(t, err)
	accepted, err := s.Commit(t.Context(), flowCommitForTest(task))
	require.NoError(t, err)
	require.Equal(t, flow.Blocked, accepted.Status)
	graph, err := s.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
	require.NoError(t, err)
	require.Len(t, graph.Artifacts, 1, "positive control must persist the result")
}

func TestFlow_StaleRevisionCannotCommit(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-stale", "key-stale"))
	require.NoError(t, err)
	task = claimFlowForTest(t, s, task)
	closing, err := s.Cancel(t.Context(), flow.CancelTask{Request: flow.CancelRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, OperationID: "cancel-stale"}, Now: task.CreatedAt + 2})
	require.NoError(t, err)
	require.Equal(t, flow.Cancelling, closing.Status)
	_, err = s.Commit(t.Context(), flowCommitForTest(task))
	require.ErrorIs(t, err, core.ErrPreconditionFailed)
	var artifacts int
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_artifacts WHERE task_id=$1`, task.ID).Scan(&artifacts))
	require.Zero(t, artifacts)
}

func TestFlow_RootCancelPreventsChildClaim(t *testing.T) {
	s, def := newFlowStoreTest(t)
	root, err := s.Create(t.Context(), flowCreateRequest(def, "task-root-close", "key-root-close"))
	require.NoError(t, err)
	childReq := flowCreateRequest(def, "task-child-close", "key-child-close")
	childReq.RootTaskID, childReq.ParentTaskID, childReq.Depth, childReq.Deadline = root.ID, root.ID, 1, root.Deadline
	child, err := s.Create(t.Context(), childReq)
	require.NoError(t, err)
	root, err = s.ReadForWorker(t.Context(), root.ID)
	require.NoError(t, err)
	_, err = s.Cancel(t.Context(), flow.CancelTask{Request: flow.CancelRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: root.ID, Expected: root.Version, OperationID: "cancel-root-tree"}, Joined: true, Now: root.CreatedAt + 2})
	require.NoError(t, err)
	child, err = s.ReadForWorker(t.Context(), child.ID)
	require.NoError(t, err)
	require.True(t, child.CancelRequested, "root close must persist the child's close intent")
	_, err = s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: child.ID, Expected: child.Version, Now: root.CreatedAt + 3, Attempt: flow.NodeAttempt{ID: "late-child", TaskID: child.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: child.Version.ExecutionEpoch, Input: child.Revision.Input, InputHash: child.Revision.InputHash}})
	require.Error(t, err, "closed root must never permit a child dispatch")
}

func TestFlow_UnknownDefinitionVersionNotFound(t *testing.T) {
	s, def := newFlowStoreTest(t)
	_, err := s.ResolveCurrent(t.Context(), def.DefinitionUID, 999)
	require.ErrorIs(t, err, flow.ErrDefinitionVersionNotFound)
}

func TestFlow_RunLedgerRequiresTheClaimedNodeIdentity(t *testing.T) {
	s, def := newFlowStoreTest(t)
	task, err := s.Create(t.Context(), flowCreateRequest(def, "task-ledger-link", "key-ledger-link"))
	require.NoError(t, err)
	_, err = s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Now: task.CreatedAt + 1, Attempt: flow.NodeAttempt{ID: "ledger-attempt", TaskID: task.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: 1, RunID: "planned-flow-run", SessionID: "planned-session", Input: task.Revision.Input, InputHash: task.Revision.InputHash}})
	require.NoError(t, err)
	for _, bad := range []string{"node-attempt", "task", "run", "session", "revision"} {
		record := core.RunRecord{RunID: "planned-flow-run", SessionID: "planned-session", Agent: "writer", Channel: "cli", UserID: "probe", Principal: core.LocalAdminCLIPrincipal(), StartedAt: time.Now(), Flow: &core.FlowRunContext{TaskID: task.ID, RootTaskID: task.ID, NodeAttemptID: "ledger-attempt", TaskRevision: 0}}
		switch bad {
		case "node-attempt":
			record.Flow.NodeAttemptID = "other"
		case "task":
			record.Flow.TaskID = "other"
		case "run":
			record.RunID = "other"
		case "session":
			record.SessionID = "other"
		case "revision":
			record.Flow.TaskRevision = 1
		}
		require.ErrorIs(t, NewRunStore(s.db).StartRun(t.Context(), record, core.RunResourceManifest{}), core.ErrPreconditionFailed, "mismatched Flow link must insert zero Runs")
	}
	var count int
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, NewRunStore(s.db).StartRun(t.Context(), core.RunRecord{RunID: "planned-flow-run", SessionID: "planned-session", Agent: "writer", Channel: "cli", UserID: "probe", Principal: core.LocalAdminCLIPrincipal(), StartedAt: time.Now(), Flow: &core.FlowRunContext{TaskID: task.ID, RootTaskID: task.ID, NodeAttemptID: "ledger-attempt"}}, core.RunResourceManifest{}))
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM runs WHERE flow_task_id=$1 AND flow_node_attempt_id='ledger-attempt'`, task.ID).Scan(&count))
	require.Equal(t, 1, count)
}

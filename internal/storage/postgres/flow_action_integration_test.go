//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/tool"
)

type pgFlowProbeTool struct {
	target string
	client *http.Client
}

func (p *pgFlowProbeTool) Name() string        { return "flow_probe" }
func (p *pgFlowProbeTool) Description() string { return "loopback-only read probe" }
func (p *pgFlowProbeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (p *pgFlowProbeTool) Execute(ctx context.Context, _ core.ToolEnv, _ json.RawMessage) (core.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.target, nil)
	if err != nil {
		return core.Result{}, err
	}
	response, err := p.client.Do(req)
	if err != nil {
		return core.Result{}, err
	}
	defer response.Body.Close()
	return core.Result{Content: `{"answer":"observed"}`}, nil
}

type pgFlowProbeResolver struct{ tool core.Tool }

func (r pgFlowProbeResolver) Resolve(name string) (core.Tool, bool) {
	return r.tool, name == r.tool.Name()
}
func (r pgFlowProbeResolver) Specs([]string) []core.ToolSpec { return nil }

type pgFlowActionProbe struct {
	store          *FlowStore
	task           flow.Task
	actions        *FlowActions
	budget         *FlowBudget
	executor       *core.ToolExecutor
	execution      core.FlowToolExecution
	env            core.ToolEnv
	call           core.ToolCall
	received       atomic.Int64
	boundAtReceive atomic.Bool
}

func newPGFlowActionProbe(t *testing.T) *pgFlowActionProbe {
	t.Helper()
	s, def := newFlowStoreTest(t)
	createReq := flowCreateRequest(def, "task-action", "key-action")
	createReq.Contract.Scope = core.FlowAuthority{CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin, Tools: []string{"flow_probe"}, Scopes: []string{core.ScopeFlowsWrite}, AgentUIDs: []string{"ag_one"}}
	created, err := s.Create(t.Context(), createReq)
	require.NoError(t, err)
	runID := "flow-probe-run"
	_, err = s.Claim(t.Context(), flow.ClaimNode{Principal: core.LocalAdminCLIPrincipal(), TaskID: created.ID, Expected: created.Version, Now: created.CreatedAt + 1, Attempt: flow.NodeAttempt{ID: "node-action", TaskID: created.ID, NodeID: "draft", NodeInvocationID: "draft-invocation", Attempt: 1, ExecutionEpoch: 1, RunID: runID, SessionID: "", Input: created.Revision.Input, InputHash: created.Revision.InputHash}})
	require.NoError(t, err)
	task, err := s.ReadForWorker(t.Context(), created.ID)
	require.NoError(t, err)
	// The original RunStore atomically validates and writes the planned Flow link.
	// This executor probe remains separate from full production AgentService wiring.
	require.NoError(t, NewRunStore(s.db).StartRun(t.Context(), core.RunRecord{RunID: runID, Agent: "writer", Channel: "cli", UserID: "test", Principal: core.LocalAdminCLIPrincipal(), StartedAt: time.Now(), Flow: &core.FlowRunContext{TaskID: task.ID, RootTaskID: task.RootTaskID, NodeAttemptID: "node-action", TaskRevision: task.Version.Revision}}, core.RunResourceManifest{}))
	p := &pgFlowActionProbe{store: s, task: task}
	p.actions = &FlowActions{Store: s, Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, NodeAttemptID: "node-action", Recheck: func(context.Context) error { return nil }}
	p.budget = &FlowBudget{Store: s, Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.received.Add(1)
		var bound bool
		err := s.db.pool.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM flow_action_attempts a JOIN tool_invocations i ON i.id=a.tool_invocation_id WHERE a.task_id=$1 AND a.outcome='dispatching' AND i.status='pending' AND a.run_id=i.run_id)`, task.ID).Scan(&bound)
		p.boundAtReceive.Store(err == nil && bound)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	probeTool := &pgFlowProbeTool{target: server.URL, client: server.Client()}
	p.executor = core.NewToolExecutor(pgFlowProbeResolver{probeTool}, NewToolInvocationRepo(s.db), nil)
	p.env = core.ToolEnv{RunID: runID, Principal: core.LocalAdminCLIPrincipal(), AgentUID: "ag_one", RunBound: true, Tools: map[string]core.Tool{probeTool.Name(): probeTool}}
	p.call = core.ToolCall{ID: "call-action", Name: probeTool.Name(), Arguments: `{}`}
	compiler := tool.NewSchemaCompiler()
	schemaHash, err := compiler.Compile(t.Context(), probeTool.InputSchema())
	require.NoError(t, err)
	p.execution = core.FlowToolExecution{Flow: &core.FlowRunContext{TaskID: task.ID, RootTaskID: task.RootTaskID, NodeInvocationID: "draft-invocation", NodeAttemptID: "node-action", TaskRevision: task.Version.Revision, AgentUID: "ag_one", Authority: core.FlowAuthority{CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin, Tools: []string{probeTool.Name()}, Scopes: []string{core.ScopeFlowsWrite}}, Actions: p.actions, Budget: p.budget}, Request: core.ActionRequest{TaskID: task.ID, RootTaskID: task.RootTaskID, NodeInvocationID: "draft-invocation", LogicalSlot: "slot-one", ToolName: probeTool.Name(), AdapterVersion: "1", SchemaHash: schemaHash, RecoveryClass: "readonly", Input: json.RawMessage(`{}`), InputHash: flowDataHash([]byte(`{}`)), RunID: runID, TaskRevision: task.Version.Revision, ExecutionEpoch: task.Version.ExecutionEpoch}, OriginalSchemaHash: flowDataHash(probeTool.InputSchema()), Validator: compiler, ReadOnlyOnly: true}
	return p
}

func (p *pgFlowActionProbe) run(ctx context.Context) (core.Result, core.ActionOutcome, error) {
	return p.executor.ExecuteFlowStep(ctx, p.env, []string{p.call.Name}, p.call, core.ToolStep{StepID: "1"}, p.execution)
}

func installFlowFault(t *testing.T, s *FlowStore, table, event, condition string) {
	t.Helper()
	query := `CREATE SEQUENCE flow_probe_fault_seq; CREATE FUNCTION flow_probe_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM nextval('flow_probe_fault_seq'); RAISE EXCEPTION 'injected Flow prewrite fault'; END $$; CREATE TRIGGER flow_probe_fault_trigger BEFORE ` + event + ` ON ` + table + ` FOR EACH ROW ` + condition + ` EXECUTE FUNCTION flow_probe_fault()`
	_, err := s.db.pool.ExecContext(t.Context(), query)
	require.NoError(t, err)
}

func TestFlow_PrewriteFailureNoCall(t *testing.T) {
	for _, phase := range []string{"action", "budget", "audit", "permit"} {
		t.Run(phase, func(t *testing.T) {
			p := newPGFlowActionProbe(t)
			switch phase {
			case "action":
				installFlowFault(t, p.store, "flow_actions", "INSERT", "")
			case "budget":
				installFlowFault(t, p.store, "flow_budget_allocations", "INSERT", "")
			case "audit":
				installFlowFault(t, p.store, "tool_invocations", "INSERT", "")
			case "permit":
				installFlowFault(t, p.store, "flow_action_attempts", "UPDATE", `WHEN (NEW.outcome='dispatching')`)
			}
			_, out, err := p.run(t.Context())
			require.Error(t, err)
			require.False(t, out.Dispatched)
			require.Zero(t, p.received.Load(), "real receiver must see zero calls on every prewrite failure")
			var fired bool
			require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT is_called FROM flow_probe_fault_seq`).Scan(&fired))
			require.True(t, fired, "nontransactional fault marker must prove the intended window fired")
		})
	}
}

func TestFlow_ActionAuditBoundBeforeRealDispatch(t *testing.T) {
	p := newPGFlowActionProbe(t)
	_, out, err := p.run(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 1, p.received.Load())
	require.True(t, p.boundAtReceive.Load(), "receiver must observe persistent action+original pending audit binding")
	require.True(t, out.Dispatched && out.AuditConfirmed)
	var actionStatus, budgetStatus string
	var toolCalls int64
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT outcome FROM flow_actions WHERE task_id=$1`, p.task.ID).Scan(&actionStatus))
	require.Equal(t, "succeeded", actionStatus)
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT state,actual_tool_calls FROM flow_budget_allocations WHERE task_id=$1`, p.task.ID).Scan(&budgetStatus, &toolCalls))
	require.Equal(t, "settled", budgetStatus)
	require.EqualValues(t, 1, toolCalls)
	_, _, err = p.run(t.Context())
	require.ErrorIs(t, err, flow.ErrActionUnknown)
	require.EqualValues(t, 1, p.received.Load(), "same successful logical action must not dispatch twice")
}

func TestFlow_UnknownActionAndBudgetSurviveClose(t *testing.T) {
	p := newPGFlowActionProbe(t)
	installFlowFault(t, p.store, "tool_invocations", "UPDATE", `WHEN (NEW.status<>'pending')`)
	_, out, err := p.run(t.Context())
	require.NoError(t, err)
	require.True(t, out.Dispatched)
	require.False(t, out.AuditConfirmed)
	require.EqualValues(t, 1, p.received.Load())
	var actionStatus, budgetStatus string
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT outcome FROM flow_actions WHERE task_id=$1`, p.task.ID).Scan(&actionStatus))
	require.Equal(t, "unknown", actionStatus)
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT state FROM flow_budget_allocations WHERE task_id=$1`, p.task.ID).Scan(&budgetStatus))
	require.Equal(t, "unknown", budgetStatus)
	closed, err := p.store.Cancel(t.Context(), flow.CancelTask{Request: flow.CancelRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: p.task.ID, Expected: p.task.Version, OperationID: "close-action"}, Joined: true, Now: time.Now().UnixMilli()})
	require.NoError(t, err)
	require.Equal(t, flow.Cancelled, closed.Status)
	p.actions.Store = NewFlowStore(p.store.db) // Discard the store wrapper's memory.
	_, _, err = p.run(t.Context())
	require.Error(t, err)
	require.EqualValues(t, 1, p.received.Load())
	unknown, err := p.store.HasUnknownActions(t.Context(), p.task.ID)
	require.NoError(t, err)
	require.True(t, unknown)
	require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT state FROM flow_budget_allocations WHERE task_id=$1`, p.task.ID).Scan(&budgetStatus))
	require.Equal(t, "unknown", budgetStatus, "closing a task must not free uncertain reservation")
	// This is real PG terminal-write injection, not the separately required kill window.
}

func TestFlow_AncestorBudgetOverageBlocksAllDimensions(t *testing.T) {
	p := newPGFlowActionProbe(t)
	ticket, err := p.budget.Reserve(t.Context(), p.env.RunID, "overage", core.UsageEstimate{ToolCalls: 1})
	require.NoError(t, err)
	require.NoError(t, p.budget.Settle(t.Context(), ticket, core.UsageActual{ToolCalls: 11, Known: true}))
	_, err = p.budget.Reserve(t.Context(), p.env.RunID, "tokens-after-tool-overage", core.UsageEstimate{Tokens: 1})
	require.ErrorIs(t, err, flow.ErrBudget, "known tool overage must block new token allocations too")
}

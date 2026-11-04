package core_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

const flowProbeSchema = `{"type":"object","properties":{"account":{"type":"object","properties":{"amount":{"type":"integer","minimum":1}},"required":["amount"],"additionalProperties":false}},"required":["account"],"additionalProperties":false}`

type flowLedgerProbe struct {
	prepare                       func(core.ActionRequest) error
	before                        func(core.ActionTicket, int64) error
	record                        func(core.ActionOutcome) error
	bound                         atomic.Int64
	prepared, permitted, recorded int
}

func (p *flowLedgerProbe) Prepare(_ context.Context, req core.ActionRequest) (core.ActionTicket, error) {
	p.prepared++
	if p.prepare != nil {
		if err := p.prepare(req); err != nil {
			return core.ActionTicket{}, err
		}
	}
	return core.ActionTicket{ActionID: "action-a", AttemptID: "attempt-a", TaskID: req.TaskID, RunID: req.RunID, InputHash: req.InputHash, TaskRevision: req.TaskRevision, ExecutionEpoch: req.ExecutionEpoch}, nil
}

func (p *flowLedgerProbe) BeforeDispatch(_ context.Context, ticket core.ActionTicket, id int64) error {
	p.permitted++
	if p.before != nil {
		if err := p.before(ticket, id); err != nil {
			return err
		}
	}
	p.bound.Store(id)
	return nil
}

func (p *flowLedgerProbe) RecordOutcome(_ context.Context, _ core.ActionTicket, out core.ActionOutcome) error {
	p.recorded++
	if p.record != nil {
		return p.record(out)
	}
	return nil
}

type flowExecutorProbe struct {
	tool           *execTool
	audit          *execAudit
	ledger         *flowLedgerProbe
	executor       *core.ToolExecutor
	env            core.ToolEnv
	call           core.ToolCall
	execution      core.FlowToolExecution
	received       atomic.Int64
	boundAtReceive atomic.Int64
}

type flowBudgetProbe struct {
	reserveErr error
	settled    []core.UsageActual
	unknown    int
}

func (b *flowBudgetProbe) Reserve(_ context.Context, runID, actionID string, estimate core.UsageEstimate) (core.BudgetTicket, error) {
	return core.BudgetTicket{ID: "budget-probe", RunID: runID, ActionID: actionID, Reserved: estimate}, b.reserveErr
}

func (b *flowBudgetProbe) Settle(_ context.Context, _ core.BudgetTicket, actual core.UsageActual) error {
	b.settled = append(b.settled, actual)
	return nil
}

func (b *flowBudgetProbe) KeepUnknown(context.Context, core.BudgetTicket) error {
	b.unknown++
	return nil
}

func newFlowExecutorProbe(t *testing.T) *flowExecutorProbe {
	t.Helper()
	p := &flowExecutorProbe{audit: &execAudit{}, ledger: &flowLedgerProbe{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.received.Add(1)
		p.boundAtReceive.Store(p.ledger.bound.Load())
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	p.tool = &execTool{name: "probe_read", schema: json.RawMessage(flowProbeSchema), exec: func(ctx context.Context, _ core.ToolEnv, _ json.RawMessage) (core.Result, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if err != nil {
			return core.Result{}, err
		}
		response, err := server.Client().Do(req)
		if err != nil {
			return core.Result{}, err
		}
		defer response.Body.Close()
		return core.Result{Content: "received"}, nil
	}}
	p.executor = core.NewToolExecutor(newExecPool(p.tool), p.audit, nil)
	p.env = core.ToolEnv{Principal: core.LocalAdminPrincipal(), AgentUID: "ag_probe", RunBound: true, RunID: "flow-probe-run", SessionID: "flow-probe-session", Tools: map[string]core.Tool{p.tool.Name(): p.tool}}
	p.call = core.ToolCall{ID: "call-a", Name: p.tool.Name(), Arguments: `{"account":{"amount":1}}`}
	compiler := tool.NewSchemaCompiler()
	schemaHash, err := compiler.Compile(context.Background(), json.RawMessage(flowProbeSchema))
	require.NoError(t, err)
	p.execution = core.FlowToolExecution{
		Flow:               &core.FlowRunContext{TaskID: "task-a", RootTaskID: "task-a", NodeInvocationID: "node-a", TaskRevision: 0, AgentUID: "ag_probe", Authority: core.FlowAuthority{CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin, Scopes: []string{core.ScopeFlowsWrite}, Tools: []string{p.tool.Name()}}, Actions: p.ledger, Budget: &flowBudgetProbe{}},
		Request:            core.ActionRequest{TaskID: "task-a", RootTaskID: "task-a", NodeInvocationID: "node-a", LogicalSlot: "slot-a", ToolName: p.tool.Name(), AdapterVersion: "1", RecoveryClass: "readonly", RunID: p.env.RunID, TaskRevision: 0, ExecutionEpoch: 1, Input: json.RawMessage(p.call.Arguments), InputHash: probeHash([]byte(p.call.Arguments)), SchemaHash: schemaHash},
		OriginalSchemaHash: probeHash(p.tool.schema), Validator: compiler, ReadOnlyOnly: true,
	}
	return p
}

func (p *flowExecutorProbe) run() (core.Result, core.ActionOutcome, error) {
	return p.executor.ExecuteFlowStep(context.Background(), p.env, []string{p.tool.Name()}, p.call, core.ToolStep{StepID: "1"}, p.execution)
}

func probeHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func TestFlow_ExecutorPrewriteFailureNoDispatch(t *testing.T) {
	for _, failure := range []string{"action", "pending", "permit"} {
		t.Run(failure, func(t *testing.T) {
			p := newFlowExecutorProbe(t)
			fired := false
			injected := errors.New("injected prewrite failure")
			switch failure {
			case "action":
				p.ledger.prepare = func(core.ActionRequest) error { fired = true; return injected }
			case "pending":
				p.audit.failStart = injected
				p.ledger.record = func(core.ActionOutcome) error { fired = true; return nil }
			case "permit":
				p.ledger.before = func(core.ActionTicket, int64) error { fired = true; return injected }
			}
			_, out, err := p.run()
			require.Error(t, err)
			require.True(t, fired, "fault must actually fire")
			require.False(t, out.Dispatched)
			require.Zero(t, p.received.Load(), "prewrite failure must issue zero network requests")
			require.Zero(t, p.tool.executedCount())
		})
	}
}

func TestFlow_ExecutorStrictPinAndSchema(t *testing.T) {
	for _, failure := range []string{"pin", "nested", "readonly", "classification", "principal", "schema-unavailable", "input-hash"} {
		t.Run(failure, func(t *testing.T) {
			p := newFlowExecutorProbe(t)
			switch failure {
			case "pin":
				p.env.Tools = nil // Same tool remains in the global pool.
			case "nested":
				p.call.Arguments = `{"account":{"amount":"bad"}}`
				p.execution.Request.Input = json.RawMessage(p.call.Arguments)
				p.execution.Request.InputHash = probeHash([]byte(p.call.Arguments))
			case "readonly":
				p.execution.Request.RecoveryClass = "unverifiable_write"
			case "classification":
				p.execution.Request.RecoveryClass = "author_says_readonly"
			case "principal":
				p.env.Principal = core.Principal{}
			case "schema-unavailable":
				p.execution.Request.SchemaHash = "missing"
			case "input-hash":
				p.execution.Request.InputHash = "wrong"
			}
			_, out, err := p.run()
			require.Error(t, err)
			require.False(t, out.Dispatched)
			require.Zero(t, p.received.Load())
			require.Zero(t, p.audit.count(), "strict validation must reject before audit/dispatch")
			require.Zero(t, p.ledger.prepared)
		})
	}
	p := newFlowExecutorProbe(t)
	result, out, err := p.run()
	require.NoError(t, err)
	require.Equal(t, "received", result.Content)
	require.EqualValues(t, 1, p.received.Load(), "positive control must reach the real receiver")
	require.Equal(t, out.AuditID, p.boundAtReceive.Load(), "original pending audit must be bound before dispatch")
	require.Positive(t, out.AuditID)
	require.True(t, out.Dispatched && out.AuditConfirmed)
	require.Equal(t, "success", out.Outcome)
	require.Equal(t, 1, p.audit.count())
}

func TestFlow_ExecutorInputAndSchemaFrozen(t *testing.T) {
	for _, mutation := range []string{"input", "schema"} {
		t.Run(mutation, func(t *testing.T) {
			p := newFlowExecutorProbe(t)
			fired := false
			if mutation == "input" {
				p.ledger.prepare = func(req core.ActionRequest) error { fired = true; req.Input[len(req.Input)-3] = '9'; return nil }
			} else {
				p.ledger.before = func(core.ActionTicket, int64) error {
					fired = true
					p.tool.schema = json.RawMessage(`{"type":"object"}`)
					return nil
				}
			}
			_, out, err := p.run()
			require.True(t, fired)
			require.Error(t, err, "mutated dispatch input/schema must fail closed")
			require.False(t, out.Dispatched)
			require.Zero(t, p.received.Load())
		})
	}
}

func TestFlow_ExecutorOutcomeRetainsAuditUncertainty(t *testing.T) {
	p := newFlowExecutorProbe(t)
	p.audit.failFinish = errors.New("injected terminal write failure")
	_, out, err := p.run()
	require.NoError(t, err)
	require.EqualValues(t, 1, p.received.Load())
	require.True(t, out.Dispatched)
	require.False(t, out.AuditConfirmed)
	require.Equal(t, "unknown", out.Outcome, "textual success must not erase uncertain audit")
	require.Equal(t, core.LLMCallPending, p.audit.row(0).status)
}

func TestFlow_SharedToolGroupCountsBeforeMessageFailure(t *testing.T) {
	h := newLoopHarness(t)
	h.store.failToolAppendOnce = true
	h.provider.onCall(0, toolCallResponse("issued-a", "read_file", `{"path":"a.txt"}`))
	result, err := h.service.Run(ctx, h.req("read"))
	require.NoError(t, err)
	require.Equal(t, core.StatusFailed, result.Status)
	require.Equal(t, 1, result.ToolCalls, "message failure must not erase the issued call")
	require.Equal(t, 1, h.store.countByRole(t, h.sessionID, core.RoleTool))
	require.Empty(t, h.store.missingResults(t, h.sessionID))
}

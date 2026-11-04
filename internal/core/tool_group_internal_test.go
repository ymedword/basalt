package core

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type groupProbeTool struct {
	calls int
	input json.RawMessage
}

func (p *groupProbeTool) Name() string                 { return "group_probe" }
func (p *groupProbeTool) Description() string          { return "isolated group probe" }
func (p *groupProbeTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (p *groupProbeTool) Execute(_ context.Context, _ ToolEnv, input json.RawMessage) (Result, error) {
	p.calls++
	p.input = append(json.RawMessage(nil), input...)
	return Result{Content: "observed"}, nil
}

type groupFlowLedger struct {
	fail     bool
	prepared int
	bound    int64
}

func (l *groupFlowLedger) Prepare(_ context.Context, r ActionRequest) (ActionTicket, error) {
	l.prepared++
	if l.fail {
		return ActionTicket{}, errors.New("injected action preparation")
	}
	return ActionTicket{ActionID: "action", AttemptID: "attempt", TaskID: r.TaskID, RunID: r.RunID, InputHash: r.InputHash, TaskRevision: r.TaskRevision, ExecutionEpoch: r.ExecutionEpoch}, nil
}

func (l *groupFlowLedger) BeforeDispatch(_ context.Context, _ ActionTicket, id int64) error {
	l.bound = id
	return nil
}

func (l *groupFlowLedger) RecordOutcome(context.Context, ActionTicket, ActionOutcome) error {
	return nil
}

type groupFlowBudget struct{}

func (groupFlowBudget) Reserve(_ context.Context, run, action string, estimate UsageEstimate) (BudgetTicket, error) {
	return BudgetTicket{ID: "ticket", RunID: run, ActionID: action, Reserved: estimate}, nil
}
func (groupFlowBudget) Settle(context.Context, BudgetTicket, UsageActual) error { return nil }
func (groupFlowBudget) KeepUnknown(context.Context, BudgetTicket) error         { return nil }

type groupFlowSchema struct{}

func (groupFlowSchema) Validate(context.Context, string, json.RawMessage) error { return nil }

type groupFlowBinder struct {
	tool     *groupProbeTool
	class    string
	override json.RawMessage
}

func (b groupFlowBinder) Bind(ctx context.Context, env ToolEnv, call ToolCall) (FlowToolExecution, error) {
	input, err := CanonicalFlowJSON(ctx, []byte(call.Arguments))
	if err != nil {
		return FlowToolExecution{}, err
	}
	if b.override != nil {
		input = b.override
	}
	return FlowToolExecution{Flow: env.Flow, Request: ActionRequest{RootTaskID: env.Flow.RootTaskID, TaskID: env.Flow.TaskID, NodeInvocationID: env.Flow.NodeInvocationID, LogicalSlot: call.ID, ToolName: call.Name, AdapterVersion: "1", SchemaHash: "compiled", InputHash: flowBytesHash(input), RecoveryClass: b.class, Input: input, RunID: env.RunID, TaskRevision: env.Flow.TaskRevision, ExecutionEpoch: 1}, OriginalSchemaHash: flowBytesHash(b.tool.InputSchema()), Validator: groupFlowSchema{}, ReadOnlyOnly: false}, nil
}

func TestFlow_SharedGroupPrewriteFailureCompletesMessagesWithoutDispatch(t *testing.T) {
	for _, scenario := range []string{"positive", "action-failure", "agent-write", "binder-replaces-input", "equivalent-numbers"} {
		t.Run(scenario, func(t *testing.T) {
			probe := &groupProbeTool{}
			audit := &groupProbeAudit{}
			ledger := &groupFlowLedger{fail: scenario == "action-failure"}
			loop := NewReActLoop(nil, NewToolExecutor(groupProbeResolver{probe}, audit, nil), nil)
			class := "readonly"
			if scenario == "agent-write" {
				class = "unverifiable_write"
			}
			flow := &FlowRunContext{TaskID: "task", RootTaskID: "task", NodeInvocationID: "node", AgentUID: "ag-probe", Actions: ledger, Budget: groupFlowBudget{}, Tools: groupFlowBinder{tool: probe, class: class}, Authority: FlowAuthority{CallerID: LocalAdminCallerID, AuthorizingKind: AuthorizingKindAdminBuiltin, Scopes: []string{ScopeFlowsWrite}, Tools: []string{probe.Name()}}}
			if scenario == "binder-replaces-input" {
				flow.Tools = groupFlowBinder{tool: probe, class: class, override: json.RawMessage(`{"amount":2}`)}
			}
			env := &RunEnv{Req: RunRequest{Principal: LocalAdminCLIPrincipal(), Flow: flow}, Flow: flow, AgentUID: "ag-probe", Session: NewSession("", nil, nil), Budget: Budget{MaxToolCalls: 2}, ToolSet: []string{probe.Name()}, pinnedTools: map[string]Tool{probe.Name(): probe}}
			calls := []ToolCall{{ID: "call-one", Name: probe.Name(), Arguments: `{}`}, {ID: "call-two", Name: probe.Name(), Arguments: `{}`}}
			if scenario == "positive" || scenario == "equivalent-numbers" {
				calls = calls[:1]
			}
			if scenario == "binder-replaces-input" {
				calls[0].Arguments = `{"amount":1}`
			}
			if scenario == "equivalent-numbers" {
				calls[0].Arguments = `{ "amount" : 1.00 }`
			}
			require.NoError(t, env.Session.AppendMessage(t.Context(), "run-group", Message{Role: RoleUser, Content: "request"}))
			require.NoError(t, loop.appendAssistantGroup(t.Context(), "run-group", env, ChatResponse{ToolCalls: calls}, nil, assistantFromModel))
			count, stopped := loop.runToolGroup(t.Context(), "run-group", env, calls, 1, 0)
			messages := env.Session.Messages()
			if scenario == "positive" || scenario == "equivalent-numbers" {
				require.Nil(t, stopped)
				require.Equal(t, 1, count)
				require.Equal(t, 1, probe.calls)
				require.Positive(t, ledger.bound)
				require.Len(t, messages, 3)
				if scenario == "equivalent-numbers" {
					require.Equal(t, `{"amount":1}`, string(probe.input))
					require.Equal(t, `{ "amount" : 1.00 }`, messages[1].ToolCalls[0].Arguments)
				}
			} else {
				require.NotNil(t, stopped)
				require.Equal(t, StatusFailed, stopped.Status)
				require.Zero(t, count)
				require.Zero(t, probe.calls)
				require.Len(t, messages, 4)
				require.Equal(t, "call-one", messages[2].ToolCallID)
				require.Equal(t, "call-two", messages[3].ToolCallID)
				if scenario == "action-failure" {
					require.Equal(t, 1, ledger.prepared, "the requested action fault must actually fire")
				} else {
					require.Zero(t, ledger.prepared, "Agent mode must override a binder attempting to allow writes")
				}
			}
		})
	}
}

type groupProbeResolver struct{ tool *groupProbeTool }

func (r groupProbeResolver) Resolve(name string) (Tool, bool) { return r.tool, name == r.tool.Name() }
func (r groupProbeResolver) Specs([]string) []ToolSpec        { return nil }

type groupProbeAudit struct{ begins, finishes int }

func (a *groupProbeAudit) BeginInvocation(context.Context, ToolInvocation) (int64, error) {
	a.begins++
	return int64(a.begins), nil
}

func (a *groupProbeAudit) FinishInvocation(context.Context, int64, ToolInvocationResult) error {
	a.finishes++
	return nil
}

func TestFlow_SharedHelperFixedAssistantGroup(t *testing.T) {
	ctx := t.Context()
	tool := &groupProbeTool{}
	audit := &groupProbeAudit{}
	loop := NewReActLoop(nil, NewToolExecutor(groupProbeResolver{tool}, audit, nil), nil)
	env := &RunEnv{Req: RunRequest{Principal: LocalAdminCLIPrincipal()}, Session: NewSession("", nil, nil), Budget: Budget{MaxToolCalls: 1}, ToolSet: []string{tool.Name()}}
	require.NoError(t, env.Session.AppendMessage(ctx, "probe-run", Message{Role: RoleUser, Content: "accepted fixed input"}))
	response := ChatResponse{ToolCalls: []ToolCall{{ID: "fixed-call", Name: tool.Name(), Arguments: `{}`}}}
	require.NoError(t, loop.appendAssistantGroup(ctx, "probe-run", env, response, nil, assistantFromFixedTool))
	count, stopped := loop.runToolGroup(ctx, "probe-run", env, response.ToolCalls, 0, 0)
	require.Nil(t, stopped)
	require.Equal(t, 1, count)
	require.Equal(t, 1, tool.calls)
	require.Equal(t, 1, audit.begins)
	require.Equal(t, 1, audit.finishes)
	messages := env.Session.Messages()
	require.Len(t, messages, 3)
	require.Equal(t, RoleUser, messages[0].Role)
	require.Equal(t, RoleAssistant, messages[1].Role)
	require.Equal(t, response.ToolCalls, messages[1].ToolCalls)
	require.Contains(t, messages[1].Content, "未调用模型")
	require.Empty(t, messages[1].ReasoningContent)
	require.Nil(t, messages[1].ProviderExtra)
	require.Equal(t, RoleTool, messages[2].Role)
	require.Equal(t, "fixed-call", messages[2].ToolCallID)
	require.Equal(t, "observed", messages[2].Content)
	// This is a helper probe, not a FixedTool Run/RunQuota or PG acceptance.
	bad := response
	bad.ProviderExtra = map[string]any{"provider": "fabricated"}
	require.Error(t, loop.appendAssistantGroup(ctx, "probe-run", env, bad, nil, assistantFromFixedTool))
	require.Len(t, env.Session.Messages(), 3)
}

func TestFlow_FixedToolClientSelectionHasNoModelDependency(t *testing.T) {
	client, err := pinnedClientWhenNeeded(ResolvedResources{}, false)
	require.NoError(t, err)
	require.Nil(t, client, "fixed execution must not fabricate a model client")
	_, err = pinnedClientWhenNeeded(ResolvedResources{}, true)
	require.Error(t, err, "ordinary model execution must keep strict pinned mapping refusal")
}

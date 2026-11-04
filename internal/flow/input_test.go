package flow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

type outputAuthorityProbe struct {
	failure error
	calls   int
}

func (p *outputAuthorityProbe) CheckOutput(context.Context, core.Principal, Task, NodeOutput) error {
	p.calls++
	return p.failure
}

func mappingTask() Task {
	input := json.RawMessage(`{"answer":"original"}`)
	output := json.RawMessage(`{"answer":"verified result"}`)
	return Task{ID: "task-map", CallerID: core.LocalAdminCallerID, Version: TaskVersion{Revision: 0, StateVersion: 2, ExecutionEpoch: 1}, Contract: validDefinition().Contract, AcceptedVersion: DefinitionVersion{Definition: validDefinition()}, Revision: TaskRevision{FormatVersion: 1, TaskID: "task-map", Input: input, InputHash: flowInputHash(input), Outputs: map[string]NodeOutput{"draft": {Value: output, Hash: flowInputHash(output), NodeAttemptID: "attempt-one", Verified: true}}, Slots: map[string]Slot{"facts": {ID: "facts", State: "missing"}}, Attempts: map[string]int{"draft": 1}}}
}

func TestFlow_InputMappingRequiresCurrentVerifiedOutput(t *testing.T) {
	for _, failure := range []string{"missing", "not-verified", "unbound-attempt", "hash", "future-revision", "no-current-check", "revoked"} {
		t.Run(failure, func(t *testing.T) {
			task := mappingTask()
			output := task.Revision.Outputs["draft"]
			checker := &outputAuthorityProbe{}
			var port OutputRevalidator = checker
			switch failure {
			case "missing":
				delete(task.Revision.Outputs, "draft")
			case "not-verified":
				output.Verified = false
				task.Revision.Outputs["draft"] = output
			case "unbound-attempt":
				output.NodeAttemptID = ""
				task.Revision.Outputs["draft"] = output
			case "hash":
				output.Hash = "wrong"
				task.Revision.Outputs["draft"] = output
			case "future-revision":
				output.Revision = 1
				task.Revision.Outputs["draft"] = output
			case "no-current-check":
				port = nil
			case "revoked":
				checker.failure = errors.New("source authority revoked")
			}
			_, _, err := ResolveNodeInput(t.Context(), core.LocalAdminCLIPrincipal(), TrustedState{Task: task}, Node{Inputs: []InputMapping{{Target: "value", Source: "outputs.draft.answer"}}}, port)
			require.Error(t, err, "unverified/stale output must not become tool parameters")
		})
	}
	task := mappingTask()
	checker := &outputAuthorityProbe{}
	input, hash, err := ResolveNodeInput(t.Context(), core.LocalAdminCLIPrincipal(), TrustedState{Task: task}, Node{Inputs: []InputMapping{{Target: "value", Source: "outputs.draft.answer"}, {Target: "request", Source: "input.answer"}, {Target: "limit", Literal: json.RawMessage(`2`)}}}, checker)
	require.NoError(t, err)
	require.JSONEq(t, `{"value":"verified result","request":"original","limit":2}`, string(input))
	require.Equal(t, flowInputHash(input), hash)
	require.Equal(t, 1, checker.calls, "positive mapping must actually recheck current source authority")
	task.Revision.Input[0] = 'x'
	require.Equal(t, flowInputHash(input), hash, "returned parameters must be independent frozen bytes")
}

func TestFlow_BranchUsesProgramStateAndExactNumbers(t *testing.T) {
	state := TrustedState{Task: mappingTask(), Remaining: core.UsageEstimate{Tokens: 9007199254740993}}
	branch := Branch{Field: "budget.remaining_tokens", Op: "gt", Value: json.RawMessage(`9007199254740992`), Then: "enough", Else: "exhausted"}
	next, err := SelectBranch(t.Context(), core.LocalAdminCLIPrincipal(), state, branch, nil)
	require.NoError(t, err)
	require.Equal(t, "enough", next, "numbers above float64 exact range must compare correctly")
	branch = Branch{Field: "slots.facts.state", Op: "eq", Value: json.RawMessage(`"verified"`), Then: "done", Else: "collect"}
	next, err = SelectBranch(t.Context(), core.LocalAdminCLIPrincipal(), state, branch, nil)
	require.NoError(t, err)
	require.Equal(t, "collect", next, "a model's output wording must not promote the trusted slot")
	state.Task.Revision.Slots["facts"] = Slot{ID: "facts", State: "verified"}
	_, err = SelectBranch(t.Context(), core.LocalAdminCLIPrincipal(), state, branch, nil)
	require.Error(t, err, "verified without a verification binding must fail closed")
}

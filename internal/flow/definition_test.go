package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/tool"
)

func validDefinition() Definition {
	schema := json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`)
	cap := BudgetCap{MaxTotalTokens: 1000, MaxToolCalls: 10, MaxDurationSeconds: 60, MaxDepth: 3, MaxChildren: 16, MaxAttemptsPerNode: 3, MaxRevisions: 2}
	return Definition{
		FormatVersion: 1, Name: "report", Entry: "draft", TaskContract: "TASK.json", Limits: cap,
		Nodes:    []Node{{ID: "draft", Kind: "agent", Agent: &AgentRef{UID: "ag_pinned", Name: "writer", Version: 1, ConfigHash: "config", ManifestHash: "manifest"}, OutputSchema: schema, Failure: FailurePolicy{MaxAttempts: 1}}},
		Contract: TaskContract{ContractVersion: 1, Goal: "answer the question", InputSchema: schema, OutputSchema: schema, Budget: cap, Constraints: []string{"preserve citations"}, StopConditions: []string{"budget", "deadline", "unknown"}, Escalation: "show unresolved", ArtifactSpec: []ArtifactSpec{{ID: "report", Format: "json", Required: true}}, Acceptance: []AcceptanceRule{{ID: "shape", Version: 1, Kind: "deterministic", TargetKind: "artifact", TargetID: "report", Required: true, Schema: schema}}},
	}
}

type publishedDefinitionProbe struct {
	definitionResolverStub
	versions   map[DefinitionRef]DefinitionVersion
	calibrated bool
}

func (r publishedDefinitionProbe) ReadDefinition(_ context.Context, ref DefinitionRef) (DefinitionVersion, error) {
	value, ok := r.versions[ref]
	if !ok {
		return DefinitionVersion{}, ErrDefinitionVersionNotFound
	}
	return value, nil
}

func (r publishedDefinitionProbe) CheckCalibration(_ context.Context, validator, calibration string) error {
	if r.calibrated && validator == "trusted-judge" && calibration == "fixed-calibration" {
		return nil
	}
	return errors.New("calibration not registered")
}

func TestFlow_SubflowBoundsIncludeParentReworkMultipliers(t *testing.T) {
	parent := validDefinition()
	parent.Name = "parent"
	parent.Nodes[0].Kind = "subflow"
	parent.Nodes[0].Agent = nil
	parent.Nodes[0].Subflow = &DefinitionRef{UID: "fd_child", Version: 1}
	parent.Nodes[0].Failure = FailurePolicy{MaxAttempts: 3, MaxReworks: 2, ReworkTarget: "draft"}
	child := validDefinition()
	child.Name = "child"
	child.Nodes = nil
	for i := range 64 {
		node := validDefinition().Nodes[0]
		node.ID = fmt.Sprintf("node%d", i)
		if i < 63 {
			node.Next = fmt.Sprintf("node%d", i+1)
		}
		child.Nodes = append(child.Nodes, node)
	}
	child.Entry = "node0"
	resolver := publishedDefinitionProbe{versions: map[DefinitionRef]DefinitionVersion{{UID: "fd_child", Version: 1}: {DefinitionUID: "fd_child", Version: 1, Definition: child}}}
	validator := &ContractValidator{Schemas: tool.NewSchemaCompiler(), Resolver: resolver}
	require.Error(t, validator.ValidateDefinition(t.Context(), parent), "parent attempts/reworks multiply child steps beyond 256")
	parent.Nodes[0].Failure = FailurePolicy{MaxAttempts: 1}
	require.NoError(t, validator.ValidateDefinition(t.Context(), parent), "the same 64-step child has a bounded positive control")
}

func TestFlow_TrustedJudgeCalibrationAndSlotMinimum(t *testing.T) {
	def := validDefinition()
	def.Contract.Acceptance[0].Kind = "judgment"
	def.Contract.Acceptance[0].ValidatorID = "trusted-judge"
	def.Contract.Acceptance[0].CalibrationID = "fixed-calibration"
	validator := definitionValidator()
	require.Error(t, validator.ValidateDefinition(t.Context(), def), "author strings cannot register a scoring oracle")
	validator.Resolver = publishedDefinitionProbe{calibrated: true}
	require.NoError(t, validator.ValidateDefinition(t.Context(), def))
	def = validDefinition()
	def.Contract.RequiredSlots = []RequiredSlot{{ID: "facts", MinSources: 2, Validators: []string{"source-rule"}}}
	def.Contract.Acceptance = append(def.Contract.Acceptance, AcceptanceRule{ID: "source-rule", Version: 1, Kind: "evidence", TargetKind: "slot", TargetID: "facts", Required: true, MinSources: 1})
	require.Error(t, validator.ValidateDefinition(t.Context(), def), "an evidence rule cannot weaken the required slot count")
	def.Contract.Acceptance[1].MinSources = 2
	require.NoError(t, validator.ValidateDefinition(t.Context(), def))
}

func TestFlow_InputReferencesRequireGuaranteedUpstreamNode(t *testing.T) {
	def := validDefinition()
	def.Nodes[0].Inputs = []InputMapping{{Target: "value", Source: "outputs.draft.answer"}}
	require.Error(t, definitionValidator().ValidateDefinition(t.Context(), def), "self output cannot supply its own first invocation")
	def = validDefinition()
	later := def.Nodes[0]
	later.ID = "later"
	def.Nodes[0].Next = "later"
	def.Nodes = append(def.Nodes, later)
	def.Nodes[0].Inputs = []InputMapping{{Target: "value", Source: "outputs.later.answer"}}
	require.Error(t, definitionValidator().ValidateDefinition(t.Context(), def), "future output must not be read")
	def.Nodes[0].Inputs = nil
	def.Nodes[1].Inputs = []InputMapping{{Target: "value", Source: "outputs.draft.answer"}}
	require.NoError(t, definitionValidator().ValidateDefinition(t.Context(), def))
}

func TestFlow_ContractCannotExceedPublishedLimits(t *testing.T) {
	def := validDefinition()
	def.Contract.Budget.MaxTotalTokens++
	require.Error(t, definitionValidator().ValidateDefinition(t.Context(), def))
	def = validDefinition()
	def.Contract.Budget.MaxToolCalls--
	require.NoError(t, definitionValidator().ValidateDefinition(t.Context(), def), "author may tighten the task ceiling")
}

func definitionValidator() *ContractValidator {
	return &ContractValidator{Schemas: tool.NewSchemaCompiler(), Resolver: definitionResolverStub{}}
}

func TestFlow_RequiredClaimHasGuaranteedLeafProducer(t *testing.T) {
	def := validDefinition()
	def.Contract.Acceptance = append(def.Contract.Acceptance, AcceptanceRule{ID: "claim-shape", Version: 1, Kind: "deterministic", TargetKind: "claim", TargetID: "draft", Required: true, Schema: def.Nodes[0].OutputSchema})
	require.NoError(t, definitionValidator().ValidateDefinition(t.Context(), def))
	def.Contract.Acceptance[1].TargetID = "missing"
	require.Error(t, definitionValidator().ValidateDefinition(t.Context(), def))
	def.Contract.Acceptance[1].TargetID = "draft"
	def.Entry = "choose"
	def.Nodes = append([]Node{{ID: "choose", Kind: "branch", Branch: &Branch{Field: "budget.remaining_tokens", Op: "gt", Value: json.RawMessage(`0`), Then: "draft", Else: ""}, Failure: FailurePolicy{MaxAttempts: 1}, OutputSchema: def.Nodes[0].OutputSchema}}, def.Nodes...)
	require.ErrorContains(t, definitionValidator().ValidateDefinition(t.Context(), def), "every successful route")
	def.Nodes[0].Branch.Else = "draft"
	require.NoError(t, definitionValidator().ValidateDefinition(t.Context(), def))
	def.Contract.Acceptance[1].TargetID = "choose"
	require.ErrorContains(t, definitionValidator().ValidateDefinition(t.Context(), def), "leaf producer")
}

type definitionResolverStub struct{}

func (definitionResolverStub) CheckAgent(context.Context, AgentRef, string, bool) error { return nil }

func (definitionResolverStub) ReadDefinition(context.Context, DefinitionRef) (DefinitionVersion, error) {
	return DefinitionVersion{Definition: validDefinition()}, nil
}

func TestFlow_DefinitionRejectsUnsupported(t *testing.T) {
	for _, kind := range []string{"parallel", "fanout", "execute_code", ""} {
		def := validDefinition()
		def.Nodes[0].Kind = kind
		require.Error(t, definitionValidator().ValidateDefinition(context.Background(), def), "unsupported node must be rejected")
	}
	def := validDefinition()
	def.Nodes[0].Kind, def.Nodes[0].Tool = "tool", "write_file"
	require.Error(t, definitionValidator().ValidateDefinition(context.Background(), def), "write gate must block tool publication before G-FT")
	require.NoError(t, definitionValidator().ValidateDefinition(context.Background(), validDefinition()))
}

func TestFlow_DefinitionRejectsUnboundedRoute(t *testing.T) {
	def := validDefinition()
	def.Nodes[0].Next = "draft"
	require.Error(t, definitionValidator().ValidateDefinition(context.Background(), def), "unbounded success cycle must be rejected")
	def = validDefinition()
	def.Nodes[0].Failure = FailurePolicy{MaxAttempts: 3, ReworkTarget: "draft", MaxReworks: 0}
	require.Error(t, definitionValidator().ValidateDefinition(context.Background(), def), "rework needs a positive bound")
	def.Nodes[0].Failure.MaxReworks = 1
	require.NoError(t, definitionValidator().ValidateDefinition(context.Background(), def), "bounded explicit rework is permitted")
}

func TestFlow_RequiredSlotMustHaveValidator(t *testing.T) {
	def := validDefinition()
	def.Contract.RequiredSlots = []RequiredSlot{{ID: "facts", MinSources: 1}}
	require.Error(t, definitionValidator().ValidateDefinition(context.Background(), def), "required slot must have an applicable rule")
	def.Contract.RequiredSlots[0].Validators = []string{"shape"}
	require.Error(t, definitionValidator().ValidateDefinition(context.Background(), def), "artifact rule cannot verify a slot")
	def.Contract.Acceptance = append(def.Contract.Acceptance, AcceptanceRule{ID: "facts", Version: 1, Kind: "judgment", TargetKind: "slot", TargetID: "facts", Required: true})
	def.Contract.RequiredSlots[0].Validators = []string{"facts"}
	require.Error(t, definitionValidator().ValidateDefinition(context.Background(), def), "uncalibrated judgment must be rejected")
	def.Contract.Acceptance[1].HumanRequired = true
	require.NoError(t, definitionValidator().ValidateDefinition(context.Background(), def), "explicit human requirement may publish but cannot auto-pass")
}

func TestFlow_BranchReadsTrustedSlotState(t *testing.T) {
	def := validDefinition()
	def.Nodes = append(def.Nodes, Node{ID: "branch", Kind: "branch", OutputSchema: def.Nodes[0].OutputSchema, Branch: &Branch{Field: "model.says.verified", Op: "eq", Value: json.RawMessage(`true`), Then: "draft"}, Failure: FailurePolicy{MaxAttempts: 1}})
	def.Entry = "branch"
	require.Error(t, definitionValidator().ValidateDefinition(context.Background(), def), "model status strings must not control branches")
	def.Nodes[1].Branch.Field = "slots.facts.state"
	def.Contract.RequiredSlots = []RequiredSlot{{ID: "facts", Validators: []string{"facts"}, MinSources: 1}}
	def.Contract.Acceptance = append(def.Contract.Acceptance, AcceptanceRule{ID: "facts", Version: 1, Kind: "evidence", TargetKind: "slot", TargetID: "facts", Required: true, MinSources: 1})
	def.Nodes[1].Branch.Value = json.RawMessage(`"missing"`)
	require.NoError(t, definitionValidator().ValidateDefinition(context.Background(), def))
	def.Nodes[1].Branch.Field = "slots.unknown.state"
	require.Error(t, definitionValidator().ValidateDefinition(context.Background(), def), "unknown task slot must be rejected")
}

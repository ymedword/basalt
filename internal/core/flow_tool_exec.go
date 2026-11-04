package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
)

// ErrFlowDispatch rejects a Flow call before the sole tool dispatch point.
var ErrFlowDispatch = errors.New("flow dispatch rejected")

type preparedFlowCall struct {
	execution FlowToolExecution
	ticket    ActionTicket
	input     []byte
	tool      Tool
}

// ExecuteFlowStep is the strict Flow seam of the same executor. It does not append
// messages or resolve a missing pinned tool from the global registry. FixedTool
// service dispatch remains disabled until the G-FT prerequisite probes pass.
func (e *ToolExecutor) ExecuteFlowStep(ctx context.Context, env ToolEnv, toolSet []string, call ToolCall, step ToolStep, execution FlowToolExecution) (Result, ActionOutcome, error) {
	outcome := ActionOutcome{Outcome: "not_dispatched"}
	prepared := &preparedFlowCall{execution: execution, input: bytes.Clone(execution.Request.Input)}
	prepared.execution.Request.Input = bytes.Clone(prepared.input)
	tool, pinned := env.Tools[call.Name]
	if !pinned || tool == nil || !slices.Contains(toolSet, call.Name) || tool.Name() != call.Name || prepared.validate(ctx, env, call, tool) != nil {
		e.logger.Warn("Flow tool dispatch rejected", "run_id", env.RunID, "tool", call.Name, "reason", "binding_or_schema_invalid")
		return Result{Content: ErrFlowDispatch.Error()}, outcome, ErrFlowDispatch
	}
	prepared.tool = tool
	flow := execution.Flow
	if flow.Progress != nil {
		if err := flow.Progress.BeforeCall(ctx, call); err != nil {
			return Result{Content: "flow progress guard rejected"}, outcome, err
		}
	}
	ticket, err := flow.Actions.Prepare(ctx, prepared.execution.Request)
	if err != nil {
		return Result{Content: "flow action preparation failed"}, outcome, err
	}
	prepared.ticket = ticket
	req := prepared.execution.Request
	if ticket.ActionID == "" || ticket.AttemptID == "" || ticket.TaskID != req.TaskID || ticket.RunID != req.RunID || ticket.InputHash != req.InputHash || ticket.TaskRevision != req.TaskRevision || ticket.ExecutionEpoch != req.ExecutionEpoch {
		return Result{Content: ErrFlowDispatch.Error()}, outcome, ErrFlowDispatch
	}
	budget, err := flow.Budget.Reserve(ctx, env.RunID, ticket.ActionID, UsageEstimate{ToolCalls: 1})
	if err != nil {
		finishCtx, cancel := finishContext(ctx)
		defer cancel()
		return Result{Content: "flow ancestor budget rejected"}, outcome, errors.Join(err, flow.Actions.RecordOutcome(finishCtx, ticket, outcome))
	}
	result, outcome, dispatchErr := e.executeStep(ctx, env, toolSet, call, step, prepared)
	finishCtx, cancel := finishContext(ctx)
	defer cancel()
	var settleErr error
	if outcome.Dispatched && !outcome.AuditConfirmed {
		settleErr = flow.Budget.KeepUnknown(finishCtx, budget)
	} else {
		actual := UsageActual{Known: true}
		if outcome.Dispatched {
			actual.ToolCalls = 1
		}
		settleErr = flow.Budget.Settle(finishCtx, budget, actual)
	}
	dispatchErr = errors.Join(dispatchErr, settleErr)
	if err := flow.Actions.RecordOutcome(finishCtx, ticket, outcome); err != nil {
		e.logger.Warn("Flow action outcome remains unresolved", "run_id", env.RunID, "action_id", ticket.ActionID, "reason", "outcome_write_failed")
		dispatchErr = errors.Join(dispatchErr, err)
		return result, outcome, dispatchErr
	}
	if settleErr != nil {
		return result, outcome, dispatchErr
	}
	if flow.Progress != nil {
		dispatchErr = errors.Join(dispatchErr, flow.Progress.AfterCall(finishCtx, call, result, outcome))
	}
	return result, outcome, dispatchErr
}

func (p *preparedFlowCall) validate(ctx context.Context, env ToolEnv, call ToolCall, tool Tool) error {
	x, req := p.execution, p.execution.Request
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if x.Flow == nil || x.Flow.Actions == nil || x.Flow.Budget == nil || x.Validator == nil || !env.Principal.Valid() || !env.Principal.HasScope(ScopeFlowsWrite) || !env.RunBound || env.RunID == "" || req.RunID != env.RunID || req.TaskID != x.Flow.TaskID || req.RootTaskID != x.Flow.RootTaskID || req.NodeInvocationID != x.Flow.NodeInvocationID || req.TaskRevision != x.Flow.TaskRevision || req.TaskRevision < 0 || req.ExecutionEpoch < 1 || req.ToolName != call.Name || req.LogicalSlot == "" || req.AdapterVersion == "" || req.SchemaHash == "" || x.OriginalSchemaHash == "" || req.InputHash != flowBytesHash(p.input) || !bytes.Equal(p.input, []byte(call.Arguments)) || flowBytesHash(tool.InputSchema()) != x.OriginalSchemaHash {
		return ErrFlowDispatch
	}
	if x.Flow.Authority.CallerID != env.Principal.CallerID || x.Flow.Authority.KeyID != env.Principal.KeyID || x.Flow.AgentUID == "" || x.Flow.AgentUID != string(env.AgentUID) || !slices.Contains(x.Flow.Authority.Tools, call.Name) || !slices.Contains(x.Flow.Authority.Scopes, ScopeFlowsWrite) {
		return ErrFlowDispatch
	}
	kind := AuthorizingKindCallerKey
	if env.Principal.Kind == PrincipalLocalAdmin {
		kind = AuthorizingKindAdminBuiltin
	}
	if x.Flow.Authority.AuthorizingKind != kind {
		return ErrFlowDispatch
	}
	if req.RecoveryClass != "readonly" && req.RecoveryClass != "verifiable_write" && req.RecoveryClass != "unverifiable_write" {
		return ErrFlowDispatch
	}
	if x.ReadOnlyOnly && req.RecoveryClass != "readonly" {
		return ErrFlowDispatch
	}
	return x.Validator.Validate(ctx, req.SchemaHash, p.input)
}

func (p *preparedFlowCall) beforeDispatch(ctx context.Context, tool Tool, auditID int64) error {
	if auditID <= 0 || ctx.Err() != nil {
		return ErrFlowDispatch
	}
	if err := p.execution.Flow.Actions.BeforeDispatch(ctx, p.ticket, auditID); err != nil {
		return err
	}
	// Recheck after the persistent grant: callbacks cannot replace the frozen input
	// or change the schema of the admitted instance in the pending/dispatch window.
	if ctx.Err() != nil || !bytes.Equal(p.input, p.execution.Request.Input) || flowBytesHash(tool.InputSchema()) != p.execution.OriginalSchemaHash {
		return ErrFlowDispatch
	}
	return p.execution.Validator.Validate(ctx, p.execution.Request.SchemaHash, p.input)
}

func flowBytesHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

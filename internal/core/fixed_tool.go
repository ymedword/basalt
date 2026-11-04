package core

import (
	"context"
	"slices"
)

func validFixedFlowRequest(req RunRequest) bool {
	f := req.Flow
	if f == nil || f.TaskID == "" || f.RootTaskID == "" || f.NodeInvocationID == "" || f.NodeAttemptID == "" || f.AgentUID == "" || f.AgentConfigHash == "" || f.ResourceManifestHash == "" || req.RunID == "" || f.Actions == nil || f.Budget == nil || f.Tools == nil || f.Progress == nil || f.Authority.CallerID != req.Principal.CallerID || f.Authority.KeyID != req.Principal.KeyID || !req.Principal.HasScope(ScopeFlowsWrite) || !req.Principal.HasAgentScope(req.Agent) || !slices.Contains(f.Authority.Scopes, ScopeFlowsWrite) {
		return false
	}
	if req.FixedTool != nil {
		if req.FixedTool.ID == "" || req.FixedTool.Name == "" {
			return false
		}
	} else {
		if f.Models == nil {
			return false
		}
		if _, ok := f.Tools.(FlowReadOnlyToolFilter); !ok {
			return false
		}
	}
	kind := AuthorizingKindCallerKey
	if req.Principal.Kind == PrincipalLocalAdmin {
		kind = AuthorizingKindAdminBuiltin
	}
	return f.Authority.AuthorizingKind == kind
}

// runFixedTool uses the original message group and executor. Production remains
// disabled until app enables the durable Run path and its separate write gate.
func (l *ReActLoop) runFixedTool(ctx context.Context, runID string, env *RunEnv, call ToolCall) (RunResult, error) {
	if err := ctx.Err(); err != nil {
		return env.cancelled(err, 0, 0), nil
	}
	response := ChatResponse{ToolCalls: []ToolCall{call}}
	if err := l.appendAssistantGroup(ctx, runID, env, response, nil, assistantFromFixedTool); err != nil {
		return env.fail(err, 0, 0), nil
	}
	count, stopped := l.runToolGroup(ctx, runID, env, response.ToolCalls, 0, 0)
	if stopped != nil {
		return *stopped, nil
	}
	if !env.lastFlowToolOutcome.Dispatched || !env.lastFlowToolOutcome.AuditConfirmed || env.lastFlowToolOutcome.Outcome != string(LLMCallSuccess) {
		return env.fail(ErrFlowDispatch, 0, count), nil
	}
	return env.complete(env.lastFlowToolResult.Content, 0, count), nil
}

package core

import (
	"bytes"
	"context"
	"errors"
	"strconv"
)

type assistantOrigin uint8

const (
	assistantFromModel assistantOrigin = iota
	assistantFromFixedTool
)

// appendAssistantGroup is the only assistant append seam for model and explicit
// fixed-call groups. The origin is trusted core control, never provider metadata.
func (l *ReActLoop) appendAssistantGroup(ctx context.Context, runID string, env *RunEnv, response ChatResponse, refs []EvidenceRef, origin assistantOrigin) error {
	if origin == assistantFromFixedTool {
		if len(response.ToolCalls) != 1 || response.ToolCalls[0].ID == "" || response.ReasoningContent != "" || len(response.ProviderExtra) != 0 {
			return errors.New("invalid fixed tool assistant group")
		}
		response.Content = "固定工具调用（由流程定义指定，未调用模型）"
	} else if origin != assistantFromModel {
		return errors.New("unknown assistant origin")
	}
	assistant := env.assistantMessage(response)
	assistant.EvidenceRefs = refs
	return env.Session.AppendMessage(ctx, runID, assistant)
}

// runToolGroup owns serial execution and message completion. Count the admitted
// tool attempt before persisting its message so an append failure cannot erase
// an issued call. Ordinary Run budget semantics (including refusals) stay intact.
func (l *ReActLoop) runToolGroup(ctx context.Context, runID string, env *RunEnv, calls []ToolCall, round, toolCalls int) (int, *RunResult) {
	stop := func(result RunResult, reason string) (int, *RunResult) {
		l.completeGroup(ctx, runID, env, calls, reason)
		return toolCalls, &result
	}
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return stop(env.cancelled(err, round, toolCalls), ReasonCancelled)
		}
		if env.Budget.ToolCallsExhausted(toolCalls) {
			return stop(env.limitReached(ReasonToolBudget, round, toolCalls), ReasonToolBudget)
		}
		var result Result
		var outcome ActionOutcome
		var flowErr error
		toolEnv := env.ToolEnv(ctx, runID)
		step := ToolStep{StepID: strconv.Itoa(round), Seq: int64(round)}
		if env.Flow != nil {
			if env.Flow.Tools == nil {
				flowErr = ErrFlowDispatch
			} else {
				var execution FlowToolExecution
				execution, flowErr = env.Flow.Tools.Bind(ctx, toolEnv, call)
				if flowErr == nil && execution.Flow != env.Flow {
					flowErr = ErrFlowDispatch
				}
				if flowErr == nil {
					original, err := CanonicalFlowJSON(ctx, []byte(call.Arguments))
					if err != nil || !bytes.Equal(original, execution.Request.Input) {
						flowErr = ErrFlowDispatch
					}
				}
				if flowErr == nil {
					// Agent calls cannot be promoted to fixed calls by the binder.
					execution.ReadOnlyOnly = env.Req.FixedTool == nil || !env.flowWritesReady
					call.Arguments = string(execution.Request.Input)
					result, outcome, flowErr = l.executor.ExecuteFlowStep(ctx, toolEnv, env.ToolSet, call, step, execution)
				}
			}
			if outcome.Dispatched {
				toolCalls++
			}
			env.lastFlowToolOutcome, env.lastFlowToolResult = outcome, result
			if flowErr != nil {
				result.Content = "流程工具调用未确认，已停止本次运行"
			}
		} else {
			result = l.executor.ExecuteStep(ctx, toolEnv, env.ToolSet, call, step)
			toolCalls++ // Preserve ordinary Run attempt/budget semantics.
		}
		if err := env.Session.AppendToolResult(ctx, runID, call, result); err != nil {
			return stop(env.fail(err, round, toolCalls), ReasonRunFailed)
		}
		if env.Flow != nil && (flowErr != nil || outcome.Outcome == string(LLMCallUnknown)) {
			if !outcome.Dispatched && errors.Is(flowErr, ErrFlowBudget) {
				return stop(env.limitReached("flow_budget_exhausted", round, toolCalls), "flow_budget_exhausted")
			}
			var limit *ProgressLimitError
			if errors.As(flowErr, &limit) {
				return stop(env.limitReached(limit.Reason, round, toolCalls), limit.Reason)
			}
			l.logger.Warn("Flow tool group stopped", "run_id", runID, "tool", call.Name, "reason", "dispatch_unresolved")
			return stop(env.fail(ErrFlowDispatch, round, toolCalls), ReasonRunFailed)
		}
	}
	if err := ctx.Err(); err != nil {
		return stop(env.cancelled(err, round, toolCalls), ReasonCancelled)
	}
	return toolCalls, nil
}

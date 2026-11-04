package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"sync"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/tool"
)

// flowToolBinder classifies each actual pinned instance and freezes one logical
// call position. Neither a model call ID nor a readonlyHint chooses action IDs.
type flowToolBinder struct {
	flow              *core.FlowRunContext
	task              flow.Task
	schemas           *tool.SchemaCompiler
	fixed, writeReady bool
	mu                sync.Mutex
	position          int
	last              *flowProgressCall
}

type flowProgressCall struct {
	env       core.ToolEnv
	pinned    core.Tool
	input     json.RawMessage
	signature string
	before    string
}

func (b *flowToolBinder) ReadOnlyTools(ctx context.Context, pinned map[string]core.Tool, declared []string) ([]string, error) {
	if b == nil || b.flow == nil || b.schemas == nil {
		return nil, core.ErrFlowDispatch
	}
	result := make([]string, 0, len(declared))
	for _, name := range declared {
		if !slices.Contains(b.flow.Authority.Tools, name) {
			continue
		}
		spec, err := tool.FlowAdapterFor(ctx, pinned[name], b.schemas)
		if err == nil && spec.Name == name && spec.RecoveryClass == "readonly" {
			result = append(result, name)
		}
	}
	return result, nil
}

func (b *flowToolBinder) Bind(ctx context.Context, env core.ToolEnv, call core.ToolCall) (core.FlowToolExecution, error) {
	if b == nil || b.flow == nil || env.Flow != b.flow || b.schemas == nil || !env.RunBound || env.Principal.CallerID != b.task.CallerID || env.Principal.KeyID != b.task.AuthorizingKeyID || !slices.Contains(b.flow.Authority.Tools, call.Name) {
		return core.FlowToolExecution{}, core.ErrFlowDispatch
	}
	pinned, ok := env.Tools[call.Name]
	if !ok {
		return core.FlowToolExecution{}, core.ErrFlowDispatch
	}
	spec, err := tool.FlowAdapterFor(ctx, pinned, b.schemas)
	if err != nil {
		return core.FlowToolExecution{}, err
	}
	if spec.Name != call.Name || spec.RecoveryClass != "readonly" && (!b.fixed || !b.writeReady) {
		return core.FlowToolExecution{}, core.ErrFlowDispatch
	}
	input, err := core.CanonicalFlowJSON(ctx, []byte(call.Arguments))
	if err != nil {
		return core.FlowToolExecution{}, err
	}
	if err := spec.Validator.Validate(ctx, spec.SchemaHash, input); err != nil {
		return core.FlowToolExecution{}, err
	}
	before, err := tool.PrepareFlowReadProgress(ctx, pinned, env, input)
	if err != nil {
		if ctx.Err() != nil {
			return core.FlowToolExecution{}, ctx.Err()
		}
		// 无进展证明不替代原工具的权限检查／失败审计；原唯一执行路径仍记录拒绝。
		// Missing proof preserves the original tool policy check and denial audit.
		before = ""
	}
	b.mu.Lock()
	b.position++
	slot := "call:" + strconv.Itoa(b.position)
	signature, err := core.ProgressSignature(ctx, call)
	if err != nil {
		b.mu.Unlock()
		return core.FlowToolExecution{}, err
	}
	b.last = &flowProgressCall{env: env, pinned: pinned, input: input, signature: signature, before: before}
	b.mu.Unlock()
	if b.fixed {
		slot = "fixed-tool"
	}
	sum := sha256.Sum256(input)
	return core.FlowToolExecution{Flow: b.flow, Request: core.ActionRequest{TaskID: b.task.ID, RootTaskID: b.task.RootTaskID, NodeInvocationID: b.flow.NodeInvocationID, LogicalSlot: slot, ToolName: call.Name, AdapterVersion: spec.Version, SchemaHash: spec.SchemaHash, InputHash: hex.EncodeToString(sum[:]), RecoveryClass: spec.RecoveryClass, Input: json.RawMessage(input), RunID: env.RunID, TaskRevision: b.task.Version.Revision, ExecutionEpoch: b.task.Version.ExecutionEpoch}, OriginalSchemaHash: spec.OriginalSchemaHash, Validator: spec.Validator, ReadOnlyOnly: !b.fixed || !b.writeReady}, nil
}

func (b *flowToolBinder) progressBasis(ctx context.Context, call core.ToolCall, result core.Result, out core.ActionOutcome) (string, error) {
	signature, err := core.ProgressSignature(ctx, call)
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	last := b.last
	b.mu.Unlock()
	if last == nil || last.signature != signature {
		return "", core.ErrFlowDispatch
	}
	return tool.FlowReadProgressBasis(ctx, last.pinned, last.env, last.input, last.before, result, out)
}

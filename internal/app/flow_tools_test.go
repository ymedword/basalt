package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/tool"
)

type flowImpersonatedTool struct{ core.Tool }

func (flowImpersonatedTool) Name() string        { return "read_file" }
func (flowImpersonatedTool) Description() string { return "pretends readonly" }
func (flowImpersonatedTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (flowImpersonatedTool) Execute(context.Context, core.ToolEnv, json.RawMessage) (core.Result, error) {
	panic("classification must never execute")
}

func TestFlow_ToolBinderUsesPinnedCodeOwnedAdapter(t *testing.T) {
	f := &core.FlowRunContext{TaskID: "task", RootTaskID: "task", NodeInvocationID: "task:draft", Authority: core.FlowAuthority{CallerID: core.LocalAdminCallerID, Tools: []string{"read_file", "write_file"}}}
	task := flow.Task{ID: "task", RootTaskID: "task", CallerID: core.LocalAdminCallerID, Version: flow.TaskVersion{ExecutionEpoch: 1}}
	binder := &flowToolBinder{flow: f, task: task, schemas: tool.NewSchemaCompiler(), writeReady: true}
	pinned := map[string]core.Tool{"read_file": tool.NewReadFileTool(nil, t.TempDir()), "write_file": tool.NewWriteFileTool(nil, t.TempDir(), "")}
	allowed, err := binder.ReadOnlyTools(t.Context(), pinned, []string{"read_file", "write_file"})
	require.NoError(t, err)
	require.Equal(t, []string{"read_file"}, allowed)
	env := core.ToolEnv{Flow: f, RunID: "run", Principal: core.LocalAdminCLIPrincipal(), RunBound: true, Tools: pinned}
	call := core.ToolCall{ID: "model-chosen-id", Name: "read_file", Arguments: `{"path":"notes.txt"}`}
	execution, err := binder.Bind(t.Context(), env, call)
	require.NoError(t, err)
	require.Equal(t, "call:1", execution.Request.LogicalSlot)
	require.Equal(t, "readonly", execution.Request.RecoveryClass)
	clone := &flowToolBinder{flow: f, task: task, schemas: binder.schemas}
	call.ID = "renamed-by-model"
	again, err := clone.Bind(t.Context(), env, call)
	require.NoError(t, err)
	require.Equal(t, execution.Request.LogicalSlot, again.Request.LogicalSlot)
	require.Equal(t, execution.Request.InputHash, again.Request.InputHash)
	call.Arguments = `{"path":"notes.txt","extra":true}`
	_, err = binder.Bind(t.Context(), env, call)
	require.Error(t, err)
	env.Tools = map[string]core.Tool{"read_file": flowImpersonatedTool{}}
	call.Arguments = `{}`
	_, err = binder.Bind(t.Context(), env, call)
	require.Error(t, err)
	allowed, err = binder.ReadOnlyTools(t.Context(), env.Tools, []string{"read_file"})
	require.NoError(t, err)
	require.Empty(t, allowed, "a name and hint do not grant readonly status")
	env.Tools = nil
	_, err = binder.Bind(t.Context(), env, call)
	require.Error(t, err, "no global pool fallback")
}

func TestFlow_WriteBinderRequiresFixedNodeAndBehaviorGate(t *testing.T) {
	f := &core.FlowRunContext{TaskID: "task", RootTaskID: "task", NodeInvocationID: "task:write", Authority: core.FlowAuthority{CallerID: core.LocalAdminCallerID, Tools: []string{"write_file"}}}
	task := flow.Task{ID: "task", RootTaskID: "task", CallerID: core.LocalAdminCallerID, Version: flow.TaskVersion{ExecutionEpoch: 1}}
	env := core.ToolEnv{Flow: f, RunID: "run", Principal: core.LocalAdminCLIPrincipal(), RunBound: true, Tools: map[string]core.Tool{"write_file": tool.NewWriteFileTool(nil, t.TempDir(), "")}}
	call := core.ToolCall{ID: "fixed", Name: "write_file", Arguments: `{"path":"notes.txt","content":"verified input"}`}
	for _, mode := range []struct{ fixed, ready bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		binder := &flowToolBinder{flow: f, task: task, schemas: tool.NewSchemaCompiler(), fixed: mode.fixed, writeReady: mode.ready}
		x, err := binder.Bind(t.Context(), env, call)
		if mode.fixed && mode.ready {
			require.NoError(t, err)
			require.Equal(t, "fixed-tool", x.Request.LogicalSlot)
			require.Equal(t, "verifiable_write", x.Request.RecoveryClass)
		} else {
			require.Error(t, err)
		}
	}
}

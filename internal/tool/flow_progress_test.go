package tool_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// 本组件测试只证明文件身份判据；真实执行与审计关联由 app 集成测试证明。
// This component test proves stable source identity, not executor/audit provenance.
func TestFlow_ReadProgressUsesStableSourceIdentity(t *testing.T) {
	f := newFileToolFixture(t, nil)
	env := f.toolEnv()
	env.RunBound = true
	env.Flow = &core.FlowRunContext{AgentUID: "original-agent"}
	out := core.ActionOutcome{Dispatched: true, AuditConfirmed: true, Outcome: string(core.LLMCallSuccess)}
	write := func(name, text string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(f.agentDir(), name), []byte(text), 0o600))
	}
	basis := func(name, text string) string {
		t.Helper()
		input, err := json.Marshal(map[string]string{"path": name})
		require.NoError(t, err)
		before, err := tool.PrepareFlowReadProgress(t.Context(), f.read, env, input)
		require.NoError(t, err)
		value, err := tool.FlowReadProgressBasis(t.Context(), f.read, env, input, before, core.Result{Content: text}, out)
		require.NoError(t, err)
		return value
	}
	write("a.txt", "first")
	first := basis("a.txt", "first")
	require.NotEmpty(t, first)
	write("a.txt", "changed bytes")
	require.Equal(t, first, basis("a.txt", "changed bytes"), "content changes cannot mint a new source")
	require.Empty(t, basis("a.txt", "first"), "changed source cannot prove the old observed result")
	require.NoError(t, os.Link(filepath.Join(f.agentDir(), "a.txt"), filepath.Join(f.agentDir(), "alias.txt")))
	alias := basis("alias.txt", "changed bytes")
	require.NotEqual(t, first, alias, "record the alias path without minting another physical source")
	require.Equal(t, strings.Split(first, ":")[2], strings.Split(alias, ":")[2])
	write("b.txt", "changed bytes")
	require.NotEqual(t, first, basis("b.txt", "changed bytes"))
	out.AuditConfirmed = false
	require.Empty(t, basis("b.txt", "changed bytes"))
	out.AuditConfirmed = true
	out.Outcome = "unknown"
	require.Empty(t, basis("b.txt", "changed bytes"))
}

func TestFlow_ReadProgressRejectsPostReadAtomicReplacement(t *testing.T) {
	f := newFileToolFixture(t, nil)
	env := f.toolEnv()
	env.RunBound = true
	env.Flow = &core.FlowRunContext{AgentUID: "original-agent"}
	input := json.RawMessage(`{"path":"a.txt"}`)
	require.NoError(t, os.WriteFile(filepath.Join(f.agentDir(), "a.txt"), []byte("same bytes"), 0o600))
	before, err := tool.PrepareFlowReadProgress(t.Context(), f.read, env, input)
	require.NoError(t, err)
	require.NotEmpty(t, before)
	require.NoError(t, os.WriteFile(filepath.Join(f.agentDir(), "replacement.txt"), []byte("same bytes"), 0o600))
	require.NoError(t, os.Rename(filepath.Join(f.agentDir(), "replacement.txt"), filepath.Join(f.agentDir(), "a.txt")))
	after, err := tool.PrepareFlowReadProgress(t.Context(), f.read, env, input)
	require.NoError(t, err)
	require.NotEqual(t, before, after, "fixture must actually replace the physical identity")
	proof, err := tool.FlowReadProgressBasis(t.Context(), f.read, env, input, before, core.Result{Content: "same bytes"}, core.ActionOutcome{Dispatched: true, AuditConfirmed: true, Outcome: string(core.LLMCallSuccess)})
	require.NoError(t, err)
	require.Empty(t, proof, "a new post-audit file cannot be attributed to the original read")
}

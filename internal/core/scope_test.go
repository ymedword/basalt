package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 验收点（T104）：范围只有两个合法字面量；资格 = 共享 ∪ 本人专属；
// 他人专属不可用；范围转换的占用证据可判定。
// Acceptance points (T104): exactly two scope literals; eligibility is shared plus
// the caller's own exclusive resources; other agents' exclusive resources are denied.

const (
	testAgentA AgentUID = "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	testAgentB AgentUID = "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"
)

func TestScope_Eligibility(t *testing.T) {
	t.Parallel()
	require.True(t, Eligible(ScopeShared, "", testAgentA))
	require.True(t, Eligible(ScopeShared, testAgentB, testAgentA))
	require.True(t, Eligible(ScopeExclusive, testAgentA, testAgentA))
	require.False(t, Eligible(ScopeExclusive, testAgentB, testAgentA))
	require.False(t, Eligible(ResourceScope(""), "", testAgentA), "非法范围 fail-closed")
}

func TestScope_ExclusiveOwnerOnly(t *testing.T) {
	t.Parallel()
	require.True(t, Eligible(ScopeExclusive, testAgentA, testAgentA))
	require.False(t, Eligible(ScopeExclusive, testAgentA, testAgentB))
	require.False(t, Eligible(ScopeExclusive, "", testAgentA), "专属资源必须有属主")

	usage := ScopeUsage{
		BoundAgents:   []AgentUID{testAgentB},
		ActiveRuns:    []string{"run-1"},
		MessageRoutes: []string{"route-1"},
	}
	require.True(t, usage.InUse())
	require.ElementsMatch(t,
		[]string{testAgentB.String(), "run-1", "route-1"}, usage.Blockers())
	require.False(t, ScopeUsage{}.InUse())
}

func TestScope_InvalidLiteral(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "global", "shared ", "Shared"} {
		_, err := ParseScope(raw)
		require.Error(t, err, raw)
	}
	scope, err := ParseScope("exclusive")
	require.NoError(t, err)
	require.Equal(t, ScopeExclusive, scope)
}

package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ymedword/basalt/internal/core"
)

// 逐键凭据状态、依赖轴、apply 结果与取代者都是契约闭集：只接受逐字取值。
// The per-key credential, dependency, apply-outcome and superseded-by sets are closed.
func TestMCPCredentialAndApply_ClosedSets(t *testing.T) {
	t.Parallel()

	for _, state := range []core.MCPCredentialKeyState{
		core.MCPCredentialManaged,
		core.MCPCredentialManagedRevoked,
		core.MCPCredentialEnv,
		core.MCPCredentialMissing,
	} {
		assert.True(t, state.Valid(), "%q 是契约键状态", state)
	}
	for _, raw := range []core.MCPCredentialKeyState{"", "revoked", "Managed"} {
		assert.False(t, raw.Valid(), "%q 不是契约键状态", raw)
	}

	for _, state := range []core.MCPDependencyState{
		core.MCPDependencyReady,
		core.MCPDependencyMissing,
		core.MCPDependencyVersionMismatch,
	} {
		assert.True(t, state.Valid(), "%q 是契约依赖轴取值", state)
	}
	// 自定义命令的依赖轴为 null，用空串表达"不适用"；它不是契约枚举值。
	assert.False(t, core.MCPDependencyState("").Valid())

	for _, outcome := range []core.MCPApplyOutcome{
		core.MCPApplyApplied,
		core.MCPApplySuperseded,
		core.MCPApplyFailed,
	} {
		assert.True(t, outcome.Valid(), "%q 是契约 apply 结果", outcome)
	}
	for _, raw := range []core.MCPApplyOutcome{"", "aborted"} {
		assert.False(t, raw.Valid(), "%q 不是契约 apply 结果", raw)
	}

	// 空串表示未被取代，是合法取值。
	for _, by := range []core.MCPApplySupersededBy{
		core.MCPApplySupersededByNone,
		core.MCPApplySupersededByPublish,
		core.MCPApplySupersededByRevoke,
		core.MCPApplySupersededByApply,
	} {
		assert.True(t, by.Valid(), "%q 是合法取代者", by)
	}
	assert.False(t, core.MCPApplySupersededBy("delete").Valid())

	assert.Equal(t, "drain_timeout", core.MCPApplyFailureDrainTimeout)
	assert.Equal(t, "mcp_server.apply", core.ActionMCPServerApply)
	assert.Equal(t, "mcp_server.credential.revoke", core.ActionMCPServerCredentialRevoke)
}

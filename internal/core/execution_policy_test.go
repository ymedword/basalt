package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ymedword/basalt/internal/core"
)

// 两档与四类细分原因都是契约闭集：只接受逐字取值，空串与大小写变体一律拒绝。
// Both closed sets accept their literals only.
func TestExecutionProfile_ClosedSet(t *testing.T) {
	t.Parallel()

	for _, profile := range []core.ExecutionProfile{core.ProfileStandard, core.ProfileStrict} {
		assert.True(t, profile.Valid(), "%q 是契约档位", profile)
	}
	for _, raw := range []core.ExecutionProfile{"", "Standard", "STRICT", "strict ", "off"} {
		assert.False(t, raw.Valid(), "%q 不是契约档位", raw)
	}

	for _, reason := range []core.PolicyDenialReason{
		core.DenialProfileDenied,
		core.DenialDeniedDomain,
		core.DenialProtectedAddress,
		core.DenialExceptionMissing,
	} {
		assert.True(t, reason.Valid(), "%q 是契约细分原因", reason)
	}
	// 空串表示"没有被拒"，不是合法原因。
	for _, raw := range []core.PolicyDenialReason{"", "sandbox_denied", "denied"} {
		assert.False(t, raw.Valid(), "%q 不是契约细分原因", raw)
	}

	for _, axis := range []core.PendingConfirmationAxis{
		core.PendingAxisNetworkAllowlist,
		core.PendingAxisShellCommand,
	} {
		assert.True(t, axis.Valid(), "%q 是契约待确认轴", axis)
	}
	for _, raw := range []core.PendingConfirmationAxis{"", "network_denylist", "shell"} {
		assert.False(t, raw.Valid(), "%q 不是契约待确认轴", raw)
	}

	for _, reason := range []core.PendingConfirmationReason{
		core.PendingReasonProtectedAddress,
		core.PendingReasonShellName,
	} {
		assert.True(t, reason.Valid(), "%q 是契约待确认原因", reason)
	}
	for _, raw := range []core.PendingConfirmationReason{"", "protected", "shell_command"} {
		assert.False(t, raw.Valid(), "%q 不是契约待确认原因", raw)
	}
}

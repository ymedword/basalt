package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ymedword/basalt/internal/core"
)

// 三态闭集与 `master_key` 固定字面量：`recovery_required` 必须能被独立识别，
// 不能被归并成"未初始化"或"就绪"。
// The three-state set is closed and master_key is the only record scope.
func TestSecretState_ClosedSet(t *testing.T) {
	t.Parallel()

	for _, state := range []core.SecretState{
		core.SecretStateUninitialized,
		core.SecretStateReady,
		core.SecretStateRecoveryRequired,
	} {
		assert.True(t, state.Valid(), "%q 是契约状态", state)
	}
	for _, raw := range []core.SecretState{"", "ready ", "recovery", "uninitialized2"} {
		assert.False(t, raw.Valid(), "%q 不是契约状态", raw)
	}

	// 空串 = 不需要恢复；五个细分原因各自独立，任何"模糊说法"一律拒绝。
	for _, reason := range []core.SecretRecoveryReason{
		core.SecretRecoveryNone,
		core.SecretRecoveryMissing,
		core.SecretRecoveryCorrupt,
		core.SecretRecoveryKeyIDMismatch,
		core.SecretRecoveryUnprovableProvenance,
		core.SecretRecoveryUnavailable,
	} {
		assert.True(t, reason.Valid(), "%q 是契约恢复原因", reason)
	}
	for _, raw := range []core.SecretRecoveryReason{"unknown", "missing_key", "unavailable "} {
		assert.False(t, raw.Valid(), "%q 不是契约恢复原因", raw)
	}

	assert.Equal(t, "master_key", core.InstanceSecretScopeMasterKey)
	assert.Equal(t, "master_key", core.InstanceTargetMasterKey)
	assert.Equal(t, "system.secrets_init", core.ActionSystemSecretsInit)
	assert.Equal(t, "system", core.TargetKindSystem)
}

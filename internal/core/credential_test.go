package core

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 验收点（T106）：模式只有 env / managed 两个字面量；身份与版本状态机逐条可判，
// **版本**的 `revoked` 是终态（身份可经重新录入回到 active，见 2026-10-23 判据裁决）；
// 目标绑定规范化不抹掉路径与查询串。
// Acceptance points (T106): exactly two credential modes; identity and version state
// machines are exhaustive with version revocation terminal; re-entry reactivates the
// identity only; target canonicalisation preserves path and query.

func TestCredential_ModeLiterals(t *testing.T) {
	t.Parallel()
	env, err := ParseCredentialMode("env")
	require.NoError(t, err)
	require.Equal(t, CredentialModeEnv, env)
	managed, err := ParseCredentialMode("managed")
	require.NoError(t, err)
	require.Equal(t, CredentialModeManaged, managed)

	for _, raw := range []string{"", "auto", "plaintext", "Managed", " managed"} {
		_, err := ParseCredentialMode(raw)
		require.ErrorIs(t, err, ErrCredentialModeRequired, raw)
	}
}

func TestCredential_VersionStateMachine(t *testing.T) {
	t.Parallel()
	identityCases := []struct {
		from CredentialIdentityState
		to   CredentialIdentityState
		want bool
	}{
		{CredentialIdentityActive, CredentialIdentityRevoked, true},
		{CredentialIdentityActive, CredentialIdentityRetired, true},
		{CredentialIdentityRetired, CredentialIdentityRevoked, true},
		{CredentialIdentityRetired, CredentialIdentityActive, true},
		{CredentialIdentityRevoked, CredentialIdentityActive, true},
		{CredentialIdentityRevoked, CredentialIdentityRetired, false},
	}
	for _, tc := range identityCases {
		require.Equal(t, tc.want, tc.from.CanTransitionTo(tc.to), "%s → %s", tc.from, tc.to)
	}

	versionCases := []struct {
		from CredentialVersionState
		to   CredentialVersionState
		want bool
	}{
		{CredentialVersionActive, CredentialVersionSuperseded, true},
		{CredentialVersionActive, CredentialVersionRevoked, true},
		{CredentialVersionActive, CredentialVersionRetired, true},
		{CredentialVersionSuperseded, CredentialVersionRevoked, true},
		{CredentialVersionSuperseded, CredentialVersionRetired, true},
		{CredentialVersionSuperseded, CredentialVersionActive, false},
		{CredentialVersionRetired, CredentialVersionRevoked, true},
		{CredentialVersionRetired, CredentialVersionActive, false},
		{CredentialVersionRevoked, CredentialVersionActive, false},
		{CredentialVersionRevoked, CredentialVersionRetired, false},
	}
	for _, tc := range versionCases {
		require.Equal(t, tc.want, tc.from.CanTransitionTo(tc.to), "%s → %s", tc.from, tc.to)
	}
}

// TestCredential_RevocationIsTerminal 固定 2026-10-23 判据裁决后的矩阵：身份的撤销
// **不是**终态（重新录入把身份迁回 active），版本的撤销是终态——即便身份已重新录入，
// 被撤销的旧版本也不能迁回 active 或任何其他状态。
// TestCredential_RevocationIsTerminal pins the ruled matrix: re-entry reactivates the
// identity, while revoked versions stay permanently unusable.
func TestCredential_RevocationIsTerminal(t *testing.T) {
	t.Parallel()

	// 身份：撤销后只能经重新录入回到 active；不得绕过录入直接转 retired。
	require.True(t, CredentialIdentityRevoked.CanTransitionTo(CredentialIdentityActive))
	require.False(t, CredentialIdentityRevoked.CanTransitionTo(CredentialIdentityRetired))
	// 退役同样可经环境 → 托管重新录入复用身份，但不得自转 retired。
	require.True(t, CredentialIdentityRetired.CanTransitionTo(CredentialIdentityActive))
	// active 到 active 不是迁移：已经在用，不需要也不允许「重新激活」自己。
	require.False(t, CredentialIdentityActive.CanTransitionTo(CredentialIdentityActive))

	// 版本：任何版本都不能迁回 active（重新录入追加新版本，绝不复活旧版本）；
	// 被撤销的版本对任何后继状态都不可迁移——这是比身份更强的终态。
	for _, version := range []CredentialVersionState{
		CredentialVersionActive, CredentialVersionSuperseded,
		CredentialVersionRetired, CredentialVersionRevoked,
	} {
		require.False(t, version.CanTransitionTo(CredentialVersionActive), version)
	}
	for _, next := range []CredentialVersionState{
		CredentialVersionActive, CredentialVersionSuperseded,
		CredentialVersionRetired, CredentialVersionRevoked,
	} {
		require.False(t, CredentialVersionRevoked.CanTransitionTo(next), next)
	}

	require.NoError(t, ValidateCredentialID("cred_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"))
	for _, raw := range []string{"", "cred_", "cred_01J8Z6YQ4M2S3T7V9W0X1Y2Z3", "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", "cred_01J8Z6YQ4M2S3T7V9W0X1Y2Z3I"} {
		require.Error(t, ValidateCredentialID(raw), raw)
	}

	connection := TargetBinding{Kind: BindingModelConnection, BaseURL: "HTTPS://API.Example.COM:443/v1/"}
	require.Equal(t, "https://api.example.com/v1/", connection.Canonical())
	channel := TargetBinding{Kind: BindingNotifyChannel, FullURL: "https://hooks.example.com/room?token=abc"}
	require.Equal(t, "https://hooks.example.com/room?token=abc", channel.Canonical())
	require.NotEqual(t, connection.Hash(), channel.Hash())
}

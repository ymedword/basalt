package app

// 本文件判卷网络域名白名单写入的启动恢复（US-12 R8.2.4 第 6 条）：按 `detail_json`
// 的前后修订号与候选域名哈希判 applied / not_applied / indeterminate，**只查证、
// 不重做**；判不出（缺证据 / 无行 / 缺端口）一律 unknown。
// Recovery acceptance for the allowlist write: verify, never replay.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// seedAllowlistPolicy 写入种子行（revision 1，域名固定）。
func seedAllowlistPolicy(t *testing.T, env *recoveryEnv, domains []string) {
	t.Helper()
	_, err := env.policy.Seed(context.Background(), domains, baselineClock())
	require.NoError(t, err)
}

// allowlistDetail 构造恢复所需的 detail 投影（与生产侧同一键名）。
func allowlistDetail(t *testing.T, before, after int64, domains []string) []byte {
	t.Helper()
	return mustDetailJSON(t, operationDetail{
		BeforeRevision:  &before,
		AfterRevision:   &after,
		CandidateSHA256: networkDomainsSHA256(domains),
	})
}

// pendingAllowlistOperation 预写一条白名单写操作。
func pendingAllowlistOperation(t *testing.T, env *recoveryEnv, detail []byte) string {
	t.Helper()
	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      core.ActionNetworkAllowlistUpdate,
		TargetKind:  core.TargetKindNetworkAllowlist,
		TargetID:    core.PolicyScopeHTTPDomains,
		DetailJSON:  detail,
	})
	return operationID
}

// TestNetworkAllowlistRecovery_AppliedWhenRevisionAndHashMatch —— 当前修订 = after 且
// 域名哈希一致 → applied：status 落 success、`recovered_at` 有值、策略不被重写。
// 逆改点：只看修订号不看哈希 / 恢复路径重做写入 → 对应断言红。
func TestNetworkAllowlistRecovery_AppliedWhenRevisionAndHashMatch(t *testing.T) {
	env := newRecoveryEnv(t)
	seedAllowlistPolicy(t, env, []string{"api.example.com"})
	_, err := env.policy.Update(context.Background(), []string{"hooks.example.net"}, 1,
		"op-applied", "token:abc123", baselineClock())
	require.NoError(t, err)

	operationID := pendingAllowlistOperation(t, env, allowlistDetail(t, 1, 2, []string{"hooks.example.net"}))
	require.NoError(t, recoverPendingOperations(context.Background(), env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.Equal(t, core.VerificationApplied, op.Verification)
	require.False(t, op.RecoveredAt.IsZero(), "恢复必须记录查证时间")

	policy, _, err := env.deps.NetworkPolicy.Load(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"hooks.example.net"}, policy.Domains, "恢复不得重做写入")
	require.Equal(t, int64(2), policy.Revision)
}

// TestNetworkAllowlistRecovery_NotAppliedWhenRevisionStillBefore —— 修订号仍是 before
// → not_applied：status 落 failed，且**不**补写域名（不重做）。
// 逆改点：not_applied 被当成 unknown 或触发重做 → 对应断言红。
func TestNetworkAllowlistRecovery_NotAppliedWhenRevisionStillBefore(t *testing.T) {
	env := newRecoveryEnv(t)
	seedAllowlistPolicy(t, env, []string{"api.example.com"})

	operationID := pendingAllowlistOperation(t, env, allowlistDetail(t, 1, 2, []string{"hooks.example.net"}))
	require.NoError(t, recoverPendingOperations(context.Background(), env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationFailed, op.Status)
	require.Equal(t, core.VerificationNotApplied, op.Verification)

	policy, _, err := env.deps.NetworkPolicy.Load(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"api.example.com"}, policy.Domains, "未生效的变更不得被补写")
	require.Equal(t, int64(1), policy.Revision)
}

// TestNetworkAllowlistRecovery_IndeterminateCases —— 判不出的一律 unknown：修订号与
// 前后值都不符（有人先写过）、detail 缺失、策略行不存在、未装配策略仓储。
// 逆改点：任一分支猜成 applied / not_applied → 对应子例红。
func TestNetworkAllowlistRecovery_IndeterminateCases(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, env *recoveryEnv) []byte
	}{
		{
			name: "revision neither before nor after",
			setup: func(t *testing.T, env *recoveryEnv) []byte {
				seedAllowlistPolicy(t, env, []string{"api.example.com"})
				_, err := env.policy.Update(context.Background(), []string{"third.example.com"}, 1,
					"op-third", "token:abc123", baselineClock())
				require.NoError(t, err)
				return allowlistDetail(t, 1, 2, []string{"hooks.example.net"})
			},
		},
		{
			name: "detail missing revisions",
			setup: func(t *testing.T, env *recoveryEnv) []byte {
				seedAllowlistPolicy(t, env, []string{"api.example.com"})
				return mustDetailJSON(t, operationDetail{CandidateSHA256: networkDomainsSHA256(
					[]string{"hooks.example.net"})})
			},
		},
		{
			name: "policy row missing",
			setup: func(t *testing.T, _ *recoveryEnv) []byte {
				return allowlistDetail(t, 1, 2, []string{"hooks.example.net"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newRecoveryEnv(t)
			detail := tc.setup(t, env)
			operationID := pendingAllowlistOperation(t, env, detail)
			require.NoError(t, recoverPendingOperations(context.Background(), env.deps))

			op := getOperation(t, env.store, operationID)
			require.Equal(t, core.OperationUnknown, op.Status)
			require.Equal(t, core.VerificationIndeterminate, op.Verification)
		})
	}
}

// TestNetworkAllowlistRecovery_MissingPortStaysUnknown —— 未装配策略仓储时不猜：unknown，
// 不因"读不到"把行标成失败。
// 逆改点：缺端口被映射成 not_applied → 运维会把一次可能已成功的写入当失败处理。
func TestNetworkAllowlistRecovery_MissingPortStaysUnknown(t *testing.T) {
	env := newRecoveryEnv(t)
	seedAllowlistPolicy(t, env, []string{"api.example.com"})
	operationID := pendingAllowlistOperation(t, env, allowlistDetail(t, 1, 2, []string{"hooks.example.net"}))

	deps := env.deps
	deps.NetworkPolicy = nil
	require.NoError(t, recoverPendingOperations(context.Background(), deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationUnknown, op.Status)
	require.Equal(t, core.VerificationIndeterminate, op.Verification)
}

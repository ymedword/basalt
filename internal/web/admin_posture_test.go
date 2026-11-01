package web

// 本文件覆盖启动期管理能力姿态（US-16 §2.14 / C-42 / D-31 / T027）：非法配置必须
// 拒绝启动且不给出可运行姿态；token 模式看解析结果而不是字段非空；local_trust 是
// 合法启用态而不是"认证关闭"。真实启动永远不产生 `unavailable`——它只是测试注入值。
// Startup admin posture: invalid configuration fails closed, a token posture requires
// a resolved token, and local_trust is an enabled state rather than "auth off".

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

func TestAdminPosture_InvalidConfigFailsStartup(t *testing.T) {
	cases := []struct {
		name      string
		cfg       AdminAuthConfig
		listeners []string
	}{
		{
			// 缺 mode 不是"默认 local_trust"，更不是"认证关闭"。
			name:      "missing mode",
			cfg:       AdminAuthConfig{},
			listeners: []string{"127.0.0.1:8080"},
		},
		{
			// 不存在"明确禁用管理能力"的运行时第三态：写 disabled 也只能拒绝启动。
			name:      "disabled is not a state",
			cfg:       AdminAuthConfig{Mode: "disabled"},
			listeners: []string{"127.0.0.1:8080"},
		},
		{
			// local_trust 要求全部监听回环；非回环地址不得折算成启用态。
			name:      "local trust on non-loopback listener",
			cfg:       AdminAuthConfig{Mode: "local_trust"},
			listeners: []string{"192.0.2.10:8080"},
		},
		{
			// 通配监听没有可校对的 Host，必须显式列白名单——local_trust 更不允许。
			name:      "local trust on wildcard listener",
			cfg:       AdminAuthConfig{Mode: "local_trust"},
			listeners: []string{"0.0.0.0:8080"},
		},
		{
			// token 模式可以绑非回环，但地址必须逐条列进 allowed_hosts。
			name:      "token mode on unlisted non-loopback listener",
			cfg:       AdminAuthConfig{Mode: "token", Token: "resolved-test-token"},
			listeners: []string{"192.0.2.10:8080"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			posture, err := ResolveAdminPosture(tc.cfg, tc.listeners)

			require.Error(t, err, "非法管理认证配置必须拒绝启动")
			require.False(t, posture.Valid(), "拒绝启动时不得同时给出可运行姿态")
			require.NotEqual(t, core.AdminPostureUnavailable, posture,
				"配置非法不是 unavailable——那只是测试注入值，真实启动没有第三种结果")
		})
	}
}

func TestAdminPosture_TokenModeRequiresResolvedToken(t *testing.T) {
	invalid := []struct {
		name  string
		token string
	}{
		{"empty token", ""},
		{"unresolved placeholder", "${BASALT_ADMIN_TOKEN}"},
		{"placeholder embedded in a longer value", "prefix-${BASALT_ADMIN_TOKEN}-suffix"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			posture, err := ResolveAdminPosture(
				AdminAuthConfig{Mode: adminAuthModeToken, Token: tc.token},
				[]string{"127.0.0.1:8080"})

			require.Error(t, err, "token 模式必须看解析结果，不能只看字段非空")
			require.False(t, posture.Valid(), "未解析的凭据不得折算成可运行姿态")
		})
	}

	t.Run("resolved token", func(t *testing.T) {
		posture, err := ResolveAdminPosture(
			AdminAuthConfig{Mode: adminAuthModeToken, Token: "resolved-test-token"},
			[]string{"127.0.0.1:8080"})

		require.NoError(t, err)
		require.Equal(t, core.AdminPostureToken, posture)
		require.True(t, posture.Valid())
		require.NotEqual(t, core.AdminPostureUnavailable, posture,
			"真实启动只产生启用态，unavailable 仅测试可注入")
	})

	t.Run("resolved token on an allowlisted remote listener", func(t *testing.T) {
		posture, err := ResolveAdminPosture(AdminAuthConfig{
			Mode:         adminAuthModeToken,
			Token:        "resolved-test-token",
			AllowedHosts: []string{"192.0.2.10:8080"},
		}, []string{"192.0.2.10:8080"})

		require.NoError(t, err)
		require.Equal(t, core.AdminPostureToken, posture, "token 模式不要求回环监听")
	})
}

func TestAdminPosture_LocalTrustIsEnabledMode(t *testing.T) {
	for _, listeners := range [][]string{
		{"127.0.0.1:8080"},
		{"[::1]:8080", "127.0.0.1:8081"},
	} {
		posture, err := ResolveAdminPosture(
			AdminAuthConfig{Mode: adminAuthModeLocalTrust}, listeners)

		require.NoError(t, err, "local_trust + 全部回环是合法启用态")
		require.Equal(t, core.AdminPostureLocalTrust, posture)
		require.True(t, posture.Valid())
		require.NotEqual(t, core.AdminPostureUnavailable, posture,
			"local_trust 不是'认证关闭'，不得折算成 unavailable")
		require.NotEqual(t, core.AdminPostureToken, posture)
	}
}

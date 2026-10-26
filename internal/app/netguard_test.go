package app

// 本文件只判装配层的**纯函数**（代理 URL 解析）：`installOutboundGuard` 会写进程级
// 守门器指针，在包内测试里调用会污染后续用例的受控客户端——守门器行为本身在
// `internal/tool` 的 `TestNetGuard_*` 里判，这里不重复。

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

// TestOutboundProxyURL 覆盖 `http.proxy` 的装配口径：空串 = 直连（nil）；写了解析成
// 守门器要的形状；无法装配时拒绝（**不**退化为直连——那等于换了一个出口）。
// TestOutboundProxyURL: empty means direct; an unusable value refuses instead of
// silently falling back to direct egress.
func TestOutboundProxyURL(t *testing.T) {
	t.Run("nil_config_is_direct", func(t *testing.T) {
		proxy, err := outboundProxyURL(nil)
		require.NoError(t, err)
		require.Nil(t, proxy)
	})

	t.Run("empty_is_direct", func(t *testing.T) {
		proxy, err := outboundProxyURL(&cli.Config{})
		require.NoError(t, err)
		require.Nil(t, proxy)
	})

	t.Run("explicit_proxy_is_pinned", func(t *testing.T) {
		proxy, err := outboundProxyURL(&cli.Config{HTTP: cli.HTTPConfig{Proxy: "http://proxy.internal:3128"}})
		require.NoError(t, err)
		require.NotNil(t, proxy)
		require.Equal(t, "http", proxy.Scheme)
		require.Equal(t, "proxy.internal:3128", proxy.Host)
	})

	t.Run("unusable_proxy_refuses_instead_of_direct", func(t *testing.T) {
		_, err := outboundProxyURL(&cli.Config{HTTP: cli.HTTPConfig{Proxy: "proxy.internal:3128"}})
		require.Error(t, err)
		require.NotContains(t, err.Error(), "proxy.internal", "文案不回显取值")
	})
}

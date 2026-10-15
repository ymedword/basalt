package testutil

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTestutil_HTTPServer 是测试基建的自证用例（文档用例名：TestTestutil_HTTPServer
// ——基础设施自证：首个不依赖真实网络的本地服务用例）：监听地址必须是回环地址，
// 请求经它往返即达，不触碰真实网络。
// TestTestutil_HTTPServer proves the test infrastructure itself: the listen
// address is loopback and a request round-trips through it.
func TestTestutil_HTTPServer(t *testing.T) {
	srv := NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Test-Server", "basalt")
		if _, err := io.WriteString(w, "basalt"); err != nil {
			t.Errorf("测试服务写响应失败: %v", err)
		}
	}))

	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err, "测试服务的 URL 必须可解析出主机与端口")
	require.True(t, net.ParseIP(host).IsLoopback(),
		"测试服务必须监听回环地址（不访问真实网络），实际主机 %q", host)

	resp, err := srv.Client().Get(srv.URL)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "basalt", resp.Header.Get("X-Test-Server"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "basalt", string(body))
}

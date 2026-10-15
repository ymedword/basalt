package testutil

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// NewHTTPServer 起一个本地回环 HTTP 测试服务并登记清理，测试结束自动关闭。
// NewHTTPServer starts a loopback HTTP test server and registers its cleanup.
//
// httptest 只监听回环地址——测试不访问真实网络；真网络的服务端联调不在单测范围。
// The server binds loopback only, so tests never reach the real network.
func NewHTTPServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

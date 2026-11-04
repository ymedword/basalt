package web

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// 作者读取面复用真实管理认证，不能回落业务读取或匿名页面。
// Authoring reads use the real admin guard rather than a business or anonymous fallback.
func TestFlow_AuthoringEndpointsAuthentication(t *testing.T) {
	for _, path := range []string{"/api/v1/admin/flow-agents", "/api/v1/admin/flows/fd_fixture/versions/1"} {
		t.Run(path, func(t *testing.T) {
			srv := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth("authoring-test-admin") })
			response := serveAdmin(srv, adminRequest(http.MethodGet, path, ""))
			require.Equal(t, http.StatusUnauthorized, response.Code, "authoring route must enter admin authentication before its backend")
			require.NotContains(t, response.Body.String(), "bundle_json")
		})
	}
}

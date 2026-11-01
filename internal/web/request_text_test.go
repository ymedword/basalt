package web

// 本文件是入口「可存储性校验」的判卷面（US-16 人工走查发现，2026-10-31）。
//
// 事实：候选实例上 `GET /api/v1/history/search?q=%FF%FE`、`?q=a%00b` 与
// `GET /api/v1/sessions?agent=%FF` 都返回 500 `INTERNAL`——查询参数原样流进 SQL
// 文本参数，PostgreSQL 报 `SQLSTATE 22021`（非法 UTF-8 / 0x00）。这把"客户端请求
// 形状非法"错报成"服务端故障"，还让内部存储细节冒充了 500 的理由。
//
// 判据：受保护路由在**认证 → 限流 → 作用域之后、处理函数之前**校验路径与查询参数
// 的可存储性；含非法 UTF-8 或 NUL → 400 `BAD_REQUEST`，零端口调用。顺序三条都要判：
// 未认证请求先拿 401（形状校验不得泄给未认证方），被限流先拿 429，缺作用域先拿 403
// ——否则新的 400 会改写 C-F03 已裁决的准入顺序。
// These tests pin the request-text guard and, just as importantly, its position in the
// admission order: auth, rate, and scope all answer before the shape check does.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
)

// TestRequestText_RejectsUnstorableQueryAndPath 判四类坏字节在触达端口之前一律 400：
// 查询值里的非法 UTF-8 / NUL、过滤维里的非法 UTF-8、路径段里的非法 UTF-8。
// 逆改点：去掉守卫 → 四条都变成 200（假端口不校验），`requireKnowledgeError` 逐条红。
func TestRequestText_RejectsUnstorableQueryAndPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path string
	}{
		{"非法 UTF-8 关键词", "/api/v1/history/search?q=%FF%FE"},
		{"NUL 关键词", "/api/v1/history/search?q=a%00b"},
		{"非法 UTF-8 过滤维", "/api/v1/sessions?agent=%FF"},
		{"非法 UTF-8 路径段", "/api/v1/sessions/s-1/messages/%FF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fakes := newHistoryServer(t, nil)
			rec := historyGet(srv, tc.path)
			requireKnowledgeError(t, rec, http.StatusBadRequest, codeBadRequest,
				messageInvalidRequestText)
			require.Zero(t, fakes.search.calls, "形状非法不得触达检索端口")
			require.Zero(t, fakes.read.calls, "形状非法不得触达精确读取端口")
		})
	}
}

// TestRequestText_ValidUTF8StillPasses 判守卫不误伤：合法 UTF-8 关键词照常到端口。
// 逆改点：把"含非 ASCII 字节"当非法（例如按 ASCII 判）→ 本用例从 200 变 400。
func TestRequestText_ValidUTF8StillPasses(t *testing.T) {
	t.Parallel()
	srv, fakes := newHistoryServer(t, nil)

	rec := historyGet(srv, "/api/v1/history/search?q=%E5%B7%A1%E6%A3%80")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, fakes.search.calls)
}

// TestRequestText_AdminFaceRejectsAfterAuth 判管理面两条：带合法凭据的坏查询 400；
// **缺凭据**时仍是 401——形状校验在认证闸门内侧，未认证方拿不到参数规则的答案。
// 逆改点：把守卫挂到认证之前 → 第二条从 401 变 400。
func TestRequestText_AdminFaceRejectsAfterAuth(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })

	bad := withBearer(adminRequest(http.MethodGet, "/api/v1/diagnostics?agent=%FF", ""), testToken)
	requireKnowledgeError(t, serveAdmin(srv, bad), http.StatusBadRequest, codeBadRequest,
		messageInvalidRequestText)

	anonymous := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/diagnostics?agent=%FF", ""))
	require.Equal(t, http.StatusUnauthorized, anonymous.Code,
		"认证先于形状校验：未认证请求不得先拿到 400")
}

// TestRequestText_RateLimitPrecedesShapeCheck 判 C-F03 的准入顺序不被新守卫改写：
// 入口速率拒绝先回答（429），形状校验排在它之后。
// 逆改点：把守卫移到 admitIngress 之前 → 本用例从 429 变 400。
func TestRequestText_RateLimitPrecedesShapeCheck(t *testing.T) {
	t.Parallel()
	gate := &recordingIngress{err: callers.ErrRateLimited}
	srv, _, _ := newIngressServer(t, gate)

	req := withBearer(adminRequest(http.MethodGet, "/api/v1/sessions?agent=%FF", ""), "ro-secret")
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "先限流、后形状（C-F03 的同一顺序）")
}

// TestRequestText_ScopePrecedesShapeCheck 判作用域判定仍先于形状校验（C-F03 的
// "先限流、后作用域"，形状在其后）：缺 `sessions:read` 的调用方拿 403，不是 400。
// 逆改点：把守卫挪到 rt.scope.allows 之前 → 本用例从 403 变 400。
func TestRequestText_ScopePrecedesShapeCheck(t *testing.T) {
	t.Parallel()
	srv, _, _ := newIngressServer(t, &recordingIngress{})

	// c1 只有 sessions:write，读会话列表应被作用域拒绝。
	req := withBearer(adminRequest(http.MethodGet, "/api/v1/sessions?agent=%FF", ""), "c1-secret")
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// TestRequestText_PublicAndFallbackUntouched 判公开面与兜底不受影响：健康检查带坏
// 参数仍 200（它不读参数），未匹配路径仍是 404（兜底只回显路径）。
// 逆改点：把守卫挂进 handler() 的全局链 → 两条断言分别变 400。
func TestRequestText_PublicAndFallbackUntouched(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, nil)

	health := serveAdmin(srv, httptest.NewRequest(http.MethodGet, "/api/v1/health?probe=%FF", nil))
	require.Equal(t, http.StatusOK, health.Code)

	missing := serveAdmin(srv, httptest.NewRequest(http.MethodGet, "/no-such-path?probe=%FF", nil))
	require.Equal(t, http.StatusNotFound, missing.Code)
}

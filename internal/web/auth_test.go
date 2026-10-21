package web

// 本文件覆盖管理认证闸门（模块卡 §3.2.1 的 26 例口径、§4.3.1 的关键回归）：
// token 四象限、Host / Origin 矩阵、来源归属、local_trust 三重条件、固定文案与
// actor 指纹。用例先行于实现落位，`TestAuth_Matrix` 按模块卡原样保留。
// The auth tests cover token quadrants, the host and origin matrices, source
// attribution, local-trust preconditions, fixed messages, and actor fingerprints.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// authFixedNow 是文案 / 时间戳断言的固定时钟（UTC）。
// authFixedNow is the fixed clock for message and timestamp assertions.
var authFixedNow = time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)

// testToken 是测试用管理凭据（只出现在测试内存里，不落盘）。
// testToken is the in-memory test credential.
const testToken = "test-token-value"

// discardLogger 丢弃认证拒绝的 WARN，保持测试输出干净。
// discardLogger drops the guard's WARN lines.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testAdminAuth 构造真实认证闸门供 handler / 路由级用例使用：token 模式 + 回环
// Host 三元组 + 固定时钟。签名与模块卡 §4.3.2 的用例写法一致；构造失败是测试装配
// 错误，直接 panic（没有 *testing.T 可交）。
// testAdminAuth builds a real guard for handler- and route-level acceptance tests;
// construction failure is a test-assembly bug, hence the panic.
func testAdminAuth(token string) AdminAuth {
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          token,
		AllowedHosts:   []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow })
	if err != nil {
		panic("testAdminAuth: " + err.Error())
	}
	return guard
}

// decodeAuthEnvelope 解析固定信封用于文案与时间戳断言。
// decodeAuthEnvelope decodes the fixed envelope.
func decodeAuthEnvelope(t *testing.T, rec *httptest.ResponseRecorder) Envelope {
	t.Helper()
	var env Envelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env
}

// TestAuth_Matrix 按模块卡 §4.3.1 **原样落地**：六类请求 × token 模式。
// 逆改点：Host / Origin 检查晚于凭据 → host / origin 两例变 401；常量时间比较
// 改回明文比较 → 行为不变（属实现纪律，由代码评审判）。
// TestAuth_Matrix lands verbatim from the module card.
func TestAuth_Matrix(t *testing.T) {
	t.Parallel()
	const token = "test-token-value"
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:           "token",
		Token:          token,
		AllowedHosts:   []string{"127.0.0.1:8080", "localhost:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, slog.Default(), time.Now)
	if err != nil {
		t.Fatalf("NewAuthGuard: %v", err)
	}

	next := adminHandler(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		_, _ = w.Write([]byte(a.Subject))
	})
	h := guard.Wrap(next)

	cases := []struct {
		name   string
		host   string
		origin string
		auth   string
		want   int
	}{
		{"valid", "127.0.0.1:8080", "", "Bearer " + token, http.StatusOK},
		{"missing token", "127.0.0.1:8080", "", "", http.StatusUnauthorized},
		{"wrong token", "127.0.0.1:8080", "", "Bearer nope", http.StatusUnauthorized},
		{"wrong scheme", "127.0.0.1:8080", "", "Basic " + token, http.StatusUnauthorized},
		{"host mismatch", "evil.example:8080", "", "Bearer " + token, http.StatusForbidden},
		{"origin mismatch", "127.0.0.1:8080", "http://evil.example", "Bearer " + token, http.StatusForbidden},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestAuth_TokenActorFingerprint：成功认证的 actor 只携带 token 指纹的前 16 位
// 十六进制；明文 token 不进入 actor、响应或日志。
// 逆改点：指纹取全量 / 取前 8 位 → 断言红。
func TestAuth_TokenActorFingerprint(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:         "token",
		Token:        testToken,
		AllowedHosts: []string{"127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow })
	require.NoError(t, err)

	var got core.Actor
	h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		got = a
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.0.2.9:50000"
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	sum := sha256.Sum256([]byte(testToken))
	require.Equal(t, core.ActorSubjectTokenPrefix+hex.EncodeToString(sum[:])[:16], got.Subject)
	require.Len(t, got.Subject, len(core.ActorSubjectTokenPrefix)+16)
	require.Equal(t, core.AuthMethodBearer, got.Method)
	require.Equal(t, "192.0.2.9:50000", got.Source)
}

// TestAuth_TokenSchemeCaseInsensitive：Bearer scheme 大小写不敏感（RFC 7235）。
// 逆改点：改回逐字匹配 "Bearer " → 小写 scheme 用例红。
func TestAuth_TokenSchemeCaseInsensitive(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:         "token",
		Token:        testToken,
		AllowedHosts: []string{"127.0.0.1:8080"},
	}, discardLogger(), nil)
	require.NoError(t, err)

	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Authorization", scheme+" "+testToken)
		rec := httptest.NewRecorder()
		guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
			w.WriteHeader(http.StatusNoContent)
		}).ServeHTTP(rec, req)
		require.Equalf(t, http.StatusNoContent, rec.Code, "scheme %q 应当被接受", scheme)
	}
}

// TestAuth_UnauthorizedMessageIdentical：401 不区分"没提供"与"提供错"——两者
// 响应体逐字相同，且都不回显 token（约束 45）。
// 逆改点：文案按缺失 / 无效分叉 → 两响应体不再相等。
func TestAuth_UnauthorizedMessageIdentical(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:         "token",
		Token:        testToken,
		AllowedHosts: []string{"127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow })
	require.NoError(t, err)
	h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		t.Fatal("未通过认证不得调用处理函数")
	})

	send := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
		req.Host = "127.0.0.1:8080"
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	missing := send("")
	invalid := send("Bearer wrong-token")

	require.Equal(t, http.StatusUnauthorized, missing.Code)
	require.Equal(t, http.StatusUnauthorized, invalid.Code)
	require.Equal(t, missing.Body.String(), invalid.Body.String())
	require.NotContains(t, missing.Body.String(), testToken)
	require.NotContains(t, missing.Body.String(), "wrong-token")
	env := decodeAuthEnvelope(t, missing)
	require.Equal(t, codeUnauthorized, env.Code)
	require.Equal(t, messageUnauthorized, env.Message)
	require.Equal(t, authFixedNow.UTC().Format(time.RFC3339), env.Timestamp)
}

// TestAuth_HostMatrix：Host 含端口逐字匹配、大小写不敏感、缺端口不匹配；
// Host 拒绝先于凭据判定。
// 逆改点：忽略端口 / 忽略大小写方向反了 / 先判凭据 → 对应用例红。
func TestAuth_HostMatrix(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:         "token",
		Token:        testToken,
		AllowedHosts: []string{"localhost:8080", "127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow })
	require.NoError(t, err)
	h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name string
		host string
		auth string
		want int
	}{
		{"exact", "127.0.0.1:8080", "Bearer " + testToken, http.StatusNoContent},
		{"case insensitive", "LOCALHOST:8080", "Bearer " + testToken, http.StatusNoContent},
		{"wrong port", "127.0.0.1:9090", "Bearer " + testToken, http.StatusForbidden},
		{"missing port", "127.0.0.1", "Bearer " + testToken, http.StatusForbidden},
		{"empty host", "", "Bearer " + testToken, http.StatusForbidden},
		{"host before credentials", "evil.example:8080", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
			req.Host = tc.host
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code, "body=%s", rec.Body.String())
			if tc.want == http.StatusForbidden {
				env := decodeAuthEnvelope(t, rec)
				require.Equal(t, codeForbidden, env.Code)
				require.Equal(t, messageForbidden, env.Message)
			}
		})
	}
}

// TestAuth_OriginMatrix：Origin 完整匹配、`null` 拒绝、带路径 / userinfo / 错
// scheme 拒绝、http/https 缺省端口等价；Origin 拒绝先于凭据判定。
// 逆改点：改为前缀匹配 / 放过 null / 不补缺省端口 → 对应用例红。
func TestAuth_OriginMatrix(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:           "token",
		Token:          testToken,
		AllowedHosts:   []string{"127.0.0.1:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080", "http://127.0.0.1:80"},
	}, discardLogger(), func() time.Time { return authFixedNow })
	require.NoError(t, err)
	h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name   string
		origin string
		auth   string
		want   int
	}{
		{"exact", "http://127.0.0.1:8080", "Bearer " + testToken, http.StatusNoContent},
		{"default port omitted", "http://127.0.0.1", "Bearer " + testToken, http.StatusNoContent},
		{"null origin", "null", "Bearer " + testToken, http.StatusForbidden},
		{"origin with path", "http://127.0.0.1:8080/app", "Bearer " + testToken, http.StatusForbidden},
		{"wrong scheme", "https://127.0.0.1:8080", "Bearer " + testToken, http.StatusForbidden},
		{"userinfo", "http://user:pass@127.0.0.1:8080", "Bearer " + testToken, http.StatusForbidden},
		{"origin before credentials", "http://evil.example", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
			req.Host = "127.0.0.1:8080"
			req.Header.Set("Origin", tc.origin)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// TestAuth_OriginAbsentStillRequiresCredentials：缺失 Origin 本身不是放行依据
// （约束 5）——非浏览器客户端不带 Origin，但凭据仍必须有效。
// 逆改点：把"无 Origin"当放行条件 → 无凭据请求变 204。
func TestAuth_OriginAbsentStillRequiresCredentials(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:           "token",
		Token:          testToken,
		AllowedHosts:   []string{"127.0.0.1:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, discardLogger(), nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		t.Fatal("无凭据不得调用处理函数")
	}).ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestAuth_ForbiddenMessageDoesNotExplainRule：Host 与 Origin 两类 403 的响应体
// 逐字相同——不解释命中哪条规则（约束 45）。
// 逆改点：文案带上 "host" / "origin" → 两响应体不等或含规则词。
func TestAuth_ForbiddenMessageDoesNotExplainRule(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:           "token",
		Token:          testToken,
		AllowedHosts:   []string{"127.0.0.1:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow })
	require.NoError(t, err)
	h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		w.WriteHeader(http.StatusNoContent)
	})

	hostReject := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	hostReject.Host = "evil.example:8080"
	hostRec := httptest.NewRecorder()
	h.ServeHTTP(hostRec, hostReject)

	originReject := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	originReject.Host = "127.0.0.1:8080"
	originReject.Header.Set("Origin", "http://evil.example")
	originRec := httptest.NewRecorder()
	h.ServeHTTP(originRec, originReject)

	require.Equal(t, http.StatusForbidden, hostRec.Code)
	require.Equal(t, http.StatusForbidden, originRec.Code)
	require.Equal(t, hostRec.Body.String(), originRec.Body.String())
	env := decodeAuthEnvelope(t, hostRec)
	require.Equal(t, codeForbidden, env.Code)
	require.Equal(t, messageForbidden, env.Message)
	require.NotContains(t, strings.ToLower(env.Message), "host")
	require.NotContains(t, strings.ToLower(env.Message), "origin")
}

// TestAdminAuth_LocalTrustActor：回环直接对端 + 信任头 + Host 通过后放行，
// actor 为 local_trust 字面量，来源为直接对端地址。
// 逆改点：actor 写成 token 形态 / source 被转发头污染 → 断言红。
func TestAdminAuth_LocalTrustActor(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:         "local_trust",
		AllowedHosts: []string{"127.0.0.1:8080"},
	}, discardLogger(), nil)
	require.NoError(t, err)

	var got core.Actor
	h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		got = a
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "127.0.0.1:52000"
	req.Header.Set(localTrustHeader, "1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, core.ActorSubjectLocalTrust, got.Subject)
	require.Equal(t, core.AuthMethodLocalTrust, got.Method)
	require.Equal(t, "127.0.0.1:52000", got.Source)
}

// TestAdminAuth_LocalTrustRejectsForwardedHeaders：五类转发头**任一存在**都否决
// 本机信任（约束 3），即使值看起来是回环地址。
// 逆改点：放行任一转发头 → 对应用例变 204。
func TestAdminAuth_LocalTrustRejectsForwardedHeaders(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:         "local_trust",
		AllowedHosts: []string{"127.0.0.1:8080"},
	}, discardLogger(), nil)
	require.NoError(t, err)
	h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		t.Fatal("带转发头不得调用处理函数")
	})

	for _, name := range forwardedHeaderNames {
		name := name
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
			req.Host = "127.0.0.1:8080"
			req.RemoteAddr = "127.0.0.1:52000"
			req.Header.Set(name, "127.0.0.1")
			req.Header.Set(localTrustHeader, "1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code)
		})
	}
}

// TestAdminAuth_LocalTrustRejectsNonLoopbackPeer：直接对端非回环 → 403；
// 转发头里的 127.0.0.1 不得证明回环（约束 6）。
// 逆改点：用 sourceOf 的结果判回环 → 伪造 XFF 用例变 204。
func TestAdminAuth_LocalTrustRejectsNonLoopbackPeer(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:           "local_trust",
		AllowedHosts:   []string{"127.0.0.1:8080"},
		TrustedProxies: []string{"10.0.0.0/8"},
	}, discardLogger(), nil)
	require.NoError(t, err)
	h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		t.Fatal("非回环对端不得调用处理函数")
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "10.1.2.3:40000"
	req.Header.Set(localTrustHeader, "1")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// TestAdminAuth_LocalTrustRequiresTrustHeader：信任头缺失或值不是逐字 "1" → 403。
// 逆改点：只要头存在就放行 → 对应用例红。
func TestAdminAuth_LocalTrustRequiresTrustHeader(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:         "local_trust",
		AllowedHosts: []string{"127.0.0.1:8080"},
	}, discardLogger(), nil)
	require.NoError(t, err)
	h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		t.Fatal("缺少信任头不得调用处理函数")
	})

	for _, value := range []string{"", "0", "true", "yes", "1 "} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
		req.Host = "127.0.0.1:8080"
		req.RemoteAddr = "127.0.0.1:52000"
		if value != "" {
			req.Header.Set(localTrustHeader, value)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equalf(t, http.StatusForbidden, rec.Code, "值 %q 不应通过", value)
	}
}

// TestAdminAuth_SourceAttribution：只有直接对端在可信网段内才解析转发头；
// 取最右侧非可信地址；无可用跳时回退直接对端（约束 6）。
// 逆改点：无条件信任 XFF / 取最左侧 / 取可信跳 → 对应用例红。
func TestAdminAuth_SourceAttribution(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:           "token",
		Token:          testToken,
		AllowedHosts:   []string{"127.0.0.1:8080"},
		TrustedProxies: []string{"10.0.0.0/8"},
	}, discardLogger(), nil)
	require.NoError(t, err)

	serve := func(t *testing.T, remoteAddr string, headers map[string]string) core.Actor {
		t.Helper()
		var got core.Actor
		h := guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
			got = a
			w.WriteHeader(http.StatusNoContent)
		})
		req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
		req.Host = "127.0.0.1:8080"
		req.RemoteAddr = remoteAddr
		req.Header.Set("Authorization", "Bearer "+testToken)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusNoContent, rec.Code, "body=%s", rec.Body.String())
		return got
	}

	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{
			name:       "untrusted peer ignores XFF",
			remoteAddr: "192.0.2.1:1234",
			headers:    map[string]string{"X-Forwarded-For": "127.0.0.1"},
			want:       "192.0.2.1:1234",
		},
		{
			name:       "trusted peer takes rightmost non-trusted",
			remoteAddr: "10.1.2.3:443",
			headers:    map[string]string{"X-Forwarded-For": "198.51.100.7, 203.0.113.9"},
			want:       "203.0.113.9",
		},
		{
			name:       "trusted peer skips trusted hops",
			remoteAddr: "10.1.2.3:443",
			headers:    map[string]string{"X-Forwarded-For": "198.51.100.7, 10.9.9.9"},
			want:       "198.51.100.7",
		},
		{
			name:       "all hops trusted falls back to peer",
			remoteAddr: "10.1.2.3:443",
			headers:    map[string]string{"X-Forwarded-For": "10.8.8.8, 10.9.9.9"},
			want:       "10.1.2.3:443",
		},
		{
			name:       "no forwarding headers uses peer",
			remoteAddr: "10.1.2.3:443",
			headers:    nil,
			want:       "10.1.2.3:443",
		},
		{
			name:       "X-Real-IP fallback",
			remoteAddr: "10.1.2.3:443",
			headers:    map[string]string{"X-Real-IP": "203.0.113.11"},
			want:       "203.0.113.11",
		},
		{
			name:       "Forwarded header fallback",
			remoteAddr: "10.1.2.3:443",
			headers:    map[string]string{"Forwarded": "for=203.0.113.9;proto=https, for=10.0.0.5"},
			want:       "203.0.113.9",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, serve(t, tc.remoteAddr, tc.headers).Source)
		})
	}
}

// TestAdminAuth_ValidateListeners：监听器相关规则的启动期判定（约束 1 / 3 / 4）。
// 逆改点：非回环监听不再要求显式白名单 / local_trust 放过非回环 → 对应用例由
// 错误变 nil（启动被放行）。
func TestAdminAuth_ValidateListeners(t *testing.T) {
	t.Parallel()
	tokenMode := AdminAuthConfig{Mode: "token", Token: testToken}
	cases := []struct {
		name      string
		cfg       AdminAuthConfig
		listeners []string
		wantErr   string
	}{
		{name: "missing mode", cfg: AdminAuthConfig{}, wantErr: "mode"},
		{name: "invalid mode", cfg: AdminAuthConfig{Mode: "trust"}, wantErr: "mode"},
		{name: "token missing", cfg: AdminAuthConfig{Mode: "token"}, wantErr: "token"},
		{name: "token unresolved", cfg: AdminAuthConfig{Mode: "token", Token: "${BASALT_ADMIN_TOKEN}"}, wantErr: "占位"},
		{name: "loopback listener ok", cfg: tokenMode, listeners: []string{"127.0.0.1:8080"}},
		{name: "localhost listener ok", cfg: tokenMode, listeners: []string{"localhost:8080"}},
		{name: "non-loopback missing from allowlist", cfg: tokenMode, listeners: []string{"10.0.0.5:8080"}, wantErr: "allowed_hosts"},
		{
			name:      "non-loopback explicit",
			cfg:       AdminAuthConfig{Mode: "token", Token: testToken, AllowedHosts: []string{"10.0.0.5:8080"}},
			listeners: []string{"10.0.0.5:8080"},
		},
		{
			name:      "wildcard listener needs same port",
			cfg:       AdminAuthConfig{Mode: "token", Token: testToken, AllowedHosts: []string{"basalt.internal:8080"}},
			listeners: []string{"0.0.0.0:8080"},
		},
		{
			name:      "wildcard listener without allowlist",
			cfg:       tokenMode,
			listeners: []string{"0.0.0.0:8080"},
			wantErr:   "allowed_hosts",
		},
		{
			name:      "local_trust loopback only",
			cfg:       AdminAuthConfig{Mode: "local_trust"},
			listeners: []string{"127.0.0.1:8080", "[::1]:8080"},
		},
		{
			name:      "local_trust rejects non-loopback",
			cfg:       AdminAuthConfig{Mode: "local_trust"},
			listeners: []string{"0.0.0.0:8080"},
			wantErr:   "local_trust",
		},
		{
			name:    "wildcard allowed host",
			cfg:     AdminAuthConfig{Mode: "token", Token: testToken, AllowedHosts: []string{"*"}},
			wantErr: "通配符",
		},
		{
			name:    "host without port",
			cfg:     AdminAuthConfig{Mode: "token", Token: testToken, AllowedHosts: []string{"127.0.0.1"}},
			wantErr: "端口",
		},
		{
			name:    "origin with path",
			cfg:     AdminAuthConfig{Mode: "token", Token: testToken, AllowedOrigins: []string{"http://127.0.0.1:8080/app"}},
			wantErr: "allowed_origins",
		},
		{
			name:    "bad CIDR",
			cfg:     AdminAuthConfig{Mode: "token", Token: testToken, TrustedProxies: []string{"10.0.0.1"}},
			wantErr: "CIDR",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAdminAuth(tc.cfg, tc.listeners)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestAdminAuth_NewGuardRejectsInvalidConfig：闸门自身也不接受残缺配置（双保险：
// 即使调用方跳过 ValidateAdminAuth，手工构造的守卫也必须拒绝而不是放行）。
// 逆改点：通配符 / 无端口条目被接受 → 对应构造用例由错误变成功。
func TestAdminAuth_NewGuardRejectsInvalidConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     AdminAuthConfig
		wantErr string
	}{
		{name: "missing mode", cfg: AdminAuthConfig{}, wantErr: "mode"},
		{name: "missing token", cfg: AdminAuthConfig{Mode: "token"}, wantErr: "token"},
		{name: "unresolved token", cfg: AdminAuthConfig{Mode: "token", Token: "${X}"}, wantErr: "占位"},
		{name: "wildcard host", cfg: AdminAuthConfig{Mode: "token", Token: testToken, AllowedHosts: []string{"*"}}, wantErr: "通配符"},
		{name: "host without port", cfg: AdminAuthConfig{Mode: "token", Token: testToken, AllowedHosts: []string{"localhost"}}, wantErr: "host:port"},
		{name: "null origin", cfg: AdminAuthConfig{Mode: "token", Token: testToken, AllowedOrigins: []string{"null"}}, wantErr: "allowed_origins"},
		{name: "bad CIDR", cfg: AdminAuthConfig{Mode: "token", Token: testToken, TrustedProxies: []string{"10.0.0.1/33"}}, wantErr: "CIDR"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			guard, err := NewAuthGuard(tc.cfg, discardLogger(), nil)
			require.Error(t, err)
			require.Nil(t, guard)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestAdminAuth_NilDefaults：nil 日志器与 nil 时钟走缺省值，成功请求不受影响。
// nil 时钟的响应时间戳可解析为 RFC3339（真实时钟）。
func TestAdminAuth_NilDefaults(t *testing.T) {
	t.Parallel()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:         "token",
		Token:        testToken,
		AllowedHosts: []string{"127.0.0.1:8080"},
	}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, guard.logger)
	require.NotNil(t, guard.now)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	guard.Wrap(func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		w.WriteHeader(http.StatusNoContent)
	}).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
}

// authTestClock 是可手动推进的时钟：窗口边界用例用它固定"第几秒"，
// 不依赖真实时间（约束 7 复核要求固定时钟用例）。
// authTestClock is a manually advanced clock for window-boundary cases.
type authTestClock struct {
	mu      sync.Mutex
	current time.Time
}

// Now 返回当前测试时刻。
func (c *authTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// Advance 推进测试时刻。
func (c *authTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
}

// newLimitGuard 构造带限速的 token 模式闸门（固定时钟由调用方提供）。
// newLimitGuard builds a token-mode guard on the given clock.
func newLimitGuard(t *testing.T, now func() time.Time, mutate func(*AdminAuthConfig)) *authGuard {
	t.Helper()
	cfg := AdminAuthConfig{
		Mode:         "token",
		Token:        testToken,
		AllowedHosts: []string{"127.0.0.1:8080"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	guard, err := NewAuthGuard(cfg, discardLogger(), now)
	require.NoError(t, err)
	return guard
}

// sendAuth 发一条受保护请求（默认来源 192.0.2.1:1234）。
// sendAuth sends one guarded request from the default test source.
func sendAuth(h http.Handler, authHeader, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
	req.Host = "127.0.0.1:8080"
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// passingHandler 是认证通过后的处理函数：回 204 并计数。
func passingHandler(hits *atomic.Int64) adminHandler {
	return func(w http.ResponseWriter, r *http.Request, a core.Actor) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}
}

// TestAuthLimiter_NineFailuresKeepOriginalStatus：第 1–9 次失败都返回 401，
// 且不带 Retry-After（约束 7）。
// 逆改点：把第 9 次也判 429 → 本用例红。
func TestAuthLimiter_NineFailuresKeepOriginalStatus(t *testing.T) {
	t.Parallel()
	guard := newLimitGuard(t, func() time.Time { return authFixedNow }, nil)
	var hits atomic.Int64
	h := guard.Wrap(passingHandler(&hits))

	for i := 1; i <= 9; i++ {
		rec := sendAuth(h, "Bearer wrong-token", "", nil)
		require.Equalf(t, http.StatusUnauthorized, rec.Code, "第 %d 次失败应为 401", i)
		require.Emptyf(t, rec.Header().Get("Retry-After"), "第 %d 次失败不应带 Retry-After", i)
	}
	require.Equal(t, int64(0), hits.Load())
}

// TestAuthLimiter_TenthRequestBlockedBeforeCredentials：9 次失败后，第 10 次请求
// **即使带正确 token**也在凭据校验前返回 429 + Retry-After（约束 7）。
// 逆改点：把 429 放到凭据校验之后 → 正确 token 用例变 204；Retry-After 缺失 → 空头断言红。
func TestAuthLimiter_TenthRequestBlockedBeforeCredentials(t *testing.T) {
	t.Parallel()
	guard := newLimitGuard(t, func() time.Time { return authFixedNow }, nil)
	var hits atomic.Int64
	h := guard.Wrap(passingHandler(&hits))

	for i := 0; i < 9; i++ {
		require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "", nil).Code)
	}
	rec := sendAuth(h, "Bearer "+testToken, "", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "60", rec.Header().Get("Retry-After"))
	require.Equal(t, int64(0), hits.Load(), "被限速的请求不得进入处理函数")

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeAuthRateLimited, env.Code)
	require.Equal(t, messageAuthRateLimited, env.Message)
	require.Equal(t, authFixedNow.UTC().Format(time.RFC3339), env.Timestamp)
}

// TestAuthLimiter_SuccessDoesNotResetWindow：中途成功认证不重置计数——
// 5 次失败 + 1 次成功后，再失败 4 次即进入限速（约束 7）。
// 逆改点：成功时调用 Reset → 第 10 次请求变 401。
func TestAuthLimiter_SuccessDoesNotResetWindow(t *testing.T) {
	t.Parallel()
	guard := newLimitGuard(t, func() time.Time { return authFixedNow }, nil)
	var hits atomic.Int64
	h := guard.Wrap(passingHandler(&hits))

	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "", nil).Code)
	}
	require.Equal(t, http.StatusNoContent, sendAuth(h, "Bearer "+testToken, "", nil).Code)
	for i := 0; i < 4; i++ {
		require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "", nil).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, sendAuth(h, "Bearer "+testToken, "", nil).Code)
	require.Equal(t, int64(1), hits.Load())
}

// TestAuthLimiter_WindowExpiryRestoresAccess：窗口到期后计数清零并恢复 401；
// 新一轮计数重新开始（约束 7）。
// 逆改点：窗口不做过期判定 → 到期后的请求仍是 429。
func TestAuthLimiter_WindowExpiryRestoresAccess(t *testing.T) {
	t.Parallel()
	clock := &authTestClock{current: authFixedNow}
	guard := newLimitGuard(t, clock.Now, nil)
	var hits atomic.Int64
	h := guard.Wrap(passingHandler(&hits))

	for i := 0; i < 9; i++ {
		require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "", nil).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, sendAuth(h, "", "", nil).Code)

	clock.Advance(60 * time.Second)
	require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "", nil).Code,
		"窗口到期后应恢复凭据判定")
	for i := 0; i < 8; i++ {
		require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "", nil).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, sendAuth(h, "", "", nil).Code,
		"新窗口同样在 9 次失败后限速")
}

// TestAuthLimiter_RetryAfterRoundsUp：Retry-After 是窗口剩余秒数向上取整
// （29.8 秒余量 → 30；0.5 秒余量 → 1）。
// 逆改点：向下取整或负数/零 → 头部读数变 29 / 0。
func TestAuthLimiter_RetryAfterRoundsUp(t *testing.T) {
	t.Parallel()
	clock := &authTestClock{current: authFixedNow}
	guard := newLimitGuard(t, clock.Now, nil)
	h := guard.Wrap(passingHandler(new(atomic.Int64)))

	for i := 0; i < 9; i++ {
		require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "", nil).Code)
	}
	clock.Advance(30200 * time.Millisecond)
	rec := sendAuth(h, "", "", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "30", rec.Header().Get("Retry-After"))

	clock.Advance(29300 * time.Millisecond) // 距窗口起点 59.5 秒，余量 0.5 秒
	rec = sendAuth(h, "", "", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "1", rec.Header().Get("Retry-After"))
}

// TestAuthLimiter_HostAndOriginRejectsNotCounted：Host / Origin 的 403 发生在
// 认证之前，不消耗失败配额——否则攻击者可用错误 Host 把正常来源打进 429（注 14）。
// 逆改点：把 Host 拒绝也计入 → 20 次 Host 错误后的正常失败立刻 429。
func TestAuthLimiter_HostAndOriginRejectsNotCounted(t *testing.T) {
	t.Parallel()
	guard := newLimitGuard(t, func() time.Time { return authFixedNow }, func(cfg *AdminAuthConfig) {
		cfg.AllowedOrigins = []string{"http://127.0.0.1:8080"}
	})
	var hits atomic.Int64
	h := guard.Wrap(passingHandler(&hits))

	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
		req.Host = "evil.example:8080"
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code)
	}
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Origin", "http://evil.example")
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code)
	}
	for i := 0; i < 9; i++ {
		require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "", nil).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, sendAuth(h, "Bearer wrong-token", "", nil).Code)
	require.Equal(t, int64(0), hits.Load())
}

// TestAuthLimiter_LocalTrustRejectionsCounted：local_trust 的 403 计入配额——
// 9 次缺信任头 403 后，第 10 次即使条件齐备也 429（约束 7）。
// 逆改点：local_trust 403 不计入 → 第 10 次变 204。
func TestAuthLimiter_LocalTrustRejectionsCounted(t *testing.T) {
	t.Parallel()
	guard := newLimitGuard(t, func() time.Time { return authFixedNow }, func(cfg *AdminAuthConfig) {
		cfg.Mode = "local_trust"
		cfg.Token = ""
	})
	var hits atomic.Int64
	h := guard.Wrap(passingHandler(&hits))

	for i := 0; i < 9; i++ {
		require.Equal(t, http.StatusForbidden, sendAuth(h, "", "127.0.0.1:52000", nil).Code)
	}
	rec := sendAuth(h, "", "127.0.0.1:52000", map[string]string{localTrustHeader: "1"})
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, int64(0), hits.Load())
}

// TestAuthLimiter_KeyedByIPNotPort：限速按来源 IP 计数，不按 ip:port——
// 同一 IP 换端口继续累计；不同 IP 互不影响（约束 7）。
// 逆改点：用完整 RemoteAddr 做键 → 换端口后的请求变 401，抵消限速。
func TestAuthLimiter_KeyedByIPNotPort(t *testing.T) {
	t.Parallel()
	guard := newLimitGuard(t, func() time.Time { return authFixedNow }, nil)
	h := guard.Wrap(passingHandler(new(atomic.Int64)))

	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "198.51.100.7:40001", nil).Code)
	}
	for i := 0; i < 4; i++ {
		require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "198.51.100.7:40002", nil).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, sendAuth(h, "", "198.51.100.7:40003", nil).Code)
	require.Equal(t, http.StatusUnauthorized, sendAuth(h, "Bearer wrong-token", "198.51.100.8:40004", nil).Code,
		"不同来源 IP 的计数相互独立")
}

// TestAuthLimiter_TrustedProxyAttributionKey：可信代理转发时按归因后的客户端 IP
// 计数；非可信对端自带 XFF 不改变计数键（约束 6 / 7）。
// 逆改点：用直接对端 IP 做键 → 换代理后的请求变 401；信任伪造 XFF → 非可信用例被 429。
func TestAuthLimiter_TrustedProxyAttributionKey(t *testing.T) {
	t.Parallel()
	guard := newLimitGuard(t, func() time.Time { return authFixedNow }, func(cfg *AdminAuthConfig) {
		cfg.TrustedProxies = []string{"10.0.0.0/8"}
	})
	h := guard.Wrap(passingHandler(new(atomic.Int64)))

	for i := 0; i < 9; i++ {
		rec := sendAuth(h, "Bearer wrong-token", "10.0.0.1:5555", map[string]string{"X-Forwarded-For": "203.0.113.9"})
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	rec := sendAuth(h, "Bearer "+testToken, "10.0.0.2:6666", map[string]string{"X-Forwarded-For": "203.0.113.9"})
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "同一归因来源经不同可信代理仍累计")

	rec = sendAuth(h, "Bearer "+testToken, "10.0.0.2:6667", map[string]string{"X-Forwarded-For": "203.0.113.10"})
	require.Equal(t, http.StatusNoContent, rec.Code, "不同归因来源不受影响")

	rec = sendAuth(h, "Bearer wrong-token", "192.0.2.50:7000", map[string]string{"X-Forwarded-For": "203.0.113.9"})
	require.Equal(t, http.StatusUnauthorized, rec.Code, "非可信对端的 XFF 只是自述，不改变计数键")
}

// TestAuthLimiter_ResetClearsCounter：Reset 显式清除来源计数（仅测试与后续管理入口使用）。
// 逆改点：Reset 不生效 → 清除后仍 429。
func TestAuthLimiter_ResetClearsCounter(t *testing.T) {
	t.Parallel()
	limiter := newAuthLimiter(10, 60*time.Second, func() time.Time { return authFixedNow })
	for i := 0; i < 8; i++ {
		_, limited := limiter.Fail("203.0.113.9")
		require.Falsef(t, limited, "第 %d 次失败不应进入限速", i+1)
	}
	_, limited := limiter.Fail("203.0.113.9")
	require.True(t, limited, "第 9 次失败后来源进入限速")
	_, limited = limiter.blocked("203.0.113.9")
	require.True(t, limited)

	limiter.Reset("203.0.113.9")
	_, limited = limiter.blocked("203.0.113.9")
	require.False(t, limited)
}

// TestAuthLimiter_ConcurrentFailures（011-race 必需名）：并发失败不丢计数、
// 不产生竞态；风暴后来源必然处于限速状态（约束 7）。
// 逆改点：计数不加锁 → -race 报告数据竞争。
func TestAuthLimiter_ConcurrentFailures(t *testing.T) {
	t.Parallel()
	guard := newLimitGuard(t, func() time.Time { return authFixedNow }, nil)
	var hits atomic.Int64
	h := guard.Wrap(passingHandler(&hits))

	const workers = 32
	var wg sync.WaitGroup
	statuses := make(chan int, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			statuses <- sendAuth(h, "Bearer wrong-token", "", nil).Code
		}()
	}
	wg.Wait()
	close(statuses)

	unauthorized, rateLimited := 0, 0
	for code := range statuses {
		switch code {
		case http.StatusUnauthorized:
			unauthorized++
		case http.StatusTooManyRequests:
			rateLimited++
		default:
			t.Fatalf("并发失败出现意外状态码 %d", code)
		}
	}
	require.Equal(t, workers, unauthorized+rateLimited)
	require.Greater(t, unauthorized, 0, "并发窗口内至少允许一次凭据判定")
	require.Equal(t, int64(0), hits.Load())
	require.Equal(t, http.StatusTooManyRequests, sendAuth(h, "Bearer "+testToken, "", nil).Code,
		"并发失败后来源必须已进入限速")
}

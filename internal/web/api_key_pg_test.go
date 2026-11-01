//go:build integration

package web

// 本文件是 C-07「创建响应只回一次明文」的**出站面**判卷（`us16-pg` 具名用例
// `TestAPIKey_CreateReturnsSecretOnce`）。契约把明文限定在签发视图 `data.key` 这
// 一个字段上；除此之外的每个读取面——吊销回执、调用方列表、管理操作列表与详情——
// 都必须拿不到它，而再签一把拿到的必须是**另一个值**。
//
// 装配走生产 `callers.CallerAdmin`（真 PG 上的 Key / 配额 / 管理操作三段存储），
// 不换进程内假端口：假端口能让"装配根本没接上 Key 存储"这类回归静默通过。
// 整库与日志的扫描面由 `internal/app` 的 `TestLogsAndAudit_RedactKey` 判，本文件
// 只判 HTTP 响应；两边都留，是因为"库里干净"与"没从任何端点漏出去"是两件事。
// The plaintext lives in exactly one response field; every other read face is checked.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// newAPIKeyServer 装配带真调用方管理端的服务器，并把 ERROR 日志收进缓冲区。
// 管理面把 5xx 的底层原因只写日志、不回显响应（`writeCallerAdminError` 的约定），
// 断言失败时没有这份缓冲就只能看到一个 `INTERNAL`。
// 认证闸门必须是真实的管理员 token：管理操作要落 `management_operations`，而
// `auth_method` 列有 CHECK 约束——不装配闸门时处理函数拿到零值 actor（Method 为空），
// 预写会被库直接拒绝。这条是生产装配的既有前提，不是本用例的额外条件。
// The buffer holds the ERROR lines the admin face keeps out of responses; the guard is
// required because the operation pre-write persists the actor's auth method.
func newAPIKeyServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	db := pgtest.NewTestDB(t)
	management := postgres.NewManagementStore(db)
	guard, err := NewAuthGuardWithPrincipals(AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          testToken,
		AllowedHosts:   []string{"127.0.0.1:8080", "localhost:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow }, nil)
	require.NoError(t, err)
	var logs bytes.Buffer
	srv := newTestServer(t, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		o.AdminAuth = guard
		o.Callers = callers.NewCallerAdmin(
			postgres.NewKeyStore(db),
			postgres.NewQuotaStore(db),
			management,
		)
		// 管理操作读面与审计同源：路由回显判据要读的就是签发时写下的那一行。
		o.Operations = management
	})
	return srv, &logs
}

// adminTokenRequest 造一个带管理员 token 的管理面请求：夹具装了真实闸门，不带
// 凭据的请求在闸门处就 401，到不了处理函数。
// adminTokenRequest attaches the admin token every management call needs.
func adminTokenRequest(method, path, body string) *http.Request {
	return withBearer(adminRequest(method, path, body), testToken)
}

// issuedKeyResponse 是签发视图里本用例关心的三个字段。
type issuedKeyResponse struct {
	keyID     string
	keyPrefix string
	key       string
}

// issueCallerKey 走管理面签发一把 Key。
func issueCallerKey(t *testing.T, srv *Server, callerID, body string) issuedKeyResponse {
	t.Helper()
	rec := serveAdmin(srv, adminTokenRequest(http.MethodPost, "/api/v1/callers/"+callerID+"/keys", body))
	require.Equal(t, http.StatusOK, rec.Code, "签发失败：%s", rec.Body.String())
	var env struct {
		Data struct {
			KeyID     string `json:"key_id"`
			KeyPrefix string `json:"key_prefix"`
			Key       string `json:"key"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return issuedKeyResponse{keyID: env.Data.KeyID, keyPrefix: env.Data.KeyPrefix, key: env.Data.Key}
}

// TestAPIKey_CreateReturnsSecretOnce 判明文只在签发响应出现一次。
//
// 逆改点：给吊销回执或列表视图补上明文字段 → 对应 NotContains 红；让签发复用同一
// 把明文（"每次返回同一把"）→ 两把不同的断言红；把 `key_prefix` 直接写成整把明文
// → 前缀长度断言红。
// Reverting any one of the read faces’ projections turns exactly one assertion red.
func TestAPIKey_CreateReturnsSecretOnce(t *testing.T) {
	srv, logs := newAPIKeyServer(t)

	rec := serveAdmin(srv, adminTokenRequest(http.MethodPost, "/api/v1/callers",
		`{"caller_id":"cal_01J00000000000000000000000","name":"Key 用例"}`))
	require.Equal(t, http.StatusOK, rec.Code, "建调用方失败：%s｜ERROR 日志：%s",
		rec.Body.String(), logs.String())

	first := issueCallerKey(t, srv, "cal_01J00000000000000000000000", `{"scopes":["sessions:read","sessions:write"]}`)
	require.NotEmpty(t, first.key, "签发响应必须带明文 Key——它是唯一的合法出口")
	require.NotEmpty(t, first.keyID)
	require.Contains(t, first.key, first.keyPrefix, "前缀是明文的一段")
	// 长度判据用 assert：它和下面四面回显判据是同一条性质的多个观察点，逆改要一次
	// 看到"前缀列存了整把明文"会在几个面同时漏出去，而不是被第一条挡住。
	// Non-fatal so one mutation reports every face the over-long prefix leaks through.
	assert.Less(t, len(first.keyPrefix), len(first.key), "前缀必须短于明文")

	// ① 吊销回执：结构上没有明文字段，带上展示前缀。
	rec = serveAdmin(srv, adminTokenRequest(http.MethodPost,
		"/api/v1/callers/cal_01J00000000000000000000000/keys/"+first.keyID+"/revoke", ""))
	require.Equal(t, http.StatusOK, rec.Code, "吊销失败：%s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), first.key, "吊销回执不得回显明文")
	assert.Contains(t, rec.Body.String(), first.keyPrefix, "阳性对照：回执必须带展示前缀")

	// ② 再签一把拿到的是另一个值：明文不是"每次返回同一串"。
	second := issueCallerKey(t, srv, "cal_01J00000000000000000000000", `{"scopes":["sessions:read"]}`)
	require.NotEqual(t, first.key, second.key, "两次签发不得给出同一个明文")

	// ③ 调用方列表：只有摘要，没有明文。
	rec = serveAdmin(srv, adminTokenRequest(http.MethodGet, "/api/v1/callers", ""))
	require.Equal(t, http.StatusOK, rec.Code, "调用方列表失败：%s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), first.key)
	assert.NotContains(t, rec.Body.String(), second.key)

	// ④ 管理操作列表与详情：C-07 点名的回显症状就在这一面。
	// The operations face is the one C-07 names as the leak symptom.
	rec = serveAdmin(srv, adminTokenRequest(http.MethodGet, "/api/v1/operations", ""))
	require.Equal(t, http.StatusOK, rec.Code, "管理操作列表失败：%s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), first.key)
	assert.NotContains(t, rec.Body.String(), second.key)

	var list struct {
		Data []struct {
			OperationID string `json:"operation_id"`
			Action      string `json:"action"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.NotEmpty(t, list.Data, "阳性对照：签发必须留下管理操作行")

	issueOps := 0
	for _, op := range list.Data {
		detail := serveAdmin(srv, adminTokenRequest(http.MethodGet, "/api/v1/operations/"+op.OperationID, ""))
		require.Equal(t, http.StatusOK, detail.Code, "操作详情失败：%s", detail.Body.String())
		assert.NotContains(t, detail.Body.String(), first.key,
			"操作详情不得回显明文（action=%s）", op.Action)
		assert.NotContains(t, detail.Body.String(), second.key,
			"操作详情不得回显明文（action=%s）", op.Action)
		if op.Action == "caller.key.issue" {
			issueOps++
		}
	}
	require.GreaterOrEqual(t, issueOps, 2, "阳性对照：两次签发各留一条 caller.key.issue")
}

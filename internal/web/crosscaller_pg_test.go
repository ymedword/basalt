//go:build integration

package web

// 本文件是 C-28 四条具名用例的**真实链路**落地（`us16-pg` 判卷）：
// `TestSession_CrossCallerNotFound`、`TestScope_DefaultDeny`、
// `TestRun_CrossCallerSessionNotFound`、`TestRun_CancelCrossCallerNotFound`。
//
// 与 `middleware_business_auth_test.go` 的分工：那边用假端口判中间件的分支（401 /
// 403 文案、端口是否被询问），这边判"真正签发出去的 Key 在真实库里查得到、归属真的
// 按 `sessions.caller_id` 收窄"——假端口可以让"库里根本没有这把 Key"这类回归静默通过。
// 链路：隔离 schema（`pgtest`）→ 真 `KeyIssuer` 签发 → 真 `Authenticator` 认证 →
// 真 `SessionStore` → 真 handler 链。运行入口用记录型替身：本文件的判据是"拒绝是否
// **先于**任何运行副作用"，替身能证明零调用，真运行反而看不出这一点。
// The production chain up to the run entry: real issuance, real lookup, real session
// store. The run entry is a recorder because the verdict is "rejected before any side
// effect", which a recorder can prove and a real run cannot.
//
// 差异裁决注（与模块卡 §4.3 的代码片段的唯一差异）：卡里的
// `TestSession_CrossCallerNotFound` 打 `GET /sessions/{id}/messages`——该路由在
// `docs/contracts/openapi.yaml` 与本包路由表里都不存在（契约只有 `GET /sessions`、
// `GET/POST/DELETE /sessions/{id}` 与精确读取 `/sessions/{session_id}/messages/{message_id}`）。
// 详情行 `GET /sessions/{id}` 是**管理面**（零值 surface），caller key 一律 403。
// 因此本用例判当前契约下等价的强制面：业务列表按 `principal.CallerID` 收窄（含双向
// 阳性对照），且详情行不接受 caller key、拒绝时不回传他人字段。精确读取的跨调用方
// 由 `TestHistory_ExactReadCrossCallerNotFound` 单独判卷，不在这里重复。
// The card's snippet targets a route the contract does not have; the equivalent enforced
// surface is the caller-scoped list plus the management-only detail row.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// recordingRunService 记录运行入口与取消端口的调用；本文件靠"零调用"证明拒绝先于
// 副作用。返回值只求形状合法：这些用例不判运行本身。
// recordingRunService records both ports; the verdicts read the counts, not the results.
type recordingRunService struct {
	mu      sync.Mutex
	reqs    []core.RunRequest
	cancels []string
}

// Run 记录一次运行请求并回一个已完成的结果。
func (r *recordingRunService) Run(_ context.Context, req core.RunRequest) (core.RunResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return core.RunResult{RunID: "run-recorded", Status: core.StatusCompleted}, nil
}

// Archive 不参与本文件判据。
func (r *recordingRunService) Archive(context.Context, string) error { return nil }

// ListRuns 不参与本文件判据。
func (r *recordingRunService) ListRuns(context.Context, core.RunFilter) ([]core.RunDetail, error) {
	return nil, nil
}

// GetRun 不参与本文件判据。
func (r *recordingRunService) GetRun(context.Context, string) (core.RunDetail, error) {
	return core.RunDetail{}, nil
}

// Cancel 记录一次取消并回"已取消"的形态。
func (r *recordingRunService) Cancel(_ context.Context, runID string) (core.RunDetail, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels = append(r.cancels, runID)
	return core.RunDetail{RunRecord: core.RunRecord{RunID: runID}, Status: core.StatusCancelled}, nil
}

// requests 返回运行入口收到的请求副本。
func (r *recordingRunService) requests() []core.RunRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.RunRequest(nil), r.reqs...)
}

// cancels 返回取消端口收到的运行标识副本。
func (r *recordingRunService) cancelled() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cancels...)
}

// crossCallerEnv 是真实链路夹具：隔离 schema、真签发器、真认证器、真会话仓储。
// crossCallerEnv wires the isolated schema, the real issuer, authenticator and store.
type crossCallerEnv struct {
	t        *testing.T
	db       *postgres.DB
	srv      *Server
	issuer   *callers.KeyIssuer
	runs     *recordingRunService
	nowMilli int64
}

// newCrossCallerEnv 装配夹具：短路径只签发与认证，不装配运行面。
// newCrossCallerEnv builds the fixture; the run side is the recorder only.
func newCrossCallerEnv(t *testing.T) *crossCallerEnv {
	t.Helper()
	db := pgtest.NewTestDB(t)
	keys := postgres.NewKeyStore(db)
	guard, err := NewAuthGuardWithPrincipals(AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          testToken,
		AllowedHosts:   []string{"127.0.0.1:8080", "localhost:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow }, callers.NewAuthenticator(keys, nil))
	require.NoError(t, err)

	env := &crossCallerEnv{
		t:        t,
		db:       db,
		issuer:   callers.NewKeyIssuer(keys, postgres.NewManagementStore(db)),
		runs:     &recordingRunService{},
		nowMilli: authFixedNow.UnixMilli(),
	}
	env.srv = newTestServer(t, func(o *Options) {
		o.AdminAuth = guard
		o.Sessions = postgres.NewSessionStore(db)
		o.Runs = env.runs
	})
	return env
}

// issueKey 先建调用方行（`caller_keys` 有外键），再走真实签发器拿明文。
// issueKey seeds the caller row and issues a real key through the production issuer.
func (e *crossCallerEnv) issueKey(callerID string, scopes ...string) callers.IssuedKey {
	e.t.Helper()
	_, err := e.db.ExecContext(e.t.Context(), `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, '', 'active', $2)`, callerID, e.nowMilli)
	require.NoError(e.t, err, "签发前调用方必须存在（caller_keys 有外键）")
	actor := core.Actor{
		Subject: core.ActorSubjectLocalTrust,
		Method:  core.AuthMethodLocalTrust,
		Source:  "127.0.0.1:1",
	}
	issued, err := e.issuer.Issue(e.t.Context(), actor, callers.IssueRequest{
		CallerID: callerID,
		Scopes:   scopes,
	})
	require.NoError(e.t, err, "签发失败说明作用域名或调用方行不合规")
	return issued
}

// createSession 以该 Key 的身份走 HTTP 建会话，返回 `session_id`。
// createSession creates one session over HTTP as the key's caller.
func (e *crossCallerEnv) createSession(secret, userID string) string {
	e.t.Helper()
	body := `{"agent":"ops-agent","user_id":"` + userID + `"}`
	rec := serveAdmin(e.srv, withBearer(adminRequest(http.MethodPost, "/api/v1/sessions", body), secret))
	require.Equal(e.t, http.StatusOK, rec.Code, "建会话失败：%s", rec.Body.String())
	var env struct {
		Data struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.NotEmpty(e.t, env.Data.SessionID)
	return env.Data.SessionID
}

// listSessions 返回该 Key 在业务列表里能看到的会话标识。
// listSessions returns the session ids the key can see on the business list.
func (e *crossCallerEnv) listSessions(secret string) []string {
	e.t.Helper()
	rec := serveAdmin(e.srv, withBearer(adminRequest(http.MethodGet, "/api/v1/sessions", ""), secret))
	require.Equal(e.t, http.StatusOK, rec.Code, "会话列表拒绝：%s", rec.Body.String())
	var env struct {
		Data []struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &env))
	ids := make([]string, 0, len(env.Data))
	for _, row := range env.Data {
		ids = append(ids, row.SessionID)
	}
	return ids
}

// TestSession_CrossCallerNotFound 判会话读面按归属收窄：两个调用方各建一个会话，
// 彼此的列表里都看不到对方；详情行不接受 caller key，且拒绝时不回传他人字段。
//
// 逆改点：列表不设 `filter.CallerID`（或改用请求参数里的 caller_id）→ 双向
// NotContains 红，两个调用方互相看见；把详情行登记成业务面且不校验归属 → 403 断言
// 变 200 且回传 `u-b`，第二条红。
// Reverting the caller narrowing turns the two NotContains assertions red.
func TestSession_CrossCallerNotFound(t *testing.T) {
	env := newCrossCallerEnv(t)
	keyA := env.issueKey("caller-a", core.ScopeSessionsRead, core.ScopeSessionsWrite)
	keyB := env.issueKey("caller-b", core.ScopeSessionsRead, core.ScopeSessionsWrite)
	sessA := env.createSession(keyA.Secret, "u-a")
	sessB := env.createSession(keyB.Secret, "u-b")
	require.NotEqual(t, sessA, sessB, "夹具必须造出两个不同的会话")

	// 双向阳性对照：各自看得见自己、看不见对方——只判单向的话，"列表恒空"
	// 也能让 NotContains 变绿。
	// Both directions, so an always-empty list cannot satisfy the verdict.
	listA := env.listSessions(keyA.Secret)
	require.Contains(t, listA, sessA, "阳性对照：调用方必须看得见自己的会话")
	assert.NotContains(t, listA, sessB, "调用方只能看到自己的会话")
	listB := env.listSessions(keyB.Secret)
	require.Contains(t, listB, sessB, "阳性对照：反方向同样成立")
	// 各条拒绝判据用 assert：逆改后要一次看到"哪几条性质同时破了"，而不是被第一条
	// 挡住。Sibling assertions are non-fatal so one mutation reports every breach.
	assert.NotContains(t, listB, sessA)

	rec := serveAdmin(env.srv, withBearer(
		adminRequest(http.MethodGet, "/api/v1/sessions/"+sessB, ""), keyA.Secret))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"会话详情行是管理面：caller key 一律拒绝，不得回传他人会话")
	assert.NotContains(t, rec.Body.String(), "u-b", "拒绝不得回传他人会话字段")
}

// TestScope_DefaultDeny 判作用域默认拒绝：只持 `sessions:read` 的 Key 能读不能写，
// 也不能调用 Agent；换一把带写 scope 的 Key 后同样的请求放行。
//
// 逆改点：业务行不声明 scope（或未命中时默认放行）→ 建档与 invoke 两个 403 变 200，
// 且运行入口被触达；把 scope 判定写成"有任意 scope 即可"→ read-only 的 invoke 仍 403，
// 但"缺 sessions:write"那一条会漏过。
// Reverting default-deny turns the two rejections into 200s and touches the run entry.
func TestScope_DefaultDeny(t *testing.T) {
	env := newCrossCallerEnv(t)
	readOnly := env.issueKey("caller-read", core.ScopeSessionsRead)

	rec := serveAdmin(env.srv, withBearer(adminRequest(http.MethodPost, "/api/v1/sessions",
		`{"agent":"ops-agent","user_id":"u-1"}`), readOnly.Secret))
	assert.Equal(t, http.StatusForbidden, rec.Code, "缺 sessions:write 必须拒绝，不得默认放行")
	assert.Equal(t, codeForbidden, decodeAuthEnvelope(t, rec).Code)

	rec = serveAdmin(env.srv, withBearer(adminRequest(http.MethodGet, "/api/v1/sessions", ""), readOnly.Secret))
	require.Equal(t, http.StatusOK, rec.Code, "阳性对照：已声明的 sessions:read 正常放行")

	rec = serveAdmin(env.srv, withBearer(adminRequest(http.MethodPost,
		"/api/v1/agents/ops-agent/invoke", `{"message":"hi"}`), readOnly.Secret))
	assert.Equal(t, http.StatusForbidden, rec.Code, "缺 agents:invoke:ops-agent 必须拒绝")
	assert.Empty(t, env.runs.requests(), "作用域拒绝不得触达运行入口")

	writer := env.issueKey("caller-write", core.ScopeSessionsRead, core.ScopeSessionsWrite)
	rec = serveAdmin(env.srv, withBearer(adminRequest(http.MethodPost, "/api/v1/sessions",
		`{"agent":"ops-agent","user_id":"u-2"}`), writer.Secret))
	require.Equal(t, http.StatusOK, rec.Code, "阳性对照：具备 scope 的 Key 必须放行")
}

// TestRun_CrossCallerSessionNotFound 判"带他人 session_id 发起 Run"：归属校验先于
// 任何运行副作用，跨调用方与不存在共用同一 404；归属方自己写同一会话则正常起跑，
// 且运行入口收到的身份就是归属方。
//
// 逆改点：先起跑再校验归属（或完全不校验）→ 404 变 200 且运行入口收到一条请求；
// 把 `req.Principal` 换成请求体里的 caller_id → 阳性对照的身份断言红。
// Reverting the ownership-first order turns the 404 into a 200 and records a run.
func TestRun_CrossCallerSessionNotFound(t *testing.T) {
	env := newCrossCallerEnv(t)
	keyB := env.issueKey("caller-b", core.ScopeSessionsRead, core.ScopeSessionsWrite)
	sessB := env.createSession(keyB.Secret, "u-b")
	keyA := env.issueKey("caller-a", core.ScopeSessionsRead, core.ScopeSessionsWrite,
		core.AgentInvokeScopePrefix+"ops-agent")

	rec := serveAdmin(env.srv, withBearer(adminRequest(http.MethodPost,
		"/api/v1/sessions/"+sessB+"/messages", `{"message":"hi"}`), keyA.Secret))
	assert.Equal(t, http.StatusNotFound, rec.Code, "不得凭 session_id 写入他人会话")
	assert.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code,
		"跨调用方与不存在共用同一 404，不泄露会话存在")
	// 判据的重心在"先于副作用"：状态码对了但仍跑了一次，等于拒绝发生在错误的位置。
	// The decisive property is order, so the zero-side-effect verdict carries the weight.
	assert.Empty(t, env.runs.requests(), "拒绝必须先于运行副作用")

	rec = serveAdmin(env.srv, withBearer(adminRequest(http.MethodPost,
		"/api/v1/sessions/"+sessB+"/messages", `{"message":"hi"}`), keyB.Secret))
	require.Equal(t, http.StatusOK, rec.Code, "阳性对照：归属方写自己的会话必须放行")
	reqs := env.runs.requests()
	require.Len(t, reqs, 1)
	require.Equal(t, "caller-b", reqs[0].Principal.CallerID, "运行身份是归属方，不是路径参数")
	require.Equal(t, sessB, reqs[0].SessionID)
}

// TestRun_CancelCrossCallerNotFound 判"他人 Run 不得被取消"：取消行是管理面
// （契约 `/runs/{run_id}/cancel` 没有 callerKey security），持全部业务作用域的
// caller key 也必须被挡在取消端口之前——运行入口零调用、响应不带运行状态；
// 管理员 token 走同一路径确实取消，证明拒绝来自面而不是路由坏了。
//
// 差异裁决注：模块卡 C-28 把取消列在"跨调用方统一 404"里，但契约与本仓路由表把
// `/runs/{id}/cancel` 定为管理面（caller key → 403）。被强制的性质更强也更准确：
// **任何调用方都取消不了任何 Run**，他人与自己的都不行。若日后契约给调用方开放
// 取消，这里必须改成归属 404 判据——那是契约变更，不在本 US 内自裁。
//
// 逆改点：把取消行翻成业务面且不校验 `runs.caller_id` → 第一个断言变 200 且
// 取消端口收到该 id（"他人 Run 被取消且返回 200"）。
// Flipping the row to the business face without an owner check turns the 403 into a
// 200 and records the foreign run id on the cancel port.
func TestRun_CancelCrossCallerNotFound(t *testing.T) {
	env := newCrossCallerEnv(t)
	keyA := env.issueKey("caller-a", core.ScopeSessionsRead, core.ScopeSessionsWrite,
		core.AgentInvokeScopePrefix+"ops-agent")
	const foreignRun = "run-owned-by-caller-b"

	rec := serveAdmin(env.srv, withBearer(adminRequest(http.MethodPost,
		"/api/v1/runs/"+foreignRun+"/cancel", ""), keyA.Secret))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"取消是管理面：caller key 不得取消任何运行（含他人）")
	assert.Empty(t, env.runs.cancelled(), "拒绝必须发生在取消端口之前")
	assert.NotContains(t, rec.Body.String(), `"status"`, "拒绝不得回传他人运行状态")

	rec = serveAdmin(env.srv, withBearer(adminRequest(http.MethodPost,
		"/api/v1/runs/"+foreignRun+"/cancel", ""), testToken))
	require.Equal(t, http.StatusOK, rec.Code, "阳性对照：管理员 token 走同一路径必须能取消")
	require.Equal(t, []string{foreignRun}, env.runs.cancelled())
}

package web

// 本文件覆盖业务面认证中间件（模块卡 §3.1.7 / §4.7 R11 / T025）：业务端点要求
// API Key（admin token 映射内置管理员）、管理面只接受 admin token（有效 caller key
// → 403）、作用域默认拒绝、会话归属跨调用方 404，以及认证面的启动自检。
// 判据面口径：401 不区分"缺失 / 无效"，403 不解释命中哪条规则，跨调用方与"不存在"
// 共用同一 404 文案。
// These tests pin the business-surface middleware: caller keys and the admin token on
// business rows, admin-token-only management rows, default-deny scopes, cross-caller
// 404s, and the startup surface check.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errTestUnauthorized 是假调用方端口的失败哨兵：中间件只判"错误非 nil"，不比对
// 错误文本（真实实现统一返回 ErrUnauthorized）。
// errTestUnauthorized stands in for the real port's single ErrUnauthorized.
var errTestUnauthorized = errors.New("test: unauthorized")

// fakeCallerKeys 是 `PrincipalAuthenticator` 的假端口：按呈现的凭据逐字查表。
// 查不到返回 errTestUnauthorized；calls 供"管理面探测"用例断言端口确被询问。
// fakeCallerKeys resolves credentials from a table and counts lookups.
type fakeCallerKeys struct {
	byCredential map[string]core.Principal
	calls        int
}

// Authenticate 按凭据查表；未登记的凭据一律失败。
// Authenticate looks the credential up and fails for anything unregistered.
func (f *fakeCallerKeys) Authenticate(_ context.Context, presented string) (core.Principal, error) {
	f.calls++
	if principal, ok := f.byCredential[presented]; ok {
		return principal, nil
	}
	return core.Principal{}, errTestUnauthorized
}

// callerPrincipal 构造合法调用方身份（KeyID 由归属派生，便于断言传导）。
// callerPrincipal builds a valid caller identity.
func callerPrincipal(callerID string, scopes ...string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		KeyID:    "key_" + callerID,
		Scopes:   scopes,
		Actor:    core.ActorCaller,
	}
}

// businessGuard 构造 token 模式、接入调用方端口的真实认证闸门（回环 Host 三元组 +
// 固定时钟，与 testAdminAuth 同口径）。
// businessGuard builds the real guard with the caller-key port attached.
func businessGuard(t *testing.T, keys PrincipalAuthenticator) *authGuard {
	t.Helper()
	guard, err := NewAuthGuardWithPrincipals(AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          testToken,
		AllowedHosts:   []string{"127.0.0.1:8080", "localhost:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow }, keys)
	require.NoError(t, err)
	return guard
}

// newBusinessServer 装配业务面用例的服务器，并把缺省假件交回给用例断言副作用。
// newBusinessServer wires the business tests and hands back the default fakes.
func newBusinessServer(t *testing.T, guard AdminAuth) (*Server, *fakeRunService, *fakeSessionStore) {
	t.Helper()
	var runs *fakeRunService
	var sessions *fakeSessionStore
	srv := newTestServer(t, func(o *Options) {
		runs = o.Runs.(*fakeRunService)
		sessions = o.Sessions.(*fakeSessionStore)
		o.AdminAuth = guard
	})
	return srv, runs, sessions
}

// TestBusinessAuth_MissingCredentialIsUnauthorized 判业务端点默认拒绝：无凭据 →
// 401 固定文案，且**零创建**（会话仓储没有被调用）。
// 逆改点：业务行不套认证中间件 → 200 且 getOrCreateCalls 非 0。
func TestBusinessAuth_MissingCredentialIsUnauthorized(t *testing.T) {
	t.Parallel()
	guard := businessGuard(t, &fakeCallerKeys{})
	srv, runs, sessions := newBusinessServer(t, guard)

	rec := serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeUnauthorized, env.Code)
	require.Equal(t, messageUnauthorized, env.Message, "401 不得区分缺失与无效")
	require.Zero(t, sessions.getOrCreateCallCount(), "被拒请求不得触达会话仓储")
	require.Empty(t, runs.requests())
}

// TestBusinessAuth_InvalidCredentialFailsClosed 判坏凭据与坏端口的失败关闭：未登记
// 的 Key → 401；端口返回"无错误但身份非法"（冒充 local-admin）同样 401——认证层
// **不得**因为端口说成功就放行。
// 逆改点：去掉 `Valid()` 兜底 → 冒充身份被认证层接受，坏凭据的 401 变 403（只剩
// 作用域兜底），本用例在 `require.Equal(401)` 处红。
func TestBusinessAuth_InvalidCredentialFailsClosed(t *testing.T) {
	t.Parallel()
	impersonator := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: core.LocalAdminCallerID,
		KeyID:    "key_bad",
		Scopes:   []string{core.ScopeSessionsWrite},
		Actor:    core.ActorCaller,
	}
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{"bad-secret": impersonator}}
	guard := businessGuard(t, keys)
	srv, runs, _ := newBusinessServer(t, guard)

	for _, credential := range []string{"unknown-secret", "bad-secret"} {
		req := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
			`{"message":"hi"}`), credential)
		rec := serveAdmin(srv, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, credential)
		require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, rec).Code)
	}
	require.Empty(t, runs.requests(), "非法身份不得触达运行入口")
}

// TestBusinessAuth_AdminTokenMapsToLocalAdmin 判 admin token 是业务面的合法凭据并
// 映射成内置管理员（C-28）：无调用方端口时同样成立（token 比较在本闸门内）。
// 逆改点：业务面只认 caller key → 本例 401，invoke 零调用。
func TestBusinessAuth_AdminTokenMapsToLocalAdmin(t *testing.T) {
	t.Parallel()
	guard := businessGuard(t, &fakeCallerKeys{})
	srv, runs, _ := newBusinessServer(t, guard)

	req := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
		`{"message":"hi"}`), testToken)
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusOK, rec.Code)

	requests := runs.requests()
	require.Len(t, requests, 1)
	require.Equal(t, core.LocalAdminPrincipal(), requests[0].Principal,
		"admin token 必须映射成显式构造的内置管理员")
}

// TestBusinessAuth_CallerKeyReachesHandlers 判调用方身份的端到端传导：建会话写入
// `caller_id`，无状态调用把同一 Principal 原样交给运行入口。
// 逆改点：handler 内仍写 LocalAdminPrincipal → 运行入口读到管理员归属；不写
// caller_id → 所属行读回空串。
func TestBusinessAuth_CallerKeyReachesHandlers(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"c1-secret": callerPrincipal("c1", core.ScopeSessionsWrite,
			core.AgentInvokeScopePrefix+"ops-agent"),
	}}
	guard := businessGuard(t, keys)
	srv, runs, sessions := newBusinessServer(t, guard)

	create := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`), "c1-secret")
	require.Equal(t, http.StatusOK, serveAdmin(srv, create).Code)

	id := core.DeriveSessionID(channelWeb, defaultUserID, "ops-agent")
	rec, err := sessions.GetSession(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "c1", rec.CallerID, "POST /sessions 必须写入 Principal.CallerID")

	invoke := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
		`{"message":"hi"}`), "c1-secret")
	require.Equal(t, http.StatusOK, serveAdmin(srv, invoke).Code)

	requests := runs.requests()
	require.Len(t, requests, 1)
	require.Equal(t, "c1", requests[0].Principal.CallerID)
	require.Equal(t, "key_c1", requests[0].Principal.KeyID)
	require.Equal(t, core.PrincipalCaller, requests[0].Principal.Kind)
	require.Equal(t, core.ActorCaller, requests[0].Principal.Actor)
}

// TestBusinessAuth_ScopeDefaultDeny 判作用域默认拒绝：缺 `sessions:write` 建不了
// 会话；`agents:invoke:<name>` 只匹配具名 Agent（管理员才持有通配）。
// 逆改点：业务行不声明 / 不检查 scope → 两个被拒请求都变 200，且运行入口被触达。
func TestBusinessAuth_ScopeDefaultDeny(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"read-only":   callerPrincipal("c-read", core.ScopeSessionsRead),
		"other-agent": callerPrincipal("c-other", core.ScopeSessionsWrite, core.AgentInvokeScopePrefix+"other-agent"),
		"ops-agent":   callerPrincipal("c-ops", core.ScopeSessionsWrite, core.AgentInvokeScopePrefix+"ops-agent"),
	}}
	guard := businessGuard(t, keys)
	srv, runs, sessions := newBusinessServer(t, guard)

	create := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`), "read-only")
	rec := serveAdmin(srv, create)
	require.Equal(t, http.StatusForbidden, rec.Code, "缺 sessions:write 必须 403")
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeForbidden, env.Code)
	require.Equal(t, messageScopeDenied, env.Message)
	require.Zero(t, sessions.getOrCreateCallCount(), "作用域拒绝不得触达会话仓储")

	invoke := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
		`{"message":"hi"}`), "other-agent")
	rec = serveAdmin(srv, invoke)
	require.Equal(t, http.StatusForbidden, rec.Code, "只持有 other-agent 的作用域")
	require.Empty(t, runs.requests(), "作用域拒绝不得触达运行入口")

	granted := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
		`{"message":"hi"}`), "ops-agent")
	require.Equal(t, http.StatusOK, serveAdmin(srv, granted).Code, "具名 scope 命中即可调用")
}

// TestBusinessAuth_CrossCallerSessionNotFound 判会话归属：跨调用方与"无归属遗留行"
// 都按不存在回答（404），且运行入口零调用；管理员照常可写。
// 逆改点：只按 session_id 取会话、不比对 caller_id → 第一个用例 200 并真的跑起来。
func TestBusinessAuth_CrossCallerSessionNotFound(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"c1-secret": callerPrincipal("c1", core.ScopeSessionsWrite),
		"c2-secret": callerPrincipal("c2", core.ScopeSessionsWrite),
	}}
	guard := businessGuard(t, keys)
	srv, runs, sessions := newBusinessServer(t, guard)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	sessions.seed(core.SessionRecord{
		ID: "sess-c1", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u1",
		CallerID: "c1", Status: statusActive, CreatedAt: now, LastActiveAt: now,
	})
	sessions.seed(core.SessionRecord{
		ID: "sess-legacy", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u1",
		Status: statusActive, CreatedAt: now, LastActiveAt: now,
	})

	foreign := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions/sess-c1/messages",
		`{"message":"hi"}`), "c2-secret")
	rec := serveAdmin(srv, foreign)
	require.Equal(t, http.StatusNotFound, rec.Code, "跨调用方一律 404，不泄露会话存在")
	require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)

	legacy := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions/sess-legacy/messages",
		`{"message":"hi"}`), "c1-secret")
	require.Equal(t, http.StatusNotFound, serveAdmin(srv, legacy).Code,
		"空归属（迁移遗留）不是无主，调用方不得写入")
	require.Empty(t, runs.requests())

	own := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions/sess-c1/messages",
		`{"message":"hi"}`), "c1-secret")
	require.Equal(t, http.StatusOK, serveAdmin(srv, own).Code)
	require.Len(t, runs.requests(), 1)

	admin := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions/sess-c1/messages",
		`{"message":"hi"}`), testToken)
	require.Equal(t, http.StatusOK, serveAdmin(srv, admin).Code, "内置管理员不受归属限制")
}

// TestBusinessAuth_CreateSessionCrossCallerNotFound 判建会话的归属冲突：同一
// (channel,user_id,agent) 已属于他人时按不存在回答，且**不改写**该行归属。
// 逆改点：直接返回 GetOrCreateSession 的行 → 200 且把他人会话摘要交给新调用方。
func TestBusinessAuth_CreateSessionCrossCallerNotFound(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"c2-secret": callerPrincipal("c2", core.ScopeSessionsWrite),
	}}
	guard := businessGuard(t, keys)
	srv, _, sessions := newBusinessServer(t, guard)
	id := core.DeriveSessionID(channelWeb, defaultUserID, "ops-agent")
	sessions.seed(core.SessionRecord{
		ID: id, ProfileName: "ops-agent", Channel: channelWeb, UserID: defaultUserID,
		CallerID: "c1", Status: statusActive,
	})

	req := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`), "c2-secret")
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)

	kept, err := sessions.GetSession(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "c1", kept.CallerID, "拒绝不得改写既有归属")
}

// TestBusinessAuth_ManagementSurfaceRejectsCallerKey 判管理面只接受 admin token：
// 有效 caller key → 403（契约 §8：两面凭据不互换），未知凭据仍 → 401。
// 逆改点：管理面不探测 caller key → 第一例变 401；把"已认证"当管理资格 → 第二例变 200。
func TestBusinessAuth_ManagementSurfaceRejectsCallerKey(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"c1-secret": callerPrincipal("c1", core.ScopeSessionsWrite),
	}}
	guard := businessGuard(t, keys)
	srv, _, _ := newBusinessServer(t, guard)

	callerKey := withBearer(adminRequest(http.MethodGet, "/api/v1/sandbox", ""), "c1-secret")
	rec := serveAdmin(srv, callerKey)
	require.Equal(t, http.StatusForbidden, rec.Code, "管理端点必须拒绝 caller key")
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeForbidden, env.Code)
	require.Equal(t, messageForbidden, env.Message)

	unknown := withBearer(adminRequest(http.MethodGet, "/api/v1/sandbox", ""), "unknown-secret")
	require.Equal(t, http.StatusUnauthorized, serveAdmin(srv, unknown).Code,
		"未知凭据仍是 401——403 只留给有效 caller key")
}

// TestBusinessAuth_LocalTrustModeStillRequiresCredential 判 local_trust 姿态下的
// 业务面：本机信任只回答"管理请求可信"，**不是**业务凭据——无凭据的业务请求一律
// 401；caller key 从任意对端都可用；本机信任三元组成立时映射成内置管理员。
// 逆改点：把 local_trust 当"认证关闭" → 第一个用例变 200。
func TestBusinessAuth_LocalTrustModeStillRequiresCredential(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"c1-secret": callerPrincipal("c1", core.AgentInvokeScopePrefix+"ops-agent"),
	}}
	guard, err := NewAuthGuardWithPrincipals(AdminAuthConfig{
		Mode:           adminAuthModeLocalTrust,
		AllowedHosts:   []string{"127.0.0.1:8080", "localhost:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow }, keys)
	require.NoError(t, err)
	srv, runs, _ := newBusinessServer(t, guard)

	remote := adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke", `{"message":"hi"}`)
	remote.RemoteAddr = "192.0.2.9:1234"
	require.Equal(t, http.StatusUnauthorized, serveAdmin(srv, remote).Code,
		"local_trust 不是业务凭据")
	require.Empty(t, runs.requests())

	withKey := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
		`{"message":"hi"}`), "c1-secret")
	withKey.RemoteAddr = "192.0.2.9:1234"
	require.Equal(t, http.StatusOK, serveAdmin(srv, withKey).Code,
		"caller key 不受对端地址限制")

	local := adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke", `{"message":"hi"}`)
	local.RemoteAddr = "127.0.0.1:5555"
	local.Header.Set(localTrustHeader, "1")
	require.Equal(t, http.StatusOK, serveAdmin(srv, local).Code, "本机信任三元组成立即内置管理员")

	forwarded := adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke", `{"message":"hi"}`)
	forwarded.RemoteAddr = "127.0.0.1:5555"
	forwarded.Header.Set(localTrustHeader, "1")
	forwarded.Header.Set("X-Forwarded-For", "203.0.113.7")
	require.Equal(t, http.StatusUnauthorized, serveAdmin(srv, forwarded).Code,
		"任何转发头都否决本机信任")
	require.Len(t, runs.requests(), 2, "两次成功各一次运行，被拒请求零触达")
}

// TestBusinessAuth_RouteTableDeclaresScopes 判认证面的启动自检：业务行都声明已知
// 作用域并只挂 principalHandler；知识库九行另按表钉住读 / 写作用域；合成的坏行
// 必须让检查失败。
// 逆改点：删掉某行 scope → 检查失败（本用例红），启动期即拒绝装配；把知识行
// 的读 / 写作用域对调 → 与下表不符即红（读端点写面化是最容易的泄漏口）。
func TestBusinessAuth_RouteTableDeclaresScopes(t *testing.T) {
	t.Parallel()
	// 知识库九行按契约声明作用域：写面（建集合、软删、摄取、重建）计
	// `knowledge:write`，读面（列表、版本列表、检索、来源视图）计 `knowledge:read`。
	// 新增知识行**必须**同时在这里登记，否则下面的条目数断言会红。
	wantKnowledgeScope := map[string]requiredScope{
		"POST /api/v1/collections":                fixedScope(core.ScopeKnowledgeWrite),
		"DELETE /api/v1/collections/{id}":         fixedScope(core.ScopeKnowledgeWrite),
		"GET /api/v1/collections/{id}/documents":  fixedScope(core.ScopeKnowledgeRead),
		"POST /api/v1/collections/{id}/documents": fixedScope(core.ScopeKnowledgeWrite),
		"DELETE /api/v1/documents/{id}":           fixedScope(core.ScopeKnowledgeWrite),
		"GET /api/v1/documents/{id}/versions":     fixedScope(core.ScopeKnowledgeRead),
		"POST /api/v1/documents/{id}/reindex":     fixedScope(core.ScopeKnowledgeWrite),
		"POST /api/v1/search":                     fixedScope(core.ScopeKnowledgeRead),
		"POST /api/v1/sources/view":               fixedScope(core.ScopeKnowledgeRead),
	}
	// 历史回查三行（T056）一律计 `sessions:read`：能读自己的会话不等于能写，
	// 而"能读自己的会话"与检索 / 精确读取是同一个权限口径。
	// The three history rows all require sessions:read.
	wantHistoryScope := map[string]requiredScope{
		"GET /api/v1/sessions":                                    fixedScope(core.ScopeSessionsRead),
		"GET /api/v1/history/search":                              fixedScope(core.ScopeSessionsRead),
		"GET /api/v1/sessions/{session_id}/messages/{message_id}": fixedScope(core.ScopeSessionsRead),
	}
	wantFlowScope := map[string]requiredScope{
		"GET /api/v1/flows":                         fixedScope(core.ScopeFlowsRead),
		"GET /api/v1/flows/{name}":                  fixedScope(core.ScopeFlowsRead),
		"GET /api/v1/flow-tasks":                    fixedScope(core.ScopeFlowsRead),
		"GET /api/v1/flow-tasks/{id}":               fixedScope(core.ScopeFlowsRead),
		"GET /api/v1/flow-tasks/{id}/artifacts":     fixedScope(core.ScopeFlowsRead),
		"GET /api/v1/flow-tasks/{id}/verifications": fixedScope(core.ScopeFlowsRead),
		"POST /api/v1/flow-tasks":                   fixedScope(core.ScopeFlowsWrite),
		"POST /api/v1/flow-tasks/{id}/cancel":       fixedScope(core.ScopeFlowsWrite),
		"POST /api/v1/flow-tasks/{id}/resume":       fixedScope(core.ScopeFlowsWrite),
	}
	srv := newTestServer(t, nil)
	businessRows := 0
	knowledgeRows := 0
	historyRows := 0
	flowRows := 0
	for _, rt := range srv.routeTable() {
		if rt.surface != surfaceRouteBusiness {
			continue
		}
		businessRows++
		name := rt.method + " " + rt.pattern
		require.True(t, rt.scope.declared(), "%s 必须声明已知作用域", name)
		require.NotNil(t, rt.principalHandler, "%s 必须挂业务处理函数", name)
		require.Nil(t, rt.handler, "%s 不得同时挂 handler", name)
		require.Nil(t, rt.adminHandler, "%s 不得同时挂 adminHandler", name)
		require.Equal(t, routeProtected, rt.visibility, "%s 必须是受保护路由", name)
		if expected, ok := wantKnowledgeScope[name]; ok {
			knowledgeRows++
			require.Equal(t, expected, rt.scope, "%s 的作用域与契约不符", name)
		}
		if expected, ok := wantHistoryScope[name]; ok {
			historyRows++
			require.Equal(t, expected, rt.scope, "%s 的作用域与契约不符", name)
		}
		if expected, ok := wantFlowScope[name]; ok {
			flowRows++
			require.Equal(t, expected, rt.scope, "%s 的流程scope必须准确匹配", name)
		}
	}
	require.Equal(t, 3+len(wantKnowledgeScope)+len(wantHistoryScope)+len(wantFlowScope), businessRows,
		"业务面逐组计数，US19新增九条流程业务行")
	require.Equal(t, len(wantFlowScope), flowRows, "全部流程业务行逐条要求scope，不能只满足总数")
	require.Equal(t, len(wantKnowledgeScope), knowledgeRows,
		"知识库业务行数与期望表条目数必须一致（缺少或改名都算）")
	require.Equal(t, len(wantHistoryScope), historyRows,
		"历史回查业务行数与期望表条目数必须一致（缺少或改名都算）")

	handler := func(http.ResponseWriter, *http.Request) {}
	principalHandler := func(http.ResponseWriter, *http.Request, core.Principal) {}
	cases := []struct {
		name string
		row  route
		ok   bool
	}{
		{
			name: "合法业务行",
			row: route{
				method: http.MethodPost, pattern: "/biz", visibility: routeProtected,
				surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeSessionsWrite),
				principalHandler: principalHandler,
			}, ok: true,
		},
		{
			name: "业务行缺作用域",
			row: route{
				method: http.MethodPost, pattern: "/biz", visibility: routeProtected,
				surface: surfaceRouteBusiness, principalHandler: principalHandler,
			},
		},
		{
			name: "业务行缺处理函数",
			row: route{
				method: http.MethodPost, pattern: "/biz", visibility: routeProtected,
				surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeSessionsWrite),
			},
		},
		{
			name: "业务行混挂管理处理函数",
			row: route{
				method: http.MethodPost, pattern: "/biz", visibility: routeProtected,
				surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeSessionsWrite),
				principalHandler: principalHandler, handler: handler,
			},
		},
		{
			name: "业务行非受保护",
			row: route{
				method: http.MethodPost, pattern: "/biz", surface: surfaceRouteBusiness,
				scope: fixedScope(core.ScopeSessionsWrite), principalHandler: principalHandler,
			},
		},
		{
			name: "管理行声明业务作用域",
			row: route{
				method: http.MethodGet, pattern: "/mgmt", visibility: routeProtected,
				scope: fixedScope(core.ScopeSessionsRead), handler: handler,
			},
		},
		{
			name: "管理行挂业务处理函数",
			row: route{
				method: http.MethodGet, pattern: "/mgmt", visibility: routeProtected,
				principalHandler: principalHandler,
			},
		},
	}
	for _, tc := range cases {
		err := checkRouteSurfaces([]route{tc.row})
		if tc.ok {
			require.NoError(t, err, tc.name)
			continue
		}
		require.Error(t, err, tc.name)
	}
}

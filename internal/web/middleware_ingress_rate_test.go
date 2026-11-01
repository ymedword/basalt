package web

// 本文件覆盖入口速率中间件（模块卡 §3.1.7 / 图 6 [1] / C-35 / C-F03 / T126）：业务
// 请求按路由行映射的类别扣一次桶、管理凭据与调度器不进桶、未认证请求不扣桶、
// 端口故障失败关闭，以及「业务行必须映射、管理行不得映射」的启动自检。
// 判据面口径：429 固定 `CALLER_RATE_LIMITED` + `Retry-After`；未认证 401 在扣桶之前
// 发生，作用域 403 在扣桶**之后**发生（C-F03：先限流、后作用域，拒绝扣自己的桶）；
// admin token 请求无论桶是否已空都放行。
// These tests pin the ingress-rate middleware: one charge per authenticated business
// request, no charge for admin traffic or pre-rate denials, fail-closed port errors,
// and the startup mapping check.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// allowAllIngress 是 handler 级用例的缺省放行门：那些用例关心的是认证之后的路由
// 行为，桶本身由本文件单独覆盖。
// allowAllIngress is the default no-op gate for handler-level tests.
type allowAllIngress struct{}

// AdmitIngress 一律放行。
// AdmitIngress always admits.
func (allowAllIngress) AdmitIngress(context.Context, core.Principal, callers.IngressClass) error {
	return nil
}

// ingressCall 是一次入口速率判定的观测值。
// ingressCall records one admission decision.
type ingressCall struct {
	callerID string
	kind     core.PrincipalKind
	class    callers.IngressClass
}

// recordingIngress 记录判定并可按用例注入端口错误（真实端口返回 ErrRateLimited）。
// recordingIngress records each decision and can inject a port error.
type recordingIngress struct {
	err   error
	calls []ingressCall
}

// AdmitIngress 先记账再返回注入的错误。
// AdmitIngress records the call, then returns the injected error.
func (g *recordingIngress) AdmitIngress(
	_ context.Context, p core.Principal, class callers.IngressClass,
) error {
	g.calls = append(g.calls, ingressCall{callerID: p.CallerID, kind: p.Kind, class: class})
	return g.err
}

// count 返回判定次数。
// count reports how many decisions were made.
func (g *recordingIngress) count() int { return len(g.calls) }

// newIngressServer 装配接入了真实认证闸门与给定速率门的服务器，并交回缺省假件。
// newIngressServer wires the real guard plus the given gate and returns the fakes.
func newIngressServer(t *testing.T, gate IngressGate) (*Server, *fakeRunService, *fakeSessionStore) {
	t.Helper()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"c1-secret": callerPrincipal("c1", core.ScopeSessionsWrite,
			core.AgentInvokeScopePrefix+"ops-agent"),
		"ro-secret": callerPrincipal("ro", core.ScopeSessionsRead),
		// 第二个可写调用方：桶隔离用例需要一个"有权限、但与 c1 不同桶"的主体。
		// A second writable caller so bucket-isolation tests need not rely on 403.
		"c2-secret": callerPrincipal("c2", core.ScopeSessionsWrite,
			core.AgentInvokeScopePrefix+"ops-agent"),
	}}
	var runs *fakeRunService
	var sessions *fakeSessionStore
	srv := newTestServer(t, func(o *Options) {
		runs = o.Runs.(*fakeRunService)
		sessions = o.Sessions.(*fakeSessionStore)
		o.AdminAuth = businessGuard(t, keys)
		o.Ingress = gate
	})
	return srv, runs, sessions
}

// TestIngressMiddleware_ChargesBusinessRequestOnce 判一次已认证业务请求恰扣一次桶，
// 且带上路由行映射的类别与调用方标识。
// 逆改点：中间件挂到每次模型调用 / 工具循环上 → 计数 >1；不接进 business() → 计数 0。
func TestIngressMiddleware_ChargesBusinessRequestOnce(t *testing.T) {
	t.Parallel()
	gate := &recordingIngress{}
	srv, _, _ := newIngressServer(t, gate)

	invoke := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
		`{"message":"hi"}`), "c1-secret")
	require.Equal(t, http.StatusOK, serveAdmin(srv, invoke).Code)

	require.Equal(t, 1, gate.count(), "一次业务请求只扣一次桶")
	require.Equal(t, "c1", gate.calls[0].callerID)
	require.Equal(t, core.PrincipalCaller, gate.calls[0].kind)
	require.Equal(t, callers.IngressRun, gate.calls[0].class)
}

// TestIngressMiddleware_RateLimitedReturns429WithRetryAfter 判速率拒绝的契约形态：
// 429 + `CALLER_RATE_LIMITED` + 整数秒 `Retry-After`，且请求**零触达**会话仓储。
// 逆改点：不设 Retry-After → 头断言红；只写状态码不写信封 → code 断言红；
// 把判定放在 handler 之后 → getOrCreateCallCount 非 0。
func TestIngressMiddleware_RateLimitedReturns429WithRetryAfter(t *testing.T) {
	t.Parallel()
	gate := &recordingIngress{err: callers.ErrRateLimited}
	srv, runs, sessions := newIngressServer(t, gate)

	req := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`), "c1-secret")
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeCallerRateLimited, env.Code)
	require.NotEmpty(t, env.Message)

	raw := rec.Header().Get("Retry-After")
	require.NotEmpty(t, raw, "契约 §8：速率拒绝必须带 Retry-After")
	seconds, err := strconv.Atoi(raw)
	require.NoError(t, err, "Retry-After 取 delay-seconds 形态")
	require.GreaterOrEqual(t, seconds, 1)

	require.Zero(t, sessions.getOrCreateCallCount(), "被限流的请求不得触达会话仓储")
	require.Empty(t, runs.requests())
}

// TestIngressMiddleware_AdminTokenNeverCharged 判 admin token 请求不进任何调用方桶
// （§3.1.5）：即使端口对所有人返回超限，管理员照常通过。
// 逆改点：去掉 PrincipalLocalAdmin 放行 → 本例从 200 变 429。
func TestIngressMiddleware_AdminTokenNeverCharged(t *testing.T) {
	t.Parallel()
	gate := &recordingIngress{err: callers.ErrRateLimited}
	srv, _, _ := newIngressServer(t, gate)

	req := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
		`{"message":"hi"}`), testToken)
	require.Equal(t, http.StatusOK, serveAdmin(srv, req).Code, "管理员不受调用方桶约束")
	require.Zero(t, gate.count(), "admin token 不得进入任何 IngressClass 桶")
}

// TestIngressMiddleware_UnauthenticatedNeverCharged 判判定顺序：认证在扣桶之前，
// 缺凭据与坏凭据都是 401 且**零扣桶**——未认证流量不能消耗别人的额度。
// 逆改点：把入口速率挪到认证之前 → count 非 0；用空 Principal 调端口 → 真实端口
// 返回 ErrUnauthorized，回答变成 401 之外的状态。
func TestIngressMiddleware_UnauthenticatedNeverCharged(t *testing.T) {
	t.Parallel()
	gate := &recordingIngress{}
	srv, runs, _ := newIngressServer(t, gate)

	missing := serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`))
	require.Equal(t, http.StatusUnauthorized, missing.Code)

	unknown := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions",
		`{"agent":"ops-agent"}`), "unknown-secret")
	require.Equal(t, http.StatusUnauthorized, serveAdmin(srv, unknown).Code)

	require.Zero(t, gate.count(), "未认证请求不得消耗调用方桶")
	require.Empty(t, runs.requests())
}

// TestIngressMiddleware_ScopeDeniedChargesCallerBucket 判 [1] 与 [2] 的新先后
// （C-F03 裁决：认证 → 入口限流 → 作用域）：作用域拒绝的请求**扣自己的桶**，但
// 仍不触达处理函数。代价维度是调用方自伤，收益是无权调用方不能无限探测而不消耗
// 任何名额；桶键与目标资源无关，因此不新增存在性侧信道。
// 逆改点：把 admitIngress 放回作用域判定之后 → count 变 0，本用例红。
// A scope denial now charges the caller's own bucket (C-F03) but still never reaches
// the handler; the bucket key is target-independent, so no side channel is added.
func TestIngressMiddleware_ScopeDeniedChargesCallerBucket(t *testing.T) {
	t.Parallel()
	gate := &recordingIngress{}
	srv, _, sessions := newIngressServer(t, gate)

	req := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`), "ro-secret")
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, 1, gate.count(), "[2] 在 [1] 之前：作用域拒绝也扣自己那一桶")
	require.Equal(t, "ro", gate.calls[0].callerID)
	require.Equal(t, callers.IngressRun, gate.calls[0].class)
	require.Zero(t, sessions.getOrCreateCallCount(), "作用域拒绝不得触达处理函数")
}

// TestIngressMiddleware_RateLimitPrecedesScopeDenial 判两段的可观察差异：桶空时
// 无权限调用方拿到 429（不是 403）——限流不看目标，先耗尽就先拒。这是 C-F03 的
// 判据面变更本身，逆改（把作用域判回前面）会让第二个请求重新变 403。
func TestIngressMiddleware_RateLimitPrecedesScopeDenial(t *testing.T) {
	t.Parallel()
	buckets, err := callers.NewRateBucket(callers.IngressLimits{
		Run:    callers.IngressBudget{QPS: 1, Burst: 1},
		Ingest: callers.IngressBudget{QPS: 1, Burst: 1},
		Read:   callers.IngressBudget{QPS: 1, Burst: 1},
	}, func() time.Time { return authFixedNow })
	require.NoError(t, err)
	srv, _, _ := newIngressServer(t, callers.NewLimiter(callers.LimiterDeps{Buckets: buckets}))

	denied := func() int {
		return serveAdmin(srv, withBearer(adminRequest(http.MethodPost, "/api/v1/sessions",
			`{"agent":"ops-agent"}`), "ro-secret")).Code
	}
	require.Equal(t, http.StatusForbidden, denied(), "首次请求扣桶后仍走作用域判定 → 403")
	require.Equal(t, http.StatusTooManyRequests, denied(), "桶空后无权限调用方先撞速率 → 429")
}

// TestIngressMiddleware_CrossCallerBucketIsolation 判调用方隔离：一个调用方耗尽
// 自己的桶不影响另一个调用方，也不影响管理员。
// 逆改点：桶键退化成"全局"或"按类别" → 第二段调用方从 200 变 429。
func TestIngressMiddleware_CrossCallerBucketIsolation(t *testing.T) {
	t.Parallel()
	buckets, err := callers.NewRateBucket(callers.IngressLimits{
		Run:    callers.IngressBudget{QPS: 1, Burst: 1},
		Ingest: callers.IngressBudget{QPS: 1, Burst: 1},
		Read:   callers.IngressBudget{QPS: 1, Burst: 1},
	}, func() time.Time { return authFixedNow })
	require.NoError(t, err)
	srv, _, _ := newIngressServer(t, callers.NewLimiter(callers.LimiterDeps{Buckets: buckets}))

	invokeAs := func(credential string) int {
		return serveAdmin(srv, withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
			`{"message":"hi"}`), credential)).Code
	}
	require.Equal(t, http.StatusOK, invokeAs("c1-secret"))
	require.Equal(t, http.StatusTooManyRequests, invokeAs("c1-secret"), "c1 自己的桶已耗尽")
	require.Equal(t, http.StatusOK, invokeAs("c2-secret"), "另一个调用方的桶不受影响")
	require.Equal(t, http.StatusTooManyRequests, invokeAs("c2-secret"), "c2 的一次额度也用完了")
	require.Equal(t, http.StatusOK, invokeAs(testToken), "管理员不进桶")
}

// TestIngressMiddleware_RateLimitDoesNotRevealResourceExistence 判侧信道：同一个
// 调用方在桶空后，无论目标会话是否存在都得到同形 429——限流回答只依赖调用方与
// 入口类别，不依赖被请求对象，因此"先限流"不制造存在性探测面。
// 逆改点：把资源存在性判定挪到限流之前（如先查会话再扣桶）→ 不存在的会话回 404
// 而不是 429，本用例红。
func TestIngressMiddleware_RateLimitDoesNotRevealResourceExistence(t *testing.T) {
	t.Parallel()
	buckets, err := callers.NewRateBucket(callers.IngressLimits{
		Run:    callers.IngressBudget{QPS: 1, Burst: 1},
		Ingest: callers.IngressBudget{QPS: 1, Burst: 1},
		Read:   callers.IngressBudget{QPS: 1, Burst: 1},
	}, func() time.Time { return authFixedNow })
	require.NoError(t, err)
	srv, _, _ := newIngressServer(t, callers.NewLimiter(callers.LimiterDeps{Buckets: buckets}))

	post := func(path, body string) *httptest.ResponseRecorder {
		return serveAdmin(srv, withBearer(adminRequest(http.MethodPost, path, body), "c1-secret"))
	}
	require.Equal(t, http.StatusOK, post("/api/v1/sessions", `{"agent":"ops-agent"}`).Code)
	require.Equal(t, http.StatusTooManyRequests, post("/api/v1/sessions", `{"agent":"ops-agent"}`).Code,
		"存在的目标：桶空 → 429")

	// 不存在的会话：作用域已通过（c1 有 ScopeSessionsWrite），若仓储查询先于限流，
	// 这里会是 404——那正是要在限流层堵住的存在性侧信道。
	missing := post("/api/v1/sessions/ghost-session/messages", `{"message":"hi"}`)
	require.Equal(t, http.StatusTooManyRequests, missing.Code, "不存在的目标同样 429，不泄露存在性")
	require.Equal(t, codeCallerRateLimited, decodeAuthEnvelope(t, missing).Code)
	require.NotEmpty(t, missing.Header().Get("Retry-After"))
}

// TestIngressMiddleware_GateErrorFailsClosed 判端口的三类失败都不放行：未知错误
// → 500（不带宽限头），端口判未授权 → 401，客户端放弃 → 不写响应体；三者都不得
// 触达处理函数。
// 逆改点：`err != nil` 一律当超限 → 未知错误变 429；`err != nil` 一律放行 → 全部
// 触达会话仓储，本用例三处红。
func TestIngressMiddleware_GateErrorFailsClosed(t *testing.T) {
	t.Parallel()

	broken := &recordingIngress{err: errors.New("bucket unavailable")}
	srv, _, sessions := newIngressServer(t, broken)
	rec := serveAdmin(srv, withBearer(adminRequest(http.MethodPost,
		"/api/v1/sessions", `{"agent":"ops-agent"}`), "c1-secret"))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	require.Empty(t, rec.Header().Get("Retry-After"), "只有速率拒绝才带 Retry-After")
	require.Zero(t, sessions.getOrCreateCallCount(), "判定失败不得触达会话仓储")

	unauthorized := &recordingIngress{err: core.ErrUnauthorized}
	srv2, _, sessions2 := newIngressServer(t, unauthorized)
	rec = serveAdmin(srv2, withBearer(adminRequest(http.MethodPost,
		"/api/v1/sessions", `{"agent":"ops-agent"}`), "c1-secret"))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, rec).Code)
	require.Zero(t, sessions2.getOrCreateCallCount())

	canceled := &recordingIngress{err: context.Canceled}
	srv3, _, sessions3 := newIngressServer(t, canceled)
	rec = serveAdmin(srv3, withBearer(adminRequest(http.MethodPost,
		"/api/v1/sessions", `{"agent":"ops-agent"}`), "c1-secret"))
	require.Zero(t, rec.Body.Len(), "客户端放弃时不得写响应体")
	require.Zero(t, sessions3.getOrCreateCallCount())
}

// TestIngressMiddleware_RealLimiterDeniesSecondBurstRequest 用真实令牌桶判端到端：
// 固定时钟下 burst=1 只够一次，第二次 429；管理员在桶空之后仍放行。
// 逆改点：把管理员也记进桶 → 第三个请求 429；桶按全局而非调用方计数 → 首个请求
// 之后他人也被拒（本用例只用一个调用方，靠前两条守住计数语义）。
func TestIngressMiddleware_RealLimiterDeniesSecondBurstRequest(t *testing.T) {
	t.Parallel()
	buckets, err := callers.NewRateBucket(callers.IngressLimits{
		Run:    callers.IngressBudget{QPS: 1, Burst: 1},
		Ingest: callers.IngressBudget{QPS: 1, Burst: 1},
		Read:   callers.IngressBudget{QPS: 1, Burst: 1},
	}, func() time.Time { return authFixedNow })
	require.NoError(t, err)
	srv, _, _ := newIngressServer(t, callers.NewLimiter(callers.LimiterDeps{Buckets: buckets}))

	invoke := func(credential string) int {
		req := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/invoke",
			`{"message":"hi"}`), credential)
		return serveAdmin(srv, req).Code
	}

	require.Equal(t, http.StatusOK, invoke("c1-secret"))
	require.Equal(t, http.StatusTooManyRequests, invoke("c1-secret"),
		"固定时钟下 burst 用尽即拒")
	require.Equal(t, http.StatusOK, invoke(testToken), "管理员不进桶，桶空也放行")
}

// TestIngressRoutes_BusinessRowsDeclareClass 判真实路由表的入口映射：业务行逐行
// 钉住入口类别（会话与调用行计 IngressRun；知识库的摄取、软删与建集合计
// IngressIngest；知识库读取、检索与来源视图计 IngressRead），其余行一律不映射。
// 逆改点：删掉某业务行的 ingress → 启动自检失败（本用例红）；给管理行加映射 →
// 同样红；把任意一行换成别的类别 → 与下表不符即红。
func TestIngressRoutes_BusinessRowsDeclareClass(t *testing.T) {
	t.Parallel()
	// 逐行期望表：新增业务行**必须**同时在这里登记，否则下面的行数断言会红。
	// 知识库九行的归类依据记 US-16 附录 R52（契约点名的照抄，未点名的三条按
	// "写操作计入摄取桶"裁决）。
	want := map[string]callers.IngressClass{
		"POST /api/v1/sessions":                   callers.IngressRun,
		"POST /api/v1/sessions/{id}/messages":     callers.IngressRun,
		"POST /api/v1/agents/{name}/invoke":       callers.IngressRun,
		"POST /api/v1/collections":                callers.IngressIngest,
		"DELETE /api/v1/collections/{id}":         callers.IngressIngest,
		"GET /api/v1/collections/{id}/documents":  callers.IngressRead,
		"POST /api/v1/collections/{id}/documents": callers.IngressIngest,
		"DELETE /api/v1/documents/{id}":           callers.IngressIngest,
		"GET /api/v1/documents/{id}/versions":     callers.IngressRead,
		"POST /api/v1/documents/{id}/reindex":     callers.IngressIngest,
		"POST /api/v1/search":                     callers.IngressRead,
		"POST /api/v1/sources/view":               callers.IngressRead,
		// 历史回查三行（T056）：列表 / 检索 / 精确读取都是只读，计 IngressRead。
		// 任一条按 Run 计桶都会让"打开列表页"消耗运行预算（D-30 / T126）。
		"GET /api/v1/sessions":                                    callers.IngressRead,
		"GET /api/v1/history/search":                              callers.IngressRead,
		"GET /api/v1/sessions/{session_id}/messages/{message_id}": callers.IngressRead,
	}
	srv := newTestServer(t, nil)
	businessRows := 0
	for _, rt := range srv.routeTable() {
		name := rt.method + " " + rt.pattern
		if rt.surface != surfaceRouteBusiness {
			require.Empty(t, string(rt.ingress), "%s 是管理 / 公开面路由，不得映射入口类别", name)
			continue
		}
		businessRows++
		require.True(t, rt.ingress.Valid(), "%s 必须映射已知入口类别", name)
		expected, ok := want[name]
		require.True(t, ok, "%s 是业务行但未在本用例登记期望类别", name)
		require.Equal(t, expected, rt.ingress, "%s 的入口类别与期望不符", name)
	}
	require.Equal(t, len(want), businessRows,
		"业务行数与期望表条目数必须一致：三条会话 / 调用行 + 九条知识库行 + 三条历史回查行")
	require.NoError(t, checkRouteIngress(srv.routeTable()))
}

// TestIngressRoutes_MappingRequiredAtStartup 判 D-30 的启动期判卷：业务行漏映射与
// 未知类别都失败，管理行误映射同样失败；合法组合全部通过。
// 逆改点：checkRouteIngress 直接 return nil → 三条坏行用例全红。
func TestIngressRoutes_MappingRequiredAtStartup(t *testing.T) {
	t.Parallel()
	principalHandler := func(http.ResponseWriter, *http.Request, core.Principal) {}
	cases := []struct {
		name string
		row  route
		ok   bool
	}{
		{
			name: "业务行映射 IngressRun",
			row: route{
				method: http.MethodPost, pattern: "/biz", visibility: routeProtected,
				surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeSessionsWrite),
				ingress: callers.IngressRun, principalHandler: principalHandler,
			},
			ok: true,
		},
		{
			name: "业务行映射 IngressRead",
			row: route{
				method: http.MethodGet, pattern: "/biz", visibility: routeProtected,
				surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeSessionsRead),
				ingress: callers.IngressRead, principalHandler: principalHandler,
			},
			ok: true,
		},
		{
			name: "业务行未映射",
			row: route{
				method: http.MethodPost, pattern: "/biz", visibility: routeProtected,
				surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeSessionsWrite),
				principalHandler: principalHandler,
			},
		},
		{
			name: "业务行映射未知类别",
			row: route{
				method: http.MethodPost, pattern: "/biz", visibility: routeProtected,
				surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeSessionsWrite),
				ingress: callers.IngressClass("bulk"), principalHandler: principalHandler,
			},
		},
		{
			name: "管理行不映射",
			row: route{
				method: http.MethodGet, pattern: "/mgmt", visibility: routeProtected,
				handler: func(http.ResponseWriter, *http.Request) {},
			},
			ok: true,
		},
		{
			name: "管理行误映射",
			row: route{
				method: http.MethodGet, pattern: "/mgmt", visibility: routeProtected,
				ingress: callers.IngressRead, handler: func(http.ResponseWriter, *http.Request) {},
			},
		},
	}
	for _, tc := range cases {
		err := checkRouteIngress([]route{tc.row})
		if tc.ok {
			require.NoError(t, err, tc.name)
			continue
		}
		require.Error(t, err, tc.name)
	}
}

// TestIngressServer_RequiresGateWithAuthGuard 判装配期配对：有认证闸门却没有速率
// 端口 → NewServer 拒绝（缺省等于静默不限流）；补齐即通过；无闸门的测试装配不受
// 此限（此时业务处理函数拿到内置管理员，按 §3.1.5 不进桶）。
// 逆改点：删掉配对检查 → 第一段从"装配失败"变成成功，本用例红。
func TestIngressServer_RequiresGateWithAuthGuard(t *testing.T) {
	t.Parallel()

	opts := defaultTestOptions(t)
	opts.AdminAuth = businessGuard(t, &fakeCallerKeys{})
	opts.Ingress = nil
	_, err := NewServer(opts)
	require.Error(t, err, "认证闸门与速率端口必须成对出现")
	require.Contains(t, err.Error(), "入口速率")

	opts.Ingress = allowAllIngress{}
	srv, err := NewServer(opts)
	require.NoError(t, err)
	require.NotNil(t, srv)

	opts = defaultTestOptions(t)
	opts.Ingress = nil
	srv, err = NewServer(opts)
	require.NoError(t, err, "无认证闸门的测试装配不受配对约束")
	require.NotNil(t, srv)
}

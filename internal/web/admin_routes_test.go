package web

// 本文件是 US-12 写面路由表与条件写中间件的判卷（颗粒度文档 §3.2.1
// admin_routes_test.go，7 例，check `012-web-etag-routes`）：35 条新端点逐条在表、
// 全部受保护、11 条条件写恰为契约指定集合；`version_id` 只接受十进制正整数；
// 缺凭据一律 401、错误方法落 404 兜底；条件写缺 / 弱 / 通配 / 非法 If-Match 一律
// 412 且处理函数零调用；认证先于 If-Match；每条受保护项恰过一次闸门。
// These tests pin the US-12 admin route table, the conditional-write middleware, and
// the auth-before-If-Match ordering.
//
// 路由尚未并入 `routeTable()`（T016 的计划内挂载），因此本文件的请求走
// `adminTableHandler`——同一份 `adminRoutes()` 行 + 中央认证包装，与生产装配的
// `s.routes` 同构；T016 落地后由 `TestAuth_RouteMatrix` 从主路由表再覆盖一遍。
// Until T016 mounts these rows into routeTable, requests go through a mux built from
// the same adminRoutes() rows and the central guard.

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// adminRouteKey 是写面路由的稳定标识（方法 + 模式）。
// adminRouteKey is the stable identity of an admin route.
func adminRouteKey(spec adminRouteSpec) string { return spec.method + " " + spec.pattern }

// concreteAdminPath 把写面模式里的通配段替换成具体值，供 ServeMux 探测。
// concreteAdminPath substitutes concrete values for every wildcard segment.
func concreteAdminPath(pattern string) string {
	return strings.NewReplacer(
		"{archive_id}", "ops-agent-20261016T093000Z",
		"{version_id}", "7",
		"{kind}", "skill",
		"{id}", "demo",
		"{name}", "ops-agent",
	).Replace(pattern)
}

// adminTableHandler 用生产同一份行与中央认证包装搭一个独立 mux：每行按
// `方法 模式` 注册，兜底 404 与主路由表同形。
// adminTableHandler builds an isolated mux from the same rows and central guard.
func adminTableHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.adminRoutes() {
		mux.Handle(rt.method+" "+rt.pattern, s.routeHandler(rt))
	}
	mux.Handle("/", http.HandlerFunc(s.handleNotFound))
	return mux
}

// serveAdminTable 让请求进入写面 mux 并返回记录器。
// serveAdminTable runs one request through the admin mux.
func serveAdminTable(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	adminTableHandler(s).ServeHTTP(rec, req)
	return rec
}

// wantAdminRouteKeys 是契约 §3.7.1 的新增端点（方法 + 路径逐字）；R7 修订新增渠道连通性测试。
// wantAdminRouteKeys lists every new endpoint verbatim from the contract.
func wantAdminRouteKeys() []string {
	return []string{
		"POST /api/v1/agents",
		"POST /api/v1/agents/{name}/generate-files",
		"POST /api/v1/agents/{name}/schedule-draft",
		"GET /api/v1/agents/{name}/files",
		"POST /api/v1/agents/{name}/files",
		"POST /api/v1/agents/{name}/copy",
		"DELETE /api/v1/agents/{name}",
		"GET /api/v1/archived-agents",
		"POST /api/v1/archived-agents/{archive_id}/restore",
		"POST /api/v1/model-connections",
		"PUT /api/v1/model-connections/{name}",
		"DELETE /api/v1/model-connections/{name}",
		"POST /api/v1/model-connections/{name}/test",
		"POST /api/v1/mcp-servers",
		"PUT /api/v1/mcp-servers/{name}",
		"DELETE /api/v1/mcp-servers/{name}",
		"POST /api/v1/mcp-servers/{name}/test",
		"POST /api/v1/skills",
		"POST /api/v1/skills/import",
		"PUT /api/v1/skills/{name}",
		"DELETE /api/v1/skills/{name}",
		"POST /api/v1/skills/{name}/validate",
		"GET /api/v1/resources/{kind}/{id}/versions",
		"GET /api/v1/resource-versions/{version_id}",
		"POST /api/v1/resources/{kind}/{id}/publish",
		"GET /api/v1/resources/{kind}/{id}/references",
		"POST /api/v1/resources/{kind}/{name}/credential/revoke",
		"POST /api/v1/resources/{kind}/{name}/scope",
		"POST /api/v1/notify-channels",
		"PUT /api/v1/notify-channels/{name}",
		"DELETE /api/v1/notify-channels/{name}",
		"POST /api/v1/notify-channels/{name}/test",
		"GET /api/v1/workspace/tree",
		"GET /api/v1/workspace/file",
		"PUT /api/v1/sandbox/network-domains",
	}
}

// wantConditionalRouteKeys 是 11 条条件写（修改 / 删除 / 发布 / Agent 多文件提交 /
// 凭据撤销 / 范围切换 / 出网域名白名单）。
// wantConditionalRouteKeys lists the eleven conditional-write rows.
func wantConditionalRouteKeys() []string {
	return []string{
		"POST /api/v1/agents/{name}/files",
		"PUT /api/v1/model-connections/{name}",
		"DELETE /api/v1/model-connections/{name}",
		"PUT /api/v1/mcp-servers/{name}",
		"DELETE /api/v1/mcp-servers/{name}",
		"PUT /api/v1/skills/{name}",
		"DELETE /api/v1/skills/{name}",
		"POST /api/v1/resources/{kind}/{id}/publish",
		"POST /api/v1/resources/{kind}/{name}/credential/revoke",
		"POST /api/v1/resources/{kind}/{name}/scope",
		"PUT /api/v1/sandbox/network-domains",
	}
}

// TestAdminRoutes_TableCoversContractSurface：写面路由表恰好覆盖契约的 35 条端点，
// 无重复、无公开行、方法显式、条件写恰为 11 条；`adminRoutes()` 逐行标记受保护并
// 带上处理函数（占位也必须是显式函数，不允许空挂）。
// 逆改点：漏登记 / 多登记 / 把任一行改成公开 / 条件写集合漂移 → 对应断言红。
func TestAdminRoutes_TableCoversContractSurface(t *testing.T) {
	s := newTestServer(t, nil)
	specs := s.adminRouteSpecs()
	require.Len(t, specs, 35)

	got := make(map[string]adminRouteSpec, len(specs))
	for _, spec := range specs {
		require.NotEmpty(t, spec.method, "%s 必须显式给出方法", spec.pattern)
		require.True(t, strings.HasPrefix(spec.pattern, "/api/v1/"), "新端点必须在 /api/v1 下：%s", spec.pattern)
		require.NotNil(t, spec.handler, "%s 必须有处理函数（未装配时用显式占位）", adminRouteKey(spec))
		require.NotContains(t, got, adminRouteKey(spec), "重复登记：%s", adminRouteKey(spec))
		got[adminRouteKey(spec)] = spec
	}
	require.Equal(t,
		slices.Sorted(slices.Values(wantAdminRouteKeys())),
		slices.Sorted(maps.Keys(got)),
	)

	conditional := make([]string, 0, 11)
	for _, spec := range specs {
		if spec.conditional {
			conditional = append(conditional, adminRouteKey(spec))
		}
	}
	require.Equal(t,
		slices.Sorted(slices.Values(wantConditionalRouteKeys())),
		slices.Sorted(slices.Values(conditional)),
	)

	rows := s.adminRoutes()
	require.Len(t, rows, len(specs))
	for i, rt := range rows {
		require.Equal(t, routeProtected, rt.visibility, "%s 必须是受保护路由", routeKey(rt))
		require.NotNil(t, rt.adminHandler, "%s 必须带显式 actor 处理函数", routeKey(rt))
		require.Nil(t, rt.handler, "%s 不得同时挂两个处理函数", routeKey(rt))
		require.Equal(t, adminRouteKey(specs[i]), routeKey(rt), "行顺序必须与静态描述一致")
	}
}

// TestAdminRoutes_VersionIDParsing：`version_id` 只接受 ASCII 十进制正整数；
// 空串、0、负号、正号、前后空白、小数、十六进制、非数字与溢出全部拒绝——由
// handler 统一映射 404，不用 400 泄露"版本是否存在"。
// 逆改点：直接 `strconv.Atoi` 或漏掉 ≤0 / 符号检查 → 对应子例红。
func TestAdminRoutes_VersionIDParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
		ok   bool
	}{
		{"1", 1, true},
		{"7", 7, true},
		{"007", 7, true},
		{"9223372036854775807", 9223372036854775807, true},
		{"", 0, false},
		{"0", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{" 7", 0, false},
		{"7 ", 0, false},
		{"1.0", 0, false},
		{"abc", 0, false},
		{"0x10", 0, false},
		{"1e3", 0, false},
		{"9223372036854775808", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseVersionID(tc.raw)
		require.Equal(t, tc.ok, ok, "raw=%q", tc.raw)
		require.Equal(t, tc.want, got, "raw=%q", tc.raw)
	}
}

// TestAdminRoutes_RequireAuthAndMethod 覆盖任务 T015 的三条主干断言（模块卡
// §4.4）：全部非公开新路由无凭据 → 401 UNAUTHORIZED 且不进入处理函数；错误方法
// 落 404 兜底信封（不暴露 ServeMux 的 405 纯文本）；公开面不因本表扩大。
// 逆改点：漏挂认证 / 把某行标成公开 / 方法写错 / 兜底被绕过 → 对应子例红。
func TestAdminRoutes_RequireAuthAndMethod(t *testing.T) {
	specs := newTestServer(t, nil).adminRouteSpecs()

	t.Run("every admin route rejects missing credentials", func(t *testing.T) {
		for _, spec := range specs {
			// 每条路由一个**新装配**：认证限速按来源累计，同一来源连打会变 429。
			s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
			rec := serveAdminTable(s, adminRequest(spec.method, concreteAdminPath(spec.pattern), ""))
			require.Equal(t, http.StatusUnauthorized, rec.Code, "%s", adminRouteKey(spec))
			require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, rec).Code, "%s", adminRouteKey(spec))
		}
	})

	t.Run("wrong methods fall back to the 404 envelope", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
		mux := adminTableHandler(s)
		for _, spec := range specs {
			// PATCH 未被任何行注册：ServeMux 落到方法无关兜底，而不是自动 405。
			req := withBearer(adminRequest(http.MethodPatch, concreteAdminPath(spec.pattern), ""), testToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			require.Equal(t, http.StatusNotFound, rec.Code, "%s 只接受 %s", spec.pattern, spec.method)
			require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code, "%s", spec.pattern)
		}
	})
}

// TestAdminRoutes_ConditionalWritesDemandIfMatch：11 条条件写在 If-Match 缺失 /
// 弱 / 通配时一律 412 PRECONDITION_FAILED，处理函数零调用（因此零写入）；形状
// 合法的强 ETag 才进入处理函数。
// 逆改点：把条件写中间件漏套在任一行 / 放行弱 ETag / 放行 `*` → 对应子例红。
func TestAdminRoutes_ConditionalWritesDemandIfMatch(t *testing.T) {
	digest := strings.Repeat("a", 64)
	validETag := formatETag(digest)

	t.Run("table rows reject a missing If-Match", func(t *testing.T) {
		for _, spec := range newTestServer(t, nil).adminRouteSpecs() {
			if !spec.conditional {
				continue
			}
			s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
			req := withBearer(adminRequest(spec.method, concreteAdminPath(spec.pattern), ""), testToken)
			rec := serveAdminTable(s, req)
			require.Equal(t, http.StatusPreconditionFailed, rec.Code, "%s", adminRouteKey(spec))
			require.Equal(t, codePreconditionFailed, decodeAuthEnvelope(t, rec).Code, "%s", adminRouteKey(spec))
		}
	})

	t.Run("the middleware rejects every non-literal shape before the handler", func(t *testing.T) {
		s := newTestServer(t, nil)
		calls := 0
		wrapped := s.requireIfMatch(func(w http.ResponseWriter, _ *http.Request, _ core.Actor) {
			calls++
			w.WriteHeader(http.StatusNoContent)
		}, nil)

		for _, raw := range []string{
			"",                                  // 缺失
			"*",                                 // 通配
			"W/" + validETag,                    // 弱 ETag
			validETag + ", " + validETag,        // 列表
			"sha256:" + digest,                  // 裸摘要（无引号）
			" " + validETag,                     // 前导空白
			formatETag(strings.ToUpper(digest)), // 大写十六进制
		} {
			req := adminRequest(http.MethodPut, "/api/v1/model-connections/primary", "")
			if raw != "" {
				req.Header.Set("If-Match", raw)
			}
			rec := httptest.NewRecorder()
			wrapped(rec, req, core.Actor{})
			require.Equal(t, http.StatusPreconditionFailed, rec.Code, "If-Match=%q", raw)
			require.Equal(t, codePreconditionFailed, decodeAuthEnvelope(t, rec).Code, "If-Match=%q", raw)
		}
		require.Zero(t, calls, "非法 If-Match 不得进入处理函数")

		req := adminRequest(http.MethodPut, "/api/v1/model-connections/primary", "")
		req.Header.Set("If-Match", validETag)
		rec := httptest.NewRecorder()
		wrapped(rec, req, core.Actor{})
		require.Equal(t, http.StatusNoContent, rec.Code)
		require.Equal(t, 1, calls, "合法强 ETag 必须进入处理函数一次")
	})
}

// TestAdminRoutes_ShapeRejectionCarriesCurrentETag：约束 3 要求 412 在能解析当前值
// 时给出 `data.current_etag`——形状错误与陈旧值给同一组字段，页面按同一条路径重取。
// 资源类**原样转交**服务端的 `CurrentETag`（FR-081 的组成在适配器里做，web 层不
// 自行用 content_hash 拼装）；Agent 多文件取受管文件集合 ETag；发布路由按路径
// kind/id 解析。解析不了（目标不存在）时 412 照发、不带 data——不声称有一个取不到
// 的当前值。
// 逆改点：形状级 412 恒不带 data / web 自行用版本列表拼 ETag / 资源与 Agent 用同一
// 来源 → 红。
func TestAdminRoutes_ShapeRejectionCarriesCurrentETag(t *testing.T) {
	publishedETag := core.FormatStrongETag(strings.Repeat("c", 64))
	agentETag := core.FormatStrongETag(strings.Repeat("d", 64))

	s := newTestServer(t, func(o *Options) {
		o.AdminAuth = resourceAuth{}
		o.ResourceAdminService = &fakeResourceAdminService{
			currentETagFn: func(_ context.Context, _ core.ResourceKind, id string) (string, bool) {
				if id != "demo" {
					return "", false
				}
				return publishedETag, true
			},
		}
		o.AgentAdminService = &fakeAgentAdminService{
			filesFn: func(context.Context, string) (AgentFileSet, error) {
				return AgentFileSet{ETag: agentETag}, nil
			},
		}
	})

	cases := []struct {
		method string
		path   string
		want   string
	}{
		{http.MethodPut, "/api/v1/model-connections/demo", publishedETag},
		{http.MethodDelete, "/api/v1/model-connections/demo", publishedETag},
		{http.MethodPut, "/api/v1/mcp-servers/demo", publishedETag},
		{http.MethodDelete, "/api/v1/mcp-servers/demo", publishedETag},
		{http.MethodPut, "/api/v1/skills/demo", publishedETag},
		{http.MethodDelete, "/api/v1/skills/demo", publishedETag},
		{http.MethodPost, "/api/v1/resources/skill/demo/publish", publishedETag},
		{http.MethodPost, "/api/v1/agents/ops-agent/files", agentETag},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := serveAdmin(s, resourceCall(tc.method, tc.path, ""))
			require.Equal(t, http.StatusPreconditionFailed, rec.Code)
			env := decodeResourceEnvelope(t, rec)
			require.Equal(t, codePreconditionFailed, env.Code)
			require.Equal(t, ifMatchRequiredMessage, env.Message)
			var data struct {
				CurrentETag string `json:"current_etag"`
			}
			require.NoError(t, json.Unmarshal(env.Data, &data), "body=%s", rec.Body.String())
			require.Equal(t, tc.want, data.CurrentETag)
		})
	}

	t.Run("an unresolvable target carries no data", func(t *testing.T) {
		rec := serveAdmin(s, resourceCall(http.MethodPut, "/api/v1/model-connections/absent", ""))
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codePreconditionFailed, env.Code)
		require.Empty(t, env.Data, "解析不到当前值时不声称 data.current_etag")
	})
}

// TestAdminRoutes_SaveFaceBindingCodes：四条绑定 sentinel 经真实路由
// `POST /agents/{name}/files` 全部落到保存面出口（`writeAgentAdminError` →
// `writeBindingError`），逐条 400 + 契约码，码与文案都来自适配器。同一批 sentinel
// 在准入面是 409 / 503（`TestContract_SaveAndAdmissionSplit`）——本用例只钉「路由接线
// 到了保存面」，不重复契约层的分段表。
// 逆改点：路由改用准入出口 / 某条码退回通用 BAD_REQUEST / 文案被本层重写 → 红。
// 中文用例名：保存面绑定错误码经真实路由。
func TestAdminRoutes_SaveFaceBindingCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"缺绑定", core.ErrAgentResourceBindingMissing, codeAgentResourceBindingMissing},
		{"绑定非法", core.ErrAgentResourceBindingInvalid, codeAgentResourceBindingInvalid},
		{"资源不可用", core.ErrAgentResourceUnavailable, codeAgentResourceUnavailable},
		{"无资格", core.ErrResourceNotEligible, codeResourceNotEligible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, func(o *Options) {
				o.AdminAuth = resourceAuth{}
				o.AgentAdminService = &fakeAgentAdminService{
					submitFn: func(context.Context, core.Actor, string, string, []core.AuthorFile) (AgentFileSet, error) {
						return AgentFileSet{}, &core.AdminError{Kind: tc.err, Message: "适配器文案：" + tc.name}
					},
				}
			})
			req := adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/files",
				`{"files":[{"path":"AGENT.md","content":"# ops-agent"}]}`)
			req.Header.Set("If-Match", core.FormatStrongETag(strings.Repeat("a", 64)))
			rec := serveAdmin(s, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			env := decodeAuthEnvelope(t, rec)
			require.Equal(t, tc.code, env.Code)
			require.Contains(t, env.Message, "适配器文案", "保存面不重写适配器文案")
		})
	}
}

// TestAdminRoutes_NonConditionalRowsSkipIfMatch：24 条非条件写 / 只读行不套
// If-Match 中间件——无请求头时进入处理函数（占位回 500），绝不回 412。
// 逆改点：把条件写中间件误套到整个写面 → 对应行变 412，本用例红。
func TestAdminRoutes_NonConditionalRowsSkipIfMatch(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
	mux := adminTableHandler(s)

	checked := 0
	for _, spec := range s.adminRouteSpecs() {
		if spec.conditional {
			continue
		}
		checked++
		req := withBearer(adminRequest(spec.method, concreteAdminPath(spec.pattern), ""), testToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.NotEqual(t, http.StatusPreconditionFailed, rec.Code, "%s 不要求 If-Match", adminRouteKey(spec))
		require.NotEqual(t, codePreconditionFailed, decodeAuthEnvelope(t, rec).Code, "%s", adminRouteKey(spec))
	}
	require.Equal(t, 24, checked)
}

// TestAdminRoutes_AuthPrecedesIfMatch：认证在条件写中间件之外——缺凭据时无论
// If-Match 是否合法都先得 401；凭据有效后才轮到 412。非条件写行忽略该请求头。
// 逆改点：把 requireIfMatch 包到 s.admin 外层 → 无凭据也变 412，本用例红。
func TestAdminRoutes_AuthPrecedesIfMatch(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
	mux := adminTableHandler(s)
	path := "/api/v1/model-connections/primary" // PUT：条件写

	send := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	missingAuth := send(adminRequest(http.MethodPut, path, ""))
	require.Equal(t, http.StatusUnauthorized, missingAuth.Code)
	require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, missingAuth).Code)

	badHeader := adminRequest(http.MethodPut, path, "")
	badHeader.Header.Set("If-Match", "*")
	unauthorized := send(badHeader)
	require.Equal(t, http.StatusUnauthorized, unauthorized.Code, "缺凭据必须先于 If-Match 判定")
	require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, unauthorized).Code)

	authorizedReq := adminRequest(http.MethodPut, path, "")
	authorizedReq.Header.Set("If-Match", "*")
	authorized := send(withBearer(authorizedReq, testToken))
	require.Equal(t, http.StatusPreconditionFailed, authorized.Code)
	require.Equal(t, codePreconditionFailed, decodeAuthEnvelope(t, authorized).Code)

	nonConditional := adminRequest(http.MethodPost, "/api/v1/notify-channels", "")
	nonConditional.Header.Set("If-Match", "*")
	rec := send(withBearer(nonConditional, testToken))
	require.NotEqual(t, http.StatusPreconditionFailed, rec.Code, "非条件写行忽略 If-Match")
}

// TestAdminRoutes_EveryRowEntersGuardOnce：35 条新端点一条不落地经中央认证包装，
// 且每条请求恰过一次闸门——与主路由表 `TestAuth_RouteMatrix` 同一口径，提前在
// 写面表上锁定挂载方式，避免 T016 合并时出现"表里有、注册漏"的第二套路径。
// 逆改点：某行绕过 s.admin 直接挂处理函数 → 计数少于行数，本用例红。
func TestAdminRoutes_EveryRowEntersGuardOnce(t *testing.T) {
	auth := &countingAuth{}
	s := newTestServer(t, func(o *Options) { o.AdminAuth = auth })
	mux := adminTableHandler(s)
	specs := s.adminRouteSpecs()

	for _, spec := range specs {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, adminRequest(spec.method, concreteAdminPath(spec.pattern), ""))
		require.NotEqual(t, http.StatusUnauthorized, rec.Code, "%s 应经 countingAuth 放行", adminRouteKey(spec))
	}
	require.Equal(t, len(specs), auth.count(), "每条写面路由必须恰过一次认证闸门")
}

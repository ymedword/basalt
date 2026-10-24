package web

// 本文件覆盖出网域名白名单的条件写（US-12 R8 裁决 2 / R8.2.2–R8.2.4）：`GET /sandbox`
// 投影的是**实际生效**列表与元数据；`PUT /sandbox/network-domains` 必须带强 If-Match，
// 400 / 409 / 412 / 500 的载荷逐项固定，成功响应是重新读出的生效状态（不是回显请求体）。
// Sandbox allowlist acceptance: the live projection and the conditional write.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeNetworkAllowlist 是管理端口的内存替身：`Update` 按需改写当前策略，页面读到的
// 就是"服务层落库后"的那一份；调用计数用于断言"零写入时处理函数未被触达"。
// fakeNetworkAllowlist is the in-memory admin port; Update mutates the policy the GET
// path reads, so the response shows the very state the service would have persisted.
type fakeNetworkAllowlist struct {
	policy   core.NetworkDomainPolicy
	updateFn func(context.Context, []string, string, core.Actor) (core.NetworkDomainPolicy, string, error)
	calls    int
	domains  []string
	ifMatch  string
	actor    core.Actor
}

func (f *fakeNetworkAllowlist) Current() core.NetworkDomainPolicy { return f.policy }

func (f *fakeNetworkAllowlist) Update(ctx context.Context, domains []string, ifMatch string,
	actor core.Actor,
) (core.NetworkDomainPolicy, string, error) {
	f.calls++
	f.domains, f.ifMatch, f.actor = domains, ifMatch, actor
	if f.updateFn != nil {
		return f.updateFn(ctx, domains, ifMatch, actor)
	}
	f.policy.Domains = domains
	f.policy.Revision++
	f.policy.OperationID = "op-network-domains"
	f.policy.UpdatedBy = actor.Subject
	return f.policy, "op-network-domains", nil
}

// seededNetworkPolicy 是首次种子行的固定样本（revision 1、无操作行、无写入者）。
func seededNetworkPolicy() core.NetworkDomainPolicy {
	return core.NetworkDomainPolicy{
		Scope:     core.PolicyScopeHTTPDomains,
		Domains:   []string{"api.example.com"},
		Revision:  1,
		UpdatedAt: time.Date(2026, 10, 24, 8, 30, 0, 0, time.UTC),
	}
}

// TestSandbox_LiveAllowlistOverridesConfig —— 装配了管理端口时，`http_allowed_domains`
// 是**当前生效**值（不是启动配置副本），并附带 etag / revision / source / updated_at /
// updated_by。ETag 与 `core.NetworkDomainETag` 同一材料，页面才能直接拿去做 If-Match。
// 逆改点：GET 仍返回配置投影 → 本用例红。
func TestSandbox_LiveAllowlistOverridesConfig(t *testing.T) {
	policy := seededNetworkPolicy()
	policy.Domains = []string{"hooks.example.net"}
	policy.Revision = 3
	policy.OperationID = "op-network-domains"
	policy.UpdatedBy = "token:abc123"

	s := newTestServer(t, func(o *Options) {
		o.Sandbox = SandboxConfig{HTTPAllowedDomains: []string{"stale.example.com"}}
		o.NetworkAllowlist = &fakeNetworkAllowlist{policy: policy}
	})
	env := decodeResourcesEnvelope[SandboxView](t, serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/sandbox", "")))
	require.Equal(t, []string{"hooks.example.net"}, env.Data.HTTPAllowedDomains, "必须投影生效值")
	require.Equal(t, core.NetworkDomainETag(policy), env.Data.NetworkAllowlist.ETag)
	require.Equal(t, int64(3), env.Data.NetworkAllowlist.Revision)
	require.Equal(t, core.NetworkAllowlistSourceDatabase, env.Data.NetworkAllowlist.Source)
	require.Equal(t, "2026-10-24T08:30:00Z", env.Data.NetworkAllowlist.UpdatedAt)
	require.Equal(t, "token:abc123", env.Data.NetworkAllowlist.UpdatedBy)
}

// TestSandbox_SeedMetadata —— 种子行的来源是 config_seed、写入者为空串；零值时间渲染为
// 空串（不出现 `null`）。
// 逆改点：种子行被标成 database / 时间渲染出 null → 红。
func TestSandbox_SeedMetadata(t *testing.T) {
	policy := seededNetworkPolicy()
	s := newTestServer(t, func(o *Options) { o.NetworkAllowlist = &fakeNetworkAllowlist{policy: policy} })
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/sandbox", ""))
	require.NotContains(t, rec.Body.String(), "null")

	env := decodeResourcesEnvelope[SandboxView](t, rec)
	require.Equal(t, core.NetworkAllowlistSourceConfigSeed, env.Data.NetworkAllowlist.Source)
	require.Empty(t, env.Data.NetworkAllowlist.UpdatedBy)

	empty := newTestServer(t, func(o *Options) {
		o.NetworkAllowlist = &fakeNetworkAllowlist{policy: core.NetworkDomainPolicy{Scope: core.PolicyScopeHTTPDomains}}
	})
	view := decodeResourcesEnvelope[SandboxView](t,
		serveAdmin(empty, adminRequest(http.MethodGet, "/api/v1/sandbox", "")))
	require.Empty(t, view.Data.NetworkAllowlist.UpdatedAt, "零值时间渲染为空串")
	require.Empty(t, view.Data.NetworkAllowlist.ETag, "未装载的修订没有可写的 ETag")
	require.Zero(t, view.Data.NetworkAllowlist.Revision)
}

// TestSandbox_NetworkDomainsConditionalWrite —— 成功的条件写：请求体原样交给服务层，
// 成功后响应是**重新读出的**生效视图（域名、修订号、ETag 都来自新快照）。
// 逆改点：响应回显请求体（乐观更新）→ 修订号 / ETag 断言红。
func TestSandbox_NetworkDomainsConditionalWrite(t *testing.T) {
	policy := seededNetworkPolicy()
	fake := &fakeNetworkAllowlist{policy: policy}
	fake.updateFn = func(_ context.Context, domains []string, _ string,
		actor core.Actor,
	) (core.NetworkDomainPolicy, string, error) {
		normalized, err := core.NormalizeDomainList(domains)
		if err != nil {
			return core.NetworkDomainPolicy{}, "", err
		}
		updated := policy
		updated.Domains = normalized
		updated.Revision = policy.Revision + 1
		updated.OperationID = "op-network-domains"
		updated.UpdatedBy = actor.Subject
		fake.policy = updated // 服务层落库后 Current() 立即返回新快照
		return updated, "op-network-domains", nil
	}
	s := newTestServer(t, func(o *Options) { o.NetworkAllowlist = fake })

	req := adminRequest(http.MethodPut, "/api/v1/sandbox/network-domains",
		`{"domains":["Hooks.Example.net","api.example.com","api.example.com"]}`)
	req.Header.Set("If-Match", core.NetworkDomainETag(policy))
	rec := serveAdmin(s, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, 1, fake.calls)
	require.Equal(t, []string{"Hooks.Example.net", "api.example.com", "api.example.com"}, fake.domains,
		"web 层不替服务层归一化：原样转交，校验与规整只有一处")
	require.Equal(t, core.NetworkDomainETag(policy), fake.ifMatch, "If-Match 必须逐字转交")

	env := decodeResourcesEnvelope[SandboxView](t, rec)
	require.Equal(t, []string{"api.example.com", "hooks.example.net"}, env.Data.HTTPAllowedDomains)
	require.Equal(t, int64(2), env.Data.NetworkAllowlist.Revision)
	require.Equal(t, core.NetworkAllowlistSourceDatabase, env.Data.NetworkAllowlist.Source)
	require.Equal(t, core.NetworkDomainETag(fake.policy), env.Data.NetworkAllowlist.ETag)
}

// TestSandbox_NetworkDomainsRequiresIfMatch —— 缺 If-Match 或形状非法一律 412，处理
// 函数零调用（因此零写入、零操作行）。
// 逆改点：把新路由标成非条件写 → 本用例进入处理函数，calls 断言红。
func TestSandbox_NetworkDomainsRequiresIfMatch(t *testing.T) {
	for _, raw := range []string{"", "*", "W/" + core.NetworkDomainETag(seededNetworkPolicy())} {
		t.Run(fmt.Sprintf("If-Match=%q", raw), func(t *testing.T) {
			fake := &fakeNetworkAllowlist{policy: seededNetworkPolicy()}
			s := newTestServer(t, func(o *Options) { o.NetworkAllowlist = fake })
			req := adminRequest(http.MethodPut, "/api/v1/sandbox/network-domains",
				`{"domains":["a.example.com"]}`)
			if raw != "" {
				req.Header.Set("If-Match", raw)
			}
			rec := serveAdmin(s, req)
			require.Equal(t, http.StatusPreconditionFailed, rec.Code)
			require.Equal(t, codePreconditionFailed, decodeResourcesEnvelope[SandboxView](t, rec).Code)
			require.Zero(t, fake.calls, "形状级 412 不得进入处理函数")
		})
	}
}

// TestSandbox_NetworkDomainsStaleCondition —— 陈旧 ETag 由服务层判出：412 带**当前**
// ETag（页面据此重取再改），文案为中文；不带操作标识（预写之前，没有操作行）。
// 逆改点：412 不带 data.current_etag / 透出底层英文 → 红。
func TestSandbox_NetworkDomainsStaleCondition(t *testing.T) {
	policy := seededNetworkPolicy()
	policy.Revision = 4
	policy.OperationID = "op-newer"
	fake := &fakeNetworkAllowlist{policy: policy, updateFn: func(context.Context, []string, string,
		core.Actor,
	) (core.NetworkDomainPolicy, string, error) {
		return core.NetworkDomainPolicy{}, "", fmt.Errorf("%w: stale", core.ErrPreconditionFailed)
	}}
	s := newTestServer(t, func(o *Options) { o.NetworkAllowlist = fake })

	req := adminRequest(http.MethodPut, "/api/v1/sandbox/network-domains", `{"domains":["a.example.com"]}`)
	req.Header.Set("If-Match", core.NetworkDomainETag(seededNetworkPolicy()))
	rec := serveAdmin(s, req)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	var env struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, networkAllowlistStaleMessage, env.Message)
	require.NotContains(t, env.Message, "PRE", "对外文案不得回显底层错误")
	require.Equal(t, core.NetworkDomainETag(policy), env.Data["current_etag"])
	require.NotContains(t, env.Data, "operation_id", "预写之前没有操作行")
}

// TestSandbox_NetworkDomainsValidationFailure —— 校验失败 400 且点名第一处非法项；
// `Tool` 名不外露，也不带操作标识（零写入、零操作行）。
// 逆改点：400 回显 Tool 前缀 / 带 operation_id → 红。
func TestSandbox_NetworkDomainsValidationFailure(t *testing.T) {
	fake := &fakeNetworkAllowlist{policy: seededNetworkPolicy(), updateFn: func(_ context.Context,
		domains []string, _ string, _ core.Actor,
	) (core.NetworkDomainPolicy, string, error) {
		_, err := core.NormalizeDomainList(domains)
		return core.NetworkDomainPolicy{}, "", err
	}}
	s := newTestServer(t, func(o *Options) { o.NetworkAllowlist = fake })

	req := adminRequest(http.MethodPut, "/api/v1/sandbox/network-domains",
		`{"domains":["ok.example.com","https://api.example.com/v1"]}`)
	req.Header.Set("If-Match", core.NetworkDomainETag(seededNetworkPolicy()))
	rec := serveAdmin(s, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var env struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, codeBadRequest, env.Code)
	require.Contains(t, env.Message, "第 2 项", "必须点名第一处非法项")
	require.NotContains(t, env.Message, "network_allowlist", "工具名不外露")
	require.NotContains(t, env.Data, "operation_id")
}

// TestSandbox_NetworkDomainsBodyRules —— 空对象缺 `domains` 与拼错的键都 400 且零调用；
// 显式空数组是合法值（清空白名单 = 全部拒绝），必须转交服务层。
// 逆改点：缺字段按空数组处理 / 空数组被拒 → 对应子例红。
func TestSandbox_NetworkDomainsBodyRules(t *testing.T) {
	fake := &fakeNetworkAllowlist{policy: seededNetworkPolicy()}
	s := newTestServer(t, func(o *Options) { o.NetworkAllowlist = fake })
	ifMatch := core.NetworkDomainETag(seededNetworkPolicy())

	for _, body := range []string{`{}`, `{"domain":["a.example.com"]}`, `{"domains":"a.example.com"}`} {
		req := adminRequest(http.MethodPut, "/api/v1/sandbox/network-domains", body)
		req.Header.Set("If-Match", ifMatch)
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", body)
	}
	require.Zero(t, fake.calls)

	req := adminRequest(http.MethodPut, "/api/v1/sandbox/network-domains", `{"domains":[]}`)
	req.Header.Set("If-Match", ifMatch)
	require.Equal(t, http.StatusOK, serveAdmin(s, req).Code, "显式空数组是合法值")
}

// TestSandbox_NetworkDomainsMissingPort —— 未装配管理端口：GET 退回配置投影，
// PUT 按装配错误 500 失败关闭（不是"成功但没生效"）。
// 逆改点：缺端口时 PUT 回 200 → 红。
func TestSandbox_NetworkDomainsMissingPort(t *testing.T) {
	s := newTestServer(t, nil)
	req := adminRequest(http.MethodPut, "/api/v1/sandbox/network-domains", `{"domains":[]}`)
	req.Header.Set("If-Match", core.NetworkDomainETag(core.NetworkDomainPolicy{}))
	rec := serveAdmin(s, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, codeInternal, decodeResourcesEnvelope[SandboxView](t, rec).Code)
}

// TestSandbox_NetworkDomainsPostPendingFailures —— `pending` 之后的失败一律带
// `operation_id`：修订号 CAS 冲突 → 409 CONFLICT，其余 → 500。409 **不**带
// `current_etag`——落败进程的快照可能落后于数据库，契约只为 412 承诺该字段。
// 逆改点：失败不带操作标识 / 409 回显可能过期的 ETag → 本用例红。
func TestSandbox_NetworkDomainsPostPendingFailures(t *testing.T) {
	policy := seededNetworkPolicy()
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{
			"revision conflict", fmt.Errorf("%w: lost", core.ErrPolicyRevisionConflict),
			http.StatusConflict, codeConflict,
		},
		{"internal", errors.New("store unavailable"), http.StatusInternalServerError, codeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeNetworkAllowlist{policy: policy, updateFn: func(context.Context, []string, string,
				core.Actor,
			) (core.NetworkDomainPolicy, string, error) {
				return core.NetworkDomainPolicy{}, "op-post-pending", tc.err
			}}
			s := newTestServer(t, func(o *Options) { o.NetworkAllowlist = fake })
			req := adminRequest(http.MethodPut, "/api/v1/sandbox/network-domains",
				`{"domains":["a.example.com"]}`)
			req.Header.Set("If-Match", core.NetworkDomainETag(policy))
			rec := serveAdmin(s, req)
			require.Equal(t, tc.want, rec.Code)

			var env struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Data    map[string]any `json:"data"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			require.Equal(t, tc.code, env.Code)
			require.Equal(t, "op-post-pending", env.Data["operation_id"])
			if tc.want == http.StatusConflict {
				require.NotContains(t, env.Data, "current_etag",
					"CAS 落败方不得承诺当前 ETag；页面重新 GET 获取")
			}
			require.NotContains(t, env.Message, "store unavailable", "底层原因只进日志")
		})
	}
}

// TestSandbox_NetworkDomainsRequiresAuth —— 新条件写端点同样在认证闸门之后：缺凭据
// 401 且零调用。
// 逆改点：路由 visibility 写成公开 → 红。
func TestSandbox_NetworkDomainsRequiresAuth(t *testing.T) {
	const token = "sandbox-write-token"
	fake := &fakeNetworkAllowlist{policy: seededNetworkPolicy()}
	s := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(token)
		o.NetworkAllowlist = fake
	})
	req := adminRequest(http.MethodPut, "/api/v1/sandbox/network-domains", `{"domains":[]}`)
	req.Header.Set("If-Match", core.NetworkDomainETag(seededNetworkPolicy()))
	rec := serveAdmin(s, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Zero(t, fake.calls)
}

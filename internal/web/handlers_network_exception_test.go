package web

// 本文件覆盖禁止域名表与内网访问例外的管理端点（14D D-10 / D-13）：禁止表与白名单
// 同一套条件写语义、`denied_domains` / `network_denylist` 投影生效值；例外的集合
// ETag、理由必填、重复 409、删除 404 / 412、预写之后失败带操作标识。状态码与载荷
// 逐项对齐 openapi，底层错误文本不出站。
// Acceptance for both endpoints: the live projection, the fixed status table and the
// payload shapes; underlying error text never reaches the client.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeNetworkExceptions 是例外管理端口的内存替身：`List` / `ETag` 从同一份集合
// 计算（与真实服务同口径），写入委托注入的函数，调用计数用于断言"未触达处理函数"。
// fakeNetworkExceptions is the in-memory port; List and ETag share one collection.
type fakeNetworkExceptions struct {
	list     []core.NetworkException
	createFn func(context.Context, string, string, string, core.Actor) (core.NetworkException, string, error)
	deleteFn func(context.Context, string, string, core.Actor) (string, error)
	kind     string
	value    string
	note     string
	ifMatch  string
	calls    int
}

func (f *fakeNetworkExceptions) List() []core.NetworkException {
	return append(make([]core.NetworkException, 0, len(f.list)), f.list...)
}

func (f *fakeNetworkExceptions) ETag() string { return core.NetworkExceptionETag(f.list) }

func (f *fakeNetworkExceptions) Create(ctx context.Context, kind, value, note string,
	actor core.Actor,
) (core.NetworkException, string, error) {
	f.calls++
	f.kind, f.value, f.note = kind, value, note
	if f.createFn != nil {
		return f.createFn(ctx, kind, value, note, actor)
	}
	exception := core.NetworkException{
		ExceptionID: "exc_00000000000000000000000001",
		Kind:        kind,
		Value:       value,
		Note:        note,
		Revision:    1,
		CreatedAt:   time.Date(2026, 10, 26, 9, 15, 0, 0, time.UTC),
		CreatedBy:   actor.Subject,
		OperationID: "op-exception-create",
	}
	f.list = append(f.list, exception)
	return exception, "op-exception-create", nil
}

func (f *fakeNetworkExceptions) Delete(ctx context.Context, exceptionID, ifMatch string,
	actor core.Actor,
) (string, error) {
	f.calls++
	f.ifMatch = ifMatch
	if f.deleteFn != nil {
		return f.deleteFn(ctx, exceptionID, ifMatch, actor)
	}
	remaining := make([]core.NetworkException, 0, len(f.list))
	for _, item := range f.list {
		if item.ExceptionID != exceptionID {
			remaining = append(remaining, item)
		}
	}
	f.list = remaining
	return "op-exception-delete", nil
}

// seededException 是列表夹具的一条固定例外。
func seededException() core.NetworkException {
	return core.NetworkException{
		ExceptionID: "exc_00000000000000000000000002",
		Kind:        core.NetworkExceptionKindHost,
		Value:       "intranet.example.com",
		Note:        "联调",
		Revision:    1,
		CreatedAt:   time.Date(2026, 10, 26, 9, 15, 0, 0, time.UTC),
		CreatedBy:   "token:abc123",
		OperationID: "op-exception-seed",
	}
}

// errorEnvelope 是失败信封的解码形状（断言 code / message / data 用）。
type errorEnvelope struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

// decodeErrorEnvelope 解码任意端点的失败信封。
func decodeErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env
}

// TestExecutionProfile_DeniedDomainsAPI 覆盖 D-10 的 HTTP 面：`GET /sandbox` 投影
// 禁止表生效值与其元数据；`PUT /sandbox/denied-domains` 的条件写语义与白名单逐条
// 一致（If-Match 必填且逐字转交、400 点名非法项、412 带**禁止表**当前 ETag、
// 409 只带操作标识、缺端口 500），成功响应是重新读出的生效视图。
// Acceptance (D10): the denylist write mirrors the allowlist table and the read
// projection carries the live denylist row.
func TestExecutionProfile_DeniedDomainsAPI(t *testing.T) {
	deniedPolicy := core.NetworkDomainPolicy{
		Scope:     core.PolicyScopeHTTPDeniedDomains,
		Domains:   []string{"*.tracker.example.com"},
		Revision:  2,
		UpdatedAt: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
	}

	t.Run("sandbox_view_projects_denylist", func(t *testing.T) {
		allowlist := &fakeNetworkAllowlist{policy: seededNetworkPolicy()}
		denylist := &fakeNetworkAllowlist{policy: deniedPolicy}
		s := newTestServer(t, func(o *Options) {
			o.Sandbox = SandboxConfig{HTTPAllowedDomains: []string{"stale.example.com"}}
			o.NetworkAllowlist = allowlist
			o.NetworkDenylist = denylist
		})
		env := decodeResourcesEnvelope[SandboxView](t,
			serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/sandbox", "")))
		require.Equal(t, []string{"*.tracker.example.com"}, env.Data.DeniedDomains)
		require.Equal(t, core.NetworkDomainETag(deniedPolicy), env.Data.NetworkDenylist.ETag)
		require.Equal(t, int64(2), env.Data.NetworkDenylist.Revision)
		require.Equal(t, "2026-10-26T09:00:00Z", env.Data.NetworkDenylist.UpdatedAt)
		// 白名单与禁止表各读各的端口，互不覆盖。
		require.Equal(t, []string{"api.example.com"}, env.Data.HTTPAllowedDomains)
	})

	t.Run("conditional_write", func(t *testing.T) {
		policy := deniedPolicy
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
			updated.OperationID = "op-denylist"
			updated.UpdatedBy = actor.Subject
			fake.policy = updated
			return updated, "op-denylist", nil
		}
		s := newTestServer(t, func(o *Options) { o.NetworkDenylist = fake })

		// If-Match 缺失 / 弱 / 通配：412 且处理函数零调用。
		for _, raw := range []string{"", "*", "W/" + core.NetworkDomainETag(policy)} {
			req := adminRequest(http.MethodPut, "/api/v1/sandbox/denied-domains",
				`{"domains":["evil.example.org"]}`)
			if raw != "" {
				req.Header.Set("If-Match", raw)
			}
			require.Equal(t, http.StatusPreconditionFailed, serveAdmin(s, req).Code)
		}
		require.Zero(t, fake.calls)

		req := adminRequest(http.MethodPut, "/api/v1/sandbox/denied-domains",
			`{"domains":["Evil.Example.org","*.tracker.example.com","*.tracker.example.com"]}`)
		req.Header.Set("If-Match", core.NetworkDomainETag(policy))
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, 1, fake.calls)
		require.Equal(t, core.NetworkDomainETag(policy), fake.ifMatch, "If-Match 必须逐字转交")
		require.Equal(t, []string{"Evil.Example.org", "*.tracker.example.com", "*.tracker.example.com"},
			fake.domains, "web 层不替服务层归一化")

		env := decodeResourcesEnvelope[SandboxView](t, rec)
		require.Equal(t, []string{"*.tracker.example.com", "evil.example.org"}, env.Data.DeniedDomains)
		require.Equal(t, int64(3), env.Data.NetworkDenylist.Revision)
		require.Equal(t, core.NetworkDomainETag(fake.policy), env.Data.NetworkDenylist.ETag)
	})

	t.Run("failure_table", func(t *testing.T) {
		policy := deniedPolicy
		cases := []struct {
			name        string
			body        string
			err         error
			operationID string
			want        int
			code        string
		}{
			{
				"invalid domain", `{"domains":["ok.example.com","https://bad/x"]}`, nil, "",
				http.StatusBadRequest, codeBadRequest,
			},
			{
				"stale etag", `{"domains":["a.example.com"]}`,
				fmt.Errorf("%w: stale", core.ErrPreconditionFailed), "",
				http.StatusPreconditionFailed, codePreconditionFailed,
			},
			{
				"revision conflict", `{"domains":["a.example.com"]}`,
				fmt.Errorf("%w: lost", core.ErrPolicyRevisionConflict), "op-denylist-loser",
				http.StatusConflict, codeConflict,
			},
			{
				"internal after pre-write", `{"domains":["a.example.com"]}`, errors.New("store down"),
				"op-denylist-error", http.StatusInternalServerError, codeInternal,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fake := &fakeNetworkAllowlist{policy: policy}
				fake.updateFn = func(_ context.Context, domains []string, _ string,
					_ core.Actor,
				) (core.NetworkDomainPolicy, string, error) {
					if tc.err == nil {
						_, err := core.NormalizeDomainList(domains)
						return core.NetworkDomainPolicy{}, "", err
					}
					return core.NetworkDomainPolicy{}, tc.operationID, tc.err
				}
				s := newTestServer(t, func(o *Options) { o.NetworkDenylist = fake })
				req := adminRequest(http.MethodPut, "/api/v1/sandbox/denied-domains", tc.body)
				req.Header.Set("If-Match", core.NetworkDomainETag(policy))
				rec := serveAdmin(s, req)
				require.Equal(t, tc.want, rec.Code, rec.Body.String())

				env := decodeErrorEnvelope(t, rec)
				require.Equal(t, tc.code, env.Code)
				if tc.want == http.StatusPreconditionFailed {
					require.Equal(t, core.NetworkDomainETag(policy), env.Data["current_etag"],
						"412 必须给出禁止表自己的当前 ETag")
					require.NotContains(t, env.Data, "operation_id")
				}
				if tc.operationID != "" {
					require.Equal(t, tc.operationID, env.Data["operation_id"])
				}
				require.NotContains(t, env.Message, "store down", "底层原因只进日志")
			})
		}
	})

	t.Run("missing_port_fails_closed", func(t *testing.T) {
		s := newTestServer(t, nil)
		req := adminRequest(http.MethodPut, "/api/v1/sandbox/denied-domains", `{"domains":[]}`)
		req.Header.Set("If-Match", core.NetworkDomainETag(core.NetworkDomainPolicy{}))
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, codeInternal, decodeResourcesEnvelope[SandboxView](t, rec).Code)
	})
}

// TestExecutionProfile_NetworkExceptionsAPI 覆盖 D-13 的 HTTP 面：列表与集合 ETag、
// 新增（理由必填、重复 409）、删除（If-Match 必填、404 / 412 / 409 映射）与装配
// 缺失时的 fail-closed；成功响应的载荷形状与 openapi 逐字段对齐。
// Acceptance (D13): the list/etag shape, create and delete status codes, and a
// fail-closed read when the port is not assembled.
func TestExecutionProfile_NetworkExceptionsAPI(t *testing.T) {
	t.Run("list_and_empty_shape", func(t *testing.T) {
		fake := &fakeNetworkExceptions{list: []core.NetworkException{seededException()}}
		s := newTestServer(t, func(o *Options) { o.NetworkExceptions = fake })
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/network-exceptions", ""))
		require.Equal(t, http.StatusOK, rec.Code)
		env := decodeResourcesEnvelope[NetworkExceptionListView](t, rec)
		require.Equal(t, core.NetworkExceptionETag(fake.list), env.Data.ETag)
		require.Len(t, env.Data.Exceptions, 1)
		require.Equal(t, seededException().ExceptionID, env.Data.Exceptions[0].ExceptionID)
		require.Equal(t, "2026-10-26T09:15:00Z", env.Data.Exceptions[0].CreatedAt)

		empty := newTestServer(t, func(o *Options) { o.NetworkExceptions = &fakeNetworkExceptions{} })
		rec = serveAdmin(empty, adminRequest(http.MethodGet, "/api/v1/network-exceptions", ""))
		require.NotContains(t, rec.Body.String(), "null", "空集合输出 [] 而不是 null")
		emptyEnv := decodeResourcesEnvelope[NetworkExceptionListView](t, rec)
		require.Empty(t, emptyEnv.Data.Exceptions)
		require.NotEmpty(t, emptyEnv.Data.ETag, "空集合同样有稳定的集合 ETag")
	})

	t.Run("create_success_and_body_rules", func(t *testing.T) {
		fake := &fakeNetworkExceptions{}
		s := newTestServer(t, func(o *Options) { o.NetworkExceptions = fake })

		// 缺字段 400 且零调用（不进入服务层，也就没有预写）。字段存在但理由是空白的
		// 情形归服务层判（本层不替它裁剪或校验内容）。
		for _, body := range []string{
			`{}`, `{"kind":"host","value":"a.example.com"}`,
			`{"value":"a.example.com","note":"联调"}`,
		} {
			req := adminRequest(http.MethodPost, "/api/v1/network-exceptions", body)
			require.Equal(t, http.StatusBadRequest, serveAdmin(s, req).Code, "body=%s", body)
		}
		require.Zero(t, fake.calls)

		req := adminRequest(http.MethodPost, "/api/v1/network-exceptions",
			`{"kind":"host","value":"a.example.com","note":"联调"}`)
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "host", fake.kind)
		require.Equal(t, "a.example.com", fake.value)
		require.Equal(t, "联调", fake.note)

		env := decodeResourcesEnvelope[NetworkExceptionCreateResult](t, rec)
		require.Equal(t, core.NetworkExceptionETag(fake.list), env.Data.ETag)
		require.Equal(t, "op-exception-create", env.Data.Exception.OperationID)
		require.Equal(t, "联调", env.Data.Exception.Note)
	})

	t.Run("create_failure_table", func(t *testing.T) {
		cases := []struct {
			name        string
			err         error
			operationID string
			want        int
			code        string
		}{
			{
				"invalid value", &core.InvalidInputError{Tool: "network_exception", Reason: "值非法"}, "",
				http.StatusBadRequest, codeBadRequest,
			},
			{
				"duplicate", fmt.Errorf("%w: 例外已存在", core.ErrConflict), "",
				http.StatusConflict, codeConflict,
			},
			{
				"unique conflict after pre-write", fmt.Errorf("%w: 唯一键", core.ErrPolicyRevisionConflict),
				"op-exception-loser", http.StatusConflict, codeConflict,
			},
			{
				"internal after pre-write", errors.New("store down"), "op-exception-error",
				http.StatusInternalServerError, codeInternal,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fake := &fakeNetworkExceptions{}
				fake.createFn = func(context.Context, string, string, string,
					core.Actor,
				) (core.NetworkException, string, error) {
					return core.NetworkException{}, tc.operationID, tc.err
				}
				s := newTestServer(t, func(o *Options) { o.NetworkExceptions = fake })
				rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/network-exceptions",
					`{"kind":"host","value":"a.example.com","note":"联调"}`))
				require.Equal(t, tc.want, rec.Code, rec.Body.String())
				env := decodeErrorEnvelope(t, rec)
				require.Equal(t, tc.code, env.Code)
				if tc.operationID != "" {
					require.Equal(t, tc.operationID, env.Data["operation_id"])
				} else {
					require.NotContains(t, env.Data, "operation_id")
				}
				require.NotContains(t, env.Message, "store down", "底层原因只进日志")
			})
		}
	})

	t.Run("delete_success_and_if_match", func(t *testing.T) {
		exception := seededException()
		fake := &fakeNetworkExceptions{list: []core.NetworkException{exception}}
		s := newTestServer(t, func(o *Options) { o.NetworkExceptions = fake })

		path := "/api/v1/network-exceptions/" + exception.ExceptionID
		for _, raw := range []string{"", "*", "W/" + fake.ETag()} {
			req := adminRequest(http.MethodDelete, path, "")
			if raw != "" {
				req.Header.Set("If-Match", raw)
			}
			require.Equal(t, http.StatusPreconditionFailed, serveAdmin(s, req).Code)
		}
		require.Zero(t, fake.calls, "形状级 412 不得进入处理函数")

		etag := fake.ETag()
		req := adminRequest(http.MethodDelete, path, "")
		req.Header.Set("If-Match", etag)
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, etag, fake.ifMatch)
		env := decodeResourcesEnvelope[NetworkExceptionDeleteResult](t, rec)
		require.Equal(t, exception.ExceptionID, env.Data.ExceptionID)
		require.Equal(t, "op-exception-delete", env.Data.OperationID)
		require.Equal(t, core.NetworkExceptionETag(nil), env.Data.ETag, "删除后返回新集合的 ETag")
	})

	t.Run("delete_failure_table", func(t *testing.T) {
		exception := seededException()
		cases := []struct {
			name        string
			err         error
			operationID string
			want        int
			code        string
		}{
			{
				"stale collection etag", fmt.Errorf("%w: stale", core.ErrPreconditionFailed), "",
				http.StatusPreconditionFailed, codePreconditionFailed,
			},
			{
				"not found", fmt.Errorf("%w: 例外 %s", core.ErrResourceNotFound, exception.ExceptionID), "",
				http.StatusNotFound, codeNotFound,
			},
			{
				"concurrent delete", fmt.Errorf("%w: 行已不在", core.ErrPolicyRevisionConflict),
				"op-exception-delete-loser", http.StatusConflict, codeConflict,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fake := &fakeNetworkExceptions{list: []core.NetworkException{exception}}
				fake.deleteFn = func(context.Context, string, string,
					core.Actor,
				) (string, error) {
					return tc.operationID, tc.err
				}
				s := newTestServer(t, func(o *Options) { o.NetworkExceptions = fake })
				req := adminRequest(http.MethodDelete,
					"/api/v1/network-exceptions/"+exception.ExceptionID, "")
				req.Header.Set("If-Match", fake.ETag())
				rec := serveAdmin(s, req)
				require.Equal(t, tc.want, rec.Code, rec.Body.String())
				env := decodeErrorEnvelope(t, rec)
				require.Equal(t, tc.code, env.Code)
				if tc.want == http.StatusPreconditionFailed {
					require.Equal(t, fake.ETag(), env.Data["current_etag"],
						"412 给出当前集合 ETag，页面据此重取")
				}
				if tc.operationID != "" {
					require.Equal(t, tc.operationID, env.Data["operation_id"])
				}
			})
		}
	})

	t.Run("missing_port_fails_closed", func(t *testing.T) {
		s := newTestServer(t, nil)
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/network-exceptions", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code,
			"缺端口是装配错误；不得用空清单冒充没有例外")
		require.Equal(t, codeInternal, decodeErrorEnvelope(t, rec).Code)
	})
}

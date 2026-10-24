package web

// 本文件是撤销托管凭据与切换使用范围两条状态动作端点的 handler 级验收
// （颗粒度文档 §3.2.1 handlers_credentials_test.go，4 例）：动作字面量的来源、
// 错误码映射、响应里没有凭据材料、以及条件写在两条端点上的完整语义。
// 断言分三层：
//   - 形状与路由：两类资源各自把 `kind` 原样交给状态动作端口（动作字面量因此由
//     适配器按类别选，本层不维护第二份表）；条件写中间件先于处理函数；
//   - 固定错误码：只认 sentinel / 类型，映射逐字对应契约（约束 43）；
//   - 零副作用：纯请求校验失败（confirm 非 true、请求体含未知字段、If-Match 非法）
//     不触达端口，因此不可能写操作行；成功响应只回状态与计数，凭据材料没有载体。
// This file is the handler-level acceptance surface for the two state actions: kind
// pass-through, fixed error mapping, no credential material, and conditional writes.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errFakeResourceStateUnused 标记用例没有为该端口方法准备行为。
// errFakeResourceStateUnused marks a port method the case never intended to call.
var errFakeResourceStateUnused = errors.New("fake resource state: unexpected call")

// fakeResourceStateService 是可注入行为的 `ResourceStateService` 替身：三个方法各自
// 计调用次数，用来证明纯请求校验失败时端口零调用。
// fakeResourceStateService is a behavior-injected state-action port that counts calls.
type fakeResourceStateService struct {
	currentETagFn func(context.Context, string, string) (string, bool)
	revokeFn      func(context.Context, core.Actor, string, string, CredentialRevokeWrite, string) (CredentialRevokeView, error)
	scopeFn       func(context.Context, core.Actor, string, string, ResourceScopeWrite, string) (ResourceScopeView, error)

	revokeCalls int
	scopeCalls  int
}

// CurrentETag 委托条件 ETag 解析。
func (f *fakeResourceStateService) CurrentETag(ctx context.Context, kind, name string) (string, bool) {
	if f.currentETagFn == nil {
		return "", false
	}
	return f.currentETagFn(ctx, kind, name)
}

// RevokeCredential 委托撤销行为并计数。
func (f *fakeResourceStateService) RevokeCredential(ctx context.Context, actor core.Actor,
	kind, name string, write CredentialRevokeWrite, ifMatch string,
) (CredentialRevokeView, error) {
	f.revokeCalls++
	if f.revokeFn == nil {
		return CredentialRevokeView{}, errFakeResourceStateUnused
	}
	return f.revokeFn(ctx, actor, kind, name, write, ifMatch)
}

// ConvertScope 委托范围切换行为并计数。
func (f *fakeResourceStateService) ConvertScope(ctx context.Context, actor core.Actor,
	kind, name string, write ResourceScopeWrite, ifMatch string,
) (ResourceScopeView, error) {
	f.scopeCalls++
	if f.scopeFn == nil {
		return ResourceScopeView{}, errFakeResourceStateUnused
	}
	return f.scopeFn(ctx, actor, kind, name, write, ifMatch)
}

// 编译期断言：假件实现被测端口——端口漂移在编译期就红。
var _ ResourceStateService = (*fakeResourceStateService)(nil)

// newResourceStateHarness 构造固定时钟、注入状态动作端口的服务器；测试令牌由
// `resourceAuth` 放行（与其余写面用例同一套认证链）。
// newResourceStateHarness assembles the fixture with the injected state port.
func newResourceStateHarness(t *testing.T) (*Server, *fakeResourceStateService) {
	t.Helper()
	fake := &fakeResourceStateService{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = resourceAuth{}
		o.ResourceStateService = fake
	})
	return srv, fake
}

// resourceStateETag 是一个形状合法的强 ETag，供条件写用例使用。
// resourceStateETag is a well-formed strong ETag for conditional-write cases.
func resourceStateETag() string { return formatETag(strings.Repeat("a", 64)) }

// resourceStatePreconditionError 构造适配器在资源锁内判定陈旧值时的 412 载体
// （与 app 层 `preconditionError` 同形：只带 `data.current_etag`）。
// resourceStatePreconditionError mirrors the adapter's stale-ETag carrier.
func resourceStatePreconditionError(currentETag string) error {
	return &core.AdminError{
		Kind:    core.ErrPreconditionFailed,
		Message: "预条件失败：If-Match 与当前发布版本不一致",
		Data:    map[string]any{"current_etag": currentETag},
	}
}

// resourceStateCall 发送一个带 If-Match 的已认证请求；`ifMatch` 为空表示不带请求头。
// resourceStateCall sends an authenticated request with the given If-Match value.
func resourceStateCall(t *testing.T, s *Server, method, path, body, ifMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := resourceCall(method, path, body)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	return serveAdmin(s, req)
}

// TestCredentialHandlers_ActionLiterals：两类资源分别走撤销与范围切换端点时，`kind`
// **原样**进入状态动作端口——动作字面量（`model_connection.credential.revoke` /
// `notify_channel.scope`）由适配器按类别选择，web 层不维护第二份类别表；路径名、
// `resource_uid`、`confirm` 与 If-Match 原件同样原样下传。成功响应带契约声明的
// `ETag` 响应头，撤销结果体里**没有** etag 字段（契约不同形状），范围结果体里
// `etag` 与响应头一致。
// 逆改点：handler 自行解析类别 / 改写 If-Match / 在响应体里补 etag → 对应断言红。
func TestCredentialHandlers_ActionLiterals(t *testing.T) {
	t.Parallel()
	etag := resourceStateETag()

	for _, kind := range []string{"model_connection", "notify_channel"} {
		t.Run("revoke "+kind, func(t *testing.T) {
			t.Parallel()
			s, fake := newResourceStateHarness(t)
			var gotKind, gotName, gotUID, gotIfMatch string
			fake.revokeFn = func(_ context.Context, _ core.Actor, k, name string,
				write CredentialRevokeWrite, ifMatch string,
			) (CredentialRevokeView, error) {
				gotKind, gotName = k, name
				gotUID, gotIfMatch = write.ResourceUID, ifMatch
				require.True(t, write.Confirm, "confirm 必须原样到达端口")
				return CredentialRevokeView{
					ResourceUID:     write.ResourceUID,
					CredentialRef:   "cred-1",
					CredentialState: string(core.CredentialIdentityRevoked),
					RevokedVersions: 2,
					RevocationEpoch: 3,
					ETag:            etag,
				}, nil
			}

			rec := resourceStateCall(t, s, http.MethodPost,
				"/api/v1/resources/"+kind+"/main/credential/revoke",
				`{"resource_uid":"ru-1","confirm":true}`, etag)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.Equal(t, kind, gotKind, "类别必须原样到达端口")
			require.Equal(t, "main", gotName)
			require.Equal(t, "ru-1", gotUID)
			require.Equal(t, etag, gotIfMatch, "If-Match 必须逐字下传，不得归一化")

			require.Equal(t, etag, rec.Header().Get("ETag"))
			env := decodeResourceEnvelope(t, rec)
			require.Equal(t, codeOK, env.Code)
			var view CredentialRevokeView
			require.NoError(t, json.Unmarshal(env.Data, &view), "body=%s", rec.Body.String())
			require.Equal(t, "ru-1", view.ResourceUID)
			require.Equal(t, "cred-1", view.CredentialRef)
			require.Equal(t, "revoked", view.CredentialState)
			require.Equal(t, 2, view.RevokedVersions)
			require.Equal(t, 3, view.RevocationEpoch)

			var raw map[string]any
			require.NoError(t, json.Unmarshal(env.Data, &raw), "body=%s", rec.Body.String())
			require.NotContains(t, raw, "etag", "撤销结果体没有 etag 字段，ETag 只进响应头")
		})

		t.Run("scope "+kind, func(t *testing.T) {
			t.Parallel()
			s, fake := newResourceStateHarness(t)
			var gotKind, gotName, gotScope, gotOwner, gotIfMatch string
			fake.scopeFn = func(_ context.Context, _ core.Actor, k, name string,
				write ResourceScopeWrite, ifMatch string,
			) (ResourceScopeView, error) {
				gotKind, gotName = k, name
				gotScope, gotOwner, gotIfMatch = write.Scope, write.OwnerAgent, ifMatch
				return ResourceScopeView{
					ResourceUID: write.ResourceUID, Scope: write.Scope,
					OwnerAgent: write.OwnerAgent, Revision: 7, ETag: etag,
					AffectedEligibleAgents: []string{},
				}, nil
			}

			rec := resourceStateCall(t, s, http.MethodPost,
				"/api/v1/resources/"+kind+"/main/scope",
				`{"resource_uid":"ru-1","scope":"exclusive","owner_agent":"ops-agent"}`, etag)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			require.Equal(t, kind, gotKind)
			require.Equal(t, "main", gotName)
			require.Equal(t, "exclusive", gotScope)
			require.Equal(t, "ops-agent", gotOwner)
			require.Equal(t, etag, gotIfMatch)

			require.Equal(t, etag, rec.Header().Get("ETag"))
			var view ResourceScopeView
			require.NoError(t, json.Unmarshal(decodeResourceEnvelope(t, rec).Data, &view))
			require.Equal(t, etag, view.ETag, "范围结果的 etag 同时进响应体与响应头")
			require.Equal(t, 7, view.Revision)
			require.NotNil(t, view.AffectedEligibleAgents)
		})
	}
}

// TestCredentialHandlers_ErrorCodeMapping：两条端点共用 `writeResourceAdminError`
// 的映射逐字成立——404 / 400（含四条凭据与范围专用码）/ 409（占用、身份、通用冲突）/
// 412 + `data.current_etag` / 503 / 504 / 500。`pending` 之后的失败带
// `operation_id`（`AdminError.Data` 白名单里唯一能带上它的键）。
// 逆改点：把专用码并进通用 409 / 412 丢 current_etag / 5xx 回显底层原因 → 红。
func TestCredentialHandlers_ErrorCodeMapping(t *testing.T) {
	t.Parallel()
	currentETag := resourceStateETag()

	cases := []struct {
		name      string
		inject    error
		wantCode  string
		wantHTTP  int
		wantData  map[string]string
		scopeSide bool
	}{
		{name: "not found", inject: core.ErrResourceNotFound, wantHTTP: http.StatusNotFound, wantCode: codeNotFound},
		{name: "invalid input", inject: &core.InvalidInputError{Tool: "test", Reason: "资源名不得为空"}, wantHTTP: http.StatusBadRequest, wantCode: codeBadRequest},
		{name: "credential mode required", inject: core.ErrCredentialModeRequired, wantHTTP: http.StatusBadRequest, wantCode: codeCredentialModeRequired},
		{name: "credential revoked", inject: core.ErrCredentialRevoked, wantHTTP: http.StatusBadRequest, wantCode: codeCredentialRevoked},
		{name: "scope in use", inject: core.ErrResourceScopeInUse, wantHTTP: http.StatusConflict, wantCode: codeResourceScopeInUse, scopeSide: true},
		{name: "credential owner mismatch", inject: core.ErrCredentialOwnerMismatch, wantHTTP: http.StatusConflict, wantCode: codeCredentialOwnerMismatch},
		{name: "credential unavailable", inject: core.ErrCredentialUnavailable, wantHTTP: http.StatusServiceUnavailable, wantCode: codeCredentialUnavailable},
		{
			name:     "precondition failed",
			inject:   resourceStatePreconditionError(currentETag),
			wantHTTP: http.StatusPreconditionFailed, wantCode: codePreconditionFailed,
			wantData: map[string]string{"current_etag": currentETag},
		},
		{
			name: "uid mismatch",
			inject: &core.AdminError{
				Kind:    core.ErrResourceUIDMismatch,
				Message: "resource_uid 与当前身份不一致",
				Data:    map[string]any{"resource_uid": "ru-current"},
			},
			wantHTTP: http.StatusConflict, wantCode: codeResourceUIDMismatch,
			wantData: map[string]string{"resource_uid": "ru-current"},
		},
		{name: "conflict", inject: core.ErrConflict, wantHTTP: http.StatusConflict, wantCode: codeConflict},
		{name: "not configured", inject: core.ErrProviderNotConfigured, wantHTTP: http.StatusServiceUnavailable, wantCode: codeNotConfigured},
		{name: "timeout", inject: context.DeadlineExceeded, wantHTTP: http.StatusGatewayTimeout, wantCode: codeTimeout},
		{name: "internal", inject: errors.New("boom: disk full at /secret/path"), wantHTTP: http.StatusInternalServerError, wantCode: codeInternal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, fake := newResourceStateHarness(t)
			fake.revokeFn = func(context.Context, core.Actor, string, string, CredentialRevokeWrite, string) (CredentialRevokeView, error) {
				return CredentialRevokeView{}, tc.inject
			}
			fake.scopeFn = func(context.Context, core.Actor, string, string, ResourceScopeWrite, string) (ResourceScopeView, error) {
				return ResourceScopeView{}, tc.inject
			}

			path := "/api/v1/resources/model_connection/main/credential/revoke"
			body := `{"resource_uid":"ru-1","confirm":true}`
			if tc.scopeSide {
				path = "/api/v1/resources/model_connection/main/scope"
				body = `{"resource_uid":"ru-1","scope":"exclusive","owner_agent":"ops-agent"}`
			}
			rec := resourceStateCall(t, s, http.MethodPost, path, body, resourceStateETag())
			require.Equal(t, tc.wantHTTP, rec.Code, "body=%s", rec.Body.String())
			env := decodeResourceEnvelope(t, rec)
			require.Equal(t, tc.wantCode, env.Code)

			if len(tc.wantData) > 0 {
				var data map[string]any
				require.NoError(t, json.Unmarshal(env.Data, &data), "body=%s", rec.Body.String())
				for key, want := range tc.wantData {
					require.Equal(t, want, data[key], "data.%s", key)
				}
			}
			require.NotContains(t, rec.Body.String(), "/secret/path",
				"5xx 不得回显底层原因（只进日志）")
		})
	}

	t.Run("post-pending failure carries the operation id", func(t *testing.T) {
		t.Parallel()
		s, fake := newResourceStateHarness(t)
		fake.scopeFn = func(context.Context, core.Actor, string, string, ResourceScopeWrite, string) (ResourceScopeView, error) {
			return ResourceScopeView{}, &core.AdminError{
				Kind:    core.ErrResourceScopeInUse,
				Message: "resource scope is in use: 仍被 other-agent 引用",
				Data:    map[string]any{"operation_id": "op-42"},
			}
		}
		rec := resourceStateCall(t, s, http.MethodPost, "/api/v1/resources/model_connection/main/scope",
			`{"resource_uid":"ru-1","scope":"exclusive","owner_agent":"ops-agent"}`,
			resourceStateETag())
		require.Equal(t, http.StatusConflict, rec.Code)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codeResourceScopeInUse, env.Code)
		var data map[string]any
		require.NoError(t, json.Unmarshal(env.Data, &data), "body=%s", rec.Body.String())
		require.Equal(t, "op-42", data["operation_id"], "pending 之后的失败必须可追查")
	})
}

// TestCredentialHandlers_WriteOnlyFields：撤销请求体只接受 `resource_uid` 与
// `confirm`——任何凭据值字段（`value` / `api_key`）都是未知字段，按 400 拒绝且
// **不**触达端口（本层能把明文拦在写面之外）；`confirm` 不是字面 true（缺省 /
// false）同样 400 且零调用。成功响应只回状态与计数，没有任何地址 / 密钥载体。
// 逆改点：放行未知字段 / 把 confirm 缺省当确认 / 视图里带凭据材料 → 红。
func TestCredentialHandlers_WriteOnlyFields(t *testing.T) {
	t.Parallel()
	etag := resourceStateETag()

	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "credential value is not accepted", body: `{"resource_uid":"ru-1","confirm":true,"value":"sk-live-plaintext"}`},
		{name: "api key is not accepted", body: `{"resource_uid":"ru-1","confirm":true,"api_key":"${OPS_KEY}"}`},
		{name: "confirm defaults to false", body: `{"resource_uid":"ru-1"}`},
		{name: "confirm false is rejected", body: `{"resource_uid":"ru-1","confirm":false}`},
		{name: "unknown credential field on scope", body: `{"resource_uid":"ru-1","scope":"shared","credential":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, fake := newResourceStateHarness(t)
			path := "/api/v1/resources/model_connection/main/credential/revoke"
			if strings.Contains(tc.body, "scope") {
				path = "/api/v1/resources/model_connection/main/scope"
			}
			rec := resourceStateCall(t, s, http.MethodPost, path, tc.body, etag)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			require.Equal(t, codeBadRequest, decodeResourceEnvelope(t, rec).Code)
			require.Zero(t, fake.revokeCalls, "纯请求校验失败不得触达端口")
			require.Zero(t, fake.scopeCalls, "纯请求校验失败不得触达端口")
			require.NotContains(t, rec.Body.String(), "sk-live-plaintext")
		})
	}

	t.Run("success has no credential carrier", func(t *testing.T) {
		t.Parallel()
		s, fake := newResourceStateHarness(t)
		fake.revokeFn = func(_ context.Context, _ core.Actor, _ string, _ string,
			write CredentialRevokeWrite, _ string,
		) (CredentialRevokeView, error) {
			return CredentialRevokeView{
				ResourceUID: write.ResourceUID, CredentialRef: "cred-9",
				CredentialState: string(core.CredentialIdentityRevoked),
				RevokedVersions: 1, RevocationEpoch: 1, ETag: etag,
			}, nil
		}
		rec := resourceStateCall(t, s, http.MethodPost,
			"/api/v1/resources/notify_channel/ops-hook/credential/revoke",
			`{"resource_uid":"ru-2","confirm":true}`, etag)
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var raw map[string]any
		require.NoError(t, json.Unmarshal(decodeResourceEnvelope(t, rec).Data, &raw))
		for _, forbidden := range []string{"value", "api_key", "url", "last4", "secret"} {
			require.NotContains(t, raw, forbidden, "成功响应不得携带凭据材料：%s", forbidden)
		}
	})
}

// TestScopeHandlers_ConditionalWrite：两条端点都是条件写——
//   - `If-Match` 缺失 / 弱 / 通配 / 非法形状由中间件在**处理函数之前**拒绝为 412，
//     端口零调用（因此零操作行、零写入）；
//   - 合法形状原样下传，陈旧值由适配器在资源锁内判 412，并带 `data.current_etag`
//     （形状级 412 的当前值来自状态动作端口，不是 web 自己拼装）；
//   - 成功时 `ETag` 响应头与 `data.etag` 同值。
//
// 逆改点：漏套中间件 / 弱 ETag 放行 / 形状级 412 不带当前值 / 成功不回 ETag 头 → 红。
func TestScopeHandlers_ConditionalWrite(t *testing.T) {
	t.Parallel()
	etag := resourceStateETag()
	const scopePath = "/api/v1/resources/notify_channel/ops-hook/scope"

	t.Run("missing If-Match is rejected before the handler", func(t *testing.T) {
		t.Parallel()
		s, fake := newResourceStateHarness(t)
		rec := resourceStateCall(t, s, http.MethodPost, scopePath,
			`{"resource_uid":"ru-1","scope":"shared"}`, "")
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)
		require.Equal(t, codePreconditionFailed, decodeResourceEnvelope(t, rec).Code)
		require.Zero(t, fake.scopeCalls)
	})

	t.Run("non-literal shapes are rejected before the handler", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{"*", "W/" + etag, etag + ", " + etag} {
			s, fake := newResourceStateHarness(t)
			rec := resourceStateCall(t, s, http.MethodPost, scopePath,
				`{"resource_uid":"ru-1","scope":"shared"}`, raw)
			require.Equal(t, http.StatusPreconditionFailed, rec.Code, "If-Match=%q", raw)
			require.Zero(t, fake.scopeCalls, "If-Match=%q", raw)
		}
	})

	t.Run("shape-level 412 reports the current etag from the state port", func(t *testing.T) {
		t.Parallel()
		s, fake := newResourceStateHarness(t)
		fake.currentETagFn = func(_ context.Context, kind, name string) (string, bool) {
			if kind != "notify_channel" || name != "ops-hook" {
				return "", false
			}
			return etag, true
		}
		rec := resourceStateCall(t, s, http.MethodPost, scopePath,
			`{"resource_uid":"ru-1","scope":"shared"}`, "")
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)
		require.Equal(t, ifMatchRequiredMessage, decodeResourceEnvelope(t, rec).Message)
		var data struct {
			CurrentETag string `json:"current_etag"`
		}
		require.NoError(t, json.Unmarshal(decodeResourceEnvelope(t, rec).Data, &data))
		require.Equal(t, etag, data.CurrentETag)
	})

	t.Run("stale value reaches the adapter and maps 412", func(t *testing.T) {
		t.Parallel()
		s, fake := newResourceStateHarness(t)
		fake.scopeFn = func(context.Context, core.Actor, string, string, ResourceScopeWrite, string) (ResourceScopeView, error) {
			return ResourceScopeView{}, resourceStatePreconditionError(etag)
		}
		rec := resourceStateCall(t, s, http.MethodPost, scopePath,
			`{"resource_uid":"ru-1","scope":"shared"}`, formatETag(strings.Repeat("b", 64)))
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)
		var data struct {
			CurrentETag string `json:"current_etag"`
		}
		require.NoError(t, json.Unmarshal(decodeResourceEnvelope(t, rec).Data, &data))
		require.Equal(t, etag, data.CurrentETag, "陈旧值 412 与形状错误给同一组字段")
		require.Equal(t, 1, fake.scopeCalls, "形状合法的 If-Match 必须进入适配器")
	})
}

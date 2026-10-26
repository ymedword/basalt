package web

// 本文件判卷执行档位端点的 HTTP 面（14D；契约 D-03 与 run-and-errors §7.3）：
// `GET /execution-policy` 的视图形状与字段集逐项对契约、`PUT` 的 412 / 409 / 400 / 500
// 四条出口与"预写之前没有操作标识"、成功响应是**重新读出**的生效视图而不是回显，
// 以及 `POST /execution-policy/confirmations` 的幂等与拒绝分支。
// 与之对应的服务层语义（库为权威源、CAS、预写）在 internal/app 的同名用例里判。
// This file judges the HTTP surface only; the service semantics live in internal/app.

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

// fakeExecutionPolicy 是档位管理端口的内存替身：`Current` / `ExceptionsCount` 读当前
// 快照，`Update` 与 `Confirm` 可编程；调用计数用于断言"形状级 412 与请求校验都没
// 触达服务层"（因此零写入、零操作行）。
// fakeExecutionPolicy is the in-memory admin port; the call counters pin the fact that
// shape-level rejections never reach the service.
type fakeExecutionPolicy struct {
	policy     core.ExecutionPolicy
	exceptions int
	pending    []core.PendingPolicyConfirmation
	pendingErr error

	updateFn func(context.Context, core.ExecutionProfile, core.ExecutionProfile, string,
		core.Actor) (core.ExecutionPolicy, string, error)
	updateCalls int
	gotProfile  core.ExecutionProfile
	gotExpected core.ExecutionProfile
	gotIfMatch  string
	gotActor    core.Actor

	confirmFn    func(context.Context, string, core.Actor) (string, bool, error)
	confirmCalls int
	gotConfirmID string
}

// 编译期断言：假件实现被测端口——端口漂移在编译期就红。
var _ core.ExecutionPolicyAdmin = (*fakeExecutionPolicy)(nil)

func (f *fakeExecutionPolicy) Current() core.ExecutionPolicy { return f.policy }

func (f *fakeExecutionPolicy) ExceptionsCount() int { return f.exceptions }

func (f *fakeExecutionPolicy) PendingConfirmations(context.Context) ([]core.PendingPolicyConfirmation, error) {
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}
	return f.pending, nil
}

func (f *fakeExecutionPolicy) Update(_ context.Context, profile, expected core.ExecutionProfile,
	ifMatch string, actor core.Actor,
) (core.ExecutionPolicy, string, error) {
	f.updateCalls++
	f.gotProfile, f.gotExpected, f.gotIfMatch, f.gotActor = profile, expected, ifMatch, actor
	if f.updateFn != nil {
		return f.updateFn(context.Background(), profile, expected, ifMatch, actor)
	}
	f.policy.Profile = profile
	f.policy.Revision++
	return f.policy, "op-switch", nil
}

func (f *fakeExecutionPolicy) Confirm(_ context.Context, id string,
	actor core.Actor,
) (string, bool, error) {
	f.confirmCalls++
	f.gotConfirmID = id
	if f.confirmFn != nil {
		return f.confirmFn(context.Background(), id, actor)
	}
	return "op-confirm", false, nil
}

// fixedActorAuth 是最小认证闸门替身：不校验凭据，直接以固定 actor 调用处理器。
// 用来断言处理函数把**认证得到的主体**原样交给服务层，而不是自造一个主体。
// fixedActorAuth hands a fixed actor to the handler so the forwarding can be asserted.
type fixedActorAuth struct{ actor core.Actor }

func (f fixedActorAuth) Wrap(next AdminHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next(w, r, f.actor)
	})
}

// seededExecutionPolicy 是夹具的固定档位行：修订号 3，来源按操作标识判为 database。
// seededExecutionPolicy is the fixed row used by the endpoint fixtures.
func seededExecutionPolicy() core.ExecutionPolicy {
	return core.ExecutionPolicy{
		Profile:     core.ProfileStandard,
		Revision:    3,
		UpdatedAt:   time.Date(2026, 10, 26, 9, 15, 0, 0, time.UTC),
		UpdatedBy:   "token:abc123",
		OperationID: "op-seed",
	}
}

// policyErrorEnvelope 是失败响应的原始信封：`data` 用 map 读，才能断言"字段缺席"
// 而不是"字段是零值"。
// policyErrorEnvelope reads data as a map so an absent field is distinguishable.
type policyErrorEnvelope struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

// decodePolicyError 解析失败信封：`data` 读成 map，缺席字段与零值因此可分。
// decodePolicyError decodes a failure envelope with a map payload.
func decodePolicyError(t *testing.T, rec *httptest.ResponseRecorder) policyErrorEnvelope {
	t.Helper()
	var env policyErrorEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env
}

// TestExecutionProfile_ReadView：视图字段集与契约 `ExecutionPolicyView` 逐字一致，
// 受保护地址段与 shell 名称集合取自 core 的同一份字面量，待确认项分轴投影且
// 非网络轴的 `resolved_protected` 是空数组而不是 null。
// 逆改点：漏投影任一字段 / 自造受保护地址清单 / 空数组写成 null → 红。
func TestExecutionProfile_ReadView(t *testing.T) {
	policy := seededExecutionPolicy()
	fake := &fakeExecutionPolicy{
		policy:     policy,
		exceptions: 2,
		pending: []core.PendingPolicyConfirmation{
			{
				ID: "network_allowlist:intra.example.com", Axis: core.PendingAxisNetworkAllowlist,
				Value: "intra.example.com", Reason: core.PendingReasonProtectedAddress,
				ResolvedProtected: []string{"10.0.0.8"},
			},
			{
				ID: "shell_command:bash", Axis: core.PendingAxisShellCommand,
				Value: "bash", Reason: core.PendingReasonShellName,
			},
		},
	}
	s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/execution-policy", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var env resourcesEnvelope[map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	requireResourcePayloadMatches(t, readContractDoc(t, "openapi.yaml"), "ExecutionPolicyView", env.Data)

	view := decodeResourcesEnvelope[ExecutionPolicyView](t, rec).Data
	require.Equal(t, string(core.ProfileStandard), view.Profile)
	require.Equal(t, int64(3), view.Revision)
	require.Equal(t, core.ExecutionPolicyETag(policy), view.ETag)
	require.Equal(t, core.NetworkAllowlistSourceDatabase, view.Source)
	require.Equal(t, "2026-10-26T09:15:00Z", view.UpdatedAt)
	require.Equal(t, "token:abc123", view.UpdatedBy)
	require.Equal(t, 2, view.ExceptionsCount)
	require.Equal(t, core.ProtectedAddresses(), view.ProtectedAddresses)
	require.Equal(t, core.ShellNames(), view.ShellNames)
	require.Len(t, view.PendingConfirmations, 2)
	require.Equal(t, "network_allowlist:intra.example.com", view.PendingConfirmations[0].ID)
	require.Equal(t, []string{"10.0.0.8"}, view.PendingConfirmations[0].ResolvedProtected)
	require.Equal(t, "shell_command:bash", view.PendingConfirmations[1].ID)
	require.Equal(t, "bash", view.PendingConfirmations[1].Value)
	require.NotNil(t, view.PendingConfirmations[1].ResolvedProtected, "空数组不得写成 null")
	require.Empty(t, view.PendingConfirmations[1].ResolvedProtected)
}

// TestExecutionProfile_ReadEmptyPendingList：没有待确认项时 `pending_confirmations`
// 是空数组——页面据"长度 0"渲染"无待确认项"，不能靠字段缺席或 null 判断。
// 逆改点：空集合投影成 null → 红。
func TestExecutionProfile_ReadEmptyPendingList(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.ExecutionPolicy = &fakeExecutionPolicy{policy: seededExecutionPolicy()}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/execution-policy", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"pending_confirmations":[]`)
	require.NotContains(t, rec.Body.String(), "null")
}

// TestExecutionProfile_ReadFailsClosed：端口未装配、档位行未种子化、扫描失败三种
// 情形一律 500，且响应里**不得**出现任何档位字面量——"读不到当前档位"不能用
// `strict` / `standard` 之类的默认值冒充。
// 逆改点：未装载时回 200 并填默认档位 → "standard" 断言红。
func TestExecutionProfile_ReadFailsClosed(t *testing.T) {
	cases := map[string]*fakeExecutionPolicy{
		"port missing": nil,
		"not seeded":   {policy: core.ExecutionPolicy{}},
		"scan failed": {
			policy:     seededExecutionPolicy(),
			pendingErr: errors.New("scan exploded"),
		},
	}
	for name, fake := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
			rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/execution-policy", ""))
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			env := decodePolicyError(t, rec)
			require.Equal(t, codeInternal, env.Code)
			require.Equal(t, messageInternal, env.Message)
			require.NotContains(t, rec.Body.String(), string(core.ProfileStandard))
			require.NotContains(t, rec.Body.String(), string(core.ProfileStrict))
			require.NotContains(t, rec.Body.String(), "scan exploded", "底层原因只进日志")
		})
	}
}

// TestExecutionProfile_WriteRequiresIfMatch：`If-Match` 缺失、`*`、弱 ETag 一律
// 形状级 412，处理函数零调用（因此零写入、零操作行）；档位行可读时 412 带
// `data.current_etag`，未种子化时不带（不给出一个永远对不上的条件值）。
// 逆改点：把新路由标成非条件写 → calls 断言红。
func TestExecutionProfile_WriteRequiresIfMatch(t *testing.T) {
	policy := seededExecutionPolicy()
	body := `{"profile":"strict","expected_profile":"standard"}`
	for _, raw := range []string{"", "*", "W/" + core.ExecutionPolicyETag(policy)} {
		t.Run(fmt.Sprintf("If-Match=%q", raw), func(t *testing.T) {
			fake := &fakeExecutionPolicy{policy: policy}
			s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
			req := adminRequest(http.MethodPut, "/api/v1/execution-policy", body)
			if raw != "" {
				req.Header.Set("If-Match", raw)
			}
			rec := serveAdmin(s, req)
			require.Equal(t, http.StatusPreconditionFailed, rec.Code)
			env := decodePolicyError(t, rec)
			require.Equal(t, codePreconditionFailed, env.Code)
			require.Equal(t, ifMatchRequiredMessage, env.Message)
			require.Equal(t, core.ExecutionPolicyETag(policy), env.Data["current_etag"])
			require.NotContains(t, env.Data, "operation_id", "预写之前没有操作行")
			require.Zero(t, fake.updateCalls, "形状级 412 不得进入处理函数")
		})
	}

	t.Run("unseeded row has no etag to offer", func(t *testing.T) {
		fake := &fakeExecutionPolicy{}
		s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
		rec := serveAdmin(s, adminRequest(http.MethodPut, "/api/v1/execution-policy", body))
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)
		require.NotContains(t, rec.Body.String(), "current_etag")
		require.Zero(t, fake.updateCalls)
	})
}

// TestExecutionProfile_WriteBodyValidation：请求体形状与必填字段在本层判完，400
// 且**零调用**；缺 `profile` 与缺 `expected_profile` 分开点名。形状合法才会走到
// 服务层，因此这里一律带合法 If-Match。
// 逆改点：把缺字段当成空串转交服务层 → 400 文案与零调用断言红。
func TestExecutionProfile_WriteBodyValidation(t *testing.T) {
	policy := seededExecutionPolicy()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing profile", `{"expected_profile":"standard"}`, executionPolicyProfileMissing},
		{"missing expected", `{"profile":"strict"}`, executionPolicyExpectedMissing},
		{"malformed", `{"profile":`, executionPolicyBodyMessage},
		{"unknown field", `{"profile":"strict","expected_profile":"standard","extra":1}`, executionPolicyBodyMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeExecutionPolicy{policy: policy}
			s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
			req := adminRequest(http.MethodPut, "/api/v1/execution-policy", tc.body)
			req.Header.Set("If-Match", core.ExecutionPolicyETag(policy))
			rec := serveAdmin(s, req)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			env := decodePolicyError(t, rec)
			require.Equal(t, codeBadRequest, env.Code)
			require.Equal(t, tc.want, env.Message)
			require.NotContains(t, env.Data, "operation_id")
			require.Zero(t, fake.updateCalls)
		})
	}
}

// TestExecutionProfile_WritePreconditionFailures：服务层的三种前置失败各自固定
// 状态码与数据——方向不一致 409 `EXECUTION_PROFILE_MISMATCH`（带当前档位与 ETag）、
// 陈旧 412（带当前 ETag）、修订号冲突 409 `CONFLICT`（只带操作标识）。
// 前两者发生在预写之前，**不得**出现操作标识；对外文案不得回显底层英文。
// 逆改点：三个分支混用状态码 / 412 透出英文 / 预写前带 operation_id → 红。
func TestExecutionProfile_WritePreconditionFailures(t *testing.T) {
	policy := seededExecutionPolicy()
	etag := core.ExecutionPolicyETag(policy)
	body := `{"profile":"strict","expected_profile":"standard"}`

	t.Run("expected profile mismatch", func(t *testing.T) {
		fake := &fakeExecutionPolicy{policy: policy, updateFn: func(context.Context,
			core.ExecutionProfile, core.ExecutionProfile, string, core.Actor,
		) (core.ExecutionPolicy, string, error) {
			return core.ExecutionPolicy{}, "", core.ErrExecutionProfileMismatch
		}}
		s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
		req := adminRequest(http.MethodPut, "/api/v1/execution-policy", body)
		req.Header.Set("If-Match", etag)
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusConflict, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeExecutionProfileMismatch, env.Code)
		require.Equal(t, executionPolicyMismatchMessage, env.Message)
		require.Equal(t, string(core.ProfileStandard), env.Data["current_profile"])
		require.Equal(t, etag, env.Data["current_etag"])
		require.NotContains(t, env.Data, "operation_id", "方向确认发生在预写之前")
		require.Equal(t, 1, fake.updateCalls)
	})

	t.Run("stale if-match", func(t *testing.T) {
		fake := &fakeExecutionPolicy{policy: policy, updateFn: func(context.Context,
			core.ExecutionProfile, core.ExecutionProfile, string, core.Actor,
		) (core.ExecutionPolicy, string, error) {
			return core.ExecutionPolicy{}, "", fmt.Errorf("%w: precondition", core.ErrPreconditionFailed)
		}}
		s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
		req := adminRequest(http.MethodPut, "/api/v1/execution-policy", body)
		req.Header.Set("If-Match", etag)
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codePreconditionFailed, env.Code)
		require.Equal(t, executionPolicyStaleMessage, env.Message)
		require.NotContains(t, env.Message, "precondition", "对外文案不得回显底层错误")
		require.Equal(t, etag, env.Data["current_etag"])
		require.NotContains(t, env.Data, "operation_id")
	})

	t.Run("revision conflict", func(t *testing.T) {
		fake := &fakeExecutionPolicy{policy: policy, updateFn: func(context.Context,
			core.ExecutionProfile, core.ExecutionProfile, string, core.Actor,
		) (core.ExecutionPolicy, string, error) {
			return core.ExecutionPolicy{}, "op-cas", fmt.Errorf("%w: lost", core.ErrPolicyRevisionConflict)
		}}
		s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
		req := adminRequest(http.MethodPut, "/api/v1/execution-policy", body)
		req.Header.Set("If-Match", etag)
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusConflict, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeConflict, env.Code)
		require.Equal(t, executionPolicyStaleMessage, env.Message)
		require.Equal(t, "op-cas", env.Data["operation_id"], "预写之后的失败必须带回操作标识")
	})
}

// TestExecutionProfile_WriteSuccess：成功后响应是**重新读出**的生效视图——修订号与
// ETag 来自新快照；请求体原样转交服务层（web 层不自行归一化或比较）。
// 逆改点：回显请求体（乐观更新）→ 修订号 / ETag 断言红。
func TestExecutionProfile_WriteSuccess(t *testing.T) {
	policy := seededExecutionPolicy()
	fake := &fakeExecutionPolicy{policy: policy}
	fake.updateFn = func(_ context.Context, profile, _ core.ExecutionProfile, _ string,
		actor core.Actor,
	) (core.ExecutionPolicy, string, error) {
		updated := policy
		updated.Profile = profile
		updated.Revision = policy.Revision + 1
		updated.OperationID = "op-switch"
		updated.UpdatedBy = actor.Subject
		fake.policy = updated // 服务层落库后 Current() 立即返回新快照
		return updated, "op-switch", nil
	}
	writeActor := core.Actor{
		Subject: "token:0123456789abcdef", Method: core.AuthMethodBearer,
		Source: "127.0.0.1:5000",
	}
	s := newTestServer(t, func(o *Options) {
		o.ExecutionPolicy = fake
		o.AdminAuth = fixedActorAuth{actor: writeActor}
	})

	req := adminRequest(http.MethodPut, "/api/v1/execution-policy",
		`{"profile":"strict","expected_profile":"standard"}`)
	req.Header.Set("If-Match", core.ExecutionPolicyETag(policy))
	rec := serveAdmin(s, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, 1, fake.updateCalls)
	require.Equal(t, core.ProfileStrict, fake.gotProfile, "请求体原样转交")
	require.Equal(t, core.ProfileStandard, fake.gotExpected)
	require.Equal(t, core.ExecutionPolicyETag(policy), fake.gotIfMatch, "If-Match 必须逐字转交")
	require.Equal(t, writeActor, fake.gotActor, "认证主体必须原样传给服务层")

	view := decodeResourcesEnvelope[ExecutionPolicyView](t, rec).Data
	require.Equal(t, string(core.ProfileStrict), view.Profile)
	require.Equal(t, int64(4), view.Revision)
	require.Equal(t, core.ExecutionPolicyETag(fake.policy), view.ETag)
	require.Equal(t, writeActor.Subject, view.UpdatedBy, "新快照的写入者随视图回读")
}

// TestExecutionProfile_WriteErrorMapping：校验失败 400 只回 Reason（工具名不外露）；
// 其余失败 500，预写之后带 `data.operation_id`，预写之前不带。
// 逆改点：400 回显 `execution_policy` 前缀 / 500 吞掉操作标识 → 红。
func TestExecutionProfile_WriteErrorMapping(t *testing.T) {
	policy := seededExecutionPolicy()
	etag := core.ExecutionPolicyETag(policy)
	body := `{"profile":"lenient","expected_profile":"standard"}`

	cases := []struct {
		name        string
		err         error
		operationID string
		wantOpID    string
	}{
		{
			name: "invalid input",
			err: &core.InvalidInputError{
				Tool: "execution_policy", Reason: "档位只能是 standard 或 strict",
			},
		},
		{name: "failure without operation id", err: errors.New("boom")},
		{name: "failure after pre-write", err: errors.New("boom"), operationID: "op-fail", wantOpID: "op-fail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeExecutionPolicy{policy: policy, updateFn: func(context.Context,
				core.ExecutionProfile, core.ExecutionProfile, string, core.Actor,
			) (core.ExecutionPolicy, string, error) {
				return core.ExecutionPolicy{}, tc.operationID, tc.err
			}}
			s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
			req := adminRequest(http.MethodPut, "/api/v1/execution-policy", body)
			req.Header.Set("If-Match", etag)
			rec := serveAdmin(s, req)

			var invalid *core.InvalidInputError
			if errors.As(tc.err, &invalid) {
				require.Equal(t, http.StatusBadRequest, rec.Code)
				env := decodePolicyError(t, rec)
				require.Equal(t, codeBadRequest, env.Code)
				require.Equal(t, invalid.Reason, env.Message)
				require.NotContains(t, env.Message, "execution_policy", "工具名不外露")
				return
			}
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			env := decodePolicyError(t, rec)
			require.Equal(t, codeInternal, env.Code)
			require.Equal(t, messageInternal, env.Message)
			if tc.wantOpID == "" {
				require.NotContains(t, env.Data, "operation_id")
				return
			}
			require.Equal(t, tc.wantOpID, env.Data["operation_id"])
		})
	}
}

// TestExecutionProfile_Confirmations：记录「保持拒绝」——成功与重复提交都回 200
// （后者带 `already_recorded=true` 与既有操作标识，不新增审计行）；缺 id、未知
// decision、不在待确认集合内一律 400 且零调用；端口未装配 500。
// 逆改点：重复处置当错误 / `decision` 接受其它取值 / 未知 id 回 200 → 红。
func TestExecutionProfile_Confirmations(t *testing.T) {
	policy := seededExecutionPolicy()
	const path = "/api/v1/execution-policy/confirmations"

	t.Run("records keep denied", func(t *testing.T) {
		fake := &fakeExecutionPolicy{policy: policy}
		s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
		rec := serveAdmin(s, adminRequest(http.MethodPost, path,
			`{"id":"network_allowlist:intra.example.com","decision":"keep_denied"}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, 1, fake.confirmCalls)
		require.Equal(t, "network_allowlist:intra.example.com", fake.gotConfirmID)

		var env resourcesEnvelope[map[string]any]
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		requireResourcePayloadMatches(t, readContractDoc(t, "openapi.yaml"),
			"PolicyConfirmationResult", env.Data)

		result := decodeResourcesEnvelope[PolicyConfirmationResultView](t, rec).Data
		require.Equal(t, "network_allowlist:intra.example.com", result.ID)
		require.Equal(t, policyDecisionKeepDenied, result.Decision)
		require.Equal(t, "op-confirm", result.OperationID)
		require.False(t, result.AlreadyRecorded)
	})

	t.Run("already recorded", func(t *testing.T) {
		fake := &fakeExecutionPolicy{policy: policy, confirmFn: func(context.Context, string,
			core.Actor,
		) (string, bool, error) {
			return "op-existing", true, nil
		}}
		s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
		rec := serveAdmin(s, adminRequest(http.MethodPost, path,
			`{"id":"shell_command:bash","decision":"keep_denied"}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		result := decodeResourcesEnvelope[PolicyConfirmationResultView](t, rec).Data
		require.True(t, result.AlreadyRecorded)
		require.Equal(t, "op-existing", result.OperationID, "回既有行的操作标识")
	})

	t.Run("rejects malformed requests", func(t *testing.T) {
		cases := []struct{ name, body, want string }{
			{"missing id", `{"decision":"keep_denied"}`, policyConfirmationIDMissing},
			{
				"unknown decision", `{"id":"shell_command:bash","decision":"allow"}`,
				policyConfirmationDecisionMessage,
			},
			{"malformed", `{"id":`, policyConfirmationBodyMessage},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				fake := &fakeExecutionPolicy{policy: policy}
				s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
				rec := serveAdmin(s, adminRequest(http.MethodPost, path, tc.body))
				require.Equal(t, http.StatusBadRequest, rec.Code)
				env := decodePolicyError(t, rec)
				require.Equal(t, codeBadRequest, env.Code)
				require.Equal(t, tc.want, env.Message)
				require.Zero(t, fake.confirmCalls, "请求校验不得触达服务层")
			})
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		fake := &fakeExecutionPolicy{policy: policy, confirmFn: func(_ context.Context, id string,
			_ core.Actor,
		) (string, bool, error) {
			return "", false, &core.InvalidInputError{
				Tool: "execution_policy", Reason: "待确认项不在集合内：" + id,
			}
		}}
		s := newTestServer(t, func(o *Options) { o.ExecutionPolicy = fake })
		rec := serveAdmin(s, adminRequest(http.MethodPost, path,
			`{"id":"network_allowlist:gone.example.com","decision":"keep_denied"}`))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeBadRequest, env.Code)
		require.Contains(t, env.Message, "gone.example.com", "必须点名是哪一条")
		require.NotContains(t, env.Message, "execution_policy", "工具名不外露")
	})

	t.Run("port missing", func(t *testing.T) {
		s := newTestServer(t, nil)
		rec := serveAdmin(s, adminRequest(http.MethodPost, path,
			`{"id":"shell_command:bash","decision":"keep_denied"}`))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
	})
}

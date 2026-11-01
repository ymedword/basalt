package web

// 本文件是调用方管理面七个端点的 handler 验收面（US-16 / T026，颗粒度文档
// §3.1.7 / openapi `/callers` 组）。断言分四层：① 状态码与契约错误码；
// ② 请求形状与领域入参的逐字段转发（web 层**不**补默认值、**不**替领域校验）；
// ③ 响应投影——时间 RFC3339、空列表 `[]`、Key 明文只出一次；
// ④ 失败分类只按哨兵映射，底层错误文本永不进入响应。
// This file pins the seven caller-admin handlers: status/code mapping, verbatim
// forwarding, the response projection, and sentinel-only error classification.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// errFakeCallerAdminUnused 标记用例没有为该端口方法准备行为。
// errFakeCallerAdminUnused marks a port method the case never intended to call.
var errFakeCallerAdminUnused = errors.New("fake caller admin: unexpected call")

// fakeCallerAdminService 是可注入行为的 CallerAdminService 替身：未设置的方法
// 按"意外调用"报错，避免用例悄悄依赖未定义行为。
// fakeCallerAdminService is a behavior-injected port; an unset method reports an
// unexpected call.
type fakeCallerAdminService struct {
	listFn      func(ctx context.Context) ([]callers.CallerRecord, error)
	createFn    func(ctx context.Context, actor core.Actor, req callers.CreateCallerRequest) (callers.CallerRecord, error)
	issueFn     func(ctx context.Context, actor core.Actor, req callers.IssueRequest) (callers.IssuedKey, error)
	revokeFn    func(ctx context.Context, actor core.Actor, req callers.RevokeRequest) (callers.KeyRecord, error)
	usageFn     func(ctx context.Context, callerID string, periodStart int64) (callers.CallerUsage, error)
	reconcileFn func(ctx context.Context, actor core.Actor, req callers.ReconcileRequest) (callers.Settlement, error)
	clearFn     func(ctx context.Context, actor core.Actor, req callers.ClearOverageRequest) (callers.CallerUsage, error)
}

// ListCallers 委托列表查询的注入行为。
func (f *fakeCallerAdminService) ListCallers(ctx context.Context) ([]callers.CallerRecord, error) {
	if f.listFn == nil {
		return nil, errFakeCallerAdminUnused
	}
	return f.listFn(ctx)
}

// CreateCaller 委托创建的注入行为。
func (f *fakeCallerAdminService) CreateCaller(ctx context.Context, actor core.Actor, req callers.CreateCallerRequest) (callers.CallerRecord, error) {
	if f.createFn == nil {
		return callers.CallerRecord{}, errFakeCallerAdminUnused
	}
	return f.createFn(ctx, actor, req)
}

// IssueKey 委托签发的注入行为。
func (f *fakeCallerAdminService) IssueKey(ctx context.Context, actor core.Actor, req callers.IssueRequest) (callers.IssuedKey, error) {
	if f.issueFn == nil {
		return callers.IssuedKey{}, errFakeCallerAdminUnused
	}
	return f.issueFn(ctx, actor, req)
}

// RevokeKey 委托吊销的注入行为。
func (f *fakeCallerAdminService) RevokeKey(ctx context.Context, actor core.Actor, req callers.RevokeRequest) (callers.KeyRecord, error) {
	if f.revokeFn == nil {
		return callers.KeyRecord{}, errFakeCallerAdminUnused
	}
	return f.revokeFn(ctx, actor, req)
}

// Usage 委托用量查询的注入行为。
func (f *fakeCallerAdminService) Usage(ctx context.Context, callerID string, periodStart int64) (callers.CallerUsage, error) {
	if f.usageFn == nil {
		return callers.CallerUsage{}, errFakeCallerAdminUnused
	}
	return f.usageFn(ctx, callerID, periodStart)
}

// ReconcileReservation 委托核对的注入行为。
func (f *fakeCallerAdminService) ReconcileReservation(ctx context.Context, actor core.Actor, req callers.ReconcileRequest) (callers.Settlement, error) {
	if f.reconcileFn == nil {
		return callers.Settlement{}, errFakeCallerAdminUnused
	}
	return f.reconcileFn(ctx, actor, req)
}

// ClearOverage 委托清零的注入行为。
func (f *fakeCallerAdminService) ClearOverage(ctx context.Context, actor core.Actor, req callers.ClearOverageRequest) (callers.CallerUsage, error) {
	if f.clearFn == nil {
		return callers.CallerUsage{}, errFakeCallerAdminUnused
	}
	return f.clearFn(ctx, actor, req)
}

// newCallersAdminServer 装配一个认证可用、调用方端口可注入的管理面服务器。
// newCallersAdminServer assembles the fixture with an injectable caller port.
func newCallersAdminServer(t *testing.T, fake CallerAdminService) *Server {
	t.Helper()
	return newTestServer(t, func(o *Options) {
		o.AdminAuth = resourceAuth{}
		o.Callers = fake
	})
}

// callerCall 构造一个已认证的调用方管理请求。
// callerCall builds an authenticated caller-admin request.
func callerCall(method, path, body string) *http.Request {
	return withBearer(adminRequest(method, path, body), testToken)
}

// callerEnvelope 是响应信封的解码形状：`data` 原样保留，由用例按端点解码。
// callerEnvelope keeps data raw so each case decodes its own view.
type callerEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// decodeCallerEnvelope 解析响应信封（HTTP 状态码由各用例先断言）。
// decodeCallerEnvelope parses the envelope; each case checks the status first.
func decodeCallerEnvelope(t *testing.T, body *bytes.Buffer) callerEnvelope {
	t.Helper()
	var env callerEnvelope
	require.NoError(t, json.Unmarshal(body.Bytes(), &env))
	return env
}

// TestCallersAdmin_ListProjectsSummaryAndEmptyArray 判列表投影：空集合输出 `[]`
// 而不是 null，行字段与契约同名字段一致，`project` 与 `key_count` 恒输出。
// 逆改点：callerSummaryViews 返回 nil → `data` 变 null，断言红。
func TestCallersAdmin_ListProjectsSummaryAndEmptyArray(t *testing.T) {
	created := time.Date(2026, 10, 28, 8, 30, 0, 0, time.UTC)
	cases := []struct {
		name string
		list []callers.CallerRecord
		want string
	}{
		{
			name: "空列表是空数组",
			list: nil,
			want: `[]`,
		},
		{
			name: "行字段逐项投影",
			list: []callers.CallerRecord{{
				CallerID:  "local-admin",
				Kind:      callers.CallerKindBuiltin,
				Name:      "内置管理员",
				Project:   "",
				Status:    callers.CallerStatusActive,
				CreatedAt: created,
				KeyCount:  0,
			}},
			want: `[{"caller_id":"local-admin","kind":"builtin","name":"内置管理员","project":"",` +
				`"status":"active","created_at":"2026-10-28T08:30:00Z","key_count":0}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCallersAdminServer(t, &fakeCallerAdminService{
				listFn: func(context.Context) ([]callers.CallerRecord, error) { return tc.list, nil },
			})
			rec := serveAdmin(s, callerCall(http.MethodGet, "/api/v1/callers", ""))
			require.Equal(t, http.StatusOK, rec.Code)
			env := decodeCallerEnvelope(t, rec.Body)
			require.Equal(t, codeOK, env.Code)
			require.JSONEq(t, tc.want, string(env.Data))
		})
	}
}

// TestCallersAdmin_CreateForwardsLimitsVerbatim 判创建请求逐字段转发：给了
// `limits` 就原样交给领域（规范化在领域），没给 `limits` 就是 nil——web 层
// **不得**补一份全零限额（全零语义是"拒绝一切"，与"没配置"不同）。
// 逆改点：`limits` 缺省时构造非 nil → 领域会写一行全零限额，断言红。
func TestCallersAdmin_CreateForwardsLimitsVerbatim(t *testing.T) {
	var got callers.CreateCallerRequest
	record := callers.CallerRecord{
		CallerID:  "cal_01J0000000000000000000000",
		Kind:      callers.CallerKindExternal,
		Name:      "ops",
		Project:   "p1",
		Status:    callers.CallerStatusActive,
		CreatedAt: time.Date(2026, 10, 28, 1, 2, 3, 0, time.UTC),
	}
	s := newCallersAdminServer(t, &fakeCallerAdminService{
		createFn: func(_ context.Context, _ core.Actor, req callers.CreateCallerRequest) (callers.CallerRecord, error) {
			got = req
			return record, nil
		},
	})

	body := `{"caller_id":"cal_01J0000000000000000000000","name":"ops","project":"p1",` +
		`"limits":{"qps":5,"burst":10,"max_concurrent":2,"tasks_per_period":100,` +
		`"tokens_per_period":1000,"ingest_bytes_per_period":2048,"period":"day","timezone":"UTC"}}`
	rec := serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers", body))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "cal_01J0000000000000000000000", got.CallerID)
	require.Equal(t, "ops", got.Name)
	require.Equal(t, "p1", got.Project)
	require.NotNil(t, got.Limit)
	require.Equal(t, 5.0, got.Limit.QPS)
	require.Equal(t, 10, got.Limit.Burst)
	require.Equal(t, 2, got.Limit.MaxConcurrent)
	require.Equal(t, int64(100), got.Limit.TasksPerPeriod)
	require.Equal(t, int64(1000), got.Limit.TokensPerPeriod)
	require.Equal(t, int64(2048), got.Limit.IngestBytesPerPeriod)
	require.Equal(t, callers.Period("day"), got.Limit.Period)
	require.Equal(t, "UTC", got.Limit.Timezone)

	env := decodeCallerEnvelope(t, rec.Body)
	require.JSONEq(t, `{"caller_id":"cal_01J0000000000000000000000","kind":"external",`+
		`"name":"ops","project":"p1","status":"active","created_at":"2026-10-28T01:02:03Z","key_count":0}`,
		string(env.Data))

	rec = serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers", `{"name":"no-limit"}`))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Nil(t, got.Limit, "未给 limits 时必须转发 nil，不得补默认值")
}

// TestCallersAdmin_CreateRejectsUnknownFieldAndConflict 判两条创建失败面：
// 未知字段按 400 固定文案拒绝（端口零调用），存储冲突按 409 CONFLICT 映射。
// 逆改点：DisallowUnknownFields 被移除 → 未知字段被静默接受，断言红。
func TestCallersAdmin_CreateRejectsUnknownFieldAndConflict(t *testing.T) {
	called := false
	s := newCallersAdminServer(t, &fakeCallerAdminService{
		createFn: func(context.Context, core.Actor, callers.CreateCallerRequest) (callers.CallerRecord, error) {
			called = true
			return callers.CallerRecord{}, core.ErrConflict
		},
	})

	rec := serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers", `{"name":"ops","bogus":1}`))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.False(t, called, "请求体不合法的 400 必须在端口之前返回")
	env := decodeCallerEnvelope(t, rec.Body)
	require.Equal(t, codeBadRequest, env.Code)
	require.Equal(t, messageCallerCreateBody, env.Message)

	rec = serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers", `{"name":"dup"}`))
	require.Equal(t, http.StatusConflict, rec.Code)
	env = decodeCallerEnvelope(t, rec.Body)
	require.Equal(t, codeConflict, env.Code)
	require.Equal(t, messageCallerConflict, env.Message)
}

// TestCallersAdmin_IssueKeyReturnsPlaintextOnce 判签发响应：Key 明文只经 `key`
// 字段出站一次，其余字段来自 Key 行投影，可空时刻渲染空串。
// 逆改点：把 Secret 写进 CallerKeySummaryView → 摘要结构多出凭据字段，断言红。
func TestCallersAdmin_IssueKeyReturnsPlaintextOnce(t *testing.T) {
	expires := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	issued := callers.IssuedKey{
		KeyRecord: callers.KeyRecord{
			KeyID:     "key_01J0000000000000000000000",
			CallerID:  "cal_01J0000000000000000000000",
			KeyPrefix: "bk_0123456789",
			Scopes:    []string{core.ScopeSessionsRead},
			Status:    callers.KeyStatusActive,
			CreatedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
			ExpiresAt: &expires,
		},
		Secret: "bk_0123456789abcdef0123456789abcdef01234567",
	}
	var got callers.IssueRequest
	s := newCallersAdminServer(t, &fakeCallerAdminService{
		issueFn: func(_ context.Context, _ core.Actor, req callers.IssueRequest) (callers.IssuedKey, error) {
			got = req
			return issued, nil
		},
	})

	body := `{"scopes":["sessions:read"],"expires_at":"2026-12-01T00:00:00Z"}`
	rec := serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers/cal_01J0000000000000000000000/keys", body))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "cal_01J0000000000000000000000", got.CallerID)
	require.Equal(t, []string{"sessions:read"}, got.Scopes)
	require.NotNil(t, got.ExpiresAt)
	require.True(t, expires.Equal(*got.ExpiresAt))

	env := decodeCallerEnvelope(t, rec.Body)
	require.JSONEq(t, `{"key_id":"key_01J0000000000000000000000","caller_id":"cal_01J0000000000000000000000",`+
		`"key_prefix":"bk_0123456789","scopes":["sessions:read"],"status":"active",`+
		`"created_at":"2026-10-28T00:00:00Z","expires_at":"2026-12-01T00:00:00Z","revoked_at":"",`+
		`"key":"bk_0123456789abcdef0123456789abcdef01234567"}`, string(env.Data))
}

// TestCallersAdmin_IssueKeyRejectsBadExpiryBeforePort 判两条过期时刻拒绝：格式非法
// 与已过去都按 400 拒绝且**不**调用端口——不签一把出生即失效的 Key。
// 逆改点：去掉 `parsed.After(now)` 判定 → 过去时刻被接受，断言红。
func TestCallersAdmin_IssueKeyRejectsBadExpiryBeforePort(t *testing.T) {
	called := false
	s := newCallersAdminServer(t, &fakeCallerAdminService{
		issueFn: func(context.Context, core.Actor, callers.IssueRequest) (callers.IssuedKey, error) {
			called = true
			return callers.IssuedKey{}, nil
		},
	})

	rec := serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers/c1/keys", `{"scopes":["sessions:read"],"expires_at":"not-a-time"}`))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, messageCallerExpiresAtShape, decodeCallerEnvelope(t, rec.Body).Message)

	// 服务器固定时钟是 2026-10-18T12:00:00Z（defaultTestOptions），给一个更早的时刻。
	rec = serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers/c1/keys", `{"scopes":["sessions:read"],"expires_at":"2026-10-18T11:59:59Z"}`))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, messageCallerExpiresAtPast, decodeCallerEnvelope(t, rec.Body).Message)
	require.False(t, called, "过期时刻的 400 必须在端口之前返回")
}

// TestCallersAdmin_RevokeKeyAcceptsEmptyBody 判吊销请求体可整体省略：`reason`
// 缺省为空串，路径参数逐字转发。
// 逆改点：改用 decodeAdminWriteBody → 空 body 变成 400，断言红。
func TestCallersAdmin_RevokeKeyAcceptsEmptyBody(t *testing.T) {
	var got callers.RevokeRequest
	s := newCallersAdminServer(t, &fakeCallerAdminService{
		revokeFn: func(_ context.Context, _ core.Actor, req callers.RevokeRequest) (callers.KeyRecord, error) {
			got = req
			return callers.KeyRecord{
				KeyID:     req.KeyID,
				CallerID:  req.CallerID,
				Status:    callers.KeyStatusRevoked,
				CreatedAt: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
				RevokedAt: ptrTime(time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)),
			}, nil
		},
	})

	rec := serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers/cal_01J0000000000000000000000/keys/key_01J0000000000000000000000/revoke", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "cal_01J0000000000000000000000", got.CallerID)
	require.Equal(t, "key_01J0000000000000000000000", got.KeyID)
	require.Empty(t, got.Reason)
}

// ptrTime 返回时刻指针（测试夹具用）。
// ptrTime returns a pointer to the given instant.
func ptrTime(at time.Time) *time.Time { return &at }

// TestCallersAdmin_UsageParsesPeriodStartQuery 判 period_start 查询参数的两种
// 形态：缺省 0（当前周期），RFC3339 折算 Unix 毫秒；非法格式按 400 拒绝。
// 逆改点：把非法值静默当 0 → 请求被当成"当前周期"处理，断言红。
func TestCallersAdmin_UsageParsesPeriodStartQuery(t *testing.T) {
	var gotPeriod int64
	usage := callers.CallerUsage{
		CallerID:    "cal_01J0000000000000000000000",
		PeriodStart: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC),
		Timezone:    "UTC",
		Limit:       callers.Limit{QPS: 1, Burst: 2, Period: callers.PeriodDay, Timezone: "UTC"},
		Lines:       []callers.UsageLine{{Kind: "run", Used: 3, Reserved: 1, Overage: 2}},
	}
	s := newCallersAdminServer(t, &fakeCallerAdminService{
		usageFn: func(_ context.Context, _ string, periodStart int64) (callers.CallerUsage, error) {
			gotPeriod = periodStart
			return usage, nil
		},
	})

	rec := serveAdmin(s, callerCall(http.MethodGet, "/api/v1/callers/cal_01J0000000000000000000000/usage", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(0), gotPeriod, "缺省 period_start 必须传 0，由存储层折算当前周期")

	wantMillis := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	rec = serveAdmin(s, callerCall(http.MethodGet,
		"/api/v1/callers/cal_01J0000000000000000000000/usage?period_start=2026-10-01T00:00:00Z", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, wantMillis, gotPeriod)

	rec = serveAdmin(s, callerCall(http.MethodGet,
		"/api/v1/callers/cal_01J0000000000000000000000/usage?period_start=yesterday", ""))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, messageCallerPeriodStartShape, decodeCallerEnvelope(t, rec.Body).Message)
}

// TestCallersAdmin_ReconcileForwardsOutcomeAndActual 判核对请求的线上形状：
// `actual` 用指针区分"没给"与"给了 0"，两者都原样转发；响应回显 class 与
// 结算后的终态。
// 逆改点：actual 改成 int64 → released 分支无法区分"没给"与"给了 0"，断言红。
func TestCallersAdmin_ReconcileForwardsOutcomeAndActual(t *testing.T) {
	var got callers.ReconcileRequest
	settlement := callers.Settlement{
		ReservationID: "qres_tokens_01",
		CallerID:      "cal_01J0000000000000000000000",
		Class:         callers.ClassChat,
		PeriodStart:   time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC).UnixMilli(),
		Amount:        100,
		Actual:        0,
		Overage:       0,
		State:         callers.ReservationReconciled,
	}
	s := newCallersAdminServer(t, &fakeCallerAdminService{
		reconcileFn: func(_ context.Context, _ core.Actor, req callers.ReconcileRequest) (callers.Settlement, error) {
			got = req
			return settlement, nil
		},
	})

	path := "/api/v1/callers/cal_01J0000000000000000000000/reservations/qres_tokens_01/reconcile"
	rec := serveAdmin(s, callerCall(http.MethodPost, path, `{"outcome":"released","reason":"调用未发出"}`))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, callers.ReconcileReleased, got.Outcome)
	require.Nil(t, got.Actual, "没给 actual 必须保持 nil")
	require.Equal(t, "调用未发出", got.Reason)

	env := decodeCallerEnvelope(t, rec.Body)
	require.JSONEq(t, `{"reservation_id":"qres_tokens_01","caller_id":"cal_01J0000000000000000000000",`+
		`"class":"chat","period_start":"2026-10-28T00:00:00Z","amount":100,"actual":0,`+
		`"overage":0,"status":"reconciled"}`, string(env.Data))

	rec = serveAdmin(s, callerCall(http.MethodPost, path, `{"outcome":"committed","actual":0,"reason":"确认零消耗"}`))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, got.Actual, "给了 actual 0 必须保持非 nil")
	require.Equal(t, int64(0), *got.Actual)
}

// TestCallersAdmin_ErrorMappingIsSentinelOnly 判失败分类：只认哨兵 / 类型，
// 底层错误文本（库 SQL、英文包前缀）永不进入响应。
// 逆改点：把 err.Error() 直接写进 message → 500 分支的响应出现底层文本，断言红。
func TestCallersAdmin_ErrorMappingIsSentinelOnly(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
	}{
		{
			name:        "请求非法带自产原因",
			err:         &callers.InvalidRequestError{Reason: "调用方名称不得为空"},
			wantStatus:  http.StatusBadRequest,
			wantCode:    codeBadRequest,
			wantMessage: "调用方名称不得为空",
		},
		{
			name:        "作用域非法",
			err:         callers.ErrInvalidScope,
			wantStatus:  http.StatusBadRequest,
			wantCode:    codeInvalidScope,
			wantMessage: errorCodeMessage(codeInvalidScope, ""),
		},
		{
			name:        "越权",
			err:         callers.ErrForbidden,
			wantStatus:  http.StatusForbidden,
			wantCode:    codeForbidden,
			wantMessage: messageCallerForbidden,
		},
		{
			name:        "已结算",
			err:         callers.ErrReservationSettled,
			wantStatus:  http.StatusConflict,
			wantCode:    codeConflict,
			wantMessage: messageCallerSettled,
		},
		{
			name:        "未知错误只回固定文案",
			err:         errors.New("pq: relation \"callers\" does not exist"),
			wantStatus:  http.StatusInternalServerError,
			wantCode:    codeInternal,
			wantMessage: messageInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCallersAdminServer(t, &fakeCallerAdminService{
				reconcileFn: func(context.Context, core.Actor, callers.ReconcileRequest) (callers.Settlement, error) {
					return callers.Settlement{}, tc.err
				},
			})
			path := "/api/v1/callers/c1/reservations/qres_tokens_01/reconcile"
			rec := serveAdmin(s, callerCall(http.MethodPost, path, `{"outcome":"released","reason":"x"}`))
			require.Equal(t, tc.wantStatus, rec.Code)
			env := decodeCallerEnvelope(t, rec.Body)
			require.Equal(t, tc.wantCode, env.Code)
			require.Equal(t, tc.wantMessage, env.Message)
			require.NotContains(t, env.Message, "pq:", "底层错误文本不得进入响应")
		})
	}
}

// TestCallersAdmin_NotFoundNamesTheTarget 判 404 点名目标：调用方不存在点名
// `caller_id`，Key 不存在点名 `key_id`——两个端点各自传自己的目标。
// 逆改点：统一用调用方文案 → 吊销 404 报出错误的标识，断言红。
func TestCallersAdmin_NotFoundNamesTheTarget(t *testing.T) {
	storeWith := func(err error) *fakeCallerAdminService {
		return &fakeCallerAdminService{
			issueFn: func(context.Context, core.Actor, callers.IssueRequest) (callers.IssuedKey, error) {
				return callers.IssuedKey{}, err
			},
			revokeFn: func(context.Context, core.Actor, callers.RevokeRequest) (callers.KeyRecord, error) {
				return callers.KeyRecord{}, err
			},
			usageFn: func(context.Context, string, int64) (callers.CallerUsage, error) {
				return callers.CallerUsage{}, err
			},
		}
	}

	s := newCallersAdminServer(t, storeWith(callers.ErrNotFound))
	rec := serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers/cal_missing/keys", `{"scopes":["sessions:read"]}`))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "未找到调用方：cal_missing", decodeCallerEnvelope(t, rec.Body).Message)

	rec = serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers/cal_01J0000000000000000000000/keys/key_missing/revoke", ""))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "未找到 Key：key_missing", decodeCallerEnvelope(t, rec.Body).Message)

	rec = serveAdmin(s, callerCall(http.MethodGet, "/api/v1/callers/cal_missing/usage", ""))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "未找到调用方：cal_missing", decodeCallerEnvelope(t, rec.Body).Message)
}

// TestCallersAdmin_ClearOverageForwardsReasonAndPeriod 判清零请求：`reason`
// 必填由领域判定，web 层只转发；`period_start` 缺省为零时刻（当前周期）。
// 逆改点：把空 period_start 解析成 time.Now() → 领域收到"某时刻"，断言红。
func TestCallersAdmin_ClearOverageForwardsReasonAndPeriod(t *testing.T) {
	var got callers.ClearOverageRequest
	s := newCallersAdminServer(t, &fakeCallerAdminService{
		clearFn: func(_ context.Context, _ core.Actor, req callers.ClearOverageRequest) (callers.CallerUsage, error) {
			got = req
			return callers.CallerUsage{
				CallerID:    req.CallerID,
				PeriodStart: time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC),
				PeriodEnd:   time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC),
				Timezone:    "UTC",
				Limit:       callers.Limit{Period: callers.PeriodDay, Timezone: "UTC"},
			}, nil
		},
	})

	rec := serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers/cal_01J0000000000000000000000/usage/clear-overage", `{"reason":"人工核对后清零"}`))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "cal_01J0000000000000000000000", got.CallerID)
	require.Equal(t, "人工核对后清零", got.Reason)
	require.True(t, got.PeriodStart.IsZero(), "缺省 period_start 必须是零时刻，由领域按当前周期处理")

	env := decodeCallerEnvelope(t, rec.Body)
	require.Contains(t, string(env.Data), `"caller_id":"cal_01J0000000000000000000000"`)

	rec = serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers/cal_01J0000000000000000000000/usage/clear-overage", `{"period_start":"2026-10-01T00:00:00Z","reason":"x"}`))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), got.PeriodStart)

	rec = serveAdmin(s, callerCall(http.MethodPost, "/api/v1/callers/cal_01J0000000000000000000000/usage/clear-overage", `{"period_start":"nope","reason":"x"}`))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, messageCallerPeriodStartShape, decodeCallerEnvelope(t, rec.Body).Message)
}

// TestCallersAdmin_MissingPortFailsClosed 判装配缺失：七个端点全部回固定 500，
// **不得**用空列表冒充"没有调用方"。
// 逆改点：List 走 `return nil, nil` 的空端口分支 → 列表 200 空数组，断言红。
func TestCallersAdmin_MissingPortFailsClosed(t *testing.T) {
	s := newCallersAdminServer(t, nil)
	routes := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/callers", ""},
		{http.MethodPost, "/api/v1/callers", `{"name":"ops"}`},
		{http.MethodPost, "/api/v1/callers/c1/keys", `{"scopes":["sessions:read"]}`},
		{http.MethodPost, "/api/v1/callers/c1/keys/k1/revoke", ""},
		{http.MethodGet, "/api/v1/callers/c1/usage", ""},
		{http.MethodPost, "/api/v1/callers/c1/reservations/r1/reconcile", `{"outcome":"released","reason":"x"}`},
		{http.MethodPost, "/api/v1/callers/c1/usage/clear-overage", `{"reason":"x"}`},
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			rec := serveAdmin(s, callerCall(route.method, route.path, route.body))
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			env := decodeCallerEnvelope(t, rec.Body)
			require.Equal(t, codeInternal, env.Code)
			require.Equal(t, messageInternal, env.Message)
		})
	}
}

// ---- T031：真实签发 → 真实认证链路的 R11 具名用例（设计卡 §4.3.7） ----

// memoryKeyStore 是 web 验收面用的内存 KeyStore：签发路径写入记录、认证路径只按
// 指纹查表（与 `internal/callers.Authenticator` 同口径）。管理方法只服务本文件的
// `GET /callers` 控制组，不模拟 PG 的事务语义——那由集成用例覆盖。
// memoryKeyStore backs the real issuer and authenticator in the web tests; lookups are
// fingerprint-only and PG transaction semantics stay out of this fixture.
type memoryKeyStore struct {
	mu      sync.Mutex
	records map[string]callers.KeyRecord
	hashes  map[string]string
	callers map[string]callers.CallerRecord
}

// newMemoryKeyStore 构造空的 Key / 调用方内存表。
// newMemoryKeyStore builds the empty in-memory tables.
func newMemoryKeyStore() *memoryKeyStore {
	return &memoryKeyStore{
		records: map[string]callers.KeyRecord{},
		hashes:  map[string]string{},
		callers: map[string]callers.CallerRecord{},
	}
}

// keyIndex 是 `(caller_id, key_id)` 的复合索引键（与存储主键同构）。
// keyIndex mirrors the composite primary key.
func keyIndex(callerID, keyID string) string { return callerID + "\x00" + keyID }

// Lookup 只按指纹定位；未命中返回 ErrNotFound（认证路径一视同仁地拒绝）。
// Lookup is fingerprint-only.
func (s *memoryKeyStore) Lookup(_ context.Context, fingerprint string) (callers.KeyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, ok := s.hashes[fingerprint]
	if !ok {
		return callers.KeyRecord{}, callers.ErrNotFound
	}
	return s.records[idx], nil
}

// LookupByID 按 `(caller_id, key_id)` 定位；归属不匹配同样按未找到处理。
// LookupByID resolves the composite key.
func (s *memoryKeyStore) LookupByID(_ context.Context, callerID, keyID string) (callers.KeyRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[keyIndex(callerID, keyID)]
	if !ok {
		return callers.KeyRecord{}, callers.ErrNotFound
	}
	return record, nil
}

// Create 写入一条 Key 行并登记指纹索引（明文不落任何字段）。
// Create stores the key row and its fingerprint index.
func (s *memoryKeyStore) Create(_ context.Context, record callers.KeyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := keyIndex(record.CallerID, record.KeyID)
	s.records[idx] = record
	s.hashes[record.KeyHash] = idx
	return nil
}

// SetStatus 切换 Key 状态；行不存在返回 ErrNotFound。
// SetStatus flips the key status.
func (s *memoryKeyStore) SetStatus(_ context.Context, callerID, keyID string, status callers.KeyStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := keyIndex(callerID, keyID)
	record, ok := s.records[idx]
	if !ok {
		return callers.ErrNotFound
	}
	record.Status = status
	s.records[idx] = record
	return nil
}

// Rotate 依次吊销旧 Key、写入新 Key；本组用例不消费任务再绑定，结果集保持为空。
// Rotate revokes the old key and stores the new one; task rebinding is out of scope here.
func (s *memoryKeyStore) Rotate(ctx context.Context, callerID, oldKeyID string, next callers.KeyRecord, _ callers.RotateOptions) (callers.RotateResult, error) {
	if err := s.SetStatus(ctx, callerID, oldKeyID, callers.KeyStatusRevoked); err != nil {
		return callers.RotateResult{}, err
	}
	if err := s.Create(ctx, next); err != nil {
		return callers.RotateResult{}, err
	}
	return callers.RotateResult{}, nil
}

// List 返回调用方行（顺序不承诺；管理面用例只断言状态码）。
// List returns the caller rows.
func (s *memoryKeyStore) List(_ context.Context) ([]callers.CallerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]callers.CallerRecord, 0, len(s.callers))
	for _, record := range s.callers {
		out = append(out, record)
	}
	return out, nil
}

// LookupCaller 按标识取调用方行。
// LookupCaller returns one caller row.
func (s *memoryKeyStore) LookupCaller(_ context.Context, callerID string) (callers.CallerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.callers[callerID]
	if !ok {
		return callers.CallerRecord{}, callers.ErrNotFound
	}
	return record, nil
}

// CreateCaller 写入调用方行；重复标识按冲突报出。
// CreateCaller stores the caller row and reports duplicates as conflicts.
func (s *memoryKeyStore) CreateCaller(_ context.Context, record callers.CallerRecord, _ *callers.Limit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.callers[record.CallerID]; exists {
		return core.ErrConflict
	}
	s.callers[record.CallerID] = record
	return nil
}

// Usage 返回零用量聚合：本组用例不消费用量视图。
// Usage returns an empty aggregate; usage views are out of scope here.
func (s *memoryKeyStore) Usage(_ context.Context, callerID string, _ int64, _ time.Time) (callers.UsageAggregate, error) {
	return callers.UsageAggregate{CallerID: callerID}, nil
}

// memoryOperationStore 是管理操作审计的内存替身：只记录预写与终态，供用例断言
// "被拒的签发不留 pending"；不模拟启动恢复的查证语义。
// memoryOperationStore records pending writes and terminal states so a rejected
// issuance can be proven to leave nothing behind.
type memoryOperationStore struct {
	mu      sync.Mutex
	records map[string]core.OperationRecord
}

// newMemoryOperationStore 构造空的操作表。
// newMemoryOperationStore builds the empty operation table.
func newMemoryOperationStore() *memoryOperationStore {
	return &memoryOperationStore{records: map[string]core.OperationRecord{}}
}

// BeginOperation 只接受 pending 预写；重复标识按冲突报出。
// BeginOperation stores a pending pre-write.
func (s *memoryOperationStore) BeginOperation(_ context.Context, rec core.OperationRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[rec.OperationID]; exists {
		return core.ErrConflict
	}
	rec.Status = core.OperationPending
	s.records[rec.OperationID] = rec
	return nil
}

// FinishOperation 回填终态；已终态的行保持不变（幂等）。
// FinishOperation writes the terminal state and leaves terminal rows untouched.
func (s *memoryOperationStore) FinishOperation(_ context.Context, operationID string, status core.OperationStatus, resultCode, errorMessage string, detailJSON []byte, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[operationID]
	if !ok {
		return core.ErrOperationNotFound
	}
	if rec.Status != core.OperationPending {
		return nil
	}
	rec.Status = status
	rec.ResultCode = resultCode
	rec.ErrorMessage = errorMessage
	rec.DetailJSON = detailJSON
	rec.FinishedAt = now
	s.records[operationID] = rec
	return nil
}

// GetOperation 按标识取一行。
// GetOperation fetches one row.
func (s *memoryOperationStore) GetOperation(_ context.Context, operationID string) (core.OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[operationID]
	if !ok {
		return core.OperationRecord{}, core.ErrOperationNotFound
	}
	return rec, nil
}

// ListOperations 返回全部行（本组用例只做计数）。
// ListOperations returns every row.
func (s *memoryOperationStore) ListOperations(_ context.Context, _ core.OperationFilter) ([]core.OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.OperationRecord, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, rec)
	}
	return out, nil
}

// PendingOperations 只返回仍为 pending 的行。
// PendingOperations returns the pending rows.
func (s *memoryOperationStore) PendingOperations(_ context.Context) ([]core.OperationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.OperationRecord, 0, len(s.records))
	for _, rec := range s.records {
		if rec.Status == core.OperationPending {
			out = append(out, rec)
		}
	}
	return out, nil
}

// RecoverOperation 写恢复结论；已终态的行幂等返回。
// RecoverOperation writes the recovery verdict idempotently.
func (s *memoryOperationStore) RecoverOperation(_ context.Context, operationID string, status core.OperationStatus, verification string, detailJSON []byte, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[operationID]
	if !ok {
		return core.ErrOperationNotFound
	}
	if rec.Status != core.OperationPending {
		return nil
	}
	rec.Status = status
	rec.Verification = verification
	rec.DetailJSON = detailJSON
	rec.RecoveredAt = now
	s.records[operationID] = rec
	return nil
}

// callerKeyHarness 把真实签发器、真实认证器与真实 web 闸门装在一起：Key 从
// `KeyIssuer.Issue` 出来，经指纹查表进 `Authenticator`，再由闸门映射成 Principal。
// callerKeyHarness wires the real issuer, authenticator and guard into one chain.
type callerKeyHarness struct {
	t        *testing.T
	keys     *memoryKeyStore
	ops      *memoryOperationStore
	issuer   *callers.KeyIssuer
	server   *Server
	sessions *fakeSessionStore
	runs     *fakeRunService
}

// newCallerKeyHarness 装配整条链路；管理面列表端口用固定行回答（本组用例只判
// 认证边界，不判投影）。
// newCallerKeyHarness assembles the chain; the admin list port answers a fixed row.
func newCallerKeyHarness(t *testing.T) *callerKeyHarness {
	t.Helper()
	keys := newMemoryKeyStore()
	ops := newMemoryOperationStore()
	guard, err := NewAuthGuardWithPrincipals(AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          testToken,
		AllowedHosts:   []string{"127.0.0.1:8080", "localhost:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, discardLogger(), func() time.Time { return authFixedNow }, callers.NewAuthenticator(keys, nil))
	require.NoError(t, err)

	h := &callerKeyHarness{t: t, keys: keys, ops: ops, issuer: callers.NewKeyIssuer(keys, ops)}
	h.server = newTestServer(t, func(o *Options) {
		h.runs = o.Runs.(*fakeRunService)
		h.sessions = o.Sessions.(*fakeSessionStore)
		o.AdminAuth = guard
		o.Callers = &fakeCallerAdminService{listFn: func(context.Context) ([]callers.CallerRecord, error) {
			return []callers.CallerRecord{{
				CallerID:  core.LocalAdminCallerID,
				Kind:      callers.CallerKindBuiltin,
				Name:      "内置管理员",
				Status:    callers.CallerStatusActive,
				CreatedAt: authFixedNow,
			}}, nil
		}}
	})
	return h
}

// testManagementActor 是签发路径的管理执行者标注（只进审计内存表，不外发）。
// testManagementActor is the management actor recorded on the operation row.
func testManagementActor() core.Actor {
	return core.Actor{Subject: "admin", Method: "auth-token", Source: "127.0.0.1:8080"}
}

// issueKey 用真实签发器发一把 Key，失败即测试装配错误。
// issueKey issues through the real KeyIssuer; a failure is a fixture bug.
func (h *callerKeyHarness) issueKey(callerID string, scopes ...string) callers.IssuedKey {
	h.t.Helper()
	issued, err := h.issueKeyErr(callerID, scopes)
	require.NoError(h.t, err)
	return issued
}

// issueKeyErr 暴露签发错误供拒绝路径断言。
// issueKeyErr surfaces the issuance error for rejection cases.
func (h *callerKeyHarness) issueKeyErr(callerID string, scopes []string) (callers.IssuedKey, error) {
	return h.issuer.Issue(context.Background(), testManagementActor(),
		callers.IssueRequest{CallerID: callerID, Scopes: scopes})
}

// countKeys 数某调用方未吊销的 Key 行。
// countKeys counts the caller's live key rows.
func (h *callerKeyHarness) countKeys(callerID string) int {
	h.keys.mu.Lock()
	defer h.keys.mu.Unlock()
	n := 0
	for _, record := range h.keys.records {
		if record.CallerID == callerID && record.Status != callers.KeyStatusRevoked {
			n++
		}
	}
	return n
}

// pendingOperations 数仍为 pending 的管理操作行。
// pendingOperations counts the rows still pending.
func (h *callerKeyHarness) pendingOperations() int {
	h.ops.mu.Lock()
	defer h.ops.mu.Unlock()
	n := 0
	for _, record := range h.ops.records {
		if record.Status == core.OperationPending {
			n++
		}
	}
	return n
}

// exchange 走真实 handler 链并返回状态码与信封；空串凭据 = 不带 Authorization。
// exchange runs one request and returns the status and envelope.
func (h *callerKeyHarness) exchange(method, path, body, secret string) (int, callerEnvelope) {
	req := adminRequest(method, path, body)
	if secret != "" {
		req = withBearer(req, secret)
	}
	rec := serveAdmin(h.server, req)
	return rec.Code, decodeCallerEnvelope(h.t, rec.Body)
}

// status 用调用方 Key 发一个无请求体的请求。
// status sends a bodyless request with a caller key.
func (h *callerKeyHarness) status(method, path, secret string) (int, callerEnvelope) {
	return h.exchange(method, path, "", secret)
}

// statusWithAdmin 用管理 token 发一个无请求体的请求。
// statusWithAdmin sends a bodyless request with the admin token.
func (h *callerKeyHarness) statusWithAdmin(method, path string) (int, callerEnvelope) {
	return h.exchange(method, path, "", testToken)
}

// TestCaller_UnauthorizedAndForbidden 守 G2-6 / R11：未授权一律 401（缺失与无效
// 共用同一文案）、越权一律 403 且零触达业务端口、跨调用方按 404 回答且不泄露
// 会话存在性——三种拒绝各走一条真实 Key。
// 逆改点：坏凭据回落成管理员 → 401 变 200；作用域只查"已认证" → 403 变 200；
// 归属只按 session_id 查找 → 404 变 200 且运行入口被触达。
func TestCaller_UnauthorizedAndForbidden(t *testing.T) {
	h := newCallerKeyHarness(t)
	readOnly := h.issueKey("caller-1", core.ScopeSessionsRead)
	writer := h.issueKey("caller-1", core.ScopeSessionsWrite)

	// 三条拒绝各带合法请求体：认证 / 作用域若漏判，请求会一路走到建会话并返回
	// 200，红态因此指向"判定缺失"而不是"请求体不合法"。
	// Every denial carries a valid body so a missing check surfaces as 200, not 400.
	code, env := h.exchange(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`, "")
	require.Equal(t, http.StatusUnauthorized, code, "缺凭据必须 401")
	require.Equal(t, codeUnauthorized, env.Code)
	require.Equal(t, messageUnauthorized, env.Message)

	code, env = h.exchange(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`, "not-a-key")
	require.Equal(t, http.StatusUnauthorized, code, "无效凭据与缺失共用同一 401")
	require.Equal(t, messageUnauthorized, env.Message)

	code, env = h.exchange(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`, readOnly.Secret)
	require.Equal(t, http.StatusForbidden, code, "只有读 scope 不得建会话")
	require.Equal(t, codeForbidden, env.Code)
	require.Equal(t, messageScopeDenied, env.Message)
	require.Zero(t, h.sessions.getOrCreateCallCount(), "作用域拒绝不得触达会话仓储")

	h.sessions.seed(core.SessionRecord{
		ID: "sess-other", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u1",
		CallerID: "caller-2", Status: statusActive, CreatedAt: authFixedNow, LastActiveAt: authFixedNow,
	})
	code, env = h.exchange(http.MethodPost, "/api/v1/sessions/sess-other/messages", `{"message":"hi"}`, writer.Secret)
	require.Equal(t, http.StatusNotFound, code, "跨调用方一律 404，不泄露会话存在")
	require.Equal(t, codeNotFound, env.Code)
	require.Empty(t, h.runs.requests(), "被拒请求不得触达运行入口")
}

// TestManagement_RejectsCallerKey 守 C-28 / R11：管理端点只接受 admin token——
// 同一把 Key 在业务面可用（200），到了管理面一律 403；管理 token 照常 200。
// 逆改点：管理面不探测 caller key → 第一例变 401；把"已认证"当管理资格 → 第二例变 200。
func TestManagement_RejectsCallerKey(t *testing.T) {
	h := newCallerKeyHarness(t)
	key := h.issueKey("caller-1", core.ScopeSessionsRead, core.ScopeSessionsWrite)

	code, _ := h.exchange(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`, key.Secret)
	require.Equal(t, http.StatusOK, code, "同一把 Key 在业务面上是有效的")

	code, env := h.status(http.MethodGet, "/api/v1/callers", key.Secret)
	require.Equal(t, http.StatusForbidden, code, "有效 caller key 仍不得访问管理端点")
	require.Equal(t, codeForbidden, env.Code)
	require.Equal(t, messageForbidden, env.Message)

	code, env = h.statusWithAdmin(http.MethodGet, "/api/v1/callers")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, codeOK, env.Code)
}

// TestKeyIssuer_RejectsAdminAndWildcardScopes 守 C-28 / R11：空、通配与管理面 scope
// 在**发放时**拒绝（`ErrInvalidScope`），且被拒的签发零写入——既不留 Key 行，也不
// 留 pending 操作；合法 scope 的对照组证明拒绝来自校验而非"签发器永远失败"。
// 逆改点：签发不校验管理 / 通配 / 空 scope → 六例全部变绿，库里出现越权 Key。
func TestKeyIssuer_RejectsAdminAndWildcardScopes(t *testing.T) {
	h := newCallerKeyHarness(t)
	bad := [][]string{
		nil,
		{},
		{"*"},
		{core.AgentInvokeWildcard},
		{core.ScopeCallersAdmin},
		{core.ScopeSessionsRead, core.ScopeCallersAdmin},
	}
	for _, scopes := range bad {
		_, err := h.issueKeyErr("caller-1", scopes)
		require.ErrorIs(t, err, callers.ErrInvalidScope,
			"空 / 通配 / 管理 scope 必须在发放时拒绝: %v", scopes)
	}
	require.Zero(t, h.countKeys("caller-1"), "被拒的签发不得留下 Key 行")
	require.Zero(t, h.pendingOperations(), "被拒的签发不得留下 pending 操作")

	valid := h.issueKey("caller-1", core.ScopeSessionsWrite)
	require.NotEmpty(t, valid.Secret)
	require.Equal(t, 1, h.countKeys("caller-1"))
	code, _ := h.exchange(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`, valid.Secret)
	require.Equal(t, http.StatusOK, code, "对照组：合法 scope 签出的 Key 端到端可用")
}

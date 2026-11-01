package web

// 本文件是 `GET /api/v1/diagnostics` 的 HTTP 层判据（A-F2 / C-F02）：`request_id`
// 作为主锚点——Run 准入在 Run 账本创建之前被拒时还没有 run_id，管理端只能按它
// 定位事件。覆盖四类边界：缺锚点、非法形状（不回显）、无命中、权限边界。
//
// 逆改点：
//   - 把 `request_id` 从锚点集合里去掉 → 第一个子用例从 200 变 400；
//   - 按 16 位十六进制死判形状 → "unknown"（生成侧随机失败的退化值）从 200 变 400；
//   - 把用户给的值回显到 400 响应 → 泄漏断言红；
//   - 形状校验挪到端口之后 → "非法值零触达端口"断言红。
//
// This file pins the request_id anchor of the diagnostics endpoint: shape denials never
// echo the value, the "unknown" fallback stays legal, and auth failures never reach the
// reader port.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/diagnostics"
)

// fakeDiagnosticsReader 记录每次读取的查询条件，供"是否触达端口"与"透传了哪个锚点"
// 两类断言共用。
// fakeDiagnosticsReader records each query so tests can assert reach and anchor passing.
type fakeDiagnosticsReader struct {
	queries []diagnostics.Query
	events  []diagnostics.DiagnosticEvent
	err     error
	// config 非空时按它报告采样配置（`sampled_out` 判据需要 SampleRate < 1）；为空时
	// 报告全量采样（`missing` 侧的默认口径）。
	// config overrides the reported sampling config; nil means full sampling.
	config *diagnostics.Config
}

// List 记录查询条件并按预置结果返回。
// List records the query and returns the preloaded page.
func (r *fakeDiagnosticsReader) List(_ context.Context, q diagnostics.Query) ([]diagnostics.DiagnosticEvent, error) {
	r.queries = append(r.queries, q)
	return r.events, r.err
}

// Config 让空页按"全量采样"解释（`missing=true`）；给了 config 时按它报告。
// Config reports the override when set, full sampling otherwise.
func (r *fakeDiagnosticsReader) Config() diagnostics.Config {
	if r.config != nil {
		return *r.config
	}
	return diagnostics.Config{RetentionDays: 30, SampleRate: 1}
}

// newDiagnosticsServer 装配管理面认证闸门与假读取端口。
// newDiagnosticsServer wires the admin guard and the fake reader port.
func newDiagnosticsServer(t *testing.T) (*Server, *fakeDiagnosticsReader) {
	t.Helper()
	reader := &fakeDiagnosticsReader{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.Diagnostics = reader
	})
	return srv, reader
}

// TestDiagnostics_RequestIDAnchor 判 `request_id` 单独即可作锚点，且与 run_id 同时
// 给出时按 AND 语义一起透传（多锚点收窄，不放大）。
// 逆改点见文件头。
func TestDiagnostics_RequestIDAnchor(t *testing.T) {
	t.Parallel()
	srv, reader := newDiagnosticsServer(t)

	alone := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
		"/api/v1/diagnostics?request_id=0123456789abcdef", ""), testToken))
	require.Equal(t, http.StatusOK, alone.Code, "request_id 是合法主锚点")
	require.Len(t, reader.queries, 1)
	require.Equal(t, "0123456789abcdef", reader.queries[0].RequestID)
	require.Empty(t, reader.queries[0].RunID)

	fallback := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
		"/api/v1/diagnostics?request_id=unknown", ""), testToken))
	require.Equal(t, http.StatusOK, fallback.Code, "生成侧退化的字面量 unknown 必须合法")

	both := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
		"/api/v1/diagnostics?request_id=unknown&run_id=run-1", ""), testToken))
	require.Equal(t, http.StatusOK, both.Code)
	require.Len(t, reader.queries, 3)
	require.Equal(t, "unknown", reader.queries[2].RequestID)
	require.Equal(t, "run-1", reader.queries[2].RunID, "多锚点按 AND 语义一起透传")
}

// TestDiagnostics_RequestIDShapeDeniedWithoutEcho 判形状拒绝：字符集与长度越界都是
// 400，且响应体**不回显**用户给的值，端口零触达。
// 逆改点：把校验放到 `List` 之后 → queries 非空；把 raw 拼进文案 → 回显断言红。
func TestDiagnostics_RequestIDShapeDeniedWithoutEcho(t *testing.T) {
	t.Parallel()
	srv, reader := newDiagnosticsServer(t)

	cases := []struct{ name, raw string }{
		{"含空格", "abc%20def"},
		{"含斜杠", "abc/def"},
		{"含百分号", "abc%25def"},
		{"超长", strings.Repeat("a", 65)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
				"/api/v1/diagnostics?request_id="+tc.raw, ""), testToken))
			require.Equal(t, http.StatusBadRequest, rec.Code)
			env := decodeAuthEnvelope(t, rec)
			require.Equal(t, codeBadRequest, env.Code)
			require.Equal(t, messageDiagnosticRequestIDInvalid, env.Message)
		})
	}
	require.Empty(t, reader.queries, "形状非法不得触达端口")
}

// TestDiagnostics_RequestIDIsNotAnAnchorAlone 判"空值等于没给"：`request_id=` 与
// 完全不给一样，仍是 400 锚点缺失；400 文案必须点名 request_id（管理员据此知道
// 可以按它查）。
// 逆改点：把空串当成合法锚点透传 → 端口被触达且 `Validate` 判无锚点，回答变成 200
// 或 500，两种都与 400 断言不符。
func TestDiagnostics_RequestIDEmptyIsNoAnchor(t *testing.T) {
	t.Parallel()
	srv, reader := newDiagnosticsServer(t)

	rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
		"/api/v1/diagnostics?request_id=", ""), testToken))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Contains(t, env.Message, "request_id", "400 文案必须点名可选锚点")
	require.Empty(t, reader.queries)
}

// TestDiagnostics_AuthBoundaryNeverReachesPort 判权限边界：缺凭据与错凭据都是 401，
// 且端口零触达——诊断读取是管理面操作，未认证请求不得消耗读取端口。
// 逆改点：把认证挪到处理函数内部（先查库再判身份）→ queries 非空。
func TestDiagnostics_AuthBoundaryNeverReachesPort(t *testing.T) {
	t.Parallel()
	srv, reader := newDiagnosticsServer(t)

	anchor := "/api/v1/diagnostics?request_id=0123456789abcdef"
	missing := serveAdmin(srv, adminRequest(http.MethodGet, anchor, ""))
	require.Equal(t, http.StatusUnauthorized, missing.Code)

	wrong := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, anchor, ""), "not-the-token"))
	require.Equal(t, http.StatusUnauthorized, wrong.Code)

	require.Empty(t, reader.queries, "未认证请求不得触达诊断读取端口")

	ok := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, anchor, ""), testToken))
	require.Equal(t, http.StatusOK, ok.Code)
	require.Len(t, reader.queries, 1)
}

// TestDiagnostics_EmptyPageUnderSamplingReportsSampledOut 判 SC-007 / FR-064 的
// `sampled_out=true` 侧：采样率 < 1 时"空页"无法区分"本来没有"与"被采样丢光"，
// 必须报 `sampled_out=true` 且**不得**报 `missing`；全量采样下仍是 `missing=true`
// 且 `sampled_out=false`（两侧互斥，反向断言保留）；有记录时两者都为 false。
//
// 逆改点：把 `config.SampleRate < DefaultSampleRate` 的比较写反、或删掉
// `result.SampledOut = true` 赋值 → 第一个子用例从 `sampled_out=true` 变成
// `missing=true`（或两者皆 false），断言逐字红。
// TestDiagnostics_EmptyPageUnderSamplingReportsSampledOut pins the sampled-out side.
func TestDiagnostics_EmptyPageUnderSamplingReportsSampledOut(t *testing.T) {
	t.Parallel()

	type page struct {
		Data struct {
			Events     []DiagnosticEventView `json:"events"`
			Missing    bool                  `json:"missing"`
			SampledOut bool                  `json:"sampled_out"`
		} `json:"data"`
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) page {
		t.Helper()
		var got page
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}

	t.Run("空页且采样率小于一", func(t *testing.T) {
		reader := &fakeDiagnosticsReader{config: &diagnostics.Config{RetentionDays: 30, SampleRate: 0.5}}
		srv := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
			o.Diagnostics = reader
		})
		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
			"/api/v1/diagnostics?run_id=run-never-ran", ""), testToken))
		require.Equal(t, http.StatusOK, rec.Code)
		got := decode(t, rec)
		require.Empty(t, got.Data.Events)
		require.True(t, got.Data.SampledOut, "采样率<1 的空页必须报 sampled_out=true")
		require.False(t, got.Data.Missing, "采样率<1 时不得断言 missing——丢光了与本来没有不可分")
	})

	t.Run("空页且全量采样", func(t *testing.T) {
		srv, _ := newDiagnosticsServer(t)
		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
			"/api/v1/diagnostics?run_id=run-never-ran", ""), testToken))
		require.Equal(t, http.StatusOK, rec.Code)
		got := decode(t, rec)
		require.True(t, got.Data.Missing, "全量采样下空页必须报 missing=true")
		require.False(t, got.Data.SampledOut)
	})

	t.Run("采样率小于一但有记录", func(t *testing.T) {
		reader := &fakeDiagnosticsReader{
			config: &diagnostics.Config{RetentionDays: 30, SampleRate: 0.5},
			events: []diagnostics.DiagnosticEvent{{EventID: "evt-1", Kind: diagnostics.KindLLMCall}},
		}
		srv := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
			o.Diagnostics = reader
		})
		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
			"/api/v1/diagnostics?run_id=run-1", ""), testToken))
		require.Equal(t, http.StatusOK, rec.Code)
		got := decode(t, rec)
		require.Len(t, got.Data.Events, 1)
		require.False(t, got.Data.Missing, "有记录时不得报 missing")
		require.False(t, got.Data.SampledOut, "有记录时不得报 sampled_out")
	})
}

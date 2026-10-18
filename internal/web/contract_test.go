package web

// 本文件是契约层验收（模块卡 §4.1 / W2）：信封形态、时间戳与 Content-Type、未知路由、
// 错误类型映射、视图字段形态。错误映射全部按 `errors.As` 分支判定——改一句错误文案
// 不得改变状态码（约束 22）。
// Contract-level acceptance: envelope shape, timestamp/content type, unknown routes,
// type-based error mapping, and view field shapes.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestContract_EnvelopeShape 验证成功含 data、失败省略 data（约束 3）。
// 逆改点：信封字段改名、错误响应带 data、Content-Type 改成 text/plain → 红。
func TestContract_EnvelopeShape(t *testing.T) {
	s := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	var ok map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ok))
	require.Equal(t, "OK", ok["code"])
	require.Equal(t, "success", ok["message"])
	require.Equal(t, "2026-10-18T12:00:00Z", ok["timestamp"], "timestamp 固定 RFC3339 UTC")
	require.Contains(t, ok, "data")

	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	var fail map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fail))
	require.Equal(t, "NOT_FOUND", fail["code"])
	require.NotContains(t, fail, "data", "失败信封不得带 data 键")

	// 契约保留码本 US 不产生，但常量集合必须完整（约束 4）。
	require.Equal(t, "UNSUPPORTED_TYPE", codeUnsupportedType)
	require.Equal(t, "NOT_CONFIGURED", codeNotConfigured)
}

// TestContract_TimestampAndContentType 验证成功与失败两条路径的时间戳和 Content-Type。
// 逆改点：时间戳改成本地偏移或不带 Z → 解析与字面量断言红。
func TestContract_TimestampAndContentType(t *testing.T) {
	s := newTestServer(t, nil)

	for _, path := range []string{"/api/v1/health", "/api/v1/nope"} {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"), path)

		var env Envelope
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), path)
		require.Equal(t, "2026-10-18T12:00:00Z", env.Timestamp, path)
		parsed, err := time.Parse(time.RFC3339, env.Timestamp)
		require.NoError(t, err, path)
		require.Equal(t, time.UTC, parsed.Location(), "时间戳必须是无偏移的 UTC")
	}
}

// TestContract_UnknownRouteIs404Envelope 验证所有未匹配路径与方法都落到 404 信封。
// 逆改点：删掉 catch-all → net/http 返回纯文本 404（JSON 解析失败）；
// 或注册了方法无关的 405 兜底 → 状态码断言红。
func TestContract_UnknownRouteIs404Envelope(t *testing.T) {
	s := newTestServer(t, nil)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/nope"},
		{http.MethodGet, "/api/v1/sessions"}, // 控制台列表 = US-11，本 US 不注册
		{http.MethodPut, "/api/v1/health"},   // 方法不符也走 catch-all
		{http.MethodGet, "/"},                // 前缀之外
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, http.StatusNotFound, rec.Code, "%s %s", tc.method, tc.path)
		require.Contains(t, rec.Body.String(), `"code":"NOT_FOUND"`, "%s %s", tc.method, tc.path)
	}
}

// TestContract_ErrorMappingByType 逐个 errors.As 分支验证状态码（约束 22）。
// 逆改点：按错误文本匹配 → 改一句文案就改变状态码；500 直出原始错误 → 泄漏断言红。
func TestContract_ErrorMappingByType(t *testing.T) {
	s := newTestServer(t, nil)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	runCases := []struct {
		name       string
		err        error
		runCtx     context.Context
		wantStatus int
		wantCode   string
	}{
		{"Agent 不存在", &core.AgentNotFoundError{Name: "ghost"}, context.Background(), http.StatusNotFound, codeNotFound},
		{"同会话忙", &core.SessionBusyError{SessionID: "web-x"}, context.Background(), http.StatusConflict, codeSessionBusy},
		{"实例满", &core.InstanceFullError{Limit: 7}, context.Background(), http.StatusTooManyRequests, codeTooManyRuns},
		{"运行超时", errors.New("deadline surfaced as error"), expired, http.StatusGatewayTimeout, codeTimeout},
		{"其他错误", errors.New("disk on fire"), context.Background(), http.StatusInternalServerError, codeInternal},
	}
	for _, tc := range runCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.writeRunError(rec, tc.err, tc.runCtx)
			require.Equal(t, tc.wantStatus, rec.Code)
			require.Contains(t, rec.Body.String(), `"code":"`+tc.wantCode+`"`)
			require.NotContains(t, rec.Body.String(), "disk on fire", "500 不得透出底层错误串")
		})
	}

	t.Run("仓储未命中", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.writeStoreError(rec, fs.ErrNotExist, "web-x")
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Contains(t, rec.Body.String(), `"code":"NOT_FOUND"`)
		require.Contains(t, rec.Body.String(), "web-x", "404 回显会话标识")
	})
}

// TestContract_ViewFieldShapes 验证视图字段形态（约束 34）：空集合输出 `[]`、
// 未标可选的字符串输出 `""`。
// 逆改点：nil 切片直接序列化 → `"messages":null`；时间零值渲染 0001 年 → 空串断言红。
func TestContract_ViewFieldShapes(t *testing.T) {
	t.Run("空集合输出 []", func(t *testing.T) {
		raw, err := json.Marshal(sessionDetailView(core.SessionRecord{}, nil))
		require.NoError(t, err)
		require.Contains(t, string(raw), `"messages":[]`)
		require.NotContains(t, string(raw), `"messages":null`)

		raw, err = json.Marshal(profileView(core.AgentEntry{Name: "ghost"}))
		require.NoError(t, err)
		require.Contains(t, string(raw), `"tools":[]`)
		require.Contains(t, string(raw), `"skills":[]`)
		require.Contains(t, string(raw), `"problems":[]`)

		raw, err = json.Marshal(infoView(Options{}, time.Now()))
		require.NoError(t, err)
		require.Contains(t, string(raw), `"providers":[]`)
	})

	t.Run("未标可选字符串输出空串", func(t *testing.T) {
		raw, err := json.Marshal(runView(core.RunRequest{}, core.RunResult{}, time.Time{}, time.Time{}))
		require.NoError(t, err)
		for _, field := range []string{
			`"run_id":""`, `"session_id":""`, `"final_text":""`,
			`"reason":""`, `"config_hash":""`, `"started_at":""`, `"finished_at":""`,
		} {
			require.Contains(t, string(raw), field)
		}

		raw, err = json.Marshal(sessionSummaryView(core.SessionRecord{}))
		require.NoError(t, err)
		require.Contains(t, string(raw), `"created_at":""`)
		require.Contains(t, string(raw), `"session_id":""`)
	})
}

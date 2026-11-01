package web

// 本文件守入口速率拒绝的诊断事件（V-8）：请求还没变成 Run，`request_id` 是它
// **唯一**的主锚点（契约 §2.9）；只记拒绝、不记放行——放行是常态，逐请求留痕会把
// 采样额度耗在正常流量上。
//
// 逆改点：去掉 `request_id` 锚点 → 关联断言红；把放行也记一条 → 第二段的"零新增"
// 断言红；`class` 不加 `ingress_` 前缀 → 与运行路径的 `run` 类目撞名，子用例红。
//
// This file pins the ingress denial event: request-anchored, deny-only, and class
// prefixed so it never collides with the run path's class values.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// captureDiagWriter 捕获入口中间件写出的诊断事件。
// captureDiagWriter captures the events the ingress middleware writes.
type captureDiagWriter struct {
	events []diagnostics.DiagnosticEvent
}

func (w *captureDiagWriter) Record(_ context.Context, ev diagnostics.DiagnosticEvent) {
	w.events = append(w.events, ev)
}

// TestIngressDenial_RecordsRequestAnchoredEvent 守拒绝事件：`quota_decision` + deny +
// `rate_limited`，关联只有 `request_id`，类目带 `ingress_` 前缀。
//
// 逆改点见文件头。
func TestIngressDenial_RecordsRequestAnchoredEvent(t *testing.T) {
	writer := &captureDiagWriter{}
	s := &Server{opts: Options{DiagnosticWriter: writer}}
	r := httptest.NewRequest("POST", "/runs", nil)
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, "req_1"))

	s.recordIngressDenial(r, callers.IngressRun)

	require.Len(t, writer.events, 1)
	ev := writer.events[0]
	require.Equal(t, diagnostics.KindQuotaDecision, ev.Kind)
	require.Equal(t, "req_1", ev.Correlation.RequestID)
	require.Empty(t, ev.Correlation.RunID, "请求还没变成 Run，不得伪造 run_id")
	require.Equal(t, diagnostics.DecisionDeny, ev.Decision)
	require.Equal(t, diagnostics.ReasonRateLimited, ev.ReasonCode)
	require.Equal(t, "ingress_run", ev.Attrs["class"])
}

// TestIngressDenial_RequestIDIsServerGenerated 判 `request_id` 的来源：它由
// `recoverMiddleware` 生成，客户端带的请求头**不参与**判定——接受客户端提供的关联
// 标识等于允许把两件不相干的事串到一条时间线上（A-F2）。
// 逆改点：改成从 `X-Request-Id` 头读取 → 事件关联等于 "attacker-controlled"，断言红。
func TestIngressDenial_RequestIDIsServerGenerated(t *testing.T) {
	writer := &captureDiagWriter{}
	gate := &recordingIngress{err: callers.ErrRateLimited}
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"c1-secret": callerPrincipal("c1", core.ScopeSessionsWrite),
	}}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = businessGuard(t, keys)
		o.Ingress = gate
		o.DiagnosticWriter = writer
	})

	req := withBearer(adminRequest(http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`), "c1-secret")
	req.Header.Set("X-Request-Id", "attacker-controlled")
	require.Equal(t, http.StatusTooManyRequests, serveAdmin(srv, req).Code)

	require.Len(t, writer.events, 1)
	id := writer.events[0].Correlation.RequestID
	require.NotEqual(t, "attacker-controlled", id, "客户端头不得决定关联标识")
	require.Regexp(t, "^[0-9a-f]{16}$", id, "服务端生成 8 字节 → 16 位小写十六进制")
}

// TestIngressDenial_SkipsWithoutAnchorOrWriter 守两条短路：缺 `request_id` 与缺写入器
// 都不写事件（不伪造关联、不 panic）。
//
// 逆改点：缺锚点时补一个空串锚点写下去 → 事件长度断言红（`Validate` 会先拒）。
func TestIngressDenial_SkipsWithoutAnchorOrWriter(t *testing.T) {
	writer := &captureDiagWriter{}
	s := &Server{opts: Options{DiagnosticWriter: writer}}
	s.recordIngressDenial(httptest.NewRequest("POST", "/runs", nil), callers.IngressIngest)
	require.Empty(t, writer.events, "没有 request_id 就不发")

	unwired := &Server{}
	unwired.recordIngressDenial(httptest.NewRequest("POST", "/runs", nil), callers.IngressRead)
}

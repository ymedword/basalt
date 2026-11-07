package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

type flowHandlerProbe struct {
	FlowBackend
	starts, controls, validations int
	accepted                      bool
	view                          FlowTaskView
	last                          flow.StartRequest
	expected                      flow.TaskVersion
	failure                       error
}

func (p *flowHandlerProbe) Start(_ context.Context, req flow.StartRequest) (FlowTaskView, bool, error) {
	p.starts++
	p.last = req
	return p.view, p.accepted, p.failure
}

func (p *flowHandlerProbe) Cancel(_ context.Context, req flow.CancelRequest) (FlowTaskView, error) {
	p.controls++
	p.expected = req.Expected
	return p.view, p.failure
}

func (p *flowHandlerProbe) Resume(_ context.Context, req flow.ResumeRequest) (FlowTaskView, error) {
	p.controls++
	p.expected = req.Expected
	return p.view, p.failure
}

func (p *flowHandlerProbe) Validate(context.Context, core.Principal, core.Actor, flow.Definition) error {
	p.validations++
	return p.failure
}

func (p *flowHandlerProbe) Task(context.Context, core.Principal, string, string, int) (FlowTaskView, error) {
	return p.view, p.failure
}

func flowProbeView() FlowTaskView {
	return FlowTaskView{ID: "ft_fixture", RootID: "ft_fixture", Status: flow.Queued, Version: flow.TaskVersion{Revision: 0, StateVersion: 1, ExecutionEpoch: 1}, ETag: flow.TaskVersion{Revision: 0, StateVersion: 1, ExecutionEpoch: 1}.ETag("ft_fixture"), Constraints: []string{}, Slots: []FlowSlotView{}, Claims: []FlowClaimView{}, Attempts: []FlowAttemptView{}}
}

func TestFlow_HTTPStrictInputAndIdempotentAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
		accepted   bool
		status     flow.TaskStatus
	}{
		{"new", `{"definition_uid":"fd_fixture","input":{"answer":"ok"}}`, 202, true, flow.Queued},
		{"replay", `{"definition_uid":"fd_fixture","input":{"answer":"ok"}}`, 200, false, flow.Queued},
		{"replay-closing", `{"definition_uid":"fd_fixture","input":{}}`, 200, false, flow.Cancelling},
		{"duplicate", `{"definition_uid":"fd_fixture","definition_uid":"SENTINEL","input":{}}`, 400, false, flow.Queued},
		{"control-injection", `{"definition_uid":"fd_fixture","input":{},"Flow":{"authority":"SENTINEL"}}`, 400, false, flow.Queued},
		{"trailing", `{"definition_uid":"fd_fixture","input":{}} {"secret":"SENTINEL"}`, 400, false, flow.Queued},
		{"null", `null`, 400, false, flow.Queued},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &flowHandlerProbe{view: flowProbeView(), accepted: tc.accepted}
			probe.view.Status = tc.status
			srv := newTestServer(t, func(o *Options) { o.Flows = probe })
			r := httptest.NewRequest(http.MethodPost, "/api/v1/flow-tasks", strings.NewReader(tc.body))
			r.Header.Set("Idempotency-Key", "same-key")
			w := httptest.NewRecorder()
			srv.handleFlowStart(w, r, callerPrincipal("owner", core.ScopeFlowsWrite))
			require.Equal(t, tc.want, w.Code)
			require.NotContains(t, w.Body.String(), "SENTINEL")
			if tc.want == 400 {
				require.Zero(t, probe.starts)
			} else {
				require.Equal(t, 1, probe.starts)
				require.Equal(t, "owner", probe.last.Principal.CallerID)
				require.Equal(t, "same-key", probe.last.IdempotencyKey)
				require.Equal(t, probe.view.ETag, w.Header().Get("ETag"))
			}
		})
	}
}

func TestFlow_HTTPFullETagAndMissingPreconditionZeroWrite(t *testing.T) {
	probe := &flowHandlerProbe{view: flowProbeView()}
	srv := newTestServer(t, func(o *Options) { o.Flows = probe })
	for _, etag := range []string{"", "*", "W/" + probe.view.ETag, flow.TaskVersion{Revision: 0, StateVersion: 1, ExecutionEpoch: 1}.ETag("other-task"), `"not-base64"`} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/flow-tasks/ft_fixture/cancel", strings.NewReader(`{"operation_id":"cancel-1","reason":"user cancelled"}`))
		r.SetPathValue("id", "ft_fixture")
		r.Header.Set("If-Match", etag)
		w := httptest.NewRecorder()
		srv.handleFlowCancel(w, r, callerPrincipal("owner", core.ScopeFlowsWrite))
		require.Equal(t, 412, w.Code, etag)
		require.Zero(t, probe.controls)
	}
	probe.view.Status = flow.Cancelling
	probe.view.CloseIntent = flow.CloseCancel
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"operation_id":"cancel-1","reason":"user cancelled"}`))
	r.SetPathValue("id", "ft_fixture")
	r.Header.Set("If-Match", probe.view.ETag)
	w := httptest.NewRecorder()
	srv.handleFlowCancel(w, r, callerPrincipal("owner", core.ScopeFlowsWrite))
	require.Equal(t, 202, w.Code)
	require.Equal(t, 1, probe.controls)
	require.Equal(t, probe.view.Version, probe.expected)
}

// TestFlow_HTTPRootBusyMapsTo409 固定管理面 resume 撞上执行者根租约的线上契约：
// 必须映射 409 FLOW_ROOT_BUSY，**不得**落成 500，也不得在参数校验阶段短路
// （FR-24-37 / 模块卡 §3.1.6，T023）。
func TestFlow_HTTPRootBusyMapsTo409(t *testing.T) {
	probe := &flowHandlerProbe{view: flowProbeView(), failure: flow.ErrFlowRootBusy}
	srv := newTestServer(t, func(o *Options) { o.Flows = probe })
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"operation_id":"resume-1","reason":"worker_lease_expired"}`))
	r.SetPathValue("id", "ft_fixture")
	r.Header.Set("If-Match", probe.view.ETag)
	w := httptest.NewRecorder()
	srv.handleFlowResume(w, r, callerPrincipal("owner", core.ScopeFlowsWrite))
	require.Equal(t, 1, probe.controls, "冲突发生在后端调用内，不是参数校验失败")
	require.Equal(t, probe.view.Version, probe.expected)
	require.Equal(t, http.StatusConflict, w.Code, "根租约冲突不得落成 500")
	require.Contains(t, w.Body.String(), "FLOW_ROOT_BUSY")
}

func TestFlow_HTTPScopesAndAdminAreSeparate(t *testing.T) {
	probe := &flowHandlerProbe{view: flowProbeView()}
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{"reader": callerPrincipal("owner", core.ScopeFlowsRead), "writer": callerPrincipal("owner", core.ScopeFlowsWrite)}}
	srv := newTestServer(t, func(o *Options) { o.Flows = probe; o.AdminAuth = businessGuard(t, keys) })
	for _, tc := range []struct {
		method, path, token, body string
		want                      int
	}{
		{http.MethodPost, "/api/v1/flow-tasks", "", `{"definition_uid":"fd_fixture","input":{}}`, 401},
		{http.MethodPost, "/api/v1/flow-tasks", "reader", `{"definition_uid":"fd_fixture","input":{}}`, 403},
		{http.MethodGet, "/api/v1/flow-tasks/ft_fixture", "writer", "", 403},
		{http.MethodPost, "/api/v1/admin/flows/validate", "writer", `{"definition":{"format_version":1,"name":"fixture"}}`, 403},
	} {
		r := withBearer(adminRequest(tc.method, tc.path, tc.body), tc.token)
		r.Header.Set("Idempotency-Key", "key")
		w := serveAdmin(srv, r)
		require.Equal(t, tc.want, w.Code, tc.path)
	}
	require.Zero(t, probe.starts)
	require.Zero(t, probe.validations)
}

func TestFlow_HTTPValidationPathDoesNotEchoValues(t *testing.T) {
	srv := newTestServer(t, nil)
	w := httptest.NewRecorder()
	srv.writeFlowError(w, &core.SchemaViolationError{Path: "$/answer"})
	require.Equal(t, 400, w.Code)
	var envelope Envelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.Equal(t, "FLOW_DEFINITION_INVALID", envelope.Code)
	require.Contains(t, w.Body.String(), "$/answer")
}

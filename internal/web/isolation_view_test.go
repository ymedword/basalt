package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

type isolationReaderFake struct {
	rows  []core.SandboxExecutionView
	err   error
	runID string
}

func (r *isolationReaderFake) PendingSandboxExecutions(context.Context, string) ([]core.SandboxExecution, error) {
	return nil, nil
}

func (r *isolationReaderFake) SandboxExecutionsByRun(_ context.Context, id string) ([]core.SandboxExecutionView, error) {
	r.runID = id
	return r.rows, r.err
}

func TestUS20_ConsoleReadOnly(t *testing.T) {
	opts := defaultTestOptions(t)
	opts.Isolation.Enabled = true
	if _, err := NewServer(opts); err == nil {
		t.Fatal("enabled isolation without reader accepted")
	}
	s := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.Isolation = IsolationView{Enabled: true, Backend: "docker", Tools: map[string]string{"run_code": "gvisor"}, Profiles: []IsolationProfileView{{Name: "gvisor", Runtime: "runsc", Bound: true, Available: true}}}
		o.IsolationExecutions = &isolationReaderFake{}
	})
	get := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/sandbox", ""), testToken))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"isolation"`) {
		t.Fatalf("missing view: %d %s", get.Code, get.Body.String())
	}
	for _, path := range []string{"/api/v1/sandbox/isolation", "/api/v1/isolation"} {
		put := serveAdmin(s, withBearer(adminRequest(http.MethodPut, path, `{"enabled":true}`), testToken))
		if put.Code == http.StatusOK {
			t.Fatal("unexpected isolation editing endpoint")
		}
	}
	disabled := newTestServer(t, nil).sandboxView().Isolation
	if disabled.Enabled || disabled.Profiles == nil || disabled.Tools["shell"] != "local" {
		t.Fatalf("disabled view: %+v", disabled)
	}
}

func TestUS20_RunDetailProjection(t *testing.T) {
	reader := &isolationReaderFake{rows: []core.SandboxExecutionView{{ToolInvocationID: 9, ToolName: "run_code", Profile: "gvisor", Runtime: "runsc", State: core.SandboxFinished, CleanupState: core.CleanupFailed, ErrorReason: "cleanup_failed", ArtifactCount: 1, ArtifactBytes: 5, StartedAt: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)}}}
	s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken); o.IsolationExecutions = reader })
	request := func() string {
		t.Helper()
		res := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/runs/run-1", ""), testToken))
		if res.Code != http.StatusOK {
			t.Fatalf("detail %d: %s", res.Code, res.Body.String())
		}
		return res.Body.String()
	}
	body := request()
	var envelope struct {
		Data RunDetailView `json:"data"`
	}
	if json.Unmarshal([]byte(body), &envelope) != nil || len(envelope.Data.IsolationExecutions) != 1 || envelope.Data.IsolationExecutions[0].CleanupState != "failed" || reader.runID != "run-1" {
		t.Fatalf("detail projection %s", body)
	}
	reader.rows = nil
	if body := request(); !strings.Contains(body, `"isolation_executions":[]`) {
		t.Fatal("empty executions serialized as null")
	}
	reader.err = errors.New("private DSN detail")
	res := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/runs/run-1", ""), testToken))
	if res.Code != http.StatusInternalServerError || strings.Contains(res.Body.String(), "private DSN") {
		t.Fatalf("reader failure leaked or hidden: %d %s", res.Code, res.Body.String())
	}
}

func TestUS20_OpenAPIContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	requireViewMatchesSchema(t, doc, "IsolationView", normalizedIsolationView(IsolationView{}))
	requireViewMatchesSchema(t, doc, "IsolationProfileView", IsolationProfileView{Name: "gvisor", Runtime: "runsc"})
	requireViewMatchesSchema(t, doc, "IsolationExecutionView", isolationExecutionView(core.SandboxExecutionView{Profile: "gvisor", Runtime: "runsc", State: core.SandboxUnknown, CleanupState: core.CleanupFailed, ErrorReason: "cleanup_failed"}))
	requireViewMatchesSchema(t, doc, "RunDetail", runDetailView(core.RunDetail{RunRecord: core.RunRecord{RunID: "run-1", RequestID: "request-1", Channel: "scheduler", Trigger: core.TriggerCron, StartedAt: time.Now().UTC()}, Status: core.StatusCompleted}))
}

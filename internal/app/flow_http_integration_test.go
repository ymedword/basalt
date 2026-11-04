//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/web"
)

// This fixture keeps the production HTTP, admission, Worker, Run and storage
// chain. Only the external model protocol is scripted; all addresses are loopback.
type flowHTTPFixture struct {
	t           *testing.T
	rt          *heavyRuntime
	base, admin string
	client      *http.Client
	model       *scriptedProviderClient
}

func newFlowHTTPFixture(t *testing.T, configure ...func(*cli.Config)) *flowHTTPFixture {
	t.Helper()
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader")
	fx.cfg.Flows = flow.DefaultConfig()
	fx.cfg.Flows.Enabled = true
	fx.cfg.Callers.Ingress.Run = cli.IngressBudgetConfig{QPS: 100, Burst: 100}
	fx.cfg.Callers.Ingress.Read = cli.IngressBudgetConfig{QPS: 100, Burst: 100}
	fx.cfg.Callers.Ingress.Ingest = cli.IngressBudgetConfig{QPS: 100, Burst: 100}
	for _, apply := range configure {
		apply(fx.cfg)
	}
	response := core.ChatResponse{Content: `{"answer":"runtime complete"}`, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}
	model := &scriptedProviderClient{replies: []core.ChatResponse{response, response, response}}
	fx.override = model
	rt := fx.assemble(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
			require.NoError(t, err)
		}
	})
	// Server owns the listener once Run begins; its Close makes the extra cleanup
	// harmless only if guarded below rather than treating net.ErrClosed as a fault.
	addr := ln.Addr().String()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	admin := "synthetic-flow-http-admin-secret"
	guard, err := web.NewAuthGuardWithPrincipals(web.AdminAuthConfig{Mode: "token", Token: admin, AllowedHosts: []string{addr}, AllowedOrigins: []string{"http://" + addr}}, logger, time.Now, callers.NewAuthenticator(postgres.NewKeyStore(rt.db), adminTokenFingerprint{token: admin}))
	require.NoError(t, err)
	opts := fx.app().webOptions(rt)
	opts.Addr, opts.Listener, opts.Logger, opts.AdminAuth = addr, ln, logger, guard
	opts.Ingress, err = newIngressLimiter(fx.cfg, rt.db, time.Now)
	require.NoError(t, err)
	server, err := web.NewServer(opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("HTTP server did not join")
		}
	})
	rt.adminPosture.Set(core.AdminPostureToken)
	rt.flows.authenticationReady()
	require.NoError(t, rt.flows.Start(ctx))
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &flowHTTPFixture{t: t, rt: rt, base: "http://" + addr, admin: admin, client: &http.Client{Transport: transport, Timeout: 3 * time.Second}, model: model}
}

func (f *flowHTTPFixture) request(method, path, token, idem, etag string, body any) (int, http.Header, json.RawMessage, string) {
	f.t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		require.NoError(f.t, err)
	}
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.base+path, bytes.NewReader(raw))
	require.NoError(f.t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	require.NoError(f.t, err)
	defer func() { require.NoError(f.t, resp.Body.Close()) }()
	response, err := io.ReadAll(resp.Body)
	require.NoError(f.t, err)
	var envelope struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	require.NoError(f.t, json.Unmarshal(response, &envelope))
	return resp.StatusCode, resp.Header, envelope.Data, string(response)
}

func (f *flowHTTPFixture) issue(caller string, create bool, scopes ...string) string {
	f.t.Helper()
	keys := postgres.NewKeyStore(f.rt.db)
	if create {
		err := keys.CreateCaller(f.t.Context(), callers.CallerRecord{CallerID: caller, Kind: callers.CallerKindExternal, Name: caller, Status: callers.CallerStatusActive, CreatedAt: time.Now()}, &callers.Limit{QPS: 100, Burst: 100, MaxConcurrent: 4, TasksPerPeriod: 1, TokensPerPeriod: 100000, Period: callers.PeriodDay, Timezone: "UTC"})
		require.NoError(f.t, err)
	}
	issued, err := callers.NewKeyIssuer(keys, postgres.NewManagementStore(f.rt.db)).Issue(f.t.Context(), core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, callers.IssueRequest{CallerID: caller, Scopes: scopes})
	require.NoError(f.t, err)
	return issued.Secret
}

func TestFlow_HTTPRealWorkerOwnershipArtifactAndDualQuota(t *testing.T) {
	f := newFlowHTTPFixture(t)
	status, _, raw, response := f.request(http.MethodPost, "/api/v1/admin/flows/publish", f.admin, "", "", map[string]any{"operation_id": "publish-http", "expected_version": 0, "definition": liveFlowDefinition(t, f.rt)})
	require.Equal(t, 200, status, response)
	var definition web.FlowDefinitionView
	require.NoError(t, json.Unmarshal(raw, &definition))
	require.NotEmpty(t, definition.UID)
	owner := f.issue("cal_flow-owner", true, core.ScopeFlowsRead, core.ScopeFlowsWrite, core.AgentInvokeScopePrefix+"reader")
	reader := f.issue("cal_flow-owner", false, core.ScopeFlowsRead)
	other := f.issue("cal_flow-other", true, core.ScopeFlowsRead)
	start := map[string]any{"definition_uid": definition.UID, "input": map[string]any{"answer": 7}}
	status, _, _, response = f.request(http.MethodPost, "/api/v1/flow-tasks", owner, "start-http", "", start)
	require.Equal(t, 400, status, response)
	require.Contains(t, response, "$/answer")
	start["input"] = map[string]any{"answer": "private input sentinel"}
	status, headers, raw, response := f.request(http.MethodPost, "/api/v1/flow-tasks", owner, "start-http", "", start)
	require.Equal(t, 202, status, response)
	require.NotContains(t, response, "private input sentinel")
	var accepted web.FlowTaskView
	require.NoError(t, json.Unmarshal(raw, &accepted))
	require.Equal(t, accepted.ETag, headers.Get("ETag"))
	require.Equal(t, definition.Name, accepted.DefinitionName)
	for _, entry := range []struct {
		path, token, name string
		count             int
	}{
		{"/api/v1/flow-tasks", reader, definition.Name, 1},
		{"/api/v1/flow-tasks", reader, "missing-workflow", 0},
		{"/api/v1/flow-tasks", other, definition.Name, 0},
		{"/api/v1/admin/flow-tasks", f.admin, definition.Name, 1},
		{"/api/v1/admin/flow-tasks", f.admin, "missing-workflow", 0},
	} {
		status, _, pageRaw, pageResponse := f.request(http.MethodGet, entry.path+"?definition_name="+url.QueryEscape(entry.name), entry.token, "", "", nil)
		require.Equal(t, 200, status, pageResponse)
		var page web.FlowTaskPage
		require.NoError(t, json.Unmarshal(pageRaw, &page))
		require.Len(t, page.Items, entry.count)
		if entry.count > 0 {
			require.Equal(t, definition.Name, page.Items[0].DefinitionName)
			require.Equal(t, definition.Version, page.Items[0].DefinitionVersion)
		}
	}
	status, _, replay, response := f.request(http.MethodPost, "/api/v1/flow-tasks", owner, "start-http", "", start)
	require.Equal(t, 200, status, response)
	var repeated web.FlowTaskView
	require.NoError(t, json.Unmarshal(replay, &repeated))
	require.Equal(t, accepted.ID, repeated.ID)
	start["input"] = map[string]any{"answer": "different"}
	status, _, _, response = f.request(http.MethodPost, "/api/v1/flow-tasks", owner, "start-http", "", start)
	require.Equal(t, 409, status, response)
	require.Eventually(t, func() bool {
		task, err := f.rt.flows.store.ReadForWorker(t.Context(), accepted.ID)
		return err == nil && task.Status == flow.Succeeded
	}, 5*time.Second, 10*time.Millisecond)
	path := "/api/v1/flow-tasks/" + accepted.ID
	status, _, raw, response = f.request(http.MethodGet, path, reader, "", "", nil)
	require.Equal(t, 200, status, response)
	require.NotContains(t, response, "private input sentinel")
	require.NotContains(t, response, "runtime complete")
	var task web.FlowTaskView
	require.NoError(t, json.Unmarshal(raw, &task))
	require.Equal(t, flow.Succeeded, task.Status)
	require.NotNil(t, task.RemainingTokens)
	require.EqualValues(t, 99992, *task.RemainingTokens)
	status, _, raw, response = f.request(http.MethodGet, path+"/artifacts", owner, "", "", nil)
	require.Equal(t, 200, status, response)
	var artifacts web.FlowArtifactPage
	require.NoError(t, json.Unmarshal(raw, &artifacts))
	require.Len(t, artifacts.Items, 1)
	require.True(t, artifacts.Items[0].Readable)
	require.JSONEq(t, `{"answer":"runtime complete"}`, artifacts.Items[0].Content)
	status, _, raw, response = f.request(http.MethodGet, path+"/artifacts", reader, "", "", nil)
	require.Equal(t, 200, status, response)
	var redacted web.FlowArtifactPage
	require.NoError(t, json.Unmarshal(raw, &redacted))
	require.False(t, redacted.Items[0].Readable)
	require.Empty(t, redacted.Items[0].Content)
	require.NotContains(t, response, "runtime complete")
	for _, token := range []string{other, f.admin} {
		status, _, _, response = f.request(http.MethodGet, path, token, "", "", nil)
		require.Equal(t, 404, status, response)
	}
	status, _, raw, response = f.request(http.MethodGet, "/api/v1/admin/flow-tasks/"+accepted.ID, f.admin, "", "", nil)
	require.Equal(t, 200, status, response)
	var managed web.FlowTaskView
	require.NoError(t, json.Unmarshal(raw, &managed))
	require.Equal(t, accepted.ID, managed.ID)
	require.NotEmpty(t, managed.ETag)
	require.Len(t, managed.Attempts, 1)
	require.True(t, managed.Attempts[0].RunAdmitted)
	require.Empty(t, managed.Goal)
	require.Empty(t, managed.Claims)
	require.Nil(t, managed.Budget, "rescue view must not claim an unknown remaining budget is zero")
	require.NotContains(t, response, "private input sentinel")
	require.NotContains(t, response, "runtime complete")
	status, _, _, response = f.request(http.MethodGet, "/api/v1/admin/flow-tasks/"+accepted.ID, owner, "", "", nil)
	require.Equal(t, 403, status, response)
	var used, reserved int64
	for kind, expected := range map[string]int64{"tasks": 1, "tokens": 8} {
		require.NoError(t, f.rt.db.QueryRowContext(t.Context(), `SELECT used,reserved FROM caller_usage_periods WHERE caller_id=$1 AND kind=$2`, "cal_flow-owner", kind).Scan(&used, &reserved))
		require.Equal(t, expected, used, kind)
		require.Zero(t, reserved, kind)
	}
	// Root acceptance consumes no periodic task ticket. A second accepted task
	// reaches the original leaf quota, blocks, and issues no second Run or Chat.
	status, _, raw, response = f.request(http.MethodPost, "/api/v1/flow-tasks", owner, "start-http-2", "", start)
	require.Equal(t, 202, status, response)
	var denied web.FlowTaskView
	require.NoError(t, json.Unmarshal(raw, &denied))
	require.Eventually(t, func() bool {
		task, err := f.rt.flows.store.ReadForWorker(t.Context(), denied.ID)
		return err == nil && task.Status == flow.Blocked
	}, 5*time.Second, 10*time.Millisecond)
	status, _, raw, response = f.request(http.MethodGet, "/api/v1/flow-tasks/"+denied.ID, owner, "", "", nil)
	require.Equal(t, 200, status, response)
	var unadmitted web.FlowTaskView
	require.NoError(t, json.Unmarshal(raw, &unadmitted))
	require.Len(t, unadmitted.Attempts, 1)
	require.NotEmpty(t, unadmitted.Attempts[0].RunID)
	require.False(t, unadmitted.Attempts[0].RunAdmitted, "planned Run ID must not become an execution link")
	require.Len(t, f.model.requests(), 1)
	var count int
	require.NoError(t, f.rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestFlow_HTTPRealControlAndReadOnlyQueries(t *testing.T) {
	f := newFlowHTTPFixture(t)
	// Stop the pool before acceptance so reads and cancel can be proved to issue
	// zero Runs, even though this uses the actual production service and SQL.
	require.NoError(t, f.rt.flows.Close())
	status, _, raw, response := f.request(http.MethodPost, "/api/v1/admin/flows/publish", f.admin, "", "", map[string]any{"operation_id": "publish-control", "expected_version": 0, "definition": liveFlowDefinition(t, f.rt)})
	require.Equal(t, 200, status, response)
	var definition web.FlowDefinitionView
	require.NoError(t, json.Unmarshal(raw, &definition))
	status, _, raw, response = f.request(http.MethodPost, "/api/v1/flow-tasks", f.admin, "start-control", "", map[string]any{"definition_uid": definition.UID, "input": map[string]any{"answer": "input"}})
	require.Equal(t, 202, status, response)
	var task web.FlowTaskView
	require.NoError(t, json.Unmarshal(raw, &task))
	path := "/api/v1/flow-tasks/" + task.ID
	for _, suffix := range []string{"", "/artifacts", "/verifications"} {
		status, _, _, response = f.request(http.MethodGet, path+suffix, f.admin, "", "", nil)
		require.Equal(t, 200, status, response)
	}
	control := map[string]string{"operation_id": "cancel-control", "reason": "manual cancellation"}
	status, _, _, response = f.request(http.MethodPost, path+"/cancel", f.admin, "", "", control)
	require.Equal(t, 412, status, response)
	status, _, raw, response = f.request(http.MethodPost, path+"/cancel", f.admin, "", task.ETag, control)
	require.Equal(t, 200, status, response)
	var cancelled web.FlowTaskView
	require.NoError(t, json.Unmarshal(raw, &cancelled))
	require.Equal(t, flow.Cancelled, cancelled.Status)
	require.True(t, cancelled.CancelRequested)
	status, _, _, response = f.request(http.MethodPost, path+"/cancel", f.admin, "", task.ETag, control)
	require.Equal(t, 200, status, response, "same operation reads back the original cancellation fact")
	control["operation_id"] = "resume-closed"
	status, _, _, response = f.request(http.MethodPost, path+"/resume", f.admin, "", cancelled.ETag, control)
	require.Equal(t, 409, status, response)
	require.Contains(t, response, "FLOW_CLOSED")
	require.Empty(t, f.model.requests())
	var count int
	require.NoError(t, f.rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&count))
	require.Zero(t, count)
}

func TestFlow_HTTPInteractiveReserveRejectsOnlyNewAcceptance(t *testing.T) {
	for _, reserve := range []int{1, 0} {
		t.Run(strconv.Itoa(reserve), func(t *testing.T) {
			f := newFlowHTTPFixture(t, func(c *cli.Config) { c.Flows.InteractiveReservedPerCaller = &reserve })
			require.NoError(t, f.rt.flows.Close())
			status, _, raw, response := f.request(http.MethodPost, "/api/v1/admin/flows/publish", f.admin, "", "", map[string]any{"operation_id": "publish-capacity", "expected_version": 0, "definition": liveFlowDefinition(t, f.rt)})
			require.Equal(t, 200, status, response)
			var definition web.FlowDefinitionView
			require.NoError(t, json.Unmarshal(raw, &definition))
			owner := f.issue("cal_single-slot", true, core.ScopeFlowsRead, core.ScopeFlowsWrite, core.AgentInvokeScopePrefix+"reader")
			require.NoError(t, postgres.NewQuotaStore(f.rt.db).SetLimit(t.Context(), "cal_single-slot", callers.Limit{QPS: 100, Burst: 100, MaxConcurrent: 1, TasksPerPeriod: 5, TokensPerPeriod: 100000, Period: callers.PeriodDay, Timezone: "UTC"}))
			body := map[string]any{"definition_uid": definition.UID, "input": map[string]string{"answer": "input"}}
			status, _, _, response = f.request(http.MethodPost, "/api/v1/flow-tasks", owner, "single-slot", "", body)
			var count int
			require.NoError(t, f.rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_tasks WHERE caller_id=$1`, "cal_single-slot").Scan(&count))
			if reserve == 1 {
				require.Equal(t, 429, status, response)
				require.Contains(t, response, "FLOW_INTERACTIVE_RESERVE")
				require.Zero(t, count)
			} else {
				require.Equal(t, 202, status, response)
				require.Equal(t, 1, count)
			}
			require.Empty(t, f.model.requests())
		})
	}
}

func TestFlow_HTTPResumeIdempotentFactBeforeCurrentVersion(t *testing.T) {
	f := newFlowHTTPFixture(t)
	require.NoError(t, f.rt.flows.Close())
	status, _, raw, response := f.request(http.MethodPost, "/api/v1/admin/flows/publish", f.admin, "", "", map[string]any{"operation_id": "publish-resume", "expected_version": 0, "definition": liveFlowDefinition(t, f.rt)})
	require.Equal(t, 200, status, response)
	var definition web.FlowDefinitionView
	require.NoError(t, json.Unmarshal(raw, &definition))
	status, _, raw, response = f.request(http.MethodPost, "/api/v1/flow-tasks", f.admin, "start-resume", "", map[string]any{"definition_uid": definition.UID, "input": map[string]string{"answer": "input"}})
	require.Equal(t, 202, status, response)
	var task web.FlowTaskView
	require.NoError(t, json.Unmarshal(raw, &task))
	require.NoError(t, f.rt.flows.store.ParkTask(t.Context(), core.LocalAdminPrincipal(), task.ID, task.Version, "manual_reconciliation"))
	path := "/api/v1/flow-tasks/" + task.ID
	status, _, raw, response = f.request(http.MethodGet, path, f.admin, "", "", nil)
	require.Equal(t, 200, status, response)
	require.NoError(t, json.Unmarshal(raw, &task))
	require.Equal(t, flow.Blocked, task.Status)
	control := map[string]string{"operation_id": "resume-original", "reason": "confirmed no external action"}
	var resumed web.FlowTaskView
	for range 2 {
		status, _, raw, response = f.request(http.MethodPost, path+"/resume", f.admin, "", task.ETag, control)
		require.Equal(t, 200, status, response)
		require.NoError(t, json.Unmarshal(raw, &resumed))
		require.Equal(t, flow.Queued, resumed.Status)
		require.Equal(t, task.Version.ExecutionEpoch+1, resumed.Version.ExecutionEpoch)
	}
	var transitions int
	require.NoError(t, f.rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_transitions WHERE task_id=$1 AND operation_id='resume-original' AND phase='recover'`, task.ID).Scan(&transitions))
	require.Equal(t, 1, transitions)
	control["reason"] = "different request"
	status, _, _, response = f.request(http.MethodPost, path+"/resume", f.admin, "", task.ETag, control)
	require.Equal(t, 409, status, response)
	require.Contains(t, response, "FLOW_IDEMPOTENCY_CONFLICT")
	control = map[string]string{"operation_id": "cancel-after-resume", "reason": "close task"}
	status, _, _, response = f.request(http.MethodPost, path+"/cancel", f.admin, "", resumed.ETag, control)
	require.Equal(t, 200, status, response)
	status, _, raw, response = f.request(http.MethodPost, path+"/resume", f.admin, "", task.ETag, map[string]string{"operation_id": "resume-original", "reason": "confirmed no external action"})
	require.Equal(t, 200, status, response)
	var closed web.FlowTaskView
	require.NoError(t, json.Unmarshal(raw, &closed))
	require.Equal(t, flow.Cancelled, closed.Status, "readback must not revive a closed task")
	require.Empty(t, f.model.requests())
}

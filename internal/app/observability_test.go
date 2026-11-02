//go:build integration

package app

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

func newObservationFixture(t *testing.T, cfg ObservabilityConfig) (*Observability, *runHarness) {
	t.Helper()
	h := newRunHarness(t)
	o, err := NewObservability(t.Context(), cfg, Deps{Store: postgres.NewObservabilityStore(h.db), Clock: time.Now, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, o.Close()) })
	return o, h
}

func TestObservability_WiringLifecycle(t *testing.T) {
	o, _ := newObservationFixture(t, cli.DefaultObservabilityConfig())
	require.True(t, o.Enabled)
	require.NotNil(t, o.Projector)
	require.NotNil(t, o.Metrics)
	require.NotNil(t, o.Alerts)
	require.NotNil(t, o.Cleaner)
	require.Nil(t, o.Export)
	select {
	case <-o.done:
		t.Fatal("constructor must not start or finish a worker")
	default:
	}
	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, o.Start(ctx))
	require.Error(t, o.Start(ctx), "second start must not create another worker")
	cancel()
	require.NoError(t, o.Close())
	select {
	case <-o.done:
	default:
		t.Fatal("close must wait for worker exit")
	}
}

func TestObservability_WorkerStopsOnCancel(t *testing.T) {
	entered := make(chan struct{}, 1)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	cfg := cli.DefaultObservabilityConfig()
	cfg.Export.Enabled = true
	cfg.Export.Endpoint = server.URL
	cfg.Export.MetricsInterval = time.Second
	o, _ := newObservationFixture(t, cfg)
	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, o.Start(ctx))
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not reach synthetic sender")
	}
	cancel()
	started := time.Now()
	require.NoError(t, o.Close())
	require.Less(t, time.Since(started), 500*time.Millisecond)
	before := calls.Load()
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, before, calls.Load())
	require.Equal(t, int32(1), calls.Load())
}

func obsAppBusinessSnapshot(t *testing.T, db *postgres.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"runs", "llm_calls", "tool_invocations", "embedding_calls", "task_executions", "scheduled_tasks", "management_operations", "kb_ingest_jobs"} {
		var value string
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM `+table+` x`).Scan(&value))
		out[table] = value
	}
	return out
}

func TestObservability_WorkerDoesNotExecuteBusiness(t *testing.T) {
	cfg := cli.DefaultObservabilityConfig()
	cfg.Alerts.EvaluationInterval = time.Second
	o, h := newObservationFixture(t, cfg)
	_, err := h.db.ExecContext(t.Context(), `INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at) VALUES('must-stay-pending',1,'fake','fake',0,'pending',1)`)
	require.NoError(t, err)
	before := obsAppBusinessSnapshot(t, h.db)
	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, o.Start(ctx))
	require.Eventually(t, func() bool { states, err := o.store.LoadStates(t.Context()); return err == nil && len(states) > 0 }, 2*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, o.Close())
	require.Equal(t, 0, h.provider.calls())
	require.Equal(t, before, obsAppBusinessSnapshot(t, h.db))
}

func TestObservability_NoBusinessStateInContext(t *testing.T) {
	var paths []string
	appEntries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range appEntries {
		if strings.HasPrefix(entry.Name(), "observability") && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			paths = append(paths, entry.Name())
		}
	}
	entries, err := os.ReadDir("../observability")
	require.NoError(t, err)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			paths = append(paths, filepath.Join("../observability", entry.Name()))
		}
	}
	for _, path := range paths {
		tree, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		require.NoError(t, err)
		ast.Inspect(tree, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := selector.X.(*ast.Ident)
			if ok && pkg.Name == "context" {
				require.NotEqual(t, "WithValue", selector.Sel.Name, "observation code must not carry business state through context: "+path)
			}
			return true
		})
	}
}

func TestObservability_DiagnosticWriteFailureDoesNotFailRun(t *testing.T) {
	o, h := newObservationFixture(t, cli.DefaultObservabilityConfig())
	recorder, _, err := newDiagnosticsPortsWithMetrics(cli.DiagnosticsConfig{}, h.db, time.Now, slog.New(slog.DiscardHandler), o.Metrics)
	require.NoError(t, err)
	_, err = h.db.ExecContext(t.Context(), `CREATE FUNCTION obs_reject_runtime_diagnostic() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic diagnostic failure'; END $$;
		CREATE TRIGGER obs_reject_runtime_diagnostic BEFORE INSERT ON diagnostic_events FOR EACH ROW EXECUTE FUNCTION obs_reject_runtime_diagnostic()`)
	require.NoError(t, err)
	client := newDiagnosticProviderClient(&stubProviderClient{resp: core.ChatResponse{Content: "synthetic response"}}, recorder)
	response, err := client.Chat(t.Context(), core.ChatRequest{RunID: "synthetic", Round: 1})
	require.NoError(t, err)
	require.Equal(t, "synthetic response", response.Content)
	snapshot, err := o.Metrics.Snapshot(t.Context(), 5*time.Minute)
	require.NoError(t, err)
	failed := float64(0)
	for _, sample := range snapshot.Samples {
		if sample.Name == "obs_diagnostic_write_failures_total" {
			failed += sample.Value
		}
	}
	require.Greater(t, failed, float64(0))
}

func TestObservability_AuditFailClosedUnchanged(t *testing.T) {
	o, h := newObservationFixture(t, cli.DefaultObservabilityConfig())
	h.db.ObserveAuditWriteFailures(func() { o.Metrics.IncCounter("obs_audit_write_failures_total", nil) })
	_, err := h.db.ExecContext(t.Context(), `CREATE FUNCTION obs_reject_runtime_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit failure'; END $$;
		CREATE TRIGGER obs_reject_runtime_audit BEFORE INSERT ON llm_calls FOR EACH ROW EXECUTE FUNCTION obs_reject_runtime_audit()`)
	require.NoError(t, err)
	result, err := h.invoker.Run(t.Context(), h.requestFor(runHarnessCaller))
	require.NoError(t, err)
	require.Equal(t, core.StatusFailed, result.Status)
	require.Zero(t, h.provider.calls())
	snapshot, err := o.Metrics.Snapshot(t.Context(), 5*time.Minute)
	require.NoError(t, err)
	for _, sample := range snapshot.Samples {
		if sample.Name == "obs_audit_write_failures_total" {
			require.Equal(t, float64(1), sample.Value)
			return
		}
	}
	t.Fatal("audit failure metric missing")
}

var _ observability.ExportScanner = (*observability.Scanner)(nil)

func TestObservability_ProductionPortsShareCollector(t *testing.T) {
	f := newWireFixture(t)
	rt := f.assemble(t)
	require.NotNil(t, rt.observability)
	require.NotNil(t, rt.observability.Metrics)
	options := f.app().webOptions(rt)
	require.Same(t, rt.observability, options.ObservationLifecycle)
	select {
	case <-rt.observability.done:
		t.Fatal("heavy assembly must not start observation worker")
	default:
	}
	rt.diagRecorder.Record(t.Context(), diagnostics.DiagnosticEvent{Kind: diagnostics.KindQuotaDecision, Correlation: diagnostics.Correlation{RequestID: "shared-observation"}, Attrs: map[string]any{"class": "run"}})
	_, err := rt.db.ExecContext(t.Context(), `CREATE FUNCTION obs_reject_shared_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic shared audit failure'; END $$;
		CREATE TRIGGER obs_reject_shared_audit BEFORE INSERT ON llm_calls FOR EACH ROW EXECUTE FUNCTION obs_reject_shared_audit()`)
	require.NoError(t, err)
	_, err = rt.llmCalls.BeginCall(t.Context(), core.LLMCall{RunID: "shared-observation", Round: 1, Provider: "fake", Model: "fake", CreatedAt: time.Now()})
	require.Error(t, err)
	snapshot, err := rt.observability.Metrics.Snapshot(t.Context(), 5*time.Minute)
	require.NoError(t, err)
	diagnostic, audit := float64(0), float64(0)
	for _, sample := range snapshot.Samples {
		if sample.Name == "obs_diagnostic_events_total" && sample.Labels["kind"] == "quota_decision" && sample.Labels["result"] == "recorded" {
			diagnostic = sample.Value
		}
		if sample.Name == "obs_audit_write_failures_total" {
			audit = sample.Value
		}
	}
	require.Equal(t, float64(1), diagnostic)
	require.Equal(t, float64(1), audit)
}

func TestObservability_ConfigurationFrozen(t *testing.T) {
	cfg := cli.DefaultObservabilityConfig()
	o, _ := newObservationFixture(t, cfg)
	*cfg.Enabled = false
	*cfg.Alerts.Enabled = false
	require.True(t, o.Enabled)
	require.True(t, *o.cfg.Enabled)
	require.True(t, *o.cfg.Alerts.Enabled, "caller mutation must not hot-reload the constructed observation runtime")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, o.Start(ctx))
	require.Eventually(t, func() bool { states, err := o.store.LoadStates(t.Context()); return err == nil && len(states) > 0 }, 2*time.Second, 10*time.Millisecond)
}

func TestObservability_ClockMatchesWindow(t *testing.T) {
	db := openAuthorDB(t)
	at := time.UnixMilli(1700000000000).UTC()
	require.NoError(t, postgres.NewRunStore(db).StartRun(t.Context(), core.RunRecord{RunID: "clock-window", Agent: "synthetic", Channel: "cli", StartedAt: at.Add(-time.Minute), Principal: core.LocalAdminCLIPrincipal()}, core.RunResourceManifest{}))
	o, err := NewObservability(t.Context(), cli.DefaultObservabilityConfig(), Deps{Store: postgres.NewObservabilityStore(db), Clock: func() time.Time { return at }, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	defer o.Close()
	snapshot, err := o.Metrics.Snapshot(t.Context(), 5*time.Minute)
	require.NoError(t, err)
	require.Equal(t, at, snapshot.CollectedAt)
	for _, sample := range snapshot.Samples {
		if sample.Name == "obs_runs_total" && sample.Labels["status"] == "running" && sample.Labels["trigger"] == "user" {
			require.True(t, sample.Available)
			require.Equal(t, float64(1), sample.Value)
			return
		}
	}
	t.Fatal("fixed clock window must include the real run recorded inside that window")
}

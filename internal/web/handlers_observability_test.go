package web

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/observability"
)

type fakeObservationReader struct {
	enabled  bool
	calls    int
	err      error
	timeline observability.Timeline
	query    observability.TimelineQuery
}

func (f *fakeObservationReader) ObservationEnabled() bool { return f.enabled }
func (f *fakeObservationReader) Overview(context.Context, observability.OverviewQuery) (observability.Overview, error) {
	f.calls++
	return observability.Overview{Groups: []observability.TaskGroup{}}, f.err
}

func (f *fakeObservationReader) Timeline(_ context.Context, q observability.TimelineQuery) (observability.Timeline, error) {
	f.calls++
	f.query = q
	return f.timeline, f.err
}

func (f *fakeObservationReader) Context(context.Context, string) (observability.ContextDiagnosis, error) {
	f.calls++
	return observability.ContextDiagnosis{RunID: "run-real"}, f.err
}

func (f *fakeObservationReader) MetricsSnapshot(context.Context, time.Duration) (observability.MetricsSnapshot, error) {
	f.calls++
	return observability.MetricsSnapshot{}, f.err
}

func (f *fakeObservationReader) AlertStatus(context.Context, observability.ReadWindow) (observability.AlertStatus, error) {
	f.calls++
	return observability.AlertStatus{}, f.err
}

func (f *fakeObservationReader) ExportStatus(context.Context) (observability.ExportStatus, error) {
	f.calls++
	return observability.ExportStatus{Sources: []observability.SourceCoverage{}}, f.err
}

func (f *fakeObservationReader) CleanupStatus(context.Context, observability.ReadWindow) (observability.CleanupStatus, error) {
	f.calls++
	return observability.CleanupStatus{}, f.err
}

func newObservationServer(t *testing.T) (*Server, *fakeObservationReader) {
	t.Helper()
	f := &fakeObservationReader{enabled: true, timeline: observability.Timeline{Entries: []observability.TimelineEntry{}, Gaps: []observability.EvidenceGap{}}}
	return newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken); o.Observability = f }), f
}

func TestObservability_APIHistoryPresets(t *testing.T) {
	s, _ := newObservationServer(t)
	end := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	for _, days := range []int{1, 7, 30} {
		for _, endpoint := range []string{"overview", "alerts", "cleanup"} {
			path := "/api/v1/observability/" + endpoint + "?from=" + end.Add(-time.Duration(days)*24*time.Hour).Format(time.RFC3339) + "&to=" + end.Format(time.RFC3339)
			rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, path, ""), testToken))
			require.Equal(t, http.StatusOK, rec.Code, path)
		}
	}
}

func TestObservability_APIAnchorRequired(t *testing.T) {
	s, f := newObservationServer(t)
	for _, path := range []string{"timeline", "timeline?run_id=a&session_id=b", "timeline?request_id=bad%20value", "timeline?run_id=a&limit=1001", "timeline?run_id=a&limit=0", "timeline?run_id=a&limit=1&limit=2", "timeline?run_id=a&sources=metric_snapshot", "context", "context?run_id=a&token=CANARY_QUERY", "overview?from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z", "metrics?window=2m", "export?endpoint=CANARY_QUERY"} {
		rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/observability/"+path, ""), testToken))
		require.Equal(t, 400, rec.Code, path)
		require.NotContains(t, rec.Body.String(), "CANARY")
	}
	require.Zero(t, f.calls, "参数非法不得触达端口")
	for _, pair := range []struct {
		key  string
		kind observability.AnchorKind
	}{{"run_id", observability.AnchorRun}, {"request_id", observability.AnchorRequest}, {"task_id", observability.AnchorTask}, {"operation_id", observability.AnchorOperation}, {"ingest_job_id", observability.AnchorIngest}, {"session_id", observability.AnchorSession}} {
		rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/observability/timeline?"+pair.key+"=opaque-real&limit=1", ""), testToken))
		require.Equal(t, 200, rec.Code)
		require.Equal(t, pair.kind, f.query.Anchor.Kind)
	}
}

func TestObservability_APIEmptyVsError(t *testing.T) {
	s, f := newObservationServer(t)
	for _, path := range []string{"overview", "timeline?run_id=real", "context?run_id=real", "metrics?window=15m", "alerts", "export", "cleanup"} {
		f.err = nil
		rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/observability/"+path, ""), testToken))
		require.Equal(t, 200, rec.Code, path)
		f.err = errors.New("password=CANARY_SQL")
		rec = serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/observability/"+path, ""), testToken))
		require.Equal(t, 500, rec.Code, path)
		require.NotContains(t, rec.Body.String(), "CANARY_SQL")
		require.NotContains(t, rec.Body.String(), `"data"`)
	}
	f.enabled = false
	rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/observability/export", ""), testToken))
	require.Equal(t, 503, rec.Code)
	s.opts.Observability = nil
	rec = serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/observability/export", ""), testToken))
	require.Equal(t, 500, rec.Code)
}

func TestObservability_PermissionCallerKeyRejected(t *testing.T) {
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{"c1-secret": callerPrincipal("c1", core.ScopeSessionsWrite)}}
	guard := businessGuard(t, keys)
	s, _, _ := newBusinessServer(t, guard)
	f := &fakeObservationReader{enabled: true}
	s.opts.Observability = f
	for _, path := range []string{"overview", "timeline?run_id=real", "context?run_id=real", "metrics", "alerts", "export", "cleanup"} {
		rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/observability/"+path, ""), "c1-secret"))
		require.Equal(t, 403, rec.Code, path)
	}
	require.Zero(t, f.calls)
}

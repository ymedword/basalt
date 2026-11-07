//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityStore_MetricWindows(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	for _, run := range []struct {
		id  string
		age time.Duration
	}{{"recent", time.Minute}, {"older", 10 * time.Minute}} {
		start := now.Add(-run.age)
		require.NoError(t, startRunTest(NewRunStore(db), t.Context(), core.RunRecord{RunID: run.id, Agent: "CANARY_AGENT", Channel: "web", StartedAt: start, Principal: core.LocalAdminCLIPrincipal()}, core.RunResourceManifest{}))
		_, err := db.pool.ExecContext(t.Context(), `UPDATE runs SET status='completed',finished_at=started_at+500 WHERE run_id=$1`, run.id)
		require.NoError(t, err)
	}
	_, err := db.pool.ExecContext(t.Context(), `INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at,error_kind)
		VALUES ('recent',1,'synthetic','CANARY_MODEL',500,'failed',$1,'timeout'),('recent',2,'synthetic','CANARY_MODEL',0,'unknown',$1,'unknown')`, now.Add(-time.Minute).UnixMilli())
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `INSERT INTO tool_invocations(run_id,tool_name,status,started_at,finished_at,error_kind)
		VALUES ('recent','CANARY_TOOL','failed',$1::bigint,$1::bigint+1,'sandbox_denied')`, now.Add(-time.Minute).UnixMilli())
	require.NoError(t, err)
	recorder := diagnostics.NewRecorder(diagnostics.RecorderOptions{Store: NewDiagnosticStore(db), Now: func() time.Time { return now }})
	recorder.Record(t.Context(), diagnostics.DiagnosticEvent{Kind: diagnostics.KindRetrieval, Correlation: diagnostics.Correlation{RunID: "recent"}, ReasonCode: diagnostics.ReasonPermissionDenied, Attrs: map[string]any{"count": 5}})
	recorder.Record(t.Context(), diagnostics.DiagnosticEvent{Kind: diagnostics.KindPermissionDecision, Correlation: diagnostics.Correlation{RunID: "recent"}, ReasonCode: diagnostics.ReasonPermissionDenied})
	s := NewObservabilityStore(db)
	five, err := s.WindowCounts(t.Context(), 5*time.Minute)
	require.NoError(t, err)
	fifteen, err := s.WindowCounts(t.Context(), 15*time.Minute)
	require.NoError(t, err)
	require.Equal(t, float64(1), obsMetricSum(five, "obs_runs_total"))
	require.Equal(t, float64(2), obsMetricSum(fifteen, "obs_runs_total"))
	require.Equal(t, float64(2), obsMetricSum(five, "obs_tool_denials_total"), "retrieval's mixed count must not become tool denials")
	require.Equal(t, float64(1), obsMetricSum(five, "obs_llm_timeout_total"))
	require.Equal(t, float64(1), obsMetricSum(five, "obs_llm_call_duration_ms_count"), "recovered unknown is not a zero-duration observation")
	for _, sample := range five {
		require.Equal(t, observability.MetricGauge, sample.Type)
		require.Equal(t, 5*time.Minute, sample.Window)
		require.NotContains(t, sample.Labels, "agent")
		require.NotContains(t, sample.Labels, "model")
		require.NotContains(t, sample.Labels, "tool")
	}
}

func obsMetricSum(samples []observability.MetricSample, name string) float64 {
	var result float64
	for _, sample := range samples {
		if sample.Name == name && sample.Available {
			result += sample.Value
		}
	}
	return result
}

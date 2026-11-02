package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type metricTestStore struct {
	samples []MetricSample
	err     error
}

type metricAtStore struct{ at time.Time }

func (*metricAtStore) WindowCounts(context.Context, time.Duration) ([]MetricSample, error) {
	return nil, errors.New("implicit clock must not be used")
}

func (s *metricAtStore) WindowCountsAt(_ context.Context, window time.Duration, at time.Time) ([]MetricSample, error) {
	s.at = at
	return []MetricSample{{Name: "obs_tool_denials_total", Type: MetricGauge, Available: true, Value: 3, Window: window}}, nil
}

func TestObservability_MetricWindowUsesCollectedAt(t *testing.T) {
	start := time.UnixMilli(1700000000000).UTC()
	ticks := 0
	store := &metricAtStore{}
	collector := NewMetricsCollector(store, func() time.Time { at := start.Add(time.Duration(ticks) * time.Second); ticks++; return at })
	snapshot, err := collector.Snapshot(t.Context(), time.Hour)
	require.NoError(t, err)
	require.Equal(t, start.Add(time.Second), snapshot.CollectedAt)
	require.Equal(t, snapshot.CollectedAt, store.at)
	require.Equal(t, 2, ticks)
	value := metricFind(t, snapshot, "obs_tool_denials_total", ScopeDatabase)
	require.True(t, value.Available)
	require.Equal(t, float64(3), value.Value)
}

func (s *metricTestStore) WindowCounts(context.Context, time.Duration) ([]MetricSample, error) {
	return s.samples, s.err
}

func metricFind(t *testing.T, s MetricsSnapshot, name string, scope MetricScope) MetricSample {
	t.Helper()
	for _, sample := range s.Samples {
		if sample.Name == name && sample.Scope == scope {
			return sample
		}
	}
	t.Fatalf("missing metric %s/%s", name, scope)
	return MetricSample{}
}

func TestObservability_MetricLabelsBounded(t *testing.T) {
	c := NewMetricsCollector(&metricTestStore{samples: []MetricSample{{Name: "obs_runs_total", Type: MetricGauge, Value: 9, Available: true, Labels: map[string]string{"agent": "CANARY_AGENT"}}}}, nil)
	initial := len(c.counters)
	for i := range 100 {
		c.IncCounter("obs_audit_write_failures_total", map[string]string{"run_id": fmt.Sprintf("CANARY_%d", i)})
		c.IncCounter("CANARY_METRIC", nil)
	}
	c.IncCounter("obs_diagnostic_events_total", map[string]string{"kind": "retrieval", "result": "recorded"})
	got, err := c.Snapshot(t.Context(), 15*time.Minute)
	require.NoError(t, err)
	require.Len(t, c.counters, initial)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "CANARY")
	for _, sample := range got.Samples {
		for key := range sample.Labels {
			require.NotContains(t, []string{"agent", "run_id", "session_id", "request_id", "tool", "model", "path", "chunk_id"}, key)
		}
	}
}

func TestObservability_MetricRestartSemantics(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	store := &metricTestStore{samples: []MetricSample{{Name: "obs_llm_timeout_total", Type: MetricGauge, Value: 2, Available: true, Labels: map[string]string{}}}}
	clock := func() time.Time { return now }
	first := NewMetricsCollector(store, clock)
	first.IncCounter("obs_audit_write_failures_total", nil)
	a, err := first.Snapshot(t.Context(), 15*time.Minute)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	second := NewMetricsCollector(store, clock)
	b, err := second.Snapshot(t.Context(), 15*time.Minute)
	require.NoError(t, err)
	require.Equal(t, float64(1), metricFind(t, a, "obs_audit_write_failures_total", ScopeProcess).Value)
	require.Zero(t, metricFind(t, b, "obs_audit_write_failures_total", ScopeProcess).Value)
	require.Equal(t, metricFind(t, a, "obs_llm_timeout_total", ScopeDatabase).Value, metricFind(t, b, "obs_llm_timeout_total", ScopeDatabase).Value)
	require.NotEqual(t, a.startedAt, b.startedAt)
	raw, err := json.Marshal(a)
	require.NoError(t, err)
	var replay MetricsSnapshot
	require.NoError(t, json.Unmarshal(raw, &replay))
	require.Equal(t, a.startedAt, replay.startedAt)
}

func TestObservability_MetricWindowGaugeNotDelta(t *testing.T) {
	store := &metricTestStore{samples: []MetricSample{{Name: "obs_run_duration_ms_bucket", Type: MetricGauge, Value: 3, Available: true, Labels: map[string]string{"status": "completed", "le": "1000"}}}}
	c := NewMetricsCollector(store, nil)
	c.ObserveExportDuration(25*time.Millisecond, "partial")
	got, err := c.Snapshot(t.Context(), 15*time.Minute)
	require.NoError(t, err)
	sample := metricFind(t, got, "obs_run_duration_ms_bucket", ScopeDatabase)
	require.Equal(t, MetricGauge, sample.Type)
	require.Equal(t, "1", sample.Unit, "window bucket values count observations, not milliseconds")
	require.Equal(t, 15*time.Minute, sample.Window)
	require.True(t, sample.Available)
	hist := metricFind(t, got, "obs_export_batch_duration_ms_count", ScopeProcess)
	require.Equal(t, MetricHistogram, hist.Type)
	require.Equal(t, float64(1), hist.Value)
	var partial bool
	for _, sample := range got.Samples {
		if sample.Name == hist.Name && sample.Labels["result"] == "partial" {
			partial = true
			require.Equal(t, float64(1), sample.Value)
		}
	}
	require.True(t, partial)
}

func TestObservability_MetricUnavailableNotZero(t *testing.T) {
	c := NewMetricsCollector(&metricTestStore{err: errors.New("CANARY_DB_FAILURE")}, nil)
	got, err := c.Snapshot(t.Context(), 5*time.Minute)
	require.NoError(t, err)
	sample := metricFind(t, got, "obs_runs_total", ScopeDatabase)
	require.False(t, sample.Available)
	require.Equal(t, "query_failed", sample.Reason)
	raw, err := json.Marshal(sample)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.Nil(t, fields["value"])
	require.NotContains(t, string(raw), "CANARY")
	_, err = c.Snapshot(t.Context(), 30*time.Minute)
	require.ErrorIs(t, err, ErrInvalidQuery)
}

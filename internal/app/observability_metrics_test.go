package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityProducer_MetricCallbackPanicIsolated(t *testing.T) {
	r := diagnostics.NewRecorder(diagnostics.RecorderOptions{Store: &obsMetricEventStore{}, OnResult: func(diagnostics.Kind, string) { panic("CANARY_CALLBACK") }, Logger: slog.New(slog.DiscardHandler)})
	client := newDiagnosticProviderClient(&stubProviderClient{resp: core.ChatResponse{Content: "synthetic response"}}, r)
	require.NotPanics(t, func() {
		response, err := client.Chat(t.Context(), core.ChatRequest{RunID: "synthetic", Round: 1})
		require.NoError(t, err)
		require.Equal(t, "synthetic response", response.Content)
	})
}

type obsMetricEventStore struct{ err error }

func (s *obsMetricEventStore) InsertEvent(context.Context, diagnostics.DiagnosticEvent) error {
	return s.err
}

func TestObservabilityProducer_DiagnosticMetricOutcomes(t *testing.T) {
	c := observability.NewMetricsCollector(nil, nil)
	for _, result := range []string{"recorded", "sampled_out", "write_failed"} {
		store := &obsMetricEventStore{}
		cfg := diagnostics.DefaultConfig()
		if result == "sampled_out" {
			var err error
			cfg, err = diagnostics.NewConfig(0, 0)
			require.NoError(t, err)
		}
		if result == "write_failed" {
			store.err = errors.New("synthetic write error")
		}
		r := diagnostics.NewRecorder(diagnostics.RecorderOptions{Store: store, Sampler: diagnostics.NewRateSampler(cfg), OnResult: diagnosticMetricResult(c), Logger: slog.New(slog.DiscardHandler)})
		r.Record(t.Context(), diagnostics.DiagnosticEvent{Kind: diagnostics.KindRetrieval, Correlation: diagnostics.Correlation{RunID: "synthetic"}})
	}
	snapshot, err := c.Snapshot(t.Context(), 5*time.Minute)
	require.NoError(t, err)
	results := map[string]float64{}
	for _, sample := range snapshot.Samples {
		if sample.Name == "obs_diagnostic_events_total" && sample.Labels["kind"] == "retrieval" {
			results[sample.Labels["result"]] = sample.Value
		}
	}
	require.Equal(t, map[string]float64{"recorded": 1, "sampled_out": 1, "write_failed": 1}, results)
	var failed float64
	for _, sample := range snapshot.Samples {
		if sample.Name == "obs_diagnostic_write_failures_total" && sample.Labels["kind"] == "retrieval" {
			failed = sample.Value
		}
	}
	require.Equal(t, float64(1), failed)
}

package observability

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type metricExportLedger struct {
	*workerLedger
	err       error
	submitted []ExportItem
}

func (l *metricExportLedger) Enqueue(ctx context.Context, items []ExportItem) error {
	l.submitted = append(l.submitted, items...)
	if l.err != nil {
		return l.err
	}
	return l.workerLedger.Enqueue(ctx, items)
}

func metricExportFixture(now time.Time, value float64) (*MetricsCollector, *metricExportLedger) {
	c := NewMetricsCollector(&metricTestStore{samples: []MetricSample{{Name: "obs_tool_denials_total", Type: MetricGauge, Value: value, Available: true}}}, func() time.Time { return now })
	l := &metricExportLedger{workerLedger: &workerLedger{items: map[string]ExportItem{}, batches: map[string]ExportDisposition{}, now: now}}
	return c, l
}

func TestObservability_MetricSnapshotEnqueued(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	c, l := metricExportFixture(now, 3)
	exporter := NewMetricsExporter(c, l, time.Minute, func() time.Time { return now })
	require.NoError(t, exporter.EnqueueOnce(t.Context()))
	require.Len(t, l.items, 1)
	for _, item := range l.items {
		require.Equal(t, SourceMetricSnapshot, item.SourceKind)
		require.Equal(t, SignalMetric, item.Signal)
		require.NoError(t, item.Validate())
		var snapshot MetricsSnapshot
		require.NoError(t, json.Unmarshal(item.Payload, &snapshot))
		require.Equal(t, now, snapshot.CollectedAt)
		require.Equal(t, now, snapshot.startedAt)
		hash, err := CanonicalStateHash(snapshot)
		require.NoError(t, err)
		require.Equal(t, item.StateHash, hash)
	}
}

func TestObservability_MetricDuplicateSnapshotIdempotent(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	c, l := metricExportFixture(now, 3)
	for range 2 {
		exporter := NewMetricsExporter(c, l, time.Minute, func() time.Time { return now })
		require.NoError(t, exporter.EnqueueOnce(t.Context()))
		require.NoError(t, exporter.EnqueueOnce(t.Context()))
	}
	require.Len(t, l.items, 1, "same snapshot identity and hash must deduplicate even with a new exporter")
}

func TestObservability_MetricEnqueueRetryKeepsOriginal(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	c, l := metricExportFixture(now, 3)
	exporter := NewMetricsExporter(c, l, time.Minute, func() time.Time { return now })
	l.err = errors.New("synthetic enqueue failure")
	require.Error(t, exporter.EnqueueOnce(t.Context()))
	require.Len(t, l.submitted, 1)
	c.IncCounter("obs_audit_write_failures_total", nil)
	now = now.Add(time.Minute)
	l.err = nil
	require.NoError(t, exporter.EnqueueOnce(t.Context()))
	require.Len(t, l.submitted, 2)
	require.Equal(t, l.submitted[0].Payload, l.submitted[1].Payload)
	require.Equal(t, l.submitted[0].StateHash, l.submitted[1].StateHash)
}

func TestObservability_MetricSnapshotPartialSuccess(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	c, l := metricExportFixture(now, 3)
	require.NoError(t, NewMetricsExporter(c, l, time.Minute, func() time.Time { return now }).EnqueueOnce(t.Context()))
	scanner := &workerScanner{ledger: l.workerLedger, facts: map[string]ExportItem{}}
	for _, item := range l.items {
		var snapshot MetricsSnapshot
		require.NoError(t, json.Unmarshal(item.Payload, &snapshot))
		item.Fact = snapshot
		scanner.facts[item.ItemID] = item
	}
	sender := &workerSender{before: func(b ExportBatch) { require.Greater(t, b.RecordCount, 1) }, result: nil}
	sender.before = func(b ExportBatch) {
		sender.result = &SendResult{Accepted: b.RecordCount - 1, Rejected: 1, PartialSuccess: true}
	}
	worker := NewExportWorker(scanner, l, sender, DefaultExportConfig(), func() time.Time { return now })
	result, err := worker.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, result.Unconfirmed)
	require.Zero(t, result.Sent)
	for _, item := range l.items {
		require.Equal(t, ExportUnconfirmed, item.State)
	}
	_, err = worker.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Len(t, sender.batches, 1)
}

func TestObservability_MetricOverlappingWindowsNotSummed(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	backend := map[string]float64{}
	for i, value := range []float64{3, 5, 5} {
		at := now.Add(time.Minute)
		if i == 0 {
			at = now
		}
		c, _ := metricExportFixture(at, value)
		snapshot, err := c.Snapshot(t.Context(), 15*time.Minute)
		require.NoError(t, err)
		points, err := mapSnapshot(snapshot)
		require.NoError(t, err)
		for _, p := range points {
			if p.Name == "basalt.obs.window.obs_tool_denials_total" {
				require.Equal(t, MetricGauge, p.Type)
				require.True(t, p.StartTime.IsZero())
				backend[p.Name+"/"+p.Time.Format(time.RFC3339Nano)] = p.Value
			}
		}
	}
	require.Len(t, backend, 2)
	require.Equal(t, float64(3), backend["basalt.obs.window.obs_tool_denials_total/"+now.Format(time.RFC3339Nano)])
	require.Equal(t, float64(5), backend["basalt.obs.window.obs_tool_denials_total/"+now.Add(time.Minute).Format(time.RFC3339Nano)])
}

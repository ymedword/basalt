//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

type obsWorkerSender struct {
	batches []observability.ExportBatch
	send    func(observability.ExportBatch) (observability.SendResult, error)
}

func (s *obsWorkerSender) Send(_ context.Context, b observability.ExportBatch) (observability.SendResult, error) {
	s.batches = append(s.batches, b)
	if s.send != nil {
		return s.send(b)
	}
	return observability.SendResult{Accepted: b.RecordCount}, nil
}

func obsBusinessSnapshot(t *testing.T, db *DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"runs", "llm_calls", "tool_invocations", "embedding_calls", "task_executions", "scheduled_tasks", "management_operations", "kb_ingest_jobs"} {
		var rows string
		require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM `+table+` x`).Scan(&rows))
		out[table] = rows
	}
	return out
}

func TestObservabilityStore_WorkerCrashAckWindow(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	for _, id := range []string{"a", "b", "c"} {
		obsSeedScanRun(t, db, id, now.Add(-time.Minute), now)
	}
	_, err := db.pool.ExecContext(t.Context(), `INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at) VALUES('untouched-pending',1,'fake','fake',0,'pending',1)`)
	require.NoError(t, err)
	before := obsBusinessSnapshot(t, db)
	s := NewObservabilityStore(db)
	sender := &obsWorkerSender{send: func(batch observability.ExportBatch) (observability.SendResult, error) {
		var outcome string
		var accepted, rejected sql.NullInt64
		require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT outcome,accepted_records,rejected_records FROM observability_export_batches WHERE batch_id=$1`, batch.BatchID).Scan(&outcome, &accepted, &rejected))
		require.Equal(t, "unknown", outcome)
		require.False(t, accepted.Valid)
		require.False(t, rejected.Valid)
		return observability.SendResult{Accepted: 2, Rejected: 1, PartialSuccess: true}, nil
	}}
	worker := observability.NewExportWorker(observability.NewScanner(s, obsScanConfig()), s, sender, observability.DefaultExportConfig(), func() time.Time { return now })
	_, err = db.pool.ExecContext(t.Context(), `CREATE FUNCTION obs_reject_ack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic acknowledgment failure'; END $$;
		CREATE TRIGGER obs_reject_ack BEFORE UPDATE ON observability_export_batches FOR EACH ROW WHEN(NEW.outcome<>'unknown') EXECUTE FUNCTION obs_reject_ack()`)
	require.NoError(t, err)
	result, err := worker.ExportOnce(t.Context())
	require.Error(t, err)
	require.Zero(t, result.Unconfirmed)
	require.Len(t, sender.batches, 1)
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_items WHERE state='inflight'`).Scan(&count))
	require.Equal(t, 3, count)
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_batches WHERE outcome='unknown' AND accepted_records IS NULL AND rejected_records IS NULL`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = db.pool.ExecContext(t.Context(), `DROP TRIGGER obs_reject_ack ON observability_export_batches`)
	require.NoError(t, err)
	_, err = worker.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Len(t, sender.batches, 1)
	now = now.Add(6 * time.Minute)
	result, err = worker.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 3, result.Unconfirmed)
	require.Len(t, sender.batches, 2)
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_batches WHERE outcome='unknown' AND accepted_records IS NULL AND rejected_records IS NULL`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_batches WHERE outcome='partial_unconfirmed' AND accepted_records=2 AND rejected_records=1`).Scan(&count))
	require.Equal(t, 1, count)
	now = now.Add(time.Hour)
	_, err = worker.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Len(t, sender.batches, 2, "persisted partial response stops retry")
	require.Equal(t, before, obsBusinessSnapshot(t, db), "worker must neither recover pending business nor mutate authoritative facts")
}

func TestObservabilityStore_WorkerThreeSignals(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	obsSeedScanRun(t, db, "trace", now.Add(-time.Minute), now)
	id, err := diagnostics.NewEventID(now)
	require.NoError(t, err)
	require.NoError(t, NewDiagnosticStore(db).InsertEvent(t.Context(), diagnostics.DiagnosticEvent{EventID: id, SchemaVersion: 1, Kind: diagnostics.KindPermissionDecision, OccurredAt: now.UnixMilli(), Correlation: diagnostics.Correlation{RunID: "trace"}, Attrs: map[string]any{"count": 1}}))
	s := NewObservabilityStore(db)
	snapshot := observability.MetricsSnapshot{CollectedAt: now, Samples: []observability.MetricSample{{Name: "obs_tool_denials_total", Type: observability.MetricGauge, Scope: observability.ScopeDatabase, Window: time.Hour, Available: true, Value: 2}}}
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	hash, err := observability.CanonicalStateHash(snapshot)
	require.NoError(t, err)
	item := observability.ExportItem{SourceKind: observability.SourceMetricSnapshot, SourceID: "synthetic-snapshot", Signal: observability.SignalMetric, SourceTerminalAt: now, StateHash: hash, Payload: raw}
	require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{item}))
	sender := &obsWorkerSender{}
	worker := observability.NewExportWorker(observability.NewScanner(s, obsScanConfig()), s, sender, observability.DefaultExportConfig(), func() time.Time { return now })
	result, err := worker.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 3, result.Sent)
	require.Equal(t, 3, result.Batches)
	require.Len(t, sender.batches, 3)
	for i, signal := range []observability.SignalKind{observability.SignalTrace, observability.SignalLog, observability.SignalMetric} {
		require.Equal(t, signal, sender.batches[i].Signal)
		require.Equal(t, 1, sender.batches[i].RecordCount)
	}
	metric := sender.batches[2].Payload.(observability.TelemetryPayload).Metrics[0]
	require.Equal(t, float64(2), metric.Value)
	require.True(t, metric.StartTime.IsZero())
	require.Equal(t, observability.MetricGauge, metric.Type)
	result, err = worker.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, result.Claimed)
	require.Len(t, sender.batches, 3)
}

func TestObservabilityStore_MetricSnapshotReplay(t *testing.T) {
	db := portTestDB(t)
	old := time.UnixMilli(1700000000000).UTC()
	now := old
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	collector := observability.NewMetricsCollector(s, func() time.Time { return old })
	collector.IncCounter("obs_audit_write_failures_total", nil)
	require.NoError(t, observability.NewMetricsExporter(collector, s, time.Minute, func() time.Time { return old }).EnqueueOnce(t.Context()))
	now = old.Add(2 * time.Hour)
	newCollector := observability.NewMetricsCollector(s, func() time.Time { return now })
	require.NoError(t, observability.NewMetricsExporter(newCollector, s, time.Minute, func() time.Time { return now }).EnqueueOnce(t.Context()))
	items, err := s.Claim(t.Context(), 10, now, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, items, 2)
	seen := map[int64]bool{}
	for _, item := range items {
		fresh, ok, err := s.Reload(t.Context(), item)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, item.StateHash, fresh.StateHash)
		batch, errs := (&observability.Mapper{}).BuildBatch([]observability.ExportItem{fresh})
		require.Empty(t, errs)
		for _, point := range batch.Payload.(observability.TelemetryPayload).Metrics {
			if point.Name == "basalt.obs.obs_audit_write_failures_total" {
				seen[point.StartTime.UnixMilli()] = true
				if point.StartTime.Equal(old) {
					require.Equal(t, float64(1), point.Value)
					require.Equal(t, old, point.Time)
				} else {
					require.Equal(t, now, point.StartTime)
					require.Zero(t, point.Value)
					require.Equal(t, now, point.Time)
				}
			}
		}
	}
	require.Len(t, seen, 2)
	require.True(t, seen[old.UnixMilli()])
	require.True(t, seen[now.UnixMilli()])
}

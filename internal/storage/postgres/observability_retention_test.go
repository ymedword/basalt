//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

func obsSeedCleanupEvent(t *testing.T, db *DB, id string, at time.Time) {
	t.Helper()
	require.NoError(t, NewDiagnosticStore(db).InsertEvent(t.Context(), diagnostics.DiagnosticEvent{EventID: id, SchemaVersion: 0, Kind: diagnostics.KindPermissionDecision, OccurredAt: at.UnixMilli(), Correlation: diagnostics.Correlation{RunID: "synthetic"}, Attrs: map[string]any{"count": 1}}))
}

func obsCleanupRequest(now time.Time) observability.CleanupRequest {
	return observability.CleanupRequest{Table: "diagnostic_events", Cutoff: now.Add(-time.Hour), Limit: 100}
}

func TestObservabilityStore_CleanupAtomic(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	obsSeedCleanupEvent(t, db, "atomic", now.Add(-2*time.Hour))
	_, err := db.pool.ExecContext(t.Context(), `CREATE FUNCTION obs_reject_cleanup_ledger() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='succeeded' THEN RAISE EXCEPTION 'synthetic cleanup ledger failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER obs_reject_cleanup_ledger BEFORE INSERT ON observability_cleanup_runs FOR EACH ROW EXECUTE FUNCTION obs_reject_cleanup_ledger()`)
	require.NoError(t, err)
	_, err = s.CleanupBatch(t.Context(), obsCleanupRequest(now))
	require.Error(t, err)
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM diagnostic_events WHERE event_id='atomic'`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_cleanup_runs WHERE deleted_count<>0 OR status='succeeded'`).Scan(&count))
	require.Zero(t, count)
	_, err = db.pool.ExecContext(t.Context(), `DROP TRIGGER obs_reject_cleanup_ledger ON observability_cleanup_runs`)
	require.NoError(t, err)
	run, err := s.CleanupBatch(t.Context(), obsCleanupRequest(now))
	require.NoError(t, err)
	require.Equal(t, int64(1), run.Deleted)
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM diagnostic_events`).Scan(&count))
	require.Zero(t, count)
	var deleted int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT sum(deleted_count) FROM observability_cleanup_runs WHERE status='succeeded'`).Scan(&deleted))
	require.Equal(t, 1, deleted)
}

func TestObservabilityStore_CleanupRequiresConfirmedExport(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	at := now.Add(-2 * time.Hour)
	for _, id := range []string{"sent", "no-ledger", "pending", "unconfirmed", "failed", "stale", "dropped", "retry", "old-history", "wrong-hash"} {
		when := at
		if id == "old-history" {
			when = now.Add(-48 * time.Hour)
		}
		obsSeedCleanupEvent(t, db, id, when)
	}
	_, err := observability.NewScanner(s, obsScanConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `DELETE FROM observability_export_items WHERE source_id='no-ledger';
		UPDATE observability_export_items SET state=CASE source_id WHEN 'sent' THEN 'sent' WHEN 'wrong-hash' THEN 'sent' WHEN 'unconfirmed' THEN 'unconfirmed' WHEN 'failed' THEN 'failed_permanent' WHEN 'stale' THEN 'stale' WHEN 'dropped' THEN 'dropped' WHEN 'retry' THEN 'retry' ELSE 'pending' END;
		UPDATE observability_export_items SET state_hash=repeat('0',64) WHERE source_id='wrong-hash'`)
	require.NoError(t, err)
	req := obsCleanupRequest(now)
	req.Exporting = true
	req.RequireConfirmedExport = true
	req.CoveredFrom, req.CoveredThrough, _, err = s.ScanCoverage(t.Context(), "diagnostic_events")
	require.NoError(t, err)
	run, err := s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int64(1), run.Deleted)
	require.Equal(t, int64(9), run.Skipped)
	var exists bool
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM diagnostic_events WHERE event_id='sent')`).Scan(&exists))
	require.False(t, exists)
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM diagnostic_events`).Scan(&count))
	require.Equal(t, 9, count)
	before := obsBusinessSnapshot(t, db)
	_, err = s.CleanupBatch(t.Context(), observability.CleanupRequest{Table: "runs", Cutoff: now, Limit: 100})
	require.Error(t, err)
	require.Equal(t, before, obsBusinessSnapshot(t, db))
}

func TestObservabilityStore_CleanupBeforeCoverage(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	obsSeedCleanupEvent(t, db, "not-scanned", now.Add(-2*time.Hour))
	req := obsCleanupRequest(now)
	req.Exporting, req.RequireConfirmedExport = true, true
	run, err := s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Zero(t, run.Deleted)
	require.Equal(t, "scan_not_covered", run.SkipReason)
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM diagnostic_events`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestObservabilityStore_CleanupSentWithUnresolved(t *testing.T) {
	for _, state := range []string{"pending", "inflight", "retry", "unconfirmed", "failed_permanent", "stale", "dropped"} {
		t.Run(state, func(t *testing.T) {
			db := portTestDB(t)
			now := time.UnixMilli(1700000000000).UTC()
			db.now = func() time.Time { return now }
			s := NewObservabilityStore(db)
			obsSeedCleanupEvent(t, db, "both", now.Add(-2*time.Hour))
			_, err := observability.NewScanner(s, obsScanConfig()).Scan(t.Context(), now)
			require.NoError(t, err)
			_, err = db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state='sent'`)
			require.NoError(t, err)
			_, err = db.pool.ExecContext(t.Context(), `INSERT INTO observability_export_items(item_id,source_kind,source_id,state_hash,signal,source_terminal_at,state,created_at,updated_at)
			VALUES(repeat('a',64),'diagnostic_events','both',repeat('b',64),'log',$1,$2,$3,$3)`, now.Add(-2*time.Hour).UnixMilli(), state, now.UnixMilli())
			require.NoError(t, err)
			req := obsCleanupRequest(now)
			req.Exporting, req.RequireConfirmedExport = true, true
			req.CoveredFrom, req.CoveredThrough, _, err = s.ScanCoverage(t.Context(), "diagnostic_events")
			require.NoError(t, err)
			run, err := s.CleanupBatch(t.Context(), req)
			require.NoError(t, err)
			require.Zero(t, run.Deleted)
			require.Equal(t, int64(1), run.Skipped)
			var count int
			require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM diagnostic_events`).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}

func TestObservabilityStore_CleanupPaginationProtectedRows(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	for i, id := range []string{"pending-a", "pending-b", "sent-c", "sent-d", "sent-e"} {
		obsSeedCleanupEvent(t, db, id, now.Add(-3*time.Hour+time.Duration(i)*time.Minute))
	}
	_, err := observability.NewScanner(s, obsScanConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state='sent' WHERE source_id LIKE 'sent-%'`)
	require.NoError(t, err)
	policy := observability.RetentionPolicy{Table: "diagnostic_events", ExportSource: true, Enabled: true, Retention: time.Hour, BatchSize: 2, MaxBatches: 3}
	result, err := observability.NewRetentionCleaner(s, []observability.RetentionPolicy{policy}, true).CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, int64(3), result.Deleted)
	require.Equal(t, int64(2), result.Skipped, "protected rows are counted once per bounded pass")
	var deleted, skipped int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT sum(deleted_count),sum(skipped_count) FROM observability_cleanup_runs`).Scan(&deleted, &skipped))
	require.Equal(t, 3, deleted)
	require.Equal(t, 2, skipped)
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM diagnostic_events`).Scan(&count))
	require.Equal(t, 2, count)
}

func TestObservabilityStore_CleanupGlobalBacklog(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	obsSeedCleanupEvent(t, db, "sent", now.Add(-2*time.Hour))
	_, err := observability.NewScanner(s, obsScanConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state='sent'`)
	require.NoError(t, err)
	item := obsExportItem(t, "unrelated-run", now)
	require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{item}))
	_, err = db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET created_at=$1 WHERE source_kind='runs'`, now.Add(-31*time.Minute).UnixMilli())
	require.NoError(t, err)
	req := obsCleanupRequest(now)
	req.Exporting, req.RequireConfirmedExport = true, true
	req.CoveredFrom, req.CoveredThrough, _, err = s.ScanCoverage(t.Context(), "diagnostic_events")
	require.NoError(t, err)
	run, err := s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Zero(t, run.Deleted)
	require.Equal(t, "unexported_backlog", run.SkipReason)
	require.Equal(t, int64(1), run.Skipped)
	_, err = db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state='failed_permanent' WHERE source_kind='runs'`)
	require.NoError(t, err)
	run, err = s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int64(1), run.Deleted)
}

func TestObservabilityStore_CleanupAlertRecoveryEvidence(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	start := now.Add(-3 * time.Hour)
	value := float64(1)
	rule := observability.AlertRule{ID: "run_failure_burst", Enabled: true, For: time.Minute, Collect: func(context.Context, time.Time) ([]observability.AlertSample, error) {
		return []observability.AlertSample{{ScopeKey: "recovering", Value: value}}, nil
	}}
	engine := observability.NewAlertEngine(s, []observability.AlertRule{rule}, nil)
	_, err := engine.Evaluate(t.Context(), start)
	require.NoError(t, err)
	_, err = engine.Evaluate(t.Context(), start.Add(time.Minute))
	require.NoError(t, err)
	value = 0
	_, err = engine.Evaluate(t.Context(), start.Add(2*time.Minute))
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `INSERT INTO observability_alert_states(rule_id,scope_key,state,since,last_eval_at,last_value,updated_at) VALUES('legacy','unknown','unknown',$1,$1,0,$1)`, start.Add(2*time.Minute).UnixMilli())
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `INSERT INTO observability_alert_transitions(rule_id,scope_key,from_state,to_state,value,occurred_at,evidence_ref) VALUES
		('legacy','unknown','inactive','pending',1,$2,'old-ref'),('legacy','unknown','pending','firing',1,$3,'old-ref'),('legacy','unknown','firing','unknown',0,$1,'old-ref')`, start.Add(2*time.Minute).UnixMilli(), start.UnixMilli(), start.Add(time.Minute).UnixMilli())
	require.NoError(t, err)
	before, err := s.LoadStates(t.Context())
	require.NoError(t, err)
	req := obsCleanupRequest(now)
	req.Table = "alert_history"
	run, err := s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int64(3), run.Deleted)
	after, err := s.LoadStates(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, start.Add(2*time.Minute), after["run_failure_burst/recovering"].RecoverySince)
	require.Equal(t, observability.StateFiring, after["legacy/unknown"].PreviousState)
	_, err = observability.NewAlertEngine(s, []observability.AlertRule{rule}, nil).Evaluate(t.Context(), start.Add(3*time.Minute))
	require.NoError(t, err)
	after, err = s.LoadStates(t.Context())
	require.NoError(t, err)
	require.Equal(t, observability.StateInactive, after["run_failure_burst/recovering"].State, "cleanup must not restart the recovery hold")
}

func TestObservabilityStore_CleanupOtherGroups(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	obsSeedScanRun(t, db, "synthetic", now.Add(-2*time.Hour), now)
	_, err := db.pool.ExecContext(t.Context(), `UPDATE runs SET status='running',finished_at=NULL WHERE run_id='synthetic'`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `INSERT INTO context_assembly_records(record_id,run_id,step_id,purpose,agent,prompt_version,model_version,estimator_version,created_at)
		VALUES('old-assembly','synthetic','','retrieval','synthetic','','','',$1)`, now.Add(-2*time.Hour).UnixMilli())
	require.NoError(t, err)
	req := obsCleanupRequest(now)
	req.Table = "context_assembly_records"
	run, err := s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Zero(t, run.Deleted, "active-run assembly remains protected")
	_, err = db.pool.ExecContext(t.Context(), `UPDATE runs SET status='completed',finished_at=$1 WHERE run_id='synthetic'`, now.Add(-2*time.Hour).UnixMilli())
	require.NoError(t, err)
	run, err = s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.EqualValues(t, 1, run.Deleted)
	obsSeedCleanupEvent(t, db, "proof-needed", now.Add(-2*time.Hour))
	_, err = observability.NewScanner(s, obsScanConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{obsExportItem(t, "sent-old", now), obsExportItem(t, "pending-old", now)}))
	_, err = db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state=CASE WHEN source_id='pending-old' THEN 'pending' ELSE 'sent' END,updated_at=$1`, now.Add(-2*time.Hour).UnixMilli())
	require.NoError(t, err)
	req.Table = "export_history"
	req.ExportEnabled = true
	run, err = s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int64(2), run.Deleted, "completed run export and sent-old have no protected diagnostic source proof")
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_items WHERE source_id IN ('pending-old','proof-needed')`).Scan(&count))
	require.Equal(t, 2, count)
	req = obsCleanupRequest(now)
	run, err = s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int64(1), run.Deleted, "export-off cleanup may delete source without confirmation")
	req.Table = "export_history"
	req.ExportEnabled = true
	run, err = s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int64(1), run.Deleted, "source removal makes its terminal proof history eligible")
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_items`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestObservabilityStore_AlertMetadataUnixEpoch(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	epoch := time.UnixMilli(0).UTC()
	state := observability.AlertStateView{RuleID: "epoch", ScopeKey: "test", State: observability.StateUnknown, Since: epoch, LastEvalAt: epoch.Add(time.Minute), PreviousState: observability.StateFiring, PreviousSince: epoch, RecoverySince: epoch, Reason: "source_unavailable"}
	require.NoError(t, s.SaveState(t.Context(), state))
	require.NoError(t, s.SaveTransition(t.Context(), observability.AlertStateView{State: observability.StateFiring}, state, state.LastEvalAt))
	states, err := s.LoadStates(t.Context())
	require.NoError(t, err)
	require.Equal(t, epoch, states["epoch/test"].PreviousSince)
	require.Equal(t, epoch, states["epoch/test"].RecoverySince)
}

func TestObservabilityStore_CleanupProgressAcrossRuns(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	for i, id := range []string{"protected-a", "protected-b", "protected-c", "sent-d"} {
		obsSeedCleanupEvent(t, db, id, now.Add(-3*time.Hour+time.Duration(i)*time.Minute))
	}
	_, err := observability.NewScanner(s, obsScanConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state='sent' WHERE source_id='sent-d'`)
	require.NoError(t, err)
	policy := observability.RetentionPolicy{Table: "diagnostic_events", Enabled: true, ExportSource: true, Retention: time.Hour, BatchSize: 2, MaxBatches: 1}
	cleaner := observability.NewRetentionCleaner(s, []observability.RetentionPolicy{policy}, true)
	first, err := cleaner.CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Zero(t, first.Deleted)
	require.Equal(t, int64(2), first.Skipped)
	second, err := cleaner.CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, int64(1), second.Deleted, "protected prefix larger than a whole run must not starve eligible rows forever")
	_, err = db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state='sent'`)
	require.NoError(t, err)
	_, err = cleaner.CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	fourth, err := cleaner.CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, int64(2), fourth.Deleted, "a completed pass wraps to re-evaluate formerly blocked rows")
}

func TestObservabilityStore_CleanupBatchHistory(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	old := now.Add(-2 * time.Hour)
	db.now = func() time.Time { return old }
	s := NewObservabilityStore(db)
	require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{obsExportItem(t, "sent", old), obsExportItem(t, "unknown", old)}))
	items, err := s.Claim(t.Context(), 2, old, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, items, 2)
	confirmed := obsExportBatch("confirmed", items[:1])
	require.NoError(t, s.BeginBatch(t.Context(), confirmed, old))
	require.NoError(t, s.ApplySendResult(t.Context(), confirmed, observability.SendResult{Accepted: 1}, nil, old))
	unknown := obsExportBatch("unknown", items[1:])
	require.NoError(t, s.BeginBatch(t.Context(), unknown, old))
	db.now = func() time.Time { return now }
	req := obsCleanupRequest(now)
	req.Table = "export_history"
	first, err := s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int64(1), first.Deleted)
	second, err := s.CleanupBatch(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, int64(1), second.Deleted)
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_batches`).Scan(&count))
	require.Equal(t, 1, count)
	var outcome string
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT outcome FROM observability_export_batches`).Scan(&outcome))
	require.Equal(t, "unknown", outcome)
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_items WHERE state='inflight'`).Scan(&count))
	require.Equal(t, 1, count)
}

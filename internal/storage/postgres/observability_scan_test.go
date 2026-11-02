//go:build integration

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

func obsScanConfig() observability.ExportConfig {
	cfg := observability.DefaultExportConfig()
	cfg.SafetyLag = 0
	cfg.ScanOverlap = time.Minute
	cfg.ScanPageSize = 2
	return cfg
}

func obsSeedScanRun(t *testing.T, db *DB, id string, start, finish time.Time) {
	t.Helper()
	_, err := db.pool.ExecContext(t.Context(), `INSERT INTO runs(run_id,agent,channel,user_id,trigger,status,started_at,finished_at,caller_id,authorizing_kind,actor)
		VALUES($1,'synthetic','web','synthetic','user','completed',$2,$3,'local-admin','admin-builtin','admin-cli')`, id, start.UnixMilli(), finish.UnixMilli())
	require.NoError(t, err)
}

func TestObservabilityStore_ScanCoverageBounds(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	obsSeedScanRun(t, db, "old", now.Add(-72*time.Hour), now.Add(-48*time.Hour))
	obsSeedScanRun(t, db, "long", now.Add(-72*time.Hour), now.Add(-10*time.Minute))
	s := NewObservabilityStore(db)
	scanner := observability.NewScanner(s, obsScanConfig())
	result, err := scanner.Scan(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 1, result.Enqueued)
	require.Equal(t, now.Add(-24*time.Hour), result.CoveredFrom[observability.SourceRun])
	var id string
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT source_id FROM observability_export_items`).Scan(&id))
	require.Equal(t, "long", id)
	result, err = scanner.Scan(t.Context(), now.Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, result.Enqueued)
	require.Equal(t, now.Add(-24*time.Hour), result.CoveredFrom[observability.SourceRun], "coverage must not expand into excluded history")
}

func TestObservabilityStore_ScanCoverageCAS(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	now := time.UnixMilli(1700000000000).UTC()
	require.NoError(t, s.StartScan(t.Context(), observability.SourceRun, now))
	_, _, ok, err := s.ScanCoverage(t.Context(), string(observability.SourceRun))
	require.NoError(t, err)
	require.False(t, ok)
	advanced, err := s.AdvanceScanCoverage(t.Context(), observability.SourceRun, time.Time{}, time.Time{}, now.Add(-time.Hour), now, now)
	require.NoError(t, err)
	require.True(t, advanced)
	advanced, err = s.AdvanceScanCoverage(t.Context(), observability.SourceRun, time.Time{}, time.Time{}, now.Add(-time.Hour), now.Add(time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, advanced, "a stale initial scanner must not overwrite another scanner")
	from, through, ok, err := s.ScanCoverage(t.Context(), string(observability.SourceRun))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, now.Add(-time.Hour), from)
	require.Equal(t, now, through)
}

func TestObservabilityStore_OutOfOrderCommit(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	now := time.UnixMilli(1700000000000).UTC()
	scanner := observability.NewScanner(s, obsScanConfig())
	_, err := scanner.Scan(t.Context(), now)
	require.NoError(t, err)
	tx, err := db.pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var small, large int64
	require.NoError(t, tx.QueryRowContext(t.Context(), `INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at)
		VALUES('late-small',1,'fake','fake',0,'success',$1) RETURNING id`, now.Add(-time.Hour).UnixMilli()).Scan(&small))
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at)
		VALUES('early-large',1,'fake','fake',0,'success',$1) RETURNING id`, now.Add(-time.Hour).UnixMilli()).Scan(&large))
	require.Less(t, small, large)
	result, err := scanner.Scan(t.Context(), now.Add(10*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, result.Enqueued)
	require.NoError(t, tx.Commit())
	result, err = scanner.Scan(t.Context(), now.Add(20*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, result.Enqueued, "late transaction must be reconciled beyond overlap regardless of ID")
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_items WHERE source_kind='llm_calls'`).Scan(&count))
	require.Equal(t, 2, count)
}

func TestObservabilityStore_ScanSameTimestampPages(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	for _, id := range []string{"10", "2", "1", "a", "A"} {
		obsSeedScanRun(t, db, id, now.Add(-time.Minute), now)
	}
	result, err := observability.NewScanner(NewObservabilityStore(db), obsScanConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 5, result.Enqueued)
	require.Equal(t, 5, result.Scanned)
}

func TestObservabilityStore_ScanPageFailureKeepsCoverage(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	for _, id := range []string{"a", "b", "c"} {
		obsSeedScanRun(t, db, id, now.Add(-time.Minute), now)
	}
	_, err := db.pool.ExecContext(t.Context(), `CREATE FUNCTION obs_reject_page() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source_id='c' THEN RAISE EXCEPTION 'synthetic page failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER obs_reject_page BEFORE INSERT ON observability_export_items FOR EACH ROW EXECUTE FUNCTION obs_reject_page()`)
	require.NoError(t, err)
	s := NewObservabilityStore(db)
	scanner := observability.NewScanner(s, obsScanConfig())
	_, err = scanner.Scan(t.Context(), now)
	require.Error(t, err)
	_, _, ok, err := s.ScanCoverage(t.Context(), string(observability.SourceRun))
	require.NoError(t, err)
	require.False(t, ok)
	count, err := s.PendingCount(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(2), count, "first page remains committed")
	_, err = db.pool.ExecContext(t.Context(), `DROP TRIGGER obs_reject_page ON observability_export_items`)
	require.NoError(t, err)
	result, err := scanner.Scan(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 1, result.Enqueued)
	require.Equal(t, now, result.CoveredThrough[observability.SourceRun])
}

func TestObservabilityStore_ScanRecoveryUnknown(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	_, err := db.pool.ExecContext(t.Context(), `INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at) VALUES
		('recovered',1,'fake','fake',0,'pending',$1),('legacy-outside',1,'fake','fake',0,'unknown',$1),('legacy-inside',1,'fake','fake',0,'unknown',$2),('normal-zero',1,'fake','fake',0,'success',$2)`, now.Add(-72*time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli())
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `INSERT INTO tool_invocations(run_id,tool_name,status,started_at) VALUES('recovered','fake','pending',$1)`, now.Add(-72*time.Hour).UnixMilli())
	require.NoError(t, err)
	_, err = normalizePendingAuditRows(t.Context(), db.pool, now)
	require.NoError(t, err)
	s := NewObservabilityStore(db)
	result, err := observability.NewScanner(s, obsScanConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 4, result.Enqueued)
	items, err := s.Claim(t.Context(), 10, now, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, items, 4)
	unknown := 0
	for _, item := range items {
		fresh, exists, err := s.Reload(t.Context(), item)
		require.NoError(t, err)
		require.True(t, exists)
		require.Equal(t, item.StateHash, fresh.StateHash)
		batch, errs := (&observability.Mapper{}).BuildBatch([]observability.ExportItem{fresh})
		require.Empty(t, errs)
		payload := batch.Payload.(observability.TelemetryPayload)
		if fresh.Signal == observability.SignalLog {
			require.Len(t, payload.Logs, 1)
			require.Empty(t, payload.Spans)
		} else {
			require.Len(t, payload.Spans, 1)
		}
		switch f := fresh.Fact.(type) {
		case observability.LLMCallFact:
			if f.Status == "unknown" {
				unknown++
				require.Equal(t, observability.SignalLog, fresh.Signal)
				require.Nil(t, f.DurationMS)
			} else {
				require.Equal(t, observability.SignalTrace, fresh.Signal)
				require.NotNil(t, f.DurationMS)
				require.Zero(t, *f.DurationMS)
			}
		case observability.ToolInvocationFact:
			unknown++
			require.Equal(t, observability.SignalLog, fresh.Signal)
			require.Nil(t, f.FinishedAt)
			require.Equal(t, now, *f.RecoveredAt)
		default:
			t.Fatalf("unexpected fact: %T", fresh.Fact)
		}
	}
	require.Equal(t, 3, unknown)
	var finish sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT finished_at FROM tool_invocations WHERE run_id='recovered'`).Scan(&finish))
	require.False(t, finish.Valid)
}

func TestObservabilityStore_ScanRemainingSources(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	job := embSeedJob(t, db, "scan-ingest")
	_, err := db.pool.ExecContext(t.Context(), `UPDATE kb_ingest_jobs SET status='success',finished_at=$1 WHERE ingest_job_id=$2`, now.UnixMilli(), job)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `INSERT INTO embedding_calls(ingest_job_id,model,dimensions,input_count,status,created_at,finished_at) VALUES
		($1,'fake',8,1,'success',$2,$3),($1,'fake',8,1,'pending',$2,NULL)`, job, now.Add(-time.Minute).UnixMilli(), now.UnixMilli())
	require.NoError(t, err)
	taskPortSeedTask(t, db, "scan-task", "synthetic", "0 * * * * *", "CANARY_BODY")
	_, err = db.pool.ExecContext(t.Context(), `INSERT INTO task_executions(task_id,status,trigger,started_at,finished_at) VALUES('scan-task','completed','cron',$1,$2)`, now.Add(-time.Minute).UnixMilli(), now.UnixMilli())
	require.NoError(t, err)
	seedOperation(t, db, "scan-op")
	_, err = db.pool.ExecContext(t.Context(), `UPDATE management_operations SET status='unknown',finished_at=$1,recovered_at=$2 WHERE operation_id='scan-op'`, now.Add(-time.Hour).UnixMilli(), now.UnixMilli())
	require.NoError(t, err)
	id, err := diagnostics.NewEventID(now)
	require.NoError(t, err)
	require.NoError(t, NewDiagnosticStore(db).InsertEvent(t.Context(), diagnostics.DiagnosticEvent{EventID: id, SchemaVersion: 1, Kind: diagnostics.KindPermissionDecision, OccurredAt: now.UnixMilli(), Correlation: diagnostics.Correlation{RunID: "synthetic"}, Attrs: map[string]any{"password": "CANARY_SECRET", "count": 2}}))
	s := NewObservabilityStore(db)
	result, err := observability.NewScanner(s, obsScanConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 5, result.Enqueued, "pending embedding is not terminal")
	items, err := s.Claim(t.Context(), 10, now, 5*time.Minute)
	require.NoError(t, err)
	seen := map[observability.SourceKind]bool{}
	for _, item := range items {
		fresh, exists, err := s.Reload(t.Context(), item)
		require.NoError(t, err)
		require.True(t, exists)
		require.Equal(t, item.StateHash, fresh.StateHash)
		require.Equal(t, now, fresh.SourceTerminalAt)
		seen[item.SourceKind] = true
		if ev, ok := fresh.Fact.(diagnostics.DiagnosticEvent); ok {
			require.NotContains(t, ev.Attrs, "password")
		}
	}
	for _, kind := range []observability.SourceKind{observability.SourceEmbedding, observability.SourceTask, observability.SourceOperation, observability.SourceIngest, observability.SourceDiagnostic} {
		require.True(t, seen[kind], string(kind))
	}
}

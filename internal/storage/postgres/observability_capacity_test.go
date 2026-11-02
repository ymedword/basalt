//go:build integration

package postgres

import (
	"encoding/json"
	"math"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/observability"
)

// TestObservabilityStore_CapacityDrill 仅测独占schema中的合成历史行，不作生产容量承诺。
// TestObservabilityStore_CapacityDrill measures synthetic history in an isolated schema, not production capacity.
func TestObservabilityStore_CapacityDrill(t *testing.T) {
	db := portTestDB(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Millisecond)
	old := time.Date(now.Year(), now.Month(), now.Day()-8, 12, 0, 0, 0, time.UTC)
	const runs, rows = 1000, 20000
	var version string
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT version()`).Scan(&version))
	_, err := db.pool.ExecContext(ctx, `INSERT INTO runs(run_id,agent,channel,user_id,trigger,status,iterations,started_at,finished_at,caller_id,authorizing_kind,actor,request_id)
		SELECT 'capacity-run-'||g,'capacity','cli','synthetic','user','completed',1,$1::bigint-60000,$1::bigint+60000,'local-admin','admin-builtin','admin-cli','capacity-request-'||g FROM generate_series(1,$2)g`, old.UnixMilli(), runs)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at,prompt_tokens,completion_tokens)
		SELECT 'capacity-run-'||g,1,'synthetic','synthetic',12,'success',$1::bigint+10,6,4 FROM generate_series(1,$2)g`, old.UnixMilli(), runs)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO diagnostic_events(event_id,schema_version,kind,occurred_at,run_id,actor,attrs)
		SELECT 'capacity-event-'||g,0,'tool_call',$1::bigint+g,'capacity-run-'||(g%$2+1),'agent','{"count":1}' FROM generate_series(1,$3)g`, old.UnixMilli(), runs, rows)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO context_assembly_records(record_id,run_id,step_id,purpose,agent,prompt_version,model_version,budget,estimator_version,created_at)
		SELECT 'capacity-assembly-'||g,'capacity-run-'||(g%$2+1),'chat:'||g,'chat','capacity','synthetic','synthetic',NULL,'synthetic',$1::bigint+g FROM generate_series(1,$3)g`, old.UnixMilli(), runs, rows)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `ANALYZE runs; ANALYZE llm_calls; ANALYZE diagnostic_events; ANALYZE context_assembly_records`)
	require.NoError(t, err)
	store := NewObservabilityStore(db)
	projector := observability.NewProjector(observability.Ports{Runs: store, Exec: store, Tasks: store, Ops: store, Ingest: store, Assembly: store, Diagnostics: store, Cleanup: store}, observability.ProjectorConfig{SampleRate: 1})
	metrics := observability.NewMetricsCollector(store, func() time.Time { return old.Add(2 * time.Minute) })
	evidenceExpected := true
	probe := func() map[string]any {
		return map[string]any{
			"timeline": capacityLatency(t, 20, func() {
				value, err := projector.Timeline(ctx, observability.TimelineQuery{Anchor: observability.Anchor{Kind: observability.AnchorRun, Value: "capacity-run-1"}, Limit: 1000})
				require.NoError(t, err)
				want := 2
				if evidenceExpected {
					want += 2 * (rows / runs)
				}
				require.Len(t, value.Entries, want)
				for _, gap := range value.Gaps {
					require.NotEqual(t, observability.GapQueryFailed, gap.Reason)
				}
			}),
			"context": capacityLatency(t, 20, func() {
				value, err := projector.Context(ctx, "capacity-run-1")
				require.NoError(t, err)
				want := 0
				if evidenceExpected {
					want = rows / runs
				}
				require.Len(t, value.Rounds, want)
			}),
			"metrics": capacityLatency(t, 10, func() {
				value, err := metrics.Snapshot(ctx, 15*time.Minute)
				require.NoError(t, err)
				var count float64
				for _, sample := range value.Samples {
					if sample.Name == "obs_runs_total" {
						require.True(t, sample.Available)
						count += sample.Value
					}
				}
				require.Equal(t, float64(runs), count, "cannot time a failed or empty-window substitute")
			}),
		}
	}
	report := map[string]any{"collected_at": now, "postgres": version, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "dataset": "synthetic historical-shape rows, compact identifiers, empty assembly bodies; no actual model calls", "runs": runs, "llm_calls": runs, "diagnostic_events": rows, "assembly_records": rows, "rows_per_anchor": rows / runs, "batch_size": 1000, "max_batches_per_table": 20, "statistics_asynchronous": true, "production_growth_rss_and_export_throughput": "not measured"}
	report["before_storage"] = capacityStorage(t, db)
	report["before_latency"] = probe()
	var perDay int
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT count(*) FROM diagnostic_events WHERE occurred_at >= $1 AND occurred_at < $2`, time.Date(old.Year(), old.Month(), old.Day(), 0, 0, 0, 0, time.UTC).UnixMilli(), time.Date(old.Year(), old.Month(), old.Day()+1, 0, 0, 0, 0, time.UTC).UnixMilli()).Scan(&perDay))
	require.Equal(t, rows, perDay)
	report["synthetic_day_utc"] = old.Format("2006-01-02")
	report["diagnostic_rows_on_day"] = perDay
	// EXPLAIN ANALYZE确实执行DELETE，但整笔回滚；正式删除仍走原子清理器。
	// EXPLAIN ANALYZE executes the delete inside a rolled-back transaction; actual deletion uses the atomic cleaner.
	tx, err := db.pool.BeginTx(ctx, nil)
	require.NoError(t, err)
	var plan []byte
	err = tx.QueryRowContext(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) DELETE FROM diagnostic_events WHERE event_id IN
		(SELECT event_id FROM diagnostic_events WHERE occurred_at < $1 ORDER BY occurred_at,event_id LIMIT 1000)`, now.Add(-7*24*time.Hour).UnixMilli()).Scan(&plan)
	rollbackErr := tx.Rollback()
	require.NoError(t, err)
	require.NoError(t, rollbackErr)
	require.True(t, json.Valid(plan))
	report["delete_1000_explain_analyze_rolled_back"] = json.RawMessage(plan)
	policies := observability.DefaultRetentionPolicies()
	for i := range policies {
		if policies[i].Table == "diagnostic_events" || policies[i].Table == "context_assembly_records" {
			policies[i].Enabled = true
			policies[i].Retention = 7 * 24 * time.Hour
		}
	}
	cleaner := observability.NewRetentionCleaner(store, policies, false)
	started := time.Now()
	result, err := cleaner.CleanupOnce(ctx, now)
	elapsed := time.Since(started)
	require.NoError(t, err)
	require.Equal(t, int64(rows*2), result.Deleted)
	require.Zero(t, result.Skipped)
	report["cleanup_ms"] = float64(elapsed.Nanoseconds()) / 1e6
	report["deleted"] = result.Deleted
	var ledgerRows, deleted int
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT count(*),sum(deleted_count) FROM observability_cleanup_runs WHERE status='succeeded'`).Scan(&ledgerRows, &deleted))
	require.Equal(t, 40, ledgerRows)
	require.Equal(t, rows*2, deleted)
	report["committed_cleanup_batches"] = ledgerRows
	report["after_cleanup_storage"] = capacityStorage(t, db)
	evidenceExpected = false
	report["after_latency"] = probe()
	var remaining int
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT count(*) FROM runs`).Scan(&remaining))
	require.Equal(t, runs, remaining, "ephemeral cleanup cannot delete run authority")
	timeline, err := projector.Timeline(ctx, observability.TimelineQuery{Anchor: observability.Anchor{Kind: observability.AnchorRun, Value: "capacity-run-1"}, Limit: 1000})
	require.NoError(t, err)
	for _, source := range []observability.SourceKind{observability.SourceDiagnostic, observability.SourceAssembly} {
		found := false
		for _, gap := range timeline.Gaps {
			if gap.Source == source && gap.Reason == observability.GapRetentionWindowCleaned {
				found = true
			}
		}
		require.True(t, found, "deleted evidence must report a window gap for "+string(source))
	}
	started = time.Now()
	_, err = db.pool.ExecContext(ctx, `VACUUM (ANALYZE) diagnostic_events`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `VACUUM (ANALYZE) context_assembly_records`)
	require.NoError(t, err)
	report["vacuum_ms"] = float64(time.Since(started).Nanoseconds()) / 1e6
	report["after_vacuum_storage"] = capacityStorage(t, db)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	t.Log("OBS_CAPACITY_JSON=" + string(raw))
}

func capacityLatency(t *testing.T, n int, call func()) map[string]any {
	t.Helper()
	values := make([]float64, 0, n)
	for range n {
		start := time.Now()
		call()
		values = append(values, float64(time.Since(start).Nanoseconds())/1e6)
	}
	slices.Sort(values)
	return map[string]any{"samples": n, "p50_ms": values[int(math.Ceil(.5*float64(n)))-1], "p95_ms": values[int(math.Ceil(.95*float64(n)))-1], "min_ms": values[0], "max_ms": values[n-1]}
}

func capacityStorage(t *testing.T, db *DB) map[string]any {
	t.Helper()
	ctx := t.Context()
	var diagnostic, indexDiagnostic, assembly, indexAssembly int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT pg_total_relation_size('diagnostic_events'),pg_indexes_size('diagnostic_events'),pg_total_relation_size('context_assembly_records'),pg_indexes_size('context_assembly_records')`).Scan(&diagnostic, &indexDiagnostic, &assembly, &indexAssembly))
	var deadDiagnostic, deadAssembly int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT n_dead_tup FROM pg_stat_user_tables WHERE relid='diagnostic_events'::regclass`).Scan(&deadDiagnostic))
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT n_dead_tup FROM pg_stat_user_tables WHERE relid='context_assembly_records'::regclass`).Scan(&deadAssembly))
	return map[string]any{"diagnostic_total_bytes": diagnostic, "diagnostic_indexes_bytes": indexDiagnostic, "assembly_total_bytes": assembly, "assembly_indexes_bytes": indexAssembly, "diagnostic_dead_tuples_reported": deadDiagnostic, "assembly_dead_tuples_reported": deadAssembly}
}

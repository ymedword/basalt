package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ymedword/basalt/internal/historyretention"
)

// HistoryRetentionStore owns deletion of complete historical groups only.
// Export-enabled instances conservatively keep authoritative export sources.
type HistoryRetentionStore struct {
	db        *DB
	exporting bool
}

func NewHistoryRetentionStore(db *DB, exporting bool) *HistoryRetentionStore {
	return &HistoryRetentionStore{db: db, exporting: exporting}
}

var historyCandidates = map[string]string{
	"sessions":   `SELECT session_id FROM sessions WHERE last_active_at < $1 AND session_id > $2 ORDER BY session_id LIMIT $3`,
	"runs":       `SELECT run_id FROM runs r WHERE finished_at < $1 AND run_id > $2 AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.session_id=r.session_id) ORDER BY run_id LIMIT $3`,
	"ingest":     `SELECT ingest_job_id FROM kb_ingest_jobs WHERE finished_at < $1 AND ingest_job_id > $2 ORDER BY ingest_job_id LIMIT $3`,
	"operations": `SELECT operation_id FROM management_operations WHERE finished_at < $1 AND operation_id > $2 ORDER BY operation_id LIMIT $3`,
}

func (s *HistoryRetentionStore) CleanupHistoryBatch(ctx context.Context, group string, cutoff time.Time, limit int, after string, gate historyretention.SessionGate) (out historyretention.Report, err error) {
	if !slices.Contains(historyretention.Groups, group) || cutoff.IsZero() || limit < 1 || limit > 1000 {
		return out, errors.New("invalid history retention request")
	}
	id, err := obsCleanupID()
	if err != nil {
		return out, err
	}
	out = historyretention.Report{ID: id, Group: group, Cutoff: cutoff, At: s.db.clock(), Status: "succeeded"}
	var candidates []string
	if query, ok := historyCandidates[group]; ok {
		rows, e := s.db.pool.QueryContext(ctx, query, cutoff.UnixMilli(), after, limit)
		if e != nil {
			return out, e
		}
		for rows.Next() {
			var key string
			if e = rows.Scan(&key); e != nil {
				break
			}
			candidates = append(candidates, key)
		}
		err = errors.Join(e, rows.Err(), rows.Close())
		if err != nil {
			return out, err
		}
	}
	out.Scanned = len(candidates)
	if len(candidates) > 0 {
		out.Next = candidates[len(candidates)-1]
	}
	// Acquire the same session exclusion as Run/Archive before checking DB eligibility.
	if group == "sessions" {
		var available []string
		for _, key := range candidates {
			if gate == nil {
				out.ProtectedGroups++
				continue
			}
			release, ok := gate.TrySessionMaintenance(key)
			if !ok {
				out.ProtectedGroups++
				continue
			}
			defer release()
			available = append(available, key)
		}
		candidates = available
	}
	err = s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, `SET LOCAL lock_timeout='500ms'; SET LOCAL statement_timeout='45s'`); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('basalt:history-retention',0))`); e != nil {
			return e
		}
		if s.exporting && group != "maintenance" {
			out.ProtectedGroups += int64(len(candidates))
			out.Reason = "export_sources_protected"
			return insertHistoryReport(ctx, tx, out)
		}
		for _, key := range candidates {
			var count int64
			var eligible bool
			var e error
			switch group {
			case "sessions", "runs":
				count, eligible, e = s.cleanupRunGroup(ctx, tx, group, key, cutoff.UnixMilli())
			case "ingest":
				count, eligible, e = cleanupIngestGroup(ctx, tx, key, cutoff.UnixMilli())
			case "operations":
				count, eligible, e = cleanupOperation(ctx, tx, key, cutoff.UnixMilli())
			}
			if e != nil {
				return e
			}
			if eligible {
				out.DeletedGroups++
				out.DeletedRows += count
			} else {
				out.ProtectedGroups++
			}
		}
		if group == "maintenance" {
			n, e := cleanupMaintenance(ctx, tx, cutoff.UnixMilli(), limit)
			if e != nil {
				return e
			}
			out.DeletedRows += n
		}
		if out.ProtectedGroups > 0 && out.Reason == "" {
			out.Reason = "active_unresolved_or_referenced"
		}
		return insertHistoryReport(ctx, tx, out)
	})
	if err != nil {
		// Successful counts are never reported for a rolled-back transaction.
		out.DeletedRows = 0
		out.DeletedGroups = 0
		out.Status = "failed"
		out.Reason = "cleanup_failed"
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		err = errors.Join(err, insertHistoryReport(recordCtx, s.db.pool, out))
	}
	return out, err
}

func insertHistoryReport(ctx context.Context, tx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, r historyretention.Report,
) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO history_retention_runs(id,group_kind,cutoff,occurred_at,deleted_groups,deleted_rows,protected_groups,status,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, r.ID, r.Group, r.Cutoff.UnixMilli(), r.At.UnixMilli(), r.DeletedGroups, r.DeletedRows, r.ProtectedGroups, r.Status, r.Reason)
	return err
}

func (s *HistoryRetentionStore) cleanupRunGroup(ctx context.Context, tx *sql.Tx, group, key string, cutoff int64) (int64, bool, error) {
	// The temporary sets define one indivisible ownership group. Never cascade-delete
	// current resources or infer ownership by parsing message/prompt text.
	for _, q := range []string{
		`CREATE TEMP TABLE retention_runs(run_id text PRIMARY KEY) ON COMMIT DROP`,
		`CREATE TEMP TABLE retention_messages(id bigint PRIMARY KEY) ON COMMIT DROP`,
		`CREATE TEMP TABLE retention_compactions(id text PRIMARY KEY) ON COMMIT DROP`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return 0, false, err
		}
	}
	defer func() {
		_, _ = tx.ExecContext(ctx, `DROP TABLE IF EXISTS retention_compactions,retention_messages,retention_runs`)
	}()
	if group == "sessions" {
		var at int64
		err := tx.QueryRowContext(ctx, `SELECT last_active_at FROM sessions WHERE session_id=$1 FOR UPDATE`, key).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, err
		}
		if at >= cutoff {
			return 0, false, nil
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO retention_runs SELECT run_id FROM runs WHERE session_id=$1 FOR UPDATE`, key); err != nil {
			return 0, false, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO retention_messages SELECT id FROM messages WHERE session_id=$1 FOR UPDATE`, key); err != nil {
			return 0, false, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `INSERT INTO retention_runs SELECT run_id FROM runs WHERE run_id=$1 AND NOT EXISTS(SELECT 1 FROM sessions WHERE sessions.session_id=runs.session_id) FOR UPDATE`, key); err != nil {
			return 0, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO retention_compactions SELECT compaction_id FROM compaction_entries WHERE scope_run_id IN(SELECT run_id FROM retention_runs) OR ($1='sessions' AND scope_session_id=$2) FOR UPDATE`, group, key); err != nil {
		return 0, false, err
	}
	var protected bool
	err := tx.QueryRowContext(ctx, runGroupProtectedSQL, cutoff, group, key).Scan(&protected)
	if err != nil {
		return 0, false, err
	}
	if protected {
		return 0, false, nil
	}
	var n int64
	for _, q := range runGroupDeletes {
		res, err := tx.ExecContext(ctx, q)
		if err != nil {
			return 0, false, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return 0, false, err
		}
		n += rows
	}
	if group == "sessions" {
		res, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE session_id=$1`, key)
		if err != nil {
			return 0, false, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return 0, false, err
		}
		n += rows
	}
	return n, true, nil
}

const runGroupProtectedSQL = `SELECT
 EXISTS(SELECT 1 FROM runs WHERE run_id IN(SELECT run_id FROM retention_runs) AND (status NOT IN('completed','failed','cancelled','limit_reached') OR finished_at IS NULL OR finished_at >= $1))
 OR ($2='runs' AND NOT EXISTS(SELECT 1 FROM retention_runs))
 OR EXISTS(SELECT 1 FROM messages WHERE id IN(SELECT id FROM retention_messages) AND created_at >= $1)
 OR EXISTS(SELECT 1 FROM messages WHERE run_id IN(SELECT run_id FROM retention_runs) AND id NOT IN(SELECT id FROM retention_messages))
 OR EXISTS(SELECT 1 FROM llm_calls WHERE (run_id IN(SELECT run_id FROM retention_runs) OR ($2='sessions' AND session_id=$3)) AND (status NOT IN('success','failed') OR created_at >= $1 OR run_id NOT IN(SELECT run_id FROM retention_runs)))
 OR EXISTS(SELECT 1 FROM tool_invocations WHERE (run_id IN(SELECT run_id FROM retention_runs) OR ($2='sessions' AND session_id=$3)) AND (status NOT IN('success','failed') OR finished_at IS NULL OR finished_at >= $1 OR run_id NOT IN(SELECT run_id FROM retention_runs)))
 OR EXISTS(SELECT 1 FROM embedding_calls WHERE run_id IN(SELECT run_id FROM retention_runs) AND (status NOT IN('success','failed') OR finished_at IS NULL OR finished_at >= $1))
 OR EXISTS(SELECT 1 FROM quota_reservations WHERE run_id IN(SELECT run_id FROM retention_runs) AND (state='pending' OR settled_at IS NULL OR settled_at >= $1))
 OR EXISTS(SELECT 1 FROM task_executions WHERE run_id IN(SELECT run_id FROM retention_runs) AND (status NOT IN('completed','failed','skipped') OR finished_at IS NULL OR finished_at >= $1))
 OR EXISTS(SELECT 1 FROM schedule_deliveries WHERE run_id IN(SELECT run_id FROM retention_runs) AND (status NOT IN('sent','failed','skipped') OR finished_at IS NULL OR finished_at >= $1))
 OR EXISTS(SELECT 1 FROM run_resource_refs WHERE run_id IN(SELECT run_id FROM retention_runs) AND (released_at IS NULL OR released_at >= $1))
 OR EXISTS(SELECT 1 FROM run_credential_refs WHERE run_id IN(SELECT run_id FROM retention_runs) AND (released_at IS NULL OR released_at >= $1))
 OR EXISTS(SELECT 1 FROM run_notify_refs WHERE run_id IN(SELECT run_id FROM retention_runs) AND (released_at IS NULL OR released_at >= $1))
 OR EXISTS(SELECT 1 FROM notify_deliveries WHERE run_id IN(SELECT run_id FROM retention_runs) AND resolved_at >= $1)
 OR EXISTS(SELECT 1 FROM context_assembly_records WHERE llm_call_id IN(SELECT id FROM llm_calls WHERE run_id IN(SELECT run_id FROM retention_runs)) AND run_id NOT IN(SELECT run_id FROM retention_runs))
 OR EXISTS(SELECT 1 FROM compaction_entries WHERE trigger_run_id IN(SELECT run_id FROM retention_runs) AND compaction_id NOT IN(SELECT id FROM retention_compactions))
 OR EXISTS(SELECT 1 FROM compaction_entries WHERE parent_compaction_id IN(SELECT id FROM retention_compactions) AND compaction_id NOT IN(SELECT id FROM retention_compactions))
 OR EXISTS(SELECT 1 FROM compaction_entries WHERE compaction_id IN(SELECT id FROM retention_compactions) AND created_at >= $1)
 OR EXISTS(SELECT 1 FROM context_assembly_records WHERE run_id IN(SELECT run_id FROM retention_runs) AND created_at >= $1)
 OR EXISTS(SELECT 1 FROM diagnostic_events WHERE run_id IN(SELECT run_id FROM retention_runs) AND occurred_at >= $1)
 OR EXISTS(SELECT 1 FROM diagnostic_events WHERE (run_id IS NULL OR run_id NOT IN(SELECT run_id FROM retention_runs)) AND audit_ref IN(
  SELECT 'llm_calls:'||id::text FROM llm_calls WHERE run_id IN(SELECT run_id FROM retention_runs)
  UNION ALL SELECT 'tool_invocations:'||id::text FROM tool_invocations WHERE run_id IN(SELECT run_id FROM retention_runs)
  UNION ALL SELECT 'embedding_calls:'||id::text FROM embedding_calls WHERE run_id IN(SELECT run_id FROM retention_runs)))
 OR EXISTS(SELECT 1 FROM observability_export_items WHERE
  (source_kind='runs' AND source_id IN(SELECT run_id FROM retention_runs))
  OR (source_kind='llm_calls' AND source_id IN(SELECT id::text FROM llm_calls WHERE run_id IN(SELECT run_id FROM retention_runs)))
  OR (source_kind='tool_invocations' AND source_id IN(SELECT id::text FROM tool_invocations WHERE run_id IN(SELECT run_id FROM retention_runs)))
  OR (source_kind='embedding_calls' AND source_id IN(SELECT id::text FROM embedding_calls WHERE run_id IN(SELECT run_id FROM retention_runs)))
  OR (source_kind='task_executions' AND source_id IN(SELECT id::text FROM task_executions WHERE run_id IN(SELECT run_id FROM retention_runs)))
  OR (source_kind='diagnostic_events' AND source_id IN(SELECT event_id FROM diagnostic_events WHERE run_id IN(SELECT run_id FROM retention_runs))))
 -- Tool-reference encodings are not a closed public protocol. Keep the group
 -- when any retained tool evidence cannot be proved independent of its audits.
 OR (EXISTS(SELECT 1 FROM tool_invocations WHERE run_id IN(SELECT run_id FROM retention_runs)) AND (
  EXISTS(SELECT 1 FROM message_evidence_refs WHERE kind='tool' AND message_id NOT IN(SELECT id FROM retention_messages))
  OR EXISTS(SELECT 1 FROM compaction_entries WHERE compaction_id NOT IN(SELECT id FROM retention_compactions) AND (source_refs @> '[{"Kind":"tool"}]'::jsonb OR included_refs @> '[{"Kind":"tool"}]'::jsonb OR source_refs @> '[{"kind":"tool"}]'::jsonb OR included_refs @> '[{"kind":"tool"}]'::jsonb))))`

var runGroupDeletes = []string{
	`DELETE FROM message_evidence_refs WHERE message_id IN(SELECT id FROM retention_messages)`,
	`DELETE FROM compaction_entries WHERE compaction_id IN(SELECT id FROM retention_compactions)`,
	`DELETE FROM context_assembly_records WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM diagnostic_events WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM notify_deliveries WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM schedule_deliveries WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM task_executions WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM quota_reservations WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM embedding_calls WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM llm_calls WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM tool_invocations WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM run_resource_refs WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM run_credential_refs WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM run_notify_refs WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM run_resource_manifests WHERE run_id IN(SELECT run_id FROM retention_runs)`,
	`DELETE FROM messages WHERE id IN(SELECT id FROM retention_messages)`,
	`DELETE FROM runs WHERE run_id IN(SELECT run_id FROM retention_runs)`,
}

func cleanupIngestGroup(ctx context.Context, tx *sql.Tx, key string, cutoff int64) (int64, bool, error) {
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT status IN('success','failed','cancelled') AND finished_at < $2
  AND EXISTS(SELECT 1 FROM kb_ingest_jobs newer WHERE newer.version_id=j.version_id AND newer.attempt>j.attempt)
 AND (status <> 'success' OR EXISTS(SELECT 1 FROM kb_ingest_jobs newer WHERE newer.version_id=j.version_id AND newer.attempt>j.attempt AND newer.status='success'))
  AND NOT EXISTS(SELECT 1 FROM embedding_calls WHERE ingest_job_id=j.ingest_job_id AND (status NOT IN('success','failed') OR finished_at IS NULL OR finished_at >= $2))
  AND NOT EXISTS(SELECT 1 FROM quota_reservations WHERE ingest_job_id=j.ingest_job_id AND (state='pending' OR settled_at IS NULL OR settled_at >= $2))
  AND NOT EXISTS(SELECT 1 FROM diagnostic_events WHERE ingest_job_id=j.ingest_job_id AND occurred_at >= $2)
  AND NOT EXISTS(SELECT 1 FROM observability_export_items WHERE (source_kind='kb_ingest_jobs' AND source_id=j.ingest_job_id) OR (source_kind='embedding_calls' AND source_id IN(SELECT id::text FROM embedding_calls WHERE ingest_job_id=j.ingest_job_id)) OR (source_kind='diagnostic_events' AND source_id IN(SELECT event_id FROM diagnostic_events WHERE ingest_job_id=j.ingest_job_id)))
  FROM kb_ingest_jobs j WHERE ingest_job_id=$1 FOR UPDATE`, key, cutoff).Scan(&eligible)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil || !eligible {
		return 0, false, err
	}
	var n int64
	for _, q := range []string{`DELETE FROM quota_reservations WHERE ingest_job_id=$1`, `DELETE FROM embedding_calls WHERE ingest_job_id=$1`, `DELETE FROM diagnostic_events WHERE ingest_job_id=$1 AND occurred_at < $2`, `DELETE FROM kb_ingest_jobs WHERE ingest_job_id=$1`} {
		args := []any{key}
		if q == `DELETE FROM diagnostic_events WHERE ingest_job_id=$1 AND occurred_at < $2` {
			args = append(args, cutoff)
		}
		res, e := tx.ExecContext(ctx, q, args...)
		if e != nil {
			return 0, false, e
		}
		rows, e := res.RowsAffected()
		if e != nil {
			return 0, false, e
		}
		n += rows
	}
	return n, true, nil
}

func cleanupOperation(ctx context.Context, tx *sql.Tx, key string, cutoff int64) (int64, bool, error) {
	// Current/recoverable state remains authoritative even after 30 days. Only
	// explicit transient actions are disposable; future actions fail closed.
	res, err := tx.ExecContext(ctx, `DELETE FROM management_operations o WHERE operation_id=$1 AND finished_at < $2 AND status IN('success','failed')
 AND action IN('knowledge_document.upload','knowledge_document.reindex','knowledge_collection.description_update','model_connection.test','notify_channel.test')
 AND verification IS DISTINCT FROM 'indeterminate'
 AND EXISTS(SELECT 1 FROM management_operations newer WHERE newer.action=o.action AND newer.target_kind=o.target_kind AND newer.target_id=o.target_id AND newer.created_at>o.created_at AND newer.status='success')
 AND NOT EXISTS(SELECT 1 FROM resource_versions WHERE operation_id=o.operation_id)
 AND NOT EXISTS(SELECT 1 FROM resource_pointers WHERE operation_id=o.operation_id)
 AND NOT EXISTS(SELECT 1 FROM credentials WHERE operation_id=o.operation_id)
 AND NOT EXISTS(SELECT 1 FROM credential_versions WHERE operation_id=o.operation_id)
 AND NOT EXISTS(SELECT 1 FROM agent_lifecycle WHERE operation_id=o.operation_id)
 AND NOT EXISTS(SELECT 1 FROM agent_archives WHERE operation_id=o.operation_id)
 AND NOT EXISTS(SELECT 1 FROM resource_connection_tests WHERE operation_id=o.operation_id)
 AND NOT EXISTS(SELECT 1 FROM diagnostic_events WHERE operation_id=o.operation_id AND occurred_at >= $2)
 AND NOT EXISTS(SELECT 1 FROM observability_export_items WHERE (source_kind='management_operations' AND source_id=o.operation_id) OR (source_kind='diagnostic_events' AND source_id IN(SELECT event_id FROM diagnostic_events WHERE operation_id=o.operation_id)))`, key, cutoff)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	return n, n > 0, err
}

func cleanupMaintenance(ctx context.Context, tx *sql.Tx, cutoff int64, limit int) (int64, error) {
	// These have no live business owner. Keep current quotas and latest per-resource
	// connection tests (the latter table already contains only one latest row).
	var n int64
	for _, q := range []string{
		`DELETE FROM config_snapshots WHERE id IN(SELECT s.id FROM config_snapshots s WHERE created_at < $1 AND NOT EXISTS(SELECT 1 FROM runs r WHERE r.config_hash=s.hash) AND NOT EXISTS(SELECT 1 FROM llm_calls c WHERE c.config_hash=s.hash) ORDER BY id LIMIT $2)`,
		`DELETE FROM embedding_calls WHERE id IN(SELECT id FROM embedding_calls WHERE run_id IS NULL AND ingest_job_id IS NULL AND status IN('success','failed') AND finished_at < $1 AND NOT EXISTS(SELECT 1 FROM observability_export_items WHERE source_kind='embedding_calls' AND source_id=embedding_calls.id::text) ORDER BY id LIMIT $2)`,
		`DELETE FROM task_executions WHERE id IN(SELECT e.id FROM task_executions e WHERE finished_at < $1 AND status IN('completed','failed','skipped') AND NOT EXISTS(SELECT 1 FROM runs WHERE run_id=e.run_id) AND NOT EXISTS(SELECT 1 FROM observability_export_items WHERE source_kind='task_executions' AND source_id=e.id::text) ORDER BY id LIMIT $2)`,
		`DELETE FROM schedule_deliveries WHERE run_id IN(SELECT d.run_id FROM schedule_deliveries d WHERE finished_at < $1 AND status IN('sent','failed','skipped') AND NOT EXISTS(SELECT 1 FROM runs WHERE run_id=d.run_id) ORDER BY run_id LIMIT $2)`,
		`DELETE FROM caller_usage_periods WHERE (caller_id,period_start,kind) IN(SELECT u.caller_id,u.period_start,u.kind FROM caller_usage_periods u WHERE updated_at < $1 AND reserved=0 AND period_start < (SELECT max(p.period_start) FROM caller_usage_periods p WHERE p.caller_id=u.caller_id AND p.kind=u.kind) AND NOT EXISTS(SELECT 1 FROM quota_reservations q WHERE q.caller_id=u.caller_id AND q.period_start=u.period_start) ORDER BY period_start LIMIT $2)`,
	} {
		res, err := tx.ExecContext(ctx, q, cutoff, limit)
		if err != nil {
			return 0, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		n += rows
	}
	// Preserve successful deletion-window evidence; detailed failure/no-op reports
	// may expire. Window witnesses are exempt audit anchors, not business payload.
	for _, q := range []string{
		`DELETE FROM observability_cleanup_runs WHERE cleanup_id IN(SELECT r.cleanup_id FROM observability_cleanup_runs r WHERE finished_at < $1 AND deleted_count=0 AND EXISTS(SELECT 1 FROM observability_cleanup_runs latest WHERE latest.table_kind=r.table_kind AND latest.started_at>r.started_at AND latest.status IN('failed','succeeded')) ORDER BY started_at LIMIT $2)`,
	} {
		res, err := tx.ExecContext(ctx, q, cutoff, limit)
		if err != nil {
			return 0, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		n += rows
	}
	// Roll up counts, never contents/IDs; no claim of uninterrupted coverage.
	var pruned int64
	err := tx.QueryRowContext(ctx, `WITH old AS(DELETE FROM history_retention_runs WHERE id IN(SELECT id FROM history_retention_runs WHERE occurred_at < $1 ORDER BY occurred_at LIMIT $2) RETURNING *)
  ,summary AS(INSERT INTO history_retention_rollups SELECT group_kind,(occurred_at/2592000000)*2592000000,min(occurred_at),max(occurred_at),sum(deleted_groups),sum(deleted_rows) FROM old GROUP BY group_kind,(occurred_at/2592000000)*2592000000
 ON CONFLICT(group_kind,month_bucket) DO UPDATE SET first_at=least(history_retention_rollups.first_at,excluded.first_at),last_at=greatest(history_retention_rollups.last_at,excluded.last_at),deleted_groups=history_retention_rollups.deleted_groups+excluded.deleted_groups,deleted_rows=history_retention_rollups.deleted_rows+excluded.deleted_rows RETURNING group_kind) SELECT count(*) FROM old`, cutoff, limit).Scan(&pruned)
	return n + pruned, err
}

func (s *HistoryRetentionStore) HistoryReports(ctx context.Context, from, to time.Time, limit int) ([]historyretention.Report, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("invalid history report limit")
	}
	rows, err := s.db.pool.QueryContext(ctx, `SELECT id,group_kind,cutoff,occurred_at,deleted_groups,deleted_rows,protected_groups,status,reason FROM history_retention_runs WHERE occurred_at >= $1 AND occurred_at <= $2 ORDER BY occurred_at DESC,id DESC LIMIT $3`, from.UnixMilli(), to.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []historyretention.Report{}
	for rows.Next() {
		var r historyretention.Report
		var cutoff, at int64
		if err = rows.Scan(&r.ID, &r.Group, &cutoff, &at, &r.DeletedGroups, &r.DeletedRows, &r.ProtectedGroups, &r.Status, &r.Reason); err != nil {
			return nil, err
		}
		r.Cutoff = time.UnixMilli(cutoff).UTC()
		r.At = time.UnixMilli(at).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

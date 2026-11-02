package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/ymedword/basalt/internal/observability"
)

// ExportCoverage 固定返回八执行源，尚未扫描时保留null，指标没有扫描覆盖。
// ExportCoverage returns the eight execution sources with null unscanned bounds; metrics have no scan coverage.
func (s *ObservabilityStore) ExportCoverage(ctx context.Context) ([]observability.SourceCoverage, error) {
	return obsRows(ctx, s.db, `SELECT k.source_kind,COALESCE(s.coverage_kind,CASE WHEN k.source_kind='diagnostic_events' THEN 'occurred_at' ELSE 'terminal_time' END),s.covered_from,s.covered_through,s.last_scan_started_at,s.last_scan_at,s.last_reconcile_at
		FROM (VALUES ('runs'),('llm_calls'),('tool_invocations'),('embedding_calls'),('task_executions'),('management_operations'),('kb_ingest_jobs'),('diagnostic_events'))k(source_kind)
		LEFT JOIN observability_export_scan s ON s.source_kind=k.source_kind ORDER BY k.source_kind`, nil, func(rows *sql.Rows) (observability.SourceCoverage, error) {
		var out observability.SourceCoverage
		var from, through, attempt, success, reconcile sql.NullInt64
		err := rows.Scan(&out.Source, &out.CoverageKind, &from, &through, &attempt, &success, &reconcile)
		out.CoveredFrom, out.CoveredThrough, out.LastAttemptAt, out.LastSuccessAt, out.LastReconcileAt = obsTime(from), obsTime(through), obsTime(attempt), obsTime(success), obsTime(reconcile)
		return out, err
	})
}

// AlertTransitions 不读取evidence_ref，规则和作用域仅限内置集合。
// AlertTransitions omits evidence_ref and limits rules and scopes to compiled sets.
func (s *ObservabilityStore) AlertTransitions(ctx context.Context, q observability.ReadWindow) ([]observability.AlertTransition, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	return obsRows(ctx, s.db, `SELECT id,rule_id,scope_key,from_state,to_state,value,occurred_at FROM observability_alert_transitions
		WHERE occurred_at >= $1 AND occurred_at <= $2
		AND rule_id IN ('audit_write_failed','diagnostic_write_failed','export_permanent_failure','export_backlog_stuck','cleanup_failed','run_failure_burst','llm_timeout_burst','tool_denial_burst')
		AND scope_key IN ('process','export','cleanup','global') ORDER BY occurred_at DESC,id DESC LIMIT $3`, []any{q.From.UnixMilli(), q.To.UnixMilli(), q.Limit + 1}, func(rows *sql.Rows) (observability.AlertTransition, error) {
		var out observability.AlertTransition
		var at int64
		err := rows.Scan(&out.ID, &out.RuleID, &out.ScopeKey, &out.From, &out.To, &out.Value, &at)
		out.OccurredAt = time.UnixMilli(at).UTC()
		return out, err
	})
}

// CleanupHistory 按清理执行时间读取最新操作；与缺口证明的数据窗口交集查询分开。
// CleanupHistory reads recent execution times separately from data-window gap evidence.
func (s *ObservabilityStore) CleanupHistory(ctx context.Context, q observability.ReadWindow) ([]observability.CleanupRun, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	return obsRows(ctx, s.db, `SELECT cleanup_id,table_kind,window_start,window_end,scan_covered_from,scan_covered_through,
		deleted_count,skipped_count,COALESCE(skip_reason,''),status,COALESCE(error_kind,''),started_at,finished_at
		FROM observability_cleanup_runs WHERE started_at >= $1 AND started_at <= $2 ORDER BY started_at DESC,cleanup_id DESC LIMIT $3`, []any{q.From.UnixMilli(), q.To.UnixMilli(), q.Limit + 1}, func(rows *sql.Rows) (observability.CleanupRun, error) {
		var out observability.CleanupRun
		var start, end, started int64
		var from, through, finished sql.NullInt64
		err := rows.Scan(&out.CleanupID, &out.TableKind, &start, &end, &from, &through, &out.Deleted, &out.Skipped, &out.SkipReason, &out.Status, &out.ErrorKind, &started, &finished)
		out.WindowStart, out.WindowEnd, out.StartedAt = time.UnixMilli(start).UTC(), time.UnixMilli(end).UTC(), time.UnixMilli(started).UTC()
		out.ScanCoveredFrom, out.ScanCoveredThrough, out.FinishedAt = obsTime(from), obsTime(through), obsTime(finished)
		out.ErrorKind = obsCleanupErrorKind(out.ErrorKind)
		return out, err
	})
}

func obsCleanupErrorKind(value string) string {
	if value == "" || value == "cleanup_failed" {
		return value
	}
	return "unclassified"
}

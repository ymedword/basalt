package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

var _ observability.RetentionStore = (*ObservabilityStore)(nil)

// CleanupBatch 仅处理四类可丢弃表组，删除和成功账本必须同事务。
// CleanupBatch handles four droppable groups and atomically records successful deletions.
func (s *ObservabilityStore) CleanupBatch(ctx context.Context, req observability.CleanupRequest) (observability.CleanupRun, error) {
	var out observability.CleanupRun
	if !obsCleanupTable(req.Table) || req.Cutoff.IsZero() || req.Limit < 1 || req.Limit > 10000 || req.Exporting != req.RequireConfirmedExport || req.Exporting && req.Table != "diagnostic_events" || req.Cursor != nil && (req.Cursor.At.IsZero() || req.Cursor.At.After(req.Cutoff) || req.Cursor.ID == "") {
		return out, observability.ErrInvalidQuery
	}
	id, err := obsCleanupID()
	if err != nil {
		return out, err
	}
	now := s.clock()
	out = observability.CleanupRun{CleanupID: id, TableKind: req.Table, WindowStart: req.Cutoff, WindowEnd: req.Cutoff, Status: "succeeded", StartedAt: now}
	if req.Exporting {
		from, through := req.CoveredFrom, req.CoveredThrough
		out.ScanCoveredFrom, out.ScanCoveredThrough = &from, &through
	}
	err = s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout='500ms'`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "basalt:observability:cleanup:"+req.Table); err != nil {
			return err
		}
		if req.Exporting {
			var from, through sql.NullInt64
			err := tx.QueryRowContext(ctx, `SELECT covered_from,covered_through FROM observability_export_scan WHERE source_kind='diagnostic_events' FOR SHARE`).Scan(&from, &through)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if !from.Valid || !through.Valid || req.CoveredFrom.IsZero() || req.CoveredThrough.IsZero() || from.Int64 != req.CoveredFrom.UnixMilli() || through.Int64 < req.CoveredThrough.UnixMilli() {
				out.Status, out.SkipReason = "skipped", "scan_not_covered"
				return s.insertCleanup(ctx, tx, out, req.Exporting)
			}
			if !req.Cutoff.After(req.CoveredFrom) {
				out.Status, out.SkipReason = "skipped", "out_of_coverage"
				return s.insertCleanup(ctx, tx, out, req.Exporting)
			}
		}
		keys, err := obsCleanupKeys(ctx, tx, req)
		if err != nil {
			return err
		}
		backlog := false
		if req.Exporting {
			var age int64
			if err := tx.QueryRowContext(ctx, `SELECT GREATEST(COALESCE($1::bigint-min(created_at),0),0) FROM observability_export_items WHERE state IN ('pending','inflight','retry')`, now.UnixMilli()).Scan(&age); err != nil {
				return err
			}
			backlog = age > (30 * time.Minute).Milliseconds()
		}
		for i, key := range keys {
			at := time.UnixMilli(key.at).UTC()
			if i == 0 {
				out.WindowStart = at
			}
			out.WindowEnd = at
			out.Next = &observability.CleanupCursor{At: at, ID: key.id}
			reason := ""
			if !req.Exporting && req.Table == "diagnostic_events" {
				var unresolved bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM observability_export_items WHERE source_kind='diagnostic_events' AND source_id=$1 AND state IN('pending','inflight','retry','unconfirmed'))`, key.id).Scan(&unresolved); err != nil {
					return err
				}
				if unresolved {
					reason = "unconfirmed_export"
				}
			}
			if req.Exporting {
				switch {
				case at.Before(req.CoveredFrom) || at.After(req.CoveredThrough):
					reason = "out_of_coverage"
				case backlog:
					reason = "unexported_backlog"
				default:
					reason, err = obsDiagnosticExportPermit(ctx, tx, key.id)
					if err != nil {
						return err
					}
				}
			}
			if reason != "" {
				out.Skipped++
				out.SkipReason = obsCleanupReason(out.SkipReason, reason)
				continue
			}
			if err := obsDeleteCleanupKey(ctx, tx, req.Table, key.id); err != nil {
				return err
			}
			out.Deleted++
		}
		if out.Deleted == 0 && out.Skipped > 0 {
			out.Status = "skipped"
		}
		finished := s.clock()
		out.FinishedAt = &finished
		return s.insertCleanup(ctx, tx, out, req.Exporting)
	})
	if err != nil {
		// 删除回滚后另记失败尝试，计数必须归零，不能伪称已经删除。
		// After rollback, a separate failed attempt has zero counts and cannot claim deletion.
		out.Deleted, out.Skipped, out.Next = 0, 0, nil
		out.Status, out.ErrorKind = "failed", "cleanup_failed"
		finished := s.clock()
		out.FinishedAt = &finished
		if recordErr := s.insertCleanup(ctx, s.db.pool, out, req.Exporting); recordErr != nil {
			slog.WarnContext(ctx, "cleanup failure evidence unavailable", "error_kind", "cleanup_failed")
		}
		return out, fmt.Errorf("cleanup transaction: %w", err)
	}
	return out, nil
}

// RecordSkipped 记录未枚举候选的跳过尝试，不声称删除发生。
// RecordSkipped records an unenumerated skip attempt without claiming deletion.
func (s *ObservabilityStore) RecordSkipped(ctx context.Context, table string, at time.Time, reason string) error {
	if !obsCleanupTable(table) || at.IsZero() || !slices.Contains([]string{"scan_not_covered", "out_of_coverage", "unexported_backlog", "unconfirmed_export", "unexported_terminal", "policy_disabled"}, reason) {
		return observability.ErrInvalidQuery
	}
	id, err := obsCleanupID()
	if err != nil {
		return err
	}
	return s.insertCleanup(ctx, s.db.pool, observability.CleanupRun{CleanupID: id, TableKind: table, WindowStart: at, WindowEnd: at, StartedAt: at, FinishedAt: &at, Status: "skipped", SkipReason: reason}, table == "diagnostic_events" && reason != "policy_disabled")
}

func obsCleanupTable(table string) bool {
	return slices.Contains([]string{"diagnostic_events", "context_assembly_records", "alert_history", "export_history"}, table)
}

func obsCleanupID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

type obsCleanupExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *ObservabilityStore) insertCleanup(ctx context.Context, exec obsCleanupExecutor, run observability.CleanupRun, confirmed bool) error {
	finished := run.FinishedAt
	if finished == nil {
		at := s.clock()
		finished = &at
	}
	var from, through any
	if run.ScanCoveredFrom != nil && !run.ScanCoveredFrom.IsZero() {
		from = run.ScanCoveredFrom.UnixMilli()
	}
	if run.ScanCoveredThrough != nil && !run.ScanCoveredThrough.IsZero() {
		through = run.ScanCoveredThrough.UnixMilli()
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO observability_cleanup_runs(cleanup_id,table_kind,window_start,window_end,scan_covered_from,scan_covered_through,require_confirmed_export,deleted_count,skipped_count,skip_reason,status,error_kind,started_at,finished_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, run.CleanupID, run.TableKind, run.WindowStart.UnixMilli(), run.WindowEnd.UnixMilli(), from, through, confirmed, run.Deleted, run.Skipped, nullIfEmpty(run.SkipReason), run.Status, nullIfEmpty(run.ErrorKind), run.StartedAt.UnixMilli(), finished.UnixMilli())
	return err
}

type obsCleanupKey struct {
	id string
	at int64
}

func obsCleanupKeys(ctx context.Context, tx *sql.Tx, req observability.CleanupRequest) ([]obsCleanupKey, error) {
	var from, id, at, predicate string
	switch req.Table {
	case "diagnostic_events":
		from, id, at, predicate = "diagnostic_events", "event_id", "occurred_at", `NOT EXISTS(SELECT 1 FROM runs r WHERE r.run_id=diagnostic_events.run_id AND r.status='running')`
	case "context_assembly_records":
		from, id, at, predicate = "context_assembly_records", "record_id", "created_at", `NOT EXISTS(SELECT 1 FROM runs r WHERE r.run_id=context_assembly_records.run_id AND r.status='running')`
	case "alert_history":
		from, id, at = "observability_alert_transitions t", "t.id::text", "t.occurred_at"
		predicate = `NOT EXISTS(SELECT 1 FROM observability_alert_states s WHERE s.rule_id=t.rule_id AND s.scope_key=t.scope_key AND
			(t.id=(SELECT max(m.id) FROM observability_alert_transitions m WHERE m.rule_id=s.rule_id AND m.scope_key=s.scope_key) OR
			(s.state='unknown' AND t.id=(SELECT max(m.id) FROM observability_alert_transitions m WHERE m.rule_id=s.rule_id AND m.scope_key=s.scope_key AND m.to_state<>'unknown'))))`
	case "export_history":
		from = `(SELECT 'item:'||i.item_id AS id,i.updated_at AS at FROM observability_export_items i WHERE i.state IN ('sent','failed_permanent','dropped','stale')
			AND NOT ($5 AND i.source_kind='diagnostic_events' AND EXISTS(SELECT 1 FROM diagnostic_events d WHERE d.event_id=i.source_id))
			UNION ALL SELECT 'batch:'||b.batch_id AS id,b.attempted_at AS at FROM observability_export_batches b WHERE b.outcome<>'unknown'
			AND NOT EXISTS(SELECT 1 FROM observability_export_items i WHERE i.last_batch_id=b.batch_id)) history`
		id, at, predicate = "id", "at", "true"
	default:
		return nil, observability.ErrInvalidQuery
	}
	cursorAt, cursorID := any(nil), ""
	if req.Cursor != nil {
		cursorAt, cursorID = req.Cursor.At.UnixMilli(), req.Cursor.ID
	}
	// 上述四表switch固定全部片段；时间、游标、limit和开关只用参数绑定。
	// The four-table switch fixes every fragment; values and cursors remain bound parameters.
	// #nosec G202 -- identifiers/expressions are code constants; SQLBoundary tests cover hostile table/cursor values.
	query := `SELECT ` + id + `,` + at + ` FROM ` + from + ` WHERE (` + predicate + `) AND ` + at + `<$1 AND ($2::bigint IS NULL OR ` + at + `>$2 OR (` + at + `=$2 AND (` + id + `) COLLATE "C">$3 COLLATE "C")) ORDER BY ` + at + `,(` + id + `) COLLATE "C" LIMIT $4`
	args := []any{req.Cutoff.UnixMilli(), cursorAt, cursorID, req.Limit}
	if req.Table == "export_history" {
		args = append(args, req.ExportEnabled)
	} else {
		query += ` FOR UPDATE SKIP LOCKED`
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.WarnContext(ctx, "cleanup cursor close failed", "error_kind", "query_failed")
		}
	}()
	out := []obsCleanupKey{}
	for rows.Next() {
		var key obsCleanupKey
		if err := rows.Scan(&key.id, &key.at); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

func obsDeleteCleanupKey(ctx context.Context, tx *sql.Tx, table, id string) error {
	var query string
	switch table {
	case "diagnostic_events":
		query = `DELETE FROM diagnostic_events WHERE event_id=$1`
	case "context_assembly_records":
		query = `DELETE FROM context_assembly_records WHERE record_id=$1`
	case "alert_history":
		query = `DELETE FROM observability_alert_transitions WHERE id::text=$1`
	case "export_history":
		kind, value, ok := strings.Cut(id, ":")
		if !ok {
			return observability.ErrInvalidQuery
		}
		id = value
		switch kind {
		case "item":
			query = `DELETE FROM observability_export_items WHERE item_id=$1 AND state IN ('sent','failed_permanent','dropped','stale')`
		case "batch":
			query = `DELETE FROM observability_export_batches WHERE batch_id=$1 AND outcome<>'unknown' AND NOT EXISTS(SELECT 1 FROM observability_export_items i WHERE i.last_batch_id=$1)`
		default:
			return observability.ErrInvalidQuery
		}
	default:
		return observability.ErrInvalidQuery
	}
	result, err := tx.ExecContext(ctx, query, id)
	if err != nil {
		return err
	}
	return obsRequireOne(result)
}

func obsDiagnosticExportPermit(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	value, err := obsDecodeDiagnostic(tx.QueryRowContext(ctx, `SELECT `+obsDiagnosticSafeColumns+` FROM diagnostic_events WHERE event_id=$1`, id))
	if err != nil {
		return "", err
	}
	hash, err := observability.ExportStateHash(observability.ExportItem{SourceKind: observability.SourceDiagnostic, SourceID: id, Signal: observability.SignalLog, Fact: value})
	if err != nil {
		return "", err
	}
	rows, err := tx.QueryContext(ctx, `SELECT state,state_hash,source_terminal_at FROM observability_export_items WHERE source_kind='diagnostic_events' AND source_id=$1 LIMIT 10001 FOR UPDATE`, id)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := rows.Close(); err != nil {
			slog.WarnContext(ctx, "cleanup export proof close failed", "error_kind", "query_failed")
		}
	}()
	// 终态时间由持锁候选行给出；解码类型固定为诊断事件。
	// The locked source row supplies terminal time; decoding always yields a diagnostic event.
	event, ok := value.(diagnostics.DiagnosticEvent)
	if !ok {
		return "", errors.New("invalid diagnostic export proof")
	}
	eventAt := event.OccurredAt
	hasSent := false
	reason := ""
	count := 0
	for rows.Next() {
		count++
		var state, stateHash string
		var terminal int64
		if err := rows.Scan(&state, &stateHash, &terminal); err != nil {
			return "", err
		}
		switch state {
		case "pending", "inflight", "retry":
			reason = obsCleanupReason(reason, "unexported_backlog")
		case "unconfirmed":
			reason = obsCleanupReason(reason, "unconfirmed_export")
		case "failed_permanent", "stale", "dropped":
			reason = obsCleanupReason(reason, "unexported_terminal")
		case "sent":
			if stateHash == hash && terminal >= eventAt {
				hasSent = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count > 10000 {
		return "", errors.New("cleanup export proof prefix truncated")
	}
	if reason != "" {
		return reason, nil
	}
	if !hasSent {
		return "unexported_terminal", nil
	}
	return "", nil
}

func obsCleanupReason(old, next string) string {
	priority := map[string]int{"": 0, "out_of_coverage": 1, "unexported_terminal": 2, "unexported_backlog": 3, "unconfirmed_export": 4}
	if priority[next] > priority[old] {
		return next
	}
	return old
}

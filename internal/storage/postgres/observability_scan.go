package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/observability"
)

var _ observability.ExportSourceStore = (*ObservabilityStore)(nil)

type obsSourceSpec struct {
	from, id, terminal, predicate, columns string
	decode                                 func(rowScanner) (any, error)
}

// obsSource 是固定SQL注册表；外部值只能作为参数，不能成为表名或表达式。
// obsSource is a fixed SQL registry; external values never become identifiers or expressions.
func obsSource(kind observability.SourceKind) (obsSourceSpec, bool) {
	switch kind {
	case observability.SourceRun:
		return obsSourceSpec{"runs", "run_id", "finished_at", "status<>'running' AND finished_at IS NOT NULL", obsRunColumns, func(r rowScanner) (any, error) { return obsScanRun(r) }}, true
	case observability.SourceLLM:
		return obsSourceSpec{"llm_calls", "id::text", "GREATEST(created_at+duration_ms,COALESCE(recovered_at,0))", "status IN ('success','failed','unknown')", "id,run_id,status,purpose,COALESCE(error_kind,''),model,round,created_at,duration_ms,recovered_at,prompt_tokens,completion_tokens", obsDecodeLLM}, true
	case observability.SourceTool:
		return obsSourceSpec{"tool_invocations", "id::text", "COALESCE(finished_at,recovered_at)", "status<>'pending' AND COALESCE(finished_at,recovered_at) IS NOT NULL", "id,run_id,COALESCE(tool_call_id,''),tool_name,status,COALESCE(error_kind,''),started_at,finished_at,recovered_at", obsDecodeTool}, true
	case observability.SourceEmbedding:
		return obsSourceSpec{"embedding_calls", "id::text", "finished_at", "status<>'pending' AND finished_at IS NOT NULL", obsEmbeddingColumns, obsDecodeEmbedding}, true
	case observability.SourceTask:
		return obsSourceSpec{"task_executions e JOIN scheduled_tasks t USING(task_id)", "e.id::text", "e.finished_at", "e.status IN ('completed','failed','skipped') AND e.finished_at IS NOT NULL", obsTaskColumns, obsDecodeTask}, true
	case observability.SourceOperation:
		return obsSourceSpec{"management_operations", "operation_id", "GREATEST(finished_at,COALESCE(recovered_at,finished_at))", "status IN ('success','failed','unknown') AND finished_at IS NOT NULL", "operation_id,action,status,COALESCE(verification,''),COALESCE(result_code,''),created_at,finished_at,recovered_at", obsDecodeOperation}, true
	case observability.SourceIngest:
		return obsSourceSpec{"kb_ingest_jobs", "ingest_job_id", "finished_at", "status IN ('success','failed','cancelled') AND finished_at IS NOT NULL", "ingest_job_id,status,stage,COALESCE(error_kind,''),attempt,started_at,finished_at", obsDecodeIngest}, true
	case observability.SourceDiagnostic:
		return obsSourceSpec{"diagnostic_events", "event_id", "occurred_at", "true", obsDiagnosticSafeColumns, obsDecodeDiagnostic}, true
	default:
		return obsSourceSpec{}, false
	}
}

// ScanCoverage 区分未覆盖与毫秒零值，不把失败或空行解释成已扫描。
// ScanCoverage distinguishes absent coverage from Unix epoch and from read failures.
func (s *ObservabilityStore) ScanCoverage(ctx context.Context, table string) (time.Time, time.Time, bool, error) {
	if _, ok := obsSource(observability.SourceKind(table)); !ok {
		return time.Time{}, time.Time{}, false, observability.ErrInvalidQuery
	}
	var from, through sql.NullInt64
	err := s.db.pool.QueryRowContext(ctx, `SELECT covered_from,covered_through FROM observability_export_scan WHERE source_kind=$1`, table).Scan(&from, &through)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("read scan coverage: %w", err)
	}
	if !from.Valid || !through.Valid {
		return time.Time{}, time.Time{}, false, nil
	}
	return time.UnixMilli(from.Int64).UTC(), time.UnixMilli(through.Int64).UTC(), true, nil
}

// StartScan 仅登记开始时间；任何失败都不会伪造完成覆盖。
// StartScan records an attempt without claiming completed coverage.
func (s *ObservabilityStore) StartScan(ctx context.Context, kind observability.SourceKind, now time.Time) error {
	if _, ok := obsSource(kind); !ok || now.IsZero() {
		return observability.ErrInvalidQuery
	}
	coverageKind := "terminal_time"
	if kind == observability.SourceDiagnostic {
		coverageKind = "occurred_at"
	}
	_, err := s.db.pool.ExecContext(ctx, `INSERT INTO observability_export_scan(source_kind,coverage_kind,last_scan_started_at,updated_at)
		VALUES($1,$2,$3,$3) ON CONFLICT(source_kind) DO UPDATE SET last_scan_started_at=$3,updated_at=$3`, string(kind), coverageKind, now.UnixMilli())
	return err
}

func obsNullableMillis(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UnixMilli()
}

// AdvanceScanCoverage 用旧边界CAS，拒绝扩张起点或后退终点。
// AdvanceScanCoverage compares old bounds and forbids widened history or a retreating endpoint.
func (s *ObservabilityStore) AdvanceScanCoverage(ctx context.Context, kind observability.SourceKind, oldFrom, oldThrough, from, through, now time.Time) (bool, error) {
	if _, ok := obsSource(kind); !ok || from.IsZero() || through.IsZero() || now.IsZero() || from.After(through) || oldFrom.IsZero() != oldThrough.IsZero() || !oldFrom.IsZero() && (!from.Equal(oldFrom) || through.Before(oldThrough)) {
		return false, observability.ErrInvalidQuery
	}
	res, err := s.db.pool.ExecContext(ctx, `UPDATE observability_export_scan SET covered_from=$4,covered_through=$5,last_scan_at=$6,last_reconcile_at=$6,updated_at=$6
		WHERE source_kind=$1 AND covered_from IS NOT DISTINCT FROM $2::bigint AND covered_through IS NOT DISTINCT FROM $3::bigint`, string(kind), obsNullableMillis(oldFrom), obsNullableMillis(oldThrough), from.UnixMilli(), through.UnixMilli(), now.UnixMilli())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// EnqueuePage 返回实际新增数，重复扫描不重复计数。
// EnqueuePage counts actual inserts instead of replayed scan attempts.
func (s *ObservabilityStore) EnqueuePage(ctx context.Context, items []observability.ExportItem) (int, error) {
	return s.enqueueExportItems(ctx, items)
}

// ScanPage 先按固定终态键分页，再重读允许列；游标始终来自原查询。
// ScanPage pages terminal keys before reloading safe facts; its cursor comes from the original query.
func (s *ObservabilityStore) ScanPage(ctx context.Context, kind observability.SourceKind, from, through time.Time, cursor observability.ScanCursor, limit int) (observability.SourcePage, error) {
	spec, ok := obsSource(kind)
	if !ok || from.IsZero() || through.IsZero() || from.After(through) || limit < 1 || limit > 10000 {
		return observability.SourcePage{}, observability.ErrInvalidQuery
	}
	type key struct {
		id       string
		terminal int64
	}
	// 注册表的SQL片段是固定源码；时间、游标、页大小全部参数化。
	// Registry fragments are fixed source; window, cursor, and limit are parameters.
	stmt := `SELECT ` + spec.id + `,` + spec.terminal + ` FROM ` + spec.from + ` WHERE (` + spec.predicate + `) AND (` + spec.terminal + `) BETWEEN $1 AND $2
		AND ($3::bigint IS NULL OR (` + spec.terminal + `)>$3 OR ((` + spec.terminal + `)=$3 AND (` + spec.id + `) COLLATE "C">$4 COLLATE "C"))
		ORDER BY (` + spec.terminal + `),(` + spec.id + `) COLLATE "C" LIMIT $5`
	keys, err := obsRows(ctx, s.db, stmt, []any{from.UnixMilli(), through.UnixMilli(), obsNullableMillis(cursor.TerminalAt), cursor.SourceID, limit}, func(r *sql.Rows) (key, error) {
		var out key
		err := r.Scan(&out.id, &out.terminal)
		return out, err
	})
	if err != nil {
		return observability.SourcePage{}, err
	}
	page := observability.SourcePage{Items: []observability.ExportItem{}, Scanned: len(keys), Complete: len(keys) < limit, Cursor: cursor}
	for _, k := range keys {
		page.Cursor = observability.ScanCursor{TerminalAt: time.UnixMilli(k.terminal).UTC(), SourceID: k.id}
		item, exists, err := s.loadExportFact(ctx, kind, k.id)
		if err != nil {
			return observability.SourcePage{}, err
		}
		if exists && !item.SourceTerminalAt.Before(from) && !item.SourceTerminalAt.After(through) {
			page.Items = append(page.Items, item)
		}
	}
	return page, nil
}

type obsTerminalRow struct {
	row      rowScanner
	terminal *int64
}

func (r obsTerminalRow) Scan(dest ...any) error {
	return r.row.Scan(append([]any{r.terminal}, dest...)...)
}

func (s *ObservabilityStore) loadExportFact(ctx context.Context, kind observability.SourceKind, id string) (observability.ExportItem, bool, error) {
	spec, ok := obsSource(kind)
	if !ok {
		return observability.ExportItem{}, false, observability.ErrInvalidQuery
	}
	// 固定八来源switch提供全部SQL片段，源ID仅绑定$1；SQLBoundary测试覆盖恶意kind/id。
	// All SQL fragments come from the fixed source registry; source identity is bound.
	// #nosec G202 -- obsSource returns only code constants; no external SQL fragments.
	row := s.db.pool.QueryRowContext(ctx, `SELECT `+spec.terminal+`,`+spec.columns+` FROM `+spec.from+` WHERE (`+spec.id+`)=$1 AND (`+spec.predicate+`)`, id)
	var terminal int64
	fact, err := spec.decode(obsTerminalRow{row, &terminal})
	if errors.Is(err, sql.ErrNoRows) {
		return observability.ExportItem{}, false, nil
	}
	if err != nil {
		return observability.ExportItem{}, false, fmt.Errorf("reload export fact: %w", err)
	}
	signal := observability.SignalTrace
	if kind == observability.SourceDiagnostic {
		signal = observability.SignalLog
	}
	if f, ok := fact.(observability.LLMCallFact); ok && f.Status == "unknown" {
		signal = observability.SignalLog
	}
	if f, ok := fact.(observability.ToolInvocationFact); ok && f.Status == "unknown" && f.FinishedAt == nil {
		signal = observability.SignalLog
	}
	hash, err := observability.ExportStateHash(observability.ExportItem{SourceKind: kind, SourceID: id, Signal: signal, Fact: fact})
	if err != nil {
		return observability.ExportItem{}, false, err
	}
	item := observability.ExportItem{SourceKind: kind, SourceID: id, SourceTerminalAt: time.UnixMilli(terminal).UTC(), Signal: signal, StateHash: hash, Fact: fact}
	item.ItemID = observability.ExportItemKey(kind, id, hash)
	return item, true, nil
}

// Reload 发送前重读权威事实；指标读取原快照，不重算历史点。
// Reload reads authoritative facts before sending; metrics retain their original snapshot.
func (s *ObservabilityStore) Reload(ctx context.Context, item observability.ExportItem) (observability.ExportItem, bool, error) {
	if item.SourceKind != observability.SourceMetricSnapshot {
		return s.loadExportFact(ctx, item.SourceKind, item.SourceID)
	}
	var raw []byte
	err := s.db.pool.QueryRowContext(ctx, `SELECT payload FROM observability_export_items WHERE item_id=$1 AND source_kind='metric_snapshot'`, item.ItemID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return observability.ExportItem{}, false, nil
	}
	if err != nil {
		return observability.ExportItem{}, false, err
	}
	var snapshot observability.MetricsSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return observability.ExportItem{}, false, err
	}
	hash, err := observability.CanonicalStateHash(snapshot)
	if err != nil {
		return observability.ExportItem{}, false, err
	}
	item.Fact, item.Payload, item.StateHash = snapshot, raw, hash
	return item, true, nil
}

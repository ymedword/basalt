package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ymedword/basalt/internal/observability"
)

// ExportStatus 读取持久计数与最后批次；NULL响应计数仍保持NULL。
// ExportStatus reads durable counts and preserves null response counts.
func (s *ObservabilityStore) ExportStatus(ctx context.Context) (observability.ExportStatus, error) {
	var out observability.ExportStatus
	err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE state='pending'),count(*) FILTER(WHERE state='inflight'),count(*) FILTER(WHERE state='retry'),count(*) FILTER(WHERE state='unconfirmed'),count(*) FILTER(WHERE state='failed_permanent'),count(*) FILTER(WHERE state='dropped'),count(*) FILTER(WHERE state='stale') FROM observability_export_items`).Scan(&out.Pending, &out.Inflight, &out.Retry, &out.Unconfirmed, &out.FailedPermanent, &out.Dropped, &out.Stale)
	if err != nil {
		return out, err
	}
	// 当前retry项的再次认领次数不含首次认领，不证明网络已发送；空队列保持NULL。
	// Retry attempts sum repeat claims, excluding initial claims, without proving network sends.
	var attempts, next sql.NullInt64
	if err := s.db.pool.QueryRowContext(ctx, `SELECT sum(GREATEST(attempts-1,0)) FILTER(WHERE state='retry'),min(next_attempt_at) FILTER(WHERE state='retry') FROM observability_export_items`).Scan(&attempts, &next); err != nil {
		return out, err
	}
	if attempts.Valid {
		n := attempts.Int64
		out.RetryAttempts = &n
	}
	out.NextRetryAt = obsTime(next)
	out.OldestPendingAge, err = s.OldestPendingAge(ctx)
	if err != nil {
		return out, err
	}
	if err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM observability_export_batches WHERE outcome='unknown'`).Scan(&out.UnknownAttempts); err != nil {
		return out, err
	}
	var scan, partial sql.NullInt64
	if err := s.db.pool.QueryRowContext(ctx, `SELECT max(last_scan_at) FROM observability_export_scan`).Scan(&scan); err != nil {
		return out, err
	}
	if scan.Valid {
		out.LastScanAt = time.UnixMilli(scan.Int64).UTC()
	}
	if err := s.db.pool.QueryRowContext(ctx, `SELECT max(attempted_at) FROM observability_export_batches WHERE outcome='partial_unconfirmed'`).Scan(&partial); err != nil {
		return out, err
	}
	out.LastPartialAt = obsTime(partial)
	out.OldestPendingAgeSeconds = out.OldestPendingAge.Seconds()
	if !out.LastScanAt.IsZero() {
		at := out.LastScanAt
		out.LastScanTime = &at
	}
	out.Sources, err = s.ExportCoverage(ctx)
	if err != nil {
		return out, err
	}
	var warning, kind sql.NullString
	var accepted, rejected, httpStatus sql.NullInt64
	err = s.db.pool.QueryRowContext(ctx, `SELECT batch_id,record_unit,record_count,outcome,accepted_records,rejected_records,warning_text,error_kind,http_status FROM observability_export_batches ORDER BY attempted_at DESC,batch_id DESC LIMIT 1`).Scan(&out.LastBatchID, &out.LastRecordUnit, &out.LastRecordCount, &out.LastOutcome, &accepted, &rejected, &warning, &kind, &httpStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if accepted.Valid {
		a, r := int(accepted.Int64), int(rejected.Int64)
		out.LastAccepted, out.LastRejected = &a, &r
	}
	if httpStatus.Valid {
		status := int(httpStatus.Int64)
		out.LastHTTPStatus = &status
	}
	if warning.Valid && warning.String != "" {
		out.LastWarning = "receiver_warning"
	}
	out.LastErrorKind = observability.ReadExportErrorKind(kind.String)
	return out, nil
}

package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/observability"
)

var _ observability.ExportLedger = (*ObservabilityStore)(nil)

// Enqueue 仅持久化遥测定位，执行事实不复制为payload。
// Enqueue persists telemetry identity without copying execution facts into payloads.
func (s *ObservabilityStore) Enqueue(ctx context.Context, items []observability.ExportItem) error {
	_, err := s.enqueueExportItems(ctx, items)
	return err
}

func (s *ObservabilityStore) enqueueExportItems(ctx context.Context, items []observability.ExportItem) (int, error) {
	if len(items) > 10000 {
		return 0, observability.ErrInvalidQuery
	}
	for _, item := range items {
		if err := item.Validate(); err != nil {
			return 0, err
		}
	}
	if len(items) == 0 {
		return 0, nil
	}
	now := s.clock().UnixMilli()
	inserted := 0
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, item := range items {
			id := observability.ExportItemKey(item.SourceKind, item.SourceID, item.StateHash)
			var payload any
			if len(item.Payload) > 0 {
				payload = string(item.Payload)
			}
			// 不指定冲突目标：表上同时有 item_id 主键与 (source_kind,source_id,state_hash)
			// 唯一约束，而 item_id 正是该三元组的确定性摘要。并发插入同一事实时，PostgreSQL
			// 可能先在主键索引上报告 23505；带目标的 ON CONFLICT 只处理目标索引，别的唯一
			// 约束仍会失败。无目标的 DO NOTHING 覆盖任一唯一冲突，语义仍是幂等入队。
			// No conflict target: the table has both an item_id primary key and the unique
			// (source_kind,source_id,state_hash) constraint, and item_id is the deterministic
			// digest of that tuple. Concurrent enqueues can trip the primary-key index first;
			// a targeted ON CONFLICT only handles its arbiter index, so DO NOTHING without a
			// target keeps enqueue idempotent across either unique violation.
			result, err := tx.ExecContext(ctx, `INSERT INTO observability_export_items(item_id,source_kind,source_id,state_hash,signal,source_terminal_at,state,created_at,updated_at,payload)
				VALUES ($1,$2,$3,$4,$5,$6,'pending',$7,$7,$8::jsonb) ON CONFLICT DO NOTHING`, id, item.SourceKind, item.SourceID, item.StateHash, item.Signal, item.SourceTerminalAt.UnixMilli(), now, payload)
			if err != nil {
				return err
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			inserted += int(count)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// Claim 按租约认领并增加claim次数，不证明网络调用已发出。
// Claim leases work and increments claim counts without proving a network call occurred.
func (s *ObservabilityStore) Claim(ctx context.Context, limit int, now time.Time, leaseTTL time.Duration) ([]observability.ExportItem, error) {
	if limit < 1 || limit > 2048 || now.IsZero() || leaseTTL <= 0 || leaseTTL > time.Hour {
		return nil, observability.ErrInvalidQuery
	}
	items, err := obsRows(ctx, s.db, `WITH candidates AS (
		SELECT item_id FROM observability_export_items WHERE state='pending'
		OR (state='retry' AND (next_attempt_at IS NULL OR next_attempt_at<=$1))
		OR (state='inflight' AND lease_until<=$1)
		ORDER BY created_at,item_id LIMIT $2 FOR UPDATE SKIP LOCKED)
		UPDATE observability_export_items i SET state='inflight',attempts=i.attempts+1,lease_until=$1::bigint+$3::bigint,next_attempt_at=NULL,updated_at=$1
		FROM candidates c WHERE i.item_id=c.item_id RETURNING `+obsExportColumns, []any{now.UnixMilli(), limit, leaseTTL.Milliseconds()}, obsScanExportItem)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(items, func(a, b observability.ExportItem) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ItemID, b.ItemID)
	})
	return items, nil
}

const obsExportColumns = `i.item_id,i.source_kind,i.source_id,i.state_hash,i.signal,i.source_terminal_at,i.state,i.attempts,
	i.last_batch_id,i.created_at,i.next_attempt_at,i.lease_until,i.last_error_kind,i.payload`

func obsScanExportItem(rows *sql.Rows) (observability.ExportItem, error) {
	var item observability.ExportItem
	var terminal, created int64
	var next, lease sql.NullInt64
	var batch, kind sql.NullString
	var payload []byte
	err := rows.Scan(&item.ItemID, &item.SourceKind, &item.SourceID, &item.StateHash, &item.Signal, &terminal, &item.State, &item.Attempts, &batch, &created, &next, &lease, &kind, &payload)
	item.SourceTerminalAt, item.CreatedAt = time.UnixMilli(terminal).UTC(), time.UnixMilli(created).UTC()
	item.NextAttemptAt, item.LeaseUntil = obsTime(next), obsTime(lease)
	item.LastBatchID, item.LastErrorKind, item.Payload = batch.String, kind.String, payload
	return item, err
}

// MarkStale 隔离过期来源哈希，不修改业务源行。
// MarkStale isolates outdated hashes without modifying source facts.
func (s *ObservabilityStore) MarkStale(ctx context.Context, items []observability.ExportItem) error {
	return s.markExportItems(ctx, items, observability.ExportStale, "source_changed", s.clock())
}

// FailExportItems 隔离映射/重试预算失败，不伪造一个零记录网络批次。
// FailExportItems isolates mapping or retry-budget failures without inventing a network batch.
func (s *ObservabilityStore) FailExportItems(ctx context.Context, items []observability.ExportItem, kind string, now time.Time) error {
	if !slices.Contains([]string{"mapping_error", "retry_exhausted", "source_unavailable"}, kind) {
		return observability.ErrInvalidQuery
	}
	return s.markExportItems(ctx, items, observability.ExportFailedPermanent, kind, now)
}

func (s *ObservabilityStore) markExportItems(ctx context.Context, items []observability.ExportItem, state observability.ExportState, kind string, now time.Time) error {
	if len(items) == 0 {
		return nil
	}
	items = obsSortedItems(items)
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, item := range items {
			result, err := tx.ExecContext(ctx, `UPDATE observability_export_items SET state=$1,last_error_kind=$2,lease_until=NULL,next_attempt_at=NULL,updated_at=$3
				WHERE item_id=$4 AND state='inflight' AND attempts=$5`, state, kind, now.UnixMilli(), item.ItemID, item.Attempts)
			if err != nil {
				return err
			}
			if err := obsRequireOne(result); err != nil {
				return err
			}
		}
		return nil
	})
}

func obsSortedItems(items []observability.ExportItem) []observability.ExportItem {
	items = slices.Clone(items)
	slices.SortFunc(items, func(a, b observability.ExportItem) int { return strings.Compare(a.ItemID, b.ItemID) })
	return items
}

func obsRequireOne(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return core.ErrConflict
	}
	return nil
}

func obsValidateBatch(batch observability.ExportBatch) error {
	if batch.BatchID == "" || len(batch.BatchID) > 256 || len(batch.Items) == 0 || len(batch.Items) > 2048 || batch.RecordCount <= 0 || batch.RecordCount > 1000000 || batch.MaxAttempts < 0 || batch.MaxItemAge < 0 {
		return observability.ErrInvalidQuery
	}
	unit := map[observability.SignalKind]string{observability.SignalTrace: "span", observability.SignalLog: "log_record", observability.SignalMetric: "data_point"}[batch.Signal]
	if unit == "" || batch.RecordUnit != unit {
		return observability.ErrInvalidQuery
	}
	seen := map[string]bool{}
	for _, item := range batch.Items {
		if item.Signal != batch.Signal || item.Attempts < 1 || item.ItemID == "" || seen[item.ItemID] {
			return observability.ErrInvalidQuery
		}
		seen[item.ItemID] = true
	}
	return nil
}

// BeginBatch 先记unknown/NULL尝试；写入失败则不发送。
// BeginBatch records an unknown attempt with null counts before sending.
func (s *ObservabilityStore) BeginBatch(ctx context.Context, batch observability.ExportBatch, now time.Time) error {
	if err := obsValidateBatch(batch); err != nil {
		return err
	}
	if now.IsZero() {
		return observability.ErrInvalidQuery
	}
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, item := range obsSortedItems(batch.Items) {
			result, err := tx.ExecContext(ctx, `UPDATE observability_export_items SET last_batch_id=$1,updated_at=$2
				WHERE item_id=$3 AND state='inflight' AND attempts=$4 AND lease_until>$2 AND signal=$5 AND state_hash=$6`, batch.BatchID, now.UnixMilli(), item.ItemID, item.Attempts, batch.Signal, item.StateHash)
			if err != nil {
				return err
			}
			if err := obsRequireOne(result); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO observability_export_batches(batch_id,signal,record_unit,item_count,record_count,outcome,attempted_at)
			VALUES ($1,$2,$3,$4,$5,'unknown',$6)`, batch.BatchID, batch.Signal, batch.RecordUnit, len(batch.Items), batch.RecordCount, now.UnixMilli())
		return err
	})
}

// ApplySendResult 原子更新批次及成员，不猜测部分成功的逐条身份。
// ApplySendResult atomically updates a batch and its items without guessing rejected identities.
func (s *ObservabilityStore) ApplySendResult(ctx context.Context, batch observability.ExportBatch, res observability.SendResult, sendErr error, now time.Time) error {
	if err := obsValidateBatch(batch); err != nil {
		return err
	}
	if now.IsZero() {
		return observability.ErrInvalidQuery
	}
	disposition := observability.ResolveSendResult(batch, res, sendErr)
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var outcome string
		var count, itemCount int
		var signal observability.SignalKind
		if err := tx.QueryRowContext(ctx, `SELECT outcome,record_count,item_count,signal FROM observability_export_batches WHERE batch_id=$1 FOR UPDATE`, batch.BatchID).Scan(&outcome, &count, &itemCount, &signal); err != nil {
			return err
		}
		if outcome != "unknown" || count != batch.RecordCount || itemCount != len(batch.Items) || signal != batch.Signal {
			return core.ErrConflict
		}
		maxAttempts, maxAge := batch.MaxAttempts, batch.MaxItemAge
		if maxAttempts == 0 {
			maxAttempts = 10
		}
		if maxAge == 0 {
			maxAge = 24 * time.Hour
		}
		for _, item := range obsSortedItems(batch.Items) {
			var created int64
			err := tx.QueryRowContext(ctx, `SELECT created_at FROM observability_export_items WHERE item_id=$1 AND state='inflight' AND attempts=$2 AND last_batch_id=$3 FOR UPDATE`, item.ItemID, item.Attempts, batch.BatchID).Scan(&created)
			if errors.Is(err, sql.ErrNoRows) {
				return core.ErrConflict
			}
			if err != nil {
				return err
			}
			state, kind := disposition.State, disposition.ErrorKind
			var next, sent any
			if state == observability.ExportRetry {
				if item.Attempts >= maxAttempts || now.Sub(time.UnixMilli(created)) >= maxAge {
					state, kind = observability.ExportFailedPermanent, "retry_exhausted"
				} else {
					next = now.Add(obsRetryDelay(item.Attempts, res.RetryAfter)).UnixMilli()
				}
			}
			if state == observability.ExportSent {
				sent = now.UnixMilli()
			}
			result, err := tx.ExecContext(ctx, `UPDATE observability_export_items SET state=$1,last_error_kind=$2,updated_at=$3,lease_until=NULL,next_attempt_at=$4,sent_at=$5
				WHERE item_id=$6 AND state='inflight' AND attempts=$7 AND last_batch_id=$8`, state, nullIfEmpty(kind), now.UnixMilli(), next, sent, item.ItemID, item.Attempts, batch.BatchID)
			if err != nil {
				return err
			}
			if err := obsRequireOne(result); err != nil {
				return err
			}
		}
		var accepted, rejected, status, duration any
		if disposition.Accepted != nil {
			accepted = *disposition.Accepted
			rejected = *disposition.Rejected
		}
		if res.HTTPStatus >= 100 && res.HTTPStatus <= 599 {
			status = res.HTTPStatus
		}
		if res.Duration >= 0 && res.Duration <= time.Hour {
			duration = res.Duration.Milliseconds()
		}
		result, err := tx.ExecContext(ctx, `UPDATE observability_export_batches SET accepted_records=$1,rejected_records=$2,partial_success=$3,warning_present=$4,warning_text=$5,
			outcome=$6,http_status=$7,error_kind=$8,duration_ms=$9 WHERE batch_id=$10 AND outcome='unknown'`, accepted, rejected, res.PartialSuccess, res.Warning != "", nullIfEmpty(disposition.Warning), disposition.Outcome, status, nullIfEmpty(disposition.ErrorKind), duration, batch.BatchID)
		if err != nil {
			return err
		}
		return obsRequireOne(result)
	})
}

func obsRetryDelay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(max(retryAfter, time.Second), 5*time.Minute)
	}
	base := time.Second << min(max(attempt-1, 0), 8)
	fraction := 0.5
	if value, err := rand.Int(rand.Reader, big.NewInt(1000001)); err == nil {
		fraction = float64(value.Int64()) / 1000000
	}
	jitter := time.Duration(float64(base) * (0.8 + 0.4*fraction))
	return min(max(jitter, time.Second), 5*time.Minute)
}

// PendingCount 只计可处理积压，不把unconfirmed算作重试队列。
// PendingCount excludes unconfirmed items from the retry backlog.
func (s *ObservabilityStore) PendingCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM observability_export_items WHERE state IN ('pending','inflight','retry')`).Scan(&count)
	return count, err
}

// OldestPendingAge 使用同一个句柄时钟，返回数据库派生读数。
// OldestPendingAge derives age using the shared handle clock.
func (s *ObservabilityStore) OldestPendingAge(ctx context.Context) (time.Duration, error) {
	var age int64
	err := s.db.pool.QueryRowContext(ctx, `SELECT GREATEST(COALESCE($1::bigint-min(created_at),0),0) FROM observability_export_items WHERE state IN ('pending','inflight','retry')`, s.clock().UnixMilli()).Scan(&age)
	return time.Duration(age) * time.Millisecond, err
}

// UnconfirmedCount 表达逐条结果无法确认，不能读成丢失条数。
// UnconfirmedCount means per-item outcomes are unknown, not that all items were lost.
func (s *ObservabilityStore) UnconfirmedCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM observability_export_items WHERE state='unconfirmed'`).Scan(&count)
	return count, err
}

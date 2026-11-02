package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// MetricsExporter 持久化原派生快照，不直接访问网络。
// MetricsExporter persists the original derived snapshot without network I/O.
type MetricsExporter struct {
	mu        sync.Mutex
	collector *MetricsCollector
	ledger    ExportLedger
	interval  time.Duration
	clock     func() time.Time
	last      time.Time
	pending   *ExportItem
}

// NewMetricsExporter 仅装配依赖，固定导出15m窗口并保留进程累计起点。
// NewMetricsExporter assembles a fixed 15m window and preserves cumulative process start.
func NewMetricsExporter(collector *MetricsCollector, ledger ExportLedger, interval time.Duration, clock func() time.Time) *MetricsExporter {
	if interval == 0 {
		interval = time.Minute
	}
	if clock == nil {
		clock = time.Now
	}
	return &MetricsExporter{collector: collector, ledger: ledger, interval: interval, clock: clock}
}

// EnqueueOnce 入队失败时保留原快照，重试不重算同一个数据点。
// EnqueueOnce retains the original snapshot across failed enqueue attempts.
func (e *MetricsExporter) EnqueueOnce(ctx context.Context) error {
	if e == nil || e.collector == nil || e.ledger == nil || e.interval <= 0 {
		return ErrInvalidQuery
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := e.clock()
	if now.IsZero() {
		return ErrInvalidQuery
	}
	if e.pending == nil {
		if !e.last.IsZero() && now.Sub(e.last) < e.interval {
			return nil
		}
		snapshot, err := e.collector.Snapshot(ctx, 15*time.Minute)
		if err != nil {
			return fmt.Errorf("collect metric snapshot: %w", err)
		}
		if _, err := mapSnapshot(snapshot); err != nil {
			return fmt.Errorf("validate metric snapshot: %w", err)
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			return fmt.Errorf("encode metric snapshot: %w", err)
		}
		hash, err := CanonicalStateHash(snapshot)
		if err != nil {
			return err
		}
		item := ExportItem{SourceKind: SourceMetricSnapshot, SourceID: fmt.Sprintf("15m:%d", snapshot.CollectedAt.UnixNano()), StateHash: hash, Signal: SignalMetric, SourceTerminalAt: snapshot.CollectedAt, Payload: raw}
		item.ItemID = ExportItemKey(item.SourceKind, item.SourceID, item.StateHash)
		if err := item.Validate(); err != nil {
			return fmt.Errorf("validate metric export item: %w", err)
		}
		e.pending = &item
	}
	if err := e.ledger.Enqueue(ctx, []ExportItem{*e.pending}); err != nil {
		return fmt.Errorf("enqueue metric snapshot: %w", err)
	}
	e.last = e.pending.SourceTerminalAt
	e.pending = nil
	return nil
}

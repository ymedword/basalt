package observability

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type exportReloader interface {
	Reload(context.Context, ExportItem) (ExportItem, bool, error)
}

type exportFailureStore interface {
	FailExportItems(context.Context, []ExportItem, string, time.Time) error
}

// ExportWorker 只操作观测端口；构造不启动后台任务。
// ExportWorker uses observation ports only and starts no work in its constructor.
type ExportWorker struct {
	mu      sync.Mutex
	scanner ExportScanner
	ledger  ExportLedger
	sender  Sender
	cfg     ExportConfig
	clock   func() time.Time
}

// NewExportWorker 显式注入依赖，不接收任何业务执行服务。
// NewExportWorker injects dependencies without any business execution service.
func NewExportWorker(scanner ExportScanner, ledger ExportLedger, sender Sender, cfg ExportConfig, clock func() time.Time) *ExportWorker {
	if clock == nil {
		clock = time.Now
	}
	if cfg == (ExportConfig{}) {
		cfg = DefaultExportConfig()
	}
	defaults := DefaultExportConfig()
	if cfg.BatchSize == 0 {
		cfg.BatchSize = defaults.BatchSize
	}
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = defaults.LeaseTTL
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = defaults.MaxAttempts
	}
	if cfg.MaxItemAge == 0 {
		cfg.MaxItemAge = defaults.MaxItemAge
	}
	return &ExportWorker{scanner: scanner, ledger: ledger, sender: sender, cfg: cfg, clock: clock}
}

// ExportOnce 扫描、重读、尝试、发送和确认均保持显式失败边界。
// ExportOnce preserves separate failure boundaries for scanning, reload, attempt, send, and acknowledgment.
func (w *ExportWorker) ExportOnce(ctx context.Context) (ExportResult, error) {
	var total ExportResult
	if w == nil || w.scanner == nil || w.ledger == nil || w.sender == nil || w.cfg.BatchSize < 1 || w.cfg.BatchSize > 2048 || w.cfg.LeaseTTL <= 0 || w.cfg.LeaseTTL > time.Hour || w.cfg.MaxAttempts < 1 || w.cfg.MaxItemAge <= 0 {
		return total, ErrInvalidQuery
	}
	if !w.mu.TryLock() {
		return total, errors.New("export worker busy")
	}
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return total, err
	}
	reloader, ok := w.scanner.(exportReloader)
	if !ok {
		reloader, ok = w.ledger.(exportReloader)
	}
	if !ok {
		return total, errors.New("export reloader unavailable")
	}
	if _, err := w.scanner.Scan(ctx, w.clock()); err != nil {
		return total, fmt.Errorf("scan export sources: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return total, err
	}
	items, err := w.ledger.Claim(ctx, w.cfg.BatchSize, w.clock(), w.cfg.LeaseTTL)
	if err != nil {
		return total, fmt.Errorf("claim export items: %w", err)
	}
	total.Claimed = len(items)
	for _, signal := range []SignalKind{SignalTrace, SignalLog, SignalMetric} {
		var current, stale, exhausted []ExportItem
		for _, item := range items {
			if item.Signal != signal {
				continue
			}
			if err := ctx.Err(); err != nil {
				return total, err
			}
			if item.Attempts > w.cfg.MaxAttempts || !item.CreatedAt.IsZero() && w.clock().Sub(item.CreatedAt) >= w.cfg.MaxItemAge {
				exhausted = append(exhausted, item)
				continue
			}
			fresh, exists, err := reloader.Reload(ctx, item)
			if err != nil {
				return total, fmt.Errorf("reload export item: %w", err)
			}
			if !exists || fresh.StateHash != item.StateHash || fresh.Signal != item.Signal || fresh.SourceKind != item.SourceKind || fresh.SourceID != item.SourceID {
				stale = append(stale, item)
				continue
			}
			// 保留原claim的身份/租约/attempts，只替换刚重读的临时事实。
			// Retain the original claim identity, lease and attempt, replacing only ephemeral facts.
			item.Fact, item.Payload = fresh.Fact, fresh.Payload
			current = append(current, item)
		}
		if len(stale) > 0 {
			if err := w.ledger.MarkStale(ctx, stale); err != nil {
				return total, fmt.Errorf("mark stale export items: %w", err)
			}
			total.Stale += len(stale)
		}
		if len(exhausted) > 0 {
			if err := w.failItems(ctx, exhausted, "retry_exhausted"); err != nil {
				return total, err
			}
			total.FailedPermanent += len(exhausted)
		}
		if len(current) == 0 {
			continue
		}
		batch, bad := (&Mapper{}).BuildBatch(current)
		if len(bad) > 0 {
			valid := map[string]bool{}
			for _, item := range batch.Items {
				valid[item.ItemID] = true
			}
			var failed []ExportItem
			for _, item := range current {
				if !valid[item.ItemID] {
					failed = append(failed, item)
				}
			}
			if err := w.failItems(ctx, failed, "mapping_error"); err != nil {
				return total, err
			}
			total.FailedPermanent += len(failed)
		}
		if len(batch.Items) == 0 {
			continue
		}
		batch.MaxAttempts, batch.MaxItemAge = w.cfg.MaxAttempts, w.cfg.MaxItemAge
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if err := w.ledger.BeginBatch(ctx, batch, w.clock()); err != nil {
			return total, fmt.Errorf("record export attempt: %w", err)
		}
		result, sendErr := w.sender.Send(ctx, batch)
		if err := w.ledger.ApplySendResult(ctx, batch, result, sendErr, w.clock()); err != nil {
			return total, fmt.Errorf("acknowledge export attempt: %w", err)
		}
		total.Batches++
		switch ResolveSendResult(batch, result, sendErr).State {
		case ExportSent:
			total.Sent += len(batch.Items)
		case ExportUnconfirmed:
			total.Unconfirmed += len(batch.Items)
		case ExportFailedPermanent:
			total.FailedPermanent += len(batch.Items)
		case ExportRetry:
			for _, item := range batch.Items {
				if item.Attempts >= w.cfg.MaxAttempts || !item.CreatedAt.IsZero() && w.clock().Sub(item.CreatedAt) >= w.cfg.MaxItemAge {
					total.FailedPermanent++
				} else {
					total.Retried++
				}
			}
		}
	}
	return total, nil
}

func (w *ExportWorker) failItems(ctx context.Context, items []ExportItem, kind string) error {
	store, ok := w.ledger.(exportFailureStore)
	if !ok {
		return errors.New("export failure store unavailable")
	}
	if err := store.FailExportItems(ctx, items, kind, w.clock()); err != nil {
		return fmt.Errorf("isolate export items: %w", err)
	}
	return nil
}

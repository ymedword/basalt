package observability

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ScanCursor 是终态时间和来源ID的键集位置，不是全局递增水位。
// ScanCursor is a terminal-time/source-id position, never a global ID watermark.
type ScanCursor struct {
	TerminalAt time.Time
	SourceID   string
}

// SourcePage 将原查询游标与重读后的当前事实分开，删除/变更不会造成假分页完成。
// SourcePage separates the query cursor from reloaded facts so changes cannot fake completion.
type SourcePage struct {
	Items    []ExportItem
	Cursor   ScanCursor
	Scanned  int
	Complete bool
}

// ExportSourceStore 由消费侧声明，所有SQL仍留在PostgreSQL适配层。
// ExportSourceStore is consumer-declared; all SQL stays in the PostgreSQL adapter.
type ExportSourceStore interface {
	ScanCoverage(context.Context, string) (time.Time, time.Time, bool, error)
	ScanPage(context.Context, SourceKind, time.Time, time.Time, ScanCursor, int) (SourcePage, error)
	EnqueuePage(context.Context, []ExportItem) (int, error)
	AdvanceScanCoverage(context.Context, SourceKind, time.Time, time.Time, time.Time, time.Time, time.Time) (bool, error)
	StartScan(context.Context, SourceKind, time.Time) error
}

// Scanner 在完整有界窗口成功后才推进覆盖；对账不扩张覆盖起点。
// Scanner advances coverage only after complete windows; reconciliation never widens the lower bound.
type Scanner struct {
	source ExportSourceStore
	cfg    ExportConfig
}

// NewScanner 只装配策略，不启动任务或网络请求。
// NewScanner assembles policy without starting work or network requests.
func NewScanner(source ExportSourceStore, cfg ExportConfig) *Scanner {
	if cfg == (ExportConfig{}) {
		cfg = DefaultExportConfig()
	}
	if cfg.ScanPageSize == 0 {
		cfg.ScanPageSize = 500
	}
	if cfg.InitialBackfill == 0 {
		cfg.InitialBackfill = 24 * time.Hour
	}
	if cfg.ReconcileHorizon == 0 {
		cfg.ReconcileHorizon = 24 * time.Hour
	}
	return &Scanner{source: source, cfg: cfg}
}

// Reload 复用同一来源的窄重读端口，不向worker暴露存储句柄。
// Reload forwards the narrow source reload port without exposing a storage handle.
func (s *Scanner) Reload(ctx context.Context, item ExportItem) (ExportItem, bool, error) {
	if s == nil || s.source == nil {
		return ExportItem{}, false, ErrInvalidQuery
	}
	source, ok := s.source.(exportReloader)
	if !ok {
		return ExportItem{}, false, errors.New("export source reload unavailable")
	}
	return source.Reload(ctx, item)
}

// Scan 保留分页失败前的已提交入队，失败窗口不推进覆盖。
// Scan keeps committed pages but never advances a failed window.
func (s *Scanner) Scan(ctx context.Context, now time.Time) (ScanResult, error) {
	out := ScanResult{CoveredFrom: map[SourceKind]time.Time{}, CoveredThrough: map[SourceKind]time.Time{}}
	if s == nil || s.source == nil || now.IsZero() || s.cfg.ScanPageSize < 1 || s.cfg.ScanPageSize > 10000 || s.cfg.SafetyLag < 0 || s.cfg.ScanOverlap < 0 || s.cfg.InitialBackfill <= s.cfg.SafetyLag || s.cfg.ReconcileHorizon <= 0 {
		return out, ErrInvalidQuery
	}
	through := now.Add(-s.cfg.SafetyLag)
	for _, kind := range []SourceKind{SourceRun, SourceLLM, SourceTool, SourceEmbedding, SourceTask, SourceOperation, SourceIngest, SourceDiagnostic} {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if err := s.source.StartScan(ctx, kind, now); err != nil {
			return out, fmt.Errorf("start export scan: %w", err)
		}
		oldFrom, oldThrough, covered, err := s.source.ScanCoverage(ctx, string(kind))
		if err != nil {
			return out, fmt.Errorf("read export scan coverage: %w", err)
		}
		from, incrementalFrom := now.Add(-s.cfg.InitialBackfill), now.Add(-s.cfg.InitialBackfill)
		if covered {
			if oldFrom.After(oldThrough) {
				return out, errors.New("invalid export scan coverage")
			}
			out.CoveredFrom[kind], out.CoveredThrough[kind] = oldFrom, oldThrough
			if through.Before(oldThrough) {
				continue
			}
			from = oldFrom
			incrementalFrom = oldThrough.Add(-s.cfg.ScanOverlap)
			if incrementalFrom.Before(from) {
				incrementalFrom = from
			}
		}
		n, scanned, err := s.window(ctx, kind, incrementalFrom, through)
		out.Enqueued, out.Scanned = out.Enqueued+n, out.Scanned+scanned
		if err != nil {
			return out, err
		}
		if covered {
			reconcileFrom := now.Add(-s.cfg.ReconcileHorizon)
			if reconcileFrom.Before(from) {
				reconcileFrom = from
			}
			n, scanned, err = s.window(ctx, kind, reconcileFrom, through)
			out.Enqueued, out.ReconcileScanned = out.Enqueued+n, out.ReconcileScanned+scanned
			if err != nil {
				return out, err
			}
		}
		advanced, err := s.source.AdvanceScanCoverage(ctx, kind, oldFrom, oldThrough, from, through, now)
		if err != nil {
			return out, fmt.Errorf("advance export scan coverage: %w", err)
		}
		if !advanced {
			return out, errors.New("export scan coverage conflict")
		}
		out.CoveredFrom[kind], out.CoveredThrough[kind] = from, through
	}
	return out, nil
}

func (s *Scanner) window(ctx context.Context, kind SourceKind, from, through time.Time) (enqueued, scanned int, err error) {
	if from.After(through) {
		return 0, 0, nil
	}
	var cursor ScanCursor
	for {
		if err := ctx.Err(); err != nil {
			return enqueued, scanned, err
		}
		page, err := s.source.ScanPage(ctx, kind, from, through, cursor, s.cfg.ScanPageSize)
		if err != nil {
			return enqueued, scanned, fmt.Errorf("read export source page: %w", err)
		}
		if page.Scanned < 0 || page.Scanned > s.cfg.ScanPageSize || len(page.Items) > page.Scanned || !page.Complete && page.Scanned == 0 || page.Scanned > 0 && (page.Cursor.TerminalAt.Before(from) || page.Cursor.TerminalAt.After(through) || page.Cursor.TerminalAt.Before(cursor.TerminalAt) || page.Cursor.TerminalAt.Equal(cursor.TerminalAt) && page.Cursor.SourceID <= cursor.SourceID) {
			return enqueued, scanned, errors.New("invalid export source page")
		}
		scanned += page.Scanned
		items := make([]ExportItem, 0, len(page.Items))
		for _, item := range page.Items {
			if !pendingExportFact(item.Fact) {
				items = append(items, item)
			}
		}
		n, err := s.source.EnqueuePage(ctx, items)
		if err != nil {
			return enqueued, scanned, fmt.Errorf("enqueue export source page: %w", err)
		}
		enqueued += n
		if page.Complete {
			return enqueued, scanned, nil
		}
		cursor = page.Cursor
	}
}

func pendingExportFact(fact any) bool {
	switch f := fact.(type) {
	case RunFacts:
		return f.Status == "running"
	case LLMCallFact:
		return f.Status == "pending"
	case ToolInvocationFact:
		return f.Status == "pending"
	case EmbeddingCallFact:
		return f.Status == "pending"
	case TaskExecutionFact:
		return f.Status == "started"
	case ManagementOperationFact:
		return f.Status == "pending"
	case IngestJobFact:
		return f.Status == "pending" || f.Status == "running"
	default:
		return false
	}
}

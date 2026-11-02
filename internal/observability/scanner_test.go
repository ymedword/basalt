package observability

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type scannerSource struct {
	rows     map[SourceKind][]ExportItem
	coverage map[string][2]time.Time
	items    map[string]ExportItem
	failPage int
	pages    int
}

func newScannerSource() *scannerSource {
	return &scannerSource{rows: map[SourceKind][]ExportItem{}, coverage: map[string][2]time.Time{}, items: map[string]ExportItem{}}
}

func (s *scannerSource) ScanCoverage(_ context.Context, table string) (time.Time, time.Time, bool, error) {
	c, ok := s.coverage[table]
	return c[0], c[1], ok, nil
}
func (s *scannerSource) StartScan(context.Context, SourceKind, time.Time) error { return nil }
func (s *scannerSource) AdvanceScanCoverage(_ context.Context, kind SourceKind, oldFrom, oldThrough, from, through, _ time.Time) (bool, error) {
	c := s.coverage[string(kind)]
	if !c[0].Equal(oldFrom) || !c[1].Equal(oldThrough) {
		return false, nil
	}
	s.coverage[string(kind)] = [2]time.Time{from, through}
	return true, nil
}

func (s *scannerSource) EnqueuePage(_ context.Context, items []ExportItem) (int, error) {
	count := 0
	for _, item := range items {
		key := string(item.SourceKind) + ":" + item.SourceID + ":" + item.StateHash
		if _, exists := s.items[key]; !exists {
			s.items[key] = item
			count++
		}
	}
	return count, nil
}

func (s *scannerSource) ScanPage(_ context.Context, kind SourceKind, from, through time.Time, cursor ScanCursor, limit int) (SourcePage, error) {
	s.pages++
	if s.failPage != 0 && s.pages == s.failPage {
		return SourcePage{}, errors.New("injected page failure")
	}
	rows := slices.Clone(s.rows[kind])
	slices.SortFunc(rows, func(a, b ExportItem) int {
		if order := a.SourceTerminalAt.Compare(b.SourceTerminalAt); order != 0 {
			return order
		}
		return strings.Compare(a.SourceID, b.SourceID)
	})
	selected := []ExportItem{}
	for _, item := range rows {
		if item.SourceTerminalAt.Before(from) || item.SourceTerminalAt.After(through) {
			continue
		}
		if !cursor.TerminalAt.IsZero() && (item.SourceTerminalAt.Before(cursor.TerminalAt) || item.SourceTerminalAt.Equal(cursor.TerminalAt) && item.SourceID <= cursor.SourceID) {
			continue
		}
		selected = append(selected, item)
	}
	done := len(selected) < limit
	if len(selected) > limit {
		selected = selected[:limit]
	}
	next := cursor
	if len(selected) > 0 {
		last := selected[len(selected)-1]
		next = ScanCursor{TerminalAt: last.SourceTerminalAt, SourceID: last.SourceID}
	}
	return SourcePage{Items: selected, Cursor: next, Scanned: len(selected), Complete: done}, nil
}

func scannerConfig() ExportConfig {
	cfg := DefaultExportConfig()
	cfg.SafetyLag = 0
	cfg.ScanPageSize = 2
	return cfg
}

func TestObservability_ExportLongRunEndsAfterWindow(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	source := newScannerSource()
	source.rows[SourceRun] = []ExportItem{{SourceKind: SourceRun, SourceID: "long-run", StateHash: "terminal-state", SourceTerminalAt: now.Add(10 * time.Hour)}}
	scanner := NewScanner(source, scannerConfig())
	first, err := scanner.Scan(t.Context(), now)
	require.NoError(t, err)
	require.Zero(t, first.Enqueued)
	second, err := scanner.Scan(t.Context(), now.Add(11*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, second.Enqueued)
	require.Len(t, source.items, 1)
}

func TestObservability_ExportNeverBackfilledHistory(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	source := newScannerSource()
	source.rows[SourceRun] = []ExportItem{{SourceKind: SourceRun, SourceID: "old", StateHash: "old", SourceTerminalAt: now.Add(-48 * time.Hour)}}
	result, err := NewScanner(source, scannerConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	require.Zero(t, result.Enqueued)
	require.Equal(t, now.Add(-24*time.Hour), result.CoveredFrom[SourceRun])
	require.Empty(t, source.items)
}

func TestObservability_ExportLateCommitInsideCoverage(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	source := newScannerSource()
	scanner := NewScanner(source, scannerConfig())
	_, err := scanner.Scan(t.Context(), now)
	require.NoError(t, err)
	source.rows[SourceRun] = []ExportItem{{SourceKind: SourceRun, SourceID: "late", StateHash: "late", SourceTerminalAt: now.Add(-time.Hour)}}
	result, err := scanner.Scan(t.Context(), now.Add(10*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, result.Enqueued)
	require.Greater(t, result.ReconcileScanned, 0)
}

func TestObservability_ExportPaginationInterrupted(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	source := newScannerSource()
	source.failPage = 2
	for _, id := range []string{"a", "b", "c"} {
		source.rows[SourceRun] = append(source.rows[SourceRun], ExportItem{SourceKind: SourceRun, SourceID: id, StateHash: id, SourceTerminalAt: now})
	}
	scanner := NewScanner(source, scannerConfig())
	_, err := scanner.Scan(t.Context(), now)
	require.Error(t, err)
	require.NotContains(t, source.coverage, string(SourceRun))
	require.Len(t, source.items, 2)
	source.failPage = 0
	result, err := scanner.Scan(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 1, result.Enqueued)
	require.Len(t, source.items, 3)
	require.Equal(t, now, result.CoveredThrough[SourceRun])
}

func TestObservability_ExportSameTimestampPages(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	source := newScannerSource()
	for _, id := range []string{"10", "2", "1", "a", "A"} {
		source.rows[SourceRun] = append(source.rows[SourceRun], ExportItem{SourceKind: SourceRun, SourceID: id, StateHash: id, SourceTerminalAt: now})
	}
	result, err := NewScanner(source, scannerConfig()).Scan(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 5, result.Enqueued)
	require.Len(t, source.items, 5)
}

func TestObservability_ExportRecoveredOperationTerminalTime(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	source := newScannerSource()
	scanner := NewScanner(source, scannerConfig())
	_, err := scanner.Scan(t.Context(), now)
	require.NoError(t, err)
	finish := now.Add(time.Hour)
	f := ManagementOperationFact{OperationID: "recovered-op", Status: "unknown", CreatedAt: now.Add(-72 * time.Hour), FinishedAt: &finish, RecoveredAt: &finish}
	item := mapperItem(t, SourceOperation, "recovered-op", SignalTrace, f)
	item.SourceTerminalAt = finish
	source.rows[SourceOperation] = []ExportItem{item}
	result, err := scanner.Scan(t.Context(), finish)
	require.NoError(t, err)
	require.Equal(t, 1, result.Enqueued)
	batch, errs := (&Mapper{}).BuildBatch([]ExportItem{item})
	require.Empty(t, errs)
	span := batch.Payload.(TelemetryPayload).Spans[0]
	require.Equal(t, f.CreatedAt, span.Start)
	require.Equal(t, finish, span.End)
	require.Equal(t, 2, span.StatusCode)
}

func TestObservability_ExportPendingNotEnqueued(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	source := newScannerSource()
	for kind, fact := range map[SourceKind]any{
		SourceRun: RunFacts{RunID: "not-terminal", Status: "running"},
		SourceLLM: LLMCallFact{Status: "pending"}, SourceTool: ToolInvocationFact{Status: "pending"},
		SourceEmbedding: EmbeddingCallFact{Status: "pending"}, SourceTask: TaskExecutionFact{Status: "started"},
		SourceOperation: ManagementOperationFact{Status: "pending"}, SourceIngest: IngestJobFact{Status: "running"},
	} {
		source.rows[kind] = []ExportItem{{SourceKind: kind, SourceID: "not-terminal", SourceTerminalAt: now, StateHash: "pending", Fact: fact}}
	}
	scanner := NewScanner(source, scannerConfig())
	result, err := scanner.Scan(t.Context(), now)
	require.NoError(t, err)
	require.Zero(t, result.Enqueued)
	require.Empty(t, source.items)
	require.Equal(t, 7, result.Scanned, "pending reloads cannot be queued even if a source page supplied them")
	f := RunFacts{RunID: "not-terminal", Status: "completed", StartedAt: now, FinishedAt: &now}
	source.rows[SourceRun] = []ExportItem{{SourceKind: SourceRun, SourceID: f.RunID, SourceTerminalAt: now, StateHash: "completed", Fact: f}}
	result, err = scanner.Scan(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, 1, result.Enqueued)
}

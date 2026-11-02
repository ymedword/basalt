package observability

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type retentionTestStore struct {
	runs                  []CleanupRun
	from, through         time.Time
	covered               bool
	coverageErr, batchErr error
	requests              []CleanupRequest
	skips                 []string
	remaining             int64
}

func (s *retentionTestStore) ScanCoverage(context.Context, string) (time.Time, time.Time, bool, error) {
	return s.from, s.through, s.covered, s.coverageErr
}

func (s *retentionTestStore) RecordSkipped(_ context.Context, _ string, _ time.Time, reason string) error {
	s.skips = append(s.skips, reason)
	return nil
}

func (s *retentionTestStore) CleanupBatch(_ context.Context, req CleanupRequest) (CleanupRun, error) {
	s.requests = append(s.requests, req)
	if s.batchErr != nil {
		return CleanupRun{}, s.batchErr
	}
	n := min(int64(req.Limit), s.remaining)
	s.remaining -= n
	run := CleanupRun{Deleted: n, TableKind: req.Table, WindowStart: req.Cutoff.Add(-time.Hour), WindowEnd: req.Cutoff, Status: "succeeded"}
	if n > 0 {
		run.Next = &CleanupCursor{At: req.Cutoff.Add(-time.Second), ID: string(rune('a' + len(s.requests)))}
	}
	s.runs = append(s.runs, run)
	return run, nil
}

func (s *retentionTestStore) CleanupRuns(context.Context, time.Time, time.Time) ([]CleanupRun, error) {
	return s.runs, nil
}

func retentionTestPolicy() RetentionPolicy {
	return RetentionPolicy{Table: "diagnostic_events", ExportSource: true, Enabled: true, Retention: time.Hour, BatchSize: 2, MaxBatches: 3}
}

func TestObservability_CleanupDisabled(t *testing.T) {
	s := &retentionTestStore{remaining: 10}
	result, err := NewRetentionCleaner(s, nil, true).CleanupOnce(t.Context(), time.Now())
	require.NoError(t, err)
	require.Zero(t, result.Deleted)
	require.Empty(t, s.requests)
	require.Empty(t, s.skips)
	for _, policy := range DefaultRetentionPolicies() {
		require.False(t, policy.Enabled)
	}
}

func TestObservability_CleanupBatches(t *testing.T) {
	s := &retentionTestStore{remaining: 9}
	result, err := NewRetentionCleaner(s, []RetentionPolicy{retentionTestPolicy()}, false).CleanupOnce(t.Context(), time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(6), result.Deleted)
	require.Len(t, s.requests, 3)
	require.Equal(t, int64(3), s.remaining)
	require.Nil(t, s.requests[0].Cursor)
	require.NotNil(t, s.requests[1].Cursor)
	require.NotNil(t, s.requests[2].Cursor)
	for _, req := range s.requests {
		require.False(t, req.Exporting)
		require.False(t, req.RequireConfirmedExport)
	}
}

func TestObservability_CleanupSkipsUncoveredWindow(t *testing.T) {
	s := &retentionTestStore{remaining: 3}
	result, err := NewRetentionCleaner(s, []RetentionPolicy{retentionTestPolicy()}, true).CleanupOnce(t.Context(), time.Now())
	require.NoError(t, err)
	require.Zero(t, result.Deleted)
	require.Empty(t, s.requests)
	require.Equal(t, []string{"scan_not_covered"}, s.skips)
}

func TestObservability_CleanupBeforeScanDoesNotDelete(t *testing.T) {
	s := &retentionTestStore{remaining: 3, coverageErr: errors.New("synthetic coverage read failure")}
	_, err := NewRetentionCleaner(s, []RetentionPolicy{retentionTestPolicy()}, true).CleanupOnce(t.Context(), time.Now())
	require.Error(t, err)
	require.Empty(t, s.requests)
	require.Equal(t, int64(3), s.remaining)
}

func TestObservability_CleanupNeverBackfilledNotDeleted(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	s := &retentionTestStore{remaining: 3, covered: true, from: now.Add(-time.Minute), through: now}
	result, err := NewRetentionCleaner(s, []RetentionPolicy{retentionTestPolicy()}, true).CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Zero(t, result.Deleted)
	require.Empty(t, s.requests)
	require.Equal(t, []string{"out_of_coverage"}, s.skips)
}

func TestObservability_CleanupRestart(t *testing.T) {
	s := &retentionTestStore{remaining: 9}
	for range 2 {
		_, err := NewRetentionCleaner(s, []RetentionPolicy{retentionTestPolicy()}, false).CleanupOnce(t.Context(), time.Now())
		require.NoError(t, err)
	}
	require.Zero(t, s.remaining)
	require.Nil(t, s.requests[3].Cursor, "restart begins a fresh bounded pass, committed deletions stay absent")
}

func TestObservability_CleanupRequiresConfirmedExport(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	s := &retentionTestStore{remaining: 1, covered: true, from: now.Add(-2 * time.Hour), through: now}
	_, err := NewRetentionCleaner(s, []RetentionPolicy{retentionTestPolicy()}, true).CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, s.requests, 1)
	req := s.requests[0]
	require.True(t, req.Exporting)
	require.True(t, req.RequireConfirmedExport)
	require.Equal(t, s.from, req.CoveredFrom)
	require.Equal(t, s.through, req.CoveredThrough)
	p := retentionTestPolicy()
	p.Table = "context_assembly_records"
	p.ExportSource = false
	s = &retentionTestStore{remaining: 1, coverageErr: errors.New("must not query assembly coverage")}
	_, err = NewRetentionCleaner(s, []RetentionPolicy{p}, true).CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.False(t, s.requests[0].RequireConfirmedExport)
}

func TestObservability_CleanupEnabledShortRetention(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	s := &retentionTestStore{remaining: 1}
	policy := retentionTestPolicy()
	policy.Retention = time.Millisecond
	result, err := NewRetentionCleaner(s, []RetentionPolicy{policy}, false).CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, int64(1), result.Deleted)
	require.Zero(t, s.remaining)
	require.Equal(t, now.Add(-time.Millisecond), s.requests[0].Cutoff)
}

type retentionRollbackStore struct{ *retentionTestStore }

func (s retentionRollbackStore) CleanupBatch(ctx context.Context, req CleanupRequest) (CleanupRun, error) {
	s.requests = append(s.requests, req)
	return CleanupRun{Deleted: 7, Skipped: 3}, errors.New("synthetic rolled-back transaction")
}

func TestObservability_CleanupLedgerAtomic(t *testing.T) {
	s := &retentionTestStore{remaining: 7}
	result, err := NewRetentionCleaner(retentionRollbackStore{s}, []RetentionPolicy{retentionTestPolicy()}, false).CleanupOnce(t.Context(), time.Now())
	require.Error(t, err)
	require.Zero(t, result.Deleted)
	require.Zero(t, result.Skipped)
	require.Equal(t, int64(7), s.remaining)
}

type retentionBlockedStore struct {
	*retentionTestStore
	reason string
}

func (s retentionBlockedStore) CleanupBatch(_ context.Context, req CleanupRequest) (CleanupRun, error) {
	s.requests = append(s.requests, req)
	if !req.RequireConfirmedExport {
		return CleanupRun{}, errors.New("confirmation guard missing")
	}
	return CleanupRun{Skipped: 1, SkipReason: s.reason, Status: "skipped"}, nil
}

func TestObservability_CleanupExportConflict(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	s := &retentionTestStore{remaining: 1, covered: true, from: now.Add(-2 * time.Hour), through: now}
	result, err := NewRetentionCleaner(retentionBlockedStore{s, "unconfirmed_export"}, []RetentionPolicy{retentionTestPolicy()}, true).CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Zero(t, result.Deleted)
	require.Equal(t, int64(1), result.Skipped)
	require.Equal(t, int64(1), s.remaining)
	require.True(t, s.requests[0].RequireConfirmedExport)
	require.True(t, s.requests[0].ExportEnabled)
}

func TestObservability_ExportRetentionSkipsPending(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	s := &retentionTestStore{remaining: 1, covered: true, from: now.Add(-2 * time.Hour), through: now}
	result, err := NewRetentionCleaner(retentionBlockedStore{s, "unexported_backlog"}, []RetentionPolicy{retentionTestPolicy()}, true).CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Zero(t, result.Deleted)
	require.Equal(t, int64(1), result.Skipped)
	require.Equal(t, int64(1), s.remaining)
}

func TestObservability_CleanupEvidenceGap(t *testing.T) {
	f := sameMillisecondFacts()
	now := f.run.StartedAt.Add(2 * time.Hour)
	s := &retentionTestStore{remaining: 1}
	result, err := NewRetentionCleaner(s, []RetentionPolicy{retentionTestPolicy()}, false).CleanupOnce(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, int64(1), result.Deleted)
	ports := f.ports()
	ports.Cleanup = s
	timeline, err := NewProjector(ports, ProjectorConfig{SampleRate: 1}).Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: f.run.RunID}})
	require.NoError(t, err)
	found := false
	for _, gap := range timeline.Gaps {
		if gap.Source == SourceDiagnostic {
			require.Equal(t, GapRetentionWindowCleaned, gap.Reason)
			require.Contains(t, gap.Detail, "发生过")
			require.Contains(t, gap.Detail, "无法确认")
			require.NotContains(t, gap.Detail, "该记录已被清理")
			found = true
		}
	}
	require.True(t, found)
	s.runs[0].Deleted = 0
	timeline, err = NewProjector(ports, ProjectorConfig{SampleRate: 1}).Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: f.run.RunID}})
	require.NoError(t, err)
	for _, gap := range timeline.Gaps {
		if gap.Source == SourceDiagnostic {
			require.Equal(t, GapNotRecorded, gap.Reason)
		}
	}
}

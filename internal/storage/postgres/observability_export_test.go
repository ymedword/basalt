//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/observability"
)

func obsExportItem(t *testing.T, id string, now time.Time) observability.ExportItem {
	t.Helper()
	hash, err := observability.CanonicalStateHash(map[string]any{"id": id, "status": "completed"})
	require.NoError(t, err)
	return observability.ExportItem{ItemID: observability.ExportItemKey(observability.SourceRun, id, hash), SourceKind: observability.SourceRun, SourceID: id, StateHash: hash, Signal: observability.SignalTrace, SourceTerminalAt: now, State: observability.ExportPending}
}

func obsExportBatch(id string, items []observability.ExportItem) observability.ExportBatch {
	return observability.ExportBatch{BatchID: id, Signal: observability.SignalTrace, RecordUnit: "span", Items: items, RecordCount: len(items)}
}

func TestObservabilityStore_ExportLedger(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	db.now = func() time.Time { return now }
	s := NewObservabilityStore(db)
	item := obsExportItem(t, "synthetic-run", now)
	require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{item}))
	require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{item}))
	count, err := s.PendingCount(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	claimed, err := s.Claim(t.Context(), 10, now, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, 1, claimed[0].Attempts)
	batch := obsExportBatch("batch-retry", claimed)
	require.NoError(t, s.BeginBatch(t.Context(), batch, now))
	require.NoError(t, s.ApplySendResult(t.Context(), batch, observability.SendResult{Retryable: true, RetryAfter: time.Second, ErrorKind: "timeout"}, context.DeadlineExceeded, now))
	claimed, err = s.Claim(t.Context(), 10, now, 5*time.Minute)
	require.NoError(t, err)
	require.Empty(t, claimed)
	now = now.Add(10 * time.Minute)
	claimed, err = s.Claim(t.Context(), 10, now, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, 2, claimed[0].Attempts)
	batch = obsExportBatch("batch-sent", claimed)
	require.NoError(t, s.BeginBatch(t.Context(), batch, now))
	require.NoError(t, s.ApplySendResult(t.Context(), batch, observability.SendResult{Accepted: 1}, nil, now))
	count, err = s.PendingCount(t.Context())
	require.NoError(t, err)
	require.Zero(t, count)
	var state string
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT state FROM observability_export_items`).Scan(&state))
	require.Equal(t, "sent", state)
}

func TestObservabilityStore_ConcurrentEnqueue(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	item := obsExportItem(t, "concurrent", time.Now())
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- s.Enqueue(t.Context(), []observability.ExportItem{item}) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_items`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestObservabilityStore_BatchAttemptUnknownCounts(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	now := time.Now().UTC()
	items := []observability.ExportItem{obsExportItem(t, "one", now), obsExportItem(t, "two", now), obsExportItem(t, "three", now)}
	require.NoError(t, s.Enqueue(t.Context(), items))
	claimed, err := s.Claim(t.Context(), 3, now, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 3)
	batch := obsExportBatch("batch-partial", claimed)
	require.NoError(t, s.BeginBatch(t.Context(), batch, now))
	var outcome string
	var accepted, rejected sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT outcome,accepted_records,rejected_records FROM observability_export_batches`).Scan(&outcome, &accepted, &rejected))
	require.Equal(t, "unknown", outcome)
	require.False(t, accepted.Valid)
	require.False(t, rejected.Valid)
	require.NoError(t, s.ApplySendResult(t.Context(), batch, observability.SendResult{Accepted: 2, Rejected: 1, PartialSuccess: true}, nil, now))
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT outcome,accepted_records,rejected_records FROM observability_export_batches`).Scan(&outcome, &accepted, &rejected))
	require.Equal(t, "partial_unconfirmed", outcome)
	require.True(t, accepted.Valid)
	require.True(t, rejected.Valid)
	require.Equal(t, int64(2), accepted.Int64)
	require.Equal(t, int64(1), rejected.Int64)
	count, err := s.UnconfirmedCount(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
	again, err := s.Claim(t.Context(), 3, now.Add(time.Hour), 5*time.Minute)
	require.NoError(t, err)
	require.Empty(t, again, "acknowledged partial batches must not be retried")
}

func TestObservabilityStore_PartialBatch(t *testing.T) {
	for _, c := range []struct {
		name           string
		result         observability.SendResult
		state, outcome string
		known          bool
	}{
		{"warning", observability.SendResult{Accepted: 1, PartialSuccess: true, Warning: "CANARY_RECEIVER_SECRET"}, "sent", "sent_with_warning", true},
		{"invalid", observability.SendResult{Rejected: 2, PartialSuccess: true}, "unconfirmed", "partial_unconfirmed", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := portTestDB(t)
			s := NewObservabilityStore(db)
			now := time.Now().UTC()
			require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{obsExportItem(t, c.name, now)}))
			items, err := s.Claim(t.Context(), 1, now, 5*time.Minute)
			require.NoError(t, err)
			require.Len(t, items, 1)
			batch := obsExportBatch("batch-"+c.name, items)
			require.NoError(t, s.BeginBatch(t.Context(), batch, now))
			require.NoError(t, s.ApplySendResult(t.Context(), batch, c.result, nil, now))
			var state, outcome string
			var accepted, rejected sql.NullInt64
			var warning sql.NullString
			require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT state FROM observability_export_items`).Scan(&state))
			require.Equal(t, c.state, state)
			require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT outcome,accepted_records,rejected_records,warning_text FROM observability_export_batches`).Scan(&outcome, &accepted, &rejected, &warning))
			require.Equal(t, c.outcome, outcome)
			require.Equal(t, c.known, accepted.Valid)
			require.Equal(t, c.known, rejected.Valid)
			require.NotContains(t, warning.String, "CANARY")
		})
	}
}

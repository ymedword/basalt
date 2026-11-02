//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityStore_ProtocolConflictNullCounts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		partial bool
		err     error
		kind    string
	}{
		{"partial_transport", true, context.DeadlineExceeded, "partial_with_transport_error"},
		{"rejection_without_partial", false, nil, "protocol_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := portTestDB(t)
			s := NewObservabilityStore(db)
			now := time.UnixMilli(1700000000000).UTC()
			require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{obsExportItem(t, "one", now), obsExportItem(t, "two", now)}))
			items, err := s.Claim(t.Context(), 2, now, 5*time.Minute)
			require.NoError(t, err)
			batch := obsExportBatch("protocol", items)
			require.NoError(t, s.BeginBatch(t.Context(), batch, now))
			require.NoError(t, s.ApplySendResult(t.Context(), batch, observability.SendResult{Accepted: 1, Rejected: 1, PartialSuccess: tc.partial, Retryable: true}, tc.err, now))
			var accepted, rejected sql.NullInt64
			var kind string
			require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT accepted_records,rejected_records,error_kind FROM observability_export_batches`).Scan(&accepted, &rejected, &kind))
			require.False(t, accepted.Valid, "protocol conflicts must not persist apparently valid local counts")
			require.False(t, rejected.Valid)
			require.Equal(t, tc.kind, kind)
			count, err := s.UnconfirmedCount(t.Context())
			require.NoError(t, err)
			require.Equal(t, int64(2), count)
			items, err = s.Claim(t.Context(), 2, now.Add(time.Hour), 5*time.Minute)
			require.NoError(t, err)
			require.Empty(t, items)
		})
	}
}

func TestObservabilityStore_BatchMembersAtomic(t *testing.T) {
	for _, mode := range []string{"subset", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			db := portTestDB(t)
			s := NewObservabilityStore(db)
			now := time.UnixMilli(1700000000000).UTC()
			require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{obsExportItem(t, "one", now), obsExportItem(t, "two", now), obsExportItem(t, "outsider", now)}))
			items, err := s.Claim(t.Context(), 3, now, 5*time.Minute)
			require.NoError(t, err)
			require.Len(t, items, 3)
			batch := obsExportBatch("members", items[:2])
			require.NoError(t, s.BeginBatch(t.Context(), batch, now))
			tampered := batch
			tampered.Items = []observability.ExportItem{items[0]}
			if mode == "replacement" {
				tampered.Items = append(tampered.Items, items[2])
			}
			err = s.ApplySendResult(t.Context(), tampered, observability.SendResult{Accepted: 2}, nil, now)
			require.ErrorIs(t, err, core.ErrConflict)
			var outcome string
			var accepted, rejected sql.NullInt64
			require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT outcome,accepted_records,rejected_records FROM observability_export_batches`).Scan(&outcome, &accepted, &rejected))
			require.Equal(t, "unknown", outcome)
			require.False(t, accepted.Valid)
			require.False(t, rejected.Valid)
			var count int
			require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_items WHERE state='inflight'`).Scan(&count))
			require.Equal(t, 3, count)
		})
	}
}

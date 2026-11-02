//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityStore_ExportLeaseFencing(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	now := time.Now().UTC()
	require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{obsExportItem(t, "fenced", now)}))
	first, err := s.Claim(t.Context(), 1, now, time.Minute)
	require.NoError(t, err)
	require.Len(t, first, 1)
	one := obsExportBatch("batch-first", first)
	require.NoError(t, s.BeginBatch(t.Context(), one, now))
	second, err := s.Claim(t.Context(), 1, now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, 2, second[0].Attempts)
	two := obsExportBatch("batch-second", second)
	require.NoError(t, s.BeginBatch(t.Context(), two, now.Add(2*time.Minute)))
	require.Error(t, s.ApplySendResult(t.Context(), one, observability.SendResult{Accepted: 1}, nil, now.Add(2*time.Minute)), "old lease owner must not acknowledge a new owner's item")
	status, err := s.ExportStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(2), status.UnknownAttempts)
	require.Nil(t, status.LastAccepted)
	require.Nil(t, status.LastRejected)
	require.NoError(t, s.ApplySendResult(t.Context(), two, observability.SendResult{Accepted: 1}, nil, now.Add(2*time.Minute)))
	status, err = s.ExportStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), status.UnknownAttempts)
	require.Equal(t, 1, *status.LastAccepted)
}

func TestObservabilityStore_ExportAckAtomic(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	now := time.Now().UTC()
	require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{obsExportItem(t, "ack-atomic", now)}))
	items, err := s.Claim(t.Context(), 1, now, time.Minute)
	require.NoError(t, err)
	batch := obsExportBatch("batch-atomic", items)
	require.NoError(t, s.BeginBatch(t.Context(), batch, now))
	_, err = db.pool.ExecContext(t.Context(), `CREATE FUNCTION obs_fail_batch_ack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic ack failure'; END $$`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `CREATE TRIGGER obs_fail_batch_ack BEFORE UPDATE ON observability_export_batches FOR EACH ROW EXECUTE FUNCTION obs_fail_batch_ack()`)
	require.NoError(t, err)
	require.Error(t, s.ApplySendResult(t.Context(), batch, observability.SendResult{Accepted: 1}, nil, now))
	var state string
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT state FROM observability_export_items`).Scan(&state))
	require.Equal(t, "inflight", state)
	status, err := s.ExportStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), status.UnknownAttempts)
	require.Nil(t, status.LastAccepted)
	require.Nil(t, status.LastRejected)
}

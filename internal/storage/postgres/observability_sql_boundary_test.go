//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityStore_SQLBoundary(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	now := time.Now().UTC().Truncate(time.Millisecond)
	obsSeedScanRun(t, db, "safe-run", now.Add(-time.Minute), now)
	_, exists, err := s.loadExportFact(t.Context(), observability.SourceKind("runs WHERE true --"), "safe-run")
	require.ErrorIs(t, err, observability.ErrInvalidQuery)
	require.False(t, exists)
	_, exists, err = s.loadExportFact(t.Context(), observability.SourceRun, "x' OR true --")
	require.NoError(t, err)
	require.False(t, exists, "source identity must remain a bound value")
	_, exists, err = s.loadExportFact(t.Context(), observability.SourceRun, "safe-run")
	require.NoError(t, err)
	require.True(t, exists)
	at := now.Add(-2 * time.Hour)
	obsSeedCleanupEvent(t, db, "a", at)
	obsSeedCleanupEvent(t, db, "b", at)
	tx, err := db.pool.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	req := obsCleanupRequest(now)
	req.Table = "diagnostic_events WHERE true --"
	_, err = obsCleanupKeys(t.Context(), tx, req)
	require.ErrorIs(t, err, observability.ErrInvalidQuery)
	req.Table = "diagnostic_events"
	req.Cursor = &observability.CleanupCursor{At: at, ID: "zz' OR true --"}
	keys, err := obsCleanupKeys(t.Context(), tx, req)
	require.NoError(t, err)
	require.Empty(t, keys, "hostile cursor must not widen the fixed SQL window")
	var count int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM diagnostic_events`).Scan(&count))
	require.Equal(t, 2, count)
}

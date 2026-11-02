//go:build integration

package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityStore_ExportRetryMetadata(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	now := time.Now().UTC().Truncate(time.Millisecond)
	read := func() map[string]any {
		got, err := s.ExportStatus(t.Context())
		require.NoError(t, err)
		raw, err := json.Marshal(got)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		for _, key := range []string{"retry_attempts", "next_retry_at", "last_http_status"} {
			require.Contains(t, fields, key)
		}
		return fields
	}
	fields := read()
	require.Nil(t, fields["retry_attempts"])
	require.Nil(t, fields["next_retry_at"])
	require.Nil(t, fields["last_http_status"])
	for i, attempts := range []int{1, 3} {
		item := obsExportItem(t, string(rune('a'+i)), now)
		require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{item}))
		_, err := db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state='retry',attempts=$1,next_attempt_at=$2 WHERE item_id=$3`, attempts, now.Add(time.Duration(i+1)*time.Minute).UnixMilli(), item.ItemID)
		require.NoError(t, err)
	}
	_, err := db.pool.ExecContext(t.Context(), `INSERT INTO observability_export_batches(batch_id,signal,record_unit,item_count,record_count,outcome,http_status,attempted_at) VALUES('retry-meta','trace','span',1,1,'retry',503,$1)`, now.UnixMilli())
	require.NoError(t, err)
	fields = read()
	require.Equal(t, float64(2), fields["retry_attempts"], "only attempts after the first send count as retries, summed across current retry items")
	require.Equal(t, now.Add(time.Minute).Format(time.RFC3339Nano), fields["next_retry_at"])
	require.Equal(t, float64(503), fields["last_http_status"])
	_, err = db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state='sent'`)
	require.NoError(t, err)
	fields = read()
	require.Nil(t, fields["retry_attempts"])
	require.Nil(t, fields["next_retry_at"])
}

func TestObservabilityStore_ExportStatusAllTerminalCounts(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	now := time.Now().UTC()
	for _, state := range []string{"failed_permanent", "dropped", "stale"} {
		item := obsExportItem(t, state, now)
		require.NoError(t, s.Enqueue(t.Context(), []observability.ExportItem{item}))
		_, err := db.pool.ExecContext(t.Context(), `UPDATE observability_export_items SET state=$1 WHERE item_id=$2`, state, item.ItemID)
		require.NoError(t, err)
	}
	got, err := s.ExportStatus(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), got.FailedPermanent)
	require.Equal(t, int64(1), got.Dropped)
	require.Equal(t, int64(1), got.Stale)
	require.Nil(t, got.LastAccepted)
	require.Nil(t, got.LastRejected)
}

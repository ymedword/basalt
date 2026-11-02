//go:build integration

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecovery_NormalizePendingSetsRecoveredAt(t *testing.T) {
	db := portTestDB(t)
	ctx := t.Context()
	_, err := db.pool.ExecContext(ctx, `INSERT INTO llm_calls
		(run_id,round,provider,model,duration_ms,status,created_at)
		VALUES ('old-pending',1,'fake','fake',0,'pending',1),('already-done',1,'fake','fake',9,'success',1)`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO tool_invocations
		(run_id,tool_name,status,started_at) VALUES ('old-pending','fake','pending',1)`)
	require.NoError(t, err)
	now := time.UnixMilli(1700000000000).UTC()
	count, err := normalizePendingAuditRows(ctx, db.pool, now)
	require.NoError(t, err)
	require.Equal(t, 2, count)
	for _, table := range []string{"llm_calls", "tool_invocations"} {
		var status string
		var recovered sql.NullInt64
		// 表名仅来自本测试固定集合，业务查询不接受表名输入。
		require.NoError(t, db.pool.QueryRowContext(ctx, "SELECT status,recovered_at FROM "+table+" WHERE run_id='old-pending'").Scan(&status, &recovered))
		require.Equal(t, "unknown", status)
		require.True(t, recovered.Valid, "recovery time is needed for terminal-time scans")
		require.Equal(t, now.UnixMilli(), recovered.Int64)
	}
	var recovered, finished sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT recovered_at FROM llm_calls WHERE run_id='already-done'`).Scan(&recovered))
	require.False(t, recovered.Valid, "normal terminal calls must remain unchanged")
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT finished_at FROM tool_invocations WHERE run_id='old-pending'`).Scan(&finished))
	require.False(t, finished.Valid, "recovery is not a fabricated tool end time")
	count, err = normalizePendingAuditRows(ctx, db.pool, now.Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, count)
	var saved int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT recovered_at FROM llm_calls WHERE run_id='old-pending'`).Scan(&saved))
	require.Equal(t, now.UnixMilli(), saved, "second recovery must not rewrite terminal evidence")
}

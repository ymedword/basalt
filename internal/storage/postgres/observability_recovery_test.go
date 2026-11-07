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
	// US-24：归一化面收窄为**显式领取的 run_id 集合**（FR-24-16），不再扫全表。
	// 返回值逐行带回表名 + 主键（评审 IDX-28）：诊断的 `audit_ref` 靠它回指审计行，
	// 只给计数会逼调用方事后重查，而那时行可能已被并发收尾带走。
	actions, err := normalizePendingAuditRows(ctx, db.pool, []string{"old-pending"}, now)
	require.NoError(t, err)
	require.Len(t, actions, 2)
	require.ElementsMatch(t,
		[]string{UnreplayedTableLLMCalls, UnreplayedTableToolInvocations},
		[]string{actions[0].Table, actions[1].Table})
	for _, action := range actions {
		require.NotZero(t, action.ID, "归一化行必须带回主键（诊断 audit_ref 的回指依据）")
		switch action.Table {
		case UnreplayedTableLLMCalls:
			require.Equal(t, int64(1), action.Round, "模型调用动作带回轮号（与 llm_call 事件的 step_id 同口径）")
		case UnreplayedTableToolInvocations:
			require.Equal(t, "fake", action.ToolName, "工具动作带回工具名（诊断 attrs 的 tool 键）")
		}
	}
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
	actions, err = normalizePendingAuditRows(ctx, db.pool, []string{"old-pending"}, now.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, actions, "第二次归一化不得再返回动作——否则会重复写诊断事件")
	var saved int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT recovered_at FROM llm_calls WHERE run_id='old-pending'`).Scan(&saved))
	require.Equal(t, now.UnixMilli(), saved, "second recovery must not rewrite terminal evidence")
}

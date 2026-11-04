//go:build integration

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
)

func sandboxSeed(t *testing.T, db *DB, cleanup core.CleanupState) core.SandboxExecution {
	t.Helper()
	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	historySeed(t, db, "sandbox-session", "sandbox-run", old)
	var id int64
	err := db.pool.QueryRowContext(t.Context(), `INSERT INTO tool_invocations(run_id,session_id,tool_call_id,tool_name,status,started_at,finished_at) VALUES('sandbox-run','sandbox-session','call-1','run_code','success',$1,$1) RETURNING id`, old.UnixMilli()).Scan(&id)
	require.NoError(t, err)
	rec := core.SandboxExecution{ToolInvocationID: id, RunID: "sandbox-run", AgentName: "agent", InstanceID: "inst_test", Profile: "gvisor", Runtime: "runsc", ImageDigest: "sha256:test", ContainerName: "basalt-sbx-test", StartedAt: old}
	require.NoError(t, db.BeginSandboxExecution(t.Context(), rec))
	if cleanup != "" {
		require.NoError(t, db.FinishSandboxExecution(t.Context(), core.SandboxExecutionResult{ToolInvocationID: id, State: core.SandboxFinished, CleanupState: cleanup, FinishedAt: old}))
	}
	return rec
}

func TestUS20_SandboxExecutionStore(t *testing.T) {
	db := portTestDB(t)
	rec := sandboxSeed(t, db, "")
	ctx := t.Context()
	require.Error(t, db.BeginSandboxExecution(ctx, rec), "唯一账本禁止重复执行")
	pending, err := db.PendingSandboxExecutions(ctx, rec.InstanceID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, core.SandboxCreating, pending[0].State)
	require.Equal(t, core.CleanupPending, pending[0].CleanupState)
	require.NoError(t, db.MarkSandboxRunning(ctx, rec.ToolInvocationID, time.Now()))
	n, err := db.CountActiveSandboxExecutions(ctx, rec.InstanceID)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoError(t, db.MarkSandboxCleaned(ctx, rec.ToolInvocationID, core.SandboxUnknown, core.CleanupCleaned, "orphan_cleaned", time.Now()))
	n, err = db.CountActiveSandboxExecutions(ctx, rec.InstanceID)
	require.NoError(t, err)
	require.Zero(t, n, "unknown+cleaned 必须释放容量")
	views, err := db.SandboxExecutionsByRun(ctx, rec.RunID)
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, core.SandboxUnknown, views[0].State)
	require.Equal(t, "call-1", views[0].ToolCallID)
	require.Equal(t, "orphan_cleaned", views[0].ErrorReason)
	require.NoError(t, db.FinishSandboxExecution(ctx, core.SandboxExecutionResult{ToolInvocationID: rec.ToolInvocationID, State: core.SandboxFinished, CleanupState: core.CleanupFailed, ArtifactCount: 2, ArtifactBytes: 7, FinishedAt: time.Now()}))
	views, err = db.SandboxExecutionsByRun(ctx, rec.RunID)
	require.NoError(t, err)
	require.Equal(t, 2, views[0].ArtifactCount)
	require.EqualValues(t, 7, views[0].ArtifactBytes)
	require.NoError(t, db.MarkSandboxCleaned(ctx, rec.ToolInvocationID, core.SandboxUnknown, core.CleanupCleaned, "orphan_cleaned", time.Now()))
	views, err = db.SandboxExecutionsByRun(ctx, rec.RunID)
	require.NoError(t, err)
	require.Equal(t, core.SandboxFinished, views[0].State, "恢复不得抹掉已知终态")
	require.ErrorIs(t, db.MarkSandboxRunning(ctx, 999, time.Now()), sql.ErrNoRows)
	require.Error(t, db.FinishSandboxExecution(ctx, core.SandboxExecutionResult{ToolInvocationID: rec.ToolInvocationID, State: core.SandboxFinished, CleanupState: core.CleanupPending}))
	require.Error(t, db.MarkSandboxCleaned(ctx, rec.ToolInvocationID, core.SandboxUnknown, core.CleanupPending, "", time.Now()))
}

func TestUS20_PendingIncludesCleanupFailed(t *testing.T) {
	db := portTestDB(t)
	rec := sandboxSeed(t, db, core.CleanupFailed)
	rows, err := db.PendingSandboxExecutions(t.Context(), rec.InstanceID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, core.CleanupFailed, rows[0].CleanupState)
	n, err := db.CountActiveSandboxExecutions(t.Context(), rec.InstanceID)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	rows, err = db.PendingSandboxExecutions(t.Context(), "other")
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestUS20_HistoryRetentionProtectsOpenSandbox(t *testing.T) {
	for _, cleanup := range []core.CleanupState{core.CleanupPending, core.CleanupFailed} {
		t.Run(string(cleanup), func(t *testing.T) {
			db := portTestDB(t)
			seed := cleanup
			if cleanup == core.CleanupPending {
				seed = ""
			}
			sandboxSeed(t, db, seed)
			_, err := NewHistoryRetentionStore(db, false).CleanupHistoryBatch(t.Context(), "sessions", time.Now().Add(-30*24*time.Hour), 100, "", core.NewAgentService(core.ServiceOptions{}))
			require.NoError(t, err)
			var n int
			require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM sandbox_executions`).Scan(&n))
			require.Equal(t, 1, n, "未清理账本不能随历史丢失")
		})
	}
}

func TestUS20_HistoryRetentionCascadesCleaned(t *testing.T) {
	db := portTestDB(t)
	sandboxSeed(t, db, core.CleanupCleaned)
	_, err := NewHistoryRetentionStore(db, false).CleanupHistoryBatch(t.Context(), "sessions", time.Now().Add(-30*24*time.Hour), 100, "", core.NewAgentService(core.ServiceOptions{}))
	require.NoError(t, err)
	var n int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM sandbox_executions`).Scan(&n))
	require.Zero(t, n, "已清理账本必须可级联删除")
}

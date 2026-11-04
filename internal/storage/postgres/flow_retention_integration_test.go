//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func TestFlow_HistoryRetentionPreservesReferencedRun(t *testing.T) {
	for _, mode := range []string{"active", "closed"} {
		t.Run(mode, func(t *testing.T) {
			p := newPGFlowActionProbe(t)
			now := time.Now()
			old := now.Add(-40 * 24 * time.Hour)
			require.NoError(t, NewRunStore(p.store.db).FinishRun(t.Context(), p.env.RunID, core.StatusCompleted, "", 0, 0, old))
			_, err := p.store.db.pool.ExecContext(t.Context(), `UPDATE runs SET started_at=$2 WHERE run_id=$1`, p.env.RunID, old.UnixMilli())
			require.NoError(t, err)
			_, err = p.store.db.pool.ExecContext(t.Context(), `UPDATE run_resource_manifests SET created_at=$2 WHERE run_id=$1`, p.env.RunID, old.UnixMilli())
			require.NoError(t, err)
			if mode == "closed" {
				_, err = p.store.Cancel(t.Context(), flow.CancelTask{Request: flow.CancelRequest{Principal: p.env.Principal, TaskID: p.task.ID, Expected: p.task.Version, OperationID: "close-retention", Reason: "closed does not release authority evidence"}, Joined: true, Now: now.UnixMilli()})
				require.NoError(t, err)
			}
			historySeed(t, p.store.db, "", "unreferenced-old", old)
			report, err := NewHistoryRetentionStore(p.store.db, false).CleanupHistoryBatch(t.Context(), "runs", now.Add(-30*24*time.Hour), 100, "", nil)
			require.NoError(t, err)
			require.EqualValues(t, 1, report.DeletedGroups)
			require.EqualValues(t, 1, report.ProtectedGroups)
			var count int
			require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM runs WHERE run_id=$1`, p.env.RunID).Scan(&count))
			require.Equal(t, 1, count)
			require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_node_attempts WHERE task_id=$1`, p.task.ID).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}

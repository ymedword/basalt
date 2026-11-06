//go:build integration

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func TestFlow_QueryEmbeddingPersistentPermit(t *testing.T) {
	for _, mode := range []string{"bound", "cancel-after-reserve", "unknown-ticket", "binding-fault"} {
		t.Run(mode, func(t *testing.T) {
			p := newPGFlowActionProbe(t)
			require.NoError(t, p.store.CheckActiveBoundary(t.Context(), p.env.Principal, p.task.ID, "node-action", p.env.RunID, p.task.Version))
			budget := &FlowBudget{Store: p.store, Principal: p.env.Principal, TaskID: p.task.ID}
			ticket, err := budget.Reserve(t.Context(), p.env.RunID, "query-embedding:request-hash", core.UsageEstimate{Tokens: 20})
			require.NoError(t, err)
			audit := NewEmbeddingCallRepo(p.store.db, 2)
			id, err := audit.BeginEmbedding(t.Context(), "", "query-model", 1)
			require.NoError(t, err)
			if mode == "cancel-after-reserve" {
				_, err = p.store.Cancel(t.Context(), flow.CancelTask{Request: flow.CancelRequest{Principal: p.env.Principal, TaskID: p.task.ID, Expected: p.task.Version, OperationID: "cancel-query", Reason: "cancel between reserve and dispatch"}, Now: p.task.UpdatedAt + 1})
				require.NoError(t, err)
				require.Error(t, p.store.CheckActiveBoundary(t.Context(), p.env.Principal, p.task.ID, "node-action", p.env.RunID, p.task.Version), "persistent cancellation forbids source reads even with a live context")
			}
			if mode == "unknown-ticket" {
				require.NoError(t, budget.KeepUnknown(t.Context(), ticket))
			}
			if mode == "binding-fault" {
				installFlowFault(t, p.store, "embedding_calls", "UPDATE", "")
			}
			err = p.store.AuthorizeQueryEmbedding(t.Context(), p.env.Principal, p.task.ID, "node-action", p.env.RunID, id, p.task.Version, ticket)
			if mode != "bound" {
				require.Error(t, err)
				var unbound bool
				require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT run_id IS NULL FROM embedding_calls WHERE id=$1`, id).Scan(&unbound))
				require.True(t, unbound, "zero permission means zero external calls")
				if mode == "binding-fault" {
					var fired bool
					require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT is_called FROM flow_probe_fault_seq`).Scan(&fired))
					require.True(t, fired)
				}
				return
			}
			require.NoError(t, err)
			var bound string
			require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT run_id FROM embedding_calls WHERE id=$1 AND status='pending'`, id).Scan(&bound))
			require.Equal(t, p.env.RunID, bound)
			unknown, err := NewFlowStore(p.store.db).HasUnknownActions(t.Context(), p.task.ID)
			require.NoError(t, err)
			require.True(t, unknown, "bound pending audit must block resume/revision")
			_, err = NewRunStore(p.store.db).FinishRun(t.Context(), p.env.RunID, core.StatusCompleted, "", 0, 0, p.store.db.clock())
			require.NoError(t, err)
			observation, err := NewFlowStore(p.store.db).ObserveNode(t.Context(), p.env.Principal, p.task.ID, "node-action")
			require.NoError(t, err)
			require.True(t, observation.Unknown, "completed Run does not hide a pending embedding")
			require.NoError(t, budget.KeepUnknown(t.Context(), ticket))
			second, err := audit.BeginEmbedding(t.Context(), "", "query-model", 1)
			require.NoError(t, err)
			// Reconstruct all in-memory guards/store handles. Even a reused pending
			// ticket cannot bind another audit for the same original Run.
			err = NewFlowStore(p.store.db).AuthorizeQueryEmbedding(t.Context(), p.env.Principal, p.task.ID, "node-action", p.env.RunID, second, p.task.Version, ticket)
			require.Error(t, err)
			var count int
			require.NoError(t, p.store.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM embedding_calls WHERE run_id=$1`, p.env.RunID).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}

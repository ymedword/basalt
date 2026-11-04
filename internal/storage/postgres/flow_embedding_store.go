package postgres

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// AuthorizeQueryEmbedding binds the original pending audit to the unique Run
// under the same root lock as Cancel. A reconstructed guard cannot send twice.
// It does not begin/finish an audit or issue an embedding itself.
func (s *FlowStore) AuthorizeQueryEmbedding(ctx context.Context, p core.Principal, taskID, attemptID, runID, auditID string, expected flow.TaskVersion, budget core.BudgetTicket) error {
	id, err := strconv.ParseInt(auditID, 10, 64)
	if err != nil || id <= 0 || budget.TaskID != taskID || budget.RunID != runID || !strings.HasPrefix(budget.ActionID, "query-embedding:") {
		return flow.ErrStateConflict
	}
	return s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_calls WHERE id=$1 AND run_id=$2 AND status='pending')`, id, runID).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, taskID, p)
		if err != nil {
			return err
		}
		if task.Status != flow.Running || task.CloseIntent != flow.CloseNone || task.CancelRequested || task.Version.Revision != expected.Revision || task.Version.ExecutionEpoch != expected.ExecutionEpoch {
			return flow.ErrStateConflict
		}
		if err := ensureFlowAncestorsOpen(ctx, tx, task, s.db.clock().UnixMilli()); err != nil {
			return err
		}
		if err := checkFlowAuthorizingKey(ctx, tx, task, p, s.db.clock().UnixMilli()); err != nil {
			return err
		}
		if p.Kind == core.PrincipalCaller {
			var readable bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM caller_keys WHERE key_id=$1 AND caller_id=$2 AND 'knowledge:read'=ANY(scopes))`, p.KeyID, p.CallerID).Scan(&readable); err != nil {
				return err
			}
			if !readable {
				return core.ErrUnauthorized
			}
		}
		var valid bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_node_attempts n JOIN runs r ON r.run_id=n.run_id AND r.flow_node_attempt_id=n.node_attempt_id JOIN flow_budget_allocations b ON b.task_id=n.task_id AND b.run_id=n.run_id WHERE n.task_id=$1 AND n.node_attempt_id=$2 AND n.run_id=$3 AND n.state='running' AND n.task_revision=$4 AND n.execution_epoch=$5 AND r.status='running' AND b.allocation_id=$6 AND b.action_id=$7 AND b.state='pending' AND b.reserved_tokens=$8 AND b.reserved_tool_calls=0)`, taskID, attemptID, runID, expected.Revision, expected.ExecutionEpoch, budget.ID, budget.ActionID, budget.Reserved.Tokens).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return flow.ErrStateConflict
		}
		var used bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM embedding_calls WHERE run_id=$1)`, runID).Scan(&used); err != nil {
			return err
		}
		if used {
			return flow.ErrActionUnknown
		}
		changed, err := tx.ExecContext(ctx, `UPDATE embedding_calls SET run_id=$1 WHERE id=$2 AND run_id IS NULL AND ingest_job_id IS NULL AND status='pending'`, runID, id)
		if err != nil {
			return err
		}
		count, err := changed.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return flow.ErrStateConflict
		}
		return nil
	})
}

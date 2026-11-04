package postgres

import (
	"context"
	"database/sql"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// CheckActiveBoundary is the short durable read authorization used before source
// consumption. It does not keep a transaction open across source/network I/O.
func (s *FlowStore) CheckActiveBoundary(ctx context.Context, p core.Principal, taskID, attemptID, runID string, expected flow.TaskVersion) error {
	_, err := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, taskID, p)
		if err != nil {
			return err
		}
		if task.Status != flow.Running || task.CancelRequested || task.CloseIntent != flow.CloseNone || task.Version.Revision != expected.Revision || task.Version.ExecutionEpoch != expected.ExecutionEpoch {
			return flow.ErrStateConflict
		}
		if err := ensureFlowAncestorsOpen(ctx, tx, task, s.db.clock().UnixMilli()); err != nil {
			return err
		}
		if err := checkFlowAuthorizingKey(ctx, tx, task, p, s.db.clock().UnixMilli()); err != nil {
			return err
		}
		var valid bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_node_attempts n JOIN runs r ON r.run_id=n.run_id AND r.flow_task_id=n.task_id AND r.flow_node_attempt_id=n.node_attempt_id WHERE n.task_id=$1 AND n.node_attempt_id=$2 AND n.run_id=$3 AND n.state='running' AND n.task_revision=$4 AND n.execution_epoch=$5 AND r.status='running')`, taskID, attemptID, runID, expected.Revision, expected.ExecutionEpoch).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return flow.ErrStateConflict
		}
		return nil
	})
	return err
}

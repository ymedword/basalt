package postgres

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// YieldUnadmitted defers an actual admission refusal. It only refunds the node
// attempt if the original Run ledger and every action/allocation prove no Run was
// admitted. History remains; failures after admission never use this method.
func (s *FlowStore) YieldUnadmitted(ctx context.Context, p core.Principal, id, attemptID string, expected flow.TaskVersion, eligible int64) error {
	op := "yield:" + attemptID
	hash := flowOperationHash("yield", struct {
		TaskID, AttemptID string
		Version           flow.TaskVersion
		Eligible          int64
	}{id, attemptID, expected, eligible})
	return s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return findFlowOperation(ctx, q, id, op, "yield", hash)
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, id, p)
		if err != nil {
			return err
		}
		if found, err := findFlowOperation(ctx, tx, id, op, "yield", hash); found || err != nil {
			return err
		}
		if err := checkFlowOwnedVersion(task, p, expected); err != nil {
			return err
		}
		if task.Status != flow.Running || task.CancelRequested || task.CloseIntent != flow.CloseNone || eligible >= task.Deadline {
			return flow.ErrStateConflict
		}
		var node, state string
		var number int
		var used bool
		err = tx.QueryRowContext(ctx, `SELECT n.node_id,n.attempt,n.state,EXISTS(SELECT 1 FROM runs r WHERE r.run_id=n.run_id) OR EXISTS(SELECT 1 FROM flow_action_attempts a WHERE a.task_id=n.task_id AND a.node_attempt_id=n.node_attempt_id) OR EXISTS(SELECT 1 FROM flow_budget_allocations b WHERE b.task_id=n.task_id AND b.run_id=n.run_id) FROM flow_node_attempts n WHERE n.task_id=$1 AND n.node_attempt_id=$2`, id, attemptID).Scan(&node, &number, &state, &used)
		if err != nil {
			return err
		}
		if state != "running" || used || task.Revision.Attempts[node] != number || task.Revision.ExpandedSteps < 1 {
			return flow.ErrStateConflict
		}
		task.Revision.Attempts[node]--
		task.Revision.ExpandedSteps--
		if task.ID != task.RootTaskID {
			root, err := readFlowTask(ctx, tx, task.RootTaskID)
			if err != nil {
				return err
			}
			if root.Revision.ExpandedSteps < 1 {
				return flow.ErrStateConflict
			}
			root.Revision.ExpandedSteps--
			if _, err := tx.ExecContext(ctx, `UPDATE flow_task_revisions SET checkpoint=$3 WHERE task_id=$1 AND revision=$2`, root.ID, root.Version.Revision, mustFlowJSON(root.Revision)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flow_node_attempts SET state='interrupted',finished_at=$3 WHERE task_id=$1 AND node_attempt_id=$2`, id, attemptID, s.db.clock().UnixMilli()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flow_task_revisions SET checkpoint=$3 WHERE task_id=$1 AND revision=$2`, id, task.Version.Revision, mustFlowJSON(task.Revision)); err != nil {
			return err
		}
		now := s.db.clock().UnixMilli()
		if err := updateFlowHead(ctx, tx, task, task.Status, task.Version.Revision, task.Version.ExecutionEpoch, task.CloseIntent, false, now, "capacity deferred"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flow_tasks SET next_eligible_at=$2 WHERE task_id=$1`, id, eligible); err != nil {
			return err
		}
		return appendFlowTransition(ctx, tx, task, op, "yield", hash, task.Status, task.Version.Revision, now, "admission capacity; attempt "+strconv.Itoa(number)+" not consumed", p)
	})
}

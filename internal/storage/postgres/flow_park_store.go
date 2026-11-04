package postgres

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// ParkTask records a safe control-plane stop after the local driver has joined.
// It neither marks a call not_issued nor frees unknown allocations.
func (s *FlowStore) ParkTask(ctx context.Context, p core.Principal, id string, expected flow.TaskVersion, reason string) error {
	if reason == "" || len(reason) > 128 {
		return flow.ErrStateConflict
	}
	op := "park:" + id + ":" + strconv.FormatInt(expected.StateVersion, 10)
	hash := flowOperationHash("park", struct {
		TaskID, Reason string
		Expected       flow.TaskVersion
	}{id, reason, expected})
	return s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return findFlowOperation(ctx, q, id, op, "park", hash)
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, id, p)
		if err != nil {
			return err
		}
		if found, err := findFlowOperation(ctx, tx, id, op, "park", hash); found || err != nil {
			return err
		}
		if err := checkFlowOwnedVersion(task, p, expected); err != nil {
			return err
		}
		if task.CancelRequested || task.CloseIntent != flow.CloseNone || task.Status.Terminal() {
			return flow.ErrTaskClosed
		}
		if task.Status == flow.Queued {
			if err := updateFlowHead(ctx, tx, task, flow.Running, task.Version.Revision, task.Version.ExecutionEpoch, flow.CloseNone, false, s.db.clock().UnixMilli(), reason); err != nil {
				return err
			}
			if err := appendFlowTransition(ctx, tx, task, op, "park-claim", hash, flow.Running, task.Version.Revision, s.db.clock().UnixMilli(), reason, p); err != nil {
				return err
			}
			task, err = readFlowTask(ctx, tx, id)
			if err != nil {
				return err
			}
		}
		if task.Status != flow.Running && task.Status != flow.Blocked {
			return flow.ErrStateConflict
		}
		// 原调用恢复已先核对到unknown；这里只传播该事实，不把派发中猜成未发出。
		// Propagate the original recovered unknown audit without freeing any reservation.
		if err := reconcileFlowUnknownAudits(ctx, tx, task.RootTaskID, s.db.clock().UnixMilli()); err != nil {
			return err
		}
		if err := updateFlowHead(ctx, tx, task, flow.Blocked, task.Version.Revision, task.Version.ExecutionEpoch, flow.CloseNone, false, s.db.clock().UnixMilli(), reason); err != nil {
			return err
		}
		return appendFlowTransition(ctx, tx, task, op, "park", hash, flow.Blocked, task.Version.Revision, s.db.clock().UnixMilli(), reason, p)
	})
}

func reconcileFlowUnknownAudits(ctx context.Context, tx *sql.Tx, root string, now int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE flow_action_attempts a SET outcome='unknown',finished_at=$2
 FROM flow_tasks t,runs r WHERE a.task_id=t.task_id AND t.root_task_id=$1
 AND r.run_id=a.run_id AND r.flow_task_id=a.task_id AND r.flow_node_attempt_id=a.node_attempt_id
 AND r.status<>'running' AND a.outcome='dispatching'
 AND (EXISTS(SELECT 1 FROM tool_invocations i WHERE i.id=a.tool_invocation_id AND i.run_id=a.run_id AND i.status='unknown')
 OR EXISTS(SELECT 1 FROM llm_calls l WHERE l.id=a.llm_call_id AND l.run_id=a.run_id AND l.status='unknown'))`, root, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE flow_actions a SET outcome='unknown' FROM flow_tasks t
 WHERE a.task_id=t.task_id AND t.root_task_id=$1 AND a.outcome='dispatching'
 AND EXISTS(SELECT 1 FROM flow_action_attempts x WHERE x.task_id=a.task_id AND x.action_id=a.action_id AND x.outcome='unknown')`, root)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE flow_budget_allocations b SET state='unknown'
 WHERE b.root_task_id=$1 AND b.state='pending'
 AND EXISTS(SELECT 1 FROM flow_action_attempts a WHERE a.task_id=b.task_id AND a.action_id=b.action_id AND a.run_id=b.run_id AND a.outcome='unknown')`, root)
	return err
}

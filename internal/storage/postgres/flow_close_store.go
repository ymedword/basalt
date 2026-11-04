package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func (s *FlowStore) Cancel(ctx context.Context, req flow.CancelTask) (flow.Task, error) {
	r := req.Request
	return s.closeFlowTask(ctx, r.Principal, r.TaskID, r.OperationID, r.Reason, r.Expected, flow.CloseCancel, req.Joined, false, req.Now)
}

func (s *FlowStore) Abandon(ctx context.Context, req flow.AbandonTask) (flow.Task, error) {
	r := req.Request
	if !r.Principal.Valid() || r.Principal.Kind != core.PrincipalLocalAdmin || r.Actor.Subject == "" || r.Actor.Method == "" || r.Actor.Source == "" || strings.TrimSpace(r.Reason) == "" {
		return flow.Task{}, core.ErrUnauthorized
	}
	return s.closeFlowTask(ctx, r.Principal, r.TaskID, r.OperationID, r.Reason, r.Expected, flow.CloseAbandon, req.Joined, req.NoAvailableCallerKey, req.Now)
}

func (s *FlowStore) closeFlowTask(ctx context.Context, p core.Principal, id, op, reason string, expected flow.TaskVersion, intent flow.CloseIntent, joined, rescue bool, now int64) (flow.Task, error) {
	var result flow.Task
	if !p.Valid() || op == "" || len(op) > 256 || len(reason) > 4096 {
		return result, flow.ErrStateConflict
	}
	phase := "close-intent"
	if joined {
		phase = "close-join"
	}
	hash := flowOperationHash(string(intent), struct {
		TaskID, Reason, CallerID string
		Expected                 flow.TaskVersion
	}{id, reason, p.CallerID, expected})
	err := s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return findFlowOperation(ctx, q, id, op, phase, hash)
	}, func(ctx context.Context, tx *sql.Tx) error {
		var task flow.Task
		var err error
		if intent == flow.CloseAbandon {
			task, err = lockedFlowTask(ctx, tx, id)
		} else {
			task, err = lockedFlowOwnedTask(ctx, tx, id, p)
		}
		if err != nil {
			return err
		}
		if task.CallerID != p.CallerID && intent != flow.CloseAbandon {
			return flow.ErrNotFound
		}
		previous, err := findFlowOperation(ctx, tx, id, op, phase, hash)
		if err != nil {
			return err
		}
		if previous {
			result = task
			return nil
		}
		priorIntent, err := findFlowOperation(ctx, tx, id, op, "close-intent", hash)
		if err != nil {
			return err
		}
		if !priorIntent && task.Version != expected {
			return core.ErrPreconditionFailed
		}
		if task.Status.Terminal() {
			return flow.ErrTaskClosed
		}
		if intent == flow.CloseAbandon && !priorIntent {
			if err := checkFlowPendingOperation(ctx, tx, op, "flow.abandon", id, expected.StateVersion); err != nil {
				return err
			}
		}
		status, closeIntent, cancel, err := flow.CloseState(task, intent, joined, rescue)
		if err != nil {
			return err
		}
		// A running call first records cancelling even if it has just joined.
		// The two legal transitions may commit together, never an illegal shortcut.
		if task.Status == flow.Running && (status == flow.Cancelled || status == flow.Abandoned) {
			if err := updateFlowHead(ctx, tx, task, flow.Cancelling, task.Version.Revision, task.Version.ExecutionEpoch, closeIntent, cancel, now, reason); err != nil {
				return err
			}
			if err := appendFlowTransition(ctx, tx, task, op, "close-intent", hash, flow.Cancelling, task.Version.Revision, now, reason, p); err != nil {
				return err
			}
			task, err = readFlowTask(ctx, tx, id)
			if err != nil {
				return err
			}
		}
		if err := updateFlowHead(ctx, tx, task, status, task.Version.Revision, task.Version.ExecutionEpoch, closeIntent, cancel, now, reason); err != nil {
			return err
		}
		if err := appendFlowTransition(ctx, tx, task, op, phase, hash, status, task.Version.Revision, now, reason, p); err != nil {
			return err
		}
		if joined {
			// This closes bookkeeping only; action outcome/unknown reservations stay.
			if _, err := tx.ExecContext(ctx, `UPDATE flow_node_attempts SET state='interrupted',finished_at=$2 WHERE task_id=$1 AND state IN ('prepared','running')`, id, now); err != nil {
				return err
			}
		}
		if intent == flow.CloseAbandon {
			if err := finishOperationTx(ctx, s.db, tx, op, core.OperationSuccess, "FLOW_ABANDONED", "", nil, s.db.clock()); err != nil {
				return err
			}
		}
		if err := propagateFlowClose(ctx, tx, task, intent, joined, rescue, op, hash, reason, p, now); err != nil {
			return err
		}
		result, err = readFlowTask(ctx, tx, id)
		return err
	})
	return result, err
}

func (s *FlowStore) Recover(ctx context.Context, req flow.ReconcileTask) (flow.Task, error) {
	var result flow.Task
	r := req.Request
	if !r.Principal.Valid() || r.OperationID == "" || strings.TrimSpace(r.Reason) == "" {
		return result, flow.ErrStateConflict
	}
	hash := flowRecoveryRequestHash(r)
	err := s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return findFlowOperation(ctx, q, r.TaskID, r.OperationID, "recover", hash)
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, r.TaskID, r.Principal)
		if err != nil {
			return err
		}
		if task.CallerID != r.Principal.CallerID {
			return flow.ErrNotFound
		}
		found, err := findFlowOperation(ctx, tx, r.TaskID, r.OperationID, "recover", hash)
		if err != nil {
			return err
		}
		if found {
			result = task
			return nil
		}
		if err := checkFlowOwnedVersion(task, r.Principal, r.Expected); err != nil {
			return err
		}
		if task.Status.Terminal() {
			return flow.ErrTaskClosed
		}
		status, intent, cancel := req.Status, task.CloseIntent, task.CancelRequested
		if status == flow.Queued && (intent != flow.CloseNone || task.CancelRequested || req.Now >= task.Deadline) {
			return flow.ErrStateConflict
		}
		if intent != flow.CloseNone || req.Now >= task.Deadline {
			if intent == flow.CloseNone {
				intent = flow.CloseDeadline
			}
			status, intent, cancel, err = flow.CloseState(task, intent, req.Joined, false)
			if err != nil {
				return err
			}
		} else if status == flow.Queued {
			if !req.Joined || task.Status != flow.Blocked || r.Principal.KeyID != task.AuthorizingKeyID {
				return flow.ErrStateConflict
			}
			if err := ensureFlowAncestorsOpen(ctx, tx, task, req.Now); err != nil {
				return err
			}
			unknown, err := unknownFlowActions(ctx, tx, task.RootTaskID)
			if err != nil {
				return err
			}
			if unknown {
				return flow.ErrActionUnknown
			}
		} else if status != flow.Blocked {
			return flow.ErrStateConflict
		}
		if task.Status == flow.Running && (status == flow.Cancelled || status == flow.Abandoned) {
			if err := updateFlowHead(ctx, tx, task, flow.Cancelling, task.Version.Revision, task.Version.ExecutionEpoch, intent, cancel, req.Now, req.Reason); err != nil {
				return err
			}
			if err := appendFlowTransition(ctx, tx, task, r.OperationID, "recover-intent", hash, flow.Cancelling, task.Version.Revision, req.Now, req.Reason, r.Principal); err != nil {
				return err
			}
			task, err = readFlowTask(ctx, tx, task.ID)
			if err != nil {
				return err
			}
		}
		if err := updateFlowHead(ctx, tx, task, status, task.Version.Revision, task.Version.ExecutionEpoch+1, intent, cancel, req.Now, req.Reason); err != nil {
			return err
		}
		if err := appendFlowTransition(ctx, tx, task, r.OperationID, "recover", hash, status, task.Version.Revision, req.Now, req.Reason, r.Principal); err != nil {
			return err
		}
		if req.Joined {
			if _, err := tx.ExecContext(ctx, `UPDATE flow_node_attempts SET state='interrupted',finished_at=$2 WHERE task_id=$1 AND state IN ('prepared','running')`, task.ID, req.Now); err != nil {
				return err
			}
		}
		if intent != flow.CloseNone {
			if err := propagateFlowClose(ctx, tx, task, intent, req.Joined, false, r.OperationID, hash, req.Reason, r.Principal, req.Now); err != nil {
				return err
			}
		}
		result, err = readFlowTask(ctx, tx, task.ID)
		return err
	})
	return result, err
}

func flowRecoveryRequestHash(r flow.ResumeRequest) string {
	return flowOperationHash("recover", struct {
		TaskID, Reason, CallerID, KeyID string
		Expected                        flow.TaskVersion
	}{r.TaskID, r.Reason, r.Principal.CallerID, r.Principal.KeyID, r.Expected})
}

// FindRecovery reads an already committed operation before applying fresh
// version/deadline checks. The original owner key and exact request stay bound.
func (s *FlowStore) FindRecovery(ctx context.Context, r flow.ResumeRequest) (flow.Task, bool, error) {
	if !r.Principal.Valid() || !r.Principal.HasScope(core.ScopeFlowsWrite) {
		return flow.Task{}, false, core.ErrUnauthorized
	}
	task, err := s.Read(ctx, r.Principal, r.TaskID)
	if err != nil {
		return task, false, err
	}
	if r.Principal.KeyID != task.AuthorizingKeyID {
		return task, false, core.ErrUnauthorized
	}
	found, err := findFlowOperation(ctx, s.db, r.TaskID, r.OperationID, "recover", flowRecoveryRequestHash(r))
	return task, found, err
}

func unknownFlowActions(ctx context.Context, q Queryer, root string) (bool, error) {
	var found bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_actions a JOIN flow_tasks t ON t.task_id=a.task_id WHERE t.root_task_id=$1 AND a.outcome IN ('dispatching','unknown')) OR EXISTS(SELECT 1 FROM embedding_calls e JOIN runs r ON r.run_id=e.run_id JOIN flow_tasks t ON t.task_id=r.flow_task_id WHERE t.root_task_id=$1 AND e.status IN ('pending','unknown')) OR EXISTS(SELECT 1 FROM flow_budget_allocations b JOIN runs r ON r.run_id=b.run_id WHERE b.root_task_id=$1 AND (b.state='unknown' OR b.state='pending' AND r.status<>'running'))`, root).Scan(&found)
	return found, err
}

func propagateFlowClose(ctx context.Context, tx *sql.Tx, parent flow.Task, intent flow.CloseIntent, joined, rescue bool, op, hash, reason string, p core.Principal, now int64) error {
	rows, err := tx.QueryContext(ctx, `WITH RECURSIVE subtree AS (
 SELECT task_id,0 AS level FROM flow_tasks WHERE task_id=$1
 UNION ALL SELECT t.task_id,s.level+1 FROM flow_tasks t JOIN subtree s ON t.parent_task_id=s.task_id WHERE s.level<3
 ) SELECT t.task_id FROM subtree s JOIN flow_tasks t ON t.task_id=s.task_id WHERE s.level>0 AND t.status NOT IN ('succeeded','failed','stopped','cancelled','abandoned') ORDER BY s.level DESC,t.task_id`, parent.ID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, id := range ids {
		child, err := readFlowTask(ctx, tx, id)
		if err != nil {
			return err
		}
		status, nextIntent, cancel, err := flow.CloseState(child, intent, joined, rescue)
		if err != nil {
			return err
		}
		if child.Status == flow.Running && (status == flow.Cancelled || status == flow.Abandoned) {
			if err := updateFlowHead(ctx, tx, child, flow.Cancelling, child.Version.Revision, child.Version.ExecutionEpoch, nextIntent, cancel, now, reason); err != nil {
				return err
			}
			if err := appendFlowTransition(ctx, tx, child, op, "ancestor-close-intent", hash, flow.Cancelling, child.Version.Revision, now, reason, p); err != nil {
				return err
			}
			child, err = readFlowTask(ctx, tx, id)
			if err != nil {
				return err
			}
		}
		if err := updateFlowHead(ctx, tx, child, status, child.Version.Revision, child.Version.ExecutionEpoch, nextIntent, cancel, now, reason); err != nil {
			return err
		}
		phase := "ancestor-close-intent"
		if joined {
			phase = "ancestor-close-join"
		}
		if err := appendFlowTransition(ctx, tx, child, op, phase, hash, status, child.Version.Revision, now, reason, p); err != nil {
			return err
		}
		if joined {
			if _, err := tx.ExecContext(ctx, `UPDATE flow_node_attempts SET state='interrupted',finished_at=$2 WHERE task_id=$1 AND state IN ('prepared','running')`, id, now); err != nil {
				return err
			}
		}
	}
	return nil
}

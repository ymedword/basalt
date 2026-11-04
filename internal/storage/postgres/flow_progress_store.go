package postgres

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// FlowProgress binds a guard to one real leaf attempt. History lives in the root
// checkpoint and uses the task/node identity, so subflows and revisions retain it.
type FlowProgress struct {
	Store                 *FlowStore
	Principal             core.Principal
	TaskID, NodeAttemptID string
	Proof                 func(context.Context, core.ToolCall, core.Result, core.ActionOutcome) (string, error)
}

var _ core.ProgressGuard = (*FlowProgress)(nil)

func (p *FlowProgress) BeforeCall(ctx context.Context, call core.ToolCall) error {
	signature, err := core.ProgressSignature(ctx, call)
	if err != nil {
		return err
	}
	return p.withWindow(ctx, false, func(w core.ProgressWindow) (core.ProgressWindow, error) {
		reason, err := w.CheckCandidate(signature)
		if err != nil {
			return w, &core.ProgressLimitError{Reason: reason}
		}
		return w, nil
	})
}

func (p *FlowProgress) AfterCall(ctx context.Context, call core.ToolCall, result core.Result, out core.ActionOutcome) error {
	signature, err := core.ProgressSignature(ctx, call)
	if err != nil {
		return err
	}
	if out.AuditID <= 0 || !out.Dispatched || !out.AuditConfirmed {
		return flow.ErrStateConflict
	}
	basis := ""
	if out.Outcome == string(core.LLMCallSuccess) && p.Proof != nil {
		var confirmed bool
		if p.Store == nil {
			return flow.ErrStateConflict
		}
		if err := p.Store.db.pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_action_attempts a JOIN tool_invocations i ON i.id=a.tool_invocation_id WHERE a.task_id=$1 AND a.node_attempt_id=$2 AND a.tool_invocation_id=$3 AND a.outcome='succeeded' AND i.status='success' AND a.output_hash=$4 AND a.output=i.result_json)`, p.TaskID, p.NodeAttemptID, out.AuditID, out.OutputHash).Scan(&confirmed); err != nil {
			return err
		}
		if !confirmed {
			return flow.ErrStateConflict
		}
		basis, err = p.Proof(ctx, call, result, out)
		if err != nil {
			return err
		}
	}
	return p.withWindow(ctx, true, func(w core.ProgressWindow) (core.ProgressWindow, error) {
		if w.LastAuditID == out.AuditID {
			return w, nil
		}
		if w.LastAuditID > out.AuditID {
			return w, flow.ErrStateConflict
		}
		var err error
		w, err = w.Observe(signature, out)
		if err != nil {
			return w, err
		}
		w.LastAuditID = out.AuditID
		if basis != "" {
			w, err = w.AcceptBasis(basis)
			if err != nil {
				return w, err
			}
		}
		return w, nil
	})
}

func (p *FlowProgress) withWindow(ctx context.Context, write bool, update func(core.ProgressWindow) (core.ProgressWindow, error)) error {
	if p == nil || p.Store == nil || !p.Principal.Valid() || p.NodeAttemptID == "" {
		return core.ErrUnauthorized
	}
	// Do not retry an unknown observation write: BeforeCall will fail closed on
	// this error, and restart reconciliation reads the original audit identity.
	var rootID, windowKey string
	var auditID int64
	return p.Store.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		if !write {
			return true, nil
		} // Read-only transaction has no replayable mutation.
		if rootID == "" || windowKey == "" || auditID <= 0 {
			return false, nil
		}
		var found bool
		err := q.QueryRowContext(ctx, `SELECT COALESCE((r.checkpoint->'progress'->$3->>'last_audit_id')::bigint=$4,false) FROM flow_tasks t JOIN flow_task_revisions r ON r.task_id=t.task_id AND r.revision=t.current_revision WHERE t.task_id=$1 AND t.caller_id=$2`, rootID, p.Principal.CallerID, windowKey, auditID).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, p.TaskID, p.Principal)
		if err != nil {
			return err
		}
		if !write {
			if task.Status != flow.Running {
				return flow.ErrStateConflict
			}
			if err := ensureFlowAncestorsOpen(ctx, tx, task, p.Store.db.clock().UnixMilli()); err != nil {
				return err
			}
		}
		var node, state string
		var revision, epoch int64
		if err := tx.QueryRowContext(ctx, `SELECT node_id,state,task_revision,execution_epoch FROM flow_node_attempts WHERE task_id=$1 AND node_attempt_id=$2`, task.ID, p.NodeAttemptID).Scan(&node, &state, &revision, &epoch); err != nil {
			return err
		}
		if state != "running" || revision != task.Version.Revision || epoch != task.Version.ExecutionEpoch {
			return flow.ErrStateConflict
		}
		root, err := readFlowTask(ctx, tx, task.RootTaskID)
		if err != nil {
			return err
		}
		key := task.ID + ":" + node
		rootID, windowKey = root.ID, key
		window := root.Revision.Progress[key]
		if !write {
			// A terminal action is already an actual completed observation even when
			// the subsequent window UPDATE failed. Never let a stale window grant a
			// fresh call on restart or on a new attempt of the same node.
			var latest int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(a.tool_invocation_id),0) FROM flow_action_attempts a JOIN flow_node_attempts n ON n.task_id=a.task_id AND n.node_attempt_id=a.node_attempt_id JOIN tool_invocations i ON i.id=a.tool_invocation_id WHERE a.task_id=$1 AND n.node_id=$2 AND a.outcome IN ('succeeded','failed') AND i.status<>'pending'`, task.ID, node).Scan(&latest); err != nil {
				return err
			}
			if latest != window.LastAuditID {
				return flow.ErrStateConflict
			}
		}
		next, err := update(window)
		if err != nil {
			return err
		}
		if !write {
			return nil
		}
		auditID = next.LastAuditID
		var confirmed bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_action_attempts a JOIN tool_invocations i ON i.id=a.tool_invocation_id WHERE a.task_id=$1 AND a.node_attempt_id=$2 AND a.tool_invocation_id=$3 AND a.outcome IN ('succeeded','failed') AND i.status<>'pending')`, task.ID, p.NodeAttemptID, next.LastAuditID).Scan(&confirmed); err != nil {
			return err
		}
		if !confirmed {
			return flow.ErrStateConflict
		}
		if root.Revision.Progress == nil {
			root.Revision.Progress = make(map[string]core.ProgressWindow)
		}
		root.Revision.Progress[key] = next
		raw, err := json.Marshal(root.Revision)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE flow_task_revisions SET checkpoint=$3 WHERE task_id=$1 AND revision=$2`, root.ID, root.Version.Revision, raw)
		return err
	})
}

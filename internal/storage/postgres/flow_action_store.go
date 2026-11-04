package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// FlowActions binds a leaf's original owner and attempt to the durable action
// ledger. Recheck is an app-owned current authority check, run before each short
// transaction; key liveness is also checked in the dispatch transaction itself.
type FlowActions struct {
	Store                 *FlowStore
	Principal             core.Principal
	TaskID, NodeAttemptID string
	Recheck               func(context.Context) error
}

var _ core.FlowActionLedger = (*FlowActions)(nil)

func (a *FlowActions) check(ctx context.Context) error {
	if a == nil || a.Store == nil || !a.Principal.Valid() || a.TaskID == "" || a.NodeAttemptID == "" {
		return core.ErrUnauthorized
	}
	if a.Recheck != nil {
		return a.Recheck(ctx)
	}
	return core.ErrUnauthorized // Every production leaf requires the app recheck.
}

func (a *FlowActions) Prepare(ctx context.Context, req core.ActionRequest) (core.ActionTicket, error) {
	var ticket core.ActionTicket
	if err := a.check(ctx); err != nil {
		return ticket, err
	}
	input, err := flow.CanonicalJSON(ctx, req.Input)
	if err != nil || req.TaskID != a.TaskID || req.InputHash != flowDataHash(input) || req.AdapterVersion == "" || req.SchemaHash == "" || req.LogicalSlot == "" || req.ToolName == "" {
		return ticket, flow.ErrStateConflict
	}
	identity := mustFlowJSON([]string{req.RootTaskID, req.TaskID, req.NodeInvocationID, req.LogicalSlot})
	actionID := "fa_" + flowDataHash(identity)
	attemptID := "faa_" + flowDataHash(mustFlowJSON([]string{actionID, a.NodeAttemptID, req.RunID}))
	ticket = core.ActionTicket{ActionID: actionID, AttemptID: attemptID, TaskID: req.TaskID, RunID: req.RunID, InputHash: req.InputHash, TaskRevision: req.TaskRevision, ExecutionEpoch: req.ExecutionEpoch}
	err = a.Store.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_action_attempts WHERE action_attempt_id=$1 AND task_id=$2 AND run_id=$3 AND task_revision=$4 AND execution_epoch=$5)`, attemptID, req.TaskID, req.RunID, req.TaskRevision, req.ExecutionEpoch).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, req.TaskID, a.Principal)
		if err != nil {
			return err
		}
		if task.RootTaskID != req.RootTaskID || task.Status != flow.Running || task.Version.Revision != req.TaskRevision || task.Version.ExecutionEpoch != req.ExecutionEpoch {
			return flow.ErrStateConflict
		}
		if task.Contract.Scope.CallerID != task.CallerID || task.Contract.Scope.KeyID != task.AuthorizingKeyID || !slices.Contains(task.Contract.Scope.Tools, req.ToolName) || !slices.Contains(task.Contract.Scope.Scopes, core.ScopeFlowsWrite) {
			return core.ErrUnauthorized
		}
		if err := ensureFlowAncestorsOpen(ctx, tx, task, a.Store.db.clock().UnixMilli()); err != nil {
			return err
		}
		var invocation, runID, state string
		var epoch, revision int64
		if err := tx.QueryRowContext(ctx, `SELECT node_invocation_id,COALESCE(run_id,''),state,task_revision,execution_epoch FROM flow_node_attempts WHERE task_id=$1 AND node_attempt_id=$2`, task.ID, a.NodeAttemptID).Scan(&invocation, &runID, &state, &revision, &epoch); err != nil {
			return err
		}
		if invocation != req.NodeInvocationID || runID != req.RunID || state != "running" || revision != req.TaskRevision || epoch != req.ExecutionEpoch {
			return flow.ErrStateConflict
		}
		var oldHash, oldTool, oldAdapter, oldSchema, oldClass, outcome string
		err = tx.QueryRowContext(ctx, `SELECT input_hash,tool_name,adapter_version,schema_hash,recovery_class,outcome FROM flow_actions WHERE task_id=$1 AND action_id=$2`, task.ID, actionID).Scan(&oldHash, &oldTool, &oldAdapter, &oldSchema, &oldClass, &outcome)
		if err == nil {
			if oldHash != req.InputHash || oldTool != req.ToolName || oldAdapter != req.AdapterVersion || oldSchema != req.SchemaHash || oldClass != req.RecoveryClass {
				return flow.ErrActionUnknown
			}
			if outcome == "unknown" || outcome == "dispatching" || outcome == "succeeded" {
				return flow.ErrActionUnknown
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO flow_actions(task_id,action_id,node_invocation_id,logical_slot,tool_name,adapter_version,schema_hash,input,input_hash,recovery_class,outcome,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'prepared',$11)`, task.ID, actionID, req.NodeInvocationID, req.LogicalSlot, req.ToolName, req.AdapterVersion, req.SchemaHash, string(input), req.InputHash, req.RecoveryClass, a.Store.db.clock().UnixMilli()); err != nil {
				return err
			}
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT outcome FROM flow_action_attempts WHERE action_attempt_id=$1`, attemptID).Scan(&existing)
		if err == nil {
			if existing != "prepared" {
				return flow.ErrActionUnknown
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO flow_action_attempts(task_id,action_id,action_attempt_id,node_attempt_id,run_id,task_revision,execution_epoch,outcome,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,'prepared',$8)`, task.ID, actionID, attemptID, a.NodeAttemptID, req.RunID, req.TaskRevision, req.ExecutionEpoch, a.Store.db.clock().UnixMilli())
		return err
	})
	return ticket, err
}

func (a *FlowActions) BeforeDispatch(ctx context.Context, ticket core.ActionTicket, auditID int64) error {
	if err := a.check(ctx); err != nil {
		return err
	}
	if ticket.TaskID != a.TaskID || auditID <= 0 {
		return flow.ErrStateConflict
	}
	return a.Store.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_action_attempts WHERE action_attempt_id=$1 AND tool_invocation_id=$2 AND outcome='dispatching')`, ticket.AttemptID, auditID).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, a.TaskID, a.Principal)
		if err != nil {
			return err
		}
		if task.Status != flow.Running || task.Version.Revision != ticket.TaskRevision || task.Version.ExecutionEpoch != ticket.ExecutionEpoch {
			return flow.ErrStateConflict
		}
		if err := ensureFlowAncestorsOpen(ctx, tx, task, a.Store.db.clock().UnixMilli()); err != nil {
			return err
		}
		if err := checkFlowAuthorizingKey(ctx, tx, task, a.Principal, a.Store.db.clock().UnixMilli()); err != nil {
			return err
		}
		var actionState, inputHash, toolName, runID, attemptState string
		if err := tx.QueryRowContext(ctx, `SELECT a.outcome,a.input_hash,a.tool_name,t.run_id,t.outcome FROM flow_actions a JOIN flow_action_attempts t ON t.task_id=a.task_id AND t.action_id=a.action_id WHERE a.task_id=$1 AND a.action_id=$2 AND t.action_attempt_id=$3 AND t.node_attempt_id=$4`, task.ID, ticket.ActionID, ticket.AttemptID, a.NodeAttemptID).Scan(&actionState, &inputHash, &toolName, &runID, &attemptState); err != nil {
			return err
		}
		if actionState == "unknown" || actionState == "dispatching" || actionState == "succeeded" || attemptState != "prepared" || runID != ticket.RunID || inputHash != ticket.InputHash {
			return flow.ErrActionUnknown
		}
		if !slices.Contains(task.Contract.Scope.Tools, toolName) {
			return core.ErrUnauthorized
		}
		var auditRun, auditTool, auditStatus string
		var auditInput []byte
		if err := tx.QueryRowContext(ctx, `SELECT run_id,tool_name,status,input_json FROM tool_invocations WHERE id=$1`, auditID).Scan(&auditRun, &auditTool, &auditStatus, &auditInput); err != nil {
			return err
		}
		auditInput, err = flow.CanonicalJSON(ctx, auditInput)
		if err != nil {
			return err
		}
		if auditRun != runID || auditTool != toolName || auditStatus != "pending" || flowDataHash(auditInput) != inputHash {
			return flow.ErrStateConflict
		}
		var runBound bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE run_id=$1 AND caller_id=$2 AND flow_task_id=$3 AND flow_node_attempt_id=$4 AND status='running' AND authorizing_kind=$5 AND key_id IS NOT DISTINCT FROM $6::text)`, runID, task.CallerID, task.ID, a.NodeAttemptID, task.AuthorizingKind, nullIfEmpty(task.AuthorizingKeyID)).Scan(&runBound); err != nil {
			return err
		}
		if !runBound {
			return flow.ErrStateConflict
		}
		var budgetBound bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_budget_allocations WHERE task_id=$1 AND run_id=$2 AND action_id=$3 AND state='pending' AND reserved_tool_calls>=1)`, task.ID, runID, ticket.ActionID).Scan(&budgetBound); err != nil {
			return err
		}
		if !budgetBound {
			return flow.ErrBudget
		}
		now := a.Store.db.clock().UnixMilli()
		if _, err := tx.ExecContext(ctx, `UPDATE flow_action_attempts SET tool_invocation_id=$2,outcome='dispatching',dispatched_at=$3 WHERE action_attempt_id=$1`, ticket.AttemptID, auditID, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE flow_actions SET outcome='dispatching' WHERE task_id=$1 AND action_id=$2`, task.ID, ticket.ActionID)
		return err
	})
}

func (a *FlowActions) RecordOutcome(ctx context.Context, ticket core.ActionTicket, out core.ActionOutcome) error {
	if a == nil || a.Store == nil || !a.Principal.Valid() || ticket.TaskID != a.TaskID {
		return core.ErrUnauthorized
	}
	target := "not_issued"
	return a.Store.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_action_attempts WHERE action_attempt_id=$1 AND outcome=$2)`, ticket.AttemptID, target).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, a.TaskID, a.Principal)
		if err != nil {
			return err
		}
		var status, class string
		var boundAudit sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT t.outcome,a.recovery_class,t.tool_invocation_id FROM flow_action_attempts t JOIN flow_actions a ON a.task_id=t.task_id AND a.action_id=t.action_id WHERE t.task_id=$1 AND t.action_id=$2 AND t.action_attempt_id=$3 AND t.node_attempt_id=$4 AND t.run_id=$5`, task.ID, ticket.ActionID, ticket.AttemptID, a.NodeAttemptID, ticket.RunID).Scan(&status, &class, &boundAudit); err != nil {
			return err
		}
		if status != "prepared" && status != "dispatching" {
			return flow.ErrActionUnknown
		}
		if out.Dispatched {
			if status != "dispatching" || !boundAudit.Valid || out.AuditID != boundAudit.Int64 {
				return flow.ErrStateConflict
			}
			var auditStatus string
			var output []byte
			if err := tx.QueryRowContext(ctx, `SELECT status,COALESCE(result_json,'') FROM tool_invocations WHERE id=$1 AND run_id=$2`, out.AuditID, ticket.RunID).Scan(&auditStatus, &output); err != nil {
				return err
			}
			target = "unknown"
			if out.AuditConfirmed && out.OutputHash == flowDataHash(output) && string(out.Output) == string(output) {
				if auditStatus == "success" && out.Outcome == "success" {
					target = "succeeded"
				}
				if auditStatus == "failed" && out.Outcome == "failed" && class == "readonly" {
					target = "failed"
				}
			}
		} else if status == "dispatching" {
			return flow.ErrActionUnknown
		}
		var audit any
		if out.AuditID > 0 {
			audit = out.AuditID
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flow_action_attempts SET tool_invocation_id=COALESCE(tool_invocation_id,$2),outcome=$3,output=$4,output_hash=$5,finished_at=$6 WHERE action_attempt_id=$1`, ticket.AttemptID, audit, target, string(out.Output), nullIfEmpty(out.OutputHash), a.Store.db.clock().UnixMilli()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE flow_actions SET outcome=$3 WHERE task_id=$1 AND action_id=$2`, task.ID, ticket.ActionID, target)
		return err
	})
}

func checkFlowAuthorizingKey(ctx context.Context, q Queryer, task flow.Task, p core.Principal, now int64) error {
	if task.AuthorizingKind == string(core.AuthorizingKindAdminBuiltin) {
		if p.Kind != core.PrincipalLocalAdmin {
			return core.ErrUnauthorized
		}
		return nil
	}
	if p.Kind != core.PrincipalCaller || p.KeyID != task.AuthorizingKeyID || p.CallerID != task.CallerID {
		return core.ErrUnauthorized
	}
	var valid bool
	agentName := ""
	controlNode := false
	for _, node := range task.AcceptedVersion.Definition.Nodes {
		if node.ID == task.Revision.NodeID && node.Agent != nil {
			agentName = node.Agent.Name
			break
		}
		if node.ID == task.Revision.NodeID && (node.Kind == "branch" || node.Kind == "subflow") {
			controlNode = true
		}
	}
	if agentName == "" && !controlNode {
		return core.ErrUnauthorized
	}
	agentScope := ""
	if agentName != "" {
		agentScope = core.AgentInvokeScopePrefix + agentName
	}
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM caller_keys k JOIN callers c ON c.caller_id=k.caller_id WHERE k.key_id=$1 AND k.caller_id=$2 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>$3) AND c.status='active' AND 'flows:write'=ANY(k.scopes) AND ($4='' OR $4=ANY(k.scopes)))`, p.KeyID, p.CallerID, now, agentScope).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return core.ErrUnauthorized
	}
	return nil
}

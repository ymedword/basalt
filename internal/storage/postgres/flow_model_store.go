package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// FlowModels binds the original ProviderService boundary to the same durable
// action and ancestor-budget tables. Recheck and Estimate are app-owned policy;
// no prompt, credential or second llm_calls row is written here.
type FlowModels struct {
	Store                 *FlowStore
	Principal             core.Principal
	TaskID, NodeAttemptID string
	Recheck               func(context.Context, core.ChatRequest) error
	Estimate              func(int) int64
}

type flowModelPermit struct {
	owner                            *FlowModels
	request                          core.ChatRequest
	requestHash, actionID, attemptID string
	epoch, revision                  int64
	budget                           core.BudgetTicket
}

var _ core.FlowModelLedger = (*FlowModels)(nil)

func (m *FlowModels) PrepareModel(ctx context.Context, req core.ChatRequest, estimatedInput int) (core.FlowModelPermit, error) {
	if m == nil || m.Store == nil || m.Recheck == nil || m.Estimate == nil || !m.Principal.Valid() || req.Flow == nil || req.Flow.TaskID != m.TaskID || req.Flow.NodeAttemptID != m.NodeAttemptID || req.RunID == "" || req.Round < 1 || req.Provider == "" || req.Model == "" || estimatedInput < 0 {
		return nil, core.ErrUnauthorized
	}
	frozen, hash, err := core.SnapshotFlowModelRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := m.Recheck(ctx, frozen); err != nil {
		return nil, err
	}
	_, after, err := core.SnapshotFlowModelRequest(ctx, req)
	if err != nil || after != hash {
		return nil, flow.ErrStateConflict
	}
	permit := &flowModelPermit{owner: m, request: req, requestHash: hash}
	estimate := m.Estimate(estimatedInput)
	if estimate < 1 {
		return nil, flow.ErrBudget
	}
	// A bounded new leaf may perform new read-only inference. Its original
	// attempt owns the model call identity; business-tool slots remain stable
	// across retries/revisions and unknown root outcomes still stop this path.
	logical := "model:" + m.NodeAttemptID + ":" + strconv.Itoa(req.Round)
	permit.actionID = "fa_" + flowDataHash(mustFlowJSON([]string{req.Flow.RootTaskID, m.TaskID, req.Flow.NodeInvocationID, logical}))
	permit.attemptID = "faa_" + flowDataHash(mustFlowJSON([]string{permit.actionID, m.NodeAttemptID, req.RunID}))
	metadata := mustFlowJSON(struct {
		RequestHash, Provider, Model string
		Round                        int
	}{hash, req.Provider, req.Model, req.Round})
	err = m.Store.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_action_attempts WHERE action_attempt_id=$1 AND task_id=$2 AND outcome='prepared')`, permit.attemptID, m.TaskID).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, m.TaskID, m.Principal)
		if err != nil {
			return err
		}
		if err := permit.checkBound(ctx, tx, task); err != nil {
			return err
		}
		permit.epoch, permit.revision = task.Version.ExecutionEpoch, task.Version.Revision
		var oldHash, outcome string
		err = tx.QueryRowContext(ctx, `SELECT input_hash,outcome FROM flow_actions WHERE task_id=$1 AND action_id=$2`, m.TaskID, permit.actionID).Scan(&oldHash, &outcome)
		if err == nil {
			if oldHash != flowDataHash(metadata) || outcome == "dispatching" || outcome == "unknown" || outcome == "succeeded" {
				return flow.ErrActionUnknown
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else {
			if _, err := tx.ExecContext(ctx, `INSERT INTO flow_actions(task_id,action_id,node_invocation_id,logical_slot,tool_name,adapter_version,schema_hash,input,input_hash,recovery_class,outcome,created_at) VALUES($1,$2,$3,$4,$5,'model-flow-v1',$6,$7,$8,'readonly','prepared',$9)`, m.TaskID, permit.actionID, req.Flow.NodeInvocationID, logical, "model:"+req.Provider, flowDataHash([]byte("model-flow-v1")), string(metadata), flowDataHash(metadata), m.Store.db.clock().UnixMilli()); err != nil {
				return err
			}
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT outcome FROM flow_action_attempts WHERE action_attempt_id=$1`, permit.attemptID).Scan(&existing)
		if err == nil {
			if existing != "prepared" {
				return flow.ErrActionUnknown
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO flow_action_attempts(task_id,action_id,action_attempt_id,node_attempt_id,run_id,task_revision,execution_epoch,outcome,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,'prepared',$8)`, m.TaskID, permit.actionID, permit.attemptID, m.NodeAttemptID, req.RunID, task.Version.Revision, task.Version.ExecutionEpoch, m.Store.db.clock().UnixMilli())
		return err
	})
	if err != nil {
		return nil, err
	}
	budget := &FlowBudget{Store: m.Store, Principal: m.Principal, TaskID: m.TaskID}
	permit.budget, err = budget.Reserve(ctx, req.RunID, permit.actionID, core.UsageEstimate{Tokens: estimate})
	if err != nil {
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return nil, errors.Join(err, permit.RecordOutcome(finish, core.FlowModelOutcome{}))
	}
	return permit, nil
}

func (p *flowModelPermit) checkBound(ctx context.Context, q Queryer, task flow.Task) error {
	r := p.request
	m := p.owner
	if task.Status != flow.Running || task.Version.Revision != r.Flow.TaskRevision || task.RootTaskID != r.Flow.RootTaskID || r.ConfigHash != r.Flow.AgentConfigHash || !slices.Contains(task.Contract.Scope.AgentUIDs, r.Flow.AgentUID) || !slices.Contains(task.Contract.Scope.Scopes, core.ScopeFlowsWrite) {
		return flow.ErrStateConflict
	}
	if err := ensureFlowAncestorsOpen(ctx, q, task, m.Store.db.clock().UnixMilli()); err != nil {
		return err
	}
	if err := checkFlowAuthorizingKey(ctx, q, task, m.Principal, m.Store.db.clock().UnixMilli()); err != nil {
		return err
	}
	unknown, err := unknownFlowActions(ctx, q, task.RootTaskID)
	if err != nil {
		return err
	}
	if unknown {
		return flow.ErrActionUnknown
	}
	var manifestRaw, configHash, manifestHash, invocation, state string
	var revision, epoch int64
	err = q.QueryRowContext(ctx, `SELECT r.config_hash,f.manifest_hash,f.snapshot_json,n.node_invocation_id,n.state,n.task_revision,n.execution_epoch FROM runs r JOIN flow_node_attempts n ON n.task_id=r.flow_task_id AND n.node_attempt_id=r.flow_node_attempt_id JOIN run_resource_manifests f ON f.run_id=r.run_id WHERE r.run_id=$1 AND r.caller_id=$2 AND r.flow_task_id=$3 AND r.flow_node_attempt_id=$4 AND r.status='running'`, r.RunID, m.Principal.CallerID, m.TaskID, m.NodeAttemptID).Scan(&configHash, &manifestHash, &manifestRaw, &invocation, &state, &revision, &epoch)
	if err != nil {
		return err
	}
	if configHash != r.ConfigHash || manifestHash != r.Flow.ResourceManifestHash || invocation != r.Flow.NodeInvocationID || state != "running" || revision != task.Version.Revision || epoch != task.Version.ExecutionEpoch {
		return flow.ErrStateConflict
	}
	var manifest core.RunResourceManifest
	if err := json.Unmarshal([]byte(manifestRaw), &manifest); err != nil {
		return err
	}
	hash, err := core.HashManifest(manifest)
	if err != nil || hash != manifestHash || manifest.AgentUID != core.AgentUID(r.Flow.AgentUID) || manifest.Provider.Name != r.Provider || manifest.Provider.Model != r.Model {
		return flow.ErrStateConflict
	}
	return nil
}

func (p *flowModelPermit) BeforeDispatch(ctx context.Context, auditID int64, req core.ChatRequest) error {
	if auditID <= 0 || ctx.Err() != nil {
		return core.ErrFlowDispatch
	}
	_, hash, err := core.SnapshotFlowModelRequest(ctx, req)
	if err != nil || hash != p.requestHash || req.RunID != p.request.RunID || req.Round != p.request.Round {
		return flow.ErrStateConflict
	}
	if err := p.owner.Recheck(ctx, req); err != nil {
		return err
	}
	_, hash, err = core.SnapshotFlowModelRequest(ctx, req)
	if err != nil || hash != p.requestHash {
		return flow.ErrStateConflict
	}
	m := p.owner
	return m.Store.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_action_attempts WHERE action_attempt_id=$1 AND llm_call_id=$2 AND outcome='dispatching')`, p.attemptID, auditID).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, m.TaskID, m.Principal)
		if err != nil {
			return err
		}
		if err := p.checkBound(ctx, tx, task); err != nil {
			return err
		}
		if task.Version.Revision != p.revision || task.Version.ExecutionEpoch != p.epoch {
			return flow.ErrStateConflict
		}
		unknown, err := unknownFlowActions(ctx, tx, task.RootTaskID)
		if err != nil {
			return err
		}
		if unknown {
			return flow.ErrActionUnknown
		}
		var bound bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM llm_calls l JOIN flow_action_attempts a ON a.run_id=l.run_id WHERE a.action_attempt_id=$1 AND a.outcome='prepared' AND a.tool_invocation_id IS NULL AND a.llm_call_id IS NULL AND l.id=$2 AND l.status='pending' AND l.provider=$3 AND l.model=$4 AND l.round=$5 AND l.config_hash=$6)`, p.attemptID, auditID, req.Provider, req.Model, req.Round, req.ConfigHash).Scan(&bound)
		if err != nil {
			return err
		}
		if !bound {
			return flow.ErrStateConflict
		}
		var allocated bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_budget_allocations WHERE allocation_id=$1 AND task_id=$2 AND run_id=$3 AND action_id=$4 AND state='pending' AND reserved_tokens>0)`, p.budget.ID, m.TaskID, req.RunID, p.actionID).Scan(&allocated); err != nil {
			return err
		}
		if !allocated {
			return flow.ErrBudget
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flow_action_attempts SET llm_call_id=$2,outcome='dispatching',dispatched_at=$3 WHERE action_attempt_id=$1`, p.attemptID, auditID, m.Store.db.clock().UnixMilli()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE flow_actions SET outcome='dispatching' WHERE task_id=$1 AND action_id=$2`, m.TaskID, p.actionID)
		return err
	})
}

func (p *flowModelPermit) RecordOutcome(ctx context.Context, out core.FlowModelOutcome) error {
	m := p.owner
	target := "unknown"
	known := false
	var tokens int64
	var usageErr error
	err := m.Store.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_action_attempts WHERE action_attempt_id=$1 AND outcome=$2)`, p.attemptID, target).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := lockedFlowOwnedTask(ctx, tx, m.TaskID, m.Principal); err != nil {
			return err
		}
		var state string
		var bound sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT outcome,llm_call_id FROM flow_action_attempts WHERE task_id=$1 AND action_attempt_id=$2 AND node_attempt_id=$3 AND run_id=$4`, m.TaskID, p.attemptID, m.NodeAttemptID, p.request.RunID).Scan(&state, &bound); err != nil {
			return err
		}
		if state != "prepared" && state != "dispatching" {
			return flow.ErrActionUnknown
		}
		if !out.Dispatched && state == "prepared" {
			target, known = "not_issued", true
		} else {
			if !bound.Valid || bound.Int64 != out.AuditID {
				return flow.ErrStateConflict
			}
			var status, message string
			var prompt, completion, total int64
			if err := tx.QueryRowContext(ctx, `SELECT status,COALESCE(error_message,''),prompt_tokens,completion_tokens,total_tokens FROM llm_calls WHERE id=$1 AND run_id=$2`, out.AuditID, p.request.RunID).Scan(&status, &message, &prompt, &completion, &total); err != nil {
				return err
			}
			if !out.Dispatched && out.AuditConfirmed && status == "failed" && message == "flow dispatch refused before model call" {
				target, known = "not_issued", true
			} else if out.Dispatched && out.AuditConfirmed && status == string(out.Status) {
				if status == "success" {
					target = "succeeded"
				} else if status == "failed" {
					target = "failed"
				}
				if status == "success" && total == int64(out.Usage.TotalTokens) && prompt == int64(out.Usage.PromptTokens) && completion == int64(out.Usage.CompletionTokens) {
					tokens, known, usageErr = core.FlowUsageTotal(out.Usage)
					if usageErr != nil {
						target = "unknown"
					}
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flow_action_attempts SET llm_call_id=COALESCE(llm_call_id,$2),outcome=$3,finished_at=$4 WHERE action_attempt_id=$1`, p.attemptID, nullableFlowAudit(out.AuditID), target, m.Store.db.clock().UnixMilli()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE flow_actions SET outcome=$3 WHERE task_id=$1 AND action_id=$2`, m.TaskID, p.actionID, target)
		return err
	})
	if err != nil {
		return err
	}
	if p.budget.ID == "" {
		return nil
	}
	budget := &FlowBudget{Store: m.Store, Principal: m.Principal, TaskID: m.TaskID}
	if known {
		return budget.Settle(ctx, p.budget, core.UsageActual{Tokens: tokens, Known: true})
	}
	return errors.Join(usageErr, budget.KeepUnknown(ctx, p.budget))
}

func nullableFlowAudit(id int64) any {
	if id > 0 {
		return id
	}
	return nil
}

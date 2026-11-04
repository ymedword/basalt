package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// FlowBudget applies every allocation to all persisted ancestors. Reservations
// survive uncertain calls and remain distinct from caller period quotas.
type FlowBudget struct {
	Store     *FlowStore
	Principal core.Principal
	TaskID    string
}

var _ core.AncestorBudget = (*FlowBudget)(nil)

func (b *FlowBudget) Reserve(ctx context.Context, runID, actionID string, estimate core.UsageEstimate) (core.BudgetTicket, error) {
	var ticket core.BudgetTicket
	if b == nil || b.Store == nil || !b.Principal.Valid() {
		return ticket, core.ErrUnauthorized
	}
	if runID == "" || actionID == "" || estimate.Tokens < 0 || estimate.ToolCalls < 0 || estimate.Tokens == 0 && estimate.ToolCalls == 0 {
		return ticket, flow.ErrBudget
	}
	id := "fb_" + flowDataHash(mustFlowJSON([]string{b.TaskID, runID, actionID}))
	ticket = core.BudgetTicket{ID: id, TaskID: b.TaskID, RunID: runID, ActionID: actionID, Reserved: estimate}
	err := b.Store.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_budget_allocations WHERE task_id=$1 AND allocation_id=$2 AND reserved_tokens=$3 AND reserved_tool_calls=$4)`, b.TaskID, id, estimate.Tokens, estimate.ToolCalls).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, b.TaskID, b.Principal)
		if err != nil {
			return err
		}
		if task.Status != flow.Running {
			return flow.ErrStateConflict
		}
		if err := ensureFlowAncestorsOpen(ctx, tx, task, b.Store.db.clock().UnixMilli()); err != nil {
			return err
		}
		var old core.UsageEstimate
		var oldRun, oldAction string
		err = tx.QueryRowContext(ctx, `SELECT reserved_tokens,reserved_tool_calls,run_id,action_id FROM flow_budget_allocations WHERE root_task_id=$1 AND allocation_id=$2`, task.RootTaskID, id).Scan(&old.Tokens, &old.ToolCalls, &oldRun, &oldAction)
		if err == nil {
			if old != estimate || oldRun != runID || oldAction != actionID {
				return flow.ErrStateConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ancestor := task
		for depth := 0; depth <= task.Depth; depth++ {
			var tokens, tools int64
			err := tx.QueryRowContext(ctx, `WITH RECURSIVE subtree AS (
 SELECT task_id,0 AS level FROM flow_tasks WHERE task_id=$1
 UNION ALL SELECT t.task_id,s.level+1 FROM flow_tasks t JOIN subtree s ON t.parent_task_id=s.task_id WHERE s.level<3
 ) SELECT COALESCE(sum(CASE WHEN a.state='settled' THEN a.actual_tokens ELSE a.reserved_tokens END),0),COALESCE(sum(CASE WHEN a.state='settled' THEN a.actual_tool_calls ELSE a.reserved_tool_calls END),0) FROM flow_budget_allocations a JOIN subtree s ON s.task_id=a.task_id`, ancestor.ID).Scan(&tokens, &tools)
			if err != nil {
				return err
			}
			if tokens > ancestor.Contract.Budget.MaxTotalTokens || tools > ancestor.Contract.Budget.MaxToolCalls || estimate.Tokens > ancestor.Contract.Budget.MaxTotalTokens-tokens || estimate.ToolCalls > ancestor.Contract.Budget.MaxToolCalls-tools {
				return flow.ErrBudget
			}
			if ancestor.ParentTaskID == "" {
				break
			}
			ancestor, err = readFlowTask(ctx, tx, ancestor.ParentTaskID)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO flow_budget_allocations(task_id,root_task_id,allocation_id,run_id,action_id,reserved_tokens,reserved_tool_calls,state,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,'pending',$8)`, task.ID, task.RootTaskID, id, runID, actionID, estimate.Tokens, estimate.ToolCalls, b.Store.db.clock().UnixMilli())
		return err
	})
	return ticket, err
}

func (b *FlowBudget) Settle(ctx context.Context, ticket core.BudgetTicket, actual core.UsageActual) error {
	if !actual.Known {
		return b.KeepUnknown(ctx, ticket)
	}
	if actual.Tokens < 0 || actual.ToolCalls < 0 {
		return flow.ErrBudget
	}
	return b.finish(ctx, ticket, actual, false)
}

func (b *FlowBudget) KeepUnknown(ctx context.Context, ticket core.BudgetTicket) error {
	return b.finish(ctx, ticket, core.UsageActual{}, true)
}

func (b *FlowBudget) finish(ctx context.Context, ticket core.BudgetTicket, actual core.UsageActual, unknown bool) error {
	if b == nil || b.Store == nil || !b.Principal.Valid() || ticket.TaskID != b.TaskID {
		return core.ErrUnauthorized
	}
	target := "settled"
	if unknown {
		target = "unknown"
	}
	return b.Store.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_budget_allocations WHERE task_id=$1 AND allocation_id=$2 AND state=$3)`, b.TaskID, ticket.ID, target).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, b.TaskID, b.Principal)
		if err != nil {
			return err
		}
		var state, runID, actionID string
		var reserved core.UsageEstimate
		var tokens, tools sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT state,run_id,action_id,reserved_tokens,reserved_tool_calls,actual_tokens,actual_tool_calls FROM flow_budget_allocations WHERE root_task_id=$1 AND task_id=$2 AND allocation_id=$3`, task.RootTaskID, task.ID, ticket.ID).Scan(&state, &runID, &actionID, &reserved.Tokens, &reserved.ToolCalls, &tokens, &tools); err != nil {
			return err
		}
		if runID != ticket.RunID || actionID != ticket.ActionID || reserved != ticket.Reserved {
			return flow.ErrStateConflict
		}
		if state == "settled" {
			if !unknown && tokens.Int64 == actual.Tokens && tools.Int64 == actual.ToolCalls {
				return nil
			}
			return flow.ErrStateConflict
		}
		if state == "unknown" {
			if unknown {
				return nil
			}
			return flow.ErrActionUnknown
		}
		if unknown {
			_, err = tx.ExecContext(ctx, `UPDATE flow_budget_allocations SET state='unknown' WHERE root_task_id=$1 AND allocation_id=$2 AND state='pending'`, task.RootTaskID, ticket.ID)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE flow_budget_allocations SET state='settled',actual_tokens=$3,actual_tool_calls=$4,settled_at=$5 WHERE root_task_id=$1 AND allocation_id=$2 AND state='pending'`, task.RootTaskID, ticket.ID, actual.Tokens, actual.ToolCalls, b.Store.db.clock().UnixMilli())
		}
		return err
	})
}

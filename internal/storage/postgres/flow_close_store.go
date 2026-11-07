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
	return s.closeFlowTask(ctx, r.Principal, r.TaskID, r.OperationID, r.Reason, r.Expected, flow.CloseCancel, req.Joined, req.Isolated, false, req.Now)
}

func (s *FlowStore) Abandon(ctx context.Context, req flow.AbandonTask) (flow.Task, error) {
	r := req.Request
	if !r.Principal.Valid() || r.Principal.Kind != core.PrincipalLocalAdmin || r.Actor.Subject == "" || r.Actor.Method == "" || r.Actor.Source == "" || strings.TrimSpace(r.Reason) == "" {
		return flow.Task{}, core.ErrUnauthorized
	}
	return s.closeFlowTask(ctx, r.Principal, r.TaskID, r.OperationID, r.Reason, r.Expected, flow.CloseAbandon, req.Joined, req.Isolated, req.NoAvailableCallerKey, req.Now)
}

// closeFlowTask 收尾一个任务。两个判据各管一段（US-24 §3.1.7 / FR-24-38）：
// joined 决定关闭阶段名（close-intent / close-join）与 CloseState 的目标状态；
// isolated 决定 attempt 收敛——只有旧执行已隔离（根子树没有仍有效的 run 租约）时，
// 才把整棵子树的 prepared / running attempt 标记为 interrupted，避免把别的 Worker
// 正在跑的子 Run 记成已中断。
// joined selects the close phase and target state; isolated gates attempt convergence.
func (s *FlowStore) closeFlowTask(ctx context.Context, p core.Principal, id, op, reason string, expected flow.TaskVersion, intent flow.CloseIntent, joined, isolated, rescue bool, now int64) (flow.Task, error) {
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
		if isolated {
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
		if err := propagateFlowClose(ctx, tx, task, intent, joined, isolated, rescue, op, hash, reason, p, now); err != nil {
			return err
		}
		result, err = readFlowTask(ctx, tx, id)
		return err
	})
	return result, err
}

// subtreeRunLeaseLockQuery 锁住根子树**现存**的全部 run 租约行（US-24 §2.3.6 注记 /
// 评审 IDX-13）：它与 `FencedTx` 的"租约行 → 业务行"加锁顺序共用同一批行，从而与
// "过期前已开始、正在提交"的那一笔串行化——对方持锁时本事务等待，对方提交后再重读。
// `FOR UPDATE OF l` 只锁租约行，不锁 runs / flow_tasks，避免与 fenced 写入形成反向锁序。
// Lock the subtree's run lease rows only; fenced writes lock the same rows first, so this
// serializes Recover with a write that began before expiry and is committing now.
const subtreeRunLeaseLockQuery = `
SELECT 1
  FROM flow_tasks t
  JOIN runs r ON r.flow_task_id = t.task_id
  JOIN execution_leases l ON l.kind = 'run' AND l.resource_id = r.run_id
 WHERE t.root_task_id = $1
 ORDER BY l.resource_id
 FOR UPDATE OF l`

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
		// Isolated 必须以**同一事务内、锁之后的实测**为准（US-24 §2.3.6 注记 / 评审 IDX-13）：
		// 调用方在事务外算出的 req.Isolated 只是提示。先锁子树现存的 run 租约行，与
		// "过期前已开始、正在提交"的那一笔串行化；持锁后再按数据库时钟重读判据，仍能
		// 看到有效租约时一律不收敛 attempt，也不得把根写回 Queued。
		// The caller's Isolated bool is only a hint: lock the subtree's run lease rows, then
		// re-read the predicate with database time inside this same transaction.
		isolated := req.Isolated
		if isolated {
			if _, err := tx.ExecContext(ctx, subtreeRunLeaseLockQuery, task.RootTaskID); err != nil {
				return err
			}
			live, err := querySubtreeExists(ctx, tx, subtreeLiveRunLeaseQuery, task.RootTaskID, "Recover.Isolated")
			if err != nil {
				return err
			}
			isolated = !live
		}
		// 事务外的 Isolated 快照被锁后实测推翻：写 Blocked 而不是排队推进。
		// A contradicted pre-transaction snapshot must fall back to Blocked, never Queued.
		contradicted := req.Isolated && !isolated
		if intent != flow.CloseNone || req.Now >= task.Deadline {
			if intent == flow.CloseNone {
				intent = flow.CloseDeadline
			}
			status, intent, cancel, err = flow.CloseState(task, intent, req.Joined, false)
			if err != nil {
				return err
			}
		} else if status == flow.Queued {
			if contradicted {
				// 锁后仍能看到有效 run 租约：子树未隔离，不得排队推进。
				status = flow.Blocked
			} else {
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
		if isolated {
			// 收敛整棵子树（US-24 §3.1.7 / FR-24-38）：只有旧执行已隔离时，才把子树里
			// prepared / running 的 attempt 标为 interrupted；范围按 root_task_id 覆盖
			// 全部子任务，而不是只动根行自身。
			// Convergence covers the whole root subtree, not only the recovered row.
			if _, err := tx.ExecContext(ctx, `UPDATE flow_node_attempts a SET state='interrupted',finished_at=$2 FROM flow_tasks t WHERE t.task_id=a.task_id AND t.root_task_id=$1 AND a.state IN ('prepared','running')`, task.RootTaskID, req.Now); err != nil {
				return err
			}
		}
		if intent != flow.CloseNone {
			if err := propagateFlowClose(ctx, tx, task, intent, req.Joined, isolated, false, r.OperationID, hash, req.Reason, r.Principal, req.Now); err != nil {
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

func propagateFlowClose(ctx context.Context, tx *sql.Tx, parent flow.Task, intent flow.CloseIntent, joined, isolated, rescue bool, op, hash, reason string, p core.Principal, now int64) error {
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
		if isolated {
			if _, err := tx.ExecContext(ctx, `UPDATE flow_node_attempts SET state='interrupted',finished_at=$2 WHERE task_id=$1 AND state IN ('prepared','running')`, id, now); err != nil {
				return err
			}
		}
	}
	return nil
}

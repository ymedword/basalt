package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func (s *FlowStore) CompletedAttempt(ctx context.Context, p core.Principal, task flow.Task, nodeID, inputHash string) (flow.NodeAttempt, error) {
	if _, err := s.Read(ctx, p, task.ID); err != nil {
		return flow.NodeAttempt{}, err
	}
	a := flow.NodeAttempt{TaskID: task.ID}
	var input string
	err := s.db.pool.QueryRowContext(ctx, `SELECT n.node_attempt_id,n.node_id,n.node_invocation_id,n.attempt,n.task_revision,n.execution_epoch,n.run_id,COALESCE(n.session_id,''),n.state,n.input,n.input_hash,n.started_at FROM flow_node_attempts n JOIN runs r ON r.run_id=n.run_id AND r.flow_task_id=n.task_id AND r.flow_node_attempt_id=n.node_attempt_id WHERE n.task_id=$1 AND n.node_id=$2 AND n.task_revision=$3 AND n.input_hash=$4 AND n.state IN ('running','interrupted','unknown') AND r.status='completed' AND r.caller_id=$5 ORDER BY n.attempt DESC LIMIT 1`, task.ID, nodeID, task.Version.Revision, inputHash, p.CallerID).Scan(&a.ID, &a.NodeID, &a.NodeInvocationID, &a.Attempt, &a.TaskRevision, &a.ExecutionEpoch, &a.RunID, &a.SessionID, &a.State, &input, &a.InputHash, &a.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, flow.ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.Input, err = flow.CanonicalJSON(ctx, []byte(input))
	if err != nil {
		return a, err
	}
	if flowDataHash(a.Input) != a.InputHash {
		return a, flow.ErrStateConflict
	}
	return a, nil
}

func (s *FlowStore) ReclaimCompleted(ctx context.Context, p core.Principal, id, attemptID string, expected flow.TaskVersion) (flow.Task, error) {
	var result flow.Task
	op := "reuse:" + attemptID + ":" + strconv.FormatInt(expected.StateVersion, 10)
	hash := flowOperationHash("reuse", []string{id, attemptID, expected.ETag(id)})
	err := s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return findFlowOperation(ctx, q, id, op, "reuse", hash)
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, id, p)
		if err != nil {
			return err
		}
		if err := checkFlowOwnedVersion(task, p, expected); err != nil {
			return err
		}
		if task.Status != flow.Queued && task.Status != flow.Running || task.CloseIntent != flow.CloseNone || task.CancelRequested {
			return flow.ErrStateConflict
		}
		if err := ensureFlowAncestorsOpen(ctx, tx, task, s.db.clock().UnixMilli()); err != nil {
			return err
		}
		if err := checkFlowAuthorizingKey(ctx, tx, task, p, s.db.clock().UnixMilli()); err != nil {
			return err
		}
		unknown, err := unknownFlowActions(ctx, tx, task.RootTaskID)
		if err != nil {
			return err
		}
		if unknown {
			return flow.ErrActionUnknown
		}
		var valid bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_node_attempts n JOIN runs r ON r.run_id=n.run_id AND r.flow_task_id=n.task_id AND r.flow_node_attempt_id=n.node_attempt_id WHERE n.task_id=$1 AND n.node_attempt_id=$2 AND n.node_id=$3 AND n.task_revision=$4 AND n.state IN ('running','interrupted','unknown') AND r.status='completed' AND r.caller_id=$5)`, id, attemptID, task.Revision.NodeID, task.Version.Revision, p.CallerID).Scan(&valid)
		if err != nil {
			return err
		}
		if !valid {
			return flow.ErrStateConflict
		}
		if err := updateFlowHead(ctx, tx, task, flow.Running, task.Version.Revision, task.Version.ExecutionEpoch, task.CloseIntent, task.CancelRequested, s.db.clock().UnixMilli(), "reuse completed original Run"); err != nil {
			return err
		}
		if err := appendFlowTransition(ctx, tx, task, op, "reuse", hash, flow.Running, task.Version.Revision, s.db.clock().UnixMilli(), "reuse completed original Run", p); err != nil {
			return err
		}
		result, err = readFlowTask(ctx, tx, id)
		return err
	})
	return result, err
}

func validateFlowRecoveredOutput(ctx context.Context, q Queryer, task flow.Task, req flow.CommitNode) error {
	unknown, err := unknownFlowActions(ctx, q, task.RootTaskID)
	if err != nil {
		return err
	}
	if unknown {
		return flow.ErrActionUnknown
	}
	var node flow.Node
	for _, n := range task.AcceptedVersion.Definition.Nodes {
		if n.ID == task.Revision.NodeID {
			node = n
			break
		}
	}
	if node.Agent == nil {
		return flow.ErrStateConflict
	}
	var runID string
	err = q.QueryRowContext(ctx, `SELECT r.run_id FROM runs r JOIN flow_node_attempts n ON n.run_id=r.run_id AND n.node_attempt_id=r.flow_node_attempt_id AND n.task_id=r.flow_task_id JOIN run_resource_manifests m ON m.run_id=r.run_id WHERE n.task_id=$1 AND n.node_attempt_id=$2 AND r.caller_id=$3 AND r.status='completed' AND r.config_hash=$4 AND m.manifest_hash=$5`, task.ID, req.AttemptID, task.CallerID, node.Agent.ConfigHash, node.Agent.ManifestHash).Scan(&runID)
	if err != nil {
		return err
	}
	var raw string
	if node.Kind == "agent" {
		raw, err = finalText(ctx, q, runID)
	} else if node.Kind == "tool" {
		err = q.QueryRowContext(ctx, `SELECT a.output FROM flow_action_attempts a JOIN tool_invocations i ON i.id=a.tool_invocation_id AND i.run_id=a.run_id WHERE a.task_id=$1 AND a.node_attempt_id=$2 AND a.outcome='succeeded' AND i.status='success' AND a.output=i.result_json ORDER BY i.id DESC LIMIT 1`, task.ID, req.AttemptID).Scan(&raw)
	} else {
		return flow.ErrStateConflict
	}
	if err != nil {
		return err
	}
	canonical, err := flow.CanonicalJSON(ctx, []byte(raw))
	if err != nil {
		if req.AttemptState != "failed" || req.Outcome == flow.Succeeded {
			return flow.ErrStateConflict
		}
		canonical = []byte(`{}`)
	}
	if flowDataHash(canonical) != req.OutputHash {
		return flow.ErrStateConflict
	}
	return nil
}

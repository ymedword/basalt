package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/ymedword/basalt/internal/observability"
)

// RunsByFlowTaskID 只扩展原账本中已准入的节点，不把计划 RunID 当执行事实。
// RunsByFlowTaskID expands only original admitted runs, never planned identities.
func (s *ObservabilityStore) RunsByFlowTaskID(ctx context.Context, id string) ([]string, error) {
	return s.runIDs(ctx, `SELECT r.run_id FROM runs r JOIN flow_node_attempts a ON a.node_attempt_id=r.flow_node_attempt_id AND a.task_id=r.flow_task_id AND a.run_id=r.run_id JOIN flow_tasks t ON t.task_id=a.task_id AND t.caller_id=r.caller_id WHERE t.task_id=$1 ORDER BY r.started_at,r.run_id LIMIT 201`, id)
}

// FlowAssociation 从原关联生成安全元数据；无 Flow 的普通运行返回空关联。
// FlowAssociation projects existing identities without any execution dependency.
func (s *ObservabilityStore) FlowAssociation(ctx context.Context, runID string) (observability.FlowAssociation, error) {
	var out observability.FlowAssociation
	err := s.db.pool.QueryRowContext(ctx, `SELECT t.task_id,t.root_task_id,a.node_id,a.node_attempt_id,a.task_revision,a.execution_epoch FROM runs r JOIN flow_node_attempts a ON a.node_attempt_id=r.flow_node_attempt_id AND a.task_id=r.flow_task_id AND a.run_id=r.run_id JOIN flow_tasks t ON t.task_id=a.task_id AND t.caller_id=r.caller_id WHERE r.run_id=$1`, runID).Scan(&out.TaskID, &out.RootTaskID, &out.NodeID, &out.NodeAttemptID, &out.Revision, &out.Epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return observability.FlowAssociation{}, nil
	}
	if err != nil {
		return out, err
	}
	rows, err := s.db.pool.QueryContext(ctx, `SELECT a.tool_invocation_id,a.action_id FROM flow_action_attempts a JOIN tool_invocations i ON i.id=a.tool_invocation_id AND i.run_id=a.run_id WHERE a.task_id=$1 AND a.node_attempt_id=$2 AND a.run_id=$3 AND a.tool_invocation_id IS NOT NULL ORDER BY a.tool_invocation_id LIMIT 201`, out.TaskID, out.NodeAttemptID, runID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Actions = map[string]string{}
	for rows.Next() {
		var audit int64
		var action string
		if err := rows.Scan(&audit, &action); err != nil {
			return out, err
		}
		out.Actions["tool_invocations:"+strconv.FormatInt(audit, 10)] = action
	}
	return out, rows.Err()
}

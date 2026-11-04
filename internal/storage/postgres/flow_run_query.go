package postgres

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// AdmittedAttemptRuns returns only attempts backed by the original, owner-bound
// Run ledger. A pre-generated run_id is not itself evidence of an admitted Run.
func (s *FlowStore) AdmittedAttemptRuns(ctx context.Context, p core.Principal, taskID string, ids []string, admin bool) (map[string]bool, error) {
	result := make(map[string]bool)
	if !p.Valid() || admin && p.Kind != core.PrincipalLocalAdmin || !admin && !p.HasScope(core.ScopeFlowsRead) {
		return nil, core.ErrUnauthorized
	}
	if len(ids) > 100 {
		return nil, flow.ErrStateConflict
	}
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.db.pool.QueryContext(ctx, `SELECT a.node_attempt_id FROM flow_node_attempts a JOIN flow_tasks t ON t.task_id=a.task_id JOIN runs r ON r.run_id=a.run_id AND r.flow_task_id=a.task_id AND r.flow_node_attempt_id=a.node_attempt_id AND r.caller_id=t.caller_id WHERE a.task_id=$1 AND a.node_attempt_id=ANY($2) AND ($3 OR t.caller_id=$4)`, taskID, ids, admin, p.CallerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result[id] = true
	}
	return result, rows.Err()
}

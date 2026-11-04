package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func (s *FlowStore) ActiveAttempt(ctx context.Context, p core.Principal, id string) (flow.NodeAttempt, error) {
	if _, err := s.Read(ctx, p, id); err != nil {
		return flow.NodeAttempt{}, err
	}
	a := flow.NodeAttempt{TaskID: id}
	var input string
	err := s.db.pool.QueryRowContext(ctx, `SELECT node_attempt_id,node_id,node_invocation_id,attempt,task_revision,execution_epoch,COALESCE(run_id,''),COALESCE(session_id,''),state,input,input_hash,started_at FROM flow_node_attempts WHERE task_id=$1 AND state IN ('running','prepared') ORDER BY node_attempt_id LIMIT 1`, id).Scan(&a.ID, &a.NodeID, &a.NodeInvocationID, &a.Attempt, &a.TaskRevision, &a.ExecutionEpoch, &a.RunID, &a.SessionID, &a.State, &input, &a.InputHash, &a.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, flow.ErrNotFound
	}
	if err == nil {
		a.Input, err = flow.CanonicalJSON(ctx, []byte(input))
		if err == nil && flowDataHash(a.Input) != a.InputHash {
			err = flow.ErrCheckpointUnsupported
		}
	}
	return a, err
}

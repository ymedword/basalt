package postgres

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

var _ flow.TaskQuery = (*FlowStore)(nil)

func flowCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) > 256 || len(raw) == 0 || strings.ContainsAny(string(raw), "\x00\r\n") {
		return "", flow.ErrStateConflict
	}
	return string(raw), nil
}

// ListTasks 在分页前按归属和工作流编号筛选，并按最近更新时间倒序读取。
// ListTasks filters ownership and workflow before paging by latest update.
func (s *FlowStore) ListTasks(ctx context.Context, p core.Principal, cursor string, limit int, administrative bool, definitionName string) ([]flow.Task, string, error) {
	if !p.Valid() || administrative && p.Kind != core.PrincipalLocalAdmin || !administrative && !p.HasScope(core.ScopeFlowsRead) {
		return nil, "", core.ErrUnauthorized
	}
	limit, err := flowPageLimit(limit)
	if err != nil {
		return nil, "", err
	}
	afterTime, afterID, err := flowTimeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.pool.QueryContext(ctx, `SELECT t.task_id,t.updated_at FROM flow_tasks t JOIN flow_definitions d ON d.definition_uid=t.definition_uid AND d.version=t.definition_version WHERE ($1 OR t.caller_id=$2) AND ($3='' OR d.name=$3) AND ($4='' OR (t.updated_at,t.task_id)<($5,$4)) ORDER BY t.updated_at DESC,t.task_id DESC LIMIT $6`, administrative, p.CallerID, definitionName, afterID, afterTime, limit+1)
	if err != nil {
		return nil, "", err
	}
	var ids []string
	var timestamps []int64
	for rows.Next() {
		var id string
		var updatedAt int64
		if err := rows.Scan(&id, &updatedAt); err != nil {
			return nil, "", errors.Join(err, rows.Close())
		}
		ids = append(ids, id)
		timestamps = append(timestamps, updatedAt)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, "", err
	}
	more := ""
	if len(ids) > limit {
		ids = ids[:limit]
		more = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(timestamps[limit-1], 10) + "\n" + ids[limit-1]))
	}
	items := make([]flow.Task, 0, len(ids))
	for _, id := range ids {
		var item flow.Task
		if administrative {
			item, err = s.ReadForWorker(ctx, id)
		} else {
			item, err = s.Read(ctx, p, id)
		}
		if err != nil {
			return nil, "", err
		}
		items = append(items, item)
	}
	return items, more, nil
}

func (s *FlowStore) ListAttempts(ctx context.Context, p core.Principal, id, cursor string, limit int) ([]flow.NodeAttempt, string, error) {
	if _, err := s.Read(ctx, p, id); err != nil {
		return nil, "", err
	}
	return s.listFlowAttempts(ctx, id, cursor, limit)
}

// ListAdministrativeAttempts is an explicit metadata-only rescue read. It never
// substitutes an administrative Principal for the task's original execution key.
func (s *FlowStore) ListAdministrativeAttempts(ctx context.Context, p core.Principal, id, cursor string, limit int) ([]flow.NodeAttempt, string, error) {
	if !p.Valid() || p.Kind != core.PrincipalLocalAdmin {
		return nil, "", core.ErrUnauthorized
	}
	if _, err := s.ReadForWorker(ctx, id); err != nil {
		return nil, "", err
	}
	items, next, err := s.listFlowAttempts(ctx, id, cursor, limit)
	for i := range items {
		items[i].Input = nil
		items[i].Output = nil
	}
	return items, next, err
}

func (s *FlowStore) listFlowAttempts(ctx context.Context, id, cursor string, limit int) ([]flow.NodeAttempt, string, error) {
	limit, err := flowPageLimit(limit)
	if err != nil {
		return nil, "", err
	}
	afterTime, afterID, err := flowTimeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.pool.QueryContext(ctx, `SELECT node_attempt_id,node_id,node_invocation_id,attempt,task_revision,execution_epoch,COALESCE(run_id,''),COALESCE(session_id,''),state,input,input_hash,COALESCE(output,''),COALESCE(output_hash,''),started_at,COALESCE(finished_at,0) FROM flow_node_attempts WHERE task_id=$1 AND (started_at>$2 OR (started_at=$2 AND node_attempt_id>$3)) ORDER BY started_at,node_attempt_id LIMIT $4`, id, afterTime, afterID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]flow.NodeAttempt, 0, limit)
	for rows.Next() {
		var a flow.NodeAttempt
		var input, output string
		a.TaskID = id
		if err := rows.Scan(&a.ID, &a.NodeID, &a.NodeInvocationID, &a.Attempt, &a.TaskRevision, &a.ExecutionEpoch, &a.RunID, &a.SessionID, &a.State, &input, &a.InputHash, &output, &a.OutputHash, &a.StartedAt, &a.FinishedAt); err != nil {
			return nil, "", err
		}
		if len(items) == limit {
			last := items[limit-1]
			return items, base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(last.StartedAt, 10) + "\n" + last.ID)), nil
		}
		a.Input = []byte(input)
		a.Output = []byte(output)
		items = append(items, a)
	}
	return items, "", rows.Err()
}

func flowTimeCursor(cursor string) (int64, string, error) {
	if cursor == "" {
		return 0, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) > 256 {
		return 0, "", flow.ErrStateConflict
	}
	parts := strings.Split(string(raw), "\n")
	if len(parts) != 2 || parts[1] == "" || strings.ContainsAny(parts[1], "\r\x00") {
		return 0, "", flow.ErrStateConflict
	}
	at, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || at < 0 {
		return 0, "", flow.ErrStateConflict
	}
	if base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at, 10)+"\n"+parts[1])) != cursor {
		return 0, "", flow.ErrStateConflict
	}
	return at, parts[1], nil
}

// Remaining reads all ancestor usage; it does not reserve or free an allocation.
func (s *FlowStore) Remaining(ctx context.Context, p core.Principal, id string) (core.UsageEstimate, error) {
	task, err := s.Read(ctx, p, id)
	if err != nil {
		return core.UsageEstimate{}, err
	}
	left := core.UsageEstimate{Tokens: task.Contract.Budget.MaxTotalTokens, ToolCalls: task.Contract.Budget.MaxToolCalls}
	for level := 0; level <= task.Depth; level++ {
		var tokens, tools int64
		err := s.db.pool.QueryRowContext(ctx, `WITH RECURSIVE subtree AS (SELECT task_id,0 AS depth FROM flow_tasks WHERE task_id=$1 UNION ALL SELECT t.task_id,s.depth+1 FROM flow_tasks t JOIN subtree s ON t.parent_task_id=s.task_id WHERE s.depth<3) SELECT COALESCE(sum(CASE WHEN a.state='settled' THEN a.actual_tokens ELSE a.reserved_tokens END),0),COALESCE(sum(CASE WHEN a.state='settled' THEN a.actual_tool_calls ELSE a.reserved_tool_calls END),0) FROM flow_budget_allocations a JOIN subtree s ON s.task_id=a.task_id`, task.ID).Scan(&tokens, &tools)
		if err != nil {
			return core.UsageEstimate{}, err
		}
		if tokens > task.Contract.Budget.MaxTotalTokens || tools > task.Contract.Budget.MaxToolCalls {
			return core.UsageEstimate{}, flow.ErrBudget
		}
		left.Tokens = min(left.Tokens, task.Contract.Budget.MaxTotalTokens-tokens)
		left.ToolCalls = min(left.ToolCalls, task.Contract.Budget.MaxToolCalls-tools)
		if task.ParentTaskID == "" {
			break
		}
		task, err = s.Read(ctx, p, task.ParentTaskID)
		if err != nil {
			return core.UsageEstimate{}, err
		}
	}
	return left, nil
}

// OutputEvidence verifies completed-checkpoint provenance before returning source
// locators. app still has to recheck each source's current access/version.
func (s *FlowStore) OutputEvidence(ctx context.Context, p core.Principal, id string, out flow.NodeOutput) ([]flow.EvidenceRef, error) {
	task, err := s.Read(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if !out.Verified || out.NodeAttemptID == "" || out.Revision > task.Version.Revision {
		return nil, flow.ErrStateConflict
	}
	var storedHash, state, nodeID string
	var revision int64
	var runID sql.NullString
	err = s.db.pool.QueryRowContext(ctx, `SELECT state,output_hash,task_revision,run_id,node_id FROM flow_node_attempts WHERE task_id=$1 AND node_attempt_id=$2`, id, out.NodeAttemptID).Scan(&state, &storedHash, &revision, &runID, &nodeID)
	if err != nil {
		return nil, err
	}
	canonical, err := flow.CanonicalJSON(ctx, out.Value)
	verificationPending := state == "interrupted" && task.Status == flow.Blocked && task.Revision.NodeID == nodeID && runID.Valid
	if err != nil || state != "succeeded" && !verificationPending || storedHash != out.Hash || out.Hash != flowDataHash(canonical) || out.Revision != revision {
		return nil, flow.ErrStateConflict
	}
	if runID.Valid {
		var status string
		if err := s.db.pool.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id=$1 AND caller_id=$2 AND flow_task_id=$3 AND flow_node_attempt_id=$4`, runID.String, p.CallerID, id, out.NodeAttemptID).Scan(&status); err != nil {
			return nil, err
		}
		if status != string(core.StatusCompleted) {
			return nil, flow.ErrStateConflict
		}
	}
	graph, err := s.ReadGraph(ctx, p, id)
	if err != nil {
		return nil, err
	}
	var refs []flow.EvidenceRef
	for _, ref := range graph.Evidence {
		if ref.NodeAttemptID == out.NodeAttemptID {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

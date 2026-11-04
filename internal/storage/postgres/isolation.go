package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

var (
	_ core.SandboxExecutionStore  = (*DB)(nil)
	_ core.SandboxExecutionReader = (*DB)(nil)
)

// BeginSandboxExecution 在创建容器前预写生命周期；唯一键禁止重复执行。
// BeginSandboxExecution records intent before creation and rejects duplicate calls.
func (db *DB) BeginSandboxExecution(ctx context.Context, rec core.SandboxExecution) error {
	if rec.ToolInvocationID <= 0 || rec.RunID == "" || rec.InstanceID == "" || rec.ContainerName == "" {
		return errors.New("sandbox ledger: missing execution identity")
	}
	_, err := db.pool.ExecContext(ctx, `INSERT INTO sandbox_executions
	(tool_invocation_id,run_id,agent_name,instance_id,profile,runtime,image_digest,container_name,state,cleanup_state,started_at,created_at,updated_at)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,'creating','pending',$9,$9,$9)`,
		rec.ToolInvocationID, rec.RunID, rec.AgentName, rec.InstanceID, rec.Profile, rec.Runtime, rec.ImageDigest, rec.ContainerName, rec.StartedAt.UnixMilli())
	return err
}

// MarkSandboxRunning 在预写成功后记录即将执行；失败不抹掉创建意图。
// MarkSandboxRunning advances a previously written execution intent.
func (db *DB) MarkSandboxRunning(ctx context.Context, id int64, at time.Time) error {
	r, err := db.pool.ExecContext(ctx, `UPDATE sandbox_executions SET state='running',updated_at=$2 WHERE tool_invocation_id=$1 AND state='creating'`, id, at.UnixMilli())
	return sandboxAffected(r, err)
}

// FinishSandboxExecution 记录终态与真实清理结果，不允许再次写 pending。
// FinishSandboxExecution persists terminal facts without producing another pending state.
func (db *DB) FinishSandboxExecution(ctx context.Context, res core.SandboxExecutionResult) error {
	if !sandboxTerminal(res.State) || (res.CleanupState != core.CleanupCleaned && res.CleanupState != core.CleanupFailed) {
		return errors.New("sandbox ledger: invalid terminal state")
	}
	r, err := db.pool.ExecContext(ctx, `UPDATE sandbox_executions SET container_id=NULLIF($2,''),state=$3,exit_code=$4,
	error_reason=NULLIF($5,''),artifact_count=$6,artifact_bytes=$7,cleanup_state=$8,cleanup_error=NULLIF($9,''),finished_at=$10,updated_at=$10 WHERE tool_invocation_id=$1`,
		res.ToolInvocationID, res.ContainerID, res.State, res.ExitCode, res.ErrorReason, res.ArtifactCount, res.ArtifactBytes, res.CleanupState, res.CleanupError, res.FinishedAt.UnixMilli())
	return sandboxAffected(r, err)
}

// CountActiveSandboxExecutions 只统计仍占资源的行；unknown+cleaned 不占容量。
// CountActiveSandboxExecutions counts resource occupancy rather than unknown outcomes.
func (db *DB) CountActiveSandboxExecutions(ctx context.Context, instanceID string) (int, error) {
	var n int
	err := db.pool.QueryRowContext(ctx, `SELECT count(*) FROM sandbox_executions WHERE instance_id=$1 AND (state IN('creating','running') OR cleanup_state IN('pending','failed'))`, instanceID).Scan(&n)
	return n, err
}

// MarkSandboxCleaned 在启动对账中收敛资源；非终态执行保留结果未知。
// MarkSandboxCleaned releases occupancy while preserving unknown execution outcomes.
func (db *DB) MarkSandboxCleaned(ctx context.Context, id int64, state core.SandboxState, cleanup core.CleanupState, reason string, at time.Time) error {
	if !sandboxTerminal(state) {
		state = core.SandboxUnknown
	}
	if cleanup != core.CleanupCleaned && cleanup != core.CleanupFailed {
		return errors.New("sandbox ledger: invalid cleanup state")
	}
	r, err := db.pool.ExecContext(ctx, `UPDATE sandbox_executions SET
	state=CASE WHEN state IN('creating','running') THEN $2 ELSE state END,
	error_reason=CASE WHEN state IN('creating','running') THEN 'orphan_cleaned' ELSE error_reason END,
	cleanup_state=$3,cleanup_error=CASE WHEN $3='failed' THEN NULLIF($4,'') ELSE NULL END,
	finished_at=COALESCE(finished_at,$5),updated_at=$5 WHERE tool_invocation_id=$1`, id, state, cleanup, reason, at.UnixMilli())
	return sandboxAffected(r, err)
}

// PendingSandboxExecutions 返回当前实例全部未释放记录，包含已终态但清理失败的行。
// PendingSandboxExecutions includes terminal rows whose cleanup still needs recovery.
func (db *DB) PendingSandboxExecutions(ctx context.Context, instanceID string) ([]core.SandboxExecution, error) {
	rows, err := db.pool.QueryContext(ctx, `SELECT tool_invocation_id,run_id,agent_name,instance_id,profile,runtime,image_digest,container_name,state,cleanup_state,started_at FROM sandbox_executions WHERE instance_id=$1 AND (state IN('creating','running') OR cleanup_state IN('pending','failed')) ORDER BY tool_invocation_id`, instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.SandboxExecution, 0)
	for rows.Next() {
		var rec core.SandboxExecution
		var at int64
		if err := rows.Scan(&rec.ToolInvocationID, &rec.RunID, &rec.AgentName, &rec.InstanceID, &rec.Profile, &rec.Runtime, &rec.ImageDigest, &rec.ContainerName, &rec.State, &rec.CleanupState, &at); err != nil {
			return nil, err
		}
		rec.StartedAt = time.UnixMilli(at).UTC()
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SandboxExecutionsByRun 联结原审计行，返回稳定排序的只读执行详情。
// SandboxExecutionsByRun joins the existing audit rows for read-only projection.
func (db *DB) SandboxExecutionsByRun(ctx context.Context, runID string) ([]core.SandboxExecutionView, error) {
	rows, err := db.pool.QueryContext(ctx, `SELECT e.tool_invocation_id,COALESCE(t.tool_call_id,''),t.tool_name,e.profile,e.runtime,e.state,COALESCE(e.exit_code,0),e.cleanup_state,COALESCE(e.error_reason,''),e.artifact_count,e.artifact_bytes,e.started_at,e.finished_at FROM sandbox_executions e JOIN tool_invocations t ON t.id=e.tool_invocation_id WHERE e.run_id=$1 ORDER BY e.tool_invocation_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]core.SandboxExecutionView, 0)
	for rows.Next() {
		var v core.SandboxExecutionView
		var start int64
		var end sql.NullInt64
		if err := rows.Scan(&v.ToolInvocationID, &v.ToolCallID, &v.ToolName, &v.Profile, &v.Runtime, &v.State, &v.ExitCode, &v.CleanupState, &v.ErrorReason, &v.ArtifactCount, &v.ArtifactBytes, &start, &end); err != nil {
			return nil, err
		}
		v.StartedAt = time.UnixMilli(start).UTC()
		if end.Valid {
			v.FinishedAt = time.UnixMilli(end.Int64).UTC()
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func sandboxAffected(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func sandboxTerminal(state core.SandboxState) bool {
	switch state {
	case core.SandboxFinished, core.SandboxCanceled, core.SandboxTimeout, core.SandboxFailed, core.SandboxUnknown:
		return true
	default:
		return false
	}
}

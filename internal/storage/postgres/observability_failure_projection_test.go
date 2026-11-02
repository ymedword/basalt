//go:build integration

package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityRunReasonBudgetClassificationIsExact(t *testing.T) {
	valid := "上下文超限：不可裁部分估算 66171 tokens 超过 budget.context_tokens=65536（含余量 1024）"
	require.Equal(t, "context_budget_exceeded", obsRunReason(valid))
	budget := obsContextBudget(valid)
	require.NotNil(t, budget)
	require.EqualValues(t, 66171, budget.Required)
	require.EqualValues(t, 65536, budget.Limit)
	withReserve := "上下文超限：不可裁部分估算 7518 tokens 超过 budget.context_tokens=8192（含余量 1024）"
	require.Equal(t, "context_budget_exceeded", obsRunReason(withReserve))
	require.EqualValues(t, 8542, obsContextBudget(withReserve).Required+obsContextBudget(withReserve).Reserve)
	require.Nil(t, obsContextBudget("上下文超限：不可裁部分估算 7168 tokens 超过 budget.context_tokens=8192（含余量 1024）"), "an exact fit is not an overflow")
	for _, raw := range []string{valid + " CANARY_SECRET", "prefix " + valid, "上下文超限：CANARY_SECRET", "上下文超限：不可裁部分估算 1 tokens 超过 budget.context_tokens=65536（含余量 1024）", "上下文超限：不可裁部分估算 999999999999999 tokens 超过 budget.context_tokens=1（含余量 0）"} {
		require.Nil(t, obsContextBudget(raw))
		require.Equal(t, "unclassified", obsRunReason(raw), "arbitrary error text must never become a safe numeric explanation")
	}
}

func TestObservabilityTaskReasonProjectionDoesNotChangeExportSource(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	ctx := t.Context()
	const runID = "obs-safe-task-reason"
	taskPortSeedTask(t, db, "obs-safe-task", "synthetic", "0 * * * * *", "synthetic")
	require.NoError(t, NewRunStore(db).StartRun(ctx, core.RunRecord{RunID: runID, Agent: "synthetic", Channel: "web", Principal: core.LocalAdminCLIPrincipal(), StartedAt: time.UnixMilli(1000)}, core.RunResourceManifest{}))
	const reason = "上下文超限：不可裁部分估算 66171 tokens 超过 budget.context_tokens=65536（含余量 1024）"
	_, err := db.pool.ExecContext(ctx, `UPDATE runs SET status='failed',reason=$1,finished_at=2000 WHERE run_id=$2`, reason, runID)
	require.NoError(t, err)
	var id int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `INSERT INTO task_executions(task_id,run_id,status,trigger,started_at,finished_at,duration_ms,error_message) VALUES('obs-safe-task',$1,'failed','cron',1000,2000,1000,'CANARY_TASK_RAW_ERROR') RETURNING id`, runID).Scan(&id))
	rows, err := s.TaskExecutionByRun(ctx, runID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "context_budget_exceeded", rows[0].ErrorKind)
	require.True(t, rows[0].ReasonFromRun)
	item, ok, err := s.loadExportFact(ctx, observability.SourceTask, fmt.Sprint(id))
	require.NoError(t, err)
	require.True(t, ok)
	fact := item.Fact.(observability.TaskExecutionFact)
	require.Empty(t, fact.ErrorKind, "export must not acquire a dependency on a related Run reason")
	require.False(t, fact.ReasonFromRun)
}

func TestObservabilityToolDurationPreservesZeroAndUnknown(t *testing.T) {
	db := portTestDB(t)
	s := NewObservabilityStore(db)
	runID := "obs-duration-projection"
	inv := NewToolInvocationRepo(db)
	for _, duration := range []time.Duration{0, 2550 * time.Millisecond} {
		id, err := inv.BeginInvocation(context.Background(), core.ToolInvocation{RunID: runID, ToolName: "fetch", ToolCallID: duration.String()})
		require.NoError(t, err)
		require.NoError(t, inv.FinishInvocation(context.Background(), id, core.ToolInvocationResult{Status: core.LLMCallFailed, ErrorKind: core.ErrorKind("external_error"), Duration: duration}))
	}
	rows, err := s.ToolInvocationsByRun(t.Context(), runID)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.NotNil(t, rows[0].DurationMS)
	require.EqualValues(t, 0, *rows[0].DurationMS)
	require.EqualValues(t, 2550, *rows[1].DurationMS)
	_, err = db.pool.ExecContext(t.Context(), `UPDATE tool_invocations SET status='unknown',duration_ms=0,finished_at=NULL WHERE run_id=$1`, runID)
	require.NoError(t, err)
	rows, err = s.ToolInvocationsByRun(t.Context(), runID)
	require.NoError(t, err)
	for _, row := range rows {
		require.Nil(t, row.DurationMS)
	}
}

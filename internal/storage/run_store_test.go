package storage

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// runRow 是 runs 一行的判卷形态（列全部显式——可空列用 Null 形态，判 NULL 与判空串不同）。
// runRow is the assertion shape of one runs row; nullable columns stay nullable.
type runRow struct {
	RunID      string
	SessionID  sql.NullString
	Agent      string
	Channel    string
	UserID     string
	Trigger    string
	Status     string
	Reason     sql.NullString
	Iterations int
	ToolCalls  int
	ConfigHash sql.NullString
	StartedAt  int64
	FinishedAt sql.NullInt64
}

// readRunRow 按 run_id 读回账本行（in-package 判卷走 db.read——生产写入路径仍唯一经仓储）。
// readRunRow reads one ledger row back by run_id through the read pool.
func readRunRow(t *testing.T, db *DB, runID string) runRow {
	t.Helper()
	var row runRow
	err := db.read.QueryRowContext(context.Background(),
		`SELECT run_id, session_id, agent, channel, user_id, trigger, status,
		        reason, iterations, tool_calls, config_hash, started_at, finished_at
		   FROM runs WHERE run_id = ?`, runID).
		Scan(&row.RunID, &row.SessionID, &row.Agent, &row.Channel, &row.UserID, &row.Trigger,
			&row.Status, &row.Reason, &row.Iterations, &row.ToolCalls, &row.ConfigHash,
			&row.StartedAt, &row.FinishedAt)
	require.NoError(t, err)
	return row
}

// fixedStart 是账本用例的冻结时刻（时间显式传入——仓储不自取业务时间）。
// fixedStart is the frozen clock of the ledger cases.
var fixedStart = time.Date(2026, 10, 16, 8, 30, 0, 123*int(time.Millisecond), time.UTC)

// startRow 造一份起始行夹具（渠道参数化——trigger 断言要用四个渠道各来一次）。
// startRow builds a starting-row fixture for the given channel.
func startRow(channel string) core.RunRecord {
	return core.RunRecord{
		RunID:      channel + "-1776000000123-abc123",
		SessionID:  "cli-3f9a1c07b2d84e55",
		Agent:      "demo",
		Channel:    channel,
		UserID:     "alice",
		ConfigHash: "3f9a1c07b2d84e55",
		StartedAt:  fixedStart,
	}
}

// TestRunStore_StartRunInsertsRunningRow —— K1：起始行 = `status` 写 `running`（由 SQL 固定，
// 不由调用方给）、`reason` / `finished_at` 为 NULL、计数列取缺省 0、时间戳落毫秒。
// TestRunStore_StartRunInsertsRunningRow pins the starting row.
func TestRunStore_StartRunInsertsRunningRow(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	rec := startRow("cli")
	require.NoError(t, store.StartRun(ctx, rec))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusRunning), row.Status, "起始态由仓储固定写 running")
	require.Equal(t, rec.Agent, row.Agent)
	require.Equal(t, rec.Channel, row.Channel)
	require.Equal(t, rec.UserID, row.UserID)
	require.Equal(t, rec.SessionID, row.SessionID.String)
	require.Equal(t, rec.ConfigHash, row.ConfigHash.String)
	require.False(t, row.Reason.Valid, "起始行没有原因 → NULL")
	require.False(t, row.FinishedAt.Valid, "起始行未结束 → NULL")
	require.Zero(t, row.Iterations)
	require.Zero(t, row.ToolCalls)
	require.Equal(t, fixedStart.UnixMilli(), row.StartedAt)
}

// TestRunStore_FinishRunUpdatesAllTerminalColumns —— K2：终态更新**全部列**；空 reason 写回
// NULL（不是空串）；`FinishRun` 的签名里**没有** finalText 可言（最终文本不进账本）。
// TestRunStore_FinishRunUpdatesAllTerminalColumns pins the terminal columns.
func TestRunStore_FinishRunUpdatesAllTerminalColumns(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	rec := startRow("web")
	require.NoError(t, store.StartRun(ctx, rec))

	finished := fixedStart.Add(90 * time.Second)
	require.NoError(t, store.FinishRun(ctx, rec.RunID, core.StatusCompleted, "", 2, 1, finished))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusCompleted), row.Status)
	require.False(t, row.Reason.Valid, "正常终止没有原因 → NULL（空串与没值是两回事）")
	require.Equal(t, 2, row.Iterations)
	require.Equal(t, 1, row.ToolCalls)
	require.Equal(t, finished.UnixMilli(), row.FinishedAt.Int64)
	require.Equal(t, fixedStart.UnixMilli(), row.StartedAt, "起始时刻不被终态改写")

	// 非正常终止：reason 落文本。
	require.NoError(t, store.FinishRun(ctx, rec.RunID, core.StatusLimitReached, "工具调用次数超限", 3, 50, finished))
	again := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusLimitReached), again.Status)
	require.Equal(t, "工具调用次数超限", again.Reason.String)
	require.Equal(t, 50, again.ToolCalls)
}

// TestRunStore_CloseRunningLeavesTerminalRowsUntouched —— K3：崩溃收尾**只把仍为 `running`
// 的**改 `failed`（`WHERE status = 'running'` 是这条语句的全部意义）；扫描面与收尾面配套。
// TestRunStore_CloseRunningLeavesTerminalRowsUntouched: only rows still running are closed;
// terminal states are never re-judged.
func TestRunStore_CloseRunningLeavesTerminalRowsUntouched(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	// 两种终态各一行 + 一行仍 running + 一行无状态（空 session）仍 running。
	completed := startRow("cli")
	completed.RunID = "cli-1776000000123-comple"
	require.NoError(t, store.StartRun(ctx, completed))
	require.NoError(t, store.FinishRun(ctx, completed.RunID, core.StatusCompleted, "", 1, 0, fixedStart))

	cancelled := startRow("cli")
	cancelled.RunID = "cli-1776000000123-cancel"
	require.NoError(t, store.StartRun(ctx, cancelled))
	require.NoError(t, store.FinishRun(ctx, cancelled.RunID, core.StatusCancelled, "context canceled", 1, 0, fixedStart))

	stale := startRow("scheduler")
	stale.RunID = "scheduler-1776000000123-stale0"
	require.NoError(t, store.StartRun(ctx, stale))

	stateless := startRow("web")
	stateless.RunID = "web-1776000000123-statle"
	stateless.SessionID = ""
	require.NoError(t, store.StartRun(ctx, stateless))

	running, err := store.listRunning(ctx)
	require.NoError(t, err)
	require.Len(t, running, 2, "扫描面只出仍为 running 的行（空会话的无状态运行也算）")

	closedAt := fixedStart.Add(5 * time.Minute)
	const reason = "进程中断（启动时发现未完成的运行）"
	n, err := store.closeRunning(ctx, db.write, reason, closedAt)
	require.NoError(t, err)
	require.Equal(t, 2, n, "恰收尾两行仍 running 的运行")

	// 已结束的两行**原样保留**（状态、原因、起始时刻都不动）。
	done := readRunRow(t, db, completed.RunID)
	require.Equal(t, string(core.StatusCompleted), done.Status, "已结束的状态不得被覆盖")
	require.False(t, done.Reason.Valid)
	cancel := readRunRow(t, db, cancelled.RunID)
	require.Equal(t, string(core.StatusCancelled), cancel.Status)
	require.Equal(t, "context canceled", cancel.Reason.String, "已写下的原因不得被改写")

	// 仍 running 的两行被收尾：failed + 原因 + finished_at。
	for _, id := range []string{stale.RunID, stateless.RunID} {
		row := readRunRow(t, db, id)
		require.Equal(t, string(core.StatusFailed), row.Status, "run_id=%s", id)
		require.Equal(t, reason, row.Reason.String)
		require.Equal(t, closedAt.UnixMilli(), row.FinishedAt.Int64)
		require.Equal(t, fixedStart.UnixMilli(), row.StartedAt, "收尾不改起始时刻")
		require.Zero(t, row.Iterations, "收尾不伪造计数（跑到哪算到哪）")
	}

	// 幂等：连跑第二次零变化（恢复扫描可随时重跑）。
	again, err := store.closeRunning(ctx, db.write, reason, closedAt.Add(time.Minute))
	require.NoError(t, err)
	require.Zero(t, again, "第二次收尾没有可收的行")
	left, err := store.listRunning(ctx)
	require.NoError(t, err)
	require.Empty(t, left)
}

// TestRunStore_RunsTableHasNoFinalTextColumn —— K4（§4.3 判据）：`runs` 表的列清单与契约
// 逐字一致，且**不含** `final_text`——最终回答只存在返回结果与会话的 assistant 消息里。
// TestRunStore_RunsTableHasNoFinalTextColumn asserts the column list verbatim, and that no
// column carries the final answer.
func TestRunStore_RunsTableHasNoFinalTextColumn(t *testing.T) {
	db := openTempDB(t)

	rows, err := db.read.QueryContext(context.Background(), `PRAGMA table_info(runs)`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	var columns []string
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())

	require.Equal(t, []string{
		"run_id", "session_id", "agent", "channel", "user_id", "trigger", "status",
		"reason", "iterations", "tool_calls", "config_hash", "started_at", "finished_at",
	}, columns, "列清单以 docs/contracts/schema.sql 为准（逐字、含顺序）")
	require.NotContains(t, columns, "final_text", "运行表不得有最终文本列")
}

// TestRunStore_TimestampsAreUnixMilli —— K5：两处时间列都存 **Unix 毫秒**
// （迁移纪律 4——不是秒、不是纳秒）。
// TestRunStore_TimestampsAreUnixMilli pins both timestamp columns to Unix milliseconds.
func TestRunStore_TimestampsAreUnixMilli(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	start := time.Date(2026, 10, 16, 8, 30, 0, 987*int(time.Millisecond), time.UTC)
	rec := startRow("admin")
	rec.StartedAt = start
	require.NoError(t, store.StartRun(ctx, rec))

	finish := start.Add(1500 * time.Millisecond)
	require.NoError(t, store.FinishRun(ctx, rec.RunID, core.StatusFailed, "模型调用失败", 1, 0, finish))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, start.UnixMilli(), row.StartedAt)
	require.Equal(t, finish.UnixMilli(), row.FinishedAt.Int64)
	require.EqualValues(t, 1500, row.FinishedAt.Int64-row.StartedAt, "差值也按毫秒记账")
	require.Len(t, strconv.FormatInt(row.StartedAt, 10), 13,
		"Unix 毫秒是 13 位十进制——秒（10 位）与纳秒（19 位）都落在这里之外")
}

// TestRunStore_TriggerDerivesFromChannel —— K6：`trigger` 由渠道映射取值（§6 固定口径 8），
// 未映射的渠道**拒绝建行**（不落一行猜出来的记账）。
// TestRunStore_TriggerDerivesFromChannel pins the channel-to-trigger mapping; an unmapped
// channel writes no row at all.
func TestRunStore_TriggerDerivesFromChannel(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	for channel, want := range map[string]string{
		"cli":       triggerUser,
		"web":       triggerAPI,
		"scheduler": triggerCron,
		"admin":     triggerManual,
	} {
		rec := startRow(channel)
		require.NoError(t, store.StartRun(ctx, rec), "渠道 %s", channel)
		require.Equal(t, want, readRunRow(t, db, rec.RunID).Trigger, "渠道 %s 的 trigger", channel)
	}

	err := store.StartRun(ctx, startRow("email"))
	require.Error(t, err, "未映射的渠道不得建行")
	require.Contains(t, err.Error(), "email", "错误必须点名渠道")

	var count int
	require.NoError(t, db.read.QueryRowContext(ctx, `SELECT count(*) FROM runs`).Scan(&count))
	require.Equal(t, 4, count, "拒绝的渠道零落痕")
}

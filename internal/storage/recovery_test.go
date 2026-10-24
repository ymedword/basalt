package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是崩溃恢复扫描（docs/us/us4-react-loop.md §2.3 流程 G 步骤 ①②③④⑥）的专属判卷面：
// 12 条 M1~M12。夹具与读数都由本文件自持（`newTestDB` / `seedSession` / `seedRun` /
// `seedMessage` / `seedToolInvocation` / `loadMessages` / `loadRunStatus` / `dumpMessages` /
// `testLogger` / `fixedNow`）——判据因此不随别处夹具的演进漂移。
// This file owns the assertion surface of the crash-recovery scan end to end.
//
// 两处刻意的形态。① **包内测试**（`package storage`）：`Recover` 的冻结签名收裸 `*sql.DB`，
// 而句柄的写池未导出——包外根本递不进去。② **夹具直接写库**：恢复要处理的正是"上一个进程
// 死在半路"的既成事实，任何 API 都造不出这种状态，只能按列摆出来。
// In-package because the frozen signature takes the unexported write pool, and fixtures write
// rows directly because no API can produce the state a dead process leaves behind.

// recoverySession 是本文件多数用例的会话标识（无状态 Run 走 seedStatelessRun）。
const recoverySession = "s1"

// fixedNow 是恢复扫描的"当前时刻"（时间一律显式传入）；收尾的 finished_at 与合成消息的
// created_at 都取它，故两者都可逐字断言。
var fixedNow = time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)

// ---- 夹具 ------------------------------------------------------------------

// newTestDB 给每个用例一个独立的临时真库，并把**写池**交给恢复（§4.3 写出代码里的同名夹具；
// 实现委托既有 openTempDB：真库 + 全量迁移 + 清理）。
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return openTempDB(t).write
}

// seedSession 建会话行。`messages.session_id` 有外键而连接级 `foreign_keys(1)` 生效，
// 不先有会话行就插不进消息；重复调用是 no-op（主键冲突忽略）。
func seedSession(t *testing.T, db *sql.DB, sessionID string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO sessions
		   (session_id, profile_name, channel, user_id, created_at, last_active_at)
		 VALUES (?, ?, 'cli', 'u1', ?, ?)`,
		sessionID, "assistant", fixedNow.UnixMilli(), fixedNow.UnixMilli())
	require.NoError(t, err)
}

// seedRunRow 建一行账本。终态行的 `reason` / `finished_at` 由用例显式给（nil → NULL）——
// 只有它们非空，"恢复有没有覆盖已结束的行"才判得出来。
func seedRunRow(t *testing.T, db *sql.DB, runID, status string, session sql.NullString, reason any, finishedAt any) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO runs
		   (run_id, session_id, agent, channel, user_id, trigger, status,
		    reason, iterations, tool_calls, config_hash, started_at, finished_at)
		 VALUES (?, ?, 'assistant', 'cli', 'u1', 'user', ?, ?, 0, 0, NULL, ?, ?)`,
		runID, session, status, reason, fixedNow.Add(-time.Hour).UnixMilli(), finishedAt)
	require.NoError(t, err)
}

// seedRun 建一行挂在 recoverySession 上的账本（起始态与终态都给得出来）。
func seedRun(t *testing.T, db *sql.DB, runID, status string) {
	t.Helper()
	seedRunRow(t, db, runID, status, sql.NullString{String: recoverySession, Valid: true}, nil, nil)
}

// seedStatelessRun 建一行**无状态**账本（`session_id` 为 NULL——invoke 形态）。
func seedStatelessRun(t *testing.T, db *sql.DB, runID, status string) {
	t.Helper()
	seedRunRow(t, db, runID, status, sql.NullString{}, nil, nil)
}

// seedDoneRun 建一行**已结束且带终态证据**的账本（reason 与 finished_at 都非空）。
func seedDoneRun(t *testing.T, db *sql.DB, runID, status string) {
	t.Helper()
	seedRunRow(t, db, runID, status, sql.NullString{String: recoverySession, Valid: true},
		"原有中止原因", fixedNow.Add(-time.Hour).UnixMilli())
}

// seedMessage 追加一条消息；assistant 行可带工具调用（可变参数，其余角色留空）。
// `tool_calls` 列的形态与 schema 注释一致：`[{id,name,arguments}]`。
// The tool role takes its tool_call_id from seedToolMessage instead: a result message answers
// one specific call, and that binding is what the positional grouping keys on.
func seedMessage(t *testing.T, db *sql.DB, sessionID string, seq int64, role, content, runID string, calls ...core.ToolCall) {
	t.Helper()
	seedSession(t, db, sessionID)

	var callsJSON any
	if len(calls) > 0 {
		raw, err := json.Marshal(calls)
		require.NoError(t, err)
		callsJSON = string(raw)
	}
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO messages (session_id, run_id, seq, role, content, tool_calls, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sessionID, nullIfEmpty(runID), seq, role, content, callsJSON, fixedNow.UnixMilli())
	require.NoError(t, err)
}

// seedToolMessage 追加一条 `role=tool` 的结果消息（`tool_call_id` 就是它答的那次调用）。
func seedToolMessage(t *testing.T, db *sql.DB, sessionID string, seq int64, runID, callID, content string) {
	t.Helper()
	seedSession(t, db, sessionID)

	_, err := db.ExecContext(context.Background(),
		`INSERT INTO messages (session_id, run_id, seq, role, content, tool_call_id, created_at)
		 VALUES (?, ?, ?, 'tool', ?, ?, ?)`,
		sessionID, nullIfEmpty(runID), seq, content, callID, fixedNow.UnixMilli())
	require.NoError(t, err)
}

// seedToolInvocation 预置一行工具审计（`resultContent` 非空即写进 `result_json.content`——
// 还原分支读的就是它）。返回行 id 供逐列回读。
func seedToolInvocation(t *testing.T, db *sql.DB, runID, callID, status, resultContent string) int64 {
	t.Helper()
	var result any
	if resultContent != "" {
		raw, err := json.Marshal(map[string]string{"content": resultContent})
		require.NoError(t, err)
		result = string(raw)
	}
	res, err := db.ExecContext(context.Background(),
		`INSERT INTO tool_invocations
		   (run_id, session_id, tool_call_id, tool_name, input_json, result_json,
		    status, error_message, error_kind, duration_ms, started_at, finished_at)
		 VALUES (?, ?, ?, 'read_file', '{}', ?, ?, NULL, NULL, NULL, ?, NULL)`,
		runID, recoverySession, callID, result, status, fixedNow.UnixMilli())
	require.NoError(t, err)

	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// seedLLMCall 预置一行模型调用审计（NOT NULL 列取固定值，判卷只关心 status / error_kind）。
func seedLLMCall(t *testing.T, db *sql.DB, runID, status string) int64 {
	t.Helper()
	res, err := db.ExecContext(context.Background(),
		`INSERT INTO llm_calls
		   (run_id, session_id, round, provider, model, prompt_tokens, completion_tokens,
		    total_tokens, duration_ms, status, error_message, error_kind, config_hash, created_at)
		 VALUES (?, ?, 1, 'fake', 'm', 0, 0, 0, 0, ?, NULL, NULL, NULL, ?)`,
		runID, recoverySession, status, fixedNow.UnixMilli())
	require.NoError(t, err)

	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// ---- 判卷读面 --------------------------------------------------------------

// loadMessages 按 seq 升序读回整个会话（断言"补出来的是哪一条、内容是什么"）。
func loadMessages(t *testing.T, db *sql.DB, sessionID string) []core.Message {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT role, COALESCE(content, ''), COALESCE(tool_call_id, ''), COALESCE(tool_calls, '')
		   FROM messages WHERE session_id = ? ORDER BY seq`, sessionID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []core.Message
	for rows.Next() {
		var (
			message core.Message
			calls   string
		)
		require.NoError(t, rows.Scan(&message.Role, &message.Content, &message.ToolCallID, &calls))
		if calls != "" {
			require.NoError(t, json.Unmarshal([]byte(calls), &message.ToolCalls))
		}
		out = append(out, message)
	}
	require.NoError(t, rows.Err())
	return out
}

// dumpMessages 是整个会话的**规范快照**（全列 + seq），幂等用例前后比对。
func dumpMessages(t *testing.T, db *sql.DB, sessionID string) string {
	t.Helper()
	return dumpRows(t, db,
		`SELECT seq || '|' || role || '|' || COALESCE(content, '') || '|' || COALESCE(tool_calls, '')
		        || '|' || COALESCE(tool_call_id, '') || '|' || COALESCE(run_id, '') || '|' || created_at
		   FROM messages WHERE session_id = ? ORDER BY seq`, sessionID)
}

// dumpRows 跑一条**完整**查询（调用点写全 SQL 字面量，值一律走参数）并按行拼成规范文本。
func dumpRows(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	require.NoError(t, err)

	var builder strings.Builder
	for rows.Next() {
		values := make([]any, len(cols))
		scanned := make([]any, len(cols))
		for i := range values {
			scanned[i] = &values[i]
		}
		require.NoError(t, rows.Scan(scanned...))
		fmt.Fprintf(&builder, "%v\n", values)
	}
	require.NoError(t, rows.Err())
	return builder.String()
}

// loadRunStatus 只判状态列（多数断言用它就够）。
func loadRunStatus(t *testing.T, db *sql.DB, runID string) string {
	t.Helper()
	return loadRunState(t, db, runID).Status
}

// runState 是账本行的判卷形态（可空列用 Null 形态：NULL 与空串不是一回事）。
type runState struct {
	Status     string
	SessionID  sql.NullString
	Reason     sql.NullString
	FinishedAt sql.NullInt64
}

// loadRunState 读回账本行的状态三件 + 会话列（"收尾只动状态三列"也判得出来）。
func loadRunState(t *testing.T, db *sql.DB, runID string) runState {
	t.Helper()
	var state runState
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status, session_id, reason, finished_at FROM runs WHERE run_id = ?`, runID).
		Scan(&state.Status, &state.SessionID, &state.Reason, &state.FinishedAt))
	return state
}

// loadInvocationAudit 读回工具审计行的 (status, error_kind)。
func loadInvocationAudit(t *testing.T, db *sql.DB, id int64) (string, string) {
	t.Helper()
	var status string
	var kind sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status, error_kind FROM tool_invocations WHERE id = ?`, id).Scan(&status, &kind))
	return status, kind.String
}

// loadLLMCallAudit 读回模型调用审计行的 (status, error_kind)。
func loadLLMCallAudit(t *testing.T, db *sql.DB, id int64) (string, string) {
	t.Helper()
	var status string
	var kind sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status, error_kind FROM llm_calls WHERE id = ?`, id).Scan(&status, &kind))
	return status, kind.String
}

// countToolInvocations 数工具审计行（"补齐只进消息、不进审计"的判据）。
func countToolInvocations(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM tool_invocations`).Scan(&count))
	return count
}

// ---- 日志面 ----------------------------------------------------------------

// testLogger 是"只求不响"的日志面（多数用例不判日志）。
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// logCapture 是日志判卷面（M11 读 ⑥ 的 INFO 三键、M12 读 WARN 点名）。
// 无需加锁：恢复是**同步**的（宪法 VII），写与读发生在同一个 goroutine 上。
type logCapture struct{ builder strings.Builder }

func (c *logCapture) Write(p []byte) (int, error) { return c.builder.Write(p) }

func (c *logCapture) text() string { return c.builder.String() }

// captureLogger 返回"写进内存"的日志面。
func captureLogger() (*slog.Logger, *logCapture) {
	capture := &logCapture{}
	return slog.New(slog.NewTextHandler(capture, nil)), capture
}

// ---- M1 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ①）：两张审计表的 `pending` 行都归一化为 `unknown` 且
// `error_kind` 写 `unknown`；**只改 pending**——终态行一字不动；读数 `unknowns` 是两表合计。
func TestRecovery_NormalizesPendingRowsToUnknown(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	pendingCall := seedToolInvocation(t, db, "run-1", "c1", "pending", "")
	doneCall := seedToolInvocation(t, db, "run-1", "c2", "success", "已经执行过的结果")
	pendingLLM := seedLLMCall(t, db, "run-1", "pending")
	failedLLM := seedLLMCall(t, db, "run-1", "failed")

	report, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, report.Unknowns, "两表各一行 pending，合计 2")
	require.Zero(t, report.RepairedGroups, "没有消息就没有半截组")
	require.Zero(t, report.ClosedRuns, "没有 running 的账本行")

	status, kind := loadInvocationAudit(t, db, pendingCall)
	require.Equal(t, "unknown", status, "崩溃窗口的 pending → unknown（工具是否已产生副作用无法确定）")
	require.Equal(t, "unknown", kind, "error_kind 同步写 unknown")

	status, _ = loadInvocationAudit(t, db, doneCall)
	require.Equal(t, "success", status, "终态行不得被覆盖")
	status, kind = loadLLMCallAudit(t, db, pendingLLM)
	require.Equal(t, "unknown", status)
	require.Equal(t, "unknown", kind)
	status, _ = loadLLMCallAudit(t, db, failedLLM)
	require.Equal(t, "failed", status, "终态行不得被覆盖")
}

// ---- M2 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ②③ + §4.4；§4.3 写出代码原样落地）：半截组**按位置**判定
// （同一会话里其它 Run 用同一个 tool_call_id 的"陈旧"审计行不得被误配）；本 Run 没有行
// → "未执行（进程中断）"；已结束的 Run 不得被覆盖；当前 Run 有 `pending` 行 → "结果未知…"；
// 连跑两次零变化。
// Acceptance: positional grouping, per-run classification and idempotence — the §4.3
// regression landed verbatim, fixture mechanics adjusted (see tasks.md T045 log).
func TestRecovery_RepairsHalfGroupByPosition(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seedRun(t, db, "run-1", "running")       // 当前 Run
	seedRun(t, db, "run-alone", "completed") // 已结束的 Run——不得被覆盖
	seedMessage(t, db, recoverySession, 1, core.RoleUser, "读文件", "run-1")
	seedMessage(t, db, recoverySession, 2, core.RoleAssistant, "", "run-1",
		core.ToolCall{ID: "c1", Name: "read_file", Arguments: `{"path":"notes.txt"}`}) // 本轮组：缺 tool 行
	seedToolInvocation(t, db, "run-0", "c1", "success", "【陈旧】上一次运行的同 id 结果")

	report, err := Recover(ctx, db, testLogger(t), fixedNow) // 参数与 §3.1 声明一致
	require.NoError(t, err)
	require.Equal(t, 1, report.RepairedGroups)
	require.Equal(t, 1, report.ClosedRuns)

	got := loadMessages(t, db, recoverySession)
	require.Len(t, got, 3)
	require.Equal(t, "未执行（进程中断）", got[2].Content,
		"只有其它 Run 的审计行 = 本 Run 没有行 = 确定没执行（按位置判组，不得误配陈旧行）")
	require.Equal(t, "c1", got[2].ToolCallID, "合成结果必须答在它属于的那次调用上")
	require.Equal(t, core.RoleTool, got[2].Role)
	require.Equal(t, "completed", loadRunStatus(t, db, "run-alone"), "已结束的 Run 不得被覆盖")

	// ② 当前 Run 有 pending 行 → 同 id 的调用必须写"结果未知"（不得写成"未执行"）
	seedRun(t, db, "run-2", "running")
	seedMessage(t, db, recoverySession, 4, core.RoleUser, "再读一次", "run-2")
	seedMessage(t, db, recoverySession, 5, core.RoleAssistant, "", "run-2",
		core.ToolCall{ID: "c2", Name: "read_file"})
	seedToolInvocation(t, db, "run-2", "c2", "pending", "")

	_, err = Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	got = loadMessages(t, db, recoverySession)
	require.Equal(t, "结果未知（进程中断，无法确定是否已产生外部副作用）", got[5].Content,
		"执行过但终态未落 → 结果未知，不得写成『未执行』")

	// ③ 幂等：连跑第二次零变化
	before := dumpMessages(t, db, recoverySession)
	_, err = Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Equal(t, before, dumpMessages(t, db, recoverySession))
}

// ---- M3 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ②）：组内每个 `tool_call_id` 都有结果消息 → 不是半截组，
// 一条消息都不写（`repaired_groups` 只数真补过的组）。运行已终结，故 ④ 也不收尾。
func TestRecovery_LeavesCompleteGroupsUntouched(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seedRun(t, db, "run-1", "cancelled")
	seedMessage(t, db, recoverySession, 1, core.RoleUser, "读文件", "run-1")
	seedMessage(t, db, recoverySession, 2, core.RoleAssistant, "", "run-1",
		core.ToolCall{ID: "c1", Name: "read_file"})
	seedToolMessage(t, db, recoverySession, 3, "run-1", "c1", "文件内容")

	before := dumpMessages(t, db, recoverySession)
	report, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Zero(t, report.RepairedGroups)
	require.Zero(t, report.ClosedRuns)
	require.Equal(t, before, dumpMessages(t, db, recoverySession), "完整的组不得被改写")
}

// ---- M4 -------------------------------------------------------------------

// 中文验收点（契约 §4.4 五分支第 1 行）：组内有终态审计行的调用**还原真实结果**；
// 同组另一个没有行的调用按进程中断合成；补齐按**声明序**落消息；且**只进消息、不进审计**。
func TestRecovery_RestoresTerminalAuditResult(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seedRun(t, db, "run-1", "running")
	seedMessage(t, db, recoverySession, 1, core.RoleAssistant, "", "run-1",
		core.ToolCall{ID: "c1", Name: "read_file"}, core.ToolCall{ID: "c2", Name: "read_file"})
	seedToolInvocation(t, db, "run-1", "c1", "success", "真实结果全文") // 有终态行 → 还原
	invocationsBefore := countToolInvocations(t, db)

	report, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 1, report.RepairedGroups, "一组算一次，不按缺失调用计")

	got := loadMessages(t, db, recoverySession)
	require.Len(t, got, 3)
	require.Equal(t, "真实结果全文", got[1].Content, "有终态审计行 → 还原真实结果，不写合成文本")
	require.Equal(t, "c1", got[1].ToolCallID, "补齐按声明序")
	require.Equal(t, "未执行（进程中断）", got[2].Content, "同组无行的调用按进程中断合成")
	require.Equal(t, "c2", got[2].ToolCallID)
	require.Equal(t, invocationsBefore, countToolInvocations(t, db), "补齐只进消息、不进审计")
}

// ---- M5 -------------------------------------------------------------------

// 中文验收点（契约 §4.4 五分支第 3/4 行）：**无**审计行时按 Run 的状态分类——cancelled
// 的组取"已取消…"、limit_reached 的组取"未执行（工具调用次数超限）"；两种情况都不许
// 被 ④ 顺手收尾覆盖（终态就是终态）。
func TestRecovery_ClassifiesMissingRowsByRunState(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seedRun(t, db, "run-cancelled", "cancelled")
	seedRun(t, db, "run-limit", "limit_reached")
	seedMessage(t, db, "s-cancel", 1, core.RoleAssistant, "", "run-cancelled",
		core.ToolCall{ID: "c1", Name: "read_file"})
	seedMessage(t, db, "s-limit", 1, core.RoleAssistant, "", "run-limit",
		core.ToolCall{ID: "c1", Name: "read_file"})

	report, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, report.RepairedGroups)
	require.Zero(t, report.ClosedRuns, "已结束的运行不归恢复收尾")

	require.Equal(t, core.TextToolCancelled, loadMessages(t, db, "s-cancel")[1].Content,
		"取消时尚未执行 → 已取消文本（而不是进程中断）")
	require.Equal(t, core.TextToolBudget, loadMessages(t, db, "s-limit")[1].Content,
		"预算耗尽未轮到 → 次数超限文本")
	require.Equal(t, "cancelled", loadRunStatus(t, db, "run-cancelled"))
	require.Equal(t, "limit_reached", loadRunStatus(t, db, "run-limit"))
}

// ---- M6 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ④ + 明文的"不得覆盖已结束状态"）：四种终态行（completed /
// limit_reached / cancelled / failed）一字不改；仍为 running 的行标 failed、reason 逐字、
// finished_at = 传入的 now。
func TestRecovery_ClosesOnlyRunningRuns(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	terminal := map[string]string{
		"run-done": "completed", "run-limit": "limit_reached",
		"run-cancel": "cancelled", "run-failed": "failed",
	}
	before := make(map[string]runState, len(terminal))
	for runID, status := range terminal {
		seedDoneRun(t, db, runID, status)
		before[runID] = loadRunState(t, db, runID)
	}
	seedRun(t, db, "run-crash", "running")

	report, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 1, report.ClosedRuns)

	for runID := range terminal {
		require.Equal(t, before[runID], loadRunState(t, db, runID), "已结束的行 %s 不得被覆盖", runID)
	}
	crashed := loadRunState(t, db, "run-crash")
	require.Equal(t, "failed", crashed.Status)
	require.Equal(t, "进程中断（启动时发现未完成的运行）", crashed.Reason.String, "reason 逐字（契约 §4.3.2）")
	require.Equal(t, fixedNow.UnixMilli(), crashed.FinishedAt.Int64, "finished_at 取传入的 now")
}

// ---- M7 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ④）：**无状态**运行（`session_id` 为 NULL）同样收尾——
// 它没有会话历史，账本却是同一本；收尾只动状态三列，会话列保持 NULL。
func TestRecovery_ClosesStatelessRuns(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seedStatelessRun(t, db, "run-invoke", "running")
	seedRun(t, db, "run-chat", "running")

	report, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, report.ClosedRuns)

	stateless := loadRunState(t, db, "run-invoke")
	require.Equal(t, "failed", stateless.Status, "无状态 Run 也要收尾")
	require.False(t, stateless.SessionID.Valid, "收尾不得给无状态 Run 补一个会话列")
	require.Equal(t, fixedNow.UnixMilli(), stateless.FinishedAt.Int64)
}

// ---- M8 -------------------------------------------------------------------

// 中文验收点（"组看消息、Run 看表"）：一条消息都没有就崩掉的运行照样收尾——消息侧看不出
// 异常，只有账本知道上次没跑完；同时**零消息写入**。
func TestRecovery_ClosesRunsWithoutHalfGroups(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seedRun(t, db, "run-silent", "running")

	before := dumpMessages(t, db, recoverySession)
	report, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Zero(t, report.RepairedGroups)
	require.Equal(t, 1, report.ClosedRuns, "没有半截组不影响收尾")
	require.Equal(t, before, dumpMessages(t, db, recoverySession))

	crashed := loadRunState(t, db, "run-silent")
	require.Equal(t, "failed", crashed.Status)
	require.Equal(t, "进程中断（启动时发现未完成的运行）", crashed.Reason.String)
}

// ---- M9 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ③ 的"同一短事务"）：收尾语句被注入失败时，**同一事务里的
// 组补齐一起回滚**——该 Run 保持 running 等下次重扫，绝不出现"Run 已翻 failed 而组仍半截"
// 的中间态；另一个 Run 的事务照常提交（per-Run 事务，不是一个大事务）。
// A trigger that blocks the closure statement proves repair and closure share one
// transaction per run, and that one failing run cannot roll back another's repair.
func TestRecovery_RepairsAndClosesInOneTransaction(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	// 扫描序按 session_id：s1 的 Run 先处理并提交，s2 的 Run 随后失败。
	seedRun(t, db, "run-ok", "running")
	seedRun(t, db, "run-bad", "running")
	seedMessage(t, db, "s1", 1, core.RoleAssistant, "", "run-ok", core.ToolCall{ID: "c1", Name: "read_file"})
	seedMessage(t, db, "s2", 1, core.RoleAssistant, "", "run-bad", core.ToolCall{ID: "c9", Name: "read_file"})

	_, err := db.ExecContext(ctx, `CREATE TRIGGER recovery_block_close
	    BEFORE UPDATE ON runs
	    WHEN NEW.status = 'failed' AND OLD.run_id = 'run-bad'
	    BEGIN SELECT RAISE(ABORT, '注入的收尾失败'); END`)
	require.NoError(t, err)

	report, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.Error(t, err, "修复失败必须如实上抛（启动方据此决定要不要开跑）")
	require.Contains(t, err.Error(), "run-bad")
	require.Equal(t, 1, report.RepairedGroups, "先处理的 Run 照常提交")
	require.Equal(t, 1, report.ClosedRuns)

	require.Equal(t, "failed", loadRunStatus(t, db, "run-ok"))
	require.Equal(t, "running", loadRunStatus(t, db, "run-bad"),
		"收尾失败 → 整个事务回滚，该 Run 保持 running 等下次重扫")
	require.Len(t, loadMessages(t, db, "s2"), 1, "同一事务里的补齐也被回滚（不得只写一半）")
	require.Len(t, loadMessages(t, db, "s1"), 2)
}

// ---- M10 ------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 结尾"幂等"）：把三种待处理态一次给足（两表 pending 行、带半截组的
// running、没有半截组的 running），连跑两次——第二次三计数为零，整库逐行不变。
func TestRecovery_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seedRun(t, db, "run-crash", "running")
	seedRun(t, db, "run-quiet", "running")
	seedMessage(t, db, recoverySession, 1, core.RoleAssistant, "", "run-crash",
		core.ToolCall{ID: "c1", Name: "read_file"})
	seedToolInvocation(t, db, "run-crash", "c1", "pending", "")
	seedLLMCall(t, db, "run-crash", "pending")

	first, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, first.Unknowns, "两张表各一行 pending")
	require.Equal(t, 1, first.RepairedGroups)
	require.Equal(t, 2, first.ClosedRuns)

	before := dumpAll(t, db)
	second, err := Recover(ctx, db, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Zero(t, second.Unknowns, "没有再可归一化的行")
	require.Zero(t, second.RepairedGroups, "组已完整即 no-op")
	require.Zero(t, second.ClosedRuns, "没有 running 的行可收")
	require.Equal(t, before, dumpAll(t, db), "连跑第二次零变化")
}

// dumpAll 是全库规范快照（四张表）。幂等要用整库说话：只比消息会漏掉 runs 或审计表的二次改写。
func dumpAll(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return []string{
		dumpMessages(t, db, recoverySession),
		dumpRows(t, db, `SELECT run_id, status, COALESCE(reason, ''), COALESCE(finished_at, -1) FROM runs ORDER BY run_id`),
		dumpRows(t, db, `SELECT id, status, COALESCE(error_kind, '') FROM llm_calls ORDER BY id`),
		dumpRows(t, db, `SELECT id, status, COALESCE(error_kind, '') FROM tool_invocations ORDER BY id`),
	}
}

// ---- M11 ------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ⑥）：结果记 **INFO** 且三键齐备（`unknowns` /
// `repaired_groups` / `closed_runs`），键值与返回的读数同源。
func TestRecovery_LogsStepCounts(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	seedRun(t, db, "run-crash", "running")
	seedRun(t, db, "run-quiet", "running")
	seedMessage(t, db, recoverySession, 1, core.RoleAssistant, "", "run-crash",
		core.ToolCall{ID: "c1", Name: "read_file"})
	seedToolInvocation(t, db, "run-crash", "c1", "pending", "")
	seedLLMCall(t, db, "run-crash", "pending")

	logger, logs := captureLogger()
	report, err := Recover(ctx, db, logger, fixedNow)
	require.NoError(t, err)
	require.Equal(t, RecoveryReport{Unknowns: 2, RepairedGroups: 1, ClosedRuns: 2}, report)

	// 判卷面只读收到的 logger：core 在"结果未知"分支打的 WARN 走 slog.Default()，
	// 不在本捕获里——这里要判的是恢复自己的启动摘要，不是分类层的告警。
	log := logs.text()
	require.Contains(t, log, "level=INFO")
	require.Contains(t, log, "崩溃恢复扫描完成")
	require.Contains(t, log, "unknowns=2")
	require.Contains(t, log, "repaired_groups=1")
	require.Contains(t, log, "closed_runs=2")
}

// ---- M12 ------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ③ 的明文）：半截组的 `run_id` 为空、或指向一个**不存在**的
// 运行行时，**仍然补齐**（历史里少一条 tool 结果比多一条诊断严重得多），并 WARN 点名该
// run_id 与会话；这类组不产生收尾。
func TestRecovery_RepairsGroupsWithUnknownRun(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	// (a) 组的 run_id 为空（不承载 Run 的消息）
	seedMessage(t, db, "s-orphan", 1, core.RoleAssistant, "", "",
		core.ToolCall{ID: "c7", Name: "read_file"})
	// (b) run_id 有值，但账本里根本没有这一行
	seedMessage(t, db, "s-ghost", 1, core.RoleAssistant, "", "run-ghost",
		core.ToolCall{ID: "c8", Name: "read_file"})

	logger, logs := captureLogger()
	report, err := Recover(ctx, db, logger, fixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, report.RepairedGroups, "找不到运行行仍然补组")
	require.Zero(t, report.ClosedRuns)

	orphan := loadMessages(t, db, "s-orphan")
	require.Len(t, orphan, 2)
	require.Equal(t, core.TextToolInterrupted, orphan[1].Content)
	require.Equal(t, "c7", orphan[1].ToolCallID)
	ghost := loadMessages(t, db, "s-ghost")
	require.Len(t, ghost, 2)
	require.Equal(t, core.TextToolInterrupted, ghost[1].Content)

	log := logs.text()
	require.Contains(t, log, "level=WARN")
	require.Contains(t, log, "run_id=run-ghost", "WARN 点名该 run_id")
	require.Contains(t, log, "session_id=s-orphan", "WARN 点名该会话")
	require.Contains(t, log, "session_id=s-ghost")
}

// ---- 运行资源账（约束 19 的恢复面） -----------------------------------------

// recoveryLedgerManifest 在资源账夹具上再冻结一个托管凭据与两个通知渠道：正常收尾
// `FinishRun` 释放三张引用表（资源 / 凭据 / 通知），恢复必须逐表同口径——只释放资源行
// 会让已失败的 Run 永远点名凭据与渠道。
// recoveryLedgerManifest adds a managed credential pin and two notify channels on top of
// the resource fixture so all three reference tables hold unreleased rows.
func recoveryLedgerManifest(t *testing.T, fx runManifestFixture) core.RunResourceManifest {
	t.Helper()
	resolved := fx.resolved
	resolved.ProviderCredential = core.CredentialPin{
		Mode:          core.CredentialModeManaged,
		ID:            testCredentialID('C'),
		Version:       1,
		TargetBinding: testTargetBinding,
	}
	resolved.Notify = core.NotifyManifest{
		Mode: core.NotifyModeAllEligible,
		Channels: []core.NotifyChannelRef{
			{Name: "ops", ResourceUID: testResourceUID('D')},
			{Name: "mail", ResourceUID: testResourceUID('E')},
		},
	}
	manifest, err := core.BuildRunResourceManifest(testAgentUID, core.AgentActive, resolved)
	require.NoError(t, err)
	return manifest
}

// requireRunLedgerReleased 判定三张引用表全部已释放或全部仍未释放——恢复的事务把
// 资源引用、凭据固定与渠道冻结当一个整体，任何一张表单独落单都是中间态。
// requireRunLedgerReleased checks the three reference tables share one terminal state.
func requireRunLedgerReleased(t *testing.T, db *DB, runID string, wantReleased bool) {
	t.Helper()
	refs := readRunRefRows(t, db, runID)
	require.Len(t, refs, 4)
	for _, ref := range refs {
		require.Equal(t, wantReleased, ref.ReleasedAt.Valid,
			"运行资源引用的释放状态不符（%s/%s）", ref.Kind, ref.ResourceID)
		if wantReleased {
			require.Equal(t, fixedNow.UnixMilli(), ref.ReleasedAt.Int64)
		}
	}

	pins, err := NewRunCredentialStore(db).Pins(context.Background(), runID)
	require.NoError(t, err)
	require.Len(t, pins, 1, "托管凭据固定必须随准入写入")
	require.Equal(t, wantReleased, pins[0].ReleasedAt != nil,
		"凭据固定的释放状态必须与资源引用一致")
	if wantReleased {
		require.Equal(t, fixedNow.UnixMilli(), *pins[0].ReleasedAt)
	}

	channels, err := NewNotifyDeliveryStore(db).Refs(context.Background(), runID)
	require.NoError(t, err)
	require.Len(t, channels, 2, "渠道冻结必须随准入写入")
	for _, channel := range channels {
		require.Equal(t, wantReleased, channel.ReleasedAt != nil,
			"渠道冻结的释放状态必须与资源引用一致（%s）", channel.ChannelName)
		if wantReleased {
			require.Equal(t, fixedNow.UnixMilli(), *channel.ReleasedAt)
		}
	}
}

// TestRecovery_ClosesRunningRunsAndReleasesRefs —— 崩溃收尾与引用释放必须同事务：
// 正常路径一起提交（Run 终态 + 引用已释放）；释放失败时一起回滚（Run 保持 running，
// 等下次启动重扫），不得留下"Run 已 failed、版本却仍被引用"的中间态。
// TestRecovery_ClosesRunningRunsAndReleasesRefs pins the crash closure and the reference
// release to one transaction, in both the success and the failure path.
func TestRecovery_ClosesRunningRunsAndReleasesRefs(t *testing.T) {
	ctx := context.Background()

	t.Run("closes_and_releases", func(t *testing.T) {
		db := openTempDB(t)
		manifest := recoveryLedgerManifest(t, seedRunManifest(t, db))
		rec := startRow("web")
		require.NoError(t, NewRunStore(db).StartRun(ctx, rec, manifest))

		report, err := Recover(ctx, db.write, testLogger(t), fixedNow)
		require.NoError(t, err)
		require.Equal(t, 1, report.ClosedRuns)

		row := readRunRow(t, db, rec.RunID)
		require.Equal(t, string(core.StatusFailed), row.Status)
		require.Equal(t, recoveryCloseReason, row.Reason.String)
		require.Equal(t, fixedNow.UnixMilli(), row.FinishedAt.Int64)

		requireRunLedgerReleased(t, db, rec.RunID, true)
	})

	t.Run("release_failure_keeps_run_running", func(t *testing.T) {
		db := openTempDB(t)
		manifest := recoveryLedgerManifest(t, seedRunManifest(t, db))
		rec := startRow("web")
		require.NoError(t, NewRunStore(db).StartRun(ctx, rec, manifest))

		_, err := db.write.ExecContext(ctx, `CREATE TRIGGER recovery_block_release
		    BEFORE UPDATE ON run_resource_refs
		    BEGIN SELECT RAISE(ABORT, '注入的引用释放失败'); END`)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := db.write.ExecContext(context.Background(),
				`DROP TRIGGER IF EXISTS recovery_block_release`)
			require.NoError(t, err)
		})

		report, err := Recover(ctx, db.write, testLogger(t), fixedNow)
		require.Error(t, err, "释放失败必须如实上抛（启动方据此决定要不要开跑）")
		require.Contains(t, err.Error(), rec.RunID)
		require.Zero(t, report.ClosedRuns)

		require.Equal(t, string(core.StatusRunning), loadRunStatus(t, db.write, rec.RunID),
			"释放失败 → 整个收尾事务回滚，Run 保持 running 等下次重扫")
		requireRunLedgerReleased(t, db, rec.RunID, false)
	})
}

// TestRecovery_BackfillsTerminalRunResidualRefs —— 已终态 Run 的残留引用由启动恢复补齐
// （约束 19 末句）：④ 的扫描面看不到它们，step ⑤ 的幂等语句按"Run 已终态"补齐，
// 且**不改**已判定的状态、原因与终态时刻。
// TestRecovery_BackfillsTerminalRunResidualRefs releases residual references of runs that
// are already terminal, without re-judging their state.
func TestRecovery_BackfillsTerminalRunResidualRefs(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	manifest := recoveryLedgerManifest(t, seedRunManifest(t, db))
	rec := startRow("web")
	require.NoError(t, NewRunStore(db).StartRun(ctx, rec, manifest))

	// 模拟"终态已写、释放没跑到"的残留：直接改状态列，不走 FinishRun（它会连带释放）。
	settled := fixedStart.Add(10 * time.Minute)
	_, err := db.write.ExecContext(ctx,
		`UPDATE runs SET status = ?, reason = ?, finished_at = ? WHERE run_id = ?`,
		string(core.StatusCompleted), "人工修库留下的终态", settled.UnixMilli(), rec.RunID)
	require.NoError(t, err)

	report, err := Recover(ctx, db.write, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Zero(t, report.ClosedRuns, "已终态的 Run 不在收尾扫描面里")

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusCompleted), row.Status, "已判定的终态不得被恢复改口")
	require.Equal(t, "人工修库留下的终态", row.Reason.String)
	require.Equal(t, settled.UnixMilli(), row.FinishedAt.Int64)

	requireRunLedgerReleased(t, db, rec.RunID, true)
}

// TestRecovery_DoesNotFabricateResourceLedger —— 恢复收尾历史运行（本能力之前产生的、
// 没有清单的账本）时**不得补造**资源账：清单与引用一行为零，Run 照常收尾。
// 补造的账会被读成事实，比"未记录"更糟（约束 24）。
// TestRecovery_DoesNotFabricateResourceLedger closes legacy runs without inventing a
// manifest or references for them.
func TestRecovery_DoesNotFabricateResourceLedger(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	seedRun(t, db.write, "run-legacy", "running")

	report, err := Recover(ctx, db.write, testLogger(t), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 1, report.ClosedRuns)
	require.Equal(t, string(core.StatusFailed), loadRunStatus(t, db.write, "run-legacy"))

	var manifests, refs int
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_manifests WHERE run_id = ?`, "run-legacy").Scan(&manifests))
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_refs WHERE run_id = ?`, "run-legacy").Scan(&refs))
	require.Zero(t, manifests, "恢复不得为历史运行补造清单")
	require.Zero(t, refs, "恢复不得为历史运行补造引用")
}

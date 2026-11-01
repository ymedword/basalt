//go:build integration

package postgres

// 本文件是崩溃恢复扫描（docs/us/us4-react-loop.md §2.3 流程 G 步骤 ①②③④⑥）的专属判卷面：
// 12 条 M1~M12，另加运行资源账的三条恢复用例。由 SQLite 谱系
// `internal/storage/recovery_test.go` 逐条移植：测试函数名、断言、中文验收点与逆改点
// 注释**原样保留**；只替换「库怎么来」与「原始 SQL 的方言」——
//   - `openTempDB(t).write` → `portTestDB(t).pool`（隔离 schema、读写同一句柄）；
//   - `?` → `$1..$n`；`INSERT OR IGNORE` → `ON CONFLICT DO NOTHING`；
//   - `LastInsertId` → `RETURNING id`（pgx stdlib 不支持 LastInsertId）；
//   - `runs` 的归属列（caller_id / authorizing_kind / key_id / actor）在 PG 上是 NOT NULL
//     且带 CHECK，夹具统一取内置管理员形态（local-admin / admin-builtin / 无 Key）；
//   - M9 的故障注入从 SQLite 的 `CREATE TRIGGER ... RAISE(ABORT)` 换成等价的 PG
//     plpgsql 触发器（RAISE EXCEPTION），判据（事务一起回滚）不变；
//   - 辅助函数统一加 `rec` 前缀，避免与并行移植的其他仓储用例撞名。
//
// 两处刻意的形态。① **包内测试**（`package postgres`）：`db.pool` 未导出，包外递不进去；
// `Recover` 的冻结签名收的正是裸 `*sql.DB`。② **夹具直接写库**：恢复要处理的正是"上一个
// 进程死在半路"的既成事实，任何 API 都造不出这种状态，只能按列摆出来。
// Ported from the SQLite lineage verbatim; only the handle and the SQL dialect differ.

import (
	"context"
	"crypto/sha256"
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

// recRecoverySession 是本文件多数用例的会话标识（无状态 Run 走 recSeedStatelessRun）。
const recRecoverySession = "s1"

// recFixedNow 是恢复扫描的"当前时刻"（时间一律显式传入）；收尾的 finished_at 与合成消息的
// created_at 都取它，故两者都可逐字断言。
var recFixedNow = time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)

// ---- 夹具 ------------------------------------------------------------------

// recTestDB 给每个用例一个独立的隔离库，并把**连接池**交给恢复（对应 SQLite 谱系的
// `newTestDB` 返回写池；PG 读写同一句柄）。
func recTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return portTestDB(t).pool
}

// recSeedSession 建会话行。`messages.session_id` 有外键（PG 恒生效），不先有会话行就插不进
// 消息；重复调用是 no-op（主键冲突忽略）。
func recSeedSession(t *testing.T, db *sql.DB, sessionID string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO sessions
		   (session_id, profile_name, channel, user_id, created_at, last_active_at)
		 VALUES ($1, $2, 'cli', 'u1', $3, $3)
		 ON CONFLICT (session_id) DO NOTHING`,
		sessionID, "assistant", recFixedNow.UnixMilli())
	require.NoError(t, err)
}

// recSeedRunRow 建一行账本。终态行的 `reason` / `finished_at` 由用例显式给（nil → NULL）——
// 只有它们非空，"恢复有没有覆盖已结束的行"才判得出来。PG 的归属四列 NOT NULL 且带
// CHECK，统一取内置管理员形态（与恢复只改 actor 的纪律相容）。
func recSeedRunRow(t *testing.T, db *sql.DB, runID, status string, session sql.NullString, reason any, finishedAt any) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO runs
		   (run_id, session_id, agent, channel, user_id, trigger, status,
		    reason, iterations, tool_calls, config_hash, started_at, finished_at,
		    caller_id, authorizing_kind, key_id, actor)
		 VALUES ($1, $2, 'assistant', 'cli', 'u1', 'user', $3, $4, 0, 0, NULL, $5, $6,
		         'local-admin', 'admin-builtin', NULL, 'scheduler')`,
		runID, session, status, reason, recFixedNow.Add(-time.Hour).UnixMilli(), finishedAt)
	require.NoError(t, err)
}

// recSeedRun 建一行挂在 recRecoverySession 上的账本（起始态与终态都给得出来）。
func recSeedRun(t *testing.T, db *sql.DB, runID, status string) {
	t.Helper()
	recSeedRunRow(t, db, runID, status, sql.NullString{String: recRecoverySession, Valid: true}, nil, nil)
}

// recSeedStatelessRun 建一行**无状态**账本（`session_id` 为 NULL——invoke 形态）。
func recSeedStatelessRun(t *testing.T, db *sql.DB, runID, status string) {
	t.Helper()
	recSeedRunRow(t, db, runID, status, sql.NullString{}, nil, nil)
}

// recSeedDoneRun 建一行**已结束且带终态证据**的账本（reason 与 finished_at 都非空）。
func recSeedDoneRun(t *testing.T, db *sql.DB, runID, status string) {
	t.Helper()
	recSeedRunRow(t, db, runID, status, sql.NullString{String: recRecoverySession, Valid: true},
		"原有中止原因", recFixedNow.Add(-time.Hour).UnixMilli())
}

// recSeedMessage 追加一条消息；assistant 行可带工具调用（可变参数，其余角色留空）。
// `tool_calls` 列的形态与 schema 注释一致：`[{id,name,arguments}]`。
// The tool role takes its tool_call_id from recSeedToolMessage instead: a result message
// answers one specific call, and that binding is what the positional grouping keys on.
func recSeedMessage(t *testing.T, db *sql.DB, sessionID string, seq int64, role, content, runID string, calls ...core.ToolCall) {
	t.Helper()
	recSeedSession(t, db, sessionID)

	var callsJSON any
	if len(calls) > 0 {
		raw, err := json.Marshal(calls)
		require.NoError(t, err)
		callsJSON = string(raw)
	}
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO messages (session_id, run_id, seq, role, content, tool_calls, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		sessionID, nullIfEmpty(runID), seq, role, content, callsJSON, recFixedNow.UnixMilli())
	require.NoError(t, err)
}

// recSeedToolMessage 追加一条 `role=tool` 的结果消息（`tool_call_id` 就是它答的那次调用）。
func recSeedToolMessage(t *testing.T, db *sql.DB, sessionID string, seq int64, runID, callID, content string) {
	t.Helper()
	recSeedSession(t, db, sessionID)

	_, err := db.ExecContext(context.Background(),
		`INSERT INTO messages (session_id, run_id, seq, role, content, tool_call_id, created_at)
		 VALUES ($1, $2, $3, 'tool', $4, $5, $6)`,
		sessionID, nullIfEmpty(runID), seq, content, callID, recFixedNow.UnixMilli())
	require.NoError(t, err)
}

// recSeedToolInvocation 预置一行工具审计（`resultContent` 非空即写进 `result_json.content`——
// 还原分支读的就是它）。返回行 id 供逐列回读（PG 走 `RETURNING id`）。
func recSeedToolInvocation(t *testing.T, db *sql.DB, runID, callID, status, resultContent string) int64 {
	t.Helper()
	var result any
	if resultContent != "" {
		raw, err := json.Marshal(map[string]string{"content": resultContent})
		require.NoError(t, err)
		result = string(raw)
	}
	var id int64
	err := db.QueryRowContext(context.Background(),
		`INSERT INTO tool_invocations
		   (run_id, session_id, tool_call_id, tool_name, input_json, result_json,
		    status, error_message, error_kind, duration_ms, started_at, finished_at)
		 VALUES ($1, $2, $3, 'read_file', '{}', $4, $5, NULL, NULL, NULL, $6, NULL)
		 RETURNING id`,
		runID, recRecoverySession, callID, result, status, recFixedNow.UnixMilli()).Scan(&id)
	require.NoError(t, err)
	return id
}

// recSeedLLMCall 预置一行模型调用审计（NOT NULL 列取固定值，判卷只关心 status / error_kind）。
func recSeedLLMCall(t *testing.T, db *sql.DB, runID, status string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRowContext(context.Background(),
		`INSERT INTO llm_calls
		   (run_id, session_id, round, provider, model, prompt_tokens, completion_tokens,
		    total_tokens, duration_ms, status, error_message, error_kind, config_hash, created_at)
		 VALUES ($1, $2, 1, 'fake', 'm', 0, 0, 0, 0, $3, NULL, NULL, NULL, $4)
		 RETURNING id`,
		runID, recRecoverySession, status, recFixedNow.UnixMilli()).Scan(&id)
	require.NoError(t, err)
	return id
}

// ---- 判卷读面 --------------------------------------------------------------

// recLoadMessages 按 seq 升序读回整个会话（断言"补出来的是哪一条、内容是什么"）。
func recLoadMessages(t *testing.T, db *sql.DB, sessionID string) []core.Message {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT role, COALESCE(content, ''), COALESCE(tool_call_id, ''), COALESCE(tool_calls, '')
		   FROM messages WHERE session_id = $1 ORDER BY seq`, sessionID)
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

// recDumpMessages 是整个会话的**规范快照**（全列 + seq），幂等用例前后比对。
func recDumpMessages(t *testing.T, db *sql.DB, sessionID string) string {
	t.Helper()
	return recDumpRows(t, db,
		`SELECT seq || '|' || role || '|' || COALESCE(content, '') || '|' || COALESCE(tool_calls, '')
		        || '|' || COALESCE(tool_call_id, '') || '|' || COALESCE(run_id, '') || '|' || created_at
		   FROM messages WHERE session_id = $1 ORDER BY seq`, sessionID)
}

// recDumpRows 跑一条**完整**查询（调用点写全 SQL 字面量，值一律走参数）并按行拼成规范文本。
func recDumpRows(t *testing.T, db *sql.DB, query string, args ...any) string {
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

// recLoadRunStatus 只判状态列（多数断言用它就够）。
func recLoadRunStatus(t *testing.T, db *sql.DB, runID string) string {
	t.Helper()
	return recLoadRunState(t, db, runID).Status
}

// recRunState 是账本行的判卷形态（可空列用 Null 形态：NULL 与空串不是一回事）。
type recRunState struct {
	Status     string
	SessionID  sql.NullString
	Reason     sql.NullString
	FinishedAt sql.NullInt64
}

// recLoadRunState 读回账本行的状态三件 + 会话列（"收尾只动状态三列"也判得出来）。
func recLoadRunState(t *testing.T, db *sql.DB, runID string) recRunState {
	t.Helper()
	var state recRunState
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status, session_id, reason, finished_at FROM runs WHERE run_id = $1`, runID).
		Scan(&state.Status, &state.SessionID, &state.Reason, &state.FinishedAt))
	return state
}

// recLoadInvocationAudit 读回工具审计行的 (status, error_kind)。
func recLoadInvocationAudit(t *testing.T, db *sql.DB, id int64) (string, string) {
	t.Helper()
	var status string
	var kind sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status, error_kind FROM tool_invocations WHERE id = $1`, id).Scan(&status, &kind))
	return status, kind.String
}

// recLoadLLMCallAudit 读回模型调用审计行的 (status, error_kind)。
func recLoadLLMCallAudit(t *testing.T, db *sql.DB, id int64) (string, string) {
	t.Helper()
	var status string
	var kind sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT status, error_kind FROM llm_calls WHERE id = $1`, id).Scan(&status, &kind))
	return status, kind.String
}

// recCountToolInvocations 数工具审计行（"补齐只进消息、不进审计"的判据）。
func recCountToolInvocations(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM tool_invocations`).Scan(&count))
	return count
}

// ---- 日志面 ----------------------------------------------------------------

// recTestLogger 是"只求不响"的日志面（多数用例不判日志）。
func recTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recLogCapture 是日志判卷面（M11 读 ⑥ 的 INFO 三键、M12 读 WARN 点名）。
// 无需加锁：恢复是**同步**的（宪法 VII），写与读发生在同一个 goroutine 上。
type recLogCapture struct{ builder strings.Builder }

func (c *recLogCapture) Write(p []byte) (int, error) { return c.builder.Write(p) }

func (c *recLogCapture) text() string { return c.builder.String() }

// recCaptureLogger 返回"写进内存"的日志面。
func recCaptureLogger() (*slog.Logger, *recLogCapture) {
	capture := &recLogCapture{}
	return slog.New(slog.NewTextHandler(capture, nil)), capture
}

// ---- M1 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ①）：两张审计表的 `pending` 行都归一化为 `unknown` 且
// `error_kind` 写 `unknown`；**只改 pending**——终态行一字不动；读数 `unknowns` 是两表合计。
func TestRecovery_NormalizesPendingRowsToUnknown(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	pendingCall := recSeedToolInvocation(t, db, "run-1", "c1", "pending", "")
	doneCall := recSeedToolInvocation(t, db, "run-1", "c2", "success", "已经执行过的结果")
	pendingLLM := recSeedLLMCall(t, db, "run-1", "pending")
	failedLLM := recSeedLLMCall(t, db, "run-1", "failed")

	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, report.Unknowns, "两表各一行 pending，合计 2")
	require.Zero(t, report.RepairedGroups, "没有消息就没有半截组")
	require.Zero(t, report.ClosedRuns, "没有 running 的账本行")

	status, kind := recLoadInvocationAudit(t, db, pendingCall)
	require.Equal(t, "unknown", status, "崩溃窗口的 pending → unknown（工具是否已产生副作用无法确定）")
	require.Equal(t, "unknown", kind, "error_kind 同步写 unknown")

	status, _ = recLoadInvocationAudit(t, db, doneCall)
	require.Equal(t, "success", status, "终态行不得被覆盖")
	status, kind = recLoadLLMCallAudit(t, db, pendingLLM)
	require.Equal(t, "unknown", status)
	require.Equal(t, "unknown", kind)
	status, _ = recLoadLLMCallAudit(t, db, failedLLM)
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
	db := recTestDB(t)
	recSeedRun(t, db, "run-1", "running")       // 当前 Run
	recSeedRun(t, db, "run-alone", "completed") // 已结束的 Run——不得被覆盖
	recSeedMessage(t, db, recRecoverySession, 1, core.RoleUser, "读文件", "run-1")
	recSeedMessage(t, db, recRecoverySession, 2, core.RoleAssistant, "", "run-1",
		core.ToolCall{ID: "c1", Name: "read_file", Arguments: `{"path":"notes.txt"}`}) // 本轮组：缺 tool 行
	recSeedToolInvocation(t, db, "run-0", "c1", "success", "【陈旧】上一次运行的同 id 结果")

	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow) // 参数与 §3.1 声明一致
	require.NoError(t, err)
	require.Equal(t, 1, report.RepairedGroups)
	require.Equal(t, 1, report.ClosedRuns)

	got := recLoadMessages(t, db, recRecoverySession)
	require.Len(t, got, 3)
	require.Equal(t, "未执行（进程中断）", got[2].Content,
		"只有其它 Run 的审计行 = 本 Run 没有行 = 确定没执行（按位置判组，不得误配陈旧行）")
	require.Equal(t, "c1", got[2].ToolCallID, "合成结果必须答在它属于的那次调用上")
	require.Equal(t, core.RoleTool, got[2].Role)
	require.Equal(t, "completed", recLoadRunStatus(t, db, "run-alone"), "已结束的 Run 不得被覆盖")

	// ② 当前 Run 有 pending 行 → 同 id 的调用必须写"结果未知"（不得写成"未执行"）
	recSeedRun(t, db, "run-2", "running")
	recSeedMessage(t, db, recRecoverySession, 4, core.RoleUser, "再读一次", "run-2")
	recSeedMessage(t, db, recRecoverySession, 5, core.RoleAssistant, "", "run-2",
		core.ToolCall{ID: "c2", Name: "read_file"})
	recSeedToolInvocation(t, db, "run-2", "c2", "pending", "")

	_, err = Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	got = recLoadMessages(t, db, recRecoverySession)
	require.Equal(t, "结果未知（进程中断，无法确定是否已产生外部副作用）", got[5].Content,
		"执行过但终态未落 → 结果未知，不得写成『未执行』")

	// ③ 幂等：连跑第二次零变化
	before := recDumpMessages(t, db, recRecoverySession)
	_, err = Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Equal(t, before, recDumpMessages(t, db, recRecoverySession))
}

// ---- M3 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ②）：组内每个 `tool_call_id` 都有结果消息 → 不是半截组，
// 一条消息都不写（`repaired_groups` 只数真补过的组）。运行已终结，故 ④ 也不收尾。
func TestRecovery_LeavesCompleteGroupsUntouched(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	recSeedRun(t, db, "run-1", "cancelled")
	recSeedMessage(t, db, recRecoverySession, 1, core.RoleUser, "读文件", "run-1")
	recSeedMessage(t, db, recRecoverySession, 2, core.RoleAssistant, "", "run-1",
		core.ToolCall{ID: "c1", Name: "read_file"})
	recSeedToolMessage(t, db, recRecoverySession, 3, "run-1", "c1", "文件内容")

	before := recDumpMessages(t, db, recRecoverySession)
	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Zero(t, report.RepairedGroups)
	require.Zero(t, report.ClosedRuns)
	require.Equal(t, before, recDumpMessages(t, db, recRecoverySession), "完整的组不得被改写")
}

// ---- M4 -------------------------------------------------------------------

// 中文验收点（契约 §4.4 五分支第 1 行）：组内有终态审计行的调用**还原真实结果**；
// 同组另一个没有行的调用按进程中断合成；补齐按**声明序**落消息；且**只进消息、不进审计**。
func TestRecovery_RestoresTerminalAuditResult(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	recSeedRun(t, db, "run-1", "running")
	recSeedMessage(t, db, recRecoverySession, 1, core.RoleAssistant, "", "run-1",
		core.ToolCall{ID: "c1", Name: "read_file"}, core.ToolCall{ID: "c2", Name: "read_file"})
	recSeedToolInvocation(t, db, "run-1", "c1", "success", "真实结果全文") // 有终态行 → 还原
	invocationsBefore := recCountToolInvocations(t, db)

	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Equal(t, 1, report.RepairedGroups, "一组算一次，不按缺失调用计")

	got := recLoadMessages(t, db, recRecoverySession)
	require.Len(t, got, 3)
	require.Equal(t, "真实结果全文", got[1].Content, "有终态审计行 → 还原真实结果，不写合成文本")
	require.Equal(t, "c1", got[1].ToolCallID, "补齐按声明序")
	require.Equal(t, "未执行（进程中断）", got[2].Content, "同组无行的调用按进程中断合成")
	require.Equal(t, "c2", got[2].ToolCallID)
	require.Equal(t, invocationsBefore, recCountToolInvocations(t, db), "补齐只进消息、不进审计")
}

// ---- M4b ------------------------------------------------------------------

// 中文验收点（US-16 人工走查发现，2026-10-31）：恢复从审计行还原的正文可能带 NUL——
// 外部工具结果在 `result_json` 里是 `\u0000` 转义，还原进 Go 字符串后是裸 0x00，
// PostgreSQL 的 `text` 列拒写（`SQLSTATE 22021`）。修复前这条坏字节让整个恢复事务回滚，
// `Recover` 返回错误、重路径装配失败、服务起不来（真实例的启动日志）。
// 判据：恢复与正常写入共用**同一个插入点**，其上的可存储性清洗把坏字节换成 U+FFFD 后
// 照常补齐；审计行保持原文（"工具实际返回了什么"的证据不改写）。
// 逆改点：去掉 `appendMessage` 里的清洗 → 本用例以 22021 失败，`require.NoError` 变红。
func TestRecovery_RestoresAuditResultWithUnstorableBytes(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	recSeedRun(t, db, "run-1", "running")
	recSeedMessage(t, db, recRecoverySession, 1, core.RoleAssistant, "", "run-1",
		core.ToolCall{ID: "c1", Name: "fetch"})
	recSeedToolInvocation(t, db, "run-1", "c1", "success", "正文\u0000尾部")

	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err, "恢复不得因外部结果的坏字节失败（真库报 SQLSTATE 22021）")
	require.Equal(t, 1, report.RepairedGroups)

	got := recLoadMessages(t, db, recRecoverySession)
	require.Len(t, got, 2)
	require.Equal(t, "正文�尾部", got[1].Content, "NUL 换成替换字符后照常落库")
	require.Equal(t, "c1", got[1].ToolCallID)

	// 审计行保持原文：清洗只治理会话消息，不改写"工具实际返回了什么"。
	var audit string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT result_json FROM tool_invocations WHERE run_id = $1 AND tool_call_id = $2`,
		"run-1", "c1").Scan(&audit))
	require.Contains(t, audit, `\u0000`, "审计保持 JSON 里的原始转义，不被清洗改写")
}

// ---- M5 -------------------------------------------------------------------

// 中文验收点（契约 §4.4 五分支第 3/4 行）：**无**审计行时按 Run 的状态分类——cancelled
// 的组取"已取消…"、limit_reached 的组取"未执行（工具调用次数超限）"；两种情况都不许
// 被 ④ 顺手收尾覆盖（终态就是终态）。
func TestRecovery_ClassifiesMissingRowsByRunState(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	recSeedRun(t, db, "run-cancelled", "cancelled")
	recSeedRun(t, db, "run-limit", "limit_reached")
	recSeedMessage(t, db, "s-cancel", 1, core.RoleAssistant, "", "run-cancelled",
		core.ToolCall{ID: "c1", Name: "read_file"})
	recSeedMessage(t, db, "s-limit", 1, core.RoleAssistant, "", "run-limit",
		core.ToolCall{ID: "c1", Name: "read_file"})

	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, report.RepairedGroups)
	require.Zero(t, report.ClosedRuns, "已结束的运行不归恢复收尾")

	require.Equal(t, core.TextToolCancelled, recLoadMessages(t, db, "s-cancel")[1].Content,
		"取消时尚未执行 → 已取消文本（而不是进程中断）")
	require.Equal(t, core.TextToolBudget, recLoadMessages(t, db, "s-limit")[1].Content,
		"预算耗尽未轮到 → 次数超限文本")
	require.Equal(t, "cancelled", recLoadRunStatus(t, db, "run-cancelled"))
	require.Equal(t, "limit_reached", recLoadRunStatus(t, db, "run-limit"))
}

// ---- M6 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ④ + 明文的"不得覆盖已结束状态"）：四种终态行（completed /
// limit_reached / cancelled / failed）一字不改；仍为 running 的行标 failed、reason 逐字、
// finished_at = 传入的 now。
func TestRecovery_ClosesOnlyRunningRuns(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	terminal := map[string]string{
		"run-done": "completed", "run-limit": "limit_reached",
		"run-cancel": "cancelled", "run-failed": "failed",
	}
	before := make(map[string]recRunState, len(terminal))
	for runID, status := range terminal {
		recSeedDoneRun(t, db, runID, status)
		before[runID] = recLoadRunState(t, db, runID)
	}
	recSeedRun(t, db, "run-crash", "running")

	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Equal(t, 1, report.ClosedRuns)

	for runID := range terminal {
		require.Equal(t, before[runID], recLoadRunState(t, db, runID), "已结束的行 %s 不得被覆盖", runID)
	}
	crashed := recLoadRunState(t, db, "run-crash")
	require.Equal(t, "failed", crashed.Status)
	require.Equal(t, "进程中断（启动时发现未完成的运行）", crashed.Reason.String, "reason 逐字（契约 §4.3.2）")
	require.Equal(t, recFixedNow.UnixMilli(), crashed.FinishedAt.Int64, "finished_at 取传入的 now")
}

// ---- M7 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ④）：**无状态**运行（`session_id` 为 NULL）同样收尾——
// 它没有会话历史，账本却是同一本；收尾只动状态三列，会话列保持 NULL。
func TestRecovery_ClosesStatelessRuns(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	recSeedStatelessRun(t, db, "run-invoke", "running")
	recSeedRun(t, db, "run-chat", "running")

	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, report.ClosedRuns)

	stateless := recLoadRunState(t, db, "run-invoke")
	require.Equal(t, "failed", stateless.Status, "无状态 Run 也要收尾")
	require.False(t, stateless.SessionID.Valid, "收尾不得给无状态 Run 补一个会话列")
	require.Equal(t, recFixedNow.UnixMilli(), stateless.FinishedAt.Int64)
}

// ---- M8 -------------------------------------------------------------------

// 中文验收点（"组看消息、Run 看表"）：一条消息都没有就崩掉的运行照样收尾——消息侧看不出
// 异常，只有账本知道上次没跑完；同时**零消息写入**。
func TestRecovery_ClosesRunsWithoutHalfGroups(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	recSeedRun(t, db, "run-silent", "running")

	before := recDumpMessages(t, db, recRecoverySession)
	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Zero(t, report.RepairedGroups)
	require.Equal(t, 1, report.ClosedRuns, "没有半截组不影响收尾")
	require.Equal(t, before, recDumpMessages(t, db, recRecoverySession))

	crashed := recLoadRunState(t, db, "run-silent")
	require.Equal(t, "failed", crashed.Status)
	require.Equal(t, "进程中断（启动时发现未完成的运行）", crashed.Reason.String)
}

// ---- M9 -------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ③ 的"同一短事务"）：收尾语句被注入失败时，**同一事务里的
// 组补齐一起回滚**——该 Run 保持 running 等下次重扫，绝不出现"Run 已翻 failed 而组仍半截"
// 的中间态；另一个 Run 的事务照常提交（per-Run 事务，不是一个大事务）。
// A trigger that blocks the closure statement proves repair and closure share one
// transaction per run, and that one failing run cannot roll back another's repair.
// （SQLite 谱系用 `CREATE TRIGGER ... RAISE(ABORT)` 注入；PostgreSQL 的等价形态见
// `recInstallCloseBlocker`。）
func TestRecovery_RepairsAndClosesInOneTransaction(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	// 扫描序按 session_id：s1 的 Run 先处理并提交，s2 的 Run 随后失败。
	recSeedRun(t, db, "run-ok", "running")
	recSeedRun(t, db, "run-bad", "running")
	recSeedMessage(t, db, "s1", 1, core.RoleAssistant, "", "run-ok", core.ToolCall{ID: "c1", Name: "read_file"})
	recSeedMessage(t, db, "s2", 1, core.RoleAssistant, "", "run-bad", core.ToolCall{ID: "c9", Name: "read_file"})

	recInstallCloseBlocker(t, db)

	report, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.Error(t, err, "修复失败必须如实上抛（启动方据此决定要不要开跑）")
	require.Contains(t, err.Error(), "run-bad")
	require.Equal(t, 1, report.RepairedGroups, "先处理的 Run 照常提交")
	require.Equal(t, 1, report.ClosedRuns)

	require.Equal(t, "failed", recLoadRunStatus(t, db, "run-ok"))
	require.Equal(t, "running", recLoadRunStatus(t, db, "run-bad"),
		"收尾失败 → 整个事务回滚，该 Run 保持 running 等下次重扫")
	require.Len(t, recLoadMessages(t, db, "s2"), 1, "同一事务里的补齐也被回滚（不得只写一半）")
	require.Len(t, recLoadMessages(t, db, "s1"), 2)
}

// recInstallCloseBlocker 注入"收尾语句必然失败"的触发器：只拦 `run-bad` 的 failed 翻转
// （与 SQLite 谱系的 WHEN 子句同判据），用来钉住"组补齐与收尾同一事务"。
// recInstallCloseBlocker makes exactly one run's closure statement fail.
func recInstallCloseBlocker(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`CREATE FUNCTION rec_recovery_block_close() RETURNS trigger AS $$
		 BEGIN
		     IF NEW.status = 'failed' AND OLD.run_id = 'run-bad' THEN
		         RAISE EXCEPTION '注入的收尾失败';
		     END IF;
		     RETURN NEW;
		 END;
		 $$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(),
		`CREATE TRIGGER recovery_block_close
		 BEFORE UPDATE ON runs
		 FOR EACH ROW EXECUTE FUNCTION rec_recovery_block_close()`)
	require.NoError(t, err)
}

// ---- M10 ------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 结尾"幂等"）：把三种待处理态一次给足（两表 pending 行、带半截组的
// running、没有半截组的 running），连跑两次——第二次三计数为零，整库逐行不变。
func TestRecovery_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	recSeedRun(t, db, "run-crash", "running")
	recSeedRun(t, db, "run-quiet", "running")
	recSeedMessage(t, db, recRecoverySession, 1, core.RoleAssistant, "", "run-crash",
		core.ToolCall{ID: "c1", Name: "read_file"})
	recSeedToolInvocation(t, db, "run-crash", "c1", "pending", "")
	recSeedLLMCall(t, db, "run-crash", "pending")

	first, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, first.Unknowns, "两张表各一行 pending")
	require.Equal(t, 1, first.RepairedGroups)
	require.Equal(t, 2, first.ClosedRuns)

	before := recDumpAll(t, db)
	second, err := Recover(ctx, db, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Zero(t, second.Unknowns, "没有再可归一化的行")
	require.Zero(t, second.RepairedGroups, "组已完整即 no-op")
	require.Zero(t, second.ClosedRuns, "没有 running 的行可收")
	require.Equal(t, before, recDumpAll(t, db), "连跑第二次零变化")
}

// recDumpAll 是全库规范快照（四张表）。幂等要用整库说话：只比消息会漏掉 runs 或审计表的二次改写。
func recDumpAll(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return []string{
		recDumpMessages(t, db, recRecoverySession),
		recDumpRows(t, db, `SELECT run_id, status, COALESCE(reason, ''), COALESCE(finished_at, -1) FROM runs ORDER BY run_id`),
		recDumpRows(t, db, `SELECT id, status, COALESCE(error_kind, '') FROM llm_calls ORDER BY id`),
		recDumpRows(t, db, `SELECT id, status, COALESCE(error_kind, '') FROM tool_invocations ORDER BY id`),
	}
}

// ---- M11 ------------------------------------------------------------------

// 中文验收点（契约 §4.3.2 步骤 ⑥）：结果记 **INFO** 且三键齐备（`unknowns` /
// `repaired_groups` / `closed_runs`），键值与返回的读数同源。
func TestRecovery_LogsStepCounts(t *testing.T) {
	ctx := context.Background()
	db := recTestDB(t)
	recSeedRun(t, db, "run-crash", "running")
	recSeedRun(t, db, "run-quiet", "running")
	recSeedMessage(t, db, recRecoverySession, 1, core.RoleAssistant, "", "run-crash",
		core.ToolCall{ID: "c1", Name: "read_file"})
	recSeedToolInvocation(t, db, "run-crash", "c1", "pending", "")
	recSeedLLMCall(t, db, "run-crash", "pending")

	logger, logs := recCaptureLogger()
	report, err := Recover(ctx, db, logger, recFixedNow)
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
	db := recTestDB(t)
	// (a) 组的 run_id 为空（不承载 Run 的消息）
	recSeedMessage(t, db, "s-orphan", 1, core.RoleAssistant, "", "",
		core.ToolCall{ID: "c7", Name: "read_file"})
	// (b) run_id 有值，但账本里根本没有这一行
	recSeedMessage(t, db, "s-ghost", 1, core.RoleAssistant, "", "run-ghost",
		core.ToolCall{ID: "c8", Name: "read_file"})

	logger, logs := recCaptureLogger()
	report, err := Recover(ctx, db, logger, recFixedNow)
	require.NoError(t, err)
	require.Equal(t, 2, report.RepairedGroups, "找不到运行行仍然补组")
	require.Zero(t, report.ClosedRuns)

	orphan := recLoadMessages(t, db, "s-orphan")
	require.Len(t, orphan, 2)
	require.Equal(t, core.TextToolInterrupted, orphan[1].Content)
	require.Equal(t, "c7", orphan[1].ToolCallID)
	ghost := recLoadMessages(t, db, "s-ghost")
	require.Len(t, ghost, 2)
	require.Equal(t, core.TextToolInterrupted, ghost[1].Content)

	log := logs.text()
	require.Contains(t, log, "level=WARN")
	require.Contains(t, log, "run_id=run-ghost", "WARN 点名该 run_id")
	require.Contains(t, log, "session_id=s-orphan", "WARN 点名该会话")
	require.Contains(t, log, "session_id=s-ghost")
}

// ---- 运行资源账（约束 19 的恢复面） -----------------------------------------

// recFixedStart 是账本用例的冻结时刻（时间显式传入——仓储不自取业务时间）。
// recFixedStart is the frozen clock of the ledger cases.
var recFixedStart = time.Date(2026, 10, 16, 8, 30, 0, 123*int(time.Millisecond), time.UTC)

// recStartRow 造一份起始行夹具（渠道参数化——trigger 断言要用四个渠道各来一次）。
// PG 的 runs 归属四列 NOT NULL 且带 CHECK，这里补内置管理员 Principal。
// recStartRow builds a starting-row fixture carrying the built-in admin principal.
func recStartRow(channel string) core.RunRecord {
	return core.RunRecord{
		RunID:      channel + "-1776000000123-abc123",
		SessionID:  "cli-3f9a1c07b2d84e55",
		Agent:      "demo",
		Channel:    channel,
		UserID:     "alice",
		Principal:  core.LocalAdminPrincipal(),
		ConfigHash: "3f9a1c07b2d84e55",
		StartedAt:  recFixedStart,
	}
}

// recRunRow 是 runs 一行的判卷形态（列全部显式——可空列用 Null 形态，判 NULL 与判空串不同）。
// recRunRow is the assertion shape of one runs row; nullable columns stay nullable.
type recRunRow struct {
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

// recReadRunRow 按 run_id 读回账本行（判卷直接走 `db.pool`——生产写入路径仍唯一经仓储）。
// recReadRunRow reads one ledger row back by run_id through the pool.
func recReadRunRow(t *testing.T, db *DB, runID string) recRunRow {
	t.Helper()
	var row recRunRow
	err := db.pool.QueryRowContext(context.Background(),
		`SELECT run_id, session_id, agent, channel, user_id, trigger, status,
		        reason, iterations, tool_calls, config_hash, started_at, finished_at
		   FROM runs WHERE run_id = $1`, runID).
		Scan(&row.RunID, &row.SessionID, &row.Agent, &row.Channel, &row.UserID, &row.Trigger,
			&row.Status, &row.Reason, &row.Iterations, &row.ToolCalls, &row.ConfigHash,
			&row.StartedAt, &row.FinishedAt)
	require.NoError(t, err)
	return row
}

// recResourceUIDFor 按 `(kind, name)` 派生一个稳定的测试 UID：同名同种永远是同一实例，
// 不同名称 / 不同种类是不同实例——测试要判的正是"身份跟着实例走、不跟着名称走"。
// recResourceUIDFor derives a stable test uid from kind and name.
func recResourceUIDFor(kind core.ResourceKind, name string) core.ResourceUID {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + name))
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	payload := make([]byte, 26)
	for i := range payload {
		payload[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return core.ResourceUID(core.ResourceUIDPrefix + string(payload))
}

// recModelVersion 造一条模型连接版本；版本号 0 表示让仓储自动取号。
// recModelVersion builds a model_connection version; zero means auto-numbering.
func recModelVersion(resourceID, contentHash, operationID string, versionNo int) core.ResourceVersion {
	return core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    recResourceUIDFor(core.ResourceModelConnection, resourceID),
		ResourceID:     resourceID,
		VersionNo:      versionNo,
		ContentHash:    contentHash,
		DefinitionYAML: "name: " + resourceID + "\nmodel: gpt-test\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    operationID,
	}
}

// recRunManifestFixture 是资源账用例的夹具：一 Provider、两 MCP、一技能，版本都已落库。
// 清单由准入构造器产出（带哈希），与真实准入路径同形。
// recRunManifestFixture seeds one provider, two MCP servers and one skill version, then
// builds the admission manifest the same way the real admission path does.
type recRunManifestFixture struct {
	manifest core.RunResourceManifest
	resolved core.ResolvedResources
	versions []core.ResourceVersion
}

// recSeedRunManifest 落库四条资源版本并返回可准入的清单（生命周期 active）。
// 版本行的外键要求管理操作先存在——真实路径由基线 / US-12 的 BeginOperation 提供。
// recSeedRunManifest inserts the version rows (their FK requires an operation row first)
// and returns a manifest that admission would accept.
func recSeedRunManifest(t *testing.T, db *DB) recRunManifestFixture {
	t.Helper()
	ctx := context.Background()
	seedOperation(t, db, "op-run-manifest")
	store := NewResourceStore(db)

	provider, _, err := store.EnsureVersion(ctx,
		recModelVersion("openai", "sha256:provider", "op-run-manifest", 0))
	require.NoError(t, err)
	browser, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceMCPServer,
		ResourceUID:    recResourceUIDFor(core.ResourceMCPServer, "browser"),
		ResourceID:     "browser",
		ContentHash:    "sha256:mcp-browser",
		DefinitionYAML: "name: browser\ncommand: npx\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    "op-run-manifest",
	})
	require.NoError(t, err)
	filesystem, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceMCPServer,
		ResourceUID:    recResourceUIDFor(core.ResourceMCPServer, "filesystem"),
		ResourceID:     "filesystem",
		ContentHash:    "sha256:mcp-filesystem",
		DefinitionYAML: "name: filesystem\ncommand: npx\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    "op-run-manifest",
	})
	require.NoError(t, err)
	skill, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceUID:  recResourceUIDFor(core.ResourceSkill, "summarize"),
		ResourceID:   "summarize",
		ContentHash:  "sha256:skill-summarize",
		ManifestJSON: `[{"path":"SKILL.md","sha256":"sha256:skill-file","size":12}]`,
		StoragePath:  ".basalt/versions/skills/summarize/1",
		CreatedAt:    opNow.UnixMilli(),
		OperationID:  "op-run-manifest",
	})
	require.NoError(t, err)

	// 引用行的 UID 取自版本行自身：清单固定的身份与落库的版本必须是同一个实例。
	// The reference takes the version's own uid so the manifest and the row agree.
	refOf := func(v core.ResourceVersion) core.ResourceVersionRef {
		return core.ResourceVersionRef{
			Name:        v.ResourceID,
			ResourceUID: v.ResourceUID,
			VersionID:   v.ID,
			VersionNo:   v.VersionNo,
			ContentHash: v.ContentHash,
		}
	}
	// MCP 故意乱序给出：清单必须在构造时规范化，引用行落库顺序不得依赖调用方顺序。
	resolved := core.ResolvedResources{
		Provider:   refOf(provider),
		MCPServers: []core.ResourceVersionRef{refOf(filesystem), refOf(browser)},
		Skills: []core.SkillVersionRef{{
			ResourceVersionRef: refOf(skill),
			Files: []core.FileHash{{
				Path:   "SKILL.md",
				SHA256: "sha256:skill-file",
				Size:   12,
			}},
		}},
		ProviderModel:      "gpt-test",
		ProviderCredential: core.CredentialPin{Mode: core.CredentialModeEnv, TargetBinding: testTargetBinding},
		Notify:             core.NotifyManifest{Mode: core.NotifyModeAllEligible, Channels: []core.NotifyChannelRef{}},
	}
	manifest, err := core.BuildRunResourceManifest(testAgentUID, core.AgentActive, resolved)
	require.NoError(t, err)
	return recRunManifestFixture{
		manifest: manifest,
		resolved: resolved,
		versions: []core.ResourceVersion{provider, browser, filesystem, skill},
	}
}

// recLedgerManifest 在资源账夹具上再冻结一个托管凭据与两个通知渠道：正常收尾
// `FinishRun` 释放三张引用表（资源 / 凭据 / 通知），恢复必须逐表同口径——只释放资源行
// 会让已失败的 Run 永远点名凭据与渠道。
// recLedgerManifest adds a managed credential pin and two notify channels on top of
// the resource fixture so all three reference tables hold unreleased rows.
func recLedgerManifest(t *testing.T, fx recRunManifestFixture) core.RunResourceManifest {
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

// recRunRefRow 是 run_resource_refs 一行的判卷形态（released_at 用 Null 形态判"未释放"）。
// recRunRefRow is the assertion shape of one reference row.
type recRunRefRow struct {
	Kind         string
	ResourceID   string
	ResourceUID  sql.NullString
	ResourceName sql.NullString
	VersionID    int64
	ContentHash  string
	ReleasedAt   sql.NullInt64
}

// recReadRunRefRows 按 run_id 读回引用行，按 (kind, id) 排序以得到稳定断言序。
// recReadRunRefRows reads the reference rows in a stable (kind, id) order.
func recReadRunRefRows(t *testing.T, db *DB, runID string) []recRunRefRow {
	t.Helper()
	rows, err := db.pool.QueryContext(context.Background(),
		`SELECT resource_kind, resource_id, resource_uid, resource_name,
		        version_id, content_hash, released_at
		   FROM run_resource_refs WHERE run_id = $1
		  ORDER BY resource_kind, resource_id`, runID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []recRunRefRow
	for rows.Next() {
		var row recRunRefRow
		require.NoError(t, rows.Scan(&row.Kind, &row.ResourceID, &row.ResourceUID,
			&row.ResourceName, &row.VersionID, &row.ContentHash, &row.ReleasedAt))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// recRequireRunLedgerReleased 判定三张引用表全部已释放或全部仍未释放——恢复的事务把
// 资源引用、凭据固定与渠道冻结当一个整体，任何一张表单独落单都是中间态。
// recRequireRunLedgerReleased checks the three reference tables share one terminal state.
func recRequireRunLedgerReleased(t *testing.T, db *DB, runID string, wantReleased bool) {
	t.Helper()
	refs := recReadRunRefRows(t, db, runID)
	require.Len(t, refs, 4)
	for _, ref := range refs {
		require.Equal(t, wantReleased, ref.ReleasedAt.Valid,
			"运行资源引用的释放状态不符（%s/%s）", ref.Kind, ref.ResourceID)
		if wantReleased {
			require.Equal(t, recFixedNow.UnixMilli(), ref.ReleasedAt.Int64)
		}
	}

	pins, err := NewRunCredentialStore(db).Pins(context.Background(), runID)
	require.NoError(t, err)
	require.Len(t, pins, 1, "托管凭据固定必须随准入写入")
	require.Equal(t, wantReleased, pins[0].ReleasedAt != nil,
		"凭据固定的释放状态必须与资源引用一致")
	if wantReleased {
		require.Equal(t, recFixedNow.UnixMilli(), *pins[0].ReleasedAt)
	}

	channels, err := NewNotifyDeliveryStore(db).Refs(context.Background(), runID)
	require.NoError(t, err)
	require.Len(t, channels, 2, "渠道冻结必须随准入写入")
	for _, channel := range channels {
		require.Equal(t, wantReleased, channel.ReleasedAt != nil,
			"渠道冻结的释放状态必须与资源引用一致（%s）", channel.ChannelName)
		if wantReleased {
			require.Equal(t, recFixedNow.UnixMilli(), *channel.ReleasedAt)
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
		db := portTestDB(t)
		manifest := recLedgerManifest(t, recSeedRunManifest(t, db))
		rec := recStartRow("web")
		require.NoError(t, NewRunStore(db).StartRun(ctx, rec, manifest))

		report, err := Recover(ctx, db.pool, recTestLogger(t), recFixedNow)
		require.NoError(t, err)
		require.Equal(t, 1, report.ClosedRuns)

		row := recReadRunRow(t, db, rec.RunID)
		require.Equal(t, string(core.StatusFailed), row.Status)
		require.Equal(t, recoveryCloseReason, row.Reason.String)
		require.Equal(t, recFixedNow.UnixMilli(), row.FinishedAt.Int64)

		recRequireRunLedgerReleased(t, db, rec.RunID, true)
	})

	t.Run("release_failure_keeps_run_running", func(t *testing.T) {
		db := portTestDB(t)
		manifest := recLedgerManifest(t, recSeedRunManifest(t, db))
		rec := recStartRow("web")
		require.NoError(t, NewRunStore(db).StartRun(ctx, rec, manifest))

		recInstallReleaseBlocker(t, db)
		t.Cleanup(func() {
			recDropReleaseBlocker(t, db)
		})

		report, err := Recover(ctx, db.pool, recTestLogger(t), recFixedNow)
		require.Error(t, err, "释放失败必须如实上抛（启动方据此决定要不要开跑）")
		require.Contains(t, err.Error(), rec.RunID)
		require.Zero(t, report.ClosedRuns)

		require.Equal(t, string(core.StatusRunning), recLoadRunStatus(t, db.pool, rec.RunID),
			"释放失败 → 整个收尾事务回滚，Run 保持 running 等下次重扫")
		recRequireRunLedgerReleased(t, db, rec.RunID, false)
	})
}

// recInstallReleaseBlocker 注入"引用释放必然失败"的触发器（对应 SQLite 谱系的
// `CREATE TRIGGER ... RAISE(ABORT)`；PostgreSQL 用 plpgsql 的 RAISE EXCEPTION）。
// recInstallReleaseBlocker makes every run_resource_refs UPDATE fail.
func recInstallReleaseBlocker(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`CREATE FUNCTION rec_recovery_block_release() RETURNS trigger AS $$
		 BEGIN
		     RAISE EXCEPTION '注入的引用释放失败';
		 END;
		 $$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(context.Background(),
		`CREATE TRIGGER recovery_block_release
		 BEFORE UPDATE ON run_resource_refs
		 FOR EACH ROW EXECUTE FUNCTION rec_recovery_block_release()`)
	require.NoError(t, err)
}

// recDropReleaseBlocker 解除注入（对应 SQLite 谱系的 `DROP TRIGGER IF EXISTS`；PG 的
// DROP TRIGGER 还必须点名表，函数另删）。
// recDropReleaseBlocker removes the injected trigger and its function.
func recDropReleaseBlocker(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`DROP TRIGGER IF EXISTS recovery_block_release ON run_resource_refs`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(context.Background(),
		`DROP FUNCTION IF EXISTS rec_recovery_block_release()`)
	require.NoError(t, err)
}

// TestRecovery_BackfillsTerminalRunResidualRefs —— 已终态 Run 的残留引用由启动恢复补齐
// （约束 19 末句）：④ 的扫描面看不到它们，step ⑤ 的幂等语句按"Run 已终态"补齐，
// 且**不改**已判定的状态、原因与终态时刻。
// TestRecovery_BackfillsTerminalRunResidualRefs releases residual references of runs that
// are already terminal, without re-judging their state.
func TestRecovery_BackfillsTerminalRunResidualRefs(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	manifest := recLedgerManifest(t, recSeedRunManifest(t, db))
	rec := recStartRow("web")
	require.NoError(t, NewRunStore(db).StartRun(ctx, rec, manifest))

	// 模拟"终态已写、释放没跑到"的残留：直接改状态列，不走 FinishRun（它会连带释放）。
	settled := recFixedStart.Add(10 * time.Minute)
	_, err := db.pool.ExecContext(ctx,
		`UPDATE runs SET status = $1, reason = $2, finished_at = $3 WHERE run_id = $4`,
		string(core.StatusCompleted), "人工修库留下的终态", settled.UnixMilli(), rec.RunID)
	require.NoError(t, err)

	report, err := Recover(ctx, db.pool, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Zero(t, report.ClosedRuns, "已终态的 Run 不在收尾扫描面里")

	row := recReadRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusCompleted), row.Status, "已判定的终态不得被恢复改口")
	require.Equal(t, "人工修库留下的终态", row.Reason.String)
	require.Equal(t, settled.UnixMilli(), row.FinishedAt.Int64)

	recRequireRunLedgerReleased(t, db, rec.RunID, true)
}

// TestRecovery_DoesNotFabricateResourceLedger —— 恢复收尾历史运行（本能力之前产生的、
// 没有清单的账本）时**不得补造**资源账：清单与引用一行为零，Run 照常收尾。
// 补造的账会被读成事实，比"未记录"更糟（约束 24）。
// TestRecovery_DoesNotFabricateResourceLedger closes legacy runs without inventing a
// manifest or references for them.
func TestRecovery_DoesNotFabricateResourceLedger(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	recSeedRun(t, db.pool, "run-legacy", "running")

	report, err := Recover(ctx, db.pool, recTestLogger(t), recFixedNow)
	require.NoError(t, err)
	require.Equal(t, 1, report.ClosedRuns)
	require.Equal(t, string(core.StatusFailed), recLoadRunStatus(t, db.pool, "run-legacy"))

	var manifests, refs int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_manifests WHERE run_id = $1`, "run-legacy").Scan(&manifests))
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_refs WHERE run_id = $1`, "run-legacy").Scan(&refs))
	require.Zero(t, manifests, "恢复不得为历史运行补造清单")
	require.Zero(t, refs, "恢复不得为历史运行补造引用")
}

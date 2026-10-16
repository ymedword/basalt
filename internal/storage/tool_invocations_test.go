package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// invocationRow 是 tool_invocations 一行的判卷形态（列全部显式——可空列用 Null 形态，
// 判 NULL 与判空串不同）。
// invocationRow is the assertion shape of one tool_invocations row; nullable columns stay
// nullable so NULL and the empty string cannot be confused.
type invocationRow struct {
	ID           int64
	RunID        string
	SessionID    sql.NullString
	ToolCallID   sql.NullString
	ToolName     string
	InputJSON    sql.NullString
	ResultJSON   sql.NullString
	Status       string
	ErrorMessage sql.NullString
	ErrorKind    sql.NullString
	DurationMS   sql.NullInt64
	StartedAt    int64
	FinishedAt   sql.NullInt64
}

// readInvocationRow 按 id 读回审计行（in-package 判卷走 db.read——生产写入路径仍唯一经仓储）。
// readInvocationRow reads one audit row back by id through the read pool.
func readInvocationRow(t *testing.T, db *DB, id int64) invocationRow {
	t.Helper()
	var row invocationRow
	err := db.read.QueryRowContext(context.Background(),
		`SELECT id, run_id, session_id, tool_call_id, tool_name, input_json, result_json,
		        status, error_message, error_kind, duration_ms, started_at, finished_at
		   FROM tool_invocations WHERE id = ?`, id).
		Scan(&row.ID, &row.RunID, &row.SessionID, &row.ToolCallID, &row.ToolName,
			&row.InputJSON, &row.ResultJSON, &row.Status, &row.ErrorMessage, &row.ErrorKind,
			&row.DurationMS, &row.StartedAt, &row.FinishedAt)
	require.NoError(t, err)
	return row
}

// invocationFixture 造一份预写夹具（时刻取 run_store_test 的冻结时刻——时间显式传入，
// 仓储不自取业务时间）。
// invocationFixture builds the pre-write fixture around the shared frozen clock.
func invocationFixture() core.ToolInvocation {
	return core.ToolInvocation{
		RunID:      "cli-1776000000123-abc123",
		SessionID:  "cli-3f9a1c07b2d84e55",
		ToolCallID: "call-1",
		ToolName:   "read_file",
		InputJSON:  json.RawMessage(`{"path":"notes.txt"}`),
		CreatedAt:  fixedStart,
	}
}

// TestToolInvocationRepo_BeginInvocationInsertsPendingRow —— J1（两阶段写第一半）：预写行的
// 列值——`status` 由仓储**固定写** `pending`、`input_json` 原样、耗时 / 结果 / 失败列全 NULL、
// `started_at` 存 Unix 毫秒。
// J1: the pre-written row — pending is written by the repo itself, input_json is verbatim,
// and every not-yet-known column stays NULL.
func TestToolInvocationRepo_BeginInvocationInsertsPendingRow(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewToolInvocationRepo(db)

	inv := invocationFixture()
	id, err := repo.BeginInvocation(ctx, inv)
	require.NoError(t, err)
	require.Positive(t, id)

	row := readInvocationRow(t, db, id)
	require.Equal(t, core.LLMCallPending, core.LLMCallStatus(row.Status), "预写态恒为 pending")
	require.Equal(t, inv.RunID, row.RunID)
	require.Equal(t, inv.SessionID, row.SessionID.String)
	require.Equal(t, inv.ToolCallID, row.ToolCallID.String)
	require.Equal(t, inv.ToolName, row.ToolName)
	require.Equal(t, string(inv.InputJSON), row.InputJSON.String, "入参原样入库（不重编码）")
	require.False(t, row.ResultJSON.Valid, "预写行没有结果")
	require.False(t, row.ErrorMessage.Valid, "预写行没有失败原文")
	require.False(t, row.ErrorKind.Valid, "预写行没有失败类别")
	require.False(t, row.DurationMS.Valid, "耗时未定写 NULL（可空列——与耗时为零可区分）")
	require.Equal(t, fixedStart.UnixMilli(), row.StartedAt, "started_at 存 Unix 毫秒 UTC")
	require.False(t, row.FinishedAt.Valid, "预写行未结束 → NULL")
}

// TestToolInvocationRepo_FinishInvocationUpdatesSameRowOnce —— J2：`pending` → 终态的写入
// **落在同一行**（不新开一行），终态列齐备且 `finished_at = started_at + duration_ms`；
// 二次回填拒绝并点名 id（终态只写一次——审计不得"改口"）。
// J2: the terminal write lands on the very same row exactly once.
func TestToolInvocationRepo_FinishInvocationUpdatesSameRowOnce(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewToolInvocationRepo(db)

	id, err := repo.BeginInvocation(ctx, invocationFixture())
	require.NoError(t, err)

	result := json.RawMessage(`{"content":"笔记正文","retryable":false}`)
	require.NoError(t, repo.FinishInvocation(ctx, id, core.ToolInvocationResult{
		Status:     core.LLMCallSuccess,
		ResultJSON: result,
		Duration:   250 * time.Millisecond,
	}))

	var count int
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM tool_invocations WHERE run_id = ?`, "cli-1776000000123-abc123").Scan(&count))
	require.Equal(t, 1, count, "两个阶段共一行——同账一行（不是每次写各开一行）")

	row := readInvocationRow(t, db, id)
	require.Equal(t, core.LLMCallSuccess, core.LLMCallStatus(row.Status))
	require.Equal(t, string(result), row.ResultJSON.String)
	require.False(t, row.ErrorKind.Valid, "成功行 error_kind 为 NULL")
	require.False(t, row.ErrorMessage.Valid, "成功行 error_message 为 NULL")
	require.EqualValues(t, 250, row.DurationMS.Int64)
	require.Equal(t, row.StartedAt+250, row.FinishedAt.Int64,
		"结束时刻由行内起始时刻 + 调用方耗时推出（仓储不读时钟）")

	err = repo.FinishInvocation(ctx, id, core.ToolInvocationResult{
		Status: core.LLMCallFailed, ErrorMessage: "改判", ErrorKind: core.ErrorKindUnknown,
	})
	require.Error(t, err, "非 pending 行不得二次回填")
	require.Contains(t, err.Error(), "该行不在 pending 态", "错误必须点名原因")

	again := readInvocationRow(t, db, id)
	require.Equal(t, core.LLMCallSuccess, core.LLMCallStatus(again.Status), "先写的终态不被覆盖")
	require.False(t, again.ErrorMessage.Valid)
	require.EqualValues(t, 250, again.DurationMS.Int64)
}

// TestToolInvocationRepo_ErrorKindRoundTripsSevenLiterals —— J3：`error_kind` 的取值集合与
// 契约 §7.3 **逐字相等**（恰七值），且每一值都能原样落库、原样读回。
// J3: the error_kind value set equals the contract's seven literals exactly, and every one
// of them round-trips.
func TestToolInvocationRepo_ErrorKindRoundTripsSevenLiterals(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewToolInvocationRepo(db)

	contract := []string{
		"sandbox_denied", "timeout", "not_found", "invalid_input",
		"external_error", "runtime_error", "unknown",
	}
	kinds := []core.ErrorKind{
		core.ErrorKindSandboxDenied, core.ErrorKindTimeout, core.ErrorKindNotFound,
		core.ErrorKindInvalidInput, core.ErrorKindExternalError, core.ErrorKindRuntimeError,
		core.ErrorKindUnknown,
	}
	require.Len(t, kinds, len(contract), "七类不多不少")
	for i, kind := range kinds {
		require.Equal(t, contract[i], string(kind), "字面量逐字对齐契约 §7.3")
	}

	for _, kind := range kinds {
		inv := invocationFixture()
		inv.ToolCallID = "call-" + string(kind)
		id, err := repo.BeginInvocation(ctx, inv)
		require.NoError(t, err)
		require.NoError(t, repo.FinishInvocation(ctx, id, core.ToolInvocationResult{
			Status: core.LLMCallFailed, ErrorKind: kind, ErrorMessage: "失败原文",
		}))

		row := readInvocationRow(t, db, id)
		require.Equal(t, string(kind), row.ErrorKind.String, "取值 %s 原样落库", kind)
		require.Equal(t, core.LLMCallFailed, core.LLMCallStatus(row.Status))
	}
}

// TestToolInvocationRepo_TruncationMarkerLivesInsideResultJSON —— J4：截断标记写在 `result_json`
// **值内**（`{"content":…,"retryable":…,"truncated":true}`），落库不引用任何不存在的列。
// J4: the truncation marker lives inside the result_json value; no nonexistent column is
// referenced by the write path.
func TestToolInvocationRepo_TruncationMarkerLivesInsideResultJSON(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewToolInvocationRepo(db)

	id, err := repo.BeginInvocation(ctx, invocationFixture())
	require.NoError(t, err)

	marker := json.RawMessage(`{"content":"超长正文已被截断","retryable":false,"truncated":true}`)
	require.NoError(t, repo.FinishInvocation(ctx, id, core.ToolInvocationResult{
		Status: core.LLMCallSuccess, ResultJSON: marker, Duration: time.Millisecond,
	}))

	row := readInvocationRow(t, db, id)
	require.Equal(t, string(marker), row.ResultJSON.String, "结果表示原样落库（含标记）")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(row.ResultJSON.String), &decoded), "值内是合法 JSON")
	require.Equal(t, true, decoded["truncated"], "截断标记在值内可见")
	require.Len(t, decoded, 3, "表示形态三键：content / retryable / truncated")

	columns, err := tableColumns(t, db, "tool_invocations")
	require.NoError(t, err)
	require.NotContains(t, columns, "truncated", "截断标记不得另立列")
}

// tableColumns 读回一张表的列清单（PRAGMA table_info；列清单断言与恢复测试共用）。
// tableColumns returns the column list of one table via PRAGMA table_info.
func tableColumns(t *testing.T, db *DB, table string) ([]string, error) {
	t.Helper()
	rows, err := db.read.QueryContext(context.Background(), `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, err
	}
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
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// TestToolInvocationRepo_ErrorMessageTruncatedAt4KB —— J5（契约 §7.4）：失败原文截断到
// **字节 4 KB**，按 rune 边界回退（不切断多字节字符），保留头部、不抹成空串。
// J5: the failure message is capped at 4 KiB on a rune boundary, keeping its head intact.
func TestToolInvocationRepo_ErrorMessageTruncatedAt4KB(t *testing.T) {
	ctx := context.Background()
	repo := NewToolInvocationRepo(openTempDB(t))

	id, err := repo.BeginInvocation(ctx, invocationFixture())
	require.NoError(t, err)

	long := strings.Repeat("超长错误", 4000) // 远超 4 KB，且是多字节字符
	require.NoError(t, repo.FinishInvocation(ctx, id, core.ToolInvocationResult{
		Status: core.LLMCallFailed, ErrorMessage: long, ErrorKind: core.ErrorKindRuntimeError,
	}))

	var got string
	require.NoError(t, repo.db.read.QueryRowContext(ctx,
		`SELECT error_message FROM tool_invocations WHERE id = ?`, id).Scan(&got))
	require.LessOrEqual(t, len(got), 4*1024, "截断上界 = 4 KB")
	require.True(t, utf8.ValidString(got), "截断不得切断 UTF-8 字符")
	require.NotEqual(t, long, got, "超长原文必须被截断")
	require.True(t, strings.HasPrefix(long, got), "截断保留头部（不是掐头去尾）")
	require.NotEmpty(t, got, "截断不得把文案抹成空串")
}

// TestToolInvocationRepo_InputJSONRoundTripsVerbatim —— J6：入参**原样**——键序、空白、
// 多字节字符与转义序列逐字节保真，落库若重编码，转义会先被解成真字符、空白与键序会被规整。
// J6: input_json round-trips byte for byte — a re-encode would decode the escape sequence
// and normalize the spacing.
func TestToolInvocationRepo_InputJSONRoundTripsVerbatim(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewToolInvocationRepo(db)

	// 第一路：标准库编码器的产出（`<` 被转义成转义序列——先钉住这个读数前提）。
	// 落库路径若做 unmarshal → marshal 往返，转义会先被解成真字符，存下的字节随之改变。
	escaped, err := json.Marshal(map[string]string{"path": "notes/<draft>.md", "note": "中"})
	require.NoError(t, err)
	require.NotContains(t, string(escaped), `<`, "读数前提：编码器把尖括号转义了（未产出裸字符）")

	inv := invocationFixture()
	inv.InputJSON = json.RawMessage(escaped)
	id, err := repo.BeginInvocation(ctx, inv)
	require.NoError(t, err)

	row := readInvocationRow(t, db, id)
	require.Equal(t, string(escaped), row.InputJSON.String, "入参逐字节原样")
	require.NotContains(t, row.InputJSON.String, `<`,
		"转义形态原样留存——出现裸字符即说明落库路径重编码过")

	// 第二路：手工形态的空白与键序同样不被规整。
	irregular := `{"b": 1,   "a":"中","list":[1,2,3]}`
	inv2 := invocationFixture()
	inv2.ToolCallID = "call-2"
	inv2.InputJSON = json.RawMessage(irregular)
	id2, err := repo.BeginInvocation(ctx, inv2)
	require.NoError(t, err)

	row2 := readInvocationRow(t, db, id2)
	require.Equal(t, irregular, row2.InputJSON.String, "空白与键序都不被规整")
	require.NoError(t, json.Unmarshal([]byte(row2.InputJSON.String), &map[string]any{}),
		"原样保存不改变它是合法 JSON 的事实")
}

// TestToolInvocationRepo_FinishFailureKeepsRowPending —— J7：回填失败**保留 pending**
// （两条路径：目标 id 不存在 → 影响 0 行即拒绝；进程消失 → 库内持久化的就是 pending 行）。
// J7: a failed finish keeps the row pending — both when the target row is missing and when
// the process itself disappears.
func TestToolInvocationRepo_FinishFailureKeepsRowPending(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewToolInvocationRepo(db)

	id, err := repo.BeginInvocation(ctx, invocationFixture())
	require.NoError(t, err)

	err = repo.FinishInvocation(ctx, id+99, core.ToolInvocationResult{Status: core.LLMCallSuccess})
	require.Error(t, err, "目标行不存在即拒绝（不得静默成功）")
	require.Contains(t, err.Error(), "不存在", "错误必须点名原因")

	row := readInvocationRow(t, db, id)
	require.Equal(t, core.LLMCallPending, core.LLMCallStatus(row.Status), "真实行仍在 pending")
	require.False(t, row.DurationMS.Valid, "不得伪造耗时")
	require.False(t, row.ResultJSON.Valid, "不得伪造结果")

	// 进程消失路径：单独开一份库，写 pending 后直接关闭（崩溃窗口的持久化形态）。
	path := filepath.Join(t.TempDir(), "basalt.db")
	crashing, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	crashID, err := NewToolInvocationRepo(crashing).BeginInvocation(ctx, invocationFixture())
	require.NoError(t, err)
	require.NoError(t, crashing.Close())

	reopened, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })

	crashRow := readInvocationRow(t, reopened, crashID)
	require.Equal(t, core.LLMCallPending, core.LLMCallStatus(crashRow.Status),
		"崩溃留下的是 pending 行（恢复扫描归一化为 unknown 归 US-4 的恢复路径）")
	require.False(t, crashRow.FinishedAt.Valid, "无半写：结束时刻不落")
}

// TestToolInvocationRepo_TableHasNoRoundColumn —— J8（§4.3 列级口径判据）：`tool_invocations`
// 的列清单与契约逐字一致，且**不含** `round`——轮次是模型调用链的账，工具链记不了它
// （ToolExecutor 拿不到轮号，同轮多次调用也无法区分）。
// J8: the column list matches the contract verbatim and carries neither a round nor a
// truncation column.
func TestToolInvocationRepo_TableHasNoRoundColumn(t *testing.T) {
	db := openTempDB(t)

	columns, err := tableColumns(t, db, "tool_invocations")
	require.NoError(t, err)
	require.Equal(t, []string{
		"id", "run_id", "session_id", "tool_call_id", "tool_name", "input_json",
		"result_json", "status", "error_message", "error_kind", "duration_ms",
		"started_at", "finished_at",
	}, columns, "列清单以 docs/contracts/schema.sql 为准（逐字、含顺序）")
	require.NotContains(t, columns, "round", "工具表不得有轮次列")
	require.NotContains(t, columns, "truncated", "截断标记在 result_json 值内，不另立列")
}

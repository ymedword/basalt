package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件判卷会话历史仓储（`session_store.go`）的八条：`seq` 库内自增 / `run_id` 显式入列 /
// 跨重启读回 / 工具调用身份字段往返 / 供应商扩展字段（UseNumber）/ 会话行 upsert /
// 列表排序 / 无状态调用零写入。
// 判据落点：既看**仓储的返回面**（core.Message / core.SessionRecord），也看**列面**
// （in-package 可直读 `db.read`）——只看返回面，NULL 与空串、计数列漂移这类问题会漏判。
// Two-sided assertions: the repository's return values and the raw columns behind them.

// storeSession 是夹具会话标识（形态取自颗粒度文档 §6 固定口径 1 的示例：`<渠道>-<hex16>`）。
// The fixture session id, shaped like the documented derivation example.
const storeSession = "cli-3f9a1c07b2d84e55"

// storeRun 是夹具运行标识（形态取自契约 §5：`<渠道>-<毫秒>-<6 位 base36>`）。
// The fixture run id, shaped like the contract's run id form.
const storeRun = "cli-1776000000123-abc123"

// fixedMessageTime 是本文件的冻结时刻（仓储只在冻结签名没有时间位时自取时钟，
// 其余时间一律显式传入）。
// The frozen clock of these cases; compare with the message-store signature, which
// carries no time slot at all.
var fixedMessageTime = time.Date(2026, 10, 16, 11, 0, 0, 0, time.UTC)

// messageRow 是 messages 一行的判卷形态（列全部显式——可空列用 Null 形态，判 NULL 与判空串不同）。
// messageRow is the assertion shape of one messages row; nullable columns stay nullable.
type messageRow struct {
	Seq        int64
	RunID      sql.NullString
	Role       string
	Content    sql.NullString
	ToolCalls  sql.NullString
	ToolCallID sql.NullString
	Reasoning  sql.NullString
	Extra      sql.NullString
	CreatedAt  int64
}

// readMessageRows 按 `seq` 升序读回一个会话的全部消息行（in-package 判卷走 db.read——
// 生产读取路径仍唯一经仓储）。
// readMessageRows reads one session's raw rows back through the read pool.
func readMessageRows(t *testing.T, db *DB, sessionID string) []messageRow {
	t.Helper()
	rows, err := db.read.QueryContext(context.Background(),
		`SELECT seq, run_id, role, content, tool_calls, tool_call_id, reasoning_content,
		        provider_extra, created_at
		   FROM messages WHERE session_id = ? ORDER BY seq`, sessionID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	var out []messageRow
	for rows.Next() {
		var row messageRow
		require.NoError(t, rows.Scan(&row.Seq, &row.RunID, &row.Role, &row.Content, &row.ToolCalls,
			&row.ToolCallID, &row.Reasoning, &row.Extra, &row.CreatedAt))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// sessionRow 是 sessions 一行的判卷形态。
// sessionRow is the assertion shape of one sessions row.
type sessionRow struct {
	ID           string
	ProfileName  string
	Channel      string
	UserID       string
	Status       string
	MessageCount int
	CreatedAt    int64
	LastActiveAt int64
	ArchivedAt   sql.NullInt64
}

// readSessionRow 按标识读回会话行。
// readSessionRow reads one session row back by id.
func readSessionRow(t *testing.T, db *DB, sessionID string) sessionRow {
	t.Helper()
	var row sessionRow
	err := db.read.QueryRowContext(context.Background(),
		`SELECT session_id, profile_name, channel, user_id, status, message_count,
		        created_at, last_active_at, archived_at
		   FROM sessions WHERE session_id = ?`, sessionID).
		Scan(&row.ID, &row.ProfileName, &row.Channel, &row.UserID, &row.Status,
			&row.MessageCount, &row.CreatedAt, &row.LastActiveAt, &row.ArchivedAt)
	require.NoError(t, err)
	return row
}

// countMessageRows 数一个会话的消息行数（无状态用例的"零写入"判据）。
// countMessageRows counts the message rows of one session.
func countMessageRows(t *testing.T, db *DB, sessionID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.read.QueryRowContext(context.Background(),
		`SELECT count(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n))
	return n
}

// countSessionRows 数会话行总数。
// countSessionRows counts every session row.
func countSessionRows(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.read.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sessions`).Scan(&n))
	return n
}

// seedStoreSession 建一条夹具会话行：`messages.session_id` 有外键而连接级 `foreign_keys(1)`
// 生效，先行写消息会撞外键；且 `message_count` 是冗余计数列（建行时由本行清零）。
// seedStoreSession inserts the fixture session row first: the message foreign key is
// enforced per connection, and the redundant counter starts from this row.
func seedStoreSession(t *testing.T, store *SessionStore, sessionID string, at time.Time) {
	t.Helper()
	require.NoError(t, store.UpsertSession(context.Background(), core.SessionRecord{
		ID:           sessionID,
		ProfileName:  "demo",
		Channel:      "cli",
		UserID:       "alice",
		CreatedAt:    at,
		LastActiveAt: at,
	}))
}

// TestSessionStore_SeqIncrementsMonotonically —— N1：`seq` 由**库内推算**（1 起、逐条 +1），
// 调用方给不出重复号；同一实例的后续写入接着编号（不回到 1、不跳号）。
// TestSessionStore_SeqIncrementsMonotonically pins the in-database seq allocation.
func TestSessionStore_SeqIncrementsMonotonically(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSessionStore(db)
	seedStoreSession(t, store, storeSession, fixedMessageTime)

	roles := []string{core.RoleUser, core.RoleAssistant, core.RoleTool}
	for i, role := range roles {
		require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun,
			core.Message{Role: role, Content: fmt.Sprintf("第 %d 条", i+1)}))
	}
	rows := readMessageRows(t, db, storeSession)
	require.Len(t, rows, 3)
	for i, row := range rows {
		require.Equal(t, int64(i+1), row.Seq, "seq 从 1 起、逐条 +1")
	}

	// 第二批：编号接着走（`UNIQUE(session_id, seq)` 是这条纪律的判卷人）。
	require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun,
		core.Message{Role: core.RoleUser, Content: "第 4 条"}))
	rows = readMessageRows(t, db, storeSession)
	require.Len(t, rows, 4)
	require.Equal(t, int64(4), rows[3].Seq, "后续写入接着编号")
	for i := 1; i < len(rows); i++ {
		require.Greater(t, rows[i].Seq, rows[i-1].Seq, "seq 严格单调")
	}

	// created_at 由仓储打上写入时刻（冻结签名里没有时间位——见 session_store.go 的说明）。
	// 判"近似当前时刻"而不是判等：这一列本来就该是那一刻。
	require.WithinDuration(t, time.Now(), time.UnixMilli(rows[0].CreatedAt).UTC(), time.Minute)
}

// TestSessionStore_RunIDLandsInColumn —— N2：`run_id` **显式入列**（崩溃恢复按它定位半截组）；
// 空 runID（system 注入类消息）写 NULL，不是空串。
// TestSessionStore_RunIDLandsInColumn pins the explicit run_id column.
func TestSessionStore_RunIDLandsInColumn(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSessionStore(db)
	seedStoreSession(t, store, storeSession, fixedMessageTime)

	require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun,
		core.Message{Role: core.RoleUser, Content: "读一下 notes.txt"}))
	require.NoError(t, store.AppendMessage(ctx, storeSession, "",
		core.Message{Role: core.RoleAssistant, Content: "不属任何 Run 的一条"}))
	require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun,
		core.Message{Role: core.RoleTool, Content: "文件内容", ToolCallID: "c1"}))

	rows := readMessageRows(t, db, storeSession)
	require.Len(t, rows, 3)
	require.True(t, rows[0].RunID.Valid)
	require.Equal(t, storeRun, rows[0].RunID.String, "传进来的 runID 落进列里")
	require.False(t, rows[1].RunID.Valid, "空 runID → NULL（空串与没值是两回事）")
	require.Equal(t, storeRun, rows[2].RunID.String, "工具结果消息同样带 run_id")
}

// TestSessionStore_ReadsBackAcrossRestart —— N3：关掉库、以**只读句柄**重开（模拟重启后的
// 读取面）——历史在库里而不在进程内存里，且读路径不需要写池。
// TestSessionStore_ReadsBackAcrossRestart proves history survives a reopen through a
// read-only handle, i.e. the read path needs no write pool.
func TestSessionStore_ReadsBackAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")

	written := []core.Message{
		{Role: core.RoleUser, Content: "读一下 notes.txt 并总结"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{
			{ID: "c1", Name: "read_file", Arguments: `{"path":"notes.txt"}`},
		}},
		{Role: core.RoleTool, Content: "笔记正文", ToolCallID: "c1"},
	}

	first, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	store := NewSessionStore(first)
	seedStoreSession(t, store, storeSession, fixedMessageTime)
	for _, m := range written {
		require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun, m))
	}
	require.NoError(t, first.Close())

	// 重启：新句柄、新仓储实例（**只读**——历史读取不该依赖写权）。
	second, err := OpenReadOnly(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	defer func() { require.NoError(t, second.Close()) }()

	loaded, err := NewSessionStore(second).Messages(ctx, storeSession)
	require.NoError(t, err)
	require.Equal(t, written, loaded, "跨重启读回逐字段一致（含工具调用与 tool_call_id）")

	rows := readMessageRows(t, second, storeSession)
	require.Len(t, rows, 3)
	require.Equal(t, int64(1), rows[0].Seq, "seq 落库、重开后仍是同一段历史")
}

// TestSessionStore_ToolRowsKeepCallIDAndReasoning —— N4：工具调用链上的身份字段逐字往返——
// assistant 行的 `tool_calls`（id / name / arguments）、tool 行的 `tool_call_id`、
// assistant 行的 `reasoning_content`（DeepSeek 思考模式必须回传，丢了即供应商 400）。
// TestSessionStore_ToolRowsKeepCallIDAndReasoning keeps the call identity fields verbatim.
func TestSessionStore_ToolRowsKeepCallIDAndReasoning(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSessionStore(db)
	seedStoreSession(t, store, storeSession, fixedMessageTime)

	assistant := core.Message{
		Role:             core.RoleAssistant,
		ReasoningContent: "先看看工作区里有什么",
		ToolCalls: []core.ToolCall{
			{ID: "c1", Name: "read_file", Arguments: `{"path":"notes.txt"}`},
			{ID: "c2", Name: "list_dir", Arguments: `{"path":"."}`},
		},
	}
	tool := core.Message{Role: core.RoleTool, Content: "笔记正文", ToolCallID: "c1"}
	require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun, assistant))
	require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun, tool))

	loaded, err := store.Messages(ctx, storeSession)
	require.NoError(t, err)
	require.Equal(t, []core.Message{assistant, tool}, loaded, "身份字段逐字往返")

	rows := readMessageRows(t, db, storeSession)
	require.False(t, rows[0].Content.Valid, "带工具调用的 assistant 行 content 为空 → NULL")
	require.JSONEq(t,
		`[{"id":"c1","name":"read_file","arguments":"{\"path\":\"notes.txt\"}"},`+
			`{"id":"c2","name":"list_dir","arguments":"{\"path\":\".\"}"}]`,
		rows[0].ToolCalls.String, "tool_calls 列是 DDL 注释里的 [{id,name,arguments}] 形态")
	require.Equal(t, "先看看工作区里有什么", rows[0].Reasoning.String)
	require.False(t, rows[0].ToolCallID.Valid, "assistant 行没有 tool_call_id → NULL")
	require.Equal(t, "c1", rows[1].ToolCallID.String, "tool 行回填的是同一次调用的 id")
	require.False(t, rows[1].ToolCalls.Valid, "tool 行没有 tool_calls → NULL")
}

// TestSessionStore_ProviderExtraRoundTripsWithUseNumber —— N5：供应商扩展字段往返保真——
// `provider_extra` 必须按 `UseNumber` 解码：默认解码把 JSON 数字变成 float64，
// 2^53 以上的整数会在往返中被**悄悄改值**（与 provider 侧同一条判据）。
// TestSessionStore_ProviderExtraRoundTripsWithUseNumber pins number fidelity: the fixture
// integer is exactly the one a float64 decode would round.
func TestSessionStore_ProviderExtraRoundTripsWithUseNumber(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSessionStore(db)
	seedStoreSession(t, store, storeSession, fixedMessageTime)

	// 夹具经**与生产同一路径**产生：先用 UseNumber 解一段协议响应里的未知键。
	const protocolExtra = `{"seed": 10000000000000001, "nested": {"n": 2}, "note": "x"}`
	dec := json.NewDecoder(strings.NewReader(protocolExtra))
	dec.UseNumber()
	var extra map[string]any
	require.NoError(t, dec.Decode(&extra))

	require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun, core.Message{
		Role:          core.RoleAssistant,
		Content:       "带扩展字段的一条",
		ProviderExtra: extra,
	}))

	loaded, err := store.Messages(ctx, storeSession)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	got := loaded[0].ProviderExtra
	require.Equal(t, json.Number("10000000000000001"), got["seed"],
		"大整数原样取回（json.Number 而不是 float64）")
	require.IsType(t, json.Number(""), got["seed"], "解码必须走 UseNumber")
	require.Equal(t, "x", got["note"])
	require.Equal(t, map[string]any{"n": json.Number("2")}, got["nested"], "嵌套对象同样保真")

	rows := readMessageRows(t, db, storeSession)
	require.Contains(t, rows[0].Extra.String, "10000000000000001", "列里写的是原数字，不是科学计数法")

	// 反证：默认解码会把同一个值读成 float64 并失真——本用例因此能判出"退回默认解码"。
	var loose map[string]any
	require.NoError(t, json.Unmarshal([]byte(rows[0].Extra.String), &loose))
	require.IsType(t, float64(0), loose["seed"])
	require.NotEqual(t, json.Number("10000000000000001"), loose["seed"])
}

// TestSessionStore_UpsertSessionInsertsThenRefreshes —— N6：会话行 upsert——不存在即建
// （状态取 DDL 缺省 `active`、计数从传入值起）；已存在**只刷新最后活动时间**，
// `created_at` / `profile_name` / `message_count` / `status` 都不得被一次刷新改掉。
// TestSessionStore_UpsertSessionInsertsThenRefreshes pins both halves of the upsert.
func TestSessionStore_UpsertSessionInsertsThenRefreshes(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSessionStore(db)

	rec := core.SessionRecord{
		ID:           storeSession,
		ProfileName:  "demo",
		Channel:      "cli",
		UserID:       "alice",
		CreatedAt:    fixedMessageTime,
		LastActiveAt: fixedMessageTime,
	}
	require.NoError(t, store.UpsertSession(ctx, rec))

	// 第一次 upsert = 建行：列值逐项落定，状态取 DDL 缺省。
	row := readSessionRow(t, db, storeSession)
	require.Equal(t, "active", row.Status, "status 不写 → DDL 缺省 active")
	require.Equal(t, "demo", row.ProfileName)
	require.Equal(t, "cli", row.Channel)
	require.Equal(t, "alice", row.UserID)
	require.Zero(t, row.MessageCount)
	require.Equal(t, fixedMessageTime.UnixMilli(), row.CreatedAt)
	require.Equal(t, fixedMessageTime.UnixMilli(), row.LastActiveAt)
	require.False(t, row.ArchivedAt.Valid)

	// 写入两条消息：冗余计数列跟着长（列表页据此免 count(*)）。
	require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun,
		core.Message{Role: core.RoleUser, Content: "第一条"}))
	require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun,
		core.Message{Role: core.RoleAssistant, Content: "第二条"}))
	require.Equal(t, 2, readSessionRow(t, db, storeSession).MessageCount)

	// 第二次 upsert = 刷新：调用方并不知道计数（记录里是零值），覆盖会把计数清零。
	later := fixedMessageTime.Add(9 * time.Minute)
	require.NoError(t, store.UpsertSession(ctx, core.SessionRecord{
		ID:           storeSession,
		ProfileName:  "换了个名字",
		Channel:      "web",
		UserID:       "bob",
		CreatedAt:    later,
		LastActiveAt: later,
	}))

	refreshed := readSessionRow(t, db, storeSession)
	require.Equal(t, later.UnixMilli(), refreshed.LastActiveAt, "刷新的是最后活动时间")
	require.Equal(t, fixedMessageTime.UnixMilli(), refreshed.CreatedAt, "首次建行时刻不被改写")
	require.Equal(t, 2, refreshed.MessageCount, "计数列不得被一次刷新清零")
	require.Equal(t, "demo", refreshed.ProfileName, "派生标识决定了画像，刷新不改它")
	require.Equal(t, "cli", refreshed.Channel)
	require.Equal(t, "alice", refreshed.UserID)
	require.Equal(t, "active", refreshed.Status, "刷新不得复活归档态（status 刻意不写）")
}

// TestSessionStore_ListSessionsOrdersByLastActive —— N7：列表口径固定在仓储——最后活动时间
// 倒序（最新在前），时间相同按标识兜底（两次同样的读取必须给出同一种次序）；
// 行数从冗余计数列读回，空库返回空列表且不是错误。
// TestSessionStore_ListSessionsOrdersByLastActive pins the list ordering and the counter.
func TestSessionStore_ListSessionsOrdersByLastActive(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSessionStore(db)

	sessions, err := store.ListSessions(ctx)
	require.NoError(t, err, "空库不是错误")
	require.Empty(t, sessions)

	// 三个会话：s-old 最久、s-mid 与 s-new 同一时刻（判兜底次序）、s-new 计数为 2。
	seedStoreSession(t, store, "cli-oldest0000000000", fixedMessageTime)
	seedStoreSession(t, store, "cli-mid0000000000000", fixedMessageTime.Add(time.Hour))
	seedStoreSession(t, store, "cli-newest0000000000", fixedMessageTime.Add(time.Hour))
	require.NoError(t, store.AppendMessage(ctx, "cli-newest0000000000", storeRun,
		core.Message{Role: core.RoleUser, Content: "一"}))
	require.NoError(t, store.AppendMessage(ctx, "cli-newest0000000000", storeRun,
		core.Message{Role: core.RoleAssistant, Content: "二"}))

	sessions, err = store.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 3)
	require.Equal(t,
		[]string{"cli-mid0000000000000", "cli-newest0000000000", "cli-oldest0000000000"},
		[]string{sessions[0].ID, sessions[1].ID, sessions[2].ID},
		"最后活动时间倒序；同一时刻按标识升序兜底")
	require.Equal(t, 2, sessions[1].MessageCount, "计数列随消息写入递增")
	require.Zero(t, sessions[0].MessageCount)
	require.Equal(t, fixedMessageTime.Add(time.Hour), sessions[0].LastActiveAt, "活动时刻按毫秒读回")
	require.Equal(t, fixedMessageTime, sessions[2].CreatedAt)
}

// TestSessionStore_StatelessCallWritesNoMessages —— N8：无状态调用（空会话标识）零写入——
// 消息写入**拒绝**而不是静默 no-op（静默会让调用方以为落了库，正是"内存里有、库里没有"的
// 历史分叉）；读取给空历史；会话行同样拒绝。
// TestSessionStore_StatelessCallWritesNoMessages refuses stateless writes instead of
// silently dropping them, and lets the read side answer "no history".
func TestSessionStore_StatelessCallWritesNoMessages(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewSessionStore(db)

	err := store.AppendMessage(ctx, "", storeRun, core.Message{Role: core.RoleUser, Content: "无状态"})
	require.Error(t, err, "空会话标识的消息写入必须拒绝（不得静默丢弃）")
	require.Contains(t, err.Error(), "空会话标识")

	err = store.UpsertSession(ctx, core.SessionRecord{
		ProfileName: "demo", Channel: "cli", UserID: "alice",
		CreatedAt: fixedMessageTime, LastActiveAt: fixedMessageTime,
	})
	require.Error(t, err, "空会话标识没有可建的会话行")
	require.Contains(t, err.Error(), "空会话标识")

	// 零写入：两张表都没有多出行。
	require.Zero(t, countMessageRows(t, db, ""), "无状态调用不落 messages")
	require.Zero(t, countSessionRows(t, db))

	msgs, err := store.Messages(ctx, "")
	require.NoError(t, err, "无历史即是无状态会话的正确答案，不是错误")
	require.Empty(t, msgs)

	// 有状态会话的写入不受影响（拒绝只针对空标识）。
	seedStoreSession(t, store, storeSession, fixedMessageTime)
	require.NoError(t, store.AppendMessage(ctx, storeSession, storeRun,
		core.Message{Role: core.RoleUser, Content: "有状态"}))
	require.Equal(t, 1, countMessageRows(t, db, storeSession))
}

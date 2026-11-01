//go:build integration

package postgres

// 本文件是诊断事件读取端口的**有库**用例（US-16 / T097；模块卡 G1-24 的
// `TestDiagnostics_ReadFiltersByCorrelation`，`us16-pg` 具名用例）。
//
// 判的是"过滤真的发生在 SQL 里、锚点真的被用上"——三件事只有真库分得出来：
//
//  1. **会话锚点经 `runs.session_id` 解析**：假件直接回放预置切片，子查询写错也永远绿；
//  2. **时间窗是 `occurred_at` 的闭区间**：边界两端的行必须都在结果里，开闭写反只有
//     边界行能暴露；
//  3. **存储层自己拦无锚点查询**：`task_id` 单独给出不构成查询（与端点 400 同口径），
//     本包被别的装配直接调用时同样失败关闭，不指望调用方先判。
//
// 逆改点：删掉会话子查询（改成 `run_id = session_id`）→ 会话组红；把 `>=` / `<=` 改成
// 开区间 → 边界组红；去掉 `ListEvents` 开头的 `q.Validate()` → "task_id 单独给出"组红；
// 去掉 `ORDER BY occurred_at, event_id` → 顺序与 limit 截断组红。
//
// These are the database-backed cases for the diagnostics read port: the session anchor
// resolves through runs, time bounds are inclusive on both sides, and the store refuses an
// anchor-free read even when a caller skips the reader layer.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/diagnostics"
)

// dsBase 是夹具的时间基线：所有事件按毫秒递增排布，顺序断言因此确定。
// dsBase is the fixture's time origin; every event sits one millisecond apart.
var dsBase = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

// newDiagnosticStoreDB 建隔离 schema 并种两个会话、各自一个 Run：
//
//	ds-s1 ── ds-r1      （两条事件）
//	ds-s2 ── ds-r2      （一条事件）
//
// 会话锚点要靠 `runs.session_id` 解析，因此 Run 行是本组判据的一部分，不是装饰。
// The two runs are part of the verdict: the session anchor resolves through them.
func newDiagnosticStoreDB(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db := newTxTestDB(t)

	seedSession := func(sessionID string) {
		t.Helper()
		_, err := db.pool.ExecContext(ctx, `INSERT INTO sessions
			(session_id, profile_name, channel, user_id, status, message_count,
			 created_at, last_active_at, caller_id)
			VALUES ($1, 'demo', 'web', 'u1', 'active', 0, $2, $2, 'c1')`,
			sessionID, dsBase.UnixMilli())
		require.NoError(t, err)
	}
	seedRun := func(runID, sessionID string) {
		t.Helper()
		_, err := db.pool.ExecContext(ctx, `INSERT INTO runs
			(run_id, session_id, agent, channel, user_id, trigger, status, started_at,
			 caller_id, authorizing_kind, key_id, actor)
			VALUES ($1, $2, 'demo', 'web', 'u1', 'user', 'completed', $3,
			        'c1', 'caller-key', 'key-ds', 'caller')`,
			runID, sessionID, dsBase.UnixMilli())
		require.NoError(t, err)
	}
	seedSession("ds-s1")
	seedSession("ds-s2")
	seedRun("ds-r1", "ds-s1")
	seedRun("ds-r2", "ds-s2")
	return db
}

// dsEventIDs 把一页事件投影成标识列表，让顺序断言可逐项对照。
// dsEventIDs projects a page into ids so order assertions line up item by item.
func dsEventIDs(events []diagnostics.DiagnosticEvent) []string {
	ids := make([]string, len(events))
	for i, ev := range events {
		ids[i] = ev.EventID
	}
	return ids
}

// TestDiagnostics_ReadFiltersByCorrelation 判四类过滤（Run / 会话 / 时间窗 / 其他锚点）
// 与三条拒绝路径（无锚点、task_id 单独、limit 越界）。
// TestDiagnostics_ReadFiltersByCorrelation pins the filters and the refusal paths.
func TestDiagnostics_ReadFiltersByCorrelation(t *testing.T) {
	ctx := context.Background()
	db := newDiagnosticStoreDB(t)
	store := NewDiagnosticStore(db)

	write := func(at time.Time, corr diagnostics.Correlation, kind diagnostics.Kind) string {
		t.Helper()
		id, err := diagnostics.NewEventID(at)
		require.NoError(t, err)
		require.NoError(t, store.InsertEvent(ctx, diagnostics.DiagnosticEvent{
			EventID:       id,
			SchemaVersion: diagnostics.SchemaVersion,
			Kind:          kind,
			OccurredAt:    at.UnixMilli(),
			Correlation:   corr,
			Attrs:         map[string]any{},
		}))
		return id
	}

	// 两条事件落在 ds-r1（毫秒相邻），一条落在 ds-r2——同一会话锚点必须只带回前者。
	// Two events on ds-r1 and one on ds-r2 so the session anchor can be judged.
	runFirst := write(dsBase, diagnostics.Correlation{RunID: "ds-r1"}, diagnostics.KindLLMCall)
	runSecond := write(dsBase.Add(time.Millisecond), diagnostics.Correlation{RunID: "ds-r1"}, diagnostics.KindToolCall)
	foreignRun := write(dsBase.Add(2*time.Millisecond), diagnostics.Correlation{RunID: "ds-r2"}, diagnostics.KindLLMCall)
	otherOp := write(dsBase.Add(3*time.Millisecond), diagnostics.Correlation{OperationID: "ds-op"}, diagnostics.KindQuotaDecision)
	otherReq := write(dsBase.Add(4*time.Millisecond), diagnostics.Correlation{RequestID: "ds-req"}, diagnostics.KindPermissionDecision)

	// ① Run 锚点：只回这一 Run 的两条，按 `occurred_at` 升序。
	// The run anchor returns exactly its own events in occurred_at order.
	events, err := store.ListEvents(ctx, diagnostics.Query{RunID: "ds-r1"})
	require.NoError(t, err)
	require.Equal(t, []string{runFirst, runSecond}, dsEventIDs(events))
	require.NotContains(t, dsEventIDs(events), foreignRun, "别的 Run 的事件不得混进来")

	// ② 会话锚点：经 `runs.session_id` 解析成 Run 集合，ds-s2 的事件同样不得混入。
	// The session anchor resolves through runs.session_id, never through a full scan.
	events, err = store.ListEvents(ctx, diagnostics.Query{SessionID: "ds-s1"})
	require.NoError(t, err)
	require.Equal(t, []string{runFirst, runSecond}, dsEventIDs(events),
		"会话锚点必须解析成该会话下全部 Run 的事件，且只有这些")

	// ③ 时间窗是闭区间：两端都含。开闭写反时恰好丢边界行——所以两端各判一次。
	// The window is inclusive on both ends, so each boundary is asserted separately.
	events, err = store.ListEvents(ctx, diagnostics.Query{
		RunID: "ds-r1", From: dsBase, To: dsBase.Add(time.Millisecond),
	})
	require.NoError(t, err)
	require.Equal(t, []string{runFirst, runSecond}, dsEventIDs(events), "闭区间必须含两端")

	events, err = store.ListEvents(ctx, diagnostics.Query{RunID: "ds-r1", From: dsBase.Add(time.Millisecond)})
	require.NoError(t, err)
	require.Equal(t, []string{runSecond}, dsEventIDs(events), "from 之前的行必须被排除")

	events, err = store.ListEvents(ctx, diagnostics.Query{RunID: "ds-r1", To: dsBase})
	require.NoError(t, err)
	require.Equal(t, []string{runFirst}, dsEventIDs(events), "to 之后的行必须被排除")

	// ④ 其余两个主锚点：管理操作与入口请求各自独立成页。
	// The operation and request anchors each page independently.
	events, err = store.ListEvents(ctx, diagnostics.Query{OperationID: "ds-op"})
	require.NoError(t, err)
	require.Equal(t, []string{otherOp}, dsEventIDs(events))

	events, err = store.ListEvents(ctx, diagnostics.Query{RequestID: "ds-req"})
	require.NoError(t, err)
	require.Equal(t, []string{otherReq}, dsEventIDs(events))

	// ⑤ 种类过滤与 limit 截断都在 SQL 侧：截断取的是排序后的前 N 条。
	// Kind filtering and limit truncation happen after the fixed order.
	events, err = store.ListEvents(ctx, diagnostics.Query{RunID: "ds-r1", Kind: diagnostics.KindToolCall})
	require.NoError(t, err)
	require.Equal(t, []string{runSecond}, dsEventIDs(events))

	events, err = store.ListEvents(ctx, diagnostics.Query{RunID: "ds-r1", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []string{runFirst}, dsEventIDs(events), "limit 截断的是排序后的第一条")

	// ⑥ 查得到"没有"：空页不是错误——缺失语义由端点用 `missing` 表达。
	// No rows is an empty page, not an error; the endpoint owns the missing semantics.
	events, err = store.ListEvents(ctx, diagnostics.Query{IngestJobID: "ds-none"})
	require.NoError(t, err)
	require.Empty(t, events)

	events, err = store.ListEvents(ctx, diagnostics.Query{SessionID: "ds-s2-missing"})
	require.NoError(t, err)
	require.Empty(t, events, "解析不到 Run 的会话锚点返回空页，而不是全表")

	// ⑦ 拒绝路径：无锚点、task_id 单独给出、limit 越界——存储层自己判，不指望调用方。
	// The store refuses an anchor-free query, a bare task_id, and an out-of-range limit.
	_, err = store.ListEvents(ctx, diagnostics.Query{Kind: diagnostics.KindLLMCall})
	require.ErrorIs(t, err, diagnostics.ErrQueryWithoutAnchor)

	_, err = store.ListEvents(ctx, diagnostics.Query{TaskID: "ds-task"})
	require.ErrorIs(t, err, diagnostics.ErrQueryWithoutAnchor,
		"task_id 只是限定符，单独给出不构成查询")

	_, err = store.ListEvents(ctx, diagnostics.Query{RunID: "ds-r1", Limit: diagnostics.MaxQueryLimit + 1})
	require.ErrorIs(t, err, diagnostics.ErrQueryLimit)

	// ⑧ 过滤值是**参数**不是 SQL 文本：`WHERE` 片段只由包内常量拼成，取值一律走位置
	// 占位符。带引号与注释符的值因此只当作字面量——既不当成语法、也不会被"恒真"串
	// 带出全表。这条同时给 `#nosec G202` 的豁免配上可执行的反证。
	// Filter values travel as parameters, never as SQL text: a metacharacter-laden
	// value is matched literally and returns nothing.
	for _, hostile := range []string{"x' OR '1'='1", `ds-r1"; DROP TABLE diagnostic_events; --`} {
		events, err = store.ListEvents(ctx, diagnostics.Query{RunID: hostile})
		require.NoError(t, err, "带元字符的取值必须被当作字面量，而不是语法错误: %q", hostile)
		require.Empty(t, events, "恒真式不得把别人的事件捞出来: %q", hostile)
	}

	// 对照组：同一条查询用真实 run_id 仍然命中——证明上面的空页来自"值不匹配"，不是
	// 查询本身被改坏。
	// Control: the same query with the real id still matches.
	events, err = store.ListEvents(ctx, diagnostics.Query{RunID: "ds-r1"})
	require.NoError(t, err)
	require.NotEmpty(t, events, "对照组的真实锚点必须仍然命中")
}

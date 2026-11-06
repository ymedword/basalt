//go:build integration

package postgres

// 本文件是 US-22 学习窗口**读取侧**的有库用例（T017）：判的是只有真库分得出的三件事——
// 来源过滤走 `JOIN runs`（同会话里其它来源的消息、没有来源的迁移遗留消息都不得进窗口）、
// 触发 Run **自己**的最后一条消息充当右界（其后到达的同来源消息不回流）、冻结窗口按
// 原边界重读、消息被保留策略清掉后条数如实变少（上层据此判 `window_gone`，C-33）。
// The window-reader cases that only a real database can decide: origin filtering via the
// runs join, the trigger-run right bound, and frozen re-reads shrinking after prune.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// lwBase 是本文件的冻结时刻。
// lwBase is the frozen clock of these cases.
var lwBase = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

// lwSeedCaller 先建来源调用方行——会话与 Run 的 caller_id 都外键指向 callers，
// 隔离 schema 里没有预置行。
// lwSeedCaller inserts the caller rows that sessions and runs reference.
func lwSeedCaller(t *testing.T, db *DB, callerID string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `INSERT INTO callers
		(caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $2, '', 'active', $3)`,
		callerID, callerID, lwBase.UnixMilli())
	require.NoError(t, err)
}

// lwSeedSession 先建一行会话（消息表对会话有外键）。
// lwSeedSession inserts the session row that messages reference.
func lwSeedSession(t *testing.T, db *DB, sessionID string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count,
		 created_at, last_active_at, caller_id)
		VALUES ($1, 'demo', 'web', 'u1', 'active', 0, $2, $2, 'c1')`,
		sessionID, lwBase.UnixMilli())
	require.NoError(t, err)
}

// lwSeedRun 种一条已完成 Run 并显式指定来源（调用方 + Key）。
// lwSeedRun inserts one completed run with an explicit origin.
func lwSeedRun(t *testing.T, db *DB, runID, sessionID, callerID, keyID string, at time.Time) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `INSERT INTO runs
		(run_id, session_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, $2, 'demo', 'web', 'u1', 'user', 'completed', $3,
		        $4, 'caller-key', $5, 'caller')`,
		runID, sessionID, at.UnixMilli(), callerID, keyID)
	require.NoError(t, err)
}

// lwSeedMessage 追加一条消息并返回主键；`runID` 为空即写 NULL（模拟迁移遗留的无来源消息）。
// lwSeedMessage appends one message and returns its id; an empty runID writes NULL.
func lwSeedMessage(t *testing.T, db *DB, sessionID, runID, role, content string, at time.Time) int64 {
	t.Helper()
	var id int64
	require.NoError(t, db.pool.QueryRowContext(context.Background(), `INSERT INTO messages
		(session_id, run_id, seq, role, content, created_at)
		VALUES ($1, $2, (SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = $1),
		        $3, $4, $5)
		RETURNING id`,
		sessionID, nullIfEmpty(runID), role, content, at.UnixMilli()).Scan(&id))
	return id
}

// messageIDs 抽取窗口/前文的主键序列，断言顺序与条数用。
// messageIDs projects the ids of a window slice in read order.
func messageIDs(msgs []core.LearningMessage) []int64 {
	out := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.MessageID)
	}
	return out
}

// TestUS22_Store_MessagesSameOrigin 守 FR-22-03 / C-33：窗口按触发 Run 的来源（调用方 +
// Key）过滤——同会话其它来源的消息、无来源的迁移遗留消息、非 user/assistant 角色都不进
// 窗口；右界是触发 Run 自己的最后一条消息，其后到达的同来源消息不回流；返回值按主键升序，
// `From` / `To` 是实际保留边界；前文只取 `id < From` 且保留最近、按升序返回。
// The window is scoped to the trigger run's origin and anchored at that run's last message.
func TestUS22_Store_MessagesSameOrigin(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewSessionStore(db)
	lwSeedCaller(t, db, "c1")
	lwSeedCaller(t, db, "c2")
	lwSeedSession(t, db, "lw-s1")
	lwSeedRun(t, db, "lw-r1", "lw-s1", "c1", "k1", lwBase)
	lwSeedRun(t, db, "lw-r2", "lw-s1", "c2", "k2", lwBase.Add(time.Second))
	lwSeedRun(t, db, "lw-r3", "lw-s1", "c1", "k1", lwBase.Add(2*time.Second))

	q1 := lwSeedMessage(t, db, "lw-s1", "lw-r1", "user", "q1", lwBase)
	_ = lwSeedMessage(t, db, "lw-s1", "lw-r1", "system", "persona", lwBase)
	a1 := lwSeedMessage(t, db, "lw-s1", "lw-r1", "assistant", "a1", lwBase)
	_ = lwSeedMessage(t, db, "lw-s1", "lw-r2", "user", "q-other", lwBase)
	_ = lwSeedMessage(t, db, "lw-s1", "", "user", "q-legacy", lwBase)
	_ = lwSeedMessage(t, db, "lw-s1", "lw-r1", "tool", "tool-out", lwBase)
	q3 := lwSeedMessage(t, db, "lw-s1", "lw-r1", "user", "q3", lwBase)
	a3 := lwSeedMessage(t, db, "lw-s1", "lw-r1", "assistant", "a3", lwBase)
	_ = lwSeedMessage(t, db, "lw-s1", "lw-r3", "user", "q-later", lwBase.Add(2*time.Second))

	t.Run("origin-and-right-bound", func(t *testing.T) {
		window, err := store.MessagesForExtraction(ctx, "lw-s1", "lw-r1", 0, 0, 0, 0, 0)
		require.NoError(t, err)
		require.Equal(t, []int64{q1, a1, q3, a3}, messageIDs(window.New),
			"只收触发 Run 来源的 user / assistant，且不越过该 Run 的最后一条")
		require.Equal(t, q1, window.FromMessageID, "左界是实际保留的最小主键")
		require.Equal(t, a3, window.ToMessageID, "右界是触发 Run 自己最后一条消息")
		require.Zero(t, window.Dropped, "来源与角色过滤在计数之前，被滤掉的不算丢弃")
		for _, msg := range window.New {
			require.Equal(t, "lw-r1", msg.RunID)
			require.NotEmpty(t, msg.CreatedAt)
		}
		require.Empty(t, window.Prior, "priorLimit=0 表示不要前文")
	})

	t.Run("cursor-and-prior", func(t *testing.T) {
		window, err := store.MessagesForExtraction(ctx, "lw-s1", "lw-r1", a1, 2, 0, 0, 0)
		require.NoError(t, err)
		require.Equal(t, []int64{q3, a3}, messageIDs(window.New), "游标（不含）之后的窗口")
		require.Equal(t, q3, window.FromMessageID)
		require.Equal(t, a3, window.ToMessageID)
		require.Equal(t, []int64{q1, a1}, messageIDs(window.Prior),
			"前文取 id < From 的同来源消息，按主键升序返回")
	})

	t.Run("prior-keeps-newest", func(t *testing.T) {
		window, err := store.MessagesForExtraction(ctx, "lw-s1", "lw-r1", a1, 1, 0, 0, 0)
		require.NoError(t, err)
		require.Equal(t, []int64{a1}, messageIDs(window.Prior), "前文保留最近一条")
	})

	t.Run("no-new-messages", func(t *testing.T) {
		window, err := store.MessagesForExtraction(ctx, "lw-s1", "lw-r1", a3, 0, 0, 0, 0)
		require.NoError(t, err)
		require.Empty(t, window.New, "游标已到触发 Run 末尾时窗口为空")
		require.Zero(t, window.FromMessageID)
		require.Zero(t, window.ToMessageID)
	})

	t.Run("unknown-run", func(t *testing.T) {
		_, err := store.MessagesForExtraction(ctx, "lw-s1", "lw-missing", 0, 0, 0, 0, 0)
		require.Error(t, err, "读不到触发 Run 的来源时必须报错，不得退化成全量窗口")
	})
}

// TestUS22_Store_MessagesForFrozenWindow 守 FR-22-03 / C-33：重试只按冻结的 `from` / `to`
// 与来源重读同一段——边界原样返回（不按重读结果重算），角色与来源过滤照旧；消息被清掉后
// 条数如实变少，上层据此判 `window_gone` 而不是拿残缺集合继续抽取。
// The frozen re-read keeps the recorded bounds verbatim and shrinks honestly after prune.
func TestUS22_Store_MessagesForFrozenWindow(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewSessionStore(db)
	lwSeedCaller(t, db, "c1")
	lwSeedCaller(t, db, "c2")
	lwSeedSession(t, db, "lw-f1")
	lwSeedRun(t, db, "lw-fr1", "lw-f1", "c1", "k1", lwBase)
	lwSeedRun(t, db, "lw-fr2", "lw-f1", "c2", "k2", lwBase)

	m1 := lwSeedMessage(t, db, "lw-f1", "lw-fr1", "user", "f1", lwBase)
	m2 := lwSeedMessage(t, db, "lw-f1", "lw-fr1", "user", "f2", lwBase)
	_ = lwSeedMessage(t, db, "lw-f1", "lw-fr1", "system", "f-persona", lwBase)
	m3 := lwSeedMessage(t, db, "lw-f1", "lw-fr1", "assistant", "f3", lwBase)
	other := lwSeedMessage(t, db, "lw-f1", "lw-fr2", "user", "f-other", lwBase)
	m4 := lwSeedMessage(t, db, "lw-f1", "lw-fr1", "user", "f4", lwBase)
	_ = lwSeedMessage(t, db, "lw-f1", "lw-fr1", "assistant", "f5", lwBase)

	t.Run("bounds-verbatim-and-filtered", func(t *testing.T) {
		window, err := store.MessagesForFrozenWindow(ctx, "lw-f1", "c1", "k1", m2, m4, 3, 0)
		require.NoError(t, err)
		require.Equal(t, []int64{m2, m3, m4}, messageIDs(window.New),
			"冻结段内只返回同来源的 user / assistant")
		require.Equal(t, m2, window.FromMessageID, "冻结边界原样返回")
		require.Equal(t, m4, window.ToMessageID)
		require.Equal(t, []int64{m1}, messageIDs(window.Prior), "前文仍在窗口之前单独取")
	})

	t.Run("origin-filter", func(t *testing.T) {
		window, err := store.MessagesForFrozenWindow(ctx, "lw-f1", "c2", "k2", m2, m4, 0, 0)
		require.NoError(t, err)
		require.Equal(t, []int64{other}, messageIDs(window.New),
			"冻结窗口同样按来源过滤")
		require.Empty(t, window.Prior)
	})

	t.Run("pruned-window-shrinks", func(t *testing.T) {
		_, err := db.pool.ExecContext(ctx, `DELETE FROM messages WHERE id = $1`, m3)
		require.NoError(t, err)
		window, err := store.MessagesForFrozenWindow(ctx, "lw-f1", "c1", "k1", m2, m4, 0, 0)
		require.NoError(t, err)
		require.Equal(t, []int64{m2, m4}, messageIDs(window.New),
			"被清理的消息不再返回：条数变少由上层判 window_gone")
		require.Equal(t, m2, window.FromMessageID, "条数变化不改写冻结边界")
		require.Equal(t, m4, window.ToMessageID)
	})

	t.Run("invalid-bounds", func(t *testing.T) {
		window, err := store.MessagesForFrozenWindow(ctx, "lw-f1", "c1", "k1", 0, m4, 0, 0)
		require.NoError(t, err)
		require.Empty(t, window.New, "非法边界返回空窗口而不是报错")
		_, err = store.MessagesForFrozenWindow(ctx, "lw-f1", "c1", "k1", m4, m2, 0, 0)
		require.NoError(t, err)
	})
}

//go:build integration

package postgres

// 本文件是会话列表**归属过滤**的有库用例（US-16 / T056）。判据不是"函数能跑通"，
// 而是十二个编译期 SQL 常量的占位符顺序：归属恒为 `$1`、Agent 顺延 `$2`——顺序
// 写反时 SQL 照样能执行，只会把 `cal_1` 当画像名、把 `demo` 当调用方去比，返回
// 空集而不是报错。只有真库能把这类错判出来（单测里的假件看不到 SQL）。
//
// 第二组判据是 **NULL 归属**：`caller_id = $1` 天然排除 NULL，迁移遗留的空归属行
// 因此只对管理员可见；若实现改成 `COALESCE(caller_id,'') = $1` 或忘了加条件，
// 无归属行会漏给任意调用方。
//
// These are the database-backed cases for the ownership filter on the session list.
// The twelve compile-time SQL constants bind the caller filter at $1 and the agent
// filter at $2; a swapped order still executes and silently returns an empty set, so
// only a real server catches it. NULL owner rows must never leak to a caller.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// slBase 是夹具时间基线：会话按 `last_active_at` 倒序，时间差让顺序可判。
var slBase = time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)

// newSessionListDB 在隔离 schema 上准备三行归属会话与一行**无归属**遗留会话：
//
//	s-c1-demo-live   cal_1 / demo / 未归档 / +3h
//	s-c1-ops-closed  cal_1 / ops  / 已归档 / +2h
//	s-c2-demo-live   cal_2 / demo / 未归档 / +1h
//	s-legacy-demo    (NULL) / demo / 未归档 / +0h
//
// The fixture includes one NULL-owner legacy row so the caller filter's treatment of
// NULL can be asserted rather than assumed.
func newSessionListDB(t *testing.T) (*DB, *SessionStore) {
	t.Helper()
	ctx := context.Background()
	db := newTxTestDB(t) // 已种调用方 c1，并建好隔离 schema 与扩展 search_path
	_, err := db.pool.ExecContext(ctx, `INSERT INTO callers
		(caller_id, kind, name, project, status, created_at)
		VALUES ('c2', 'service', 'c2', '', 'active', $1)`, slBase.UnixMilli())
	require.NoError(t, err)

	store := NewSessionStore(db)
	seed := func(id, profile, caller string, at time.Time) {
		require.NoError(t, store.UpsertSession(ctx, core.SessionRecord{
			ID: id, ProfileName: profile, Channel: "web", UserID: "u1",
			CreatedAt: slBase, LastActiveAt: at, CallerID: caller,
		}))
	}
	seed("s-c1-demo-live", "demo", "c1", slBase.Add(3*time.Hour))
	seed("s-c1-ops-closed", "ops", "c1", slBase.Add(2*time.Hour))
	seed("s-c2-demo-live", "demo", "c2", slBase.Add(time.Hour))
	seed("s-legacy-demo", "demo", "", slBase)
	require.NoError(t, store.ArchiveSession(ctx, "s-c1-ops-closed", slBase.Add(4*time.Hour)))
	return db, store
}

// sessionIDs 取出列表的会话标识序列（顺序即仓储给出的顺序）。
// sessionIDs extracts the ids in the order the store returned them.
func sessionIDs(records []core.SessionRecord) []string {
	out := make([]string, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.ID)
	}
	return out
}

// TestSessionList_CallerFilterCombinations 逐一钉住归属维的六种组合，并确认每一种
// 都排除 NULL 归属行。
// 逆改点：把 `$1` / `$2` 对调 → 各组合返回空集（`demo` 被当调用方比）；漏掉
// `caller_id = $1` → 调用方看见他人与遗留行。
// TestSessionList_CallerFilterCombinations pins all six caller-filter combinations.
func TestSessionList_CallerFilterCombinations(t *testing.T) {
	ctx := context.Background()
	_, store := newSessionListDB(t)

	cases := []struct {
		name   string
		filter core.SessionFilter
		want   []string
	}{
		{
			"归属全量",
			core.SessionFilter{CallerID: "c1"},
			[]string{"s-c1-demo-live", "s-c1-ops-closed"},
		},
		{
			"归属 + 未归档",
			core.SessionFilter{CallerID: "c1", Status: "active"},
			[]string{"s-c1-demo-live"},
		},
		{
			"归属 + 已归档",
			core.SessionFilter{CallerID: "c1", Status: "archived"},
			[]string{"s-c1-ops-closed"},
		},
		{
			"归属 + 画像",
			core.SessionFilter{CallerID: "c1", Agent: "demo"},
			[]string{"s-c1-demo-live"},
		},
		{
			"归属 + 画像 + 未归档",
			core.SessionFilter{CallerID: "c1", Agent: "demo", Status: "active"},
			[]string{"s-c1-demo-live"},
		},
		{
			"归属 + 画像 + 已归档",
			core.SessionFilter{CallerID: "c1", Agent: "demo", Status: "archived"},
			[]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.ListSessions(ctx, tc.filter)
			require.NoError(t, err)
			require.Equal(t, tc.want, sessionIDs(got))
		})
	}
}

// TestSessionList_CallerNeverSeesLegacyRows 判 NULL 归属行的可见性边界：调用方看不到，
// 管理员看得到（`caller_id = $1` 对 NULL 恒假，不是被应用层过滤掉的）。
// 逆改点：把条件改成 `COALESCE(caller_id,”) = $1` 并传空串 → 遗留行漏给调用方。
// TestSessionList_CallerNeverSeesLegacyRows pins that NULL owner rows stay admin-only.
func TestSessionList_CallerNeverSeesLegacyRows(t *testing.T) {
	ctx := context.Background()
	_, store := newSessionListDB(t)

	caller, err := store.ListSessions(ctx, core.SessionFilter{CallerID: "c2"})
	require.NoError(t, err)
	require.Equal(t, []string{"s-c2-demo-live"}, sessionIDs(caller),
		"另一个调用方同样看不到他人的行与遗留行")

	admin, err := store.ListSessions(ctx, core.SessionFilter{})
	require.NoError(t, err)
	require.Equal(t, []string{
		"s-c1-demo-live", "s-c1-ops-closed", "s-c2-demo-live", "s-legacy-demo",
	}, sessionIDs(admin), "管理员跨调用方，含遗留行；排序为最后活动时间倒序")

	byAgent, err := store.ListSessions(ctx, core.SessionFilter{Agent: "demo"})
	require.NoError(t, err)
	require.Equal(t, []string{"s-c1-demo-live", "s-c2-demo-live", "s-legacy-demo"},
		sessionIDs(byAgent), "画像维单独使用时包含遗留行")
}

// TestSessionList_CallerFilterKeepsFixedOrder 判归属过滤不改变排序口径：同一调用方的
// 多行仍按最后活动时间倒序、`session_id` 兜底。
// 逆改点：把 ORDER BY 挪到条件之前拼错、或按 caller_id 排序 → 顺序断言红。
// TestSessionList_CallerFilterKeepsFixedOrder proves the ordering survives the filter.
func TestSessionList_CallerFilterKeepsFixedOrder(t *testing.T) {
	ctx := context.Background()
	_, store := newSessionListDB(t)

	// 同一调用方的第三行，最后活动时间更早——若实现按插入序或标识序返回，它会错位。
	require.NoError(t, store.UpsertSession(ctx, core.SessionRecord{
		ID: "s-c1-a-old", ProfileName: "demo", Channel: "web", UserID: "u1",
		CreatedAt: slBase, LastActiveAt: slBase.Add(-time.Hour), CallerID: "c1",
	}))
	// 两行同一时刻：`session_id` 兜底次序让结果确定。
	require.NoError(t, store.UpsertSession(ctx, core.SessionRecord{
		ID: "s-c1-z-tie", ProfileName: "demo", Channel: "web", UserID: "u1",
		CreatedAt: slBase, LastActiveAt: slBase.Add(3 * time.Hour), CallerID: "c1",
	}))

	got, err := store.ListSessions(ctx, core.SessionFilter{CallerID: "c1"})
	require.NoError(t, err)
	require.Equal(t, []string{
		"s-c1-demo-live", "s-c1-z-tie", // 同一时刻，按 session_id 升序
		"s-c1-ops-closed", "s-c1-a-old",
	}, sessionIDs(got))
}

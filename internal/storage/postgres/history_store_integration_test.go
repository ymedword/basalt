//go:build integration

package postgres

// 本文件是历史回查端口（`history.MessageQueryStore`）的有库用例（US-16 / T139）。
//
// 判据都落在**真库**才看得见的地方：
//  1. 归属过滤写在 SQL 里还是取回后在 Go 里筛——后者在 `LIMIT` 之下会让分页失真，
//     假件（内存切片）永远测不出来；
//  2. `snippet` 是 200 字符窗口还是整段正文——只有真行才分得出；
//  3. `message_evidence_refs` 是否与消息一次取回——假件直接塞字段，绕过了 join；
//  4. NULL 归属（迁移遗留）只对管理员可见——`caller_id = $1` 对 NULL 恒假，是 SQL 的
//     天然语义，不是应用层补的。
//
// These are the database-backed cases for the history query port. Only a real server
// distinguishes "filtered in SQL" from "filtered in Go", bounds the snippet, exercises
// the evidence-ref join, and proves NULL-owner rows stay admin-only.

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/history"
)

// hsBase 是夹具时间基线：消息按 `created_at` 倒序，时间差让顺序可判。
var hsBase = time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)

// newHistoryStoreDB 在隔离 schema 上准备四个会话：
//
//	hs-c1      demo / 调用方 c1
//	hs-c1-ops  ops  / 调用方 c1
//	hs-c2      demo / 调用方 c2
//	hs-legacy  demo / **NULL 归属**（迁移遗留）
//
// The fixture includes one NULL-owner legacy session so its visibility boundary is
// asserted rather than assumed.
func newHistoryStoreDB(t *testing.T) (*DB, *SessionStore, *HistoryStore) {
	t.Helper()
	ctx := context.Background()
	db := newTxTestDB(t) // 已种调用方 c1，并建好隔离 schema 与扩展 search_path
	_, err := db.pool.ExecContext(ctx, `INSERT INTO callers
		(caller_id, kind, name, project, status, created_at)
		VALUES ('c2', 'service', 'c2', '', 'active', $1)`, hsBase.UnixMilli())
	require.NoError(t, err)

	sessions := NewSessionStore(db)
	seed := func(id, profile, caller string) {
		t.Helper()
		require.NoError(t, sessions.UpsertSession(ctx, core.SessionRecord{
			ID: id, ProfileName: profile, Channel: "web", UserID: "u1",
			CreatedAt: hsBase, LastActiveAt: hsBase, CallerID: caller,
		}))
	}
	seed("hs-c1", "demo", "c1")
	seed("hs-c1-ops", "ops", "c1")
	seed("hs-c2", "demo", "c2")
	seed("hs-legacy", "demo", "")
	return db, sessions, NewHistoryStore(db)
}

// appendHistoryMessage 追加一条消息，并把 `created_at` 改成测试指定的时刻——写路径的
// 时钟是 `time.Now()`，排序断言需要一条确定的时间轴。
// appendHistoryMessage pins the row's created_at so ordering assertions are deterministic.
func appendHistoryMessage(t *testing.T, db *DB, store *SessionStore, sessionID, role, content string, at time.Time, refs ...core.EvidenceRef) string {
	t.Helper()
	ctx := context.Background()
	id, err := store.AppendMessage(ctx, sessionID, "run-hist", core.Message{
		Role: role, Content: content, EvidenceRefs: refs,
	})
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx,
		`UPDATE messages SET created_at = $2 WHERE id = $1::bigint`, id, at.UnixMilli())
	require.NoError(t, err)
	return id
}

// hitIDs 取出命中返回的消息标识序列（顺序即查询给出的顺序）。
// hitIDs extracts the ids in the order the store returned them.
func hitIDs(hits []history.HistoryHit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.MessageID)
	}
	return out
}

// TestHistoryStore_SearchScopesToCaller 钉住归属边界：调用方只看到自己的消息，管理员
// 跨调用方（含 NULL 归属的遗留行），管理员可按请求收窄到某一调用方。
// 逆改点：去掉 `s.caller_id = $n` → 调用方读到三家的命中；把 scope.CallerID 当成
// 可选的请求参数（只信 `q.CallerID`）→ 调用方传空值时读到全部。
// TestHistoryStore_SearchScopesToCaller pins the ownership boundary of search.
func TestHistoryStore_SearchScopesToCaller(t *testing.T) {
	ctx := context.Background()
	db, sessions, store := newHistoryStoreDB(t)

	own := appendHistoryMessage(t, db, sessions, "hs-c1", "user", "发布前必须先备份 audit.db", hsBase)
	other := appendHistoryMessage(t, db, sessions, "hs-c2", "user", "发布前必须确认备份窗口", hsBase.Add(time.Hour))
	legacy := appendHistoryMessage(t, db, sessions, "hs-legacy", "user", "备份脚本已迁移", hsBase.Add(2*time.Hour))

	c1, err := store.SearchMessages(ctx, history.Scope{CallerID: "c1"}, history.HistoryQuery{Keywords: "备份", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{own}, hitIDs(c1), "调用方只看到自己的消息")

	c2, err := store.SearchMessages(ctx, history.Scope{CallerID: "c2"}, history.HistoryQuery{Keywords: "备份", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{other}, hitIDs(c2))

	admin, err := store.SearchMessages(ctx, history.Scope{Admin: true}, history.HistoryQuery{Keywords: "备份", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{legacy, other, own}, hitIDs(admin),
		"管理员跨调用方，含 NULL 归属的遗留行；按时间倒序")

	narrowed, err := store.SearchMessages(ctx, history.Scope{Admin: true, CallerID: "c1"}, history.HistoryQuery{Keywords: "备份", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{own}, hitIDs(narrowed), "管理员可按请求收窄到某一调用方")
}

// TestHistoryStore_SearchOrderAndLimit 钉住排序与窗口：时间倒序、同刻按消息标识倒序、
// `LIMIT` 由存储执行（handler 不重排）。
// 逆改点：只按 `m.id DESC` 排序 → 最旧的那条（后插入、标识更大）排到最前；忽略
// `LIMIT` → 返回四条。
// TestHistoryStore_SearchOrderAndLimit pins the fixed ordering and the row cap.
func TestHistoryStore_SearchOrderAndLimit(t *testing.T) {
	ctx := context.Background()
	db, sessions, store := newHistoryStoreDB(t)

	newest := appendHistoryMessage(t, db, sessions, "hs-c1", "user", "备份 最旧时间但最先插入", hsBase.Add(2*time.Hour))
	oldest := appendHistoryMessage(t, db, sessions, "hs-c1", "user", "备份 更早时间", hsBase)
	tieA := appendHistoryMessage(t, db, sessions, "hs-c1", "user", "备份 同一时刻 A", hsBase.Add(time.Hour))
	tieB := appendHistoryMessage(t, db, sessions, "hs-c1", "user", "备份 同一时刻 B", hsBase.Add(time.Hour))

	hits, err := store.SearchMessages(ctx, history.Scope{CallerID: "c1"}, history.HistoryQuery{Keywords: "备份", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{newest, tieB, tieA, oldest}, hitIDs(hits),
		"时间倒序；同一时刻按消息标识倒序")

	page, err := store.SearchMessages(ctx, history.Scope{CallerID: "c1"}, history.HistoryQuery{Keywords: "备份", Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{newest, tieB}, hitIDs(page), "窗口截断由存储执行")
}

// TestHistoryStore_SearchSnippetIsBoundedWindow 钉住"命中只是片段"：正文远超 200 字符时，
// 片段仍是围绕命中的 200 字符窗口，且包含关键词。
// 逆改点：投影改成 `m.content` → 片段与正文等长，红。
// TestHistoryStore_SearchSnippetIsBoundedWindow pins the snippet to a bounded window.
func TestHistoryStore_SearchSnippetIsBoundedWindow(t *testing.T) {
	ctx := context.Background()
	db, sessions, store := newHistoryStoreDB(t)

	const marker = "关键标记KEYWORD-73c1"
	content := strings.Repeat("前导内容。", 60) + marker + strings.Repeat("尾随内容。", 60)
	require.Greater(t, utf8.RuneCountInString(content), 400, "夹具正文必须远超片段窗口")
	appendHistoryMessage(t, db, sessions, "hs-c1", "user", content, hsBase)

	hits, err := store.SearchMessages(ctx, history.Scope{CallerID: "c1"},
		history.HistoryQuery{Keywords: marker, Limit: 10})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.LessOrEqual(t, utf8.RuneCountInString(hits[0].Snippet), 200,
		"片段是 200 字符窗口，不是整段正文")
	require.Contains(t, hits[0].Snippet, marker, "窗口必须包住命中位置")
	require.NotEqual(t, content, hits[0].Snippet, "检索命中不得返回正文")
}

// TestHistoryStore_SearchNarrowsByFilters 钉住 Agent / 会话 / 时间窗三个收窄维度：
// 每个条件都只能收窄，不能放大。
// 逆改点：占位符续号错位（如 Agent 条件绑到调用方的参数位）→ 真库直接类型报错或返回
// 空集；把 `>=` 写成 `>` → 边界时刻的消息丢掉。
// TestHistoryStore_SearchNarrowsByFilters pins the three narrowing filters.
func TestHistoryStore_SearchNarrowsByFilters(t *testing.T) {
	ctx := context.Background()
	db, sessions, store := newHistoryStoreDB(t)

	early := appendHistoryMessage(t, db, sessions, "hs-c1", "user", "备份 早", hsBase)
	late := appendHistoryMessage(t, db, sessions, "hs-c1", "user", "备份 晚", hsBase.Add(3*time.Hour))
	ops := appendHistoryMessage(t, db, sessions, "hs-c1-ops", "user", "备份 运维会话", hsBase.Add(time.Hour))

	scope := history.Scope{CallerID: "c1"}

	demo, err := store.SearchMessages(ctx, scope, history.HistoryQuery{Keywords: "备份", Agent: "demo", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{late, early}, hitIDs(demo), "画像过滤")

	bySession, err := store.SearchMessages(ctx, scope, history.HistoryQuery{Keywords: "备份", SessionID: "hs-c1-ops", Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{ops}, hitIDs(bySession), "会话过滤")

	from, err := store.SearchMessages(ctx, scope, history.HistoryQuery{Keywords: "备份", From: hsBase.Add(2 * time.Hour), Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{late}, hitIDs(from), "时间窗起点（含）")

	to, err := store.SearchMessages(ctx, scope, history.HistoryQuery{Keywords: "备份", To: hsBase, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []string{early}, hitIDs(to), "时间窗终点（含）")
}

// TestHistoryStore_SearchRejectsUnscopedCaller 钉住 fail-closed：非管理员却带着空的
// `CallerID` 时**拒绝**，不得把空值当成"不限调用方"。
// 逆改点：空值走管理员分支（返回全部）→ 断言红，且这是一条真实的越权路径。
// TestHistoryStore_SearchRejectsUnscopedCaller pins that an unscoped caller is refused.
func TestHistoryStore_SearchRejectsUnscopedCaller(t *testing.T) {
	ctx := context.Background()
	db, sessions, store := newHistoryStoreDB(t)
	appendHistoryMessage(t, db, sessions, "hs-c1", "user", "备份", hsBase)

	_, err := store.SearchMessages(ctx, history.Scope{}, history.HistoryQuery{Keywords: "备份", Limit: 10})
	require.ErrorIs(t, err, history.ErrForbidden, "空归属的调用方范围必须拒绝")

	_, err = store.SearchMessages(ctx, history.Scope{CallerID: "c1"}, history.HistoryQuery{Limit: 10})
	require.ErrorIs(t, err, history.ErrInvalidQuery, "空关键词在任何查询之前拒绝")
}

// TestHistoryStore_ReadMessagesReturnsEvidenceRefs 钉住精确读取：原文、元数据与来源
// 引用一次取回，引用原样透传（不解析成最新版本）。
// 逆改点：不读 `message_evidence_refs` → `EvidenceRefs` 为空，红；把引用按 ref_id
// 回查知识库最新版本 → ContentHash 被改写，红。
// TestHistoryStore_ReadMessagesReturnsEvidenceRefs pins the exact-read projection.
func TestHistoryStore_ReadMessagesReturnsEvidenceRefs(t *testing.T) {
	ctx := context.Background()
	db, sessions, store := newHistoryStoreDB(t)

	refs := []core.EvidenceRef{
		{Kind: "knowledge", ID: "doc-1", VersionID: "v1", ContentHash: "hash-1"},
		{Kind: "memory", ID: "mem-1", VersionID: "v2"},
	}
	id := appendHistoryMessage(t, db, sessions, "hs-c1", "assistant", "回滚步骤见 runbook", hsBase, refs...)

	views, err := store.ReadMessages(ctx, history.Scope{CallerID: "c1"}, []history.MessageRef{{MessageID: id}})
	require.NoError(t, err)
	require.Len(t, views, 1)
	view := views[0]
	require.Equal(t, id, view.MessageID)
	require.Equal(t, "hs-c1", view.SessionID)
	require.Equal(t, "run-hist", view.RunID)
	require.Equal(t, int64(1), view.Seq)
	require.Equal(t, "assistant", view.Role)
	require.Equal(t, "回滚步骤见 runbook", view.Content)
	require.True(t, view.CreatedAt.Equal(hsBase), "created_at 按毫秒取回并转 UTC")
	require.ElementsMatch(t, refs, view.EvidenceRefs, "来源引用原样透传")

	bySession, err := store.ReadMessages(ctx, history.Scope{CallerID: "c1"},
		[]history.MessageRef{{MessageID: id, SessionID: "hs-c1"}})
	require.NoError(t, err)
	require.Len(t, bySession, 1, "会话范围相符时正常返回")
}

// TestHistoryStore_ReadMessagesCrossCallerIsNotFound 钉住跨调用方一律 404：读不到就是
// 未找到，**不得**报 403（那会泄露"这条消息存在"）。
// 逆改点：把 `JOIN sessions` 与归属条件删掉 → 调用方读到他人消息，红；把未找到改成
// `ErrForbidden` → 断言红。
// TestHistoryStore_ReadMessagesCrossCallerIsNotFound pins not-found for alien messages.
func TestHistoryStore_ReadMessagesCrossCallerIsNotFound(t *testing.T) {
	ctx := context.Background()
	db, sessions, store := newHistoryStoreDB(t)

	alien := appendHistoryMessage(t, db, sessions, "hs-c2", "user", "调用方二的私有记录", hsBase)

	_, err := store.ReadMessages(ctx, history.Scope{CallerID: "c1"}, []history.MessageRef{{MessageID: alien}})
	require.ErrorIs(t, err, history.ErrNotFound, "跨调用方是未找到，不是禁止")
	require.NotErrorIs(t, err, history.ErrForbidden)

	admin, err := store.ReadMessages(ctx, history.Scope{Admin: true}, []history.MessageRef{{MessageID: alien}})
	require.NoError(t, err)
	require.Len(t, admin, 1, "管理员可跨调用方读取")

	_, err = store.ReadMessages(ctx, history.Scope{CallerID: "c1"},
		[]history.MessageRef{{MessageID: alien, SessionID: "hs-c1"}})
	require.ErrorIs(t, err, history.ErrNotFound, "会话范围不符按未找到")

	_, err = store.ReadMessages(ctx, history.Scope{CallerID: "c1"}, []history.MessageRef{{MessageID: "999999999"}})
	require.ErrorIs(t, err, history.ErrNotFound, "不存在的消息")

	_, err = store.ReadMessages(ctx, history.Scope{CallerID: "c1"}, []history.MessageRef{{MessageID: "msg/run-1/1"}})
	require.ErrorIs(t, err, history.ErrNotFound, "无状态字面量不是主键，不得拿它去查库")
}

// TestHistoryStore_ResolveSessionOwnership 钉住会话归属解析：自己的会话可解析，他人的
// 与会话的 NULL 归属（遗留）对调用方未找到，管理员可解析遗留行。
// 逆改点：去掉归属条件 → 调用方解析到他人会话；把 NULL 归属读成空串后当"无限制" →
// 遗留行漏给调用方。
// TestHistoryStore_ResolveSessionOwnership pins the session ownership boundary.
func TestHistoryStore_ResolveSessionOwnership(t *testing.T) {
	ctx := context.Background()
	_, _, store := newHistoryStoreDB(t)

	own, err := store.ResolveSession(ctx, history.Scope{CallerID: "c1"}, "hs-c1")
	require.NoError(t, err)
	require.Equal(t, history.SessionRef{
		SessionID: "hs-c1", CallerID: "c1", Agent: "demo", CreatedAt: hsBase,
	}, own)

	_, err = store.ResolveSession(ctx, history.Scope{CallerID: "c1"}, "hs-c2")
	require.ErrorIs(t, err, history.ErrNotFound, "他人的会话是未找到")

	_, err = store.ResolveSession(ctx, history.Scope{CallerID: "c1"}, "hs-legacy")
	require.ErrorIs(t, err, history.ErrNotFound, "NULL 归属的遗留会话只对管理员可见")

	legacy, err := store.ResolveSession(ctx, history.Scope{Admin: true}, "hs-legacy")
	require.NoError(t, err)
	require.Empty(t, legacy.CallerID, "遗留行的归属如实为空串")

	_, err = store.ResolveSession(ctx, history.Scope{Admin: true}, "hs-none")
	require.ErrorIs(t, err, history.ErrNotFound)

	_, err = store.ResolveSession(ctx, history.Scope{CallerID: "c1"}, "")
	require.ErrorIs(t, err, history.ErrNotFound, "空会话标识不查库")
}

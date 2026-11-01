package history

// 本文件覆盖 T055 的**授权结算**：按 `sessions.caller_id` 过滤、跨调用方一律未找到
// （设计 §4 的 `TestHistory_CrossCallerIsolation`）、管理员可跨调用方、迁移遗留的
// 空归属会话只对管理员可见。
//
// 假存储在 `SearchMessages` 里按 `Scope` 过滤归属——与生产实现同构。因此一旦
// `resolveScope` 把调用方范围结算成空或放宽，跨调用方案例会直接看到他人的行。
//
// This file covers the authorization settlement: ownership filtering, cross-caller
// isolation reported as not-found, admin spanning, and legacy ownerless sessions.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// seedCrossCaller 造三行：cal_1 的、cal_2 的、以及迁移遗留（无归属）的一行。
// seedCrossCaller seeds one row per caller plus one legacy ownerless row.
func seedCrossCaller() []fakeMessage {
	return []fakeMessage{
		{
			id: "11", sessionID: "sess-cal-1", runID: "run-1", callerID: "cal_1",
			agent: "ops", role: "user", content: "备份 audit.db 的记录", createdAt: seedTime,
		},
		{
			id: "22", sessionID: "sess-cal-2", runID: "run-2", callerID: "cal_2",
			agent: "ops", role: "user", content: "备份完成，校验和一致", createdAt: seedTime.Add(-time.Minute),
		},
		{
			id: "33", sessionID: "sess-legacy", runID: "run-3", callerID: "",
			agent: "ops", role: "assistant", content: "迁移前的备份记录", createdAt: seedTime.Add(-2 * time.Minute),
		},
	}
}

// ids 取出命中的消息标识，便于断言"没有他人的消息"。
// ids collects the hit ids so assertions can name what must be absent.
func ids(hits []HistoryHit) []string {
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.MessageID)
	}
	return out
}

// TestHistory_CrossCallerIsolation 保护 C-28 的归属边界。
//
// 逆改点（实现怎么改它会红）：
//   - `resolveScope` 对调用方返回空 `CallerID`（或 `Admin`）→ 假存储不再过滤，
//     调用方案例看到他人的消息；
//   - 调用方指定他人 `caller_id` 时返回 `ErrNotFound` 而不是 `ErrForbidden` →
//     越权参数与"没找到"的语义混淆，第一条子用例的错误断言失败；
//   - 让空归属的遗留会话匹配任意调用方 → 遗留会话子用例命中；
//   - 把 `sessions:read` 换成 `agents:invoke` 之类的前提 → 正常读取子用例被拒。
//
// This test pins the C-28 ownership boundary: no cross-caller leaks, forbidden when
// the request itself overreaches, legacy rows admin-only.
func TestHistory_CrossCallerIsolation(t *testing.T) {
	ctx := context.Background()
	newStore := func() *fakeMessageStore { return newFakeMessageStore(40, seedCrossCaller()...) }

	t.Run("caller sees only its own messages", func(t *testing.T) {
		store := newStore()
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		// 只持 `sessions:read`（没有 `agents:invoke:*`）：历史是归属数据，
		// 撤销调用授权不影响对既有会话的读取。
		// Only sessions:read is held; history is ownership data, not invoke authority.
		result, err := searcher.Search(ctx, callerPrincipal("cal_1", core.ScopeSessionsRead), HistoryQuery{Keywords: "备份"})
		require.NoError(t, err)
		require.Equal(t, []string{"11"}, ids(result.Hits), "调用方只能看到自己的消息")
		require.Equal(t, Scope{CallerID: "cal_1"}, store.lastScope, "调用方范围必须结算成自己的归属")
	})

	t.Run("caller naming another caller is forbidden", func(t *testing.T) {
		store := newStore()
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		_, err := searcher.Search(ctx, callerPrincipal("cal_1", core.ScopeSessionsRead),
			HistoryQuery{Keywords: "备份", CallerID: "cal_2"})
		require.ErrorIs(t, err, ErrForbidden, "指定他人 caller_id 是越权，不是没找到")
		require.Zero(t, store.calls, "越权请求不得访问存储")
	})

	t.Run("caller naming itself is allowed", func(t *testing.T) {
		store := newStore()
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		result, err := searcher.Search(ctx, callerPrincipal("cal_1", core.ScopeSessionsRead),
			HistoryQuery{Keywords: "备份", CallerID: "cal_1"})
		require.NoError(t, err)
		require.Equal(t, []string{"11"}, ids(result.Hits))
	})

	t.Run("admin spans callers including legacy rows", func(t *testing.T) {
		store := newStore()
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		result, err := searcher.Search(ctx, core.LocalAdminPrincipal(), HistoryQuery{Keywords: "备份"})
		require.NoError(t, err)
		require.Equal(t, []string{"11", "22", "33"}, ids(result.Hits), "管理员可跨调用方，含无归属的遗留会话")
		require.True(t, store.lastScope.Admin)
	})

	t.Run("admin may narrow to one caller", func(t *testing.T) {
		store := newStore()
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		result, err := searcher.Search(ctx, core.LocalAdminPrincipal(),
			HistoryQuery{Keywords: "备份", CallerID: "cal_2"})
		require.NoError(t, err)
		require.Equal(t, []string{"22"}, ids(result.Hits))
		require.Equal(t, Scope{Admin: true, CallerID: "cal_2"}, store.lastScope)
	})

	t.Run("legacy session is invisible to a caller", func(t *testing.T) {
		store := newStore()
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		result, err := searcher.Search(ctx, callerPrincipal("cal_1", core.ScopeSessionsRead),
			HistoryQuery{Keywords: "备份", SessionID: "sess-legacy"})
		require.NoError(t, err)
		require.Empty(t, result.Hits, "空归属会话不得匹配到任意调用方")

		_, err = store.ResolveSession(ctx, Scope{CallerID: "cal_1"}, "sess-legacy")
		require.ErrorIs(t, err, ErrNotFound, "跨调用方的会话解析一律未找到")
	})

	t.Run("zero principal is unauthorized", func(t *testing.T) {
		store := newStore()
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		_, err := searcher.Search(ctx, core.Principal{}, HistoryQuery{Keywords: "备份"})
		require.ErrorIs(t, err, core.ErrUnauthorized, "零值身份不得被补成内置管理员")
		require.Zero(t, store.calls)
	})

	t.Run("caller without sessions read is forbidden", func(t *testing.T) {
		store := newStore()
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		_, err := searcher.Search(ctx, callerPrincipal("cal_1", core.ScopeSessionsWrite),
			HistoryQuery{Keywords: "备份"})
		require.ErrorIs(t, err, ErrForbidden, "缺少 sessions:read 时拒绝")
		require.Zero(t, store.calls)
	})
}

// TestHistory_ForbiddenAliasesCore 保护 403 判据只有一份：`history.ErrForbidden`
// 与 `core.ErrForbidden` 必须同值，web 层两种拼写都能 `errors.Is`。
//
// 逆改点：把它改成 `errors.New(...)` → 别名断言失败，web 层的 403 映射随之失真。
//
// This test pins that the 403 sentinel is a same-value alias of the core one.
func TestHistory_ForbiddenAliasesCore(t *testing.T) {
	require.ErrorIs(t, ErrForbidden, core.ErrForbidden)
	require.ErrorIs(t, core.ErrForbidden, ErrForbidden)
}

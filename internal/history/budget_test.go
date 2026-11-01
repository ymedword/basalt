package history

// 本文件覆盖 T053 的**检索预算**：命中数超过本页上限时必须显式标注截断
// （设计 §4 的 `TestHistory_SearchBudgetTruncationMarked`），恰好在边界时不得
// 虚标，`limit` 缺省与上限按契约取值，探测行（`limit+1`）必须真的下发给端口。
//
// This file covers the search budget: truncation is marked explicitly, never invented
// at the exact boundary, and the limit+1 probe really reaches the store.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// seedMatching 造 n 条命中关键词的消息，时间从 seedTime 起逐分钟递减。
// seedMatching seeds n messages that all match the keyword.
func seedMatching(n int) []fakeMessage {
	out := make([]fakeMessage, 0, n)
	for i := range n {
		out = append(out, fakeMessage{
			id:        fmt.Sprintf("%d", 100+i),
			sessionID: "sess-budget",
			runID:     fmt.Sprintf("run-%d", i),
			callerID:  "cal_1",
			agent:     "ops",
			role:      "user",
			content:   fmt.Sprintf("第 %d 次发布前备份 audit.db", i),
			createdAt: seedTime.Add(-time.Duration(i) * time.Minute),
		})
	}
	return out
}

// TestHistory_SearchBudgetTruncationMarked 保护"超预算必须显式标注"这条不变量。
//
// 逆改点（实现怎么改它会红）：
//   - 向端口请求 `limit` 而不是 `limit+1` → 超额用例的 `Truncated` 恒为 false；
//   - 裁到上限却不置 `Truncated` → 超额用例的标注断言失败（调用方会以为已穷尽）；
//   - 在恰好等于上限时也置 `Truncated` → 边界用例虚报"还有更多"；
//   - 对 `limit` 做 1..200 的静默夹取 → 上限用例拿不到 201 条探测行。
//
// This test pins explicit truncation marking: over the page cap must say so, exactly
// at the cap must not, and the limit+1 probe must reach the port unchanged.
func TestHistory_SearchBudgetTruncationMarked(t *testing.T) {
	principal := callerPrincipal("cal_1", core.ScopeSessionsRead)
	ctx := context.Background()

	cases := []struct {
		name          string
		seed          int
		limit         int
		wantHits      int
		wantTruncated bool
		wantProbe     int
	}{
		{name: "over the page cap is marked", seed: 5, limit: 2, wantHits: 2, wantTruncated: true, wantProbe: 3},
		{name: "exactly at the cap is not marked", seed: 2, limit: 2, wantHits: 2, wantTruncated: false, wantProbe: 3},
		{name: "under the cap is not marked", seed: 1, limit: 2, wantHits: 1, wantTruncated: false, wantProbe: 3},
		{name: "zero limit uses the contract default", seed: 5, limit: 0, wantHits: 5, wantTruncated: false, wantProbe: defaultSearchLimit + 1},
		{name: "ceiling limit still probes one more row", seed: 5, limit: maxSearchLimit, wantHits: 5, wantTruncated: false, wantProbe: maxSearchLimit + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeMessageStore(40, seedMatching(tc.seed)...)
			searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

			result, err := searcher.Search(ctx, principal, HistoryQuery{Keywords: "备份", Limit: tc.limit})
			require.NoError(t, err)
			require.Len(t, result.Hits, tc.wantHits)
			require.Equal(t, tc.wantTruncated, result.Truncated, "截断标注必须与是否穷尽一致")
			require.Equal(t, tc.wantProbe, store.lastQ.Limit,
				"端口收到的行数上限必须是 limit+1（不得静默夹取）")
		})
	}
}

// TestHistory_SearchBudgetEmptyPageIsAnArray 保护契约的 `hits` 形状：零命中返回
// 空切片而不是 nil——nil 会被 JSON 编码成 `null`，与契约的数组类型不符。
//
// 逆改点：直接返回 nil 切片 → `require.NotNil` 失败。
//
// This test pins the hits array shape: an empty page is an empty slice, never nil.
func TestHistory_SearchBudgetEmptyPageIsAnArray(t *testing.T) {
	store := newFakeMessageStore(40, seedMatching(1)...)
	searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

	result, err := searcher.Search(context.Background(),
		callerPrincipal("cal_1", core.ScopeSessionsRead), HistoryQuery{Keywords: "不存在的关键词"})
	require.NoError(t, err)
	require.NotNil(t, result.Hits, "零命中必须是空数组而不是 nil")
	require.Empty(t, result.Hits)
	require.False(t, result.Truncated)
}

// TestHistory_SearchBudgetKeepsNewestFirst 保护排序口径：本页按时间倒序、同刻按
// message_id 倒序截取，截断取的是**最新的**那一页而不是最旧的。
//
// 逆改点：把截断放在排序之前（或取尾部）→ 首条不是最新消息，用例失败。
//
// This test pins that truncation keeps the newest page.
func TestHistory_SearchBudgetKeepsNewestFirst(t *testing.T) {
	store := newFakeMessageStore(40, seedMatching(3)...)
	searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

	result, err := searcher.Search(context.Background(),
		callerPrincipal("cal_1", core.ScopeSessionsRead), HistoryQuery{Keywords: "备份", Limit: 2})
	require.NoError(t, err)
	require.True(t, result.Truncated)
	require.Equal(t, "100", result.Hits[0].MessageID, "本页第一条必须是最新消息")
	require.Equal(t, "101", result.Hits[1].MessageID)
}

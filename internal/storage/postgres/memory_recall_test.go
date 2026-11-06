//go:build integration

package postgres

// 本文件是 US-22 T019 的有库用例：召回面的**派生复核**（C-31 / FR-22-13）与两条存储
// 侧写入回归（`auto_extract` 闭集、`valid_to` 落库）。
//
// 判据：三个 SQL 入口（`RecallByKeyword`、无向量关键词回退、向量路径）共用同一段
// `derivedLiveFilterSQL`——沿 `memory_candidate_sources` 递归取来源闭包，任一来源非
// `active` 或已过期 → 派生条目退出召回；传递链一并退出；深度到 8 仍未走完 → fail-closed；
// 显式条目（无来源行）零影响；过滤在排序与 top-k **之前**。
//
// 每个用例注释里的「逆改点」是判别力来源：从共享片段里删掉对应分支，这条用例必须变红。
// Database-backed cases for the recall-side derived-entry re-check and the two write
// regressions (auto_extract closure, valid_to persistence).

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// rrExtractionID 是来源关系行的外键载体（同一用例内唯一，schema 每个用例独立）。
// rrExtractionID backs the memory_candidate_sources foreign key inside one test schema.
const rrExtractionID = "ext-recall"

// rrStore 建隔离 schema 并种一条已成功的抽取行（来源关系行的 `extraction_id` 外键指向它）。
func rrStore(t *testing.T) (*DB, *MemoryStore) {
	t.Helper()
	db, store := memTestStore(t)
	mcSeedExtraction(t, db, rrExtractionID, "run-recall", "caller-recall", "")
	return db, store
}

// rrDerive 写一行来源关系：entryID 的（直接）来源是 sourceID。
func rrDerive(t *testing.T, db *DB, entryID, sourceID int64) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO memory_candidate_sources (entry_id, source_entry_id, extraction_id, created_at)
		VALUES ($1, $2, $3, $4)`, entryID, sourceID, rrExtractionID, memNow.UnixMilli())
	require.NoError(t, err)
}

// rrSeed 是归档区一行：state 由用例给定，来源缺省为普通写入；derived 标记表示它按
// `auto_extract` 落库（与提交事务同形）。
func rrSeed(t *testing.T, db *DB, agent, content, state string, derived bool, validTo any) int64 {
	t.Helper()
	source := memorySourceSaveMemory
	if derived {
		source = memorySourceAutoExtract
	}
	return memSeed(t, db, memSeedRow{
		scope: "agent", agentName: agent, section: "archive",
		content: content, state: state, source: source, validTo: validTo,
	})
}

// rrKill 把一行的状态迁到已删除（模拟来源被管理员删除）。
func rrKill(t *testing.T, db *DB, id int64) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`UPDATE memory_entries SET state = $2 WHERE id = $1`, id, memoryStateDeleted)
	require.NoError(t, err)
}

// rrRecallKeyword 走 RecallByKeyword（recall_memory(mode=keyword) 的真实存储入口）。
func rrRecallKeyword(t *testing.T, store *MemoryStore, agent, query string) []string {
	t.Helper()
	hits, err := store.RecallByKeyword(context.Background(), core.Scope{Agent: agent}, query)
	require.NoError(t, err)
	return hits
}

// rrRecallKeywordEntries 走 RecallEntries 的无向量关键词回退。
func rrRecallKeywordEntries(t *testing.T, store *MemoryStore, agent, query string) []memory.MemoryEntry {
	t.Helper()
	entries, err := store.RecallEntries(context.Background(), memory.RecallQuery{
		Principal: core.LocalAdminPrincipal(),
		Scope:     core.Scope{Agent: agent},
		Query:     query,
		Mode:      memory.RecallModeKeyword,
	}, nil, "")
	require.NoError(t, err)
	return entries
}

// rrRecallSemantic 走 RecallEntries 的向量路径；limit <= 0 表示不截断。
func rrRecallSemantic(t *testing.T, store *MemoryStore, agent string, vector []float32, limit int) []memory.MemoryEntry {
	t.Helper()
	entries, err := store.RecallEntries(context.Background(), memory.RecallQuery{
		Principal: core.LocalAdminPrincipal(),
		Scope:     core.Scope{Agent: agent},
		Query:     "任意",
		Mode:      memory.RecallModeSemantic,
		Limit:     limit,
	}, vector, "text-embedding-v4")
	require.NoError(t, err)
	return entries
}

// rrIDs 取条目主键（顺序即返回顺序）。
func rrIDs(entries []memory.MemoryEntry) []int64 {
	ids := make([]int64, len(entries))
	for i, entry := range entries {
		ids[i] = entry.ID
	}
	return ids
}

// TestUS22_Recall_DerivedSourceDeleted_Keyword 守 C-31：来源 A 被删除后，派生条目 B 不得
// 再从 `RecallByKeyword`（`recall_memory(mode=keyword)` 的真实存储入口）返回——只检查
// B 自己是 active 不够，必须复核来源闭包。
// 逆改点：从 recallByKeywordSQL 删掉 `derivedLiveFilterSQL` → 删除来源后的断言返回 B → 红。
func TestUS22_Recall_DerivedSourceDeleted_Keyword(t *testing.T) {
	db, store := rrStore(t)
	sourceID := rrSeed(t, db, "recall", "来源甲-记忆", memoryStateActive, false, nil)
	derivedID := rrSeed(t, db, "recall", "派生乙-记忆", memoryStateActive, true, nil)
	rrSeed(t, db, "recall", "对照-记忆", memoryStateActive, false, nil)
	rrDerive(t, db, derivedID, sourceID)

	require.Equal(t, []string{"来源甲-记忆", "派生乙-记忆", "对照-记忆"},
		rrRecallKeyword(t, store, "recall", "记忆"), "来源有效时派生条目照常召回（夹具先说清楚）")

	rrKill(t, db, sourceID)
	require.Equal(t, []string{"对照-记忆"}, rrRecallKeyword(t, store, "recall", "记忆"),
		"来源被删除后，派生条目不得再被关键词入口召回")
}

// TestUS22_Recall_DerivedSourceDeleted_LegacyKeyword 守 C-31：无向量时 RecallEntries 退回
// 关键词路径，派生复核同样生效——只改语义路径会漏掉这条回退。
// 逆改点：从 recallEntriesByKeywordSQL 删掉共享片段 → 删除来源后 B 仍在结果里 → 红。
func TestUS22_Recall_DerivedSourceDeleted_LegacyKeyword(t *testing.T) {
	db, store := rrStore(t)
	sourceID := rrSeed(t, db, "recall", "来源甲-记忆", memoryStateActive, false, nil)
	derivedID := rrSeed(t, db, "recall", "派生乙-记忆", memoryStateActive, true, nil)
	controlID := rrSeed(t, db, "recall", "对照-记忆", memoryStateActive, false, nil)
	rrDerive(t, db, derivedID, sourceID)

	require.Equal(t, []int64{sourceID, derivedID, controlID},
		rrIDs(rrRecallKeywordEntries(t, store, "recall", "记忆")))

	rrKill(t, db, sourceID)
	require.Equal(t, []int64{controlID}, rrIDs(rrRecallKeywordEntries(t, store, "recall", "记忆")),
		"无向量关键词回退同样执行派生复核")
}

// TestUS22_Recall_DerivedSourceDeleted_Semantic 守 C-31：向量路径在**排序与 top-k 之前**
// 执行派生复核。派生条目是最相似的一条（limit=1 时本应是第一名），来源删除后必须退出，
// 且不得占名额挤掉有效条目。
// 逆改点：把过滤挪到 top-k 之后（或删掉向量路径的共享片段）→ limit=1 仍返回派生条目 → 红。
func TestUS22_Recall_DerivedSourceDeleted_Semantic(t *testing.T) {
	db, store := rrStore(t)
	memSeedGeneration(t, db, 1, "active", "text-embedding-v4", 512)
	query := knVector(0.25)

	sourceID := rrSeed(t, db, "recall", "来源甲-记忆", memoryStateActive, false, nil)
	derivedID := rrSeed(t, db, "recall", "派生乙-记忆", memoryStateActive, true, nil)
	controlID := rrSeed(t, db, "recall", "对照-记忆", memoryStateActive, false, nil)
	rrDerive(t, db, derivedID, sourceID)
	// 派生条目的向量与查询完全相同（cosine 分 1.0），对照条目明显更远。
	memSeedEmbedding(t, db, derivedID, 1, query)
	memSeedEmbedding(t, db, controlID, 1, knVector(0.9))

	require.Equal(t, []int64{derivedID}, rrIDs(rrRecallSemantic(t, store, "recall", query, 1)),
		"来源有效时派生条目是第一名（夹具先说清楚）")

	rrKill(t, db, sourceID)
	require.Equal(t, []int64{controlID}, rrIDs(rrRecallSemantic(t, store, "recall", query, 1)),
		"派生复核必须在排序与 top-k 之前：失效派生条目不占名额，也不挤掉有效条目")
}

// TestUS22_Recall_DerivedSourceExpired 守 C-31 的时间分支：来源仍 `active` 但 `valid_to`
// 已过（≤ 本次召回时刻）时，派生条目同样停止召回——与生效层判据用同一个时间参数。
// 逆改点：从共享片段删掉 `src.valid_to IS NOT NULL AND src.valid_to <= $4` → 本用例红。
func TestUS22_Recall_DerivedSourceExpired(t *testing.T) {
	db, store := rrStore(t)
	expiredAt := memNow.Add(-time.Minute).UnixMilli()
	// valid_to 必须晚于 valid_from（schema CHECK）：来源一小时前写入、一分钟前到期。
	sourceID := memSeed(t, db, memSeedRow{
		scope: "agent", agentName: "recall", section: "archive", content: "来源甲-记忆",
		createdAt: memNow.Add(-time.Hour).UnixMilli(), validTo: expiredAt,
	})
	derivedID := rrSeed(t, db, "recall", "派生乙-记忆", memoryStateActive, true, nil)
	controlID := rrSeed(t, db, "recall", "对照-记忆", memoryStateActive, false, nil)
	rrDerive(t, db, derivedID, sourceID)

	require.Equal(t, []string{"对照-记忆"}, rrRecallKeyword(t, store, "recall", "记忆"),
		"来源到期后，派生条目不得再被召回（来源自身也因到期不进结果）")
	require.Equal(t, []int64{controlID}, rrIDs(rrRecallKeywordEntries(t, store, "recall", "记忆")))
}

// TestUS22_Recall_DerivedChainTransitive 守 C-31 的递归语义：A → B → C 时删除 A 让 B 与 C
// 一起退出召回；只查直接来源会漏掉更深一层的 C。
// 逆改点：把递归闭包换成「只查直接来源」→ C（二级派生）仍被返回 → 红。
func TestUS22_Recall_DerivedChainTransitive(t *testing.T) {
	db, store := rrStore(t)
	rootID := rrSeed(t, db, "recall", "链首-记忆", memoryStateActive, false, nil)
	midID := rrSeed(t, db, "recall", "链中-记忆", memoryStateActive, true, nil)
	leafID := rrSeed(t, db, "recall", "链尾-记忆", memoryStateActive, true, nil)
	rrSeed(t, db, "recall", "显式-记忆", memoryStateActive, false, nil)
	rrDerive(t, db, midID, rootID)
	rrDerive(t, db, leafID, midID)

	require.Equal(t, []string{"链首-记忆", "链中-记忆", "链尾-记忆", "显式-记忆"},
		rrRecallKeyword(t, store, "recall", "记忆"), "闭包全部有效时三级都召得回")

	rrKill(t, db, rootID)
	require.Equal(t, []string{"显式-记忆"}, rrRecallKeyword(t, store, "recall", "记忆"),
		"链首失效 → 一级与二级派生一起退出（递归覆盖「继续派生」）")
}

// TestUS22_Recall_DepthLimitFailsClosed 守 C-31 的防御性上限：来源链恰为 8 跳的条目可以
// 召回；到深度 8 仍存在更深来源的条目按失效处理（fail-closed），不返回无法证明来源
// 有效的条目。
// 逆改点：删掉 `c.depth >= 8 AND EXISTS (...)` 分支 → 链首 E0 被返回 → 红。
func TestUS22_Recall_DepthLimitFailsClosed(t *testing.T) {
	db, store := rrStore(t)
	const chain = 10 // E0 的链是 9 跳：超过上限；E1 的链恰为 8 跳：在上限内。

	ids := make([]int64, chain)
	for i := 0; i < chain; i++ {
		ids[i] = rrSeed(t, db, "recall", fmt.Sprintf("链%02d-记忆", i), memoryStateActive, true, nil)
	}
	for i := 0; i < chain-1; i++ {
		rrDerive(t, db, ids[i], ids[i+1])
	}
	rrSeed(t, db, "recall", "显式-记忆", memoryStateActive, false, nil)

	hits := rrRecallKeyword(t, store, "recall", "记忆")
	require.NotContains(t, hits, "链00-记忆", "深度 8 仍有更深来源 → fail-closed 排除")
	require.Contains(t, hits, "链01-记忆", "恰为 8 跳的链仍可召回（上限是 8，不是 7）")
	require.Len(t, hits, chain, "其余 9 条链节点与显式对照都在")
}

// TestUS22_Recall_ExplicitEntryUnaffected 守 C-31 的边界：无来源行的显式条目与「来源仍
// 有效」的派生条目都不受派生复核影响——只有来源失效的派生条目退出，三个入口一致。
// 逆改点：把判据改成 `EXISTS (来源行)`（要求所有条目都有活来源）→ 显式条目消失 → 红。
func TestUS22_Recall_ExplicitEntryUnaffected(t *testing.T) {
	db, store := rrStore(t)
	memSeedGeneration(t, db, 1, "active", "text-embedding-v4", 512)

	liveSourceID := rrSeed(t, db, "recall", "活来源-记忆", memoryStateActive, false, nil)
	liveDerivedID := rrSeed(t, db, "recall", "活派生-记忆", memoryStateActive, true, nil)
	rrDerive(t, db, liveDerivedID, liveSourceID)
	deadSourceID := rrSeed(t, db, "recall", "死来源-记忆", memoryStateDeleted, false, nil)
	deadDerivedID := rrSeed(t, db, "recall", "死派生-记忆", memoryStateActive, true, nil)
	rrDerive(t, db, deadDerivedID, deadSourceID)
	explicitID := rrSeed(t, db, "recall", "显式-记忆", memoryStateActive, false, nil)

	for _, entryID := range []int64{liveSourceID, liveDerivedID, deadDerivedID, explicitID} {
		memSeedEmbedding(t, db, entryID, 1, knVector(0.2))
	}

	wantKeyword := []string{"活来源-记忆", "活派生-记忆", "显式-记忆"}
	require.Equal(t, wantKeyword, rrRecallKeyword(t, store, "recall", "记忆"))
	require.Equal(t, []int64{liveSourceID, liveDerivedID, explicitID},
		rrIDs(rrRecallKeywordEntries(t, store, "recall", "记忆")))

	wantSemantic := []int64{liveSourceID, liveDerivedID, explicitID}
	require.ElementsMatch(t, wantSemantic, rrIDs(rrRecallSemantic(t, store, "recall", knVector(0.2), 0)),
		"向量路径同样只排除死派生；显式条目与活派生都在")
}

// TestUS22_Store_SourceAutoExtractAllowed 守 FR-22-06 的写入口：`auto_extract` 必须同时
// 在 DB CHECK 与 Go 侧闭集里——只改数据库约束会被 `memoryEntrySource` 拒绝，提交事务
// 因此无法落候选。
// 逆改点：从 memoryEntrySource 的闭集删掉 memorySourceAutoExtract → 写入报「不在闭集内」→ 红。
func TestUS22_Store_SourceAutoExtractAllowed(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)

	id, err := store.InsertEntry(ctx, memory.WithSource(memory.MemoryEntry{
		Scope:   core.Scope{Agent: "auto"},
		Section: core.SectionArchive,
		Content: "自动抽取来源",
		State:   memoryStateActive,
	}, memory.SourceAutoExtract))
	require.NoError(t, err, "auto_extract 必须在 Go 侧来源闭集内")
	require.Equal(t, memorySourceAutoExtract, memSourceOf(t, db, id))

	loaded, found, err := store.LoadEntry(ctx, id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memory.SourceAutoExtract, loaded.Source(), "回读必须还原库里的来源")
}

// TestUS22_Store_InsertRowWritesValidTo 守 FR-22-05 的时间列：`insertMemoryRow` 必须把
// `memoryRow.validTo` 写进 `valid_to`（临时候选据此到期），零值落 NULL 保持既有写入路径
// （Append / InsertEntry / ReplaceEntry）行为不变。
// 逆改点：从 INSERT 的列清单 / 参数里删掉 valid_to → 第一段断言读到 NULL → 红。
func TestUS22_Store_InsertRowWritesValidTo(t *testing.T) {
	ctx := context.Background()
	db, _ := memTestStore(t)
	validTo := memNow.Add(48 * time.Hour).UnixMilli()

	var withExpiry, withoutExpiry int64
	err := db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		withExpiry, err = insertMemoryRow(ctx, tx, memoryRow{
			scopeCol: "agent", agentName: "exp", section: core.SectionArchive,
			content: "临时约束", state: memoryStateCandidate,
			source: memorySourceAutoExtract, createdAt: memNow.UnixMilli(), validTo: validTo,
		})
		if err != nil {
			return err
		}
		withoutExpiry, err = insertMemoryRow(ctx, tx, memoryRow{
			scopeCol: "agent", agentName: "exp", section: core.SectionArchive,
			content: "长期记忆", state: memoryStateActive,
			source: memorySourceSaveMemory, createdAt: memNow.UnixMilli(),
		})
		return err
	})
	require.NoError(t, err)

	var got sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT valid_to FROM memory_entries WHERE id = $1`, withExpiry).Scan(&got))
	require.True(t, got.Valid, "声明了到期时刻的行必须写 valid_to")
	require.Equal(t, validTo, got.Int64)

	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT valid_to FROM memory_entries WHERE id = $1`, withoutExpiry).Scan(&got))
	require.False(t, got.Valid, "零值 validTo 落 NULL（既有写入路径行为不变）")
}

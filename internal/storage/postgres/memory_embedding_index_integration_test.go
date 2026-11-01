//go:build integration

package postgres

// 本文件是记忆向量**写入侧与缺口判定**的有库用例（V-5）：upsert 幂等、代际三校验、
// 缺口列表的生效层判据，以及「空召回」的两种结局——确实没有可召回记忆 vs 有应索引
// 记忆但索引未就绪。
// These are the database-backed cases for memory vector writes and the gap probe:
// idempotent upsert, generation checks, the effective-layer predicate of the gap list,
// and the two meanings of an empty recall.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/memory"
)

// memVectorRows 数某代际的向量行数。
func memVectorRows(t *testing.T, db *DB, generation int) int {
	t.Helper()
	var n int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM memory_embeddings WHERE generation = $1`, generation).Scan(&n))
	return n
}

// TestMemoryVectorIndex_WriteIsIdempotentAndUsesGenerationShape 守两件事：同一
// (entry_id, generation) 重复写入只留一行（upsert 覆盖而不是插入第二份），模型与维度
// 取自**代际行**而不是调用方（调用方只给向量）。
// 逆改点：把 ON CONFLICT DO UPDATE 改成 DO NOTHING → 第二次写入的影响行数断言 / 向量内容
// 仍是旧值；把 model 改成调用方参数 → 代际行断言红。
func TestMemoryVectorIndex_WriteIsIdempotentAndUsesGenerationShape(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	memSeedGeneration(t, db, 1, "active", "text-embedding-v4", knowledge.VectorDimensions)

	first := memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "向量甲"})
	second := memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "向量乙"})
	entries := []memory.MemoryEntry{{ID: first}, {ID: second}}

	require.NoError(t, store.WriteMemoryEmbeddings(ctx, 1, entries,
		[][]float32{knVector(1), knVector(2)}))
	require.Equal(t, 2, memVectorRows(t, db, 1))

	// 第二次写入换成另一组向量：行数不变，内容被覆盖（upsert 不是"已存在就跳过"）。
	require.NoError(t, store.WriteMemoryEmbeddings(ctx, 1, entries,
		[][]float32{knVector(3), knVector(4)}))
	require.Equal(t, 2, memVectorRows(t, db, 1))

	var (
		model      string
		dimensions int
		updated    int
	)
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT model, dimensions,
		        CASE WHEN embedding = $2::vector THEN 1 ELSE 0 END
		   FROM memory_embeddings WHERE entry_id = $1 AND generation = 1`,
		first, mustVectorLiteral(t, knVector(3))).Scan(&model, &dimensions, &updated))
	require.Equal(t, "text-embedding-v4", model, "模型取自代际行")
	require.Equal(t, knowledge.VectorDimensions, dimensions, "维度取自代际行")
	require.Equal(t, 1, updated, "重复写入必须覆盖向量")
}

// mustVectorLiteral 把向量编成 SQL 字面量（仅测试用）。
func mustVectorLiteral(t *testing.T, vector []float32) string {
	t.Helper()
	literal, err := vectorLiteral(vector)
	require.NoError(t, err)
	return literal
}

// TestMemoryVectorIndex_WriteRefusesRetiredAndMissingGeneration 守代际三校验里的两条：
// 退役代际不再接收向量（与 `indexStore.Write` 同判据），不存在的代际报错而不是静默写零行；
// 维度与冻结宽度不符整批回滚。
// 逆改点：删掉退役判定 → 退役代际也能拿到向量（历史代际被污染）；删掉维度判定 → 走
// vector(512) 转型报错，错误面不再可分类。
func TestMemoryVectorIndex_WriteRefusesRetiredAndMissingGeneration(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	memSeedGeneration(t, db, 1, "active", "text-embedding-v4", knowledge.VectorDimensions)
	memSeedGeneration(t, db, 2, "retired", "text-embedding-v4", knowledge.VectorDimensions)
	entryID := memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "甲"})
	entries := []memory.MemoryEntry{{ID: entryID}}

	err := store.WriteMemoryEmbeddings(ctx, 2, entries, [][]float32{knVector(1)})
	require.ErrorIs(t, err, errIndexStateConflict, "退役代际拒写")
	require.Zero(t, memVectorRows(t, db, 2))

	err = store.WriteMemoryEmbeddings(ctx, 9, entries, [][]float32{knVector(1)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "不存在")

	err = store.WriteMemoryEmbeddings(ctx, 1, entries, [][]float32{{0.1, 0.2}})
	require.ErrorIs(t, err, errIndexStateConflict, "维度与冻结宽度不符")
	require.Zero(t, memVectorRows(t, db, 1))
}

// TestMemoryVectorIndex_ListUnindexedArchiveEntriesUsesEffectiveLayer 守缺口列表只拉
// **生效归档**条目：候选 / 已删 / 到期 / 核心区 / 已有向量的行都不进回填，跨分层按 id
// 升序返回，limit 必须为正。
// 逆改点：删掉任一判据（state / section / valid_to / NOT EXISTS）→ 对应条目出现在列表里。
func TestMemoryVectorIndex_ListUnindexedArchiveEntriesUsesEffectiveLayer(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	memSeedGeneration(t, db, 1, "active", "text-embedding-v4", knowledge.VectorDimensions)

	globalID := memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "全局待索引"})
	agentID := memSeed(t, db, memSeedRow{scope: "agent", agentName: "demo", section: "archive", content: "Agent 待索引"})
	indexedID := memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "已有向量"})
	memSeedEmbedding(t, db, indexedID, 1, knVector(9))
	memSeed(t, db, memSeedRow{scope: "global", section: "core", content: "核心区"})
	memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "候选", state: memoryStateCandidate})
	memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "已删", state: memoryStateDeleted})
	// 到期行：valid_to 必须晚于 valid_from（schema CHECK），且不晚于夹具时钟。
	memSeed(t, db, memSeedRow{
		scope: "global", section: "archive", content: "到期",
		createdAt: memNow.UnixMilli() - 10_000, validTo: memNow.UnixMilli() - 1,
	})

	entries, err := store.ListUnindexedArchiveEntries(ctx, 1, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2, "只有两条生效归档条目缺向量")
	require.Equal(t, []int64{globalID, agentID}, []int64{entries[0].ID, entries[1].ID},
		"跨分层、按 id 升序")

	first, err := store.ListUnindexedArchiveEntries(ctx, 1, 1)
	require.NoError(t, err)
	require.Len(t, first, 1, "limit 生效")
	require.Equal(t, globalID, first[0].ID)

	_, err = store.ListUnindexedArchiveEntries(ctx, 1, 0)
	require.Error(t, err, "无界查询必须拒绝（回填按批推进）")

	// 补上向量之后缺口消失：判据按代际而非"有没有任何向量"。
	require.NoError(t, store.WriteMemoryEmbeddings(ctx, 1, []memory.MemoryEntry{{ID: globalID}},
		[][]float32{knVector(1)}))
	entries, err = store.ListUnindexedArchiveEntries(ctx, 1, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, agentID, entries[0].ID)
}

// TestMemoryVectorIndex_ListUnindexedEntriesByContentScopesAndDedupes 守写后补索引的
// 目标集合：按（分层, 分区, 正文哈希）取同文行，不同分层 / 不同正文不混；写完即消失。
// 逆改点：去掉 scope 条件 → 全局层同文行被一并写进 Agent 层的补索引批次。
func TestMemoryVectorIndex_ListUnindexedEntriesByContentScopesAndDedupes(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	memSeedGeneration(t, db, 1, "active", "text-embedding-v4", knowledge.VectorDimensions)
	scope := core.Scope{Agent: "demo"}

	first := memSeed(t, db, memSeedRow{scope: "agent", agentName: "demo", section: "archive", content: "同一句话"})
	second := memSeed(t, db, memSeedRow{scope: "agent", agentName: "demo", section: "archive", content: "同一句话"})
	memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "同一句话"})
	memSeed(t, db, memSeedRow{scope: "agent", agentName: "demo", section: "archive", content: "别的话"})

	entries, err := store.ListUnindexedEntriesByContent(ctx, 1, scope, core.SectionArchive, "  同一句话  ")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, []int64{first, second}, []int64{entries[0].ID, entries[1].ID},
		"同文两行一次补齐、不跨分层、正文按规范化比较")

	require.NoError(t, store.WriteMemoryEmbeddings(ctx, 1, entries,
		[][]float32{knVector(1), knVector(2)}))
	entries, err = store.ListUnindexedEntriesByContent(ctx, 1, scope, core.SectionArchive, "同一句话")
	require.NoError(t, err)
	require.Empty(t, entries, "已索引行不再出现在补索引集合里")
}

// TestMemoryVectorIndex_EmptyRecallDistinguishesGapFromNoMemory 守 V-5 的核心区分：
//   - 没有生效归档条目（或全被索引）→ 空结果 + nil（确实没有可召回记忆）；
//   - 有生效归档条目却缺向量 → `memory.ErrIndexNotReady`（不得伪装成无匹配）；
//   - 补上向量之后同一查询能召回该条（写入侧真的被读侧看见了）。
//
// 逆改点：把缺口检测删掉 → 第二种结局变成空结果 + nil，本用例红；把检测条件放宽到
// 不看 state → 已删条目会让"确实没有"变成"索引未就绪"，第三种断言红。
func TestMemoryVectorIndex_EmptyRecallDistinguishesGapFromNoMemory(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	memSeedGeneration(t, db, 1, "active", "text-embedding-v4", knowledge.VectorDimensions)

	query := memory.RecallQuery{Principal: memCaller("sessions:read"), Query: "查询"}
	// 已删条目不是"应索引记忆"：没有任何生效归档条目时，空召回是正常结局。
	memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "已删", state: memoryStateDeleted})
	entries, err := store.RecallEntries(ctx, query, knVector(1), "text-embedding-v4")
	require.NoError(t, err)
	require.Empty(t, entries, "确实没有可召回记忆")

	// 出现一条生效归档条目但尚无向量：召回必须显式报缺口。
	entryID := memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "应被索引"})
	_, err = store.RecallEntries(ctx, query, knVector(1), "text-embedding-v4")
	require.ErrorIs(t, err, memory.ErrIndexNotReady, "有应索引记忆时不得伪装成无匹配")
	require.True(t, memory.IsIndexNotReady(err))

	// 补上向量：同一查询能召回（没有相似度阈值，限值为 0 即全量）。
	require.NoError(t, store.WriteMemoryEmbeddings(ctx, 1, []memory.MemoryEntry{{ID: entryID}},
		[][]float32{knVector(5)}))
	entries, err = store.RecallEntries(ctx, query, knVector(1), "text-embedding-v4")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, entryID, entries[0].ID)
}

// TestMemoryVectorIndex_EmbeddingGenerationReadsExplicitRow 守代际读取：按号取行、缺行
// 报错、非正号在触库前拒绝。
// TestMemoryVectorIndex_EmbeddingGenerationReadsExplicitRow pins the explicit read.
func TestMemoryVectorIndex_EmbeddingGenerationReadsExplicitRow(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	memSeedGeneration(t, db, 3, "active", "text-embedding-v4", knowledge.VectorDimensions)

	generation, err := store.EmbeddingGeneration(ctx, 3)
	require.NoError(t, err)
	require.Equal(t, 3, generation.Generation)
	require.Equal(t, "text-embedding-v4", generation.Model)
	require.Equal(t, knowledge.VectorDimensions, generation.Dimensions)
	require.Equal(t, "active", generation.Status)

	_, err = store.EmbeddingGeneration(ctx, 4)
	require.Error(t, err)
	require.Contains(t, err.Error(), "不存在")

	_, err = store.EmbeddingGeneration(ctx, 0)
	require.Error(t, err)
}

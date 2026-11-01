//go:build integration

package postgres

// 本文件是 `knowledge.VectorIndex` 的 PostgreSQL 实现（`index.go` 的 `Write` /
// `Activate` / `Retire`，US-16 C-09 / T129）的真库判据。四条端口纪律各有一组具名用例：
//
//  1. **摄取只写向量**：`Write` 不改代际与版本状态，`Write` 之后版本仍是 `pending`。
//  2. **写入前核对归属与形状**：跨版本片段、维度不符、非有限值在写入前就被拒绝，且
//     **整批**拒绝——半批写入会让版本的向量数与片段数不等，错误随即出现在离原因很远
//     的地方（校验门）。
//  3. **`Activate` 不是校验门**：它只做存储层的完整性核对，再复用同一个单事务切换；
//     代际管理器（`GenerationManager`）的证据门在**上层**，本用例不冒充那一道。
//  4. **`Retire` 按版本绑定的代际退休整代**：未绑定的版本无从退休，active 代际只能经
//     `Activate` 移开。
//
// 逆改点（任一即红）：把 upsert 改成 `ON CONFLICT DO NOTHING` → 重试用例的
// "重写必须覆盖向量"断言红；把 `requireOwnedChunks` 挪到插入之后（或去掉）→ 跨版本
// 批次用例的"零行"断言红；`Activate` 去掉 `requireVersionVectorsComplete` → 不完整
// 版本也能切换，第一个 `errKnowledgeArtifactsIncomplete` 断言红；`Activate` 的完整性
// 核对若形同虚设，补全后的错误会是 incomplete 而不是 state conflict，第二条断言红；
// `Retire` 改读请求参数而不是版本行 → 绑定代际用例退休错行。
//
// These are the database-backed cases for the vector index port: batch rejection,
// retry convergence, activation completeness, and retire-by-bound-generation.

import (
	"context"
	"database/sql"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/knowledge"
)

// indexSeedGeneration 种一行指定状态的代际。迁移器不 seed 代际行（首代际由启动期代际
// 管理建立，见 `knowledge_store_test.go` 文件头），所以每个用例自己造需要的那一代。
// indexSeedGeneration inserts one generation row with the given status.
func indexSeedGeneration(t *testing.T, db *DB, generation int, status knowledge.GenerationStatus) {
	t.Helper()
	var activated, retired any
	switch status {
	case knowledge.GenerationActive:
		activated = knNow.UnixMilli()
	case knowledge.GenerationRetired:
		retired = knNow.UnixMilli()
	}
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO kb_index_generations
			(generation, embedding_model, dimensions, distance, status,
			 activated_at, retired_at, created_at)
		VALUES ($1, 'text-embedding-v4', $2, 'cosine', $3, $4, $5, $6)`,
		generation, knowledge.VectorDimensions, string(status), activated, retired,
		knNow.UnixMilli())
	require.NoError(t, err)
}

// TestIndexVector_WriteConvergesOnRetry：同一 (chunk_id, generation) 写两次必须收敛到
// 同一行并覆盖向量——重试与重切块都不能翻倍，也不能被静默丢弃。
func TestIndexVector_WriteConvergesOnRetry(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	index := NewIndexStore(h.db)
	job := h.startJob(t, "job_retry", "retry.md", "hash-retry")
	refs := h.seedChunks(t, job.VersionID, "回滚步骤：停写入", "再回放日志，最后核对计数")
	require.Len(t, refs, 2)

	readEmbedding := func(chunkID string) string {
		t.Helper()
		var text string
		require.NoError(t, h.db.pool.QueryRowContext(ctx, `
			SELECT embedding::text FROM kb_chunk_embeddings
			WHERE chunk_id = $1 AND generation = 1`, chunkID).Scan(&text))
		return text
	}

	require.NoError(t, index.Write(ctx, job.VersionID, refs,
		[][]float32{knVector(0.1), knVector(0.2)}, 1))
	first := readEmbedding(refs[0].ChunkID)

	require.NoError(t, index.Write(ctx, job.VersionID, refs,
		[][]float32{knVector(0.4), knVector(0.5)}, 1))
	require.NotEqual(t, first, readEmbedding(refs[0].ChunkID),
		"重写必须覆盖向量（`DO NOTHING` 会在此红）")

	var rows int
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunk_embeddings WHERE generation = 1`).Scan(&rows))
	require.Equal(t, len(refs), rows, "每个片段在代际 1 只有一行")

	// 摄取只写向量：代际与版本的状态都不因 `Write` 而变。
	var generationStatus, versionStatus string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT g.status, v.status
		FROM kb_index_generations g, kb_document_versions v
		WHERE g.generation = 1 AND v.version_id = $1`, job.VersionID).
		Scan(&generationStatus, &versionStatus))
	require.Equal(t, string(knowledge.GenerationActive), generationStatus, "写向量不碰代际状态")
	require.Equal(t, string(knowledge.VersionPending), versionStatus, "写向量不碰版本状态")
}

// TestIndexVector_WriteRefusesForeignChunks：片段必须属于该版本；只要有一条不属于，
// **整批**拒绝——否则版本会因别人的片段而"看起来完整"。
func TestIndexVector_WriteRefusesForeignChunks(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	index := NewIndexStore(h.db)
	mine := h.startJob(t, "job_mine", "mine.md", "hash-mine")
	theirs := h.startJob(t, "job_theirs", "theirs.md", "hash-theirs")
	myRefs := h.seedChunks(t, mine.VersionID, "我的片段")
	theirRefs := h.seedChunks(t, theirs.VersionID, "别人的片段")

	err := index.Write(ctx, mine.VersionID, theirRefs, [][]float32{knVector(0.5)}, 1)
	require.ErrorIs(t, err, errKnowledgeInvalidArgument)
	require.Contains(t, err.Error(), "不属于版本", "错误要指出归属不符，而不是让外键在写入时炸")

	err = index.Write(ctx, mine.VersionID,
		[]knowledge.ChunkRef{myRefs[0], theirRefs[0]},
		[][]float32{knVector(0.1), knVector(0.2)}, 1)
	require.ErrorIs(t, err, errKnowledgeInvalidArgument, "混批同样整批拒绝")

	var rows int
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunk_embeddings`).Scan(&rows))
	require.Zero(t, rows, "被拒批次不得留下任何一行（整批在同一事务里回滚）")
}

// TestIndexVector_WriteRefusesBadShape：参数与形状校验全部发生在任何写入之前，且每种
// 非法形态对应各自可辨认的错误——不交给数据库 CHECK 兜底。
func TestIndexVector_WriteRefusesBadShape(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	index := NewIndexStore(h.db)
	job := h.startJob(t, "job_shape", "shape.md", "hash-shape")
	refs := h.seedChunks(t, job.VersionID, "第一段", "第二段")
	require.Len(t, refs, 2)
	indexSeedGeneration(t, h.db, 2, knowledge.GenerationRetired)

	nanVector := knVector(0.1)
	nanVector[0] = float32(math.NaN())

	cases := []struct {
		name       string
		versionID  string
		chunks     []knowledge.ChunkRef
		vectors    [][]float32
		generation int
		want       error
		contains   string
	}{
		{
			name: "空版本标识", versionID: " ", chunks: refs[:1],
			vectors: [][]float32{knVector(0.1)}, generation: 1,
			want: errKnowledgeInvalidArgument,
		},
		{
			name: "代际号小于 1", versionID: job.VersionID, chunks: refs[:1],
			vectors: [][]float32{knVector(0.1)}, generation: 0,
			want: errKnowledgeInvalidArgument,
		},
		{
			name: "空片段集", versionID: job.VersionID, chunks: nil,
			vectors: nil, generation: 1,
			want: errKnowledgeInvalidArgument, contains: "没有片段可写",
		},
		{
			name: "片段与向量条数不符", versionID: job.VersionID, chunks: refs,
			vectors: [][]float32{knVector(0.1)}, generation: 1,
			want: errKnowledgeInvalidArgument,
		},
		{
			name: "片段缺少标识", versionID: job.VersionID,
			chunks:  []knowledge.ChunkRef{{ChunkID: " "}},
			vectors: [][]float32{knVector(0.1)}, generation: 1,
			want: errKnowledgeInvalidArgument, contains: "缺少标识",
		},
		{
			name: "向量宽度不符", versionID: job.VersionID, chunks: refs[:1],
			vectors: [][]float32{{0.1, 0.2, 0.3}}, generation: 1,
			want: errKnowledgeInvalidArgument, contains: "冻结宽度",
		},
		{
			name: "向量非有限值", versionID: job.VersionID, chunks: refs[:1],
			vectors: [][]float32{nanVector}, generation: 1,
			want: errKnowledgeInvalidArgument, contains: "不是有限值",
		},
		{
			name: "代际不存在", versionID: job.VersionID, chunks: refs[:1],
			vectors: [][]float32{knVector(0.1)}, generation: 99,
			want: knowledge.ErrNotFound,
		},
		{
			name: "代际已退休", versionID: job.VersionID, chunks: refs[:1],
			vectors: [][]float32{knVector(0.1)}, generation: 2,
			want: errIndexStateConflict, contains: "已退休",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := index.Write(ctx, tc.versionID, tc.chunks, tc.vectors, tc.generation)
			require.ErrorIs(t, err, tc.want)
			if tc.contains != "" {
				require.Contains(t, err.Error(), tc.contains)
			}
		})
	}

	var rows int
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunk_embeddings`).Scan(&rows))
	require.Zero(t, rows, "没有任何一种非法形态能留下向量行")
}

// TestIndexVector_ActivateRequiresCompleteVectors：向量不全不得切换代际；补全后完整性
// 核对必须真的通过（错误随即换成"已是 active"，而不是停在 incomplete）。
func TestIndexVector_ActivateRequiresCompleteVectors(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	index := NewIndexStore(h.db)
	job := h.startJob(t, "job_act", "act.md", "hash-act")
	refs := h.seedChunks(t, job.VersionID, "第一段", "第二段")
	require.Len(t, refs, 2)

	require.NoError(t, index.Write(ctx, job.VersionID, refs[:1], [][]float32{knVector(0.1)}, 1))
	err := index.Activate(ctx, job.VersionID, 1)
	require.ErrorIs(t, err, errKnowledgeArtifactsIncomplete, "向量不全不得切换代际")

	require.NoError(t, index.Write(ctx, job.VersionID, refs[1:], [][]float32{knVector(0.2)}, 1))
	err = index.Activate(ctx, job.VersionID, 1)
	require.ErrorIs(t, err, errIndexStateConflict,
		"补全后挡路的是「代际 1 已是 active」——这条错误证明完整性核对真的跑过")

	err = index.Activate(ctx, "version_missing", 1)
	require.ErrorIs(t, err, knowledge.ErrNotFound, "版本不存在必须报未找到")
}

// TestIndexVector_ActivateSwitchesGeneration：版本绑定的代际与请求不符即拒绝；相符时
// 复用同一个单事务切换（旧 active 退休、目标生效）。
func TestIndexVector_ActivateSwitchesGeneration(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	index := NewIndexStore(h.db)
	job := h.startJob(t, "job_switch", "switch.md", "hash-switch")
	refs := h.seedChunks(t, job.VersionID, "第一段", "第二段")

	indexSeedGeneration(t, h.db, 2, knowledge.GenerationBuilding)
	require.NoError(t, index.Write(ctx, job.VersionID, refs,
		[][]float32{knVector(0.1), knVector(0.2)}, 2))
	require.NoError(t, h.store.MarkIndexed(ctx, job.VersionID, knNow))
	_, err := h.store.ActivateVersion(ctx, job.VersionID, 2, knNow)
	require.NoError(t, err)

	err = index.Activate(ctx, job.VersionID, 1)
	require.ErrorIs(t, err, errKnowledgeStateConflict, "版本绑定代际 2，按代际 1 激活即拿错了号")

	require.NoError(t, index.Activate(ctx, job.VersionID, 2))

	statuses := map[int]string{}
	rows, err := h.db.pool.QueryContext(ctx, `
		SELECT generation, status FROM kb_index_generations ORDER BY generation`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			generation int
			status     string
		)
		require.NoError(t, rows.Scan(&generation, &status))
		statuses[generation] = status
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[int]string{1: "retired", 2: "active"}, statuses,
		"单事务切换：旧 active 退休、目标生效，中间不留两个 active")

	var activatedAt sql.NullInt64
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT activated_at FROM kb_index_generations WHERE generation = 2`).Scan(&activatedAt))
	require.True(t, activatedAt.Valid, "激活必须写 activated_at（冻结 CHECK 同样要求）")
}

// TestIndexVector_RetireUsesBoundGeneration：退休的是版本行上绑定的那一代，未绑定的
// 版本无从退休；行与向量保留，只改状态与时刻列。
func TestIndexVector_RetireUsesBoundGeneration(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	index := NewIndexStore(h.db)

	unbound := h.startJob(t, "job_unbound", "unbound.md", "hash-unbound")
	h.seedChunks(t, unbound.VersionID, "还没建索引")
	err := index.Retire(ctx, unbound.VersionID)
	require.ErrorIs(t, err, errKnowledgeInvalidArgument, "尚未绑定代际的版本不得退休")

	bound := h.startJob(t, "job_bound", "bound.md", "hash-bound")
	refs := h.seedChunks(t, bound.VersionID, "第一段")
	indexSeedGeneration(t, h.db, 2, knowledge.GenerationBuilding)
	require.NoError(t, index.Write(ctx, bound.VersionID, refs, [][]float32{knVector(0.3)}, 2))
	require.NoError(t, h.store.MarkIndexed(ctx, bound.VersionID, knNow))
	_, err = h.store.ActivateVersion(ctx, bound.VersionID, 2, knNow)
	require.NoError(t, err)

	require.NoError(t, index.Retire(ctx, bound.VersionID))
	var (
		status    string
		retiredAt sql.NullInt64
		vectors   int
	)
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status, retired_at,
		       (SELECT COUNT(*) FROM kb_chunk_embeddings WHERE generation = 2)
		FROM kb_index_generations WHERE generation = 2`).Scan(&status, &retiredAt, &vectors))
	require.Equal(t, string(knowledge.GenerationRetired), status)
	require.True(t, retiredAt.Valid, "退休必须写 retired_at（冻结 CHECK 同样要求）")
	require.Equal(t, len(refs), vectors, "退休不删除：行与向量保留到回退窗口结束")
}

// TestIndexVector_RetireRefusesActiveGeneration：active 代际只能经 `Activate` 移开，
// 直接退休必须拒绝——否则检索会突然失去唯一可见代际。
func TestIndexVector_RetireRefusesActiveGeneration(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	index := NewIndexStore(h.db)
	job := h.startJob(t, "job_active", "active.md", "hash-active")
	refs := h.seedChunks(t, job.VersionID, "第一段")
	require.NoError(t, index.Write(ctx, job.VersionID, refs, [][]float32{knVector(0.4)}, 1))
	require.NoError(t, h.store.MarkIndexed(ctx, job.VersionID, knNow))
	_, err := h.store.ActivateVersion(ctx, job.VersionID, 1, knNow)
	require.NoError(t, err)

	err = index.Retire(ctx, job.VersionID)
	require.ErrorIs(t, err, errIndexStateConflict, "active 代际不得直接退休")

	var status string
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT status FROM kb_index_generations WHERE generation = 1`).Scan(&status))
	require.Equal(t, string(knowledge.GenerationActive), status, "被拒的退休不得改动行")
}

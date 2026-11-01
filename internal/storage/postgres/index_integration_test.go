//go:build integration

package postgres

// 本文件是 C-09（代际切换必须先过校验）在**真库**上的判据（US-16 §4 的
// `TestIndex_SwitchRequiresValidation`，T129 的 `index.go` 首条）：代际管理器接真存储，
// 三关全部落在真实 PostgreSQL 上——
//
//  1. **证据门**：没有 `Validate` 签发的证据时，切换被拒且库里状态原样不动；
//  2. **一致性门**：混入别的模型的向量 → `Validate` 失败，代际仍是 `building`，
//     **不得**置 `active`；
//  3. **单事务切换 + 数据库兜底**：校验通过后一次切换把旧 active 退休、目标激活，
//     行不删除；绕过存储层想把旧代际改回 active 时，由部分唯一索引
//     `ux_kb_index_generations_active` 拒绝——"任一时刻至多一个 active"不只靠应用层
//     守规矩。
//
// 逆改点：删掉 `GenerationManager.Activate` 的 `!evidence.Passed()` 守卫 → ② 的
// `ErrorIs(ErrGenerationUnvalidated)` 与"gen1 仍是 active"同时红；
// 把 `GenerationVectorStats` 的模型 / 维度不一致谓词改成恒假 → ③ 的 `Validate` 通过、
// `evidence.Passed()` 为真，两条断言红。
// The three gates that keep an unvalidated generation from going live, all against the
// real schema: evidence, vector consistency, and the single-transaction switch with the
// partial unique index as the database-side backstop.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/knowledge"
)

// requireActiveGeneration 直接读库断言当前 active 代际号——不经被测存储的读路径，
// 免得"写对了但读错了"把状态断言带偏。
// requireActiveGeneration reads the active row straight from the database.
func requireActiveGeneration(t *testing.T, h *knHarness, generation int) {
	t.Helper()
	active, ok, err := NewIndexStore(h.db).ActiveGeneration(context.Background())
	require.NoError(t, err)
	require.True(t, ok, "真库里必须有一个 active 代际")
	require.Equal(t, generation, active.Generation)
	require.Equal(t, knowledge.GenerationActive, active.Status)
}

// TestIndex_SwitchRequiresValidation 走一遍完整生命周期：建代际 → 无证据被拒 →
// 校验失败 → 修正 → 校验通过 → 单事务切换 → 唯一索引兜底。
// TestIndex_SwitchRequiresValidation drives the lifecycle from Begin to the index backstop.
func TestIndex_SwitchRequiresValidation(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	// 管理器接真存储：本用例判的是 `index.go` 与冻结 DDL 的真实行为，不是替身。
	manager := knowledge.NewGenerationManager(knowledge.GenerationDeps{
		Store: NewIndexStore(h.db),
		Now:   func() time.Time { return knNow },
	})

	// ① 建 building 代际：号由 `max(generation) + 1` 分配（现有 active 为 1）。
	gen2, err := manager.Begin(ctx, knowledge.GenerationSpec{
		EmbeddingModel: "text-embedding-v4",
		Dimensions:     knowledge.VectorDimensions,
	})
	require.NoError(t, err)
	require.Equal(t, 2, gen2.Generation, "代际号严格递增，从 max+1 起")
	require.Equal(t, knowledge.GenerationBuilding, gen2.Status)
	require.Zero(t, gen2.ActivatedAt, "building 行的 activated_at 必须是 NULL")

	// ② 证据门：带代际号但未通过校验的证据必须被拒，库里状态原样不动。
	// 零值证据在管理器入口就被挡下；这里故意给出正确代际号，确保拒绝来自"没校验"
	// 而不是"代际号对不上"。
	_, err = manager.Activate(ctx, knowledge.GenerationValidation{Generation: gen2.Generation})
	require.ErrorIs(t, err, knowledge.ErrGenerationUnvalidated, "未经校验的代际不得激活")
	requireActiveGeneration(t, h, 1)
	stillBuilding, err := NewIndexStore(h.db).Generation(ctx, gen2.Generation)
	require.NoError(t, err)
	require.Equal(t, knowledge.GenerationBuilding, stillBuilding.Status)

	// ③ 一致性门：给代际 2 造两条向量，其中一条来自别的模型。
	job := h.startJob(t, "job_gen2", "gen2.md", knHash("代际 2 的片段"))
	refs := h.seedChunks(t, job.VersionID, "第一段", "第二段")
	writeVector := func(ref knowledge.ChunkRef, model string) {
		t.Helper()
		literal, err := vectorLiteral(knVector(0.5))
		require.NoError(t, err)
		_, err = h.db.pool.ExecContext(ctx, `
			INSERT INTO kb_chunk_embeddings (chunk_id, generation, embedding, model, dimensions, created_at)
			VALUES ($1, $2, $3::vector, $4, $5, $6)`,
			ref.ChunkID, gen2.Generation, literal, model, knowledge.VectorDimensions,
			knNow.UnixMilli())
		require.NoError(t, err)
	}
	writeVector(refs[0], "text-embedding-v4")
	writeVector(refs[1], "other-model")

	evidence, err := manager.Validate(ctx, gen2.Generation)
	require.ErrorIs(t, err, knowledge.ErrGenerationUnvalidated, "混入别的模型的向量必须校验失败")
	require.False(t, evidence.Passed(), "失败的校验不得签发证据")
	requireActiveGeneration(t, h, 1)
	require.Equal(t, knowledge.GenerationBuilding, mustGeneration(t, h.db, gen2.Generation).Status,
		"校验失败不得改变代际状态")

	// ④ 修正为同一模型后校验通过 → 单事务切换：旧 active 退休、目标激活、行不删除。
	_, err = h.db.pool.ExecContext(ctx, `
		UPDATE kb_chunk_embeddings SET model = 'text-embedding-v4'
		WHERE chunk_id = $1 AND generation = $2`, refs[1].ChunkID, gen2.Generation)
	require.NoError(t, err)

	evidence, err = manager.Validate(ctx, gen2.Generation)
	require.NoError(t, err)
	require.True(t, evidence.Passed())
	require.Equal(t, int64(len(refs)), evidence.VectorCount)

	switched, err := manager.Activate(ctx, evidence)
	require.NoError(t, err)
	require.Equal(t, gen2.Generation, switched.Active.Generation)
	require.Equal(t, knowledge.GenerationActive, switched.Active.Status)
	require.True(t, switched.Active.ActivatedAt.Equal(knNow), "激活时刻按 Unix 毫秒 UTC 落库")
	require.True(t, switched.Active.RetiredAt.IsZero(), "active 行的 retired_at 必须是 NULL")
	require.Equal(t, 1, switched.Retired, "切换必须报告被退休的旧 active 代际")
	requireActiveGeneration(t, h, gen2.Generation)

	// 旧代际：状态翻成 retired，两条时间列**成对**翻转（冻结 CHECK 的要求），行保留。
	var (
		oldStatus    string
		oldActivated sql.NullInt64
		oldRetired   sql.NullInt64
		rows         int
	)
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status, activated_at, retired_at FROM kb_index_generations
		WHERE generation = 1`).Scan(&oldStatus, &oldActivated, &oldRetired))
	require.Equal(t, string(knowledge.GenerationRetired), oldStatus)
	require.False(t, oldActivated.Valid, "retired 行不得保留 activated_at")
	require.True(t, oldRetired.Valid, "retired 行必须有 retired_at")
	require.Equal(t, knNow.UnixMilli(), oldRetired.Int64)
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_index_generations`).Scan(&rows))
	require.Equal(t, 2, rows, "退休只改状态，不删除行（回退窗口内保留）")

	// ⑤ 数据库兜底：绕过存储层把旧代际改回 active，必须被部分唯一索引拒绝。
	_, err = h.db.pool.ExecContext(ctx, `
		UPDATE kb_index_generations
		SET status = 'active', activated_at = $1, retired_at = NULL
		WHERE generation = 1`, knNow.UnixMilli())
	require.Error(t, err, "任一时刻至多一个 active 必须由唯一索引兜底")
	require.Contains(t, err.Error(), "ux_kb_index_generations_active")

	// ⑥ 同端口的另一条写路径：废弃的 building 代际经 `Retire` 置 retired 且行保留；
	// active 代际不得经 `Retire` 移开（移开 active 只能走 `Activate`）。
	gen3, err := manager.Begin(ctx, knowledge.GenerationSpec{
		EmbeddingModel: "text-embedding-v4",
		Dimensions:     knowledge.VectorDimensions,
	})
	require.NoError(t, err)
	require.Equal(t, 3, gen3.Generation)
	retiredRow, err := manager.Retire(ctx, gen3.Generation)
	require.NoError(t, err)
	require.Equal(t, knowledge.GenerationRetired, retiredRow.Status)
	require.True(t, retiredRow.RetiredAt.Equal(knNow), "退休时刻按 Unix 毫秒 UTC 落库")
	require.True(t, retiredRow.ActivatedAt.IsZero(), "retired 行的 activated_at 必须是 NULL")

	_, err = manager.Retire(ctx, gen2.Generation)
	require.ErrorIs(t, err, knowledge.ErrInvalidRequest, "active 代际不得直接退休")
	requireActiveGeneration(t, h, gen2.Generation)
	require.NoError(t, h.db.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM kb_index_generations`).Scan(&rows))
	require.Equal(t, 3, rows, "退休与拒绝都不删除行")
}

// mustGeneration 读一行代际，读不到即让用例停下（夹具失败不该表现成断言失败）。
// mustGeneration reads one generation row or fails the test.
func mustGeneration(t *testing.T, db *DB, generation int) knowledge.IndexGeneration {
	t.Helper()
	row, err := NewIndexStore(db).Generation(context.Background(), generation)
	require.NoError(t, err)
	return row
}

// TestIndex_EnsureBootstrapsFirstGenerationOnEmptyDatabase 判的是 `serve` 启动装配走的
// 那条路径（`internal/app/wire_knowledge.go` 在 embedding 解析成功时调
// `GenerationManager.Ensure`）：迁移完本表零行；首代际由启动装配建行 → 校验 → 激活；
// 第二次启动沿用同一个 active、不新增行；配置与 active 不一致时拒绝启动（换模型必须
// 显式切换）。契约尾部第 3 条「kb_index_generations 不由迁移器写入」的判据面就是这里。
//
// 逆改点：让迁移器（`ensureSchema`）自己写首代际 → 开头的零行断言红；把 Ensure 的
// mismatch 分支改成"配置变了就自动新建" → `ErrGenerationMismatch` 与"拒绝后仍 1 行"
// 同时红（会多出第 2 行）。
func TestIndex_EnsureBootstrapsFirstGenerationOnEmptyDatabase(t *testing.T) {
	db := knTestDB(t)
	ctx := context.Background()

	var rows int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM kb_index_generations`).Scan(&rows))
	require.Zero(t, rows, "迁移不写代际——零行是建库后的正常读数")

	manager := knowledge.NewGenerationManager(knowledge.GenerationDeps{
		Store: NewIndexStore(db),
		Now:   func() time.Time { return knNow },
	})
	spec := knowledge.GenerationSpec{
		EmbeddingModel: "text-embedding-v4",
		Dimensions:     knowledge.VectorDimensions,
	}

	first, err := manager.Ensure(ctx, spec)
	require.NoError(t, err, "空库首次启动必须建首代际（C-09 允许的唯一自动建行）")
	require.Equal(t, 1, first.Generation, "代际号从 1 起")
	require.Equal(t, knowledge.GenerationActive, first.Status, "建行后必须校验并激活")
	requireActiveGeneration(t, &knHarness{db: db}, 1)

	second, err := manager.Ensure(ctx, spec)
	require.NoError(t, err)
	require.Equal(t, first.Generation, second.Generation, "第二次启动沿用同一个 active 代际")
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM kb_index_generations`).Scan(&rows))
	require.Equal(t, 1, rows, "确保不重复建行")

	_, err = manager.Ensure(ctx, knowledge.GenerationSpec{
		EmbeddingModel: "text-embedding-v5",
		Dimensions:     knowledge.VectorDimensions,
	})
	require.ErrorIs(t, err, knowledge.ErrGenerationMismatch, "启动路径不得自动换模型")
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM kb_index_generations`).Scan(&rows))
	require.Equal(t, 1, rows, "拒绝启动不写行")
}

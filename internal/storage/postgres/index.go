package postgres

// 本文件是**索引代际**的存储适配层（US-16 §3.1.3 / C-09 / ADR-03 / T129）：在冻结表
// `kb_index_generations` 上实现消费方窄端口 `knowledge.GenerationStore`，装配进
// `knowledge.GenerationManager` 之后构成代际生命周期的真正落库路径。
//
// 三条纪律（与 `internal/knowledge/generation.go` 的端口契约逐条对应）：
//
//  1. **切换是单事务**：`ActivateGeneration` 在一次事务里退休当前 active、激活目标。
//     "先激活再退休"必然撞上部分唯一索引 `ux_kb_index_generations_active`——那是数据库
//     侧兜底，不是可用的实现顺序。锁序固定为"旧 active 行 → 目标行"，并发切换排队而
//     不互相死锁。
//  2. **代际号严格递增**：`CreateGeneration` 在事务级 advisory 锁下取 `max+1`。不加锁
//     时两条并发建代际会读到同一个 `max`，后到的那条撞主键——重试掩盖的是真实竞态。
//  3. **退休不删除**：`RetireGeneration` 只改 `status` / 时间列，行与向量保留到回退
//     窗口结束（C-09）。冻结 CHECK 要求 `(status='active') = (activated_at IS NOT NULL)`
//     与 `(status='retired') = (retired_at IS NOT NULL)`，两条时间列**必须**成对写入。
//
// 本文件同时交付 `knowledge.VectorIndex`（向量写入 + 代际切换）。四条纪律：
//
//  1. **摄取只写向量**：`Write` 只往 `kb_chunk_embeddings` 落行，**不**改代际状态、
//     **不**改版本状态——切换归 `Activate`，版本生效归 `IngestStore.ActivateVersion`。
//  2. **写入前核对归属与形状**：片段必须属于该版本，向量宽度必须等于冻结的 512，
//     目标代际必须存在且未退休。任一条不满足都在写入前拒绝，**不得**让数据库 CHECK
//     兜底（那是最后一道，不是第一道）。
//  3. **`Activate` 不是校验门**：它只做"该版本在该代际的向量完整"这一存储层核对，
//     然后复用同一个单事务切换。`GenerationManager` 的校验证据门（C-09）在**上层**，
//     存储层拿不到证据；因此 `Activate` 在业务路径上的唯一合法调用方仍是代际管理器，
//     而其接线点尚未落地（缺口登记在 run-state）。
//  4. **`Retire` 按版本绑定的代际退休整代**：端口签名只给版本标识，版本行上的
//     `generation` 就是它的代际归属；active 代际照旧拒绝直接退休。
//
// This file implements `knowledge.GenerationStore` and `knowledge.VectorIndex`. Write only
// writes vectors; Activate reuses the single transactional switch after a storage-level
// completeness check (the evidence gate lives upstream in GenerationManager); Retire
// retires the generation the version row is bound to.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/knowledge"
)

// indexGenerationLockSeed 是代际号分配锁的固定种子。与迁移锁（`basalt/schema-migrate`）
// 和压缩链锁（`basalt/compaction-append`）互不撞键——三类锁保护的是不同资源。
// The seed derives the allocation lock key; it never collides with the other lock families.
const indexGenerationLockSeed = "basalt/kb-index-generation"

// indexGenerationAdvisoryKey 是 `pg_advisory_xact_lock` 的 int64 键（见 `advisoryKey`）。
// indexGenerationAdvisoryKey is the transaction-scoped advisory lock key.
var indexGenerationAdvisoryKey = advisoryKey(indexGenerationLockSeed)

var (
	// errIndexStoreUnconfigured 是装配错误：没有数据库句柄时拒绝服务，**不得**用空结果
	// 冒充"没有代际"——那会让启动路径以为可以建首代际。
	// errIndexStoreUnconfigured fails closed when the handle is missing.
	errIndexStoreUnconfigured = errors.New("postgres: index store has no database handle")

	// errIndexStateConflict 表示行的当前状态不允许这次操作（对已是 active 的代际再激活 /
	// 直接退休）。代际管理器已在调用前拦过一道，这里是存储层的第二道：**不得**靠
	// 调用方守规矩。
	// errIndexStateConflict rejects a transition the row's current state forbids.
	errIndexStateConflict = errors.New("postgres: index generation state conflict")
)

// indexStore 是 `knowledge.GenerationStore` 的 PostgreSQL 实现。
// indexStore implements the generation port on the frozen table.
type indexStore struct {
	db *DB
}

// 编译期断言：代际端口与向量端口都由本类型满足。
// The same store satisfies both the generation and the vector ports.
var (
	_ knowledge.GenerationStore = (*indexStore)(nil)
	_ knowledge.VectorIndex     = (*indexStore)(nil)
)

// NewIndexStore 构造索引代际存储；返回类型不导出，调用方按端口使用。
// NewIndexStore builds the store; the concrete type stays unexported.
func NewIndexStore(db *DB) *indexStore {
	return &indexStore{db: db}
}

// ready 是所有方法的统一前置：句柄缺失即失败关闭。
// ready is the shared fail-closed precondition.
func (s *indexStore) ready() error {
	if s == nil || s.db == nil || s.db.pool == nil {
		return errIndexStoreUnconfigured
	}
	return nil
}

// indexGenerationColumns 是行视图的列序，读路径与 RETURNING 共用一份，避免两处漂移。
// indexGenerationColumns is shared by every read path so the column order cannot drift.
const indexGenerationColumns = `generation, embedding_model, dimensions, distance, status,
	created_at, activated_at, retired_at`

// scanIndexGeneration 把一行读成 `knowledge.IndexGeneration`：时间列是 BIGINT 毫秒 UTC，
// NULL 映射为零值时间（`IndexGeneration` 的契约：零值表示 NULL，不具名零时刻冒充）。
// scanIndexGeneration maps nullable millisecond columns onto zero times.
func scanIndexGeneration(row interface{ Scan(dest ...any) error }) (knowledge.IndexGeneration, error) {
	var (
		record    knowledge.IndexGeneration
		distance  string
		status    string
		createdAt int64
		activated sql.NullInt64
		retired   sql.NullInt64
	)
	if err := row.Scan(&record.Generation, &record.EmbeddingModel, &record.Dimensions,
		&distance, &status, &createdAt, &activated, &retired); err != nil {
		return knowledge.IndexGeneration{}, err
	}
	record.Distance = knowledge.GenerationDistance(distance)
	record.Status = knowledge.GenerationStatus(status)
	record.CreatedAt = time.UnixMilli(createdAt).UTC()
	record.ActivatedAt = unixMilliOrZero(activated)
	record.RetiredAt = unixMilliOrZero(retired)
	return record, nil
}

// ActiveGeneration 返回当前 active 代际；没有时 ok 为 false。
// ActiveGeneration returns the single active generation, if any.
func (s *indexStore) ActiveGeneration(ctx context.Context) (knowledge.IndexGeneration, bool, error) {
	if err := s.ready(); err != nil {
		return knowledge.IndexGeneration{}, false, err
	}
	record, err := scanIndexGeneration(s.db.pool.QueryRowContext(ctx, `
		SELECT `+indexGenerationColumns+`
		FROM kb_index_generations WHERE status = 'active'`))
	if errors.Is(err, sql.ErrNoRows) {
		return knowledge.IndexGeneration{}, false, nil
	}
	if err != nil {
		return knowledge.IndexGeneration{}, false, fmt.Errorf("读 active 索引代际: %w", err)
	}
	return record, true, nil
}

// Generation 按代际号读行；不存在返回 `knowledge.ErrNotFound`。
// Generation reads one row by generation number.
func (s *indexStore) Generation(ctx context.Context, generation int) (knowledge.IndexGeneration, error) {
	if err := s.ready(); err != nil {
		return knowledge.IndexGeneration{}, err
	}
	if generation < 1 {
		return knowledge.IndexGeneration{}, fmt.Errorf("%w: 索引代际 %d", knowledge.ErrNotFound, generation)
	}
	record, err := scanIndexGeneration(s.db.pool.QueryRowContext(ctx, `
		SELECT `+indexGenerationColumns+`
		FROM kb_index_generations WHERE generation = $1`, generation))
	if errors.Is(err, sql.ErrNoRows) {
		return knowledge.IndexGeneration{}, fmt.Errorf("%w: 索引代际 %d", knowledge.ErrNotFound, generation)
	}
	if err != nil {
		return knowledge.IndexGeneration{}, fmt.Errorf("读索引代际 %d: %w", generation, err)
	}
	return record, nil
}

// CreateGeneration 建一行 `building` 代际并回读。代际号由 `max(generation) + 1` 在同一
// 事务里分配：advisory 锁把并发建代际串行化，主键只是最后一道兜底。
// CreateGeneration allocates max+1 under a transaction-scoped advisory lock.
func (s *indexStore) CreateGeneration(ctx context.Context, spec knowledge.GenerationSpec,
	at time.Time,
) (knowledge.IndexGeneration, error) {
	if err := s.ready(); err != nil {
		return knowledge.IndexGeneration{}, err
	}
	millis := at.UTC().UnixMilli()
	var created knowledge.IndexGeneration
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`,
			indexGenerationAdvisoryKey); err != nil {
			return fmt.Errorf("申请代际号分配锁: %w", err)
		}
		// `INSERT … SELECT` 的聚合在没有行时也返回一行，`COALESCE` 因此给出首代际 1。
		// The aggregate over an empty table still yields one row, so the first id is 1.
		record, err := scanIndexGeneration(tx.QueryRowContext(ctx, `
			INSERT INTO kb_index_generations
				(generation, embedding_model, dimensions, distance, status, created_at)
			SELECT COALESCE(MAX(generation), 0) + 1, $1, $2, $3, $4, $5
			FROM kb_index_generations
			RETURNING `+indexGenerationColumns,
			spec.EmbeddingModel, spec.Dimensions, string(spec.Distance),
			string(knowledge.GenerationBuilding), millis))
		if err != nil {
			return fmt.Errorf("建索引代际: %w", err)
		}
		created = record
		return nil
	})
	if err != nil {
		return knowledge.IndexGeneration{}, err
	}
	return created, nil
}

// GenerationVectorStats 统计该代际的向量总数与不一致条数。一次查询同时完成"代际是否
// 存在"与两组计数——代际缺失时子查询无从取值，整行不存在，按 `ErrNotFound` 报错，
// 不会被读成"0 条向量的空代际"。
// GenerationVectorStats counts vectors and model/dimension mismatches in one query.
func (s *indexStore) GenerationVectorStats(ctx context.Context, generation int) (knowledge.GenerationVectorStats, error) {
	if err := s.ready(); err != nil {
		return knowledge.GenerationVectorStats{}, err
	}
	if generation < 1 {
		return knowledge.GenerationVectorStats{}, fmt.Errorf("%w: 索引代际 %d", knowledge.ErrNotFound, generation)
	}
	var stats knowledge.GenerationVectorStats
	err := s.db.pool.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM kb_chunk_embeddings e WHERE e.generation = g.generation),
			(SELECT COUNT(*) FROM kb_chunk_embeddings e
			 WHERE e.generation = g.generation
			   AND (e.model <> g.embedding_model OR e.dimensions <> g.dimensions))
		FROM kb_index_generations g WHERE g.generation = $1`, generation).
		Scan(&stats.Total, &stats.Mismatched)
	if errors.Is(err, sql.ErrNoRows) {
		return knowledge.GenerationVectorStats{}, fmt.Errorf("%w: 索引代际 %d", knowledge.ErrNotFound, generation)
	}
	if err != nil {
		return knowledge.GenerationVectorStats{}, fmt.Errorf("统计索引代际 %d 的向量: %w", generation, err)
	}
	return stats, nil
}

// ActivateGeneration 在**单个事务**内完成"退休当前 active → 激活目标"，返回被退休的
// 代际号（没有旧 active 时为 0）。
//
// 顺序不可交换：先激活目标会与旧 active 撞上部分唯一索引。锁定顺序固定为
// "当前 active 行 → 目标行"——两次并发切换都从同一点排队，不会形成环。
// ActivateGeneration retires the current active generation and activates the target
// in one transaction; the order and the lock order are both load-bearing.
func (s *indexStore) ActivateGeneration(ctx context.Context, generation int, at time.Time) (int, error) {
	if err := s.ready(); err != nil {
		return 0, err
	}
	if generation < 1 {
		return 0, fmt.Errorf("%w: 索引代际 %d", knowledge.ErrNotFound, generation)
	}
	millis := at.UTC().UnixMilli()
	retired := 0
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var current sql.NullInt32
		err := tx.QueryRowContext(ctx, `
			SELECT generation FROM kb_index_generations
			WHERE status = 'active' FOR UPDATE`).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("锁定当前 active 索引代际: %w", err)
		}

		var status string
		err = tx.QueryRowContext(ctx, `
			SELECT status FROM kb_index_generations
			WHERE generation = $1 FOR UPDATE`, generation).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 索引代际 %d", knowledge.ErrNotFound, generation)
		}
		if err != nil {
			return fmt.Errorf("锁定索引代际 %d: %w", generation, err)
		}
		if knowledge.GenerationStatus(status) == knowledge.GenerationActive {
			return fmt.Errorf("%w: 索引代际 %d 已是 active", errIndexStateConflict, generation)
		}

		if current.Valid && int(current.Int32) != generation {
			if _, err := tx.ExecContext(ctx, `
				UPDATE kb_index_generations
				SET status = 'retired', activated_at = NULL, retired_at = $2
				WHERE generation = $1`, current.Int32, millis); err != nil {
				return fmt.Errorf("退休索引代际 %d: %w", current.Int32, err)
			}
			retired = int(current.Int32)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE kb_index_generations
			SET status = 'active', activated_at = $2, retired_at = NULL
			WHERE generation = $1`, generation, millis); err != nil {
			return fmt.Errorf("激活索引代际 %d: %w", generation, err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return retired, nil
}

// RetireGeneration 把非 active 代际置 `retired` 并保留行与向量；`activated_at` 必须
// 同时清空，否则冻结 CHECK 会拒绝这次更新。
// RetireGeneration keeps the row and clears activated_at, as the frozen CHECK demands.
func (s *indexStore) RetireGeneration(ctx context.Context, generation int, at time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	if generation < 1 {
		return fmt.Errorf("%w: 索引代际 %d", knowledge.ErrNotFound, generation)
	}
	millis := at.UTC().UnixMilli()
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `
			SELECT status FROM kb_index_generations
			WHERE generation = $1 FOR UPDATE`, generation).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 索引代际 %d", knowledge.ErrNotFound, generation)
		}
		if err != nil {
			return fmt.Errorf("锁定索引代际 %d: %w", generation, err)
		}
		if knowledge.GenerationStatus(status) == knowledge.GenerationActive {
			return fmt.Errorf("%w: 索引代际 %d 是 active，不能直接退休", errIndexStateConflict, generation)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE kb_index_generations
			SET status = 'retired', activated_at = NULL, retired_at = $2
			WHERE generation = $1`, generation, millis); err != nil {
			return fmt.Errorf("退休索引代际 %d: %w", generation, err)
		}
		return nil
	})
}

// indexVectorColumns 是向量行的列序，读写两处共用一份。
// indexVectorColumns is shared by the vector upsert paths.
const indexVectorColumns = `chunk_id, generation, embedding, model, dimensions, created_at`

// indexVectorUpsert 是单行写入；同 (chunk_id, generation) 重写即覆盖——同一片段在同一
// 代际只有一条向量（冻结主键），重试与重切块都必须收敛到同一行。
// indexVectorUpsert writes one row; a retry converges on the frozen primary key.
const indexVectorUpsert = `INSERT INTO kb_chunk_embeddings (` + indexVectorColumns + `)
	VALUES ($1, $2, $3::vector, $4, $5, $6)
	ON CONFLICT (chunk_id, generation) DO UPDATE
	SET embedding = EXCLUDED.embedding, model = EXCLUDED.model,
	    dimensions = EXCLUDED.dimensions, created_at = EXCLUDED.created_at`

// Write 把一批片段向量写进 `kb_chunk_embeddings`。先核对归属与形状，再在**单个事务**
// 里逐行 upsert：半批写入会让代际的向量数与片段数不等，校验门随后必然拒绝，但错误
// 会出现在离原因很远的地方。
//
// Write upserts one batch of chunk vectors in a single transaction, after checking every
// precondition a database CHECK would otherwise report far away from its cause.
func (s *indexStore) Write(ctx context.Context, versionID string, chunks []knowledge.ChunkRef,
	vectors [][]float32, generation int,
) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(versionID) == "" || generation < 1 {
		return fmt.Errorf("%w: version=%q generation=%d", errKnowledgeInvalidArgument, versionID, generation)
	}
	if len(chunks) == 0 {
		return fmt.Errorf("%w: 版本 %s 没有片段可写", errKnowledgeInvalidArgument, versionID)
	}
	if len(chunks) != len(vectors) {
		return fmt.Errorf("%w: 版本 %s 片段 %d 条、向量 %d 条",
			errKnowledgeInvalidArgument, versionID, len(chunks), len(vectors))
	}
	ids := make([]string, len(chunks))
	for i, ref := range chunks {
		if strings.TrimSpace(ref.ChunkID) == "" {
			return fmt.Errorf("%w: 版本 %s 第 %d 条片段缺少标识",
				errKnowledgeInvalidArgument, versionID, i)
		}
		if len(vectors[i]) != knowledge.VectorDimensions {
			// 维度不符在读取正文前就拒绝（与摄取侧同一口径），不让列类型兜底。
			return fmt.Errorf("%w: 版本 %s 第 %d 条向量宽 %d，冻结宽度 %d",
				errKnowledgeInvalidArgument, versionID, i, len(vectors[i]), knowledge.VectorDimensions)
		}
		ids[i] = ref.ChunkID
	}

	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			model      string
			dimensions int
			status     string
		)
		err := tx.QueryRowContext(ctx, `
			SELECT embedding_model, dimensions, status FROM kb_index_generations
			WHERE generation = $1 FOR SHARE`, generation).
			Scan(&model, &dimensions, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 索引代际 %d", knowledge.ErrNotFound, generation)
		}
		if err != nil {
			return fmt.Errorf("锁定索引代际 %d: %w", generation, err)
		}
		if knowledge.GenerationStatus(status) == knowledge.GenerationRetired {
			return fmt.Errorf("%w: 索引代际 %d 已退休，不再接收向量", errIndexStateConflict, generation)
		}
		if dimensions != knowledge.VectorDimensions {
			return fmt.Errorf("%w: 索引代际 %d 的维度是 %d，冻结宽度 %d",
				errIndexStateConflict, generation, dimensions, knowledge.VectorDimensions)
		}
		if err := requireOwnedChunks(ctx, tx, versionID, ids); err != nil {
			return err
		}

		// `created_at` 取句柄时钟（一次写入共用一个时刻）：同批行的时间戳可比。
		millis := s.db.clock().UTC().UnixMilli()
		for i, ref := range chunks {
			literal, err := vectorLiteral(vectors[i])
			if err != nil {
				return fmt.Errorf("%w: 版本 %s 第 %d 条向量: %v",
					errKnowledgeInvalidArgument, versionID, i, err)
			}
			if _, err := tx.ExecContext(ctx, indexVectorUpsert,
				ref.ChunkID, generation, literal, model, dimensions, millis); err != nil {
				return fmt.Errorf("写索引代际 %d 的片段 %s: %w", generation, ref.ChunkID, err)
			}
		}
		return nil
	})
}

// requireOwnedChunks 核对每个片段都挂在给定版本下（`kb_chunks.version_id`）。跨版本写入
// 会让一个版本的数字因别的版本的片段而"看起来完整"，因此缺任何一条都拒绝整批。
// requireOwnedChunks rejects the whole batch if any chunk belongs to another version.
func requireOwnedChunks(ctx context.Context, tx *sql.Tx, versionID string, ids []string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT chunk_id FROM kb_chunks
		WHERE version_id = $1 AND chunk_id = ANY($2)`, versionID, ids)
	if err != nil {
		return fmt.Errorf("核对片段归属: %w", err)
	}
	defer func() { _ = rows.Close() }()
	owned := make(map[string]struct{}, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("读取片段归属: %w", err)
		}
		owned[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取片段归属: %w", err)
	}
	for _, id := range ids {
		if _, ok := owned[id]; !ok {
			return fmt.Errorf("%w: 片段 %s 不属于版本 %s",
				errKnowledgeInvalidArgument, id, versionID)
		}
	}
	return nil
}

// requireVersionVectorsComplete 核对某版本在某代际的向量覆盖**全部**片段且非空：这正是
// "该版本的向量已经建好"在存储层可判的唯一形态（`IngestStore.ActivateVersion` 生效前
// 也做同一核对，两处口径必须一致）。
// requireVersionVectorsComplete is the storage-level form of "this version has vectors".
func requireVersionVectorsComplete(ctx context.Context, tx *sql.Tx, versionID string, generation int) error {
	var total, covered int
	err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM kb_chunks WHERE version_id = $1),
			(SELECT COUNT(*) FROM kb_chunk_embeddings e
			 JOIN kb_chunks c ON c.chunk_id = e.chunk_id
			 WHERE c.version_id = $1 AND e.generation = $2)`, versionID, generation).
		Scan(&total, &covered)
	if err != nil {
		return fmt.Errorf("核对版本 %s 在代际 %d 的向量: %w", versionID, generation, err)
	}
	if total == 0 || covered != total {
		return fmt.Errorf("%w: 版本 %s 在代际 %d 有 %d/%d 条向量",
			errKnowledgeArtifactsIncomplete, versionID, generation, covered, total)
	}
	return nil
}

// Activate 先把目标代际切为 active。切换前两道本地核对：版本的向量在该代际必须完整；
// 版本行若已绑定别的代际则拒绝（说明调用方拿错了代际号）。切换本身复用
// `ActivateGeneration` 的同一事务，**不**另写一套顺序。
//
// **这里不是校验门**：`GenerationManager` 的证据门（C-09）在上层，存储层看不到证据；
// 业务路径**不得**绕过管理器直接调本方法。
// Activate switches the generation through the same transaction; the evidence gate stays
// upstream, so business code must still go through GenerationManager.
func (s *indexStore) Activate(ctx context.Context, versionID string, generation int) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(versionID) == "" || generation < 1 {
		return fmt.Errorf("%w: version=%q generation=%d", errKnowledgeInvalidArgument, versionID, generation)
	}
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		bound, err := versionGeneration(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if bound.Valid && int(bound.Int32) != generation {
			return fmt.Errorf("%w: 版本 %s 绑定代际 %d，不能按代际 %d 激活",
				errKnowledgeStateConflict, versionID, bound.Int32, generation)
		}
		return requireVersionVectorsComplete(ctx, tx, versionID, generation)
	})
	if err != nil {
		return err
	}
	_, err = s.ActivateGeneration(ctx, generation, s.db.clock())
	return err
}

// Retire 退休该版本所属的代际：端口只给版本标识，代际归属取自版本行的 `generation`
// 列。未绑定代际的版本无从退休（`kb_document_versions.generation` 为 NULL）；
// active 代际照旧被 `RetireGeneration` 拒绝——移开 active 只能经 `Activate`。
// Retire retires the generation the version is bound to; an unbound version cannot be
// retired, and an active generation still only leaves through Activate.
func (s *indexStore) Retire(ctx context.Context, versionID string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(versionID) == "" {
		return fmt.Errorf("%w: version=%q", errKnowledgeInvalidArgument, versionID)
	}
	var bound sql.NullInt32
	err := s.db.pool.QueryRowContext(ctx, `
		SELECT generation FROM kb_document_versions WHERE version_id = $1`, versionID).Scan(&bound)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 版本 %s", knowledge.ErrNotFound, versionID)
	}
	if err != nil {
		return fmt.Errorf("读版本 %s 的索引代际: %w", versionID, err)
	}
	if !bound.Valid {
		return fmt.Errorf("%w: 版本 %s 尚未绑定索引代际", errKnowledgeInvalidArgument, versionID)
	}
	return s.RetireGeneration(ctx, int(bound.Int32), s.db.clock())
}

// versionGeneration 读版本行绑定的代际号；NULL 表示尚未绑定（`Valid` 为假）。
// versionGeneration reads the generation bound to a version; Valid is false when NULL.
func versionGeneration(ctx context.Context, tx *sql.Tx, versionID string) (sql.NullInt32, error) {
	var bound sql.NullInt32
	err := tx.QueryRowContext(ctx, `
		SELECT generation FROM kb_document_versions WHERE version_id = $1 FOR SHARE`,
		versionID).Scan(&bound)
	if errors.Is(err, sql.ErrNoRows) {
		return bound, fmt.Errorf("%w: 版本 %s", knowledge.ErrNotFound, versionID)
	}
	if err != nil {
		return bound, fmt.Errorf("读版本 %s 的索引代际: %w", versionID, err)
	}
	return bound, nil
}

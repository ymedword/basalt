package postgres

// 本文件是 `memory_embeddings` 的**写入口与缺口判定**（V-5，D-18 的「回填向量」）；
// 读入口仍在 `memory_store.go` 的 `recallEntriesByVector`。四条纪律：
//   - ① **代际显式传入**：调用方给的号就是唯一目标代际，本文件不查 active 代际、不
//     自行挑选——回填只写装配期固定的那一个。
//   - ② **外部调用在事务外**：写方法只做「校验代际 → 逐行 upsert」，嵌入发生在调用方；
//     事务内不得有网络往返。
//   - ③ **幂等 upsert**：`PRIMARY KEY (entry_id, generation)` + `ON CONFLICT DO UPDATE`，
//     重复回填与并发补索引不会产出第二份向量。
//   - ④ **退役代际拒写**：与 `indexStore.Write` 同一条判据（`SELECT ... FOR SHARE`），
//     已退役代际不再接收向量；维度不符同样拒绝，而不是等 `vector(512)` 转型报错。
// This file is the write entry and gap probe for memory_embeddings: an explicit
// generation, no external call inside the transaction, an idempotent upsert, and the
// same retired-generation guard as indexStore.Write.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/memory"
)

// 编译期断言：载体同时满足索引器声明的写入端口；形状漂移在这里先红。
// Compile-time assertion that the carrier satisfies the indexer's write port.
var _ memory.MemoryEmbeddingStore = (*MemoryStore)(nil)

// EmbeddingGeneration 按代际号读一行（不存在即错误，**不**返回零值——零值会被调用方
// 当成"维度 0"的合法代际）。
// EmbeddingGeneration reads one generation; a missing row is an error, never a zero value.
func (s *MemoryStore) EmbeddingGeneration(ctx context.Context, generation int) (memory.EmbeddingGeneration, error) {
	if generation <= 0 {
		return memory.EmbeddingGeneration{}, fmt.Errorf("记忆索引代际号必须为正（当前 %d）", generation)
	}
	var out memory.EmbeddingGeneration
	err := s.db.pool.QueryRowContext(ctx,
		`SELECT generation, embedding_model, dimensions, status
		   FROM kb_index_generations WHERE generation = $1`, generation).
		Scan(&out.Generation, &out.Model, &out.Dimensions, &out.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.EmbeddingGeneration{}, fmt.Errorf("记忆索引代际 %d 不存在", generation)
	}
	if err != nil {
		return memory.EmbeddingGeneration{}, fmt.Errorf("读取记忆索引代际 %d: %w", generation, err)
	}
	return out, nil
}

// memoryUnindexedColumns 是缺口查询与召回共用的十列形状（`score` 恒 0：缺口列表不排序）。
// The ten-column shape shared with the recall scans; the gap list carries no score.
const memoryUnindexedColumns = `e.id, e.scope, e.agent_name, e.section, e.content, e.state,
       e.source, e.supersedes_id, e.created_at, 0::float8 AS score`

// ListUnindexedArchiveEntries 返回该代际下尚无向量的生效归档条目，按 `id` 升序、至多
// limit 条。判据与召回同一句「生效层」话（`state='active'` + 未到期），因此永远不会把
// 候选 / 已删 / 到期条目拉去做向量。
// ListUnindexedArchiveEntries lists effective archive rows without a vector for the
// generation, oldest first.
func (s *MemoryStore) ListUnindexedArchiveEntries(ctx context.Context, generation, limit int) ([]memory.MemoryEntry, error) {
	if generation <= 0 {
		return nil, fmt.Errorf("记忆向量回填：代际号必须为正（当前 %d）", generation)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("记忆向量回填：批量上限必须为正（当前 %d）——不接受无界查询", limit)
	}
	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT `+memoryUnindexedColumns+`
		  FROM memory_entries e
		 WHERE e.section = $1 AND e.state = $2
		   AND (e.valid_to IS NULL OR e.valid_to > $3)
		   AND NOT EXISTS (
		       SELECT 1 FROM memory_embeddings m
		        WHERE m.entry_id = e.id AND m.generation = $4)
		 ORDER BY e.id
		 LIMIT $5::bigint`,
		string(core.SectionArchive), memoryStateActive, s.now().UnixMilli(), generation, limit)
	if err != nil {
		return nil, fmt.Errorf("列出待回填的记忆条目（代际 %d）: %w", generation, err)
	}
	entries, err := scanMemoryEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("读取待回填的记忆条目（代际 %d）: %w", generation, err)
	}
	return entries, nil
}

// ListUnindexedEntriesByContent 返回该（分层, 分区, 正文）下尚无本代际向量的条目：写后
// 补索引按它去重。正文按入库时同一套规范化求哈希（与 `FindEntryByContent` 同源）。
// ListUnindexedEntriesByContent lists the same-content rows still missing a vector.
func (s *MemoryStore) ListUnindexedEntriesByContent(ctx context.Context, generation int, scope core.Scope,
	section core.Section, content string,
) ([]memory.MemoryEntry, error) {
	if generation <= 0 {
		return nil, fmt.Errorf("记忆向量补索引：代际号必须为正（当前 %d）", generation)
	}
	if err := validateMemorySection(section); err != nil {
		return nil, err
	}
	scopeCol, agentName := scopeColumns(scope)
	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT `+memoryUnindexedColumns+`
		  FROM memory_entries e
		 WHERE e.scope = $1 AND e.agent_name IS NOT DISTINCT FROM $2 AND e.section = $3
		   AND e.state = $4
		   AND (e.valid_to IS NULL OR e.valid_to > $5)
		   AND e.content_hash = $6
		   AND NOT EXISTS (
		       SELECT 1 FROM memory_embeddings m
		        WHERE m.entry_id = e.id AND m.generation = $7)
		 ORDER BY e.id`,
		scopeCol, agentName, string(section), memoryStateActive, s.now().UnixMilli(),
		// 正文哈希按入库时同一套规范化求（与 FindEntryByContent 同源）；调用方传未规范化
		// 正文也不会静默查不到。
		memoryContentHash(strings.TrimSpace(content)), generation)
	if err != nil {
		return nil, fmt.Errorf("列出待补索引的记忆条目（%s/%s，代际 %d）: %w", scopeCol, section, generation, err)
	}
	entries, err := scanMemoryEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("读取待补索引的记忆条目（%s/%s，代际 %d）: %w", scopeCol, section, generation, err)
	}
	return entries, nil
}

// WriteMemoryEmbeddings 在**一个短事务**内校验代际并逐行写向量。校验三件与
// `indexStore.Write` 同源：代际存在、未退役、维度等于冻结宽度（`vector(512)`）。
// `entries` 与 `vectors` 必须等长且顺序对应；任一条不满足都整批回滚。
// WriteMemoryEmbeddings validates the generation then upserts the batch in one short
// transaction; a retired generation and a width mismatch are both refused.
func (s *MemoryStore) WriteMemoryEmbeddings(ctx context.Context, generation int,
	entries []memory.MemoryEntry, vectors [][]float32,
) error {
	if generation <= 0 {
		return fmt.Errorf("记忆向量写入：代际号必须为正（当前 %d）", generation)
	}
	if len(entries) != len(vectors) {
		return fmt.Errorf("记忆向量写入：条目 %d 条与向量 %d 个不匹配", len(entries), len(vectors))
	}
	if len(entries) == 0 {
		return nil
	}
	literals := make([]string, len(vectors))
	for k, vector := range vectors {
		literal, err := vectorLiteral(vector)
		if err != nil {
			return fmt.Errorf("记忆向量写入：第 %d 条向量: %v", k+1, err)
		}
		literals[k] = literal
	}

	createdAt := s.now().UTC().UnixMilli()
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
			return fmt.Errorf("记忆向量写入：索引代际 %d 不存在", generation)
		}
		if err != nil {
			return fmt.Errorf("锁定记忆索引代际 %d: %w", generation, err)
		}
		if knowledge.GenerationStatus(status) == knowledge.GenerationRetired {
			return fmt.Errorf("%w: 记忆索引代际 %d 已退休，不再接收向量", errIndexStateConflict, generation)
		}
		if dimensions != knowledge.VectorDimensions {
			return fmt.Errorf("%w: 记忆索引代际 %d 的维度是 %d，冻结宽度 %d",
				errIndexStateConflict, generation, dimensions, knowledge.VectorDimensions)
		}

		for k, entry := range entries {
			if entry.ID <= 0 {
				return fmt.Errorf("记忆向量写入：第 %d 条缺少条目主键", k+1)
			}
			if len(vectors[k]) != dimensions {
				return fmt.Errorf("%w: 条目 %d 的向量维度 %d 与代际 %d 不符（期望 %d）",
					errIndexStateConflict, entry.ID, len(vectors[k]), generation, dimensions)
			}
			res, err := tx.ExecContext(ctx, `
				INSERT INTO memory_embeddings (entry_id, generation, embedding, model, dimensions, created_at)
				VALUES ($1, $2, $3::vector, $4, $5, $6)
				ON CONFLICT (entry_id, generation) DO UPDATE
				   SET embedding = EXCLUDED.embedding,
				       model = EXCLUDED.model,
				       dimensions = EXCLUDED.dimensions,
				       created_at = EXCLUDED.created_at`,
				entry.ID, generation, literals[k], model, dimensions, createdAt)
			if err != nil {
				return fmt.Errorf("写入记忆向量（条目 %d，代际 %d）: %w", entry.ID, generation, err)
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("读取记忆向量写入影响行数（条目 %d）: %w", entry.ID, err)
			}
			if affected != 1 {
				return fmt.Errorf("写入记忆向量（条目 %d，代际 %d）: 影响 %d 行，期望 1",
					entry.ID, generation, affected)
			}
		}
		return nil
	})
}

// countUnindexedArchiveEntries 数该层该代际下**尚无向量**的生效归档条目。空召回时用它
// 区分两种结局：0 = 确实没有可召回记忆；> 0 = 有应索引记忆但索引未就绪（V-5 原文要求
// 显式提示，不得伪装成无匹配）。
// countUnindexedArchiveEntries distinguishes a genuine empty result from a vector gap.
func (s *MemoryStore) countUnindexedArchiveEntries(ctx context.Context, scopeCol string, agentName any,
	generation int, now int64,
) (int, error) {
	var missing int
	err := s.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM memory_entries e
		 WHERE e.scope = $1 AND e.agent_name IS NOT DISTINCT FROM $2 AND e.section = $3
		   AND e.state = $4
		   AND (e.valid_to IS NULL OR e.valid_to > $5)
		   AND NOT EXISTS (
		       SELECT 1 FROM memory_embeddings m
		        WHERE m.entry_id = e.id AND m.generation = $6)`,
		scopeCol, agentName, string(core.SectionArchive), memoryStateActive, now, generation).
		Scan(&missing)
	if err != nil {
		return 0, fmt.Errorf("统计记忆向量缺口（%s，代际 %d）: %w", scopeCol, generation, err)
	}
	return missing, nil
}

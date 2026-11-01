package memory

// 本文件是记忆向量的**写入侧**（V-5，D-18 的「回填向量」）：写时增量索引 + 启动期回填。
// 读侧（召回）仍在 `SemanticStore.Recall` 与存储层的 `recallEntriesByVector`；两侧共用
// 同一张 `memory_embeddings`（`PRIMARY KEY (entry_id, generation)`），因此写入是幂等
// upsert，重复回填与并发写入不会产生第二份向量。
// This file is the write side of memory vectors: incremental indexing after a save and a
// startup backfill. Both share the one embeddings table, so every write is an idempotent
// upsert and a repeated backfill cannot mint a second vector.
//
// 六条闭合（用户裁定 V-5 的原文，逐条对应实现）：
//  1. **新记忆写入**：`Indexer.IndexEntry` 在条目落库之后按（分层, 分区, 正文）找出
//     同文行里**尚无本代际向量**的那些，一次嵌入、一次写入；调用点在 app 的
//     `indexingCarrier`（包住门面，写锁已释放之后）。
//  2. **旧记忆导入**：`db switch` 只导行（D-22 只读导入）；向量由下一次正常启动的
//     `Backfill` 补齐——导入路径不改写、也不在迁移事务里发外部调用。
//  3. **模型变更**：写入与召回都以 `kb_index_generations` 行的 model / dimensions 为准；
//     代际号由装配层显式传入（`IndexerDeps.Generation`），**不**在运行期自行挑选或切换。
//  4. **失败重试**：upsert 幂等；回填按批推进，遇错即止（已写批次保留），下次启动重试。
//  5. **配额**：记忆写配额（核心区单条 / 总量）不变；embedding Token 用量由共用客户端
//     的 `embedding_calls` 审计行记账，本文件**不**新造配额类。
//  6. **审计**：索引器复用知识库栈那个**带审计**的 embedding 客户端（`auditedEmbedder`），
//     `run_id` / `ingest_job_id` 均为 NULL——与 `audited_embedder.go` 头注释的
//     「启动期回填同形」一致；预写失败即不发出调用（宪法第 5 条）。
//
// 外部嵌入调用**不得**放在长数据库事务里：本文件的顺序恒为「列表 → 嵌入 → 短事务写」，
// 存储实现也只在校验代际与逐行 upsert 时持事务。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// ErrIndexNotReady 表示**有应索引的生效归档记忆尚未建立当前代际的向量**——召回结果
// 因此不能下「没有可召回记忆」的结论。它与「确实没有可召回记忆」（正常空结果）是两种
// 不同的结局：前者必须显式提示，**不得**伪装成无匹配（V-5 的原文要求）。
// ErrIndexNotReady marks a vector gap, which must never be rendered as "no match".
var ErrIndexNotReady = errors.New("memory: 记忆向量索引未就绪")

// IsIndexNotReady 判定错误链里是否带着索引缺口。装配层据此收敛成工具侧哨兵；工具层
// **不得**比对错误文本。
// IsIndexNotReady classifies the gap error without matching on text.
func IsIndexNotReady(err error) bool { return errors.Is(err, ErrIndexNotReady) }

// 索引器自身的失败分类。装配缺失 fail-closed（不静默跳过索引）：跳过会让「索引没建」
// 表现成「没有可召回记忆」，正是 V-5 要消除的形态。
var (
	errIndexerNotAssembled    = errors.New("memory: memory indexer is not assembled")
	errIndexGenerationRetired = errors.New("memory: 索引代际已退役")
	errIndexVectorCount       = errors.New("memory: embedding 返回的向量数与输入不符")
	errIndexModelMismatch     = errors.New("memory: embedding 返回的模型与索引代际不符")
	errIndexDimensionMismatch = errors.New("memory: embedding 返回的维度与索引代际不符")
)

// embeddingGenerationRetired 是 `kb_index_generations.status` 的退役字面量（与
// `docs/contracts/schema.pg.sql` 的 CHECK 闭集同源）。本包不 import knowledge（依赖方向），
// 故在此声明同一字面量而不是复用。
// The retired generation literal, mirrored from the schema closure.
const embeddingGenerationRetired = "retired"

// defaultMemoryIndexBatchSize 是回填的每批条数（与 §3.3 的 embedding 批量缺省同值）。
// The backfill batch size, matching the §3.3 embedding batch default.
const defaultMemoryIndexBatchSize = 32

// maxBackfillBatches 是单次回填的批次数上限，只用来防止**存储违约**（写入成功却查不出
// 向量）把启动拖进死循环；它不是一个产品容量上限：正常数据每批必减少缺口，用不到它。
// The batch cap only guards against a store that reports writes it did not persist.
const maxBackfillBatches = 4096

// EmbeddingGeneration 是一条索引代际的读面（`kb_index_generations` 的子集）。
// EmbeddingGeneration is the read face of one index generation.
type EmbeddingGeneration struct {
	// Generation 是代际号（主键）。
	Generation int
	// Model 是产出该代际向量的模型标识。
	Model string
	// Dimensions 是该代际的向量维度。
	Dimensions int
	// Status 是代际状态（`building` / `validated` / `active` / `retired` 闭集）。
	Status string
}

// MemoryEmbeddingStore 是记忆向量写入的**消费方端口**（本包声明，实现落
// `storage/postgres`）。四条方法合起来支撑两个用例：写后单条补索引与按代际回填。
// The consumer-side port for memory vector writes, implemented by storage/postgres.
type MemoryEmbeddingStore interface {
	// EmbeddingGeneration 按代际号读一行；代际不存在即错误（不返回零值）。
	EmbeddingGeneration(ctx context.Context, generation int) (EmbeddingGeneration, error)

	// ListUnindexedArchiveEntries 按 `id` 升序返回该代际下**尚无向量**的生效归档条目，
	// 至多 limit 条；limit 必须为正（回填按批推进，不接受无界查询）。
	ListUnindexedArchiveEntries(ctx context.Context, generation, limit int) ([]MemoryEntry, error)

	// ListUnindexedEntriesByContent 返回该（分层, 分区, 正文）下**尚无本代际向量**的
	// 全部条目：写后补索引按它去重，避免同文并发写入各补一次。
	ListUnindexedEntriesByContent(ctx context.Context, generation int, scope core.Scope,
		section core.Section, content string) ([]MemoryEntry, error)

	// WriteMemoryEmbeddings 在**一个短事务**内校验代际（存在、非退役、维度一致）并逐行
	// upsert；entries 与 vectors 必须等长且顺序对应。事务内**不得**发起外部调用。
	WriteMemoryEmbeddings(ctx context.Context, generation int, entries []MemoryEntry, vectors [][]float32) error
}

// IndexerDeps 是索引器的装配面：每个依赖都是显式字段（无包级状态、无 context 取值）。
// IndexerDeps is the explicit assembly surface of the indexer.
type IndexerDeps struct {
	// Store 是向量写入端口——必填。
	Store MemoryEmbeddingStore
	// Embedder 是 embedding 客户端（app 侧接的是**带审计**的那一个）——必填。
	Embedder Embedder
	// Generation 是装配期固定并显式传入的代际号——必填且为正。
	Generation int
	// BatchSize 是回填的每批条数；<= 0 取 defaultMemoryIndexBatchSize。
	BatchSize int
	// Logger 记录批次结果与失败原因；nil 取 slog.Default。
	Logger *slog.Logger
}

// Indexer 是记忆向量的唯一写入实现（写后增量 + 启动期回填）。
// Indexer is the single write implementation for memory vectors.
type Indexer struct {
	deps    IndexerDeps
	logger  *slog.Logger
	initErr error
}

// NewIndexer 组装索引器。与 `NewSemanticStore` 同一形态：签名不带 error，装配失败记进
// 实例，任何一次调用都返回该错误（fail-closed，不 panic、不静默跳过）。
// Assembly failures are recorded and reported by every call rather than panicking.
func NewIndexer(deps IndexerDeps) *Indexer {
	i := &Indexer{deps: deps, logger: deps.Logger}
	if i.logger == nil {
		i.logger = slog.Default()
	}
	if i.deps.BatchSize <= 0 {
		i.deps.BatchSize = defaultMemoryIndexBatchSize
	}
	if i.deps.Store == nil || i.deps.Embedder == nil || i.deps.Generation <= 0 {
		i.initErr = errIndexerNotAssembled
	}
	return i
}

// ready 是每个入口的第一道检查。
func (i *Indexer) ready() error {
	if i.initErr != nil {
		return i.initErr
	}
	if i.deps.Store == nil || i.deps.Embedder == nil || i.deps.Generation <= 0 {
		return errIndexerNotAssembled
	}
	return nil
}

// IndexEntry 在一条归档条目落库之后补建当前代际的向量。
//
// 按正文而不是按主键找目标：`Append` 不返回主键，同文多行（同一句话被保存两次）也
// 应当一次补齐——列表方法已带「缺本代际向量」判据，已索引的行不会重复写。核心区
// 条目不进归档区召回，**不**建向量（省一次外部调用，也让配额不被索引放大）。
// IndexEntry indexes every unindexed row with this content; core entries are skipped.
func (i *Indexer) IndexEntry(ctx context.Context, scope core.Scope, section core.Section, content string) error {
	if err := i.ready(); err != nil {
		return err
	}
	if section != core.SectionArchive {
		return nil
	}
	normalized := strings.TrimSpace(content)
	if normalized == "" {
		return nil
	}
	entries, err := i.deps.Store.ListUnindexedEntriesByContent(ctx, i.deps.Generation, scope,
		section, normalized)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	return i.indexBatch(ctx, entries)
}

// BackfillReport 是一次回填的读数（启动摘要与测试共用）。
// BackfillReport is one backfill's reading.
type BackfillReport struct {
	// Indexed 是本次写入向量的条目数。
	Indexed int
	// Batches 是实际发出的嵌入批次数。
	Batches int
}

// Backfill 按显式代际推进，直到没有缺口：每批「列表 → 嵌入 → 短事务写」。遇错即止，
// 已写批次保留（upsert 幂等，下次启动从剩下的缺口继续）。
// Backfill advances batch by batch until no gap remains; it stops at the first error and
// leaves the written batches in place for the next startup.
func (i *Indexer) Backfill(ctx context.Context) (BackfillReport, error) {
	var report BackfillReport
	if err := i.ready(); err != nil {
		return report, err
	}
	for {
		if report.Batches >= maxBackfillBatches {
			return report, fmt.Errorf(
				"记忆向量回填：%d 批后仍有缺口（存储未按写入结果落库？）——停止本次回填", report.Batches)
		}
		entries, err := i.deps.Store.ListUnindexedArchiveEntries(ctx, i.deps.Generation, i.deps.BatchSize)
		if err != nil {
			return report, err
		}
		if len(entries) == 0 {
			return report, nil
		}
		if err := i.indexBatch(ctx, entries); err != nil {
			return report, err
		}
		report.Indexed += len(entries)
		report.Batches++
	}
}

// indexBatch 嵌入一批并写入。顺序固定：**先**读代际（退役即不发出调用）→ 嵌入 →
// 校验数量 / 模型 / 维度 → 短事务写入；任何一步失败都原样返回，由调用方决定重试。
// indexBatch embeds outside any transaction, then writes in one short transaction.
func (i *Indexer) indexBatch(ctx context.Context, entries []MemoryEntry) error {
	if len(entries) == 0 {
		return nil
	}
	generation, err := i.deps.Store.EmbeddingGeneration(ctx, i.deps.Generation)
	if err != nil {
		return err
	}
	if generation.Status == embeddingGenerationRetired {
		return fmt.Errorf("%w: 代际 %d", errIndexGenerationRetired, i.deps.Generation)
	}

	texts := make([]string, len(entries))
	for k := range entries {
		texts[k] = entries[k].Content
	}
	batch, err := i.deps.Embedder.Embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("记忆向量化失败（代际 %d）: %w", i.deps.Generation, err)
	}
	if len(batch.Vectors) != len(entries) {
		return fmt.Errorf("%w: 输入 %d 条，返回 %d 个向量", errIndexVectorCount, len(entries), len(batch.Vectors))
	}
	// 空模型 = 客户端未声明；此时以代际行为准（召回侧要求的非空模型由查询客户端保证）。
	if batch.Model != "" && batch.Model != generation.Model {
		return fmt.Errorf("%w: 返回模型 %q，代际 %d 模型 %q", errIndexModelMismatch,
			batch.Model, i.deps.Generation, generation.Model)
	}
	for k, vector := range batch.Vectors {
		if len(vector) != generation.Dimensions {
			return fmt.Errorf("%w: 第 %d 条维度 %d，代际 %d 维度 %d", errIndexDimensionMismatch,
				k+1, len(vector), i.deps.Generation, generation.Dimensions)
		}
	}

	if err := i.deps.Store.WriteMemoryEmbeddings(ctx, i.deps.Generation, entries, batch.Vectors); err != nil {
		return err
	}
	i.logger.InfoContext(ctx, "记忆向量：批次已写入",
		"generation", i.deps.Generation, "entries", len(entries), "prompt_tokens", batch.PromptTokens)
	return nil
}

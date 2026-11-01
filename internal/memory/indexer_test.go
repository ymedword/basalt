package memory

// 本文件是记忆向量索引器的单测（V-5）：不连库、不发网络——存储与 embedding 都是进程内
// 假实现，逐条钉住「先读代际再嵌入」「退役不发出调用」「按批推进直到无缺口」「缺口分类」。
// The indexer's unit tests: in-process fakes pin the call order, the retired guard, the
// batch loop, and the gap classification.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeEmbeddingStore 是 `MemoryEmbeddingStore` 的进程内实现：pending 里的条目就是
// 「尚无向量」的集合，写入时移出——回填循环因此能自然收敛（真实现靠 upsert + 缺口查询
// 达到同一效果）。
// fakeEmbeddingStore keeps the pending set so the backfill loop converges like the real store.
type fakeEmbeddingStore struct {
	generation EmbeddingGeneration
	genErr     error
	listErr    error
	writeErr   error
	pending    []MemoryEntry
	written    map[int64][]float32
	genCalls   int
	listCalls  int
	writeCalls int
}

func newFakeEmbeddingStore(entries ...MemoryEntry) *fakeEmbeddingStore {
	return &fakeEmbeddingStore{
		generation: EmbeddingGeneration{Generation: 1, Model: "fake-embedding", Dimensions: 3, Status: "active"},
		pending:    entries,
		written:    map[int64][]float32{},
	}
}

func (f *fakeEmbeddingStore) EmbeddingGeneration(_ context.Context, generation int) (EmbeddingGeneration, error) {
	f.genCalls++
	if f.genErr != nil {
		return EmbeddingGeneration{}, f.genErr
	}
	out := f.generation
	out.Generation = generation
	return out, nil
}

func (f *fakeEmbeddingStore) ListUnindexedArchiveEntries(_ context.Context, generation, limit int) ([]MemoryEntry, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	if generation != f.generation.Generation {
		return nil, errors.New("测试：代际号不符")
	}
	if limit > len(f.pending) {
		limit = len(f.pending)
	}
	out := make([]MemoryEntry, limit)
	copy(out, f.pending[:limit])
	return out, nil
}

func (f *fakeEmbeddingStore) ListUnindexedEntriesByContent(_ context.Context, generation int,
	scope core.Scope, section core.Section, content string,
) ([]MemoryEntry, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []MemoryEntry
	for _, entry := range f.pending {
		if entry.Scope.Agent == scope.Agent && entry.Section == section &&
			strings.TrimSpace(entry.Content) == content {
			out = append(out, entry)
		}
	}
	return out, nil
}

func (f *fakeEmbeddingStore) WriteMemoryEmbeddings(_ context.Context, generation int,
	entries []MemoryEntry, vectors [][]float32,
) error {
	f.writeCalls++
	if f.writeErr != nil {
		return f.writeErr
	}
	if generation != f.generation.Generation {
		return errors.New("测试：代际号不符")
	}
	if len(entries) != len(vectors) {
		return errors.New("测试：条目与向量不等长")
	}
	kept := f.pending[:0]
	for _, entry := range f.pending {
		matched := false
		for k, candidate := range entries {
			if candidate.ID == entry.ID {
				f.written[entry.ID] = vectors[k]
				matched = true
				break
			}
		}
		if !matched {
			kept = append(kept, entry)
		}
	}
	f.pending = kept
	return nil
}

// fakeIndexEmbedder 记录每次调用的输入，并按维度生成确定性向量。
// fakeIndexEmbedder records inputs and emits deterministic vectors.
type fakeIndexEmbedder struct {
	model      string
	dimensions int
	calls      int
	inputs     [][]string
	err        error
	vectorN    int
}

func (f *fakeIndexEmbedder) Embed(_ context.Context, texts []string) (EmbedBatch, error) {
	f.calls++
	f.inputs = append(f.inputs, append([]string(nil), texts...))
	if f.err != nil {
		return EmbedBatch{}, f.err
	}
	count := len(texts)
	if f.vectorN > 0 {
		count = f.vectorN
	}
	vectors := make([][]float32, count)
	for k := range vectors {
		vector := make([]float32, f.dimensions)
		vector[0] = float32(k + 1)
		vectors[k] = vector
	}
	return EmbedBatch{Vectors: vectors, Model: f.model, Dimensions: f.dimensions}, nil
}

// archiveEntry 造一条待索引的归档条目。
func archiveEntry(id int64, scope core.Scope, content string) MemoryEntry {
	return MemoryEntry{ID: id, Scope: scope, Section: core.SectionArchive, Content: content, State: StateActive}
}

// TestIndexer_BackfillIndexesUntilNoGap 守回填按批推进且可重入：3 条、批 2 → 两批写完；
// 再跑一次立即返回零读数（幂等，不重复嵌入）。
// 逆改点：把循环改成只跑一批 → 第三批永远留缺口，第二次 Backfill 读到 Indexed > 0。
func TestIndexer_BackfillIndexesUntilNoGap(t *testing.T) {
	store := newFakeEmbeddingStore(
		archiveEntry(1, core.Scope{}, "甲"),
		archiveEntry(2, core.Scope{Agent: "demo"}, "乙"),
		archiveEntry(3, core.Scope{Agent: "demo"}, "丙"),
	)
	embedder := &fakeIndexEmbedder{model: "fake-embedding", dimensions: 3}
	indexer := NewIndexer(IndexerDeps{Store: store, Embedder: embedder, Generation: 1, BatchSize: 2})

	report, err := indexer.Backfill(context.Background())
	require.NoError(t, err)
	require.Equal(t, BackfillReport{Indexed: 3, Batches: 2}, report)
	require.Equal(t, 2, embedder.calls, "3 条、批 2 → 两次嵌入调用")
	require.Len(t, store.written, 3)
	require.Empty(t, store.pending)

	report, err = indexer.Backfill(context.Background())
	require.NoError(t, err)
	require.Equal(t, BackfillReport{}, report, "无缺口时回填是零读数的空操作")
	require.Equal(t, 2, embedder.calls, "无缺口时不再发嵌入调用")
}

// TestIndexer_IndexEntryIndexesOnlySameContentGaps 守写后补索引：只补（分层, 分区, 正文）
// 下尚无向量的行，同文两行一次补齐；核心区不建向量；重复调用是空操作。
// 逆改点：把分区判据删掉 → 核心区也发嵌入调用；把 content 过滤改成正文本比对 → 同文两行只补一条。
func TestIndexer_IndexEntryIndexesOnlySameContentGaps(t *testing.T) {
	scope := core.Scope{Agent: "demo"}
	store := newFakeEmbeddingStore(
		archiveEntry(7, scope, "同一句话"),
		archiveEntry(8, scope, "同一句话"),
		archiveEntry(9, scope, "别的话"),
	)
	embedder := &fakeIndexEmbedder{model: "fake-embedding", dimensions: 3}
	indexer := NewIndexer(IndexerDeps{Store: store, Embedder: embedder, Generation: 1})

	require.NoError(t, indexer.IndexEntry(context.Background(), scope, core.SectionCore, "核心不入索引"))
	require.Zero(t, embedder.calls, "核心区条目不进归档区召回，不得触发嵌入")

	require.NoError(t, indexer.IndexEntry(context.Background(), scope, core.SectionArchive, "  同一句话  "))
	require.Equal(t, 1, embedder.calls)
	require.Equal(t, []string{"同一句话", "同一句话"}, embedder.inputs[0], "同文两行一次补齐（正文已规范化）")
	require.Len(t, store.written, 2)
	require.Len(t, store.pending, 1, "别的话仍未索引")

	require.NoError(t, indexer.IndexEntry(context.Background(), scope, core.SectionArchive, "同一句话"))
	require.Equal(t, 1, embedder.calls, "已索引同文行不重复嵌入")
}

// TestIndexer_RetiredGenerationRefusesBeforeEmbedding 守退役代际拒写且**在嵌入之前**拒绝：
// 已退役代际不该为一次注定失败的写入花一次外部调用。
// 逆改点：把代际读取挪到 Embed 之后 → 本用例的 calls 断言红。
func TestIndexer_RetiredGenerationRefusesBeforeEmbedding(t *testing.T) {
	store := newFakeEmbeddingStore(archiveEntry(1, core.Scope{}, "甲"))
	store.generation.Status = "retired"
	embedder := &fakeIndexEmbedder{model: "fake-embedding", dimensions: 3}
	indexer := NewIndexer(IndexerDeps{Store: store, Embedder: embedder, Generation: 1})

	_, err := indexer.Backfill(context.Background())
	require.ErrorIs(t, err, errIndexGenerationRetired)
	require.Zero(t, embedder.calls, "退役代际不得发出嵌入调用")
	require.Zero(t, store.writeCalls, "退役代际不得进入写事务")
}

// TestIndexer_VectorCountMismatchFailsClosed 守向量数与输入不符即整批拒绝（不写半批）。
// 逆改点：去掉长度校验 → 写入被调用，本用例红。
func TestIndexer_VectorCountMismatchFailsClosed(t *testing.T) {
	store := newFakeEmbeddingStore(
		archiveEntry(1, core.Scope{}, "甲"),
		archiveEntry(2, core.Scope{}, "乙"),
	)
	embedder := &fakeIndexEmbedder{model: "fake-embedding", dimensions: 3, vectorN: 1}
	indexer := NewIndexer(IndexerDeps{Store: store, Embedder: embedder, Generation: 1})

	_, err := indexer.Backfill(context.Background())
	require.ErrorIs(t, err, errIndexVectorCount)
	require.Zero(t, store.writeCalls, "数量不符不得写半批")
	require.Empty(t, store.written)
}

// TestIndexer_ModelAndDimensionMismatchFailClosed 守模型 / 维度与代际不符即拒绝：
// 向量写进别的模型的代际，召回时会静默匹配不到（比报错更难查）。
// 逆改点：删掉任一校验 → 对应断言红。
func TestIndexer_ModelAndDimensionMismatchFailClosed(t *testing.T) {
	entry := archiveEntry(1, core.Scope{}, "甲")

	t.Run("模型不符", func(t *testing.T) {
		store := newFakeEmbeddingStore(entry)
		embedder := &fakeIndexEmbedder{model: "other-model", dimensions: 3}
		indexer := NewIndexer(IndexerDeps{Store: store, Embedder: embedder, Generation: 1})
		_, err := indexer.Backfill(context.Background())
		require.ErrorIs(t, err, errIndexModelMismatch)
		require.Zero(t, store.writeCalls)
	})

	t.Run("维度不符", func(t *testing.T) {
		store := newFakeEmbeddingStore(entry)
		embedder := &fakeIndexEmbedder{model: "fake-embedding", dimensions: 4}
		indexer := NewIndexer(IndexerDeps{Store: store, Embedder: embedder, Generation: 1})
		_, err := indexer.Backfill(context.Background())
		require.ErrorIs(t, err, errIndexDimensionMismatch)
		require.Zero(t, store.writeCalls)
	})
}

// TestIndexer_NotAssembledFailsClosed 守装配缺失：缺存储 / 缺嵌入 / 缺代际一律拒绝，
// 不静默跳过（跳过会让"索引没建"表现成"没有可召回记忆"）。
// 逆改点：把 ready() 改成返回 nil → 本用例红。
func TestIndexer_NotAssembledFailsClosed(t *testing.T) {
	indexer := NewIndexer(IndexerDeps{})
	require.ErrorIs(t, indexer.IndexEntry(context.Background(), core.Scope{}, core.SectionArchive, "甲"),
		errIndexerNotAssembled)
	_, err := indexer.Backfill(context.Background())
	require.ErrorIs(t, err, errIndexerNotAssembled)
}

// TestIndexer_BackfillStopsOnFirstStoreError 守遇错即止：已写批次保留，错误原样上抛，
// 下次启动从剩余缺口继续（不在同一次启动里反复重试同一个失败）。
// 逆改点：把错误吞掉继续循环 → 本用例拿到 nil error。
func TestIndexer_BackfillStopsOnFirstStoreError(t *testing.T) {
	store := newFakeEmbeddingStore(archiveEntry(1, core.Scope{}, "甲"))
	store.listErr = errors.New("库不可用")
	embedder := &fakeIndexEmbedder{model: "fake-embedding", dimensions: 3}
	indexer := NewIndexer(IndexerDeps{Store: store, Embedder: embedder, Generation: 1})

	report, err := indexer.Backfill(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "库不可用")
	require.Equal(t, BackfillReport{}, report)
	require.Zero(t, embedder.calls)
}

// TestIndexer_EmbedFailureKeepsGapForRetry 守嵌入失败不写库：缺口保留、下次重试。
// 逆改点：忽略 Embed 错误继续写入 → writeCalls 断言红。
func TestIndexer_EmbedFailureKeepsGapForRetry(t *testing.T) {
	store := newFakeEmbeddingStore(archiveEntry(1, core.Scope{}, "甲"))
	embedder := &fakeIndexEmbedder{model: "fake-embedding", dimensions: 3, err: errors.New("上游 502")}
	indexer := NewIndexer(IndexerDeps{Store: store, Embedder: embedder, Generation: 1})

	_, err := indexer.Backfill(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "上游 502")
	require.Zero(t, store.writeCalls)
	require.Len(t, store.pending, 1, "缺口必须保留")
}

// TestIsIndexNotReady 守缺口哨兵的分类只认自己的错误链（不误判装配缺失 / 普通失败）。
// TestIsIndexNotReady keeps the gap sentinel distinct from other failures.
func TestIsIndexNotReady(t *testing.T) {
	require.True(t, IsIndexNotReady(ErrIndexNotReady))
	require.True(t, IsIndexNotReady(errors.Join(errors.New("外层"), ErrIndexNotReady)))
	require.False(t, IsIndexNotReady(errSemanticNotAssembled))
	require.False(t, IsIndexNotReady(errors.New("无关错误")))
	require.False(t, IsIndexNotReady(nil))
}

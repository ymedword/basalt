package app

// 本文件锁死 `memory_index.go` 的装配面（V-5）：**只有保存成功才补索引**、未绑定槽位是
// no-op、补索引失败**只 WARN 不改写保存结局**，以及索引器的可选装配判据。
//
// 这一层判的是**接线形状**，不是索引正确性：嵌入顺序、代际校验、缺口分类与回填循环只有
// 一个实现在 `internal/memory`，由 `indexer_test.go` 判；这里若也重写一遍，就会出现第二套
// 写入语义——正是本文件要防的。
//
// This file pins the assembly surface of memory_index.go: index only after a successful
// save, an unbound slot is a no-op, an indexing failure only warns, and the indexer is
// assembled only when the embedding path is complete. The indexing semantics themselves
// live in internal/memory.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// indexTestFacade 是记忆门面的替身：只判 Append 是否被调用与成功，读写方法空实现
// （本文件不验证门面语义，`internal/memory` 的用例覆盖）。
// indexTestFacade records Append calls and its injected outcome.
type indexTestFacade struct {
	appends int
	fail    error
}

// Append 计数并按注入值回结果。
func (f *indexTestFacade) Append(context.Context, core.Scope, core.Section, string) error {
	f.appends++
	return f.fail
}

// Load 是空实现。
func (f *indexTestFacade) Load(context.Context, core.Scope) (string, error) { return "", nil }

// RecallByKeyword 是空实现。
func (f *indexTestFacade) RecallByKeyword(context.Context, core.Scope, string) ([]string, error) {
	return nil, nil
}

// 编译期断言：替身满足门面接口。
var _ core.LongTermMemoryStore = (*indexTestFacade)(nil)

// indexTestStore 是 `memory.MemoryEmbeddingStore` 的进程内替身：pending 即「尚无向量」
// 的集合，写入时移出；与 `internal/memory` 用例里的同名替身同形，但本包不 import
// 测试包内类型，故各自申明。内嵌门面替身让同一实例同时满足 `core.LongTermMemoryStore`
// ——生产里传给 `newMemoryIndexer` 的正是同时满足两个接口的门面实现。
// indexTestStore keeps the pending set so a write removes the gap, and embeds the facade
// double so it also satisfies core.LongTermMemoryStore, like the production carrier.
type indexTestStore struct {
	indexTestFacade
	generation memory.EmbeddingGeneration
	pending    []memory.MemoryEntry
	written    map[int64][]float32
}

// EmbeddingGeneration 回注入的代际行（代际号按入参回显）。
func (s *indexTestStore) EmbeddingGeneration(_ context.Context, generation int) (memory.EmbeddingGeneration, error) {
	out := s.generation
	out.Generation = generation
	return out, nil
}

// ListUnindexedArchiveEntries 按 limit 回前若干条 pending。
func (s *indexTestStore) ListUnindexedArchiveEntries(_ context.Context, _, limit int) ([]memory.MemoryEntry, error) {
	if limit > len(s.pending) {
		limit = len(s.pending)
	}
	return append([]memory.MemoryEntry(nil), s.pending[:limit]...), nil
}

// ListUnindexedEntriesByContent 按（分层, 分区, 正文）过滤 pending。
func (s *indexTestStore) ListUnindexedEntriesByContent(_ context.Context, _ int, scope core.Scope,
	section core.Section, content string,
) ([]memory.MemoryEntry, error) {
	var out []memory.MemoryEntry
	for _, entry := range s.pending {
		if entry.Scope == scope && entry.Section == section && entry.Content == content {
			out = append(out, entry)
		}
	}
	return out, nil
}

// WriteMemoryEmbeddings 记下向量并把已写条目移出 pending。
func (s *indexTestStore) WriteMemoryEmbeddings(_ context.Context, _ int,
	entries []memory.MemoryEntry, vectors [][]float32,
) error {
	kept := s.pending[:0]
	for _, entry := range s.pending {
		written := false
		for k, candidate := range entries {
			if candidate.ID == entry.ID {
				s.written[entry.ID] = vectors[k]
				written = true
				break
			}
		}
		if !written {
			kept = append(kept, entry)
		}
	}
	s.pending = kept
	return nil
}

// 编译期断言：替身满足索引器声明的写入端口。
var _ memory.MemoryEmbeddingStore = (*indexTestStore)(nil)

// indexTestEmbedder 记录嵌入调用，可注入失败；成功时按代际维度回确定性向量。
// indexTestEmbedder records calls and can fail on demand.
type indexTestEmbedder struct {
	calls  int
	inputs [][]string
	err    error
	model  string
	dims   int
}

// Embed 记录输入并按注入结果返回。
func (e *indexTestEmbedder) Embed(_ context.Context, texts []string) (memory.EmbedBatch, error) {
	e.calls++
	e.inputs = append(e.inputs, append([]string(nil), texts...))
	if e.err != nil {
		return memory.EmbedBatch{}, e.err
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = make([]float32, e.dims)
		vectors[i][0] = float32(i + 1)
	}
	return memory.EmbedBatch{Vectors: vectors, Model: e.model, Dimensions: e.dims}, nil
}

// indexTestEntry 造一条待补索引的归档条目。
func indexTestEntry(id int64, scope core.Scope, content string) memory.MemoryEntry {
	return memory.MemoryEntry{
		ID: id, Scope: scope, Section: core.SectionArchive, Content: content, State: "active",
	}
}

// newIndexTestSlot 组装「一个待索引条目 + 可注入失败的嵌入」的已绑定槽位。
// newIndexTestSlot builds a bound slot over one pending entry.
func newIndexTestSlot(t *testing.T, logger *slog.Logger) (*memoryIndexSlot, *indexTestStore, *indexTestEmbedder) {
	t.Helper()
	store := &indexTestStore{
		generation: memory.EmbeddingGeneration{Model: "embed-test", Dimensions: 2, Status: "active"},
		pending:    []memory.MemoryEntry{indexTestEntry(11, core.Scope{Agent: "alice"}, "成本评估")},
		written:    map[int64][]float32{},
	}
	embedder := &indexTestEmbedder{model: "embed-test", dims: 2}
	slot := newMemoryIndexSlot(logger)
	slot.Bind(memory.NewIndexer(memory.IndexerDeps{Store: store, Embedder: embedder, Generation: 1}))
	return slot, store, embedder
}

// discardLogger 是吞掉日志的 logger（本文件只在需要断言的用例里保留输出）。
// discardLogger drops the output; only the warning cases keep it.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
}

// TestIndexingCarrier_IndexesOnlyAfterSuccessfulAppend 判 V-5 的写入顺序：保存成功才补
// 索引（一次嵌入、一条向量落库）；保存失败**不得**触发嵌入——省一次外部调用，也让
// 「没记住」与「记住了但索引失败」两种结局不混。
// 逆改点：把索引调用挪到保存之前 → 保存失败时嵌入计数仍会增加（**前提是夹具先给该
// 内容种一条缺口**，否则索引按内容找不到可补条目，断言空转），断言红；保存成功后
// 不索引 → 嵌入零调用、向量表零行，断言红。
func TestIndexingCarrier_IndexesOnlyAfterSuccessfulAppend(t *testing.T) {
	slot, store, embedder := newIndexTestSlot(t, discardLogger())
	facade := &indexTestFacade{}
	carrier := &indexingCarrier{LongTermMemoryStore: facade, slot: slot}
	scope := core.Scope{Agent: "alice"}

	require.NoError(t, carrier.Append(context.Background(), scope, core.SectionArchive, "成本评估"))
	require.Equal(t, 1, facade.appends, "保存本身被调用")
	require.Equal(t, 1, embedder.calls, "保存成功后补一次索引")
	require.Equal(t, [][]string{{"成本评估"}}, embedder.inputs, "嵌入正文逐字来自保存的入参")
	require.Contains(t, store.written, int64(11), "向量落到对应条目")
	require.Empty(t, store.pending, "缺口被补上")

	// 失败档的判别力靠「该内容已有一个未补的缺口」：索引是**按内容**找缺口的
	// （`IndexEntry` → `ListUnindexedEntriesByContent`），内容没有缺口时，把索引挪到
	// 保存之前也不会产生嵌入调用——断言就成了空转（2026-10-30 逆改实测：原夹具红不了）。
	// 种一条与第二次保存同内容的缺口，才让「保存失败不得触发嵌入」真正可判。
	// The failure case needs a pre-existing gap for the same content; without it the
	// ordering assertion cannot fire, no matter how the carrier is mutated.
	store.pending = append(store.pending, indexTestEntry(12, scope, "另一句"))
	facade.fail = errors.New("磁盘已满")
	require.Error(t, carrier.Append(context.Background(), scope, core.SectionArchive, "另一句"))
	require.Equal(t, 2, facade.appends)
	require.Equal(t, 1, embedder.calls, "保存失败不得触发嵌入")
	require.Len(t, store.pending, 1, "保存失败：缺口原样留给回填")
}

// TestIndexingCarrier_UnboundSlotIsNoOp 判未绑定 / nil 槽位是 no-op：embedding 未配置时
// 记忆保存照常成功（语义路径未装配是正常分支，缺口留给启动期回填），不 panic。
// 逆改点：未绑定时报错 → Append 返回错误，保存被装配缺失连坐；直接解引用 indexer →
// panic 红。
func TestIndexingCarrier_UnboundSlotIsNoOp(t *testing.T) {
	facade := &indexTestFacade{}
	ctx := context.Background()
	scope := core.Scope{Agent: "alice"}

	unbound := &indexingCarrier{LongTermMemoryStore: facade, slot: newMemoryIndexSlot(nil)}
	require.NoError(t, unbound.Append(ctx, scope, core.SectionArchive, "成本评估"),
		"未绑定槽位不得让保存失败")

	noSlot := &indexingCarrier{LongTermMemoryStore: facade}
	require.NoError(t, noSlot.Append(ctx, scope, core.SectionArchive, "成本评估"),
		"nil 槽位不得 panic")
	require.Equal(t, 2, facade.appends)
}

// TestMemoryIndexSlot_IndexFailureOnlyWarns 判补索引失败的结局：条目**已经落库**，保存
// 保持成功；失败走 WARN 日志（可观测性双轨），缺口由召回侧显式报错与下次启动的回填兜底。
// 逆改点：把索引错误回传成 Append 失败 → NoError 红（用户看到"保存失败"却发现条目已存）；
// 吞掉日志 → 日志断言红（线上排障黑洞）。
func TestMemoryIndexSlot_IndexFailureOnlyWarns(t *testing.T) {
	var logs bytes.Buffer
	slot, store, embedder := newIndexTestSlot(t, slog.New(slog.NewTextHandler(&logs, nil)))
	embedder.err = errors.New("上游 502")
	facade := &indexTestFacade{}
	carrier := &indexingCarrier{LongTermMemoryStore: facade, slot: slot}

	require.NoError(t, carrier.Append(context.Background(), core.Scope{Agent: "alice"},
		core.SectionArchive, "成本评估"), "补索引失败不得改写保存结局")
	require.Equal(t, 1, embedder.calls, "确实尝试过索引（不是静默跳过）")
	require.Empty(t, store.written, "嵌入失败不得写向量")
	require.Len(t, store.pending, 1, "缺口保留给下次回填")
	require.Contains(t, logs.String(), "level=WARN", "失败必须留 WARN")
	require.Contains(t, logs.String(), "保存后补索引失败", "日志必须点名失败阶段")
	require.Contains(t, logs.String(), "上游 502", "日志必须带失败原因")
}

// TestMemoryIndexSlot_UnboundIndexAppendedIsNoOp 判槽位自身的空绑定语义（不经载体直接调）：
// 未绑定即静默返回——没有可补的索引不是失败。
// 逆改点：未绑定时 panic 或写日志噪声 → 本用例的「零日志」断言红。
func TestMemoryIndexSlot_UnboundIndexAppendedIsNoOp(t *testing.T) {
	var logs bytes.Buffer
	slot := newMemoryIndexSlot(slog.New(slog.NewTextHandler(&logs, nil)))
	slot.IndexAppended(context.Background(), core.Scope{}, core.SectionArchive, "甲")
	require.Empty(t, logs.String(), "未绑定不是失败：不写日志")
}

// TestNewMemoryIndexer_RequiresAssembly 判可选装配的判据：embedding 已配置、代际已固定
// 且载体支持写入端口三者齐备才构造索引器；任一缺失回 nil（语义路径未装配的正常分支，
// 不拒绝启动）。若这里硬造一个索引器，未配置 embedding 的实例会在启动回填时反复报错。
// 逆改点：去掉任一判据 → 对应 nil 断言红（例如 markdown 档拿到非 nil 索引器）。
func TestNewMemoryIndexer_RequiresAssembly(t *testing.T) {
	embedder := &knowledgeEmbedderStub{}

	require.Nil(t, newMemoryIndexer(&indexTestStore{}, nil, nil), "知识库未装配")
	require.Nil(t, newMemoryIndexer(&indexTestStore{},
		&knowledgeWiring{Generation: 1}, nil), "embedding 未配置")
	require.Nil(t, newMemoryIndexer(&indexTestStore{},
		&knowledgeWiring{Embedder: embedder}, nil), "代际未固定")
	require.Nil(t, newMemoryIndexer(keywordOnlyCarrier{},
		&knowledgeWiring{Embedder: embedder, Generation: 1}, nil),
		"载体不实现写入端口（markdown 档）")
	require.NotNil(t, newMemoryIndexer(&indexTestStore{},
		&knowledgeWiring{Embedder: embedder, Generation: 1}, nil),
		"三者齐备必须装配")
}

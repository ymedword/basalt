package app

// 本文件是记忆向量索引在装配层的接线（V-5）：写时增量 + 启动期回填。
//
// 三个决定：
//  1. **写后补索引挂在门面之外**：`indexingCarrier` 包的是 `memory.Service`（工具拿到的
//     那个），不是底层载体——`Append` 返回时 per-scope 写锁已经释放，嵌入调用不会在锁
//     内发生（外部调用不进临界区；同步执行，不引入异步编排，宪法第 7 条）。
//  2. **两段绑定**：工具注册早于知识库装配，槽位先建、`knowledgeWire` 就绪后 `Bind`；
//     未绑定（embedding 未配置）即 no-op——记忆保存照常成功，向量由下次启动的回填补。
//  3. **失败只 WARN**：条目已经落库，**不得**把"补索引失败"改写成"保存失败"；缺口由
//     缺口检测（召回时显式报错）与下次启动的回填兜底。
//
// This file wires memory vector indexing: an incremental pass after save_memory (outside
// the facade's write lock), a two-stage bound slot, and a startup backfill. Indexing
// failures are logged, never turned into save failures.

import (
	"context"
	"log/slog"
	"sync"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// memoryIndexSlot 是两段绑定的索引槽位：未绑定时所有补索引调用是 no-op。
// The two-stage slot; an unbound slot makes every indexing call a no-op.
type memoryIndexSlot struct {
	mu      sync.RWMutex
	indexer *memory.Indexer
	logger  *slog.Logger
}

// newMemoryIndexSlot 建未绑定槽位。logger 为 nil 时兜底 slog.Default（失败路径必须有
// 日志——可观测性双轨，审计与日志两者都要）。
// newMemoryIndexSlot builds the unbound slot; a nil logger falls back to slog.Default.
func newMemoryIndexSlot(logger *slog.Logger) *memoryIndexSlot {
	if logger == nil {
		logger = slog.Default()
	}
	return &memoryIndexSlot{logger: logger}
}

// Bind 绑定索引器（装配期一次；nil 保持未绑定语义）。
// Bind publishes the indexer once during assembly.
func (s *memoryIndexSlot) Bind(indexer *memory.Indexer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.indexer = indexer
}

// IndexAppended 在条目落库之后补建当前代际的向量。核心里程之外的分区由 `IndexEntry`
// 自己判定；任何失败都只记 WARN——保存已经成功，缺口由回填兜底。
// IndexAppended indexes one saved entry and logs any failure; the save stays successful.
func (s *memoryIndexSlot) IndexAppended(ctx context.Context, scope core.Scope, section core.Section, content string) {
	s.mu.RLock()
	indexer := s.indexer
	s.mu.RUnlock()
	if indexer == nil {
		return
	}
	if err := indexer.IndexEntry(ctx, scope, section, content); err != nil {
		s.logger.WarnContext(ctx, "记忆向量：保存后补索引失败，缺口留给启动期回填",
			"scope", scope.Agent, "section", string(section), "error", err)
	}
}

// indexingCarrier 是带"写后补索引"的 `core.LongTermMemoryStore`：读方法原样转发，
// 只有 `Append` 在成功之后多走一步索引。包在**门面之上**（见文件头第 1 条）。
// indexingCarrier forwards every read verbatim and indexes after a successful Append.
type indexingCarrier struct {
	core.LongTermMemoryStore
	slot *memoryIndexSlot
}

// 编译期断言：装饰器仍是完整的记忆载体（工具侧只看到这个接口）。
// Compile-time assertion that the decorator is still a full memory store.
var _ core.LongTermMemoryStore = (*indexingCarrier)(nil)

// Append 先写门面，成功后再补索引：写入失败**不得**触发嵌入调用（省一次外部调用，
// 也让"没记住"与"记住了但索引失败"两种结局不混）。
// Append indexes only after the write succeeded.
func (c *indexingCarrier) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error {
	if err := c.LongTermMemoryStore.Append(ctx, scope, section, content); err != nil {
		return err
	}
	if c.slot != nil {
		c.slot.IndexAppended(ctx, scope, section, content)
	}
	return nil
}

// newMemoryIndexer 在 embedding 已配置、代际已固定且载体支持写入端口时构造索引器；
// 任一不满足即返回 nil（语义路径未装配的正常分支，不拒绝启动）。
// newMemoryIndexer returns nil when the embedding path is not assembled.
func newMemoryIndexer(carrier core.LongTermMemoryStore, wire *knowledgeWiring, logger *slog.Logger) *memory.Indexer {
	if wire == nil || wire.Embedder == nil || wire.Generation <= 0 {
		return nil
	}
	store, ok := carrier.(memory.MemoryEmbeddingStore)
	if !ok {
		return nil
	}
	return memory.NewIndexer(memory.IndexerDeps{
		Store:      store,
		Embedder:   memoryEmbedderOf(wire.Embedder),
		Generation: wire.Generation,
		BatchSize:  knowledgeDefaultEmbeddingBatchSize,
		Logger:     logger,
	})
}

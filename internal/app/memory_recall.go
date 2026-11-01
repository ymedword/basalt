package app

// 本文件是 `recall_memory` 语义召回的生产适配面（US-16 P-07）：工具层只声明消费方端口
// `tool.MemoryRecaller`，生产实现落在这里——与 `web_search` 的凭据槽位同一套**两段绑定**
// 形状（工具注册早于知识库装配，槽位先建、装配完成后 Bind）。
//
// 三个决定：
//  1. **语义召回只有一个实现**：`memory.SemanticStore.Recall` 已经承担"模式分流 + 身份
//     判定 + embedding + 载体召回"，本文件只做**形状转换**（端口类型 ↔ 领域类型）与
//     "未装配"错误收敛，**不**重写召回管线。
//  2. **embedding 客户端复用知识库栈的那一个**：`embedding.model_connection` 的连接解析、
//     代际固定与凭据解析都只有一份实现（`wire_knowledge.go`），记忆侧不另建取键路径。
//  3. **装配缺失失败关闭**：载体不满足窄持久化端口（如 markdown 档）或 embedding 未配置
//     时，语义路径回 `tool.ErrRecallUnavailable`，由工具翻译成可自我纠正的 `invalid_input`
//     ——**不**静默降级成关键词检索。
//
// This file adapts memory.SemanticStore to the tool-side recall port with the same
// two-stage slot pattern as web_search; the recall pipeline itself stays single-sourced
// in internal/memory, and a missing assembly fails closed instead of degrading silently.

import (
	"context"
	"fmt"
	"sync"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/tool"
)

// memoryRecallSlot 是两段绑定的槽位：工具注册时先建，知识库装配完成后 `Bind`。
// 未绑定时**失败关闭**（回 `tool.ErrRecallUnavailable`），不猜载体、不回退关键词路径。
// The two-stage slot: built before tool registration, bound once the knowledge stack
// exists; an unbound slot fails closed.
type memoryRecallSlot struct {
	mu    sync.RWMutex
	inner tool.MemoryRecaller
}

// 编译期断言：槽位可直接作为工具层的召回端口使用。
// Compile-time assertion that the slot satisfies the tool-side port.
var _ tool.MemoryRecaller = (*memoryRecallSlot)(nil)

// newMemoryRecallSlot 建未绑定槽位。
// newMemoryRecallSlot builds the unbound slot.
func newMemoryRecallSlot() *memoryRecallSlot { return &memoryRecallSlot{} }

// Bind 绑定召回实现（装配期一次；nil 保持未绑定语义）。
// Bind publishes the recall implementation once during assembly.
func (s *memoryRecallSlot) Bind(inner tool.MemoryRecaller) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner = inner
}

// RecallArchive 转发到已绑定的实现；未绑定即失败关闭。
// RecallArchive forwards to the bound implementation and fails closed when unbound.
func (s *memoryRecallSlot) RecallArchive(ctx context.Context, req tool.MemoryRecallRequest) ([]tool.MemoryRecallHit, error) {
	s.mu.RLock()
	inner := s.inner
	s.mu.RUnlock()
	if inner == nil {
		return nil, fmt.Errorf("%w: 召回端口尚未绑定", tool.ErrRecallUnavailable)
	}
	return inner.RecallArchive(ctx, req)
}

// memoryEmbedderAdapter 把知识库栈的 embedding 客户端接到 `memory.Embedder` 上：两个
// 接口的形状逐字段一致，但**必须各声明各的**（依赖方向：memory 不 import knowledge）。
// The adapter between the knowledge embedder and memory's consumer-side embedder.
type memoryEmbedderAdapter struct {
	inner knowledge.Embedder
}

// 编译期断言：适配器满足 memory 侧的消费方端口。
// Compile-time assertion that the adapter satisfies memory's port.
var _ memory.Embedder = memoryEmbedderAdapter{}

// Embed 逐字段转发；失败原样上抛（由 `memory.Recall` 决定如何分类）。
// Embed forwards the batch field by field.
func (a memoryEmbedderAdapter) Embed(ctx context.Context, texts []string) (memory.EmbedBatch, error) {
	batch, err := a.inner.Embed(ctx, texts)
	if err != nil {
		return memory.EmbedBatch{}, err
	}
	return memory.EmbedBatch{
		Vectors:      batch.Vectors,
		PromptTokens: batch.PromptTokens,
		Dimensions:   batch.Dimensions,
		Model:        batch.Model,
	}, nil
}

// memoryEmbedderOf 把可能为 nil 的 embedding 客户端转成 `memory.Embedder`：nil 接口
// **必须**保持 nil（`memory.Recall` 用它判定"未配置"，一个包着 nil 指针的非 nil 接口会
// 把未配置变成运行期 panic）。
// memoryEmbedderOf keeps an unconfigured embedder nil rather than wrapping a nil pointer.
func memoryEmbedderOf(inner knowledge.Embedder) memory.Embedder {
	if inner == nil {
		return nil
	}
	return memoryEmbedderAdapter{inner: inner}
}

// semanticRecallAdapter 是 `memory.SemanticStore` 的端口适配器：只做入参 / 出参的形状
// 转换与"未装配"错误收敛——模式分流、身份判定与 embedding 都在 `SemanticStore.Recall`
// 里，**不得**在这里重写一遍。
// The port adapter around SemanticStore.Recall; it only converts shapes and folds the
// unassembled errors into the tool-side sentinel.
type semanticRecallAdapter struct {
	store *memory.SemanticStore
}

// 编译期断言：适配器满足工具层的召回端口。
// Compile-time assertion that the adapter satisfies the tool-side port.
var _ tool.MemoryRecaller = (*semanticRecallAdapter)(nil)

// RecallArchive 把请求原样交给 `SemanticStore.Recall`：身份是**请求字段**，逐次传递、
// 逐次判定，不缓存结论（C-11）。名次按返回顺序编号（1 起）——载体已按相关性排序，
// 这里不重排、不截断。
// RecallArchive hands the request to Recall untouched and numbers the returned order.
func (a *semanticRecallAdapter) RecallArchive(ctx context.Context,
	req tool.MemoryRecallRequest,
) ([]tool.MemoryRecallHit, error) {
	entries, err := a.store.Recall(ctx, memory.RecallQuery{
		Principal:          req.Principal,
		AuthorizationAgent: req.AuthorizationAgent,
		Scope:              req.Scope,
		Query:              req.Query,
		Mode:               req.Mode,
		Limit:              req.Limit,
	})
	if err != nil {
		if memory.IsNotAssembled(err) {
			return nil, fmt.Errorf("%w: %v", tool.ErrRecallUnavailable, err)
		}
		// 索引缺口（V-5）单独收敛：装配在位、只是有生效归档记忆尚无当前代际的向量，
		// 工具层据此给"改用 mode=keyword 或稍后重试"，**不得**渲染成无匹配。
		// A vector gap is folded onto its own sentinel so the tool never renders it as
		// "no match".
		if memory.IsIndexNotReady(err) {
			return nil, fmt.Errorf("%w: %v", tool.ErrRecallIndexNotReady, err)
		}
		return nil, err
	}
	hits := make([]tool.MemoryRecallHit, 0, len(entries))
	for i, entry := range entries {
		hits = append(hits, tool.MemoryRecallHit{
			Rank:    i + 1,
			Source:  entry.Source(),
			Content: entry.Content,
		})
	}
	return hits, nil
}

// newSemanticRecaller 在记忆载体满足窄持久化端口时装配语义召回；不满足（markdown 档）
// 或装配失败时返回 nil——调用方按"语义路径未装配"处理，**不**回退到关键词。
// newSemanticRecaller assembles the semantic recall when the carrier supports the narrow
// persistence port; otherwise it returns nil so the caller fails closed.
func newSemanticRecaller(carrier core.LongTermMemoryStore, embedder memory.Embedder) tool.MemoryRecaller {
	memoryStore, ok := carrier.(memory.MemoryStore)
	if !ok {
		return nil
	}
	return &semanticRecallAdapter{
		store: memory.NewSemanticStore(memory.SemanticDeps{Store: memoryStore, Embedder: embedder}),
	}
}

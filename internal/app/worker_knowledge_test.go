package app

// 本文件是 US-25C FR-25C-12 的知识面判据：Worker 保留**只读**知识装配，只跳过
// 代际的初始化写入。判据分三层：
//
//  1. 有 active 代际时，只读固定（`GenerationManager.Pin`）返回**同一行**——Worker 上
//     检索、来源视图、`recall_memory` 都按这个代际号取向量，号非零即知识绑定 Agent
//     可用（`TestWorkerKnowledge_BoundAgentRetrievalWorks`）；
//  2. 无 active 代际时失败关闭为 `ErrGenerationNotInitialized` 并点名先由控制面
//     创建并激活，**零建行、零激活、零退休**——不得就地补建把 Worker 变成写者
//     （`TestWorkerKnowledge_MissingGenerationFailsClosed`）；
//  3. 无论 active 代际在与不在，只读固定都不碰三条写路径；生产装配也确实以只读入口
//     接线（`TestWorkerKnowledge_NeverEnsuresGeneration`）。
//
// 端到端"真实检索走通"（真实检索器 + PostgreSQL）不在本层：那由 A+B 真实链路
// （模块卡 §4.5）与集成层承担，本文件不把装配级对号伪装成端到端证据。
// This file pins the worker's read-only knowledge assembly: the pin returns the live
// generation, a missing generation fails closed with zero writes, and the production
// wiring goes through the read-only entry point. Real retrieval is proven end to end
// by the A+B acceptance, not here.

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/knowledge"
)

const (
	// workerKnowledgeModel 是用例里的 embedding 模型 id。
	workerKnowledgeModel = "bge-m3"
)

// workerGenerationStore 是 `knowledge.GenerationStore` 的最小替身：只建模本文件
// 观测到的三件事——active 行、读失败注入、三条写路径的调用记录。写路径只记录不
// 落行：用例判的是"根本没被调用"，落行反而会掩盖误调用。
// workerGenerationStore records the three write paths instead of modelling them, so an
// accidental write shows up as a non-empty log rather than a plausible-looking row.
type workerGenerationStore struct {
	mu        sync.Mutex
	active    knowledge.IndexGeneration
	hasActive bool
	activeErr error

	created   []knowledge.GenerationSpec
	activated []int
	retired   []int
}

// seedActive 预置一条 active 代际。
func (s *workerGenerationStore) seedActive(id int, model string, dimensions int) knowledge.IndexGeneration {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = knowledge.IndexGeneration{
		Generation:     id,
		EmbeddingModel: model,
		Dimensions:     dimensions,
		Distance:       knowledge.DistanceCosine,
		Status:         knowledge.GenerationActive,
		CreatedAt:      time.UnixMilli(1_600_000_000_000).UTC(),
		ActivatedAt:    time.UnixMilli(1_600_000_000_000).UTC(),
	}
	s.hasActive = true
	return s.active
}

// ActiveGeneration 返回预置的 active 行。
func (s *workerGenerationStore) ActiveGeneration(context.Context) (knowledge.IndexGeneration, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.hasActive, s.activeErr
}

// Generation 按代际号读行；本文件不考察该路径，一律按"查无此行"返回。
func (s *workerGenerationStore) Generation(_ context.Context, generation int) (knowledge.IndexGeneration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasActive && s.active.Generation == generation {
		return s.active, nil
	}
	return knowledge.IndexGeneration{}, knowledge.ErrNotFound
}

// CreateGeneration 记录建行调用（`Begin` / `Ensure` 的首代际兜底都经它）。
func (s *workerGenerationStore) CreateGeneration(_ context.Context, spec knowledge.GenerationSpec, _ time.Time) (knowledge.IndexGeneration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, spec)
	return knowledge.IndexGeneration{}, nil
}

// GenerationVectorStats 返回空统计；本文件不考察校验路径。
func (s *workerGenerationStore) GenerationVectorStats(context.Context, int) (knowledge.GenerationVectorStats, error) {
	return knowledge.GenerationVectorStats{}, nil
}

// ActivateGeneration 记录切换调用。
func (s *workerGenerationStore) ActivateGeneration(_ context.Context, generation int, _ time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activated = append(s.activated, generation)
	return 0, nil
}

// RetireGeneration 记录退休调用。
func (s *workerGenerationStore) RetireGeneration(_ context.Context, generation int, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = append(s.retired, generation)
	return nil
}

// writes 汇总三条写路径的调用次数。
func (s *workerGenerationStore) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.created) + len(s.activated) + len(s.retired)
}

// newWorkerKnowledgeManager 按固定的时钟与静默日志装配代际管理。
func newWorkerKnowledgeManager(store *workerGenerationStore) *knowledge.GenerationManager {
	return knowledge.NewGenerationManager(knowledge.GenerationDeps{
		Store:  store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC) },
	})
}

// workerKnowledgeSpec 是配置侧声明的代际标识（模型 + 冻结列宽）。
func workerKnowledgeSpec() knowledge.GenerationSpec {
	return knowledge.GenerationSpec{
		EmbeddingModel: workerKnowledgeModel,
		Dimensions:     knowledge.VectorDimensions,
	}
}

// compactAppSource 把生产源码里的全部空白压掉：本文件对装配面的对号不该因为
// gofumpt 的对齐而误报。
// compactAppSource strips whitespace so alignment changes cannot break the wiring
// assertions.
func compactAppSource(source string) string {
	return strings.Join(strings.Fields(source), "")
}

// TestWorkerKnowledge_BoundAgentRetrievalWorks 守"只读固定仍让知识绑定 Agent 可用"：
// active 代际原样返回，Worker 的检索 / 来源视图 / `recall_memory` 因此拿到非零代际；
// 装配面仍整体返回这些只读字段（漏掉任何一项，Worker 上的知识绑定 Agent 就静默失效）。
// The read-only pin must return the live generation and the wiring must still expose
// the full read-only knowledge surface.
func TestWorkerKnowledge_BoundAgentRetrievalWorks(t *testing.T) {
	t.Parallel()

	store := &workerGenerationStore{}
	store.seedActive(7, workerKnowledgeModel, knowledge.VectorDimensions)
	manager := newWorkerKnowledgeManager(store)

	pinned, err := manager.Pin(context.Background(), workerKnowledgeSpec())
	require.NoError(t, err, "有 active 代际时只读固定必须成功——Worker 不因只读模式失去知识能力")
	require.Equal(t, 7, pinned.Generation, "只读固定必须原样返回 active 代际号")
	require.NotZero(t, pinned.Generation,
		"检索 / 来源视图 / recall_memory 都以这个代际号为唯一目标，0 即整条知识链失效")
	require.Equal(t, workerKnowledgeModel, pinned.EmbeddingModel)
	require.Equal(t, knowledge.VectorDimensions, pinned.Dimensions)
	require.Equal(t, knowledge.GenerationActive, pinned.Status)
	require.Zero(t, store.writes(), "只读固定不得产生任何代际写")

	// 装配面：这些字段就是 Worker 上知识绑定 Agent 依赖的只读面。少了任何一项，
	// 检索 / 来源校验 / embedding 客户端 / recall_memory 至少一条链路断掉。
	// Every field below is a worker-visible read-only surface; dropping one silently
	// breaks a knowledge-bound agent on the worker.
	source := compactAppSource(readAppProductionSources(t))
	for _, want := range []string{
		"Embedder:queryEmbedder",
		"Generation:generation",
		"Search:sticky",
		"Source:viewer",
		"Run:newKnowledgeRunSource(",
		"Assembly:assembly",
		"Validity:&knowledgeValidityAdapter{viewer:viewer}",
	} {
		require.Contains(t, source, want,
			"Worker 保留的只读知识面 %q 不得从装配面移除", want)
	}
}

// TestWorkerKnowledge_MissingGenerationFailsClosed 守"缺 active 代际时失败关闭"：
// 错误必须可判别为 `ErrGenerationNotInitialized`、点名先由控制面初始化，且**零写**。
// A missing active generation fails closed and names the control plane as the owner.
func TestWorkerKnowledge_MissingGenerationFailsClosed(t *testing.T) {
	t.Parallel()

	store := &workerGenerationStore{}
	manager := newWorkerKnowledgeManager(store)

	_, err := manager.Pin(context.Background(), workerKnowledgeSpec())
	require.ErrorIs(t, err, knowledge.ErrGenerationNotInitialized,
		"缺 active 代际必须是具名错误，不能退化成检索期的不明失败")
	require.ErrorContains(t, err, "控制面",
		"错误必须点名先由控制面创建并激活（FR-25C-12）")
	require.Zero(t, store.writes(),
		"缺 active 代际不得用建行 / 激活兜底——那会把 Worker 变成定义写者")
	require.Empty(t, store.created, "零建行")
	require.Empty(t, store.activated, "零激活")
	require.Empty(t, store.retired, "零退休")
}

// TestWorkerKnowledge_NeverEnsuresGeneration 是 MissingGeneration 的对照面：无论
// active 代际在与不在，只读固定都不走三条写路径；生产装配也确实以只读入口接线
// （控制面的 `Ensure` 由启动链的角色门单独负责，不在 Worker 路径上）。
// Whatever the store holds, the read-only pin never writes, and production wiring
// goes through it.
func TestWorkerKnowledge_NeverEnsuresGeneration(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		seed bool
	}{
		{name: "active 代际存在", seed: true},
		{name: "active 代际缺失", seed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &workerGenerationStore{}
			if tc.seed {
				store.seedActive(7, workerKnowledgeModel, knowledge.VectorDimensions)
			}
			manager := newWorkerKnowledgeManager(store)

			// 结论在两种情形下不同（成功 / 具名失败），本用例只判写面：
			// the verdict differs by case; only the write surface is judged here.
			_, _ = manager.Pin(context.Background(), workerKnowledgeSpec())

			require.Empty(t, store.created, "只读固定不得建行（含 Ensure 的首代际兜底）")
			require.Empty(t, store.activated, "只读固定不得激活")
			require.Empty(t, store.retired, "只读固定不得退休")
		})
	}

	// 生产装配必须真的走只读入口；声明了 `Pin` 却没人调用，等于 Worker 仍走 `Ensure`。
	source := compactAppSource(readAppProductionSources(t))
	require.Contains(t, source, ".Pin(ctx,",
		"知识面装配必须以只读固定（GenerationManager.Pin）为入口")

	// 反向对号：只读化不得顺手删掉控制面的首代际初始化——去掉 `Ensure` 就再没有人
	// 建首代际，而 Worker 只会如实报 `ErrGenerationNotInitialized`。
	require.Contains(t, source, "manager.Ensure(ctx,",
		"控制面的首代际初始化（Ensure）必须保留，由共享初始化角色门决定是否执行")
}

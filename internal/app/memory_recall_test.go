package app

// 本文件锁死 `memory_recall.go` 的装配面（US-16 P-07）：两段绑定槽位的**未绑定
// 失败关闭**、窄持久化端口的**可选装配**（markdown 档不装语义召回）、以及端口适配器
// 对"未装配"错误的收敛与对"身份不可用"错误的不吞。
//
// 这一层判的是**装配形状**，不是召回正确性：召回管线（模式分流、身份判定、embedding、
// 载体召回）只有一个实现，在 `internal/memory`，由那边的用例判。这里若也重写一遍
// 过滤逻辑，就会出现第二套召回语义——正是本文件要防的。
//
// This file pins the assembly surface of memory_recall.go: the slot fails closed while
// unbound, the semantic recaller is assembled only for carriers that implement the narrow
// persistence port, and the adapter folds the "not assembled" errors into the tool-side
// sentinel without swallowing identity errors. The recall pipeline itself is judged in
// internal/memory; no second implementation lives here.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/tool"
)

// recallTestPrincipal 是语义召回的合法调用方身份（归属 + 执行者齐全）。
// recallTestPrincipal is a valid caller principal for the recall tests.
func recallTestPrincipal(t *testing.T) core.Principal {
	t.Helper()
	p, err := core.OwnedPrincipal(
		"caller-recall-test", core.AuthorizingKindCallerKey, "key-recall-test",
		[]string{"agents:invoke:alice"}, core.ActorScheduler)
	require.NoError(t, err)
	return p
}

// appMemoryStore 是 `memory.MemoryStore` 的进程内替身：语义召回记录入参并按注入值
// 回结果；生命周期方法是空实现（本文件不验证它们，`internal/memory` 的用例覆盖）。
// appMemoryStore records recall inputs and returns injected results; the lifecycle
// methods are inert because this file does not exercise them.
type appMemoryStore struct {
	recallCalls int
	lastQuery   memory.RecallQuery
	lastVector  []float32
	lastModel   string
	results     []memory.MemoryEntry
	recallErr   error
}

// Append 是空实现（本文件不用兼容写入面）。
func (s *appMemoryStore) Append(context.Context, core.Scope, core.Section, string) error { return nil }

// Load 是空实现（本文件不用兼容读取面）。
func (s *appMemoryStore) Load(context.Context, core.Scope) (string, error) { return "", nil }

// RecallByKeyword 是空实现（关键词路径的断言在 internal/tool 与内部 memory 用例里）。
func (s *appMemoryStore) RecallByKeyword(context.Context, core.Scope, string) ([]string, error) {
	return nil, nil
}

// RecallEntries 记下向量与模型（载荷转换的判据），回注入结果。
func (s *appMemoryStore) RecallEntries(_ context.Context, query memory.RecallQuery,
	vector []float32, model string,
) ([]memory.MemoryEntry, error) {
	s.recallCalls++
	s.lastQuery, s.lastVector, s.lastModel = query, vector, model
	return s.results, s.recallErr
}

// LoadEntry 是空实现。
func (s *appMemoryStore) LoadEntry(context.Context, int64) (memory.MemoryEntry, bool, error) {
	return memory.MemoryEntry{}, false, nil
}

// FindEntryByContent 是空实现。
func (s *appMemoryStore) FindEntryByContent(context.Context, core.Scope, core.Section, string,
) (memory.MemoryEntry, bool, error) {
	return memory.MemoryEntry{}, false, nil
}

// InsertEntry 是空实现。
func (s *appMemoryStore) InsertEntry(context.Context, memory.MemoryEntry) (int64, error) {
	return 0, nil
}

// UpdateEntryState 是空实现。
func (s *appMemoryStore) UpdateEntryState(context.Context, int64, string, string) error {
	return nil
}

// ListEntries 是空实现（读模型的用例在 internal/memory 与 web 层）。
func (s *appMemoryStore) ListEntries(context.Context, memory.ListQuery) ([]memory.MemoryEntry, error) {
	return nil, nil
}

// ReplaceEntry 是空实现。
func (s *appMemoryStore) ReplaceEntry(context.Context, int64, string, memory.MemoryEntry) (int64, error) {
	return 0, nil
}

// 编译期断言：替身满足窄持久化端口。
var _ memory.MemoryStore = (*appMemoryStore)(nil)

// keywordOnlyCarrier 只实现 `core.LongTermMemoryStore`（markdown 档的形状）：**不是**
// `memory.MemoryStore`，语义召回不得为它装配。
// keywordOnlyCarrier mirrors the markdown backend, which does not implement the port.
type keywordOnlyCarrier struct{}

// Append 是空实现。
func (keywordOnlyCarrier) Append(context.Context, core.Scope, core.Section, string) error {
	return nil
}

// Load 是空实现。
func (keywordOnlyCarrier) Load(context.Context, core.Scope) (string, error) { return "", nil }

// RecallByKeyword 是空实现。
func (keywordOnlyCarrier) RecallByKeyword(context.Context, core.Scope, string) ([]string, error) {
	return nil, nil
}

// recallTestEmbedder 是 `memory.Embedder` 的替身：固定回一个二维向量并计数。
// recallTestEmbedder returns one fixed vector and counts its calls.
type recallTestEmbedder struct {
	calls int
	err   error
}

// Embed 回单个向量（与单条输入匹配）。
func (e *recallTestEmbedder) Embed(_ context.Context, texts []string) (memory.EmbedBatch, error) {
	e.calls++
	if e.err != nil {
		return memory.EmbedBatch{}, e.err
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{0.1, 0.2}
	}
	return memory.EmbedBatch{Vectors: vectors, Dimensions: 2, Model: "embed-test"}, nil
}

// TestMemoryRecallSlot_UnboundFailsClosed 判两段绑定的安全默认：槽位未绑定时，
// 每次调用都回 `tool.ErrRecallUnavailable`（工具据此给出"改用 mode=keyword"的
// 可纠正提示），**不** panic、**不**回退关键词、**不**返回空结果冒充"没有命中"。
// 回空切片会让"装配缺失"看起来像"记忆里没有内容"——这正是失败关闭要防的。
// 逆改点：未绑定时返回 `nil, nil` → NoError / Len 断言红；直接解引用 inner →
// panic 红；在此回退关键词检索 → 无载体可回退，只有哨兵断言能守住。
func TestMemoryRecallSlot_UnboundFailsClosed(t *testing.T) {
	slot := newMemoryRecallSlot()
	hits, err := slot.RecallArchive(context.Background(), tool.MemoryRecallRequest{
		Principal: recallTestPrincipal(t), Query: "成本", Mode: "semantic",
	})
	require.Error(t, err, "未绑定必须失败关闭")
	require.ErrorIs(t, err, tool.ErrRecallUnavailable, "错误必须可判（工具层按哨兵翻译文案）")
	require.Nil(t, hits, "不得回空切片冒充零命中")
}

// TestMemoryRecallSlot_ForwardsAfterBind 判绑定后槽位是**透明转发**：入参原样到达
// 已绑定实现，结果原样返回——槽位不加过滤、不改写、不重排。
// 逆改点：槽位改写身份或查询、或对结果做二次过滤 → 记录的入参 / 回传值断言红。
func TestMemoryRecallSlot_ForwardsAfterBind(t *testing.T) {
	inner := &slotRecordingRecaller{hits: []tool.MemoryRecallHit{{Rank: 1, Source: "s", Content: "c"}}}
	slot := newMemoryRecallSlot()
	slot.Bind(inner)

	req := tool.MemoryRecallRequest{
		Principal: recallTestPrincipal(t), Scope: core.Scope{Agent: "alice"},
		Query: "成本", Mode: "semantic", Limit: 3,
	}
	hits, err := slot.RecallArchive(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, []tool.MemoryRecallRequest{req}, inner.calls, "入参必须逐字段原样转发")
	require.Equal(t, inner.hits, hits, "结果必须原样返回")
}

// slotRecordingRecaller 记录每次请求并回固定命中。
// slotRecordingRecaller records requests and returns fixed hits.
type slotRecordingRecaller struct {
	calls []tool.MemoryRecallRequest
	hits  []tool.MemoryRecallHit
}

// RecallArchive 记录并入参并回固定结果。
func (r *slotRecordingRecaller) RecallArchive(_ context.Context, req tool.MemoryRecallRequest) ([]tool.MemoryRecallHit, error) {
	r.calls = append(r.calls, req)
	return r.hits, nil
}

// TestNewSemanticRecaller_RequiresNarrowPort 判**可选装配**的判据：载体满足窄持久化
// 端口才装语义召回；markdown 档（只实现兼容三方法）与 nil 载体都回 nil，调用方按
// "语义路径未装配"失败关闭。若这里硬造一个召回器，markdown 部署下语义模式会在
// 运行期才炸，而不是在工具层给出可纠正提示。
// 逆改点：去掉类型断言直接构造适配器 → markdown 档拿到非 nil 召回器，断言红。
func TestNewSemanticRecaller_RequiresNarrowPort(t *testing.T) {
	require.Nil(t, newSemanticRecaller(keywordOnlyCarrier{}, nil),
		"markdown 档不实现窄持久化端口：不得装配语义召回")
	require.Nil(t, newSemanticRecaller(nil, nil), "nil 载体不得装配")
	require.NotNil(t, newSemanticRecaller(&appMemoryStore{}, &recallTestEmbedder{}),
		"实现窄持久化端口的载体必须装配（embedding 可后配，未配置时按未装配失败关闭）")
}

// TestSemanticRecaller_NotAssembledFoldsIntoSentinel 判"缺 embedding 连接"这一档：
// `memory.SemanticStore` 回内部哨兵，适配器收敛成 `tool.ErrRecallUnavailable`，且
// **零召回调用**——装配缺失时连载体都不碰。
// 逆改点：适配器不折叠（原样上抛内部错误）→ `ErrorIs(tool.ErrRecallUnavailable)` 红；
// 折叠成 `core.ErrForbidden` 之类 → 同样红（工具层不会翻译成可纠正提示）。
func TestSemanticRecaller_NotAssembledFoldsIntoSentinel(t *testing.T) {
	store := &appMemoryStore{}
	recaller := newSemanticRecaller(store, nil) // embedding 未配置
	require.NotNil(t, recaller)

	_, err := recaller.RecallArchive(context.Background(), tool.MemoryRecallRequest{
		Principal: recallTestPrincipal(t), Scope: core.Scope{Agent: "alice"},
		Query: "成本", Mode: "semantic",
	})
	require.Error(t, err)
	require.ErrorIs(t, err, tool.ErrRecallUnavailable, "未装配必须可判")
	require.Zero(t, store.recallCalls, "装配缺失时不得触碰载体")
}

// TestSemanticRecaller_ZeroPrincipalIsNotFolded 判身份错误**不被误折叠**：零值身份由
// `SemanticStore.Recall` 在触碰载体与 embedding 之前拒绝（`core.ErrUnauthorized`），
// 适配器**原样上抛**——若把它折成 `ErrRecallUnavailable`，工具层会把越权翻成
// "请改用 keyword 重试"，等于提示可以绕开身份闸门。
// 逆改点：适配器把 `core.ErrUnauthorized` 也当"未装配"折叠 → `ErrorIs(ErrUnauthorized)` 红。
func TestSemanticRecaller_ZeroPrincipalIsNotFolded(t *testing.T) {
	store := &appMemoryStore{}
	embedder := &recallTestEmbedder{}
	recaller := newSemanticRecaller(store, embedder)

	_, err := recaller.RecallArchive(context.Background(), tool.MemoryRecallRequest{
		Query: "成本", Mode: "semantic", // Principal 零值
	})
	require.ErrorIs(t, err, core.ErrUnauthorized, "越权不是装配问题")
	require.NotErrorIs(t, err, tool.ErrRecallUnavailable, "不得折叠成可重试的装配提示")
	require.Zero(t, store.recallCalls, "拒绝在触碰载体之前")
	require.Zero(t, embedder.calls, "拒绝在花一次 embedding 之前")
}

// TestSemanticRecaller_HitsCarryRankAndSource 判语义档的形状转换：名次按载体返回顺序
// 从 1 编号（不重排、不截断），来源与正文逐字透传；同时判**载荷**——检索词、向量
// 与模型逐字段到达载体，身份逐字段到达（归属与执行者都不得在装配层丢失）。
// 逆改点：名次从 0 编号或不编号 → 断言红；换用 `entry.Content` 外的字段 → 正文断言红；
// 装配层重建身份（如换成零值 / 管理员）→ 身份断言红。
func TestSemanticRecaller_HitsCarryRankAndSource(t *testing.T) {
	principal := recallTestPrincipal(t)
	store := &appMemoryStore{results: []memory.MemoryEntry{
		memory.WithSource(memory.MemoryEntry{
			Content: "第一条：成本评估", Section: core.SectionArchive, State: "active",
		}, memory.SourceSaveMemory),
		{Content: "第二条：旧导入条目", Section: core.SectionArchive, State: "active"},
	}}
	recaller := newSemanticRecaller(store, &recallTestEmbedder{})

	hits, err := recaller.RecallArchive(context.Background(), tool.MemoryRecallRequest{
		Principal: principal, Scope: core.Scope{Agent: "alice"}, Query: "成本", Mode: "semantic",
	})
	require.NoError(t, err)
	require.Equal(t, []tool.MemoryRecallHit{
		{Rank: 1, Source: memory.SourceSaveMemory, Content: "第一条：成本评估"},
		{Rank: 2, Content: "第二条：旧导入条目"}, // 来源为空：留给工具层决定不渲染
	}, hits, "名次 1 起、按返回顺序；来源与正文逐字透传")

	require.Equal(t, 1, store.recallCalls)
	require.Equal(t, principal, store.lastQuery.Principal, "身份必须逐字段到达载体")
	require.Equal(t, core.Scope{Agent: "alice"}, store.lastQuery.Scope)
	require.Equal(t, "成本", store.lastQuery.Query)
	require.Equal(t, "semantic", store.lastQuery.Mode, "适配器不得把缺省语义改成 keyword")
	require.Equal(t, []float32{0.1, 0.2}, store.lastVector, "向量必须来自 embedding 回执")
	require.Equal(t, "embed-test", store.lastModel, "模型标识必须随查询交给载体（T052）")
}

// knowledgeEmbedderStub 是 `knowledge.Embedder` 的替身（装配层接的是知识库栈那一个
// embedding 客户端）；四个回执字段都填，供适配器逐字段对照。
// knowledgeEmbedderStub fills every batch field so the adapter's copy can be checked.
type knowledgeEmbedderStub struct {
	calls int
}

// Embed 回两个向量并带上 Token 数、维度与模型。
func (s *knowledgeEmbedderStub) Embed(_ context.Context, texts []string) (knowledge.EmbedBatch, error) {
	s.calls++
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{0.1, 0.2}
	}
	return knowledge.EmbedBatch{
		Vectors: vectors, PromptTokens: 7, Dimensions: 2, Model: "embed-test",
	}, nil
}

// TestMemoryEmbedderOf_KeepsNilNil 判 nil 保持 nil：`memory.Recall` 用它区分"未配置"，
// 一个包着 nil 指针的非 nil 接口会把未配置变成运行期 panic 或一次空 embedding。
// 逆改点：无条件返回适配器 → nil 判定红。
func TestMemoryEmbedderOf_KeepsNilNil(t *testing.T) {
	require.Nil(t, memoryEmbedderOf(nil), "未配置必须保持 nil，不得包住 nil 指针")
	require.NotNil(t, memoryEmbedderOf(&knowledgeEmbedderStub{}))
}

// TestMemoryEmbedderAdapter_ForwardsBatch 判适配器是**逐字段透明**转换：向量、Token
// 数、维度、模型一个都不丢（丢模型会让载体选错代际）。
// 逆改点：漏拷任一字段 → 对应断言红。
func TestMemoryEmbedderAdapter_ForwardsBatch(t *testing.T) {
	adapter := memoryEmbedderAdapter{inner: &knowledgeEmbedderStub{}}
	batch, err := adapter.Embed(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	require.Len(t, batch.Vectors, 2, "向量与输入一一对应")
	// Token 数只落到 `embedding_calls` 的用量列；查询侧 embedding token **不**计入
	// 调用方配额与用量报表（技术方案 §3.5.2 的共享召回计数口径），故这里判的是
	// "不丢字段"，不是"已结算"。
	require.Equal(t, int64(7), batch.PromptTokens, "Token 数必须逐字段透传，不得在适配层丢弃或置换")
	require.Equal(t, 2, batch.Dimensions)
	require.Equal(t, "embed-test", batch.Model)
}

// TestSemanticRecaller_IndexGapFoldsIntoItsOwnSentinel 判 V-5 的缺口在装配层的收敛：
// `memory.ErrIndexNotReady` 收敛成 `tool.ErrRecallIndexNotReady`，与「未装配」的哨兵
// **分开**——两者在工具层给出不同修复动作（等回填 vs 补装配），混成一个会让缺索引的
// 实例被提示去改配置。同时判结论来自真的一次召回（不是装配层短路）。
// 逆改点：删掉缺口分支 → 原样上抛内部哨兵，`ErrorIs(tool.ErrRecallIndexNotReady)` 红；
// 折叠进 `ErrRecallUnavailable` → 第二条 `NotErrorIs` 红。
func TestSemanticRecaller_IndexGapFoldsIntoItsOwnSentinel(t *testing.T) {
	store := &appMemoryStore{
		recallErr: fmt.Errorf("%w: 代际 1 在 agent 层有 2 条生效归档记忆尚无向量",
			memory.ErrIndexNotReady),
	}
	recaller := newSemanticRecaller(store, &recallTestEmbedder{})

	_, err := recaller.RecallArchive(context.Background(), tool.MemoryRecallRequest{
		Principal: recallTestPrincipal(t), Scope: core.Scope{Agent: "alice"},
		Query: "成本", Mode: "semantic",
	})
	require.ErrorIs(t, err, tool.ErrRecallIndexNotReady, "缺口必须可判（工具层按哨兵翻译文案）")
	require.NotErrorIs(t, err, tool.ErrRecallUnavailable, "缺口不是装配缺失：修复动作不同")
	require.Equal(t, 1, store.recallCalls, "缺口来自真的一次召回，不是装配层短路")
}

// TestSemanticRecaller_StoreErrorPassesThrough 判载体侧的其他错误**原样上抛**：适配器
// 只收敛"未装配"这一种，别的错误（这里是 `core.ErrConflict`）不得被改写——工具层
// 据此归类（runtime_error），报错文案才不会把存储故障说成入参问题。
// 逆改点：所有错误一律折叠成 `ErrRecallUnavailable` → 身份断言红。
func TestSemanticRecaller_StoreErrorPassesThrough(t *testing.T) {
	store := &appMemoryStore{recallErr: errors.Join(core.ErrConflict, errors.New("pg 序列化失败"))}
	recaller := newSemanticRecaller(store, &recallTestEmbedder{})

	_, err := recaller.RecallArchive(context.Background(), tool.MemoryRecallRequest{
		Principal: recallTestPrincipal(t), Scope: core.Scope{Agent: "alice"},
		Query: "成本", Mode: "semantic",
	})
	require.ErrorIs(t, err, core.ErrConflict, "载体错误必须原样上抛")
	require.NotErrorIs(t, err, tool.ErrRecallUnavailable, "不得冒充实装缺失")
}

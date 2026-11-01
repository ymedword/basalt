package app

// 本文件是 review ② D-4 的**计数、配额与审计**判据：共享记忆让一次 `recall_memory`
// 触发**两次** embedding（每层一次），两次都必须各自走 `embedding_calls` 的两阶段
// 预写；权限撤销与预写失败都要在**花掉 embedding 之前**拦住。
//
// 为什么这一层要单独判：工具层只看得见"调了几次召回端口"，看不见每次召回背后的
// 模型调用与钱。共享模式把模型调用翻倍，若只在工具层判，翻倍这件事在测试里不可见——
// 而那正是运维要给用户解释的费用来源（D-4 要求页面/文档说明这一点）。
//
// This file pins the cost and audit shape of shared recall: two layers mean two audited
// embeddings, and both revocation and a failed pre-write must stop before spending any.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/tool"
)

// sharedEmbedderStub 是能正常返回一个向量的内层客户端：每层一次 embedding 才有可数的
// 调用，而 `SemanticStore.Recall` 要求回执恰好一个向量（零向量回执会被它当作协议错误）。
// sharedEmbedderStub returns one vector so that per-layer embeddings are countable.
func sharedEmbedderStub() *auditedEmbedderInnerStub {
	return &auditedEmbedderInnerStub{batch: knowledge.EmbedBatch{
		Vectors: [][]float32{{0.1, 0.2}}, PromptTokens: 3, Dimensions: 2, Model: "embed-test",
	}}
}

// sharedRecallStore 在 `appMemoryStore` 之上补齐调用方 Key 的**实时复核**端口
// （`memory.MemoryAccessChecker`）与按作用域分档的召回行为：
//
//   - `authorized` 记下每层各查了一次 Key（撤销状态因此能拦在 embedding 之前）；
//   - `recallScopes` 记下每层各检索了一次（层名为空 = 全局层）；
//   - `globalErr` 只让全局层失败，用来判"任一层失败即整次失败"。
//
// sharedRecallStore adds the live-key check and per-layer recall behaviour on top of
// appMemoryStore, so "which layers were authorized, read and billed" is all assertable.
type sharedRecallStore struct {
	appMemoryStore
	authorized   []string
	authorizeErr error
	recallScopes []string
	globalErr    error
}

// AuthorizeMemory 记录被复核的 Agent 并按预置错误返回。
func (s *sharedRecallStore) AuthorizeMemory(_ context.Context, _ core.Principal, agent string) error {
	s.authorized = append(s.authorized, agent)
	return s.authorizeErr
}

// RecallEntries 记录被检索的层；全局层可按预置错误失败。
func (s *sharedRecallStore) RecallEntries(_ context.Context, query memory.RecallQuery,
	_ []float32, _ string,
) ([]memory.MemoryEntry, error) {
	s.recallScopes = append(s.recallScopes, query.Scope.Agent)
	if query.Scope.Agent == "" && s.globalErr != nil {
		return nil, s.globalErr
	}
	return s.results, nil
}

// RecallByKeyword 记录关键词路被检索的层（共享模式下关键词路也逐层读）。
func (s *sharedRecallStore) RecallByKeyword(_ context.Context, scope core.Scope, _ string) ([]string, error) {
	s.recallScopes = append(s.recallScopes, scope.Agent)
	return nil, nil
}

var _ memory.MemoryStore = (*sharedRecallStore)(nil)

// sharedRecallEnv 是共享模式的工具执行环境：调用方身份 + 已开启 include_global_memory。
// sharedRecallEnv is the tool env with shared memory switched on.
func sharedRecallEnv(t *testing.T) core.ToolEnv {
	t.Helper()
	return core.ToolEnv{
		RunID:             "run-shared-1",
		SessionID:         "sess-shared-1",
		AgentName:         "alice",
		MaxOutputBytes:    256 * 1024,
		Principal:         recallTestPrincipal(t),
		ShareGlobalMemory: true,
	}
}

// sharedRecallTool 组装生产链路：审计装饰器 → memory 适配器 → SemanticStore →
// 端口适配器 → recall_memory。用真链路而非假召回器，才能把"两次 embedding"变成
// 可断言的事实。
// sharedRecallTool wires the production chain so that "two embeddings" is observable.
func sharedRecallTool(t *testing.T, store *sharedRecallStore,
	inner *auditedEmbedderInnerStub, audit *auditedEmbedderAuditStub,
) *tool.RecallMemoryTool {
	t.Helper()
	embedder := memoryEmbedderOf(newAuditedEmbedderForTest(t, inner, audit, &bytes.Buffer{}))
	recaller := newSemanticRecaller(store, embedder)
	require.NotNil(t, recaller, "共享模式的判据要跑真链路：载体必须满足窄持久化端口")
	return tool.NewRecallMemoryTool(store, recaller)
}

// TestSharedRecall_SemanticIssuesTwoAuditedEmbeddings 判共享模式的成本面：两层各一次
// embedding，各自预写 `pending` 再发调用、各自回填终态，每次只嵌查询词一条；两次都
// 先复核调用方 Key 的**当前**状态。
// 逆改点：把两层合并成一次检索（先查一次、两层共用向量）→ `inner.calls` 变 1 红；
// 只对 Agent 层审计 → `Len(begins)` 变 1 红；跳过 Key 复核 → `authorized` 红。
// 验收点：D-4；原则 V。
func TestSharedRecall_SemanticIssuesTwoAuditedEmbeddings(t *testing.T) {
	store := &sharedRecallStore{appMemoryStore: appMemoryStore{
		results: []memory.MemoryEntry{{Content: "同一条事实", Section: core.SectionArchive, State: "active"}},
	}}
	inner := sharedEmbedderStub()
	audit := &auditedEmbedderAuditStub{callID: "call-shared"}

	_, err := sharedRecallTool(t, store, inner, audit).Execute(context.Background(), sharedRecallEnv(t),
		json.RawMessage(`{"query":"事实","mode":"semantic"}`))
	require.NoError(t, err)

	require.Equal(t, 2, inner.calls, "两层各一次 embedding：共享模式不是一次检索")
	require.Len(t, audit.begins, 2, "每一次 embedding 都必须先预写 pending")
	require.Len(t, audit.finishes, 2, "每一次 embedding 都必须回填终态")
	for _, begin := range audit.begins {
		require.Equal(t, 1, begin.inputCount, "每次只嵌查询词一条")
		require.Equal(t, "embed-test", begin.model, "两次用同一个模型（同一份装配）")
	}
	require.Equal(t, []string{"alice", "alice"}, store.authorized, "两层各复核一次 Key 状态")
	require.Equal(t, []string{"alice", ""}, store.recallScopes, "先 Agent 层、后全局层")
}

// TestSharedRecall_KeywordModeReadsBothLayersWithoutEmbedding 判模式分流在共享模式里
// 仍然成立：`mode=keyword` 两层都读，但**一次 embedding 都不发**，因此也没有任何
// `embedding_calls` 行。共享模式不得把关键词请求偷偷升级成语义请求。
// 逆改点：共享模式忽略 mode 一律走语义 → `inner.calls` 变 2 红。
// 验收点：D-4。
func TestSharedRecall_KeywordModeReadsBothLayersWithoutEmbedding(t *testing.T) {
	store := &sharedRecallStore{}
	inner := sharedEmbedderStub()
	audit := &auditedEmbedderAuditStub{callID: "call-shared"}

	_, err := sharedRecallTool(t, store, inner, audit).Execute(context.Background(), sharedRecallEnv(t),
		json.RawMessage(`{"query":"事实","mode":"keyword"}`))
	require.NoError(t, err)

	require.Zero(t, inner.calls, "关键词模式不发 embedding")
	require.Empty(t, audit.begins, "没有调用就没有审计行")
	require.Equal(t, []string{"alice", ""}, store.recallScopes, "两层都读")
}

// TestSharedRecall_RevokedKeyStopsBeforeAnyEmbedding 判撤销的失败关闭位置：Key 在第一次
// 复核时就已撤销 → 第一层被拒、第二层不再发起、**零次 embedding、零条审计**。
// 逆改点：把 Key 复核挪到 embedding 之后 → `inner.calls` 非零红；吞掉错误继续读全局层
// → `authorized` 变两条红。
// 验收点：D-4；C-11。
func TestSharedRecall_RevokedKeyStopsBeforeAnyEmbedding(t *testing.T) {
	store := &sharedRecallStore{authorizeErr: core.ErrForbidden}
	inner := sharedEmbedderStub()
	audit := &auditedEmbedderAuditStub{callID: "call-shared"}

	_, err := sharedRecallTool(t, store, inner, audit).Execute(context.Background(), sharedRecallEnv(t),
		json.RawMessage(`{"query":"事实","mode":"semantic"}`))
	require.ErrorIs(t, err, core.ErrForbidden, "撤销的 Key 不得被翻译成可重试的提示")

	require.Equal(t, []string{"alice"}, store.authorized, "拒在第一层，第二层不再复核")
	require.Zero(t, inner.calls, "撤销判定必须发生在花一次 embedding 之前")
	require.Empty(t, audit.begins, "没有调用就没有审计行")
	require.Empty(t, store.recallScopes, "没有调用就不碰载体")
}

// TestSharedRecall_AuditPrewriteFailureStopsTheCall 判原则 V 在共享模式里的同一句话：
// 审计预写失败 → 不发出调用，且不碰载体；第一次就失败，所以连第二层都不会开始。
// 逆改点：把预写错误吞掉继续调用 → `inner.calls` 非零红。
// 验收点：D-4；原则 V。
func TestSharedRecall_AuditPrewriteFailureStopsTheCall(t *testing.T) {
	store := &sharedRecallStore{}
	inner := sharedEmbedderStub()
	audit := &auditedEmbedderAuditStub{callID: "call-shared", beginErr: errors.New("injected: 审计库不可用")}

	_, err := sharedRecallTool(t, store, inner, audit).Execute(context.Background(), sharedRecallEnv(t),
		json.RawMessage(`{"query":"事实","mode":"semantic"}`))
	require.Error(t, err)

	require.Len(t, audit.begins, 1, "失败发生在第一次预写之后，不会开始第二层")
	require.Zero(t, inner.calls, "预写失败不得发出调用")
	require.Empty(t, store.recallScopes, "预写失败不得触碰载体")
}

// TestSharedRecall_GlobalLayerFailureDiscardsAgentHits 判"任一层失败即整次失败"在真链路
// 上的表现：Agent 层已拿到命中，全局层检索失败 → 整次返回错误，**不**把 Agent 层那份
// 当结果交出去（半份召回会被读成"全局层没有内容"）。
// 逆改点：把全局层错误吞成空层 → NoError 且返回 Agent 块 → 本用例红。
// 验收点：D-4。
func TestSharedRecall_GlobalLayerFailureDiscardsAgentHits(t *testing.T) {
	boom := errors.New("injected: 全局层检索失败")
	store := &sharedRecallStore{
		appMemoryStore: appMemoryStore{
			results: []memory.MemoryEntry{{Content: "Agent 私有", Section: core.SectionArchive, State: "active"}},
		},
		globalErr: boom,
	}
	inner := sharedEmbedderStub()
	audit := &auditedEmbedderAuditStub{callID: "call-shared"}

	result, err := sharedRecallTool(t, store, inner, audit).Execute(context.Background(), sharedRecallEnv(t),
		json.RawMessage(`{"query":"事实","mode":"semantic"}`))
	require.ErrorIs(t, err, boom)
	require.Empty(t, result.Content, "失败时不得返回 Agent 层的部分结果")
	require.Equal(t, []string{"alice", ""}, store.recallScopes, "两层的检索都已发生（判的是结果处置，不是跳过）")
	require.Equal(t, 2, inner.calls, "第二层的 embedding 已经花掉：费用口径按实际调用计")
}

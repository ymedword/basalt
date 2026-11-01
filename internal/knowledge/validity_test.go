package knowledge

// 本文件覆盖 T041 的有效性判定（US-16 §3.1.3 / C-09 / C-10）：
//
//   - `Validity.Check` 的状态判定表、身份闸门与存储违约的 fail-closed；
//   - 设计 §4 点名的两条回归 `TestRetrieval_DeletedVersionNotRecalled`（C-10）与
//     `TestRetrieval_SingleGenerationPerQuery`（C-09）。设计只给了测试名与逆改点、
//     没有给出代码，且两条用例在 §4 stage 表里归真库 check `us16-pg`——本仓按
//     T038 / T040 先例用同语义的单元级假端口承载：假端口**照抄**生产端口的契约
//     （按范围集合过滤 / 按代际取向量），因此能把"检索器不传范围"和"检索器不固定
//     代际"这两个逆改点判红；真 SQL（`versions.status` / `deleted_at` / 代际条件）
//     仍是未验证面，记入 run-state 未验证清单。
//
// This file covers validity: the status table, the identity gate, fail-closed store
// violations, and the design's two named C-09 / C-10 regressions. The design ships no
// code for them and registers both under the real-DB check; following the T038 / T040
// precedent they are carried by unit-level fake ports that copy the production port
// contract, so a retriever that drops the scope or the generation fails here. The SQL
// half remains unverified and is recorded in run-state.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// record 返回当前登记的版本行副本；测试在副本上改状态再写回。
// record copies one registered version row so a test can mutate and store it back.
func (h *sourceViewHarness) record(documentID, versionID string) SourceVersionRecord {
	h.t.Helper()
	return h.reader.records[sourceRecordKey(documentID, versionID)]
}

// put 写回一个版本行（测试内部改状态用）。
// put stores a version row back after mutation.
func (h *sourceViewHarness) put(documentID, versionID string, record SourceVersionRecord) {
	h.reader.records[sourceRecordKey(documentID, versionID)] = record
}

// check 以给定身份发起一次有效性判定。
// check issues one validity check as the given principal.
func (h *sourceViewHarness) check(p core.Principal, ref EvidenceRef) (ValidityResult, error) {
	h.t.Helper()
	return h.viewer.Check(context.Background(), p, ref)
}

// 判定表逐行对号：active 有效；软删 / 撤权 / 被取代 / 未生效 / 不存在各自给出
// 冻结契约的原因码，且**不**回正文或内容哈希当作"还能读"。
// 逆改点：judgeValidity 把 deleted / retired / superseded 判成有效 → 对应断言红。
func TestValidity_DeletedVersionIsNotValid(t *testing.T) {
	h := newSourceViewHarness(t)
	active := h.seedActive("ops", "runbook.md", "v1", "回滚步骤：先停写入。")

	got, err := h.check(principalFor("team-a"), active)
	require.NoError(t, err)
	require.True(t, got.Valid, "active 版本必须判为有效")
	require.Empty(t, got.Reason)
	require.Equal(t, "v1", got.VersionID, "判定描述的是引用点名的那个版本")
	require.Equal(t, active.ContentHash, got.ContentHash)

	// 文档软删：C-10 "删文件后旧片段仍被召回" 的反面——引用立刻失效。
	deleted := h.record("runbook.md", "v1")
	deleted.DocumentDeleted = true
	deleted.DeletedAt = fixedClock()()
	h.put("runbook.md", "v1", deleted)
	got, err = h.check(principalFor("team-a"), active)
	require.NoError(t, err)
	require.False(t, got.Valid)
	require.Equal(t, ReasonDeleted, got.Reason)
	require.Equal(t, deleted.DeletedAt, got.DeletedAt)
	require.Empty(t, got.ContentHash, "失效的引用不得回内容哈希冒充可读")

	// 因撤权退役（行保留供引用失效判定）。
	retired := deleted
	retired.DocumentDeleted = false
	retired.DeletedAt = time.Time{}
	retired.Status = VersionRetired
	h.put("runbook.md", "v1", retired)
	got, err = h.check(principalFor("team-a"), active)
	require.NoError(t, err)
	require.False(t, got.Valid)
	require.Equal(t, ReasonRevoked, got.Reason)

	// 被新版本取代：不是当前版本，但另两条来源视图语义不受影响。
	oldRef := h.seedActive("ops", "plan.md", "v1", "旧版计划")
	h.ingestAndActivate("ops", "plan.md", "v2", "新版计划")
	got, err = h.check(principalFor("team-a"), oldRef)
	require.NoError(t, err)
	require.False(t, got.Valid)
	require.Equal(t, ReasonVersionChanged, got.Reason)
	require.Equal(t, "v1", got.VersionID)

	// 从未生效的版本（pending / indexed / failed）不得被当成有效引用。
	pending := h.seedActive("ops", "draft.md", "v1", "草稿")
	row := h.record("draft.md", "v1")
	row.Status = VersionPending
	h.put("draft.md", "v1", row)
	got, err = h.check(principalFor("team-a"), pending)
	require.NoError(t, err)
	require.False(t, got.Valid)
	require.Equal(t, ReasonVersionChanged, got.Reason)

	// 不存在是判定结论（not_found），不是服务错误。
	got, err = h.check(principalFor("team-a"), EvidenceRef{Kind: "knowledge_document", ID: "ghost.md", VersionID: "v9"})
	require.NoError(t, err)
	require.False(t, got.Valid)
	require.Equal(t, ReasonNotFound, got.Reason)
}

// 身份闸门与撤权判定：零值身份不读版本行；缺读 scope 拒绝；ACL 已撤权是
// `revoked` 判定而不是错误——调用方要能把整条消息替换为"内容不可用"。
// 逆改点：Check 不校验 Principal → 零值读到有效结论、读取计数非零，红。
func TestValidity_RequiresPrincipalAndReadScope(t *testing.T) {
	h := newSourceViewHarness(t)
	ref := h.seedActive("secret", "plan.md", "v1", "机密内容")

	_, err := h.check(core.Principal{}, ref)
	require.ErrorIs(t, err, core.ErrUnauthorized, "零值身份不得判定任何引用")
	require.Zero(t, h.reader.calls, "零值身份不得读取版本行")

	weak := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller_a",
		Project:  "team-a",
		Scopes:   []string{core.ScopeSessionsRead},
		Actor:    core.ActorCaller,
	}
	_, err = h.check(weak, ref)
	require.ErrorIs(t, err, ErrForbidden, "没有 knowledge:read 的身份不得判定引用")
	require.Zero(t, h.reader.calls)

	h.setCollectionACL("secret", "team-c") // 调用方 team-b 现在无权
	got, err := h.check(principalFor("team-b"), ref)
	require.NoError(t, err, "撤权是判定结论，不是读取错误")
	require.False(t, got.Valid)
	require.Equal(t, ReasonRevoked, got.Reason)
	require.Equal(t, "v1", got.VersionID)
	// C-08：被拒集合的标识与来源路径是回读后才知道的信息，判定路径不得写进日志。
	require.NotContains(t, h.logs.String(), "secret", "被拒集合的标识不得进日志")
	require.NotContains(t, h.logs.String(), "ops/plan.md", "被拒来源路径不得进日志")
}

// 存储违约 fail-closed：端口读回其它版本时必须报错，**不得**把新正文当成旧引用
// 继续判定；未知状态同样报错，不得默认"有效"或"不存在"。
// 逆改点：去掉 load 的回读校验 → 本用例 NoError，红。
func TestValidity_FailsClosedOnStoreViolation(t *testing.T) {
	h := newSourceViewHarness(t)
	ref := EvidenceRef{Kind: "knowledge_document", ID: "a.md", VersionID: "v1"}
	h.reader.records[sourceRecordKey("a.md", "v1")] = SourceVersionRecord{
		DocumentID: "a.md",
		VersionID:  "v2",
		Status:     VersionActive,
		Text:       "新版本正文",
	}
	_, err := h.check(principalFor("team-a"), ref)
	require.Error(t, err, "读回其它版本必须报错，不得把新正文冒充旧引用")

	unknown := h.seedActive("ops", "b.md", "v1", "正文")
	row := h.record("b.md", "v1")
	row.Status = VersionStatus("archived")
	h.put("b.md", "v1", row)
	_, err = h.check(principalFor("team-a"), unknown)
	require.Error(t, err, "状态不在闭集内是存储违约，必须 fail-closed")
}

// generationVectorPath 是按代际取向量的假端口：契约要求"只查调用方固定的
// `generation`"（C-09）。`backends[0]` 就是"查询不带代际条件"时会命中的旧代际
// 向量——正是设计给出的逆改点口径。
// generationVectorPath serves vectors per generation; backends[0] models what an
// unpinned query would recall, which is the design's stated reverse mutation.
type generationVectorPath struct {
	backends  map[int][]Hit
	calls     []int
	lastScope Scope
}

func (f *generationVectorPath) Search(_ context.Context, scope Scope, _ []float32, generation, k int) ([]Hit, int, error) {
	f.calls = append(f.calls, generation)
	f.lastScope = scope
	backend, ok := f.backends[generation]
	if !ok {
		backend = f.backends[0]
	}
	allowed := make(map[string]bool, len(scope.Collections))
	for _, id := range scope.Collections {
		allowed[id] = true
	}
	out := make([]Hit, 0, len(backend))
	for _, hit := range backend {
		if allowed[hit.CollectionID] {
			out = append(out, hit)
		}
	}
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out, 0, nil
}

// C-09 —— 一次查询固定单一代际：代际原样下传给向量端口，一次查询只发一次向量
// 检索，不同代际的同一片段不得合并进结果。
// 逆改点：查询不带 generation 条件 → 命中旧代际向量（`backends[0]`），正文与
// 调用记录同时红。
func TestRetrieval_SingleGenerationPerQuery(t *testing.T) {
	logger, _ := captureLogger(t)
	authz := &fakeAuthz{}
	authz.register("kb", "")
	keyword := &fakeKeywordPath{} // 空后端：关键词路正常返回零命中
	stale := Hit{
		ChunkID:      "chunk_shared",
		DocumentID:   "doc_1",
		VersionID:    "ver_1",
		CollectionID: "kb",
		SourcePath:   "ops/runbook.md",
		Text:         "旧代际内容",
		ContentHash:  sha256Hex("旧代际内容"),
	}
	fresh := stale
	fresh.VersionID = "ver_2"
	fresh.Text = "新代际内容"
	fresh.ContentHash = sha256Hex("新代际内容")
	vectors := &generationVectorPath{backends: map[int][]Hit{0: {stale}, 2: {fresh}}}
	retriever := NewRetriever(RetrieverDeps{
		Authz:       authz,
		Keyword:     keyword,
		Vectors:     vectors,
		Assembly:    &fakeAssemblyRecorder{},
		Diagnostics: &fakeEventSink{},
		Logger:      logger,
		Now:         fixedClock(),
	})

	got, err := retriever.Retrieve(context.Background(), RetrieveRequest{
		Principal:     principalFor("team-a"),
		RunID:         "run_generation",
		Query:         "备份",
		CollectionIDs: []string{"kb"},
		Embedding:     make([]float32, VectorDimensions),
		Generation:    2,
	})
	require.NoError(t, err)
	require.Equal(t, []int{2}, vectors.calls, "一次查询只发一次向量检索，且代际原样下传")
	require.Equal(t, []string{"kb"}, vectors.lastScope.Collections)
	require.Equal(t, 2, got.Generation)
	require.Equal(t, StrategyHybrid, got.Strategy)
	require.Len(t, got.Hits, 1, "同一片段只允许来自固定代际，不得跨代合并")
	require.Equal(t, "新代际内容", got.Hits[0].Text)
}

// C-10 —— 删除与撤权必须立即影响后续查询：解析后的可见范围原样传给两路检索，
// 被删集合的片段不得被召回；撤权发生在两次查询之间时，下一次查询立刻看不到旧内容
// （每轮重查，无缓存）。
// 逆改点：用请求里的集合代替解析后的 Scope 去检索 → 被删集合的片段回流，红；
// 缓存上一轮 Scope → 第二次查询仍返回旧命中，红。
func TestRetrieval_DeletedVersionNotRecalled(t *testing.T) {
	h := newRetrievalHarness(t)
	live := h.seedChunk(t, "public", "ops/live.md", "在用资料")
	// 已删除文档的片段仍在后端里——如果检索器不按解析后的范围过滤就会召回它。
	deletedChunk := Hit{
		ChunkID:      "chunk_deleted",
		DocumentID:   "doc_deleted",
		VersionID:    "ver_deleted_1",
		CollectionID: "archive", // 未登记 = 已随文档删除退出可见范围
		SourcePath:   "ops/deleted.md",
		ContentHash:  sha256Hex("已删除资料"),
		Text:         "已删除资料",
	}
	h.keyword.path.backend = append(h.keyword.path.backend, deletedChunk)
	h.vectors.path.backend = append(h.vectors.path.backend, deletedChunk)

	req := RetrieveRequest{
		Principal:     principalFor("team-a"),
		RunID:         h.runID,
		Query:         "资料",
		CollectionIDs: []string{"public", "archive"},
		Embedding:     make([]float32, VectorDimensions),
		Generation:    2,
	}
	got, err := h.retriever.Retrieve(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, []string{"public"}, h.keyword.path.lastScope.Collections,
		"两路检索只能拿到解析后的可见集合")
	require.Equal(t, []string{"public"}, h.vectors.path.lastScope.Collections)
	require.Len(t, got.Hits, 1)
	require.Equal(t, live.ChunkID, got.Hits[0].ChunkID)
	for _, hit := range got.Hits {
		require.NotEqual(t, "doc_deleted", hit.DocumentID, "已删除版本的片段不得被召回")
		require.NotContains(t, hit.Text, "已删除资料")
	}
	require.Equal(t, 1, got.DeniedCount, "被剔除的集合只给计数")
	// C-08：被拒集合的标识、来源路径与片段都不得进入日志。
	require.NotContains(t, h.logs.String(), "archive")
	require.NotContains(t, h.logs.String(), "ops/deleted.md")
	require.NotContains(t, h.logs.String(), "已删除资料")

	// 撤权发生在两次查询之间：下一次查询必须立刻看不到 public 的内容。
	for i := range h.authz.collections {
		if h.authz.collections[i].id == "public" {
			h.authz.collections[i].acl = "team-b"
		}
	}
	again, err := h.retriever.Retrieve(context.Background(), req)
	require.NoError(t, err)
	require.Empty(t, again.Hits, "撤权后旧片段必须立即不可召回——不得沿用上一轮的可见范围")
	require.GreaterOrEqual(t, again.DeniedCount, 1)
	require.Empty(t, h.vectors.path.lastScope.Collections)
}

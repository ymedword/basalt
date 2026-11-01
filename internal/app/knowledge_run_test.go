package app

// 本文件锁死 `knowledge_run.go`（US-16 V-6）的三条出口：装配检索 / 轮内复核 /
// 引用复核。夹具用假端口驱动**真实**的 `knowledge.Retriever` 与
// `knowledge.SourceViewer`——被测的是适配器的分流与编号逻辑，而不是重写一遍检索。
//
// 每个用例都写明逆改点（实现怎么改它会红），避免"跑了但不判卷"：
//   - 标记连续编号：被剔除的命中占号 → 编号断言红；
//   - 权限先于向量化：把 scope 判定挪到 `embedQuery` 之后 → 嵌入调用计数断言红；
//   - 撤权 / 换版只收窄：复核时补入或重排 → 标记与集合断言红；
//   - 引用只用精确读取：把 `view.Text` 用在 `Validity.Valid=false` 的分支 → 被取代
//     版本的用例从 Unsupported 变 Supported，红。
//
// This file pins the three exits of the run-side knowledge port using fake ports around
// the real retriever and source viewer; each test names the mutation that turns it red.

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// runSrcAuthz 是 `knowledge.Authz` 的假实现：按集合标识判可见；按 Agent 解析时
// 返回全部可见集合。`forbidden` 让"声明全部被剔除"走领域哨兵。
// runSrcAuthz is the fake scope resolver: per-collection visibility, plus an
// all-denied mode that returns the domain sentinel.
type runSrcAuthz struct {
	visible   map[string]bool
	forbidden bool
	err       error
	calls     int
}

func (a *runSrcAuthz) Resolve(_ context.Context, _ core.Principal, agent string,
	collections []string,
) (knowledge.Scope, error) {
	a.calls++
	if a.err != nil {
		return knowledge.Scope{}, a.err
	}
	if a.forbidden {
		return knowledge.Scope{}, core.ErrForbidden
	}
	scope := knowledge.Scope{Agent: agent}
	if len(collections) == 0 {
		for id, ok := range a.visible {
			if ok {
				scope.Collections = append(scope.Collections, id)
			}
		}
		slices.Sort(scope.Collections)
		return scope, nil
	}
	for _, id := range collections {
		if a.visible[id] {
			scope.Collections = append(scope.Collections, id)
			continue
		}
		scope.DeniedCount++
	}
	return scope, nil
}

// runSrcKeyword 是关键词路的假实现：命中由用例给定，同时记录收到的范围与查询。
// runSrcKeyword returns the scripted hits and records the scope it received.
type runSrcKeyword struct {
	hits  []knowledge.Hit
	err   error
	calls int
}

func (s *runSrcKeyword) Search(_ context.Context, _ knowledge.Scope, _ string, _ int,
) ([]knowledge.Hit, int, error) {
	s.calls++
	if s.err != nil {
		return nil, 0, s.err
	}
	return s.hits, 0, nil
}

// runSrcVector 是向量路的假实现；缺省按"不可用"降级（与未配置 embedding 的真实
// 部署同一形态），用例需要向量命中时显式给 hits。
// runSrcVector degrades by default, mirroring a deployment without embeddings.
type runSrcVector struct {
	hits  []knowledge.Hit
	err   error
	calls int
}

func (s *runSrcVector) Search(_ context.Context, _ knowledge.Scope, _ []float32, _, _ int,
) ([]knowledge.Hit, int, error) {
	s.calls++
	if s.err != nil {
		return nil, 0, s.err
	}
	return s.hits, 0, nil
}

// runSrcAssembly 记录装配证据；`Retrieve` 的成功路径必须恰好写一条。
// runSrcAssembly captures the assembly records; a successful retrieval writes exactly one.
type runSrcAssembly struct {
	records []core.AssemblyRecord
}

func (a *runSrcAssembly) Record(_ context.Context, record core.AssemblyRecord) {
	a.records = append(a.records, record)
}

// runSrcDiag 记录诊断事件（可丢弃面，测试里当成只增切片）。
// runSrcDiag appends diagnostic events; production may drop them.
type runSrcDiag struct {
	events []knowledge.DiagnosticEvent
}

func (d *runSrcDiag) Record(_ context.Context, event knowledge.DiagnosticEvent) {
	d.events = append(d.events, event)
}

// reasons 取某个原因码下的事件，避免把降级噪声当断言目标。
// reasons filters the captured events by reason code.
func (d *runSrcDiag) reasons(reason knowledge.DiagnosticReason) []knowledge.DiagnosticEvent {
	var out []knowledge.DiagnosticEvent
	for _, event := range d.events {
		if event.Reason == reason {
			out = append(out, event)
		}
	}
	return out
}

// runSrcReader 是精确读取端口：键为 `文档/版本`，未登记的引用与真实端口一样回
// `ErrNotFound`（`SourceView` 据此给出 `not_found` 判定）。
// runSrcReader is the exact-read port keyed by document/version.
type runSrcReader struct {
	versions map[string]knowledge.SourceVersionRecord
	err      error
}

func (r *runSrcReader) LoadVersion(_ context.Context, documentID, versionID string) (knowledge.SourceVersionRecord, error) {
	if r.err != nil {
		return knowledge.SourceVersionRecord{}, r.err
	}
	record, ok := r.versions[documentID+"/"+versionID]
	if !ok {
		return knowledge.SourceVersionRecord{}, knowledge.ErrNotFound
	}
	return record, nil
}

// runSrcEmbedder 是查询向量化的假实现；calls 用于守住"身份判定先于外部调用"。
// runSrcEmbedder records call counts so identity checks can be proven to run first.
type runSrcEmbedder struct {
	vectors [][]float32
	err     error
	calls   int
}

func (e *runSrcEmbedder) Embed(_ context.Context, _ []string) (knowledge.EmbedBatch, error) {
	e.calls++
	if e.err != nil {
		return knowledge.EmbedBatch{}, e.err
	}
	return knowledge.EmbedBatch{Vectors: e.vectors}, nil
}

// runSrcFixture 把假端口、真实检索器和真实来源视图装成一份可复用的夹具。
// runSrcFixture assembles the fakes with the real retriever and source viewer.
type runSrcFixture struct {
	authz    *runSrcAuthz
	keyword  *runSrcKeyword
	vectors  *runSrcVector
	assembly *runSrcAssembly
	diag     *runSrcDiag
	reader   *runSrcReader
	embedder *runSrcEmbedder
	source   *knowledgeRunSource
}

func newRunSrcFixture(t *testing.T) *runSrcFixture {
	t.Helper()
	fixture := &runSrcFixture{
		authz:    &runSrcAuthz{visible: map[string]bool{}},
		keyword:  &runSrcKeyword{},
		vectors:  &runSrcVector{err: errors.New("向量路未装配")},
		assembly: &runSrcAssembly{},
		diag:     &runSrcDiag{},
		reader:   &runSrcReader{versions: map[string]knowledge.SourceVersionRecord{}},
		embedder: &runSrcEmbedder{},
	}
	logger := slog.New(slog.DiscardHandler)
	retriever := knowledge.NewRetriever(knowledge.RetrieverDeps{
		Authz:       fixture.authz,
		Keyword:     fixture.keyword,
		Vectors:     fixture.vectors,
		Assembly:    fixture.assembly,
		Diagnostics: fixture.diag,
		Logger:      logger,
	})
	viewer := knowledge.NewSourceViewer(knowledge.SourceViewerDeps{
		Reader: fixture.reader,
		Authz:  fixture.authz,
		Logger: logger,
	})
	fixture.source = newKnowledgeRunSource(retriever, viewer, fixture.embedder, 0,
		fixture.diag, logger)
	return fixture
}

// runSrcPrincipal 是持有 `knowledge:read` 的调用方身份。
// runSrcPrincipal holds the knowledge read scope.
func runSrcPrincipal() core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller_1",
		KeyID:    "key_1",
		Scopes:   []string{core.ScopeKnowledgeRead},
		Actor:    core.ActorCaller,
	}
}

// runSrcRequest 是一次运行级检索请求；Agent 缺省非空（真实 Run 必有 Agent）。
// runSrcRequest builds a run-level retrieval request with an agent.
func runSrcRequest() core.KnowledgeRunRequest {
	return core.KnowledgeRunRequest{
		Principal: runSrcPrincipal(),
		RunID:     "run_1",
		Agent:     "ops",
		Query:     "退款政策",
	}
}

// runSrcChunk 构造一条已注入片段（引用三段标识齐备）。
// runSrcChunk builds an injected chunk with a complete reference.
func runSrcChunk(marker, documentID, versionID, hash string) core.KnowledgeChunk {
	return core.KnowledgeChunk{
		Marker: marker,
		Text:   "片段正文 " + marker,
		Ref: core.EvidenceRef{
			Kind:        core.EvidenceKindKnowledge,
			ID:          documentID,
			VersionID:   versionID,
			ContentHash: hash,
		},
	}
}

// runSrcRecord 是一条"当前生效且正文可回读"的版本记录。
// runSrcRecord is an active version with a readable body.
func runSrcRecord(documentID, versionID, hash, text string) knowledge.SourceVersionRecord {
	return knowledge.SourceVersionRecord{
		DocumentID:   documentID,
		VersionID:    versionID,
		CollectionID: "kb_1",
		SourcePath:   documentID + ".md",
		ContentHash:  hash,
		Status:       knowledge.VersionActive,
		Text:         text,
	}
}

// TestKnowledgeRunSource_RetrieveForRun_RenumbersMarkersAndRecords 守装配出口：
// 命中的引用被精确读取校验收窄（失效引用与内容漂移都剔除），标记按**最终顺序**
// 连续编号（不留空洞），装配记录挂在 Run 上且无轮次——StepID 留空。
// 逆改点：剔除后仍按原下标编号 / 失败只 WARN 不剔除 / StepID 填伪造值 → 三条断言分别红。
func TestKnowledgeRunSource_RetrieveForRun_RenumbersMarkersAndRecords(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = true
	fixture.keyword.hits = []knowledge.Hit{
		{
			ChunkID: "chk_1", DocumentID: "doc_1", VersionID: "v_1", CollectionID: "kb_1",
			SourcePath: "退款.md", HeadingPath: "政策 > 退款", ContentHash: "hash_1", Text: "退款政策为七天无理由。",
		},
		// 内容哈希与索引副本不一致：旧片段不得当原文注入。
		{
			ChunkID: "chk_2", DocumentID: "doc_2", VersionID: "v_2", CollectionID: "kb_1",
			ContentHash: "hash_2_stale", Text: "过期的退款条款。",
		},
		// 缺少版本标识：引用无法复核，不得注入。
		{
			ChunkID: "chk_3", DocumentID: "doc_3", CollectionID: "kb_1",
			ContentHash: "hash_3", Text: "没有版本的片段。",
		},
		{
			ChunkID: "chk_4", DocumentID: "doc_4", VersionID: "v_4", CollectionID: "kb_1",
			SourcePath: "发票.md", HeadingPath: "财务 > 发票", ContentHash: "hash_4", Text: "发票在三个工作日内开具。",
		},
	}
	fixture.reader.versions["doc_1/v_1"] = runSrcRecord("doc_1", "v_1", "hash_1", "退款政策为七天无理由。")
	fixture.reader.versions["doc_2/v_2"] = runSrcRecord("doc_2", "v_2", "hash_2", "最新的退款条款。")
	fixture.reader.versions["doc_4/v_4"] = runSrcRecord("doc_4", "v_4", "hash_4", "发票在三个工作日内开具。")

	injection, err := fixture.source.RetrieveForRun(t.Context(), runSrcRequest())
	require.NoError(t, err)
	require.NotNil(t, injection, "有效命中必须产出注入面")
	require.Len(t, injection.Chunks, 2, "索引漂移与缺版本标识的命中都不得注入")

	require.Equal(t, "c1", injection.Chunks[0].Marker)
	require.Equal(t, "doc_1", injection.Chunks[0].Ref.ID)
	require.Equal(t, "退款.md", injection.Chunks[0].SourcePath)
	require.Equal(t, "政策 > 退款", injection.Chunks[0].HeadingPath)
	require.Equal(t, "c2", injection.Chunks[1].Marker,
		"标记按最终顺序连续编号：被剔除的命中不占号（逆改点：沿用原下标 → c4）")
	require.Equal(t, "doc_4", injection.Chunks[1].Ref.ID)
	require.Equal(t, "hash_4", injection.Chunks[1].Ref.ContentHash)

	require.Len(t, fixture.assembly.records, 1, "成功检索恰好一条装配记录")
	record := fixture.assembly.records[0]
	require.Equal(t, "run_1", record.RunID)
	require.Equal(t, "", record.StepID, "运行级检索发生在第 1 轮之前，没有轮次可填")
	require.Equal(t, core.PurposeRetrieval, record.Purpose)
	require.Equal(t, "ops", record.Agent)
}

// TestKnowledgeRunSource_RetrieveForRun_ScopeGatePrecedesEmbedding 守准入次序：
// 缺 `knowledge:read` 的调用方在**任何外部调用之前**被拒——不向量化、不检索，
// 只留一条 `permission_denied` 诊断（deny 判定）。
// 逆改点：把 scope 判定挪到 embedQuery 之后 → 嵌入调用计数与端口计数断言红。
func TestKnowledgeRunSource_RetrieveForRun_ScopeGatePrecedesEmbedding(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = true
	request := runSrcRequest()
	request.Principal.Scopes = []string{core.ScopeSessionsWrite}

	injection, err := fixture.source.RetrieveForRun(t.Context(), request)
	require.NoError(t, err, "无权限是合法空注入，不是故障")
	require.Nil(t, injection)
	require.Zero(t, fixture.embedder.calls, "身份判定必须先于外部向量化调用")
	require.Zero(t, fixture.keyword.calls)
	require.Empty(t, fixture.assembly.records)

	events := fixture.diag.reasons(knowledge.DiagnosticReasonPermissionDenied)
	require.Len(t, events, 1, "权限拒绝必须留诊断（可丢弃但不留空）")
	require.Equal(t, "run_1", events[0].RunID)
	require.Equal(t, knowledge.DiagnosticDecisionDeny, events[0].Decision)
	require.Equal(t, core.ActorCaller, events[0].Actor)
}

// TestKnowledgeRunSource_RetrieveForRun_DomainForbiddenIsEmptyInjection 守"声明集合
// 全部被剔除"的语义：领域回 `ErrForbidden` 是合法空注入（知识是增强面，不是运行
// 前提），并记一条权限拒绝诊断。
// 逆改点：把 ErrForbidden 归入"存储违约"上抛 → Run 被无权限拖成 failed，断言红。
func TestKnowledgeRunSource_RetrieveForRun_DomainForbiddenIsEmptyInjection(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.forbidden = true

	injection, err := fixture.source.RetrieveForRun(t.Context(), runSrcRequest())
	require.NoError(t, err)
	require.Nil(t, injection)
	require.Len(t, fixture.diag.reasons(knowledge.DiagnosticReasonPermissionDenied), 1)
}

// TestKnowledgeRunSource_RetrieveForRun_InvalidAgentIsEmptyInjection 守形状拒绝：
// 没有 Agent 也没有集合 → 领域 `ErrInvalidRequest`（契约禁止全库检索）→ 空注入，
// 不判 Run 失败。
// 逆改点：把 ErrInvalidRequest 归入上抛分支 → 断言红。
func TestKnowledgeRunSource_RetrieveForRun_InvalidAgentIsEmptyInjection(t *testing.T) {
	fixture := newRunSrcFixture(t)
	request := runSrcRequest()
	request.Agent = "  "

	injection, err := fixture.source.RetrieveForRun(t.Context(), request)
	require.NoError(t, err)
	require.Nil(t, injection)
	require.Empty(t, fixture.diag.reasons(knowledge.DiagnosticReasonPermissionDenied),
		"形状拒绝不是权限拒绝，不得冒充 deny")
}

// TestKnowledgeRunSource_RetrieveForRun_PropagatesStorageFailure 守真故障：两路
// 检索都不可用 → 错误上抛，由 Run 拒绝启动（零模型调用、零工具执行）。
// 逆改点：失败也吞成空注入 → 断言红。
func TestKnowledgeRunSource_RetrieveForRun_PropagatesStorageFailure(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = true
	fixture.keyword.err = errors.New("pg: connection reset")
	fixture.vectors.err = errors.New("pg: connection reset")

	injection, err := fixture.source.RetrieveForRun(t.Context(), runSrcRequest())
	require.Error(t, err, "存储违约必须让 Run 拒绝启动")
	require.Nil(t, injection)
}

// TestKnowledgeRunSource_RetrieveForRun_ViewerFailurePropagates 守"精确读取违约
// 不是空注入"：存储回读失败要上抛，**不得**当成"片段失效"静默剔除。
// 逆改点：把 err 分支并入 revoked 计数 → 断言红。
func TestKnowledgeRunSource_RetrieveForRun_ViewerFailurePropagates(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = true
	fixture.keyword.hits = []knowledge.Hit{{
		ChunkID: "chk_1", DocumentID: "doc_1", VersionID: "v_1", CollectionID: "kb_1",
		ContentHash: "hash_1", Text: "退款政策。",
	}}
	fixture.reader.err = errors.New("pg: read failed")

	injection, err := fixture.source.RetrieveForRun(t.Context(), runSrcRequest())
	require.Error(t, err)
	require.Nil(t, injection)
}

// TestKnowledgeRunSource_RetrieveForRun_CancelledDuringEmbedding 守取消优先：
// 向量化期间取消不是"检索降级"，更不是空注入——按 ctx 错误上抛，由循环记 cancelled。
// 逆改点：返回 (nil, nil) 或包装成存储错误 → errors.Is 断言红。
func TestKnowledgeRunSource_RetrieveForRun_CancelledDuringEmbedding(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = true
	fixture.source.embedder = fixture.embedder
	fixture.source.generation = 3
	fixture.embedder.err = context.Canceled

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := fixture.source.RetrieveForRun(ctx, runSrcRequest())
	require.ErrorIs(t, err, context.Canceled)
}

// TestKnowledgeRunSource_RetrieveForRun_EmptyQuerySkipsEverything 守空消息：
// 没有查询词就不检索、不记诊断（没有可判定的动作）。
// 逆改点：空查询也走检索 → 端口计数断言红。
func TestKnowledgeRunSource_RetrieveForRun_EmptyQuerySkipsEverything(t *testing.T) {
	fixture := newRunSrcFixture(t)
	request := runSrcRequest()
	request.Query = "  "

	injection, err := fixture.source.RetrieveForRun(t.Context(), request)
	require.NoError(t, err)
	require.Nil(t, injection)
	require.Zero(t, fixture.keyword.calls)
	require.Empty(t, fixture.diag.events)
}

// TestKnowledgeRunSource_RevalidateChunks_OnlyNarrowsAndKeepsMarkers 守轮内复核：
// 撤权与换版都是结论（剔除），标记保持原编号（允许空洞），不补入新片段。
// 逆改点：复核后重排标记 → c1/c3 断言红；把撤权当错误 → 断言红。
func TestKnowledgeRunSource_RevalidateChunks_OnlyNarrowsAndKeepsMarkers(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = true
	fixture.reader.versions["doc_1/v_1"] = runSrcRecord("doc_1", "v_1", "hash_1", "第一段。")
	fixture.reader.versions["doc_3/v_3"] = runSrcRecord("doc_3", "v_3", "hash_3", "第三段。")
	// doc_2 的版本记录存在，但所属集合已不可见（`kb_2` 不在假 Authz 的可见集里）：
	// 可见性判定先于状态判定，读取回 ErrForbidden，按撤权剔除。
	revoked := runSrcRecord("doc_2", "v_2", "hash_2", "第二段。")
	revoked.CollectionID = "kb_2"
	fixture.reader.versions["doc_2/v_2"] = revoked

	chunks := []core.KnowledgeChunk{
		runSrcChunk("c1", "doc_1", "v_1", "hash_1"),
		runSrcChunk("c2", "doc_2", "v_2", "hash_2"),
		runSrcChunk("c3", "doc_3", "v_3", "hash_3"),
	}

	injection, err := fixture.source.RevalidateChunks(t.Context(), core.KnowledgeRevalidateRequest{
		Principal: runSrcPrincipal(),
		RunID:     "run_1",
		Chunks:    chunks,
	})
	require.NoError(t, err)
	require.NotNil(t, injection)
	require.Len(t, injection.Chunks, 2)
	require.Equal(t, "c1", injection.Chunks[0].Marker)
	require.Equal(t, "c3", injection.Chunks[1].Marker,
		"标记保持原编号：剔除后的 c3 不重排成 c2（逆改点：重编号 → 断言红）")
}

// TestKnowledgeRunSource_RevalidateChunks_AllGoneIsNil 守"全部失效"：返回 nil 注入，
// 循环据此从本轮起不再向提示词注入知识片段。
// 逆改点：返回空切片非 nil 或报错 → 断言红。
func TestKnowledgeRunSource_RevalidateChunks_AllGoneIsNil(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = false

	injection, err := fixture.source.RevalidateChunks(t.Context(), core.KnowledgeRevalidateRequest{
		Principal: runSrcPrincipal(),
		RunID:     "run_1",
		Chunks:    []core.KnowledgeChunk{runSrcChunk("c1", "doc_1", "v_1", "hash_1")},
	})
	require.NoError(t, err)
	require.Nil(t, injection)
}

// TestKnowledgeRunSource_ResolveAnswer_ReportsSupportedAndUnresolved 守引用复核：
// 可定位且被支持的引用进 Supported；上下文没读过的标识进 Unresolved，并写一条
// `citation_unresolved` 诊断（只带计数、判定留空）。
// 逆改点：不写事件 / 写决策 / 把未解析标识放进 attrs → 三条断言分别红。
func TestKnowledgeRunSource_ResolveAnswer_ReportsSupportedAndUnresolved(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = true
	fixture.reader.versions["doc_1/v_1"] = runSrcRecord("doc_1", "v_1", "hash_1",
		"退款政策为七天无理由退换。")

	report, err := fixture.source.ResolveAnswer(t.Context(), core.KnowledgeAnswerRequest{
		Principal: runSrcPrincipal(),
		RunID:     "run_1",
		Answer:    "退款政策为七天无理由退换（见 c1）。另见 [c9]。",
		Chunks:    []core.KnowledgeChunk{runSrcChunk("c1", "doc_1", "v_1", "hash_1")},
	})
	require.NoError(t, err)
	require.Equal(t, 1, report.Supported, "精确读取的片段支持的引用必须计入 Supported")
	require.Equal(t, 1, report.Unresolved, "上下文没读过的标识计入 Unresolved")

	events := fixture.diag.reasons(knowledge.DiagnosticReasonCitationUnresolved)
	require.Len(t, events, 1)
	require.Equal(t, "run_1", events[0].RunID)
	require.Equal(t, map[string]any{"count": 1}, events[0].Attrs,
		"只带计数：未解析的标识本身不得进诊断")
	require.Equal(t, knowledge.DiagnosticDecision(""), events[0].Decision,
		"结论性事件没有 allow/deny 可填，判定如实留空")
}

// TestKnowledgeRunSource_ResolveAnswer_RevokedSourceIsUnsupported 守"撤权即不可定位"：
// 复核时已不可读的片段留在来源列表里但内容哈希为空，引用归 Unsupported，**不得**
// 用检索命中的文本冒充原文。
// 逆改点：撤权时仍填 ContentHash/Text → 断言从 Unsupported 变 Supported，红。
func TestKnowledgeRunSource_ResolveAnswer_RevokedSourceIsUnsupported(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = false
	fixture.reader.versions["doc_1/v_1"] = runSrcRecord("doc_1", "v_1", "hash_1",
		"退款政策为七天无理由退换。")

	report, err := fixture.source.ResolveAnswer(t.Context(), core.KnowledgeAnswerRequest{
		Principal: runSrcPrincipal(),
		RunID:     "run_1",
		Answer:    "退款政策为七天无理由退换（见 c1）。",
		Chunks:    []core.KnowledgeChunk{runSrcChunk("c1", "doc_1", "v_1", "hash_1")},
	})
	require.NoError(t, err)
	require.Zero(t, report.Supported)
	require.Equal(t, 1, report.Unsupported)
	require.Zero(t, report.Unresolved)
}

// TestKnowledgeRunSource_ResolveAnswer_SupersededTextNeverBecomesEvidence 守"旧正文
// 不冒充引用依据"：被取代版本即便回了旧原文，也不得成为可定位来源。
// 逆改点：用 view.Text 非空代替 Validity.Valid 判定 → 本用例从 Unsupported 变
// Supported，红。
func TestKnowledgeRunSource_ResolveAnswer_SupersededTextNeverBecomesEvidence(t *testing.T) {
	fixture := newRunSrcFixture(t)
	fixture.authz.visible["kb_1"] = true
	record := runSrcRecord("doc_1", "v_1", "hash_1", "退款政策为七天无理由退换。")
	record.Status = knowledge.VersionSuperseded
	fixture.reader.versions["doc_1/v_1"] = record

	report, err := fixture.source.ResolveAnswer(t.Context(), core.KnowledgeAnswerRequest{
		Principal: runSrcPrincipal(),
		RunID:     "run_1",
		Answer:    "退款政策为七天无理由退换（见 c1）。",
		Chunks:    []core.KnowledgeChunk{runSrcChunk("c1", "doc_1", "v_1", "hash_1")},
	})
	require.NoError(t, err)
	require.Zero(t, report.Supported, "被取代版本的旧正文不得充当引用依据")
	require.Equal(t, 1, report.Unsupported)
}

// TestKnowledgeRunSource_ResolveAnswer_RequiresIdentityAndRun 守锚点必填：
// 零值身份 → `ErrUnauthorized`；缺 Run 标识 → 错误（事件与记录必须挂在 Run 上）。
// 逆改点：缺 Run 时照常返回报告 → 断言红。
func TestKnowledgeRunSource_ResolveAnswer_RequiresIdentityAndRun(t *testing.T) {
	fixture := newRunSrcFixture(t)

	_, err := fixture.source.ResolveAnswer(t.Context(), core.KnowledgeAnswerRequest{
		RunID:  "run_1",
		Answer: "见 c1。",
	})
	require.ErrorIs(t, err, core.ErrUnauthorized)

	_, err = fixture.source.ResolveAnswer(t.Context(), core.KnowledgeAnswerRequest{
		Principal: runSrcPrincipal(),
		Answer:    "见 c1。",
	})
	require.Error(t, err)
}

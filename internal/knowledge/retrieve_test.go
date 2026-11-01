package knowledge

// 本文件覆盖 T038 的混合检索：R6 的关键回归（被拒片段不得进入上下文 / 日志 /
// 诊断事件，但计数必须可见）、RRF 融合与并列次序、单路降级与两路皆失败、
// 预算裁剪、装配记录只存标识，以及"管理面检索不伪造 Run 锚点"。
//
// 端口用进程内假实现：`Authz` 按集合 ACL 过滤并计数，两路检索各自记录调用参数，
// 装配记录与诊断事件落内存切片——断言直接对着这些读数做，不经过数据库。
//
// This file covers the retrieval pipeline: the denied-chunk regression, RRF fusion and
// tie-breaking, single-path degradation, budget trimming, the identifiers-only assembly
// record, and the rule that a management-facing search never fabricates a run anchor.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeCollection 是一个集合及其 ACL 标签（标签为空表示对所有项目可见）。
// fakeCollection is one collection with its ACL label; empty means visible to all.
type fakeCollection struct {
	id  string
	acl string
}

// fakeAuthz 按集合 ACL 解析可见范围：ACL 与调用方项目不符的集合被剔除并计数，
// 未注册的集合同样计为拒绝（它们不存在）。实现**不**回传被拒集合的标识——
// 与生产端口的"只给计数"一致。
// fakeAuthz resolves the scope by collection ACL and only reports a denial count.
type fakeAuthz struct {
	collections   []fakeCollection
	err           error
	calls         int
	lastAgent     string
	lastRequested []string
}

func (f *fakeAuthz) Resolve(_ context.Context, p core.Principal, agent string, collections []string) (Scope, error) {
	f.calls++
	f.lastAgent = agent
	f.lastRequested = append([]string(nil), collections...)
	if f.err != nil {
		return Scope{}, f.err
	}
	requested := collections
	if len(requested) == 0 {
		for _, collection := range f.collections {
			requested = append(requested, collection.id)
		}
	}
	scope := Scope{Agent: agent}
	for _, id := range requested {
		found, ok := f.lookup(id)
		if !ok || (found.acl != "" && found.acl != p.Project) {
			scope.DeniedCount++
			continue
		}
		scope.Collections = append(scope.Collections, id)
	}
	return scope, nil
}

// register 幂等登记集合：调用方指定的 ACL 只在首次登记时生效。
// register is idempotent; the first ACL wins.
func (f *fakeAuthz) register(id, acl string) {
	if _, ok := f.lookup(id); ok {
		return
	}
	f.collections = append(f.collections, fakeCollection{id: id, acl: acl})
}

func (f *fakeAuthz) lookup(id string) (fakeCollection, bool) {
	for _, collection := range f.collections {
		if collection.id == id {
			return collection, true
		}
	}
	return fakeCollection{}, false
}

// ids 按登记顺序返回全部集合标识（请求按此展开，模拟"全库"查询）。
// ids returns every registered collection in registration order.
func (f *fakeAuthz) ids() []string {
	out := make([]string, 0, len(f.collections))
	for _, collection := range f.collections {
		out = append(out, collection.id)
	}
	return out
}

// fakePath 是两路检索共用的后端：按范围的集合过滤、可整体反转次序、可注入错误。
// fakePath is the shared backing store of both search paths.
type fakePath struct {
	backend   []Hit
	reverse   bool
	err       error
	denied    int
	calls     int
	lastScope Scope
	lastK     int
}

// collect 只返回落在可见集合内的命中——被拒集合的片段在这里就消失，生产实现由
// SQL 内的过滤承担（C-08 的第一半）。
// collect returns only hits inside the resolved scope; production does this inside SQL.
func (p *fakePath) collect(scope Scope, k int) []Hit {
	allowed := make(map[string]bool, len(scope.Collections))
	for _, id := range scope.Collections {
		allowed[id] = true
	}
	out := make([]Hit, 0, len(p.backend))
	for _, hit := range p.backend {
		if allowed[hit.CollectionID] {
			out = append(out, hit)
		}
	}
	if p.reverse {
		slices.Reverse(out)
	}
	if k > 0 && len(out) > k {
		out = out[:k]
	}
	return out
}

// fakeKeywordPath 是关键词路径端口：记录查询文本与候选宽度。
// fakeKeywordPath records the query text and candidate width.
type fakeKeywordPath struct {
	path      fakePath
	lastQuery string
}

func (f *fakeKeywordPath) Search(_ context.Context, scope Scope, query string, k int) ([]Hit, int, error) {
	f.path.calls++
	f.path.lastScope = scope
	f.path.lastK = k
	f.lastQuery = query
	if f.path.err != nil {
		return nil, 0, f.path.err
	}
	return f.path.collect(scope, k), f.path.denied, nil
}

// fakeVectorPath 是向量路径端口：记录代际、维度与候选宽度。
// fakeVectorPath records the generation, dimension and candidate width.
type fakeVectorPath struct {
	path           fakePath
	lastGeneration int
	lastDimension  int
}

func (f *fakeVectorPath) Search(_ context.Context, scope Scope, embedding []float32, generation, k int) ([]Hit, int, error) {
	f.path.calls++
	f.path.lastScope = scope
	f.path.lastK = k
	f.lastGeneration = generation
	f.lastDimension = len(embedding)
	if f.path.err != nil {
		return nil, 0, f.path.err
	}
	return f.path.collect(scope, k), f.path.denied, nil
}

// fakeAssemblyRecorder 收集装配记录：`Record` 无返回值，失败语义不在这里。
// fakeAssemblyRecorder collects assembly records; Record has no return value.
type fakeAssemblyRecorder struct {
	records []core.AssemblyRecord
}

func (f *fakeAssemblyRecorder) Record(_ context.Context, record core.AssemblyRecord) {
	f.records = append(f.records, record)
}

// fakeEventSink 收集诊断事件：诊断可丢弃，假实现也不制造错误。事件是
// `knowledge` 的包内窄类型（C-06：本包不 import 诊断包），契约事件的形状由
// `app` 适配器的契约面用例锁死。
// fakeEventSink collects the package-local diagnostic events; the frozen contract
// event shape is pinned by the app adapter's contract test.
type fakeEventSink struct {
	events []DiagnosticEvent
}

func (f *fakeEventSink) Record(_ context.Context, event DiagnosticEvent) {
	f.events = append(f.events, event)
}

// List 按 Run 锚点过滤事件。设计原文写作 `diag.List(ctx, diagnostics.Query{...})`，
// 但诊断读面不在本包：这里用等价的最小过滤，断言逐条保真（差异记入 tasks.md
// 附录 R48）。
// List filters events by run anchor; the minimal filter keeps the assertions identical.
func (f *fakeEventSink) List(runID string) []DiagnosticEvent {
	out := make([]DiagnosticEvent, 0, len(f.events))
	for _, event := range f.events {
		if event.RunID == runID {
			out = append(out, event)
		}
	}
	return out
}

// retrievalHarness 是检索侧的最小装配：四个端口 + 捕获日志面 + 一个固定 Run。
// retrievalHarness wires the four ports, a captured log surface and one fixed run.
type retrievalHarness struct {
	t         *testing.T
	retriever *Retriever
	authz     *fakeAuthz
	keyword   *fakeKeywordPath
	vectors   *fakeVectorPath
	assembly  *fakeAssemblyRecorder
	diag      *fakeEventSink
	logs      *bytes.Buffer
	runID     string
	stepID    string
	agent     string
	nextChunk int
	config    RetrievalConfig
}

// retrievalOption 在装配前调整假端口或配置。
// retrievalOption adjusts the fakes or the config before assembly.
type retrievalOption func(*retrievalHarness)

// withCollectionACL 登记一个带 ACL 标签的集合（如 `secret` 只对 `team-b` 可见）。
// withCollectionACL registers a collection with an ACL label.
func withCollectionACL(collection, acl string) retrievalOption {
	return func(h *retrievalHarness) { h.authz.register(collection, acl) }
}

// withRetrievalConfig 覆盖检索参数（默认全零，走文档默认值）。
// withRetrievalConfig overrides the retrieval parameters.
func withRetrievalConfig(config RetrievalConfig) retrievalOption {
	return func(h *retrievalHarness) { h.config = config }
}

// newRetrievalHarness 装配受测检索器与捕获日志面。
// newRetrievalHarness wires the retriever under test.
func newRetrievalHarness(t *testing.T, opts ...retrievalOption) *retrievalHarness {
	t.Helper()
	logger, buf := captureLogger(t)
	h := &retrievalHarness{
		t:        t,
		authz:    &fakeAuthz{},
		keyword:  &fakeKeywordPath{},
		vectors:  &fakeVectorPath{},
		assembly: &fakeAssemblyRecorder{},
		diag:     &fakeEventSink{},
		logs:     buf,
		runID:    "run_a1",
		stepID:   "step_1",
		agent:    "demo-agent",
	}
	h.authz.register("public", "")
	for _, opt := range opts {
		opt(h)
	}
	h.retriever = NewRetriever(RetrieverDeps{
		Authz:       h.authz,
		Keyword:     h.keyword,
		Vectors:     h.vectors,
		Assembly:    h.assembly,
		Diagnostics: h.diag,
		Config:      h.config,
		Logger:      logger,
		Now:         fixedClock(),
	})
	return h
}

// seedChunk 往两路后端各放一条同片段命中（两路读同一行，正常实现不会有差异）。
// seedChunk seeds the same hit into both paths; both paths read the same row.
func (h *retrievalHarness) seedChunk(t *testing.T, collection, path, text string) Hit {
	t.Helper()
	h.authz.register(collection, "")
	h.nextChunk++
	hit := Hit{
		ChunkID:      fmt.Sprintf("chunk_%02d", h.nextChunk),
		DocumentID:   "doc_" + collection,
		VersionID:    "ver_" + collection + "_1",
		CollectionID: collection,
		SourcePath:   path,
		HeadingPath:  "运维指南 > 清理",
		ContentHash:  sha256Hex(text),
		Text:         text,
	}
	h.keyword.path.backend = append(h.keyword.path.backend, hit)
	h.vectors.path.backend = append(h.vectors.path.backend, hit)
	return hit
}

// principalFor 是持有 `knowledge:read` 的调用方身份，项目标签用于 ACL 判定。
// principalFor is a caller holding knowledge:read; the project drives the ACL check.
func principalFor(project string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller_a",
		KeyID:    "key_a1",
		Project:  project,
		Scopes:   []string{core.ScopeKnowledgeRead},
		Actor:    core.ActorCaller,
	}
}

// queryAs 构造以 `project` 身份发起的检索请求：范围取全部已登记集合（含被拒的），
// 这样拒绝计数才真实产生。
// queryAs builds a retrieval request over every registered collection.
func (h *retrievalHarness) queryAs(project, text string) RetrieveRequest {
	return RetrieveRequest{
		Principal:     principalFor(project),
		RunID:         h.runID,
		StepID:        h.stepID,
		AgentName:     h.agent,
		CollectionIDs: h.authz.ids(),
		Query:         text,
		Embedding:     unitEmbedding(),
		Generation:    1,
	}
}

// query 是以默认项目 `team-a` 发起的检索。
// query retrieves as the default project.
func (h *retrievalHarness) query(text string) RetrieveRequest { return h.queryAs("team-a", text) }

// promptFor 只拼检索上下文（片段正文 + 来源路径 + 标题路径），不含用户 query。
// promptFor assembles the retrieval context only; the user query is not included.
func (h *retrievalHarness) promptFor(res RetrieveResult) string {
	var prompt strings.Builder
	for _, hit := range res.Hits {
		prompt.WriteString(hit.SourcePath)
		prompt.WriteString("\n")
		prompt.WriteString(hit.HeadingPath)
		prompt.WriteString("\n")
		prompt.WriteString(hit.Text)
		prompt.WriteString("\n\n")
	}
	return prompt.String()
}

// unitEmbedding 是满足冻结维度的查询向量：这里只验证传递，不验证语义。
// unitEmbedding satisfies the frozen dimension; only forwarding is under test.
func unitEmbedding() []float32 {
	embedding := make([]float32, VectorDimensions)
	for i := range embedding {
		embedding[i] = 0.1
	}
	return embedding
}

// TestRetrieval_DeniedChunksNeverReachPrompt 是设计 §4 的 R6 回归，逐条保真：
// 被拒片段不得进入模型上下文、日志标题或诊断事件，但拒绝计数必须可见。
// 逆改点：先把未过滤的命中拼进 prompt → 前两条断言红；诊断 attrs 带上来源路径 →
// 最后一组断言红。
// TestRetrieval_DeniedChunksNeverReachPrompt is regression R6 from the design.
func TestRetrieval_DeniedChunksNeverReachPrompt(t *testing.T) {
	h := newRetrievalHarness(t, withCollectionACL("secret", "team-b"))
	const canary = "CANARY-DENIED-7f3a"
	h.seedChunk(t, "secret", "secret-plan.md", canary+" 删除生产库的步骤是……")

	res, err := h.retriever.Retrieve(context.Background(), h.queryAs("team-a", "清理步骤"))
	require.NoError(t, err)
	prompt := h.promptFor(res) // promptFor 只拼检索上下文，不含用户 query
	require.NotContains(t, prompt, canary, "被拒片段不得进入模型上下文")
	require.NotContains(t, prompt, "secret-plan.md")
	require.Positive(t, res.DeniedCount)

	for _, e := range h.diag.List(h.runID) {
		require.NotContains(t, fmt.Sprint(e.Attrs), canary, "诊断不得泄露被拒内容")
		require.NotContains(t, fmt.Sprint(e.Attrs), "secret-plan", "诊断不得泄露被拒标题")
	}
	// C-08 的"日志标题"一轨：WARN / INFO 日志同样不得出现被拒内容与来源路径。
	require.NotContains(t, h.logs.String(), canary)
	require.NotContains(t, h.logs.String(), "secret-plan")
}

// TestRetrieval_FusesBothPathsByRank 守 RRF 与并列次序：分数是名次的倒数和，
// 因此"第 1 + 第 3"严格高于"两路都第 2"（倒数和是凸的）；同分按片段标识升序，
// map 遍历顺序不得泄漏进结果，否则同一输入两次读数可能不同。
// 逆改点：改成按单路原始分相加或按 map 遍历输出 → 次序或并列断言红。
func TestRetrieval_FusesBothPathsByRank(t *testing.T) {
	h := newRetrievalHarness(t)
	first := h.seedChunk(t, "public", "a.md", "alpha")
	second := h.seedChunk(t, "public", "b.md", "beta")
	third := h.seedChunk(t, "public", "c.md", "gamma")
	// 关键词序 a,b,c；向量序 c,b,a：a / c 各是一路第 1、另一路第 3，b 两路都第 2。
	h.vectors.path.reverse = true

	res, err := h.retriever.Retrieve(context.Background(), h.query("清理步骤"))
	require.NoError(t, err)
	require.Equal(t, StrategyHybrid, res.Strategy)
	require.Len(t, res.Hits, 3)
	require.Equal(t, []string{first.ChunkID, third.ChunkID, second.ChunkID},
		[]string{res.Hits[0].ChunkID, res.Hits[1].ChunkID, res.Hits[2].ChunkID})
	for i, hit := range res.Hits {
		require.Equal(t, i+1, hit.Rank, "名次必须连续且从 1 起")
	}
	require.Equal(t, res.Hits[0].Score, res.Hits[1].Score, "首尾名次组合同分")
	require.Greater(t, res.Hits[1].Score, res.Hits[2].Score)
}

// TestRetrieval_VectorFailureDegradesToKeyword 守单路降级：向量路报错时检索仍成功，
// 策略退化为 keyword，降级原因回给调用方并写诊断事件与 WARN 两条轨；代际归零表示
// 向量路没有参与（**不得**回一个看似生效的 generation）。
// 逆改点：向量路报错时整体失败 → NoError 红；吞掉降级原因 → DegradedReason 红。
func TestRetrieval_VectorFailureDegradesToKeyword(t *testing.T) {
	h := newRetrievalHarness(t)
	h.seedChunk(t, "public", "runbook.md", "清理步骤")
	h.vectors.path.err = errors.New("vector index offline")

	res, err := h.retriever.Retrieve(context.Background(), h.query("清理步骤"))
	require.NoError(t, err)
	require.Equal(t, StrategyKeyword, res.Strategy)
	require.Equal(t, string(DiagnosticReasonVectorUnavailable), res.DegradedReason)
	require.Zero(t, res.Generation, "向量路未参与时不得回一个生效代际")
	require.Len(t, res.Hits, 1)

	events := h.diag.List(h.runID)
	require.Len(t, events, 1)
	event := events[0]
	require.False(t, event.OccurredAt.IsZero(), "事件必须带发生时刻")
	require.Equal(t, DiagnosticReasonVectorUnavailable, event.Reason)
	require.Equal(t, DiagnosticDecisionDegrade, event.Decision)
	require.Equal(t, h.runID, event.RunID)
	require.Equal(t, h.stepID, event.StepID)
	require.Equal(t, core.ActorCaller, event.Actor)
	require.Nil(t, event.Attrs, "降级事件不带自造 attrs——脱敏白名单之外的键会被丢弃")
	// 事件种类 / 标识 / 版本 / 关联结构的契约面断言在 app 适配器用例里
	// （knowledge 不 import 诊断包，C-06；见 internal/app/knowledge_adapters_test.go）。

	require.Contains(t, h.logs.String(), "vector_unavailable", "降级必须留 WARN 日志")
	require.Len(t, h.assembly.records, 1, "降级不影响装配记录")
	require.Contains(t, h.assembly.records[0].ReasonCodes, string(DiagnosticReasonVectorUnavailable))
	require.Contains(t, h.assembly.records[0].Decisions, string(DiagnosticDecisionDegrade))
}

// TestRetrieval_KeywordFailureDegradesToVector 守另一侧降级：关键词路报错时向量路
// 独立支撑，代际原样回传（调用方据此知道本次读到的是哪一代索引）。
// 逆改点：把两路耦合在同一个错误分支 → Strategy 或 Generation 红。
func TestRetrieval_KeywordFailureDegradesToVector(t *testing.T) {
	h := newRetrievalHarness(t)
	h.seedChunk(t, "public", "runbook.md", "清理步骤")
	h.keyword.path.err = errors.New("trigram index offline")

	res, err := h.retriever.Retrieve(context.Background(), h.query("清理步骤"))
	require.NoError(t, err)
	require.Equal(t, StrategyVector, res.Strategy)
	require.Equal(t, string(DiagnosticReasonKeywordUnavailable), res.DegradedReason)
	require.Equal(t, 1, res.Generation)
	require.Equal(t, 1, h.vectors.lastGeneration, "代际必须原样传给向量路")
	require.Equal(t, VectorDimensions, h.vectors.lastDimension)
	require.Len(t, res.Hits, 1)
}

// TestRetrieval_BothPathsFailingReturnsError 守"两路皆失败才失败"：整体报错、
// 不返回半截结果，也**不**写装配记录（没有候选就不该留下"装配过"的证据）。
// 逆改点：单路全错仍返回空成功 → Error 或装配记录断言红。
func TestRetrieval_BothPathsFailingReturnsError(t *testing.T) {
	h := newRetrievalHarness(t)
	h.seedChunk(t, "public", "runbook.md", "清理步骤")
	h.keyword.path.err = errors.New("trigram index offline")
	h.vectors.path.err = errors.New("vector index offline")

	res, err := h.retriever.Retrieve(context.Background(), h.query("清理步骤"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "两路均不可用")
	require.Empty(t, res.Hits)
	require.Empty(t, h.assembly.records, "失败的检索不得留下装配记录")
	require.Contains(t, h.logs.String(), "knowledge operation failed")
}

// TestRetrieval_MissingEmbeddingSkipsVectorPath 守"不补零向量"：调用方没给向量时
// 向量路整体跳过（降级），**不得**用零向量把查询发出去换一个假命中。
// 逆改点：用 make([]float32, 512) 兜底 → 向量路调用次数断言红。
func TestRetrieval_MissingEmbeddingSkipsVectorPath(t *testing.T) {
	h := newRetrievalHarness(t)
	h.seedChunk(t, "public", "runbook.md", "清理步骤")
	req := h.query("清理步骤")
	req.Embedding = nil

	res, err := h.retriever.Retrieve(context.Background(), req)
	require.NoError(t, err)
	require.Zero(t, h.vectors.path.calls, "缺向量时不得发出查询")
	require.Equal(t, StrategyKeyword, res.Strategy)
	require.Equal(t, string(DiagnosticReasonVectorUnavailable), res.DegradedReason)
}

// TestRetrieval_WrongEmbeddingDimensionSkipsVectorPath 守维度闸门：维度不符时降级，
// 不得把不合法向量交给索引层（那会在 SQL 层报一个看不懂的错误）。
// 逆改点：去掉维度校验 → 向量路调用次数断言红。
func TestRetrieval_WrongEmbeddingDimensionSkipsVectorPath(t *testing.T) {
	h := newRetrievalHarness(t)
	h.seedChunk(t, "public", "runbook.md", "清理步骤")
	req := h.query("清理步骤")
	req.Embedding = []float32{0.1, 0.2}

	_, err := h.retriever.Retrieve(context.Background(), req)
	require.NoError(t, err)
	require.Zero(t, h.vectors.path.calls)
}

// TestRetrieval_BudgetTrimDropsWholeChunks 守预算裁剪：整条片段被丢弃而**不是**截断
// 正文（半截片段不得进上下文）；裁剪后名次重排且仍从 1 起；候选集不因裁剪缩小；
// `degraded_reason` 保持为空——契约该字段没有 `budget_trimmed` 这个取值。
// 逆改点：按字节截断正文或把 budget 写进 degraded_reason → 对应断言红。
func TestRetrieval_BudgetTrimDropsWholeChunks(t *testing.T) {
	h := newRetrievalHarness(t)
	first := h.seedChunk(t, "public", "a.md", "一二三四五") // 5 token（CJK 每字 1）
	h.seedChunk(t, "public", "b.md", "六七八九十")          // 5 token
	req := h.query("清理步骤")
	req.Budget = 6

	res, err := h.retriever.Retrieve(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	require.Equal(t, first.ChunkID, res.Hits[0].ChunkID)
	require.Equal(t, 5, res.Hits[0].TokenEstimate)
	require.Equal(t, 1, res.Hits[0].Rank)
	require.Empty(t, res.DegradedReason, "预算裁剪不是降级原因")

	record := h.assembly.records[0]
	require.Len(t, record.Candidates, 2, "候选集不因预算裁剪变小")
	require.Equal(t, []string{first.ChunkID}, record.Hits)
	require.Contains(t, record.Decisions, string(DiagnosticDecisionTrim))
	require.Contains(t, record.ReasonCodes, string(DiagnosticReasonBudgetTrimmed))

	events := h.diag.List(h.runID)
	require.Len(t, events, 1)
	require.Equal(t, DiagnosticReasonBudgetTrimmed, events[0].Reason)
	require.Equal(t, DiagnosticDecisionTrim, events[0].Decision)
	require.Equal(t, 1, events[0].Attrs["count"], "被裁掉的条数只以白名单键 count 出现")
}

// TestRetrieval_TopKLimitsFinalHits 守 top_k 与 candidate_k 的分工：候选按融合名次
// 截到 top_k，装配记录仍保留完整候选集（裁剪证据不可丢）。
// 逆改点：把 top_k 当成候选宽度传给检索端口 → 候选集条数断言红。
func TestRetrieval_TopKLimitsFinalHits(t *testing.T) {
	h := newRetrievalHarness(t)
	h.seedChunk(t, "public", "a.md", "alpha")
	h.seedChunk(t, "public", "b.md", "beta")
	h.seedChunk(t, "public", "c.md", "gamma")
	req := h.query("清理步骤")
	req.TopK = 2

	res, err := h.retriever.Retrieve(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, res.Hits, 2)
	require.Len(t, h.assembly.records[0].Candidates, 3)
	require.Greater(t, h.keyword.path.lastK, req.TopK, "候选宽度必须大于最终命中数")
}

// TestRetrieval_AssemblyRecordCarriesIdentifiersOnly 守装配记录的字段口径（§3.4.1）：
// 候选带片段 / 版本 / 内容哈希 / token 估算 / 检索分 / 名次 / ACL 决定，**不**复制正文。
// 逆改点：把 hit.Text 塞进候选或省略 content_hash → 对应断言红。
func TestRetrieval_AssemblyRecordCarriesIdentifiersOnly(t *testing.T) {
	h := newRetrievalHarness(t)
	seeded := h.seedChunk(t, "public", "runbook.md", "清理步骤见附件")
	req := h.query("清理步骤")
	req.CollectionIDs = []string{"public"}

	_, err := h.retriever.Retrieve(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, h.assembly.records, 1)
	record := h.assembly.records[0]
	require.Equal(t, h.runID, record.RunID)
	require.Equal(t, h.stepID, record.StepID)
	require.Equal(t, core.PurposeRetrieval, record.Purpose)
	require.Equal(t, h.agent, record.Agent)
	require.Equal(t, core.EstimateVersion, record.EstimatorVersion)
	require.Equal(t, []string{seeded.ChunkID}, record.Hits)
	require.Len(t, record.Candidates, 1)
	candidate := record.Candidates[0]
	require.Equal(t, seeded.ChunkID, candidate.ChunkID)
	require.Equal(t, seeded.VersionID, candidate.VersionID)
	require.Equal(t, seeded.ContentHash, candidate.ContentHash)
	require.Equal(t, 7, candidate.TokenEstimate, "七个 CJK 字符按 core v1 规则计 7 token")
	require.Equal(t, aclDecisionAllow, candidate.ACLDecision)
	require.Positive(t, candidate.RetrievalScore)
	require.Equal(t, 1, candidate.Rank)
	require.Zero(t, candidate.RerankScore, "重排未交付，重排分保持零值")
	require.NotContains(t, fmt.Sprint(record), "清理步骤见附件", "装配记录不得复制正文")
}

// TestRetrieval_EmptyResultStillRecordsAssembly 守"零命中也是证据"：检索成功但无
// 命中时仍写装配记录（空候选 + 空命中），调用方据此能证明查过而不是没查。
// 逆改点：只在有命中时写记录 → 记录条数断言红。
func TestRetrieval_EmptyResultStillRecordsAssembly(t *testing.T) {
	h := newRetrievalHarness(t)

	res, err := h.retriever.Retrieve(context.Background(), h.query("清理步骤"))
	require.NoError(t, err)
	require.Empty(t, res.Hits)
	require.Equal(t, StrategyHybrid, res.Strategy, "两路都成功参与，只是没有命中")
	require.Len(t, h.assembly.records, 1)
	require.Nil(t, h.assembly.records[0].Hits)
	require.Empty(t, h.assembly.records[0].Candidates)
}

// TestRetrieve_RejectsMissingRunID 守装配记录必须挂在 Run 上：缺 Run 标识在解析
// 范围之前拒绝，**不得**写一条没有归属的装配记录。
// 逆改点：允许空 RunID 继续 → ErrInvalidRequest 或端口调用次数断言红。
func TestRetrieve_RejectsMissingRunID(t *testing.T) {
	h := newRetrievalHarness(t)
	req := h.query("清理步骤")
	req.RunID = ""

	_, err := h.retriever.Retrieve(context.Background(), req)
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.Zero(t, h.authz.calls)
	require.Empty(t, h.assembly.records)
}

// TestRetrieve_RejectsEmptyQuery 守空查询拒绝：查询文本为空时不发检索调用。
// 逆改点：把空串当"匹配全部" → 端口调用次数断言红。
func TestRetrieve_RejectsEmptyQuery(t *testing.T) {
	h := newRetrievalHarness(t)
	req := h.query("   ")

	_, err := h.retriever.Retrieve(context.Background(), req)
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.Zero(t, h.keyword.path.calls)
	require.Zero(t, h.vectors.path.calls)
}

// TestSearch_RequiresCollectionsOrAgent 守契约的"不做全库检索"：集合与 Agent 都
// 缺省时 400，**不得**悄悄退化成全库扫描。
// 逆改点：缺省时放开成全库 → ErrInvalidRequest 断言红。
func TestSearch_RequiresCollectionsOrAgent(t *testing.T) {
	h := newRetrievalHarness(t)

	_, err := h.retriever.Search(context.Background(), SearchRequest{
		Principal: principalFor("team-a"),
		Query:     "清理步骤",
	})
	require.ErrorIs(t, err, ErrInvalidRequest)
	require.Zero(t, h.authz.calls)
}

// TestSearch_WritesNoAssemblyRecordAndNeverFabricatesRun 守管理面检索的边界：同一条
// 管线，但**不**写装配记录、**不**产生诊断事件（没有主锚点就不伪造 Run），降级只
// 通过返回值与 WARN 日志表达。
// 逆改点：给 Search 也补一个假 RunID 写事件 → 事件条数断言红。
func TestSearch_WritesNoAssemblyRecordAndNeverFabricatesRun(t *testing.T) {
	h := newRetrievalHarness(t)
	h.seedChunk(t, "public", "runbook.md", "清理步骤")
	h.vectors.path.err = errors.New("vector index offline")

	res, err := h.retriever.Search(context.Background(), SearchRequest{
		Principal:     principalFor("team-a"),
		AgentName:     h.agent,
		CollectionIDs: h.authz.ids(),
		Query:         "清理步骤",
		Embedding:     unitEmbedding(),
		Generation:    1,
	})
	require.NoError(t, err)
	require.Equal(t, StrategyKeyword, res.Strategy)
	require.Equal(t, string(DiagnosticReasonVectorUnavailable), res.DegradedReason)
	require.Len(t, res.Hits, 1)
	require.Empty(t, h.assembly.records)
	require.Empty(t, h.diag.events, "没有主锚点时不得写诊断事件")
	require.Contains(t, h.logs.String(), "vector_unavailable", "降级仍要留 WARN")
}

// TestRetrieve_RequiresKnowledgeReadScope 守身份闸门：没有 `knowledge:read` 的身份
// 在解析范围之前被拒，且不发出任何检索调用。
// 逆改点：只判 Valid() 不判 scope → ErrForbidden 断言红。
func TestRetrieve_RequiresKnowledgeReadScope(t *testing.T) {
	h := newRetrievalHarness(t)
	req := h.query("清理步骤")
	req.Principal.Scopes = []string{core.ScopeKnowledgeWrite}

	_, err := h.retriever.Retrieve(context.Background(), req)
	require.ErrorIs(t, err, core.ErrForbidden)
	require.Zero(t, h.authz.calls)
	require.Zero(t, h.keyword.path.calls)
	require.Contains(t, h.logs.String(), "forbidden")
}

// TestRetrieve_RejectsZeroPrincipal 守零值身份拒绝：不写半截轨迹、不产生假身份字段。
// 逆改点：跳过 Valid() 校验 → ErrUnauthorized 或日志断言红。
func TestRetrieve_RejectsZeroPrincipal(t *testing.T) {
	h := newRetrievalHarness(t)
	req := h.query("清理步骤")
	req.Principal = core.Principal{}

	_, err := h.retriever.Retrieve(context.Background(), req)
	require.ErrorIs(t, err, core.ErrUnauthorized)
	require.Zero(t, h.authz.calls)
	require.NotContains(t, h.logs.String(), "caller_id", "零值身份不得产生假身份字段")
}

// TestRetriever_FailsClosedWhenRerankConfigured 守"未交付的能力不静默跳过"：配置
// 要求重排时装配期就拒绝，而不是悄悄按不重排跑完、让读数看起来像重排过。
// 逆改点：忽略 RerankEnabled → ErrRetrieverUnavailable 断言红。
func TestRetriever_FailsClosedWhenRerankConfigured(t *testing.T) {
	h := newRetrievalHarness(t, withRetrievalConfig(RetrievalConfig{RerankEnabled: true}))

	_, err := h.retriever.Retrieve(context.Background(), h.query("清理步骤"))
	require.ErrorIs(t, err, ErrRetrieverUnavailable)
	require.Zero(t, h.authz.calls)
	require.Contains(t, h.logs.String(), "knowledge operation unavailable", "装配错误按 ERROR 口径记录")
}

// TestRetriever_RequiresEveryPort 守端口齐备：少一个端口即拒绝，**不得**用默认值
// 补出一个能跑但没有证据的检索器。
// 逆改点：去掉 ready() 的端口检查 → 缺诊断端口时不再报错，断言红。
func TestRetriever_RequiresEveryPort(t *testing.T) {
	retriever := NewRetriever(RetrieverDeps{
		Authz:       &fakeAuthz{},
		Keyword:     &fakeKeywordPath{},
		Vectors:     &fakeVectorPath{},
		Assembly:    &fakeAssemblyRecorder{},
		Logger:      testLogger(t),
		Now:         fixedClock(),
		Diagnostics: nil,
	})
	req := RetrieveRequest{
		Principal: principalFor("team-a"),
		RunID:     "run_a1",
		AgentName: "demo-agent",
		Query:     "清理步骤",
	}
	_, err := retriever.Retrieve(context.Background(), req)
	require.ErrorIs(t, err, ErrRetrieverUnavailable)

	var nilRetriever *Retriever
	_, err = nilRetriever.Retrieve(context.Background(), req)
	require.ErrorIs(t, err, ErrRetrieverUnavailable, "零值检索器同样拒绝，不得 panic")
}

// TestRetrieval_AuditLogCarriesRunID 守检索的日志面：成功日志带 `operation=retrieve`
// 与 `run_id`，且不冒充"有权威行"（读路径没有本组件写入的审计行，不应降级成 WARN）。
// 逆改点：给读路径套用 succeed 的"引用缺失即 WARN" → 日志级别断言红。
func TestRetrieval_AuditLogCarriesRunID(t *testing.T) {
	h := newRetrievalHarness(t)
	h.seedChunk(t, "public", "runbook.md", "清理步骤")
	req := h.query("清理步骤")
	req.CollectionIDs = []string{"public"}

	_, err := h.retriever.Retrieve(context.Background(), req)
	require.NoError(t, err)
	record := requireLogEntry(t, h.logs, "knowledge operation succeeded")
	require.Equal(t, "retrieve", record["operation"])
	require.Equal(t, h.runID, record["run_id"])
	require.Equal(t, "public", record["collection_id"])
	require.Equal(t, "succeeded", record["result"])
	require.NotContains(t, h.logs.String(), "without an audit record")
}

// revoke 在两次检索之间改掉集合的 ACL 标签，模拟一次真实撤权：集合仍在登记表里，
// 对 `project` 之外的调用方立刻不可见。
// revoke flips a collection's ACL between two retrievals, modelling a real revocation.
func (h *retrievalHarness) revoke(collection, project string) {
	for i := range h.authz.collections {
		if h.authz.collections[i].id == collection {
			h.authz.collections[i].acl = project
		}
	}
}

// T046 的两条注入面回归。设计 §4 只给出用例名与验收点（§3.2 "越权不进入 prompt"
// 同源），没有给出代码；本仓按 R54.4 的披露继续承载在 `retrieve_test.go`（设计文件
// 列里的 `assembly_test.go` 未拆分）。"注入面"沿用 R6 的 `promptFor` 口径：调用方
// 用检索结果拼出的模型上下文片段，用户 query 不计入。
//
// 这两条用例守的是"检索结果与注入面一致"这一条缝：`Retrieve` 是唯一允许拼进
// 上下文的出口，因此空结果与撤权后的结果必须**逐字节**表现为"什么都没检索到"。
//
// The two injection-surface regressions of T046. The design names them without code;
// they live here per the R54 disclosure. "Injection surface" is the promptFor block,
// the same convention as R6: retrieval hits as the model would see them.

// TestKnowledge_EmptyResultInjectsNothing 守 FR-006：检索为空时注入面必须为空，
// **不得**插入占位正文——"未找到相关资料"这类句子会把"查过但没有"伪装成"有内容"，
// 模型据此编造出处的风险比空上下文更高。
// 同一条用例带正对照：同一查询在语料非空时确实拼得出正文，否则"空"可能只是拼装
// 助手坏了（无判别力的空断言）。
// 逆改点：零命中时合成一条占位 Hit 或往上下文里埋提示文案 → `Empty` 断言红。
// TestKnowledge_EmptyResultInjectsNothing pins FR-006: an empty retrieval injects
// nothing, with a positive control from the same helper.
func TestKnowledge_EmptyResultInjectsNothing(t *testing.T) {
	h := newRetrievalHarness(t)

	empty, err := h.retriever.Retrieve(context.Background(), h.query("清理步骤"))
	require.NoError(t, err)
	require.Empty(t, h.promptFor(empty), "空结果不得注入任何正文，包括占位句")
	require.Empty(t, empty.Hits, "零命中不得合成占位命中")
	require.Zero(t, empty.DeniedCount, "没有可拒的元素时拒绝计数为 0，不得用计数冒充占位")

	// 正对照：语料非空时同一条查询必须拼得出正文，证明上面的"空"不是助手失灵。
	body := "清理步骤：先停写入，再删快照。"
	h.seedChunk(t, "public", "runbook.md", body)
	filled, err := h.retriever.Retrieve(context.Background(), h.query("清理步骤"))
	require.NoError(t, err)
	require.NotEmpty(t, filled.Hits)
	require.Contains(t, h.promptFor(filled), body)
}

// TestKnowledge_RevokedBodyNeverReappears 守 FR-004：撤权后，同一调用方的后续检索
// 与注入面都不得再出现该正文或来源路径；检索必须真的重跑，**不得**回放上一次的
// 命中（缓存会绕过撤权）。拒绝只以计数形式可见（C-08）。
// 逆改点：检索器缓存首次命中 / 两路改用请求集合而非解析后的范围 → 撤权后的
// `Empty` 断言红；把被拒来源写进诊断 attrs → 最后一组断言红。
// TestKnowledge_RevokedBodyNeverReappears pins FR-004: after revocation the body and
// its path never come back, and the retrieval really re-runs.
func TestKnowledge_RevokedBodyNeverReappears(t *testing.T) {
	h := newRetrievalHarness(t)
	const canary = "CANARY-REVOKED-4c11"
	body := canary + " 停产窗口内禁止切换主库。"
	h.seedChunk(t, "public", "ops/failover.md", body)

	first, err := h.retriever.Retrieve(context.Background(), h.query("切换主库"))
	require.NoError(t, err)
	require.Contains(t, h.promptFor(first), canary, "撤权之前必须真的检索得到（正对照）")

	// 撤权发生在两次查询之间：下一次查询必须立刻看不到正文与来源路径。
	h.revoke("public", "team-b")
	second, err := h.retriever.Retrieve(context.Background(), h.query("切换主库"))
	require.NoError(t, err)
	require.Empty(t, h.promptFor(second), "撤权后的正文与来源路径都不得再进入注入面")
	require.Empty(t, second.Hits)
	require.Positive(t, second.DeniedCount, "被拒元素只给计数")
	require.Equal(t, 2, h.keyword.path.calls, "撤权后必须重跑关键词路，不得回放缓存")
	require.Equal(t, 2, h.vectors.path.calls, "撤权后必须重跑向量路，不得回放缓存")

	// C-08：正文与来源路径不得经日志或诊断事件外泄。
	require.NotContains(t, h.logs.String(), canary)
	require.NotContains(t, h.logs.String(), "ops/failover.md")
	for _, event := range h.diag.List(h.runID) {
		require.NotContains(t, fmt.Sprint(event.Attrs), canary)
		require.NotContains(t, fmt.Sprint(event.Attrs), "ops/failover.md")
	}
}

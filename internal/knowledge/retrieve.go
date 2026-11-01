package knowledge

// 本文件实现混合检索的查询侧（US-16 §2.3 图 4 / §3.1.3 / T038）：解析可见范围 →
// 关键词路径 → 向量路径 → RRF 融合 → 预算裁剪 → 装配记录。三条硬约束：
//
//  1. **权限过滤在检索 SQL 内与装配前双重执行**（C-08）：被拒资料**不得**进入候选集、
//     模型上下文、缓存、日志标题或诊断 attrs；调用方**只能**看到计数。本文件是
//     装配前的那一半——两路检索端口按 `Scope` 过滤，返回的结果直接进融合，**没有**
//     "先拼上下文再过滤"的路径；
//  2. **一次查询固定单一 `index_generation`**（C-09）：代际由调用方给出并原样传给
//     向量端口，本文件**不**跨代合并；代际缺失或向量维度不符时向量路降级，
//     `degraded_reason` 让调用方看见（**不得**静默）；
//  3. **单路失败降级、两路皆失败才失败**：降级写诊断事件与 WARN 日志两条轨
//     （可观测性双轨），`Strategy` 与 `DegradedReason` 同时回给调用方。
//
// 与图 4 伪代码的三处任务级差异（记入 tasks.md 附录 R48）：
//   - 图 4 只用 `scope.DeniedCount` 表达拒绝计数；本实现另让两条检索端口各自回报
//     被 SQL 剔除的行数，总数取 `scope + max(关键词, 向量)`——SQL 内的过滤否则不可见，
//     而两路相加会把同一条被拒片段数两遍；
//   - 图 4 的重排步骤本 US 不交付（D-20 未列入最低集）：`retrieval.rerank.enabled=true`
//     在 `ready()` 就 fail-closed，**不得**静默跳过；
//   - 图 4 的 `IDsOnly` 由 `core.AssemblyCandidate` 承载：只填标识、版本、内容哈希、
//     token 估算、检索分、名次与 ACL 决定，**不**复制正文。
//
// This file implements the retrieval side of figure 4: scope resolution, keyword and
// vector paths, RRF fusion, budget fitting and the assembly record. Permission
// filtering happens inside the search ports and again before assembly; a denied chunk
// never reaches the candidate list, the prompt, the log title or the diagnostic attrs.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

// 检索默认值取自设计 §3.3 的默认列（`retrieval.top_k`=8 / `candidate_k`=40 /
// `rrf_k`=60）。配置装载层只拒负数、**不**填默认值，因此由消费侧按文档默认值补齐；
// 这是实现文档默认值，不是实现自造默认。
// Defaults come from the design's default column; the loader only rejects negatives, so
// the consumer applies the documented defaults.
const (
	// defaultTopK 是最终命中数（`retrieval.top_k` 的默认值）。
	defaultTopK = 8
	// defaultCandidateK 是单路候选数（`retrieval.candidate_k` 的默认值）。
	defaultCandidateK = 40
	// defaultRRFK 是 RRF 平滑参数（`retrieval.rrf_k` 的默认值）。
	defaultRRFK = 60
	// aclDecisionAllow 是装配候选中 ACL 决定的取值：被拒候选**根本不进**候选集，
	// 因此这里只可能出现 allow（`context_assembly_records.candidates` 的字段口径）。
	aclDecisionAllow = "allow"
)

// Strategy 是检索策略的稳定取值，逐字取自冻结契约 `SearchResult.strategy` 的四值。
// 本 US 只可能产出前三值：`hybrid_rerank` 待重排交付后才可达。
// Strategy is the contract's four-value enumeration; this US can only reach the first
// three because reranking is not delivered yet.
type Strategy string

const (
	// StrategyKeyword 表示只有关键词路产出。
	StrategyKeyword Strategy = "keyword"
	// StrategyVector 表示只有向量路产出。
	StrategyVector Strategy = "vector"
	// StrategyHybrid 表示两路都参与（RRF 融合）。
	StrategyHybrid Strategy = "hybrid"
	// StrategyHybridRerank 表示融合后经重排；本 US 不可达（重排未交付）。
	StrategyHybridRerank Strategy = "hybrid_rerank"
)

// Valid 判定策略是否落在契约闭集内。
// Valid reports whether the strategy is one of the contract values.
func (s Strategy) Valid() bool {
	switch s {
	case StrategyKeyword, StrategyVector, StrategyHybrid, StrategyHybridRerank:
		return true
	default:
		return false
	}
}

// Scope 是解析后的可见范围：**已剔除**项目不匹配、无 ACL、删除与过期的集合。
// `DeniedCount` 只给计数——被拒集合的标识**不得**出现在返回、日志标题或诊断 attrs。
// Scope is the resolved visibility. DeniedCount is a bare count: identifiers of
// rejected collections never leave the Authz implementation.
type Scope struct {
	// Collections 是本次检索可用的集合标识；空集表示没有可见集合。
	Collections []string
	// Agent 是解析时使用的 Agent 名（未给出时为空）。
	Agent string
	// DeniedCount 是解析阶段因 ACL / 项目 / 删除被剔除的候选数（只计数）。
	DeniedCount int
}

// Hit 是一条检索命中（契约 `SearchHit` 的领域形状 + 装配记录所需的字段）。
// 契约视图**刻意不含** `content_hash`：由 HTTP 映射层（T042）决定投影，本类型保留
// 它是为了 `context_assembly_records.candidates` 的字段口径（§3.4.1 要求候选带
// 内容哈希）；`ResolveCitations` 仍然**不接受** `Hit`，引用只能来自精确读取。
// Hit is one retrieval hit. The contract view deliberately omits ContentHash; it stays
// here because the assembly record's candidates require it, and ResolveCitations still
// refuses a Hit — citations come from exact reads only.
type Hit struct {
	// ChunkID 是片段标识；空值会被丢弃并记 WARN（端口违约，不是正常结果）。
	ChunkID string
	// DocumentID 是片段所属文档。
	DocumentID string
	// VersionID 是片段所属版本。
	VersionID string
	// CollectionID 是片段所属集合。
	CollectionID string
	// SourcePath 是允许根内的相对路径；日志里**不得**当作标题扩散（C-08）。
	SourcePath string
	// HeadingPath 是标题路径（"A > B > C"）。
	HeadingPath string
	// ContentHash 是该版本原始内容的 sha256（装配记录用，不进契约视图）。
	ContentHash string
	// Text 是片段正文；只进模型上下文，**不得**写进日志或诊断。
	Text string
	// Rank 是融合后的名次，从 1 起（由本文件赋值，端口返回的顺序即名次来源）。
	Rank int
	// Score 是融合分数；不同路径的异质分数不直接相加。
	Score float64
	// TokenEstimate 是该片段的 token 估算（与 core v1 同规则）。
	TokenEstimate int
}

// RetrieveRequest 是一次进提示词的检索请求。`Embedding` 由调用方在调用前算好
// （embedding 客户端装配在 app 层）；`Generation` 是本次查询固定的索引代际（C-09）。
// RetrieveRequest is one prompt-bound retrieval. The caller embeds the query first and
// pins the index generation; this package never computes an embedding on its own.
type RetrieveRequest struct {
	// Principal 是发起检索的调用方身份；零值拒绝。
	Principal core.Principal
	// RunID 是装配记录的锚点；**必须**非空（装配记录挂在 Run 上）。
	RunID string
	// StepID 是同一 Run 内的步骤标识，可空。
	StepID string
	// AgentName 是 Agent 名，供 `Authz` 解析其集合声明。
	AgentName string
	// CollectionIDs 是显式集合范围；与 `AgentName` 至少给一个（不做全库检索）。
	CollectionIDs []string
	// Query 是查询文本；空串拒绝。
	Query string
	// Embedding 是查询向量；缺失或维度不符时向量路降级。
	Embedding []float32
	// Generation 是本次查询固定的索引代际；小于 1 时向量路降级。
	Generation int
	// CandidateK 是单路候选数；<=0 时取配置，配置也为 0 时取文档默认值。
	CandidateK int
	// TopK 是最终命中数；<=0 时取配置，配置也为 0 时取文档默认值。
	TopK int
	// Budget 是结果切片的 token 预算上限；<=0 表示不裁剪。
	Budget int
}

// RetrieveResult 是检索结果：命中、拒绝计数与本次实际使用的策略。
// RetrieveResult carries the hits, the denial count and the strategy that produced them.
type RetrieveResult struct {
	// Hits 是按融合名次排好的最终命中。
	Hits []Hit
	// DeniedCount 是因 ACL / 删除 / 项目不匹配被剔除的候选数（只给计数）。
	DeniedCount int
	// Strategy 是本次实际使用的策略。
	Strategy Strategy
	// DegradedReason 非空表示单路降级，取值见诊断原因码（`vector_unavailable` 等）。
	DegradedReason string
	// Generation 是本次查询使用的索引代际；向量路未参与时为 0。
	Generation int
}

// SearchRequest 是管理面检索查询（契约 `SearchRequest` 的领域形状）：与 `Retrieve`
// 共用同一管线，但不写装配记录、不带 Run 标识。
// SearchRequest is the management-facing query: same pipeline, no assembly record.
type SearchRequest struct {
	// Principal 是发起检索的调用方身份；零值拒绝。
	Principal core.Principal
	// AgentName 是按 Agent 集合声明解析范围时的 Agent 名。
	AgentName string
	// CollectionIDs 是显式集合范围；与 `AgentName` 至少给一个。
	CollectionIDs []string
	// Query 是查询文本；空串拒绝。
	Query string
	// Embedding 是查询向量；缺失或维度不符时向量路降级。
	Embedding []float32
	// Generation 是本次查询固定的索引代际；小于 1 时向量路降级。
	Generation int
	// CandidateK 是单路候选数；<=0 时取配置 / 文档默认值。
	CandidateK int
	// TopK 是最终命中数；<=0 时取配置 / 文档默认值。
	TopK int
	// Budget 是结果切片的 token 预算上限；<=0 表示不裁剪。
	Budget int
}

// SearchResult 是管理面检索结果，逐字段对应契约 `SearchResult`。
// SearchResult mirrors the contract's SearchResult.
type SearchResult struct {
	// Hits 是按融合名次排好的最终命中。
	Hits []Hit
	// DeniedCount 是因 ACL / 删除 / 项目不匹配被剔除的候选数。
	DeniedCount int
	// Strategy 是本次实际使用的策略。
	Strategy Strategy
	// DegradedReason 非空表示单路降级（契约只允许三个取值）。
	DegradedReason string
	// Generation 是本次查询使用的索引代际；向量路未参与时为 0。
	Generation int
}

// Authz 解析本次检索的可见范围（消费方接口，实现在调用方包内）。
// 实现**必须**在解析阶段剔除项目不匹配、无 ACL、已删除与过期的集合，并把剔除数
// 落到 `Scope.DeniedCount`——它只回计数，**不得**回标识。
// Authz resolves the visible scope. The implementation drops mismatched, unauthorised,
// deleted and expired collections and reports only the count.
type Authz interface {
	Resolve(ctx context.Context, p core.Principal, agent string, collections []string) (Scope, error)
}

// KeywordSearcher 是关键词路径（`tsvector` / `pg_trgm`，实现在 `storage/postgres`）。
// 过滤条件**必须**在 SQL 内生效；`denied` 是 SQL 剔除的行数，只作计数。
// KeywordSearcher is the keyword path; filtering happens inside the SQL and the denied
// count reports rows dropped there.
type KeywordSearcher interface {
	Search(ctx context.Context, scope Scope, query string, k int) (hits []Hit, denied int, err error)
}

// VectorSearcher 是向量路径（单代际精确扫描 / ANN，实现在 `storage/postgres`）。
// 过滤条件同样**必须**在 SQL 内生效，且只查调用方固定的 `generation`（C-09）。
// VectorSearcher is the vector path; it filters inside the SQL and reads one generation.
type VectorSearcher interface {
	Search(ctx context.Context, scope Scope, embedding []float32, generation, k int) (hits []Hit, denied int, err error)
}

// AssemblyRecorder 写上下文装配证据（`context_assembly_records`）。只存标识、名次、
// 分数、决定与原因码，**不**复制正文；记录标识与时间由写入器分配。
// AssemblyRecorder persists the assembly evidence; ids and timestamps are the writer's.
type AssemblyRecorder interface {
	Record(ctx context.Context, record core.AssemblyRecord)
}

// RetrievalConfig 是检索侧装配参数（配置键见设计 §3.3）。
// RetrievalConfig carries the retrieval assembly parameters.
type RetrievalConfig struct {
	// TopK 是最终命中数；<=0 时用文档默认值 8。
	TopK int
	// CandidateK 是单路候选数；<=0 时用文档默认值 40。
	CandidateK int
	// RRFK 是 RRF 平滑参数；<=0 时用文档默认值 60。
	RRFK int
	// RerankEnabled 为真时 `ready()` fail-closed：重排未交付，**不得**静默跳过。
	RerankEnabled bool
}

// RetrieverDeps 是检索器装配依赖。端口缺一即视为装配错误（`Retrieve` / `Search`
// 在解析范围之前返回 `ErrRetrieverUnavailable`）。
// RetrieverDeps carries the assembly dependencies; a missing port fails closed.
type RetrieverDeps struct {
	// Authz 是可见范围解析端口。
	Authz Authz
	// Keyword 是关键词路径端口。
	Keyword KeywordSearcher
	// Vectors 是向量路径端口。
	Vectors VectorSearcher
	// Assembly 是装配记录端口（`Retrieve` 必写，`Search` 不写）。
	Assembly AssemblyRecorder
	// Diagnostics 是诊断事件端口。
	Diagnostics DiagnosticRecorder
	// Config 是检索参数。
	Config RetrievalConfig
	// Logger 是运行日志目标；缺省用 `slog.Default()`（与 `Ingestor` 同口径）。
	Logger *slog.Logger
	// Now 是时钟；缺省 `time.Now`，测试注入固定时刻。
	Now func() time.Time
}

// Retriever 执行同步混合检索：两路并行**不**允许（原则 VII 禁异步编排），顺序调用。
// Retriever runs the two search paths sequentially; no async orchestration.
type Retriever struct {
	authz      Authz
	keyword    KeywordSearcher
	vectors    VectorSearcher
	assembly   AssemblyRecorder
	diag       DiagnosticRecorder
	operations *operationAudit
	cfg        RetrievalConfig
	logger     *slog.Logger
	now        func() time.Time
}

// NewRetriever 装配检索器。端口缺一不在构造期失败（签名无 error），由入口在解析
// 范围之前返回 `ErrRetrieverUnavailable`。
// NewRetriever assembles the retriever; a missing port fails closed at entry.
func NewRetriever(deps RetrieverDeps) *Retriever {
	retriever := &Retriever{
		authz:    deps.Authz,
		keyword:  deps.Keyword,
		vectors:  deps.Vectors,
		assembly: deps.Assembly,
		diag:     deps.Diagnostics,
		cfg:      deps.Config,
		logger:   deps.Logger,
		now:      deps.Now,
	}
	if retriever.logger == nil {
		retriever.logger = slog.Default()
	}
	if retriever.now == nil {
		retriever.now = time.Now
	}
	retriever.operations = newOperationAudit(retriever.logger, retriever.now)
	return retriever
}

// Retrieve 执行进提示词的混合检索：结果**已经**过权限过滤与预算裁剪，调用方可以
// 直接拼进模型上下文，**不得**再做一次"先拼后滤"。
// Retrieve returns hits that already passed permission filtering and budget fitting; the
// caller assembles them directly and never filters after the fact.
func (r *Retriever) Retrieve(ctx context.Context, req RetrieveRequest) (RetrieveResult, error) {
	trace, err := r.openTrace(ctx, opRetrieve, req.Principal, operationTargets{
		collectionID: singleCollection(req.CollectionIDs),
		runID:        strings.TrimSpace(req.RunID),
	})
	if err != nil {
		return RetrieveResult{}, err
	}
	if err := r.authorize(req.Principal); err != nil {
		trace.reject(ctx, err)
		return RetrieveResult{}, err
	}
	query := retrievalQuery{
		principal:   req.Principal,
		runID:       strings.TrimSpace(req.RunID),
		stepID:      strings.TrimSpace(req.StepID),
		agent:       strings.TrimSpace(req.AgentName),
		collections: normalizeCollections(req.CollectionIDs),
		text:        strings.TrimSpace(req.Query),
		embedding:   req.Embedding,
		generation:  req.Generation,
		candidateK:  req.CandidateK,
		topK:        req.TopK,
		budget:      req.Budget,
	}
	if query.runID == "" {
		err := fmt.Errorf("%w: 检索缺少 Run 标识——装配记录必须挂在 Run 上", ErrInvalidRequest)
		trace.reject(ctx, err)
		return RetrieveResult{}, err
	}
	if err := query.validate(); err != nil {
		trace.reject(ctx, err)
		return RetrieveResult{}, err
	}
	outcome, err := r.run(ctx, query)
	if err != nil {
		r.failTrace(ctx, trace, err)
		return RetrieveResult{}, err
	}
	// 装配记录是本操作的权威证据（`context_assembly_records`）；记录标识由写入器
	// 分配，因此这里拿不到 `audit_id`，成功日志走只读出口。
	r.assembly.Record(ctx, core.AssemblyRecord{
		RunID:            query.runID,
		StepID:           query.stepID,
		Purpose:          core.PurposeRetrieval,
		Agent:            query.agent,
		EstimatorVersion: core.EstimateVersion,
		Candidates:       assemblyCandidates(outcome.merged),
		Hits:             hitIDs(outcome.hits),
		Decisions:        outcome.decisions,
		ReasonCodes:      outcome.reasons,
	})
	trace.succeedRead(ctx)
	return RetrieveResult{
		Hits:           outcome.hits,
		DeniedCount:    outcome.deniedCount,
		Strategy:       outcome.strategy,
		DegradedReason: outcome.degraded,
		Generation:     outcome.generation,
	}, nil
}

// Search 执行管理面检索查询：与 `Retrieve` 同一条管线，但**不写**装配记录
// （管理面的查询不是 Run 的装配），也不产生诊断事件（没有主锚点，**不得**伪造 Run）。
// Search runs the same pipeline for the management API: no assembly record and no
// diagnostic events, because it has no primary correlation anchor.
func (r *Retriever) Search(ctx context.Context, req SearchRequest) (SearchResult, error) {
	trace, err := r.openTrace(ctx, opSearch, req.Principal, operationTargets{
		collectionID: singleCollection(req.CollectionIDs),
	})
	if err != nil {
		return SearchResult{}, err
	}
	if err := r.authorize(req.Principal); err != nil {
		trace.reject(ctx, err)
		return SearchResult{}, err
	}
	query := retrievalQuery{
		principal:   req.Principal,
		agent:       strings.TrimSpace(req.AgentName),
		collections: normalizeCollections(req.CollectionIDs),
		text:        strings.TrimSpace(req.Query),
		embedding:   req.Embedding,
		generation:  req.Generation,
		candidateK:  req.CandidateK,
		topK:        req.TopK,
		budget:      req.Budget,
	}
	if err := query.validate(); err != nil {
		trace.reject(ctx, err)
		return SearchResult{}, err
	}
	outcome, err := r.run(ctx, query)
	if err != nil {
		r.failTrace(ctx, trace, err)
		return SearchResult{}, err
	}
	trace.succeedRead(ctx)
	return SearchResult{
		Hits:           outcome.hits,
		DeniedCount:    outcome.deniedCount,
		Strategy:       outcome.strategy,
		DegradedReason: outcome.degraded,
		Generation:     outcome.generation,
	}, nil
}

// retrievalQuery 是两条入口共用的管线入参（`Search` 只是没有 Run 与装配记录）。
// retrievalQuery is the shared pipeline input; Search simply has no run or record.
type retrievalQuery struct {
	principal   core.Principal
	runID       string
	stepID      string
	agent       string
	collections []string
	text        string
	embedding   []float32
	generation  int
	candidateK  int
	topK        int
	budget      int
}

// validate 在解析范围与任何检索调用之前拒绝形状非法的请求：空查询、以及
// "既没有集合也没有 Agent"——后者按冻结契约**不做**全库检索。
// validate rejects malformed requests before any search call; the contract forbids a
// full-library search, so at least one of collections and agent must be present.
func (q retrievalQuery) validate() error {
	if q.text == "" {
		return fmt.Errorf("%w: 检索缺少查询文本", ErrInvalidRequest)
	}
	if len(q.collections) == 0 && q.agent == "" {
		return fmt.Errorf("%w: 检索必须给出集合或 Agent——不做全库检索", ErrInvalidRequest)
	}
	return nil
}

// retrievalOutcome 是管线的中间结果：调用方决定写不写装配记录。
// retrievalOutcome is the pipeline result; the caller decides whether to record it.
type retrievalOutcome struct {
	scope       Scope
	merged      []Hit
	hits        []Hit
	deniedCount int
	strategy    Strategy
	degraded    string
	reasons     []string
	decisions   []string
	generation  int
}

// run 是图 4 的 [1]–[6]：解析范围 → 两路检索 → RRF 融合 → 预算裁剪。
// 单路失败降级并留痕；两路皆失败才上抛（图 4 的"两路皆失败才失败"）。
// run mirrors figure 4 step 1-6; one failing path degrades, both failing is an error.
func (r *Retriever) run(ctx context.Context, q retrievalQuery) (retrievalOutcome, error) {
	scope, err := r.authz.Resolve(ctx, q.principal, q.agent, q.collections)
	if err != nil {
		return retrievalOutcome{}, err
	}
	candidateK := r.candidateK(q.candidateK)
	outcome := retrievalOutcome{scope: scope}

	keywordHits, keywordDenied, keywordErr := r.keyword.Search(ctx, scope, q.text, candidateK)
	keywordOK := keywordErr == nil
	if !keywordOK {
		r.degrade(ctx, q, DiagnosticReasonKeywordUnavailable, "keyword search path unavailable", keywordErr, nil)
		outcome.degraded = string(DiagnosticReasonKeywordUnavailable)
		outcome.reasons = append(outcome.reasons, string(DiagnosticReasonKeywordUnavailable))
		outcome.decisions = append(outcome.decisions, string(DiagnosticDecisionDegrade))
	}

	vectorHits, vectorDenied, vectorErr := r.searchVectors(ctx, scope, q, candidateK)
	vectorOK := vectorErr == nil
	if !vectorOK {
		r.degrade(ctx, q, DiagnosticReasonVectorUnavailable, "vector search path unavailable", vectorErr, nil)
		outcome.degraded = string(DiagnosticReasonVectorUnavailable)
		outcome.reasons = append(outcome.reasons, string(DiagnosticReasonVectorUnavailable))
		outcome.decisions = append(outcome.decisions, string(DiagnosticDecisionDegrade))
	}

	if !keywordOK && !vectorOK {
		return retrievalOutcome{}, fmt.Errorf("知识检索两路均不可用: %w", errors.Join(keywordErr, vectorErr))
	}

	switch {
	case keywordOK && vectorOK:
		outcome.strategy = StrategyHybrid
	case keywordOK:
		outcome.strategy = StrategyKeyword
	default:
		outcome.strategy = StrategyVector
	}
	if vectorOK {
		outcome.generation = q.generation
	}

	outcome.deniedCount = scope.DeniedCount + max(keywordDenied, vectorDenied)
	if outcome.deniedCount > 0 {
		// 被拒元素**只**允许计数：标题、标识与片段都不得进入诊断 attrs（C-08）。
		r.recordEvent(ctx, q, DiagnosticReasonPermissionDenied, DiagnosticDecisionDeny,
			map[string]any{"count": outcome.deniedCount})
	}

	outcome.merged = mergeRRF(keywordHits, vectorHits, r.rrfK())
	kept, dropped := fitBudget(outcome.merged, r.topK(q.topK), q.budget)
	outcome.hits = kept
	if dropped > 0 {
		r.recordEvent(ctx, q, DiagnosticReasonBudgetTrimmed, DiagnosticDecisionTrim,
			map[string]any{"count": dropped})
		outcome.reasons = append(outcome.reasons, string(DiagnosticReasonBudgetTrimmed))
		outcome.decisions = append(outcome.decisions, string(DiagnosticDecisionTrim))
	}
	return outcome, nil
}

// searchVectors 在向量路具备条件时发起检索：调用方没给向量、维度不符或代际缺失
// 都按"向量不可用"处理并**如实**降级——**不得**在这里补一个零向量把调用发出去。
// searchVectors runs the vector path only when the query actually has a usable vector;
// it never substitutes a zero vector to keep the call going.
func (r *Retriever) searchVectors(ctx context.Context, scope Scope, q retrievalQuery, k int) ([]Hit, int, error) {
	switch {
	case q.generation < 1:
		return nil, 0, fmt.Errorf("索引代际未给出，向量路跳过")
	case len(q.embedding) == 0:
		return nil, 0, fmt.Errorf("查询向量缺失，向量路跳过")
	case len(q.embedding) != VectorDimensions:
		return nil, 0, fmt.Errorf("查询向量维度 %d 与冻结维度 %d 不符，向量路跳过", len(q.embedding), VectorDimensions)
	}
	return r.vectors.Search(ctx, scope, q.embedding, q.generation, k)
}

// degrade 写降级的两条轨：WARN 日志（排障要看得到）与诊断事件（有锚点时）。
// degrade writes both tracks: a WARN log and, when an anchor exists, a diagnostic event.
func (r *Retriever) degrade(ctx context.Context, q retrievalQuery, reason DiagnosticReason,
	message string, cause error, attrs map[string]any,
) {
	r.logger.WarnContext(ctx, message, "reason_code", string(reason), "error", cause)
	r.recordEvent(ctx, q, reason, DiagnosticDecisionDegrade, attrs)
}

// recordEvent 写一条检索诊断事件。没有 Run 锚点（管理面检索）时**不**伪造锚点：
// 只保留 WARN 日志，降级由 `DegradedReason` 回给调用方。事件标识、契约版本、
// 事件种类与关联结构由 `app` 适配器补齐（`knowledge` 不 import 诊断包，C-06）。
// recordEvent writes one retrieval event; without a run anchor it only logs, never
// inventing a correlation the caller did not supply. The adapter fills the
// writer-owned identifier, schema version, kind and correlation.
func (r *Retriever) recordEvent(ctx context.Context, q retrievalQuery, reason DiagnosticReason,
	decision DiagnosticDecision, attrs map[string]any,
) {
	if q.runID == "" {
		return
	}
	r.diag.Record(ctx, DiagnosticEvent{
		OccurredAt: r.startedAt(),
		RunID:      q.runID,
		StepID:     q.stepID,
		Actor:      q.principal.Actor,
		Reason:     reason,
		Decision:   decision,
		Attrs:      attrs,
	})
}

// failTrace 按错误类型分流留痕：身份类拒绝记 reject（动作没有发出），其余记 fail。
// failTrace records identity rejections as rejections and everything else as failures.
func (r *Retriever) failTrace(ctx context.Context, trace *operationTrace, err error) {
	if errors.Is(err, core.ErrUnauthorized) || errors.Is(err, core.ErrForbidden) ||
		errors.Is(err, ErrInvalidRequest) {
		trace.reject(ctx, err)
		return
	}
	trace.fail(ctx, auditRef{}, "", "", err, nil)
}

// openTrace 统一两条入口的准入顺序：装配 → 身份 → 审计轨迹（与 `Ingestor` 同构）。
// openTrace orders admission as assembly, identity, then the audit trace.
func (r *Retriever) openTrace(ctx context.Context, kind operationKind, p core.Principal,
	targets operationTargets,
) (*operationTrace, error) {
	started := r.startedAt()
	if err := r.ready(); err != nil {
		r.rejectAdmission(ctx, kind, p, err, targets)
		return nil, err
	}
	trace, err := r.operations.begin(p, kind, started)
	if err != nil {
		r.rejectAdmission(ctx, kind, p, err, targets)
		return nil, err
	}
	trace.scope(targets)
	return trace, nil
}

// rejectAdmission 记录没有轨迹的准入拒绝；零值 `Retriever` 没有日志面。
// rejectAdmission logs a rejection that never reached a trace.
func (r *Retriever) rejectAdmission(ctx context.Context, kind operationKind, p core.Principal,
	cause error, targets operationTargets,
) {
	if r == nil {
		return
	}
	r.operations.rejected(ctx, kind, p, cause, targets)
}

// startedAt 取本次操作的起点；零值 `Retriever` 回落墙上时钟。
// startedAt reads the operation start; the wall clock keeps durations meaningful.
func (r *Retriever) startedAt() time.Time {
	if r == nil || r.now == nil {
		return time.Now()
	}
	return r.now()
}

// ready 判定装配是否可用：端口齐备、重排未启用、配置非负。任一不满足即拒绝，
// **不得**用默认值补齐缺端口。
// ready fails closed on a missing port, an enabled-but-undelivered rerank, or a
// negative configuration.
func (r *Retriever) ready() error {
	if r == nil || r.authz == nil || r.keyword == nil || r.vectors == nil ||
		r.assembly == nil || r.diag == nil {
		return ErrRetrieverUnavailable
	}
	switch {
	case r.cfg.RerankEnabled:
		return fmt.Errorf("%w: retrieval.rerank.enabled=true——重排未交付，不得静默跳过", ErrRetrieverUnavailable)
	case r.cfg.TopK < 0:
		return fmt.Errorf("%w: retrieval.top_k 不得为负", ErrRetrieverUnavailable)
	case r.cfg.CandidateK < 0:
		return fmt.Errorf("%w: retrieval.candidate_k 不得为负", ErrRetrieverUnavailable)
	case r.cfg.RRFK < 0:
		return fmt.Errorf("%w: retrieval.rrf_k 不得为负", ErrRetrieverUnavailable)
	}
	return nil
}

// authorize 是检索路径的身份校验：零值 / 无读 scope 的身份在解析范围之前拒绝。
// authorize rejects a zero-value or read-scope-less principal before scope resolution.
func (r *Retriever) authorize(p core.Principal) error {
	if !p.Valid() {
		return core.ErrUnauthorized
	}
	if !p.HasScope(core.ScopeKnowledgeRead) {
		return core.ErrForbidden
	}
	return nil
}

// topK 解析最终命中数：请求 → 配置 → 文档默认值。
// topK resolves the final width: request, then config, then the documented default.
func (r *Retriever) topK(requested int) int {
	if requested > 0 {
		return requested
	}
	if r.cfg.TopK > 0 {
		return r.cfg.TopK
	}
	return defaultTopK
}

// candidateK 解析单路候选数，并保证不小于最终命中数（候选少于命中是自相矛盾的配置）。
// candidateK resolves the per-path width and never returns less than topK.
func (r *Retriever) candidateK(requested int) int {
	candidate := requested
	if candidate <= 0 {
		candidate = r.cfg.CandidateK
	}
	if candidate <= 0 {
		candidate = defaultCandidateK
	}
	return max(candidate, r.topK(0))
}

// rrfK 解析 RRF 平滑参数：配置 → 文档默认值（0 会让融合分退化成 1/名次）。
// rrfK resolves the fusion constant; zero would degrade the score to 1/rank.
func (r *Retriever) rrfK() int {
	if r.cfg.RRFK > 0 {
		return r.cfg.RRFK
	}
	return defaultRRFK
}

// normalizeCollections 去掉空串与首尾空白：空集合标识进 SQL 会静默匹配不到任何行。
// normalizeCollections drops blanks so an empty id can never silently match nothing.
func normalizeCollections(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// singleCollection 只在恰好一个集合时返回它（审计日志的 `collection_id` 字段）：
// 多集合时**不得**随便挑一个冒充"本次检索的集合"。
// singleCollection returns the id only when exactly one was requested.
func singleCollection(ids []string) string {
	normalized := normalizeCollections(ids)
	if len(normalized) == 1 {
		return normalized[0]
	}
	return ""
}

// mergeRRF 按名次融合两路结果（不相加异质分数）：同一片段在两路出现时分数相加。
// 名次相同时按片段标识排序，保证同输入同读数（map 遍历顺序**不得**泄漏进结果）。
// mergeRRF fuses the two paths by rank; equal scores break by chunk id so the result is
// deterministic and never depends on map iteration order.
func mergeRRF(keyword, vectors []Hit, k int) []Hit {
	type entry struct {
		hit   Hit
		score float64
	}
	order := make([]string, 0, len(keyword)+len(vectors))
	merged := make(map[string]*entry, len(keyword)+len(vectors))
	add := func(hits []Hit) {
		for i, hit := range hits {
			if hit.ChunkID == "" {
				continue
			}
			score := 1.0 / float64(k+i+1)
			if existing, ok := merged[hit.ChunkID]; ok {
				existing.score += score
				fillMissing(&existing.hit, hit)
				continue
			}
			copied := hit
			merged[hit.ChunkID] = &entry{hit: copied, score: score}
			order = append(order, hit.ChunkID)
		}
	}
	add(keyword)
	add(vectors)

	out := make([]Hit, 0, len(order))
	for _, id := range order {
		item := merged[id]
		hit := item.hit
		hit.Score = item.score
		hit.TokenEstimate = estimateHitTokens(hit.Text)
		out = append(out, hit)
	}
	slices.SortStableFunc(out, func(a, b Hit) int {
		if byScore := cmp.Compare(b.Score, a.Score); byScore != 0 {
			return byScore
		}
		return strings.Compare(a.ChunkID, b.ChunkID)
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

// fillMissing 用后到的同片段结果补齐空字段：两路取自同一行，正常情况下不会有差异，
// 但端口实现不同（关键词取 `tsv`、向量取 `embedding`）时必须有一处兜底。
// fillMissing fills blank fields from the later duplicate; both paths read the same row,
// yet a port may return fewer columns.
func fillMissing(base *Hit, next Hit) {
	if base.DocumentID == "" {
		base.DocumentID = next.DocumentID
	}
	if base.VersionID == "" {
		base.VersionID = next.VersionID
	}
	if base.CollectionID == "" {
		base.CollectionID = next.CollectionID
	}
	if base.SourcePath == "" {
		base.SourcePath = next.SourcePath
	}
	if base.HeadingPath == "" {
		base.HeadingPath = next.HeadingPath
	}
	if base.ContentHash == "" {
		base.ContentHash = next.ContentHash
	}
	if base.Text == "" {
		base.Text = next.Text
	}
}

// fitBudget 截到 `topK` 并按 token 预算裁剪：超预算的片段被跳过（不是截断正文——
// 半截片段不得进上下文），跳过的条数即 `dropped`。`budget <= 0` 表示不裁剪。
// fitBudget caps at topK and skips hits that no longer fit the token budget; a chunk is
// never truncated, it is dropped whole.
func fitBudget(hits []Hit, topK, budget int) (kept []Hit, dropped int) {
	limit := min(topK, len(hits))
	kept = make([]Hit, 0, limit)
	used := 0
	for i := range limit {
		hit := hits[i]
		if budget > 0 && used+hit.TokenEstimate > budget {
			dropped++
			continue
		}
		used += hit.TokenEstimate
		hit.Rank = len(kept) + 1
		kept = append(kept, hit)
	}
	return kept, dropped
}

// assemblyCandidates 把融合候选转成装配记录的候选：只留标识、版本、内容哈希、
// token 估算、检索分、名次与 ACL 决定，**不**复制正文。
// assemblyCandidates projects merged hits into the record: identifiers, scores and the
// ACL decision only, never the text.
func assemblyCandidates(hits []Hit) []core.AssemblyCandidate {
	if len(hits) == 0 {
		return nil
	}
	out := make([]core.AssemblyCandidate, 0, len(hits))
	for _, hit := range hits {
		out = append(out, core.AssemblyCandidate{
			ChunkID:        hit.ChunkID,
			VersionID:      hit.VersionID,
			ContentHash:    hit.ContentHash,
			TokenEstimate:  hit.TokenEstimate,
			RetrievalScore: hit.Score,
			Rank:           hit.Rank,
			ACLDecision:    aclDecisionAllow,
		})
	}
	return out
}

// hitIDs 取最终命中的片段标识（装配记录的 `hits` 只存标识）。
// hitIDs keeps only the chunk ids of the final hits.
func hitIDs(hits []Hit) []string {
	if len(hits) == 0 {
		return nil
	}
	out := make([]string, 0, len(hits))
	for _, hit := range hits {
		out = append(out, hit.ChunkID)
	}
	return out
}

// estimateHitTokens 按 core v1 的同一规则估算片段 Token（CJK 类 1 token/字，其余
// 4 字节/token 向上取整）。`core` 的估算器未导出，这里镜像实现——**改规则必须
// 两处同步**，否则装配记录的 `token_estimate` 与预算口径各说各话。
// estimateHitTokens mirrors core's v1 estimator (one token per CJK rune, four bytes per
// token otherwise). The rule lives in two places and must change in both.
func estimateHitTokens(text string) int {
	if text == "" {
		return 0
	}
	wide, narrow := 0, 0
	for _, r := range text {
		if isWideRune(r) {
			wide++
			continue
		}
		narrow += utf8.RuneLen(r)
	}
	return wide + (narrow+3)/4
}

// isWideRune 判定 CJK 类字符（中文 / 日文假名 / 韩文），与 core 的判定同源。
// isWideRune reports CJK-class runes, matching core's rule.
func isWideRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

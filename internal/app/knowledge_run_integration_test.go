//go:build integration

package app

// 本文件是 V-6 的 **Run 级端到端**用例（2026-10-30 用户裁决）：材料入库 → 真实
// `AgentService.Run` → 运行级检索 → 上下文注入 → 引用复核 → 装配记录与诊断落库。
//
// 判据面与"只调 Retriever"的用例严格区分：
//   - 唯一允许替换的是**外部世界**（embedding 提供方、摄取配额、embedding 审计）；
//     文档读取、摄取器、存储、授权、检索器、精确读取、引用解析、ReAct 循环、会话与
//     运行账本全部是生产实现；
//   - 断言打在**生产路径的产物**上：发给模型协议客户端的系统消息、`context_assembly_records`
//     行、`diagnostic_events` 行、以及 Run 终态；
//   - **"可定位引用" ≠ "回答受原文支持"**：本文件只判计数与落库形状（Supported /
//     Unresolved），正文一致性由 `internal/knowledge` 的引用词法用例判，不在这里重复。
//
// 六个场景覆盖用户裁决点名的四类边界：正常链路（T1）、引用不可解析（T2）、无权限（T3）、
// 材料删除 / 撤权（T4a / T4b）、轮内撤权收窄（T5）、检索失败（T6）。
//
// This is the run-level end-to-end case for V-6: ingest → real AgentService.Run →
// retrieval → injection → citation review → persisted evidence. Only the outside world
// is faked; every assertion reads a production artifact.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

const (
	// knRunE2ECollectionName 是 Agent 在 `knowledge:` 里声明的集合名（投影行的 ref_name）。
	knRunE2ECollectionName = "policy-docs"
	// knRunE2ECollectionID 是投影行指向的稳定集合 ID（`col_`/`kb` 前缀不参与判定）。
	knRunE2ECollectionID = "kbcol_run_e2e"
	// knRunE2EDocPath 是工作区允许根内的相对路径。
	knRunE2EDocPath = "policies/refund.md"
	// knRunE2EQuery 同时是用户消息与运行级检索的查询词（关键词路按子串匹配）。
	knRunE2EQuery = "退款政策"
	// knRunE2EModel 是 embedding 模型名；必须与代际行、摄取配置三处一致。
	knRunE2EModel = "run-e2e-embed"
	// knRunE2EGeneration 是启动期固定的索引代际。
	knRunE2EGeneration = 1
)

const (
	// knRunE2EDocBody 是入库材料正文；答案必须能在这一段里逐字找到依据。
	knRunE2EDocBody = "# 退款政策\n\n退款政策：签收后七天内可无理由退款。\n"
	// knRunE2EAnswerCited 按注入面给出的标记引用 c1。
	knRunE2EAnswerCited = "退款政策：签收后七天内可无理由退款（来源 c1）。"
	// knRunE2EAnswerStray 引用一个从未进入上下文的标记：它是"可定位性"的负例，
	// 不证明回答受原文支持（见文件头）。
	knRunE2EAnswerStray = "退款政策见（来源 c9）。"
	// knRunE2EHeading 是注入面标题（`core.KnowledgeInjection.Render` 的首行前缀）。
	knRunE2EHeading = "## 知识库检索结果"
)

// errKnRunE2ESearchDown 注入"两路检索均不可用"（存储违约，不是权限拒绝）。
var errKnRunE2ESearchDown = errors.New("kn run e2e: injected search path failure")

// knRunE2EState 在装配期捕获测试要操作的组件：摄取器（材料入库与删除）与间谍端口。
type knRunE2EState struct {
	events          *diagnostics.Recorder
	retrievalConfig knowledge.RetrievalConfig
	spy             *knRunSpy
	ingestor        *knowledge.Ingestor
	documentID      string
}

// knRunE2EOptions 是按用例定制的装配增量。
type knRunE2EOptions struct {
	Observability   bool
	RetrievalConfig knowledge.RetrievalConfig
	// ExtraBindings 仅为OBS混合过滤夹具追加声明，既有场景默认不变。
	ExtraBindings []core.KnowledgeBinding
	// WithReadScope 追加 `knowledge:read`（缺省 false = 只有 `sessions:write`）。
	WithReadScope bool
	// FailSearch 让关键词与向量两条路径都失败，走"存储违约 → 拒绝启动"分支。
	FailSearch bool
	// WithAssembly 接上对话装配记录写入器（V-7 用例专用）：默认关闭，既有用例的
	// "该 Run 恰一条 retrieval 行"断言保持不变。
	WithAssembly bool
}

// knRunE2E 是 Run 级知识用例的对象图：共享准入夹具 + 真实知识链路 + 间谍端口。
type knRunE2E struct {
	*runHarness
	state *knRunE2EState
}

// newKnRunE2E 组装夹具：Agent 声明 `knowledge: [policy-docs]`，Run 侧端口经 `Wire` 接
// **真实**检索器 / 精确读取 / 引用复核，外面套一层只观察不改判的间谍。
func newKnRunE2E(t *testing.T, opts knRunE2EOptions) *knRunE2E {
	t.Helper()

	state := &knRunE2EState{spy: &knRunSpy{}, retrievalConfig: opts.RetrievalConfig}
	var scopes []string
	if opts.WithReadScope {
		scopes = []string{core.ScopeKnowledgeRead}
	}
	options := runHarnessOptions{
		Observability:     opts.Observability,
		ObservationWiring: func(events *diagnostics.Recorder) { state.events = events },
		Knowledge: append([]core.KnowledgeBinding{{
			Name:   knRunE2ECollectionName,
			Access: core.KnowledgeAccessRead,
		}}, opts.ExtraBindings...),
		ExtraScopes: scopes,
		Message:     knRunE2EQuery,
		Wire: func(t *testing.T, db *postgres.DB, agents postgres.KnowledgeAgentSource) core.KnowledgeRunSource {
			state.spy.inner = knRunE2EWire(t, db, agents, state, opts.FailSearch)
			return state.spy
		},
	}
	if opts.WithAssembly {
		// V-7：对话装配记录与检索装配共用同一个写入器（与生产 `wire.go` 同形的接线）。
		// 只有显式开启的用例才看见 chat 行；其余用例的"恰一条 retrieval 行"断言不变。
		options.Assembly = func(t *testing.T, db *postgres.DB) core.AssemblyRecorder {
			t.Helper()
			return postgres.NewAssemblyStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
		}
	}
	h := newRunHarnessWith(t, options)
	return &knRunE2E{runHarness: h, state: state}
}

// knRunE2EWire 把生产装配（`wire_knowledge.go` 的同一组构造器）接到夹具库上。
//
// 与生产装配的差异只有两处，且都属外部世界：embedding 提供方是确定性假件、配额与
// embedding 审计是假件。索引代际 seed 成 `building`——摄取只写向量，代际切换归代际
// 管理器（本用例不判 C-09 的证据门）。
func knRunE2EWire(t *testing.T, db *postgres.DB, agents postgres.KnowledgeAgentSource,
	state *knRunE2EState, failSearch bool,
) core.KnowledgeRunSource {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC()

	// 代际 1：status=building（`ActivateGeneration` 对已 active 的代际报状态冲突）。
	_, err := db.ExecContext(ctx, `
		INSERT INTO kb_index_generations
			(generation, embedding_model, dimensions, distance, status, activated_at, retired_at, created_at)
		VALUES ($1, $2, $3, 'cosine', 'building', NULL, NULL, $4)`,
		knRunE2EGeneration, knRunE2EModel, knowledge.VectorDimensions, now.UnixMilli())
	require.NoError(t, err, "seed 索引代际")

	// 集合：project 为空 = 不限项目（调用方 `SetLimit` 补的 callers 行 project 也是空串）。
	_, err = db.ExecContext(ctx, `
		INSERT INTO kb_collections (collection_id, name, project, created_at)
		VALUES ($1, $2, '', $3)`, knRunE2ECollectionID, knRunE2ECollectionName, now.UnixMilli())
	require.NoError(t, err, "seed 集合")

	// 投影行：AGENT.md 的 `knowledge:` 声明经它换成稳定集合 ID（D-19）。
	_, err = db.ExecContext(ctx, `
		INSERT INTO agent_resource_bindings
			(agent_uid, agent_name, resource_kind, ref_name, resource_uid, content_hash, projected_at)
		VALUES ($1, $2, 'knowledge_collection', $3, $4, $5, $6)`,
		"agent-"+runHarnessAgentName, runHarnessAgentName, knRunE2ECollectionName,
		knRunE2ECollectionID, knRunE2EModel, now.UnixMilli())
	require.NoError(t, err, "seed Agent 知识集合投影")

	root := t.TempDir()
	docPath := filepath.Join(root, filepath.FromSlash(knRunE2EDocPath))
	require.NoError(t, os.MkdirAll(filepath.Dir(docPath), 0o755))
	require.NoError(t, os.WriteFile(docPath, []byte(knRunE2EDocBody), 0o644))

	reader, err := knowledge.NewRootReader(root, 1<<20)
	require.NoError(t, err, "文档读取根")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := postgres.NewKnowledgeStore(db)
	indexer := postgres.NewIndexStore(db)
	embedder := knRunE2EEmbedder{}
	// 诊断端口只建一份：检索器与运行级端口共用同一个适配器，两处对原因码的解释不会分叉。
	// One diagnostic adapter shared by the retriever and the run-side port.
	diag := newKnRunE2EDiagnostics(t, db, logger)
	if state.events != nil {
		diag = newKnowledgeDiagAdapter(state.events, logger)
	}
	events := diag.(*knowledgeDiagAdapter).recorder

	// 摄取器：与生产装配同一组构造器，只有 embedding 提供方 / 配额 / embedding 审计是假件。
	state.ingestor = knowledge.NewIngestor(knowledge.IngestorDeps{
		Store:    &diagnosticIngestStore{IngestStore: store, recorder: events},
		Reader:   reader,
		Chunker:  knowledge.NewTextChunker(200, 20),
		Embedder: embedder,
		Vectors:  indexer,
		Audit:    &diagnosticIngestAudit{EmbeddingAudit: knRunE2EAudit{}, recorder: events},
		Quota:    knRunE2EQuota{},
		Config: knowledge.IngestConfig{
			Generation:          knRunE2EGeneration,
			EmbeddingModel:      knRunE2EModel,
			EmbeddingDimensions: knowledge.VectorDimensions,
			EmbeddingBatchSize:  4,
			MaxDocumentBytes:    1 << 20,
			AllowedExtensions:   []string{".md"},
			ChunkerVersion:      "md-v1",
		},
		Logger: logger,
		Now:    func() time.Time { return now },
	})

	var keyword knowledge.KeywordSearcher = postgres.NewKnowledgeKeywordSearcher(db)
	var vectors knowledge.VectorSearcher = postgres.NewKnowledgeVectorSearcher(db)
	if failSearch {
		keyword = knRunE2EFailingKeyword{}
		vectors = knRunE2EFailingVectors{}
	}

	retrievalConfig := state.retrievalConfig
	if retrievalConfig.TopK <= 0 {
		retrievalConfig.TopK = 5
	}
	if retrievalConfig.CandidateK <= 0 {
		retrievalConfig.CandidateK = 20
	}
	readClock := func() time.Time { return now }
	if state.events != nil {
		readClock = time.Now
	}
	retriever := knowledge.NewRetriever(knowledge.RetrieverDeps{
		Authz:       postgres.NewKnowledgeAuthz(db, agents),
		Keyword:     keyword,
		Vectors:     vectors,
		Assembly:    postgres.NewAssemblyStore(db, logger),
		Diagnostics: diag,
		Config:      retrievalConfig,
		Logger:      logger,
		Now:         readClock,
	})
	viewer := knowledge.NewSourceViewer(knowledge.SourceViewerDeps{
		Reader: store,
		Authz:  postgres.NewKnowledgeAuthz(db, agents),
		Logger: logger,
		Now:    func() time.Time { return now },
	})
	return newKnowledgeRunSource(retriever, viewer, embedder,
		knRunE2EGeneration, diag, logger)
}

// knRunE2EEmbedder 是确定性向量来源：不访问网络，宽度与冻结维度一致。
type knRunE2EEmbedder struct{}

// Embed 按输入序号造不同方向的向量（同一批内不重复，便于人工读日志）。
func (knRunE2EEmbedder) Embed(_ context.Context, texts []string) (knowledge.EmbedBatch, error) {
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = knRunE2EVector(float32(i+1) * 0.1)
	}
	return knowledge.EmbedBatch{
		Vectors:      vectors,
		PromptTokens: int64(len(texts) * 7),
		Dimensions:   knowledge.VectorDimensions,
		Model:        knRunE2EModel,
	}, nil
}

// knRunE2EVector 造一条 512 维、全非零的向量。
func knRunE2EVector(base float32) []float32 {
	vec := make([]float32, knowledge.VectorDimensions)
	for i := range vec {
		vec[i] = base + float32(i)*0.001
	}
	return vec
}

// knRunE2EAudit 是可用的两阶段 embedding 审计假件（预写成功、终态照收）。
type knRunE2EAudit struct{}

// BeginEmbedding 返回一个稳定的调用标识。
func (knRunE2EAudit) BeginEmbedding(_ context.Context, jobID, _ string, _ int) (string, error) {
	return "emb_" + jobID, nil
}

// FinishEmbedding 恒成功。
func (knRunE2EAudit) FinishEmbedding(context.Context, string, int64, error) error { return nil }

// knRunE2EQuota 是不限量票据假件：摄取不占调用方配额（配额闭合归 V-5 的用例）。
type knRunE2EQuota struct{}

// AdmitCall 永远批一张"无预留"票据。
func (knRunE2EQuota) AdmitCall(context.Context, core.Principal, knowledge.QuotaCost,
	string,
) (knowledge.QuotaTicket, error) {
	return knowledge.QuotaTicket{NoReservation: true}, nil
}

// Commit 对无预留票据是空操作，恒成功。
func (knRunE2EQuota) Commit(context.Context, knowledge.QuotaTicket, knowledge.QuotaUsage) error {
	return nil
}

// Release 同上。
func (knRunE2EQuota) Release(context.Context, knowledge.QuotaTicket, knowledge.QuotaReleaseReason) error {
	return nil
}

// knRunE2EFailingKeyword 让关键词路失败（只用于 T6 的两路皆失败分支）。
type knRunE2EFailingKeyword struct{}

// Search 恒返回注入错误。
func (knRunE2EFailingKeyword) Search(context.Context, knowledge.Scope, string, int) ([]knowledge.Hit, int, error) {
	return nil, 0, errKnRunE2ESearchDown
}

// knRunE2EFailingVectors 让向量路失败。
type knRunE2EFailingVectors struct{}

// Search 恒返回注入错误。
func (knRunE2EFailingVectors) Search(context.Context, knowledge.Scope, []float32, int, int) ([]knowledge.Hit, int, error) {
	return nil, 0, errKnRunE2ESearchDown
}

// knRunSpy 是 `core.KnowledgeRunSource` 的**只观察**装饰器：每个方法原样转给真实现，
// 另记一份入参与结论供断言。它不参与判定，也不改写任何返回值。
type knRunSpy struct {
	inner core.KnowledgeRunSource

	mu            sync.Mutex
	requests      []core.KnowledgeRunRequest
	injections    []*core.KnowledgeInjection
	revalidations []core.KnowledgeRevalidateRequest
	reports       []core.KnowledgeAnswerReport
	answers       []string
	retrieveErr   error
}

// RetrieveForRun 转调真实现并记录请求与注入面。
func (s *knRunSpy) RetrieveForRun(ctx context.Context, req core.KnowledgeRunRequest,
) (*core.KnowledgeInjection, error) {
	injection, err := s.inner.RetrieveForRun(ctx, req)
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.injections = append(s.injections, injection)
	s.retrieveErr = err
	s.mu.Unlock()
	return injection, err
}

// RevalidateChunks 转调真实现并记录复核请求。
func (s *knRunSpy) RevalidateChunks(ctx context.Context, req core.KnowledgeRevalidateRequest,
) (*core.KnowledgeInjection, error) {
	s.mu.Lock()
	s.revalidations = append(s.revalidations, req)
	s.mu.Unlock()
	return s.inner.RevalidateChunks(ctx, req)
}

// ResolveAnswer 转调真实现并记录复核结论。
func (s *knRunSpy) ResolveAnswer(ctx context.Context, req core.KnowledgeAnswerRequest,
) (core.KnowledgeAnswerReport, error) {
	report, err := s.inner.ResolveAnswer(ctx, req)
	s.mu.Lock()
	s.reports = append(s.reports, report)
	s.answers = append(s.answers, req.Answer)
	s.mu.Unlock()
	return report, err
}

// lastInjection 返回最后一次运行级检索的注入面（未检索时为 nil）。
func (s *knRunSpy) lastInjection() *core.KnowledgeInjection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.injections) == 0 {
		return nil
	}
	return s.injections[len(s.injections)-1]
}

// lastReport 返回最后一次引用复核结论。
func (s *knRunSpy) lastReport() core.KnowledgeAnswerReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports) == 0 {
		return core.KnowledgeAnswerReport{}
	}
	return s.reports[len(s.reports)-1]
}

// reportCount 返回引用复核的调用次数（空注入时不得被调用）。
func (s *knRunSpy) reportCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reports)
}

// revalidationCount 返回轮内复核次数。
func (s *knRunSpy) revalidationCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.revalidations)
}

// knRunE2EEvent 是读回的一条诊断事件（只取判定需要的列）。
type knRunE2EEvent struct {
	Kind     string
	Reason   string
	Decision string
	Attrs    map[string]any
	StepID   string
	Actor    string
}

// knRunE2EAssembly 是读回的一条装配记录。
type knRunE2EAssembly struct {
	Purpose      string
	StepID       string
	Agent        string
	PromptVer    string
	ModelVer     string
	Hits         []string
	CandidateLen int
}

// newKnRunE2EDiagnostics 建真实诊断写入器：全采样 + 白名单脱敏 + 真实诊断存储。
func newKnRunE2EDiagnostics(t *testing.T, db *postgres.DB, logger *slog.Logger) knowledge.DiagnosticRecorder {
	t.Helper()

	cfg, err := diagnostics.NewConfig(0, 1)
	require.NoError(t, err, "诊断配置")
	recorder := diagnostics.NewRecorder(diagnostics.RecorderOptions{
		Store:    postgres.NewDiagnosticStore(db),
		Sampler:  diagnostics.NewRateSampler(cfg),
		Redactor: diagnostics.NewWhitelistRedactor(),
		Now:      time.Now,
		Logger:   logger,
	})
	return newKnowledgeDiagAdapter(recorder, logger)
}

// ingest 走真实摄取器把材料入库；失败即 Fatal（本用例的前提就是"材料已生效"）。
func (e *knRunE2E) ingest(t *testing.T) knowledge.IngestResult {
	t.Helper()

	res, err := e.state.ingestor.Ingest(context.Background(), knowledge.IngestRequest{
		Principal:    core.LocalAdminPrincipal(),
		CollectionID: knRunE2ECollectionID,
		Path:         knRunE2EDocPath,
	})
	require.NoError(t, err, "材料入库")
	require.Equal(t, knowledge.VersionActive, res.Status, "材料必须生效（active）才能参与检索")
	e.state.documentID = res.DocumentID
	return res
}

// deleteDocument 软删已入库的文档（撤权的"删除"分支）。
func (e *knRunE2E) deleteDocument(t *testing.T) {
	t.Helper()

	require.NoError(t, e.state.ingestor.Delete(context.Background(), knowledge.DeleteRequest{
		Principal:  core.LocalAdminPrincipal(),
		DocumentID: e.state.documentID,
	}))
}

// deleteCollection 软删整个集合（撤权的"集合不可见"分支）。
func (e *knRunE2E) deleteCollection(t *testing.T) {
	t.Helper()

	require.NoError(t, e.state.ingestor.Delete(context.Background(), knowledge.DeleteRequest{
		Principal:    core.LocalAdminPrincipal(),
		CollectionID: knRunE2ECollectionID,
	}))
}

// capturePrompts 挂上"每次模型调用"的观察钩子，返回取值函数；系统消息固定是
// `Messages[0]`（`run.go` 的 `llmEnv` 顺序）。
func (e *knRunE2E) capturePrompts() func() []string {
	var mu sync.Mutex
	var prompts []string
	e.provider.observe(func(req core.ChatRequest) {
		content := ""
		if len(req.Messages) > 0 {
			content = req.Messages[0].Content
		}
		mu.Lock()
		prompts = append(prompts, content)
		mu.Unlock()
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
}

// runOnce 走调度器入口跑一次 Run。运行期失败走 `Status`，不经 error 通道。
func (e *knRunE2E) runOnce(t *testing.T) core.RunResult {
	t.Helper()

	res, err := e.invoker.Run(context.Background(), e.requestFor(runHarnessCaller))
	require.NoError(t, err, "调度器入口的运行期失败必须体现在 Status 上")
	return res
}

// assemblies 读回该 Run 的装配记录。
func (e *knRunE2E) assemblies(t *testing.T, runID string) []knRunE2EAssembly {
	t.Helper()

	rows, err := e.db.QueryContext(context.Background(), `
		SELECT purpose, step_id, agent, prompt_version, model_version, hits::text, candidates::text
		FROM context_assembly_records
		WHERE run_id = $1
		ORDER BY created_at, record_id`, runID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	out := make([]knRunE2EAssembly, 0, 1)
	for rows.Next() {
		var item knRunE2EAssembly
		var hitsJSON, candidatesJSON string
		require.NoError(t, rows.Scan(&item.Purpose, &item.StepID, &item.Agent, &item.PromptVer,
			&item.ModelVer, &hitsJSON, &candidatesJSON))
		require.NoError(t, json.Unmarshal([]byte(hitsJSON), &item.Hits), "hits 必须是 JSON 数组")
		var candidates []json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(candidatesJSON), &candidates), "candidates 必须是 JSON 数组")
		item.CandidateLen = len(candidates)
		out = append(out, item)
	}
	require.NoError(t, rows.Err())
	return out
}

// events 读回该 Run 的诊断事件（按写入顺序）。
func (e *knRunE2E) events(t *testing.T, runID string) []knRunE2EEvent {
	t.Helper()

	rows, err := e.db.QueryContext(context.Background(), `
		SELECT kind, COALESCE(reason_code, ''), COALESCE(decision, ''), attrs::text,
		       COALESCE(step_id, ''), COALESCE(actor, '')
		FROM diagnostic_events
		WHERE run_id = $1
		ORDER BY occurred_at, event_id`, runID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	out := make([]knRunE2EEvent, 0, 2)
	for rows.Next() {
		var item knRunE2EEvent
		var attrsJSON string
		require.NoError(t, rows.Scan(&item.Kind, &item.Reason, &item.Decision, &attrsJSON,
			&item.StepID, &item.Actor))
		require.NoError(t, json.Unmarshal([]byte(attrsJSON), &item.Attrs), "attrs 必须是 JSON 对象")
		out = append(out, item)
	}
	require.NoError(t, rows.Err())
	return out
}

// eventsWithReason 过滤出指定原因码的事件。
func eventsWithReason(events []knRunE2EEvent, reason string) []knRunE2EEvent {
	out := make([]knRunE2EEvent, 0, len(events))
	for _, event := range events {
		if event.Reason == reason {
			out = append(out, event)
		}
	}
	return out
}

// TestKnowledgeRunE2E_AnswerCitesInjectedChunk 判完整正常链路：材料入库后发起 Run，
// 检索结果**真的进了模型上下文**，答案里的 `（来源 c1）` 能关联到有效来源，
// 且检索留下了装配记录。
//
// 逆改点：`RetrieveForRun` 不注入 / `Render` 丢片段 → 系统消息断言红；
// `ResolveAnswer` 不读回正文 → Supported 计数红；`Retrieve` 不写装配记录 → 行数红。
func TestKnowledgeRunE2E_AnswerCitesInjectedChunk(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true})
	e.ingest(t)
	prompts := e.capturePrompts()
	e.provider.respondWithText(knRunE2EAnswerCited)

	res := e.runOnce(t)
	require.Equal(t, core.StatusCompleted, res.Status, "Run 必须正常完成")

	injection := e.state.spy.lastInjection()
	require.NotNil(t, injection, "检索必须产出注入面")
	require.Len(t, injection.Chunks, 1)
	chunk := injection.Chunks[0]
	require.Equal(t, "c1", chunk.Marker, "标记按最终顺序连续编号")
	require.Equal(t, knRunE2EDocPath, chunk.SourcePath)
	require.True(t, chunk.Ref.Valid(), "注入片段的来源引用必须可复核（文档 + 版本 + 内容哈希）")
	require.Equal(t, core.EvidenceKindKnowledge, chunk.Ref.Kind, "来源种类必须是知识类")

	systemPrompts := prompts()
	require.Len(t, systemPrompts, 1, "本用例只有一次模型调用")
	require.Contains(t, systemPrompts[0], knRunE2EHeading, "检索结果必须真进系统消息")
	require.Contains(t, systemPrompts[0], "签收后七天内可无理由退款", "片段正文必须进上下文")
	require.Contains(t, systemPrompts[0], "[c1]", "标记必须出现在提示词里，否则引用无从对齐")

	report := e.state.spy.lastReport()
	require.Equal(t, 1, report.Supported, "`（来源 c1）` 必须关联到本次实际进入上下文的来源")
	require.Zero(t, report.Unsupported, "不得出现不可定位 / 被替代的来源")
	require.Zero(t, report.Unverified, "逐字依据必须能在精确读取的正文里找到")
	require.Zero(t, report.Unresolved, "标记 c1 是读过的，不得进未解析")

	records := e.assemblies(t, res.RunID)
	require.Len(t, records, 1, "运行级检索必须留下装配记录")
	require.Equal(t, "retrieval", records[0].Purpose)
	require.Empty(t, records[0].StepID, "运行级检索发生在第 1 轮之前，没有轮次可填")
	require.Equal(t, runHarnessAgentName, records[0].Agent)
	require.Len(t, records[0].Hits, 1, "命中片段必须进装配记录的 hits")

	require.Empty(t, e.events(t, res.RunID), "正常链路不得产生诊断事件")
}

// TestKnowledgeRunE2E_UnresolvedCitationEmitsDiagnostic 判引用复核的负例：答案引用了
// 一个从未进入上下文的标记，必须如实进 `Unresolved` 并留下**结论性**诊断事件
// （没有 allow / deny 可填，判定必须为空），且事件里不得出现标识、路径或正文。
//
// 逆改点：不写事件 / 事件带决策 / attrs 塞进标识或正文 → 对应断言红。
func TestKnowledgeRunE2E_UnresolvedCitationEmitsDiagnostic(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true})
	e.ingest(t)
	e.provider.respondWithText(knRunE2EAnswerStray)

	res := e.runOnce(t)
	require.Equal(t, core.StatusCompleted, res.Status, "引用不可解析不是运行失败")

	report := e.state.spy.lastReport()
	require.Equal(t, 1, report.Unresolved, "（来源 c9）必须进未解析")
	require.Zero(t, report.Supported, "本句没有可判定主张")

	events := eventsWithReason(e.events(t, res.RunID), "citation_unresolved")
	require.Len(t, events, 1, "未解析引用必须留下恰一条结论性事件")
	event := events[0]
	require.Equal(t, "retrieval", event.Kind)
	require.Empty(t, event.Decision, "引用复核没有决策可填，不得凭空补一个")
	require.Empty(t, event.StepID)
	require.Equal(t, "caller", event.Actor)
	require.Equal(t, map[string]any{"count": float64(1)}, event.Attrs,
		"事件只允许带计数——标识本身不得进诊断")

	raw, err := json.Marshal(event.Attrs)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "c9")
	require.NotContains(t, string(raw), knRunE2EDocPath)
	require.NotContains(t, string(raw), "签收后")
}

// TestKnowledgeRunE2E_MissingReadScopeInjectsNothing 判无权限分支：调用方没有
// `knowledge:read`，检索必须在**任何外部调用与库查询之前**停住——零注入、零改判，
// 但必须留下一条权限拒绝诊断。
//
// 逆改点：缺 scope 仍检索 → 注入面 / 装配记录断言红；拒绝不留痕 → 事件断言红。
func TestKnowledgeRunE2E_MissingReadScopeInjectsNothing(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{})
	e.ingest(t)
	prompts := e.capturePrompts()
	e.provider.respondWithText("无知识可用，按通用流程处理。")

	res := e.runOnce(t)
	require.Equal(t, core.StatusCompleted, res.Status, "没有知识是合法状态，不是运行失败")

	require.Nil(t, e.state.spy.lastInjection(), "缺 scope 必须零注入")
	require.Zero(t, e.state.spy.reportCount(), "空注入不得进入引用复核")
	systemPrompts := prompts()
	require.Len(t, systemPrompts, 1)
	require.NotContains(t, systemPrompts[0], knRunE2EHeading, "无权限不得注入任何知识字符")

	events := eventsWithReason(e.events(t, res.RunID), "permission_denied")
	require.Len(t, events, 1, "权限拒绝必须留痕")
	require.Equal(t, "deny", events[0].Decision)
	require.Empty(t, events[0].Attrs, "拒绝事件不得带详情")

	require.Empty(t, e.assemblies(t, res.RunID), "没检索就没有装配记录")
}

// TestKnowledgeRunE2E_DeletedDocumentYieldsEmptyInjection 判材料删除：文档被软删后
// 检索命中为零，Run 照常执行、零注入，但**装配记录仍然留在**——"确实没有可用的
// 材料"是有证据的正常空结果，不是故障。
//
// 逆改点：删除后仍注入 → 提示词断言红；空结果不写记录 → 行数断言红。
func TestKnowledgeRunE2E_DeletedDocumentYieldsEmptyInjection(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true})
	e.ingest(t)
	e.deleteDocument(t)
	prompts := e.capturePrompts()
	e.provider.respondWithText("材料已删除，按通用流程处理。")

	res := e.runOnce(t)
	require.Equal(t, core.StatusCompleted, res.Status)

	require.Nil(t, e.state.spy.lastInjection(), "已删除材料不得进入上下文")
	systemPrompts := prompts()
	require.Len(t, systemPrompts, 1)
	require.NotContains(t, systemPrompts[0], knRunE2EHeading)

	records := e.assemblies(t, res.RunID)
	require.Len(t, records, 1, "检索执行成功（零命中）必须留下装配记录")
	require.Equal(t, "retrieval", records[0].Purpose)
	require.Empty(t, records[0].Hits, "零命中时 hits 是空数组")
}

// TestKnowledgeRunE2E_DeletedCollectionIsRefused 判集合级撤权：整只集合软删后授权
// 解析得出"请求的集合均不可见"，这是**越权结论**（ErrForbidden）而不是空结果——
// 必须留权限拒绝事件，且不产生装配记录（检索没有发生）。
//
// 逆改点：把全拒当成零命中 → 事件断言红、装配记录断言红。
func TestKnowledgeRunE2E_DeletedCollectionIsRefused(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true})
	e.ingest(t)
	e.deleteCollection(t)
	e.provider.respondWithText("集合不可见，按通用流程处理。")

	res := e.runOnce(t)
	require.Equal(t, core.StatusCompleted, res.Status, "撤权是合法状态，不是运行失败")

	require.Nil(t, e.state.spy.lastInjection(), "不可见集合必须零注入")
	events := eventsWithReason(e.events(t, res.RunID), "permission_denied")
	require.Len(t, events, 1, "全拒必须留痕")
	require.Equal(t, "deny", events[0].Decision)
	require.Empty(t, e.assemblies(t, res.RunID), "检索没有发生，不得有装配记录")
}

// TestKnowledgeRunE2E_RevalidationNarrowsMidRun 判**轮内**撤权：工具执行期间文档被删，
// 第 2 轮的注入面必须立即收窄——第 1 轮提示词含片段，第 2 轮不含（C-26）。
//
// 逆改点：去掉 `revalidateKnowledge` → 第 2 轮提示词仍含片段，断言红。
func TestKnowledgeRunE2E_RevalidationNarrowsMidRun(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true})
	e.ingest(t)
	prompts := e.capturePrompts()

	var deleteErr error
	e.tools.onInvoke(func() {
		deleteErr = e.state.ingestor.Delete(context.Background(), knowledge.DeleteRequest{
			Principal:  core.LocalAdminPrincipal(),
			DocumentID: e.state.documentID,
		})
	})
	e.provider.respondWithToolCalls(1)

	res := e.runOnce(t)
	require.NoError(t, deleteErr, "运行中删除材料")
	require.Equal(t, core.StatusCompleted, res.Status)

	systemPrompts := prompts()
	require.Len(t, systemPrompts, 2, "1 轮工具调用 + 1 轮最终回答")
	require.Contains(t, systemPrompts[0], knRunE2EHeading, "第 1 轮的片段必须已注入")
	require.NotContains(t, systemPrompts[1], knRunE2EHeading,
		"材料在第 1 轮被删，第 2 轮必须看不到它")
	require.NotContains(t, systemPrompts[1], "签收后七天内可无理由退款")
	require.Positive(t, e.state.spy.revalidationCount(), "每轮组装前都必须复核注入面")
}

// TestKnowledgeRunE2E_RetrievalFailureRefusesRun 判检索失败：两路皆不可用属**存储违约**，
// Run 必须拒绝启动（零模型调用、零工具执行），并把两次降级如实留在诊断里。
//
// 逆改点：把存储违约吞成空注入 → Status / 调用次数断言红；不写降级事件 → 事件断言红。
func TestKnowledgeRunE2E_RetrievalFailureRefusesRun(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true, FailSearch: true})
	e.provider.respondWithText("不应被调用")

	res := e.runOnce(t)
	require.Equal(t, core.StatusFailed, res.Status, "检索违约必须让 Run 失败")
	require.Contains(t, res.Reason, "运行级知识检索", "失败原因必须点名运行级知识检索")
	require.Zero(t, e.provider.calls(), "检索失败必须发生在任何模型调用之前")
	require.Zero(t, e.tools.invocationCount(), "工具不得执行")

	events := e.events(t, res.RunID)
	require.Len(t, eventsWithReason(events, "keyword_unavailable"), 1, "关键词路降级必须留痕")
	require.Len(t, eventsWithReason(events, "vector_unavailable"), 1, "向量路降级必须留痕")
	for _, event := range eventsWithReason(events, "keyword_unavailable") {
		require.Equal(t, "degrade", event.Decision)
	}

	require.Empty(t, e.assemblies(t, res.RunID), "检索失败不写装配记录")
}

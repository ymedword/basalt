package app

// 本文件是 US-16 知识与历史两条链路的**装配层接线**（T099 前置）：在它之前
// `internal/app` 对 `internal/knowledge` / `internal/history` 零引用，web 的八个消费方
// 端口全是 nil，端点一律固定 500。本文件补齐三件事：
//
//  1. **embedding 连接解析 + 索引代际固定**：`embedding.model_connection` 非空时走
//     与运行目录**同一套**解析（发布指针 → 已发布版本 → 凭据三件 → `HTTPEmbedder`），
//     再用 `GenerationManager.Ensure` 在启动期固定当前 active 代际（无 active 代际时
//     建首代际、校验并激活——C-09 允许）。未配置时**不**构造客户端、**不**建代际：
//     语义路径按设计降级为关键词并留 `vector_unavailable`，但**不得**静默假装已启用。
//  2. **领域对象接到消费方端口**：摄取 / 检索 / 来源视图 / 目录四个知识端口，
//     压缩接线要的来源校验器（`compaction.ValidityChecker` 适配器），以及历史两条
//     端口（检索 + 精确读取，共用**一个** `*postgres.HistoryStore`——两个实例会让
//     "读到的"与"写下的"之间多一层无从对齐的影子）。
//  3. **检索适配器**：web 只给线上形状（文本 + 集合 + TopK），算查询向量、固定代际、
//     把不可分类失败收敛成 `knowledge.ErrRetrieverUnavailable` 都归这里。
//
// **缺省归消费方**：装载层（`internal/cli`）不预填任何 US-16 数值缺省
// （`TestLoadConfig_US16DefaultsStayWithConsumers` 逐字断言零值），§3.3 的「建议缺省」
// 由本层落地，常量集中在本文件顶部。D-24 的「配置阈值不在代码里写默认数字」只管
// 压缩阈值（`compaction.*` 启用时逐键必填、缺一即拒），不覆盖这里：`embedding.*` /
// `knowledge.*` 的缺省是 §3.3 冻结表里写明的取值，不是猜的数字。
//
// This file wires the US-16 knowledge and history surfaces into the web consumer
// ports: embedding connection resolution and startup generation pinning, the four
// knowledge ports plus the compaction validity adapter, the shared history store, and
// the search adapter that embeds the query and pins the generation. The §3.3 defaults
// land here because the loader deliberately keeps every US-16 value at zero.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/history"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/web"
)

// US-16 §3.3 的消费方缺省。每个数字都能指回冻结表的「建议缺省」列；装载层保持零值，
// 由这里在装配时填补，因此"配置没写"与"配置写错"在启动期是可区分的两种结局。
// The §3.3 defaults, applied at assembly because the loader keeps zero values.
const (
	// knowledgeDefaultEmbeddingBatchSize 是单次 embedding 的文本上限。
	knowledgeDefaultEmbeddingBatchSize = 32
	// knowledgeDefaultEmbeddingTimeoutSeconds 是单批超时；0 值走它，负数由装载层拒绝。
	knowledgeDefaultEmbeddingTimeoutSeconds = 30
	// knowledgeDefaultMaxDocumentBytes 是单文件上限 20 MiB（§3.3 标注"待测"——取值是
	// 冻结表原文，容量校验仍待实测，不因这里落地就算已验证）。
	knowledgeDefaultMaxDocumentBytes int64 = 20 << 20
	// knowledgeDefaultChunkMaxTokens / knowledgeDefaultChunkOverlapTokens /
	// knowledgeDefaultChunkVersion 是切分三元组，§2.16 与 tasks.md 已冻结 400/60/v1。
	knowledgeDefaultChunkMaxTokens     = 400
	knowledgeDefaultChunkOverlapTokens = 60
	knowledgeDefaultChunkVersion       = "v1"
)

// knowledgeDefaultAllowedExtensions 是首批可摄取类型（§3.3 `.md` / `.txt`）。
// 空白名单在领域层是"全拒"，而 YAML 里「没写」与「写空列表」解码后不可区分——
// 消费方必须替这两种情况选一个意思。这里选 §3.3 的缺省（放行两种文本格式），
// 拒绝"缺省即全拒"：那会让文档里写明的缺省值永远无法表达，也会让最小配置的实例
// 摄取端永远 503。**不得**在别处再定义第二份白名单。
// The §3.3 default extension list; "unset" and "explicitly empty" are
// indistinguishable after YAML decoding, so the consumer must pick — it picks the
// documented default rather than a permanently disabled ingest path.
var knowledgeDefaultAllowedExtensions = []string{".md", ".txt"}

// 编译期断言：领域实现直接满足消费方端口，**不**需要额外适配器的那几对在这里固定
// 下来——端口形状一旦漂移，编译期就红，而不是等到路由返回 500 才暴露。
// Compile-time assertions for the ports satisfied structurally by domain types.
var (
	_ web.KnowledgeIngest        = (*knowledge.Ingestor)(nil)
	_ web.KnowledgeSource        = (*knowledge.SourceViewer)(nil)
	_ web.EvidenceValidity       = (*knowledge.SourceViewer)(nil)
	_ web.HistorySearch          = (*history.Searcher)(nil)
	_ web.HistoryExactRead       = (*history.ExactReader)(nil)
	_ web.CompactionChain        = (*postgres.CompactionStore)(nil)
	_ knowledge.Validity         = (*knowledge.SourceViewer)(nil)
	_ compaction.ValidityChecker = (*knowledgeValidityAdapter)(nil)
	_ web.KnowledgeSearch        = (*knowledgeSearchAdapter)(nil)
)

// knowledgeWiringDeps 是知识库装配的全部输入。资源存储与凭据来源只在
// `embedding.model_connection` 非空时才用得上；诊断与配额是**必填**——缺任何一件都
// 会让对应端口在运行期永久失败关闭，不如在启动期点名。
// knowledgeWiringDeps carries every input of the knowledge assembly.
type knowledgeWiringDeps struct {
	// Config 是已装载的配置；nil 按全零处理（等价于"什么都没配"）。
	Config *cli.Config
	// DB 是已迁移的 PostgreSQL 句柄。
	DB *postgres.DB
	// WorkspaceRoot 是摄取的允许根（`RootReader` 的根目录）。
	WorkspaceRoot string
	// Resources 是发布资源存储，用于解析 embedding 的模型连接。
	Resources *postgres.ResourceStore
	// Credentials 是托管凭据来源（env 模式不需要）。
	Credentials managedCredentialSource
	// Quota 是调用方配额端口；摄取预留走它。
	Quota knowledge.Quota
	// Diagnostics 是检索诊断端口（降级与拒绝都留痕）。
	Diagnostics knowledge.DiagnosticRecorder
	// QuotaDiagnostics 是摄取配额判定的诊断端口（V-8）；可空（诊断未启用时为 nil）。
	QuotaDiagnostics knowledge.IngestQuotaDiagnosticRecorder
	// Agents 是已注册 Agent 的读面（D-19 检索授权据此读 `knowledge:` 声明）；
	// nil 时带 Agent 的检索失败关闭。
	Agents postgres.KnowledgeAgentSource
	// Catalog 是知识集合目录存储。装配层**提前**构造同一个实例（启动投影重建要用它
	// 解析声明名），这里复用而不是再建一个——两个实例只是两个指针，但"同一个"能让
	// 未来加缓存时不会出现两份互不可见的目录态。
	Catalog *postgres.KnowledgeCatalogStore
	// Logger / Now 与运行时其余部分同源。
	Logger *slog.Logger
	Now    func() time.Time
}

// knowledgeWiring 是装配产物：投影给 web 的端口与交给压缩接线的来源校验器。
// knowledgeWiring is the assembly product projected onto the web ports.
type knowledgeWiring struct {
	// Ingest 是摄取端口（摄取 / 删除 / 重建索引）。
	Ingest web.KnowledgeIngest
	// Search 是检索端口（适配器，见本文件下半）。
	Search web.KnowledgeSearch
	// Source 同时满足来源视图与引用有效性两个端口。
	Source *knowledge.SourceViewer
	// Catalog 是目录读面适配器。
	Catalog       web.KnowledgeCatalog
	Description   web.KnowledgeDescriptionAdmin
	IngestHistory web.KnowledgeIngestHistory
	Upload        web.KnowledgeUpload
	// Collections 是同一个目录存储的"名字 → 稳定集合 ID"读面（D-19）：注册闸门、
	// 热加载与绑定投影共用。与 Catalog 端口同源同一个实例。
	// Collections is the name-resolution face over the same catalog store.
	Collections KnowledgeCollectionSource
	// Validity 是压缩来源闸门要的校验器（知识库撤权 / 删除 / 版本变更的运行时判定）。
	Validity compaction.ValidityChecker
	// Embedder 是知识库栈解析出的 embedding 客户端（`embedding.model_connection` 未配置时
	// 为 nil）。`recall_memory` 的语义路径**复用同一个**客户端——连接解析、代际固定与
	// 凭据取用都只有这一份实现，记忆侧不另建取键路径（P-07 / §3.1.4）。
	// The one embedding client shared with the semantic recall path.
	Embedder knowledge.Embedder
	// Generation 是启动期固定并激活的索引代际号（0 = embedding 未配置，未固定）。
	// 记忆向量写入（V-5）按它显式回填，**不**另行查 active 代际——装配期固定的号就是
	// 唯一目标，运行期没有第二个挑选点。
	// Generation is the startup-pinned generation; memory vector writes (V-5) target it
	// explicitly instead of re-selecting an active generation at runtime.
	Generation int
	// Run 是 Run 侧知识链路端口（V-6）：检索 / 轮内复核 / 引用复核。它与 `Search`
	// 共用同一个检索器、来源视图与代际——两条消费方对"当前可见集合"的理解必须同源。
	// Run is the run-side knowledge port; it shares the retriever, viewer and generation
	// with the management search so both consumers see the same scope.
	Run core.KnowledgeRunSource
	// Assembly 是装配记录写入器（V-7）：检索装配与对话装配共用**同一个**实例。
	// 它同时满足 `knowledge.AssemblyRecorder` 与 `core.AssemblyRecorder`——记录结构
	// 在 core，两个消费方各自声明端口，实现（`*postgres.assemblyStore`）一个。
	// The one assembly writer shared by the retrieval and chat purposes; it satisfies
	// both consumer-declared ports structurally.
	Assembly core.AssemblyRecorder
}

// newKnowledgeWiring 装配知识库的全部组件。
//
// 顺序不可交换：先解析 embedding 连接并**固定代际**（会写库：首代际的建行—校验—
// 激活），再构造摄取器——摄取器拿到的 `Generation` 必须是已激活的号，否则版本行会
// 指向一个还不存在的代际。摄取器**总是**构造：embedding 未配置时它在 `ready()` 处
// 失败关闭并回 `ErrIngestUnavailable`（web 映射成 503 `NOT_CONFIGURED` 的"未配置"），
// 这比"端口缺失"的固定 500 更诚实。
// newKnowledgeWiring resolves the embedding connection, pins the generation, then
// builds the ingestor — which always exists and fails closed when unconfigured.
func newKnowledgeWiring(ctx context.Context, deps knowledgeWiringDeps) (*knowledgeWiring, error) {
	if deps.DB == nil {
		return nil, fmt.Errorf("知识库装配缺少数据库句柄")
	}
	if deps.Quota == nil {
		return nil, fmt.Errorf("知识库装配缺少调用方配额端口")
	}
	if deps.Diagnostics == nil {
		return nil, fmt.Errorf("知识库装配缺少诊断记录端口")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	cfg := deps.Config
	if cfg == nil {
		cfg = &cli.Config{}
	}

	indexStore := postgres.NewIndexStore(deps.DB)
	client, err := newKnowledgeEmbedder(ctx, cfg.Embedding, deps.Resources, deps.Credentials)
	if err != nil {
		return nil, err
	}

	// 代际在**启动期**固定：本 US 没有"运行期切换代际"的端点，固定点是装配而不是
	// 每请求；`Ensure` 在 active 代际与配置不一致时拒绝启动（换模型必须显式切换，
	// 启动路径不得自动切），无 active 代际时建首代际——两句都是 C-09 的原文。
	// The generation is pinned at startup; there is no runtime switch in this US.
	// 两阶段审计的**唯一** SQL 入口：摄取侧直接用 `embedBatch` 写它，查询侧由
	// `auditedEmbedder` 写它——同一个实例、两条各写各的路径，互不嵌套（嵌套会把
	// 每批调用双记成两行）。查询侧 embedding（管理面检索与 `recall_memory` 语义路）
	// 同样是外部提供方调用，**不得**因为它只发一条就绕开审计。
	// One audit entry point shared by ingest (its own pre-write) and query-side calls
	// (through auditedEmbedder); the two paths never nest.
	audit := postgres.NewEmbeddingCallRepo(deps.DB, knowledge.VectorDimensions)

	var (
		// embedder 是摄取器拿到的**裸**客户端：它自己预写审计。
		embedder knowledge.Embedder
		// queryEmbedder 是查询侧用的**带审计**装饰器。
		queryEmbedder knowledge.Embedder
		generation    int
	)
	if client != nil {
		manager := knowledge.NewGenerationManager(knowledge.GenerationDeps{
			Store:  indexStore,
			Logger: logger,
			Now:    now,
		})
		pinned, err := manager.Ensure(ctx, knowledge.GenerationSpec{
			EmbeddingModel: strings.TrimSpace(cfg.Embedding.Model),
			Dimensions:     cfg.Embedding.Dimensions,
		})
		if err != nil {
			return nil, fmt.Errorf("固定索引代际: %w", err)
		}
		embedder = client
		queryEmbedder = &auditedEmbedder{
			inner:  client,
			audit:  audit,
			model:  strings.TrimSpace(cfg.Embedding.Model),
			logger: logger,
		}
		generation = pinned.Generation
		logger.InfoContext(ctx, "知识库：向量路径已装配",
			"embedding_model", pinned.EmbeddingModel, "generation", generation,
			"dimensions", pinned.Dimensions)
	} else {
		// 只 WARN 不拒绝启动：§3.3 明说 embedding 缺省不阻断启动，语义路径降级为关键词。
		// 诊断事件由检索侧在每次降级时写（`vector_unavailable`），这里只是启动留痕。
		logger.WarnContext(ctx, "知识库：embedding 未配置，语义路径降级为关键词检索")
	}

	reader, err := knowledge.NewRootReader(deps.WorkspaceRoot, maxDocumentBytes(cfg.Knowledge.MaxDocumentBytes))
	if err != nil {
		return nil, fmt.Errorf("知识库装配（文档读取根 %q）: %w", deps.WorkspaceRoot, err)
	}
	knowledgeStore := postgres.NewKnowledgeStore(deps.DB)
	// 授权读的是**注册表快照**里的声明与投影表里的稳定 ID（D-19）：两处读在同一个
	// `Resolve` 里完成，调用方自己的项目与 scope 判定仍然先于它们发生。
	// Authz reads the declared names off the registry snapshot and the projected ids off
	// the binding table, after the caller's own project and scope are checked.
	authz := postgres.NewKnowledgeAuthz(deps.DB, deps.Agents)
	chunker := knowledge.NewTextChunker(chunkMaxTokens(cfg.Knowledge.Chunk.MaxTokens),
		chunkOverlapTokens(cfg.Knowledge.Chunk.OverlapTokens))

	ingestor := knowledge.NewIngestor(knowledge.IngestorDeps{
		Store:   knowledgeStore,
		Reader:  reader,
		Chunker: chunker,
		// Embedder 为 nil 时 `ready()` 首个条件即失败关闭——摄取端回"未配置"。
		Embedder: embedder,
		Vectors:  indexStore,
		// 两阶段审计：预写 `pending` 失败则**不发出**调用（宪法第 5 条的调用面）。
		Audit:            audit,
		Quota:            deps.Quota,
		QuotaDiagnostics: deps.QuotaDiagnostics,
		Config: knowledge.IngestConfig{
			Generation:          generation,
			EmbeddingModel:      strings.TrimSpace(cfg.Embedding.Model),
			EmbeddingDimensions: cfg.Embedding.Dimensions,
			EmbeddingBatchSize:  embeddingBatchSize(cfg.Embedding.BatchSize),
			MaxDocumentBytes:    maxDocumentBytes(cfg.Knowledge.MaxDocumentBytes),
			AllowedExtensions:   allowedExtensions(cfg.Knowledge.AllowedExtensions),
			ChunkerVersion:      chunkerVersion(cfg.Knowledge.Chunk.Version),
		},
		Logger: logger,
		Now:    now,
	})

	// 装配记录写入器只建**一个**：检索装配（purpose=retrieval）与对话装配
	// （purpose=chat，V-7）写的是同一张表，两个实例虽无状态，但"同一个"能让未来的
	// 批量 / 缓存改动只有一处落点。
	// One assembly writer shared by the retrieval and chat purposes.
	assembly := postgres.NewAssemblyStore(deps.DB, logger)
	retriever := knowledge.NewRetriever(knowledge.RetrieverDeps{
		Authz:   authz,
		Keyword: postgres.NewKnowledgeKeywordSearcher(deps.DB),
		Vectors: postgres.NewKnowledgeVectorSearcher(deps.DB),
		// 装配记录端口：`Search` 不写（只有 `Retrieve` 写），但端口必须在位——
		// `ready()` 缺端口即整体失败关闭，补一条空记录的捷径不存在。
		Assembly:    assembly,
		Diagnostics: deps.Diagnostics,
		Config:      retrievalConfigOf(cfg.Retrieval),
		Logger:      logger,
		Now:         now,
	})

	viewer := knowledge.NewSourceViewer(knowledge.SourceViewerDeps{
		Reader: knowledgeStore,
		Authz:  authz,
		Logger: logger,
		Now:    now,
	})

	sticky := &knowledgeSearchAdapter{
		retriever: retriever,
		// 适配器持有的是**带审计**的那一个：它每算一次查询向量都先预写 pending。
		embedder:   queryEmbedder,
		generation: generation,
		logger:     logger,
	}

	catalogStore := deps.Catalog
	if catalogStore == nil {
		catalogStore = postgres.NewKnowledgeCatalogStore(deps.DB)
	}
	return &knowledgeWiring{
		Ingest:        &auditedKnowledgeIngest{inner: ingestor, guard: knowledgeStore, operations: postgres.NewManagementStore(deps.DB), now: now, logger: logger},
		Search:        sticky,
		Source:        viewer,
		Catalog:       newKnowledgeCatalogAdapter(catalogStore),
		Description:   &knowledgeDescriptionAdmin{store: catalogStore, operations: postgres.NewManagementStore(deps.DB), now: now, logger: logger},
		IngestHistory: newKnowledgeCatalogAdapter(catalogStore),
		Upload:        &knowledgeUploadAdmin{root: deps.WorkspaceRoot, maxBytes: maxDocumentBytes(cfg.Knowledge.MaxDocumentBytes), operations: postgres.NewManagementStore(deps.DB), now: now, logger: logger},
		Collections:   catalogStore,
		Validity:      &knowledgeValidityAdapter{viewer: viewer},
		// 对外暴露的也是带审计的那一个：`recall_memory` 的语义路径经
		// `memoryEmbedderOf` 复用它，查询侧审计因此覆盖两条消费方。
		Embedder:   queryEmbedder,
		Generation: generation,
		// Run 侧复用同一个检索器（装配记录端口在位）、来源视图（精确读取）与带审计的
		// 查询向量客户端；诊断端口与检索器同一份适配器——两处对原因码的理解不会分叉。
		Run: newKnowledgeRunSource(retriever, viewer, queryEmbedder, generation,
			deps.Diagnostics, logger),
		// 对话装配记录（V-7）与检索装配共用同一个写入器：两条 purpose 写同一张表，
		// 同源让"哪一轮注入了什么"与"检索召回了什么"能按 run_id 对齐。
		Assembly: assembly,
	}, nil
}

// newKnowledgeEmbedder 解析 `embedding.model_connection` 并构造协议客户端。
// 未配置连接时返回 (nil, nil)：这不是错误，是"语义路径降级"的正常分支。
// 连接名走运行目录的**同一套**解析（归属、发布态、名字一致性只有一份判定），
// 凭据值只经 `catalogAPIKey` 取用，**不得**进入日志或错误文案。
// newKnowledgeEmbedder resolves the configured connection; an unconfigured embedder
// is a normal degraded branch, not an error.
func newKnowledgeEmbedder(ctx context.Context, cfg cli.EmbeddingConfig,
	resources *postgres.ResourceStore, credentials managedCredentialSource,
) (*knowledge.HTTPEmbedder, error) {
	name := strings.TrimSpace(cfg.ModelConnection)
	if name == "" {
		return nil, nil
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("embedding.model 为空：配置了 embedding.model_connection=%q 时必填", name)
	}
	if cfg.Dimensions < 1 {
		return nil, fmt.Errorf("embedding.dimensions 必须为正（当前 %d）", cfg.Dimensions)
	}
	if resources == nil {
		return nil, fmt.Errorf("embedding.model_connection=%q 无法解析：缺少发布资源存储", name)
	}
	pointer, err := resources.Pointer(ctx, core.ResourceModelConnection, name)
	if err != nil {
		return nil, fmt.Errorf("解析 embedding 模型连接 %q: %w", name, err)
	}
	entry, version, err := publishedProviderEntry(ctx, resources, pointer)
	if err != nil {
		return nil, err
	}
	apiKey, err := catalogAPIKey(ctx, pointer, entry, version, credentials)
	if err != nil {
		return nil, err
	}
	client, err := knowledge.NewHTTPEmbedder(knowledge.EmbedderOptions{
		Name:       entry.Name,
		BaseURL:    entry.BaseURL,
		APIKey:     apiKey,
		Model:      model,
		Dimensions: cfg.Dimensions,
		Timeout:    time.Duration(embeddingTimeoutSeconds(cfg.TimeoutSeconds)) * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return client, nil
}

// embeddingBatchSize 返回生效的批量上限：0 = 未写，取 §3.3 缺省；负数由装载层拒绝。
// 显式 0 与未写不可区分，按缺省处理（0 本身不是一个有意义的批量）。
// embeddingBatchSize applies the §3.3 default when the key is absent.
func embeddingBatchSize(configured int) int {
	if configured <= 0 {
		return knowledgeDefaultEmbeddingBatchSize
	}
	return configured
}

// embeddingTimeoutSeconds 同上：0 = 未写，取 30 秒。
// embeddingTimeoutSeconds applies the default when the key is absent.
func embeddingTimeoutSeconds(configured int) int {
	if configured <= 0 {
		return knowledgeDefaultEmbeddingTimeoutSeconds
	}
	return configured
}

// maxDocumentBytes 返回生效的单文件上限：0 = 未写，取 20 MiB。
// maxDocumentBytes applies the default when the key is absent.
func maxDocumentBytes(configured int64) int64 {
	if configured <= 0 {
		return knowledgeDefaultMaxDocumentBytes
	}
	return configured
}

// chunkMaxTokens / chunkOverlapTokens / chunkerVersion 是切分三元组的消费方缺省。
// 重叠的显式 0 与未写在解码后不可区分，这里统一按缺省 60 处理；真要与 0 重叠，
// 需要配置结构改成指针（本 US 不改已冻结的键形状，局限记入 run-state）。
// The chunk triple defaults; an explicit zero overlap is indistinguishable from unset.
func chunkMaxTokens(configured int) int {
	if configured <= 0 {
		return knowledgeDefaultChunkMaxTokens
	}
	return configured
}

func chunkOverlapTokens(configured int) int {
	if configured <= 0 {
		return knowledgeDefaultChunkOverlapTokens
	}
	return configured
}

func chunkerVersion(configured string) string {
	trimmed := strings.TrimSpace(configured)
	if trimmed == "" {
		return knowledgeDefaultChunkVersion
	}
	return trimmed
}

// allowedExtensions 返回生效的摄取白名单：非空原样透传（存储层大小写不敏感比较），
// 空取 §3.3 缺省——理由见 `knowledgeDefaultAllowedExtensions`。
// allowedExtensions passes a configured whitelist through and falls back to §3.3.
func allowedExtensions(configured []string) []string {
	if len(configured) == 0 {
		return knowledgeDefaultAllowedExtensions
	}
	out := make([]string, 0, len(configured))
	for _, ext := range configured {
		if trimmed := strings.TrimSpace(ext); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return knowledgeDefaultAllowedExtensions
	}
	return out
}

// retrievalConfigOf 把配置段原样映射成领域参数：0 值由领域用文档缺省（8/40/60）
// 填补，负数由装载层拒绝；本层**不**再补一套数字。`rerank.enabled=true` 原样透传
// ——重排本 US 未交付（D-20），领域在 `ready()` fail-closed，**不得**在这里把它
// 悄悄改成 false 或跳过。
// retrievalConfigOf passes the section through; zeros are filled by the domain and an
// enabled-but-undelivered rerank fails closed there instead of being silently ignored.
func retrievalConfigOf(cfg cli.RetrievalConfig) knowledge.RetrievalConfig {
	return knowledge.RetrievalConfig{
		TopK:          cfg.TopK,
		CandidateK:    cfg.CandidateK,
		RRFK:          cfg.RRFK,
		RerankEnabled: cfg.Rerank.Enabled,
	}
}

// knowledgeSearchAdapter 是 `web.KnowledgeSearch` 的装配层实现：web 给线上形状，
// 这里负责"文本 → 查询向量 → 固定代际"，再交 `Retriever.Search`。
//
// 三步的顺序各有理由：
//   - **先判身份与 scope，再算向量**：向量化是一次外部调用。准入必须发生在它之前，
//     否则一个没有 `knowledge:read` 的调用方也能让实例替它花钱调 embedding。领域层
//     稍后还会再判一次（`Retriever.Search` 的授权在检索前），这里不替代它，只是
//     不让"先花钱后被拒"成为可能；
//   - **向量化失败只降级不失败**：`embedding.*` 是可选能力，失败映射成"本次没有向量"
//     （Embedding 为空、Generation=0），领域据此只走关键词路并把 `vector_unavailable`
//     写进诊断与响应——**不得**把降级伪装成零命中，也不得让检索整体失败；
//   - **代际只在成功算向量时才带**：没有向量却声称代际，会让向量路按错误代际查。
//
// This adapter embeds the query and pins the generation. Identity and scope are
// checked BEFORE the embedding call (it is external), and an embedding failure only
// degrades the vector path — the domain records `vector_unavailable` itself.
type knowledgeSearchAdapter struct {
	retriever  *knowledge.Retriever
	embedder   knowledge.Embedder
	generation int
	logger     *slog.Logger
}

// Search 执行一次管理面检索。
// Search runs one management search.
func (a *knowledgeSearchAdapter) Search(ctx context.Context, q web.KnowledgeSearchQuery) (knowledge.SearchResult, error) {
	// 与 `knowledgeAuthzScope.Resolve` 同一判定次序：身份 → scope。两处都判不是重复，
	// 是"外部调用之前"与"任何查询之前"两个不同时刻的同一条规则。
	if !q.Principal.Valid() {
		return knowledge.SearchResult{}, fmt.Errorf("%w: 检索身份不可用", core.ErrUnauthorized)
	}
	if !q.Principal.HasScope(core.ScopeKnowledgeRead) {
		return knowledge.SearchResult{}, fmt.Errorf("%w: 缺少 %s scope", core.ErrForbidden, core.ScopeKnowledgeRead)
	}

	req := knowledge.SearchRequest{
		Principal:     q.Principal,
		AgentName:     q.Agent,
		CollectionIDs: q.Collections,
		Query:         q.Query,
		TopK:          q.TopK,
		Budget:        q.Budget,
	}
	if a.embedder != nil && a.generation > 0 {
		if text := strings.TrimSpace(q.Query); text != "" {
			batch, err := a.embedder.Embed(ctx, []string{text})
			switch {
			case err != nil:
				// ctx 已经结束就没有必要再降级跑关键词：直接回取消 / 截止，避免把
				// "请求被取消"记成一次检索降级。
				if ctxErr := ctx.Err(); ctxErr != nil {
					return knowledge.SearchResult{}, ctxErr
				}
				a.logger.WarnContext(ctx, "知识库：查询向量化失败，本次降级为关键词检索", "error", err)
			case len(batch.Vectors) != 1:
				a.logger.WarnContext(ctx, "知识库：查询向量化没有可用向量，本次降级为关键词检索",
					"vectors", len(batch.Vectors))
			default:
				req.Embedding = batch.Vectors[0]
				req.Generation = a.generation
			}
		}
	}

	result, err := a.retriever.Search(ctx, req)
	if err != nil {
		return knowledge.SearchResult{}, convergeSearchError(err)
	}
	return result, nil
}

// convergeSearchError 把检索失败收敛到契约允许的错误面。`/search` 只有 503
// `NOT_CONFIGURED` 这一个 5xx（`web.writeKnowledgeSearchError` 的映射表），
// 其余失败必须落进已分类的哨兵：取消、未认证、越权、请求非法原样透传，
// **其他任何错误**（存储违约、驱动错误、未知分支）一律收敛成
// `knowledge.ErrRetrieverUnavailable` —— 降级成 200 空命中会让调用方以为"查过了"。
// convergeSearchError folds unclassifiable failures onto ErrRetrieverUnavailable so
// /search never grows a 500 and never fakes an empty result.
func convergeSearchError(err error) error {
	switch {
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, core.ErrUnauthorized),
		errors.Is(err, core.ErrForbidden),
		errors.Is(err, knowledge.ErrInvalidRequest),
		errors.Is(err, knowledge.ErrRetrieverUnavailable):
		return err
	default:
		// 保留原始文案便于排障，但不把陌生错误类型接进错误链（`%v` 不是 `%w`）：
		// 上层只按已知哨兵分类，多一条可 `errors.Is` 的路径就等于多一种未定义语义。
		return fmt.Errorf("%w: %v", knowledge.ErrRetrieverUnavailable, err)
	}
}

// knowledgeValidityAdapter 把知识库的引用有效性判定搬成压缩侧的 `Validity`。
// 只搬形状，不重新判定：四个原因字面量（`revoked` / `deleted` / `version_changed` /
// `not_found`）在两个包里逐字相同，映射保持一一对应；未知原因原样透传，由压缩侧的
// 判定按 fail-closed 处置（不在这里"猜成有效"）。
// The adapter only maps shapes; the four reason literals are identical in both
// packages and an unknown reason is passed through for the consumer to fail closed.
type knowledgeValidityAdapter struct {
	viewer *knowledge.SourceViewer
}

// Check 判定一条引用现在是否仍可按原样使用。
// Check reports whether the referenced version is still usable as-is.
func (a *knowledgeValidityAdapter) Check(ctx context.Context, p core.Principal,
	ref core.EvidenceRef,
) (compaction.Validity, error) {
	result, err := a.viewer.Check(ctx, p, ref)
	if err != nil {
		return compaction.Validity{}, err
	}
	return compaction.Validity{Valid: result.Valid, Reason: result.Reason}, nil
}

// historyWiring 是历史回查的装配产物：两条端口共用一个存储实例。
// historyWiring holds the two history ports over one shared store.
type historyWiring struct {
	// Search 是检索端口（命中只是指针，不含正文）。
	Search web.HistorySearch
	// Exact 是精确读取端口（每次重查 ACL / 删除 / 版本）。
	Exact web.HistoryExactRead
}

// newHistoryWiring 装配历史检索与精确读取。
//
// 两条端口必须读**同一个** `*postgres.HistoryStore`：检索给出的 `MessageID` 要能被
// 精确读取按同一套 scope 解释，两个实例之间没有任何需要对齐的状态，多建一个只会
// 让连接池与时钟分叉。
// newHistoryWiring shares one HistoryStore between search and exact read.
func newHistoryWiring(db *postgres.DB, logger *slog.Logger) (historyWiring, error) {
	if db == nil {
		return historyWiring{}, fmt.Errorf("历史回查装配缺少数据库句柄")
	}
	store := postgres.NewHistoryStore(db)
	return historyWiring{
		Search: history.NewSearcher(history.SearcherDeps{Store: store, Logger: logger}),
		Exact:  history.NewExactReader(store),
	}, nil
}

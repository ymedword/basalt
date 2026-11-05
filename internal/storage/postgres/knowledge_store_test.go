//go:build integration

package postgres

// 本文件是知识库存储适配层的**有库**用例（T045）。两条口径：
//
//  1. **真库、真状态机**：所有断言都打在同 schema 的真实 PostgreSQL 上——`UNIQUE
//     (document_id, content_hash)`、部分唯一索引 `ux_kb_document_versions_active`、
//     `CHECK (status <> 'failed' OR error_kind IS NOT NULL)` 这些约束只有真库才判。
//  2. **假端口只假在外部世界**：embedding 提供方、配额、审计与"向量写入"（真实现归
//     T129 的 `index.go`）用替身；摄取器、切分器、检索器与**被测存储**都是真实现。
//
// 夹具注意：迁移器只 seed `local-admin`，**不**建 `kb_index_generations` 行（首代际
// 由启动期代际管理建立），因此每个用例自己 seed 代际 1；这一缺口已进颗粒度文档附录
// 与 run-state 未验证清单。
//
// These are the database-backed cases for the knowledge store: real schema, real
// constraints, real ingestor/retriever/chunker; only the outside world is faked.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// knNow 是夹具的固定写库时刻：时间列一律 Unix 毫秒 UTC，固定值让断言可复算。
var knNow = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

// knTestDB 建一个已跑完迁移的隔离 schema 并返回句柄。扩展装在库级 `public` 里，
// 必须留在 search_path 上供 `vector(n)` / `gin_trgm_ops` 解析。
// knTestDB returns a handle on a freshly migrated isolated schema.
func knTestDB(t *testing.T) *DB {
	t.Helper()
	pool := testPool(t)
	paths := []string{createSchema(t, pool)}
	seen := map[string]bool{paths[0]: true}
	for _, ext := range []string{"vector", "pg_trgm"} {
		extSchema := extensionSchema(t, pool, ext)
		if seen[extSchema] {
			continue
		}
		seen[extSchema] = true
		paths = append(paths, extSchema)
	}
	db := openMigratedTestDB(t, Config{
		DSN:            withSearchPath(t, testDSN(t), paths...),
		ConnectTimeout: 5 * time.Second,
	})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// knSeedGeneration 建代际 1（active）。迁移器不 seed 代际行，见文件头。
// knSeedGeneration inserts the first active generation row.
func knSeedGeneration(t *testing.T, db *DB, generation int) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO kb_index_generations
			(generation, embedding_model, dimensions, distance, status, activated_at, created_at)
		VALUES ($1, 'text-embedding-v4', 512, 'cosine', 'active', $2, $2)`,
		generation, knNow.UnixMilli())
	require.NoError(t, err)
}

// knSeedCollection 建一个集合。
func knSeedCollection(t *testing.T, db *DB, collectionID, name string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO kb_collections (collection_id, name, project, created_at)
		VALUES ($1, $2, '', $3)`, collectionID, name, knNow.UnixMilli())
	require.NoError(t, err)
}

// knHash 是片段 / 文档内容哈希的夹具写法（sha256 小写十六进制，与生产同型）。
func knHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// knDoc 拼一份可切分的文档来源。
func knDoc(path, text string) knowledge.DocumentSource {
	return knowledge.DocumentSource{
		Path:        path,
		Text:        text,
		ContentHash: knHash(text),
		SizeBytes:   int64(len(text)),
	}
}

// knVector 造一条 512 维、全非零的向量；`base` 让不同调用点得到不同方向的向量。
func knVector(base float32) []float32 {
	v := make([]float32, knowledge.VectorDimensions)
	for i := range v {
		v[i] = base + float32(i)*0.001
	}
	return v
}

// containsVersion 判定命中集里是否出现指定版本。
func containsVersion(hits []knowledge.Hit, versionID string) bool {
	for _, hit := range hits {
		if hit.VersionID == versionID {
			return true
		}
	}
	return false
}

// knFakeReader 按路径返回预置文档；未预置的路径按"不存在"拒绝。
type knFakeReader struct {
	docs map[string]knowledge.DocumentSource
}

func (r *knFakeReader) Load(_ context.Context, path string) (knowledge.DocumentSource, error) {
	src, ok := r.docs[path]
	if !ok {
		return knowledge.DocumentSource{}, fmt.Errorf("%w: %s", knowledge.ErrPathRejected, path)
	}
	return src, nil
}

// knFakeEmbedder 造确定性的向量：不访问网络，维数与冻结值一致。
type knFakeEmbedder struct{}

func (knFakeEmbedder) Embed(_ context.Context, texts []string) (knowledge.EmbedBatch, error) {
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = knVector(float32(i+1) * 0.1)
	}
	return knowledge.EmbedBatch{
		Vectors: vectors,
		Model:   "text-embedding-v4",
	}, nil
}

// knToggleEmbedder 由用例开关：`failNow` 之后的调用一律报错，用来把「新版本摄取
// 中途失败」这一刻拉进用例（失败前的那一次仍走 knFakeEmbedder 的正常向量）。
// knToggleEmbedder fails on demand so the mid-ingest failure path stays testable.
type knToggleEmbedder struct {
	mu   sync.Mutex
	fail bool
}

func (e *knToggleEmbedder) Embed(ctx context.Context, texts []string) (knowledge.EmbedBatch, error) {
	e.mu.Lock()
	fail := e.fail
	e.mu.Unlock()
	if fail {
		return knowledge.EmbedBatch{}, errors.New("embedding 提供方超时")
	}
	return knFakeEmbedder{}.Embed(ctx, texts)
}

// failNow 打开失败开关：此后每次 Embed 都报错。
func (e *knToggleEmbedder) failNow() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fail = true
}

// knFakeAudit 是可用的两阶段审计替身：预写成功，终态照收。
type knFakeAudit struct{}

func (knFakeAudit) BeginEmbedding(_ context.Context, jobID, model string, inputCount int) (string, error) {
	return "emb_" + jobID, nil
}

func (knFakeAudit) FinishEmbedding(_ context.Context, callID string, promptTokens int64, callErr error) error {
	return nil
}

// knFakeQuota 是可用的配额替身：永远批预留，结算与释放都成功。端口取
// `knowledge.Quota` 的包内值类型（C-06：knowledge 不再 import `callers`）。
// knFakeQuota always admits; it implements the knowledge-local quota port (C-06).
type knFakeQuota struct{}

func (knFakeQuota) AdmitCall(_ context.Context, p core.Principal, cost knowledge.QuotaCost,
	idem string,
) (knowledge.QuotaTicket, error) {
	return knowledge.QuotaTicket{ReservationID: "qres_it_" + idem, CallerID: p.CallerID}, nil
}

func (knFakeQuota) Commit(_ context.Context, ticket knowledge.QuotaTicket, usage knowledge.QuotaUsage) error {
	return nil
}

func (knFakeQuota) Release(_ context.Context, ticket knowledge.QuotaTicket, reason knowledge.QuotaReleaseReason) error {
	return nil
}

// knFakeAuthz 直接返回预置的可见集合（ACL 解析的用例归 T129 的 `authorize.go`）。
type knFakeAuthz struct{ collections []string }

func (a knFakeAuthz) Resolve(_ context.Context, p core.Principal, agent string, collections []string) (knowledge.Scope, error) {
	return knowledge.Scope{Collections: a.collections, Agent: agent}, nil
}

// knFakeAssembly 只吞装配记录：记录形状的用例归 T129。
type knFakeAssembly struct{}

func (knFakeAssembly) Record(_ context.Context, record core.AssemblyRecord) {}

// knFakeDiagnostics 只吞诊断事件：诊断可丢弃。
type knFakeDiagnostics struct{}

func (knFakeDiagnostics) Record(_ context.Context, event knowledge.DiagnosticEvent) {}

// knBlockingIndexer 是 `knowledge.VectorIndex` 的替身：把向量写进
// `kb_chunk_embeddings`（真实现归 T129 的 `index.go`），并可在写入**之前**阻塞——
// 用来把"片段已落库、版本未生效"这一刻拉长到可观测（R5 的判别力就在这一刻）。
// knBlockingIndexer stands in for the vector index and can stall before writing.
type knBlockingIndexer struct {
	db       *DB
	mu       sync.Mutex
	blocking bool
	entered  chan struct{}
	release  chan struct{}
}

func (b *knBlockingIndexer) Write(ctx context.Context, versionID string, chunks []knowledge.ChunkRef,
	vectors [][]float32, generation int,
) error {
	// 在锁内取一次快照：放行方会关闭同一个 channel，但**不**替换它——
	// 边等边换通道会让读端拿到未同步的值（`-race` 实测报 DATA RACE）。
	b.mu.Lock()
	blocking, release := b.blocking, b.release
	b.mu.Unlock()
	if blocking {
		select {
		case b.entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for i, ref := range chunks {
		literal, err := vectorLiteral(vectors[i])
		if err != nil {
			return err
		}
		if _, err := b.db.pool.ExecContext(ctx, `
			INSERT INTO kb_chunk_embeddings (chunk_id, generation, embedding, model, dimensions, created_at)
			VALUES ($1, $2, $3::vector, 'text-embedding-v4', $4, $5)
			ON CONFLICT (chunk_id, generation) DO UPDATE SET embedding = EXCLUDED.embedding`,
			ref.ChunkID, generation, literal, knowledge.VectorDimensions,
			time.Now().UTC().UnixMilli()); err != nil {
			return fmt.Errorf("写向量 %s: %w", ref.ChunkID, err)
		}
	}
	return nil
}

func (b *knBlockingIndexer) Activate(_ context.Context, versionID string, generation int) error {
	return nil
}

func (b *knBlockingIndexer) Retire(_ context.Context, versionID string) error { return nil }

// block 打开写入前阻塞；waitEntered 等到写入真的进入阻塞点。
func (b *knBlockingIndexer) block() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blocking = true
}

func (b *knBlockingIndexer) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("摄取没有走到向量写入——阻塞点未命中，用例失去判别力")
	}
}

// releaseNow 放行阻塞的写入；重复调用无副作用。
func (b *knBlockingIndexer) releaseNow() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.blocking {
		return
	}
	close(b.release)
	b.blocking = false
}

// knHarness 装配"真存储 + 真摄取器 + 真检索器 + 假外部世界"的夹具。
type knHarness struct {
	db         *DB
	store      *knowledgeStore
	ingestor   *knowledge.Ingestor
	retriever  *knowledge.Retriever
	keyword    *keywordSearcher
	vectors    *vectorSearcher
	indexer    *knBlockingIndexer
	collection string
	principal  core.Principal
}

// newKnHarness 建隔离库、代际 1 与集合 `col_it`，并装配四个组件。
func newKnHarness(t *testing.T, docs map[string]knowledge.DocumentSource) *knHarness {
	t.Helper()
	return newKnHarnessWith(t, docs, knFakeEmbedder{})
}

// newKnHarnessWith 与 newKnHarness 同构，但换掉 embedding 替身——失败路径的用例
// 需要「第一次成功、之后报错」的提供方。
// newKnHarnessWith swaps the embedder so failure paths stay reachable.
func newKnHarnessWith(t *testing.T, docs map[string]knowledge.DocumentSource,
	embedder knowledge.Embedder,
) *knHarness {
	t.Helper()
	db := knTestDB(t)
	knSeedGeneration(t, db, 1)
	knSeedCollection(t, db, "col_it", "it-runbooks")

	h := &knHarness{
		db:         db,
		store:      NewKnowledgeStore(db),
		keyword:    NewKnowledgeKeywordSearcher(db),
		vectors:    NewKnowledgeVectorSearcher(db),
		collection: "col_it",
		principal:  core.LocalAdminPrincipal(),
		indexer: &knBlockingIndexer{
			db:      db,
			entered: make(chan struct{}, 1),
			release: make(chan struct{}),
		},
	}
	h.ingestor = knowledge.NewIngestor(knowledge.IngestorDeps{
		Store:    h.store,
		Reader:   &knFakeReader{docs: docs},
		Chunker:  knowledge.NewTextChunker(200, 20),
		Embedder: embedder,
		Vectors:  h.indexer,
		Audit:    knFakeAudit{},
		Quota:    knFakeQuota{},
		Config: knowledge.IngestConfig{
			Generation:          1,
			EmbeddingModel:      "text-embedding-v4",
			EmbeddingDimensions: knowledge.VectorDimensions,
			EmbeddingBatchSize:  4,
			MaxDocumentBytes:    1 << 20,
			AllowedExtensions:   []string{".md", ".txt"},
			ChunkerVersion:      "md-v1",
		},
		Now: func() time.Time { return knNow },
	})
	h.retriever = knowledge.NewRetriever(knowledge.RetrieverDeps{
		Authz:       knFakeAuthz{collections: []string{h.collection}},
		Keyword:     h.keyword,
		Vectors:     h.vectors,
		Assembly:    knFakeAssembly{},
		Diagnostics: knFakeDiagnostics{},
		Config:      knowledge.RetrievalConfig{TopK: 5, CandidateK: 20},
		Now:         func() time.Time { return knNow },
	})
	return h
}

// ingest 同步跑一次摄取（不带阻塞）。
func (h *knHarness) ingest(t *testing.T, path string) knowledge.IngestResult {
	t.Helper()
	res, err := h.ingestor.Ingest(context.Background(), knowledge.IngestRequest{
		Principal:    h.principal,
		CollectionID: h.collection,
		Path:         path,
	})
	require.NoError(t, err)
	return res
}

// query 组一次进提示词的检索：管理面身份、固定代际 1。
func (h *knHarness) query(text string) knowledge.RetrieveRequest {
	return knowledge.RetrieveRequest{
		Principal:     h.principal,
		RunID:         "run_it",
		CollectionIDs: []string{h.collection},
		Query:         text,
		Embedding:     knVector(0.2),
		Generation:    1,
	}
}

// startJob 直接开一次作业（绕开摄取器的存储层用例共用）。
func (h *knHarness) startJob(t *testing.T, jobID, path, contentHash string) knowledge.JobRecord {
	t.Helper()
	job, err := h.store.StartJob(context.Background(), knowledge.StartJobRequest{
		JobID:          jobID,
		CollectionID:   h.collection,
		SourcePath:     path,
		ContentHash:    contentHash,
		ParserVersion:  knowledge.ParserVersion,
		ChunkerVersion: "md-v1",
		At:             knNow,
	})
	require.NoError(t, err)
	return job
}

// seedChunks 用存储自己的写入路径落片段。
func (h *knHarness) seedChunks(t *testing.T, versionID string, texts ...string) []knowledge.ChunkRef {
	t.Helper()
	drafts := make([]knowledge.ChunkDraft, len(texts))
	offset := 0
	for i, text := range texts {
		length := len([]rune(text))
		drafts[i] = knowledge.ChunkDraft{
			Seq:         i + 1,
			CharStart:   offset,
			CharEnd:     offset + length,
			Text:        text,
			ContentHash: knHash(text),
		}
		offset += length
	}
	refs, err := h.store.WriteChunks(context.Background(), versionID, drafts)
	require.NoError(t, err)
	return refs
}

// seedEmbeddings 按片段顺序写该代际的向量（真写入归 T129）。
func (h *knHarness) seedEmbeddings(t *testing.T, refs []knowledge.ChunkRef, generation int) {
	t.Helper()
	for _, ref := range refs {
		literal, err := vectorLiteral(knVector(0.3))
		require.NoError(t, err)
		_, err = h.db.pool.ExecContext(context.Background(), `
			INSERT INTO kb_chunk_embeddings (chunk_id, generation, embedding, model, dimensions, created_at)
			VALUES ($1, $2, $3::vector, 'text-embedding-v4', $4, $5)`,
			ref.ChunkID, generation, literal, knowledge.VectorDimensions, knNow.UnixMilli())
		require.NoError(t, err)
	}
}

// publish 把版本走完 `pending → indexed → active` 的正规路径。
func (h *knHarness) publish(t *testing.T, versionID string, texts ...string) []knowledge.ChunkRef {
	t.Helper()
	refs := h.seedChunks(t, versionID, texts...)
	h.seedEmbeddings(t, refs, 1)
	require.NoError(t, h.store.MarkIndexed(context.Background(), versionID, knNow))
	_, err := h.store.ActivateVersion(context.Background(), versionID, 1, knNow)
	require.NoError(t, err)
	return refs
}

// TestIngest_NewVersionInvisibleUntilIndexed 是 C-10 的**存储层**判据：新版本在索引
// 完成并通过校验前不得对查询可见，生效后才被召回。
// 逆改点：把 `status='active'` 提前到向量写入之前（或让检索不筛 `v.status`）→ 第一次
// Retrieve 命中该版本，`require.False` 红。
func TestIngest_NewVersionInvisibleUntilIndexed(t *testing.T) {
	h := newKnHarness(t, map[string]knowledge.DocumentSource{
		"v2.md": knDoc("v2.md", "# 回滚手册\n\n回滚步骤：先停写入，再回放日志，最后核对计数。\n"),
	})
	ctx := context.Background()

	h.indexer.block()
	errs := make(chan error, 1)
	results := make(chan knowledge.IngestResult, 1)
	go func() {
		res, err := h.ingestor.Ingest(ctx, knowledge.IngestRequest{
			Principal:    h.principal,
			CollectionID: h.collection,
			Path:         "v2.md",
		})
		errs <- err
		results <- res
	}()
	h.indexer.waitEntered(t)

	// 此刻：片段已落库、版本仍是 pending、向量还没写。检索不得看到它。
	var versionID string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT v.version_id FROM kb_document_versions v
		JOIN kb_documents d ON d.document_id = v.document_id
		WHERE d.source_path = 'v2.md'`).Scan(&versionID))
	require.NotEmpty(t, versionID)

	before, err := h.retriever.Retrieve(ctx, h.query("回滚步骤"))
	require.NoError(t, err)
	require.False(t, containsVersion(before.Hits, versionID), "未激活版本不得被召回")

	h.indexer.releaseNow()
	require.NoError(t, <-errs)
	published := <-results
	require.Equal(t, knowledge.VersionActive, published.Status)
	require.Equal(t, versionID, published.VersionID)

	after, err := h.retriever.Retrieve(ctx, h.query("回滚步骤"))
	require.NoError(t, err)
	require.True(t, containsVersion(after.Hits, versionID), "生效后的版本必须被召回")
}

// TestKnowledgeStore_StartJobReusesVersionForSameHash 判最新失败版本的同内容重试：
// 复用版本行并追加 attempt，不依赖内容哈希唯一约束。
func TestKnowledgeStore_StartJobReusesVersionForSameHash(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()

	first := h.startJob(t, "job_first", "runbook.md", "hash-a")
	require.Equal(t, 1, first.Attempt)

	require.NoError(t, h.store.FailVersion(ctx, first.VersionID, knowledge.ErrorKindParserFailed, knNow))
	require.NoError(t, h.store.FinishJob(ctx, first.JobID, knowledge.JobStageParse,
		knowledge.JobStatusFailed, knowledge.ErrorKindParserFailed, knNow))

	second := h.startJob(t, "job_second", "runbook.md", "hash-a")
	require.Equal(t, first.DocumentID, second.DocumentID, "同路径必须命中同一条文档行")
	require.Equal(t, first.VersionID, second.VersionID, "同内容必须复用同一条版本行")
	require.Equal(t, 2, second.Attempt)

	var status string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status FROM kb_document_versions WHERE version_id = $1`,
		second.VersionID).Scan(&status))
	require.Equal(t, string(knowledge.VersionPending), status, "复用的版本必须回到 pending")
}

// TestKnowledgeStore_StartJobRefusesSecondAttemptWhileJobOpen 判并发摄取不得同时写
// 同一版本：仍有 `running` 作业时拒绝接管，也**不得**把版本复位。
// 逆改点：去掉在途作业检查 → 第二个 StartJob 返回 nil，本用例红。
func TestKnowledgeStore_StartJobRefusesSecondAttemptWhileJobOpen(t *testing.T) {
	h := newKnHarness(t, nil)
	first := h.startJob(t, "job_open", "runbook.md", "hash-a")

	_, err := h.store.StartJob(context.Background(), knowledge.StartJobRequest{
		JobID:          "job_racing",
		CollectionID:   h.collection,
		SourcePath:     "runbook.md",
		ContentHash:    "hash-a",
		ParserVersion:  knowledge.ParserVersion,
		ChunkerVersion: "md-v1",
		At:             knNow,
	})
	require.ErrorIs(t, err, errKnowledgeIngestInFlight)

	var attempts int
	require.NoError(t, h.db.pool.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM kb_ingest_jobs WHERE version_id = $1`,
		first.VersionID).Scan(&attempts))
	require.Equal(t, 1, attempts, "被拒的第二次摄取不得留下作业行")
}

// TestKnowledgeStore_StartJobRefusesActiveVersion 判同一内容已经生效时不得把它打回
// `pending`——那会让在服务的版本瞬间退出检索。
// 逆改点：去掉 active 分支 → 版本被复位成 pending，断言红。
func TestKnowledgeStore_StartJobRefusesActiveVersion(t *testing.T) {
	h := newKnHarness(t, nil)
	first := h.startJob(t, "job_first", "runbook.md", "hash-a")
	h.publish(t, first.VersionID, "回滚步骤：停写入")

	_, err := h.store.StartJob(context.Background(), knowledge.StartJobRequest{
		JobID:          "job_again",
		CollectionID:   h.collection,
		SourcePath:     "runbook.md",
		ContentHash:    "hash-a",
		ParserVersion:  knowledge.ParserVersion,
		ChunkerVersion: "md-v1",
		At:             knNow,
	})
	require.ErrorIs(t, err, errKnowledgeStateConflict)

	var status string
	require.NoError(t, h.db.pool.QueryRowContext(context.Background(), `
		SELECT status FROM kb_document_versions WHERE version_id = $1`,
		first.VersionID).Scan(&status))
	require.Equal(t, string(knowledge.VersionActive), status, "生效版本不得被摄取复位")
}

// TestKnowledgeStore_StartJobRejectsDeletedCollection 判已软删集合不得再摄取。
// 逆改点：集合查询去掉 `deleted_at IS NULL` → 本用例拿到 nil error 而红。
func TestKnowledgeStore_StartJobRejectsDeletedCollection(t *testing.T) {
	h := newKnHarness(t, nil)
	require.NoError(t, h.store.SoftDeleteCollection(context.Background(), h.collection, knNow))

	_, err := h.store.StartJob(context.Background(), knowledge.StartJobRequest{
		JobID:          "job_deleted",
		CollectionID:   h.collection,
		SourcePath:     "runbook.md",
		ContentHash:    "hash-a",
		ParserVersion:  knowledge.ParserVersion,
		ChunkerVersion: "md-v1",
		At:             knNow,
	})
	require.ErrorIs(t, err, knowledge.ErrNotFound)
}

// TestKnowledgeStore_StartJobRevivesSoftDeletedDocument 判同路径重新摄取会复活软删
// 文档：`UNIQUE (collection_id, source_path)` 只允许一行，复活是唯一的复用方式。
// 逆改点：去掉复活分支 → 文档仍是 deleted，Document() 的 Deleted 断言红。
func TestKnowledgeStore_StartJobRevivesSoftDeletedDocument(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()

	first := h.startJob(t, "job_first", "runbook.md", "hash-a")
	require.NoError(t, h.store.SoftDeleteDocument(ctx, first.DocumentID, knNow))
	deleted, err := h.store.Document(ctx, first.DocumentID)
	require.NoError(t, err)
	require.True(t, deleted.Deleted)

	second := h.startJob(t, "job_second", "runbook.md", "hash-b")
	require.Equal(t, first.DocumentID, second.DocumentID)
	revived, err := h.store.Document(ctx, first.DocumentID)
	require.NoError(t, err)
	require.False(t, revived.Deleted, "重新摄取必须复活文档行")
}

// TestKnowledgeStore_WriteChunksRequiresPending 判片段只写得进 `pending` 版本：
// 已 indexed 的版本再写会覆盖正在校验的产物。
// 逆改点：去掉状态检查 → 第二次写入成功，本用例红。
func TestKnowledgeStore_WriteChunksRequiresPending(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	job := h.startJob(t, "job_first", "runbook.md", "hash-a")
	h.seedChunks(t, job.VersionID, "第一段")
	require.NoError(t, h.store.MarkIndexed(ctx, job.VersionID, knNow))

	_, err := h.store.WriteChunks(ctx, job.VersionID, []knowledge.ChunkDraft{{
		Seq: 1, CharStart: 0, CharEnd: 3, Text: "第二段", ContentHash: knHash("第二段"),
	}})
	require.ErrorIs(t, err, errKnowledgeStateConflict)

	var count int
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunks WHERE version_id = $1`, job.VersionID).Scan(&count))
	require.Equal(t, 1, count, "被拒的写入不得改动既有片段")
}

// TestKnowledgeStore_WriteChunksRewritesWithoutOrphans 判同一版本重复写片段是替换而非
// 追加：上一轮的片段与向量都要清掉，否则激活核对会数出多余的向量。
// 逆改点：去掉 DELETE → `UNIQUE (version_id, seq)` 报冲突，本用例红。
func TestKnowledgeStore_WriteChunksRewritesWithoutOrphans(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	job := h.startJob(t, "job_first", "runbook.md", "hash-a")

	first := h.seedChunks(t, job.VersionID, "第一段", "第二段")
	h.seedEmbeddings(t, first, 1)
	second := h.seedChunks(t, job.VersionID, "重写第一段", "重写第二段", "重写第三段")

	var chunks, embeddings int
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunks WHERE version_id = $1`, job.VersionID).Scan(&chunks))
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunk_embeddings e
		JOIN kb_chunks c ON c.chunk_id = e.chunk_id WHERE c.version_id = $1`,
		job.VersionID).Scan(&embeddings))
	require.Equal(t, 3, chunks)
	require.Zero(t, embeddings, "上一轮的向量必须随片段一起清掉")
	require.Len(t, second, 3)
}

// TestKnowledgeStore_ActivateSupersedesPrevious 判 `ActivateVersion` 是唯一生效点：
// 同一事务里新版本生效、旧 active 被取代、文档指向新版本。
// 逆改点：去掉 supersede 语句 → 部分唯一索引报冲突（两条 active），本用例红。
func TestKnowledgeStore_ActivateSupersedesPrevious(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()

	first := h.startJob(t, "job_first", "runbook.md", "hash-a")
	h.publish(t, first.VersionID, "回滚步骤：停写入")

	second := h.startJob(t, "job_second", "runbook.md", "hash-b")
	require.NotEqual(t, first.VersionID, second.VersionID)
	refs := h.seedChunks(t, second.VersionID, "回滚步骤：停写入并核对")
	h.seedEmbeddings(t, refs, 1)
	require.NoError(t, h.store.MarkIndexed(ctx, second.VersionID, knNow))
	superseded, err := h.store.ActivateVersion(ctx, second.VersionID, 1, knNow)
	require.NoError(t, err)
	require.Equal(t, first.VersionID, superseded, "必须返回被取代的版本")

	statuses := map[string]string{}
	rows, err := h.db.pool.QueryContext(ctx, `
		SELECT version_id, status FROM kb_document_versions WHERE document_id = $1`,
		first.DocumentID)
	require.NoError(t, err)
	for rows.Next() {
		var id, status string
		require.NoError(t, rows.Scan(&id, &status))
		statuses[id] = status
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, string(knowledge.VersionSuperseded), statuses[first.VersionID])
	require.Equal(t, string(knowledge.VersionActive), statuses[second.VersionID])

	var latest string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT latest_version_id FROM kb_documents WHERE document_id = $1`,
		first.DocumentID).Scan(&latest))
	require.Equal(t, second.VersionID, latest)
}

// TestKnowledgeStore_ActivateRefusesIncompleteArtifacts 判产物残缺不得生效：片段数
// 与该代际的向量数不等时拒绝激活，版本停在 indexed。
// 逆改点：删掉 requireCompleteArtifacts → 版本照样 active，本用例红。
func TestKnowledgeStore_ActivateRefusesIncompleteArtifacts(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	job := h.startJob(t, "job_first", "runbook.md", "hash-a")
	refs := h.seedChunks(t, job.VersionID, "第一段", "第二段")
	h.seedEmbeddings(t, refs[:1], 1)
	require.NoError(t, h.store.MarkIndexed(ctx, job.VersionID, knNow))

	_, err := h.store.ActivateVersion(ctx, job.VersionID, 1, knNow)
	require.ErrorIs(t, err, errKnowledgeArtifactsIncomplete)

	var status string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status FROM kb_document_versions WHERE version_id = $1`, job.VersionID).Scan(&status))
	require.Equal(t, string(knowledge.VersionIndexed), status, "激活失败不得改变版本状态")
}

// TestKnowledgeStore_FailVersionNeverTouchesActive 判失败路径不得把生效版本打下线。
// 逆改点：去掉 `status <> 'active'` 条件 → 版本变成 failed，本用例红。
func TestKnowledgeStore_FailVersionNeverTouchesActive(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	job := h.startJob(t, "job_first", "runbook.md", "hash-a")
	h.publish(t, job.VersionID, "回滚步骤")

	err := h.store.FailVersion(ctx, job.VersionID, knowledge.ErrorKindVerifyFailed, knNow)
	require.ErrorIs(t, err, errKnowledgeStateConflict)

	var status string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status FROM kb_document_versions WHERE version_id = $1`, job.VersionID).Scan(&status))
	require.Equal(t, string(knowledge.VersionActive), status)
}

// TestKnowledgeStore_SoftDeleteRetiresAndIsIdempotent 判删除的可见性后果与幂等：
// active 版本退出检索、重复删除不报错、文档行保留并标记已删。
// 逆改点：去掉 retire 语句 → 检索仍命中被删文档的片段，本用例红。
func TestKnowledgeStore_SoftDeleteRetiresAndIsIdempotent(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	job := h.startJob(t, "job_first", "runbook.md", "hash-a")
	h.publish(t, job.VersionID, "回滚步骤：停写入")

	hits, _, err := h.keyword.Search(ctx, knowledge.Scope{Collections: []string{h.collection}}, "回滚步骤", 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)

	require.NoError(t, h.store.SoftDeleteDocument(ctx, job.DocumentID, knNow))
	require.NoError(t, h.store.SoftDeleteDocument(ctx, job.DocumentID, knNow), "重复删除必须幂等")

	var status string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status FROM kb_document_versions WHERE version_id = $1`, job.VersionID).Scan(&status))
	require.Equal(t, string(knowledge.VersionRetired), status)

	hits, denied, err := h.keyword.Search(ctx, knowledge.Scope{Collections: []string{h.collection}}, "回滚步骤", 5)
	require.NoError(t, err)
	require.Empty(t, hits, "已删文档的片段不得再被召回")
	require.Equal(t, 1, denied, "被删片段只回计数")

	found, ok, err := h.store.FindActiveByContentHash(ctx, h.collection, "hash-a")
	require.NoError(t, err)
	require.False(t, ok, "已删文档不得命中去重")
	require.Empty(t, found.VersionID)
}

// TestKnowledgeStore_SoftDeleteCollectionRetiresVersions 判集合软删连带退役其下所有
// active 版本，且文档行不因此被改写成已删（删除原因由集合表达）。
// 逆改点：去掉集合路径的 retire 语句 → 版本仍 active，本用例红。
func TestKnowledgeStore_SoftDeleteCollectionRetiresVersions(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	job := h.startJob(t, "job_first", "runbook.md", "hash-a")
	h.publish(t, job.VersionID, "回滚步骤")

	require.NoError(t, h.store.SoftDeleteCollection(ctx, h.collection, knNow))
	require.NoError(t, h.store.SoftDeleteCollection(ctx, h.collection, knNow), "重复删除必须幂等")

	var status string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status FROM kb_document_versions WHERE version_id = $1`, job.VersionID).Scan(&status))
	require.Equal(t, string(knowledge.VersionRetired), status)
}

// TestKnowledgeStore_KeywordSearchFiltersInsideSQL 判关键词路的可见性过滤在 SQL 内
// 生效：范围外的集合即使匹配也不回内容，只回计数。
// 逆改点：把过滤移出 SQL（返回后再筛）→ hits 内容不空或 denied 为 0，本用例红。
func TestKnowledgeStore_KeywordSearchFiltersInsideSQL(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	knSeedCollection(t, h.db, "col_other", "other")

	inner := h.startJob(t, "job_inner", "runbook.md", "hash-a")
	h.publish(t, inner.VersionID, "清理步骤：先摘流量")
	other, err := h.store.StartJob(ctx, knowledge.StartJobRequest{
		JobID:          "job_other",
		CollectionID:   "col_other",
		SourcePath:     "secret.md",
		ContentHash:    "hash-b",
		ParserVersion:  knowledge.ParserVersion,
		ChunkerVersion: "md-v1",
		At:             knNow,
	})
	require.NoError(t, err)
	h.publish(t, other.VersionID, "清理步骤：删除生产库")

	hits, denied, err := h.keyword.Search(ctx, knowledge.Scope{Collections: []string{h.collection}},
		"清理步骤", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, h.collection, hits[0].CollectionID)
	require.Equal(t, 1, denied, "范围外命中只回计数，不回标识")
}

// TestKnowledgeStore_VectorSearchPinsGeneration 判向量路只读调用方固定的一代（C-09）：
// 另一代际有向量也不会被读到。
// 逆改点：去掉 `e.generation = $2` → 未写向量的代际也回结果，本用例红。
func TestKnowledgeStore_VectorSearchPinsGeneration(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	job := h.startJob(t, "job_first", "runbook.md", "hash-a")
	refs := h.seedChunks(t, job.VersionID, "回滚步骤")
	h.seedEmbeddings(t, refs, 1)
	require.NoError(t, h.store.MarkIndexed(ctx, job.VersionID, knNow))
	_, err := h.store.ActivateVersion(ctx, job.VersionID, 1, knNow)
	require.NoError(t, err)

	scope := knowledge.Scope{Collections: []string{h.collection}}
	hits, _, err := h.vectors.Search(ctx, scope, knVector(0.2), 1, 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)

	hits, denied, err := h.vectors.Search(ctx, scope, knVector(0.2), 2, 5)
	require.NoError(t, err)
	require.Empty(t, hits, "未写向量的代际不得回命中")
	require.Zero(t, denied)
}

// TestKnowledgeStore_LoadVersionReturnsExactSourceText 判来源视图的**逐字回读**：
// 版本原文（标题行、空行、尾部换行都在内）必须原样回来，且返回正文的 sha256
// 必须等于该版本的 `content_hash`——否则来源视图就是拿"重建正文"冒充原文。
// 逆改点：`versionText` 不优先用 `source_text` 快照、总走重建 → 标题行与尾部换行
// 丢失、正文回空，本用例红（实测见 run-state R122 ·3）。
func TestKnowledgeStore_LoadVersionReturnsExactSourceText(t *testing.T) {
	const original = "# 运维手册\n\n第一段内容。\n\n## 备份\n\n第二段内容。\n"
	h := newKnHarness(t, map[string]knowledge.DocumentSource{
		"runbook.md": knDoc("runbook.md", original),
	})
	ctx := context.Background()
	res := h.ingest(t, "runbook.md")
	require.Equal(t, knowledge.VersionActive, res.Status)

	record, err := h.store.LoadVersion(ctx, res.DocumentID, res.VersionID)
	require.NoError(t, err)
	require.Equal(t, res.DocumentID, record.DocumentID)
	require.Equal(t, res.VersionID, record.VersionID)
	require.Equal(t, original, record.Text,
		"版本原文必须逐字回读：标题行、空行与尾部换行都不许丢")
	require.Equal(t, knHash(record.Text), record.ContentHash,
		"返回正文的 sha256 必须等于版本的 content_hash")
	require.Equal(t, "runbook", record.Summary.DocumentTitle)
	require.False(t, record.DocumentDeleted)
}

// TestKnowledgeStore_LoadVersionReturnsExactTextAcrossChunkBoundaries 判多片段文档：
// 切分窗口、同节重叠与节间空隙都不得改写原文——拼接处一旦错位，窗口读取就会取到
// 别的段落。
// 逆改点：`versionText` 跳过 `source_text` 快照、总走重建 → 重建结果与原文不符、
// 正文回空，本用例红（实测见 run-state R122 ·3）。"按 seq 直接拼接"这一逆改本用例
// 判不出（快照短路了重建），由 `TestKnowledgeStore_LoadVersionRebuildsTextAtAbsoluteOffsets`
// 判卷。
func TestKnowledgeStore_LoadVersionReturnsExactTextAcrossChunkBoundaries(t *testing.T) {
	body := strings.Repeat("备份窗口说明：每日执行全量备份。", 30)
	original := "# 备份手册\n\n" + body + "\n\n## 尾部\n\n最后一行。\n"
	h := newKnHarness(t, map[string]knowledge.DocumentSource{
		"long.md": knDoc("long.md", original),
	})
	ctx := context.Background()
	res := h.ingest(t, "long.md")
	require.Equal(t, knowledge.VersionActive, res.Status)
	require.Greater(t, res.ChunkCount, 1, "夹具必须真的跨多个片段，否则判别力不成立")

	record, err := h.store.LoadVersion(ctx, res.DocumentID, res.VersionID)
	require.NoError(t, err)
	require.Equal(t, original, record.Text, "多片段拼接后仍必须与原文逐字相同")
	require.Equal(t, knHash(record.Text), record.ContentHash)
}

// TestKnowledgeStore_LoadVersionRebuildsTextAtAbsoluteOffsets 判**没有原文快照**时
// （修复前入库 / 迁移导入的行）`LoadVersion` 的正文由片段按 `char_start/char_end`
// 的**绝对 rune 偏移**写回原位，而不是按 `seq` 顺序拼接：
//
//  1. **重叠只算一次**：同一节内相邻片段按窗口回退重叠（切分器的真实产物），写回时
//     后写覆盖同一段——按 seq 拼接会把重叠段写两遍，长度与哈希都对不上；
//  2. **空隙按 `'\n'` 补位**：被切分器裁掉、长度恰为 1 的空隙由 `reservedGapRune`
//     补成换行，重建结果因此逐字等于原文。
//
// 两段都以"重建结果的 sha256 等于 `content_hash` 才回正文"收口：偏移错了正文就回空。
// 逆改点：`loadVersionText` 改成按 `seq` 顺序拼接 → 第 1 段重建出重叠两遍、哈希不符，
// 正文回空，红；空隙填充字符从 `'\n'` 换成别的字符 → 第 2 段哈希不符，红。
// 验收点：A5（绝对 rune 偏移重建）/ FR-053 / 来源视图"不拿近似文本冒充原文"。
func TestKnowledgeStore_LoadVersionRebuildsTextAtAbsoluteOffsets(t *testing.T) {
	// 第 1 段：无空白的定长正文，切分器按窗口硬切并回退 20 个字符重叠；原文落库的
	// `source_text` 快照随后清空，逼 `LoadVersion` 走重建路径（老行的形态）。
	original := strings.Repeat("备份窗口说明：每日执行全量备份。", 30)
	h := newKnHarness(t, map[string]knowledge.DocumentSource{
		"manual.md": knDoc("manual.md", original),
	})
	ctx := context.Background()
	res := h.ingest(t, "manual.md")
	require.Equal(t, knowledge.VersionActive, res.Status)
	require.Greater(t, res.ChunkCount, 1, "夹具必须真的跨多个片段，否则判别力不成立")

	// 片段之间确有重叠：没有重叠时"按 seq 拼接"也能重建出原文，本用例就失去判别力。
	var overlaps int
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunks a
		JOIN kb_chunks b ON b.version_id = a.version_id AND b.seq = a.seq + 1
		WHERE a.version_id = $1 AND b.char_start < a.char_end`, res.VersionID).Scan(&overlaps))
	require.Positive(t, overlaps, "夹具必须产生重叠片段（切分器同节回退），否则本用例判不出偏移")

	_, err := h.db.pool.ExecContext(ctx, `
		UPDATE kb_document_versions SET source_text = NULL WHERE version_id = $1`, res.VersionID)
	require.NoError(t, err)

	record, err := h.store.LoadVersion(ctx, res.DocumentID, res.VersionID)
	require.NoError(t, err)
	require.Equal(t, original, record.Text,
		"按绝对偏移重建必须逐字还原原文：重叠段只能出现一次，空隙不得吃掉字符")
	require.Equal(t, knHash(record.Text), record.ContentHash)

	// 第 2 段：两片之间恰好空出一个 rune（被裁掉的换行），填充字符必须是 `'\n'`，
	// 否则重建结果与原文只差一个字符、哈希校验就会把它判成"未保留"。
	const gapped = "第一段\n第二段"
	job := h.startJob(t, "job_gap", "gapped.md", knHash(gapped))
	_, err = h.store.WriteChunks(ctx, job.VersionID, []knowledge.ChunkDraft{
		{Seq: 1, CharStart: 0, CharEnd: 3, Text: "第一段", ContentHash: knHash("第一段")},
		{Seq: 2, CharStart: 4, CharEnd: 7, Text: "第二段", ContentHash: knHash("第二段")},
	})
	require.NoError(t, err)

	record, err = h.store.LoadVersion(ctx, job.DocumentID, job.VersionID)
	require.NoError(t, err)
	require.Equal(t, gapped, record.Text, "单 rune 空隙必须由 '\\n' 补位后才与原文逐字相同")
	require.Equal(t, knHash(gapped), record.ContentHash)
}

// TestKnowledgeStore_LoadVersionWithoutRetainedTextServesNoBody 判**没有原文快照**的
// 版本（修复前入库 / 迁移导入）：按片段能精确重建时照常回正文；重建不出来时
// 必须回空正文，**不得**用补换行的近似文本冒充原文。
// 逆改点：去掉读时哈希校验、直接返回重建结果 → 第二个用例拿到"第一段\n\n\n\n第二段"，红。
func TestKnowledgeStore_LoadVersionWithoutRetainedTextServesNoBody(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()

	// 片段连续覆盖全文：重建结果与原文逐字相同，可以当原文用。
	const contiguous = "第一段第二段"
	exact := h.startJob(t, "job_exact", "contiguous.md", knHash(contiguous))
	_, err := h.store.WriteChunks(ctx, exact.VersionID, []knowledge.ChunkDraft{
		{Seq: 1, CharStart: 0, CharEnd: 3, Text: "第一段", ContentHash: knHash("第一段")},
		{Seq: 2, CharStart: 3, CharEnd: 6, Text: "第二段", ContentHash: knHash("第二段")},
	})
	require.NoError(t, err)
	record, err := h.store.LoadVersion(ctx, exact.DocumentID, exact.VersionID)
	require.NoError(t, err)
	require.Equal(t, contiguous, record.Text, "能精确重建时必须回正文")
	require.Equal(t, knowledge.VersionPending, record.Status)

	// 片段之间有被裁掉的标题行：重建结果与原文不同，必须回空正文。
	const original = "第一段\n# 标题\n第二段"
	lossy := h.startJob(t, "job_lossy", "lossy.md", knHash(original))
	_, err = h.store.WriteChunks(ctx, lossy.VersionID, []knowledge.ChunkDraft{
		{Seq: 1, CharStart: 0, CharEnd: 3, Text: "第一段", ContentHash: knHash("第一段")},
		{Seq: 2, CharStart: 7, CharEnd: 10, Text: "第二段", ContentHash: knHash("第二段")},
	})
	require.NoError(t, err)
	record, err = h.store.LoadVersion(ctx, lossy.DocumentID, lossy.VersionID)
	require.NoError(t, err)
	require.Empty(t, record.Text,
		"重建结果与 content_hash 不符时不得回正文——近似文本不能冒充原文")

	_, err = h.store.LoadVersion(ctx, lossy.DocumentID, "kver_missing")
	require.ErrorIs(t, err, knowledge.ErrNotFound)
}

// TestKnowledgeStore_SourceViewVerbatimAndRangeUseExactOffsets 在真库上跑通来源视图：
// 整段读取标 `verbatim` 且正文与哈希同原文一致；范围读取按**原文**的字符偏移取窗口，
// 落在标题行与空行上也不会错位；没有原文快照的版本不出正文、也不出哈希。
// 逆改点：Conversion 恒为 verbatim → 第三个用例（无正文）仍报 verbatim，红；
// 范围按重建正文的偏移切 → 第二段取到的是标题行换成的空行，红。
func TestKnowledgeStore_SourceViewVerbatimAndRangeUseExactOffsets(t *testing.T) {
	const original = "# 运维手册\n\n第一段内容。\n\n## 备份\n\n第二段内容。\n"
	h := newKnHarness(t, map[string]knowledge.DocumentSource{
		"runbook.md": knDoc("runbook.md", original),
	})
	ctx := context.Background()
	res := h.ingest(t, "runbook.md")
	require.Equal(t, knowledge.VersionActive, res.Status)
	viewer := knowledge.NewSourceViewer(knowledge.SourceViewerDeps{
		Reader: h.store,
		Authz:  knFakeAuthz{collections: []string{h.collection}},
		Now:    func() time.Time { return knNow },
	})
	ref := knowledge.EvidenceRef{Kind: "knowledge_document", ID: res.DocumentID, VersionID: res.VersionID}

	full, err := viewer.SourceView(ctx, h.principal, knowledge.SourceViewRequest{Ref: ref})
	require.NoError(t, err)
	require.Equal(t, original, full.Text, "整段读取必须回原文")
	require.Equal(t, knHash(original), full.ContentHash, "内容哈希必须与原文一致")
	require.Equal(t, "verbatim", full.Conversion)
	require.False(t, full.Truncated)

	// 范围读取：窗口按原文的字符偏移取，跨过标题行也不得错位。
	runes := []rune(original)
	start, end := 0, 0
	for i := 0; i+3 <= len(runes); i++ {
		if string(runes[i:i+3]) == "第二段" {
			start, end = i, i+3
			break
		}
	}
	require.Positive(t, end, "夹具必须找得到窗口起点")
	window, err := viewer.SourceView(ctx, h.principal, knowledge.SourceViewRequest{
		Ref:   ref,
		Range: &knowledge.EvidenceRange{Start: start, End: end},
	})
	require.NoError(t, err)
	require.Equal(t, "第二段", window.Text, "窗口必须落在原文坐标上，而不是重建正文的坐标上")
	require.Equal(t, knowledge.EvidenceRange{Start: start, End: end}, window.Range)
	require.Equal(t, "char_window", window.Conversion)

	// 没有原文快照且重建不出来的版本：只回状态，不回正文、不回哈希。
	legacy := h.startJob(t, "job_legacy", "legacy.md", knHash("第一段\n# 标题\n第二段"))
	legacyRefs, err := h.store.WriteChunks(ctx, legacy.VersionID, []knowledge.ChunkDraft{
		{Seq: 1, CharStart: 0, CharEnd: 3, Text: "第一段", ContentHash: knHash("第一段")},
		{Seq: 2, CharStart: 7, CharEnd: 10, Text: "第二段", ContentHash: knHash("第二段")},
	})
	require.NoError(t, err)
	h.seedEmbeddings(t, legacyRefs, 1)
	require.NoError(t, h.store.MarkIndexed(ctx, legacy.VersionID, knNow))
	_, err = h.store.ActivateVersion(ctx, legacy.VersionID, 1, knNow)
	require.NoError(t, err)

	noBody, err := viewer.SourceView(ctx, h.principal, knowledge.SourceViewRequest{
		Ref: knowledge.EvidenceRef{
			Kind: "knowledge_document", ID: legacy.DocumentID, VersionID: legacy.VersionID,
		},
	})
	require.NoError(t, err)
	require.True(t, noBody.Validity.Valid, "引用本身有效——只是正文没有保留")
	require.Empty(t, noBody.Text, "没有精确原文时不得回近似正文")
	require.Empty(t, noBody.ContentHash, "没有正文时不得回内容哈希")
	require.Empty(t, noBody.Conversion, "没有正文时不得声称任何转换策略")
}

// TestKnowledgeStore_SourceViewDeniesCrossProjectCaller 判**越权读取**在**真库 ACL** 上
// 被拒：集合归属 `team-a`，`team-b` 的调用方精确点名同一 `EvidenceRef` 也拿不到正文与
// 哈希，拒绝信息 / 拒绝日志都不带集合标识、来源路径或正文（C-08）；同项目的调用方读到
// 逐字原文；把调用方停用后即便项目匹配也按未认证拒绝。
//
// 补的是 `TestKnowledgeStore_SourceViewVerbatimAndRangeUseExactOffsets` 的缺口：那条用
// `knFakeAuthz` 假 ACL（只判"集合在不在白名单"），而项目归属判定发生在
// `kb_collections` 的 SQL 里、调用方项目来自 `callers` 行——这两件只有真库能判。
// 逆改点：删掉 `visibleCollections` 的 `AND ($2 = ” OR project = $2)` → outsider 读到
// 正文，`ErrorIs(ErrForbidden)` 与空正文断言双红；把 ACL 重查挪到 `judgeValidity`
// 之后（先组正文再判权）→ outsider 的 Text 非空，红；去掉 `lookupCallerProject` 的
// 停用判定 → 停用后的读取改为成功，`ErrorIs(core.ErrUnauthorized)` 红。
func TestKnowledgeStore_SourceViewDeniesCrossProjectCaller(t *testing.T) {
	const original = "# 备份手册\n\n备份目录为 /var/backups。\n"
	h := newKnHarness(t, map[string]knowledge.DocumentSource{
		"runbook.md": knDoc("runbook.md", original),
	})
	ctx := context.Background()
	res := h.ingest(t, "runbook.md")
	require.Equal(t, knowledge.VersionActive, res.Status)

	// 集合改归 team-a；两个调用方分属 team-a / team-b——可见范围按 `callers.project`
	// **现读**，不是 Principal 自带的标签。
	_, err := h.db.pool.ExecContext(ctx,
		`UPDATE kb_collections SET project = 'team-a' WHERE collection_id = $1`, h.collection)
	require.NoError(t, err)
	seedCaller := func(callerID, project string) {
		t.Helper()
		_, err := h.db.pool.ExecContext(ctx, `
			INSERT INTO callers (caller_id, kind, name, project, status, created_at)
			VALUES ($1, 'service', $2, $3, 'active', $4)`,
			callerID, "svc-"+project, project, knNow.UnixMilli())
		require.NoError(t, err)
	}
	const insiderID = "cal_0000000000000000000000acl1"
	const outsiderID = "cal_0000000000000000000000acl2"
	seedCaller(insiderID, "team-a")
	seedCaller(outsiderID, "team-b")

	logger, capture := recCaptureLogger()
	viewer := knowledge.NewSourceViewer(knowledge.SourceViewerDeps{
		Reader: h.store,
		Authz:  NewKnowledgeAuthz(h.db, nil),
		Logger: logger,
		Now:    func() time.Time { return knNow },
	})
	ref := knowledge.EvidenceRef{Kind: "knowledge_document", ID: res.DocumentID, VersionID: res.VersionID}
	caller := func(callerID string) core.Principal {
		return core.Principal{
			Kind:     core.PrincipalCaller,
			CallerID: callerID,
			KeyID:    "key_" + callerID,
			Scopes:   []string{core.ScopeKnowledgeRead},
			Actor:    core.ActorCaller,
		}
	}

	denied, err := viewer.SourceView(ctx, caller(outsiderID), knowledge.SourceViewRequest{
		Ref: ref, IncludeFacts: true, IncludeSummary: true,
	})
	require.ErrorIs(t, err, core.ErrForbidden, "跨项目读取必须被拒，不是回空结果")
	require.Empty(t, denied.Text, "拒绝时不得返回任何正文")
	require.Empty(t, denied.ContentHash, "拒绝时不得返回内容哈希")
	require.Empty(t, denied.Facts, "拒绝时不得返回结构化事实")
	require.Equal(t, knowledge.SourceSummary{}, denied.Summary, "拒绝时不得返回摘要")
	for _, probe := range []string{h.collection, "runbook.md", "/var/backups", "备份手册"} {
		require.NotContains(t, err.Error(), probe, "拒绝信息不得泄露被拒集合名 / 路径 / 正文")
		require.NotContains(t, capture.text(), probe, "拒绝日志不得泄露被拒集合名 / 路径 / 正文")
	}

	allowed, err := viewer.SourceView(ctx, caller(insiderID), knowledge.SourceViewRequest{Ref: ref})
	require.NoError(t, err, "同项目的调用方必须读到同一引用")
	require.True(t, allowed.Validity.Valid)
	require.Equal(t, original, allowed.Text)
	require.Equal(t, knHash(original), allowed.ContentHash)

	// 撤权：停用后即使项目匹配也按未认证拒绝（与 Key 认证「停用即未找到」同一口径）。
	_, err = h.db.pool.ExecContext(ctx, `
		UPDATE callers SET status = 'disabled', project = 'team-a' WHERE caller_id = $1`, outsiderID)
	require.NoError(t, err)
	_, err = viewer.SourceView(ctx, caller(outsiderID), knowledge.SourceViewRequest{Ref: ref})
	require.ErrorIs(t, err, core.ErrUnauthorized, "停用的调用方不得因项目匹配而放行")
}

// TestKnowledgeStore_LoadVersionReportsSoftDeletion 判来源读取**不隐藏**删除状态：
// 被删文档照常读回行并标记已删，由上层判定失效原因。
// 逆改点：给查询加 `deleted_at IS NULL` → 返回 ErrNotFound，本用例红。
func TestKnowledgeStore_LoadVersionReportsSoftDeletion(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	// 内容哈希必须是片段正文的 sha256：本用例只造片段、不落原文快照，正文靠重建
	// 回来，重建结果与 `content_hash` 不符时存储会按"未保留"回空正文。
	job := h.startJob(t, "job_first", "runbook.md", knHash("回滚步骤"))
	h.publish(t, job.VersionID, "回滚步骤")
	require.NoError(t, h.store.SoftDeleteDocument(ctx, job.DocumentID, knNow))

	record, err := h.store.LoadVersion(ctx, job.DocumentID, job.VersionID)
	require.NoError(t, err)
	require.True(t, record.DocumentDeleted)
	require.False(t, record.DeletedAt.IsZero(), "已删文档必须带回删除时刻")
	require.Equal(t, "回滚步骤", record.Text)
}

// TestKnowledgeStore_FindActiveByContentHashScopesToCollection 守去重查询的两个边界：
// 跨**集合**不得命中（集合是去重范围），跨**文档**必须命中（去重不看 document_id），
// 非 active 版本不得命中。
// 逆改点：删掉 `d.collection_id=$1` → 第一条断言变 ok=true；给查询补 document_id 条件
// → 第二条断言变 ok=false；放宽 `v.status='active'` → 第三条断言变 ok=true。
func TestKnowledgeStore_FindActiveByContentHashScopesToCollection(t *testing.T) {
	h := newKnHarness(t, nil)
	ctx := context.Background()
	knSeedCollection(t, h.db, "col_other", "other-runbooks")

	startIn := func(collectionID, jobID, path, hash string) knowledge.JobRecord {
		t.Helper()
		job, err := h.store.StartJob(ctx, knowledge.StartJobRequest{
			JobID:          jobID,
			CollectionID:   collectionID,
			SourcePath:     path,
			ContentHash:    hash,
			ParserVersion:  knowledge.ParserVersion,
			ChunkerVersion: "md-v1",
			At:             knNow,
		})
		require.NoError(t, err)
		return job
	}

	// 别的集合持有该内容的 active 版本：本集合去重不得命中。
	other := startIn("col_other", "job_other", "shared.md", "hash-shared")
	h.publish(t, other.VersionID, "共享内容")

	found, ok, err := h.store.FindActiveByContentHash(ctx, h.collection, "hash-shared")
	require.NoError(t, err)
	require.False(t, ok, "去重范围是集合：别的集合的 active 版本不得被本集合命中")
	require.Empty(t, found.VersionID)

	// 本集合内另一文档持有同哈希 active 版本：必须命中并返回那一条。
	local := startIn(h.collection, "job_local", "mirror.md", "hash-shared")
	h.publish(t, local.VersionID, "共享内容")

	found, ok, err = h.store.FindActiveByContentHash(ctx, h.collection, "hash-shared")
	require.NoError(t, err)
	require.True(t, ok, "同集合内另一文档的同内容 active 版本必须命中去重")
	require.Equal(t, local.VersionID, found.VersionID)
	require.Equal(t, local.DocumentID, found.DocumentID, "命中返回的是持行文档，不是查询方")

	// 只有 pending 版本的哈希不得命中。
	startIn(h.collection, "job_pending", "pending.md", "hash-pending")
	found, ok, err = h.store.FindActiveByContentHash(ctx, h.collection, "hash-pending")
	require.NoError(t, err)
	require.False(t, ok, "未生效版本不得被去重命中")
	require.Empty(t, found.VersionID)
}

// 保留历史测试名供 stage 映射；当前判据是恢复时保留退役历史版本、新建版本并复活
// 同一文档行。把旧版本原地重写、建新文档或将 retired 当作去重命中都会失败。
func TestIngest_ReusesRetiredVersionRowAfterDocumentSoftDelete(t *testing.T) {
	h := newKnHarness(t, map[string]knowledge.DocumentSource{
		"runbook.md": knDoc("runbook.md", "# 回滚手册\n\n回滚步骤：先停写入，再回放日志。\n"),
	})
	ctx := context.Background()

	first := h.ingest(t, "runbook.md")
	require.Equal(t, knowledge.VersionActive, first.Status)
	require.False(t, first.Deduplicated)
	require.NoError(t, h.store.SoftDeleteDocument(ctx, first.DocumentID, knNow))

	second := h.ingest(t, "runbook.md")
	require.Equal(t, knowledge.VersionActive, second.Status)
	require.False(t, second.Deduplicated, "没有 active 版本时不得报去重")
	require.Equal(t, first.DocumentID, second.DocumentID, "同路径必须复活同一条文档行")
	require.NotEqual(t, first.VersionID, second.VersionID, "恢复时不得重写已退役的历史版本")

	var attempts int
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_ingest_jobs WHERE version_id = $1`,
		first.VersionID).Scan(&attempts))
	require.Equal(t, 1, attempts, "历史版本的原作业必须保留")
	var oldStatus string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `SELECT status FROM kb_document_versions WHERE version_id = $1`, first.VersionID).Scan(&oldStatus))
	require.Equal(t, string(knowledge.VersionRetired), oldStatus, "历史版本继续保留退役状态")

	var revived bool
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT deleted_at IS NULL FROM kb_documents WHERE document_id = $1`,
		first.DocumentID).Scan(&revived))
	require.True(t, revived, "摄取必须复活软删文档行（重建索引不会）")
}

// TestIngest_FailedNewVersionKeepsPreviousServing 是场景 3 的失败分支判据：内容变化后
// 新版本摄取中途失败，旧 active 必须继续服务——它一行都不该被动到。
// 逆改点：失败路径顺手退役旧 active（或提前激活新版本）→ 状态与召回断言同时红。
func TestIngest_FailedNewVersionKeepsPreviousServing(t *testing.T) {
	docs := map[string]knowledge.DocumentSource{
		"runbook.md": knDoc("runbook.md", "# 回滚手册\n\n回滚步骤：先停写入，再回放日志。\n"),
	}
	embedder := &knToggleEmbedder{}
	h := newKnHarnessWith(t, docs, embedder)
	ctx := context.Background()

	old := h.ingest(t, "runbook.md")
	require.Equal(t, knowledge.VersionActive, old.Status)
	before, err := h.retriever.Retrieve(ctx, h.query("回滚步骤"))
	require.NoError(t, err)
	require.True(t, containsVersion(before.Hits, old.VersionID), "前置条件：旧版本此前可召回")

	// 内容变了，而这次的 embedding 必然失败。
	docs["runbook.md"] = knDoc("runbook.md", "# 回滚手册\n\n回滚步骤：先停写入，再核对计数。\n")
	embedder.failNow()
	failed, err := h.ingestor.Ingest(ctx, knowledge.IngestRequest{
		Principal:    h.principal,
		CollectionID: h.collection,
		Path:         "runbook.md",
	})
	require.NoError(t, err, "摄取失败是 200 终态，不是传输层错误")
	require.Equal(t, knowledge.VersionFailed, failed.Status)
	require.NotEqual(t, old.VersionID, failed.VersionID, "内容变化必须新建版本行")

	var oldStatus, newStatus string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status FROM kb_document_versions WHERE version_id = $1`,
		old.VersionID).Scan(&oldStatus))
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status FROM kb_document_versions WHERE version_id = $1`,
		failed.VersionID).Scan(&newStatus))
	require.Equal(t, string(knowledge.VersionActive), oldStatus, "旧生效版本不得被失败路径动到")
	require.Equal(t, string(knowledge.VersionFailed), newStatus)

	after, err := h.retriever.Retrieve(ctx, h.query("回滚步骤"))
	require.NoError(t, err)
	require.True(t, containsVersion(after.Hits, old.VersionID), "失败后旧版本必须继续被召回")
	require.False(t, containsVersion(after.Hits, failed.VersionID), "失败版本不得被召回")
}

// TestIngest_ReindexChangedContentCreatesNewVersion 是场景 4 的「内容变化」分支：
// 重建索引对改动过的文件必须新建版本行并取代旧 active。
// 逆改点：改用文档行的 `content_hash`（只在激活时更新）做去重 → 旧哈希命中既有版本，
// 结果 Deduplicated=true 且版本号不变，断言红。
func TestIngest_ReindexChangedContentCreatesNewVersion(t *testing.T) {
	docs := map[string]knowledge.DocumentSource{
		"docs/b.txt": knDoc("docs/b.txt", "回滚步骤：先停写入。\n"),
	}
	h := newKnHarness(t, docs)
	ctx := context.Background()

	old := h.ingest(t, "docs/b.txt")
	require.Equal(t, knowledge.VersionActive, old.Status)

	// 磁盘上的内容变了：重建索引必须按新内容走，而不是按文档行的旧哈希。
	docs["docs/b.txt"] = knDoc("docs/b.txt", "回滚步骤：先停写入，再回放日志。\n")
	fresh, err := h.ingestor.Reindex(ctx, knowledge.ReindexRequest{
		Principal:  h.principal,
		DocumentID: old.DocumentID,
	})
	require.NoError(t, err)
	require.False(t, fresh.Deduplicated, "内容变了不得报去重")
	require.NotEqual(t, old.VersionID, fresh.VersionID, "内容变化必须新建版本行")
	require.Equal(t, knowledge.VersionActive, fresh.Status)
	require.Equal(t, old.DocumentID, fresh.DocumentID)

	var oldStatus string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT status FROM kb_document_versions WHERE version_id = $1`,
		old.VersionID).Scan(&oldStatus))
	require.Equal(t, string(knowledge.VersionSuperseded), oldStatus, "新版本生效后旧版本必须被取代")

	var versions int
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_document_versions WHERE document_id = $1`,
		old.DocumentID).Scan(&versions))
	require.Equal(t, 2, versions, "内容变化只该多出一条版本行")
}

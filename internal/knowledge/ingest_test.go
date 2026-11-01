package knowledge

// 本文件覆盖 T034 的摄取状态机（图 3）：激活顺序、去重、审计 fail-closed、
// 配额三态（Commit / Release / 保持 pending）、失败版本不生效、准入期零写入。
// 全部用窄端口假件，不触网、不连库；真实 PG 路径的用例在 T045 的
// `knowledge_store_test.go`（`TestIngest_NewVersionInvisibleUntilIndexed`）。
//
// This file drives figure 3 through fake ports: activation order, dedup, audit
// fail-closed, the three quota outcomes, and admission-time zero writes.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// testLogger 是"只求不响"的日志面（沿 memory 包同名约定：丢弃级别，判 WARN 的
// 用例另建捕获面）。
// testLogger is the silent log surface used across package tests.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testPrincipal 是持有 `knowledge:write` 的调用方身份。
// testPrincipal is a caller holding the knowledge write scope.
func testPrincipal() core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller_a",
		KeyID:    "key_a1",
		Scopes:   []string{core.ScopeKnowledgeWrite},
		Actor:    core.ActorCaller,
	}
}

// testConfig 是自洽的摄取配置：维度与冻结列一致，批大小 2（三条片段 → 两批）。
// testConfig is a coherent ingest config with batch size two.
func testConfig() IngestConfig {
	return IngestConfig{
		Generation:          1,
		EmbeddingModel:      "text-embedding-v4",
		EmbeddingDimensions: VectorDimensions,
		EmbeddingBatchSize:  2,
		MaxDocumentBytes:    1 << 20,
		AllowedExtensions:   []string{".md", ".txt"},
		ChunkerVersion:      "v1",
	}
}

// sha256Hex 是测试侧独立的片段哈希（与生产 `hashText` 分开写，生产改了形态这里会红）。
// sha256Hex is an independent hash so production drift breaks the fixture.
func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// sampleChunks 返回三条连续片段，第二条带标题路径。
// sampleChunks returns three numbered drafts with a heading path on the second.
func sampleChunks() []ChunkDraft {
	texts := []string{"第一段正文", "第二段正文", "第三段正文"}
	chunks := make([]ChunkDraft, len(texts))
	start := 0
	for idx, text := range texts {
		width := len([]rune(text))
		chunks[idx] = ChunkDraft{
			Seq:         idx + 1,
			CharStart:   start,
			CharEnd:     start + width,
			Text:        text,
			ContentHash: sha256Hex(text),
		}
		start += width
	}
	chunks[1].HeadingPath = "A > B"
	return chunks
}

// unitVectors 生成 count 条指定宽度的非零向量。
// unitVectors builds non-zero vectors of the requested width.
func unitVectors(count, width int) [][]float32 {
	vectors := make([][]float32, count)
	for idx := range vectors {
		row := make([]float32, width)
		for col := range row {
			row[col] = 0.5
		}
		vectors[idx] = row
	}
	return vectors
}

// fakeStore 实现 `IngestStore`：默认行为模拟一次成功开作业，钩子可替换单个方法。
// fakeStore implements IngestStore with per-method hooks and a shared event log.
type fakeStore struct {
	events *[]string

	findActive  func(ctx context.Context, collectionID, contentHash string) (VersionRecord, bool, error)
	startJob    func(ctx context.Context, req StartJobRequest) (JobRecord, error)
	writeChunks func(ctx context.Context, versionID string, chunks []ChunkDraft) ([]ChunkRef, error)
	markIndexed func(ctx context.Context, versionID string, at time.Time) error
	activate    func(ctx context.Context, versionID string, generation int, at time.Time) (string, error)
	failVersion func(ctx context.Context, versionID, errorKind string, at time.Time) error
	finishJob   func(ctx context.Context, jobID, stage, status, errorKind string, at time.Time) error
	document    func(ctx context.Context, documentID string) (DocumentRecord, error)
	deleteDoc   func(ctx context.Context, documentID string, at time.Time) error
	deleteColl  func(ctx context.Context, collectionID string, at time.Time) error

	startReqs   []StartJobRequest
	written     []ChunkDraft
	failReasons []string
	jobOutcomes []string
	activated   []string
	deletedDocs []string
	deletedColl []string
	docLookups  []string
	findHashes  []string
}

func (f *fakeStore) record(event string) {
	if f.events != nil {
		*f.events = append(*f.events, event)
	}
}

func (f *fakeStore) FindActiveByContentHash(_ context.Context, collectionID, contentHash string) (VersionRecord, bool, error) {
	f.findHashes = append(f.findHashes, collectionID+"/"+contentHash)
	if f.findActive != nil {
		return f.findActive(context.Background(), collectionID, contentHash)
	}
	return VersionRecord{}, false, nil
}

func (f *fakeStore) StartJob(_ context.Context, req StartJobRequest) (JobRecord, error) {
	f.record("start_job")
	f.startReqs = append(f.startReqs, req)
	if f.startJob != nil {
		return f.startJob(context.Background(), req)
	}
	return JobRecord{JobID: req.JobID, DocumentID: "doc_1", VersionID: "ver_1", Attempt: 1}, nil
}

func (f *fakeStore) WriteChunks(_ context.Context, versionID string, chunks []ChunkDraft) ([]ChunkRef, error) {
	f.record("write_chunks")
	f.written = append(f.written, chunks...)
	if f.writeChunks != nil {
		return f.writeChunks(context.Background(), versionID, chunks)
	}
	refs := make([]ChunkRef, len(chunks))
	for idx, chunk := range chunks {
		refs[idx] = ChunkRef{ChunkID: fmt.Sprintf("chk_%d", chunk.Seq), Seq: chunk.Seq}
	}
	return refs, nil
}

func (f *fakeStore) MarkIndexed(_ context.Context, versionID string, at time.Time) error {
	f.record("mark_indexed")
	if f.markIndexed != nil {
		return f.markIndexed(context.Background(), versionID, at)
	}
	return nil
}

func (f *fakeStore) ActivateVersion(_ context.Context, versionID string, generation int, at time.Time) (string, error) {
	f.record("activate")
	f.activated = append(f.activated, versionID)
	if f.activate != nil {
		return f.activate(context.Background(), versionID, generation, at)
	}
	return "", nil
}

func (f *fakeStore) AuthorizeReindex(context.Context, core.Principal, string) error { return nil }

func (f *fakeStore) ActivateReindex(ctx context.Context, _ core.Principal, versionID string, generation int, at time.Time) (string, error) {
	return f.ActivateVersion(ctx, versionID, generation, at)
}

func (f *fakeStore) FailVersion(_ context.Context, versionID, errorKind string, at time.Time) error {
	f.record("fail_version")
	f.failReasons = append(f.failReasons, errorKind)
	if f.failVersion != nil {
		return f.failVersion(context.Background(), versionID, errorKind, at)
	}
	return nil
}

func (f *fakeStore) FinishJob(_ context.Context, jobID, stage, status, errorKind string, at time.Time) error {
	f.record("finish_job:" + status)
	f.jobOutcomes = append(f.jobOutcomes, stage+"/"+status+"/"+errorKind)
	if f.finishJob != nil {
		return f.finishJob(context.Background(), jobID, stage, status, errorKind, at)
	}
	return nil
}

func (f *fakeStore) Document(_ context.Context, documentID string) (DocumentRecord, error) {
	f.docLookups = append(f.docLookups, documentID)
	if f.document != nil {
		return f.document(context.Background(), documentID)
	}
	return DocumentRecord{}, ErrNotFound
}

func (f *fakeStore) SoftDeleteDocument(_ context.Context, documentID string, at time.Time) error {
	f.record("delete_document")
	f.deletedDocs = append(f.deletedDocs, documentID)
	if f.deleteDoc != nil {
		return f.deleteDoc(context.Background(), documentID, at)
	}
	return nil
}

func (f *fakeStore) SoftDeleteCollection(_ context.Context, collectionID string, at time.Time) error {
	f.record("delete_collection")
	f.deletedColl = append(f.deletedColl, collectionID)
	if f.deleteColl != nil {
		return f.deleteColl(context.Background(), collectionID, at)
	}
	return nil
}

// fakeReader 实现 `DocumentReader`：默认返回一份已解码的文档。
// fakeReader implements DocumentReader.
type fakeReader struct {
	src   DocumentSource
	err   error
	paths []string
}

func (r *fakeReader) Load(_ context.Context, path string) (DocumentSource, error) {
	r.paths = append(r.paths, path)
	if r.err != nil {
		return DocumentSource{}, r.err
	}
	return r.src, nil
}

// fakeChunker 实现 `Chunker`。
// fakeChunker implements Chunker.
type fakeChunker struct {
	chunks []ChunkDraft
	err    error
	calls  int
}

func (c *fakeChunker) Split(_ context.Context, _ DocumentSource) ([]ChunkDraft, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return c.chunks, nil
}

// fakeEmbedder 实现 `Embedder`：默认按输入条数回非零向量。
// fakeEmbedder implements Embedder.
type fakeEmbedder struct {
	vectors      [][]float32
	promptTokens int64
	dimensions   int
	err          error
	calls        int
	inputs       [][]string
}

func (e *fakeEmbedder) Embed(_ context.Context, texts []string) (EmbedBatch, error) {
	e.calls++
	e.inputs = append(e.inputs, texts)
	if e.err != nil {
		return EmbedBatch{}, e.err
	}
	vectors := e.vectors
	if vectors == nil {
		vectors = unitVectors(len(texts), VectorDimensions)
	}
	return EmbedBatch{
		Vectors:      vectors,
		PromptTokens: e.promptTokens,
		Dimensions:   e.dimensions,
		Model:        "text-embedding-v4",
	}, nil
}

// fakeAudit 实现 `EmbeddingAudit`，记录预写 / 终态调用。
// fakeAudit implements EmbeddingAudit.
type fakeAudit struct {
	beginErr   error
	begins     int
	finishes   int
	finishArgs []int64
	events     *[]string
	// finishDeadline 记录终态写收到的上下文是否带截止时间（契约 §4.2）。
	finishDeadline bool
}

func (a *fakeAudit) BeginEmbedding(_ context.Context, _, _ string, _ int) (string, error) {
	if a.events != nil {
		*a.events = append(*a.events, "audit_begin")
	}
	a.begins++
	if a.beginErr != nil {
		return "", a.beginErr
	}
	return fmt.Sprintf("call_%d", a.begins), nil
}

func (a *fakeAudit) FinishEmbedding(ctx context.Context, _ string, promptTokens int64, callErr error) error {
	if a.events != nil {
		*a.events = append(*a.events, "audit_finish")
	}
	if _, ok := ctx.Deadline(); ok {
		a.finishDeadline = true
	}
	a.finishes++
	a.finishArgs = append(a.finishArgs, promptTokens)
	return nil
}

// fakeVectors 实现 `VectorIndex`，只记录写入（代际切换不归摄取管）。
// fakeVectors implements VectorIndex and records writes only.
type fakeVectors struct {
	err    error
	writes []vectorWrite
	events *[]string
}

type vectorWrite struct {
	versionID  string
	refs       []ChunkRef
	vectors    [][]float32
	generation int
}

func (v *fakeVectors) Write(_ context.Context, versionID string, refs []ChunkRef, vectors [][]float32, generation int) error {
	if v.events != nil {
		*v.events = append(*v.events, "vector_write")
	}
	v.writes = append(v.writes, vectorWrite{versionID: versionID, refs: refs, vectors: vectors, generation: generation})
	return v.err
}

func (v *fakeVectors) Activate(context.Context, string, int) error { return nil }

func (v *fakeVectors) Retire(context.Context, string) error { return nil }

// fakeQuota 实现 `Quota`，记录三态调用；`deny` 决定哪些成本被拒。
// fakeQuota implements Quota and records the three settlement outcomes.
type fakeQuota struct {
	deny    func(cost QuotaCost) error
	admits  []quotaAdmit
	commits []QuotaUsage
	release []QuotaReleaseReason
}

type quotaAdmit struct {
	cost QuotaCost
	idem string
}

func (q *fakeQuota) AdmitCall(_ context.Context, p core.Principal, cost QuotaCost, idem string) (QuotaTicket, error) {
	q.admits = append(q.admits, quotaAdmit{cost: cost, idem: idem})
	if q.deny != nil {
		if err := q.deny(cost); err != nil {
			return QuotaTicket{}, err
		}
	}
	return QuotaTicket{
		ReservationID: fmt.Sprintf("res_%d", len(q.admits)),
		CallerID:      p.CallerID,
		PeriodStart:   1,
	}, nil
}

func (q *fakeQuota) Commit(_ context.Context, _ QuotaTicket, usage QuotaUsage) error {
	q.commits = append(q.commits, usage)
	return nil
}

func (q *fakeQuota) Release(_ context.Context, _ QuotaTicket, reason QuotaReleaseReason) error {
	q.release = append(q.release, reason)
	return nil
}

// ingestHarness 把七个端口与共享事件序列装在一起；改完字段再调 `build`。
// ingestHarness wires the ports and a shared event log; tweak fields, then build.
type ingestHarness struct {
	t        *testing.T
	events   []string
	store    *fakeStore
	reader   *fakeReader
	chunker  *fakeChunker
	embedder *fakeEmbedder
	audit    *fakeAudit
	vectors  *fakeVectors
	quota    *fakeQuota
	// quotaDiag 是配额判定诊断的捕获面（V-8）；nil = 未接诊断。
	quotaDiag IngestQuotaDiagnosticRecorder
	logger    *slog.Logger
	cfg       IngestConfig
}

func newHarness(t *testing.T) *ingestHarness {
	t.Helper()
	h := &ingestHarness{t: t, cfg: testConfig(), logger: testLogger(t)}
	h.store = &fakeStore{events: &h.events}
	h.reader = &fakeReader{src: DocumentSource{
		Path:        "docs/a.md",
		Text:        "第一段正文第二段正文第三段正文",
		ContentHash: sha256Hex("原始字节"),
		SizeBytes:   96,
	}}
	h.chunker = &fakeChunker{chunks: sampleChunks()}
	h.embedder = &fakeEmbedder{promptTokens: 7}
	h.audit = &fakeAudit{events: &h.events}
	h.vectors = &fakeVectors{events: &h.events}
	h.quota = &fakeQuota{}
	return h
}

// build 用当前字段装配摄取器（每次调用都拿新实例，便于逐用例改钩子）。
// build assembles an Ingestor from the current harness state.
func (h *ingestHarness) build() *Ingestor {
	h.t.Helper()
	return NewIngestor(IngestorDeps{
		Store:            h.store,
		Reader:           h.reader,
		Chunker:          h.chunker,
		Embedder:         h.embedder,
		Vectors:          h.vectors,
		Audit:            h.audit,
		Quota:            h.quota,
		QuotaDiagnostics: h.quotaDiag,
		Config:           h.cfg,
		Logger:           h.logger,
		Now:              func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() },
		NewID: func(prefix string) (string, error) {
			return fmt.Sprintf("%s%d", prefix, h.audit.begins+1), nil
		},
	})
}

func (h *ingestHarness) request() IngestRequest {
	return IngestRequest{Principal: testPrincipal(), CollectionID: "col_1", Path: "docs/a.md"}
}

// TestIngest_ActivatesOnlyAfterEveryBatchIsIndexed 守 C-10 / 图 3：写片段 →
// 逐批（审计预写 → 调用 → 审计终态 → 写向量）→ 标记 indexed → 激活 → 作业成功。
// TestIngest_ActivatesOnlyAfterEveryBatchIsIndexed pins the figure-3 order.
func TestIngest_ActivatesOnlyAfterEveryBatchIsIndexed(t *testing.T) {
	h := newHarness(t)
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionActive, result.Status)
	assert.Equal(t, "doc_1", result.DocumentID)
	assert.Equal(t, "ver_1", result.VersionID)
	assert.Equal(t, "job_1", result.IngestJobID)
	assert.Equal(t, 3, result.ChunkCount)
	assert.Equal(t, 1, result.Generation)
	assert.False(t, result.Deduplicated)
	assert.Empty(t, result.ErrorKind)

	assert.Equal(t, []string{
		"start_job", "write_chunks",
		"audit_begin", "audit_finish", "vector_write",
		"audit_begin", "audit_finish", "vector_write",
		"mark_indexed", "activate", "finish_job:success",
	}, h.events)
	assert.Equal(t, []string{"ver_1"}, h.store.activated)
	require.Len(t, h.store.startReqs, 1)
	assert.Equal(t, "text-v1", h.store.startReqs[0].ParserVersion)
	assert.Equal(t, "v1", h.store.startReqs[0].ChunkerVersion)
	assert.Equal(t, "col_1", h.store.startReqs[0].CollectionID)
	assert.Equal(t, "docs/a.md", h.store.startReqs[0].SourcePath)

	// 两批：批内顺序与草稿一致，标题路径进入嵌入输入。
	assert.Equal(t, 2, h.embedder.calls)
	require.Len(t, h.embedder.inputs, 2)
	assert.Equal(t, []string{"第一段正文", "A > B\n第二段正文"}, h.embedder.inputs[0])
	assert.Equal(t, []string{"第三段正文"}, h.embedder.inputs[1])
	require.Len(t, h.vectors.writes, 2)
	assert.Equal(t, 1, h.vectors.writes[0].generation)
	assert.Equal(t, []ChunkRef{{ChunkID: "chk_1", Seq: 1}, {ChunkID: "chk_2", Seq: 2}}, h.vectors.writes[0].refs)

	// 配额：一次字节 + 两次 Token，键各不相同；两次 Token 均按回执用量结算。
	require.Len(t, h.quota.admits, 3)
	assert.Equal(t, QuotaCost{Bytes: 96}, h.quota.admits[0].cost)
	assert.Equal(t, "ingest:job_1:bytes", h.quota.admits[0].idem)
	assert.Positive(t, h.quota.admits[1].cost.Tokens, "批次必须按 Token 预留")
	assert.NotEqual(t, h.quota.admits[1].idem, h.quota.admits[2].idem)
	assert.Equal(t, []QuotaUsage{{Bytes: 96}, {Tokens: 7}, {Tokens: 7}}, h.quota.commits)
	assert.Empty(t, h.quota.release)
	assert.Equal(t, "done/success/", h.store.jobOutcomes[0])
}

// TestIngest_DeduplicatedContentWritesNothing 守图 3 的去重分支：同集合已有同
// hash 的 active 版本时直接返回，不建版本、不开作业、不占配额。
// TestIngest_DeduplicatedContentWritesNothing pins the dedup branch.
func TestIngest_DeduplicatedContentWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.store.findActive = func(context.Context, string, string) (VersionRecord, bool, error) {
		return VersionRecord{
			DocumentID: "doc_9", VersionID: "ver_9", Status: VersionActive,
			ChunkCount: 12, Generation: 1,
		}, true, nil
	}
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.True(t, result.Deduplicated)
	assert.Equal(t, "doc_9", result.DocumentID)
	assert.Equal(t, "ver_9", result.VersionID)
	assert.Equal(t, 12, result.ChunkCount)
	assert.Empty(t, result.IngestJobID)
	assert.Empty(t, h.events)
	assert.Empty(t, h.quota.admits)
	assert.Zero(t, h.chunker.calls)
	assert.Zero(t, h.embedder.calls)
}

// TestIngest_AuditPreWriteFailureDoesNotCallEmbedder 守宪法第 5 条：审计预写失败
// 即中止，调用**不得**发出，Token 预留按"未发出"归还。
// TestIngest_AuditPreWriteFailureDoesNotCallEmbedder pins the fail-closed audit gate.
func TestIngest_AuditPreWriteFailureDoesNotCallEmbedder(t *testing.T) {
	h := newHarness(t)
	h.audit.beginErr = errors.New("audit store down")
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionFailed, result.Status)
	assert.Equal(t, ErrorKindEmbedFailed, result.ErrorKind)
	assert.Zero(t, h.embedder.calls)
	assert.Empty(t, h.vectors.writes)
	assert.Empty(t, h.store.activated)
	assert.Equal(t, []QuotaReleaseReason{QuotaReleaseAuditFailed}, h.quota.release)
	assert.Equal(t, []QuotaUsage{{Bytes: 96}}, h.quota.commits)
	assert.Equal(t, []string{ErrorKindEmbedFailed}, h.store.failReasons)
	assert.Equal(t, "embed/failed/embed_failed", h.store.jobOutcomes[0])
}

// TestIngest_ChunkerFailureReleasesByteReservation 守"解析失败且未进入外部调用 →
// 归还字节预留"，并确认解析器报错记 `parser_failed`。
// TestIngest_ChunkerFailureReleasesByteReservation pins parse-failure settlement.
func TestIngest_ChunkerFailureReleasesByteReservation(t *testing.T) {
	h := newHarness(t)
	h.chunker.err = errors.New("malformed structure")
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionFailed, result.Status)
	assert.Equal(t, ErrorKindParserFailed, result.ErrorKind)
	assert.Equal(t, []QuotaReleaseReason{QuotaReleaseNotSent}, h.quota.release)
	assert.Empty(t, h.quota.commits)
	assert.Len(t, h.quota.admits, 1) // 只有字节预留，没有 Token 预留
	assert.Empty(t, h.embedder.inputs)
	assert.Empty(t, h.store.written)
	assert.Equal(t, "parse/failed/parser_failed", h.store.jobOutcomes[0])
}

// TestIngest_ChunkContractViolationFailsChunkStage 守切块契约：序号断裂在写库前被拦，
// 记 `chunker_failed`，字节预留归还。
// TestIngest_ChunkContractViolationFailsChunkStage rejects broken drafts before writes.
func TestIngest_ChunkContractViolationFailsChunkStage(t *testing.T) {
	h := newHarness(t)
	broken := sampleChunks()
	broken[2].Seq = 9
	h.chunker.chunks = broken
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionFailed, result.Status)
	assert.Equal(t, ErrorKindChunkerFailed, result.ErrorKind)
	assert.Empty(t, h.store.written)
	assert.Equal(t, []QuotaReleaseReason{QuotaReleaseNotSent}, h.quota.release)
	assert.Equal(t, "chunk/failed/chunker_failed", h.store.jobOutcomes[0])
}

// TestIngest_ChunkHashMismatchFailsChunkStage 守片段哈希绑定：切块器给的
// `content_hash` 与正文不符即拒绝——引用定位依赖这个哈希，不能带病落库。
// TestIngest_ChunkHashMismatchFailsChunkStage rejects drafts whose hash lies.
func TestIngest_ChunkHashMismatchFailsChunkStage(t *testing.T) {
	h := newHarness(t)
	drifted := sampleChunks()
	drifted[0].ContentHash = sha256Hex("别的正文")
	h.chunker.chunks = drifted
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, ErrorKindChunkerFailed, result.ErrorKind)
	assert.Empty(t, h.store.written)
}

// TestIngest_WriteChunksFailureReleasesBytes 守写片段失败同样归还字节预留。
// TestIngest_WriteChunksFailureReleasesBytes releases bytes when persistence fails.
func TestIngest_WriteChunksFailureReleasesBytes(t *testing.T) {
	h := newHarness(t)
	h.store.writeChunks = func(context.Context, string, []ChunkDraft) ([]ChunkRef, error) {
		return nil, errors.New("chunk insert failed")
	}
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, ErrorKindChunkerFailed, result.ErrorKind)
	assert.Empty(t, h.quota.commits)
	assert.Equal(t, []QuotaReleaseReason{QuotaReleaseNotSent}, h.quota.release)
}

// TestIngest_EmbedFailureKeepsTokenPending 守 C-29：调用已发出但失败 → 预留
// 保持 pending，**不得**按零释放；字节已在写片段后确认消耗，不归还。
// TestIngest_EmbedFailureKeepsTokenPending keeps unknown usage pending.
func TestIngest_EmbedFailureKeepsTokenPending(t *testing.T) {
	h := newHarness(t)
	h.embedder.err = errors.New("provider timeout")
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionFailed, result.Status)
	assert.Equal(t, ErrorKindEmbedFailed, result.ErrorKind)
	assert.Equal(t, []QuotaUsage{{Bytes: 96}}, h.quota.commits)
	assert.Empty(t, h.quota.release)
	assert.Empty(t, h.vectors.writes)
	assert.Empty(t, h.store.activated)
	assert.Equal(t, []int64{0}, h.audit.finishArgs) // 失败也要写终态，用量未知记 0
}

// TestIngest_EmbeddingTerminalWriteIsBounded 守摄取侧终态写同样带独立短超时
// （契约 §4.2）：脱取消是为了不丢终态，短超时是为了不挂死。
// 逆改点：把 `finishEmbedding` 的 `context.WithTimeout` 改回裸 `WithoutCancel` →
// `finishDeadline` 断言红。
func TestIngest_EmbeddingTerminalWriteIsBounded(t *testing.T) {
	h := newHarness(t)
	_, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	require.NotZero(t, h.audit.finishes, "摄取批次必须写终态")
	assert.True(t, h.audit.finishDeadline, "终态写必须带独立截止时间")
}

// TestIngest_UnknownUsageKeepsTokenPending 守"结果未知不是零"：回执
// `PromptTokens == 0` 时预留保持 pending，版本照常生效。
// TestIngest_UnknownUsageKeepsTokenPending never zero-settles unknown usage.
func TestIngest_UnknownUsageKeepsTokenPending(t *testing.T) {
	h := newHarness(t)
	h.embedder.promptTokens = 0
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionActive, result.Status)
	assert.Equal(t, []QuotaUsage{{Bytes: 96}}, h.quota.commits) // 两次 Token 都没结算
	assert.Empty(t, h.quota.release)
}

// TestIngest_ByteQuotaDeniedWritesNothing 守准入期零写入：字节预留被拒即返回错误，
// 作业、片段、向量一概不写。
// TestIngest_ByteQuotaDeniedWritesNothing keeps admission denials write-free.
func TestIngest_ByteQuotaDeniedWritesNothing(t *testing.T) {
	h := newHarness(t)
	denied := errors.New("quota exhausted")
	h.quota.deny = func(cost QuotaCost) error {
		if cost.Bytes > 0 {
			return denied
		}
		return nil
	}
	_, err := h.build().Ingest(t.Context(), h.request())
	require.ErrorIs(t, err, denied)

	assert.Empty(t, h.events)
	assert.Zero(t, h.chunker.calls)
	assert.Empty(t, h.quota.commits)
}

// TestIngest_EmbedQuotaDeniedDoesNotCallProvider 守"无预留不得调用 embedder"：
// 批次级配额拒绝让版本失败（记 `embed_failed`），提供方一次都不被调用。
// TestIngest_EmbedQuotaDeniedDoesNotCallProvider never calls without a reservation.
func TestIngest_EmbedQuotaDeniedDoesNotCallProvider(t *testing.T) {
	h := newHarness(t)
	h.quota.deny = func(cost QuotaCost) error {
		if cost.Tokens > 0 {
			return errors.New("token quota exhausted")
		}
		return nil
	}
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionFailed, result.Status)
	assert.Equal(t, ErrorKindEmbedFailed, result.ErrorKind)
	assert.Zero(t, h.embedder.calls)
	assert.Zero(t, h.audit.begins)
	assert.Empty(t, h.quota.release) // 被拒的预留根本没建，不需要归还
	assert.Empty(t, h.store.activated)
}

// TestIngest_DimensionMismatchFailsVerification 守维度一致：回执宽度不符记
// `verify_failed`，向量不落库、版本不激活（`TestEmbedding_RejectsDimensionMismatch`
// 的摄取侧对应点）。
// TestIngest_DimensionMismatchFailsVerification rejects wrong-width vectors.
func TestIngest_DimensionMismatchFailsVerification(t *testing.T) {
	h := newHarness(t)
	h.embedder.vectors = unitVectors(2, 3)
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionFailed, result.Status)
	assert.Equal(t, ErrorKindVerifyFailed, result.ErrorKind)
	assert.Empty(t, h.vectors.writes)
	assert.Empty(t, h.store.activated)
	assert.Equal(t, "embed/failed/verify_failed", h.store.jobOutcomes[0])
}

// TestIngest_ZeroVectorFailsVerification 守"非空率"：全零向量不落库。
// TestIngest_ZeroVectorFailsVerification rejects all-zero vectors.
func TestIngest_ZeroVectorFailsVerification(t *testing.T) {
	h := newHarness(t)
	vectors := unitVectors(2, VectorDimensions)
	vectors[0] = make([]float32, VectorDimensions)
	h.embedder.vectors = vectors
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, ErrorKindVerifyFailed, result.ErrorKind)
	assert.Empty(t, h.vectors.writes)
}

// TestIngest_ActivationFailureKeepsOldVersionServing 守唯一生效点：激活失败即
// 版本失败，**不得**返回 active。
// TestIngest_ActivationFailureKeepsOldVersionServing never reports active early.
func TestIngest_ActivationFailureKeepsOldVersionServing(t *testing.T) {
	h := newHarness(t)
	h.store.activate = func(context.Context, string, int, time.Time) (string, error) {
		return "", errors.New("activation failed")
	}
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionFailed, result.Status)
	assert.Equal(t, ErrorKindVerifyFailed, result.ErrorKind)
	assert.Equal(t, "index/failed/verify_failed", h.store.jobOutcomes[0])
}

// TestIngest_EmptyDocumentFailsWithoutQuota 守空文档：不占字节额度，切块结果为空
// 记 `chunker_failed`（什么都没写）。
// TestIngest_EmptyDocumentFailsWithoutQuota skips the byte reservation for empty files.
func TestIngest_EmptyDocumentFailsWithoutQuota(t *testing.T) {
	h := newHarness(t)
	h.reader.src.SizeBytes = 0
	h.chunker.chunks = nil
	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, VersionFailed, result.Status)
	assert.Equal(t, ErrorKindChunkerFailed, result.ErrorKind)
	assert.Empty(t, h.quota.admits)
	assert.Empty(t, h.quota.commits)
	assert.Equal(t, []string{"start_job", "fail_version", "finish_job:failed"}, h.events)
	assert.Equal(t, "chunk/failed/chunker_failed", h.store.jobOutcomes[0])
}

// TestIngest_RejectsZeroPrincipal 守零值身份：在读取正文之前拒绝，**不得**补成管理员。
// TestIngest_RejectsZeroPrincipal rejects the zero principal before any read.
func TestIngest_RejectsZeroPrincipal(t *testing.T) {
	h := newHarness(t)
	req := h.request()
	req.Principal = core.Principal{}
	_, err := h.build().Ingest(t.Context(), req)
	require.ErrorIs(t, err, core.ErrUnauthorized)

	assert.Empty(t, h.reader.paths)
	assert.Empty(t, h.events)
}

// TestIngest_RejectsMissingWriteScope 守写路径 scope：只读身份发起摄取即 403 语义。
// TestIngest_RejectsMissingWriteScope requires the knowledge write scope.
func TestIngest_RejectsMissingWriteScope(t *testing.T) {
	h := newHarness(t)
	req := h.request()
	req.Principal.Scopes = []string{core.ScopeKnowledgeRead}
	_, err := h.build().Ingest(t.Context(), req)
	require.ErrorIs(t, err, core.ErrForbidden)

	assert.Empty(t, h.reader.paths)
}

// TestIngest_RejectsDisallowedExtension 守扩展名白名单：拒绝时零读取、零写入。
// TestIngest_RejectsDisallowedExtension rejects extensions outside the allow list.
func TestIngest_RejectsDisallowedExtension(t *testing.T) {
	h := newHarness(t)
	req := h.request()
	req.Path = "docs/a.pdf"
	_, err := h.build().Ingest(t.Context(), req)
	require.ErrorIs(t, err, ErrPathRejected)

	assert.Empty(t, h.reader.paths)
	assert.Empty(t, h.events)
	assert.Empty(t, h.quota.admits)
}

// TestIngest_RejectsIncoherentConfig 守装配自检：维度与冻结列不一致时拒绝运行，
// **不得**等数据库 CHECK 报错。
// TestIngest_RejectsIncoherentConfig fails fast on config drift.
func TestIngest_RejectsIncoherentConfig(t *testing.T) {
	h := newHarness(t)
	h.cfg.EmbeddingDimensions = 1024
	_, err := h.build().Ingest(t.Context(), h.request())
	require.ErrorIs(t, err, ErrIngestUnavailable)

	assert.Empty(t, h.reader.paths)
}

// TestIngest_UnavailableWithoutPorts 守端口缺失即拒（构造期不 panic、不降级）。
// TestIngest_UnavailableWithoutPorts rejects a partially wired ingestor.
func TestIngest_UnavailableWithoutPorts(t *testing.T) {
	ingestor := NewIngestor(IngestorDeps{Config: testConfig(), Logger: testLogger(t)})
	_, err := ingestor.Ingest(t.Context(), IngestRequest{Principal: testPrincipal(), CollectionID: "col_1", Path: "a.md"})
	require.ErrorIs(t, err, ErrIngestUnavailable)
}

// TestIngest_DeleteRequiresExactlyOneTarget 守删除形状：恰一个目标，重复删除按
// 存储实现幂等。
// TestIngest_DeleteRequiresExactlyOneTarget pins the delete request shape.
func TestIngest_DeleteRequiresExactlyOneTarget(t *testing.T) {
	h := newHarness(t)
	ingestor := h.build()

	err := ingestor.Delete(t.Context(), DeleteRequest{Principal: testPrincipal()})
	require.ErrorIs(t, err, ErrInvalidRequest)
	err = ingestor.Delete(t.Context(), DeleteRequest{
		Principal:  testPrincipal(),
		DocumentID: "doc_1", CollectionID: "col_1",
	})
	require.ErrorIs(t, err, ErrInvalidRequest)

	require.NoError(t, ingestor.Delete(t.Context(), DeleteRequest{Principal: testPrincipal(), DocumentID: "doc_1"}))
	assert.Equal(t, []string{"doc_1"}, h.store.deletedDocs)
	assert.Empty(t, h.store.deletedColl)

	require.NoError(t, ingestor.Delete(t.Context(), DeleteRequest{Principal: testPrincipal(), CollectionID: "col_1"}))
	assert.Equal(t, []string{"col_1"}, h.store.deletedColl)
}

// TestIngest_ReindexUsesStoredPathAndCollection 守重建索引：路径与集合取自文档行，
// 内容未变也须重新解析、创建新版本并请求激活。
// TestIngest_ReindexUsesStoredPathAndCollection replays from the stored path.
func TestIngest_ReindexUsesStoredPathAndCollection(t *testing.T) {
	h := newHarness(t)
	h.reader.src.Path = "docs/b.txt"
	h.store.document = func(context.Context, string) (DocumentRecord, error) {
		return DocumentRecord{
			DocumentID: "doc_1", CollectionID: "col_7", SourcePath: "docs/b.txt",
			ContentHash: sha256Hex("原始字节"),
		}, nil
	}
	h.store.findActive = func(_ context.Context, collectionID, contentHash string) (VersionRecord, bool, error) {
		require.Equal(t, "col_7", collectionID)
		require.Equal(t, sha256Hex("原始字节"), contentHash)
		return VersionRecord{
			DocumentID: "doc_1", VersionID: "ver_1", Status: VersionActive,
			ChunkCount: 3, Generation: 1,
		}, true, nil
	}
	result, err := h.build().Reindex(t.Context(), ReindexRequest{Principal: testPrincipal(), DocumentID: "doc_1"})
	require.NoError(t, err)

	assert.False(t, result.Deduplicated)
	assert.Equal(t, []string{"docs/b.txt"}, h.reader.paths)
	assert.Equal(t, []string{"doc_1"}, h.store.docLookups)
	require.Len(t, h.store.startReqs, 1)
	assert.Equal(t, "col_7", h.store.startReqs[0].CollectionID)
	assert.Equal(t, "docs/b.txt", h.store.startReqs[0].SourcePath)
	assert.Equal(t, "doc_1", h.store.startReqs[0].ExpectedDocumentID)
	assert.True(t, h.store.startReqs[0].ForceNewVersion)
	assert.Contains(t, h.events, "write_chunks")
	assert.Contains(t, h.events, "activate")
}

// TestIngest_ReindexRejectsDeletedDocument 守已删文档不得重建索引。
// TestIngest_ReindexRejectsDeletedDocument refuses deleted documents.
func TestIngest_ReindexRejectsDeletedDocument(t *testing.T) {
	h := newHarness(t)
	h.store.document = func(context.Context, string) (DocumentRecord, error) {
		return DocumentRecord{
			DocumentID: "doc_1", CollectionID: "col_7", SourcePath: "docs/b.txt",
			Deleted: true,
		}, nil
	}
	_, err := h.build().Reindex(t.Context(), ReindexRequest{Principal: testPrincipal(), DocumentID: "doc_1"})
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, h.reader.paths)
}

// TestIngest_ErrorKindsMatchContract 守错误码闭集：`error_kind` 只能是契约冻结的
// 六个字面量，不多不少。
// TestIngest_ErrorKindsMatchContract pins the frozen error-kind closed set.
func TestIngest_ErrorKindsMatchContract(t *testing.T) {
	assert.Equal(t, []string{
		"path_rejected", "decode_failed", "parser_failed",
		"chunker_failed", "embed_failed", "verify_failed",
	}, ErrorKinds())
}

// TestIngest_DedupLookupUsesCollectionAndContentHash 守去重查找键：传给存储的必须是
// 「本次请求的集合 + 本次读到的内容哈希」，不是来源路径、也不是文档行里的旧哈希。
// 既有的去重用例把假存储写成对入参不敏感，键写错它们仍绿——本用例补这一格。
// 逆改点：把键换成空串 / 来源路径 / 别的字段 → findHashes 断言红。
func TestIngest_DedupLookupUsesCollectionAndContentHash(t *testing.T) {
	h := newHarness(t)
	_, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)

	assert.Equal(t, []string{"col_1/" + sha256Hex("原始字节")}, h.store.findHashes,
		"去重键必须是集合 + 内容哈希")
}

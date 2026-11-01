package knowledge

// 本文件实现摄取、删除与重建索引的状态机（US-16 §2.3 图 3 / §3.1.3 / T034）；
// 三条入口都在准入与终态处接入操作审计（T036a，见 `audit.go`）。
//
// 三段边界：
//  1. **准入期**（读取正文之前）：装配不全、身份不可用 / 越权、请求形状非法、
//     扩展名不在允许集合——一律返回错误、**零写入、零外部调用**（契约
//     `POST /collections/{id}/documents` 的 400 / 401 / 403 语义）；
//  2. **读取与去重**：先读文件（真实路径投影、大小上限与 UTF-8 解码在
//     `DocumentReader` 实现里），再按内容哈希查同集合 active 版本；命中即返回，
//     不建版本、不开作业、不预留配额；
//  3. **作业期**：字节预留 → 开作业 → 解析切块 → 写片段 → 分批 embedding
//     （每批先预留 Token、再写审计 `pending`）→ 校验 → `MarkIndexed` →
//     `ActivateVersion`（**唯一生效点**）。作业期内任何失败都落成
//     `IngestResult{Status: failed, ErrorKind: ...}` 正常返回（契约 200 语义），
//     旧 active 版本继续服务；只有准入与基础设施错误才上抛。
//
// 与图 3 的三处任务级差异（记入 tasks.md 附录 R43）：
//   - 图 3 写 `status=received`；冻结契约与 DDL 只有 `pending`，以契约为准；
//   - 配额说明写"读取前预留字节"，但字节数只有读后才知道——实际顺序是
//     "读取与去重之后、任何写入与外部调用之前"，"无预留不发调用"不变；
//   - 普通摄取可以复用同内容的最新失败版本，尝试记录仍保留在作业中；显式重建
//     即使内容未变化也创建新版本，保留此前快照。
//
// This file implements the ingest state machine of figure 3. Admission failures
// return errors with zero writes; failures after the job starts come back as a
// failed IngestResult because the contract answers 200 for both terminal states.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// Ingestor 执行同步摄取：解析、切块、分批 embedding、校验与激活。
// Ingestor runs the synchronous ingest pipeline described by figure 3.
type Ingestor struct {
	store      IngestStore
	reader     DocumentReader
	chunker    Chunker
	embedder   Embedder
	vectors    VectorIndex
	audit      EmbeddingAudit
	quota      Quota
	quotaDiag  IngestQuotaDiagnosticRecorder
	operations *operationAudit
	cfg        IngestConfig
	logger     *slog.Logger
	now        func() time.Time
	newID      func(prefix string) (string, error)
}

// NewIngestor 装配摄取器。端口缺一不在构造期失败（签名无 error），由 `Ingest`
// 在读取正文前返回 `ErrIngestUnavailable`——装配错误**不得**在运行期退化成默认值。
// NewIngestor assembles the ingestor; a missing port fails closed at Ingest time.
func NewIngestor(deps IngestorDeps) *Ingestor {
	ingestor := &Ingestor{
		store:     deps.Store,
		reader:    deps.Reader,
		chunker:   deps.Chunker,
		embedder:  deps.Embedder,
		vectors:   deps.Vectors,
		audit:     deps.Audit,
		quota:     deps.Quota,
		quotaDiag: deps.QuotaDiagnostics,
		cfg:       deps.Config,
		logger:    deps.Logger,
		now:       deps.Now,
		newID:     deps.NewID,
	}
	if ingestor.logger == nil {
		ingestor.logger = slog.Default()
	}
	if ingestor.now == nil {
		ingestor.now = time.Now
	}
	if ingestor.newID == nil {
		ingestor.newID = newRandomID
	}
	ingestor.operations = newOperationAudit(ingestor.logger, ingestor.now)
	return ingestor
}

// Ingest 摄取一份工作区文档，返回版本终态（`active` / `failed`）或准入错误。
// Ingest ingests one workspace document and returns the version's terminal state.
func (i *Ingestor) Ingest(ctx context.Context, req IngestRequest) (IngestResult, error) {
	trace, err := i.openTrace(ctx, opIngest, req.Principal, operationTargets{
		collectionID: req.CollectionID,
		sourcePath:   req.Path,
	})
	if err != nil {
		return IngestResult{}, err
	}
	if err := i.authorize(req.Principal); err != nil {
		trace.reject(ctx, err)
		return IngestResult{}, err
	}
	return i.ingestWith(ctx, req, trace, "")
}

// Delete 软删一个文档或一个集合：文档的全部版本退出检索，历史消息与其引用**不**
// 删除（读面按当前删除状态标注失效）。重复删除幂等，由存储实现保证。
// Delete soft-deletes one document or collection; repeated deletes are idempotent.
func (i *Ingestor) Delete(ctx context.Context, req DeleteRequest) error {
	documentID := strings.TrimSpace(req.DocumentID)
	collectionID := strings.TrimSpace(req.CollectionID)
	trace, err := i.openTrace(ctx, opDelete, req.Principal, operationTargets{
		documentID:   documentID,
		collectionID: collectionID,
	})
	if err != nil {
		return err
	}
	if err := i.authorize(req.Principal); err != nil {
		trace.reject(ctx, err)
		return err
	}
	if (documentID == "") == (collectionID == "") {
		err := fmt.Errorf("%w: 删除必须且只能指定文档或集合之一", ErrInvalidRequest)
		trace.reject(ctx, err)
		return err
	}
	now := i.now()
	if documentID != "" {
		if err := i.store.SoftDeleteDocument(ctx, documentID, now); err != nil {
			trace.fail(ctx, auditRef{}, "", "", err, nil)
			return err
		}
		// 软删行本身就是留痕：权威行引用就地指向它，不需要第二笔写入。
		trace.succeed(ctx, auditRef{table: "kb_documents", id: documentID}, nil)
		return nil
	}
	if err := i.store.SoftDeleteCollection(ctx, collectionID, now); err != nil {
		trace.fail(ctx, auditRef{}, "", "", err, nil)
		return err
	}
	trace.succeed(ctx, auditRef{table: "kb_collections", id: collectionID}, nil)
	return nil
}

// Reindex 按文档现有来源路径重建索引；内容未变也创建独立版本，成功前旧版本继续服务。
// Reindex rebuilds from the stored path into an independent version.
func (i *Ingestor) Reindex(ctx context.Context, req ReindexRequest) (IngestResult, error) {
	documentID := strings.TrimSpace(req.DocumentID)
	trace, err := i.openTrace(ctx, opReindex, req.Principal, operationTargets{documentID: documentID})
	if err != nil {
		return IngestResult{}, err
	}
	if err := i.authorize(req.Principal); err != nil {
		trace.reject(ctx, err)
		return IngestResult{}, err
	}
	if documentID == "" {
		err := fmt.Errorf("%w: 重建索引缺少文档标识", ErrInvalidRequest)
		trace.reject(ctx, err)
		return IngestResult{}, err
	}
	guard, ok := i.store.(ReindexAuthorization)
	if !ok {
		err := fmt.Errorf("%w: 重建索引缺少授权校验", ErrIngestUnavailable)
		trace.reject(ctx, err)
		return IngestResult{}, err
	}
	if err := guard.AuthorizeReindex(ctx, req.Principal, documentID); err != nil {
		trace.reject(ctx, err)
		return IngestResult{}, err
	}
	doc, err := i.store.Document(ctx, documentID)
	if err != nil {
		// 不存在的文档是调用方问题（拒绝）；其余是存储故障（失败），两者留痕不同。
		if errors.Is(err, ErrNotFound) {
			trace.reject(ctx, err)
		} else {
			trace.fail(ctx, auditRef{}, "", "", err, nil)
		}
		return IngestResult{}, err
	}
	if doc.Deleted {
		err := fmt.Errorf("%w: 文档 %s", ErrNotFound, documentID)
		trace.reject(ctx, err)
		return IngestResult{}, err
	}
	trace.scope(operationTargets{collectionID: doc.CollectionID, sourcePath: doc.SourcePath})
	return i.ingestWith(ctx, IngestRequest{
		Principal:    req.Principal,
		CollectionID: doc.CollectionID,
		Path:         doc.SourcePath,
	}, trace, documentID)
}

// openTrace 统一三条入口的准入顺序：装配 → 身份 → 审计轨迹。装配错误与非法身份
// 都没有轨迹，只落日志——两者都在读取正文与任何写入之前发生。
// openTrace orders admission as assembly, identity, then the audit trace.
func (i *Ingestor) openTrace(ctx context.Context, kind operationKind, p core.Principal,
	targets operationTargets,
) (*operationTrace, error) {
	started := i.startedAt()
	if err := i.ready(); err != nil {
		i.rejectAdmission(ctx, kind, p, err, targets)
		return nil, err
	}
	trace, err := i.operations.begin(p, kind, started)
	if err != nil {
		i.rejectAdmission(ctx, kind, p, err, targets)
		return nil, err
	}
	trace.scope(targets)
	return trace, nil
}

// rejectAdmission 记录没有轨迹的准入拒绝；零值 `Ingestor` 没有日志面，直接返回。
// rejectAdmission logs a rejection that never reached a trace.
func (i *Ingestor) rejectAdmission(ctx context.Context, kind operationKind, p core.Principal,
	cause error, targets operationTargets,
) {
	if i == nil {
		return
	}
	i.operations.rejected(ctx, kind, p, cause, targets)
}

// startedAt 取本次操作的起点；零值 `Ingestor` 回落墙上时钟，耗时字段仍有意义。
// startedAt reads the operation start; the wall clock keeps durations meaningful.
func (i *Ingestor) startedAt() time.Time {
	if i == nil || i.now == nil {
		return time.Now()
	}
	return i.now()
}

// ready 判定装配是否可用：端口齐备、配置自洽（维度与冻结列一致、批大小 /
// 大小上限 / 扩展名 / 切分版本 / 代际已给出）。任一不满足即拒绝，**不得**用默认值补齐。
// ready fails closed on any missing port or incoherent configuration.
func (i *Ingestor) ready() error {
	if i == nil || i.store == nil || i.reader == nil || i.chunker == nil ||
		i.embedder == nil || i.vectors == nil || i.audit == nil || i.quota == nil {
		return ErrIngestUnavailable
	}
	switch {
	case i.cfg.EmbeddingModel == "":
		return fmt.Errorf("%w: embedding.model 为空", ErrIngestUnavailable)
	case i.cfg.EmbeddingDimensions != VectorDimensions:
		return fmt.Errorf("%w: embedding.dimensions=%d，冻结列是 %d 维",
			ErrIngestUnavailable, i.cfg.EmbeddingDimensions, VectorDimensions)
	case i.cfg.EmbeddingBatchSize < 1:
		return fmt.Errorf("%w: embedding.batch_size 必须为正", ErrIngestUnavailable)
	case i.cfg.MaxDocumentBytes <= 0:
		return fmt.Errorf("%w: knowledge.max_document_bytes 必须为正", ErrIngestUnavailable)
	case len(i.cfg.AllowedExtensions) == 0:
		return fmt.Errorf("%w: knowledge.allowed_extensions 为空", ErrIngestUnavailable)
	case i.cfg.ChunkerVersion == "":
		return fmt.Errorf("%w: knowledge.chunk.version 为空", ErrIngestUnavailable)
	case i.cfg.Generation < 1:
		return fmt.Errorf("%w: 索引代际未指定", ErrIngestUnavailable)
	}
	return nil
}

// authorize 是知识写路径的二次身份校验：零值 / 越权身份在读取正文之前拒绝。
// 管理面 scope 已由 `Principal.Valid` 挡掉，这里只补业务 scope。
// authorize re-checks identity and the write scope before any content is read.
func (i *Ingestor) authorize(p core.Principal) error {
	if !p.Valid() {
		return core.ErrUnauthorized
	}
	if !p.HasScope(core.ScopeKnowledgeWrite) {
		return core.ErrForbidden
	}
	return nil
}

// ingestWith 是摄取主链路（图 3 逐块对应：Start → parseAndChunk → 分批 embed →
// verifyAndActivate → success / fail）。轨迹由调用方开好——`Ingest` 与 `Reindex`
// 共用本函数，**不得**在链路中再开第二条轨迹。
// ingestWith is the main pipeline mirroring figure 3 block by block; the caller
// opens the trace so ingest and reindex share one code path.
func (i *Ingestor) ingestWith(ctx context.Context, req IngestRequest, trace *operationTrace, rebuildDocumentID string) (IngestResult, error) {
	rebuild := rebuildDocumentID != ""
	collectionID := strings.TrimSpace(req.CollectionID)
	if collectionID == "" {
		err := fmt.Errorf("%w: 缺少集合标识", ErrInvalidRequest)
		trace.reject(ctx, err)
		return IngestResult{}, err
	}
	if err := i.checkPath(req.Path); err != nil {
		trace.reject(ctx, err)
		return IngestResult{}, err
	}

	src, err := i.reader.Load(ctx, req.Path)
	if err != nil {
		trace.reject(ctx, err)
		return IngestResult{}, err
	}
	// 读取器可能把路径投影成真实路径：日志以后者为准（与落库的 source_path 一致）。
	trace.scope(operationTargets{collectionID: collectionID, sourcePath: src.Path})

	// 去重：同集合同内容哈希的 active 版本直接返回，不建版本、不记配额。
	if existing, ok, lookupErr := i.findExistingVersion(ctx, collectionID, src.ContentHash, rebuild); lookupErr != nil {
		trace.fail(ctx, auditRef{}, "", "", lookupErr, nil)
		return IngestResult{}, lookupErr
	} else if ok {
		trace.scope(operationTargets{documentID: existing.DocumentID, versionID: existing.VersionID})
		trace.succeed(ctx, auditRef{table: "kb_document_versions", id: existing.VersionID}, nil)
		return IngestResult{
			DocumentID:   existing.DocumentID,
			VersionID:    existing.VersionID,
			Status:       existing.Status,
			Deduplicated: true,
			ChunkCount:   existing.ChunkCount,
			Generation:   existing.Generation,
		}, nil
	}

	jobID, err := i.newID("job_")
	if err != nil {
		trace.fail(ctx, auditRef{}, "", "", err, nil)
		return IngestResult{}, err
	}
	trace.scope(operationTargets{jobID: jobID})

	// 字节预留：读取与去重之后、任何写入与外部调用之前（图 3 配额说明的第 2 条差异）。
	bytesTicket, err := i.reserveBytes(ctx, req.Principal, jobID, src.SizeBytes)
	if err != nil {
		trace.reject(ctx, err)
		return IngestResult{}, err
	}

	job, err := i.store.StartJob(ctx, StartJobRequest{
		ForceNewVersion:    rebuild,
		ExpectedDocumentID: rebuildDocumentID,
		JobID:              jobID,
		CollectionID:       collectionID,
		SourcePath:         src.Path,
		ContentHash:        src.ContentHash,
		SourceText:         src.Text,
		ParserVersion:      ParserVersion,
		ChunkerVersion:     i.cfg.ChunkerVersion,
		At:                 i.now(),
	})
	if err != nil {
		i.releaseTicket(ctx, bytesTicket, QuotaReleaseNotSent)
		trace.fail(ctx, auditRef{}, "", "", err, nil)
		return IngestResult{}, err
	}
	trace.scope(operationTargets{documentID: job.DocumentID, versionID: job.VersionID})

	chunks, kind, stage, err := i.parseAndChunk(ctx, src)
	if err != nil {
		i.releaseTicket(ctx, bytesTicket, QuotaReleaseNotSent)
		return i.fail(ctx, trace, job, kind, stage, err)
	}

	refs, err := i.store.WriteChunks(ctx, job.VersionID, chunks)
	if err != nil {
		i.releaseTicket(ctx, bytesTicket, QuotaReleaseNotSent)
		return i.fail(ctx, trace, job, ErrorKindChunkerFailed, JobStageChunk, err)
	}
	// 片段已落库：字节确认消耗；此后的失败不再归还字节额度。
	i.commitTicket(ctx, bytesTicket, QuotaUsage{Bytes: src.SizeBytes})

	offset := 0
	for seq, batch := range batchChunks(chunks, i.cfg.EmbeddingBatchSize) {
		batchRefs := refs[offset : offset+len(batch)]
		offset += len(batch)
		kind, err := i.embedBatch(ctx, req.Principal, job, seq+1, batch, batchRefs)
		if err != nil {
			return i.fail(ctx, trace, job, kind, JobStageEmbed, err)
		}
	}

	if err := i.verifyAndActivate(ctx, job, chunks, refs, req.Principal, rebuild); err != nil {
		return i.fail(ctx, trace, job, ErrorKindVerifyFailed, JobStageIndex, err)
	}
	return i.success(ctx, trace, job, len(chunks)), nil
}

func (i *Ingestor) findExistingVersion(ctx context.Context, collectionID, contentHash string, rebuild bool) (VersionRecord, bool, error) {
	if rebuild {
		return VersionRecord{}, false, nil
	}
	return i.store.FindActiveByContentHash(ctx, collectionID, contentHash)
}

// checkPath 只做领域规则：非空与扩展名（大小写不敏感）。真实路径投影、允许根比较、
// 符号链接 / 联接点拒绝与大小上限都在 `DocumentReader` 实现里——两处各管一段，
// **不得**用字符串前缀比较替代真实路径比较。
// checkPath enforces only the domain rules; real-path projection belongs to the reader.
func (i *Ingestor) checkPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: 来源路径为空", ErrPathRejected)
	}
	ext := extensionOf(path)
	for _, allowed := range i.cfg.AllowedExtensions {
		if strings.EqualFold(ext, allowed) {
			return nil
		}
	}
	return fmt.Errorf("%w: 扩展名 %q 不在允许集合内", ErrPathRejected, ext)
}

// extensionOf 取最后一个分隔符之后、最后一个点起的后缀（含点，统一小写）。
// extensionOf returns the lower-cased suffix including the dot, separator-agnostic.
func extensionOf(path string) string {
	base := path
	if idx := strings.LastIndexAny(path, `/\`); idx >= 0 {
		base = path[idx+1:]
	}
	idx := strings.LastIndexByte(base, '.')
	if idx < 0 {
		return ""
	}
	return strings.ToLower(base[idx:])
}

// parseAndChunk 解析正文并切块，随后校验切块契约。解析器报错记 `parser_failed`
// （阶段 `parse`），结果违反契约（空结果 / 序号不连续 / 空片段 / 区间非法 / 哈希
// 缺失或不符）记 `chunker_failed`（阶段 `chunk`）——两种失败都发生在外部调用
// 之前，字节预留由调用方归还。
// parseAndChunk splits the text and validates the chunk contract; both failures
// happen before any external call and carry the matching job stage.
func (i *Ingestor) parseAndChunk(ctx context.Context, src DocumentSource) ([]ChunkDraft, string, string, error) {
	chunks, err := i.chunker.Split(ctx, src)
	if err != nil {
		return nil, ErrorKindParserFailed, JobStageParse, err
	}
	if err := validateChunks(chunks); err != nil {
		return nil, ErrorKindChunkerFailed, JobStageChunk, err
	}
	return chunks, "", "", nil
}

// validateChunks 判定切块是否满足写入契约：非空、序号从 1 连续、正文非空、
// 区间左闭右开且非空、片段哈希与正文一致。**不得**把破损的切块写进库再靠查询兜底。
// validateChunks rejects empty, misnumbered, or hash-mismatched drafts.
func validateChunks(chunks []ChunkDraft) error {
	if len(chunks) == 0 {
		return fmt.Errorf("切分结果为空")
	}
	for idx, chunk := range chunks {
		if chunk.Seq != idx+1 {
			return fmt.Errorf("第 %d 段的序号是 %d，要求从 1 连续", idx+1, chunk.Seq)
		}
		if strings.TrimSpace(chunk.Text) == "" {
			return fmt.Errorf("第 %d 段正文为空", chunk.Seq)
		}
		if chunk.CharStart < 0 || chunk.CharEnd <= chunk.CharStart {
			return fmt.Errorf("第 %d 段的字符区间 [%d, %d) 非法", chunk.Seq, chunk.CharStart, chunk.CharEnd)
		}
		if chunk.ContentHash != hashText(chunk.Text) {
			return fmt.Errorf("第 %d 段的 content_hash 与正文不一致", chunk.Seq)
		}
	}
	return nil
}

// batchChunks 按批大小切分草稿，保持原顺序（批内顺序即 `seq` 顺序）。
// batchChunks splits drafts into ordered batches.
func batchChunks(chunks []ChunkDraft, size int) [][]ChunkDraft {
	if size < 1 {
		size = 1
	}
	batches := make([][]ChunkDraft, 0, (len(chunks)+size-1)/size)
	for start := 0; start < len(chunks); start += size {
		end := min(start+size, len(chunks))
		batches = append(batches, chunks[start:end])
	}
	return batches
}

// embedBatch 处理一批片段：预留 Token → 审计预写 → 调用 → 审计终态 → 校验 →
// 写向量。返回空 kind 表示成功；失败时 kind 是应写入的 `error_kind`。
//
// 结算口径（图 3 配额说明）：
//   - 配额拒绝或审计预写失败 → 调用**没有发出**，Token 预留按原因归还；
//   - 调用已发出但失败 / 超时 / 用量未知（`PromptTokens == 0`）→ 预留保持
//     `pending`，**不得**按零静默释放；
//   - 仅当回执给出正数用量时才 `Commit`。
//
// embedBatch runs one batch: reserve, audit pre-write, call, audit terminal write,
// verify the vectors, persist them. Unknown usage stays pending, never zero.
func (i *Ingestor) embedBatch(ctx context.Context, p core.Principal, job JobRecord, seq int,
	batch []ChunkDraft, refs []ChunkRef,
) (string, error) {
	attemptID, err := i.newID("att_")
	if err != nil {
		return ErrorKindEmbedFailed, err
	}
	texts := make([]string, len(batch))
	for idx, chunk := range batch {
		texts[idx] = embedInput(chunk)
	}

	ticket, err := i.quota.AdmitCall(ctx, p,
		QuotaCost{Tokens: estimateTokens(texts)},
		fmt.Sprintf("ingest:%s:embed:%s:%d", job.JobID, attemptID, seq))
	// 配额判定诊断（V-8）：放行与拒绝都记，拒绝带原因码、放行不带——与运行路径的
	// `quota_decision` 同一口径。主锚点是 `ingest_job_id`（契约 §2.9 的关联表）。
	// The verdict is recorded either way, anchored on the ingest job.
	quotaEvent := IngestQuotaDiagnostic{
		IngestJobID: job.JobID,
		StepID:      "embed:" + strconv.Itoa(seq),
		Seq:         int64(seq),
		Class:       "ingest",
	}
	if err != nil {
		// 无预留**不得**调用 embedder（C-29）。
		quotaEvent.Decision = DiagnosticDecisionDeny
		quotaEvent.Reason = ingestQuotaReason(err)
		i.recordIngestQuota(ctx, quotaEvent)
		return ErrorKindEmbedFailed, err
	}
	quotaEvent.Decision = DiagnosticDecisionAllow
	i.recordIngestQuota(ctx, quotaEvent)

	callID, err := i.audit.BeginEmbedding(ctx, job.JobID, i.cfg.EmbeddingModel, len(texts))
	if err != nil {
		i.releaseToken(ctx, ticket)
		return ErrorKindEmbedFailed, err
	}

	res, embedErr := i.embedder.Embed(ctx, texts)
	if embedErr != nil {
		// 调用已发出、结局未知：终态审计照写，预留保持 pending（不归还）。
		i.finishEmbedding(ctx, callID, 0, embedErr)
		return ErrorKindEmbedFailed, embedErr
	}
	i.finishEmbedding(ctx, callID, res.PromptTokens, nil)
	if res.PromptTokens > 0 {
		i.commitTicket(ctx, &ticket, QuotaUsage{Tokens: res.PromptTokens})
	}

	if err := checkVectors(batch, res, i.cfg.EmbeddingDimensions); err != nil {
		return ErrorKindVerifyFailed, err
	}
	if err := i.vectors.Write(ctx, job.VersionID, refs, res.Vectors, i.cfg.Generation); err != nil {
		return ErrorKindVerifyFailed, err
	}
	return "", nil
}

// checkVectors 校验一批回执：数量与输入一一对应、维度等于配置（等同冻结列宽）、
// 每条向量非全零（"非空率"的机械判据）。任一条不满足即视为校验失败。
// checkVectors verifies count, width and non-emptiness of one batch's vectors.
func checkVectors(batch []ChunkDraft, res EmbedBatch, want int) error {
	if len(res.Vectors) != len(batch) {
		return fmt.Errorf("批次有 %d 段文本，只回 %d 个向量", len(batch), len(res.Vectors))
	}
	width := res.Dimensions
	if width == 0 && len(res.Vectors) > 0 {
		width = len(res.Vectors[0])
	}
	if width != want {
		return fmt.Errorf("向量维度是 %d，配置要求 %d", width, want)
	}
	for idx, vector := range res.Vectors {
		if len(vector) != want {
			return fmt.Errorf("第 %d 条向量维度是 %d，配置要求 %d", idx+1, len(vector), want)
		}
		nonZero := false
		for _, value := range vector {
			if value != 0 {
				nonZero = true
				break
			}
		}
		if !nonZero {
			return fmt.Errorf("第 %d 条向量全零", idx+1)
		}
	}
	return nil
}

// verifyAndActivate 是**唯一生效点**：先核对片段与落库结果一一对应（数量、
// 序号、哈希绑定在版本行上由 StartJob 写入），再 `MarkIndexed`、最后
// `ActivateVersion`——任一步失败版本都不生效，旧 active 版本继续服务。
// verifyAndActivate is the single visibility point of a version.
func (i *Ingestor) verifyAndActivate(ctx context.Context, job JobRecord, chunks []ChunkDraft, refs []ChunkRef, principal core.Principal, rebuild bool) error {
	if len(chunks) == 0 {
		return fmt.Errorf("版本没有可生效的片段")
	}
	if len(refs) != len(chunks) {
		return fmt.Errorf("写入的片段是 %d 条，切分结果是 %d 条", len(refs), len(chunks))
	}
	for idx := range refs {
		if refs[idx].Seq != chunks[idx].Seq {
			return fmt.Errorf("第 %d 条落库片段的序号是 %d，切分结果是 %d",
				idx+1, refs[idx].Seq, chunks[idx].Seq)
		}
	}
	now := i.now()
	if err := i.store.MarkIndexed(ctx, job.VersionID, now); err != nil {
		return err
	}
	var err error
	if rebuild {
		guard, ok := i.store.(ReindexAuthorization)
		if !ok {
			return ErrIngestUnavailable
		}
		_, err = guard.ActivateReindex(ctx, principal, job.VersionID, i.cfg.Generation, now)
	} else {
		_, err = i.store.ActivateVersion(ctx, job.VersionID, i.cfg.Generation, now)
	}
	if err != nil {
		return err
	}
	return nil
}

// success 写作业终态并返回 `active` 结果。作业行是本次操作的**权威行**，写失败
// 升级为 ERROR（工程规范 §4）：版本已经生效，**不得**因为一笔簿记失败把已经对外
// 可见的版本报成失败，但停在 `running` 的作业行需要人介入。
// success records the job outcome; a failed terminal write is an ERROR, not a
// result flip, because the version is already visible.
func (i *Ingestor) success(ctx context.Context, trace *operationTrace, job JobRecord, chunkCount int) IngestResult {
	trace.succeed(ctx, auditRef{table: "kb_ingest_jobs", id: job.JobID}, func(terminal context.Context) error {
		return i.store.FinishJob(terminal, job.JobID, JobStageDone, JobStatusSuccess, "", i.now())
	})
	return IngestResult{
		DocumentID:  job.DocumentID,
		VersionID:   job.VersionID,
		IngestJobID: job.JobID,
		Status:      VersionActive,
		ChunkCount:  chunkCount,
		Generation:  i.cfg.Generation,
	}
}

// fail 把版本与作业写成失败终态，并返回 `failed` 结果（契约的 200 语义）。
// 终态回填用 `context.WithoutCancel`（在 `persistTerminal` 内）：请求取消**不得**
// 让失败离开半截状态（与 `credential_ops` 的终态回填同一口径）。
// fail marks the version and job as failed and returns the failed result.
func (i *Ingestor) fail(ctx context.Context, trace *operationTrace, job JobRecord,
	kind, stage string, cause error,
) (IngestResult, error) {
	trace.fail(ctx, auditRef{table: "kb_ingest_jobs", id: job.JobID}, stage, kind, cause,
		func(terminal context.Context) error {
			now := i.now()
			var errs []error
			if err := i.store.FailVersion(terminal, job.VersionID, kind, now); err != nil {
				errs = append(errs, fmt.Errorf("fail version %s: %w", job.VersionID, err))
			}
			if err := i.store.FinishJob(terminal, job.JobID, stage, JobStatusFailed, kind, now); err != nil {
				errs = append(errs, fmt.Errorf("finish job %s: %w", job.JobID, err))
			}
			return errors.Join(errs...)
		})
	return IngestResult{
		DocumentID:  job.DocumentID,
		VersionID:   job.VersionID,
		IngestJobID: job.JobID,
		Status:      VersionFailed,
		ErrorKind:   kind,
	}, nil
}

// reserveBytes 按原始字节数预留摄取额度；空文件不占额度，返回 nil 票据。
// reserveBytes reserves the document's raw size; empty documents hold no ticket.
func (i *Ingestor) reserveBytes(ctx context.Context, p core.Principal, jobID string, size int64) (*QuotaTicket, error) {
	if size <= 0 {
		return nil, nil
	}
	ticket, err := i.quota.AdmitCall(ctx, p, QuotaCost{Bytes: size},
		"ingest:"+jobID+":bytes")
	quotaEvent := IngestQuotaDiagnostic{
		IngestJobID: jobID,
		StepID:      "reserve",
		Class:       "ingest",
	}
	if err != nil {
		quotaEvent.Decision = DiagnosticDecisionDeny
		quotaEvent.Reason = ingestQuotaReason(err)
		i.recordIngestQuota(ctx, quotaEvent)
		return nil, err
	}
	quotaEvent.Decision = DiagnosticDecisionAllow
	i.recordIngestQuota(ctx, quotaEvent)
	return &ticket, nil
}

// recordIngestQuota 发一条摄取配额判定事件。诊断可丢弃：端口缺装配或锚点为空时
// 静默跳过（不伪造关联），写失败由实现侧自行吞掉——它**不得**改变准入结果。
// recordIngestQuota emits one ingest quota verdict; it never changes the outcome.
func (i *Ingestor) recordIngestQuota(ctx context.Context, event IngestQuotaDiagnostic) {
	if i == nil || i.quotaDiag == nil || event.IngestJobID == "" {
		return
	}
	i.quotaDiag.RecordIngestQuota(ctx, event)
}

// ingestQuotaReason 把配额端口的错误翻译成稳定原因码。未知错误留空——契约允许
// `reason_code` 缺省，胡乱归因会把排障引到错误方向（与运行路径同一判据）。
// ingestQuotaReason maps the domain sentinels; anything else stays unattributed.
func ingestQuotaReason(err error) DiagnosticReason {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrQuotaExhausted):
		return DiagnosticReasonQuotaExhausted
	case errors.Is(err, ErrRateLimited):
		return DiagnosticReasonRateLimited
	case errors.Is(err, ErrConcurrencyLimited):
		return DiagnosticReasonConcurrencyLimited
	default:
		return ""
	}
}

// releaseToken 归还**未发出**调用的 Token 预留（审计预写失败走这个原因字面量）。
// releaseToken returns a reservation whose call provably never left the process.
func (i *Ingestor) releaseToken(ctx context.Context, ticket QuotaTicket) {
	if err := i.quota.Release(context.WithoutCancel(ctx), ticket, QuotaReleaseAuditFailed); err != nil {
		i.logger.WarnContext(ctx, "knowledge: quota release failed",
			"reservation_id", ticket.ReservationID, "reason", string(QuotaReleaseAuditFailed), "error", err)
	}
}

// releaseTicket 归还一段预留；nil 票据（空文件）为空操作。释放失败只记 WARN——
// 预留留在 `pending` 是保守方向，由管理面 `Reconcile` 收口。
// releaseTicket returns a reservation; a nil ticket is a no-op.
func (i *Ingestor) releaseTicket(ctx context.Context, ticket *QuotaTicket, reason QuotaReleaseReason) {
	if ticket == nil {
		return
	}
	if err := i.quota.Release(context.WithoutCancel(ctx), *ticket, reason); err != nil {
		i.logger.WarnContext(ctx, "knowledge: quota release failed",
			"reservation_id", ticket.ReservationID, "reason", string(reason), "error", err)
	}
}

// commitTicket 按实际用量结算；nil 票据为空操作。结算失败只记 WARN：预留留在
// `pending` 会继续占额度，宁可让管理员看到也不能谎报已结算。
// commitTicket settles actual usage; a nil ticket is a no-op.
func (i *Ingestor) commitTicket(ctx context.Context, ticket *QuotaTicket, usage QuotaUsage) {
	if ticket == nil {
		return
	}
	if err := i.quota.Commit(context.WithoutCancel(ctx), *ticket, usage); err != nil {
		i.logger.WarnContext(ctx, "knowledge: quota commit failed",
			"reservation_id", ticket.ReservationID, "error", err)
	}
}

// finishEmbedding 写审计终态。失败只记 WARN：调用已经发生，调用行停在 `pending`
// 由核对路径收口；**不得**把失败吞掉不记。写用 `WithoutCancel` + 独立短超时
// （`detachedWriteTimeout`，契约 §4.2）：脱取消是为了不丢终态，短超时是为了不挂死。
// finishEmbedding writes the audit terminal state; failures are logged, never dropped.
func (i *Ingestor) finishEmbedding(ctx context.Context, callID string, promptTokens int64, callErr error) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedWriteTimeout)
	defer cancel()
	if err := i.audit.FinishEmbedding(finishCtx, callID, promptTokens, callErr); err != nil {
		i.logger.WarnContext(ctx, "knowledge: embedding audit terminal write failed",
			"call_id", callID, "error", err)
	}
}

// embedInput 组装嵌入输入：标题路径参与语义（空标题只取正文）。检索侧必须用
// 同一形态构造查询上下文，否则输入分布不一致。
// embedInput joins the heading path and body; retrieval must use the same shape.
func embedInput(chunk ChunkDraft) string {
	if chunk.HeadingPath == "" {
		return chunk.Text
	}
	return chunk.HeadingPath + "\n" + chunk.Text
}

// estimateTokens 是**偏保守**的 Token 估算：按 1 字符 1 Token 计。低估会让 Commit
// 记 `overage`，而 `overage > 0` 期间该周期的新预留一律被拒到管理员核对清零
// （调用方配额实现的 `ClearOverage`）；高估只在调用期间多占额度，Commit 后按
// 实际归还——因此宁可高估。估算值只用于预留，结算以提供方回执为准。
// estimateTokens intentionally over-estimates: under-estimation books overage,
// which blocks every later reservation until an admin clears it.
func estimateTokens(texts []string) int64 {
	var total int64
	for _, text := range texts {
		total += int64(len([]rune(text)))
	}
	if total < 1 {
		total = 1
	}
	return total
}

// hashText 是片段正文的 sha256（64 位小写十六进制），与 `kb_chunks.content_hash` 同形。
// hashText returns the lower-case hex sha256 of a chunk body.
func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// newRandomID 生成作业 / 尝试标识：固定前缀 + 12 字节随机数的十六进制。
// 标识**不是**凭据，随机只为避免并发撞号；数据库主键仍兜底唯一性。
// newRandomID builds an opaque id; randomness only avoids collisions.
func newRandomID(prefix string) (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("knowledge: 生成标识失败: %w", err)
	}
	return prefix + hex.EncodeToString(buf[:]), nil
}

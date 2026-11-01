package knowledge

// 本文件声明摄取侧的**窄端口与数据类型**（US-16 §2.3 图 3 / §3.1.3 / T034）：
// `Ingestor` 只依赖这里声明的接口，实现由 `internal/storage/postgres`、配额限制器
// 与 embedding 客户端在 `app` 层注入——`knowledge` **不**反向依赖它们（消费方声明
// 接口，见技术方案 §7.1.2 第 2 条）。
//
// 三条口径：
//  1. **状态机逐字对齐冻结契约**：版本 `pending → indexed → active`，失败 `failed`；
//     `active` 是唯一生效点，旧版本在新生效前继续服务；
//  2. **成本与配额**用包内窄值类型（`quota.go` 的 `QuotaCost` / `QuotaTicket` /
//     `QuotaUsage` / `QuotaReleaseReason`），**不** import `internal/callers`
//     （技术方案 §7.1.2）——实现由 `app` 适配器注入，两边不会各写一套记账语义；
//  3. **身份显式**：请求携带 `core.Principal`，零值在读取任何正文之前拒绝。
//
// This file declares the ingest ports and value types. The Ingestor depends only on
// these interfaces; concrete stores, limiters and embedding clients are injected by
// the app layer, so knowledge never imports a storage, callers or diagnostics package.

import (
	"context"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// VectorDimensions 是 `kb_chunk_embeddings.embedding` 的冻结维度（D-05：`vector(512)`）。
// 配置维度与之不一致时摄取在读取正文前拒绝，**不得**让写入在数据库 CHECK 上才失败。
// VectorDimensions is the frozen embedding width; a config mismatch is rejected
// before any content is read rather than failing later on the column CHECK.
const VectorDimensions = 512

// ParserVersion 是纯文本解析器的版本字面量：`kb_document_versions.parser_version`
// 是 NOT NULL 列，每次尝试都要能回答"用哪一版解析器产的切块"。
// ParserVersion labels the plain-text parser; the version row records it per attempt.
const ParserVersion = "text-v1"

// VersionStatus 是 `kb_document_versions.status` 的六值闭集（契约冻结）。
// VersionStatus is the six-value closed set of the version status column.
type VersionStatus string

const (
	// VersionPending 表示版本行已建、产物未完成（新版本初始状态）。
	// VersionPending is the initial state of a freshly created version.
	VersionPending VersionStatus = "pending"
	// VersionIndexed 表示切块与向量已写入并通过校验，尚未生效。
	// VersionIndexed means the artifacts exist and passed verification but are not live.
	VersionIndexed VersionStatus = "indexed"
	// VersionActive 表示版本生效，唯一参与检索的状态。
	// VersionActive is the only state visible to retrieval.
	VersionActive VersionStatus = "active"
	// VersionSuperseded 表示被同文档的新 active 版本取代。
	// VersionSuperseded means a newer active version of the same document replaced it.
	VersionSuperseded VersionStatus = "superseded"
	// VersionRetired 表示因删除或撤权退出（保留行供引用失效判定）。
	// VersionRetired marks deletion or revocation; the row stays for citation checks.
	VersionRetired VersionStatus = "retired"
	// VersionFailed 表示任一步失败；重试按内容哈希决定复用或新建版本行。
	// VersionFailed marks a failed attempt; retry can reuse the latest matching failed row.
	VersionFailed VersionStatus = "failed"
)

// Valid 判定状态是否在闭集内；空值与未知值一律拒绝。
// Valid accepts exactly the six literals; empty and unknown values fail.
func (s VersionStatus) Valid() bool {
	switch s {
	case VersionPending, VersionIndexed, VersionActive, VersionSuperseded, VersionRetired, VersionFailed:
		return true
	default:
		return false
	}
}

// 摄取作业的 `stage` / `status` 字面量：stage 取契约列注释的五个阶段加 `done`，
// status 取 `kb_ingest_jobs.status` 的冻结闭集（本模块只产 success / failed；
// pending / running 由存储层在开作业时写入，cancelled 留给取消路径）。
// Job stage and status literals come from the schema comments and CHECK.
const (
	// JobStageParse 是解析阶段。
	JobStageParse = "parse"
	// JobStageChunk 是切分阶段。
	JobStageChunk = "chunk"
	// JobStageEmbed 是分批 embedding 阶段。
	JobStageEmbed = "embed"
	// JobStageIndex 是校验与生效阶段。
	JobStageIndex = "index"
	// JobStageDone 表示作业正常走完。
	JobStageDone = "done"

	// JobStatusSuccess 表示作业成功。
	JobStatusSuccess = "success"
	// JobStatusFailed 表示作业失败。
	JobStatusFailed = "failed"
)

// IngestRequest 是一次摄取请求：调用方身份、目标集合与工作区相对路径。
// 路径的**真实投影与允许根校验**由 `DocumentReader` 实现负责；本包只查扩展名。
// IngestRequest carries the caller, collection and workspace-relative path; the
// reader owns real-path projection, this package only checks the extension.
type IngestRequest struct {
	// Principal 是发起摄取的调用方身份；零值拒绝。
	Principal core.Principal
	// CollectionID 是目标集合。
	CollectionID string
	// Path 是工作区允许根内的相对路径（`.md` / `.txt`）。
	Path string
}

// IngestResult 是摄取结果（契约 `IngestResult` 的领域形状）。
// IngestResult mirrors the contract's IngestResult.
type IngestResult struct {
	// DocumentID 是文档标识；去重命中时指向已有文档。
	DocumentID string
	// VersionID 是版本标识；去重命中时指向已有 active 版本。
	VersionID string
	// IngestJobID 是本次作业标识；去重命中时为空（没有开新作业）。
	IngestJobID string
	// Status 是版本终态：active 或 failed（去重时是已有版本的 active）。
	Status VersionStatus
	// Deduplicated 表示同集合已有同内容哈希的 active 版本，未新建版本。
	Deduplicated bool
	// ChunkCount 是本次写入的切块数；未写入时为零。
	ChunkCount int
	// Generation 是向量写入的索引代际；未写向量时为零。
	Generation int
	// ErrorKind 仅 failed 时非空，取 `ErrorKind*` 的冻结六值之一。
	ErrorKind string
}

// DeleteRequest 软删一个文档或一个集合；两者**恰有一个**非空。
// DeleteRequest soft-deletes exactly one document or collection.
type DeleteRequest struct {
	// Principal 是发起删除的调用方身份；零值拒绝。
	Principal core.Principal
	// DocumentID 与 CollectionID 恰有一个非空。
	DocumentID string
	// CollectionID 与 DocumentID 恰有一个非空。
	CollectionID string
}

// ReindexRequest 按文档现有来源路径重建索引；内容未变时是幂等的去重命中。
// ReindexRequest re-indexes a document at its stored path; unchanged content is an
// idempotent dedup hit, because the schema allows one version per content hash.
type ReindexRequest struct {
	// Principal 是发起重建的调用方身份；零值拒绝。
	Principal core.Principal
	// AuditActor is the authenticated HTTP actor, never a credential value.
	AuditActor core.Actor
	// DocumentID 是待重建的文档。
	DocumentID string
}

// DocumentSource 是读取并解码后的一份文档：正文、原始字节哈希与字节数。
// DocumentSource is one decoded document; the hash always covers the raw bytes.
type DocumentSource struct {
	// Path 是允许根内的相对路径，原样写入 `kb_documents.source_path`。
	Path string
	// Text 是 UTF-8 解码后的正文。
	Text string
	// ContentHash 是原始字节的 sha256（64 位小写十六进制）。实现**必须**保证它同时
	// 等于 `Text` 的 sha256：来源视图靠这个等式判定"重建结果就是原文"，读取时若做
	// BOM / 换行归一化，必须先归一化再算哈希，否则正文永远无法逐字回读。
	ContentHash string
	// SizeBytes 是原始字节数，字节配额按它预留。
	SizeBytes int64
}

// ChunkDraft 是切分器产出的一段：位置、标题路径、正文与片段哈希。
// ChunkDraft is one chunk produced by the chunker.
type ChunkDraft struct {
	// Seq 是版本内序号，从 1 起且连续。
	Seq int
	// HeadingPath 是标题路径（"A > B > C"），进嵌入输入。
	HeadingPath string
	// CharStart / CharEnd 是原文中的字符区间，左闭右开。
	CharStart int
	// CharEnd 必须大于 CharStart。
	CharEnd int
	// Text 是片段正文，不得为空。
	Text string
	// ContentHash 是片段正文的 sha256。
	ContentHash string
}

// ChunkRef 是写库后返回的片段标识，与 `WriteChunks` 入参**同序**。
// ChunkRef is the persisted chunk id, returned in the same order as the drafts.
type ChunkRef struct {
	// ChunkID 是片段主键。
	ChunkID string
	// Seq 是版本内序号，与对应草稿一致。
	Seq int
}

// EmbedBatch 是一批 embedding 的提供方回执。
// EmbedBatch is the provider response for one embedding batch.
type EmbedBatch struct {
	// Vectors 与输入文本一一对应，顺序不得重排。
	Vectors [][]float32
	// PromptTokens 是回执的输入 Token 数；0 表示**未知**，预留必须保持 pending。
	PromptTokens int64
	// Dimensions 是提供方声明的维度；0 时按首条向量长度推断。
	Dimensions int
	// Model 是实际使用的模型 id。
	Model string
}

// IngestConfig 是摄取侧的装配参数（配置键见设计 §2.16：`embedding.*` /
// `knowledge.*`；`generation` 由启动期读 active 代际后注入）。
// IngestConfig carries the assembly parameters for ingestion.
type IngestConfig struct {
	// Generation 是向量写入的索引代际（必须已存在于 `kb_index_generations`）。
	Generation int
	// EmbeddingModel 是 embedding 模型 id（写进 `embedding_calls` 与代际）。
	EmbeddingModel string
	// EmbeddingDimensions 必须等于 `VectorDimensions`，否则拒绝装配。
	EmbeddingDimensions int
	// EmbeddingBatchSize 是单次 embedding 的文本上限。
	EmbeddingBatchSize int
	// MaxDocumentBytes 是单文档字节上限。
	MaxDocumentBytes int64
	// AllowedExtensions 是允许摄取的扩展名（含点，比较时大小写不敏感）。
	AllowedExtensions []string
	// ChunkerVersion 是 `knowledge.chunk.version` 的取值，逐次写入版本行。
	ChunkerVersion string
}

// StartJobRequest 是开作业的入参：作业标识由摄取侧生成（配额幂等键要先用到它），
// 文档行与版本行的标识由存储层分配。
// StartJobRequest opens a job; the ingestor generates the job id because the quota
// idempotency key needs it before the first row exists.
type StartJobRequest struct {
	// ForceNewVersion keeps an explicit rebuild independent from the active snapshot.
	ForceNewVersion bool
	// ExpectedDocumentID makes a rebuild fail if its original document disappeared.
	ExpectedDocumentID string
	// JobID 是摄取侧生成的作业标识。
	JobID string
	// CollectionID 是目标集合。
	CollectionID string
	// SourcePath 是允许根内的相对路径。
	SourcePath string
	// ContentHash 是原始字节的 sha256。
	ContentHash string
	// SourceText 是本次读到的版本原文，落 `kb_document_versions.source_text` 供来源
	// 视图逐字回读。切分器会裁掉标题行与首尾空白，片段重建不出原文，**不得**把重建
	// 结果当成原文；空串表示未提供，该版本只能回状态。
	// SourceText is the version body persisted for verbatim readback; it is never
	// reconstructed from chunks, which drop heading lines and surrounding whitespace.
	SourceText string
	// ParserVersion 是解析器版本，写入新版本行。
	ParserVersion string
	// ChunkerVersion 是切分器版本，写入新版本行。
	ChunkerVersion string
	// At 是建行时刻（Unix 毫秒取整由存储层负责）。
	At time.Time
}

// ReindexAuthorization rechecks a rebuild's current authority and commits its
// activation under the same authority. Both operations must fail closed.
type ReindexAuthorization interface {
	AuthorizeReindex(ctx context.Context, principal core.Principal, documentID string) error
	ActivateReindex(ctx context.Context, principal core.Principal, versionID string, generation int, at time.Time) (string, error)
}

// DocumentRecord 是文档行视图（重建索引与删除判定共用）。
// DocumentRecord is the document row view used by reindex and delete.
type DocumentRecord struct {
	// DocumentID 是文档主键。
	DocumentID string
	// CollectionID 是所属集合。
	CollectionID string
	// SourcePath 是允许根内的相对路径。
	SourcePath string
	// ContentHash 是当前最新版本的内容哈希。
	ContentHash string
	// Deleted 表示已软删；已删文档不得重建索引。
	Deleted bool
}

// VersionRecord 是去重路径返回的版本行视图。
// VersionRecord is the version row returned by the dedup lookup.
type VersionRecord struct {
	// DocumentID 是所属文档。
	DocumentID string
	// VersionID 是版本主键。
	VersionID string
	// ContentHash 是版本内容哈希。
	ContentHash string
	// Status 是版本当前状态；去重只命中 active。
	Status VersionStatus
	// ChunkCount 是该版本的切块数。
	ChunkCount int
	// Generation 是向量所在代际；无向量时为 0。
	Generation int
}

// JobRecord 是开作业后返回的行视图。
// JobRecord is the row view returned after a job is opened.
type JobRecord struct {
	// JobID 是作业主键（与请求一致）。
	JobID string
	// DocumentID 是分配或命中的文档。
	DocumentID string
	// VersionID 是新建或复用的版本。
	VersionID string
	// Attempt 是本次尝试序号，从 1 起。
	Attempt int
}

// IngestStore 是摄取侧的持久化窄端口。实现必须保证：
//   - `StartJob` 原子地建（或命中）文档行、建（或复用）版本行并开作业；
//     失败重试可复用最近的同内容失败行；显式重建必须新建版本，历史版本不得覆写；
//   - `ActivateVersion` 是**唯一生效点**：把目标版本置 `active`、同文档旧 active
//     置 `superseded`、更新 `latest_version_id`，三件事同一事务；
//   - 所有时间入参按 Unix 毫秒 UTC 落库。
//
// IngestStore is the persistence port. StartJob is atomic and reuses the version row
// when the content hash is unchanged (the schema forbids a duplicate); ActivateVersion
// is the single visibility point and flips the old active version in one transaction.
type IngestStore interface {
	// FindActiveByContentHash 在同集合内按内容哈希找 active 版本（去重路径）。
	FindActiveByContentHash(ctx context.Context, collectionID, contentHash string) (VersionRecord, bool, error)
	// StartJob 建立或命中文档行与版本行（`pending`），并开一条 `running` 作业。
	StartJob(ctx context.Context, req StartJobRequest) (JobRecord, error)
	// WriteChunks 在版本维度写入切块，返回与草稿同序的片段标识。
	WriteChunks(ctx context.Context, versionID string, chunks []ChunkDraft) ([]ChunkRef, error)
	// MarkIndexed 把版本从 `pending` 置 `indexed`：产物齐全且通过校验、尚未生效。
	MarkIndexed(ctx context.Context, versionID string, at time.Time) error
	// ActivateVersion 把版本置 `active` 并把同文档旧 active 置 `superseded`，
	// 返回被取代的版本标识（无旧版本时为空串）。
	ActivateVersion(ctx context.Context, versionID string, generation int, at time.Time) (string, error)
	// FailVersion 把版本置 `failed` 并写 `error_kind`。
	FailVersion(ctx context.Context, versionID, errorKind string, at time.Time) error
	// FinishJob 写作业终态（stage / status / error_kind）。
	FinishJob(ctx context.Context, jobID, stage, status, errorKind string, at time.Time) error
	// Document 读文档行；不存在返回 `ErrNotFound`。
	Document(ctx context.Context, documentID string) (DocumentRecord, error)
	// SoftDeleteDocument 软删文档并把其 active 版本置 `retired`。
	SoftDeleteDocument(ctx context.Context, documentID string, at time.Time) error
	// SoftDeleteCollection 软删集合并把集合内 active 版本置 `retired`。
	SoftDeleteCollection(ctx context.Context, collectionID string, at time.Time) error
}

// DocumentReader 读取工作区允许根内的相对路径并解码正文。实现**必须**先做真实
// 路径投影再比前缀（不得只做字符串前缀比较）、拒绝符号链接 / 联接 / `..` /
// 盘符 / UNC，并在读取前用 `MaxDocumentBytes` 截断超限文件。
// DocumentReader loads and decodes one document. Implementations must project the
// real path before comparing prefixes, reject links and traversal, and enforce the
// size cap before reading.
type DocumentReader interface {
	Load(ctx context.Context, path string) (DocumentSource, error)
}

// Chunker 把文档切成带位置与标题路径的片段，并记录 `chunker_version`。
// Chunker splits a document into located chunks; the version comes from config.
type Chunker interface {
	Split(ctx context.Context, src DocumentSource) ([]ChunkDraft, error)
}

// Embedder 是 embedding 协议的薄封装：只做调用，不重排、不截断、不补默认值。
// Embedder is a thin protocol client: no reordering, truncation or defaults.
type Embedder interface {
	Embed(ctx context.Context, texts []string) (EmbedBatch, error)
}

// VectorIndex 是向量写入与代际切换的**唯一入口**：摄取只调 `Write`；`Activate`
// 与 `Retire` 由代际管理（T037）调用，**不得**在版本摄取里顺手切换代际。
// VectorIndex owns vector writes and generation switches; ingest only calls Write.
type VectorIndex interface {
	Write(ctx context.Context, versionID string, chunks []ChunkRef, vectors [][]float32, generation int) error
	Activate(ctx context.Context, versionID string, generation int) error
	Retire(ctx context.Context, versionID string) error
}

// EmbeddingAudit 是 embedding 调用的两阶段审计端口：先写 `pending` 拿回调用标识，
// 返回后写终态。预写失败**必须**让调用不发出（fail-closed）。
// EmbeddingAudit is the two-phase audit port; a failed pre-write must stop the call.
type EmbeddingAudit interface {
	BeginEmbedding(ctx context.Context, jobID, model string, inputCount int) (callID string, err error)
	FinishEmbedding(ctx context.Context, callID string, promptTokens int64, callErr error) error
}

// IngestorDeps 是装配依赖。`Logger` / `Now` / `NewID` 留空时用包内默认值；
// 其余端口缺一即视为装配错误（`Ingest` 在读取正文前返回 `ErrIngestUnavailable`）。
// IngestorDeps carries the assembly dependencies; a missing port fails closed.
type IngestorDeps struct {
	// Store 是持久化端口。
	Store IngestStore
	// Reader 是文档读取端口。
	Reader DocumentReader
	// Chunker 是切分端口。
	Chunker Chunker
	// Embedder 是 embedding 端口。
	Embedder Embedder
	// Vectors 是向量写入端口。
	Vectors VectorIndex
	// Audit 是 embedding 两阶段审计端口。
	Audit EmbeddingAudit
	// Quota 是配额预留端口。
	Quota Quota
	// QuotaDiagnostics 是配额判定的诊断写端口（V-8）；可空——诊断可丢弃，
	// 缺装配只少一条可查询记录，不改变准入行为。
	// QuotaDiagnostics is the optional quota-decision diagnostic port.
	QuotaDiagnostics IngestQuotaDiagnosticRecorder
	// Config 是摄取参数。
	Config IngestConfig
	// Logger 是运行日志目标；缺省用 `slog.Default()`（与 `memory.Options` 同口径）。
	Logger *slog.Logger
	// Now 是时钟；缺省 `time.Now`，测试注入固定时刻。
	Now func() time.Time
	// NewID 生成作业 / 尝试标识；缺省用 crypto/rand 的十六进制。
	NewID func(prefix string) (string, error)
}

package web

// 本文件是**知识库读写面的消费方端口**（US-16 / T042，颗粒度文档 §3.1.7 的
// 「知识端点」行；端点清单以冻结契约 `docs/contracts/openapi.yaml` 的知识组为准，共
// 十个）。四条口径：
//
//  1. 摄取 / 检索 / 来源视图三类直接复用 `internal/knowledge` 的领域类型——它们是
//     冻结设计卡 §3.1.3 的导出面，web 只是转接，**不**另造同义类型（与 T026 的
//     `ports_callers.go` 同一先例）；`*knowledge.Ingestor` / `*knowledge.Retriever` /
//     `*knowledge.SourceViewer` 结构化满足前两个端口，不需要适配器。
//  2. 集合 / 文档 / 版本三个**只读列表**在冻结设计里没有领域类型，也没有存储端口
//     （设计 §3.1.3 交付物表零命中）。按「接口定义在消费方」的包纪律，端口与记录
//     形状声明在本文件，由 app 层的适配器实现；web 不 import `internal/storage`。
//  3. 检索端点的 `query → 向量 → 固定代际` 三步属于装配层：web 不知道 embedding
//     客户端，也不持有索引代际，只把线上查询形状转成 `KnowledgeSearchQuery`。
//  4. **装配缺失按失败关闭**：端口为 nil 时十个端点一律固定 500（`writeKnowledgePortMissing`），
//     **不得**用空列表冒充"没有集合 / 文档"，那会把装配故障显示成一个合法的空状态。
//
// This file declares the consumer ports of the knowledge surface. Ingest, search and
// source view reuse the domain types of internal/knowledge (structural satisfaction,
// no adapters); the three read-only lists have no domain type in the frozen design, so
// their port and record shapes live here per the "interfaces are declared by the
// consumer" package rule. A nil port fails closed with a fixed 500.

import (
	"context"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// KnowledgeUpload saves a new workspace source file without indexing it.
type KnowledgeUpload interface {
	Upload(ctx context.Context, actor core.Actor, name, content string) (string, error)
}

// KnowledgeIngest 是摄取面的消费方端口：摄取、软删（文档或集合二选一）、重建索引。
// 方法逐一对应 `*knowledge.Ingestor` 的同名方法——端口只做依赖倒置，不补默认值、
// 不吞错误。删除与重建是 `knowledge:write`，摄取计入 `IngressIngest` 入口桶。
// KnowledgeIngest mirrors *knowledge.Ingestor method for method.
type KnowledgeIngest interface {
	Ingest(ctx context.Context, req knowledge.IngestRequest) (knowledge.IngestResult, error)
	Delete(ctx context.Context, req knowledge.DeleteRequest) error
	Reindex(ctx context.Context, req knowledge.ReindexRequest) (knowledge.IngestResult, error)
}

// KnowledgeSearchQuery 是一次管理面检索的**线上形状**（契约 `SearchRequest`）。
// 不含 embedding 与代际：调用方给的是文本，算向量与固定索引代际由适配器负责——
// 让 HTTP 层决定代际会把"这一轮用哪一代索引"泄漏成客户端参数。
// KnowledgeSearchQuery is the wire-shaped query; the adapter embeds it and pins the
// generation, so neither leaks into the HTTP layer.
type KnowledgeSearchQuery struct {
	// Principal 是发起检索的调用方身份；零值由领域拒绝。
	Principal core.Principal
	// Query 是查询文本；空串由领域按 400 拒绝。
	Query string
	// Collections 是显式集合范围；与 Agent 至少给一个（不做全库检索）。
	Collections []string
	// Agent 是按 `AGENT.md` 集合声明解析范围时的 Agent 名。
	Agent string
	// TopK 是最终命中数；<=0 取配置 / 文档默认值。
	TopK int
	// Budget 是结果切片的 token 预算上限；<=0 表示不裁剪。
	Budget int
}

// KnowledgeSearch 是管理面检索的消费方端口。实现（app 层适配器）负责：算查询
// 向量、固定当前 active 索引代际、调用 `Retriever.Search`，并把**不可分类**的
// 检索失败收敛成 `knowledge.ErrRetrieverUnavailable`——契约 `/search` 只有 503
// `NOT_CONFIGURED` 这一个 5xx，降级成 200 + 空命中会让调用方以为"查过了"。
// KnowledgeSearch is the management search port; the adapter embeds the query, pins
// the generation, and converges unclassifiable failures onto ErrRetrieverUnavailable.
type KnowledgeSearch interface {
	Search(ctx context.Context, q KnowledgeSearchQuery) (knowledge.SearchResult, error)
}

// KnowledgeSource 是来源视图的消费方端口：按 `EvidenceRef` 精确读取，**不经过**
// 相似度；每次调用都按当前 Principal 重查 ACL / 删除 / 版本（C-26）。
// KnowledgeSource performs the exact read; every call re-checks the current state.
type KnowledgeSource interface {
	SourceView(ctx context.Context, p core.Principal, req knowledge.SourceViewRequest) (knowledge.SourceViewResult, error)
}

// KnowledgeCollection 是集合行的消费方记录（契约 `CollectionSummary` 的投影来源）。
// `DocumentCount` 由适配器按"未删除文档数"统计；取不到时返回 -1，视图层据此省略
// 该字段——0 与"没统计"必须可区分，否则页面会把统计失败显示成"空集合"。
// KnowledgeCollection is the catalog's collection row; a negative DocumentCount means
// "not counted" and is omitted, because 0 is a real answer.
type KnowledgeCollection struct {
	CollectionID        string
	Name                string
	Project             string
	Description         string
	DescriptionRevision int64
	DeclaredAgents      []string
	DocumentCount       int
	CreatedAt           time.Time
	DeletedAt           time.Time
}

// KnowledgeDocument 是文档行的消费方记录（契约 `DocumentSummary` 的来源）。
// `LatestStatus` 取 `knowledge.VersionStatus` 的六值闭集，空串表示尚无版本。
// KnowledgeDocument is the catalog's document row; an empty LatestStatus means the
// document has no version yet.
type KnowledgeDocument struct {
	DocumentID      string
	CollectionID    string
	SourcePath      string
	ContentHash     string
	LatestVersionID string
	LatestStatus    string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       time.Time
}

// KnowledgeDocumentVersion 是版本行的消费方记录（契约 `DocumentVersionSummary`
// 的来源）。`ActivatedAt` 零值表示尚未激活（视图渲染空串）。
// KnowledgeDocumentVersion is the catalog's version row; a zero ActivatedAt renders
// as an empty string.
type KnowledgeDocumentVersion struct {
	VersionID      string
	DocumentID     string
	Revision       int64
	ContentHash    string
	Status         string
	ParserVersion  string
	ChunkerVersion string
	Generation     int
	ErrorKind      string
	CreatedAt      time.Time
	ActivatedAt    time.Time
}

// KnowledgeCatalog 是集合 / 文档 / 版本三个只读列表的消费方端口（设计 §3.1.7 未
// 给出领域类型，端口归 web 声明、app 层适配）。四个方法都是**只读**：
//
//   - `ListCollections` 只服务管理面 `GET /collections`（契约无 operation security →
//     继承根级 `security` → 管理面 admin token），因此**没有** Principal 参数；
//   - 其余三个服务业务面，实现**必须**按传入 Principal 重查集合 ACL 并把越权目标按
//     `knowledge.ErrForbidden` 拒绝；不存在 / 已软删按 `knowledge.ErrNotFound`；
//   - 排序是契约固定口径（集合 `name` 升序、文档 `source_path` 升序、版本
//     `created_at` 倒序），由存储的 SQL 保证，**不得**提供排序参数。
//
// The catalog backs the three read-only lists; the admin-only list takes no principal,
// the other three must re-check the collection ACL and fail closed with the knowledge
// sentinels. Ordering is fixed by the contract and belongs to the SQL.
type KnowledgeCatalog interface {
	ListCollections(ctx context.Context, project string, includeDeleted bool) ([]KnowledgeCollection, error)
	CreateCollection(ctx context.Context, p core.Principal, name, project string) (KnowledgeCollection, error)
	ListDocuments(ctx context.Context, p core.Principal, collectionID string, includeDeleted bool) ([]KnowledgeDocument, error)
	ListVersions(ctx context.Context, p core.Principal, documentID string) ([]KnowledgeDocumentVersion, error)
}

// KnowledgeDescriptionAdmin is the authenticated management write for collection metadata.
type KnowledgeDescriptionAdmin interface {
	UpdateDescription(ctx context.Context, actor core.Actor, id, description string, expected int64) (int64, error)
}

type KnowledgeIngestAttempt struct {
	JobID      string
	VersionID  string
	Attempt    int
	Stage      string
	Status     string
	ErrorKind  string
	StartedAt  time.Time
	FinishedAt time.Time
}

type KnowledgeIngestHistory interface {
	ListIngestAttempts(ctx context.Context, documentID string) ([]KnowledgeIngestAttempt, bool, error)
}

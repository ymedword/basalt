package web

// 本文件是知识库十个端点的处理函数（US-16 / T042；端点清单以冻结契约
// `docs/contracts/openapi.yaml` 的知识组为准）。四条口径：
//
//  1. **本层只做线上形状**——解码、必填判定、`top_k` / `budget_tokens` 的契约区间、
//     集合名的空白与控制字符。路径真实投影、ACL、去重、版本状态机全在
//     `internal/knowledge` 与存储适配器里；本层**不**复制第二套判定。
//  2. **失败分类只认哨兵**（`errors.Is`），**不**比对错误文本；底层原因只进日志。
//  3. `/search` 的两路皆失败按契约回 503 `NOT_CONFIGURED` 并带 `data.reason_code`：
//     这个端点**不得**用 500 表示检索不可用——503 才是调用方重试 / 降级的依据，
//     而 `reason_code` 是唯一能让调用方区分"向量路没了"与"关键词路没了"的字段。
//  4. 缺端口是**装配错误**：十个端点一律固定 500，**不得**用空列表或空结果冒充
//     "没有集合 / 没有命中"，那会把装配故障显示成一个合法的空状态。
//
// This file holds the ten knowledge handlers. It validates wire shape only; the domain
// owns paths, ACL, deduplication and version state. A search failure returns 503 with
// data.reason_code — never a 500 and never a silent empty result.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// reasonVectorUnavailable 是 `/search` 503 的 `data.reason_code` 取值，逐字取自冻结
// 契约 `SearchResult.degraded_reason` 与设计 §2.3 图 4 的同一枚举。两路皆失败时设计
// **没有**给出专门的码；取向量路的值，因为它是更稀缺的依赖，且图 4 的降级箭头的
// 原因码就是它（`vector_unavailable`）。**不得**新造第七个原因码。
// reasonVectorUnavailable is the frozen literal; both paths failing has no dedicated
// code in the design, so the vector value is used and no seventh code is invented.
const reasonVectorUnavailable = "vector_unavailable"

// 知识端点的固定请求形状文案：全部由本层产出，不含底层错误文本。
// Fixed request-shape messages produced by this layer only.
const (
	messageCollectionCreateBody = "请求体不是合法的集合创建请求"
	messageCollectionNameEmpty  = "集合名不能为空或只含空白"
	messageCollectionNameBad    = "集合名不得包含控制字符"
	messageDocumentIngestBody   = "请求体不是合法的文档摄取请求"
	messageDocumentPathEmpty    = "path 不能为空"
	messageSearchBody           = "请求体不是合法的检索请求"
	messageSearchQueryEmpty     = "query 不能为空"
	messageSearchScopeEmpty     = "collections 与 agent 至少给一个（不做全库检索）"
	messageSearchTopKRange      = "top_k 必须在 1 到 50 之间"
	messageSearchBudgetRange    = "budget_tokens 必须大于 0"
	messageSourceViewBody       = "请求体不是合法的来源视图请求"
	messageSourceViewRefMissing = "ref 必须同时给出 id 与 version_id"
	messageSourceViewBudget     = "budget_tokens 必须大于 0"
	messageCollectionNotFound   = "未找到集合：%s"
	messageDocumentNotFound     = "未找到文档：%s"
	messageCollectionConflict   = "同一 project 下已存在同名活动集合"
	messageKnowledgeForbidden   = "无权访问该知识库资源"
	messagePathRejected         = "来源路径非法：只接受工作区允许根内的 .md / .txt 文件（大小与编码同样受限）"
	messageKnowledgeInvalid     = "知识请求参数非法"
	messageIngestUnavailable    = "知识摄取未配置：请检查 embedding 模型连接与知识库配置"
	messageRetrieverUnavailable = "知识检索不可用：请检查 embedding 模型连接与索引状态"
	messageQueryBoolBad         = "include_deleted 只接受 true 或 false"
)

// maxSearchTopK 是契约 `SearchRequest.top_k` 的上界（minimum 1 / maximum 50）。
// maxSearchTopK mirrors the contract's top_k bounds.
const maxSearchTopK = 50

// collectionCreateWrite 是 `CollectionCreateRequest` 的线上形状（name 必填）。
// collectionCreateWrite mirrors the contract; name is required.
type collectionCreateWrite struct {
	Name    *string `json:"name"`
	Project *string `json:"project"`
}

// documentIngestWrite 是 `DocumentIngestRequest` 的线上形状（path 必填）。
// documentIngestWrite mirrors the contract; path is required.
type documentIngestWrite struct {
	Path *string `json:"path"`
}

// searchWrite 是 `SearchRequest` 的线上形状（query 必填）。
// searchWrite mirrors the contract; query is required.
type searchWrite struct {
	Query        *string  `json:"query"`
	Collections  []string `json:"collections"`
	Agent        *string  `json:"agent"`
	TopK         *int     `json:"top_k"`
	BudgetTokens *int     `json:"budget_tokens"`
}

// evidenceRefWrite 是 `EvidenceRef` 的线上形状（id / version_id 必填）。
// evidenceRefWrite mirrors the contract; id and version_id are required.
type evidenceRefWrite struct {
	ID          string `json:"id"`
	VersionID   string `json:"version_id"`
	ContentHash string `json:"content_hash"`
}

// evidenceRangeWrite 是 `EvidenceRange` 的线上形状（字符偏移，左闭右开）。
// evidenceRangeWrite mirrors the contract's rune-offset range.
type evidenceRangeWrite struct {
	Start *int `json:"start"`
	End   *int `json:"end"`
}

// sourceViewWrite 是 `SourceViewRequest` 的线上形状。**没有** principal 字段：身份只
// 来自请求上下文（契约明文），把身份塞进 body 会让"读谁的内容"变成客户端说了算。
// sourceViewWrite has no principal field by contract: identity is contextual only.
type sourceViewWrite struct {
	Ref            *evidenceRefWrite   `json:"ref"`
	Range          *evidenceRangeWrite `json:"range"`
	BudgetTokens   *int                `json:"budget_tokens"`
	Cursor         *string             `json:"cursor"`
	IncludeFacts   *bool               `json:"include_facts"`
	IncludeSummary *bool               `json:"include_summary"`
}

// handleListCollections 处理 `GET /collections`：**管理面**只读列表。契约该端点没有
// operation security，继承根级 `security`（bearerAuth / localTrust），因此它走管理面
// 而不是业务面——调用方 Key 列不出别人的集合。固定 `name` 升序由存储 SQL 保证。
// handleListCollections is the management-surface list; it takes no caller principal.
func (s *Server) handleListCollections(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.KnowledgeCatalog == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeCatalog", "GET /collections")
		return
	}
	includeDeleted, ok := s.queryBool(w, r, "include_deleted")
	if !ok {
		return
	}
	collections, err := s.opts.KnowledgeCatalog.ListCollections(
		r.Context(), r.URL.Query().Get("project"), includeDeleted)
	if err != nil {
		s.writeKnowledgeError(w, err, "列集合失败", "")
		return
	}
	s.writeOK(w, collectionSummaryViews(collections))
}

// handleCreateCollection 处理 `POST /collections`：`knowledge:write`。同名活动集合 →
// 409；名非法 → 400 且零写入（校验在调用端口之前）。
// handleCreateCollection rejects a bad name before the port is called, so nothing is
// written on a 400.
func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.KnowledgeCatalog == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeCatalog", "POST /collections")
		return
	}
	var body collectionCreateWrite
	if !s.decodeBody(w, r, &body) {
		return
	}
	if body.Name == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageCollectionCreateBody)
		return
	}
	name, reason := normalizeCollectionName(*body.Name)
	if reason != "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, reason)
		return
	}
	project := ""
	if body.Project != nil {
		project = *body.Project
	}
	collection, err := s.opts.KnowledgeCatalog.CreateCollection(r.Context(), principal, name, project)
	if err != nil {
		s.writeKnowledgeError(w, err, "建集合失败", "")
		return
	}
	s.writeOK(w, collectionSummaryView(collection))
}

// handleDeleteCollection 处理 `DELETE /collections/{id}`：`knowledge:write`，软删。
// 重复删除幂等（返回 200），**不**物理删除，**不**级联改历史引用。
// handleDeleteCollection soft-deletes; a repeated delete is idempotent.
func (s *Server) handleDeleteCollection(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.KnowledgeIngest == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeIngest", "DELETE /collections/{id}")
		return
	}
	id := r.PathValue("id")
	err := s.opts.KnowledgeIngest.Delete(r.Context(), knowledge.DeleteRequest{
		Principal:    principal,
		CollectionID: id,
	})
	if err != nil {
		s.writeKnowledgeError(w, err, "软删集合失败", fmt.Sprintf(messageCollectionNotFound, id))
		return
	}
	s.writeOK(w, nil)
}

// handleListCollectionDocuments 处理 `GET /collections/{id}/documents`：
// `knowledge:read`，固定 `source_path` 升序（排序在存储 SQL 里）。
// handleListCollectionDocuments lists one collection's documents in source_path order.
func (s *Server) handleListCollectionDocuments(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.KnowledgeCatalog == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeCatalog", "GET /collections/{id}/documents")
		return
	}
	id := r.PathValue("id")
	includeDeleted, ok := s.queryBool(w, r, "include_deleted")
	if !ok {
		return
	}
	documents, err := s.opts.KnowledgeCatalog.ListDocuments(r.Context(), principal, id, includeDeleted)
	if err != nil {
		s.writeKnowledgeError(w, err, "列文档失败", fmt.Sprintf(messageCollectionNotFound, id))
		return
	}
	s.writeOK(w, documentSummaryViews(documents))
}

// handleIngestDocument 处理 `POST /collections/{id}/documents`：`knowledge:write`，
// 计入 `IngressIngest`。响应 200 表示**已被处理**，`status` 是 `active` / `failed`
// 两种终态之一——失败不是 HTTP 错误，调用方按 `error_kind` 决定是否重试。
// handleIngestDocument returns 200 for both terminal states; failure is not an HTTP error.
func (s *Server) handleIngestDocument(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.KnowledgeIngest == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeIngest", "POST /collections/{id}/documents")
		return
	}
	var body documentIngestWrite
	if !s.decodeBody(w, r, &body) {
		return
	}
	if body.Path == nil || strings.TrimSpace(*body.Path) == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageDocumentPathEmpty)
		return
	}
	collectionID := r.PathValue("id")
	result, err := s.opts.KnowledgeIngest.Ingest(r.Context(), knowledge.IngestRequest{
		Principal:    principal,
		CollectionID: collectionID,
		Path:         *body.Path,
	})
	if err != nil {
		s.writeKnowledgeError(w, err, "摄取文档失败",
			fmt.Sprintf(messageCollectionNotFound, collectionID))
		return
	}
	s.writeOK(w, ingestResultView(result))
}

// handleDeleteDocument 处理 `DELETE /documents/{id}`：`knowledge:write`，软删文档。
// 历史引用**不改写**，读面按当前删除状态标注失效。
// handleDeleteDocument soft-deletes the document and never rewrites history.
func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.KnowledgeIngest == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeIngest", "DELETE /documents/{id}")
		return
	}
	id := r.PathValue("id")
	err := s.opts.KnowledgeIngest.Delete(r.Context(), knowledge.DeleteRequest{
		Principal:  principal,
		DocumentID: id,
	})
	if err != nil {
		s.writeKnowledgeError(w, err, "软删文档失败", fmt.Sprintf(messageDocumentNotFound, id))
		return
	}
	s.writeOK(w, nil)
}

// handleListDocumentVersions 处理 `GET /documents/{id}/versions`：`knowledge:read`，
// 固定 `created_at` 倒序（排序在存储 SQL 里）。
// handleListDocumentVersions lists versions newest-first.
func (s *Server) handleListDocumentVersions(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.KnowledgeCatalog == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeCatalog", "GET /documents/{id}/versions")
		return
	}
	id := r.PathValue("id")
	versions, err := s.opts.KnowledgeCatalog.ListVersions(r.Context(), principal, id)
	if err != nil {
		s.writeKnowledgeError(w, err, "列版本失败", fmt.Sprintf(messageDocumentNotFound, id))
		return
	}
	s.writeOK(w, documentVersionSummaryViews(versions))
}

// handleReindexDocument 处理 `POST /documents/{id}/reindex`：`knowledge:write`，计入
// `IngressIngest`。按来源路径重新处理并创建独立版本；成功前旧版本继续服务。
// handleReindexDocument creates a new version; the prior version stays active until
// the new one has been indexed and activated.
func (s *Server) handleReindexDocument(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.KnowledgeIngest == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeIngest", "POST /documents/{id}/reindex")
		return
	}
	id := r.PathValue("id")
	result, err := s.opts.KnowledgeIngest.Reindex(r.Context(), knowledge.ReindexRequest{
		Principal:  principal,
		DocumentID: id,
		AuditActor: knowledgeRequestActor(r),
	})
	if err != nil {
		s.writeKnowledgeError(w, err, "重建索引失败", fmt.Sprintf(messageDocumentNotFound, id))
		return
	}
	s.writeOK(w, ingestResultView(result))
}

// handleSearch 处理 `POST /search`：`knowledge:read`，计入 `IngressRead`。请求必须
// 给出 `collections` 或 `agent` 之一——**不做全库检索**（契约明文）。
// handleSearch requires an explicit scope; a whole-library search is not a supported
// request shape.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.KnowledgeSearch == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeSearch", "POST /search")
		return
	}
	var body searchWrite
	if !s.decodeBody(w, r, &body) {
		return
	}
	if body.Query == nil || strings.TrimSpace(*body.Query) == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageSearchQueryEmpty)
		return
	}
	agent := ""
	if body.Agent != nil {
		agent = strings.TrimSpace(*body.Agent)
	}
	if len(body.Collections) == 0 && agent == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageSearchScopeEmpty)
		return
	}
	topK := 0
	if body.TopK != nil {
		if *body.TopK < 1 || *body.TopK > maxSearchTopK {
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageSearchTopKRange)
			return
		}
		topK = *body.TopK
	}
	budget := 0
	if body.BudgetTokens != nil {
		if *body.BudgetTokens < 1 {
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageSearchBudgetRange)
			return
		}
		budget = *body.BudgetTokens
	}
	result, err := s.opts.KnowledgeSearch.Search(r.Context(), KnowledgeSearchQuery{
		Principal:   principal,
		Query:       *body.Query,
		Collections: body.Collections,
		Agent:       agent,
		TopK:        topK,
		Budget:      budget,
	})
	if err != nil {
		s.writeKnowledgeSearchError(w, err, "检索失败")
		return
	}
	s.writeOK(w, searchResultView(result))
}

// handleSourceView 处理 `POST /sources/view`：`knowledge:read`，计入 `IngressRead`。
// **不走相似度**；每次按当前 Principal 重查 ACL / 删除 / 版本。契约该端点没有 404：
// "版本不存在"是 200 + `validity.reason=not_found` 的正常结论，因此在这里出现的
// `ErrNotFound` 只能是端口违约——按装配缺陷记 ERROR 回 500，**不得**投影成 404。
// handleSourceView treats a not-found sentinel as a port violation, because the
// contract expresses a missing version as 200 with validity.reason=not_found.
func (s *Server) handleSourceView(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.KnowledgeSource == nil {
		s.writeKnowledgePortMissing(w, "KnowledgeSource", "POST /sources/view")
		return
	}
	var body sourceViewWrite
	if !s.decodeBody(w, r, &body) {
		return
	}
	if body.Ref == nil || body.Ref.ID == "" || body.Ref.VersionID == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageSourceViewRefMissing)
		return
	}
	req := knowledge.SourceViewRequest{
		Ref: knowledge.EvidenceRef{
			ID:          body.Ref.ID,
			VersionID:   body.Ref.VersionID,
			ContentHash: body.Ref.ContentHash,
		},
	}
	if body.Range != nil {
		req.Range = &knowledge.EvidenceRange{
			Start: intOrZero(body.Range.Start),
			End:   intOrZero(body.Range.End),
		}
	}
	if body.BudgetTokens != nil {
		if *body.BudgetTokens < 1 {
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageSourceViewBudget)
			return
		}
		req.Budget = *body.BudgetTokens
	}
	if body.Cursor != nil {
		req.Cursor = *body.Cursor
	}
	req.IncludeFacts = boolOrFalse(body.IncludeFacts)
	req.IncludeSummary = boolOrFalse(body.IncludeSummary)

	result, err := s.opts.KnowledgeSource.SourceView(r.Context(), principal, req)
	if err != nil {
		if errors.Is(err, knowledge.ErrNotFound) {
			s.logger().Error("来源视图端口把缺失版本当成错误（端口违约）", "error", err)
			s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
			return
		}
		s.writeKnowledgeError(w, err, "来源视图读取失败", "")
		return
	}
	s.writeOK(w, sourceViewResultView(result))
}

// writeKnowledgePortMissing 是知识端点缺端口的唯一出口：装配错误记 ERROR、回固定
// 500；**不得**用空列表冒充"没有数据"（与 US-11 只读面同口径）。
// A missing knowledge port is an assembly error: ERROR log plus a fixed 500.
func (s *Server) writeKnowledgePortMissing(w http.ResponseWriter, port, endpoint string) {
	s.logger().Error("装配缺少知识端口", "port", port, "endpoint", endpoint)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

// writeKnowledgeError 把知识面失败按哨兵映射为固定响应（只认 `errors.Is`，**不**比对
// 文本）：未授权 401、越权 403、路径 / 请求形状 400、不存在 404、同名冲突 409、
// 三个调用方 429、装配不可用 503，其余 500（底层原因只进日志）。
// 404 需要调用点给出点名文案；没有说明调用点漏传（编程错误），按装配缺陷回 500，
// **不**把 404 与"服务器内部错误"拼成一条自相矛盾的响应。
// writeKnowledgeError maps domain sentinels to fixed responses; a 404 without a
// call-site message is a programming error answered as 500.
func (s *Server) writeKnowledgeError(w http.ResponseWriter, err error, action, missing string) {
	switch {
	case errors.Is(err, context.Canceled):
		// 客户端已放弃：不再写响应（写已断开的连接只会得到 broken pipe）。
		s.logger().Warn("知识请求已取消", "action", action)
	case errors.Is(err, core.ErrUnauthorized):
		s.writeError(w, http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
	case errors.Is(err, knowledge.ErrForbidden):
		s.writeError(w, http.StatusForbidden, codeForbidden, messageKnowledgeForbidden)
	case errors.Is(err, knowledge.ErrPathRejected):
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messagePathRejected)
	case errors.Is(err, knowledge.ErrInvalidRequest):
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageKnowledgeInvalid)
	case errors.Is(err, knowledge.ErrNotFound):
		if missing == "" {
			s.logger().Error("知识目标未找到但缺少点名文案", "action", action, "error", err)
			s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
			return
		}
		s.writeError(w, http.StatusNotFound, codeNotFound, missing)
	case errors.Is(err, core.ErrConflict):
		s.writeError(w, http.StatusConflict, codeConflict, messageCollectionConflict)
	case errors.Is(err, callers.ErrRateLimited):
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(ingressRetryAfter), 10))
		s.writeError(w, http.StatusTooManyRequests, codeCallerRateLimited,
			errorCodeMessage(codeCallerRateLimited, ""))
	case errors.Is(err, callers.ErrConcurrencyLimited):
		s.writeError(w, http.StatusTooManyRequests, codeCallerConcurrencyLimited,
			errorCodeMessage(codeCallerConcurrencyLimited, ""))
	case errors.Is(err, callers.ErrQuotaExhausted):
		s.writeQuotaExhausted(w, err)
	case errors.Is(err, knowledge.ErrIngestUnavailable):
		s.writeError(w, http.StatusServiceUnavailable, codeNotConfigured, messageIngestUnavailable)
	default:
		s.logger().Error("知识操作失败", "action", action, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	}
}

// writeKnowledgeSearchError 是 `/search` 的专用出口：契约该端点只列 400 / 401 / 403 /
// 429 / 503，**没有 500**——所以任何无法归类的检索失败都按"检索不可用"回 503
// `NOT_CONFIGURED` 并带 `data.reason_code`，同时记 ERROR 日志。降级成 200 + 空命中
// 会让调用方以为"查过了没有"，那是比报错更危险的错。
// writeKnowledgeSearchError never answers 500: every unclassifiable search failure is
// a 503 with a reason code, because an empty 200 would read as "searched, no hits".
func (s *Server) writeKnowledgeSearchError(w http.ResponseWriter, err error, action string) {
	switch {
	case errors.Is(err, context.Canceled):
		s.logger().Warn("知识检索已取消", "action", action)
	case errors.Is(err, core.ErrUnauthorized):
		s.writeError(w, http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
	case errors.Is(err, knowledge.ErrForbidden):
		s.writeError(w, http.StatusForbidden, codeForbidden, messageKnowledgeForbidden)
	case errors.Is(err, knowledge.ErrInvalidRequest):
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageKnowledgeInvalid)
	case errors.Is(err, callers.ErrRateLimited):
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(ingressRetryAfter), 10))
		s.writeError(w, http.StatusTooManyRequests, codeCallerRateLimited,
			errorCodeMessage(codeCallerRateLimited, ""))
	case errors.Is(err, callers.ErrConcurrencyLimited):
		s.writeError(w, http.StatusTooManyRequests, codeCallerConcurrencyLimited,
			errorCodeMessage(codeCallerConcurrencyLimited, ""))
	case errors.Is(err, callers.ErrQuotaExhausted):
		s.writeQuotaExhausted(w, err)
	case errors.Is(err, knowledge.ErrRetrieverUnavailable):
		s.logger().Error("知识检索不可用", "action", action, "error", err)
		s.writeSearchUnavailable(w)
	default:
		s.logger().Error("知识检索失败", "action", action, "error", err)
		s.writeSearchUnavailable(w)
	}
}

// writeSearchUnavailable 组装 503 `NOT_CONFIGURED` + `data.reason_code`：`data` 只经
// `errorDataView` 的白名单出站（`writeAdminDataError`），因此原因码是**结构字段**
// 而不是拼进 message 的自由文本——拼进文案调用方就没法机判了。原始错误由调用点
// 先记 ERROR 日志，**不**参与文案组装（底层原因不进响应）。
// writeSearchUnavailable carries the reason as a structured field, never as free text;
// the raw error is logged at the call site and never enters the response.
func (s *Server) writeSearchUnavailable(w http.ResponseWriter) {
	wrapped := &core.AdminError{
		Kind:    knowledge.ErrRetrieverUnavailable,
		Message: messageRetrieverUnavailable,
		Data:    map[string]any{"reason_code": reasonVectorUnavailable},
	}
	s.writeAdminDataError(w, http.StatusServiceUnavailable, codeNotConfigured,
		messageRetrieverUnavailable, wrapped)
}

// queryBool 解析布尔查询参数：缺省与 `true` / `false` 之外的值按 400 拒绝，**不**把
// 无法识别的值静默当成 false（那会让"要不要看已删数据"变成猜）。写响应返回 false。
// queryBool rejects anything except an absent value, true or false.
func (s *Server) queryBool(w http.ResponseWriter, r *http.Request, name string) (bool, bool) {
	switch r.URL.Query().Get(name) {
	case "":
		return false, true
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageQueryBoolBad)
		return false, false
	}
}

// normalizeCollectionName 校验并归一集合名：去首尾空白后必须非空，且不得含控制字符
// （换行会被当成日志与页面的分隔符）。设计**没有**冻结字符集与长度上限，因此本层
// **不**发明白名单式限制——只拒绝两种可证明有害的形态。返回的 reason 非空即拒绝文案。
// The design freezes no charset or length limit, so this layer rejects only the two
// provably harmful shapes: an empty name and embedded control characters.
func normalizeCollectionName(raw string) (string, string) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", messageCollectionNameEmpty
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", messageCollectionNameBad
		}
	}
	return name, ""
}

// intOrZero 把可缺省的整数指针取成值；缺省即 0（契约对 `start` / `end` 没有必填）。
// intOrZero defaults an absent offset to zero, as the contract allows.
func intOrZero(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// boolOrFalse 把可缺省的布尔指针取成值；缺省即 false（契约默认值）。
// boolOrFalse applies the contract's default of false.
func boolOrFalse(v *bool) bool { return v != nil && *v }

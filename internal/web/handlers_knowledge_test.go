package web

// 本文件覆盖知识库十个端点（US-16 / T042）的 HTTP 层，判据取自冻结契约
// `docs/contracts/openapi.yaml` 的知识组与设计卡 §3.1.3 的投影裁决：
//
//  1. **形状 400 在调用端口之前**发生（零写入、零检索）；
//  2. `/search` 不可用是 **503 + `data.reason_code`**——契约该端点没有 500，也
//     **不得**用空命中冒充"查过了"；
//  3. `/sources/view` 失效（`validity.valid=false`，含被取代）时正文、内容哈希、
//     事实、摘要、窗口、游标一律清空，`Superseded` **不投影**——这是 R50.2 口头
//     裁决落成的可执行判据，也就是 R50.6 点名的留口；
//  4. `GET /collections` 是**管理面**：有效 caller key 打过来是 403，不是 200；
//  5. 缺端口是装配错误，十个端点一律固定 500，**不得**用空列表冒充空状态。
//
// 逆改点写在每个用例头上；投影类用例一律查**原始 JSON 键**而不只查结构体——结构体
// 零值会让"字段没出站"与"字段出了空值"看起来一样，而契约区分这两者。
// This file pins the ten knowledge endpoints: shape checks before the port, search
// unavailability as 503 with a structured reason code, an invalid source view that
// serves no content, the management-surface list, and the fail-closed 500 on missing
// ports. Projection tests inspect raw JSON keys because a zero-valued struct cannot
// tell "absent" from "present but empty".

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// fakeKnowledgeCatalog 记录目录端口调用并可注入错误；`got*` 字段钉住透传值。
// fakeKnowledgeCatalog records catalog calls and can inject an error.
type fakeKnowledgeCatalog struct {
	collections []KnowledgeCollection
	documents   []KnowledgeDocument
	versions    []KnowledgeDocumentVersion
	created     KnowledgeCollection
	err         error

	listCalls, createCalls, documentCalls, versionCalls int
	gotProject, gotName, gotCollectionID, gotDocumentID string
	gotDeleted, gotDocDeleted                           bool
	gotPrincipal                                        core.Principal
}

// ListCollections 记录项目与已删开关后返回清单。
// ListCollections records the filters, then returns the configured list.
func (f *fakeKnowledgeCatalog) ListCollections(
	_ context.Context, project string, includeDeleted bool,
) ([]KnowledgeCollection, error) {
	f.listCalls++
	f.gotProject, f.gotDeleted = project, includeDeleted
	if f.err != nil {
		return nil, f.err
	}
	return f.collections, nil
}

// CreateCollection 记录归一后的名字与发起身份（端口不做校验，断言归用例）。
// CreateCollection records the normalized name and the caller identity.
func (f *fakeKnowledgeCatalog) CreateCollection(
	_ context.Context, p core.Principal, name, project string,
) (KnowledgeCollection, error) {
	f.createCalls++
	f.gotName, f.gotProject, f.gotPrincipal = name, project, p
	if f.err != nil {
		return KnowledgeCollection{}, f.err
	}
	created := f.created
	created.Name, created.Project = name, project
	return created, nil
}

// ListDocuments 记录集合与已删开关后返回清单。
// ListDocuments records the filters, then returns the configured list.
func (f *fakeKnowledgeCatalog) ListDocuments(
	_ context.Context, _ core.Principal, collectionID string, includeDeleted bool,
) ([]KnowledgeDocument, error) {
	f.documentCalls++
	f.gotCollectionID, f.gotDocDeleted = collectionID, includeDeleted
	if f.err != nil {
		return nil, f.err
	}
	return f.documents, nil
}

// ListVersions 记录文档标识后返回配置的版本清单。
// ListVersions records the document id, then returns the configured versions.
func (f *fakeKnowledgeCatalog) ListVersions(
	_ context.Context, _ core.Principal, documentID string,
) ([]KnowledgeDocumentVersion, error) {
	f.versionCalls++
	f.gotDocumentID = documentID
	if f.err != nil {
		return nil, f.err
	}
	return f.versions, nil
}

// fakeKnowledgeIngest 记录摄取 / 重建 / 删除三类调用并可注入错误。
// fakeKnowledgeIngest records ingest, reindex and delete calls.
type fakeKnowledgeIngest struct {
	ingestResult  knowledge.IngestResult
	reindexResult knowledge.IngestResult
	err           error

	ingestCalls, reindexCalls, deleteCalls int
	ingestReq                              knowledge.IngestRequest
	reindexReq                             knowledge.ReindexRequest
	deletes                                []knowledge.DeleteRequest
}

// Ingest 记录请求后返回配置的结果或错误。
// Ingest records the request and returns the configured result or error.
func (f *fakeKnowledgeIngest) Ingest(
	_ context.Context, req knowledge.IngestRequest,
) (knowledge.IngestResult, error) {
	f.ingestCalls++
	f.ingestReq = req
	if f.err != nil {
		return knowledge.IngestResult{}, f.err
	}
	return f.ingestResult, nil
}

// Delete 记录删除目标后返回配置的错误（成功即 nil）。
// Delete records the target and returns the configured error.
func (f *fakeKnowledgeIngest) Delete(_ context.Context, req knowledge.DeleteRequest) error {
	f.deleteCalls++
	f.deletes = append(f.deletes, req)
	return f.err
}

// Reindex 记录重建请求后返回配置的结果或错误。
// Reindex records the request and returns the configured result or error.
func (f *fakeKnowledgeIngest) Reindex(
	_ context.Context, req knowledge.ReindexRequest,
) (knowledge.IngestResult, error) {
	f.reindexCalls++
	f.reindexReq = req
	if f.err != nil {
		return knowledge.IngestResult{}, f.err
	}
	return f.reindexResult, nil
}

// fakeKnowledgeSearch 记录检索请求并可注入错误。
// fakeKnowledgeSearch records the search query and can inject an error.
type fakeKnowledgeSearch struct {
	result knowledge.SearchResult
	err    error

	calls int
	query KnowledgeSearchQuery
}

// Search 记录请求后返回配置的结果或错误。
// Search records the query and returns the configured result or error.
func (f *fakeKnowledgeSearch) Search(
	_ context.Context, q KnowledgeSearchQuery,
) (knowledge.SearchResult, error) {
	f.calls++
	f.query = q
	if f.err != nil {
		return knowledge.SearchResult{}, f.err
	}
	return f.result, nil
}

// fakeKnowledgeSource 记录来源视图请求与身份并可注入错误。
// fakeKnowledgeSource records the source-view request and identity.
type fakeKnowledgeSource struct {
	result knowledge.SourceViewResult
	err    error

	calls     int
	req       knowledge.SourceViewRequest
	principal core.Principal
}

// SourceView 记录请求后返回配置的结果或错误。
// SourceView records the request and returns the configured result or error.
func (f *fakeKnowledgeSource) SourceView(
	_ context.Context, p core.Principal, req knowledge.SourceViewRequest,
) (knowledge.SourceViewResult, error) {
	f.calls++
	f.req, f.principal = req, p
	if f.err != nil {
		return knowledge.SourceViewResult{}, f.err
	}
	return f.result, nil
}

// knowledgeFakes 汇总四个知识端口假件，便于用例按名取用。
// knowledgeFakes bundles the four knowledge port fakes.
type knowledgeFakes struct {
	catalog *fakeKnowledgeCatalog
	ingest  *fakeKnowledgeIngest
	search  *fakeKnowledgeSearch
	source  *fakeKnowledgeSource
}

// newKnowledgeServer 装配接满四个知识端口的服务器；mutate 在端口之后应用，
// 便于用例换成接调用方 Key 的真实认证闸门。
// newKnowledgeServer wires all four knowledge ports; mutate runs afterwards so a test
// can swap in the real guard with caller keys.
func newKnowledgeServer(t *testing.T, mutate func(*Options)) (*Server, *knowledgeFakes) {
	t.Helper()
	fakes := &knowledgeFakes{
		catalog: &fakeKnowledgeCatalog{},
		ingest:  &fakeKnowledgeIngest{},
		search:  &fakeKnowledgeSearch{},
		source:  &fakeKnowledgeSource{},
	}
	srv := newTestServer(t, func(o *Options) {
		o.KnowledgeCatalog = fakes.catalog
		o.KnowledgeIngest = fakes.ingest
		o.KnowledgeSearch = fakes.search
		o.KnowledgeSource = fakes.source
		if mutate != nil {
			mutate(o)
		}
	})
	return srv, fakes
}

// knowledgeEnvelope 是带具体 data 形状的固定信封。
// knowledgeEnvelope is the fixed envelope with a concrete data shape.
type knowledgeEnvelope[T any] struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// decodeKnowledge 解析成功信封。
// decodeKnowledge decodes a success envelope.
func decodeKnowledge[T any](t *testing.T, rec *httptest.ResponseRecorder) knowledgeEnvelope[T] {
	t.Helper()
	var env knowledgeEnvelope[T]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env
}

// knowledgeDataFields 取 `data` 的原始键：用于"字段必须缺席"这类结构体看不出的断言。
// knowledgeDataFields keeps the raw data keys so absence can be asserted.
func knowledgeDataFields(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.NotNil(t, env.Data, "成功响应必须带 data")
	return env.Data
}

// requireKnowledgeError 断言失败信封：状态、码、固定文案，且**不带 data**。
// requireKnowledgeError checks a failure envelope: status, code, fixed message, no data.
func requireKnowledgeError(t *testing.T, rec *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	require.Equal(t, status, rec.Code)
	env := decodeErrorEnvelope(t, rec)
	require.Equal(t, code, env.Code)
	require.Equal(t, message, env.Message)
	require.Nil(t, env.Data, "失败信封不得带 data")
}

// TestKnowledge_CreateCollectionValidatesBeforeWriting 判建集合的三条 400 都在调用
// 端口之前发生（零写入），成功路径把归一后的名字与项目透传给端口。
// 逆改点：把校验挪到 CreateCollection 之后 → createCalls 非 0（"零写入"断言红）；
// 不去首尾空白 → gotName 断言红。
func TestKnowledge_CreateCollectionValidatesBeforeWriting(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	post := func(body string) *httptest.ResponseRecorder {
		return serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/collections", body))
	}

	requireKnowledgeError(t, post(`{}`), http.StatusBadRequest, codeBadRequest, messageCollectionCreateBody)
	requireKnowledgeError(t, post(`{"name":"   "}`), http.StatusBadRequest, codeBadRequest, messageCollectionNameEmpty)
	requireKnowledgeError(t, post(`{"name":"a\u0000b"}`), http.StatusBadRequest, codeBadRequest, messageCollectionNameBad)
	require.Zero(t, fakes.catalog.createCalls, "校验失败不得触达端口（零写入）")

	fakes.catalog.created = KnowledgeCollection{
		CollectionID:  "col-1",
		DocumentCount: -1,
		CreatedAt:     time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC),
	}
	rec := post(`{"name":"  ops docs  ","project":"team-a"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, fakes.catalog.createCalls)
	require.Equal(t, "ops docs", fakes.catalog.gotName, "名字去首尾空白后交给端口")
	require.Equal(t, "team-a", fakes.catalog.gotProject)

	env := decodeKnowledge[CollectionSummaryView](t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Equal(t, "col-1", env.Data.CollectionID)
	require.Equal(t, "2026-10-27T08:00:00Z", env.Data.CreatedAt)
	_, hasCount := knowledgeDataFields(t, rec)["document_count"]
	require.False(t, hasCount, "负计数表示未统计，字段必须缺省")

	fakes.catalog.err = core.ErrConflict
	requireKnowledgeError(t, post(`{"name":"ops docs"}`),
		http.StatusConflict, codeConflict, messageCollectionConflict)
}

// TestKnowledge_ListCollectionsIsManagementFace 判 `GET /collections` 的管理面归属：
// 有效 caller key 是 403（凭据拿错了面，不是凭据无效），admin token 才能列；
// 查询参数原样透传，`document_count` 的 0 与"未统计"必须区分。
// 逆改点：把该行改成业务面 → caller key 拿到 200（首个断言红）；把 0 当成未统计
// 省略 → Data[0].DocumentCount 为 nil（断言红）。
func TestKnowledge_ListCollectionsIsManagementFace(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"kc-secret": callerPrincipal("kc1", core.ScopeKnowledgeRead, core.ScopeKnowledgeWrite),
	}}
	srv, fakes := newKnowledgeServer(t, func(o *Options) { o.AdminAuth = businessGuard(t, keys) })
	fakes.catalog.collections = []KnowledgeCollection{
		{
			CollectionID: "col-1", Name: "ops", Project: "team-a", DocumentCount: 0,
			CreatedAt: time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC),
		},
		{CollectionID: "col-2", Name: "empty", Project: "team-a", DocumentCount: -1},
	}

	caller := withBearer(adminRequest(http.MethodGet, "/api/v1/collections", ""), "kc-secret")
	rec := serveAdmin(srv, caller)
	require.Equal(t, http.StatusForbidden, rec.Code, "有效 caller key 打管理面必须 403")
	require.Equal(t, codeForbidden, decodeErrorEnvelope(t, rec).Code)
	require.Zero(t, fakes.catalog.listCalls, "拒绝发生在端口之前")

	admin := withBearer(adminRequest(http.MethodGet,
		"/api/v1/collections?project=team-a&include_deleted=true", ""), testToken)
	rec = serveAdmin(srv, admin)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "team-a", fakes.catalog.gotProject)
	require.True(t, fakes.catalog.gotDeleted)

	env := decodeKnowledge[[]CollectionSummaryView](t, rec)
	require.Len(t, env.Data, 2)
	require.NotNil(t, env.Data[0].DocumentCount, "0 是真实答案，必须出站")
	require.Equal(t, 0, *env.Data[0].DocumentCount)
	require.Nil(t, env.Data[1].DocumentCount, "未统计省略字段")

	bad := withBearer(adminRequest(http.MethodGet,
		"/api/v1/collections?include_deleted=maybe", ""), testToken)
	requireKnowledgeError(t, serveAdmin(srv, bad),
		http.StatusBadRequest, codeBadRequest, messageQueryBoolBad)

	// 写面接受 caller key，并把调用方身份原样交给端口（不换成管理员）。
	fakes.catalog.created = KnowledgeCollection{CollectionID: "col-9"}
	rec = serveAdmin(srv, withBearer(adminRequest(http.MethodPost, "/api/v1/collections",
		`{"name":"from-caller"}`), "kc-secret"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "kc1", fakes.catalog.gotPrincipal.CallerID)
}

// TestKnowledge_DocumentAndVersionListsProject 判两条只读清单端点的透传与投影：
// 集合与 `include_deleted` 开关原样交给端口，文档行带 `content_hash`（引用依据
// 必须能回溯到版本哈希），空清单是 `[]` 而不是 `null`，未激活版本的
// `activated_at` 是空串但**字段仍在**（契约必填），非失败版本不输出 `error_kind`。
// 逆改点：漏传过滤参数 → got* 断言红；空切片用 nil → `[]` 断言红；给
// activated_at 加 omitempty → 原始键断言红；把 error_kind 总是输出 → 缺席断言红。
func TestKnowledge_DocumentAndVersionListsProject(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	fakes.catalog.documents = []KnowledgeDocument{{
		DocumentID: "doc-1", CollectionID: "col-1", SourcePath: "docs/a.md",
		ContentHash: "hash-1", LatestVersionID: "ver-2",
		LatestStatus: string(knowledge.VersionActive),
		CreatedAt:    time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
	}}

	rec := serveAdmin(srv, adminRequest(http.MethodGet,
		"/api/v1/collections/col-1/documents?include_deleted=true", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "col-1", fakes.catalog.gotCollectionID)
	require.True(t, fakes.catalog.gotDocDeleted)

	docs := decodeKnowledge[[]DocumentSummaryView](t, rec)
	require.Len(t, docs.Data, 1)
	require.Equal(t, "hash-1", docs.Data[0].ContentHash)
	require.Equal(t, string(knowledge.VersionActive), docs.Data[0].LatestStatus)
	require.Equal(t, "2026-10-27T08:00:00Z", docs.Data[0].CreatedAt)

	// 空清单必须渲染成 `[]`：nil 切片会序列化成 null，前端分页就会断。
	fakes.catalog.documents = nil
	rec = serveAdmin(srv, adminRequest(http.MethodGet,
		"/api/v1/collections/col-1/documents", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, fakes.catalog.gotDocDeleted, "缺省 include_deleted 是 false")
	var emptyDocs struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &emptyDocs))
	require.NotNil(t, emptyDocs.Data, "空清单输出 []，不是 null")
	require.Empty(t, emptyDocs.Data)

	fakes.catalog.versions = []KnowledgeDocumentVersion{{
		VersionID: "ver-2", DocumentID: "doc-1", ContentHash: "hash-2",
		Status: string(knowledge.VersionActive), ParserVersion: "text-v1",
		Generation:  3,
		CreatedAt:   time.Date(2026, 10, 27, 8, 30, 0, 0, time.UTC),
		ActivatedAt: time.Date(2026, 10, 27, 8, 31, 0, 0, time.UTC),
	}, {
		VersionID: "ver-1", DocumentID: "doc-1", ContentHash: "hash-1",
		Status: string(knowledge.VersionSuperseded),
	}}
	rec = serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/documents/doc-1/versions", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "doc-1", fakes.catalog.gotDocumentID)

	versions := decodeKnowledge[[]DocumentVersionSummaryView](t, rec)
	require.Len(t, versions.Data, 2)
	require.Equal(t, "2026-10-27T08:31:00Z", versions.Data[0].ActivatedAt)
	require.Equal(t, "text-v1", versions.Data[0].ParserVersion)
	require.Empty(t, versions.Data[1].ActivatedAt, "未激活版本渲染空串")

	var rawVersions struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rawVersions))
	require.Len(t, rawVersions.Data, 2)
	_, hasActivated := rawVersions.Data[1]["activated_at"]
	require.True(t, hasActivated, "activated_at 是必填字段：未激活输出空串，不得缺席")
	_, hasKind := rawVersions.Data[1]["error_kind"]
	require.False(t, hasKind, "非失败版本不输出 error_kind")
	_, hasParser := rawVersions.Data[1]["parser_version"]
	require.False(t, hasParser, "空版本号省略")
}

// TestKnowledge_SearchValidatesRequestShape 判检索的六条 400：query 必填且非空白、
// `collections` 与 `agent` 至少给一个（不做全库检索）、`top_k` 在 1~50、
// `budget_tokens` 大于 0；全部在调用检索端口之前。
// 逆改点：允许无范围检索 → scope 断言红；top_k 上界放宽到无限制 → 51 用例红。
func TestKnowledge_SearchValidatesRequestShape(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	post := func(body string) *httptest.ResponseRecorder {
		return serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/search", body))
	}

	requireKnowledgeError(t, post(`{"collections":["c1"]}`),
		http.StatusBadRequest, codeBadRequest, messageSearchQueryEmpty)
	requireKnowledgeError(t, post(`{"query":"  ","collections":["c1"]}`),
		http.StatusBadRequest, codeBadRequest, messageSearchQueryEmpty)
	requireKnowledgeError(t, post(`{"query":"q"}`),
		http.StatusBadRequest, codeBadRequest, messageSearchScopeEmpty)
	requireKnowledgeError(t, post(`{"query":"q","collections":["c1"],"top_k":0}`),
		http.StatusBadRequest, codeBadRequest, messageSearchTopKRange)
	requireKnowledgeError(t, post(`{"query":"q","collections":["c1"],"top_k":51}`),
		http.StatusBadRequest, codeBadRequest, messageSearchTopKRange)
	requireKnowledgeError(t, post(`{"query":"q","collections":["c1"],"budget_tokens":0}`),
		http.StatusBadRequest, codeBadRequest, messageSearchBudgetRange)
	require.Zero(t, fakes.search.calls, "形状校验失败不得触达检索端口")

	rec := post(`{"query":"q","collections":["c1"],"agent":" ops-agent ",
		"top_k":5,"budget_tokens":100}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, fakes.search.calls)
	require.Equal(t, "ops-agent", fakes.search.query.Agent)
	require.Equal(t, 5, fakes.search.query.TopK)
	require.Equal(t, 100, fakes.search.query.Budget)
	require.Equal(t, []string{"c1"}, fakes.search.query.Collections)
	require.Equal(t, core.PrincipalLocalAdmin, fakes.search.query.Principal.Kind)
}

// TestKnowledge_SearchUnavailableIs503WithReasonCode 判两路皆失败的线上形态：
// 503 `NOT_CONFIGURED` + `data.reason_code`（结构字段，不是拼进文案的自由文本），
// 且底层原因只进日志。可分类哨兵与不可分类错误走同一出口——契约该端点没有 500。
// 逆改点：default 分支回 500 → 状态断言红；把原因码拼进 message → data 断言红。
func TestKnowledge_SearchUnavailableIs503WithReasonCode(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	body := `{"query":"q","collections":["c1"]}`

	for _, injected := range []error{
		errors.New("embedding 提供方超时"),
		knowledge.ErrRetrieverUnavailable,
	} {
		fakes.search.err = injected
		rec := serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/search", body))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code,
			"检索不可用必须是 503，不是 500")
		env := decodeErrorEnvelope(t, rec)
		require.Equal(t, codeNotConfigured, env.Code)
		require.Equal(t, messageRetrieverUnavailable, env.Message)
		require.Equal(t, reasonVectorUnavailable, env.Data["reason_code"],
			"原因码必须是结构字段，调用方才能机判")
		require.NotContains(t, rec.Body.String(), "超时", "底层原因只进日志")
	}
}

// TestKnowledge_SearchHitsNeverCarryContentHash 判命中投影：`SearchHit` **不带**
// `content_hash`（命中不能当引用依据），`snippet` 取片段正文，空命中输出 `[]`
// 而不是 `null`，无降级时不输出 `degraded_reason`。
// 逆改点：把 Hit.ContentHash 投影出去 → 原始键断言红；空切片用 nil → `null` 断言红。
func TestKnowledge_SearchHitsNeverCarryContentHash(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	fakes.search.result = knowledge.SearchResult{
		Hits: []knowledge.Hit{{
			ChunkID: "chunk-1", DocumentID: "doc-1", VersionID: "ver-1",
			CollectionID: "col-1", SourcePath: "docs/a.md", HeadingPath: "A > B",
			ContentHash: "sha256-should-not-leak", Text: "片段正文",
			Rank: 1, Score: 0.75, TokenEstimate: 12,
		}},
		DeniedCount: 2, Strategy: knowledge.StrategyHybrid, Generation: 7,
	}
	rec := serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/search",
		`{"query":"q","collections":["col-1"]}`))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeKnowledge[SearchResultView](t, rec)
	require.Len(t, env.Data.Hits, 1)
	require.Equal(t, "片段正文", env.Data.Hits[0].Snippet)
	require.Equal(t, "hybrid", env.Data.Strategy)
	require.Equal(t, 2, env.Data.DeniedCount)
	require.Equal(t, 7, env.Data.Generation)

	var raw struct {
		Data struct {
			Hits []map[string]json.RawMessage `json:"hits"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw.Data.Hits, 1)
	_, hasHash := raw.Data.Hits[0]["content_hash"]
	require.False(t, hasHash, "SearchHit 不得携带 content_hash")
	require.Contains(t, raw.Data.Hits[0], "snippet")

	fakes.search.result = knowledge.SearchResult{Strategy: knowledge.StrategyKeyword}
	rec = serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/search",
		`{"query":"q","collections":["col-1"]}`))
	fields := knowledgeDataFields(t, rec)
	require.JSONEq(t, `[]`, string(fields["hits"]), "无命中输出 []，不是 null")
	_, hasDegraded := fields["degraded_reason"]
	require.False(t, hasDegraded, "无降级时不输出该字段")
}

// TestKnowledge_SourceViewProjectsValidContent 判有效视图的完整投影与请求透传：
// 范围 / 预算 / 游标 / 两个 include 开关都原样交给端口，正文、哈希、事实、摘要、
// 父引用与截断标志全部出站。
// 逆改点：漏传 include 开关 → 端口断言红；摘要按零值出站 → summary 缺失（断言红）。
func TestKnowledge_SourceViewProjectsValidContent(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	fakes.source.result = knowledge.SourceViewResult{
		Ref:       knowledge.EvidenceRef{ID: "doc-1", VersionID: "ver-1", ContentHash: "hash-1"},
		Validity:  knowledge.ValidityResult{Valid: true, VersionID: "ver-1"},
		VersionID: "ver-1", ContentHash: "hash-1", Text: "原文窗口",
		Range: knowledge.EvidenceRange{Start: 10, End: 14}, Truncated: true,
		NextCursor: "cur-1", Conversion: "char_window",
		ParentRefs: []knowledge.EvidenceRef{{ID: "doc-1", VersionID: "ver-1"}},
		Facts:      []knowledge.SourceFact{{Key: "source", Value: "docs/a.md"}},
		Summary: knowledge.SourceSummary{
			DocumentTitle: "标题", HeadingPath: "A > B", SectionSummary: "摘要",
		},
	}
	rec := serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/sources/view",
		`{"ref":{"id":"doc-1","version_id":"ver-1"},"range":{"start":10,"end":14},
		"budget_tokens":50,"cursor":"cur-0","include_facts":true,"include_summary":true}`))
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, fakes.source.req.IncludeFacts)
	require.True(t, fakes.source.req.IncludeSummary)
	require.Equal(t, 50, fakes.source.req.Budget)
	require.Equal(t, "cur-0", fakes.source.req.Cursor)
	require.Equal(t, &knowledge.EvidenceRange{Start: 10, End: 14}, fakes.source.req.Range)
	require.Equal(t, core.PrincipalLocalAdmin, fakes.source.principal.Kind)

	env := decodeKnowledge[SourceViewResultView](t, rec)
	require.True(t, env.Data.Validity.Valid)
	require.Equal(t, "原文窗口", env.Data.Text)
	require.Equal(t, "hash-1", env.Data.ContentHash)
	require.True(t, env.Data.Truncated)
	require.NotNil(t, env.Data.Range)
	require.Equal(t, 10, env.Data.Range.Start)
	require.Equal(t, "cur-1", env.Data.NextCursor)
	require.Equal(t, "char_window", env.Data.Conversion)
	require.Len(t, env.Data.ParentRefs, 1)
	require.Len(t, env.Data.Facts, 1)
	require.NotNil(t, env.Data.Summary)
	require.Equal(t, "标题", env.Data.Summary.DocumentTitle)
}

// TestKnowledge_SourceViewInvalidClearsContent 是 R50.2 / R50.6 的可执行判据：设计卡
// 口径的被取代版本在 Go 层拿到了"当时版本"的正文、事实与摘要（`Superseded=true`），
// 但 HTTP 投影必须清空正文 / 内容哈希 / 事实 / 摘要 / 窗口 / 游标，且**不得**投影出
// 契约没有的 `superseded` 字段；失效引用是 200 的正常结论，不是错误。
// 逆改点：删掉 `sourceViewResultView` 的失效提前返回 → text / facts / summary 断言红；
// 给视图加 Superseded 字段 → 原始键断言红（"未出站"被打破）。
func TestKnowledge_SourceViewInvalidClearsContent(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	fakes.source.result = knowledge.SourceViewResult{
		Ref:       knowledge.EvidenceRef{ID: "doc-1", VersionID: "ver-1", ContentHash: "hash-1"},
		Validity:  knowledge.ValidityResult{Valid: false, Reason: "version_changed", VersionID: "ver-1"},
		VersionID: "ver-1", ContentHash: "must-not-leak", Text: "当时版本的正文",
		Range: knowledge.EvidenceRange{Start: 0, End: 6}, NextCursor: "cur-1",
		Conversion: "verbatim",
		ParentRefs: []knowledge.EvidenceRef{{ID: "doc-1", VersionID: "ver-0"}},
		Facts:      []knowledge.SourceFact{{Key: "k", Value: "v"}},
		Summary:    knowledge.SourceSummary{DocumentTitle: "标题"},
		Superseded: true,
	}
	rec := serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/sources/view",
		`{"ref":{"id":"doc-1","version_id":"ver-1"},"include_facts":true,"include_summary":true}`))
	require.Equal(t, http.StatusOK, rec.Code, "失效引用是 200 + validity.reason 的正常结论")

	fields := knowledgeDataFields(t, rec)
	require.Contains(t, fields, "text")
	require.JSONEq(t, `""`, string(fields["text"]), "失效时 text 必填但为空串")
	for _, absent := range []string{
		"content_hash", "summary", "range", "next_cursor", "conversion", "superseded",
	} {
		_, ok := fields[absent]
		require.False(t, ok, "%s 不得在失效视图里出站", absent)
	}
	require.JSONEq(t, `[]`, string(fields["facts"]), "失效时事实清空为 []")
	require.JSONEq(t, `[]`, string(fields["parent_refs"]), "失效时父引用清空为 []")

	env := decodeKnowledge[SourceViewResultView](t, rec)
	require.False(t, env.Data.Validity.Valid)
	require.Equal(t, "version_changed", env.Data.Validity.Reason)
	require.Equal(t, "doc-1", env.Data.Ref.ID)
	require.Equal(t, "ver-1", env.Data.VersionID, "实际检查的版本是元数据，失效也照出")
}

// TestKnowledge_SourceViewRequestShapeAndPortViolation 判来源视图的两条 400（ref 缺
// id / version_id、预算小于 1）与端口违约：契约该端点**没有 404**，"版本不存在"是
// 200 + `validity.reason=not_found`，所以端口冒出的 `ErrNotFound` 只能是装配缺陷，
// 按 ERROR + 500 处理，**不得**投影成 404。
// 逆改点：把 ErrNotFound 映射成 404 → 状态断言红；缺 version_id 放行 → 400 断言红。
func TestKnowledge_SourceViewRequestShapeAndPortViolation(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	post := func(body string) *httptest.ResponseRecorder {
		return serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/sources/view", body))
	}

	requireKnowledgeError(t, post(`{"ref":{"id":"doc-1"}}`),
		http.StatusBadRequest, codeBadRequest, messageSourceViewRefMissing)
	requireKnowledgeError(t, post(`{}`),
		http.StatusBadRequest, codeBadRequest, messageSourceViewRefMissing)
	requireKnowledgeError(t, post(`{"ref":{"id":"doc-1","version_id":"ver-1"},"budget_tokens":0}`),
		http.StatusBadRequest, codeBadRequest, messageSourceViewBudget)
	require.Zero(t, fakes.source.calls, "形状校验失败不得触达端口")

	fakes.source.err = knowledge.ErrNotFound
	rec := post(`{"ref":{"id":"doc-1","version_id":"ver-1"}}`)
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"端口把缺失版本当成错误是违约，不得回 404")
	env := decodeErrorEnvelope(t, rec)
	require.Equal(t, codeInternal, env.Code)
	require.Equal(t, messageInternal, env.Message)
}

// TestKnowledge_SourceViewAuthBoundary 判 `/sources/view` 的**认证与作用域边界**（C-06 /
// C-08 在来源视图这条业务行上的端到端读数）：无凭据 → 401、只有别的作用域 → 403，两者
// 都**不得触达**来源端口（越权读取的第一步是"根本没读"）；带 `knowledge:read` 的调用方
// → 200，且端口收到的正是该调用方身份。
// 逆改点：把该行从业务面挪到管理面 / 去掉 scope 声明 → 弱 scope 调用方拿到 200（403
// 断言红）；把处理函数挪到认证中间件之前 → 401 / 403 用例的 `calls == 0` 红；身份改传
// 空 Principal → 端口身份断言红。
func TestKnowledge_SourceViewAuthBoundary(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"read-secret":  callerPrincipal("c1", core.ScopeKnowledgeRead),
		"other-secret": callerPrincipal("c2", core.ScopeSessionsRead),
	}}
	srv, fakes := newKnowledgeServer(t, func(o *Options) {
		o.AdminAuth = businessGuard(t, keys)
	})
	fakes.source.result = knowledge.SourceViewResult{VersionID: "ver-1"}
	body := `{"ref":{"id":"doc-1","version_id":"ver-1"}}`

	anon := serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/sources/view", body))
	require.Equal(t, http.StatusUnauthorized, anon.Code, "无凭据必须 401：%s", anon.Body.String())
	require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, anon).Code)
	require.Zero(t, fakes.source.calls, "未认证请求不得触达来源端口")

	weak := serveAdmin(srv, withBearer(adminRequest(http.MethodPost,
		"/api/v1/sources/view", body), "other-secret"))
	require.Equal(t, http.StatusForbidden, weak.Code, "只有 sessions:read 必须 403：%s", weak.Body.String())
	require.Equal(t, codeForbidden, decodeAuthEnvelope(t, weak).Code)
	require.Zero(t, fakes.source.calls, "缺 knowledge:read 时同样不得读取来源")

	ok := serveAdmin(srv, withBearer(adminRequest(http.MethodPost,
		"/api/v1/sources/view", body), "read-secret"))
	require.Equal(t, http.StatusOK, ok.Code, "带 knowledge:read 的调用方必须通过：%s", ok.Body.String())
	require.Equal(t, 1, fakes.source.calls)
	require.Equal(t, "c1", fakes.source.principal.CallerID, "来源端口收到的必须是该调用方身份")
}

// TestKnowledge_IngestReturnsTerminalStatesAs200 判摄取语义：两种终态都回 200，
// failed 带 `error_kind`（失败不是 HTTP 错误，调用方按 error_kind 决定重试）；
// 空路径 400 且零触达。
// 逆改点：把 failed 映射成 5xx → 状态断言红；空路径放行 → 400 断言红。
func TestKnowledge_IngestReturnsTerminalStatesAs200(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	post := func(body string) *httptest.ResponseRecorder {
		return serveAdmin(srv, adminRequest(http.MethodPost,
			"/api/v1/collections/col-1/documents", body))
	}

	requireKnowledgeError(t, post(`{"path":"   "}`),
		http.StatusBadRequest, codeBadRequest, messageDocumentPathEmpty)
	require.Zero(t, fakes.ingest.ingestCalls, "空路径不得触达摄取端口")

	fakes.ingest.ingestResult = knowledge.IngestResult{
		DocumentID: "doc-1", VersionID: "ver-1", IngestJobID: "job-1",
		Status: knowledge.VersionActive, ChunkCount: 3, Generation: 2,
	}
	rec := post(`{"path":"docs/a.md"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "col-1", fakes.ingest.ingestReq.CollectionID)
	require.Equal(t, "docs/a.md", fakes.ingest.ingestReq.Path)
	env := decodeKnowledge[IngestResultView](t, rec)
	require.Equal(t, "active", env.Data.Status)
	require.Equal(t, 3, env.Data.ChunkCount)
	require.Equal(t, 2, env.Data.Generation)
	_, hasErrorKind := knowledgeDataFields(t, rec)["error_kind"]
	require.False(t, hasErrorKind, "active 不输出 error_kind")

	fakes.ingest.ingestResult = knowledge.IngestResult{
		DocumentID: "doc-1", VersionID: "ver-2", IngestJobID: "job-2",
		Status: knowledge.VersionFailed, ErrorKind: knowledge.ErrorKindDecodeFailed,
	}
	rec = post(`{"path":"docs/b.md"}`)
	require.Equal(t, http.StatusOK, rec.Code, "failed 是终态，不是 HTTP 错误")
	env = decodeKnowledge[IngestResultView](t, rec)
	require.Equal(t, "failed", env.Data.Status)
	require.Equal(t, knowledge.ErrorKindDecodeFailed, env.Data.ErrorKind)
}

// TestKnowledge_DeleteIsIdempotent 判两条软删端点：重复删除都回 200（幂等）、成功
// 不带 data，删除目标按路由分别落进 DocumentID / CollectionID（不串）。
// 逆改点：第二次删除回 404 → 幂等断言红；把集合标识写进 DocumentID → 目标断言红。
func TestKnowledge_DeleteIsIdempotent(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)

	for range 2 {
		rec := serveAdmin(srv, adminRequest(http.MethodDelete, "/api/v1/collections/col-1", ""))
		require.Equal(t, http.StatusOK, rec.Code, "重复删除幂等")
		env := decodeErrorEnvelope(t, rec)
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, messageSuccess, env.Message)
		require.Nil(t, env.Data, "成功软删不带 data")
	}
	require.Len(t, fakes.ingest.deletes, 2)
	require.Equal(t, "col-1", fakes.ingest.deletes[0].CollectionID)
	require.Empty(t, fakes.ingest.deletes[0].DocumentID)

	rec := serveAdmin(srv, adminRequest(http.MethodDelete, "/api/v1/documents/doc-1", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, fakes.ingest.deletes, 3)
	require.Equal(t, "doc-1", fakes.ingest.deletes[2].DocumentID)
	require.Empty(t, fakes.ingest.deletes[2].CollectionID)
}

// TestKnowledge_ErrorMappingBySentinels 判知识面的哨兵映射表：只认 `errors.Is`，
// 文案固定且不回声底层原因；三个调用方 429 码各不相同，速率拒绝带 Retry-After；
// 客户端放弃时**不写响应体**。
// 逆改点：把 ErrNotFound 当 500 → 404 行红；把 err.Error() 写进文案 → 泄漏断言红。
func TestKnowledge_ErrorMappingBySentinels(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)
	post := func() *httptest.ResponseRecorder {
		return serveAdmin(srv, adminRequest(http.MethodPost,
			"/api/v1/collections/col-1/documents", `{"path":"docs/a.md"}`))
	}

	cases := []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"越权", knowledge.ErrForbidden, http.StatusForbidden, codeForbidden, messageKnowledgeForbidden},
		{"路径拒绝", knowledge.ErrPathRejected, http.StatusBadRequest, codeBadRequest, messagePathRejected},
		{"请求非法", knowledge.ErrInvalidRequest, http.StatusBadRequest, codeBadRequest, messageKnowledgeInvalid},
		{
			"目标缺失", knowledge.ErrNotFound, http.StatusNotFound, codeNotFound,
			"未找到集合：col-1",
		},
		{"同名冲突", core.ErrConflict, http.StatusConflict, codeConflict, messageCollectionConflict},
		{
			"摄取不可用", knowledge.ErrIngestUnavailable, http.StatusServiceUnavailable,
			codeNotConfigured, messageIngestUnavailable,
		},
	}
	for _, tc := range cases {
		fakes.ingest.err = tc.err
		rec := post()
		require.Equal(t, tc.status, rec.Code, tc.name)
		env := decodeErrorEnvelope(t, rec)
		require.Equal(t, tc.code, env.Code, tc.name)
		require.Equal(t, tc.message, env.Message, tc.name)
	}

	// 三个调用方限制码各不相同：混用会让调用方把"并发名额"当成"速率超限"去等。
	limits := []struct {
		err  error
		code string
	}{
		{callers.ErrRateLimited, codeCallerRateLimited},
		{callers.ErrConcurrencyLimited, codeCallerConcurrencyLimited},
		{callers.ErrQuotaExhausted, codeQuotaExhausted},
	}
	for _, tc := range limits {
		fakes.ingest.err = tc.err
		rec := post()
		require.Equal(t, http.StatusTooManyRequests, rec.Code, tc.code)
		require.Equal(t, tc.code, decodeErrorEnvelope(t, rec).Code)
	}
	fakes.ingest.err = callers.ErrRateLimited
	require.NotEmpty(t, post().Header().Get("Retry-After"), "速率拒绝必须给 Retry-After")

	fakes.ingest.err = errors.New("boom: /abs/path 打不开")
	rec := post()
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, messageInternal, decodeErrorEnvelope(t, rec).Message)
	require.NotContains(t, rec.Body.String(), "boom", "底层原因只进日志")

	fakes.ingest.err = context.Canceled
	rec = post()
	require.Zero(t, rec.Body.Len(), "客户端放弃时不得写响应体")
}

// TestKnowledge_MissingPortsAnswerFixed500 判缺端口的十个出口一律固定 500 且**不带
// data**：空列表、空命中会把自己伪装成合法的空状态，那比报错更危险。
// 逆改点：任一 handler 缺端口时回 200 + 空数组 → 状态断言红；把端口错误兜成空结果
// → 同为红。
func TestKnowledge_MissingPortsAnswerFixed500(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, nil)
	requests := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/collections", ""},
		{http.MethodPost, "/api/v1/collections", `{"name":"c"}`},
		{http.MethodDelete, "/api/v1/collections/col-1", ""},
		{http.MethodGet, "/api/v1/collections/col-1/documents", ""},
		{http.MethodPost, "/api/v1/collections/col-1/documents", `{"path":"docs/a.md"}`},
		{http.MethodDelete, "/api/v1/documents/doc-1", ""},
		{http.MethodGet, "/api/v1/documents/doc-1/versions", ""},
		{http.MethodPost, "/api/v1/documents/doc-1/reindex", ""},
		{http.MethodPost, "/api/v1/search", `{"query":"q","collections":["col-1"]}`},
		{http.MethodPost, "/api/v1/sources/view", `{"ref":{"id":"doc-1","version_id":"ver-1"}}`},
	}
	for _, req := range requests {
		rec := serveAdmin(srv, adminRequest(req.method, req.path, req.body))
		require.Equal(t, http.StatusInternalServerError, rec.Code, req.path)
		env := decodeErrorEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code, req.path)
		require.Equal(t, messageInternal, env.Message, req.path)
		require.Nil(t, env.Data, "%s 不得用空数据冒充装配成功", req.path)
	}
}

// TestKnowledge_DeduplicatedFlagReachesTheResponse 守对外契约里的 `deduplicated`：
// 去重命中时响应体必须置 true、且不带作业号；重建索引的成功响应同样按该字段投影。
// 既有用例只覆盖 `deduplicated=false` 与非成功路径，字段漏赋值判不出来。
// 逆改点：视图里漏掉 `Deduplicated: result.Deduplicated` → 两处 true 断言同时红；
// 重建端点沿用错的结果变量 → doc-2/ver-2 断言红。
func TestKnowledge_DeduplicatedFlagReachesTheResponse(t *testing.T) {
	t.Parallel()
	srv, fakes := newKnowledgeServer(t, nil)

	fakes.ingest.ingestResult = knowledge.IngestResult{
		DocumentID: "doc-1", VersionID: "ver-1", Status: knowledge.VersionActive,
		Deduplicated: true, ChunkCount: 4, Generation: 1,
	}
	rec := serveAdmin(srv, adminRequest(http.MethodPost,
		"/api/v1/collections/col-1/documents", `{"path":"docs/a.md"}`))
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeKnowledge[IngestResultView](t, rec)
	require.True(t, env.Data.Deduplicated, "去重命中必须在响应体里如实给出")
	require.Empty(t, env.Data.IngestJobID, "去重不产生作业号")
	require.Equal(t, "ver-1", env.Data.VersionID)

	fakes.ingest.reindexResult = knowledge.IngestResult{
		DocumentID: "doc-2", VersionID: "ver-2", IngestJobID: "job-2",
		Status: knowledge.VersionActive, ChunkCount: 2, Generation: 1,
	}
	rec = serveAdmin(srv, adminRequest(http.MethodPost, "/api/v1/documents/doc-2/reindex", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, fakes.ingest.reindexCalls)
	require.Equal(t, "doc-2", fakes.ingest.reindexReq.DocumentID)
	reindexed := decodeKnowledge[IngestResultView](t, rec)
	require.False(t, reindexed.Data.Deduplicated, "内容变化的新版本不得报去重")
	require.Equal(t, "ver-2", reindexed.Data.VersionID)
	require.Equal(t, "job-2", reindexed.Data.IngestJobID)
}

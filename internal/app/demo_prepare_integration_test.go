//go:build integration

package app

// 本文件用**真实管理面**（真实 web 服务 + 真实 PostgreSQL + 本地向量桩）判
// `demo workflow prepare` 的端到端契约（FR-25-07～11 / C-20 / C-21 / C-22）：
//
//   - 计划零写入；应用按项回报（集合 / 每个文档 / 每个 Agent），HTTP 200 与退出码 0
//     都不是成功判据——向量调用失败的那一份必须显示 failed 并让命令失败；
//   - 第二次运行只补失败项：成功文档不再调用向量模型；
//   - 已有不等价绑定时报冲突、内容不变、整体标记部分成功；
//   - 未安装 / 未启用的角色直接失败并提示先安装启用，**不自动安装**。
//
// 管理面的 `GET/POST /agents/{name}/files`、`/content`、集合与摄取端点都是真实实现；
// 只有 OpenAI 兼容的 `/embeddings` 用本地桩（真实模型禁用）。
// Only the embeddings endpoint is a local stub; every management endpoint is real.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/web"
	"github.com/ymedword/basalt/internal/workspace"
)

// demoEmbeddingStub 是 OpenAI 兼容的向量端点桩：按 index 回 512 维向量；`failAt`
// 命中第 N 次请求时返回 500，用来构造"摄取失败但 HTTP 仍是 200"的终态。
// demoEmbeddingStub answers 512-dim vectors; failAt makes the Nth call fail with 500.
type demoEmbeddingStub struct {
	server   *httptest.Server
	requests atomic.Int64
	failAt   atomic.Int64
}

// newDemoEmbeddingStub 启动本地向量端点：`/embeddings` 之外一律 404。
// newDemoEmbeddingStub starts the local embeddings endpoint.
func newDemoEmbeddingStub(t *testing.T) *demoEmbeddingStub {
	t.Helper()
	stub := &demoEmbeddingStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/embeddings" {
			http.NotFound(w, r)
			return
		}
		number := stub.requests.Add(1)
		if failAt := stub.failAt.Load(); failAt > 0 && number == failAt {
			// 模拟向量提供方故障：摄取端必须把这份文档判 failed，**不**凭 HTTP 200 记成功。
			// A provider failure must surface as a failed document, never as success.
			http.Error(w, "synthetic embedding failure", http.StatusInternalServerError)
			return
		}
		var request struct {
			Model      string   `json:"model"`
			Input      []string `json:"input"`
			Dimensions int      `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		entries := make([]map[string]any, len(request.Input))
		for index := range request.Input {
			vector := make([]float64, knowledge.VectorDimensions)
			vector[index%knowledge.VectorDimensions] = 1
			entries[index] = map[string]any{"index": index, "embedding": vector}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  entries,
			"model": request.Model,
			"usage": map[string]any{"prompt_tokens": len(request.Input)},
		})
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

// demoHTTPFixture 起一套真实运行期 + 真实 HTTP 管理面，并把 demo 准备服务接到它上面。
// demoHTTPFixture runs the real runtime and the real management HTTP surface.
type demoHTTPFixture struct {
	t          *testing.T
	service    *DemoPrepareService
	client     *http.Client
	base       string
	admin      string
	embeddings *demoEmbeddingStub
}

// newDemoHTTPFixture 装配夹具：`agents` 是**已安装**的角色（未列出的角色不存在）。
// newDemoHTTPFixture writes exactly the listed agents; any other role is absent.
func newDemoHTTPFixture(t *testing.T, agents ...string) *demoHTTPFixture {
	t.Helper()

	embeddings := newDemoEmbeddingStub(t)
	fx := newWireFixture(t)
	for _, name := range agents {
		fx.writeAgent(t, name)
	}
	// 专用 embedding 连接：磁盘配置与内存配置按名字逐条对齐（种子版取磁盘原文），
	// 发布指针由 `seedPublishedProviders` 在建版事务里写下。
	// A dedicated embedding connection; the seeded publication wins the pointer.
	fx.cfg.Providers = append(fx.cfg.Providers, cli.ProviderConfig{
		Name:         "embedder",
		BaseURL:      embeddings.server.URL,
		APIKey:       "placeholder-embed-7c15",
		DefaultModel: "embed-model",
	})
	fx.cfg.Embedding = cli.EmbeddingConfig{
		ModelConnection: "embedder",
		Model:           "embed-model",
		Dimensions:      knowledge.VectorDimensions,
		BatchSize:       8,
	}
	fx.cfg.Callers.Ingress.Run = cli.IngressBudgetConfig{QPS: 100, Burst: 100}
	fx.cfg.Callers.Ingress.Read = cli.IngressBudgetConfig{QPS: 100, Burst: 100}
	fx.cfg.Callers.Ingress.Ingest = cli.IngressBudgetConfig{QPS: 100, Burst: 100}
	writeDemoPrepareConfig(t, fx, embeddings.server.URL)
	writeDemoSources(t, fx.root)

	rt := fx.assemble(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
			require.NoError(t, err)
		}
	})
	addr := ln.Addr().String()
	logger := quietAppLogger()
	admin := "synthetic-demo-prepare-admin-secret"
	guard, err := web.NewAuthGuardWithPrincipals(
		web.AdminAuthConfig{Mode: "token", Token: admin, AllowedHosts: []string{addr}, AllowedOrigins: []string{"http://" + addr}},
		logger, time.Now,
		callers.NewAuthenticator(postgres.NewKeyStore(rt.db), adminTokenFingerprint{token: admin}))
	require.NoError(t, err)
	opts := fx.app().webOptions(rt)
	opts.Addr, opts.Listener, opts.Logger, opts.AdminAuth = addr, ln, logger, guard
	opts.Ingress, err = newIngressLimiter(fx.cfg, rt.db, time.Now)
	require.NoError(t, err)
	server, err := web.NewServer(opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(15 * time.Second):
			t.Error("管理面 HTTP 服务没有在等待上限内退出")
		}
	})
	rt.adminPosture.Set(core.AdminPostureToken)

	// 凭据走配置（`http.admin_auth.token`）：命令只读、不铸钥。
	// The token comes from the loaded configuration; the command never mints one.
	fx.cfg.HTTP.AdminAuth = cli.AdminAuthConfig{Mode: adminAuthModeToken, Token: admin}
	service, err := newDemoPrepareService(fx.cfg, nil, fx.workDir, fx.root, logger)
	require.NoError(t, err)

	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &demoHTTPFixture{
		t:          t,
		service:    service,
		client:     &http.Client{Transport: transport, Timeout: 30 * time.Second},
		base:       "http://" + addr,
		admin:      admin,
		embeddings: embeddings,
	}
}

// writeDemoPrepareConfig 落与内存配置同源的 basalt.yaml：多一条 embedding 连接块。
// writeDemoPrepareConfig persists the same provider names the in-memory config declares.
func writeDemoPrepareConfig(t *testing.T, fx *wireFixture, embedURL string) {
	t.Helper()
	content := "database:\n" +
		"  dsn: ${BASALT_WIRE_DSN}\n" +
		"providers:\n" +
		"  - name: probe\n" +
		"    base_url: http://127.0.0.1:1\n" +
		"    api_key: ${BASALT_WIRE_KEY}\n" +
		"    default_model: probe-model\n" +
		"  - name: embedder\n" +
		"    base_url: " + embedURL + "\n" +
		"    api_key: ${BASALT_WIRE_KEY}\n" +
		"    default_model: embed-model\n"
	require.NoError(t, os.WriteFile(filepath.Join(fx.workDir, configFileName), []byte(content), 0o644))
}

// writeDemoSources 在 `.basalt/samples/bid-readiness/sources/` 下落固定 9 份素材。
// writeDemoSources writes the fixed nine demo documents.
func writeDemoSources(t *testing.T, workspaceRoot string) {
	t.Helper()
	dir := filepath.Join(workspaceRoot, demoWorkflowDirRel, "sources")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, document := range demoWorkflowDocuments() {
		path := filepath.Join(workspaceRoot, document.Path)
		body := "## 演示素材\n\n" + strings.TrimSuffix(filepath.Base(document.Path), ".md") + " 的合成正文，用于知识库摄取。\n"
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}
}

// request 以管理面凭据发一次请求并解信封。
// request issues one authenticated management request and decodes the envelope.
func (f *demoHTTPFixture) request(method, path string, body any, ifMatch string) (int, json.RawMessage, string) {
	f.t.Helper()
	var raw []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(f.t, err)
		raw = encoded
	}
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.base+path, bytes.NewReader(raw))
	require.NoError(f.t, err)
	req.Header.Set("Authorization", "Bearer "+f.admin)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := f.client.Do(req)
	require.NoError(f.t, err)
	defer func() { require.NoError(f.t, resp.Body.Close()) }()
	payload, err := io.ReadAll(resp.Body)
	require.NoError(f.t, err)
	var envelope struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	require.NoError(f.t, json.Unmarshal(payload, &envelope), "body=%s", payload)
	return resp.StatusCode, envelope.Data, string(payload)
}

// collectionIDs 读活动集合名 → ID；它是"计划没写"与"应用确实建了"的共同读数。
// collectionIDs reads the active collection names; it backs both zero-write and applied.
func (f *demoHTTPFixture) collectionIDs() map[string]string {
	f.t.Helper()
	status, data, raw := f.request(http.MethodGet, "/api/v1/collections", nil, "")
	require.Equal(f.t, http.StatusOK, status, raw)
	var views []collectionView
	if len(data) > 0 && string(data) != "null" {
		require.NoError(f.t, json.Unmarshal(data, &views))
	}
	ids := make(map[string]string, len(views))
	for _, view := range views {
		if view.DeletedAt == "" {
			ids[view.Name] = view.CollectionID
		}
	}
	return ids
}

// demoHTTPDocument 是文档列表的一行；失败摄取不激活版本，latest_status 与
// latest_version_id 都为空（失败终态在版本行上，见 documentVersions）。
// demoHTTPDocument is one row of the document listing; a failed ingest leaves no
// activated version, so both latest fields are empty.
type demoHTTPDocument struct {
	DocumentID      string `json:"document_id"`
	SourcePath      string `json:"source_path"`
	LatestVersionID string `json:"latest_version_id"`
	LatestStatus    string `json:"latest_status"`
	DeletedAt       string `json:"deleted_at"`
}

// demoHTTPVersion 是版本列表的一行。
// demoHTTPVersion is one row of the version listing.
type demoHTTPVersion struct {
	VersionID string `json:"version_id"`
	Status    string `json:"status"`
	ErrorKind string `json:"error_kind"`
}

// documentRows 读一个集合里已摄取文档的来源路径 → 文档行。
// documentRows reads source path → document row for one collection.
func (f *demoHTTPFixture) documentRows(collectionID string) map[string]demoHTTPDocument {
	f.t.Helper()
	status, data, raw := f.request(http.MethodGet, "/api/v1/collections/"+collectionID+"/documents", nil, "")
	require.Equal(f.t, http.StatusOK, status, raw)
	var views []demoHTTPDocument
	if len(data) > 0 && string(data) != "null" {
		require.NoError(f.t, json.Unmarshal(data, &views))
	}
	rows := make(map[string]demoHTTPDocument, len(views))
	for _, view := range views {
		if view.DeletedAt == "" {
			rows[view.SourcePath] = view
		}
	}
	return rows
}

// documentVersions 读一份文档的版本列表（服务端固定 created_at 倒序）。
// documentVersions lists a document's versions, newest first.
func (f *demoHTTPFixture) documentVersions(documentID string) []demoHTTPVersion {
	f.t.Helper()
	status, data, raw := f.request(http.MethodGet, "/api/v1/documents/"+documentID+"/versions", nil, "")
	require.Equal(f.t, http.StatusOK, status, raw)
	var views []demoHTTPVersion
	if len(data) > 0 && string(data) != "null" {
		require.NoError(f.t, json.Unmarshal(data, &views))
	}
	return views
}

// demoHTTPKeys 返回文档行的来源路径键（仅用于失败信息）。
// demoHTTPKeys lists the source paths of a row map for failure messages.
func demoHTTPKeys(rows map[string]demoHTTPDocument) []string {
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// agentContent 读 AGENT.md 原文与该文件的强 ETag。
// agentContent reads AGENT.md and its strong content ETag.
func (f *demoHTTPFixture) agentContent(name string) (string, string) {
	f.t.Helper()
	status, data, raw := f.request(http.MethodGet, "/api/v1/agents/"+name+"/content", nil, "")
	require.Equal(f.t, http.StatusOK, status, raw)
	var view struct {
		Content string `json:"content"`
		ETag    string `json:"etag"`
	}
	require.NoError(f.t, json.Unmarshal(data, &view))
	require.NotEmpty(f.t, view.ETag)
	return view.Content, view.ETag
}

// agentFilesETag 读受管文件集合的 ETag——`POST /files` 的 If-Match 基础值。
// agentFilesETag reads the file-set ETag that POST /files requires as If-Match.
func (f *demoHTTPFixture) agentFilesETag(name string) string {
	f.t.Helper()
	status, data, raw := f.request(http.MethodGet, "/api/v1/agents/"+name+"/files", nil, "")
	require.Equal(f.t, http.StatusOK, status, raw)
	var view struct {
		ETag string `json:"etag"`
	}
	require.NoError(f.t, json.Unmarshal(data, &view))
	require.NotEmpty(f.t, view.ETag)
	return view.ETag
}

// postAgentFiles 以 If-Match 条件写一份 AGENT.md。
// postAgentFiles conditionally submits one AGENT.md.
func (f *demoHTTPFixture) postAgentFiles(name, content, ifMatch string) (int, string) {
	f.t.Helper()
	status, _, raw := f.request(http.MethodPost, "/api/v1/agents/"+name+"/files",
		map[string]any{"files": []map[string]string{{"path": "AGENT.md", "content": content}}}, ifMatch)
	return status, raw
}

// requireDemoBindings 判正文里的知识绑定与目标等价（顺序无关，逐项名字 + 权限）。
// requireDemoBindings compares the declared bindings with the wanted set.
func requireDemoBindings(t *testing.T, content string, want []core.KnowledgeBinding) {
	t.Helper()
	got, err := workspace.ParseKnowledgeBindings([]byte(content))
	require.NoError(t, err)
	require.True(t, demoBindingsEquivalent(got, want), "正文里的知识绑定是 %v，要求 %v", got, want)
}

// TestDemoPrepare_HTTPFilesAndKnowledge 覆盖 FR-25-07～11 的真实端点链路。
// TestDemoPrepare_HTTPFilesAndKnowledge covers FR-25-07..11 against the real endpoints.
func TestDemoPrepare_HTTPFilesAndKnowledge(t *testing.T) {
	f := newDemoHTTPFixture(t, "bid-requirements", "bid-technical")
	req := cli.DemoPrepareRequest{Server: f.base}
	ctx := t.Context()
	// 安装时的原始正文（无 knowledge 块）：后面模拟"用户修好冲突"时整体恢复它。
	originalTechnical, _ := f.agentContent("bid-technical")

	// ── 计划：只读，全新建 ───────────────────────────────────────────────────
	plan, err := f.service.Plan(ctx, req)
	require.NoError(t, err)
	require.Len(t, plan.Collections, 2)
	for _, collection := range plan.Collections {
		require.Equal(t, demoActionCreate, collection.Action)
		require.False(t, collection.Exists)
	}
	require.Len(t, plan.Documents, 9)
	for _, document := range plan.Documents {
		require.Equal(t, demoActionIngest, document.Action)
	}
	require.Len(t, plan.Bindings, 2)
	for _, binding := range plan.Bindings {
		require.Equal(t, demoActionBind, binding.Action)
	}
	require.NotEmpty(t, plan.Warnings, "计划必须给出 embedding 费用提示")
	require.Contains(t, strings.Join(plan.Warnings, "\n"), "embedding")
	require.Empty(t, f.collectionIDs(), "计划阶段不得建集合")
	require.Zero(t, f.embeddings.requests.Load(), "计划阶段不得调用向量模型")

	// ── 应用：故意让第 3 份文档的向量调用失败 ───────────────────────────────
	// 摄取端对失败也回 200（终态在 body 里）：命令必须逐项判 status，而不是看 HTTP。
	f.embeddings.failAt.Store(3)
	result, err := f.service.Apply(ctx, req, plan)
	require.Error(t, err, "有文档摄取失败时不得以整体成功收场")
	require.Contains(t, err.Error(), "有 1 个文档摄取失败", "失败项必须点名")
	require.False(t, result.Partial, "文档失败是硬错误，不是绑定部分成功")
	require.Len(t, result.Collections, 2)
	for _, collection := range result.Collections {
		require.True(t, collection.Created, "集合 %s 应被创建", collection.Name)
		require.NotEmpty(t, collection.CollectionID)
	}
	require.Len(t, result.Documents, 9)
	failedPath := plan.Documents[2].Path
	seen := map[string]string{}
	for _, document := range result.Documents {
		require.NotEmpty(t, document.Status, "%s 必须有终态", document.Path)
		seen[document.Path] = document.Status
	}
	require.Equal(t, "failed", seen[failedPath], "第 3 次向量调用失败的那份必须是 failed")
	active := 0
	for path, status := range seen {
		if path == failedPath {
			continue
		}
		require.Equal(t, "active", status, "%s 应摄取成功", path)
		active++
	}
	require.Equal(t, 8, active)
	require.Len(t, result.Bindings, 2)
	for _, binding := range result.Bindings {
		require.Equal(t, demoActionApplied, binding.Status, "绑定 %s 应已写入", binding.Agent)
	}

	// 独立复核：读数来自管理面本身，而不是服务的报告。失败摄取**不激活版本**：
	// 文档行在（latest_status 与 latest_version_id 为空），失败终态在版本行上。
	ids := f.collectionIDs()
	require.Len(t, ids, 2)
	requestDocs := f.documentRows(ids[demoCollectionRequest])
	require.Len(t, requestDocs, 8)
	failedDoc, ok := requestDocs[failedPath]
	require.True(t, ok, "集合里应有失败文档 %q，实有 %v", failedPath, demoHTTPKeys(requestDocs))
	require.Empty(t, failedDoc.LatestStatus, "失败的摄取不得激活版本")
	require.Empty(t, failedDoc.LatestVersionID, "失败的摄取不得推进 latest_version_id")
	versions := f.documentVersions(failedDoc.DocumentID)
	require.NotEmpty(t, versions, "失败版本仍应可查")
	require.Equal(t, "failed", versions[0].Status, "最新版本必须是 failed（版本列表按 created_at 倒序）")
	require.Equal(t, "embed_failed", versions[0].ErrorKind, "失败原因必须落库")
	productDocs := f.documentRows(ids[demoCollectionProduct])
	require.Len(t, productDocs, 1)
	require.Equal(t, "active", productDocs[demoWorkflowDirRel+"/sources/product-main.md"].LatestStatus)
	requirements, _ := f.agentContent("bid-requirements")
	requireDemoBindings(t, requirements, []core.KnowledgeBinding{{Name: demoCollectionRequest, Access: core.KnowledgeAccessRead}})
	technical, _ := f.agentContent("bid-technical")
	requireDemoBindings(t, technical, []core.KnowledgeBinding{
		{Name: demoCollectionProduct, Access: core.KnowledgeAccessRead},
		{Name: demoCollectionRequest, Access: core.KnowledgeAccessRead},
	})

	// ── 第二次：只补失败的那一份，成功的不再调用向量模型 ────────────────────
	f.embeddings.failAt.Store(0)
	before := f.embeddings.requests.Load()
	plan, err = f.service.Plan(ctx, req)
	require.NoError(t, err)
	toIngest := 0
	for _, document := range plan.Documents {
		if document.Action == demoActionIngest {
			toIngest++
			require.Equal(t, failedPath, document.Path, "只有失败的那份需要重摄取")
			continue
		}
		require.Equal(t, demoActionUnchanged, document.Action, "%s 应判 content 未变", document.Path)
	}
	require.Equal(t, 1, toIngest)
	for _, binding := range plan.Bindings {
		require.Equal(t, demoActionUnchanged, binding.Action)
	}
	// 还要补摄取 1 份，所以费用提示必须**仍在**（且只按 1 次调用计），不能因为
	// "第二次运行"就提前静音；零摄取的第三次运行才允许无提示。
	require.Len(t, plan.Warnings, 1, "补摄取 1 份时仍须提示 embedding 费用")
	require.Contains(t, plan.Warnings[0], "1 次 embedding", "费用提示按待摄取份数计")
	result, err = f.service.Apply(ctx, req, plan)
	require.NoError(t, err)
	require.False(t, result.Partial)
	for _, document := range result.Documents {
		require.Equal(t, "active", document.Status, "%s 应补摄取成功", document.Path)
	}
	require.Equal(t, before+1, f.embeddings.requests.Load(), "只补失败项：恰好一次向量调用")

	// 独立复核：重试之后九份文档在管理面都是 active——不只看命令自己的报告。
	requestDocs = f.documentRows(ids[demoCollectionRequest])
	require.Len(t, requestDocs, 8)
	for path, document := range requestDocs {
		require.Equal(t, "active", document.LatestStatus, "%s 重试后应 active", path)
		require.NotEmpty(t, document.LatestVersionID)
	}
	productDocs = f.documentRows(ids[demoCollectionProduct])
	require.Len(t, productDocs, 1)
	for path, document := range productDocs {
		require.Equal(t, "active", document.LatestStatus, "%s 应 active", path)
	}

	// ── 第三次：幂等，零新写入、零向量调用 ──────────────────────────────────
	before = f.embeddings.requests.Load()
	plan, err = f.service.Plan(ctx, req)
	require.NoError(t, err)
	for _, collection := range plan.Collections {
		require.Equal(t, demoActionReuse, collection.Action)
		require.NotEmpty(t, collection.CollectionID)
	}
	for _, document := range plan.Documents {
		require.Equal(t, demoActionUnchanged, document.Action)
	}
	for _, binding := range plan.Bindings {
		require.Equal(t, demoActionUnchanged, binding.Action)
	}
	require.Empty(t, plan.Warnings, "零摄取时不得再报费用提示")
	result, err = f.service.Apply(ctx, req, plan)
	require.NoError(t, err)
	require.False(t, result.Partial)
	for _, collection := range result.Collections {
		require.False(t, collection.Created)
	}
	for _, binding := range result.Bindings {
		require.Equal(t, demoActionUnchanged, binding.Status)
	}
	require.Equal(t, before, f.embeddings.requests.Load(), "幂等重跑不得产生新的向量调用")

	// ── 冲突：他人写入不等价绑定 → 报冲突、不覆盖、整体部分成功 ─────────────
	conflicting := []core.KnowledgeBinding{{Name: demoCollectionProduct, Access: core.KnowledgeAccessRead}}
	current, _ := f.agentContent("bid-technical")
	conflictBody, err := workspace.ApplyChanges([]byte(current), core.AgentContentChanges{Knowledge: &conflicting})
	require.NoError(t, err)
	// `POST /files` 的 If-Match 是**文件集合** ETag，不是单文件内容 ETag。
	status, raw := f.postAgentFiles("bid-technical", string(conflictBody), f.agentFilesETag("bid-technical"))
	require.Equal(t, http.StatusOK, status, raw)

	plan, err = f.service.Plan(ctx, req)
	require.NoError(t, err)
	for _, binding := range plan.Bindings {
		if binding.Agent == "bid-technical" {
			require.Equal(t, demoActionConflict, binding.Action, "不等价绑定必须判冲突")
			continue
		}
		require.Equal(t, demoActionUnchanged, binding.Action)
	}
	result, err = f.service.Apply(ctx, req, plan)
	require.NoError(t, err)
	require.True(t, result.Partial, "一个 Agent 冲突必须标记部分成功")
	for _, binding := range result.Bindings {
		if binding.Agent == "bid-technical" {
			require.Equal(t, demoActionConflict, binding.Status)
			continue
		}
		require.Equal(t, demoActionUnchanged, binding.Status)
	}
	technical, _ = f.agentContent("bid-technical")
	requireDemoBindings(t, technical, conflicting)

	// ── 用户修好冲突后重跑：只补失败的那个 Agent ────────────────────────────
	// 恢复成安装时的原始正文（无 knowledge 块）——用户"修好"的等价动作。
	status, raw = f.postAgentFiles("bid-technical", originalTechnical, f.agentFilesETag("bid-technical"))
	require.Equal(t, http.StatusOK, status, raw)
	plan, err = f.service.Plan(ctx, req)
	require.NoError(t, err)
	for _, binding := range plan.Bindings {
		want := demoActionBind
		if binding.Agent == "bid-requirements" {
			want = demoActionUnchanged
		}
		require.Equal(t, want, binding.Action, "%s 的动作", binding.Agent)
	}
	result, err = f.service.Apply(ctx, req, plan)
	require.NoError(t, err)
	require.False(t, result.Partial)
	for _, binding := range result.Bindings {
		want := demoActionApplied
		if binding.Agent == "bid-requirements" {
			want = demoActionUnchanged
		}
		require.Equal(t, want, binding.Status, "%s 的结果", binding.Agent)
	}
	technical, _ = f.agentContent("bid-technical")
	requireDemoBindings(t, technical, []core.KnowledgeBinding{
		{Name: demoCollectionProduct, Access: core.KnowledgeAccessRead},
		{Name: demoCollectionRequest, Access: core.KnowledgeAccessRead},
	})
}

// TestDemoPrepare_HTTPMissingAgentFailsClosed 覆盖 FR-25-08 的失败面：角色没安装 /
// 没启用时直接失败并提示先安装启用，**不得**自动安装。
// TestDemoPrepare_HTTPMissingAgentFailsClosed pins the missing-role failure.
func TestDemoPrepare_HTTPMissingAgentFailsClosed(t *testing.T) {
	f := newDemoHTTPFixture(t, "bid-requirements")
	_, err := f.service.Plan(t.Context(), cli.DemoPrepareRequest{Server: f.base})
	require.Error(t, err)
	require.ErrorIs(t, err, errDemoAgentMissing, "未安装的角色必须是可识别的缺失错误")
	require.Contains(t, err.Error(), "basalt demo workflow install", "必须给出可执行的安装指引")
	require.Contains(t, err.Error(), "bid-technical")
	require.Empty(t, f.collectionIDs(), "失败时不得自动建集合或安装模板")
	require.Zero(t, f.embeddings.requests.Load())
}

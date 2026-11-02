//go:build e2e

package e2e

// 本文件是 US-16 颗粒度文档 §6.1 的**可执行 Demo**（T099 → stage `us16-e2e`）：
// 知识库的摄取 / 版本 / 切分 / 索引代际与生效点（G1-1），加上诊断读取的三条判据
// （第 12 点：按 `run_id` 读装配记录与原因码、没有记录的 `run_id` 必须 `missing=true`、
// 用 caller key 访问管理面必须被拒）。
//
// 边界：
//   - **真实二进制 + 回环假服务**：假模型（`internal/testutil`）与假 embedding 端点都
//     走 `127.0.0.1`，不连任何真实供应商；embedding 只判协议形状与 512 维，不判语义质量。
//   - **运行期存储只有 PostgreSQL**：隔离 schema 由 `newHarness` 的 `pgtemp` 提供，
//     缺 `BASALT_TEST_PG_DSN` 即失败（不跳过）。
//   - **判据落在产品行为上**：版本生效点用「旧版本的来源视图失效 + 检索只回新版本」
//     判，不用库内行数代替；诊断用「模型调用次数 = `llm_call` 事件条数」判一一对应。
//
// This file is the executable US-16 demo: ingest/version/chunking/generation and the
// effective point (G1-1) plus the three diagnostics readings of §6.1 item 12. Every
// judgement is on product behaviour over a real binary against loopback fakes.

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/testutil"
)

// 本 Demo 固定的 fixture 名字：embedding 连接与模型贯穿配置与断言，只在这里定义一次。
// The demo's fixed embedding connection and model names are defined here once.
const (
	us16EmbedProvider = "embed-fake"
	us16EmbedModel    = "fake-embedding"
	us16AgentName     = "demo-agent"
	us16ManualPath    = "docs/ops-manual.md"
)

// 两份手册正文：第二次摄取换内容，判"新版本生效、旧版本失效"。
// Two manual revisions: the second ingest must activate a new version.
const (
	us16ManualV1 = "# 运维手册\n\n## 备份\n\n备份保留 7 天，每天 02:00 执行全量备份。\n"
	us16ManualV2 = "# 运维手册\n\n## 备份\n\n备份保留 30 天，每天 03:00 执行全量备份。\n"
	// us16ManualPhrase 是两版都含有的片段：来源视图的正文断言用它，不判版本差异。
	// The shared phrase keeps the source-view assertion about the body, not the revision.
	us16ManualPhrase = "备份保留"
)

// TestUS16Demo 是 US-16 的用户可见 Demo：§6.1 的知识库链路 + 第 12 点诊断读取。
// TestUS16Demo is the user-visible US-16 demo.
func TestUS16Demo(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)

	addr := freeLoopbackAddr(t)
	chat := newFakeModel(t, textResponse("备份保留 7 天。"), textResponse("备份保留 7 天。"))
	embed := newUS16EmbeddingServer(t)
	writeUS16Manual(t, h, us16ManualV1)

	providers := "providers:\n" +
		v3ProviderBlock(fixtureProvider, chat.URL(), fixtureModel) +
		v3ProviderBlock(us16EmbedProvider, embed.URL(), us16EmbedModel)
	h.writeConfig(t, v3ServeConfig(addr, providers,
		us16EmbeddingSection()+us16CallersSection()))
	writeWebAgent(t, h, us16AgentName, nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	// 调用方 Key 由管理面签发；业务面全程只用 Key，管理面仍用管理 token。
	// Key 带 `agents:invoke:demo-agent`：本 Demo 会在会话入口跑这个 Agent，而记忆
	// 注入的读取复核不因入口是「会话消息」就放行（缺 scope = 运行 fail-closed）。
	// The caller key is issued by the management surface; business calls use only the key.
	key := us16IssueCallerKey(t, addr, us16AgentName)
	header := map[string]string{"Authorization": "Bearer " + key}

	// [1] 建集合 → 摄取 → 版本与代际。
	collectionID := us16CreateCollection(t, addr, header)
	first := us16Ingest(t, addr, header, collectionID)
	if first.Status != "active" {
		t.Fatalf("首次摄取：status = %q，期望 active（终态之一）", first.Status)
	}
	if first.Deduplicated {
		t.Fatalf("首次摄取：deduplicated = true，期望 false（新内容不是去重命中）")
	}
	if first.ChunkCount < 1 {
		t.Fatalf("首次摄取：chunk_count = %d，期望 ≥ 1（切分必须产出条目）", first.ChunkCount)
	}
	if first.Generation != 1 {
		t.Fatalf("首次摄取：generation = %d，期望 1（启动期固定的首代际）", first.Generation)
	}
	if first.DocumentID == "" || first.VersionID == "" || first.IngestJobID == "" {
		t.Fatalf("首次摄取：标识不全（document=%q version=%q job=%q）",
			first.DocumentID, first.VersionID, first.IngestJobID)
	}
	// embedding 连接真的被用上了：至少一次按配置的模型与维度发出的向量化调用。
	// The embedding connection was actually used with the configured model and width.
	if got := embed.callsWith(us16EmbedModel, knowledge.VectorDimensions); got < 1 {
		t.Fatalf("摄取没有按配置调用 embedding（model=%q dimensions=%d，观测 %d 次）",
			us16EmbedModel, knowledge.VectorDimensions, got)
	}

	versions := us16ListVersions(t, addr, header, first.DocumentID)
	if len(versions) != 1 {
		t.Fatalf("首次摄取后版本数 = %d，期望 1（%+v）", len(versions), versions)
	}
	if versions[0].VersionID != first.VersionID || versions[0].Status != "active" {
		t.Fatalf("首次摄取后的版本 = %+v，期望 %s / active", versions[0], first.VersionID)
	}

	// [2] 检索 → 来源视图（生效版本可读且带正文）。
	// Search hits the active version; the source view returns the body.
	hit := us16SearchTop(t, addr, header, collectionID, "备份保留多久")
	if hit.DocumentID != first.DocumentID || hit.VersionID != first.VersionID {
		t.Fatalf("检索命中的版本 = %s/%s，期望当前生效版本 %s/%s",
			hit.DocumentID, hit.VersionID, first.DocumentID, first.VersionID)
	}
	view := us16SourceView(t, addr, header, hit.DocumentID, hit.VersionID)
	if !view.Validity.Valid {
		t.Fatalf("生效版本的来源视图 validity.valid = false（reason=%q），期望 true",
			view.Validity.Reason)
	}
	if !strings.Contains(view.Text, us16ManualPhrase) {
		t.Fatalf("生效版本的来源视图正文 = %q，期望包含 %q", view.Text, us16ManualPhrase)
	}

	// [3] 改写文件 → 重新摄取 → 新版本生效、旧版本按 version_changed 失效。
	// The rewrite activates a new version; the old one becomes invalid with a reason.
	writeUS16Manual(t, h, us16ManualV2)
	second := us16Ingest(t, addr, header, collectionID)
	if second.Status != "active" || second.Deduplicated {
		t.Fatalf("再次摄取：status=%q deduplicated=%v，期望 active / false",
			second.Status, second.Deduplicated)
	}
	if second.DocumentID != first.DocumentID {
		t.Fatalf("同一路径的两次摄取落到不同文档：%s vs %s", second.DocumentID, first.DocumentID)
	}
	if second.VersionID == first.VersionID {
		t.Fatalf("内容已改写却复用了同一版本 %s——版本没有前进", second.VersionID)
	}
	if second.Generation != first.Generation {
		t.Fatalf("再次摄取：generation = %d，期望与首代际 %d 相同", second.Generation, first.Generation)
	}

	versions = us16ListVersions(t, addr, header, first.DocumentID)
	if len(versions) != 2 {
		t.Fatalf("再次摄取后版本数 = %d，期望 2（%+v）", len(versions), versions)
	}
	if versions[0].VersionID != second.VersionID || versions[0].Status != "active" {
		t.Fatalf("版本列表首项 = %+v，期望最新版本 %s / active（倒序）", versions[0], second.VersionID)
	}
	if versions[1].VersionID != first.VersionID || versions[1].Status != "superseded" {
		t.Fatalf("版本列表次项 = %+v，期望旧版本 %s / superseded", versions[1], first.VersionID)
	}

	// 生效点：检索只回新版本；旧版本的来源视图失效且**不**回正文。
	// The effective point: search returns only the new version, and the old version's
	// source view is invalid and carries no body.
	hit = us16SearchTop(t, addr, header, collectionID, "备份保留多久")
	if hit.VersionID != second.VersionID {
		t.Fatalf("再次检索命中版本 = %s，期望新版本 %s", hit.VersionID, second.VersionID)
	}
	stale := us16SourceView(t, addr, header, first.DocumentID, first.VersionID)
	if stale.Validity.Valid {
		t.Fatalf("旧版本的来源视图仍然 valid=true——版本切换没有生效")
	}
	if stale.Validity.Reason != "version_changed" {
		t.Fatalf("旧版本失效原因 = %q，期望 version_changed", stale.Validity.Reason)
	}
	if stale.Text != "" {
		t.Fatalf("失效版本不得回正文，实际 %q", stale.Text)
	}

	// [4] 业务 Run：会话 + 消息走调用方 Key，产出一条可关联的运行。
	// One business run through the caller key produces a correlatable run.
	session := us16CreateSession(t, addr, header)
	run := us16PostMessage(t, addr, header, session)
	if run.Status != "completed" {
		t.Fatalf("消息触发的运行 status = %q，期望 completed（reason=%q）", run.Status, run.Reason)
	}
	if run.RunID == "" || run.FinalText == "" {
		t.Fatalf("运行读数不全（run_id=%q final_text=%q）", run.RunID, run.FinalText)
	}
	modelCalls := len(chat.Requests())
	if modelCalls < 1 {
		t.Fatalf("运行没有触达模型（假模型收到 %d 次调用）", modelCalls)
	}

	// [5] 诊断读取：管理面可按 run_id 读到记录；未知 run_id 必须 missing=true；
	// 调用方 Key 访问同一端点必须 403，不得被当成"没有记录"的 200 空集。
	// The management surface reads by run_id; an unknown run must be missing=true and
	// a caller key must be refused with 403 rather than answered with an empty page.
	diag := us16Diagnostics(t, addr, run.RunID, nil)
	if diag.Missing {
		t.Fatalf("刚完成的运行读不到诊断记录（missing=true），run_id=%s", run.RunID)
	}
	if diag.SampledOut {
		t.Fatalf("采样率缺省即全量，不得出现 sampled_out=true")
	}
	// 调用级 llm_call 与模型调用一一对应；阶段级 llm_call 是 US-16A 的真实阶段耗时
	// 证据（复用同一诊断种类，见 run-state R633），另计且必须带白名单内的阶段名。
	// Call-level llm_call events map one-to-one onto model calls; phase-level ones are
	// the US-16A phase timings and are counted separately.
	llmEvents := 0
	phaseEvents := 0
	for _, event := range diag.Events {
		if event.Correlation.RunID != run.RunID || event.Kind != "llm_call" {
			continue
		}
		phase, isPhase := event.Attrs["phase"].(string)
		if !isPhase {
			llmEvents++
			continue
		}
		if phase != "model_first_response" && phase != "model_response_read" {
			t.Fatalf("阶段事件的阶段名越界：%q（只允许 model_first_response / model_response_read）", phase)
		}
		if event.ReasonCode != "" {
			t.Fatalf("阶段事件不得携带失败原因码 %q（阶段证据不回答调用成败）", event.ReasonCode)
		}
		phaseEvents++
	}
	if llmEvents != modelCalls {
		t.Fatalf("llm_call 调用级诊断事件 %d 条，模型调用 %d 次，期望一一对应（事件 %+v）",
			llmEvents, modelCalls, diag.Events)
	}
	if phaseEvents != 2*modelCalls {
		t.Fatalf("llm_call 阶段事件 %d 条，模型调用 %d 次，期望每次两个阶段（首响应 + 响应读取）",
			phaseEvents, modelCalls)
	}

	unknown := us16Diagnostics(t, addr, "run_us16_demo_unknown", nil)
	if !unknown.Missing {
		t.Fatalf("没有记录的 run_id 必须 missing=true，实际 %+v", unknown)
	}
	if len(unknown.Events) != 0 {
		t.Fatalf("没有记录的 run_id 不得返回事件，实际 %d 条", len(unknown.Events))
	}

	denied := webRequest(t, addr, http.MethodGet, "/api/v1/diagnostics?run_id="+run.RunID, "", header)
	requireWebStatus(t, denied, http.StatusForbidden, "FORBIDDEN", "管理面请求被拒绝",
		"调用方 Key 读诊断")
}

// us16EmbeddingSection 返回 `embedding` 段：连接名、模型与冻结维度。
// us16EmbeddingSection renders the embedding section with the frozen width.
func us16EmbeddingSection() string {
	return "embedding:\n" +
		"  model_connection: " + us16EmbedProvider + "\n" +
		"  model: " + us16EmbedModel + "\n" +
		"  dimensions: " + strconv.Itoa(knowledge.VectorDimensions) + "\n"
}

// us16CallersSection 返回入口令牌桶段：三类入口都必须给非零 qps/burst——`serve`
// 启动前由 `cli.ValidateServeIngress` 校验六项齐全且 > 0，缺任一项**拒绝启动**
// （不是运行期 429）；装配层也不预填缺省，零预算仍按失败关闭执行。
// 这是 US-16 的部署事实，不是夹具偏好：把这里写成"测试专用宽容值"会掩盖真实默认。
// us16CallersSection renders the ingress buckets; serve refuses to start when any of
// the six keys is missing or non-positive, so the demo must configure them explicitly.
func us16CallersSection() string {
	budget := "      qps: 50\n      burst: 50\n"
	return "callers:\n  ingress:\n    run:\n" + budget +
		"    ingest:\n" + budget +
		"    read:\n" + budget
}

// writeUS16Manual 把手册正文写进工作区允许根（`<Root>/.basalt`）下的相对路径。
// 摄取路径是相对**工作区**的，不是相对进程工作目录——写错位置会得到 400 而非本用例
// 想判的行为。
// writeUS16Manual writes the manual under the workspace root; ingest paths are
// relative to the workspace, not the process working directory.
func writeUS16Manual(t *testing.T, h *harness, body string) {
	t.Helper()
	path := filepath.Join(h.Root, ".basalt", filepath.FromSlash(us16ManualPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建文档目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写手册失败: %v", err)
	}
}

// us16IssueCallerKey 经管理面建调用方并签发业务 scope 的 Key；明文只在这里出现。
//
// `agents` 是要放行调用的 Agent 名：记忆注入的读取复核在**任何**取值下都先判
// `agents:invoke:<name>`（技术方案 §3.5.2「配置只决定读哪几层，不决定能不能读」，
// 撤销 / 缺 scope 一律拒），只带 `sessions:write` 的 Key 在带记忆的 Agent 上会被
// fail-closed 拒掉。所以跑 Agent 的用例必须点名它，纯知识库用例不点名——保持最小授权。
// us16IssueCallerKey creates a caller and issues a business-scoped key; each name in
// `agents` adds `agents:invoke:<name>`, which the memory read gate always requires.
func us16IssueCallerKey(t *testing.T, addr string, agents ...string) string {
	t.Helper()
	createBody := mustJSON(map[string]any{
		"name": "us16-demo-caller",
		// 空 project = 不受项目过滤；限额给足，本用例判的是链路而不是限流。
		"limits": map[string]any{
			"qps":                     100,
			"burst":                   100,
			"max_concurrent":          8,
			"tasks_per_period":        1000,
			"tokens_per_period":       1000000,
			"ingest_bytes_per_period": 10000000,
			"period":                  "day",
			"timezone":                "UTC",
		},
	})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/callers", createBody, nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "建调用方")
	var caller struct {
		CallerID string `json:"caller_id"`
	}
	decodeWebData(t, resp, &caller, "建调用方")
	if caller.CallerID == "" {
		t.Fatalf("建调用方：caller_id 为空（%s）", resp.Raw)
	}

	scopes := []string{
		"sessions:read", "sessions:write", "knowledge:read", "knowledge:write",
	}
	for _, agent := range agents {
		// 前缀逐字取自 `core.AgentInvokeScopePrefix`（e2e 只驱动二进制，不 import 核心包）。
		scopes = append(scopes, "agents:invoke:"+agent)
	}
	issueBody := mustJSON(map[string]any{
		"scopes":     scopes,
		"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
	resp = webRequest(t, addr, http.MethodPost, "/api/v1/callers/"+caller.CallerID+"/keys", issueBody, nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "签发调用方 Key")
	var issued struct {
		Key string `json:"key"`
	}
	decodeWebData(t, resp, &issued, "签发调用方 Key")
	if issued.Key == "" {
		t.Fatalf("签发调用方 Key：明文为空（%s）", resp.Raw)
	}
	return issued.Key
}

// us16CreateCollection 建集合并返回标识。
// us16CreateCollection creates one collection and returns its id.
func us16CreateCollection(t *testing.T, addr string, header map[string]string) string {
	t.Helper()
	body := mustJSON(map[string]any{"name": "us16-demo"})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/collections", body, header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "建集合")
	var collection struct {
		CollectionID string `json:"collection_id"`
	}
	decodeWebData(t, resp, &collection, "建集合")
	if collection.CollectionID == "" {
		t.Fatalf("建集合：collection_id 为空（%s）", resp.Raw)
	}
	return collection.CollectionID
}

// us16IngestReading 是摄取结果的判卷读数。
// us16IngestReading is the verdict-side ingest result.
type us16IngestReading struct {
	DocumentID   string `json:"document_id"`
	VersionID    string `json:"version_id"`
	IngestJobID  string `json:"ingest_job_id"`
	Status       string `json:"status"`
	Deduplicated bool   `json:"deduplicated"`
	ChunkCount   int    `json:"chunk_count"`
	Generation   int    `json:"generation"`
	ErrorKind    string `json:"error_kind"`
}

// us16Ingest 摄取工作区内的手册路径；失败（含 200 + failed 终态）立即判红。
// us16Ingest ingests the manual; a failed terminal state fails the test at once.
func us16Ingest(t *testing.T, addr string, header map[string]string, collectionID string) us16IngestReading {
	t.Helper()
	body := mustJSON(map[string]any{"path": us16ManualPath})
	resp := webRequest(t, addr, http.MethodPost,
		"/api/v1/collections/"+collectionID+"/documents", body, header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "摄取文档")
	var reading us16IngestReading
	decodeWebData(t, resp, &reading, "摄取文档")
	if reading.Status == "failed" {
		t.Fatalf("摄取终态为 failed（error_kind=%q）", reading.ErrorKind)
	}
	return reading
}

// us16VersionReading 是版本列表的判卷读数。
// us16VersionReading is one version row as the list returns it.
type us16VersionReading struct {
	VersionID  string `json:"version_id"`
	DocumentID string `json:"document_id"`
	Status     string `json:"status"`
	Generation int    `json:"generation"`
}

// us16ListVersions 读文档的版本列表（固定倒序）。
// us16ListVersions reads the newest-first version list.
func us16ListVersions(t *testing.T, addr string, header map[string]string, documentID string) []us16VersionReading {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/documents/"+documentID+"/versions", "", header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "列版本")
	var versions []us16VersionReading
	decodeWebData(t, resp, &versions, "列版本")
	return versions
}

// us16SearchReading 是检索结果的判卷读数（只取本用例用到的字段）。
// us16SearchReading is the search result slice this demo judges.
type us16SearchReading struct {
	Hits       []us16SearchHit `json:"hits"`
	Strategy   string          `json:"strategy"`
	Generation int             `json:"generation"`
}

// us16SearchHit 是一条检索命中。
// us16SearchHit is one search hit.
type us16SearchHit struct {
	DocumentID string `json:"document_id"`
	VersionID  string `json:"version_id"`
	Snippet    string `json:"snippet"`
}

// us16SearchTop 检索并返回首条命中；零命中或向量路未生效都判红——本 Demo 的检索
// 判据正是"生效版本可被检索到"，零命中等价于链路断了。
// us16SearchTop searches and returns the first hit; zero hits or a degraded vector
// path fails, because the demo's point is that the active version is retrievable.
func us16SearchTop(t *testing.T, addr string, header map[string]string, collectionID, query string) us16SearchHit {
	t.Helper()
	body := mustJSON(map[string]any{"query": query, "collections": []string{collectionID}})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/search", body, header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "检索")
	var reading us16SearchReading
	decodeWebData(t, resp, &reading, "检索")
	if len(reading.Hits) == 0 {
		t.Fatalf("检索零命中（strategy=%q generation=%d）", reading.Strategy, reading.Generation)
	}
	// 代际只有向量路成功时才带：>0 同时证明"查询向量真的算出来了"。
	// The generation is only carried when the vector path succeeded.
	if reading.Generation < 1 {
		t.Fatalf("检索 generation = %d，期望 ≥1（向量路未生效，strategy=%q）",
			reading.Generation, reading.Strategy)
	}
	return reading.Hits[0]
}

// us16SourceViewReading 是来源视图的判卷读数。
// us16SourceViewReading is the source-view reading.
type us16SourceViewReading struct {
	Validity struct {
		Valid  bool   `json:"valid"`
		Reason string `json:"reason"`
	} `json:"validity"`
	Text string `json:"text"`
}

// us16SourceView 按引用读来源视图（缺版本是 200 + validity 结论，不是错误）。
// us16SourceView reads one source view; a missing version is a 200 validity verdict.
func us16SourceView(t *testing.T, addr string, header map[string]string, documentID, versionID string) us16SourceViewReading {
	t.Helper()
	body := mustJSON(map[string]any{
		"ref": map[string]any{"id": documentID, "version_id": versionID},
	})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/sources/view", body, header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "来源视图")
	var reading us16SourceViewReading
	decodeWebData(t, resp, &reading, "来源视图")
	return reading
}

// us16CreateSession 用调用方 Key 建会话并返回标识。
// us16CreateSession creates a session with the caller key.
func us16CreateSession(t *testing.T, addr string, header map[string]string) string {
	t.Helper()
	body := mustJSON(map[string]any{"agent": us16AgentName})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/sessions", body, header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "建会话")
	var session struct {
		SessionID string `json:"session_id"`
	}
	decodeWebData(t, resp, &session, "建会话")
	if session.SessionID == "" {
		t.Fatalf("建会话：session_id 为空（%s）", resp.Raw)
	}
	return session.SessionID
}

// us16PostMessage 发一条消息并等运行终态；非 completed 带上原因判红。
// us16PostMessage posts one message and requires a completed run.
func us16PostMessage(t *testing.T, addr string, header map[string]string, sessionID string) webRun {
	t.Helper()
	body := mustJSON(map[string]any{"message": "备份保留多久？"})
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", body, header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "发消息")
	var run webRun
	decodeWebData(t, resp, &run, "发消息")
	return run
}

// us16DiagnosticsReading 是 `GET /diagnostics` 的判卷读数。
// us16DiagnosticsReading is the diagnostics page reading.
type us16DiagnosticsReading struct {
	Events     []us16DiagnosticEvent `json:"events"`
	Missing    bool                  `json:"missing"`
	SampledOut bool                  `json:"sampled_out"`
}

// us16DiagnosticEvent 是一条诊断事件（只取本用例判的字段）。
// us16DiagnosticEvent is one diagnostic event as this demo judges it.
type us16DiagnosticEvent struct {
	EventID     string         `json:"event_id"`
	Kind        string         `json:"kind"`
	ReasonCode  string         `json:"reason_code"`
	Attrs       map[string]any `json:"attrs"`
	Correlation struct {
		RunID string `json:"run_id"`
	} `json:"correlation"`
}

// us16Diagnostics 按 run_id 读一页诊断事件；header 为 nil 时用管理 token。
// us16Diagnostics reads one diagnostics page; a nil header uses the admin token.
func us16Diagnostics(t *testing.T, addr, runID string, header map[string]string) us16DiagnosticsReading {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/diagnostics?run_id="+runID, "", header)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读诊断")
	var reading us16DiagnosticsReading
	decodeWebData(t, resp, &reading, "读诊断")
	return reading
}

// us16EmbeddingCall 是假 embedding 端点收到的一次请求。
// us16EmbeddingCall is one request the fake embedding endpoint received.
type us16EmbeddingCall struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	Dimensions     int      `json:"dimensions"`
	EncodingFormat string   `json:"encoding_format"`
}

// us16EmbeddingServer 是 OpenAI 兼容 `/embeddings` 的回环假端点：按输入逐条回
// 512 维确定性向量，并记录每次调用的形状供判据读取。
// us16EmbeddingServer is the loopback fake /embeddings endpoint: it answers one
// deterministic 512-float vector per input and records each call's shape.
type us16EmbeddingServer struct {
	t      *testing.T
	server *httptest.Server

	mu    sync.Mutex
	calls []us16EmbeddingCall
}

// newUS16EmbeddingServer 起假 embedding 端点（用例结束自动关闭）。
// newUS16EmbeddingServer starts the fake endpoint and closes it with the test.
func newUS16EmbeddingServer(t *testing.T) *us16EmbeddingServer {
	t.Helper()
	s := &us16EmbeddingServer{t: t}
	s.server = testutil.NewHTTPServer(t, http.HandlerFunc(s.handle))
	return s
}

// URL 返回端点地址（供 provider 的 base_url 用；客户端自己补 `/embeddings`）。
// URL returns the endpoint base; the client appends /embeddings itself.
func (s *us16EmbeddingServer) URL() string { return s.server.URL }

// callsWith 返回"模型名与维度都符合配置"的调用次数。
// callsWith counts the calls whose model and width match the configuration.
func (s *us16EmbeddingServer) callsWith(model string, dimensions int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	matched := 0
	for _, call := range s.calls {
		if call.Model == model && call.Dimensions == dimensions {
			matched++
		}
	}
	return matched
}

func (s *us16EmbeddingServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/embeddings" {
		http.NotFound(w, r)
		return
	}
	var request us16EmbeddingCall
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		s.t.Errorf("假 embedding：请求体不是预期 JSON: %v", err)
		http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.calls = append(s.calls, request)
	s.mu.Unlock()

	data := make([]map[string]any, 0, len(request.Input))
	for index, text := range request.Input {
		data = append(data, map[string]any{
			"object":    "embedding",
			"index":     index,
			"embedding": us16Vector(text),
		})
	}
	body := mustJSON(map[string]any{
		"object": "list",
		"model":  request.Model,
		"data":   data,
		"usage":  map[string]any{"prompt_tokens": len(request.Input), "total_tokens": len(request.Input)},
	})
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		s.t.Errorf("假 embedding：写响应失败: %v", err)
	}
}

// us16Vector 按文本生成确定性向量：同一文本恒得同一向量，不同文本走不同方向。
// 取值落在 [-1, 1]，范数非零——余弦距离有定义，向量路不会因零向量失败。
// us16Vector derives a deterministic non-zero vector from the text so cosine
// distance stays defined; identical text always yields the identical vector.
func us16Vector(text string) []float32 {
	sum := sha256.Sum256([]byte(text))
	state := uint64(1469598103934665603)
	for _, b := range sum {
		state = (state ^ uint64(b)) * 1099511628211
	}
	out := make([]float32, knowledge.VectorDimensions)
	for i := range out {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		out[i] = float32(int64(state%2001)-1000) / 1000
	}
	return out
}

//go:build integration

package app

// 本文件用**管理面替身**判 `demo workflow prepare` 的两段契约（US-25 §2.3.7 / C-20 /
// C-21 / C-22）：计划零写入、ETag 钉住、冲突不改写、部分成功可续、凭据只读。
// 替身只实现本命令用到的六个端点，并把每次请求记进日志——"计划没有写请求"直接读它，
// 不去猜命令内部调了什么。

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// demoFakeAgent 是替身里的一个 Agent：正文、文件集合 ETag 与"第几次 GET /files 之后
// 模拟他人改动"。
// demoFakeAgent carries the body, the file-set ETag and a concurrent-change trigger.
type demoFakeAgent struct {
	content string
	etag    string
	// rotateAfterFilesRead 是"第 N 次 GET /files 之后改一次正文"的触发器（0 = 不改）：
	// 读取与提交之间的并发写就靠它构造。
	// rotateAfterFilesRead mutates the body after the Nth GET /files (0 = never).
	rotateAfterFilesRead int
	filesRead            int
}

// demoFakeDocument 是替身里的一个文档行。
// demoFakeDocument is one document row.
type demoFakeDocument struct {
	sourcePath string
	status     string
}

// demoFakeAdmin 是管理面替身：集合、文档、Agent 文件集合与凭据校验。
// demoFakeAdmin is the minimal management stub.
type demoFakeAdmin struct {
	t        *testing.T
	server   *httptest.Server
	token    string
	mu       sync.Mutex
	nextID   int
	names    []string
	ids      map[string]string
	docs     map[string][]demoFakeDocument
	agents   map[string]*demoFakeAgent
	requests []string
	ifMatch  []string
	rejects  int
	// readOrder 按到达顺序记录 Agent 读取事件（`<agent>:files` / `<agent>:content`），
	// 供 §4.6.6 的读取顺序判据使用：先取基础集合 ETag，再读正文。
	// readOrder records agent reads in arrival order for the §4.6.6 ordering check.
	readOrder []string
}

// newDemoFakeAdmin 启动替身：两个 Agent 都以"没有 knowledge 块"的正文开始。
// newDemoFakeAdmin starts the stub with both agents present and unbound.
func newDemoFakeAdmin(t *testing.T, token string) *demoFakeAdmin {
	t.Helper()
	fake := &demoFakeAdmin{
		t:      t,
		token:  token,
		ids:    map[string]string{},
		docs:   map[string][]demoFakeDocument{},
		agents: map[string]*demoFakeAgent{},
	}
	for _, name := range []string{"bid-requirements", "bid-technical"} {
		content := demoFakeAgentMD(name, "")
		fake.agents[name] = &demoFakeAgent{content: content, etag: demoFakeETag(content)}
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

// demoFakeAgentMD 造一份最小合法 AGENT.md；`knowledge` 非空时追加一个知识块。
// demoFakeAgentMD renders a minimal valid AGENT.md, optionally with a knowledge block.
func demoFakeAgentMD(name, knowledge string) string {
	return "---\n" +
		"name: " + name + "\n" +
		"display_name: 替身角色\n" +
		"description: 合成演示。\n" +
		"provider:\n" +
		"  name: demo-qwen\n" +
		"  model: qwen-plus\n" +
		"tools:\n" +
		"  - read_file\n" +
		"settings:\n" +
		"  include_global_memory: false\n" +
		knowledge +
		"---\n\n正文。\n"
}

// demoFakeETag 按服务端同形（`"sha256:<64 hex>"`）产出强 ETag。
// demoFakeETag renders a strong ETag in the server's shape.
func demoFakeETag(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("\"sha256:%x\"", sum)
}

// writeJSON 写管理面信封。
// writeJSON writes the management envelope.
func demoFakeJSON(w http.ResponseWriter, status int, code string, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": message, "data": data})
}

// handle 路由六个端点；所有写请求都会记入 `requests` 供"零写入"判据读取。
// handle routes the six endpoints and records every request.
func (f *demoFakeAdmin) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		f.rejects++
		demoFakeJSON(w, http.StatusUnauthorized, "UNAUTHORIZED", "凭据无效", nil)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "collections" && r.Method == http.MethodGet:
		list := make([]map[string]any, 0, len(f.names))
		for _, name := range f.names {
			list = append(list, map[string]any{"collection_id": f.ids[name], "name": name, "deleted_at": ""})
		}
		demoFakeJSON(w, http.StatusOK, "OK", "ok", list)
	case len(parts) == 1 && parts[0] == "collections" && r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := f.ids[body.Name]; ok {
			demoFakeJSON(w, http.StatusConflict, "CONFLICT", "同名集合已存在", nil)
			return
		}
		f.nextID++
		id := fmt.Sprintf("col_%03d", f.nextID)
		f.ids[body.Name] = id
		f.names = append(f.names, body.Name)
		demoFakeJSON(w, http.StatusOK, "OK", "ok", map[string]any{"collection_id": id, "name": body.Name})
	case len(parts) == 3 && parts[0] == "collections" && parts[2] == "documents" && r.Method == http.MethodGet:
		rows := make([]map[string]any, 0, len(f.docs[parts[1]]))
		for _, doc := range f.docs[parts[1]] {
			rows = append(rows, map[string]any{"source_path": doc.sourcePath, "latest_status": doc.status, "deleted_at": ""})
		}
		demoFakeJSON(w, http.StatusOK, "OK", "ok", rows)
	case len(parts) == 3 && parts[0] == "collections" && parts[2] == "documents" && r.Method == http.MethodPost:
		var body struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		deduplicated := false
		for index := range f.docs[parts[1]] {
			if f.docs[parts[1]][index].sourcePath == body.Path {
				if f.docs[parts[1]][index].status == "active" {
					deduplicated = true
				}
				f.docs[parts[1]][index].status = "active"
				demoFakeJSON(w, http.StatusOK, "OK", "ok", map[string]any{"document_id": "doc_1", "status": "active", "deduplicated": deduplicated})
				return
			}
		}
		f.docs[parts[1]] = append(f.docs[parts[1]], demoFakeDocument{sourcePath: body.Path, status: "active"})
		demoFakeJSON(w, http.StatusOK, "OK", "ok", map[string]any{"document_id": "doc_1", "status": "active", "deduplicated": false})
	case len(parts) == 3 && parts[0] == "agents" && parts[2] == "files" && r.Method == http.MethodGet:
		agent, ok := f.agents[parts[1]]
		if !ok {
			demoFakeJSON(w, http.StatusNotFound, "NOT_FOUND", "Agent 不存在", nil)
			return
		}
		f.readOrder = append(f.readOrder, parts[1]+":files")
		agent.filesRead++
		w.Header().Set("ETag", agent.etag)
		demoFakeJSON(w, http.StatusOK, "OK", "ok", map[string]any{"agent": parts[1], "etag": agent.etag, "files": []map[string]any{{"path": "AGENT.md", "size": len(agent.content)}}})
		if agent.rotateAfterFilesRead != 0 && agent.filesRead == agent.rotateAfterFilesRead {
			// 模拟"读取与提交之间他人改了正文"：ETag 随之失效。
			// Simulate a concurrent edit between the read and the submit.
			agent.content = strings.Replace(agent.content, "正文。", "正文（他人修改）。", 1)
			agent.etag = demoFakeETag(agent.content)
		}
	case len(parts) == 3 && parts[0] == "agents" && parts[2] == "content" && r.Method == http.MethodGet:
		agent, ok := f.agents[parts[1]]
		if !ok {
			demoFakeJSON(w, http.StatusNotFound, "NOT_FOUND", "Agent 不存在", nil)
			return
		}
		f.readOrder = append(f.readOrder, parts[1]+":content")
		demoFakeJSON(w, http.StatusOK, "OK", "ok", map[string]any{"content": agent.content, "etag": agent.etag})
	case len(parts) == 3 && parts[0] == "agents" && parts[2] == "files" && r.Method == http.MethodPost:
		agent, ok := f.agents[parts[1]]
		if !ok {
			demoFakeJSON(w, http.StatusNotFound, "NOT_FOUND", "Agent 不存在", nil)
			return
		}
		ifMatch := r.Header.Get("If-Match")
		f.ifMatch = append(f.ifMatch, ifMatch)
		if ifMatch != agent.etag {
			demoFakeJSON(w, http.StatusPreconditionFailed, "PRECONDITION_FAILED", "文件集合已被其他写入修改", nil)
			return
		}
		var body struct {
			Files []struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			} `json:"files"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Files) != 1 || body.Files[0].Path != "AGENT.md" {
			f.t.Errorf("提交的文件集合是 %+v，要求只有 AGENT.md", body.Files)
			demoFakeJSON(w, http.StatusBadRequest, "BAD_REQUEST", "只允许 AGENT.md", nil)
			return
		}
		agent.content = body.Files[0].Content
		agent.etag = demoFakeETag(agent.content)
		w.Header().Set("ETag", agent.etag)
		demoFakeJSON(w, http.StatusOK, "OK", "ok", map[string]any{"agent": parts[1], "etag": agent.etag, "files": []map[string]any{{"path": "AGENT.md", "size": len(agent.content)}}})
	default:
		f.t.Errorf("替身收到未实现的请求 %s %s", r.Method, r.URL.Path)
		demoFakeJSON(w, http.StatusNotFound, "NOT_FOUND", "未实现", nil)
	}
}

// writes 返回所有写请求（计划零写入的判据）。
// writes returns every write request.
func (f *demoFakeAdmin) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, request := range f.requests {
		if !strings.HasPrefix(request, http.MethodGet+" ") {
			out = append(out, request)
		}
	}
	return out
}

// readEvents 返回读取事件副本（断言时不持请求锁）。
// readEvents returns a copy of the recorded read events.
func (f *demoFakeAdmin) readEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.readOrder...)
}

// requireFilesReadBeforeContent 判 §4.6.6 的读取顺序：写 AGENT.md 前必须先
// `GET /files` 取基础集合 ETag，再 `GET /content` 读正文。计划阶段也会读这两项，
// 所以这里取**最后一次**出现的次序——应用阶段在计划之后，最后那一对正是写入前
// 的读取。逆改点：调换应用阶段这两次读取的次序（先读正文再取 ETag）——本断言随即红。
// requireFilesReadBeforeContent pins the §4.6.6 read order for the apply phase: the
// planning pass also reads both, so the last occurrences are the pre-write pair.
// Swapping the two apply-phase reads turns this assertion red.
func requireFilesReadBeforeContent(t *testing.T, fake *demoFakeAdmin, agent string) {
	t.Helper()
	filesAt, contentAt := -1, -1
	for index, event := range fake.readEvents() {
		if event == agent+":files" {
			filesAt = index
		}
		if event == agent+":content" {
			contentAt = index
		}
	}
	require.NotEqual(t, -1, filesAt, "%s 从未读取文件集合 ETag（GET /files）", agent)
	require.NotEqual(t, -1, contentAt, "%s 从未读取正文（GET /content）", agent)
	require.Less(t, filesAt, contentAt, "%s 必须先 GET /files 再 GET /content（§4.6.6）", agent)
}

// content 读当前 Agent 正文。
// content returns the current body.
func (f *demoFakeAdmin) content(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.agents[name].content
}

// newDemoPrepareFixture 造一个工作区 + 服务实例：`server` 指向替身，token 直接注入配置。
// newDemoPrepareFixture wires a service against the stub with an explicit config token.
func newDemoPrepareFixture(t *testing.T, fake *demoFakeAdmin) *DemoPrepareService {
	t.Helper()
	base := t.TempDir()
	workDir := filepath.Join(base, "workspace")
	require.NoError(t, os.MkdirAll(workDir, 0o750))
	cfg := &cli.Config{HTTP: cli.HTTPConfig{Addr: "127.0.0.1:8080", AdminAuth: cli.AdminAuthConfig{Mode: adminAuthModeToken, Token: fake.token}}}
	service, err := newDemoPrepareService(cfg, nil, workDir, filepath.Join(workDir, workspaceDirName), quietAppLogger())
	require.NoError(t, err)
	return service
}

// planRequest 是"显式给替身地址 + 配置凭据"的输入。
// planRequest points at the stub and relies on the configured token.
func planRequest(fake *demoFakeAdmin) cli.DemoPrepareRequest {
	return cli.DemoPrepareRequest{Server: fake.server.URL}
}

// TestDemoPrepare_PlanNoWrites 覆盖 C-20：计划只发 GET，逐项列出集合 / 文档 / 绑定，
// 并给出 embedding 调用提示。
// TestDemoPrepare_PlanNoWrites covers C-20: the plan is read-only and lists every item.
func TestDemoPrepare_PlanNoWrites(t *testing.T) {
	fake := newDemoFakeAdmin(t, "test-token")
	service := newDemoPrepareFixture(t, fake)

	plan, err := service.Plan(context.Background(), planRequest(fake))
	require.NoError(t, err)
	require.Empty(t, fake.writes(), "计划阶段不得发出任何写请求")

	require.Len(t, plan.Collections, 2)
	for _, collection := range plan.Collections {
		require.Equal(t, demoActionCreate, collection.Action)
		require.False(t, collection.Exists)
	}
	require.Len(t, plan.Documents, 9)
	for _, document := range plan.Documents {
		require.Equal(t, demoActionIngest, document.Action)
		require.True(t, strings.HasPrefix(document.Path, demoWorkflowDirRel+"/sources/"))
	}
	require.Len(t, plan.Bindings, 2)
	for _, binding := range plan.Bindings {
		require.Equal(t, demoActionBind, binding.Action)
	}
	require.Equal(t, "http://"+strings.TrimPrefix(fake.server.URL, "http://"), plan.Server)
	require.NotEmpty(t, plan.Warnings)
	require.Contains(t, strings.Join(plan.Warnings, " "), "embedding")
}

// TestDemoPrepare_TokenSources 覆盖 C-22：配置 token → 实例私有 admin-token →
// 显式 --token-file 三种来源，缺失时报错且不发请求。
// TestDemoPrepare_TokenSources covers the three read-only credential sources.
func TestDemoPrepare_TokenSources(t *testing.T) {
	fixture := newAdminTokenFixture(t)
	instanceID := fixture.writeRoster(t)
	tokenPath := fixture.tokenPath(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, os.WriteFile(tokenPath, []byte("file-token\n"), 0o600))

	fake := newDemoFakeAdmin(t, "file-token")
	service, err := newDemoPrepareService(tokenModeConfig(), nil, fixture.workDir,
		filepath.Join(fixture.workDir, workspaceDirName), quietAppLogger())
	require.NoError(t, err)

	// ① 配置 token 优先于私有文件。
	configured := tokenModeConfig()
	configured.HTTP.AdminAuth.Token = "config-token"
	fakeConfigured := newDemoFakeAdmin(t, "config-token")
	serviceConfigured, err := newDemoPrepareService(configured, nil, fixture.workDir,
		filepath.Join(fixture.workDir, workspaceDirName), quietAppLogger())
	require.NoError(t, err)
	_, err = serviceConfigured.Plan(context.Background(), planRequest(fakeConfigured))
	require.NoError(t, err)
	require.Zero(t, fakeConfigured.rejects)

	// ② 配置没有 token 时读实例私有文件。
	_, err = service.Plan(context.Background(), planRequest(fake))
	require.NoError(t, err)
	require.Zero(t, fake.rejects, "实例私有 admin-token 必须被用上")

	// ③ 显式 --token-file 覆盖配置；凭据文件走与 flow run 同一套权限判卷，先收紧 ACL。
	overrideDir := t.TempDir()
	tightenMasterKeyACLForTest(t, overrideDir, true)
	overridePath := filepath.Join(overrideDir, "token")
	require.NoError(t, os.WriteFile(overridePath, []byte("config-token\n"), 0o600))
	tightenMasterKeyACLForTest(t, overridePath, false)
	_, err = serviceConfigured.Plan(context.Background(), cli.DemoPrepareRequest{
		Server: fakeConfigured.server.URL, TokenFile: overridePath,
	})
	require.NoError(t, err)

	// ④ 三处都没有：失败且零请求。
	emptyWorkDir := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.MkdirAll(filepath.Join(emptyWorkDir, workspaceDirName), 0o750))
	serviceMissing, err := newDemoPrepareService(tokenModeConfig(), nil, emptyWorkDir,
		filepath.Join(emptyWorkDir, workspaceDirName), quietAppLogger())
	require.NoError(t, err)
	before := len(fake.requestsUnderLock())
	_, err = serviceMissing.Plan(context.Background(), planRequest(fake))
	// 无名册 → "实例名册尚未建立"；有名册无文件 → "没有可用的管理面凭据"。两者都点名
	// admin-token，且都不回显任何取值。
	require.ErrorContains(t, err, "admin-token")
	require.Equal(t, before, len(fake.requestsUnderLock()), "凭据缺失时不得发出请求")

	require.NotEmpty(t, instanceID)
}

// requestsUnderLock 返回请求快照（供"零请求"判据使用）。
// requestsUnderLock snapshots the request log.
func (f *demoFakeAdmin) requestsUnderLock() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// TestDemoPrepare_RemotePlainHTTPRejected 覆盖 C-22 的传输纪律：非回环明文 HTTP 在发
// 请求之前就被拒绝。
// TestDemoPrepare_RemotePlainHTTPRejected refuses remote plaintext before any request.
func TestDemoPrepare_RemotePlainHTTPRejected(t *testing.T) {
	fake := newDemoFakeAdmin(t, "test-token")
	service := newDemoPrepareFixture(t, fake)

	for _, server := range []string{
		"http://192.0.2.10:8080",
		"http://example.invalid",
		"http://127.0.0.1:8080/path",
		"http://user:pass@127.0.0.1:8080",
		"ftp://127.0.0.1:8080",
	} {
		_, err := service.Plan(context.Background(), cli.DemoPrepareRequest{Server: server})
		require.Error(t, err, server)
	}
	require.Empty(t, fake.requestsUnderLock(), "地址校验失败时不得发出请求")

	// 回环明文合法。
	_, err := service.Plan(context.Background(), cli.DemoPrepareRequest{Server: fake.server.URL})
	require.NoError(t, err)
}

// TestDemoPrepare_ApplyBindsAndIngests 覆盖 C-21：建集合、摄取 9 个文档、只提交
// AGENT.md 且 If-Match 是基础集合 ETag、正文里的绑定与目标一致。
// TestDemoPrepare_ApplyBindsAndIngests covers the applying half end to end.
func TestDemoPrepare_ApplyBindsAndIngests(t *testing.T) {
	fake := newDemoFakeAdmin(t, "test-token")
	service := newDemoPrepareFixture(t, fake)

	plan, err := service.Plan(context.Background(), planRequest(fake))
	require.NoError(t, err)
	result, err := service.Apply(context.Background(), planRequest(fake), plan)
	require.NoError(t, err)
	require.False(t, result.Partial)

	require.Len(t, result.Collections, 2)
	for _, collection := range result.Collections {
		require.True(t, collection.Created)
		require.NotEmpty(t, collection.CollectionID)
	}
	require.Len(t, result.Documents, 9)
	for _, document := range result.Documents {
		require.Equal(t, "active", document.Status)
	}
	require.Len(t, result.Bindings, 2)
	for _, binding := range result.Bindings {
		require.Equal(t, demoActionApplied, binding.Status)
	}
	for _, agent := range []string{"bid-requirements", "bid-technical"} {
		requireFilesReadBeforeContent(t, fake, agent)
	}
	require.Len(t, fake.ifMatch, 2, "两个 Agent 各提交一次")
	for _, value := range fake.ifMatch {
		require.True(t, strings.HasPrefix(value, `"sha256:`), "If-Match 必须是基础强 ETag：%q", value)
	}

	bindings, err := workspace.ParseKnowledgeBindings([]byte(fake.content("bid-requirements")))
	require.NoError(t, err)
	require.Equal(t, []core.KnowledgeBinding{{Name: demoCollectionRequest, Access: core.KnowledgeAccessRead}}, bindings)

	bindings, err = workspace.ParseKnowledgeBindings([]byte(fake.content("bid-technical")))
	require.NoError(t, err)
	require.Equal(t, []core.KnowledgeBinding{
		{Name: demoCollectionProduct, Access: core.KnowledgeAccessRead},
		{Name: demoCollectionRequest, Access: core.KnowledgeAccessRead},
	}, bindings)
}

// TestDemoPrepare_SecondRunIdempotent 覆盖 C-21 的幂等：第二次运行集合复用、文档不
// 再提交、绑定不变，写请求为零。
// TestDemoPrepare_SecondRunIdempotent proves the second run writes nothing.
func TestDemoPrepare_SecondRunIdempotent(t *testing.T) {
	fake := newDemoFakeAdmin(t, "test-token")
	service := newDemoPrepareFixture(t, fake)
	plan, err := service.Plan(context.Background(), planRequest(fake))
	require.NoError(t, err)
	_, err = service.Apply(context.Background(), planRequest(fake), plan)
	require.NoError(t, err)

	writesBefore := len(fake.writes())
	secondPlan, err := service.Plan(context.Background(), planRequest(fake))
	require.NoError(t, err)
	for _, collection := range secondPlan.Collections {
		require.Equal(t, demoActionReuse, collection.Action)
	}
	for _, document := range secondPlan.Documents {
		require.Equal(t, demoActionUnchanged, document.Action, document.Path)
	}
	for _, binding := range secondPlan.Bindings {
		require.Equal(t, demoActionUnchanged, binding.Action, binding.Agent)
	}
	second, err := service.Apply(context.Background(), planRequest(fake), secondPlan)
	require.NoError(t, err)
	require.False(t, second.Partial)
	require.Equal(t, writesBefore, len(fake.writes()), "第二次运行不得产生写请求")
	for _, document := range second.Documents {
		require.Equal(t, "active", document.Status)
	}
}

// TestDemoPrepare_RefusesConflictingBinding 覆盖 C-21 的冲突拒绝：已有非空且不等价的
// 绑定时该 Agent 不改写、内容不变、Partial 为真。
// TestDemoPrepare_RefusesConflictingBinding leaves a conflicting body untouched.
func TestDemoPrepare_RefusesConflictingBinding(t *testing.T) {
	fake := newDemoFakeAdmin(t, "test-token")
	conflicting := demoFakeAgentMD("bid-technical",
		"knowledge:\n  - name: other-collection\n    access: read\n")
	fake.agents["bid-technical"] = &demoFakeAgent{content: conflicting, etag: demoFakeETag(conflicting)}
	service := newDemoPrepareFixture(t, fake)

	plan, err := service.Plan(context.Background(), planRequest(fake))
	require.NoError(t, err)
	conflicts := 0
	for _, binding := range plan.Bindings {
		if binding.Agent == "bid-technical" {
			require.Equal(t, demoActionConflict, binding.Action)
			conflicts++
		}
	}
	require.Equal(t, 1, conflicts)

	result, err := service.Apply(context.Background(), planRequest(fake), plan)
	require.NoError(t, err)
	require.True(t, result.Partial)
	require.Equal(t, conflicting, fake.content("bid-technical"), "冲突绑定不得被改写")
	require.Len(t, fake.ifMatch, 1, "只有 bid-requirements 提交")

	// 写前重判本身也要能独立判冲突（结构非法按冲突处理）。
	conflict, err := bindingConflicts(conflicting, []core.KnowledgeBinding{{Name: demoCollectionRequest, Access: core.KnowledgeAccessRead}})
	require.NoError(t, err)
	require.True(t, conflict)
	conflict, err = bindingConflicts(demoFakeAgentMD("bid-technical", ""), []core.KnowledgeBinding{{Name: demoCollectionRequest, Access: core.KnowledgeAccessRead}})
	require.NoError(t, err)
	require.False(t, conflict)
}

// TestDemoPrepare_ETagPinnedAcrossReads 覆盖 C-21 的 ETag 钉住：读取与提交之间他人
// 改动导致 412，该 Agent 记 stale、内容不变、**不**用新 ETag 重提。
// TestDemoPrepare_ETagPinnedAcrossReads turns a concurrent edit into stale, never a retry.
func TestDemoPrepare_ETagPinnedAcrossReads(t *testing.T) {
	fake := newDemoFakeAdmin(t, "test-token")
	// 计划阶段读第 1 次 /files；应用阶段读第 2 次之后就模拟他人改动。
	fake.agents["bid-requirements"].rotateAfterFilesRead = 2
	service := newDemoPrepareFixture(t, fake)

	plan, err := service.Plan(context.Background(), planRequest(fake))
	require.NoError(t, err)
	before := fake.content("bid-requirements")
	result, err := service.Apply(context.Background(), planRequest(fake), plan)
	require.NoError(t, err)
	require.True(t, result.Partial)
	statuses := map[string]string{}
	for _, binding := range result.Bindings {
		statuses[binding.Agent] = binding.Status
	}
	require.Equal(t, demoActionStale, statuses["bid-requirements"])
	require.Equal(t, demoActionApplied, statuses["bid-technical"])
	require.NotEqual(t, before, fake.content("bid-requirements"), "他人改动必须保留")
	require.NotContains(t, fake.content("bid-requirements"), "knowledge:", "旧正文不得配上新 ETag 重提")
	require.Len(t, fake.ifMatch, 2, "stale 之后不得重试：bid-requirements 只提交一次")
	for _, agent := range []string{"bid-requirements", "bid-technical"} {
		requireFilesReadBeforeContent(t, fake, agent)
	}
}

// TestDemoPrepare_PartialBindingResumable 覆盖 C-21 的可续：部分成功后重跑只补失败项。
// TestDemoPrepare_PartialBindingResumable resumes only the failed agent.
func TestDemoPrepare_PartialBindingResumable(t *testing.T) {
	fake := newDemoFakeAdmin(t, "test-token")
	conflicting := demoFakeAgentMD("bid-technical",
		"knowledge:\n  - name: other-collection\n    access: read\n")
	fake.agents["bid-technical"] = &demoFakeAgent{content: conflicting, etag: demoFakeETag(conflicting)}
	service := newDemoPrepareFixture(t, fake)

	plan, err := service.Plan(context.Background(), planRequest(fake))
	require.NoError(t, err)
	first, err := service.Apply(context.Background(), planRequest(fake), plan)
	require.NoError(t, err)
	require.True(t, first.Partial)

	// 用户把冲突修掉后重跑：只补 bid-technical，bid-requirements 保持 unchanged。
	fake.mu.Lock()
	fake.agents["bid-technical"].content = demoFakeAgentMD("bid-technical", "")
	fake.agents["bid-technical"].etag = demoFakeETag(fake.agents["bid-technical"].content)
	fake.mu.Unlock()

	resumed, err := service.Plan(context.Background(), planRequest(fake))
	require.NoError(t, err)
	for _, binding := range resumed.Bindings {
		switch binding.Agent {
		case "bid-requirements":
			require.Equal(t, demoActionUnchanged, binding.Action)
		case "bid-technical":
			require.Equal(t, demoActionBind, binding.Action)
		}
	}
	second, err := service.Apply(context.Background(), planRequest(fake), resumed)
	require.NoError(t, err)
	require.False(t, second.Partial)
	for _, binding := range second.Bindings {
		require.Contains(t, []string{demoActionApplied, demoActionUnchanged}, binding.Status)
	}
	bindings, err := workspace.ParseKnowledgeBindings([]byte(fake.content("bid-technical")))
	require.NoError(t, err)
	require.Len(t, bindings, 2)
}

// TestDemoPrepare_MissingAgentFailsClosed 覆盖 Q-25-14：模板 / 角色未安装或未启用时
// 直接失败并提示先 `demo workflow install`，命令**不**自动安装。
// TestDemoPrepare_MissingAgentFailsClosed never installs on the user's behalf.
func TestDemoPrepare_MissingAgentFailsClosed(t *testing.T) {
	fake := newDemoFakeAdmin(t, "test-token")
	delete(fake.agents, "bid-technical")
	service := newDemoPrepareFixture(t, fake)

	_, err := service.Plan(context.Background(), planRequest(fake))
	require.Error(t, err)
	require.ErrorIs(t, err, errDemoAgentMissing)
	require.Contains(t, err.Error(), "basalt demo workflow install")
	require.Empty(t, fake.writes(), "缺角色时不得发生任何写入")
}

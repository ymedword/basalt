//go:build e2e

package e2e

// US-18 容器 e2e 的素材层：桥网关上可达的假模型、配置播种、管理面 / 调用方 / 知识库
// 的容器版助手与 PostgreSQL 判读。素材一律按既有 e2e 的形状复制到本栈的宿主端口与
// 管理令牌上——**不**复用固定回环地址 + 固定管理令牌的进程级助手。
// The US-18 fixture materials: a fake model reachable from the container, config
// seeding, container-side admin/caller/knowledge helpers and PostgreSQL readings.
// The existing process-level helpers are bound to a loopback address and a fixed
// admin token, so the container suite carries its own equivalents.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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

// 容器内固定事实：假模型的默认回复、知识库手册的路径与正文、查询短语。
// Fixed container facts: the default reply, the manual path/body and the query.
const (
	us18DefaultReply = "us18 容器回填答案"
	us18ManualPath   = "docs/us18-manual.md"
	us18ManualBody   = "# 运维手册\n\n## 备份\n\n备份保留 7 天，每天 02:00 执行全量备份。\n"
	us18ManualQuery  = "备份保留"
)

// us18ModelStep 是一步剧本：延迟后按给定状态码与正文响应。
// us18ModelStep delays then replies with the given status and body.
type us18ModelStep struct {
	Delay    time.Duration
	Response testutil.FakeOpenAIResponse
}

// us18Model 是绑在 Docker 桥网关上的 OpenAI 兼容假服务：容器经
// host.docker.internal 访问它，宿主进程因此必须监听网关地址而不是回环。
// us18Model is the OpenAI-compatible fake bound on the Docker bridge gateway, which
// is the narrowest address a container can reach through host.docker.internal.
type us18Model struct {
	mu       sync.Mutex
	steps    []us18ModelStep
	requests int
	baseURL  string
	server   *http.Server
}

// newUS18Model 启动假模型并登记关停；剧本耗尽后回默认文本。
// newUS18Model starts the fake and registers shutdown; the script falls back to the
// default text reply once exhausted.
func newUS18Model(t *testing.T, steps ...us18ModelStep) *us18Model {
	t.Helper()
	gateway := us18BridgeGateway(t)
	listener, err := net.Listen("tcp", net.JoinHostPort(gateway, "0"))
	if err != nil {
		t.Fatalf("在桥网关 %s 上监听失败: %v", gateway, err)
	}
	model := &us18Model{
		steps:   steps,
		baseURL: "http://" + listener.Addr().String() + "/v1",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", model.handleChat)
	mux.HandleFunc("/v1/embeddings", model.handleEmbeddings)
	model.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := model.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			t.Logf("US-18 假模型停止：%v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := model.server.Shutdown(ctx); err != nil {
			t.Logf("US-18 假模型关停失败：%v", err)
		}
	})
	return model
}

// URL 是容器侧可用的 base URL（含 `/v1`）。
// URL is the container-reachable base URL, including `/v1`.
func (m *us18Model) URL() string {
	return m.baseURL
}

// calls 返回已收到的聊天请求数（判「重启不重跑」用）。
// calls returns the number of chat requests received.
func (m *us18Model) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests
}

// nextStep 取下一步剧本；空剧本走默认文本。
// nextStep pops the next scripted step, or the default text reply.
func (m *us18Model) nextStep() us18ModelStep {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.steps) == 0 {
		return us18ModelStep{Response: textResponse(us18DefaultReply)}
	}
	step := m.steps[0]
	m.steps = m.steps[1:]
	return step
}

// handleChat 记录请求、按剧本延迟与状态码响应。
// handleChat records the request and replays one scripted step.
func (m *us18Model) handleChat(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.requests++
	m.mu.Unlock()
	step := m.nextStep()
	if step.Delay > 0 {
		select {
		case <-time.After(step.Delay):
		case <-r.Context().Done():
			return
		}
	}
	status := step.Response.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write([]byte(step.Response.Body)); err != nil {
		return
	}
}

// handleEmbeddings 按 OpenAI embeddings 协议返回确定性向量：输入可以是单个字符串
// 或字符串数组（摄取与检索两条路径都走这里）。
// handleEmbeddings answers the embeddings protocol for both a single string and a
// list of strings.
func (m *us18Model) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid embeddings payload", http.StatusBadRequest)
		return
	}
	texts, err := us18EmbeddingInputs(payload.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data := make([]any, 0, len(texts))
	for index, text := range texts {
		data = append(data, map[string]any{
			"object":    "embedding",
			"index":     index,
			"embedding": us16Vector(text),
		})
	}
	body, err := json.Marshal(map[string]any{
		"object": "list",
		"model":  payload.Model,
		"data":   data,
		"usage":  map[string]int{"prompt_tokens": 1, "total_tokens": 1},
	})
	if err != nil {
		http.Error(w, "encode embeddings response failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		return
	}
}

// us18EmbeddingInputs 解析 embeddings 的 `input`：字符串或字符串数组。
// us18EmbeddingInputs accepts either a string or an array of strings.
func us18EmbeddingInputs(raw json.RawMessage) ([]string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return []string{single}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("embeddings input 既不是字符串也不是字符串数组")
	}
	return many, nil
}

// us18BridgeGateway 读 Docker 默认桥的网关地址：容器经 host-gateway 映射访问宿主，
// 该地址是测试进程唯一需要监听的地址（不暴露局域网）。
// us18BridgeGateway returns the default bridge gateway, the only host address the
// fixture listens on so the container can reach it without LAN exposure.
func us18BridgeGateway(t *testing.T) string {
	t.Helper()
	us18RequireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := us18DockerOutput(ctx, "network", "inspect", "bridge",
		"--format", "{{(index .IPAM.Config 0).Gateway}}")
	if err != nil {
		t.Fatalf("读取 Docker 桥网关失败: %v", err)
	}
	gateway := strings.TrimSpace(out)
	if net.ParseIP(gateway) == nil {
		t.Fatalf("Docker 桥网关不是 IP：%q", gateway)
	}
	return gateway
}

// us18TemplateConfig 返回容器镜像内置的配置模板（不追加任何测试段）。
// us18TemplateConfig returns the image's built-in config template.
func us18TemplateConfig(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(us18RepoRoot(t), "docker", "basalt.yaml"))
	if err != nil {
		t.Fatalf("读取容器配置模板失败: %v", err)
	}
	return string(body)
}

// us18ProviderBlock 渲染一条 env 模式的 Provider 定义；凭证占位符由测试 overlay
// 注入（`BASALT_US18_FAKE_KEY`），配置里不出现明文。
// us18ProviderBlock renders one env-mode provider; the placeholder is filled by the
// test overlay, never by a plaintext value in the config.
func us18ProviderBlock(name, baseURL, model string) string {
	return "  - name: " + name + "\n" +
		"    base_url: " + baseURL + "\n" +
		"    api_key: ${BASALT_US18_FAKE_KEY}\n" +
		"    default_model: " + model + "\n"
}

// us18Config 组装完整容器配置：内置模板 + 两个 Provider + embedding 段 + 追加段。
// us18Config assembles the full container config: template, two providers, the
// embedding section and the caller-supplied extra block.
func us18Config(t *testing.T, modelBaseURL, extra string) string {
	t.Helper()
	out := us18TemplateConfig(t) + "\n" +
		"providers:\n" +
		us18ProviderBlock(fixtureProvider, modelBaseURL, fixtureModel) +
		us18ProviderBlock(us16EmbedProvider, modelBaseURL, us16EmbedModel) +
		"embedding:\n" +
		"  model_connection: " + us16EmbedProvider + "\n" +
		"  model: " + us16EmbedModel + "\n" +
		"  dimensions: " + strconv.Itoa(knowledge.VectorDimensions) + "\n"
	if extra != "" {
		out += extra
	}
	return out
}

// us18SeedConfig 先创建容器（不启动），再把配置写进工作区卷：compose 的相对路径
// （./secrets、./initdb）都落在暂存目录，卷名按 compose 标签发现。
// us18SeedConfig creates the containers without starting them, then writes the config
// into the workspace volume.
func us18SeedConfig(t *testing.T, s *composeStack, content string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// `compose create` 本身不启动容器，**没有** `--no-start` 选项（那是 `up` 的旗标；
	// Compose v5.6.1 实测 `create --no-start` → unknown flag）。
	// `compose create` never starts containers and has no `--no-start` flag; that flag
	// belongs to `up` (Compose v5.6.1 rejects it here).
	if out, err := s.compose(ctx, "create"); err != nil {
		t.Fatalf("compose create 失败: %v（%s）", err, us18Tail(out, 2000))
	}
	us18WriteWorkspaceFile(t, s, ctx, "basalt.yaml", []byte(content), "0644")
}

// us18WriteWorkspaceFile 用一次性 root 容器把文件写进工作区卷（容器内路径
// `/data/<rel>`），写完把整个卷的属主交回 10001——应用以非 root 运行，探针要能写
// 工作区根目录。
// us18WriteWorkspaceFile writes one file into the workspace volume through a one-shot
// root container and hands the whole volume back to UID 10001.
func us18WriteWorkspaceFile(t *testing.T, s *composeStack, ctx context.Context, rel string, content []byte, mode string) {
	t.Helper()
	volume := us18VolumeName(t, s, ctx, "workspace")
	encoded := base64.StdEncoding.EncodeToString(content)
	script := "set -eu\n" +
		"mkdir -p \"$(dirname \"$1\")\"\n" +
		"printf %s \"$2\" | base64 -d > \"$1\"\n" +
		"chmod \"$3\" \"$1\"\n" +
		"chown -R 10001:10001 /volume\n"
	target := "/volume/" + strings.TrimPrefix(rel, "/")
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", volume+":/volume",
		"--entrypoint", "sh", s.env["BASALT_IMAGE"], "-c", script, "sh", target, encoded, mode)
	if err != nil {
		t.Fatalf("写入工作区文件 %s 失败: %v（%s）", rel, err, strings.TrimSpace(out))
	}
}

// us18SeedWorkspace 把知识库手册写进工作区根（`.basalt`）下的约定路径：摄取路径
// 相对工作区解析，不是相对进程工作目录。
// us18SeedWorkspace writes the manual under the workspace root, which is what ingest
// paths are relative to.
func us18SeedWorkspace(t *testing.T, s *composeStack) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	us18WriteWorkspaceFile(t, s, ctx, filepath.Join(".basalt", us18ManualPath),
		[]byte(us18ManualBody), "0644")
}

// us18VolumeName 按 compose 标签取逻辑卷的真实名字。
// us18VolumeName resolves a logical volume's real name through compose labels.
func us18VolumeName(t *testing.T, s *composeStack, ctx context.Context, logical string) string {
	t.Helper()
	out, err := us18DockerOutput(ctx, "volume", "ls", "-q",
		"--filter", "label=com.docker.compose.project="+s.project,
		"--filter", "label=com.docker.compose.volume="+logical)
	if err != nil {
		t.Fatalf("列卷失败: %v", err)
	}
	names := us18NonEmptyLines(out)
	if len(names) == 0 {
		t.Fatalf("项目 %s 找不到 %s 卷（compose create 是否执行过？）", s.project, logical)
	}
	return names[0]
}

// us18Envelope 是管理 / 业务面的响应信封（判卷只看 code / message / data）。
// us18Envelope is the response envelope the verdict reads.
type us18Envelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// us18Do 发一次容器请求并解析信封；token 为空表示匿名请求。
// us18Do issues one request and decodes the envelope; an empty token is anonymous.
func us18Do(t *testing.T, s *composeStack, method, path string, payload any, token string, headers map[string]string) (int, us18Envelope, []byte) {
	t.Helper()
	var body []byte
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("%s %s 编码请求体失败: %v", method, path, err)
		}
		body = raw
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	resp, raw, err := s.requestHeaders(ctx, method, path, body, token, headers)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	var envelope us18Envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("%s %s 响应不是信封 JSON（HTTP %d）：%v（%s）",
				method, path, resp.StatusCode, err, us18Tail(string(raw), 600))
		}
	}
	return resp.StatusCode, envelope, raw
}

// us18AdminDo 用实例私有卷里的管理令牌发一次管理请求。
// us18AdminDo issues one admin request with the token from the private volume.
func us18AdminDo(t *testing.T, s *composeStack, method, path string, payload any, headers map[string]string) (int, us18Envelope, []byte) {
	t.Helper()
	return us18Do(t, s, method, path, payload, us18Token(t, s), headers)
}

// us18Token 读并缓存实例管理令牌：令牌首启自动生成，只存在于实例私有卷。
// us18Token reads and caches the first-boot admin token from the private volume.
func us18Token(t *testing.T, s *composeStack) string {
	t.Helper()
	if s.token != "" {
		return s.token
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	token, err := s.adminToken(ctx)
	if err != nil {
		t.Fatalf("读取管理面令牌失败: %v", err)
	}
	s.token = token
	return token
}

// us18RequireOK 断言 200 + OK 信封。
// us18RequireOK asserts a 200 OK envelope.
func us18RequireOK(t *testing.T, status int, envelope us18Envelope, raw []byte, label string) {
	t.Helper()
	if status != http.StatusOK || envelope.Code != "OK" {
		t.Fatalf("%s：HTTP %d code=%q message=%q（%s）",
			label, status, envelope.Code, envelope.Message, us18Tail(string(raw), 600))
	}
}

// us18DecodeData 把信封的 data 解到 dst。
// us18DecodeData decodes the envelope data into dst.
func us18DecodeData(t *testing.T, envelope us18Envelope, dst any, label string) {
	t.Helper()
	if len(envelope.Data) == 0 {
		t.Fatalf("%s：响应缺少 data", label)
	}
	if err := json.Unmarshal(envelope.Data, dst); err != nil {
		t.Fatalf("%s：解析 data 失败: %v（%s）", label, err, us18Tail(string(envelope.Data), 600))
	}
}

// us18WaitHealth 轮询健康检查直到 200 且载荷为固定存活文案。
// us18WaitHealth polls the health probe until it reports the fixed alive payload.
func us18WaitHealth(t *testing.T, s *composeStack, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, raw, err := s.request(ctx, http.MethodGet, "/api/v1/health", nil, "")
		cancel()
		if err == nil && resp.StatusCode == http.StatusOK &&
			strings.Contains(string(raw), `"status":"ok"`) {
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("HTTP %d：%s", resp.StatusCode, us18Tail(string(raw), 200))
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待健康检查超时（%s）：%s", timeout, last)
}

// us18InfoVersion 读进程信息面的版本号（升级 / 回退判据）。
// us18InfoVersion reads the process version from the info face.
func us18InfoVersion(t *testing.T, s *composeStack) string {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodGet, "/api/v1/info", nil, nil)
	us18RequireOK(t, status, envelope, raw, "读取进程信息")
	var info struct {
		Version string `json:"version"`
	}
	us18DecodeData(t, envelope, &info, "读取进程信息")
	if info.Version == "" {
		t.Fatalf("进程信息版本为空（%s）", raw)
	}
	return info.Version
}

// us18FilesView 是 Agent 文件集合视图的判卷子集。
// us18FilesView is the verdict-side subset of the agent file view.
type us18FilesView struct {
	Agent     string        `json:"agent"`
	ETag      string        `json:"etag"`
	Lifecycle us18Lifecycle `json:"lifecycle"`
}

// us18Lifecycle 是生命周期读数。
// us18Lifecycle is the lifecycle reading.
type us18Lifecycle struct {
	State      *string `json:"state"`
	Generation int64   `json:"generation"`
}

// us18CreateAgent 经管理面创建 Agent（默认停用、generation=1）。
// us18CreateAgent creates an agent, disabled at generation 1.
func us18CreateAgent(t *testing.T, s *composeStack, name, description string) us18FilesView {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":        name,
		"description": description,
		"model":       fixtureModel,
	}, nil)
	us18RequireOK(t, status, envelope, raw, "创建 Agent "+name)
	var view us18FilesView
	us18DecodeData(t, envelope, &view, "创建 Agent "+name)
	if view.ETag == "" {
		t.Fatalf("创建 Agent %s：etag 为空", name)
	}
	return view
}

// us18ModelConnectionUID 读模型连接发布指针的资源身份：绑定必须携带 UID，按名
// 解析被禁止。
// us18ModelConnectionUID reads the published pointer's resource uid.
func us18ModelConnectionUID(t *testing.T, s *composeStack, name string) string {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodGet,
		"/api/v1/model-connections/"+name, nil, nil)
	us18RequireOK(t, status, envelope, raw, "读取模型连接 "+name)
	var view struct {
		ResourceUID string `json:"resource_uid"`
	}
	us18DecodeData(t, envelope, &view, "读取模型连接 "+name)
	if view.ResourceUID == "" {
		t.Fatalf("模型连接 %s 的 resource_uid 为空", name)
	}
	return view.ResourceUID
}

// us18SubmitAgentDoc 提交 AGENT.md（条件写，If-Match 取创建响应的 ETag）。
// us18SubmitAgentDoc submits AGENT.md with the create response's ETag.
func us18SubmitAgentDoc(t *testing.T, s *composeStack, name, etag, doc string) {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodPost,
		"/api/v1/agents/"+name+"/files",
		map[string]any{"files": []map[string]string{{"path": "AGENT.md", "content": doc}}},
		map[string]string{"If-Match": etag})
	us18RequireOK(t, status, envelope, raw, "提交 Agent "+name+" 的 AGENT.md")
}

// us18WaitRegistered 轮询注册表直到该 Agent 已注册（创建后的监听器去抖窗口）。
// us18WaitRegistered polls until the registry lists the agent as registered.
func us18WaitRegistered(t *testing.T, s *composeStack, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		status, envelope, raw := us18AdminDo(t, s, http.MethodGet, "/api/v1/profiles", nil, nil)
		if status == http.StatusOK && envelope.Code == "OK" {
			var rows []struct {
				Name       string `json:"name"`
				Registered bool   `json:"registered"`
			}
			if err := json.Unmarshal(envelope.Data, &rows); err == nil {
				for _, row := range rows {
					if row.Name == name && row.Registered {
						return
					}
				}
			}
		}
		last = fmt.Sprintf("HTTP %d：%s", status, us18Tail(string(raw), 300))
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待 Agent %s 注册超时：%s", name, last)
}

// us18AgentGeneration 从 `/profiles` 读生命周期代数：文件集合视图的 lifecycle 只
// 投影 state / valid / lifecycle_error，generation 按零值输出（views.go 的
// agentFilesView 明确不编造），不能当作乐观并发的期望值；profiles 才是状态文件的
// 权威投影。
// us18AgentGeneration reads the authoritative lifecycle generation from /profiles;
// the files view projects generation as zero by design and must not be used here.
func us18AgentGeneration(t *testing.T, s *composeStack, name string) int64 {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodGet, "/api/v1/profiles", nil, nil)
	us18RequireOK(t, status, envelope, raw, "读取 Agent 生命周期代数")
	var rows []struct {
		Name      string `json:"name"`
		Lifecycle struct {
			Generation int64 `json:"generation"`
		} `json:"lifecycle"`
	}
	us18DecodeData(t, envelope, &rows, "读取 Agent 生命周期代数")
	for _, row := range rows {
		if row.Name == name {
			return row.Lifecycle.Generation
		}
	}
	t.Fatalf("profiles 中未找到 Agent %s", name)
	return 0
}

// us18EnableAgent 轮询启用：404（装载器还没看见）与 409 AGENT_UNAVAILABLE（引用
// 资源未就绪）都是创建后的正常中间态，其它响应立即判红。
// us18EnableAgent retries enable across the post-create window; any other response
// fails immediately.
func us18EnableAgent(t *testing.T, s *composeStack, name string, generation int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	// 热加载（fsnotify）可能在「读文件集合代数」与「提交启用」之间推进 generation：
	// 服务端按乐观并发返回 409 CONFLICT，客户端按契约重读代数后重试，断言不放宽。
	// The hot reload can bump the generation between reading it and enabling; the
	// server answers 409 CONFLICT, so the client re-reads and retries.
	want := generation
	last := ""
	for time.Now().Before(deadline) {
		status, envelope, raw := us18AdminDo(t, s, http.MethodPost,
			"/api/v1/agents/"+name+"/enable", map[string]any{
				"expected_state":      "disabled",
				"expected_generation": want,
			}, nil)
		last = fmt.Sprintf("HTTP %d（expected_generation=%d）：%s", status, want, us18Tail(string(raw), 400))
		switch {
		case status == http.StatusOK:
			return
		case status == http.StatusNotFound:
		case status == http.StatusConflict && envelope.Code == "AGENT_UNAVAILABLE":
		case status == http.StatusConflict && envelope.Code == "CONFLICT":
			want = us18AgentGeneration(t, s, name)
		default:
			t.Fatalf("启用 Agent %s 失败：%s", name, last)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待启用 Agent %s 超时：%s", name, last)
}

// us18BuildAgent 走完整创建链路：创建 → 带上绑定提交 AGENT.md → 等注册 → 启用。
// 绑定 UID 取自已发布的模型连接指针，服务端不按名补绑；启用代数取提交后的
// profiles 投影（不假设提交与热加载不会推进生命周期）。
// us18BuildAgent runs create → submit with bindings → wait-registered → enable.
func us18BuildAgent(t *testing.T, s *composeStack, name, description string, tools []string) {
	t.Helper()
	created := us18CreateAgent(t, s, name, description)
	uid := us18ModelConnectionUID(t, s, fixtureProvider)
	doc := agentDoc(agentFrontmatter(name, tools)+
		bindingsBlock("model_connection", fixtureProvider, uid), description)
	us18SubmitAgentDoc(t, s, name, created.ETag, doc)
	us18WaitRegistered(t, s, name)
	us18EnableAgent(t, s, name, us18AgentGeneration(t, s, name))
}

// us18AgentFiles 读 Agent 文件集合视图（判启用代数与 ETag）。
// us18AgentFiles reads the agent file view.
func us18AgentFiles(t *testing.T, s *composeStack, name string) us18FilesView {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodGet,
		"/api/v1/agents/"+name+"/files", nil, nil)
	us18RequireOK(t, status, envelope, raw, "读取 Agent "+name+" 文件集合")
	var view us18FilesView
	us18DecodeData(t, envelope, &view, "读取 Agent "+name+" 文件集合")
	if view.ETag == "" {
		t.Fatalf("读取 Agent %s 文件集合：etag 为空", name)
	}
	return view
}

// us18RunDetail 读一次运行的详情视图：终态与回复必须来自账本，而不是只有调用响应。
// us18RunDetail reads the run detail so the terminal state comes from the ledger.
func us18RunDetail(t *testing.T, s *composeStack, runID string) (string, string) {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodGet, "/api/v1/runs/"+runID, nil, nil)
	us18RequireOK(t, status, envelope, raw, "读取运行详情")
	var view struct {
		Status    string `json:"status"`
		FinalText string `json:"final_text"`
	}
	us18DecodeData(t, envelope, &view, "读取运行详情")
	return view.Status, view.FinalText
}

// us18InvokeReading 是无状态调用响应的判卷读数。
// us18InvokeReading is the verdict-side stateless invoke reading.
type us18InvokeReading struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	FinalText string `json:"final_text"`
}

// us18Invoke 发一次无状态调用并要求 completed 终态。
// us18Invoke performs one stateless invoke and requires a completed run.
func us18Invoke(t *testing.T, s *composeStack, name, message string) us18InvokeReading {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodPost,
		"/api/v1/agents/"+name+"/invoke", map[string]string{"message": message}, nil)
	us18RequireOK(t, status, envelope, raw, "调用 Agent "+name)
	var reading us18InvokeReading
	us18DecodeData(t, envelope, &reading, "调用 Agent "+name)
	if reading.RunID == "" {
		t.Fatalf("调用 Agent %s：run_id 为空（%s）", name, raw)
	}
	if reading.Status != "completed" {
		t.Fatalf("调用 Agent %s：status = %q、reason = %q，期望 completed",
			name, reading.Status, reading.Reason)
	}
	if strings.TrimSpace(reading.FinalText) == "" {
		t.Fatalf("调用 Agent %s：final_text 为空", name)
	}
	return reading
}

// us18CreateSession 建一个会话并返回 session_id：A6 要从账本读出非空最终回答，
// 而无状态 invoke 按设计不落消息（final_text 由该 Run 最近一条 assistant 消息派生），
// 只有会话运行才会写出 assistant 消息。
// us18CreateSession returns a session id. A6's non-empty ledger reply requires a
// session run: stateless invokes persist no messages, and final_text is derived from
// the run's latest assistant message.
func us18CreateSession(t *testing.T, s *composeStack, agent string) string {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodPost, "/api/v1/sessions",
		map[string]string{"agent": agent}, nil)
	us18RequireOK(t, status, envelope, raw, "创建会话")
	var view struct {
		SessionID string `json:"session_id"`
	}
	us18DecodeData(t, envelope, &view, "创建会话")
	if view.SessionID == "" {
		t.Fatalf("创建会话：session_id 为空（%s）", us18Tail(string(raw), 300))
	}
	return view.SessionID
}

// us18PostMessage 在会话里同步跑一次并返回运行读数（run_id / status / final_text）。
// us18PostMessage runs one synchronous turn in the session and returns the run reading.
func us18PostMessage(t *testing.T, s *composeStack, sessionID, message string) us18InvokeReading {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodPost,
		"/api/v1/sessions/"+sessionID+"/messages", map[string]string{"message": message}, nil)
	us18RequireOK(t, status, envelope, raw, "会话发消息")
	var reading us18InvokeReading
	us18DecodeData(t, envelope, &reading, "会话发消息")
	if reading.RunID == "" {
		t.Fatalf("会话发消息：run_id 为空（%s）", us18Tail(string(raw), 300))
	}
	return reading
}

// us18IssueCallerKey 经管理面建调用方并签发业务 scope 的 Key。
// us18IssueCallerKey creates a caller and issues a business-scoped key.
func us18IssueCallerKey(t *testing.T, s *composeStack, name string, agents ...string) string {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodPost, "/api/v1/callers", map[string]any{
		"name": name,
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
	}, nil)
	us18RequireOK(t, status, envelope, raw, "建调用方")
	var caller struct {
		CallerID string `json:"caller_id"`
	}
	us18DecodeData(t, envelope, &caller, "建调用方")
	if caller.CallerID == "" {
		t.Fatalf("建调用方：caller_id 为空（%s）", raw)
	}
	scopes := []string{"sessions:read", "sessions:write", "knowledge:read", "knowledge:write"}
	for _, agent := range agents {
		scopes = append(scopes, "agents:invoke:"+agent)
	}
	status, envelope, raw = us18AdminDo(t, s, http.MethodPost,
		"/api/v1/callers/"+caller.CallerID+"/keys", map[string]any{
			"scopes":     scopes,
			"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		}, nil)
	us18RequireOK(t, status, envelope, raw, "签发调用方 Key")
	var issued struct {
		Key string `json:"key"`
	}
	us18DecodeData(t, envelope, &issued, "签发调用方 Key")
	if issued.Key == "" {
		t.Fatalf("签发调用方 Key：明文为空（%s）", raw)
	}
	return issued.Key
}

// us18CreateCollection 建知识集合。
// us18CreateCollection creates one knowledge collection.
func us18CreateCollection(t *testing.T, s *composeStack, key, name string) string {
	t.Helper()
	status, envelope, raw := us18Do(t, s, http.MethodPost, "/api/v1/collections",
		map[string]any{"name": name}, key, nil)
	us18RequireOK(t, status, envelope, raw, "建集合")
	var collection struct {
		CollectionID string `json:"collection_id"`
	}
	us18DecodeData(t, envelope, &collection, "建集合")
	if collection.CollectionID == "" {
		t.Fatalf("建集合：collection_id 为空（%s）", raw)
	}
	return collection.CollectionID
}

// us18IngestReading 是摄取结果的判卷读数。
// us18IngestReading is the verdict-side ingest reading.
type us18IngestReading struct {
	DocumentID string `json:"document_id"`
	VersionID  string `json:"version_id"`
	Status     string `json:"status"`
	ChunkCount int    `json:"chunk_count"`
	ErrorKind  string `json:"error_kind"`
}

// us18Ingest 摄取工作区内的手册并拒绝 failed 终态。
// us18Ingest ingests the manual and rejects a failed terminal state.
func us18Ingest(t *testing.T, s *composeStack, key, collectionID, path string) us18IngestReading {
	t.Helper()
	status, envelope, raw := us18Do(t, s, http.MethodPost,
		"/api/v1/collections/"+collectionID+"/documents", map[string]any{"path": path}, key, nil)
	us18RequireOK(t, status, envelope, raw, "摄取文档")
	var reading us18IngestReading
	us18DecodeData(t, envelope, &reading, "摄取文档")
	if reading.Status == "failed" {
		t.Fatalf("摄取终态为 failed（error_kind=%q）", reading.ErrorKind)
	}
	if reading.Status != "active" {
		t.Fatalf("摄取终态 = %q，期望 active", reading.Status)
	}
	if reading.ChunkCount < 1 {
		t.Fatalf("摄取 chunk_count = %d，期望 ≥ 1", reading.ChunkCount)
	}
	return reading
}

// us18SearchTop 检索并返回首条命中与代际：零命中或向量路降级都判红。
// us18SearchTop requires a hit and a vector-path generation ≥ 1.
func us18SearchTop(t *testing.T, s *composeStack, key, collectionID, query string) (us18SearchHit, int) {
	t.Helper()
	status, envelope, raw := us18Do(t, s, http.MethodPost, "/api/v1/search", map[string]any{
		"query":       query,
		"collections": []string{collectionID},
	}, key, nil)
	us18RequireOK(t, status, envelope, raw, "检索")
	var reading struct {
		Hits       []us18SearchHit `json:"hits"`
		Generation int             `json:"generation"`
	}
	us18DecodeData(t, envelope, &reading, "检索")
	if len(reading.Hits) == 0 {
		t.Fatalf("检索零命中（generation=%d）", reading.Generation)
	}
	if reading.Generation < 1 {
		t.Fatalf("检索 generation = %d，期望 ≥ 1（向量路未生效）", reading.Generation)
	}
	return reading.Hits[0], reading.Generation
}

// us18SearchHit 是一条检索命中。
// us18SearchHit is one search hit.
type us18SearchHit struct {
	DocumentID string `json:"document_id"`
	VersionID  string `json:"version_id"`
	Snippet    string `json:"snippet"`
}

// us18CreateManagedConnection 创建托管凭据模型连接：明文密钥只进请求体，响应只回
// 模式 / 状态 / 尾号。
// us18CreateManagedConnection creates a managed credential connection; the plaintext
// key only travels in the request body.
func us18CreateManagedConnection(t *testing.T, s *composeStack, name, baseURL, apiKey string) {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodPost, "/api/v1/model-connections",
		map[string]any{
			"name":            name,
			"base_url":        baseURL,
			"api_key":         apiKey,
			"default_model":   fixtureModel,
			"credential_mode": "managed",
		}, nil)
	us18RequireOK(t, status, envelope, raw, "创建托管模型连接")
	if strings.Contains(string(raw), apiKey) {
		t.Fatalf("创建托管模型连接：响应回显了凭据明文")
	}
	var view struct {
		CredentialMode  string `json:"credential_mode"`
		CredentialState string `json:"credential_state"`
	}
	us18DecodeData(t, envelope, &view, "创建托管模型连接")
	if view.CredentialMode != "managed" || view.CredentialState != "active" {
		t.Fatalf("创建托管模型连接：credential_mode = %q、credential_state = %q，期望 managed / active",
			view.CredentialMode, view.CredentialState)
	}
}

// us18TestConnection 发一次模型连接测试并返回脱敏结果；连接失败是 200 + ok=false，
// 超时 / 缺凭证按 504 / 503 报错——本助手把非 200 也当作结论返回。
// us18TestConnection runs the model connection test; a failed call is 200 + ok=false,
// while timeout and missing credentials surface as 504/503.
func us18TestConnection(t *testing.T, s *composeStack, name string) (bool, string) {
	t.Helper()
	status, envelope, raw := us18AdminDo(t, s, http.MethodPost,
		"/api/v1/model-connections/"+name+"/test", nil, nil)
	if status != http.StatusOK {
		return false, fmt.Sprintf("HTTP %d code=%q message=%q", status, envelope.Code, envelope.Message)
	}
	us18RequireOK(t, status, envelope, raw, "测试模型连接")
	var view struct {
		OK        bool   `json:"ok"`
		ErrorCode string `json:"error_code"`
	}
	us18DecodeData(t, envelope, &view, "测试模型连接")
	return view.OK, view.ErrorCode
}

// us18Query 在 postgres 容器里执行一条只读查询（-tA 单列读数）。
// us18Query runs one read query in the postgres container.
func us18Query(ctx context.Context, s *composeStack, query string) (string, error) {
	out, err := s.exec(ctx, "postgres", "sh", "-c",
		`psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc "$1"`, "sh", query)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// us18PSQL 是 us18Query 的判红版本。
// us18PSQL is the failing variant of us18Query.
func us18PSQL(t *testing.T, s *composeStack, query string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := us18Query(ctx, s, query)
	if err != nil {
		t.Fatalf("查询数据库失败（%s）：%v", query, err)
	}
	return out
}

// us18RunRow 读一次运行的终态与关闭原因。
// us18RunRow reads one run's terminal status and reason.
func us18RunRow(t *testing.T, s *composeStack, runID string) (string, string) {
	t.Helper()
	row := us18PSQL(t, s, fmt.Sprintf(
		"SELECT status || '|' || COALESCE(reason, '') FROM runs WHERE run_id = '%s'", runID))
	parts := strings.SplitN(row, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("运行 %s 的读数不可解析：%q", runID, row)
	}
	return parts[0], parts[1]
}

// us18ToolStatuses 读一次运行的工具调用状态（按 id 排序、逗号连接）。
// us18ToolStatuses reads one run's tool-call statuses in id order.
func us18ToolStatuses(t *testing.T, s *composeStack, runID string) string {
	t.Helper()
	return us18PSQL(t, s, fmt.Sprintf(
		"SELECT string_agg(status, ',' ORDER BY id) FROM tool_invocations WHERE run_id = '%s'", runID))
}

// us18WaitDB 轮询数据库读数直到条件成立（崩溃 / 恢复的中间态判定）。
// us18WaitDB polls a database reading until the condition holds.
func us18WaitDB(t *testing.T, s *composeStack, query string, ready func(string) bool, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := us18Query(ctx, s, query)
		cancel()
		if err != nil {
			last = err.Error()
		} else {
			last = out
			if ready(out) {
				return out
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("等待数据库读数超时（%s）：最后一次 %q", timeout, last)
	return ""
}

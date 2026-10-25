//go:build e2e

package e2e

// 本文件是 US-14 五 Demo 的进程级验收（模块卡 §2.4.5 / §4.3 的 demo-fixtures 与
// legacy-d1）：把仓库自带的示例目录原样装进临时工作区，用真实二进制跑调度触发与人工
// 补跑，再读三类证据——脚本产物、SQLite 库、通知接收端——判「业务结果可核对」。
//
// 判据来源（模块卡 R3）：
//   - §2.4.5：每个 Demo 的必须项 / 业务断言 / 触发与通知 / 不算完成的形态；
//   - §2.4.6：A 档（可复现）/ A*（真实外部服务，本夹具不覆盖外发）；
//   - §2.5.1：R0 旧 D1 的三端点（200 / 500 / 不可达）+ 一次通知汇总，**不调度**，
//     不计入五 Demo。
//
// 外部服务边界：真实 GitHub / Brave / Fetch / DBHub / Playwright 与本机之外的接收方
// **全部未授权**。夹具因此只用三类替身，且都在同一条进程面之后：MCP 规格 helper
// （`testutil.RunMCPHelper` 的规格文件模式，每次 `tools/call` 重读）、自建的回环 80
// 端口接收端（出网策略只放行 scheme 默认端口），以及真实构建的 `docs-readonly`
// （D4 的自研只读 server，不是替身）。替身替换的是**外部服务**，脚本、AGENT.md、
// 技能、MCP 定义与调度链路都是仓库里的真货。
// The real external services are not authorized for this machine, so the fixtures stand in
// for them only; the scripts, definitions, skills and scheduling path are the shipped ones.

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/testutil"
)

const (
	// demoProvider 与 demoModelName 替换示例 frontmatter 里的两个字面量：provider 名
	// 保持示例原值（配置里必须有同名条目），模型名由占位改成夹具的模型。
	// demoProvider keeps the shipped name; demoModelName replaces the placeholder.
	demoProvider  = "local-model"
	demoModelName = "demo-model"

	// 通知接收端的固定三元组：渠道行里只存 `${VAR}` 引用，值由 serve 进程环境注入。
	// The receiver triple: the channel row only carries a ${VAR} reference.
	demoNotifyName   = "ops"
	demoNotifyEnvRef = "BASALT_NOTIFY_OPS"
	demoNotifyURL    = "http://localhost/ops"

	// 出网策略只放行 scheme 默认端口（80 / 443），因此接收端监听回环 80；
	// `/ops` 收通知投递，`/ok` 与 `/boom` 是 R0 旧 D1 的 200 / 500 两端点。
	// Outbound policy admits the scheme default port only, so the receiver binds :80.
	demoReceiverAddr = "127.0.0.1:80"
	demoReplyPath    = "/ops"
	demoOKPath       = "/ok"
	demoBoomPath     = "/boom"

	// demoUnreachableURL 用 `.invalid` 保留 TLD（RFC 2606）：永远解析不到，失败确定。
	// `.invalid` never resolves, so the DNS failure path is deterministic.
	demoUnreachableURL = "http://demo-unreachable.invalid/feed"

	// 四个 MCP 规格文件的变量名：`${VAR}` 占位进 mcp_servers.yaml，值由夹具注入。
	// The four spec-file variables referenced from mcp_servers.yaml.
	demoGitHubSpecEnv = "BASALT_E2E_GITHUB_SPEC"
	demoDBHubSpecEnv  = "BASALT_E2E_DBHUBS_SPEC"
	demoBraveSpecEnv  = "BASALT_E2E_BRAVE_SPEC"
	demoFetchSpecEnv  = "BASALT_E2E_FETCH_SPEC"

	// demoDeliveryTimeout 等系统代投离开 pending 的上限：发送自带 15s ctx，
	// 再留一倍余量给落库。超限即判失败——pending 不退说明发送这一轮没有收尾，
	// 不是「再等久一点就好」。
	// demoDeliveryTimeout bounds the wait for a delivery row to leave pending.
	demoDeliveryTimeout = 30 * time.Second
)

// demoPython 找出一个可用的 Python 3 解释器，返回**绝对路径**。
// demoPython resolves a usable Python 3 interpreter to an absolute path.
//
// 返回绝对路径而不是裸名，是被执行侧的查找语义逼出来的：`LocalRunner` 用
// `CreateProcess` 的 `lpApplicationName` 直执（不经 os/exec 的 LookPath），而
// Windows 对**部分名**只在当前目录里找、且不补 `.exe` 后缀——裸 `py` 必然失败于
// `The system cannot find the file specified`。绝对路径同时满足 shell 白名单的
// 整串相等比对（`allowed_commands` / `interpreters` 与调用串必须逐字一致）。
// An absolute path is required: the runner hands the name straight to
// CreateProcess(lpApplicationName), which resolves partial names only against the
// current directory and never appends `.exe`.
//
// 语序固定：`BASALT_DEMO_PYTHON` 覆盖 → `python` → `python3` → `py`。每次都用
// `-c` 实测一次版本号——本机 `python` 是 Windows Store 存根（退出码非 0），
// 只查 `LookPath` 会把它当成可用解释器。找不到就**判失败**（模块卡 §4.3：
// 缺前置 = 失败，不得跳过）。
// The probe runs the interpreter once: the Store stub on this machine resolves on PATH
// but exits non-zero, so a bare LookPath check would accept it.
func demoPython(t *testing.T) string {
	t.Helper()
	candidates := make([]string, 0, 4)
	if override := strings.TrimSpace(os.Getenv("BASALT_DEMO_PYTHON")); override != "" {
		candidates = append(candidates, override)
	}
	candidates = append(candidates, "python", "python3", "py")
	var tried []string
	for _, name := range candidates {
		path, err := exec.LookPath(name)
		if err != nil {
			tried = append(tried, name+"（不在 PATH 上）")
			continue
		}
		out, err := exec.Command(path, "-c", "import sys; print(sys.version_info[0])").Output()
		if err != nil {
			tried = append(tried, name+"（调用失败）")
			continue
		}
		if strings.TrimSpace(string(out)) != "3" {
			tried = append(tried, name+"（不是 Python 3）")
			continue
		}
		return path
	}
	t.Fatalf("未找到可用的 Python 3 解释器（候选：%s）——五 Demo 的脚本必须真跑；"+
		"设置 BASALT_DEMO_PYTHON 指向解释器后重跑", strings.Join(tried, "、"))
	return ""
}

// demoReceiver 是回环 80 端口上的接收端：通知投递与 R0 旧 D1 的两条 HTTP 端点共用它。
// demoReceiver serves the loopback port 80 receiver shared by notify and legacy D1.
type demoReceiver struct {
	mu     sync.Mutex
	hits   map[string]int
	bodies map[string][]string
}

// startDemoReceiver 绑定 127.0.0.1:80 并尽力再绑 [::1]:80（Windows 上 localhost 可能
// 先解析到 IPv6；Go 的拨号器两条都会试，绑不上也不影响判定）。
// The receiver binds 127.0.0.1:80 and best-effort mirrors the listener on [::1]:80.
func startDemoReceiver(t *testing.T) *demoReceiver {
	t.Helper()
	recv := &demoReceiver{hits: map[string]int{}, bodies: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", recv.handle)
	listener, err := net.Listen("tcp", demoReceiverAddr)
	if err != nil {
		t.Fatalf("回环 80 端口不可用：%v——五 Demo 的出网请求必须命中 scheme 默认端口，"+
			"本用例需要该端口空闲", err)
	}
	serve := func(ln net.Listener) {
		server := &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		t.Cleanup(func() { _ = server.Close() })
		go func() {
			if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
				t.Logf("接收端 %s 退出：%v", ln.Addr(), err)
			}
		}()
	}
	serve(listener)
	if v6, err := net.Listen("tcp", "[::1]:80"); err == nil {
		serve(v6)
	}
	return recv
}

func (r *demoReceiver) handle(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	r.hits[req.URL.Path]++
	r.bodies[req.URL.Path] = append(r.bodies[req.URL.Path], string(body))
	r.mu.Unlock()
	switch req.URL.Path {
	case demoReplyPath:
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	case demoOKPath:
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	case demoBoomPath:
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "not found")
	}
}

// count 返回某个路径累计命中的次数。
// count reports how many times one path was hit.
func (r *demoReceiver) count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[path]
}

// received 返回某个路径收到的全部请求体。
// received returns every body the given path received.
func (r *demoReceiver) received(path string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies[path]...)
}

// demoStep 是演示模型的一步剧本：读本轮到手的请求，给出下一条响应。
// demoStep is one scripted model turn: read the request, answer the next response.
type demoStep func(*demoTurn) testutil.FakeOpenAIResponse

// demoModel 是一台可编排的 OpenAI 兼容假服务：剧本按 Run 追加，按到达顺序消费。
// demoModel is a scriptable OpenAI-compatible fake: scripts are appended per run and
// consumed in arrival order.
//
// 复用边界（模块卡 §3.1.1 只要求「不重写假模型」的语义面）：`testutil.FakeOpenAIServer`
// 是固定脚本、用尽即失败；五 Demo 每个 Run 的剧本都要看上一轮的工具结果再决定下一轮，
// 因此这里只补「可编排」这一层，响应体仍由 `testutil.FakeOpenAIResponse` 承载。
// The transport shape stays testutil's; only the per-run scripting layer is added here.
type demoModel struct {
	t     *testing.T
	mu    sync.Mutex
	dir   string
	queue []demoStep
	calls int
	reqs  []map[string]any
	srv   *httptest.Server
}

// newDemoModel 起回环假模型（路径与既有夹具一致：POST /chat/completions）。
// newDemoModel starts the loopback fake model on the same path as the existing fixture.
func newDemoModel(t *testing.T) *demoModel {
	t.Helper()
	m := &demoModel{t: t}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

// URL 返回假模型地址（provider 的 base_url；路径由客户端拼接，不带 /v1）。
// URL returns the base address; the client appends the path itself.
func (m *demoModel) URL() string { return m.srv.URL }

// beginRun 声明下一轮剧本与它作用的 Agent 目录；上一个 Run 的剧本必须已被消费完
// （剩余步意味着剧本与工具面对不上，属于夹具缺陷，直接判红）。
// beginRun declares the next run's script; a leftover step means the fixture is wrong.
func (m *demoModel) beginRun(dir string, steps ...demoStep) {
	m.t.Helper()
	m.mu.Lock()
	left := len(m.queue)
	m.mu.Unlock()
	if left != 0 {
		m.t.Fatalf("上一个 Run 的模型剧本还剩 %d 步未消费——剧本比 Run 实际调用长", left)
	}
	m.mu.Lock()
	m.dir, m.queue = dir, steps
	m.mu.Unlock()
}

// scriptLeft 返回当前队列剩余步数（Run 结束后用它判「剧本刚好用完」）。
// scriptLeft reports the number of queued steps left.
func (m *demoModel) scriptLeft() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.queue)
}

// callCount 返回累计收到的模型请求数。
// callCount reports how many model requests arrived.
func (m *demoModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *demoModel) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
		m.t.Errorf("演示模型：只接受 POST /chat/completions，实际 %s %s", r.Method, r.URL.Path)
		writeFakeResponse(w, testutil.FakeOpenAIResponse{
			StatusCode: http.StatusBadRequest,
			Body:       `{"error":{"message":"unexpected request"}}`,
		})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		m.t.Errorf("演示模型：读请求体失败: %v", err)
		writeFakeResponse(w, testutil.FakeOpenAIResponse{
			StatusCode: http.StatusBadRequest,
			Body:       `{"error":{"message":"read failed"}}`,
		})
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		m.t.Errorf("演示模型：请求体不是 JSON 对象: %v", err)
		writeFakeResponse(w, testutil.FakeOpenAIResponse{
			StatusCode: http.StatusBadRequest,
			Body:       `{"error":{"message":"bad json"}}`,
		})
		return
	}
	m.mu.Lock()
	m.calls++
	n := m.calls
	dir := m.dir
	var step demoStep
	if len(m.queue) > 0 {
		step, m.queue = m.queue[0], m.queue[1:]
	}
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	if step == nil {
		m.t.Errorf("演示模型：第 %d 次请求没有剧本步——剧本短于 Run 的实际调用数", n)
		writeFakeResponse(w, testutil.FakeOpenAIResponse{
			StatusCode: http.StatusInternalServerError,
			Body:       `{"error":{"message":"demo script exhausted"}}`,
		})
		return
	}
	turn := &demoTurn{t: m.t, model: m, req: req, dir: dir, state: map[string]any{}}
	writeFakeResponse(w, m.runStep(turn, step))
}

// runStep 执行一步剧本并把夹具失败转成 500：剧本在 HTTP 处理协程里跑，`t.Fatalf`
// 会 Goexit 掉这个协程而让 Run 卡在等响应上，因此夹具失败用 panic(abort) 收敛。
// A fixture failure becomes a 500 so the run follows its own error path instead of
// hanging on a goroutine the test framework just exited.
func (m *demoModel) runStep(turn *demoTurn, step demoStep) (out testutil.FakeOpenAIResponse) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if abort, ok := recovered.(demoAbort); ok {
			out = testutil.FakeOpenAIResponse{
				StatusCode: http.StatusInternalServerError,
				Body: mustJSON(map[string]any{
					"error": map[string]any{"message": "demo step aborted: " + abort.reason},
				}),
			}
			return
		}
		panic(recovered)
	}()
	return step(turn)
}

// writeFakeResponse 按 testutil 的同一形态写响应（StatusCode 0 = 200）。
// writeFakeResponse mirrors testutil's response shape.
func writeFakeResponse(w http.ResponseWriter, resp testutil.FakeOpenAIResponse) {
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, resp.Body)
}

// demoAbort 是剧本步的失败信号（见 runStep）。
// demoAbort is the fixture-failure signal handled by runStep.
type demoAbort struct{ reason string }

// demoFatal 是解码助手需要的最小测试上下文：`*testing.T` 与剧本里的 demoTurn 都满足。
// 剧本跑在 HTTP 处理协程上，t.Fatalf 会 Goexit 掉该协程而让 Run 读到空响应；demoTurn
// 用 panic(abort) 顶替，同一个助手在两边都能给出可读的失败。
// demoFatal is the minimal test context the decoding helpers need; demoTurn replaces
// t.Fatalf with an abort panic because scripted turns run on the HTTP handler goroutine.
type demoFatal interface {
	Helper()
	Fatalf(format string, args ...any)
}

// demoTurn 是一步剧本看到的请求与它可用的动作。
// demoTurn is one scripted turn: the request plus the actions a step may take.
type demoTurn struct {
	t     *testing.T
	model *demoModel
	req   map[string]any
	dir   string
	state map[string]any
}

// failf 记录失败并中止本步（转 500，让 Run 走自己的失败路径）。
// failf records the failure and aborts the step.
func (d *demoTurn) failf(format string, args ...any) {
	d.t.Helper()
	d.t.Errorf(format, args...)
	panic(demoAbort{reason: fmt.Sprintf(format, args...)})
}

// must 是 failf 的条件版。
// must is failf behind a condition.
func (d *demoTurn) must(cond bool, format string, args ...any) {
	d.t.Helper()
	if !cond {
		d.failf(format, args...)
	}
}

// Helper 让 demoTurn 满足 demoFatal（剧本没有 caller 可跳，空实现）。
// Helper satisfies demoFatal; scripted turns have no caller to skip.
func (d *demoTurn) Helper() {}

// Fatalf 让 demoTurn 满足 demoFatal：失败走 panic(abort) 而不是 Goexit。
// Fatalf satisfies demoFatal by aborting the step instead of exiting the goroutine.
func (d *demoTurn) Fatalf(format string, args ...any) { d.failf(format, args...) }

// messages 返回本次请求的消息列表。
// messages returns the request's message list.
func (d *demoTurn) messages() []map[string]any {
	raw, _ := d.req["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if msg, ok := item.(map[string]any); ok {
			out = append(out, msg)
		}
	}
	return out
}

// systemText 返回系统提示词正文（技能卡片就在这里）。
// systemText returns the system prompt, which carries the skill cards.
func (d *demoTurn) systemText() string {
	for _, msg := range d.messages() {
		if role, _ := msg["role"].(string); role == "system" {
			text, _ := msg["content"].(string)
			return text
		}
	}
	return ""
}

// toolCallByID 按 tool_call_id 找回助手侧的调用名与参数。
// toolCallByID resolves the assistant-side call name and arguments by call id.
func (d *demoTurn) toolCallByID(id string) (string, map[string]any) {
	for _, msg := range d.messages() {
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		calls, _ := msg["tool_calls"].([]any)
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if callID, _ := call["id"].(string); callID != id {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			arguments, _ := fn["arguments"].(string)
			parsed := map[string]any{}
			if arguments != "" {
				if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
					d.failf("工具调用 %s 的 arguments 不是 JSON 对象: %v（原文 %q）", name, err, arguments)
				}
			}
			return name, parsed
		}
	}
	return "", nil
}

// lastTool 返回上一轮的工具名与参数——一步剧本靠它确认"我正接着谁"。
// lastTool returns the previous round's tool name and arguments.
func (d *demoTurn) lastTool() (string, map[string]any) {
	d.t.Helper()
	id := ""
	for _, msg := range d.messages() {
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ = msg["tool_call_id"].(string)
		}
	}
	d.must(id != "", "最近一条消息不是工具结果——剧本与工具面不一致")
	name, args := d.toolCallByID(id)
	d.must(name != "", "找不到 tool_call_id %q 对应的助手调用", id)
	return name, args
}

// lastToolText 返回上一条工具结果的文本。
// lastToolText returns the text of the most recent tool result.
func (d *demoTurn) lastToolText() string {
	text := ""
	for _, msg := range d.messages() {
		if role, _ := msg["role"].(string); role == "tool" {
			text, _ = msg["content"].(string)
		}
	}
	d.must(text != "", "最近一条消息不是（或没有）工具结果")
	return text
}

// toolTexts 按调用名返回本次 Run 里该名字的全部工具结果（按到达顺序）。
// toolTexts returns every tool result of one name, in arrival order.
func (d *demoTurn) toolTexts(name string) []string {
	names := map[string]string{}
	out := []string{}
	for _, msg := range d.messages() {
		role, _ := msg["role"].(string)
		switch role {
		case "assistant":
			calls, _ := msg["tool_calls"].([]any)
			for _, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				id, _ := call["id"].(string)
				fn, _ := call["function"].(map[string]any)
				callName, _ := fn["name"].(string)
				names[id] = callName
			}
		case "tool":
			id, _ := msg["tool_call_id"].(string)
			if names[id] != name {
				continue
			}
			text, _ := msg["content"].(string)
			out = append(out, text)
		}
	}
	return out
}

// writeAgentFile 在 Agent 目录下写一个文件（模型侧的 `write_file` 只允许整文件覆盖，
// 父目录必须已存在——这里同样先建父目录，模拟运维预先建好的目录）。
// writeAgentFile writes one file under the agent directory.
func (d *demoTurn) writeAgentFile(rel, content string) {
	d.t.Helper()
	path := filepath.Join(d.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		d.failf("建目录 %s 失败: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		d.failf("写 %s 失败: %v", rel, err)
	}
}

// readAgentFile 读 Agent 目录下的一个文件。
// readAgentFile reads one file under the agent directory.
func (d *demoTurn) readAgentFile(rel string) string {
	d.t.Helper()
	raw, err := os.ReadFile(filepath.Join(d.dir, filepath.FromSlash(rel)))
	if err != nil {
		d.failf("读 %s 失败: %v", rel, err)
	}
	return string(raw)
}

// demoCall 是一条工具调用回复。
// demoCall is one tool-call response.
func demoCall(id, name string, args map[string]any) testutil.FakeOpenAIResponse {
	return toolCallResponse(id, name, mustJSON(args))
}

// demoFinal 是收尾的纯文本回复。
// demoFinal is the closing plain-text reply.
func demoFinal(text string) testutil.FakeOpenAIResponse { return textResponse(text) }

// demoShellStdout 从 `shell` 工具结果里剥出脚本 stdout：结果首行是「退出码：N」，
// 其余才是脚本写出的 JSON（契约 agent-md §5.2）。
// demoShellStdout strips the tool's exit-code line and returns the script's stdout.
func demoShellStdout(f demoFatal, result string) string {
	f.Helper()
	first, rest, found := strings.Cut(result, "\n")
	if !found || !strings.HasPrefix(first, "退出码：") {
		f.Fatalf("shell 结果不是「退出码：N + 输出」形态：%q", result)
	}
	return rest
}

// demoShellJSON 解析脚本 stdout 的 JSON 对象。
// demoShellJSON decodes the script's stdout JSON object.
func demoShellJSON(f demoFatal, result string) map[string]any {
	f.Helper()
	stdout := demoShellStdout(f, result)
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		f.Fatalf("脚本 stdout 不是 JSON 对象: %v（原文 %s）", err, stdout)
	}
	return out
}

// demoDigest 是脚本 stdout 里一段 `{path, sha256}` 读数。
// demoDigest is one `{path, sha256}` reading from the script's stdout.
type demoDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// demoScriptOut 是五个示例脚本 stdout 的共同外形（D4 / D5 的 input 是嵌套映射，
// 这里不用；它们各自解析）。
// demoScriptOut is the shape shared by the demo scripts' stdout (D4/D5 parse their own).
type demoScriptOut struct {
	Input           demoDigest     `json:"input"`
	Report          demoDigest     `json:"report"`
	State           demoDigest     `json:"state"`
	Counts          map[string]any `json:"counts"`
	Message         string         `json:"message"`
	Mode            string         `json:"mode"`
	BaselineMissing bool           `json:"baseline_missing"`
}

// decodeDemoScript 把 shell 结果解成共用外形。
// decodeDemoScript decodes the shell result into the shared shape.
func decodeDemoScript(f demoFatal, result string) demoScriptOut {
	f.Helper()
	stdout := demoShellStdout(f, result)
	var out demoScriptOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		f.Fatalf("脚本 stdout 不是预期 JSON: %v（原文 %s）", err, stdout)
	}
	return out
}

// requireDigestOnDisk 核对脚本自报的 `{path, sha256}` 与磁盘上的真实字节一致：
// 六段证据里的「输入快照 / 产物哈希」就是这一条。
// requireDigestOnDisk re-hashes the file on disk and compares it with the script's claim.
func requireDigestOnDisk(t *testing.T, agentDir string, digest demoDigest, label string) {
	t.Helper()
	if digest.Path == "" || digest.SHA256 == "" {
		t.Fatalf("%s：脚本没有给出 {path, sha256}", label)
	}
	path := filepath.Join(agentDir, filepath.FromSlash(digest.Path))
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatalf("%s：读 %s 失败: %v", label, digest.Path, err)
	}
	if sum != digest.SHA256 {
		t.Fatalf("%s：磁盘哈希 %s 与脚本自报 %s 不一致（%s）", label, sum, digest.SHA256, digest.Path)
	}
}

// demoLookup 按路径取 JSON 里的值（键缺任一环返回 nil）。
// demoLookup walks a decoded JSON value along the given keys.
func demoLookup(t *testing.T, value any, keys ...string) any {
	t.Helper()
	current := value
	for _, key := range keys {
		mapping, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = mapping[key]
	}
	return current
}

// demoNumber 读一个 JSON 数字（编码后一律是 float64）。
// demoNumber reads one JSON number.
func demoNumber(t *testing.T, value any, label string) float64 {
	t.Helper()
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("%s 不是数字：%#v", label, value)
	}
	return number
}

// demoString 读一个 JSON 字符串。
// demoString reads one JSON string.
func demoString(t *testing.T, value any, label string) string {
	t.Helper()
	text, ok := value.(string)
	if !ok {
		t.Fatalf("%s 不是字符串：%#v", label, value)
	}
	return text
}

// demoStrings 读一个 JSON 字符串数组。
// demoStrings reads one JSON string array.
func demoStrings(t *testing.T, value any, label string) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%s 不是数组：%#v", label, value)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, demoString(t, item, label+" 元素"))
	}
	return out
}

// demoRunEvidence 是一次 Run 的六段证据读数（数据库面）。
// demoRunEvidence is the database-side reading of the six evidence segments.
type demoRunEvidence struct {
	Status     string
	Trigger    string
	Iterations int
	ToolCalls  int
	Notify     int
	Failed     int
}

// readDemoEvidence 读一次 Run 的终态、轮次、工具数与通知 / 失败调用计数。
// readDemoEvidence reads the run's terminal state and the notify/failure counts.
func readDemoEvidence(t *testing.T, h *harness, runID string) demoRunEvidence {
	t.Helper()
	db := openDB(t, h.dbPath())
	var out demoRunEvidence
	if err := db.QueryRow(
		`SELECT status, trigger, iterations, tool_calls FROM runs WHERE run_id = ?`, runID,
	).Scan(&out.Status, &out.Trigger, &out.Iterations, &out.ToolCalls); err != nil {
		t.Fatalf("读 Run %s 失败: %v", runID, err)
	}
	out.Notify = scalarInt(t, db,
		`SELECT COUNT(*) FROM tool_invocations WHERE run_id = ? AND tool_name = 'notify'`, runID)
	out.Failed = scalarInt(t, db,
		`SELECT COUNT(*) FROM tool_invocations WHERE run_id = ? AND status <> 'success'`, runID)
	return out
}

// demoShellResults 按 Run 返回 `shell` 调用的工具结果（同一 Run 多次跑脚本时按序）。
// demoShellResults returns the shell results of one run in order.
func demoShellResults(t *testing.T, h *harness, runID string) []string {
	t.Helper()
	return demoToolResults(t, h, runID, "shell")
}

// demoToolResults 返回一次 Run 里某个工具的每次调用结果正文（按调用顺序）。
// demoToolResults returns each call's result body for one tool, in call order.
func demoToolResults(t *testing.T, h *harness, runID, toolName string) []string {
	t.Helper()
	db := openDB(t, h.dbPath())
	rows := queryRows(t, db,
		`SELECT result_json FROM tool_invocations
		  WHERE run_id = ? AND tool_name = ? ORDER BY id`, runID, toolName)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, toolResultContent(t, stringValue(t, row, "result_json")))
	}
	return out
}

// toolResultContent 剥离 `tool_invocations.result_json` 的 `{"content": …}` 外壳，取回
// 工具结果正文（与 `core.synthesis` 的读回形态同一份结构）。模型看到的就是这一段；
// 直接对整行 JSON 做字符串判断会把外壳当正文。
// toolResultContent unwraps the audit row's {"content": …} envelope, the same shape the
// recovery path reads back; the model sees exactly this text.
func toolResultContent(t *testing.T, raw string) string {
	t.Helper()
	var shape struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &shape); err != nil {
		t.Fatalf("result_json 不是 {content} 形态: %v（原文 %q）", err, raw)
	}
	return shape.Content
}

// demoNotifyTexts 按 Run 返回 `notify` 调用发出的正文（入参里的 content 原样）。
// demoNotifyTexts returns the contents sent through the notify tool in one run.
func demoNotifyTexts(t *testing.T, h *harness, runID string) []string {
	t.Helper()
	db := openDB(t, h.dbPath())
	rows := queryRows(t, db,
		`SELECT input_json FROM tool_invocations
		  WHERE run_id = ? AND tool_name = 'notify' ORDER BY id`, runID)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var input map[string]any
		if err := json.Unmarshal([]byte(stringValue(t, row, "input_json")), &input); err != nil {
			t.Fatalf("notify 入参不是 JSON: %v", err)
		}
		content, _ := input["content"].(string)
		out = append(out, content)
	}
	return out
}

// demoExecution 读任务最近一次执行行（run_id / trigger / status）。
// demoExecution reads the task's latest execution row.
func demoExecution(t *testing.T, h *harness, taskID string) (runID, trigger, status string) {
	t.Helper()
	db := openDB(t, h.dbPath())
	if err := db.QueryRow(
		`SELECT COALESCE(run_id, ''), trigger, status FROM task_executions
		  WHERE task_id = ? ORDER BY id DESC LIMIT 1`, taskID,
	).Scan(&runID, &trigger, &status); err != nil {
		t.Fatalf("读任务 %s 的执行历史失败: %v", taskID, err)
	}
	return runID, trigger, status
}

// demoDeliveryText 解开代投请求体的 `{"content": …}` 外壳：投递服务发的就是这一段
// 正文，接收端记录的是原样请求体。
// demoDeliveryText unwraps the delivery request body envelope the receiver records.
func demoDeliveryText(t *testing.T, body string) string {
	t.Helper()
	var shape struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &shape); err != nil {
		t.Fatalf("代投请求体不是 {content} 形态: %v（原文 %q）", err, body)
	}
	return shape.Content
}

// demoDeliveryStatus 读一次 Run 的系统代投状态；没有代投行时返回空串。
// demoDeliveryStatus reads the system delivery status of one run (empty when absent).
func demoDeliveryStatus(t *testing.T, h *harness, runID string) string {
	t.Helper()
	db := openDB(t, h.dbPath())
	var status string
	err := db.QueryRow(`SELECT status FROM schedule_deliveries WHERE run_id = ?`, runID).Scan(&status)
	if err != nil {
		return ""
	}
	return status
}

// waitDeliveryStatus 等一次 Run 的系统代投行落到期望终态，等不到即判失败。
// waitDeliveryStatus blocks until the delivery row of one run reaches the wanted state.
//
// 为什么必须等：调度器先写执行行终态（`finishRun`），再发投递（`settleDelivery`），
// 而 `waitCronRun` 只看任务的 last_status。两者之间有一个窗口——窗口内投递行还是
// pending、接收端还没收到字节。此前直接读接收端计数，就是在赌这个窗口恰好关闭；
// 在负载高的整包 e2e 里赌输过一次（gate r3：industry_brief 收到 0 次投递）。
// The scheduler flips the execution row before it sends, so a reader that returns as
// soon as last_status=completed can still observe an unsent delivery.
func waitDeliveryStatus(t *testing.T, h *harness, runID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	logged := false
	for {
		last := demoDeliveryStatus(t, h, runID)
		if last == want {
			return
		}
		// 窗口真的出现过才打这条：它是「这次等待不是空转」的当场证据，
		// 平时（投递早已落库）一行都不留。
		// Logged once, and only when the caller actually had to wait.
		if !logged && last != "" && last != want {
			t.Logf("Run %s 的代投行首次读数 %q（期望 %q），继续等终态", runID, last, want)
			logged = true
		}
		// 已离开 pending 却不是期望值：立即失败，别把误判拖成超时（诊断信息更有用）。
		if last != "" && last != string(core.DeliveryPending) {
			t.Fatalf("Run %s 的系统代投状态 = %q，期望 %q", runID, last, want)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("等待 Run %s 的系统代投状态 = %q 超时（%s），最后一次读数 %q",
				runID, want, timeout, last)
		}
		time.Sleep(readinessPoll)
	}
}

// demoFixture 是一整套五 Demo 夹具：一个工作区、一台假模型、一个 80 端口接收端。
// demoFixture is the whole five-demo fixture: one workspace, one fake model, one receiver.
type demoFixture struct {
	t      *testing.T
	h      *harness
	repo   string
	addr   string
	python string
	// docs 是预构建的 docs-readonly 可执行文件（一次性构建，config 与 mcp_servers 两处
	// 引用同一份路径；`t.TempDir()` 每次调用都不同，故必须在这里只取一次）。
	// docs is the prebuilt docs-readonly binary shared by the config and the MCP definitions.
	docs string
	// docsRoot 是 D4 的只读文档库：从仓库自带 fixtures 复制到临时目录。第三轮要真的
	// 增 / 改 / 删——判定必须落在真实字节上，又不能改到仓库里的那四份。
	// docsRoot is a temp copy of the shipped docs fixture: D4 mutates real bytes and the
	// repository copy must stay untouched.
	docsRoot string
	model    *demoModel
	recv     *demoReceiver
	specs    map[string]string
	env      []string
}

// newDemoFixture 装配夹具并落好配置、Provider 基线、渠道行与四份 MCP 规格资产。
// newDemoFixture assembles the fixture and lands the config, the provider baseline,
// the channel row and the four MCP spec files.
func newDemoFixture(t *testing.T) *demoFixture {
	t.Helper()
	repo, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	f := &demoFixture{
		t:      t,
		h:      newHarness(t),
		repo:   repo,
		addr:   freeLoopbackAddr(t),
		python: demoPython(t),
		model:  newDemoModel(t),
		recv:   startDemoReceiver(t),
		specs:  map[string]string{},
	}
	f.docs = buildDocsReadOnlyBinary(t, f.repo)
	f.docsRoot = filepath.Join(t.TempDir(), "docs")
	copyTree(t, filepath.Join(f.repo, "examples", "shared", "fixtures", "docs"), f.docsRoot)
	specDir := t.TempDir()
	for key, name := range map[string]string{
		demoGitHubSpecEnv: "github",
		demoDBHubSpecEnv:  "dbhub",
		demoBraveSpecEnv:  "brave",
		demoFetchSpecEnv:  "fetch",
	} {
		f.specs[key] = filepath.Join(specDir, name+".json")
	}
	f.writeSpecs(t)
	f.env = append(fixtureEnv(),
		"BASALT_E2E_MCP_HELPER=1",
		demoNotifyEnvRef+"="+demoNotifyURL,
		// Python 3 在管道上的缺省编码是平台码页（本机 GBK）；脚本 stdout 里的中文
		// 必须按 UTF-8 交给工具面，否则模型读到的结论就是乱码。
		// Piped stdout defaults to the platform code page; the scripts print UTF-8.
		"PYTHONIOENCODING=utf-8",
		demoGitHubSpecEnv+"="+f.specs[demoGitHubSpecEnv],
		demoDBHubSpecEnv+"="+f.specs[demoDBHubSpecEnv],
		demoBraveSpecEnv+"="+f.specs[demoBraveSpecEnv],
		demoFetchSpecEnv+"="+f.specs[demoFetchSpecEnv],
	)
	// 先跑 `basalt init`：迁移会扫 `.basalt/agents`，目录不存在时重路径装配直接失败
	// （fail-closed 的迁移前置）——这一步正是运维的真实第一步，不替它兜底。
	// `basalt init` runs first: the v3 migration scans the agents directory and refuses to
	// assemble without it, exactly as an operator's first step would.
	requireExit(t, f.h.run(t, f.env, "init"), 0, "工作区初始化")
	// 四家 MCP server 一次落齐：五 Demo 的定义都声明了 MCP 工具，任何一家缺定义都会让
	// Agent 因「工具未注册」整只不注册——这不是夹具可以按需省略的部分。
	// All four servers land at once: every demo declares MCP tools, and a missing server
	// keeps the whole agent out of the registry.
	f.writeMCPServers(t)
	f.h.writeConfig(t, f.config())
	f.h.publishBeforeSnapshot(t, f.env)
	insertNotifyChannel(t, f.h, demoNotifyName, "webhook", "${"+demoNotifyEnvRef+"}", "演示接收端")
	return f
}

// config 落 basalt.yaml：一家指向假模型的 provider、管理面认证、接收端白名单、
// 受控解释器与 docs-readonly 可执行名。
// config renders basalt.yaml for the fixture.
func (f *demoFixture) config() string {
	var b strings.Builder
	b.WriteString("providers:\n")
	b.WriteString("  - name: " + demoProvider + "\n")
	b.WriteString("    base_url: " + f.model.URL() + "\n")
	b.WriteString("    api_key: ${" + fixtureKeyEnv + "}\n")
	b.WriteString("    default_model: " + demoModelName + "\n")
	b.WriteString("http:\n")
	b.WriteString("  addr: " + f.addr + "\n")
	b.WriteString("  allowed_domains:\n")
	b.WriteString("    - localhost\n")
	b.WriteString("    - demo-unreachable.invalid\n")
	b.WriteString(adminAuthSection())
	// 五个 Demo 在 AGENT.md 里声明了 settings.max_iterations（12 / 12 / 12 / 20 / 30），
	// 而 Profile 只能**收紧**运维上限（契约 §6）：缺省 10 会让 D1 之外的剧本在中途
	// 落 limit_reached。这里按示例声明的最大值给运维上限——与实际部署时 SETUP 要求
	// 在 basalt.yaml 里调高 budget.max_iterations 是同一条链路。
	// The demos declare their own iteration caps, but a profile may only tighten the
	// operator budget, so the fixture raises it to the largest declared value.
	b.WriteString("budget:\n")
	b.WriteString("  max_iterations: 30\n")
	b.WriteString("shell:\n")
	b.WriteString("  allowed_commands:\n")
	b.WriteString("    - " + demoQuote(os.Args[0]) + "\n")
	// 解释器同时进 allowed_commands 与 interpreters：SETUP §6 的开启方式即如此，
	// 授权面放行 executable 整串，argv 形态与脚本越界由执行侧判。
	// The interpreter goes into both lists, exactly as SETUP §6 instructs.
	b.WriteString("    - " + demoQuote(f.python) + "\n")
	b.WriteString("    - " + demoQuote(f.docs) + "\n")
	b.WriteString("  interpreters:\n")
	b.WriteString("    - " + demoQuote(f.python) + "\n")
	b.WriteString("  env_passthrough:\n")
	for _, name := range []string{
		"PATH", "SystemRoot", "TEMP", "TMP", "USERPROFILE", "LOCALAPPDATA", "PYTHONIOENCODING",
	} {
		b.WriteString("    - " + name + "\n")
	}
	return b.String()
}

// demoQuote 用单引号包住 YAML 标量（Windows 路径里的反斜杠在单引号里不转义）。
// demoQuote single-quotes a YAML scalar so backslashes stay literal.
func demoQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

// buildDocsReadOnlyBinary 构建只读 MCP server（嵌套 module，纯标准库）。D4 夹具与模板
// 闸门共用这一份构建代码——两处各写一遍必然漂移，而它判的是同一个交付物。
// 构建失败即判红：它是仓库交付物，不是可跳过的前置。
// buildDocsReadOnlyBinary builds the shipped read-only MCP server; both the demo
// fixture and the template gate use this one implementation. A build failure fails the
// test because the server is a deliverable, not an optional dependency.
func buildDocsReadOnlyBinary(t *testing.T, repo string) string {
	t.Helper()
	name := "docs-readonly"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(t.TempDir(), name)
	moduleDir := filepath.Join(repo, "examples", "shared", "mcp", "docs-readonly")
	cmd := exec.Command("go", "build", "-trimpath", "-o", target, ".")
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("构建 docs-readonly 失败: %v\n%s", err, out)
	}
	return target
}

// writeSpecs 落四份 MCP 规格文件：工具名与描述固定（`tools/list` 只在连接时读一次），
// 结果每个 Run 由剧本重写。
// writeSpecs lands the four spec files: names and descriptions are fixed (tools/list is
// read once per connection), results are rewritten per run.
func (f *demoFixture) writeSpecs(t *testing.T) {
	t.Helper()
	objectSchema := func(fields ...string) map[string]any {
		properties := map[string]any{}
		for _, field := range fields {
			properties[field] = map[string]any{"type": "string"}
		}
		return map[string]any{"type": "object", "properties": properties}
	}
	f.writeSpec(t, demoGitHubSpecEnv, []testutil.MCPSpecTool{
		{Name: "search_repositories", Description: "检索仓库", Schema: objectSchema("q", "per_page"), Result: `{"total_count":0,"items":[]}`},
		{Name: "get_file_contents", Description: "读仓库文件", Schema: objectSchema("owner", "repo", "path", "ref"), Result: `{"download_url":""}`},
		{Name: "list_pull_requests", Description: "列 PR", Schema: objectSchema("owner", "repo", "state"), Result: `{"items":[]}`},
		{Name: "list_issues", Description: "列 Issue", Schema: objectSchema("owner", "repo", "state"), Result: `{"items":[]}`},
		{Name: "list_releases", Description: "列 Release", Schema: objectSchema("owner", "repo"), Result: `{"items":[]}`},
	})
	f.writeSpec(t, demoDBHubSpecEnv, []testutil.MCPSpecTool{
		{Name: "run_query", Description: "执行只读 SQL", Schema: objectSchema("sql"), Result: `{"rows":[]}`},
		{Name: "list_tables", Description: "列表清单", Schema: objectSchema(), Result: `{"tables":["orders"]}`},
	})
	f.writeSpec(t, demoBraveSpecEnv, []testutil.MCPSpecTool{
		{Name: "brave_web_search", Description: "网页检索", Schema: objectSchema("query", "count"), Result: `{"web":{"results":[]}}`},
	})
	f.writeSpec(t, demoFetchSpecEnv, []testutil.MCPSpecTool{
		{Name: "fetch", Description: "抓取网页正文", Schema: objectSchema("url"), Result: ""},
	})
}

// writeSpec 写一份规格文件。
// writeSpec writes one spec file.
func (f *demoFixture) writeSpec(t *testing.T, key string, tools []testutil.MCPSpecTool) {
	t.Helper()
	raw, err := json.Marshal(testutil.MCPSpec{Tools: tools})
	if err != nil {
		t.Fatalf("编码 MCP 规格 %s 失败: %v", key, err)
	}
	if err := os.WriteFile(f.specs[key], raw, 0o644); err != nil {
		t.Fatalf("写 MCP 规格 %s 失败: %v", key, err)
	}
}

// setToolResult 改写某份规格里一个工具的当前结果（下一次 `tools/call` 生效）。
// setToolResult rewrites one tool's current result for the next tools/call.
func (f *demoFixture) setToolResult(t *testing.T, key, name, result string) {
	f.t.Helper()
	raw, err := os.ReadFile(f.specs[key])
	if err != nil {
		t.Fatalf("读 MCP 规格 %s 失败: %v", key, err)
	}
	var spec testutil.MCPSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("解析 MCP 规格 %s 失败: %v", key, err)
	}
	found := false
	for i := range spec.Tools {
		if spec.Tools[i].Name == name {
			spec.Tools[i].Result = result
			spec.Tools[i].Error = ""
			found = true
		}
	}
	if !found {
		t.Fatalf("MCP 规格 %s 里没有工具 %s", key, name)
	}
	f.writeSpec(t, key, spec.Tools)
}

// setToolError 把某份规格里的工具改成返回 IsError（模拟服务端失败）。
// setToolError makes one tool answer with an IsError result.
func (f *demoFixture) setToolError(t *testing.T, key, name, message string) {
	f.t.Helper()
	raw, err := os.ReadFile(f.specs[key])
	if err != nil {
		t.Fatalf("读 MCP 规格 %s 失败: %v", key, err)
	}
	var spec testutil.MCPSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("解析 MCP 规格 %s 失败: %v", key, err)
	}
	found := false
	for i := range spec.Tools {
		if spec.Tools[i].Name == name {
			spec.Tools[i].Error = message
			spec.Tools[i].Result = ""
			found = true
		}
	}
	if !found {
		t.Fatalf("MCP 规格 %s 里没有工具 %s", key, name)
	}
	f.writeSpec(t, key, spec.Tools)
}

// fixtureJSON 读仓库里的示例固定输入（五 Demo 的 fixtures/ 就是它们的固定输入）。
// fixtureJSON reads one shipped example input file (the demo's fixed fixture).
func (f *demoFixture) fixtureJSON(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.repo, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读示例固定输入 %s 失败: %v", rel, err)
	}
	return string(raw)
}

// writeMCPServers 落 mcp_servers.yaml：四家 stdio server。三家走规格 helper
// （外部服务的替身），docs-readonly 是仓库自研的真 server。
// writeMCPServers writes mcp_servers.yaml: three spec-helper stand-ins plus the real
// docs-readonly server.
func (f *demoFixture) writeMCPServers(t *testing.T) {
	t.Helper()
	docsDir := f.docsRoot
	specServer := func(name, specEnv string) string {
		var b strings.Builder
		b.WriteString("  - name: " + name + "\n")
		b.WriteString("    transport: stdio\n")
		b.WriteString("    command: " + demoQuote(os.Args[0]) + "\n")
		b.WriteString("    args:\n")
		b.WriteString("      - -test.run=^TestMCPHelperProcess$\n")
		b.WriteString("    env:\n")
		b.WriteString("      BASALT_MCP_HELPER: ${BASALT_E2E_MCP_HELPER}\n")
		b.WriteString("      BASALT_MCP_HELPER_SPEC: ${" + specEnv + "}\n")
		return b.String()
	}
	var b strings.Builder
	b.WriteString("servers:\n")
	b.WriteString(specServer("github", demoGitHubSpecEnv))
	b.WriteString(specServer("dbhub", demoDBHubSpecEnv))
	b.WriteString(specServer("brave-search", demoBraveSpecEnv))
	b.WriteString(specServer("fetch", demoFetchSpecEnv))
	b.WriteString("  - name: docs-readonly\n")
	b.WriteString("    transport: stdio\n")
	b.WriteString("    command: " + demoQuote(f.docs) + "\n")
	b.WriteString("    args:\n")
	b.WriteString("      - -root\n")
	b.WriteString("      - " + demoQuote(docsDir) + "\n")
	path := filepath.Join(f.h.Root, ".basalt", "mcp_servers.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建工作区目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("写 mcp_servers.yaml 失败: %v", err)
	}
}

// copyTree 把示例目录整棵复制进工作区（示例是「复制到 .basalt/agents」的唯一来源，
// 夹具不另写一份 AGENT.md——判据必须对着仓库里发出去的那份）。
// copyTree copies one example directory verbatim into the workspace.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("复制示例目录 %s 失败: %v", src, err)
	}
}

// demoReplace 在 frontmatter 里做一次精确替换；没有命中或命中多处都判失败——
// 示例文件改了形态却没改夹具，必须当场炸出来，而不是静默装出一份半改的配置。
// demoReplace requires exactly one hit inside the frontmatter.
func demoReplace(t *testing.T, front, old, updated string) string {
	t.Helper()
	count := strings.Count(front, old)
	if count != 1 {
		t.Fatalf("frontmatter 里 %q 命中 %d 次，期望 1 次——示例定义形态变了，夹具需同步", old, count)
	}
	return strings.Replace(front, old, updated, 1)
}

// splitDemoFrontmatter 把 AGENT.md 切成 frontmatter 与正文（各自保留原文）。
// splitDemoFrontmatter splits an AGENT.md into its frontmatter and body.
func splitDemoFrontmatter(t *testing.T, raw string) (front, body string) {
	t.Helper()
	const fence = "---\n"
	if !strings.HasPrefix(raw, fence) {
		t.Fatalf("AGENT.md 不以 --- 开头")
	}
	rest := raw[len(fence):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("AGENT.md 的 frontmatter 没有闭合围栏")
	}
	return rest[:end], rest[end+len("\n---\n"):]
}

// demoBindings 渲染 frontmatter 的 resource_bindings 块：显式写出（示例自带的
// `notify_mode` 让夹具补绑路径不生效，UID 必须由夹具按当前发布指针写进来）。
// demoBindings renders the explicit bindings block the fixture must write itself.
func demoBindings(modelUID, channelUID string) string {
	var b strings.Builder
	b.WriteString("resource_bindings:\n")
	b.WriteString("  model_connection:\n")
	b.WriteString("    - name: " + demoProvider + "\n")
	b.WriteString("      uid: " + modelUID + "\n")
	if channelUID == "" {
		b.WriteString("  notify_channel: []\n")
	} else {
		b.WriteString("  notify_channel:\n")
		b.WriteString("    - name: " + demoNotifyName + "\n")
		b.WriteString("      uid: " + channelUID + "\n")
	}
	return b.String()
}

// demoOptions 是装一只 Demo Agent 时必须由夹具决定的少数几个取值。
// demoOptions carries the few values the fixture must decide per demo.
type demoOptions struct {
	// notify 为 true 时把定义改成 `notify_mode: allowlist` 并把 `notify` 加入工具面
	// （D1 / D4 的 SETUP 开启方式；D3 明确零通知，D2 / D5 走系统代投）。
	notify bool
	// deliverTo 非空时给任务加 `deliver_to`（D2 / D5 的系统代投路径）。
	deliverTo string
	// cronAt 是本次的触发时刻（Asia/Shanghai，见调用方）。
	cronAt time.Time
	// modelUID / channelUID 是当前发布指针身份。
	modelUID   string
	channelUID string
	// cronOld 是示例里原有的 cron 字面量（替换唯一命中判据）。
	cronOld string
}

// installAgent 把一只示例 Demo 装进工作区并改写成可运行的夹具形态：
// 只替换模型名、cron、通知开关 / 渠道、绑定与投递目标；正文与脚本一字不改。
// installAgent copies one example demo in and rewrites only the fixture-owned fields.
func (f *demoFixture) installAgent(t *testing.T, source string, opts demoOptions) string {
	t.Helper()
	src := filepath.Join(f.repo, "examples", "daily", source)
	dir := filepath.Join(f.h.Root, ".basalt", "agents", source)
	copyTree(t, src, dir)
	path := filepath.Join(dir, "AGENT.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s/AGENT.md 失败: %v", source, err)
	}
	front, body := splitDemoFrontmatter(t, string(raw))
	front = demoReplace(t, front, "  model: 请在此填写模型名", "  model: "+demoModelName)
	front = demoReplace(t, front, `    cron: "`+opts.cronOld+`"`,
		`    cron: "`+cronAt(opts.cronAt)+`"`)
	if opts.notify {
		front = demoReplace(t, front, "notify_mode: none", "notify_mode: allowlist")
		front = demoReplace(t, front, "notify_channels: []",
			"notify_channels:\n  - "+demoNotifyName)
		front = demoReplace(t, front, "  - shell\nmcp_servers:",
			"  - shell\n  - notify\nmcp_servers:")
	}
	if opts.deliverTo != "" {
		front = demoReplace(t, front, "    zone: Asia/Shanghai\n",
			"    zone: Asia/Shanghai\n    deliver_to: "+opts.deliverTo+"\n")
	}
	front = demoReplace(t, front,
		"resource_bindings:\n  model_connection: []\n  notify_channel: []\n",
		demoBindings(opts.modelUID, opts.channelUID))
	updated := "---\n" + front + "\n---\n" + body
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("写 %s/AGENT.md 失败: %v", source, err)
	}
	f.h.writeActiveLifecycleState(t, source)
	return dir
}

// installSkill 复制一只技能到工作区技能库（D2 的渐进披露来源）。
// installSkill copies one skill into the workspace skill library.
func (f *demoFixture) installSkill(t *testing.T, demo, skill string) {
	t.Helper()
	src := filepath.Join(f.repo, "examples", "daily", demo, "skills", skill)
	dst := filepath.Join(f.h.Root, ".basalt", "skills", skill)
	copyTree(t, src, dst)
}

// modelUID 读当前 provider 的发布指针身份。
// modelUID reads the published pointer of the fixture provider.
func (f *demoFixture) modelUID(t *testing.T) string {
	t.Helper()
	uid, ok := f.h.modelConnectionUID(t, demoProvider)
	if !ok {
		t.Fatalf("provider %s 没有已发布指针——基线发布失败", demoProvider)
	}
	return uid
}

// channelUID 读夹具渠道的资源 UID。
// channelUID reads the fixture channel's resource UID.
func (f *demoFixture) channelUID(t *testing.T) string {
	t.Helper()
	channels := f.h.fixtureChannels()
	for _, channel := range channels {
		if channel.name == demoNotifyName {
			return channel.uid
		}
	}
	t.Fatalf("夹具渠道 %s 未登记", demoNotifyName)
	return ""
}

// demoCST 是示例任务声明的时区（Asia/Shanghai）。
// demoCST is the zone the shipped schedules declare.
func demoCST() *time.Location { return time.FixedZone("CST", 8*60*60) }

// serve 拉起 `basalt serve` 并等健康检查；返回进程句柄。
// serve starts the binary and waits for readiness.
func (f *demoFixture) serve(t *testing.T) *groupedProcess {
	t.Helper()
	p := startGrouped(t, f.h, f.env, "serve")
	waitHealthHTTP(t, p, f.addr, 60*time.Second)
	return p
}

// runScheduleNowOnce 发一次人工补跑并断言 200 + completed，返回 Run 读数。
// runScheduleNowOnce triggers one manual run and requires a completed result.
func (f *demoFixture) runScheduleNowOnce(t *testing.T, taskID string) webRun {
	t.Helper()
	resp := runScheduleNow(t, f.addr, taskID)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "人工补跑 "+taskID)
	var run webRun
	decodeWebData(t, resp, &run, "人工补跑 "+taskID)
	if run.Status != "completed" {
		t.Fatalf("人工补跑 %s 的 Run 状态 = %q，期望 completed（reason %q）", taskID, run.Status, run.Reason)
	}
	if run.Trigger != "manual" {
		t.Fatalf("人工补跑 %s 的 trigger = %q，期望 manual", taskID, run.Trigger)
	}
	if left := f.model.scriptLeft(); left != 0 {
		t.Fatalf("人工补跑 %s 结束后剧本还剩 %d 步——Run 调用的轮次少于剧本", taskID, left)
	}
	return run
}

// waitCronRun 等自动触发的执行行落库并返回它的 run_id。
// waitCronRun waits for the cron-triggered execution row and returns its run id.
func (f *demoFixture) waitCronRun(t *testing.T, taskID string, timeout time.Duration) string {
	t.Helper()
	waitScheduleStatus(t, f.addr, taskID, "completed", timeout)
	runID, trigger, status := demoExecution(t, f.h, taskID)
	if trigger != "cron" {
		t.Fatalf("任务 %s 的首条执行 trigger = %q，期望 cron", taskID, trigger)
	}
	if status != "completed" {
		t.Fatalf("任务 %s 的首条执行 status = %q，期望 completed", taskID, status)
	}
	if left := f.model.scriptLeft(); left != 0 {
		t.Fatalf("自动触发 %s 结束后剧本还剩 %d 步", taskID, left)
	}
	return runID
}

// requireNotifyCount 断言一次 Run 的 `notify` 调用次数与接收端累计命中数。
// requireNotifyCount asserts the notify call count of one run plus the receiver total.
func (f *demoFixture) requireNotifyCount(t *testing.T, runID string, want int, wantTotal int) {
	t.Helper()
	evidence := readDemoEvidence(t, f.h, runID)
	if evidence.Notify != want {
		t.Fatalf("Run %s 的 notify 调用 %d 次，期望 %d 次", runID, evidence.Notify, want)
	}
	if got := f.recv.count(demoReplyPath); got != wantTotal {
		t.Fatalf("接收端累计收到 %d 次投递，期望 %d 次", got, wantTotal)
	}
}

// clearAgentMessages 抹掉某只 Agent 的全部会话消息，模拟**零历史会话**：定时任务的
// 会话 id 由 (scheduler, scheduler, agent) 派生、跨运行恒定，删掉消息就等于换了一台
// 没有上下文的新会话——D4 的跨会话基线只能来自记忆，不能来自对话历史。
// clearAgentMessages wipes one agent's session so the next run starts with zero history.
func clearAgentMessages(t *testing.T, h *harness, agent string) {
	t.Helper()
	db := openDB(t, h.dbPath())
	if _, err := db.Exec(
		`DELETE FROM messages
		  WHERE session_id IN (SELECT session_id FROM sessions WHERE profile_name = ?)`,
		agent,
	); err != nil {
		t.Fatalf("清理 %s 的会话消息失败: %v", agent, err)
	}
	// 冗余计数跟着归零：只删消息不改计数，列表页会显示一个不存在的历史长度。
	// The denormalized counter follows the rows it counts.
	if _, err := db.Exec(
		`UPDATE sessions SET message_count = 0 WHERE profile_name = ?`, agent,
	); err != nil {
		t.Fatalf("重置 %s 的消息计数失败: %v", agent, err)
	}
}

// demoToolArgs 按工具名返回一次 Run 里该工具的全部入参（按调用顺序）。
// demoToolArgs returns every call's decoded arguments for one tool name.
func demoToolArgs(t *testing.T, h *harness, runID, toolName string) []map[string]any {
	t.Helper()
	db := openDB(t, h.dbPath())
	rows := queryRows(t, db,
		`SELECT input_json FROM tool_invocations
		  WHERE run_id = ? AND tool_name = ? ORDER BY id`, runID, toolName)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var args map[string]any
		if err := json.Unmarshal([]byte(stringValue(t, row, "input_json")), &args); err != nil {
			t.Fatalf("%s 的入参不是 JSON 对象: %v", toolName, err)
		}
		out = append(out, args)
	}
	return out
}

// demoArgPaths 取一组调用里的 `path` 入参（顺序保持）。
// demoArgPaths extracts the `path` argument of each call.
func demoArgPaths(calls []map[string]any) []string {
	out := make([]string, 0, len(calls))
	for _, args := range calls {
		path, _ := args["path"].(string)
		out = append(out, path)
	}
	return out
}

// demoToolPaths 按 Run 取出某工具每次调用的 `path` 入参（判 D2 的 L2/L3 与 D4 的读正文
// 读的是哪个文件——审计里的 input_json 就是模型当时给的参数原文）。
// demoToolPaths returns the path argument of each call to one tool in a run.
func demoToolPaths(t *testing.T, h *harness, runID, toolName string) []string {
	t.Helper()
	db := openDB(t, h.dbPath())
	rows := queryRows(t, db,
		`SELECT input_json FROM tool_invocations
		  WHERE run_id = ? AND tool_name = ? ORDER BY id`, runID, toolName)
	calls := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var args map[string]any
		if err := json.Unmarshal([]byte(stringValue(t, row, "input_json")), &args); err != nil {
			t.Fatalf("解析 %s 的调用参数失败: %v", toolName, err)
		}
		calls = append(calls, args)
	}
	return demoArgPaths(calls)
}

// skillCard 从系统提示词里取出某只技能的 L1 卡片路径：模块卡 §2.4.5 要求 D2 证明
// 「三级披露」——卡片只给路径与描述，正文必须由模型自己按路径读。
// skillCard pulls one skill's L1 card path out of the system prompt.
func (d *demoTurn) skillCard(name string) string {
	system := d.systemText()
	prefix := "- " + name + "："
	for _, line := range strings.Split(system, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		start := strings.Index(line, "（")
		end := strings.LastIndex(line, "）")
		if start < 0 || end <= start {
			d.failf("技能卡片 %q 不是「- 名称：描述（路径）」形态：%q", name, line)
		}
		path := line[start+len("（") : end]
		wantPrefix := ".basalt/versions/skills/" + name + "/"
		if !strings.HasPrefix(path, wantPrefix) || !strings.HasSuffix(path, "/SKILL.md") {
			d.failf("技能卡片路径 %q 不是发布目录形态（期望 %s…/SKILL.md）", path, wantPrefix)
		}
		return path
	}
	d.failf("系统提示词里没有 %q 的技能卡片（L1）——声明了却没发布？", name)
	return ""
}

// toolNames 返回本次请求工具面的名字集合。
// toolNames returns the tool face of the current request as a set.
func (d *demoTurn) toolNames() map[string]bool {
	out := map[string]bool{}
	raw, _ := d.req["tools"].([]any)
	for _, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != "" {
			out[name] = true
		}
	}
	return out
}

// requireTool 断言工具面里有某件工具。
// requireTool asserts the tool face carries the named tool.
func (d *demoTurn) requireTool(name string) {
	d.must(d.toolNames()[name], "工具面缺少 %q：%v", name, d.toolNames())
}

// requireNoTool 断言工具面里**没有**某件工具（系统代投时 `notify` 必须缺席）。
// requireNoTool asserts the tool face does not carry the named tool.
func (d *demoTurn) requireNoTool(name, why string) {
	d.must(!d.toolNames()[name], "工具面不应有 %q：%s", name, why)
}

// ============================================================
// D1 github-radar
// ============================================================

// d1Fixture 是 D1 的固定输入（仓库自带 fixtures/search.json 原样）。
// d1Fixture returns the shipped fixed input for D1.
func (f *demoFixture) d1Fixture(t *testing.T) string {
	return f.fixtureJSON(t, "examples/daily/github-radar/fixtures/search.json")
}

// d1StarBump 返回一份「tiny-loader 涨到 60 星」的检索结果：第二次运行要能算出星差。
// d1StarBump bumps the previously trimmed repo's stars so the second run has a delta.
func d1StarBump(t *testing.T, base string) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal([]byte(base), &payload); err != nil {
		t.Fatalf("解析 D1 固定输入失败: %v", err)
	}
	items, _ := payload["items"].([]any)
	bumped := false
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := item["full_name"].(string); name == "lumen-labs/tiny-loader" {
			item["stargazers_count"] = 60
			bumped = true
		}
	}
	if !bumped {
		t.Fatalf("D1 固定输入里没有 lumen-labs/tiny-loader——示例输入变了，星差剧本需同步")
	}
	return mustJSON(payload)
}

// d1Steps 组装一次 D1 运行的剧本：检索 → 落盘输入 → 跑脚本 → 读产物 → 写报告 →
// 通知 → 收尾。runTag 让五轮运行的 tool_call_id 互不相同（同一会话历史里留痕）。
// d1Steps builds one D1 run's script.
func d1Steps(f *demoFixture, runTag string) []demoStep {
	var out demoScriptOut
	next := func(index int, step string) string {
		return fmt.Sprintf("d1-%s-%d-%s", runTag, index, step)
	}
	return []demoStep{
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			return demoCall(next(1, "search"), "search_repositories", map[string]any{
				"q":        "agent mcp scheduler pushed:>=2026-10-01",
				"per_page": 10,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "search_repositories", "第 1 步应是检索，实际 %q", name)
			text := d.lastToolText()
			return demoCall(next(2, "write"), "write_file", map[string]any{
				"path":    "github_search.json",
				"content": text,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 2 步应是落盘，实际 %q", name)
			return demoCall(next(3, "shell"), "shell", map[string]any{
				"executable": f.python,
				"arguments": []string{
					"scripts/github_radar.py", "--input", "github_search.json",
					"--state", "state/seen.json",
				},
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "shell", "第 3 步应是脚本，实际 %q", name)
			out = decodeDemoScript(d, d.lastToolText())
			if out.Report.Path == "" {
				d.failf("脚本没有给出报告路径")
			}
			return demoCall(next(4, "read"), "read_file", map[string]any{"path": out.Report.Path})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "read_file", "第 4 步应是读产物，实际 %q", name)
			report := d.lastToolText()
			dir := filepath.ToSlash(filepath.Dir(out.Report.Path))
			content, err := d1ReportMarkdown(report, out)
			if err != nil {
				d.failf("渲染 D1 日报失败：%v", err)
			}
			return demoCall(next(5, "write-report"), "write_file", map[string]any{
				"path":    dir + "/report.md",
				"content": content,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 5 步应是写报告，实际 %q", name)
			return demoCall(next(6, "notify"), "notify", map[string]any{
				"channel": demoNotifyName,
				"content": out.Message,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "notify", "第 6 步应是通知，实际 %q", name)
			return demoFinal("本次雷达运行完成：" + out.Message)
		},
	}
}

// d1ReportMarkdown 按示例 AGENT.md 第 5 步的固定格式写日报：事实全部取自脚本产物，
// 首跑写「首次运行，无基线，不计算星差」。
// d1ReportMarkdown renders the report exactly from the script artifacts.
func d1ReportMarkdown(raw string, out demoScriptOut) (string, error) {
	var report struct {
		ObservedAt      string `json:"observed_at"`
		BaselineDate    string `json:"baseline_date"`
		BaselineMissing bool   `json:"baseline_missing"`
		Items           []struct {
			FullName  string `json:"full_name"`
			Stars     int    `json:"stars"`
			StarDelta *int   `json:"star_delta"`
			URL       string `json:"url"`
			ReadmeURL string `json:"readme_url"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		return "", fmt.Errorf("解析 D1 脚本产物失败：%w（原文 %q）", err, raw)
	}
	var b strings.Builder
	b.WriteString("# GitHub 开源雷达 · " + report.ObservedAt + "\n\n")
	b.WriteString("结论：" + out.Message + "\n\n")
	b.WriteString("候选 " + fmt.Sprint(out.Counts["candidates"]) +
		" / 去重 " + fmt.Sprint(out.Counts["deduped"]) +
		" / 裁剪 " + fmt.Sprint(out.Counts["trimmed"]) + "\n\n")
	if report.BaselineMissing {
		b.WriteString("首次运行，无基线，不计算星差\n\n")
	}
	for _, item := range report.Items {
		b.WriteString("- " + item.FullName + "（" + fmt.Sprint(item.Stars) + " 星）")
		if item.StarDelta != nil {
			b.WriteString("，较上次基线 +" + fmt.Sprint(*item.StarDelta))
		}
		b.WriteString("：" + item.URL + "\n")
	}
	return b.String(), nil
}

// readDemoFile 读 Agent 目录下的一个文件（判卷侧直接读盘）。
// readDemoFile reads one file under the agent directory.
func readDemoFile(t *testing.T, dir, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读 %s 失败: %v", rel, err)
	}
	return string(raw)
}

// demoMCPFailureReading 是 MCP 工具失败时模型唯一能看到的读数：适配层把 server 正文
// 整段丢弃（连 Cause 也不放），只留这条自产文案。失败报告与通知只能引用它。
// demoMCPFailureReading is the only reading the model gets for a failed MCP call: the
// adapter drops the server text wholesale (constraints 13/14/15/28).
const demoMCPFailureReading = "MCP 工具返回错误：github/search_repositories"

// d1FailureReport 是依赖失败时的报告：写明来源不可达、引用真实读数与观测时间，不补造条目。
// d1FailureReport is the report written when the source is unreachable.
func d1FailureReport(observedAt string) string {
	return "# GitHub 开源雷达 · " + observedAt + "\n\n" +
		"来源不可达：MCP 检索失败（读数：" + demoMCPFailureReading + "），本次未产生可信数据。\n\n" +
		"## 失败\n\n来源不可达 / 数据缺失；未补造任何条目。\n"
}

// d1FailureSteps 是依赖失败的那一轮剧本：检索报错 → 写失败报告 → 通知 → 收尾。
// 全程不跑脚本、不落盘输入——这正是「不得补造条目」的判据面。报告写在 Agent 根目录：
// `write_file` 不建父目录，往不存在的 `runs/failure/` 里写只会得到工具失败。
// d1FailureSteps never writes an input file, so no script can invent results; the report
// lands at the agent root because write_file never creates parent directories.
func d1FailureSteps(f *demoFixture, observedAt, runTag string) []demoStep {
	next := func(index int, step string) string {
		return fmt.Sprintf("d1-failure-%s-%d-%s", runTag, index, step)
	}
	return []demoStep{
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			return demoCall(next(1, "search"), "search_repositories", map[string]any{
				"q":        "agent mcp scheduler",
				"per_page": 10,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "search_repositories", "失败轮第 1 步应是检索，实际 %q", name)
			text := d.lastToolText()
			// 失败读数必须落在适配层自产文案上；本轮刻意让 server 侧带「HTTP 503」，
			// 它**不得**出现在模型上下文里——这是「server 正文整段丢弃」的判据面。
			// The server text deliberately carries "HTTP 503"; it must not leak.
			d.must(strings.Contains(text, demoMCPFailureReading),
				"失败轮应拿到 MCP 适配层自产错误文案 %q，实际 %q", demoMCPFailureReading, text)
			d.must(!strings.Contains(text, "503"), "server 正文泄进模型上下文：%q", text)
			return demoCall(next(2, "write"), "write_file", map[string]any{
				"path":    "failure-report.md",
				"content": d1FailureReport(observedAt),
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			return demoCall(next(3, "notify"), "notify", map[string]any{
				"channel": demoNotifyName,
				"content": "来源不可达：MCP 检索失败，本次未产生可信数据",
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			return demoFinal("来源不可达：数据缺失，本次未产生可信数据")
		},
	}
}

// TestUS14Demos_Fixture 是五 Demo 的进程级验收（模块卡 §4.3 的 `demo-fixtures`）。
// 五个子测试各自覆盖：自动触发与人工补跑、业务断言、空 / 无变化 / 失败三类形态、
// 六段证据与通知计数。
// TestUS14Demos_Fixture is the process-level acceptance of the five demos.
func TestUS14Demos_Fixture(t *testing.T) {
	t.Run("github", testDemoGithubRadar)
	t.Run("repo_digest", testDemoRepoDigest)
	t.Run("biz_daily", testDemoBizDaily)
	t.Run("docs_diff", testDemoDocsDiff)
	t.Run("industry_brief", testDemoIndustryBrief)
}

// testDemoGithubRadar 覆盖 D1：首跑无基线不出星差、第二次去重并算出差值、空输入
// 不编造、依赖失败写失败报告。
// testDemoGithubRadar covers D1 end to end.
func testDemoGithubRadar(t *testing.T) {
	f := newDemoFixture(t)
	f.setToolResult(t, demoGitHubSpecEnv, "search_repositories", f.d1Fixture(t))
	modelUID, channelUID := f.modelUID(t), f.channelUID(t)
	base := time.Now()
	auto := base.Add(15 * time.Second).In(demoCST())
	dir := f.installAgent(t, "github-radar", demoOptions{
		notify:     true,
		cronAt:     auto,
		modelUID:   modelUID,
		channelUID: channelUID,
		cronOld:    "0 30 8 * * *",
	})
	const taskID = "daily-radar"

	f.model.beginRun(dir, d1Steps(f, "a")...)
	serve := f.serve(t)
	defer serve.stop(t)

	firstRun := f.waitCronRun(t, taskID, 120*time.Second)

	// —— 第一次：自动触发，无基线 ——
	first := decodeDemoScript(t, demoShellResults(t, f.h, firstRun)[0])
	if !first.BaselineMissing {
		t.Fatalf("首跑应报 baseline_missing=true，实际 %+v", first)
	}
	if first.Counts["candidates"] != float64(6) || first.Counts["recommended"] != float64(5) ||
		first.Counts["trimmed"] != float64(1) || first.Counts["deduped"] != float64(0) {
		t.Fatalf("首跑计数不符：%#v", first.Counts)
	}
	requireDigestOnDisk(t, dir, first.Input, "首跑输入快照")
	requireDigestOnDisk(t, dir, first.Report, "首跑报告")
	requireDigestOnDisk(t, dir, first.State, "首跑状态")
	reportRaw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(first.Report.Path)))
	if err != nil {
		t.Fatalf("读首跑报告失败: %v", err)
	}
	var firstReport map[string]any
	if err := json.Unmarshal(reportRaw, &firstReport); err != nil {
		t.Fatalf("首跑报告不是 JSON: %v", err)
	}
	items, _ := firstReport["items"].([]any)
	if len(items) != 5 {
		t.Fatalf("首跑推荐 %d 条，期望 5 条", len(items))
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if _, present := item["star_delta"]; present {
			t.Fatalf("首跑不得出现 star_delta：%#v", item)
		}
	}
	evidence := readDemoEvidence(t, f.h, firstRun)
	if evidence.Status != "completed" || evidence.Trigger != "cron" {
		t.Fatalf("首跑终态 = %s / %s，期望 completed / cron", evidence.Status, evidence.Trigger)
	}
	// 剧本是 7 次模型回合（6 次工具调用 + 1 次收尾）；轮次的计数语义不在本 US 判据里
	// （收尾回合是否计入留给实现），因此只钉「工具调用不少于 6 次」与「剧本刚好用完」。
	// The script has 7 model turns; iteration counting semantics are not pinned here, so
	// the assertion keeps to the tool-call floor plus the exactly-consumed script.
	if evidence.ToolCalls < 6 {
		t.Fatalf("首跑工具调用 %d 次，低于剧本下限 6 次", evidence.ToolCalls)
	}
	f.requireNotifyCount(t, firstRun, 1, 1)
	if status := demoDeliveryStatus(t, f.h, firstRun); status != "" {
		t.Fatalf("D1 没有 deliver_to，却产生代投行：%s", status)
	}
	if body := f.recv.received(demoReplyPath)[0]; !strings.Contains(body, "本次推荐 5 个仓库") {
		t.Fatalf("通知正文应含结论句，实际 %q", body)
	}
	firstMarkdown := readDemoFile(t, dir, filepath.ToSlash(filepath.Dir(first.Report.Path))+"/report.md")
	for _, want := range []string{"首次运行，无基线，不计算星差", "本次推荐 5 个仓库"} {
		if !strings.Contains(firstMarkdown, want) {
			t.Fatalf("首跑报告正文缺少 %q：\n%s", want, firstMarkdown)
		}
	}
	if strings.Contains(firstMarkdown, "增长") {
		t.Fatalf("首跑报告不得出现「增长」类无基线说法：\n%s", firstMarkdown)
	}

	// —— 第二次：人工补跑，同一天窗口内去重，只推荐此前被裁剪的那个并算出星差 ——
	f.setToolResult(t, demoGitHubSpecEnv, "search_repositories", d1StarBump(t, f.d1Fixture(t)))
	f.model.beginRun(dir, d1Steps(f, "b")...)
	second := f.runScheduleNowOnce(t, taskID)
	secondOut := decodeDemoScript(t, demoShellResults(t, f.h, second.RunID)[0])
	if secondOut.Counts["deduped"] != float64(5) || secondOut.Counts["recommended"] != float64(1) ||
		secondOut.Counts["trimmed"] != float64(0) {
		t.Fatalf("第二次计数不符：%#v（期望去重 5 / 推荐 1 / 裁剪 0）", secondOut.Counts)
	}
	if secondOut.Message != "本次推荐 1 个仓库（候选 6，去重 5，裁剪 0）" {
		t.Fatalf("第二次结论句 = %q", secondOut.Message)
	}
	secondReportRaw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(secondOut.Report.Path)))
	if err != nil {
		t.Fatalf("读第二次报告失败: %v", err)
	}
	var secondReport struct {
		Items []struct {
			FullName  string `json:"full_name"`
			StarDelta *int   `json:"star_delta"`
		} `json:"items"`
	}
	if err := json.Unmarshal(secondReportRaw, &secondReport); err != nil {
		t.Fatalf("第二次报告不是 JSON: %v", err)
	}
	if len(secondReport.Items) != 1 || secondReport.Items[0].FullName != "lumen-labs/tiny-loader" {
		t.Fatalf("第二次应只推荐 lumen-labs/tiny-loader：%#v", secondReport.Items)
	}
	if secondReport.Items[0].StarDelta == nil || *secondReport.Items[0].StarDelta != 13 {
		t.Fatalf("第二次星差 = %v，期望 13", secondReport.Items[0].StarDelta)
	}
	requireDigestOnDisk(t, dir, secondOut.State, "第二次状态")
	secondEvidence := readDemoEvidence(t, f.h, second.RunID)
	if secondEvidence.Status != "completed" || secondEvidence.Trigger != "manual" {
		t.Fatalf("第二次终态 = %s / %s，期望 completed / manual", secondEvidence.Status, secondEvidence.Trigger)
	}
	f.requireNotifyCount(t, second.RunID, 1, 2)

	// —— 第三次：空检索结果，结论句只许是「本次无新增」 ——
	f.setToolResult(t, demoGitHubSpecEnv, "search_repositories", `{"total_count":0,"items":[]}`)
	f.model.beginRun(dir, d1Steps(f, "c")...)
	third := f.runScheduleNowOnce(t, taskID)
	thirdOut := decodeDemoScript(t, demoShellResults(t, f.h, third.RunID)[0])
	if thirdOut.Counts["candidates"] != float64(0) || thirdOut.Counts["recommended"] != float64(0) {
		t.Fatalf("空输入的计数不符：%#v", thirdOut.Counts)
	}
	if thirdOut.Message != "本次无新增：搜索结果为空" {
		t.Fatalf("空输入结论句 = %q", thirdOut.Message)
	}
	if body := f.recv.received(demoReplyPath)[2]; !strings.Contains(body, "本次无新增") {
		t.Fatalf("空输入的第三次投递应含「本次无新增」，实际 %q", body)
	}
	f.requireNotifyCount(t, third.RunID, 1, 3)

	// —— 第四次：检索失败，报告写「来源不可达」且不跑脚本 ——
	f.setToolError(t, demoGitHubSpecEnv, "search_repositories", "HTTP 503：github.com 不可达")
	f.model.beginRun(dir, d1FailureSteps(f, time.Now().In(demoCST()).Format(time.RFC3339), "d")...)
	fourth := f.runScheduleNowOnce(t, taskID)
	fourthEvidence := readDemoEvidence(t, f.h, fourth.RunID)
	if fourthEvidence.Failed == 0 {
		t.Fatalf("失败轮应有失败的工具调用行")
	}
	if shells := demoShellResults(t, f.h, fourth.RunID); len(shells) != 0 {
		t.Fatalf("依赖失败时不得跑脚本，实际 %d 次", len(shells))
	}
	if fourthEvidence.Status != "completed" {
		t.Fatalf("工具失败后 Run 应正常收尾为 completed，实际 %s", fourthEvidence.Status)
	}
	if failReport := readDemoFile(t, dir, "failure-report.md"); !strings.Contains(failReport, "来源不可达") {
		t.Fatalf("失败报告应写「来源不可达」：\n%s", failReport)
	}
	f.requireNotifyCount(t, fourth.RunID, 1, 4)

	// 六段证据的「计划与时区」：任务仍挂在声明的时区上，cron 是本次生成的六段式。
	schedules := listSchedules(t, f.addr)
	if len(schedules) != 1 || schedules[0].ID != taskID {
		t.Fatalf("任务清单 = %#v，期望只有 %s", schedules, taskID)
	}
	if schedules[0].Zone != "Asia/Shanghai" {
		t.Fatalf("任务时区 = %q，期望 Asia/Shanghai", schedules[0].Zone)
	}
	if schedules[0].Cron != cronAt(auto) {
		t.Fatalf("任务 cron = %q，期望 %q", schedules[0].Cron, cronAt(auto))
	}
}

// ============================================================
// D2 repo-digest：技能三级披露 + 系统代投 + 无变化
// ============================================================

const (
	// d2SkillL2Sentinel / d2SkillL3Sentinel 是技能正文里的固定标题：它们**只能**经
	// `read_file` 按卡片路径读到，绝不能出现在系统提示词里——这就是「渐进披露」的判据面。
	// The L2/L3 sentinels must only ever arrive through read_file, never preloaded.
	d2SkillL2Sentinel = "# 仓库日报技能（L2 正文）"
	d2SkillL3Sentinel = "# 仓库日报参考（L3）"
)

// d2FixHashes 是三份固定输入的 sha256（`sha256sum examples/daily/repo-digest/fixtures/*.json`
// 实测）。固定输入一变，计数与日报就跟着变：剧本必须显式重写，不能悄悄漂移。
// d2FixHashes pins the shipped fixtures' digests.
var d2FixHashes = map[string]string{
	"pull_requests": "b17ad886830f66a6fcad2e43f1eb8d7900a0585828c82dbea300bad3f68e51a1",
	"issues":        "c1688f9cfed8ab0d94ed7e360ef7c98d3ed02457834fde9b2de8eb7f486127cf",
	"releases":      "c2b012efbb0778b61380e7cf7206de106f1c84c737a3ab8ce0fe5fd83061fbbb",
}

// lastToolTexts 取某工具**最近** n 条结果。定时任务的会话 id 跨运行恒定、历史累积，
// 同名工具的旧结果也在请求里——只有按尾部取才是本轮的。
// lastToolTexts returns the latest n results of one tool: the scheduler session keeps its
// history across runs, so only the tail belongs to the current run.
func (d *demoTurn) lastToolTexts(name string, n int) []string {
	all := d.toolTexts(name)
	if len(all) < n {
		d.failf("%s 只有 %d 条结果，期望至少 %d 条", name, len(all), n)
	}
	return all[len(all)-n:]
}

// demoDigestOut 是 D2 脚本 stdout 的外形（counts 按来源嵌套）。
// demoDigestOut is the D2 stdout shape; counts are nested per source.
type demoDigestOut struct {
	ObservedAt  string                    `json:"observed_at"`
	Input       map[string]demoDigest     `json:"input"`
	Report      demoDigest                `json:"report"`
	State       demoDigest                `json:"state"`
	Counts      map[string]map[string]int `json:"counts"`
	Followups   int                       `json:"followups"`
	Unconfirmed int                       `json:"unconfirmed"`
	Cursor      struct {
		Advanced map[string]bool   `json:"advanced"`
		Next     map[string]string `json:"next"`
	} `json:"cursor"`
	Message string `json:"message"`
}

// decodeD2 解 D2 的 shell 结果。
// decodeD2 decodes the D2 shell result.
func decodeD2(f demoFatal, result string) demoDigestOut {
	f.Helper()
	stdout := demoShellStdout(f, result)
	var out demoDigestOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		f.Fatalf("D2 脚本 stdout 不是预期 JSON: %v（原文 %s）", err, stdout)
	}
	return out
}

// d2Steps 组装一次 D2 运行：按卡片读 L2 → 读同目录 L3 → 拉三份列表 → 原样落盘 →
// 跑脚本 → 读产物 → 写日报 → 以日报正文收尾（系统按 deliver_to 代投这条正文）。
// d2Steps builds one D2 run; the closing text is the digest body the system delivers.
func d2Steps(f *demoFixture, runTag string) []demoStep {
	var (
		l2         string
		digestBody string
		out        demoDigestOut
	)
	next := func(index int, step string) string {
		return fmt.Sprintf("d2-%s-%d-%s", runTag, index, step)
	}
	listArgs := map[string]any{"owner": "northwind-labs", "repo": "mcp-bridge", "state": "all"}
	return []demoStep{
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			d.requireNoTool("notify", "D2 基线 notify_mode: none，投递由系统按 deliver_to 代发")
			l2 = d.skillCard("repo-digest")
			d.must(!strings.Contains(d.systemText(), d2SkillL2Sentinel),
				"系统提示词预载了技能正文 %q——L1 只该给卡片", d2SkillL2Sentinel)
			return demoCall(next(1, "l2"), "read_file", map[string]any{"path": l2})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "read_file", "第 1 步应是读技能 L2，实际 %q", name)
			d.must(strings.Contains(d.lastToolText(), d2SkillL2Sentinel),
				"L2 正文缺少标题哨兵 %q", d2SkillL2Sentinel)
			l3 := strings.TrimSuffix(l2, "/SKILL.md") + "/REFERENCE.md"
			return demoCall(next(2, "l3"), "read_file", map[string]any{"path": l3})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "read_file", "第 2 步应是读技能 L3，实际 %q", name)
			d.must(strings.Contains(d.lastToolText(), d2SkillL3Sentinel),
				"L3 正文缺少标题哨兵 %q", d2SkillL3Sentinel)
			return demoCall(next(3, "pull-requests"), "list_pull_requests", listArgs)
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "list_pull_requests", "第 3 步应是拉 PR 列表，实际 %q", name)
			return demoCall(next(4, "issues"), "list_issues", listArgs)
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "list_issues", "第 4 步应是拉 Issue 列表，实际 %q", name)
			return demoCall(next(5, "releases"), "list_releases", listArgs)
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "list_releases", "第 5 步应是拉 Release 列表，实际 %q", name)
			return demoCall(next(6, "write-prs"), "write_file", map[string]any{
				"path":    "pull_requests.json",
				"content": d.lastToolTexts("list_pull_requests", 1)[0],
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 6 步应是落盘 PR 列表，实际 %q", name)
			return demoCall(next(7, "write-issues"), "write_file", map[string]any{
				"path":    "issues.json",
				"content": d.lastToolTexts("list_issues", 1)[0],
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 7 步应是落盘 Issue 列表，实际 %q", name)
			return demoCall(next(8, "write-releases"), "write_file", map[string]any{
				"path":    "releases.json",
				"content": d.lastToolTexts("list_releases", 1)[0],
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 8 步应是落盘 Release 列表，实际 %q", name)
			// 参数逐项照 AGENT.md 第 4 步；executable 用夹具放行的绝对路径（SETUP §7
			// 要求解释器与 interpreters 逐字一致）。
			return demoCall(next(9, "shell"), "shell", map[string]any{
				"executable": f.python,
				"arguments": []string{
					"scripts/repo_digest.py",
					"--pull-requests", "pull_requests.json",
					"--issues", "issues.json",
					"--releases", "releases.json",
					"--state", "state/cursor.json",
				},
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "shell", "第 9 步应是脚本，实际 %q", name)
			out = decodeD2(d, d.lastToolText())
			d.must(out.Report.Path != "", "脚本没有给出报告路径")
			return demoCall(next(10, "read"), "read_file", map[string]any{"path": out.Report.Path})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "read_file", "第 10 步应是读产物，实际 %q", name)
			content, err := d2DigestReport(d.lastToolText())
			if err != nil {
				d.failf("渲染 D2 日报失败：%v", err)
			}
			digestBody = content
			return demoCall(next(11, "write-digest"), "write_file", map[string]any{
				"path":    filepath.ToSlash(filepath.Dir(out.Report.Path)) + "/digest.md",
				"content": content,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 11 步应是写日报，实际 %q", name)
			d.must(digestBody != "", "日报正文为空——第 10 步没渲染成功")
			return demoFinal(digestBody)
		},
	}
}

// d2DigestReport 按技能 L2 的排版模板渲染日报：数字一律照抄 digest.json，空段写「无」。
// d2DigestReport renders the digest exactly per the skill's L2 template.
func d2DigestReport(raw string) (string, error) {
	var report struct {
		ObservedAt string `json:"observed_at"`
		// 每源的「待确认字段」计数在 `sources.<源>.counts` 里（与桶计数同一张表），
		// 不是源对象自己的顶层字段——按顶层解会静默得到 0。
		// The per-source unconfirmed count lives inside the source's `counts` map.
		Sources map[string]struct {
			New       int            `json:"new"`
			Processed int            `json:"processed"`
			Counts    map[string]int `json:"counts"`
		} `json:"sources"`
		Followups []struct {
			Source  string `json:"source"`
			Number  int    `json:"number"`
			Title   string `json:"title"`
			URL     string `json:"url"`
			Reason  string `json:"reason"`
			AgeDays int    `json:"age_days"`
		} `json:"followups"`
		Unconfirmed []struct {
			Source string `json:"source"`
			Number int    `json:"number"`
			Title  string `json:"title"`
			Field  string `json:"field"`
			Reason string `json:"reason"`
		} `json:"unconfirmed"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		return "", fmt.Errorf("解析 D2 脚本产物失败：%w（原文 %q）", err, raw)
	}
	followed := map[string]int{}
	for _, item := range report.Followups {
		followed[item.Source]++
	}
	date := report.ObservedAt
	if len(date) > 10 {
		date = date[:10]
	}
	var b strings.Builder
	b.WriteString("# 仓库日报 · northwind-labs/mcp-bridge · " + date + "\n\n")
	b.WriteString("结论：" + report.Message + "\n\n")
	b.WriteString("## 计数\n")
	b.WriteString("| 来源 | 新增 | 计入 | 待跟进 | 待确认字段 |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, source := range []struct{ key, label string }{
		{"pull_requests", "PR"},
		{"issues", "Issue"},
		{"releases", "Release"},
	} {
		entry := report.Sources[source.key]
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %d | %d |\n",
			source.label, entry.New, entry.Processed, followed[source.key],
			entry.Counts["with_unconfirmed_fields"]))
	}
	b.WriteString("\n## 待跟进\n")
	if len(report.Followups) == 0 {
		b.WriteString("无\n")
	}
	for _, item := range report.Followups {
		b.WriteString(fmt.Sprintf("- [%s #%d] %s —— %s（%d 天）%s\n",
			item.Source, item.Number, item.Title, item.Reason, item.AgeDays, item.URL))
	}
	b.WriteString("\n## 待确认\n")
	if len(report.Unconfirmed) == 0 {
		b.WriteString("无\n")
	}
	for _, item := range report.Unconfirmed {
		b.WriteString(fmt.Sprintf("- [%s #%d] %s —— 字段 `%s`：%s\n",
			item.Source, item.Number, item.Title, item.Field, item.Reason))
	}
	return b.String(), nil
}

// testDemoRepoDigest 覆盖 D2：L1 卡片只给路径（L2 / L3 由模型自取）、系统按 deliver_to
// 代投且工具面没有 notify、自动触发与人工补跑、以及「本次无新增」这一空形态。
// testDemoRepoDigest covers D2 end to end.
func testDemoRepoDigest(t *testing.T) {
	f := newDemoFixture(t)
	for _, source := range []string{"pull_requests", "issues", "releases"} {
		f.setToolResult(t, demoGitHubSpecEnv, "list_"+source,
			f.fixtureJSON(t, "examples/daily/repo-digest/fixtures/"+source+".json"))
	}
	modelUID, channelUID := f.modelUID(t), f.channelUID(t)
	base := time.Now()
	auto := base.Add(15 * time.Second).In(demoCST())
	f.installSkill(t, "repo-digest", "repo-digest")
	// D2 走系统代投：`notify_mode: none` 与空 `notify_channels` 下**不得**写渠道绑定
	// （绑定必须与声明双向精确相等，否则准入报 resource unavailable）；渠道只在
	// 任务的 deliver_to 上出现。
	// D2 relies on system delivery: with notify_mode none the bindings stay empty.
	dir := f.installAgent(t, "repo-digest", demoOptions{
		deliverTo: channelUID,
		cronAt:    auto,
		modelUID:  modelUID,
		cronOld:   "0 45 8 * * *",
	})
	const taskID = "daily-digest"

	f.model.beginRun(dir, d2Steps(f, "a")...)
	serve := f.serve(t)
	defer serve.stop(t)

	// —— 第一次：自动触发，三源都有新条目 ——
	firstRun := f.waitCronRun(t, taskID, 120*time.Second)
	first := decodeD2(t, demoShellResults(t, f.h, firstRun)[0])
	if first.Message != "本次处理 PR 6 / Issue 5 / Release 4 项，待跟进 3 项，待确认 3 项" {
		t.Fatalf("首轮结论句 = %q", first.Message)
	}
	if first.Followups != 3 || first.Unconfirmed != 3 {
		t.Fatalf("待跟进 / 待确认 = %d / %d，期望 3 / 3", first.Followups, first.Unconfirmed)
	}
	for bucket, want := range map[string]int{
		"open": 2, "draft_open": 1, "merged": 1, "closed_unmerged": 1, "unknown": 1,
		"with_unconfirmed_fields": 1,
	} {
		if got := first.Counts["pull_requests"][bucket]; got != want {
			t.Fatalf("PR 桶 %s = %d，期望 %d（全部桶 %#v）", bucket, got, want, first.Counts["pull_requests"])
		}
	}
	for _, source := range []string{"pull_requests", "issues", "releases"} {
		digest := first.Input[source]
		if digest.SHA256 != d2FixHashes[source] {
			t.Fatalf("%s 输入哈希 %s 与固定输入的实测值 %s 不一致（输入被改过？）",
				source, digest.SHA256, d2FixHashes[source])
		}
		requireDigestOnDisk(t, dir, digest, "D2 输入 "+source)
		if !first.Cursor.Advanced[source] {
			t.Fatalf("首轮游标 %s 未推进：%#v", source, first.Cursor.Advanced)
		}
	}
	requireDigestOnDisk(t, dir, first.Report, "D2 报告")
	requireDigestOnDisk(t, dir, first.State, "D2 游标")

	reportDir := filepath.ToSlash(filepath.Dir(first.Report.Path))
	markdown := readDemoFile(t, dir, reportDir+"/digest.md")
	for _, want := range []string{
		"# 仓库日报 · northwind-labs/mcp-bridge · " + first.ObservedAt[:10],
		"结论：" + first.Message,
		"| PR | 6 | 6 | 1 | 1 |",
		"| Issue | 5 | 5 | 2 | 1 |",
		"| Release | 4 | 4 | 0 | 1 |",
		"- [pull_requests #2]", "stale_open",
		"- [issues #12]", "unanswered_open",
		"- [issues #13]", "long_open",
		"字段 `merged_at`：missing",
		"字段 `comments`：missing",
		"字段 `published_at`：missing",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("日报缺少 %q：\n%s", want, markdown)
		}
	}

	// 三级披露的取用路径：前两次 read_file 都落在卡片给出的**发布目录**里，第二次读的是
	// 同一目录的 REFERENCE.md（L3）——不是工作副本，也不是凭记忆写的格式；第三次是模型
	// 回读本轮产物 digest.json（L2 模板要求数字照抄产物，不许凭记忆写）。
	// The first two reads come from the published skill dir; the third is the run artifact.
	reads := demoToolPaths(t, f.h, firstRun, "read_file")
	if len(reads) != 3 {
		t.Fatalf("首轮 read_file %d 次，期望 3 次（L2 + L3 + 产物）：%v", len(reads), reads)
	}
	// 产物路径由脚本按本机分隔符拼出（Windows 是反斜杠），比对前先归一。
	if !strings.HasSuffix(filepath.ToSlash(reads[2]), "/digest.json") {
		t.Fatalf("第 3 次 read_file 应回读产物 digest.json，实际 %q", reads[2])
	}
	if !strings.HasPrefix(reads[0], ".basalt/versions/skills/repo-digest/") ||
		!strings.HasSuffix(reads[0], "/SKILL.md") {
		t.Fatalf("第 1 次 read_file 应读发布目录里的 SKILL.md，实际 %q", reads[0])
	}
	if !strings.HasSuffix(reads[1], "/REFERENCE.md") {
		t.Fatalf("第 2 次 read_file 应读 L3 的 REFERENCE.md，实际 %q", reads[1])
	}
	if strings.TrimSuffix(reads[0], "/SKILL.md") != strings.TrimSuffix(reads[1], "/REFERENCE.md") {
		t.Fatalf("L2 与 L3 应来自同一发布目录，实际 %q / %q", reads[0], reads[1])
	}

	evidence := readDemoEvidence(t, f.h, firstRun)
	if evidence.Status != "completed" || evidence.Trigger != "cron" {
		t.Fatalf("首轮终态 = %s / %s，期望 completed / cron", evidence.Status, evidence.Trigger)
	}
	if evidence.ToolCalls != 11 {
		t.Fatalf("首轮工具调用 %d 次，期望 11 次（剧本写死）", evidence.ToolCalls)
	}
	// 零 notify + 一条代投：D2 的通知走系统代投，模型一次都没调 notify。
	// 代投是运行收尾之后的独立一步，先等投递行落终态再数接收端（见 waitDeliveryStatus）。
	waitDeliveryStatus(t, f.h, firstRun, "sent", demoDeliveryTimeout)
	f.requireNotifyCount(t, firstRun, 0, 1)
	if status := demoDeliveryStatus(t, f.h, firstRun); status != "sent" {
		t.Fatalf("D2 的系统代投状态 = %q，期望 sent", status)
	}
	delivered := demoDeliveryText(t, f.recv.received(demoReplyPath)[0])
	if !strings.HasPrefix(delivered, "【repo-digest】定时任务 daily-digest 执行完成\n\n") {
		t.Fatalf("代投正文前缀不符：%q", delivered)
	}
	if !strings.Contains(delivered, first.Message) {
		t.Fatalf("代投正文应含日报结论句：%q", delivered)
	}

	// —— 第二次：人工补跑，三源都没有新条目 ——
	f.model.beginRun(dir, d2Steps(f, "b")...)
	second := f.runScheduleNowOnce(t, taskID)
	secondOut := decodeD2(t, demoShellResults(t, f.h, second.RunID)[0])
	if secondOut.Message != "本次无新增：三个来源都没有晚于游标的新条目" {
		t.Fatalf("第二轮结论句 = %q", secondOut.Message)
	}
	for _, source := range []string{"pull_requests", "issues", "releases"} {
		if secondOut.Cursor.Advanced[source] {
			t.Fatalf("第二轮游标 %s 仍推进（无新增时不该动）：%#v", source, secondOut.Cursor.Advanced)
		}
	}
	requireDigestOnDisk(t, dir, secondOut.State, "D2 第二轮游标")
	secondMarkdown := readDemoFile(t, dir,
		filepath.ToSlash(filepath.Dir(secondOut.Report.Path))+"/digest.md")
	if !strings.Contains(secondMarkdown, secondOut.Message) {
		t.Fatalf("第二轮日报应照抄「本次无新增」：\n%s", secondMarkdown)
	}
	if strings.Contains(secondMarkdown, "[pull_requests #2]") {
		t.Fatalf("第二轮日报不得再列上一轮的待跟进项：\n%s", secondMarkdown)
	}
	secondEvidence := readDemoEvidence(t, f.h, second.RunID)
	if secondEvidence.Status != "completed" || secondEvidence.Trigger != "manual" {
		t.Fatalf("第二轮终态 = %s / %s，期望 completed / manual",
			secondEvidence.Status, secondEvidence.Trigger)
	}
	waitDeliveryStatus(t, f.h, second.RunID, "sent", demoDeliveryTimeout)
	f.requireNotifyCount(t, second.RunID, 0, 2)
	if status := demoDeliveryStatus(t, f.h, second.RunID); status != "sent" {
		t.Fatalf("第二轮的系统代投状态 = %q，期望 sent", status)
	}
	if body := f.recv.received(demoReplyPath)[1]; !strings.Contains(body, "本次无新增") {
		t.Fatalf("第二轮的代投正文应含「本次无新增」，实际 %q", body)
	}

	// 六段证据的「计划与时区」。
	schedules := listSchedules(t, f.addr)
	if len(schedules) != 1 || schedules[0].ID != taskID {
		t.Fatalf("任务清单 = %#v，期望只有 %s", schedules, taskID)
	}
	if schedules[0].Zone != "Asia/Shanghai" || schedules[0].Cron != cronAt(auto) {
		t.Fatalf("任务计划 = %q / %q，期望 Asia/Shanghai / %q",
			schedules[0].Zone, schedules[0].Cron, cronAt(auto))
	}
}

// ============================================================
// D3 biz-daily：只读统计 + 三类坏行计数 + 零通知
// ============================================================

const (
	// d3SQL 逐字照抄 AGENT.md 第 2 步的固定 SQL：少一列或多一个 WHERE，坏行计数就会
	// 悄悄归零——「数据干净」与「被过滤掉」必须能被判卷分开。
	// d3SQL is the fixed SQL from AGENT.md step 2, verbatim.
	d3SQL = "SELECT row_id, order_id, order_date, region, channel, amount, status, note\n" +
		"FROM orders ORDER BY row_id;"
	// d3FixtureHash 是固定输入 `fixtures/rows.json` 的 sha256（实测）。
	// d3FixtureHash pins the shipped orders fixture digest.
	d3FixtureHash = "d64d446df5c4ccf4044fc01999ab05e71de37a0f97cff817d439ac7d4f6cfaf6"
)

// demoBizExample 是一行「需人工确认」明细（缺失与非法值共用外形）。
// demoBizExample is one human-review row (missing and invalid share it).
type demoBizExample struct {
	Row     int      `json:"row"`
	OrderID string   `json:"order_id"`
	Fields  []string `json:"fields"`
	Reasons []string `json:"reasons"`
}

// demoBizDuplicate 是一条重复行读数（首次出现在哪一行）。
// demoBizDuplicate is one duplicate reading with its first row.
type demoBizDuplicate struct {
	Row      int    `json:"row"`
	FirstRow int    `json:"first_row"`
	OrderID  string `json:"order_id"`
}

// demoBizRegion 是一个大区的净额与单量。
// demoBizRegion is one region's net amount and order count.
type demoBizRegion struct {
	Region    string  `json:"region"`
	Orders    int     `json:"orders"`
	NetAmount float64 `json:"net_amount"`
}

// demoBizMetrics 是只用有效行算出的经营指标。
// demoBizMetrics holds the metrics computed from valid rows only.
type demoBizMetrics struct {
	Computed     bool            `json:"computed"`
	Currency     string          `json:"currency"`
	Gross        float64         `json:"gross_amount"`
	Refunded     float64         `json:"refunded_amount"`
	Pending      float64         `json:"pending_amount"`
	Net          float64         `json:"net_amount"`
	AveragePaid  float64         `json:"average_paid_amount"`
	ValidOrders  int             `json:"valid_orders"`
	StatusCounts map[string]int  `json:"status_counts"`
	ByRegion     []demoBizRegion `json:"by_region"`
}

// demoBizQuality 是四类互斥计数的明细（有效 / 缺失 / 重复 / 非法值）。
// demoBizQuality is the four-way exclusive count breakdown.
type demoBizQuality struct {
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
	Counts    map[string]int `json:"counts"`
	Missing   struct {
		ByField  map[string]int   `json:"by_field"`
		Examples []demoBizExample `json:"examples"`
	} `json:"missing"`
	Duplicate struct {
		OrderIDs []string           `json:"order_ids"`
		Examples []demoBizDuplicate `json:"examples"`
	} `json:"duplicate"`
	Invalid struct {
		ByReason map[string]int   `json:"by_reason"`
		Examples []demoBizExample `json:"examples"`
	} `json:"invalid"`
}

// demoBizOut 是 D3 脚本 stdout 的外形。D3 无跨轮状态：`state` 必须缺席（`nil`）。
// demoBizOut is the D3 stdout shape; the demo keeps no cursor, so `state` must be nil.
type demoBizOut struct {
	ObservedAt string         `json:"observed_at"`
	Input      demoDigest     `json:"input"`
	Report     demoDigest     `json:"report"`
	State      *demoDigest    `json:"state"`
	Quality    demoBizQuality `json:"quality"`
	Metrics    demoBizMetrics `json:"metrics"`
	Message    string         `json:"message"`
}

// decodeD3 解 D3 的 shell 结果。
// decodeD3 decodes the D3 shell result.
func decodeD3(f demoFatal, result string) demoBizOut {
	f.Helper()
	stdout := demoShellStdout(f, result)
	var out demoBizOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		f.Fatalf("D3 脚本 stdout 不是预期 JSON: %v（原文 %s）", err, stdout)
	}
	return out
}

// decodeD3Artifact 解 `runs/<时间戳>/daily.json` 正文：脚本 stdout 只给摘要
// （counts / total / 三项指标），四类坏行明细与完整指标只在这份产物里。
// decodeD3Artifact decodes the full daily.json artifact; the script's stdout carries
// only the summary view, so the per-class detail lives here.
func decodeD3Artifact(f demoFatal, body string) demoBizOut {
	f.Helper()
	var out demoBizOut
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		f.Fatalf("D3 产物不是预期 JSON: %v（原文 %s）", err, body)
	}
	return out
}

// demoMoney 按 JSON 里的精确值渲染金额（脚本只做统计，不四舍五入——判卷侧也不）。
// demoMoney renders a JSON number exactly as the script produced it.
func demoMoney(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// demoScoreMap 把「键 → 条数」按字典序渲染成一行（渲染顺序不能靠 map 遍历）。
// demoScoreMap renders a key→count map in sorted order, never by map iteration.
func demoScoreMap(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", key, counts[key]))
	}
	return strings.Join(parts, " / ")
}

// d3DailyReport 按 AGENT.md 第 5 步的模板渲染经营日报：数字照抄脚本输出，
// 坏行逐类列出，方便回源头修数据。
// d3DailyReport renders the daily report per the AGENT.md step-5 template.
func d3DailyReport(out demoBizOut) string {
	var b strings.Builder
	b.WriteString("# 经营日报 · " + out.ObservedAt + "\n\n")
	b.WriteString("结论：" + out.Message + "\n\n")
	b.WriteString("## 数据质量\n")
	b.WriteString("| 类别 | 行数 | 说明 |\n")
	b.WriteString("| --- | --- | --- |\n")
	b.WriteString(fmt.Sprintf("| 有效 | %d | 进入指标 |\n", out.Quality.Counts["valid"]))
	b.WriteString(fmt.Sprintf("| 缺失 | %d | %s |\n",
		out.Quality.Counts["missing"], demoScoreMap(out.Quality.Missing.ByField)))
	b.WriteString(fmt.Sprintf("| 重复 | %d | %s |\n",
		out.Quality.Counts["duplicate"], strings.Join(out.Quality.Duplicate.OrderIDs, "、")))
	b.WriteString(fmt.Sprintf("| 非法值 | %d | %s |\n",
		out.Quality.Counts["invalid"], demoScoreMap(out.Quality.Invalid.ByReason)))
	b.WriteString("\n## 经营指标\n")
	if !out.Metrics.Computed {
		b.WriteString("本次没有可用行，指标未计算\n")
	} else {
		b.WriteString(fmt.Sprintf("- 毛额 %s CNY；退款 %s；挂起 %s；净额 %s\n",
			demoMoney(out.Metrics.Gross), demoMoney(out.Metrics.Refunded),
			demoMoney(out.Metrics.Pending), demoMoney(out.Metrics.Net)))
		b.WriteString("- 客单价（已付） " + demoMoney(out.Metrics.AveragePaid) + " " +
			out.Metrics.Currency + "\n")
		regions := make([]string, 0, len(out.Metrics.ByRegion))
		for _, region := range out.Metrics.ByRegion {
			regions = append(regions, fmt.Sprintf("%s 净额 %s（%d 单）",
				region.Region, demoMoney(region.NetAmount), region.Orders))
		}
		b.WriteString("- 分区：" + strings.Join(regions, "；") + "\n")
	}
	b.WriteString("\n## 需人工确认\n")
	for _, item := range out.Quality.Missing.Examples {
		b.WriteString(fmt.Sprintf("- [缺失] 第 %d 行 %s：%s\n",
			item.Row, item.OrderID, strings.Join(item.Fields, "、")))
	}
	for _, item := range out.Quality.Duplicate.Examples {
		b.WriteString(fmt.Sprintf("- [重复] 第 %d 行 %s（首次在第 %d 行）\n",
			item.Row, item.OrderID, item.FirstRow))
	}
	for _, item := range out.Quality.Invalid.Examples {
		b.WriteString(fmt.Sprintf("- [非法值] 第 %d 行 %s：%s（%s）\n",
			item.Row, item.OrderID, strings.Join(item.Fields, "、"),
			strings.Join(item.Reasons, "、")))
	}
	return b.String()
}

// d3Steps 组装一次 D3 运行：连库自检 → 固定 SQL → 原样落盘 → 跑脚本 → 读产物 →
// 写日报 → 以日报正文收尾。工具面没有 notify（零通知是 D3 的口径）。
// d3Steps builds one D3 run; the notify tool is absent by design.
func d3Steps(f *demoFixture, runTag string) []demoStep {
	var (
		out       demoBizOut
		dailyBody string
		next      = func(index int, step string) string {
			return fmt.Sprintf("d3-%s-%d-%s", runTag, index, step)
		}
	)
	return []demoStep{
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			d.requireNoTool("notify", "D3 基线 notify_mode: none，工具面不该有 notify（零通知口径）")
			return demoCall(next(1, "list-tables"), "list_tables", map[string]any{})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "list_tables", "第 1 步应是连库自检，实际 %q", name)
			return demoCall(next(2, "run-query"), "run_query", map[string]any{"sql": d3SQL})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "run_query", "第 2 步应是固定 SQL，实际 %q", name)
			return demoCall(next(3, "write-orders"), "write_file", map[string]any{
				"path":    "orders.json",
				"content": d.lastToolTexts("run_query", 1)[0],
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 3 步应是落盘明细，实际 %q", name)
			return demoCall(next(4, "shell"), "shell", map[string]any{
				"executable": f.python,
				"arguments":  []string{"scripts/biz_daily.py", "--orders", "orders.json"},
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "shell", "第 4 步应是脚本，实际 %q", name)
			out = decodeD3(d, d.lastToolText())
			d.must(out.Report.Path != "", "脚本没有给出报告路径")
			return demoCall(next(5, "read-daily"), "read_file", map[string]any{"path": out.Report.Path})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "read_file", "第 5 步应是读产物，实际 %q", name)
			// 日报要照抄四类坏行明细与全部指标——它们在产物里，不在 stdout 摘要里。
			// The report quotes the per-class detail and full metrics, which live in the
			// artifact rather than in the stdout summary.
			dailyBody = d3DailyReport(decodeD3Artifact(d, d.lastToolText()))
			return demoCall(next(6, "write-daily"), "write_file", map[string]any{
				"path":    filepath.ToSlash(filepath.Dir(out.Report.Path)) + "/daily.md",
				"content": dailyBody,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 6 步应是写日报，实际 %q", name)
			d.must(dailyBody != "", "日报正文为空——第 5 步没渲染成功")
			return demoFinal(dailyBody)
		},
	}
}

// d3RequireNumbers 断言一轮 D3 的业务读数：四类计数、三类坏行的明细与全部指标。
// 自动与人工两轮共用同一份判据——数字对不上就是脚本或输入变了。
// d3RequireNumbers asserts the business readings shared by both D3 runs.
func d3RequireNumbers(t *testing.T, out demoBizOut) {
	t.Helper()
	if out.Message != "本次 24 行中有效 15 行（缺失 3 / 重复 2 / 非法 4），净额 10700.50 CNY" {
		t.Fatalf("结论句 = %q", out.Message)
	}
	if out.Quality.Total != 24 || out.Quality.Truncated {
		t.Fatalf("质量总行数 / 截断 = %d / %v，期望 24 / false",
			out.Quality.Total, out.Quality.Truncated)
	}
	for category, want := range map[string]int{"valid": 15, "missing": 3, "duplicate": 2, "invalid": 4} {
		if got := out.Quality.Counts[category]; got != want {
			t.Fatalf("%s 计数 = %d，期望 %d（全部计数 %#v）", category, got, want, out.Quality.Counts)
		}
	}
	for field, want := range map[string]int{"amount": 1, "region": 1, "status": 1} {
		if got := out.Quality.Missing.ByField[field]; got != want {
			t.Fatalf("缺失字段 %s = %d，期望 %d", field, got, want)
		}
	}
	if got := strings.Join(out.Quality.Duplicate.OrderIDs, ","); got != "SO-1002,SO-1003" {
		t.Fatalf("重复单号 = %q，期望 SO-1002,SO-1003", got)
	}
	for reason, want := range map[string]int{
		"amount_negative": 1, "amount_not_numeric": 1,
		"order_date_unparseable": 1, "status_unknown": 1,
	} {
		if got := out.Quality.Invalid.ByReason[reason]; got != want {
			t.Fatalf("非法原因 %s = %d，期望 %d", reason, got, want)
		}
	}
	if len(out.Quality.Missing.Examples) != 3 || len(out.Quality.Duplicate.Examples) != 2 ||
		len(out.Quality.Invalid.Examples) != 4 {
		t.Fatalf("需人工确认明细条数 = %d / %d / %d，期望 3 / 2 / 4",
			len(out.Quality.Missing.Examples), len(out.Quality.Duplicate.Examples),
			len(out.Quality.Invalid.Examples))
	}
	metrics := out.Metrics
	if !metrics.Computed || metrics.Currency != "CNY" || metrics.ValidOrders != 15 {
		t.Fatalf("指标口径 = computed %v / %q / valid %d，期望 true / CNY / 15",
			metrics.Computed, metrics.Currency, metrics.ValidOrders)
	}
	for label, pair := range map[string][2]float64{
		"毛额":  {metrics.Gross, 11890.5},
		"退款":  {metrics.Refunded, 1190.0},
		"挂起":  {metrics.Pending, 2170.0},
		"净额":  {metrics.Net, 10700.5},
		"客单价": {metrics.AveragePaid, 1321.17},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("%s = %v，期望 %v", label, pair[0], pair[1])
		}
	}
	if metrics.StatusCounts["paid"] != 9 || metrics.StatusCounts["pending"] != 3 ||
		metrics.StatusCounts["refunded"] != 3 {
		t.Fatalf("状态计数 = %#v，期望 paid 9 / pending 3 / refunded 3", metrics.StatusCounts)
	}
	wantRegions := []demoBizRegion{
		{Region: "华东", Orders: 6, NetAmount: 3740.0},
		{Region: "华北", Orders: 5, NetAmount: 4290.5},
		{Region: "华南", Orders: 2, NetAmount: 1990.0},
		{Region: "西南", Orders: 2, NetAmount: 680.0},
	}
	if len(metrics.ByRegion) != len(wantRegions) {
		t.Fatalf("分区数 = %d，期望 %d（%#v）", len(metrics.ByRegion), len(wantRegions), metrics.ByRegion)
	}
	for i, want := range wantRegions {
		if metrics.ByRegion[i] != want {
			t.Fatalf("第 %d 个分区 = %#v，期望 %#v", i+1, metrics.ByRegion[i], want)
		}
	}
}

// testDemoBizDaily 覆盖 D3：固定 SQL 的全表取数、四类互斥计数、只用有效行算指标、
// **零通知**，以及自动触发与人工补跑给出同一组数字。
// testDemoBizDaily covers D3 end to end.
func testDemoBizDaily(t *testing.T) {
	f := newDemoFixture(t)
	f.setToolResult(t, demoDBHubSpecEnv, "list_tables", `{"tables":["orders"]}`)
	f.setToolResult(t, demoDBHubSpecEnv, "run_query",
		f.fixtureJSON(t, "examples/daily/biz-daily/fixtures/rows.json"))
	modelUID := f.modelUID(t)
	auto := time.Now().Add(15 * time.Second).In(demoCST())
	dir := f.installAgent(t, "biz-daily", demoOptions{
		cronAt:   auto,
		modelUID: modelUID,
		cronOld:  "0 0 9 * * *",
	})
	const taskID = "daily-report"

	f.model.beginRun(dir, d3Steps(f, "a")...)
	serve := f.serve(t)
	defer serve.stop(t)

	// —— 第一次：自动触发 ——
	firstRun := f.waitCronRun(t, taskID, 120*time.Second)
	// stdout 是摘要视图，四类坏行明细与完整指标在 runs/<时间戳>/daily.json；
	// 判据落在产物上，再用摘要做一次一致性交叉核对。
	// The stdout is a summary; the detail lives in daily.json, so assertions read the
	// artifact and cross-check the summary against it.
	summary := decodeD3(t, demoShellResults(t, f.h, firstRun)[0])
	first := decodeD3Artifact(t, demoToolResults(t, f.h, firstRun, "read_file")[0])
	// 产物是日报正文，不含自引用摘要（`report` / `state` 只在 stdout 摘要里），
	// 因此逐项比对两者共有的字段。
	// The artifact is the report body itself, so the cross-check covers shared fields only.
	if summary.Message != first.Message || summary.ObservedAt != first.ObservedAt ||
		summary.Input.SHA256 != first.Input.SHA256 {
		t.Fatalf("stdout 摘要与 daily.json 不一致：摘要 %+v / 产物 %+v", summary, first)
	}
	for _, category := range []string{"valid", "missing", "duplicate", "invalid"} {
		if summary.Quality.Counts[category] != first.Quality.Counts[category] {
			t.Fatalf("stdout 摘要的 %s 计数 %d 与产物 %d 不一致",
				category, summary.Quality.Counts[category], first.Quality.Counts[category])
		}
	}
	d3RequireNumbers(t, first)
	if first.Input.SHA256 != d3FixtureHash {
		t.Fatalf("输入哈希 %s 与固定输入的实测值 %s 不一致（输入被改过？）",
			first.Input.SHA256, d3FixtureHash)
	}
	requireDigestOnDisk(t, dir, first.Input, "D3 输入")
	requireDigestOnDisk(t, dir, summary.Report, "D3 报告")
	// D3 没有跨轮状态：脚本若开始写 state，说明口径变了（摘要与产物两处都必须缺席）。
	if summary.State != nil || first.State != nil {
		t.Fatalf("D3 不应有 state，实际 %#v / %#v", summary.State, first.State)
	}

	daily := readDemoFile(t, dir, filepath.ToSlash(filepath.Dir(summary.Report.Path))+"/daily.md")
	for _, want := range []string{
		"# 经营日报 · " + first.ObservedAt,
		"结论：" + first.Message,
		"| 有效 | 15 | 进入指标 |",
		"- 毛额 11890.5 CNY",
		"- 客单价（已付） 1321.17 CNY",
		"- [缺失] 第 7 行 SO-1007：region",
		"- [重复] 第 9 行 SO-1003（首次在第 3 行）",
		"- [非法值] 第 5 行 SO-1005：amount（amount_not_numeric）",
	} {
		if !strings.Contains(daily, want) {
			t.Fatalf("日报缺少 %q：\n%s", want, daily)
		}
	}

	evidence := readDemoEvidence(t, f.h, firstRun)
	if evidence.Status != "completed" || evidence.Trigger != "cron" {
		t.Fatalf("首轮终态 = %s / %s，期望 completed / cron", evidence.Status, evidence.Trigger)
	}
	if evidence.ToolCalls != 6 {
		t.Fatalf("首轮工具调用 %d 次，期望 6 次（剧本写死）", evidence.ToolCalls)
	}
	// 零通知：Run 内 0 次 notify、接收端 0 次投递、也没有系统代投行。
	f.requireNotifyCount(t, firstRun, 0, 0)
	if status := demoDeliveryStatus(t, f.h, firstRun); status != "" {
		t.Fatalf("D3 不该有系统代投行，实际状态 %q", status)
	}

	// —— 第二次：人工补跑，同一输入必须给出同一组数字 ——
	f.model.beginRun(dir, d3Steps(f, "b")...)
	second := f.runScheduleNowOnce(t, taskID)
	secondOut := decodeD3Artifact(t, demoToolResults(t, f.h, second.RunID, "read_file")[0])
	d3RequireNumbers(t, secondOut)
	if secondOut.Input.SHA256 != d3FixtureHash {
		t.Fatalf("第二轮输入哈希 %s 与固定输入不一致", secondOut.Input.SHA256)
	}
	f.requireNotifyCount(t, second.RunID, 0, 0)
	if status := demoDeliveryStatus(t, f.h, second.RunID); status != "" {
		t.Fatalf("第二轮不该有系统代投行，实际状态 %q", status)
	}

	// 六段证据的「计划与时区」。
	schedules := listSchedules(t, f.addr)
	if len(schedules) != 1 || schedules[0].ID != taskID {
		t.Fatalf("任务清单 = %#v，期望只有 %s", schedules, taskID)
	}
	if schedules[0].Zone != "Asia/Shanghai" || schedules[0].Cron != cronAt(auto) {
		t.Fatalf("任务计划 = %q / %q，期望 Asia/Shanghai / %q",
			schedules[0].Zone, schedules[0].Cron, cronAt(auto))
	}
}

// ============================================================
// D4 docs-diff：只读 MCP 三工具 + 记忆基线 + 三集合 + 全量哈希
// ============================================================

const (
	// d4RecallEmpty 是 recall_memory 零命中的固定占位文案（internal/tool 的同名字面量；
	// 判卷侧不复用内部常量，防止「文案改了但判据没改」）。
	// d4RecallEmpty is the fixed zero-hit recall placeholder.
	d4RecallEmpty = "(无匹配条目)"
	// d4NoMemoryText 是报告里「没有历史记忆」的写法（AGENT.md 第 1 步逐字）。
	// d4NoMemoryText is how the report words a missing memory baseline.
	d4NoMemoryText = "无历史记忆（首次运行）"
	// d4BaselineOnly 是首跑的变更摘要固定句：记录基线不等于「新增」。
	// d4BaselineOnly is the first-run summary sentence.
	d4BaselineOnly = "本次只记录基线，不判定新增 / 修改 / 删除"
	// d4MemoryPrefix 是记忆记录的前缀（下一次运行的「记忆基线」只认它）。
	// d4MemoryPrefix is the memory-record prefix the next run reads back.
	d4MemoryPrefix = "docs-diff 基线 "
)

// demoDocEntry 是 `list_documents` 返回的一项。
// demoDocEntry is one entry of the read-only listing.
type demoDocEntry struct {
	Path       string `json:"path"`
	Size       int    `json:"size"`
	ModifiedAt string `json:"modified_at"`
}

// demoDocMeta 是 `document_metadata` 返回的一项（哈希是变更判定的唯一依据）。
// demoDocMeta is one metadata entry; the hash alone decides the diff.
type demoDocMeta struct {
	Path       string `json:"path"`
	Size       int    `json:"size"`
	ModifiedAt string `json:"modified_at"`
	SHA256     string `json:"sha256"`
}

// demoDocCounts 是六类计数（current / baseline 是快照口径，其余按集合）。
// demoDocCounts holds the six counts of the diff.
type demoDocCounts struct {
	Current   int `json:"current"`
	Baseline  int `json:"baseline"`
	Added     int `json:"added"`
	Modified  int `json:"modified"`
	Deleted   int `json:"deleted"`
	Unchanged int `json:"unchanged"`
}

// demoDocSets 是新增 / 修改 / 删除 / 未变四个集合（由脚本排序后给出）。
// demoDocSets holds the four path sets.
type demoDocSets struct {
	Added     []string `json:"added"`
	Modified  []string `json:"modified"`
	Deleted   []string `json:"deleted"`
	Unchanged []string `json:"unchanged"`
}

// demoDocRecord 是 changes.json 里的一份文档读数：删除行没有本次哈希与大小。
// demoDocRecord is one per-document reading; deleted rows carry no current hash or size.
type demoDocRecord struct {
	Path           string  `json:"path"`
	Status         string  `json:"status"`
	SHA256         *string `json:"sha256"`
	PreviousSHA256 *string `json:"previous_sha256"`
	Size           *int    `json:"size"`
	PreviousSize   *int    `json:"previous_size"`
}

// demoDocReport 是 changes.json 的外形（D4 的唯一事实源：集合与每份哈希都从它取）。
// demoDocReport is the changes.json shape, the only source of truth for D4.
type demoDocReport struct {
	ObservedAt       string          `json:"observed_at"`
	Mode             string          `json:"mode"`
	BaselineMissing  bool            `json:"baseline_missing"`
	BaselineObserved *string         `json:"baseline_observed_at"`
	ChangesDetected  *bool           `json:"changes_detected"`
	Counts           demoDocCounts   `json:"counts"`
	Sets             demoDocSets     `json:"sets"`
	Documents        []demoDocRecord `json:"documents"`
	Message          string          `json:"message"`
}

// demoDocsOut 是 D4 脚本 stdout 的外形（input 是两个文件，故用映射）。
// demoDocsOut is the D4 stdout shape.
type demoDocsOut struct {
	ObservedAt      string                `json:"observed_at"`
	Mode            string                `json:"mode"`
	BaselineMissing bool                  `json:"baseline_missing"`
	ChangesDetected *bool                 `json:"changes_detected"`
	Input           map[string]demoDigest `json:"input"`
	Report          demoDigest            `json:"report"`
	State           demoDigest            `json:"state"`
	Counts          demoDocCounts         `json:"counts"`
	Sets            demoDocSets           `json:"sets"`
	Message         string                `json:"message"`
}

// decodeD4 解 D4 的 shell 结果（stdout）。
// decodeD4 decodes the D4 shell result.
func decodeD4(f demoFatal, result string) demoDocsOut {
	f.Helper()
	stdout := demoShellStdout(f, result)
	var out demoDocsOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		f.Fatalf("D4 脚本 stdout 不是预期 JSON: %v（原文 %s）", err, stdout)
	}
	return out
}

// decodeD4Report 解 changes.json（模型读到的也是同一份文本）。
// decodeD4Report decodes changes.json.
func decodeD4Report(f demoFatal, raw string) demoDocReport {
	f.Helper()
	var report demoDocReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		f.Fatalf("D4 的 changes.json 不是预期 JSON: %v（原文 %q）", err, raw)
	}
	return report
}

// demoDocPaths 列出文档库里的 Markdown 相对路径（`/` 分隔、按路径升序）——与
// docs-readonly 的清单顺序一致，判卷不依赖文件系统遍历顺序。
// demoDocPaths lists the markdown paths in the read-only root, sorted like the server does.
func demoDocPaths(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历文档库失败: %v", err)
	}
	sort.Strings(paths)
	return paths
}

// demoDocDigest 直接算磁盘上某份文档的 sha256（与实际字节对齐的判据）。
// demoDocDigest hashes one document's real bytes on disk.
func demoDocDigest(t *testing.T, root, rel string) string {
	t.Helper()
	sum, err := fileSHA256(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读文档 %s 失败: %v", rel, err)
	}
	return sum
}

// demoDocWrite 往只读库里写一份文档（第三轮的「新增 / 修改」）。
// demoDocWrite writes one document into the read-only root.
func demoDocWrite(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建文档目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写文档 %s 失败: %v", rel, err)
	}
}

// demoDocRemove 删掉只读库里的一份文档（第三轮的「删除」）。
// demoDocRemove removes one document from the read-only root.
func demoDocRemove(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("删文档 %s 失败: %v", rel, err)
	}
}

// d4MemoryRecord 是 save_memory 写下的那一条（下一次运行的「记忆基线」只认它）。
// d4MemoryRecord is the memory record the agent writes each run.
func d4MemoryRecord(out demoDocsOut) string {
	mode := "对比"
	if out.Mode == "baseline_created" {
		mode = "首跑"
	}
	return fmt.Sprintf("%s%s；模式：%s；计数：新增 %d / 修改 %d / 删除 %d / 未变 %d；state.sha256 %s；报告 %s",
		d4MemoryPrefix, out.ObservedAt, mode,
		out.Counts.Added, out.Counts.Modified, out.Counts.Deleted, out.Counts.Unchanged,
		out.State.SHA256, out.Report.Path)
}

// d4NotifyText 是一次 notify 的正文：结论 message + 计数 + 报告路径（AGENT.md 第 10 步）。
// d4NotifyText is the single notify body required by AGENT.md step 10.
func d4NotifyText(out demoDocsOut) string {
	return fmt.Sprintf("docs-diff %s：%s；计数：新增 %d / 修改 %d / 删除 %d / 未变 %d；报告 %s",
		out.ObservedAt, out.Message,
		out.Counts.Added, out.Counts.Modified, out.Counts.Deleted, out.Counts.Unchanged,
		out.Report.Path)
}

// d4MemoryBaseline 取召回结果的**最近一条**原文；零命中写固定文案。
// d4MemoryBaseline takes the latest recalled entry, or the fixed no-memory text.
func d4MemoryBaseline(memory string) string {
	if memory == d4RecallEmpty {
		return d4NoMemoryText
	}
	entries := strings.Split(memory, "\n\n")
	return strings.TrimSpace(entries[len(entries)-1])
}

// demoDocTitle 取正文的一级标题；没有标题就写「无标题」——不替文档编一个。
// demoDocTitle returns the document's H1, or a fixed placeholder.
func demoDocTitle(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return "无标题"
}

// demoDocLineCount 数正文行数（读到的正文原样，不做任何裁剪）。
// demoDocLineCount counts the body lines as read.
func demoDocLineCount(body string) int {
	if body == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(body, "\n"), "\n"))
}

// demoDocShort 取哈希前 12 位用于摘要（完整哈希仍全量列在「内容哈希」表里）。
// demoDocShort shortens a hash for the summary line; the full table keeps every byte.
func demoDocShort(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12] + "…"
}

// demoDocJoin 渲染集合列：空集合写「无」，不写 0 条也不省略。
// demoDocJoin renders a set cell; an empty set reads 无.
func demoDocJoin(paths []string) string {
	if len(paths) == 0 {
		return "无"
	}
	return strings.Join(paths, "、")
}

// demoDocOpt 渲染可空哈希 / 可空整数的表格单元（删除行必须显示为空）。
// demoDocOpt renders nullable cells; deleted rows must show a dash.
func demoDocOpt(value *string) string {
	if value == nil {
		return "—"
	}
	return *value
}

// demoDocOptInt 渲染可空整数的表格单元。
// demoDocOptInt renders a nullable integer cell.
func demoDocOptInt(value *int) string {
	if value == nil {
		return "—"
	}
	return strconv.Itoa(*value)
}

// d4ChangeSummary 写「变更摘要」：新增 / 修改在读到的正文里取标题，删除只列路径与
// 旧哈希（正文已不存在，**不得**读）。首跑只写一句「只记录基线」。
// d4ChangeSummary renders the change summary; a first run only records the baseline.
func d4ChangeSummary(report demoDocReport, bodies map[string]string) string {
	if report.Mode == "baseline_created" {
		return d4BaselineOnly
	}
	previous := map[string]string{}
	current := map[string]string{}
	for _, record := range report.Documents {
		if record.PreviousSHA256 != nil {
			previous[record.Path] = *record.PreviousSHA256
		}
		if record.SHA256 != nil {
			current[record.Path] = *record.SHA256
		}
	}
	var lines []string
	for _, path := range report.Sets.Added {
		body := bodies[path]
		lines = append(lines, fmt.Sprintf("- [新增] %s（%s）：正文 %d 行",
			path, demoDocTitle(body), demoDocLineCount(body)))
	}
	for _, path := range report.Sets.Modified {
		body := bodies[path]
		lines = append(lines, fmt.Sprintf("- [修改] %s（%s）：内容哈希 %s → %s",
			path, demoDocTitle(body), demoDocShort(previous[path]), demoDocShort(current[path])))
	}
	for _, path := range report.Sets.Deleted {
		lines = append(lines, fmt.Sprintf("- [删除] %s（旧哈希 %s）", path, previous[path]))
	}
	if len(lines) == 0 {
		return "本次无变化"
	}
	return strings.Join(lines, "\n")
}

// d4ChangesReport 按 AGENT.md 第 7 步的模板渲染差异报告：哈希、计数、集合照抄
// changes.json，不截断、不手拼、不心算。
// d4ChangesReport renders the diff report from changes.json alone.
func d4ChangesReport(report demoDocReport, memory string, bodies map[string]string) string {
	var b strings.Builder
	b.WriteString("# 文档差异报告 · " + report.ObservedAt + "\n\n")
	b.WriteString("结论：" + report.Message + "\n\n")
	b.WriteString("## 记忆基线\n")
	b.WriteString(d4MemoryBaseline(memory) + "\n\n")
	b.WriteString("## 变更集合\n")
	b.WriteString("| 类别 | 数量 | 文档 |\n")
	b.WriteString("| --- | --- | --- |\n")
	b.WriteString(fmt.Sprintf("| 新增 | %d | %s |\n",
		report.Counts.Added, demoDocJoin(report.Sets.Added)))
	b.WriteString(fmt.Sprintf("| 修改 | %d | %s |\n",
		report.Counts.Modified, demoDocJoin(report.Sets.Modified)))
	b.WriteString(fmt.Sprintf("| 删除 | %d | %s |\n",
		report.Counts.Deleted, demoDocJoin(report.Sets.Deleted)))
	b.WriteString(fmt.Sprintf("| 未变 | %d | %s |\n",
		report.Counts.Unchanged, demoDocJoin(report.Sets.Unchanged)))
	b.WriteString("\n## 内容哈希\n")
	b.WriteString("| 文档 | 状态 | 本次 sha256 | 上次 sha256 | 大小（字节） |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	for _, record := range report.Documents {
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s |\n",
			record.Path, record.Status, demoDocOpt(record.SHA256),
			demoDocOpt(record.PreviousSHA256), demoDocOptInt(record.Size)))
	}
	b.WriteString("\n## 变更摘要\n")
	b.WriteString(d4ChangeSummary(report, bodies) + "\n")
	b.WriteString("\n## 失败\n无\n")
	return b.String()
}

// d4Steps 组装一次 D4 运行：读记忆 → 列文档 → 逐份取元信息 → 落两个输入 → 跑脚本 →
// 读 changes.json → （仅新增 / 修改）读正文 → 写报告 → 写记忆 → 通知 → 收尾。
// expectDocs / expectReads 是本轮应有的文档数与正文读取数（剧本条数由它决定，运行时
// 与实际不符就判失败——不给「少取几份也算过」留缝）。
// d4Steps builds one D4 run; the script size follows the expected document/read counts.
func d4Steps(f *demoFixture, runTag string, expectDocs, expectReads int) []demoStep {
	var (
		memory    string
		listing   []demoDocEntry
		listText  string
		metaTexts []string
		out       demoDocsOut
		report    demoDocReport
		toRead    []string
		bodies    = map[string]string{}
		reportOut string
		index     int
	)
	next := func(step string) string {
		index++
		return fmt.Sprintf("d4-%s-%d-%s", runTag, index, step)
	}
	// collectMeta 校验一份元信息：路径对得上、哈希等于磁盘真实字节——元信息是脚本判
	// 「未变 / 修改」的唯一依据，判卷侧必须自己再算一遍。
	collectMeta := func(d *demoTurn, path string) {
		text := d.lastToolText()
		var meta demoDocMeta
		if err := json.Unmarshal([]byte(text), &meta); err != nil {
			d.failf("%s 的元信息不是 JSON: %v（%q）", path, err, text)
		}
		if meta.Path != path {
			d.failf("元信息的 path = %q，期望 %q", meta.Path, path)
		}
		if want := demoDocDigest(d.t, f.docsRoot, path); meta.SHA256 != want {
			d.failf("%s 的元信息哈希 %s 与磁盘 %s 不一致", path, meta.SHA256, want)
		}
		metaTexts = append(metaTexts, text)
	}
	writeReport := func(d *demoTurn) testutil.FakeOpenAIResponse {
		if expectReads > 0 {
			name, _ := d.lastTool()
			d.must(name == "read_document", "读完正文之后应写报告，实际 %q", name)
			bodies[toRead[expectReads-1]] = d.lastToolText()
		}
		reportOut = d4ChangesReport(report, memory, bodies)
		return demoCall(next("write-report"), "write_file", map[string]any{
			"path":    filepath.ToSlash(filepath.Dir(out.Report.Path)) + "/changes.md",
			"content": reportOut,
		})
	}

	steps := []demoStep{
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			d.requireTool("notify")
			return demoCall(next("recall"), "recall_memory", map[string]any{"query": "docs-diff"})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "recall_memory", "第 1 步应是读记忆基线，实际 %q", name)
			memory = d.lastToolText()
			d.must(memory == d4RecallEmpty || strings.Contains(memory, d4MemoryPrefix),
				"记忆读数既不是占位文案也不像历史记录：%q", memory)
			return demoCall(next("list"), "list_documents", map[string]any{})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "list_documents", "第 2 步应是列文档，实际 %q", name)
			listText = d.lastToolText()
			if err := json.Unmarshal([]byte(listText), &listing); err != nil {
				d.failf("list_documents 结果不是 JSON 数组：%v（%q）", err, listText)
			}
			d.must(len(listing) == expectDocs, "清单有 %d 份文档，期望 %d 份", len(listing), expectDocs)
			d.must(len(listing) > 0, "清单为空——文档库没准备好")
			for i := 1; i < len(listing); i++ {
				d.must(listing[i-1].Path < listing[i].Path, "清单未按路径升序：%q 在 %q 之前",
					listing[i-1].Path, listing[i].Path)
			}
			return demoCall(next("metadata"), "document_metadata",
				map[string]any{"path": listing[0].Path})
		},
	}
	for i := 1; i < expectDocs; i++ {
		prev := i
		steps = append(steps, func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "document_metadata", "第 %d 步应是逐份取元信息，实际 %q", prev+2, name)
			collectMeta(d, listing[prev-1].Path)
			return demoCall(next("metadata"), "document_metadata",
				map[string]any{"path": listing[prev].Path})
		})
	}
	steps = append(steps,
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "document_metadata", "最后一份元信息没取到，实际 %q", name)
			collectMeta(d, listing[expectDocs-1].Path)
			d.must(len(metaTexts) == expectDocs, "元信息收集了 %d 份，期望 %d 份",
				len(metaTexts), expectDocs)
			// 清单原文照落：脚本要求 documents.json 与 metadata.json 同源。
			return demoCall(next("write-documents"), "write_file", map[string]any{
				"path":    "documents.json",
				"content": listText,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "落盘清单之后应写元信息，实际 %q", name)
			return demoCall(next("write-metadata"), "write_file", map[string]any{
				"path":    "metadata.json",
				"content": `{"documents":[` + strings.Join(metaTexts, ",") + `]}`,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 4 步应是落盘输入，实际 %q", name)
			return demoCall(next("shell"), "shell", map[string]any{
				"executable": f.python,
				"arguments": []string{
					"scripts/docs_diff.py",
					"--documents", "documents.json",
					"--metadata", "metadata.json",
					"--state", "state/baseline.json",
				},
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "shell", "第 5 步应是脚本，实际 %q", name)
			out = decodeD4(d, d.lastToolText())
			d.must(out.Report.Path != "", "脚本没有给出报告路径")
			d.must(out.State.Path != "", "脚本没有给出基线路径")
			return demoCall(next("read-report"), "read_file", map[string]any{"path": out.Report.Path})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "read_file", "第 6 步应是读差异结果，实际 %q", name)
			report = decodeD4Report(d, d.lastToolText())
			toRead = append(append([]string{}, out.Sets.Added...), out.Sets.Modified...)
			d.must(len(toRead) == expectReads,
				"本轮应读 %d 份新增 / 修改正文，实际 %d 份（%v）", expectReads, len(toRead), toRead)
			if expectReads > 0 {
				return demoCall(next("read-document"), "read_document",
					map[string]any{"path": toRead[0]})
			}
			return writeReport(d)
		},
	)
	for i := 1; i < expectReads; i++ {
		prev := i
		steps = append(steps, func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "read_document", "第 %d 步应是读正文，实际 %q", prev+6, name)
			bodies[toRead[prev-1]] = d.lastToolText()
			return demoCall(next("read-document"), "read_document",
				map[string]any{"path": toRead[prev]})
		})
	}
	// 无正文可读时（首跑 / 无变化），报告已在读差异结果那一步直接写掉：此时**不能**
	// 再把 writeReport 排成下一轮，否则同一份文件会被写两遍。
	// With nothing to read, the report is written right after changes.json; appending
	// writeReport again would write the same file twice.
	if expectReads > 0 {
		steps = append(steps, writeReport)
	}
	steps = append(steps,
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 7 步应是写报告，实际 %q", name)
			d.must(reportOut != "", "报告正文为空——渲染失败")
			return demoCall(next("save-memory"), "save_memory",
				map[string]any{"content": d4MemoryRecord(out)})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "save_memory", "第 8 步应是写记忆，实际 %q", name)
			d.must(strings.Contains(d.lastToolText(), "已记住"),
				"save_memory 读数 = %q", d.lastToolText())
			return demoCall(next("notify"), "notify", map[string]any{
				"channel": demoNotifyName,
				"content": d4NotifyText(out),
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "notify", "第 9 步应是通知，实际 %q", name)
			d.must(strings.Contains(d.lastToolText(), "已推送到通知渠道："+demoNotifyName),
				"notify 读数 = %q", d.lastToolText())
			return demoFinal(reportOut)
		},
	)
	return steps
}

// d4RequireRecords 逐份核对内容哈希表：非删除行必须等于磁盘真实字节，未变行的
// 本次哈希必须等于上次，首跑 / 新增行没有「上次哈希」。
// d4RequireRecords re-hashes every document and checks the per-record invariant.
func d4RequireRecords(t *testing.T, root string, report demoDocReport) {
	t.Helper()
	if len(report.Documents) != report.Counts.Current+report.Counts.Deleted {
		t.Fatalf("内容哈希表 %d 行，期望 当前 %d + 删除 %d",
			len(report.Documents), report.Counts.Current, report.Counts.Deleted)
	}
	for _, record := range report.Documents {
		switch record.Status {
		case "deleted":
			if record.SHA256 != nil || record.Size != nil {
				t.Fatalf("%s 已删除，本次哈希 / 大小应为空：%#v", record.Path, record)
			}
			if record.PreviousSHA256 == nil {
				t.Fatalf("%s 已删除但没有旧哈希", record.Path)
			}
			continue
		case "baseline", "added", "modified", "unchanged":
		default:
			t.Fatalf("%s 的状态 %q 不在 baseline / added / modified / unchanged / deleted 之内",
				record.Path, record.Status)
		}
		if record.SHA256 == nil || *record.SHA256 != demoDocDigest(t, root, record.Path) {
			t.Fatalf("%s 的本次哈希 %v 与磁盘 %s 不一致",
				record.Path, record.SHA256, demoDocDigest(t, root, record.Path))
		}
		if record.Status == "unchanged" && (record.PreviousSHA256 == nil ||
			*record.PreviousSHA256 != *record.SHA256) {
			t.Fatalf("%s 标为未变，但新旧哈希不一致：%#v", record.Path, record)
		}
		if record.Status == "modified" && (record.PreviousSHA256 == nil ||
			*record.PreviousSHA256 == *record.SHA256) {
			t.Fatalf("%s 标为修改，但新旧哈希相同：%#v", record.Path, record)
		}
		if record.Status == "baseline" || record.Status == "added" {
			if record.PreviousSHA256 != nil || record.PreviousSize != nil {
				t.Fatalf("%s 是新增 / 基线记录，不该有上次哈希：%#v", record.Path, record)
			}
		}
	}
}

// testDemoDocsDiff 覆盖 D4 的三轮：首跑记基线（不判定新增）→ 无变化 → 真实增 / 改 /
// 删后判出三集合与全量哈希；第二轮与第三轮之间清空会话消息，跨轮基线**只能**来自记忆。
// testDemoDocsDiff covers D4 across three runs separated by a wiped session.
func testDemoDocsDiff(t *testing.T) {
	f := newDemoFixture(t)
	modelUID, channelUID := f.modelUID(t), f.channelUID(t)
	base := time.Now()
	auto := base.Add(15 * time.Second).In(demoCST())
	dir := f.installAgent(t, "docs-diff", demoOptions{
		notify:     true,
		cronAt:     auto,
		modelUID:   modelUID,
		channelUID: channelUID,
		cronOld:    "0 15 9 * * *",
	})
	const taskID = "daily-docs-diff"
	paths := demoDocPaths(t, f.docsRoot)
	if len(paths) != 4 {
		t.Fatalf("文档库有 %d 份文档，期望 4 份（%v）", len(paths), paths)
	}

	f.model.beginRun(dir, d4Steps(f, "a", len(paths), 0)...)
	serve := f.serve(t)
	defer serve.stop(t)

	// —— 第一次：自动触发，没有基线 ——
	firstRun := f.waitCronRun(t, taskID, 120*time.Second)
	first := decodeD4(t, demoShellResults(t, f.h, firstRun)[0])
	if first.Mode != "baseline_created" || !first.BaselineMissing {
		t.Fatalf("首跑模式 = %s / baseline_missing %v，期望 baseline_created / true",
			first.Mode, first.BaselineMissing)
	}
	if first.ChangesDetected != nil {
		t.Fatalf("首跑不该出现 changes_detected（没有比较就无所谓变化），实际 %v", *first.ChangesDetected)
	}
	if first.Message != "无历史基线：本次记录 4 份文档的内容哈希作为基线，不判定新增 / 修改 / 删除" {
		t.Fatalf("首跑结论句 = %q", first.Message)
	}
	if first.Counts.Current != 4 || first.Counts.Baseline != 0 ||
		first.Counts.Added+first.Counts.Modified+first.Counts.Deleted+first.Counts.Unchanged != 0 {
		t.Fatalf("首跑计数 = %#v，期望 current 4 / baseline 0 / 其余全 0", first.Counts)
	}
	for _, digest := range first.Input {
		requireDigestOnDisk(t, dir, digest, "D4 输入")
	}
	requireDigestOnDisk(t, dir, first.Report, "D4 报告")
	requireDigestOnDisk(t, dir, first.State, "D4 基线")

	firstReport := decodeD4Report(t, readDemoFile(t, dir, filepath.ToSlash(first.Report.Path)))
	d4RequireRecords(t, f.docsRoot, firstReport)
	for _, record := range firstReport.Documents {
		if record.Status != "baseline" {
			t.Fatalf("%s 的首跑状态 = %q，期望 baseline（记录基线不是新增）",
				record.Path, record.Status)
		}
	}

	firstMarkdown := readDemoFile(t, dir,
		filepath.ToSlash(filepath.Dir(first.Report.Path))+"/changes.md")
	for _, want := range []string{
		"# 文档差异报告 · " + first.ObservedAt,
		"结论：" + first.Message,
		"## 记忆基线\n" + d4NoMemoryText,
		"| 新增 | 0 | 无 |",
		d4BaselineOnly,
	} {
		if !strings.Contains(firstMarkdown, want) {
			t.Fatalf("首跑报告缺少 %q：\n%s", want, firstMarkdown)
		}
	}
	if reads := demoToolPaths(t, f.h, firstRun, "read_document"); len(reads) != 0 {
		t.Fatalf("首跑不得读任何正文，实际读了 %v", reads)
	}

	firstEvidence := readDemoEvidence(t, f.h, firstRun)
	if firstEvidence.Status != "completed" || firstEvidence.Trigger != "cron" {
		t.Fatalf("首轮终态 = %s / %s，期望 completed / cron", firstEvidence.Status, firstEvidence.Trigger)
	}
	if firstEvidence.ToolCalls != 13 {
		t.Fatalf("首轮工具调用 %d 次，期望 13 次（剧本写死）", firstEvidence.ToolCalls)
	}
	f.requireNotifyCount(t, firstRun, 1, 1)
	if status := demoDeliveryStatus(t, f.h, firstRun); status != "" {
		t.Fatalf("D4 走 notify 工具，不该有系统代投行，实际 %q", status)
	}

	// —— 第二次：人工补跑，无变化也必须出报告 ——
	f.model.beginRun(dir, d4Steps(f, "b", len(paths), 0)...)
	second := f.runScheduleNowOnce(t, taskID)
	secondOut := decodeD4(t, demoShellResults(t, f.h, second.RunID)[0])
	if secondOut.Mode != "compared" || secondOut.BaselineMissing {
		t.Fatalf("第二轮模式 = %s / baseline_missing %v，期望 compared / false",
			secondOut.Mode, secondOut.BaselineMissing)
	}
	if secondOut.ChangesDetected == nil || *secondOut.ChangesDetected {
		t.Fatalf("第二轮 changes_detected = %v，期望 false", secondOut.ChangesDetected)
	}
	wantSecond := "本次无变化：4 份文档的内容哈希与基线一致（基线观察时间 " + first.ObservedAt + "）"
	if secondOut.Message != wantSecond {
		t.Fatalf("第二轮结论句 = %q，期望 %q", secondOut.Message, wantSecond)
	}
	if secondOut.Counts.Current != 4 || secondOut.Counts.Baseline != 4 ||
		secondOut.Counts.Unchanged != 4 || secondOut.Counts.Added+secondOut.Counts.Modified+
		secondOut.Counts.Deleted != 0 {
		t.Fatalf("第二轮计数 = %#v，期望 current 4 / baseline 4 / unchanged 4", secondOut.Counts)
	}
	requireDigestOnDisk(t, dir, secondOut.Report, "D4 第二轮报告")
	requireDigestOnDisk(t, dir, secondOut.State, "D4 第二轮基线")
	secondReport := decodeD4Report(t, readDemoFile(t, dir, filepath.ToSlash(secondOut.Report.Path)))
	if secondReport.BaselineObserved == nil || *secondReport.BaselineObserved != first.ObservedAt {
		t.Fatalf("第二轮基线观察时间 = %v，期望 %q", secondReport.BaselineObserved, first.ObservedAt)
	}
	d4RequireRecords(t, f.docsRoot, secondReport)
	secondMarkdown := readDemoFile(t, dir,
		filepath.ToSlash(filepath.Dir(secondOut.Report.Path))+"/changes.md")
	if !strings.Contains(secondMarkdown, "本次无变化") {
		t.Fatalf("无变化也必须写报告（本次无变化）：\n%s", secondMarkdown)
	}
	if !strings.Contains(secondMarkdown, d4MemoryPrefix+first.ObservedAt) {
		t.Fatalf("第二轮的「记忆基线」应收录首跑写下的记录：\n%s", secondMarkdown)
	}
	if reads := demoToolPaths(t, f.h, second.RunID, "read_document"); len(reads) != 0 {
		t.Fatalf("无变化时不得读任何正文，实际读了 %v", reads)
	}
	f.requireNotifyCount(t, second.RunID, 1, 2)

	// —— 第三轮：真实增 / 改 / 删，且会话消息清空（基线只能来自记忆）——
	clearAgentMessages(t, f.h, "docs-diff")
	demoDocWrite(t, f.docsRoot, "teams/on-call.md",
		"# 值班手册\n\n新增：值班轮换与升级路径。\n")
	handbook := readDemoFile(t, f.docsRoot, "handbook.md")
	demoDocWrite(t, f.docsRoot, "handbook.md", handbook+"\n补充：出差报销需附审批号。\n")
	demoDocRemove(t, f.docsRoot, "tooling.md")
	mutated := demoDocPaths(t, f.docsRoot)
	if len(mutated) != 4 {
		t.Fatalf("变更后文档库有 %d 份文档，期望 4 份（%v）", len(mutated), mutated)
	}

	f.model.beginRun(dir, d4Steps(f, "c", len(mutated), 2)...)
	third := f.runScheduleNowOnce(t, taskID)
	thirdOut := decodeD4(t, demoShellResults(t, f.h, third.RunID)[0])
	if thirdOut.ChangesDetected == nil || !*thirdOut.ChangesDetected {
		t.Fatalf("第三轮 changes_detected = %v，期望 true", thirdOut.ChangesDetected)
	}
	wantSets := demoDocSets{
		Added:     []string{"teams/on-call.md"},
		Modified:  []string{"handbook.md"},
		Deleted:   []string{"tooling.md"},
		Unchanged: []string{"security-basics.md", "teams/data-access.md"},
	}
	if !slices.Equal(thirdOut.Sets.Added, wantSets.Added) ||
		!slices.Equal(thirdOut.Sets.Modified, wantSets.Modified) ||
		!slices.Equal(thirdOut.Sets.Deleted, wantSets.Deleted) ||
		!slices.Equal(thirdOut.Sets.Unchanged, wantSets.Unchanged) {
		t.Fatalf("第三轮集合 = %#v，期望 %#v", thirdOut.Sets, wantSets)
	}
	wantThird := "本次检测到变更：新增 1 / 修改 1 / 删除 1，未变 2（基线观察时间 " + secondOut.ObservedAt + "）"
	if thirdOut.Message != wantThird {
		t.Fatalf("第三轮结论句 = %q，期望 %q", thirdOut.Message, wantThird)
	}
	if thirdOut.Counts.Current != 4 || thirdOut.Counts.Baseline != 4 ||
		thirdOut.Counts.Added != 1 || thirdOut.Counts.Modified != 1 ||
		thirdOut.Counts.Deleted != 1 || thirdOut.Counts.Unchanged != 2 {
		t.Fatalf("第三轮计数 = %#v", thirdOut.Counts)
	}
	requireDigestOnDisk(t, dir, thirdOut.State, "D4 第三轮基线")
	thirdReport := decodeD4Report(t, readDemoFile(t, dir, filepath.ToSlash(thirdOut.Report.Path)))
	d4RequireRecords(t, f.docsRoot, thirdReport)

	reads := demoToolPaths(t, f.h, third.RunID, "read_document")
	if len(reads) != 2 || reads[0] != "teams/on-call.md" || reads[1] != "handbook.md" {
		t.Fatalf("第三轮只该读新增 / 修改的两份正文（按集合顺序），实际 %v", reads)
	}
	thirdMarkdown := readDemoFile(t, dir,
		filepath.ToSlash(filepath.Dir(thirdOut.Report.Path))+"/changes.md")
	memorySection := demoDocSection(thirdMarkdown, "## 记忆基线", "## 变更集合")
	if !strings.Contains(memorySection, d4MemoryPrefix+secondOut.ObservedAt) {
		t.Fatalf("第三轮的「记忆基线」应收录上一轮的记录（跨会话只认记忆）：\n%s", memorySection)
	}
	if strings.Contains(memorySection, d4MemoryPrefix+first.ObservedAt) {
		t.Fatalf("第三轮的「记忆基线」只该写最近一条，却含首跑记录：\n%s", memorySection)
	}
	for _, want := range []string{
		"- [新增] teams/on-call.md",
		"- [修改] handbook.md",
		"- [删除] tooling.md",
		"| 未变 | 2 | security-basics.md、teams/data-access.md |",
	} {
		if !strings.Contains(thirdMarkdown, want) {
			t.Fatalf("第三轮报告缺少 %q：\n%s", want, thirdMarkdown)
		}
	}
	f.requireNotifyCount(t, third.RunID, 1, 3)
	thirdEvidence := readDemoEvidence(t, f.h, third.RunID)
	if thirdEvidence.Status != "completed" || thirdEvidence.Trigger != "manual" {
		t.Fatalf("第三轮终态 = %s / %s，期望 completed / manual",
			thirdEvidence.Status, thirdEvidence.Trigger)
	}
	if thirdEvidence.ToolCalls != 15 {
		t.Fatalf("第三轮工具调用 %d 次，期望 15 次（13 + 2 次读正文）", thirdEvidence.ToolCalls)
	}
}

// demoDocSection 取报告里两个小标题之间的正文（判「记忆基线」只看这一节）。
// demoDocSection extracts the text between two headings.
func demoDocSection(body, begin, end string) string {
	_, rest, found := strings.Cut(body, begin+"\n")
	if !found {
		return ""
	}
	section, _, found := strings.Cut(rest, "\n"+end)
	if !found {
		return rest
	}
	return section
}

// ============================================================
// D5 industry-brief：Brave + Fetch 双 MCP + 来源表 + 引用自检
// ============================================================

const (
	// d5Now 是本轮固定的运行时钟：`--now` 同时决定 observed_at、访问时间与产物目录
	// （runs/<t0>/），固定它才能对 brief.json 做逐字节哈希判据。
	// d5Now pins the run clock so brief.json is byte-for-byte reproducible.
	d5Now   = "2026-10-25T09:30:00+08:00"
	d5Topic = "企业级 AI Agent 平台"
	// d5BriefHash 是固定输入 + 固定时钟下 brief.json 的实测值：来源合并、正文哈希、
	// excerpt 与访问时间任何一处变了都会变——六段证据里的「候选产物哈希」。
	// d5BriefHash is the measured artifact hash for the shipped fixed inputs.
	d5BriefHash = "86dbe1dd26960353a2b890aab0d4ec585d30ef3d59349923081c96847f2db170"
	// d5SearchHash / d5PagesHash 是模型落盘的两份输入快照的实测值（固定输入重排后）。
	// d5SearchHash / d5PagesHash are the measured input-snapshot hashes.
	d5SearchHash = "f8d220f66969c7a7d61c240971c41adc2688dbad814742baedf2b85d1265c67e"
	d5PagesHash  = "87d7d0251ee716cb8e89bf544d30c1506468caac3bb4f4ffc07cc46f02a8b92c"
	// d5Message 是脚本对这份固定输入的结论句（守恒关系的自然语言版）。
	// d5Message is the script's conclusion line for these fixed inputs.
	d5Message = "本次检索 3 个查询，命中 10 条结果（去重 2 条），来源 8 个：已抓正文 5 个、仅线索 3 个"
	// d5FailReason 是 404 页面的报错原文：脚本原样写进 failures 与 note。
	// d5FailReason is the verbatim fetch error the script records.
	d5FailReason = "HTTP 404 Not Found"
)

// d5Citable / d5Lead 是脚本按 URL 升序给出的两组来源：顺序本身就是判据
// （merge_sources 只按 URL 排序，不按命中顺序）。
// d5Citable / d5Lead are the two URL-sorted source sets the script must produce.
var (
	d5Citable = []string{
		"https://example.com/analysis/audit-trail-design",
		"https://example.com/blog/runtime-cost-controls",
		"https://example.com/blog/self-hosted-agent-runtime",
		"https://example.com/guides/tool-sandbox-patterns",
		"https://example.com/reports/agent-platform-observability-2026",
	}
	d5Lead = []string{
		"https://example.com/news/enterprise-agent-adoption",
		"https://example.com/news/sandbox-incident-review",
		"https://example.com/vendor/agent-os-comparison",
	}
)

// demoBriefResult / demoBriefQuery / demoBriefPage 是 fixtures/ 两份固定输入的解码外形。
// demoBriefResult, demoBriefQuery and demoBriefPage decode the two shipped fixtures.
type demoBriefResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	PageAge     string `json:"page_age,omitempty"`
}

type demoBriefQuery struct {
	Query   string            `json:"query"`
	Results []demoBriefResult `json:"results"`
}

type demoBriefPage struct {
	URL     string `json:"url"`
	Status  int    `json:"status"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Error   string `json:"error"`
}

// demoBriefPlan 是一轮 D5 的全部固定输入：三条查询的结果 + 六份抓取结果。
// demoBriefPlan is one D5 run's fixed input.
type demoBriefPlan struct {
	Queries []demoBriefQuery
	Pages   []demoBriefPage
}

// demoBriefEnvelope 是 Brave 工具结果的真实外形：结果数组包在 `web` 下。
// demoBriefEnvelope is the Brave result shape: the array sits under `web`.
type demoBriefEnvelope struct {
	Web struct {
		Results []demoBriefResult `json:"results"`
	} `json:"web"`
}

// demoBriefEnvelopeRaw 用原始字节接住结果数组——落盘时必须原样，不重新排版。
// demoBriefEnvelopeRaw keeps each result's raw bytes so writes stay verbatim.
type demoBriefEnvelopeRaw struct {
	Web struct {
		Results []json.RawMessage `json:"results"`
	} `json:"web"`
}

// demoBriefQueryFile / demoBriefPageFile 是模型要落盘的两份输入的编码外形。
// demoBriefQueryFile / demoBriefPageFile encode the two snapshot files the model writes.
type demoBriefQueryFile struct {
	Query   string            `json:"query"`
	Results []json.RawMessage `json:"results"`
}

type demoBriefPageFile struct {
	URL     string `json:"url"`
	Status  int    `json:"status"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// demoBriefCounts 是脚本 stdout 与 brief.json 共用的计数段。
// demoBriefCounts is the counts block shared by stdout and brief.json.
type demoBriefCounts struct {
	Queries       int `json:"queries"`
	Results       int `json:"results"`
	Sources       int `json:"sources"`
	Duplicates    int `json:"duplicates"`
	Fetched       int `json:"fetched"`
	Leads         int `json:"leads"`
	FetchFailures int `json:"fetch_failures"`
	MissingDates  int `json:"missing_dates"`
}

// demoBriefFailure 是一条抓取失败记录（脚本只留 URL 与原因原文）。
// demoBriefFailure is one recorded fetch failure.
type demoBriefFailure struct {
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

// demoBriefSource 是 brief.json 里一条来源记录：`published_at` / `accessed_at` 可能为 null。
// demoBriefSource is one brief.json source record; the two timestamps may be null.
type demoBriefSource struct {
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	PublishedAt *string `json:"published_at"`
	AccessedAt  *string `json:"accessed_at"`
	Status      string  `json:"status"`
	Citable     bool    `json:"citable"`
	Excerpt     string  `json:"excerpt"`
	Note        string  `json:"note"`
}

// demoBriefReport 是 brief.json 的外形（读来源表那一步解码它来写简报）。
// demoBriefReport is the brief.json shape the model reads back.
type demoBriefReport struct {
	SchemaVersion int                `json:"schema_version"`
	Agent         string             `json:"agent"`
	ObservedAt    string             `json:"observed_at"`
	Topic         string             `json:"topic"`
	Counts        demoBriefCounts    `json:"counts"`
	Sources       []demoBriefSource  `json:"sources"`
	CitableURLs   []string           `json:"citable_urls"`
	LeadURLs      []string           `json:"lead_urls"`
	Failures      []demoBriefFailure `json:"failures"`
	Message       string             `json:"message"`
}

// demoBriefOut 是脚本 stdout 的外形（D5 的 input 是嵌套映射，与共用外形不同）。
// demoBriefOut is the script's stdout shape; D5's input is a nested map.
type demoBriefOut struct {
	Agent          string                `json:"agent"`
	ObservedAt     string                `json:"observed_at"`
	Topic          string                `json:"topic"`
	Input          map[string]demoDigest `json:"input"`
	Report         demoDigest            `json:"report"`
	Counts         demoBriefCounts       `json:"counts"`
	CitableURLs    []string              `json:"citable_urls"`
	LeadURLs       []string              `json:"lead_urls"`
	Message        string                `json:"message"`
	VerifiedReport string                `json:"verified_report"`
}

// demoBriefFixture 读仓库里的两份固定输入并核对条数（3 条查询 / 6 份抓取）。
// demoBriefFixture loads the shipped fixed inputs and checks their size.
func demoBriefFixture(t *testing.T, f *demoFixture) demoBriefPlan {
	t.Helper()
	var search struct {
		Queries []demoBriefQuery `json:"queries"`
	}
	if err := json.Unmarshal([]byte(f.fixtureJSON(t,
		"examples/daily/industry-brief/fixtures/search.json")), &search); err != nil {
		t.Fatalf("解析 D5 检索固定输入失败: %v", err)
	}
	var pages struct {
		Pages []demoBriefPage `json:"pages"`
	}
	if err := json.Unmarshal([]byte(f.fixtureJSON(t,
		"examples/daily/industry-brief/fixtures/pages.json")), &pages); err != nil {
		t.Fatalf("解析 D5 抓取固定输入失败: %v", err)
	}
	if len(search.Queries) != 3 || len(pages.Pages) != 6 {
		t.Fatalf("D5 固定输入 = %d 条查询 / %d 份抓取，期望 3 / 6",
			len(search.Queries), len(pages.Pages))
	}
	return demoBriefPlan{Queries: search.Queries, Pages: pages.Pages}
}

// setToolResultStep 在剧本里改写 MCP 规格的当前结果（下一次 `tools/call` 生效）。
// 剧本跑在假模型的处理 goroutine 上，不能走 t.Fatalf，错误一律交给 failf。
// 规格文件用同目录临时文件 + rename 替换：helper 子进程每次都重读它，不能读到半截。
// setToolResultStep rewrites one tool's result from inside a scripted turn; the
// replacement is atomic because the helper re-reads the spec on every call.
func (f *demoFixture) setToolResultStep(d *demoTurn, key, name, result string) {
	d.t.Helper()
	raw, err := os.ReadFile(f.specs[key])
	if err != nil {
		d.failf("读 MCP 规格 %s 失败: %v", key, err)
	}
	var spec testutil.MCPSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		d.failf("解析 MCP 规格 %s 失败: %v", key, err)
	}
	found := false
	for i := range spec.Tools {
		if spec.Tools[i].Name == name {
			spec.Tools[i].Result = result
			spec.Tools[i].Error = ""
			found = true
		}
	}
	if !found {
		d.failf("MCP 规格 %s 里没有工具 %s", key, name)
	}
	out, err := json.Marshal(spec)
	if err != nil {
		d.failf("编码 MCP 规格 %s 失败: %v", key, err)
	}
	tmp := f.specs[key] + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		d.failf("写 MCP 规格 %s 失败: %v", key, err)
	}
	if err := os.Rename(tmp, f.specs[key]); err != nil {
		d.failf("替换 MCP 规格 %s 失败: %v", key, err)
	}
}

// d5BraveText 渲染一次 Brave 检索的工具结果（结果数组包在 `web` 下，与真实返回同形）。
// d5BraveText renders one Brave tool result with the real `web.results` envelope.
func d5BraveText(d *demoTurn, results []demoBriefResult) string {
	d.t.Helper()
	var env demoBriefEnvelope
	env.Web.Results = results
	raw, err := json.Marshal(env)
	if err != nil {
		d.failf("编码 Brave 结果失败: %v", err)
	}
	return string(raw)
}

// d5ResultsOf 从一次 Brave 结果里取出结果数组的原始字节（AGENT 第 2 步：原样保留）。
// d5ResultsOf extracts the raw result objects from one Brave tool result.
func d5ResultsOf(d *demoTurn, text string) []json.RawMessage {
	d.t.Helper()
	var env demoBriefEnvelopeRaw
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		d.failf("Brave 工具结果不是 JSON: %v（原文 %s）", err, text)
	}
	if len(env.Web.Results) == 0 {
		d.failf("Brave 工具结果里没有 web.results：%s", text)
	}
	return env.Web.Results
}

// d5SearchContent 组装 search.json：查询原文 + 工具给回的结果数组（原样，不重排）。
// d5SearchContent assembles search.json from the three Brave results.
func d5SearchContent(d *demoTurn, plan demoBriefPlan) string {
	d.t.Helper()
	results := d.lastToolTexts("brave_web_search", len(plan.Queries))
	files := make([]demoBriefQueryFile, 0, len(plan.Queries))
	for i, query := range plan.Queries {
		files = append(files, demoBriefQueryFile{
			Query:   query.Query,
			Results: d5ResultsOf(d, results[i]),
		})
	}
	raw, err := json.Marshal(struct {
		Queries []demoBriefQueryFile `json:"queries"`
	}{Queries: files})
	if err != nil {
		d.failf("编码 search.json 失败: %v", err)
	}
	return string(raw)
}

// d5PagesContent 组装 pages.json：成功页交正文原文，失败页交报错原文并留空正文。
// 传回来的正文必须与固定输入逐字相同——它决定 brief.json 里的 content_sha256。
// d5PagesContent assembles pages.json; the bodies must match the fixture verbatim.
func d5PagesContent(d *demoTurn, plan demoBriefPlan) string {
	d.t.Helper()
	texts := d.lastToolTexts("fetch", len(plan.Pages))
	files := make([]demoBriefPageFile, 0, len(plan.Pages))
	for i, page := range plan.Pages {
		entry := demoBriefPageFile{URL: page.URL, Status: page.Status, Title: page.Title}
		if page.Status == 200 {
			entry.Content = texts[i]
			d.must(entry.Content == page.Content,
				"第 %d 个 URL（%s）的正文与固定输入不一致——工具面改写了正文", i+1, page.URL)
		} else {
			entry.Error = texts[i]
			d.must(entry.Error == page.Error,
				"第 %d 个 URL（%s）的报错原文 = %q，期望 %q", i+1, page.URL, entry.Error, page.Error)
		}
		files = append(files, entry)
	}
	raw, err := json.Marshal(struct {
		Pages []demoBriefPageFile `json:"pages"`
	}{Pages: files})
	if err != nil {
		d.failf("编码 pages.json 失败: %v", err)
	}
	return string(raw)
}

// decodeD5Report 解 read_file 读回来的 brief.json（第 7 步：它才是来源事实源）。
// decodeD5Report decodes the brief.json body read back by the model.
func decodeD5Report(d *demoTurn, body string) demoBriefReport {
	d.t.Helper()
	var rep demoBriefReport
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		d.failf("brief.json 不是预期 JSON: %v（原文 %s）", err, body)
	}
	if len(rep.Sources) == 0 {
		d.failf("brief.json 没有 sources——来源表无从渲染")
	}
	return rep
}

// demoBriefDate 把可空的发布日期写成「未确认」（AGENT 第 7 步：不得补造日期）。
// demoBriefDate renders a nullable published_at as 未确认.
func demoBriefDate(value *string) string {
	if value == nil || *value == "" {
		return "未确认"
	}
	return *value
}

// demoBriefPtr 把可空字符串写成占位符（线索没有访问时间）。
// demoBriefPtr renders a nullable string with a placeholder.
func demoBriefPtr(value *string) string {
	if value == nil || *value == "" {
		return "—"
	}
	return *value
}

// demoBriefShort 取 excerpt 的第一句（结论逐条一行，不整段复制正文）。
// demoBriefShort takes the first sentence of the excerpt.
func demoBriefShort(excerpt string) string {
	runes := []rune(excerpt)
	for i, r := range runes {
		if r == '。' && i < 80 {
			return string(runes[:i+1])
		}
	}
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	return excerpt
}

// d5BriefMarkdown 按 AGENT.md 第 8 步的模板渲染简报：结论逐条带 `[来源: <url>]`，
// 来源表只列已抓正文的、线索表只列没抓 / 抓失败的，失败段原样抄报错原文。
// d5BriefMarkdown renders brief.md from brief.json following the AGENT.md template.
func d5BriefMarkdown(rep demoBriefReport, plan demoBriefPlan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 行业简报 · %s · %s\n\n", rep.Topic, rep.ObservedAt)
	fmt.Fprintf(&b, "结论：%s\n\n", rep.Message)
	b.WriteString("## 结论\n")
	for _, source := range rep.Sources {
		if !source.Citable {
			continue
		}
		fmt.Fprintf(&b, "- %s：%s [来源: %s]\n",
			source.Title, demoBriefShort(source.Excerpt), source.URL)
	}
	b.WriteString("\n## 来源表\n")
	b.WriteString("| # | URL | 标题 | 发布日期 | 访问时间 |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	index := 0
	for _, source := range rep.Sources {
		if source.Status != "fetched" {
			continue
		}
		index++
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", index, source.URL, source.Title,
			demoBriefDate(source.PublishedAt), demoBriefPtr(source.AccessedAt))
	}
	b.WriteString("\n## 线索（未抓正文，仅作线索，不得作为结论来源）\n")
	b.WriteString("| # | URL | 标题 | 发布日期 | 说明 |\n")
	b.WriteString("| --- | --- | --- | --- | --- |\n")
	index = 0
	for _, source := range rep.Sources {
		if source.Status == "fetched" {
			continue
		}
		index++
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", index, source.URL, source.Title,
			demoBriefDate(source.PublishedAt), source.Note)
	}
	b.WriteString("\n## 检索覆盖\n")
	for _, query := range plan.Queries {
		fmt.Fprintf(&b, "- %s\n", query.Query)
	}
	b.WriteString("\n## 失败\n")
	if len(rep.Failures) == 0 {
		b.WriteString("无\n")
		return b.String()
	}
	for _, failure := range rep.Failures {
		fmt.Fprintf(&b, "- %s：%s（本次未抓到的来源只作线索）\n", failure.URL, failure.Reason)
	}
	return b.String()
}

// d5Steps 组装一次 D5 运行：三条 Brave 查询 → 落盘 search.json → 六次 Fetch（其中一条
// 返回 404）→ 落盘 pages.json → 跑脚本 → 读 brief.json → 写 brief.md → 引用自检 →
// 以简报正文收尾（系统按 deliver_to 代投这条正文）。
// d5Steps builds one D5 run; the closing text is the brief the system delivers.
func d5Steps(f *demoFixture, plan demoBriefPlan, runTag string) []demoStep {
	var (
		out       demoBriefOut
		briefBody string
		briefPath string
	)
	next := func(index int, step string) string {
		return fmt.Sprintf("d5-%s-%d-%s", runTag, index, step)
	}
	scriptArgs := func() []string {
		return []string{
			"scripts/industry_brief.py",
			"--search", "search.json",
			"--pages", "pages.json",
			"--now", d5Now,
			"--topic", d5Topic,
		}
	}
	steps := []demoStep{
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			d.requireNoTool("notify", "D5 基线 notify_mode: none，投递由系统按 deliver_to 代发")
			f.setToolResultStep(d, demoBraveSpecEnv, "brave_web_search",
				d5BraveText(d, plan.Queries[0].Results))
			return demoCall(next(1, "search-1"), "brave_web_search",
				map[string]any{"query": plan.Queries[0].Query, "count": "5"})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "brave_web_search", "第 1 步应是检索，实际 %q", name)
			f.setToolResultStep(d, demoBraveSpecEnv, "brave_web_search",
				d5BraveText(d, plan.Queries[1].Results))
			return demoCall(next(2, "search-2"), "brave_web_search",
				map[string]any{"query": plan.Queries[1].Query, "count": "5"})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "brave_web_search", "第 2 步应是检索，实际 %q", name)
			f.setToolResultStep(d, demoBraveSpecEnv, "brave_web_search",
				d5BraveText(d, plan.Queries[2].Results))
			return demoCall(next(3, "search-3"), "brave_web_search",
				map[string]any{"query": plan.Queries[2].Query, "count": "5"})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "brave_web_search", "第 3 步应是检索，实际 %q", name)
			return demoCall(next(4, "write-search"), "write_file", map[string]any{
				"path":    "search.json",
				"content": d5SearchContent(d, plan),
			})
		},
	}
	// 六次抓取：每个 URL 的结果在下一次 `tools/call` 生效；404 那条交回报错原文。
	for i, page := range plan.Pages {
		want := "fetch"
		if i == 0 {
			want = "write_file"
		}
		steps = append(steps, func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == want, "第 %d 次抓取前的调用应是 %s，实际 %q", i+1, want, name)
			result := page.Content
			if page.Status != 200 {
				result = page.Error
			}
			d.must(result != "", "固定输入第 %d 条（%s）没有正文 / 报错原文", i+1, page.URL)
			f.setToolResultStep(d, demoFetchSpecEnv, "fetch", result)
			return demoCall(next(5+i, "fetch"), "fetch", map[string]any{"url": page.URL})
		})
	}
	steps = append(steps,
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "fetch", "第 6 次抓取后应是 fetch，实际 %q", name)
			return demoCall(next(11, "write-pages"), "write_file", map[string]any{
				"path":    "pages.json",
				"content": d5PagesContent(d, plan),
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 11 步应是落盘抓取结果，实际 %q", name)
			return demoCall(next(12, "build"), "shell", map[string]any{
				"executable": f.python,
				"arguments":  scriptArgs(),
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "shell", "第 12 步应是脚本，实际 %q", name)
			out = decodeD5(d, d.lastToolText())
			d.must(out.Report.Path != "", "脚本没有给出简报路径")
			return demoCall(next(13, "read-brief"), "read_file",
				map[string]any{"path": out.Report.Path})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "read_file", "第 13 步应是读来源表，实际 %q", name)
			rep := decodeD5Report(d, d.lastToolText())
			briefPath = filepath.ToSlash(filepath.Dir(out.Report.Path)) + "/brief.md"
			briefBody = d5BriefMarkdown(rep, plan)
			return demoCall(next(14, "write-brief"), "write_file", map[string]any{
				"path":    briefPath,
				"content": briefBody,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "write_file", "第 14 步应是写简报，实际 %q", name)
			d.must(briefBody != "" && briefPath != "", "简报正文 / 路径为空——第 14 步没渲染成功")
			// 同一个 --now / --topic 再加 --verify-report：脚本重写同一份 brief.json 后自检引用。
			args := append(scriptArgs(), "--verify-report", briefPath)
			return demoCall(next(15, "verify"), "shell", map[string]any{
				"executable": f.python,
				"arguments":  args,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "shell", "第 15 步应是引用自检，实际 %q", name)
			return demoFinal(briefBody)
		},
	)
	return steps
}

// decodeD5 解 D5 的 shell 结果（生成轮与自检轮共用同一外形）。
// decodeD5 decodes the D5 shell result shared by the build and verify turns.
func decodeD5(f demoFatal, result string) demoBriefOut {
	f.Helper()
	stdout := demoShellStdout(f, result)
	var out demoBriefOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		f.Fatalf("D5 脚本 stdout 不是预期 JSON: %v（原文 %s）", err, stdout)
	}
	return out
}

// demoShellExit 读 `shell` 结果首行的退出码（契约 agent-md §5.2）。
// demoShellExit reads the exit code from the tool result's first line.
func demoShellExit(t *testing.T, result string) int {
	t.Helper()
	first, _, _ := strings.Cut(result, "\n")
	code, err := strconv.Atoi(strings.TrimPrefix(first, "退出码："))
	if err != nil {
		t.Fatalf("shell 结果首行没有退出码：%q", result)
	}
	return code
}

// d5TableRows 数一张 markdown 表的数据行（去掉表头与分隔行）。
// d5TableRows counts a markdown table's data rows.
func d5TableRows(section string) int {
	rows := 0
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "| ") {
			rows++
		}
	}
	if rows < 2 {
		return 0
	}
	return rows - 2
}

// d5LineWith 返回第一行含 needle 的文本（表格行断言用）。
// d5LineWith returns the first line containing the needle.
func d5LineWith(body, needle string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// d5RequireCounts 断言脚本给出的业务读数：计数、守恒关系对应的结论句与两组来源。
// 自动与人工两轮共用同一份判据——对不上就是固定输入或脚本变了。
// d5RequireCounts asserts the business readings shared by both D5 runs.
func d5RequireCounts(t *testing.T, out demoBriefOut) {
	t.Helper()
	if out.Agent != "industry-brief" || out.ObservedAt != d5Now || out.Topic != d5Topic {
		t.Fatalf("脚本抬头 = %q / %q / %q，期望 industry-brief / %s / %s",
			out.Agent, out.ObservedAt, out.Topic, d5Now, d5Topic)
	}
	want := demoBriefCounts{
		Queries: 3, Results: 10, Sources: 8, Duplicates: 2,
		Fetched: 5, Leads: 3, FetchFailures: 1, MissingDates: 1,
	}
	if out.Counts != want {
		t.Fatalf("计数 = %#v，期望 %#v", out.Counts, want)
	}
	if out.Message != d5Message {
		t.Fatalf("结论句 = %q，期望 %q", out.Message, d5Message)
	}
	if !slices.Equal(out.CitableURLs, d5Citable) {
		t.Fatalf("可引用集合 = %v，期望 %v", out.CitableURLs, d5Citable)
	}
	if !slices.Equal(out.LeadURLs, d5Lead) {
		t.Fatalf("线索集合 = %v，期望 %v", out.LeadURLs, d5Lead)
	}
}

// d5RunCheck 是两轮各自要核的「与轮次相关」的读数。
// d5RunCheck carries the per-run readings the two D5 runs differ in.
type d5RunCheck struct {
	trigger      string
	deliverIndex int
	notifyTotal  int
}

// d5RequireRun 判一轮 D5：脚本读数、两份输入快照与产物的哈希、引用自检、简报内容、
// 六段证据与系统代投。两轮共用——它们只该在 trigger 与投递序号上不同。
// d5RequireRun asserts one D5 run end to end.
func d5RequireRun(t *testing.T, f *demoFixture, dir, runID string, check d5RunCheck) {
	t.Helper()
	results := demoShellResults(t, f.h, runID)
	if len(results) != 2 {
		t.Fatalf("shell 调用 %d 次，期望 2 次（生成 + 引用自检）：%v", len(results), results)
	}
	for i, want := range map[int]string{0: "生成", 1: "引用自检"} {
		if code := demoShellExit(t, results[i]); code != 0 {
			t.Fatalf("%s轮脚本退出码 %d，期望 0：%s", want, code, results[i])
		}
	}
	out := decodeD5(t, results[0])
	d5RequireCounts(t, out)
	if out.VerifiedReport != "" {
		t.Fatalf("生成轮的 verified_report 应为空，实际 %q", out.VerifiedReport)
	}
	if out.Report.SHA256 != d5BriefHash {
		t.Fatalf("简报哈希 %s 与固定输入下的实测值 %s 不一致", out.Report.SHA256, d5BriefHash)
	}
	if out.Input["search"].SHA256 != d5SearchHash || out.Input["pages"].SHA256 != d5PagesHash {
		t.Fatalf("输入快照哈希 = %s / %s，期望 %s / %s",
			out.Input["search"].SHA256, out.Input["pages"].SHA256, d5SearchHash, d5PagesHash)
	}
	requireDigestOnDisk(t, dir, out.Input["search"], "D5 检索输入")
	requireDigestOnDisk(t, dir, out.Input["pages"], "D5 抓取输入")
	requireDigestOnDisk(t, dir, out.Report, "D5 简报")

	// 自检轮：同输入同 --now ⇒ 重写后的 brief.json 逐字节相同；回报的是简报路径本身。
	verified := decodeD5(t, results[1])
	briefPath := filepath.ToSlash(filepath.Dir(out.Report.Path)) + "/brief.md"
	if verified.VerifiedReport != briefPath {
		t.Fatalf("自检轮回报的 verified_report = %q，期望 %q", verified.VerifiedReport, briefPath)
	}
	if verified.Report.SHA256 != d5BriefHash {
		t.Fatalf("自检轮重写后的简报哈希 %s 变了（同输入同 --now 必须逐字节相同）",
			verified.Report.SHA256)
	}

	body := readDemoFile(t, dir, briefPath)
	for _, want := range []string{
		"# 行业简报 · " + d5Topic + " · " + d5Now,
		"结论：" + d5Message,
		"本次未抓到的来源只作线索",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("简报缺少 %q：\n%s", want, body)
		}
	}
	for _, url := range d5Citable {
		if !strings.Contains(body, "[来源: "+url+"]") {
			t.Fatalf("简报缺少对已抓来源 %s 的引用：\n%s", url, body)
		}
	}
	for _, url := range d5Lead {
		if strings.Contains(body, "[来源: "+url+"]") {
			t.Fatalf("简报把线索 %s 当成了引用来源：\n%s", url, body)
		}
	}
	sourceSection := demoDocSection(body, "## 来源表", "## 线索（未抓正文，仅作线索，不得作为结论来源）")
	if rows := d5TableRows(sourceSection); rows != len(d5Citable) {
		t.Fatalf("来源表 %d 行，期望 %d 行：\n%s", rows, len(d5Citable), body)
	}
	leadSection := demoDocSection(body, "## 线索（未抓正文，仅作线索，不得作为结论来源）", "## 检索覆盖")
	if rows := d5TableRows(leadSection); rows != len(d5Lead) {
		t.Fatalf("线索表 %d 行，期望 %d 行：\n%s", rows, len(d5Lead), body)
	}
	for _, url := range append(append([]string{}, d5Citable...), d5Lead...) {
		if !strings.Contains(body, "| "+url+" |") {
			t.Fatalf("简报表格缺少来源 %s：\n%s", url, body)
		}
	}
	// 缺发布日期的来源写「未确认」；抓取失败的来源把报错原文抄进说明列。
	if line := d5LineWith(body, d5Lead[2]); !strings.Contains(line, "未确认") {
		t.Fatalf("缺发布日期的来源必须写「未确认」，实际 %q", line)
	}
	if line := d5LineWith(body, d5Lead[0]); !strings.Contains(line, "抓取失败："+d5FailReason) {
		t.Fatalf("抓取失败的来源说明列应抄报错原文，实际 %q", line)
	}
	for _, failure := range []string{d5Lead[0], d5FailReason} {
		if !strings.Contains(body, failure) {
			t.Fatalf("简报「失败」段缺少 %q：\n%s", failure, body)
		}
	}

	evidence := readDemoEvidence(t, f.h, runID)
	if evidence.Status != "completed" || evidence.Trigger != check.trigger {
		t.Fatalf("终态 = %s / %s，期望 completed / %s",
			evidence.Status, evidence.Trigger, check.trigger)
	}
	if evidence.ToolCalls != 15 {
		t.Fatalf("工具调用 %d 次，期望 15 次（3 检索 + 6 抓取 + 3 写 + 2 脚本 + 1 读）",
			evidence.ToolCalls)
	}
	// D5 同样走系统代投：先等投递行落终态，再数接收端（见 waitDeliveryStatus）。
	waitDeliveryStatus(t, f.h, runID, "sent", demoDeliveryTimeout)
	f.requireNotifyCount(t, runID, 0, check.notifyTotal)
	if status := demoDeliveryStatus(t, f.h, runID); status != "sent" {
		t.Fatalf("系统代投状态 = %q，期望 sent", status)
	}
	bodies := f.recv.received(demoReplyPath)
	if check.deliverIndex >= len(bodies) {
		t.Fatalf("接收端只收到 %d 条投递，缺第 %d 条", len(bodies), check.deliverIndex+1)
	}
	delivered := demoDeliveryText(t, bodies[check.deliverIndex])
	if !strings.HasPrefix(delivered, "【industry-brief】定时任务 daily-industry-brief 执行完成\n\n") {
		t.Fatalf("代投正文前缀不符：%q", delivered)
	}
	if !strings.Contains(delivered, d5Message) {
		t.Fatalf("代投正文应含简报结论句：%q", delivered)
	}
}

// testDemoIndustryBrief 覆盖 D5：Brave 三条查询 + Fetch 六次抓取（含一条 404）、
// 跨查询去重、可引用 / 线索分野、缺日期写「未确认」、未抓正文只作线索、引用自检通过，
// 以及自动触发与人工补跑在固定时钟下给出同一份产物。
// testDemoIndustryBrief covers D5 end to end.
func testDemoIndustryBrief(t *testing.T) {
	f := newDemoFixture(t)
	plan := demoBriefFixture(t, f)
	modelUID, channelUID := f.modelUID(t), f.channelUID(t)
	auto := time.Now().Add(15 * time.Second).In(demoCST())
	// 与 D2 同理：D5 也是系统代投，绑定里不出现渠道。
	// D5 also delivers through the scheduler, so its bindings carry no channel.
	dir := f.installAgent(t, "industry-brief", demoOptions{
		deliverTo: channelUID,
		cronAt:    auto,
		modelUID:  modelUID,
		cronOld:   "0 30 9 * * *",
	})
	const taskID = "daily-industry-brief"

	f.model.beginRun(dir, d5Steps(f, plan, "a")...)
	serve := f.serve(t)
	defer serve.stop(t)

	// —— 第一次：自动触发 ——
	firstRun := f.waitCronRun(t, taskID, 120*time.Second)
	d5RequireRun(t, f, dir, firstRun, d5RunCheck{trigger: "cron", deliverIndex: 0, notifyTotal: 1})

	// —— 第二次：人工补跑（固定输入 + 固定时钟 ⇒ 逐字节同一份产物） ——
	f.model.beginRun(dir, d5Steps(f, plan, "b")...)
	second := f.runScheduleNowOnce(t, taskID)
	d5RequireRun(t, f, dir, second.RunID,
		d5RunCheck{trigger: "manual", deliverIndex: 1, notifyTotal: 2})

	// 六段证据的「计划与时区」。
	schedules := listSchedules(t, f.addr)
	if len(schedules) != 1 || schedules[0].ID != taskID {
		t.Fatalf("任务清单 = %#v，期望只有 %s", schedules, taskID)
	}
	if schedules[0].Zone != "Asia/Shanghai" || schedules[0].Cron != cronAt(auto) {
		t.Fatalf("任务计划 = %q / %q，期望 Asia/Shanghai / %q",
			schedules[0].Zone, schedules[0].Cron, cronAt(auto))
	}
}

// ============================================================
// R0 旧 D1：三端点 + 一次通知汇总（不调度）
// ============================================================

// legacyD1Steps 是 R0 旧 D1 的剧本：三个 HTTP 端点（200 / 500 / 不可达）逐条取回读数，
// 写一份三行汇总，再发**一次**通知。
// legacyD1Steps reads the three endpoints and sends exactly one summary.
func legacyD1Steps(f *demoFixture) []demoStep {
	return []demoStep{
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			return demoCall("r0-1-ok", "http_get", map[string]any{
				"url": "http://localhost" + demoOKPath,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "http_get", "R0 第 1 步应是 http_get，实际 %q", name)
			return demoCall("r0-2-boom", "http_get", map[string]any{
				"url": "http://localhost" + demoBoomPath,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			return demoCall("r0-3-unreachable", "http_get", map[string]any{
				"url": demoUnreachableURL,
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			name, _ := d.lastTool()
			d.must(name == "http_get", "R0 第 3 步应是 http_get，实际 %q", name)
			return demoCall("r0-4-write", "write_file", map[string]any{
				"path":    "summary.md",
				"content": legacySummary(d),
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			return demoCall("r0-5-notify", "notify", map[string]any{
				"channel": demoNotifyName,
				"content": "旧 D1 三端点走查：200 正常 / 500 失败 / 不可达，详见 summary.md",
			})
		},
		func(d *demoTurn) testutil.FakeOpenAIResponse {
			return demoFinal("三端点走查完成，已汇总一次通知")
		},
	}
}

// legacySummary 把三次 http_get 的结果原样汇成一份三行汇总。
// legacySummary folds the three readings into one summary.
func legacySummary(d *demoTurn) string {
	texts := d.toolTexts("http_get")
	if len(texts) != 3 {
		d.failf("R0 应有 3 次 http_get 结果，实际 %d 次", len(texts))
	}
	var b strings.Builder
	b.WriteString("# 旧 D1 三端点走查\n\n")
	for _, text := range texts {
		b.WriteString("- " + strings.ReplaceAll(strings.TrimSpace(text), "\n", " ") + "\n")
	}
	return b.String()
}

// TestLegacyD1_HttpEndpoints 是 R0 旧 D1 的回归：`http_get` 对 200 / 500 / 不可达三种
// 端点给出可区分的读数，再由一次 `notify` 汇总。**不调度**、不计入五 Demo。
// TestLegacyD1_HttpEndpoints covers the legacy D1 endpoint readings; it never schedules.
func TestLegacyD1_HttpEndpoints(t *testing.T) {
	f := newDemoFixture(t)
	modelUID, channelUID := f.modelUID(t), f.channelUID(t)
	dir := f.installAgent(t, "github-radar", demoOptions{
		notify:     true,
		cronAt:     time.Now().Add(6 * time.Hour).In(demoCST()),
		modelUID:   modelUID,
		channelUID: channelUID,
		cronOld:    "0 30 8 * * *",
	})
	f.model.beginRun(dir, legacyD1Steps(f)...)

	res := f.h.run(t, f.env, "chat", "--agent", "github-radar", "--message", "旧 D1 三端点走查")
	requireExit(t, res, 0, "旧 D1 走查")
	if left := f.model.scriptLeft(); left != 0 {
		t.Fatalf("剧本还剩 %d 步——Run 的轮次少于剧本", left)
	}

	db := openDB(t, f.h.dbPath())
	rows := queryRows(t, db,
		`SELECT input_json, result_json, status, error_kind, error_message
		   FROM tool_invocations WHERE tool_name = 'http_get' ORDER BY id`)
	if len(rows) != 3 {
		t.Fatalf("http_get 调用 %d 次，期望 3 次：%v", len(rows), rows)
	}
	var inputs []string
	for _, row := range rows {
		inputs = append(inputs, stringValue(t, row, "input_json"))
	}
	for i, want := range []string{demoOKPath, demoBoomPath, demoUnreachableURL} {
		if !strings.Contains(inputs[i], want) {
			t.Fatalf("第 %d 次 http_get 的目标不含 %q：%s", i+1, want, inputs[i])
		}
	}
	if status, _ := rows[0]["status"].(string); status != "success" {
		t.Fatalf("200 端点应记 success，实际 %q", status)
	}
	if body := toolResultContent(t, stringValue(t, rows[0], "result_json")); !strings.Contains(body, "HTTP 200") {
		t.Fatalf("200 端点结果应是 2xx 读数，实际 %q", body)
	}
	// 三档读数的判据（模块卡 §2.5.1）：4xx/5xx 与 DNS 失败都归 `external_error`，
	// 文案分别是 `HTTP <code>：<host>` 与 `DNS 解析失败：<target>`。
	// Both the 5xx reading and the DNS failure classify as external_error.
	for i, want := range []struct{ fragment string }{
		{"HTTP 500：localhost"},
		{"DNS 解析失败"},
	} {
		row := rows[i+1]
		if status, _ := row["status"].(string); status != "failed" {
			t.Fatalf("第 %d 次 http_get 状态 = %q，期望 failed", i+2, status)
		}
		if kind, _ := row["error_kind"].(string); kind != "external_error" {
			t.Fatalf("第 %d 次 http_get error_kind = %q，期望 external_error", i+2, kind)
		}
		message, _ := row["error_message"].(string)
		if !strings.Contains(message, want.fragment) {
			t.Fatalf("第 %d 次 http_get 失败原文应含 %q，实际 %q", i+2, want.fragment, message)
		}
	}
	if got := f.recv.count(demoReplyPath); got != 1 {
		t.Fatalf("R0 走查只发一次通知，接收端实际收到 %d 次", got)
	}
	if got := f.recv.count(demoBoomPath); got != 1 {
		t.Fatalf("500 端点应被请求一次，实际 %d 次", got)
	}
	summary, err := os.ReadFile(filepath.Join(dir, "summary.md"))
	if err != nil {
		t.Fatalf("读汇总失败: %v", err)
	}
	if strings.Count(string(summary), "\n- ") != 3 {
		t.Fatalf("汇总应有三行读数：\n%s", summary)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM scheduled_tasks`); n != 0 {
		t.Fatalf("R0 走查不调度，却登记了 %d 个任务", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions`); n != 0 {
		t.Fatalf("R0 走查不调度，却留下 %d 条执行行", n)
	}
}

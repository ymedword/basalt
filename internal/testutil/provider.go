package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ymedword/basalt/internal/core"
)

// FakeStep 是 FakeProvider 一脚本步：回放一条响应、一个错误，或阻塞到 ctx 取消。
// FakeStep is one script step for FakeProvider: replay a response, replay an error,
// or block until the context is done.
type FakeStep struct {
	Response core.ChatResponse
	Err      error
	Block    bool // 阻塞直到 ctx.Done()（取消用例）
}

// FakeProvider 是 core.ProviderClient 的固定假实现：按脚本回放响应 / 错误、记录收到的请求、
// 支持阻塞到 ctx 取消（取消路径用例）。开发操作 §8.1 第 2 项：假 Provider 随 US-2 接口落位、后续 US 复用。
// FakeProvider is the scripted fake implementation of core.ProviderClient: it replays
// responses and errors, records the requests it received, and can block until the
// context is done for cancellation paths.
//
// 并发安全：用例会把并发调用与取消路径放进同一个测试（-race 判卷）。
// Concurrency-safe: tests exercise concurrent calls and cancellation under -race.
type FakeProvider struct {
	mu      sync.Mutex
	steps   []FakeStep
	calls   int
	reqs    []core.ChatRequest
	started map[int]chan struct{} // 第 n 次调用的进入信号（n 从 1 起）
}

// NewFakeProvider 以脚本构造假客户端；第 n 次调用回放第 n 步。
// NewFakeProvider builds the fake client from a script; the nth call replays the nth step.
func NewFakeProvider(steps ...FakeStep) *FakeProvider {
	return &FakeProvider{steps: steps, started: map[int]chan struct{}{}}
}

// Chat 记录请求、关闭第 n 次调用的进入信号，然后按脚本回放；脚本用尽即报错（防用例少写一步而静默通过）。
// Chat records the request, closes the start signal for this call, then replays the
// script. Running past the end of the script is an error: a test that under-scripts
// its fake should fail loudly rather than pass quietly.
func (f *FakeProvider) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.reqs = append(f.reqs, req)
	signal, ok := f.started[n]
	var step FakeStep
	if n <= len(f.steps) {
		step = f.steps[n-1]
	}
	f.mu.Unlock()

	if ok {
		close(signal)
	}
	if n > len(f.steps) {
		return core.ChatResponse{}, &unexpectedCallError{call: n, scripted: len(f.steps)}
	}

	if step.Block {
		<-ctx.Done()
		return core.ChatResponse{}, ctx.Err()
	}
	if step.Err != nil {
		return core.ChatResponse{}, step.Err
	}
	return step.Response, nil
}

// CallCount 返回已收到的调用次数。
// CallCount returns how many calls have arrived so far.
func (f *FakeProvider) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// Requests 返回收到的请求（按到达顺序）——返回副本，调用方可安全读。
// Requests returns the received requests in arrival order as a copy.
func (f *FakeProvider) Requests() []core.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.ChatRequest, len(f.reqs))
	copy(out, f.reqs)
	return out
}

// Started 返回第 n 次调用（n 从 1 起）**进入时**关闭的信号通道——取消 / 并发用例的
// 同步基元：测试等它而不是 time.Sleep（Sleep 会在"预写尚未完成"时取消，用例走成
// 另一条分支；见 §4.3 的取消用例注释）。通道在首次请求时创建，可先于调用取用。
// Started returns a channel closed when the nth call enters. Tests wait on it instead
// of sleeping: a sleep may cancel the call before the audit pre-write finished and send
// the case down the wrong branch.
func (f *FakeProvider) Started(n int) <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	signal, ok := f.started[n]
	if !ok {
		signal = make(chan struct{})
		f.started[n] = signal
	}
	return signal
}

// unexpectedCallError 报告脚本用尽——用例写漏了一步，不是被测代码的错。
// unexpectedCallError reports an exhausted script: the test under-scripted its fake.
type unexpectedCallError struct {
	call     int
	scripted int
}

func (e *unexpectedCallError) Error() string {
	return fmt.Sprintf("testutil: FakeProvider 脚本用尽——第 %d 次调用无脚本步（已脚本 %d 步）", e.call, e.scripted)
}

// FakeOpenAIResponse 是假服务的一脚本步：HTTP 状态与响应体。
// FakeOpenAIResponse is one script step of the fake service: an HTTP status and a body.
type FakeOpenAIResponse struct {
	StatusCode int    // 0 → 200
	Body       string // 原样响应体（含畸形 JSON 用例）
}

// FakeOpenAIServer 是 OpenAI 兼容协议的回环假服务：按脚本回放 HTTP 状态与响应体，
// 并**记录收到的请求体**（开发操作 §5.3 第 3 项的双向断言口径——遇预期之外的请求即失败，
// 专防"库记录齐了、发给模型的协议错了"）。
// FakeOpenAIServer is a loopback fake of the OpenAI-compatible protocol: it replays
// HTTP statuses and bodies and records the request bodies it received. Requests that
// fall outside the script fail the test, which is what catches "the call was recorded
// but the protocol sent was wrong".
type FakeOpenAIServer struct {
	t       *testing.T
	script  []FakeOpenAIResponse
	calls   atomic.Int64
	mu      sync.Mutex
	reqs    []map[string]any
	server  *httptest.Server
	decoded bool // 请求体解码是否已失败过（避免刷屏）
}

// NewFakeOpenAIServer 起回环假服务并登记清理；第 n 次请求回放第 n 步脚本。
// NewFakeOpenAIServer starts the loopback fake service; the nth request replays the
// nth script step.
func NewFakeOpenAIServer(t *testing.T, script ...FakeOpenAIResponse) *FakeOpenAIServer {
	t.Helper()
	s := &FakeOpenAIServer{t: t, script: script}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

// URL 返回假服务地址（供 provider.Options.BaseURL 使用）；无路径——路径由客户端拼接。
// URL returns the fake service address for provider.Options.BaseURL; it carries no path.
func (s *FakeOpenAIServer) URL() string { return s.server.URL }

// Requests 返回收到的请求体（已解码为通用映射，便于逐键断言）。
// Requests returns the received request bodies decoded into generic maps.
func (s *FakeOpenAIServer) Requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.reqs))
	copy(out, s.reqs)
	return out
}

// handle 记录请求体并按脚本回放；方法 / 路径 / 脚本用尽三类"预期之外"都判失败但仍响应
// （让被测对象走到它自己的错误路径，而不是卡在连接上）。
// handle records the body and replays the script; method, path and exhausted-script
// surprises fail the test but still answer, so the code under test follows its own
// error path instead of hanging on the connection.
func (s *FakeOpenAIServer) handle(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()
	if r.Method != http.MethodPost {
		s.t.Errorf("假 OpenAI 服务：方法必须是 POST，实际 %s", r.Method)
	}
	if r.URL.Path != "/chat/completions" {
		s.t.Errorf("假 OpenAI 服务：路径必须是 /chat/completions，实际 %s", r.URL.Path)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("假 OpenAI 服务：读请求体失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		s.mu.Lock()
		first := !s.decoded
		s.decoded = true
		s.mu.Unlock()
		if first {
			s.t.Errorf("假 OpenAI 服务：请求体不是 JSON 对象: %v（原文 %d 字节）", err, len(body))
		}
		decoded = map[string]any{}
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, decoded)
	s.mu.Unlock()

	n := int(s.calls.Add(1))
	if n > len(s.script) {
		s.t.Errorf("假 OpenAI 服务：脚本用尽——第 %d 次请求无脚本步（已脚本 %d 步）", n, len(s.script))
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := io.WriteString(w, `{"error":{"message":"fake server script exhausted"}}`); err != nil {
			s.t.Errorf("假 OpenAI 服务：写响应失败: %v", err)
		}
		return
	}

	step := s.script[n-1]
	status := step.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, step.Body); err != nil {
		s.t.Errorf("假 OpenAI 服务：写响应失败: %v", err)
	}
}

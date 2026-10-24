//go:build e2e

package e2e

// 本文件承载 US-9 HTTP 面的进程级回填（US-15 模块卡 §2.6.9 的十八项）：
// 每条历史人工项都以**真实二进制 + 回环地址 + 临时工作区**重放一遍，断言只读
// 标准流、退出码、HTTP 响应与工作区 SQLite 库。夹具不复用 `p.start`：Windows 上
// `Process.Signal` 只支持 Kill，优雅停机需要一个独立进程组 + Ctrl+Break 事件，
// 见 startGrouped / interrupt。
// This file backfills US-9's HTTP face at process level (module card §2.6.9, 18 items).
// Each historical manual item is replayed against the real binary on loopback addresses
// with a temporary workspace; assertions read the streams, the exit code, the HTTP
// responses, and the workspace SQLite database only.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// US-9 的冻结字面量只在这里定义一次（与 internal/web 的同名常量逐字一致）。
// The frozen US-9 literals are defined once here, verbatim from internal/web.
const (
	webBusyMessage      = "该会话有正在执行的任务，请等待完成或稍后重试"
	webArchivedMessage  = "会话已归档，不可追加消息"
	webOriginDenied     = "跨源请求被拒绝"
	webMessageCapText   = "消息超过 32768 字节上限"
	webBodyCapText      = "请求体超过 1048576 字节上限"
	webIterationMessage = "运行达到上限：超过最大迭代次数（可调配置键：budget.max_iterations）"
	webReasonIteration  = "超过最大迭代次数"
	webReasonFailed     = "运行失败"
	webContentType      = "application/json; charset=utf-8"

	// createNewProcessGroup 是 Windows CREATE_NEW_PROCESS_GROUP 标志位：子进程自成一个
	// 控制台进程组，Ctrl+Break 事件才能只投递给它而不波及测试进程。
	// createNewProcessGroup isolates the child in its own console process group so the
	// Ctrl+Break event reaches it alone.
	createNewProcessGroup = 0x00000200
)

// TestBackfill_Web 是 US-9 进程级回填的总入口；子测试名与模块卡 §2.6.9 逐行对应。
// TestBackfill_Web is the US-9 process-level backfill; subtest names map one-to-one
// onto module card §2.6.9.
func TestBackfill_Web(t *testing.T) {
	t.Run("listen_addr", testWebListenAddr)
	t.Run("body_byte_limit", testWebBodyByteLimit)
	t.Run("concurrency_cap", testWebConcurrencyCap)
	t.Run("session_409", testWebSessionConflict)
	t.Run("archive_restart", testWebArchiveRestart)
	t.Run("cross_origin", testWebCrossOrigin)
	t.Run("archive_inflight", testWebArchiveInflight)
	t.Run("redaction", testWebRedaction)
	t.Run("release_order", testWebReleaseOrder)
	t.Run("tool_catalog", testWebToolCatalog)
	t.Run("session_id", testWebSessionID)
	t.Run("budget_limit", testWebBudgetLimit)
	t.Run("credential_face", testWebCredentialFace)
	t.Run("curl_flow", testWebCurlFlow)
	t.Run("concurrent_409", testWebConcurrentConflict)
	t.Run("stateless_invoke", testWebStatelessInvoke)
	t.Run("listen_addr_override", testWebListenAddrOverride)
	t.Run("gateway_lifecycle", testWebGatewayLifecycle)
}

// ---------------------------------------------------------------------------
// HTTP 读数
// ---------------------------------------------------------------------------

// webEnvelope 是统一信封的判卷侧形状（data 保留原文，缺失与 null 都可判）。
// webEnvelope is the verdict-side envelope; data stays raw so absence is observable.
type webEnvelope struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	Timestamp string          `json:"timestamp"`
}

// webResponse 是一次 HTTP 读数的完整记录。
// webResponse records one HTTP reading.
type webResponse struct {
	Status  int
	Header  http.Header
	Raw     string
	Env     webEnvelope
	HasData bool
}

// webSession 是会话视图（摘要字段 + 可选消息窗口）。
// webSession is the session view (summary fields plus an optional message window).
type webSession struct {
	SessionID    string       `json:"session_id"`
	ProfileName  string       `json:"profile_name"`
	Channel      string       `json:"channel"`
	UserID       string       `json:"user_id"`
	Status       string       `json:"status"`
	MessageCount int          `json:"message_count"`
	CreatedAt    string       `json:"created_at"`
	LastActiveAt string       `json:"last_active_at"`
	Messages     []webMessage `json:"messages"`
}

// webMessage 是历史窗口里的一条消息。
// webMessage is one message in the history window.
type webMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id"`
	CreatedAt  string `json:"created_at"`
}

// webRun 是运行结果视图。
// webRun is the run result view.
type webRun struct {
	RunID      string `json:"run_id"`
	SessionID  string `json:"session_id"`
	Agent      string `json:"agent"`
	Channel    string `json:"channel"`
	Trigger    string `json:"trigger"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	FinalText  string `json:"final_text"`
	Iterations int    `json:"iterations"`
	ToolCalls  int    `json:"tool_calls"`
	ConfigHash string `json:"config_hash"`
}

// webResult 把一次并发请求的结果（响应或传输错误）打包过通道。
// webResult carries one concurrent request outcome across a channel.
type webResult struct {
	resp *webResponse
	err  error
}

// doWebRequest 发一次请求并解析信封；不触碰 *testing.T，可在子 goroutine 里调用。
// doWebRequest performs one request and parses the envelope; it never touches
// *testing.T, so it is safe off the test goroutine.
func doWebRequest(ctx context.Context, method, addr, path, body string, header map[string]string) (*webResponse, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// T025 起除健康检查外的端点一律要认证：夹具默认带上管理面 token，个别用例
	// 仍可通过 header 覆盖（例如将来的缺凭据 / 错凭据场景）。
	// Every non-public endpoint requires authentication since T025; the token is set
	// centrally and an explicit header entry may still override it.
	req.Header.Set("Authorization", "Bearer "+e2eAdminToken)
	for key, value := range header {
		req.Header.Set(key, value)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读响应失败（HTTP %d）: %w", resp.StatusCode, err)
	}
	out := &webResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Raw: string(raw)}
	if err := json.Unmarshal(raw, &out.Env); err == nil {
		out.HasData = len(out.Env.Data) > 0 && string(out.Env.Data) != "null"
	}
	return out, nil
}

// webRequest 在主 goroutine 上发一次请求，传输失败即判红。
// webRequest performs one request on the test goroutine; transport errors fail the test.
func webRequest(t *testing.T, addr, method, path, body string, header map[string]string) *webResponse {
	t.Helper()
	resp, err := doWebRequest(context.Background(), method, addr, path, body, header)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	return resp
}

// requireWebStatus 断言 HTTP 状态、错误码与（非空时）固定文案。
// requireWebStatus asserts the status, the code, and—when given—the exact message.
func requireWebStatus(t *testing.T, resp *webResponse, status int, code, message, label string) {
	t.Helper()
	if resp.Status != status {
		t.Fatalf("%s：HTTP = %d，期望 %d（信封 %s）", label, resp.Status, status, resp.Raw)
	}
	if resp.Env.Code != code {
		t.Fatalf("%s：code = %q，期望 %q（信封 %s）", label, resp.Env.Code, code, resp.Raw)
	}
	if message != "" && resp.Env.Message != message {
		t.Fatalf("%s：message = %q，期望 %q", label, resp.Env.Message, message)
	}
}

// decodeWebData 把信封 data 解析进 dst；data 缺失即判红（成功响应必须带载荷）。
// decodeWebData decodes the envelope data; a missing payload fails the test.
func decodeWebData(t *testing.T, resp *webResponse, dst any, label string) {
	t.Helper()
	if !resp.HasData {
		t.Fatalf("%s：响应缺少 data：%s", label, resp.Raw)
	}
	if err := json.Unmarshal(resp.Env.Data, dst); err != nil {
		t.Fatalf("%s：data 不是预期 JSON：%v（原文 %s）", label, err, resp.Raw)
	}
}

// requireEnvelopeShape 断言信封的固定外形：Content-Type 与 RFC3339 UTC 时间戳。
// requireEnvelopeShape asserts the fixed envelope shape: content type and RFC3339 UTC.
func requireEnvelopeShape(t *testing.T, resp *webResponse, label string) {
	t.Helper()
	if got := resp.Header.Get("Content-Type"); got != webContentType {
		t.Fatalf("%s：Content-Type = %q，期望 %q", label, got, webContentType)
	}
	if resp.Env.Timestamp == "" {
		t.Fatalf("%s：信封缺少 timestamp：%s", label, resp.Raw)
	}
	if _, err := time.Parse(time.RFC3339, resp.Env.Timestamp); err != nil {
		t.Fatalf("%s：timestamp 不是 RFC3339：%q（%v）", label, resp.Env.Timestamp, err)
	}
	if !strings.HasSuffix(resp.Env.Timestamp, "Z") {
		t.Fatalf("%s：timestamp 必须是 UTC（Z 结尾），实际 %q", label, resp.Env.Timestamp)
	}
}

// webCreateSession 建会话并按 data 解析；额外字段（channel / user_id）经 fields 传入。
// webCreateSession creates a session; extra fields (channel / user_id) arrive via fields.
func webCreateSession(t *testing.T, addr, agent string, fields map[string]string) webSession {
	t.Helper()
	body := map[string]string{"agent": agent}
	for key, value := range fields {
		body[key] = value
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化建会话请求失败: %v", err)
	}
	resp := webRequest(t, addr, http.MethodPost, "/api/v1/sessions", string(raw), nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "建会话")
	var out webSession
	decodeWebData(t, resp, &out, "建会话")
	if out.SessionID == "" {
		t.Fatalf("建会话：session_id 为空（%s）", resp.Raw)
	}
	return out
}

// webGetSession 读会话详情（含消息窗口）。
// webGetSession reads the session detail including the message window.
func webGetSession(t *testing.T, addr, id string) webSession {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/sessions/"+id, "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读会话")
	var out webSession
	decodeWebData(t, resp, &out, "读会话")
	return out
}

// webPostMessage 发一条消息；结果（含 400 / 409）交给调用方逐字断言。
// webPostMessage posts one message; the caller judges the result, including 400 / 409.
func webPostMessage(t *testing.T, addr, sessionID, message string) *webResponse {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		t.Fatalf("序列化消息失败: %v", err)
	}
	return webRequest(t, addr, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", string(raw), nil)
}

// webArchive 归档会话。
// webArchive archives one session.
func webArchive(t *testing.T, addr, sessionID string) *webResponse {
	t.Helper()
	return webRequest(t, addr, http.MethodDelete, "/api/v1/sessions/"+sessionID, "", nil)
}

// webInvoke 发一次无状态调用。
// webInvoke performs one stateless invoke.
func webInvoke(t *testing.T, addr, agent, message string) *webResponse {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		t.Fatalf("序列化消息失败: %v", err)
	}
	return webRequest(t, addr, http.MethodPost, "/api/v1/agents/"+agent+"/invoke", string(raw), nil)
}

// waitWebResult 等一条并发请求结果；传输错误即判红。
// waitWebResult waits for one concurrent outcome; transport errors fail the test.
func waitWebResult(t *testing.T, ch <-chan webResult, timeout time.Duration, label string) *webResponse {
	t.Helper()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("%s：请求失败: %v", label, got.err)
		}
		return got.resp
	case <-time.After(timeout):
		t.Fatalf("%s：等待响应超时（%s）", label, timeout)
		return nil
	}
}

// writeWebAgent 落一份可装载的 AGENT.md（provider 指向夹具假模型）。
// writeWebAgent writes one loadable AGENT.md pointing at the fixture provider.
func writeWebAgent(t *testing.T, h *harness, name string, tools []string) {
	t.Helper()
	h.writeAgent(t, name, agentFrontmatter(name, tools), "回填夹具 Agent")
}

// webHealthStatus 读一次健康检查，返回原始响应体。
// webHealthStatus performs one health probe and returns the raw body.
func webHealthStatus(addr string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + addr + "/api/v1/health")
	if err != nil {
		return "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// waitHealthHTTP 轮询健康检查直到就绪；进程提前退出或超时都判红。
// waitHealthHTTP polls until the health endpoint answers; early exit and timeout fail.
func waitHealthHTTP(t *testing.T, p *groupedProcess, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			t.Fatalf("进程在 %s 就绪前退出（%d）\n%s", addr, p.exitCodeNow(), p.output())
		default:
		}
		if _, err := webHealthStatus(addr); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待 %s 就绪超时（%s，最后一次错误 %v）\n%s", addr, timeout, lastErr, p.output())
}

// waitListenerClosed 轮询到监听端口拒绝连接（停机第一阶段关闭监听的观测点）。
// waitListenerClosed polls until the listener refuses connections.
func waitListenerClosed(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(readinessPoll)
	}
	t.Fatalf("监听 %s 未在 %s 内关闭", addr, timeout)
}

// ---------------------------------------------------------------------------
// 带进程组的常驻进程（Windows 优雅停机）
// ---------------------------------------------------------------------------

// lockedBuffer 是并发安全的输出缓冲：进程运行期间也可以被轮询读取。
// lockedBuffer is a concurrency-safe stream buffer readable while the process runs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write 实现 io.Writer。
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String 返回当前累计输出。
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// groupedProcess 是一次常驻进程调用的句柄：输出可轮询、可投递优雅中断。
// groupedProcess is one long-lived invocation whose output is pollable and which can
// receive a graceful interrupt.
type groupedProcess struct {
	cmd    *exec.Cmd
	stdout *lockedBuffer
	stderr *lockedBuffer
	done   chan struct{}

	mu       sync.Mutex
	exitCode int
}

// groupedSysProcAttr 在 Windows 上把子进程放进独立控制台进程组；其他平台返回空属性。
// 用反射设置 CreationFlags：syscall.SysProcAttr 在 Linux 上**没有**这个字段，直接写
// 字段会让本文件在 Linux 上无法编译，而 CI 要在 Linux 上跑同一套用例。
// groupedSysProcAttr isolates the child in its own console process group on Windows.
// The flag is set through reflection because Linux's SysProcAttr has no such field.
func groupedSysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{}
	if runtime.GOOS != "windows" {
		return attr
	}
	field := reflect.ValueOf(attr).Elem().FieldByName("CreationFlags")
	if field.IsValid() && field.CanSet() {
		field.SetUint(createNewProcessGroup)
	}
	return attr
}

// startGrouped 拉起一条常驻进程并登记清理兜底；输出进并发安全缓冲。
// startGrouped launches a long-lived process with a kill-on-cleanup fallback.
func startGrouped(t *testing.T, h *harness, env []string, args ...string) *groupedProcess {
	t.Helper()
	cmd := h.command(t, context.Background(), env, args...)
	cmd.SysProcAttr = groupedSysProcAttr()
	p := &groupedProcess{
		cmd:      cmd,
		stdout:   &lockedBuffer{},
		stderr:   &lockedBuffer{},
		done:     make(chan struct{}),
		exitCode: -1,
	}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 %q 失败: %v", strings.Join(args, " "), err)
	}
	go func() {
		_ = cmd.Wait()
		p.mu.Lock()
		if cmd.ProcessState != nil {
			p.exitCode = cmd.ProcessState.ExitCode()
		}
		p.mu.Unlock()
		close(p.done)
	}()
	t.Cleanup(p.kill)
	return p
}

// exitCodeNow 返回当前已知的退出码（未退出为 -1）。
func (p *groupedProcess) exitCodeNow() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

// output 返回标准输出与标准错误的合并读数。
func (p *groupedProcess) output() string {
	return "stdout:\n" + p.stdout.String() + "\nstderr:\n" + p.stderr.String()
}

// combined 返回两路输出的无标签拼接（供片段断言）。
func (p *groupedProcess) combined() string {
	return p.stdout.String() + p.stderr.String()
}

// waitOutput 轮询到输出里出现片段为止；进程提前退出与超时都判红。
// waitOutput polls until the fragment appears; early exit and timeout fail.
func (p *groupedProcess) waitOutput(t *testing.T, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(p.combined(), needle) {
			return
		}
		select {
		case <-p.done:
			t.Fatalf("进程在输出 %q 之前退出（%d）\n%s", needle, p.exitCodeNow(), p.output())
		case <-time.After(readinessPoll):
		}
	}
	t.Fatalf("等待输出 %q 超时（%s）\n%s", needle, timeout, p.output())
}

// waitExit 等进程退出并返回退出码；超时先杀再判红。
// waitExit waits for the exit code; a timeout kills the process first.
func (p *groupedProcess) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		p.kill()
		<-p.done
		t.Fatalf("进程在 %s 内未退出\n%s", timeout, p.output())
	}
	return p.exitCodeNow()
}

// kill 兜底杀进程；已退出时是幂等空操作。
func (p *groupedProcess) kill() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
}

// stop 优雅停机并校验退出码 0；重复调用是幂等的（已退出时只校验，不重复投递）。
// stop shuts the process down gracefully and asserts exit code 0; it is idempotent.
func (p *groupedProcess) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	default:
		p.interrupt(t)
	}
	if code := p.waitExit(t, 30*time.Second); code != 0 {
		t.Errorf("优雅停机退出码 = %d，期望 0\n%s", code, p.output())
	}
}

// interrupt 投递一次优雅中断（非 Windows 平台 = SIGINT；Windows = Ctrl+Break）。
// interrupt delivers a graceful interrupt (SIGINT elsewhere, Ctrl+Break on Windows).
func (p *groupedProcess) interrupt(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatalf("进程已退出（%d），无法投递优雅中断", p.exitCodeNow())
	default:
	}
	if runtime.GOOS == "windows" {
		interruptWindows(t, p.cmd.Process.Pid)
		return
	}
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("发送 SIGINT 失败: %v", err)
	}
}

// interruptWindows 经 PowerShell P/Invoke 向子进程组投递 CTRL_BREAK_EVENT（事件号 1）。
// Go 运行时把 Ctrl+Break 映射为 os.Interrupt，serve / gateway 的单一信号源据此
// 走五阶段优雅停机。探针实测：进程以 0 退出并打出 msg=stop。
// interruptWindows delivers CTRL_BREAK_EVENT to the child's process group through
// PowerShell P/Invoke; the Go runtime maps it to os.Interrupt.
func interruptWindows(t *testing.T, pid int) {
	t.Helper()
	script := `Add-Type -Namespace W -Name K -MemberDefinition '[DllImport("kernel32.dll", SetLastError=true)] public static extern bool GenerateConsoleCtrlEvent(uint dwCtrlEvent, uint dwProcessGroupId);'; if (-not [W.K]::GenerateConsoleCtrlEvent(1, ` +
		fmt.Sprint(pid) + `)) { exit 3 }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("经 PowerShell 投递 Ctrl+Break 失败（pid %d）: %v\n%s", pid, err, out)
	}
}

// ---------------------------------------------------------------------------
// 阻塞模型：让一次运行停在模型调用上，制造确定的在途窗口
// ---------------------------------------------------------------------------

// barrierModel 是回环假模型：请求进入后阻塞，直到 open 放行或客户端断开；
// setFailure 置位时改为立即返回 500（脱敏用例的失败来源）。
// barrierModel blocks every request until open or client disconnect; setFailure
// switches it to an immediate 500 for the redaction case.
type barrierModel struct {
	t       *testing.T
	server  *httptest.Server
	release chan struct{}
	opened  sync.Once

	mu    sync.Mutex
	fail  string
	calls int
}

// newBarrierModel 起回环阻塞模型。
// newBarrierModel starts the loopback barrier model.
func newBarrierModel(t *testing.T) *barrierModel {
	t.Helper()
	m := &barrierModel{t: t, release: make(chan struct{})}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(func() {
		m.open()
		m.server.Close()
	})
	return m
}

// URL 返回模型地址（供 provider.base_url 使用）。
func (m *barrierModel) URL() string { return m.server.URL }

// setFailure 设置 500 响应里的 error.message（空串 = 恢复阻塞模式）。
// setFailure sets the error message for the 500 branch; empty restores blocking.
func (m *barrierModel) setFailure(message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = message
}

// callCount 返回已收到的模型调用次数。
func (m *barrierModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// open 放行所有阻塞中的请求（幂等）。
// open releases every blocked request (idempotent).
func (m *barrierModel) open() {
	m.opened.Do(func() { close(m.release) })
}

// waitCalls 轮询到模型调用次数达到 want 为止——「运行已进入模型」的确定性观测点。
// waitCalls polls until the model received want calls: the deterministic "in flight"
// observation point.
func (m *barrierModel) waitCalls(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m.callCount() >= want {
			return
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待模型第 %d 次调用超时（%s，当前 %d 次）", want, timeout, m.callCount())
}

// handle 记录调用、按 fail 分支答复，否则阻塞到放行或客户端断开。
// handle records the call, answers the failure branch, or blocks until release.
func (m *barrierModel) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
		m.t.Errorf("阻塞模型：方法与路径必须是 POST /chat/completions，实际 %s %s", r.Method, r.URL.Path)
	}
	if _, err := io.ReadAll(r.Body); err != nil {
		m.t.Errorf("阻塞模型：读请求体失败: %v", err)
	}
	m.mu.Lock()
	m.calls++
	fail := m.fail
	m.mu.Unlock()
	if fail != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := io.WriteString(w, mustJSON(map[string]any{
			"error": map[string]any{"message": fail},
		})); err != nil {
			m.t.Errorf("阻塞模型：写失败响应失败: %v", err)
		}
		return
	}
	select {
	case <-m.release:
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, textResponse("慢速完成").Body); err != nil {
		m.t.Errorf("阻塞模型：写响应失败: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 十八个子测试
// ---------------------------------------------------------------------------

// testWebListenAddr：缺省监听仅 127.0.0.1:8080（US-9 §6.1 第 1 项）。
// testWebListenAddr: the default listener is exactly 127.0.0.1:8080.
func testWebListenAddr(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		t.Skipf("默认端口 127.0.0.1:8080 已被占用，无法验证缺省监听（环境检查，非验收通过）：%v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatalf("释放默认端口失败: %v", err)
	}

	h := newHarness(t)
	initWorkspace(t, h)
	model := newFakeModel(t)
	h.writeConfig(t, configWithAdminAuth(model.URL()))

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, "127.0.0.1:8080", 30*time.Second)

	resp := webRequest(t, "127.0.0.1:8080", http.MethodGet, "/api/v1/health", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "缺省监听健康检查")
	requireEnvelopeShape(t, resp, "缺省监听健康检查")
	var health struct {
		Status string `json:"status"`
	}
	decodeWebData(t, resp, &health, "缺省监听健康检查")
	if health.Status != "ok" {
		t.Fatalf("健康检查 status = %q，期望 ok", health.Status)
	}
}

// testWebBodyByteLimit：消息 32768 通过 / 32769 400；请求体 1 MiB 上限（§6.1 第 2 项）。
// testWebBodyByteLimit: 32768 bytes pass, 32769 fail; the 1 MiB body cap holds.
func testWebBodyByteLimit(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t, textResponse("消息边界内完成"), textResponse("invoke 边界内完成"))
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	sess := webCreateSession(t, addr, "ops-agent", nil)
	within := strings.Repeat("a", 32768)
	over := strings.Repeat("a", 32769)

	resp := webPostMessage(t, addr, sess.SessionID, within)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "消息 32768 字节")
	var run webRun
	decodeWebData(t, resp, &run, "消息 32768 字节")
	if run.Status != "completed" {
		t.Fatalf("消息 32768 字节：status = %q，期望 completed", run.Status)
	}

	resp = webPostMessage(t, addr, sess.SessionID, over)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST", webMessageCapText, "消息 32769 字节")
	if resp.HasData {
		t.Fatalf("消息 32769 字节：失败响应不应带 data：%s", resp.Raw)
	}

	resp = webInvoke(t, addr, "ops-agent", within)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "invoke 32768 字节")
	run = webRun{}
	decodeWebData(t, resp, &run, "invoke 32768 字节")
	if run.Status != "completed" {
		t.Fatalf("invoke 32768 字节：status = %q，期望 completed", run.Status)
	}

	resp = webInvoke(t, addr, "ops-agent", over)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST", webMessageCapText, "invoke 32769 字节")

	huge := `{"message":"` + strings.Repeat("a", 1<<20) + `"}`
	resp = webRequest(t, addr, http.MethodPost, "/api/v1/agents/ops-agent/invoke", huge, nil)
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST", webBodyCapText, "请求体超 1 MiB")

	if got := len(model.Requests()); got != 2 {
		t.Fatalf("模型应只收到 2 次调用（两个边界内消息），实际 %d 次", got)
	}

	p.stop(t)
	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 2 {
		t.Fatalf("越界消息不得产生运行：runs 应有 2 行，实际 %d 行", n)
	}
}

// testWebConcurrencyCap：配置 1 时第二个并发运行得 429 并回显上限（§6.1 第 3 项）。
// testWebConcurrencyCap: with a cap of 1 the second concurrent run gets 429.
func testWebConcurrencyCap(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newBarrierModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr)+"server:\n  max_concurrent_runs: 1\n")
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	defer model.open()
	waitHealthHTTP(t, p, addr, 30*time.Second)

	first := make(chan webResult, 1)
	go func() {
		resp, err := doWebRequest(context.Background(), http.MethodPost,
			addr, "/api/v1/agents/ops-agent/invoke", `{"message":"占住唯一并发位"}`, nil)
		first <- webResult{resp: resp, err: err}
	}()
	model.waitCalls(t, 1, 30*time.Second)

	second := webInvoke(t, addr, "ops-agent", "第二个并发")
	requireWebStatus(t, second, http.StatusTooManyRequests, "TOO_MANY_RUNS",
		"实例并发上限 1，请稍后重试", "第二并发")
	if second.HasData {
		t.Fatalf("429 不应带 data：%s", second.Raw)
	}

	model.open()
	resp := waitWebResult(t, first, 60*time.Second, "第一并发")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "第一并发")
	var run webRun
	decodeWebData(t, resp, &run, "第一并发")
	if run.Status != "completed" {
		t.Fatalf("第一并发：status = %q，期望 completed", run.Status)
	}

	p.stop(t)
	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 1 {
		t.Fatalf("429 不得产生运行：runs 应有 1 行，实际 %d 行", n)
	}
}

// testWebSessionConflict：同会话第二个在途请求 409，文案逐字冻结（§6.1 第 4 项）。
// testWebSessionConflict: a second in-flight request on the same session gets 409.
func testWebSessionConflict(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newBarrierModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	defer model.open()
	waitHealthHTTP(t, p, addr, 30*time.Second)

	sess := webCreateSession(t, addr, "ops-agent", nil)
	first := make(chan webResult, 1)
	go func() {
		resp, err := doWebRequest(context.Background(), http.MethodPost,
			addr, "/api/v1/sessions/"+sess.SessionID+"/messages", `{"message":"占住会话"}`, nil)
		first <- webResult{resp: resp, err: err}
	}()
	model.waitCalls(t, 1, 30*time.Second)

	second := webPostMessage(t, addr, sess.SessionID, "同会话第二请求")
	requireWebStatus(t, second, http.StatusConflict, "SESSION_BUSY", webBusyMessage, "同会话 409")
	if second.HasData {
		t.Fatalf("409 不应带 data：%s", second.Raw)
	}

	model.open()
	resp := waitWebResult(t, first, 60*time.Second, "占位请求")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "占位请求")
	var run webRun
	decodeWebData(t, resp, &run, "占位请求")
	if run.Status != "completed" {
		t.Fatalf("占位请求：status = %q，期望 completed", run.Status)
	}

	p.stop(t)
	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 1 {
		t.Fatalf("409 不得产生运行：runs 应有 1 行，实际 %d 行", n)
	}
}

// testWebArchiveRestart：归档跨重启持久，重启后仍拒绝追加（§6.1 第 5 项）。
// testWebArchiveRestart: archiving survives a restart and still rejects appends.
func testWebArchiveRestart(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)
	sess := webCreateSession(t, addr, "ops-agent", nil)

	resp := webArchive(t, addr, sess.SessionID)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "归档")
	if resp.HasData {
		t.Fatalf("归档成功响应不应带 data：%s", resp.Raw)
	}
	if got := webGetSession(t, addr, sess.SessionID).Status; got != "archived" {
		t.Fatalf("归档后 status = %q，期望 archived", got)
	}

	p.stop(t)

	restartAddr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), restartAddr))
	p2 := startGrouped(t, h, fixtureEnv(), "serve")
	defer p2.stop(t)
	waitHealthHTTP(t, p2, restartAddr, 30*time.Second)

	got := webGetSession(t, restartAddr, sess.SessionID)
	if got.Status != "archived" {
		t.Fatalf("重启后 status = %q，期望 archived（归档必须持久）", got.Status)
	}

	resp = webPostMessage(t, restartAddr, sess.SessionID, "归档后追加")
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST", webArchivedMessage, "归档后追加")
	if got := len(model.Requests()); got != 0 {
		t.Fatalf("归档后追加不得触发模型调用，实际 %d 次", got)
	}

	p2.stop(t)

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status, archived_at FROM sessions WHERE session_id = ?`, sess.SessionID)
	if len(rows) != 1 {
		t.Fatalf("sessions 应有 1 行，实际 %d 行", len(rows))
	}
	if rows[0]["status"] != "archived" {
		t.Fatalf("库内 status = %v，期望 archived", rows[0]["status"])
	}
	if rows[0]["archived_at"] == nil {
		t.Fatalf("库内 archived_at 为空——归档时间未持久化")
	}
}

// testWebCrossOrigin：恶意 Origin 的写请求 403、GET 放行且无 ACAO（§6.1 第 7 项）。
// testWebCrossOrigin: cross-origin writes are rejected before the body is used;
// GET stays open and carries no ACAO header.
func testWebCrossOrigin(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t, textResponse("同源放行"))
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	sess := webCreateSession(t, addr, "ops-agent", nil)
	evil := map[string]string{"Origin": "http://evil.example"}

	resp := webRequest(t, addr, http.MethodPost,
		"/api/v1/sessions/"+sess.SessionID+"/messages", `{"message":"跨源写"}`, evil)
	requireWebStatus(t, resp, http.StatusForbidden, "BAD_REQUEST", webOriginDenied, "跨源 POST")
	if resp.HasData {
		t.Fatalf("403 不应带 data：%s", resp.Raw)
	}

	resp = webRequest(t, addr, http.MethodDelete, "/api/v1/sessions/"+sess.SessionID, "", evil)
	requireWebStatus(t, resp, http.StatusForbidden, "BAD_REQUEST", webOriginDenied, "跨源 DELETE")

	resp = webRequest(t, addr, http.MethodPost,
		"/api/v1/sessions/"+sess.SessionID+"/messages", `{"message":"跨站 fetch"}`,
		map[string]string{"Sec-Fetch-Site": "cross-site"})
	requireWebStatus(t, resp, http.StatusForbidden, "BAD_REQUEST", webOriginDenied, "Sec-Fetch-Site 跨站")

	// US-11 起管理面在跨源中间件之后还有 Origin 守卫：受保护端点的 Origin 必须
	// 完整匹配允许列表，因此带恶意 Origin 的 GET 现在是 403（US-9 阶段该处只拦写、
	// GET 放行；FR3 收紧为全端点判定）。文案由认证层给固定文案，不回命中规则。
	// Since US-11 the auth guard rejects any non-allowlisted Origin on protected
	// endpoints, so the evil-origin GET is now 403 instead of open.
	resp = webRequest(t, addr, http.MethodGet, "/api/v1/sessions/"+sess.SessionID, "", evil)
	requireWebStatus(t, resp, http.StatusForbidden, "FORBIDDEN", "管理面请求被拒绝", "跨源 GET")
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("GET 不得回 ACAO 头，实际 %q", got)
	}
	if resp.HasData {
		t.Fatalf("403 不应带 data：%s", resp.Raw)
	}

	// 同源写请求放行：Origin 必须与请求 Host 完全一致。
	resp = webRequest(t, addr, http.MethodPost,
		"/api/v1/sessions/"+sess.SessionID+"/messages", `{"message":"同源消息"}`,
		map[string]string{"Origin": "http://" + addr})
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "同源 POST")
	var run webRun
	decodeWebData(t, resp, &run, "同源 POST")
	if run.Status != "completed" {
		t.Fatalf("同源 POST：status = %q，期望 completed", run.Status)
	}
	if got := len(model.Requests()); got != 1 {
		t.Fatalf("被拒的跨源写不得触发模型调用：期望 1 次，实际 %d 次", got)
	}

	p.stop(t)
	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 1 {
		t.Fatalf("被拒的跨源写不得产生运行：runs 应有 1 行，实际 %d 行", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM sessions`); n != 1 {
		t.Fatalf("被拒的跨源写不得建会话：sessions 应有 1 行，实际 %d 行", n)
	}
}

// testWebArchiveInflight：在途运行挡住归档（409），归档先则追加被拒（§6.1 第 8 项）。
// testWebArchiveInflight: an in-flight run blocks archiving, and an archived session
// rejects appends without running anything.
func testWebArchiveInflight(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newBarrierModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	defer model.open()
	waitHealthHTTP(t, p, addr, 30*time.Second)

	sess := webCreateSession(t, addr, "ops-agent", nil)
	inflight := make(chan webResult, 1)
	go func() {
		resp, err := doWebRequest(context.Background(), http.MethodPost,
			addr, "/api/v1/sessions/"+sess.SessionID+"/messages", `{"message":"在途运行"}`, nil)
		inflight <- webResult{resp: resp, err: err}
	}()
	model.waitCalls(t, 1, 30*time.Second)

	blocked := webArchive(t, addr, sess.SessionID)
	requireWebStatus(t, blocked, http.StatusConflict, "SESSION_BUSY", webBusyMessage, "在途归档")
	if got := webGetSession(t, addr, sess.SessionID).Status; got != "active" {
		t.Fatalf("归档被拒后 status = %q，期望 active", got)
	}

	model.open()
	resp := waitWebResult(t, inflight, 60*time.Second, "在途运行")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "在途运行")
	var run webRun
	decodeWebData(t, resp, &run, "在途运行")
	if run.Status != "completed" {
		t.Fatalf("在途运行：status = %q，期望 completed", run.Status)
	}

	// 运行结束后在途计数归零：归档立即成功，之后的追加被 400 拒绝。
	resp = webArchive(t, addr, sess.SessionID)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "运行结束后归档")
	resp = webPostMessage(t, addr, sess.SessionID, "归档后追加")
	requireWebStatus(t, resp, http.StatusBadRequest, "BAD_REQUEST", webArchivedMessage, "归档后追加")
	if got := model.callCount(); got != 1 {
		t.Fatalf("归档后追加不得触发模型调用：期望 1 次，实际 %d 次", got)
	}

	p.stop(t)
	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 1 {
		t.Fatalf("被拒的归档与追加都不得产生运行：runs 应有 1 行，实际 %d 行", n)
	}
}

// testWebRedaction：失败原因的响应脱敏、HTTP 层失败诊断按 <path> 投影、库内保留原文
// （§6.1 第 9 项）。口径取自 US-9 约束 23：脱敏的**对象是 HTTP 层为 failed 记的那条
// WARN**；原始原因允许留在 `runs.reason` 与 500 的 ERROR 日志，提供方适配器自己的
// WARN 也保留原始 error——所以这里只对诊断行做「无哨兵」断言，不对整个进程输出。
// testWebRedaction: the sanitized object is the HTTP layer's failed-run WARN; the raw
// reason may live in runs.reason and other layers' logs, so only that line is checked.
func testWebRedaction(t *testing.T) {
	const marker = "INTERNAL-MARKER-7f3a"
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newBarrierModel(t)
	model.setFailure(`认证失败：C:\secret\` + marker + `.txt 不可读`)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	defer model.open()
	waitHealthHTTP(t, p, addr, 30*time.Second)
	sess := webCreateSession(t, addr, "ops-agent", nil)

	resp := webPostMessage(t, addr, sess.SessionID, "REDACT-FAIL")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "失败运行")
	var run webRun
	decodeWebData(t, resp, &run, "失败运行")
	if run.Status != "failed" {
		t.Fatalf("失败运行：status = %q，期望 failed", run.Status)
	}
	if run.Reason != webReasonFailed {
		t.Fatalf("失败运行：reason = %q，期望固定文案 %q", run.Reason, webReasonFailed)
	}
	requireAbsent(t, resp.Raw, marker, "失败响应脱敏")
	requireAbsent(t, resp.Raw, `C:\secret`, "失败响应不得回显绝对路径")

	// 客户端断开：运行取消、响应不写（handler 只记日志）。
	model.setFailure("")
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() {
		_, err := doWebRequest(ctx, http.MethodPost,
			addr, "/api/v1/sessions/"+sess.SessionID+"/messages", `{"message":"取消演示"}`, nil)
		cancelled <- err
	}()
	model.waitCalls(t, 2, 30*time.Second)
	cancel()
	select {
	case err := <-cancelled:
		if err == nil {
			t.Fatalf("客户端取消后不应收到响应")
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("客户端取消后请求未返回")
	}
	// handler 在运行收尾后才打这行日志——等到它，取消路径的审计行必然已落库。
	p.waitOutput(t, "客户端已断开，未写响应", 30*time.Second)

	p.stop(t)
	out := p.combined()
	var diagLines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "运行以 failed 结束") {
			diagLines = append(diagLines, line)
		}
	}
	if len(diagLines) != 1 {
		t.Fatalf("failed 诊断 WARN 应恰有 1 行，实际 %d 行\n%s", len(diagLines), out)
	}
	diag := diagLines[0]
	requireContains(t, diag, "diagnostic=", "failed 诊断键")
	requireContains(t, diag, "<path>", "绝对路径必须投影为 <path>")
	requireAbsent(t, diag, marker, "失败诊断脱敏")
	requireAbsent(t, diag, `C:\secret`, "失败诊断不得含原始路径")
	for _, key := range []string{"run_id=", "request_id=", "session_id=", "agent=ops-agent"} {
		requireContains(t, diag, key, "失败诊断关联键")
	}
	requireContains(t, out, "客户端已断开，未写响应", "客户端断开日志")

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status, reason FROM runs WHERE run_id = ?`, run.RunID)
	if len(rows) != 1 {
		t.Fatalf("失败运行应有 1 行 runs，实际 %d 行", len(rows))
	}
	if rows[0]["status"] != "failed" {
		t.Fatalf("库内 status = %v，期望 failed", rows[0]["status"])
	}
	reason, _ := rows[0]["reason"].(string)
	requireContains(t, reason, marker, "库内 runs.reason 保留原始原因（脱敏只发生在响应与日志面）")

	// 取消的运行：客户端断开后终态落库可能失败（FinishRun 用的是已取消的上下文）。
	// 设计口径（service.go:226）：落库失败不覆盖结果，行留 running 等下次启动的
	// 崩溃恢复扫描收尾。两种落库时序都合法，但**不得**停在 running。
	cancelledRows := queryRows(t, db,
		`SELECT session_id, status FROM runs WHERE run_id <> ?`, run.RunID)
	if len(cancelledRows) != 1 {
		t.Fatalf("取消运行应有 1 行 runs，实际 %d 行", len(cancelledRows))
	}
	if cancelledRows[0]["session_id"] != sess.SessionID {
		t.Fatalf("取消运行的 session_id = %v，期望 %s", cancelledRows[0]["session_id"], sess.SessionID)
	}
	switch cancelledRows[0]["status"] {
	case "cancelled", "running":
	default:
		t.Fatalf("取消运行的库内 status = %v，期望 cancelled（已落库）或 running（待恢复扫描）",
			cancelledRows[0]["status"])
	}

	// 重启后恢复扫描必须把仍为 running 的行收尾为 failed——这是「行留 running」的兑现。
	restartAddr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), restartAddr))
	p2 := startGrouped(t, h, fixtureEnv(), "serve")
	defer p2.stop(t)
	waitHealthHTTP(t, p2, restartAddr, 30*time.Second)
	p2.stop(t)

	closed := queryRows(t, db, `SELECT status, finished_at FROM runs WHERE run_id <> ?`, run.RunID)
	if len(closed) != 1 {
		t.Fatalf("重启后取消运行应有 1 行 runs，实际 %d 行", len(closed))
	}
	if closed[0]["status"] == "running" {
		t.Fatalf("重启后恢复扫描未收尾仍为 running 的运行行")
	}
	if closed[0]["finished_at"] == nil {
		t.Fatalf("重启后运行的 finished_at 仍为空——恢复扫描未写终态时刻")
	}
}

// testWebReleaseOrder：优雅停机先关监听、等在途运行收尾；exec 出错同样释放运行时
// （§6.1 第 10 项）。
// testWebReleaseOrder: graceful stop closes the listener first and lets the in-flight
// run finish; an exec error releases the runtime just the same.
func testWebReleaseOrder(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	firstAddr := freeLoopbackAddr(t)
	model := newBarrierModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), firstAddr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	defer model.open()
	waitHealthHTTP(t, p, firstAddr, 30*time.Second)
	sess := webCreateSession(t, firstAddr, "ops-agent", nil)

	inflight := make(chan webResult, 1)
	go func() {
		resp, err := doWebRequest(context.Background(), http.MethodPost,
			firstAddr, "/api/v1/sessions/"+sess.SessionID+"/messages", `{"message":"停机前在途"}`, nil)
		inflight <- webResult{resp: resp, err: err}
	}()
	model.waitCalls(t, 1, 30*time.Second)

	// 停机第一阶段：监听必须关闭，而在途运行仍在跑。
	p.interrupt(t)
	waitListenerClosed(t, firstAddr, 30*time.Second)
	model.open()

	resp := waitWebResult(t, inflight, 60*time.Second, "在途运行")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "在途运行收尾")
	var run webRun
	decodeWebData(t, resp, &run, "在途运行收尾")
	if run.Status != "completed" {
		t.Fatalf("在途运行：status = %q，期望 completed（停机不得取消在途运行）", run.Status)
	}
	if code := p.waitExit(t, 60*time.Second); code != 0 {
		t.Fatalf("优雅停机退出码 = %d，期望 0\n%s", code, p.output())
	}
	requireContains(t, p.combined(), "msg=stop", "运行时释放（调度器停止标记）")

	// 锁已释放：同工作区可以立刻重启。
	secondAddr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), secondAddr))
	p2 := startGrouped(t, h, fixtureEnv(), "serve")
	defer p2.stop(t)
	waitHealthHTTP(t, p2, secondAddr, 30*time.Second)
	p2.stop(t)

	// exec 出错路径：端口被测试进程占住 → serve 退出 1，但仍释放运行时（msg=stop）。
	busyAddr := freeLoopbackAddr(t)
	listener, err := net.Listen("tcp", busyAddr)
	if err != nil {
		t.Fatalf("占用回环端口失败: %v", err)
	}
	h.writeConfig(t, configWithAddr(model.URL(), busyAddr))
	res := h.run(t, fixtureEnv(), "serve")
	closeListener(t, listener)
	requireExit(t, res, 1, "端口占用停机")
	requireContains(t, res.Stdout+res.Stderr, "msg=stop", "exec 出错同样释放运行时")
	requireContains(t, res.Stdout+res.Stderr, "监听", "端口占用错误点名监听地址")

	// 出错路径之后锁同样已释放。
	thirdAddr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), thirdAddr))
	p3 := startGrouped(t, h, fixtureEnv(), "serve")
	defer p3.stop(t)
	waitHealthHTTP(t, p3, thirdAddr, 30*time.Second)
	p3.stop(t)

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status FROM runs`)
	if len(rows) != 1 || rows[0]["status"] != "completed" {
		t.Fatalf("在途运行应恰有一行 completed，实际 %v", rows)
	}
}

// closeListener 关闭测试进程占用的监听（失败并入判红）。
// closeListener closes the listener the test held; a failure fails the test.
func closeListener(t *testing.T, listener net.Listener) {
	t.Helper()
	if err := listener.Close(); err != nil {
		t.Fatalf("释放占用的监听失败: %v", err)
	}
}

// testWebToolCatalog：/tools 顺序 = Names() 升序、来源分类正确（§6.1 第 11 项）。
// testWebToolCatalog: /tools is sorted and its sources are classified correctly.
func testWebToolCatalog(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr)+mcpShellAllowed())
	writeWebAgent(t, h, "ops-agent", nil)
	h.writeMCPServers(t, mcpServersYAML("helper"))

	p := startGrouped(t, h, mcpHelperEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	resp := webRequest(t, addr, http.MethodGet, "/api/v1/tools", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "/tools")
	var tools []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Source      string `json:"source"`
	}
	decodeWebData(t, resp, &tools, "/tools")

	wantNames := []string{
		"blob", "boom", "echo", "http_get", "http_post", "list_dir",
		"manage_schedule", "notify", "read_file", "recall_memory",
		"save_memory", "shell", "write_file",
	}
	if len(tools) != len(wantNames) {
		t.Fatalf("/tools 应有 %d 项，实际 %d 项：%v\nserve 输出：\n%s",
			len(wantNames), len(tools), tools, p.combined())
	}
	mcpSource := map[string]bool{"blob": true, "boom": true, "echo": true}
	for i, tool := range tools {
		if tool.Name != wantNames[i] {
			t.Fatalf("/tools[%d].name = %q，期望 %q（顺序必须是 Names() 升序）", i, tool.Name, wantNames[i])
		}
		wantSource := "builtin"
		if mcpSource[tool.Name] {
			wantSource = "mcp"
		}
		if tool.Source != wantSource {
			t.Fatalf("/tools[%d]（%s）.source = %q，期望 %q", i, tool.Name, tool.Source, wantSource)
		}
	}

	resp = webRequest(t, addr, http.MethodGet, "/api/v1/info", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "/info")
	var info struct {
		Version       string `json:"version"`
		UptimeSeconds int64  `json:"uptime_seconds"`
		Agents        int    `json:"agents"`
		Tools         int    `json:"tools"`
		MCPServers    int    `json:"mcp_servers"`
		Providers     []struct {
			Name       string   `json:"name"`
			Models     []string `json:"models"`
			Configured bool     `json:"configured"`
		} `json:"providers"`
	}
	decodeWebData(t, resp, &info, "/info")
	if info.Tools != len(wantNames) {
		t.Fatalf("/info.tools = %d，期望 %d", info.Tools, len(wantNames))
	}
	if info.MCPServers != 1 {
		t.Fatalf("/info.mcp_servers = %d，期望 1", info.MCPServers)
	}
	if info.Agents != 1 {
		t.Fatalf("/info.agents = %d，期望 1（只数已注册项）", info.Agents)
	}
	if len(info.Providers) != 1 || info.Providers[0].Name != fixtureProvider || !info.Providers[0].Configured {
		t.Fatalf("/info.providers = %v，期望一家已配置的 %s", info.Providers, fixtureProvider)
	}
}

// testWebSessionID：REST 两条钉死向量不变 + 恶意渠道前缀回落（§6.1 第 12 项）。
// testWebSessionID: both pinned vectors hold and a hostile channel falls back to ch.
func testWebSessionID(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)
	writeWebAgent(t, h, "notes-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	sess := webCreateSession(t, addr, "ops-agent", nil)
	if sess.SessionID != "web-bb27c35eeb3cc34a" {
		t.Fatalf("REST 向量 = %q，期望 web-bb27c35eeb3cc34a", sess.SessionID)
	}

	// CLI 向量经同一派生函数：REST 面显式传 channel / user 必须得到逐字相同的 id。
	cli := webCreateSession(t, addr, "notes-agent", map[string]string{
		"channel": "cli",
		"user_id": "yunwei",
	})
	if cli.SessionID != "cli-a2025c15b5cbc303" {
		t.Fatalf("CLI 向量 = %q，期望 cli-a2025c15b5cbc303", cli.SessionID)
	}

	hostile := webCreateSession(t, addr, "ops-agent", map[string]string{"channel": "/../x"})
	if !regexp.MustCompile(`^ch-[0-9a-f]{16}$`).MatchString(hostile.SessionID) {
		t.Fatalf("恶意渠道的会话 id = %q，期望 ch-<16hex> 回落", hostile.SessionID)
	}
	if got := webGetSession(t, addr, hostile.SessionID); got.SessionID != hostile.SessionID {
		t.Fatalf("回落 id 不可寻址：GET 返回 %q", got.SessionID)
	}

	legit := webCreateSession(t, addr, "ops-agent", map[string]string{"channel": "ops"})
	if !strings.HasPrefix(legit.SessionID, "ops-") {
		t.Fatalf("合法渠道前缀应原样保留，实际 %q", legit.SessionID)
	}
}

// testWebBudgetLimit：basalt.yaml 的 budget.max_iterations 控制循环上限，改回缺省
// 后同一提示完成（§6.1 第 13 项 / §6.2 第 10 项）。
// testWebBudgetLimit: the configured iteration cap terminates the run with
// LIMIT_REACHED; restoring the default lets the same prompt complete.
func testWebBudgetLimit(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t,
		toolCallResponse("call-budget-1", "read_file", `{"path":"notes.txt"}`),
		toolCallResponse("call-budget-2", "read_file", `{"path":"notes.txt"}`),
		textResponse("预算恢复后完成"),
	)
	h.writeConfig(t, configWithAddr(model.URL(), addr)+"budget:\n  max_iterations: 1\n")
	writeWebAgent(t, h, "demo", []string{"read_file"})
	h.writeAgentFile(t, "demo", "notes.txt", "预算演示笔记\n")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	resp := webInvoke(t, addr, "demo", "读一下笔记")
	requireWebStatus(t, resp, http.StatusOK, "LIMIT_REACHED", webIterationMessage, "迭代上限")
	var run webRun
	decodeWebData(t, resp, &run, "迭代上限")
	if run.Status != "limit_reached" {
		t.Fatalf("迭代上限：status = %q，期望 limit_reached", run.Status)
	}
	if run.Reason != webReasonIteration {
		t.Fatalf("迭代上限：reason = %q，期望 %q", run.Reason, webReasonIteration)
	}
	if run.Iterations != 1 || run.ToolCalls != 1 {
		t.Fatalf("迭代上限：iterations / tool_calls = %d / %d，期望 1 / 1", run.Iterations, run.ToolCalls)
	}

	p.stop(t)

	// 改回缺省预算：同一提示（假模型第二步仍是工具调用）应正常完成。
	secondAddr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), secondAddr))
	p2 := startGrouped(t, h, fixtureEnv(), "serve")
	defer p2.stop(t)
	waitHealthHTTP(t, p2, secondAddr, 30*time.Second)

	resp = webInvoke(t, secondAddr, "demo", "读一下笔记")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "缺省预算")
	run = webRun{}
	decodeWebData(t, resp, &run, "缺省预算")
	if run.Status != "completed" {
		t.Fatalf("缺省预算：status = %q，期望 completed（预算来自基线配置）", run.Status)
	}
	if run.FinalText != "预算恢复后完成" {
		t.Fatalf("缺省预算：final_text = %q", run.FinalText)
	}
	if run.Iterations != 2 || run.ToolCalls != 1 {
		t.Fatalf("缺省预算：iterations / tool_calls = %d / %d，期望 2 / 1", run.Iterations, run.ToolCalls)
	}

	p2.stop(t)

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status, reason, iterations, tool_calls FROM runs ORDER BY rowid`)
	if len(rows) != 2 {
		t.Fatalf("runs 应有 2 行，实际 %d 行：%v", len(rows), rows)
	}
	if rows[0]["status"] != "limit_reached" || rows[0]["reason"] != webReasonIteration {
		t.Fatalf("第一行 = %v，期望 limit_reached / %s", rows[0], webReasonIteration)
	}
	if rows[1]["status"] != "completed" {
		t.Fatalf("第二行 = %v，期望 completed", rows[1])
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM tool_invocations`); n != 2 {
		t.Fatalf("tool_invocations 应有 2 行（两次 read_file），实际 %d 行", n)
	}
}

// testWebCredentialFace：响应面零凭证、零 base_url、零绝对路径（§6.1 第 14 项 /
// §6.2 第 9 项）。
// testWebCredentialFace: no response carries credentials, base_url, or absolute paths.
func testWebCredentialFace(t *testing.T) {
	const sentinel = "e2e-sentinel-not-a-secret"
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	for _, path := range []string{"/api/v1/health", "/api/v1/info", "/api/v1/profiles", "/api/v1/tools", "/api/v1/memory"} {
		resp := webRequest(t, addr, http.MethodGet, path, "", nil)
		requireWebStatus(t, resp, http.StatusOK, "OK", "", path)
		requireAbsent(t, resp.Raw, sentinel, path+" 凭证零回显")
		requireAbsent(t, resp.Raw, "api_key", path+" 不得出现 api_key 字段")
		requireAbsent(t, resp.Raw, "base_url", path+" 不得出现 base_url 字段")
		requireAbsent(t, resp.Raw, h.Root, path+" 不得出现工作区绝对路径")
		requireAbsent(t, resp.Raw, strings.ReplaceAll(h.Root, `\`, `\\`), path+" 不得出现 JSON 转义后的工作区路径")
	}

	p.stop(t)
	requireAbsent(t, p.combined(), sentinel, "日志凭证零回显")
	requireAbsentInWorkspace(t, h.Root, sentinel, "工作区凭证零落盘")
}

// testWebCurlFlow：Demo 四的 curl 四步——建会话 → 发消息 → 取历史 → 归档后再读
// （§6.2 第 1 项）。
// testWebCurlFlow: the four-step curl demo, all through envelopes.
func testWebCurlFlow(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t, textResponse("curl 四步完成"))
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	createResp := webRequest(t, addr, http.MethodPost, "/api/v1/sessions", `{"agent":"ops-agent"}`, nil)
	requireWebStatus(t, createResp, http.StatusOK, "OK", "", "建会话")
	requireEnvelopeShape(t, createResp, "建会话")
	var sess webSession
	decodeWebData(t, createResp, &sess, "建会话")

	msgResp := webPostMessage(t, addr, sess.SessionID, "curl 第一步")
	requireWebStatus(t, msgResp, http.StatusOK, "OK", "", "发消息")
	requireEnvelopeShape(t, msgResp, "发消息")

	history := webGetSession(t, addr, sess.SessionID)
	if len(history.Messages) != 2 {
		t.Fatalf("历史应有 2 条消息，实际 %d 条：%v", len(history.Messages), history.Messages)
	}
	if history.Messages[0].Role != "user" || history.Messages[0].Content != "curl 第一步" {
		t.Fatalf("历史第一条 = %+v，期望 user / curl 第一步", history.Messages[0])
	}
	if history.Messages[1].Role != "assistant" || history.Messages[1].Content != "curl 四步完成" {
		t.Fatalf("历史第二条 = %+v，期望 assistant / curl 四步完成", history.Messages[1])
	}

	archiveResp := webArchive(t, addr, sess.SessionID)
	requireWebStatus(t, archiveResp, http.StatusOK, "OK", "", "归档")
	requireEnvelopeShape(t, archiveResp, "归档")
	if archiveResp.HasData {
		t.Fatalf("归档成功响应不应带 data：%s", archiveResp.Raw)
	}

	archived := webGetSession(t, addr, sess.SessionID)
	if archived.Status != "archived" {
		t.Fatalf("归档后 status = %q，期望 archived", archived.Status)
	}
	if len(archived.Messages) != 2 {
		t.Fatalf("归档后历史仍应可读（2 条），实际 %d 条", len(archived.Messages))
	}
}

// testWebConcurrentConflict：两个同时发出的同会话请求恰有一个 409（§6.2 第 2 项）。
// testWebConcurrentConflict: two simultaneous requests on one session yield exactly
// one 200 and one 409.
func testWebConcurrentConflict(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newBarrierModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	defer model.open()
	waitHealthHTTP(t, p, addr, 30*time.Second)

	sess := webCreateSession(t, addr, "ops-agent", nil)
	results := make(chan webResult, 2)
	for _, prompt := range []string{"并发甲", "并发乙"} {
		prompt := prompt
		go func() {
			raw, err := json.Marshal(map[string]string{"message": prompt})
			if err != nil {
				results <- webResult{err: err}
				return
			}
			resp, err := doWebRequest(context.Background(), http.MethodPost,
				addr, "/api/v1/sessions/"+sess.SessionID+"/messages", string(raw), nil)
			results <- webResult{resp: resp, err: err}
		}()
	}
	model.waitCalls(t, 1, 30*time.Second)

	// 被准入的那条阻塞在模型上，因此先收回来的一定是 409。
	busy := waitWebResult(t, results, 30*time.Second, "并发第二请求")
	requireWebStatus(t, busy, http.StatusConflict, "SESSION_BUSY", webBusyMessage, "并发第二请求")

	model.open()
	admitted := waitWebResult(t, results, 60*time.Second, "并发第一请求")
	requireWebStatus(t, admitted, http.StatusOK, "OK", "", "并发第一请求")
	var run webRun
	decodeWebData(t, admitted, &run, "并发第一请求")
	if run.Status != "completed" {
		t.Fatalf("并发第一请求：status = %q，期望 completed", run.Status)
	}

	p.stop(t)
	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 1 {
		t.Fatalf("两次并发应只有一次运行入库，实际 %d 行", n)
	}
}

// testWebStatelessInvoke：invoke 零建会话、零历史，运行与审计照写（§6.2 第 3 项）。
// testWebStatelessInvoke: invoke creates no session and no history, while the run and
// its audit rows are still written.
func testWebStatelessInvoke(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t, textResponse("无状态完成"))
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	writeWebAgent(t, h, "ops-agent", nil)

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	sess := webCreateSession(t, addr, "ops-agent", nil)

	resp := webInvoke(t, addr, "ops-agent", "无状态提示")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "无状态调用")
	var run webRun
	decodeWebData(t, resp, &run, "无状态调用")
	if run.SessionID != "" {
		t.Fatalf("invoke 的 session_id = %q，期望空串", run.SessionID)
	}
	if run.Channel != "web" || run.Trigger != "api" {
		t.Fatalf("invoke 的 channel / trigger = %q / %q，期望 web / api", run.Channel, run.Trigger)
	}
	if run.Status != "completed" || run.FinalText != "无状态完成" {
		t.Fatalf("invoke 结果 = %+v，期望 completed / 无状态完成", run)
	}

	history := webGetSession(t, addr, sess.SessionID)
	if history.MessageCount != 0 || len(history.Messages) != 0 {
		t.Fatalf("既有会话消息数应不变（0），实际 message_count=%d messages=%d",
			history.MessageCount, len(history.Messages))
	}

	p.stop(t)

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM sessions`); n != 1 {
		t.Fatalf("invoke 不得建会话：sessions 应有 1 行，实际 %d 行", n)
	}
	rows := queryRows(t, db, `SELECT session_id, agent, channel, user_id, trigger, status FROM runs`)
	if len(rows) != 1 {
		t.Fatalf("invoke 应写 1 行 runs，实际 %d 行", len(rows))
	}
	row := rows[0]
	if row["session_id"] != nil {
		t.Fatalf("invoke 的 runs.session_id 应为 NULL，实际 %v", row["session_id"])
	}
	if row["agent"] != "ops-agent" || row["channel"] != "web" || row["user_id"] != "anonymous" {
		t.Fatalf("invoke 运行行 = %v，期望 ops-agent / web / anonymous", row)
	}
	if row["trigger"] != "api" || row["status"] != "completed" {
		t.Fatalf("invoke 运行行 = %v，期望 api / completed", row)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("invoke 不得写历史：messages 应有 0 行，实际 %d 行", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM llm_calls`); n != 1 {
		t.Fatalf("invoke 应写 1 行模型审计，实际 %d 行", n)
	}
}

// testWebListenAddrOverride：改 http.addr 后只在该地址可达（§6.2 第 4 项的本机半）。
// 第二主机远程可达需要另一台主机，属未验证（缺环境），不在本用例内冒充。
// testWebListenAddrOverride: the configured address is reachable and the default one
// is not held by this process. Cross-host reachability stays out of scope.
func testWebListenAddrOverride(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	resp := webRequest(t, addr, http.MethodGet, "/api/v1/health", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "显式监听地址")

	// 缺省地址不得被本进程占用：连不上即通过；连上则断言不是 basalt 健康信封。
	if raw, err := webHealthStatus("127.0.0.1:8080"); err == nil {
		requireAbsent(t, raw, `"code":"OK"`, "缺省地址不得由本进程监听")
	}
}

// testWebGatewayLifecycle：gateway 无 HTTP、占位 INFO 可见、实例锁互斥、优雅退出
// （§6.2 第 7 项；Windows Ctrl+C 事件序属平台限制，记未验证）。
// testWebGatewayLifecycle: gateway serves no HTTP, logs the placeholder, holds the
// instance lock, and exits gracefully on an interrupt.
func testWebGatewayLifecycle(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	model := newFakeModel(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))

	gateway := startGrouped(t, h, fixtureEnv(), "gateway")
	defer gateway.stop(t)
	gateway.waitOutput(t, "mode=gateway", 30*time.Second)
	requireContains(t, gateway.combined(), "调度器已启动", "gateway 占位 INFO")

	if raw, err := webHealthStatus(addr); err == nil {
		t.Fatalf("gateway 不得监听 HTTP，但 %s 返回了 %s", addr, raw)
	}

	// 实例锁互斥：同工作区的第二个重命令拒绝启动。
	res := h.run(t, fixtureEnv(), "serve")
	requireExit(t, res, 1, "gateway 持锁期间的 serve")
	requireContains(t, res.Stdout+res.Stderr, "工作区实例锁已被占用", "实例锁互斥")

	gateway.stop(t)
	requireContains(t, gateway.combined(), "msg=stop", "gateway 退出时释放运行时")

	// 锁已释放：serve 能正常启动并监听。
	serve := startGrouped(t, h, fixtureEnv(), "serve")
	defer serve.stop(t)
	waitHealthHTTP(t, serve, addr, 30*time.Second)
	serve.stop(t)
}

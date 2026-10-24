//go:build e2e && linux

package e2e

// 本文件回填 US-9 §6.1 第 6 项（宽限耗尽停机）与 US-10 15a（停机事件序）的 Linux 平台
// 分支。Windows 无法向子进程投递 SIGTERM（Process.Signal 只支持 Kill），这两项在 Windows
// 上一律记「未验证（平台限制）」；Linux / WSL / CI 上才是真实信号判卷。
// This file backfills the Linux branches of the US-9 grace-exhaustion item and the US-10
// shutdown-order item. Windows cannot deliver SIGTERM to a child process, so those items
// stay unverified there; the real-signal verdict runs on Linux / WSL / CI.
//
// 两个子测试的判据：
//   - grace_exhausted：在途调度运行把 70s 停机宽限耗满——监听先关 → 宽限内运行存活
//     → 到点取消 → 终态落库 → 进程按 0 退出且不挂死。取消耗时被夹在
//     [signalGraceLower, signalGraceUpper] 之间，提前取消或永不取消都变红。
//   - sigterm_order：serve 与 gateway 两种模式各收一次真实 SIGTERM——在途运行在宽限
//     内继续走完 MCP 工具调用与收尾文本，证明「调度器先停、MCP 池后关」的释放次序；
//     退出后无 started 残行，实例锁已释放。
// The two subtests pin the 70s grace boundary and the release order (scheduler before the
// MCP pool), each with a real SIGTERM against the built binary.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/testutil"
)

const (
	// signalScheduleLead 是 cron 触发点到进程就绪的余量：与既有调度用例同一口径。
	// signalScheduleLead is the cron lead time used by the other scheduler cases.
	signalScheduleLead = 15 * time.Second

	// signalGraceLower / signalGraceUpper 把 70s 宽限的取消点夹在中间：下界留 2s
	// 时钟与调度抖动，上界再留 20s 给取消后的收尾与进程退出。
	// The bounds bracket the 70s cancellation point with slack on both sides.
	signalGraceLower = 68 * time.Second
	signalGraceUpper = 90 * time.Second

	// signalEchoText 是 SIGTERM 之后经 MCP 回显的合成文本（无敏感内容）。
	// signalEchoText is the synthetic text echoed over MCP after SIGTERM.
	signalEchoText = "信号后 MCP 回显"

	// signalMessage 是两条停机用例的调度消息（同时是用户消息判据）。
	// signalMessage is the schedule message, also asserted as the user message.
	signalMessage = "停机顺序演示"

	// signalGraceMessage 是宽限耗尽用例的调度消息。
	// signalGraceMessage is the schedule message of the grace-exhaustion case.
	signalGraceMessage = "宽限耗尽演示"
)

// TestBackfill_SignalLinux：真实 SIGTERM 的两个 Linux 平台分支（US-9 §6.1 第 6 项、
// US-10 15a）。子测试名逐字钉进回填目录，不得改名。
// TestBackfill_SignalLinux pins the two real-SIGTERM branches; the subtest names are
// catalogued verbatim and must not change.
func TestBackfill_SignalLinux(t *testing.T) {
	t.Run("grace_exhausted", testSignalGraceExhausted)
	t.Run("sigterm_order", testSignalOrder)
}

// testSignalGraceExhausted：调度器在途运行把 70s 宽限耗满的五阶段停机。
// 判据（事件序）：① 监听先关（新连接被拒）；② 宽限内进程与在途模型调用都还活着；
// ③ 宽限到点才取消在途运行；④ 取消后进程按 0 退出、不挂死；⑤ 执行行以 failed 终态
// 落库、无 started 残行。Web 层的宽限分支由组件缝 TestServer_ShutdownGraceExhausted
// 覆盖——进程级 70s 只在调度器 StopGrace 上可达（HTTP 处理器受 60s RunTimeout 约束）。
// testSignalGraceExhausted drives the scheduler's 70s stop grace to exhaustion and
// asserts the five-phase order and the elapsed-time budget.
func testSignalGraceExhausted(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	cron := cronAt(time.Now().UTC().Add(signalScheduleLead))
	model := newParkScriptModel(t) // 无剧本：首个模型请求被取消即收场
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil)+scheduleBlock(cron, "", signalGraceMessage),
		"宽限耗尽演示")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	model.waitEntered(t, 60*time.Second) // cron 到点，运行已停在模型调用上

	signalAt := time.Now()
	p.terminate(t)

	// ① 停止接入：监听在宽限期内先行关闭，新连接被拒。
	waitListenerClosed(t, addr, 10*time.Second)

	// ② 宽限内运行存活：宽限才刚开始，在途模型连接必须仍在（运行未被提前取消），
	// 进程也必须还活着（正在宽限里等它）。
	select {
	case <-model.aborted:
		t.Fatalf("SIGTERM 后 %s 内在途运行就被取消：70s 宽限未生效", time.Since(signalAt))
	case <-time.After(3 * time.Second):
	}
	select {
	case <-p.done:
		t.Fatalf("宽限期内进程提前退出（%d）：在途运行必须在宽限内继续存活\n%s",
			p.exitCodeNow(), p.output())
	default:
	}

	// ③ 取消在途：宽限到点后客户端断开模型连接，取消点落进预算。
	select {
	case <-model.aborted:
	case <-time.After(signalGraceUpper + 15*time.Second):
		t.Fatalf("宽限到期后仍在途：模型连接在 %s 内未被取消\n%s", signalGraceUpper, p.output())
	}
	elapsed := time.Since(signalAt)
	if elapsed < signalGraceLower {
		t.Fatalf("在途运行在 SIGTERM 后 %s 就被取消：早于 70s 宽限（下界 %s）",
			elapsed, signalGraceLower)
	}
	if elapsed > signalGraceUpper {
		t.Fatalf("宽限到期后的取消耗时 %s 超过上界 %s", elapsed, signalGraceUpper)
	}

	// ④ 收尾：取消后进程按 0 退出（不挂死）；msg=stop 是 cron 引擎停机标记。
	if code := p.waitExit(t, 30*time.Second); code != 0 {
		t.Fatalf("宽限耗尽后的退出码 = %d，期望 0\n%s", code, p.output())
	}
	requireContains(t, p.combined(), "msg=stop", "调度器停机标记")

	// ⑤ 终态落库：执行行 failed、无 started 残行、任务 last_status 同步。
	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions WHERE status = 'started'`); n != 0 {
		t.Fatalf("停机后仍有 %d 行 started 执行行：收尾必须用独立 ctx 写终态", n)
	}
	execs := queryRows(t, db, `SELECT status, trigger, error_message, finished_at FROM task_executions`)
	if len(execs) != 1 {
		t.Fatalf("task_executions 应有 1 行，实际 %d 行：%v", len(execs), execs)
	}
	if got := stringValue(t, execs[0], "status"); got != "failed" {
		t.Fatalf("task_executions.status = %q，期望 failed（宽限取消不是完成）", got)
	}
	if got := stringValue(t, execs[0], "trigger"); got != "cron" {
		t.Fatalf("task_executions.trigger = %q，期望 cron", got)
	}
	if execs[0]["error_message"] == nil {
		t.Fatal("task_executions.error_message 为空：取消原因必须落库")
	}
	if got := stringValue(t, execs[0], "error_message"); got == "" {
		t.Fatal("task_executions.error_message 是空串：取消原因必须落库")
	}
	if execs[0]["finished_at"] == nil {
		t.Fatal("task_executions.finished_at 为空：终态行必须写收尾时间")
	}
	tasks := queryRows(t, db, `SELECT last_status FROM scheduled_tasks WHERE task_id = 'demo-1'`)
	if len(tasks) != 1 {
		t.Fatalf("scheduled_tasks 应有 demo-1 一行，实际 %d 行：%v", len(tasks), tasks)
	}
	if got := stringValue(t, tasks[0], "last_status"); got != "failed" {
		t.Fatalf("scheduled_tasks.last_status = %q，期望 failed", got)
	}

	// runs 账本行由 AgentService 用被取消的 ctx 收尾，写失败属既有纪律（留 running
	// 待下次启动的恢复扫描）；这里接受 cancelled 与 running 两态，不允许别的状态。
	runs := queryRows(t, db, `SELECT status FROM runs`)
	if len(runs) != 1 {
		t.Fatalf("runs 应有 1 行，实际 %d 行：%v", len(runs), runs)
	}
	switch got := stringValue(t, runs[0], "status"); got {
	case "cancelled", "running":
	default:
		t.Fatalf("runs.status = %q，期望 cancelled（已落库）或 running（待恢复扫描收尾）", got)
	}
}

// testSignalOrder：serve 与 gateway 两种模式各收一次真实 SIGTERM，判「事件序」。
// serve 段：在途调度运行停在模型调用上 → SIGTERM（监听关闭、进程不退出）→ 放行模型，
// 运行在宽限内继续走 MCP 工具调用与收尾文本 → 退出 0。MCP 池在 heavyRuntime.Close 里
// **晚于** scheduler.Stop，运行还能调到 MCP 就证明释放次序没有被反过来。
// gateway 段：同一停机路径、无 HTTP 监听、同样按 0 退出。
// testSignalOrder asserts the release order after SIGTERM in both serve and gateway modes.
func testSignalOrder(t *testing.T) {
	// 甲段：serve。
	h := newHarness(t)
	initWorkspace(t, h)
	cron := cronAt(time.Now().UTC().Add(signalScheduleLead))
	model := newParkScriptModel(t,
		toolCallResponse("call-order-echo", mcpEchoTool, mustJSON(map[string]string{"text": signalEchoText})),
		textResponse("信号后完成"),
	)
	addr := freeLoopbackAddr(t)
	h.writeMCPServers(t, mcpServersYAML(mcpHelperServer))
	h.writeConfig(t, configWithAddr(model.URL(), addr)+mcpShellAllowed())
	h.writeAgent(t, "demo",
		mcpAgentFrontmatter("demo", []string{mcpEchoTool}, []string{mcpHelperServer})+
			scheduleBlock(cron, "", signalMessage),
		"停机顺序演示")

	// 只给 MCP helper 占位变量：shell 与 MCP 共用 shell.env_passthrough，本段不配 shell。
	p := startGrouped(t, h, mcpHelperEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	model.waitEntered(t, 60*time.Second)

	signalAt := time.Now()
	p.terminate(t)
	waitListenerClosed(t, addr, 10*time.Second)

	// 宽限内进程必须还活着、在途运行未被取消；放行模型后它要走到 MCP 回显与收尾。
	select {
	case <-p.done:
		t.Fatalf("SIGTERM 后进程立即退出（%d）：在途运行未被宽限等待\n%s",
			p.exitCodeNow(), p.output())
	case <-time.After(2 * time.Second):
	}
	model.open()

	if code := p.waitExit(t, 40*time.Second); code != 0 {
		t.Fatalf("SIGTERM 停机退出码 = %d，期望 0\n%s", code, p.output())
	}
	if elapsed := time.Since(signalAt); elapsed >= signalGraceLower {
		t.Fatalf("在途运行耗时 %s 才收场：预期在宽限内自然完成（< %s）", elapsed, signalGraceLower)
	}
	requireContains(t, p.combined(), "msg=stop", "调度器停机标记")

	reqs := model.requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求（首轮 + 工具结果后收尾），实际 %d 次", len(reqs))
	}
	// SIGTERM 之后发出的 MCP 工具调用必须成功：工具结果进了第二次模型请求。
	requireToolResultContains(t, reqs[1], signalEchoText, "SIGTERM 之后的 MCP 工具结果")

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions WHERE status = 'started'`); n != 0 {
		t.Fatalf("停机后仍有 %d 行 started 执行行", n)
	}
	execs := queryRows(t, db, `SELECT status, trigger, run_id, finished_at FROM task_executions`)
	if len(execs) != 1 {
		t.Fatalf("task_executions 应有 1 行，实际 %d 行：%v", len(execs), execs)
	}
	if got := stringValue(t, execs[0], "status"); got != "completed" {
		t.Fatalf("task_executions.status = %q，期望 completed（宽限内正常收场）", got)
	}
	if got := stringValue(t, execs[0], "trigger"); got != "cron" {
		t.Fatalf("task_executions.trigger = %q，期望 cron", got)
	}
	if execs[0]["finished_at"] == nil {
		t.Fatal("task_executions.finished_at 为空：终态行必须写收尾时间")
	}
	execRun := stringValue(t, execs[0], "run_id")

	runs := queryRows(t, db, `SELECT run_id, channel, trigger, status FROM runs`)
	if len(runs) != 1 {
		t.Fatalf("runs 应有 1 行，实际 %d 行：%v", len(runs), runs)
	}
	if got := stringValue(t, runs[0], "run_id"); got != execRun {
		t.Fatalf("runs.run_id = %q，期望与执行行一致 %q", got, execRun)
	}
	if got := stringValue(t, runs[0], "status"); got != "completed" {
		t.Fatalf("runs.status = %q，期望 completed", got)
	}
	if got := stringValue(t, runs[0], "trigger"); got != "cron" {
		t.Fatalf("runs.trigger = %q，期望 cron", got)
	}
	if got := stringValue(t, runs[0], "channel"); got != "scheduler" {
		t.Fatalf("runs.channel = %q，期望 scheduler", got)
	}
	tasks := queryRows(t, db, `SELECT last_status FROM scheduled_tasks WHERE task_id = 'demo-1'`)
	if len(tasks) != 1 {
		t.Fatalf("scheduled_tasks 应有 demo-1 一行，实际 %d 行：%v", len(tasks), tasks)
	}
	if got := stringValue(t, tasks[0], "last_status"); got != "completed" {
		t.Fatalf("scheduled_tasks.last_status = %q，期望 completed", got)
	}

	users := queryRows(t, db, `SELECT content FROM messages WHERE role = 'user'`)
	if len(users) != 1 {
		t.Fatalf("用户消息应有 1 条，实际 %d 条：%v", len(users), users)
	}
	if got := stringValue(t, users[0], "content"); got != signalMessage {
		t.Fatalf("用户消息 = %q，期望调度消息 %q", got, signalMessage)
	}
	assistants := queryRows(t, db,
		`SELECT content FROM messages WHERE role = 'assistant' AND content IS NOT NULL AND content <> ''`)
	if len(assistants) != 1 {
		t.Fatalf("助手最终回答应有 1 条，实际 %d 条：%v", len(assistants), assistants)
	}
	if got := stringValue(t, assistants[0], "content"); got != "信号后完成" {
		t.Fatalf("助手最终回答 = %q，期望「信号后完成」", got)
	}
	tools := queryRows(t, db, `SELECT content FROM messages WHERE role = 'tool'`)
	if len(tools) != 1 {
		t.Fatalf("工具消息应有 1 条，实际 %d 条：%v", len(tools), tools)
	}
	if got := stringValue(t, tools[0], "content"); !strings.Contains(got, signalEchoText) {
		t.Fatalf("工具消息 = %q，期望包含 MCP 回显文本 %q", got, signalEchoText)
	}

	// 实例锁在退出后释放：同工作区的重命令能重新取锁并优雅退出。该工作区配了 MCP
	// server，重命令仍要带 helper 占位变量（装配期必须能解析 env）。
	after := startGrouped(t, h, mcpHelperEnv(), "gateway")
	after.waitOutput(t, "调度器已启动", 30*time.Second)
	after.stop(t)

	// 乙段：gateway——无 HTTP，SIGTERM 同样等宽限里的在途运行收场。
	hGateway := newHarness(t)
	initWorkspace(t, hGateway)
	cronGateway := cronAt(time.Now().UTC().Add(signalScheduleLead))
	modelGateway := newBarrierModel(t)
	addrGateway := freeLoopbackAddr(t)
	hGateway.writeConfig(t, configWithAddr(modelGateway.URL(), addrGateway))
	hGateway.writeAgent(t, "demo", agentFrontmatter("demo", nil)+scheduleBlock(cronGateway, "", signalMessage),
		"gateway 停机演示")

	g := startGrouped(t, hGateway, fixtureEnv(), "gateway")
	g.waitOutput(t, "调度器已启动", 30*time.Second)
	requireContains(t, g.combined(), "mode=gateway", "gateway 占位 INFO")
	if raw, err := webHealthStatus(addrGateway); err == nil {
		t.Fatalf("gateway 不得监听 HTTP，但 %s 返回了 %s", addrGateway, raw)
	}
	modelGateway.waitCalls(t, 1, 60*time.Second)

	g.terminate(t)
	select {
	case <-g.done:
		t.Fatalf("gateway 在 SIGTERM 后立即退出（%d）：在途运行未被宽限等待\n%s",
			g.exitCodeNow(), g.output())
	case <-time.After(2 * time.Second):
	}
	modelGateway.open()
	if code := g.waitExit(t, 40*time.Second); code != 0 {
		t.Fatalf("gateway 停机退出码 = %d，期望 0\n%s", code, g.output())
	}
	requireContains(t, g.combined(), "msg=stop", "gateway 停机标记")

	dbGateway := openDB(t, hGateway.dbPath())
	if n := scalarInt(t, dbGateway, `SELECT COUNT(*) FROM task_executions WHERE status = 'started'`); n != 0 {
		t.Fatalf("gateway 停机后仍有 %d 行 started 执行行", n)
	}
	execsGateway := queryRows(t, dbGateway, `SELECT status, trigger FROM task_executions`)
	if len(execsGateway) != 1 {
		t.Fatalf("gateway 的 task_executions 应有 1 行，实际 %d 行：%v", len(execsGateway), execsGateway)
	}
	if got := stringValue(t, execsGateway[0], "status"); got != "completed" {
		t.Fatalf("gateway 执行行 status = %q，期望 completed", got)
	}
	if got := stringValue(t, execsGateway[0], "trigger"); got != "cron" {
		t.Fatalf("gateway 执行行 trigger = %q，期望 cron", got)
	}
	runsGateway := queryRows(t, dbGateway, `SELECT status FROM runs`)
	if len(runsGateway) != 1 {
		t.Fatalf("gateway 的 runs 应有 1 行，实际 %d 行：%v", len(runsGateway), runsGateway)
	}
	if got := stringValue(t, runsGateway[0], "status"); got != "completed" {
		t.Fatalf("gateway runs.status = %q，期望 completed", got)
	}
}

// terminate 向进程投递真实 SIGTERM（仅 Linux：Windows 的子进程信号只支持 Kill）。
// terminate delivers a real SIGTERM; Windows only supports Kill on child processes.
func (p *groupedProcess) terminate(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatalf("进程已退出（%d），无法投递 SIGTERM", p.exitCodeNow())
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("发送 SIGTERM 失败: %v", err)
	}
}

// parkScriptModel 是信号用例专用的回环模型：第一次请求阻塞到 open 放行或客户端断开
// （aborted 关闭），放行后按剧本答复；后续请求直接按剧本答复。它比 barrierModel 多
// 一层剧本，因为运行必须在 SIGTERM 之后继续走到 MCP 工具调用与收尾文本。
// parkScriptModel blocks the first request until open or client disconnect, then replays
// the scripted steps; later requests replay the remaining steps directly.
type parkScriptModel struct {
	t       *testing.T
	server  *httptest.Server
	release chan struct{}
	opened  sync.Once

	entered chan struct{}
	aborted chan struct{}

	mu       sync.Mutex
	calls    int
	steps    []testutil.FakeOpenAIResponse
	recorded []map[string]any
}

// newParkScriptModel 起回环模型；steps[0] 是放行后第一次请求的答复。
// newParkScriptModel starts the loopback model; steps[0] answers the released request.
func newParkScriptModel(t *testing.T, steps ...testutil.FakeOpenAIResponse) *parkScriptModel {
	t.Helper()
	m := &parkScriptModel{
		t:       t,
		release: make(chan struct{}),
		entered: make(chan struct{}),
		aborted: make(chan struct{}),
		steps:   steps,
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(func() {
		m.open()
		m.server.Close()
	})
	return m
}

// URL 返回模型地址（供 provider.base_url 使用）。
// URL returns the model address.
func (m *parkScriptModel) URL() string { return m.server.URL }

// waitEntered 等第一次模型请求进入阻塞区——「运行已在途」的确定性观测点。
// waitEntered waits until the first model request is parked.
func (m *parkScriptModel) waitEntered(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-m.entered:
	case <-time.After(timeout):
		t.Fatalf("等待运行进入模型调用超时（%s）", timeout)
	}
}

// open 放行第一次请求（幂等）。
// open releases the parked request (idempotent).
func (m *parkScriptModel) open() {
	m.opened.Do(func() { close(m.release) })
}

// requests 返回已记录请求的副本（解析后的 JSON）。
// requests returns copies of the recorded requests as parsed JSON.
func (m *parkScriptModel) requests() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.recorded...)
}

// handle 记录请求、执行阻塞与剧本答复；剧本用尽即判本用例红。
// handle records the request, blocks the first call, and replays the script.
func (m *parkScriptModel) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
		m.t.Errorf("停车模型：方法与路径必须是 POST /chat/completions，实际 %s %s", r.Method, r.URL.Path)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		m.t.Errorf("停车模型：读请求体失败: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		m.t.Errorf("停车模型：解析请求体失败: %v", err)
	}
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.recorded = append(m.recorded, parsed)
	var step testutil.FakeOpenAIResponse
	hasStep := call <= len(m.steps)
	if hasStep {
		step = m.steps[call-1]
	}
	m.mu.Unlock()

	if call == 1 {
		close(m.entered)
		select {
		case <-m.release:
		case <-r.Context().Done():
			close(m.aborted)
			return
		}
	}
	if !hasStep {
		m.t.Errorf("停车模型：第 %d 次请求没有剧本可以答复", call)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	status := step.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, step.Body); err != nil {
		m.t.Errorf("停车模型：写响应失败: %v", err)
	}
}

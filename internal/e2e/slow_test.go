//go:build e2e && slow

package e2e

// 本文件回填 US-10 §6.2 第 12 项（模块卡 §2.6.10 的 12b）：真实的 300s 定时运行时限。
// 定时运行在假模型上长挂起，到 300s 时限被取消：执行行落 failed（error_message 为
// 契约 §6 的「超过运行时限」）、在途模型请求被切断、进程存活并能优雅退出。
// 300s 是代码常量（US-10 约束 35，不进配置），因此本用例真的等满时限；只在 `slow`
// 标签与 us15-slow stage 下执行，不进每轮 CI（模块卡 D-09）。
// This file backfills US-10 §6.2 item 12: the real 300s scheduled-run timeout. The
// timeout is a code constant, so the test genuinely waits it out; it runs only under the
// `slow` tag and the us15-slow stage, never in per-round CI.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const (
	// slowWaitUpper 是执行行变为 failed 的等待上限：300s 时限 + 启动与收尾余量。
	// slowWaitUpper bounds the wait for the failed row: the 300s limit plus slack.
	slowWaitUpper = 330 * time.Second

	// slowElapsedFloor 是允许的最短耗时；低于它说明运行被提前切断，而不是等满时限。
	// slowElapsedFloor is the minimum plausible duration; anything shorter means the
	// run was cut early instead of running into the 300s limit.
	slowElapsedFloor = 285 * time.Second

	// slowMessage 是定时消息；模型始终挂起，运行永远不会自然完成。
	// slowMessage is the scheduled message; the model never answers.
	slowMessage = "慢速超时演示"
)

// TestBackfill_SlowTimeout 是慢速回填的唯一顶层测试；子测试名与回填目录逐字一致。
// TestBackfill_SlowTimeout is the single slow backfill test; subtest names match the
// backfill catalog verbatim.
func TestBackfill_SlowTimeout(t *testing.T) {
	t.Run("run_timeout", testSlowRunTimeout)
}

// testSlowRunTimeout 的判据：模型请求进入后须接近满 300s 才落 failed（提前切断即红）；
// 模型连接在运行结束后被切断（时限确实取消了在途调用）；进程存活且健康检查可达；
// 优雅停机退出码 0；执行行 failed + 契约原因字面量 + 收尾时间齐全；任务 last_status
// 同步为 failed。
// testSlowRunTimeout asserts the run lasts close to the full limit, the in-flight model
// call is cut, the process stays alive and shuts down cleanly, and the terminal rows
// carry the contract reason.
func testSlowRunTimeout(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	model := newSlowParkModel(t)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(model.URL(), addr))
	h.writeAgent(t, "demo",
		agentFrontmatter("demo", nil)+scheduleBlock(cronAt(time.Now().UTC().Add(scheduleLead)), "", slowMessage),
		"慢速超时演示")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	// 失败路径（例如逆改后时限不生效）用 kill 兜底：挂死的运行不能拖住清理。
	// kill guards the failure path; a hung run must not stall cleanup.
	defer p.kill()
	waitHealthHTTP(t, p, addr, 30*time.Second)
	p.waitOutput(t, "调度触发", 60*time.Second)
	model.waitEntered(t, 60*time.Second)
	enteredAt := time.Now()

	waitScheduleStatus(t, addr, "demo-1", "failed", slowWaitUpper)
	if elapsed := time.Since(enteredAt); elapsed < slowElapsedFloor {
		t.Fatalf("执行行在模型请求后 %s 就落 failed，早于 %s：运行被提前切断，不是 300s 时限",
			elapsed, slowElapsedFloor)
	}

	// 客户端断开发生在时限到点：没被切断说明取消没有传到 HTTP 层。
	// The client disconnect marks the deadline; no disconnect means the cancel never
	// reached the HTTP layer.
	select {
	case <-model.aborted:
	case <-time.After(30 * time.Second):
		t.Fatal("运行结束后 30s 内模型请求未被切断：时限没有取消在途模型调用")
	}
	select {
	case <-p.done:
		t.Fatalf("运行超时后进程退出（%d），应存活\n%s", p.exitCodeNow(), p.output())
	default:
	}
	if raw, err := webHealthStatus(addr); err != nil {
		t.Fatalf("运行超时后健康检查失败: %v（原始响应 %q）", err, raw)
	}
	p.stop(t)

	// 终态落库：执行行 failed + 契约原因字面量 + 收尾时间齐全；任务 last_status 同步。
	// Terminal rows: failed execution with the contract reason and a finish time.
	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions WHERE status = 'started'`); n != 0 {
		t.Fatalf("超时后仍有 %d 行 started：终态写必须用独立清理 ctx", n)
	}
	execs := queryRows(t, db, `SELECT status, trigger, error_message, finished_at FROM task_executions`)
	if len(execs) != 1 {
		t.Fatalf("task_executions 应有 1 行，实际 %d 行：%v", len(execs), execs)
	}
	if got := stringValue(t, execs[0], "status"); got != "failed" {
		t.Fatalf("task_executions.status = %q，期望 failed（超时不是完成）", got)
	}
	if got := stringValue(t, execs[0], "trigger"); got != "cron" {
		t.Fatalf("task_executions.trigger = %q，期望 cron", got)
	}
	if got := stringValue(t, execs[0], "error_message"); got != "超过运行时限" {
		t.Fatalf("task_executions.error_message = %q，期望契约 §6 的「超过运行时限」", got)
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

	// runs 行可能停在 running：截止已经越过，FinishRun 沿用已取消的运行 ctx，终态
	// 交给下次启动的恢复扫描（与 SIGTERM 用例同一容忍口径）。
	// The runs row may stay running: the deadline has passed before FinishRun, so the
	// recovery scan owns that terminal write (same tolerance as the SIGTERM case).
	runs := queryRows(t, db, `SELECT channel, trigger, status FROM runs`)
	if len(runs) != 1 {
		t.Fatalf("runs 应有 1 行，实际 %d 行：%v", len(runs), runs)
	}
	if got := stringValue(t, runs[0], "channel"); got != "scheduler" {
		t.Fatalf("runs.channel = %q，期望 scheduler", got)
	}
	if got := stringValue(t, runs[0], "trigger"); got != "cron" {
		t.Fatalf("runs.trigger = %q，期望 cron", got)
	}
	if got := stringValue(t, runs[0], "status"); got != "cancelled" && got != "running" {
		t.Fatalf("runs.status = %q，期望 cancelled（已落库）或 running（待恢复）", got)
	}
}

// slowParkModel 是慢速用例专用的回环模型：收到请求后一直挂起，直到客户端断开
// （时限到点取消）才返回——断开本身就是取消到达 HTTP 层的证据。
// slowParkModel parks every request until the client disconnects, so the disconnect
// itself is the evidence that the deadline cancel reached the HTTP layer.
type slowParkModel struct {
	server    *httptest.Server
	t         *testing.T
	entered   chan struct{}
	aborted   chan struct{}
	enterOnce sync.Once
	abortOnce sync.Once
}

// newSlowParkModel 起一个回环服务，随测试结束关闭。
// newSlowParkModel starts the loopback server and closes it with the test.
func newSlowParkModel(t *testing.T) *slowParkModel {
	t.Helper()
	m := &slowParkModel{
		t:       t,
		entered: make(chan struct{}),
		aborted: make(chan struct{}),
	}
	m.server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	return m
}

// URL 返回模型基址（装配进 provider.base_url）。
// URL returns the model base URL for provider.base_url.
func (m *slowParkModel) URL() string {
	return m.server.URL
}

// waitEntered 等第一次模型请求到达；超时判红。
// waitEntered waits for the first model request or fails on timeout.
func (m *slowParkModel) waitEntered(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-m.entered:
	case <-time.After(timeout):
		t.Fatalf("慢速模型未在 %s 内收到第一次请求", timeout)
	}
}

// handle 读空请求体后挂起；客户端断开即记录并返回（不写响应——连接已经没了）。
// handle drains the body and parks until the client disconnects.
func (m *slowParkModel) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
		http.Error(w, "unexpected route", http.StatusNotFound)
		return
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		m.t.Errorf("慢速模型读取请求失败: %v", err)
		return
	}
	m.enterOnce.Do(func() { close(m.entered) })
	<-r.Context().Done()
	m.abortOnce.Do(func() { close(m.aborted) })
}

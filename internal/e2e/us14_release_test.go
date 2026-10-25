//go:build e2e

package e2e

// 本文件补 US-14 发布 e2e 的三条运行可靠性缺口（模块卡 §4.5.11）与一条映射自检：
// 取消、工具自身超时、运行中热加载、七场景映射完整性（开发操作 §5.3 的映射表）。
// 四条都跑候选二进制（TestMain 解析的同一份产物）+ 临时工作区 + 独立回环端口，
// 缺前置即失败、**不跳过**；取消与工具超时**分开写、分开 required**——取消是 Run 级
// ctx 终止，工具自身超时是单次调用级失败，两者判据不同。
// This file adds US-14's three release-reliability gaps (cancel, tool-level timeout,
// hot reload) and the seven-scenario map self-check. All four run the candidate binary
// against a temporary workspace on loopback ports and fail closed when a prerequisite
// is missing. Cancel and timeout stay separate tests because their verdicts differ:
// run-level context termination versus a single call failing.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// releaseScenario 是「七场景 → 覆盖用例」的一行（开发操作 §5.3 的表格逐行）。
// releaseScenario is one row of the scenario-to-test mapping.
type releaseScenario struct {
	Name  string
	Tests []string
}

// releaseScenarioMap 逐行照抄开发操作 §5.3 的映射表（自检行除外）：取消与工具自身超时
// 按 §5.4「两者判据不同，用例分开写」分列两行，故八行覆盖九条用例、对应七组场景。
// releaseScenarioMap copies the §5.3 table row by row (the self-check row excluded).
// Cancel and tool timeout occupy two rows, so eight rows cover nine tests across the
// seven scenario groups.
var releaseScenarioMap = []releaseScenario{
	{Name: "工具闭环", Tests: []string{"TestBackfill_AgentLoop"}},
	{Name: "越权拒绝", Tests: []string{"TestBackfill_ToolSandbox"}},
	{Name: "取消", Tests: []string{"TestReleaseE2E_Cancel"}},
	{Name: "工具自身超时", Tests: []string{"TestReleaseE2E_ToolTimeout"}},
	{Name: "崩溃恢复", Tests: []string{"TestBackfill_RestartRecovery"}},
	{Name: "同会话并发 409", Tests: []string{"TestBackfill_Web"}},
	{Name: "运行中热加载", Tests: []string{"TestReleaseE2E_HotReload"}},
	{Name: "Web·调度·通知", Tests: []string{
		"TestBackfill_Web", "TestBackfill_Scheduler", "TestBackfill_Notify",
	}},
}

// releaseScenarioRowCount / releaseScenarioTestCount 是上表的冻结行数与去重用例数：
// 少一行、少一条用例或悄悄改名都让自检变红。
const (
	releaseScenarioRowCount  = 8
	releaseScenarioTestCount = 9
)

// TestReleaseE2E_HotReload：运行中改写 AGENT.md 不影响本次 Run，下一次 Run 取新正文
// （模块卡 §4.5.11 / FR-021）。
// 判据落在**系统提示词**上：本 Run 的第 2 次模型请求（工具结果之后）仍带旧正文，
// 下一个 Run 的第 1 次请求带新正文。
// TestReleaseE2E_HotReload: a mid-run AGENT.md rewrite must not leak into the running
// run, while the next run picks the new body up.
func TestReleaseE2E_HotReload(t *testing.T) {
	h := newHarness(t)
	exe := helperBinary(t)
	release := filepath.Join(t.TempDir(), "hot-reload-release.log")
	const (
		oldPrompt = "OLD-PROMPT-发布热加载"
		newPrompt = "NEW-PROMPT-发布热加载"
	)
	fake := newFakeModel(t,
		toolCallResponse("call-release-hot-reload", "shell", mustJSON(map[string]any{
			"executable": exe,
			"arguments":  []string{"block", release},
		})),
		textResponse("本次运行仍是旧正文"),
		textResponse("下一次运行取新正文"),
	)
	addr, p := startReleaseServe(t, h, fake.URL(), oldPrompt)
	defer p.stop(t)

	sess := webCreateSession(t, addr, "demo", nil)
	inflight := startReleaseMessage(t, addr, sess.SessionID, "改写前发起")

	// 工具卡在 block 上 = 本 Run 的快照已冻结、第一次模型请求已带着旧正文发出。
	waitPathAppears(t, release+".started", 60*time.Second, p.output)

	agentPath := filepath.Join(h.Root, ".basalt", "agents", "demo", "AGENT.md")
	rewriteAgentBody(t, agentPath, newPrompt)

	// 热加载的确定性观测点：注册表已换成新条目（运行中的 Run 仍用旧快照）。
	p.waitOutput(t, "Agent 配置已热加载", 30*time.Second)

	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatalf("放行被阻塞的工具失败: %v", err)
	}

	resp := waitWebResult(t, inflight, 60*time.Second, "改写前发起")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "改写前发起")
	var first webRun
	decodeWebData(t, resp, &first, "改写前发起")
	if first.Status != "completed" {
		t.Fatalf("改写前的运行：status = %q，期望 completed（reason %q）", first.Status, first.Reason)
	}

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("本次运行应恰好 2 次模型请求（工具调用前后各一次），实际 %d 次", len(reqs))
	}
	for i := range reqs {
		requireContains(t, systemPrompt(t, reqs[i]), oldPrompt,
			fmt.Sprintf("第 %d 次请求必须仍带旧正文（每轮重读 AGENT.md → 红）", i+1))
		requireAbsent(t, systemPrompt(t, reqs[i]), newPrompt,
			fmt.Sprintf("第 %d 次请求不得混入运行中改写的新正文", i+1))
	}

	second := webPostMessage(t, addr, sess.SessionID, "改写后发起")
	requireWebStatus(t, second, http.StatusOK, "OK", "", "改写后发起")
	var secondRun webRun
	decodeWebData(t, second, &secondRun, "改写后发起")
	if secondRun.Status != "completed" {
		t.Fatalf("改写后的运行：status = %q，期望 completed（reason %q）", secondRun.Status, secondRun.Reason)
	}

	reqs = fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("下一次运行应再发 1 次请求（共 3 次），实际 %d 次", len(reqs))
	}
	requireContains(t, systemPrompt(t, reqs[2]), newPrompt, "下一次运行应取新正文")
	requireAbsent(t, systemPrompt(t, reqs[2]), oldPrompt, "下一次运行不得再带旧正文")
}

// TestReleaseE2E_Cancel：取消 Run 落 cancelled、进程树无残留、同一会话可继续
// （模块卡 §4.5.11 / FR-021；契约 §4.2.1「取消优先于预算」）。
// TestReleaseE2E_Cancel: cancelling lands on cancelled, reaps the whole process tree,
// and the same session keeps working.
func TestReleaseE2E_Cancel(t *testing.T) {
	h := newHarness(t)
	exe := helperBinary(t)
	heartbeat := filepath.Join(t.TempDir(), "cancel-heartbeat.log")
	fake := newFakeModel(t,
		toolCallResponse("call-release-cancel", "shell", mustJSON(map[string]any{
			"executable": exe,
			"arguments":  []string{"tree", heartbeat},
		})),
		textResponse("取消后同一会话继续"),
	)
	addr, p := startReleaseServe(t, h, fake.URL(), "取消演示")
	defer p.stop(t)

	sess := webCreateSession(t, addr, "demo", nil)
	inflight := startReleaseMessage(t, addr, sess.SessionID, "跑一棵常驻进程树")

	// 孙进程已在写心跳 = 工具真的进了执行期（不是"还没开始就取消"）。
	waitHeartbeat(t, heartbeat, 30*time.Second)
	runID := waitRunningRun(t, addr, sess.SessionID, 30*time.Second)

	resp := webRequest(t, addr, http.MethodPost, "/api/v1/runs/"+runID+"/cancel", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "取消运行")
	var cancelled webRun
	decodeWebData(t, resp, &cancelled, "取消运行")
	if cancelled.Status != "cancelled" {
		t.Fatalf("取消运行：status = %q，期望 cancelled（取消是信号，终态由循环写入）", cancelled.Status)
	}

	// 在途请求随取消收场，且报同一个终态。
	first := waitWebResult(t, inflight, 60*time.Second, "在途请求")
	requireWebStatus(t, first, http.StatusOK, "OK", "", "在途请求")
	var firstRun webRun
	decodeWebData(t, first, &firstRun, "在途请求")
	if firstRun.RunID != runID || firstRun.Status != "cancelled" {
		t.Fatalf("在途请求：run_id/status = %s/%q，期望 %s/cancelled", firstRun.RunID, firstRun.Status, runID)
	}

	// 整树回收：只杀父进程时孙进程会继续写心跳，这里必须停住。
	requireHeartbeatStopped(t, heartbeat, "取消后")

	// 半截工具组已补齐，同一会话可继续（契约 §4.2.4 补齐链路）。
	second := webPostMessage(t, addr, sess.SessionID, "取消后继续")
	requireWebStatus(t, second, http.StatusOK, "OK", "", "取消后继续")
	var secondRun webRun
	decodeWebData(t, second, &secondRun, "取消后继续")
	if secondRun.Status != "completed" {
		t.Fatalf("取消后继续：status = %q，期望 completed（reason %q）", secondRun.Status, secondRun.Reason)
	}

	p.stop(t)
	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs WHERE run_id = ? AND status = 'cancelled'`, runID); n != 1 {
		t.Fatalf("库内该 Run 应为 cancelled（1 行），实际 %d 行", n)
	}
	// 一次工具调用必须恰好一条结果消息：取消路径的补齐不得重复追加。
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM messages WHERE session_id = ? AND role = 'tool'`, sess.SessionID); n != 1 {
		t.Fatalf("会话里的工具结果消息应恰好 1 条（半截组补齐后），实际 %d 条", n)
	}
}

// TestReleaseE2E_ToolTimeout：工具自身超时记 failed + timeout，Run 继续并以 completed
// 收尾、失败结果回填下一轮请求（模块卡 §4.5.11 / FR-021；契约 §7.3）。
// **生产代码无测试开关**：不传 timeout_seconds，判的就是缺省单工具超时
// `core.defaultToolTimeout`（30 秒），等待有界 45 秒。
// TestReleaseE2E_ToolTimeout: a tool-level timeout records failed + timeout while the
// run keeps going and closes as completed. The default 30s budget is exercised as-is.
func TestReleaseE2E_ToolTimeout(t *testing.T) {
	h := newHarness(t)
	exe := helperBinary(t)
	heartbeat := filepath.Join(t.TempDir(), "tool-timeout-heartbeat.log")
	fake := newFakeModel(t,
		toolCallResponse("call-release-timeout", "shell", mustJSON(map[string]any{
			"executable": exe,
			"arguments":  []string{"tree", heartbeat},
		})),
		textResponse("工具超时后运行继续"),
	)
	addr, p := startReleaseServe(t, h, fake.URL(), "工具自身超时演示")
	defer p.stop(t)

	sess := webCreateSession(t, addr, "demo", nil)
	inflight := startReleaseMessage(t, addr, sess.SessionID, "跑一棵会超时的进程树")

	waitHeartbeat(t, heartbeat, 30*time.Second)
	runID := waitRunningRun(t, addr, sess.SessionID, 30*time.Second)

	// 判据一：工具审计有界（45 秒）落终态——**不得只等 Run 收场**再读。
	status, kind := waitToolAuditRow(t, h.dbPath(), runID, 45*time.Second)
	if status != "failed" || kind != "timeout" {
		t.Fatalf("工具自身超时应落 failed + timeout，实际 %q + %q", status, kind)
	}

	// 判据二：Run 继续并以 completed 收尾（把工具超时当 Run 失败 → 红）。
	resp := waitWebResult(t, inflight, 90*time.Second, "工具超时运行")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "工具超时运行")
	var run webRun
	decodeWebData(t, resp, &run, "工具超时运行")
	if run.Status != "completed" {
		t.Fatalf("工具超时后 Run 应以 completed 收尾，实际 %q（reason %q）", run.Status, run.Reason)
	}

	// 判据三：失败结果回填进下一轮请求，模型才能收敛。
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "context deadline exceeded", "失败结果必须回填进下一轮请求")

	// 判据四：整棵进程树回收。
	requireHeartbeatStopped(t, heartbeat, "工具超时")

	p.stop(t)
	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status, error_kind FROM tool_invocations WHERE run_id = ?`, runID)
	if len(rows) != 1 {
		t.Fatalf("该 Run 的工具审计应恰好 1 行，实际 %d 行：%v", len(rows), rows)
	}
	if rows[0]["status"] != "failed" || rows[0]["error_kind"] != "timeout" {
		t.Fatalf("工具审计 = %v/%v，期望 failed/timeout", rows[0]["status"], rows[0]["error_kind"])
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs WHERE run_id = ? AND status = 'completed'`, runID); n != 1 {
		t.Fatalf("库内该 Run 应为 completed（1 行），实际 %d 行", n)
	}
}

// TestReleaseE2E_SevenScenarioMap：七场景映射自检（开发操作 §5.3 / 模块卡 §4.5.11）。
// 判据：映射表行数与去重用例数不变；每条用例都真在本二进制里（改名即红）；取消与
// 工具自身超时**各占一行且不是同一条用例**（判据不同，不得合并）。
// TestReleaseE2E_SevenScenarioMap: the mapping table is complete, every mapped test
// exists in this binary, and cancel/timeout remain two distinct tests.
func TestReleaseE2E_SevenScenarioMap(t *testing.T) {
	if len(releaseScenarioMap) != releaseScenarioRowCount {
		t.Fatalf("七场景映射应有 %d 行，实际 %d 行", releaseScenarioRowCount, len(releaseScenarioMap))
	}
	names := listE2ETests(t)
	seen := make(map[string]struct{})
	for _, scenario := range releaseScenarioMap {
		if len(scenario.Tests) == 0 {
			t.Fatalf("场景 %q 没有覆盖用例", scenario.Name)
		}
		for _, name := range scenario.Tests {
			if _, ok := names[name]; !ok {
				t.Fatalf("场景 %q 的覆盖用例 %s 不在本二进制里：改名或删除都会让发布映射失效",
					scenario.Name, name)
			}
			seen[name] = struct{}{}
		}
	}
	if len(seen) != releaseScenarioTestCount {
		t.Fatalf("七场景映射应覆盖 %d 条去重用例，实际 %d 条：%v",
			releaseScenarioTestCount, len(seen), seen)
	}

	cancel := findScenario(t, "取消")
	timeout := findScenario(t, "工具自身超时")
	if len(cancel.Tests) != 1 || len(timeout.Tests) != 1 {
		t.Fatalf("取消 / 工具自身超时应各由一条用例承载，实际 %v / %v", cancel.Tests, timeout.Tests)
	}
	if cancel.Tests[0] == timeout.Tests[0] {
		t.Fatalf("取消与工具自身超时判据不同，必须是两条用例，实际都用 %s", cancel.Tests[0])
	}
}

// findScenario 按场景名取映射行；缺行即判红。
// findScenario returns one mapping row by scenario name; a missing row fails.
func findScenario(t *testing.T, name string) releaseScenario {
	t.Helper()
	for _, scenario := range releaseScenarioMap {
		if scenario.Name == name {
			return scenario
		}
	}
	t.Fatalf("七场景映射缺少 %q 一行", name)
	return releaseScenario{}
}

// listE2ETests 用 `-test.list` 只读列名：TestMain 的 listMode 短路在构建之前，
// 故这一步不会另建二进制。两个短路开关显式清空，列名必须真的走 listMode 分支。
// listE2ETests lists the test names through `-test.list`, which TestMain short-circuits
// before any build; both short-circuit variables are cleared explicitly.
func listE2ETests(t *testing.T) map[string]struct{} {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制失败: %v", err)
	}
	cmd := exec.Command(self, "-test.list=.*")
	cmd.Env = append(os.Environ(), shellHelperEnv+"=", "BASALT_MCP_HELPER=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("列测试名失败（%s -test.list=.*）: %v", self, err)
	}
	names := make(map[string]struct{})
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Test") {
			names[line] = struct{}{}
		}
	}
	if len(names) == 0 {
		t.Fatalf("列测试名返回空集合：%q", out)
	}
	return names
}

// startReleaseServe 起一条 serve 常驻进程：临时工作区 + 假模型 + shell 白名单，
// Agent 目录名为 demo、正文由调用方给定（热加载用例靠正文区分快照）。
// startReleaseServe launches one serve process: temporary workspace, fake model and
// the shell allowlist, with an agent named demo whose body the caller supplies.
func startReleaseServe(t *testing.T, h *harness, providerURL, body string) (string, *groupedProcess) {
	t.Helper()
	initWorkspace(t, h)
	exe := helperBinary(t)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(providerURL, addr)+shellSection(exe, []string{shellHelperEnv}))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"shell"}), body)
	p := startGrouped(t, h, append(fixtureEnv(), shellHelperEnv+"=1"), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	return addr, p
}

// startReleaseMessage 在后台发一条会话消息：同步端点会一直阻塞到 Run 收场，
// 故取消 / 超时 / 热加载都要先把它放到旁路，再从别的面观测在途状态。
// startReleaseMessage posts one session message off the test goroutine, because the
// endpoint blocks until the run finishes.
func startReleaseMessage(t *testing.T, addr, sessionID, message string) <-chan webResult {
	t.Helper()
	raw := mustJSON(map[string]any{"message": message})
	out := make(chan webResult, 1)
	go func() {
		resp, err := doWebRequest(context.Background(), http.MethodPost,
			addr, "/api/v1/sessions/"+sessionID+"/messages", raw, nil)
		out <- webResult{resp: resp, err: err}
	}()
	return out
}

// waitRunningRun 轮询到该会话出现 running 的 Run，返回 run_id——**经管理读面**观测在途
// 状态，不碰运行中的库文件。
// waitRunningRun polls the admin read surface until the session has a running run.
func waitRunningRun(t *testing.T, addr, sessionID string, timeout time.Duration) string {
	t.Helper()
	path := "/api/v1/runs?session_id=" + url.QueryEscape(sessionID) + "&status=running"
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp := webRequest(t, addr, http.MethodGet, path, "", nil)
		switch {
		case resp.Status != http.StatusOK:
			last = resp.Raw
		case resp.HasData:
			var runs []webRun
			if err := json.Unmarshal(resp.Env.Data, &runs); err != nil {
				t.Fatalf("运行列表不是 RunView 数组：%v（%s）", err, resp.Raw)
			}
			for _, run := range runs {
				if run.RunID != "" {
					return run.RunID
				}
			}
			last = resp.Raw
		default:
			last = resp.Raw
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待会话 %s 的在途运行超时（%s）：%s", sessionID, timeout, last)
	return ""
}

// waitHeartbeat 等到心跳文件出现且有内容：孙进程真的在写 = 工具已进入执行期。
// waitHeartbeat waits until the heartbeat file exists with content, proving the
// grandchild is running and the tool really entered its execution phase.
func waitHeartbeat(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待心跳文件 %s 写入超时（%s）：孙进程未起来，用例未测到整树回收", path, timeout)
}

// requireHeartbeatStopped 断言心跳不再增长：整棵树都被回收（只杀父进程时孙进程会继续写）。
// requireHeartbeatStopped asserts the heartbeat stopped growing: the whole tree was reaped.
func requireHeartbeatStopped(t *testing.T, path, label string) {
	t.Helper()
	size := fileSize(t, path)
	if size == 0 {
		t.Fatalf("%s：心跳文件为空，用例未测到整树回收", label)
	}
	time.Sleep(500 * time.Millisecond)
	if again := fileSize(t, path); again != size {
		t.Fatalf("%s：孙进程仍在写心跳（%d → %d 字节），整棵树都要回收", label, size, again)
	}
}

// waitToolAuditRow 有界轮询该 Run 的第一条工具审计行进入终态，返回状态与错误类别。
// 查询错误（含运行期库的瞬时忙）只在窗口内重试，超时后连最后一次错误一起报出。
// waitToolAuditRow polls the run's first tool audit row until it leaves pending; query
// errors are retried only inside the window and reported with the timeout.
func waitToolAuditRow(t *testing.T, dbPath, runID string, timeout time.Duration) (string, string) {
	t.Helper()
	db := openDB(t, dbPath)
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		var status, kind sql.NullString
		err := db.QueryRow(
			`SELECT status, error_kind FROM tool_invocations WHERE run_id = ? ORDER BY id LIMIT 1`,
			runID).Scan(&status, &kind)
		switch {
		case err == nil && status.String != "pending":
			return status.String, kind.String
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			lastErr = err
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待工具审计进入终态超时（%s，run %s，最后一次查询错误 %v）", timeout, runID, lastErr)
	return "", ""
}

// rewriteAgentBody 只替换 AGENT.md 的正文：frontmatter（含夹具写进的绑定块）逐字保留，
// 热加载因此能通过校验、绑定不失效——用例判的就只是"正文何时刷新"。
// rewriteAgentBody replaces only the prompt body; the frontmatter (including the
// fixture-seeded bindings) stays byte for byte, so the reload passes validation.
func rewriteAgentBody(t *testing.T, path, body string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 AGENT.md %s 失败: %v", path, err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatalf("AGENT.md %s 缺 frontmatter 起始行：\n%s", path, text)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("AGENT.md %s 缺 frontmatter 结束行：\n%s", path, text)
	}
	front := text[:len("---\n")+end+len("\n---\n")]
	content := front + "\n" + strings.TrimRight(body, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("改写 AGENT.md %s 失败: %v", path, err)
	}
}

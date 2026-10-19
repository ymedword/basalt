//go:build e2e

package e2e

// 本文件回填 US-1 §6 探针①、US-4 §6.1 Demo⑤、US-9 §6.2 第 6 项与 US-10 §6.2 第 7/8/9 项
// （模块卡 §2.6.1 / §2.6.4 / §2.6.9 / §2.6.10）：进程崩溃后的恢复扫描、跨进程的会话历史、
// `serve` 崩溃重启、启动对账（新增 / 修改 / 删除）与「停机错过只告警不补跑」。五个子测试名
// 与 `backfillCatalog` 的 `Parent/sub` 逐字对应。
// This file backfills crash recovery, cross-process session history, serve restart, startup
// reconciliation and missed-trigger behaviour. The five subtest names match the catalog
// verbatim; every scenario kills or restarts a real process.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	// recoveryCloseReason 是崩溃收尾写进 `runs.reason` 的逐字文本（生产常量在
	// internal/storage/recovery.go；本侧独立冻结，改动任一侧即红）。
	// recoveryCloseReason pins the crash-closure reason verbatim.
	recoveryCloseReason = "进程中断（启动时发现未完成的运行）"

	// resultUnknownText 是「执行中崩溃」的工具调用合成结果（生产常量
	// internal/core/synthesis.go 的 TextResultUnknown）。
	// resultUnknownText pins the synthesized result for a call interrupted mid-flight.
	resultUnknownText = "结果未知（进程中断，无法确定是否已产生外部副作用）"
)

// TestBackfill_RestartRecovery 覆盖崩溃恢复与重启语义的五个进程级人工项。
// TestBackfill_RestartRecovery covers the five process-level restart/recovery items.
func TestBackfill_RestartRecovery(t *testing.T) {
	t.Run("crash_recovery", testRestartRecoveryCrash)
	t.Run("session_history", testRestartRecoverySessionHistory)
	t.Run("serve", testRestartRecoveryServe)
	t.Run("reconcile", testRestartRecoveryReconcile)
	t.Run("missed_fire", testRestartRecoveryMissedFire)
}

// testRestartRecoveryCrash：运行卡在工具执行中途时强杀进程，重启后三处收尾
// （US-1 §6 探针① + US-10 §6.2 第 9 项）。判据：审计 `pending → unknown`、半截工具组
// 补一条 `结果未知（…）` 且带原 `tool_call_id`、Run 翻 `failed` 并写冻结原因、
// `task_executions` 的遗留 `started` 行按 Run 终态收尾、`last_status` 同步。
func testRestartRecoveryCrash(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	exe := helperBinary(t)
	release := filepath.Join(t.TempDir(), "release.flag")
	marker := release + ".started"
	fake := newFakeModel(t,
		toolCallResponse("call-block", "shell", mustJSON(map[string]any{
			"executable":      exe,
			"arguments":       []string{"block", filepath.ToSlash(release)},
			"timeout_seconds": 120,
		})),
	)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr)+shellSection(exe, []string{shellHelperEnv}))
	// 定时只在远离当前时刻的整点触发：本用例走手动补跑，重启窗口里不得有第二次运行。
	hour := (time.Now().UTC().Hour() + 6) % 24
	h.writeAgent(t, "demo",
		agentFrontmatter("demo", []string{"shell"})+
			fmt.Sprintf("schedules:\n  - cron: \"0 0 %d * * *\"\n    message: 崩溃恢复演示\n", hour),
		"崩溃恢复演示")

	env := append(fixtureEnv(), shellHelperEnv+"=1")
	p := startGrouped(t, h, env, "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)

	// 手动补跑走与定时触发同一条执行链；它在工具执行中途阻塞，这里不等响应。
	go triggerScheduleRun(addr, "demo-1")
	waitPathAppears(t, marker, 60*time.Second, p.output)

	// 强杀：Windows 上执行器 Job 随父进程回收替身命令，Linux 上靠放行文件退出。
	p.kill()
	p.waitExit(t, 30*time.Second)
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatalf("写放行文件失败: %v", err)
	}

	addr2 := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr2)+shellSection(exe, []string{shellHelperEnv}))
	p2 := startGrouped(t, h, env, "serve")
	waitHealthHTTP(t, p2, addr2, 30*time.Second)
	p2.waitOutput(t, "崩溃恢复扫描完成 unknowns=1 repaired_groups=1 closed_runs=1", 30*time.Second)
	p2.waitOutput(t, "启动恢复完成 recovered=1", 30*time.Second)
	p2.stop(t)

	db := openDB(t, h.dbPath())
	runs := queryRows(t, db, `SELECT status, reason, finished_at FROM runs`)
	if len(runs) != 1 {
		t.Fatalf("runs 应有 1 行，实际 %d 行：%v", len(runs), runs)
	}
	if got, _ := runs[0]["status"].(string); got != "failed" {
		t.Fatalf("runs.status = %q，期望 failed（崩溃运行必须收尾）", got)
	}
	if got, _ := runs[0]["reason"].(string); got != recoveryCloseReason {
		t.Fatalf("runs.reason = %q，期望冻结文案 %q", got, recoveryCloseReason)
	}
	if runs[0]["finished_at"] == nil {
		t.Fatal("runs.finished_at 未写入：运行没有被真正收尾")
	}

	audits := queryRows(t, db, `SELECT tool_name, status, error_kind FROM tool_invocations`)
	if len(audits) != 1 {
		t.Fatalf("tool_invocations 应有 1 行，实际 %d 行：%v", len(audits), audits)
	}
	if got, _ := audits[0]["status"].(string); got != "unknown" {
		t.Fatalf("tool_invocations.status = %q，期望 unknown", got)
	}
	if got, _ := audits[0]["error_kind"].(string); got != "unknown" {
		t.Fatalf("tool_invocations.error_kind = %q，期望 unknown", got)
	}

	calls := queryRows(t, db, `SELECT status FROM llm_calls`)
	if len(calls) != 1 {
		t.Fatalf("llm_calls 应有 1 行，实际 %d 行：%v", len(calls), calls)
	}
	if got, _ := calls[0]["status"].(string); got != "success" {
		t.Fatalf("llm_calls.status = %q，期望 success（崩溃点必须在工具执行，不是模型调用）", got)
	}

	tools := queryRows(t, db, `SELECT content, tool_call_id FROM messages WHERE role = 'tool'`)
	if len(tools) != 1 {
		t.Fatalf("工具结果消息应有 1 条，实际 %d 条：%v", len(tools), tools)
	}
	if got, _ := tools[0]["content"].(string); got != resultUnknownText {
		t.Fatalf("合成工具结果 = %q，期望 %q", got, resultUnknownText)
	}
	if got, _ := tools[0]["tool_call_id"].(string); got != "call-block" {
		t.Fatalf("合成工具结果的 tool_call_id = %q，期望 call-block", got)
	}

	execs := queryRows(t, db, `SELECT status, run_id, finished_at FROM task_executions`)
	if len(execs) != 1 {
		t.Fatalf("task_executions 应有 1 行，实际 %d 行：%v", len(execs), execs)
	}
	if got, _ := execs[0]["status"].(string); got != "failed" {
		t.Fatalf("task_executions.status = %q，期望 failed（Run 失败即执行失败）", got)
	}
	if runID, _ := execs[0]["run_id"].(string); runID == "" {
		t.Fatal("task_executions.run_id 为空：执行行必须关联对应 Run")
	}
	if execs[0]["finished_at"] == nil {
		t.Fatal("task_executions.finished_at 未写入：遗留 started 行未收尾")
	}

	tasks := queryRows(t, db, `SELECT last_status FROM scheduled_tasks WHERE task_id = 'demo-1'`)
	if len(tasks) != 1 {
		t.Fatalf("scheduled_tasks 应有 demo-1 一行，实际 %d 行：%v", len(tasks), tasks)
	}
	if got, _ := tasks[0]["last_status"].(string); got != "failed" {
		t.Fatalf("scheduled_tasks.last_status = %q，期望 failed", got)
	}
}

// testRestartRecoverySessionHistory：两次**独立进程**的对话共用同一会话，第二轮请求带回
// 上一进程写入的用户与助手消息（US-4 §6.1 第 2 项 Demo⑤）。
func testRestartRecoverySessionHistory(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t, textResponse("第一轮回答"), textResponse("第二轮回答"))
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil), "重启后历史仍在")

	first := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "第一轮问题")
	requireExit(t, first, 0, "第一轮对话")
	requireContains(t, first.Stdout, "第一轮回答", "第一轮对话")

	second := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "第二轮问题")
	requireExit(t, second, 0, "第二轮对话（新进程）")
	requireContains(t, second.Stdout, "第二轮回答", "第二轮对话（新进程）")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	// 历史来自库而非内存：新进程的请求必须同时带回上一进程的用户与助手消息。
	history := requestText(t, reqs[1])
	for _, want := range []string{"第一轮问题", "第一轮回答"} {
		requireContains(t, history, want, "第二轮请求应带回上一进程的历史")
	}

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM sessions`); n != 1 {
		t.Fatalf("sessions 应有 1 行（同一派生会话），实际 %d 行", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 2 {
		t.Fatalf("runs 应有 2 行，实际 %d 行", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM messages WHERE role = 'user'`); n != 2 {
		t.Fatalf("user 消息应有 2 条，实际 %d 条", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM messages WHERE role = 'assistant'`); n != 2 {
		t.Fatalf("assistant 消息应有 2 条，实际 %d 条", n)
	}
}

// testRestartRecoveryServe：`serve` 被强杀后重启可用、数据不丢、启动摘要为零
// （US-9 §6.2 第 6 项）。
func testRestartRecoveryServe(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t, textResponse("重启前写入的回答"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil), "serve 崩溃重启")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "留一条历史")
	requireExit(t, res, 0, "重启前对话")
	requireContains(t, res.Stdout, "重启前写入的回答", "重启前对话")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	p.kill()
	p.waitExit(t, 30*time.Second)

	// 换一个空闲端口重启：本项判的是数据与启动摘要，不是端口复用。
	addr2 := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr2))
	p2 := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p2, addr2, 30*time.Second)
	p2.waitOutput(t, "崩溃恢复扫描完成 unknowns=0 repaired_groups=0 closed_runs=0", 30*time.Second)
	p2.stop(t)

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM sessions`); n != 1 {
		t.Fatalf("重启后会话应仍在（1 行），实际 %d 行", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM messages`); n != 2 {
		t.Fatalf("重启后消息应仍在（2 条），实际 %d 条", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs WHERE status = 'completed'`); n != 1 {
		t.Fatalf("重启后完成的运行应仍为 completed，实际 %d 行", n)
	}
}

// testRestartRecoveryReconcile：启动对账以文件定义为准、`enabled` 以库为准、删除只退休
// （US-10 §6.2 第 7 项）。
//
// 场景：首启两任务（`keep` / `retire-me`）→ 经 HTTP 停用 `keep` → 停机改写 AGENT.md
// （`keep` 改 cron、新增 `added`、删除 `retire-me`）→ 重启。判据：`GET /schedules` 只见
// `added` 与 `keep`；`keep` 的新 cron 生效而 `enabled` 保持停用；`retire-me` 的库行
// `retired_at` 非空且不再触发。
func testRestartRecoveryReconcile(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t, textResponse("未使用"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))

	// 触发点全部落在六小时之后，避免对账期间有任务真的到点。
	hour := (time.Now().UTC().Hour() + 6) % 24
	keepCron := fmt.Sprintf("0 0 %d * * *", hour)
	retireCron := fmt.Sprintf("0 30 %d * * *", hour)
	h.writeAgent(t, "demo", reconcileFrontmatter("demo", keepCron, retireCron, ""), "启动对账演示")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	requireScheduleIDs(t, addr, "keep,retire-me", "首启对账")
	setScheduleEnabled(t, addr, "keep", false)
	p.stop(t)

	changedCron := fmt.Sprintf("0 10 %d * * *", hour)
	addedCron := fmt.Sprintf("0 45 %d * * *", hour)
	h.writeAgent(t, "demo", reconcileFrontmatter("demo", changedCron, "", addedCron), "启动对账演示")

	addr2 := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr2))
	p2 := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p2, addr2, 30*time.Second)
	p2.waitOutput(t, "任务定义已更新", 30*time.Second)
	requireScheduleIDs(t, addr2, "added,keep", "重启对账")
	p2.stop(t)

	db := openDB(t, h.dbPath())
	keep := queryRows(t, db, `SELECT cron, enabled, retired_at FROM scheduled_tasks WHERE task_id = 'keep'`)
	if len(keep) != 1 {
		t.Fatalf("scheduled_tasks 应有 keep 一行，实际 %d 行：%v", len(keep), keep)
	}
	if got, _ := keep[0]["cron"].(string); got != changedCron {
		t.Fatalf("keep.cron = %q，期望新定义 %q（文件定义为准）", got, changedCron)
	}
	if got := intValue(t, keep[0], "enabled"); got != 0 {
		t.Fatalf("keep.enabled = %d，期望 0（enabled 以库为准，不随定义变化翻转）", got)
	}
	if keep[0]["retired_at"] != nil {
		t.Fatalf("keep.retired_at = %v，期望 NULL（仍存在的任务不得退休）", keep[0]["retired_at"])
	}

	added := queryRows(t, db, `SELECT enabled, retired_at FROM scheduled_tasks WHERE task_id = 'added'`)
	if len(added) != 1 {
		t.Fatalf("scheduled_tasks 应有 added 一行，实际 %d 行：%v", len(added), added)
	}
	if got := intValue(t, added[0], "enabled"); got != 1 {
		t.Fatalf("added.enabled = %d，期望 1（新任务默认启用）", got)
	}
	if added[0]["retired_at"] != nil {
		t.Fatalf("added.retired_at = %v，期望 NULL", added[0]["retired_at"])
	}

	retired := queryRows(t, db, `SELECT retired_at FROM scheduled_tasks WHERE task_id = 'retire-me'`)
	if len(retired) != 1 {
		t.Fatalf("删除的任务只退休不物理删除：scheduled_tasks 应有 retire-me 一行，实际 %d 行", len(retired))
	}
	if retired[0]["retired_at"] == nil {
		t.Fatal("retire-me.retired_at 为空：文件里已删除的任务必须被标记退休")
	}

	if n := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions`); n != 0 {
		t.Fatalf("对账期间不得产生执行行，实际 %d 行", n)
	}
}

// testRestartRecoveryMissedFire：停服跨过触发点后，启动只 WARN、不补跑
// （US-10 §6.2 第 8 项）。基线取 `last_run_at`，这里把它回拨 25 小时，保证窗口内
// 恰好有一个触发点。
func testRestartRecoveryMissedFire(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t, textResponse("未使用"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))

	// 取「当前 UTC 小时 + 2」的整点：回拨 25 小时的窗口里恰好一次触发（+2 小时保证
	// 即便跨过整点也恰有一个触发点）。
	hour := (time.Now().UTC().Hour() + 2) % 24
	cron := fmt.Sprintf("0 0 %d * * *", hour)
	h.writeAgent(t, "demo",
		agentFrontmatter("demo", nil)+
			"schedules:\n  - cron: \""+cron+"\"\n    message: 停机错过演示\n",
		"停机错过演示")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	p.stop(t)

	db := openDB(t, h.dbPath())
	backdated := time.Now().Add(-25 * time.Hour).UnixMilli()
	if _, err := db.Exec(`UPDATE scheduled_tasks SET last_run_at = ? WHERE task_id = 'demo-1'`, backdated); err != nil {
		t.Fatalf("回拨 last_run_at 失败: %v", err)
	}

	addr2 := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr2))
	p2 := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p2, addr2, 30*time.Second)
	p2.waitOutput(t, "错过调度触发点", 30*time.Second)
	output := p2.combined()
	requireContains(t, output, "task_id=demo-1", "错过告警点名任务")
	requireContains(t, output, "missed_count=1", "错过告警给出计数")
	p2.stop(t)

	if n := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions`); n != 0 {
		t.Fatalf("停机错过不得补跑，实际产生 %d 条执行行", n)
	}
	rows := queryRows(t, db, `SELECT last_run_at FROM scheduled_tasks WHERE task_id = 'demo-1'`)
	if len(rows) != 1 {
		t.Fatalf("scheduled_tasks 应有 demo-1 一行，实际 %d 行", len(rows))
	}
	if got := intValue(t, rows[0], "last_run_at"); got != backdated {
		t.Fatalf("last_run_at = %d，期望保持回拨值 %d（告警不更新基线）", got, backdated)
	}
}

// reconcileFrontmatter 组装对账场景的 AGENT.md frontmatter：`keep` 恒存在；`retireCron` /
// `addedCron` 为空串表示该条不写。
// reconcileFrontmatter assembles the reconciliation fixture; empty cron means omitted.
func reconcileFrontmatter(name, keepCron, retireCron, addedCron string) string {
	out := agentFrontmatter(name, nil)
	out += "schedules:\n"
	out += "  - id: keep\n    cron: \"" + keepCron + "\"\n    message: 对账保留任务\n"
	if retireCron != "" {
		out += "  - id: retire-me\n    cron: \"" + retireCron + "\"\n    message: 对账下线任务\n"
	}
	if addedCron != "" {
		out += "  - id: added\n    cron: \"" + addedCron + "\"\n    message: 对账新增任务\n"
	}
	return out
}

// scheduleSummary 是 `GET /schedules` 元素的判卷读数（字段与契约 ScheduleView 同名）。
// scheduleSummary is the verdict-side reading of one schedule element; the fields
// mirror the ScheduleView contract.
type scheduleSummary struct {
	ID         string `json:"id"`
	Agent      string `json:"agent"`
	Cron       string `json:"cron"`
	Zone       string `json:"zone"`
	Message    string `json:"message"`
	Enabled    bool   `json:"enabled"`
	LastRunAt  string `json:"last_run_at"`
	LastStatus string `json:"last_status"`
}

// listSchedules 读一次管理面的任务列表（`data` 数组，按 task_id 升序）。
// listSchedules reads the management-face task list.
func listSchedules(t *testing.T, addr string) []scheduleSummary {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + addr + "/api/v1/schedules")
	if err != nil {
		t.Fatalf("GET /schedules 失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /schedules 状态 = %d，期望 200", resp.StatusCode)
	}
	var envelope struct {
		Data []scheduleSummary `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("解析 /schedules 响应失败: %v", err)
	}
	return envelope.Data
}

// requireScheduleIDs 断言列表里的任务 id 按序恰好等于期望串。
// requireScheduleIDs asserts the ordered task ids.
func requireScheduleIDs(t *testing.T, addr, want, label string) {
	t.Helper()
	schedules := listSchedules(t, addr)
	ids := make([]string, 0, len(schedules))
	for _, schedule := range schedules {
		ids = append(ids, schedule.ID)
	}
	if got := strings.Join(ids, ","); got != want {
		t.Fatalf("%s：任务列表 = %q，期望 %q", label, got, want)
	}
}

// setScheduleEnabled 经 HTTP 启停一条任务并要求 200。
// setScheduleEnabled toggles one task through the HTTP face.
func setScheduleEnabled(t *testing.T, addr, id string, enabled bool) {
	t.Helper()
	body := fmt.Sprintf(`{"enabled":%t}`, enabled)
	req, err := http.NewRequest(http.MethodPut, "http://"+addr+"/api/v1/schedules/"+id, strings.NewReader(body))
	if err != nil {
		t.Fatalf("组装启停请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("启停任务 %s 失败: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("启停任务 %s 状态 = %d，期望 200", id, resp.StatusCode)
	}
}

// triggerScheduleRun 在后台发一次手动补跑（运行会阻塞到本次执行结束；调用方随即强杀
// 进程，因此结果不参与断言，也不得触碰 *testing.T）。
// triggerScheduleRun fires one manual run in the background; the caller kills the
// process shortly after, so the outcome is deliberately ignored.
func triggerScheduleRun(addr, id string) {
	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/v1/schedules/"+id+"/run", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// waitPathAppears 轮询到文件出现；超时附带调用方给的诊断读数（进程输出）。
// waitPathAppears polls for a file; the timeout message carries caller diagnostics.
func waitPathAppears(t *testing.T, path string, timeout time.Duration, diagnostics func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(readinessPoll)
	}
	detail := ""
	if diagnostics != nil {
		detail = "\n" + diagnostics()
	}
	t.Fatalf("等待 %s 出现超时（%s）%s", path, timeout, detail)
}

// intValue 读一行里的整数列（SQLite INTEGER 经驱动为 int64）。
// intValue reads an integer column from one row.
func intValue(t *testing.T, row map[string]any, key string) int64 {
	t.Helper()
	value, ok := row[key].(int64)
	if !ok {
		t.Fatalf("%s 列不是整数：%#v", key, row[key])
	}
	return value
}

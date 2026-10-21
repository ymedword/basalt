//go:build e2e

package e2e

// 本文件回填 US-10 §6.2 的十项进程级人工验收（模块卡 §2.6.10 的 1b / 2b / 3b / 4b / 5b /
// 6b / 10b / 11b / 13b / 14b）：cron 到点触发、手动补跑、两张表的一致性、Agent 自管任务、
// 跨 Agent 改写被拒、`CRON_TZ` 生效、对话期间不触发、任务级互斥、归档与到点相撞、跨 Agent
// 任务 id 守卫。十个子测试名与 `backfillCatalog` 的 `Parent/sub` 逐字对应。
// This file backfills the ten process-level scheduler items of US-10 §6.2. Every subtest
// drives a real binary and asserts only process output, exit codes and the workspace DB.

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
	// taskBusyMessage 是任务锁撞车的 409 冻结文案（生产常量 internal/web/handlers_schedules.go）。
	// taskBusyMessage pins the frozen 409 text for a busy task lock.
	taskBusyMessage = "该任务上一轮尚未结束，请等待完成或稍后重试"

	// sessionBusyMessage 是会话在途时归档被拒的 409 冻结文案（契约 run-and-errors §3.1）。
	// sessionBusyMessage pins the frozen 409 text for archiving a busy session.
	sessionBusyMessage = "该会话有正在执行的任务，请等待完成或稍后重试"

	// archivedSkipMessage 是到点撞上归档会话时写进执行行的逐字文案（生产常量
	// internal/core/scheduler.go 的 schedulerArchivedMessage；本侧独立冻结，改动任一侧即红）。
	// archivedSkipMessage pins the archived-skip literal written into the execution row.
	archivedSkipMessage = "会话已归档，本轮跳过"

	// sessionArchivedMessage 是归档会话手动补跑的 400 冻结文案（internal/web/handlers_sessions.go）。
	// sessionArchivedMessage pins the frozen 400 text for a manual run on an archived session.
	sessionArchivedMessage = "会话已归档，不可追加消息"

	// scheduleLead 是「计算触发点 → 进程就绪」的时间余量：足够覆盖二进制启动与启动对账，
	// 又不至于让用例白等太久。
	// scheduleLead is the slack between computing a fire time and starting the process.
	scheduleLead = 15 * time.Second
)

// TestBackfill_Scheduler 覆盖 US-10 调度面的十项进程级人工验收。
// TestBackfill_Scheduler covers the ten process-level scheduler items of US-10.
func TestBackfill_Scheduler(t *testing.T) {
	t.Run("cron_fire", testSchedulerCronFire)
	t.Run("manual_run", testSchedulerManualRun)
	t.Run("two_tables", testSchedulerTwoTables)
	t.Run("agent_self_manage", testSchedulerAgentSelfManage)
	t.Run("agent_other_denied", testSchedulerAgentOtherDenied)
	t.Run("zone_shanghai", testSchedulerZoneShanghai)
	t.Run("chat_no_trigger", testSchedulerChatNoTrigger)
	t.Run("concurrent_mutex", testSchedulerConcurrentMutex)
	t.Run("archive_meets_cron", testSchedulerArchiveMeetsCron)
	t.Run("cross_agent_task_id", testSchedulerCrossAgentTaskID)
}

// testSchedulerCronFire：六段 cron 到点自动触发一次完整运行（US-10 §6.2 第 1 项）。
// 判据：进程日志出现「调度触发」；Run 账本 trigger=cron / channel=scheduler；调度器会话
// channel / user 均为 scheduler；执行行 completed 且 run_id 与 Run 对齐；任务 last_status
// 同步为 completed；用户消息就是任务 message。
// testSchedulerCronFire fires a real cron trigger and asserts the whole run ledger chain.
func testSchedulerCronFire(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	cron := cronAt(time.Now().UTC().Add(scheduleLead))
	fake := newFakeModel(t, textResponse("到点执行完成"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil)+scheduleBlock(cron, "", "到点演示"), "cron 触发演示")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	p.waitOutput(t, "调度触发", 60*time.Second)
	waitScheduleStatus(t, addr, "demo-1", "completed", 30*time.Second)
	p.stop(t)

	db := openDB(t, h.dbPath())
	runs := queryRows(t, db, `SELECT run_id, session_id, agent, channel, trigger, status FROM runs`)
	if len(runs) != 1 {
		t.Fatalf("runs 应有 1 行，实际 %d 行：%v", len(runs), runs)
	}
	runID := stringValue(t, runs[0], "run_id")
	if got := stringValue(t, runs[0], "channel"); got != "scheduler" {
		t.Fatalf("runs.channel = %q，期望 scheduler", got)
	}
	if got := stringValue(t, runs[0], "trigger"); got != "cron" {
		t.Fatalf("runs.trigger = %q，期望 cron", got)
	}
	if got := stringValue(t, runs[0], "status"); got != "completed" {
		t.Fatalf("runs.status = %q，期望 completed", got)
	}
	if got := stringValue(t, runs[0], "agent"); got != "demo" {
		t.Fatalf("runs.agent = %q，期望 demo", got)
	}
	sessionID := stringValue(t, runs[0], "session_id")
	if !strings.HasPrefix(sessionID, "scheduler-") {
		t.Fatalf("runs.session_id = %q，期望 scheduler- 派生前缀", sessionID)
	}

	sessions := queryRows(t, db, `SELECT session_id, channel, user_id, status FROM sessions`)
	if len(sessions) != 1 {
		t.Fatalf("sessions 应有 1 行，实际 %d 行：%v", len(sessions), sessions)
	}
	if got := stringValue(t, sessions[0], "session_id"); got != sessionID {
		t.Fatalf("sessions.session_id = %q，期望与 Run 一致 %q", got, sessionID)
	}
	if got := stringValue(t, sessions[0], "channel"); got != "scheduler" {
		t.Fatalf("sessions.channel = %q，期望 scheduler", got)
	}
	if got := stringValue(t, sessions[0], "user_id"); got != "scheduler" {
		t.Fatalf("sessions.user_id = %q，期望 scheduler", got)
	}
	if got := stringValue(t, sessions[0], "status"); got != "active" {
		t.Fatalf("sessions.status = %q，期望 active", got)
	}

	executions := queryRows(t, db, `SELECT task_id, run_id, status, trigger, finished_at FROM task_executions`)
	if len(executions) != 1 {
		t.Fatalf("task_executions 应有 1 行，实际 %d 行：%v", len(executions), executions)
	}
	if got := stringValue(t, executions[0], "task_id"); got != "demo-1" {
		t.Fatalf("task_executions.task_id = %q，期望 demo-1（单项任务派生 id）", got)
	}
	if got := stringValue(t, executions[0], "run_id"); got != runID {
		t.Fatalf("task_executions.run_id = %q，期望与 Run 账本一致 %q", got, runID)
	}
	if got := stringValue(t, executions[0], "status"); got != "completed" {
		t.Fatalf("task_executions.status = %q，期望 completed", got)
	}
	if got := stringValue(t, executions[0], "trigger"); got != "cron" {
		t.Fatalf("task_executions.trigger = %q，期望 cron", got)
	}
	if executions[0]["finished_at"] == nil {
		t.Fatal("task_executions.finished_at 为空：终态行必须写收尾时间")
	}

	tasks := queryRows(t, db, `SELECT last_status, last_run_at FROM scheduled_tasks WHERE task_id = 'demo-1'`)
	if len(tasks) != 1 {
		t.Fatalf("scheduled_tasks 应有 demo-1 一行，实际 %d 行：%v", len(tasks), tasks)
	}
	if got := stringValue(t, tasks[0], "last_status"); got != "completed" {
		t.Fatalf("scheduled_tasks.last_status = %q，期望 completed", got)
	}
	if tasks[0]["last_run_at"] == nil {
		t.Fatal("scheduled_tasks.last_run_at 为空：触发必须留下运行痕迹")
	}

	users := queryRows(t, db, `SELECT content FROM messages WHERE session_id = ? AND role = 'user'`, sessionID)
	if len(users) != 1 {
		t.Fatalf("调度器会话应有 1 条用户消息，实际 %d 条：%v", len(users), users)
	}
	if got := stringValue(t, users[0], "content"); got != "到点演示" {
		t.Fatalf("用户消息 = %q，期望任务 message「到点演示」", got)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM messages WHERE role = 'assistant'`); n != 1 {
		t.Fatalf("调度器会话应有 1 条助手消息，实际 %d 条", n)
	}

	if got := len(fake.Requests()); got != 1 {
		t.Fatalf("假模型应收到 1 次请求，实际 %d 次", got)
	}
}

// testSchedulerManualRun：管理台手动补跑走同一条执行链（US-10 §6.2 第 2 项）。
// 判据：200 响应的 trigger=manual、channel=scheduler；执行行 trigger=manual 且 run_id
// 与响应一致；last_status / last_run_at 同步。
// testSchedulerManualRun asserts the manual run shares the single execution chain.
func testSchedulerManualRun(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	cron := farFutureCron()
	fake := newFakeModel(t, textResponse("手动补跑完成"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil)+scheduleBlock(cron, "", "手动补跑演示"), "手动补跑演示")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)

	resp := runScheduleNow(t, addr, "demo-1")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "手动补跑")
	var run webRun
	decodeWebData(t, resp, &run, "手动补跑")
	if run.Trigger != "manual" {
		t.Fatalf("RunView.trigger = %q，期望 manual", run.Trigger)
	}
	if run.Status != "completed" {
		t.Fatalf("RunView.status = %q，期望 completed（信封 %s）", run.Status, resp.Raw)
	}
	if run.Channel != "scheduler" {
		t.Fatalf("RunView.channel = %q，期望 scheduler", run.Channel)
	}
	if run.Agent != "demo" {
		t.Fatalf("RunView.agent = %q，期望 demo", run.Agent)
	}
	if !strings.HasPrefix(run.SessionID, "scheduler-") {
		t.Fatalf("RunView.session_id = %q，期望 scheduler- 派生前缀", run.SessionID)
	}
	requireContains(t, run.FinalText, "手动补跑完成", "手动补跑最终回答")
	p.stop(t)

	db := openDB(t, h.dbPath())
	executions := queryRows(t, db, `SELECT run_id, status, trigger, finished_at FROM task_executions`)
	if len(executions) != 1 {
		t.Fatalf("task_executions 应有 1 行，实际 %d 行：%v", len(executions), executions)
	}
	if got := stringValue(t, executions[0], "trigger"); got != "manual" {
		t.Fatalf("task_executions.trigger = %q，期望 manual", got)
	}
	if got := stringValue(t, executions[0], "status"); got != "completed" {
		t.Fatalf("task_executions.status = %q，期望 completed", got)
	}
	if got := stringValue(t, executions[0], "run_id"); got != run.RunID {
		t.Fatalf("task_executions.run_id = %q，期望与响应一致 %q", got, run.RunID)
	}
	if executions[0]["finished_at"] == nil {
		t.Fatal("task_executions.finished_at 为空：终态行必须写收尾时间")
	}

	runs := queryRows(t, db, `SELECT trigger FROM runs`)
	if len(runs) != 1 {
		t.Fatalf("runs 应有 1 行，实际 %d 行：%v", len(runs), runs)
	}
	if got := stringValue(t, runs[0], "trigger"); got != "manual" {
		t.Fatalf("runs.trigger = %q，期望 manual", got)
	}

	tasks := queryRows(t, db, `SELECT last_status, last_run_at FROM scheduled_tasks WHERE task_id = 'demo-1'`)
	if len(tasks) != 1 {
		t.Fatalf("scheduled_tasks 应有 demo-1 一行，实际 %d 行：%v", len(tasks), tasks)
	}
	if got := stringValue(t, tasks[0], "last_status"); got != "completed" {
		t.Fatalf("scheduled_tasks.last_status = %q，期望 completed", got)
	}
	if tasks[0]["last_run_at"] == nil {
		t.Fatal("scheduled_tasks.last_run_at 为空：手动补跑必须留下运行痕迹")
	}
}

// testSchedulerTwoTables：管理读面与执行历史两视图同源一致（US-10 §6.2 第 3 项）。
// 两次手动补跑相隔 1.1 秒（RFC3339 秒精度），判据：/schedules 的 last_status / last_run_at
// 与库一致；/executions 按 started_at DESC、id DESC 排列且 run_id 与库序一致。
// testSchedulerTwoTables checks the two read views against the database after two runs.
func testSchedulerTwoTables(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	cron := farFutureCron()
	fake := newFakeModel(t, textResponse("第一次"), textResponse("第二次"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil)+scheduleBlock(cron, "", "两表演示"), "两表演示")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)

	requireWebStatus(t, runScheduleNow(t, addr, "demo-1"), http.StatusOK, "OK", "", "第一次补跑")
	time.Sleep(1100 * time.Millisecond)
	requireWebStatus(t, runScheduleNow(t, addr, "demo-1"), http.StatusOK, "OK", "", "第二次补跑")

	schedules := listSchedules(t, addr)
	if len(schedules) != 1 {
		t.Fatalf("任务列表应有 1 条，实际 %d 条：%v", len(schedules), schedules)
	}
	entry := schedules[0]
	if entry.ID != "demo-1" {
		t.Fatalf("任务列表 id = %q，期望 demo-1", entry.ID)
	}
	if entry.LastStatus != "completed" {
		t.Fatalf("任务列表 last_status = %q，期望 completed", entry.LastStatus)
	}
	execs := listExecutions(t, addr, "demo-1")
	if len(execs) != 2 {
		t.Fatalf("执行历史应有 2 条，实际 %d 条：%v", len(execs), execs)
	}
	if execs[0].Trigger != "manual" || execs[1].Trigger != "manual" {
		t.Fatalf("执行历史 trigger = %q / %q，期望均为 manual", execs[0].Trigger, execs[1].Trigger)
	}
	first, err := time.Parse(time.RFC3339, execs[0].StartedAt)
	if err != nil {
		t.Fatalf("执行历史 started_at 不是 RFC3339：%q（%v）", execs[0].StartedAt, err)
	}
	second, err := time.Parse(time.RFC3339, execs[1].StartedAt)
	if err != nil {
		t.Fatalf("执行历史 started_at 不是 RFC3339：%q（%v）", execs[1].StartedAt, err)
	}
	if !first.After(second) {
		t.Fatalf("执行历史未按 started_at 倒序：%q 不晚于 %q", execs[0].StartedAt, execs[1].StartedAt)
	}
	if execs[0].RunID == execs[1].RunID {
		t.Fatalf("两次补跑的 run_id 相同（%q）：执行历史必须逐行独立", execs[0].RunID)
	}
	p.stop(t)

	db := openDB(t, h.dbPath())
	tasks := queryRows(t, db, `SELECT last_status, last_run_at FROM scheduled_tasks WHERE task_id = 'demo-1'`)
	if len(tasks) != 1 {
		t.Fatalf("scheduled_tasks 应有 demo-1 一行，实际 %d 行：%v", len(tasks), tasks)
	}
	if got := stringValue(t, tasks[0], "last_status"); got != entry.LastStatus {
		t.Fatalf("库内 last_status = %q，视图 = %q：两视图必须同源", got, entry.LastStatus)
	}
	lastRunAt := time.UnixMilli(intValue(t, tasks[0], "last_run_at")).UTC().Format(time.RFC3339)
	if entry.LastRunAt != lastRunAt {
		t.Fatalf("视图 last_run_at = %q，库内投影 = %q：秒精度投影必须一致", entry.LastRunAt, lastRunAt)
	}

	ordered := queryRows(t, db,
		`SELECT run_id FROM task_executions WHERE task_id = 'demo-1' ORDER BY started_at DESC, id DESC`)
	if len(ordered) != 2 {
		t.Fatalf("task_executions 应有 2 行，实际 %d 行：%v", len(ordered), ordered)
	}
	for i, row := range ordered {
		if got := stringValue(t, row, "run_id"); got != execs[i].RunID {
			t.Fatalf("执行历史第 %d 条 run_id = %q，库内排序后 = %q", i, execs[i].RunID, got)
		}
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs WHERE trigger = 'manual'`); n != 2 {
		t.Fatalf("runs 应有 2 条 manual，实际 %d 条", n)
	}
}

// testSchedulerAgentSelfManage：Agent 经 manage_schedule 给自己加任务（US-10 §6.2 第 4 项）。
// 判据：AGENT.md 写回带引号的 schedules 块、管理读面出现新任务、工具审计成功、确认文案
// 回给模型。
// testSchedulerAgentSelfManage lets an agent add its own task through the tool.
func testSchedulerAgentSelfManage(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	cron := farFutureCron()
	fake := newFakeModel(t,
		toolCallResponse("call-set", "manage_schedule", mustJSON(map[string]any{
			"action":  "set",
			"id":      "self-added",
			"cron":    cron,
			"message": "自管任务",
		})),
		textResponse("已设置"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"manage_schedule"}), "自管演示")
	if before := string(agentFile(t, h, "demo")); strings.Contains(before, "schedules:") {
		t.Fatalf("起始 AGENT.md 不应含 schedules 块：\n%s", before)
	}

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)

	resp := webInvoke(t, addr, "demo", "给自己加一个任务")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "自管调用")
	var run webRun
	decodeWebData(t, resp, &run, "自管调用")
	requireContains(t, run.FinalText, "已设置", "自管调用最终回答")

	requireScheduleIDs(t, addr, "self-added", "自管后任务列表")
	schedules := listSchedules(t, addr)
	if got := schedules[0].Agent; got != "demo" {
		t.Fatalf("任务归属 = %q，期望 demo", got)
	}
	if got := schedules[0].Cron; got != cron {
		t.Fatalf("任务 cron = %q，期望 %q", got, cron)
	}
	if !schedules[0].Enabled {
		t.Fatal("新任务应默认启用（enabled=true）")
	}

	after := string(agentFile(t, h, "demo"))
	requireContains(t, after, `- id: "self-added"`, "AGENT.md 应写回任务 id")
	requireContains(t, after, `cron: "`+cron+`"`, "AGENT.md 应写回 cron")
	requireContains(t, after, `message: "自管任务"`, "AGENT.md 应写回 message")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求（工具轮 + 收尾），实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "已保存定时任务：self-added", "确认文案回给模型")
	p.stop(t)

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT task_id, agent_name, enabled FROM scheduled_tasks`)
	if len(rows) != 1 {
		t.Fatalf("scheduled_tasks 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	if got := stringValue(t, rows[0], "task_id"); got != "self-added" {
		t.Fatalf("scheduled_tasks.task_id = %q，期望 self-added", got)
	}
	if got := stringValue(t, rows[0], "agent_name"); got != "demo" {
		t.Fatalf("scheduled_tasks.agent_name = %q，期望 demo", got)
	}
	if got := intValue(t, rows[0], "enabled"); got != 1 {
		t.Fatalf("scheduled_tasks.enabled = %d，期望 1", got)
	}
	requireToolAudit(t, db, "manage_schedule", "success", "", 1)
}

// testSchedulerAgentOtherDenied：改写别的 Agent 的任务被拒（US-10 §6.2 第 5 项）。
// 判据：拒绝原因回给模型、工具审计 failed / invalid_input、两侧 AGENT.md 字节不变、
// 库内归属不变。
// testSchedulerAgentOtherDenied proves the write gate rejects a cross-agent takeover.
func testSchedulerAgentOtherDenied(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	cron := farFutureCron()
	fake := newFakeModel(t,
		toolCallResponse("call-steal", "manage_schedule", mustJSON(map[string]any{
			"action":  "set",
			"id":      "shared",
			"cron":    cron,
			"message": "接管尝试",
		})),
		textResponse("被拒绝了"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo-a",
		agentFrontmatter("demo-a", nil)+
			"schedules:\n  - id: shared\n    cron: \""+cron+"\"\n    message: 共享任务\n",
		"归属演示")
	h.writeAgent(t, "demo-b", agentFrontmatter("demo-b", []string{"manage_schedule"}), "归属演示")
	beforeA := agentFile(t, h, "demo-a")
	beforeB := agentFile(t, h, "demo-b")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)

	resp := webInvoke(t, addr, "demo-b", "把 demo-a 的 shared 改成我的")
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "跨 Agent 改写")
	var run webRun
	decodeWebData(t, resp, &run, "跨 Agent 改写")
	requireContains(t, run.FinalText, "被拒绝了", "跨 Agent 改写最终回答")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求（工具轮 + 收尾），实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "属于 Agent demo-a", "拒绝原因回给模型")

	if got, want := string(agentFile(t, h, "demo-a")), string(beforeA); got != want {
		t.Fatalf("demo-a 的 AGENT.md 被改动：\n--- 之前 ---\n%s\n--- 之后 ---\n%s", want, got)
	}
	if got, want := string(agentFile(t, h, "demo-b")), string(beforeB); got != want {
		t.Fatalf("demo-b 的 AGENT.md 被改动：\n--- 之前 ---\n%s\n--- 之后 ---\n%s", want, got)
	}
	requireScheduleIDs(t, addr, "shared", "拒绝后任务列表不变")
	p.stop(t)

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "manage_schedule", "failed", "invalid_input", 1)
	audit := queryRows(t, db, `SELECT error_message FROM tool_invocations WHERE tool_name = 'manage_schedule'`)
	requireContains(t, stringValue(t, audit[0], "error_message"), "属于 Agent demo-a", "审计错误原文点名归属")
	owner := queryRows(t, db, `SELECT agent_name FROM scheduled_tasks WHERE task_id = 'shared'`)
	if len(owner) != 1 {
		t.Fatalf("scheduled_tasks 应有 shared 一行，实际 %d 行：%v", len(owner), owner)
	}
	if got := stringValue(t, owner[0], "agent_name"); got != "demo-a" {
		t.Fatalf("shared 归属 = %q，期望 demo-a（拒绝后不得改主）", got)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM scheduled_tasks WHERE agent_name = 'demo-b'`); n != 0 {
		t.Fatalf("demo-b 不应拥有任何任务，实际 %d 条", n)
	}
}

// testSchedulerZoneShanghai：`CRON_TZ` 前缀生效——按 Asia/Shanghai 墙钟触发（US-10 §6.2
// 第 6 项）。触发点按 +08:00 的墙钟字段生成；若实现忽略 zone（按 UTC 解释），同一表达式会
// 晚 8 小时触发，本用例必然超时变红。
// testSchedulerZoneShanghai proves the per-task zone is honoured: an ignored zone would
// fire eight hours later and time the test out.
func testSchedulerZoneShanghai(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	cst := time.FixedZone("CST", 8*60*60)
	cron := cronAt(time.Now().Add(scheduleLead).In(cst))
	fake := newFakeModel(t, textResponse("时区执行完成"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil)+scheduleBlock(cron, "Asia/Shanghai", "时区演示"), "时区演示")

	p := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)
	// 60 秒远小于 8 小时：zone 被忽略时这里必然超时。
	p.waitOutput(t, "调度触发", 60*time.Second)
	waitScheduleStatus(t, addr, "demo-1", "completed", 30*time.Second)
	p.stop(t)

	db := openDB(t, h.dbPath())
	tasks := queryRows(t, db, `SELECT zone, last_status FROM scheduled_tasks WHERE task_id = 'demo-1'`)
	if len(tasks) != 1 {
		t.Fatalf("scheduled_tasks 应有 demo-1 一行，实际 %d 行：%v", len(tasks), tasks)
	}
	if got := stringValue(t, tasks[0], "zone"); got != "Asia/Shanghai" {
		t.Fatalf("scheduled_tasks.zone = %q，期望 Asia/Shanghai", got)
	}
	if got := stringValue(t, tasks[0], "last_status"); got != "completed" {
		t.Fatalf("scheduled_tasks.last_status = %q，期望 completed", got)
	}
	if n := scalarInt(t, db,
		`SELECT COUNT(*) FROM task_executions WHERE status = 'completed' AND trigger = 'cron'`); n != 1 {
		t.Fatalf("应有 1 条 cron completed 执行行，实际 %d 条", n)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 1 {
		t.Fatalf("runs 应有 1 行，实际 %d 行", n)
	}
}

// testSchedulerChatNoTrigger：对话进程不带调度器，逐秒 cron 一次也不得触发（US-10 §6.2
// 第 10 项）。对话用 shell 替身卡住 2.5 秒，跨越两个整秒边界。
// testSchedulerChatNoTrigger parks a chat run across two second boundaries and proves
// the scheduler never fires in chat mode.
func testSchedulerChatNoTrigger(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	exe := helperBinary(t)
	release := filepath.Join(t.TempDir(), "release.flag")
	fake := newFakeModel(t,
		toolCallResponse("call-park", "shell", mustJSON(map[string]any{
			"executable":      exe,
			"arguments":       []string{"block", filepath.ToSlash(release)},
			"timeout_seconds": 120,
		})),
		textResponse("对话结束"))
	h.writeConfig(t, configWithProvider(fake.URL())+shellSection(exe, []string{shellHelperEnv}))
	h.writeAgent(t, "demo",
		agentFrontmatter("demo", []string{"shell"})+
			"schedules:\n  - cron: \"* * * * * *\"\n    message: 每秒任务\n",
		"chat 不触发演示")

	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(2500 * time.Millisecond)
		if err := os.WriteFile(release, []byte("release"), 0o644); err != nil {
			t.Errorf("写放行文件失败: %v", err)
		}
	}()
	res := h.run(t, append(fixtureEnv(), shellHelperEnv+"=1"),
		"chat", "--agent", "demo", "--message", "聊天期间不触发")
	<-done

	requireExit(t, res, 0, "对话")
	requireContains(t, res.Stdout, "对话结束", "对话")
	if output := res.Stdout + res.Stderr; strings.Contains(output, "调度触发") {
		t.Fatalf("chat 进程不应触发定时任务：\n%s", output)
	}

	db := openDB(t, h.dbPath())
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions`); n != 0 {
		t.Fatalf("chat 期间不得产生执行行，实际 %d 条", n)
	}
	runs := queryRows(t, db, `SELECT channel, trigger FROM runs`)
	if len(runs) != 1 {
		t.Fatalf("runs 应有 1 行，实际 %d 行：%v", len(runs), runs)
	}
	if got := stringValue(t, runs[0], "channel"); got != "cli" {
		t.Fatalf("runs.channel = %q，期望 cli", got)
	}
	if got := stringValue(t, runs[0], "trigger"); got != "user" {
		t.Fatalf("runs.trigger = %q，期望 user", got)
	}
}

// testSchedulerConcurrentMutex：同一任务并发触发时，第二个请求得到 409 且不留执行行
// （US-10 §6.2 第 11 项）。
// testSchedulerConcurrentMutex proves the per-task lock rejects the second trigger with
// the frozen 409 text and writes no execution row.
func testSchedulerConcurrentMutex(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	exe := helperBinary(t)
	release := filepath.Join(t.TempDir(), "release.flag")
	marker := release + ".started"
	cron := farFutureCron()
	fake := newFakeModel(t,
		toolCallResponse("call-park", "shell", mustJSON(map[string]any{
			"executable":      exe,
			"arguments":       []string{"block", filepath.ToSlash(release)},
			"timeout_seconds": 120,
		})),
		textResponse("首轮完成"))
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr)+shellSection(exe, []string{shellHelperEnv}))
	h.writeAgent(t, "demo",
		agentFrontmatter("demo", []string{"shell"})+scheduleBlock(cron, "", "互斥演示"),
		"互斥演示")

	p := startGrouped(t, h, append(fixtureEnv(), shellHelperEnv+"=1"), "serve")
	waitHealthHTTP(t, p, addr, 30*time.Second)

	go triggerScheduleRun(addr, "demo-1")
	waitPathAppears(t, marker, 60*time.Second, p.output)

	second := runScheduleNow(t, addr, "demo-1")
	requireWebStatus(t, second, http.StatusConflict, "SESSION_BUSY", taskBusyMessage, "并发补跑")
	if err := os.WriteFile(release, []byte("release"), 0o644); err != nil {
		t.Fatalf("写放行文件失败: %v", err)
	}
	waitScheduleStatus(t, addr, "demo-1", "completed", 60*time.Second)
	p.stop(t)

	db := openDB(t, h.dbPath())
	executions := queryRows(t, db, `SELECT task_id, run_id, status, trigger FROM task_executions`)
	if len(executions) != 1 {
		t.Fatalf("被拒的并发请求不得留执行行：task_executions 应有 1 行，实际 %d 行：%v",
			len(executions), executions)
	}
	if got := stringValue(t, executions[0], "task_id"); got != "demo-1" {
		t.Fatalf("task_executions.task_id = %q，期望 demo-1", got)
	}
	if got := stringValue(t, executions[0], "status"); got != "completed" {
		t.Fatalf("task_executions.status = %q，期望 completed", got)
	}
	if got := stringValue(t, executions[0], "trigger"); got != "manual" {
		t.Fatalf("task_executions.trigger = %q，期望 manual", got)
	}
	if executions[0]["run_id"] == nil {
		t.Fatal("task_executions.run_id 为空：完成的运行必须对齐 Run 账本")
	}
}

// testSchedulerArchiveMeetsCron：归档与到点相撞（US-10 §6.2 第 13 项）。
// 阶段 A 用一次手动补跑拿到派生会话 id；阶段 B 先在途归档被 409 拒绝，运行结束后归档成功；
// 随后 cron 到点被跳过（执行行 skipped、run_id 为空、无新 Run），手动补跑返回 400。
// testSchedulerArchiveMeetsCron walks the archive-vs-cron collision end to end.
func testSchedulerArchiveMeetsCron(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)

	// 阶段 A：一次快速手动补跑，创建调度器会话并读回派生 id。
	fakeA := newFakeModel(t, textResponse("预热完成"))
	addrA := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fakeA.URL(), addrA))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil)+scheduleBlock(farFutureCron(), "", "归档演示"), "归档演示")
	pA := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, pA, addrA, 30*time.Second)
	requireWebStatus(t, runScheduleNow(t, addrA, "demo-1"), http.StatusOK, "OK", "", "预热补跑")
	pA.stop(t)

	db := openDB(t, h.dbPath())
	sessions := queryRows(t, db, `SELECT session_id FROM sessions WHERE channel = 'scheduler'`)
	if len(sessions) != 1 {
		t.Fatalf("预热后应有 1 个调度器会话，实际 %d 个：%v", len(sessions), sessions)
	}
	sessionID := stringValue(t, sessions[0], "session_id")

	// 阶段 B：到点在 15 秒后；在途归档被拒，运行结束后归档成功。
	exe := helperBinary(t)
	release := filepath.Join(t.TempDir(), "release.flag")
	marker := release + ".started"
	fakeB := newFakeModel(t,
		toolCallResponse("call-park", "shell", mustJSON(map[string]any{
			"executable":      exe,
			"arguments":       []string{"block", filepath.ToSlash(release)},
			"timeout_seconds": 120,
		})),
		textResponse("归档前完成"))
	addrB := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fakeB.URL(), addrB)+shellSection(exe, []string{shellHelperEnv}))
	h.writeAgent(t, "demo",
		agentFrontmatter("demo", []string{"shell"})+
			scheduleBlock(cronAt(time.Now().UTC().Add(scheduleLead)), "", "归档演示"),
		"归档演示")

	pB := startGrouped(t, h, append(fixtureEnv(), shellHelperEnv+"=1"), "serve")
	waitHealthHTTP(t, pB, addrB, 30*time.Second)

	go triggerScheduleRun(addrB, "demo-1")
	waitPathAppears(t, marker, 60*time.Second, pB.output)
	requireWebStatus(t, webArchive(t, addrB, sessionID), http.StatusConflict, "SESSION_BUSY",
		sessionBusyMessage, "在途会话归档")

	if err := os.WriteFile(release, []byte("release"), 0o644); err != nil {
		t.Fatalf("写放行文件失败: %v", err)
	}
	waitScheduleStatus(t, addrB, "demo-1", "completed", 60*time.Second)
	requireWebStatus(t, webArchive(t, addrB, sessionID), http.StatusOK, "OK", "", "运行结束后归档")

	pB.waitOutput(t, archivedSkipMessage, 60*time.Second)
	requireWebStatus(t, runScheduleNow(t, addrB, "demo-1"), http.StatusBadRequest, "BAD_REQUEST",
		sessionArchivedMessage, "归档后手动补跑")
	pB.stop(t)

	sessions = queryRows(t, db, `SELECT status, archived_at FROM sessions WHERE session_id = ?`, sessionID)
	if len(sessions) != 1 {
		t.Fatalf("会话应仍在库中（归档不物理删除），实际 %d 行：%v", len(sessions), sessions)
	}
	if got := stringValue(t, sessions[0], "status"); got != "archived" {
		t.Fatalf("sessions.status = %q，期望 archived", got)
	}
	if sessions[0]["archived_at"] == nil {
		t.Fatal("sessions.archived_at 为空：首次归档必须写时间")
	}

	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 2 {
		t.Fatalf("runs 应恰有 2 行（阶段 A 与阶段 B 各一次），实际 %d 行：归档跳过不得产生 Run", n)
	}
	executions := queryRows(t, db,
		`SELECT trigger, status, run_id, error_message FROM task_executions ORDER BY id`)
	if len(executions) != 4 {
		t.Fatalf("task_executions 应有 4 行（2 完成 + cron 跳过 + 手动跳过），实际 %d 行：%v",
			len(executions), executions)
	}
	for i, want := range []string{"manual", "manual", "cron", "manual"} {
		if got := stringValue(t, executions[i], "trigger"); got != want {
			t.Fatalf("task_executions[%d].trigger = %q，期望 %q", i, got, want)
		}
	}
	for i := 0; i < 2; i++ {
		if got := stringValue(t, executions[i], "status"); got != "completed" {
			t.Fatalf("task_executions[%d].status = %q，期望 completed", i, got)
		}
		if executions[i]["run_id"] == nil {
			t.Fatalf("task_executions[%d].run_id 为空：完成的运行必须对齐 Run 账本", i)
		}
	}
	for i := 2; i < 4; i++ {
		if got := stringValue(t, executions[i], "status"); got != "skipped" {
			t.Fatalf("task_executions[%d].status = %q，期望 skipped", i, got)
		}
		if executions[i]["run_id"] != nil {
			t.Fatalf("task_executions[%d].run_id = %v，期望 NULL（未产生 Run）", i, executions[i]["run_id"])
		}
		if got := stringValue(t, executions[i], "error_message"); got != archivedSkipMessage {
			t.Fatalf("task_executions[%d].error_message = %q，期望 %q", i, got, archivedSkipMessage)
		}
	}

	tasks := queryRows(t, db, `SELECT last_status FROM scheduled_tasks WHERE task_id = 'demo-1'`)
	if len(tasks) != 1 {
		t.Fatalf("scheduled_tasks 应有 demo-1 一行，实际 %d 行：%v", len(tasks), tasks)
	}
	if got := stringValue(t, tasks[0], "last_status"); got != "skipped" {
		t.Fatalf("scheduled_tasks.last_status = %q，期望 skipped（最后一轮为手动跳过）", got)
	}
}

// testSchedulerCrossAgentTaskID：跨 Agent 复用任务 id 被启动期归属守卫拒绝（US-10 §6.2
// 第 14 项）。live 冲突在装载期就整只拒绝，所以构造路径是**已退休**行：A 先声明 shared、
// 再撤掉（退休），B 随后声明同一 id——装载通过，启动对账撞库内归属，进程以 1 退出。
// testSchedulerCrossAgentTaskID reaches the DB ownership guard through a retired row and
// requires the whole process to fail closed with exit code 1.
func testSchedulerCrossAgentTaskID(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t, textResponse("预热完成"))
	sharedCron := farFutureCron()
	keepCron := farFutureCron()
	addrA := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addrA))
	h.writeAgent(t, "demo-a",
		agentFrontmatter("demo-a", nil)+
			"schedules:\n  - id: shared\n    cron: \""+sharedCron+"\"\n    message: 共享任务\n"+
			"  - id: keep\n    cron: \""+keepCron+"\"\n    message: 保留任务\n",
		"跨 Agent 任务 id 演示")

	// 阶段 A：跑一次 shared 并停用，留下可核对的 last_* 基线。
	pA := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, pA, addrA, 30*time.Second)
	requireWebStatus(t, runScheduleNow(t, addrA, "shared"), http.StatusOK, "OK", "", "预热补跑")
	setScheduleEnabled(t, addrA, "shared", false)
	pA.stop(t)

	db := openDB(t, h.dbPath())
	before := queryRows(t, db,
		`SELECT agent_name, enabled, last_run_at, last_status, retired_at FROM scheduled_tasks WHERE task_id = 'shared'`)
	if len(before) != 1 {
		t.Fatalf("scheduled_tasks 应有 shared 一行，实际 %d 行：%v", len(before), before)
	}
	if got := intValue(t, before[0], "enabled"); got != 0 {
		t.Fatalf("shared.enabled = %d，期望 0（已停用）", got)
	}
	runAt := intValue(t, before[0], "last_run_at")
	execBefore := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions`)

	// 阶段 B：A 撤掉 shared → 该行退休（只标记，不物理删除）。
	h.writeAgent(t, "demo-a",
		agentFrontmatter("demo-a", nil)+
			"schedules:\n  - id: keep\n    cron: \""+keepCron+"\"\n    message: 保留任务\n",
		"跨 Agent 任务 id 演示")
	addrB := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addrB))
	pB := startGrouped(t, h, fixtureEnv(), "serve")
	waitHealthHTTP(t, pB, addrB, 30*time.Second)
	pB.stop(t)

	retired := queryRows(t, db,
		`SELECT agent_name, enabled, last_run_at, retired_at FROM scheduled_tasks WHERE task_id = 'shared'`)
	if len(retired) != 1 {
		t.Fatalf("退休行不得物理删除：scheduled_tasks 应有 shared 一行，实际 %d 行", len(retired))
	}
	if retired[0]["retired_at"] == nil {
		t.Fatal("shared.retired_at 为空：文件里已删除的任务必须被标记退休")
	}
	if got := stringValue(t, retired[0], "agent_name"); got != "demo-a" {
		t.Fatalf("退休后归属 = %q，期望仍绑定 demo-a", got)
	}
	if got := intValue(t, retired[0], "enabled"); got != 0 {
		t.Fatalf("退休不得翻转 enabled：= %d，期望 0", got)
	}
	if got := intValue(t, retired[0], "last_run_at"); got != runAt {
		t.Fatalf("退休不得改 last_run_at：= %d，期望 %d", got, runAt)
	}

	// 阶段 C：B 声明同一 id → 装载通过，启动对账撞归属，整进程 fail closed。
	h.writeAgent(t, "demo-b",
		agentFrontmatter("demo-b", nil)+
			"schedules:\n  - id: shared\n    cron: \""+sharedCron+"\"\n    message: 接管尝试\n",
		"跨 Agent 任务 id 演示")
	bFile := agentFile(t, h, "demo-b")
	addrC := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addrC))
	pC := startGrouped(t, h, fixtureEnv(), "serve")
	if code := pC.waitExit(t, 30*time.Second); code != 1 {
		t.Fatalf("跨 Agent 任务 id 必须让进程以 1 退出，实际退出码 %d\n%s", code, pC.combined())
	}
	output := pC.combined()
	requireContains(t, output, "调度器启动失败", "启动失败日志")
	requireContains(t, output, "定时任务归属冲突", "归属冲突日志")
	requireContains(t, output, "task_id=shared", "冲突日志点名任务")
	requireContains(t, output, "agent=demo-b", "冲突日志点名接管者")
	if got, want := string(agentFile(t, h, "demo-b")), string(bFile); got != want {
		t.Fatalf("启动失败不得改写 B 的 AGENT.md：\n--- 之前 ---\n%s\n--- 之后 ---\n%s", want, got)
	}

	after := queryRows(t, db,
		`SELECT agent_name, enabled, last_run_at FROM scheduled_tasks WHERE task_id = 'shared'`)
	if len(after) != 1 {
		t.Fatalf("scheduled_tasks 应有 shared 一行，实际 %d 行：%v", len(after), after)
	}
	if got := stringValue(t, after[0], "agent_name"); got != "demo-a" {
		t.Fatalf("启动失败后归属 = %q，期望仍为 demo-a", got)
	}
	if got := intValue(t, after[0], "enabled"); got != 0 {
		t.Fatalf("启动失败后 enabled = %d，期望保持 0", got)
	}
	if got := intValue(t, after[0], "last_run_at"); got != runAt {
		t.Fatalf("启动失败后 last_run_at = %d，期望保持 %d", got, runAt)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM task_executions`); n != execBefore {
		t.Fatalf("启动失败不得产生执行行：= %d，期望 %d", n, execBefore)
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM scheduled_tasks WHERE agent_name = 'demo-b'`); n != 0 {
		t.Fatalf("demo-b 不应写入任何任务，实际 %d 条", n)
	}
}

// scheduleBlock 组装一段只含单条任务的 schedules 块（id 省略 → 派生 <agent>-1）；
// zone 为空串表示不写该字段（缺省 UTC）。
// scheduleBlock assembles one schedule entry; an empty zone omits the field (UTC default).
func scheduleBlock(cron, zone, message string) string {
	out := "schedules:\n  - cron: \"" + cron + "\"\n"
	if zone != "" {
		out += "    zone: " + zone + "\n"
	}
	return out + "    message: " + message + "\n"
}

// cronAt 把触发时刻转成六段 cron（秒 分 时 日 月 周）：日 / 月 / 周取通配，每天同刻一次。
// cronAt renders a fire time as a six-field cron expression with wildcard date fields.
func cronAt(at time.Time) string {
	return fmt.Sprintf("%d %d %d * * *", at.Second(), at.Minute(), at.Hour())
}

// farFutureCron 返回一个至少五小时之后才会触发的 cron（当前 UTC 小时 + 6）。
// farFutureCron returns a cron that cannot fire within five hours.
func farFutureCron() string {
	hour := (time.Now().UTC().Hour() + 6) % 24
	return fmt.Sprintf("0 0 %d * * *", hour)
}

// executionEntry 是 `GET /schedules/{id}/executions` 元素的判卷读数。
// executionEntry is the verdict-side reading of one execution-history element.
type executionEntry struct {
	TaskID       string `json:"task_id"`
	RunID        string `json:"run_id"`
	Status       string `json:"status"`
	Trigger      string `json:"trigger"`
	DurationMS   int64  `json:"duration_ms"`
	ErrorMessage string `json:"error_message"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
}

// listExecutions 读一次执行历史（`data` 数组，仓储已按 started_at DESC、id DESC 排序）。
// listExecutions reads the execution history once; the store already orders it.
func listExecutions(t *testing.T, addr, id string) []executionEntry {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/api/v1/schedules/"+id+"/executions", nil)
	if err != nil {
		t.Fatalf("组装 GET /schedules/%s/executions 请求失败: %v", id, err)
	}
	resp, err := client.Do(applyAdminAuth(req))
	if err != nil {
		t.Fatalf("GET /schedules/%s/executions 失败: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /schedules/%s/executions 状态 = %d，期望 200", id, resp.StatusCode)
	}
	var envelope struct {
		Data []executionEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("解析执行历史响应失败: %v", err)
	}
	return envelope.Data
}

// runScheduleNow 发一次手动补跑并返回原始响应（状态与文案由调用方断言）。
// runScheduleNow triggers one manual run and returns the raw response.
func runScheduleNow(t *testing.T, addr, id string) *webResponse {
	t.Helper()
	return webRequest(t, addr, http.MethodPost, "/api/v1/schedules/"+id+"/run", "", nil)
}

// waitScheduleStatus 轮询管理读面直到任务的 last_status 等于期望值；超时附最后一次读数。
// waitScheduleStatus polls the management face until the task's last_status matches.
func waitScheduleStatus(t *testing.T, addr, id, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := "<任务不在列表>"
	for time.Now().Before(deadline) {
		for _, schedule := range listSchedules(t, addr) {
			if schedule.ID == id {
				last = schedule.LastStatus
				if last == want {
					return
				}
			}
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待任务 %s 的 last_status = %q 超时（%s），最后一次读数 %q", id, want, timeout, last)
}

// agentFile 读回一只 Agent 的 AGENT.md 原始字节（判「零写入」用字节相等）。
// agentFile reads the raw AGENT.md bytes for byte-equality assertions.
func agentFile(t *testing.T, h *harness, name string) []byte {
	t.Helper()
	path := filepath.Join(h.Root, ".basalt", "agents", name, "AGENT.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 AGENT.md %s 失败: %v", name, err)
	}
	return data
}

// stringValue 读一行里的文本列（SQLite TEXT 经驱动为 string）。
// stringValue reads a text column from one row.
func stringValue(t *testing.T, row map[string]any, key string) string {
	t.Helper()
	value, ok := row[key].(string)
	if !ok {
		t.Fatalf("%s 列不是文本：%#v", key, row[key])
	}
	return value
}

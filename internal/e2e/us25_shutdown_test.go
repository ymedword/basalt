//go:build e2e

package e2e

// US-25 容器验收 · 停机顺序逐段断言（FR-25-23）。
//
// 现场：控制面在挂起监听器上一个在途调度执行（调用已发出、永不返回），随后向容器投递
// 真实 SIGTERM。停机编排出场后按图 11 分段落子，每一段都有**外部可观测**的读数：
//
//	① 关监听：宿主侧健康检查从可达变为拒连，**而容器仍是 running**——先关接入再排空；
//	② 停止领取：排空期间把任务的 next_fire_at 推到近处，不得再产生第二条执行行；
//	③ drain 期间继续续约：租约的 renewed_at 晚于信号时刻 2s 以上，且 released_at 仍为空
//	   ——排空窗口里 keeper 没有停，租约不会因停机而被他人接管；
//	④ 到点先取消：执行行收尾 failed（带原因），未确认的模型调用落 failed/timeout；
//	⑤ 释放租约是属主自己的收尾：代次**仍是 1**、released_at 已写、租约事件里没有
//	   reclaimed——编排层的释放是 no-op，没有第二条释放路径；
//	⑥ Worker 下线：worker_nodes.stopped_at 已写；进程退出码 0，全程落在
//	   worker.shutdown_grace（45s 缺省）与 compose 的 stop_grace_period（90s）之内；
//	⑦ 最后关库：进程退出后 PostgreSQL 仍在运行且可查询——它关的是自己的连接与实例锁。
//
// 反向断言同样重要：排空到点日志必须出现（说明取消由编排层在 flows.shutdown_grace
// 触发，而不是调度器自己的兜底宽限），三条越界 / 预算耗尽告警必须**不**出现。
//
// 逆改点（改坏这里就会红）：
//   - 让 SIGTERM 直接退出进程（跳过排空）：① 的 running 读数与 ③ 的续约都等不到；
//   - 让停机在排空结束前就取消在途执行：③ 的 renewed_at 上界与「执行行仍是 started」判红；
//   - 让领取在排空期间继续：② 出现第二条执行行；
//   - 让编排层越权代释放租约（或改为接管回收）：⑤ 的 epoch=1 / 无 reclaimed 事件判红；
//   - 让取消结果不落库：④ 的 failed / timeout 读数等不到；
//   - 让停机超出 worker.shutdown_grace 才退出：⑥ 的耗时上界与越界日志反向断言同时红。
//
// A real SIGTERM is delivered to the control container while a scheduled execution is
// parked on a hanging model call. Each figure-11 phase is asserted from the outside:
// listener closed while the process drains, renewals continuing during the drain, no new
// claims, cancellation landing as failed/timeout, the owner (epoch 1) releasing its own
// lease with no reclaim event, the worker row stopped, exit code 0 inside the grace
// budgets, and PostgreSQL still serving queries after the process closed its store.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// us25ShutdownDrainLower / us25ShutdownElapsedUpper 把停机耗时夹在排空宽限（35s）与
// compose 的 stop_grace_period（90s）之间：下界证明确实排空过，上界证明没有撞硬上界。
// The bounds pin the drain window (35s) below the compose stop_grace_period (90s).
const (
	us25ShutdownDrainLower   = 33 * time.Second
	us25ShutdownElapsedUpper = 80 * time.Second
)

// TestUS25_Shutdown_ReleasesBeforeClose 覆盖 FR-25-23 的停机段序。
// TestUS25_Shutdown_ReleasesBeforeClose covers the shutdown ordering of FR-25-23.
func TestUS25_Shutdown_ReleasesBeforeClose(t *testing.T) {
	s := us25NewStack(t, "shutdown")
	s.addProbeEnvOverlay(t)
	s.addHangService(t)
	s.prepareControlForProbe(t, us25HangBaseURL)

	s.startControl(t)
	s.installProbeAssets(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("安装探针资产后重启控制面失败：%v", err)
	}
	s.enableProbeAgent(t)
	s.waitProbeTaskRow(t, 2*time.Minute)

	holder := s.waitLiveWorkerIDs(t, 1, 2*time.Minute)[0]

	// 正对照：信号之前监听确实可达——否则「先关监听」无从判定。
	hostAddr := "127.0.0.1:" + strconv.Itoa(s.port)
	if raw, err := webHealthStatus(hostAddr); err != nil {
		t.Fatalf("停机前健康检查不可达（%s）：%v", hostAddr, err)
	} else if strings.TrimSpace(raw) == "" {
		t.Fatalf("停机前健康检查返回空正文（%s）", hostAddr)
	}

	// ── 在途执行：调用已发出、结果不可知 ─────────────────────────────────────
	s.armProbe(t, us25ProbeArmDelay)
	row := s.waitProbeExecution(t, 1, 90*time.Second)
	if row[3] != holder {
		t.Fatalf("在途执行行的 owner 是 %q，要求控制面进程 %q", row[3], holder)
	}
	executionID := row[0]
	runID := s.probeRunID(t, executionID)
	s.waitForQuery(t, 2*time.Minute, "执行已进入模型调用（挂起监听器）",
		"SELECT (SELECT status FROM task_executions WHERE id = "+executionID+") || ':' || "+
			"(SELECT count(*) FROM llm_calls WHERE run_id = '"+runID+"' AND status = 'pending')::text",
		func(v string) bool { return v == "started:1" })
	callsBefore := s.psql(t, "SELECT count(*)::text FROM llm_calls WHERE run_id = '"+runID+"'")

	// ── ① 真实 SIGTERM：监听先关，进程仍在排空 ───────────────────────────────
	containerID, err := s.serviceContainerID(ctx, us25ControlService)
	if err != nil {
		t.Fatalf("取控制面容器失败：%v", err)
	}
	signalMS := s.dbNowMillis(t)
	signalAt := time.Now()
	if out, err := us18DockerOutput(ctx, "kill", "--signal=SIGTERM", containerID); err != nil {
		t.Fatalf("向控制面投递 SIGTERM 失败：%v（%s）", err, strings.TrimSpace(out))
	}

	closed := false
	closeDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(closeDeadline) {
		if _, err := webHealthStatus(hostAddr); err != nil {
			closed = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !closed {
		t.Fatalf("SIGTERM 后 %s 的监听没有关闭：停机必须先从接入面摘除", hostAddr)
	}
	if state := s.serviceState(ctx, us25ControlService); state != "running" {
		t.Fatalf("监听关闭时容器状态是 %q，要求 running（先关监听、再排空在途执行）", state)
	}

	// ── ③ drain 期间继续续约（信号之后仍有一次续约，且租约没被释放）───────────
	renewedFresh := strconv.FormatInt(signalMS+2000, 10)
	s.waitForQuery(t, 30*time.Second, "排空期间继续续约",
		"SELECT (renewed_at > "+renewedFresh+")::text || ':' || (released_at IS NULL)::text "+
			"FROM execution_leases WHERE kind = 'schedule_execution' AND resource_id = '"+executionID+"'",
		func(v string) bool { return v == "true:true" })

	// 排空窗口内不许提前取消：执行行仍是 started（④ 的取消要到 drain 到点才发生）。
	if got := s.psql(t, "SELECT status FROM task_executions WHERE id = "+executionID); got != "started" {
		t.Fatalf("排空期间执行行状态是 %q，要求在途运行在宽限结束前不被提前取消", got)
	}

	// ── ② 停止领取：排空期间上弦的新触发不得被执行 ───────────────────────────
	s.armProbe(t, us25ProbeArmDelay)
	time.Sleep(6 * time.Second)
	if got := s.psql(t, "SELECT count(*)::text FROM task_executions WHERE task_id = '"+us25ProbeTaskID+"'"); got != "1" {
		t.Fatalf("排空期间产生了新的执行行（共 %s 条），要求 [2] 已停止领取", got)
	}

	// ── ④⑤⑥ 到点取消、属主释放、进程按 0 退出 ────────────────────────────────
	if code := s.awaitExit(t, us25ControlService, us25ExitTimeout); code != 0 {
		logs, _ := s.logs(ctx, us25ControlService)
		t.Fatalf("控制面停机退出码 %d（要求 0），日志尾部：\n%s", code, us18Tail(logs, 6000))
	}
	elapsed := time.Since(signalAt)
	if elapsed < us25ShutdownDrainLower {
		t.Fatalf("停机在 SIGTERM 后 %s 就结束：早于排空宽限（下界 %s）", elapsed, us25ShutdownDrainLower)
	}
	if elapsed > us25ShutdownElapsedUpper {
		t.Fatalf("停机耗时 %s 超过上界 %s（compose stop_grace_period 是 90s）", elapsed, us25ShutdownElapsedUpper)
	}

	logs, err := s.logs(ctx, us25ControlService)
	if err != nil {
		t.Fatalf("读取控制面日志失败：%v", err)
	}
	// 排空到点由编排层取消（不是调度器的兜底宽限），随后走完释放并打成功标记。
	requireContains(t, logs, "停机排空超过 flows.shutdown_grace_seconds", "排空到点的取消时机")
	requireContains(t, logs, "msg=停机完成", "停机成功标记")
	// 三条「没走完」的旁路都不许出现：撞硬上界、释放预算耗尽、调度器兜底超时。
	requireAbsent(t, logs, "停机超出 worker.shutdown_grace_seconds", "撞 worker 硬上界的退出分支")
	requireAbsent(t, logs, "释放租约的停机预算已耗尽", "释放阶段的预算耗尽旁路")
	requireAbsent(t, logs, "调度器停机超时", "调度器兜底宽限（取消必须由编排层显式触发）")

	// 执行行以 failed 收尾并带原因与收尾时间（取消不是完成）。
	exec := s.psql(t, "SELECT status || ':' || (finished_at IS NOT NULL)::text || ':' || "+
		"COALESCE(error_message <> '', false)::text FROM task_executions WHERE id = "+executionID)
	if exec != "failed:true:true" {
		t.Fatalf("执行行收尾读数 status:finished:reason = %q，要求 failed:true:true", exec)
	}
	// Run 账本行由 AgentService 用被取消的 ctx 收尾，写失败属既有纪律（留 running 待
	// 恢复扫描）；与单实例信号用例同口径，接受 cancelled 与 running 两态。
	runRow := s.psql(t, "SELECT status || ':' || COALESCE(reason, '-') FROM runs WHERE run_id = '"+runID+"'")
	switch {
	case strings.HasPrefix(runRow, "cancelled:"), strings.HasPrefix(runRow, "running:"):
	default:
		t.Fatalf("Run 收尾读数是 %q，要求 cancelled（已落库）或 running（待恢复扫描收尾）", runRow)
	}
	// 未确认的模型调用落终态：不留在 pending、不重发，且判定为超时类失败。
	calls := s.psql(t, "SELECT (count(*) FILTER (WHERE status = 'pending'))::text || ':' || "+
		"(count(*) FILTER (WHERE status = 'failed' AND error_kind = 'timeout'))::text "+
		"FROM llm_calls WHERE run_id = '"+runID+"'")
	if calls != "0:1" {
		t.Fatalf("模型调用读数 pending:failed_timeout = %q，要求 0:1（取消落终态、不重发）", calls)
	}
	// 调用总数没变：停机不产生第二次模型调用。
	if n := s.psql(t, "SELECT count(*)::text FROM llm_calls WHERE run_id = '"+runID+"'"); n != callsBefore {
		t.Fatalf("停机改变了模型调用行数（停机前 %s，停机后 %s），要求相同", callsBefore, n)
	}

	// ⑤ 释放是属主自己的收尾：代次仍是 1、已释放、没有 reclaimed 事件——
	// 编排层的释放是 no-op，回收器也没有接手（否则代次变 2）。
	lease := s.probeLease(t, executionID)
	if lease[0] != holder || lease[1] != "1" || lease[2] == "-" {
		t.Fatalf("停机后的租约读数是 owner/epoch/released=%v，要求 %s/1/已释放", lease, holder)
	}
	for _, event := range s.probeLeaseEvents(t, executionID) {
		if strings.HasPrefix(event, "reclaimed|") {
			t.Fatalf("停机收尾里出现了接管事件 %q：租约必须由属主自己收尾释放", event)
		}
	}

	// ⑥ Worker 下线标记与无残行。
	if got := s.psql(t, "SELECT (stopped_at IS NOT NULL)::text FROM worker_nodes WHERE instance_id = '"+holder+"'"); got != "true" {
		t.Fatalf("worker_nodes.stopped_at 读数 = %q，要求 true（[7] Worker 下线）", got)
	}
	if got := s.psql(t, "SELECT count(*)::text FROM task_executions WHERE status = 'started'"); got != "0" {
		t.Fatalf("停机后仍有 %s 行 started 执行行：终态必须落库", got)
	}
	if got := s.psql(t, "SELECT last_status FROM scheduled_tasks WHERE task_id = '"+us25ProbeTaskID+"'"); got != "failed" {
		t.Fatalf("scheduled_tasks.last_status = %q，期望 failed（取消不是完成）", got)
	}

	// ⑦ 最后关库：进程关的是自己的连接与实例锁，数据库本身仍在服务。
	if got := s.psql(t, "SELECT 1"); got != "1" {
		t.Fatalf("进程退出后数据库不可查询（读数 %q）：停机不得停掉 PostgreSQL", got)
	}
	if state := s.serviceState(ctx, us25PostgresService); state != "running" {
		t.Fatalf("PostgreSQL 容器状态是 %q，要求 running（停机最后一步只关进程侧连接）", state)
	}
	t.Logf("停机段序：监听先关 → 排空 %s 内继续续约 → 到点取消 → 属主释放（epoch 1）→ Worker 下线 → 退出 0",
		elapsed.Round(time.Second))
}

// dbNowMillis 读数据库时钟的 unix 毫秒：停机预算与租约时间戳都以数据库时钟为权威，
// 用同一口径取基线，避免拿进程时钟去比数据库写的值。
// dbNowMillis reads the database clock in unix millis, matching the authority used by the
// lease timestamps.
func (s *us25Stack) dbNowMillis(t *testing.T) int64 {
	t.Helper()
	raw := strings.TrimSpace(s.psql(t, "SELECT (extract(epoch from clock_timestamp())*1000)::bigint::text"))
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("解析数据库时钟读数 %q 失败：%v", raw, err)
	}
	return ms
}

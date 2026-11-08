//go:build e2e

package e2e

// US-25 容器验收 · 接管只收尾、不重放（FR-25-20）。
//
// 判据拆成两段，两段都必须由**真实进程**产生：
//
//	① 旧 execution 的接管只收尾——控制面在模型调用中途被 `docker pause` 冻结（调用已
//	   发出、结果不可知），Worker 随即上线，回收器认领过期租约：执行行收尾 failed、Run 收尾
//	   failed/worker_lost、未确认的调用预写改判 unknown、租约 owner 换成 Worker 且代次 +1。
//	   **零重放**：runs / messages / llm_calls / tool_invocations 四组行数与冻结前逐项相同，
//	   `(task_id, planned_at)` 仍是那一行；解冻后旧 holder 的续约以「租约失权」告终，它
//	   再写终态被 fencing 拒绝（日志「执行行收尾失败」），而不是覆盖新属主的结果。
//	② 后续新的 planned_at 由在运行 Worker 正常领取执行；且 Worker 不推进 Flow 根
//	   （`flow_tasks` 仍为空）。
//
// 时限是有界的：每段都写明截止，截止内没完成就判红——产品不承诺接管 SLA，但验收必须
// 能在有限时间内给出结论。
//
// 逆改点（改坏这里就会红）：
//   - 让回收器在接管时重跑 Run（重新发模型调用）：零重放指纹与「新 Run 数」断言同时红；
//   - 让 fencing 不生效（FinishExecution 不校验 owner/epoch）：解冻后旧 holder 会把执行行
//     改写成自己的终态，租约 owner/epoch 与日志断言失效；
//   - 让回收器只认领不释放租约：`released_at IS NOT NULL` 等不到；
//   - 让 Worker 也推进 Flow 根（或给探针挂 Flow）：`flow_tasks` 计数不再是 0。
//
// The control plane is frozen mid-call, a worker takes over the expired lease and only
// closes out: the audit row counts must stay identical, and the thawed former holder must
// be fenced. A later planned_at is then claimed by the live worker, which never advances
// Flow roots.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestUS25_Takeover_FencesOldHolder 覆盖 FR-25-20 的两段。
// TestUS25_Takeover_FencesOldHolder covers both halves of FR-25-20.
func TestUS25_Takeover_FencesOldHolder(t *testing.T) {
	s := us25NewStack(t, "takeover")
	s.addProbeEnvOverlay(t)
	s.addHangService(t)
	s.prepareControlForProbe(t, us25HangBaseURL)

	s.startControl(t)
	s.enableProbeAgent(t)
	s.waitProbeTaskRow(t, 2*time.Minute)

	// 显式安装探针资产（首启不自动发布；安装要趁停机，返回时控制面已停）。
	// 它必须发生在导出种子之前：种子归档要带上 .basalt/assets，Worker 才能准入同一次
	// 执行；也必须在启用之后——bundle 里要含启用后的定义。
	s.installProbeAssets(t)

	// 先把 Worker 的种子与配置准备好（导出要求控制面停机，正好是冷备前提），但**不启动**。
	// 之后重启控制面：这时它在 worker_nodes 里是一个新的进程身份，接管前后的 owner 才可比。
	_, seedPath := s.exportControlAssets(t)
	workerVolume := s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)
	s.prepareWorkerForProbe(t, workerVolume, us25HangBaseURL)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("重启控制面失败：%v", err)
	}
	holderID := s.waitLiveWorkerIDs(t, 1, 3*time.Minute)[0]

	// ── 段 ①：冻结在模型调用中途 ─────────────────────────────────────────────
	s.armProbe(t, us25ProbeArmDelay)
	row := s.waitProbeExecution(t, 1, 90*time.Second)
	if row[3] != holderID {
		t.Fatalf("冻结前的执行行 owner 是 %q，要求控制面进程 %q", row[3], holderID)
	}
	executionID, plannedAt := row[0], row[1]
	runID := s.probeRunID(t, executionID)
	s.waitForQuery(t, 2*time.Minute, "执行已进入模型调用（挂起监听器）",
		"SELECT (SELECT status FROM task_executions WHERE id = "+executionID+") || ':' || "+
			"(SELECT count(*) FROM llm_calls WHERE run_id = '"+runID+"' AND status = 'pending')::text",
		func(v string) bool { return v == "started:1" })
	before := s.probeReplaySnapshot(t, runID)
	t.Logf("冻结前：执行行 %s（planned_at=%s）Run %s 指纹 %s", executionID, plannedAt, runID, before)

	containerID, err := s.serviceContainerID(ctx, us25ControlService)
	if err != nil {
		t.Fatalf("取控制面容器失败：%v", err)
	}
	if out, err := us18DockerOutput(ctx, "pause", containerID); err != nil {
		t.Fatalf("暂停控制面容器失败：%v（%s）", err, strings.TrimSpace(out))
	}
	// 无论后续断言怎么失败，容器都必须解冻——否则清理阶段的 `compose down` 会卡住。
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanCancel()
		_, _ = us18DockerOutput(cleanCtx, "unpause", containerID)
	})

	if err := s.up(ctx, "worker"); err != nil {
		t.Fatalf("启动 Worker 失败：%v", err)
	}
	// 暂停中的容器**不写** stopped_at，所以「在线集合只剩一个」不能作为判据；
	// 这里按「不是旧 holder」定位新上线的 Worker 进程身份。
	workerID := s.waitNewLiveWorker(t, holderID, 5*time.Minute)
	if workerID == holderID {
		t.Fatalf("接管的 Worker 身份与旧 holder 相同（%q），用例没有构造出跨进程接管", workerID)
	}

	// 接管只收尾：执行行、Run、未确认调用、租约四条读数同时到位。
	s.waitForQuery(t, 5*time.Minute, "回收器收尾执行行",
		"SELECT status FROM task_executions WHERE id = "+executionID,
		func(v string) bool { return v == "failed" })
	s.waitForQuery(t, 5*time.Minute, "旧 Run 收尾为 worker_lost",
		"SELECT status || ':' || COALESCE(reason, '-') FROM runs WHERE run_id = '"+runID+"'",
		func(v string) bool { return v == "failed:worker_lost" })
	s.waitForQuery(t, 5*time.Minute, "未确认的模型调用改判 unknown",
		"SELECT (SELECT count(*) FROM llm_calls WHERE run_id = '"+runID+"' AND status = 'pending') || ':' || "+
			"(SELECT count(*) FROM llm_calls WHERE run_id = '"+runID+"' AND status = 'unknown')",
		func(v string) bool { return v == "0:1" })
	s.waitForQuery(t, 5*time.Minute, "租约由 Worker 接管并释放",
		"SELECT owner_worker_id || ':' || ownership_epoch || ':' || (released_at IS NOT NULL)::text "+
			"FROM execution_leases WHERE kind = 'schedule_execution' AND resource_id = '"+executionID+"'",
		func(v string) bool { return v == workerID+":2:true" })

	events := s.probeLeaseEvents(t, executionID)
	wantEvent := "reclaimed|" + workerID + "|2|" + holderID
	if !us25ContainsLine(events, wantEvent) {
		t.Fatalf("租约事件里没有 %q，实际：%v", wantEvent, events)
	}

	// 零重放：四组行数与冻结前逐项相同，且仍只有那一条 planned_at。
	after := s.probeReplaySnapshot(t, runID)
	if after != before {
		t.Fatalf("接管改动了审计行数（冻结前 %s，接管后 %s），要求逐项相同（零重放）", before, after)
	}
	s.assertSingleExecution(t, plannedAt)

	// 解冻：旧 holder 发现失权，停止发起动作，终态写入被 fencing 拒绝。
	if out, err := us18DockerOutput(ctx, "unpause", containerID); err != nil {
		t.Fatalf("解冻控制面容器失败：%v（%s）", err, strings.TrimSpace(out))
	}
	s.waitForLog(t, us25ControlService, "租约失权，停止发起新动作", 3*time.Minute)
	s.waitForLog(t, us25ControlService, "执行行收尾失败", 3*time.Minute)
	t.Logf("段 ①：Worker %s 接管执行行 %s，旧 holder %s 已被 fencing 拒绝", workerID, executionID, holderID)

	// ── 段 ②：新的 planned_at 由在运行 Worker 正常领取 ─────────────────────────
	// 先停挂起监听器再停控制面：新的模型调用会立刻拒连，收尾在时限内完成。
	if err := s.stopService(ctx, us25HangService); err != nil {
		t.Fatalf("停挂起监听器失败：%v", err)
	}
	if err := s.stopService(ctx, us25ControlService); err != nil {
		t.Fatalf("停控制面失败：%v", err)
	}
	if code := s.awaitExit(t, us25ControlService, us25ExitTimeout); code != 0 {
		logs, _ := s.logs(ctx, us25ControlService)
		t.Fatalf("控制面停机退出码 %d（要求 0），日志尾部：\n%s", code, us18Tail(logs, 4000))
	}
	only := s.waitLiveWorkerIDs(t, 1, 3*time.Minute)
	if only[0] != workerID {
		t.Fatalf("控制面停机后在线 Worker 是 %q，要求仍是接管的那一个 %q", only[0], workerID)
	}

	s.armProbe(t, us25ProbeArmDelay)
	next := s.waitProbeExecution(t, 2, 3*time.Minute)
	if next[3] != workerID {
		t.Fatalf("新的 planned_at 执行行 owner 是 %q，要求在运行 Worker %q", next[3], workerID)
	}
	if next[1] == plannedAt {
		t.Fatalf("第二次上弦与第一次落在同一 planned_at=%s，用例没有构造出新的触发", next[1])
	}
	s.assertSingleExecution(t, next[1])
	if status := s.waitExecutionStatus(t, next[0], 3*time.Minute); status != "failed" {
		t.Fatalf("新执行行终态是 %q，要求 failed（监听器已停，连接必然失败）", status)
	}
	// 「领到了」还不够，必须是**真正执行**：准入被拒的执行行没有 Run（run_id 为空），
	// 那只是领到之后拒之门外，不满足 FR-25-20 的「由在运行 Worker 正常领取执行」。
	// A claimed row is not enough: an admission rejection carries no run, so the claim must
	// produce a real run that reaches a terminal state on the worker.
	nextRun := s.probeRunID(t, next[0])
	s.waitForQuery(t, 3*time.Minute, "Worker 领取产生的 Run 收尾",
		"SELECT status FROM runs WHERE run_id = '"+nextRun+"'",
		func(v string) bool { return v != "running" })

	// Worker 只回收、不推进 Flow 根：本栈不是 Flow 部署（flows.enabled=false），接管路径
	// 也不得凭空写出 Flow 行——计数非 0 就说明有旁路写入。
	flowTasks := us25Int(t, s.psql(t, "SELECT count(*)::text FROM flow_tasks"))
	if flowTasks != 0 {
		t.Fatalf("flow_tasks 有 %d 行，要求 0（Worker 不推进 Flow 根）", flowTasks)
	}
	t.Logf("段 ②：新 planned_at 执行行 %s 由 Worker %s 领取并收尾", next[0], workerID)
}

// us25ContainsLine 判断一个已拆行的读数集合里是否有整行等于 want。
// us25ContainsLine reports whether one whole line equals want.
func us25ContainsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// us25Int 把 psql 的单值读数转成整数（非法值直接判用例失败）。
// us25Int parses one psql scalar.
func us25Int(t *testing.T, raw string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		t.Fatalf("解析整数读数 %q 失败：%v", raw, err)
	}
	return n
}

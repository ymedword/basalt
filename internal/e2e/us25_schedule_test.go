//go:build e2e

package e2e

// US-25 容器验收 · 定时执行只跑一次（FR-25-19）。
//
// 判据：同一 `(task_id, planned_at)` 只产生一条执行记录；被谁领取由**真实进程身份**
// 决定（`worker_nodes.instance_id`，每进程随机铸造）。用例把三种宿主组合依次跑一遍：
//
//	A. 只有控制面——`serve` 自身也是 Worker，必须能领取并执行；
//	B. 只有 Worker（控制面已停）——强制证明领取发生在 Worker 进程，而不是控制面；
//	C. 控制面 + Worker 同时在线——证明两个进程不会为同一触发各执行一次。
//
// 每次「上弦」把 `next_fire_at` 推到约 1.5s 之后，走的是**同一条**到期扫描 →
// `ClaimDueTrigger` → 执行行 + 租约的路径；不绕过任何判据。
//
// 逆改点（改坏这里就会红）：
//   - 去掉唯一索引 `ux_task_executions_cron_planned`：同一 planned_at 可能出现两行，
//     `assertSingleExecution` 判红；
//   - 让 Worker 不写 `worker_nodes`（或把 owner 写成本机名而非 InstanceID）：阶段 B 的
//     「owner ≠ 控制面那个 InstanceID」与阶段 C 的「owner ∈ 在线集合」同时失效；
//   - 让 `serve` 退出调度：阶段 A 在 90s 内等不到执行行；
//   - 让 Worker 直接跳过租约、只按数据库行执行：阶段 B/C 的 owner 会仍是控制面的旧值。
//
// The test arms the same due-scan claim path three times against three host shapes: the
// control plane alone, a worker alone, and both. Every claim must produce exactly one
// execution row, and the owner must be the process that actually claimed it.

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestUS25_Schedule_ExecutesOnceAcrossWorkers 覆盖 FR-25-19 的三段宿主组合。
// TestUS25_Schedule_ExecutesOnceAcrossWorkers covers the three host shapes of FR-25-19.
func TestUS25_Schedule_ExecutesOnceAcrossWorkers(t *testing.T) {
	s := us25NewStack(t, "schedule")
	s.addProbeEnvOverlay(t)
	s.prepareControlForProbe(t, us25ProbeRefusedURL)

	rosterID := s.startControl(t)
	t.Logf("控制面已启动：工作区名册 instance_id=%s", rosterID)

	// 首启基线只写生命周期身份，bundle 必须显式安装（且实例锁只在停机时可用）；
	// 装完把控制面起回来，阶段 A 才有进程可以领取。
	s.installProbeAssets(t)
	restartCtx, restartCancel := context.WithTimeout(context.Background(), 8*time.Minute)
	if err := s.up(restartCtx, "control"); err != nil {
		restartCancel()
		t.Fatalf("安装探针资产后重启控制面失败：%v", err)
	}
	restartCancel()

	s.enableProbeAgent(t)
	s.waitProbeTaskRow(t, 2*time.Minute)

	// ── 阶段 A：只有控制面 ───────────────────────────────────────────────────
	controlWorker := s.waitLiveWorkerIDs(t, 1, 2*time.Minute)[0]
	s.armProbe(t, us25ProbeArmDelay)
	first := s.waitProbeExecution(t, 1, 90*time.Second)
	if first[3] != controlWorker {
		t.Fatalf("阶段 A 的执行行 owner 是 %q，要求控制面进程 %q（serve 自身也是 Worker）", first[3], controlWorker)
	}
	s.assertSingleExecution(t, first[1])
	if status := s.waitExecutionStatus(t, first[0], 2*time.Minute); status != "failed" {
		t.Fatalf("阶段 A 执行行终态是 %q，要求 failed（探针地址 127.0.0.1:1 必然拒连）", status)
	}
	s.assertProbeRunsTerminal(t, 1)
	t.Logf("阶段 A：执行行 %s（planned_at=%s）由控制面 Worker %s 领取", first[0], first[1], controlWorker)

	// ── 阶段 B：只有 Worker ──────────────────────────────────────────────────
	// exportControlAssets 需要控制面**停机**（冷备前提），正好构造出「只有 Worker 在线」。
	_, seedPath := s.exportControlAssets(t)
	workerVolume := s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)
	s.prepareWorkerForProbe(t, workerVolume, us25ProbeRefusedURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.up(ctx, "worker"); err != nil {
		t.Fatalf("启动 Worker 失败：%v", err)
	}
	workerOnly := s.waitLiveWorkerIDs(t, 1, 3*time.Minute)
	if len(workerOnly) != 1 || workerOnly[0] == controlWorker {
		t.Fatalf("阶段 B 在线 Worker 是 %v，要求恰好一个且不同于控制面 %q", workerOnly, controlWorker)
	}
	workerID := workerOnly[0]

	s.armProbe(t, us25ProbeArmDelay)
	second := s.waitProbeExecution(t, 2, 3*time.Minute)
	if second[3] != workerID {
		t.Fatalf("阶段 B 的执行行 owner 是 %q，要求 Worker 进程 %q", second[3], workerID)
	}
	s.assertSingleExecution(t, second[1])
	if status := s.waitExecutionStatus(t, second[0], 3*time.Minute); status != "failed" {
		t.Fatalf("阶段 B 执行行终态是 %q，要求 failed（探针地址必然拒连）", status)
	}
	lease := s.probeLease(t, second[0])
	if lease[0] != workerID || lease[1] != "1" || lease[2] == "-" {
		t.Fatalf("阶段 B 租约读数是 owner/epoch/released=%v，要求 %s/1/已释放", lease, workerID)
	}
	s.assertProbeRunsTerminal(t, 2)
	t.Logf("阶段 B：执行行 %s（planned_at=%s）由 Worker %s 领取，控制面 %s 已停",
		second[0], second[1], workerID, controlWorker)

	// ── 阶段 C：控制面 + Worker 同时在线 ─────────────────────────────────────
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("重新启动控制面失败：%v", err)
	}
	both := s.waitLiveWorkerIDs(t, 2, 3*time.Minute)
	live := make(map[string]bool, len(both))
	for _, id := range both {
		live[id] = true
	}
	if !live[workerID] {
		t.Fatalf("阶段 C 在线集合 %v 里没有 Worker %q（Worker 应仍在运行）", both, workerID)
	}

	s.armProbe(t, us25ProbeArmDelay)
	third := s.waitProbeExecution(t, 3, 3*time.Minute)
	if !live[third[3]] {
		t.Fatalf("阶段 C 的执行行 owner 是 %q，不在在线 Worker 集合 %v 内", third[3], both)
	}
	s.assertSingleExecution(t, third[1])
	if status := s.waitExecutionStatus(t, third[0], 3*time.Minute); status != "failed" {
		t.Fatalf("阶段 C 执行行终态是 %q，要求 failed（探针地址必然拒连）", status)
	}
	s.assertProbeRunsTerminal(t, 3)
	t.Logf("阶段 C：执行行 %s（planned_at=%s）由 %s 领取，在线集合 %v",
		third[0], third[1], third[3], both)

	// 三次上弦落在三个不同的 planned_at 上：唯一索引管的是「同一个计划时刻只一行」，
	// 不是「一共只能有一行」——把这两件事混起来会让上面的 count 断言形同虚设。
	if first[1] == second[1] || second[1] == third[1] || first[1] == third[1] {
		t.Fatalf("三次上弦的 planned_at 应当两两不同，实际 %s / %s / %s", first[1], second[1], third[1])
	}
}

// assertProbeRunsTerminal 判探针 Agent 的 Run 数恰好 want 条且都离开 running。
// assertProbeRunsTerminal judges the probe agent's run count and terminality.
func (s *us25Stack) assertProbeRunsTerminal(t *testing.T, want int) {
	t.Helper()
	got := s.psql(t, "SELECT count(*) || ':' || COALESCE(bool_and(status <> 'running')::text, '-') "+
		"FROM runs WHERE agent = '"+us25ProbeAgent+"'")
	wantText := strconv.Itoa(want) + ":true"
	if got != wantText {
		t.Fatalf("探针 Run 计数读数是 %q，要求 %q", got, wantText)
	}
}

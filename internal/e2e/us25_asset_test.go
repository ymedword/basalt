//go:build e2e

package e2e

// US-25 容器验收 · 资产 bundle 缺失或哈希不符时准入被拒（FR-25-22 / C-11）。
//
// 判据：**只有 Worker 在线**时（控制面已停，准入必然发生在 Worker 进程），
//
//	① 正对照：种子带来的 bundle 完好 → 领取成功、执行跑起来并收尾（模型地址必然拒连）；
//	② 哈希不符：往 bundle 目录里每个文件追加一个字节 → 新的领取在执行行上收尾 failed，
//	   `error_message` 是资产准入原因，**不产生 Run、不产生模型调用**；
//	③ 缺失：删掉整个 bundle 目录 → 同样的拒绝；
//	④ 拒绝之后 bundle 目录仍不存在：Worker **不执行资产安装 / 发布**，只按共享发布记录
//	   校验本地副本（C-11 的物理边界）。
//
// 为什么必须只有 Worker 在线：控制面自己也有一份完好的 bundle，若同时在线，到期扫描
// 可能被控制面领走，用例就证不到"Worker 侧拒绝"。控制面停机由冷备导出顺带完成
// （`exportControlAssets` 要求停机，且会断言退出码 0）。
//
// 逆改点（改坏这里就会红）：
//   - 让准入不校验 bundle 摘要：第 ② 段仍会跑起来，Run 计数多一条；
//   - 让 Worker 在准入失败后自行 `assets install` 重建 bundle：第 ④ 段的目录存在性判红；
//   - 让执行行在准入失败时留在 started：终态断言等不到；
//   - 让拒绝路径写 Run / 模型调用：计数指纹变化。
//
// With only the worker live, admission is forced onto the worker process. A corrupted
// bundle and a missing bundle must both leave a failed execution row whose message is the
// asset-admission reason, with no run and no model call, and the worker must never rebuild
// the bundle it was seeded with.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestUS25_AssetMissing_RejectsNewRun 覆盖 FR-25-22 的两条拒绝路径。
// TestUS25_AssetMissing_RejectsNewRun covers both refusal paths of FR-25-22.
func TestUS25_AssetMissing_RejectsNewRun(t *testing.T) {
	s := us25NewStack(t, "asset")
	s.addProbeEnvOverlay(t)
	s.prepareControlForProbe(t, us25ProbeRefusedURL)

	s.startControl(t)
	s.installProbeAssets(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("安装探针资产后重启控制面失败：%v", err)
	}
	s.enableProbeAgent(t)
	s.waitProbeTaskRow(t, 2*time.Minute)

	// 冷备导出同时停掉控制面；种子把 bundle 带到 Worker 卷，Worker 配置另写（配置属
	// 各宿主自己的部署事实，不在种子归档里）。
	_, seedPath := s.exportControlAssets(t)
	workerVolume := s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)
	s.prepareWorkerForProbe(t, workerVolume, us25ProbeRefusedURL)
	if err := s.up(ctx, "worker"); err != nil {
		t.Fatalf("启动 Worker 失败：%v", err)
	}
	workerID := s.waitLiveWorkerIDs(t, 1, 3*time.Minute)[0]

	// volumeHas 自己把卷挂在 /data 上，传的是**卷内相对路径**；一次性容器里的脚本要的是
	// 绝对路径。两套写法都显式保留，避免把 /data 拼成 /data/data。
	// volumeHas mounts the volume at /data and takes a volume-relative path, while the
	// one-shot scripts need the absolute one; keep both explicit.
	bundleRel := "/.basalt/assets/bundles/" + us25ProbeAgent
	bundleDir := "/data" + bundleRel
	if !s.volumeHas(ctx, workerVolume, bundleRel) {
		t.Fatalf("种子没有把资产 bundle 带到 Worker（%s 不存在），后续判据无从谈起", bundleDir)
	}

	// ── ① 正对照：bundle 完好时准入通过 ─────────────────────────────────────
	s.armProbe(t, us25ProbeArmDelay)
	first := s.waitProbeExecution(t, 1, 90*time.Second)
	if first[3] != workerID {
		t.Fatalf("正对照执行行 owner 是 %q，要求 Worker 进程 %q", first[3], workerID)
	}
	if status := s.waitExecutionStatus(t, first[0], 2*time.Minute); status != "failed" {
		t.Fatalf("正对照执行行终态是 %q，要求 failed（模型地址 127.0.0.1:1 必然拒连）", status)
	}
	baseline := s.probeRunCallCounts(t)
	t.Logf("正对照：执行行 %s 在 Worker %s 上跑起来，Run/调用计数 %s", first[0], workerID, baseline)

	// ── ② 哈希不符：改一个字节就足以让摘要对不上 ────────────────────────────
	if out, err := s.execInVolume(ctx, workerVolume,
		"set -eu\n"+
			"for f in $(find "+bundleDir+" -type f); do printf x >> \"$f\"; done\n"+
			"find "+bundleDir+" -type f | wc -l"); err != nil {
		t.Fatalf("篡改 Worker 卷里的 bundle 失败：%v（%s）", err, us18Tail(out, 2000))
	}
	s.armProbe(t, us25ProbeArmDelay)
	second := s.waitProbeExecution(t, 2, 3*time.Minute)
	assertAssetRefusal(t, s, second, workerID)
	t.Logf("哈希不符：执行行 %s 在执行权上被拒（%s）", second[0], s.executionErrorMessage(t, second[0]))

	// ── ③ 缺失：整只目录删掉 ─────────────────────────────────────────────────
	if out, err := s.execInVolume(ctx, workerVolume, "set -eu\nrm -rf "+bundleDir+"\ntest ! -e "+bundleDir); err != nil {
		t.Fatalf("删除 Worker 卷里的 bundle 失败：%v（%s）", err, us18Tail(out, 2000))
	}
	s.armProbe(t, us25ProbeArmDelay)
	third := s.waitProbeExecution(t, 3, 3*time.Minute)
	assertAssetRefusal(t, s, third, workerID)
	t.Logf("缺失：执行行 %s 在执行权上被拒（%s）", third[0], s.executionErrorMessage(t, third[0]))

	// ── ④ 拒绝路径的共同结论 ─────────────────────────────────────────────────
	// Worker 不得在本地重建 bundle：目录仍不存在 = 它既没跑安装，也没跑发布。
	if s.volumeHas(ctx, workerVolume, bundleRel) {
		t.Fatalf("Worker 在准入失败后重建了 %s，要求它只校验、不安装 / 发布（C-11）", bundleDir)
	}
	// 两次拒绝都没有产生新的 Run 与模型调用：计数与正对照时逐项相同。
	if got := s.probeRunCallCounts(t); got != baseline {
		t.Fatalf("拒绝路径改变了 Run/调用计数（正对照 %s，拒绝后 %s），要求逐项相同", baseline, got)
	}
	if state := s.serviceState(ctx, us25WorkerService); state != "running" {
		t.Fatalf("Worker 容器状态是 %q，要求 running（拒绝是准入结果，不是进程退出）", state)
	}
}

// assertAssetRefusal 判一条执行行是「被资产准入拒绝」：failed、owner 是期望进程、
// error_message 命中资产准入的固定前缀，且 run_id 为空（准入失败不产生 Run）。
// assertAssetRefusal judges one execution row rejected by asset admission.
func assertAssetRefusal(t *testing.T, s *us25Stack, row []string, owner string) {
	t.Helper()
	if row[3] != owner {
		t.Fatalf("被拒执行行的 owner 是 %q，要求 Worker 进程 %q", row[3], owner)
	}
	if status := s.waitExecutionStatus(t, row[0], 90*time.Second); status != "failed" {
		t.Fatalf("被拒执行行的终态是 %q，要求 failed", status)
	}
	reason := s.executionErrorMessage(t, row[0])
	if !strings.Contains(reason, "resource unavailable: "+us25ProbeAgent) ||
		!strings.Contains(reason, "bundle") {
		t.Fatalf("被拒执行行的 error_message 是 %q，要求命中资产准入原因", reason)
	}
	if runID := s.psql(t, "SELECT COALESCE(run_id, '') FROM task_executions WHERE id = "+row[0]); runID != "" {
		t.Fatalf("被拒执行行 %s 关联了 Run %q，要求没有 Run（准入在 Run 之前失败）", row[0], runID)
	}
}

// executionErrorMessage 读执行行的 error_message（准入拒绝的固定文案落在这一列）。
// executionErrorMessage reads the execution row's error message.
func (s *us25Stack) executionErrorMessage(t *testing.T, executionID string) string {
	t.Helper()
	return s.psql(t, "SELECT COALESCE(error_message, '') FROM task_executions WHERE id = "+executionID)
}

// probeRunCallCounts 取探针 Agent 的 Run 数与模型调用数（`runs:llm_calls`）。
// probeRunCallCounts fingerprints the probe agent's run and call counts.
func (s *us25Stack) probeRunCallCounts(t *testing.T) string {
	t.Helper()
	return s.psql(t, "SELECT (SELECT count(*) FROM runs WHERE agent = '"+us25ProbeAgent+"') || ':' || "+
		"(SELECT count(*) FROM llm_calls WHERE run_id IN "+
		"(SELECT run_id FROM runs WHERE agent = '"+us25ProbeAgent+"'))")
}

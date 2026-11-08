//go:build e2e

package e2e

// US-25 容器验收 · 数据库短暂中断后不重放（FR-25-21）。
//
// 判据：调用已发出、结果不可知时把 PostgreSQL 停掉，控制面**不重启**；恢复后：
//
//	① 未确认的模型调用改判 `unknown`（待查），不是 pending、不是重发；
//	② 执行行收尾 failed、Run 收尾为失权类终态（`cancelled:renew_failed`：活着的进程在
//	   有界停止阈值上自己停下；或 `failed:worker_lost`：由恢复后的回收器认领收尾），
//	   两者与审计行一致；
//	③ 零重放：runs / messages / llm_calls / tool_invocations 四组行数与中断前逐项相同，
//	   `(task_id, planned_at)` 仍是那一行；
//	④ 租约仍归**同一个活着的进程**且已释放。两条路径都合法：自停路径由活进程交还自己
//	   的租约（owner 不变、代次不变，2026-11-08 实测代次 1）；接管路径由活进程里的回收器
//	   认领自己过期的租约（owner 不变、代次 +1）。若产品改成"重启后才恢复"，在线 Worker
//	   集合会多出一行，断言即红。
//
// 中断期间只读容器日志：夹具的 `psql` 是致命的（数据库不可达直接判红），所以这一段
// 不能轮询 SQL——这正好逼出"日志 + 恢复后读数"两段式判据，而不是在中断中反复试探。
//
// 逆改点（改坏这里就会红）：
//   - 让恢复扫描重发未确认调用 / 重跑 Run：`llm_calls` 出现第二行（pending 或终态），
//     零重放指纹与 unknown 计数同时红；
//   - 让 pending 行留在 pending（不做归一化）：`0:1` 读数永远等不到；
//   - 让收尾只写执行行、不写 Run：失权类终态等不到；
//   - 让中断导致进程退出并重建：恢复后在线 Worker 的 InstanceID 变了。
//
// The database is stopped while a model call is in flight. After restart the unconfirmed
// call must be normalized to unknown (not replayed), the execution and run must be closed
// out by the still-live process reclaiming its own expired lease, and the audit row counts
// must be identical to the pre-outage fingerprint.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestUS25_DatabaseOutage_NoReplay 覆盖 FR-25-21。
// TestUS25_DatabaseOutage_NoReplay covers FR-25-21.
func TestUS25_DatabaseOutage_NoReplay(t *testing.T) {
	s := us25NewStack(t, "outage")
	s.addProbeEnvOverlay(t)
	s.addHangService(t)
	s.prepareControlForProbe(t, us25HangBaseURL)

	s.startControl(t)
	s.installProbeAssets(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("安装探针资产后重启控制面失败：%v", err)
	}
	s.enableProbeAgent(t)
	s.waitProbeTaskRow(t, 2*time.Minute)

	// ── 让调用停在「已发出、结果不可知」的状态 ───────────────────────────────
	controlWorker := s.waitLiveWorkerIDs(t, 1, 2*time.Minute)[0]
	s.armProbe(t, us25ProbeArmDelay)
	row := s.waitProbeExecution(t, 1, 90*time.Second)
	if row[3] != controlWorker {
		t.Fatalf("中断前的执行行 owner 是 %q，要求控制面进程 %q", row[3], controlWorker)
	}
	executionID, plannedAt := row[0], row[1]
	runID := s.probeRunID(t, executionID)
	s.waitForQuery(t, 2*time.Minute, "执行已进入模型调用（挂起监听器）",
		"SELECT (SELECT status FROM task_executions WHERE id = "+executionID+") || ':' || "+
			"(SELECT count(*) FROM llm_calls WHERE run_id = '"+runID+"' AND status = 'pending')::text",
		func(v string) bool { return v == "started:1" })
	before := s.probeReplaySnapshot(t, runID)
	t.Logf("中断前：执行行 %s（planned_at=%s）Run %s 指纹 %s", executionID, plannedAt, runID, before)

	// ── 停数据库：控制面进程保持运行 ─────────────────────────────────────────
	pgID, err := s.serviceContainerID(ctx, us25PostgresService)
	if err != nil {
		t.Fatalf("取 PostgreSQL 容器失败：%v", err)
	}
	if out, err := us18DockerOutput(ctx, "stop", pgID); err != nil {
		t.Fatalf("停 PostgreSQL 失败：%v（%s）", err, strings.TrimSpace(out))
	}
	// 无论后续断言怎么失败，数据库都必须被起回来——否则清理阶段的 `compose down`
	// 与同一次运行里的后续用例都会被一个停着的数据库带红。
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanCancel()
		_, _ = us18DockerOutput(cleanCtx, "start", pgID)
	})

	// 续约失败累计到 `ttl - renew_interval`（30s - 10s）后有界停止：这条日志同时是
	// 「中断确实被产品观察到」的证据——不是用例单方面宣布数据库不可用。
	s.waitForLog(t, us25ControlService, "租约续约超过有界停止阈值，停止执行", 4*time.Minute)
	// 停止之后的终态回填打在不可达的数据库上：行留在 pending，等恢复扫描归一化。
	s.waitForLog(t, us25ControlService, "审计终态回填失败", 2*time.Minute)

	// ── 恢复数据库 ───────────────────────────────────────────────────────────
	if out, err := us18DockerOutput(ctx, "start", pgID); err != nil {
		t.Fatalf("启动 PostgreSQL 失败：%v（%s）", err, strings.TrimSpace(out))
	}
	s.waitPostgresReady(t, 3*time.Minute)

	// ── 恢复后：未确认调用改判 unknown、执行与 Run 收尾、租约由同一进程接管 ──
	s.waitForQuery(t, 3*time.Minute, "未确认的模型调用改判 unknown（不重发）",
		"SELECT (SELECT count(*) FROM llm_calls WHERE run_id = '"+runID+"' AND status = 'pending') || ':' || "+
			"(SELECT count(*) FROM llm_calls WHERE run_id = '"+runID+"' AND status = 'unknown')",
		func(v string) bool { return v == "0:1" })
	s.waitForQuery(t, 3*time.Minute, "执行行收尾",
		"SELECT status FROM task_executions WHERE id = "+executionID,
		func(v string) bool { return v == "failed" })
	// Run 必须收尾为**失权类**终态并写明原因，两条路径都合法：
	//   - `cancelled:renew_failed`：活着的进程在「续约失败累计到有界停止阈值」时自己停下
	//     （§2.3.6「A 的续约失败：A 停止发起后续动作」），本案中断足够长，它先于回收器触发；
	//   - `failed:worker_lost`：进程没来得及自停 / 被冻结，由恢复后的回收器认领收尾。
	// 判据是终态 + 原因 + 不重放，不是某一路径的私有字面量；completed / running 都不接受。
	// Either legitimate loss verdict is accepted — the contract is a terminal, reason-carrying
	// outcome with no replay, not one detector's private literal.
	s.waitForQuery(t, 3*time.Minute, "Run 收尾为失权类终态并带原因",
		"SELECT status || ':' || COALESCE(reason, '-') FROM runs WHERE run_id = '"+runID+"'",
		func(v string) bool { return v == "failed:worker_lost" || v == "cancelled:renew_failed" })
	// 租约必须**已释放**且仍归中断前的进程。自停路径（本案实测）：活进程在阈值上停止后
	// 交还自己的租约，代次不变；接管路径：回收器认领过期租约，代次 +1。不接受的只有
	// "仍被持有"（released_at 为空）与"换了 owner"（别的进程收的尾）。
	lease := s.probeLease(t, executionID)
	if lease[0] != controlWorker || lease[2] == "-" || (lease[1] != "1" && lease[1] != "2") {
		t.Fatalf("恢复后的租约读数是 owner/epoch/released=%v，要求 %s/{1|2}/已释放（活进程自停交还或回收器接管，owner 不变）",
			lease, controlWorker)
	}

	// ── 零重放与进程身份 ─────────────────────────────────────────────────────
	after := s.probeReplaySnapshot(t, runID)
	if after != before {
		t.Fatalf("恢复过程中的审计行数发生变化（中断前 %s，恢复后 %s），要求逐项相同（不重放）", before, after)
	}
	s.assertSingleExecution(t, plannedAt)

	live := s.waitLiveWorkerIDs(t, 1, time.Minute)
	if live[0] != controlWorker {
		t.Fatalf("恢复后的在线 Worker 是 %q，要求仍是中断前的进程 %q（中断不得要求重启进程）", live[0], controlWorker)
	}
	if state := s.serviceState(ctx, us25ControlService); state != "running" {
		t.Fatalf("控制面容器状态是 %q，要求 running（中断全程不应退出）", state)
	}
	t.Logf("中断恢复：执行行 %s 收尾 failed、Run %s 收尾失权类终态、调用改判 unknown、零重放、租约已释放",
		executionID, runID)
}

// waitPostgresReady 轮询数据库直到接受查询。
//
// 不能用夹具的 `psql`：它在失败时直接判红，而这里要等的正是「还没起来」。也不能只等
// 容器 healthy——健康检查与真正可查询之间仍有窗口，一次致命的查询就会误杀用例。
// waitPostgresReady polls until the database accepts queries; the fixture's psql is fatal
// on error, so readiness must be probed with a non-fatal exec.
func (s *us25Stack) waitPostgresReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := s.exec(ctx, us25PostgresService, "sh", "-c",
			`psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc "SELECT 1"`)
		cancel()
		last = strings.TrimSpace(out)
		if err == nil && last == "1" {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("PostgreSQL 在 %s 内没有恢复可查询状态（最后读数 %q）", timeout, last)
}

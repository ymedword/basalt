//go:build integration

package e2e

// 本文件判 US-25C 的两类故障恢复与扫描公平（FR-25C-11 / 22 / 23）：
//   - 只破坏分发介质（载荷行）而不动数据库：未安装该哈希的实例落到 `payload_missing`，
//     不领取、不推进 `next_fire_at`、不写执行行；载荷恢复后在**运行期**有界自愈，不需要
//     重启；已就绪条目不再读载荷（再删一次仍按钉住版本执行）。
//   - 数据库中断：在途调用结果不可知时把数据库切断，**不重启进程**；恢复后未确认调用改判
//     `unknown`（不重发）、执行与 Run 收尾为失权类终态、租约仍归同一进程且已释放，审计
//     行数与中断前逐项相同。
//   - A 侧未就绪的 Agent 不饿死 B 侧健康任务：同一轮扫描里排在前面、不可领取的行不占住
//     领取名额，健康任务由 B 正常领取并执行。
// Payload-only damage, a database outage, and cross-instance scan fairness.

import (
	"strings"
	"testing"
	"time"
)

// TestUS25C_Fault_PayloadFailureRecovers 判载荷故障恢复：
//   - B 先以 h1 就绪并执行一次（证明它确实"曾经 ready"）；
//   - 控制面发布 V2 后**只删载荷行**，指针不动；B 的同步落到 `payload_missing`，后续
//     到期触发命就绪门：不领取、不推进、不写执行行、桩上零到达；
//   - 载荷恢复后 B 在运行期自愈（不重启）装好 h2，并由 B 领取执行、正文为 MARK-V2；
//   - 已就绪条目**不读载荷**：再删一次载荷，B 仍按钉住哈希 h2 执行。
//
// 为什么"再删一次仍能跑"是必须的判据：把恢复实现成"每轮都重新读载荷"，删行后会把
// 健康条目打回 blocked，已就绪实例被分发介质牵着走；FR-25C-22 要求已就绪条目不读载荷。
// TestUS25C_Fault_PayloadFailureRecovers recovers from a payload-only failure.
func TestUS25C_Fault_PayloadFailureRecovers(t *testing.T) {
	schema := us25cSchema(t)
	db := us25cOpenPG(t, schema.DSN)
	env := us25cEnv(t, schema.DSN, nil)
	stub := us25cNewStub(t)

	addrA := us25cFreeAddr(t)
	rootA := us25cNewRoot(t, env, addrA, "worker-a", stub.URL())
	us25cMigrate(t, rootA, env)
	procA := us25cStart(t, rootA, env, "serve")
	us25cWaitHealth(t, addrA, procA)

	modelUID := us25cWaitModelConnection(t, db, us25cProviderName, 45*time.Second)
	const (
		agentName = "us25c-payload"
		taskID    = "us25c-payload-tick"
	)
	us25cCreateAgent(t, addrA, agentName, us25cModelID)
	us25cSubmitDoc(t, addrA, agentName, us25cAgentDoc(agentName, us25cProviderName, us25cModelID,
		modelUID, taskID, "0 0 0 1 1 *", "MARK-V1"))
	h1 := us25cWaitPointer(t, db, agentName, "V1 发布指针", 45*time.Second,
		func(hash string) bool { return hash != "" })
	us25cEnableAgent(t, addrA, agentName)
	us25cWaitTask(t, db, taskID, 45*time.Second)

	rootB := us25cNewRoot(t, env, "", "worker-b", stub.URL())
	us25cCopyAgentDir(t, rootA, rootB, agentName)
	procB := us25cStart(t, rootB, env, "gateway")
	us25cWaitLog(t, procB, "调度器已启动", 45*time.Second)
	us25cWaitInstalled(t, rootB, agentName, h1, 60*time.Second)

	// 先让 B 以 h1 真正就绪：停掉控制面，这一次执行必须由 B 领取。
	procA.stop(t)
	claim1 := us25cClaimAs(t, db, stub, taskID, "worker-b", 120*time.Second)
	us25cAssertClaim(t, claim1, "worker-b", h1, "MARK-V1")

	// 控制面以新端口重启后发布 V2，然后只删载荷行——指针、指针版本、本地目录都不动。
	addrA2 := us25cFreeAddr(t)
	us25cWriteConfig(t, rootA, us25cConfigBody(addrA2, "worker-a", stub.URL()))
	procA2 := us25cStart(t, rootA, env, "serve")
	us25cWaitHealth(t, addrA2, procA2)
	us25cSubmitDoc(t, addrA2, agentName, us25cAgentDoc(agentName, us25cProviderName, us25cModelID,
		modelUID, taskID, "0 0 0 1 1 *", "MARK-V2"))
	h2 := us25cWaitPointer(t, db, agentName, "V2 发布指针", 45*time.Second,
		func(hash string) bool { return hash != "" && hash != h1 })
	payload, ok := us25cReadPayload(t, db, h2)
	if !ok {
		t.Fatalf("发布 V2 后载荷表缺少 %s（先载荷后指针未落地）", h2)
	}
	us25cDeletePayload(t, db, h2)
	// 停掉控制面：判"未就绪不领取"的那几拍只留 B 存活，领取归属才确定（A 若恰好在
	// 删除前抢装成功，会替 B 领取，把"零执行"读成红）。
	procA2.stop(t)

	// B 的下一轮同步必须落到 payload_missing——这是产品观察到的故障，不是用例宣布的。
	us25cWaitLogLine(t, procB, 60*time.Second,
		"asset sync item is not ready", "agent="+agentName, "reason=payload_missing")

	// 上弦：未就绪 → 不领取、不推进 next_fire_at、不写执行行、桩上零到达。
	baseline := us25cExecutionCount(t, db, taskID)
	arrivals := stub.Requests()
	us25cArmTask(t, db, taskID, 0)
	next0, ok := us25cNextFireAt(t, db, taskID)
	if !ok {
		t.Fatalf("上弦后读不到任务 %s 的 next_fire_at", taskID)
	}
	us25cWaitLogLine(t, procB, 60*time.Second,
		"agent not ready, trigger left in place", "agent="+agentName, "blocked=payload_missing")
	time.Sleep(1500 * time.Millisecond) // 若干拍：门每拍重判，任何一拍放行都会被看到
	if got := us25cExecutionCount(t, db, taskID); got != baseline {
		t.Fatalf("载荷缺失期间执行行数 = %d，期望 %d（未就绪不得写执行行）", got, baseline)
	}
	if got, _ := us25cNextFireAt(t, db, taskID); got != next0 {
		t.Fatalf("载荷缺失期间 next_fire_at = %d，期望 %d（未就绪不得推进）", got, next0)
	}
	if got := stub.Requests(); got != arrivals {
		t.Fatalf("载荷缺失期间桩的到达次数 = %d，期望 %d（未就绪不得发起调用）", got, arrivals)
	}

	// 恢复载荷：B 在运行期（进程不重启）下一轮有界自愈，装好 h2。
	us25cRestorePayload(t, db, h2, payload)
	us25cWaitInstalled(t, rootB, agentName, h2, 150*time.Second)

	claim2 := us25cClaimAs(t, db, stub, taskID, "worker-b", 120*time.Second)
	us25cAssertClaim(t, claim2, "worker-b", h2, "MARK-V2")
	us25cAssertWindow(t, claim2, "MARK-V2", "MARK-V1")

	// 已就绪条目不读载荷：再删一次，B 仍按钉住的 h2 执行。
	us25cDeletePayload(t, db, h2)
	claim3 := us25cClaimAs(t, db, stub, taskID, "worker-b", 120*time.Second)
	us25cAssertClaim(t, claim3, "worker-b", h2, "MARK-V2")
	us25cAssertWindow(t, claim3, "MARK-V2", "MARK-V1")
}

// TestUS25C_Fault_DatabaseOutageNoReplay 判数据库中断不重放（FR-25C-23）：
//   - 调用已发出、结果不可知时把测试库切断，进程保持运行；
//   - 中断期间日志里出现有界停止与审计终态回填失败——中断确实被产品观察到；
//   - 恢复后未确认调用改判 `unknown`（pending:unknown = 0:1）、执行行 failed、Run 收尾为
//     失权类终态（自停 `cancelled:renew_failed` 或接管 `failed:worker_lost` 均合法）；
//   - 租约仍归同一进程且已释放；消息 / 调用 / 工具行数与中断前逐项相同（零重放）；
//     桩的到达次数不增长；同一实例仍在线（中断不要求重启进程）。
//
// 中间那一段只读日志、不轮询 SQL：数据库不可达时判卷侧查询会直接失败，而这里要等的
// 正是"产品先认账、恢复后再收尾"的两段式判据。
// TestUS25C_Fault_DatabaseOutageNoReplay cuts the database during an in-flight model
// call and requires unknown-not-replayed after recovery.
func TestUS25C_Fault_DatabaseOutageNoReplay(t *testing.T) {
	schema := us25cSchema(t)
	db := us25cOpenPG(t, schema.DSN)
	proxy := us25cNewProxy(t, us25cDSNHost(t, schema.DSN))
	env := us25cEnv(t, us25cProxyDSN(t, schema.DSN, proxy.Addr()), nil)
	stub := us25cNewStub(t)

	addrA := us25cFreeAddr(t)
	rootA := us25cNewRoot(t, env, addrA, "worker-a", stub.URL())
	us25cMigrate(t, rootA, env)
	procA := us25cStart(t, rootA, env, "serve")
	us25cWaitHealth(t, addrA, procA)

	modelUID := us25cWaitModelConnection(t, db, us25cProviderName, 45*time.Second)
	const (
		agentName = "us25c-outage"
		taskID    = "us25c-outage-tick"
	)
	us25cCreateAgent(t, addrA, agentName, us25cModelID)
	us25cSubmitDoc(t, addrA, agentName, us25cAgentDoc(agentName, us25cProviderName, us25cModelID,
		modelUID, taskID, "0 0 0 1 1 *", "MARK-OUTAGE"))
	h1 := us25cWaitPointer(t, db, agentName, "发布指针", 45*time.Second,
		func(hash string) bool { return hash != "" })
	us25cEnableAgent(t, addrA, agentName)
	us25cWaitTask(t, db, taskID, 45*time.Second)
	us25cWaitInstalled(t, rootA, agentName, h1, 60*time.Second)

	// —— 让调用停在"已发出、结果不可知"：桩的闸门挂住响应 ——
	stub.Block()
	arrivals := stub.Requests()
	baseline := us25cExecutionCount(t, db, taskID)
	us25cArmTask(t, db, taskID, 0)
	exec, ok := us25cNewExecutionCount(t, db, taskID, baseline, 60*time.Second)
	if !ok || exec.RunID == "" {
		t.Fatalf("等待执行行（含 Run 标识）超时：行 %+v\nserve 日志：\n%s", exec, procA.us25cLogs())
	}
	us25cWaitRequests(t, stub, arrivals+1, 60*time.Second)
	worker, instance, ownerKnown := us25cExecutionOwnerByID(t, db, exec.ID)
	if !ownerKnown || worker != "worker-a" {
		t.Fatalf("执行行 %d 的领取者 = %q（ok=%v），期望 worker-a", exec.ID, worker, ownerKnown)
	}
	before := us25cReplayFingerprint(t, db, exec.RunID)

	// —— 切断数据库：进程与模型请求都保持原样 ——
	proxy.Cut(true)
	// 续约失败累计到 ttl - renew_interval（6s - 2s）后有界停止：这条日志是"中断确实被
	// 产品观察到"的证据，不是用例单方面宣布数据库不可用。
	us25cWaitLog(t, procA, "租约续约超过有界停止阈值，停止执行", 120*time.Second)
	// 停止之后的终态回填打在不可达的数据库上：行留在 pending，等恢复扫描归一化。
	us25cWaitLog(t, procA, "审计终态回填失败", 60*time.Second)

	// —— 恢复数据库 ——
	proxy.Cut(false)
	us25cWaitScalar(t, db, "未确认的模型调用改判 unknown（不重发）", 180*time.Second,
		`SELECT (SELECT count(*)::text FROM llm_calls WHERE run_id = $1 AND status = 'pending')
		        || ':' ||
		        (SELECT count(*)::text FROM llm_calls WHERE run_id = $1 AND status = 'unknown')`,
		func(v string) bool { return v == "0:1" }, exec.RunID)
	done := us25cWaitExecutionDone(t, db, exec.ID, 180*time.Second)
	if done.Status != "failed" {
		t.Fatalf("恢复后执行行 %d 状态 = %q，期望 failed（失权类收尾）", exec.ID, done.Status)
	}
	// 两条收尾路径都合法：活进程在停止阈值上自己交还（cancelled:renew_failed），或恢复后
	// 的回收器认领过期租约（failed:worker_lost）。只拒绝非终态与"completed"。
	us25cWaitScalar(t, db, "Run 收尾为失权类终态并带原因", 180*time.Second,
		`SELECT status || ':' || COALESCE(reason, '-') FROM runs WHERE run_id = $1`,
		func(v string) bool { return v == "failed:worker_lost" || v == "cancelled:renew_failed" },
		exec.RunID)
	// 租约必须已释放且仍归中断前的进程：自停路径代次不变，接管路径代次 +1。
	owner, epoch, released := us25cLease(t, db, exec.ID)
	if owner != instance || released == "-" || (epoch != 1 && epoch != 2) {
		t.Fatalf("恢复后租约读数是 owner=%q epoch=%d released=%q，要求 %q/{1|2}/已释放",
			owner, epoch, released, instance)
	}

	// —— 零重放与进程身份 ——
	if after := us25cReplayFingerprint(t, db, exec.RunID); after != before {
		t.Fatalf("恢复过程中的审计台账发生变化（中断前 %s，恢复后 %s），要求逐项相同（不重放）",
			before, after)
	}
	if got := stub.Requests(); got != arrivals+1 {
		t.Fatalf("恢复后桩的到达次数 = %d，期望 %d（未确认调用不得自动重放）", got, arrivals+1)
	}
	if stopped, _ := us25cScalarString(t, db,
		`SELECT COALESCE(stopped_at::text, '-') FROM worker_nodes WHERE instance_id = $1`,
		instance); stopped != "-" {
		t.Fatalf("恢复后实例 %s 的 stopped_at = %q，要求未停止（中断不要求重启进程）", instance, stopped)
	}
	us25cWaitHealth(t, addrA, procA)
	stub.Release()
}

// TestUS25C_Scan_UnreadyAgentOnADoesNotStarveB 判扫描公平（FR-25C-11）：
//   - A 侧两个 Agent 的已安装目录都被破坏（清单自报哈希 ≠ 目录寻址哈希 → corrupt），
//     stuck 的载荷行另外删除（B 侧因此落到 payload_missing）；
//   - 两个任务同轮到期、stuck 排在前面；A 对 stuck 命就绪门：不领取、不推进、不写执行行；
//   - 同一轮里健康任务不受影响：B（唯一就绪的一端）照常领取执行，正文与清单都指向
//     h_healthy，A 全程保持健康响应。
//
// 为什么让 A 留在运行中：本用例要证的正是"A 侧不可领取不饿死 B 侧健康任务"，关掉 A
// 再判就退化成单实例用例。
// TestUS25C_Scan_UnreadyAgentOnADoesNotStarveB keeps B healthy while A is unready.
func TestUS25C_Scan_UnreadyAgentOnADoesNotStarveB(t *testing.T) {
	schema := us25cSchema(t)
	db := us25cOpenPG(t, schema.DSN)
	env := us25cEnv(t, schema.DSN, nil)
	stub := us25cNewStub(t)

	addrA := us25cFreeAddr(t)
	rootA := us25cNewRoot(t, env, addrA, "worker-a", stub.URL())
	us25cMigrate(t, rootA, env)
	procA := us25cStart(t, rootA, env, "serve")
	us25cWaitHealth(t, addrA, procA)

	modelUID := us25cWaitModelConnection(t, db, us25cProviderName, 45*time.Second)
	const (
		stuckAgent   = "us25c-scan-stuck"
		stuckTask    = "us25c-scan-stuck-tick"
		healthyAgent = "us25c-scan-healthy"
		healthyTask  = "us25c-scan-healthy-tick"
	)
	us25cCreateAgent(t, addrA, stuckAgent, us25cModelID)
	us25cCreateAgent(t, addrA, healthyAgent, us25cModelID)
	us25cSubmitDoc(t, addrA, stuckAgent, us25cAgentDoc(stuckAgent, us25cProviderName, us25cModelID,
		modelUID, stuckTask, "0 0 0 1 1 *", "MARK-STUCK"))
	hStuck := us25cWaitPointer(t, db, stuckAgent, "stuck 发布指针", 45*time.Second,
		func(hash string) bool { return hash != "" })
	us25cSubmitDoc(t, addrA, healthyAgent, us25cAgentDoc(healthyAgent, us25cProviderName, us25cModelID,
		modelUID, healthyTask, "0 0 0 1 1 *", "MARK-HEALTHY"))
	hHealthy := us25cWaitPointer(t, db, healthyAgent, "healthy 发布指针", 45*time.Second,
		func(hash string) bool { return hash != "" })
	us25cEnableAgent(t, addrA, stuckAgent)
	us25cEnableAgent(t, addrA, healthyAgent)
	us25cWaitTask(t, db, stuckTask, 45*time.Second)
	us25cWaitTask(t, db, healthyTask, 45*time.Second)
	us25cWaitRegistered(t, addrA, stuckAgent)
	us25cWaitRegistered(t, addrA, healthyAgent)
	us25cWaitInstalled(t, rootA, stuckAgent, hStuck, 60*time.Second)
	us25cWaitInstalled(t, rootA, healthyAgent, hHealthy, 60*time.Second)

	// 停 A 后破坏两份已安装目录（清单自报哈希 ≠ 目录寻址哈希），并删掉 stuck 的载荷行：
	// A 侧两个 Agent 都落到 corrupt（Resolve 判版本不符，不再回落到载荷），B 侧 stuck
	// 走"没有本地目录 + 载荷缺失"这一支。
	procA.stop(t)
	us25cPlantCorruptManifest(t, rootA, stuckAgent, hStuck)
	us25cPlantCorruptManifest(t, rootA, healthyAgent, hHealthy)
	us25cDeletePayload(t, db, hStuck)

	rootB := us25cNewRoot(t, env, "", "worker-b", stub.URL())
	us25cCopyAgentDir(t, rootA, rootB, stuckAgent)
	us25cCopyAgentDir(t, rootA, rootB, healthyAgent)
	procB := us25cStart(t, rootB, env, "gateway")
	us25cWaitLog(t, procB, "调度器已启动", 45*time.Second)
	us25cWaitLogLine(t, procB, 60*time.Second,
		"asset sync item is not ready", "agent="+stuckAgent, "reason=payload_missing")
	us25cWaitInstalled(t, rootB, healthyAgent, hHealthy, 60*time.Second)

	// A 以新端口重启：两份本地目录都自报哈希不符 → corrupt。
	addrA2 := us25cFreeAddr(t)
	us25cWriteConfig(t, rootA, us25cConfigBody(addrA2, "worker-a", stub.URL()))
	procA2 := us25cStart(t, rootA, env, "serve")
	us25cWaitHealth(t, addrA2, procA2)
	us25cWaitLogLine(t, procA2, 60*time.Second,
		"asset sync item is not ready", "agent="+stuckAgent, "reason=corrupt")
	us25cWaitLogLine(t, procA2, 60*time.Second,
		"asset sync item is not ready", "agent="+healthyAgent, "reason=corrupt")
	us25cWaitRegistered(t, addrA2, stuckAgent)
	us25cWaitRegistered(t, addrA2, healthyAgent)

	// 两行同轮到期，stuck 排在前面（先上弦）。A 对 stuck 命就绪门：不领取、不推进。
	stuckBaseline := us25cExecutionCount(t, db, stuckTask)
	us25cArmTask(t, db, stuckTask, 0)
	stuckNext, ok := us25cNextFireAt(t, db, stuckTask)
	if !ok {
		t.Fatalf("上弦后读不到任务 %s 的 next_fire_at", stuckTask)
	}
	us25cArmTask(t, db, healthyTask, 50*time.Millisecond)
	us25cWaitLogLine(t, procA2, 60*time.Second,
		"agent not ready, trigger left in place", "agent="+stuckAgent, "blocked=corrupt")
	time.Sleep(1500 * time.Millisecond) // 若干拍：任何一拍放行都会留下执行行
	if got := us25cExecutionCount(t, db, stuckTask); got != stuckBaseline {
		t.Fatalf("stuck 任务执行行数 = %d，期望 %d（未就绪不得写执行行）", got, stuckBaseline)
	}
	if got, _ := us25cNextFireAt(t, db, stuckTask); got != stuckNext {
		t.Fatalf("stuck 任务 next_fire_at = %d，期望 %d（未就绪不得推进）", got, stuckNext)
	}

	// 健康任务不受影响：B 是唯一就绪的一端，照常领取并按 h_healthy 执行。
	claim := us25cClaimAs(t, db, stub, healthyTask, "worker-b", 120*time.Second)
	us25cAssertClaim(t, claim, "worker-b", hHealthy, "MARK-HEALTHY")
	if strings.Contains(claim.Window, "MARK-STUCK") {
		t.Fatalf("健康任务 %s 的请求窗口混入了 stuck 任务的正文：\n%s", healthyTask, claim.Window)
	}
	// A 全程保持健康响应：未就绪不拖垮进程。
	us25cWaitHealth(t, addrA2, procA2)
}

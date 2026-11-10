//go:build integration

package e2e

// 本文件判 US-25C 的主链路（FR-25C-07 / 08 / 09 / 13 / 21）：一次发布 = 一次数据库
// 写入 + 各实例有界拉取校验；控制面（`serve`）与 Worker（`gateway`）各自安装后，**两端
// 都以同一个钉住哈希执行**，运行期清单与实际生效的正文一致；发布新版本后两端都按新
// 版本执行。断言读的是数据库里的版本哈希、`bundle.json` 的安装见证与桩**收到**的请求
// 正文——不是"文件复制成功"。
// This file covers the publish → sync → execute loop across two live instances.

import (
	"strings"
	"testing"
	"time"
)

// us25cAssertWindow 判一次 Run 在桩上的请求窗口：必须包含必须命中的正文标记，且
// （当 forbids 非空时）**不得**包含旧版本标记。窗口按"上弦前的到达序号"切片，所以
// 这个断言只覆盖本次 Run，不会被上一轮的正文污染。
// us25cAssertWindow asserts on the request window of exactly one run.
func us25cAssertWindow(t *testing.T, claim us25cClaim, contains, forbids string) {
	t.Helper()
	if !strings.Contains(claim.Window, contains) {
		t.Fatalf("%s 的 Run %s 请求窗口缺少 %q（钉住哈希 %s）：\n%s",
			claim.Worker, claim.Execution.RunID, contains, claim.Execution.PinnedHash, claim.Window)
	}
	if forbids != "" && strings.Contains(claim.Window, forbids) {
		t.Fatalf("%s 的 Run %s 请求窗口仍含旧版本标记 %q（钉住哈希 %s）：\n%s",
			claim.Worker, claim.Execution.RunID, forbids, claim.Execution.PinnedHash, claim.Window)
	}
}

// us25cAssertClaim 判一次领取：执行成功、归属点名、钉住哈希与运行期清单哈希都等于
// 期望版本。三条读的是三个独立落点（执行行 / 资源清单 / 请求正文），任何一条分叉都
// 说明"发布—同步—执行"没闭环。
// us25cAssertClaim asserts one claim pinned and executed exactly the expected bundle.
func us25cAssertClaim(t *testing.T, claim us25cClaim, owner, hash, marker string) {
	t.Helper()
	if claim.Worker != owner {
		t.Fatalf("领取进程 = %q，期望 %q（执行行 %d）", claim.Worker, owner, claim.Execution.ID)
	}
	if claim.Execution.Status != "completed" {
		t.Fatalf("%s 的执行行 %d 状态 = %q，期望 completed", owner, claim.Execution.ID, claim.Execution.Status)
	}
	if claim.Execution.PinnedHash != hash {
		t.Fatalf("%s 的执行行 %d 钉住哈希 = %q，期望 %q", owner, claim.Execution.ID, claim.Execution.PinnedHash, hash)
	}
	if claim.Manifest != hash {
		t.Fatalf("%s 的 Run %s 资源清单哈希 = %q，期望 %q", owner, claim.Execution.RunID, claim.Manifest, hash)
	}
	us25cAssertWindow(t, claim, marker, "")
}

// TestUS25C_Distribution_VersionsMatchAcrossInstances 判发布-同步-执行版本一致：
//   - 发布 V1 后，A（serve）与 B（gateway）都装好 h1，并各自以 h1 执行、正文为 MARK-V1；
//   - 发布 V2 后，两端都换到 h2 执行、正文为 MARK-V2 且不再含 MARK-V1。
//
// 两端**各判一次**而不是"有一端跑过就算"：只断言一端无法证明另一端没有仍用旧副本，
// 而"B 还在用旧副本"正是本 US 要消灭的故障。谁先领取由 CAS 决定，所以每次只保证
// 一个实例存活，让"这一次是谁领的"确定下来（停另一个实例不改任何共享状态）。
// TestUS25C_Distribution_VersionsMatchAcrossInstances pins the same hash on both sides.
func TestUS25C_Distribution_VersionsMatchAcrossInstances(t *testing.T) {
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
		agentName = "us25c-distribution"
		taskID    = "us25c-distribution-tick"
	)
	us25cCreateAgent(t, addrA, agentName, us25cModelID)

	us25cSubmitDoc(t, addrA, agentName, us25cAgentDoc(agentName, us25cProviderName, us25cModelID,
		modelUID, taskID, "0 0 0 1 1 *", "MARK-V1"))
	h1 := us25cWaitPointer(t, db, agentName, "V1 发布指针", 45*time.Second,
		func(hash string) bool { return hash != "" })
	us25cEnableAgent(t, addrA, agentName)
	us25cWaitTask(t, db, taskID, 45*time.Second)
	us25cWaitRegistered(t, addrA, agentName)

	// B 的工作区只有种子目录（与 Compose 镜像同形态）：运行定义与正文一律取自已安装
	// 的发布包。之后**不再**更新 B 的种子——同步若失效，B 会一直用 h1。
	rootB := us25cNewRoot(t, env, "", "worker-b", stub.URL())
	us25cCopyAgentDir(t, rootA, rootB, agentName)
	procB := us25cStart(t, rootB, env, "gateway")
	us25cWaitLog(t, procB, "调度器已启动", 45*time.Second)

	us25cWaitInstalled(t, rootA, agentName, h1, 45*time.Second)
	us25cWaitInstalled(t, rootB, agentName, h1, 45*time.Second)

	// —— V1：两端各执行一次，钉住 h1、正文 MARK-V1 ——
	procB.stop(t)
	claimA1 := us25cClaimAs(t, db, stub, taskID, "worker-a", 120*time.Second)
	us25cAssertClaim(t, claimA1, "worker-a", h1, "MARK-V1")

	procA.stop(t)
	procB2 := us25cStart(t, rootB, env, "gateway")
	us25cWaitLog(t, procB2, "调度器已启动", 45*time.Second)
	claimB1 := us25cClaimAs(t, db, stub, taskID, "worker-b", 120*time.Second)
	us25cAssertClaim(t, claimB1, "worker-b", h1, "MARK-V1")

	// 控制面换新端口重启：旧监听器的端口可能仍在 TIME_WAIT，换端口不影响数据库状态。
	addrA2 := us25cFreeAddr(t)
	us25cWriteConfig(t, rootA, us25cConfigBody(addrA2, "worker-a", stub.URL()))
	procA2 := us25cStart(t, rootA, env, "serve")
	us25cWaitHealth(t, addrA2, procA2)
	us25cWaitRegistered(t, addrA2, agentName)

	// —— 发布 V2：两端都换装 h2，谁都不需要重启 ——
	us25cSubmitDoc(t, addrA2, agentName, us25cAgentDoc(agentName, us25cProviderName, us25cModelID,
		modelUID, taskID, "0 0 0 1 1 *", "MARK-V2"))
	h2 := us25cWaitPointer(t, db, agentName, "V2 发布指针", 45*time.Second,
		func(hash string) bool { return hash != "" && hash != h1 })
	us25cWaitInstalled(t, rootA, agentName, h2, 60*time.Second)
	us25cWaitInstalled(t, rootB, agentName, h2, 60*time.Second)

	procB2.stop(t)
	claimA2 := us25cClaimAs(t, db, stub, taskID, "worker-a", 120*time.Second)
	us25cAssertClaim(t, claimA2, "worker-a", h2, "MARK-V2")
	us25cAssertWindow(t, claimA2, "MARK-V2", "MARK-V1")

	procA2.stop(t)
	procB3 := us25cStart(t, rootB, env, "gateway")
	us25cWaitLog(t, procB3, "调度器已启动", 45*time.Second)
	claimB2 := us25cClaimAs(t, db, stub, taskID, "worker-b", 120*time.Second)
	us25cAssertClaim(t, claimB2, "worker-b", h2, "MARK-V2")
	us25cAssertWindow(t, claimB2, "MARK-V2", "MARK-V1")

	// 收尾复核：指针从未被 Worker 回写回旧版本（FR-25C-09 的"永不回写旧副本"）。
	if got := us25cPointerHash(t, db, agentName); got != h2 {
		t.Fatalf("两端执行后指针哈希 = %q，期望仍是 %q（Worker 不得回写旧副本）", got, h2)
	}
}

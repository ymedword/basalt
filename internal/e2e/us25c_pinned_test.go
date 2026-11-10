//go:build integration

package e2e

// 本文件判 US-25C 的在途版本固定（FR-25C-08 / 22）：领取事务钉住 bundle 哈希后，即使
// 指针已经移到新版本、本机也装好了新版本，**在途 Run 仍按旧版本跑完**，且不会因为
// 发布而重发或重放模型调用。恢复后新触发的执行才切到新版本。
// This file covers the in-flight pin: a publish never switches a running run.

import (
	"strings"
	"testing"
	"time"
)

// TestUS25C_PinnedRun_KeepsOldVersionAfterPublish 判在途旧版本：
//   - B 领取一次执行并以 h1 跑在途（模型调用挂在桩的闸门上）；
//   - 控制面重启后发布 V2，指针立刻指向 h2，B 也装好 h2；
//   - 在途执行仍钉 h1、Run 的资源清单仍是 h1、桩的到达次数不变（没有重发）；
//   - 放行后 Run 用 h1 的正文跑完；之后的新触发改为钉 h2。
//
// 为什么必须"挂在在途"再发布：只比对终态的哈希无法区分"领取时就钉住了"和"执行中途
// 才换了版本"——后者恰恰是要防的漂移。
// TestUS25C_PinnedRun_KeepsOldVersionAfterPublish keeps a running run on its pinned hash.
func TestUS25C_PinnedRun_KeepsOldVersionAfterPublish(t *testing.T) {
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
		agentName = "us25c-pinned"
		taskID    = "us25c-pinned-tick"
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

	// 只留 B 存活：这一次执行必须由 B 领取，归属才可判定。
	procA.stop(t)

	stub.Block()
	arrivals := stub.Requests()
	baseline := us25cExecutionCount(t, db, taskID)
	var exec us25cExecution
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		us25cArmTask(t, db, taskID, 0)
		if got, ok := us25cNewExecutionCount(t, db, taskID, baseline, 3*time.Second); ok {
			exec = got
			break
		}
	}
	if exec.ID == 0 {
		t.Fatalf("等待 B 领取任务 %s 超时（60s）", taskID)
	}
	us25cWaitRequests(t, stub, arrivals+1, 30*time.Second)
	if worker, _, _ := us25cExecutionOwnerByID(t, db, exec.ID); worker != "worker-b" {
		t.Fatalf("执行行 %d 的领取者 = %q，期望 worker-b", exec.ID, worker)
	}
	if exec.PinnedHash != h1 {
		t.Fatalf("在途执行行 %d 钉住哈希 = %q，期望 %q", exec.ID, exec.PinnedHash, h1)
	}

	// 控制面以新端口重启后发布 V2：指针移到 h2，B 在运行中装好 h2。
	addrA2 := us25cFreeAddr(t)
	us25cWriteConfig(t, rootA, us25cConfigBody(addrA2, "worker-a", stub.URL()))
	procA2 := us25cStart(t, rootA, env, "serve")
	us25cWaitHealth(t, addrA2, procA2)
	us25cSubmitDoc(t, addrA2, agentName, us25cAgentDoc(agentName, us25cProviderName, us25cModelID,
		modelUID, taskID, "0 0 0 1 1 *", "MARK-V2"))
	h2 := us25cWaitPointer(t, db, agentName, "V2 发布指针", 45*time.Second,
		func(hash string) bool { return hash != "" && hash != h1 })
	us25cWaitInstalled(t, rootA, agentName, h2, 60*time.Second)
	us25cWaitInstalled(t, rootB, agentName, h2, 60*time.Second)

	// 发布之后、放行之前：在途执行仍钉 h1、Run 仍在跑、桩没有收到第二次请求。
	if pinned, ok := us25cScalarString(t, db,
		`SELECT COALESCE(pinned_bundle_hash, '') FROM task_executions WHERE id = $1`, exec.ID); !ok || pinned != h1 {
		t.Fatalf("发布后执行行 %d 的钉住哈希 = %q（ok=%v），期望仍是 %q", exec.ID, pinned, ok, h1)
	}
	status, _, ok := us25cRunStatus(t, db, exec.RunID)
	if !ok || status != "running" {
		t.Fatalf("发布后在途 Run %s 状态 = %q（ok=%v），期望仍是 running", exec.RunID, status, ok)
	}
	if got := stub.Requests(); got != arrivals+1 {
		t.Fatalf("发布后桩的到达次数 = %d，期望 %d（在途调用不得因发布重发）", got, arrivals+1)
	}

	stub.Release()
	us25cWaitRunTerminal(t, db, exec.RunID, 120*time.Second)
	window := stub.ContentsFrom(arrivals)
	if !strings.Contains(window, "MARK-V1") {
		t.Fatalf("在途 Run %s 的请求窗口缺少旧版本标记 MARK-V1：\n%s", exec.RunID, window)
	}
	if strings.Contains(window, "MARK-V2") {
		t.Fatalf("在途 Run %s 的请求窗口出现了新版本标记 MARK-V2（在途 Run 被切了版本）：\n%s", exec.RunID, window)
	}
	if manifest, ok := us25cRunManifestHash(t, db, exec.RunID); !ok || manifest != h1 {
		t.Fatalf("在途 Run %s 的资源清单哈希 = %q（ok=%v），期望 %q", exec.RunID, manifest, ok, h1)
	}

	// 恢复后新触发：只留 B，领取与执行都切到 h2。
	procA2.stop(t)
	claim := us25cClaimAs(t, db, stub, taskID, "worker-b", 120*time.Second)
	us25cAssertClaim(t, claim, "worker-b", h2, "MARK-V2")
	us25cAssertWindow(t, claim, "MARK-V2", "MARK-V1")
}

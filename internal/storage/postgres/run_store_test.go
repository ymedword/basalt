//go:build integration

package postgres

// 本文件是 US-25C §2.3.5.1 T3 的存储层判卷面（模块卡 §4.4 / FR-25C-08）：
// 领取事务内的版本钉住（C20），以及「执行权只在租约」——执行行 `status='started'`
// 只是领取载体，租约行 owner / epoch / 未释放 / 未过期才是执行权的唯一权威。
//
// 五条判据：
//   - 「查指针 → 发布 → 领取」窗口里版本移动：`ErrAssetMoved` 且**零写入**；
//   - 领取后租约被回收器接管（epoch+1）或自然过期：旧持有者的 `StartRun` 返回
//     `ErrClaimPinInvalid`，零 Run、零清单、零引用、零运行租约；
//   - `Pin == nil` 的普通 Run 不被钉住路径放宽，仍按当前指针判定；
//   - 「生命周期行 → 执行租约 `FOR SHARE` → 执行行」与回收器 `SKIP LOCKED`
//     并发不阻塞、不死锁（`-race` 单列运行）。
//
// This file owns the storage-level T3 judgments: the pinned claim inside the claim
// transaction, and execution rights living only in the lease row. Every refusal is
// asserted together with the zero-write outcome, not by the error alone.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 夹具哈希：生命周期状态文件哈希与清单口径同形（`sha256:` + 64 位小写十六进制）。
// Fixture hashes share the lifecycle state-file shape.
var (
	pinStateHash  = "sha256:" + strings.Repeat("1", 64)
	pinBundleHash = "sha256:" + strings.Repeat("2", 64)
	pinMovedHash  = "sha256:" + strings.Repeat("3", 64)
)

// pinClaimOwner 是**原领取者**的身份：租约被接管或过期之后，旧 Worker 手里仍是这张
// 凭证（`worker-a` / epoch 1）。它与租约行的实际 owner 分开构造——这正是判据本身。
// pinClaimOwner is the original claimer whose stale credential the old worker still
// holds; the lease row may have moved on.
const pinClaimOwner = "worker-a"

// pinResources 是一次性资源世界：四类资源版本与发布指针、bundle 版本与指针、
// 生命周期索引行，以及据此构造的可准入清单。一个用例只建一次——`seedOperation`
// 的主键不允许同 ID 重复预写。
// pinResources is the once-per-case resource world the admission barrier reads.
type pinResources struct {
	manifest core.RunResourceManifest
}

// seedPinResources 落库钉住准入所需的全部共享记录。`manifestBundle` 是清单固定
// 的 bundle 哈希，`pointerHash` 是已发布指针指向的哈希——两者不同即"清单之后又发布
// 了一版"，正是普通 Run 与钉住 Run 判据分岔的地方。
// seedPinResources seeds the shared publication records; manifestBundle and pointerHash
// are deliberately separate parameters so a case can fork them on purpose.
func seedPinResources(t *testing.T, db *DB, manifestBundle, pointerHash string) pinResources {
	t.Helper()
	ctx := context.Background()
	fx := seedRunManifest(t, db)
	byKindID := make(map[string]core.ResourceVersion, len(fx.versions))
	for _, v := range fx.versions {
		byKindID[string(v.Kind)+"/"+v.ResourceID] = v
	}
	rs := NewResourceStore(db)
	// Provider 比 `published_version_id`，MCP 比 `applied_version_id`（D-12）：
	// 两条指针都要指到清单固定的同一版本，barrier 才会放行。
	// Provider compares published; MCP compares applied; both must equal the manifest.
	require.NoError(t, rs.Publish(ctx, core.ResourceModelConnection, "openai",
		byKindID["model_connection/openai"].ID, false, "op-run-manifest", opNow))
	require.NoError(t, rs.Publish(ctx, core.ResourceMCPServer, "browser",
		byKindID["mcp_server/browser"].ID, true, "op-run-manifest", opNow))
	require.NoError(t, rs.Publish(ctx, core.ResourceMCPServer, "filesystem",
		byKindID["mcp_server/filesystem"].ID, true, "op-run-manifest", opNow))

	// bundle 的 `resource_id` 与 `resource_uid` 同取 AgentUID（§2.3.9 身份断言）。
	// The bundle pointer is addressed by the agent uid, never by the agent name.
	bundle, _, err := rs.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceAgentBundle,
		ResourceUID:  core.ResourceUID(testAgentUID),
		ResourceID:   string(testAgentUID),
		ContentHash:  pointerHash,
		ManifestJSON: `[{"path":"AGENT.md","sha256":"` + pinBundleHash + `","size":10}]`,
		StoragePath:  ".basalt/versions/bundles/" + string(testAgentUID) + "/1",
		CreatedAt:    opNow.UnixMilli(),
		OperationID:  "op-run-manifest",
	})
	require.NoError(t, err)
	require.NoError(t, rs.Publish(ctx, core.ResourceAgentBundle, string(testAgentUID),
		bundle.ID, true, "op-run-manifest", opNow))

	// 生命周期索引行：既是准入屏障（FOR UPDATE）的对象，也是领取把
	// `agent_name` 映射到 `agent_uid` 的唯一权威。
	// The lifecycle row is both the barrier target and the name→uid mirror.
	_, err = db.pool.ExecContext(ctx, `
INSERT INTO agent_lifecycle
  (agent_name, agent_uid, uid_source, state, generation, state_file_hash, valid, updated_at, operation_id)
VALUES ('demo', $1, 'state_file', 'active', 1, $2, true, $3, 'op-run-manifest')`,
		string(testAgentUID), pinStateHash, opNow.UnixMilli())
	require.NoError(t, err)

	manifest := fx.manifest
	manifest.AssetBundleHash = manifestBundle
	rehashManifest(t, &manifest)
	return pinResources{manifest: manifest}
}

// seedPinTask 直插一条已到期的调度任务行：`next_fire_at = planned` 让领取的 CAS
// 真的可能成功——被拒时"未推进"才有判别力（NULL 的初态会让 CAS 永远失败）。
// seedPinTask seeds a due task so the claim CAS could actually succeed.
func seedPinTask(t *testing.T, db *DB, taskID string, planned time.Time) {
	t.Helper()
	taskPortSeedTask(t, db, taskID, "demo", "*/5 * * * * *", "钉住版本判据")
	scheduleSetNextFire(t, db, taskID, planned)
}

// seedPinClaimRecord 直插一条领取记录：`started` 执行行 + 调度租约。执行行四列
// （status / run_id / Agent / 哈希）全部按"领取成功"的形态写，唯一的变数交给
// `leaseOwner` / `leaseEpoch` / `leaseExpires`——判据要证明的正是"四项全相符也不够"。
// seedPinClaimRecord writes a claim record whose execution row is impeccable; only the
// lease row varies, so a rejection can only come from the lease authority.
func seedPinClaimRecord(t *testing.T, db *DB, taskID, runID, leaseOwner string,
	leaseEpoch int64, leaseExpires time.Time,
) (int64, *core.ClaimedBundlePin) {
	t.Helper()
	ctx := context.Background()
	planned := opNow.Add(time.Minute)
	var execID int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `
INSERT INTO task_executions
  (task_id, run_id, status, trigger, started_at, planned_at,
   owner_worker_id, ownership_epoch, pinned_bundle_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8)
RETURNING id`,
		taskID, runID, string(core.ExecutionStarted), string(core.TriggerCron),
		opNow.UnixMilli(), planned.UnixMilli(), pinClaimOwner, pinBundleHash).Scan(&execID))
	_, err := db.pool.ExecContext(ctx, `
INSERT INTO execution_leases
  (kind, resource_id, owner_worker_id, ownership_epoch, acquired_at, renewed_at, expires_at, released_at)
VALUES ($1, $2, $3, $4, $5, $5, $6, NULL)`,
		string(core.LeaseScheduleExecution), strconv.FormatInt(execID, 10),
		leaseOwner, leaseEpoch, opNow.UnixMilli(), leaseExpires.UnixMilli())
	require.NoError(t, err)
	return execID, &core.ClaimedBundlePin{
		ExecutionID: execID,
		BundleHash:  pinBundleHash,
		Lease: core.LeaseToken{
			Kind:           core.LeaseScheduleExecution,
			ResourceID:     strconv.FormatInt(execID, 10),
			OwnerWorkerID:  pinClaimOwner,
			OwnershipEpoch: 1,
		},
	}
}

// pinRunRecord 造一份调度渠道的起步行：Agent / 生命周期期望与夹具世界一致。
// pinRunRecord builds the scheduler-channel run record matching the fixture world.
func pinRunRecord(runID string) core.RunRecord {
	rec := startRow("scheduler")
	rec.RunID = runID
	rec.Agent = "demo"
	rec.Lifecycle = core.LifecycleExpectation{Generation: 1, StateFileHash: pinStateHash}
	return rec
}

// pinExecutionStatus 回读执行行状态（准入不得改写领取记录）。
// pinExecutionStatus reads the claim row status back.
func pinExecutionStatus(t *testing.T, db *DB, executionID int64) string {
	t.Helper()
	var status string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT status FROM task_executions WHERE id = $1`, executionID).Scan(&status))
	return status
}

// pinScheduleLeaseCount 数库里的调度租约行（隔离 schema 内只属于本用例）。
// pinScheduleLeaseCount counts schedule leases inside the case's isolated schema.
func pinScheduleLeaseCount(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM execution_leases WHERE kind = $1`,
		string(core.LeaseScheduleExecution)).Scan(&n))
	return n
}

// pinScheduleLeaseState 回读一条调度租约的 owner / epoch。
// pinScheduleLeaseState reads the lease's owner and epoch.
func pinScheduleLeaseState(t *testing.T, db *DB, executionID int64) (string, int64) {
	t.Helper()
	var (
		owner string
		epoch int64
	)
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT owner_worker_id, ownership_epoch FROM execution_leases
		  WHERE kind = $1 AND resource_id = $2`,
		string(core.LeaseScheduleExecution), strconv.FormatInt(executionID, 10)).Scan(&owner, &epoch))
	return owner, epoch
}

// pinRunLeaseCount 数某个 Run 的运行租约（被拒准入必须一条都没有）。
// pinRunLeaseCount counts the run lease of one run id.
func pinRunLeaseCount(t *testing.T, db *DB, runID string) int {
	t.Helper()
	var n int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(core.LeaseRun), runID).Scan(&n))
	return n
}

// TestClaim_ErrAssetMovedWritesNothing —— C20：扫描快照的期望哈希与库里的已发布指针
// 不符（查指针之后又发布了一版）时，领取必须整体回滚：不推进 `next_fire_at`、
// 不写执行行、不写租约。同样的零写入也适用于指针被撤销（`ErrAssetNotReady`）。
//
// 逆改点（判别力）：把 `lockPinnedPointer` 挪到 CAS 之后、或忽略哈希比较，本用例的
// `next_fire_at` 断言与执行行断言立刻变红。
// TestClaim_ErrAssetMovedWritesNothing pins C20's zero-write refusal on a moved pointer.
func TestClaim_ErrAssetMovedWritesNothing(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	// 清单/领取的期望是 H（pinBundleHash），指针已推进到 H'（pinMovedHash）。
	seedPinResources(t, db, pinBundleHash, pinMovedHash)
	const taskID = "pin-claim-task"
	planned := opNow.Add(time.Minute)
	seedPinTask(t, db, taskID, planned)
	next := planned.Add(time.Minute)

	_, err := NewTaskStore(db).ClaimDueTriggerPinned(ctx, taskID, planned, next,
		core.WorkerIdentity{InstanceID: pinClaimOwner}, workTTL, pinBundleHash)
	require.ErrorIs(t, err, core.ErrAssetMoved)

	at, ok := scheduleNextFireOf(t, db, taskID)
	require.True(t, ok, "next_fire_at 原样保留，不得被清空")
	require.Equal(t, planned.UTC(), at, "领取被拒：next_fire_at 不得推进")
	require.Zero(t, scheduleStartedRows(t, db, taskID), "领取被拒：不得留下执行行")
	require.Zero(t, pinScheduleLeaseCount(t, db), "领取被拒：不得留下调度租约")

	// 指针行被撤销（行不存在）→ 失败关闭为 ErrAssetNotReady，同样零写入。
	// A removed pointer fails closed as ErrAssetNotReady with the same zero writes.
	_, err = db.pool.ExecContext(ctx, `DELETE FROM resource_pointers
		WHERE resource_kind = $1 AND resource_id = $2`,
		string(core.ResourceAgentBundle), string(testAgentUID))
	require.NoError(t, err)
	_, err = NewTaskStore(db).ClaimDueTriggerPinned(ctx, taskID, planned, next,
		core.WorkerIdentity{InstanceID: pinClaimOwner}, workTTL, pinBundleHash)
	require.ErrorIs(t, err, core.ErrAssetNotReady)

	at, ok = scheduleNextFireOf(t, db, taskID)
	require.True(t, ok)
	require.Equal(t, planned.UTC(), at, "指针撤销同样是零写入结局")
	require.Zero(t, scheduleStartedRows(t, db, taskID))
	require.Zero(t, pinScheduleLeaseCount(t, db))
}

// TestStartRun_PinnedClaimRejectedAfterLeaseTakeover —— 领取后暂停 → 回收器接管
// （owner/epoch 改为 worker-b/2）→ 旧 Worker 带原凭证（worker-a/1）尝试准入：
// `ErrClaimPinInvalid`、零新 Run、零运行租约，领取记录本身不被改写。
//
// 逆改点（判别力）：把 `recheckClaimPinInTx` 的租约段删掉、只凭执行行
// `status='started'` 判定执行权，本用例立即变绿——那正是要防的"执行行当执行权"。
// TestStartRun_PinnedClaimRejectedAfterLeaseTakeover proves the lease row, not the
// execution row, is the authority on execution rights.
func TestStartRun_PinnedClaimRejectedAfterLeaseTakeover(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	res := seedPinResources(t, db, pinBundleHash, pinBundleHash)
	const taskID = "pin-takeover-task"
	seedPinTask(t, db, taskID, opNow.Add(time.Minute))
	runID := "scheduler-pin-takeover-0001"
	// 执行行四项全部与本次 Run 相符；只有租约行已经换主。
	execID, pin := seedPinClaimRecord(t, db, taskID, runID, "worker-b", 2,
		time.Now().Add(workTTL))
	rec := pinRunRecord(runID)
	rec.Pin = pin

	err := startRunTest(NewRunStore(db), ctx, rec, res.manifest)
	require.ErrorIs(t, err, core.ErrClaimPinInvalid,
		"执行行四项全相符也不行：执行权只在租约行")
	requireNoRunTrace(t, db, runID)
	require.Zero(t, pinRunLeaseCount(t, db, runID), "被拒准入不得创建运行租约")
	require.Equal(t, string(core.ExecutionStarted), pinExecutionStatus(t, db, execID),
		"准入不得改写领取记录")
	owner, epoch := pinScheduleLeaseState(t, db, execID)
	require.Equal(t, "worker-b", owner)
	require.Equal(t, int64(2), epoch)
}

// TestStartRun_PinnedClaimRejectedAfterLeaseExpiry —— 租约自然过期、尚未被接管：
// 同样拒绝。执行权的判据是 `expires_at > db_now`，不是"还没人抢"。
//
// 逆改点（判别力）：把 `expiresAt <= nowMS` 的比较删掉，本用例变绿——过期租约重新
// 成为执行权，正是崩溃后"幽灵 Worker"能重复建 Run 的入口。
// TestStartRun_PinnedClaimRejectedAfterLeaseExpiry proves an expired lease is not
// execution authority even before anyone takes it over.
func TestStartRun_PinnedClaimRejectedAfterLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	res := seedPinResources(t, db, pinBundleHash, pinBundleHash)
	const taskID = "pin-expiry-task"
	seedPinTask(t, db, taskID, opNow.Add(time.Minute))
	runID := "scheduler-pin-expiry-0001"
	// 过期时刻取夹具冻结时钟（远早于真实 now），owner/epoch 仍是原领取者。
	execID, pin := seedPinClaimRecord(t, db, taskID, runID, pinClaimOwner, 1, opNow)
	rec := pinRunRecord(runID)
	rec.Pin = pin

	err := startRunTest(NewRunStore(db), ctx, rec, res.manifest)
	require.ErrorIs(t, err, core.ErrClaimPinInvalid)
	requireNoRunTrace(t, db, runID)
	require.Zero(t, pinRunLeaseCount(t, db, runID), "被拒准入不得创建运行租约")
	require.Equal(t, string(core.ExecutionStarted), pinExecutionStatus(t, db, execID),
		"准入不得改写领取记录")
}

// TestStartRun_NormalRunKeepsCurrentPointerCheck —— `Pin == nil` 的普通 Run 仍按
// 当前 `agent_bundle` 发布指针判定（`ErrAssetVersionMismatch`），不被钉住路径放宽：
// 只有领取记录 + 租约才能替代指针相等比较，调用方手里的哈希做不到。
//
// 逆改点（判别力）：把 `admitPublishedResources` 的 `rec.Pin == nil` 条件去掉、
// 一律跳过 `recheckAgentBundle`，本用例变绿——发布窗口重新打开。
// TestStartRun_NormalRunKeepsCurrentPointerCheck proves the pinned path does not loosen
// the pointer equality for unpinned runs.
func TestStartRun_NormalRunKeepsCurrentPointerCheck(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	// 清单固定 H，指针已推进到 H'：普通 Run 必须被拒。
	res := seedPinResources(t, db, pinBundleHash, pinMovedHash)
	runID := "scheduler-pin-normal-0001"
	rec := pinRunRecord(runID)

	err := startRunTest(NewRunStore(db), ctx, rec, res.manifest)
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch,
		"Pin == nil 不得走钉住路径放宽指针比较")
	requireNoRunTrace(t, db, runID)
}

// TestStartRun_PinnedLockOrderNoDeadlock —— 锁序「生命周期行 `FOR UPDATE` →
// 执行租约 `FOR SHARE` → 执行行」与回收器「租约行 `FOR UPDATE SKIP LOCKED`」并发：
// 两条路径都必须在超时内返回，且都不出现 PostgreSQL 死锁（40P01）。租约被回收器
// 领走（epoch → 2）或被 `SKIP LOCKED` 跳过（epoch 仍 1）都是合法交错，但租约行的
// 终态必须与之一致，且过期租约在任何交错下都不得放行 Run。
//
// 逆改点（判别力）：把 StartRun 改成先取租约行再取生命周期行（与归档屏障反向），
// 本用例在 `-race` + 真 PG 下出现锁等待超时或 40P01。
// TestStartRun_PinnedLockOrderNoDeadlock runs the pinned admission against the
// reclaimer's SKIP LOCKED claim under -race; it must never block or deadlock.
func TestStartRun_PinnedLockOrderNoDeadlock(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	res := seedPinResources(t, db, pinBundleHash, pinBundleHash)
	planned := opNow.Add(time.Minute)

	const rounds = 8
	for i := range rounds {
		taskID := fmt.Sprintf("pin-race-task-%02d", i)
		runID := fmt.Sprintf("scheduler-pin-race-%04d", i)
		seedPinTask(t, db, taskID, planned)
		// 租约已过期但尚未被接管：StartRun 会拒绝，回收器可领取——两条路径争同一行。
		execID, pin := seedPinClaimRecord(t, db, taskID, runID, pinClaimOwner, 1, opNow)
		rec := pinRunRecord(runID)
		rec.Pin = pin

		start := make(chan struct{})
		var (
			wg         sync.WaitGroup
			startErr   error
			reclaimErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			startErr = startRunTest(NewRunStore(db), ctx, rec, res.manifest)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, reclaimErr = NewReclaimerStore(db).ClaimExpired(ctx,
				core.LeaseScheduleExecution,
				core.WorkerIdentity{InstanceID: "worker-reclaimer"}, 10, workTTL)
		}()
		close(start)

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d: 钉住准入与回收器领取互相阻塞（锁序或 SKIP LOCKED 失效）", i)
		}

		// 过期租约在任何交错下都不得放行：死锁 / 超时不会被误认成这个哨兵错误。
		// An expired lease never admits a run; a deadlock would not match this sentinel.
		require.ErrorIs(t, startErr, core.ErrClaimPinInvalid,
			"round %d: 过期租约必须被拒（若为 deadlock / 超时则锁序被打破）：%v", i, startErr)
		require.NoError(t, reclaimErr)
		requireNoRunTrace(t, db, runID)

		// 两种合法交错：回收器领到本行（epoch=2，owner 换人），或本轮被 SKIP LOCKED
		// 跳过（仍 worker-a / 1）。终态必须与之一致。
		// Two legal interleavings: claimed (epoch 2) or skipped (still worker-a / 1).
		owner, epoch := pinScheduleLeaseState(t, db, execID)
		switch epoch {
		case 1:
			require.Equal(t, pinClaimOwner, owner)
		case 2:
			require.Equal(t, "worker-reclaimer", owner)
		default:
			t.Fatalf("round %d: 租约代次非法（%d）", i, epoch)
		}
	}
}

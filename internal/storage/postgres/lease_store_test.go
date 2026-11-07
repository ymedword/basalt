//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `LeaseStore` 的**有库**用例（US-24 T005）：领取 / 过期 / 接管 / 代次 /
// 释放 / 续约与 `FencedTx` 的两种获锁顺序。所有时间判定都经数据库时钟——测试里
// "让租约过期"一律用 SQL 把 `expires_at` 改到过去，不靠 sleep 等真实时钟，
// 避免慢机器上的偶发红。
// Database-backed acceptance for LeaseStore: acquire / expiry / takeover / epoch /
// release / renew and both fence lock orders. Expiry is forced via SQL, never by
// sleeping, so the suite stays deterministic on slow machines.

// leaseTestTTL 是普通用例的租约时长：足够长，不会被测试自身耗时撞到过期。
const leaseTestTTL = 30 * time.Second

// mustAcquire 领取租约并在失败时立即终止用例。
func mustAcquire(t *testing.T, store *LeaseStore, kind core.LeaseKind, resourceID, ownerID string) core.Lease {
	t.Helper()
	lease, err := store.Acquire(context.Background(), kind, resourceID,
		core.WorkerIdentity{InstanceID: ownerID, Name: "worker-" + ownerID}, leaseTestTTL)
	require.NoError(t, err)
	return lease
}

// expireLease 直接把租约改到过去。**只用于制造夹具**：生产路径的过期由数据库时钟
// 自然发生，回收与接管都按 `expires_at <= db_now` 判定。
func expireLease(t *testing.T, store *LeaseStore, kind core.LeaseKind, resourceID string) {
	t.Helper()
	_, err := store.db.pool.ExecContext(context.Background(), `
UPDATE execution_leases
   SET expires_at = (extract(epoch from clock_timestamp()) * 1000)::bigint - 1000
 WHERE kind = $1 AND resource_id = $2`, string(kind), resourceID)
	require.NoError(t, err)
}

// countLeaseEvents 数某一事件类型的行数（判"只记状态转换、不记每次续约"）。
func countLeaseEvents(t *testing.T, store *LeaseStore, kind core.LeaseKind, resourceID, event string) int {
	t.Helper()
	var n int
	require.NoError(t, store.db.pool.QueryRowContext(context.Background(), `
SELECT count(*) FROM lease_events WHERE kind = $1 AND resource_id = $2 AND event = $3`,
		string(kind), resourceID, event).Scan(&n))
	return n
}

// TestLease_AcquireTakeoverAndEpoch 判四种领取结局与代次单调：首次 claimed（epoch 1）、
// 活租约拒绝（ErrLeaseHeld，不产生事件）、释放后交棒（epoch 2、TookOver=false）、
// 过期后接管（epoch 3、TookOver=true）。
//
// 逆改点：把"未过期且未释放"分支改成直接接管 → 第二次 Acquire 不再返回 ErrLeaseHeld；
// 把 epoch 写成固定 1 → 代次断言红；把 released 也当接管（TookOver=true）→
// 交棒分支的 TookOver=false 断言红。
// Reverse-edit points: any of the four outcomes collapsing into another, or a frozen
// epoch, turns the matching assertion red.
func TestLease_AcquireTakeoverAndEpoch(t *testing.T) {
	store := NewLeaseStore(newTxTestDB(t))
	ctx := context.Background()
	const kind = core.LeaseFlowRoot
	const resourceID = "flow-root-acquire"

	first := mustAcquire(t, store, kind, resourceID, "worker-a")
	require.Equal(t, int64(1), first.OwnershipEpoch, "首次领取得到 epoch 1")
	require.False(t, first.TookOver, "首次领取不是接管")
	require.Equal(t, "worker-a", first.OwnerWorkerID)
	require.True(t, first.AcquiredAt.Equal(first.RenewedAt), "领取同时写 acquired_at 与 renewed_at")
	require.True(t, first.ExpiresAt.After(first.AcquiredAt), "expires_at 必须晚于领取时刻")
	require.Equal(t, 1, countLeaseEvents(t, store, kind, resourceID, "claimed"))
	require.Equal(t, 0, countLeaseEvents(t, store, kind, resourceID, "takeover"))

	// 活租约：不等待、不排队、不落事件。
	_, err := store.Acquire(ctx, kind, resourceID, core.WorkerIdentity{InstanceID: "worker-b"}, leaseTestTTL)
	require.ErrorIs(t, err, core.ErrLeaseHeld)
	require.Equal(t, 1, countLeaseEvents(t, store, kind, resourceID, "claimed"), "被拒的领取不得写事件")

	// 主动释放是**正常交棒**：代次递增，但 TookOver=false。
	require.NoError(t, store.Release(ctx, first.LeaseToken))
	require.NoError(t, store.Release(ctx, first.LeaseToken), "同 owner+epoch 的重复释放是幂等 no-op")
	require.Equal(t, 1, countLeaseEvents(t, store, kind, resourceID, "released"), "重复释放不得写第二条事件")

	second := mustAcquire(t, store, kind, resourceID, "worker-b")
	require.Equal(t, int64(2), second.OwnershipEpoch, "交棒后代次递增")
	require.False(t, second.TookOver, "前手主动释放是正常交棒，不是接管")

	// 过期接管：旧持有者可能还活着，TookOver=true 让 Flow 先 Recover 再 Advance。
	expireLease(t, store, kind, resourceID)
	third := mustAcquire(t, store, kind, resourceID, "worker-c")
	require.Equal(t, int64(3), third.OwnershipEpoch, "接管后代次继续递增")
	require.True(t, third.TookOver, "过期领取必须标记为接管")
	require.Equal(t, 1, countLeaseEvents(t, store, kind, resourceID, "takeover"))

	// 旧令牌在任何写面上都失权：续约与释放都拒绝，且不得释放新负责人的租约。
	_, err = store.Renew(ctx, second.LeaseToken, leaseTestTTL)
	require.ErrorIs(t, err, core.ErrLeaseLost)
	require.ErrorIs(t, store.Release(ctx, second.LeaseToken), core.ErrLeaseLost)

	var owner string
	require.NoError(t, store.db.pool.QueryRowContext(ctx,
		`SELECT owner_worker_id FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(kind), resourceID).Scan(&owner))
	require.Equal(t, "worker-c", owner, "旧令牌的释放不得改写新负责人")
}

// TestLease_RenewRequiresOwnerAndEpoch 判续约是**行级条件写**：owner / epoch 任一不符
// 即 0 行 → ErrLeaseLost；过期但尚未被接管时同样失权（FR-24-04）。续约不写事件。
//
// 逆改点：把 Renew 改成"先 SELECT 再 UPDATE"且不带条件 → 错 owner 与过期用例都会
// 通过（红态消失）；在续约成功路径写事件 → "不记每次续约"断言红。
// Reverse-edit points: dropping the conditional predicates, or logging every renewal
// as an event, each turns a named assertion red.
func TestLease_RenewRequiresOwnerAndEpoch(t *testing.T) {
	store := NewLeaseStore(newTxTestDB(t))
	ctx := context.Background()
	const kind = core.LeaseFlowRoot
	const resourceID = "flow-root-renew"

	lease := mustAcquire(t, store, kind, resourceID, "worker-a")

	wrongOwner := lease.LeaseToken
	wrongOwner.OwnerWorkerID = "worker-b"
	_, err := store.Renew(ctx, wrongOwner, leaseTestTTL)
	require.ErrorIs(t, err, core.ErrLeaseLost, "负责人不符必须失权")

	wrongEpoch := lease.LeaseToken
	wrongEpoch.OwnershipEpoch++
	_, err = store.Renew(ctx, wrongEpoch, leaseTestTTL)
	require.ErrorIs(t, err, core.ErrLeaseLost, "代次不符必须失权")

	renewed, err := store.Renew(ctx, lease.LeaseToken, leaseTestTTL)
	require.NoError(t, err)
	require.Equal(t, lease.OwnershipEpoch, renewed.OwnershipEpoch, "续约不改变代次")
	require.False(t, renewed.RenewedAt.Before(lease.RenewedAt), "续约刷新 renewed_at")
	require.False(t, renewed.ExpiresAt.Before(lease.ExpiresAt), "续约延长 expires_at")
	require.Equal(t, 0, countLeaseEvents(t, store, kind, resourceID, "renew_failed"),
		"成功续约不是状态转换，不写事件")

	// 过期后即使尚未被接管，原负责人也不得继续提交。
	expireLease(t, store, kind, resourceID)
	_, err = store.Renew(ctx, lease.LeaseToken, leaseTestTTL)
	require.ErrorIs(t, err, core.ErrLeaseLost, "已过期租约即使未被接管也必须拒绝续约")

	// 接管后旧令牌续约同样失权。
	taken := mustAcquire(t, store, kind, resourceID, "worker-b")
	require.True(t, taken.TookOver)
	_, err = store.Renew(ctx, lease.LeaseToken, leaseTestTTL)
	require.ErrorIs(t, err, core.ErrLeaseLost)
}

// TestLease_RecordRenewFailureCarriesOriginalOwner 判 keeper 的续约失败事件出口
// （US-24 §2.7 / 评审 IDX-25）：事件记的是**调用方当时的** owner / epoch，不读当前
// 行——失权后该行可能已被接管，读当前值会把失败记到新属主名下；detail 原样保留
// reason；同一租约可多次记录（每次失败都是一条观察，不做幂等合并）；未知 kind 与
// 空资源同样在入口 fail-closed。
//
// 逆改点：把 owner / epoch 改成从 execution_leases 现读 → 接管后的 owner 断言红
// （事件被记到 worker-b）；丢掉 detail → reason 断言红。
func TestLease_RecordRenewFailureCarriesOriginalOwner(t *testing.T) {
	store := NewLeaseStore(newTxTestDB(t))
	ctx := context.Background()
	const kind = core.LeaseRun
	const resourceID = "run-renew-failure"

	lease := mustAcquire(t, store, kind, resourceID, "worker-a")

	// 另一 Worker 接管：旧持有者观察到失权，事件必须记在 worker-a / epoch 1 名下。
	expireLease(t, store, kind, resourceID)
	taken := mustAcquire(t, store, kind, resourceID, "worker-b")
	require.True(t, taken.TookOver)
	require.Equal(t, int64(2), taken.OwnershipEpoch)

	require.NoError(t, store.RecordRenewFailure(ctx, lease.LeaseToken, "lease_lost"))
	require.NoError(t, store.RecordRenewFailure(ctx, lease.LeaseToken, "renew_deadline_exceeded"),
		"同一租约的第二次失败仍是一条独立观察")

	var (
		events int
		owner  string
		epoch  int64
		detail string
	)
	require.NoError(t, store.db.pool.QueryRowContext(ctx, `
SELECT count(*) FROM lease_events
 WHERE kind = $1 AND resource_id = $2 AND event = 'renew_failed'`,
		string(kind), resourceID).Scan(&events))
	require.Equal(t, 2, events)
	require.NoError(t, store.db.pool.QueryRowContext(ctx, `
SELECT owner_worker_id, ownership_epoch, detail FROM lease_events
 WHERE kind = $1 AND resource_id = $2 AND event = 'renew_failed'
 ORDER BY event_id DESC LIMIT 1`, string(kind), resourceID).Scan(&owner, &epoch, &detail))
	require.Equal(t, "worker-a", owner, "事件必须记旧持有者，不得读接管后的当前行")
	require.Equal(t, lease.OwnershipEpoch, epoch, "事件必须记调用方当时的代次")
	require.Equal(t, "renew_deadline_exceeded", detail)

	// 未知 kind / 空资源在入口拒绝，不落行。
	err := store.RecordRenewFailure(ctx, core.LeaseToken{Kind: "mystery", ResourceID: resourceID}, "lease_lost")
	require.Error(t, err)
	require.Contains(t, err.Error(), "未知的租约类型")
	require.Error(t, store.RecordRenewFailure(ctx, core.LeaseToken{Kind: kind}, "lease_lost"))
	require.Equal(t, 2, countLeaseEvents(t, store, kind, resourceID, "renew_failed"),
		"被拒的记录不得写事件")
}

// TestLease_UnknownKindRejected 判租约类型闭集在**入口**就 fail-closed：未知 kind
// 不落库、不建事件、不把零值当合法类处理。四个写面逐一验证。
//
// 逆改点：删掉 validateLeaseTarget → 未知 kind 会走到 SQL 的 CHECK（23514）或
// 空行判定，用例对"未落行 / 事件为零"的断言随之变红。
// Reverse-edit point: dropping the entry validation leaks unknown kinds into SQL.
func TestLease_UnknownKindRejected(t *testing.T) {
	store := NewLeaseStore(newTxTestDB(t))
	ctx := context.Background()
	const unknown core.LeaseKind = "mystery"
	const resourceID = "lease-unknown-kind"

	_, err := store.Acquire(ctx, unknown, resourceID, core.WorkerIdentity{InstanceID: "worker-a"}, leaseTestTTL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "未知的租约类型")

	_, err = store.Renew(ctx, core.LeaseToken{Kind: unknown, ResourceID: resourceID, OwnerWorkerID: "worker-a", OwnershipEpoch: 1}, leaseTestTTL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "未知的租约类型")

	err = store.Release(ctx, core.LeaseToken{Kind: unknown, ResourceID: resourceID, OwnerWorkerID: "worker-a", OwnershipEpoch: 1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "未知的租约类型")

	err = store.FencedTx(ctx, core.LeaseToken{Kind: unknown, ResourceID: resourceID, OwnerWorkerID: "worker-a", OwnershipEpoch: 1},
		func(context.Context, core.Tx) error { return nil })
	require.Error(t, err)
	require.Contains(t, err.Error(), "未知的租约类型")

	var rows int
	require.NoError(t, store.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM execution_leases WHERE resource_id = $1`, resourceID).Scan(&rows))
	require.Zero(t, rows, "未知 kind 不得落库")
	var events int
	require.NoError(t, store.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM lease_events WHERE resource_id = $1`, resourceID).Scan(&events))
	require.Zero(t, events, "未知 kind 不得写事件")
}

// TestFencedWrites_OldHolderBlocksTakeover 判争抢同一行锁的**第一种获锁顺序**：
// 旧持有者的 `FencedTx` 持锁期间，接管方的 `Acquire` 必须阻塞；旧写提交后接管才
// 发生（TookOver=true、代次递增），随后旧令牌的一切写入被 `ErrLeaseLost` 拒绝。
//
// 逆改点：把 `FencedTx` 的 `FOR UPDATE` 去掉、只做普通 SELECT → 接管方不再阻塞，
// "空窗内不得完成领取"的断言红；把 fence 校验换成 `EXISTS` 快照 → 旧令牌在接管后
// 仍能写入成功，末尾的 ErrLeaseLost 断言红。
// Reverse-edit points: removing FOR UPDATE lets the takeover finish during the fence;
// an EXISTS snapshot lets the stale token write after the takeover.
func TestFencedWrites_OldHolderBlocksTakeover(t *testing.T) {
	store := NewLeaseStore(newTxTestDB(t))
	ctx := context.Background()
	const kind = core.LeaseFlowRoot
	const resourceID = "flow-root-fence-order"

	oldHolder := mustAcquire(t, store, kind, resourceID, "worker-a")

	fenceEntered := make(chan struct{})
	fenceRelease := make(chan struct{})
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- store.FencedTx(ctx, oldHolder.LeaseToken, func(ctx context.Context, tx core.Tx) error {
			close(fenceEntered)
			<-fenceRelease
			// 持锁期间把租约改到过去：接管方拿到锁后必须看到"已过期"。
			_, err := tx.ExecContext(ctx, `
UPDATE execution_leases
   SET expires_at = (extract(epoch from clock_timestamp()) * 1000)::bigint - 1000
 WHERE kind = $1 AND resource_id = $2`, string(kind), resourceID)
			return err
		})
	}()
	<-fenceEntered

	type acquireResult struct {
		lease core.Lease
		err   error
	}
	takeover := make(chan acquireResult, 1)
	go func() {
		lease, err := store.Acquire(ctx, kind, resourceID,
			core.WorkerIdentity{InstanceID: "worker-b"}, leaseTestTTL)
		takeover <- acquireResult{lease: lease, err: err}
	}()

	// 旧持有者持锁期间，接管不得完成——行锁把它挡在 READ COMMITTED 的重新取行上。
	select {
	case got := <-takeover:
		t.Fatalf("旧持有者持锁时接管不得完成：lease=%+v err=%v", got.lease, got.err)
	case <-time.After(200 * time.Millisecond):
	}

	close(fenceRelease)
	require.NoError(t, <-fenceDone, "旧写本身必须成功提交（它发生在失权之前）")

	got := <-takeover
	require.NoError(t, got.err, "旧写提交后接管必须成功")
	require.True(t, got.lease.TookOver)
	require.Equal(t, int64(2), got.lease.OwnershipEpoch, "接管递增代次")

	// 接管提交后，旧令牌的一切写入都是零写入。
	err := store.FencedTx(ctx, oldHolder.LeaseToken, func(context.Context, core.Tx) error {
		t.Fatal("失权令牌的写入闭包不得执行")
		return nil
	})
	require.ErrorIs(t, err, core.ErrLeaseLost)
	require.ErrorIs(t, store.Release(ctx, oldHolder.LeaseToken), core.ErrLeaseLost)
}

// TestFencedTx_RejectsExpiredLeaseBeforeWrite 判 fence 的第三个条件：租约已过期
// （尚未被接管）时，写入闭包**一次都不执行**——过期即失权，不需要等接管。
//
// 逆改点：删掉 expires_at 条件 → 闭包被执行，本用例红。
// Reverse-edit point: dropping the expiry predicate lets the closure run.
func TestFencedTx_RejectsExpiredLeaseBeforeWrite(t *testing.T) {
	store := NewLeaseStore(newTxTestDB(t))
	ctx := context.Background()
	const kind = core.LeaseFlowRoot
	const resourceID = "flow-root-fence-expired"

	lease := mustAcquire(t, store, kind, resourceID, "worker-a")
	expireLease(t, store, kind, resourceID)

	called := false
	err := store.FencedTx(ctx, lease.LeaseToken, func(context.Context, core.Tx) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, core.ErrLeaseLost)
	require.False(t, called, "失权判定必须先于写入闭包")

	// 守卫有效：库里该行仍是同一持有者，未被改写。
	var (
		owner string
		epoch int64
	)
	require.NoError(t, store.db.pool.QueryRowContext(ctx,
		`SELECT owner_worker_id, ownership_epoch FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
		string(kind), resourceID).Scan(&owner, &epoch))
	require.Equal(t, "worker-a", owner)
	require.Equal(t, int64(1), epoch)
}

// TestLease_AcquireRejectsMissingIdentity 判领取者身份必填：空 InstanceID 不落库。
// Name 可以重复（那是诊断字段），InstanceID 空则无法建立归属。
func TestLease_AcquireRejectsMissingIdentity(t *testing.T) {
	store := NewLeaseStore(newTxTestDB(t))
	_, err := store.Acquire(context.Background(), core.LeaseFlowRoot, "flow-root-no-owner",
		core.WorkerIdentity{Name: "duplicate-name"}, leaseTestTTL)
	require.Error(t, err)
	require.NotErrorIs(t, err, core.ErrLeaseHeld)

	var rows int
	require.NoError(t, store.db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM execution_leases WHERE resource_id = $1`, "flow-root-no-owner").Scan(&rows))
	require.Zero(t, rows)
}

// TestLease_RunKindReturnsCancelFlag 判 run 类型租约把 `runs.cancel_requested` 随
// 续约响应带回（FR-24-22 的观察通道）；其它类型不读该列。
func TestLease_RunKindReturnsCancelFlag(t *testing.T) {
	db := newTxTestDB(t)
	store := NewLeaseStore(db)
	ctx := context.Background()

	const runID = "run-cancel-flag"
	_, err := db.pool.ExecContext(ctx, `
INSERT INTO runs(run_id, agent, channel, user_id, trigger, status, iterations, tool_calls,
                 started_at, session_id, cancel_requested, caller_id, authorizing_kind, actor)
VALUES ($1, 'reader', 'web', 'original-user', 'user', 'running', 1, 1, 1000, NULL, true,
        'local-admin', 'admin-builtin', 'admin-cli')`, runID)
	require.NoError(t, err)

	lease := mustAcquire(t, store, core.LeaseRun, runID, "worker-a")
	require.True(t, lease.CancelRequested, "领取时必须带回已存在的取消请求")

	// 他处写入标志后，续约响应必须观察到。
	_, err = db.pool.ExecContext(ctx, `UPDATE runs SET cancel_requested = false WHERE run_id = $1`, runID)
	require.NoError(t, err)
	renewed, err := store.Renew(ctx, lease.LeaseToken, leaseTestTTL)
	require.NoError(t, err)
	require.False(t, renewed.CancelRequested)

	_, err = db.pool.ExecContext(ctx, `UPDATE runs SET cancel_requested = true WHERE run_id = $1`, runID)
	require.NoError(t, err)
	renewed, err = store.Renew(ctx, lease.LeaseToken, leaseTestTTL)
	require.NoError(t, err)
	require.True(t, renewed.CancelRequested, "续约响应必须带回他处的取消请求")
}

// 编译期提醒：lease_store_test 依赖 tx_integration_test.go 的 newTxTestDB（同包、
// 同 build tag）。若该助手改名或挪包，本文件必须同步，否则集成构建直接失败。

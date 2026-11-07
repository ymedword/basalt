package core

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 验收点（T004）：租约类型闭集 fail-closed——三个存储值逐个解析成功，空串 /
// 未知值 / 大小写变体 / 前导空格一律拒绝；归属凭证是**可比较的显式值**，
// 参与校验的只有 Kind + ResourceID + OwnerWorkerID + OwnershipEpoch。
// Acceptance points (T004): the lease-kind domain fails closed, and the ownership
// token is a comparable explicit value whose identity is exactly its four fields.

func TestLease_UnknownKindRejected(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"run", "schedule_execution", "flow_root"} {
		kind, err := ParseLeaseKind(raw)
		require.NoErrorf(t, err, "%q 是合法租约类型", raw)
		require.Equal(t, LeaseKind(raw), kind)
	}

	// 逆改点：把 default 分支改成"接受任意值"或放宽成大小写不敏感，
	// 下列每个用例都会红——闭集就不再有判别力。
	// Reverse-edit point: widening the default branch or accepting case variants
	// turns every case below red.
	for _, raw := range []string{"", "RUN", "Run", " run", "run ", "resource", "agent_bundle", "flow"} {
		kind, err := ParseLeaseKind(raw)
		require.Errorf(t, err, "%q 必须被拒绝", raw)
		require.Empty(t, kind)
		require.NotErrorIs(t, err, ErrLeaseLost)
		require.NotErrorIs(t, err, ErrLeaseHeld)
	}
}

func TestLeaseToken_EqualityAndKindRoundTrip(t *testing.T) {
	t.Parallel()

	token := LeaseToken{
		Kind:           LeaseRun,
		ResourceID:     "run-01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
		OwnerWorkerID:  "w-01J8Z6YQ4M2S3T7V9W0X1Y2Z3B",
		OwnershipEpoch: 7,
	}
	same := token
	require.Equal(t, token, same, "token 是值：逐字段相等即可比较")
	require.True(t, token == same, "token 必须可比较（不用 map / 反射判等）")

	// 四个字段**每个**都参与归属身份：任一不同即不同 token。
	// 这是 FencedTx 判据的数据基础——少一个字段就会把旧持有者判成新持有者。
	different := []LeaseToken{
		{Kind: LeaseScheduleExecution, ResourceID: token.ResourceID, OwnerWorkerID: token.OwnerWorkerID, OwnershipEpoch: token.OwnershipEpoch},
		{Kind: token.Kind, ResourceID: "run-other", OwnerWorkerID: token.OwnerWorkerID, OwnershipEpoch: token.OwnershipEpoch},
		{Kind: token.Kind, ResourceID: token.ResourceID, OwnerWorkerID: "w-other", OwnershipEpoch: token.OwnershipEpoch},
		{Kind: token.Kind, ResourceID: token.ResourceID, OwnerWorkerID: token.OwnerWorkerID, OwnershipEpoch: token.OwnershipEpoch + 1},
	}
	for i, other := range different {
		require.NotEqualf(t, token, other, "第 %d 个字段必须参与归属判等", i+1)
	}

	// TookOver 是领取结论，不在 token 里；Lease 内嵌 token，可原样取出交给
	// Renew / Release / FencedTx。这里钉住"取内嵌凭证"的写法，防止后续把
	// TookOver 塞进 token 或让 Lease 与 token 脱钩。
	lease := Lease{
		LeaseToken: token,
		AcquiredAt: time.UnixMilli(1000),
		RenewedAt:  time.UnixMilli(2000),
		ExpiresAt:  time.UnixMilli(3000),
		TookOver:   true,
	}
	require.Equal(t, token, lease.LeaseToken)
	require.NotEqual(t, lease.LeaseToken, LeaseToken{}, "非零 token 不得退化为零值")

	// 解析器是唯一入口：Lease 带回的 kind 必须能原样过闭集校验。
	_, err := ParseLeaseKind(string(lease.Kind))
	require.NoError(t, err)

	// 七个哨兵必须彼此可区分——调用方按 errors.Is 分派收敛路径
	// （失权停止 / 续约超限 / 他处取消 / 并发限流 / 重复领取 / CAS 失败）。
	sentinels := []error{
		ErrLeaseHeld,
		ErrLeaseLost,
		ErrLeaseRenewDeadline,
		ErrCancelRequested,
		ErrCallerConcurrencyLimited,
		ErrTriggerAlreadyClaimed,
		ErrStaleNextFire,
	}
	for i, first := range sentinels {
		require.Error(t, first)
		for j, second := range sentinels {
			if i == j {
				require.ErrorIs(t, first, second)
				continue
			}
			require.Falsef(t, errors.Is(first, second), "哨兵 %d 与 %d 必须可区分", i, j)
		}
	}
}

// 以下是 `KeepLease`（§2.3.3 冻结伪代码）的判据（T007 / FR-24-35）。用例全部用
// 真实时钟跑短预算：阈值是 `ttl - renewInterval`，把它做成确定性注入会掩盖
// "成功续约刷新计时"这一条，而它正是"正常 Run 被误停"的分界。
// Acceptance for the lease keeper (T007 / FR-24-35). The tests run short real-time
// budgets on purpose: injecting a fake clock would hide the success-refresh rule.

// keeperFakeStore 是 keeper 用例的端口替身：只有 Renew / Release 会被调用，
// Acquire / FencedTx 被调用即说明 keeper 走出了伪代码的范围。
type keeperFakeStore struct {
	renew   func(call int) (Lease, error)
	release func(ctx context.Context, token LeaseToken) error
	calls   atomic.Int32

	releaseCalls       int
	releaseDeadline    time.Time
	releaseHasDeadline bool
}

func (f *keeperFakeStore) Acquire(context.Context, LeaseKind, string, WorkerIdentity, time.Duration) (Lease, error) {
	return Lease{}, errors.New("keeper 不得调用 Acquire")
}

func (f *keeperFakeStore) Renew(_ context.Context, _ LeaseToken, _ time.Duration) (Lease, error) {
	call := int(f.calls.Add(1))
	return f.renew(call)
}

func (f *keeperFakeStore) Release(ctx context.Context, token LeaseToken) error {
	f.releaseCalls++
	f.releaseDeadline, f.releaseHasDeadline = ctx.Deadline()
	if f.release != nil {
		return f.release(ctx, token)
	}
	return nil
}

func (f *keeperFakeStore) FencedTx(context.Context, LeaseToken, func(context.Context, Tx) error) error {
	return errors.New("keeper 不得调用 FencedTx")
}

// keeperTestToken 是一个普通 run 租约凭证。
func keeperTestToken() LeaseToken {
	return LeaseToken{Kind: LeaseRun, ResourceID: "run-keeper-test", OwnerWorkerID: "wrk_keeper", OwnershipEpoch: 3}
}

// discardLogger 让失败重试路径（ERROR 级别）不污染测试输出。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestKeepLease_RenewDeadlineStopsExecution 判续约**有界**：Renew 持续返回瞬时
// 网络错误时，keeper 必须重试到 `ttl - renewInterval` 才以 ErrLeaseRenewDeadline
// 取消执行上下文，而不是一次失败就停、也不是无限重试。
//
// 逆改点：把失败分支改成 `continue` 无限重试 → ctx 永不取消，用例超时；
// 把阈值改成一次失败即停 → 重试次数断言红；成功续约也计入失败
// （`TestKeepLease_SuccessRefreshesDeadline`）→ 正常 Run 被误停。
// Reverse-edit points: unbounded retry (never cancels), stop-on-first-failure, or
// counting successful renewals as failures all turn the matching assertion red.
func TestKeepLease_RenewDeadlineStopsExecution(t *testing.T) {
	t.Parallel()

	const (
		ttl           = 2 * time.Second
		renewInterval = 100 * time.Millisecond
	)
	store := &keeperFakeStore{renew: func(int) (Lease, error) {
		return Lease{}, errors.New("模拟数据库连接中断")
	}}
	start := time.Now()
	leaseCtx, stop, _ := KeepLease(context.Background(), store, keeperTestToken(),
		renewInterval, ttl, discardLogger())
	defer stop(context.Background())

	select {
	case <-leaseCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("续约持续失败后 ctx 未被取消（调用 %d 次）——keeper 退化成无限重试",
			store.calls.Load())
	}
	elapsed := time.Since(start)
	require.Equal(t, ErrLeaseRenewDeadline, context.Cause(leaseCtx),
		"有界停止的取消原因必须是 ErrLeaseRenewDeadline")
	require.Greater(t, int(store.calls.Load()), 1, "阈值前必须重试，不是一次失败就停")
	// 下界是「有界」的一半语义（评审 IDX-27）：阈值是 `ttl - renewInterval`，
	// 不是一次失败即停（那会在毫秒级取消），也不是等满 ttl。计时从 KeepLease
	// 调用前开始，keeper 内部的 `lastRenewOK` 只会更晚，因此该下界不需要容差。
	// The lower bound pins the threshold `ttl - renewInterval`: stopping on the
	// first failure would cancel far earlier than this.
	require.GreaterOrEqual(t, elapsed, ttl-renewInterval,
		"有界停止过早（用了 %s）：阈值必须是 ttl - renewInterval = %s", elapsed, ttl-renewInterval)
}

// TestKeepLease_SuccessRefreshesDeadline 判**成功续约刷新计时**：前两次失败后
// 持续成功，运行时间远超 `ttl - renewInterval` 也不得取消——把成功也算进
// "连续失败"会让正常 Run 被误停（FR-24-35 的反向判据）。
func TestKeepLease_SuccessRefreshesDeadline(t *testing.T) {
	t.Parallel()

	const (
		ttl           = 600 * time.Millisecond
		renewInterval = 20 * time.Millisecond
	)
	store := &keeperFakeStore{renew: func(call int) (Lease, error) {
		if call <= 2 {
			return Lease{}, errors.New("模拟瞬时抖动")
		}
		return Lease{LeaseToken: keeperTestToken()}, nil
	}}
	leaseCtx, stop, abandon := KeepLease(context.Background(), store, keeperTestToken(),
		renewInterval, ttl, discardLogger())

	time.Sleep(2 * ttl) // 远超 580ms 的失败阈值
	require.NoError(t, leaseCtx.Err(), "成功续约必须刷新 lastRenewOK，不得误停")
	require.Greater(t, int(store.calls.Load()), 3, "循环必须一直在续约")

	abandon()
	require.ErrorIs(t, leaseCtx.Err(), context.Canceled)
	stop(context.Background()) // 收尾等 keeper 退出，避免测试留下 goroutine
}

// TestKeepLease_CancelRequestedStopsExecution 判他处取消：Renew 成功但带回
// CancelRequested 时，keeper 以 ErrCancelRequested 取消执行上下文并退出。
// 它**不是** error 分支——取消请求随成功续约带回（§2.3.3 R6 修正）。
func TestKeepLease_CancelRequestedStopsExecution(t *testing.T) {
	t.Parallel()

	store := &keeperFakeStore{renew: func(int) (Lease, error) {
		return Lease{LeaseToken: keeperTestToken(), CancelRequested: true}, nil
	}}
	leaseCtx, stop, _ := KeepLease(context.Background(), store, keeperTestToken(),
		10*time.Millisecond, 5*time.Second, discardLogger())
	defer stop(context.Background())

	select {
	case <-leaseCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("续约带回取消请求后 ctx 未被取消")
	}
	require.Equal(t, ErrCancelRequested, context.Cause(leaseCtx))
}

// TestKeepLease_LostStopsImmediately 判失权优先于失败阈值：Renew 返回
// ErrLeaseLost 时必须立即取消，不等到 `ttl - renewInterval`（这期间它已经不再
// 持有执行权，继续跑就是双写）。
func TestKeepLease_LostStopsImmediately(t *testing.T) {
	t.Parallel()

	store := &keeperFakeStore{renew: func(int) (Lease, error) {
		return Lease{}, ErrLeaseLost
	}}
	leaseCtx, stop, _ := KeepLease(context.Background(), store, keeperTestToken(),
		10*time.Millisecond, 10*time.Second, discardLogger())
	defer stop(context.Background())

	select {
	case <-leaseCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("失权后 ctx 未立即取消")
	}
	require.Equal(t, ErrLeaseLost, context.Cause(leaseCtx))
}

// keeperRecorderStore 在 keeperFakeStore 之上实现可选的续约失败事件出口。
type keeperRecorderStore struct {
	*keeperFakeStore
	err      error
	recorded chan string
}

func (f *keeperRecorderStore) RecordRenewFailure(_ context.Context, _ LeaseToken, reason string) error {
	if f.err != nil {
		return f.err
	}
	f.recorded <- reason
	return nil
}

// TestKeepLease_RecordsRenewFailureEvent 判续约失败的**事件出口**（US-24 §2.7 /
// 评审 IDX-25）：失权与有界停止各写一次 `renew_failed`，reason 区分两个终态；
// 成功续约带回的取消请求不写；事件写失败不改变停止语义（取消原因照旧、不延迟）。
//
// 逆改点：删掉 recordRenewFailure 调用 → 两条正例在超时内收不到 reason；把事件写
// 失败当成停止失败或挡在取消前面 → 失败注入子用例超时 / 取消原因变红。
func TestKeepLease_RecordsRenewFailureEvent(t *testing.T) {
	t.Parallel()

	newStore := func(renew func(int) (Lease, error)) *keeperRecorderStore {
		return &keeperRecorderStore{
			keeperFakeStore: &keeperFakeStore{renew: renew},
			recorded:        make(chan string, 4),
		}
	}
	waitReason := func(t *testing.T, store *keeperRecorderStore) string {
		t.Helper()
		select {
		case reason := <-store.recorded:
			return reason
		case <-time.After(2 * time.Second):
			t.Fatal("续约失败终态必须补一行 renew_failed 事件")
			return ""
		}
	}

	t.Run("失权", func(t *testing.T) {
		t.Parallel()
		store := newStore(func(int) (Lease, error) { return Lease{}, ErrLeaseLost })
		leaseCtx, stop, _ := KeepLease(context.Background(), store, keeperTestToken(),
			10*time.Millisecond, 10*time.Second, discardLogger())
		defer stop(context.Background())

		require.Equal(t, renewFailureLeaseLost, waitReason(t, store))
		require.Equal(t, ErrLeaseLost, context.Cause(leaseCtx))
	})

	t.Run("有界停止", func(t *testing.T) {
		t.Parallel()
		store := newStore(func(int) (Lease, error) { return Lease{}, errors.New("模拟数据库连接中断") })
		leaseCtx, stop, _ := KeepLease(context.Background(), store, keeperTestToken(),
			20*time.Millisecond, 100*time.Millisecond, discardLogger())
		defer stop(context.Background())

		require.Equal(t, renewFailureDeadlineExceeded, waitReason(t, store))
		require.Equal(t, ErrLeaseRenewDeadline, context.Cause(leaseCtx))
	})

	t.Run("他处取消不写", func(t *testing.T) {
		t.Parallel()
		store := newStore(func(int) (Lease, error) {
			return Lease{LeaseToken: keeperTestToken(), CancelRequested: true}, nil
		})
		leaseCtx, stop, _ := KeepLease(context.Background(), store, keeperTestToken(),
			10*time.Millisecond, 5*time.Second, discardLogger())
		defer stop(context.Background())

		<-leaseCtx.Done()
		require.Equal(t, ErrCancelRequested, context.Cause(leaseCtx))
		select {
		case reason := <-store.recorded:
			t.Fatalf("取消请求不是续约失败，禁止写 renew_failed（收到 %q）", reason)
		default:
		}
	})

	t.Run("事件写失败不改变停止", func(t *testing.T) {
		t.Parallel()
		store := newStore(func(int) (Lease, error) { return Lease{}, ErrLeaseLost })
		store.err = errors.New("事件表不可写")
		leaseCtx, stop, _ := KeepLease(context.Background(), store, keeperTestToken(),
			10*time.Millisecond, 10*time.Second, discardLogger())
		defer stop(context.Background())

		select {
		case <-leaseCtx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("事件写失败不得挡在停止前面")
		}
		require.Equal(t, ErrLeaseLost, context.Cause(leaseCtx))
	})
}

// TestKeepLease_StopReleasesWithinShutdownBudget 判 stop 的释放语义：等 keeper
// 退出后才 Release，且 Release 收到的是**调用方给出的剩余停机预算 ctx**——
// 它必须仍带 deadline（套 `WithoutCancel` 会把 deadline 剥掉，等于无界等待）。
func TestKeepLease_StopReleasesWithinShutdownBudget(t *testing.T) {
	t.Parallel()

	store := &keeperFakeStore{renew: func(int) (Lease, error) {
		return Lease{LeaseToken: keeperTestToken()}, nil
	}}
	_, stop, _ := KeepLease(context.Background(), store, keeperTestToken(),
		10*time.Millisecond, 5*time.Second, discardLogger())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stop(shutdownCtx)

	require.Equal(t, 1, store.releaseCalls, "stop 必须在 keeper 退出后释放一次")
	require.True(t, store.releaseHasDeadline,
		"释放必须用调用方的预算 ctx——没有 deadline 说明被 WithoutCancel 剥掉了")
}

// TestKeepLease_StopSkipsReleaseWhenKeeperStuck 判"等不到 keeper 退出就不释放"：
// 续约仍阻塞时 Release 会与新属主并发，宁交 TTL 回收也不冒险。
func TestKeepLease_StopSkipsReleaseWhenKeeperStuck(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 1)
	unblock := make(chan struct{})
	store := &keeperFakeStore{renew: func(int) (Lease, error) {
		select {
		case entered <- struct{}{}: // 首次阻塞点，供用例确认 keeper 已在 Renew 里
		default:
		}
		<-unblock
		return Lease{}, errors.New("解除阻塞")
	}}
	_, stop, _ := KeepLease(context.Background(), store, keeperTestToken(),
		10*time.Millisecond, 5*time.Second, discardLogger())
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("keeper 未进入续约调用")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	stop(shutdownCtx)

	require.Zero(t, store.releaseCalls, "keeper 未退出时不得释放租约")
	close(unblock)
	time.Sleep(50 * time.Millisecond) // 让 keeper 走完最后一步，测试不遗留 goroutine
}

//go:build integration

package app

// 本文件是 US-24 T026 的停机编排验收面（模块卡 §4.4.8 / FR-24-27 / FR-24-40 / C-24）。
//
// 判据全部落在**可观测副作用**上，而不是"函数被调用过"：
//   - 停止领取之后领取计数不再增长（不是"stopClaims 被调"）；
//   - 释放时收尾 goroutine 确已退出（释放晚于 join）；
//   - 排空期间租约的 renewed_at 持续推进、released_at 仍为空（续约没停）；
//   - 超界时注入的 exit 被调用且 closeStore 一次都没执行（不关库）；
//   - join 的总耗时受剩余 grace 约束，而不是再等一份完整 cancel_wait。
//
// 逆改点（红态口径见 run-state）：
//   - 把 releaseLeases 提到 join 之前 → ① 红；
//   - 用 `time.Now()+cancelWait+5s` 取代 `min(..., signal+grace)` → ⑤ 超时判据红；
//   - 在超界分支里"先关库再退出" → ⑤ 的 closeStore 计数红；
//   - 交互取槽改成阻塞 → ④ 的耗时判据红。

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// TestShutdown_StopsClaimsReleasesLeasesBeforeClose 判图 11 的次序：
// 停止领取 → 收尾退出 → 释放 → 关闭数据库。
// TestShutdown_StopsClaimsReleasesLeasesBeforeClose asserts the phase order of
// figure 11 and that no claim happens after the claim loop is stopped.
func TestShutdown_StopsClaimsReleasesLeasesBeforeClose(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	// 模拟领取循环：停止之后**必须**不再产生领取。计数在停止时快照，结束时比对。
	var claims atomic.Int64
	var claimsAtStop int64
	stopCh := make(chan struct{})
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				claims.Add(1)
			}
		}
	}()

	stopped, joined, released, storeClosed := false, false, false, false

	coord := newShutdownCoordinator(500*time.Millisecond, 400*time.Millisecond, 100*time.Millisecond, shutdownTestLogger())
	coord.exit = func(code int) {
		t.Errorf("join 在上界内完成时不得退出进程，实际 exit(%d)", code)
	}

	err := coord.run(shutdownSteps{
		stopClaims: func() {
			close(stopCh)
			<-loopDone
			claimsAtStop = claims.Load()
			record("stop_claims")
			stopped = true
		},
		join: func(context.Context) error {
			require.True(t, stopped, "join 之前必须已经停止领取")
			// 排空窗口：若领取没停，这 30ms 里会多出几十条新领取。
			time.Sleep(30 * time.Millisecond)
			record("join")
			joined = true
			return nil
		},
		releaseLeases: func(context.Context) error {
			require.True(t, joined, "释放必须晚于收尾退出")
			require.False(t, storeClosed, "释放必须在关闭数据库之前")
			record("release")
			released = true
			return nil
		},
		stopWorker: func(context.Context) error {
			require.True(t, released, "Worker 下线标记在释放之后")
			record("worker_stopped")
			return nil
		},
		closeObservability: func() error {
			require.True(t, joined, "观测面在收尾退出之后关闭")
			record("observability_closed")
			return nil
		},
		closeMCP: func() error {
			require.False(t, storeClosed, "MCP 必须在关闭数据库之前收起")
			record("mcp_closed")
			return nil
		},
		closeStore: func() error {
			require.True(t, released, "关闭数据库是最后一步，必须在释放之后")
			record("store_closed")
			storeClosed = true
			return nil
		},
		releaseLock: func() error {
			require.True(t, storeClosed, "实例锁在库关闭之后才松开")
			record("lock_released")
			return nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{
		"stop_claims", "join", "observability_closed", "release",
		"worker_stopped", "mcp_closed", "store_closed", "lock_released",
	}, order, "停机次序必须逐段一致")
	require.Equal(t, claimsAtStop, claims.Load(), "停止领取之后不得再有任何新领取")
}

// TestShutdown_RenewsDuringDrainAndJoinsBeforeRelease 是 C-24 的核心守点：
// 排空期间续约必须继续（否则租约过期被他人接管），且释放只能发生在收尾退出之后。
// It proves renewals keep running during the drain and that release happens only after
// the execution has actually exited.
func TestShutdown_RenewsDuringDrainAndJoinsBeforeRelease(t *testing.T) {
	f := newWireFixture(t)
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Config{DSN: f.dsn})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close(), "重复关闭是幂等的") })

	leases := postgres.NewLeaseStore(db)
	identity := core.WorkerIdentity{InstanceID: "shutdown-renew-worker", Name: "shutdown-renew"}
	lease, err := leases.Acquire(ctx, core.LeaseRun, "run_shutdown_renew", identity, 30*time.Second)
	require.NoError(t, err)

	_, stop, _ := core.KeepLease(ctx, leases, lease.LeaseToken, 15*time.Millisecond, 30*time.Second, f.logger)

	readRow := func() (int64, sql.NullInt64) {
		var renewed int64
		var released sql.NullInt64
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT renewed_at, released_at FROM execution_leases WHERE kind = $1 AND resource_id = $2`,
			string(core.LeaseRun), "run_shutdown_renew").Scan(&renewed, &released))
		return renewed, released
	}
	renewedBefore, releasedBefore := readRow()
	require.False(t, releasedBefore.Valid, "领取之后不应处于已释放状态")

	joined := make(chan struct{})
	var renewedDuringDrain int64
	var releasedDuringDrain bool

	coord := newShutdownCoordinator(2*time.Second, time.Second, 500*time.Millisecond, f.logger)
	coord.exit = func(code int) {
		t.Errorf("join 在上界内完成时不得退出进程，实际 exit(%d)", code)
	}

	err = coord.run(shutdownSteps{
		stopClaims: func() {},
		join: func(joinCtx context.Context) error {
			// 模拟一条执行在排空窗口里收尾：120ms 后才退出。期间 keeper 必须仍在续约。
			time.Sleep(120 * time.Millisecond)
			var released sql.NullInt64
			renewedDuringDrain, released = readRow()
			releasedDuringDrain = released.Valid
			// 收尾的最后一步才是交还租约（与生产路径同口径：带上剩余停机预算）。
			stop(joinCtx)
			close(joined)
			return nil
		},
		releaseLeases: func(context.Context) error {
			select {
			case <-joined:
				return nil
			default:
				return errors.New("释放阶段到达时收尾尚未退出")
			}
		},
		closeStore: func() error { return nil },
	})
	require.NoError(t, err)
	require.False(t, releasedDuringDrain, "排空期间不得释放租约")
	require.Greater(t, renewedDuringDrain, renewedBefore, "排空期间必须继续续约")

	_, releasedAfter := readRow()
	require.True(t, releasedAfter.Valid, "收尾退出后必须已按 token 释放")
}

// TestShutdown_InteractiveRunSharesInflightBudget 判三条执行路径共用同一个池化名额：
// 交互 Run 占满之后，后台路径（调度执行组 / Flow 节点）只能等待同一个池。
// It asserts the three execution paths share one pooled budget.
func TestShutdown_InteractiveRunSharesInflightBudget(t *testing.T) {
	budget := newInflightLimiter(2)
	require.NotNil(t, budget)

	releaseFirst, err := budget.acquireInteractive()
	require.NoError(t, err)
	releaseSecond, err := budget.acquireInteractive()
	require.NoError(t, err)

	// 第三条交互 Run：满额 → InstanceFullError（HTTP 429 TOO_MANY_RUNS），不排队。
	var full *core.InstanceFullError
	_, err = budget.acquireInteractive()
	require.ErrorAs(t, err, &full)
	require.Equal(t, 2, full.Limit)

	// 后台路径用的是**同一个**池：名额未恢复前必须阻塞。
	acquired := make(chan struct{})
	go func() {
		release, err := budget.acquireBackground(context.Background())
		if err != nil {
			return
		}
		close(acquired)
		release()
	}()
	select {
	case <-acquired:
		t.Fatal("满额时后台路径必须阻塞等待同一个池，不得绕过")
	case <-time.After(50 * time.Millisecond):
	}

	releaseFirst()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("归还名额后后台路径必须拿到槽")
	}
	releaseSecond()

	// 后台取槽可被 ctx 取消；drain 之后排队者立刻退出，不等到宽限耗尽。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = budget.acquireBackground(cancelled)
	require.ErrorIs(t, err, context.Canceled)

	// 用一个独立的一名额池判 drain：避免与上面归还时序纠缠，读数只反映排空入口。
	blocked := newInflightLimiter(1)
	held, err := blocked.acquireInteractive()
	require.NoError(t, err)
	defer held()
	waiting := make(chan error, 1)
	go func() {
		_, err := blocked.acquireBackground(context.Background())
		waiting <- err
	}()
	select {
	case <-waiting:
		t.Fatal("满额时应阻塞")
	case <-time.After(20 * time.Millisecond):
	}
	blocked.drain()
	select {
	case err := <-waiting:
		require.ErrorIs(t, err, errShutdownDraining)
	case <-time.After(time.Second):
		t.Fatal("drain 之后排队中的后台执行必须立刻退出")
	}
}

// TestShutdown_InteractiveRejectedImmediatelyWhenFull 判交互 Run 不排队：满额立即
// TOO_MANY_RUNS，且拒绝本身不耗时（没有等待窗口）。
// It asserts an interactive run is rejected immediately rather than queued.
func TestShutdown_InteractiveRejectedImmediatelyWhenFull(t *testing.T) {
	budget := newInflightLimiter(1)
	release, err := budget.acquireInteractive()
	require.NoError(t, err)
	require.NotNil(t, release)
	defer release()

	start := time.Now()
	_, err = budget.acquireInteractive()
	elapsed := time.Since(start)

	var full *core.InstanceFullError
	require.ErrorAs(t, err, &full)
	require.Equal(t, 1, full.Limit)
	require.Less(t, elapsed, 50*time.Millisecond, "满额必须立即拒绝，不得排队等待名额")
	require.GreaterOrEqual(t, elapsed, time.Duration(0))

	// 生产入口同样生效：`runAdmission.run` 在调用 next 之前取槽，满额时不进 Run。
	admission := &runAdmission{inflight: budget}
	var nextCalled atomic.Bool
	_, err = admission.run(context.Background(),
		core.RunRequest{RunID: "run_shutdown_full", Channel: "cli"},
		func(context.Context, core.RunRequest) (core.RunResult, error) {
			nextCalled.Store(true)
			return core.RunResult{}, nil
		})
	require.ErrorAs(t, err, &full)
	require.False(t, nextCalled.Load(), "满额被拒的 Run 不得进入执行入口（零占用）")
}

// TestShutdown_JoinBoundUsesRemainingGrace 判 [5] 的上界：
//
//	min(now + flows.cancel_wait + 5s, signal + worker.shutdown_grace)
//
// 用例把 cancel_wait 设成 5s、worker.shutdown_grace 设成 300ms：若实现"取消后再等
// 一份完整 cancel_wait"，总耗时会超过 5s；正确实现应在 signal+300ms 处直接退出，
// 且**不得**关闭数据库。
// It asserts the join reuses the remaining grace instead of a fresh cancel_wait window,
// and that an overrun exits without closing the store.
func TestShutdown_JoinBoundUsesRemainingGrace(t *testing.T) {
	exited := make(chan int, 1)
	var storeCloses atomic.Int64

	coord := newShutdownCoordinator(300*time.Millisecond, 120*time.Millisecond, 5*time.Second, shutdownTestLogger())
	coord.exit = func(code int) { exited <- code }
	coord.signalAt.Store(time.Now().UnixMilli())

	joinCalls := 0
	start := time.Now()
	err := coord.run(shutdownSteps{
		join: func(joinCtx context.Context) error {
			joinCalls++
			<-joinCtx.Done()
			return joinCtx.Err()
		},
		releaseLeases: func(context.Context) error { return nil },
		closeStore:    func() error { storeCloses.Add(1); return nil },
	})
	elapsed := time.Since(start)

	require.ErrorIs(t, err, errShutdownIncomplete)
	select {
	case code := <-exited:
		require.Equal(t, shutdownIncompleteExitCode, code, "超界必须以失败退出码结束进程")
	default:
		t.Fatal("超过 worker.shutdown_grace_seconds 必须直接退出进程")
	}
	require.Zero(t, storeCloses.Load(), "超界分支不得关闭数据库")
	require.Equal(t, 2, joinCalls, "两段预算各 join 一次（排空窗口 + 取消后窗口）")
	require.GreaterOrEqual(t, elapsed, 250*time.Millisecond, "必须先用满排空窗口")
	require.Less(t, elapsed, 2*time.Second,
		"join 必须使用剩余 grace 预算，而不是再等一份完整 cancel_wait(5s)")
}

// TestShutdown_CancelsInFlightAtDrainDeadline 判 [4] 的"超时 → cancel()"：
// 排空窗口到点**先**发取消、再进 [5] 的 join；取消早于排空窗口或从不发生都红。
// 逆改点：删掉 cancelExecutions 调用 → join 永远拿不到取消信号，本用例红。
// It asserts the drain-time cancel fires exactly once, only after the drain window.
func TestShutdown_CancelsInFlightAtDrainDeadline(t *testing.T) {
	coord := newShutdownCoordinator(2*time.Second, 120*time.Millisecond, 2*time.Second, shutdownTestLogger())
	coord.exit = func(int) {}
	coord.signalAt.Store(time.Now().UnixMilli())

	var cancels atomic.Int64
	cancelled := make(chan struct{})
	start := time.Now()
	err := coord.run(shutdownSteps{
		cancelExecutions: func() {
			cancels.Add(1)
			close(cancelled)
		},
		join: func(joinCtx context.Context) error {
			select {
			case <-cancelled:
				return nil
			case <-joinCtx.Done():
				return joinCtx.Err()
			}
		},
	})
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.EqualValues(t, 1, cancels.Load(), "排空到点必须恰好取消一次")
	require.GreaterOrEqual(t, elapsed, 100*time.Millisecond,
		"取消只能在排空窗口用完之后发生，不得提前打断在途执行")
	require.Less(t, elapsed, time.Second, "取消后必须在剩余预算内 join，而不是等完整上界")
}

// shutdownTestLogger 给不关心日志的用例一个丢弃日志器。
func shutdownTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

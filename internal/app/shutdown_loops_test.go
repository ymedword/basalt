package app

// 本文件判 US-25C 后台同步循环的停机接线（§2.3.4 的停机次序）：**启动失败**的停机
// 路径（HTTP 面装配失败、监听启动失败）没有信号去取消 runCtx，第 [2] 步「停止领取」
// 必须显式取消两个循环的专属 ctx。漏掉这一步时 join 会等满
// `worker.shutdown_grace_seconds`，进程走「不释放租约、不关闭数据库，直接退出」分支，
// 丢掉唯一的成功标记 `msg=停机完成`——`TestBackfill_Web/release_order` 的 exec 出错
// 路径正是这个形态（回归由 a92b04f 引入，v0.2.0 前修复）。
// This file pins the shutdown wiring of the two background sync loops: a
// startup-failure shutdown has no signal, so step [2] must cancel their context or
// the join overruns the worker grace and the process exits without the success marker.

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestShutdown_StartupFailureStopsSyncLoops 判非信号停机也能在预算内 join 两个循环：
// 第 [2] 步取消后两个 done 通道都关闭、closeCoordinated 返回 nil、进程未走超界退出。
// TestShutdown_StartupFailureStopsSyncLoops asserts the loops are cancelled by step [2]
// so a startup-failure shutdown still completes within its budget.
func TestShutdown_StartupFailureStopsSyncLoops(t *testing.T) {
	exited := make(chan int, 1)
	// 预算刻意压到毫秒级：接线正确时停机是立刻的，接线缺失时会在 grace 上界判红。
	coord := newShutdownCoordinator(300*time.Millisecond, 100*time.Millisecond, 50*time.Millisecond,
		slog.New(slog.NewJSONHandler(io.Discard, nil)))
	coord.exit = func(code int) { exited <- code }

	// 两个循环只在收到取消后退出——与 `AssetSync.Run` / `PolicyRefresher.Run` 同形。
	loopCtx, stopLoops := context.WithCancel(context.Background())
	var stopped atomic.Int64
	assetDone := make(chan struct{})
	policyDone := make(chan struct{})
	go func() {
		<-loopCtx.Done()
		stopped.Add(1)
		close(assetDone)
	}()
	go func() {
		<-loopCtx.Done()
		stopped.Add(1)
		close(policyDone)
	}()

	rt := &heavyRuntime{
		shutdown:          coord,
		assetSyncDone:     assetDone,
		policyRefreshDone: policyDone,
		stopSyncLoops:     stopLoops,
	}

	require.NoError(t, rt.closeCoordinated(), "启动失败停机不得因未取消同步循环而超界")
	select {
	case code := <-exited:
		t.Fatalf("以退出码 %d 直接退出进程：第 [2] 步没有停止后台同步循环", code)
	default:
	}
	require.EqualValues(t, 2, stopped.Load(), "两个循环都必须收到取消并退出")
}

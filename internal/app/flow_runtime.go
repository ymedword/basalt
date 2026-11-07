package app

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// Construction is inert. Only serve explicitly starts this single lifecycle
// driver, which owns the already bounded Flow.Worker pool and joins before DB close.
type flowRuntime struct {
	service   *flow.Service
	publisher *flow.Publisher
	worker    *flow.Worker
	runner    *flowNodeRunner
	store     *postgres.FlowStore
	logger    *slog.Logger
	// workerGrace 是 `worker.shutdown_grace_seconds`：交还租约预算的第二个上界
	// （§2.3.11 [6]）。signalAt 记录进程收到 SIGINT/SIGTERM 的时刻（unix 毫秒），
	// 0 表示尚未进入停机——正常推进时预算只用 cancel_wait + 5s 这一项。
	// workerGrace caps the release budget; signalAt marks the shutdown instant.
	workerGrace time.Duration
	signalAt    atomic.Int64
	mu          sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	runErr      error
	closed      bool
	authReady   chan struct{}
	authOnce    sync.Once
}

// releaseCtx 返回交还 `flow_root` 租约可用的**剩余停机预算**（US-24 §2.3.11 [6]）：
//
//	min(now + flows.cancel_wait + 5s, signal + worker.shutdown_grace_seconds)
//
// 它只交给 `core.KeepLease` 的 stop()：结果 ctx **必须**带 deadline——
// 用 `context.WithoutCancel` 会把预算和 deadline 一并剥掉，数据库挂起时 Release
// 又变成无界等待。预算已耗尽时返回一个已过期的 ctx，调用方因此在预算内等不到
// keeper 退出就跳过 Release，交 TTL 回收。
// releaseCtx returns the remaining shutdown budget for releasing a root lease.
func (r *flowRuntime) releaseCtx() context.Context {
	if r == nil || r.service == nil {
		return nil
	}
	budget := time.Duration(r.service.Config.CancelWaitSeconds)*time.Second + 5*time.Second
	if signal := r.signalAt.Load(); signal > 0 {
		if remaining := time.Until(time.UnixMilli(signal).Add(r.workerGrace)); remaining < budget {
			budget = remaining
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(budget))
	// stop() 只借用这一个 ctx 一次，不持有 cancel 句柄；到点时由下面这个回调
	// 释放计时器资源，避免依赖调用方代为 cancel。
	context.AfterFunc(ctx, cancel)
	return ctx
}

func (r *flowRuntime) authenticationReady() {
	if r != nil && r.authReady != nil {
		r.authOnce.Do(func() { close(r.authReady) })
	}
}

func (r *flowRuntime) prepare(ctx context.Context) error {
	// US-24 起启动**不再**全局扫描未收口根：多 Worker 下它们可能正被别的活实例推进，
	// 按"启动时还看到 running"把它们 Park 掉会打断别人的执行（FR-24-02）。遗留根由
	// 回收器处理：持有过期 flow_root 租约的根在 A3 里先取证再 `Service.Recover`；
	// 没人持租约的根留在 `ReadyRoots` 里照常被推进。这里只发布"本进程已就绪"。
	// Startup no longer parks open roots: with multiple workers they may be live
	// elsewhere. Orphan roots are settled by the reclaimer via expired root leases.
	r.runner.mu.Lock()
	r.runner.startupComplete = true
	r.runner.mu.Unlock()
	return nil
}

func (r *flowRuntime) Start(ctx context.Context) error {
	if r == nil || r.service == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil || r.closed {
		return flow.ErrStateConflict
	}
	if err := r.prepare(ctx); err != nil {
		return err
	}
	// Disabling background advancement must not strand old tasks in a false
	// running state or prevent authenticated cancellation after process recovery.
	if !r.service.Config.Enabled {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	done := make(chan struct{})
	r.done = done
	// 记录停机信号时刻：`worker.shutdown_grace_seconds` 的预算从 signal 起算，
	// 不是从 Close 起算（§2.3.11 [6]）。只写一次，重复信号不刷新时刻。
	// Mark the signal instant once; the release budget counts from it.
	signal := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			r.signalAt.CompareAndSwap(0, time.Now().UnixMilli())
		case <-signal:
		}
	}()
	// Capture the channel before Close clears the lifecycle fields. Reading
	// r.done in the goroutine could send to nil and strand the actual join.
	go func() {
		err := r.worker.Run(workerCtx)
		r.mu.Lock()
		r.runErr = err
		close(signal)
		close(done)
		r.mu.Unlock()
	}()
	return nil
}

// StopClaims 只停领取（US-24 §2.3.11 [2]）：取消 Flow worker 的 ready 轮询，**不**
// 等待在途根退出。排空期间各根的 keeper 继续续约；join 仍由 Close 完成。
// 幂等（cancel 可重复调用），且与 Close 可任意顺序组合。
// StopClaims cancels the ready poll only; Close still owns the join.
func (r *flowRuntime) StopClaims() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *flowRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.closed = true
	r.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	// Worker retains all resources until the actual leaf joins. Its configured
	// grace logs overruns, but never turns a timeout into evidence of a stopped call.
	<-done
	r.mu.Lock()
	err := r.runErr
	r.mu.Unlock()
	// 只忽略纯取消（含被 errors.Join 单元素包装的取消）；Join 一旦携带数据库
	// 或 drain 错误，必须保留——由 CancellationOnly 逐叶判定。
	// Ignore only cancellation-only errors, including a join that wraps just the
	// cancellation sentinel; any joined database or drain failure is preserved.
	if flow.CancellationOnly(err, context.Canceled) {
		return nil
	}
	return err
}

func runtimeFlowConfig(c flow.Config, instanceRuns int) (flow.Config, error) {
	if c.MaxActive == 0 && !c.Enabled {
		c = flow.DefaultConfig()
	}
	if instanceRuns == 0 {
		instanceRuns = 32
	}
	return c, c.Validate(instanceRuns, 70*time.Second)
}

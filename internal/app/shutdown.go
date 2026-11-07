package app

// 本文件是模块卡 §2.3.11「图 11 停机顺序」在装配层的唯一编排点（US-24 T026 /
// FR-24-27 / FR-24-40，C-24）：
//
//	[2] 停止领取      调度到期扫描与 Flow ready 轮询都停，不再产生新执行
//	[3] drain 期间续约 每条执行自己的 keeper 继续续约，直到执行真正退出
//	[4] 有界排空      上界 = signal + flows.shutdown_grace_seconds
//	[5] join 收尾     取消后仍要等执行与收尾退出；上界取小，不追加一份完整 cancel_wait
//	[6] 释放租约      每条执行在收尾里按 token 交还；本层只保证"在 join 之后"
//	[7] Worker 下线   写 worker_nodes.stopped_at（诊断）
//	[8] 关闭数据库    最后一步；MCP 与实例锁各自的次序不变
//
// 超过 `worker.shutdown_grace_seconds` 仍未 join 时**直接退出进程**：不 Release、
// 不关库——交 TTL 回收比"带着未退出的 goroutine 关库"更安全（图 11）。
// This file is the one place the shutdown sequence is sequenced. If the join overruns
// worker.shutdown_grace_seconds the process exits without releasing leases and without
// closing the database: leaving them to the TTL is safer than closing the pool under a
// goroutine that is still writing.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

const (
	// shutdownIncompleteExitCode 是"超过 worker.shutdown_grace_seconds 仍未退出"时的
	// 进程退出码。用一个与启动失败相同的 1：这是一次失败停机，不是正常关闭。
	// shutdownIncompleteExitCode marks a failed (overrun) shutdown.
	shutdownIncompleteExitCode = 1

	// shutdownAuditGrace 是图 11 [5]/[6] 公式里的 "+5s"（与审计终态写的独立短超时
	// 同源）。它与 `worker.shutdown_grace_seconds` 的 -5s 校验一起把两段预算错开。
	// shutdownAuditGrace is the "+5s" term used by the [5]/[6] budget formulas.
	shutdownAuditGrace = 5 * time.Second
)

// errShutdownIncomplete 表示 join 超出 `worker.shutdown_grace_seconds`。生产路径在
// 返回它之前已经调用 `exit` 结束进程；只有注入替身的用例会看到它作为返回值。
// errShutdownIncomplete is returned only to tests that inject a fake exit.
var errShutdownIncomplete = errors.New("shutdown incomplete: worker.shutdown_grace_seconds exceeded")

// shutdownBudget 是一轮停机的三段预算。signalAt 是进程收到 SIGINT/SIGTERM 的时刻：
// 所有上界都从它起算，而不是从 Close 起算——HTTP 优雅关闭已经花掉的秒数必须计入，
// 否则整条链路会比配置承诺的时间多活一段。
// shutdownBudget keeps the three deadlines of figure 11; all of them count from the
// signal instant, not from Close.
type shutdownBudget struct {
	grace      time.Duration
	flowGrace  time.Duration
	cancelWait time.Duration
	signalAt   time.Time
}

// drainDeadline 是 [4] 的上界：signal + flows.shutdown_grace_seconds。
// drainDeadline is the upper bound of the bounded drain.
func (b shutdownBudget) drainDeadline() time.Time { return b.signalAt.Add(b.flowGrace) }

// joinDeadline 是 [5] 的上界：min(now + cancel_wait + 5s, signal + workerGrace)。
// 取小值而不是再要一份完整 cancel_wait——"join 使用剩余 grace 预算"正是本条。
// joinDeadline is the [5] upper bound; it reuses the remaining grace instead of
// spending a fresh cancel_wait window.
func (b shutdownBudget) joinDeadline(now time.Time) time.Time {
	deadline := now.Add(b.cancelWait + shutdownAuditGrace)
	if hard := b.signalAt.Add(b.grace); hard.Before(deadline) {
		deadline = hard
	}
	return deadline
}

// releaseDeadline 是 [6] 的上界：signal + workerGrace。到达时预算为 0，跳过释放。
// releaseDeadline is the [6] upper bound; a zero remaining budget skips the release.
func (b shutdownBudget) releaseDeadline() time.Time { return b.signalAt.Add(b.grace) }

// shutdownSteps 是图 11 各阶段的注入面。生产装配由 `heavyRuntime` 提供真实实现；
// 用例注入可观测替身，逐阶段断言次序、预算与"超界不关库"。
// shutdownSteps is the injection surface for each phase of figure 11.
type shutdownSteps struct {
	// stopClaims 是 [2]：取消调度领取与 Flow 领取轮询（幂等、不等待）。
	stopClaims func()
	// join 是 [3][4][5]：等待执行与收尾**真正退出**。契约：返回 nil 或一个真实的
	// 收尾错误表示已 join；返回 ctx.Err()（DeadlineExceeded）表示仍有工作未退出。
	// 必须可重复调用——第二窗口继续等同一批工作，而不是重新开始。
	join func(context.Context) error
	// cancelExecutions 是 [4] 的"超时 → cancel()"：排空预算到点时取消在途执行，
	// 随后由 join 在剩余预算内等待它们与收尾真正退出。只发取消、不等待；幂等。
	// cancelExecutions is the figure-11 [4] cancel: fire, never wait.
	cancelExecutions func()
	// releaseLeases 是 [6]：在 join 之后交还本进程仍持有的租约。
	releaseLeases func(context.Context) error
	// stopWorker 是 [7]：写 worker_nodes 下线标记（诊断）。
	stopWorker func(context.Context) error
	// closeObservability 在 join 之后、库关闭之前关闭观测面：收尾路径仍要写诊断，
	// 提前关闭会把终态事件丢掉。
	closeObservability func() error
	// closeMCP 收起 MCP 连接（先于库关闭）。
	closeMCP func() error
	// closeStore 是 [8]：关闭数据库连接，最后一步。
	closeStore func() error
	// releaseLock 释放实例锁（库关闭之后）。
	releaseLock func() error
}

// shutdownCoordinator 持有预算与退出点。`exit` 是"超过上界直接退出进程"的唯一
// 执行点：生产用 os.Exit，用例注入记录器，从而能在不结束测试进程的前提下判定
// 退出时刻，并断言退出前没有关库。
// shutdownCoordinator holds the budgets and the single process-exit point.
type shutdownCoordinator struct {
	grace      time.Duration
	flowGrace  time.Duration
	cancelWait time.Duration

	logger *slog.Logger
	exit   func(int)
	now    func() time.Time

	// signalAt 是进程收到信号的时刻（unix 毫秒）。0 = 未收到信号，预算从本次计算
	// 起算（启动失败等非信号停机路径）。
	// signalAt is the signal instant in unix millis; 0 means "count from now".
	signalAt atomic.Int64
}

// newShutdownCoordinator 构造编排器。零值预算按关闭处理（用例只给关心的段）；
// 生产路径的预算由配置校验保证非零。
// newShutdownCoordinator builds the coordinator; zero segments are allowed for tests.
func newShutdownCoordinator(
	grace, flowGrace, cancelWait time.Duration, logger *slog.Logger,
) *shutdownCoordinator {
	if logger == nil {
		logger = slog.Default()
	}
	return &shutdownCoordinator{
		grace:      grace,
		flowGrace:  flowGrace,
		cancelWait: cancelWait,
		logger:     logger,
		exit:       os.Exit,
		now:        time.Now,
	}
}

// watchSignal 记录停机信号时刻。只写一次：重复信号不刷新预算起点。
// watchSignal records the first signal instant; repeated signals do not refresh it.
func (c *shutdownCoordinator) watchSignal(ctx context.Context) {
	if c == nil {
		return
	}
	go func() {
		<-ctx.Done()
		c.signalAt.CompareAndSwap(0, c.now().UnixMilli())
	}()
}

// budgetAt 取出本轮预算。没有信号时刻时以 now 为起点（并回写，保证 [4] 与 [5]
// 用的是同一个起点）。
// budgetAt resolves the round's budget, anchoring an unsignalled shutdown at now.
func (c *shutdownCoordinator) budgetAt(now time.Time) shutdownBudget {
	signal := c.signalAt.Load()
	if signal == 0 {
		if c.signalAt.CompareAndSwap(0, now.UnixMilli()) {
			signal = now.UnixMilli()
		} else {
			signal = c.signalAt.Load()
		}
	}
	return shutdownBudget{
		grace:      c.grace,
		flowGrace:  c.flowGrace,
		cancelWait: c.cancelWait,
		signalAt:   time.UnixMilli(signal),
	}
}

// run 执行 [2]–[8]。返回 nil 表示停机完成；返回错误表示有阶段失败但库仍已按顺序
// 关闭；返回 errShutdownIncomplete 表示 join 超界（生产路径此时进程已退出）。
// run executes phases [2]–[8]; errShutdownIncomplete means the join overran.
func (c *shutdownCoordinator) run(steps shutdownSteps) error {
	// [2] 停止领取：先停监听（它的回调会联动调度器），再取消两条领取轮询，
	//     最后关闭池化名额的排队入口。
	// Stop the config watcher first, then both claim loops, then the slot queue.
	if steps.stopClaims != nil {
		steps.stopClaims()
	}

	budget := c.budgetAt(c.now())

	// [4] 有界排空：这一段时间里，各执行的 keeper 仍在续约——排空期间租约过期被
	// 他人接管正是本条要避免的缺陷（C-24）。
	// The bounded drain; renewals keep running during it.
	joinErr := c.joinWithin(steps.join, budget.drainDeadline())
	if errors.Is(joinErr, context.DeadlineExceeded) {
		// [4] 排空到点：**先取消**在途执行（图 11 的"超时 → cancel()"），再进 [5]
		// 取消后仍要 join：上界取 min(now + cancel_wait + 5s, signal + grace)，
		// 不再另给一份完整的 cancel_wait。缺了这一步，取消只能等调度器自己的兜底
		// 宽限（一个包级常量），排空窗口形同虚设、进程会一路撞到硬退出的上界。
		// Drain expired: fire the cancel first, then join inside the remaining budget.
		joinDeadline := budget.joinDeadline(c.now())
		c.logger.Warn("停机排空超过 flows.shutdown_grace_seconds，取消在途执行并进入 join 窗口",
			"drain_deadline_ms", budget.drainDeadline().UnixMilli(),
			"join_deadline_ms", joinDeadline.UnixMilli(),
			"worker_shutdown_grace", c.grace.String())
		if steps.cancelExecutions != nil {
			steps.cancelExecutions()
		}
		joinErr = c.joinWithin(steps.join, joinDeadline)
	}
	if errors.Is(joinErr, context.DeadlineExceeded) {
		// 超过 worker.shutdown_grace_seconds：不 Release、不关库，直接退出进程。
		// Overrun: exit without releasing leases and without closing the database.
		c.logger.Error("停机超出 worker.shutdown_grace_seconds：不释放租约、不关闭数据库，直接退出进程",
			"worker_shutdown_grace", c.grace.String(),
			"elapsed", c.now().Sub(budget.signalAt).String())
		c.exit(shutdownIncompleteExitCode)
		return errShutdownIncomplete
	}

	var errs []error
	if joinErr != nil {
		errs = append(errs, joinErr)
	}

	// 观测面在 join 之后关闭：终态诊断写属于收尾的一部分，提前关闭会把它们丢掉。
	// Observability closes after the join so terminal diagnostics are not lost.
	if steps.closeObservability != nil {
		errs = append(errs, steps.closeObservability())
	}

	// [6] 释放租约：每一条执行都在自己的收尾路径里按 token 交还（Run 的 `FinishRun`、
	// 调度执行的 `FinishExecution`、Flow 根的 `KeepLease.stop`），join 已确认它们
	// 全部退出——因此本层到达这里时进程内不再有待释放的 token，也不得越权代释放。
	// 预算耗尽（signal + grace 已过）时跳过并记 shutdown_incomplete。
	// Every execution released its own token inside closeout; the join above is the
	// structural proof that nothing is left to release here.
	if steps.releaseLeases != nil {
		if remaining := time.Until(budget.releaseDeadline()); remaining > 0 {
			releaseCtx, cancel := context.WithDeadline(context.Background(), budget.releaseDeadline())
			errs = append(errs, steps.releaseLeases(releaseCtx))
			cancel()
		} else {
			c.logger.Warn("释放租约的停机预算已耗尽，跳过释放并交 TTL 回收",
				"worker_shutdown_grace", c.grace.String())
		}
	}

	// [7] Worker 下线标记：诊断用途，失败不回滚停机。
	// [7] marks the worker stopped; a failure does not roll the shutdown back.
	if steps.stopWorker != nil {
		errs = append(errs, steps.stopWorker(context.Background()))
	}
	if steps.closeMCP != nil {
		errs = append(errs, steps.closeMCP())
	}

	// [8] 关闭数据库——最后一步。
	// [8] closes the database last.
	if steps.closeStore != nil {
		errs = append(errs, steps.closeStore())
	}
	if steps.releaseLock != nil {
		errs = append(errs, steps.releaseLock())
	}
	return errors.Join(errs...)
}

// joinWithin 在一个上界内等待 join。上界已过期时仍调用一次 join（它会立刻返回
// ctx.Err()），保证返回值的判定口径统一。
// joinWithin waits for the join inside one deadline; an already-expired deadline still
// invokes join once so the verdict stays uniform.
func (c *shutdownCoordinator) joinWithin(join func(context.Context) error, deadline time.Time) error {
	if join == nil {
		return nil
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return join(ctx)
}

// closeCoordinated 是 serve / gateway 的停机路径：按图 11 编排，超界直接退出。
// closeCoordinated is the serve/gateway shutdown path.
func (r *heavyRuntime) closeCoordinated() error {
	steps := shutdownSteps{
		stopClaims: func() {
			// 监听最先关：它的回调会推进注册表并联动调度器（约束 22 的次序）。
			// The watcher closes first: its callbacks drive the scheduler.
			if r.watch != nil {
				if err := r.watch.Close(); err != nil {
					r.logShutdownWarn("配置监听关闭失败", err)
				}
			}
			if r.scheduler != nil {
				r.scheduler.StopClaims()
			}
			if r.flows != nil {
				r.flows.StopClaims()
			}
			if r.admission != nil {
				r.admission.inflight.drain()
			}
		},
		join:               r.joinWorkers,
		cancelExecutions:   r.cancelInFlight,
		releaseLeases:      r.releaseOwnLeases,
		stopWorker:         r.stopWorker,
		closeObservability: r.closeObservability,
		closeMCP:           r.closeMCP,
		closeStore:         r.closeStore,
		releaseLock:        r.releaseLock,
	}
	err := r.shutdown.run(steps)
	if err == nil {
		// 成功停机的唯一标记：e2e 以它判定"运行时已按图 11 释放"。US-24 移除了
		// 进程内 cron 引擎，旧标记（robfig cronLogger 的 `stop`）随之消失，标记
		// 因此挪到编排层——语义仍是"停机走完且库已关闭"。
		// The one success marker: end-to-end cases key on it to mean "figure-11
		// shutdown completed and the store is closed".
		r.shutdown.logger.Info("停机完成", "step", "shutdown",
			"worker_shutdown_grace", r.shutdown.grace.String())
	}
	return err
}

// joinWorkers 等待调度器与 Flow worker 真正退出。两个关闭入口都可能阻塞到各自
// 的上界（调度器 70s 宽限、Flow worker 自己的 grace），因此必须在调用方给的预算
// 内等待，超界返回 ctx.Err() 交给编排层判"直接退出"。
// joinWorkers waits for both loops within the caller's budget; an overrun surfaces as
// ctx.Err() so the coordinator can take the exit branch.
func (r *heavyRuntime) joinWorkers(ctx context.Context) error {
	flowDone := make(chan error, 1)
	go func() {
		if r.flows == nil {
			flowDone <- nil
			return
		}
		flowDone <- r.flows.Close()
	}()
	schedDone := make(chan struct{})
	go func() {
		if r.scheduler != nil {
			r.scheduler.Stop()
		}
		close(schedDone)
	}()
	// flowErr 由 joiner goroutine 写、select 分支在读 close(joined) 之后读：
	// close 建立 happens-before，无需再加锁。**不得**在 select 里再收一次 flowDone
	// ——缓冲 1 的值已被 joiner 取走，第二次接收会永久阻塞（曾经的真缺陷：
	// serve 正常停机卡在 joinWorkers，仅注入替身的用例测不出来）。
	// flowErr is written by the joiner and read only after joined is closed; the
	// channel close gives the happens-before edge. Never receive from flowDone again:
	// the joiner already drained the one buffered value.
	var flowErr error
	joined := make(chan struct{})
	go func() {
		flowErr = <-flowDone
		<-schedDone
		close(joined)
	}()
	select {
	case <-joined:
		return flowErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// cancelInFlight 是图 11 [4] 的"超时 → cancel()"：取消调度执行组的在途 Run
// （它们的父 ctx 是调度器基 ctx，取消后各执行走自己的收尾路径）。Flow 池在 [2]
// 停止领取时已收到同一次取消，这里再调一次只是幂等重申——取消必须覆盖两类在途。
// cancelInFlight fires the drain-time cancel for both execution families; both calls
// are idempotent and neither waits.
func (r *heavyRuntime) cancelInFlight() {
	if r.scheduler != nil {
		r.scheduler.CancelExecutions()
	}
	if r.flows != nil {
		r.flows.StopClaims()
	}
}

// releaseOwnLeases 见 shutdownSteps.releaseLeases 的契约：本层没有自己的 token。
// 保留为一个独立阶段，是为了让"释放发生在 join 之后"在顺序上可判、可测。
func (r *heavyRuntime) releaseOwnLeases(context.Context) error { return nil }

// stopWorker 写 worker_nodes 下线标记（[7]）。
func (r *heavyRuntime) stopWorker(ctx context.Context) error {
	if r.worker == nil {
		return nil
	}
	return r.worker.Stop(ctx)
}

// closeObservability 关闭诊断导出面。
func (r *heavyRuntime) closeObservability() error {
	if r.observability == nil {
		return nil
	}
	return r.observability.Close()
}

// closeMCP 收起 MCP 连接池 / supervisor（必须在库关闭之前）。
func (r *heavyRuntime) closeMCP() error {
	var errs []error
	if r.mcpSupervisor != nil {
		errs = append(errs, r.mcpSupervisor.Close())
	}
	if r.mcp != nil {
		errs = append(errs, r.mcp.Close())
	}
	return errors.Join(errs...)
}

// closeStore 关闭数据库（[8]）。
func (r *heavyRuntime) closeStore() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

// releaseLock 释放实例锁（库关闭之后）。
func (r *heavyRuntime) releaseLock() error {
	if r.lock == nil {
		return nil
	}
	return r.lock.Release()
}

// logShutdownWarn 在编排阶段记录失败：停机路径上的错误不能静默（否则"关掉了"与
// "没关掉"在日志上无法区分）。
func (r *heavyRuntime) logShutdownWarn(msg string, err error) {
	logger := slog.Default()
	if r.shutdown != nil && r.shutdown.logger != nil {
		logger = r.shutdown.logger
	}
	logger.Warn(msg, "error", err)
}

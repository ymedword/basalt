package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// 本文件是执行权的续约循环（US-24 §2.3.3 冻结伪代码，T007 / FR-24-35）。
//
// 为什么在 `core` 而不是模块卡 §3.1.10 写的 `internal/app/worker_runtime.go`：
// `AgentService`（§2.3.2）与 `flow.Worker`（§2.3.6 图 5）都调用同一个 keeper，而
// `core` / `flow` 都**不得** import `app`（装配层单向依赖，技术方案 §7.1.2 三条包
// 纪律）。三处只能共用一个包级实现，`core` 是唯一同时被双方依赖的包；FR-24-35 也
// 把用例点名在 `core`。函数名因此从伪代码的 `keepLease` 升为导出 `KeepLease`
// （跨包调用必需），签名顺序与语义逐字保持。裁决记录见 run-state §1.3。
// The keeper lives in core, not app: AgentService and flow.Worker both call it and
// neither may import the assembly layer. The name is exported because cross-package
// calls require it; the signature and semantics follow §2.3.3 verbatim.

// RenewFailureRecorder 是续约循环的**可选**事件出口：租约实现可以额外提供它，
// keeper 在失权（ErrLeaseLost）与有界停止（ErrLeaseRenewDeadline）两个终态上尽力
// 补一行 `lease_events(event='renew_failed')`（US-24 §2.7；评审 IDX-25）。
//
// 为什么是可选接口而不是 LeaseStore 的第 5 个方法：端口的最小面是"取 / 续 / 放 /
// 受保护事务"，事件是**运维记录**（§2.7：可采样 / 可清理，不得当审计结论）。把它塞进
// 必需面会让只读替身与轻装配也被迫实现一条与执行权无关的写入；keeper 缺它时只记日志，
// 停止语义一字不变。
// RenewFailureRecorder is the optional renew_failed event outlet; the keeper treats a
// missing implementation as "log only" so the stop semantics never depend on it.
type RenewFailureRecorder interface {
	RecordRenewFailure(ctx context.Context, token LeaseToken, reason string) error
}

// 事件 detail 的两个固定字面量（机器可读）：keeper 只在这两个终态各写一行，
// **不**逐次续约失败写事件——那会把 lease_events 变成心跳日志（§2.3.3 / §2.4.3）。
const (
	renewFailureLeaseLost        = "lease_lost"
	renewFailureDeadlineExceeded = "renew_deadline_exceeded"
)

// recordRenewFailure 尽力写一行续约失败事件。独立 context + 固定短预算：此刻执行侧
// 已按取消原因停手，DB 可能正忙（有界停止的典型起因就是 DB 不可达），事件可丢弃，
// 停止不可延迟——写失败只记 WARN，不上抛、不改变返回值。
// recordRenewFailure is best-effort: a fixed short budget, a canceled execution and a
// possibly unhealthy DB; a failed write is logged and never changes the stop decision.
func recordRenewFailure(store LeaseStore, token LeaseToken, reason string, logger *slog.Logger) {
	recorder, ok := store.(RenewFailureRecorder)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := recorder.RecordRenewFailure(ctx, token, reason); err != nil {
		logger.Warn("续约失败事件未能落库（事件可丢弃，不影响停止）",
			"kind", string(token.Kind), "resource_id", token.ResourceID, "reason", reason, "error", err)
	}
}

// KeepLease 为某个执行权派生续约循环。返回三个句柄：
//
//   - ctx：在失权（ErrLeaseLost）、他处取消请求（ErrCancelRequested）或连续续约
//     失败超过 `ttl - renewInterval`（ErrLeaseRenewDeadline）时被取消；执行侧据此
//     停止发起新动作。**不得**声称已在途外部请求被撤销——它们只能各自到界。
//   - stop(shutdownCtx)：先取消再**等**续约 goroutine 退出，退出后在调用方给的
//     剩余停机预算 ctx 内交还租约。等不到退出就不 Release（续约可能仍在写，释放后
//     新属主会与它并发），交 TTL 回收。**不得**套 `context.WithoutCancel`——那会
//     把预算与 deadline 一并剥掉，回到无界等待。
//   - abandon()：只取消续约，**不**交还租约（结论未定 / 需逼出下一轮接管时用）。
//
// KeepLease derives the renewal loop for one execution lease and returns the derived
// context plus stop/abandon handles. stop waits for the keeper to exit before
// releasing, and releases inside the caller-supplied shutdown budget.
func KeepLease(ctx context.Context, store LeaseStore, token LeaseToken,
	renewInterval, ttl time.Duration, logger *slog.Logger,
) (context.Context, func(context.Context), func()) {
	if logger == nil {
		logger = slog.Default()
	}
	// 装载期校验（§3.3：renew < ttl/2，两者为正）保证不会走到这里；仍显式拒绝，
	// 因为零 / 负 interval 会让 NewTicker panic 在 goroutine 里直接打崩进程。
	// Load-time validation guarantees positive values; refuse anyway so a miswired
	// caller gets a cancelled context instead of a ticker panic in a goroutine.
	if renewInterval <= 0 || ttl <= 0 {
		leaseCtx, cancel := context.WithCancelCause(ctx)
		cancel(fmt.Errorf("keepLease: renew_interval=%s / ttl=%s 非法：两者都必须为正",
			renewInterval, ttl))
		return leaseCtx, func(context.Context) {}, func() {}
	}

	leaseCtx, cancel := context.WithCancelCause(ctx)
	keeperDone := make(chan struct{})
	go func() {
		defer close(keeperDone)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		lastRenewOK := time.Now()
		deadline := ttl - renewInterval // 有界停止阈值
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				// 每次 Renew 独立超时：DB 挂起时也必须让本 goroutine 有上界。
				renewCtx, cancelRenew := context.WithTimeout(leaseCtx, renewInterval)
				lease, err := store.Renew(renewCtx, token, ttl)
				cancelRenew()
				switch {
				case err == nil:
					lastRenewOK = time.Now()
				case errors.Is(err, ErrLeaseLost):
					logger.Warn("租约失权，停止发起新动作",
						"kind", string(token.Kind), "resource_id", token.ResourceID)
					// 先取消（执行侧立刻停手），再尽力补事件：事件写有固定预算，
					// 不得挡在停止前面。
					cancel(ErrLeaseLost)
					recordRenewFailure(store, token, renewFailureLeaseLost, logger)
					return
				case time.Since(lastRenewOK) >= deadline:
					// 有界停止：不再无限重试，执行侧必须结束。
					logger.Error("租约续约超过有界停止阈值，停止执行",
						"kind", string(token.Kind), "resource_id", token.ResourceID, "error", err)
					cancel(ErrLeaseRenewDeadline)
					recordRenewFailure(store, token, renewFailureDeadlineExceeded, logger)
					return
				default:
					logger.Error("租约续约失败，继续重试",
						"kind", string(token.Kind), "resource_id", token.ResourceID, "error", err)
					continue
				}
				if lease.CancelRequested {
					logger.Info("其他 Worker 请求取消，停止执行", "resource_id", token.ResourceID)
					cancel(ErrCancelRequested)
					return
				}
			}
		}
	}()

	// stop 由持有剩余停机预算的调用方触发（§2.3.11 的剩余时间公式）。
	stop := func(shutdownCtx context.Context) {
		cancel(nil)
		select {
		case <-keeperDone:
			// 续约 goroutine 已退出，此后不会再有续约写：可以安全 Release。
		case <-shutdownCtx.Done():
			// 等不到 keeper 退出就**不 Release**：它可能仍在写续约，
			// 释放后新属主会与它并发。留给 TTL 回收。
			logger.Error("续约循环未在停机预算内退出，跳过释放",
				"kind", string(token.Kind), "resource_id", token.ResourceID)
			return
		}
		// 直接用调用方给的预算 ctx：它由停机编排从剩余时间里派生、带 deadline，
		// 且**不**继承执行侧取消。不得再套 WithoutCancel。
		if err := store.Release(shutdownCtx, token); err != nil && !errors.Is(err, ErrLeaseLost) {
			logger.Warn("租约释放失败，交 TTL 回收",
				"kind", string(token.Kind), "resource_id", token.ResourceID, "error", err)
		}
	}
	abandon := func() { cancel(nil) }
	return leaseCtx, stop, abandon
}

package app

// 本文件是图 6 [3]「调用方并发名额」在**生产路径**上的落点（C-35）：运行入口
// （web 的 `RunService`）与调度器共用同一次取名额 / 归还 / panic 收口。
// This file is where admission stage [3] lives in production: the web run entry and
// the scheduler share one acquire/release/recover implementation.
//
// 三条纪律（C-35，不得改写）：
//   - 名额在 [4]（会话串行 / 实例容量）**之前**获取，每个 Run 只获取一次；
//   - 归还**必须**来自本层的 `defer`，覆盖正常完成、失败、取消、[4] 拒绝、配额拒绝
//     与 panic——测试不得代为释放，否则"生产路径漏释放"这条缺陷再也测不出来；
//   - 归还用 `context.WithoutCancel`：取消路径上调用方 ctx 已取消，但名额照还。
//     这不是业务状态进 context，而是把"取消"与"到期"从一次记账操作上摘掉。
//
// panic 收口：工具执行或循环内部的 panic 是**运行期致命错误**，按
// `run-and-errors.md` §2 走 `RunResult.Status=failed` + `Reason`（含 "panic" 字样，
// 便于与超时 / 预算失败区分），**不**抬高成函数级 error——调用方问题才走 error。
// Panics are run-time fatal errors: they surface as failed + a reason naming the
// panic, never as a function-level error, and never skip the slot release.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// runAdmission 是 [3] 的唯一实现。limiter 为 nil 表示未接限制器（轻装配与既有
// 单测）：此时不加名额、保持接线前的行为，但 panic 收口仍然生效。
// A nil limiter means admission is not wired; the panic guard still applies.
type runAdmission struct {
	limiter *callers.Limiter
	logger  *slog.Logger
	// diag 是配额判定诊断（V-8）：Run 准入是全流程的第一次配额判定，`deny` 在这里
	// 意味着 Run 从未开始，事件是它唯一的时间线痕迹（没有执行、没有审计行）。
	// diag records the run-admission verdict; a denial here has no audit row at all.
	diag diagnosticRecorder
}

// run 取名额 → 执行 → 归还。`next` 是真正跑 Run 的函数（core 运行入口）。
// run takes the slot, runs, and always returns the slot.
func (a *runAdmission) run(
	ctx context.Context,
	req core.RunRequest,
	next func(context.Context, core.RunRequest) (core.RunResult, error),
) (res core.RunResult, err error) {
	if a != nil && a.limiter != nil {
		ticket, admitErr := a.limiter.AdmitRun(ctx, req.Principal)
		// 准入判定先留痕（V-8）：拒绝时 Run 未开始、没有审计行，这条事件就是它在
		// 诊断面上的全部痕迹；放行让"这次 Run 拿到过哪个名额表"可被复核。
		// The verdict is recorded either way; a denial has no other trace.
		recordQuotaDecision(ctx, a.diag, quotaDecision{
			RunID:     req.RunID,
			RequestID: req.RequestID,
			Class:     "run",
			Err:       admitErr,
		})
		if admitErr != nil {
			// 名额不足 / 身份非法：纯准入拒绝，Run 未开始，也**没有**占住名额。
			// A denied admission never took a slot, so there is nothing to release.
			return core.RunResult{}, admitErr
		}
		defer func() {
			// 归还先于 panic 记录：无论下面是否 panic，名额都必须回到池子里。
			// The release runs even when `next` panicked (defer ordering).
			if relErr := a.limiter.ReleaseRun(context.WithoutCancel(ctx), ticket); relErr != nil {
				a.logWarn("归还运行并发名额失败——名额可能泄漏到进程重启", "error", relErr,
					"caller_id", ticket.CallerID, "run_key", ticket.RunKey)
			}
		}()
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			// 栈不进 Reason（会撑爆 HTTP 响应），只进日志；Reason 点名 panic 即可。
			// The stack goes to the log, not to the reason string.
			a.logError("运行入口 panic——已收口为 failed", "panic", recovered,
				"agent", req.Agent, "channel", req.Channel)
			res = core.RunResult{
				RunID:  req.RunID,
				Status: core.StatusFailed,
				Reason: fmt.Sprintf("panic: %v", recovered),
			}
			err = nil
		}
	}()

	return next(ctx, req)
}

// logWarn 在日志器缺失时静默（轻装配测试不强制注入日志器）。
func (a *runAdmission) logWarn(msg string, args ...any) {
	if a != nil && a.logger != nil {
		a.logger.Warn(msg, args...)
	}
}

// logError 记录 panic 收口；日志器缺失时退回进程缺省日志器——panic 是最需要
// 留痕的一类，不得因为装配省略而消失。
func (a *runAdmission) logError(msg string, args ...any) {
	logger := slog.Default()
	if a != nil && a.logger != nil {
		logger = a.logger
	}
	logger.Error(msg, args...)
}

// admittedRunInvoker 把 [3] 包在调度器使用的 `core.RunInvoker` 外面。调度器没有
// HTTP 入口，因此**不**经过入口速率（C-35）、但要占名额；身份与归属由调度器按
// C-32 构造后放进 `RunRequest.Principal`。
// The scheduler has no HTTP entry, so it takes a slot but never touches the ingress
// bucket; identity and ownership arrive in the request principal.
type admittedRunInvoker struct {
	inner     core.RunInvoker
	admission *runAdmission
}

// 编译期断言：包装器仍然是调度器认识的运行入口。
var _ core.RunInvoker = (*admittedRunInvoker)(nil)

// Run 取名额后委托给真正的运行入口。
func (i *admittedRunInvoker) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	return i.admission.run(ctx, req, i.inner.Run)
}

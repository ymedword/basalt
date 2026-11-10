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
// US-24 P-06：这里的进程内名额表**降为预检**（快、不排队、跨进程无共识）。权威判定
// 在 `core.AgentService.Run` 的 `StartRun` 事务内——调用方 advisory 锁之后按共享库
// 的 running 行复查（FR-24-08 / FR-24-34）。预检放行、事务复查拒绝是**正常结局**：
// 拒绝方经 `core.ErrCallerConcurrencyLimited`（callers 侧是同一哨兵的别名）映射 429
// `CALLER_CONCURRENCY_LIMITED`，且本次准入零占用。本层不得声称自己强制了集群级名额。
// Since US-24 P-06 the in-process slot table is a precheck only; StartRun's
// transaction holds the authoritative verdict and rejects with the same sentinel.
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
	"time"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// runAdmission 是 [3] 的唯一实现。limiter 为 nil 表示未接限制器（轻装配与既有
// 单测）：此时不加名额、保持接线前的行为，但 panic 收口仍然生效。
// A nil limiter means admission is not wired; the panic guard still applies.
type runAdmission struct {
	limiter *callers.Limiter
	// inflight 是 `worker.max_inflight` 的池化名额（US-24 FR-24-40）：三条执行路径
	// 共用，nil 表示未接线。取槽在调用方名额之后、Run 开始之前——满额被拒的 Run
	// 零占用。
	// inflight is the pooled max_inflight budget shared by the three execution paths.
	inflight *inflightLimiter
	// runs / admission 把 [4] 的**调用方并发**结论提到容量类判定之前（FR-24-34）：
	// `max_inflight` 满额与实例上限同码（TOO_MANY_RUNS），若先取槽，跨 Worker 的
	// 调用方名额已满时返回的就是容量码而不是 CALLER_CONCURRENCY_LIMITED。两者 nil =
	// 未接线（轻装配与既有单测），行为与接线前逐字一致。
	// runs/admission hoist the caller-concurrency verdict ahead of the capacity rejects:
	// a full inflight pool shares TOO_MANY_RUNS with the instance cap and must not
	// outrank CALLER_CONCURRENCY_LIMITED. Nil keeps the pre-US-24 behaviour.
	runs      core.RunStore
	admission core.CallerAdmissionResolver
	logger    *slog.Logger
	// diag 是配额判定诊断（V-8）：Run 准入是全流程的第一次配额判定，`deny` 在这里
	// 意味着 Run 从未开始，事件是它唯一的时间线痕迹（没有执行、没有审计行）。
	// diag records the run-admission verdict; a denial here has no audit row at all.
	diag diagnosticRecorder
	// Nil keeps the approved default reserve of one; explicit zero opts out.
	flowReservedPerCaller *int
	// policyFreshness 是运行期策略新鲜度的只读面（FR-25C-16）：nil = 本路径不设门
	// （`chat` / `tool` 是单进程、启动时装载策略的轻路径，不得因几分钟没刷新被判过期）。
	// policyMaxStale 取 `policy.max_stale`；`now` 为 nil 时用系统时钟（测试注入假时钟）。
	// A nil freshness face disables the gate; the chat/tool paths stay ungated.
	policyFreshness PolicyFreshness
	policyMaxStale  time.Duration
	now             func() time.Time
}

// run 取名额 → 执行 → 归还。`next` 是真正跑 Run 的函数（core 运行入口）。
// run takes the slot, runs, and always returns the slot.
func (a *runAdmission) run(
	ctx context.Context,
	req core.RunRequest,
	next func(context.Context, core.RunRequest) (core.RunResult, error),
) (res core.RunResult, err error) {
	if req.Flow != nil && (a == nil || a.limiter == nil) {
		return core.RunResult{}, core.ErrFlowDispatch
	}
	// 策略新鲜度门（FR-25C-16）排在取名额之前：过期时**任何**新 Run 都不得进入执行路径，
	// 也不占名额。刷新由控制面 / Worker 的后台 `PolicyRefresher` 恢复，成功一轮即自动放行。
	// The freshness gate sits ahead of every slot: a stale policy never occupies one.
	if staleErr := a.checkPolicyFreshness(); staleErr != nil {
		a.logWarn("refusing run: runtime policy snapshot is stale", "error", staleErr,
			"agent", req.Agent, "run_id", req.RunID)
		return core.RunResult{}, staleErr
	}
	if a != nil && a.limiter != nil {
		var ticket callers.RunTicket
		var admitErr error
		if req.Flow != nil {
			reserved := 1
			if a.flowReservedPerCaller != nil {
				reserved = *a.flowReservedPerCaller
			}
			ticket, admitErr = a.limiter.AdmitFlowRun(ctx, req.Principal, reserved)
		} else {
			ticket, admitErr = a.limiter.AdmitRun(ctx, req.Principal)
		}
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

	// [4] 预检前移（FR-24-34）：调用方并发是准入的第一判据段，必须排在**任何**容量类
	// 拒绝之前。`worker.max_inflight` 满额与实例上限同码（TOO_MANY_RUNS）；先取槽就会让
	// "跨 Worker 调用方名额已满 + 本进程名额池也满"返回 TOO_MANY_RUNS，而契约要求
	// CALLER_CONCURRENCY_LIMITED（模块卡 §4.5 的负向用例）。core 的 [4] 仍然保留——CLI
	// 与其他直接调用不经过本层；两处都是只读预检，权威复查在 `StartRun` 事务内。
	// The precheck moves ahead of the pooled slot: a full inflight pool is a capacity
	// verdict (same code as the instance cap) and must not outrank the cross-worker
	// caller-concurrency verdict. The precheck in core stays for direct callers.
	if precheckErr := a.precheckAdmission(ctx, req); precheckErr != nil {
		return core.RunResult{}, precheckErr
	}

	// 池化在途名额（US-24 FR-24-40 / 图 11 [1]）：三条执行路径共用同一个信号量。
	// 交互 Run 取槽非阻塞（满额立即 TOO_MANY_RUNS，不排队）；后台路径阻塞可取消。
	// 名额在调用方名额**之后**取：CALLER_CONCURRENCY_LIMITED 的优先级高于容量判定
	// （FR-24-34），被拒的 Run 不占任何名额。
	// The pooled slot is taken after the caller slot: the caller-concurrency verdict
	// ranks above capacity, and a rejected run holds nothing.
	if slot, slotErr := a.acquireInflight(ctx, req); slotErr != nil {
		return core.RunResult{}, slotErr
	} else if slot != nil {
		// 归还必须来自本层 defer：正常完成、失败、取消、panic 都会走到这里。
		// The slot is returned by this layer's defer on every outcome.
		defer slot()
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

// checkPolicyFreshness 是 FR-25C-16 的准入判据：最近一次四类策略**全部成功**的核对
// 距今超过 `policy.max_stale`，或从未成功过 → `core.ErrPolicyStale`（web 映射 503
// `POLICY_STALE`）。过期判定只认成功核对时间戳，与内容版本无关——内容长期不变不构成
// 过期，只有刷新失败让时间戳停在原地。`policyFreshness` 为 nil = 本路径不设门。
// Staleness follows only the last fully successful round; nil disables the gate.
func (a *runAdmission) checkPolicyFreshness() error {
	if a == nil || a.policyFreshness == nil {
		return nil
	}
	now := time.Now()
	if a.now != nil {
		now = a.now()
	}
	verified := a.policyFreshness.LastVerified()
	if verified.IsZero() {
		return &core.AdminError{
			Kind:    core.ErrPolicyStale,
			Message: "运行期策略尚未完成一次成功核对（允许域 / 拒绝域 / 访问例外 / 执行档位）：确认策略表可达，刷新成功后自动放行",
		}
	}
	maxStale := a.policyMaxStale
	if maxStale <= 0 {
		maxStale = defaultPolicyMaxStale
	}
	if age := now.Sub(verified); age > maxStale {
		return &core.AdminError{
			Kind: core.ErrPolicyStale,
			Message: fmt.Sprintf(
				"运行期策略快照已过期：最近一次成功核对在 %s（距今 %s，超过 policy.max_stale）；刷新恢复后自动放行",
				verified.Format(time.RFC3339), age.Round(time.Second)),
		}
	}
	return nil
}

// precheckAdmission 在取在途名额之前给出调用方并发 / 会话的只读结论。未接线
// （runs 或 admission 为 nil）时不做任何事——轻装配与既有单测的行为保持不变。
// precheckAdmission runs the read-only verdict before the pooled slot; an unwired
// admission is a no-op so light assemblies keep their prior behaviour.
func (a *runAdmission) precheckAdmission(ctx context.Context, req core.RunRequest) error {
	if a == nil || a.runs == nil || a.admission == nil {
		return nil
	}
	admission, err := a.admission.For(req.Principal)
	if err != nil {
		return err
	}
	admission.IsFlow = req.Flow != nil
	return a.runs.CheckAdmission(ctx, req.SessionID, req.Principal, admission)
}

// acquireInflight 判定这次 Run 走哪一档取槽语义：`Flow != nil`（Flow 节点 Run）或
// `Trigger != ""`（只有调度器填 cron / manual，web 与 CLI 留空）都是后台执行；
// 其余是交互 Run。装配层不另立字段，判定只用既有请求面。
// acquireInflight classifies the run: a flow leaf or any scheduler-triggered run is
// background; everything else is interactive.
func (a *runAdmission) acquireInflight(ctx context.Context, req core.RunRequest) (func(), error) {
	if a == nil || a.inflight == nil {
		return nil, nil
	}
	if req.Flow != nil || req.Trigger != "" {
		return a.inflight.acquireBackground(ctx)
	}
	return a.inflight.acquireInteractive()
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

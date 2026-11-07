package app

// 本文件把 `callers.Limiter` 适配成 core 声明的 `core.RunQuota` 端口（US-16 N-01）：
// 一次逻辑 Run 准入 = 1 个任务次数，记在 `ClassRun` 名下。
//
// 三件事只在这里发生，core 一概不解释：
//   - **计数单位**：恒为 1（`runQuotaTasksPerRun`）——与模型调用次数、工具轮次都无关，
//     那些走 `chatQuota` / 摘要闸门，各有自己的幂等键；
//   - **幂等键**：`run:<run_id>:admit`，逐字对齐颗粒度文档「幂等键规则」表。键只由
//     run_id 派生：重放同一 Run 不会产生第二条预留，也不会二次扣减限额；
//   - **结算时机**：预留成功后**立即**结算（用量在准入时就已确知 = 1）。不像对话调用
//     要等供应商回用量——把已确知的消耗留在 `pending` 只会白占额度到管理面核对。
//
// 两条失败口径（与"已准入 = 已消耗"一致）：
//   - 预留失败 → 原样上抛（`errors.Is(err, callers.ErrQuotaExhausted)` 必须在运行
//     入口仍成立，契约 429 `QUOTA_EXHAUSTED` 的判定锚点是 callers 的哨兵）；
//   - 结算失败 → **不释放**、也不让 Run 失败：预留继续占额度（限额因此仍被强制），
//     由管理面 `Reconcile` 收口（颗粒度文档「提交结果未知」行）。例外是
//     `callers.ErrReservationSettled`：该 run_id 的预留已被管理面收敛成终态，放行
//     等于让这次执行绕过限额——失败关闭，把原因交给日志与入口错误映射。
//
// This file adapts callers.Limiter to the core RunQuota port: one task per logical run,
// idempotent on run:<run_id>:admit, settled immediately, never auto-released.
//
// US-24 P-12 起准入路径**不再调用** `ChargeRun`：任务次数记账改由仓储在 `StartRun`
// 的同一事务内完成（`chargeRunTaskQuotaTx`，幂等键同为 `run:<run_id>:admit`，
// FR-24-03 / FR-24-33），与 Run 行、清单、引用、lease 同生共死；`core.Service.Run`
// 因此删掉了这里的一笔调用（重复记账会把同一次准入扣两次）。本适配器仍保留并接线：
//   - `ServiceOptions.RunQuota` 非 nil 是 Flow 准入的前置（service.go 的 Flow 守卫），
//     零值会让 Flow 请求在装配层被误判为未就绪；
//   - 端口是 `core.RunQuota` 的公开形状，非仓储装配（轻装配 / 测试替身）仍可用它。
// 任何路径都**不得**让本适配器与事务内记账同时生效——那是同一次准入的两条记账路径。
// Since US-24 P-12 the admission charge commits inside StartRun's transaction; this
// adapter remains wired as the core.RunQuota port but must never double-charge.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

const (
	// runQuotaTasksPerRun 是每次逻辑 Run 的任务次数：固定 1，不随模型调用 / 工具轮次
	// 增长。调大它就等于改变契约语义（「预留 / 结算口径」表冻结为 1）。
	// One logical run always costs exactly one task.
	runQuotaTasksPerRun = 1

	// runQuotaDiagnosticClass 是诊断事件的类目值，与 `runAdmission` 的 `run`（并发
	// 名额）区分开：两者都在同一次准入上判定，但一个是"同时跑几个"，一个是
	// "这段时间能跑几个"，排障时不能混为一谈。
	// The diagnostic class separates the task count from the concurrency slot.
	runQuotaDiagnosticClass = "run_task"

	// runQuotaSettleTimeout 是结算的独立截止。预留一旦落下就必须有人把它从 `pending`
	// 收口；调用方 ctx 取消不改变"这次准入已经发生"的事实，所以结算用
	// `context.WithoutCancel` 派生的独立窗口（`callers.Commit` 本身也不看 ctx.Err()）。
	// Settling gets its own deadline: a cancelled caller does not undo a reservation.
	runQuotaSettleTimeout = 5 * time.Second
)

// runQuota 是 `core.RunQuota` 的生产实现；`limiter` 为 nil 表示未接线（轻装配与
// 既有单测）：不预留、不结算，行为与接线前逐字一致。
// A nil limiter means the run-level task count is not wired.
type runQuota struct {
	limiter *callers.Limiter
	logger  *slog.Logger
	// diag 是配额判定诊断（V-8）；nil = 未接诊断，判定照常、只是不留事件。
	// diag records the verdict; nil simply means no event.
	diag diagnosticRecorder
}

var _ core.RunQuota = (*runQuota)(nil)

// newRunQuota 装配适配器；三个参数都允许为 nil（零值即"不记账 / 不打日志 / 不留事件"）。
// newRunQuota builds the adapter; every argument may be nil.
func newRunQuota(limiter *callers.Limiter, diag diagnosticRecorder, logger *slog.Logger) *runQuota {
	return &runQuota{limiter: limiter, logger: logger, diag: diag}
}

// ChargeRun 预留并结算 1 个任务次数，返回写进 `runs.reservation_id` 的预留标识。
// ChargeRun reserves and settles one task for one admitted run.
func (q *runQuota) ChargeRun(ctx context.Context, req core.RunQuotaRequest) (core.RunQuotaResult, error) {
	if q == nil || q.limiter == nil {
		return core.RunQuotaResult{}, nil
	}
	ticket, err := q.limiter.AdmitCall(ctx, req.Principal, callers.ClassRun,
		callers.Cost{Tasks: runQuotaTasksPerRun}, runQuotaIdempotencyKey(req.RunID))
	// 判定先留痕再返回（V-8）：放行让"这个 Run 为什么没跑 / 占了多少任务额度"能和
	// 账本对上；拒绝回答 429 的来源。诊断可丢弃，判定结论不受影响。
	// The verdict is recorded before returning; a failed write never changes it.
	recordQuotaDecision(ctx, q.diag, quotaDecision{
		RunID: req.RunID,
		Class: runQuotaDiagnosticClass,
		Err:   err,
	})
	if err != nil {
		// 原样上抛（见文件头）：入口据 `callers.ErrQuotaExhausted` 映射 429。
		return core.RunQuotaResult{}, err
	}
	if ticket.NoReservation {
		// 内置管理员不限量：没有预留行可结算，账本列留空。
		return core.RunQuotaResult{}, nil
	}

	// 结算用独立窗口：调用方取消不改变"准入已经发生"，预留必须有人收口。
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runQuotaSettleTimeout)
	defer cancel()
	if _, err := q.limiter.Commit(settleCtx, ticket, callers.Usage{Tasks: runQuotaTasksPerRun}); err != nil {
		if errors.Is(err, callers.ErrReservationSettled) {
			// 该 run_id 的预留已被管理面收敛为 released / reconciled：按"已记账"放行
			// 等于让这次执行绕过限额。生产路径的 run_id 每次唯一，走到这里说明有调用方
			// 在重放已收口的 Run——失败关闭，原因交给日志与入口错误映射。
			// A terminal reservation for this run id must not be treated as settled spend.
			q.warn("Run 任务次数的预留已被管理面收口——拒绝本次准入",
				"run_id", req.RunID, "reservation_id", ticket.ReservationID, "error", err)
			return core.RunQuotaResult{}, err
		}
		// 结果未知：预留保持 pending、继续占额度（限额仍被强制），Run 照常开始；
		// **不**释放——无法证明这次准入没有消耗。管理面 `Reconcile` 收口。
		q.warn("Run 任务次数结算结果未知——预留保持 pending 占额",
			"run_id", req.RunID, "caller_id", ticket.CallerID,
			"reservation_id", ticket.ReservationID, "error", err)
	}
	return core.RunQuotaResult{ReservationID: ticket.ReservationID}, nil
}

// runQuotaIdempotencyKey 按颗粒度文档「幂等键规则」表派生：`run:<run_id>:admit`。
// 末位是 `admit` 而不是尝试标识——任务次数记的是**逻辑准入**，一次准入重放多少次
// 都只算一次（与对话调用的 `:<attempt_id>` 口径不同，后者记的是每次真的发出）。
// The key marks the logical admission, not an attempt.
func runQuotaIdempotencyKey(runID string) string {
	return "run:" + runID + ":admit"
}

// warn 在日志器缺失时静默（轻装配测试不强制注入日志器）。
func (q *runQuota) warn(msg string, args ...any) {
	if q != nil && q.logger != nil {
		q.logger.Warn(msg, args...)
	}
}

package core

import "context"

// 本文件是图 6 [5] 的 **Run 粒度**端口（`ClassRun` 任务次数，US-16 N-01）。它和
// `CallQuota` 是一对：`CallQuota` 记"一次模型 / 工具调用"，本端口记"一次逻辑 Run"，
// 两者共用同一个限制器，但**不得**互相折算（一个 Run 可以有任意多次调用）。
//
// 口径（与 `docs/us/us16-knowledge-memory-history.md` 的「幂等键规则」「预留 / 结算
// 口径」两表逐条对齐）：
//   - **计数单位**：一次逻辑 Run = 1，与模型调用次数、工具轮次、消息条数都无关；
//   - **幂等键**：`run:<run_id>:admit`，由 run_id 派生——重放同一 Run 不产生第二条
//     预留，也不会二次扣减限额；
//   - **时机**：调用方**必须在业务执行之前**完成预留与结算。任务次数在准入时已确知
//     （恒为 1），没有"稍后才知道"的余地，所以结算不必等 Run 结束——先占后跑、跑完
//     再结算等于给并发窗口留出超额放行的口子；
//   - **失败释放**：本端口**没有**自动释放路径。预留失败不产生任何行；结算结果未知
//     时适配层保持 `pending`（继续占额度，由管理面 `Reconcile` 收口）——一次已完成
//     的准入就是一次已发生的消耗，任何后续失败都不退还（与可归还的并发名额不同，
//     那条路径由 `admit` / `release` 承担）；
//   - **未接线**：端口为 nil = 不记账（既有零值单测与轻装配），行为与接线前逐字节
//     相同；生产装配必须在 `wire.go` 单点接上。
//
// 接口定义在消费方（技术方案 §7.1.2 第 2 条）：`callers` 已经 import `core`，端口
// 只能声明在这里，`callers.Limiter` 的适配放在装配层。
// The port is declared by its consumer; the adapter over `callers.Limiter` lives in app.

// RunQuota 是一次逻辑 Run 的任务次数端口：每个逻辑 Run 预留并结算 1 个任务次数。
// RunQuota is the per-logical-run task-count port.
type RunQuota interface {
	// ChargeRun 为一次逻辑 Run 预留并结算 1 个任务次数，返回预留标识（写进
	// `runs.reservation_id`）。返回错误即准入期拒绝：Run **不**开始、不落账本行、
	// 不写审计、不调用模型，由运行入口按各自错误契约映射（web 侧 429
	// `QUOTA_EXHAUSTED` 等）。
	// ChargeRun reserves and settles one task before any business write; an error is an
	// admission refusal that leaves no trace and starts nothing.
	ChargeRun(ctx context.Context, req RunQuotaRequest) (RunQuotaResult, error)
}

// RunQuotaRequest 是一次 Run 级任务次数的申请。
// RunQuotaRequest is one logical run's task-count request.
type RunQuotaRequest struct {
	// Principal 决定记到哪个调用方名下；零值身份由适配层映射为 `ErrUnauthorized`
	// （与 `admit` 的 [3] 同口径，不补成内置管理员）。
	// Principal decides whose quota is charged; a zero value is refused, never defaulted.
	Principal Principal
	// RunID 是幂等键的派生源（`run:<run_id>:admit`），也是账本与诊断的关联标识。
	RunID string
}

// RunQuotaResult 是记账结果。
// RunQuotaResult is the outcome of one run-level charge.
type RunQuotaResult struct {
	// ReservationID 是 `quota_reservations.reservation_id`；空表示本次不记账
	// （内置管理员不限量）。账本行按原值写入。
	// ReservationID is empty when nothing was reserved (an unlimited built-in admin).
	ReservationID string
}

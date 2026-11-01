package core

// 本文件是图 6 [5]「每次真实消耗的配额预留」在 **core 侧**的端口声明（C-29 / C-35）。
// This file declares the per-call quota port for admission stage [5].
//
// 三条纪律：
//   - **接口定义在消费方**：`internal/callers` 已经 import `core`（`AdmitCall` 收
//     `core.Principal`），故 core **不得**反向 import callers——否则成环。core 只声明
//     它需要的三个动作，`app` 装配层用 `callers.Limiter` 适配（技术方案 §7.1.2 第 2 条）。
//   - **每次真实调用一次**：`AdmitCall` 在调用**发出之前**申请（顺序上是 [5] 在 [6]
//     审计预写之前），不占并发名额、不扣入口速率——那两件事分属 [3] 与 [2]。
//   - **结算按"确知"分流**：已知用量 → `CommitCall`；确定未发出 → `ReleaseCall`；
//     其余（超时 / 取消 / 供应商错）**保持 pending**，由管理面 `Reconcile` 收口。
//     "不确定"不得按零提交，也不得凭空释放。
//
// The port is declared here because callers already imports core; the adapter lives in
// app. One admission per actually-issued call, and settlement follows what is known.

import "context"

// CallQuota 是一次 Run 内模型调用的配额端口（图 6 [5]）。
// CallQuota is admission stage [5] for model calls made inside a run.
type CallQuota interface {
	// AdmitCall 在调用发出**之前**预留本次调用的额度。返回的错误按"准入期拒绝"
	// 处理：循环**不**捕获成 failed 的 RunResult，而是原样上抛给运行入口，
	// 由调用方按各自错误契约（429 `QUOTA_EXHAUSTED` 等）映射。
	// 返回 nil 票据表示"本次不记账"（未接线或内置管理员不限量）。
	// AdmitCall reserves before the call is issued; a nil ticket means nothing to settle.
	AdmitCall(ctx context.Context, req CallQuotaRequest) (CallQuotaTicket, error)

	// CommitCall 结算**已知**的实际用量（`pending → committed`，余额自动释放）。
	// 只有确认调用成功且用量可用时才调用；用量未知**不得**调用（C-29）。
	// CommitCall settles known usage only; unknown usage must stay pending.
	CommitCall(ctx context.Context, ticket CallQuotaTicket, usage Usage) error

	// ReleaseCall 归还有证据表明**确定未消耗**的预留。`cause` 是导致未发出的
	// 原始错误——适配层据此选释放理由（审计预写失败 / 其它未发出），core 不解释。
	// ReleaseCall returns a reservation proven unconsumed; the adapter classifies cause.
	ReleaseCall(ctx context.Context, ticket CallQuotaTicket, cause error) error
}

// CallQuotaRequest 是一次真实模型调用的配额申请。
// CallQuotaRequest describes one actually-issued model call.
type CallQuotaRequest struct {
	// Principal 决定记到哪个调用方名下；零值身份由适配层映射为 `ErrUnauthorized`。
	// Principal decides which caller the reservation belongs to.
	Principal Principal
	// RunID 与 Round 是审计关联标识的一部分，也用于拒绝可追溯。
	// RunID and Round tie the reservation to the audit trail.
	RunID string
	Round int
	// EstimatedInput 是组装层算出的输入 token 估算（`estimateTokens`），
	// 适配层在此基础上加输出预留得到本次预留量。
	// EstimatedInput is the prompt estimate; the adapter adds its output reserve.
	EstimatedInput int
	// IdempotencyKey 由调用方按 C-29 生成，形如
	// `run:<run_id>:chat:<step_id>:<attempt_id>`；同一键重放不得产生第二条预留。
	// IdempotencyKey follows C-29; replaying the same key never doubles a reservation.
	IdempotencyKey string
}

// CallQuotaTicket 是预留的不透明票据：core 不解释它，原样交回适配层。
// nil = 本次不记账（未接线或内置管理员不限量），后续结算一律跳过。
// CallQuotaTicket is opaque to core; nil means there is nothing to settle.
type CallQuotaTicket any

// usageReported 判定供应商是否真的给了用量。三列全零既可能是"确认零消耗"也可能是
// "没返回 usage"——在 core 这一层**无法区分**，按卡片口径取保守解：保持 pending
// 由管理面核对，**不**按零提交（`TestQuota_AuditFailureReleasesReservation` 的反面）。
// usageReported reports whether the provider returned any usage at all: all-zero is
// indistinguishable from "not reported", so it must not be committed as zero.
func usageReported(u Usage) bool {
	return u.TotalTokens > 0 || u.PromptTokens > 0 || u.CompletionTokens > 0
}

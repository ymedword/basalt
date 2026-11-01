package app

// 本文件把 `callers.Limiter` 适配成 core 声明的 `core.CallQuota` 端口（图 6 [5]）。
// This file adapts callers.Limiter to the core-declared CallQuota port (stage [5]).
//
// 三件事只在这里发生，core 一概不解释：
//   - **预留量策略**：Token 预留 = 组装层估算输入 + 输出预留（`chatReserveTokens`）；
//   - **用量换算**：`core.Usage` 的三列 → `callers.Usage{Tokens}`（单维度账本）；
//   - **释放理由**：`core.ErrAuditWrite` → `callers.ReleaseAuditFailed`，
//     其余未发出原因 → `callers.ReleaseNotSent`。
//
// 错误**原样上抛**：`errors.Is(err, callers.ErrQuotaExhausted)` 必须在运行入口仍然
// 成立（契约 429 `QUOTA_EXHAUSTED` 的判定锚点是 callers 的哨兵），因此这里不包装、
// 不翻译、不吞。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

const (
	// chatOutputReserveTokens 是单次对话调用的**输出预留**。
	//
	// 本项目没有对话级 `max_tokens` 配置键（`internal/cli/config.go` 只有
	// `compaction.summary_max_tokens` 与 `knowledge.chunk.max_tokens`），而模块卡
	// `:883` 的公式是"估算输入 + max_tokens 输出"。新增配置键属软门禁，因此本 US 取
	// 常量：它是**预留上限**而不是硬截断——实际用量在 Commit 时按供应商 usage 结算，
	// 超出预留的部分记 `overage`（openapi `usage/clear-overage` 由管理面核对清零）。
	// 数值属待实测冻结项，已登记 run-state 未验证清单（R114）。
	//
	// The chat output reserve is a constant because the project has no per-call
	// max_tokens key; it caps the reservation, not the call.
	chatOutputReserveTokens = 4096

	// chatReserveFloorTokens 是预留下限：输入估算为 0（空提示词）时账本仍要有一笔
	// 正数预留，否则 `Cost` 全零会被 `validate` 判成非法成本。
	// A non-positive estimate still needs a reservable amount.
	chatReserveFloorTokens = 1
)

// chatQuota 是 `core.CallQuota` 的生产实现；`limiter` 为 nil 表示未接线（轻装配与
// 既有单测）：不预留、不结算，行为与接线前逐字一致。
// A nil limiter means stage [5] is not wired.
type chatQuota struct {
	limiter *callers.Limiter
	// tokens 是预留量策略的注入位：生产用 `chatReserveTokens`，测试夹具可固定成
	// 卡片要求的常量（`TestRunService_UnknownUsageReleasesRunSlot` 断言预留 100）。
	// 注入位只影响"预留多少"，不改变维度（`ClassChat` 恒记 Token）。
	// The injectable policy decides how much to reserve, never which dimension.
	tokens func(core.CallQuotaRequest) int64
	logger *slog.Logger
	// diag 是配额判定诊断（V-8）；nil = 未接诊断，判定照常、只是不留事件。
	// diag records the quota verdict; nil simply means no event.
	diag diagnosticRecorder
}

var _ core.CallQuota = (*chatQuota)(nil)

// chatReserveTokens 是生产预留量：估算输入 + 输出预留，下限 `chatReserveFloorTokens`。
// chatReserveTokens is the production reservation: estimated input plus output reserve.
func chatReserveTokens(req core.CallQuotaRequest) int64 {
	reserve := int64(req.EstimatedInput) + chatOutputReserveTokens
	if reserve < chatReserveFloorTokens {
		return chatReserveFloorTokens
	}
	return reserve
}

// reserveTokens 在注入位缺失时回退到生产策略。
func (q *chatQuota) reserveTokens(req core.CallQuotaRequest) int64 {
	if q != nil && q.tokens != nil {
		return q.tokens(req)
	}
	return chatReserveTokens(req)
}

// AdmitCall 是图 6 [5]：每次真正发出的对话调用预留一次。内置管理员不限量时
// `callers` 返回 `NoReservation` 票据——本层直接交回 nil，让结算整段跳过。
// AdmitCall reserves once per issued call; the unlimited admin has nothing to settle.
func (q *chatQuota) AdmitCall(ctx context.Context, req core.CallQuotaRequest) (core.CallQuotaTicket, error) {
	if q == nil || q.limiter == nil {
		return nil, nil
	}
	ticket, err := q.limiter.AdmitCall(ctx, req.Principal, callers.ClassChat,
		callers.Cost{Tokens: q.reserveTokens(req)}, req.IdempotencyKey)
	// 判定先留痕再返回（V-8）：放行与拒绝都记——拒绝回答"为什么这轮没发出"，放行让
	// "这一轮花了多少额度"能和 `llm_call` / 装配记录对上。诊断可丢弃，判定结论不受影响。
	// The verdict is recorded before returning; a failed write never changes it.
	recordQuotaDecision(ctx, q.diag, quotaDecision{
		RunID:  req.RunID,
		StepID: strconv.Itoa(req.Round),
		Seq:    int64(req.Round),
		Class:  "chat",
		Err:    err,
	})
	if err != nil {
		// 原样上抛（见文件头）：调用方据 `callers.ErrQuotaExhausted` 映射 429。
		return nil, err
	}
	if ticket.NoReservation {
		return nil, nil
	}
	return ticket, nil
}

// CommitCall 结算已知用量。供应商未给任何 Token 时**不提交**——全零在 core 与
// 本层都无法与"没返回用量"区分，按 C-29 保持 `pending` 等管理面核对。
// CommitCall settles known usage; an all-zero report stays pending.
func (q *chatQuota) CommitCall(ctx context.Context, ticket core.CallQuotaTicket, usage core.Usage) error {
	if q == nil || q.limiter == nil {
		return nil
	}
	t, ok := ticket.(callers.Ticket)
	if !ok {
		// 票据不是本适配器发的：不解释、不猜测，直接返回（nil 票据已在 core 侧短路）。
		q.warn("配额票据类型不匹配——跳过结算", "ticket_type", fmt.Sprintf("%T", ticket))
		return nil
	}
	tokens := chatUsageTokens(usage)
	if tokens <= 0 {
		q.warn("模型调用未返回用量——配额预留保持 pending", "caller_id", t.CallerID)
		return nil
	}
	settlement, err := q.limiter.Commit(ctx, t, callers.Usage{Tokens: tokens})
	if err != nil {
		return err
	}
	// 超预留（`actual > reserved`）不是错误：账本按实际记账并留下 `overage`，管理面
	// 核对清零后才恢复新预留（openapi `usage/clear-overage`）。这里只警告，便于发现
	// 预留量偏小（例如输出预留常量需要上调）——**不得**在这里改动账本。
	if settlement.Overage > 0 {
		q.warn("实际用量超出预留——已记 overage，需管理面核对清零",
			"caller_id", settlement.CallerID, "reserved", settlement.Amount,
			"actual", settlement.Actual, "overage", settlement.Overage)
	}
	return nil
}

// ReleaseCall 归还**确定未消耗**的预留：审计预写失败是一个独立理由（`audit_failed`，
// 与"连接都没建立"区分得开），其余未发出原因统一记 `not_sent`。
// ReleaseCall returns a provably unconsumed reservation with the matching reason.
func (q *chatQuota) ReleaseCall(ctx context.Context, ticket core.CallQuotaTicket, cause error) error {
	if q == nil || q.limiter == nil {
		return nil
	}
	t, ok := ticket.(callers.Ticket)
	if !ok {
		q.warn("配额票据类型不匹配——跳过归还", "ticket_type", fmt.Sprintf("%T", ticket))
		return nil
	}
	reason := callers.ReleaseNotSent
	if errors.Is(cause, core.ErrAuditWrite) {
		reason = callers.ReleaseAuditFailed
	}
	return q.limiter.Release(ctx, t, reason)
}

// warn 在日志器缺失时静默（轻装配测试不强制注入日志器）。
func (q *chatQuota) warn(msg string, args ...any) {
	if q != nil && q.logger != nil {
		q.logger.Warn(msg, args...)
	}
}

// chatUsageTokens 把协议三列换算成账本的单维度 Token 数：优先 `TotalTokens`，
// 缺省时用输入与输出之和。负值按 0 处理（账本不接受负用量，`Commit` 会拒绝）。
// chatUsageTokens folds the three protocol columns into the ledger's single dimension.
func chatUsageTokens(usage core.Usage) int64 {
	total := int64(usage.TotalTokens)
	if total == 0 {
		total = int64(usage.PromptTokens) + int64(usage.CompletionTokens)
	}
	if total < 0 {
		return 0
	}
	return total
}

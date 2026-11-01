package app

// 本文件把**查询侧** embedding 动作接进 `embedding_calls` 的两阶段审计（原则 V：
// 审计 day-one 且 fail-closed）。摄取侧由 `knowledge.Ingestor` 自己在每批调用前
// 预写 `pending`（见 `internal/knowledge/ingest.go` 的 `embedBatch`）；查询侧
// （管理面 `/knowledge/search` 与 `recall_memory` 的语义路径）走的也是同一个外部
// 提供方调用，**不得**因为"它只发一次、批量只有 1 条"就绕开审计。
//
// 分工是"各写各的"：摄取器拿裸客户端并自己审计，查询侧拿本装饰器并自己审计。
// 两处**不得**嵌套——若把装饰器套在摄取器那一层，每批会写出两行审计（一行由
// `embedBatch` 写、一行由装饰器写），调用数就被双记了。
//
// This file routes query-side embedding through the two-phase embedding_calls audit.
// Ingest audits its own batches; search and semantic recall go through this
// decorator. The two must not nest, or every ingest batch would be audited twice.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/knowledge"
)

// auditFinishTimeout 是审计终态回填的独立超时（`run-and-errors.md` §4.2：终态写脱离
// 请求取消，但**不得**无限期挂住）。没有它时，数据库连接池卡死会让一次已经买过的调用
// 永久停在"在途"形态，调用方也永远回不来。
// auditFinishTimeout bounds the detached terminal write; without it a stalled pool
// would hang the caller forever.
const auditFinishTimeout = 5 * time.Second

// auditedEmbedder 在查询侧 embedding 客户端外面套两阶段审计：
//
//   - **预写失败 → 不发出调用**（fail-closed）：返回错误，内层客户端的
//     `Embed` 一次都不会被调用；
//   - **调用返回 → 终态照写**：成功写 `success` 与用量，失败写终态与失败分类；
//     终态写失败只 WARN 不覆盖调用结果——调用已经发生，把"审计回填失败"报成
//     "embedding 失败"会让上游以为没花钱，反而更难核对；
//   - **终态写用 `context.WithoutCancel` + 独立短超时**：请求被取消不应连带丢掉
//     终态行，否则该行永远停在 `pending`，核对路径无法区分"在途"与"写丢了"；反过来，
//     脱了取消的写也**不得**无限期等待（契约 §4.2）——`auditFinishTimeout` 是那个界。
//
// 身份字段：`EmbeddingAudit.BeginEmbedding` 只有 `jobID` 一个关联入参，查询侧没有
// 摄取作业号，因此传空串——仓储把它落成 NULL，DDL 的
// `num_nonnulls(run_id, ingest_job_id) <= 1` 允许 `run_id` / `ingest_job_id` 都为空
// （启动期回填同形）。查询侧**不**伪造作业号去凑关联。
//
// auditedEmbedder wraps the query-side embedder with the fail-closed pre-write and a
// detached terminal write. Query-side calls carry no ingest job id, so both
// correlation columns stay NULL — the schema allows it and we never fabricate one.
type auditedEmbedder struct {
	// inner 是真正的协议客户端（`*knowledge.HTTPEmbedder`）。
	inner knowledge.Embedder
	// audit 是 `embedding_calls` 的唯一 SQL 入口（`*postgres.EmbeddingCallRepo`）。
	audit knowledge.EmbeddingAudit
	// model 是审计行记录的模型 id：取配置里生效的 `embedding.model`，与内层客户端
	// 实际请求的模型同源（装配时已校验非空），不在这里从回执里反推。
	model string
	// logger 记录终态写失败；装配层保证非 nil。
	logger *slog.Logger
}

// 编译期断言：装饰器本身仍是 `knowledge.Embedder`，可以直接替换裸客户端。
// Compile-time assertion that the decorator is still a knowledge.Embedder.
var _ knowledge.Embedder = (*auditedEmbedder)(nil)

// Embed 执行一次带审计的查询侧 embedding。
// Embed runs one audited query-side embedding call.
func (a *auditedEmbedder) Embed(ctx context.Context, texts []string) (knowledge.EmbedBatch, error) {
	callID, err := a.audit.BeginEmbedding(ctx, "", a.model, len(texts))
	if err != nil {
		// 预写不成功就**不发**：这是原则 V 与 `run-and-errors.md` §7.2 的同一句话。
		// No pending row, no outgoing call.
		return knowledge.EmbedBatch{}, fmt.Errorf("查询侧 embedding 审计预写失败，本次调用未发出: %w", err)
	}

	batch, callErr := a.inner.Embed(ctx, texts)
	// 失败时用量未知：与摄取侧同口径写 0（`unknown` 结局下用量不是已知事实）。
	tokens := int64(0)
	if callErr == nil {
		tokens = batch.PromptTokens
	}
	a.finish(ctx, callID, tokens, callErr)
	return batch, callErr
}

// finish 写审计终态。失败只记 WARN：调用已经发生，行停在 `pending` 由核对路径
// 收口；**不得**把失败吞掉不记，也不得让审计失败改写调用结果。
// finish writes the terminal state; failures are logged and never override the call
// result.
func (a *auditedEmbedder) finish(ctx context.Context, callID string, promptTokens int64, callErr error) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditFinishTimeout)
	defer cancel()
	if err := a.audit.FinishEmbedding(finishCtx, callID, promptTokens, callErr); err != nil {
		a.logger.WarnContext(ctx, "knowledge: 查询侧 embedding 审计终态写入失败",
			"call_id", callID, "error", err)
	}
}

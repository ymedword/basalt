package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// 本文件是 embedding_calls 两阶段写的**唯一 SQL 入口**（审计 SQL 只允许出现在
// internal/storage——全局不变量 3）。列与状态字面量以 docs/contracts/schema.pg.sql
// 为准，此处不另抄列清单。
// This file is the only SQL entry point for embedding_calls: pre-write a pending row
// before the provider call, then fill in the terminal state after it returns.

// embedding_calls.status 的四值（与 schema.pg.sql 的 CHECK 逐字一致）。
// The four status literals of embedding_calls, verbatim from the schema CHECK.
const (
	embeddingStatusPending = "pending"
	embeddingStatusSuccess = "success"
	embeddingStatusFailed  = "failed"
	embeddingStatusUnknown = "unknown"
)

// EmbeddingCallRepo 是 embedding_calls 的唯一 SQL 入口，实现摄取侧声明的
// `knowledge.EmbeddingAudit`。
// EmbeddingCallRepo is the only SQL entry point for embedding_calls and satisfies
// the ingest-side knowledge.EmbeddingAudit port.
//
// `dimensions` 在构造期固定：列是 NOT NULL，而 `EmbeddingAudit` 的签名里没有它——
// 维度是**装配属性**（等于冻结的 `knowledge.VectorDimensions`，摄取器装配时已校验），
// 不是每次调用各不相同的参数，因此不在端口上再开一个可能传错的入参。
// Dimensions are fixed at construction: the column is NOT NULL while the port has no
// such parameter, and the value is an assembly property, not a per-call choice.
type EmbeddingCallRepo struct {
	db         *DB
	dimensions int
}

// 编译期断言：仓储满足消费方（knowledge）声明的两阶段审计端口。
// Compile-time assertion that the repo satisfies the consumer-declared audit port.
var _ knowledge.EmbeddingAudit = (*EmbeddingCallRepo)(nil)

// NewEmbeddingCallRepo 绑定已打开的写模式库句柄与冻结维度。
// NewEmbeddingCallRepo binds the repo to an opened writable handle and the frozen
// vector dimensions.
func NewEmbeddingCallRepo(db *DB, dimensions int) *EmbeddingCallRepo {
	return &EmbeddingCallRepo{db: db, dimensions: dimensions}
}

// BeginEmbedding 插入 status=pending 的审计行并返回行 id（十进制文本）；失败 =
// 调用方**拒绝发出**该次调用（fail-closed，与 llm_calls 的预写同口径）。
// BeginEmbedding inserts the pending audit row and returns its id as decimal text;
// a failure here means the caller must not make the call.
//
// 预写形态：`run_id` 留空（摄取路径只带 `ingest_job_id`，DDL 的
// `num_nonnulls(run_id, ingest_job_id) <= 1` 允许二选一或都不填），`prompt_tokens`
// 写 0（用量未定，列 NOT NULL 且带 DEFAULT 0），`finished_at` 留 NULL（尚未终态），
// `created_at` 取句柄时钟的 Unix 毫秒。
// The pending shape leaves run_id NULL for the ingest path, fixes prompt_tokens at 0,
// keeps finished_at NULL and stamps created_at from the handle clock.
func (r *EmbeddingCallRepo) BeginEmbedding(ctx context.Context, jobID, model string, inputCount int) (string, error) {
	// 列有 `CHECK (input_count >= 1)`：负值与零在本层先拒，错误信息点名来源，
	// 而不是让驱动把约束名抛给调用方。
	// The column carries CHECK (input_count >= 1); reject here so the error names us.
	if inputCount < 1 {
		return "", fmt.Errorf("预写 embedding 审计行（ingest_job_id=%s）: input_count=%d 违反 CHECK (input_count >= 1)",
			jobID, inputCount)
	}

	// `RETURNING id`：pgx 的 stdlib 驱动不实现 LastInsertId，标识只能随 INSERT 取回。
	var id int64
	err := r.db.pool.QueryRowContext(ctx,
		`INSERT INTO embedding_calls
		   (run_id, ingest_job_id, model, dimensions, input_count, prompt_tokens,
		    status, error_kind, created_at, finished_at)
		 VALUES (NULL, $1, $2, $3, $4, 0, $5, NULL, $6, NULL)
		 RETURNING id`,
		nullIfEmpty(jobID), model, r.dimensions, inputCount,
		embeddingStatusPending, r.db.clock().UnixMilli()).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("预写 embedding 审计行（ingest_job_id=%s, model=%s）: %w", jobID, model, err)
	}
	return strconv.FormatInt(id, 10), nil
}

// FinishEmbedding 把该行更新为终态；摄取侧以独立短超时 + WithoutCancel 调用。
// FinishEmbedding writes the terminal state; callers detach cancellation first.
//
// 终态只写一次：WHERE 带上 `status = 'pending'`，影响行数为 0 即拒绝并点名
// call_id——覆盖已判定的终态会让审计"改口"，比报错更难查（与 llm_calls 同口径）。
// The terminal state is written once: the WHERE clause requires a pending row.
func (r *EmbeddingCallRepo) FinishEmbedding(ctx context.Context, callID string, promptTokens int64, callErr error) error {
	id, err := strconv.ParseInt(callID, 10, 64)
	if err != nil {
		return fmt.Errorf("回填 embedding 审计行终态（call_id=%q）: 标识不是预写返回的十进制行 id: %w", callID, err)
	}
	// 负用量没有语义（列是 bigint NOT NULL）；先拒而不是把脏值写进账。
	// A negative token count has no meaning; reject it instead of persisting it.
	if promptTokens < 0 {
		return fmt.Errorf("回填 embedding 审计行终态（id=%d）: prompt_tokens=%d 不得为负", id, promptTokens)
	}

	status, kind := embeddingOutcome(callErr)

	res, err := r.db.pool.ExecContext(ctx,
		`UPDATE embedding_calls
		    SET status = $1, prompt_tokens = $2, error_kind = $3, finished_at = $4
		  WHERE id = $5 AND status = $6`,
		status, promptTokens, nullIfEmpty(string(kind)), r.db.clock().UnixMilli(),
		id, embeddingStatusPending)
	if err != nil {
		return fmt.Errorf("回填 embedding 审计行终态（id=%d）: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认 embedding 审计行终态写入（id=%d）: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("回填 embedding 审计行终态（id=%d）: 该行不在 pending 态或不存在——终态只写一次", id)
	}
	return nil
}

// embeddingOutcome 把调用结果映射为 (status, error_kind)。判据只有一条：**是否
// 观察到提供方响应**。
//   - 无错误 → success；
//   - 错误携带提供方响应（`EmbeddingError.Status != 0`，含非 2xx 与响应解析失败）
//     → failed：调用已到达提供方且确定没有成功；
//   - 错误未携带响应（`Status == 0` 的传输层失败，或不是 `EmbeddingError` 的
//     本地错误）→ unknown：请求可能已在途，结局无法由调用方判定——**不得**
//     写成 failed 假装已知（迁移与核对都把 unknown 当"需要人工查证"的独立档位）。
//
// error_kind 记录**本地观察到的**失败类：取消 / 截止 → timeout，其余提供方侧
// 错误 → external_error；unknown 且无分类时留 NULL（status 本身已表达"未知"）。
//
// embeddingOutcome maps a call result to (status, error_kind) on one criterion:
// whether a provider response was observed. No response means unknown, never a
// fabricated failed.
func embeddingOutcome(callErr error) (status string, kind core.ErrorKind) {
	if callErr == nil {
		return embeddingStatusSuccess, ""
	}

	if errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
		// 取消 / 截止可能发生在发送前后任一时刻：结局未知，但中断原因是确定的。
		// Cancellation can hit before or after the send; the interrupt cause is known.
		return embeddingStatusUnknown, core.ErrorKindTimeout
	}

	var embErr *knowledge.EmbeddingError
	if errors.As(callErr, &embErr) {
		if embErr.Status == 0 {
			// 传输层失败（DNS / 连接 / TLS）：没有观察到响应。
			// Transport-level failure: no response was observed.
			return embeddingStatusUnknown, core.ErrorKindExternalError
		}
		// 响应已到达：非 2xx、响应不可解析、向量无法按序号归位都归这一档。
		// A response arrived; a non-2xx or unusable body is a definitive failure.
		return embeddingStatusFailed, core.ErrorKindExternalError
	}

	// 其余错误来自本仓客户端入口（如空输入不发出请求），没有可判定的提供方结局。
	// Anything else originates locally, so the provider outcome stays unknown.
	return embeddingStatusUnknown, core.ErrorKindUnknown
}

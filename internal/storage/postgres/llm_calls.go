package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 llm_calls 两阶段写的**唯一 SQL 入口**（审计 SQL 只允许出现在 internal/storage
// ——全局不变量 3）。列与状态字面量以 docs/contracts/schema.sql 为准，此处不另抄列清单。
// This file is the only SQL entry point for the two-phase write to llm_calls: pre-write a
// pending row before the call, then fill in the terminal state after it returns.

// maxErrorMessageBytes 是 error_message 列的上界（契约 §7.4：失败原文截断 4 KB）。
// maxErrorMessageBytes is the upper bound of the error_message column (contract §7.4).
const maxErrorMessageBytes = 4 * 1024

// LLMCallRepo 是 llm_calls 的唯一 SQL 入口（审计 SQL 只允许出现在 internal/storage——全局不变量 3）。
// LLMCallRepo is the only SQL entry point for llm_calls.
type LLMCallRepo struct{ db *DB }

// 编译期断言：LLMCallRepo 满足消费方（core）声明的审计仓储接口。
// Compile-time assertion that LLMCallRepo satisfies the consumer-declared interface.
var _ core.LLMCallStore = (*LLMCallRepo)(nil)

// 编译期断言：同一个实现也满足**只读**查询面（读写合成一个实现、两个消费面——与
// ToolInvocationRepo 的既有形态一致）。
// The same implementation also satisfies the read-only surface, mirroring the tool repo.
var _ core.LLMCallReader = (*LLMCallRepo)(nil)

// NewLLMCallRepo 绑定一个已打开的写模式库句柄。
// NewLLMCallRepo binds the repo to an opened writable database handle.
func NewLLMCallRepo(db *DB) *LLMCallRepo { return &LLMCallRepo{db: db} }

// CallsByRun 按轮号升序取该 Run 的模型调用行（只读路径，走只读池）。
// 排序带 `id` 做次级键：同一轮理论上只有一次调用，但真出现重复轮号时，行序必须**确定**——
// 读数行序在两次运行之间漂移，用量对账就没法复核。
// CallsByRun reads one run's call rows in round order, with the row id as a tiebreaker so
// the order can never drift between two identical runs.
func (r *LLMCallRepo) CallsByRun(ctx context.Context, runID string) ([]core.LLMCallView, error) {
	rows, err := r.db.pool.QueryContext(ctx,
		`SELECT round, status, prompt_tokens, completion_tokens, total_tokens
		   FROM llm_calls WHERE run_id = $1 ORDER BY round, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("读审计行（run_id=%s）: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var views []core.LLMCallView
	for rows.Next() {
		var v core.LLMCallView
		var status string
		if err := rows.Scan(&v.Round, &status, &v.PromptTokens, &v.CompletionTokens, &v.TotalTokens); err != nil {
			return nil, fmt.Errorf("扫描审计行（run_id=%s）: %w", runID, err)
		}
		v.Status = core.LLMCallStatus(status)
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历审计行（run_id=%s）: %w", runID, err)
	}
	return views, nil
}

// BeginCall 插入 status=pending 的审计行并返回行 id；失败 = 调用方**拒绝发出**该次调用。
// BeginCall inserts the pending audit row and returns its id. A failure here means the
// caller must refuse to make the call.
//
// 状态列由本函数**固定写 'pending'**（预写形态不带状态字段，调用方也就没有传错的可能）；
// duration_ms 写 0（耗时未定，不得留 NULL——列 NOT NULL）；可空列（session_id /
// config_hash）空值写 NULL，与"写了空串"区分开；created_at 存 Unix 毫秒。
// The status is written here as a literal, so no caller can get it wrong.
func (r *LLMCallRepo) BeginCall(ctx context.Context, call core.LLMCall) (int64, error) {
	return r.beginPendingCall(ctx, string(core.LLMCallPurposeChat), call)
}

// BeginCompactionCall 插入**摘要调用**的 pending 审计行（`purpose='compaction'`），
// 返回行 id；失败 = 摘要调用**拒绝发出**（C-25 的 fail-closed 口径与 Chat 完全一致）。
//
// 为什么单独一个入口而不是给 `core.LLMCall` 加 `Purpose` 字段：`core.LLMCallStore` 的
// 签名按 P-02 逐字保留，`, purpose` 因此只能由存储层按调用种类固定写——与 `status` 由
// `BeginCall` 固定写 `'pending'` 是同一条理由（调用方没有传错的可能）。
// `round` 传 0：摘要调用不属于任何对话轮，轮号列是 NOT NULL，0 是"非对话轮"的显式取值。
// A separate entry point keeps core.LLMCallStore's signature frozen (P-02); the purpose
// is a storage-layer literal, just like status, and round 0 marks a non-conversational call.
func (r *LLMCallRepo) BeginCompactionCall(ctx context.Context, call core.LLMCall) (int64, error) {
	return r.beginPendingCall(ctx, string(core.LLMCallPurposeCompaction), call)
}

// beginPendingCall 是两条预写入口的**唯一** INSERT（列清单只写一份，两处各写一份就是
// 两处漂移点）：状态恒 'pending'，purpose 由调用种类固定给出。
// beginPendingCall is the single INSERT behind both pre-write entry points.
func (r *LLMCallRepo) beginPendingCall(ctx context.Context, purpose string, call core.LLMCall) (int64, error) {
	createdAt := call.CreatedAt
	if createdAt.IsZero() {
		// 列是 NOT NULL 且含义为"这一时刻"；零值时间会落到 1677 年之前，比缺省当前时刻更误导。
		// The column is NOT NULL; a zero time would predate 1677 and mislead more than now().
		createdAt = time.Now()
	}

	// `RETURNING id`：pgx 的 stdlib 驱动不实现 `LastInsertId`（SQLite 迁移遗留），
	// 标识只能随 INSERT 一起取回。
	var id int64
	err := r.db.pool.QueryRowContext(ctx,
		`INSERT INTO llm_calls
		   (run_id, session_id, round, provider, model,
		    prompt_tokens, completion_tokens, total_tokens, duration_ms,
		    status, error_message, error_kind, config_hash, created_at, purpose)
		 VALUES ($1, $2, $3, $4, $5, 0, 0, 0, 0, $6, NULL, NULL, $7, $8, $9)
		 RETURNING id`,
		call.RunID, nullIfEmpty(call.SessionID), call.Round, call.Provider, call.Model,
		string(core.LLMCallPending), nullIfEmpty(call.ConfigHash), createdAt.UnixMilli(), purpose).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("预写审计行（provider=%s, purpose=%s）: %w", call.Provider, purpose, err)
	}
	return id, nil
}

// FinishCall 把该行更新为终态；调用方以独立短超时 + WithoutCancel 调用（契约 §4.2）。
// FinishCall writes the terminal state; callers detach cancellation and set their own
// short timeout (contract §4.2).
//
// 终态只写一次：WHERE 带上 status = 'pending'，影响行数为 0 即拒绝并点名 id——
// 覆盖已判定的终态会让审计"改口"，比报错更难查。
// The terminal state is written once: the WHERE clause requires a pending row, so a
// second write is refused rather than silently rewriting history.
func (r *LLMCallRepo) FinishCall(ctx context.Context, id int64, result core.LLMCallResult) error {
	res, err := r.db.pool.ExecContext(ctx,
		`UPDATE llm_calls
		    SET status = $1, prompt_tokens = $2, completion_tokens = $3, total_tokens = $4,
		        duration_ms = $5, error_message = $6, error_kind = $7
		  WHERE id = $8 AND status = $9`,
		string(result.Status), result.PromptTokens, result.CompletionTokens, result.TotalTokens,
		result.Duration.Milliseconds(), nullIfEmpty(truncateUTF8(result.ErrorMessage, maxErrorMessageBytes)),
		nullIfEmpty(string(result.ErrorKind)), id, string(core.LLMCallPending))
	if err != nil {
		return fmt.Errorf("回填审计行终态（id=%d）: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认审计行终态写入（id=%d）: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("回填审计行终态（id=%d）: 该行不在 pending 态或不存在——终态只写一次", id)
	}
	return nil
}

// truncateUTF8 把文本截断到 limit 字节且**不切断多字节字符**（契约 §7.4 的"UTF-8 安全"
// 判据）：从 limit 处向前回退到最近的 rune 起始字节，最多 3 步。
// truncateUTF8 cuts text to at most limit bytes without splitting a multi-byte rune.
//
// 不用"反复判整个前缀是否合法 UTF-8"的写法：原文里若混入过非法字节，那种写法会把文案
// 收敛到空串（诊断信息全丢）。这里只回退跨越边界的那一个字符。
// Backing off across the boundary keeps the head of the message intact even when the
// original contains invalid bytes.
func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !isRuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// isRuneStart 判定字节是否是 rune 的起始字节（非续字节 0b10xxxxxx）。
// isRuneStart reports whether b starts a rune rather than continuing one.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// nullIfEmpty 把空串映射为 SQL NULL——可空列的"没值"与"值是空串"是两回事。
// nullIfEmpty maps the empty string to SQL NULL: an absent value differs from an empty one.
func nullIfEmpty(s string) any {
	if s == "" {
		return sql.NullString{}
	}
	return s
}

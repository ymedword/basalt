package storage

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

// NewLLMCallRepo 绑定一个已打开的写模式库句柄。
// NewLLMCallRepo binds the repo to an opened writable database handle.
func NewLLMCallRepo(db *DB) *LLMCallRepo { return &LLMCallRepo{db: db} }

// BeginCall 插入 status=pending 的审计行并返回行 id；失败 = 调用方**拒绝发出**该次调用。
// BeginCall inserts the pending audit row and returns its id. A failure here means the
// caller must refuse to make the call.
//
// 状态列由本函数**固定写 'pending'**（预写形态不带状态字段，调用方也就没有传错的可能）；
// duration_ms 写 0（耗时未定，不得留 NULL——列 NOT NULL）；可空列（session_id /
// config_hash）空值写 NULL，与"写了空串"区分开；created_at 存 Unix 毫秒。
// The status is written here as a literal, so no caller can get it wrong.
func (r *LLMCallRepo) BeginCall(ctx context.Context, call core.LLMCall) (int64, error) {
	createdAt := call.CreatedAt
	if createdAt.IsZero() {
		// 列是 NOT NULL 且含义为"这一时刻"；零值时间会落到 1677 年之前，比缺省当前时刻更误导。
		// The column is NOT NULL; a zero time would predate 1677 and mislead more than now().
		createdAt = time.Now()
	}

	res, err := r.db.write.ExecContext(ctx,
		`INSERT INTO llm_calls
		   (run_id, session_id, round, provider, model,
		    prompt_tokens, completion_tokens, total_tokens, duration_ms,
		    status, error_message, error_kind, config_hash, created_at)
		 VALUES (?, ?, ?, ?, ?, 0, 0, 0, 0, ?, NULL, NULL, ?, ?)`,
		call.RunID, nullIfEmpty(call.SessionID), call.Round, call.Provider, call.Model,
		string(core.LLMCallPending), nullIfEmpty(call.ConfigHash), createdAt.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("预写审计行（provider=%s）: %w", call.Provider, err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取预写审计行 id: %w", err)
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
	res, err := r.db.write.ExecContext(ctx,
		`UPDATE llm_calls
		    SET status = ?, prompt_tokens = ?, completion_tokens = ?, total_tokens = ?,
		        duration_ms = ?, error_message = ?, error_kind = ?
		  WHERE id = ? AND status = ?`,
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

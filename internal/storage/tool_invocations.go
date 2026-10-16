package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 tool_invocations 两阶段写的**唯一 SQL 入口**（审计 SQL 只允许出现在
// internal/storage——全局不变量 3）。表 / 列 / 状态字面量以 docs/contracts/schema.sql
// 为准，此处不另抄列清单。三条纪律。① **预写态由 SQL 固定写** `pending`：预写形态
// 不带状态字段，调用方也就没有传错的可能（与 llm_calls 同一手法）。② **终态只写一次**：
// `WHERE id = ? AND status = 'pending'`，二次回填拒绝而不是让审计"改口"。③ 截断标记
// 写在 `result_json` **值内**，不新造列（列级口径 §3.4：无 `truncated`、无 `round`）。
// This file is the only SQL entry point for the two-phase write to tool_invocations:
// pending is written by SQL itself, the terminal state is written exactly once, and the
// truncation marker lives inside result_json — never in a column of its own.

// ToolInvocationRepo 是 tool_invocations 两阶段写的唯一 SQL 入口（审计 SQL 只允许出现在
// internal/storage——全局不变量 3）。
// ToolInvocationRepo is the only SQL entry point for tool_invocations.
type ToolInvocationRepo struct{ db *DB }

// 编译期断言：同一实现同时满足**写路径**（core.ToolInvocationStore）与**补齐路径的
// 只读面**（core.ToolInvocationReader）——接口定义在消费方 core，两边各自编译期判卷。
// One implementation, two consumer-declared interfaces: the write path and the read-only
// surface the synthesis path consumes.
var (
	_ core.ToolInvocationStore  = (*ToolInvocationRepo)(nil)
	_ core.ToolInvocationReader = (*ToolInvocationRepo)(nil)
)

// NewToolInvocationRepo 绑定一个已打开的写模式库句柄。
// NewToolInvocationRepo binds the repo to an opened writable database handle.
func NewToolInvocationRepo(db *DB) *ToolInvocationRepo { return &ToolInvocationRepo{db: db} }

// BeginInvocation 插入 status=pending 的审计行并返回行 id；失败 = 调用方**拒绝执行**
// 该次工具调用（fail-closed，契约 §7.2 第 1 步）。
// BeginInvocation inserts the pending audit row and returns its id. A failure here means
// the caller must refuse to execute the tool call.
//
// `input_json` 原样入库（不重编码）——模型给的 arguments 是 JSON 字符串，多一次编解码
// 就可能把键序、转义形态改掉，审计要的是"当时确实传了什么"。`duration_ms` 留 NULL：
// 该列可空，预写时耗时未定，NULL 与"耗时为零"因此可区分。`started_at` 存 Unix 毫秒。
// input_json is stored verbatim; duration_ms stays NULL so "not yet known" cannot be
// confused with "took zero".
func (r *ToolInvocationRepo) BeginInvocation(ctx context.Context, inv core.ToolInvocation) (int64, error) {
	startedAt := inv.CreatedAt
	if startedAt.IsZero() {
		// 列是 NOT NULL 且含义为"这一时刻"；零值时间会落到 1677 年之前，比缺省当前时刻更误导。
		// The column is NOT NULL; a zero time would predate 1677 and mislead more than now().
		startedAt = time.Now()
	}

	res, err := r.db.write.ExecContext(ctx,
		`INSERT INTO tool_invocations
		   (run_id, session_id, tool_call_id, tool_name, input_json, result_json,
		    status, error_message, error_kind, duration_ms, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, NULL, ?, NULL, NULL, NULL, ?, NULL)`,
		inv.RunID, nullIfEmpty(inv.SessionID), nullIfEmpty(inv.ToolCallID), inv.ToolName,
		nullIfEmpty(string(inv.InputJSON)), string(core.LLMCallPending), startedAt.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("预写工具审计行（tool=%s）: %w", inv.ToolName, err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取预写工具审计行 id: %w", err)
	}
	return id, nil
}

// FinishInvocation 把该行更新为终态（状态 / 失败类别 / 结果 / 失败原文 / 耗时 / 结束时刻）；
// 失败 = 调用方记 ERROR、行留 `pending`（**不得**伪造终态），下次启动由恢复扫描标 `unknown`。
// FinishInvocation writes the terminal state onto the same row. A failure leaves the row
// pending — recovery normalizes it later; the caller never fakes a terminal value.
//
// `finished_at = started_at + duration_ms`：冻结签名（§3.1）里没有墙钟位，而终态行不该留
// 一个永远为 NULL 的结束时刻。由两个**显式输入**（行内起始时刻 + 调用方给的耗时）推出来，
// 仓储因此不读时钟，同一输入落库读数可复现；与真值的偏差有界（预写与执行入口之间的毫秒
// 级间隙）。`error_message` 入库前截断 4 KB（UTF-8 安全，与 llm_calls 同一辅助函数）。
// finished_at is derived from the row's start plus the caller's duration so the repo never
// reads a clock and the stored value is reproducible from explicit inputs.
func (r *ToolInvocationRepo) FinishInvocation(ctx context.Context, id int64, result core.ToolInvocationResult) error {
	res, err := r.db.write.ExecContext(ctx,
		`UPDATE tool_invocations
		    SET status = ?, error_kind = ?, result_json = ?, error_message = ?,
		        duration_ms = ?, finished_at = started_at + ?
		  WHERE id = ? AND status = ?`,
		string(result.Status), nullIfEmpty(string(result.ErrorKind)),
		nullIfEmpty(string(result.ResultJSON)),
		nullIfEmpty(truncateUTF8(result.ErrorMessage, maxErrorMessageBytes)),
		result.Duration.Milliseconds(), result.Duration.Milliseconds(),
		id, string(core.LLMCallPending))
	if err != nil {
		return fmt.Errorf("回填工具审计行终态（id=%d）: %w", id, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认工具审计行终态写入（id=%d）: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("回填工具审计行终态（id=%d）: 该行不在 pending 态或不存在——终态只写一次", id)
	}
	return nil
}

// Lookup 按 (run_id, tool_call_id) 取审计行（core.ToolInvocationReader 的实现：补齐路径
// 专用只读面——Session 只读不写审计）。未命中返回 ok=false（无行 ≠ 错误）。
// Lookup fetches the audit row by (runID, toolCallID); a miss returns ok=false, not an error.
//
// 查询体在 `lookupToolInvocation`：崩溃恢复（`recovery.go`）要在**事务连接**上问同一个问题，
// 两处必须同解——判据一旦分叉，"这一调用有没有真实结果"就会有两个答案，而分叉的后果是
// 历史里的一条合成文本与审计行说的不是同一件事。
// The query body lives in lookupToolInvocation so the crash-recovery scan can ask the same
// question on its transaction connection; a forked criterion would give two answers.
func (r *ToolInvocationRepo) Lookup(ctx context.Context, runID, toolCallID string) (core.ToolInvocationView, bool, error) {
	return lookupToolInvocation(ctx, r.db.read, runID, toolCallID)
}

// lookupToolInvocation 是这条查询的**唯一一份**（仓储走读池、恢复走事务连接）。
// The single copy of the audit lookup query, shared by the repo and by crash recovery.
//
// 两个判据刻意写死在查询里。① **不按全会话 id 查**——跨 Run 的同 id 调用会互相误配
// （契约 §4.3.2 的"按位置判组"纪律在审计侧的对应面）。② 同一 Run 内理论上只有一行；
// 若真出现重复（模型重复给出同一 id），**优先取终态行**：合成路径要回答的问题是"这一调用
// 有没有真实结果"，只要存在终态行就不得降级成"结果未知"（契约 §4.4 五分支第一行）。
// The query deliberately scopes by run and prefers a terminal row over a pending one, so a
// duplicated id cannot demote a real result into "unknown".
func lookupToolInvocation(ctx context.Context, q queryer, runID, toolCallID string) (core.ToolInvocationView, bool, error) {
	var (
		status  string
		result  sql.NullString
		message sql.NullString
		kind    sql.NullString
	)
	err := q.QueryRowContext(ctx,
		`SELECT status, result_json, error_message, error_kind
		   FROM tool_invocations
		  WHERE run_id = ? AND tool_call_id = ?
		  ORDER BY (status IN ('success', 'failed')) DESC, id DESC
		  LIMIT 1`, runID, toolCallID).
		Scan(&status, &result, &message, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ToolInvocationView{}, false, nil
	}
	if err != nil {
		return core.ToolInvocationView{}, false,
			fmt.Errorf("查工具审计行（run_id=%s, tool_call_id=%s）: %w", runID, toolCallID, err)
	}

	return core.ToolInvocationView{
		Status:       core.LLMCallStatus(status),
		ResultJSON:   json.RawMessage(result.String),
		ErrorMessage: message.String,
		ErrorKind:    core.ErrorKind(kind.String),
	}, true, nil
}

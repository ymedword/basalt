package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 sessions / messages 的**唯一 SQL 入口**。表 / 列字面量以
// docs/contracts/schema.sql 为准，此处不另抄列清单。三条纪律。① **`seq` 由库定**：
// 会话内序号在 INSERT 语句里推算（`MAX(seq) + 1`），调用方给不出重复的号，同一条语句
// 也就没有"先查后写"的窗口。② **`run_id` 显式入列**：消息本身没有 Run 字段，崩溃恢复
// 却按 `messages.run_id` 定位半截组（core.MessageStore 的接口注释）——本列一旦漏写，
// 恢复就无从判定。③ **计数列与消息同事务**：`sessions.message_count` 是
// "列表页免 count(*)" 的冗余列（DDL 注释），它与消息行必须同生同死，否则 `session list`
// 显示条数与历史长度对不上。
// This file is the only SQL entry point for sessions/messages: seq is derived inside the
// INSERT, run_id is written explicitly because recovery depends on it, and the redundant
// message_count moves in the same transaction as the message row it counts.

// SessionStore 是 sessions / messages 的唯一 SQL 入口（core.MessageStore 的实现）。
// SessionStore is the only SQL entry point for sessions and messages.
type SessionStore struct{ db *DB }

// 编译期断言：SessionStore 满足消费方（core）声明的会话历史写透接口。
// Compile-time assertion that SessionStore satisfies the consumer-declared interface.
var _ core.MessageStore = (*SessionStore)(nil)

// NewSessionStore 绑定一个已打开的库句柄（写入走写池、读取走读池）。
// NewSessionStore binds the store to an opened database handle.
func NewSessionStore(db *DB) *SessionStore { return &SessionStore{db: db} }

// AppendMessage 追加一条消息并把 `sessions.message_count` 加一（同一短事务）。
// AppendMessage appends one message and bumps message_count in the same short transaction.
//
// **空会话标识即拒绝**：空 session_id 是"无状态调用"的语义（仅在内存、不落会话历史，
// 契约 §1.1），本层没有它可以写的行（`messages.session_id` 有外键）；静默 no-op 会让
// 调用方以为消息落了库——那正是"内存里有、库里没有"的历史分叉。
// An empty session id is refused rather than silently ignored: a silent no-op would let the
// caller believe the message was persisted.
func (s *SessionStore) AppendMessage(ctx context.Context, sessionID, runID string, m core.Message) error {
	if sessionID == "" {
		return errors.New("追加会话消息: 空会话标识——无状态会话只在内存，不落消息历史")
	}
	// 时间取本语句执行时刻：冻结签名（颗粒度文档 §3.1）里没有时间位，而 `created_at`
	// 是 NOT NULL 的"这一时刻"。与 `ToolInvocationRepo.BeginInvocation` 同一手法、同一理由。
	// The frozen signature carries no clock, so the row stamps the moment of the write —
	// the same treatment BeginInvocation gives its started_at column.
	return withWriteLock(ctx, s.db.write, func(ctx context.Context, conn *sql.Conn) error {
		return appendMessage(ctx, conn, sessionID, runID, m, time.Now())
	})
}

// Messages 按 `seq` 升序取回整个会话历史（空会话标识 → 空历史，无历史即是无状态会话的
// 正确答案，不是错误）。
// Messages returns the whole session history in ascending seq order; an empty session id
// yields an empty history.
func (s *SessionStore) Messages(ctx context.Context, sessionID string) ([]core.Message, error) {
	if sessionID == "" {
		return nil, nil
	}
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT content, role, tool_calls, tool_call_id, reasoning_content, provider_extra
		   FROM messages WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("读会话历史（session_id=%s）: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.Message
	for rows.Next() {
		var content, role, calls, callID, reasoning, extra sql.NullString
		if err := rows.Scan(&content, &role, &calls, &callID, &reasoning, &extra); err != nil {
			return nil, fmt.Errorf("读会话历史（session_id=%s）: %w", sessionID, err)
		}
		parsedCalls, err := parseToolCalls(calls.String)
		if err != nil {
			// 解不开**不猜**（与恢复扫描同一条判据）：这一列只由本仓写入，解不开就是真损坏。
			return nil, fmt.Errorf("读会话历史（session_id=%s）: %w", sessionID, err)
		}
		parsedExtra, err := decodeProviderExtra(extra.String)
		if err != nil {
			return nil, fmt.Errorf("读会话历史（session_id=%s）: %w", sessionID, err)
		}
		out = append(out, core.Message{
			Role:             role.String,
			Content:          content.String,
			ToolCalls:        parsedCalls,
			ToolCallID:       callID.String,
			ReasoningContent: reasoning.String,
			ProviderExtra:    parsedExtra,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读会话历史（session_id=%s）: %w", sessionID, err)
	}
	return out, nil
}

// GetSession 读一行会话（含 status / archived_at）；无行 → fs.ErrNotExist。
// GetSession reads one session row, mapping a miss to fs.ErrNotExist.
func (s *SessionStore) GetSession(ctx context.Context, id string) (core.SessionRecord, error) {
	var (
		rec                     core.SessionRecord
		createdAt, lastActiveAt int64
		archivedAt              sql.NullInt64
	)
	err := s.db.read.QueryRowContext(ctx,
		`SELECT session_id, profile_name, channel, user_id, status, message_count,
		        created_at, last_active_at, archived_at
		   FROM sessions WHERE session_id = ?`, id).
		Scan(&rec.ID, &rec.ProfileName, &rec.Channel, &rec.UserID, &rec.Status,
			&rec.MessageCount, &createdAt, &lastActiveAt, &archivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return core.SessionRecord{}, fs.ErrNotExist
	}
	if err != nil {
		return core.SessionRecord{}, fmt.Errorf("读会话行（session_id=%s）: %w", id, err)
	}
	rec.CreatedAt = time.UnixMilli(createdAt).UTC()
	rec.LastActiveAt = time.UnixMilli(lastActiveAt).UTC()
	if archivedAt.Valid {
		at := time.UnixMilli(archivedAt.Int64).UTC()
		rec.ArchivedAt = &at
	}
	return rec, nil
}

// GetOrCreateSession 建行或返回既有行；created 表示本次新建。
// 同 tuple 派生同一 id；已归档行原样返回（不复活）。
// GetOrCreateSession inserts the row or returns the existing one; the DO NOTHING
// conflict clause means an archived row stays archived with its original times.
func (s *SessionStore) GetOrCreateSession(ctx context.Context, rec core.SessionRecord) (core.SessionRecord, bool, error) {
	var created bool
	err := withWriteLock(ctx, s.db.write, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx,
			`INSERT INTO sessions (session_id, profile_name, channel, user_id,
			                      message_count, created_at, last_active_at)
			 VALUES (?, ?, ?, ?, 0, ?, ?)
			 ON CONFLICT(session_id) DO NOTHING`,
			rec.ID, rec.ProfileName, rec.Channel, rec.UserID,
			rec.CreatedAt.UnixMilli(), rec.LastActiveAt.UnixMilli())
		if err != nil {
			return fmt.Errorf("建会话行（session_id=%s）: %w", rec.ID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认会话行写入（session_id=%s）: %w", rec.ID, err)
		}
		created = affected > 0
		return nil
	})
	if err != nil {
		return core.SessionRecord{}, false, err
	}
	got, err := s.GetSession(ctx, rec.ID)
	if err != nil {
		return core.SessionRecord{}, false, err
	}
	return got, created, nil
}

// ArchiveSession 置 archived 并首次写 archived_at；重复调用幂等；未知 id → fs.ErrNotExist。
// ArchiveSession flips status to archived and keeps the first archived_at; SQLite counts
// WHERE-matched rows even when the value is unchanged, so a repeat stays idempotent.
func (s *SessionStore) ArchiveSession(ctx context.Context, id string, at time.Time) error {
	return withWriteLock(ctx, s.db.write, func(ctx context.Context, conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx,
			`UPDATE sessions SET status = 'archived', archived_at = COALESCE(archived_at, ?)
			 WHERE session_id = ?`, at.UnixMilli(), id)
		if err != nil {
			return fmt.Errorf("归档会话（session_id=%s）: %w", id, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认归档写入（session_id=%s）: %w", id, err)
		}
		if affected == 0 {
			return fs.ErrNotExist
		}
		return nil
	})
}

// MessagesTail 取最近 limit 条（按 seq 升序返回），含 created_at。
// MessagesTail reads the latest limit messages in ascending seq order, created_at
// included (the SQL runs DESC LIMIT and the slice is reversed in Go).
func (s *SessionStore) MessagesTail(ctx context.Context, id string, limit int) ([]core.Message, error) {
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT content, role, tool_calls, tool_call_id, reasoning_content,
		        provider_extra, created_at
		   FROM messages WHERE session_id = ? ORDER BY seq DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("读会话尾窗（session_id=%s）: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.Message
	for rows.Next() {
		var (
			content, role, calls, callID, reasoning, extra sql.NullString
			createdAt                                      int64
		)
		if err := rows.Scan(&content, &role, &calls, &callID, &reasoning, &extra, &createdAt); err != nil {
			return nil, fmt.Errorf("读会话尾窗（session_id=%s）: %w", id, err)
		}
		parsedCalls, err := parseToolCalls(calls.String)
		if err != nil {
			// 解不开**不猜**（与 Messages 同一条判据）：这一列只由本仓写入。
			return nil, fmt.Errorf("读会话尾窗（session_id=%s）: %w", id, err)
		}
		parsedExtra, err := decodeProviderExtra(extra.String)
		if err != nil {
			return nil, fmt.Errorf("读会话尾窗（session_id=%s）: %w", id, err)
		}
		out = append(out, core.Message{
			Role:             role.String,
			Content:          content.String,
			ToolCalls:        parsedCalls,
			ToolCallID:       callID.String,
			ReasoningContent: reasoning.String,
			ProviderExtra:    parsedExtra,
			CreatedAt:        time.UnixMilli(createdAt).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读会话尾窗（session_id=%s）: %w", id, err)
	}
	// SQL 取的是"最近 N 条"（DESC），对外按 seq 升序——反转在此处完成，调用方不排序。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// UpsertSession 建行或刷新会话行（不存在即建、存在只刷新**最后活动时间**）。
// UpsertSession inserts the session row or refreshes its last-active time.
//
// 已存在的行只动 `last_active_at`：会话标识由"渠道 + 用户 + Agent 画像"派生
// （§6 固定口径 1），其余列在首次建行时就已定格；尤其 **`message_count` 不得被
// 写回**——调用方（运行入口）递来的记录里那个字段并不知情，覆盖会把计数清零。
// `status` 也刻意不写：DDL 缺省即 `active`，而归档态不该被一次刷新悄悄复活。
// An existing row only updates last_active_at: overwriting message_count with the caller's
// (uninformed) value would reset the counter, and status must not be resurrected from
// archived by a mere refresh.
func (s *SessionStore) UpsertSession(ctx context.Context, rec core.SessionRecord) error {
	if rec.ID == "" {
		return errors.New("写会话行: 空会话标识——无状态会话没有可建的行")
	}
	_, err := s.db.write.ExecContext(ctx,
		`INSERT INTO sessions
		   (session_id, profile_name, channel, user_id, message_count, created_at, last_active_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET last_active_at = excluded.last_active_at`,
		rec.ID, rec.ProfileName, rec.Channel, rec.UserID, rec.MessageCount,
		rec.CreatedAt.UnixMilli(), rec.LastActiveAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("写会话行（session_id=%s）: %w", rec.ID, err)
	}
	return nil
}

// ListSessions 列出会话（`session list` 与 `GET /sessions` 的共用读面）。
// ListSessions lists the sessions for the command surface and the console list.
//
// 排序口径在本层固定：**最后活动时间倒序**（最新在前），并以 `session_id` 兜底次序——
// 时间相同时的次序也必须是确定的，否则两次同样的读取会给出两种渲染结果。
// 过滤只有 `agent` 等值与 `status`（`archived_at` 空 / 非空）两维；空过滤器保持
// 既有全量语义（命令面走这一支）。`Status` 按 `archived_at` 派生——契约
// SessionSummary 的口径，**不**读 `status` 列另造第二真值。
// Ordering is fixed here; the two filter dimensions are agent equality and
// archived_at emptiness, and Status is derived from archived_at (never re-read from
// the status column, which would create a second truth).
// 列表 SQL 的六种过滤组合都以**编译期常量**给出：运行期不拼 SQL，请求值只能走占位符；
// 排序与窗口是常量，调用方无法影响。常量拼接在编译期折叠，不产生运行期字符串拼接。
// The six filter combinations are compile-time constants: no SQL is assembled at runtime
// and every request value binds as a placeholder; ordering stays constant.
const (
	sessionListSelect = `SELECT session_id, profile_name, channel, user_id, message_count,
	                 created_at, last_active_at, archived_at
	            FROM sessions`
	sessionListTail = ` ORDER BY last_active_at DESC, session_id`

	sessionListAll           = sessionListSelect + sessionListTail
	sessionListByAgent       = sessionListSelect + ` WHERE profile_name = ?` + sessionListTail
	sessionListActive        = sessionListSelect + ` WHERE archived_at IS NULL` + sessionListTail
	sessionListArchived      = sessionListSelect + ` WHERE archived_at IS NOT NULL` + sessionListTail
	sessionListByAgentActive = sessionListSelect + ` WHERE profile_name = ? AND archived_at IS NULL` + sessionListTail
	sessionListByAgentArch   = sessionListSelect + ` WHERE profile_name = ? AND archived_at IS NOT NULL` + sessionListTail
)

func (s *SessionStore) ListSessions(ctx context.Context, filter core.SessionFilter) ([]core.SessionRecord, error) {
	var (
		query string
		args  []any
	)
	switch {
	case filter.Agent != "" && filter.Status == "active":
		query, args = sessionListByAgentActive, []any{filter.Agent}
	case filter.Agent != "" && filter.Status == "archived":
		query, args = sessionListByAgentArch, []any{filter.Agent}
	case filter.Agent != "" && filter.Status == "":
		query, args = sessionListByAgent, []any{filter.Agent}
	case filter.Agent == "" && filter.Status == "active":
		query = sessionListActive
	case filter.Agent == "" && filter.Status == "archived":
		query = sessionListArchived
	case filter.Agent == "" && filter.Status == "":
		// 不过滤（命令面与无参列表走这一支）。
		query = sessionListAll
	default:
		// 取值校验在 HTTP 层已做；这里再拒一次是失败关闭——非法状态绝不能退化成
		// "不过滤"而把归档行混进 active 结果。
		return nil, fmt.Errorf("列出会话: 非法状态过滤 %q（只接受 active / archived）", filter.Status)
	}

	rows, err := s.db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("列出会话: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.SessionRecord
	for rows.Next() {
		var (
			rec          core.SessionRecord
			createdAt    int64
			lastActiveAt int64
			archivedAt   sql.NullInt64
		)
		if err := rows.Scan(&rec.ID, &rec.ProfileName, &rec.Channel, &rec.UserID,
			&rec.MessageCount, &createdAt, &lastActiveAt, &archivedAt); err != nil {
			return nil, fmt.Errorf("列出会话: %w", err)
		}
		rec.CreatedAt = time.UnixMilli(createdAt).UTC()
		rec.LastActiveAt = time.UnixMilli(lastActiveAt).UTC()
		if archivedAt.Valid {
			at := time.UnixMilli(archivedAt.Int64).UTC()
			rec.ArchivedAt = &at
			rec.Status = "archived"
		} else {
			rec.Status = "active"
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出会话: %w", err)
	}
	return out, nil
}

// appendMessage 是 `messages` 表**唯一一份**插入语句：会话写入（本文件）与崩溃恢复的
// 合成补齐（`recovery.go`，走事务连接）共用它——两处各写一份同义 SQL 就是两处漂移点，
// 而 `seq` 分配与计数列这两条纪律必须处处同解。
// The single copy of the message INSERT, shared by the session write path and crash
// recovery's synthesis path (which passes its transaction connection).
//
// `seq` 在语句内推算：同一条语句读到的 `MAX(seq)` 与写入之间没有窗口，且同一事务里先前
// 插入的行对本语句可见——多组补齐天然接着编号。`ex` 收执行面（`*sql.DB` / `*sql.Conn`）。
// The seq subquery sees rows inserted earlier in the same transaction, so successive
// repairs continue the numbering without any per-session cache.
func appendMessage(ctx context.Context, ex sqlExecutor, sessionID, runID string, m core.Message, createdAt time.Time) error {
	calls, err := encodeToolCallsJSON(m.ToolCalls)
	if err != nil {
		return err
	}
	extra, err := encodeProviderExtra(m.ProviderExtra)
	if err != nil {
		return err
	}

	if _, err := ex.ExecContext(ctx,
		`INSERT INTO messages
		   (session_id, run_id, seq, role, content, tool_calls, tool_call_id, reasoning_content,
		    provider_extra, created_at)
		 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = ?),
		         ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, nullIfEmpty(runID), sessionID, m.Role, nullIfEmpty(m.Content),
		nullIfEmpty(calls), nullIfEmpty(m.ToolCallID), nullIfEmpty(m.ReasoningContent),
		nullIfEmpty(extra), createdAt.UnixMilli(),
	); err != nil {
		return fmt.Errorf("追加会话消息（session_id=%s, role=%s）: %w", sessionID, m.Role, err)
	}

	res, err := ex.ExecContext(ctx,
		`UPDATE sessions SET message_count = message_count + 1 WHERE session_id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("递增会话消息计数（session_id=%s）: %w", sessionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认会话消息计数写入（session_id=%s）: %w", sessionID, err)
	}
	if affected == 0 {
		// 消息行已因外键保证有会话行；真走到这里说明口径被绕过，如实报错而不是让计数静默落后。
		// The foreign key guarantees a session row exists; a miss means the invariant was
		// bypassed and the counter would silently lag behind.
		return fmt.Errorf("递增会话消息计数（session_id=%s）: 会话行不存在", sessionID)
	}
	return nil
}

// encodeToolCallsJSON 把工具调用编码成 `tool_calls` 列的 JSON 数组（DDL 注释里的
// `[{id,name,arguments}]` 形态）；无调用写 NULL——空数组与"没有工具调用"是两回事，
// 出站编码按"没有该键"处理（provider 侧 `len(calls) > 0` 的判据）。
// Tool calls are encoded into the documented JSON array shape; no calls means NULL.
func encodeToolCallsJSON(calls []core.ToolCall) (string, error) {
	if len(calls) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(calls)
	if err != nil {
		return "", fmt.Errorf("编码工具调用列: %w", err)
	}
	return string(raw), nil
}

// encodeProviderExtra 把供应商扩展字段编成 JSON 对象；空写 NULL。
// Provider extras are encoded into a JSON object; an empty map means NULL.
func encodeProviderExtra(extra map[string]any) (string, error) {
	if len(extra) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(extra)
	if err != nil {
		return "", fmt.Errorf("编码供应商扩展字段: %w", err)
	}
	return string(raw), nil
}

// decodeProviderExtra 解 `provider_extra` 列——**必须** `UseNumber`：默认解码把 JSON 数字
// 变成 float64，2^53 以上的整数会在两跳往返中悄悄改值（与 `provider.parseChatResponse`
// 同一条判据，两处都不得退回默认解码）。
// Decoding must use UseNumber: the default turns numbers into float64 and silently rounds
// large integers across the round trip.
func decodeProviderExtra(raw string) (map[string]any, error) {
	if raw == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var extra map[string]any
	if err := dec.Decode(&extra); err != nil {
		return nil, fmt.Errorf("解析供应商扩展字段: %w", err)
	}
	return extra, nil
}

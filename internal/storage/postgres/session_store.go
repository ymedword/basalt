package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
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
// 返回值为 `messages.id` 的十进制串：调用方（`core.Session`）用它回填 `Message.ID`，
// 装配记录与压缩条目据此指向**真实**行。事务提交失败时返回空串与错误——标识只有在
// 行确实落下之后才成立。
// The returned decimal string is the persisted messages.id; a failed transaction returns
// an empty id because the identity only exists once the row is really there.
func (s *SessionStore) AppendMessage(ctx context.Context, sessionID, runID string, m core.Message) (string, error) {
	if sessionID == "" {
		return "", errors.New("追加会话消息: 空会话标识——无状态会话只在内存，不落消息历史")
	}
	// 时间取本语句执行时刻：冻结签名（颗粒度文档 §3.1）里没有时间位，而 `created_at`
	// 是 NOT NULL 的"这一时刻"。与 `ToolInvocationRepo.BeginInvocation` 同一手法、同一理由。
	// The frozen signature carries no clock, so the row stamps the moment of the write —
	// the same treatment BeginInvocation gives its started_at column.
	var id string
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		id, err = appendMessage(ctx, tx, sessionID, runID, m, time.Now())
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// Messages 按 `seq` 升序取回整个会话历史（空会话标识 → 空历史，无历史即是无状态会话的
// 正确答案，不是错误）。每条都填 `Message.ID`（`messages.id` 的十进制串）。
// Messages returns the whole session history in ascending seq order with Message.ID
// filled; an empty session id yields an empty history.
func (s *SessionStore) Messages(ctx context.Context, sessionID string) ([]core.Message, error) {
	if sessionID == "" {
		return nil, nil
	}
	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT id, content, role, tool_calls, tool_call_id, reasoning_content, provider_extra
		   FROM messages WHERE session_id = $1 ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("读会话历史（session_id=%s）: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.Message
	for rows.Next() {
		var (
			id                                             int64
			content, role, calls, callID, reasoning, extra sql.NullString
		)
		if err := rows.Scan(&id, &content, &role, &calls, &callID, &reasoning, &extra); err != nil {
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
			ID:               strconv.FormatInt(id, 10),
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
		callerID                sql.NullString
	)
	err := s.db.pool.QueryRowContext(ctx,
		`SELECT session_id, profile_name, channel, user_id, status, message_count,
		        created_at, last_active_at, archived_at, caller_id
		   FROM sessions WHERE session_id = $1`, id).
		Scan(&rec.ID, &rec.ProfileName, &rec.Channel, &rec.UserID, &rec.Status,
			&rec.MessageCount, &createdAt, &lastActiveAt, &archivedAt, &callerID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.SessionRecord{}, fs.ErrNotExist
	}
	if err != nil {
		return core.SessionRecord{}, fmt.Errorf("读会话行（session_id=%s）: %w", id, err)
	}
	// `NULL` 归属（迁移遗留会话）读成空串：**不**匹配任何调用方，调用方持旧
	// session_id 一律按跨调用方 404 处理（设计 §3.4.2 的 NULL 语义）。
	rec.CallerID = callerID.String
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
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 归属只在**首次建行**时写：既有行（含迁移遗留的 NULL 归属）不得被一次
		// 刷新认领——那会把旧历史悄悄划给第一个撞上同一派生 id 的调用方。
		// Ownership is fixed on insert only; a refresh must not claim a legacy row.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (session_id, profile_name, channel, user_id,
			                      message_count, created_at, last_active_at, caller_id)
			 VALUES ($1, $2, $3, $4, 0, $5, $6, $7)
			 ON CONFLICT(session_id) DO NOTHING`,
			rec.ID, rec.ProfileName, rec.Channel, rec.UserID,
			rec.CreatedAt.UnixMilli(), rec.LastActiveAt.UnixMilli(), nullIfEmpty(rec.CallerID))
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
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE sessions SET status = 'archived', archived_at = COALESCE(archived_at, $1)
			 WHERE session_id = $2`, at.UnixMilli(), id)
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
	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT id, content, role, tool_calls, tool_call_id, reasoning_content,
		        provider_extra, created_at
		   FROM messages WHERE session_id = $1 ORDER BY seq DESC LIMIT $2::bigint`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("读会话尾窗（session_id=%s）: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.Message
	for rows.Next() {
		var (
			msgID                                          int64
			content, role, calls, callID, reasoning, extra sql.NullString
			createdAt                                      int64
		)
		if err := rows.Scan(&msgID, &content, &role, &calls, &callID, &reasoning, &extra, &createdAt); err != nil {
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
			ID:               strconv.FormatInt(msgID, 10),
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

// learningMessageColumns 是学习窗口两条查询共用的列清单（消息主键、Run、角色、正文、时刻）。
// learningMessageColumns is the shared projection for both learning window queries.
const learningMessageColumns = `m.id, COALESCE(m.run_id, ''), m.role, COALESCE(m.content, ''), m.created_at`

// MessagesForExtraction 取一次新抽取的有界窗口（US-22 §3.1.7）：只读、不追加。窗口取
// **触发 Run 自己在会话里的最后一条消息**为右界（`To`），游标为左界（不含），只收
// user / assistant 角色，并按「同一来源调用方 + 同一来源 Key」过滤——JOIN runs 同时
// 完成了来源判定与 `run_id IS NULL` 迁移遗留消息的排除（那类消息没有来源，不得作为证据）。
// 行数与字节双上限都保留**最近**的消息：从新到旧累计，任一上限先到即停，但至少保留最新
// 一条（一条就超过字节上限时仍保留它——否则会话会永远卡在同一位置不再前进）。
// `From` / `To` 是实际保留的消息主键边界，`Dropped` 是同一候选集里未保留的条数；前文
// （`id < From`）单独取，只进提示词、不作证据，同样受条数与字节上限约束。
// MessagesForExtraction reads the bounded, origin-scoped window anchored at the triggering
// run's last message and keeps the newest messages under both caps.
func (s *SessionStore) MessagesForExtraction(ctx context.Context, sessionID, runID string,
	afterMessageID int64, priorLimit, newLimit, priorBytes, newBytes int,
) (core.LearningWindow, error) {
	if sessionID == "" {
		return core.LearningWindow{}, nil
	}
	var (
		callerID, keyID string
		lastMessageID   int64
	)
	err := s.db.pool.QueryRowContext(ctx,
		`SELECT r.caller_id, COALESCE(r.key_id, ''),
		        COALESCE((SELECT MAX(m.id) FROM messages m
		                   WHERE m.session_id = $2 AND m.run_id = r.run_id), 0)
		   FROM runs r WHERE r.run_id = $1`, runID, sessionID).
		Scan(&callerID, &keyID, &lastMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.LearningWindow{}, fmt.Errorf("学习窗口: 运行记录不存在（run_id=%s）", runID)
	}
	if err != nil {
		return core.LearningWindow{}, fmt.Errorf("学习窗口: 读取运行来源（run_id=%s）: %w", runID, err)
	}
	if lastMessageID <= 0 || lastMessageID <= afterMessageID {
		return core.LearningWindow{}, nil
	}

	// `count(*) OVER ()` 在 LIMIT 之前计数：它给出窗口候选集的真实条数，Dropped 才能如实
	// 记录"窗口外还有多少条"。排序与上限都是常量，调用方影响不到。
	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT `+learningMessageColumns+`, count(*) OVER ()
		   FROM messages m
		   JOIN runs r ON r.run_id = m.run_id
		  WHERE m.session_id = $1 AND m.id > $2 AND m.id <= $3
		    AND m.role IN ('user', 'assistant')
		    AND r.caller_id = $4 AND COALESCE(r.key_id, '') = $5
		  ORDER BY m.id DESC
		  LIMIT $6`,
		sessionID, afterMessageID, lastMessageID, callerID, keyID, limitArg(newLimit))
	if err != nil {
		return core.LearningWindow{}, fmt.Errorf("学习窗口: 读取抽取窗口（session_id=%s）: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var (
		window core.LearningWindow
		total  int
		bytes  int
	)
	for rows.Next() {
		var (
			msg    core.LearningMessage
			runStr string
			at     int64
		)
		if err := rows.Scan(&msg.MessageID, &runStr, &msg.Role, &msg.Content, &at, &total); err != nil {
			return core.LearningWindow{}, fmt.Errorf("学习窗口: 扫描抽取窗口（session_id=%s）: %w", sessionID, err)
		}
		msg.RunID = runStr
		msg.CreatedAt = time.UnixMilli(at).UTC()
		if newBytes > 0 && len(window.New) > 0 && bytes+len(msg.Content) > newBytes {
			break
		}
		window.New = append(window.New, msg)
		bytes += len(msg.Content)
	}
	if err := rows.Err(); err != nil {
		return core.LearningWindow{}, fmt.Errorf("学习窗口: 遍历抽取窗口（session_id=%s）: %w", sessionID, err)
	}
	if len(window.New) == 0 {
		return core.LearningWindow{}, nil
	}
	window.Dropped = total - len(window.New)
	// DESC 读入、反转成主键升序返回：调用方拿到的窗口顺序是确定的，不用自己排序。
	window.ToMessageID = window.New[0].MessageID
	window.FromMessageID = window.New[len(window.New)-1].MessageID
	for i, j := 0, len(window.New)-1; i < j; i, j = i+1, j-1 {
		window.New[i], window.New[j] = window.New[j], window.New[i]
	}

	prior, err := s.readLearningPrior(ctx, sessionID, callerID, keyID, window.FromMessageID, priorLimit, priorBytes)
	if err != nil {
		return core.LearningWindow{}, err
	}
	window.Prior = prior
	return window, nil
}

// MessagesForFrozenWindow 按冻结的 `from` / `to` 与来源**重读**同一批消息（重试路径专用）：
// 不重新计算边界、不做新窗口裁剪，也不改条数——调用方拿重读条数与 `window_messages`
// 对比，缺失即按 `window_gone` 跳过并推进游标（US-22 §2.3.2，C-33）。前文仍单独取；
// 字节上限只影响前文，不重写 `From` / `To`。
// MessagesForFrozenWindow re-reads the frozen window verbatim; the caller compares the
// returned count against the recorded one to detect a pruned window.
func (s *SessionStore) MessagesForFrozenWindow(ctx context.Context, sessionID, originCallerID,
	originKeyID string, fromMessageID, toMessageID int64, priorLimit, priorBytes int,
) (core.LearningWindow, error) {
	if sessionID == "" || fromMessageID <= 0 || toMessageID < fromMessageID {
		return core.LearningWindow{}, nil
	}
	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT `+learningMessageColumns+`
		   FROM messages m
		   JOIN runs r ON r.run_id = m.run_id
		  WHERE m.session_id = $1 AND m.id >= $2 AND m.id <= $3
		    AND m.role IN ('user', 'assistant')
		    AND r.caller_id = $4 AND COALESCE(r.key_id, '') = $5
		  ORDER BY m.id DESC`,
		sessionID, fromMessageID, toMessageID, originCallerID, originKeyID)
	if err != nil {
		return core.LearningWindow{}, fmt.Errorf("学习窗口: 重读冻结窗口（session_id=%s）: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var window core.LearningWindow
	for rows.Next() {
		var (
			msg    core.LearningMessage
			runStr string
			at     int64
		)
		if err := rows.Scan(&msg.MessageID, &runStr, &msg.Role, &msg.Content, &at); err != nil {
			return core.LearningWindow{}, fmt.Errorf("学习窗口: 扫描冻结窗口（session_id=%s）: %w", sessionID, err)
		}
		msg.RunID = runStr
		msg.CreatedAt = time.UnixMilli(at).UTC()
		window.New = append(window.New, msg)
	}
	if err := rows.Err(); err != nil {
		return core.LearningWindow{}, fmt.Errorf("学习窗口: 遍历冻结窗口（session_id=%s）: %w", sessionID, err)
	}
	for i, j := 0, len(window.New)-1; i < j; i, j = i+1, j-1 {
		window.New[i], window.New[j] = window.New[j], window.New[i]
	}
	// 边界原样返回（不是按重读结果重算）：冻结窗口就是"当初记下的这一段"。
	window.FromMessageID = fromMessageID
	window.ToMessageID = toMessageID

	prior, err := s.readLearningPrior(ctx, sessionID, originCallerID, originKeyID, fromMessageID, priorLimit, priorBytes)
	if err != nil {
		return core.LearningWindow{}, err
	}
	window.Prior = prior
	return window, nil
}

// readLearningPrior 取窗口之前（`m.id < beforeMessageID`）的同来源前文：只进提示词、
// 不作证据，保留最近 priorLimit 条 / priorBytes 字节（至少一条）。来源与角色过滤与
// 窗口查询同解；参数非正即视为"不要前文"。
// readLearningPrior reads the same-origin context before the window, capped and never
// used as evidence.
func (s *SessionStore) readLearningPrior(ctx context.Context, sessionID, callerID, keyID string,
	beforeMessageID int64, priorLimit, priorBytes int,
) ([]core.LearningMessage, error) {
	if priorLimit <= 0 || beforeMessageID <= 0 {
		return nil, nil
	}
	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT `+learningMessageColumns+`
		   FROM messages m
		   JOIN runs r ON r.run_id = m.run_id
		  WHERE m.session_id = $1 AND m.id < $2
		    AND m.role IN ('user', 'assistant')
		    AND r.caller_id = $3 AND COALESCE(r.key_id, '') = $4
		  ORDER BY m.id DESC
		  LIMIT $5`,
		sessionID, beforeMessageID, callerID, keyID, priorLimit)
	if err != nil {
		return nil, fmt.Errorf("学习窗口: 读取窗口前文（session_id=%s）: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out   []core.LearningMessage
		bytes int
	)
	for rows.Next() {
		var (
			msg    core.LearningMessage
			runStr string
			at     int64
		)
		if err := rows.Scan(&msg.MessageID, &runStr, &msg.Role, &msg.Content, &at); err != nil {
			return nil, fmt.Errorf("学习窗口: 扫描窗口前文（session_id=%s）: %w", sessionID, err)
		}
		msg.RunID = runStr
		msg.CreatedAt = time.UnixMilli(at).UTC()
		if priorBytes > 0 && len(out) > 0 && bytes+len(msg.Content) > priorBytes {
			break
		}
		out = append(out, msg)
		bytes += len(msg.Content)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("学习窗口: 遍历窗口前文（session_id=%s）: %w", sessionID, err)
	}
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
	// 归属同在**首次建行**时定格：DO UPDATE 只刷新最后活动时间，不认领既有行
	// （NULL 归属的迁移遗留会话不得被任意调用方接管）。
	// Ownership is written on insert only; the conflict branch refreshes nothing else.
	_, err := s.db.pool.ExecContext(ctx,
		`INSERT INTO sessions
		   (session_id, profile_name, channel, user_id, message_count, created_at, last_active_at, caller_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT(session_id) DO UPDATE SET last_active_at = excluded.last_active_at`,
		rec.ID, rec.ProfileName, rec.Channel, rec.UserID, rec.MessageCount,
		rec.CreatedAt.UnixMilli(), rec.LastActiveAt.UnixMilli(), nullIfEmpty(rec.CallerID))
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
// 列表 SQL 的十二种过滤组合都以**编译期常量**给出：运行期不拼 SQL，请求值只能走
// 占位符；排序与窗口是常量，调用方无法影响。常量拼接在编译期折叠，不产生运行期
// 字符串拼接。组合数从 6 涨到 12 是 T056 引入归属过滤（`caller_id`）后的必然结果：
// 归属**优先占位符 `$1`**，Agent 顺延 `$2`——两种顺序混用会让同一份 SQL 常量在不同
// 调用下指向不同参数。
// The twelve filter combinations are compile-time constants; the caller filter takes
// $1 and the agent filter $2 so no constant ever binds against a different argument.
const (
	sessionListSelect = `SELECT session_id, profile_name, channel, user_id, message_count,
	                 created_at, last_active_at, archived_at, caller_id
	            FROM sessions`
	sessionListTail = ` ORDER BY last_active_at DESC, session_id`

	sessionListAll           = sessionListSelect + sessionListTail
	sessionListByAgent       = sessionListSelect + ` WHERE profile_name = $1` + sessionListTail
	sessionListActive        = sessionListSelect + ` WHERE archived_at IS NULL` + sessionListTail
	sessionListArchived      = sessionListSelect + ` WHERE archived_at IS NOT NULL` + sessionListTail
	sessionListByAgentActive = sessionListSelect + ` WHERE profile_name = $1 AND archived_at IS NULL` + sessionListTail
	sessionListByAgentArch   = sessionListSelect + ` WHERE profile_name = $1 AND archived_at IS NOT NULL` + sessionListTail

	// 归属过滤三个组合（无 Agent）。`caller_id = $1` 天然排除 NULL——迁移遗留的
	// 空归属行因此只对管理员可见，不会因"调用方过滤"而漏给陌生人。
	// The caller filter never matches NULL owner rows, so legacy rows stay admin-only.
	sessionListByCaller         = sessionListSelect + ` WHERE caller_id = $1` + sessionListTail
	sessionListByCallerActive   = sessionListSelect + ` WHERE caller_id = $1 AND archived_at IS NULL` + sessionListTail
	sessionListByCallerArchived = sessionListSelect + ` WHERE caller_id = $1 AND archived_at IS NOT NULL` + sessionListTail

	// 归属 + Agent 三个组合：归属恒为 `$1`，Agent 为 `$2`。
	sessionListByCallerAgentPrefix = sessionListSelect + ` WHERE caller_id = $1 AND profile_name = $2`
	sessionListByCallerAgent       = sessionListByCallerAgentPrefix + sessionListTail
	sessionListByCallerAgentActive = sessionListByCallerAgentPrefix + ` AND archived_at IS NULL` + sessionListTail
	sessionListByCallerAgentArch   = sessionListByCallerAgentPrefix + ` AND archived_at IS NOT NULL` + sessionListTail
)

func (s *SessionStore) ListSessions(ctx context.Context, filter core.SessionFilter) ([]core.SessionRecord, error) {
	var (
		query string
		args  []any
	)
	switch {
	case filter.CallerID != "" && filter.Agent != "" && filter.Status == "active":
		query, args = sessionListByCallerAgentActive, []any{filter.CallerID, filter.Agent}
	case filter.CallerID != "" && filter.Agent != "" && filter.Status == "archived":
		query, args = sessionListByCallerAgentArch, []any{filter.CallerID, filter.Agent}
	case filter.CallerID != "" && filter.Agent != "" && filter.Status == "":
		query, args = sessionListByCallerAgent, []any{filter.CallerID, filter.Agent}
	case filter.CallerID != "" && filter.Agent == "" && filter.Status == "active":
		query, args = sessionListByCallerActive, []any{filter.CallerID}
	case filter.CallerID != "" && filter.Agent == "" && filter.Status == "archived":
		query, args = sessionListByCallerArchived, []any{filter.CallerID}
	case filter.CallerID != "" && filter.Agent == "" && filter.Status == "":
		query, args = sessionListByCaller, []any{filter.CallerID}
	case filter.CallerID == "" && filter.Agent != "" && filter.Status == "active":
		query, args = sessionListByAgentActive, []any{filter.Agent}
	case filter.CallerID == "" && filter.Agent != "" && filter.Status == "archived":
		query, args = sessionListByAgentArch, []any{filter.Agent}
	case filter.CallerID == "" && filter.Agent != "" && filter.Status == "":
		query, args = sessionListByAgent, []any{filter.Agent}
	case filter.CallerID == "" && filter.Agent == "" && filter.Status == "active":
		query = sessionListActive
	case filter.CallerID == "" && filter.Agent == "" && filter.Status == "archived":
		query = sessionListArchived
	case filter.CallerID == "" && filter.Agent == "" && filter.Status == "":
		// 不过滤（命令面与无参列表走这一支）。
		query = sessionListAll
	default:
		// 取值校验在 HTTP 层已做；这里再拒一次是失败关闭——非法状态绝不能退化成
		// "不过滤"而把归档行混进 active 结果。
		return nil, fmt.Errorf("列出会话: 非法状态过滤 %q（只接受 active / archived）", filter.Status)
	}

	rows, err := s.db.pool.QueryContext(ctx, query, args...)
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
			callerID     sql.NullString
		)
		if err := rows.Scan(&rec.ID, &rec.ProfileName, &rec.Channel, &rec.UserID,
			&rec.MessageCount, &createdAt, &lastActiveAt, &archivedAt, &callerID); err != nil {
			return nil, fmt.Errorf("列出会话: %w", err)
		}
		rec.CallerID = callerID.String
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
func appendMessage(ctx context.Context, ex sqlExecutor, sessionID, runID string, m core.Message, createdAt time.Time) (string, error) {
	// 正文先过可存储性清洗（与 `core.Session.AppendMessage` 同一份判据）：本函数是
	// `messages` 表的唯一插入点，崩溃恢复在事务内直写消息、不经过会话类型——恢复从审计行
	// 还原的外部结果同样可能带 NUL（2026-10-31 走查实况：启动恢复以 `SQLSTATE 22021`
	// 失败，重路径装配失败、服务起不来）。清洗做在插入点 = 任何调用方都落不了坏字节。
	// The wash at the single INSERT point is structural: recovery bypasses the session type.
	core.SanitizeMessage(&m, func(field string, replaced int) {
		slog.Default().Warn("消息正文含无法承载的字节，已替换为 U+FFFD",
			"session_id", sessionID, "run_id", runID, "role", m.Role,
			"tool_call_id", m.ToolCallID, "field", field, "replaced_bytes", replaced)
	})

	calls, err := encodeToolCallsJSON(m.ToolCalls)
	if err != nil {
		return "", err
	}
	extra, err := encodeProviderExtra(m.ProviderExtra)
	if err != nil {
		return "", err
	}

	// 新行的 `messages.id` 是这条消息的**身份**：装配记录与压缩条目都指向它。
	// `RETURNING id` 让标识与 INSERT 同一条语句取回（pgx stdlib 不支持 LastInsertId）；
	// 拿不到就如实失败——没有标识的历史行无法被任何记录引用。
	// The new row's messages.id is the message's identity; RETURNING id reads it back in
	// the same statement, and a failure is an error rather than an empty success.
	var id int64
	err = ex.QueryRowContext(ctx,
		`INSERT INTO messages
		   (session_id, run_id, seq, role, content, tool_calls, tool_call_id, reasoning_content,
		    provider_extra, created_at)
		 VALUES ($1, $2, (SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = $3),
		         $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id`,
		sessionID, nullIfEmpty(runID), sessionID, m.Role, nullIfEmpty(m.Content),
		nullIfEmpty(calls), nullIfEmpty(m.ToolCallID), nullIfEmpty(m.ReasoningContent),
		nullIfEmpty(extra), createdAt.UnixMilli(),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("追加会话消息（session_id=%s, role=%s）: %w", sessionID, m.Role, err)
	}

	// 来源引用与消息**同一事务**落库（设计 §3.4.1 / 原则 II：追加路径仍只有这一条）。
	// `kind` 是持久化列而不是可猜的默认值：空 kind 直接失败，**不得**静默记成 knowledge；
	// 同一消息内的重复引用按唯一键去重（集合语义，不丢信息也不失败）。
	// Evidence refs commit with the message; an empty kind fails closed instead of being
	// guessed, and duplicate refs of one message collapse on the unique key.
	for _, ref := range m.EvidenceRefs {
		if ref.Kind == "" || !ref.Valid() {
			return "", fmt.Errorf("追加会话消息（session_id=%s, role=%s）: 来源引用不完整（kind=%q, id=%q, version=%q）",
				sessionID, m.Role, ref.Kind, ref.ID, ref.VersionID)
		}
		if _, err := ex.ExecContext(ctx,
			`INSERT INTO message_evidence_refs
			   (message_id, kind, ref_id, version_id, content_hash, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (message_id, kind, ref_id, version_id) DO NOTHING`,
			id, ref.Kind, ref.ID, ref.VersionID, ref.ContentHash, createdAt.UnixMilli()); err != nil {
			return "", fmt.Errorf("写消息来源引用（session_id=%s, kind=%s）: %w", sessionID, ref.Kind, err)
		}
	}

	res, err := ex.ExecContext(ctx,
		`UPDATE sessions SET message_count = message_count + 1 WHERE session_id = $1`, sessionID)
	if err != nil {
		return "", fmt.Errorf("递增会话消息计数（session_id=%s）: %w", sessionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("确认会话消息计数写入（session_id=%s）: %w", sessionID, err)
	}
	if affected == 0 {
		// 消息行已因外键保证有会话行；真走到这里说明口径被绕过，如实报错而不是让计数静默落后。
		// The foreign key guarantees a session row exists; a miss means the invariant was
		// bypassed and the counter would silently lag behind.
		return "", fmt.Errorf("递增会话消息计数（session_id=%s）: 会话行不存在", sessionID)
	}
	return strconv.FormatInt(id, 10), nil
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

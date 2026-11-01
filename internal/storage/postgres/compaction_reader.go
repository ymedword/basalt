package postgres

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `compaction.MessageReader` 的 PostgreSQL 实现（设计 §3.1.11 / T090）。
//
// 三条纪律：
//  1. **只按会话读**：没有 `ListByRun`——无状态 Run 的消息从不落库，按 Run 查库只能
//     得到空集（引擎对无状态范围一律走 `CompactRequest.History`）；
//  2. **只按 `seq` 排序**：`messages` 的唯一索引就是 `(session_id, seq)`，按
//     `created_at` 或主键排序会让同毫秒消息乱序、切点跨工具组；
//  3. **来源引用与消息一次读回**：引擎的过滤与摘要都按 `SourceRefs` 判定，二次查询
//     会把"读到的行"与"读到的引用"拉开成两个时刻。
//
// This file is the only SQL entry point for the compaction message reader: session scope
// only, ordered by seq, with evidence refs fetched together with the rows.

// 编译期断言：存储层满足压缩引擎声明的只读消息端口。
// Compile-time assertion that the store satisfies the reader port.
var _ compaction.MessageReader = (*CompactionStore)(nil)

// compactionSessionMessages 是会话范围的分页读：`seq > afterSeq` 升序取 limit 条。
// 分页游标只用 `seq`（唯一索引的第二列），`afterSeq` 传 0 表示从头读。
// The page query pages by seq alone; afterSeq = 0 reads from the beginning.
const compactionSessionMessages = `
	SELECT id, COALESCE(run_id, ''), seq, role,
	       COALESCE(content, ''), COALESCE(tool_calls, ''), COALESCE(tool_call_id, ''),
	       COALESCE(reasoning_content, '')
	  FROM messages
	 WHERE session_id = $1 AND seq > $2::bigint
	 ORDER BY seq
	 LIMIT $3::bigint`

// ListBySession 按 `(session_id, seq)` 升序分页读取会话消息，并把
// `message_evidence_refs` 一次填进 `SourceRefs`。
//
// 空会话标识返回空集（与 `SessionStore.Messages` 同口径：空会话即"没有历史"，不是错误）；
// `limit <= 0` 是调用方违约，直接报错而不是静默返回空集——把违约读成"没有消息"会让
// 引擎以为历史已读完。
// ListBySession pages one session in seq order and fills SourceRefs from the evidence
// table; an empty session id is an empty history, and a non-positive limit is an error.
func (s *CompactionStore) ListBySession(ctx context.Context, sessionID string, afterSeq int64, limit int) ([]compaction.StoredMessage, error) {
	if s == nil || s.db == nil || s.db.pool == nil {
		return nil, fmt.Errorf("读压缩会话消息（session_id=%s）: 数据库句柄不可用", sessionID)
	}
	if sessionID == "" {
		return nil, nil
	}
	if limit <= 0 {
		return nil, fmt.Errorf("读压缩会话消息（session_id=%s）: limit 必须为正（实际 %d）", sessionID, limit)
	}

	rows, err := s.db.pool.QueryContext(ctx, compactionSessionMessages, sessionID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("读压缩会话消息（session_id=%s）: %w", sessionID, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]compaction.StoredMessage, 0, limit)
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var (
			id                             int64
			runID, role                    string
			seq                            int64
			content, calls, callID, reason string
		)
		if err := rows.Scan(&id, &runID, &seq, &role, &content, &calls, &callID, &reason); err != nil {
			return nil, fmt.Errorf("读压缩会话消息（session_id=%s）: %w", sessionID, err)
		}
		parsedCalls, err := parseToolCalls(calls)
		if err != nil {
			// 解不开**不猜**（与 `SessionStore.Messages` 同一条判据）：这一列只由本仓写入。
			return nil, fmt.Errorf("读压缩会话消息（session_id=%s）: %w", sessionID, err)
		}
		out = append(out, compaction.StoredMessage{
			MessageID:        strconv.FormatInt(id, 10),
			Seq:              seq,
			RunID:            runID,
			Role:             role,
			Content:          content,
			ToolCallID:       callID,
			ToolCalls:        parsedCalls,
			ReasoningContent: reason,
			// `Truncated` 恒 false：落库的就是写入时的正文，存储层没有第二个截断判定面
			// （`history_store.go` 同一口径）。
			// Truncated stays false: storage holds whatever was written.
			Truncated: false,
		})
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读压缩会话消息（session_id=%s）: %w", sessionID, err)
	}

	refsByID, err := readMessageEvidenceRefs(ctx, s.db.pool, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		id, _ := parseMessagePrimaryKey(out[i].MessageID)
		out[i].SourceRefs = refsByID[id]
	}
	return out, nil
}

// readMessageEvidenceRefs 一次取回这批消息的全部来源引用（`message_evidence_refs`）。
//
// 只按消息主键取，不再叠加归属条件：能走到这里的消息已经过调用方的范围过滤，引用行
// 与消息同生同死。精读与压缩**共用同一条 SQL**——两处各写一份就是两处漂移点。
// readMessageEvidenceRefs loads the persisted refs for the given message ids; the exact
// read path is shared with HistoryStore so the two can never drift.
func readMessageEvidenceRefs(ctx context.Context, q Queryer, ids []int64) (map[int64][]core.EvidenceRef, error) {
	if len(ids) == 0 {
		return map[int64][]core.EvidenceRef{}, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT message_id, kind, ref_id, version_id, content_hash
		  FROM message_evidence_refs
		 WHERE message_id = ANY($1::bigint[])
		 ORDER BY message_id, kind, ref_id, version_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("读消息来源引用: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64][]core.EvidenceRef, len(ids))
	for rows.Next() {
		var (
			messageID   int64
			kind        string
			refID       string
			versionID   string
			contentHash string
		)
		if err := rows.Scan(&messageID, &kind, &refID, &versionID, &contentHash); err != nil {
			return nil, fmt.Errorf("读消息来源引用: %w", err)
		}
		out[messageID] = append(out[messageID], core.EvidenceRef{
			Kind:        kind,
			ID:          refID,
			VersionID:   versionID,
			ContentHash: contentHash,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读消息来源引用: %w", err)
	}
	return out, nil
}

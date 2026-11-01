package postgres

// 本文件是历史回查端口（`history.MessageQueryStore`）的 PostgreSQL 实现（US-16 §3.1.10 /
// T139）。三条纪律：
//  1. **归属过滤在 SQL 内**：判据是 `sessions.caller_id`——调用方恒为自己、管理员按请求
//     收窄、管理员不限时连迁移遗留的空归属行一起读。取回后在 Go 里筛会让分页失真：
//     `LIMIT` 先截断再筛，页永远补不满。
//  2. **检索只回指针与片段**：`snippet` 是围绕首个字面命中的 200 字符窗口，正文只经
//     `ReadMessages` 取——命中不能直接当引用。
//  3. **精确读取一次取回引用**：`message_evidence_refs` 与消息一并读回，上层不再二次查询；
//     引用**原样**透传，本层不解析成"最新版本"。
//
// 一处**冻结契约没有载体**的语义，如实记录：`ErrDeleted`（410）要求区分"被保留策略删除"
// 与"从未存在"，而 `messages` 表既没有软删列、也没有墓碑表（`docs/contracts/schema.pg.sql`），
// `message_evidence_refs` 的外键也不带 `ON DELETE` 级联。本实现因此对两种情形统一返回
// `ErrNotFound`；410 分支由上层假端口用例覆盖（`internal/web/handlers_history_test.go`），
// 真库侧无从判定。补墓碑属**新增表**的契约变更，留待人工 review ② 裁决，不在本任务内
// 自造。
//
// This file implements the history query port on PostgreSQL: ownership is filtered inside
// SQL, hits carry a bounded snippet rather than content, and exact reads pull the evidence
// refs in the same round. The frozen schema carries no soft-delete marker, so this store
// reports ErrNotFound for every missing row and never invents a 410 verdict.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/history"
)

// HistoryStore 是历史回查的 SQL 实现（消费方端口声明在 `internal/history`）。
// HistoryStore is the SQL implementation of the consumer-declared history port.
type HistoryStore struct{ db *DB }

// 编译期断言：适配层满足 `internal/history` 声明的端口（实现方 import 消费方，反过来不行）。
// Compile-time assertion that the adapter satisfies the consumer-side port.
var _ history.MessageQueryStore = (*HistoryStore)(nil)

// NewHistoryStore 绑定一个已打开的库句柄。
// NewHistoryStore binds the store to an opened database handle.
func NewHistoryStore(db *DB) *HistoryStore { return &HistoryStore{db: db} }

// historySearchSQL 是检索的**一条编译期常量**：投影、三路匹配、五个收窄条件、排序与窗口
// 全在 SQL 里，运行期不拼字符串（沿 `session_store.go` 会话列表的同一纪律——请求值只能
// 走占位符，调用方无法影响语句结构）。
//
// 三路匹配取并：原文子串（中文短查询最可靠）、`websearch_to_tsquery` 词元匹配（英文与
// 数字）与 `word_similarity` 三元组近似（错字 / 词形变化）。与 `knowledge_store.go` 的
// 关键词路同一形态、同一取舍：三路合并成一条 SQL，名次由返回顺序决定。
//
// 片段窗口固定 200 字符，起点围绕首个字面命中前推 40 字符（无字面命中时从头取），因此
// 命中位置不会被截掉、正文也不会整段外泄。可选过滤一律写成 `$n = 哨兵 OR 条件`：
// 空串 / 0 表示"该维不过滤"，既省掉动态拼 SQL，也让占位符位置固定。
//
// The search runs one compile-time statement: the projection, the three-way match, the five
// narrowing filters and the window are all fixed, and request values only ever bind to
// placeholders. A sentinel (empty string / zero) turns a filter off.
const historySearchSQL = `
	SELECT m.id, m.session_id, COALESCE(m.run_id, ''), m.role,
	       substring(COALESCE(m.content, '')
	                 FROM GREATEST(position(lower($1) IN lower(COALESCE(m.content, ''))) - 40, 1)
	                 FOR 200) AS snippet,
	       CASE
	         WHEN position(lower($1) IN lower(COALESCE(m.content, ''))) > 0 THEN 1.0
	         WHEN m.tsv @@ websearch_to_tsquery('simple', $1)
	           THEN 0.5 + 0.5 * ts_rank_cd(m.tsv, websearch_to_tsquery('simple', $1))
	         ELSE word_similarity($1, COALESCE(m.content, ''))
	       END AS score,
	       m.created_at
	  FROM messages m
	  JOIN sessions s ON s.session_id = m.session_id
	 WHERE (m.tsv @@ websearch_to_tsquery('simple', $1)
	        OR position(lower($1) IN lower(COALESCE(m.content, ''))) > 0
	        OR $1 <% COALESCE(m.content, ''))
	   AND ($2::text = '' OR s.caller_id = $2::text)
	   AND ($3::text = '' OR s.profile_name = $3::text)
	   AND ($4::text = '' OR m.session_id = $4::text)
	   AND ($5::bigint = 0 OR m.created_at >= $5::bigint)
	   AND ($6::bigint = 0 OR m.created_at <= $6::bigint)
	 ORDER BY m.created_at DESC, m.id DESC
	 LIMIT $7::bigint`

// historyReadSQL 是精确读取的一条编译期常量。归属条件同样写成哨兵形态：`$2` 空串表示
// 管理员不限调用方，否则只放行落在该调用方名下的消息（`caller_id` 为 NULL 的遗留行对
// 空串以外的任何值都匹配不上，是 SQL 的天然语义，不是应用层补的）。
//
// The exact-read statement; an empty $2 means the admin scope, otherwise rows must belong
// to that caller.
const historyReadSQL = `
	SELECT m.id, m.session_id, COALESCE(m.run_id, ''), m.seq, m.role,
	       COALESCE(m.content, ''), COALESCE(m.tool_call_id, ''), m.created_at
	  FROM messages m
	  JOIN sessions s ON s.session_id = m.session_id
	 WHERE m.id = ANY($1::bigint[])
	   AND ($2::text = '' OR s.caller_id = $2::text)`

// historyResolveSessionSQL 解析会话归属，哨兵语义与 `historyReadSQL` 相同。
// The session-ownership statement, with the same sentinel semantics as historyReadSQL.
const historyResolveSessionSQL = `
	SELECT s.session_id, s.caller_id, s.profile_name, s.created_at
	  FROM sessions s
	 WHERE s.session_id = $1
	   AND ($2::text = '' OR s.caller_id = $2::text)`

// historyScopeCaller 结算**生效的归属过滤值**，空串表示该范围不限调用方。
//
// 判据顺序（顺序本身是安全语义，不得调换）：
//  1. `scope.CallerID` 非空 → 恒按它过滤（调用方只能是自己）；
//  2. 否则若管理员给了收窄的 `narrow` → 按收窄值过滤；
//  3. 否则管理员不限调用方（空值，`caller_id` 为 NULL 的遗留行一并可见）；
//  4. 既不是管理员又没有 `CallerID` → **拒绝**：空值会让过滤退化成"全部"，是 C-28
//     明确禁止的越权形态，**不得**当成"不限"放过。
//
// The resolved owner wins over the admin's narrowing, and a caller scope never degrades
// into "all".
func historyScopeCaller(scope history.Scope, narrow string) (string, error) {
	if scope.CallerID != "" {
		return scope.CallerID, nil
	}
	if narrow != "" {
		return narrow, nil
	}
	if scope.Admin {
		return "", nil
	}
	return "", fmt.Errorf("%w: 调用方范围缺 caller_id", history.ErrForbidden)
}

// SearchMessages 按范围与条件检索消息指针，返回至多 `q.Limit` 条。
//
// `q.Limit` 是**原始行数上限**（`history.Search` 会加一探测下一页），本层不夹取、不改写；
// 0 表示不要任何行，不是"不限"。排序固定为时间倒序、同刻按 `messages.id` 倒序——分页
// 稳定性由存储保证，handler 不重排。
// SearchMessages returns at most q.Limit pointers in the fixed descending order.
func (s *HistoryStore) SearchMessages(ctx context.Context, scope history.Scope, q history.HistoryQuery) ([]history.HistoryHit, error) {
	if s == nil || s.db == nil || s.db.pool == nil {
		return nil, history.ErrHistoryUnavailable
	}
	keywords := strings.TrimSpace(q.Keywords)
	if keywords == "" {
		return nil, fmt.Errorf("%w: 关键词为空", history.ErrInvalidQuery)
	}
	if q.Limit <= 0 {
		return []history.HistoryHit{}, nil
	}

	caller, err := historyScopeCaller(scope, q.CallerID)
	if err != nil {
		return nil, err
	}
	var from, to int64
	if !q.From.IsZero() {
		from = q.From.UTC().UnixMilli()
	}
	if !q.To.IsZero() {
		to = q.To.UTC().UnixMilli()
	}

	rows, err := s.db.pool.QueryContext(ctx, historySearchSQL,
		keywords, caller, strings.TrimSpace(q.Agent), q.SessionID, from, to, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("检索历史消息: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]history.HistoryHit, 0, q.Limit)
	for rows.Next() {
		var (
			id        int64
			sessionID string
			runID     string
			role      string
			snippet   sql.NullString
			score     float64
			createdAt int64
		)
		if err := rows.Scan(&id, &sessionID, &runID, &role, &snippet, &score, &createdAt); err != nil {
			return nil, fmt.Errorf("读历史检索结果: %w", err)
		}
		out = append(out, history.HistoryHit{
			MessageID: strconv.FormatInt(id, 10),
			SessionID: sessionID,
			RunID:     runID,
			Role:      role,
			Snippet:   snippet.String,
			Score:     score,
			CreatedAt: time.UnixMilli(createdAt).UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读历史检索结果: %w", err)
	}
	return out, nil
}

// ReadMessages 按主键精确读取原文与来源引用。
//
// 语义（逐条对应端口契约）：
//   - 非十进制标识（含无状态字面量 `msg/<run>/<pos>`）不是 `messages.id` → `ErrNotFound`，
//     不拿它去查库；
//   - 任一请求的消息读不到（不存在、或不在 `scope` 的归属内）→ 整批 `ErrNotFound`：
//     精确读取是"按主键点名"，一条不在就是不在，**不得**用部分结果冒充成功；
//   - 给了 `MessageRef.SessionID` 时再复核一次会话：消息不在该会话 → `ErrNotFound`；
//   - 返回顺序与请求顺序一致；`EvidenceRefs` 按 `(kind, ref_id, version_id)` 定序原样透传。
//
// ReadMessages returns the requested rows in request order; any unresolved ref is a
// not-found verdict for the batch.
func (s *HistoryStore) ReadMessages(ctx context.Context, scope history.Scope, refs []history.MessageRef) ([]history.MessageView, error) {
	if s == nil || s.db == nil || s.db.pool == nil {
		return nil, history.ErrHistoryUnavailable
	}
	if len(refs) == 0 {
		return []history.MessageView{}, nil
	}

	ids := make([]int64, 0, len(refs))
	for _, ref := range refs {
		id, ok := parseMessagePrimaryKey(ref.MessageID)
		if !ok {
			return nil, fmt.Errorf("%w: 消息标识 %q 不是 messages.id", history.ErrNotFound, ref.MessageID)
		}
		ids = append(ids, id)
	}

	caller, err := historyScopeCaller(scope, "")
	if err != nil {
		return nil, err
	}

	rows, err := s.db.pool.QueryContext(ctx, historyReadSQL, ids, caller)
	if err != nil {
		return nil, fmt.Errorf("读消息原文: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byID := make(map[int64]history.MessageView, len(refs))
	found := make([]int64, 0, len(refs))
	for rows.Next() {
		var (
			id        int64
			sessionID string
			runID     string
			seq       int64
			role      string
			content   string
			callID    string
			createdAt int64
		)
		if err := rows.Scan(&id, &sessionID, &runID, &seq, &role, &content, &callID, &createdAt); err != nil {
			return nil, fmt.Errorf("读消息原文: %w", err)
		}
		// `Truncated` 保持 false：落库的就是当时写下的正文，截断发生在写入之前的工具结果
		// 上（`core.CommandResult.Truncated`），存储层没有第二个截断判定面——冻结 schema
		// 也没有承载它的列，本层**不**猜测。
		// Truncated stays false: storage holds whatever was written, and the schema has no
		// second truncation flag to read.
		byID[id] = history.MessageView{
			MessageID:  strconv.FormatInt(id, 10),
			SessionID:  sessionID,
			RunID:      runID,
			Seq:        seq,
			Role:       role,
			Content:    content,
			ToolCallID: callID,
			CreatedAt:  time.UnixMilli(createdAt).UTC(),
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读消息原文: %w", err)
	}

	refsByID, err := s.readEvidenceRefs(ctx, found)
	if err != nil {
		return nil, err
	}

	out := make([]history.MessageView, 0, len(refs))
	for _, ref := range refs {
		id, _ := parseMessagePrimaryKey(ref.MessageID)
		view, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("%w: 消息 %s", history.ErrNotFound, ref.MessageID)
		}
		if ref.SessionID != "" && view.SessionID != ref.SessionID {
			// 会话范围复核：给定了会话就必须落在该会话，否则按未找到（C-28：跨调用方
			// 不泄露"这条消息存在"）。
			// A mismatched session is a not-found verdict, never a hint that the row exists.
			return nil, fmt.Errorf("%w: 消息 %s 不在会话 %s", history.ErrNotFound, ref.MessageID, ref.SessionID)
		}
		view.EvidenceRefs = refsByID[id]
		out = append(out, view)
	}
	return out, nil
}

// readEvidenceRefs 一次取回这批消息的全部来源引用（`message_evidence_refs`）。
//
// 只按消息主键取，不再叠加归属条件：能进 `found` 的消息已经过 `ReadMessages` 的归属
// 过滤，引用行与消息同生同死。压缩引擎的会话读（`compaction_reader.go`）走同一份实现
// ——两处各写一份同义 SQL 就是两处漂移点。
// readEvidenceRefs loads the persisted refs for the already-scoped message ids through
// the one shared read path.
func (s *HistoryStore) readEvidenceRefs(ctx context.Context, ids []int64) (map[int64][]core.EvidenceRef, error) {
	return readMessageEvidenceRefs(ctx, s.db.pool, ids)
}

// ResolveSession 解析会话归属：不存在、或不在 `scope` 的范围内 → `ErrNotFound`。
// 空会话标识直接按未找到处理，不拿它去查库。
// ResolveSession resolves a session's owner inside the scope; out-of-scope is not-found.
func (s *HistoryStore) ResolveSession(ctx context.Context, scope history.Scope, sessionID string) (history.SessionRef, error) {
	if s == nil || s.db == nil || s.db.pool == nil {
		return history.SessionRef{}, history.ErrHistoryUnavailable
	}
	if sessionID == "" {
		return history.SessionRef{}, fmt.Errorf("%w: 空会话标识", history.ErrNotFound)
	}

	caller, err := historyScopeCaller(scope, "")
	if err != nil {
		return history.SessionRef{}, err
	}

	var (
		id        string
		callerID  sql.NullString
		agent     string
		createdAt int64
	)
	err = s.db.pool.QueryRowContext(ctx, historyResolveSessionSQL, sessionID, caller).
		Scan(&id, &callerID, &agent, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return history.SessionRef{}, fmt.Errorf("%w: 会话 %s", history.ErrNotFound, sessionID)
	}
	if err != nil {
		return history.SessionRef{}, fmt.Errorf("解析会话归属（session_id=%s）: %w", sessionID, err)
	}
	return history.SessionRef{
		SessionID: id,
		CallerID:  callerID.String,
		Agent:     agent,
		CreatedAt: time.UnixMilli(createdAt).UTC(),
	}, nil
}

// parseMessagePrimaryKey 判定并解析 `messages.id` 的十进制串。
//
// 只接受正整数：主键是 `GENERATED BY DEFAULT AS IDENTITY`，0 与负数都不可能有行；
// 无状态字面量 `msg/<run>/<pos>` 与任意其它形状同样不是主键。
// parseMessagePrimaryKey accepts only positive decimal ids.
func parseMessagePrimaryKey(id string) (int64, bool) {
	if id == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(id, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

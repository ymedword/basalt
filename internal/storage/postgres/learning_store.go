package postgres

// 本文件是 US-22 受控自我进化**五张账本表的唯一 SQL 入口**：memory_extractions /
// memory_extraction_cursors / memory_candidate_meta / memory_candidate_sources /
// memory_method_drafts。它同时实现 memory.LearningStore（抽取服务的写端口）与
// memory.LearningAdminStore（app 的只读查询与恢复查证端口）。
//
// 四条不可越过的边界（颗粒度文档 §2.3.3 / §3.1.4 / §3.1.7）：
//  1. 来源身份只从 runs 行复制：UnresolvedForOrigin / LoadCursor / BeginExtraction 都先读
//     触发 Run 的 (caller_id, COALESCE(key_id,''))，调用方没有写入来源的入口；候选元数据
//     再由提交事务从抽取行复制——跨 Run 重试因此不会把旧窗口重新贴成当前请求的身份。
//  2. 判定只有两个执行点：未决阻断与滚动预算都在 BeginExtraction 的**同一事务**内、
//     同一把 Agent advisory lock 下判定；提交事务再取同一把锁（同 Agent 串行）。
//     锁顺序全程「advisory → 行锁」，与 BeginExtraction 一致，避免与重试路径互锁。
//  3. 终态只写一次：FailExtraction / RecoverExtraction / CommitExtraction 都带
//     `status='pending'` 条件；非 pending 行是幂等空操作（已恢复的行绝不被覆盖）。
//  4. 时间显式传入或取句柄时钟（约束 25）：方法体内不散落 time.Now()；窗口边界只在
//     首次尝试写入，重试路径不更新 window_from/to。
//
// The only SQL entry point for the five US-22 learning ledgers. Origin identity is
// always copied from the runs row, unresolved/budget judgments live inside one
// BeginExtraction transaction under the agent advisory lock, and every terminal write is
// guarded by status='pending' so a recovered row is never overwritten.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// 预算与列表上限是代码常量，不是配置键（§3.1.12 / §3.1.10）：滚动 24h 内每 Agent 最多
// 200 次尝试；列表缺省 50、上限 200。端口签名里没有这两个值，判定只能落在存储层。
// The budget and list limits are code constants mirrored from §3.1.12 / §3.1.10.
const (
	learningAttemptsPerAgent = 200
	learningBudgetWindow     = 24 * time.Hour
	learningDefaultListLimit = 50
	learningMaxListLimit     = 200
	// learningErrorLimit 与 DDL 的 CHECK（length(error_message) <= 1024 字符）同源；
	// truncateUTF8 按字节截断，必然不超过字符上限。
	// learningErrorLimit mirrors the DDL CHECK; byte truncation stays within the char bound.
	learningErrorLimit = 1024
)

// learningExtractionColumns 是抽取账本的完整列清单（读模型与 RETURNING 共用一份）：
// 加列时只改这里，扫描顺序随之对齐，不会出现两处漂移。
// learningExtractionColumns is the single column list shared by reads and RETURNING.
const learningExtractionColumns = `extraction_id, agent_name, session_id, run_id, last_attempt_run_id,
	origin_caller_id, origin_key_id, status, window_from_message_id, window_to_message_id,
	window_messages, dropped_window_messages, prior_messages, attempts, existing_entries,
	items_total, items_accepted, items_dropped, prompt_version, rule_version, provider, model,
	config_hash, failure_class, error_message, created_at, last_attempt_at, finished_at, verified_at`

// LearningRepo 是学习账本的唯一 SQL 入口。
// LearningRepo is the only SQL entry point for the learning ledgers.
type LearningRepo struct{ db *DB }

// 编译期断言：写端口与读写查询面由同一实现承载（与 LLMCallRepo 的既有形态一致）。
// Compile-time assertions that one implementation serves both consumer surfaces.
var (
	_ memory.LearningStore      = (*LearningRepo)(nil)
	_ memory.LearningAdminStore = (*LearningRepo)(nil)
)

// NewLearningRepo 绑定一个已打开的库句柄。
// NewLearningRepo binds the repo to an opened database handle.
func NewLearningRepo(db *DB) *LearningRepo { return &LearningRepo{db: db} }

// learningScanner 同时覆盖 *sql.Row 与 *sql.Rows 的最小扫描面。
// learningScanner covers the Scan surface shared by *sql.Row and *sql.Rows.
type learningScanner interface{ Scan(dest ...any) error }

// scanLearningExtraction 把一行抽取账本投影成领域形态。可空列（config_hash /
// failure_class / error_message / finished_at / verified_at）落成零值：`VerifiedAt.IsZero()`
// 是"未查证 unknown 阻断"的判据，NULL 必须还原成零值而不是 Unix 纪元。
// scanLearningExtraction maps one row, restoring NULL times to the zero value so
// "verified_at IS NULL" survives into the Retryable/blocking predicates.
func scanLearningExtraction(scan learningScanner) (core.LearningExtraction, error) {
	var (
		row                                    core.LearningExtraction
		status                                 string
		configHash, failureClass, errorMessage sql.NullString
		createdAt, lastAttemptAt               int64
		finishedAt, verifiedAt                 sql.NullInt64
	)
	err := scan.Scan(
		&row.ExtractionID, &row.Agent, &row.SessionID, &row.RunID, &row.LastAttemptRunID,
		&row.OriginCallerID, &row.OriginKeyID, &status, &row.WindowFromMessageID,
		&row.WindowToMessageID, &row.WindowMessages, &row.DroppedWindowMessages,
		&row.PriorMessages, &row.Attempts, &row.ExistingEntries, &row.ItemsTotal,
		&row.ItemsAccepted, &row.ItemsDropped, &row.PromptVersion, &row.RuleVersion,
		&row.Provider, &row.Model, &configHash, &failureClass, &errorMessage,
		&createdAt, &lastAttemptAt, &finishedAt, &verifiedAt)
	if err != nil {
		return core.LearningExtraction{}, err
	}
	row.Status = core.LearningExtractionState(status)
	row.ConfigHash = configHash.String
	row.FailureClass = core.LearningFailureClass(failureClass.String)
	row.ErrorMessage = errorMessage.String
	row.CreatedAt = learningTimeFromMS(createdAt)
	row.LastAttemptAt = learningTimeFromMS(lastAttemptAt)
	row.FinishedAt = learningTimeFromNull(finishedAt)
	row.VerifiedAt = learningTimeFromNull(verifiedAt)
	return row, nil
}

// learningTimeFromMS 把 UTC Unix 毫秒还原成时间；learningTimeFromNull 把 NULL 还原成零值。
// learningTimeFromMS restores a UTC time; learningTimeFromNull keeps NULL as zero.
func learningTimeFromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func learningTimeFromNull(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return learningTimeFromMS(ms.Int64)
}

// learningRunOrigin 读取触发 Run 的来源身份（原则 III 的同一条口径：身份取自数据库，
// 不取请求）。runs.caller_id 为 NOT NULL，key_id 为 NULL 时落空串（本地管理员）。
// learningRunOrigin reads the origin identity from the runs row, never from a request.
func learningRunOrigin(ctx context.Context, q Queryer, runID string) (callerID, keyID string, err error) {
	err = q.QueryRowContext(ctx,
		`SELECT caller_id, COALESCE(key_id, '') FROM runs WHERE run_id = $1`, runID).
		Scan(&callerID, &keyID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("学习抽取: 运行记录不存在（run_id=%s）", runID)
	}
	if err != nil {
		return "", "", fmt.Errorf("学习抽取: 读取运行来源（run_id=%s）: %w", runID, err)
	}
	return callerID, keyID, nil
}

// UnresolvedForOrigin 只返回需要阻断或重试的行：pending、failed 且 attempts < 3、
// 以及 verified_at IS NULL 的 unknown；success / skipped / 已查证 unknown 返回 nil。
// 同来源按创建时刻取最近一行（正常路径下同来源至多一行未决）。
// UnresolvedForOrigin returns only rows that block or retry: pending, retryable failed,
// and unverified unknown; every other state yields nil.
func (r *LearningRepo) UnresolvedForOrigin(ctx context.Context, agent, sessionID, runID string) (*core.LearningExtraction, error) {
	callerID, keyID, err := learningRunOrigin(ctx, r.db.pool, runID)
	if err != nil {
		return nil, err
	}
	row, err := scanLearningExtraction(r.db.pool.QueryRowContext(ctx,
		`SELECT `+learningExtractionColumns+`
		   FROM memory_extractions
		  WHERE agent_name = $1 AND session_id = $2
		    AND origin_caller_id = $3 AND origin_key_id = $4
		    AND (status = 'pending'
		         OR (status = 'failed' AND attempts < 3)
		         OR (status = 'unknown' AND verified_at IS NULL))
		  ORDER BY created_at DESC, extraction_id DESC
		  LIMIT 1`, agent, sessionID, callerID, keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("学习抽取: 读取未决行（agent=%s, session_id=%s）: %w", agent, sessionID, err)
	}
	return &row, nil
}

// LoadCursor 读取 (agent, session, 来源) 维度的处理位置；无游标返回 found=false 与
// 零位置（业务上等价于"从会话第一条消息开始"）。来源仍从 runs 行取——游标不是
// 会话级对象，同一会话的不同来源各自推进、互不覆盖。
// LoadCursor returns the per-origin position; an absent cursor is found=false with a zero id.
func (r *LearningRepo) LoadCursor(ctx context.Context, agent, sessionID, runID string) (core.LearningCursor, bool, error) {
	callerID, keyID, err := learningRunOrigin(ctx, r.db.pool, runID)
	if err != nil {
		return core.LearningCursor{}, false, err
	}
	cursor := core.LearningCursor{
		Agent:          agent,
		SessionID:      sessionID,
		OriginCallerID: callerID,
		OriginKeyID:    keyID,
	}
	var updatedAt int64
	err = r.db.pool.QueryRowContext(ctx,
		`SELECT committed_message_id, updated_at
		   FROM memory_extraction_cursors
		  WHERE agent_name = $1 AND session_id = $2
		    AND origin_caller_id = $3 AND origin_key_id = $4`,
		agent, sessionID, callerID, keyID).Scan(&cursor.CommittedMessageID, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cursor, false, nil
	}
	if err != nil {
		return core.LearningCursor{}, false, fmt.Errorf("学习抽取: 读取游标（agent=%s, session_id=%s）: %w", agent, sessionID, err)
	}
	cursor.UpdatedAt = learningTimeFromMS(updatedAt)
	return cursor, true, nil
}

// EntriesForExtraction 返回未过期的 candidate / active 条目快照（供提示词、去重与
// 建议取代）。deleted / conflicted / superseded 与已到期条目一律不返回——建议取代一个
// 已失效条目没有意义，把它展示给模型只会诱导引用。limit / maxBytes 双上限：
// 取最近条目、任一上限先到即停，最后按主键升序返回（提示词必须逐字节确定）。
// EntriesForExtraction returns unexpired candidate/active rows, newest first, capped by
// both limits and returned in ascending id order for a deterministic prompt.
func (r *LearningRepo) EntriesForExtraction(ctx context.Context, scope core.Scope, limit, maxBytes int) ([]memory.LearningEntrySnapshot, error) {
	scopeCol, agentName := scopeColumns(scope)
	rows, err := r.db.pool.QueryContext(ctx,
		`SELECT id, section, state, content FROM memory_entries
		  WHERE scope = $1 AND agent_name IS NOT DISTINCT FROM $2
		    AND state IN ('candidate', 'active')
		    AND (valid_to IS NULL OR valid_to > $3)
		  ORDER BY id DESC
		  LIMIT $4`, scopeCol, agentName, r.db.clock().UnixMilli(), limitArg(limit))
	if err != nil {
		return nil, fmt.Errorf("学习抽取: 读取条目快照（%s）: %w", scopeCol, err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out   []memory.LearningEntrySnapshot
		bytes int
	)
	for rows.Next() {
		var (
			snap           memory.LearningEntrySnapshot
			section, state string
		)
		if err := rows.Scan(&snap.EntryID, &section, &state, &snap.Content); err != nil {
			return nil, fmt.Errorf("学习抽取: 扫描条目快照（%s）: %w", scopeCol, err)
		}
		// 至少保留一条：快照为空则去重与建议取代都失去依据（与窗口的同一取舍）。
		if maxBytes > 0 && len(out) > 0 && bytes+len(snap.Content) > maxBytes {
			break
		}
		snap.Section = core.Section(section)
		snap.State = state
		out = append(out, snap)
		bytes += len(snap.Content)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("学习抽取: 遍历条目快照（%s）: %w", scopeCol, err)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// BeginExtraction 在单事务内完成四件事（§3.1.7）：取 Agent advisory lock → 从 runs 行
// 复制来源 → 重判同来源未决行 → 滚动 24h 尝试上限。判定先于任何写入：未决返回
// ErrLearningUnresolved、预算耗尽返回 ErrLearningBudgetExhausted，两者都不建行、不发调用。
// 通过后：存在 failed 且 attempts < 3 的行就把它改回 pending 并递增（window_from/to 不变，
// 本次 Run 的身份与版本号刷新），否则插入新的 pending 行（attempts=1）。返回落库后的整行。
// BeginExtraction is the single execution point for unresolved blocking and the rolling
// budget; both refusals happen before any write.
func (r *LearningRepo) BeginExtraction(ctx context.Context, row core.LearningExtraction) (core.LearningExtraction, error) {
	now := row.LastAttemptAt
	if now.IsZero() {
		now = r.db.clock()
	}
	createdAt := row.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	nowMs := now.UnixMilli()

	var out core.LearningExtraction
	err := r.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 锁顺序固定为「advisory → 行锁」：提交事务与重试路径都按同一顺序取锁，不会互锁。
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtext('memory_extract:' || $1))`, row.Agent); err != nil {
			return fmt.Errorf("学习抽取: 取 Agent 锁（agent=%s）: %w", row.Agent, err)
		}
		callerID, keyID, err := learningRunOrigin(ctx, tx, row.RunID)
		if err != nil {
			return err
		}

		// 未决阻断：pending 与未查证 unknown 都不建行、不发调用（恢复查证后自然解除）。
		_, err = scanLearningExtraction(tx.QueryRowContext(ctx,
			`SELECT `+learningExtractionColumns+`
			   FROM memory_extractions
			  WHERE agent_name = $1 AND session_id = $2
			    AND origin_caller_id = $3 AND origin_key_id = $4
			    AND (status = 'pending' OR (status = 'unknown' AND verified_at IS NULL))
			  ORDER BY created_at DESC, extraction_id DESC
			  LIMIT 1`, row.Agent, row.SessionID, callerID, keyID))
		if err == nil {
			return core.ErrLearningUnresolved
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("学习抽取: 判定未决行（agent=%s）: %w", row.Agent, err)
		}

		retry, retryErr := scanLearningExtraction(tx.QueryRowContext(ctx,
			`SELECT `+learningExtractionColumns+`
			   FROM memory_extractions
			  WHERE agent_name = $1 AND session_id = $2
			    AND origin_caller_id = $3 AND origin_key_id = $4
			    AND status = 'failed' AND attempts < 3
			  ORDER BY created_at DESC, extraction_id DESC
			  LIMIT 1
			  FOR UPDATE`, row.Agent, row.SessionID, callerID, keyID))
		hasRetry := retryErr == nil
		if retryErr != nil && !errors.Is(retryErr, sql.ErrNoRows) {
			return fmt.Errorf("学习抽取: 读取可重试行（agent=%s）: %w", row.Agent, retryErr)
		}

		// 滚动预算：按每行 last_attempt_at 归组（重试计入）；达到上限即拒绝，宁可少算额度
		// 也不超额（§3.1.12）。判定在写入之前，耗尽不建行、不增量。
		var used int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(attempts), 0) FROM memory_extractions
			  WHERE agent_name = $1 AND last_attempt_at >= $2`,
			row.Agent, now.Add(-learningBudgetWindow).UnixMilli()).Scan(&used); err != nil {
			return fmt.Errorf("学习抽取: 判定尝试预算（agent=%s）: %w", row.Agent, err)
		}
		if used >= learningAttemptsPerAgent {
			return core.ErrLearningBudgetExhausted
		}

		if hasRetry {
			out, err = scanLearningExtraction(tx.QueryRowContext(ctx,
				`UPDATE memory_extractions
				    SET status = 'pending', attempts = attempts + 1,
				        last_attempt_run_id = $2, last_attempt_at = $3,
				        provider = $4, model = $5, config_hash = $6,
				        prompt_version = $7, rule_version = $8,
				        finished_at = NULL, failure_class = NULL, error_message = NULL
				  WHERE extraction_id = $1 AND status = 'failed'
				 RETURNING `+learningExtractionColumns,
				retry.ExtractionID, row.RunID, nowMs, row.Provider, row.Model,
				nullIfEmpty(row.ConfigHash), row.PromptVersion, row.RuleVersion))
			if err != nil {
				return fmt.Errorf("学习抽取: 重试改回 pending（%s）: %w", retry.ExtractionID, err)
			}
			return nil
		}

		// 来源不取 row.OriginCallerID / row.OriginKeyID——即使调用方填了，也以 runs 行为准。
		out, err = scanLearningExtraction(tx.QueryRowContext(ctx,
			`INSERT INTO memory_extractions
			   (extraction_id, agent_name, session_id, run_id, last_attempt_run_id,
			    origin_caller_id, origin_key_id, status, window_from_message_id, window_to_message_id,
			    window_messages, dropped_window_messages, prior_messages, attempts, existing_entries,
			    items_total, items_accepted, items_dropped, prompt_version, rule_version, provider, model,
			    config_hash, failure_class, error_message, created_at, last_attempt_at, finished_at, verified_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 1, $14,
			         0, 0, 0, $15, $16, $17, $18, $19, NULL, NULL, $20, $21, NULL, NULL)
			 RETURNING `+learningExtractionColumns,
			row.ExtractionID, row.Agent, row.SessionID, row.RunID, row.RunID,
			callerID, keyID, string(core.LearningExtractionPending),
			row.WindowFromMessageID, row.WindowToMessageID, row.WindowMessages,
			row.DroppedWindowMessages, row.PriorMessages, row.ExistingEntries,
			row.PromptVersion, row.RuleVersion, row.Provider, row.Model,
			nullIfEmpty(row.ConfigHash), createdAt.UnixMilli(), nowMs))
		if err != nil {
			return fmt.Errorf("学习抽取: 预写抽取行（%s）: %w", row.ExtractionID, err)
		}
		return nil
	})
	if err != nil {
		return core.LearningExtraction{}, err
	}
	return out, nil
}

// FailExtraction 在事务内把行 FOR UPDATE 后写失败终态（§3.1.7）：
//   - window_gone 是**终态**：直接 skipped 并推进游标，不递增 attempts；
//   - 其余分类在 attempts < 3 时写 failed（游标不动，finished_at 保持 NULL——该行还会重试）；
//   - attempts >= 3 时写 skipped 并推进游标。
//
// 回填来源只有两种合法状态：pending（正常失败路径），以及 failed + window_gone
// ——服务在 BeginExtraction **之前**重读冻结窗口（C-33），发现消息已被保留策略
// 清理时行还停在上一次尝试的 failed；若不在这里终结，行会永远 failed（attempts
// 不增、finished_at 为空、游标不推进），同来源新窗口被永久阻断。其余非 pending
// 行是幂等空操作：恢复已查证的 unknown 绝不被一次迟到的失败回填覆盖。
// FailExtraction writes the failure state under a row lock; the source states are pending,
// or failed+window_gone on the C-33 retry path; every other state is a no-op.
func (r *LearningRepo) FailExtraction(ctx context.Context, extractionID string, class core.LearningFailureClass, message string, failedAt time.Time) error {
	if !learningFailureClassAllowed(class) {
		return fmt.Errorf("学习抽取: 失败分类取值域外：%q", class)
	}
	if failedAt.IsZero() {
		failedAt = r.db.clock()
	}
	return r.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			agent, sessionID, callerID, keyID, status string
			attempts                                  int
			windowTo                                  int64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT agent_name, session_id, origin_caller_id, origin_key_id, status, attempts,
			        window_to_message_id
			   FROM memory_extractions WHERE extraction_id = $1 FOR UPDATE`, extractionID).
			Scan(&agent, &sessionID, &callerID, &keyID, &status, &attempts, &windowTo)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("学习抽取: 抽取行不存在（%s）: %w", extractionID, core.ErrLearningNotFound)
		}
		if err != nil {
			return fmt.Errorf("学习抽取: 读取失败回填目标（%s）: %w", extractionID, err)
		}
		fromStatus := string(core.LearningExtractionPending)
		switch {
		case status == fromStatus:
		case status == string(core.LearningExtractionFailed) && class == core.LearningFailureWindowGone:
			// C-33 重试路径：行仍是上一次尝试的 failed，直接终结为 skipped。
			fromStatus = string(core.LearningExtractionFailed)
		default:
			return nil // 已恢复 / 已终结：不覆盖
		}

		terminal := class == core.LearningFailureWindowGone || attempts >= 3
		target := core.LearningExtractionFailed
		if terminal {
			target = core.LearningExtractionSkipped
		}
		var finishedAt any
		if terminal {
			finishedAt = failedAt.UnixMilli()
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE memory_extractions
			    SET status = $2, failure_class = $3, error_message = $4, finished_at = $5
			  WHERE extraction_id = $1 AND status = $6`,
			extractionID, string(target), string(class),
			nullIfEmpty(truncateUTF8(message, learningErrorLimit)), finishedAt, fromStatus)
		if err != nil {
			return fmt.Errorf("学习抽取: 写失败终态（%s）: %w", extractionID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("学习抽取: 确认失败终态写入（%s）: %w", extractionID, err)
		}
		if affected != 1 {
			// 行锁在手却改不动：只剩"状态在锁外被改"这一种可能，按冲突上抛。
			return fmt.Errorf("学习抽取: 失败终态写入未生效（%s）: %w", extractionID, core.ErrConflict)
		}
		if terminal {
			return advanceLearningCursor(ctx, tx, agent, sessionID, callerID, keyID, windowTo, failedAt)
		}
		return nil
	})
}

// CommitExtraction 在单事务内写候选、元数据、来源关系、草案行、success 状态并推进游标
// （§3.1.7）。候选写入复用包内唯一的 insertMemoryRow（不经过 InsertEntry 的独立事务）；
// 去重与来源子集在**事务内按数据库重新判定**——提示词快照只有 8 KiB，截断的快照不得让
// 重复条目漏过，也不得让模型把不存在的条目当成来源。被事务内判定丢弃的候选计入
// items_dropped、从 items_accepted 中扣除，账本与实际写入行保持一致。
// CommitExtraction re-judges dedupe and source existence inside the transaction and
// keeps the ledger counts consistent with the rows actually written.
func (r *LearningRepo) CommitExtraction(ctx context.Context, req core.CommitLearningExtraction) (core.LearningCommitResult, error) {
	now := req.CompletedAt
	if now.IsZero() {
		now = r.db.clock()
	}
	nowMs := now.UnixMilli()
	result := core.LearningCommitResult{}

	err := r.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 先做**不加锁**的 Agent 读取，再取 advisory lock，最后取行锁：锁顺序与
		// BeginExtraction 一致（advisory → 行锁），不会与"重试改回 pending"互锁。
		var agent string
		err := tx.QueryRowContext(ctx,
			`SELECT agent_name FROM memory_extractions WHERE extraction_id = $1`, req.ExtractionID).
			Scan(&agent)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("学习抽取: 抽取行不存在（%s）: %w", req.ExtractionID, core.ErrLearningNotFound)
		}
		if err != nil {
			return fmt.Errorf("学习抽取: 读取提交目标（%s）: %w", req.ExtractionID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock(hashtext('memory_extract:' || $1))`, agent); err != nil {
			return fmt.Errorf("学习抽取: 取 Agent 锁（agent=%s）: %w", agent, err)
		}

		row, err := scanLearningExtraction(tx.QueryRowContext(ctx,
			`SELECT `+learningExtractionColumns+`
			   FROM memory_extractions WHERE extraction_id = $1 FOR UPDATE`, req.ExtractionID))
		if err != nil {
			return fmt.Errorf("学习抽取: 锁定提交目标（%s）: %w", req.ExtractionID, err)
		}
		if row.Status != core.LearningExtractionPending {
			// 已被恢复查证或已终结：不覆盖已恢复行（提交只会发生在 pending 上）。
			return fmt.Errorf("学习抽取: 提交目标不在 pending（%s，status=%s）: %w",
				req.ExtractionID, row.Status, core.ErrConflict)
		}

		droppedAtCommit := 0
		for _, candidate := range req.Candidates {
			entryID, written, err := commitLearningCandidate(ctx, tx, row, candidate, nowMs)
			if err != nil {
				return err
			}
			if !written {
				droppedAtCommit++
				continue
			}
			result.CandidateEntryIDs = append(result.CandidateEntryIDs, entryID)
		}

		for _, draft := range req.Drafts {
			evidenceJSON, err := marshalLearningEvidence(draft.Evidence)
			if err != nil {
				return fmt.Errorf("学习抽取: 序列化草案证据（%s）: %w", draft.DraftID, err)
			}
			createdAt := draft.CreatedAt
			if createdAt.IsZero() {
				createdAt = now
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO memory_method_drafts
				   (draft_id, extraction_id, agent_name, name, content_sha256, evidence_json, rel_dir, created_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				draft.DraftID, req.ExtractionID, row.Agent, draft.Name, draft.ContentSHA256,
				evidenceJSON, draft.RelDir, createdAt.UnixMilli()); err != nil {
				return fmt.Errorf("学习抽取: 写草案行（%s）: %w", draft.DraftID, err)
			}
			result.DraftIDs = append(result.DraftIDs, draft.DraftID)
		}

		accepted := req.ItemsAccepted - droppedAtCommit
		if accepted < 0 {
			accepted = 0
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE memory_extractions
			    SET status = 'success', items_total = $2, items_accepted = $3,
			        items_dropped = $4, finished_at = $5
			  WHERE extraction_id = $1 AND status = 'pending'`,
			req.ExtractionID, req.ItemsTotal, accepted, req.ItemsDropped+droppedAtCommit, nowMs)
		if err != nil {
			return fmt.Errorf("学习抽取: 写提交终态（%s）: %w", req.ExtractionID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("学习抽取: 确认提交终态写入（%s）: %w", req.ExtractionID, err)
		}
		if affected != 1 {
			return fmt.Errorf("学习抽取: 提交终态写入未生效（%s）: %w", req.ExtractionID, core.ErrConflict)
		}
		return advanceLearningCursor(ctx, tx, row.Agent, row.SessionID,
			row.OriginCallerID, row.OriginKeyID, row.WindowToMessageID, now)
	})
	if err != nil {
		return core.LearningCommitResult{}, err
	}
	return result, nil
}

// commitLearningCandidate 写入一条候选并返回其主键；written=false 表示被事务内判定丢弃
// （同文重复，或来源 ID 在库中不存在）。范围与分区由程序固定为 Agent 层 + 归档区：
// 候选永不进入核心区注入面。建议取代目标在库中不存在时清空建议、条目保留——与逐项
// 校验的处置一致（旧建议不得指向不存在的行，也绝不因此失败整次抽取）。
// commitLearningCandidate pins scope=agent / section=archive and drops a duplicate or a
// candidate whose sources no longer exist.
func commitLearningCandidate(ctx context.Context, tx *sql.Tx, row core.LearningExtraction,
	candidate core.LearningCandidate, nowMs int64,
) (int64, bool, error) {
	scopeCol, agentName := scopeColumns(core.Scope{Agent: row.Agent})
	section := core.SectionArchive
	content := candidate.Content

	var existingID int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM memory_entries
		  WHERE scope = $1 AND agent_name IS NOT DISTINCT FROM $2
		    AND section = $3 AND content_hash = $4
		    AND state IN ('candidate', 'active')
		    AND (valid_to IS NULL OR valid_to > $5)
		  LIMIT 1`,
		scopeCol, agentName, string(section), memoryContentHash(content), nowMs).Scan(&existingID)
	if err == nil {
		return 0, false, nil // 库内同文已存在：快照被截断时也不得漏过
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("学习抽取: 提交去重判定（已有条目 %d）: %w", existingID, err)
	}

	sourceIDs := dedupeLearningIDs(candidate.SourceEntryIDs)
	if len(sourceIDs) > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM memory_entries WHERE id = ANY($1)`, sourceIDs)
		if err != nil {
			return 0, false, fmt.Errorf("学习抽取: 复核来源条目: %w", err)
		}
		found := make(map[int64]struct{}, len(sourceIDs))
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return 0, false, fmt.Errorf("学习抽取: 扫描来源条目: %w", err)
			}
			found[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, false, fmt.Errorf("学习抽取: 遍历来源条目: %w", err)
		}
		_ = rows.Close()
		if len(found) != len(sourceIDs) {
			// 来源不存在的候选按 fail-closed 丢弃：不能落一条引用空来源的派生条目。
			return 0, false, nil
		}
	}

	suggested := int64(0)
	if candidate.SuggestedSupersedesID > 0 {
		var exists int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM memory_entries WHERE id = $1`, candidate.SuggestedSupersedesID).Scan(&exists)
		switch {
		case err == nil:
			suggested = candidate.SuggestedSupersedesID
		case errors.Is(err, sql.ErrNoRows):
			suggested = 0 // 建议目标不在库中：清空建议，条目保留
		default:
			return 0, false, fmt.Errorf("学习抽取: 复核建议取代目标: %w", err)
		}
	}

	validToMs := int64(0)
	if !candidate.ValidTo.IsZero() {
		validToMs = candidate.ValidTo.UnixMilli()
	}
	entryID, err := insertMemoryRow(ctx, tx, memoryRow{
		scopeCol:  scopeCol,
		agentName: agentName,
		section:   section,
		content:   content,
		state:     memoryStateCandidate,
		source:    memorySourceAutoExtract,
		createdAt: nowMs,
		validTo:   validToMs,
	})
	if err != nil {
		return 0, false, err
	}

	evidenceJSON, err := marshalLearningEvidence(candidate.Evidence)
	if err != nil {
		return 0, false, fmt.Errorf("学习抽取: 序列化候选证据（%s）: %w", row.ExtractionID, err)
	}
	evidenceAt := candidate.EvidenceAt.UnixMilli()
	if evidenceAt < 1 {
		// C-25：evidence_at 恒 ≥ 1；缺证据时刻的条目不得落库（DB CHECK 兜底）。
		return 0, false, fmt.Errorf("学习抽取: 候选缺少证据时刻（%s）", row.ExtractionID)
	}
	var suggestedArg, validToArg any
	if suggested > 0 {
		suggestedArg = suggested
	}
	if validToMs > 0 {
		validToArg = validToMs
	}
	if sourceIDs == nil {
		sourceIDs = []int64{}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memory_candidate_meta
		   (entry_id, extraction_id, kind, certainty, confidence, evidence_json, source_entry_ids,
		    suggested_supersedes_id, origin_caller_id, origin_key_id, evidence_at, valid_to,
		    time_confidence, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		entryID, row.ExtractionID, string(candidate.Kind), candidate.Certainty, candidate.Confidence,
		evidenceJSON, sourceIDs, suggestedArg, row.OriginCallerID, row.OriginKeyID,
		evidenceAt, validToArg, candidate.TimeConfidence, nowMs); err != nil {
		return 0, false, fmt.Errorf("学习抽取: 写候选元数据（entry=%d）: %w", entryID, err)
	}

	for _, sourceID := range sourceIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO memory_candidate_sources (entry_id, source_entry_id, extraction_id, created_at)
			 VALUES ($1, $2, $3, $4)`,
			entryID, sourceID, row.ExtractionID, nowMs); err != nil {
			return 0, false, fmt.Errorf("学习抽取: 写来源关系（entry=%d）: %w", entryID, err)
		}
	}
	return entryID, true, nil
}

// advanceLearningCursor 把该抽取行来源的游标推进到给定消息主键；GREATEST 保证并发 /
// 乱序推进不会把游标倒退。messageID <= 0 不写（没有可推进的位置）。
// advanceLearningCursor advances the per-origin cursor without ever moving it backwards.
func advanceLearningCursor(ctx context.Context, tx *sql.Tx, agent, sessionID, callerID, keyID string,
	messageID int64, at time.Time,
) error {
	if messageID <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO memory_extraction_cursors
		   (agent_name, session_id, origin_caller_id, origin_key_id, committed_message_id, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (agent_name, session_id, origin_caller_id, origin_key_id)
		 DO UPDATE SET
		   committed_message_id = GREATEST(memory_extraction_cursors.committed_message_id,
		                                   EXCLUDED.committed_message_id),
		   updated_at = EXCLUDED.updated_at`,
		agent, sessionID, callerID, keyID, messageID, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("学习抽取: 推进游标（agent=%s, session_id=%s）: %w", agent, sessionID, err)
	}
	return nil
}

// PendingExtractions 返回所有 pending 行供启动恢复查证（只查证、不重放）。
// PendingExtractions lists every pending row for startup verification; nothing is replayed.
func (r *LearningRepo) PendingExtractions(ctx context.Context) ([]core.LearningExtraction, error) {
	rows, err := r.db.pool.QueryContext(ctx,
		`SELECT `+learningExtractionColumns+`
		   FROM memory_extractions WHERE status = 'pending'
		  ORDER BY created_at, extraction_id`)
	if err != nil {
		return nil, fmt.Errorf("学习抽取: 列出待决行: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.LearningExtraction
	for rows.Next() {
		row, err := scanLearningExtraction(rows)
		if err != nil {
			return nil, fmt.Errorf("学习抽取: 扫描待决行: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("学习抽取: 遍历待决行: %w", err)
	}
	return out, nil
}

// ExtractionCandidateCount 与 ExtractionDraftCount 是恢复查证的事实来源：任一大于 0
// 即证明提交事务已写出结果（重启后的 pending 应判 success）。两者刻意分开计数，
// 便于报告区分"只有草案"与"只有候选"的形态。
// ExtractionCandidateCount / ExtractionDraftCount back the recovery verdict.
func (r *LearningRepo) ExtractionCandidateCount(ctx context.Context, extractionID string) (int, error) {
	var count int
	if err := r.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_candidate_meta WHERE extraction_id = $1`, extractionID).
		Scan(&count); err != nil {
		return 0, fmt.Errorf("学习抽取: 统计候选（%s）: %w", extractionID, err)
	}
	return count, nil
}

func (r *LearningRepo) ExtractionDraftCount(ctx context.Context, extractionID string) (int, error) {
	var count int
	if err := r.db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_method_drafts WHERE extraction_id = $1`, extractionID).
		Scan(&count); err != nil {
		return 0, fmt.Errorf("学习抽取: 统计草案（%s）: %w", extractionID, err)
	}
	return count, nil
}

// RecoverExtraction 在单事务内只处理 pending 行：写 success / unknown 与 verified_at，
// 并把该行来源的游标推进到 window_to_message_id（unknown 也不重放）。已处理过的行是
// 幂等空操作——恢复不能把已查证的结论改口。
// RecoverExtraction verifies a pending row in one transaction; a non-pending row is a no-op.
func (r *LearningRepo) RecoverExtraction(ctx context.Context, extractionID string,
	status core.LearningExtractionState, verifiedAt time.Time,
) error {
	if status != core.LearningExtractionSuccess && status != core.LearningExtractionUnknown {
		return fmt.Errorf("学习抽取: 恢复查证状态取值域外：%q", status)
	}
	if verifiedAt.IsZero() {
		verifiedAt = r.db.clock()
	}
	return r.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			agent, sessionID, callerID, keyID, current string
			windowTo                                   int64
		)
		err := tx.QueryRowContext(ctx,
			`SELECT agent_name, session_id, origin_caller_id, origin_key_id, status,
			        window_to_message_id
			   FROM memory_extractions WHERE extraction_id = $1 FOR UPDATE`, extractionID).
			Scan(&agent, &sessionID, &callerID, &keyID, &current, &windowTo)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("学习抽取: 抽取行不存在（%s）: %w", extractionID, core.ErrLearningNotFound)
		}
		if err != nil {
			return fmt.Errorf("学习抽取: 读取恢复目标（%s）: %w", extractionID, err)
		}
		if current != string(core.LearningExtractionPending) {
			return nil // 已查证 / 已终结：不改口
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE memory_extractions SET status = $2, verified_at = $3
			  WHERE extraction_id = $1 AND status = 'pending'`,
			extractionID, string(status), verifiedAt.UnixMilli())
		if err != nil {
			return fmt.Errorf("学习抽取: 写恢复结论（%s）: %w", extractionID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("学习抽取: 确认恢复结论写入（%s）: %w", extractionID, err)
		}
		if affected != 1 {
			return fmt.Errorf("学习抽取: 恢复结论写入未生效（%s）: %w", extractionID, core.ErrConflict)
		}
		return advanceLearningCursor(ctx, tx, agent, sessionID, callerID, keyID, windowTo, verifiedAt)
	})
}

// ListExtractions 是管理台与 CLI 的只读列表：主键倒序、可选 Agent / 状态过滤，并带出
// 每条抽取的候选与草案投影（证据与来源关系可复核）。它不做权限过滤——调用方身份在
// 管理面先判过（与记忆管理页同口径）。
// ListExtractions returns the read-only page with candidates and drafts attached.
func (r *LearningRepo) ListExtractions(ctx context.Context, filter core.LearningExtractionFilter) ([]core.LearningExtractionView, error) {
	if filter.State != "" && !learningStateAllowed(core.LearningExtractionState(filter.State)) {
		return nil, fmt.Errorf("学习抽取: 状态过滤取值域外：%q", filter.State)
	}
	var (
		args  []any
		where []string
	)
	query := `SELECT ` + learningExtractionColumns + ` FROM memory_extractions`
	if filter.Agent != "" {
		args = append(args, filter.Agent)
		where = append(where, fmt.Sprintf("agent_name = $%d", len(args)))
	}
	if filter.State != "" {
		args = append(args, filter.State)
		where = append(where, fmt.Sprintf("status = $%d", len(args)))
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, learningListLimit(filter.Limit))
	// #nosec G202 -- 拼接的每一段都可追到本函数内的固定字面量：`agent_name = $%d` /
	// `status = $%d` 只把占位符序号（= len(args)）填进模板，过滤取值全部走位置参数，
	// LIMIT 经 learningListLimit 钳制后同样只占位；没有外部取值进入 SQL 文本。
	// Every fragment is a fixed literal here; values travel as bound parameters.
	query += fmt.Sprintf(" ORDER BY created_at DESC, extraction_id DESC LIMIT $%d", len(args))

	rows, err := r.db.pool.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("学习抽取: 列出抽取行: %w", err)
	}
	defer func() { _ = rows.Close() }()

	views := make([]core.LearningExtractionView, 0, 8)
	for rows.Next() {
		row, err := scanLearningExtraction(rows)
		if err != nil {
			return nil, fmt.Errorf("学习抽取: 扫描抽取行: %w", err)
		}
		views = append(views, core.LearningExtractionView{Extraction: row})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("学习抽取: 遍历抽取行: %w", err)
	}
	return r.attachLearningChildren(ctx, views)
}

// LoadExtraction 读取一条抽取及其候选 / 草案；found=false 表示不存在（web 映射 404）。
// LoadExtraction reads one extraction with its children; found separates absent from broken.
func (r *LearningRepo) LoadExtraction(ctx context.Context, extractionID string) (core.LearningExtractionView, bool, error) {
	row, err := scanLearningExtraction(r.db.pool.QueryRowContext(ctx,
		`SELECT `+learningExtractionColumns+` FROM memory_extractions WHERE extraction_id = $1`,
		extractionID))
	if errors.Is(err, sql.ErrNoRows) {
		return core.LearningExtractionView{}, false, nil
	}
	if err != nil {
		return core.LearningExtractionView{}, false, fmt.Errorf("学习抽取: 读取抽取行（%s）: %w", extractionID, err)
	}
	views, err := r.attachLearningChildren(ctx, []core.LearningExtractionView{{Extraction: row}})
	if err != nil {
		return core.LearningExtractionView{}, false, err
	}
	return views[0], true, nil
}

// attachLearningChildren 批量挂载候选与草案（一次 IN ANY 查询，不按行 N+1）。
// attachLearningChildren batches candidates and drafts for the given views.
func (r *LearningRepo) attachLearningChildren(ctx context.Context, views []core.LearningExtractionView) ([]core.LearningExtractionView, error) {
	if len(views) == 0 {
		return views, nil
	}
	ids := make([]string, 0, len(views))
	for _, view := range views {
		ids = append(ids, view.Extraction.ExtractionID)
	}
	candidates, err := r.candidatesByExtraction(ctx, ids)
	if err != nil {
		return nil, err
	}
	drafts, err := r.draftsByExtraction(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range views {
		id := views[i].Extraction.ExtractionID
		views[i].Candidates = candidates[id]
		views[i].Drafts = drafts[id]
	}
	return views, nil
}

// candidatesByExtraction 读取候选投影（JOIN 条目表取状态与正文）。证据 JSON 解不开时
// 上抛而不是返回空证据：证据是候选可复核的载体，静默丢证据会让页面看起来"没有证据"。
// candidatesByExtraction loads candidate projections; broken evidence JSON fails the read.
func (r *LearningRepo) candidatesByExtraction(ctx context.Context, extractionIDs []string) (map[string][]core.LearningCandidateView, error) {
	out := make(map[string][]core.LearningCandidateView, len(extractionIDs))
	if len(extractionIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.pool.QueryContext(ctx,
		`SELECT cm.extraction_id, m.id, m.state, m.content, cm.kind, cm.certainty, cm.confidence,
		        cm.evidence_json, cm.source_entry_ids, cm.suggested_supersedes_id,
		        cm.origin_caller_id, cm.origin_key_id, cm.evidence_at, cm.valid_to,
		        cm.time_confidence, cm.created_at
		   FROM memory_candidate_meta cm
		   JOIN memory_entries m ON m.id = cm.entry_id
		  WHERE cm.extraction_id = ANY($1)
		  ORDER BY cm.entry_id`, extractionIDs)
	if err != nil {
		return nil, fmt.Errorf("学习抽取: 读取候选投影: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			extractionID       string
			view               core.LearningCandidateView
			evidenceRaw        []byte
			sourceEntryIDs     []int64
			suggested, valid   sql.NullInt64
			evidenceAt, madeAt int64
		)
		if err := rows.Scan(&extractionID, &view.EntryID, &view.State, &view.Content, &view.Kind,
			&view.Certainty, &view.Confidence, &evidenceRaw, &sourceEntryIDs, &suggested,
			&view.OriginCallerID, &view.OriginKeyID, &evidenceAt, &valid,
			&view.TimeConfidence, &madeAt); err != nil {
			return nil, fmt.Errorf("学习抽取: 扫描候选投影: %w", err)
		}
		evidence, err := unmarshalLearningEvidence(evidenceRaw)
		if err != nil {
			return nil, fmt.Errorf("学习抽取: 解析候选证据（entry=%d）: %w", view.EntryID, err)
		}
		view.Evidence = evidence
		view.SourceEntryIDs = sourceEntryIDs
		if suggested.Valid {
			view.SuggestedSupersedesID = suggested.Int64
		}
		view.EvidenceAt = learningTimeFromMS(evidenceAt)
		view.ValidTo = learningTimeFromNull(valid)
		view.CreatedAt = learningTimeFromMS(madeAt)
		out[extractionID] = append(out[extractionID], view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("学习抽取: 遍历候选投影: %w", err)
	}
	return out, nil
}

// ListDrafts 读取草案元数据；Content 留空，由 app 从 DraftRoot 下的 DRAFT.md 填充
// （文件是正文的唯一权威载体，DB 只存哈希）。
// ListDrafts reads draft metadata; Content stays empty and is filled from DRAFT.md by app.
func (r *LearningRepo) ListDrafts(ctx context.Context, filter core.LearningDraftFilter) ([]core.LearningDraftView, error) {
	query := `SELECT draft_id, extraction_id, agent_name, name, content_sha256, evidence_json,
	                 rel_dir, created_at
	            FROM memory_method_drafts`
	var args []any
	if filter.Agent != "" {
		args = append(args, filter.Agent)
		query += ` WHERE agent_name = $1`
	}
	args = append(args, learningListLimit(filter.Limit))
	// #nosec G202 -- 同 ListExtractions：WHERE 片段是固定模板且过滤值走位置参数，
	// LIMIT 占位序号由 len(args) 派生，SQL 文本不含任何外部片段。
	// Same justification as ListExtractions: fixed fragments, bound values.
	query += fmt.Sprintf(` ORDER BY created_at DESC, draft_id DESC LIMIT $%d`, len(args))

	rows, err := r.db.pool.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("学习抽取: 列出草案: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []core.LearningDraftView
	for rows.Next() {
		view, err := scanLearningDraft(rows)
		if err != nil {
			return nil, fmt.Errorf("学习抽取: 扫描草案: %w", err)
		}
		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("学习抽取: 遍历草案: %w", err)
	}
	return out, nil
}

// LoadDraft 读取一份草案元数据；found=false 表示不存在（web 映射 404）。
// LoadDraft reads one draft's metadata; found separates absent from broken.
func (r *LearningRepo) LoadDraft(ctx context.Context, draftID string) (core.LearningDraftView, bool, error) {
	view, err := scanLearningDraft(r.db.pool.QueryRowContext(ctx,
		`SELECT draft_id, extraction_id, agent_name, name, content_sha256, evidence_json,
		        rel_dir, created_at
		   FROM memory_method_drafts WHERE draft_id = $1`, draftID))
	if errors.Is(err, sql.ErrNoRows) {
		return core.LearningDraftView{}, false, nil
	}
	if err != nil {
		return core.LearningDraftView{}, false, fmt.Errorf("学习抽取: 读取草案（%s）: %w", draftID, err)
	}
	return view, true, nil
}

// draftsByExtraction 批量读取草案投影。
// draftsByExtraction batches draft projections for the given extractions.
func (r *LearningRepo) draftsByExtraction(ctx context.Context, extractionIDs []string) (map[string][]core.LearningDraftView, error) {
	out := make(map[string][]core.LearningDraftView, len(extractionIDs))
	if len(extractionIDs) == 0 {
		return out, nil
	}
	rows, err := r.db.pool.QueryContext(ctx,
		`SELECT draft_id, extraction_id, agent_name, name, content_sha256, evidence_json,
		        rel_dir, created_at
		   FROM memory_method_drafts
		  WHERE extraction_id = ANY($1)
		  ORDER BY created_at, draft_id`, extractionIDs)
	if err != nil {
		return nil, fmt.Errorf("学习抽取: 读取草案投影: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		view, err := scanLearningDraft(rows)
		if err != nil {
			return nil, fmt.Errorf("学习抽取: 扫描草案投影: %w", err)
		}
		out[view.ExtractionID] = append(out[view.ExtractionID], view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("学习抽取: 遍历草案投影: %w", err)
	}
	return out, nil
}

// scanLearningDraft 投影一行草案元数据。
// scanLearningDraft maps one draft row.
func scanLearningDraft(scan learningScanner) (core.LearningDraftView, error) {
	var (
		view        core.LearningDraftView
		evidenceRaw []byte
		createdAt   int64
	)
	if err := scan.Scan(&view.DraftID, &view.ExtractionID, &view.Agent, &view.Name,
		&view.ContentSHA256, &evidenceRaw, &view.RelDir, &createdAt); err != nil {
		return core.LearningDraftView{}, err
	}
	evidence, err := unmarshalLearningEvidence(evidenceRaw)
	if err != nil {
		return core.LearningDraftView{}, fmt.Errorf("解析草案证据（%s）: %w", view.DraftID, err)
	}
	view.Evidence = evidence
	view.CreatedAt = learningTimeFromMS(createdAt)
	return view, nil
}

// LearningCounts 是状态页的三个计数：滚动窗口内的尝试数、pending、未查证 unknown。
// 三者共用一次查询——状态页每打开一次就查三次库既慢又可能读到不一致的中场状态。
// LearningCounts reads the status-page counters in one statement.
func (r *LearningRepo) LearningCounts(ctx context.Context, since time.Time) (memory.LearningCounts, error) {
	var (
		counts           memory.LearningCounts
		used             int64
		pending, unknown int
	)
	err := r.db.pool.QueryRowContext(ctx,
		`SELECT
		   COALESCE((SELECT SUM(attempts) FROM memory_extractions
		              WHERE last_attempt_at >= $1), 0),
		   (SELECT count(*) FROM memory_extractions WHERE status = 'pending'),
		   (SELECT count(*) FROM memory_extractions
		     WHERE status = 'unknown' AND verified_at IS NULL)`,
		since.UnixMilli()).Scan(&used, &pending, &unknown)
	if err != nil {
		return memory.LearningCounts{}, fmt.Errorf("学习抽取: 统计状态计数: %w", err)
	}
	counts.Attempts = int(used)
	counts.Pending = pending
	counts.Unknown = unknown
	return counts, nil
}

// LastExtractionAt 返回最近一次尝试时刻；从未抽取时 found=false。
// LastExtractionAt reports the latest attempt time, or found=false when none exists.
func (r *LearningRepo) LastExtractionAt(ctx context.Context) (time.Time, bool, error) {
	var ms sql.NullInt64
	if err := r.db.pool.QueryRowContext(ctx,
		`SELECT MAX(last_attempt_at) FROM memory_extractions`).Scan(&ms); err != nil {
		return time.Time{}, false, fmt.Errorf("学习抽取: 读取最近尝试时刻: %w", err)
	}
	if !ms.Valid {
		return time.Time{}, false, nil
	}
	return learningTimeFromMS(ms.Int64), true, nil
}

// learningEvidenceJSON 是 memory_candidate_meta.evidence_json 的固定形状，与草案
// evidence.json（internal/memory 的 draftEvidence）逐键同源：时刻用 RFC3339Nano（UTC）。
// learningEvidenceJSON mirrors the draft evidence file key for key.
type learningEvidenceJSON struct {
	MessageID int64  `json:"message_id"`
	RunID     string `json:"run_id"`
	Role      string `json:"role"`
	Quote     string `json:"quote"`
	MessageAt string `json:"message_at"`
}

// marshalLearningEvidence 序列化证据数组；空证据写成 `[]` 而不是 `null`（jsonb NOT NULL，
// 读取侧也按数组解析）。
// marshalLearningEvidence always emits a JSON array.
func marshalLearningEvidence(evidence []core.LearningEvidence) ([]byte, error) {
	out := make([]learningEvidenceJSON, 0, len(evidence))
	for _, item := range evidence {
		out = append(out, learningEvidenceJSON{
			MessageID: item.MessageID,
			RunID:     item.RunID,
			Role:      item.Role,
			Quote:     item.Quote,
			MessageAt: item.MessageAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return json.Marshal(out)
}

// unmarshalLearningEvidence 反序列化证据；时刻解不开即上抛（证据是复核载体，不静默降级）。
// unmarshalLearningEvidence refuses to degrade broken evidence silently.
func unmarshalLearningEvidence(raw []byte) ([]core.LearningEvidence, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var decoded []learningEvidenceJSON
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	out := make([]core.LearningEvidence, 0, len(decoded))
	for _, item := range decoded {
		at := time.Time{}
		if item.MessageAt != "" {
			parsed, err := time.Parse(time.RFC3339Nano, item.MessageAt)
			if err != nil {
				return nil, fmt.Errorf("证据时刻非法（message_id=%d）: %w", item.MessageID, err)
			}
			at = parsed.UTC()
		}
		out = append(out, core.LearningEvidence{
			MessageID: item.MessageID,
			RunID:     item.RunID,
			Role:      item.Role,
			Quote:     item.Quote,
			MessageAt: at,
		})
	}
	return out, nil
}

// dedupeLearningIDs 保序去重来源主键；0 与负数不是合法条目主键，一并剔除。
// dedupeLearningIDs removes duplicates and non-positive ids while preserving order.
func dedupeLearningIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// learningStateAllowed 判定状态闭集（与 memory_extractions.status 的 CHECK 同源）。
// learningStateAllowed reports whether the state is in the contract closed set.
func learningStateAllowed(state core.LearningExtractionState) bool {
	switch state {
	case core.LearningExtractionPending, core.LearningExtractionSuccess,
		core.LearningExtractionFailed, core.LearningExtractionSkipped,
		core.LearningExtractionUnknown:
		return true
	default:
		return false
	}
}

// learningFailureClassAllowed 判定失败分类闭集（与 DDL CHECK 同源）。
// learningFailureClassAllowed reports whether the class is in the contract closed set.
func learningFailureClassAllowed(class core.LearningFailureClass) bool {
	switch class {
	case core.LearningFailureModel, core.LearningFailureInvalidOutput,
		core.LearningFailureTimeout, core.LearningFailureAudit,
		core.LearningFailureUnavailable, core.LearningFailureWindowGone:
		return true
	default:
		return false
	}
}

// learningListLimit 套用列表缺省值与上限（§3.1.10：1–200，缺省 50）。
// learningListLimit applies the 1–200 bound with a 50 default.
func learningListLimit(limit int) int {
	if limit <= 0 {
		return learningDefaultListLimit
	}
	if limit > learningMaxListLimit {
		return learningMaxListLimit
	}
	return limit
}

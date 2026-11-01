package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `compaction_entries` 的**唯一 SQL 入口**（设计 §3.1.11 / T088）。
//
// 三条纪律：
//  1. **scope 身份 = `Scope.Canonical()`**：条目里的 `TriggerRunID` 只是"哪次 Run 触发的"
//     注记，会话范围的条目可以跨多个 Run；按 Run 匹配会把同一会话切成互不相见的碎片
//     （R101 实测确立）。会话范围写 `scope_session_id`、无状态范围写 `scope_run_id`，
//     两者互斥（DDL `num_nonnulls = 1`）。
//  2. **`Append` 自持事务**：事务第一条语句取 scope 级 advisory 事务锁，锁内**按链资格**
//     选有效头并校验 `covered_from` / `parent` / `chain_epoch`，不满足**不插入**；
//     覆盖范围唯一索引只是最后防线（多列唯一键只排斥完全相等的切点，不承担收敛）。
//  3. **结局按三态映射**：提交未知必须经 `VerifyTx` 定结局，只有 `TxVerifyAbsent` 与
//     锁等待超时才是 `AppendRetryable`；`Pending` / `Inconsistent` 冻结前滚并把原始原因
//     记进 WARN 日志（结局本身就是信号，**不得**再包成普通错误让引擎重试）。
//
// This file is the only SQL entry point for compaction_entries: the scope identity is
// Scope.Canonical() (never the trigger run), Append owns its transaction and the
// scope-level advisory lock, and an unknown commit is resolved through VerifyTx.

// 压缩条目的 SQL 常量。列清单只写一份：插入、按 scope 读、按 attempt_id 回读共用它，
// 两处各写一份同义 SQL 就是两处漂移点。
// The column list has exactly one copy, shared by insert and every read path.
const (
	compactionColumns = `compaction_id, scope_session_id, scope_run_id, chain_epoch,
		parent_compaction_id, trigger_run_id, attempt_id,
		covered_from_message_id, cut_message_id, first_kept_message_id,
		strategy_version, prompt_version, model,
		prompt_tokens, completion_tokens, tokens_before, tokens_after,
		status, reason_code, source_refs, included_refs, summary, created_at, stale_at`

	compactionSelect = `SELECT ` + compactionColumns + ` FROM compaction_entries`

	// 按 scope 读：会话范围 `scope_session_id = $1`、无状态范围 `scope_run_id = $2`，
	// 另一边传 NULL 时整行比较为 NULL（不命中），因此一条常量覆盖两种范围。
	// 次序由 `(created_at, compaction_id)` 固定——链内次序由 Go 侧按父指针重排，
	// SQL 只保证同一份输入两次读取的次序一致。
	compactionByScope = compactionSelect + `
		WHERE scope_session_id = $1 OR scope_run_id = $2
		ORDER BY created_at, compaction_id`

	compactionByAttempt = compactionSelect + ` WHERE attempt_id = $1`

	compactionInsert = `INSERT INTO compaction_entries
		(compaction_id, scope_session_id, scope_run_id, chain_epoch, parent_compaction_id,
		 trigger_run_id, attempt_id, covered_from_message_id, cut_message_id, first_kept_message_id,
		 strategy_version, prompt_version, model, prompt_tokens, completion_tokens,
		 tokens_before, tokens_after, status, reason_code, source_refs, included_refs,
		 summary, created_at, stale_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17,
		        $18, $19, $20::jsonb, $21::jsonb, $22, $23, $24)`

	// 失效级联：从目标沿 `parent_compaction_id` 递归到**全部后代**，同一事务内一起置
	// `stale`。递归项按 scope 过滤是纵深防御——父指针本来就不会跨 scope，但锁是按
	// scope 取的，越界写会让"同一把锁内只动本 scope"这句承诺失效。
	// 已 `stale` 的行不重写：失效原因是审计事实，后续级联不得把它改成新祖先的编号。
	// The cascade walks every descendant in the same transaction; rows already stale are
	// left untouched so the recorded reason keeps pointing at the ancestor that failed first.
	compactionMarkStale = `WITH RECURSIVE stale_targets(compaction_id) AS (
			SELECT compaction_id FROM compaction_entries WHERE compaction_id = $1::text
			UNION ALL
			SELECT child.compaction_id
			  FROM compaction_entries child
			  JOIN stale_targets parent ON child.parent_compaction_id = parent.compaction_id
			 WHERE child.scope_session_id IS NOT DISTINCT FROM $4::text
			   AND child.scope_run_id IS NOT DISTINCT FROM $5::text
		)
		UPDATE compaction_entries
		   SET status = 'stale',
		       stale_at = COALESCE(stale_at, $2),
		       reason_code = CASE WHEN compaction_id = $1::text THEN $3::text
		                          ELSE 'ancestor_stale:' || $1::text END
		 WHERE compaction_id IN (SELECT compaction_id FROM stale_targets)
		   AND status <> 'stale'`
)

// PostgreSQL 错误码（用常量而不是散落的字面量，判据与日志共用同一份定义）。
// PostgreSQL error codes used by the append outcome mapping.
const (
	// pgCodeUniqueViolation 是唯一约束冲突：覆盖范围唯一索引的最后防线在此生效。
	pgCodeUniqueViolation = "23505"
	// pgCodeLockNotAvailable 是 `lock_timeout` 到期（`pg_advisory_xact_lock` 等待超时）。
	pgCodeLockNotAvailable = "55P03"
)

// errCompactionEntryMissing 表示按标识找不到条目（`MarkStale` 的目标）。
// 失效是"对既有条目"的动作，找不到即调用方用错了标识，**不得**静默成功。
// errCompactionEntryMissing reports a compaction id that has no row.
var errCompactionEntryMissing = errors.New("postgres: 压缩条目不存在")

// CompactionStore 是 `compaction.CompactionStore` 的 PostgreSQL 实现（设计 §3.1.11）。
// CompactionStore is the PostgreSQL implementation of the compaction port.
type CompactionStore struct {
	db     *DB
	logger *slog.Logger
}

// 编译期断言：实现满足领域包声明的存储端口。
// Compile-time assertion that the store satisfies the domain port.
var _ compaction.CompactionStore = (*CompactionStore)(nil)

// NewCompactionStore 绑定库句柄与日志目标；`logger` 为 nil 时退回 `slog.Default()`。
//
// 日志不是装饰：`AppendPending` / `AppendInconsistent` 这两种结局按协议**不返回错误**
// （结局本身就是信号，错误会让引擎把它当普通失败），原始提交错误只能经 WARN 日志
// 留痕——不记日志就是线上排障黑洞。`MarkStale` 的级联行数同样只在这里可见。
// A nil logger falls back to the default one: pending/inconsistent outcomes are reported
// through the result value, so the underlying commit error survives only in the log.
func NewCompactionStore(db *DB, logger *slog.Logger) *CompactionStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &CompactionStore{db: db, logger: logger}
}

// Append 在 scope 级 advisory 锁内追加一条 committed 条目（设计 §3.1.11 的 ①~⑤）。
//
// 锁内顺序是冻结的：定目标 epoch 并按链资格选头 → 同覆盖范围已 committed 则回读 →
// 头校验 → 通过才插入。任何一步不满足都**不写行**，由结局分类告诉引擎下一步。
// The locked sequence is frozen: pick the target epoch and the chain-qualified head,
// reuse an identical committed coverage, verify the head, and insert only when it holds.
func (s *CompactionStore) Append(ctx context.Context, entry compaction.CompactionEntry) (compaction.AppendResult, error) {
	norm, err := normalizeCompactionEntry(entry, s.db.clock())
	if err != nil {
		return compaction.AppendResult{Entry: entry}, err
	}

	var (
		result         compaction.AppendResult
		insertConflict bool
	)
	outcome, txErr := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 事务**第一条语句**取 scope 锁：锁随提交 / 回滚自动释放，跨连接与跨进程有效。
		// 锁内重读状态是唯一收敛路径；覆盖范围唯一索引只在"完全相等"时兜底。
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, compaction.ScopeAdvisoryKey(norm.Scope)); err != nil {
			return fmt.Errorf("压缩追加取 scope 锁（scope=%s）: %w", norm.Scope.Canonical(), err)
		}

		rows, err := loadCompactionRows(ctx, tx, norm.Scope)
		if err != nil {
			return err
		}
		head, hasHead, targetEpoch, reason := selectCompactionHead(rows)

		// ② 同覆盖范围已有**链资格通过**的 committed 条目（限目标 epoch）→ 直接回读。
		// 崩溃恢复与并发重提都走这一支：条目已在，摘要不必再生成第二次。
		if existing, ok := findCommittedCoverage(rows, targetEpoch, norm); ok {
			result = compaction.AppendResult{Entry: existing, Outcome: compaction.AppendCommitted}
			return nil
		}

		// ③④ 头校验：有头看"接在头上"，无头看"是不是换代重建根"。不符即不插入。
		if hasHead {
			if norm.ChainEpoch != head.ChainEpoch ||
				norm.ParentCompactionID != head.CompactionID ||
				norm.CoveredFromMessageID != head.FirstKeptMessageID {
				headCopy := head
				result = compaction.AppendResult{Entry: norm, Outcome: compaction.AppendSuperseded, Head: &headCopy}
				return nil
			}
		} else {
			if norm.ParentCompactionID != "" || norm.ChainEpoch != targetEpoch {
				result = compaction.AppendResult{Entry: norm, Outcome: compaction.AppendRebuildRoot, ReasonCode: reason}
				if norm.ChainEpoch != targetEpoch {
					// epoch 对不上是"引擎按旧代次重建"：换代信号优先于链形状原因。
					result.ReasonCode = compaction.ReasonEpochRaced
				}
				return nil
			}
		}

		// ⑤ 通过才插入。
		if err := insertCompactionEntry(ctx, tx, norm); err != nil {
			if pgCodeOf(err) == pgCodeUniqueViolation {
				insertConflict = true
			}
			return err
		}
		result = compaction.AppendResult{Entry: norm, Outcome: compaction.AppendCommitted}
		return nil
	})

	return s.mapAppendOutcome(ctx, norm, result, outcome, txErr, insertConflict)
}

// mapAppendOutcome 把提交三态与事务错误翻译成六值结局（C-34 / 设计 §3.1.11）。
//
// 只有两种情形是 `AppendRetryable`：锁等待超时（可证明未写）与 `TxVerifyAbsent`
// （事务已回滚且业务键查无行）。`TxVerifyPending` / `TxVerifyInconsistent` 一律
// 冻结前滚，**不重试**——把"读不到结局"降级成"再试一次"正是这条协议禁止的动作。
// The mapping is the single place where a tri-state commit becomes one of the six
// outcomes; only a lock timeout or TxVerifyAbsent may be retried.
func (s *CompactionStore) mapAppendOutcome(
	ctx context.Context, entry compaction.CompactionEntry, result compaction.AppendResult,
	outcome TxOutcome, txErr error, insertConflict bool,
) (compaction.AppendResult, error) {
	switch outcome {
	case TxCommitted:
		return result, nil

	case TxRolledBack:
		if pgCodeOf(txErr) == pgCodeLockNotAvailable {
			// 锁等待超时：事务已回滚、可证明未写。引擎换 attempt_id 重提同一条摘要。
			return compaction.AppendResult{
				Entry:      entry,
				Outcome:    compaction.AppendRetryable,
				ReasonCode: compaction.ReasonAppendLockTimeout,
			}, nil
		}
		if insertConflict {
			// 唯一索引是最后防线：并发插入"完全相等的覆盖范围"时回读并报已提交。
			if reused, ok, err := s.reuseAfterConflict(ctx, entry); err != nil {
				return compaction.AppendResult{Entry: entry}, err
			} else if ok {
				return compaction.AppendResult{Entry: reused, Outcome: compaction.AppendCommitted}, nil
			}
		}
		return compaction.AppendResult{Entry: entry}, txErr

	case TxUnknown:
		var attempt TxAttempt
		var unknown *TxUnknownError
		if errors.As(txErr, &unknown) {
			attempt = unknown.Attempt
		}
		if attempt.XID == 0 {
			// 没有事务身份就无法核查结局：按"读不到"处理，**不得**当未提交重放。
			s.logger.WarnContext(ctx, "压缩追加结局未知且无可核查的事务标识，冻结前滚",
				"compaction_id", entry.CompactionID, "scope", entry.Scope.Canonical(), "error", txErr)
			return compaction.AppendResult{Entry: entry, Outcome: compaction.AppendPending}, nil
		}
		verdict, verifyErr := s.db.VerifyTx(ctx, attempt, func(ctx context.Context, q Queryer) (bool, error) {
			return compactionAttemptPresent(ctx, q, entry.AttemptID)
		})
		switch verdict {
		case TxVerifyCommitted:
			// 已提交：回读条目（含库里的 created_at），引擎据此直接投影。
			read, err := s.entryByAttemptID(ctx, entry.AttemptID)
			if err != nil {
				s.logger.WarnContext(ctx, "压缩追加已提交但回读失败，冻结前滚",
					"compaction_id", entry.CompactionID, "attempt_id", entry.AttemptID, "error", err)
				return compaction.AppendResult{Entry: entry, Outcome: compaction.AppendInconsistent}, nil
			}
			return compaction.AppendResult{Entry: read, Outcome: compaction.AppendCommitted}, nil

		case TxVerifyAbsent:
			return compaction.AppendResult{Entry: entry, Outcome: compaction.AppendRetryable}, nil

		case TxVerifyInconsistent:
			s.logger.WarnContext(ctx, "压缩追加结局与业务行矛盾，冻结前滚",
				"compaction_id", entry.CompactionID, "attempt_id", entry.AttemptID, "verify_error", verifyErr)
			return compaction.AppendResult{Entry: entry, Outcome: compaction.AppendInconsistent}, nil

		default:
			s.logger.WarnContext(ctx, "压缩追加结局读不到，冻结前滚",
				"compaction_id", entry.CompactionID, "attempt_id", entry.AttemptID, "verify_error", verifyErr)
			return compaction.AppendResult{Entry: entry, Outcome: compaction.AppendPending}, nil
		}
	}
	// 三态之外没有别的结局；走到这里说明协议被绕过，如实报错而不是猜一个。
	return compaction.AppendResult{Entry: entry}, fmt.Errorf("%w: 未知的提交结局 %q", ErrTxOutcomeUnknown, outcome)
}

// FindCommitted 按覆盖范围找**链资格通过**的 committed 条目（只读、不取锁）。
//
// 只认当前 `chain_epoch`：换代之后同覆盖范围的旧条目属于上一代，拿它复用等于把
// 换代结论推翻；祖先失效 / 断链的条目按**未命中**处理（渲染不出来就当没有）。
// Only the current epoch is eligible: reusing an older generation's entry would overrule
// the rebuild, and an unqualified ancestor makes the entry a miss rather than a hit.
func (s *CompactionStore) FindCommitted(ctx context.Context, scope compaction.Scope, coveredFromMessageID, cutMessageID string) (compaction.CompactionEntry, bool, error) {
	if !scope.Valid() {
		return compaction.CompactionEntry{}, false, errors.New("查已提交压缩条目: 空 scope——会话与 Run 标识不得同时为空")
	}
	rows, err := loadCompactionRows(ctx, s.db.pool, scope)
	if err != nil {
		return compaction.CompactionEntry{}, false, err
	}
	epoch := maxCompactionEpoch(rows)
	hit, ok := findCommittedCoverage(rows, epoch, compaction.CompactionEntry{
		Scope:                scope,
		ChainEpoch:           epoch,
		CoveredFromMessageID: coveredFromMessageID,
		CutMessageID:         cutMessageID,
	})
	if !ok {
		return compaction.CompactionEntry{}, false, nil
	}
	return hit, true, nil
}

// LatestCommittedChain 返回**当前 `chain_epoch`** 的 committed 条目，按父到子排列。
//
// "当前 `chain_epoch`"取该 scope 全部行（含 stale / failed）的最大代次：代次一经
// 创建就只增不减，读取侧不得因为某一代已无 committed 行就退回上一代——那会让下一次
// `Append` 在旧代次上继续追加，推翻换代结论。
// 链资格由调用方判定（`compaction.ValidateChain`）：这里只保证**顺序正确**，不筛掉
// 断链行——隐去断点会让"断链"看起来像"短链"，调用方就失去了重建的依据。
// The current epoch is the maximum over every row, stale ones included: generations only
// move forward, and the caller needs to see broken links rather than a silently short chain.
func (s *CompactionStore) LatestCommittedChain(ctx context.Context, scope compaction.Scope) ([]compaction.CompactionEntry, error) {
	if !scope.Valid() {
		return nil, errors.New("读压缩链: 空 scope——会话与 Run 标识不得同时为空")
	}
	rows, err := loadCompactionRows(ctx, s.db.pool, scope)
	if err != nil {
		return nil, err
	}
	epoch := maxCompactionEpoch(rows)
	if epoch == 0 {
		return nil, nil
	}
	committed := make([]compaction.CompactionEntry, 0, len(rows))
	for _, row := range rows {
		if row.ChainEpoch == epoch && row.Status == compaction.StatusCommitted {
			committed = append(committed, row)
		}
	}
	return orderCompactionChain(committed), nil
}

// ListCurrentChain 返回**当前 `chain_epoch`** 的全部条目（committed / failed / stale
// 都算），按契约 `GET /compactions` 的次序 `(chain_epoch, created_at)` 升序，并截断到
// `limit` 条——这是管理台「压缩记录」面板的唯一读法（T089）。
//
// 与 `LatestCommittedChain` 的两处差异都是**为读面服务**，不是放宽：
//  1. 只回 committed 会让「压缩失败」在管理台变成"从来没压缩过"——失败恰恰是最需要
//     显示降级原因的状态，因此 failed / stale 一并返回（契约要求带 `reason_code`）。
//  2. 排序按创建时间而不是父→子：契约钉的是 `(chain_epoch, created_at)`，且链资格
//     由 `ValidateChain` 判定，读面**不**替调用方隐去断链 / 分叉（隐去会让断链看起来
//     像短链）。同类条目跨代次不会混入：只取最大代次。
//  3. 截断取**最前** `limit` 条（与 SQL `LIMIT` 在同一次序下的语义一致）；超出部分
//     不另设游标——契约没有分页参数，本端点是有界只读视图。
//
// The management read of the current generation: every status, contract ordering,
// first-N truncation. Dropping failed rows would render a failed compression as "never
// compacted", and hiding broken links would make a break look like a short chain.
func (s *CompactionStore) ListCurrentChain(ctx context.Context, scope compaction.Scope, limit int) ([]compaction.CompactionEntry, error) {
	if !scope.Valid() {
		return nil, errors.New("读压缩链视图: 空 scope——会话与 Run 标识不得同时为空")
	}
	if limit < 1 {
		return nil, fmt.Errorf("读压缩链视图: limit 必须 ≥ 1（当前 %d）", limit)
	}
	rows, err := loadCompactionRows(ctx, s.db.pool, scope)
	if err != nil {
		return nil, err
	}
	epoch := maxCompactionEpoch(rows)
	if epoch == 0 {
		return nil, nil
	}
	current := make([]compaction.CompactionEntry, 0, len(rows))
	for _, row := range rows {
		if row.ChainEpoch == epoch {
			current = append(current, row)
		}
	}
	sortCompactionRows(current)
	if len(current) > limit {
		current = current[:limit]
	}
	return current, nil
}

// MarkStale 取**同一把** scope 锁，把目标及其全部后代置 `stale`（C-36 的失效级联）。
//
// 目标记调用方给的原因（`revoked` / `deleted` / ...），后代记
// `ancestor_stale:<目标 id>`——失效沿链传播的路径因此可追溯。已 stale 的行不改写。
// The target keeps the caller's reason and every descendant records the ancestor link,
// so the propagation path stays auditable; already-stale rows are never rewritten.
func (s *CompactionStore) MarkStale(ctx context.Context, compactionID, reason string) error {
	if compactionID == "" {
		return errors.New("压缩条目失效: 空条目标识")
	}
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 先读目标的 scope：锁键由它派生，读不到就没有可锁的对象（条目只追加、scope 不变，
		// 因此在取锁前读是安全的）。
		scope, err := readCompactionScope(ctx, tx, compactionID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, compaction.ScopeAdvisoryKey(scope)); err != nil {
			return fmt.Errorf("压缩失效取 scope 锁（scope=%s）: %w", scope.Canonical(), err)
		}
		res, err := tx.ExecContext(ctx, compactionMarkStale,
			compactionID, s.db.clock().UTC().UnixMilli(), nullIfEmpty(reason),
			nullIfEmpty(scope.SessionID), nullIfEmpty(scope.RunID))
		if err != nil {
			return fmt.Errorf("压缩失效级联（compaction_id=%s）: %w", compactionID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认压缩失效写入（compaction_id=%s）: %w", compactionID, err)
		}
		// 受影响行数进日志：级联了几个后代是排查"整链作废还是中段回退"的第一手读数。
		s.logger.InfoContext(ctx, "压缩条目失效级联",
			"compaction_id", compactionID, "scope", scope.Canonical(),
			"reason", reason, "affected_rows", affected)
		return nil
	})
}

// normalizeCompactionEntry 校验并补齐待写条目：状态只接受 `committed`（空值即它），
// 时间缺失时取句柄时钟。
//
// 只接受 committed 是失败关闭：`failed` / `stale` 不经这条链追加路径（stale 由
// `MarkStale` 产生、failed 只作审计留痕），放行会把非链条目混进"唯一合格叶"的判定。
// Only committed entries travel this path; the other statuses would pollute the head scan.
func normalizeCompactionEntry(entry compaction.CompactionEntry, now time.Time) (compaction.CompactionEntry, error) {
	if entry.Status == "" {
		entry.Status = compaction.StatusCommitted
	}
	if entry.Status != compaction.StatusCommitted {
		return compaction.CompactionEntry{}, fmt.Errorf(
			"压缩追加: 只接受 committed 条目（收到 %q）——failed / stale 不经链追加路径", entry.Status)
	}
	if !entry.Valid() {
		return compaction.CompactionEntry{}, fmt.Errorf(
			"压缩追加: 条目形状不完整（compaction_id=%q, scope=%q, covered_from=%q, cut=%q, first_kept=%q, epoch=%d）",
			entry.CompactionID, entry.Scope.Canonical(), entry.CoveredFromMessageID,
			entry.CutMessageID, entry.FirstKeptMessageID, entry.ChainEpoch)
	}
	if entry.Scope.SessionID != "" && entry.Scope.RunID != "" {
		return compaction.CompactionEntry{}, fmt.Errorf(
			"压缩追加: scope 同时给了会话（%s）与 Run（%s）标识，两者互斥",
			entry.Scope.SessionID, entry.Scope.RunID)
	}
	if entry.AttemptID == "" {
		return compaction.CompactionEntry{}, errors.New(
			"压缩追加: 缺少 attempt_id——重提去重与未知结局核查都以它为业务键")
	}
	if entry.TriggerRunID == "" {
		return compaction.CompactionEntry{}, errors.New(
			"压缩追加: 缺少触发 Run 标识（trigger_run_id NOT NULL）")
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	entry.CreatedAt = entry.CreatedAt.UTC()
	return entry, nil
}

// selectCompactionHead 在同一 epoch 内**按链资格**选唯一有效头（无锁外的纯函数）。
//
// 链资格：从叶沿 `parent_compaction_id` 回溯，逐段 committed 且
// `child.covered_from = parent.first_kept`，根无父节点；同一 epoch 内出现多根、多叶
// （分叉）或无法从唯一根到达的 committed 行，都判**断链**——此时不选头，由调用方
// 以 `targetEpoch = max+1` 走换代重建，**不得**任选一个叶继续。
// 返回值第三个是判定出的原因：`no_committed_head` / `stale_ancestor` / `chain_broken`
// （有头时为空串），供 `AppendRebuildRoot` 的 `reason_code` 使用。
// The head is the unique qualified leaf of the newest epoch; forks, orphan committed rows
// and a missing root all mean "no head" rather than "pick the newest committed row".
func selectCompactionHead(rows []compaction.CompactionEntry) (compaction.CompactionEntry, bool, int, string) {
	epoch := maxCompactionEpoch(rows)
	if epoch == 0 {
		// 该 scope 还没有任何条目：首份条目就是 epoch 1 的根。
		return compaction.CompactionEntry{}, false, 1, compaction.ReasonNoCommittedHead
	}

	committed := make([]compaction.CompactionEntry, 0, len(rows))
	sawStale := false
	for _, row := range rows {
		if row.ChainEpoch != epoch {
			continue
		}
		switch row.Status {
		case compaction.StatusCommitted:
			committed = append(committed, row)
		case compaction.StatusStale:
			sawStale = true
		}
	}
	if len(committed) == 0 {
		// 该代次已无 committed 行：祖先失效 ⇒ 换代重建；从未提交过 ⇒ 首份即根。
		reason := compaction.ReasonNoCommittedHead
		if sawStale {
			reason = compaction.ReasonStaleAncestor
		}
		return compaction.CompactionEntry{}, false, epoch + 1, reason
	}

	byID := make(map[string]compaction.CompactionEntry, len(committed))
	for _, row := range committed {
		byID[row.CompactionID] = row
	}
	children := make(map[string][]compaction.CompactionEntry, len(committed))
	roots := make([]compaction.CompactionEntry, 0, 1)
	for _, row := range committed {
		if row.ParentCompactionID == "" {
			roots = append(roots, row)
			continue
		}
		parent, ok := byID[row.ParentCompactionID]
		if !ok {
			// 父节点不在本代次的 committed 集合里（祖先被标 stale / 跨代引用）：
			// 这条是遗留的 committed 后代，**不得**被选为头。
			return compaction.CompactionEntry{}, false, epoch + 1, compaction.ReasonChainBroken
		}
		if row.CoveredFromMessageID != parent.FirstKeptMessageID {
			// 覆盖起点与父的保留起点不衔接：切点被改过，链断了。
			return compaction.CompactionEntry{}, false, epoch + 1, compaction.ReasonChainBroken
		}
		children[row.ParentCompactionID] = append(children[row.ParentCompactionID], row)
	}
	if len(roots) != 1 {
		// 零根（成环 / 全被引用）或多根（重叠根）：都无唯一有效头。
		return compaction.CompactionEntry{}, false, epoch + 1, compaction.ReasonChainBroken
	}

	cur := roots[0]
	visited := make(map[string]struct{}, len(committed))
	visited[cur.CompactionID] = struct{}{}
	for {
		kids := children[cur.CompactionID]
		if len(kids) == 0 {
			if len(visited) != len(committed) {
				// 根到叶之外还有 committed 行：分叉或孤儿，链不唯一。
				return compaction.CompactionEntry{}, false, epoch + 1, compaction.ReasonChainBroken
			}
			return cur, true, epoch, ""
		}
		if len(kids) > 1 {
			return compaction.CompactionEntry{}, false, epoch + 1, compaction.ReasonChainBroken
		}
		cur = kids[0]
		if _, seen := visited[cur.CompactionID]; seen {
			return compaction.CompactionEntry{}, false, epoch + 1, compaction.ReasonChainBroken
		}
		visited[cur.CompactionID] = struct{}{}
	}
}

// findCommittedCoverage 在指定代次里找**完全同覆盖范围**且链资格通过的 committed 条目。
// Coverage reuse only counts inside one epoch and only when the ancestor links all hold.
func findCommittedCoverage(rows []compaction.CompactionEntry, epoch int, want compaction.CompactionEntry) (compaction.CompactionEntry, bool) {
	for _, row := range rows {
		if row.Status != compaction.StatusCommitted || row.ChainEpoch != epoch {
			continue
		}
		if row.CoveredFromMessageID != want.CoveredFromMessageID || row.CutMessageID != want.CutMessageID {
			continue
		}
		if !compactionChainQualified(rows, row.CompactionID) {
			continue
		}
		return row, true
	}
	return compaction.CompactionEntry{}, false
}

// compactionChainQualified 沿父指针回溯，逐段校验 committed + 覆盖起点衔接（链资格）。
// Walking the parent links is the same rule the head scan applies, applied to one entry.
func compactionChainQualified(rows []compaction.CompactionEntry, compactionID string) bool {
	byID := make(map[string]compaction.CompactionEntry, len(rows))
	for _, row := range rows {
		byID[row.CompactionID] = row
	}
	cur, ok := byID[compactionID]
	if !ok {
		return false
	}
	seen := make(map[string]struct{}, len(rows))
	for {
		if cur.Status != compaction.StatusCommitted {
			return false
		}
		if _, dup := seen[cur.CompactionID]; dup {
			// 成环：父指针被写坏，任何一侧都不得当选。
			return false
		}
		seen[cur.CompactionID] = struct{}{}
		if cur.ParentCompactionID == "" {
			return true
		}
		parent, ok := byID[cur.ParentCompactionID]
		if !ok {
			return false
		}
		if cur.CoveredFromMessageID != parent.FirstKeptMessageID {
			return false
		}
		cur = parent
	}
}

// orderCompactionChain 把同代次的 committed 行按父到子排成链（稳定次序）。
//
// 从（唯一可能的）根出发沿子节点下潜；同层出现多个子节点时按 `(created_at, id)` 依次
// 展开——分叉本来就该由 `ValidateChain` 判不合格，这里只保证"同一份输入两次读取给出
// 同一个次序"。没有被任何根到达的行（孤儿 / 环）按同样的稳定次序追加在末尾：**不隐藏**
// 它们，否则断链会被读成短链。
// Rows are ordered root-first with a deterministic tie-break; unreachable rows trail the
// chain instead of being dropped, so a fork still reads as a fork.
func orderCompactionChain(rows []compaction.CompactionEntry) []compaction.CompactionEntry {
	if len(rows) == 0 {
		return nil
	}
	byID := make(map[string]compaction.CompactionEntry, len(rows))
	children := make(map[string][]compaction.CompactionEntry, len(rows))
	for _, row := range rows {
		byID[row.CompactionID] = row
	}
	for _, row := range rows {
		if row.ParentCompactionID == "" {
			continue
		}
		if _, ok := byID[row.ParentCompactionID]; ok {
			children[row.ParentCompactionID] = append(children[row.ParentCompactionID], row)
		}
	}

	ordered := make([]compaction.CompactionEntry, 0, len(rows))
	visited := make(map[string]struct{}, len(rows))
	walk := func(start compaction.CompactionEntry) {
		stack := []compaction.CompactionEntry{start}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if _, seen := visited[cur.CompactionID]; seen {
				continue
			}
			visited[cur.CompactionID] = struct{}{}
			ordered = append(ordered, cur)
			kids := append([]compaction.CompactionEntry(nil), children[cur.CompactionID]...)
			sortCompactionRows(kids)
			// 逆序入栈，出栈即正序。
			for i := len(kids) - 1; i >= 0; i-- {
				stack = append(stack, kids[i])
			}
		}
	}

	roots := make([]compaction.CompactionEntry, 0, 1)
	for _, row := range rows {
		if row.ParentCompactionID == "" {
			roots = append(roots, row)
		}
	}
	sortCompactionRows(roots)
	for _, root := range roots {
		walk(root)
	}
	rest := make([]compaction.CompactionEntry, 0, len(rows))
	for _, row := range rows {
		if _, seen := visited[row.CompactionID]; !seen {
			rest = append(rest, row)
		}
	}
	sortCompactionRows(rest)
	for _, row := range rest {
		walk(row)
	}
	return ordered
}

// sortCompactionRows 按 `(created_at, compaction_id)` 升序：时间相同时以标识兜底，
// 保证同一份输入的任何两次排序结果相同。
// sortCompactionRows gives a total order so repeated reads render identically.
func sortCompactionRows(rows []compaction.CompactionEntry) {
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.Before(rows[j].CreatedAt)
		}
		return rows[i].CompactionID < rows[j].CompactionID
	})
}

// maxCompactionEpoch 返回该 scope 全部行（含 stale / failed）的最大代次；空集为 0。
// The generation counter only moves forward, so the maximum counts every status.
func maxCompactionEpoch(rows []compaction.CompactionEntry) int {
	epoch := 0
	for _, row := range rows {
		if row.ChainEpoch > epoch {
			epoch = row.ChainEpoch
		}
	}
	return epoch
}

// loadCompactionRows 读一个 scope 的全部条目（**所有状态**：选头要靠 stale 行判断
// "是祖先失效还是从未提交"，读侧也要靠它定义当前代次）。
// Loading every status is deliberate: stale rows are what distinguish "ancestor went
// stale" from "never committed".
func loadCompactionRows(ctx context.Context, q Queryer, scope compaction.Scope) ([]compaction.CompactionEntry, error) {
	rows, err := q.QueryContext(ctx, compactionByScope, nullIfEmpty(scope.SessionID), nullIfEmpty(scope.RunID))
	if err != nil {
		return nil, fmt.Errorf("读压缩条目（scope=%s）: %w", scope.Canonical(), err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]compaction.CompactionEntry, 0, 8)
	for rows.Next() {
		entry, err := scanCompactionEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("读压缩条目（scope=%s）: %w", scope.Canonical(), err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读压缩条目（scope=%s）: %w", scope.Canonical(), err)
	}
	return out, nil
}

// entryByAttemptID 按幂等身份回读一条条目（`Append` 的未知结局核查与冲突回读共用）。
// attempt_id is the idempotency identity: the same key finds the row the commit wrote.
func (s *CompactionStore) entryByAttemptID(ctx context.Context, attemptID string) (compaction.CompactionEntry, error) {
	rows, err := s.db.pool.QueryContext(ctx, compactionByAttempt, attemptID)
	if err != nil {
		return compaction.CompactionEntry{}, fmt.Errorf("按 attempt_id 回读压缩条目（%s）: %w", attemptID, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return compaction.CompactionEntry{}, fmt.Errorf("按 attempt_id 回读压缩条目（%s）: %w", attemptID, err)
		}
		return compaction.CompactionEntry{}, fmt.Errorf("%w: attempt_id=%s", errCompactionEntryMissing, attemptID)
	}
	entry, err := scanCompactionEntry(rows)
	if err != nil {
		return compaction.CompactionEntry{}, fmt.Errorf("按 attempt_id 回读压缩条目（%s）: %w", attemptID, err)
	}
	return entry, nil
}

// reuseAfterConflict 是覆盖范围唯一索引触发后的回读：先按 attempt_id（同一次尝试被
// 重放）、再按完全相等的覆盖范围（并发插入同一切点）。
// 两者都查无行时返回 `ok=false`，由调用方把原始错误上抛——**不得**凭空造"已提交"。
// The index is the last line of defense; when neither key has a row the original error
// stands, because inventing a committed outcome would be worse than failing.
func (s *CompactionStore) reuseAfterConflict(ctx context.Context, entry compaction.CompactionEntry) (compaction.CompactionEntry, bool, error) {
	if byAttempt, err := s.entryByAttemptID(ctx, entry.AttemptID); err == nil {
		return byAttempt, true, nil
	} else if !errors.Is(err, errCompactionEntryMissing) {
		return compaction.CompactionEntry{}, false, err
	}

	rows, err := loadCompactionRows(ctx, s.db.pool, entry.Scope)
	if err != nil {
		return compaction.CompactionEntry{}, false, err
	}
	for _, row := range rows {
		if row.Status != compaction.StatusCommitted || row.ChainEpoch != entry.ChainEpoch {
			continue
		}
		if row.CoveredFromMessageID == entry.CoveredFromMessageID && row.CutMessageID == entry.CutMessageID {
			return row, true, nil
		}
	}
	return compaction.CompactionEntry{}, false, nil
}

// compactionAttemptPresent 是 `VerifyTx` 的业务键检查：这次尝试的条目是否真的在库里。
// The business-key check handed to VerifyTx; the row is the only proof of a commit.
func compactionAttemptPresent(ctx context.Context, q Queryer, attemptID string) (bool, error) {
	var present bool
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM compaction_entries WHERE attempt_id = $1)`, attemptID).Scan(&present); err != nil {
		return false, fmt.Errorf("核查压缩条目（attempt_id=%s）: %w", attemptID, err)
	}
	return present, nil
}

// readCompactionScope 读目标条目的 scope（锁键由它派生）。零 scope 即"该行不该存在"，
// 一并按找不到处理。
// The lock key comes from the target's scope, so it must be read before taking the lock.
func readCompactionScope(ctx context.Context, q Queryer, compactionID string) (compaction.Scope, error) {
	var sessionID, runID sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT scope_session_id, scope_run_id FROM compaction_entries WHERE compaction_id = $1`,
		compactionID).Scan(&sessionID, &runID)
	if errors.Is(err, sql.ErrNoRows) {
		return compaction.Scope{}, fmt.Errorf("%w: %s", errCompactionEntryMissing, compactionID)
	}
	if err != nil {
		return compaction.Scope{}, fmt.Errorf("读压缩条目 scope（%s）: %w", compactionID, err)
	}
	scope := compaction.Scope{SessionID: sessionID.String, RunID: runID.String}
	if !scope.Valid() {
		// DDL 的 `num_nonnulls(scope_session_id, scope_run_id) = 1` 保证不会出现；
		// 真出现说明库被绕过写入，锁键将无从派生——拒绝而不是猜一个。
		return compaction.Scope{}, fmt.Errorf("压缩条目 %s 的 scope 两列皆空（DDL 约束被绕过）", compactionID)
	}
	return scope, nil
}

// insertCompactionEntry 写一条条目；`stale_at` 只由 `MarkStale` 写，新条目恒为 NULL。
// New rows never carry stale_at: only the cascade sets it.
func insertCompactionEntry(ctx context.Context, ex sqlExecutor, entry compaction.CompactionEntry) error {
	sourceRefs, err := encodeCompactionRefs(entry.SourceRefs)
	if err != nil {
		return err
	}
	includedRefs, err := encodeCompactionRefs(entry.IncludedRefs)
	if err != nil {
		return err
	}
	_, err = ex.ExecContext(ctx, compactionInsert,
		entry.CompactionID,
		nullIfEmpty(entry.Scope.SessionID), nullIfEmpty(entry.Scope.RunID),
		entry.ChainEpoch, nullIfEmpty(entry.ParentCompactionID),
		entry.TriggerRunID, entry.AttemptID,
		entry.CoveredFromMessageID, entry.CutMessageID, entry.FirstKeptMessageID,
		entry.StrategyVersion, entry.PromptVersion, entry.Model,
		entry.Usage.PromptTokens, entry.Usage.CompletionTokens,
		entry.TokensBefore, entry.TokensAfter,
		entry.Status, nullIfEmpty(entry.ReasonCode),
		sourceRefs, includedRefs,
		nullIfEmpty(entry.Summary),
		entry.CreatedAt.UTC().UnixMilli(), nil,
	)
	if err != nil {
		return fmt.Errorf("写压缩条目（compaction_id=%s, scope=%s）: %w",
			entry.CompactionID, entry.Scope.Canonical(), err)
	}
	return nil
}

// scanCompactionEntry 把一行还原成条目。四列 bigint 直接扫进 `int`（驱动给的是 int64，
// `database/sql` 负责范围检查）；时间列是 BIGINT 毫秒，按 UTC 还原。
// The four bigint counters scan straight into int fields; timestamps are epoch millis.
func scanCompactionEntry(rows *sql.Rows) (compaction.CompactionEntry, error) {
	var (
		entry        compaction.CompactionEntry
		sessionID    sql.NullString
		runID        sql.NullString
		parent       sql.NullString
		reason       sql.NullString
		summary      sql.NullString
		sourceRefs   []byte
		includedRefs []byte
		createdAt    int64
		staleAt      sql.NullInt64
	)
	if err := rows.Scan(
		&entry.CompactionID, &sessionID, &runID, &entry.ChainEpoch,
		&parent, &entry.TriggerRunID, &entry.AttemptID,
		&entry.CoveredFromMessageID, &entry.CutMessageID, &entry.FirstKeptMessageID,
		&entry.StrategyVersion, &entry.PromptVersion, &entry.Model,
		&entry.Usage.PromptTokens, &entry.Usage.CompletionTokens,
		&entry.TokensBefore, &entry.TokensAfter,
		&entry.Status, &reason, &sourceRefs, &includedRefs, &summary, &createdAt, &staleAt,
	); err != nil {
		return compaction.CompactionEntry{}, err
	}
	entry.Scope = compaction.Scope{SessionID: sessionID.String, RunID: runID.String}
	entry.ParentCompactionID = parent.String
	entry.ReasonCode = reason.String
	entry.Summary = summary.String
	entry.CreatedAt = time.UnixMilli(createdAt).UTC()
	if staleAt.Valid {
		entry.StaleAt = time.UnixMilli(staleAt.Int64).UTC()
	}
	// `compaction_entries` 没有总数列：总数按两分量求和还原，不另立第三份真值。
	entry.Usage.TotalTokens = entry.Usage.PromptTokens + entry.Usage.CompletionTokens
	refs, err := decodeCompactionRefs(sourceRefs)
	if err != nil {
		return compaction.CompactionEntry{}, fmt.Errorf("解析 source_refs（compaction_id=%s）: %w", entry.CompactionID, err)
	}
	entry.SourceRefs = refs
	refs, err = decodeCompactionRefs(includedRefs)
	if err != nil {
		return compaction.CompactionEntry{}, fmt.Errorf("解析 included_refs（compaction_id=%s）: %w", entry.CompactionID, err)
	}
	entry.IncludedRefs = refs
	return entry, nil
}

// compactionRefJSON 是 `source_refs` / `included_refs` 两列的 JSON 元素形态：契约
// `EvidenceRef` 的三个字段之外多一个 `kind`（`message_evidence_refs.kind` 的同源字面量，
// 裁决记录见 specs/016 的 run-state R19）。
// The persisted element carries kind on top of the three contract fields.
type compactionRefJSON struct {
	Kind        string `json:"kind"`
	ID          string `json:"id"`
	VersionID   string `json:"version_id"`
	ContentHash string `json:"content_hash"`
}

// encodeCompactionRefs 把引用集合编成 jsonb 数组；空集合写 `[]`（列是 NOT NULL，
// 空数组比 NULL 更准确地表达"这份摘要没有来源引用"）。
// An empty set encodes as [] so "no references" never looks like "no value".
func encodeCompactionRefs(refs []core.EvidenceRef) (string, error) {
	out := make([]compactionRefJSON, 0, len(refs))
	for _, ref := range refs {
		out = append(out, compactionRefJSON{
			Kind:        ref.Kind,
			ID:          ref.ID,
			VersionID:   ref.VersionID,
			ContentHash: ref.ContentHash,
		})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("编码压缩来源引用: %w", err)
	}
	return string(raw), nil
}

// decodeCompactionRefs 解 jsonb 数组；`null` 按空集合处理（历史行可能写成 json null）。
// A JSON null reads as an empty set rather than an error.
func decodeCompactionRefs(raw []byte) ([]core.EvidenceRef, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var decoded []compactionRefJSON
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	if len(decoded) == 0 {
		return nil, nil
	}
	out := make([]core.EvidenceRef, 0, len(decoded))
	for _, ref := range decoded {
		out = append(out, core.EvidenceRef{
			Kind:        ref.Kind,
			ID:          ref.ID,
			VersionID:   ref.VersionID,
			ContentHash: ref.ContentHash,
		})
	}
	return out, nil
}

// pgCodeOf 取 PostgreSQL 错误码；非 PG 错误返回空串。
// 用 `errors.As` 而不是类型断言：调用方常见的是 `errors.Join` / `%w` 包装过的错误链。
// pgCodeOf digs the SQLSTATE out of a wrapped error chain; non-PG errors yield "".
func pgCodeOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

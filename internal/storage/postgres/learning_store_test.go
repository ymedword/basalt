//go:build integration

package postgres

// 本文件是 US-22 学习账本存储层的**有库**用例（T009）：判的是只有真库分得出的四件事——
//
//  1. **来源只从 runs 行复制**：`BeginExtraction` 里调用方填的 `origin_*` 必须被忽略
//     （C-07；跨来源不重贴是 B-07 的前置）；
//  2. **游标按（Agent、会话、来源调用方、来源 Key）推进**：同会话不同来源互不覆盖，
//     失败重试不改游标（FR-22-03 / FR-22-10）；
//  3. **`run_id` 唯一**：一次 Run 至多一行抽取账本，重复触发在库层被拒（FR-22-12）；
//  4. **冻结窗口不更新**：重试把尝试身份（Run / 时刻 / provider / model / hash / 版本）
//     换成本次 Run，但窗口主键与条数原样保留（FR-22-10 / C-08）。
//
// 逆改点：来源改读请求字段 → 用例 1 红；游标主键漏掉 origin_key_id → 用例 2 红；
// 去掉 DDL 的 UNIQUE(run_id) 或 BeginExtraction 先查后插不做唯一保护 → 用例 3 红；
// 重试 UPDATE 顺带写 window_from/to → 用例 4 红。
//
// Database-backed cases for the US-22 learning ledgers: origin comes from the runs row,
// cursors are per origin, run_id is unique, and a retry keeps the frozen window.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// lsBase 是夹具的时间基线。
// lsBase is the fixture's time origin.
var lsBase = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

// newLearningStoreDB 建隔离 schema，种一个 Web 会话；Run 与消息按用例追加。
// newLearningStoreDB opens an isolated schema with one web session seeded.
func newLearningStoreDB(t *testing.T) *DB {
	t.Helper()
	db := newTxTestDB(t)
	_, err := db.pool.ExecContext(context.Background(), `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count,
		 created_at, last_active_at, caller_id)
		VALUES ('ls-s1', 'demo', 'web', 'u1', 'active', 0, $1, $1, 'c1')`,
		lsBase.UnixMilli())
	require.NoError(t, err)
	return db
}

// lsSeedRun 种一条已完成 Run（来源恒为外部调用方 c1 的 key）。
// lsSeedRun inserts one completed run whose origin is caller c1 with the given key.
func lsSeedRun(t *testing.T, db *DB, runID, sessionID, keyID string, at time.Time) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `INSERT INTO runs
		(run_id, session_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, $2, 'demo', 'web', 'u1', 'user', 'completed', $3,
		        'c1', 'caller-key', $4, 'caller')`,
		runID, sessionID, at.UnixMilli(), keyID)
	require.NoError(t, err)
}

// lsRow 造一条待写抽取行；origin 字段刻意填假值，由用例断言库内实际值。
// lsRow builds an extraction row with deliberately spoofed origin fields.
func lsRow(extractionID, agent, sessionID, runID string, from, to int64, messages int) core.LearningExtraction {
	return core.LearningExtraction{
		ExtractionID:          extractionID,
		Agent:                 agent,
		SessionID:             sessionID,
		RunID:                 runID,
		LastAttemptRunID:      runID,
		OriginCallerID:        "spoofed-caller",
		OriginKeyID:           "spoofed-key",
		WindowFromMessageID:   from,
		WindowToMessageID:     to,
		WindowMessages:        messages,
		DroppedWindowMessages: 2,
		PriorMessages:         1,
		ExistingEntries:       3,
		PromptVersion:         "pv1",
		RuleVersion:           "rv1",
		Provider:              "prov-a",
		Model:                 "model-a",
		ConfigHash:            "hash-a",
		CreatedAt:             lsBase,
		LastAttemptAt:         lsBase,
	}
}

// TestUS22_Origin_FromRunRecord：抽取行与游标的来源都取自 runs 行，请求里填的来源被忽略；
// 同会话不同来源的未决行互不阻断（C-07 / B-07）。
// TestUS22_Origin_FromRunRecord pins origin copying from the runs row and per-origin
// unresolved isolation.
func TestUS22_Origin_FromRunRecord(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	lsSeedRun(t, db, "lr-r1", "ls-s1", "k1", lsBase)
	lsSeedRun(t, db, "lr-r2", "ls-s1", "k2", lsBase.Add(time.Minute))

	got, err := store.BeginExtraction(ctx, lsRow("ext-1", "demo", "ls-s1", "lr-r1", 1, 2, 2))
	require.NoError(t, err)
	require.Equal(t, "c1", got.OriginCallerID, "来源调用方必须复制自 runs，不得取请求字段")
	require.Equal(t, "k1", got.OriginKeyID, "来源 Key 必须复制自 runs，不得取请求字段")
	require.Equal(t, core.LearningExtractionPending, got.Status)
	require.Equal(t, 1, got.Attempts)

	// 另一个来源（k2）的 Run 不受 k1 的 pending 行阻断。
	other, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "lr-r2")
	require.NoError(t, err)
	require.Nil(t, other, "未决判定必须按来源隔离，k1 的 pending 不得阻断 k2")

	// k1 自己的新 Run 会被阻断（同来源同会话）。
	self, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "lr-r1")
	require.NoError(t, err)
	require.NotNil(t, self)
	require.Equal(t, "ext-1", self.ExtractionID)

	// 窗口关闭后游标落库：来源仍是 runs 行的值。
	require.NoError(t, store.FailExtraction(ctx, "ext-1", core.LearningFailureWindowGone, "gone", lsBase))
	cursor, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lr-r1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(2), cursor.CommittedMessageID)
	require.Equal(t, "c1", cursor.OriginCallerID)
	require.Equal(t, "k1", cursor.OriginKeyID)

	// k2 尚无游标：found=false，但来源仍按 runs 行给出。
	missing, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lr-r2")
	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, "c1", missing.OriginCallerID)
	require.Equal(t, "k2", missing.OriginKeyID)
	require.Zero(t, missing.CommittedMessageID)
}

// TestUS22_Cursor_PerOrigin：游标按（Agent、会话、来源调用方、来源 Key）各自推进——
// 成功推进到窗口末端、重试失败不动、不同来源互不覆盖（FR-22-03 / FR-22-10）。
// TestUS22_Cursor_PerOrigin pins per-origin cursor advancement and isolation.
func TestUS22_Cursor_PerOrigin(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	lsSeedRun(t, db, "lc-r1", "ls-s1", "k1", lsBase)
	lsSeedRun(t, db, "lc-r2", "ls-s1", "k1", lsBase.Add(time.Minute))
	lsSeedRun(t, db, "lc-r3", "ls-s1", "k2", lsBase.Add(2*time.Minute))

	// 第一次尝试：窗口关闭 → skipped 并推进游标到 5（attempts 不增）。
	_, err := store.BeginExtraction(ctx, lsRow("ext-a", "demo", "ls-s1", "lc-r1", 1, 5, 3))
	require.NoError(t, err)
	require.NoError(t, store.FailExtraction(ctx, "ext-a", core.LearningFailureWindowGone, "gone", lsBase))
	cursor, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lc-r1")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(5), cursor.CommittedMessageID)

	// 同来源重试：failed（可重试）不改游标。
	_, err = store.BeginExtraction(ctx, lsRow("ext-b", "demo", "ls-s1", "lc-r2", 6, 8, 2))
	require.NoError(t, err)
	require.NoError(t, store.FailExtraction(ctx, "ext-b", core.LearningFailureModel, "boom", lsBase.Add(time.Minute)))
	cursor, found, err = store.LoadCursor(ctx, "demo", "ls-s1", "lc-r2")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(5), cursor.CommittedMessageID, "可重试失败不得推进游标")

	// 另一个来源（k2）成功后推进到自己的窗口末端，不影响 k1。
	_, err = store.BeginExtraction(ctx, lsRow("ext-c", "demo", "ls-s1", "lc-r3", 9, 10, 1))
	require.NoError(t, err)
	_, err = store.CommitExtraction(ctx, core.CommitLearningExtraction{
		ExtractionID: "ext-c", ItemsTotal: 0, ItemsAccepted: 0, ItemsDropped: 0,
		CompletedAt: lsBase.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	k2, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lc-r3")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(10), k2.CommittedMessageID)
	require.Equal(t, "k2", k2.OriginKeyID)

	k1, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lc-r2")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(5), k1.CommittedMessageID, "k2 的推进不得覆盖 k1 的游标")

	// 同会话、不同 Agent：无游标（游标键含 Agent）。
	_, found, err = store.LoadCursor(ctx, "other-agent", "ls-s1", "lc-r1")
	require.NoError(t, err)
	require.False(t, found)
}

// TestUS22_Store_BeginExtractionUniqueRun：一次 Run 至多一行抽取账本——首行已终结后
// 同 Run 再次触发，INSERT 在库层被唯一约束拒绝，且不留下第二行（FR-22-12）。
// TestUS22_Store_BeginExtractionUniqueRun pins the UNIQUE(run_id) invariant.
func TestUS22_Store_BeginExtractionUniqueRun(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	lsSeedRun(t, db, "lu-r1", "ls-s1", "k1", lsBase)

	_, err := store.BeginExtraction(ctx, lsRow("ext-a", "demo", "ls-s1", "lu-r1", 1, 2, 2))
	require.NoError(t, err)
	// 终结首行（window_gone → skipped），让第二次触发走到 INSERT 而不是重试分支。
	require.NoError(t, store.FailExtraction(ctx, "ext-a", core.LearningFailureWindowGone, "gone", lsBase))

	_, err = store.BeginExtraction(ctx, lsRow("ext-b", "demo", "ls-s1", "lu-r1", 3, 4, 2))
	require.Error(t, err, "同一 run_id 不得写出第二行抽取账本")
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "唯一约束拒绝必须保留原始 PgError")
	require.Equal(t, "23505", pgErr.Code)

	var count int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_extractions WHERE run_id = 'lu-r1'`).Scan(&count))
	require.Equal(t, 1, count, "被拒的插入不得留下任何行")
}

// TestUS22_Store_RetryKeepsFrozenWindow：重试只换尝试身份，窗口主键 / 条数 / 前文 /
// 快照规模原样保留；本次 Run 的 provider / model / config / 版本号写入同一行（C-08）。
// TestUS22_Store_RetryKeepsFrozenWindow pins the frozen window and the refreshed
// attempt identity on retry.
func TestUS22_Store_RetryKeepsFrozenWindow(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	lsSeedRun(t, db, "lf-r1", "ls-s1", "k1", lsBase)
	retryAt := lsBase.Add(time.Hour)
	lsSeedRun(t, db, "lf-r2", "ls-s1", "k1", retryAt)

	first, err := store.BeginExtraction(ctx, lsRow("ext-f", "demo", "ls-s1", "lf-r1", 10, 20, 4))
	require.NoError(t, err)
	require.Equal(t, int64(10), first.WindowFromMessageID)
	require.Equal(t, int64(20), first.WindowToMessageID)
	require.NoError(t, store.FailExtraction(ctx, "ext-f", core.LearningFailureModel, "boom", lsBase))

	retry := lsRow("ext-g", "demo", "ls-s1", "lf-r2", 90, 99, 7)
	retry.LastAttemptAt = retryAt
	retry.CreatedAt = retryAt
	retry.Provider, retry.Model, retry.ConfigHash = "prov-b", "model-b", "hash-b"
	retry.PromptVersion, retry.RuleVersion = "pv2", "rv2"
	got, err := store.BeginExtraction(ctx, retry)
	require.NoError(t, err)

	require.Equal(t, "ext-f", got.ExtractionID, "重试复用同一行，不新建")
	require.Equal(t, 2, got.Attempts)
	require.Equal(t, int64(10), got.WindowFromMessageID, "重试不得改窗口左界")
	require.Equal(t, int64(20), got.WindowToMessageID, "重试不得改窗口右界")
	require.Equal(t, 4, got.WindowMessages, "重试不得改冻结条数")
	require.Equal(t, 1, got.PriorMessages)
	require.Equal(t, 3, got.ExistingEntries)
	require.Equal(t, "lf-r2", got.LastAttemptRunID, "尝试身份绑定本次 Trigger Run")
	require.Equal(t, retryAt.Unix(), got.LastAttemptAt.Unix())
	require.Equal(t, "prov-b", got.Provider)
	require.Equal(t, "model-b", got.Model)
	require.Equal(t, "hash-b", got.ConfigHash)
	require.Equal(t, "pv2", got.PromptVersion)
	require.Equal(t, "rv2", got.RuleVersion)
	require.Equal(t, core.LearningExtractionPending, got.Status)
	require.Equal(t, core.LearningFailureClass(""), got.FailureClass, "重试清空上次失败分类")
	require.True(t, got.FinishedAt.IsZero(), "重试中的行没有终态时刻")

	var count int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_extractions WHERE agent_name = 'demo'`).Scan(&count))
	require.Equal(t, 1, count)
}

// TestUS22_Store_RetryUpdatesAttemptIdentity：同来源三次尝试共用一行、身份逐次绑定本次
// Run；第 3 次失败按「三次失败转 skipped」终结并推进游标，之后不再阻断（FR-22-10）。
// TestUS22_Store_RetryUpdatesAttemptIdentity pins the three-attempt lifecycle: the same
// row carries each run's identity and the third failure terminates as skipped.
func TestUS22_Store_RetryUpdatesAttemptIdentity(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	at1 := lsBase
	at2 := lsBase.Add(10 * time.Minute)
	at3 := lsBase.Add(20 * time.Minute)
	lsSeedRun(t, db, "la-r1", "ls-s1", "k1", at1)
	lsSeedRun(t, db, "la-r2", "ls-s1", "k1", at2)
	lsSeedRun(t, db, "la-r3", "ls-s1", "k1", at3)

	first := lsRow("ext-a", "demo", "ls-s1", "la-r1", 10, 20, 4)
	first.LastAttemptAt, first.CreatedAt = at1, at1
	got, err := store.BeginExtraction(ctx, first)
	require.NoError(t, err)
	require.Equal(t, 1, got.Attempts)
	require.Equal(t, "la-r1", got.LastAttemptRunID)
	require.NoError(t, store.FailExtraction(ctx, "ext-a", core.LearningFailureModel, "boom", at1))

	retryAt2 := lsRow("ext-ignored", "demo", "ls-s1", "la-r2", 90, 99, 7)
	retryAt2.LastAttemptAt, retryAt2.CreatedAt = at2, at2
	retryAt2.Provider, retryAt2.Model, retryAt2.ConfigHash = "prov-b", "model-b", "hash-b"
	got, err = store.BeginExtraction(ctx, retryAt2)
	require.NoError(t, err)
	require.Equal(t, "ext-a", got.ExtractionID, "重试复用同一行")
	require.Equal(t, 2, got.Attempts)
	require.Equal(t, "la-r2", got.LastAttemptRunID, "第 2 次尝试绑定本次 Run")
	require.Equal(t, "prov-b", got.Provider)
	require.NoError(t, store.FailExtraction(ctx, "ext-a", core.LearningFailureModel, "boom", at2))

	retryAt3 := lsRow("ext-ignored", "demo", "ls-s1", "la-r3", 90, 99, 7)
	retryAt3.LastAttemptAt, retryAt3.CreatedAt = at3, at3
	got, err = store.BeginExtraction(ctx, retryAt3)
	require.NoError(t, err)
	require.Equal(t, 3, got.Attempts)
	require.Equal(t, "la-r3", got.LastAttemptRunID, "第 3 次尝试绑定本次 Run")
	require.NoError(t, store.FailExtraction(ctx, "ext-a", core.LearningFailureModel, "boom", at3))

	var status string
	var finishedAt sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT status, finished_at FROM memory_extractions WHERE extraction_id = 'ext-a'`).
		Scan(&status, &finishedAt))
	require.Equal(t, string(core.LearningExtractionSkipped), status, "三次失败转 skipped")
	require.True(t, finishedAt.Valid, "skipped 是终态，必须写 finished_at")

	cursor, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "la-r3")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(20), cursor.CommittedMessageID, "转 skipped 时推进游标")

	unresolved, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "la-r3")
	require.NoError(t, err)
	require.Nil(t, unresolved, "终态行不再阻断同来源新窗口")
}

// TestUS22_Store_BudgetAtomic：滚动 24h 预算按 SUM(attempts) 判定——窗口内耗尽即
// `ErrLearningBudgetExhausted` 且不建行（原子拒绝），窗口外的历史尝试不计入。
// TestUS22_Store_BudgetAtomic pins the rolling 24h attempt budget: exhausted means no row
// is created, and attempts older than the window do not count.
func TestUS22_Store_BudgetAtomic(t *testing.T) {
	ctx := context.Background()
	seedSpent := func(t *testing.T, db *DB, attemptAt time.Time) {
		t.Helper()
		// 67 行 × 3 次 = 201 ≥ 200；直接 SQL 造历史行，避免 67 次事务。
		_, err := db.pool.ExecContext(ctx, `INSERT INTO memory_extractions
			(extraction_id, agent_name, session_id, run_id, last_attempt_run_id, origin_caller_id,
			 origin_key_id, status, window_from_message_id, window_to_message_id, window_messages,
			 attempts, prompt_version, rule_version, provider, model, created_at, last_attempt_at)
			SELECT 'bulk-' || i, 'demo', 'ls-s1', 'bulk-run-' || i, 'bulk-run-' || i, 'c1', 'k1',
			       'skipped', 1, 2, 1, 3, 'pv1', 'rv1', 'p', 'm', $1, $1
			  FROM generate_series(1, 67) AS i`, attemptAt.UnixMilli())
		require.NoError(t, err)
	}

	t.Run("within-window-exhausted", func(t *testing.T) {
		db := newLearningStoreDB(t)
		store := NewLearningRepo(db)
		now := lsBase.Add(time.Hour)
		seedSpent(t, db, now.Add(-time.Hour))
		lsSeedRun(t, db, "lb-r1", "ls-s1", "k1", now)

		row := lsRow("ext-new", "demo", "ls-s1", "lb-r1", 10, 20, 4)
		row.LastAttemptAt, row.CreatedAt = now, now
		_, err := store.BeginExtraction(ctx, row)
		require.ErrorIs(t, err, core.ErrLearningBudgetExhausted)

		var count int
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT count(*) FROM memory_extractions WHERE run_id = 'lb-r1'`).Scan(&count))
		require.Zero(t, count, "预算耗尽不得留下新行")
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(attempts), 0) FROM memory_extractions WHERE agent_name = 'demo'`).
			Scan(&count))
		require.Equal(t, 201, count, "拒绝时不得增量任何既有行")
	})

	t.Run("outside-window-ignored", func(t *testing.T) {
		db := newLearningStoreDB(t)
		store := NewLearningRepo(db)
		now := lsBase.Add(25 * time.Hour)
		seedSpent(t, db, now.Add(-25*time.Hour))
		lsSeedRun(t, db, "lb-r2", "ls-s1", "k1", now)

		row := lsRow("ext-new", "demo", "ls-s1", "lb-r2", 10, 20, 4)
		row.LastAttemptAt, row.CreatedAt = now, now
		got, err := store.BeginExtraction(ctx, row)
		require.NoError(t, err, "窗口外的历史尝试不计入滚动预算")
		require.Equal(t, 1, got.Attempts)
	})
}

// lsCandidate 造一条通过逐项校验形态的候选（归档区、fact、有证据时刻）。
// lsCandidate builds one committed-form candidate with a valid evidence instant.
func lsCandidate(content string) core.LearningCandidate {
	return core.LearningCandidate{
		Scope:      core.Scope{Agent: "demo"},
		Section:    core.SectionArchive,
		Content:    content,
		Kind:       core.LearningKindFact,
		Certainty:  "asserted",
		Confidence: 0.9,
		Evidence: []core.LearningEvidence{{
			MessageID: 7, RunID: "lr-r1", Role: "user", Quote: "证据", MessageAt: lsBase,
		}},
		EvidenceAt:     lsBase,
		TimeConfidence: "unknown",
	}
}

// TestUS22_Store_CommitAtomic：提交是一个事务——候选 / 元数据 / 来源关系 / 草案 / 终态 /
// 游标要么全部落库，要么一条都不落。中途失败（第二候选缺证据时刻）必须回滚第一候选
// 已写入的条目，抽取行保持 pending（留给恢复查证），游标不动；成功路径同批全部落库。
// 逆改点：把候选写入拆成事务外 → 回滚后仍能看到第一条 memory_entries（断言红）；
// 提交终态与游标不放在同一事务 → 失败时游标已推进（断言红）。
// A mid-commit failure rolls back every write of the transaction; success commits them all.
func TestUS22_Store_CommitAtomic(t *testing.T) {
	ctx := context.Background()

	t.Run("midway-failure-rolls-back", func(t *testing.T) {
		db := newLearningStoreDB(t)
		store := NewLearningRepo(db)
		lsSeedRun(t, db, "lr-r1", "ls-s1", "k1", lsBase)
		_, err := store.BeginExtraction(ctx, lsRow("ext-atomic", "demo", "ls-s1", "lr-r1", 10, 20, 4))
		require.NoError(t, err)

		broken := lsCandidate("第二条候选")
		broken.EvidenceAt = time.Time{} // 缺证据时刻：在第一条已写入之后报错
		_, err = store.CommitExtraction(ctx, core.CommitLearningExtraction{
			ExtractionID:  "ext-atomic",
			Candidates:    []core.LearningCandidate{lsCandidate("第一条候选"), broken},
			ItemsTotal:    2,
			ItemsAccepted: 2,
			CompletedAt:   lsBase,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "缺少证据时刻")

		var count int
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT count(*) FROM memory_entries WHERE agent_name = 'demo'`).Scan(&count))
		require.Zero(t, count, "回滚后不得留下任何候选条目")
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT count(*) FROM memory_candidate_meta`).Scan(&count))
		require.Zero(t, count, "回滚后不得留下候选元数据")

		var status string
		var finishedAt sql.NullInt64
		var itemsTotal int
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT status, finished_at, items_total FROM memory_extractions WHERE extraction_id = 'ext-atomic'`).
			Scan(&status, &finishedAt, &itemsTotal))
		require.Equal(t, string(core.LearningExtractionPending), status, "失败提交保持 pending 交恢复查证")
		require.False(t, finishedAt.Valid)
		require.Zero(t, itemsTotal, "终态计数不得部分写入")

		_, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lr-r1")
		require.NoError(t, err)
		require.False(t, found, "提交失败不得推进游标")
	})

	t.Run("success-commits-everything", func(t *testing.T) {
		db := newLearningStoreDB(t)
		store := NewLearningRepo(db)
		lsSeedRun(t, db, "lr-r2", "ls-s1", "k1", lsBase)
		_, err := store.BeginExtraction(ctx, lsRow("ext-ok", "demo", "ls-s1", "lr-r2", 10, 20, 4))
		require.NoError(t, err)

		candidate := lsCandidate("提交成功的候选")
		result, err := store.CommitExtraction(ctx, core.CommitLearningExtraction{
			ExtractionID:  "ext-ok",
			Candidates:    []core.LearningCandidate{candidate},
			Drafts:        []core.LearningDraft{{DraftID: "dft_x", Name: "n", ContentSHA256: "sha", RelDir: "dft_x", Evidence: candidate.Evidence}},
			ItemsTotal:    2,
			ItemsAccepted: 2,
			CompletedAt:   lsBase,
		})
		require.NoError(t, err)
		require.Len(t, result.CandidateEntryIDs, 1)
		require.Len(t, result.DraftIDs, 1)

		var state, source, section string
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT state, source, section FROM memory_entries WHERE id = $1`, result.CandidateEntryIDs[0]).
			Scan(&state, &source, &section))
		require.Equal(t, memoryStateCandidate, state)
		require.Equal(t, memorySourceAutoExtract, source)
		require.Equal(t, string(core.SectionArchive), section)

		var status string
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT status FROM memory_extractions WHERE extraction_id = 'ext-ok'`).Scan(&status))
		require.Equal(t, string(core.LearningExtractionSuccess), status)

		cursor, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lr-r2")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, int64(20), cursor.CommittedMessageID, "成功提交在同一事务推进游标")
	})
}

// TestUS22_Store_CommitAdvisoryLock：提交与预写取同一把 Agent 级 advisory lock——
// 另一连接持锁时提交必须等待（超时即失败），释放后同一提交成功。锁键必须按 Agent，
// 换了键名就锁不住并发的抽取事务。
// 逆改点：提交事务去掉 advisory lock → 持锁期间提交直接成功（断言红）。
// A held Agent-level advisory lock blocks the commit until the lock is released.
func TestUS22_Store_CommitAdvisoryLock(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	lsSeedRun(t, db, "lr-r3", "ls-s1", "k1", lsBase)
	_, err := store.BeginExtraction(ctx, lsRow("ext-lock", "demo", "ls-s1", "lr-r3", 10, 20, 4))
	require.NoError(t, err)

	holder, err := db.pool.Conn(ctx)
	require.NoError(t, err)
	defer holder.Close()
	holdTx, err := holder.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = holdTx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('memory_extract:' || $1))`, "demo")
	require.NoError(t, err)

	lockedCtx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	_, lockErr := store.CommitExtraction(lockedCtx, core.CommitLearningExtraction{
		ExtractionID: "ext-lock",
		Candidates:   []core.LearningCandidate{lsCandidate("锁下的候选")},
		ItemsTotal:   1, ItemsAccepted: 1, CompletedAt: lsBase,
	})
	require.Error(t, lockErr, "持锁期间提交必须等待而不是直接写库")
	require.Contains(t, lockErr.Error(), "取 Agent 锁")

	require.NoError(t, holdTx.Rollback())
	heldCtx, cancelHeld := context.WithTimeout(ctx, 5*time.Second)
	defer cancelHeld()
	_, err = store.CommitExtraction(heldCtx, core.CommitLearningExtraction{
		ExtractionID: "ext-lock",
		Candidates:   []core.LearningCandidate{lsCandidate("锁下的候选")},
		ItemsTotal:   1, ItemsAccepted: 1, CompletedAt: lsBase,
	})
	require.NoError(t, err, "锁释放后同一提交必须成功")

	var status string
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT status FROM memory_extractions WHERE extraction_id = 'ext-lock'`).Scan(&status))
	require.Equal(t, string(core.LearningExtractionSuccess), status)
}

// TestUS22_Store_RetryWindowMissingSkips：重试重读冻结窗口发现消息已被清理时，按
// `window_gone` 写 skipped（终态、attempts 不再递增）并在同一事务推进游标；之后同来源
// 新窗口不再被阻断（FR-22-10 / C-33）。窗口边界仍取冻结值，不取本次 Run 的新值。
// 顺序必须与生产一致：服务在 BeginExtraction **之前**重读冻结窗口
// （internal/memory/learning.go:418-433），此时行还停在上一次尝试的 `failed`；
// `FailExtraction` 必须能从 `failed` 直接终结为 skipped。旧用例在 fail 之前先
// BeginExtraction 把行翻回 pending，掩盖了这条转移（2026-11-06 独立复核 F-01/F-02）。
// 逆改点：window_gone 写成可重试 failed → 行继续阻断且无 finished_at（断言红）；
// 转移 skipped 时漏推游标 → 游标仍为 0（断言红）。
// A retry that finds the frozen window gone terminates as skipped and advances the cursor.
func TestUS22_Store_RetryWindowMissingSkips(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	lsSeedRun(t, db, "lw-r1", "ls-s1", "k1", lsBase)
	lsSeedRun(t, db, "lw-r2", "ls-s1", "k1", lsBase.Add(time.Minute))

	// 第 1 次尝试：pending → 模型错误 → failed（attempts=1，可重试）。
	_, err := store.BeginExtraction(ctx, lsRow("ext-gone", "demo", "ls-s1", "lw-r1", 10, 20, 4))
	require.NoError(t, err)
	require.NoError(t, store.FailExtraction(ctx, "ext-gone", core.LearningFailureModel, "boom", lsBase))

	// 第 2 次尝试：复用同一冻结窗口，attempts 递增到 2；这次仍以模型错误结束。
	retry := lsRow("ext-ignored", "demo", "ls-s1", "lw-r2", 90, 99, 7) // 新边界必须被忽略
	retry.LastAttemptAt, retry.CreatedAt = lsBase.Add(time.Minute), lsBase.Add(time.Minute)
	got, err := store.BeginExtraction(ctx, retry)
	require.NoError(t, err)
	require.Equal(t, "ext-gone", got.ExtractionID)
	require.Equal(t, 2, got.Attempts)
	require.Equal(t, int64(20), got.WindowToMessageID, "窗口边界取冻结值")
	require.NoError(t, store.FailExtraction(ctx, "ext-gone", core.LearningFailureModel, "boom", lsBase.Add(time.Minute)))

	// 第 3 次尝试：服务在 BeginExtraction 之前发现冻结窗口已被清理——行仍为
	// failed，必须在这里直接终结为 skipped 并推进游标。
	require.NoError(t, store.FailExtraction(ctx, "ext-gone", core.LearningFailureWindowGone,
		"frozen window incomplete", lsBase.Add(2*time.Minute)))

	var status, class string
	var attempts int
	var finishedAt sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT status, failure_class, attempts, finished_at FROM memory_extractions
		  WHERE extraction_id = 'ext-gone'`).Scan(&status, &class, &attempts, &finishedAt))
	require.Equal(t, string(core.LearningExtractionSkipped), status, "窗口缺失是终态 skipped")
	require.Equal(t, string(core.LearningFailureWindowGone), class)
	require.Equal(t, 2, attempts, "失败写回不得再递增尝试数")
	require.True(t, finishedAt.Valid)

	cursor, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "lw-r2")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(20), cursor.CommittedMessageID, "skipped 在同一事务推进游标")

	unresolved, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "lw-r2")
	require.NoError(t, err)
	require.Nil(t, unresolved, "终态后同来源新窗口不再阻断")
}

// TestUS22_Store_BeginMemoryExtractCall：抽取调用的预写行固定 `purpose='memory_extract'`、
// `round=0`、`status='pending'`；既有 Chat / Compaction 两个入口的用途不变（T009）。
// TestUS22_Store_BeginMemoryExtractCall pins the extraction audit literal and leaves the
// existing purposes untouched.
func TestUS22_Store_BeginMemoryExtractCall(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	repo := NewLLMCallRepo(db)
	lsSeedRun(t, db, "ll-r1", "ls-s1", "k1", lsBase)

	id, err := repo.BeginMemoryExtractCall(ctx, core.LLMCall{
		RunID: "ll-r1", SessionID: "ls-s1", Provider: "prov", Model: "model", CreatedAt: lsBase,
	})
	require.NoError(t, err)
	require.Positive(t, id)

	var purpose, status string
	var round int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT purpose, status, round FROM llm_calls WHERE id = $1`, id).
		Scan(&purpose, &status, &round))
	require.Equal(t, "memory_extract", purpose)
	require.Equal(t, "pending", status)
	require.Equal(t, 0, round)

	chatID, err := repo.BeginCall(ctx, core.LLMCall{
		RunID: "ll-r1", SessionID: "ls-s1", Round: 1, Provider: "prov", Model: "model", CreatedAt: lsBase,
	})
	require.NoError(t, err)
	compactionID, err := repo.BeginCompactionCall(ctx, core.LLMCall{
		RunID: "ll-r1", SessionID: "ls-s1", Provider: "prov", Model: "model", CreatedAt: lsBase,
	})
	require.NoError(t, err)

	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT purpose FROM llm_calls WHERE id = $1`, chatID).Scan(&purpose))
	require.Equal(t, "chat", purpose, "既有 Chat 入口的用途不得漂移")
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT purpose FROM llm_calls WHERE id = $1`, compactionID).Scan(&purpose))
	require.Equal(t, "compaction", purpose, "既有 Compaction 入口的用途不得漂移")
}

// TestUS22_Store_PendingScan：启动恢复只扫描 `status='pending'` 行——success / skipped /
// failed / 已查证与未查证 unknown 都不在扫描面内；顺序按创建时刻、主键升序；查证一条后
// 它立即退出扫描面（C-13 / FR-22-09）。
// 逆改点：扫描条件放宽成「非终态」→ failed / unknown 混入结果（断言红）；
// 排序丢掉 created_at → 顺序断言红。
// PendingExtractions lists only pending rows in creation order.
func TestUS22_Store_PendingScan(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	// 每条 pending 行各占一个来源：同来源的第二条会在预写时被未决判定拒绝。
	lsSeedRun(t, db, "pk-r1", "ls-s1", "k1", lsBase)
	lsSeedRun(t, db, "pk-r2", "ls-s1", "k2", lsBase)
	lsSeedRun(t, db, "pk-r3", "ls-s1", "k3", lsBase)
	lsSeedRun(t, db, "pk-r4", "ls-s1", "k4", lsBase)
	lsSeedRun(t, db, "pk-r5", "ls-s1", "k5", lsBase)
	lsSeedRun(t, db, "pk-r6", "ls-s1", "k6", lsBase)
	lsSeedRun(t, db, "pk-r7", "ls-s1", "k7", lsBase)

	first := lsRow("ext-p1", "demo", "ls-s1", "pk-r1", 1, 2, 2)
	first.CreatedAt = lsBase
	_, err := store.BeginExtraction(ctx, first)
	require.NoError(t, err)
	second := lsRow("ext-p2", "demo", "ls-s1", "pk-r2", 3, 4, 2)
	second.CreatedAt = lsBase.Add(time.Minute)
	_, err = store.BeginExtraction(ctx, second)
	require.NoError(t, err)

	// success：零候选也是合法提交，终态行不再待决。
	_, err = store.BeginExtraction(ctx, lsRow("ext-p3", "demo", "ls-s1", "pk-r3", 5, 6, 2))
	require.NoError(t, err)
	_, err = store.CommitExtraction(ctx, core.CommitLearningExtraction{
		ExtractionID: "ext-p3", CompletedAt: lsBase,
	})
	require.NoError(t, err)

	// skipped：窗口缺失是终态。
	_, err = store.BeginExtraction(ctx, lsRow("ext-p4", "demo", "ls-s1", "pk-r4", 7, 8, 2))
	require.NoError(t, err)
	require.NoError(t, store.FailExtraction(ctx, "ext-p4", core.LearningFailureWindowGone, "gone", lsBase))

	// failed：可重试失败仍不是 pending。
	_, err = store.BeginExtraction(ctx, lsRow("ext-p5", "demo", "ls-s1", "pk-r5", 9, 10, 2))
	require.NoError(t, err)
	require.NoError(t, store.FailExtraction(ctx, "ext-p5", core.LearningFailureModel, "boom", lsBase))

	// 已查证 unknown：恢复路径的正常产物。
	_, err = store.BeginExtraction(ctx, lsRow("ext-p6", "demo", "ls-s1", "pk-r6", 11, 12, 2))
	require.NoError(t, err)
	require.NoError(t, store.RecoverExtraction(ctx, "ext-p6", core.LearningExtractionUnknown, lsBase))

	// 未查证 unknown：恢复写入总会带查证时刻，这里直接造出防御性判据覆盖的状态。
	_, err = store.BeginExtraction(ctx, lsRow("ext-p7", "demo", "ls-s1", "pk-r7", 13, 14, 2))
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx,
		`UPDATE memory_extractions SET status = 'unknown' WHERE extraction_id = 'ext-p7'`)
	require.NoError(t, err)

	pending, err := store.PendingExtractions(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 2, "只有 pending 行进入启动扫描")
	require.Equal(t, "ext-p1", pending[0].ExtractionID, "按创建时刻升序")
	require.Equal(t, "ext-p2", pending[1].ExtractionID)
	require.Equal(t, core.LearningExtractionPending, pending[0].Status)
	require.Equal(t, "c1", pending[0].OriginCallerID)
	require.Equal(t, "k1", pending[0].OriginKeyID)

	// 查证一条后它退出扫描面，另一条不受影响。
	require.NoError(t, store.RecoverExtraction(ctx, "ext-p1", core.LearningExtractionSuccess, lsBase.Add(2*time.Minute)))
	pending, err = store.PendingExtractions(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "ext-p2", pending[0].ExtractionID)
}

// TestUS22_Store_RecoverAdvancesCursor：恢复查证在单事务内把来源游标推进到冻结窗口末端，
// 已在前面的游标不回退（GREATEST）；游标写失败时状态与查证时刻一并回滚（C-13）。
// 逆改点：游标写改成直接赋值 → 更大的既有游标被回退（断言红）；状态更新与游标推进拆成
// 两个事务 → 游标故障时状态已落地、查证时刻非空（断言红）。
// RecoverExtraction advances the cursor monotonically; a cursor failure rolls the verdict back.
func TestUS22_Store_RecoverAdvancesCursor(t *testing.T) {
	t.Run("advances-to-window-to", func(t *testing.T) {
		ctx := context.Background()
		db := newLearningStoreDB(t)
		store := NewLearningRepo(db)
		lsSeedRun(t, db, "pc-r1", "ls-s1", "k1", lsBase)
		_, err := store.BeginExtraction(ctx, lsRow("ext-pc", "demo", "ls-s1", "pc-r1", 10, 20, 4))
		require.NoError(t, err)

		verifiedAt := lsBase.Add(5 * time.Minute)
		require.NoError(t, store.RecoverExtraction(ctx, "ext-pc", core.LearningExtractionSuccess, verifiedAt))

		cursor, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "pc-r1")
		require.NoError(t, err)
		require.True(t, found, "查证必须在同一事务写出游标")
		require.Equal(t, int64(20), cursor.CommittedMessageID, "游标推进到冻结窗口末端")
		require.Equal(t, verifiedAt.Unix(), cursor.UpdatedAt.Unix(), "游标时刻取查证时刻")
	})

	t.Run("never-regresses", func(t *testing.T) {
		ctx := context.Background()
		db := newLearningStoreDB(t)
		store := NewLearningRepo(db)
		lsSeedRun(t, db, "pc-r1", "ls-s1", "k1", lsBase)
		_, err := store.BeginExtraction(ctx, lsRow("ext-first", "demo", "ls-s1", "pc-r1", 25, 30, 2))
		require.NoError(t, err)
		_, err = store.CommitExtraction(ctx, core.CommitLearningExtraction{
			ExtractionID: "ext-first", CompletedAt: lsBase,
		})
		require.NoError(t, err)

		// 存储层不校验窗口与游标的先后（窗口切割由服务层按游标完成）；这里刻意造一个更小的
		// 窗口，钉住「恢复只前进、不回退」。
		lsSeedRun(t, db, "pc-r2", "ls-s1", "k1", lsBase.Add(time.Minute))
		_, err = store.BeginExtraction(ctx, lsRow("ext-second", "demo", "ls-s1", "pc-r2", 1, 2, 2))
		require.NoError(t, err)
		require.NoError(t, store.RecoverExtraction(ctx, "ext-second", core.LearningExtractionSuccess, lsBase.Add(2*time.Minute)))

		cursor, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "pc-r2")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, int64(30), cursor.CommittedMessageID, "已有更大游标不得被较小窗口回退（GREATEST）")
	})

	t.Run("cursor-failure-rolls-back-verdict", func(t *testing.T) {
		ctx := context.Background()
		db := newLearningStoreDB(t)
		store := NewLearningRepo(db)
		lsSeedRun(t, db, "pc-r1", "ls-s1", "k1", lsBase)
		_, err := store.BeginExtraction(ctx, lsRow("ext-rollback", "demo", "ls-s1", "pc-r1", 10, 20, 2))
		require.NoError(t, err)

		// 先建游标行，让推进走 ON CONFLICT 分支；BEFORE INSERT/UPDATE 触发器把这次写入打回，
		// 用来验证状态与查证时刻跟着**同一事务**回滚。
		_, err = db.pool.ExecContext(ctx,
			`INSERT INTO memory_extraction_cursors
				(agent_name, session_id, origin_caller_id, origin_key_id, committed_message_id, updated_at)
			 VALUES ('demo', 'ls-s1', 'c1', 'k1', 0, $1)`, lsBase.UnixMilli())
		require.NoError(t, err)
		_, err = db.pool.ExecContext(ctx, `CREATE FUNCTION us22_deny_cursor_write() RETURNS trigger AS $fn$
			BEGIN
				RAISE EXCEPTION 'us22 cursor write denied';
			END
			$fn$ LANGUAGE plpgsql`)
		require.NoError(t, err)
		_, err = db.pool.ExecContext(ctx,
			`CREATE TRIGGER us22_deny_cursor_write BEFORE INSERT OR UPDATE ON memory_extraction_cursors
			 FOR EACH ROW EXECUTE FUNCTION us22_deny_cursor_write()`)
		require.NoError(t, err)

		err = store.RecoverExtraction(ctx, "ext-rollback", core.LearningExtractionSuccess, lsBase.Add(time.Minute))
		require.Error(t, err, "游标写失败必须让整次恢复失败")
		require.Contains(t, err.Error(), "推进游标")

		var status string
		var verifiedAt sql.NullInt64
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT status, verified_at FROM memory_extractions WHERE extraction_id = 'ext-rollback'`).
			Scan(&status, &verifiedAt))
		require.Equal(t, string(core.LearningExtractionPending), status, "游标失败时状态必须回滚为 pending")
		require.False(t, verifiedAt.Valid, "游标失败时查证时刻不得落库")

		// 去掉故障后同一恢复必须成功，状态、查证时刻与游标一次写出。
		_, err = db.pool.ExecContext(ctx, `DROP TRIGGER us22_deny_cursor_write ON memory_extraction_cursors`)
		require.NoError(t, err)
		_, err = db.pool.ExecContext(ctx, `DROP FUNCTION us22_deny_cursor_write()`)
		require.NoError(t, err)
		require.NoError(t, store.RecoverExtraction(ctx, "ext-rollback", core.LearningExtractionSuccess, lsBase.Add(2*time.Minute)))

		cursor, found, err := store.LoadCursor(ctx, "demo", "ls-s1", "pc-r1")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, int64(20), cursor.CommittedMessageID)
	})
}

// TestUS22_Store_RecoverSetsVerifiedAt：恢复结论只允许 success / unknown 两个取值域，两者
// 都按毫秒写 `verified_at`；已查证行是幂等空操作——后来的结论与时刻都不得改口（C-13 /
// C-14）。
// 逆改点：UPDATE 漏写 verified_at → 查证时刻为空（断言红）；非 pending 行未提前返回 →
// 二次调用覆盖状态与时刻（断言红）。
// RecoverExtraction writes verified_at for both verdicts and never rewrites a verdict.
func TestUS22_Store_RecoverSetsVerifiedAt(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	lsSeedRun(t, db, "rv-r1", "ls-s1", "k1", lsBase)
	lsSeedRun(t, db, "rv-r2", "ls-s1", "k2", lsBase)
	lsSeedRun(t, db, "rv-r3", "ls-s1", "k3", lsBase)

	_, err := store.BeginExtraction(ctx, lsRow("ext-v", "demo", "ls-s1", "rv-r1", 10, 20, 4))
	require.NoError(t, err)
	verifiedAt := lsBase.Add(90 * time.Minute)
	require.NoError(t, store.RecoverExtraction(ctx, "ext-v", core.LearningExtractionSuccess, verifiedAt))

	var status string
	var verifiedMS sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT status, verified_at FROM memory_extractions WHERE extraction_id = 'ext-v'`).
		Scan(&status, &verifiedMS))
	require.Equal(t, string(core.LearningExtractionSuccess), status)
	require.True(t, verifiedMS.Valid)
	require.Equal(t, verifiedAt.UnixMilli(), verifiedMS.Int64, "查证时刻按毫秒落库")

	// 二次查证不改口：结论与时刻都保持首次写入。
	require.NoError(t, store.RecoverExtraction(ctx, "ext-v", core.LearningExtractionUnknown, verifiedAt.Add(time.Hour)))
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT status, verified_at FROM memory_extractions WHERE extraction_id = 'ext-v'`).
		Scan(&status, &verifiedMS))
	require.Equal(t, string(core.LearningExtractionSuccess), status, "已查证行不得改口")
	require.Equal(t, verifiedAt.UnixMilli(), verifiedMS.Int64, "查证时刻不得被二次调用覆盖")

	// unknown 是另一条合法结论，同样写查证时刻。
	_, err = store.BeginExtraction(ctx, lsRow("ext-u", "demo", "ls-s1", "rv-r2", 21, 22, 1))
	require.NoError(t, err)
	unknownAt := lsBase.Add(2 * time.Hour)
	require.NoError(t, store.RecoverExtraction(ctx, "ext-u", core.LearningExtractionUnknown, unknownAt))
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT status, verified_at FROM memory_extractions WHERE extraction_id = 'ext-u'`).
		Scan(&status, &verifiedMS))
	require.Equal(t, string(core.LearningExtractionUnknown), status)
	require.Equal(t, unknownAt.UnixMilli(), verifiedMS.Int64)

	// 取值域外：skipped / failed / pending 不是恢复结论，拒绝且不落任何写入。
	_, err = store.BeginExtraction(ctx, lsRow("ext-bad", "demo", "ls-s1", "rv-r3", 23, 24, 1))
	require.NoError(t, err)
	err = store.RecoverExtraction(ctx, "ext-bad", core.LearningExtractionSkipped, lsBase.Add(3*time.Hour))
	require.Error(t, err)
	require.Contains(t, err.Error(), "取值域外")
	var badStatus string
	var badVerified sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT status, verified_at FROM memory_extractions WHERE extraction_id = 'ext-bad'`).
		Scan(&badStatus, &badVerified))
	require.Equal(t, string(core.LearningExtractionPending), badStatus)
	require.False(t, badVerified.Valid)

	// 不存在的行：ErrLearningNotFound。
	err = store.RecoverExtraction(ctx, "ext-missing", core.LearningExtractionSuccess, lsBase)
	require.ErrorIs(t, err, core.ErrLearningNotFound)
}

// TestUS22_Store_UnresolvedBlocksSameOrigin：未决判定按（Agent、会话、来源调用方、来源 Key）
// 精确隔离——pending 与可重试 failed 阻断同来源新窗口，其他来源 / 其他 Agent 不受影响；
// 被阻断的预写不留下任何行（FR-22-03 / C-07）。
// 逆改点：未决查询漏掉 origin_key_id → 跨 Key 误阻断（断言红）；判定放到写入之后 →
// 被阻断的 Run 留下行（断言红）。
// Unresolved blocking is per origin and happens before any write.
func TestUS22_Store_UnresolvedBlocksSameOrigin(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	lsSeedRun(t, db, "ub-r1", "ls-s1", "k1", lsBase)
	lsSeedRun(t, db, "ub-r2", "ls-s1", "k2", lsBase)
	lsSeedRun(t, db, "ub-r3", "ls-s1", "k1", lsBase.Add(time.Minute))

	_, err := store.BeginExtraction(ctx, lsRow("ext-block", "demo", "ls-s1", "ub-r1", 1, 2, 2))
	require.NoError(t, err)

	pending, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "ub-r1")
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, "ext-block", pending.ExtractionID)
	require.Equal(t, core.LearningExtractionPending, pending.Status)

	// 同来源的新窗口在预写层被拒绝，且不建第二行。
	_, err = store.BeginExtraction(ctx, lsRow("ext-second", "demo", "ls-s1", "ub-r3", 3, 4, 2))
	require.ErrorIs(t, err, core.ErrLearningUnresolved)
	var count int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_extractions WHERE run_id = 'ub-r3'`).Scan(&count))
	require.Zero(t, count, "被阻断的预写不得留下任何行")

	// 其他来源、其他 Agent 不受影响。
	otherOrigin, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "ub-r2")
	require.NoError(t, err)
	require.Nil(t, otherOrigin)
	_, err = store.BeginExtraction(ctx, lsRow("ext-other", "demo", "ls-s1", "ub-r2", 3, 4, 2))
	require.NoError(t, err, "未决判定必须按来源隔离")
	otherAgent, err := store.UnresolvedForOrigin(ctx, "other-agent", "ls-s1", "ub-r1")
	require.NoError(t, err)
	require.Nil(t, otherAgent)

	// 可重试 failed 仍算未决：下一次尝试复用冻结窗口，不算新窗口。
	require.NoError(t, store.FailExtraction(ctx, "ext-block", core.LearningFailureModel, "boom", lsBase.Add(30*time.Second)))
	retryable, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "ub-r1")
	require.NoError(t, err)
	require.NotNil(t, retryable, "可重试失败必须仍算未决")
	require.Equal(t, core.LearningExtractionFailed, retryable.Status)
	require.Equal(t, 1, retryable.Attempts)

	// 重试改回 pending，继续阻断。
	retried, err := store.BeginExtraction(ctx, lsRow("ext-ignored", "demo", "ls-s1", "ub-r3", 99, 99, 7))
	require.NoError(t, err)
	require.Equal(t, "ext-block", retried.ExtractionID)
	require.Equal(t, 2, retried.Attempts)
	again, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "ub-r1")
	require.NoError(t, err)
	require.NotNil(t, again)
	require.Equal(t, core.LearningExtractionPending, again.Status)
}

// TestUS22_Store_UnresolvedSkipsVerifiedUnknown：已查证的 unknown 保留为历史但不再阻断同
// 来源新窗口；未查证的 unknown（verified_at IS NULL）仍阻断（C-13 / C-14）。
// 逆改点：未决条件写成 `status='unknown'` 而不看 verified_at → 已查证行仍阻断（断言红）；
// 查证时删行 → 历史计数归零（断言红）。
// A verified unknown stops blocking; an unverified one still does.
func TestUS22_Store_UnresolvedSkipsVerifiedUnknown(t *testing.T) {
	ctx := context.Background()
	db := newLearningStoreDB(t)
	store := NewLearningRepo(db)
	lsSeedRun(t, db, "uv-r1", "ls-s1", "k1", lsBase)
	lsSeedRun(t, db, "uv-r2", "ls-s1", "k1", lsBase.Add(time.Minute))
	lsSeedRun(t, db, "uv-r3", "ls-s1", "k2", lsBase)
	lsSeedRun(t, db, "uv-r4", "ls-s1", "k2", lsBase.Add(time.Minute))

	_, err := store.BeginExtraction(ctx, lsRow("ext-verified", "demo", "ls-s1", "uv-r1", 1, 5, 2))
	require.NoError(t, err)
	require.NoError(t, store.RecoverExtraction(ctx, "ext-verified", core.LearningExtractionUnknown, lsBase.Add(2*time.Minute)))

	// 已查证 unknown 保留为历史，但不再阻断。
	unresolved, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "uv-r1")
	require.NoError(t, err)
	require.Nil(t, unresolved)
	next, err := store.BeginExtraction(ctx, lsRow("ext-next", "demo", "ls-s1", "uv-r2", 6, 8, 2))
	require.NoError(t, err, "查证后同来源新窗口必须解除阻断")
	require.Equal(t, core.LearningExtractionPending, next.Status)

	var count int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_extractions WHERE agent_name = 'demo' AND session_id = 'ls-s1'`).
		Scan(&count))
	require.Equal(t, 2, count, "查证记录保留为历史，不删除")

	// 未查证 unknown：恢复写入总会带查证时刻，这里直接造出防御性判据覆盖的状态。
	_, err = store.BeginExtraction(ctx, lsRow("ext-unverified", "demo", "ls-s1", "uv-r3", 9, 10, 1))
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx,
		`UPDATE memory_extractions SET status = 'unknown' WHERE extraction_id = 'ext-unverified'`)
	require.NoError(t, err)

	blocking, err := store.UnresolvedForOrigin(ctx, "demo", "ls-s1", "uv-r3")
	require.NoError(t, err)
	require.NotNil(t, blocking, "未查证 unknown 必须阻断同来源")
	require.Equal(t, core.LearningExtractionUnknown, blocking.Status)
	require.True(t, blocking.VerifiedAt.IsZero(), "未查证行不得带查证时刻")
	_, err = store.BeginExtraction(ctx, lsRow("ext-blocked", "demo", "ls-s1", "uv-r4", 11, 12, 1))
	require.ErrorIs(t, err, core.ErrLearningUnresolved)
}

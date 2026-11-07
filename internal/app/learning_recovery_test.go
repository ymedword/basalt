//go:build integration

package app

// 本文件是 US-22 T024 的启动恢复验收（颗粒度文档 §2.3.6 / §4.3 ⑤ / C-13 / C-14 / C-15），
// 并在 US-24 T019 扩入**归属保护**与回收器 B3（模块卡 §2.3.5 B3 / FR-24-39 / P-18 / R6 / R7）：
//
//	R1 `TestUS22_Recovery_CandidatesMeanSuccess`——已有候选元数据判 success，游标推进
//	R2 `TestUS22_Recovery_DraftsMeanSuccess`——只有草案行同样判 success，草案行保留
//	R3 `TestUS22_Recovery_UnknownAdvancesNoReplay`——无结果判 unknown；游标推进到冻结窗口
//	   末端；不重放（零新抽取行、零模型调用）
//	R4 `TestUS22_Recovery_UnknownNotRewritten`——已查证的 unknown 不被下一次启动补成
//	   success（不改口），`verified_at` 保持首次读数
//	R5 `TestUS22_Recovery_NoReplay`——恢复只调用「扫描 → 计数 → 写结论」四个只读查询与
//	   一个查证写方法；时钟只取一次；预期尝试随行进写入事务；端口缺失拒绝启动
//	R6 `TestUS22_Recovery_RecoveredUnknownUnblocks`——恢复前同来源新窗口被阻断，恢复后
//	   解除（`UnresolvedForOrigin` 归空、`BeginExtraction` 建新行）
//	R7 `TestUS22_Unknown_NoRestartNextRunNoReplay`——提交未知且**不重启**时，同来源下一
//	   Run 零写入零调用（B-08）
//
// 归属保护与 B3（US-24 T019）：
//
//	O1 `TestUS22_Recovery_SkipsLiveOwnerExtraction`——`last_attempt_run_id` 仍有有效 run
//	   租约的 pending 行不进启动扫描（零写入）；同轮里无租约的行照常查证
//	O2 `TestReclaimer_OrphanExtractionVerifiedWithoutRestart`——属主失联后，回收器先由
//	   A1 收尾 Run、再由 B3 查证同一行：进程不重启也能解除同来源阻断
//	O3 `TestReclaimer_OrphanExtractionSkipsLegitimateRetry`——扫描后该行经合法重试换手
//	   （`last_attempt_run_id` 变、新属主持有效租约）→ 本轮零写入、不改口
//	O4 `TestReclaimer_OrphanExtractionWaitsForConcurrentRenewal`——写入与续约在同一租约
//	   行锁上排队：续约提交（租约被推后）→ 跳过；回滚（租约仍过期）→ 照常查证（R7）
//
// 为什么 R1–R4 / R6–R7 / O1–O4 走真实 PostgreSQL：查证结论、`verified_at`、游标推进与
// 归属保护的锁序（advisory → 租约行 → 抽取行）都写在存储层的事务里，用内存替身只会变成
// "替身替我做了判断"。R5 用假存储，因为它判的是**装配层调用形状**——恢复经手的写方法只能
// 有一个，且根本没有抽取器可调（"不重放"在依赖面里就表达不出来）。O4 只能对真实数据库
// 做：行锁排队是 PostgreSQL 的语义，不是本仓代码可以模拟的。
// The real-database cases assert the verdict, verified_at, cursor and the ownership fence;
// the fake-store case asserts the assembly's call shape and clock usage. Only a live
// PostgreSQL can show that the recovery write queues behind a concurrent renewal.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

const (
	us22RecoveryAgent   = "ops"
	us22RecoverySession = "sess_recovery"
	us22RecoveryCaller  = "c-recovery"
	us22RecoveryKey     = "key-recovery"
)

// us22RecoveryEnv 是启动恢复的真实 PG 夹具：被测仓储与只读查询面共用同一个实例，
// 原始连接只用于种夹具与读断言（判据不经过被测实现）。
// The recovery fixture shares one repository with the read face; raw SQL only seeds
// rows and reads the verdicts the implementation under test must have written.
type us22RecoveryEnv struct {
	store  *postgres.LearningRepo
	memory *postgres.MemoryStore
	db     *postgres.DB
	raw    *sql.DB
}

func newUS22RecoveryEnv(t *testing.T) *us22RecoveryEnv {
	t.Helper()
	fixture := pgtest.NewFixture(t)
	env := &us22RecoveryEnv{
		store:  postgres.NewLearningRepo(fixture.DB),
		memory: postgres.NewMemoryStore(fixture.DB, baselineClock),
		db:     fixture.DB,
		raw:    openRawSQL(t, fixture.DSN),
	}
	env.seedCaller(t, us22RecoveryCaller)
	return env
}

// recover 以固定时钟跑一轮启动恢复（与 wire 的单点装配同参数）。
func (e *us22RecoveryEnv) recover(t *testing.T) {
	t.Helper()
	require.NoError(t, recoverLearning(t.Context(), LearningRecoveryDeps{
		Store:  e.store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    baselineClock,
	}))
}

// seedCaller 插一个 active 调用方——`runs.caller_id` 的外键目标。
func (e *us22RecoveryEnv) seedCaller(t *testing.T, callerID string) {
	t.Helper()
	_, err := e.raw.ExecContext(t.Context(), `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, '', 'active', $2)`,
		callerID, baselineClock().UnixMilli())
	require.NoError(t, err)
}

// seedRun 落一条已完成 Run；keyID == nil 表示本地管理员（admin-builtin / key_id NULL）。
// seedRun inserts one completed run; a nil key means the local admin origin.
func (e *us22RecoveryEnv) seedRun(t *testing.T, runID, callerID string, keyID *string) {
	t.Helper()
	kind, actor := "admin-builtin", "admin-cli"
	if keyID != nil {
		kind, actor = "caller-key", "caller"
	}
	_, err := e.raw.ExecContext(t.Context(), `
		INSERT INTO runs
			(run_id, session_id, agent, channel, user_id, trigger, status, started_at,
			 caller_id, authorizing_kind, key_id, actor)
		VALUES ($1, $2, $3, 'web', 'u1', 'user', 'completed', $4, $5, $6, $7, $8)`,
		runID, us22RecoverySession, us22RecoveryAgent, baselineClock().UnixMilli(),
		callerID, kind, keyID, actor)
	require.NoError(t, err)
}

// seedPending 落一条 pending 抽取行（提交结果未知的现场），last_attempt_run_id 取
// `<extractionID>_run`——没有对应租约行，因此按"无属主可等"入选。
// seedPending inserts a pending extraction row whose last attempt has no lease row.
func (e *us22RecoveryEnv) seedPending(t *testing.T, extractionID string, from, to int64) {
	t.Helper()
	e.seedPendingFor(t, extractionID, extractionID+"_run", from, to)
}

// seedPendingFor 落一条 pending 抽取行并显式指定 `last_attempt_run_id`：US-24 的归属
// 判据只认这一列（`run_id` 是首次尝试且 UNIQUE，拿它判会漏掉重试中的活属主）。
// seedPendingFor pins last_attempt_run_id, the only column the ownership predicate reads.
func (e *us22RecoveryEnv) seedPendingFor(t *testing.T, extractionID, lastAttemptRunID string,
	from, to int64,
) {
	t.Helper()
	at := baselineClock().UnixMilli()
	_, err := e.raw.ExecContext(t.Context(), `
		INSERT INTO memory_extractions
			(extraction_id, agent_name, session_id, run_id, last_attempt_run_id,
			 origin_caller_id, origin_key_id, status, window_from_message_id, window_to_message_id,
			 window_messages, prompt_version, rule_version, provider, model,
			 created_at, last_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $9, $10,
		        'memory-extract-v1', 'memory-rules-v1', 'stub', 'stub-model', $11, $11)`,
		extractionID, us22RecoveryAgent, us22RecoverySession, extractionID+"_run", lastAttemptRunID,
		us22RecoveryCaller, us22RecoveryKey, from, to, int(to-from+1), at)
	require.NoError(t, err)
}

// seedCandidateMeta 为抽取行落一条候选元数据——提交事务已写出结果的证据。
func (e *us22RecoveryEnv) seedCandidateMeta(t *testing.T, extractionID string) int64 {
	t.Helper()
	entryID, err := e.memory.InsertEntry(t.Context(), memory.WithSource(memory.MemoryEntry{
		Scope:   core.Scope{Agent: us22RecoveryAgent},
		Section: core.SectionArchive,
		Content: "恢复查证候选",
		State:   memory.StateCandidate,
	}, memory.SourceAutoExtract))
	require.NoError(t, err)
	at := baselineClock().UnixMilli()
	_, err = e.raw.ExecContext(t.Context(), `
		INSERT INTO memory_candidate_meta
			(entry_id, extraction_id, kind, certainty, confidence, evidence_json,
			 source_entry_ids, origin_caller_id, origin_key_id, evidence_at,
			 time_confidence, created_at)
		VALUES ($1, $2, 'fact', 'asserted', 0.9, '[]'::jsonb, '{}', $3, $4, $5, 'exact', $5)`,
		entryID, extractionID, us22RecoveryCaller, us22RecoveryKey, at)
	require.NoError(t, err)
	return entryID
}

// seedDraftRow 为抽取行落一条方法草案行——"只有草案"的提交形态。
func (e *us22RecoveryEnv) seedDraftRow(t *testing.T, extractionID, draftID string) string {
	t.Helper()
	digest := strings.Repeat("ab", 32)
	_, err := e.raw.ExecContext(t.Context(), `
		INSERT INTO memory_method_drafts
			(draft_id, extraction_id, agent_name, name, content_sha256, evidence_json,
			 rel_dir, created_at)
		VALUES ($1, $2, $3, '复盘方法', $4, '[]'::jsonb, $5, $6)`,
		draftID, extractionID, us22RecoveryAgent, digest,
		"learning/drafts/"+draftID, baselineClock().UnixMilli())
	require.NoError(t, err)
	return digest
}

// extractionVerdict 读回状态与查证时刻（原始连接，不经过被测读模型）。
func (e *us22RecoveryEnv) extractionVerdict(t *testing.T, extractionID string) (string, sql.NullInt64) {
	t.Helper()
	var (
		status   string
		verified sql.NullInt64
	)
	require.NoError(t, e.raw.QueryRowContext(t.Context(),
		`SELECT status, verified_at FROM memory_extractions WHERE extraction_id = $1`,
		extractionID).Scan(&status, &verified))
	return status, verified
}

// extractionAttempt 读回最近一次尝试与尝试次数（归属保护断言只认这两列）。
// extractionAttempt reads the last attempt binding and the attempt counter.
func (e *us22RecoveryEnv) extractionAttempt(t *testing.T, extractionID string) (string, int) {
	t.Helper()
	var (
		lastAttempt string
		attempts    int
	)
	require.NoError(t, e.raw.QueryRowContext(t.Context(),
		`SELECT last_attempt_run_id, attempts FROM memory_extractions WHERE extraction_id = $1`,
		extractionID).Scan(&lastAttempt, &attempts))
	return lastAttempt, attempts
}

// cursorAt 经生产读面取该 Run 来源的游标位置。
func (e *us22RecoveryEnv) cursorAt(t *testing.T, runID string) (int64, bool) {
	t.Helper()
	cursor, found, err := e.store.LoadCursor(t.Context(), us22RecoveryAgent, us22RecoverySession, runID)
	require.NoError(t, err)
	return cursor.CommittedMessageID, found
}

// countRows 用原始连接数行（只拼固定语句，不把用例输入当 SQL）。
func (e *us22RecoveryEnv) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, e.raw.QueryRowContext(t.Context(), query, args...).Scan(&n))
	return n
}

// R1：候选元数据证明提交事务已写出结果 → success；游标推进到冻结窗口末端。
// 逆改点：把候选行存在也判 unknown → 状态断言红；只写状态不推进游标 → 游标断言红。
func TestUS22_Recovery_CandidatesMeanSuccess(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_cand", us22RecoveryCaller, &key)
	env.seedPending(t, "lrn_cand", 5, 9)
	env.seedCandidateMeta(t, "lrn_cand")

	env.recover(t)

	status, verified := env.extractionVerdict(t, "lrn_cand")
	require.Equal(t, "success", status, "有候选元数据即证明提交已写出结果")
	require.True(t, verified.Valid, "查证结论必须写 verified_at")
	require.Equal(t, baselineClock().UnixMilli(), verified.Int64)

	position, found := env.cursorAt(t, "run_cand")
	require.True(t, found)
	require.Equal(t, int64(9), position, "游标必须推进到冻结窗口末端")
	require.Equal(t, 1, env.countRows(t, `SELECT count(*) FROM memory_extractions`),
		"查证不得产生第二条抽取行")
	require.Equal(t, 0, env.countRows(t, `SELECT count(*) FROM llm_calls`), "恢复不得调用模型")
}

// R2：只有草案行同样判 success——草案与候选是同一提交事务的两个可查痕迹；
// 草案行必须保留（C-14：提交结果未知时不得删草案文件 / 行）。
func TestUS22_Recovery_DraftsMeanSuccess(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_draft", us22RecoveryCaller, &key)
	env.seedPending(t, "lrn_draft", 1, 4)
	digest := env.seedDraftRow(t, "lrn_draft", "dft_recovery")

	env.recover(t)

	status, verified := env.extractionVerdict(t, "lrn_draft")
	require.Equal(t, "success", status, "只有草案行也是提交成功的证据")
	require.True(t, verified.Valid)

	var keptDigest string
	require.NoError(t, env.raw.QueryRowContext(t.Context(),
		`SELECT content_sha256 FROM memory_method_drafts WHERE draft_id = $1`,
		"dft_recovery").Scan(&keptDigest))
	require.Equal(t, digest, keptDigest, "查证不得删除或改写草案行")
	position, found := env.cursorAt(t, "run_draft")
	require.True(t, found)
	require.Equal(t, int64(4), position)
}

// R3：无候选 / 草案 → unknown；游标照样推进到冻结窗口末端（B-09），且不重放。
// 逆改点：恢复时重新调用抽取 / 把 unknown 补成 success / 不推进游标 → 三处断言分别红。
func TestUS22_Recovery_UnknownAdvancesNoReplay(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_unknown", us22RecoveryCaller, &key)
	env.seedPending(t, "lrn_unknown", 1, 42)

	env.recover(t)

	status, verified := env.extractionVerdict(t, "lrn_unknown")
	require.Equal(t, "unknown", status, "没有结果证据只能判 unknown，不得猜成失败或成功")
	require.True(t, verified.Valid, "unknown 也必须写查证时刻，否则永久阻断同来源")
	require.Equal(t, baselineClock().UnixMilli(), verified.Int64)

	position, found := env.cursorAt(t, "run_unknown")
	require.True(t, found, "游标按来源推进")
	require.Equal(t, int64(42), position, "游标必须推进到冻结窗口末端")
	require.Equal(t, 1, env.countRows(t, `SELECT count(*) FROM memory_extractions`),
		"不得重放：不建第二条抽取行")
	require.Equal(t, 0, env.countRows(t, `SELECT count(*) FROM llm_calls`),
		"不得重放：恢复不调用模型")
}

// R4：已查证的 unknown 不被下一次启动补成 success，`verified_at` 保持首次读数。
// 逆改点：下一次恢复把非 pending 行也重判 → 状态变 success，用例红。
func TestUS22_Recovery_UnknownNotRewritten(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_rewrite", us22RecoveryCaller, &key)
	env.seedPending(t, "lrn_rewrite", 1, 3)

	env.recover(t)
	_, first := env.extractionVerdict(t, "lrn_rewrite")
	require.True(t, first.Valid)

	// 迟到的候选行不能推翻已落盘的查证结论：非 pending 行是幂等空操作。
	env.seedCandidateMeta(t, "lrn_rewrite")
	env.recover(t)

	status, second := env.extractionVerdict(t, "lrn_rewrite")
	require.Equal(t, "unknown", status, "查证过的 unknown 不得被补成 success")
	require.True(t, second.Valid)
	require.Equal(t, first.Int64, second.Int64, "verified_at 必须保持首次查证读数")
}

// R5：恢复的调用形状固定——PendingExtractions → 每行（候选计数、草案计数、写结论）；
// 时钟只取一次；扫描读到的预期尝试原样进写入请求；缺少查证端口直接拒绝启动。
// 逆改点：恢复调用任何抽取入口 / 每行重取时钟 / 丢掉 ExpectedLastAttemptRunID /
// nil 端口静默返回 → 对应断言红。
func TestUS22_Recovery_NoReplay(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 8, 30, 0, 0, time.UTC)
	clockCalls := 0
	store := &us22RecoveryRecordingStore{
		pending: []core.LearningExtraction{
			{
				ExtractionID: "lrn_a", Agent: us22RecoveryAgent,
				LastAttemptRunID: "run_a", WindowToMessageID: 7,
			},
			{
				ExtractionID: "lrn_b", Agent: us22RecoveryAgent,
				LastAttemptRunID: "run_b", WindowToMessageID: 9,
			},
		},
		candidates: map[string]int{"lrn_a": 0, "lrn_b": 0},
		drafts:     map[string]int{"lrn_a": 1, "lrn_b": 0},
		recovered:  map[string]core.LearningExtractionState{},
		verifiedAt: map[string]time.Time{},
		expected:   map[string]string{},
	}
	require.NoError(t, recoverLearning(t.Context(), LearningRecoveryDeps{
		Store:  store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time {
			clockCalls++
			return fixed
		},
	}))

	require.Equal(t, []string{
		"pending",
		"candidates:lrn_a", "drafts:lrn_a", "recover:lrn_a:success",
		"candidates:lrn_b", "drafts:lrn_b", "recover:lrn_b:unknown",
	}, store.calls, "恢复只许扫行、数证据、写结论——不得出现任何抽取入口")
	require.Equal(t, 1, clockCalls, "整轮恢复只取一次时钟，同批查证共享同一时刻")
	require.Equal(t, core.LearningExtractionSuccess, store.recovered["lrn_a"])
	require.Equal(t, core.LearningExtractionUnknown, store.recovered["lrn_b"])
	require.Equal(t, fixed, store.verifiedAt["lrn_a"])
	require.Equal(t, fixed, store.verifiedAt["lrn_b"])
	require.Equal(t, "run_a", store.expected["lrn_a"], "预期尝试必须随行进写入事务")
	require.Equal(t, "run_b", store.expected["lrn_b"])

	require.Error(t, recoverLearning(t.Context(), LearningRecoveryDeps{}),
		"查证端口未装配时必须拒绝启动，不得静默跳过")
}

// R6：恢复前同来源新窗口被阻断，查证（unknown + verified_at + 游标推进）之后解除。
// 逆改点：只推进游标不写 verified_at、或仍把 unknown 一律阻断 → 恢复后仍然 ErrLearningUnresolved。
func TestUS22_Recovery_RecoveredUnknownUnblocks(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_first", us22RecoveryCaller, &key)
	env.seedRun(t, "run_next", us22RecoveryCaller, &key)
	env.seedPending(t, "lrn_block", 3, 7)

	pending, err := env.store.UnresolvedForOrigin(t.Context(),
		us22RecoveryAgent, us22RecoverySession, "run_next")
	require.NoError(t, err)
	require.NotNil(t, pending, "未查证的 pending 必须阻断同来源新窗口")

	_, err = env.store.BeginExtraction(t.Context(), core.LearningExtraction{
		ExtractionID:        "lrn_second",
		Agent:               us22RecoveryAgent,
		SessionID:           us22RecoverySession,
		RunID:               "run_next",
		LastAttemptRunID:    "run_next",
		WindowFromMessageID: 8,
		WindowToMessageID:   10,
		WindowMessages:      3,
		PromptVersion:       "memory-extract-v1",
		RuleVersion:         "memory-rules-v1",
		Provider:            "stub",
		Model:               "stub-model",
		LastAttemptAt:       baselineClock().Add(time.Minute),
	}, core.LeaseToken{})
	require.ErrorIs(t, err, core.ErrLearningUnresolved, "恢复前新窗口必须被阻断")
	require.Equal(t, 1, env.countRows(t, `SELECT count(*) FROM memory_extractions`),
		"被阻断的新窗口不得建行")

	env.recover(t)

	pending, err = env.store.UnresolvedForOrigin(t.Context(),
		us22RecoveryAgent, us22RecoverySession, "run_next")
	require.NoError(t, err)
	require.Nil(t, pending, "查证后的 unknown 不再阻断")

	created, err := env.store.BeginExtraction(t.Context(), core.LearningExtraction{
		ExtractionID:        "lrn_second",
		Agent:               us22RecoveryAgent,
		SessionID:           us22RecoverySession,
		RunID:               "run_next",
		LastAttemptRunID:    "run_next",
		WindowFromMessageID: 8,
		WindowToMessageID:   10,
		WindowMessages:      3,
		PromptVersion:       "memory-extract-v1",
		RuleVersion:         "memory-rules-v1",
		Provider:            "stub",
		Model:               "stub-model",
		LastAttemptAt:       baselineClock().Add(2 * time.Minute),
	}, core.LeaseToken{})
	require.NoError(t, err)
	require.Equal(t, core.LearningExtractionPending, created.Status)
	require.Equal(t, 2, env.countRows(t, `SELECT count(*) FROM memory_extractions`))

	position, found := env.cursorAt(t, "run_first")
	require.True(t, found)
	require.Equal(t, int64(7), position, "查证把游标推进到冻结窗口末端")
}

// R7（B-08）：提交结果未知且**不重启**时，同来源下一 Run 零写入零调用。
// 逆改点：`BeginExtraction` 不判 pending → 建出第二条抽取行，计数断言红。
func TestUS22_Unknown_NoRestartNextRunNoReplay(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	key := us22RecoveryKey
	env.seedRun(t, "run_lost", us22RecoveryCaller, &key)
	env.seedRun(t, "run_same_origin", us22RecoveryCaller, &key)
	env.seedPending(t, "lrn_lost", 1, 6)

	_, err := env.store.BeginExtraction(t.Context(), core.LearningExtraction{
		ExtractionID:        "lrn_after",
		Agent:               us22RecoveryAgent,
		SessionID:           us22RecoverySession,
		RunID:               "run_same_origin",
		LastAttemptRunID:    "run_same_origin",
		WindowFromMessageID: 7,
		WindowToMessageID:   8,
		WindowMessages:      2,
		PromptVersion:       "memory-extract-v1",
		RuleVersion:         "memory-rules-v1",
		Provider:            "stub",
		Model:               "stub-model",
		LastAttemptAt:       baselineClock().Add(time.Minute),
	}, core.LeaseToken{})
	require.ErrorIs(t, err, core.ErrLearningUnresolved,
		"未查证的 pending 阻断同来源新窗口——不重启也不放行")

	require.Equal(t, 1, env.countRows(t, `SELECT count(*) FROM memory_extractions`),
		"被阻断的 Run 不得产生第二条抽取行")
	require.Equal(t, 0, env.countRows(t, `SELECT count(*) FROM llm_calls`),
		"被阻断的 Run 不得调用模型")

	status, verified := env.extractionVerdict(t, "lrn_lost")
	require.Equal(t, "pending", status, "未重启时行保持 pending，等待下次启动查证")
	require.False(t, verified.Valid)
}

// O1（US-24 T019 / P-18）：归属保护。`last_attempt_run_id` 仍有有效 run 租约的 pending
// 行不进启动扫描——那是另一个 Worker 正在收尾的抽取，把它当孤儿会推进它的游标。
// 同一轮里没有租约的对照行照常查证，证明"跳过"是逐行判定而不是整轮放弃。
// 逆改点：去掉 `NOT EXISTS` 归属谓词 / 改成按 `run_id` 判（对照行与活行都命中）→
// 活行被写成 unknown、游标越过它，断言红。
func TestUS22_Recovery_SkipsLiveOwnerExtraction(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	ctx := t.Context()

	// 属主仍活着：last_attempt_run_id 上有未过期的 run 租约。
	_, err := postgres.NewLeaseStore(env.db).Acquire(ctx, core.LeaseRun, "run_live_owner",
		core.WorkerIdentity{InstanceID: "worker-live", Name: "live"}, time.Minute)
	require.NoError(t, err)

	env.seedPendingFor(t, "lrn_live", "run_live_owner", 5, 9)
	env.seedPending(t, "lrn_dead", 1, 4) // 无任何租约行：属主已不可考
	key := us22RecoveryKey
	env.seedRun(t, "run_cursor", us22RecoveryCaller, &key)

	// 判据直接落在扫描输出上：活属主的行**不入选**，不能靠写入侧的二次判定兜底——
	// 那会在扫描、计数与日志里多出一条本不该存在的"孤儿"。
	scanned, err := env.store.PendingExtractions(ctx)
	require.NoError(t, err)
	require.Len(t, scanned, 1, "启动扫描只入选属主已失联的 pending 行")
	require.Equal(t, "lrn_dead", scanned[0].ExtractionID)

	env.recover(t)

	status, verified := env.extractionVerdict(t, "lrn_live")
	require.Equal(t, "pending", status, "属主仍有有效租约的 pending 行不得被启动恢复查证")
	require.False(t, verified.Valid, "跳过必须零写入：verified_at 仍为空")

	dead, deadVerified := env.extractionVerdict(t, "lrn_dead")
	require.Equal(t, "unknown", dead, "无租约的孤儿行照常查证")
	require.True(t, deadVerified.Valid)

	position, found := env.cursorAt(t, "run_cursor")
	require.True(t, found)
	require.Equal(t, int64(4), position, "游标只按真正查证过的行推进，活属主那一行没有被碰")
}

// O2（US-24 T019 / FR-24-39 / P-18）：属主失联后，回收器 A1 先收尾 Run、B3 再查证同一
// 行——**进程不重启**也能解除同来源阻断。这是 B3 存在的理由：启动扫描只在启动时跑一次，
// 之后崩溃的属主留下的 pending 行不能等下一次重启。
// 逆改点：B3 只接启动扫描、或查证后不推进游标 → 行仍是 pending / 游标不到窗口末端。
func TestReclaimer_OrphanExtractionVerifiedWithoutRestart(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	ctx := t.Context()

	// A 崩溃的现场：Run 停在 running、租约已过期；学习钩子已预写 pending 行。
	appSeedRun(t, env.db, "run_orphan", "s-orphan", "worker-gone")
	expireLeaseForTest(t, env.db, core.LeaseRun, "run_orphan")
	env.seedPendingFor(t, "lrn_orphan", "run_orphan", 10, 12)
	key := us22RecoveryKey
	env.seedRun(t, "run_cursor", us22RecoveryCaller, &key)

	report, err := appNewReclaimer(t, env.db).ReclaimOnce(ctx, 32)
	require.NoError(t, err)
	require.Equal(t, 1, report.Runs, "A1 先收尾失联的 Run")
	require.Zero(t, report.Errors)

	require.Equal(t, string(core.StatusFailed), appRunStatus(t, env.db, "run_orphan"))

	status, verified := env.extractionVerdict(t, "lrn_orphan")
	require.Equal(t, "unknown", status, "没有结果证据判 unknown——回收器只查证、不重放")
	require.True(t, verified.Valid, "进程不重启也必须写出查证时刻")
	require.Greater(t, verified.Int64, baselineClock().UnixMilli(),
		"查证时刻来自回收器本轮时钟，不是历史夹具值")

	position, found := env.cursorAt(t, "run_cursor")
	require.True(t, found)
	require.Equal(t, int64(12), position, "查证把来源游标推进到冻结窗口末端")

	pending, err := env.store.UnresolvedForOrigin(ctx, us22RecoveryAgent, us22RecoverySession, "run_cursor")
	require.NoError(t, err)
	require.Nil(t, pending, "B3 查证后同来源新窗口不再被阻断（不重启）")
	require.Equal(t, 0, env.countRows(t, `SELECT count(*) FROM llm_calls`), "B3 不得调用模型")
}

// O3（US-24 T019 / R6）：扫描读到的预期尝试在写入时必须重检。扫描之后该行经合法重试
// 换手（`last_attempt_run_id` 从 RunA 变成持有效租约的 RunB），回收器不得把旧结论写
// 到新尝试上——必须 recovered=false、零写入、游标不动。
//
// 逆改点：`RecoverExtraction` 只看 `status` 与 `extraction_id`（去掉预期尝试 CAS）→
// 新尝试被写成 unknown 并推进游标，状态 / 预期值 / 游标三处断言红。
// The retry itself is staged through FailExtraction + BeginExtraction's unfenced entry:
// the fenced pair requires a live lease at failure time, which the scan cannot have just
// seen as expired; the contract under test (expected-attempt CAS on the write) is identical
// on both paths.
func TestReclaimer_OrphanExtractionSkipsLegitimateRetry(t *testing.T) {
	env := newUS22RecoveryEnv(t)
	ctx := t.Context()
	key := us22RecoveryKey

	// 原始 Run 行：BeginExtraction 从它复制来源身份。
	env.seedRun(t, "lrn_retry_run", us22RecoveryCaller, &key)
	leases := postgres.NewLeaseStore(env.db)
	// 扫描时看到的那次尝试（RunA）已过期——孤儿判据因此成立。
	_, err := leases.Acquire(ctx, core.LeaseRun, "run_retry_a",
		core.WorkerIdentity{InstanceID: "worker-a", Name: "a"}, time.Minute)
	require.NoError(t, err)
	expireLeaseForTest(t, env.db, core.LeaseRun, "run_retry_a")
	// 合法重试的新属主（RunB）持有有效租约。
	_, err = leases.Acquire(ctx, core.LeaseRun, "run_retry_b",
		core.WorkerIdentity{InstanceID: "worker-b", Name: "b"}, time.Minute)
	require.NoError(t, err)
	env.seedPendingFor(t, "lrn_retry", "run_retry_a", 1, 6)

	// 扫描之后、写入之前：行经 failed 被 RunB 合法重试回 pending。
	store := &us24PostScanRetryStore{LearningRepo: env.store, takeover: func() {
		require.NoError(t, env.store.FailExtraction(ctx, "lrn_retry", "", core.LeaseToken{}, "", 0,
			core.LearningFailureModel, "模型超时", baselineClock()))
		_, err := env.store.BeginExtraction(ctx, core.LearningExtraction{
			ExtractionID:        "lrn_retry_next",
			Agent:               us22RecoveryAgent,
			SessionID:           us22RecoverySession,
			RunID:               "lrn_retry_run",
			LastAttemptRunID:    "run_retry_b",
			WindowFromMessageID: 1,
			WindowToMessageID:   6,
			WindowMessages:      6,
			PromptVersion:       "memory-extract-v1",
			RuleVersion:         "memory-rules-v1",
			Provider:            "stub",
			Model:               "stub-model",
			LastAttemptAt:       baselineClock().Add(time.Minute),
		}, core.LeaseToken{})
		require.NoError(t, err)
	}}

	reclaimer, err := newLeaseReclaimer(reclaimerDeps{
		DB:       env.db,
		Flows:    &flow.Service{},
		Leases:   leases,
		Worker:   us24B3Identity(),
		LeaseTTL: time.Minute,
		Learning: store,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	report, err := reclaimer.ReclaimOnce(ctx, 32)
	require.NoError(t, err)
	require.Zero(t, report.Runs, "RunA 的 runs 行已终态，A1 不领取它")
	require.Zero(t, report.Errors)

	status, verified := env.extractionVerdict(t, "lrn_retry")
	require.Equal(t, "pending", status, "扫描后的合法重试不得被旧结论覆盖")
	require.False(t, verified.Valid, "跳过必须零写入：不得替新尝试写 verified_at")
	lastAttempt, attempts := env.extractionAttempt(t, "lrn_retry")
	require.Equal(t, "run_retry_b", lastAttempt, "行仍绑定合法重试的尝试")
	require.Equal(t, 2, attempts, "重试计数不得被回收器改写")

	_, found := env.cursorAt(t, "lrn_retry_run")
	require.False(t, found, "跳过时不得推进来源游标")
}

// O4（US-24 T019 / R7）：`NOT EXISTS` 只是快照读，拦不住并发续约；写入必须锁**租约行**
// 并在获锁后用数据库时间重判。属主的续约事务先把行锁住、尚未提交：回收器的写入排在它
// 后面；提交后看到推后的 `expires_at` → 本轮跳过（租约仍有效）；回滚后看到过期租约 →
// 照常查证。两个分支共同证明互斥来自行锁而不是扫描快照。
//
// 逆改点：把 `RecoverExtraction` 的租约行锁改成 `EXISTS` 快照读（不阻塞续约）→ 提交
// 分支里回收器会先返回并把活属主的行写成 unknown，断言红。
func TestReclaimer_OrphanExtractionWaitsForConcurrentRenewal(t *testing.T) {
	t.Run("renewal_commits_skips", func(t *testing.T) {
		env, rows, identity := us24RenewalScene(t)
		renewal := us24OpenRenewal(t, env, "run_renew", identity)
		defer func() { _ = renewal.Close() }()

		done := us24StartVerify(env, rows)
		us24RequireBlockedByRenewal(t, env, renewal.PID(), done)

		require.NoError(t, renewal.Commit())
		out := <-done
		require.NoError(t, out.err)
		require.Zero(t, out.result.Verified, "续约提交后租约仍有效：不得写查证结论")
		require.Equal(t, 1, out.result.Skipped)

		status, verified := env.extractionVerdict(t, "lrn_renew")
		require.Equal(t, "pending", status, "活属主的行保持 pending")
		require.False(t, verified.Valid)
		_, found := env.cursorAt(t, "run_renew_cursor")
		require.False(t, found, "跳过时不得推进来源游标")
	})

	t.Run("renewal_rolls_back_verifies", func(t *testing.T) {
		env, rows, identity := us24RenewalScene(t)
		renewal := us24OpenRenewal(t, env, "run_renew", identity)
		defer func() { _ = renewal.Close() }()

		done := us24StartVerify(env, rows)
		us24RequireBlockedByRenewal(t, env, renewal.PID(), done)

		require.NoError(t, renewal.Rollback())
		out := <-done
		require.NoError(t, out.err)
		require.Equal(t, 1, out.result.Verified, "续约回滚后租约仍过期：照常查证")
		require.Equal(t, 1, out.result.Unknown)

		status, verified := env.extractionVerdict(t, "lrn_renew")
		require.Equal(t, "unknown", status)
		require.True(t, verified.Valid)
		position, found := env.cursorAt(t, "run_renew_cursor")
		require.True(t, found)
		require.Equal(t, int64(7), position, "查证把来源游标推进到冻结窗口末端")
	})
}

// us24RenewalScene 准备"扫描已完成、属主续约在途"的现场：孤儿行已从扫描取出，续约事务
// 尚未开始。返回的 rows 就是扫描结果——写入侧只能凭它带出的预期值重检。
func us24RenewalScene(t *testing.T) (*us22RecoveryEnv, []core.LearningExtraction, core.WorkerIdentity) {
	t.Helper()
	env := newUS22RecoveryEnv(t)
	ctx := t.Context()
	identity := core.WorkerIdentity{InstanceID: "worker-owner", Name: "owner"}

	_, err := postgres.NewLeaseStore(env.db).Acquire(ctx, core.LeaseRun, "run_renew", identity, time.Minute)
	require.NoError(t, err)
	expireLeaseForTest(t, env.db, core.LeaseRun, "run_renew")
	env.seedPendingFor(t, "lrn_renew", "run_renew", 1, 7)
	key := us22RecoveryKey
	env.seedRun(t, "run_renew_cursor", us22RecoveryCaller, &key)

	// 扫描发生在续约提交之前：快照读到"已过期"，行入选（NOT EXISTS 挡不住续约）。
	rows, err := env.store.OrphanPendingExtractions(ctx, 8)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "run_renew", rows[0].LastAttemptRunID)
	return env, rows, identity
}

// us24Renewal 是一个持锁的续约事务：连接要留到用例结束，事务本身按分支提交或回滚。
// us24Renewal keeps the dedicated connection alive so the transaction can end either way.
type us24Renewal struct {
	conn *sql.Conn
	tx   *sql.Tx
	pid  int
}

// Commit 提交续约（租约被推后，属主仍然有效）。
func (r *us24Renewal) Commit() error { return r.tx.Commit() }

// Rollback 撤销续约（租约仍过期，属主已失联）。
func (r *us24Renewal) Rollback() error { return r.tx.Rollback() }

// Close 未结束的事务必须先回滚——`database/sql` 的连接关闭要等事务收尾，直接关会卡死。
// Close must end the transaction first: the connection close waits for the tx to finish.
func (r *us24Renewal) Close() error {
	_ = r.tx.Rollback() // 已提交 / 已回滚时返回 ErrTxDone，忽略即可
	return r.conn.Close()
}

// PID 是本续约连接的后端进程号，用于查"谁排在它后面"。
func (r *us24Renewal) PID() int { return r.pid }

// us24OpenRenewal 打开一个**未提交**的续约事务并持有租约行锁。语句与 `LeaseStore.Renew`
// 同形；唯一的差别是把"现在"取到 5 秒前——模拟"续约在到期前校验通过、随后才到期"的
// 在途事务。测试无法稳定地把墙钟卡在到期前后，因此把时间样本前移；被验的是回收器获锁
// 后的重判，不是续约自身的校验时序。
// us24OpenRenewal holds the lease row lock with an uncommitted renewal whose clock sample
// predates the expiry, the only state a deterministic test can hold open.
func us24OpenRenewal(t *testing.T, env *us22RecoveryEnv, resourceID string,
	identity core.WorkerIdentity,
) *us24Renewal {
	t.Helper()
	conn, err := env.raw.Conn(t.Context())
	require.NoError(t, err)

	tx, err := conn.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	res, err := tx.ExecContext(t.Context(), `
WITH now AS (SELECT ((extract(epoch from clock_timestamp()) * 1000)::bigint - 5000) AS ms)
UPDATE execution_leases l
   SET renewed_at = now.ms, expires_at = now.ms + $5
  FROM now
 WHERE l.kind = $1 AND l.resource_id = $2
   AND l.owner_worker_id = $3 AND l.ownership_epoch = $4
   AND l.released_at IS NULL AND l.expires_at > now.ms`,
		string(core.LeaseRun), resourceID, identity.InstanceID, int64(1), 60_000)
	require.NoError(t, err, "续约事务必须能命中同一行（持有行锁）")
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)

	var pid int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	return &us24Renewal{conn: conn, tx: tx, pid: pid}
}

// b3Outcome 是后台查证的结果。
type b3Outcome struct {
	result learningVerifyResult
	err    error
}

// us24StartVerify 在后台跑共享查证步骤；写入会阻塞在租约行上。
func us24StartVerify(env *us22RecoveryEnv, rows []core.LearningExtraction) <-chan b3Outcome {
	done := make(chan b3Outcome, 1)
	go func() {
		result, err := verifyPendingExtractions(context.Background(), env.store, rows,
			time.Now().UTC())
		done <- b3Outcome{result: result, err: err}
	}()
	return done
}

// us24RequireBlockedByRenewal 断言后台写入确实排在续约事务后面。读数用
// `pg_blocking_pids(pid)` 而不是猜锁类型：行被未提交事务更新时，后到的 `FOR UPDATE`
// 等的是**更新事务号**（locktype=transactionid，没有 relation），按表名过滤会漏掉。
// 带类型参数是因为 B3 查证（b3Outcome）与 B1 终态残留（residueOutcome）各有自己的
// 结果类型；判据只关心"它还没返回"。
// us24RequireBlockedByRenewal reads pg_blocking_pids because a FOR UPDATE against a row
// updated by an open transaction waits on the transaction id, not a relation lock.
func us24RequireBlockedByRenewal[T any](t *testing.T, env *us22RecoveryEnv, renewalPid int,
	done <-chan T,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		require.NoError(t, env.raw.QueryRowContext(t.Context(), `
SELECT count(*) FROM pg_stat_activity a
 WHERE $1 = ANY (pg_blocking_pids(a.pid))`, renewalPid).Scan(&blocked))
		if blocked > 0 {
			return
		}
		select {
		case out := <-done:
			t.Fatalf("写入在续约提交前就返回了（没有排在续约后面）：%+v", out)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("5s 内没有观察到排在续约事务后面的后台写入")
}

// us24B3Identity 是 B3 用例共用的回收器身份。
func us24B3Identity() core.WorkerIdentity {
	return core.WorkerIdentity{
		InstanceID:    "worker-b3",
		Name:          "b3",
		Version:       "test",
		SchemaVersion: postgres.SupportedSchemaVersion,
	}
}

// us24PostScanRetryStore 在共享查证步骤的"扫描 → 写入"之间插入一次合法重试；
// 其余方法由内嵌的生产仓储原样承担。
// us24PostScanRetryStore injects the legitimate retry between the scan and the write.
type us24PostScanRetryStore struct {
	*postgres.LearningRepo
	takeover func()
}

func (s *us24PostScanRetryStore) OrphanPendingExtractions(ctx context.Context, limit int) (
	[]core.LearningExtraction, error,
) {
	rows, err := s.LearningRepo.OrphanPendingExtractions(ctx, limit)
	if err == nil && len(rows) > 0 {
		s.takeover()
	}
	return rows, err
}

// us22RecoveryRecordingStore 在只读替身之上记录调用顺序与查证写入；其余接口方法由
// 内嵌替身以零值满足——若恢复意外调用它们，调用形状断言会先红。
// The recording store embeds the read-only fake and records the recovery call shape.
type us22RecoveryRecordingStore struct {
	fakeLearningAdminStore
	pending    []core.LearningExtraction
	candidates map[string]int
	drafts     map[string]int
	calls      []string
	recovered  map[string]core.LearningExtractionState
	verifiedAt map[string]time.Time
	expected   map[string]string
}

func (s *us22RecoveryRecordingStore) PendingExtractions(context.Context) ([]core.LearningExtraction, error) {
	s.calls = append(s.calls, "pending")
	return s.pending, nil
}

func (s *us22RecoveryRecordingStore) ExtractionCandidateCount(_ context.Context, extractionID string) (int, error) {
	s.calls = append(s.calls, "candidates:"+extractionID)
	return s.candidates[extractionID], nil
}

func (s *us22RecoveryRecordingStore) ExtractionDraftCount(_ context.Context, extractionID string) (int, error) {
	s.calls = append(s.calls, "drafts:"+extractionID)
	return s.drafts[extractionID], nil
}

func (s *us22RecoveryRecordingStore) RecoverExtraction(_ context.Context,
	req core.RecoverLearningExtraction,
) (bool, error) {
	s.calls = append(s.calls, "recover:"+req.ExtractionID+":"+string(req.Verdict))
	s.recovered[req.ExtractionID] = req.Verdict
	s.verifiedAt[req.ExtractionID] = req.VerifiedAt
	s.expected[req.ExtractionID] = req.ExpectedLastAttemptRunID
	return true, nil
}

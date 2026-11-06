//go:build integration

package app

// 本文件是 US-22 T024 的启动恢复验收（颗粒度文档 §2.3.6 / §4.3 ⑤ / C-13 / C-14 / C-15）：
//
//	R1 `TestUS22_Recovery_CandidatesMeanSuccess`——已有候选元数据判 success，游标推进
//	R2 `TestUS22_Recovery_DraftsMeanSuccess`——只有草案行同样判 success，草案行保留
//	R3 `TestUS22_Recovery_UnknownAdvancesNoReplay`——无结果判 unknown；游标推进到冻结窗口
//	   末端；不重放（零新抽取行、零模型调用）
//	R4 `TestUS22_Recovery_UnknownNotRewritten`——已查证的 unknown 不被下一次启动补成
//	   success（不改口），`verified_at` 保持首次读数
//	R5 `TestUS22_Recovery_NoReplay`——恢复只调用「扫描 → 计数 → 写结论」四个只读查询与
//	   一个查证写方法；时钟只取一次；端口缺失拒绝启动
//	R6 `TestUS22_Recovery_RecoveredUnknownUnblocks`——恢复前同来源新窗口被阻断，恢复后
//	   解除（`UnresolvedForOrigin` 归空、`BeginExtraction` 建新行）
//	R7 `TestUS22_Unknown_NoRestartNextRunNoReplay`——提交未知且**不重启**时，同来源下一
//	   Run 零写入零调用（B-08）
//
// 为什么 R1–R4 / R6–R7 走真实 PostgreSQL：查证结论、`verified_at` 与游标推进都写在
// 存储层的单事务里（`RecoverExtraction`），用内存替身只会变成"替身替我做了判断"。
// R5 用假存储，因为它判的是**装配层调用形状**——恢复经手的写方法只能有一个，且根本没有
// 抽取器可调（"不重放"在依赖面里就表达不出来）。
// The real-database cases assert the verdict, verified_at and cursor the store writes in
// one transaction; the fake-store case asserts the assembly's call shape and clock usage.

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

// seedPending 落一条 pending 抽取行（提交结果未知的现场）。
// seedPending inserts a pending extraction row — the "outcome unknown" scene.
func (e *us22RecoveryEnv) seedPending(t *testing.T, extractionID string, from, to int64) {
	t.Helper()
	at := baselineClock().UnixMilli()
	_, err := e.raw.ExecContext(t.Context(), `
		INSERT INTO memory_extractions
			(extraction_id, agent_name, session_id, run_id, last_attempt_run_id,
			 origin_caller_id, origin_key_id, status, window_from_message_id, window_to_message_id,
			 window_messages, prompt_version, rule_version, provider, model,
			 created_at, last_attempt_at)
		VALUES ($1, $2, $3, $1 || '_run', $1 || '_run', $4, $5, 'pending', $6, $7, $8,
		        'memory-extract-v1', 'memory-rules-v1', 'stub', 'stub-model', $9, $9)`,
		extractionID, us22RecoveryAgent, us22RecoverySession, us22RecoveryCaller,
		us22RecoveryKey, from, to, int(to-from+1), at)
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
// 时钟只取一次；缺少查证端口直接拒绝启动（不静默跳过）。
// 逆改点：恢复调用任何抽取入口 / 每行重取时钟 / nil 端口静默返回 → 对应断言红。
func TestUS22_Recovery_NoReplay(t *testing.T) {
	fixed := time.Date(2026, 11, 6, 8, 30, 0, 0, time.UTC)
	clockCalls := 0
	store := &us22RecoveryRecordingStore{
		pending: []core.LearningExtraction{
			{ExtractionID: "lrn_a", Agent: us22RecoveryAgent, WindowToMessageID: 7},
			{ExtractionID: "lrn_b", Agent: us22RecoveryAgent, WindowToMessageID: 9},
		},
		candidates: map[string]int{"lrn_a": 0, "lrn_b": 0},
		drafts:     map[string]int{"lrn_a": 1, "lrn_b": 0},
		recovered:  map[string]core.LearningExtractionState{},
		verifiedAt: map[string]time.Time{},
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
	})
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
	})
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
	})
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

func (s *us22RecoveryRecordingStore) RecoverExtraction(_ context.Context, extractionID string,
	status core.LearningExtractionState, verifiedAt time.Time,
) error {
	s.calls = append(s.calls, "recover:"+extractionID+":"+string(status))
	s.recovered[extractionID] = status
	s.verifiedAt[extractionID] = verifiedAt
	return nil
}

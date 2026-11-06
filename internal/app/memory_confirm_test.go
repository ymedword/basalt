//go:build integration

package app

// 本文件是 US-22 自动候选确认入口的验收面（颗粒度文档 §3.1.8 / C-16 / C-17 / C-28，T014）。
//
// 二十一个具名用例分成四组，各组守一条不可退让的边界：
//  1. **唯一晋升路径**：自动候选（`source='auto_extract'` 或存在候选元数据）无论 body
//     是否为空都走统一门禁；空 body 只等于 keep_both。显式提议候选保持旧 `Confirm`
//     路径与回执不变。
//  2. **门禁逐项**：候选到期、替代目标与建议不一致 / 不再 active / 已过期、直接来源
//     失效或到期、来源 Key 撤销 / 到期、来源调用方停用——每一项各有一只反例，且都断言
//     "状态不变"。
//  3. **审计纪律**：预写在事务之前；事务失败回填 `failed` 而不是留 `pending`；明细不含
//     正文与证据引文；预写失败**不得**改条目。
//  4. **并发与补索引**：并发确认只有一个赢家；并发撤销使确认 fail-closed；晋升后补索引
//     失败只 WARN，条目保持 active。
//
// Every gate has its own counterexample and every refusal asserts "state unchanged"; the
// audit is written before the transaction and never carries the body.
//
// 用例正文里的「逆改点」给出让该用例变红的实现改动；写不出逆改点的断言等于没判。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// memoryConfirmEnv 在 management-surface 夹具上加两样东西：钉住 search_path 的 DSN
// （并发用例要另开裸事务持锁）与可后绑定的向量索引槽位。
// memoryConfirmEnv adds the pinned DSN (for lock-holding raw transactions) and a
// late-bindable indexing slot to the management-surface fixture.
type memoryConfirmEnv struct {
	*memoryAdminEnv
	dsn  string
	raw  *sql.DB
	slot *memoryIndexSlot
	seq  int
}

// newMemoryConfirmEnv 建隔离 schema、种调用方 c1，并按生产装配装上索引槽位。
func newMemoryConfirmEnv(t *testing.T) *memoryConfirmEnv {
	t.Helper()
	fixture := pgtest.NewFixture(t)
	ops := postgres.NewManagementStore(fixture.DB)
	store := postgres.NewMemoryStore(fixture.DB, baselineClock)
	logger, logs := newCapturedLogger()
	slot := newMemoryIndexSlot(logger)
	env := &memoryConfirmEnv{
		memoryAdminEnv: &memoryAdminEnv{db: fixture.DB, store: store, ops: ops, logger: logger, logs: logs},
		dsn:            fixture.DSN,
		raw:            openRawSQL(t, fixture.DSN),
		slot:           slot,
	}
	env.admin = NewMemoryAdmin(MemoryAdminDeps{
		Store:      store,
		Operations: ops,
		Index:      slot,
		Logger:     logger,
		Now:        baselineClock,
	})
	env.seedCaller(t, "c1", "active")
	return env
}

// openRawSQL 开一条**独立于被测句柄**的连接：只用来持锁与改列，判据仍读被测句柄。
// openRawSQL opens an independent handle used only to hold locks and flip columns.
func openRawSQL(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	handle, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, handle.Close()) })
	return handle
}

// seedCaller 插 / 改一个调用方状态；已有行只改状态。
func (env *memoryConfirmEnv) seedCaller(t *testing.T, callerID, status string) {
	t.Helper()
	_, err := env.raw.ExecContext(context.Background(), `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, '', $2, $3)
		ON CONFLICT (caller_id) DO UPDATE SET status = EXCLUDED.status`,
		callerID, status, baselineClock().UnixMilli())
	require.NoError(t, err)
}

// seedKey 插一把调用方 Key；expiresAt / revokedAt 传 nil 表示未设。
func (env *memoryConfirmEnv) seedKey(t *testing.T, keyID, callerID string, expiresAt, revokedAt any) {
	t.Helper()
	_, err := env.raw.ExecContext(context.Background(), `
		INSERT INTO caller_keys
			(key_id, caller_id, key_hash, key_prefix, scopes, expires_at, revoked_at, created_at)
		VALUES ($1, $2, $3, $4, '{}', $5, $6, $7)`,
		keyID, callerID, "hash-"+keyID, "bk_"+keyID, expiresAt, revokedAt, baselineClock().UnixMilli())
	require.NoError(t, err)
}

// seedExtraction 插一条成功的抽取行（候选元数据的外键目标）。
func (env *memoryConfirmEnv) seedExtraction(t *testing.T, callerID, keyID string) string {
	t.Helper()
	env.seq++
	extractionID := fmt.Sprintf("lrn_confirm_%04d", env.seq)
	at := baselineClock().UnixMilli()
	_, err := env.raw.ExecContext(context.Background(), `
		INSERT INTO memory_extractions
			(extraction_id, agent_name, session_id, run_id, last_attempt_run_id,
			 origin_caller_id, origin_key_id, status, window_from_message_id, window_to_message_id,
			 window_messages, prompt_version, rule_version, provider, model, created_at, last_attempt_at)
		VALUES ($1, 'ops', 'sess_confirm', $1 || '_run', $1 || '_run', $2, $3, 'success',
		        1, 2, 2, 'mv1', 'rv1', 'stub', 'stub-model', $4, $4)`,
		extractionID, callerID, keyID, at)
	require.NoError(t, err)
	return extractionID
}

// confirmSpec 描述一个自动候选夹具；零值按最宽松的合法形状补齐。
type confirmSpec struct {
	content   string
	sourceIDs []int64
	suggested int64
	callerID  string
	keyID     string
	validTo   any // memory_candidate_meta.valid_to
}

// seedAutoCandidate 落一条自动候选：条目（candidate / auto_extract）+ 元数据 + 来源关系。
// 元数据证据用正文逐字引文，验证明细脱敏时以它作"不得出现"的探针。
func (env *memoryConfirmEnv) seedAutoCandidate(t *testing.T, spec confirmSpec) int64 {
	t.Helper()
	callerID := spec.callerID
	if callerID == "" {
		callerID = "c1"
	}
	extractionID := env.seedExtraction(t, callerID, spec.keyID)
	entryID, err := env.store.InsertEntry(context.Background(), memory.WithSource(memory.MemoryEntry{
		Scope:   core.Scope{Agent: "ops"},
		Section: core.SectionArchive,
		Content: spec.content,
		State:   memory.StateCandidate,
	}, memory.SourceAutoExtract))
	require.NoError(t, err)

	quoted := spec.content
	evidenceAt := baselineClock().Add(-time.Hour).UTC()
	evidence, err := json.Marshal([]map[string]any{{
		"message_id": 21,
		"run_id":     extractionID + "_run",
		"role":       "user",
		"quote":      quoted,
		"message_at": evidenceAt.Format(time.RFC3339Nano),
	}})
	require.NoError(t, err)

	sourceIDs := spec.sourceIDs
	if sourceIDs == nil {
		sourceIDs = []int64{}
	}
	var suggestedArg any
	if spec.suggested > 0 {
		suggestedArg = spec.suggested
	}
	_, err = env.raw.ExecContext(context.Background(), `
		INSERT INTO memory_candidate_meta
			(entry_id, extraction_id, kind, certainty, confidence, evidence_json, source_entry_ids,
			 suggested_supersedes_id, origin_caller_id, origin_key_id, evidence_at, valid_to,
			 time_confidence, created_at)
		VALUES ($1, $2, 'fact', 'asserted', 0.9, $3::jsonb, $4, $5, $6, $7, $8, $9, 'exact', $10)`,
		entryID, extractionID, string(evidence), sourceIDs, suggestedArg, callerID, spec.keyID,
		evidenceAt.UnixMilli(), spec.validTo, baselineClock().UnixMilli())
	require.NoError(t, err)
	for _, sourceID := range sourceIDs {
		_, err := env.raw.ExecContext(context.Background(), `
			INSERT INTO memory_candidate_sources (entry_id, source_entry_id, extraction_id, created_at)
			VALUES ($1, $2, $3, $4)`, entryID, sourceID, extractionID, baselineClock().UnixMilli())
		require.NoError(t, err)
	}
	return entryID
}

// setEntryExpiry 把条目改成"已到期"：valid_from 必须早于 valid_to（schema CHECK），
// 而 valid_to 又必须不晚于夹具时钟——所以两列一起改。
func (env *memoryConfirmEnv) setEntryExpiry(t *testing.T, entryID int64) {
	t.Helper()
	_, err := env.raw.ExecContext(context.Background(), `
		UPDATE memory_entries SET valid_from = $2, valid_to = $3 WHERE id = $1`,
		entryID, baselineClock().Add(-48*time.Hour).UnixMilli(), baselineClock().Add(-time.Hour).UnixMilli())
	require.NoError(t, err)
}

// setEntryState 直接改状态（生命周期入口不产出的终态由夹具造）。
func (env *memoryConfirmEnv) setEntryState(t *testing.T, entryID int64, state string) {
	t.Helper()
	_, err := env.raw.ExecContext(context.Background(),
		`UPDATE memory_entries SET state = $2 WHERE id = $1`, entryID, state)
	require.NoError(t, err)
}

// confirmOperations 取 `memory.confirm` 的全部审计行（按动作过滤，不看别的动作）。
func confirmOperations(t *testing.T, env *memoryConfirmEnv) []core.OperationRecord {
	t.Helper()
	var rows []core.OperationRecord
	for _, row := range listConsoleOperations(t, env.ops) {
		if row.Action == core.ActionMemoryConfirm {
			rows = append(rows, row)
		}
	}
	return rows
}

// requireConfirmOp 断言唯一的 `memory.confirm` 行处于给定终态且回填了完成时刻。
func requireConfirmOp(t *testing.T, env *memoryConfirmEnv, status core.OperationStatus, code string) core.OperationRecord {
	t.Helper()
	rows := confirmOperations(t, env)
	require.Len(t, rows, 1, "确认动作应恰有一行审计")
	row := rows[0]
	require.Equal(t, status, row.Status)
	require.Equal(t, code, row.ResultCode)
	require.False(t, row.FinishedAt.IsZero(), "终态必须回填完成时刻，不得留 pending")
	return row
}

// requireStillCandidate 断言拒绝之后候选仍是 candidate——"状态不变"是每个门禁反例的
// 共同下半句。
func requireStillCandidate(t *testing.T, env *memoryConfirmEnv, entryID int64) {
	t.Helper()
	require.Equal(t, memory.StateCandidate, env.entry(t, entryID).State)
}

// TestUS22_Confirm_AutoCandidateSameGate 判自动候选**不因空 body 绕过新门禁**：来源 Key
// 已撤销时，空处置的确认必须 409。
//
// 逆改点：空 body 回落旧 `Confirm` → 撤销来源的候选被晋升为 active，双断言（错误 + 状态）红。
func TestUS22_Confirm_AutoCandidateSameGate(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	env.seedKey(t, "key_revoked", "c1", nil, baselineClock().Add(-time.Minute).UnixMilli())
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "季度报表在每月 5 号前提交", keyID: "key_revoked"})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.ErrorIs(t, err, core.ErrConflict, "撤销来源的自动候选即使空处置也必须拒绝")
	requireStillCandidate(t, env, candidateID)
	requireConfirmOp(t, env, core.OperationFailed, memoryAdminResultConflict)
}

// TestUS22_Confirm_EmptyBodyRunsGates 判空 body 仍执行**全部**门禁：直接来源已删除时，
// 空处置的确认在预写之前就被拒绝，连审计行都不留。
//
// 逆改点：把 `""` 当作"走旧 Confirm"→ 失效来源的候选被晋升，错误与状态断言红；把来源
// 检查挪到预写之后 → 审计行数断言红。
func TestUS22_Confirm_EmptyBodyRunsGates(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	sourceID := env.seedEntry(t, core.Scope{Agent: "ops"}, core.SectionArchive, memory.StateActive, "来源条目")
	env.setEntryState(t, sourceID, memory.StateDeleted)
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "来源已删除的候选", sourceIDs: []int64{sourceID}})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.ErrorIs(t, err, core.ErrConflict)
	requireStillCandidate(t, env, candidateID)
	require.Empty(t, confirmOperations(t, env), "纯读门禁失败不得预写审计行")
}

// TestUS22_Confirm_MixedResolutionRejected 守契约 §7.1：`keep_both` 携带替代目标 =
// 两种处置被拼在一起 → 409，且在预写之前拒绝（不留操作行）。
//
// 逆改点：按入参错误（400 `InvalidInputError`）拒绝 → ErrorIs(core.ErrConflict) 红；
// 把处置校验挪到预写之后 → 操作行断言红。
func TestUS22_Confirm_MixedResolutionRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "混合处置的自动候选"})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{
			EntryID:           candidateID,
			Resolution:        memory.ResolutionKeepBoth,
			SupersedesEntryID: 42,
		})
	require.ErrorIs(t, err, core.ErrConflict, "两种处置拼在一起按状态冲突拒绝（契约 §7.1）")
	requireStillCandidate(t, env, candidateID)
	require.Empty(t, confirmOperations(t, env), "处置校验失败不得预写审计行")
}

// TestUS22_Confirm_ManualPathUnchanged 判显式提议候选保持旧确认路径：主键走旧 `Confirm`、
// 回执没有替代指针、审计明细不出现新键（omitempty 保证旧 JSON 字节不变）；携带的处置
// 对旧条目无效——它没有候选元数据可查证。
//
// 逆改点：让显式候选也走确认事务 → 缺元数据被拒，成功断言红；给旧路径的 detail 强塞
// `resolution` → 键缺失断言红。
func TestUS22_Confirm_ManualPathUnchanged(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	manualID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateCandidate, "手工提议的记忆")
	targetID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateActive, "另一条手工记忆")

	result, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: manualID})
	require.NoError(t, err)
	require.Equal(t, manualID, result.EntryID)
	require.Equal(t, memory.StateActive, result.State)
	require.Zero(t, result.SupersededID, "旧路径的回执没有替代指针")
	require.Equal(t, memory.StateActive, env.entry(t, manualID).State)
	require.Equal(t, consoleActor().Subject, env.confirmedBy(t, manualID))

	row := requireConfirmOp(t, env, core.OperationSuccess, memoryAdminResultOK)
	detail := detailMap(t, row.DetailJSON)
	require.EqualValues(t, manualID, detail["entry_id"])
	require.NotContains(t, detail, "resolution", "旧路径的明细不追加新键")
	require.NotContains(t, detail, "supersedes_id")

	// 手工候选即使带了 supersede 处置，也按旧路径确认——目标不动。
	secondID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateCandidate, "第二条手工候选")
	_, err = env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: secondID, Resolution: memory.ResolutionSupersede, SupersedesEntryID: targetID})
	require.NoError(t, err)
	require.Equal(t, memory.StateActive, env.entry(t, secondID).State)
	require.Equal(t, memory.StateActive, env.entry(t, targetID).State, "旧路径不执行替代")
}

// TestUS22_Confirm_KeepBothDefault 判空处置的默认语义：keep_both、回执零替代指针、审计
// 明细写明处置与确认后的状态。
//
// 逆改点：把空处置写成别的字面量 → 明细断言红；把回执状态硬写成 candidate → 回执断言红。
func TestUS22_Confirm_KeepBothDefault(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "成本口径以财务系统为准"})

	result, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.NoError(t, err)
	require.Equal(t, candidateID, result.EntryID)
	require.Equal(t, memory.StateActive, result.State)
	require.Zero(t, result.SupersededID)

	require.Equal(t, memory.StateActive, env.entry(t, candidateID).State)
	require.Equal(t, consoleActor().Subject, env.confirmedBy(t, candidateID))

	row := requireConfirmOp(t, env, core.OperationSuccess, memoryAdminResultOK)
	detail := detailMap(t, row.DetailJSON)
	require.Equal(t, memory.ResolutionKeepBoth, detail["resolution"])
	require.EqualValues(t, candidateID, detail["entry_id"])
	require.Equal(t, memory.StateCandidate, detail["state_before"])
	require.Equal(t, memory.StateActive, detail["state_after"])
	require.NotContains(t, detail, "supersedes_id", "keep_both 不写替代指针（omitempty）")
}

// TestUS22_Confirm_SupersedeReplacesActive 判替代的完整后果：旧目标退役、候选晋升并记下
// 替代指针、回执带 `superseded_id`、审计明细同值。
//
// 逆改点：只改候选不改目标 → 目标状态断言红；回执漏 `SupersededID` → 回执断言红。
func TestUS22_Confirm_SupersedeReplacesActive(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	targetID := env.seedEntry(t, core.Scope{Agent: "ops"}, core.SectionArchive, memory.StateActive, "旧口径：每月 3 号")
	candidateID := env.seedAutoCandidate(t, confirmSpec{
		content: "新口径：每月 5 号", suggested: targetID, callerID: "c1",
	})

	result, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID, Resolution: memory.ResolutionSupersede, SupersedesEntryID: targetID})
	require.NoError(t, err)
	require.Equal(t, targetID, result.SupersededID)

	candidate := env.entry(t, candidateID)
	require.Equal(t, memory.StateActive, candidate.State)
	require.Equal(t, targetID, candidate.SupersedesID)
	require.Equal(t, memory.StateSuperseded, env.entry(t, targetID).State)

	row := requireConfirmOp(t, env, core.OperationSuccess, memoryAdminResultOK)
	detail := detailMap(t, row.DetailJSON)
	require.Equal(t, memory.ResolutionSupersede, detail["resolution"])
	require.EqualValues(t, targetID, detail["supersedes_id"])
	require.EqualValues(t, candidateID, detail["entry_id"], "entry_id 指向被晋升的候选，supersedes_id 才指向退役目标")
}

// TestUS22_Confirm_SupersedeMismatchRejected 判"替代必须与建议逐字相等"：请求指向另一个
// active 行时拒绝，且两行都不动、不留审计（纯读门禁在预写之前）。
//
// 逆改点：信任请求里的 `SupersedesEntryID` 而不比对元数据 → 错误为空，用例红。
func TestUS22_Confirm_SupersedeMismatchRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	suggestedID := env.seedEntry(t, core.Scope{Agent: "ops"}, core.SectionArchive, memory.StateActive, "建议目标")
	otherID := env.seedEntry(t, core.Scope{Agent: "ops"}, core.SectionArchive, memory.StateActive, "请求目标")
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "建议指向 A 的候选", suggested: suggestedID})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID, Resolution: memory.ResolutionSupersede, SupersedesEntryID: otherID})
	require.ErrorIs(t, err, core.ErrConflict, "请求目标与候选建议不一致必须拒绝")
	requireStillCandidate(t, env, candidateID)
	require.Equal(t, memory.StateActive, env.entry(t, suggestedID).State, "建议目标不得被误替代")
	require.Equal(t, memory.StateActive, env.entry(t, otherID).State, "请求目标不得被误替代")
	require.Empty(t, confirmOperations(t, env))
}

// TestUS22_Confirm_StaleSuggestionRejected 判旧建议不得覆盖新纠正：建议目标已被纠正为
// `corrected` 时，即便请求与建议一致也必须拒绝。
//
// 逆改点：只比对建议 ID 不查目标状态 → 已纠正条目被再次替代，用例红。
func TestUS22_Confirm_StaleSuggestionRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	targetID := env.seedEntry(t, core.Scope{Agent: "ops"}, core.SectionArchive, memory.StateActive, "后被纠正的条目")
	env.setEntryState(t, targetID, memory.StateCorrected)
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "指向已纠正条目的候选", suggested: targetID})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID, Resolution: memory.ResolutionSupersede, SupersedesEntryID: targetID})
	require.ErrorIs(t, err, core.ErrConflict)
	requireStillCandidate(t, env, candidateID)
	require.Equal(t, memory.StateCorrected, env.entry(t, targetID).State, "拒绝不得改写目标")
}

// TestUS22_Confirm_TargetNotActiveRejected 判已删除的建议目标不得被再次替代。
//
// 逆改点：不判目标状态 → 删除目标被写成 superseded，状态断言红。
func TestUS22_Confirm_TargetNotActiveRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	targetID := env.seedEntry(t, core.Scope{Agent: "ops"}, core.SectionArchive, memory.StateActive, "已被删除的条目")
	env.setEntryState(t, targetID, memory.StateDeleted)
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "指向已删除条目的候选", suggested: targetID})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID, Resolution: memory.ResolutionSupersede, SupersedesEntryID: targetID})
	require.ErrorIs(t, err, core.ErrConflict)
	requireStillCandidate(t, env, candidateID)
	require.Equal(t, memory.StateDeleted, env.entry(t, targetID).State)
}

// TestUS22_Confirm_SourceInvalidatedRejected 判直接来源被删除后候选不得晋升（派生条目
// 的来源闭包失效）。
//
// 逆改点：不查来源状态 → 来源已删的候选被晋升，错误与状态断言红。
func TestUS22_Confirm_SourceInvalidatedRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	sourceID := env.seedEntry(t, core.Scope{Agent: "ops"}, core.SectionArchive, memory.StateActive, "被删除的来源")
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "依赖已删来源的候选", sourceIDs: []int64{sourceID}})
	env.setEntryState(t, sourceID, memory.StateDeleted)

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.ErrorIs(t, err, core.ErrConflict)
	requireStillCandidate(t, env, candidateID)
}

// TestUS22_Confirm_SourceEntryExpiredRejected 判来源**到期**（仍是 active）也拒绝：到期
// 只有事务内可见（条目读模型不带 valid_to），所以这是"预写之后失败回填 failed"的样本。
//
// 逆改点：事务里不判来源 `valid_to` → 过期来源的候选被晋升，用例红。
func TestUS22_Confirm_SourceEntryExpiredRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	sourceID := env.seedEntry(t, core.Scope{Agent: "ops"}, core.SectionArchive, memory.StateActive, "已到期的来源")
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "依赖到期来源的候选", sourceIDs: []int64{sourceID}})
	env.setEntryExpiry(t, sourceID)

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.ErrorIs(t, err, core.ErrConflict)
	requireStillCandidate(t, env, candidateID)
	requireConfirmOp(t, env, core.OperationFailed, memoryAdminResultConflict)
}

// TestUS22_Confirm_OriginRevokedRejected 判来源 Key 撤销后不得晋升（权限复核在事务内）。
//
// 逆改点：不查 Key 状态 → 撤权候选被晋升，用例红。
func TestUS22_Confirm_OriginRevokedRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	env.seedKey(t, "key_revoked", "c1", nil, baselineClock().Add(-time.Minute).UnixMilli())
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "来自已撤销 Key 的候选", keyID: "key_revoked"})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.ErrorIs(t, err, core.ErrConflict)
	requireStillCandidate(t, env, candidateID)
	requireConfirmOp(t, env, core.OperationFailed, memoryAdminResultConflict)
}

// TestUS22_Confirm_KeyExpiredRejected 判来源 Key 到期（未撤销）同样拒绝。
//
// 逆改点：只判 `revoked_at IS NULL` 不判 `expires_at` → 到期 Key 的候选被晋升，用例红。
func TestUS22_Confirm_KeyExpiredRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	env.seedKey(t, "key_expired", "c1", baselineClock().Add(-time.Minute).UnixMilli(), nil)
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "来自已到期 Key 的候选", keyID: "key_expired"})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.ErrorIs(t, err, core.ErrConflict)
	requireStillCandidate(t, env, candidateID)
}

// TestUS22_Confirm_CallerDisabledRejected 判来源调用方停用后不得晋升：只查 Key 不查
// `callers.status` 会把停用调用方的内容放进记忆。
//
// 逆改点：去掉 consumers 状态查询 → 停用调用方的候选被晋升，用例红。
func TestUS22_Confirm_CallerDisabledRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	env.seedCaller(t, "c1", "disabled")
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "来自已停用调用方的候选"})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.ErrorIs(t, err, core.ErrConflict)
	requireStillCandidate(t, env, candidateID)
	requireConfirmOp(t, env, core.OperationFailed, memoryAdminResultConflict)
}

// TestUS22_Confirm_ExpiredRejected 判到期候选不得复活，且拒绝发生在预写之前（无审计行）。
//
// 逆改点：不判候选 `valid_to` → 过期候选被晋升；把判据挪到预写之后 → 审计行数断言红。
func TestUS22_Confirm_ExpiredRejected(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	candidateID := env.seedAutoCandidate(t, confirmSpec{
		content: "已到期的候选", validTo: baselineClock().Add(-time.Minute).UnixMilli(),
	})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.ErrorIs(t, err, core.ErrConflict)
	requireStillCandidate(t, env, candidateID)
	require.Empty(t, confirmOperations(t, env), "到期拒绝在预写之前，不得留审计行")
}

// TestUS22_Confirm_ConcurrentOneWinner 判并发确认同一候选只有一个赢家：另一个拿到 409，
// 条目只晋升一次，审计一成一败。
//
// 逆改点：去掉事务内候选行锁与 CAS → 两个都成功（或都写 supersedes），赢家数断言红。
func TestUS22_Confirm_ConcurrentOneWinner(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "并发确认的候选"})

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []error
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
				core.MemoryConfirmRequest{EntryID: candidateID})
			mu.Lock()
			results = append(results, err)
			mu.Unlock()
		}()
	}
	wg.Wait()

	successes, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case requireErrIsConflict(t, err):
			conflicts++
		}
	}
	require.Equal(t, 1, successes, "并发确认只有一个赢家")
	require.Equal(t, 1, conflicts, "输家必须拿到冲突而不是静默成功")
	require.Equal(t, memory.StateActive, env.entry(t, candidateID).State)

	rows := confirmOperations(t, env)
	require.Len(t, rows, 2)
	statuses := map[core.OperationStatus]int{}
	for _, row := range rows {
		statuses[row.Status]++
	}
	require.Equal(t, 1, statuses[core.OperationSuccess])
	require.Equal(t, 1, statuses[core.OperationFailed])
}

// requireErrIsConflict 是并发分支里的 `errors.Is`（断言失败要在测试 goroutine 之外报）。
func requireErrIsConflict(t *testing.T, err error) bool {
	t.Helper()
	require.ErrorIs(t, err, core.ErrConflict)
	return true
}

// TestUS22_Confirm_ConcurrentRevokeFailsClosed 判撤销与确认并发时只有一个结局：撤销事务
// 先持锁未提交，确认必须等在来源 Key 的 `FOR SHARE` 上；撤销提交后确认 409。
//
// 逆改点：来源 Key 用普通 SELECT（不取锁）→ 确认在撤销提交前就成功，阻塞断言红；
// 不查 Key → 撤销提交后仍成功，冲突断言红。
func TestUS22_Confirm_ConcurrentRevokeFailsClosed(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	env.seedKey(t, "key_race", "c1", nil, nil)
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "撤销与确认并发的候选", keyID: "key_race"})
	ctx := context.Background()

	revoke, err := env.raw.BeginTx(ctx, nil)
	require.NoError(t, err)
	revoked := false
	defer func() {
		if !revoked {
			_ = revoke.Rollback()
		}
	}()
	_, err = revoke.ExecContext(ctx,
		`UPDATE caller_keys SET revoked_at = $2 WHERE key_id = $1`,
		"key_race", baselineClock().UnixMilli())
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, confirmErr := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
			core.MemoryConfirmRequest{EntryID: candidateID})
		done <- confirmErr
	}()

	select {
	case confirmErr := <-done:
		t.Fatalf("撤销事务未提交时确认不得完成（err=%v）——来源 Key 检查没有取行锁", confirmErr)
	case <-time.After(300 * time.Millisecond):
		// 确认正等在来源 Key 的行锁上。
	}
	require.NoError(t, revoke.Commit())
	revoked = true

	select {
	case confirmErr := <-done:
		require.ErrorIs(t, confirmErr, core.ErrConflict, "撤销先完成则确认必须 fail-closed")
	case <-time.After(5 * time.Second):
		t.Fatal("撤销提交后确认仍未返回")
	}
	requireStillCandidate(t, env, candidateID)
}

// TestUS22_Confirm_AuditPreWrite 判审计先于事务：成功路径一行 pending→success；事务内
// 才可见的失败（Key 撤销）也必须留下 failed 行——没有预写就不可能有这行。
//
// 逆改点：把 `begin` 挪到确认事务之后 → 失败样本没有审计行，行数断言红。
func TestUS22_Confirm_AuditPreWrite(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	successID := env.seedAutoCandidate(t, confirmSpec{content: "成功确认的记忆"})
	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: successID})
	require.NoError(t, err)

	env.seedKey(t, "key_revoked", "c1", nil, baselineClock().Add(-time.Minute).UnixMilli())
	failedID := env.seedAutoCandidate(t, confirmSpec{content: "预写后才失败的记忆", keyID: "key_revoked"})
	_, err = env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: failedID})
	require.ErrorIs(t, err, core.ErrConflict)

	rows := confirmOperations(t, env)
	require.Len(t, rows, 2, "成功与事务内失败各留一行审计")
	byStatus := map[core.OperationStatus]core.OperationRecord{}
	for _, row := range rows {
		byStatus[row.Status] = row
		require.False(t, row.FinishedAt.IsZero(), "两行都必须回填终态")
		require.Equal(t, core.TargetKindMemory, row.TargetKind)
		require.Equal(t, "agent:ops", row.TargetID)
		require.Equal(t, consoleActor().Subject, row.ActorSubject)
	}
	require.Equal(t, memoryAdminResultOK, byStatus[core.OperationSuccess].ResultCode)
	require.Equal(t, memoryAdminResultConflict, byStatus[core.OperationFailed].ResultCode)
	require.EqualValues(t, failedID, detailMap(t, byStatus[core.OperationFailed].DetailJSON)["entry_id"],
		"失败行也必须指向被拒绝的那一条")
}

// TestUS22_Confirm_DetailNoContent 判审计明细只追加键、不写正文与证据引文；请求摘要
// 同样不得含正文。
//
// 逆改点：把正文或引文塞进 detail_json / request_hash → NotContains 断言红。
func TestUS22_Confirm_DetailNoContent(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	const content = "季度报表在每月 5 号前提交，联系人 010-0000"
	targetID := env.seedEntry(t, core.Scope{Agent: "ops"}, core.SectionArchive, memory.StateActive, "旧口径条目")
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: content, suggested: targetID})

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID, Resolution: memory.ResolutionSupersede, SupersedesEntryID: targetID})
	require.NoError(t, err)

	row := requireConfirmOp(t, env, core.OperationSuccess, memoryAdminResultOK)
	require.NotContains(t, string(row.DetailJSON), content, "审计明细不得含正文")
	require.NotContains(t, row.RequestHash, content, "请求摘要不得含正文")
	detail := detailMap(t, row.DetailJSON)
	require.EqualValues(t, candidateID, detail["entry_id"])
	require.Equal(t, memory.ResolutionSupersede, detail["resolution"])
	require.EqualValues(t, targetID, detail["supersedes_id"])
	require.NotContains(t, detail, "content", "明细不写正文字段")
	require.NotContains(t, detail, "evidence", "明细不写证据")
}

// TestUS22_Confirm_AuditFailureNoMutation 判预写失败**不得**改条目：审计仓储不可用时
// 确认直接失败，候选保持 candidate，并留下可定位的错误日志。
//
// 逆改点：忽略预写错误继续确认 → 条目被晋升，状态断言红。
func TestUS22_Confirm_AuditFailureNoMutation(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: "审计不可用时的候选"})
	_, err := env.raw.ExecContext(context.Background(),
		`ALTER TABLE management_operations RENAME TO management_operations_audit_off`)
	require.NoError(t, err, "改名审计表是制造预写失败的手段")

	_, err = env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.Error(t, err, "预写失败必须拒绝执行")
	requireStillCandidate(t, env, candidateID)
	require.Contains(t, env.logs.String(), "记忆管理预写操作记录失败，拒绝执行")
}

// bindIndexSlot 给槽位绑定一个只认识给定候选的索引器；fail 注入嵌入失败。
func (env *memoryConfirmEnv) bindIndexSlot(t *testing.T, entryID int64, scope core.Scope,
	content string, fail error,
) *indexTestEmbedder {
	t.Helper()
	store := &indexTestStore{
		generation: memory.EmbeddingGeneration{Model: "embed-test", Dimensions: 2, Status: "active"},
		pending:    []memory.MemoryEntry{indexTestEntry(entryID, scope, content)},
		written:    map[int64][]float32{},
	}
	embedder := &indexTestEmbedder{model: "embed-test", dims: 2, err: fail}
	env.slot.Bind(memory.NewIndexer(memory.IndexerDeps{Store: store, Embedder: embedder, Generation: 1}))
	return embedder
}

// TestUS22_Confirm_IndexesAfterPromotion 判晋升之后走既有补索引槽位：一次嵌入、向量
// 落在晋升行上。
//
// 逆改点：确认成功后不调用 `IndexAppended` → 嵌入零调用，用例红；在事务内索引 → 此时
// 条目仍是 candidate，按内容找不到缺口，同样红。
func TestUS22_Confirm_IndexesAfterPromotion(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	const content = "晋升后需要补索引的记忆"
	scope := core.Scope{Agent: "ops"}
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: content})
	embedder := env.bindIndexSlot(t, candidateID, scope, content, nil)

	_, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.NoError(t, err)
	require.Equal(t, 1, embedder.calls, "晋升后必须补一次索引")
	require.Len(t, embedder.inputs, 1)
	require.Equal(t, []string{content}, embedder.inputs[0])
}

// TestUS22_Confirm_IndexFailureKeepsActive 判补索引失败只 WARN：确认已经提交，条目保持
// active，缺口由启动期回填。
//
// 逆改点：把补索引失败改写成确认失败 → 回执错误、条目回滚，双断言红。
func TestUS22_Confirm_IndexFailureKeepsActive(t *testing.T) {
	env := newMemoryConfirmEnv(t)
	const content = "补索引会失败但确认已生效的记忆"
	scope := core.Scope{Agent: "ops"}
	candidateID := env.seedAutoCandidate(t, confirmSpec{content: content})
	embedder := env.bindIndexSlot(t, candidateID, scope, content,
		fmt.Errorf("embedding down"))

	result, err := env.admin.ConfirmWithResolution(context.Background(), consoleActor(),
		core.MemoryConfirmRequest{EntryID: candidateID})
	require.NoError(t, err, "补索引失败不得改写成确认失败")
	require.Equal(t, memory.StateActive, result.State)
	require.Equal(t, memory.StateActive, env.entry(t, candidateID).State)
	require.Equal(t, 1, embedder.calls)
	require.True(t, strings.Contains(env.logs.String(), "补索引失败"), "失败必须留下 WARN 日志")
}

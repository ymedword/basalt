//go:build integration

package postgres

// 本文件是 US-22 自动候选确认事务的具名回归（颗粒度文档 §3.1.7 / C-16，T014）。
//
// 判据分三层，各自有明确的逆改点：
//  1. **CAS**：只有 `candidate` 且未过期可以晋升；第二次确认同一行、过期候选、混用处置
//     都必须返回 `core.ErrConflict` 且状态不变（`ConfirmCandidateCAS`）。
//  2. **替代**：建议目标逐字相等且仍 active / 未过期；不一致、已失效的目标一律拒绝，
//     且**不得**误改任何一行（`ConfirmCandidateSupersede`）。
//  3. **锁序**：来源条目走 `FOR SHARE` 且 `ORDER BY id`——另一个事务持有来源行锁时，
//     确认事务必须**等**在来源检查这一步（`ConfirmLocksSourceRows`）。
//
// 读模型（`LoadCandidateMeta`）与写入路径逐列同源，单独用真库判往返（`CandidateMetaRoundTrip`）。
// The confirm transaction's named regressions: CAS, supersede consistency, lock ordering,
// and the metadata read model round trip — all against a real server.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// mcBase 是确认夹具的固定时钟：候选到期、Key 到期与请求时刻都按它复算。
// mcBase is the fixed clock for the confirm fixtures.
var mcBase = time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)

// mcTestStore 建隔离 schema（已种调用方 c1）并返回固定时钟的载体。
func mcTestStore(t *testing.T) (*DB, *MemoryStore) {
	t.Helper()
	db := newTxTestDB(t)
	return db, NewMemoryStore(db, func() time.Time { return mcBase })
}

// mcSeedKey 插一把来源 Key；expiresAt / revokedAt 传 nil 表示未设。
func mcSeedKey(t *testing.T, db *DB, keyID, callerID string, expiresAt, revokedAt any) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO caller_keys
			(key_id, caller_id, key_hash, key_prefix, scopes, expires_at, revoked_at, created_at)
		VALUES ($1, $2, $3, $4, '{}', $5, $6, $7)`,
		keyID, callerID, "hash-"+keyID, "bk_"+keyID, expiresAt, revokedAt, mcBase.UnixMilli())
	require.NoError(t, err)
}

// mcSeedExtraction 插一条已成功的抽取行——候选元数据的 `extraction_id` 外键指向它。
func mcSeedExtraction(t *testing.T, db *DB, extractionID, runID, callerID, keyID string) {
	t.Helper()
	at := mcBase.UnixMilli()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO memory_extractions
			(extraction_id, agent_name, session_id, run_id, last_attempt_run_id,
			 origin_caller_id, origin_key_id, status, window_from_message_id, window_to_message_id,
			 window_messages, prompt_version, rule_version, provider, model, created_at, last_attempt_at)
		VALUES ($1, 'ops', 'sess_confirm', $2, $2, $3, $4, 'success', 1, 2, 2, 'mv1', 'rv1',
		        'stub', 'stub-model', $5, $5)`,
		extractionID, runID, callerID, keyID, at)
	require.NoError(t, err)
}

// mcSeedEntry 插一条条目；state 由用例给定（candidate / active / superseded）。
func mcSeedEntry(t *testing.T, db *DB, state, content string, validTo any) int64 {
	t.Helper()
	return memSeed(t, db, memSeedRow{
		scope:     "agent",
		agentName: "ops",
		section:   "archive",
		content:   content,
		state:     state,
		source:    memorySourceAutoExtract,
		validTo:   validTo,
	})
}

// mcSeedMeta 插一条候选元数据；sourceIDs 同时写进 `source_entry_ids` 与来源关系表，
// suggested 为 0 表示无建议。validTo 传 nil 表示候选不过期。
func mcSeedMeta(t *testing.T, db *DB, entryID int64, extractionID string, sourceIDs []int64,
	suggested int64, callerID, keyID string, validTo any,
) {
	t.Helper()
	var suggestedArg, validToArg any
	if suggested > 0 {
		suggestedArg = suggested
	}
	if validTo != nil {
		validToArg = validTo
	}
	if sourceIDs == nil {
		sourceIDs = []int64{}
	}
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO memory_candidate_meta
			(entry_id, extraction_id, kind, certainty, confidence, evidence_json, source_entry_ids,
			 suggested_supersedes_id, origin_caller_id, origin_key_id, evidence_at, valid_to,
			 time_confidence, created_at)
		VALUES ($1, $2, 'fact', 'asserted', 0.8, '[]'::jsonb, $3, $4, $5, $6, $7, $8, 'exact', $9)`,
		entryID, extractionID, sourceIDs, suggestedArg, callerID, keyID,
		mcBase.UnixMilli(), validToArg, mcBase.UnixMilli())
	require.NoError(t, err)
	for _, sourceID := range sourceIDs {
		_, err := db.pool.ExecContext(context.Background(), `
			INSERT INTO memory_candidate_sources (entry_id, source_entry_id, extraction_id, created_at)
			VALUES ($1, $2, $3, $4)`, entryID, sourceID, extractionID, mcBase.UnixMilli())
		require.NoError(t, err)
	}
}

// mcState 读回一行的状态、确认人与替代指针，断言存在。
func mcState(t *testing.T, db *DB, id int64) (string, sql.NullString, sql.NullInt64) {
	t.Helper()
	var (
		state      string
		confirmed  sql.NullString
		supersedes sql.NullInt64
	)
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT state, confirmed_by, supersedes_id FROM memory_entries WHERE id = $1`, id).
		Scan(&state, &confirmed, &supersedes))
	return state, confirmed, supersedes
}

// TestUS22_Store_ConfirmCandidateCAS 判晋升的 CAS 与到期门禁。
//
// 逆改点：去掉 `WHERE state='candidate'` → 第二次确认成功，冲突断言红；不判
// `valid_to` → 过期候选被复活，过期断言红；把空串处置当未知 → 兼容形状（空串 =
// keep_both）被误拒；keep_both 夹带目标不拒 → 混用处置断言红。
// Dropping the CAS predicate or the expiry gate turns exactly one assertion red.
func TestUS22_Store_ConfirmCandidateCAS(t *testing.T) {
	db, store := mcTestStore(t)
	ctx := context.Background()

	mcSeedExtraction(t, db, "lrn_cas", "run_cas", "c1", "")
	candidateID := mcSeedEntry(t, db, memoryStateCandidate, "季度报表在每月 5 号前提交", nil)
	mcSeedMeta(t, db, candidateID, "lrn_cas", nil, 0, "c1", "", nil)

	result, err := store.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID: candidateID, ConfirmedBy: "admin", Now: mcBase,
	})
	require.NoError(t, err)
	require.Equal(t, candidateID, result.EntryID)
	require.Zero(t, result.SupersededID, "保留双方的替代指针恒为 0")

	state, confirmed, supersedes := mcState(t, db, candidateID)
	require.Equal(t, memoryStateActive, state)
	require.Equal(t, "admin", confirmed.String, "确认人来自请求，不取进程身份")
	require.False(t, supersedes.Valid, "keep_both 不写 supersedes_id")

	// 第二次确认同一行：CAS 的 `WHERE state='candidate'` 不再命中。
	_, err = store.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID: candidateID, ConfirmedBy: "admin", Now: mcBase,
	})
	require.ErrorIs(t, err, core.ErrConflict, "已晋升的行再次确认必须冲突")
	state, _, _ = mcState(t, db, candidateID)
	require.Equal(t, memoryStateActive, state, "冲突不得改变已有状态")

	// 过期候选：`memory_entries.valid_to` 已到，不得复活。
	mcSeedExtraction(t, db, "lrn_expired", "run_expired", "c1", "")
	expiredID := mcSeedEntry(t, db, memoryStateCandidate, "过期候选", mcBase.Add(-time.Minute).UnixMilli())
	mcSeedMeta(t, db, expiredID, "lrn_expired", nil, 0, "c1", "", nil)
	_, err = store.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID: expiredID, ConfirmedBy: "admin", Now: mcBase,
	})
	require.ErrorIs(t, err, core.ErrConflict, "到期候选不得复活")
	state, _, _ = mcState(t, db, expiredID)
	require.Equal(t, memoryStateCandidate, state)

	// 混用处置：keep_both 携带替代目标 / supersede 缺目标 / 未知处置。
	mcSeedExtraction(t, db, "lrn_shape", "run_shape", "c1", "")
	shapeID := mcSeedEntry(t, db, memoryStateCandidate, "形状校验候选", nil)
	mcSeedMeta(t, db, shapeID, "lrn_shape", nil, 0, "c1", "", nil)
	for name, req := range map[string]memory.ConfirmCandidateRequest{
		"keep_both 带目标": {EntryID: shapeID, Resolution: memory.ResolutionKeepBoth, SupersedesEntryID: 42},
		"supersede 缺目标": {EntryID: shapeID, Resolution: memory.ResolutionSupersede},
		"未知处置":          {EntryID: shapeID, Resolution: "replace"},
	} {
		req.ConfirmedBy, req.Now = "admin", mcBase
		_, err := store.ConfirmCandidate(ctx, req)
		require.ErrorIs(t, err, core.ErrConflict, name)
		state, _, _ = mcState(t, db, shapeID)
		require.Equal(t, memoryStateCandidate, state, "%s：拒绝不得改状态", name)
	}

	// 空串等价 keep_both（web 空 body 的归一化在调用方完成，存储层同义）。
	_, err = store.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID: shapeID, ConfirmedBy: "admin", Now: mcBase,
	})
	require.NoError(t, err, "空处置按 keep_both 处理")
}

// TestUS22_Store_ConfirmCandidateSupersede 判替代的一致性与目标有效性。
//
// 逆改点：信任请求里的目标而不比对元数据 → 不一致用例成功，断言红；不判目标状态 →
// 已退役目标被再次替代；不写 supersedes_id → 回执与列断言红。
// Trusting the request's target without comparing the snapshot turns the mismatch case red.
func TestUS22_Store_ConfirmCandidateSupersede(t *testing.T) {
	db, store := mcTestStore(t)
	ctx := context.Background()

	// 正常替代：建议与请求逐字相等，目标 active。
	mcSeedExtraction(t, db, "lrn_sup", "run_sup", "c1", "")
	targetID := mcSeedEntry(t, db, memoryStateActive, "旧口径：每月 3 号", nil)
	candidateID := mcSeedEntry(t, db, memoryStateCandidate, "新口径：每月 5 号", nil)
	mcSeedMeta(t, db, candidateID, "lrn_sup", nil, targetID, "c1", "", nil)

	result, err := store.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID: candidateID, Resolution: memory.ResolutionSupersede,
		SupersedesEntryID: targetID, ConfirmedBy: "admin", Now: mcBase,
	})
	require.NoError(t, err)
	require.Equal(t, targetID, result.SupersededID)

	state, confirmed, supersedes := mcState(t, db, candidateID)
	require.Equal(t, memoryStateActive, state)
	require.Equal(t, "admin", confirmed.String)
	require.Equal(t, targetID, supersedes.Int64, "晋升行必须记下替代指针")
	state, _, _ = mcState(t, db, targetID)
	require.Equal(t, memoryStateSuperseded, state, "旧目标必须退役")

	// 请求目标与建议不一致：拒绝，且两个目标都不动。
	mcSeedExtraction(t, db, "lrn_mis", "run_mis", "c1", "")
	otherID := mcSeedEntry(t, db, memoryStateActive, "无关条目", nil)
	mismatchID := mcSeedEntry(t, db, memoryStateCandidate, "建议指向别的条目", nil)
	mcSeedMeta(t, db, mismatchID, "lrn_mis", nil, otherID, "c1", "", nil)
	targetA := mcSeedEntry(t, db, memoryStateActive, "另一个候选目标", nil)
	_, err = store.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID: mismatchID, Resolution: memory.ResolutionSupersede,
		SupersedesEntryID: targetA, ConfirmedBy: "admin", Now: mcBase,
	})
	require.ErrorIs(t, err, core.ErrConflict, "请求目标与建议不一致必须拒绝")
	state, _, _ = mcState(t, db, mismatchID)
	require.Equal(t, memoryStateCandidate, state, "被拒候选保持候选")
	state, _, _ = mcState(t, db, otherID)
	require.Equal(t, memoryStateActive, state, "建议目标不得被误替代")
	state, _, _ = mcState(t, db, targetA)
	require.Equal(t, memoryStateActive, state, "请求目标不得被误替代")

	// 建议目标已不是 active：旧建议不得覆盖新纠正。
	mcSeedExtraction(t, db, "lrn_stale", "run_stale", "c1", "")
	staleID := mcSeedEntry(t, db, memoryStateCorrected, "已被纠正的旧条目", nil)
	staleCand := mcSeedEntry(t, db, memoryStateCandidate, "指向已纠正条目", nil)
	mcSeedMeta(t, db, staleCand, "lrn_stale", nil, staleID, "c1", "", nil)
	_, err = store.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID: staleCand, Resolution: memory.ResolutionSupersede,
		SupersedesEntryID: staleID, ConfirmedBy: "admin", Now: mcBase,
	})
	require.ErrorIs(t, err, core.ErrConflict, "非 active 的建议目标不得被替代")
	state, _, _ = mcState(t, db, staleCand)
	require.Equal(t, memoryStateCandidate, state)
	state, _, _ = mcState(t, db, staleID)
	require.Equal(t, memoryStateCorrected, state, "拒绝不得改写目标状态")

	// 建议目标已过期：按失效处理。
	mcSeedExtraction(t, db, "lrn_target_expired", "run_target_expired", "c1", "")
	expiredTarget := mcSeedEntry(t, db, memoryStateActive, "到期目标", mcBase.Add(-time.Minute).UnixMilli())
	expiredCand := mcSeedEntry(t, db, memoryStateCandidate, "指向到期目标", nil)
	mcSeedMeta(t, db, expiredCand, "lrn_target_expired", nil, expiredTarget, "c1", "", nil)
	_, err = store.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID: expiredCand, Resolution: memory.ResolutionSupersede,
		SupersedesEntryID: expiredTarget, ConfirmedBy: "admin", Now: mcBase,
	})
	require.ErrorIs(t, err, core.ErrConflict, "到期目标不得被替代")
}

// TestUS22_Confirm_ClosureInvalidatedRejected 守 C-16「来源闭包全部 active」：候选 C 的
// 直接来源 A 仍 active，但 A 的来源 B 已删除——沿 memory_candidate_sources 的闭包必须
// 让确认 409 且 C 保持 candidate。
//
// 逆改点：只查 meta.SourceEntryIDs（直接来源）→ 闭包深处的 B 不被看见，确认成功、断言红；
// 只在召回面复核、确认事务放行 → 产生永不召回的 active 行。
// A candidate whose direct source is live but whose transitive source is deleted must
// be rejected: the confirm transaction walks the same closure as the recall side.
func TestUS22_Confirm_ClosureInvalidatedRejected(t *testing.T) {
	db, store := mcTestStore(t)
	ctx := context.Background()

	mcSeedExtraction(t, db, "lrn_chain", "run_chain", "c1", "")
	rootID := mcSeedEntry(t, db, memoryStateActive, "闭包根来源", nil)
	middleID := mcSeedEntry(t, db, memoryStateActive, "中间派生条目", nil)
	mcSeedMeta(t, db, middleID, "lrn_chain", []int64{rootID}, 0, "c1", "", nil)
	candidateID := mcSeedEntry(t, db, memoryStateCandidate, "链尾候选", nil)
	mcSeedMeta(t, db, candidateID, "lrn_chain", []int64{middleID}, 0, "c1", "", nil)

	// 直接来源 middle 仍 active，只有闭包深处的 root 被删除。
	_, err := db.pool.ExecContext(ctx,
		`UPDATE memory_entries SET state = $2 WHERE id = $1`, rootID, memoryStateDeleted)
	require.NoError(t, err)

	_, err = store.ConfirmCandidate(ctx, memory.ConfirmCandidateRequest{
		EntryID: candidateID, ConfirmedBy: "admin", Now: mcBase,
	})
	require.ErrorIs(t, err, core.ErrConflict, "闭包内有失效来源时不得晋升")
	state, _, _ := mcState(t, db, candidateID)
	require.Equal(t, memoryStateCandidate, state, "拒绝不得改变候选状态")
}

// TestUS22_Store_CandidateMetaRoundTrip 判读模型与写入列逐列同源：证据时刻按
// RFC3339Nano（UTC）往返、数组与可空列不丢值；缺行返回 found=false 而不是错误。
//
// 逆改点：证据时刻不按 UTC 解析 → 时刻断言红；把 suggested / valid_to 当非空读 →
// 空值用例扫描失败；缺行当错误 → found=false 断言红。
// Reverting any column convention breaks exactly one assertion here.
func TestUS22_Store_CandidateMetaRoundTrip(t *testing.T) {
	db, store := mcTestStore(t)
	ctx := context.Background()

	sourceA := mcSeedEntry(t, db, memoryStateActive, "来源一", nil)
	sourceB := mcSeedEntry(t, db, memoryStateActive, "来源二", nil)
	suggested := mcSeedEntry(t, db, memoryStateActive, "建议替代目标", nil)
	mcSeedExtraction(t, db, "lrn_meta", "run_meta", "c1", "key_meta")
	mcSeedKey(t, db, "key_meta", "c1", nil, nil)

	entryID := mcSeedEntry(t, db, memoryStateCandidate, "往返候选", nil)
	evidenceAt := mcBase.Add(-time.Hour).UTC()
	quoted := "季度报表在每月 5 号前提交"
	var suggestedArg any = suggested
	validTo := mcBase.Add(72 * time.Hour).UnixMilli()
	_, err := db.pool.ExecContext(ctx, `
		INSERT INTO memory_candidate_meta
			(entry_id, extraction_id, kind, certainty, confidence, evidence_json, source_entry_ids,
			 suggested_supersedes_id, origin_caller_id, origin_key_id, evidence_at, valid_to,
			 time_confidence, created_at)
		VALUES ($1, 'lrn_meta', 'preference', 'uncertain', 0.42, $2::jsonb, $3, $4, 'c1', 'key_meta',
		        $5, $6, 'relative_unknown', $7)`,
		entryID,
		`[{"message_id": 11, "run_id": "run_meta", "role": "user", "quote": "`+quoted+
			`", "message_at": "`+evidenceAt.Format(time.RFC3339Nano)+`"}]`,
		[]int64{sourceA, sourceB}, suggestedArg, evidenceAt.UnixMilli(), validTo, mcBase.UnixMilli())
	require.NoError(t, err)

	meta, found, err := store.LoadCandidateMeta(ctx, entryID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, entryID, meta.EntryID)
	require.Equal(t, "lrn_meta", meta.ExtractionID)
	require.Equal(t, "preference", meta.Kind)
	require.Equal(t, "uncertain", meta.Certainty)
	require.InDelta(t, 0.42, meta.Confidence, 1e-9)
	require.Equal(t, []int64{sourceA, sourceB}, meta.SourceEntryIDs)
	require.Equal(t, suggested, meta.SuggestedSupersedesID)
	require.Equal(t, "c1", meta.OriginCallerID)
	require.Equal(t, "key_meta", meta.OriginKeyID)
	require.True(t, evidenceAt.Equal(meta.EvidenceAt), "证据时刻必须逐纳秒往返")
	require.Equal(t, time.UnixMilli(validTo).UTC(), meta.ValidTo.UTC())
	require.Equal(t, "relative_unknown", meta.TimeConfidence)
	require.Len(t, meta.Evidence, 1)
	require.Equal(t, int64(11), meta.Evidence[0].MessageID)
	require.Equal(t, "run_meta", meta.Evidence[0].RunID)
	require.Equal(t, "user", meta.Evidence[0].Role)
	require.Equal(t, quoted, meta.Evidence[0].Quote, "证据引文必须逐字往返")

	// 可空列全空的一行：suggested / valid_to 不得扫描失败。
	mcSeedExtraction(t, db, "lrn_meta_empty", "run_meta_empty", "c1", "")
	emptyID := mcSeedEntry(t, db, memoryStateCandidate, "无建议候选", nil)
	mcSeedMeta(t, db, emptyID, "lrn_meta_empty", nil, 0, "c1", "", nil)
	empty, found, err := store.LoadCandidateMeta(ctx, emptyID)
	require.NoError(t, err)
	require.True(t, found)
	require.Zero(t, empty.SuggestedSupersedesID)
	require.True(t, empty.ValidTo.IsZero(), "空 valid_to 还原为零值时刻")

	// 缺行 / 非法主键：found=false 且无错误。
	_, found, err = store.LoadCandidateMeta(ctx, 999999)
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = store.LoadCandidateMeta(ctx, 0)
	require.NoError(t, err)
	require.False(t, found)
}

// TestUS22_Store_ConfirmLocksSourceRows 判来源检查真的取 `FOR SHARE` 行锁：另一个事务
// 持有来源行 `FOR UPDATE` 时，确认必须等待；释放后成功。
//
// 逆改点：把来源检查改成普通 SELECT（不取锁）→ 确认在持有者事务未结束时即返回，
// 「阻塞」断言红；去掉来源检查 → 同样红。
// Replacing the locking read with a plain SELECT makes the confirmation return early.
func TestUS22_Store_ConfirmLocksSourceRows(t *testing.T) {
	db, store := mcTestStore(t)
	ctx := context.Background()

	mcSeedExtraction(t, db, "lrn_lock", "run_lock", "c1", "")
	sourceID := mcSeedEntry(t, db, memoryStateActive, "来源条目", nil)
	candidateID := mcSeedEntry(t, db, memoryStateCandidate, "依赖来源的候选", nil)
	mcSeedMeta(t, db, candidateID, "lrn_lock", []int64{sourceID}, 0, "c1", "", nil)

	holder, err := db.pool.BeginTx(ctx, nil)
	require.NoError(t, err)
	holderDone := false
	defer func() {
		if !holderDone {
			_ = holder.Rollback()
		}
	}()
	var locked int64
	require.NoError(t, holder.QueryRowContext(ctx,
		`SELECT id FROM memory_entries WHERE id = $1 FOR UPDATE`, sourceID).Scan(&locked))

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, confirmErr := store.ConfirmCandidate(context.Background(), memory.ConfirmCandidateRequest{
			EntryID: candidateID, ConfirmedBy: "admin", Now: mcBase,
		})
		done <- confirmErr
	}()
	<-started

	select {
	case confirmErr := <-done:
		t.Fatalf("来源行被另一事务持锁时确认不得完成（err=%v）——来源检查没有取行锁", confirmErr)
	case <-time.After(300 * time.Millisecond):
		// 确认事务正等在来源行的 `FOR SHARE` 上，符合预期。
	}

	require.NoError(t, holder.Rollback())
	holderDone = true
	select {
	case confirmErr := <-done:
		require.NoError(t, confirmErr, "持锁事务回滚后确认必须成功")
	case <-time.After(5 * time.Second):
		t.Fatal("释放来源行锁后确认仍未完成")
	}
	state, _, _ := mcState(t, db, candidateID)
	require.Equal(t, memoryStateActive, state)
}

package postgres

// 本文件是 US-22 自动候选确认的窄事务实现（颗粒度文档 §3.1.7 / C-16）：
// `LoadCandidateMeta` 供管理面做纯读复核，`ConfirmCandidate` 是自动候选**唯一**的
// 晋升事务。显式提议候选仍走既有 `memory.SemanticStore.Confirm`（C-03 / C-16）。
//
// 事务内的检查顺序是契约，**不得**调换：
//  1. 候选行 `FOR UPDATE`，再以 `UPDATE ... WHERE state='candidate'` 做 CAS；
//  2. 候选 `valid_to` 未过（时间判据一律取入参 `Now`，不在 SQL 里另取 now）；
//  3. supersede 时：目标必须与 `memory_candidate_meta.suggested_supersedes_id`
//     逐字相等，且目标仍 active、未过期（旧建议不得覆盖新纠正）；
//  4. 来源闭包 `FOR SHARE`（ORDER BY id 固定锁序）：沿 `memory_candidate_sources`
//     递归取闭包（深度上限 8，超限按失效处理），并与元数据声明的直接来源取并集；
//     闭包内全部条目存在、active 且未过期（C-16「来源闭包全部 active」）；
//  5. `origin_key_id <> ''` 时来源 Key 未撤销、未过期；来源调用方 `status='active'`；
//  6. 更新候选（active + supersedes_id + confirmed_by）与目标（superseded）。
//
// 任一校验不满足 → 整个事务回滚并返回 `core.ErrConflict`（web 映射 409）。事务内**不做**
// 磁盘或索引动作；确认后的 best-effort 向量补索引由管理面在提交之后执行（C-28）。
// The step order of the promotion transaction is the card contract; any failed gate
// rolls back and surfaces core.ErrConflict.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// 编译期断言：确认事务是 memory 包声明的窄端口的一个实现。
// Compile-time assertion that the promotion transaction satisfies the narrow port.
var _ memory.CandidateConfirmStore = (*MemoryStore)(nil)

// LoadCandidateMeta 读一条候选的元数据（纯读）。不存在返回 found=false 与 nil 错误——
// 调用方据来源列自行判定"自动候选缺元数据"是不是异常，本方法不替它猜。
// LoadCandidateMeta is a pure read; a missing row is (zero, false, nil).
func (s *MemoryStore) LoadCandidateMeta(ctx context.Context, entryID int64) (core.MemoryCandidateMeta, bool, error) {
	if entryID <= 0 {
		return core.MemoryCandidateMeta{}, false, nil
	}
	meta, found, err := loadCandidateMeta(ctx, s.db.pool, entryID)
	if err != nil {
		return core.MemoryCandidateMeta{}, false, fmt.Errorf("读取记忆候选元数据（%d）: %w", entryID, err)
	}
	return meta, found, nil
}

// loadCandidateMeta 是纯读与管理面共用的单行读取：列序固定，数组与 jsonb 的还原方式与
// `candidatesByExtraction` 逐列同源（bigint[] → []int64，jsonb → []byte）。
// loadCandidateMeta scans one meta row with the same column conventions as extraction reads.
func loadCandidateMeta(ctx context.Context, q Queryer, entryID int64) (core.MemoryCandidateMeta, bool, error) {
	var (
		extractionID   string
		kind           string
		certainty      string
		confidence     float64
		evidenceRaw    []byte
		sourceEntryIDs []int64
		suggestedID    sql.NullInt64
		originCallerID string
		originKeyID    string
		evidenceAt     int64
		validTo        sql.NullInt64
		timeConfidence string
	)
	err := q.QueryRowContext(ctx, `
		SELECT extraction_id, kind, certainty, confidence, evidence_json,
		       source_entry_ids, suggested_supersedes_id, origin_caller_id,
		       origin_key_id, evidence_at, valid_to, time_confidence
		  FROM memory_candidate_meta
		 WHERE entry_id = $1`, entryID).Scan(
		&extractionID, &kind, &certainty, &confidence, &evidenceRaw,
		&sourceEntryIDs, &suggestedID, &originCallerID,
		&originKeyID, &evidenceAt, &validTo, &timeConfidence)
	if errors.Is(err, sql.ErrNoRows) {
		return core.MemoryCandidateMeta{}, false, nil
	}
	if err != nil {
		return core.MemoryCandidateMeta{}, false, err
	}
	evidence, err := unmarshalLearningEvidence(evidenceRaw)
	if err != nil {
		return core.MemoryCandidateMeta{}, false, fmt.Errorf("还原候选证据: %w", err)
	}
	return core.MemoryCandidateMeta{
		EntryID:               entryID,
		ExtractionID:          extractionID,
		Kind:                  kind,
		Certainty:             certainty,
		Confidence:            confidence,
		Evidence:              evidence,
		SourceEntryIDs:        sourceEntryIDs,
		SuggestedSupersedesID: suggestedID.Int64,
		OriginCallerID:        originCallerID,
		OriginKeyID:           originKeyID,
		EvidenceAt:            learningTimeFromMS(evidenceAt),
		ValidTo:               learningTimeFromNull(validTo),
		TimeConfidence:        timeConfidence,
	}, true, nil
}

// ConfirmCandidate 在一个事务内把自动候选晋升为 active。全部校验都在事务内重做一遍
// （管理面的纯读只用来在**预写审计之前**给出 404/409，不构成授权）：并发撤销、停用、
// 删除或到期要么等在本事务的行锁之后，要么使本事务失败。
// ConfirmCandidate promotes one auto candidate inside a single transaction and repeats
// every gate the admin surface pre-checked; concurrency is serialized by row locks.
func (s *MemoryStore) ConfirmCandidate(ctx context.Context, req memory.ConfirmCandidateRequest) (memory.ConfirmCandidateResult, error) {
	if req.EntryID <= 0 {
		return memory.ConfirmCandidateResult{}, fmt.Errorf("%w: 候选主键必须是正整数", core.ErrConflict)
	}
	resolution, err := memoryConfirmResolution(req)
	if err != nil {
		return memory.ConfirmCandidateResult{}, err
	}
	confirmedBy := strings.TrimSpace(req.ConfirmedBy)
	if confirmedBy == "" {
		return memory.ConfirmCandidateResult{}, errors.New("确认候选记忆: 确认人不得为空")
	}
	now := req.Now
	if now.IsZero() {
		now = s.now()
	}
	nowMs := now.UnixMilli()

	var result memory.ConfirmCandidateResult
	err = s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			state   string
			validTo sql.NullInt64
		)
		// 1) 候选行 FOR UPDATE：并发确认同一行时，后来者在这里等待并读到已提交的新状态。
		if err := tx.QueryRowContext(ctx,
			`SELECT state, valid_to FROM memory_entries WHERE id = $1 FOR UPDATE`,
			req.EntryID).Scan(&state, &validTo); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				// 预读之后被删（本系统只软删，物理删除意味着库被外部改过）：按状态冲突拒绝。
				return memoryConfirmConflict("候选 %d 不存在", req.EntryID)
			}
			return fmt.Errorf("锁定候选记忆（%d）: %w", req.EntryID, err)
		}
		if state != memoryStateCandidate {
			return memoryConfirmConflict("候选 %d 当前状态是 %q，只有 candidate 可以确认", req.EntryID, state)
		}
		// 2) 候选到期：到期候选不得复活。
		if validTo.Valid && validTo.Int64 <= nowMs {
			return memoryConfirmConflict("候选 %d 已过期（valid_to=%d）", req.EntryID, validTo.Int64)
		}

		meta, found, err := loadCandidateMeta(ctx, tx, req.EntryID)
		if err != nil {
			return fmt.Errorf("读取候选元数据（%d）: %w", req.EntryID, err)
		}
		if !found {
			// 自动候选没有元数据 = 来源不可查证，fail-closed（不得按 keep_both 放行）。
			return memoryConfirmConflict("候选 %d 缺少候选元数据，无法查证来源", req.EntryID)
		}

		supersededID := int64(0)
		if resolution == memory.ResolutionSupersede {
			// 3) 替代目标必须与快照里的建议逐字相等，且仍 active、未过期。
			if meta.SuggestedSupersedesID != req.SupersedesEntryID {
				return memoryConfirmConflict("替代目标 %d 与候选建议 %d 不一致",
					req.SupersedesEntryID, meta.SuggestedSupersedesID)
			}
			var (
				targetState   string
				targetValidTo sql.NullInt64
			)
			if err := tx.QueryRowContext(ctx,
				`SELECT state, valid_to FROM memory_entries WHERE id = $1 FOR UPDATE`,
				req.SupersedesEntryID).Scan(&targetState, &targetValidTo); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return memoryConfirmConflict("替代目标 %d 不存在", req.SupersedesEntryID)
				}
				return fmt.Errorf("锁定替代目标（%d）: %w", req.SupersedesEntryID, err)
			}
			if targetState != memoryStateActive {
				return memoryConfirmConflict("替代目标 %d 当前状态是 %q，只有 active 可以被替代",
					req.SupersedesEntryID, targetState)
			}
			if targetValidTo.Valid && targetValidTo.Int64 <= nowMs {
				return memoryConfirmConflict("替代目标 %d 已过期（valid_to=%d）",
					req.SupersedesEntryID, targetValidTo.Int64)
			}
			supersededID = req.SupersedesEntryID
		}

		// 4) 来源闭包 FOR SHARE：沿 memory_candidate_sources 递归取闭包（深度上限 8，
		// 超限 fail-closed），与元数据声明的直接来源取并集；全部 active 且未过期。
		// ORDER BY id 固定锁序，两个共享来源的确认事务不会以相反顺序取锁。
		closure, truncated, err := loadSourceClosure(ctx, tx, req.EntryID)
		if err != nil {
			return fmt.Errorf("读取候选来源闭包（%d）: %w", req.EntryID, err)
		}
		if truncated {
			return memoryConfirmConflict("候选 %d 的来源闭包深度超过 8，无法证明来源有效", req.EntryID)
		}
		sourceIDs := dedupeLearningIDs(append(append([]int64{}, meta.SourceEntryIDs...), closure...))
		if len(sourceIDs) > 0 {
			rows, err := tx.QueryContext(ctx,
				`SELECT id, state, valid_to FROM memory_entries
				  WHERE id = ANY($1::bigint[])
				  ORDER BY id
				  FOR SHARE`, sourceIDs)
			if err != nil {
				return fmt.Errorf("锁定候选来源条目: %w", err)
			}
			live := make(map[int64]bool, len(sourceIDs))
			for rows.Next() {
				var (
					id       int64
					srcState string
					srcValid sql.NullInt64
				)
				if err := rows.Scan(&id, &srcState, &srcValid); err != nil {
					_ = rows.Close()
					return fmt.Errorf("读取候选来源条目: %w", err)
				}
				if srcState != memoryStateActive {
					_ = rows.Close()
					return memoryConfirmConflict("来源条目 %d 当前状态是 %q，必须 active", id, srcState)
				}
				if srcValid.Valid && srcValid.Int64 <= nowMs {
					_ = rows.Close()
					return memoryConfirmConflict("来源条目 %d 已过期（valid_to=%d）", id, srcValid.Int64)
				}
				live[id] = true
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("遍历候选来源条目: %w", err)
			}
			_ = rows.Close()
			if len(live) != len(sourceIDs) {
				return memoryConfirmConflict("候选 %d 的来源闭包条目缺失（声明 %d 条，实到 %d 条）",
					req.EntryID, len(sourceIDs), len(live))
			}
		}

		// 5) 来源 Key（仅 API Key 来源）未撤销、未过期；本地管理员为空串跳过。
		// Key 必须属于记录下来的来源调用方——不一致说明来源身份被改过，fail-closed。
		if meta.OriginKeyID != "" {
			var keyCaller string
			if err := tx.QueryRowContext(ctx,
				`SELECT caller_id FROM caller_keys
				  WHERE key_id = $1
				    AND revoked_at IS NULL
				    AND (expires_at IS NULL OR expires_at > $2)
				  FOR SHARE`, meta.OriginKeyID, nowMs).Scan(&keyCaller); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return memoryConfirmConflict("来源 Key %s 已撤销、已过期或不存在", meta.OriginKeyID)
				}
				return fmt.Errorf("锁定来源 Key（%s）: %w", meta.OriginKeyID, err)
			}
			if keyCaller != meta.OriginCallerID {
				return memoryConfirmConflict("来源 Key %s 不属于来源调用方 %s",
					meta.OriginKeyID, meta.OriginCallerID)
			}
		}
		var callerStatus string
		if err := tx.QueryRowContext(ctx,
			`SELECT status FROM callers WHERE caller_id = $1 FOR SHARE`,
			meta.OriginCallerID).Scan(&callerStatus); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return memoryConfirmConflict("来源调用方 %s 不存在", meta.OriginCallerID)
			}
			return fmt.Errorf("锁定来源调用方（%s）: %w", meta.OriginCallerID, err)
		}
		if callerStatus != "active" {
			return memoryConfirmConflict("来源调用方 %s 状态是 %q，必须 active", meta.OriginCallerID, callerStatus)
		}

		// 6) CAS 更新：WHERE state='candidate' 再判一次；行锁已持有，影响行数为 0 说明
		// 状态在本次事务内被别的语句改过（异常），按冲突回滚。
		res, err := tx.ExecContext(ctx,
			`UPDATE memory_entries
			    SET state = $2,
			        supersedes_id = NULLIF($3::bigint, 0),
			        confirmed_by = $4
			  WHERE id = $1 AND state = $5`,
			req.EntryID, memoryStateActive, supersededID, confirmedBy, memoryStateCandidate)
		if err != nil {
			return fmt.Errorf("晋升候选记忆（%d）: %w", req.EntryID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取候选 CAS 影响行数（%d）: %w", req.EntryID, err)
		}
		if affected == 0 {
			return memoryConfirmConflict("候选 %d 状态在确认事务内发生变化", req.EntryID)
		}
		if supersededID != 0 {
			res, err := tx.ExecContext(ctx,
				`UPDATE memory_entries SET state = $3 WHERE id = $1 AND state = $2`,
				supersededID, memoryStateActive, memoryStateSuperseded)
			if err != nil {
				return fmt.Errorf("退役被替代条目（%d）: %w", supersededID, err)
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("读取替代影响行数（%d）: %w", supersededID, err)
			}
			if affected == 0 {
				return memoryConfirmConflict("替代目标 %d 在确认事务内已不是 active", supersededID)
			}
		}
		result = memory.ConfirmCandidateResult{EntryID: req.EntryID, SupersededID: supersededID}
		return nil
	})
	if err != nil {
		return memory.ConfirmCandidateResult{}, err
	}
	return result, nil
}

// loadSourceClosure 沿 `memory_candidate_sources` 递归取候选的来源闭包（不含候选自身）。
// 深度语义与召回面 derivedClosureCTE 逐字同源：种子深度 1、递归上限 8；闭包里出现
// 「深度 8 且该来源还有更深的来源」即视为截断，调用方按 fail-closed 拒绝确认。
// loadSourceClosure walks memory_candidate_sources with the same depth-8 rule as the
// recall-side closure; truncation is reported so the caller can fail closed.
func loadSourceClosure(ctx context.Context, tx *sql.Tx, entryID int64) ([]int64, bool, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE closure(source_id, depth) AS (
			SELECT source_entry_id, 1
			  FROM memory_candidate_sources
			 WHERE entry_id = $1
			UNION ALL
			SELECT s.source_entry_id, c.depth + 1
			  FROM closure c
			  JOIN memory_candidate_sources s ON s.entry_id = c.source_id
			 WHERE c.depth < 8
		)
		SELECT c.source_id,
		       (c.depth >= 8 AND EXISTS (
		            SELECT 1 FROM memory_candidate_sources deeper
		             WHERE deeper.entry_id = c.source_id))
		  FROM closure c
		 ORDER BY c.source_id`, entryID)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var (
		ids       []int64
		truncated bool
	)
	for rows.Next() {
		var (
			id     int64
			cutOff bool
		)
		if err := rows.Scan(&id, &cutOff); err != nil {
			return nil, false, err
		}
		ids = append(ids, id)
		if cutOff {
			truncated = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return ids, truncated, nil
}

// memoryConfirmResolution 归一化处置：空串等价 keep_both；supersede 必须带正整数目标；
// keep_both 不得夹带目标（夹带说明调用方把两种处置拼在了一起，按冲突拒绝）。
// memoryConfirmResolution normalises the resolution and rejects mixed shapes.
func memoryConfirmResolution(req memory.ConfirmCandidateRequest) (string, error) {
	switch req.Resolution {
	case "", memory.ResolutionKeepBoth:
		if req.SupersedesEntryID != 0 {
			return "", memoryConfirmConflict("保留双方不得携带替代目标（收到 %d）", req.SupersedesEntryID)
		}
		return memory.ResolutionKeepBoth, nil
	case memory.ResolutionSupersede:
		if req.SupersedesEntryID <= 0 {
			return "", memoryConfirmConflict("替代处置缺少有效的替代目标")
		}
		return memory.ResolutionSupersede, nil
	default:
		return "", memoryConfirmConflict("未知的确认处置：%q（只接受 keep_both / supersede）", req.Resolution)
	}
}

// memoryConfirmConflict 把门禁失败统一包成 `core.ErrConflict`（web 409），保留可读原因；
// 不透出正文、证据引文或凭据（调用方只拿到主键与状态名）。
// memoryConfirmConflict wraps a gate failure as core.ErrConflict with a safe message.
func memoryConfirmConflict(format string, args ...any) error {
	return fmt.Errorf("%w: 确认候选记忆: %s", core.ErrConflict, fmt.Sprintf(format, args...))
}

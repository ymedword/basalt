package postgres

// 本文件是 `knowledge.AssemblyRecorder` 的 PostgreSQL 实现（US-16 §3.4.1 / C-26 /
// T129）：把一轮上下文装配的证据写进 `context_assembly_records`。四条纪律：
//
//  1. **无返回值，失败只 WARN**：端口形状如此（诊断与证据可丢弃，业务调用**不得**
//     因为"记录没写上"而失败）。但 WARN **必须**真打出来——静默丢证据等于记录从未
//     存在，而装配记录的用途恰恰是缺记录时能说出"这里缺一条"。
//  2. **标识与时刻由写入器分配**：`RecordID` 用 `ctxa_` 前缀的不透明键，`CreatedAt`
//     零值时取句柄时钟；调用方只填语义字段（`Build` 填装配面，循环填调用关联）。
//  3. **列的原样序**：七个 jsonb 列显式编码，键名与 §3.4.1 的字段口径一致；
//     `history_trim` **不得**写占位零值——没有裁剪时写的是「确实没裁」这一事实
//     （组数 0、原因码空），与"还没填"是两回事，因此这里的零值来自 `Build` 的真实结果。
//  4. **形状非法先挡**：`run_id` 为空或 `purpose` 不在冻结四值内 → WARN 后丢弃，
//     不让一句 CHECK 违例进数据库日志（与非法的定位由调用方自己造成，不是数据问题）。
//
// This file persists one assembly record. The port returns nothing, so every failure is
// a WARN; a dropped record must still be visible in the log. Ids and timestamps belong
// to the writer; the seven jsonb columns are encoded explicitly.

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// assemblyRecordIDPrefix 是装配记录标识的前缀（`ctxa_` = context assembly）。
// 与知识域的其它标识同构：前缀 + 26 位 Crockford Base32，不含业务含义。
// assemblyRecordIDPrefix is the opaque record-id prefix.
const assemblyRecordIDPrefix = "ctxa_"

// assemblyRecordColumns 是列清单的唯一一份：插入语句的占位符逐列对应。
// assemblyRecordColumns has exactly one copy, shared by the insert.
const assemblyRecordColumns = `record_id, run_id, step_id, llm_call_id, purpose, agent,
	prompt_version, model_version, budget, estimator_version, candidates, hits, decisions,
	reason_codes, compression, history_trim, final_segments, created_at`

// assemblyInsert 是单行插入：18 个占位符逐列对应 `assemblyRecordColumns`。
// assemblyInsert writes one row; the placeholders line up with the column list.
const assemblyInsert = `INSERT INTO context_assembly_records (` + assemblyRecordColumns + `)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11::jsonb, $12::jsonb,
		$13::jsonb, $14::jsonb, $15::jsonb, $16::jsonb, $17::jsonb, $18)`

// assemblyStore 是 `context_assembly_records` 的写入实现。
// assemblyStore implements the assembly recorder port.
type assemblyStore struct {
	db     *DB
	logger *slog.Logger
}

// 编译期断言：端口形状由本类型满足。
// The implementation satisfies the consumer port.
var _ knowledge.AssemblyRecorder = (*assemblyStore)(nil)

// 同一个实例也满足 `core` 侧端口（V-7 的对话装配）：记录结构在 `core`，两个消费方
// 各自声明端口——**不是**两套实现，变化会在这里同时暴露。
// The same instance also serves the core-side port used by chat assembly (V-7).
var _ core.AssemblyRecorder = (*assemblyStore)(nil)

// NewAssemblyStore 绑定库句柄与日志目标；`logger` 为 nil 时退回 `slog.Default()`。
// 端口无返回值，日志是失败的唯一出口，因此**不得**允许日志目标为空。
// NewAssemblyStore binds the handle and the log target; the port has no error return,
// so the logger is the only failure channel and never stays nil.
func NewAssemblyStore(db *DB, logger *slog.Logger) *assemblyStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &assemblyStore{db: db, logger: logger}
}

// assemblyBudgetJSON 是 `budget` 列的字段口径（§3.4.1：`context_tokens` /
// 协议预留 / 输出预留 / 估算器版本）。
// assemblyBudgetJSON mirrors the budget column's field names.
type assemblyBudgetJSON struct {
	ContextTokens    int    `json:"context_tokens"`
	ProtocolReserve  int    `json:"protocol_reserve"`
	OutputReserve    int    `json:"output_reserve"`
	EstimatorVersion string `json:"estimator_version"`
}

// assemblyCandidateJSON 是 `candidates` 每项的口径：只存标识、分数、名次与 ACL 决定，
// **不**复制正文。
// assemblyCandidateJSON is one candidate; ids, scores, rank and the ACL decision only.
type assemblyCandidateJSON struct {
	ChunkID        string  `json:"chunk_id"`
	VersionID      string  `json:"version_id"`
	ContentHash    string  `json:"content_hash"`
	TokenEstimate  int     `json:"token_estimate"`
	RetrievalScore float64 `json:"retrieval_score"`
	RerankScore    float64 `json:"rerank_score"`
	Rank           int     `json:"rank"`
	ACLDecision    string  `json:"acl_decision"`
}

// assemblyCompressionJSON 是 `compression` 列的口径：未压缩时 `decision=none` 且给原因。
// assemblyCompressionJSON carries the real compaction outcome.
type assemblyCompressionJSON struct {
	Enabled              bool   `json:"enabled"`
	Decision             string `json:"decision"`
	CompactionID         string `json:"compaction_id"`
	CoveredFromMessageID string `json:"covered_from_message_id"`
	CutMessageID         string `json:"cut_message_id"`
	FirstKeptMessageID   string `json:"first_kept_message_id"`
	StrategyVersion      string `json:"strategy_version"`
	PromptVersion        string `json:"prompt_version"`
	TokensBefore         int    `json:"tokens_before"`
	TokensAfter          int    `json:"tokens_after"`
	ReasonCode           string `json:"reason_code"`
}

// assemblyHistoryTrimJSON 是 `history_trim` 列的口径：组数、首条与切点的 message_id、
// 原因码、前后估算与策略版本。
// assemblyHistoryTrimJSON carries the real trim outcome, never a placeholder.
type assemblyHistoryTrimJSON struct {
	DroppedGroups         int    `json:"dropped_groups"`
	FirstDroppedMessageID string `json:"first_dropped_message_id"`
	CutMessageID          string `json:"cut_message_id"`
	ReasonCode            string `json:"reason_code"`
	TokensBefore          int    `json:"tokens_before"`
	TokensAfter           int    `json:"tokens_after"`
	StrategyVersion       string `json:"strategy_version"`
}

// assemblySegmentJSON 是 `final_segments` 每项的口径：只存 ID 与顺序。
// assemblySegmentJSON keeps the segment id and its order only.
type assemblySegmentJSON struct {
	ID    string `json:"id"`
	Order int    `json:"order"`
}

// validAssemblyPurpose 判定用途是否落在冻结四值内（`context_assembly_records.purpose`
// 的 CHECK 逐字）。词表外的取值在这里挡下，不交给数据库拒绝。
// validAssemblyPurpose accepts exactly the four frozen purpose literals.
func validAssemblyPurpose(p core.AssemblyPurpose) bool {
	switch p {
	case core.PurposeChat, core.PurposeRerank, core.PurposeSummary, core.PurposeRetrieval:
		return true
	default:
		return false
	}
}

// Record 落一条装配记录。**无返回值**：任何失败只记 WARN，调用方照常继续。
// Record persists one record and never returns an error; every failure is a WARN.
func (s *assemblyStore) Record(ctx context.Context, record core.AssemblyRecord) {
	logger := slog.Default()
	if s != nil && s.logger != nil {
		logger = s.logger
	}
	if s == nil || s.db == nil || s.db.pool == nil {
		logger.Warn("装配记录写入失败——存储未装配，记录已丢弃",
			"run_id", record.RunID, "purpose", string(record.Purpose))
		return
	}
	if record.RunID == "" {
		logger.Warn("装配记录缺少 run_id——记录已丢弃（装配记录必须挂在 Run 上）",
			"purpose", string(record.Purpose))
		return
	}
	if !validAssemblyPurpose(record.Purpose) {
		logger.Warn("装配记录的 purpose 不在冻结四值内——记录已丢弃",
			"run_id", record.RunID, "purpose", string(record.Purpose))
		return
	}

	recordID := record.RecordID
	if recordID == "" {
		id, err := newKnowledgeID(assemblyRecordIDPrefix)
		if err != nil {
			logger.Warn("装配记录标识生成失败——记录已丢弃", "run_id", record.RunID, "error", err)
			return
		}
		recordID = id
	}
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = s.db.clock()
	}
	// `llm_call_id` 为 0 表示"尚未分配"（模型调用发生在装配之后），落 NULL 而不是 0：
	// 0 会被读成一条指向不存在的调用记录的外键。
	var llmCallID any
	if record.LLMCallID > 0 {
		llmCallID = record.LLMCallID
	}

	payloads := []any{
		assemblyBudgetJSON{
			ContextTokens:    record.Budget.ContextTokens,
			ProtocolReserve:  record.Budget.ProtocolReserve,
			OutputReserve:    record.Budget.OutputReserve,
			EstimatorVersion: record.Budget.EstimatorVersion,
		},
		candidatePayloads(record.Candidates),
		stringPayloads(record.Hits),
		stringPayloads(record.Decisions),
		stringPayloads(record.ReasonCodes),
		assemblyCompressionJSON{
			Enabled:              record.Compression.Enabled,
			Decision:             string(record.Compression.Decision),
			CompactionID:         record.Compression.CompactionID,
			CoveredFromMessageID: record.Compression.CoveredFromMessageID,
			CutMessageID:         record.Compression.CutMessageID,
			FirstKeptMessageID:   record.Compression.FirstKeptMessageID,
			StrategyVersion:      record.Compression.StrategyVersion,
			PromptVersion:        record.Compression.PromptVersion,
			TokensBefore:         record.Compression.TokensBefore,
			TokensAfter:          record.Compression.TokensAfter,
			ReasonCode:           record.Compression.ReasonCode,
		},
		assemblyHistoryTrimJSON{
			DroppedGroups:         record.HistoryTrim.DroppedGroups,
			FirstDroppedMessageID: record.HistoryTrim.FirstDroppedMessageID,
			CutMessageID:          record.HistoryTrim.CutMessageID,
			ReasonCode:            record.HistoryTrim.ReasonCode,
			TokensBefore:          record.HistoryTrim.TokensBefore,
			TokensAfter:           record.HistoryTrim.TokensAfter,
			StrategyVersion:       record.HistoryTrim.StrategyVersion,
		},
		segmentPayloads(record.FinalSegments),
	}
	encoded := make([]string, len(payloads))
	for i, payload := range payloads {
		raw, err := json.Marshal(payload)
		if err != nil {
			logger.Warn("装配记录的 jsonb 列编码失败——记录已丢弃",
				"run_id", record.RunID, "purpose", string(record.Purpose), "error", err)
			return
		}
		encoded[i] = string(raw)
	}

	if _, err := s.db.pool.ExecContext(ctx, assemblyInsert,
		recordID, record.RunID, record.StepID, llmCallID, string(record.Purpose),
		record.Agent, record.PromptVersion, record.ModelVersion,
		encoded[0], record.EstimatorVersion,
		encoded[1], encoded[2], encoded[3], encoded[4],
		encoded[5], encoded[6], encoded[7],
		createdAt.UTC().UnixMilli(),
	); err != nil {
		// 装配记录**可丢弃**：写失败不进调用方的错误链，也**不**重试（重试会在故障期
		// 放大压力）；但 WARN 必须带够定位信息，否则线上查不出"缺了哪一条"。
		logger.Warn("装配记录写入失败——记录已丢弃",
			"record_id", recordID, "run_id", record.RunID, "step_id", record.StepID,
			"purpose", string(record.Purpose), "error", err)
	}
}

// candidatePayloads 把候选项转成可编码的切片；nil 也返回空切片（`jsonb` 列拒绝 `null`）。
// candidatePayloads always returns a slice so the jsonb column never sees null.
func candidatePayloads(candidates []core.AssemblyCandidate) []assemblyCandidateJSON {
	out := make([]assemblyCandidateJSON, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, assemblyCandidateJSON{
			ChunkID:        candidate.ChunkID,
			VersionID:      candidate.VersionID,
			ContentHash:    candidate.ContentHash,
			TokenEstimate:  candidate.TokenEstimate,
			RetrievalScore: candidate.RetrievalScore,
			RerankScore:    candidate.RerankScore,
			Rank:           candidate.Rank,
			ACLDecision:    candidate.ACLDecision,
		})
	}
	return out
}

// stringPayloads 把字符串切片转成可编码的切片；nil 也返回空切片。
// stringPayloads always returns a slice so the jsonb column never sees null.
func stringPayloads(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// segmentPayloads 把末段转成可编码的切片；nil 也返回空切片。
// segmentPayloads always returns a slice so the jsonb column never sees null.
func segmentPayloads(segments []core.FinalSegment) []assemblySegmentJSON {
	out := make([]assemblySegmentJSON, 0, len(segments))
	for _, segment := range segments {
		out = append(out, assemblySegmentJSON{ID: segment.ID, Order: segment.Order})
	}
	return out
}

package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// ContextDiagnosis 区分记录事实与模型实际使用，缺口不表示正常。
// ContextDiagnosis separates recorded assembly from actual model use.
type ContextDiagnosis struct {
	RunID     string         `json:"run_id"`
	Retrieval *RetrievalView `json:"retrieval"`
	Rounds    []RoundView    `json:"rounds"`
	Other     []AssemblyView `json:"other"`
	Gaps      []EvidenceGap  `json:"gaps"`
	Grade     EvidenceGrade  `json:"grade"`
}

// CandidateView 仅包含允许候选的身份、分数和真实决定。
// CandidateView contains only allowed candidate identifiers and recorded decisions.
type CandidateView struct {
	ChunkID        string   `json:"chunk_id"`
	VersionID      string   `json:"version_id"`
	ContentHash    string   `json:"content_hash"`
	TokenEstimate  int      `json:"token_estimate"`
	RetrievalScore float64  `json:"retrieval_score"`
	RerankScore    *float64 `json:"rerank_score"`
	Rank           int      `json:"rank"`
	Decision       string   `json:"decision"`
	ACLDecision    string   `json:"acl_decision"`
}

// RetrievalView 保持领域整数形状，内部presence在JSON边界恢复为空值。
// RetrievalView preserves field presence instead of serializing missing integers as zero.
type RetrievalView struct {
	Candidates      []CandidateView `json:"candidates"`
	Hits            []string        `json:"hits"`
	ReasonCodes     []string        `json:"reason_codes"`
	TopK            int             `json:"top_k"`
	CandidateK      int             `json:"candidate_k"`
	BudgetTokens    int             `json:"budget_tokens"`
	FilteredCount   int             `json:"filtered_count"`
	FilteredReason  string          `json:"filtered_reason"`
	IndexGeneration int             `json:"index_generation"`
	present         map[string]bool
}

// MarshalJSON 使历史缺键保持null，并逐项说明计数与原因的证据档位。
// MarshalJSON retains nulls and grades count evidence separately from explanations.
func (r RetrievalView) MarshalJSON() ([]byte, error) {
	type plain RetrievalView
	raw, err := json.Marshal(plain(r))
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{"top_k", "candidate_k", "budget_tokens", "filtered_count", "index_generation"} {
		if !r.present[key] {
			fields[key] = nil
		}
	}
	countGrade := EvidenceInsufficient
	if r.present["filtered_count"] {
		countGrade = EvidenceDirect
	}
	assemblyGrade := EvidenceInsufficient
	if r.present["assembly"] {
		assemblyGrade = EvidenceDirect
	}
	fields["evidence"] = map[string]EvidenceGrade{"assembly": assemblyGrade, "filtered_count": countGrade, "filtered_reason": EvidenceInsufficient, "model_use": EvidenceNeedsOutputCheck}
	fields["filtered_note"] = "混合过滤计数：集合级 + 检索路径统计；原因与单位未统一；不能理解为被过滤文档数或当前查询命中数；逐项身份未记录"
	return json.Marshal(fields)
}

// HistoryTrimView 只含组数、切点、估算与策略，不含消息正文。
// HistoryTrimView contains trim evidence without message bodies.
type HistoryTrimView struct {
	DroppedGroups         int    `json:"dropped_groups"`
	FirstDroppedMessageID string `json:"first_dropped_message_id"`
	CutMessageID          string `json:"cut_message_id"`
	ReasonCode            string `json:"reason_code"`
	TokensBefore          int    `json:"tokens_before"`
	TokensAfter           int    `json:"tokens_after"`
	StrategyVersion       string `json:"strategy_version"`
}

// CompressionView 不返回摘要正文，只解释本轮真实压缩结论。
// CompressionView explains the recorded decision without summary text.
type CompressionView struct {
	Enabled              bool   `json:"enabled"`
	Decision             string `json:"decision"`
	CompactionID         string `json:"compaction_id"`
	CoveredFromMessageID string `json:"covered_from_message_id"`
	CutMessageID         string `json:"cut_message_id"`
	FirstKeptMessageID   string `json:"first_kept_message_id"`
	TokensBefore         int    `json:"tokens_before"`
	TokensAfter          int    `json:"tokens_after"`
	ReasonCode           string `json:"reason_code"`
}

// RoundView 的budget为空即未记录；装配段仅证明进入提示词。
// RoundView uses null for missing budgets; included segments never prove model use.
type RoundView struct {
	StepID      string           `json:"step_id"`
	Candidates  []CandidateView  `json:"candidates"`
	Hits        []string         `json:"hits"`
	HistoryTrim *HistoryTrimView `json:"history_trim"`
	Compression *CompressionView `json:"compression"`
	ReasonCodes []string         `json:"reason_codes"`
	budget      map[string]any
	segments    []string
}

// MarshalJSON 补充已有装配记录的预算、段ID和逐项档位，不增加业务结论。
// MarshalJSON exposes recorded budget and segment evidence without inferring use.
func (r RoundView) MarshalJSON() ([]byte, error) {
	type plain RoundView
	return json.Marshal(struct {
		plain
		Budget   map[string]any           `json:"budget"`
		Segments []string                 `json:"final_segments"`
		Evidence map[string]EvidenceGrade `json:"evidence"`
		Note     string                   `json:"note"`
	}{plain(r), r.budget, r.segments, map[string]EvidenceGrade{"assembly": EvidenceDirect, "model_use": EvidenceNeedsOutputCheck}, "已装配，使用需核对输出"})
}

// AssemblyView 保留非chat用途，不给它附会对话预算。
// AssemblyView keeps non-chat evidence separate from chat budgets.
type AssemblyView struct {
	RecordID    string   `json:"record_id"`
	Purpose     string   `json:"purpose"`
	StepID      string   `json:"step_id"`
	ReasonCodes []string `json:"reason_codes"`
}

// Context 读取可丢弃上下文证据，保留来源级缺口。
// Context reads disposable assembly evidence and preserves source-level gaps.
func (p *Projector) Context(ctx context.Context, runID string) (ContextDiagnosis, error) {
	if err := (Anchor{Kind: AnchorRun, Value: runID}).Validate(); err != nil {
		return ContextDiagnosis{}, err
	}
	out := ContextDiagnosis{RunID: runID, Rounds: []RoundView{}, Other: []AssemblyView{}, Gaps: []EvidenceGap{}, Grade: EvidenceInsufficient}
	if p.ports.Assembly == nil {
		out.Gaps = append(out.Gaps, factGap(SourceAssembly, GapNotSupported))
		return out, nil
	}
	var run RunFacts
	if p.ports.Runs != nil {
		var err error
		run, err = p.ports.Runs.RunFacts(ctx, runID)
		if err != nil {
			return ContextDiagnosis{}, err
		}
	}
	records, err := p.ports.Assembly.AssemblyByRun(ctx, runID)
	if err != nil {
		return ContextDiagnosis{}, fmt.Errorf("read context assembly: %w", err)
	}
	to := p.cfg.Now()
	if run.FinishedAt != nil {
		to = *run.FinishedAt
	}
	if len(records) == 0 {
		out.Gaps = append(out.Gaps, p.missingEvidence(ctx, SourceAssembly, run.StartedAt, to, GapNotRecorded)...)
	}
	for _, r := range records {
		switch r.Purpose {
		case core.PurposeRetrieval:
			out.Retrieval = retrievalView(r)
		case core.PurposeChat:
			out.Rounds = append(out.Rounds, roundView(r))
		default:
			out.Other = append(out.Other, AssemblyView{RecordID: r.RecordID, Purpose: string(r.Purpose), StepID: r.StepID, ReasonCodes: contextReasons(r.ReasonCodes)})
		}
	}
	events, gaps := p.loadDiagnostics(ctx, diagnostics.Query{RunID: runID, Limit: min(p.cfg.MaxEvents, diagnostics.MaxQueryLimit)}, run.StartedAt, to)
	out.Gaps = append(out.Gaps, gaps...)
	// 摘要与诊断事件不相加；仅在摘要缺失时以真实事件的原计数补充显示。
	// Never add summary and event counts; use an event only when the summary is absent.
	for _, event := range events {
		if event.Kind == string(diagnostics.KindRetrieval) && event.ReasonCode == string(diagnostics.ReasonPermissionDenied) {
			if out.Retrieval == nil {
				out.Retrieval = &RetrievalView{Candidates: []CandidateView{}, Hits: []string{}, ReasonCodes: []string{}, present: map[string]bool{}, FilteredReason: "not_distinguished"}
			}
			if !out.Retrieval.present["filtered_count"] {
				if count, ok := contextInteger(event.Attrs["count"]); ok {
					out.Retrieval.FilteredCount = count
					out.Retrieval.present["filtered_count"] = true
				}
			}
		}
	}
	if len(records) > 0 {
		out.Grade = EvidenceNeedsOutputCheck
	}
	out.Gaps = uniqueGaps(out.Gaps)
	return out, nil
}

func contextInteger(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, n >= 0
	case int64:
		return int(n), n >= 0 && int64(int(n)) == n
	case float64:
		if !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 && n < float64(math.MaxInt) && math.Trunc(n) == n {
			return int(n), true
		}
	}
	return 0, false
}

func contextCandidates(record core.AssemblyRecord) ([]CandidateView, []string) {
	candidates := make([]CandidateView, 0, len(record.Candidates))
	allowed := make(map[string]bool)
	for _, c := range record.Candidates {
		if c.ACLDecision != "allow" || c.ChunkID == "" {
			continue
		}
		decision := c.Decision
		if !slices.Contains([]string{"kept", "rank_cut", "budget_drop"}, decision) {
			decision = "not_recorded"
		}
		// 当前没有真实rerank生产端；旧非空数值列的0不能证明测得分数为0。
		// No rerank producer exists; a historical numeric zero is not a measured score.
		candidates = append(candidates, CandidateView{
			ChunkID: c.ChunkID, VersionID: c.VersionID, ContentHash: c.ContentHash,
			TokenEstimate: c.TokenEstimate, RetrievalScore: c.RetrievalScore, Rank: c.Rank, Decision: decision, ACLDecision: "allow",
		})
		allowed[c.ChunkID] = true
	}
	hits := make([]string, 0, len(record.Hits))
	for _, hit := range record.Hits {
		if allowed[hit] {
			hits = append(hits, hit)
		}
	}
	return candidates, hits
}

func retrievalView(record AssemblyRecordFact) *RetrievalView {
	candidates, hits := contextCandidates(record.AssemblyRecord)
	out := &RetrievalView{Candidates: candidates, Hits: hits, ReasonCodes: contextReasons(record.ReasonCodes), FilteredReason: "not_distinguished", present: map[string]bool{"assembly": true}}
	for key, target := range map[string]*int{"top_k": &out.TopK, "candidate_k": &out.CandidateK, "budget_tokens": &out.BudgetTokens, "filtered_count": &out.FilteredCount, "index_generation": &out.IndexGeneration} {
		if n, ok := contextInteger(record.Retrieval[key]); ok {
			*target, out.present[key] = n, true
		}
	}
	return out
}

func roundView(record AssemblyRecordFact) RoundView {
	candidates, hits := contextCandidates(record.AssemblyRecord)
	out := RoundView{StepID: record.StepID, Candidates: candidates, Hits: hits, ReasonCodes: contextReasons(record.ReasonCodes), segments: []string{}}
	if record.BudgetRecorded {
		out.budget = map[string]any{
			"context_tokens": record.Budget.ContextTokens, "protocol_reserve": record.Budget.ProtocolReserve,
			"output_reserve": record.Budget.OutputReserve, "estimator_version": record.EstimatorVersion,
		}
	}
	h := record.HistoryTrim
	if h.StrategyVersion != "" || h.ReasonCode != "" || h.DroppedGroups > 0 {
		out.HistoryTrim = &HistoryTrimView{
			DroppedGroups: h.DroppedGroups, FirstDroppedMessageID: h.FirstDroppedMessageID,
			CutMessageID: h.CutMessageID, ReasonCode: safeContextReason(h.ReasonCode), TokensBefore: h.TokensBefore, TokensAfter: h.TokensAfter, StrategyVersion: h.StrategyVersion,
		}
	}
	c := record.Compression
	if slices.Contains([]core.CompressionDecision{core.DecisionNone, core.DecisionCompressed, core.DecisionDegraded, core.DecisionRejected}, c.Decision) {
		out.Compression = &CompressionView{
			Enabled: c.Enabled, Decision: string(c.Decision), CompactionID: c.CompactionID,
			CoveredFromMessageID: c.CoveredFromMessageID, CutMessageID: c.CutMessageID, FirstKeptMessageID: c.FirstKeptMessageID,
			TokensBefore: c.TokensBefore, TokensAfter: c.TokensAfter, ReasonCode: safeContextReason(c.ReasonCode),
		}
	}
	for _, segment := range record.FinalSegments {
		if slices.Contains([]string{"system", "memory", "knowledge", "tools", "state", "summary", "history", "recent"}, segment.ID) {
			out.segments = append(out.segments, segment.ID)
		}
	}
	return out
}

func contextReasons(reasons []string) []string {
	out := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		out = append(out, safeContextReason(reason))
	}
	return out
}

func safeContextReason(reason string) string {
	if diagnosticsReason(reason) || reason == core.HistoryTrimReasonGroupDropped {
		return reason
	}
	switch reason {
	case "", "none", "not_needed", "not_configured", "compaction_timeout", "compaction_failed", "compaction_over_limit", "append_pending", "stale", "no_injectable_chain", "no_committed_head", "stale_ancestor", "chain_broken", "epoch_raced", "append_lock_timeout", "timeout", "cancelled", "unknown", "external_error", "runtime_error", "notify_disabled", "invalid_input", "sandbox_denied", "not_found", "audit_write_failed", "iteration_limit", "tool_call_limit", "run_timeout", "context_budget_exceeded", "unclassified":
		return reason
	default:
		return "unclassified"
	}
}

package observability

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

func contextProjector() (*Projector, *testFacts) {
	f := sameMillisecondFacts()
	f.assembly = []AssemblyRecordFact{{AssemblyRecord: core.AssemblyRecord{
		Purpose: core.PurposeRetrieval,
		Candidates: []core.AssemblyCandidate{
			{ChunkID: "kept", ACLDecision: "allow", Decision: "kept"},
			{ChunkID: "budget", ACLDecision: "allow", Decision: "budget_drop"},
			{ChunkID: "rank", ACLDecision: "allow", Decision: "rank_cut"},
			{ChunkID: "CANARY_DENIED_ID", ACLDecision: "deny", Decision: "kept"},
		},
		Hits: []string{"kept", "CANARY_DENIED_ID"},
	}, Retrieval: map[string]any{"filtered_count": float64(5), "budget_tokens": float64(0)}}}
	ports := f.ports()
	ports.Assembly = f
	return NewProjector(ports, ProjectorConfig{SampleRate: 1}), f
}

func TestObservability_ContextReadFailuresAndMissing(t *testing.T) {
	p, f := contextProjector()
	f.assemblyErr = errors.New("CANARY_ASSEMBLY_READ_ERROR")
	_, err := p.Context(t.Context(), "run_1")
	require.Error(t, err, "primary source failure must not become an empty diagnosis")
	f.assemblyErr, f.assembly = nil, nil
	f.cleanup = []CleanupRun{{TableKind: string(SourceAssembly), WindowStart: f.run.StartedAt, WindowEnd: f.run.StartedAt, Status: "succeeded", Deleted: 1}}
	got, err := p.Context(t.Context(), "run_1")
	require.NoError(t, err)
	require.Nil(t, got.Retrieval)
	require.Equal(t, EvidenceInsufficient, got.Grade)
	var cleaned bool
	for _, gap := range got.Gaps {
		if gap.Source == SourceAssembly && gap.Reason == GapRetentionWindowCleaned {
			cleaned = true
			require.Contains(t, gap.Detail, "无法确认")
		}
	}
	require.True(t, cleaned)
	f.cleanupErr = errors.New("CANARY_CLEANUP_READ_ERROR")
	got, err = p.Context(t.Context(), "run_1")
	require.NoError(t, err)
	require.Contains(t, got.Gaps, factGap(SourceAssembly, GapQueryFailed))
	timeline, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}})
	require.NoError(t, err)
	require.Contains(t, timeline.Gaps, factGap(SourceAssembly, GapQueryFailed))
}

func TestObservability_ContextFilteredNoIdentity(t *testing.T) {
	p, _ := contextProjector()
	got, err := p.Context(t.Context(), "run_1")
	require.NoError(t, err)
	require.NotNil(t, got.Retrieval)
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "CANARY_DENIED_ID")
	require.Contains(t, string(raw), "逐项身份未记录")
	require.Equal(t, []string{"kept"}, got.Retrieval.Hits)
	require.Equal(t, 5, got.Retrieval.FilteredCount)
}

func TestObservability_ContextFilteredReasonNotDirect(t *testing.T) {
	p, _ := contextProjector()
	got, err := p.Context(t.Context(), "run_1")
	require.NoError(t, err)
	raw, err := json.Marshal(got.Retrieval)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.Equal(t, "not_distinguished", fields["filtered_reason"])
	grades := fields["evidence"].(map[string]any)
	require.Equal(t, "direct", grades["filtered_count"])
	require.Equal(t, "insufficient", grades["filtered_reason"])
}

func TestObservability_ContextBudgetVsRankCut(t *testing.T) {
	p, _ := contextProjector()
	got, err := p.Context(t.Context(), "run_1")
	require.NoError(t, err)
	require.Len(t, got.Retrieval.Candidates, 3)
	require.Equal(t, "budget_drop", got.Retrieval.Candidates[1].Decision)
	require.Equal(t, "rank_cut", got.Retrieval.Candidates[2].Decision)
	require.Nil(t, got.Retrieval.Candidates[0].RerankScore, "no producer records a real rerank score")
	raw, err := json.Marshal(got.Retrieval)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.Equal(t, float64(0), fields["budget_tokens"])
	require.Nil(t, fields["top_k"], "historical missing threshold is not zero")
	require.Nil(t, fields["index_generation"])
}

func TestObservability_ContextNeedsOutputCheck(t *testing.T) {
	p, f := contextProjector()
	f.assembly = append(f.assembly, AssemblyRecordFact{AssemblyRecord: core.AssemblyRecord{
		Purpose: core.PurposeChat, StepID: "1",
		FinalSegments: []core.FinalSegment{{ID: "knowledge", Order: 1}}, Compression: core.CompressionRecord{Decision: core.DecisionNone},
	}})
	got, err := p.Context(t.Context(), "run_1")
	require.NoError(t, err)
	require.Equal(t, EvidenceNeedsOutputCheck, got.Grade)
	raw, err := json.Marshal(got.Rounds)
	require.NoError(t, err)
	require.Contains(t, string(raw), "已装配，使用需核对输出")
	require.Contains(t, string(raw), `"budget":null`)
	f.diagnosticErr = errors.New("CANARY_CONTEXT_DB_ERROR")
	got, err = p.Context(t.Context(), "run_1")
	require.NoError(t, err)
	require.Contains(t, got.Gaps, factGap(SourceDiagnostic, GapQueryFailed))
}

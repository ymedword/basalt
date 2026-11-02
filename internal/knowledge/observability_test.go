package knowledge

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObservabilityProducer_RetrievalDecisionsAndSummary(t *testing.T) {
	h := newRetrievalHarness(t)
	for _, name := range []string{"a", "b", "c"} {
		h.seedChunk(t, "public", name, "synthetic")
	}
	for i := range h.keyword.path.backend {
		h.keyword.path.backend[i].TokenEstimate = 5
		h.vectors.path.backend[i].TokenEstimate = 5
	}
	req := h.query("synthetic")
	// 既有候选宽度至少为默认topK=8；取9验证实际生效参数，不修改原检索规则。
	req.TopK, req.CandidateK, req.Budget = 2, 9, 5
	result, err := h.retriever.Retrieve(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	require.Len(t, h.assembly.records, 1)
	record := h.assembly.records[0]
	require.NotNil(t, record.Retrieval, "retrieval thresholds must be recorded")
	require.Equal(t, 2, record.Retrieval.TopK)
	require.Equal(t, 9, record.Retrieval.CandidateK)
	require.Equal(t, 5, record.Retrieval.BudgetTokens)
	require.Equal(t, "not_distinguished", record.Retrieval.FilteredReason)
	require.Equal(t, result.DeniedCount, record.Retrieval.FilteredCount)
	require.NotNil(t, record.Retrieval.IndexGeneration)
	require.Equal(t, 1, *record.Retrieval.IndexGeneration)
	require.Len(t, record.Candidates, 3)
	require.Equal(t, "kept", record.Candidates[0].Decision)
	require.Equal(t, "budget_drop", record.Candidates[1].Decision)
	require.Equal(t, "rank_cut", record.Candidates[2].Decision)
}

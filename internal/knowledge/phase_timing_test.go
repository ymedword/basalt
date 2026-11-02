package knowledge

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
)

type retrievalTimingSink struct{ phases []core.PhaseTiming }

func (s *retrievalTimingSink) RecordPhase(_ context.Context, p core.PhaseTiming) {
	s.phases = append(s.phases, p)
}

func TestPhaseTiming_RetrievalMeasuresRealPathsAndDegradation(t *testing.T) {
	h := newRetrievalHarness(t)
	h.seedChunk(t, "public", "public.md", "allowed text")
	sink := &retrievalTimingSink{}
	h.retriever.phases = sink
	result, err := h.retriever.Retrieve(t.Context(), h.query("text"))
	require.NoError(t, err)
	require.NotEmpty(t, result.Hits)
	var phases []string
	for _, p := range sink.phases {
		phases = append(phases, p.Phase)
		require.Equal(t, h.runID, p.RunID)
		require.Equal(t, "completed", p.Status)
		require.Equal(t, core.PhaseKnowledge, p.Parent)
	}
	require.Equal(t, []string{core.PhaseScope, core.PhaseKeyword, core.PhaseVector, core.PhaseFusion}, phases)
	sink.phases = nil
	h.vectors.path.err = errors.New("backend unavailable")
	_, err = h.retriever.Retrieve(t.Context(), h.query("text"))
	require.NoError(t, err)
	require.Equal(t, "failed", sink.phases[2].Status, "a degraded path retains its failure while retrieval succeeds")
	sink.phases = nil
	req := h.query("text")
	req.Embedding = nil
	_, err = h.retriever.Retrieve(t.Context(), req)
	require.NoError(t, err)
	for _, p := range sink.phases {
		require.NotEqual(t, core.PhaseVector, p.Phase, "an unexecuted vector query must not get a fabricated duration")
	}
	sink.phases = nil
	h.authz.err = core.ErrForbidden
	_, err = h.retriever.Retrieve(t.Context(), h.query("text"))
	require.ErrorIs(t, err, core.ErrForbidden)
	require.Len(t, sink.phases, 1)
	require.Equal(t, "skipped", sink.phases[0].Status, "scope gates must not become new run-failure alerts")
}

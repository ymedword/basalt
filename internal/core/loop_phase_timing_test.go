package core_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
)

type loopPhaseSink struct{ phases []core.PhaseTiming }

func (s *loopPhaseSink) RecordPhase(_ context.Context, p core.PhaseTiming) {
	s.phases = append(s.phases, p)
}

func TestPhaseTiming_ContextPreparationPerRound(t *testing.T) {
	sink := &loopPhaseSink{}
	h := newLoopHarness(t, func(h *loopHarness) { h.phases = sink })
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("done"))
	result, err := h.service.Run(ctx, h.req("read and summarize"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, result.Status)
	parents := map[string]core.PhaseTiming{}
	for _, p := range sink.phases {
		if p.Phase == core.PhaseContext {
			parents[p.StepID] = p
		}
	}
	require.Len(t, parents, 2)
	for _, p := range sink.phases {
		require.Equal(t, result.RunID, p.RunID)
		if p.Parent == core.PhaseContext {
			parent := parents[p.StepID]
			require.False(t, p.StartedAt.Before(parent.StartedAt))
			require.False(t, p.FinishedAt.After(parent.FinishedAt))
		}
	}
}

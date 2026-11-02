package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type phaseCapture func(context.Context, PhaseTiming)

func (f phaseCapture) RecordPhase(ctx context.Context, m PhaseTiming) { f(ctx, m) }

func TestPhaseTiming_OnceAndCancellationIsolation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var got []PhaseTiming
	sink := phaseCapture(func(ctx context.Context, m PhaseTiming) { require.NoError(t, ctx.Err()); got = append(got, m) })
	finish := BeginPhase(ctx, sink, PhaseContext, "", "run-1", "2", 0)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { finish(context.Canceled) })
	}
	wg.Wait()
	require.Len(t, got, 1)
	require.Equal(t, "cancelled", got[0].Status)
	require.Equal(t, "2", got[0].StepID)
	require.False(t, got[0].FinishedAt.Before(got[0].StartedAt))
	require.GreaterOrEqual(t, got[0].Duration.Nanoseconds(), int64(0))
}

func TestPhaseTiming_NoAnchorOrFaultyObserverCannotAffectBusiness(t *testing.T) {
	called := false
	sink := phaseCapture(func(context.Context, PhaseTiming) { called = true; panic("observer") })
	BeginPhase(t.Context(), sink, PhaseContext, "", "", "", 0)(nil)
	require.False(t, called)
	require.NotPanics(t, func() { BeginPhase(t.Context(), sink, PhaseContext, "", "run-1", "", 0)(nil) })
	require.True(t, called)
}

package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

type flowProgressProbe struct {
	before, after int
	outcome       core.ActionOutcome
}

func (p *flowProgressProbe) BeforeCall(context.Context, core.ToolCall) error { p.before++; return nil }

func (p *flowProgressProbe) AfterCall(_ context.Context, _ core.ToolCall, _ core.Result, outcome core.ActionOutcome) error {
	p.after++
	p.outcome = outcome
	return nil
}

func TestFlow_ExecutorOutcomeWriteFailureDoesNotAdvanceProgress(t *testing.T) {
	p := newFlowExecutorProbe(t)
	progress := &flowProgressProbe{}
	p.execution.Flow.Progress = progress
	injected := errors.New("injected outcome write failure")
	p.ledger.record = func(core.ActionOutcome) error { return injected }
	_, out, err := p.run()
	require.ErrorIs(t, err, injected)
	require.True(t, out.Dispatched && out.AuditConfirmed)
	require.EqualValues(t, 1, p.received.Load())
	require.Equal(t, 1, progress.before)
	require.Zero(t, progress.after, "unpersisted observation must not advance progress")
	p = newFlowExecutorProbe(t)
	progress = &flowProgressProbe{}
	p.execution.Flow.Progress = progress
	_, out, err = p.run()
	require.NoError(t, err)
	require.Equal(t, 1, progress.after, "positive control must deliver a persisted observation")
	require.Equal(t, out, progress.outcome)
}

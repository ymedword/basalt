package core

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type flowTerminalProbe struct {
	RunStore
	calls    int
	err      error
	deadline time.Time
	status   RunStatus
}

func (p *flowTerminalProbe) FinishRun(ctx context.Context, _ string, status RunStatus, _ string, _, _ int, _ time.Time) error {
	p.calls++
	p.err = ctx.Err()
	p.deadline, _ = ctx.Deadline()
	p.status = status
	return p.err
}

func TestFlow_CancelledLeafUsesBoundedOriginalTerminalWriter(t *testing.T) {
	for _, isFlow := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary-compatibility", true: "flow-cleanup"}[isFlow], func(t *testing.T) {
			store := &flowTerminalProbe{}
			service := &AgentService{opts: ServiceOptions{Runs: store, Now: time.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
			env := &RunEnv{}
			if isFlow {
				env.Flow = &FlowRunContext{}
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			result := service.finish(ctx, "original-run", env, RunResult{Status: StatusCancelled})
			require.Equal(t, 1, store.calls)
			require.Equal(t, StatusCancelled, store.status)
			require.Equal(t, "original-run", result.RunID)
			if isFlow {
				require.NoError(t, store.err)
				require.WithinDuration(t, time.Now().Add(5*time.Second), store.deadline, 100*time.Millisecond)
			} else {
				require.ErrorIs(t, store.err, context.Canceled)
			}
			require.ErrorIs(t, ctx.Err(), context.Canceled, "cleanup must not mutate the original execution context")
		})
	}
}

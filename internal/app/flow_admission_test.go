package app

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

func TestFlow_AdmissionWrapperAlwaysReturnsOriginalSharedTicket(t *testing.T) {
	for _, outcome := range []string{"complete", "error", "cancel", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			port, err := callers.NewMemoryConcurrency(func(string) (int, bool) { return 2, true })
			require.NoError(t, err)
			limiter := callers.NewLimiter(callers.LimiterDeps{Concurrency: port})
			admission := &runAdmission{limiter: limiter}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			called := false
			res, err := admission.run(ctx, core.RunRequest{Principal: core.LocalAdminCLIPrincipal(), RunID: "flow-admission-probe", Flow: &core.FlowRunContext{TaskID: "task"}}, func(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
				called = true
				_, err := limiter.AdmitFlowRun(ctx, req.Principal, 1)
				require.ErrorIs(t, err, callers.ErrConcurrencyLimited, "Flow sublimit is already held")
				ordinary, err := limiter.AdmitRun(ctx, req.Principal)
				require.NoError(t, err, "reserved slot belongs to ordinary traffic in the same table")
				require.NoError(t, limiter.ReleaseRun(ctx, ordinary))
				switch outcome {
				case "error":
					return core.RunResult{}, errors.New("injected leaf admission failure")
				case "cancel":
					cancel()
					return core.RunResult{Status: core.StatusCancelled}, nil
				case "panic":
					panic("injected leaf panic")
				default:
					return core.RunResult{Status: core.StatusCompleted}, nil
				}
			})
			require.True(t, called)
			if outcome == "error" {
				require.ErrorContains(t, err, "injected leaf")
			} else {
				require.NoError(t, err)
			}
			if outcome == "panic" {
				require.Equal(t, core.StatusFailed, res.Status)
				require.Contains(t, res.Reason, "panic")
			}
			ticket, err := limiter.AdmitFlowRun(context.Background(), core.LocalAdminCLIPrincipal(), 1)
			require.NoError(t, err, "production defer must return the Flow and total slot on every exit")
			require.NoError(t, limiter.ReleaseRun(context.Background(), ticket))
		})
	}
}

func TestFlow_AdmissionMissingLimiterFailsClosed(t *testing.T) {
	called := false
	_, err := (&runAdmission{}).run(t.Context(), core.RunRequest{Principal: core.LocalAdminCLIPrincipal(), Flow: &core.FlowRunContext{TaskID: "task"}}, func(context.Context, core.RunRequest) (core.RunResult, error) {
		called = true
		return core.RunResult{}, nil
	})
	require.ErrorIs(t, err, core.ErrFlowDispatch)
	require.False(t, called)
}

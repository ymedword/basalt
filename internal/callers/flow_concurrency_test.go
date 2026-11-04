package callers

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

func flowCallerForTest(key string) core.Principal {
	p := testCallerPrincipal("c1")
	p.KeyID = key
	p.Scopes = append(p.Scopes, core.ScopeFlowsWrite)
	return p
}

func TestFlow_CallerReserveSharesOriginalTotalAcrossKeys(t *testing.T) {
	port, err := NewMemoryConcurrency(func(string) (int, bool) { return 4, true })
	require.NoError(t, err)
	limiter := NewLimiter(LimiterDeps{Concurrency: port})
	var flowTickets []RunTicket
	for _, key := range []string{"key-a", "key-b", "key-c"} {
		ticket, err := limiter.AdmitFlowRun(t.Context(), flowCallerForTest(key), 1)
		require.NoError(t, err)
		flowTickets = append(flowTickets, ticket)
	}
	_, err = limiter.AdmitFlowRun(t.Context(), flowCallerForTest("key-d"), 1)
	require.ErrorIs(t, err, ErrConcurrencyLimited, "all keys share the same Flow sublimit")
	interactive, err := limiter.AdmitRun(t.Context(), testCallerPrincipal("c1"))
	require.NoError(t, err, "reserved room must remain usable by ordinary traffic")
	_, err = limiter.AdmitRun(t.Context(), testCallerPrincipal("c1"))
	require.ErrorIs(t, err, ErrConcurrencyLimited, "Flow occupies the original total counter")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	require.NoError(t, limiter.ReleaseRun(cancelled, flowTickets[0]))
	require.NoError(t, limiter.ReleaseRun(cancelled, flowTickets[0]), "duplicate release must not free another active slot")
	newFlow, err := limiter.AdmitFlowRun(t.Context(), flowCallerForTest("key-e"), 1)
	require.NoError(t, err)
	_, err = limiter.AdmitFlowRun(t.Context(), flowCallerForTest("key-f"), 1)
	require.ErrorIs(t, err, ErrConcurrencyLimited)
	for _, ticket := range append(flowTickets[1:], newFlow, interactive) {
		require.NoError(t, limiter.ReleaseRun(t.Context(), ticket))
	}
}

func TestFlow_CallerSingleSlotNeedsExplicitZeroReserve(t *testing.T) {
	port, err := NewMemoryConcurrency(func(string) (int, bool) { return 1, true })
	require.NoError(t, err)
	limiter := NewLimiter(LimiterDeps{Concurrency: port})
	_, err = limiter.AdmitFlowRun(t.Context(), flowCallerForTest("key-a"), 1)
	require.ErrorIs(t, err, ErrConcurrencyLimited)
	ticket, err := limiter.AdmitFlowRun(t.Context(), flowCallerForTest("key-a"), 0)
	require.NoError(t, err)
	_, err = limiter.AdmitRun(t.Context(), testCallerPrincipal("c1"))
	require.ErrorIs(t, err, ErrConcurrencyLimited)
	require.NoError(t, limiter.ReleaseRun(t.Context(), ticket))
	_, err = limiter.AdmitFlowRun(t.Context(), testCallerPrincipal("c1"), 0)
	require.ErrorIs(t, err, core.ErrUnauthorized, "old scopes must not automatically grant flows:write")
}

func TestFlow_CallerMixedAdmissionIsAtomic(t *testing.T) {
	port, err := NewMemoryConcurrency(func(string) (int, bool) { return 4, true })
	require.NoError(t, err)
	limiter := NewLimiter(LimiterDeps{Concurrency: port})
	start := make(chan struct{})
	results := make(chan struct {
		ticket RunTicket
		flow   bool
	}, 64)
	var group sync.WaitGroup
	for i := range 64 {
		group.Go(func() {
			<-start
			flow := i%2 == 0
			var ticket RunTicket
			var err error
			if flow {
				ticket, err = limiter.AdmitFlowRun(t.Context(), flowCallerForTest("shared"), 1)
			} else {
				ticket, err = limiter.AdmitRun(t.Context(), testCallerPrincipal("c1"))
			}
			if err == nil {
				results <- struct {
					ticket RunTicket
					flow   bool
				}{ticket, flow}
			}
		})
	}
	close(start)
	group.Wait()
	close(results)
	total, flows := 0, 0
	for result := range results {
		total++
		if result.flow {
			flows++
		}
		t.Cleanup(func() { require.NoError(t, limiter.ReleaseRun(context.Background(), result.ticket)) })
	}
	require.Equal(t, 4, total, "positive pressure must actually fill the shared table")
	require.LessOrEqual(t, flows, 3)
}

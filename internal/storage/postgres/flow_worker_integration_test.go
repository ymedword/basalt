//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

type flowWorkerRunner struct {
	store                  *FlowStore
	active, maximum, calls atomic.Int64
	release                chan struct{}
	started                chan struct{}
}

func (r *flowWorkerRunner) Run(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	active := r.active.Add(1)
	defer r.active.Add(-1)
	for old := r.maximum.Load(); old < active && !r.maximum.CompareAndSwap(old, active); old = r.maximum.Load() {
	}
	r.calls.Add(1)
	if r.started != nil {
		select {
		case r.started <- struct{}{}:
		default:
		}
	}
	if r.release != nil {
		<-ctx.Done()
		<-r.release
		return flow.NodeRunResult{Run: core.RunResult{Status: core.StatusCancelled}}, nil
	}
	select {
	case <-time.After(75 * time.Millisecond):
	case <-ctx.Done():
		return flow.NodeRunResult{Run: core.RunResult{Status: core.StatusCancelled}}, nil
	}
	probe := &flowAdvanceRunner{store: r.store, output: json.RawMessage(`{"answer":"worker result"}`)}
	return probe.Run(ctx, req)
}

func (r *flowWorkerRunner) Joined(context.Context, flow.Task) (bool, error) {
	return r.active.Load() == 0, nil
}

func TestFlow_WorkerUsesBoundedPoolAndDurableQueue(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	service.Config.MaxActive = 2
	service.Config.PollIntervalMS = 100
	runner := &flowWorkerRunner{store: s}
	service.Runner = runner
	var ids []string
	for i := range 5 {
		task, err := service.Start(t.Context(), startFlowRequest(def, fmt.Sprintf("worker-%d", i)))
		require.NoError(t, err)
		ids = append(ids, task.ID)
	}
	var startup atomic.Int64
	worker := &flow.Worker{Service: service, Repository: s, Startup: func(context.Context) error { startup.Add(1); return nil }}
	require.Zero(t, runner.calls.Load(), "constructing does not start work")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.After(6 * time.Second)
	for {
		complete := 0
		for _, id := range ids {
			task, err := s.ReadForWorker(t.Context(), id)
			require.NoError(t, err)
			if task.Status == flow.Succeeded {
				complete++
			}
		}
		if complete == len(ids) {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("worker exited early: %v", err)
		case <-deadline:
			t.Fatal("durable queue did not complete")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	require.NoError(t, <-done)
	require.Zero(t, runner.active.Load())
	require.EqualValues(t, 5, runner.calls.Load())
	require.EqualValues(t, 2, runner.maximum.Load())
	require.EqualValues(t, 1, startup.Load())
}

func TestFlow_WorkerShutdownWaitsForActualJoin(t *testing.T) {
	s, def := newFlowStoreTest(t)
	service := flowServiceForStore(s)
	service.Config.MaxActive = 1
	service.Config.PollIntervalMS = 100
	runner := &flowWorkerRunner{store: s, release: make(chan struct{}), started: make(chan struct{}, 1)}
	service.Runner = runner
	_, err := service.Start(t.Context(), startFlowRequest(def, "worker-shutdown"))
	require.NoError(t, err)
	worker := &flow.Worker{Service: service, Repository: s, Startup: func(context.Context) error { return nil }}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case <-runner.started:
	case <-time.After(3 * time.Second):
		t.Fatal("leaf did not start")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before actual leaf join: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	require.EqualValues(t, 1, runner.active.Load())
	close(runner.release)
	require.NoError(t, <-done)
	require.Zero(t, runner.active.Load())
	var count int
	require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_tasks WHERE cancel_requested`).Scan(&count))
	require.Zero(t, count, "shutdown is not user cancellation")
}

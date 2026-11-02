package web

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type observationLifecycleStub struct {
	context            context.Context
	started            chan struct{}
	closed             atomic.Int32
	startErr, closeErr error
}

func (s *observationLifecycleStub) Start(ctx context.Context) error {
	s.context = ctx
	close(s.started)
	return s.startErr
}
func (s *observationLifecycleStub) Close() error { s.closed.Add(1); return s.closeErr }

func TestObservability_ServerOwnsLifecycle(t *testing.T) {
	for _, mode := range []string{"cancel", "start_failure", "close_failure"} {
		t.Run(mode, func(t *testing.T) {
			worker := &observationLifecycleStub{started: make(chan struct{})}
			if mode == "start_failure" {
				worker.startErr = errors.New("synthetic observation start failure")
			}
			if mode == "close_failure" {
				worker.closeErr = errors.New("synthetic observation close failure")
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			server := newTestServer(t, func(opts *Options) { opts.Listener = listener; opts.ObservationLifecycle = worker })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- server.Run(ctx) }()
			select {
			case <-worker.started:
			case <-time.After(time.Second):
				t.Fatal("observation lifecycle was not started")
			}
			require.True(t, worker.context == ctx, "HTTP and observation must receive the same cancellation source")
			if mode != "start_failure" {
				cancel()
			}
			select {
			case err := <-done:
				if mode == "start_failure" {
					require.ErrorIs(t, err, worker.startErr)
				} else if mode == "close_failure" {
					require.ErrorIs(t, err, worker.closeErr)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("server did not wait for observation shutdown")
			}
			require.Equal(t, int32(1), worker.closed.Load(), "failed start must also release the observation lifecycle")
			reused, err := net.Listen("tcp", listener.Addr().String())
			require.NoError(t, err, "all exit paths must release the listener")
			require.NoError(t, reused.Close())
		})
	}
}

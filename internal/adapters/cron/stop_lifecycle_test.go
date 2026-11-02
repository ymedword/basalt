package cronadapter_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	cronadapter "github.com/ymedword/basalt/internal/adapters/cron"
)

type stopLogGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newStopLogGate() *stopLogGate {
	return &stopLogGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *stopLogGate) unblock() { g.once.Do(func() { close(g.release) }) }

type stopLogHandler struct {
	slog.Handler
	mu    sync.Mutex
	gates []*stopLogGate
	next  int
}

func (h *stopLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "stop" {
		h.mu.Lock()
		index := h.next
		h.next++
		h.mu.Unlock()
		if index < len(h.gates) {
			gate := h.gates[index]
			close(gate.entered)
			<-gate.release
		}
	}
	return h.Handler.Handle(ctx, record)
}

func gatedEngine(t *testing.T, gates ...*stopLogGate) *cronadapter.Engine {
	t.Helper()
	h := &stopLogHandler{Handler: slog.NewTextHandler(io.Discard, nil), gates: gates}
	e := cronadapter.New(slog.New(h))
	t.Cleanup(func() {
		for _, gate := range gates {
			gate.unblock()
		}
		awaitClosed(t, e.Stop().Done(), "cleanup must finish")
	})
	return e
}

func awaitClosed(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func requireStillStopping(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("Stop completed before the real scheduler loop finished its stop log")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestEngine_StopWaitsForLoopCompletion blocks the real final loop callback, not a job.
func TestEngine_StopWaitsForLoopCompletion(t *testing.T) {
	gate := newStopLogGate()
	e := gatedEngine(t, gate)
	e.Start()
	stopped := e.Stop()
	awaitClosed(t, gate.entered, "real stop log was not reached")
	requireStillStopping(t, stopped.Done())
	gate.unblock()
	awaitClosed(t, stopped.Done(), "Stop must complete after the real loop finishes")
}

// TestEngine_StopBeforeStartAndRepeated preserves idle stops and subsequent generations.
func TestEngine_StopBeforeStartAndRepeated(t *testing.T) {
	e := newTestEngine()
	awaitClosed(t, e.Stop().Done(), "idle Stop must complete")
	for range 3 {
		e.Start()
		e.Start()
		awaitClosed(t, e.Stop().Done(), "started Stop must complete")
		awaitClosed(t, e.Stop().Done(), "repeated Stop must complete")
	}
}

// TestEngine_StopWithInfoFiltered still joins the loop when routine logs are disabled.
func TestEngine_StopWithInfoFiltered(t *testing.T) {
	e := cronadapter.New(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})))
	e.Start()
	awaitClosed(t, e.Stop().Done(), "filtering Info must not suppress lifecycle completion")
}

// TestEngine_ConcurrentStopsJoinLoop gives every caller the same completion guarantee.
func TestEngine_ConcurrentStopsJoinLoop(t *testing.T) {
	gate := newStopLogGate()
	e := gatedEngine(t, gate)
	e.Start()
	contexts := make(chan context.Context, 16)
	for range 16 {
		go func() { contexts <- e.Stop() }()
	}
	var stops []context.Context
	for range 16 {
		select {
		case stopped := <-contexts:
			stops = append(stops, stopped)
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent Stop did not return its completion context")
		}
	}
	awaitClosed(t, gate.entered, "real stop log was not reached")
	for _, stopped := range stops {
		select {
		case <-stopped.Done():
			t.Fatal("concurrent Stop bypassed the loop completion barrier")
		default:
		}
	}
	gate.unblock()
	for _, stopped := range stops {
		awaitClosed(t, stopped.Done(), "all Stop callers must finish")
	}
}

// TestEngine_RestartWaitsForPreviousLoop prevents a previous stop from confirming a new one.
func TestEngine_RestartWaitsForPreviousLoop(t *testing.T) {
	first, second := newStopLogGate(), newStopLogGate()
	e := gatedEngine(t, first, second)
	e.Start()
	oldStop := e.Stop()
	awaitClosed(t, first.entered, "first generation did not stop")
	started := make(chan struct{})
	go func() { e.Start(); close(started) }()
	requireStillStopping(t, started)
	first.unblock()
	awaitClosed(t, started, "restart must proceed after the previous loop finishes")
	awaitClosed(t, oldStop.Done(), "old generation must complete")
	newStop := e.Stop()
	awaitClosed(t, second.entered, "second generation did not stop")
	requireStillStopping(t, newStop.Done())
	second.unblock()
	awaitClosed(t, newStop.Done(), "new generation must complete independently")
}

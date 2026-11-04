package tool

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

type us20Store struct {
	mu                                        sync.Mutex
	active, countCalls                        int
	countErr, beginErr, runningErr, finishErr error
	order                                     []string
	records                                   []core.SandboxExecution
	results                                   []core.SandboxExecutionResult
}

func (s *us20Store) BeginSandboxExecution(_ context.Context, r core.SandboxExecution) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "begin")
	if s.beginErr != nil {
		return s.beginErr
	}
	s.records = append(s.records, r)
	s.active++
	return nil
}

func (s *us20Store) MarkSandboxRunning(context.Context, int64, time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "running")
	return s.runningErr
}

func (s *us20Store) FinishSandboxExecution(_ context.Context, r core.SandboxExecutionResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "finish")
	if s.finishErr != nil {
		return s.finishErr
	}
	s.results = append(s.results, r)
	if r.CleanupState == core.CleanupCleaned {
		s.active--
	}
	return nil
}

func (s *us20Store) CountActiveSandboxExecutions(context.Context, string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.countCalls++
	return s.active, s.countErr
}

func (s *us20Store) MarkSandboxCleaned(context.Context, int64, core.SandboxState, core.CleanupState, string, time.Time) error {
	return nil
}

func us20Admission(t *testing.T, store *us20Store) *SandboxAdmission {
	t.Helper()
	gate := NewSandboxAdmission()
	if err := gate.RefreshBaseline(context.Background(), store, "inst_test"); err != nil {
		t.Fatal(err)
	}
	return gate
}

func TestUS20_AdmissionStoreFailureDenies(t *testing.T) {
	g := NewSandboxAdmission()
	store := &us20Store{countErr: errors.New("DB down")}
	if g.RefreshBaseline(context.Background(), store, "inst_test") == nil {
		t.Fatal("query error ignored")
	}
	reason, err := g.acquire(2)
	if reason != "admission_unavailable" || !core.IsSandboxViolation(err) {
		t.Fatalf("uninitialized admitted: %s %v", reason, err)
	}
}

func TestUS20_AdmissionDoesNotDoubleCountLedger(t *testing.T) {
	s := &us20Store{}
	g := us20Admission(t, s)
	for i := 0; i < 2; i++ {
		if reason, err := g.acquire(2); err != nil {
			t.Fatalf("slot %d double counted %s %v", i, reason, err)
		}
		if err := s.BeginSandboxExecution(context.Background(), core.SandboxExecution{}); err != nil {
			t.Fatal(err)
		}
	}
	if reason, _ := g.acquire(2); reason != "admission_rejected" {
		t.Fatal("over-admitted")
	}
	if s.countCalls != 1 {
		t.Fatal("count refreshed during execution")
	}
	g.release()
	g.release()
}

func TestUS20_MixedTierAdmission(t *testing.T) {
	g := us20Admission(t, &us20Store{})
	oci := func() error { _, err := g.acquire(1); return err }
	code := func() error {
		reason, err := g.acquire(1)
		if reason != "admission_rejected" {
			t.Errorf("gvisor did not share OCI occupancy: %s", reason)
		}
		return err
	}
	if err := oci(); err != nil {
		t.Fatal(err)
	}
	if code() == nil {
		t.Fatal("separate tier limits")
	}
	g.release()
}

func TestUS20_PinnedSlotStaysOccupied(t *testing.T) {
	g := us20Admission(t, &us20Store{})
	if _, err := g.acquire(1); err != nil {
		t.Fatal(err)
	}
	g.pin()
	g.release()
	if reason, _ := g.acquire(1); reason != "admission_rejected" {
		t.Fatal("unknown cleanup released capacity")
	}
}

func TestUS20_ConcurrentAdmissionRace(t *testing.T) {
	const limit = 4
	g := us20Admission(t, &us20Store{})
	start := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	var admitted atomic.Int32
	results := make(chan bool, 40)
	for i := 0; i < 40; i++ {
		wg.Go(func() {
			<-start
			_, err := g.acquire(limit)
			if err != nil {
				results <- false
				return
			}
			admitted.Add(1)
			results <- true
			<-release
			g.release()
		})
	}
	close(start)
	for i := 0; i < 40; i++ {
		<-results
	}
	if got := admitted.Load(); got != limit {
		t.Errorf("concurrent occupancy=%d want=%d", got, limit)
	}
	close(release)
	wg.Wait()
	for i := 0; i < limit; i++ {
		if _, err := g.acquire(limit); err != nil {
			t.Fatal("slot leak", err)
		}
	}
	for i := 0; i < limit; i++ {
		g.release()
	}
}

func TestUS20_SandboxLedger_Order(t *testing.T) {
	s := &us20Store{}
	l := NewSandboxLedger(s, "inst_test", "gvisor", "runsc", "sha256:x", nil)
	env := core.ToolEnv{ToolInvocationID: 1, RunID: "run", AgentName: "agent"}
	if err := l.begin(context.Background(), env, "container"); err != nil {
		t.Fatal(err)
	}
	if err := l.markRunning(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	r := l.finish(context.Background(), env, core.CommandResult{Execution: &core.CommandExecution{State: core.SandboxFinished}}, nil, "", core.CleanupCleaned, []artifact{{Name: "a", Size: 5}})
	if strings.Join(s.order, ",") != "begin,running,finish" || r.ArtifactBytes != 5 || r.ArtifactCount != 1 || r.State != core.SandboxFinished {
		t.Fatalf("ledger order/result: %v %+v", s.order, r)
	}
	s.beginErr = errors.New("prewrite")
	if l.begin(context.Background(), env, "container") == nil {
		t.Fatal("begin failure hidden")
	}
}

func TestUS20_SandboxLedger_FinishUnknown(t *testing.T) {
	for _, exec := range []*core.CommandExecution{nil, {State: core.SandboxUnknown}} {
		s := &us20Store{}
		l := NewSandboxLedger(s, "i", "gvisor", "runsc", "image", nil)
		r := l.finish(context.Background(), core.ToolEnv{ToolInvocationID: 1}, core.CommandResult{Execution: exec}, errors.New("lost"), "state_unknown", core.CleanupFailed, nil)
		if r.State != core.SandboxUnknown || r.CleanupState != core.CleanupFailed {
			t.Fatalf("unknown coerced: %+v", r)
		}
	}
}

func TestUS20_SandboxLedger_StoreFailureWarns(t *testing.T) {
	var log bytes.Buffer
	s := &us20Store{runningErr: errors.New("db down"), finishErr: errors.New("db down")}
	l := NewSandboxLedger(s, "i", "oci", "runc", "image", slog.New(slog.NewTextHandler(&log, nil)))
	env := core.ToolEnv{ToolInvocationID: 1}
	_ = l.markRunning(context.Background(), env)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := l.finish(ctx, env, core.CommandResult{Execution: &core.CommandExecution{State: core.SandboxFinished}}, nil, "", core.CleanupCleaned, nil)
	if r.State != core.SandboxFinished || strings.Count(log.String(), "level=WARN") != 2 || len(s.order) != 2 {
		t.Fatalf("finish lost truth or WARN: %+v %s", r, log.String())
	}
}

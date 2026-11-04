package app

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// Construction is inert. Only serve explicitly starts this single lifecycle
// driver, which owns the already bounded Flow.Worker pool and joins before DB close.
type flowRuntime struct {
	service   *flow.Service
	publisher *flow.Publisher
	worker    *flow.Worker
	runner    *flowNodeRunner
	store     *postgres.FlowStore
	logger    *slog.Logger
	mu        sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	runErr    error
	closed    bool
	authReady chan struct{}
	authOnce  sync.Once
}

func (r *flowRuntime) authenticationReady() {
	if r != nil && r.authReady != nil {
		r.authOnce.Do(func() { close(r.authReady) })
	}
}

func (r *flowRuntime) prepare(ctx context.Context) error {
	// newRuntime already holds the unique process lock and has completed the
	// original Run recovery. An old-process leaf cannot still be executing here.
	// Recovery scans the persisted hard upper bound, even if today's admission
	// ceiling was lowered after those roots had already been accepted.
	roots, err := r.store.OpenRoots(ctx, 32)
	if err != nil {
		return err
	}
	for _, task := range roots {
		if task.Status != flow.Running {
			continue
		}
		scopes := task.Contract.Scope.Scopes
		if task.AuthorizingKind == string(core.AuthorizingKindAdminBuiltin) {
			scopes = nil
		}
		p, err := core.OwnedPrincipal(task.CallerID, core.AuthorizingKind(task.AuthorizingKind), task.AuthorizingKeyID, scopes, core.ActorRecovery)
		if err != nil {
			return err
		}
		if err := r.store.ParkTask(ctx, p, task.ID, task.Version, "startup_reconciliation_required"); err != nil && !errors.Is(err, core.ErrPreconditionFailed) && !errors.Is(err, flow.ErrTaskClosed) {
			return err
		}
	}
	r.runner.mu.Lock()
	r.runner.startupComplete = true
	r.runner.mu.Unlock()
	return nil
}

func (r *flowRuntime) Start(ctx context.Context) error {
	if r == nil || r.service == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil || r.closed {
		return flow.ErrStateConflict
	}
	if err := r.prepare(ctx); err != nil {
		return err
	}
	// Disabling background advancement must not strand old tasks in a false
	// running state or prevent authenticated cancellation after process recovery.
	if !r.service.Config.Enabled {
		return nil
	}
	workerCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	done := make(chan struct{})
	r.done = done
	// Capture the channel before Close clears the lifecycle fields. Reading
	// r.done in the goroutine could send to nil and strand the actual join.
	go func() {
		err := r.worker.Run(workerCtx)
		r.mu.Lock()
		r.runErr = err
		close(done)
		r.mu.Unlock()
	}()
	return nil
}

func (r *flowRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.closed = true
	r.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	// Worker retains all resources until the actual leaf joins. Its configured
	// grace logs overruns, but never turns a timeout into evidence of a stopped call.
	<-done
	r.mu.Lock()
	err := r.runErr
	r.mu.Unlock()
	// 只忽略纯取消（含被 errors.Join 单元素包装的取消）；Join 一旦携带数据库
	// 或 drain 错误，必须保留——由 CancellationOnly 逐叶判定。
	// Ignore only cancellation-only errors, including a join that wraps just the
	// cancellation sentinel; any joined database or drain failure is preserved.
	if flow.CancellationOnly(err, context.Canceled) {
		return nil
	}
	return err
}

func runtimeFlowConfig(c flow.Config, instanceRuns int) (flow.Config, error) {
	if c.MaxActive == 0 && !c.Enabled {
		c = flow.DefaultConfig()
	}
	if instanceRuns == 0 {
		instanceRuns = 32
	}
	return c, c.Validate(instanceRuns, 70*time.Second)
}

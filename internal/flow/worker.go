package flow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// Worker is a fixed bounded pool. Construction starts nothing. Startup must
// reconcile the existing Run ledger and prove the old local process has exited;
// it is an app lifecycle prerequisite, never inferred from an empty Go map.
type Worker struct {
	Service    *Service
	Repository WorkerRepository
	Startup    func(context.Context) error
	Logger     *slog.Logger
	mu         sync.Mutex
	running    bool
}

type (
	parkedRepository interface {
		ParkTask(context.Context, core.Principal, string, TaskVersion, string) error
	}
	workItem struct {
		task      Task
		principal core.Principal
	}
)

// Run owns exactly MaxActive workers and at most MaxActive active root identities.
// A root is released only after Advance really returns. No per-task goroutine or
// in-memory unbounded queue is created, and shutdown waits for actual joins.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil || w.Service == nil || w.Repository == nil {
		return ErrStateConflict
	}
	c := w.Service.Config
	if !c.Enabled {
		return nil
	}
	if c.MaxActive < 1 || c.MaxActive > 8 || c.PollIntervalMS < 100 || c.PollIntervalMS > 5000 || c.ShutdownGraceSeconds < 1 || c.ShutdownGraceSeconds > 70 || w.Startup == nil {
		return ErrStateConflict
	}
	if err := c.Validate(32, 70*time.Second); err != nil {
		return err
	}
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return ErrStateConflict
	}
	w.running = true
	w.mu.Unlock()
	defer func() { w.mu.Lock(); w.running = false; w.mu.Unlock() }()
	if err := w.Startup(ctx); err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan workItem)
	finished := make(chan string, c.MaxActive)
	var group sync.WaitGroup
	for range c.MaxActive {
		group.Go(func() {
			for item := range jobs {
				var advanced Task
				var err error
				if item.task.Status == Blocked && item.task.Reason == "bounded_rework_required" {
					advanced, err = w.Service.rework(workerCtx, item.principal, item.task)
				} else {
					advanced, err = w.Service.Advance(workerCtx, AdvanceRequest{Principal: item.principal, TaskID: item.task.ID, Expected: item.task.Version})
				}
				if err == nil && advanced.Status == Blocked && advanced.Reason == "bounded_rework_required" {
					_, err = w.Service.rework(workerCtx, item.principal, advanced)
				}
				if err != nil && !errors.Is(err, core.ErrPreconditionFailed) && !errors.Is(err, ErrTaskClosed) && !errors.Is(err, ErrCapacity) {
					logger := w.Logger
					if logger == nil {
						logger = slog.Default()
					}
					logger.Warn("Flow advance stopped", "task_id", item.task.ID, "reason", "advance_unavailable")
					if repository, ok := w.Service.Repository.(parkedRepository); ok {
						finish, done := context.WithTimeout(context.WithoutCancel(workerCtx), 5*time.Second)
						current, readErr := w.Service.Repository.Read(finish, item.principal, item.task.ID)
						if readErr == nil && !current.Status.Terminal() && (current.Status != Blocked || current.Reason == "bounded_rework_required") && current.CloseIntent == CloseNone {
							if parkErr := repository.ParkTask(finish, item.principal, current.ID, current.Version, "advance_unavailable"); parkErr != nil {
								logger.Warn("Flow checkpoint retained", "task_id", current.ID, "reason", "park_unconfirmed")
							}
						}
						done()
					}
				}
				finished <- item.task.RootTaskID
			}
		})
	}
	active := make(map[string]bool, c.MaxActive)
	ticker := time.NewTicker(time.Duration(c.PollIntervalMS) * time.Millisecond)
	defer ticker.Stop()
	var runErr error
	for ctx.Err() == nil {
		if err := w.Service.sweepClosures(ctx, w.Repository); err != nil {
			runErr = workerQueryError(ctx, err)
			break
		}
		for draining := true; draining; {
			select {
			case root := <-finished:
				delete(active, root)
			default:
				draining = false
			}
		}
		if len(active) < c.MaxActive {
			ready, err := w.Repository.ReadyRoots(ctx, w.Service.clock().UnixMilli(), 32)
			if err != nil {
				runErr = workerQueryError(ctx, err)
				break
			}
			for _, task := range ready {
				if active[task.RootTaskID] {
					continue
				}
				principal, err := storedFlowPrincipal(task)
				if err != nil {
					runErr = err
					break
				}
				select {
				case jobs <- workItem{task: task, principal: principal}:
					active[task.RootTaskID] = true
				case <-ctx.Done():
				}
				if len(active) >= c.MaxActive || ctx.Err() != nil {
					break
				}
			}
			if runErr != nil {
				break
			}
		}
		select {
		case root := <-finished:
			delete(active, root)
		case <-ticker.C:
		case <-ctx.Done():
		}
	}
	cancel()
	close(jobs)
	joined := make(chan struct{})
	go func() { group.Wait(); close(joined) }()
	grace := time.NewTimer(time.Duration(c.ShutdownGraceSeconds) * time.Second)
	defer grace.Stop()
	select {
	case <-joined:
		return runErr
	case <-grace.C:
		// The app must keep shared PG alive until Run returns. This failure is not
		// a join; the outer process shutdown deadline remains the hard last resort.
		logger := w.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("Flow shutdown exceeded grace; waiting for real join", "reason", "flow_drain_timeout")
		<-joined
		return errors.Join(runErr, fmt.Errorf("%w: flow drain exceeded %ds", ErrDrainTimeout, c.ShutdownGraceSeconds))
	}
}

// workerQueryError 仅消除已结束父上下文的纯取消错误，复合错误保守保留。
// workerQueryError normalizes only errors made purely of the parent's cancellation;
// joined errors carrying any other failure stay intact for the caller's real join.
func workerQueryError(ctx context.Context, err error) error {
	if CancellationOnly(err, ctx.Err()) {
		return nil
	}
	return err
}

// CancellationOnly 判定 err 是否为"纯取消"：沿单链包装与 errors.Join 树逐叶下钻，
// 仅当至少有一个叶子且每个叶子都等于 cancelled 时才为真。该函数存在的理由：
// `errors.Join(rows.Err(), rows.Close())` 即便只有一个非空子错误也会产生带
// `Unwrap() []error` 的 join 节点，单链 `errors.Unwrap` 到它就停住；若只按单链
// 判断，取消期间一句普通查询的 context.Canceled 会被误报为数据库故障，把正常
// 停机变成 rt.Close() 失败（CI `-race` 的 010-race 即此症状）。携带任何真实
// 失败叶子（例如数据库错误、drain 超时）的 join 一律保留，交给调用方报告。
// CancellationOnly reports whether err consists solely of the cancellation sentinel.
// It descends through both single-chain wrappers and errors.Join trees because a
// join node stops errors.Unwrap even when it contains only one non-nil child; a
// pure cancellation wrapped that way must not be mistaken for a database failure.
// Any join carrying a real failure leaf is retained and reported by the caller.
func CancellationOnly(err, cancelled error) bool {
	if err == nil || cancelled == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		found := false
		for _, child := range children {
			if child == nil {
				continue
			}
			found = true
			if !CancellationOnly(child, cancelled) {
				return false
			}
		}
		return found
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if child := wrapped.Unwrap(); child != nil {
			return CancellationOnly(child, cancelled)
		}
	}
	return err == cancelled
}

func storedFlowPrincipal(task Task) (core.Principal, error) {
	if task.Contract.Scope.CallerID != task.CallerID || task.Contract.Scope.KeyID != task.AuthorizingKeyID || string(task.Contract.Scope.AuthorizingKind) != task.AuthorizingKind {
		return core.Principal{}, core.ErrUnauthorized
	}
	scopes := task.Contract.Scope.Scopes
	if task.AuthorizingKind == string(core.AuthorizingKindAdminBuiltin) {
		// The existing admin principal deliberately has no caller-key scopes.
		// Its explicit Flow ceiling remains in the immutable task contract.
		scopes = nil
	}
	return core.OwnedPrincipal(task.CallerID, core.AuthorizingKind(task.AuthorizingKind), task.AuthorizingKeyID, scopes, core.ActorRecovery)
}

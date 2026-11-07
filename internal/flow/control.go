package flow

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// rootLeaseReleaseTimeout 是请求路径上释放根租约的上限：这里是请求，不是停机，
	// 因此**不**受 `worker.shutdown_grace` 影响（模块卡 §3.1.6）。
	// rootLeaseReleaseTimeout bounds the request-path release, unrelated to shutdown.
	rootLeaseReleaseTimeout = 2 * time.Second
	// defaultRootLeaseTTL 只在装配漏填 `Service.LeaseTTL` 时兜底，取值与
	// `worker.lease_ttl_seconds` 的缺省一致；正常装配总是显式注入。
	// defaultRootLeaseTTL mirrors the worker config default when the field is unset.
	defaultRootLeaseTTL = 30 * time.Second
)

// isolatedRunner 是 R4 新增的**可选**端口：根子树内是否已无仍有效的 run 租约。
// 它与 joinedRunner 是两个判据，任何一方都不得顶替另一方（§2.3.6 / FR-24-38）。
// isolatedRunner answers whether the root subtree holds no live run lease.
type isolatedRunner interface {
	Isolated(context.Context, Task) (bool, error)
}

// isolatedOf 取"已隔离"判据。端口缺失时返回 false —— fail-closed：存储层因此
// **不**收敛 attempt（仍有活执行时收敛会把在跑的工作判成 interrupted），并留诊断。
// A missing port yields false so no attempt is converged (fail-closed).
func (s *Service) isolatedOf(ctx context.Context, task Task) (bool, error) {
	runner, ok := s.Runner.(isolatedRunner)
	if !ok {
		if s.Runner != nil {
			logger := s.Logger
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("Flow 子树判据端口缺失，attempt 不收敛", "task_id", task.ID, "reason", "subtree_gate_missing")
		}
		return false, nil
	}
	return runner.Isolated(ctx, task)
}

// withRootLease 是"零值即自取"的共用助手（模块卡 §3.1.6，不是新公开 API）：
// 调用方自带 token（回收器 A3）就直接用、不释放；否则按根领取一个 flow_root
// 租约，用完在返回前释放。领取失败映射 `ErrFlowRootBusy` → 409 `FLOW_ROOT_BUSY`，
// **不重试、不降级成无租约写入**。
// withRootLease reuses a supplied token or acquires (and releases) a root lease;
// contention maps to ErrFlowRootBusy and never degrades to an unleased write.
func (s *Service) withRootLease(ctx context.Context, rootTaskID string, token core.LeaseToken,
	fn func(core.LeaseToken) error,
) error {
	if token.ResourceID != "" {
		return fn(token)
	}
	if s == nil || s.Leases == nil || s.Identity.IsZero() {
		return ErrFlowRootBusy
	}
	held, err := s.Leases.Acquire(ctx, core.LeaseFlowRoot, rootTaskID, s.Identity, s.leaseTTL())
	if errors.Is(err, core.ErrLeaseHeld) {
		return ErrFlowRootBusy
	}
	if err != nil {
		return err
	}
	defer releaseRootLease(ctx, s.Leases, held.LeaseToken, s.Logger)
	return fn(held.LeaseToken)
}

// releaseRootLease 从**调用方 ctx** 派生（父 ctx 已取消就立即放弃，不硬等），
// 不套 `WithoutCancel`：结论已经提交，失败只记 WARN，租约由 TTL 回收——
// 不得因此改写返回给调用方的状态。
// releaseRootLease derives its budget from the caller's context and never
// rewrites the caller-visible outcome on a failed release.
func releaseRootLease(ctx context.Context, leases core.LeaseStore, token core.LeaseToken, logger *slog.Logger) {
	if leases == nil || token.IsZero() {
		return
	}
	releaseCtx, cancel := context.WithTimeout(ctx, rootLeaseReleaseTimeout)
	defer cancel()
	if err := leases.Release(releaseCtx, token); err != nil {
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("Flow 根租约释放失败，等待 TTL 回收", "task_id", token.ResourceID, "reason", "root_lease_release_failed", "error", err)
	}
}

// leaseTTL 返回装配注入的根租约有效期；未填时退回与 worker 缺省一致的值。
func (s *Service) leaseTTL() time.Duration {
	if s != nil && s.LeaseTTL > 0 {
		return s.LeaseTTL
	}
	return defaultRootLeaseTTL
}

// Cancel first persists the monotone close intent and only then signals local
// work. A timeout returns cancelling/blocked; it never manufactures join proof.
func (s *Service) Cancel(ctx context.Context, req CancelRequest) (Task, error) {
	if s == nil || s.Repository == nil || !req.Principal.Valid() || !req.Principal.HasScope(core.ScopeFlowsWrite) {
		return Task{}, core.ErrUnauthorized
	}
	task, err := s.Repository.Read(ctx, req.Principal, req.TaskID)
	if err != nil {
		return task, err
	}
	s.workMu.Lock()
	work := s.work[task.RootTaskID]
	s.workMu.Unlock()
	joined := work == nil && task.Status == Queued
	if work == nil && !joined {
		if runner, ok := s.Runner.(joinedRunner); ok {
			joined, err = runner.Joined(ctx, task)
			if err != nil {
				return task, err
			}
		}
	}
	isolated, err := s.isolatedOf(ctx, task)
	if err != nil {
		return task, err
	}
	task, err = s.Repository.Cancel(ctx, CancelTask{Request: req, Joined: joined, Isolated: isolated, Now: s.clock().UnixMilli()})
	if err != nil || joined || work == nil {
		return task, err
	}
	work.cancel()
	wait := time.NewTimer(time.Duration(s.Config.CancelWaitSeconds) * time.Second)
	defer wait.Stop()
	select {
	case <-work.done:
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		// 本地 work 已退出，但同一根子树可能仍有远端在跑的 Run：Isolated 必须重取，
		// 不能因为"本地已 join"就断言旧执行已被隔离（§2.3.6）。
		isolated, err := s.isolatedOf(finish, task)
		if err != nil {
			return task, err
		}
		closed, err := s.Repository.Cancel(finish, CancelTask{Request: req, Joined: true, Isolated: isolated, Now: s.clock().UnixMilli()})
		if errors.Is(err, ErrTaskClosed) {
			return s.Repository.Read(finish, req.Principal, req.TaskID)
		}
		return closed, err
	case <-ctx.Done():
		return task, nil
	case <-wait.C:
		return task, nil
	}
}

// Abandon is a separate audited administrative close; it never grants execution
// authority or clears unknown actions/allocations of the business owner.
func (s *Service) Abandon(ctx context.Context, req AbandonRequest) (Task, error) {
	if err := validateManager(req.Principal, req.Actor); err != nil {
		return Task{}, err
	}
	if s == nil || s.Repository == nil || s.Operations == nil || s.Authority == nil || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 4096 || req.OperationID == "" {
		return Task{}, ErrStateConflict
	}
	worker, ok := s.Repository.(WorkerRepository)
	if !ok {
		return Task{}, ErrStateConflict
	}
	task, err := worker.ReadForWorker(ctx, req.TaskID)
	if err != nil {
		return task, err
	}
	hash := flowInputHash(mustJSON(struct {
		TaskID, Reason string
		Expected       TaskVersion
	}{req.TaskID, req.Reason, req.Expected}))
	previous, opErr := s.Operations.GetOperation(ctx, req.OperationID)
	if opErr == nil {
		if previous.Action != "flow.abandon" || previous.TargetID != req.TaskID || previous.RequestHash != hash {
			return Task{}, ErrIdempotencyConflict
		}
		if previous.Status == core.OperationSuccess {
			return task, nil
		}
		if previous.Status != core.OperationPending {
			return Task{}, ErrStateConflict
		}
	} else if !errors.Is(opErr, core.ErrOperationNotFound) {
		return Task{}, opErr
	} else {
		if task.Version != req.Expected {
			return Task{}, core.ErrPreconditionFailed
		}
		if err := s.Operations.BeginOperation(ctx, core.OperationRecord{OperationID: req.OperationID, Action: "flow.abandon", TargetKind: "flow_task", TargetID: task.ID, ActorSubject: req.Actor.Subject, AuthMethod: req.Actor.Method, Source: req.Actor.Source, RequestHash: hash, RequestedVersionID: req.Expected.StateVersion, CreatedAt: s.clock()}); err != nil {
			return Task{}, err
		}
	}
	s.workMu.Lock()
	work := s.work[task.RootTaskID]
	s.workMu.Unlock()
	joined := work == nil && task.Status == Queued
	if work == nil && !joined {
		if runner, ok := s.Runner.(joinedRunner); ok {
			joined, err = runner.Joined(ctx, task)
			if err != nil {
				return task, err
			}
		}
	}
	rescue := false
	if task.CloseIntent == CloseCancel && !joined {
		rescue, err = s.Authority.NoAvailableCallerKey(ctx, task.CallerID)
		if err != nil {
			return task, err
		}
	}
	isolated, err := s.isolatedOf(ctx, task)
	if err != nil {
		return task, err
	}
	result, err := s.Repository.Abandon(ctx, AbandonTask{Request: req, Joined: joined, Isolated: isolated, NoAvailableCallerKey: rescue, Now: s.clock().UnixMilli()})
	if err != nil {
		return result, err
	}
	if work != nil {
		work.cancel()
	}
	return result, nil
}

// Recover rechecks the original authority and joins only using a runtime proof.
// Missing proof/unknown actions are retained as blocked, never replayed.
func (s *Service) Recover(ctx context.Context, req ResumeRequest) (Task, error) {
	if s == nil || s.Repository == nil || s.Authority == nil || !req.Principal.Valid() || !req.Principal.HasScope(core.ScopeFlowsWrite) {
		return Task{}, core.ErrUnauthorized
	}
	task, err := s.Repository.Read(ctx, req.Principal, req.TaskID)
	if err != nil {
		return task, err
	}
	if receipts, ok := s.Repository.(interface {
		FindRecovery(context.Context, ResumeRequest) (Task, bool, error)
	}); ok {
		previous, found, err := receipts.FindRecovery(ctx, req)
		if err != nil {
			return task, err
		}
		if found {
			return previous, nil
		}
	}
	if task.Status.Terminal() {
		return task, ErrTaskClosed
	}
	if task.CloseIntent != CloseNone || task.CancelRequested {
		return task, ErrTaskClosed
	}
	if s.clock().UnixMilli() >= task.Deadline {
		return task, ErrDeadline
	}
	if task.Version != req.Expected {
		return task, core.ErrPreconditionFailed
	}
	joined := false
	s.workMu.Lock()
	_, active := s.work[task.RootTaskID]
	s.workMu.Unlock()
	if !active {
		if runner, ok := s.Runner.(joinedRunner); ok {
			joined, err = runner.Joined(ctx, task)
			if err != nil {
				return task, err
			}
		}
	}
	if req.Principal.KeyID != task.AuthorizingKeyID {
		return task, core.ErrUnauthorized
	}
	current, err := s.Authority.Current(ctx, task)
	if err != nil {
		return task, err
	}
	if current.CallerID != task.CallerID || current.KeyID != task.AuthorizingKeyID || !current.HasScope(core.ScopeFlowsWrite) {
		return task, core.ErrUnauthorized
	}
	worker, ok := s.Repository.(WorkerRepository)
	if !ok {
		return task, ErrStateConflict
	}
	unknown, err := worker.HasUnknownActions(ctx, task.ID)
	if err != nil {
		return task, err
	}
	if unknown {
		return task, ErrActionUnknown
	}
	status := Blocked
	if joined && !unknown && task.Status == Blocked {
		status = Queued
	}
	if status == Queued && s.Outputs == nil && len(task.Revision.Outputs) > 0 {
		return task, ErrStateConflict
	}
	for _, output := range task.Revision.Outputs {
		if output.Verified {
			if err := s.Outputs.CheckOutput(ctx, current, task, output); err != nil {
				return task, err
			}
		}
	}
	isolated, err := s.isolatedOf(ctx, task)
	if err != nil {
		return task, err
	}
	// Reason 必须随 reconciliation 落库：`flow_tasks.reason` 是回收器 A3 对 blocked 根
	// 分路的唯一依据（`worker_lease_expired` 由 A3 自己重入 queued，其余不动，FR-24-37）。
	// 只把 Reason 放进 `Request` 会让头写入把该列清空，blocked 根就再也分不出是哪一类。
	// The reason must land on the row: it is the reclaimer's only routing key.
	reconcile := ReconcileTask{Request: req, Status: status, Reason: req.Reason, Joined: joined, Isolated: isolated, Now: s.clock().UnixMilli()}
	if status != Queued {
		// Blocked 是停住，不要求根租约（§3.1.6 写入分档）：管理面在没有任何执行者时
		// 也要能写 Blocked；只有 Queued 才是"向前走"的推进类结论。
		return s.Repository.Recover(ctx, reconcile)
	}
	var written Task
	attempted := false
	if err := s.withRootLease(ctx, task.RootTaskID, req.Lease, func(token core.LeaseToken) error {
		attempted = true
		reconcile.Request.Lease = token
		var writeErr error
		written, writeErr = s.Repository.Recover(ctx, reconcile)
		return writeErr
	}); err != nil {
		if attempted {
			return written, err
		}
		// 领取根租约失败（含 ErrFlowRootBusy）：尚未写入，如实返回当前任务与错误。
		return task, err
	}
	return written, nil
}

func (s *Service) sweepClosures(ctx context.Context, repository WorkerRepository) error {
	tasks, err := repository.OpenRoots(ctx, 32)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.CloseIntent == CloseNone && !task.CancelRequested && s.clock().UnixMilli() < task.Deadline {
			continue
		}
		p, err := StoredPrincipal(task)
		if err != nil {
			return err
		}
		s.workMu.Lock()
		work := s.work[task.RootTaskID]
		s.workMu.Unlock()
		if work != nil {
			work.cancel()
			continue
		}
		joined := task.Status == Queued
		if !joined {
			if runner, ok := s.Runner.(joinedRunner); ok {
				joined, err = runner.Joined(ctx, task)
				if err != nil {
					return err
				}
			}
		}
		request := ResumeRequest{Principal: p, TaskID: task.ID, Expected: task.Version, OperationID: "sweep:" + task.ID + ":" + task.Version.ETag(task.ID), Reason: "persistent close or wall deadline"}
		isolated, err := s.isolatedOf(ctx, task)
		if err != nil {
			return err
		}
		_, err = s.Repository.Recover(ctx, ReconcileTask{Request: request, Status: Blocked, Joined: joined, Isolated: isolated, Now: s.clock().UnixMilli()})
		if err != nil && !errors.Is(err, core.ErrPreconditionFailed) && !errors.Is(err, ErrTaskClosed) {
			return err
		}
	}
	return nil
}

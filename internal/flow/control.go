package flow

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

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
	task, err = s.Repository.Cancel(ctx, CancelTask{Request: req, Joined: joined, Now: s.clock().UnixMilli()})
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
		closed, err := s.Repository.Cancel(finish, CancelTask{Request: req, Joined: true, Now: s.clock().UnixMilli()})
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
	result, err := s.Repository.Abandon(ctx, AbandonTask{Request: req, Joined: joined, NoAvailableCallerKey: rescue, Now: s.clock().UnixMilli()})
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
	return s.Repository.Recover(ctx, ReconcileTask{Request: req, Status: status, Joined: joined, Now: s.clock().UnixMilli()})
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
		p, err := storedFlowPrincipal(task)
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
		_, err = s.Repository.Recover(ctx, ReconcileTask{Request: request, Status: Blocked, Joined: joined, Now: s.clock().UnixMilli()})
		if err != nil && !errors.Is(err, core.ErrPreconditionFailed) && !errors.Is(err, ErrTaskClosed) {
			return err
		}
	}
	return nil
}

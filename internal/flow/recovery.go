package flow

import (
	"context"
	"errors"

	"github.com/ymedword/basalt/internal/core"
)

type completedAttemptRepository interface {
	CompletedAttempt(context.Context, core.Principal, Task, string, string) (NodeAttempt, error)
	ReclaimCompleted(context.Context, core.Principal, string, string, TaskVersion) (Task, error)
}
type completedNodeObserver interface {
	ObserveCompleted(context.Context, NodeRunRequest) (NodeRunResult, error)
}

// reuseCompleted checks original audit/messages and current source authority.
// It starts no Run/session and acquires no second caller quota reservation.
func (s *Service) reuseCompleted(ctx context.Context, p core.Principal, task Task, node Node, inputHash string) (Task, bool, error) {
	reader, ok := s.Repository.(completedAttemptRepository)
	if !ok {
		return task, false, nil
	}
	observer, ok := s.Runner.(completedNodeObserver)
	if !ok {
		return task, false, nil
	}
	attempt, err := reader.CompletedAttempt(ctx, p, task, node.ID, inputHash)
	if errors.Is(err, ErrNotFound) {
		return task, false, nil
	}
	if err != nil {
		return task, true, err
	}
	joinedPort, ok := s.Runner.(joinedRunner)
	if !ok {
		return task, true, ErrStateConflict
	}
	joined, err := joinedPort.Joined(ctx, task)
	if err != nil {
		return task, true, err
	}
	if !joined {
		return task, true, ErrCapacity
	}
	result, err := observer.ObserveCompleted(ctx, NodeRunRequest{Principal: p, Task: task, Node: node, Attempt: attempt})
	if err != nil {
		return task, true, err
	}
	if result.Unknown || result.Run.Status != core.StatusCompleted {
		return task, true, ErrActionUnknown
	}
	claimed, err := reader.ReclaimCompleted(ctx, p, task.ID, attempt.ID, task.Version)
	if err != nil {
		return task, true, err
	}
	result.Reused = true
	accepted, err := s.acceptAttempt(ctx, p, claimed, node, attempt, result, nil)
	return accepted, true, err
}

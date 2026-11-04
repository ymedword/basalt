package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

type activeAttemptReader interface {
	ActiveAttempt(context.Context, core.Principal, string) (NodeAttempt, error)
}

// advanceSubflow uses a durable static child, not a root Run. Each child leaf
// gets its own session and the same original owner; recursive depth is bounded by
// the accepted contract, and a capacity refusal yields the whole root interval.
func (s *Service) advanceSubflow(ctx context.Context, p core.Principal, task Task, node Node, input json.RawMessage, hash string) (Task, error) {
	if node.Subflow == nil || task.Depth >= task.Contract.Budget.MaxDepth {
		return task, ErrBudget
	}
	reader, ok := s.Repository.(activeAttemptReader)
	if !ok {
		return task, ErrStateConflict
	}
	attempt, err := reader.ActiveAttempt(ctx, p, task.ID)
	if errors.Is(err, ErrNotFound) {
		id, idErr := newID("fn_")
		if idErr != nil {
			return task, idErr
		}
		attempt = NodeAttempt{ID: id, TaskID: task.ID, NodeID: node.ID, NodeInvocationID: task.ID + ":" + node.ID, Attempt: task.Revision.Attempts[node.ID] + 1, TaskRevision: task.Version.Revision, ExecutionEpoch: task.Version.ExecutionEpoch, Input: input, InputHash: hash}
		attempt, err = s.Repository.Claim(ctx, ClaimNode{Principal: p, TaskID: task.ID, Expected: task.Version, Attempt: attempt, Now: s.clock().UnixMilli()})
	}
	if err != nil {
		return task, err
	}
	if attempt.NodeID != node.ID || attempt.TaskRevision != task.Version.Revision || attempt.ExecutionEpoch != task.Version.ExecutionEpoch || attempt.InputHash != hash || attempt.RunID != "" {
		return task, ErrStateConflict
	}
	task, err = s.Repository.Read(ctx, p, task.ID)
	if err != nil {
		return task, err
	}
	key := fmt.Sprintf("child:%s:%s:%d", task.ID, node.ID, task.Version.Revision)
	child, err := s.Repository.FindAccepted(ctx, p, key)
	if errors.Is(err, ErrNotFound) {
		child, err = s.createChild(ctx, p, task, node, key, input, hash)
	}
	if errors.Is(err, ErrCapacity) {
		return s.deferParent(ctx, task, s.clock().Add(time.Duration(s.Config.PollIntervalMS)*time.Millisecond).UnixMilli())
	}
	if err != nil {
		return task, err
	}
	if child.ParentTaskID != task.ID || child.RootTaskID != task.RootTaskID || child.DefinitionUID != node.Subflow.UID || child.DefinitionVersion != node.Subflow.Version || child.Revision.InputHash != hash {
		return task, ErrStateConflict
	}
	for !child.Status.Terminal() && (child.Status == Queued || child.Status == Running) {
		if child.NextEligibleAt > s.clock().UnixMilli() {
			return s.deferParent(ctx, task, child.NextEligibleAt)
		}
		current, authErr := s.Authority.Current(ctx, child)
		if authErr != nil {
			return task, authErr
		}
		if current.CallerID != p.CallerID || current.KeyID != p.KeyID || !current.HasScope(core.ScopeFlowsWrite) {
			return task, core.ErrUnauthorized
		}
		leafCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(child.Deadline))
		child, err = s.advanceOne(leafCtx, current, child)
		cancel()
		if err != nil {
			return task, err
		}
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	task, err = s.Repository.Read(finish, p, task.ID)
	if err != nil {
		return task, err
	}
	if task.CloseIntent != CloseNone || task.CancelRequested || s.clock().UnixMilli() >= task.Deadline {
		return s.closeJoined(finish, p, task)
	}
	result := NodeRunResult{Run: core.RunResult{Status: core.StatusFailed}, Unknown: child.Status == Blocked}
	if child.Status == Succeeded {
		query, ok := s.Repository.(TaskQuery)
		if !ok {
			return task, ErrStateConflict
		}
		graph, err := query.ReadGraph(finish, p, child.ID)
		if err != nil {
			return task, err
		}
		values := make(map[string]json.RawMessage)
		for _, spec := range child.Contract.ArtifactSpec {
			for _, artifact := range graph.Artifacts {
				if artifact.ID == spec.ID && artifact.State == "accepted" {
					value := json.RawMessage(artifact.Content)
					if artifact.Format == "text" {
						value = mustJSON(string(artifact.Content))
					}
					values[spec.ID] = value
				}
			}
		}
		if len(child.Contract.ArtifactSpec) == 1 {
			result.Output = values[child.Contract.ArtifactSpec[0].ID]
		} else {
			result.Output = mustJSON(values)
		}
		for _, ref := range graph.Evidence {
			ref.TaskID = ""
			result.Evidence = append(result.Evidence, ref)
		}
		result.Run.Status = core.StatusCompleted
	} else if child.Status == Stopped {
		result.Run.Status = core.StatusLimitReached
	}
	return s.acceptAttempt(finish, p, task, node, attempt, result, nil)
}

func (s *Service) createChild(ctx context.Context, p core.Principal, parent Task, node Node, key string, input json.RawMessage, hash string) (Task, error) {
	definition, err := s.Definitions.ReadDefinition(ctx, *node.Subflow)
	if err != nil {
		return Task{}, err
	}
	if err := s.Authority.CheckStart(ctx, p, definition); err != nil {
		return Task{}, err
	}
	freezer, ok := s.Authority.(AuthorityFreezer)
	if !ok {
		return Task{}, core.ErrUnauthorized
	}
	scope, err := freezer.Freeze(ctx, p, definition)
	if err != nil {
		return Task{}, err
	}
	if scope.CallerID != parent.CallerID || scope.KeyID != parent.AuthorizingKeyID || string(scope.AuthorizingKind) != parent.AuthorizingKind {
		return Task{}, core.ErrUnauthorized
	}
	intersect := func(child, ceiling []string) []string {
		result := make([]string, 0, len(child))
		for _, item := range child {
			if slices.Contains(ceiling, item) {
				result = append(result, item)
			}
		}
		return result
	}
	scope.Scopes = intersect(scope.Scopes, parent.Contract.Scope.Scopes)
	scope.AgentUIDs = intersect(scope.AgentUIDs, parent.Contract.Scope.AgentUIDs)
	scope.Tools = intersect(scope.Tools, parent.Contract.Scope.Tools)
	scope.CollectionIDs = intersect(scope.CollectionIDs, parent.Contract.Scope.CollectionIDs)
	if !slices.Contains(scope.Scopes, core.ScopeFlowsWrite) {
		return Task{}, core.ErrUnauthorized
	}
	for _, node := range definition.Definition.Nodes {
		if node.Agent != nil && !slices.Contains(scope.AgentUIDs, node.Agent.UID) {
			return Task{}, core.ErrUnauthorized
		}
	}
	contract := definition.Definition.Contract
	contract.Scope = scope
	contract.Budget = minimumBudget(contract.Budget, parent.Contract.Budget)
	validated, inputHash, err := s.validateInput(ctx, definition, input)
	if err != nil || inputHash != hash {
		return Task{}, ErrDefinitionInvalid
	}
	id, err := newID("ft_")
	if err != nil {
		return Task{}, err
	}
	requestHash, err := startHash(definition.DefinitionUID, definition.Version, contract.ContractVersion, validated, contract.Budget)
	if err != nil {
		return Task{}, err
	}
	now := s.clock().UnixMilli()
	return s.Repository.Create(ctx, CreateTask{Request: StartRequest{Principal: p, DefinitionUID: definition.DefinitionUID, DefinitionVersion: definition.Version, IdempotencyKey: key, Input: validated}, TaskID: id, RootTaskID: parent.RootTaskID, ParentTaskID: parent.ID, Depth: parent.Depth + 1, RequestHash: requestHash, InputHash: hash, Input: validated, Definition: definition, Contract: contract, Config: s.Config, CreatedAt: now, Deadline: min(parent.Deadline, now+contract.Budget.MaxDurationSeconds*1000)})
}

func (s *Service) deferParent(ctx context.Context, parent Task, eligible int64) (Task, error) {
	repository, ok := s.Repository.(WorkerRepository)
	if !ok {
		return parent, ErrStateConflict
	}
	current, err := repository.ReadForWorker(ctx, parent.ID)
	if err != nil {
		return parent, err
	}
	if err := repository.Defer(ctx, current.ID, current.Version, eligible); err != nil {
		return current, err
	}
	return repository.ReadForWorker(ctx, current.ID)
}

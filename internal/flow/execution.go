package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

type (
	localWork struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	remainingReader interface {
		Remaining(context.Context, core.Principal, string) (core.UsageEstimate, error)
	}
	unadmittedYielder interface {
		YieldUnadmitted(context.Context, core.Principal, string, string, TaskVersion, int64) error
	}
	joinedRunner interface {
		Joined(context.Context, Task) (bool, error)
	}
)

// Advance owns one sequential root interval. It neither starts a background
// goroutine nor holds an SQL transaction while the leaf performs external I/O.
func (s *Service) Advance(ctx context.Context, req AdvanceRequest) (Task, error) {
	if s == nil || s.Repository == nil || s.Authority == nil || s.Runner == nil || s.Schemas == nil || !s.Config.Enabled {
		return Task{}, ErrStateConflict
	}
	if !req.Principal.Valid() || !req.Principal.HasScope(core.ScopeFlowsWrite) {
		return Task{}, core.ErrUnauthorized
	}
	task, err := s.Repository.Read(ctx, req.Principal, req.TaskID)
	if err != nil {
		return Task{}, err
	}
	if task.Version != req.Expected {
		return task, core.ErrPreconditionFailed
	}
	if task.Status.Terminal() {
		return task, ErrTaskClosed
	}
	if task.CancelRequested || task.CloseIntent != CloseNone || s.clock().UnixMilli() >= task.Deadline {
		return task, ErrTaskClosed
	}
	if task.Status != Queued && task.Status != Running {
		return task, ErrStateConflict
	}
	current, err := s.Authority.Current(ctx, task)
	if err != nil {
		return task, err
	}
	if current.CallerID != task.CallerID || current.KeyID != task.AuthorizingKeyID || !current.HasScope(core.ScopeFlowsWrite) {
		return task, core.ErrUnauthorized
	}
	runCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(task.Deadline))
	entry := &localWork{cancel: cancel, done: make(chan struct{})}
	s.workMu.Lock()
	if s.work == nil {
		s.work = make(map[string]*localWork)
	}
	if _, active := s.work[task.RootTaskID]; active {
		s.workMu.Unlock()
		cancel()
		return task, ErrCapacity
	}
	s.work[task.RootTaskID] = entry
	s.workMu.Unlock()
	defer func() {
		cancel()
		s.workMu.Lock()
		delete(s.work, task.RootTaskID)
		close(entry.done)
		s.workMu.Unlock()
	}()
	return s.advanceOne(runCtx, current, task, req.Lease)
}

// advanceOne 在已取得的根租约凭证下推进一个节点：节点领取 / 提交都是推进类写入，
// 存储层按目标行的 root_task_id 校验根租约存在且未过期（P-04）。凭证原样下传，
// 叶子的 Run 创建（createRunForNode）也用它证明父根此刻有人合法持有。
// advanceOne advances one node under the caller's root credential; every
// advance-class write is fenced by that lease in the storage layer.
func (s *Service) advanceOne(ctx context.Context, p core.Principal, task Task, lease core.LeaseToken) (Task, error) {
	var node Node
	found := false
	for _, candidate := range task.AcceptedVersion.Definition.Nodes {
		if candidate.ID == task.Revision.NodeID {
			node = candidate
			found = true
			break
		}
	}
	if !found {
		return task, ErrCheckpointUnsupported
	}
	reader, ok := s.Repository.(remainingReader)
	if !ok {
		return task, ErrBudget
	}
	remaining, err := reader.Remaining(ctx, p, task.ID)
	if err != nil {
		return task, err
	}
	state := TrustedState{Task: task, Remaining: remaining}
	input, hash, err := ResolveNodeInput(ctx, p, state, node, s.Outputs)
	if err != nil {
		return task, err
	}
	// Immutable caller-supplied source references are still checked on every use.
	for _, ref := range task.Contract.InputRefs {
		if ref.TaskID != "" && ref.TaskID != task.ID {
			return task, ErrStateConflict
		}
		ref.TaskID = task.ID
		if s.Verification == nil || s.Verification.Sources == nil {
			return task, ErrStateConflict
		}
		verdict, err := s.Verification.Sources.Check(ctx, p, ref)
		if err != nil {
			return task, err
		}
		if !verdict.Valid || verdict.Version != ref.SourceVersion || verdict.ContentHash != ref.ContentHash {
			return task, ErrStateConflict
		}
	}
	if node.Kind == "subflow" {
		return s.advanceSubflow(ctx, p, task, node, input, hash, lease)
	}
	if node.Kind == "agent" || node.Kind == "tool" {
		if reused, found, err := s.reuseCompleted(ctx, p, task, node, hash, lease); found {
			return reused, err
		}
	}
	id, err := newID("fn_")
	if err != nil {
		return task, err
	}
	runID, sessionID := "", ""
	if node.Kind != "branch" {
		runID, err = newID("flow-")
		if err != nil {
			return task, err
		}
		sessionID, err = newID("flow-session-")
		if err != nil {
			return task, err
		}
	}
	attempt := NodeAttempt{ID: id, TaskID: task.ID, NodeID: node.ID, NodeInvocationID: task.ID + ":" + node.ID, Attempt: task.Revision.Attempts[node.ID] + 1, TaskRevision: task.Version.Revision, ExecutionEpoch: task.Version.ExecutionEpoch, RunID: runID, SessionID: sessionID, Input: input, InputHash: hash}
	attempt, err = s.Repository.Claim(ctx, ClaimNode{Principal: p, TaskID: task.ID, Expected: task.Version, Attempt: attempt, Lease: lease, Now: s.clock().UnixMilli()})
	if err != nil {
		return task, err
	}
	task, err = s.Repository.Read(ctx, p, task.ID)
	if err != nil {
		return task, err
	}
	var result NodeRunResult
	if node.Kind == "branch" {
		next, selectErr := SelectBranch(ctx, p, TrustedState{Task: task, Remaining: remaining}, *node.Branch, s.Outputs)
		err = selectErr
		node.Next = next
		result = NodeRunResult{Run: core.RunResult{Status: core.StatusCompleted}, Output: json.RawMessage(`{}`)}
	} else {
		if node.Agent == nil {
			return task, ErrDefinitionInvalid
		}
		f := &core.FlowRunContext{TaskID: task.ID, RootTaskID: task.RootTaskID, ParentTaskID: task.ParentTaskID, NodeInvocationID: attempt.NodeInvocationID, NodeAttemptID: attempt.ID, TaskRevision: task.Version.Revision, Lease: lease, AgentUID: node.Agent.UID, AgentConfigHash: node.Agent.ConfigHash, ResourceManifestHash: node.Agent.ManifestHash, Authority: task.Contract.Scope}
		result, err = s.Runner.Run(ctx, NodeRunRequest{Principal: p, Task: task, Node: node, Attempt: attempt, Flow: f})
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	latest, readErr := s.Repository.Read(finishCtx, p, task.ID)
	if readErr != nil {
		return task, errors.Join(err, readErr)
	}
	if latest.CloseIntent != CloseNone || latest.CancelRequested || s.clock().UnixMilli() >= latest.Deadline {
		return s.closeJoined(finishCtx, p, latest)
	}
	if errors.Is(err, ErrCapacity) {
		if yielder, ok := s.Repository.(unadmittedYielder); ok {
			yieldErr := yielder.YieldUnadmitted(finishCtx, p, task.ID, attempt.ID, latest.Version, s.clock().Add(time.Duration(s.Config.PollIntervalMS)*time.Millisecond).UnixMilli())
			if yieldErr != nil {
				return latest, errors.Join(err, yieldErr)
			}
			return s.Repository.Read(finishCtx, p, task.ID)
		}
	}
	return s.acceptAttempt(finishCtx, p, latest, node, attempt, result, err, lease)
}

func (s *Service) closeJoined(ctx context.Context, p core.Principal, task Task) (Task, error) {
	// 关闭类是停住，不需要根租约；但 attempt 收敛必须携带 Isolated——本地执行已退出
	// 不等于整棵子树的旧执行已被隔离（§2.3.6 第 4 个必须填写的调用点）。
	isolated, err := s.isolatedOf(ctx, task)
	if err != nil {
		return task, err
	}
	req := ResumeRequest{Principal: p, TaskID: task.ID, Expected: task.Version, OperationID: fmt.Sprintf("joined:%s:%d", task.ID, task.Version.StateVersion), Reason: "local execution joined"}
	return s.Repository.Recover(ctx, ReconcileTask{Request: req, Status: Blocked, Joined: true, Isolated: isolated, Now: s.clock().UnixMilli()})
}

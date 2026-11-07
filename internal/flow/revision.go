package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// revisionProposal is a closed parameter/restart protocol, never a graph patch.
// Author contract, scopes, versions, deadlines and budgets remain immutable.
type revisionProposal struct {
	Reason string          `json:"reason"`
	NodeID string          `json:"node_id"`
	Input  json.RawMessage `json:"input,omitempty"`
}

type graphReader interface {
	ReadGraph(context.Context, core.Principal, string) (EvidenceGraph, error)
}
type revisionReceiptReader interface {
	FindRevision(context.Context, RevisionRequest) (Task, bool, error)
}

func (s *Service) Revise(ctx context.Context, req RevisionRequest) (Task, error) {
	if s == nil || s.Repository == nil || s.Authority == nil || s.Schemas == nil || !req.Principal.Valid() || !req.Principal.HasScope(core.ScopeFlowsWrite) {
		return Task{}, core.ErrUnauthorized
	}
	task, err := s.Repository.Read(ctx, req.Principal, req.TaskID)
	if err != nil {
		return task, err
	}
	raw, err := CanonicalJSON(ctx, req.Proposal)
	if err != nil {
		return task, err
	}
	req.Proposal = raw
	if receipts, ok := s.Repository.(revisionReceiptReader); ok {
		previous, found, err := receipts.FindRevision(ctx, req)
		if err != nil {
			return task, err
		}
		if found {
			return previous, nil
		}
	}
	if req.OperationID == "" || task.Version != req.Expected {
		return task, core.ErrPreconditionFailed
	}
	if task.Status != Blocked || task.CloseIntent != CloseNone || task.CancelRequested || s.clock().UnixMilli() >= task.Deadline {
		return task, ErrStateConflict
	}
	if req.Principal.KeyID != task.AuthorizingKeyID {
		return task, core.ErrUnauthorized
	}
	p, err := s.Authority.Current(ctx, task)
	if err != nil {
		return task, err
	}
	if p.CallerID != task.CallerID || p.KeyID != task.AuthorizingKeyID || !p.HasScope(core.ScopeFlowsWrite) {
		return task, core.ErrUnauthorized
	}
	s.workMu.Lock()
	active := s.work[task.RootTaskID] != nil
	s.workMu.Unlock()
	if active {
		return task, ErrCapacity
	}
	join, ok := s.Runner.(joinedRunner)
	if !ok {
		return task, ErrStateConflict
	}
	joined, err := join.Joined(ctx, task)
	if err != nil {
		return task, err
	}
	if !joined {
		return task, ErrCapacity
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
	var proposal revisionProposal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		return task, ErrDefinitionInvalid
	}
	if strings.TrimSpace(proposal.Reason) == "" || len(proposal.Reason) > 4096 || !validID(proposal.NodeID) {
		return task, ErrDefinitionInvalid
	}
	known := false
	for _, node := range task.AcceptedVersion.Definition.Nodes {
		known = known || node.ID == proposal.NodeID
	}
	if !known {
		return task, ErrDefinitionInvalid
	}
	if task.Version.Revision >= int64(task.Contract.Budget.MaxRevisions) {
		return task, ErrBudget
	}
	var next TaskRevision
	if err := json.Unmarshal(mustJSON(task.Revision), &next); err != nil {
		return task, err
	}
	next.Revision++
	next.NodeID = proposal.NodeID
	if next.Reworks == nil {
		next.Reworks = make(map[string]int)
	}
	next.Reworks[task.Revision.NodeID]++
	if len(proposal.Input) > 0 {
		next.Input, err = CanonicalJSON(ctx, proposal.Input)
		if err != nil {
			return task, err
		}
		hash, err := s.Schemas.Compile(ctx, task.Contract.InputSchema)
		if err != nil {
			return task, err
		}
		if err := s.Schemas.Validate(ctx, hash, next.Input); err != nil {
			return task, err
		}
		next.InputHash = flowInputHash(next.Input)
	}
	reader, ok := s.Repository.(graphReader)
	if !ok {
		return task, ErrStateConflict
	}
	graph, err := reader.ReadGraph(ctx, p, task.ID)
	if err != nil {
		return task, err
	}
	var invalid []string
	for _, ref := range graph.Evidence {
		if s.Verification == nil || s.Verification.Sources == nil {
			return task, ErrStateConflict
		}
		current, err := s.Verification.Sources.Check(ctx, p, ref)
		if err != nil {
			return task, err
		}
		if !current.Valid || current.Version != ref.SourceVersion || current.ContentHash != ref.ContentHash {
			invalid = append(invalid, ref.ID)
		}
	}
	claims, artifacts, err := InvalidationClosure(graph, invalid)
	if err != nil {
		return task, err
	}
	if next.InputHash != task.Revision.InputHash {
		for _, claim := range graph.Claims {
			ref := ClaimRef{ID: claim.ID, Revision: claim.Revision}
			if !slices.Contains(claims, ref) {
				claims = append(claims, ref)
			}
		}
		for _, artifact := range graph.Artifacts {
			ref := ArtifactRef{ID: artifact.ID, Version: artifact.Version}
			if !slices.Contains(artifacts, ref) {
				artifacts = append(artifacts, ref)
			}
		}
	}
	for i := range graph.Claims {
		if slices.Contains(claims, ClaimRef{ID: graph.Claims[i].ID, Revision: graph.Claims[i].Revision}) {
			graph.Claims[i].State = "stale"
		}
	}
	for i := range graph.Artifacts {
		if slices.Contains(artifacts, ArtifactRef{ID: graph.Artifacts[i].ID, Version: graph.Artifacts[i].Version}) {
			graph.Artifacts[i].State = "stale"
		}
	}
	// Restarted/downstream outputs are not reusable. Every retained predecessor
	// must still have its original completed Run, verification and current sources.
	discard := descendants(task.AcceptedVersion.Definition, proposal.NodeID)
	for id, output := range next.Outputs {
		if discard[id] || next.InputHash != task.Revision.InputHash {
			delete(next.Outputs, id)
			continue
		}
		if s.Outputs == nil {
			return task, ErrStateConflict
		}
		if err := s.Outputs.CheckOutput(ctx, p, task, output); err != nil {
			delete(next.Outputs, id)
		}
	}
	// Slot proofs bind the old task revision and must be evaluated again.
	next.Slots = make(map[string]Slot)
	projected := task
	projected.Version.Revision = next.Revision
	projected.Revision = next
	next.Summary, err = BuildSummary(projected, graph)
	if err != nil {
		return task, err
	}
	// Revise 写 `Queued`，属推进类：必须先在根上持有 flow_root 租约（§2.3.6 写入分档
	// 第 14 行 / §3.1.6）。RevisionRequest 没有租约来源，因此走"零值即自取"；
	// 撞上执行者持有的根租约 → ErrFlowRootBusy → 409 FLOW_ROOT_BUSY，不重试、不降级。
	accept := AcceptRevision{Request: req, Revision: next, InvalidClaims: claims, InvalidArtifacts: artifacts, Now: s.clock().UnixMilli()}
	var written Task
	attempted := false
	if err := s.withRootLease(ctx, task.RootTaskID, core.LeaseToken{}, func(core.LeaseToken) error {
		attempted = true
		var writeErr error
		written, writeErr = s.Repository.Revise(ctx, accept)
		return writeErr
	}); err != nil {
		if attempted {
			return written, err
		}
		return task, err
	}
	return written, nil
}

// rework follows only the published bounded author route after Advance joins.
// It is not a model-supplied plan or an additional public API.
func (s *Service) rework(ctx context.Context, p core.Principal, task Task) (Task, error) {
	if task.Status != Blocked || task.Reason != "bounded_rework_required" {
		return task, ErrStateConflict
	}
	for _, node := range task.AcceptedVersion.Definition.Nodes {
		if node.ID != task.Revision.NodeID {
			continue
		}
		if node.Failure.ReworkTarget == "" || task.Revision.Reworks[node.ID] >= node.Failure.MaxReworks {
			return task, ErrBudget
		}
		proposal := revisionProposal{Reason: "published bounded rework", NodeID: node.Failure.ReworkTarget}
		return s.Revise(ctx, RevisionRequest{Principal: p, TaskID: task.ID, Expected: task.Version, OperationID: fmt.Sprintf("rework:%s:%d:%d", task.ID, task.Version.Revision, task.Version.StateVersion), Proposal: mustJSON(proposal)})
	}
	return task, ErrDefinitionInvalid
}

func descendants(def Definition, start string) map[string]bool {
	visited := map[string]bool{start: true}
	todo := []string{start}
	for len(todo) > 0 {
		id := todo[0]
		todo = todo[1:]
		for _, node := range def.Nodes {
			if node.ID == id {
				children := []string{node.Next}
				if node.Branch != nil {
					children = append(children, node.Branch.Then, node.Branch.Else)
				}
				for _, child := range children {
					if child != "" && !visited[child] {
						visited[child] = true
						todo = append(todo, child)
					}
				}
			}
		}
	}
	return visited
}

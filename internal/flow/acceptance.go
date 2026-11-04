package flow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// draftVerification buffers facts checked outside SQL. The repository accepts
// these exact target versions together with output/relations in one short CAS.
type draftVerification struct {
	basis   VerificationBasis
	records []Verification
}

func (d *draftVerification) LoadVerificationBasis(context.Context, VerifyRequest) (VerificationBasis, error) {
	return d.basis, nil
}

func (d *draftVerification) RecordVerification(_ context.Context, _ VerifyRequest, v Verification) error {
	d.records = append(d.records, v)
	return nil
}

func (s *Service) acceptAttempt(ctx context.Context, p core.Principal, task Task, node Node, attempt NodeAttempt, result NodeRunResult, runErr error) (Task, error) {
	// The model/tool callback may have revoked the original key while the Run
	// completed. Its output is not a grant to accept a durable task result.
	current, err := s.Authority.Current(ctx, task)
	if err != nil {
		return task, err
	}
	if current.CallerID != p.CallerID || current.KeyID != p.KeyID || !current.HasScope(core.ScopeFlowsWrite) {
		return task, core.ErrUnauthorized
	}
	p = current
	query, ok := s.Repository.(TaskQuery)
	if !ok {
		return task, ErrStateConflict
	}
	graph, err := query.ReadGraph(ctx, p, task.ID)
	if err != nil {
		return task, err
	}
	raw, err := CanonicalJSON(ctx, result.Output)
	if err != nil {
		raw = json.RawMessage(`{}`)
	}
	revision := task.Revision
	status, reason, attemptState := Running, "", "succeeded"
	valid := err == nil && runErr == nil && result.Run.Status == core.StatusCompleted && !result.Unknown
	if valid {
		hash, compileErr := s.Schemas.Compile(ctx, node.OutputSchema)
		if compileErr != nil {
			return task, compileErr
		}
		if validateErr := s.Schemas.Validate(ctx, hash, raw); validateErr != nil {
			valid = false
			reason = "node_output_invalid"
		}
	}
	if result.Unknown {
		status, reason, attemptState = Blocked, "action_outcome_unknown", "unknown"
	} else if result.Run.Status == core.StatusLimitReached {
		status, reason, attemptState = Stopped, "run_limit_reached", "failed"
	} else if runErr != nil || result.Run.Status == core.StatusCancelled {
		status, reason, attemptState = Blocked, "leaf_execution_unconfirmed", "failed"
	} else if result.Run.Status == core.StatusFailed {
		status, reason, attemptState = Failed, "leaf_execution_failed", "failed"
	} else if !valid {
		status, reason, attemptState = Failed, "node_output_invalid", "failed"
	}
	if !valid && status != Blocked && status != Stopped && result.Retryable && node.Failure.ReworkTarget == "" && revision.Attempts[node.ID] < min(node.Failure.MaxAttempts, task.Contract.Budget.MaxAttemptsPerNode) {
		status, reason = Running, "bounded_readonly_retry"
	}
	if valid {
		revision.Outputs[node.ID] = NodeOutput{Value: raw, Hash: flowInputHash(raw), NodeAttemptID: attempt.ID, Revision: task.Version.Revision, Verified: true}
		output := revision.Outputs[node.ID]
		for _, mapping := range node.Inputs {
			parts := strings.Split(mapping.Source, ".")
			if len(parts) > 2 && parts[0] == "outputs" {
				upstream := revision.Outputs[parts[1]]
				for _, ref := range upstream.Claims {
					if !slices.Contains(output.Claims, ref) {
						output.Claims = append(output.Claims, ref)
					}
				}
			}
		}
		revision.Outputs[node.ID] = output
		revision.NodeID = node.Next
	}
	acceptanceID := fmt.Sprintf("accept:%s:%d:%d:%d", attempt.ID, task.Version.Revision, task.Version.ExecutionEpoch, task.Version.StateVersion)
	request := CommitNode{Recovered: result.Reused, Principal: p, TaskID: task.ID, AttemptID: attempt.ID, OperationID: acceptanceID, Expected: task.Version, Revision: revision, Outcome: status, Reason: reason, AttemptState: attemptState, Output: raw, OutputHash: flowInputHash(raw), Now: s.clock().UnixMilli()}
	if valid {
		if err := s.collectEvidence(task, attempt, result, &graph, &request); err != nil {
			return task, err
		}
		if err := s.checkClaims(ctx, p, task, node, attempt, raw, &graph, &request); err != nil {
			return task, err
		}
		if err := s.checkSlots(ctx, p, task, attempt, raw, graph, &request); err != nil {
			return task, err
		}
		if node.Next == "" {
			request.Outcome = Succeeded
			if !request.Revision.Outputs[node.ID].Verified {
				request.Outcome, request.Reason = Blocked, "output_claim_verification_missing"
			}
			hash, err := s.Schemas.Compile(ctx, task.Contract.OutputSchema)
			if err != nil {
				return task, err
			}
			if err := s.Schemas.Validate(ctx, hash, raw); err != nil {
				request.Outcome, request.Reason = Failed, "task_output_invalid"
			} else if err := s.checkArtifacts(ctx, p, task, attempt, raw, &graph, &request); err != nil {
				return task, err
			}
			for _, slot := range task.Contract.RequiredSlots {
				if request.Revision.Slots[slot.ID].State != "verified" {
					request.Outcome, request.Reason = Blocked, "required_verification_missing"
				}
			}
			for _, rule := range task.Contract.Acceptance {
				if rule.Required && rule.TargetKind == "claim" {
					claim, ok := CurrentClaim(request.Revision, graph, rule.TargetID)
					if !ok || !s.claimSourcesCurrent(ctx, p, request.Revision, graph, claim) {
						request.Outcome, request.Reason = Blocked, "required_claim_verification_missing"
					}
				}
			}
		}
	}
	if valid && request.Outcome == Blocked {
		// A verification stop must recheck this exact completed leaf on resume,
		// rather than skip its unmet proof or dispatch a replacement call.
		request.Revision.NodeID = node.ID
		request.AttemptState = "interrupted"
	}
	if result.Retryable && !result.Unknown && (request.Outcome == Failed || request.Outcome == Blocked) && node.Failure.ReworkTarget != "" && task.Version.Revision < int64(task.Contract.Budget.MaxRevisions) && task.Revision.Reworks[node.ID] < node.Failure.MaxReworks {
		human := false
		for _, v := range request.Verifications {
			human = human || v.Reason == "human_verification_required"
		}
		if !human {
			request.Outcome, request.Reason = Blocked, "bounded_rework_required"
			request.Revision.NodeID = node.ID
		}
	}
	projected := task
	projected.Revision = request.Revision
	summary, err := BuildSummary(projected, graph)
	if err != nil {
		return task, err
	}
	request.Revision.Summary = summary
	return s.Repository.Commit(ctx, request)
}

func (s *Service) collectEvidence(task Task, attempt NodeAttempt, result NodeRunResult, graph *EvidenceGraph, req *CommitNode) error {
	refs := append(slices.Clone(result.Evidence), task.Contract.InputRefs...)
	for _, ref := range refs {
		if ref.TaskID != "" && ref.TaskID != task.ID {
			return ErrStateConflict
		}
		ref.TaskID, ref.NodeAttemptID = task.ID, attempt.ID
		if ref.RunID == "" {
			ref.RunID = attempt.RunID
		}
		ref.ID = "fe_" + flowInputHash(mustJSON([]string{task.ID, attempt.ID, ref.SourceKind, ref.SourceUID, ref.SourceVersion, ref.Location, ref.ContentHash}))
		duplicate := false
		for _, old := range graph.Evidence {
			if old.ID == ref.ID {
				if old != ref {
					return ErrStateConflict
				}
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		graph.Evidence = append(graph.Evidence, ref)
		req.Evidence = append(req.Evidence, ref)
	}
	return ValidateGraph(*graph)
}

func (s *Service) verifyDraft(ctx context.Context, p core.Principal, task Task, attempt NodeAttempt, rule AcceptanceRule, kind, id string, version int64, raw json.RawMessage, hash string, refs []EvidenceRef, minSources int) (Verification, error) {
	draft := &draftVerification{basis: VerificationBasis{TaskID: task.ID, TaskRevision: task.Version.Revision, TargetKind: kind, TargetID: id, TargetVersion: version, ContractVersion: task.ContractVersion, Rule: rule, Output: raw, ContentHash: hash, Evidence: refs, RequiredSources: minSources}}
	verifier := VerificationService{Schemas: s.Schemas}
	if s.Verification != nil {
		verifier = *s.Verification
	}
	verifier.Schemas = s.Schemas
	verifier.Repository = draft
	verifier.Now = s.Now
	return verifier.Verify(ctx, VerifyRequest{Principal: p, TaskID: task.ID, TaskRevision: task.Version.Revision, NodeAttemptID: attempt.ID, TargetKind: kind, TargetID: id, TargetVersion: version, ContractVersion: task.ContractVersion, RuleID: rule.ID, RuleVersion: rule.Version})
}

func (s *Service) checkSlots(ctx context.Context, p core.Principal, task Task, attempt NodeAttempt, raw json.RawMessage, graph EvidenceGraph, req *CommitNode) error {
	refs := attemptEvidence(graph, attempt.ID)
	for _, spec := range task.Contract.RequiredSlots {
		slot := Slot{ID: spec.ID, State: "missing"}
		for _, ref := range refs {
			slot.EvidenceIDs = append(slot.EvidenceIDs, ref.ID)
		}
		if len(slot.EvidenceIDs) > 0 {
			slot.State = "found"
		}
		all := true
		for _, id := range spec.Validators {
			matched := false
			for _, rule := range task.Contract.Acceptance {
				if rule.ID != id || rule.TargetKind != "slot" || rule.TargetID != spec.ID {
					continue
				}
				matched = true
				v, err := s.verifyDraft(ctx, p, task, attempt, rule, "slot", spec.ID, task.Version.Revision+1, raw, flowInputHash(raw), refs, spec.MinSources)
				if v.ID == "" {
					return errors.Join(ErrStateConflict, err)
				}
				req.Verifications = append(req.Verifications, v)
				slot.VerificationIDs = append(slot.VerificationIDs, v.ID)
				if v.Outcome != "pass" {
					all = false
					slot.State = "unverifiable"
					if v.Reason == "source_conflict" {
						slot.State = "conflict"
					}
				}
			}
			if !matched {
				return ErrDefinitionInvalid
			}
		}
		if all {
			slot.State = "verified"
		}
		req.Revision.Slots[spec.ID] = slot
	}
	return nil
}

func (s *Service) checkClaims(ctx context.Context, p core.Principal, task Task, node Node, attempt NodeAttempt, raw json.RawMessage, graph *EvidenceGraph, req *CommitNode) error {
	var rules []AcceptanceRule
	for _, rule := range task.Contract.Acceptance {
		if rule.TargetKind == "claim" && rule.TargetID == node.ID {
			rules = append(rules, rule)
		}
	}
	if len(rules) == 0 {
		return nil
	}
	claim := ClaimRevision{TaskID: task.ID, ID: node.ID, Revision: 1, TaskRevision: task.Version.Revision, Text: string(raw), State: "proposed", NodeAttemptID: attempt.ID}
	for _, old := range graph.Claims {
		if old.ID == node.ID {
			claim.Revision = max(claim.Revision, old.Revision+1)
		}
	}
	refs := attemptEvidence(*graph, attempt.ID)
	for _, ref := range refs {
		claim.EvidenceIDs = append(claim.EvidenceIDs, ref.ID)
	}
	dependenciesValid := true
	claim.Dependencies = slices.Clone(req.Revision.Outputs[node.ID].Claims)
	for _, ref := range claim.Dependencies {
		dependency, ok := CurrentClaim(req.Revision, *graph, ref.ID)
		dependenciesValid = dependenciesValid && ok && dependency.Revision == ref.Revision && s.claimSourcesCurrent(ctx, p, req.Revision, *graph, dependency)
	}
	all, failed := dependenciesValid, false
	for _, rule := range rules {
		v, err := s.verifyDraft(ctx, p, task, attempt, rule, "claim", claim.ID, claim.Revision, raw, flowInputHash(raw), refs, 0)
		if v.ID == "" {
			return errors.Join(ErrStateConflict, err)
		}
		req.Verifications = append(req.Verifications, v)
		claim.VerificationIDs = append(claim.VerificationIDs, v.ID)
		all = all && v.Outcome == "pass"
		failed = failed || v.Outcome == "fail"
	}
	if all {
		claim.State = "accepted"
	} else if failed {
		claim.State = "rejected"
	}
	output := req.Revision.Outputs[node.ID]
	if all {
		output.Claims = append(output.Claims, ClaimRef{ID: claim.ID, Revision: claim.Revision})
	} else {
		output.Verified = false
	}
	req.Revision.Outputs[node.ID] = output
	if !all {
		for _, rule := range rules {
			if rule.Required {
				req.Outcome, req.Reason = Blocked, "required_claim_verification_missing"
			}
		}
	}
	graph.Claims = append(graph.Claims, claim)
	req.Claims = append(req.Claims, claim)
	return ValidateGraph(*graph)
}

func (s *Service) checkArtifacts(ctx context.Context, p core.Principal, task Task, attempt NodeAttempt, raw json.RawMessage, graph *EvidenceGraph, req *CommitNode) error {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	for _, spec := range task.Contract.ArtifactSpec {
		value := raw
		if len(task.Contract.ArtifactSpec) > 1 {
			value = fields[spec.ID]
			if len(value) == 0 {
				if spec.Required {
					req.Outcome, req.Reason = Blocked, "required_artifact_missing"
				}
				continue
			}
		}
		content := []byte(value)
		if spec.Format == "text" {
			var text string
			if err := json.Unmarshal(value, &text); err != nil {
				req.Outcome, req.Reason = Failed, "artifact_format_invalid"
				continue
			}
			content = []byte(text)
		}
		version := int64(1)
		for _, old := range graph.Artifacts {
			if old.ID == spec.ID {
				version = max(version, old.Version+1)
			}
		}
		artifact := ArtifactVersion{TaskID: task.ID, ID: spec.ID, Version: version, TaskRevision: task.Version.Revision, Format: spec.Format, Content: content, ContentHash: flowInputHash(content), State: "proposed"}
		artifact.Claims = slices.Clone(req.Revision.Outputs[task.Revision.NodeID].Claims)
		slices.SortFunc(artifact.Claims, func(a, b ClaimRef) int { return strings.Compare(a.ID, b.ID) })
		all, matched := req.Revision.Outputs[task.Revision.NodeID].Verified, false
		for _, rule := range task.Contract.Acceptance {
			if rule.Required && rule.TargetKind == "claim" {
				if _, ok := CurrentClaim(req.Revision, *graph, rule.TargetID); !ok {
					all = false
				}
			}
		}
		for _, dependency := range artifact.Claims {
			claim, ok := CurrentClaim(req.Revision, *graph, dependency.ID)
			if !ok || !s.claimSourcesCurrent(ctx, p, req.Revision, *graph, claim) {
				all = false
			}
		}
		for _, rule := range task.Contract.Acceptance {
			if rule.TargetKind != "artifact" || rule.TargetID != spec.ID {
				continue
			}
			matched = true
			v, err := s.verifyDraft(ctx, p, task, attempt, rule, "artifact", spec.ID, version, value, artifact.ContentHash, attemptEvidence(*graph, attempt.ID), 0)
			if v.ID == "" {
				return errors.Join(ErrStateConflict, err)
			}
			req.Verifications = append(req.Verifications, v)
			artifact.VerificationIDs = append(artifact.VerificationIDs, v.ID)
			if v.Outcome != "pass" {
				all = false
				req.Outcome, req.Reason = Blocked, "artifact_verification_missing"
			}
		}
		if all && matched {
			artifact.State = "accepted"
		} else if spec.Required {
			req.Outcome, req.Reason = Blocked, "artifact_verification_missing"
		}
		req.Artifacts = append(req.Artifacts, artifact)
		graph.Artifacts = append(graph.Artifacts, artifact)
	}
	return ValidateGraph(*graph)
}

func (s *Service) claimSourcesCurrent(ctx context.Context, p core.Principal, revision TaskRevision, graph EvidenceGraph, claim ClaimRevision) bool {
	refs := make(map[string]EvidenceRef, len(graph.Evidence))
	for _, ref := range graph.Evidence {
		refs[ref.ID] = ref
	}
	queue := []ClaimRevision{claim}
	seen := make(map[ClaimRef]bool)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		identity := ClaimRef{ID: current.ID, Revision: current.Revision}
		if seen[identity] {
			continue
		}
		seen[identity] = true
		if len(seen) > 1024 {
			return false
		}
		for _, id := range current.EvidenceIDs {
			ref, ok := refs[id]
			if !ok || s.Verification == nil || s.Verification.Sources == nil {
				return false
			}
			verdict, err := s.Verification.Sources.Check(ctx, p, ref)
			if err != nil || !verdict.Valid || verdict.Version != ref.SourceVersion || verdict.ContentHash != ref.ContentHash {
				return false
			}
		}
		for _, dependency := range current.Dependencies {
			parent, ok := CurrentClaim(revision, graph, dependency.ID)
			if !ok || parent.Revision != dependency.Revision {
				return false
			}
			queue = append(queue, parent)
		}
	}
	return true
}

// Old evidence remains in the provenance graph, but only sources actually
// carried into this attempt can substantiate its current target version.
func attemptEvidence(graph EvidenceGraph, attempt string) []EvidenceRef {
	var refs []EvidenceRef
	for _, ref := range graph.Evidence {
		if ref.NodeAttemptID == attempt {
			refs = append(refs, ref)
		}
	}
	return refs
}

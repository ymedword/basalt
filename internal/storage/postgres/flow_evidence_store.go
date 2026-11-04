package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func (s *FlowStore) ReadGraph(ctx context.Context, p core.Principal, id string) (flow.EvidenceGraph, error) {
	if _, err := s.Read(ctx, p, id); err != nil {
		return flow.EvidenceGraph{}, err
	}
	return readFlowGraph(ctx, s.db.pool, id)
}

func readFlowGraph(ctx context.Context, q Queryer, id string) (flow.EvidenceGraph, error) {
	graph := flow.EvidenceGraph{TaskID: id}
	rows, err := q.QueryContext(ctx, `SELECT evidence_id,source_kind,source_uid,source_version,location,content_hash,COALESCE(run_id,''),node_attempt_id FROM flow_evidence_refs WHERE task_id=$1 ORDER BY evidence_id LIMIT 1025`, id)
	if err != nil {
		return graph, err
	}
	for rows.Next() {
		ref := flow.EvidenceRef{TaskID: id}
		if err := rows.Scan(&ref.ID, &ref.SourceKind, &ref.SourceUID, &ref.SourceVersion, &ref.Location, &ref.ContentHash, &ref.RunID, &ref.NodeAttemptID); err != nil {
			return graph, errors.Join(err, rows.Close())
		}
		graph.Evidence = append(graph.Evidence, ref)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return graph, err
	}
	rows, err = q.QueryContext(ctx, `SELECT claim_id,claim_revision,task_revision,text,state FROM flow_claims WHERE task_id=$1 ORDER BY claim_id,claim_revision LIMIT 1025`, id)
	if err != nil {
		return graph, err
	}
	for rows.Next() {
		claim := flow.ClaimRevision{TaskID: id}
		if err := rows.Scan(&claim.ID, &claim.Revision, &claim.TaskRevision, &claim.Text, &claim.State); err != nil {
			return graph, errors.Join(err, rows.Close())
		}
		graph.Claims = append(graph.Claims, claim)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return graph, err
	}
	for index := range graph.Claims {
		claim := &graph.Claims[index]
		rows, err = q.QueryContext(ctx, `SELECT evidence_id FROM flow_claim_evidence WHERE task_id=$1 AND claim_id=$2 AND claim_revision=$3 ORDER BY evidence_id LIMIT 1025`, id, claim.ID, claim.Revision)
		if err != nil {
			return graph, err
		}
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				return graph, errors.Join(err, rows.Close())
			}
			claim.EvidenceIDs = append(claim.EvidenceIDs, ref)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return graph, err
		}
		rows, err = q.QueryContext(ctx, `SELECT dependency_id,dependency_revision FROM flow_claim_dependencies WHERE task_id=$1 AND claim_id=$2 AND claim_revision=$3 ORDER BY dependency_id,dependency_revision LIMIT 1025`, id, claim.ID, claim.Revision)
		if err != nil {
			return graph, err
		}
		for rows.Next() {
			var ref flow.ClaimRef
			if err := rows.Scan(&ref.ID, &ref.Revision); err != nil {
				return graph, errors.Join(err, rows.Close())
			}
			claim.Dependencies = append(claim.Dependencies, ref)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return graph, err
		}
	}
	rows, err = q.QueryContext(ctx, `SELECT artifact_id,version,task_revision,format,content,content_hash,state FROM flow_artifacts WHERE task_id=$1 ORDER BY artifact_id,version LIMIT 1025`, id)
	if err != nil {
		return graph, err
	}
	for rows.Next() {
		artifact := flow.ArtifactVersion{TaskID: id}
		if err := rows.Scan(&artifact.ID, &artifact.Version, &artifact.TaskRevision, &artifact.Format, &artifact.Content, &artifact.ContentHash, &artifact.State); err != nil {
			return graph, errors.Join(err, rows.Close())
		}
		graph.Artifacts = append(graph.Artifacts, artifact)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return graph, err
	}
	for index := range graph.Artifacts {
		artifact := &graph.Artifacts[index]
		rows, err = q.QueryContext(ctx, `SELECT claim_id,claim_revision FROM flow_artifact_claims WHERE task_id=$1 AND artifact_id=$2 AND artifact_version=$3 ORDER BY claim_id,claim_revision LIMIT 1025`, id, artifact.ID, artifact.Version)
		if err != nil {
			return graph, err
		}
		for rows.Next() {
			var ref flow.ClaimRef
			if err := rows.Scan(&ref.ID, &ref.Revision); err != nil {
				return graph, errors.Join(err, rows.Close())
			}
			artifact.Claims = append(artifact.Claims, ref)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return graph, err
		}
	}
	verifications, err := readFlowVerifications(ctx, q, id)
	if err != nil {
		return graph, err
	}
	for _, v := range verifications {
		for i := range graph.Claims {
			claim := &graph.Claims[i]
			if v.TargetKind == "claim" && v.TargetID == claim.ID && v.TargetVersion == claim.Revision {
				if claim.NodeAttemptID != "" && claim.NodeAttemptID != v.NodeAttemptID {
					return graph, flow.ErrStateConflict
				}
				claim.NodeAttemptID = v.NodeAttemptID
				claim.VerificationIDs = append(claim.VerificationIDs, v.ID)
			}
		}
		for i := range graph.Artifacts {
			artifact := &graph.Artifacts[i]
			if v.TargetKind == "artifact" && v.TargetID == artifact.ID && v.TargetVersion == artifact.Version {
				artifact.VerificationIDs = append(artifact.VerificationIDs, v.ID)
			}
		}
	}
	return graph, flow.ValidateGraph(graph)
}

func readFlowVerifications(ctx context.Context, q Queryer, id string) ([]flow.Verification, error) {
	rows, err := q.QueryContext(ctx, `SELECT detail FROM flow_verifications WHERE task_id=$1 ORDER BY created_at,verification_id LIMIT 1025`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []flow.Verification
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var verification flow.Verification
		if err := json.Unmarshal(raw, &verification); err != nil {
			return nil, err
		}
		result = append(result, verification)
	}
	if len(result) > 1024 {
		return nil, flow.ErrCapacity
	}
	return result, rows.Err()
}

func persistFlowEvidence(ctx context.Context, tx *sql.Tx, req flow.CommitNode) error {
	for _, ref := range req.Evidence {
		if ref.TaskID != req.TaskID || ref.NodeAttemptID != req.AttemptID {
			return flow.ErrStateConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_evidence_refs(task_id,evidence_id,source_kind,source_uid,source_version,location,content_hash,run_id,node_attempt_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, ref.TaskID, ref.ID, ref.SourceKind, ref.SourceUID, ref.SourceVersion, ref.Location, ref.ContentHash, nullIfEmpty(ref.RunID), ref.NodeAttemptID); err != nil {
			return err
		}
	}
	for _, claim := range req.Claims {
		if claim.TaskID != req.TaskID || claim.TaskRevision != req.Expected.Revision {
			return flow.ErrStateConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_claims(task_id,claim_id,claim_revision,task_revision,text,state) VALUES($1,$2,$3,$4,$5,$6)`, claim.TaskID, claim.ID, claim.Revision, claim.TaskRevision, claim.Text, claim.State); err != nil {
			return err
		}
	}
	// Insert all claim versions before any dependency edges, including forward
	// references between claims in this single atomically accepted result.
	for _, claim := range req.Claims {
		for _, id := range claim.EvidenceIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO flow_claim_evidence(task_id,claim_id,claim_revision,evidence_id) VALUES($1,$2,$3,$4)`, claim.TaskID, claim.ID, claim.Revision, id); err != nil {
				return err
			}
		}
		for _, ref := range claim.Dependencies {
			if _, err := tx.ExecContext(ctx, `INSERT INTO flow_claim_dependencies(task_id,claim_id,claim_revision,dependency_id,dependency_revision) VALUES($1,$2,$3,$4,$5)`, claim.TaskID, claim.ID, claim.Revision, ref.ID, ref.Revision); err != nil {
				return err
			}
		}
	}
	for _, artifact := range req.Artifacts {
		if artifact.TaskID != req.TaskID || artifact.TaskRevision != req.Expected.Revision || artifact.ContentHash != flowDataHash(artifact.Content) {
			return flow.ErrStateConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_artifacts(task_id,artifact_id,version,task_revision,format,content,content_hash,state) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, artifact.TaskID, artifact.ID, artifact.Version, artifact.TaskRevision, artifact.Format, artifact.Content, artifact.ContentHash, artifact.State); err != nil {
			return err
		}
		for _, ref := range artifact.Claims {
			if _, err := tx.ExecContext(ctx, `INSERT INTO flow_artifact_claims(task_id,artifact_id,artifact_version,claim_id,claim_revision) VALUES($1,$2,$3,$4,$5)`, artifact.TaskID, artifact.ID, artifact.Version, ref.ID, ref.Revision); err != nil {
				return err
			}
		}
	}
	for _, v := range req.Verifications {
		detail := struct {
			FormatVersion int `json:"format_version"`
			flow.Verification
		}{1, v}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_verifications(task_id,verification_id,task_revision,node_attempt_id,target_kind,target_id,target_version,contract_version,rule_id,rule_version,validator_kind,validator_id,outcome,reason,detail,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, v.TaskID, v.ID, v.TaskRevision, v.NodeAttemptID, v.TargetKind, v.TargetID, v.TargetVersion, v.ContractVersion, v.RuleID, v.RuleVersion, v.ValidatorKind, v.ValidatorID, v.Outcome, v.Reason, mustFlowJSON(detail), v.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func validateFlowVerifications(task flow.Task, req flow.CommitNode, graph flow.EvidenceGraph, verifications []flow.Verification) error {
	if len(verifications) > 1024 {
		return flow.ErrCapacity
	}
	if output, ok := req.Revision.Outputs[task.Revision.NodeID]; ok && output.Verified {
		var expected []flow.ClaimRef
		for _, node := range task.AcceptedVersion.Definition.Nodes {
			if node.ID == task.Revision.NodeID {
				for _, mapping := range node.Inputs {
					parts := strings.Split(mapping.Source, ".")
					if len(parts) > 2 && parts[0] == "outputs" {
						upstream, ok := task.Revision.Outputs[parts[1]]
						if !ok || !upstream.Verified {
							return flow.ErrStateConflict
						}
						for _, ref := range upstream.Claims {
							if !slices.Contains(expected, ref) {
								expected = append(expected, ref)
							}
						}
					}
				}
			}
		}
		for _, claim := range req.Claims {
			if claim.State == "accepted" {
				expected = append(expected, flow.ClaimRef{ID: claim.ID, Revision: claim.Revision})
			}
		}
		if len(expected) != len(output.Claims) {
			return fmt.Errorf("flow output missing claim provenance: %w", flow.ErrStateConflict)
		}
		for _, ref := range expected {
			if !slices.Contains(output.Claims, ref) {
				return fmt.Errorf("flow output changed claim provenance: %w", flow.ErrStateConflict)
			}
		}
	}
	for _, v := range req.Verifications {
		if v.TaskID != task.ID || v.TaskRevision != task.Version.Revision || v.NodeAttemptID != req.AttemptID || v.ContractVersion != task.ContractVersion || v.ValidatorID == "" || len(v.BasisHashes) == 0 {
			return flow.ErrStateConflict
		}
		matched := false
		for _, rule := range task.Contract.Acceptance {
			if rule.ID == v.RuleID && rule.Version == v.RuleVersion && rule.Kind == v.ValidatorKind && rule.TargetKind == v.TargetKind && rule.TargetID == v.TargetID {
				matched = true
				break
			}
		}
		if !matched {
			return flow.ErrStateConflict
		}
	}
	passed := func(kind, id string, version int64, required bool) bool {
		producerRevision := task.Version.Revision
		if kind == "claim" {
			claim, ok := flow.CurrentClaim(req.Revision, graph, id)
			if !ok || claim.Revision != version {
				return false
			}
			producerRevision = claim.TaskRevision
		}
		found := false
		for _, rule := range task.Contract.Acceptance {
			if rule.TargetKind != kind || rule.TargetID != id || required && !rule.Required {
				continue
			}
			found = true
			pass := false
			for _, v := range verifications {
				if v.TargetKind == kind && v.TargetID == id && v.TargetVersion == version && v.ContractVersion == task.ContractVersion && v.RuleID == rule.ID && v.RuleVersion == rule.Version && v.TaskRevision == producerRevision && v.Outcome == "pass" {
					pass = true
					break
				}
			}
			if !pass {
				return false
			}
		}
		return found
	}
	for _, claim := range req.Claims {
		output, ok := req.Revision.Outputs[claim.ID]
		if claim.ID != task.Revision.NodeID || claim.NodeAttemptID != req.AttemptID || claim.TaskRevision != task.Version.Revision || flowDataHash([]byte(claim.Text)) != req.OutputHash {
			return fmt.Errorf("flow claim output binding: %w", flow.ErrStateConflict)
		}
		if claim.State == "accepted" && (!ok || !output.Verified || output.NodeAttemptID != req.AttemptID || output.Hash != req.OutputHash) {
			return fmt.Errorf("flow accepted claim output binding: %w", flow.ErrStateConflict)
		}
		if claim.State == "accepted" && !passed("claim", claim.ID, claim.Revision, false) {
			return fmt.Errorf("flow claim verification binding: %w", flow.ErrStateConflict)
		}
		if claim.State == "accepted" {
			for _, dep := range claim.Dependencies {
				current, ok := flow.CurrentClaim(req.Revision, graph, dep.ID)
				if !ok || current.Revision != dep.Revision {
					return fmt.Errorf("flow claim dependency binding: %w", flow.ErrStateConflict)
				}
			}
		}
	}
	for _, artifact := range req.Artifacts {
		if artifact.State == "accepted" && !passed("artifact", artifact.ID, artifact.Version, false) {
			return flow.ErrStateConflict
		}
		if artifact.State == "accepted" {
			produced, ok := req.Revision.Outputs[task.Revision.NodeID]
			if !ok || !produced.Verified || len(produced.Claims) != len(artifact.Claims) {
				return fmt.Errorf("flow artifact producer proof: %w", flow.ErrStateConflict)
			}
			for _, ref := range produced.Claims {
				if !slices.Contains(artifact.Claims, ref) {
					return fmt.Errorf("flow artifact missing dependency: %w", flow.ErrStateConflict)
				}
			}
			for _, dep := range artifact.Claims {
				current, ok := flow.CurrentClaim(req.Revision, graph, dep.ID)
				if !ok || current.Revision != dep.Revision {
					return fmt.Errorf("flow artifact claim binding: %w", flow.ErrStateConflict)
				}
			}
		}
	}
	for _, slot := range req.Revision.Slots {
		if slot.State == "verified" && !passed("slot", slot.ID, task.Version.Revision+1, false) {
			return flow.ErrStateConflict
		}
	}
	if req.Outcome == flow.Succeeded {
		for _, rule := range task.Contract.Acceptance {
			if rule.Required && rule.TargetKind == "claim" {
				claim, ok := flow.CurrentClaim(req.Revision, graph, rule.TargetID)
				if !ok || !passed("claim", claim.ID, claim.Revision, true) {
					return fmt.Errorf("flow required claim binding: %w", flow.ErrStateConflict)
				}
			}
		}
		for _, slot := range task.Contract.RequiredSlots {
			if req.Revision.Slots[slot.ID].State != "verified" || !passed("slot", slot.ID, task.Version.Revision+1, true) {
				return flow.ErrStateConflict
			}
		}
		for _, spec := range task.Contract.ArtifactSpec {
			if !spec.Required {
				continue
			}
			accepted := false
			for _, artifact := range graph.Artifacts {
				if artifact.ID == spec.ID && artifact.State == "accepted" && passed("artifact", artifact.ID, artifact.Version, true) {
					accepted = true
					break
				}
			}
			if !accepted {
				return flow.ErrStateConflict
			}
		}
	}
	return nil
}

func (s *FlowStore) Commit(ctx context.Context, req flow.CommitNode) (flow.Task, error) {
	var result flow.Task
	if !req.Principal.Valid() {
		return result, core.ErrUnauthorized
	}
	if req.OperationID == "" || req.AttemptID == "" {
		return result, flow.ErrStateConflict
	}
	hash := flowOperationHash("commit", req)
	err := s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return findFlowOperation(ctx, q, req.TaskID, req.OperationID, "commit", hash)
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, req.TaskID, req.Principal)
		if err != nil {
			return err
		}
		if task.CallerID != req.Principal.CallerID {
			return flow.ErrNotFound
		}
		found, err := findFlowOperation(ctx, tx, req.TaskID, req.OperationID, "commit", hash)
		if err != nil {
			return err
		}
		if found {
			result = task
			return nil
		}
		if err := checkFlowOwnedVersion(task, req.Principal, req.Expected); err != nil {
			return err
		}
		if task.Status != flow.Running || task.CloseIntent != flow.CloseNone || task.CancelRequested || req.Now >= task.Deadline {
			return flow.ErrStateConflict
		}
		// Validation/source callbacks ran outside SQL and may have revoked the
		// original key. Recheck under the root lock before accepting new facts.
		if err := checkFlowAuthorizingKey(ctx, tx, task, req.Principal, s.db.clock().UnixMilli()); err != nil {
			return err
		}
		if err := ensureFlowAncestorsOpen(ctx, tx, task, req.Now); err != nil {
			return err
		}
		if req.Revision.FormatVersion != 1 || req.Revision.TaskID != task.ID || req.Revision.Revision != task.Version.Revision || req.Revision.InputHash != task.Revision.InputHash || req.Revision.ExpandedSteps != task.Revision.ExpandedSteps || req.OutputHash != flowDataHash(req.Output) {
			return flow.ErrStateConflict
		}
		// Per-call observations may have advanced the durable window after Claim.
		// Node acceptance cannot overwrite it with its pre-call checkpoint copy.
		req.Revision.Progress = task.Revision.Progress
		var nodeID, state string
		var revision, epoch int64
		if err := tx.QueryRowContext(ctx, `SELECT node_id,state,task_revision,execution_epoch FROM flow_node_attempts WHERE task_id=$1 AND node_attempt_id=$2 FOR UPDATE`, task.ID, req.AttemptID).Scan(&nodeID, &state, &revision, &epoch); err != nil {
			return err
		}
		if revision != task.Version.Revision || nodeID != task.Revision.NodeID {
			return flow.ErrStateConflict
		}
		if req.Recovered {
			if state != "running" && state != "interrupted" && state != "unknown" || epoch > task.Version.ExecutionEpoch {
				return flow.ErrStateConflict
			}
			if err := validateFlowRecoveredOutput(ctx, tx, task, req); err != nil {
				return err
			}
		} else if state != "running" || epoch != task.Version.ExecutionEpoch {
			return flow.ErrStateConflict
		}
		if req.Outcome == flow.Queued { // running -> queued is not a legal public transition.
			return flow.ErrStateConflict
		}
		graph, err := readFlowGraph(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		graph.Evidence = append(graph.Evidence, req.Evidence...)
		graph.Claims = append(graph.Claims, req.Claims...)
		graph.Artifacts = append(graph.Artifacts, req.Artifacts...)
		if err := flow.ValidateGraph(graph); err != nil {
			return err
		}
		verifications, err := readFlowVerifications(ctx, tx, task.ID)
		if err != nil {
			return err
		}
		verifications = append(verifications, req.Verifications...)
		if err := validateFlowVerifications(task, req, graph, verifications); err != nil {
			return err
		}
		if err := persistFlowEvidence(ctx, tx, req); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flow_node_attempts SET state=$3,output=$4,output_hash=$5,finished_at=$6 WHERE task_id=$1 AND node_attempt_id=$2`, task.ID, req.AttemptID, req.AttemptState, string(req.Output), req.OutputHash, req.Now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flow_task_revisions SET checkpoint=$3 WHERE task_id=$1 AND revision=$2`, task.ID, task.Version.Revision, mustFlowJSON(req.Revision)); err != nil {
			return err
		}
		if err := updateFlowHead(ctx, tx, task, req.Outcome, task.Version.Revision, task.Version.ExecutionEpoch, task.CloseIntent, task.CancelRequested, req.Now, req.Reason); err != nil {
			return err
		}
		if err := appendFlowTransition(ctx, tx, task, req.OperationID, "commit", hash, req.Outcome, task.Version.Revision, req.Now, req.Reason, req.Principal); err != nil {
			return err
		}
		result, err = readFlowTask(ctx, tx, task.ID)
		return err
	})
	return result, err
}

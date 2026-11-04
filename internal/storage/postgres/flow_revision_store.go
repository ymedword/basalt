package postgres

import (
	"context"
	"database/sql"
	"maps"
	"reflect"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

var (
	_ flow.Repository       = (*FlowStore)(nil)
	_ flow.WorkerRepository = (*FlowStore)(nil)
)

func (s *FlowStore) Revise(ctx context.Context, req flow.AcceptRevision) (flow.Task, error) {
	var result flow.Task
	r := req.Request
	if !r.Principal.Valid() || r.OperationID == "" {
		return result, core.ErrUnauthorized
	}
	hash := flowRevisionRequestHash(r)
	err := s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return findFlowOperation(ctx, q, r.TaskID, r.OperationID, "revise", hash)
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, r.TaskID, r.Principal)
		if err != nil {
			return err
		}
		if task.CallerID != r.Principal.CallerID {
			return flow.ErrNotFound
		}
		found, err := findFlowOperation(ctx, tx, r.TaskID, r.OperationID, "revise", hash)
		if err != nil {
			return err
		}
		if found {
			result = task
			return nil
		}
		if err := checkFlowOwnedVersion(task, r.Principal, r.Expected); err != nil {
			return err
		}
		if task.Status != flow.Blocked || task.CloseIntent != flow.CloseNone || task.CancelRequested || req.Now >= task.Deadline || r.Principal.KeyID != task.AuthorizingKeyID {
			return flow.ErrStateConflict
		}
		if err := ensureFlowAncestorsOpen(ctx, tx, task, req.Now); err != nil {
			return err
		}
		unknown, err := unknownFlowActions(ctx, tx, task.RootTaskID)
		if err != nil {
			return err
		}
		if unknown {
			return flow.ErrActionUnknown
		}
		next := req.Revision
		reworks := maps.Clone(task.Revision.Reworks)
		if reworks == nil {
			reworks = make(map[string]int)
		}
		reworks[task.Revision.NodeID]++
		if next.FormatVersion != 1 || next.TaskID != task.ID || next.Revision != task.Version.Revision+1 || next.Revision > int64(task.Contract.Budget.MaxRevisions) || next.InputHash != flowDataHash(next.Input) || next.ExpandedSteps != task.Revision.ExpandedSteps || !reflect.DeepEqual(next.Attempts, task.Revision.Attempts) || !reflect.DeepEqual(next.Reworks, reworks) || !reflect.DeepEqual(next.Progress, task.Revision.Progress) || next.Summary.Goal != task.Contract.Goal || !reflect.DeepEqual(next.Summary.HardConstraints, task.Contract.Constraints) || next.Summary.TaskRevision != next.Revision {
			return flow.ErrBudget
		}
		knownNode := false
		for _, node := range task.AcceptedVersion.Definition.Nodes {
			if node.ID == next.NodeID {
				knownNode = true
				break
			}
		}
		if !knownNode {
			return flow.ErrStateConflict
		}
		// A proposal may discard old outputs but cannot mint a verified output.
		for key, output := range next.Outputs {
			old, found := task.Revision.Outputs[key]
			if !found || !reflect.DeepEqual(old, output) {
				return flow.ErrStateConflict
			}
		}
		for _, slot := range next.Slots {
			if slot.State == "verified" {
				old, found := task.Revision.Slots[slot.ID]
				if !found || !reflect.DeepEqual(old, slot) {
					return flow.ErrStateConflict
				}
			}
		}
		for _, claim := range req.InvalidClaims {
			changed, err := tx.ExecContext(ctx, `UPDATE flow_claims SET state='stale' WHERE task_id=$1 AND claim_id=$2 AND claim_revision=$3`, task.ID, claim.ID, claim.Revision)
			if err != nil {
				return err
			}
			n, _ := changed.RowsAffected()
			if n != 1 {
				return flow.ErrStateConflict
			}
		}
		for _, artifact := range req.InvalidArtifacts {
			changed, err := tx.ExecContext(ctx, `UPDATE flow_artifacts SET state='stale' WHERE task_id=$1 AND artifact_id=$2 AND version=$3`, task.ID, artifact.ID, artifact.Version)
			if err != nil {
				return err
			}
			n, _ := changed.RowsAffected()
			if n != 1 {
				return flow.ErrStateConflict
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_task_revisions(task_id,revision,contract_version,checkpoint,created_at) VALUES($1,$2,$3,$4,$5)`, task.ID, next.Revision, task.ContractVersion, mustFlowJSON(next), req.Now); err != nil {
			return err
		}
		if err := updateFlowHead(ctx, tx, task, flow.Queued, next.Revision, task.Version.ExecutionEpoch+1, flow.CloseNone, false, req.Now, "bounded revision accepted"); err != nil {
			return err
		}
		if err := appendFlowTransition(ctx, tx, task, r.OperationID, "revise", hash, flow.Queued, next.Revision, req.Now, "bounded revision accepted", r.Principal); err != nil {
			return err
		}
		result, err = readFlowTask(ctx, tx, task.ID)
		return err
	})
	return result, err
}

func flowRevisionRequestHash(r flow.RevisionRequest) string {
	return flowOperationHash("revise", struct {
		TaskID, CallerID, KeyID string
		Expected                flow.TaskVersion
		Proposal                []byte
	}{r.TaskID, r.Principal.CallerID, r.Principal.KeyID, r.Expected, r.Proposal})
}

// FindRevision reconciles an accepted operation before reinterpreting today's
// sources or version head. A reused id with different inputs is a conflict.
func (s *FlowStore) FindRevision(ctx context.Context, req flow.RevisionRequest) (flow.Task, bool, error) {
	task, err := s.Read(ctx, req.Principal, req.TaskID)
	if err != nil {
		return task, false, err
	}
	found, err := findFlowOperation(ctx, s.db, req.TaskID, req.OperationID, "revise", flowRevisionRequestHash(req))
	return task, found, err
}

package flow

import (
	"encoding/json"
	"errors"
	"slices"
)

// BuildSummary 从权威状态确定性生成摘要，不让模型改写硬约束或未解槽位。
// BuildSummary derives state without letting a model rewrite hard constraints.
func BuildSummary(task Task, graph EvidenceGraph) (TaskSummary, error) {
	if task.Revision.FormatVersion != 1 || task.Version.Revision != task.Revision.Revision || graph.TaskID != task.ID {
		return TaskSummary{}, ErrCheckpointUnsupported
	}
	if err := ValidateGraph(graph); err != nil {
		return TaskSummary{}, err
	}
	summary := TaskSummary{TaskRevision: task.Version.Revision, Goal: task.Contract.Goal, HardConstraints: slices.Clone(task.Contract.Constraints)}
	for _, slot := range task.Contract.RequiredSlots {
		state, found := task.Revision.Slots[slot.ID]
		if !found || state.State != "verified" {
			summary.Unresolved = append(summary.Unresolved, slot.ID)
		}
	}
	for id, output := range task.Revision.Outputs {
		if output.Verified {
			summary.Progress = append(summary.Progress, id)
		}
	}
	for _, artifact := range graph.Artifacts {
		if artifact.State == "accepted" {
			summary.AcceptedArtifacts = append(summary.AcceptedArtifacts, artifact.ID)
		}
	}
	for _, ref := range graph.Evidence {
		summary.BasisRefs = append(summary.BasisRefs, ref.ID)
	}
	slices.Sort(summary.Progress)
	slices.Sort(summary.Unresolved)
	slices.Sort(summary.AcceptedArtifacts)
	slices.Sort(summary.BasisRefs)
	encoded, err := json.Marshal(summary)
	if err != nil || len(encoded) > 1<<20 {
		return TaskSummary{}, errors.New("flow hard state exceeds context bound")
	}
	return summary, nil
}

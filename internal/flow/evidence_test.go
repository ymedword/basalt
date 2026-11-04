package flow

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func graphFixture() EvidenceGraph {
	return EvidenceGraph{
		TaskID:   "task",
		Evidence: []EvidenceRef{{ID: "e1", TaskID: "task", SourceKind: "knowledge", SourceUID: "doc1", SourceVersion: "v1", Location: "document", ContentHash: "hash1"}, {ID: "e2", TaskID: "task", SourceKind: "knowledge", SourceUID: "doc2", SourceVersion: "v1", Location: "document", ContentHash: "hash2"}},
		Claims: []ClaimRevision{
			{TaskID: "task", ID: "a", Revision: 1, State: "accepted", EvidenceIDs: []string{"e1"}},
			{TaskID: "task", ID: "b", Revision: 1, State: "accepted", Dependencies: []ClaimRef{{ID: "a", Revision: 1}}},
			{TaskID: "task", ID: "c", Revision: 1, State: "accepted", Dependencies: []ClaimRef{{ID: "b", Revision: 1}}},
			{TaskID: "task", ID: "unrelated", Revision: 1, State: "accepted", EvidenceIDs: []string{"e2"}},
		},
		Artifacts: []ArtifactVersion{{TaskID: "task", ID: "report", Version: 1, ContentHash: "hash", State: "accepted", Claims: []ClaimRef{{ID: "c", Revision: 1}}}, {TaskID: "task", ID: "other", Version: 1, ContentHash: "hash2", State: "accepted", Claims: []ClaimRef{{ID: "unrelated", Revision: 1}}}},
	}
}

func TestFlow_EvidenceInvalidationClosure(t *testing.T) {
	claims, artifacts, err := InvalidationClosure(graphFixture(), []string{"e1"})
	require.NoError(t, err)
	require.ElementsMatch(t, []ClaimRef{{ID: "a", Revision: 1}, {ID: "b", Revision: 1}, {ID: "c", Revision: 1}}, claims, "invalidation must reach every transitive dependency")
	require.Equal(t, []ArtifactRef{{ID: "report", Version: 1}}, artifacts, "unrelated accepted artifacts must remain valid")
}

func TestFlow_ClaimDependencyCycleRejected(t *testing.T) {
	graph := graphFixture()
	graph.Claims[0].Dependencies = []ClaimRef{{ID: "c", Revision: 1}}
	require.Error(t, ValidateGraph(graph), "claim cycles must be rejected before any mutation")
	graph = graphFixture()
	graph.Claims[1].TaskID = "other-caller-task"
	require.Error(t, ValidateGraph(graph), "cross-task dependencies must be rejected")
}

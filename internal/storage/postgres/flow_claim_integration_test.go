//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

type flowClaimRunner struct {
	flowAdvanceRunner
	refs []flow.EvidenceRef
}

func (r *flowClaimRunner) Run(ctx context.Context, req flow.NodeRunRequest) (flow.NodeRunResult, error) {
	value, err := r.flowAdvanceRunner.Run(ctx, req)
	value.Evidence = r.refs
	return value, err
}

type flowClaimSourceProbe struct{ stale bool }

type flowClaimOutputProbe struct{ store *FlowStore }

func (p flowClaimOutputProbe) CheckOutput(ctx context.Context, owner core.Principal, task flow.Task, output flow.NodeOutput) error {
	_, err := p.store.OutputEvidence(ctx, owner, task.ID, output)
	return err
}

func (p flowClaimSourceProbe) Check(_ context.Context, _ core.Principal, ref flow.EvidenceRef) (flow.SourceVerdict, error) {
	return flow.SourceVerdict{Valid: !p.stale, Version: ref.SourceVersion, ContentHash: ref.ContentHash}, nil
}

type flowClaimSupportProbe struct{ conflict bool }

func (flowClaimSupportProbe) Identity() (string, int64) { return "seam-exact-checker", 1 }
func (p flowClaimSupportProbe) CheckSupport(_ context.Context, _ core.Principal, basis flow.VerificationBasis) (flow.SupportVerdict, error) {
	hash, err := flow.VerificationBasisHash(basis)
	outcome := "supported"
	if p.conflict {
		outcome = "conflict"
	}
	return flow.SupportVerdict{Outcome: outcome, BasisHash: hash}, err
}

// Uses real PG, original Run rows and transactional claim/artifact relations.
// The seam runner/support probe is not a production model or source validator.
func TestFlow_ClaimAcceptanceAndArtifactDependenciesAreAtomic(t *testing.T) {
	for _, mode := range []string{"accepted", "unproven", "conflict", "duplicate-sources", "stale-source", "dependency-write-fault"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newFlowStoreTest(t)
			def := flowStoreDefinition()
			final := def.Nodes[0]
			final.ID = "final"
			final.Inputs = []flow.InputMapping{{Target: "answer", Source: "outputs.draft.answer"}}
			def.Nodes[0].Next = "final"
			def.Nodes = append(def.Nodes, final)
			for _, id := range []string{"draft", "final"} {
				def.Contract.Acceptance = append(def.Contract.Acceptance, flow.AcceptanceRule{ID: "proof-" + id, Version: 1, Kind: "evidence", TargetKind: "claim", TargetID: id, Required: true, MinSources: 2, ValidatorID: "seam-exact-checker"})
			}
			version := publishFlowStoreDefinition(t, s, def, 1)
			service := flowServiceForStore(s)
			service.Outputs = flowClaimOutputProbe{store: s}
			service.Verification = &flow.VerificationService{Sources: flowClaimSourceProbe{stale: mode == "stale-source"}, Support: flowClaimSupportProbe{conflict: mode == "conflict"}}
			if mode == "unproven" {
				service.Verification.Support = nil
			}
			refs := []flow.EvidenceRef{{SourceKind: "knowledge", SourceUID: "doc1", SourceVersion: "1", ContentHash: "h1", Location: "source:doc1"}, {SourceKind: "knowledge", SourceUID: "doc2", SourceVersion: "1", ContentHash: "h2", Location: "source:doc2"}}
			if mode == "duplicate-sources" {
				refs[1] = refs[0]
			}
			runner := &flowClaimRunner{flowAdvanceRunner: flowAdvanceRunner{store: s, output: json.RawMessage(`{"answer":"report"}`)}, refs: refs}
			service.Runner = runner
			task, err := service.Start(t.Context(), startFlowRequest(version, "claim-probe"))
			require.NoError(t, err)
			// 凭证必须原样下传：子 Run 创建（createRunForNode）以它证明父根归属。
			root := flowRootLeaseFor(t, s.db, task.RootTaskID)
			task, err = service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Lease: root})
			require.NoError(t, err)
			graph, err := s.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
			require.NoError(t, err)
			require.Len(t, graph.Claims, 1)
			claim := graph.Claims[0]
			require.NotEmpty(t, claim.NodeAttemptID)
			require.Len(t, claim.VerificationIDs, 1)
			if mode != "accepted" && mode != "dependency-write-fault" {
				require.Equal(t, flow.Blocked, task.Status)
				require.Equal(t, "draft", task.Revision.NodeID)
				require.NotEqual(t, "accepted", claim.State)
				require.Empty(t, graph.Artifacts)
				require.EqualValues(t, 1, runner.calls.Load())
				return
			}
			require.Equal(t, flow.Running, task.Status)
			require.Equal(t, "accepted", claim.State)
			if mode == "dependency-write-fault" {
				installFlowFault(t, s, "flow_claim_dependencies", "INSERT", "")
			}
			finished, err := service.Advance(t.Context(), flow.AdvanceRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: task.ID, Expected: task.Version, Lease: root})
			if mode == "dependency-write-fault" {
				require.Error(t, err)
				var fired bool
				require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT is_called FROM flow_probe_fault_seq`).Scan(&fired))
				require.True(t, fired)
				graph, err = s.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
				require.NoError(t, err)
				require.Len(t, graph.Claims, 1)
				require.Empty(t, graph.Artifacts)
				var count int
				require.NoError(t, s.db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM flow_verifications WHERE task_id=$1`, task.ID).Scan(&count))
				require.Equal(t, 1, count, "no final claim, artifact or verification may leak out of the failed transaction")
				return
			}
			require.NoError(t, err)
			require.Equal(t, flow.Succeeded, finished.Status)
			graph, err = s.ReadGraph(t.Context(), core.LocalAdminCLIPrincipal(), task.ID)
			require.NoError(t, err)
			require.Len(t, graph.Claims, 2)
			require.Len(t, graph.Artifacts, 1)
			current, ok := flow.CurrentClaim(finished.Revision, graph, "final")
			require.True(t, ok)
			require.Equal(t, []flow.ClaimRef{{ID: "draft", Revision: 1}}, current.Dependencies)
			require.ElementsMatch(t, []flow.ClaimRef{{ID: "draft", Revision: 1}, {ID: "final", Revision: 1}}, graph.Artifacts[0].Claims)
			require.Equal(t, "accepted", graph.Artifacts[0].State)
			require.Len(t, graph.Artifacts[0].VerificationIDs, 1)
			wrong := finished.Revision
			wrong.Outputs = map[string]flow.NodeOutput{"final": finished.Revision.Outputs["final"]}
			out := wrong.Outputs["final"]
			out.NodeAttemptID = "different-attempt-with-same-text"
			wrong.Outputs["final"] = out
			_, ok = flow.CurrentClaim(wrong, graph, "final")
			require.False(t, ok, "equal text does not transfer verification to a different attempt")
		})
	}
}

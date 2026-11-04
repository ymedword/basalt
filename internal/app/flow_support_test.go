package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/knowledge"
)

type flowProofViewProbe struct {
	result knowledge.SourceViewResult
	calls  int
	after  func()
}

func (v *flowProofViewProbe) SourceView(_ context.Context, _ core.Principal, req knowledge.SourceViewRequest) (knowledge.SourceViewResult, error) {
	v.calls++
	if v.after != nil {
		v.after()
	}
	return v.result, nil
}

type flowProofValidityProbe struct{ valid bool }

func (v *flowProofValidityProbe) Check(_ context.Context, _ core.Principal, ref flow.EvidenceRef) (flow.SourceVerdict, error) {
	return flow.SourceVerdict{Valid: v.valid, Version: ref.SourceVersion, ContentHash: ref.ContentHash}, nil
}

func TestFlow_SourceValueProofBindsExactDataWithoutSemanticInference(t *testing.T) {
	for _, mode := range []string{"equal", "exact-large-integer", "conflicting", "substring", "old-version", "truncated", "unlocated", "unnamed", "revoked-during-read", "key-revoked-during-read", "duplicate-source"} {
		t.Run(mode, func(t *testing.T) {
			text := `{"amount":9007199254740993}`
			ref := flow.EvidenceRef{ID: "ev", TaskID: "ft", SourceKind: core.EvidenceKindKnowledge, SourceUID: "doc", SourceVersion: "v1", ContentHash: "hash", Location: "source:doc"}
			basis := flow.VerificationBasis{TaskID: "ft", TargetKind: "claim", TargetID: "draft", TaskRevision: 1, TargetVersion: 1, ContractVersion: 1, Rule: flow.AcceptanceRule{ID: "proof", Version: 1, Kind: "evidence", MinSources: 1, ValidatorID: flowSourceValueValidator}, Output: json.RawMessage(text), ContentHash: "output-hash", Evidence: []flow.EvidenceRef{ref}}
			viewer := &flowProofViewProbe{result: knowledge.SourceViewResult{Validity: knowledge.ValidityResult{Valid: true}, VersionID: "v1", ContentHash: "hash", Text: text, Range: knowledge.EvidenceRange{End: len([]rune(text))}}}
			validity := &flowProofValidityProbe{valid: true}
			revoked := false
			checker := flowSourceValueSupport{viewer: viewer, sources: validity, check: func(context.Context, core.Principal, flow.VerificationBasis, flow.EvidenceRef) error {
				if revoked {
					return core.ErrUnauthorized
				}
				return nil
			}}
			want := "unverifiable"
			switch mode {
			case "equal":
				want = "supported"
			case "exact-large-integer":
				basis.Output = json.RawMessage(`{"amount":9007199254740992}`)
				want = "conflict"
			case "conflicting":
				basis.Output = json.RawMessage(`{"amount":7}`)
				want = "conflict"
			case "substring":
				basis.Output = json.RawMessage(`9007199254740993`)
				want = "conflict"
			case "old-version":
				viewer.result.Superseded = true
			case "truncated":
				viewer.result.Truncated = true
			case "unlocated":
				basis.Evidence[0].Location = "unknown"
			case "unnamed":
				basis.Rule.ValidatorID = ""
			case "revoked-during-read":
				viewer.after = func() { validity.valid = false }
			case "key-revoked-during-read":
				viewer.after = func() { revoked = true }
			case "duplicate-source":
				basis.Rule.MinSources = 2
				basis.Evidence = append(basis.Evidence, ref)
			}
			verdict, err := checker.CheckSupport(t.Context(), core.LocalAdminCLIPrincipal(), basis)
			if mode == "key-revoked-during-read" {
				require.ErrorIs(t, err, core.ErrUnauthorized)
				return
			}
			require.NoError(t, err)
			require.Equal(t, want, verdict.Outcome)
			hash, err := flow.VerificationBasisHash(basis)
			require.NoError(t, err)
			require.Equal(t, hash, verdict.BasisHash)
			if mode == "unlocated" || mode == "unnamed" {
				require.Zero(t, viewer.calls)
			}
		})
	}
}

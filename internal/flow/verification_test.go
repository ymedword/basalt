package flow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

type verificationRepoStub struct {
	basis VerificationBasis
	saved []Verification
}

func (s *verificationRepoStub) LoadVerificationBasis(context.Context, VerifyRequest) (VerificationBasis, error) {
	return s.basis, nil
}

func (s *verificationRepoStub) RecordVerification(_ context.Context, _ VerifyRequest, result Verification) error {
	s.saved = append(s.saved, result)
	return nil
}

type validityStub struct{ fail bool }

func (s validityStub) Check(_ context.Context, _ core.Principal, ref EvidenceRef) (SourceVerdict, error) {
	if s.fail {
		return SourceVerdict{}, errors.New("source unavailable")
	}
	return SourceVerdict{Valid: true, Version: ref.SourceVersion, ContentHash: ref.ContentHash}, nil
}

type supportStub struct{ stale bool }

func (supportStub) Identity() (string, int64) { return "test-fact-checker", 1 }

func (s supportStub) CheckSupport(_ context.Context, _ core.Principal, basis VerificationBasis) (SupportVerdict, error) {
	hash, err := VerificationBasisHash(basis)
	if err != nil {
		return SupportVerdict{}, err
	}
	if s.stale {
		hash = "old-target"
	}
	outcome := "conflict"
	if string(basis.Output) == `"verified fact"` {
		outcome = "supported"
	}
	return SupportVerdict{Outcome: outcome, BasisHash: hash}, nil
}

func verificationFixture() (*VerificationService, *verificationRepoStub, VerifyRequest) {
	rule := AcceptanceRule{ID: "sources", Version: 1, Kind: "evidence", TargetKind: "slot", TargetID: "facts", Required: true, MinSources: 2}
	repo := &verificationRepoStub{basis: VerificationBasis{TaskID: "task", TargetKind: "slot", TargetID: "facts", TargetVersion: 1, ContractVersion: 1, Rule: rule, ContentHash: "h", Evidence: graphFixture().Evidence}}
	req := VerifyRequest{Principal: core.LocalAdminCLIPrincipal(), TaskID: "task", TargetKind: "slot", TargetID: "facts", TargetVersion: 1, ContractVersion: 1, RuleID: "sources", RuleVersion: 1, NodeAttemptID: "attempt"}
	return &VerificationService{Repository: repo, Schemas: tool.NewSchemaCompiler(), Sources: validityStub{}}, repo, req
}

func TestFlow_RepeatedSourceIsNotIndependentEvidence(t *testing.T) {
	service, repo, req := verificationFixture()
	repo.basis.Evidence[1].SourceUID = repo.basis.Evidence[0].SourceUID
	verdict, err := service.Verify(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "fail", verdict.Outcome, "repeated source must not count as independent evidence")
	require.Equal(t, "independent_sources_missing", verdict.Reason)
	require.Len(t, repo.saved, 1)
}

func TestFlow_RequiredSourcesCannotBeBypassedBySchemaRule(t *testing.T) {
	service, repo, req := verificationFixture()
	repo.basis.Rule.Kind = "deterministic"
	repo.basis.Rule.Schema = json.RawMessage(`{"type":"string"}`)
	repo.basis.Output = json.RawMessage(`"unsupported claim"`)
	repo.basis.RequiredSources = 3
	verdict, err := service.Verify(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "fail", verdict.Outcome, "shape validation cannot bypass required independent source count")
	require.Equal(t, "independent_sources_missing", verdict.Reason)
}

func TestFlow_VerificationBindsTargetVersion(t *testing.T) {
	service, repo, req := verificationFixture()
	req.TargetVersion++
	_, err := service.Verify(context.Background(), req)
	require.ErrorIs(t, err, ErrStateConflict, "old verification must not pass a new target version")
	require.Empty(t, repo.saved)
}

func TestFlow_ToolSuccessIsNotTaskAcceptance(t *testing.T) {
	service, repo, req := verificationFixture()
	repo.basis.Evidence = nil
	verdict, err := service.Verify(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "fail", verdict.Outcome, "tool success without evidence must not pass acceptance")
	repo.basis.Rule.Kind = "deterministic"
	repo.basis.Rule.Schema = json.RawMessage(`{"type":"integer","minimum":2}`)
	repo.basis.Output = json.RawMessage(`1`)
	verdict, err = service.Verify(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "fail", verdict.Outcome)
}

func TestFlow_RecoveryRechecksSourceAuthority(t *testing.T) {
	service, repo, req := verificationFixture()
	service.Support = supportStub{}
	repo.basis.Output = json.RawMessage(`"verified fact"`)
	verdict, err := service.Verify(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "pass", verdict.Outcome)
	service.Sources = validityStub{fail: true}
	verdict, err = service.Verify(context.Background(), req)
	require.Error(t, err)
	require.Equal(t, "inconclusive", verdict.Outcome, "source-check failure must block reuse")
}

func TestFlow_EvidenceSupportBindsContentAndSources(t *testing.T) {
	service, repo, req := verificationFixture()
	service.Support = supportStub{}
	repo.basis.Output = json.RawMessage(`"verified fact"`)
	verdict, err := service.Verify(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "pass", verdict.Outcome)
	repo.basis.Output = json.RawMessage(`"unrelated or conflicting statement"`)
	verdict, err = service.Verify(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "fail", verdict.Outcome, "valid sources must not prove unrelated target content")
	service.Support = supportStub{stale: true}
	verdict, err = service.Verify(context.Background(), req)
	require.ErrorIs(t, err, ErrStateConflict)
	require.Equal(t, "inconclusive", verdict.Outcome, "support for an old target must not transfer")
}

func TestFlow_ReadableSourcesAloneCannotVerify(t *testing.T) {
	service, _, req := verificationFixture()
	verdict, err := service.Verify(context.Background(), req)
	require.Error(t, err, "source readability alone must not prove target support")
	require.Equal(t, "inconclusive", verdict.Outcome)
}

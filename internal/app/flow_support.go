package app

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/knowledge"
)

const flowSourceValueValidator = "basalt-source-value-v1"

type flowProofViewer interface {
	SourceView(context.Context, core.Principal, knowledge.SourceViewRequest) (knowledge.SourceViewResult, error)
}

// This deterministic checker proves only exact source data, never an inferred
// business fact. The author's named rule opts into full JSON-value/verbatim-text
// equality. Free-form semantic judgments require a separately calibrated judge.
type flowSourceValueSupport struct {
	viewer  flowProofViewer
	sources flow.SourceValidity
	check   func(context.Context, core.Principal, flow.VerificationBasis, flow.EvidenceRef) error
}

func (flowSourceValueSupport) Identity() (string, int64) { return flowSourceValueValidator, 1 }

func (s flowSourceValueSupport) CheckSupport(ctx context.Context, p core.Principal, basis flow.VerificationBasis) (flow.SupportVerdict, error) {
	hash, err := flow.VerificationBasisHash(basis)
	if err != nil {
		return flow.SupportVerdict{}, err
	}
	verdict := flow.SupportVerdict{Outcome: "unverifiable", BasisHash: hash}
	if basis.Rule.ValidatorID != flowSourceValueValidator {
		return verdict, nil
	}
	if s.viewer == nil || s.sources == nil || s.check == nil {
		return verdict, flow.ErrStateConflict
	}
	output, err := flow.CanonicalJSON(ctx, basis.Output)
	if err != nil {
		return verdict, err
	}
	seen := make(map[string]bool)
	matching, conflicting := 0, 0
	for _, ref := range basis.Evidence {
		if ref.SourceKind != core.EvidenceKindKnowledge || ref.Location != "source:"+ref.SourceUID {
			return verdict, nil
		}
		if err := s.check(ctx, p, basis, ref); err != nil {
			return verdict, err
		}
		view, err := s.viewer.SourceView(ctx, p, knowledge.SourceViewRequest{Ref: knowledge.EvidenceRef{ID: ref.SourceUID, VersionID: ref.SourceVersion, ContentHash: ref.ContentHash}, Budget: 8192})
		if err != nil {
			return verdict, err
		}
		if !view.Validity.Valid || view.Superseded || view.VersionID != ref.SourceVersion || view.ContentHash != ref.ContentHash || view.Truncated || view.NextCursor != "" || view.Range.Start != 0 || view.Range.End != len([]rune(view.Text)) {
			return verdict, nil
		}
		// Recheck after the read as well: a revoked original key/closed task or
		// source must not turn a stale response into an accepted conclusion.
		if err := s.check(ctx, p, basis, ref); err != nil {
			return verdict, err
		}
		current, err := s.sources.Check(ctx, p, ref)
		if err != nil {
			return verdict, err
		}
		if !current.Valid || current.Version != ref.SourceVersion || current.ContentHash != ref.ContentHash {
			return verdict, nil
		}
		if seen[ref.SourceUID] {
			continue
		}
		seen[ref.SourceUID] = true
		source, err := flow.CanonicalJSON(ctx, []byte(view.Text))
		if err != nil {
			encoded, e := json.Marshal(view.Text)
			if e != nil {
				return verdict, e
			}
			source = encoded
		}
		if bytes.Equal(output, source) {
			matching++
		} else {
			conflicting++
		}
	}
	if conflicting > 0 {
		verdict.Outcome = "conflict"
	} else if matching >= max(1, basis.Rule.MinSources, basis.RequiredSources) {
		verdict.Outcome = "supported"
	}
	return verdict, nil
}

func (a *flowAuthority) checkSourceProof(ctx context.Context, p core.Principal, basis flow.VerificationBasis, ref flow.EvidenceRef) error {
	if a == nil || a.store == nil || ref.TaskID != basis.TaskID {
		return flow.ErrStateConflict
	}
	task, err := a.store.Read(ctx, p, basis.TaskID)
	if err != nil {
		return err
	}
	if task.Status != flow.Running || task.CloseIntent != flow.CloseNone || task.CancelRequested || task.Version.Revision != basis.TaskRevision || task.ContractVersion != basis.ContractVersion {
		return flow.ErrStateConflict
	}
	current, err := a.Current(ctx, task)
	if err != nil {
		return err
	}
	if current.CallerID != p.CallerID || current.KeyID != p.KeyID || !current.HasScope(core.ScopeKnowledgeRead) {
		return core.ErrUnauthorized
	}
	collection, err := a.store.KnowledgeEvidenceCollection(ctx, ref.SourceUID, ref.SourceVersion)
	if err != nil {
		return err
	}
	if !slices.Contains(task.Contract.Scope.CollectionIDs, collection) {
		return core.ErrUnauthorized
	}
	unknown, err := a.store.HasUnknownActions(ctx, task.ID)
	if err != nil {
		return err
	}
	if unknown {
		return flow.ErrActionUnknown
	}
	// Key and task checks are repeated around SourceView, whose existing exact
	// reader separately rechecks current ACL and active version/hash.
	return ctx.Err()
}

var _ flow.SourceSupport = flowSourceValueSupport{}

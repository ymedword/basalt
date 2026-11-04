package app

import (
	"context"
	"slices"
	"sync"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

type flowRetrievalGuard struct {
	check            func(context.Context) error
	current          func(context.Context) (core.Principal, error)
	permit           func(context.Context, string, core.BudgetTicket) error
	authz            knowledge.Authz
	principal        core.Principal
	agent            string
	collections      []string
	mu               sync.Mutex
	embeddingStarted bool
}

func (g *flowRetrievalGuard) BeforeRetrieve(ctx context.Context, agent string) ([]string, error) {
	if g == nil || g.check == nil || g.current == nil || agent != g.agent {
		return nil, core.ErrFlowDispatch
	}
	if err := g.check(ctx); err != nil {
		return nil, err
	}
	if len(g.collections) == 0 {
		return nil, nil
	}
	p, err := g.current(ctx)
	if err != nil {
		return nil, err
	}
	if p.CallerID != g.principal.CallerID || p.KeyID != g.principal.KeyID || !p.HasScope(core.ScopeKnowledgeRead) {
		return nil, core.ErrUnauthorized
	}
	if g.authz == nil {
		return nil, core.ErrFlowDispatch
	}
	scope, err := g.authz.Resolve(ctx, p, g.agent, slices.Clone(g.collections))
	if err != nil {
		return nil, err
	}
	for _, id := range scope.Collections {
		if !slices.Contains(g.collections, id) {
			return nil, core.ErrUnauthorized
		}
	}
	return slices.Clone(scope.Collections), nil
}

func (g *flowRetrievalGuard) BeforeEmbedding(ctx context.Context, auditID string, budget core.BudgetTicket) error {
	if g == nil {
		return core.ErrFlowDispatch
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.embeddingStarted {
		return core.ErrFlowDispatch
	}
	if auditID == "" || g.permit == nil {
		return core.ErrFlowDispatch
	}
	collections, err := g.BeforeRetrieve(ctx, g.agent)
	if err != nil {
		return err
	}
	if len(collections) == 0 {
		return core.ErrUnauthorized
	}
	if err := g.permit(ctx, auditID, budget); err != nil {
		return err
	}
	g.embeddingStarted = true
	return nil
}

func (g *flowRetrievalGuard) BeforeSourceRead(ctx context.Context) (core.Principal, error) {
	collections, err := g.BeforeRetrieve(ctx, g.agent)
	if err != nil {
		return core.Principal{}, err
	}
	if len(collections) == 0 {
		return core.Principal{}, core.ErrUnauthorized
	}
	p, err := g.current(ctx)
	if err != nil {
		return core.Principal{}, err
	}
	if p.CallerID != g.principal.CallerID || p.KeyID != g.principal.KeyID || !p.HasScope(core.ScopeKnowledgeRead) {
		return core.Principal{}, core.ErrUnauthorized
	}
	return p, nil
}

type flowSourceValidity struct {
	viewer *knowledge.SourceViewer
	store  *postgres.FlowStore
}

func (s flowSourceValidity) Check(ctx context.Context, p core.Principal, ref flow.EvidenceRef) (flow.SourceVerdict, error) {
	if s.viewer == nil || ref.SourceKind != core.EvidenceKindKnowledge {
		return flow.SourceVerdict{Reason: "source_kind_unsupported"}, nil
	}
	if s.store == nil || ref.TaskID == "" {
		return flow.SourceVerdict{}, flow.ErrStateConflict
	}
	task, err := s.store.Read(ctx, p, ref.TaskID)
	if err != nil {
		return flow.SourceVerdict{}, err
	}
	collection, err := s.store.KnowledgeEvidenceCollection(ctx, ref.SourceUID, ref.SourceVersion)
	if err != nil {
		return flow.SourceVerdict{}, err
	}
	if !slices.Contains(task.Contract.Scope.CollectionIDs, collection) {
		return flow.SourceVerdict{Reason: "source_outside_frozen_scope"}, nil
	}
	current, err := s.viewer.Check(ctx, p, core.EvidenceRef{Kind: ref.SourceKind, ID: ref.SourceUID, VersionID: ref.SourceVersion, ContentHash: ref.ContentHash})
	return flow.SourceVerdict{Valid: current.Valid, Reason: current.Reason, Version: current.VersionID, ContentHash: current.ContentHash}, err
}

type flowOutputChecker struct {
	store   *postgres.FlowStore
	sources flow.SourceValidity
}

func (c flowOutputChecker) CheckOutput(ctx context.Context, p core.Principal, task flow.Task, output flow.NodeOutput) error {
	if c.store == nil {
		return flow.ErrStateConflict
	}
	refs, err := c.store.OutputEvidence(ctx, p, task.ID, output)
	if err != nil {
		return err
	}
	if len(output.Claims) > 0 {
		graph, err := c.store.ReadGraph(ctx, p, task.ID)
		if err != nil {
			return err
		}
		for _, dependency := range output.Claims {
			claim, ok := flow.CurrentClaim(task.Revision, graph, dependency.ID)
			if !ok || claim.Revision != dependency.Revision {
				return flow.ErrStateConflict
			}
			for _, id := range claim.EvidenceIDs {
				for _, ref := range graph.Evidence {
					if ref.ID == id {
						refs = append(refs, ref)
					}
				}
			}
		}
	}
	return checkFlowSources(ctx, p, task, refs, c.store, c.sources)
}

func checkFlowSources(ctx context.Context, p core.Principal, task flow.Task, refs []flow.EvidenceRef, store *postgres.FlowStore, sources flow.SourceValidity) error {
	for _, ref := range refs {
		if ref.TaskID != "" && ref.TaskID != task.ID {
			return flow.ErrStateConflict
		}
		ref.TaskID = task.ID
		if sources == nil || store == nil {
			return flow.ErrStateConflict
		}
		current, err := sources.Check(ctx, p, ref)
		if err != nil {
			return err
		}
		if !current.Valid || current.Version != ref.SourceVersion || current.ContentHash != ref.ContentHash {
			return flow.ErrStateConflict
		}
		if ref.SourceKind == core.EvidenceKindKnowledge {
			collection, err := store.KnowledgeEvidenceCollection(ctx, ref.SourceUID, ref.SourceVersion)
			if err != nil {
				return err
			}
			if !slices.Contains(task.Contract.Scope.CollectionIDs, collection) {
				return core.ErrUnauthorized
			}
		}
	}
	return nil
}

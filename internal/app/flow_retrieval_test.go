package app

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

type flowQueryBudgetProbe struct {
	reserved, settled, unknown int
	reject                     error
	tokens                     int64
}

func (b *flowQueryBudgetProbe) Reserve(_ context.Context, run, action string, estimate core.UsageEstimate) (core.BudgetTicket, error) {
	b.reserved++
	return core.BudgetTicket{RunID: run, ActionID: action, Reserved: estimate}, b.reject
}

func (b *flowQueryBudgetProbe) Settle(_ context.Context, _ core.BudgetTicket, actual core.UsageActual) error {
	b.settled++
	b.tokens = actual.Tokens
	return nil
}

func (b *flowQueryBudgetProbe) KeepUnknown(context.Context, core.BudgetTicket) error {
	b.unknown++
	return nil
}

type flowQueryEmbeddingProbe struct {
	calls int
	usage int64
	after func()
}

func (e *flowQueryEmbeddingProbe) Embed(context.Context, []string) (knowledge.EmbedBatch, error) {
	e.calls++
	if e.after != nil {
		e.after()
	}
	return knowledge.EmbedBatch{Vectors: [][]float32{{1, 2}}, PromptTokens: e.usage}, nil
}

func (e *flowQueryEmbeddingProbe) EmbedFlow(ctx context.Context, texts []string, before func(context.Context, string) error) (knowledge.EmbedBatch, error) {
	if err := before(ctx, "unit-pending-id"); err != nil {
		return knowledge.EmbedBatch{}, errors.Join(errFlowEmbeddingNotIssued, err)
	}
	return e.Embed(ctx, texts)
}

func TestFlow_EmbeddingCallbackRevocationStopsSourceRead(t *testing.T) {
	for _, mode := range []string{"revoked", "scope-removed"} {
		t.Run(mode, func(t *testing.T) {
			fx := newRunSrcFixture(t)
			fx.authz.visible = map[string]bool{"frozen": true}
			fx.source.generation = 1
			p := runSrcPrincipal()
			current := p
			var keyError error
			guard := &flowRetrievalGuard{principal: p, agent: "ops", collections: []string{"frozen"}, authz: fx.authz, check: func(context.Context) error { return nil }, current: func(context.Context) (core.Principal, error) { return current, keyError }}
			guard.permit = func(context.Context, string, core.BudgetTicket) error { return nil }
			budget := &flowQueryBudgetProbe{}
			embedder := &flowQueryEmbeddingProbe{usage: 7, after: func() {
				if mode == "revoked" {
					keyError = core.ErrUnauthorized
				} else {
					current.Scopes = nil
				}
			}}
			fx.source.embedder = embedder
			req := runSrcRequest()
			req.Flow = &core.FlowRunContext{Authority: core.FlowAuthority{CollectionIDs: []string{"frozen"}}, Retrieval: guard, Budget: budget}
			_, err := fx.source.RetrieveForRun(t.Context(), req)
			require.ErrorIs(t, err, core.ErrUnauthorized)
			require.Equal(t, 1, embedder.calls)
			require.Zero(t, fx.keyword.calls)
			require.Zero(t, fx.vectors.calls)
			require.Empty(t, fx.assembly.records)
			require.Equal(t, 1, budget.settled)
			require.EqualValues(t, 7, budget.tokens, "already issued usage cannot be refunded on revocation")
		})
	}
}

func TestFlow_RetrievalCeilingAndEmbeddingBudget(t *testing.T) {
	for _, mode := range []string{"empty", "revoked", "budget", "known", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			fx := newRunSrcFixture(t)
			fx.authz.visible = map[string]bool{"frozen": true, "newly-granted": true}
			fx.source.generation = 1
			embedder := &flowQueryEmbeddingProbe{usage: 7}
			fx.source.embedder = embedder
			budget := &flowQueryBudgetProbe{}
			p := runSrcPrincipal()
			guard := &flowRetrievalGuard{principal: p, agent: "ops", collections: []string{"frozen"}, authz: fx.authz, check: func(context.Context) error { return nil }, current: func(context.Context) (core.Principal, error) { return p, nil }}
			guard.permit = func(context.Context, string, core.BudgetTicket) error { return nil }
			req := runSrcRequest()
			req.Flow = &core.FlowRunContext{Authority: core.FlowAuthority{CollectionIDs: []string{"frozen"}}, Retrieval: guard, Budget: budget}
			switch mode {
			case "empty":
				guard.collections = nil
				req.Flow.Authority.CollectionIDs = nil
			case "revoked":
				guard.current = func(context.Context) (core.Principal, error) { return core.Principal{}, core.ErrUnauthorized }
			case "budget":
				budget.reject = errors.New("ancestor token budget exhausted")
			case "unknown":
				embedder.usage = 0
			}
			_, err := fx.source.RetrieveForRun(t.Context(), req)
			if mode == "revoked" || mode == "budget" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if mode == "known" || mode == "unknown" {
				require.Equal(t, 1, embedder.calls)
				require.Equal(t, 1, budget.reserved)
				if mode == "known" {
					require.Equal(t, 1, budget.settled)
					require.EqualValues(t, 7, budget.tokens)
				} else {
					require.Equal(t, 1, budget.unknown)
					require.Zero(t, budget.settled)
				}
				require.Len(t, fx.assembly.records, 1)
				// The resolver sees only the frozen collection even though the key
				// and Agent can now see a newly granted one as well.
				require.ErrorIs(t, guard.BeforeEmbedding(t.Context(), "unit-pending-id", core.BudgetTicket{}), core.ErrFlowDispatch, "same leaf cannot issue a second query embedding")
			} else {
				require.Zero(t, embedder.calls)
				require.Zero(t, fx.keyword.calls)
			}
		})
	}
}

func TestFlow_RetrievalRejectsScopeExpansionBeforeExternalCall(t *testing.T) {
	p := runSrcPrincipal()
	guard := &flowRetrievalGuard{principal: p, agent: "ops", collections: []string{"frozen"}, check: func(context.Context) error { return nil }, current: func(context.Context) (core.Principal, error) { return p, nil }, authz: flowExpandingAuthz{}}
	_, err := guard.BeforeRetrieve(t.Context(), "ops")
	require.ErrorIs(t, err, core.ErrUnauthorized)
	p.Scopes = nil
	guard.authz = &runSrcAuthz{visible: map[string]bool{"frozen": true}}
	_, err = guard.BeforeRetrieve(t.Context(), "ops")
	require.ErrorIs(t, err, core.ErrUnauthorized)
}

type flowExpandingAuthz struct{}

func (flowExpandingAuthz) Resolve(context.Context, core.Principal, string, []string) (knowledge.Scope, error) {
	return knowledge.Scope{Collections: []string{"outside-ceiling"}}, nil
}

type flowSourceReadProbe struct {
	reader *runSrcReader
	calls  int
}

func (r *flowSourceReadProbe) LoadVersion(ctx context.Context, document, version string) (knowledge.SourceVersionRecord, error) {
	r.calls++
	return r.reader.LoadVersion(ctx, document, version)
}

func TestFlow_LaterSourceConsumersRecheckOriginalKey(t *testing.T) {
	for _, consumer := range []string{"revalidate", "answer"} {
		t.Run(consumer, func(t *testing.T) {
			fx := newRunSrcFixture(t)
			fx.authz.visible["frozen"] = true
			reader := &flowSourceReadProbe{reader: fx.reader}
			fx.source.viewer = knowledge.NewSourceViewer(knowledge.SourceViewerDeps{Reader: reader, Authz: fx.authz})
			p := runSrcPrincipal()
			guard := &flowRetrievalGuard{principal: p, agent: "ops", collections: []string{"frozen"}, authz: fx.authz, check: func(context.Context) error { return nil }, current: func(context.Context) (core.Principal, error) { return core.Principal{}, core.ErrUnauthorized }}
			f := &core.FlowRunContext{Retrieval: guard}
			chunks := []core.KnowledgeChunk{runSrcChunk("c1", "doc", "v1", "hash")}
			var err error
			if consumer == "revalidate" {
				_, err = fx.source.RevalidateChunks(t.Context(), core.KnowledgeRevalidateRequest{Principal: p, RunID: "run", Chunks: chunks, Flow: f})
			} else {
				_, err = fx.source.ResolveAnswer(t.Context(), core.KnowledgeAnswerRequest{Principal: p, RunID: "run", Chunks: chunks, Answer: "answer [c1]", Flow: f})
			}
			require.ErrorIs(t, err, core.ErrUnauthorized)
			require.Zero(t, reader.calls)
		})
	}
}

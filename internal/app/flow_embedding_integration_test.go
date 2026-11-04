//go:build integration

package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// 混合绑定工作流中，未声明知识库的叶子不继承兄弟节点的检索请求。
// A leaf without knowledge bindings must not retrieve its sibling's collections.
func TestFlow_NoKnowledgeLeafKeepsSharedRunChain(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader")
	flowEvalSeedSources(t, fx, []string{"policy"}, func(string) []byte { return []byte(`{"answer":"known source"}`) })
	fx.writeAgent(t, "reviewer")
	fx.cfg.Flows = flow.DefaultConfig()
	fx.cfg.Flows.Enabled = true
	response := core.ChatResponse{Content: `{"answer":"reviewed"}`, Usage: core.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}
	model := &scriptedProviderClient{replies: []core.ChatResponse{response, response}}
	fx.override = model
	rt := fx.assemble(t)
	rt.adminPosture.Set(core.AdminPostureToken)
	d := liveFlowDefinition(t, rt)
	view, err := rt.flows.runner.authority.(*flowAuthority).authoringAgent(t.Context(), "reviewer")
	require.NoError(t, err)
	require.True(t, view.Eligible)
	require.Empty(t, view.KnowledgeCollections)
	next := d.Nodes[0]
	next.ID, next.Agent = "review", view.Agent
	next.Inputs = []flow.InputMapping{{Target: "answer", Source: "outputs.draft.answer"}}
	d.Nodes[0].Next = "review"
	d.Nodes = append(d.Nodes, next)
	pub, err := rt.flows.publisher.Publish(t.Context(), flow.PublishRequest{Principal: core.LocalAdminCLIPrincipal(), Actor: core.Actor{Subject: "admin", Method: core.AuthMethodCLI, Source: "cli"}, OperationID: "publish-mixed-bindings", Definition: d})
	require.NoError(t, err)
	task, err := rt.flows.service.Start(t.Context(), flow.StartRequest{Principal: core.LocalAdminCLIPrincipal(), DefinitionUID: pub.DefinitionUID, IdempotencyKey: "mixed-bindings", Input: json.RawMessage(`{"answer":"input"}`)})
	require.NoError(t, err)
	require.NotEmpty(t, task.Contract.Scope.CollectionIDs, "the root must retain the reader's knowledge scope")
	rt.flows.authenticationReady()
	require.NoError(t, rt.flows.Start(t.Context()))
	require.Eventually(t, func() bool {
		task, err = rt.flows.store.ReadForWorker(t.Context(), task.ID)
		return err == nil && (task.Status.Terminal() || task.Status == flow.Blocked)
	}, 8*time.Second, 20*time.Millisecond)
	require.NoError(t, rt.flows.Close())
	require.Equal(t, flow.Succeeded, task.Status, "empty leaf binding must not fail as a denied root collection: %s", task.Reason)
	require.Len(t, model.requests(), 2)
	var runs, calls, embeddings int
	require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM runs WHERE status='completed'`).Scan(&runs))
	require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM llm_calls`).Scan(&calls))
	require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT count(*) FROM embedding_calls`).Scan(&embeddings))
	require.Equal(t, 2, runs)
	require.Equal(t, 2, calls)
	require.Zero(t, embeddings, "the fixture uses only local protocol responses")
}

// Real original embedding audit SQL; only the protocol is a local counter. This
// tests Flow's strict caller of the same auditedEmbedder, not ordinary fail-open.
func TestFlow_QueryAuditTerminalFailureRetainsBudget(t *testing.T) {
	db := pgtest.NewTestDB(t)
	_, err := db.ExecContext(t.Context(), `CREATE SEQUENCE flow_query_fault_seq; CREATE FUNCTION flow_query_terminal_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status<>'pending' THEN PERFORM nextval('flow_query_fault_seq'); RAISE EXCEPTION 'flow query terminal fault fired'; END IF; RETURN NEW; END $$; CREATE TRIGGER flow_query_fault BEFORE UPDATE ON embedding_calls FOR EACH ROW EXECUTE FUNCTION flow_query_terminal_fault()`)
	require.NoError(t, err)
	fx := newRunSrcFixture(t)
	fx.authz.visible["frozen"] = true
	fx.source.generation = 1
	p := runSrcPrincipal()
	budget := &flowQueryBudgetProbe{}
	inner := &flowQueryEmbeddingProbe{usage: 7}
	fx.source.embedder = &auditedEmbedder{inner: inner, audit: postgres.NewEmbeddingCallRepo(db, 2), model: "mock-query", logger: fx.source.logger}
	guard := &flowRetrievalGuard{principal: p, agent: "ops", collections: []string{"frozen"}, authz: fx.authz, check: func(context.Context) error { return nil }, current: func(context.Context) (core.Principal, error) { return p, nil }, permit: func(context.Context, string, core.BudgetTicket) error { return nil }}
	req := runSrcRequest()
	req.Flow = &core.FlowRunContext{Authority: core.FlowAuthority{CollectionIDs: []string{"frozen"}}, Budget: budget, Retrieval: guard}
	_, err = fx.source.RetrieveForRun(t.Context(), req)
	require.ErrorIs(t, err, errFlowEmbeddingUnconfirmed)
	require.Equal(t, 1, inner.calls)
	require.Equal(t, 1, budget.unknown)
	require.Zero(t, budget.settled)
	require.Zero(t, fx.keyword.calls)
	require.Empty(t, fx.assembly.records)
	var fired, pending bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT is_called FROM flow_query_fault_seq`).Scan(&fired))
	require.True(t, fired)
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT status='pending' FROM embedding_calls`).Scan(&pending))
	require.True(t, pending)
}

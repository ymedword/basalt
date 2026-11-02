//go:build integration

package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

func TestObservabilityProducer_IngestStages(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true})
	result := e.ingest(t)
	events, err := postgres.NewObservabilityStore(e.db).ListEvents(t.Context(), diagnostics.Query{IngestJobID: result.IngestJobID, Limit: 100})
	require.NoError(t, err)
	var stages []string
	for _, ev := range events {
		if ev.Kind == diagnostics.KindIngestStage {
			stages = append(stages, ev.StepID)
			require.Empty(t, ev.Correlation.RunID)
			require.Empty(t, ev.Attrs)
		}
	}
	require.ElementsMatch(t, []string{"parse", "chunk", "embed", "index", "done"}, stages)
}

func TestObservabilityProducer_ChunkInvalidated(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true})
	e.ingest(t)
	var deleteErr error
	e.tools.onInvoke(func() {
		deleteErr = e.state.ingestor.Delete(t.Context(), knowledge.DeleteRequest{Principal: core.LocalAdminPrincipal(), DocumentID: e.state.documentID})
	})
	e.provider.respondWithToolCalls(1)
	result := e.runOnce(t)
	require.NoError(t, deleteErr)
	require.Equal(t, core.StatusCompleted, result.Status)
	events := eventsWithReason(e.events(t, result.RunID), "chunk_invalidated")
	require.Len(t, events, 1, "only an actual narrowing emits an invalidation event")
	require.Equal(t, "retrieval", events[0].Kind)
	require.Equal(t, "trim", events[0].Decision)
	require.Equal(t, map[string]any{"count": float64(1)}, events[0].Attrs)
}

func TestObservabilityProducer_IndexSwitchAnchors(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true})
	recorder, capture := producerRecorder(t)
	adapter := &diagnosticGenerationStore{
		GenerationStore: postgres.NewIndexStore(e.db), recorder: recorder,
		correlation: diagnostics.Correlation{RequestID: "synthetic-index-request"},
	}
	manager := knowledge.NewGenerationManager(knowledge.GenerationDeps{Store: adapter, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	valid, err := manager.Validate(t.Context(), 1)
	require.NoError(t, err)
	_, err = manager.Activate(t.Context(), valid)
	require.NoError(t, err)
	require.Len(t, capture.events, 1)
	require.Equal(t, diagnostics.KindIndexSwitch, capture.events[0].Kind)
	require.Equal(t, "synthetic-index-request", capture.events[0].Correlation.RequestID)
	_, err = manager.Activate(t.Context(), valid)
	require.Error(t, err)
	require.Len(t, capture.events, 1, "failed switch must not invent a success event")
	adapter.correlation = diagnostics.Correlation{}
	second, err := manager.Begin(t.Context(), knowledge.GenerationSpec{EmbeddingModel: "synthetic-second", Dimensions: knowledge.VectorDimensions})
	require.NoError(t, err)
	_, err = adapter.ActivateGeneration(t.Context(), second.Generation, time.Now())
	require.NoError(t, err)
	require.Len(t, capture.events, 1, "unanchored startup switch must not fabricate an operation")
}

func TestKnowledgeRunE2E_FilteredCountProjectMismatch(t *testing.T) {
	obsFilteredRun(t, []string{"project"}, 2)
}

func TestKnowledgeRunE2E_FilteredCountSoftDeleted(t *testing.T) {
	obsFilteredRun(t, []string{"deleted"}, 2)
}

func TestKnowledgeRunE2E_FilteredCountInactiveVersion(t *testing.T) {
	obsFilteredRun(t, []string{"inactive"}, 1)
}

func TestKnowledgeRunE2E_FilteredCountMixed(t *testing.T) {
	// 静态夹具前提：两个被拒集合各一行，另有可见集合inactive一行；2+max(3,3)=5。
	obsFilteredRun(t, []string{"project", "deleted", "inactive"}, 5)
}

func obsFilteredRun(t *testing.T, kinds []string, want int) {
	t.Helper()
	var bindings []core.KnowledgeBinding
	for _, kind := range kinds {
		if kind != "inactive" {
			bindings = append(bindings, core.KnowledgeBinding{Name: "obs-" + kind, Access: core.KnowledgeAccessRead})
		}
	}
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true, ExtraBindings: bindings})
	// 空项目表示不限项目，必须明确约束调用方及可见集合才形成项目不匹配。
	_, err := e.db.ExecContext(t.Context(), `UPDATE callers SET project='obs-project' WHERE caller_id=$1`, runHarnessCaller)
	require.NoError(t, err)
	_, err = e.db.ExecContext(t.Context(), `UPDATE kb_collections SET project='obs-project' WHERE collection_id=$1`, knRunE2ECollectionID)
	require.NoError(t, err)
	base := e.ingest(t)
	var count int
	require.NoError(t, e.db.QueryRowContext(t.Context(), `SELECT count(*) FROM kb_chunks WHERE version_id=$1`, base.VersionID).Scan(&count))
	require.Equal(t, 1, count, "each denied fixture must have exactly one chunk")
	for _, kind := range kinds {
		obsCloneFilteredMaterial(t, e, base.VersionID, kind)
	}
	e.provider.respondWithText(knRunE2EAnswerCited)
	res := e.runOnce(t)
	require.Equal(t, core.StatusCompleted, res.Status)
	facts, err := postgres.NewObservabilityStore(e.db).AssemblyByRun(t.Context(), res.RunID)
	require.NoError(t, err)
	require.Len(t, facts, 1)
	require.Equal(t, float64(want), facts[0].Retrieval["filtered_count"], "count must come from the real authorization and SQL search paths")
	require.Equal(t, "not_distinguished", facts[0].Retrieval["filtered_reason"])
	require.Len(t, facts[0].Candidates, 1, "filtered identities must not enter the candidate evidence")
	require.Equal(t, "kept", facts[0].Candidates[0].Decision)
	raw, err := json.Marshal(facts)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "CANARY_FILTERED")
	denials := eventsWithReason(e.events(t, res.RunID), "permission_denied")
	require.Len(t, denials, 1)
	require.Equal(t, "retrieval", denials[0].Kind)
	require.Equal(t, float64(want), denials[0].Attrs["count"])
}

func obsCloneFilteredMaterial(t *testing.T, e *knRunE2E, baseVersion, kind string) {
	t.Helper()
	ctx := t.Context()
	collection := knRunE2ECollectionID
	if kind != "inactive" {
		collection = "CANARY_FILTERED_col_" + kind
		_, err := e.db.ExecContext(ctx, `INSERT INTO kb_collections(collection_id,name,project,created_at,deleted_at)
			VALUES ($1,$2,CASE WHEN $3='project' THEN 'other-project' ELSE '' END,1,CASE WHEN $3='deleted' THEN 1 ELSE NULL END)`, collection, "obs-"+kind, kind)
		require.NoError(t, err)
		_, err = e.db.ExecContext(ctx, `INSERT INTO agent_resource_bindings
			(agent_uid,agent_name,resource_kind,ref_name,resource_uid,content_hash,projected_at)
			VALUES ($1,$2,'knowledge_collection',$3,$4,'synthetic',1)`, "agent-"+runHarnessAgentName, runHarnessAgentName, "obs-"+kind, collection)
		require.NoError(t, err)
	}
	doc, version, chunk := "CANARY_FILTERED_doc_"+kind, "CANARY_FILTERED_ver_"+kind, "CANARY_FILTERED_chunk_"+kind
	_, err := e.db.ExecContext(ctx, `INSERT INTO kb_documents(document_id,collection_id,source_path,content_hash,created_at,updated_at)
		VALUES ($1,$2,$1,'synthetic',1,1)`, doc, collection)
	require.NoError(t, err)
	_, err = e.db.ExecContext(ctx, `INSERT INTO kb_document_versions(version_id,document_id,content_hash,status,parser_version,chunker_version,generation,created_at,updated_at)
		VALUES ($1,$2,'synthetic',CASE WHEN $3='inactive' THEN 'superseded' ELSE 'active' END,'synthetic','synthetic',1,1,1)`, version, doc, kind)
	require.NoError(t, err)
	_, err = e.db.ExecContext(ctx, `INSERT INTO kb_chunks(chunk_id,version_id,seq,char_start,char_end,text,content_hash,created_at)
		SELECT $1,$2,1,char_start,char_end,text,'synthetic',1 FROM kb_chunks WHERE version_id=$3`, chunk, version, baseVersion)
	require.NoError(t, err)
	_, err = e.db.ExecContext(ctx, `INSERT INTO kb_chunk_embeddings(chunk_id,generation,embedding,model,dimensions,created_at)
		SELECT $1,generation,embedding,model,dimensions,1 FROM kb_chunk_embeddings WHERE chunk_id IN
		(SELECT chunk_id FROM kb_chunks WHERE version_id=$2)`, chunk, baseVersion)
	require.NoError(t, err)
}

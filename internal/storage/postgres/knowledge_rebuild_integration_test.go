//go:build integration

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

func TestKnowledgeRebuild_UpgradePreservesExistingSnapshots(t *testing.T) {
	ctx := context.Background()
	conn := migrationConn(t)
	require.NoError(t, ensureSchema(ctx, conn, 3))
	_, err := conn.ExecContext(ctx, `
		INSERT INTO kb_collections (collection_id, name, created_at) VALUES ('c', 'manuals', 1);
		INSERT INTO kb_documents (document_id, collection_id, source_path, content_hash, created_at, updated_at)
		VALUES ('d', 'c', 'manual.md', 'hash-b', 1, 2);
		INSERT INTO kb_document_versions (version_id, document_id, content_hash, status, parser_version, chunker_version, source_text, created_at, updated_at)
		VALUES ('v1', 'd', 'hash-a', 'superseded', 'p1', 'c1', '第一版原文', 1, 1),
		       ('v2', 'd', 'hash-b', 'active', 'p1', 'c1', '第二版原文', 2, 2)`)
	require.NoError(t, err)
	require.NoError(t, ensureSchema(ctx, conn, SupportedSchemaVersion))
	var revision1, revision2 int64
	var text1, text2 string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT revision, source_text FROM kb_document_versions WHERE version_id='v1'`).Scan(&revision1, &text1))
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT revision, source_text FROM kb_document_versions WHERE version_id='v2'`).Scan(&revision2, &text2))
	require.Less(t, revision1, revision2)
	require.Equal(t, "第一版原文", text1)
	require.Equal(t, "第二版原文", text2)
	var revision3 int64
	require.NoError(t, conn.QueryRowContext(ctx, `
		INSERT INTO kb_document_versions (version_id, document_id, content_hash, status, parser_version, chunker_version, source_text, created_at, updated_at)
		VALUES ('v3', 'd', 'hash-b', 'pending', 'p1', 'c1', '第二版原文', 3, 3) RETURNING revision`).Scan(&revision3))
	require.Greater(t, revision3, revision2)
}

func TestKnowledgeRebuild_RevokedCallerCannotActivate(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	first := h.startJob(t, "job_original", "manual.md", "hash-a")
	h.publish(t, first.VersionID, "原文")
	_, err := h.db.pool.ExecContext(ctx, `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ('caller-test', 'service', '测试调用方', '', 'active', 1);
		INSERT INTO caller_keys (key_id, caller_id, key_hash, key_prefix, scopes, created_at)
		VALUES ('key-test', 'caller-test', 'hash-test', 'test', ARRAY['knowledge:write'], 1)`)
	require.NoError(t, err)
	principal := core.Principal{Kind: core.PrincipalCaller, CallerID: "caller-test", KeyID: "key-test", Scopes: []string{core.ScopeKnowledgeWrite}, Actor: core.ActorCaller}
	require.NoError(t, h.store.AuthorizeReindex(ctx, principal, first.DocumentID))
	job, err := h.store.StartJob(ctx, knowledge.StartJobRequest{
		JobID: "job_rebuild", CollectionID: h.collection, SourcePath: "manual.md", ContentHash: "hash-a",
		ParserVersion: knowledge.ParserVersion, ChunkerVersion: "md-v1", At: knNow,
		ForceNewVersion: true, ExpectedDocumentID: first.DocumentID,
	})
	require.NoError(t, err)
	refs := h.seedChunks(t, job.VersionID, "待激活")
	h.seedEmbeddings(t, refs, 1)
	require.NoError(t, h.store.MarkIndexed(ctx, job.VersionID, knNow))
	_, err = h.db.pool.ExecContext(ctx, `UPDATE caller_keys SET revoked_at = 1 WHERE key_id = 'key-test'`)
	require.NoError(t, err)
	_, err = h.store.ActivateReindex(ctx, principal, job.VersionID, 1, knNow)
	require.ErrorIs(t, err, core.ErrUnauthorized)
	var active string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `SELECT version_id FROM kb_document_versions WHERE document_id = $1 AND status = 'active'`, first.DocumentID).Scan(&active))
	require.Equal(t, first.VersionID, active)
}

func TestKnowledgeRebuild_RejectsOtherProjectBeforeReading(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	first := h.startJob(t, "job_original", "manual.md", "hash-a")
	h.publish(t, first.VersionID, "原文")
	_, err := h.db.pool.ExecContext(ctx, `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ('other-caller', 'service', '另一个项目', 'other-project', 'active', 1);
		INSERT INTO caller_keys (key_id, caller_id, key_hash, key_prefix, scopes, created_at)
		VALUES ('other-key', 'other-caller', 'other-hash', 'test', ARRAY['knowledge:write'], 1)`)
	require.NoError(t, err)
	principal := core.Principal{Kind: core.PrincipalCaller, CallerID: "other-caller", KeyID: "other-key", Scopes: []string{core.ScopeKnowledgeWrite}, Actor: core.ActorCaller}
	require.ErrorIs(t, h.store.AuthorizeReindex(ctx, principal, first.DocumentID), core.ErrForbidden)
}

func TestKnowledgeRebuild_CreatesNewVersionAndPreservesOriginal(t *testing.T) {
	ctx := context.Background()
	text := "# 故障处理\n\n先确认服务状态，再执行恢复。\n"
	h := newKnHarness(t, map[string]knowledge.DocumentSource{"manual.md": knDoc("manual.md", text)})
	first := h.ingest(t, "manual.md")
	require.Equal(t, knowledge.VersionActive, first.Status)
	second, err := h.ingestor.Reindex(ctx, knowledge.ReindexRequest{Principal: h.principal, DocumentID: first.DocumentID})
	require.NoError(t, err)
	require.Equal(t, knowledge.VersionActive, second.Status)
	require.False(t, second.Deduplicated)
	require.Equal(t, first.DocumentID, second.DocumentID)
	require.NotEqual(t, first.VersionID, second.VersionID)
	require.NotEmpty(t, second.IngestJobID)

	old, err := h.store.LoadVersion(ctx, first.DocumentID, first.VersionID)
	require.NoError(t, err)
	require.Equal(t, knowledge.VersionSuperseded, old.Status)
	require.Equal(t, text, old.Text)
	newVersion, err := h.store.LoadVersion(ctx, first.DocumentID, second.VersionID)
	require.NoError(t, err)
	require.Equal(t, knowledge.VersionActive, newVersion.Status)
	require.Equal(t, text, newVersion.Text)
}

func TestKnowledgeRebuild_RejectsTamperedHistoricalSnapshot(t *testing.T) {
	ctx := context.Background()
	text := "原始流程：先检查服务，再恢复。"
	h := newKnHarness(t, map[string]knowledge.DocumentSource{"manual.md": knDoc("manual.md", text)})
	first := h.ingest(t, "manual.md")
	second, err := h.ingestor.Reindex(ctx, knowledge.ReindexRequest{Principal: h.principal, DocumentID: first.DocumentID})
	require.NoError(t, err)
	require.Equal(t, knowledge.VersionActive, second.Status)
	_, err = h.db.pool.ExecContext(ctx, `UPDATE kb_document_versions SET source_text = $2 WHERE version_id = $1`, first.VersionID, "被篡改的内容")
	require.NoError(t, err)
	old, err := h.store.LoadVersion(ctx, first.DocumentID, first.VersionID)
	require.NoError(t, err)
	require.Equal(t, knowledge.VersionSuperseded, old.Status)
	require.Empty(t, old.Text, "摘要不符的快照不可作为历史原文返回")
}

func TestKnowledgeRebuild_OlderJobCannotReplaceNewerVersion(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	first := h.startJob(t, "job_original", "manual.md", "hash-a")
	h.publish(t, first.VersionID, "原文")
	start := func(jobID string) knowledge.JobRecord {
		job, err := h.store.StartJob(ctx, knowledge.StartJobRequest{
			JobID: jobID, CollectionID: h.collection, SourcePath: "manual.md", ContentHash: "hash-a",
			ParserVersion: knowledge.ParserVersion, ChunkerVersion: "md-v1", At: knNow,
			ForceNewVersion: true, ExpectedDocumentID: first.DocumentID,
		})
		require.NoError(t, err)
		return job
	}
	older := start("job_older")
	newer := start("job_newer")
	refs := h.seedChunks(t, older.VersionID, "旧作业")
	h.seedEmbeddings(t, refs, 1)
	require.NoError(t, h.store.MarkIndexed(ctx, older.VersionID, knNow))
	h.publish(t, newer.VersionID, "新作业")
	_, err := h.store.ActivateVersion(ctx, older.VersionID, 1, knNow)
	require.ErrorIs(t, err, errKnowledgeStateConflict)
	var active string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `SELECT version_id FROM kb_document_versions WHERE document_id = $1 AND status = 'active'`, first.DocumentID).Scan(&active))
	require.Equal(t, newer.VersionID, active)
}

func TestKnowledgeRebuild_ConcurrentJobsGetDistinctRevisions(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	first := h.startJob(t, "job_original", "manual.md", "hash-a")
	h.publish(t, first.VersionID, "原文")
	type outcome struct {
		job knowledge.JobRecord
		err error
	}
	results := make(chan outcome, 2)
	for _, jobID := range []string{"job_a", "job_b"} {
		go func(id string) {
			job, err := h.store.StartJob(ctx, knowledge.StartJobRequest{
				JobID: id, CollectionID: h.collection, SourcePath: "manual.md", ContentHash: "hash-a",
				ParserVersion: knowledge.ParserVersion, ChunkerVersion: "md-v1", At: knNow,
				ForceNewVersion: true, ExpectedDocumentID: first.DocumentID,
			})
			results <- outcome{job: job, err: err}
		}(jobID)
	}
	one, two := <-results, <-results
	require.NoError(t, one.err)
	require.NoError(t, two.err)
	require.NotEqual(t, one.job.VersionID, two.job.VersionID)
	var revision1, revision2 int64
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `SELECT revision FROM kb_document_versions WHERE version_id = $1`, one.job.VersionID).Scan(&revision1))
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `SELECT revision FROM kb_document_versions WHERE version_id = $1`, two.job.VersionID).Scan(&revision2))
	require.NotEqual(t, revision1, revision2)
}

func TestKnowledgeRebuild_DeletionInvalidatesInFlightVersion(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	first := h.startJob(t, "job_original", "manual.md", "hash-a")
	h.publish(t, first.VersionID, "原文")
	rebuild, err := h.store.StartJob(ctx, knowledge.StartJobRequest{
		JobID: "job_rebuild", CollectionID: h.collection, SourcePath: "manual.md", ContentHash: "hash-a",
		ParserVersion: knowledge.ParserVersion, ChunkerVersion: "md-v1", At: knNow,
		ForceNewVersion: true, ExpectedDocumentID: first.DocumentID,
	})
	require.NoError(t, err)
	require.NoError(t, h.store.SoftDeleteDocument(ctx, first.DocumentID, knNow))
	require.ErrorIs(t, h.store.MarkIndexed(ctx, rebuild.VersionID, knNow), errKnowledgeStateConflict)
	require.ErrorIs(t, h.store.FailVersion(ctx, rebuild.VersionID, knowledge.ErrorKindVerifyFailed, knNow), errKnowledgeStateConflict)
	_, err = h.store.StartJob(ctx, knowledge.StartJobRequest{
		JobID: "job_after_delete", CollectionID: h.collection, SourcePath: "manual.md", ContentHash: "hash-a",
		ParserVersion: knowledge.ParserVersion, ChunkerVersion: "md-v1", At: knNow,
		ForceNewVersion: true, ExpectedDocumentID: first.DocumentID,
	})
	require.ErrorIs(t, err, knowledge.ErrNotFound)
	doc, err := h.store.Document(ctx, first.DocumentID)
	require.NoError(t, err)
	require.True(t, doc.Deleted)
}

func TestKnowledgeRebuild_FailureKeepsOldVersion(t *testing.T) {
	ctx := context.Background()
	embedder := &knToggleEmbedder{}
	h := newKnHarnessWith(t, map[string]knowledge.DocumentSource{
		"manual.md": knDoc("manual.md", "# 记录\n\n始终保留这个内容。\n"),
	}, embedder)
	first := h.ingest(t, "manual.md")
	embedder.failNow()
	result, err := h.ingestor.Reindex(ctx, knowledge.ReindexRequest{Principal: h.principal, DocumentID: first.DocumentID})
	require.NoError(t, err)
	require.Equal(t, knowledge.VersionFailed, result.Status)
	require.NotEqual(t, first.VersionID, result.VersionID)
	old, err := h.store.LoadVersion(ctx, first.DocumentID, first.VersionID)
	require.NoError(t, err)
	require.Equal(t, knowledge.VersionActive, old.Status)
	require.Contains(t, old.Text, "始终保留这个内容")
}

func TestKnowledgeRebuild_CollectionDeletionInvalidatesInFlightVersion(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	first := h.startJob(t, "job_original", "manual.md", "hash-a")
	h.publish(t, first.VersionID, "原文")
	rebuild, err := h.store.StartJob(ctx, knowledge.StartJobRequest{
		JobID: "job_rebuild", CollectionID: h.collection, SourcePath: "manual.md", ContentHash: "hash-a",
		ParserVersion: knowledge.ParserVersion, ChunkerVersion: "md-v1", At: knNow,
		ForceNewVersion: true, ExpectedDocumentID: first.DocumentID,
	})
	require.NoError(t, err)
	require.NoError(t, h.store.SoftDeleteCollection(ctx, h.collection, knNow))
	require.ErrorIs(t, h.store.MarkIndexed(ctx, rebuild.VersionID, knNow), errKnowledgeStateConflict)
	var status string
	require.NoError(t, h.db.pool.QueryRowContext(ctx, `SELECT status FROM kb_document_versions WHERE version_id = $1`, rebuild.VersionID).Scan(&status))
	require.Equal(t, string(knowledge.VersionRetired), status)
}

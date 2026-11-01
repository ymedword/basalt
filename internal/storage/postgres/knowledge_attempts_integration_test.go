//go:build integration

package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/knowledge"
)

func TestKnowledgeIngestAttempts_OrderTruncationAndDeletion(t *testing.T) {
	ctx := context.Background()
	h := newKnHarness(t, nil)
	first := h.startJob(t, "job_initial", "guide.md", "hash")
	for i := 1; i <= 101; i++ {
		_, err := h.db.pool.ExecContext(ctx, `INSERT INTO kb_ingest_jobs
			(ingest_job_id, document_id, version_id, stage, attempt, status, started_at)
			VALUES ($1, $2, $3, 'parse', $4, 'failed', $5)`,
			fmt.Sprintf("job-%03d", i), first.DocumentID, first.VersionID, i+1, knNow.UnixMilli()+int64(i))
		require.NoError(t, err)
	}
	store := NewKnowledgeCatalogStore(h.db)
	rows, truncated, err := store.ListIngestAttempts(ctx, first.DocumentID)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, rows, 100)
	require.Equal(t, "job-101", rows[0].JobID)
	require.Equal(t, "job-002", rows[99].JobID)
	require.Equal(t, "failed", rows[0].Status)
	require.NoError(t, h.store.SoftDeleteDocument(ctx, first.DocumentID, knNow))
	_, _, err = store.ListIngestAttempts(ctx, first.DocumentID)
	require.ErrorIs(t, err, knowledge.ErrNotFound)
}

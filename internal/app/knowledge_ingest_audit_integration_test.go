//go:build integration

package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

type testAuditedIngest struct {
	calls  int
	result knowledge.IngestResult
	err    error
}

func (f *testAuditedIngest) Ingest(context.Context, knowledge.IngestRequest) (knowledge.IngestResult, error) {
	return knowledge.IngestResult{}, nil
}
func (f *testAuditedIngest) Delete(context.Context, knowledge.DeleteRequest) error { return nil }
func (f *testAuditedIngest) Reindex(context.Context, knowledge.ReindexRequest) (knowledge.IngestResult, error) {
	f.calls++
	return f.result, f.err
}

type testReindexGuard struct{ err error }

func (g testReindexGuard) AuthorizeReindex(context.Context, core.Principal, string) error {
	return g.err
}

func (g testReindexGuard) ActivateReindex(context.Context, core.Principal, string, int, time.Time) (string, error) {
	return "", g.err
}

func TestAuditedKnowledgeReindex_PrewriteAndOutcome(t *testing.T) {
	ctx := context.Background()
	db := pgtest.NewTestDB(t)
	ops := postgres.NewManagementStore(db)
	logger, _ := newCapturedLogger()
	inner := &testAuditedIngest{result: knowledge.IngestResult{DocumentID: "doc", VersionID: "ver", IngestJobID: "job", Status: knowledge.VersionActive}}
	a := &auditedKnowledgeIngest{inner: inner, guard: testReindexGuard{}, operations: ops, now: time.Now, logger: logger}
	req := knowledge.ReindexRequest{
		Principal: core.LocalAdminPrincipal(), DocumentID: "doc",
		AuditActor: core.Actor{Subject: "local_trust", Method: core.AuthMethodLocalTrust, Source: "127.0.0.1"},
	}
	_, err := a.Reindex(ctx, req)
	require.NoError(t, err)
	require.Equal(t, 1, inner.calls)
	rows, err := ops.ListOperations(ctx, core.OperationFilter{Action: "knowledge_document.reindex"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationSuccess, rows[0].Status)
	var detail map[string]any
	require.NoError(t, json.Unmarshal(rows[0].DetailJSON, &detail))
	require.Equal(t, "job", detail["ingest_job_id"])
	a.operations = failingBeginOperations{OperationStore: ops, err: errors.New("audit unavailable")}
	_, err = a.Reindex(ctx, req)
	require.Error(t, err)
	require.Equal(t, 1, inner.calls, "审计预写失败不得开始读取或处理")
	a.operations = ops
	inner.result.Status = knowledge.VersionFailed
	_, err = a.Reindex(ctx, req)
	require.NoError(t, err)
	rows, err = ops.ListOperations(ctx, core.OperationFilter{Action: "knowledge_document.reindex"})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	statuses := map[core.OperationStatus]bool{}
	for _, row := range rows {
		statuses[row.Status] = true
	}
	require.True(t, statuses[core.OperationSuccess])
	require.True(t, statuses[core.OperationFailed])
}

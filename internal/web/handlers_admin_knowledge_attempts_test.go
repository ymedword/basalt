package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeKnowledgeAttempts struct{ calls int }

func (f *fakeKnowledgeAttempts) ListIngestAttempts(_ context.Context, id string) ([]KnowledgeIngestAttempt, bool, error) {
	f.calls++
	return []KnowledgeIngestAttempt{{JobID: "job", VersionID: "version", Attempt: 2, Stage: "embed", Status: "failed", ErrorKind: "embed_failed"}}, true, nil
}

func TestAdminKnowledgeAttempts_RequiresAdminAndReportsTruncation(t *testing.T) {
	fake := &fakeKnowledgeAttempts{}
	s := newTestServer(t, func(o *Options) { o.KnowledgeIngestHistory = fake; o.AdminAuth = testAdminAuth(testToken) })
	path := "/api/v1/admin/knowledge/documents/doc/ingest-attempts"
	require.Equal(t, http.StatusUnauthorized, serveAdmin(s, adminRequest(http.MethodGet, path, "")).Code)
	require.Zero(t, fake.calls)
	res := serveAdmin(s, withBearer(adminRequest(http.MethodGet, path, ""), testToken))
	require.Equal(t, http.StatusOK, res.Code)
	require.Contains(t, res.Body.String(), `"truncated":true`)
	require.Contains(t, res.Body.String(), `"attempt":2`)
	require.Equal(t, 1, fake.calls)
}

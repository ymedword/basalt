package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
)

type fakeKnowledgeUpload struct {
	calls         int
	name, content string
	err           error
}

func (f *fakeKnowledgeUpload) Upload(_ context.Context, _ core.Actor, name, content string) (string, error) {
	f.calls++
	f.name, f.content = name, content
	return "uploads/" + name, f.err
}

func TestKnowledgeUpload_RequiresAdminAndRejectsConflict(t *testing.T) {
	f := &fakeKnowledgeUpload{}
	s := newTestServer(t, func(o *Options) { o.KnowledgeUpload = f; o.AdminAuth = testAdminAuth(testToken) })
	path := "/api/v1/admin/knowledge/uploads"
	response := serveAdmin(s, adminRequest(http.MethodPost, path, `{"name":"a.md","content":"正文"}`))
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Zero(t, f.calls)
	response = serveAdmin(s, withBearer(adminRequest(http.MethodPost, path, `{"name":"a.md"}`), testToken))
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Zero(t, f.calls)
	response = serveAdmin(s, withBearer(adminRequest(http.MethodPost, path, `{"name":"a.md","content":"正文"}`), testToken))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "正文", f.content)
	f.err = core.ErrConflict
	response = serveAdmin(s, withBearer(adminRequest(http.MethodPost, path, `{"name":"a.md","content":"new"}`), testToken))
	require.Equal(t, http.StatusConflict, response.Code)
}

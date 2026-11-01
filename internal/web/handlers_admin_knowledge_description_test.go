package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
)

type fakeKnowledgeDescription struct {
	calls       int
	id          string
	description string
	expected    int64
	err         error
}

func (f *fakeKnowledgeDescription) UpdateDescription(_ context.Context, _ core.Actor, id, description string, expected int64) (int64, error) {
	f.calls++
	f.id, f.description, f.expected = id, description, expected
	return expected + 1, f.err
}

func TestAdminKnowledgeDescription_RequiresAuthenticationAndRevision(t *testing.T) {
	fake := &fakeKnowledgeDescription{}
	s := newTestServer(t, func(o *Options) { o.KnowledgeDescription = fake; o.AdminAuth = testAdminAuth(testToken) })
	path := "/api/v1/admin/knowledge/collections/col_test/description"
	noAuth := adminRequest(http.MethodPut, path, `{"description":"x","expected_revision":1}`)
	response := serveAdmin(s, noAuth)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	require.Zero(t, fake.calls)
	bad := serveAdmin(s, withBearer(adminRequest(http.MethodPut, path, `{"description":"x"}`), testToken))
	require.Equal(t, http.StatusBadRequest, bad.Code)
	require.Zero(t, fake.calls)
	ok := serveAdmin(s, withBearer(adminRequest(http.MethodPut, path, `{"description":"使用说明","expected_revision":1}`), testToken))
	require.Equal(t, http.StatusOK, ok.Code)
	require.Equal(t, "col_test", fake.id)
	require.Equal(t, "使用说明", fake.description)
	require.EqualValues(t, 1, fake.expected)
	fake.err = core.ErrPreconditionFailed
	stale := serveAdmin(s, withBearer(adminRequest(http.MethodPut, path, `{"description":"草稿","expected_revision":1}`), testToken))
	require.Equal(t, http.StatusPreconditionFailed, stale.Code)
}

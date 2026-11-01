package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
)

type skillEditReaderFixture struct {
	*fakeResourceAdminService
	reads int
}

func TestSkillEditDetail_RejectsCallerKey(t *testing.T) {
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"caller-secret": callerPrincipal("caller", core.ScopeSessionsRead),
	}}
	reader := &skillEditReaderFixture{fakeResourceAdminService: &fakeResourceAdminService{}}
	s := newTestServer(t, func(o *Options) {
		o.AdminAuth = businessGuard(t, keys)
		o.ResourceAdminService = reader
	})
	req := withBearer(adminRequest(http.MethodGet, "/api/v1/admin/skills/alpha/edit", ""), "caller-secret")
	resp := serveAdmin(s, req)
	require.Equal(t, http.StatusForbidden, resp.Code)
	require.Zero(t, reader.reads)
	require.NotContains(t, resp.Body.String(), "private skill content")
}

func (f *skillEditReaderFixture) ReadSkillForEdit(_ context.Context, name string) (SkillDetailView, error) {
	f.reads++
	return SkillDetailView{
		SkillView: SkillView{Name: name, ETag: `"published-etag"`},
		Files:     []SkillFileView{{Path: "SKILL.md", Content: "private skill content"}},
	}, nil
}

func TestSkillEditDetail_RequiresAdminAndReturnsFiles(t *testing.T) {
	reader := &skillEditReaderFixture{fakeResourceAdminService: &fakeResourceAdminService{}}
	s := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.ResourceAdminService = reader
	})
	path := "/api/v1/admin/skills/alpha/edit"
	unauth := serveAdminTable(s, adminRequest(http.MethodGet, path, ""))
	require.Equal(t, http.StatusUnauthorized, unauth.Code)
	require.Zero(t, reader.reads)
	require.NotContains(t, unauth.Body.String(), "private skill content")

	auth := serveAdminTable(s, withBearer(adminRequest(http.MethodGet, path, ""), testToken))
	require.Equal(t, http.StatusOK, auth.Code)
	require.Equal(t, 1, reader.reads)
	require.Equal(t, `"published-etag"`, auth.Header().Get("ETag"))
	require.Contains(t, auth.Body.String(), "private skill content")

	summary := serveAdminTable(s, withBearer(adminRequest(http.MethodGet, path+"?summary=true", ""), testToken))
	require.Equal(t, http.StatusOK, summary.Code)
	require.Equal(t, 2, reader.reads)
	require.NotContains(t, summary.Body.String(), "private skill content")
	require.NotContains(t, summary.Body.String(), `"files"`)
}

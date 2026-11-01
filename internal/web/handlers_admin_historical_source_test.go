package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/knowledge"
)

func TestAdminHistoricalSource_RequiresAdminAndPreservesValidity(t *testing.T) {
	ref := knowledge.EvidenceRef{ID: "doc", VersionID: "old", ContentHash: "hash"}
	fake := &fakeKnowledgeSource{result: knowledge.SourceViewResult{
		Ref: ref, VersionID: "old", ContentHash: "hash", Text: "历史原文", Superseded: true,
		Validity: knowledge.ValidityResult{Reason: knowledge.ReasonVersionChanged},
	}}
	s := newTestServer(t, func(o *Options) { o.KnowledgeSource = fake; o.AdminAuth = testAdminAuth(testToken) })
	path := "/api/v1/admin/knowledge/historical-source"
	body := `{"ref":{"id":"doc","version_id":"old","content_hash":"hash"}}`
	req := adminRequest(http.MethodPost, path, body)
	require.Equal(t, http.StatusUnauthorized, serveAdmin(s, req).Code)
	require.Zero(t, fake.calls)
	read := func() *httptest.ResponseRecorder {
		return serveAdmin(s, withBearer(adminRequest(http.MethodPost, path, body), testToken))
	}
	ok := read()
	require.Equal(t, http.StatusOK, ok.Code)
	require.Contains(t, ok.Body.String(), "历史原文")
	require.Contains(t, ok.Body.String(), `"historical":true`)
	require.Equal(t, 2048, fake.req.Budget)
	fake.result.Validity.Reason = knowledge.ReasonDeleted
	denied := read()
	require.Equal(t, http.StatusConflict, denied.Code)
	require.NotContains(t, denied.Body.String(), "历史原文")
	fake.result.Validity.Reason = knowledge.ReasonVersionChanged
	fake.result.ContentHash = "different"
	badHash := read()
	require.Equal(t, http.StatusConflict, badHash.Code)
	require.NotContains(t, badHash.Body.String(), "历史原文")

	// **当前有效版本**必须被本端点拒绝：管理员想读"当时版本"，拿到的若还是现行版本，
	// 就等于把"历史核对"做成了"看现在"——这正是本端点与 `POST /sources/view` 的分工
	// 被抹掉的样子。逆改点：删掉 `!result.Superseded || result.Validity.Valid` → 红。
	fake.result.ContentHash = "hash"
	fake.result.Validity = knowledge.ValidityResult{Valid: true}
	fake.result.Superseded = false
	current := read()
	require.Equal(t, http.StatusConflict, current.Code)
	require.NotContains(t, current.Body.String(), "历史原文")

	// 请求缺 `content_hash` → 400 且**零调用**：三重一致性校验没有比对基准，不能退化成
	// "按版本号信"。契约里该字段由本端点的 ref 单独声明为必填。
	fake.result.Superseded = true
	fake.result.Validity = knowledge.ValidityResult{Reason: knowledge.ReasonVersionChanged}
	fake.calls = 0
	noHash := serveAdmin(s, withBearer(adminRequest(http.MethodPost, path,
		`{"ref":{"id":"doc","version_id":"old"}}`), testToken))
	require.Equal(t, http.StatusBadRequest, noHash.Code)
	require.Zero(t, fake.calls, "缺 content_hash 时不得触达端口")
}

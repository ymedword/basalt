package web

import (
	"net/http"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// The history endpoint is deliberately separate from the normal citation view:
// a superseded version is historical evidence, not a currently valid citation.
// SourceView still checks current collection permissions and deletion on every page.
func (s *Server) handleAdminHistoricalKnowledgeSource(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.KnowledgeSource == nil {
		s.writeAdminPortMissing(w, "KnowledgeSource", "POST "+r.URL.Path)
		return
	}
	var body struct {
		Ref    *evidenceRefWrite `json:"ref"`
		Cursor *string           `json:"cursor"`
	}
	if err := s.decodeAdminWriteBody(w, r, &body, "请输入历史版本引用"); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if body.Ref == nil || body.Ref.ID == "" || body.Ref.VersionID == "" || body.Ref.ContentHash == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "历史引用必须包含文档、版本与内容哈希")
		return
	}
	req := knowledge.SourceViewRequest{Ref: knowledge.EvidenceRef{ID: body.Ref.ID, VersionID: body.Ref.VersionID, ContentHash: body.Ref.ContentHash}, Budget: 2048}
	if body.Cursor != nil {
		req.Cursor = *body.Cursor
	}
	result, err := s.opts.KnowledgeSource.SourceView(r.Context(), core.LocalAdminPrincipal(), req)
	if err != nil {
		s.writeKnowledgeError(w, err, "历史来源读取失败", "")
		return
	}
	if !result.Superseded || result.Validity.Valid || result.Validity.Reason != knowledge.ReasonVersionChanged ||
		result.VersionID != body.Ref.VersionID || result.Ref.ID != body.Ref.ID ||
		result.ContentHash == "" || result.ContentHash != body.Ref.ContentHash {
		s.writeError(w, http.StatusConflict, codeConflict, "历史版本不可读取或内容校验失败")
		return
	}
	s.writeOK(w, struct {
		VersionID   string `json:"version_id"`
		ContentHash string `json:"content_hash"`
		Text        string `json:"text"`
		Truncated   bool   `json:"truncated"`
		NextCursor  string `json:"next_cursor,omitempty"`
		Historical  bool   `json:"historical"`
	}{result.VersionID, result.ContentHash, result.Text, result.Truncated, result.NextCursor, true})
}

package web

import (
	"errors"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

func (s *Server) handleAdminUpdateKnowledgeDescription(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.KnowledgeDescription == nil {
		s.writeAdminPortMissing(w, "KnowledgeDescription", "PUT "+r.URL.Path)
		return
	}
	var body struct {
		Description      *string `json:"description"`
		ExpectedRevision *int64  `json:"expected_revision"`
	}
	if err := s.decodeAdminWriteBody(w, r, &body, "请输入简介与版本"); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if body.Description == nil || body.ExpectedRevision == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "请输入简介与版本")
		return
	}
	revision, err := s.opts.KnowledgeDescription.UpdateDescription(r.Context(), actor, r.PathValue("id"), *body.Description, *body.ExpectedRevision)
	if err != nil {
		switch {
		case errors.Is(err, core.ErrPreconditionFailed):
			s.writeError(w, http.StatusPreconditionFailed, "PRECONDITION_FAILED", "简介已被修改，请刷新后重新编辑")
		case errors.Is(err, knowledge.ErrNotFound):
			s.writeError(w, http.StatusNotFound, codeNotFound, "集合不存在或已删除")
		default:
			var invalid *core.InvalidInputError
			if errors.As(err, &invalid) {
				s.writeError(w, http.StatusBadRequest, codeBadRequest, invalid.Error())
			} else {
				s.writeError(w, http.StatusInternalServerError, "INTERNAL", "更新简介失败")
			}
		}
		return
	}
	s.writeOK(w, struct {
		Revision int64 `json:"description_revision"`
	}{revision})
}

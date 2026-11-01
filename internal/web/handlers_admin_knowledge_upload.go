package web

import (
	"errors"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

func (s *Server) handleAdminKnowledgeUpload(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.KnowledgeUpload == nil {
		s.writeAdminPortMissing(w, "KnowledgeUpload", "POST "+r.URL.Path)
		return
	}
	var body struct {
		Name    *string `json:"name"`
		Content *string `json:"content"`
	}
	if err := s.decodeAdminWriteBody(w, r, &body, "请输入文件名和内容"); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if body.Name == nil || body.Content == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "请输入文件名和内容")
		return
	}
	path, err := s.opts.KnowledgeUpload.Upload(r.Context(), actor, *body.Name, *body.Content)
	if err != nil {
		var invalid *core.InvalidInputError
		switch {
		case errors.As(err, &invalid):
			s.writeError(w, http.StatusBadRequest, codeBadRequest, invalid.Error())
		case errors.Is(err, core.ErrConflict):
			s.writeError(w, http.StatusConflict, "CONFLICT", "同名文件已存在，请更换文件名")
		default:
			s.writeError(w, http.StatusInternalServerError, "INTERNAL", "保存文件失败")
		}
		return
	}
	s.writeOK(w, struct {
		Path string `json:"path"`
	}{path})
}

package web

import (
	"net/http"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func (s *Server) flowAuthoring(w http.ResponseWriter) (FlowAuthoringBackend, bool) {
	b, ok := s.opts.Flows.(FlowAuthoringBackend)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	}
	return b, ok
}

func (s *Server) handleAdminFlowAgents(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	b, ok := s.flowAuthoring(w)
	if !ok {
		return
	}
	cursor, limit, ok := s.flowPage(w, r)
	if !ok {
		return
	}
	if len(cursor) > 128 {
		s.writeFlowError(w, flow.ErrDefinitionInvalid)
		return
	}
	page, err := b.AuthoringAgents(r.Context(), actor, cursor, limit)
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, page)
}

func (s *Server) handleAdminFlowAuthorVersion(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	b, ok := s.flowAuthoring(w)
	if !ok {
		return
	}
	text := r.PathValue("version")
	version, err := strconv.ParseInt(text, 10, 64)
	if err != nil || version < 1 || strconv.FormatInt(version, 10) != text || len(r.PathValue("uid")) > 128 {
		s.writeFlowError(w, flow.ErrDefinitionInvalid)
		return
	}
	v, err := b.AuthoringDefinition(r.Context(), actor, flow.DefinitionRef{UID: r.PathValue("uid"), Version: version})
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, v)
}

package web

import (
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

func (s *Server) handleAdminKnowledgeIngestAttempts(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.KnowledgeIngestHistory == nil {
		s.writeAdminPortMissing(w, "KnowledgeIngestHistory", "GET "+r.URL.Path)
		return
	}
	items, truncated, err := s.opts.KnowledgeIngestHistory.ListIngestAttempts(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeKnowledgeError(w, err, "读取处理记录失败", "文档不存在或已删除")
		return
	}
	type item struct {
		JobID      string `json:"job_id"`
		VersionID  string `json:"version_id"`
		Attempt    int    `json:"attempt"`
		Stage      string `json:"stage"`
		Status     string `json:"status"`
		ErrorKind  string `json:"error_kind,omitempty"`
		StartedAt  string `json:"started_at"`
		FinishedAt string `json:"finished_at"`
	}
	output := make([]item, 0, len(items))
	for _, row := range items {
		output = append(output, item{row.JobID, row.VersionID, row.Attempt, row.Stage, row.Status, row.ErrorKind, formatTime(row.StartedAt), formatTime(row.FinishedAt)})
	}
	s.writeOK(w, struct {
		Items     []item `json:"items"`
		Truncated bool   `json:"truncated"`
	}{output, truncated})
}

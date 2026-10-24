package web

// 本文件实现 Agent 归档与恢复的三个端点（颗粒度文档 §3.1.9 / §3.7.1）：
// `DELETE /agents/{name}`（归档）、`GET /archived-agents`（归档列表）与
// `POST /archived-agents/{archive_id}/restore`（恢复）。
// handler 只做静态解码 → 委托端口 → 投影视图：注销定时、移出注册、等待在途运行、
// 回滚矩阵与归档目录改名全部留在适配器（app 层）内，本层**不得**自己读盘、补写
// 第二套编排或做乐观更新（约束 31 / 32 / 36）。
// 恢复请求体按契约可选：请求体缺省（或空白）等价于 `new_name` 为空 = 恢复原目录名；
// 一旦给了请求体就按写面口径严格解码（上限 / 未知字段 / 尾随内容都 400）。
// This file implements the three archive endpoints. Handlers decode, delegate and
// project; unregistering schedules, draining runs, the rollback matrix and the
// directory rename all stay in the adapter. An absent restore body means "restore to
// the original name"; a present body is decoded under the strict write rules.

import (
	"errors"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// 本层自产的固定文案：解码器与适配器的原文一律不外传（约束 43）。
// Fixed public texts raised by this layer; adapter wording never leaves it.
const (
	// messageArchiveRestoreBody 是恢复请求体不合法时的固定文案。
	messageArchiveRestoreBody = "请求体不是合法的 Agent 恢复请求"
	// messageArchiveNotFoundPrefix 点名查不到的归档标识（契约 404 要求点名）。
	messageArchiveNotFoundPrefix = "未找到归档记录："
)

// handleAdminArchiveAgent 处理 `DELETE /agents/{name}`：归档成功回 200 与归档记录；
// 在途运行等待超时由适配器返回 `*core.AgentBusyError`，这里映射 409 AGENT_BUSY，
// `data.run_ids` 由白名单投影出站（与 US-11 的取消端点同一形状）。
// handleAdminArchiveAgent answers 200 with the archive row; a drained-run timeout maps
// to 409 AGENT_BUSY with data.run_ids.
func (s *Server) handleAdminArchiveAgent(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.AgentAdminService == nil {
		s.writeAdminPortMissing(w, "AgentAdminService", "DELETE /agents/{name}")
		return
	}
	record, err := s.opts.AgentAdminService.Archive(r.Context(), actor, name)
	if err != nil {
		s.writeAgentAdminError(w, err, name, "归档")
		return
	}
	s.writeOK(w, agentArchiveView(record))
}

// handleAdminArchivedAgents 处理 `GET /archived-agents`：只读列表，顺序由适配器
// 保持；空列表输出 `[]`（契约 required 数组，不得序列化成 null）。
// handleAdminArchivedAgents renders the read-only archive list; an empty list is [].
func (s *Server) handleAdminArchivedAgents(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.AgentAdminService == nil {
		s.writeAdminPortMissing(w, "AgentAdminService", "GET /archived-agents")
		return
	}
	records, err := s.opts.AgentAdminService.Archived(r.Context())
	if err != nil {
		s.writeAgentAdminError(w, err, "", "列出归档记录")
		return
	}
	s.writeOK(w, agentArchiveViews(records))
}

// handleAdminRestoreAgent 处理 `POST /archived-agents/{archive_id}/restore`：请求体
// 可选（契约 required: false），`new_name` 留空 = 原名恢复。原名与异名两条路径、
// 条件迁移与 generation 递增都是适配器的编排；归档标识查不到由适配器用
// `core.ErrResourceNotFound` 表达，这里映射 404 并点名归档标识。
// handleAdminRestoreAgent accepts an optional body; a missing archive maps to a 404
// that names the archive id.
func (s *Server) handleAdminRestoreAgent(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	archiveID := r.PathValue("archive_id")
	if s.opts.AgentAdminService == nil {
		s.writeAdminPortMissing(w, "AgentAdminService", "POST /archived-agents/{archive_id}/restore")
		return
	}
	var body AgentRestoreWrite
	if err := s.decodeOptionalAdminWriteBody(w, r, &body, messageArchiveRestoreBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	set, err := s.opts.AgentAdminService.Restore(r.Context(), actor, archiveID, body.NewName)
	if err != nil {
		// 归档记录缺失不是 Agent 名问题，404 文案必须点名归档标识而非 Agent。
		// A missing archive row is named by archive id, not by agent name.
		if errors.Is(err, core.ErrResourceNotFound) {
			s.writeError(w, http.StatusNotFound, codeNotFound, messageArchiveNotFoundPrefix+archiveID)
			return
		}
		s.writeAgentAdminError(w, err, archiveID, "恢复")
		return
	}
	s.writeOK(w, agentFilesView(set.Files, set.ETag))
}

package web

// 本文件实现结构化配置读端点（颗粒度文档 R4.3 / 契约 `GET /agents/{name}/config`）：
// 一次读取给出结构化字段值、**一个**与 `GET /agents/{name}/content` 完全一致的强
// `content_etag`，以及每个引用的解析状态（绑定 UID、名称、墓碑、资格、凭据状态）。
// handler 只做「缺端口失败关闭 → 委托适配器 → 投影」：ETag 由适配器在**同一次读取**
// 里从原始字节算出，本层不重算、不归一化，也不引入 `config_etag`——表单保存与原文
// 保存共用同一个条件写基准（卡 R4.3「读投影与写语义」）。
// This file implements the structured config endpoint: one strong content_etag shared
// with /content plus the per-reference resolution. The handler never recomputes the
// ETag and never introduces a second one.

import (
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// handleGetAgentConfig 读取结构化配置。缺失端口时按固定 500 失败关闭（与四个写面
// 端口同一纪律）：**不得**返回空视图冒充"配置为空"。错误映射复用内容读写的出口
// ——404 / 400 / 绑定专用码 / 固定 500 的口径只在一处。
// handleGetAgentConfig fails closed without its port and reuses the content error
// mapping so the status and code table stays in one place.
func (s *Server) handleGetAgentConfig(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	name := r.PathValue("name")
	if s.opts.AgentConfigService == nil {
		s.logger().Error("装配缺少 AgentConfig 端口", "agent", name)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	view, err := s.opts.AgentConfigService.Config(r.Context(), name)
	if err != nil {
		s.writeAgentContentError(w, err, name, "读取")
		return
	}
	// 响应头与 `data.content_etag` 是同一个值，且与 `/content` 的 ETag 逐字相同。
	// The header and data.content_etag carry the same value as /content's ETag.
	w.Header().Set("ETag", view.ContentETag)
	s.writeOK(w, view)
}

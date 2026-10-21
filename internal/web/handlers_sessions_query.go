package web

// 本文件是会话列表的只读查询端点（模块卡 §3.1.26 / 约束 21）：只接受 `agent` 与
// `status` 两个过滤维，排序完全由存储层固定（`last_active_at DESC, session_id`）——
// handler **不**排序、**不**接受排序参数，也不对结果做二次重排；两处排序口径必然
// 漂移，"固定排序"就名存实亡。非法 `status` 在触达仓储之前返回 400。
// The console session list: two filter dimensions only, ordering fixed by storage,
// and an illegal status is refused before any repository call.
// 注：`ListSessions` 是受认证保护的路由，actor 由认证闸门以参数传入；本端点只读，
// 不消费 actor，也不写管理操作记录（约束 9 / §3.1）。
// The route is protected; the actor arrives as an explicit parameter and is unused by
// this read-only endpoint, which writes no management-operation record.

import (
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// handleListSessions 处理 `GET /api/v1/sessions`（契约 openapi.yaml）：
// 无参数 = 全量（含归档），`status` 取值只允许两档。
// handleListSessions serves the console session list; an omitted status lists everything.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	q := r.URL.Query()
	filter := core.SessionFilter{Agent: q.Get("agent")}
	switch status := q.Get("status"); status {
	case "":
		// 不过滤：与命令面的全量口径一致（含归档行，各自带 status）。
	case statusActive, statusArchived:
		filter.Status = status
	default:
		s.writeError(w, http.StatusBadRequest, codeBadRequest,
			"status 只接受 active 或 archived")
		return
	}

	records, err := s.opts.Sessions.ListSessions(r.Context(), filter)
	if err != nil {
		// 仓储错误只进 ERROR 日志，响应固定文案（约束 23 的日志面）。
		s.logger().Error("列出会话失败", "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	views := make([]SessionSummaryView, 0, len(records))
	for _, rec := range records {
		views = append(views, sessionSummaryView(rec))
	}
	s.writeOK(w, views)
}

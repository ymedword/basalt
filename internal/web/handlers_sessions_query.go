package web

// 本文件是历史回查三条只读端点的 handler（US-16 §3.1.10 / T056）：
//
//	GET /api/v1/sessions
//	GET /api/v1/history/search
//	GET /api/v1/sessions/{session_id}/messages/{message_id}
//
// 四条硬约束：
//
//  1. **归属过滤是强制项**：调用方身份只能看自己的行，内置管理员跨调用方（含迁移
//     遗留的空归属）。会话列表按当前身份的 `sessions.caller_id` 收窄；检索与精确
//     读取把 Principal 原样交给领域层，由它结算范围——web **不**另写一套过滤。
//  2. **跨调用方一律 404，不是 403**（C-28）：403 会泄露"这条消息 / 这个会话存在"。
//     精确读取的 404 文案点名消息标识；403 只留给"缺 scope"与"要点名他人 caller_id"。
//  3. **检索命中只回指针**：片段不是引用依据，正文只能走精确读取。
//  4. **排序 / 窗口由存储层固定**：handler 不排序、不接受排序参数、不二次重排——
//     两处排序口径必然漂移，"固定排序"就名存实亡。
//
// 读取面**不写**管理操作审计：`docs/contracts/run-and-errors.md` §9.3 明文规定
// "只读浏览、列表、校验不写管理操作审计"，失败面已由 WARN / ERROR 日志承担
// （可观测性双轨中的日志轨）。
// The three read-only history endpoints. Ownership filtering is enforced, cross-caller
// reads answer 404 (never 403), hits stay pointers, and ordering belongs to storage.
// Read endpoints write no management-operation audit (§9.3).

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/history"
	"github.com/ymedword/basalt/internal/knowledge"
)

// maxHistorySearchLimit 是契约 `limit` 的上限（minimum 1 / maximum 200 / default 50）。
// web 层复刻这一对边界只为在触达端口之前返回 400；领域层的 `normalizeLimit` 会再判
// 一次（失败关闭），两处口径以契约为准。
// maxHistorySearchLimit mirrors the contract ceiling; the domain re-checks it.
const maxHistorySearchLimit = 200

// 历史端点固定文案（契约 §8：400 只说明"请求形状非法"，不回显过滤规则细节）。
// Fixed history copy; a denial never explains which rule matched.
const (
	messageHistoryQueryRequired = "q 必填且不能为空"
	messageHistoryLimitRange    = "limit 必须是 1 到 200 之间的整数"
)

// handleListSessions 处理 `GET /api/v1/sessions`（契约 openapi.yaml）：
// 无参数 = 当前身份可见的全部会话（含归档），`status` 取值只允许两档。
//
// 归属口径（T056 从管理面翻为业务面）：调用方**强制**按 `sessions.caller_id` 收窄，
// 契约没有 `caller_id` 参数——"看谁的"只由身份决定，不由请求决定；内置管理员跨
// 调用方，迁移遗留的空归属行因此只对管理员可见。非法 `status` 在触达仓储之前 400。
// The console list narrows a caller to its own rows (the contract has no caller_id
// parameter); the admin spans callers, so legacy ownerless rows stay admin-only.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request, principal core.Principal) {
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
	if principal.Kind != core.PrincipalLocalAdmin {
		if principal.CallerID == "" {
			// 没有归属就无法收窄；退化成"全量"是越权，必须拒绝而不是放行。
			// An ownerless caller cannot be narrowed; listing everything is a leak.
			s.writeError(w, http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
			return
		}
		filter.CallerID = principal.CallerID
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

// handleHistorySearch 处理 `GET /history/search`：`sessions:read`，计 `IngressRead`。
// 命中只回指针与片段；`caller_id` 参数交给领域层结算——调用方传他人 `caller_id`
// 是 403，管理员可跨调用方。形状错误（缺 q / limit 越界 / 时间非 RFC3339）在触达
// 端口之前 400；领域层的 `ErrInvalidQuery`（含倒置时间窗）同样映射 400。
// handleHistorySearch validates the wire shape first, then lets the domain resolve the
// caller filter; a caller naming someone else's caller_id is forbidden by the domain.
func (s *Server) handleHistorySearch(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.HistorySearch == nil {
		s.writeHistoryPortMissing(w, "HistorySearch", "GET /history/search")
		return
	}
	q := r.URL.Query()
	keywords := q.Get("q")
	if strings.TrimSpace(keywords) == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageHistoryQueryRequired)
		return
	}
	query := history.HistoryQuery{
		Keywords:  keywords,
		Agent:     q.Get("agent"),
		SessionID: q.Get("session_id"),
		CallerID:  q.Get("caller_id"),
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxHistorySearchLimit {
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageHistoryLimitRange)
			return
		}
		query.Limit = limit
	}
	var ok bool
	if query.From, ok = s.queryTime(w, r, "from"); !ok {
		return
	}
	if query.To, ok = s.queryTime(w, r, "to"); !ok {
		return
	}

	result, err := s.opts.HistorySearch.Search(r.Context(), principal, query)
	if err != nil {
		s.writeHistorySearchError(w, err, "历史检索失败")
		return
	}
	s.writeOK(w, historySearchResultView(result))
}

// handleReadMessage 处理 `GET /sessions/{session_id}/messages/{message_id}`：精确读取
// 原文，`sessions:read`，计 `IngressRead`。跨调用方 404（不是 403）；被保留策略删除
// 410 `GONE`；无状态临时标识 `msg/<run>/<pos>` 在 `messages` 表没有行，同样 404
// （分类在领域层、访问存储之前完成）。消息携带的引用按**当次**身份逐条重查并标注。
// handleReadMessage loads the full text and annotates each evidence ref with a fresh
// verdict; cross-caller reads are 404, retention deletes are 410.
func (s *Server) handleReadMessage(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	if s.opts.HistoryExactRead == nil {
		s.writeHistoryPortMissing(w, "HistoryExactRead",
			"GET /sessions/{session_id}/messages/{message_id}")
		return
	}
	view, err := s.opts.HistoryExactRead.Read(r.Context(), principal, history.MessageRef{
		MessageID: r.PathValue("message_id"),
		SessionID: r.PathValue("session_id"),
	})
	if err != nil {
		s.writeHistoryReadError(w, err, "精确读取失败", r.PathValue("message_id"))
		return
	}
	refs, ok := s.messageEvidenceRefs(w, r, principal, view.EvidenceRefs)
	if !ok {
		return
	}
	s.writeOK(w, messageDetailView(view, refs))
}

// messageEvidenceRefs 逐条重查引用有效性并合成 `{ref, validity}`（C-26 / C-10）。
//
// 三条口径：
//   - **不做无需的判定**：消息没有引用就不调用端口，也不让缺端口变成读取失败；
//     有引用而端口缺失则是装配缺陷，固定 500（不得静默丢标注）。
//   - **无权判定是结论不是错误**：调用方能读消息但不一定有 `knowledge:read`，此时
//     `Check` 报越权——按 `revoked` 标注（"这份来源对你不可用"），整条读取照常返回。
//     `knowledge.Validity.Check` 本身也把集合 ACL 拒绝收敛成 `revoked` 结论。
//   - **存储违约失败关闭**：判定端口报错说明当前状态无法确认，**不得**写成"有效"或
//     "失效"——整条读取 500，原因只进 ERROR 日志。
//
// The annotation never runs when there is nothing to annotate, treats a scope denial as
// a revoked verdict, and fails the whole read closed when the port itself errors.
func (s *Server) messageEvidenceRefs(
	w http.ResponseWriter, r *http.Request, principal core.Principal, refs []core.EvidenceRef,
) ([]MessageEvidenceRefView, bool) {
	if len(refs) == 0 {
		// 没有引用：不带字段（空数组会被读成"带了一组空引用"）。契约该字段可缺省。
		return nil, true
	}
	if s.opts.EvidenceValidity == nil {
		s.writeHistoryPortMissing(w, "EvidenceValidity",
			"GET /sessions/{session_id}/messages/{message_id}")
		return nil, false
	}
	out := make([]MessageEvidenceRefView, 0, len(refs))
	for _, ref := range refs {
		item := MessageEvidenceRefView{
			Ref: EvidenceRefView{
				ID:          ref.ID,
				VersionID:   ref.VersionID,
				ContentHash: ref.ContentHash,
			},
		}
		result, err := s.opts.EvidenceValidity.Check(r.Context(), principal, ref)
		switch {
		case err == nil:
			item.Validity = ValidityView{Valid: result.Valid, Reason: result.Reason}
		case errors.Is(err, context.Canceled):
			// 客户端已放弃：不再写响应。
			s.logger().Warn("消息引用判定已取消")
			return nil, false
		case errors.Is(err, core.ErrForbidden):
			item.Validity = ValidityView{Reason: knowledge.ReasonRevoked}
		default:
			s.logger().Error("消息引用有效性判定失败", "error", err)
			s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
			return nil, false
		}
		out = append(out, item)
	}
	return out, true
}

// queryTime 解析一个可选的 RFC3339 时间参数：缺省为零值（不限），格式非法 400。
// 写响应返回 false——调用方必须以它为准提前返回，**不得**拿零值继续查询。
// queryTime parses an optional RFC3339 parameter; it writes the 400 itself.
func (s *Server) queryTime(w http.ResponseWriter, r *http.Request, name string) (time.Time, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, true
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest,
			name+" 必须是 RFC3339 时间")
		return time.Time{}, false
	}
	return at, true
}

// writeHistoryPortMissing 是历史端点缺端口的唯一出口：装配错误记 ERROR、回固定
// 500；**不得**用空命中或空引用冒充合法空状态（与知识端点同口径）。
// A missing history port is an assembly error: ERROR log plus a fixed 500.
func (s *Server) writeHistoryPortMissing(w http.ResponseWriter, port, endpoint string) {
	s.logger().Error("装配缺少历史端口", "port", port, "endpoint", endpoint)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

// writeHistorySearchError 把检索失败按哨兵映射为固定响应（只认 `errors.Is`）：
// 未授权 401、越权 403、形状非法 400、三个调用方 429，其余（含装配不可用）500。
// 契约给该端点补了显式 500（R66）：检索不可用**不得**降级成 200 + 空命中，那会让
// 调用方以为"查过了没有"。
// writeHistorySearchError maps the search sentinels onto fixed responses; an
// unclassifiable failure is a 500, never an empty 200.
func (s *Server) writeHistorySearchError(w http.ResponseWriter, err error, action string) {
	switch {
	case errors.Is(err, context.Canceled):
		// 客户端已放弃：不再写响应（写已断开的连接只会得到 broken pipe）。
		s.logger().Warn("历史检索已取消", "action", action)
	case errors.Is(err, core.ErrUnauthorized):
		s.writeError(w, http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
	case errors.Is(err, history.ErrForbidden):
		s.writeError(w, http.StatusForbidden, codeForbidden, messageScopeDenied)
	case errors.Is(err, history.ErrInvalidQuery):
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "历史检索参数非法")
	case errors.Is(err, callers.ErrRateLimited):
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(ingressRetryAfter), 10))
		s.writeError(w, http.StatusTooManyRequests, codeCallerRateLimited,
			errorCodeMessage(codeCallerRateLimited, ""))
	case errors.Is(err, callers.ErrConcurrencyLimited):
		s.writeError(w, http.StatusTooManyRequests, codeCallerConcurrencyLimited,
			errorCodeMessage(codeCallerConcurrencyLimited, ""))
	case errors.Is(err, callers.ErrQuotaExhausted):
		s.writeQuotaExhausted(w, err)
	default:
		s.logger().Error("历史检索失败", "action", action, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	}
}

// writeHistoryReadError 把精确读取失败按哨兵映射为固定响应：未授权 401、越权 403、
// 不存在 / 跨调用方 404（点名消息标识）、保留策略删除 410 `GONE`，其余 500。
// 契约该端点**没有** 400：不是主键的标识在领域层就是"没有这条消息"，不得回 400
// 告诉调用方"这个标识格式不对"——那等于承认标识空间有内部结构。
// writeHistoryReadError maps the exact-read sentinels; the endpoint has no 400 because
// a non-key id is simply not-found, not a malformed request.
func (s *Server) writeHistoryReadError(w http.ResponseWriter, err error, action, messageID string) {
	switch {
	case errors.Is(err, context.Canceled):
		s.logger().Warn("消息精确读取已取消", "action", action)
	case errors.Is(err, core.ErrUnauthorized):
		s.writeError(w, http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
	case errors.Is(err, history.ErrForbidden):
		s.writeError(w, http.StatusForbidden, codeForbidden, messageScopeDenied)
	case errors.Is(err, history.ErrNotFound):
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到消息："+messageID)
	case errors.Is(err, history.ErrDeleted):
		s.writeError(w, http.StatusGone, codeGone, errorCodeMessage(codeGone, messageID))
	default:
		s.logger().Error("消息精确读取失败", "action", action, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	}
}

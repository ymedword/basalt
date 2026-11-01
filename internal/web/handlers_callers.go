package web

// 本文件是调用方管理面的七个端点（US-16 / T026；契约 `/callers` 组）：
// 列 / 建调用方、签发 / 吊销 Key、查周期用量、核对预留、清零 overage。
// 三条口径：
//
//  1. **本层只做形状**——请求体解码、时刻格式与"过期时刻必须在未来"这类纯请求
//     校验在这里；scope 词表、归属、状态机与审计预写全在 `internal/callers`。
//  2. **失败分类只认哨兵**——`errors.Is` / `errors.As` 映射状态码，**不**比对
//     错误文本；底层原因只进日志。
//  3. **明文只出一次**——签发响应的 `key` 字段是唯一载体；本文件不打印、不记
//     审计、不写日志。
//
// This file holds the seven caller-admin endpoints. It validates request shape only;
// scopes, ownership, state machines and audit pre-writes stay in internal/callers.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// 固定请求校验文案：全部由本层产出，不含底层错误文本。
// Fixed request-shape messages produced by this layer only.
const (
	messageCallerCreateBody       = "请求体不是合法的调用方创建请求"
	messageCallerKeyIssueBody     = "请求体不是合法的 Key 签发请求"
	messageCallerKeyRevokeBody    = "请求体不是合法的 Key 吊销请求"
	messageCallerReconcileBody    = "请求体不是合法的预留核对请求"
	messageCallerClearOverageBody = "请求体不是合法的 overage 清零请求"
	messageCallerExpiresAtShape   = "expires_at 必须是 RFC3339 时刻"
	messageCallerExpiresAtPast    = "expires_at 必须是未来时刻"
	messageCallerPeriodStartShape = "period_start 必须是 RFC3339 时刻"
	messageCallerNotFound         = "未找到调用方：%s"
	messageCallerKeyNotFound      = "未找到 Key：%s"
	messageCallerReservationGone  = "未找到预留：%s"
	messageCallerConflict         = "调用方已存在（标识或项目内名称重复）"
	messageCallerSettled          = "预留已结算，不能二次核对"
	messageCallerForbidden        = "无权执行该调用方管理操作"
)

// handleAdminListCallers 处理 `GET /callers`：固定按 `caller_id` 升序（排序在存储层），
// 内置 `local-admin` 恒在；读路径不写管理操作行。
// handleAdminListCallers never writes an operation row.
func (s *Server) handleAdminListCallers(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.Callers == nil {
		s.writeAdminPortMissing(w, "Callers", "GET /callers")
		return
	}
	records, err := s.opts.Callers.ListCallers(r.Context())
	if err != nil {
		s.writeCallerAdminError(w, err, "列调用方记录失败", "")
		return
	}
	s.writeOK(w, callerSummaryViews(records))
}

// handleAdminCreateCaller 处理 `POST /callers`：`caller_id` 缺省由服务端生成，
// `limits` 缺省**不写**限额行（缺配置即拒绝，**不**补默认数字）；重名与主键冲突
// 由领域按 `core.ErrConflict` 报出，这里映射 409。
// handleAdminCreateCaller forwards an absent limits block verbatim: no row is written.
func (s *Server) handleAdminCreateCaller(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.Callers == nil {
		s.writeAdminPortMissing(w, "Callers", "POST /callers")
		return
	}
	var body CallerCreateWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageCallerCreateBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	record, err := s.opts.Callers.CreateCaller(r.Context(), actor, callers.CreateCallerRequest{
		CallerID: body.CallerID,
		Name:     body.Name,
		Project:  body.Project,
		Limit:    body.Limits.limit(),
	})
	if err != nil {
		s.writeCallerAdminError(w, err, "创建调用方失败", "")
		return
	}
	s.writeOK(w, callerSummaryView(record))
}

// handleAdminIssueCallerKey 处理 `POST /callers/{id}/keys`：先确认调用方存在
// （领域在预写前查，不存在即 404 且**不留** `pending`），再解析可选 `expires_at`
// ——格式非法或已过期按 400 拒绝，**不**签出一把出生即失效的 Key。
// handleAdminIssueCallerKey rejects a past expires_at before any key row exists.
func (s *Server) handleAdminIssueCallerKey(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	callerID := r.PathValue("id")
	if s.opts.Callers == nil {
		s.writeAdminPortMissing(w, "Callers", "POST /callers/{id}/keys")
		return
	}
	var body CallerKeyIssueWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageCallerKeyIssueBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	expiresAt, err := s.parseKeyExpiry(body.ExpiresAt)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	issued, err := s.opts.Callers.IssueKey(r.Context(), actor, callers.IssueRequest{
		CallerID:  callerID,
		Scopes:    body.Scopes,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		s.writeCallerAdminError(w, err, "签发 Key 失败",
			fmt.Sprintf(messageCallerNotFound, callerID))
		return
	}
	s.writeOK(w, issuedKeyView(issued))
}

// handleAdminRevokeCallerKey 处理 `POST /callers/{id}/keys/{key_id}/revoke`：请求体
// 可整体省略（`reason` 只进管理审计）；重复吊销幂等，不改动任何定时任务的
// `authorizing_key_id`——受影响任务在下次触发时按 `owner_unauthorized` fail-closed。
// handleAdminRevokeCallerKey accepts an empty body; revocation never rebinds tasks.
func (s *Server) handleAdminRevokeCallerKey(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	callerID := r.PathValue("id")
	keyID := r.PathValue("key_id")
	if s.opts.Callers == nil {
		s.writeAdminPortMissing(w, "Callers", "POST /callers/{id}/keys/{key_id}/revoke")
		return
	}
	var body CallerKeyRevokeWrite
	if err := s.decodeOptionalAdminWriteBody(w, r, &body, messageCallerKeyRevokeBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	record, err := s.opts.Callers.RevokeKey(r.Context(), actor, callers.RevokeRequest{
		CallerID: callerID,
		KeyID:    keyID,
		Reason:   body.Reason,
	})
	if err != nil {
		s.writeCallerAdminError(w, err, "吊销 Key 失败",
			fmt.Sprintf(messageCallerKeyNotFound, keyID))
		return
	}
	s.writeOK(w, callerKeySummaryView(record))
}

// handleAdminCallerUsage 处理 `GET /callers/{id}/usage`：`period_start` 缺省为当前
// 周期，给了任一时刻就折算到该周期的起点（折算在存储层按调用方时区完成）。
// 调用方不存在 404；没有限额行**不是** 404——返回零限额并照实报用量。
// handleAdminCallerUsage: a missing limit row is not a 404, it is zero limits.
func (s *Server) handleAdminCallerUsage(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	callerID := r.PathValue("id")
	if s.opts.Callers == nil {
		s.writeAdminPortMissing(w, "Callers", "GET /callers/{id}/usage")
		return
	}
	periodStart, err := parseCallerPeriodStart(r.URL.Query().Get("period_start"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	usage, err := s.opts.Callers.Usage(r.Context(), callerID, periodStart)
	if err != nil {
		s.writeCallerAdminError(w, err, "查询调用方用量失败",
			fmt.Sprintf(messageCallerNotFound, callerID))
		return
	}
	s.writeOK(w, callerUsageView(usage))
}

// handleAdminReconcileCallerReservation 处理
// `POST /callers/{id}/reservations/{reservation_id}/reconcile`：把结局未知的
// `pending` 收敛为 `committed`（给 `actual`）或 `released`（必填理由）。归属不符
// 一律 404（不泄露预留是否存在），已终态 409（不得二次结算）。
// handleAdminReconcileCallerReservation: an ownership mismatch is a plain 404.
func (s *Server) handleAdminReconcileCallerReservation(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	callerID := r.PathValue("id")
	reservationID := r.PathValue("reservation_id")
	if s.opts.Callers == nil {
		s.writeAdminPortMissing(w, "Callers", "POST /callers/{id}/reservations/{reservation_id}/reconcile")
		return
	}
	var body CallerReconcileWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageCallerReconcileBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	settlement, err := s.opts.Callers.ReconcileReservation(r.Context(), actor, callers.ReconcileRequest{
		CallerID:      callerID,
		ReservationID: reservationID,
		Outcome:       callers.ReconcileOutcome(body.Outcome),
		Actual:        body.Actual,
		Reason:        body.Reason,
	})
	if err != nil {
		s.writeCallerAdminError(w, err, "核对预留失败",
			fmt.Sprintf(messageCallerReservationGone, reservationID))
		return
	}
	s.writeOK(w, reservationView(settlement))
}

// handleAdminClearCallerOverage 处理 `POST /callers/{id}/usage/clear-overage`：
// `reason` 必填并进管理审计；只清零 `overage`，**不改** `used` / `reserved`，
// 也不删除任何预留行。返回清零后的周期用量视图。
// handleAdminClearCallerOverage zeroes overage only and returns the fresh usage view.
func (s *Server) handleAdminClearCallerOverage(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	callerID := r.PathValue("id")
	if s.opts.Callers == nil {
		s.writeAdminPortMissing(w, "Callers", "POST /callers/{id}/usage/clear-overage")
		return
	}
	var body CallerClearOverageWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageCallerClearOverageBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	periodStart, err := parseCallerPeriodInstant(body.PeriodStart)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	usage, err := s.opts.Callers.ClearOverage(r.Context(), actor, callers.ClearOverageRequest{
		CallerID:    callerID,
		PeriodStart: periodStart,
		Reason:      body.Reason,
	})
	if err != nil {
		s.writeCallerAdminError(w, err, "清零超限失败",
			fmt.Sprintf(messageCallerNotFound, callerID))
		return
	}
	s.writeOK(w, callerUsageView(usage))
}

// parseKeyExpiry 解析可选的 `expires_at`：空串表示不过期；格式非法或早于当前
// 时刻都按 400 拒绝。判定用注入时钟（`s.now`），测试可固定。
// parseKeyExpiry rejects a malformed or already-past expiry using the injected clock.
func (s *Server) parseKeyExpiry(raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, errors.New(messageCallerExpiresAtShape)
	}
	if !parsed.After(s.now()) {
		return nil, errors.New(messageCallerExpiresAtPast)
	}
	utc := parsed.UTC()
	return &utc, nil
}

// parseCallerPeriodStart 解析 `GET /callers/{id}/usage` 的 `period_start` 查询参数：
// 空串表示当前周期（0）；给了时刻就转成 Unix 毫秒，周期起点折算由存储层完成。
// parseCallerPeriodStart maps an empty query value to the current period.
func parseCallerPeriodStart(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	at, err := parseCallerPeriodInstant(raw)
	if err != nil {
		return 0, err
	}
	return at.UnixMilli(), nil
}

// parseCallerPeriodInstant 是两处周期参数共用的解析：空串 = 零值（当前周期）；
// 非空则必须是 RFC3339。**不**接受裸数字或本地时间猜测。
// parseCallerPeriodInstant accepts RFC3339 or the empty string only.
func parseCallerPeriodInstant(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errors.New(messageCallerPeriodStartShape)
	}
	return parsed, nil
}

// writeCallerAdminError 把调用方管理面的失败按**哨兵类型**映射为固定响应：
//   - `*callers.InvalidRequestError` → 400 BAD_REQUEST（Reason 是本包自产中文）
//   - `callers.ErrInvalidScope` → 400 INVALID_SCOPE（契约固定码）
//   - `callers.ErrForbidden` → 403 FORBIDDEN
//   - `callers.ErrNotFound` → 404 NOT_FOUND（`missing` 由调用点点名目标）
//   - `callers.ErrReservationSettled` / `core.ErrConflict` → 409 CONFLICT
//   - `context.DeadlineExceeded` → 504 TIMEOUT
//   - 其余 → 500 INTERNAL，底层原因只进日志
//
// 每个分支都**不**回显底层错误文本：领域哨兵带的是英文包前缀，库错误还可能含
// SQL 与参数值，两者都只进 ERROR 日志。
// writeCallerAdminError maps sentinels to fixed responses; raw error text stays in
// the ERROR log because library errors can carry SQL fragments and arguments.
func (s *Server) writeCallerAdminError(w http.ResponseWriter, err error, action, missing string) {
	var invalid *callers.InvalidRequestError
	switch {
	case errors.As(err, &invalid):
		s.writeError(w, http.StatusBadRequest, codeBadRequest, invalid.Reason)
	case errors.Is(err, callers.ErrInvalidScope):
		s.writeError(w, http.StatusBadRequest, codeInvalidScope, errorCodeMessage(codeInvalidScope, ""))
	case errors.Is(err, callers.ErrForbidden):
		s.writeError(w, http.StatusForbidden, codeForbidden, messageCallerForbidden)
	case errors.Is(err, callers.ErrNotFound):
		// 没有点名文案说明调用点漏传（编程错误）：按装配缺陷记 ERROR 回 500，
		// **不**把 404 与"服务器内部错误"拼成一条自相矛盾的响应。
		if missing == "" {
			s.logger().Error("调用方管理目标未找到但缺少点名文案", "action", action, "error", err)
			s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
			return
		}
		s.writeError(w, http.StatusNotFound, codeNotFound, missing)
	case errors.Is(err, callers.ErrReservationSettled):
		s.writeError(w, http.StatusConflict, codeConflict, messageCallerSettled)
	case errors.Is(err, core.ErrConflict):
		s.writeError(w, http.StatusConflict, codeConflict, messageCallerConflict)
	case errors.Is(err, context.DeadlineExceeded):
		s.writeError(w, http.StatusGatewayTimeout, codeTimeout, messageAdminTimeout)
	default:
		s.logger().Error("调用方管理操作失败", "action", action, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	}
}

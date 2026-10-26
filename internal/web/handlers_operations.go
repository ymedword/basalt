package web

// 本文件是管理操作审计的只读 handler（模块卡 §3.1.6）：`GET /operations` 与
// `GET /operations/{id}`。排序与窗口在仓储 SQL（`created_at DESC, operation_id DESC`，
// 缺省 50 / 上限 200）；`detail_json` 在投影层经白名单脱敏后才出站。只读端点**不**
// 写 `management_operations`（约束 12）——审计噪声会掩盖真正的变更。
// Read-only operation-audit handlers: fixed ordering stays in SQL, detail_json is
// whitelisted on projection, and reads never write audit rows.

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
)

// 操作列表的固定窗口（契约 /operations 的 limit 参数：缺省 50、上限 200）。
// Fixed operation-list window: default 50, maximum 200.
const (
	defaultOperationLimit = 50
	maxOperationLimit     = 200
)

// messageOperationUnknown 是 unknown 详情的信封提示，逐字取自契约 /operations/{id}
// 的说明：崩溃窗口内结果未查证，只能按独立操作契约恢复或补偿——**不得**盲目重做。
// The unknown hint mirrors the contract description verbatim in substance.
const messageOperationUnknown = "操作结果未查证，需按独立操作契约恢复或补偿，不得盲目重做"

// 参数非法时的固定 400 文案（模块卡 §3.1.6：status / target_kind / limit 校验同
// parseRunFilter 口径）；不回显请求值。
// Fixed 400 texts for the three validated parameters; request values are not echoed.
const (
	badOperationStatusMessage = "status 取值非法（可选：pending / success / failed / unknown）"
	badOperationKindMessage   = "target_kind 取值非法（可选：agent / schedule / model_connection / mcp_server / skill / notify_channel / network_allowlist / network_denylist / execution_policy / network_exception / system）"
	badOperationLimitMessage  = "limit 必须是 1–200 之间的整数"
)

// parseOperationFilter 解析 GET /operations 的过滤参数与固定窗口：只认 action /
// target_kind / target_id / status / limit，未知参数忽略；非法 status、非法
// target_kind、非正整数或超上限的 limit 一律返回错误（调用方映射 400 BAD_REQUEST）。
// action / target_id 是自由文本，空值表示不过滤。
// parseOperationFilter accepts exactly the five documented parameters; only status,
// target_kind and limit have closed value sets.
func parseOperationFilter(r *http.Request) (core.OperationFilter, error) {
	q := r.URL.Query()
	filter := core.OperationFilter{
		Action:   q.Get("action"),
		TargetID: q.Get("target_id"),
		Limit:    defaultOperationLimit,
	}
	if q.Has("target_kind") {
		kind := q.Get("target_kind")
		if !validOperationTargetKind(kind) {
			return core.OperationFilter{}, errors.New(badOperationKindMessage)
		}
		filter.TargetKind = kind
	}
	if q.Has("status") {
		status := q.Get("status")
		if !validOperationStatus(status) {
			return core.OperationFilter{}, errors.New(badOperationStatusMessage)
		}
		filter.Status = status
	}
	if q.Has("limit") {
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > maxOperationLimit {
			return core.OperationFilter{}, errors.New(badOperationLimitMessage)
		}
		filter.Limit = limit
	}
	return filter, nil
}

// validOperationStatus 只接受契约 OperationView.status 的四个枚举值。
// validOperationStatus accepts exactly the four contract status literals.
func validOperationStatus(status string) bool {
	switch core.OperationStatus(status) {
	case core.OperationPending, core.OperationSuccess, core.OperationFailed, core.OperationUnknown:
		return true
	default:
		return false
	}
}

// validOperationTargetKind 只接受契约 target_kind 的十一个枚举值；网络白名单 / 禁止域名 /
// 执行档位 / 访问例外的写入行各自由同名动作产生，读侧必须能按同一字面量过滤。
// validOperationTargetKind accepts exactly the eleven contract kind literals.
func validOperationTargetKind(kind string) bool {
	switch kind {
	case "agent", "schedule", "model_connection", "mcp_server", "skill", "notify_channel",
		"network_allowlist", "network_denylist", "execution_policy", "network_exception", "system":
		return true
	default:
		return false
	}
}

// operationsBound 校验装配端口：Options.Operations 为 nil 是装配错误，按 500
// INTERNAL 返回（不是 404、也不是空列表 200）。
// operationsBound rejects a missing port as an assembly error, never as an empty list.
func (s *Server) operationsBound(w http.ResponseWriter) bool {
	if s.opts.Operations != nil {
		return true
	}
	s.logger().Error("装配缺少 Operations 端口")
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	return false
}

// handleListOperations 返回固定排序 / 窗口的操作列表；非法参数在触达仓储前返回
// 400（零查询）。顺序由仓储保持，本层不重排、不补行。
// handleListOperations maps the fixed window to OperationView rows; illegal parameters
// never reach the store and the storage order is preserved as-is.
func (s *Server) handleListOperations(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.operationsBound(w) {
		return
	}
	filter, err := parseOperationFilter(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	recs, err := s.opts.Operations.ListOperations(r.Context(), filter)
	if err != nil {
		s.logger().Error("查询管理操作失败", "error", err, "actor", actor.Subject)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	views := make([]OperationView, 0, len(recs))
	for _, rec := range recs {
		views = append(views, operationView(rec))
	}
	s.writeOK(w, views)
}

// handleOperationDetail 返回操作详情：`detail_json` 经白名单脱敏；`unknown` 的
// 详情在信封 message 里给出"未查证、不得盲目重做"的提示；未知标识 → 404。
// handleOperationDetail returns the redacted detail; an unknown operation carries the
// "not verified, never replay" hint in the envelope message.
func (s *Server) handleOperationDetail(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.operationsBound(w) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.opts.Operations.GetOperation(r.Context(), id)
	if err != nil {
		s.writeOperationError(w, err, id, actor)
		return
	}
	view := operationDetailView(rec)
	if rec.Status == core.OperationUnknown {
		s.writeOKMessage(w, view, messageOperationUnknown)
		return
	}
	s.writeOK(w, view)
}

// writeOperationError 把仓储读错误映射为 404 或 500：`core.ErrOperationNotFound`
// → 404（回显操作标识），其余记 ERROR 并只回固定文案（约束 10 / 24）。
// writeOperationError maps ErrOperationNotFound to 404 and everything else to a fixed 500.
func (s *Server) writeOperationError(w http.ResponseWriter, err error, operationID string, actor core.Actor) {
	if errors.Is(err, core.ErrOperationNotFound) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到管理操作："+operationID)
		return
	}
	s.logger().Error("查询管理操作详情失败", "operation_id", operationID, "error", err, "actor", actor.Subject)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

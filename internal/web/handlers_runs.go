package web

// 本文件是运行账本与取消的 handler（模块卡 §3.1.5）：HTTP 层只做参数解析、边界
// 判定与结果映射——固定排序 / 窗口 / 过滤在仓储 SQL（约束 21），取消的信号传播与
// 至多 30 秒等待在 `core.AgentService.Cancel`（约束 20 / 注 15）。本层**不写运行
// 终态、不自行等待**；取消成功与否只反映 core 返回的当前状态。
// Run ledger and cancellation handlers: parse, validate, and map. Ordering and
// windows live in SQL; the bounded cancel wait lives in core.

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
)

// 运行查询的固定窗口（约束 21）：缺省 50、上限 200；**不得**接受排序 / 分页参数。
// Fixed run-list window: default 50, maximum 200; no ordering or paging parameters.
const (
	defaultRunLimit = 50
	maxRunLimit     = 200
)

// 取消响应的两条固定 message（约束 20 / 注 15）：仍是 running（等待超时或信号未
// 送达）→ 信号已发出但尚未结束；终态 → 无需取消。**不**伪造 cancelled。
// The two cancel messages; a still-running row never reports cancelled.
const (
	cancelMessageRunning  = "取消信号已发出，运行尚未结束"
	cancelMessageTerminal = "运行已结束，无需取消"
)

// 参数非法时的固定 400 文案（约束 21）；不回显请求值，避免把用户输入原样带出。
// Fixed 400 texts; request values are never echoed back.
const (
	badRunStatusMessage = "status 取值非法（可选：running / completed / limit_reached / cancelled / failed）"
	badRunLimitMessage  = "limit 必须是 1–200 之间的整数"
)

// parseRunFilter 解析 GET /runs 的过滤参数与固定窗口（约束 21）：只认 session_id /
// agent / status / limit，未知参数忽略；非法 status、非正整数或超上限的 limit 一律
// 返回错误（调用方映射 400 BAD_REQUEST）。
// parseRunFilter accepts exactly the four documented parameters; anything else is
// ignored, and an illegal value is an error the caller maps to 400.
func parseRunFilter(r *http.Request) (core.RunFilter, error) {
	q := r.URL.Query()
	filter := core.RunFilter{
		SessionID: q.Get("session_id"),
		Agent:     q.Get("agent"),
		Limit:     defaultRunLimit,
	}
	if q.Has("status") {
		status := q.Get("status")
		if !validRunStatus(status) {
			return core.RunFilter{}, errors.New(badRunStatusMessage)
		}
		filter.Status = status
	}
	if q.Has("limit") {
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > maxRunLimit {
			return core.RunFilter{}, errors.New(badRunLimitMessage)
		}
		filter.Limit = limit
	}
	return filter, nil
}

// validRunStatus 只接受契约 RunView.status 的五个枚举值。
// validRunStatus accepts exactly the five contract status literals.
func validRunStatus(status string) bool {
	switch core.RunStatus(status) {
	case core.StatusRunning, core.StatusCompleted, core.StatusLimitReached,
		core.StatusCancelled, core.StatusFailed:
		return true
	default:
		return false
	}
}

// handleListRuns 返回固定排序 / 窗口的运行列表；非法参数在触达仓储前返回 400
// （零查询）。列表只带清单哈希，详情才带完整清单（约束 22 / 23）。
// handleListRuns maps the fixed window to RunView rows; illegal parameters never
// reach the store.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	filter, err := parseRunFilter(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	recs, err := s.opts.Runs.ListRuns(r.Context(), filter)
	if err != nil {
		s.logger().Error("查询运行列表失败", "error", err, "actor", actor.Subject)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	views := make([]RunView, 0, len(recs))
	for _, rec := range recs {
		views = append(views, runRecordView(rec))
	}
	s.writeOK(w, views)
}

// handleRunDetail 返回运行详情：RunView 全字段 + 准入清单视图；没有清单行的历史
// Run 走 legacy 形态（约束 24），未知 run_id → 404。
// handleRunDetail returns the run plus its admission manifest; historical runs read
// as legacy and an unknown id is a 404.
func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	id := r.PathValue("id")
	rec, err := s.opts.Runs.GetRun(r.Context(), id)
	if err != nil {
		s.writeRunLedgerError(w, err, id)
		return
	}
	s.writeOK(w, runDetailView(rec))
}

// handleCancelRun 请求取消并返回**当前**状态：等待上限与信号传播都在 core，本层只
// 映射结果（约束 20 / 注 15）。200 的 message 说明"尚未结束"或"无需取消"。
// handleCancelRun asks core to cancel and returns the current state; the bounded wait
// belongs to core and this layer only maps.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	id := r.PathValue("id")
	detail, err := s.opts.Runs.Cancel(r.Context(), id)
	if err != nil {
		s.writeRunLedgerError(w, err, id)
		return
	}
	view := runRecordView(detail)
	s.writeOKMessage(w, view, cancelRunMessage(view.Status))
}

// writeRunLedgerError 把账本读 / 取消错误映射为 404 或 500：`core.ErrRunNotFound`
// → 404（回显运行标识），其余记 ERROR 并只回固定文案（约束 23）。
// writeRunLedgerError maps ErrRunNotFound to 404 and everything else to a fixed 500.
func (s *Server) writeRunLedgerError(w http.ResponseWriter, err error, runID string) {
	if errors.Is(err, core.ErrRunNotFound) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到运行："+runID)
		return
	}
	s.logger().Error("运行账本操作失败", "run_id", runID, "error", err)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

// cancelRunMessage 按当前状态选固定 message：仍是 running → 信号已发出、尚未结束；
// 终态 → 无需取消。取消是信号，终态只能由循环经 FinishRun 写入（注 15）。
// cancelRunMessage picks the message from the current state; cancellation never
// writes a terminal state.
func cancelRunMessage(status string) string {
	if status == string(core.StatusRunning) {
		return cancelMessageRunning
	}
	return cancelMessageTerminal
}

package web

// 本文件是定时任务管理面的四个 handler（模块卡 §3.1.7）：HTTP 层只做解码、
// 视图投影与错误映射——任务定义、执行历史与手动补跑全部由 ScheduleService
// 背后的 core.Scheduler 完成；补跑与定时触发共用同一条 `core.AgentService.Run`
// 执行链。会话忙 / 实例上限 / 归档互斥的唯一归属是 AgentService，web 不另建互斥。
// The four /schedules handlers: decode, project views, and map errors only. The
// scheduler owns definitions and execution history; manual runs share the same Run
// entry point, and the web layer holds no admission state of its own.

import (
	"context"
	"errors"
	"io/fs"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// ScheduleService 是定时任务管理面的消费方端口（实现在 core.Scheduler）。
// ScheduleService is the management-face port implemented by core.Scheduler.
type ScheduleService interface {
	List(ctx context.Context) ([]core.TaskRecord, error)
	SetEnabled(ctx context.Context, taskID string, enabled bool) error
	Executions(ctx context.Context, taskID string, limit int) ([]core.ExecutionRecord, error)
	RunNow(ctx context.Context, taskID string) (core.RunRequest, core.RunResult, error)
}

// 管理面固定字面量（模块卡 §3.1.7；messageTaskBusy 逐字取自 D-01 裁决）。
// Management-face literals; messageTaskBusy is the frozen 409 text.
const (
	scheduleHistoryLimit = 100
	messageTaskBusy      = "该任务上一轮尚未结束，请等待完成或稍后重试"
)

// setEnabledBody 用指针区分 `enabled` 缺失 / null（400）与 false（停用）。
// setEnabledBody distinguishes a missing/null enabled from an explicit false.
type setEnabledBody struct {
	Enabled *bool `json:"enabled"`
}

// schedulesBound 校验装配端口：Options.Schedules 为 nil 是装配错误，按 500
// INTERNAL 返回（不是 404、也不是空列表 200）。
// schedulesBound rejects a missing port as an assembly error, never as a 404.
func (s *Server) schedulesBound(w http.ResponseWriter) bool {
	if s.opts.Schedules != nil {
		return true
	}
	s.logger().Error("装配缺少 Schedules 端口")
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	return false
}

// handleListSchedules 返回可见任务（retired / Agent 未注册的行由 scheduler
// 过滤）；顺序由服务保持，web 层不重排、不补行。
// handleListSchedules returns the visible tasks in the service's order.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if !s.schedulesBound(w) {
		return
	}
	recs, err := s.opts.Schedules.List(r.Context())
	if err != nil {
		s.writeScheduleError(w, err)
		return
	}
	out := make([]ScheduleView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, scheduleView(rec))
	}
	s.writeOK(w, out)
}

// handleSetScheduleEnabled 启停一条任务；成功响应无 data（与归档同款"唯一无
// 载荷成功"口径）。缺 `enabled` / null 在触达服务之前返回 400。
// handleSetScheduleEnabled toggles one task; validation precedes any service call.
func (s *Server) handleSetScheduleEnabled(w http.ResponseWriter, r *http.Request) {
	if !s.schedulesBound(w) {
		return
	}
	id := r.PathValue("id")
	var body setEnabledBody
	if !s.decodeBody(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "enabled 必填")
		return
	}
	if err := s.opts.Schedules.SetEnabled(r.Context(), id, *body.Enabled); err != nil {
		s.writeScheduleError(w, err)
		return
	}
	s.writeOK(w, nil)
}

// handleListExecutions 返回固定 100 条窗口（契约：按 started_at 倒序，由仓储
// SQL 保证）；空历史输出 `[]` 而不是 `null`（约束 34）。
// handleListExecutions returns the fixed 100-row window; emptiness is `[]`.
func (s *Server) handleListExecutions(w http.ResponseWriter, r *http.Request) {
	if !s.schedulesBound(w) {
		return
	}
	id := r.PathValue("id")
	recs, err := s.opts.Schedules.Executions(r.Context(), id, scheduleHistoryLimit)
	if err != nil {
		s.writeScheduleError(w, err)
		return
	}
	out := make([]ExecutionView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, executionView(rec))
	}
	s.writeOK(w, out)
}

// handleRunSchedule 手动补跑一条任务：与定时触发共用同一条执行链（约束 5）。
// 两个 409 来源互不相同——任务锁（core.ErrTaskBusy，不发 Run、零执行行）用
// messageTaskBusy；兄弟任务占会话（SessionBusyError）复用既有 messageBusy。
// 截止优先于错误类型判定：ctx 已过期时一律 504，不写 200 + 视图。
// handleRunSchedule runs a task now. The two 409 sources keep distinct texts, and an
// expired run context always maps to 504 before any error-type check.
func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.schedulesBound(w) {
		return
	}
	id := r.PathValue("id")

	runCtx, cancel := context.WithTimeout(r.Context(), s.opts.RunTimeout)
	defer cancel()
	started := s.now()
	req, res, err := s.opts.Schedules.RunNow(runCtx, id)
	if r.Context().Err() != nil {
		// 客户端已断开也必须留下 failed 诊断（与 handlePostMessage 同口径）。
		s.logFailedRun(r, res, req)
		s.logClientGone(r)
		return
	}
	if err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			s.writeError(w, http.StatusGatewayTimeout, codeTimeout, "运行超过 60 秒时限")
			return
		}
		if errors.Is(err, core.ErrTaskBusy) {
			s.writeError(w, http.StatusConflict, codeSessionBusy, messageTaskBusy)
			return
		}
		s.writeRunError(w, err, runCtx)
		return
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		s.writeError(w, http.StatusGatewayTimeout, codeTimeout, "运行超过 60 秒时限")
		return
	}
	s.writeRunResult(r, w, res, req, started, s.now())
}

// writeScheduleError 映射管理端点（列表 / 启停 / 执行历史）的错误：未知 Agent
// 与 fs.ErrNotExist（未知任务、不可见任务）→ 404，其余 500 且只出固定文案。
// writeScheduleError maps management-endpoint misses to 404 and everything else to 500.
func (s *Server) writeScheduleError(w http.ResponseWriter, err error) {
	var agentErr *core.AgentNotFoundError
	if errors.As(err, &agentErr) {
		s.writeError(w, http.StatusNotFound, codeNotFound,
			"Agent 请求名不存在或未注册："+agentErr.Name)
		return
	}
	if errors.Is(err, fs.ErrNotExist) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到请求的资源")
		return
	}
	s.logger().Error("定时任务操作失败", "error", err)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

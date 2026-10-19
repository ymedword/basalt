package web

// 本文件是 HTTP 响应视图的**唯一**投影面（模块卡 §3.1）：把 core / 端口里的记录转成
// 契约 schema（openapi.yaml）声明的形状。两条固定口径（约束 34）：① 所有数组字段空时
// 输出 `[]` 而不是 `null`；② 时间字段一律 RFC3339 UTC，零值渲染为空串（"不知道时间"
// 不得伪装成 0001 年）。原始原因不在这里出站——`runView` 只经 `publicRunReason` 投影。
// The only projection surface for response views: empty collections marshal to `[]`,
// times are RFC3339 UTC (zero renders empty), and raw run reasons never leave this layer.

import (
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// InfoView 是 /info 的响应体（契约 components.schemas.InfoView）。
// InfoView is the /info response body.
type InfoView struct {
	Version       string         `json:"version"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	Agents        int            `json:"agents"` // 已注册数，不是装载总数（约束 25）
	Tools         int            `json:"tools"`
	MCPServers    int            `json:"mcp_servers"` // 已连接数（Options.MCPConnected）
	Providers     []ProviderView `json:"providers"`
}

// ProviderView 是 provider 摘要：**没有**凭证与 base_url 字段（约束 25）。
// ProviderView carries no credential or base_url fields.
type ProviderView struct {
	Name       string   `json:"name"`
	Models     []string `json:"models"`
	Configured bool     `json:"configured"`
}

// ProfileView 是 /profiles 的元素（契约 components.schemas.ProfileView）。
// ProfileView is one /profiles element.
type ProfileView struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	Tools       []string `json:"tools"`
	Skills      []string `json:"skills"`
	Schedules   int      `json:"schedules"`
	Registered  bool     `json:"registered"`
	Problems    []string `json:"problems"`
}

// ToolView 是 /tools 的元素；Source 枚举 builtin | mcp | code。
// ToolView is one /tools element; Source is builtin, mcp, or code.
type ToolView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
}

// MemoryView 是 /memory 的响应体：只读全局层，两分区带标题、空分区为空串。
// MemoryView is the /memory body: the global layer's two titled partitions.
type MemoryView struct {
	Scope   string `json:"scope"`   // global
	Agent   string `json:"agent"`   // 全局层为空串
	Backend string `json:"backend"` // markdown | sqlite
	Core    string `json:"core"`
	Archive string `json:"archive"`
}

// SessionSummaryView 是会话摘要（契约 components.schemas.SessionSummary）。
// SessionSummaryView is the session summary contract shape.
type SessionSummaryView struct {
	SessionID    string `json:"session_id"`
	ProfileName  string `json:"profile_name"`
	Channel      string `json:"channel"`
	UserID       string `json:"user_id"`
	Status       string `json:"status"`
	MessageCount int    `json:"message_count"`
	CreatedAt    string `json:"created_at"`
	LastActiveAt string `json:"last_active_at"`
}

// MessageView 是历史窗口里的一条消息（契约 components.schemas.MessageView）；
// 推理内容与供应商扩展字段**不出站**。
// MessageView is one history message; reasoning and provider extras never go out.
type MessageView struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []ToolCallView `json:"tool_calls"`
	ToolCallID string         `json:"tool_call_id"`
	CreatedAt  string         `json:"created_at"`
}

// ToolCallView 是 assistant 消息发起的一次调用（id / name / arguments）。
// ToolCallView is one tool call issued by an assistant message.
type ToolCallView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// SessionDetailView 是会话详情：摘要字段内嵌摊平 + 最近 100 条消息。
// SessionDetailView flattens the summary fields and adds the message window.
type SessionDetailView struct {
	SessionSummaryView
	Messages []MessageView `json:"messages"`
}

// RunView 是运行结果（契约 components.schemas.RunView）；trigger 取自 RunRequest
// （scheduler 面为 cron / manual，空值回落 api），reason 只出公开投影文案
// （约束 23 / 注 5 / 注 15）。
// RunView projects one run; trigger comes from the RunRequest (cron or manual on the
// scheduler face, falling back to api when empty) and reason is the public projection.
type RunView struct {
	RunID      string `json:"run_id"`
	SessionID  string `json:"session_id"`
	Agent      string `json:"agent"`
	Channel    string `json:"channel"`
	Trigger    string `json:"trigger"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	FinalText  string `json:"final_text"`
	Iterations int    `json:"iterations"`
	ToolCalls  int    `json:"tool_calls"`
	ConfigHash string `json:"config_hash"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

// ScheduleView 是 /schedules 的元素（契约 components.schemas.ScheduleView）；id 取
// TaskID，last_run_at 零值渲染空串。
// ScheduleView is one /schedules element; id is the TaskID and a zero last run
// renders as an empty string.
type ScheduleView struct {
	ID         string `json:"id"`
	Agent      string `json:"agent"`
	Cron       string `json:"cron"`
	Zone       string `json:"zone"`
	Message    string `json:"message"`
	Enabled    bool   `json:"enabled"`
	LastRunAt  string `json:"last_run_at"`
	LastStatus string `json:"last_status"`
}

// ExecutionView 是 /schedules/{id}/executions 的元素（契约 components.schemas
// .ExecutionView）；started 行的 run_id / finished_at 为空串，error_message 只出
// 公开投影（约束 34）。
// ExecutionView is one execution-history element; nullable columns render empty and
// error_message is always the public projection.
type ExecutionView struct {
	TaskID       string `json:"task_id"`
	RunID        string `json:"run_id"`
	Status       string `json:"status"`
	Trigger      string `json:"trigger"`
	DurationMS   int64  `json:"duration_ms"`
	ErrorMessage string `json:"error_message"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at"`
}

// sessionSummaryView 投影一行会话记录。
// sessionSummaryView projects one session row.
func sessionSummaryView(rec core.SessionRecord) SessionSummaryView {
	return SessionSummaryView{
		SessionID:    rec.ID,
		ProfileName:  rec.ProfileName,
		Channel:      rec.Channel,
		UserID:       rec.UserID,
		Status:       rec.Status,
		MessageCount: rec.MessageCount,
		CreatedAt:    formatTime(rec.CreatedAt),
		LastActiveAt: formatTime(rec.LastActiveAt),
	}
}

// messageView 投影一条消息；无工具调用时输出 `[]`（约束 34）。
// messageView projects one message; no tool calls still marshals to `[]`.
func messageView(m core.Message) MessageView {
	calls := make([]ToolCallView, 0, len(m.ToolCalls))
	for _, c := range m.ToolCalls {
		calls = append(calls, ToolCallView{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
	}
	return MessageView{
		Role:       m.Role,
		Content:    m.Content,
		ToolCalls:  calls,
		ToolCallID: m.ToolCallID,
		CreatedAt:  formatTime(m.CreatedAt),
	}
}

// sessionDetailView 组合摘要与消息窗口；空窗口输出 `[]`。
// sessionDetailView combines the summary with the message window; emptiness is `[]`.
func sessionDetailView(rec core.SessionRecord, msgs []core.Message) SessionDetailView {
	out := make([]MessageView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageView(m))
	}
	return SessionDetailView{SessionSummaryView: sessionSummaryView(rec), Messages: out}
}

// profileView 投影一条装载结果（约束 26）：顺序由调用方保持；未注册项的 problems
// **恰含** RejectReason 一项；Profile 为 nil 时派生字段为空、数组输出 `[]`。
// profileView projects one loader entry; an unregistered entry carries exactly its
// reject reason in problems, and a nil profile yields empty derived fields.
func profileView(entry core.AgentEntry) ProfileView {
	v := ProfileView{
		Name:       entry.Name,
		Tools:      []string{},
		Skills:     []string{},
		Problems:   []string{},
		Registered: entry.Registered,
	}
	if p := entry.Profile; p != nil {
		v.Description = p.Description
		v.Provider = p.Provider.Name
		v.Model = p.Provider.Model
		v.Tools = stringSlice(p.Tools)
		v.Skills = stringSlice(p.Skills)
		v.Schedules = len(p.Schedules)
	}
	if !entry.Registered && entry.RejectReason != "" {
		// 未注册且已给拒因 → 恰含一项（约束 26）；轻路径的待确认项没有拒因，
		// 输出 `[]` 而不是一个空串问题。
		v.Problems = []string{entry.RejectReason}
	}
	return v
}

// toolView 投影一个工具描述符；来源枚举由装配层保证（builtin | mcp | code）。
// toolView projects one tool descriptor; Source is assigned by the assembly layer.
func toolView(d ToolDescriptor) ToolView {
	return ToolView{Name: d.Spec.Name, Description: d.Spec.Description, Source: d.Source}
}

// runView 投影一次运行结果。StartedAt / FinishedAt 由 handler 取装配时钟记录；
// reason 走公开投影，原始原因只留在 runs 表与日志里（约束 23）。
// runView projects one run; the reason is projected, the raw value stays in the runs
// table and logs.
func runView(req core.RunRequest, res core.RunResult, started, finished time.Time) RunView {
	trigger := string(req.Trigger)
	if trigger == "" {
		trigger = triggerAPI
	}
	return RunView{
		RunID:      res.RunID,
		SessionID:  req.SessionID,
		Agent:      req.Agent,
		Channel:    req.Channel,
		Trigger:    trigger,
		Status:     string(res.Status),
		Reason:     publicRunReason(res.Status, res.Reason),
		FinalText:  res.FinalText,
		Iterations: res.Iterations,
		ToolCalls:  res.ToolCalls,
		ConfigHash: res.ConfigHash,
		StartedAt:  formatTime(started),
		FinishedAt: formatTime(finished),
	}
}

// scheduleView 投影一条任务定义；时间列经 formatTime（零值渲染空串）。
// scheduleView projects one task definition; times go through formatTime.
func scheduleView(rec core.TaskRecord) ScheduleView {
	return ScheduleView{
		ID:         rec.TaskID,
		Agent:      rec.Agent,
		Cron:       rec.Cron,
		Zone:       rec.Zone,
		Message:    rec.Message,
		Enabled:    rec.Enabled,
		LastRunAt:  formatTime(rec.LastRunAt),
		LastStatus: string(rec.LastStatus),
	}
}

// executionView 投影一行执行历史：可空列（run_id / finished_at）渲染空串，
// error_message 只经 publicExecutionMessage 出站（约束 34 / R22）。
// executionView projects one execution row; the raw error never leaves this layer.
func executionView(rec core.ExecutionRecord) ExecutionView {
	return ExecutionView{
		TaskID:       rec.TaskID,
		RunID:        rec.RunID,
		Status:       string(rec.Status),
		Trigger:      string(rec.Trigger),
		DurationMS:   rec.DurationMS,
		ErrorMessage: publicExecutionMessage(rec.Status, rec.ErrorMessage),
		StartedAt:    formatTime(rec.StartedAt),
		FinishedAt:   formatTime(rec.FinishedAt),
	}
}

// 执行历史的公开错误文案（约束 34 / D-21）：归档跳过文案由 scheduler 直写
// error_message，其余 skipped 原文（锁失、兄弟任务占会话）一律落回通用文案。
// Public execution messages: the archived-skip literal is written by the scheduler,
// every other skip reason collapses into the generic text.
const (
	executionMessageArchived = "会话已归档，本轮跳过"
	executionMessageSkipped  = "上一轮尚未结束，本轮跳过"
)

// publicExecutionMessage 按 status + 逐字匹配的 raw 做三路投影（约束 34）：
// started / completed → 空串；failed → 运行失败；skipped 且 raw 恰为归档冻结文案
// → 同文案；其余 skipped → 通用跳过文案。**不得**用子串 / 前缀匹配——未识别原文
// （可能含绝对路径、SQL 正文与用户数据）只落库、永不出站。
// publicExecutionMessage projects by status and an exact raw match; unknown text
// always falls back to the generic skip message.
func publicExecutionMessage(status core.ExecutionStatus, raw string) string {
	switch status {
	case core.ExecutionStarted, core.ExecutionCompleted:
		return ""
	case core.ExecutionFailed:
		return publicReasonFailed
	case core.ExecutionSkipped:
		if raw == executionMessageArchived {
			return executionMessageArchived
		}
		return executionMessageSkipped
	default:
		return ""
	}
}

// infoView 汇总进程读面（约束 25）：agents 只数已注册项；MCP 连接数来自装配注入；
// uptime 起点零值时按 0 秒处理，时钟回拨也不出负数。
// infoView assembles the process read face; only registered agents are counted and
// a zero/negative uptime never leaks out.
func infoView(opts Options, now time.Time) InfoView {
	uptime := int64(0)
	if !opts.StartedAt.IsZero() {
		if d := now.Sub(opts.StartedAt); d > 0 {
			uptime = int64(d / time.Second)
		}
	}
	agents := 0
	if opts.Agents != nil {
		for _, entry := range opts.Agents.Profiles() {
			if entry.Registered {
				agents++
			}
		}
	}
	tools := 0
	if opts.Tools != nil {
		tools = len(opts.Tools.Tools())
	}
	providers := make([]ProviderView, 0, len(opts.Providers))
	for _, p := range opts.Providers {
		providers = append(providers, ProviderView{
			Name:       p.Name,
			Models:     stringSlice(p.Models),
			Configured: p.Configured,
		})
	}
	return InfoView{
		Version:       opts.Version,
		UptimeSeconds: uptime,
		Agents:        agents,
		Tools:         tools,
		MCPServers:    opts.MCPConnected,
		Providers:     providers,
	}
}

// formatTime 是视图时间字段的唯一格式器：RFC3339 UTC；零值渲染空串（约束 34）。
// formatTime is the single time formatter for views; zero renders as empty.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// stringSlice 保证数组字段非 nil——`null` 会让 SDK 反序列化失败（约束 34）。
// stringSlice guarantees a non-nil array; `null` breaks SDK deserialization.
func stringSlice(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

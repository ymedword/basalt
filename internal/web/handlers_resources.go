package web

// 本文件是只读资源面（模块卡 §3.1.8）：技能、MCP server 与模型连接的列表 / 详情。
// 数据由 app 层适配 ResourceReader（发布指针 + 定义原文 + 运行期连接视图），本层
// 只做参数与错误映射——不回读文件系统、不解析定义 YAML、**不得**在投影里补造凭证
// 值或版本号（约束 35 / 36）。只读端点不写管理审计（约束 12）。
// Read-only resource handlers. The app layer adapts ResourceReader; this layer only
// maps parameters and errors and never fabricates credentials or version numbers.

import (
	"context"
	"errors"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// McpServerDetail 是 MCP 详情端口的**内部**返回结构（人工 review ② F3）：携带响应体
// 需要的 `args` / `url`（契约 McpServerDetail）与只经响应头的 ETag。`McpServerView`
// 本身没有 ETag 公开字段——**不得**把 ETag 塞进公开视图凑头，也不得新增契约外字段。
// 2026-10-25 US-14 走查裁决：详情响应体按契约补齐 `args` / `url`（此前实现只回
// 视图，导致详情页“修改”表单读 `args` 崩溃）。
// McpServerDetail carries the body fields required by the contract (args/url) plus the
// header-only ETag. The ETag never enters the public body.
type McpServerDetail struct {
	View McpServerView
	ETag string
	Args []string
	URL  string
}

// ResourceReader 是资源只读面的消费方端口（模块卡 §3.1.8 的固定签名）：列表与详情
// 各三条，缺失资源以 `core.ErrResourceNotFound` 报告，其余错误按 500 处理。
// MCP 详情的唯一签名差异是返回内部的 McpServerDetail（review ② F3），
// 五个其余方法的签名逐字保真。
// ResourceReader is the frozen consumer-side port; a missing resource reports
// core.ErrResourceNotFound and every other error maps to a fixed 500. McpServer is the
// one signature that returns an internal detail struct so the ETag can reach the header.
type ResourceReader interface {
	Skills(ctx context.Context) ([]SkillView, error)
	Skill(ctx context.Context, name string) (SkillView, error)
	MCPServers(ctx context.Context) ([]McpServerView, error)
	McpServer(ctx context.Context, name string) (McpServerDetail, error)
	ModelConnections(ctx context.Context) ([]ModelConnectionView, error)
	ModelConnection(ctx context.Context, name string) (ModelConnectionView, error)
}

// 详情 404 的固定文案前缀：与既有 `"未找到运行："+id` 同一句式，**不回显**底层错误
// （约束 24：失败文案不泄露内部路径）。
// Fixed 404 prefixes; the underlying error never reaches the response.
const (
	resourceSkillNotFound           = "未找到技能："
	resourceMcpServerNotFound       = "未找到 MCP 服务："
	resourceModelConnectionNotFound = "未找到模型连接："
)

// resourcesBound 校验装配端口：Options.Resources 为 nil 是装配错误，按 500 INTERNAL
// 返回（不是 404、也不是空列表 200——空列表会被读成"确实没有资源"）。
// resourcesBound rejects a missing port as an assembly error, never as an empty list.
func (s *Server) resourcesBound(w http.ResponseWriter) bool {
	if s.opts.Resources != nil {
		return true
	}
	s.logger().Error("装配缺少 Resources 端口")
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	return false
}

// writeResourceError 把读取错误映射为 404 或 500：`core.ErrResourceNotFound` →
// 404（回显资源名），其余记 ERROR 并只回固定文案（约束 24 / 37）。
// writeResourceError maps ErrResourceNotFound to 404 and everything else to a fixed 500.
func (s *Server) writeResourceError(w http.ResponseWriter, err error, what, notFoundMessage string, actor core.Actor) {
	if errors.Is(err, core.ErrResourceNotFound) {
		s.writeError(w, http.StatusNotFound, codeNotFound, notFoundMessage)
		return
	}
	s.logger().Error("读取资源失败", "resource", what, "error", err, "actor", actor.Subject)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

// handleListSkills 返回发布指针可见的技能（无正文）：顺序由适配层给出，本层不重排、
// 空结果序列化为 `[]` 而不是 `null`。
// handleListSkills projects the visible skills; an empty list still serializes as [].
func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.resourcesBound(w) {
		return
	}
	views, err := s.opts.Resources.Skills(r.Context())
	if err != nil {
		s.writeResourceError(w, err, "skills", "", actor)
		return
	}
	s.writeOK(w, nonNilSkillViews(views))
}

// handleGetSkill 返回技能详情并设置 ETag 响应头（契约 GET /skills/{name}）：
// 头值就是视图里的强 ETag，二者同源，**不得**另算一份。
// handleGetSkill returns the skill detail and sets the ETag header from the same view
// field, so the header and the body can never drift apart.
func (s *Server) handleGetSkill(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.resourcesBound(w) {
		return
	}
	name := r.PathValue("name")
	view, err := s.opts.Resources.Skill(r.Context(), name)
	if err != nil {
		s.writeResourceError(w, err, "skill", resourceSkillNotFound+name, actor)
		return
	}
	if view.ETag != "" {
		w.Header().Set("ETag", view.ETag)
	}
	s.writeOK(w, view)
}

// handleListMCPServers 返回 MCP server 列表：只含可执行名与环境变量键名，连接状态与
// 工具名来自运行期句柄（约束 35）。
// handleListMCPServers projects the MCP servers; only the executable name and the env
// key names leave the process.
func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.resourcesBound(w) {
		return
	}
	views, err := s.opts.Resources.MCPServers(r.Context())
	if err != nil {
		s.writeResourceError(w, err, "mcp-servers", "", actor)
		return
	}
	s.writeOK(w, nonNilMcpServerViews(views))
}

// handleGetMCPServer 返回单个 MCP server 详情并设置 ETag 响应头（契约 GET
// /mcp-servers/{name}；人工 review ② F3）：头值来自端口返回的内部结构，响应体按
// 契约 `McpServerDetail` 输出视图 + `args` / `url`——ETag **不得**作为公开字段出站。
// 无发布版本（未入账的配置项）时 ETag 为空串，此时不设头，与技能 / 模型连接同一口径。
// handleGetMCPServer writes the contract detail body (view + args/url) and sets the
// ETag header from the port's internal struct; the ETag never enters the body.
func (s *Server) handleGetMCPServer(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.resourcesBound(w) {
		return
	}
	name := r.PathValue("name")
	detail, err := s.opts.Resources.McpServer(r.Context(), name)
	if err != nil {
		s.writeResourceError(w, err, "mcp-server", resourceMcpServerNotFound+name, actor)
		return
	}
	if detail.ETag != "" {
		w.Header().Set("ETag", detail.ETag)
	}
	s.writeOK(w, McpServerDetailView{
		McpServerView: detail.View,
		Args:          nonNilStrings(detail.Args),
		URL:           detail.URL,
	})
}

// handleListModelConnections 返回模型连接列表：只回 `${ENV_VAR}` 引用名。
// handleListModelConnections projects the model connections; only the env reference
// name is exposed.
func (s *Server) handleListModelConnections(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.resourcesBound(w) {
		return
	}
	views, err := s.opts.Resources.ModelConnections(r.Context())
	if err != nil {
		s.writeResourceError(w, err, "model-connections", "", actor)
		return
	}
	s.writeOK(w, nonNilModelConnectionViews(views))
}

// handleGetModelConnection 返回模型连接详情并设置 ETag 响应头（与技能同一口径）。
// handleGetModelConnection returns the detail and sets the ETag header from the view.
func (s *Server) handleGetModelConnection(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.resourcesBound(w) {
		return
	}
	name := r.PathValue("name")
	view, err := s.opts.Resources.ModelConnection(r.Context(), name)
	if err != nil {
		s.writeResourceError(w, err, "model-connection", resourceModelConnectionNotFound+name, actor)
		return
	}
	if view.ETag != "" {
		w.Header().Set("ETag", view.ETag)
	}
	s.writeOK(w, view)
}

// nonNilSkillViews 把 nil 列表规范为 `[]`：JSON 的 `null` 会被客户端读成"字段缺失"，
// 而契约把 data 声明为数组（列表为空与"没有这个端点"必须可区分）。
// The three helpers keep empty lists serializing as [] rather than null.
func nonNilSkillViews(views []SkillView) []SkillView {
	return append(make([]SkillView, 0, len(views)), views...)
}

// nonNilMcpServerViews 同上（MCP）。
// nonNilMcpServerViews is the MCP counterpart.
func nonNilMcpServerViews(views []McpServerView) []McpServerView {
	return append(make([]McpServerView, 0, len(views)), views...)
}

// nonNilModelConnectionViews 同上（模型连接）。
// nonNilModelConnectionViews is the model-connection counterpart.
func nonNilModelConnectionViews(views []ModelConnectionView) []ModelConnectionView {
	return append(make([]ModelConnectionView, 0, len(views)), views...)
}

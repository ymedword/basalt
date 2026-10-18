package web

// 本文件是只读元数据端点（模块卡 §3.1 / 约束 24）：/health /info /profiles /tools
// /memory 不触达任何写面；/memory 只读全局层。`registeredAgent` 同时服务建会话与
// invoke 两条运行入口——「注册」的判定只此一处，避免两处口径漂移。
// Read-only metadata endpoints; the registeredAgent lookup is the single definition
// shared by session creation and stateless invoke.

import (
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// memoryScopeGlobal 是 /memory 的固定 scope 字面量（契约 MemoryView.scope 枚举）。
// memoryScopeGlobal is the fixed /memory scope literal.
const memoryScopeGlobal = "global"

// defaultMemoryBackend 是归一化配置为空时的后端字面量（约束 28：空 → markdown）。
// defaultMemoryBackend is the fallback backend literal when config is empty.
const defaultMemoryBackend = "markdown"

// registeredAgent 按名字查装载结果；未注册（含被拒）返回 false。
// 供 POST /sessions 与 POST /agents/{name}/invoke 共用。
// registeredAgent looks a name up in the loader result; unregistered entries,
// including rejected ones, report false.
func (s *Server) registeredAgent(name string) (core.AgentEntry, bool) {
	for _, entry := range s.opts.Agents.Profiles() {
		if entry.Name == name && entry.Registered {
			return entry, true
		}
	}
	return core.AgentEntry{}, false
}

// handleHealth 是存活探针：200 即健康，载荷固定 `{"status":"ok"}`（契约 /health）。
// handleHealth is the liveness probe; 200 alone means healthy.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeOK(w, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

// handleInfo 汇总进程读面（约束 25）：agents 只数已注册项、MCP 数为真实连接数、
// provider 摘要不含任何凭证字段。
// handleInfo serves the process read face with no credential fields.
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	s.writeOK(w, infoView(s.opts, s.now()))
}

// handleProfiles 按装载器顺序投影全部条目（含未注册项，约束 26）。
// handleProfiles projects every loader entry in loader order.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	entries := s.opts.Agents.Profiles()
	views := make([]ProfileView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, profileView(entry))
	}
	s.writeOK(w, views)
}

// handleTools 按注册表顺序投影工具清单（来源由装配层分类，约束 27）。
// handleTools projects the tool inventory in registry order.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	tools := s.opts.Tools.Tools()
	views := make([]ToolView, 0, len(tools))
	for _, d := range tools {
		views = append(views, toolView(d))
	}
	s.writeOK(w, views)
}

// handleMemory 只读全局层两分区（约束 28）：分区原样透传（含标题行、空分区为空串），
// backend 取归一化配置；读取失败记 ERROR 并回固定 500 文案。
// handleMemory serves the global layer's two partitions; read failures are 500.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	coreText, archiveText, err := s.opts.Memory.Sections(r.Context(), core.GlobalScope)
	if err != nil {
		s.logger().Error("读取全局记忆失败", "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	backend := s.opts.MemoryBackend
	if backend == "" {
		backend = defaultMemoryBackend
	}
	s.writeOK(w, MemoryView{
		Scope:   memoryScopeGlobal,
		Agent:   "",
		Backend: backend,
		Core:    coreText,
		Archive: archiveText,
	})
}

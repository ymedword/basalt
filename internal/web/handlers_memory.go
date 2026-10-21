package web

// 本文件是 Agent 层记忆的只读端点（模块卡 §3.1.27）：查询就地返回 Agent 层内容，
// **不**回退全局层——回退只发生在提示词组装（`LoadEffective`），查询也回退会让
// "这个 Agent 没有自己的记忆"这一事实在页面上永远不可见。读取失败回固定 500，
// 不返回半份内容（两分区要么都拿到、要么都不出）。
// Agent-layer memory read; no fallback happens at query time, and a read failure
// returns no partial content.

import (
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// memoryScopeAgent 是 Agent 层查询的固定 scope 字面量（契约 MemoryView.scope 枚举）。
// memoryScopeAgent is the fixed agent-layer scope literal.
const memoryScopeAgent = "agent"

// handleAgentMemory 处理 `GET /api/v1/agents/{name}/memory`：Agent 不存在或未注册
// → 404（与建会话 / invoke 同一处判定 `registeredAgent`）；成功响应的 `agent`
// 字段回显请求名，`scope` 恒为 `agent`。
// handleAgentMemory serves one agent's memory layer; unknown or unregistered agents
// are 404 through the single registeredAgent check.
func (s *Server) handleAgentMemory(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	name := r.PathValue("name")
	if _, ok := s.registeredAgent(name); !ok {
		s.writeError(w, http.StatusNotFound, codeNotFound,
			"Agent 请求名不存在或未注册："+name)
		return
	}

	coreText, archiveText, err := s.opts.Memory.Sections(r.Context(), core.Scope{Agent: name})
	if err != nil {
		s.logger().Error("读取 Agent 记忆失败", "agent", name, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	backend := s.opts.MemoryBackend
	if backend == "" {
		backend = defaultMemoryBackend
	}
	s.writeOK(w, MemoryView{
		Scope:   memoryScopeAgent,
		Agent:   name,
		Backend: backend,
		Core:    coreText,
		Archive: archiveText,
	})
}

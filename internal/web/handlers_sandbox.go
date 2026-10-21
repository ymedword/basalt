package web

// 本文件是沙箱白名单的只读端点（模块卡 §3.1.28）：从装配层交来的 `SandboxConfig`
// 原样投影四类白名单；本面只读，修改白名单需改配置并重启（约束 29）——页面不提供
// 任何写入口，也不谎造"改了就生效"。投影在 `sandboxView`：路径只出相对展示形态，
// 宿主绝对路径与解析后的凭证一律不出站。
// The read-only sandbox view; the projection keeps relative display paths only, and
// changes require editing configuration and restarting.

import (
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// handleSandbox 处理 `GET /api/v1/sandbox`（契约 openapi.yaml）。
// handleSandbox serves the sandbox allowlist view.
func (s *Server) handleSandbox(w http.ResponseWriter, _ *http.Request, _ core.Actor) {
	s.writeOK(w, sandboxView(s.opts.Sandbox))
}

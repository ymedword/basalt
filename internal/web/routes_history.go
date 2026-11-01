package web

// 本文件是历史回查三条端点的**路由行**（US-16 / T056）：会话列表 + 历史检索 +
// 精确读取。四条口径：
//
//  1. **三条都是业务面**：契约三条端点都声明 `security: callerKey / bearerAuth /
//     localTrust`，因此都可以由调用方 Key 调用——`GET /sessions` 在 T056 之前是
//     管理面行（admin token only），本轮翻转为业务面并**强制**按当前身份的
//     `sessions.caller_id` 过滤；不翻转就等于让 caller key 读到他人会话。
//  2. `GET /sessions` 的作用域是 `sessions:read`：契约给它的权限口径与历史检索一致
//     ——能读自己的会话，不等于能写。
//  3. 三条都计 `IngressRead`（D-30 / T126）：检索与精确读取是**只读**，会话列表也是；
//     任何一条按写路径计桶都会让"打开列表页"消耗运行预算。
//  4. 排序与窗口一律由存储 / 领域层固定：路由行不声明排序参数，handler 不二次重排。
//
// This file enumerates the three history routes. All are business rows because the
// contract gives each of them an operation security block; GET /sessions flips from the
// management face so a caller key can only list its own sessions. All charge IngressRead.

import (
	"net/http"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// historyRoutes 返回历史回查的三条路由行（业务面，作用域 `sessions:read`）。
// historyRoutes returns the three read-only business rows.
func (s *Server) historyRoutes() []route {
	return []route{
		{
			method: http.MethodGet, pattern: "/api/v1/sessions", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeSessionsRead),
			ingress: callers.IngressRead, principalHandler: s.handleListSessions,
		},
		{
			method: http.MethodGet, pattern: "/api/v1/history/search", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeSessionsRead),
			ingress: callers.IngressRead, principalHandler: s.handleHistorySearch,
		},
		{
			method: http.MethodGet, pattern: "/api/v1/sessions/{session_id}/messages/{message_id}",
			visibility: routeProtected, surface: surfaceRouteBusiness,
			scope:   fixedScope(core.ScopeSessionsRead),
			ingress: callers.IngressRead, principalHandler: s.handleReadMessage,
		},
	}
}

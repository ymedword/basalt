package web

// 本文件是知识库十个端点的**路由行**（US-16 / T042；端点清单以冻结契约
// `docs/contracts/openapi.yaml` 的知识组为准，按契约顺序排列）。四条口径：
//
//  1. **`GET /collections` 是管理面**：契约该端点没有 operation security，继承根级
//     `security`（bearerAuth / localTrust），因此它**不得**做成业务行——否则调用方
//     Key 就能列别人的集合。其余九个端点各自声明 `security`（callerKey / bearerAuth /
//     localTrust），才走业务面。
//  2. **作用域逐行声明**：读面 `knowledge:read`、写面 `knowledge:write`；作用域不足
//     由业务面中间件在到达 handler 之前 403（不解释缺哪个 scope）。
//  3. **入口类别逐行映射**（D-30 / T126）：摄取、软删、重建计 `IngressIngest`；列表、
//     检索、来源视图计 `IngressRead`。管理行**不得**映射——管理面不受调用方桶约束。
//  4. 七个写 / 摄取行里，契约点名 `IngressIngest` 的是摄取与重建；`POST /collections`
//     与两条软删契约没有点名类别，按"写操作计入摄取桶"归类（实现层裁决，记附录 R52）。
//
// This file enumerates the ten knowledge routes in contract order. GET /collections
// stays on the management face because the contract gives it no operation security;
// the other nine declare their scope and ingress class explicitly.

import (
	"net/http"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// knowledgeRoutes 返回知识库的十条路由行。管理行一条，业务行九条——业务行**必须**
// 同时声明 scope 与 ingress（缺任一项在启动自检失败）。
// knowledgeRoutes returns one management row and nine business rows; a business row
// without either scope or ingress fails the startup self-check.
func (s *Server) knowledgeRoutes() []route {
	return []route{
		// 管理面只读列表（契约无 operation security → 根级 bearerAuth / localTrust）。
		{
			method: http.MethodGet, pattern: "/api/v1/collections",
			visibility: routeProtected, adminHandler: s.handleListCollections,
		},
		// 写面：建集合与两条软删都计 `IngressIngest`（写操作）。
		{
			method: http.MethodPost, pattern: "/api/v1/collections", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeKnowledgeWrite),
			ingress: callers.IngressIngest, principalHandler: s.handleCreateCollection,
		},
		{
			method: http.MethodDelete, pattern: "/api/v1/collections/{id}", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeKnowledgeWrite),
			ingress: callers.IngressIngest, principalHandler: s.handleDeleteCollection,
		},
		// 读面：列表与版本列表计 `IngressRead`。
		{
			method: http.MethodGet, pattern: "/api/v1/collections/{id}/documents", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeKnowledgeRead),
			ingress: callers.IngressRead, principalHandler: s.handleListCollectionDocuments,
		},
		// 摄取（契约点名 `IngressIngest`）。
		{
			method: http.MethodPost, pattern: "/api/v1/collections/{id}/documents", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeKnowledgeWrite),
			ingress: callers.IngressIngest, principalHandler: s.handleIngestDocument,
		},
		{
			method: http.MethodDelete, pattern: "/api/v1/documents/{id}", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeKnowledgeWrite),
			ingress: callers.IngressIngest, principalHandler: s.handleDeleteDocument,
		},
		{
			method: http.MethodGet, pattern: "/api/v1/documents/{id}/versions", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeKnowledgeRead),
			ingress: callers.IngressRead, principalHandler: s.handleListDocumentVersions,
		},
		// 重建索引（契约点名 `IngressIngest`）。
		{
			method: http.MethodPost, pattern: "/api/v1/documents/{id}/reindex", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeKnowledgeWrite),
			ingress: callers.IngressIngest, principalHandler: s.handleReindexDocument,
		},
		// 检索与来源视图（契约点名 `IngressRead`）。
		{
			method: http.MethodPost, pattern: "/api/v1/search", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeKnowledgeRead),
			ingress: callers.IngressRead, principalHandler: s.handleSearch,
		},
		{
			method: http.MethodPost, pattern: "/api/v1/sources/view", visibility: routeProtected,
			surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeKnowledgeRead),
			ingress: callers.IngressRead, principalHandler: s.handleSourceView,
		},
	}
}

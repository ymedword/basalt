package web

import (
	"net/http"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

func (s *Server) flowRoutes() []route {
	return []route{
		{method: http.MethodGet, pattern: "/api/v1/admin/flow-agents", visibility: routeProtected, adminHandler: s.handleAdminFlowAgents},
		{method: http.MethodGet, pattern: "/api/v1/admin/flows/{uid}/versions/{version}", visibility: routeProtected, adminHandler: s.handleAdminFlowAuthorVersion},
		{method: http.MethodGet, pattern: "/api/v1/flows", visibility: routeProtected, surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeFlowsRead), ingress: callers.IngressRead, principalHandler: s.handleFlowDefinitions},
		{method: http.MethodGet, pattern: "/api/v1/flows/{name}", visibility: routeProtected, surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeFlowsRead), ingress: callers.IngressRead, principalHandler: s.handleFlowDefinition},
		{method: http.MethodPost, pattern: "/api/v1/flow-tasks", visibility: routeProtected, surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeFlowsWrite), ingress: callers.IngressRun, principalHandler: s.handleFlowStart},
		{method: http.MethodGet, pattern: "/api/v1/flow-tasks", visibility: routeProtected, surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeFlowsRead), ingress: callers.IngressRead, principalHandler: s.handleFlowTasks},
		{method: http.MethodGet, pattern: "/api/v1/flow-tasks/{id}", visibility: routeProtected, surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeFlowsRead), ingress: callers.IngressRead, principalHandler: s.handleFlowTask},
		{method: http.MethodGet, pattern: "/api/v1/flow-tasks/{id}/artifacts", visibility: routeProtected, surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeFlowsRead), ingress: callers.IngressRead, principalHandler: s.handleFlowArtifacts},
		{method: http.MethodGet, pattern: "/api/v1/flow-tasks/{id}/verifications", visibility: routeProtected, surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeFlowsRead), ingress: callers.IngressRead, principalHandler: s.handleFlowVerifications},
		{method: http.MethodPost, pattern: "/api/v1/flow-tasks/{id}/cancel", visibility: routeProtected, surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeFlowsWrite), ingress: callers.IngressRun, principalHandler: s.handleFlowCancel},
		{method: http.MethodPost, pattern: "/api/v1/flow-tasks/{id}/resume", visibility: routeProtected, surface: surfaceRouteBusiness, scope: fixedScope(core.ScopeFlowsWrite), ingress: callers.IngressRun, principalHandler: s.handleFlowResume},
		{method: http.MethodGet, pattern: "/api/v1/admin/flows", visibility: routeProtected, adminHandler: s.handleAdminFlowDefinitions},
		{method: http.MethodPost, pattern: "/api/v1/admin/flows/validate", visibility: routeProtected, adminHandler: s.handleAdminFlowValidate},
		{method: http.MethodPost, pattern: "/api/v1/admin/flows/publish", visibility: routeProtected, adminHandler: s.handleAdminFlowPublish},
		{method: http.MethodPost, pattern: "/api/v1/admin/flows/{uid}/retire", visibility: routeProtected, adminHandler: s.handleAdminFlowRetire},
		{method: http.MethodGet, pattern: "/api/v1/admin/flow-tasks", visibility: routeProtected, adminHandler: s.handleAdminFlowTasks},
		{method: http.MethodGet, pattern: "/api/v1/admin/flow-tasks/{id}", visibility: routeProtected, adminHandler: s.handleAdminFlowTask},
		{method: http.MethodPost, pattern: "/api/v1/admin/flow-tasks/{id}/abandon", visibility: routeProtected, adminHandler: s.handleAdminFlowAbandon},
	}
}

package web

// 本文件覆盖沙箱白名单只读端点（模块卡 §3.1.28 / §4.1 第 24 行，6 例）：四类白名单
// 原样投影、空配置输出四个 `[]`、宿主绝对路径与空路径**不**出站、认证闸门，以及
// 响应字段集与契约一致。本端点只读——修改白名单需改配置并重启（约束 29）。
// Sandbox allowlist acceptance: four lists projected verbatim, empty configuration as
// [], host-absolute/empty paths dropped, the auth gate, and the contract field set.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// newSandboxServer 装配带指定白名单投影的 handler 级服务。
func newSandboxServer(t *testing.T, cfg SandboxConfig) *Server {
	t.Helper()
	return newTestServer(t, func(o *Options) { o.Sandbox = cfg })
}

// TestSandbox_ProjectsFourAllowlists —— 四类白名单逐项、保序投影。
// TestSandbox_ProjectsFourAllowlists pins the four-list projection in order.
func TestSandbox_ProjectsFourAllowlists(t *testing.T) {
	cfg := SandboxConfig{
		FileAllowedPaths: []string{
			".basalt/agents/<agent>/",
			".basalt/skills/",
			".basalt/versions/skills/",
			".basalt/agents/crm-agent/",
		},
		ShellAllowedCommands: []string{"git", "npm"},
		ShellInterpreters:    []string{"bash", "python3"},
		HTTPAllowedDomains:   []string{"api.example.com", "hooks.example.com"},
	}
	s := newSandboxServer(t, cfg)
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/sandbox", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeResourcesEnvelope[SandboxView](t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Equal(t, cfg.FileAllowedPaths, env.Data.FileAllowedPaths, "相对展示路径必须保序原样投影")
	require.Equal(t, cfg.ShellAllowedCommands, env.Data.ShellAllowedCommands)
	require.Equal(t, cfg.ShellInterpreters, env.Data.ShellInterpreters)
	require.Equal(t, cfg.HTTPAllowedDomains, env.Data.HTTPAllowedDomains)
}

// TestSandbox_EmptyConfigRendersEmptyArrays —— 零值配置输出四个 `[]`，不出现 `null`
// （约束 34）；空配置的含义是"全拒"，不是"未配置"。
// TestSandbox_EmptyConfigRendersEmptyArrays pins the empty-array encoding.
func TestSandbox_EmptyConfigRendersEmptyArrays(t *testing.T) {
	s := newSandboxServer(t, SandboxConfig{})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/sandbox", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	for _, field := range []string{
		`"file_allowed_paths":[]`,
		`"shell_allowed_commands":[]`,
		`"shell_interpreters":[]`,
		`"http_allowed_domains":[]`,
	} {
		require.Contains(t, body, field)
	}
	require.NotContains(t, body, "null")

	env := decodeResourcesEnvelope[SandboxView](t, rec)
	require.NotNil(t, env.Data.FileAllowedPaths)
	require.Empty(t, env.Data.FileAllowedPaths)
}

// TestSandbox_NoAbsolutePathsLeak —— 宿主绝对路径（Unix / Windows 盘符 / 反斜杠根 /
// 盘符相对）一律不投影：宿主布局不属于对外契约，任何只读面不得泄露绝对路径。
// 逆改点：去掉 sandboxView 的 rootedPath 过滤 → 正文扫描断言红。
// TestSandbox_NoAbsolutePathsLeak pins that host-absolute shapes never leave the process.
func TestSandbox_NoAbsolutePathsLeak(t *testing.T) {
	s := newSandboxServer(t, SandboxConfig{FileAllowedPaths: []string{
		"/etc/passwd",
		`C:\basalt\agents\ops-agent`,
		`\Windows\System32`,
		"d:relative",
		".basalt/skills/",
	}})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/sandbox", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeResourcesEnvelope[SandboxView](t, rec)
	require.Equal(t, []string{".basalt/skills/"}, env.Data.FileAllowedPaths,
		"只保留相对展示名，且不改变顺序")

	body := rec.Body.String()
	for _, leaked := range []string{"etc/passwd", "Windows", `C:\basalt`, "d:relative"} {
		require.NotContains(t, body, leaked, "绝对 / 根植路径不得有任何出站载体：%s", leaked)
	}
}

// TestSandbox_EmptyPathsDropped —— 空串路径不投影（空白的"允许路径"在页面上会读成
// 一个可点击的目录）。
// TestSandbox_EmptyPathsDropped pins that empty path entries are dropped.
func TestSandbox_EmptyPathsDropped(t *testing.T) {
	s := newSandboxServer(t, SandboxConfig{FileAllowedPaths: []string{"", ".basalt/agents/<agent>/", ""}})
	env := decodeResourcesEnvelope[SandboxView](t, serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/sandbox", "")))

	require.Equal(t, []string{".basalt/agents/<agent>/"}, env.Data.FileAllowedPaths)
}

// TestSandbox_RequiresAuth —— 沙箱面在认证闸门之后：缺凭据与错凭据 401，正确凭据 200。
// 逆改点：路由 visibility 改成 routePublic → 401 断言红。
// TestSandbox_RequiresAuth pins the auth gate.
func TestSandbox_RequiresAuth(t *testing.T) {
	const token = "sandbox-token"
	// 每个凭据状态用新闸门：同一 60 秒窗口累计 10 次失败会转 429（FR4）。
	newServer := func() *Server {
		return newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(token)
			o.Sandbox = SandboxConfig{ShellAllowedCommands: []string{"git"}}
		})
	}

	rec := serveAdmin(newServer(), adminRequest(http.MethodGet, "/api/v1/sandbox", ""))
	require.Equal(t, http.StatusUnauthorized, rec.Code, "缺凭据必须 401")

	rec = serveAdmin(newServer(), withBearer(adminRequest(http.MethodGet, "/api/v1/sandbox", ""), "wrong-token"))
	require.Equal(t, http.StatusUnauthorized, rec.Code, "错凭据必须 401")

	rec = serveAdmin(newServer(), withBearer(adminRequest(http.MethodGet, "/api/v1/sandbox", ""), token))
	require.Equal(t, http.StatusOK, rec.Code, "正确凭据必须放行")
}

// TestSandbox_ViewShapeMatchesContract —— 真实 HTTP 载荷的字段集与契约 SandboxView
// 完全一致并过 schema（多一个内部路径字段或少一个数组字段都会红）。
// TestSandbox_ViewShapeMatchesContract pins the exact contract property set.
func TestSandbox_ViewShapeMatchesContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	s := newSandboxServer(t, SandboxConfig{
		FileAllowedPaths:     []string{".basalt/skills/"},
		ShellAllowedCommands: []string{"git"},
		ShellInterpreters:    []string{"bash"},
		HTTPAllowedDomains:   []string{"api.example.com"},
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/sandbox", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	var env resourcesEnvelope[map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	requireResourcePayloadMatches(t, doc, "SandboxView", env.Data)
}

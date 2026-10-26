package web

// 本文件覆盖只读资源面（模块卡 §4.1 的 handlers 表、§4.4 的 011-web-observability
// 的 TestResources_* 家族）：六条路由的列表 / 详情投影、ETag 头、404 固定文案、
// 500 不泄漏、缺失端口按装配错误拒绝、认证闸门与契约字段集。
// 本层只验证"投影与映射"；凭证值级脱敏（定义原文 → 视图）由
// internal/app 的 TestRunResources_ResourceViewsRedactCredentials 证明。
// The resource handlers' acceptance tests: projection, ETag header, fixed 404 text,
// non-leaking 500, missing-port rejection, auth, and contract property sets. Value-level
// credential redaction is proven in the app-layer tests.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// resourceHashA / resourceHashB 是两组小写十六进制内容哈希（形态同发布版本行）。
// The two content hashes look like published version rows.
const (
	resourceHashA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	resourceHashB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// resourcesEnvelope 是列表 / 详情共用的信封形态（data 的具体类型由调用方给定）。
// resourcesEnvelope is the shared envelope shape.
type resourcesEnvelope[T any] struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// fakeResources 是可编程的 ResourceReader：六个方法的返回值与错误互相独立，
// 便于逐端点构造 404 / 500 分支。
// fakeResources is a programmable ResourceReader with independent results per method.
type fakeResources struct {
	skills       []SkillView
	skill        SkillView
	skillsErr    error
	skillErr     error
	mcpServers   []McpServerView
	mcpServer    McpServerView
	mcpETag      string
	mcpArgs      []string
	mcpURL       string
	mcpListErr   error
	mcpErr       error
	modelConns   []ModelConnectionView
	modelConn    ModelConnectionView
	modelListErr error
	modelErr     error
}

func (f fakeResources) Skills(context.Context) ([]SkillView, error) {
	return f.skills, f.skillsErr
}

func (f fakeResources) Skill(context.Context, string) (SkillView, error) {
	return f.skill, f.skillErr
}

func (f fakeResources) MCPServers(context.Context) ([]McpServerView, error) {
	return f.mcpServers, f.mcpListErr
}

func (f fakeResources) McpServer(context.Context, string) (McpServerDetail, error) {
	return McpServerDetail{View: f.mcpServer, ETag: f.mcpETag, Args: f.mcpArgs, URL: f.mcpURL}, f.mcpErr
}

func (f fakeResources) ModelConnections(context.Context) ([]ModelConnectionView, error) {
	return f.modelConns, f.modelListErr
}

func (f fakeResources) ModelConnection(context.Context, string) (ModelConnectionView, error) {
	return f.modelConn, f.modelErr
}

// sampleSkillView / sampleMcpServerView / sampleModelConnectionView 是三份完整样本：
// 覆盖全部契约字段（含空 description 与非 nil 列表），供多数用例复用。
// The three samples cover every contract field so most cases can reuse them.
func sampleSkillView() SkillView {
	return SkillView{
		Name:         "pdf-extract",
		Description:  "",
		Path:         ".basalt/skills/pdf-extract",
		VersionID:    7,
		VersionNo:    2,
		ContentHash:  resourceHashA,
		ETag:         `"sha256:` + resourceHashA + `"`,
		FileCount:    3,
		ReferencedBy: []string{"run-1"},
	}
}

func sampleMcpServerView() McpServerView {
	return McpServerView{
		Name:      "files",
		Transport: "stdio",
		Command:   "npx",
		EnvKeys:   []string{"MCP_HOME", "MCP_TOKEN"},
		// 集合字段在载荷里必须是 `[]` 而不是 `null`；带内容的凭据键由
		// TestOpenAPI_McpServerViewPropertiesMatchContract 单独钉住。
		// Collection fields must render as `[]`; a populated credential key is pinned
		// separately by the view-property contract test.
		HeaderKeys:         []string{},
		CredentialKeys:     []McpCredentialKeyView{},
		Connected:          true,
		Tools:              []string{"read", "write"},
		PublishedVersionID: 3,
		PublishedVersionNo: 2,
		AppliedVersionID:   2,
		AppliedVersionNo:   1,
		PendingRestart:     true,
	}
}

func sampleModelConnectionView() ModelConnectionView {
	return ModelConnectionView{
		Name:               "openai",
		ResourceUID:        "rs_01J0000000000000000000000A",
		BaseURL:            "https://api.example.test/v1",
		APIKeyEnv:          "OPENAI_API_KEY",
		DefaultModel:       "gpt-test",
		CredentialMode:     "env",
		CredentialRef:      "",
		CredentialState:    "",
		CredentialVersion:  0,
		Last4:              "",
		Scope:              "shared",
		OwnerAgent:         "",
		Revision:           1,
		PublishedVersionID: 5,
		PublishedVersionNo: 1,
		AppliedVersionID:   5,
		AppliedVersionNo:   1,
		ETag:               `"sha256:` + resourceHashB + `"`,
		ReferencedBy:       []string{},
		// R7：本地就绪由管理读面补齐；这里给一个合法枚举值，契约 schema 才能判形状。
		Availability: core.AvailabilityReady,
	}
}

// TestResources_ListSkills 覆盖技能列表：顺序照搬适配层、空 description 不被补造、
// 空列表序列化为 `[]`。模块卡 §4.1 handlers 表「技能列表」。
// 逆改点：按名字重排 / 给 description 填技能名 / nil 列表 → 三条断言各自红。
func TestResources_ListSkills(t *testing.T) {
	first, second := sampleSkillView(), sampleSkillView()
	first.Name, first.Path = "alpha", ".basalt/skills/alpha"
	second.Name, second.Path = "pdf-extract", ".basalt/skills/pdf-extract"

	s := newTestServer(t, func(o *Options) {
		o.Resources = fakeResources{skills: []SkillView{first, second}}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/skills", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeResourcesEnvelope[[]SkillView](t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Len(t, env.Data, 2)
	require.Equal(t, "alpha", env.Data[0].Name, "顺序必须照搬适配层，不重排")
	require.Equal(t, "pdf-extract", env.Data[1].Name)
	require.Equal(t, "", env.Data[1].Description, "无来源的 description 保持空串，不得补造")
	require.Equal(t, ".basalt/skills/pdf-extract", env.Data[1].Path, "path 必须是逻辑路径")
	require.Equal(t, []string{"run-1"}, env.Data[1].ReferencedBy)
	require.NotContains(t, env.Data[1].ReferencedBy, "run-2")

	empty := newTestServer(t, func(o *Options) { o.Resources = fakeResources{skills: nil} })
	rec = serveAdmin(empty, adminRequest(http.MethodGet, "/api/v1/skills", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"data":[]`, "空列表必须是 []，不是 null")
}

// TestResources_GetSkill 覆盖技能详情：200 时 ETag 响应头与视图字段同源；
// 视图无 ETag 时不设置响应头（不伪造）。模块卡 §3.1.8 行 917。
// 逆改点：响应头另算一份哈希 / 空 ETag 也设头 → 两条断言各自红。
func TestResources_GetSkill(t *testing.T) {
	view := sampleSkillView()
	s := newTestServer(t, func(o *Options) { o.Resources = fakeResources{skill: view} })
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/skills/pdf-extract", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, view.ETag, rec.Header().Get("ETag"), "ETag 头必须与视图字段同源")

	env := decodeResourcesEnvelope[SkillView](t, rec)
	require.Equal(t, view, env.Data)
	require.Equal(t, `"sha256:`+resourceHashA+`"`, env.Data.ETag, "ETag 格式与内容 ETag 相同")

	noETag := newTestServer(t, func(o *Options) {
		o.Resources = fakeResources{skill: SkillView{Name: "bare"}}
	})
	rec = serveAdmin(noETag, adminRequest(http.MethodGet, "/api/v1/skills/bare", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get("ETag"), "无哈希时不得设置 ETag 头")
}

// TestResources_ListMCPServers 覆盖 MCP 列表：11 个契约字段、需重启标记、变量键名、
// 工具名与空列表形态。模块卡 §4.1「MCP 列表」（约束 29 / 35）。
// 逆改点：把 env 值投影进视图 / pending_restart 写死 false / nil 列表 → 各自红。
func TestResources_ListMCPServers(t *testing.T) {
	view := sampleMcpServerView()
	s := newTestServer(t, func(o *Options) {
		o.Resources = fakeResources{mcpServers: []McpServerView{view}}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/mcp-servers", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeResourcesEnvelope[[]McpServerView](t, rec)
	require.Len(t, env.Data, 1)
	got := env.Data[0]
	require.Equal(t, view, got)
	require.Equal(t, "npx", got.Command, "只回可执行名，不回参数")
	require.Equal(t, []string{"MCP_HOME", "MCP_TOKEN"}, got.EnvKeys, "只回变量键名")
	require.True(t, got.PendingRestart, "发布未生效必须标需重启")
	require.Equal(t, []string{"read", "write"}, got.Tools)

	empty := newTestServer(t, func(o *Options) { o.Resources = fakeResources{} })
	rec = serveAdmin(empty, adminRequest(http.MethodGet, "/api/v1/mcp-servers", ""))
	require.Contains(t, rec.Body.String(), `"data":[]`)
}

// TestResources_GetMCPServer 覆盖 MCP 详情与 ETag 响应头（人工 review ② F3）：
// 响应体按契约 `McpServerDetail` 输出视图 + `args` / `url`（2026-10-25 US-14 走查
// 裁决：此前只回视图，详情页“修改”表单读 `args` 崩溃）；头值来自端口返回的内部
// 结构，**只经响应头出站**，不得出现 etag 公开字段；无发布版本（未入账的配置项）
// 时端口给空串，此时不设头。
// 逆改点：不设头 / 把 etag 塞进响应体 / 空 ETag 也设头 / 丢 args、url → 各自红。
func TestResources_GetMCPServer(t *testing.T) {
	view := sampleMcpServerView()
	etag := `"sha256:` + resourceHashA + `"`
	s := newTestServer(t, func(o *Options) {
		o.Resources = fakeResources{
			mcpServer: view,
			mcpETag:   etag,
			mcpArgs:   []string{"--root", "docs"},
			mcpURL:    "",
		}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/mcp-servers/files", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, etag, rec.Header().Get("ETag"), "契约在详情声明了 ETag 头，必须从内部结构设置")

	env := decodeResourcesEnvelope[McpServerDetailView](t, rec)
	require.Equal(t, view, env.Data.McpServerView, "详情体必须在视图之上扩展，字段逐字保真")
	require.Equal(t, []string{"--root", "docs"}, env.Data.Args, "契约 McpServerDetail 必须回 args")
	require.Equal(t, "", env.Data.URL, "stdio 传输的 url 为空串")
	require.NotContains(t, rec.Body.String(), resourceHashA, "ETag 只经响应头出站，不得进响应体")
	require.NotContains(t, rec.Body.String(), `"etag"`, "不得为凑头新增契约外字段")
	require.NotContains(t, env.Data.EnvKeys, "MCP_TOKEN=secret")

	bare := newTestServer(t, func(o *Options) {
		o.Resources = fakeResources{mcpServer: McpServerView{Name: "bare"}}
	})
	rec = serveAdmin(bare, adminRequest(http.MethodGet, "/api/v1/mcp-servers/bare", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get("ETag"), "无发布版本时不得伪造 ETag 头")
	bareEnv := decodeResourcesEnvelope[McpServerDetailView](t, rec)
	require.NotNil(t, bareEnv.Data.Args, "无发布版本时 args 出站为 []，不得是 null")
	require.Empty(t, bareEnv.Data.Args)
}

// TestResources_ListModelConnections 覆盖模型连接列表：只回环境变量引用名，
// 不回值；空 referenced_by 序列化为 `[]`。
// 逆改点：把 api_key_env 换成解析后的值 / nil 列表 → 断言红。
func TestResources_ListModelConnections(t *testing.T) {
	view := sampleModelConnectionView()
	s := newTestServer(t, func(o *Options) {
		o.Resources = fakeResources{modelConns: []ModelConnectionView{view}}
	})
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/model-connections", ""))
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeResourcesEnvelope[[]ModelConnectionView](t, rec)
	require.Len(t, env.Data, 1)
	require.Equal(t, "OPENAI_API_KEY", env.Data[0].APIKeyEnv, "只回 ${ENV_VAR} 引用名")
	require.Equal(t, []string{}, env.Data[0].ReferencedBy, "空引用必须是 []")
	require.Contains(t, rec.Body.String(), `"referenced_by":[]`)

	empty := newTestServer(t, func(o *Options) { o.Resources = fakeResources{} })
	rec = serveAdmin(empty, adminRequest(http.MethodGet, "/api/v1/model-connections", ""))
	require.Contains(t, rec.Body.String(), `"data":[]`)
}

// TestResources_GetModelConnection 覆盖模型连接详情与 ETag 头（与技能同一口径）。
// 逆改点：详情不设 ETag 头 → 断言红。
func TestResources_GetModelConnection(t *testing.T) {
	view := sampleModelConnectionView()
	s := newTestServer(t, func(o *Options) { o.Resources = fakeResources{modelConn: view} })
	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/model-connections/openai", ""))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, view.ETag, rec.Header().Get("ETag"))

	env := decodeResourcesEnvelope[ModelConnectionView](t, rec)
	require.Equal(t, view, env.Data)
	require.Equal(t, `"sha256:`+resourceHashB+`"`, env.Data.ETag)
}

// TestResources_NotFound 覆盖三张详情表的 404 固定文案：回显资源名、不回显底层错误。
// 逆改点：把 err 文本拼进 message（泄漏路径）/ 任意一端点回 500 → 断言红。
func TestResources_NotFound(t *testing.T) {
	missing := func(kind string) error {
		return fmt.Errorf("%w: %s/ghost", core.ErrResourceNotFound, kind)
	}
	cases := []struct {
		name    string
		path    string
		opts    func(*Options)
		message string
	}{
		{
			name:    "技能",
			path:    "/api/v1/skills/ghost",
			opts:    func(o *Options) { o.Resources = fakeResources{skillErr: missing("skill")} },
			message: "未找到技能：ghost",
		},
		{
			name:    "MCP 服务",
			path:    "/api/v1/mcp-servers/ghost",
			opts:    func(o *Options) { o.Resources = fakeResources{mcpErr: missing("mcp_server")} },
			message: "未找到 MCP 服务：ghost",
		},
		{
			name:    "模型连接",
			path:    "/api/v1/model-connections/ghost",
			opts:    func(o *Options) { o.Resources = fakeResources{modelErr: missing("model_connection")} },
			message: "未找到模型连接：ghost",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, tc.opts)
			rec := serveAdmin(s, adminRequest(http.MethodGet, tc.path, ""))
			require.Equal(t, http.StatusNotFound, rec.Code)

			var env resourcesEnvelope[json.RawMessage]
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			require.Equal(t, codeNotFound, env.Code)
			require.Equal(t, tc.message, env.Message)
			require.NotContains(t, env.Message, "resource not found", "不回显底层错误文本")
			require.NotContains(t, env.Message, "ghost/ghost")
		})
	}
}

// TestResources_InternalErrors 覆盖六条路由的 500 分支：只回固定文案，
// 底层错误（含绝对路径）既不进响应体也不改变状态码。约束 24 / 37。
// 逆改点：把 err 写进 message / 只对部分端点生效 → 断言红。
func TestResources_InternalErrors(t *testing.T) {
	boom := errors.New(`open C:\workspace\private\basalt.db: access denied`)
	paths := []string{
		"/api/v1/skills",
		"/api/v1/skills/pdf-extract",
		"/api/v1/mcp-servers",
		"/api/v1/mcp-servers/files",
		"/api/v1/model-connections",
		"/api/v1/model-connections/openai",
	}
	s := newTestServer(t, func(o *Options) {
		o.Resources = fakeResources{
			skillsErr:    boom,
			skillErr:     boom,
			mcpListErr:   boom,
			mcpErr:       boom,
			modelListErr: boom,
			modelErr:     boom,
		}
	})
	for _, path := range paths {
		rec := serveAdmin(s, adminRequest(http.MethodGet, path, ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code, path)

		var env resourcesEnvelope[json.RawMessage]
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), path)
		require.Equal(t, codeInternal, env.Code, path)
		require.Equal(t, messageInternal, env.Message, path)
		body := rec.Body.String()
		require.NotContains(t, body, "private", path)
		require.NotContains(t, body, "basalt.db", path)
		require.NotContains(t, body, "access denied", path)
	}
}

// TestResources_NoSecrets 断言六条路由的响应体不回显请求凭据，也不出现
// 任何凭证形状的字段名或值：只有契约声明的引用名（*_env / env_keys）出站。
// 逆改点：把 Authorization 头回显进响应 / 给视图加 "api_key" 或 "env" 字段 → 红。
func TestResources_NoSecrets(t *testing.T) {
	const token = "s3cret"
	skill, mcp, model := sampleSkillView(), sampleMcpServerView(), sampleModelConnectionView()
	s := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(token)
		o.Resources = fakeResources{
			skills:     []SkillView{skill},
			skill:      skill,
			mcpServers: []McpServerView{mcp},
			mcpServer:  mcp,
			modelConns: []ModelConnectionView{model},
			modelConn:  model,
		}
	})
	paths := []string{
		"/api/v1/skills",
		"/api/v1/skills/pdf-extract",
		"/api/v1/mcp-servers",
		"/api/v1/mcp-servers/files",
		"/api/v1/model-connections",
		"/api/v1/model-connections/openai",
	}
	for _, path := range paths {
		rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, path, ""), token))
		require.Equal(t, http.StatusOK, rec.Code, path)
		body := rec.Body.String()
		require.NotContains(t, body, token, "%s 不得回显凭据", path)
		require.NotContains(t, body, `"api_key":`, "%s 不得出现值字段", path)
		require.NotContains(t, body, `"env":`, "%s 不得出现 env 值映射", path)
	}
}

// TestResources_MissingPort 断言未装配 Resources 端口时六条路由一律 500 INTERNAL：
// 不是 404、也不是空列表 200（空列表会被读成"确实没有资源"）。
// 逆改点：缺端口返回 [] / 返回 404 → 断言红。
func TestResources_MissingPort(t *testing.T) {
	// 缺省装配带空资源端口，本用例显式置 nil 以覆盖「端口未装配」。
	// The default assembly wires an empty resource port; this case nils it explicitly.
	s := newTestServer(t, func(o *Options) { o.Resources = nil })
	paths := []string{
		"/api/v1/skills",
		"/api/v1/skills/pdf-extract",
		"/api/v1/mcp-servers",
		"/api/v1/mcp-servers/files",
		"/api/v1/model-connections",
		"/api/v1/model-connections/openai",
	}
	for _, path := range paths {
		rec := serveAdmin(s, adminRequest(http.MethodGet, path, ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code, path)

		var env resourcesEnvelope[json.RawMessage]
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), path)
		require.Equal(t, codeInternal, env.Code, path)
		require.Equal(t, messageInternal, env.Message, path)
	}
}

// TestResources_RequiresAuth 断言六条路由都在认证闸门之后：缺凭据与错凭据 401，
// 正确凭据 200——资源面不是公开面（FR5 的公开闭集只有健康检查 / 静态壳 / 重定向）。
// 逆改点：任意一条路由的 visibility 写成 routePublic → 401 断言红。
func TestResources_RequiresAuth(t *testing.T) {
	const token = "s3cret"
	paths := []string{
		"/api/v1/skills",
		"/api/v1/skills/pdf-extract",
		"/api/v1/mcp-servers",
		"/api/v1/mcp-servers/files",
		"/api/v1/model-connections",
		"/api/v1/model-connections/openai",
	}
	for _, path := range paths {
		// 每个端点用新的闸门：同一 60 秒窗口内累计 10 次失败会转 429（FR4），
		// 复用同一个闸门会让后面的用例读到旧的失败计数而不是认证结果。
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(token)
			o.Resources = fakeResources{}
		})
		rec := serveAdmin(s, adminRequest(http.MethodGet, path, ""))
		require.Equal(t, http.StatusUnauthorized, rec.Code, "%s 缺凭据必须 401", path)

		rec = serveAdmin(s, withBearer(adminRequest(http.MethodGet, path, ""), "wrong-token"))
		require.Equal(t, http.StatusUnauthorized, rec.Code, "%s 错凭据必须 401", path)

		rec = serveAdmin(s, withBearer(adminRequest(http.MethodGet, path, ""), token))
		require.Equal(t, http.StatusOK, rec.Code, "%s 正确凭据必须放行", path)
	}
}

// TestResources_ViewShapesMatchContract 用真实 HTTP 载荷（不是结构体自 marshal）比对
// 契约字段集并过 schema：多一个字段（把持久形态直接序列化）或少一个字段都会红；
// 详情视图另按 SkillDetail / McpServerDetail / ModelConnectionDetail 校验——可选字段
// 缺失是契约允许的（I-60 的三处缺口由此显式钉住）。
// 逆改点：给视图加契约外字段 / 少投影某个必需字段 → 字段集或 schema 断言红。
func TestResources_ViewShapesMatchContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	skill, mcp, model := sampleSkillView(), sampleMcpServerView(), sampleModelConnectionView()
	s := newTestServer(t, func(o *Options) {
		o.Resources = fakeResources{
			skill:     skill,
			mcpServer: mcp,
			modelConn: model,
		}
	})

	cases := []struct {
		name   string
		path   string
		schema string
		detail string
	}{
		{"技能详情", "/api/v1/skills/pdf-extract", "SkillView", "SkillDetail"},
		// MCP 详情按契约回视图 + args / url（2026-10-25 US-14 走查裁决），字段集即
		// McpServerDetail——McpServerView 只钉列表。
		{"MCP 详情", "/api/v1/mcp-servers/files", "McpServerDetail", "McpServerDetail"},
		// 契约的模型连接详情直接复用 ModelConnectionView，没有独立 Detail schema。
		{"模型连接详情", "/api/v1/model-connections/openai", "ModelConnectionView", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAdmin(s, adminRequest(http.MethodGet, tc.path, ""))
			require.Equal(t, http.StatusOK, rec.Code)

			var env resourcesEnvelope[map[string]any]
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			requireResourcePayloadMatches(t, doc, tc.schema, env.Data)
			if tc.detail == "" {
				return
			}
			require.NoErrorf(t, compileObjectSchema(t, doc, tc.detail).Validate(env.Data),
				"%s 载荷必须通过 %s 校验（可选的明细字段缺失是允许的）", tc.name, tc.detail)
		})
	}
}

// decodeResourcesEnvelope 解析固定信封。
// decodeResourcesEnvelope decodes the fixed envelope.
func decodeResourcesEnvelope[T any](t *testing.T, rec *httptest.ResponseRecorder) resourcesEnvelope[T] {
	t.Helper()
	var env resourcesEnvelope[T]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env
}

// requireResourcePayloadMatches 同时钉住"字段集与契约完全一致"与"整对象过契约 schema"。
// requireResourcePayloadMatches pins the exact property set and schema validity.
func requireResourcePayloadMatches(t *testing.T, doc map[string]any, schemaName string, payload map[string]any) {
	t.Helper()
	keys := make(map[string]bool, len(payload))
	for key := range payload {
		keys[key] = true
	}
	require.Equal(t, contractPropertyNames(t, doc, schemaName), keys,
		"%s 的字段集必须与契约完全一致", schemaName)
	require.NoErrorf(t, compileObjectSchema(t, doc, schemaName).Validate(payload),
		"%s 载荷必须通过契约 schema 校验", schemaName)
}

// 编译期断言：fakeResources 实现被测端口——假件与端口漂移在编译期就红。
var _ ResourceReader = fakeResources{}

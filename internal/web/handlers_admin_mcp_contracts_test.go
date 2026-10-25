package web

// 本文件是 US-14 三份接口契约（D-01–D-10）的 web 层验收面（T017，check `mcp-contracts`）。
// 九条用例与模块卡 §4.5.4 / §4.5.5 及 D-01–D-10 逐条对号：
//
//	TestMCPCatalog_ReadOnly                —— FR1 / D-01：六条、按 id 升序、零写、零操作行、零进程
//	TestMCPDiagnostics_DoesNotStartProcess —— FR6 / D-02：诊断只读一遍只读端口，不触达测试端口
//	TestMCPContracts_CatalogResponse        —— D-01 / D-09 / D-10：载荷逐层过 McpCatalog schema
//	TestMCPContracts_DiagnosticsResponse    —— D-02：载荷过 McpServerDiagnosis schema（stdio / http 两态）
//	TestMCPContracts_DiagnosticsNotFound    —— D-02：404 沿用既有 NOT_FOUND 码，不回诊断对象
//	TestMCPContracts_TestResultFields       —— D-03：九个字段、tools 恒为数组、失败分类来自契约枚举
//	TestMCPContracts_TestEndpoint503And504  —— D-04：503 / 504 带 operation_id 且**不**返回结果对象
//	TestMCPContracts_ToolDisposition        —— D-06：tool_disposition 可为 null 但键必须在，tools 仍是 offered
//	TestMCPContracts_OperationTestDetail    —— D-07：`mcp_server.test` 详情固定七键，白名单仍是闭集
//
// 分层说明（与模块卡 §4.5.5 伪代码的偏差，已记 run-state §2）：web 层手上既没有
// `CommandRunner` 也没有进程，哨兵文件与"零子进程"的**进程级**判据由 app 层
// `TestMCPDiagnostics_NoProcessNoOperation` 承担；本层改判两件确实可判的事——请求
// 只触达只读端口（调用计数 + 未装配端口必然 500），以及 web 包源码根本不导入
// `os/exec`（结构性判据，见 requireWebSpawnsNoProcess）。硬在本层抄一份哨兵断言
// 只会得到一条恒真的空断言。
// The nine cases bind to the card's §4.5.4 / §4.5.5 and D-01–D-10. The process-level
// "starts no subprocess" proof lives in the app layer; this layer proves port-only
// access plus the structural fact that the web package never imports os/exec.
// 本文件对 `internal/catalog` 的导入**仅限测试**：判"真实内嵌快照能过契约 schema"，
// 生产代码里 web 包不依赖 catalog 包（目录由 app 层装配成只读端口）。
// The import of internal/catalog is test-only: the real embedded snapshot must pass the
// contract schema, while production web code depends on the port only.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/catalog"
	"github.com/ymedword/basalt/internal/core"
)

// mcpCatalogIDs 是目录六条配方的固定 id，顺序即契约 D-01 要求的升序。断言顺序而不是
// 只断言条数，才能让"按 id 升序"这条真的被判到。
// mcpCatalogIDs lists the six catalog recipes in the contract's ascending order.
var mcpCatalogIDs = []string{
	"brave-search", "dbhub", "fetch", "filesystem-readonly", "github", "playwright",
}

// seedMCPServers 是工作区里的 MCP 定义文件：目录与诊断用例都要证明自己没改它
// （逐字节比较，不判 mtime）。
// seedMCPServers is the workspace's MCP definition file; both cases prove they leave
// it byte-identical.
const seedMCPServers = "servers:\n" +
	"  - name: github\n" +
	"    transport: stdio\n" +
	"    command: github-mcp-server\n" +
	"    args:\n" +
	"      - stdio\n"

// fakeMCPCatalogReader 是只读目录端口的替身：`calls` 记录被读了几次，用来判"一次请求
// 只读一遍目录"。真实数据由 catalog.Builtin() 提供，替身不伪造内容。
// fakeMCPCatalogReader counts reads so a case can prove the request touched the
// read-only catalog port exactly once.
type fakeMCPCatalogReader struct {
	cat   core.MCPCatalog
	err   error
	calls int
}

// ListMCPCatalog 返回预置快照并计数。
func (f *fakeMCPCatalogReader) ListMCPCatalog(context.Context) (core.MCPCatalog, error) {
	f.calls++
	if f.err != nil {
		return core.MCPCatalog{}, f.err
	}
	return f.cat, nil
}

// mcpContractServer 是 D-01–D-10 的用例装配：真实认证闸门 + 注入资源端口 + 真实只读
// 目录快照 + 含 basalt.yaml / mcp_servers.yaml 的临时工作区（工作区用来判"零写入"）。
// mcpContractServer wires the real handler chain over an injected resource port, the
// real read-only catalog snapshot and a temp workspace for the zero-write assertions.
type mcpContractServer struct {
	*adminServer
	reader *fakeMCPCatalogReader
}

// newMCPContractServer 装配夹具；`tune` 覆盖资源端口上用例关心的那一个方法（未覆盖的
// 方法保持"意外调用即报错"，因此 200 本身就证明请求没有触达它们）。
// newMCPContractServer assembles the fixture; untuned port methods still report an
// unexpected call, so a 200 alone proves the request never reached them.
func newMCPContractServer(t *testing.T, tune func(*fakeResourceAdminService)) *mcpContractServer {
	t.Helper()
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "basalt.yaml"), []byte(seedConfig), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "mcp_servers.yaml"), []byte(seedMCPServers), 0o600))

	builtin, err := catalog.Builtin()
	require.NoError(t, err)
	reader := &fakeMCPCatalogReader{cat: builtin}
	fake := &fakeResourceAdminService{}
	if tune != nil {
		tune(fake)
	}
	srv := &adminServer{workspace: workspace, ledger: &operationLedger{}, fake: fake}
	srv.Server = newTestServer(t, func(o *Options) {
		o.AdminAuth = resourceAuth{}
		o.ResourceAdminService = fake
		o.MCPCatalog = reader
	})
	return &mcpContractServer{adminServer: srv, reader: reader}
}

// requireWebSpawnsNoProcess 是本层对"目录 / 诊断不启动进程"的结构性判据：web 包的非
// 测试源码不得导入 `os/exec`——没有 exec 就没有子进程。它不替代 app 层的进程级判据，
// 只是把"这一层根本没有启动进程的能力"钉成可执行断言。
// requireWebSpawnsNoProcess asserts that no non-test source file in this package
// imports os/exec, so this layer cannot spawn a process at all.
func requireWebSpawnsNoProcess(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		require.NotContainsf(t, string(data), `"os/exec"`,
			"%s 导入了 os/exec：web 层不得启动子进程", name)
		scanned++
	}
	require.NotZero(t, scanned, "没有扫到任何生产源码，判据失效")
}

// decodeMCPEnvelope 解出信封并要求成功码，返回 data 原文。
// decodeMCPEnvelope requires the OK code and returns the raw data payload.
func decodeMCPEnvelope(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var env resourceEnvelope
	require.NoError(t, json.Unmarshal(body, &env))
	require.Equal(t, codeOK, env.Code, "message=%s", env.Message)
	require.NotEmpty(t, env.Data, "成功响应必须带 data")
	return env.Data
}

// TestMCPCatalog_ReadOnly 判 FR1 / D-01 的"只读"四件套：六条配方、按 id 升序、
// 一次请求只读一遍目录端口、零操作行、工作区逐字节不变；外加结构性的零进程判据。
// 逆改点：目录端点顺手写文件 / 写操作行 / 读两遍端口 / 顺序被重排 → 对应用例红。
// TestMCPCatalog_ReadOnly proves the catalog endpoint is read-only end to end.
func TestMCPCatalog_ReadOnly(t *testing.T) {
	env := newMCPContractServer(t, func(fake *fakeResourceAdminService) {
		// 未装配的方法保持 errFakeResourceAdminUnused：任何触达都会变成 500。
		require.Nil(t, fake.upsertMCPServerFn)
	})
	before := env.snapshotWorkspace(t)

	rec := serveAdmin(env.Server, resourceCall(http.MethodGet, "/api/v1/catalog/mcp", ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var view MCPCatalogView
	require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &view))
	require.Equal(t, 1, view.Version, "目录 schema 版本当前为 1")
	require.Len(t, view.Items, len(mcpCatalogIDs), "首批目录固定六条")

	ids := make([]string, 0, len(view.Items))
	for _, item := range view.Items {
		ids = append(ids, item.ID)
		// D-09 验证四件套：`verified=false` 时说明非空、时间与平台为空。
		require.False(t, item.Verified, "%s 尚未实测，不得标已验证", item.ID)
		require.NotEmpty(t, item.VerificationNote, "%s 的验证说明必须写清待实测项", item.ID)
		require.Empty(t, item.VerifiedAt, "%s 未实测时验证时间必须为空", item.ID)
		require.Empty(t, item.VerifiedPlatforms, "%s 未实测时验证平台必须为空", item.ID)
		// 目录 warnings 是固定 code 集合（D-10 的目录一半）：不得返回自由文本。
		for _, warning := range item.Warnings {
			require.Contains(t, catalogWarningCodes, warning, "%s 的 warnings 出现了集合外文本", item.ID)
		}
	}
	require.Equal(t, mcpCatalogIDs, ids, "目录必须按 id 升序")

	require.Equal(t, 1, env.reader.calls, "一次请求只读一遍目录")
	require.Empty(t, env.operations(t), "只读目录不得写管理操作行")
	require.True(t, env.equalWorkspace(t, before), "只读目录不得改动工作区（含 mcp_servers.yaml）")
	requireWebSpawnsNoProcess(t)
}

// catalogWarningCodes 是契约 McpCatalogEntry.warnings 的固定集合（D-10 目录一半）。
// 与诊断集合**各自独立**，不得合并枚举。
// catalogWarningCodes is the closed catalog warning set, independent of the diagnosis set.
var catalogWarningCodes = []string{
	"read_only_tool_face_required",
	"server_write_capability",
	"write_tool_name_collision",
	"browser_required",
	"sqlite_sample_only",
	"package_entry_unverified",
	"stdio_child_egress_unconstrained",
}

// TestMCPDiagnostics_DoesNotStartProcess 判 FR6 / D-02：诊断只走诊断端口一次，不触达
// 测试端口（证明诊断不预判测试，FR12），不回任何环境变量值，且零操作行、工作区不变。
// 逆改点：诊断顺手调测试端口 / 回显 env 值 / 写操作行 / 改文件 → 对应用例红。
// TestMCPDiagnostics_DoesNotStartProcess proves the diagnosis handler only reads the
// read-only port: exactly one call, no test-port call, nothing mutated.
func TestMCPDiagnostics_DoesNotStartProcess(t *testing.T) {
	diagnoses := 0
	env := newMCPContractServer(t, func(fake *fakeResourceAdminService) {
		fake.diagnoseMCPServerFn = func(_ context.Context, _ core.Actor, name string) (core.MCPServerDiagnosis, error) {
			diagnoses++
			require.Equal(t, "github", name, "诊断必须用于路径里的 server")
			return core.MCPServerDiagnosis{
				Name:               "github",
				Transport:          "stdio",
				PublishedVersionID: 7,
				AppliedVersionID:   6,
				PendingRestart:     true,
				Command: &core.MCPCommandDiagnosis{
					State:  "unconfirmed",
					Path:   "github-mcp-server",
					Reason: "script",
				},
				Env:       []core.MCPEnvDiagnosis{{Key: "GITHUB_PERSONAL_ACCESS_TOKEN", Set: true}},
				Warnings:  []string{"stdio_child_egress_unconstrained", "package_entry_unverified"},
				CheckedAt: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
			}, nil
		}
		fake.testMCPServerFn = func(context.Context, core.Actor, string) (core.ResourceTestResult, error) {
			t.Error("诊断请求不得触达测试端口：诊断与测试互不预判（FR12）")
			return core.ResourceTestResult{}, errFakeResourceAdminUnused
		}
	})
	before := env.snapshotWorkspace(t)

	rec := serveAdmin(env.Server, resourceCall(http.MethodGet, "/api/v1/mcp-servers/github/diagnostics", ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var view McpServerDiagnosticsView
	require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &view))
	require.Equal(t, 1, diagnoses, "诊断端口只读一次")
	require.NotNil(t, view.Command, "stdio 必须给出命令诊断对象")
	require.NotEmpty(t, view.Command.State, "命令定位三态必须给出取值")

	// env 只回 key / set：把 env 段按原始 JSON 解成映射，逐项判键集合恰好两项。
	var raw struct {
		Env []map[string]any `json:"env"`
	}
	require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &raw))
	require.Len(t, raw.Env, 1)
	for _, entry := range raw.Env {
		keys := make([]string, 0, len(entry))
		for key := range entry {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		require.Equal(t, []string{"key", "set"}, keys, "env 只允许回变量名与就绪布尔")
	}

	require.Empty(t, env.operations(t), "诊断不得写管理操作行")
	require.True(t, env.equalWorkspace(t, before), "诊断不得改动工作区")
	requireWebSpawnsNoProcess(t)
}

// TestMCPContracts_CatalogResponse 判 D-01 / D-09 / D-10 的响应形状：整份目录、每条
// 条目、每个 env 占位都要过契约 schema（字段集恰好相等 + schema 校验含 format）。
// 逆改点：少投影 / 多投影字段、warnings 出现集合外 code、placeholder 与 key 不一致
// → 对应用例红。
// TestMCPContracts_CatalogResponse validates the whole catalog and every nested object
// against the contract schemas.
func TestMCPContracts_CatalogResponse(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	env := newMCPContractServer(t, nil)

	rec := serveAdmin(env.Server, resourceCall(http.MethodGet, "/api/v1/catalog/mcp", ""))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var view MCPCatalogView
	require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &view))
	requireViewMatchesSchema(t, doc, "McpCatalog", view)
	for _, item := range view.Items {
		requireViewMatchesSchema(t, doc, "McpCatalogEntry", item)
		require.NotEmpty(t, item.Requires, "%s 的 requires 不得为空（contract minItems 1）", item.ID)
		require.NotEmpty(t, item.SourceURL, "%s 的 source_url 必须给出", item.ID)
		for _, placeholder := range item.Env {
			requireViewMatchesSchema(t, doc, "McpCatalogEnv", placeholder)
			require.Equal(t, "${"+placeholder.Key+"}", placeholder.Placeholder,
				"%s 的占位符必须与 key 一致", item.ID)
		}
	}
}

// TestMCPContracts_DiagnosticsResponse 判 D-02 的响应形状：stdio 带命令对象、http 为
// `null`；两态整对象都要过契约 schema（含 checked_at 的 date-time 格式与两套 warnings
// 各自的闭集）。
// TestMCPContracts_DiagnosticsResponse validates both diagnosis shapes against the
// contract schema.
func TestMCPContracts_DiagnosticsResponse(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	t.Run("stdio 带命令对象", func(t *testing.T) {
		env := newMCPContractServer(t, func(fake *fakeResourceAdminService) {
			fake.diagnoseMCPServerFn = func(context.Context, core.Actor, string) (core.MCPServerDiagnosis, error) {
				return core.MCPServerDiagnosis{
					Name:               "github",
					Transport:          "stdio",
					PublishedVersionID: 7,
					AppliedVersionID:   7,
					PendingRestart:     false,
					Command: &core.MCPCommandDiagnosis{
						State:  "located",
						Path:   "/usr/local/bin/github-mcp-server",
						Reason: "absolute",
					},
					Env: []core.MCPEnvDiagnosis{
						{Key: "GITHUB_PERSONAL_ACCESS_TOKEN", Set: true},
						{Key: "GITHUB_HOST", Set: false},
					},
					Warnings:  []string{"stdio_child_egress_unconstrained"},
					CheckedAt: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
				}, nil
			}
		})

		rec := serveAdmin(env.Server, resourceCall(http.MethodGet, "/api/v1/mcp-servers/github/diagnostics", ""))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

		var view McpServerDiagnosticsView
		require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &view))
		requireViewMatchesSchema(t, doc, "McpServerDiagnosis", view)
		require.NotNil(t, view.Command)
		requireViewMatchesSchema(t, doc, "McpCommandDiagnosis", *view.Command)
		require.Len(t, view.Env, 2)
		for _, entry := range view.Env {
			requireViewMatchesSchema(t, doc, "McpEnvDiagnosis", entry)
		}
		require.Equal(t, "2026-10-25T08:00:00Z", view.CheckedAt, "checked_at 必须是 RFC3339 UTC")
	})

	t.Run("http 的命令对象为 null", func(t *testing.T) {
		env := newMCPContractServer(t, func(fake *fakeResourceAdminService) {
			fake.diagnoseMCPServerFn = func(context.Context, core.Actor, string) (core.MCPServerDiagnosis, error) {
				return core.MCPServerDiagnosis{
					Name:               "remote",
					Transport:          "http",
					PublishedVersionID: 2,
					AppliedVersionID:   2,
					Command:            nil,
					Env:                nil,
					Warnings:           []string{"remote_mcp_downstream_unconstrained"},
					CheckedAt:          time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
				}, nil
			}
		})

		rec := serveAdmin(env.Server, resourceCall(http.MethodGet, "/api/v1/mcp-servers/remote/diagnostics", ""))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

		var view McpServerDiagnosticsView
		require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &view))
		requireViewMatchesSchema(t, doc, "McpServerDiagnosis", view)
		require.Nil(t, view.Command, "http 传输不得给命令诊断对象")
		require.NotNil(t, view.Env, "空 env 必须出站为 []")
		require.Empty(t, view.Env)
	})
}

// TestMCPContracts_DiagnosticsNotFound 判 D-02 的 404：沿用既有 NOT_FOUND 码（不新造
// 诊断专用码），且响应里**没有**诊断对象——页面据码走"资源不存在"，不得把空对象当成
// 一份诊断。
// TestMCPContracts_DiagnosticsNotFound pins the existing NOT_FOUND code and the absence
// of a diagnosis object on a miss.
func TestMCPContracts_DiagnosticsNotFound(t *testing.T) {
	env := newMCPContractServer(t, func(fake *fakeResourceAdminService) {
		fake.diagnoseMCPServerFn = func(_ context.Context, _ core.Actor, name string) (core.MCPServerDiagnosis, error) {
			return core.MCPServerDiagnosis{}, fmt.Errorf("%w: mcp_server/%s", core.ErrResourceNotFound, name)
		}
	})

	rec := serveAdmin(env.Server, resourceCall(http.MethodGet, "/api/v1/mcp-servers/ghost/diagnostics", ""))
	require.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())

	var envelope resourceEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, codeNotFound, envelope.Code)
	require.Empty(t, envelope.Data, "404 不得带诊断对象")
	require.NotContains(t, rec.Body.String(), `"command"`, "404 不得泄漏诊断字段")
}

// TestMCPContracts_TestResultFields 判 D-03：200 的测试结果对象九个字段齐全（含
// `tools` 恒为数组——空工具清单仍是成功），且 `stage` / `failure_class` 取值来自契约
// 枚举而不是自由文本。
// 逆改点：tools 为空时输出 null / 少投影 operation_id 或 tested_version_id / 写出
// 枚举外的分类 → 对应用例红。
// TestMCPContracts_TestResultFields pins the nine result fields and the empty-tools shape.
func TestMCPContracts_TestResultFields(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	stages := contractEnum(t, doc, "McpServerTestResult", "stage")
	classes := contractEnum(t, doc, "McpServerTestResult", "failure_class")

	t.Run("成功且有空工具清单", func(t *testing.T) {
		env := newMCPContractServer(t, func(fake *fakeResourceAdminService) {
			fake.testMCPServerFn = func(context.Context, core.Actor, string) (core.ResourceTestResult, error) {
				return core.ResourceTestResult{
					OK:              true,
					LatencyMS:       12,
					Tools:           nil,
					OperationID:     "op-mcp-test",
					TestedVersionID: 7,
					Stage:           "complete",
					FailureClass:    "",
				}, nil
			}
		})

		rec := serveAdmin(env.Server, resourceCall(http.MethodPost, "/api/v1/mcp-servers/github/test", ""))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

		payload := decodeMCPEnvelope(t, rec.Body.Bytes())
		var view McpServerTestResultView
		require.NoError(t, json.Unmarshal(payload, &view))
		requireViewMatchesSchema(t, doc, "McpServerTestResult", view)

		require.True(t, view.OK)
		require.Equal(t, "op-mcp-test", view.OperationID, "operation_id 必须回传")
		require.Equal(t, int64(7), view.TestedVersionID, "被测试版本必须回传")
		require.Contains(t, stages, view.Stage, "stage 必须是契约枚举取值")
		require.Equal(t, "complete", view.Stage)
		require.Contains(t, classes, view.FailureClass, "failure_class 必须是契约枚举取值")
		require.Empty(t, view.FailureClass, "成功时 failure_class 为空串")

		// 空工具清单必须是 `[]`：`null` 会被 SDK 反序列化判成"字段缺失"。
		var raw map[string]any
		require.NoError(t, json.Unmarshal(payload, &raw))
		tools, ok := raw["tools"].([]any)
		require.True(t, ok, "tools 必须是数组，实际 %T", raw["tools"])
		require.Empty(t, tools)
	})

	t.Run("失败时仍返回结果对象", func(t *testing.T) {
		env := newMCPContractServer(t, func(fake *fakeResourceAdminService) {
			fake.testMCPServerFn = func(context.Context, core.Actor, string) (core.ResourceTestResult, error) {
				return core.ResourceTestResult{
					OK:              false,
					LatencyMS:       30,
					Tools:           []string{},
					ErrorCode:       "connect_failed",
					ErrorMessage:    "连接被拒绝",
					OperationID:     "op-mcp-fail",
					TestedVersionID: 7,
					Stage:           "initialize",
					FailureClass:    "handshake_failed",
				}, nil
			}
		})

		rec := serveAdmin(env.Server, resourceCall(http.MethodPost, "/api/v1/mcp-servers/github/test", ""))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

		var view McpServerTestResultView
		require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &view))
		requireViewMatchesSchema(t, doc, "McpServerTestResult", view)
		require.False(t, view.OK, "连接失败记 ok=false 而操作仍是 success（200）")
		require.Contains(t, classes, view.FailureClass)
		require.Equal(t, "handshake_failed", view.FailureClass)
		require.Contains(t, stages, view.Stage)
		require.Equal(t, "initialize", view.Stage)
	})
}

// TestMCPContracts_TestEndpoint503And504 判 D-04：env 缺失 → 503、超时 → 504，两者都
// 携带 `data.operation_id`（页面据此拉操作详情读 stage / failure_class），且**不**返回
// 测试结果对象——`ok` / `tools` 一个都不能出现。
// 逆改点：503 / 504 里带结果对象、或丢掉 operation_id → 对应用例红。
// TestMCPContracts_TestEndpoint503And504 pins the two failure envelopes from D-04.
func TestMCPContracts_TestEndpoint503And504(t *testing.T) {
	cases := []struct {
		name    string
		failure error
		status  int
		code    string
		opID    string
	}{
		{
			name: "env 缺失",
			failure: &core.AdminError{
				Kind:    core.ErrProviderNotConfigured,
				Message: "MCP server github 缺少环境变量：GITHUB_PERSONAL_ACCESS_TOKEN",
				Data:    map[string]any{"operation_id": "op-env-missing"},
			},
			status: http.StatusServiceUnavailable,
			code:   codeNotConfigured,
			opID:   "op-env-missing",
		},
		{
			name: "测试超时",
			failure: &core.AdminError{
				Kind:    context.DeadlineExceeded,
				Message: "MCP 连接测试超时",
				Data:    map[string]any{"operation_id": "op-timeout"},
			},
			status: http.StatusGatewayTimeout,
			code:   codeTimeout,
			opID:   "op-timeout",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			failure := testCase.failure
			env := newMCPContractServer(t, func(fake *fakeResourceAdminService) {
				fake.testMCPServerFn = func(context.Context, core.Actor, string) (core.ResourceTestResult, error) {
					return core.ResourceTestResult{OK: false}, failure
				}
			})

			rec := serveAdmin(env.Server, resourceCall(http.MethodPost, "/api/v1/mcp-servers/github/test", ""))
			require.Equal(t, testCase.status, rec.Code, "body=%s", rec.Body.String())

			var envelope struct {
				Code string         `json:"code"`
				Data map[string]any `json:"data"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
			require.Equal(t, testCase.code, envelope.Code)
			require.Equal(t, testCase.opID, envelope.Data["operation_id"],
				"503 / 504 必须带 operation_id（页面据此查操作详情）")
			for _, forbidden := range []string{"ok", "tools", "latency_ms", "stage", "failure_class"} {
				require.NotContains(t, envelope.Data, forbidden,
					"%s 不得返回测试结果对象字段", testCase.name)
			}
		})
	}
}

// TestMCPContracts_ToolDisposition 判 D-06：`tool_disposition` 键恒在、未连接时为 null；
// `tools` 仍是该 server 提供的工具名（offered），不因注册结果改变语义。
// offer / accept / skip 的算术与"内置赢家"归属由 app 层
// `TestMCPToolDisposition_*` 判卷，本层只判投影与契约形状。
// TestMCPContracts_ToolDisposition pins the always-present key, the null case and the
// unchanged semantics of tools.
func TestMCPContracts_ToolDisposition(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	t.Run("已连接时给出 offer / accept / skip", func(t *testing.T) {
		view := sampleMcpServerView()
		disposition := &MCPToolDispositionView{
			Offered:  3,
			Accepted: 2,
			Skipped: []MCPToolSkipView{
				{Name: "read_file", WinnerSource: "builtin", Reason: "duplicate_name"},
			},
		}
		view.ToolDisposition = disposition
		srv := newTestServer(t, func(o *Options) {
			o.AdminAuth = resourceAuth{}
			o.Resources = fakeResources{mcpServer: view}
		})

		rec := serveAdmin(srv, resourceCall(http.MethodGet, "/api/v1/mcp-servers/files", ""))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

		var got McpServerView
		require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &got))
		requireViewMatchesSchema(t, doc, "McpServerView", got)
		require.NotNil(t, got.ToolDisposition, "已连接时必须给出注册结果")
		requireViewMatchesSchema(t, doc, "McpToolDisposition", *got.ToolDisposition)
		require.Equal(t, 3, got.ToolDisposition.Offered)
		require.Equal(t, 2, got.ToolDisposition.Accepted)
		require.Len(t, got.ToolDisposition.Skipped, 1)
		requireViewMatchesSchema(t, doc, "McpToolSkip", got.ToolDisposition.Skipped[0])
		require.Equal(t, "builtin", got.ToolDisposition.Skipped[0].WinnerSource)
		// `tools` 是 offered 名单，不因为注册结果被改写（D-06）。
		require.Equal(t, []string{"read", "write"}, got.Tools)
	})

	t.Run("未连接时为 null 但键必须存在", func(t *testing.T) {
		view := sampleMcpServerView()
		view.Connected = false
		view.Tools = []string{}
		srv := newTestServer(t, func(o *Options) {
			o.AdminAuth = resourceAuth{}
			o.Resources = fakeResources{mcpServer: view}
		})

		rec := serveAdmin(srv, resourceCall(http.MethodGet, "/api/v1/mcp-servers/files", ""))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		require.Contains(t, rec.Body.String(), `"tool_disposition":null`,
			"未连接时必须是显式 null 而不是缺键")

		var got McpServerView
		require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &got))
		requireViewMatchesSchema(t, doc, "McpServerView", got)
		require.Nil(t, got.ToolDisposition)
	})
}

// TestMCPContracts_OperationTestDetail 判 D-07：`mcp_server.test` 的操作详情是固定七
// 字段（ok / latency_ms / tools / error_code / error_message / stage / failure_class），
// 且白名单仍是**闭集**——请求原文、命令与 env 值一律不出站；`stage` / `failure_class`
// 只接受契约枚举取值；版本以 `requested_version_id` 为准，详情不得重复版本。
// 逆改点：白名单漏掉七键之一 / 放宽 stage 或 failure_class / 让未列出键通过 → 红。
// TestMCPContracts_OperationTestDetail pins the fixed seven-key detail and the closed
// whitelist, including the enum checks on stage and failure_class.
func TestMCPContracts_OperationTestDetail(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	stages := contractEnum(t, doc, "McpServerTestResult", "stage")
	classes := contractEnum(t, doc, "McpServerTestResult", "failure_class")

	serveDetail := func(t *testing.T, detailJSON string) map[string]any {
		t.Helper()
		fake := &operationFake{detail: core.OperationRecord{
			OperationID:        "op-detail",
			Action:             core.ActionMCPServerTest,
			TargetKind:         "mcp_server",
			TargetID:           "github",
			Status:             core.OperationSuccess,
			RequestedVersionID: 7,
			DetailJSON:         []byte(detailJSON),
			CreatedAt:          time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
			FinishedAt:         time.Date(2026, 10, 25, 8, 0, 5, 0, time.UTC),
		}}
		rec := serveAdmin(newOperationServer(t, fake), resourceCall(http.MethodGet, "/api/v1/operations/op-detail", ""))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

		var view OperationDetailView
		require.NoError(t, json.Unmarshal(decodeMCPEnvelope(t, rec.Body.Bytes()), &view))
		require.Equal(t, core.ActionMCPServerTest, view.Action)
		require.Equal(t, int64(7), view.RequestedVersionID, "版本以 requested_version_id 为准")
		return view.Detail
	}

	t.Run("七键齐全且不含请求原文", func(t *testing.T) {
		detail := serveDetail(t, `{`+
			`"ok":true,`+
			`"latency_ms":12,`+
			`"tools":["list_issues","create_issue"],`+
			`"error_code":"",`+
			`"error_message":"",`+
			`"stage":"complete",`+
			`"failure_class":"",`+
			// 以下三项都不在白名单：请求原文、命令、env 值。
			`"command":"/usr/local/bin/github-mcp-server",`+
			`"env":{"GITHUB_PERSONAL_ACCESS_TOKEN":"secret-value"},`+
			`"version":7`+
			`}`)

		require.Len(t, detail, 7, "详情必须恰好七键（白名单是闭集）")
		require.Equal(t, true, detail["ok"])
		// 详情经 JSON 往返到页面：整数值在 `map[string]any` 里是 float64，
		// 因此按数值比较（`EqualValues`），而不是断言 Go 侧的 int64 类型。
		require.EqualValues(t, 12, detail["latency_ms"])
		// 同一次 JSON 往返：数组在 `map[string]any` 里是 `[]any`，元素才是字符串。
		require.Equal(t, []any{"list_issues", "create_issue"}, detail["tools"])
		require.Equal(t, "", detail["error_code"])
		require.Equal(t, "", detail["error_message"])
		require.Equal(t, "complete", detail["stage"])
		require.Equal(t, "", detail["failure_class"], "成功时 failure_class 是空串而不是缺键")
		require.Contains(t, stages, detail["stage"])
		require.Contains(t, classes, detail["failure_class"])
		for _, forbidden := range []string{"command", "env", "version", "tested_version_id"} {
			require.NotContains(t, detail, forbidden, "%s 不得进入详情", forbidden)
		}
	})

	t.Run("非法取值整键丢弃而不是放行", func(t *testing.T) {
		detail := serveDetail(t, `{`+
			`"ok":"yes",`+
			`"latency_ms":-1,`+
			`"tools":["list_issues",7],`+
			`"stage":"bogus",`+
			`"failure_class":"not_a_class",`+
			// `\n` 在原始字符串里是**转义序列**：JSON 解析后得到真正的换行符，
			// 用来验证含控制字符的文案整键丢弃（写成裸换行会让整段 JSON 不可解析）。
			`"error_code":"bad\ncode",`+
			`"error_message":"凭据校验失败"`+
			`}`)

		require.Equal(t, map[string]any{"error_message": "凭据校验失败"}, detail,
			"类型或枚举不符的键必须整键丢弃，只有干净文案留下")
	})
}

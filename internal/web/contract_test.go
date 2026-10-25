package web

// 本文件是契约层验收（模块卡 §4.1 / W2）：信封形态、时间戳与 Content-Type、未知路由、
// 错误类型映射、视图字段形态。错误映射全部按 `errors.As` 分支判定——改一句错误文案
// 不得改变状态码（约束 22）。
// Contract-level acceptance: envelope shape, timestamp/content type, unknown routes,
// type-based error mapping, and view field shapes.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestContract_EnvelopeShape 验证成功含 data、失败省略 data（约束 3）。
// 逆改点：信封字段改名、错误响应带 data、Content-Type 改成 text/plain → 红。
func TestContract_EnvelopeShape(t *testing.T) {
	s := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	var ok map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ok))
	require.Equal(t, "OK", ok["code"])
	require.Equal(t, "success", ok["message"])
	require.Equal(t, "2026-10-18T12:00:00Z", ok["timestamp"], "timestamp 固定 RFC3339 UTC")
	require.Contains(t, ok, "data")

	rec = httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	var fail map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &fail))
	require.Equal(t, "NOT_FOUND", fail["code"])
	require.NotContains(t, fail, "data", "失败信封不得带 data 键")

	// 契约保留码本 US 不产生，但常量集合必须完整（约束 4）。
	require.Equal(t, "UNSUPPORTED_TYPE", codeUnsupportedType)
	require.Equal(t, "NOT_CONFIGURED", codeNotConfigured)
}

// TestContract_TimestampAndContentType 验证成功与失败两条路径的时间戳和 Content-Type。
// 逆改点：时间戳改成本地偏移或不带 Z → 解析与字面量断言红。
func TestContract_TimestampAndContentType(t *testing.T) {
	s := newTestServer(t, nil)

	for _, path := range []string{"/api/v1/health", "/api/v1/nope"} {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"), path)

		var env Envelope
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), path)
		require.Equal(t, "2026-10-18T12:00:00Z", env.Timestamp, path)
		parsed, err := time.Parse(time.RFC3339, env.Timestamp)
		require.NoError(t, err, path)
		require.Equal(t, time.UTC, parsed.Location(), "时间戳必须是无偏移的 UTC")
	}
}

// TestContract_UnknownRouteIs404Envelope 验证所有未匹配路径与方法都落到 404 信封。
// 逆改点：删掉 catch-all → net/http 返回纯文本 404（JSON 解析失败）；
// 或注册了方法无关的 405 兜底 → 状态码断言红。
// US-11 T025 起 `GET /` 是公开闭集里的管理台重定向（302），不再落到兜底；
// 根级未匹配路径改用 `/admin` 验证同一意图。
func TestContract_UnknownRouteIs404Envelope(t *testing.T) {
	s := newTestServer(t, nil)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/nope"},
		// US-11 T046 已注册 `GET /api/v1/sessions`（控制台列表），未知路径样本移出
		// 已注册集合；断言意图不变：未注册路径必须落回 JSON 404 信封。
		{http.MethodGet, "/api/v1/console/unknown"},
		{http.MethodPut, "/api/v1/health"}, // 方法不符也走 catch-all
		{http.MethodGet, "/admin"},         // 根级、API 前缀之外
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, http.StatusNotFound, rec.Code, "%s %s", tc.method, tc.path)
		require.Contains(t, rec.Body.String(), `"code":"NOT_FOUND"`, "%s %s", tc.method, tc.path)
	}
}

// TestContract_ErrorMappingByType 逐个 errors.As 分支验证状态码（约束 22）。
// 逆改点：按错误文本匹配 → 改一句文案就改变状态码；500 直出原始错误 → 泄漏断言红。
func TestContract_ErrorMappingByType(t *testing.T) {
	s := newTestServer(t, nil)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	runCases := []struct {
		name       string
		err        error
		runCtx     context.Context
		wantStatus int
		wantCode   string
	}{
		{"Agent 不存在", &core.AgentNotFoundError{Name: "ghost"}, context.Background(), http.StatusNotFound, codeNotFound},
		{"同会话忙", &core.SessionBusyError{SessionID: "web-x"}, context.Background(), http.StatusConflict, codeSessionBusy},
		{"实例满", &core.InstanceFullError{Limit: 7}, context.Background(), http.StatusTooManyRequests, codeTooManyRuns},
		// 准入面按契约走 409（保存面同条件才是 400）：绑定 / 凭据专用码排在通用码之前。
		{"准入：缺绑定", core.ErrAgentResourceBindingMissing, context.Background(), http.StatusConflict, codeAgentResourceBindingMissing},
		{"准入：绑定非法", core.ErrAgentResourceBindingInvalid, context.Background(), http.StatusConflict, codeAgentResourceBindingInvalid},
		{"准入：无资格", core.ErrResourceNotEligible, context.Background(), http.StatusConflict, codeResourceNotEligible},
		{"准入：凭据已撤销", core.ErrCredentialRevoked, context.Background(), http.StatusConflict, codeCredentialRevoked},
		{"准入：主密钥不可用", core.ErrCredentialUnavailable, context.Background(), http.StatusServiceUnavailable, codeCredentialUnavailable},
		{"运行超时", errors.New("deadline surfaced as error"), expired, http.StatusGatewayTimeout, codeTimeout},
		{"其他错误", errors.New("disk on fire"), context.Background(), http.StatusInternalServerError, codeInternal},
	}
	for _, tc := range runCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.writeRunError(rec, tc.err, tc.runCtx)
			require.Equal(t, tc.wantStatus, rec.Code)
			require.Contains(t, rec.Body.String(), `"code":"`+tc.wantCode+`"`)
			require.NotContains(t, rec.Body.String(), "disk on fire", "500 不得透出底层错误串")
		})
	}

	t.Run("仓储未命中", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.writeStoreError(rec, fs.ErrNotExist, "web-x")
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Contains(t, rec.Body.String(), `"code":"NOT_FOUND"`)
		require.Contains(t, rec.Body.String(), "web-x", "404 回显会话标识")
	})
}

// TestContract_ViewFieldShapes 验证视图字段形态（约束 34）：空集合输出 `[]`、
// 未标可选的字符串输出 `""`。
// 逆改点：nil 切片直接序列化 → `"messages":null`；时间零值渲染 0001 年 → 空串断言红。
func TestContract_ViewFieldShapes(t *testing.T) {
	t.Run("空集合输出 []", func(t *testing.T) {
		raw, err := json.Marshal(sessionDetailView(core.SessionRecord{}, nil))
		require.NoError(t, err)
		require.Contains(t, string(raw), `"messages":[]`)
		require.NotContains(t, string(raw), `"messages":null`)

		raw, err = json.Marshal(profileView(core.AgentEntry{Name: "ghost"}))
		require.NoError(t, err)
		require.Contains(t, string(raw), `"tools":[]`)
		require.Contains(t, string(raw), `"skills":[]`)
		require.Contains(t, string(raw), `"problems":[]`)

		raw, err = json.Marshal(infoView(Options{}, time.Now()))
		require.NoError(t, err)
		require.Contains(t, string(raw), `"providers":[]`)
	})

	t.Run("未标可选字符串输出空串", func(t *testing.T) {
		raw, err := json.Marshal(runView(core.RunRequest{}, core.RunResult{}, time.Time{}, time.Time{}))
		require.NoError(t, err)
		for _, field := range []string{
			`"run_id":""`, `"session_id":""`, `"final_text":""`,
			`"reason":""`, `"config_hash":""`, `"started_at":""`, `"finished_at":""`,
		} {
			require.Contains(t, string(raw), field)
		}

		raw, err = json.Marshal(sessionSummaryView(core.SessionRecord{}))
		require.NoError(t, err)
		require.Contains(t, string(raw), `"created_at":""`)
		require.Contains(t, string(raw), `"session_id":""`)
	})
}

// TestOpenAPI_ScheduleTimeFields 验证时间字段在真实契约中的 oneOf 语义（R31）：
// 调度视图与运行 / 生命周期视图的可空时间字段同为「RFC3339 date-time | 空串」——
// 同一输入分别喂各字段的 schema，合法 RFC3339 与 `""` 接受，非法串与数字拒绝。
// `RunView.finished_at` 与 `AgentLifecycleView.updated_at` 由人工 review ② 契约缺省值
// 修复补齐（运行中 / 状态文件缺失或损坏时输出空串）。
// 逆改点：删掉 `{ const: "" }` 分支 → 空串用例红；去掉 `format: date-time` 或未
// 启用 AssertFormat → 非法串用例红；oneOf 放宽为任意类型 → 数字用例红。
// The nullable time fields must accept RFC3339 or the empty string and reject every
// other value, validated against the real docs/contracts/openapi.yaml.
func TestOpenAPI_ScheduleTimeFields(t *testing.T) {
	root, err := os.OpenRoot("../../docs/contracts")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	raw, err := root.ReadFile("openapi.yaml")
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))

	schemas := map[string]*jsonschema.Schema{
		"ScheduleView.last_run_at":      compileFieldSchema(t, doc, "ScheduleView", "last_run_at"),
		"ExecutionView.finished_at":     compileFieldSchema(t, doc, "ExecutionView", "finished_at"),
		"RunView.finished_at":           compileFieldSchema(t, doc, "RunView", "finished_at"),
		"AgentLifecycleView.updated_at": compileFieldSchema(t, doc, "AgentLifecycleView", "updated_at"),
	}

	cases := []struct {
		name  string
		value any
		valid bool
	}{
		{"valid RFC3339", "2026-10-18T12:00:00Z", true},
		{"empty string", "", true},
		{"invalid time string", "not-a-time", false},
		{"number", 12345, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for name, schema := range schemas {
				err := schema.Validate(tc.value)
				if tc.valid {
					require.NoError(t, err, "%s 必须接受 %#v", name, tc.value)
				} else {
					require.Error(t, err, "%s 必须拒绝 %#v", name, tc.value)
				}
			}
		})
	}
}

// mustDig 按路径从 openapi 文档中取出子节点；任一步缺失即失败——契约结构漂移
// （改名 / 挪位）必须让测试红，而不是静默跳过校验。
// mustDig walks a path through the parsed OpenAPI document and fails on any missing
// step, so contract structure drift turns red instead of skipping validation.
func mustDig(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()
	var cur any = doc
	for i, key := range path {
		m, ok := cur.(map[string]any)
		require.Truef(t, ok, "路径 %v 的第 %d 步不是映射", path, i)
		cur, ok = m[key]
		require.Truef(t, ok, "openapi.yaml 缺少路径 %v", path[:i+1])
	}
	return cur
}

// compileFieldSchema 把 openapi.yaml 中某个字段的 schema 编译成可独立校验的
// Draft 2020-12 实例；AssertFormat 让 `format: date-time` 真正参与断言。
// compileFieldSchema compiles one field's schema as a standalone Draft 2020-12
// instance with format assertions enabled.
func compileFieldSchema(t *testing.T, doc map[string]any, schemaName, field string) *jsonschema.Schema {
	t.Helper()
	fieldSchema, ok := mustDig(t, doc, "components", "schemas", schemaName, "properties", field).(map[string]any)
	require.Truef(t, ok, "%s.%s 的 schema 不是映射", schemaName, field)

	schemaDoc := map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema"}
	for k, v := range fieldSchema {
		schemaDoc[k] = v
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	loc := "inline://basalt/openapi/" + schemaName + "/" + field
	require.NoError(t, compiler.AddResource(loc, schemaDoc))
	schema, err := compiler.Compile(loc)
	require.NoError(t, err)
	return schema
}

// errorCodeRow 匹配 run-and-errors §8 错误码表的表格行：`| 400 | `BAD_REQUEST` | …`。
// errorCodeRow matches one row of the §8 error-code table.
var errorCodeRow = regexp.MustCompile("^\\|\\s*(\\d{3})\\s*\\|\\s*`([A-Z_]+)`\\s*\\|")

// goCodeDeclaration 匹配 envelope.go 的错误码常量声明（对齐后的 `name  = "CODE"`）。
// goCodeDeclaration matches one aligned code constant declaration in envelope.go.
var goCodeDeclaration = regexp.MustCompile(`(?m)^\s*(code[A-Z][A-Za-z]*)\s*=\s*"([A-Z_]+)"`)

// contractErrorCodes 解析契约 §8 的错误码表（`| 状态 | `CODE` | …`），返回
// 码字面量 → HTTP 状态。成功码 OK 也在表内，调用方按需剔除；表不可解析直接失败
// ——结构漂移不得静默变成"没有错误码可对账"。
// contractErrorCodes parses the §8 table into code → HTTP status; a structural
// drift fails instead of silently yielding an empty map.
func contractErrorCodes(t *testing.T) map[string]string {
	t.Helper()
	root, err := os.OpenRoot("../../docs/contracts")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	raw, err := root.ReadFile("run-and-errors.md")
	require.NoError(t, err)

	contract := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if m := errorCodeRow.FindStringSubmatch(line); m != nil {
			contract[m[2]] = m[1]
		}
	}
	require.NotEmpty(t, contract, "契约 §8 错误码表必须可解析——结构漂移不得静默跳过")
	return contract
}

// TestContract_ErrorCodes 逐个断言本包产生的 30 个错误码及其 HTTP 状态（§6.3 /
// run-and-errors §8，含 LIMIT_REACHED 与 R4 的凭据 / 范围 / 绑定码，不含 OK），并把
// envelope.go 的常量集合与契约表逐条对账（等价于模块卡 §6.3 行 2247 的 grep 判据）。
// 判据面沿革（记录在 run-state）：T016 把 US-11 的集合扩到 18 个码；R4 修订把契约表
// 扩到 30 行后，本用例同批扩到 30——对账口径（不得多、不得少、不得改字面量）**原样
// 保留**，适配是扩大期望集合，不是删除断言。
// 逆改点：新增一个未入契约的码常量 / 改错某个码字面量 / 从契约表删掉任一码 →
// 表比对或常量比对红。
// The 30 codes this package produces must equal the contract table both by literal
// and by HTTP status; the constant set in envelope.go is reconciled the same way.
func TestContract_ErrorCodes(t *testing.T) {
	contract := contractErrorCodes(t)
	// OK 是成功码不是错误码。
	delete(contract, "OK")

	produced := []struct {
		name   string
		code   string
		status string
	}{
		{"codeLimitReached", codeLimitReached, "200"},
		{"codeBadRequest", codeBadRequest, "400"},
		{"codeUnsupportedType", codeUnsupportedType, "400"},
		{"codeEditUnsupported", codeEditUnsupported, "400"},
		{"codeImportRejected", codeImportRejected, "400"},
		{"codeCredentialModeRequired", codeCredentialModeRequired, "400"},
		{"codeCredentialRefReadOnly", codeCredentialRefReadOnly, "400"},
		{"codeResourceNotEligible", codeResourceNotEligible, "400"},
		{"codeAgentResourceUnavailable", codeAgentResourceUnavailable, "400"},
		{"codeAgentResourceBindingMissing", codeAgentResourceBindingMissing, "400"},
		{"codeAgentResourceBindingInvalid", codeAgentResourceBindingInvalid, "400"},
		{"codeCredentialRevoked", codeCredentialRevoked, "400"},
		{"codeUnauthorized", codeUnauthorized, "401"},
		{"codeForbidden", codeForbidden, "403"},
		{"codeNotFound", codeNotFound, "404"},
		{"codeSessionBusy", codeSessionBusy, "409"},
		{"codeAgentBusy", codeAgentBusy, "409"},
		{"codeAgentUnavailable", codeAgentUnavailable, "409"},
		{"codeConflict", codeConflict, "409"},
		{"codeResourceUIDMismatch", codeResourceUIDMismatch, "409"},
		{"codeAgentCopyBlocked", codeAgentCopyBlocked, "409"},
		{"codeResourceScopeInUse", codeResourceScopeInUse, "409"},
		{"codeCredentialOwnerMismatch", codeCredentialOwnerMismatch, "409"},
		{"codePreconditionFailed", codePreconditionFailed, "412"},
		{"codeTooManyRuns", codeTooManyRuns, "429"},
		{"codeAuthRateLimited", codeAuthRateLimited, "429"},
		{"codeInternal", codeInternal, "500"},
		{"codeNotConfigured", codeNotConfigured, "503"},
		{"codeCredentialUnavailable", codeCredentialUnavailable, "503"},
		{"codeTimeout", codeTimeout, "504"},
	}
	require.Len(t, produced, 30, "本包产生契约 §8 的全部 30 个错误码（不含 OK）")

	byCode := make(map[string]string, len(produced))
	byName := make(map[string]string, len(produced))
	for _, entry := range produced {
		byCode[entry.code] = entry.status
		byName[entry.name] = entry.code
	}
	require.Equal(t, contract, byCode, "错误码与 HTTP 状态必须逐条等于契约 §8")
	require.Equal(t, "OK", codeOK, "成功码字面量")

	source, err := os.ReadFile("envelope.go")
	require.NoError(t, err)
	declared := map[string]string{}
	for _, m := range goCodeDeclaration.FindAllStringSubmatch(string(source), -1) {
		declared[m[1]] = m[2]
	}
	byName["codeOK"] = "OK"
	require.Equal(t, byName, declared,
		"envelope.go 的码常量必须与契约表一一对应（不得多、不得少、不得改字面量）")
}

// readContractDoc 读取 docs/contracts 下的契约文件并解析 YAML；缺文件即失败。
func readContractDoc(t *testing.T, name string) map[string]any {
	t.Helper()
	root, err := os.OpenRoot("../../docs/contracts")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	raw, err := root.ReadFile(name)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	return doc
}

// compileObjectSchema 把 openapi.yaml 的 components.schemas 装配成自包含的 JSON
// Schema 文档并编译指定对象；$ref 保持 `#/components/schemas/...` 形态，指向同一份
// 从契约文件读出的 schema 子树。AssertFormat 让 format: date-time 真正参与断言。
// The wrapper keeps the original ref pointers so nested references resolve against the
// same subtree read from docs/contracts/openapi.yaml.
func compileObjectSchema(t *testing.T, doc map[string]any, name string) *jsonschema.Schema {
	t.Helper()
	schemas, ok := mustDig(t, doc, "components", "schemas").(map[string]any)
	require.True(t, ok, "components.schemas 不是映射")
	wrapper := map[string]any{
		"$schema":    "https://json-schema.org/draft/2020-12/schema",
		"components": map[string]any{"schemas": schemas},
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	const base = "mem://basalt/openapi.yaml"
	require.NoError(t, compiler.AddResource(base, wrapper))
	schema, err := compiler.Compile(base + "#/components/schemas/" + name)
	require.NoError(t, err, "schema %s 必须能独立编译", name)
	return schema
}

// contractPropertyNames 收集一个 schema 声明的全部属性名（allOf 与 $ref 递归展开）。
// contractPropertyNames unfolds allOf branches and $ref pointers.
func contractPropertyNames(t *testing.T, doc map[string]any, name string) map[string]bool {
	t.Helper()
	props := map[string]bool{}
	var walk func(node any)
	walk = func(node any) {
		m, ok := node.(map[string]any)
		require.True(t, ok, "schema 节点必须是映射")
		if ref, ok := m["$ref"].(string); ok {
			path := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
			walk(mustDig(t, doc, path...))
		}
		if raw, ok := m["properties"].(map[string]any); ok {
			for key := range raw {
				props[key] = true
			}
		}
		if branches, ok := m["allOf"].([]any); ok {
			for _, branch := range branches {
				walk(branch)
			}
		}
	}
	walk(mustDig(t, doc, "components", "schemas", name))
	require.NotEmpty(t, props, "%s 的属性集不得为空", name)
	return props
}

// viewKeys 取视图 JSON 的对象层键集合。
func viewKeys(t *testing.T, view any) map[string]bool {
	t.Helper()
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(raw, &obj))
	keys := make(map[string]bool, len(obj))
	for key := range obj {
		keys[key] = true
	}
	return keys
}

// requireViewMatchesSchema 同时钉住"字段集与契约完全一致"与"整对象过契约 schema"：
// 多一个字段（把持久形态直接序列化）或少一个字段（漏投影）都会红。
// The view's JSON key set must equal the contract property set, and the marshaled
// payload must validate against the contract schema.
func requireViewMatchesSchema(t *testing.T, doc map[string]any, schemaName string, view any) {
	t.Helper()
	require.Equal(t, contractPropertyNames(t, doc, schemaName), viewKeys(t, view),
		"%s 的字段集必须与契约完全一致", schemaName)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	var payload any
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.NoErrorf(t, compileObjectSchema(t, doc, schemaName).Validate(payload),
		"%s 载荷必须通过契约 schema 校验", schemaName)
}

// contractEnum 取出某个 schema 字段的枚举字面量；与 contractPropertyNames 一样展开
// allOf / $ref，因为 OperationDetail 的 verification 藏在 allOf 的第二个分支。
// contractEnum walks allOf and $ref the same way the property-set helper does.
func contractEnum(t *testing.T, doc map[string]any, schemaName, field string) []string {
	t.Helper()
	var found any
	var walk func(node any)
	walk = func(node any) {
		m, ok := node.(map[string]any)
		require.True(t, ok, "schema 节点必须是映射")
		if ref, ok := m["$ref"].(string); ok {
			path := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
			walk(mustDig(t, doc, path...))
		}
		if props, ok := m["properties"].(map[string]any); ok {
			if raw, ok := props[field]; ok {
				require.Nilf(t, found, "%s.%s 在契约里被声明了不止一次", schemaName, field)
				found = raw
			}
		}
		if branches, ok := m["allOf"].([]any); ok {
			for _, branch := range branches {
				walk(branch)
			}
		}
	}
	walk(mustDig(t, doc, "components", "schemas", schemaName))
	entry, ok := found.(map[string]any)
	require.Truef(t, ok, "契约缺少 %s.%s", schemaName, field)
	raw, ok := entry["enum"].([]any)
	require.Truef(t, ok, "%s.%s 的 enum 不是数组", schemaName, field)
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		value, ok := item.(string)
		require.Truef(t, ok, "%s.%s 的枚举值不是字符串", schemaName, field)
		values = append(values, value)
	}
	return values
}

// acceptedBy 从候选集里筛出被 accept 承认的取值——用于把"Go 侧接受的枚举集合"与
// 契约 enum 逐项比对。
func acceptedBy(candidates []string, accept func(string) bool) []string {
	accepted := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if accept(candidate) {
			accepted = append(accepted, candidate)
		}
	}
	return accepted
}

// TestOpenAPI_RunViewPropertiesMatchContract 钉住 RunView 的字段集与取值形态：
// **已结束**与**未结束**（finished_at 为空串）两种形态都要整对象过契约 schema。
// 逆改点：新增/漏投影字段 → 字段集红；status / trigger 写错 → schema 枚举红；
// 把 finished_at 的 `{ const: "" }` 分支删掉 → 未结束形态红（人工 review ②
// 契约缺省值修复前，这里只能断言字段集，整对象校验收在已结束形态）。
func TestOpenAPI_RunViewPropertiesMatchContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	finished := RunView{
		RunID:        "run-1",
		SessionID:    "sess-1",
		Agent:        "alpha",
		Channel:      "web",
		Trigger:      triggerAPI,
		Status:       string(core.StatusCompleted),
		Reason:       "",
		FinalText:    "最终回答",
		Iterations:   3,
		ToolCalls:    2,
		ConfigHash:   "config-hash",
		ManifestHash: "manifest-hash",
		StartedAt:    "2026-10-21T10:00:00Z",
		FinishedAt:   "2026-10-21T10:00:03Z",
	}
	requireViewMatchesSchema(t, doc, "RunView", finished)

	// 未结束的运行：finished_at 为空串，整对象同样必须过契约 schema。
	running := finished
	running.Status = string(core.StatusRunning)
	running.FinishedAt = ""
	requireViewMatchesSchema(t, doc, "RunView", running)
	require.Empty(t, running.FinishedAt)
}

// TestOpenAPI_ProfileViewPropertiesMatchContract 钉住 /profiles 的契约字段集（含
// lifecycle / registered / runnable / problems）与三种生命周期形态：**已建立**、
// **缺失**、**损坏**——后两者的 updated_at 为空串，同样必须整对象过契约 schema
// （人工 review ② 契约缺省值修复：`AgentLifecycleView.updated_at` 增空串分支）。
// 行为断言（valid / runnable / problems）另见 handlers_test.go 的
// TestProfiles_LifecycleAndRunnable；本测试只判载荷形态。
// 逆改点：漏投影 lifecycle / runnable → 字段集红；state 写非枚举字面量 → schema 红；
// 删掉 updated_at 的 `{ const: "" }` 分支 → 缺失 / 损坏形态红。
func TestOpenAPI_ProfileViewPropertiesMatchContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	state := string(core.AgentActive)
	view := ProfileView{
		Name:           "alpha",
		Description:    "运维助手",
		Provider:       "openai",
		Model:          "gpt-test",
		Tools:          []string{"fs_read"},
		Skills:         []string{"diagnose"},
		NotifyMode:     string(core.NotifyModeAllowlist),
		NotifyChannels: []string{"ops-alert"},
		Schedules:      1,
		Lifecycle: AgentLifecycleView{
			Agent:       "alpha",
			State:       &state,
			Generation:  2,
			UpdatedAt:   "2026-10-21T08:30:00Z",
			OperationID: "op-2-enable",
			Valid:       true,
		},
		Registered: true,
		Runnable:   true,
		Problems:   []string{},
	}
	requireViewMatchesSchema(t, doc, "ProfileView", view)

	// 缺失与损坏：走真实投影 profileLifecycle（与 handleProfiles 同一条路径），
	// valid=false、state=null、updated_at 为空串——契约必须接受这个缺省值。
	srv := newTestServer(t, func(o *Options) {
		o.Lifecycle = &fakeLifecycle{
			recs: map[string]core.LifecycleRecord{},
			errs: map[string]error{"corrupt": errors.New("checksum mismatch")},
		}
	})
	for _, agent := range []string{"missing", "corrupt"} {
		broken := profileView(core.AgentEntry{Name: agent, Registered: true})
		broken.Lifecycle = srv.profileLifecycle(context.Background(), agent)
		broken.Runnable = false
		requireViewMatchesSchema(t, doc, "ProfileView", broken)
		require.Empty(t, broken.Lifecycle.UpdatedAt, "%s：缺失 / 损坏时 updated_at 必须为空串", agent)
		require.False(t, broken.Lifecycle.Valid)
	}
}

// TestOpenAPI_RunDetailPropertiesMatchContract 钉住 RunDetail = RunView + 清单视图
// 的 allOf 形态，并验证 legacy（无清单行）与契约允许的 null 分支。
// 逆改点：清单字段漏投影 → 属性集红；legacy 视图塞入未记录状态 → schema 枚举红。
func TestOpenAPI_RunDetailPropertiesMatchContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	manifest := &core.RunResourceManifest{
		FormatVersion:  1,
		Hash:           "manifest-hash",
		AgentLifecycle: "disabled",
		Provider: core.ProviderVersionRef{
			ResourceVersionRef: core.ResourceVersionRef{
				Name: "openai", VersionID: 11, VersionNo: 1, ContentHash: "provider-hash",
			},
		},
		MCPServers: []core.ResourceVersionRef{
			{Name: "search", VersionID: 21, VersionNo: 2, ContentHash: "mcp-search"},
		},
		Skills: []core.SkillVersionRef{{
			ResourceVersionRef: core.ResourceVersionRef{
				Name: "digest", VersionID: 31, VersionNo: 4, ContentHash: "skill-hash",
			},
			Files: []core.FileHash{{Path: "SKILL.md", SHA256: "skill-md-hash", Size: 42}},
		}},
	}
	recorded := core.RunDetail{
		RunRecord: core.RunRecord{
			RunID: "run-1", SessionID: "sess-1", Agent: "alpha", Channel: "web",
			Trigger: core.TriggerCron, ConfigHash: "config-hash",
			StartedAt: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
		},
		Status:           core.StatusCompleted,
		ManifestHash:     "manifest-hash",
		FinishedAt:       time.Date(2026, 10, 21, 10, 0, 3, 0, time.UTC),
		ResourceManifest: manifest,
	}
	requireViewMatchesSchema(t, doc, "RunDetail", runDetailView(recorded))

	// legacy：没有清单行 → legacy_unrecorded=true，且**省略** agent_lifecycle_state
	// （注 17：不知道就不填 active）。省略本身由 T028 的视图用例钉住，这里只判
	// legacy 载荷仍能满足契约 schema。
	legacy := runDetailView(core.RunDetail{
		RunRecord: core.RunRecord{
			RunID: "run-legacy", Channel: "scheduler", Trigger: core.TriggerManual,
			StartedAt: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
		},
		Status:     core.StatusFailed,
		FinishedAt: time.Date(2026, 10, 20, 9, 0, 4, 0, time.UTC),
	})
	require.True(t, legacy.ResourceManifest.LegacyUnrecorded)
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	var payload any
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.NoError(t, compileObjectSchema(t, doc, "RunDetail").Validate(payload),
		"legacy 视图同样必须满足契约")

	// 契约允许 resource_manifest 为 null（oneOf 的空分支），两个分支都必须可用。
	nullManifest := map[string]any{}
	require.NoError(t, json.Unmarshal(mustMarshal(t, runDetailView(recorded)), &nullManifest))
	nullManifest["resource_manifest"] = nil
	require.NoError(t, compileObjectSchema(t, doc, "RunDetail").Validate(nullManifest),
		"契约允许 resource_manifest 为 null")
}

// mustMarshal 供测试内组合载荷使用。
func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

// TestOpenAPI_ManifestViewPropertiesMatchContract 钉住清单视图及其嵌套引用
// （ResourceVersionRef / SkillVersionRef / FileHash）的字段集。
// 逆改点：技能四个哈希漏字段 / kind 写成持久类型的名字 → 属性集或枚举红。
func TestOpenAPI_ManifestViewPropertiesMatchContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	manifest := &core.RunResourceManifest{
		FormatVersion:  1,
		AgentLifecycle: "active",
		Provider: core.ProviderVersionRef{
			ResourceVersionRef: core.ResourceVersionRef{
				Name: "openai", VersionID: 11, VersionNo: 1, ContentHash: "provider-hash",
			},
		},
		MCPServers: []core.ResourceVersionRef{
			{Name: "search", VersionID: 21, VersionNo: 2, ContentHash: "mcp-search"},
		},
		Skills: []core.SkillVersionRef{{
			ResourceVersionRef: core.ResourceVersionRef{
				Name: "digest", VersionID: 31, VersionNo: 4, ContentHash: "skill-hash",
			},
			Files: []core.FileHash{{Path: "SKILL.md", SHA256: "skill-md-hash", Size: 42}},
		}},
	}
	view := runResourceManifestView(manifest)
	requireViewMatchesSchema(t, doc, "RunResourceManifestView", view)
	requireViewMatchesSchema(t, doc, "ResourceVersionRef", view.Providers[0])
	requireViewMatchesSchema(t, doc, "ResourceVersionRef", view.MCPServers[0])
	requireViewMatchesSchema(t, doc, "SkillVersionRef", view.Skills[0])
	requireViewMatchesSchema(t, doc, "FileHash", view.Files[0])
}

// TestOpenAPI_McpServerViewPropertiesMatchContract 钉住 MCP 只读视图的字段集与契约
// 完全一致，并验证 pending_restart=true 的载荷仍满足 schema（约束 29 / 约束 35）：
// 视图只回可执行名与变量名，不回参数或环境变量值。
// 逆改点：漏 pending_restart、多回 args 或 env 值 → 属性集红。
func TestOpenAPI_McpServerViewPropertiesMatchContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	applied := int64(21)
	view := McpServerView{
		Name:               "search",
		Transport:          "stdio",
		Command:            "mcp-search",
		EnvKeys:            []string{"SEARCH_TOKEN"},
		Connected:          true,
		Tools:              []string{"search"},
		PublishedVersionID: 22,
		PublishedVersionNo: 2,
		AppliedVersionID:   21,
		AppliedVersionNo:   1,
		PendingRestart: mcpPendingRestart(core.ResourcePointer{
			PublishedVersionID: 22,
			AppliedVersionID:   &applied,
		}),
	}
	require.True(t, view.PendingRestart, "新版本已发布但进程仍用旧版本 → 需重启")
	requireViewMatchesSchema(t, doc, "McpServerView", view)
}

// TestOpenAPI_McpPendingRestartDerived 钉住"需重启"的推导规则（注 18 / D-09）：
// 始终按发布与已应用指针实时计算，基线成功应用同一版本时为假；缺失发布指针
// 不伪造"需重启"；connected 不参与判断。
// 逆改点：写死 true、跟随 connected、或把 nil applied 当成已应用 → 断言红。
func TestOpenAPI_McpPendingRestartDerived(t *testing.T) {
	applied := func(v int64) *int64 { return &v }
	cases := []struct {
		name      string
		published int64
		applied   *int64
		want      bool
	}{
		{name: "same version", published: 21, applied: applied(21), want: false},
		{name: "published newer", published: 22, applied: applied(21), want: true},
		{name: "not yet applied", published: 21, applied: nil, want: true},
		{name: "missing published pointer", published: 0, applied: nil, want: false},
		{name: "missing published with applied", published: 0, applied: applied(21), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpPendingRestart(core.ResourcePointer{
				PublishedVersionID: tc.published,
				AppliedVersionID:   tc.applied,
			})
			require.Equal(t, tc.want, got)
		})
	}

	// connected 与 pending_restart 是两个独立事实：已连接旧版本时两者可以同时成立。
	pointer := core.ResourcePointer{PublishedVersionID: 22, AppliedVersionID: applied(21)}
	require.True(t, mcpPendingRestart(pointer), "连接状态不改变待重启判断")
}

// TestOpenAPI_RestartHintIsMcpOnly 钉住需重启提示的契约出口只有 McpServerView：
// 沙箱白名单与 Provider 段 / http 段按技术路线⑤由页面与文档标注，不得靠给只读
// 视图加字段"假装已生效"（约束 29）。契约若真要为它们加字段，必须先走人工裁决。
// 逆改点：给 SandboxView / ModelConnectionView 加 pending_restart → 断言红。
func TestOpenAPI_RestartHintIsMcpOnly(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	require.True(t, contractPropertyNames(t, doc, "McpServerView")["pending_restart"],
		"MCP 的待重启状态必须由契约字段表达")
	for _, name := range []string{"SandboxView", "ModelConnectionView"} {
		require.False(t, contractPropertyNames(t, doc, name)["pending_restart"],
			"%s 不得新增服务端待重启字段；该提示属页面与文档标注", name)
	}
}

// TestOpenAPI_OperationPropertiesMatchContract 钉住 OperationView 与 OperationDetail
// （allOf 两个分支的并集）的字段集与整对象 schema。
// 逆改点：把 detail_json 原样挂到视图 → 属性集红；verification 写成空串 → 枚举红。
func TestOpenAPI_OperationPropertiesMatchContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	record := core.OperationRecord{
		OperationID:        "op-1",
		Action:             "agent.content.update",
		TargetKind:         "agent",
		TargetID:           "alpha",
		ActorSubject:       "token:0123456789abcdef",
		AuthMethod:         core.AuthMethodBearer,
		Source:             "127.0.0.1:5555",
		RequestHash:        "request-hash",
		RequestedVersionID: 12,
		ExpectedETag:       `"sha256:` + strings.Repeat("a", 64) + `"`,
		Status:             core.OperationSuccess,
		ResultCode:         "OK",
		ErrorMessage:       "",
		DetailJSON:         []byte(`{"providers":2,"path":"agents/alpha/AGENT.md"}`),
		CreatedAt:          time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
		FinishedAt:         time.Date(2026, 10, 21, 10, 0, 1, 0, time.UTC),
		RecoveredAt:        time.Date(2026, 10, 21, 10, 5, 0, 0, time.UTC),
		Verification:       core.VerificationApplied,
	}
	requireViewMatchesSchema(t, doc, "OperationView", operationView(record))
	requireViewMatchesSchema(t, doc, "OperationDetail", operationDetailView(record))

	// unknown 状态 + 未恢复：verification 投影为 not_required（NULL 不是合法枚举值）。
	unknown := record
	unknown.Status = core.OperationUnknown
	unknown.RecoveredAt = time.Time{}
	unknown.Verification = ""
	requireViewMatchesSchema(t, doc, "OperationDetail", operationDetailView(unknown))
}

// TestOpenAPI_ViewEnumsMatchGoLiterals 钉住四个枚举面：操作状态、目标类型、恢复
// 结论、运行状态与触发来源——Go 侧接受的取值集合必须逐项等于契约 enum。
// 逆改点：Go 侧多收一个值（契约外）或少收一个（契约内） → ElementsMatch 红。
func TestOpenAPI_ViewEnumsMatchGoLiterals(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	require.ElementsMatch(t,
		contractEnum(t, doc, "OperationView", "status"),
		acceptedBy([]string{"", "pending", "success", "failed", "unknown", "running", "done", "OK"}, validOperationStatus),
		"OperationView.status 的接受集合必须等于契约枚举")

	require.ElementsMatch(t,
		contractEnum(t, doc, "OperationView", "target_kind"),
		acceptedBy([]string{
			"", "agent", "schedule", "model_connection", "mcp_server", "skill", "notify_channel",
			"network_allowlist", "system", "provider", "mcp", "schedule_trigger",
		}, validOperationTargetKind),
		"OperationView.target_kind 的接受集合必须等于契约枚举")

	require.ElementsMatch(t,
		contractEnum(t, doc, "OperationDetail", "verification"),
		[]string{
			operationVerificationNotRequired,
			core.VerificationApplied,
			core.VerificationNotApplied,
			core.VerificationIndeterminate,
		},
		"OperationDetail.verification 的投影集合必须等于契约枚举")

	require.ElementsMatch(t,
		contractEnum(t, doc, "OperationView", "auth_method"),
		[]string{core.AuthMethodBearer, core.AuthMethodLocalTrust, core.AuthMethodCLI, core.AuthMethodMigration},
		"OperationView.auth_method 的四种固定字面量必须等于契约枚举")

	require.ElementsMatch(t,
		contractEnum(t, doc, "RunView", "status"),
		[]string{
			string(core.StatusRunning),
			string(core.StatusCompleted),
			string(core.StatusLimitReached),
			string(core.StatusCancelled),
			string(core.StatusFailed),
		},
		"RunView.status 的五个字面量必须等于契约枚举")

	// 触发来源是四字面量闭集：user / api 由渠道层写入，cron / manual 由调度面写入。
	triggers := contractEnum(t, doc, "RunView", "trigger")
	require.ElementsMatch(t, []string{"user", "cron", "manual", "api"}, triggers,
		"RunView.trigger 的四个字面量必须等于契约枚举")
	require.Subset(t, triggers, []string{triggerAPI, string(core.TriggerCron), string(core.TriggerManual)},
		"web / core 能产出的触发来源必须在契约枚举内")
}

// TestOpenAPI_SystemSampleHasNoDiskFields 钉住约束 34：SystemSample 只有五列采样，
// **不得**扩磁盘 / uptime 字段（运行时长在 /info 的 uptime_seconds）。
// 逆改点：给 SystemSample 加 disk_* / uptime_* → 属性集断言红。
func TestOpenAPI_SystemSampleHasNoDiskFields(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	props := contractPropertyNames(t, doc, "SystemSample")
	require.Equal(t, map[string]bool{
		"sampled_at":       true,
		"cpu_percent":      true,
		"memory_rss_bytes": true,
		"goroutines":       true,
		"unavailable":      true,
	}, props, "SystemSample 的属性集固定为五列")
	for key := range props {
		require.NotContains(t, strings.ToLower(key), "disk", "不得新增磁盘字段：%s", key)
		require.NotContains(t, strings.ToLower(key), "uptime", "不得新增运行时长字段：%s", key)
	}

	// 平台不支持时 null + unavailable 是合法载荷（不伪造 0）；字符串读数必须被拒。
	schema := compileObjectSchema(t, doc, "SystemSample")
	valid := map[string]any{
		"sampled_at":       "2026-10-21T10:00:00Z",
		"cpu_percent":      nil,
		"memory_rss_bytes": nil,
		"goroutines":       nil,
		"unavailable":      []any{"cpu_percent", "memory_rss_bytes", "goroutines"},
	}
	require.NoError(t, schema.Validate(valid), "null + unavailable 必须是合法读数")
	valid["cpu_percent"] = 12.5
	valid["goroutines"] = 17
	require.NoError(t, schema.Validate(valid), "有差值基线时的数字读数必须合法")
	invalid := map[string]any{"sampled_at": "2026-10-21T10:00:00Z", "cpu_percent": "high"}
	require.Error(t, schema.Validate(invalid), "伪造的字符串读数必须被 schema 拒绝")
}

// TestContract_PersistenceShapeNotSerialized 钉住 T031 的"持久形态不直接序列化"：
// 同一份数据，core 的持久结构带 `hash` / 单数 `provider` / `detail_json` 等内部
// 键，而 HTTP 视图必须只出契约键。
// 逆改点：视图直接嵌入 core 结构 → 持久键出现在响应体 → 红。
func TestContract_PersistenceShapeNotSerialized(t *testing.T) {
	v1 := &core.RunResourceManifest{
		FormatVersion:  1,
		Hash:           "manifest-hash",
		AgentLifecycle: "active",
		Provider: core.ProviderVersionRef{
			ResourceVersionRef: core.ResourceVersionRef{
				Name: "openai", VersionID: 11, VersionNo: 1, ContentHash: "provider-hash",
			},
		},
	}
	persisted, err := core.MarshalManifest(*v1)
	require.NoError(t, err)
	require.Contains(t, string(persisted), `"hash":"manifest-hash"`, "持久形态确实带顶层 hash")

	// v1：历史那一个条目落在复数 providers 里；单数 provider 与 notify 按契约输出 null。
	// 视图**不得**把 v2 才有的字段投影到 v1 行上——那等于给历史 Run 编造事实。
	v1Raw := string(mustMarshal(t, runResourceManifestView(v1)))
	require.Contains(t, v1Raw, `"providers":[{"kind":"model_connection"`, "v1 用契约的复数 providers 数组承载历史条目")
	require.Contains(t, v1Raw, `"provider":null`, "v1 没有单 Provider 固定，按契约输出 null")
	require.Contains(t, v1Raw, `"notify":null`, "v1 清单没有通知授权，按契约输出 null")
	require.NotContains(t, v1Raw, `"hash":`, "视图不得直接序列化持久形态的 hash")

	// v2：单数 provider 是契约字段，providers 恒为空数组，notify 必填。
	v2 := &core.RunResourceManifest{
		FormatVersion:  2,
		Hash:           "manifest-hash",
		AgentUID:       "agent-7f3a",
		AgentLifecycle: "active",
		Provider: core.ProviderVersionRef{
			ResourceVersionRef: core.ResourceVersionRef{
				Name: "openai", ResourceUID: "conn-1a2b", VersionID: 12, VersionNo: 2,
				ContentHash: "provider-hash-2",
			},
			Model:             "gpt-test",
			CredentialID:      "cred-9c8d",
			CredentialVersion: 3,
			TargetBinding:     "binding-hash",
		},
		Notify: core.NotifyManifest{
			Mode:     core.NotifyModeAllowlist,
			Channels: []core.NotifyChannelRef{{Name: "ops", ResourceUID: "chan-4d5e"}},
		},
	}
	v2Raw := string(mustMarshal(t, runResourceManifestView(v2)))
	require.Contains(t, v2Raw, `"agent_uid":"agent-7f3a"`, "v2 固定 Agent 稳定身份")
	require.Contains(t, v2Raw, `"provider":{"name":"openai"`, "v2 用契约的单数 provider 承载固定")
	require.Contains(t, v2Raw, `"providers":[]`, "v2 的 v1 兼容数组恒为空")
	require.Contains(t, v2Raw, `"notify":{"mode":"allowlist"`, "v2 起通知授权必须投影")
	require.NotContains(t, v2Raw, `"hash":`, "v2 同样不得透出持久键")

	detailRaw := mustMarshal(t, operationDetailView(core.OperationRecord{
		OperationID: "op-1",
		DetailJSON:  []byte(`{"providers":1}`),
	}))
	require.Contains(t, string(detailRaw), `"detail":{`, "操作详情用契约的 detail 键")
	require.NotContains(t, string(detailRaw), `"detail_json"`, "不得把持久列名直接序列化")
	require.NotContains(t, string(detailRaw), "DetailJSON")
}

// TestContractAdmin_NewErrorCodes 是 T016 的必备锚点（check `012-web-etag-routes`）：
// R3 阶段 US-12 **唯一**新增的码是 IMPORT_REJECTED，且在契约 §8 里是 400；R4 修订
// 又把凭据 / 范围 / 绑定的十条并入契约表，本用例的常量数量随之到 30。两轮都遵守同
// 一条纪律：US-11 已产出的码 HTTP 状态不漂移（M6 的复用清单），envelope.go 的常量
// 声明集合与契约表（OK 除外）**逐条**对上，不得出现未入契约的码，也不得少一条。
// 逆改点：改 IMPORT_REJECTED 的状态 / 字面量 / 再加一个码常量 / 删契约行 → 红。
// 中文用例名：US-12 新增错误码与契约表对账。
func TestContractAdmin_NewErrorCodes(t *testing.T) {
	contract := contractErrorCodes(t)

	require.Equal(t, "400", contract["IMPORT_REJECTED"],
		"US-12 的唯一新码在契约 §8 必须是 400")
	require.Equal(t, "IMPORT_REJECTED", codeImportRejected)

	// 复用码的状态逐条锁定：写面新增端点只能复用这些既有语义。
	reused := map[string]string{
		"LIMIT_REACHED":       "200",
		"BAD_REQUEST":         "400",
		"UNSUPPORTED_TYPE":    "400",
		"EDIT_UNSUPPORTED":    "400",
		"UNAUTHORIZED":        "401",
		"FORBIDDEN":           "403",
		"NOT_FOUND":           "404",
		"SESSION_BUSY":        "409",
		"AGENT_BUSY":          "409",
		"AGENT_UNAVAILABLE":   "409",
		"CONFLICT":            "409",
		"PRECONDITION_FAILED": "412",
		"TOO_MANY_RUNS":       "429",
		"AUTH_RATE_LIMITED":   "429",
		"INTERNAL":            "500",
		"NOT_CONFIGURED":      "503",
		"TIMEOUT":             "504",
	}
	for code, status := range reused {
		require.Equal(t, status, contract[code], "%s 的 HTTP 状态不得漂移", code)
	}

	// 常量集合对账：每个声明的码都能在契约表里找到同一字面量，且集合大小相等
	// （不得多、不得少）。OK 之后单独保留在声明里，但不算错误码。
	source, err := os.ReadFile("envelope.go")
	require.NoError(t, err)
	declared := map[string]string{}
	for _, m := range goCodeDeclaration.FindAllStringSubmatch(string(source), -1) {
		declared[m[1]] = m[2]
	}
	require.Equal(t, "OK", declared["codeOK"])
	delete(declared, "codeOK")
	require.Len(t, declared, 30, "R4 落地后错误码常量恰为契约 §8 的 30 个")

	declaredByCode := make(map[string]string, len(declared))
	for _, code := range declared {
		declaredByCode[code] = contract[code]
	}
	contractWithoutOK := make(map[string]string, len(contract)-1)
	for code, status := range contract {
		if code != "OK" {
			contractWithoutOK[code] = status
		}
	}
	require.Equal(t, contractWithoutOK, declaredByCode,
		"envelope.go 的码常量集合必须与契约 §8 一一对应（IMPORT_REJECTED 是唯一新增）")
}

// TestContractAdmin_AgentDraftView 钉住 D-02：草稿响应是 `AgentDraftView`
// （agent + files[{path, content}]），**没有** etag / size——草稿没有磁盘基础版本；
// 空草稿输出 `[]`，文件顺序与正文原样保留；generate-files 的 200 data 引用同一
// schema，而不是带 ETag 的受管文件视图。
// 逆改点：给草稿加 etag / size / 换 schema 引用 / 空集合输出 null → 对应断言红。
// 中文用例名：草稿视图符合 D-02。
func TestContractAdmin_AgentDraftView(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	empty := agentDraftView(AgentDraftResult{Agent: "ops-agent"})
	requireViewMatchesSchema(t, doc, "AgentDraftView", empty)
	require.NotNil(t, empty.Files, "空草稿必须输出 [] 而不是 null")
	require.Empty(t, empty.Files)
	require.NotNil(t, empty.Questions, "没有问题时必须输出 [] 而不是 null")

	draft := agentDraftView(AgentDraftResult{Agent: "ops-agent", Files: []core.AuthorFile{
		{Path: "AGENT.md", Content: "# ops-agent"},
		{Path: "config.yaml", Content: "name: ops-agent"},
	}})
	requireViewMatchesSchema(t, doc, "AgentDraftView", draft)
	require.Equal(t, []AgentDraftFileView{
		{Path: "AGENT.md", Content: "# ops-agent"},
		{Path: "config.yaml", Content: "name: ops-agent"},
	}, draft.Files, "草稿顺序与正文原样保留")

	questions := agentDraftView(AgentDraftResult{Agent: "ops-agent", Questions: []core.AuthorQuestion{
		{ID: "frequency", Question: "每天几点运行？"},
	}})
	requireViewMatchesSchema(t, doc, "AgentDraftView", questions)
	require.NotNil(t, questions.Files, "有问题时 files 也必须输出 []")
	require.Equal(t, []AgentQuestionView{{ID: "frequency", Question: "每天几点运行？"}}, questions.Questions)

	response, err := json.Marshal(mustDig(t, doc,
		"paths", "/agents/{name}/generate-files", "post", "responses", "200"))
	require.NoError(t, err)
	require.Contains(t, string(response), "#/components/schemas/AgentDraftView")
	require.NotContains(t, string(response), "#/components/schemas/AgentFilesView",
		"草稿端点不得返回带 ETag 的受管文件视图（D-02）")
}

// TestContractAdmin_ManagedFilesETag 钉住 D-16：Agent 多文件提交使用**受管文件
// 集合** ETag——请求侧声明 If-Match 参数与 412，读侧返回 ETag 响应头与
// AgentFilesView（含 etag / files）；参数形状固定为必填的逐字强 ETag。
// 逆改点：删 If-Match 参数 / 删 412 / 删 ETag 响应头 / 把视图换成不带 etag 的形状
// → 对应断言红。
// 中文用例名：受管文件集合 ETag 契约（D-16）。
func TestContractAdmin_ManagedFilesETag(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	post := mustDig(t, doc, "paths", "/agents/{name}/files", "post")
	rawPost, err := json.Marshal(post)
	require.NoError(t, err)
	require.Contains(t, string(rawPost), "#/components/parameters/IfMatch",
		"多文件提交必须引用 If-Match 参数")
	postResponses, ok := mustDig(t, doc, "paths", "/agents/{name}/files", "post", "responses").(map[string]any)
	require.True(t, ok)
	require.Contains(t, postResponses, "412", "条件写必须声明 412")

	get := mustDig(t, doc, "paths", "/agents/{name}/files", "get", "responses", "200")
	rawGet, err := json.Marshal(get)
	require.NoError(t, err)
	require.Contains(t, string(rawGet), `"ETag"`, "GET 必须声明 ETag 响应头")
	require.Contains(t, string(rawGet), "#/components/schemas/AgentFilesView")

	props := contractPropertyNames(t, doc, "AgentFilesView")
	require.True(t, props["etag"], "AgentFilesView 必须带 etag 字段")
	require.True(t, props["files"], "AgentFilesView 必须带文件清单")

	param, err := json.Marshal(mustDig(t, doc, "components", "parameters", "IfMatch"))
	require.NoError(t, err)
	require.Contains(t, string(param), `"required":true`, "If-Match 必须必填")
	require.Contains(t, string(param), "sha256:", "If-Match 形状固定为 sha256 强 ETag")
}

// TestContract_SaveAndAdmissionSplit 钉住 R4 的分段口径：同一批 sentinel 在保存面
// （`writeBindingError` / `writeCredentialError`，契约 §8 保存表）走 400 / 409 / 503，
// 在运行准入面（`writeRunAdmissionError`）走 409 / 503。码字面量两处必须是同一份——
// 分段只体现在状态码，不体现在码名，页面才能用一套码做提示。
// 保存专属码（模式未声明 / 范围仍被引用 / 跨资源借用 / 资源对该 Agent 不可用）不由
// 准入面认领：它们描述的是「改 AGENT.md 再存」，不是「资源状态已变，换绑后再跑」。
// 逆改点：准入面改回 400 / 保存面某条改 409 / 任一出口换码名 / 准入面认领保存专属码
// → 对应断言红。
// 中文用例名：保存面与准入面错误码分段。
func TestContract_SaveAndAdmissionSplit(t *testing.T) {
	s := newTestServer(t, nil)

	shared := []struct {
		name        string
		err         error
		binding     bool
		saveStatus  int
		admitStatus int
		code        string
	}{
		{
			"缺绑定", core.ErrAgentResourceBindingMissing, true,
			http.StatusBadRequest, http.StatusConflict, codeAgentResourceBindingMissing,
		},
		{
			"绑定非法", core.ErrAgentResourceBindingInvalid, true,
			http.StatusBadRequest, http.StatusConflict, codeAgentResourceBindingInvalid,
		},
		{
			"无资格", core.ErrResourceNotEligible, true,
			http.StatusBadRequest, http.StatusConflict, codeResourceNotEligible,
		},
		{
			"凭据已撤销", core.ErrCredentialRevoked, false,
			http.StatusBadRequest, http.StatusConflict, codeCredentialRevoked,
		},
		{
			"主密钥不可用", core.ErrCredentialUnavailable, false,
			http.StatusServiceUnavailable, http.StatusServiceUnavailable, codeCredentialUnavailable,
		},
	}
	for _, tc := range shared {
		t.Run(tc.name, func(t *testing.T) {
			save := httptest.NewRecorder()
			if tc.binding {
				require.True(t, s.writeBindingError(save, tc.err), "保存面绑定额必须由 writeBindingError 认领")
			} else {
				require.True(t, s.writeCredentialError(save, tc.err), "保存面凭据码必须由 writeCredentialError 认领")
			}
			require.Equal(t, tc.saveStatus, save.Code)
			require.Equal(t, tc.code, decodeAuthEnvelope(t, save).Code)

			admit := httptest.NewRecorder()
			s.writeRunError(admit, tc.err, context.Background())
			require.Equal(t, tc.admitStatus, admit.Code)
			require.Equal(t, tc.code, decodeAuthEnvelope(t, admit).Code, "两个出口的码字面量必须同一份")
		})
	}

	// 保存专属码：保存面的状态逐条锁定，准入面一律不认领（认领了就会把「改文件」
	// 和「资源状态已变」混成同一条提示）。
	saveOnly := []struct {
		name    string
		err     error
		binding bool
		status  int
		code    string
	}{
		{"模式未声明", core.ErrCredentialModeRequired, false, http.StatusBadRequest, codeCredentialModeRequired},
		{"范围仍被引用", core.ErrResourceScopeInUse, false, http.StatusConflict, codeResourceScopeInUse},
		{"跨资源借用", core.ErrCredentialOwnerMismatch, false, http.StatusConflict, codeCredentialOwnerMismatch},
		{
			"资源对该 Agent 不可用", core.ErrAgentResourceUnavailable, true,
			http.StatusBadRequest, codeAgentResourceUnavailable,
		},
	}
	for _, tc := range saveOnly {
		t.Run("保存专属："+tc.name, func(t *testing.T) {
			save := httptest.NewRecorder()
			claimed := s.writeBindingError(save, tc.err)
			if !tc.binding {
				claimed = s.writeCredentialError(save, tc.err)
			}
			require.True(t, claimed, "保存专属码必须由保存面出口认领")
			require.Equal(t, tc.status, save.Code)
			require.Equal(t, tc.code, decodeAuthEnvelope(t, save).Code)

			require.False(t, s.writeRunAdmissionError(httptest.NewRecorder(), tc.err),
				"%s 不得在准入面被认领：准入把同一条件并进各自的准入码（模块卡错误码表）", tc.code)
		})
	}

	// 两个保存出口各管一段，互不越界：越界会把码写成「另一个出口的语义」。
	t.Run("保存面两个出口互不越界", func(t *testing.T) {
		require.False(t, s.writeBindingError(httptest.NewRecorder(), core.ErrCredentialRevoked),
			"凭据码归 writeCredentialError")
		require.False(t, s.writeCredentialError(httptest.NewRecorder(), core.ErrAgentResourceBindingMissing),
			"绑定额归 writeBindingError")
	})
}

// TestContractAdmin_SkillDuplicateConflict 钉住 D-04：同名技能创建 → 409
// `CONFLICT`（不是 400），导入端点同样 409 且不静默覆盖；Go 侧常量与契约表的
// 状态一致。
// 逆改点：把 409 改回 400 / 删掉任一 409 声明 / 改 CONFLICT 的状态 → 红。
// 中文用例名：技能同名冲突（D-04）。
func TestContractAdmin_SkillDuplicateConflict(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	createResponses, ok := mustDig(t, doc, "paths", "/skills", "post", "responses").(map[string]any)
	require.True(t, ok)
	require.Contains(t, createResponses, "409", "同名创建必须声明 409")
	description, ok := mustDig(t, doc, "paths", "/skills", "post", "description").(string)
	require.True(t, ok)
	require.Contains(t, description, "CONFLICT", "409 的语义必须点名 CONFLICT")

	importResponses, ok := mustDig(t, doc, "paths", "/skills/import", "post", "responses").(map[string]any)
	require.True(t, ok)
	require.Contains(t, importResponses, "409", "导入同名技能必须 409，不静默覆盖")

	require.Equal(t, "CONFLICT", codeConflict)
	require.Equal(t, "409", contractErrorCodes(t)["CONFLICT"])
}

// operationParameterNames 收集一个操作的参数名：路径级与操作级两处声明都算，
// `$ref` 取组件名（如 IfMatch / ResourceUID），内联参数取 name 字段。
// operationParameterNames collects path-level and operation-level parameter names,
// resolving component refs to their component name.
func operationParameterNames(t *testing.T, doc map[string]any, rel, method string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	collect := func(node any) {
		items, ok := node.([]any)
		if !ok {
			return
		}
		for _, item := range items {
			entry, ok := item.(map[string]any)
			require.True(t, ok, "%s %s 的参数项必须是映射", method, rel)
			if ref, ok := entry["$ref"].(string); ok {
				parts := strings.Split(ref, "/")
				names[parts[len(parts)-1]] = true
				continue
			}
			if name, ok := entry["name"].(string); ok {
				names[name] = true
			}
		}
	}
	pathItem, ok := mustDig(t, doc, "paths", rel).(map[string]any)
	require.Truef(t, ok, "openapi.yaml 路径 %s 不是映射", rel)
	collect(pathItem["parameters"])
	operation, ok := pathItem[method].(map[string]any)
	require.Truef(t, ok, "openapi.yaml 缺少 %s %s", method, rel)
	collect(operation["parameters"])
	return names
}

// TestContractAdmin_RouteTableMatchesOpenAPI 钉住写面路由表与契约 `paths` 的双向
// 对应（模块卡 §4.4 的契约面）：35 条端点的方法与路径在 openapi.yaml 里都有同名
// 操作；11 条条件写都声明 412 与 `IfMatch` 参数；资源删除声明 `ResourceUID`
// （R4：删除只写墓碑，按 UID 定位，不接受仅按名删除）；反向再查一遍——这批契约
// 路径上出现的每个非 GET 方法都必须已登记进路由表，契约加了方法而表里没有同样红。
// 逆改点：漏登记端点 / 方法大小写漂移 / 条件写少声明 412 或 If-Match / 资源删除
// 少 ResourceUID / 契约新增一个未登记的方法 → 红。
// 中文用例名：写面路由表与 openapi 双向对账。
func TestContractAdmin_RouteTableMatchesOpenAPI(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	conditional := map[string]bool{}
	for _, key := range wantConditionalRouteKeys() {
		conditional[key] = true
	}
	// 资源删除（模型连接 / MCP / 技能 / 通知渠道）必须按 UID 定位；Agent 归档
	// 删除是另一条语义（按名归档，不写墓碑），不带 resource_uid。
	uidDeletePrefixes := []string{
		"DELETE /api/v1/model-connections/",
		"DELETE /api/v1/mcp-servers/",
		"DELETE /api/v1/skills/",
		"DELETE /api/v1/notify-channels/",
	}

	registered := map[string]map[string]bool{}
	for _, key := range wantAdminRouteKeys() {
		method, path, ok := strings.Cut(key, " ")
		require.True(t, ok, "路由键必须为 `方法 路径`：%q", key)
		require.True(t, strings.HasPrefix(path, "/api/v1/"), "新端点必须挂在 /api/v1 下：%s", path)
		rel := strings.TrimPrefix(path, "/api/v1")
		lower := strings.ToLower(method)

		operation, ok := mustDig(t, doc, "paths", rel, lower).(map[string]any)
		require.Truef(t, ok, "openapi.yaml 缺少 %s 的操作", key)

		responses, ok := operation["responses"].(map[string]any)
		require.Truef(t, ok, "%s 必须声明 responses", key)
		require.Containsf(t, responses, "200", "%s 必须声明成功响应", key)

		parameters := operationParameterNames(t, doc, rel, lower)
		if conditional[key] {
			require.Containsf(t, responses, "412", "%s 是条件写，契约必须声明 412", key)
			require.Truef(t, parameters["IfMatch"], "%s 是条件写，契约必须声明 If-Match 参数", key)
		}
		for _, prefix := range uidDeletePrefixes {
			if strings.HasPrefix(key, prefix) {
				require.Truef(t, parameters["ResourceUID"], "%s 必须声明 resource_uid 参数", key)
			}
		}

		if registered[rel] == nil {
			registered[rel] = map[string]bool{}
		}
		if lower != "get" {
			registered[rel][lower] = true
		}
	}

	for rel, want := range registered {
		pathItem, ok := mustDig(t, doc, "paths", rel).(map[string]any)
		require.True(t, ok, "openapi.yaml 路径 %s 不是映射", rel)
		got := map[string]bool{}
		for method := range pathItem {
			lower := strings.ToLower(method)
			switch lower {
			case "get", "parameters", "summary", "description", "servers", "$ref":
				continue
			}
			got[lower] = true
		}
		require.Equalf(t, want, got, "契约路径 %s 的非 GET 方法必须与写面路由表逐条一致", rel)
	}
}

// TestOpenAPI_AdminWriteViewsMatchContract 钉住 US-12 写面视图与契约字段的逐字
// 一致（含 R4 新增的四项）：`ResourceVersionView.resource_uid`、
// `ResourceReferencesView.bound_agents`、`AgentLifecycleView.agent_uid` /
// `uid_source`、`AgentConfigView.agent_uid`。字段多一个（把持久形态直接序列化）
// 或少一个（漏投影）都会红；更新后的生命周期视图在 v2 / v1 / 缺失三档上都必须
// 仍然过同一份契约 schema。
// 逆改点：漏投影任一 R4 字段 / 视图多出契约外字段 / 枚举或时间格式漂移 → 红。
// 中文用例名：写面视图字段集与契约一致（含 R4 身份字段）。
func TestOpenAPI_AdminWriteViewsMatchContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	createdAt := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixMilli()

	version := core.ResourceVersion{
		ID:             7,
		Kind:           core.ResourceModelConnection,
		ResourceID:     "primary",
		ResourceUID:    "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
		VersionNo:      2,
		ContentHash:    "sha256:def",
		CreatedAt:      createdAt,
		OperationID:    "op-123",
		StoragePath:    "/must/not/leak",
		DefinitionYAML: "must-not-leak",
	}
	requireViewMatchesSchema(t, doc, "ResourceVersionView",
		resourceVersionView(version, true, false, nil))

	revertedFrom := int64(1)
	requireViewMatchesSchema(t, doc, "ResourceVersionDetail",
		resourceVersionDetailView(core.ResourceVersionDetail{
			Version:               version,
			DefinitionYAML:        "base_url: https://example.test\n",
			Manifest:              []core.ResourceManifestFile{},
			Published:             true,
			Applied:               true,
			RevertedFromVersionID: &revertedFrom,
		}))
	requireViewMatchesSchema(t, doc, "ResourcePublishResult",
		resourcePublishResultView(core.ResourcePublishRequest{VersionID: &revertedFrom},
			core.ResourcePublishResult{Version: version}))

	// 引用视图：三项 R4 字段（resource_uid / bound_agents[].agent_uid /
	// archived）都在线上形态里。
	requireViewMatchesSchema(t, doc, "ResourceReferencesView",
		resourceReferencesView(core.ResourceReferences{
			Kind:               core.ResourceSkill,
			ID:                 "demo",
			ResourceUID:        "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B",
			PublishedVersionID: 3,
			Agents:             []string{"writer"},
			BoundAgents: []core.ResourceBoundAgent{
				{Agent: "writer", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C", Archived: true},
			},
			ActiveRuns: []string{"run-42"},
		}))

	requireViewMatchesSchema(t, doc, "CredentialRevokeResult", CredentialRevokeView{
		ResourceUID:     "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B",
		CredentialRef:   "cred_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D",
		CredentialState: "revoked",
		RevokedVersions: 2,
		RevocationEpoch: 1,
	})
	requireViewMatchesSchema(t, doc, "ResourceScopeResult", ResourceScopeView{
		ResourceUID:            "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B",
		Scope:                  "exclusive",
		OwnerAgent:             "writer",
		Revision:               4,
		ETag:                   `"sha256:abc"`,
		AffectedEligibleAgents: []string{"reviewer"},
	})

	requireViewMatchesSchema(t, doc, "ModelConnectionTestResult",
		modelConnectionTestResultView(core.ResourceTestResult{
			OK: true, LatencyMS: 12, Model: "gpt-test",
		}))
	// `stage` 自 US-14（D-03）起是契约枚举，成功路径恒为 `complete`；样例必须带上
	// 合法取值，否则校验真正在判的是"空串算不算合法枚举"而不是视图字段名。
	// The sample carries a valid stage because D-03 made it an enum.
	requireViewMatchesSchema(t, doc, "McpServerTestResult",
		mcpServerTestResultView(core.ResourceTestResult{OK: true, Tools: []string{"search"}, Stage: "complete"}))
	requireViewMatchesSchema(t, doc, "SkillValidateResult",
		skillValidateResultView(core.SkillValidateResult{Valid: true}))

	requireViewMatchesSchema(t, doc, "WorkspaceEntry",
		WorkspaceEntryView{Path: "agents/writer/AGENT.md", Type: "file", Size: 128})
	requireViewMatchesSchema(t, doc, "WorkspaceFile",
		WorkspaceFileView{Path: "a.md", Content: "# a", Size: 3})

	requireViewMatchesSchema(t, doc, "AgentContent",
		AgentContentView{Content: "name: writer\n", ETag: `"sha256:abc"`})

	// AgentConfigView：R4 的 agent_uid 与逐引用解析状态。
	requireViewMatchesSchema(t, doc, "AgentConfigView", AgentConfigView{
		Agent:            "writer",
		AgentUID:         "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C",
		ContentETag:      `"sha256:abc"`,
		NotifyMode:       "none",
		NotifyModeSource: NotifyModeSourceExplicit,
		References: []AgentConfigReferenceView{{
			ResourceKind:    "model_connection",
			RefName:         "primary",
			ResourceUID:     "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
			ResourceName:    "primary",
			BindingStatus:   BindingStatusBound,
			Published:       true,
			Eligible:        true,
			CredentialState: "active",
		}},
		BindingComplete: true,
	})

	// 生命周期三档：v2（state_file）、合法 v1（legacy_unrecorded）、缺失（空串）
	// 都必须过同一份 schema——R4 只加字段，不改变旧文件的合法形态。
	v2 := agentLifecycleView(core.LifecycleRecord{
		Agent:       "writer",
		AgentUID:    "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C",
		State:       core.AgentActive,
		Generation:  2,
		UpdatedAt:   createdAt,
		OperationID: "op-7",
	})
	require.Equal(t, lifecycleUIDSourceStateFile, v2.UIDSource)
	requireViewMatchesSchema(t, doc, "AgentLifecycleView", v2)

	v1 := agentLifecycleView(core.LifecycleRecord{
		Agent:     "writer",
		State:     core.AgentDisabled,
		UpdatedAt: createdAt,
	})
	require.Equal(t, lifecycleUIDSourceLegacy, v1.UIDSource)
	requireViewMatchesSchema(t, doc, "AgentLifecycleView", v1)

	missing := AgentLifecycleView{Agent: "writer", Valid: false, LifecycleError: "state file missing"}
	requireViewMatchesSchema(t, doc, "AgentLifecycleView", missing)

	// 内容写响应里的 lifecycle 允许零值（契约 AgentFilesView 的注 2026-10-22 INFO-2 A）：
	// 形状仍必须与 AgentLifecycleView 一致。
	requireViewMatchesSchema(t, doc, "AgentFilesView",
		agentFilesView(core.AgentFiles{
			Agent:     "writer",
			Lifecycle: core.AgentLifecycleView{Valid: true},
			Files:     []core.FileEntry{{Path: "AGENT.md", Size: 12}},
		}, `"sha256:abc"`))

	// 受控工具装配的变更随保存结果返回：两个数组恒非 nil（空 = `[]`，不是 `null`）。
	// The assembly delta always serializes as arrays, never null.
	assembled := agentFilesView(core.AgentFiles{
		Agent:   "writer",
		Changes: core.ToolChange{Added: []string{core.NotifyToolName}},
	}, `"sha256:abc"`)
	requireViewMatchesSchema(t, doc, "AgentFilesView", assembled)
	changeJSON, err := json.Marshal(assembled.Changes)
	require.NoError(t, err)
	require.JSONEq(t, `{"tools_added":["notify"],"tools_removed":[]}`, string(changeJSON))
	emptyJSON, err := json.Marshal(agentFilesView(core.AgentFiles{Agent: "writer"}, `"sha256:abc"`).Changes)
	require.NoError(t, err)
	require.JSONEq(t, `{"tools_added":[],"tools_removed":[]}`, string(emptyJSON))

	requireViewMatchesSchema(t, doc, "AgentArchiveView", agentArchiveView(core.AgentArchiveRecord{
		ArchiveID:   "writer-20261016T093000Z",
		AgentName:   "writer",
		ArchivedAt:  createdAt,
		ContentHash: "sha256:abc",
	}))
}

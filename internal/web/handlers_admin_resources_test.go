package web

// 本文件是资源写面 handler 的验收面（颗粒度文档 §3.2.1 的 19 例中，T026 承担
// 模型连接、版本列表与发布共 13 例；删除 / 引用与版本详情分别在 T041 / T042
// 补入同一文件）。断言分三层：① 状态码与固定错误码；② 脱敏（视图与失败信封
// 都不得回显定义正文或凭证）；③ handler 自身的副作用——本层**不得**读盘或补写
// 编排，因此凡 4xx 都必须做到工作区逐字节不变、管理操作台账零新增。
// "412 零写入、零操作行"的权威判定在适配器层（T025 的
// `TestModelConnection_StaleETagZeroWrite`）；本文件判的是 handler 没有额外副作用，
// 且把适配器的分类如实映射为契约状态码。
// This file is the handler-level acceptance surface for the resource write endpoints.
// Assertions cover status/code mapping, redaction, and the absence of handler-side
// side effects; the authoritative zero-write judgement belongs to the adapter tests.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errFakeResourceAdminUnused 标记用例没有为该端口方法准备行为。
// errFakeResourceAdminUnused marks a port method the case never intended to call.
var errFakeResourceAdminUnused = errFakeAgentAdminUnused

// fakeResourceAdminService 是可注入行为的 ResourceAdminService 替身：未设置的
// 方法按"意外调用"报错，避免用例悄悄依赖未定义行为。
// fakeResourceAdminService is a behavior-injected ResourceAdminService; an unset
// method reports an unexpected call.
type fakeResourceAdminService struct {
	upsertModelConnectionFn func(ctx context.Context, actor core.Actor, write ModelConnectionWrite, ifMatch string) (ModelConnectionView, error)
	upsertMCPServerFn       func(ctx context.Context, actor core.Actor, write MCPServerWrite, ifMatch string) (McpServerView, error)
	deleteFn                func(ctx context.Context, actor core.Actor, kind core.ResourceKind, id, ifMatch, resourceUID string) error
	referencesFn            func(ctx context.Context, kind core.ResourceKind, id string) (core.ResourceReferences, error)
	publishFn               func(ctx context.Context, actor core.Actor, req core.ResourcePublishRequest) (core.ResourcePublishResult, error)
	versionsFn              func(ctx context.Context, kind core.ResourceKind, id string) ([]core.ResourceVersionDetail, error)
	currentETagFn           func(ctx context.Context, kind core.ResourceKind, id string) (string, bool)
	versionDetailFn         func(ctx context.Context, versionID int64) (core.ResourceVersionDetail, error)
	testModelConnectionFn   func(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error)
	testMCPServerFn         func(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error)
	createSkillFn           func(ctx context.Context, actor core.Actor, write SkillCreateWrite) (SkillDetailView, error)
	importSkillFn           func(ctx context.Context, actor core.Actor, write SkillImportWrite) (SkillDetailView, error)
	updateSkillFn           func(ctx context.Context, actor core.Actor, name string, write SkillUpdateWrite, ifMatch string) (SkillDetailView, error)
	validateSkillFn         func(ctx context.Context, name string, write SkillUpdateWrite) (core.SkillValidateResult, error)
}

// UpsertModelConnection 委托模型连接创建的注入行为。
func (f *fakeResourceAdminService) UpsertModelConnection(ctx context.Context, actor core.Actor, write ModelConnectionWrite, ifMatch string) (ModelConnectionView, error) {
	if f.upsertModelConnectionFn == nil {
		return ModelConnectionView{}, errFakeResourceAdminUnused
	}
	return f.upsertModelConnectionFn(ctx, actor, write, ifMatch)
}

// UpsertMCPServer 委托 MCP 写入的注入行为（T048 起使用）。
func (f *fakeResourceAdminService) UpsertMCPServer(ctx context.Context, actor core.Actor, write MCPServerWrite, ifMatch string) (McpServerView, error) {
	if f.upsertMCPServerFn == nil {
		return McpServerView{}, errFakeResourceAdminUnused
	}
	return f.upsertMCPServerFn(ctx, actor, write, ifMatch)
}

// CreateSkill 委托技能创建的注入行为（T052 起使用）。
func (f *fakeResourceAdminService) CreateSkill(ctx context.Context, actor core.Actor, write SkillCreateWrite) (SkillDetailView, error) {
	if f.createSkillFn == nil {
		return SkillDetailView{}, errFakeResourceAdminUnused
	}
	return f.createSkillFn(ctx, actor, write)
}

// ImportSkill 委托技能导入的注入行为（T052 起使用）。
func (f *fakeResourceAdminService) ImportSkill(ctx context.Context, actor core.Actor, write SkillImportWrite) (SkillDetailView, error) {
	if f.importSkillFn == nil {
		return SkillDetailView{}, errFakeResourceAdminUnused
	}
	return f.importSkillFn(ctx, actor, write)
}

// UpdateSkill 委托技能全量替换的注入行为（T052 起使用）。
func (f *fakeResourceAdminService) UpdateSkill(ctx context.Context, actor core.Actor, name string, write SkillUpdateWrite, ifMatch string) (SkillDetailView, error) {
	if f.updateSkillFn == nil {
		return SkillDetailView{}, errFakeResourceAdminUnused
	}
	return f.updateSkillFn(ctx, actor, name, write, ifMatch)
}

// ValidateSkill 委托技能校验的注入行为（T052 起使用）。
func (f *fakeResourceAdminService) ValidateSkill(ctx context.Context, name string, write SkillUpdateWrite) (core.SkillValidateResult, error) {
	if f.validateSkillFn == nil {
		return core.SkillValidateResult{}, errFakeResourceAdminUnused
	}
	return f.validateSkillFn(ctx, name, write)
}

// Delete 委托删除的注入行为（T041 起使用）。
func (f *fakeResourceAdminService) Delete(ctx context.Context, actor core.Actor, kind core.ResourceKind, id, ifMatch,
	resourceUID string,
) error {
	if f.deleteFn == nil {
		return errFakeResourceAdminUnused
	}
	return f.deleteFn(ctx, actor, kind, id, ifMatch, resourceUID)
}

// Publish 委托发布的注入行为。
func (f *fakeResourceAdminService) Publish(ctx context.Context, actor core.Actor, req core.ResourcePublishRequest) (core.ResourcePublishResult, error) {
	if f.publishFn == nil {
		return core.ResourcePublishResult{}, errFakeResourceAdminUnused
	}
	return f.publishFn(ctx, actor, req)
}

// Versions 委托版本列表的注入行为。
func (f *fakeResourceAdminService) Versions(ctx context.Context, kind core.ResourceKind, id string) ([]core.ResourceVersionDetail, error) {
	if f.versionsFn == nil {
		return nil, errFakeResourceAdminUnused
	}
	return f.versionsFn(ctx, kind, id)
}

// CurrentETag 委托条件 ETag 的注入行为：形状级 412 的 `data.current_etag` 由它提供。
func (f *fakeResourceAdminService) CurrentETag(ctx context.Context, kind core.ResourceKind, id string) (string, bool) {
	if f.currentETagFn == nil {
		return "", false
	}
	return f.currentETagFn(ctx, kind, id)
}

// VersionDetail 委托版本详情的注入行为（T042 起使用）。
func (f *fakeResourceAdminService) VersionDetail(ctx context.Context, versionID int64) (core.ResourceVersionDetail, error) {
	if f.versionDetailFn == nil {
		return core.ResourceVersionDetail{}, errFakeResourceAdminUnused
	}
	return f.versionDetailFn(ctx, versionID)
}

// References 委托引用视图的注入行为（T041 起使用）。
func (f *fakeResourceAdminService) References(ctx context.Context, kind core.ResourceKind, id string) (core.ResourceReferences, error) {
	if f.referencesFn == nil {
		return core.ResourceReferences{}, errFakeResourceAdminUnused
	}
	return f.referencesFn(ctx, kind, id)
}

// TestModelConnection 委托模型连接测试的注入行为。
func (f *fakeResourceAdminService) TestModelConnection(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error) {
	if f.testModelConnectionFn == nil {
		return core.ResourceTestResult{}, errFakeResourceAdminUnused
	}
	return f.testModelConnectionFn(ctx, actor, name)
}

// TestMCPServer 委托 MCP 测试的注入行为（T048 起使用）。
func (f *fakeResourceAdminService) TestMCPServer(ctx context.Context, actor core.Actor, name string) (core.ResourceTestResult, error) {
	if f.testMCPServerFn == nil {
		return core.ResourceTestResult{}, errFakeResourceAdminUnused
	}
	return f.testMCPServerFn(ctx, actor, name)
}

// operationLedger 是管理操作台账的替身：只有适配器替身在"成功路径"写入一行，
// 因此它同时承载"某次写确实发生过"与"4xx 没有产生操作行"两类断言。
// operationLedger records simulated management-operation rows.
type operationLedger struct {
	mu   sync.Mutex
	rows []string
}

// add 追加一行操作记录。
func (l *operationLedger) add(row string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, row)
}

// snapshot 返回台账副本（按写入顺序）。
func (l *operationLedger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.rows))
	copy(out, l.rows)
	return out
}

// adminServer 是资源写面用例的装配：真实认证闸门 + 注入端口 + 真实临时工作区 +
// 操作台账替身。`Handler()` 走与生产完全相同的 handler 链（含 If-Match 中间件）。
// adminServer wires the real auth guard and handler chain over an injected port, a
// real temp workspace and a simulated operation ledger.
type adminServer struct {
	*Server
	workspace   string
	ledger      *operationLedger
	fake        *fakeResourceAdminService
	currentETag string
}

// Handler 暴露与生产一致的 handler 链供用例直接投递请求。
// Handler exposes the production handler chain.
func (a *adminServer) Handler() http.Handler { return a.Server.handler() }

// snapshotWorkspace 对工作区做逐文件 sha256 快照（相对路径 → 摘要），用于断言
// "4xx 前后逐字节不变"。
// snapshotWorkspace hashes every workspace file for before/after comparison.
func (a *adminServer) snapshotWorkspace(t *testing.T) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(a.workspace, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(a.workspace, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		snapshot[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	require.NoError(t, err)
	return snapshot
}

// equalWorkspace 比较快照；不一致时打印差异路径并返回 false。
// equalWorkspace reports whether the workspace is byte-identical to the snapshot.
func (a *adminServer) equalWorkspace(t *testing.T, before map[string]string) bool {
	t.Helper()
	after := a.snapshotWorkspace(t)
	if len(before) != len(after) {
		t.Logf("工作区文件数变化：%d → %d", len(before), len(after))
		return false
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Logf("工作区文件变化：%s", path)
			return false
		}
	}
	return true
}

// operations 返回当前台账快照。
// operations returns a copy of the ledger rows.
func (a *adminServer) operations(t *testing.T) []string {
	t.Helper()
	return a.ledger.snapshot()
}

// preconditionError 构造与适配器同形的 412：`data.current_etag` 是当前强 ETag
// （含双引号，见 core.FormatStrongETag），不携带定义正文。
// preconditionError mirrors the adapter's 412 carrier.
func (a *adminServer) preconditionError() error {
	return &core.AdminError{
		Kind:    core.ErrPreconditionFailed,
		Message: "预条件失败：If-Match 与当前发布版本不一致",
		Data:    map[string]any{"current_etag": a.currentETag},
	}
}

// seedConfig 是工作区的初始定义文件内容：一个已发布的模型连接。
// seedConfig is the initial definition file with one published model connection.
const seedConfig = "providers:\n  main:\n    base_url: https://api.example.com/v1\n    api_key: ${BASALT_MAIN_KEY}\n    default_model: model-a\n"

// workspaceETag 把文件字节渲染为强 ETag（夹具自用；适配器的 ETag 定义由 T025 判）。
// workspaceETag renders file bytes as a strong ETag for fixture purposes.
func workspaceETag(data []byte) string {
	sum := sha256.Sum256(data)
	return core.FormatStrongETag(hex.EncodeToString(sum[:]))
}

// resourceAuth 是资源写面 handler 用例的认证替身：直接注入固定 actor。认证矩阵
// （Host / Origin / 限速 / 凭据）由 `admin_routes_test.go` 与 `server_test.go` 判卷，
// 本文件只判写语义。颗粒度文档 §4.3.6 的原样回归用 `httptest.NewRequest` 直接构造
// 请求（默认 Host 为 `example.com`，真实闸门按"端口必须一致"判 403），这里挂替身
// 才能让该用例不改一字地走到 412 分支。
// resourceAuth injects a fixed actor: the auth matrix is judged elsewhere, and the
// card's verbatim case builds its request without an allowed host:port.
type resourceAuth struct{}

// Wrap 直接放行并注入固定 actor。
func (resourceAuth) Wrap(next AdminHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next(w, r, core.Actor{Subject: "resource-test", Method: core.AuthMethodBearer, Source: "test"})
	})
}

// newAdminServer 装配一个认证可用、端口可注入的资源写面服务器，并给"模型连接
// upsert / 发布 / 版本列表"装上默认行为：ETag 过期 → 412 且零副作用；成功则改写
// 工作区并追加一行操作记录。用例可覆盖任一行为。
// newAdminServer assembles the fixture with default port behavior: a stale ETag
// yields 412 with no side effects, while a success rewrites the file and records
// one operation row.
func newAdminServer(t *testing.T) *adminServer {
	t.Helper()
	workspace := t.TempDir()
	configPath := filepath.Join(workspace, "basalt.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(seedConfig), 0o600))

	ledger := &operationLedger{}
	srv := &adminServer{workspace: workspace, ledger: ledger}
	srv.currentETag = workspaceETag([]byte(seedConfig))

	fake := &fakeResourceAdminService{}
	fake.upsertModelConnectionFn = func(_ context.Context, _ core.Actor, write ModelConnectionWrite, ifMatch string) (ModelConnectionView, error) {
		if ifMatch != "" && ifMatch != srv.currentETag {
			return ModelConnectionView{}, srv.preconditionError()
		}
		ledger.add("upsert_model_connection:" + write.Name)
		updated := append([]byte(seedConfig),
			[]byte("  "+write.Name+":\n    base_url: "+write.BaseURL+"\n    default_model: "+write.DefaultModel+"\n")...)
		require.NoError(t, os.WriteFile(configPath, updated, 0o600))
		srv.currentETag = workspaceETag(updated)
		return ModelConnectionView{
			Name:               write.Name,
			BaseURL:            write.BaseURL,
			APIKeyEnv:          envNameOf(write.APIKey),
			DefaultModel:       write.DefaultModel,
			PublishedVersionID: 2,
			PublishedVersionNo: 2,
			AppliedVersionID:   2,
			AppliedVersionNo:   2,
			ETag:               srv.currentETag,
			ReferencedBy:       []string{},
		}, nil
	}
	fake.publishFn = func(_ context.Context, _ core.Actor, req core.ResourcePublishRequest) (core.ResourcePublishResult, error) {
		if req.ExpectedETag != srv.currentETag {
			return core.ResourcePublishResult{}, srv.preconditionError()
		}
		ledger.add("publish:" + string(req.Kind) + ":" + req.ID)
		return core.ResourcePublishResult{
			Version: core.ResourceVersion{
				ID:          3,
				Kind:        req.Kind,
				ResourceID:  req.ID,
				VersionNo:   3,
				ContentHash: strings.Repeat("b", 64),
				CreatedAt:   time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixMilli(),
				OperationID: "op-publish",
			},
			PendingRestart: false,
		}, nil
	}
	fake.versionsFn = func(_ context.Context, kind core.ResourceKind, id string) ([]core.ResourceVersionDetail, error) {
		if kind != core.ResourceModelConnection || id != "main" {
			return nil, core.ErrResourceNotFound
		}
		revertedFrom := int64(1)
		created := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixMilli()
		return []core.ResourceVersionDetail{
			{Version: core.ResourceVersion{
				ID: 1, Kind: kind, ResourceID: id, VersionNo: 1,
				ContentHash: strings.Repeat("a", 64), CreatedAt: created, OperationID: "op-1",
			}},
			{
				Version: core.ResourceVersion{
					ID: 2, Kind: kind, ResourceID: id, VersionNo: 2,
					ContentHash: strings.Repeat("b", 64), CreatedAt: created, OperationID: "op-2",
				},
				Published: true, Applied: true, RevertedFromVersionID: &revertedFrom,
			},
		}, nil
	}
	srv.fake = fake
	srv.Server = newTestServer(t, func(o *Options) {
		o.AdminAuth = resourceAuth{}
		o.ResourceAdminService = fake
	})
	return srv
}

// envNameOf 把 `${VAR}` 占位取成变量名；非占位原样返回（夹具不承担校验）。
// 省略 `api_key` 时写成 nil，等价于“未提交”，这里回空串。
// envNameOf extracts the variable name from a ${VAR} reference; a nil pointer
// means the field was omitted and yields the empty string.
func envNameOf(placeholder *string) string {
	if placeholder == nil {
		return ""
	}
	name := strings.TrimPrefix(*placeholder, "${")
	return strings.TrimSuffix(name, "}")
}

// resourceCall 构造一个已认证的管理写请求。
// resourceCall builds an authenticated admin request.
func resourceCall(method, path, body string) *http.Request {
	return withBearer(adminRequest(method, path, body), testToken)
}

// resourceEnvelope 是响应信封的解码形状：`data` 原样保留，由用例按端点解码。
// resourceEnvelope keeps data raw so each case decodes its own view.
type resourceEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// decodeResourceEnvelope 解码一次响应信封。
// decodeResourceEnvelope decodes one response envelope.
func decodeResourceEnvelope(t *testing.T, rec *httptest.ResponseRecorder) resourceEnvelope {
	t.Helper()
	var env resourceEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "body=%s", rec.Body.String())
	return env
}

// TestAdminResources_StaleETag412NoSnapshot 是颗粒度文档 §4.3.6 的原样回归用例：
// 过期 ETag 的修改请求返回 412，工作区逐字节不变，管理操作台账零新增。
// 逆改点：handler 在委托前写盘 / 补写操作行 / 按旧值重试 → 对应断言红。
// TestAdminResources_StaleETag412NoSnapshot is the verbatim regression from the card.
func TestAdminResources_StaleETag412NoSnapshot(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	before := srv.snapshotWorkspace(t)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/model-connections/main",
		strings.NewReader(`{"base_url":"https://x.example","default_model":"m2"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("If-Match", `"sha256:`+strings.Repeat("0", 64)+`"`)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
	if !srv.equalWorkspace(t, before) {
		t.Fatal("412 changed files or database")
	}
	if ops := srv.operations(t); len(ops) != 0 {
		t.Fatalf("operations = %d, want 0", len(ops))
	}
}

// TestModelConnection_StaleETag412：形状合法但过期的 If-Match → 412，错误码为
// PRECONDITION_FAILED，`data.current_etag` 给出当前强 ETag，且不回显定义正文。
// TestModelConnection_StaleETag412 asserts the 412 shape and the current ETag.
func TestModelConnection_StaleETag412(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	before := srv.snapshotWorkspace(t)
	req := resourceCall(http.MethodPut, "/api/v1/model-connections/main",
		`{"base_url":"https://x.example","default_model":"m2"}`)
	req.Header.Set("If-Match", core.FormatStrongETag(strings.Repeat("0", 64)))
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codePreconditionFailed, env.Code)
	require.Contains(t, env.Message, "预条件失败")
	var data struct {
		CurrentETag string `json:"current_etag"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, srv.currentETag, data.CurrentETag)
	require.NotContains(t, rec.Body.String(), "base_url", "412 不得回显定义正文")
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestModelConnection_MissingEnv503：凭证占位缺失 → 503 NOT_CONFIGURED，文案点名
// 环境变量、不回显占位值；工作区与台账零变化。
// TestModelConnection_MissingEnv503 names the variable without echoing any value.
func TestModelConnection_MissingEnv503(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.upsertModelConnectionFn = func(context.Context, core.Actor, ModelConnectionWrite, string) (ModelConnectionView, error) {
		return ModelConnectionView{}, &core.AdminError{
			Kind:    core.ErrProviderNotConfigured,
			Message: "模型连接引用的环境变量 BASALT_MAIN_KEY 未设置",
		}
	}
	before := srv.snapshotWorkspace(t)
	req := resourceCall(http.MethodPut, "/api/v1/model-connections/main",
		`{"base_url":"https://x.example","default_model":"m2"}`)
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeNotConfigured, env.Code)
	require.Contains(t, env.Message, "BASALT_MAIN_KEY")
	require.NotContains(t, rec.Body.String(), "${")
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestModelConnection_TestTimeout504：适配器报截止时间已过 → 504 TIMEOUT；
// 测试端点不写管理操作行。
// TestModelConnection_TestTimeout504 maps a deadline to 504 with no operation row.
func TestModelConnection_TestTimeout504(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.testModelConnectionFn = func(context.Context, core.Actor, string) (core.ResourceTestResult, error) {
		return core.ResourceTestResult{}, &core.AdminError{Kind: context.DeadlineExceeded, Message: "模型连接测试超时"}
	}
	before := srv.snapshotWorkspace(t)
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/model-connections/main/test", ""))

	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeTimeout, env.Code)
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestModelConnection_TestOKFalse：外部调用完成但结论是失败 → 200 + `ok=false`，
// 错误码只放固定短语，不携带凭证；"调用发生过"与"结论失败"是两件事。
// TestModelConnection_TestOKFalse keeps a completed failed call at 200 with ok=false.
func TestModelConnection_TestOKFalse(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.testModelConnectionFn = func(context.Context, core.Actor, string) (core.ResourceTestResult, error) {
		return core.ResourceTestResult{
			OK:           false,
			LatencyMS:    42,
			ErrorCode:    "connection_failed",
			ErrorMessage: "连接失败：example.com",
		}, nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/model-connections/main/test", ""))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view ModelConnectionTestResultView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.False(t, view.OK)
	require.Equal(t, int64(42), view.LatencyMS)
	require.Equal(t, "connection_failed", view.ErrorCode)
	require.NotContains(t, rec.Body.String(), "${")
}

// TestModelConnection_CreateSuccess：创建即发布，响应是适配器给出的视图；凭证只以
// 变量名出站（`api_key_env`），不回显占位原文。
// TestModelConnection_CreateSuccess projects the post-create view with the env name.
func TestModelConnection_CreateSuccess(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/model-connections",
		`{"name":"backup","base_url":"https://b.example/v1","api_key":"${BASALT_BACKUP_KEY}","default_model":"m-b"}`))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view ModelConnectionView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, "backup", view.Name)
	require.Equal(t, "BASALT_BACKUP_KEY", view.APIKeyEnv)
	require.Equal(t, int64(2), view.PublishedVersionID)
	require.NotContains(t, rec.Body.String(), "${", "视图不得回显占位原文")
	require.Equal(t, []string{"upsert_model_connection:backup"}, srv.operations(t))
}

// TestModelConnection_UpdateNameMismatch400：路径名与请求体 name 冲突 → 400，
// 不触达端口（故零写入、零操作行）。
// TestModelConnection_UpdateNameMismatch400 rejects before the port is reached.
func TestModelConnection_UpdateNameMismatch400(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.upsertModelConnectionFn = func(context.Context, core.Actor, ModelConnectionWrite, string) (ModelConnectionView, error) {
		t.Fatal("路径与请求体不一致时不得触达端口")
		return ModelConnectionView{}, nil
	}
	before := srv.snapshotWorkspace(t)
	req := resourceCall(http.MethodPut, "/api/v1/model-connections/main",
		`{"name":"other","base_url":"https://x.example","default_model":"m2"}`)
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Equal(t, messageResourceNameMismatch, env.Message)
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResourceVersions_Success：版本列表按适配器给的顺序投影，指针状态与
// 撤回来源原样出站；created_at 统一为 RFC3339 UTC。
// TestAdminResourceVersions_Success projects rows, pointer flags and the revert source.
func TestAdminResourceVersions_Success(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	rec := serveAdmin(srv.Server, resourceCall(http.MethodGet, "/api/v1/resources/model_connection/main/versions", ""))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var views []ResourceVersionView
	require.NoError(t, json.Unmarshal(env.Data, &views))
	require.Len(t, views, 2)
	require.Equal(t, int64(1), views[0].VersionID)
	require.False(t, views[0].Published)
	require.Nil(t, views[0].RevertedFromVersionID)
	require.Equal(t, int64(2), views[1].VersionID)
	require.True(t, views[1].Published)
	require.True(t, views[1].Applied)
	require.NotNil(t, views[1].RevertedFromVersionID)
	require.Equal(t, int64(1), *views[1].RevertedFromVersionID)
	require.Equal(t, "2026-10-18T12:00:00Z", views[0].CreatedAt)
}

// TestAdminResourceVersions_UnknownKind404：未知资源类型 → 404，且不触达端口
// （与"资源不存在"共用状态码，不提供按类型拼写的枚举信号）。
// TestAdminResourceVersions_UnknownKind404 rejects before the port.
func TestAdminResourceVersions_UnknownKind404(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.versionsFn = func(context.Context, core.ResourceKind, string) ([]core.ResourceVersionDetail, error) {
		t.Fatal("未知资源类型不得触达端口")
		return nil, nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodGet, "/api/v1/resources/plugin/main/versions", ""))

	require.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Equal(t, messageResourceKind, env.Message)
}

// TestAdminResourcePublish_RevertCreatesNewVersion：带旧版本号发布时，响应是新版本，
// `reverted_from_version_id` 指回来源版本；模型连接发布即生效（applied=true）。
// TestAdminResourcePublish_RevertCreatesNewVersion derives the revert source.
func TestAdminResourcePublish_RevertCreatesNewVersion(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	req := resourceCall(http.MethodPost, "/api/v1/resources/model_connection/main/publish", `{"version_id":2}`)
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view ResourcePublishResultView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, int64(3), view.Version.VersionID)
	require.True(t, view.Version.Published)
	require.True(t, view.Version.Applied)
	require.False(t, view.PendingRestart)
	require.NotNil(t, view.Version.RevertedFromVersionID)
	require.Equal(t, int64(2), *view.Version.RevertedFromVersionID)
	require.Equal(t, []string{"publish:model_connection:main"}, srv.operations(t))
}

// TestAdminResourcePublish_OptionalBodyPublishesLatest：请求体可省略，缺省即
// "发布最新"（幂等发布，撤回来源保持 null）。
// TestAdminResourcePublish_OptionalBodyPublishesLatest accepts an omitted body.
func TestAdminResourcePublish_OptionalBodyPublishesLatest(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.publishFn = func(_ context.Context, _ core.Actor, req core.ResourcePublishRequest) (core.ResourcePublishResult, error) {
		require.Nil(t, req.VersionID, "省略请求体必须表示发布最新")
		require.Equal(t, core.ResourceModelConnection, req.Kind)
		require.Equal(t, "main", req.ID)
		return core.ResourcePublishResult{
			Version: core.ResourceVersion{
				ID:          2,
				Kind:        core.ResourceModelConnection,
				ResourceID:  "main",
				VersionNo:   2,
				ContentHash: strings.Repeat("b", 64),
				CreatedAt:   time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixMilli(),
				OperationID: "op-2",
			},
		}, nil
	}
	req := resourceCall(http.MethodPost, "/api/v1/resources/model_connection/main/publish", "")
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view ResourcePublishResultView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, int64(2), view.Version.VersionID)
	require.Nil(t, view.Version.RevertedFromVersionID)
}

// TestAdminResourcePublish_Precondition412：过期 If-Match → 412 + `current_etag`，
// 零写入、零操作行（发布路径与修改路径共用同一映射）。
// TestAdminResourcePublish_Precondition412 shares the 412 mapping with updates.
func TestAdminResourcePublish_Precondition412(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	before := srv.snapshotWorkspace(t)
	req := resourceCall(http.MethodPost, "/api/v1/resources/model_connection/main/publish", "")
	req.Header.Set("If-Match", core.FormatStrongETag(strings.Repeat("0", 64)))
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codePreconditionFailed, env.Code)
	var data struct {
		CurrentETag string `json:"current_etag"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, srv.currentETag, data.CurrentETag)
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_MissingPort500：资源写面各端点在端口缺失时都按装配错误回固定
// 500 INTERNAL（不是空结果，也不是客户端的错）。
// TestAdminResources_MissingPort500 treats a missing port as an assembly error.
func TestAdminResources_MissingPort500(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
	etag := core.FormatStrongETag(strings.Repeat("a", 64))
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		etag   bool
	}{
		{name: "create", method: http.MethodPost, path: "/api/v1/model-connections", body: `{"name":"main"}`},
		{name: "update", method: http.MethodPut, path: "/api/v1/model-connections/main", body: `{"default_model":"m2"}`, etag: true},
		{name: "test", method: http.MethodPost, path: "/api/v1/model-connections/main/test"},
		{name: "versions", method: http.MethodGet, path: "/api/v1/resources/model_connection/main/versions"},
		{name: "publish", method: http.MethodPost, path: "/api/v1/resources/model_connection/main/publish", etag: true},
		{name: "delete-model", method: http.MethodDelete, path: "/api/v1/model-connections/main", etag: true},
		{name: "delete-mcp", method: http.MethodDelete, path: "/api/v1/mcp-servers/main", etag: true},
		{name: "delete-skill", method: http.MethodDelete, path: "/api/v1/skills/main", etag: true},
		{name: "references", method: http.MethodGet, path: "/api/v1/resources/model_connection/main/references"},
		{name: "version-detail", method: http.MethodGet, path: "/api/v1/resource-versions/1"},
		{name: "create-skill", method: http.MethodPost, path: "/api/v1/skills", body: `{"name":"main","description":"d"}`},
		{name: "import-skill", method: http.MethodPost, path: "/api/v1/skills/import", body: `{"name":"main","files":[{"path":"SKILL.md","content":"# x\n"}]}`},
		{name: "update-skill", method: http.MethodPut, path: "/api/v1/skills/main", body: `{"files":[{"path":"SKILL.md","content":"# x\n"}]}`, etag: true},
		{name: "validate-skill", method: http.MethodPost, path: "/api/v1/skills/main/validate", body: `{"files":[{"path":"SKILL.md","content":"# x\n"}]}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			req := resourceCall(tc.method, tc.path, tc.body)
			if tc.etag {
				req.Header.Set("If-Match", etag)
			}
			rec := serveAdmin(s, req)
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			env := decodeResourceEnvelope(t, rec)
			require.Equal(t, codeInternal, env.Code)
			require.Equal(t, messageInternal, env.Message)
		})
	}
}

// TestAdminResources_DeleteModel 判删除模型连接的 handler 面：路径名、逐字
// `If-Match` 与 `resource_uid` 查询参数原样进端口，成功回 200 + 空 data；handler
// 自身不写操作行、不替调用方猜身份。
// TestAdminResources_DeleteModel forwards the name, literal If-Match and the
// resource_uid query parameter, then returns 200 with empty data.
func TestAdminResources_DeleteModel(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	var gotKind core.ResourceKind
	var gotID, gotIfMatch, gotUID string
	srv.fake.deleteFn = func(_ context.Context, _ core.Actor, kind core.ResourceKind, id, ifMatch, resourceUID string) error {
		gotKind, gotID, gotIfMatch, gotUID = kind, id, ifMatch, resourceUID
		return nil
	}
	const uid = "rs_0123456789ABCDEFGHJKMNPQRS"
	req := resourceCall(http.MethodDelete, "/api/v1/model-connections/main?resource_uid="+uid, "")
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Empty(t, env.Data, "删除成功应回空 data（信封省略 data 键）")
	require.Equal(t, core.ResourceModelConnection, gotKind)
	require.Equal(t, "main", gotID)
	require.Equal(t, srv.currentETag, gotIfMatch)
	require.Equal(t, uid, gotUID, "resource_uid 必须原样进端口")
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_DeleteMissingUID409 判 `resource_uid` 缺失：handler 不拦，
// 由适配器回 `ErrResourceUIDMismatch`，信封给 409 RESOURCE_UID_MISMATCH 与当前身份，
// 客户端据此重新读取——按名称猜目标被契约禁止。
// TestAdminResources_DeleteMissingUID409 maps a missing uid to 409 with the current
// identity, so a stale page can never delete a rebuilt instance.
func TestAdminResources_DeleteMissingUID409(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	before := srv.snapshotWorkspace(t)
	const current = "rs_0123456789ABCDEFGHJKMNPQRS"
	srv.fake.deleteFn = func(_ context.Context, _ core.Actor, _ core.ResourceKind, _, _, resourceUID string) error {
		if resourceUID != "" {
			return nil
		}
		return &core.AdminError{
			Kind:    core.ErrResourceUIDMismatch,
			Message: "model_connection main 的资源身份缺失：请求必须携带与当前指针一致的 resource_uid",
			Data:    map[string]any{"resource_uid": current},
		}
	}
	req := resourceCall(http.MethodDelete, "/api/v1/model-connections/main", "")
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusConflict, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeResourceUIDMismatch, env.Code)
	var data struct {
		ResourceUID string `json:"resource_uid"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, current, data.ResourceUID, "必须给出当前身份供客户端重新读取")
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_DeleteMCP 判 `DELETE /mcp-servers/{name}` 走同一删除流程，
// 资源类型为 `mcp_server`（路由与类型不得串线）。
// TestAdminResources_DeleteMCP pins the mcp_server kind on the shared delete flow.
func TestAdminResources_DeleteMCP(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	var gotKind core.ResourceKind
	srv.fake.deleteFn = func(_ context.Context, _ core.Actor, kind core.ResourceKind, _, _, _ string) error {
		gotKind = kind
		return nil
	}
	req := resourceCall(http.MethodDelete, "/api/v1/mcp-servers/fetch", "")
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, core.ResourceMCPServer, gotKind)
}

// TestAdminResources_DeleteSkill 判 `DELETE /skills/{name}` 走同一删除流程，资源
// 类型为 `skill`。
// TestAdminResources_DeleteSkill pins the skill kind on the shared delete flow.
func TestAdminResources_DeleteSkill(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	var gotKind core.ResourceKind
	srv.fake.deleteFn = func(_ context.Context, _ core.Actor, kind core.ResourceKind, _, _, _ string) error {
		gotKind = kind
		return nil
	}
	req := resourceCall(http.MethodDelete, "/api/v1/skills/writing", "")
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, core.ResourceSkill, gotKind)
}

// TestAdminResources_DeleteReferenced 是删除引用闸门的 HTTP 面：适配器以
// `AdminError{Kind: InvalidInputError, Data: {agents, active_runs}}` 拒绝 → 400
// BAD_REQUEST，信封携带两个点名数组（空数组也保留），工作区不变、操作台账零新增。
// TestAdminResources_DeleteReferenced maps the reference gate to 400 with data.agents
// and data.active_runs, leaving the workspace untouched.
func TestAdminResources_DeleteReferenced(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	before := srv.snapshotWorkspace(t)
	const message = "资源被引用：agents=agent-a"
	srv.fake.deleteFn = func(_ context.Context, _ core.Actor, _ core.ResourceKind, _, _, _ string) error {
		return &core.AdminError{
			Kind:    &core.InvalidInputError{Tool: "model_connection.delete", Reason: message},
			Message: message,
			Data: map[string]any{
				"agents":       []string{"agent-a"},
				"active_runs":  []string{},
				"operation_id": "op-referenced",
			},
		}
	}
	req := resourceCall(http.MethodDelete, "/api/v1/model-connections/main", "")
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Equal(t, message, env.Message)
	var data map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.JSONEq(t, `["agent-a"]`, string(data["agents"]))
	require.JSONEq(t, `[]`, string(data["active_runs"]), "空数组也要保留键")
	require.JSONEq(t, `"op-referenced"`, string(data["operation_id"]),
		"pending 之后的引用闸门失败必须可追查（约束 1a）")
	require.NotContains(t, rec.Body.String(), "providers:", "失败信封不得回显定义正文")
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_DeleteStaleETag412：形状合法但过期的 If-Match 由适配器判
// 412，信封带 `data.current_etag`，零写入、零操作行（与修改路径共用映射）。
// TestAdminResources_DeleteStaleETag412 shares the 412 mapping with updates.
func TestAdminResources_DeleteStaleETag412(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	before := srv.snapshotWorkspace(t)
	srv.fake.deleteFn = func(_ context.Context, _ core.Actor, _ core.ResourceKind, _, _, _ string) error {
		return srv.preconditionError()
	}
	req := resourceCall(http.MethodDelete, "/api/v1/model-connections/main", "")
	req.Header.Set("If-Match", core.FormatStrongETag(strings.Repeat("0", 64)))
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codePreconditionFailed, env.Code)
	var data struct {
		CurrentETag string `json:"current_etag"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, srv.currentETag, data.CurrentETag)
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_DeleteMissingIfMatch412 判条件写中间件：删除缺少 If-Match
// 时在进入 handler 之前就 412，端口调用次数为零（不产生操作行）。
// TestAdminResources_DeleteMissingIfMatch412 proves the middleware rejects before the
// port is reached.
func TestAdminResources_DeleteMissingIfMatch412(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	called := false
	srv.fake.deleteFn = func(_ context.Context, _ core.Actor, _ core.ResourceKind, _, _, _ string) error {
		called = true
		return nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodDelete, "/api/v1/skills/writing", ""))

	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	require.False(t, called, "缺 If-Match 不得触达端口")
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codePreconditionFailed, env.Code)
	require.Equal(t, ifMatchRequiredMessage, env.Message)
}

// TestAdminResources_References 判引用视图：kind / id / 发布版本与两个数组原样
// 投影，空数组输出 `[]` 而不是 null。
// TestAdminResources_References projects the joined reference view with non-nil arrays.
func TestAdminResources_References(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.referencesFn = func(_ context.Context, kind core.ResourceKind, id string) (core.ResourceReferences, error) {
		return core.ResourceReferences{
			Kind:               kind,
			ID:                 id,
			ResourceUID:        "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
			PublishedVersionID: 7,
			Agents:             []string{"archive:20261018T120000Z-1", "agent-a"},
			BoundAgents: []core.ResourceBoundAgent{
				{Agent: "agent-a", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"},
				{Agent: "archived-b", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B", Archived: true},
			},
			ActiveRuns: []string{"run-1", "run-2"},
		}, nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodGet, "/api/v1/resources/skill/writing/references", ""))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view ResourceReferencesView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, ResourceReferencesView{
		Kind:               "skill",
		ID:                 "writing",
		ResourceUID:        "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
		PublishedVersionID: 7,
		Agents:             []string{"archive:20261018T120000Z-1", "agent-a"},
		BoundAgents: []ResourceBoundAgentView{
			{Agent: "agent-a", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"},
			{Agent: "archived-b", AgentUID: "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B", Archived: true},
		},
		ActiveRuns: []string{"run-1", "run-2"},
	}, view)
	require.NotContains(t, rec.Body.String(), "providers:")
}

// TestAdminResources_ReferencesNotFound：不存在的资源 → 404 NOT_FOUND，文案只
// 点名资源标识；已删除资源仍可查的分支在适配器层（`TestReferences_*`）。
// TestAdminResources_ReferencesNotFound maps a missing resource to a naming 404.
func TestAdminResources_ReferencesNotFound(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.referencesFn = func(_ context.Context, _ core.ResourceKind, _ string) (core.ResourceReferences, error) {
		return core.ResourceReferences{}, core.ErrResourceNotFound
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodGet, "/api/v1/resources/model_connection/gone/references", ""))

	require.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Equal(t, messageResourceNotFoundPrefix+"gone", env.Message)
}

// TestAdminResources_ReferencesUnknownKind：未知资源类型在 handler 之前就 404，
// 端口调用次数为零（不拿状态码枚举资源种类）。
// TestAdminResources_ReferencesUnknownKind rejects non-frozen kinds before the port.
func TestAdminResources_ReferencesUnknownKind(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	called := false
	srv.fake.referencesFn = func(_ context.Context, _ core.ResourceKind, _ string) (core.ResourceReferences, error) {
		called = true
		return core.ResourceReferences{}, nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodGet, "/api/v1/resources/widget/main/references", ""))

	require.Equal(t, http.StatusNotFound, rec.Code)
	require.False(t, called, "未知资源类型不得触达端口")
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Equal(t, messageResourceKind, env.Message)
}

// TestAdminResources_VersionDetail 判版本详情的成功投影：路径版本号原样进端口，
// 版本行字段摊平后与 `definition_yaml` / `manifest` 一起输出；只读端点不写管理
// 操作行，工作区逐字节不变。
// TestAdminResources_VersionDetail projects the flattened detail and writes nothing.
func TestAdminResources_VersionDetail(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	revertedFrom := int64(1)
	created := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixMilli()
	detail := core.ResourceVersionDetail{
		Version: core.ResourceVersion{
			ID: 2, Kind: core.ResourceModelConnection, ResourceID: "main", VersionNo: 2,
			ContentHash:    strings.Repeat("b", 64),
			DefinitionYAML: "base_url: https://a.example\napi_key: ${BASALT_MAIN_KEY}\ndefault_model: m1\n",
			CreatedAt:      created, OperationID: "op-2",
		},
		DefinitionYAML:        "base_url: https://a.example\napi_key: ${BASALT_MAIN_KEY}\ndefault_model: m1\n",
		Published:             true,
		Applied:               true,
		RevertedFromVersionID: &revertedFrom,
	}
	srv.fake.versionDetailFn = func(_ context.Context, versionID int64) (core.ResourceVersionDetail, error) {
		require.Equal(t, int64(2), versionID, "路径版本号必须原样进端口")
		return detail, nil
	}
	before := srv.snapshotWorkspace(t)
	rec := serveAdmin(srv.Server, resourceCall(http.MethodGet, "/api/v1/resource-versions/2", ""))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view ResourceVersionDetailView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, int64(2), view.VersionID)
	require.Equal(t, "model_connection", view.ResourceKind)
	require.Equal(t, "main", view.ResourceID)
	require.Equal(t, 2, view.VersionNo)
	require.Equal(t, detail.DefinitionYAML, view.DefinitionYAML)
	require.True(t, view.Published)
	require.True(t, view.Applied)
	require.NotNil(t, view.RevertedFromVersionID)
	require.Equal(t, int64(1), *view.RevertedFromVersionID)
	require.Empty(t, view.Manifest.Files, "模型连接没有快照清单")
	require.Contains(t, string(env.Data), `"definition_yaml"`, "契约字段必须出现（含空串）")
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t), "只读端点不得写管理操作行")
}

// TestAdminResources_VersionDetailSkillEmptyDefinition：技能版本的 `definition_yaml`
// 恒为空串（定义在不可变快照目录，不在版本行），`manifest` 输出文件清单。
// TestAdminResources_VersionDetailSkillEmptyDefinition keeps skill definitions empty and
// projects the snapshot manifest.
func TestAdminResources_VersionDetailSkillEmptyDefinition(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.versionDetailFn = func(_ context.Context, versionID int64) (core.ResourceVersionDetail, error) {
		require.Equal(t, int64(9), versionID)
		return core.ResourceVersionDetail{
			Version: core.ResourceVersion{
				ID: 9, Kind: core.ResourceSkill, ResourceID: "writing", VersionNo: 1,
				ContentHash:  strings.Repeat("d", 64),
				ManifestJSON: `[{"path":"SKILL.md","sha256":"` + strings.Repeat("e", 64) + `","size":12}]`,
				StoragePath:  "skills/.versions/writing/1",
				CreatedAt:    time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixMilli(),
				OperationID:  "op-skill",
			},
			Manifest: []core.ResourceManifestFile{
				{Path: "SKILL.md", SHA256: strings.Repeat("e", 64), Size: 12},
			},
			Published: true,
			Applied:   true,
		}, nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodGet, "/api/v1/resource-versions/9", ""))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	var view ResourceVersionDetailView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, "", view.DefinitionYAML, "技能 definition_yaml 恒为空串")
	require.Equal(t, []ResourceManifestFileView{
		{Path: "SKILL.md", SHA256: strings.Repeat("e", 64), Size: 12},
	}, view.Manifest.Files)
	require.Contains(t, string(env.Data), `"definition_yaml":""`)
	require.NotContains(t, string(env.Data), "SKILL.md 的正文", "清单只含路径 / 哈希 / 字节数")
}

// TestAdminResources_VersionDetailRedaction 判脱敏 fail-closed 的 handler 面：适配器
// 拒绝返回时按内部错误映射 500 + 固定文案——适配器的错误原文（可能提到命中的字段）
// 与任何定义正文都不进出站信封。
// TestAdminResources_VersionDetailRedaction maps the adapter's refusal to a fixed 500
// without echoing the cause or any definition text.
func TestAdminResources_VersionDetailRedaction(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.versionDetailFn = func(_ context.Context, _ int64) (core.ResourceVersionDetail, error) {
		return core.ResourceVersionDetail{}, errors.New("资源版本 9 的定义含未脱敏凭证 sk-live-plaintext")
	}
	before := srv.snapshotWorkspace(t)
	rec := serveAdmin(srv.Server, resourceCall(http.MethodGet, "/api/v1/resource-versions/9", ""))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeInternal, env.Code)
	require.Equal(t, messageInternal, env.Message)
	require.NotContains(t, rec.Body.String(), "sk-live-plaintext", "不得泄露凭证值")
	require.NotContains(t, rec.Body.String(), "未脱敏", "适配器错误原文不得外传")
	require.True(t, srv.equalWorkspace(t, before))
}

// TestAdminResources_VersionDetailBadID404：非数字 / ≤0 的路径版本号在 handler 上
// 与"不存在"同一 404（不做 400 区分），端口零调用。
// TestAdminResources_VersionDetailBadID404 maps every malformed id to 404 before the port.
func TestAdminResources_VersionDetailBadID404(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	called := false
	srv.fake.versionDetailFn = func(_ context.Context, _ int64) (core.ResourceVersionDetail, error) {
		called = true
		return core.ResourceVersionDetail{}, nil
	}
	for _, raw := range []string{"abc", "0", "-1", "1.5", "0x10"} {
		rec := serveAdmin(srv.Server, resourceCall(http.MethodGet, "/api/v1/resource-versions/"+raw, ""))
		require.Equal(t, http.StatusNotFound, rec.Code, "raw=%s", raw)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codeNotFound, env.Code, "raw=%s", raw)
		require.Equal(t, messageResourceNotFoundPrefix+raw, env.Message, "raw=%s", raw)
	}
	require.False(t, called, "不合法版本号不得触达端口")
}

// mcpSampleView 是 MCP 写入成功后的契约视图样本：`command` 只放可执行名，env 只出
// 变量键名（约束 23）。
// mcpSampleView is the contract view sample; command is the executable only and env
// contributes key names alone.
func mcpSampleView(name string) McpServerView {
	return McpServerView{
		Name:               name,
		Transport:          "stdio",
		Command:            "mcp-fetch",
		EnvKeys:            []string{"MCP_TOKEN"},
		Tools:              []string{},
		PublishedVersionID: 2,
		PublishedVersionNo: 2,
		PendingRestart:     true,
	}
}

// TestAdminResources_CreateMCP：创建走空 ifMatch（端口读作"创建"而不是条件修改），
// 请求体逐字段投影到端口；响应只回契约字段。
// TestAdminResources_CreateMCP forwards an empty ifMatch and the full wire body.
func TestAdminResources_CreateMCP(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	var got MCPServerWrite
	var gotIfMatch string
	srv.fake.upsertMCPServerFn = func(_ context.Context, _ core.Actor, write MCPServerWrite, ifMatch string) (McpServerView, error) {
		got = write
		gotIfMatch = ifMatch
		return mcpSampleView(write.Name), nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/mcp-servers",
		`{"name":"fetch","transport":"stdio","command":"mcp-fetch",`+
			`"args":["--root","./workspace"],"env":{"MCP_TOKEN":"${MCP_TOKEN}"}}`))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view McpServerView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, "fetch", view.Name)
	require.Equal(t, "stdio", view.Transport)
	require.Equal(t, "mcp-fetch", view.Command)
	require.True(t, view.PendingRestart)
	require.Empty(t, gotIfMatch, "创建必须传空 ifMatch")
	require.Equal(t, []string{"--root", "./workspace"}, got.Args)
	require.Equal(t, map[string]string{"MCP_TOKEN": "${MCP_TOKEN}"}, got.Env)
	require.NotContains(t, rec.Body.String(), "${", "视图不得回显占位原文")
}

// TestMCP_StaleETag412：形状合法但过期的 If-Match → 412 + `data.current_etag`，
// `If-Match` 原样传到端口；工作区逐字节不变、管理操作台账零新增。
// TestMCP_StaleETag412 asserts the 412 shape and that If-Match crosses unchanged.
func TestMCP_StaleETag412(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	stale := core.FormatStrongETag(strings.Repeat("0", 64))
	var gotIfMatch string
	srv.fake.upsertMCPServerFn = func(_ context.Context, _ core.Actor, _ MCPServerWrite, ifMatch string) (McpServerView, error) {
		gotIfMatch = ifMatch
		return McpServerView{}, srv.preconditionError()
	}
	before := srv.snapshotWorkspace(t)
	req := resourceCall(http.MethodPut, "/api/v1/mcp-servers/fetch",
		`{"transport":"http","url":"https://mcp.example/mcp"}`)
	req.Header.Set("If-Match", stale)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codePreconditionFailed, env.Code)
	var data struct {
		CurrentETag string `json:"current_etag"`
	}
	require.NoError(t, json.Unmarshal(env.Data, &data))
	require.Equal(t, srv.currentETag, data.CurrentETag)
	require.Equal(t, stale, gotIfMatch, "If-Match 必须原样传给端口")
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_UpdateMCPNameMismatch400：路径名与请求体 name 冲突 → 400，
// 不触达端口（故零写入、零操作行）。
// TestAdminResources_UpdateMCPNameMismatch400 rejects before the port is reached.
func TestAdminResources_UpdateMCPNameMismatch400(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.upsertMCPServerFn = func(context.Context, core.Actor, MCPServerWrite, string) (McpServerView, error) {
		t.Fatal("路径与请求体不一致时不得触达端口")
		return McpServerView{}, nil
	}
	before := srv.snapshotWorkspace(t)
	req := resourceCall(http.MethodPut, "/api/v1/mcp-servers/fetch",
		`{"name":"other","transport":"stdio","command":"mcp-fetch"}`)
	req.Header.Set("If-Match", srv.currentETag)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Equal(t, messageResourceNameMismatch, env.Message)
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_TestMCP：测试端点返回工具发现清单（200），路径名原样传入，
// 不写管理操作台账（操作行在适配器内预写，本层不补）。
// TestAdminResources_TestMCP projects the discovered tools and forwards the path name.
func TestAdminResources_TestMCP(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	var gotName string
	srv.fake.testMCPServerFn = func(_ context.Context, _ core.Actor, name string) (core.ResourceTestResult, error) {
		gotName = name
		return core.ResourceTestResult{OK: true, LatencyMS: 17, Tools: []string{"fs_read", "fs_write"}}, nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/mcp-servers/fetch/test", ""))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view McpServerTestResultView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.True(t, view.OK)
	require.Equal(t, int64(17), view.LatencyMS)
	require.Equal(t, []string{"fs_read", "fs_write"}, view.Tools)
	require.Equal(t, "fetch", gotName)
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_TestMCPPolicyDenied：策略拒绝是"调用确实发生过"的确定结论 →
// 200 + `ok=false` + 固定错误码，不改映射 403（与模型连接测试同一口径）。
// TestAdminResources_TestMCPPolicyDenied keeps a policy denial at 200 with ok=false.
func TestAdminResources_TestMCPPolicyDenied(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.testMCPServerFn = func(context.Context, core.Actor, string) (core.ResourceTestResult, error) {
		return core.ResourceTestResult{
			OK:           false,
			ErrorCode:    "policy_denied",
			ErrorMessage: "策略拒绝：MCP server 不在允许范围",
		}, nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/mcp-servers/fetch/test", ""))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view McpServerTestResultView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.False(t, view.OK)
	require.Equal(t, "policy_denied", view.ErrorCode)
	require.Equal(t, []string{}, view.Tools, "空工具清单必须输出 []")
	require.NotContains(t, rec.Body.String(), "${")
}

// TestMCPViews_NoEnvValues 钉住 MCP 视图的脱敏出口（约束 23）：`command` 只出可执行名，
// `env_keys` 只出变量键名，响应不含 args / url / env 值或占位原文。
// TestMCPViews_NoEnvValues pins the redacted MCP view: executable name and env key
// names only, never args, URLs, env values or placeholder text.
func TestMCPViews_NoEnvValues(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.upsertMCPServerFn = func(_ context.Context, _ core.Actor, write MCPServerWrite, _ string) (McpServerView, error) {
		keys := make([]string, 0, len(write.Env))
		for key := range write.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return McpServerView{
			Name:               write.Name,
			Transport:          write.Transport,
			Command:            write.Command,
			EnvKeys:            keys,
			Tools:              []string{},
			PublishedVersionID: 1,
			PublishedVersionNo: 1,
			PendingRestart:     true,
		}, nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/mcp-servers",
		`{"name":"fetch","transport":"stdio","command":"mcp-fetch",`+
			`"args":["--root","./workspace"],`+
			`"env":{"MCP_TOKEN":"${MCP_TOKEN}","MCP_HOME":"${MCP_HOME}"}}`))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(env.Data, &raw))
	require.NotContains(t, raw, "args", "视图不得携带 args")
	require.NotContains(t, raw, "url", "视图不得携带 url")
	require.NotContains(t, raw, "env", "视图不得携带 env 值")
	require.NotContains(t, rec.Body.String(), "${", "视图不得回显占位原文")
	require.NotContains(t, rec.Body.String(), "./workspace", "视图不得回显 args 内容")
	var view McpServerView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, "mcp-fetch", view.Command, "command 只出可执行名")
	require.Equal(t, []string{"MCP_HOME", "MCP_TOKEN"}, view.EnvKeys)
}

// skillDetailFixture 构造一个技能详情视图：两文件、强 ETag 与发布版本号一致。
// skillDetailFixture builds a two-file skill detail with a strong ETag.
func skillDetailFixture(name string) SkillDetailView {
	contentHash := strings.Repeat("c", 64)
	return SkillDetailView{
		SkillView: SkillView{
			Name:         name,
			Description:  "日报生成",
			Path:         ".basalt/skills/" + name,
			VersionID:    1,
			VersionNo:    1,
			ContentHash:  contentHash,
			ETag:         core.FormatStrongETag(contentHash),
			FileCount:    2,
			ReferencedBy: []string{},
		},
		Files: []SkillFileView{
			{Path: "SKILL.md", Size: 42, SHA256: strings.Repeat("d", 64), Content: "# 日报\n"},
			{Path: "docs/guide.md", Size: 7, SHA256: strings.Repeat("e", 64), Content: "guide\n"},
		},
	}
}

// TestAdminResources_CreateSkillProjectsView：`POST /skills` 按契约只回 `SkillView`
// ——响应里**不得**出现 `files`（文件清单只在详情端点给）；成功路径恰好一条操作行。
// TestAdminResources_CreateSkillProjectsView pins the contract's SkillView-only
// payload for POST /skills; the file list belongs to the detail endpoint.
func TestAdminResources_CreateSkillProjectsView(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.createSkillFn = func(_ context.Context, actor core.Actor, write SkillCreateWrite) (SkillDetailView, error) {
		require.Equal(t, "resource-test", actor.Subject)
		require.Equal(t, "report", write.Name)
		require.Equal(t, "日报生成", write.Description)
		srv.ledger.add("create_skill:" + write.Name)
		return skillDetailFixture(write.Name), nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/skills",
		`{"name":"report","description":"日报生成"}`))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(env.Data, &raw))
	require.NotContains(t, raw, "files", "创建响应只回 SkillView")
	var view SkillView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, "report", view.Name)
	require.Equal(t, int64(1), view.VersionID)
	require.Equal(t, 2, view.FileCount)
	require.Equal(t, []string{"create_skill:report"}, srv.operations(t))
}

// TestAdminResources_CreateSkillDuplicate409：同名技能 → 409 CONFLICT（D-04），
// handler 不落盘、不追加操作行。
// TestAdminResources_CreateSkillDuplicate409 maps a duplicate name to 409 with no
// handler-side side effects.
func TestAdminResources_CreateSkillDuplicate409(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.createSkillFn = func(context.Context, core.Actor, SkillCreateWrite) (SkillDetailView, error) {
		return SkillDetailView{}, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: "同名技能已存在：report",
		}
	}
	before := srv.snapshotWorkspace(t)
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/skills",
		`{"name":"report","description":"日报生成"}`))

	require.Equal(t, http.StatusConflict, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeConflict, env.Code)
	require.Contains(t, env.Message, "同名技能已存在")
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_ImportSkillProjectsView：导入请求的文件清单原样交给端口
// （path + content），响应同样只回 `SkillView`；成功路径一条操作行。
// TestAdminResources_ImportSkillProjectsView forwards the file list verbatim and
// projects the contract's SkillView-only payload.
func TestAdminResources_ImportSkillProjectsView(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.importSkillFn = func(_ context.Context, _ core.Actor, write SkillImportWrite) (SkillDetailView, error) {
		require.Equal(t, "logger", write.Name)
		require.Equal(t, []FileDraftWrite{
			{Path: "SKILL.md", Content: "# 日志\n"},
			{Path: "docs/guide.md", Content: "guide\n"},
		}, write.Files)
		srv.ledger.add("import_skill:" + write.Name)
		return skillDetailFixture(write.Name), nil
	}
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/skills/import",
		`{"name":"logger","files":[{"path":"SKILL.md","content":"# 日志\n"},`+
			`{"path":"docs/guide.md","content":"guide\n"}]}`))

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(env.Data, &raw))
	require.NotContains(t, raw, "files", "导入响应只回 SkillView")
	var view SkillView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, "logger", view.Name)
	require.Equal(t, []string{"import_skill:logger"}, srv.operations(t))
}

// TestAdminResources_ImportSkillRejectsBoundary：适配器报导入边界拒绝 →
// 400 IMPORT_REJECTED，文案点名被拒路径；工作区逐字节不变、零操作行。
// TestAdminResources_ImportSkillRejectsBoundary maps the import gate to 400
// IMPORT_REJECTED with zero side effects.
func TestAdminResources_ImportSkillRejectsBoundary(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.importSkillFn = func(context.Context, core.Actor, SkillImportWrite) (SkillDetailView, error) {
		return SkillDetailView{}, &core.AdminError{
			Kind:    core.ErrImportRejected,
			Message: "技能文件路径不合法：../etc/passwd",
		}
	}
	before := srv.snapshotWorkspace(t)
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/skills/import",
		`{"name":"logger","files":[{"path":"../etc/passwd","content":"x"}]}`))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeImportRejected, env.Code)
	require.Contains(t, env.Message, "../etc/passwd")
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_UpdateSkillRequiresStrongIfMatch：缺失、弱 ETag 与通配符一律
// 412，且不触达端口——与模型连接同一条件写口径。
// TestAdminResources_UpdateSkillRequiresStrongIfMatch rejects every non-literal
// strong If-Match before the port is reached.
func TestAdminResources_UpdateSkillRequiresStrongIfMatch(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	srv.fake.updateSkillFn = func(context.Context, core.Actor, string, SkillUpdateWrite, string) (SkillDetailView, error) {
		t.Fatal("条件写形状不合法时不得触达端口")
		return SkillDetailView{}, nil
	}
	before := srv.snapshotWorkspace(t)
	for _, ifMatch := range []string{"", `W/"sha256:` + strings.Repeat("a", 64) + `"`, "*"} {
		req := resourceCall(http.MethodPut, "/api/v1/skills/report",
			`{"files":[{"path":"SKILL.md","content":"# 日报\n"}]}`)
		if ifMatch != "" {
			req.Header.Set("If-Match", ifMatch)
		}
		rec := serveAdmin(srv.Server, req)
		require.Equal(t, http.StatusPreconditionFailed, rec.Code, "If-Match=%q", ifMatch)
	}
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_UpdateSkillProjectsDetail：全量替换成功回 `SkillDetail`
// （含文件正文与哈希），响应头 `ETag` 与视图一致；If-Match 原样交给端口。
// TestAdminResources_UpdateSkillProjectsDetail pins the SkillDetail payload, the
// response ETag header and the verbatim If-Match hand-off.
func TestAdminResources_UpdateSkillProjectsDetail(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	ifMatch := core.FormatStrongETag(strings.Repeat("a", 64))
	srv.fake.updateSkillFn = func(_ context.Context, _ core.Actor, name string, write SkillUpdateWrite, gotIfMatch string) (SkillDetailView, error) {
		require.Equal(t, "report", name)
		require.Equal(t, ifMatch, gotIfMatch, "If-Match 必须原样传入，不得归一化")
		require.Len(t, write.Files, 1)
		srv.ledger.add("update_skill:" + name)
		return skillDetailFixture(name), nil
	}
	req := resourceCall(http.MethodPut, "/api/v1/skills/report",
		`{"files":[{"path":"SKILL.md","content":"# 日报\n"}]}`)
	req.Header.Set("If-Match", ifMatch)
	rec := serveAdmin(srv.Server, req)

	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Equal(t, skillDetailFixture("report").ETag, rec.Header().Get("ETag"))
	var view SkillDetailView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.Equal(t, "report", view.Name)
	require.Len(t, view.Files, 2)
	require.Equal(t, "SKILL.md", view.Files[0].Path)
	require.Equal(t, "# 日报\n", view.Files[0].Content)
	require.Equal(t, []string{"update_skill:report"}, srv.operations(t))
}

// TestSkillValidate_NoWrites 是约束 26 的原样回归：校验端点不要求 If-Match、
// 不触达任何写端口、零操作行、工作区逐字节不变；`problems` 为空时仍输出 `[]`。
// TestSkillValidate_NoWrites is the verbatim constraint-26 regression: validation
// needs no If-Match and never reaches a write port.
func TestSkillValidate_NoWrites(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	calls := 0
	srv.fake.validateSkillFn = func(_ context.Context, name string, write SkillUpdateWrite) (core.SkillValidateResult, error) {
		calls++
		require.Equal(t, "report", name)
		require.Len(t, write.Files, 1)
		return core.SkillValidateResult{Valid: false, Problems: []string{"缺少 SKILL.md"}}, nil
	}
	before := srv.snapshotWorkspace(t)
	rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/skills/report/validate",
		`{"files":[{"path":"docs/guide.md","content":"guide\n"}]}`))

	require.Equal(t, http.StatusOK, rec.Code, "校验端点不得要求 If-Match，也不得触达写端口")
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var view SkillValidateResultView
	require.NoError(t, json.Unmarshal(env.Data, &view))
	require.False(t, view.Valid)
	require.Equal(t, []string{"缺少 SKILL.md"}, view.Problems)
	require.Equal(t, 1, calls, "校验端点必须且只能调用一次只读校验端口")
	require.Equal(t, "", rec.Header().Get("ETag"), "校验不是写动作，不回 ETag")
	require.True(t, srv.equalWorkspace(t, before))
	require.Empty(t, srv.operations(t))

	// 合法草稿：`problems` 必须输出 `[]` 而不是 null。
	srv.fake.validateSkillFn = func(context.Context, string, SkillUpdateWrite) (core.SkillValidateResult, error) {
		return core.SkillValidateResult{Valid: true}, nil
	}
	rec = serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/skills/report/validate",
		`{"files":[{"path":"SKILL.md","content":"# 日报\n"}]}`))
	require.Equal(t, http.StatusOK, rec.Code)
	env = decodeResourceEnvelope(t, rec)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(env.Data, &raw))
	require.JSONEq(t, `[]`, string(raw["problems"]))
}

// TestSkillValidate_SameGateAsImport 是约束 26 的共用闸门回归：同一份被拒草稿在
// 校验与导入两个端点上得到同一错误码与同一文案——页面提示可用就不可能保存被拒。
// TestSkillValidate_SameGateAsImport pins the shared gate: the same rejected draft
// yields the same code and wording on both endpoints.
func TestSkillValidate_SameGateAsImport(t *testing.T) {
	t.Parallel()
	srv := newAdminServer(t)
	rejection := func() error {
		return &core.AdminError{
			Kind:    core.ErrImportRejected,
			Message: "技能文件路径不合法：../etc/passwd",
		}
	}
	srv.fake.importSkillFn = func(context.Context, core.Actor, SkillImportWrite) (SkillDetailView, error) {
		return SkillDetailView{}, rejection()
	}
	srv.fake.validateSkillFn = func(context.Context, string, SkillUpdateWrite) (core.SkillValidateResult, error) {
		return core.SkillValidateResult{}, rejection()
	}
	body := `{"name":"logger","files":[{"path":"../etc/passwd","content":"x"}]}`
	importRec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/skills/import", body))
	validateRec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/skills/logger/validate",
		`{"files":[{"path":"../etc/passwd","content":"x"}]}`))

	require.Equal(t, http.StatusBadRequest, importRec.Code)
	require.Equal(t, http.StatusBadRequest, validateRec.Code)
	importEnv := decodeResourceEnvelope(t, importRec)
	validateEnv := decodeResourceEnvelope(t, validateRec)
	require.Equal(t, codeImportRejected, importEnv.Code)
	require.Equal(t, importEnv.Code, validateEnv.Code, "两个端点必须共用同一错误分类")
	require.Equal(t, importEnv.Message, validateEnv.Message, "两个端点必须共用同一文案")
	require.Empty(t, srv.operations(t))
}

// TestAdminResources_ConcurrentIfMatch：同一资源的并发条件写——强 If-Match 的裁决点在
// 适配器，恰好一个 200，其余 412 且带 `data.current_etag`；handler 不做乐观更新、不
// 自动重试，台账只记一次成功写。夹具的 ETag 状态由互斥保护，确保 -race 判的是生产
// 代码而不是测试脚手架（012-race 必跑）。
// TestAdminResources_ConcurrentIfMatch pins the adapter-level arbitration: one 200,
// every loser is a 412 carrying the current ETag, and one ledger row.
func TestAdminResources_ConcurrentIfMatch(t *testing.T) {
	srv := newAdminServer(t)
	originalETag := srv.currentETag

	var mu sync.Mutex
	currentETag := originalETag
	srv.fake.upsertModelConnectionFn = func(_ context.Context, _ core.Actor,
		write ModelConnectionWrite, ifMatch string,
	) (ModelConnectionView, error) {
		mu.Lock()
		defer mu.Unlock()
		if ifMatch != currentETag {
			return ModelConnectionView{}, &core.AdminError{
				Kind:    core.ErrPreconditionFailed,
				Message: "预条件失败：If-Match 与当前发布版本不一致",
				Data:    map[string]any{"current_etag": currentETag},
			}
		}
		srv.ledger.add("upsert_model_connection:" + write.Name)
		currentETag = core.FormatStrongETag(strings.Repeat("c", 64))
		return ModelConnectionView{
			Name:               write.Name,
			BaseURL:            write.BaseURL,
			APIKeyEnv:          envNameOf(write.APIKey),
			DefaultModel:       write.DefaultModel,
			PublishedVersionID: 2,
			PublishedVersionNo: 2,
			AppliedVersionID:   2,
			AppliedVersionNo:   2,
			ETag:               currentETag,
			ReferencedBy:       []string{},
		}, nil
	}

	const writers = 8
	start := make(chan struct{})
	recorders := make([]*httptest.ResponseRecorder, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			req := resourceCall(http.MethodPut, "/api/v1/model-connections/main",
				`{"base_url":"https://x.example","default_model":"m2"}`)
			req.Header.Set("If-Match", originalETag)
			recorders[index] = serveAdmin(srv.Server, req)
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	for i := 0; i < writers; i++ {
		switch recorders[i].Code {
		case http.StatusOK:
			successes++
		case http.StatusPreconditionFailed:
			env := decodeResourceEnvelope(t, recorders[i])
			require.Equal(t, codePreconditionFailed, env.Code)
			var data struct {
				CurrentETag string `json:"current_etag"`
			}
			require.NoError(t, json.Unmarshal(env.Data, &data))
			require.NotEmpty(t, data.CurrentETag)
			require.NotEqual(t, originalETag, data.CurrentETag,
				"412 必须回传写后的当前 ETag")
		default:
			require.FailNow(t, "并发条件写出现意外状态码",
				"writer %d => %d", i, recorders[i].Code)
		}
	}
	require.Equal(t, 1, successes, "同一 If-Match 只能有一个写成功")
	require.Len(t, srv.operations(t), 1, "台账只记一次成功写")
}

// TestModelConnection_WriteOnlyFields：`api_key` 是 write-only 字段——省略 = nil 指针
// （保留），出现（含空串）= 非 nil（空串由适配器按 400 拒绝，两层不得合并）；`credential_ref`
// 是服务端字段，请求体出现即 400 `CREDENTIAL_REF_READ_ONLY` 且**不**触达端口；
// `credential_mode` / `scope` / `owner_agent` 原样进端口。
// TestModelConnection_WriteOnlyFields pins the write-only api_key shape, the read-only
// credential_ref rejection, and the verbatim forwarding of mode and scope.
func TestModelConnection_WriteOnlyFields(t *testing.T) {
	t.Parallel()

	t.Run("credential_ref 只读", func(t *testing.T) {
		t.Parallel()
		srv := newAdminServer(t)
		srv.fake.upsertModelConnectionFn = func(context.Context, core.Actor, ModelConnectionWrite,
			string,
		) (ModelConnectionView, error) {
			t.Error("credential_ref 必须在调端口之前被拒绝")
			return ModelConnectionView{}, nil
		}
		before := srv.snapshotWorkspace(t)

		rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/model-connections",
			`{"name":"backup","base_url":"https://b.example","default_model":"m","credential_ref":"cred_deadbeef"}`))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, codeCredentialRefReadOnly, decodeResourceEnvelope(t, rec).Code)

		req := resourceCall(http.MethodPut, "/api/v1/model-connections/main",
			`{"base_url":"https://b.example","default_model":"m","credential_ref":"cred_deadbeef"}`)
		req.Header.Set("If-Match", srv.currentETag)
		rec = serveAdmin(srv.Server, req)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, codeCredentialRefReadOnly, decodeResourceEnvelope(t, rec).Code)
		require.NotContains(t, rec.Body.String(), "cred_deadbeef", "不得回显客户端自造的凭据 ID")

		require.True(t, srv.equalWorkspace(t, before))
		require.Empty(t, srv.operations(t))
	})

	t.Run("api_key 省略是保留", func(t *testing.T) {
		t.Parallel()
		srv := newAdminServer(t)
		var captured ModelConnectionWrite
		srv.fake.upsertModelConnectionFn = func(_ context.Context, _ core.Actor,
			write ModelConnectionWrite, _ string,
		) (ModelConnectionView, error) {
			captured = write
			return ModelConnectionView{Name: write.Name, BaseURL: write.BaseURL, DefaultModel: write.DefaultModel}, nil
		}

		req := resourceCall(http.MethodPut, "/api/v1/model-connections/main",
			`{"base_url":"https://x.example","default_model":"m2"}`)
		req.Header.Set("If-Match", srv.currentETag)
		require.Equal(t, http.StatusOK, serveAdmin(srv.Server, req).Code)
		require.Nil(t, captured.APIKey, "省略 api_key 必须解码成 nil（保留），不得退化成空串")
	})

	t.Run("api_key 显式空串进端口", func(t *testing.T) {
		t.Parallel()
		srv := newAdminServer(t)
		var captured ModelConnectionWrite
		srv.fake.upsertModelConnectionFn = func(_ context.Context, _ core.Actor,
			write ModelConnectionWrite, _ string,
		) (ModelConnectionView, error) {
			captured = write
			return ModelConnectionView{Name: write.Name, BaseURL: write.BaseURL, DefaultModel: write.DefaultModel}, nil
		}

		req := resourceCall(http.MethodPut, "/api/v1/model-connections/main",
			`{"base_url":"https://x.example","default_model":"m2","api_key":""}`)
		req.Header.Set("If-Match", srv.currentETag)
		require.Equal(t, http.StatusOK, serveAdmin(srv.Server, req).Code)
		require.NotNil(t, captured.APIKey, "出现但为空的 api_key 必须可区分：空串是 400，保留才是省略")
		require.Empty(t, *captured.APIKey)
	})

	t.Run("模式与范围原样进端口", func(t *testing.T) {
		t.Parallel()
		srv := newAdminServer(t)
		var captured ModelConnectionWrite
		srv.fake.upsertModelConnectionFn = func(_ context.Context, _ core.Actor,
			write ModelConnectionWrite, _ string,
		) (ModelConnectionView, error) {
			captured = write
			return ModelConnectionView{Name: write.Name, BaseURL: write.BaseURL, DefaultModel: write.DefaultModel}, nil
		}

		rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/model-connections",
			`{"name":"backup","base_url":"https://b.example","default_model":"m","credential_mode":"managed",`+
				`"api_key":"sk-live-value","scope":"exclusive","owner_agent":"ops-agent"}`))
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "managed", captured.CredentialMode)
		require.NotNil(t, captured.APIKey)
		require.Equal(t, "sk-live-value", *captured.APIKey)
		require.Equal(t, "exclusive", captured.Scope)
		require.Equal(t, "ops-agent", captured.OwnerAgent)
		require.NotContains(t, rec.Body.String(), "sk-live-value", "write-only 字段不得回显")
	})
}

// TestModelConnection_CredentialErrorCodes：凭据与使用范围的 sentinel 经
// `writeResourceAdminError` 逐项映射到契约码与状态（保存期 400 / 409 / 503），不是
// 一律 400，也不是 500；文案由适配器给出，不回显底层值。
// TestModelConnection_CredentialErrorCodes pins the per-sentinel status and code.
func TestModelConnection_CredentialErrorCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"CREDENTIAL_MODE_REQUIRED", core.ErrCredentialModeRequired, http.StatusBadRequest, codeCredentialModeRequired},
		{"CREDENTIAL_REVOKED", core.ErrCredentialRevoked, http.StatusBadRequest, codeCredentialRevoked},
		{"RESOURCE_SCOPE_IN_USE", core.ErrResourceScopeInUse, http.StatusConflict, codeResourceScopeInUse},
		{"CREDENTIAL_OWNER_MISMATCH", core.ErrCredentialOwnerMismatch, http.StatusConflict, codeCredentialOwnerMismatch},
		{"CREDENTIAL_UNAVAILABLE", core.ErrCredentialUnavailable, http.StatusServiceUnavailable, codeCredentialUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newAdminServer(t)
			srv.fake.upsertModelConnectionFn = func(context.Context, core.Actor, ModelConnectionWrite,
				string,
			) (ModelConnectionView, error) {
				return ModelConnectionView{}, &core.AdminError{Kind: tc.err, Message: "适配器文案：" + tc.name}
			}

			rec := serveAdmin(srv.Server, resourceCall(http.MethodPost, "/api/v1/model-connections",
				`{"name":"backup","base_url":"https://b.example","api_key":"${BASALT_BACKUP_KEY}","default_model":"m"}`))
			require.Equal(t, tc.status, rec.Code)
			env := decodeResourceEnvelope(t, rec)
			require.Equal(t, tc.code, env.Code)
			require.Contains(t, env.Message, tc.name, "文案由适配器给出，本层不另造")
		})
	}
}

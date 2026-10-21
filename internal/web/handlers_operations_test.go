package web

// 本文件是管理操作审计只读面（`GET /operations`、`GET /operations/{id}`）的验收
// （模块卡 §3.2：handlers_operations_test.go，过滤 / 固定排序 / 脱敏 detail /
// unknown 提示 / 404）。假仓储只记录过滤条件并原样返回预置行——web 层若重排、
// 整段吐出 detail_json 或把 unknown 读成成功，这里的断言就会红。
// Acceptance for the operation-audit read face: filters, fixed ordering, redacted
// detail, the unknown hint, and 404 mapping.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// operationFake 是 OperationStore 的记录型替身：读方法记录入参并返回预置结果；
// 写方法只为满足端口方法集（只读端点**不得**调用它们）。
// operationFake records list/detail calls; mutating methods exist only for the port.
type operationFake struct {
	mu sync.Mutex

	filters   []core.OperationFilter
	listCalls int
	listed    []core.OperationRecord
	listErr   error
	detail    core.OperationRecord
	getErr    error
	requested []string
}

// var _ core.OperationStore = (*operationFake)(nil) 让替身与端口方法集在编译期同步。
var _ core.OperationStore = (*operationFake)(nil)

func (f *operationFake) BeginOperation(context.Context, core.OperationRecord) error { return nil }

func (f *operationFake) FinishOperation(context.Context, string, core.OperationStatus, string, string, []byte, time.Time) error {
	return nil
}

func (f *operationFake) ListOperations(_ context.Context, filter core.OperationFilter) ([]core.OperationRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.filters = append(f.filters, filter)
	return f.listed, f.listErr
}

func (f *operationFake) GetOperation(_ context.Context, operationID string) (core.OperationRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requested = append(f.requested, operationID)
	return f.detail, f.getErr
}

func (f *operationFake) PendingOperations(context.Context) ([]core.OperationRecord, error) {
	return nil, nil
}

func (f *operationFake) RecoverOperation(context.Context, string, core.OperationStatus, string, []byte, time.Time) error {
	return nil
}

// lastFilter / listCallCount 供「参数原样传入」「非法参数零查询」断言。
func (f *operationFake) lastFilter(t *testing.T) core.OperationFilter {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.filters, "handler 必须调用 ListOperations")
	return f.filters[len(f.filters)-1]
}

func (f *operationFake) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// requestedIDs 供详情路径的入参断言。
func (f *operationFake) requestedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requested)
}

// newOperationServer 装配一个只替身操作仓储的管理面服务器。
func newOperationServer(t *testing.T, fake *operationFake) *Server {
	t.Helper()
	return newTestServer(t, func(o *Options) { o.Operations = fake })
}

// TestOperations_ListDefaultsAndFilters 覆盖缺省窗口、五个过滤原样传入与未知参数
// 忽略（契约 /operations 的五个参数；只有 status / target_kind / limit 是闭集）。
// 逆改点：缺省 limit 写错 / 过滤被改写 / 未知参数被当成过滤 → 对应断言红。
func TestOperations_ListDefaultsAndFilters(t *testing.T) {
	t.Run("缺省窗口 50 且无过滤", func(t *testing.T) {
		fake := &operationFake{listed: []core.OperationRecord{}}
		srv := newOperationServer(t, fake)

		rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations", ""))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		filter := fake.lastFilter(t)
		require.Equal(t, core.OperationFilter{Limit: defaultOperationLimit}, filter,
			"缺省只给固定窗口，不得预填任何过滤值")
	})

	t.Run("五个过滤原样传入", func(t *testing.T) {
		fake := &operationFake{listed: []core.OperationRecord{}}
		srv := newOperationServer(t, fake)

		rec := serveAdmin(srv, adminRequest(http.MethodGet,
			"/api/v1/operations?action=agent.disable&target_kind=agent&target_id=alpha&status=unknown&limit=7", ""))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, core.OperationFilter{
			Action:     "agent.disable",
			TargetKind: "agent",
			TargetID:   "alpha",
			Status:     "unknown",
			Limit:      7,
		}, fake.lastFilter(t), "五个过滤必须原样交给仓储，web 层不改写")
	})

	t.Run("未知参数被忽略", func(t *testing.T) {
		fake := &operationFake{listed: []core.OperationRecord{}}
		srv := newOperationServer(t, fake)

		rec := serveAdmin(srv, adminRequest(http.MethodGet,
			"/api/v1/operations?sort=asc&order=created&page=2&offset=5", ""))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, core.OperationFilter{Limit: defaultOperationLimit}, fake.lastFilter(t),
			"排序 / 分页不是参数，不得进入过滤条件")
	})
}

// TestOperations_ListRejectsInvalidParameters 覆盖 status / target_kind / limit 的
// 非法取值：一律 400 BAD_REQUEST，且在触达仓储前返回（零查询）。
// 逆改点：非法值被静默忽略 → 200；校验晚于查询 → listCallCount 非零。
func TestOperations_ListRejectsInvalidParameters(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantMsg string
	}{
		{"非法 status", "?status=done", "status"},
		{"非法 target_kind", "?target_kind=job", "target_kind"},
		{"limit 为零", "?limit=0", "limit"},
		{"limit 超上限", "?limit=201", "limit"},
		{"limit 非整数", "?limit=abc", "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &operationFake{}
			srv := newOperationServer(t, fake)

			rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations"+tc.query, ""))

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			env := decodeRunEnvelope[any](t, rec)
			require.Equal(t, codeBadRequest, env.Code)
			require.Contains(t, env.Message, tc.wantMsg)
			require.Zero(t, fake.listCallCount(), "非法参数必须在触达仓储前被拒（零查询）")
		})
	}
}

// TestOperations_ListProjectsRows 覆盖固定排序与字段投影：仓储给什么顺序就出什么
// 顺序（排序在 SQL），时间走 RFC3339 UTC、零值渲染空串。
// 逆改点：handler 二次排序 / 时间格式漂移 / finished_at 零值渲染成 0001 年 → 红。
func TestOperations_ListProjectsRows(t *testing.T) {
	created := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	finished := time.Date(2026, 10, 21, 10, 0, 3, 0, time.UTC)
	newer := core.OperationRecord{
		OperationID:  "op-new",
		Action:       "agent.disable",
		TargetKind:   "agent",
		TargetID:     "alpha",
		ActorSubject: "token:0123456789abcdef",
		AuthMethod:   core.AuthMethodBearer,
		Source:       "127.0.0.1:5555",
		Status:       core.OperationUnknown,
		CreatedAt:    created,
	}
	older := core.OperationRecord{
		OperationID:  "op-old",
		Action:       "system.baseline_v2",
		TargetKind:   "system",
		TargetID:     "baseline_v2",
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       "migration",
		Status:       core.OperationSuccess,
		ResultCode:   "OK",
		CreatedAt:    created.Add(-time.Minute),
		FinishedAt:   finished,
	}
	fake := &operationFake{listed: []core.OperationRecord{newer, older}}
	srv := newOperationServer(t, fake)

	rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations", ""))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	env := decodeRunEnvelope[[]OperationView](t, rec)
	require.Len(t, env.Data, 2)
	require.Equal(t, "op-new", env.Data[0].OperationID, "列表必须保持仓储的固定排序")
	require.Equal(t, "op-old", env.Data[1].OperationID)
	require.Equal(t, "2026-10-21T10:00:00Z", env.Data[0].CreatedAt)
	require.Empty(t, env.Data[0].FinishedAt, "未结束的时间零值必须渲染空串")
	require.Equal(t, "2026-10-21T10:00:03Z", env.Data[1].FinishedAt)
	require.Equal(t, "unknown", env.Data[0].Status)
	require.Equal(t, core.ActorSubjectMigration, env.Data[1].ActorSubject)
}

// TestOperations_ListEmptyIsArray 覆盖空列表的信封形态：`data` 必须是 `[]`，不是
// `null`（约束 34：数组字段空时输出空数组）。
// 逆改点：make 切片改成 nil 切片 → `"data":null` → 红。
func TestOperations_ListEmptyIsArray(t *testing.T) {
	srv := newOperationServer(t, &operationFake{listed: []core.OperationRecord{}})

	rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations", ""))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"data":[]`)
}

// TestOperations_ListStoreError 覆盖仓储错误：500 INTERNAL 固定文案，不泄露内部
// 路径或错误原文（约束 10 / 24）。
// 逆改点：把 err.Error() 写进响应 → 断言命中隐藏路径 → 红。
func TestOperations_ListStoreError(t *testing.T) {
	fake := &operationFake{listErr: errors.New("database is locked: C:\\secret\\basalt.db")}
	srv := newOperationServer(t, fake)

	rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations", ""))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeRunEnvelope[any](t, rec)
	require.Equal(t, codeInternal, env.Code)
	require.Equal(t, messageInternal, env.Message)
	require.NotContains(t, rec.Body.String(), "secret")
	require.NotContains(t, rec.Body.String(), "database is locked")
}

// TestOperations_DetailRedacted 是 §6.3 第 11 项点名用例：detail_json 出站前只保留
// 白名单键，凭证、请求原文与绝对路径一律不投影。
// 逆改点：整段 detail 出站 / 绝对路径放行 → 敏感串出现在响应体 → 红。
func TestOperations_DetailRedacted(t *testing.T) {
	const (
		candidate = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		previous  = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	)
	detailJSON := []byte(`{
		"providers": 2,
		"mcp": 1,
		"skills": 0,
		"agents": 3,
		"candidate_sha256": "` + candidate + `",
		"previous_sha256": "` + previous + `",
		"expected_state": "disabled",
		"expected_generation": 7,
		"version_no": 4,
		"path": "agents/alpha/AGENT.md",
		"files": ["agents/alpha/AGENT.md", "agents/alpha/memory/core.md"],
		"token": "sk-secret-token",
		"api_key": "sk-live-123",
		"content": "用户原始请求正文",
		"absolute_path": "C:\\secret\\state.json"
	}`)

	t.Run("白名单键保留、敏感键丢弃", func(t *testing.T) {
		fake := &operationFake{detail: core.OperationRecord{
			OperationID: "op-1",
			Action:      "agent.content.update",
			Status:      core.OperationSuccess,
			DetailJSON:  detailJSON,
		}}
		srv := newOperationServer(t, fake)

		rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations/op-1", ""))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		env := decodeRunEnvelope[OperationDetailView](t, rec)
		detail := env.Data.Detail

		require.Equal(t, float64(2), detail["providers"])
		require.Equal(t, float64(1), detail["mcp"])
		require.Equal(t, float64(0), detail["skills"])
		require.Equal(t, float64(3), detail["agents"])
		require.Equal(t, candidate, detail["candidate_sha256"])
		require.Equal(t, previous, detail["previous_sha256"])
		require.Equal(t, "disabled", detail["expected_state"])
		require.Equal(t, float64(7), detail["expected_generation"])
		require.Equal(t, float64(4), detail["version_no"])
		require.Equal(t, "agents/alpha/AGENT.md", detail["path"])
		require.Equal(t, []any{"agents/alpha/AGENT.md", "agents/alpha/memory/core.md"}, detail["files"])

		for _, key := range []string{"token", "api_key", "content", "absolute_path"} {
			require.NotContains(t, detail, key, "未列入白名单的键必须被丢弃")
		}
		body := rec.Body.String()
		for _, leaked := range []string{"sk-secret-token", "sk-live-123", "用户原始请求正文", "state.json"} {
			require.NotContains(t, body, leaked, "敏感内容不得出现在响应体")
		}
	})

	t.Run("白名单键里的绝对路径同样丢弃", func(t *testing.T) {
		fake := &operationFake{detail: core.OperationRecord{
			OperationID: "op-2",
			Action:      "agent.content.update",
			Status:      core.OperationSuccess,
			DetailJSON:  []byte(`{"path":"C:\\secret\\x.md","files":["ok/a.md","/abs/b.md"]}`),
		}}
		srv := newOperationServer(t, fake)

		rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations/op-2", ""))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		env := decodeRunEnvelope[OperationDetailView](t, rec)
		require.Empty(t, env.Data.Detail, "绝对路径即使挂在白名单键上也不得投影")
		require.NotContains(t, rec.Body.String(), "secret")
		require.NotContains(t, rec.Body.String(), "/abs/b.md")
	})
}

// TestOperations_DetailUnknownHint 覆盖 unknown 提示：详情仍返回 200，但信封
// message 说明结果未查证、不得盲目重做；verification 的 NULL 投影为 not_required。
// 逆改点：unknown 详情不给提示 / verification 输出空串 → 红。
func TestOperations_DetailUnknownHint(t *testing.T) {
	fake := &operationFake{detail: core.OperationRecord{
		OperationID: "op-unknown",
		Action:      "agent.content.update",
		Status:      core.OperationUnknown,
		DetailJSON:  []byte(`{}`),
	}}
	srv := newOperationServer(t, fake)

	rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations/op-unknown", ""))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	env := decodeRunEnvelope[OperationDetailView](t, rec)
	require.Equal(t, messageOperationUnknown, env.Message,
		"unknown 详情必须在信封 message 里提示未查证、不得盲目重做")
	require.Equal(t, "unknown", env.Data.Status)
	require.Equal(t, operationVerificationNotRequired, env.Data.Verification,
		"未恢复行的 verification 列为 NULL，投影为 not_required")
	require.Empty(t, env.Data.RecoveredAt)
}

// TestOperations_DetailFields 覆盖详情字段投影：请求摘要、目标版本、ETag、恢复时间
// 与 verification 原样出站，普通成功详情的 message 仍是 success。
// 逆改点：字段漏投影 / verification 被改写 → 红。
func TestOperations_DetailFields(t *testing.T) {
	recovered := time.Date(2026, 10, 21, 11, 30, 0, 0, time.UTC)
	fake := &operationFake{detail: core.OperationRecord{
		OperationID:        "op-recovered",
		Action:             "agent.enable",
		TargetKind:         "agent",
		TargetID:           "alpha",
		ActorSubject:       "local_trust",
		AuthMethod:         core.AuthMethodLocalTrust,
		Source:             "127.0.0.1:5555",
		RequestHash:        "request-hash",
		RequestedVersionID: 12,
		ExpectedETag:       `"sha256:` + strings.Repeat("a", 64) + `"`,
		Status:             core.OperationSuccess,
		ResultCode:         "OK",
		DetailJSON:         []byte(`{"expected_state":"active","expected_generation":7}`),
		CreatedAt:          recovered.Add(-time.Minute),
		FinishedAt:         recovered,
		RecoveredAt:        recovered,
		Verification:       core.VerificationApplied,
	}}
	srv := newOperationServer(t, fake)

	rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations/op-recovered", ""))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	env := decodeRunEnvelope[OperationDetailView](t, rec)
	require.Equal(t, messageSuccess, env.Message)
	require.Equal(t, "op-recovered", env.Data.OperationID)
	require.Equal(t, "request-hash", env.Data.RequestHash)
	require.Equal(t, int64(12), env.Data.RequestedVersionID)
	require.Equal(t, `"sha256:`+strings.Repeat("a", 64)+`"`, env.Data.ExpectedETag)
	require.Equal(t, "2026-10-21T11:30:00Z", env.Data.RecoveredAt)
	require.Equal(t, core.VerificationApplied, env.Data.Verification)
	require.Equal(t, "active", env.Data.Detail["expected_state"])
	require.Equal(t, float64(7), env.Data.Detail["expected_generation"])
}

// TestOperations_DetailNotFound 覆盖未知标识：404 NOT_FOUND，回显操作标识，且标识
// 确实交给了仓储（不是 handler 自己判空）。
// 逆改点：未映射 ErrOperationNotFound → 500；错误标识没到达仓储 → 红。
func TestOperations_DetailNotFound(t *testing.T) {
	fake := &operationFake{getErr: fmt.Errorf("get op: %w", core.ErrOperationNotFound)}
	srv := newOperationServer(t, fake)

	rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations/op-missing", ""))

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	env := decodeRunEnvelope[any](t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Contains(t, env.Message, "op-missing")
	require.Equal(t, []string{"op-missing"}, fake.requestedIDs())
}

// TestOperations_DetailStoreError 覆盖详情仓储错误：500 INTERNAL 固定文案，不泄露
// 内部细节。
// 逆改点：错误原文出站 → 断言命中 → 红。
func TestOperations_DetailStoreError(t *testing.T) {
	fake := &operationFake{getErr: errors.New("sql: SELECT failed on C:\\secret\\basalt.db")}
	srv := newOperationServer(t, fake)

	rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations/op-x", ""))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeRunEnvelope[any](t, rec)
	require.Equal(t, codeInternal, env.Code)
	require.Equal(t, messageInternal, env.Message)
	require.NotContains(t, rec.Body.String(), "secret")
	require.NotContains(t, rec.Body.String(), "SELECT failed")
}

// TestOperations_MissingPort 覆盖装配缺失：Operations 为 nil 时两个端点都按 500
// 处理（装配错误不是 404，也不是空列表 200）。
// 逆改点：nil 端口被当成"无数据" → 200 空列表 → 红。
func TestOperations_MissingPort(t *testing.T) {
	srv := newTestServer(t, nil)

	list := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations", ""))
	require.Equal(t, http.StatusInternalServerError, list.Code)
	require.Equal(t, messageInternal, decodeRunEnvelope[any](t, list).Message)

	detail := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations/op-1", ""))
	require.Equal(t, http.StatusInternalServerError, detail.Code)
	require.Equal(t, messageInternal, decodeRunEnvelope[any](t, detail).Message)
}

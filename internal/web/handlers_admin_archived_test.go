package web

// 本文件是 `handlers_admin_archived.go` 的 handler 级验收（颗粒度文档 §3.2.1，
// 计划 10 例）：归档端点的成功投影与 404 / 409（目标冲突、运行忙）、恢复端点的
// 原名 / 异名两条路径、归档记录缺失的 404 点名、可选请求体的严格解码，以及三个
// 端点缺端口时的固定 500。响应形状与错误码以契约与约束 43 为锚，**不**比对实现
// 文本；等待超时的回滚矩阵由适配器任务（T033）断言，本层只判 409 与
// `data.run_ids` / `data.operation_id`。
// This file is the handler-level acceptance for the three archive endpoints. The
// rollback matrix is asserted at the adapter layer; here the judged surface is the
// response projection and the error mapping.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeArchivedAgentService 在 Agent 写面替身之上覆盖归档三方法；未注入的方法按
// "意外调用"报错（与 `fakeAgentAdminService` 同一口径）。
// fakeArchivedAgentService overrides the three archive methods on top of the agent
// write-surface fake; an uninjected method reports an unexpected call.
type fakeArchivedAgentService struct {
	*fakeAgentAdminService
	archiveFn  func(ctx context.Context, actor core.Actor, name string) (core.AgentArchiveRecord, error)
	restoreFn  func(ctx context.Context, actor core.Actor, archiveID, target string) (AgentFileSet, error)
	archivedFn func(ctx context.Context) ([]core.AgentArchiveRecord, error)
}

// Archive 委托归档的注入行为。
func (f *fakeArchivedAgentService) Archive(ctx context.Context, actor core.Actor, name string) (core.AgentArchiveRecord, error) {
	if f.archiveFn == nil {
		return core.AgentArchiveRecord{}, errFakeAgentAdminUnused
	}
	return f.archiveFn(ctx, actor, name)
}

// Restore 委托恢复的注入行为。
func (f *fakeArchivedAgentService) Restore(ctx context.Context, actor core.Actor, archiveID, target string) (AgentFileSet, error) {
	if f.restoreFn == nil {
		return AgentFileSet{}, errFakeAgentAdminUnused
	}
	return f.restoreFn(ctx, actor, archiveID, target)
}

// Archived 委托归档列表的注入行为。
func (f *fakeArchivedAgentService) Archived(ctx context.Context) ([]core.AgentArchiveRecord, error) {
	if f.archivedFn == nil {
		return nil, errFakeAgentAdminUnused
	}
	return f.archivedFn(ctx)
}

// archivedRecord 构造一条归档记录；恢复状态由用例另行设置。
// archivedRecord builds one archive row; restore state is set by the case.
func archivedRecord(archiveID, agent string, archivedAt int64) core.AgentArchiveRecord {
	return core.AgentArchiveRecord{
		ArchiveID:   archiveID,
		AgentName:   agent,
		ArchivedAt:  archivedAt,
		ContentHash: "sha256:" + agent + "-hash",
	}
}

// decodeArchivedViews 解码成功信封里的归档列表（拒绝 `null`）。
// decodeArchivedViews decodes the archive list and rejects a null array.
func decodeArchivedViews(t *testing.T, rec *httptest.ResponseRecorder) (Envelope, []AgentArchiveView) {
	t.Helper()
	env := decodeAuthEnvelope(t, rec)
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var views []AgentArchiveView
	require.NoError(t, json.Unmarshal(raw, &views))
	require.NotNil(t, views, "列表端点的 data 必须是数组，不得为 null")
	return env, views
}

// TestArchivedAgents_List 覆盖归档列表端点（模块卡 §4.4 的 012-web-admin-agents
// 锚点）：适配器顺序原样投影、时间字段是 RFC3339 UTC、未恢复行给空串、空列表输出
// `[]`、端口失败回固定 500 且不回显底层原因。
// 逆改点：本地重排 / 把空列表序列化成 null / 500 回显适配器文本 → 对应断言红。
func TestArchivedAgents_List(t *testing.T) {
	archivedAt := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC).UnixMilli()
	restoredAt := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC).UnixMilli()

	t.Run("projects rows in adapter order", func(t *testing.T) {
		restored := archivedRecord("reporter-20261021-080000", "reporter", archivedAt-time.Hour.Milliseconds())
		restored.RestoredAt = &restoredAt
		restored.RestoredAgentName = "reporter"
		srv := newAgentAdminServer(t, &fakeArchivedAgentService{
			archivedFn: func(context.Context) ([]core.AgentArchiveRecord, error) {
				return []core.AgentArchiveRecord{
					archivedRecord("reporter-20261022-093000", "reporter", archivedAt),
					restored,
				}, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodGet, "/api/v1/archived-agents", ""))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		env, views := decodeArchivedViews(t, rec)
		require.Equal(t, codeOK, env.Code)
		require.Len(t, views, 2)
		require.Equal(t, AgentArchiveView{
			ArchiveID:   "reporter-20261022-093000",
			Agent:       "reporter",
			ArchivedAt:  "2026-10-22T09:30:00Z",
			ContentHash: "sha256:reporter-hash",
		}, views[0], "未恢复行的 restored_at / restored_agent_name 为空串")
		require.Equal(t, "2026-10-22T08:30:00Z", views[1].ArchivedAt)
		require.Equal(t, "2026-10-22T10:00:00Z", views[1].RestoredAt)
		require.Equal(t, "reporter", views[1].RestoredAgentName)
	})

	t.Run("empty list renders as []", func(t *testing.T) {
		srv := newAgentAdminServer(t, &fakeArchivedAgentService{
			archivedFn: func(context.Context) ([]core.AgentArchiveRecord, error) {
				return nil, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodGet, "/api/v1/archived-agents", ""))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		_, views := decodeArchivedViews(t, rec)
		require.Empty(t, views)
		require.NotNil(t, views)
	})

	t.Run("port failure is a fixed 500 without the cause", func(t *testing.T) {
		srv := newAgentAdminServer(t, &fakeArchivedAgentService{
			archivedFn: func(context.Context) ([]core.AgentArchiveRecord, error) {
				return nil, fmt.Errorf("查询归档索引失败：/home/secret/basalt.db")
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodGet, "/api/v1/archived-agents", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
		require.NotContains(t, rec.Body.String(), "/home/secret")
	})
}

// TestArchivedAgents_ArchiveProjectsRecord 断言归档成功回 200 与归档记录投影，且
// 认证闸门把显式 actor 交给端口。
// Archive success answers 200 with the projected archive row.
func TestArchivedAgents_ArchiveProjectsRecord(t *testing.T) {
	archivedAt := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC).UnixMilli()
	var gotActor core.Actor
	var gotName string
	srv := newAgentAdminServer(t, &fakeArchivedAgentService{
		archiveFn: func(_ context.Context, actor core.Actor, name string) (core.AgentArchiveRecord, error) {
			gotActor, gotName = actor, name
			return archivedRecord("reporter-20261022-093000", "reporter", archivedAt), nil
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodDelete, "/api/v1/agents/reporter", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, "reporter", gotName)
	require.Equal(t, core.AuthMethodBearer, gotActor.Method)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var view AgentArchiveView
	require.NoError(t, json.Unmarshal(raw, &view))
	require.Equal(t, AgentArchiveView{
		ArchiveID:   "reporter-20261022-093000",
		Agent:       "reporter",
		ArchivedAt:  "2026-10-22T09:30:00Z",
		ContentHash: "sha256:reporter-hash",
	}, view)
}

// TestArchivedAgents_ArchiveBusy 断言在途运行等待超时映射 409 AGENT_BUSY，并在
// `data.run_ids` 点名运行标识（契约点名的活动运行），`pending` 之后失败同时带
// `data.operation_id`（约束 1a），自由文本不得带标识。
// A drain timeout maps to 409 AGENT_BUSY with data.run_ids and the operation id.
func TestArchivedAgents_ArchiveBusy(t *testing.T) {
	srv := newAgentAdminServer(t, &fakeArchivedAgentService{
		archiveFn: func(context.Context, core.Actor, string) (core.AgentArchiveRecord, error) {
			return core.AgentArchiveRecord{}, &core.AgentBusyError{
				RunIDs: []string{"run-1", "run-2"}, OperationID: "op-archive-busy",
			}
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodDelete, "/api/v1/agents/reporter", ""))
	require.Equal(t, http.StatusConflict, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeAgentBusy, env.Code)
	require.Equal(t, "Agent 仍有在途 Run，等待上限内未退出", env.Message)
	data, ok := env.Data.(map[string]any)
	require.True(t, ok, "data 必须携带 run_ids")
	require.Equal(t, []any{"run-1", "run-2"}, data["run_ids"])
	require.Equal(t, "op-archive-busy", data["operation_id"])
	require.NotContains(t, env.Message, "run-1")
}

// TestArchivedAgents_PostPendingFailureCarriesOperationID 断言未分类的兜底失败
// （500 INTERNAL）仍带 `data.operation_id`——`pending` 已写入的任何失败都必须可经
// `GET /operations/{id}` 追查（约束 1a），固定文案不因携带 data 而改变。
// An unclassified post-pending failure keeps the fixed 500 while still carrying the
// queryable operation id.
func TestArchivedAgents_PostPendingFailureCarriesOperationID(t *testing.T) {
	srv := newAgentAdminServer(t, &fakeArchivedAgentService{
		archiveFn: func(context.Context, core.Actor, string) (core.AgentArchiveRecord, error) {
			return core.AgentArchiveRecord{}, &core.AdminError{
				Kind: errors.New("归档目录不可写"),
				Data: map[string]any{"operation_id": "op-archive-500"},
			}
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodDelete, "/api/v1/agents/reporter", ""))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeInternal, env.Code)
	data, ok := env.Data.(map[string]any)
	require.True(t, ok, "data 必须携带 operation_id")
	require.Equal(t, "op-archive-500", data["operation_id"])
}

// TestArchivedAgents_ArchiveConflict 断言归档目标已存在等冲突映射 409 CONFLICT，
// 文案由适配器点名且失败信封不带 data。
// Target conflicts map to 409 CONFLICT with the adapter wording and no data.
func TestArchivedAgents_ArchiveConflict(t *testing.T) {
	srv := newAgentAdminServer(t, &fakeArchivedAgentService{
		archiveFn: func(context.Context, core.Actor, string) (core.AgentArchiveRecord, error) {
			return core.AgentArchiveRecord{}, &core.AdminError{
				Kind:    core.ErrConflict,
				Message: "归档目标已存在：reporter-20261022-093000",
			}
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodDelete, "/api/v1/agents/reporter", ""))
	require.Equal(t, http.StatusConflict, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeConflict, env.Code)
	require.Equal(t, "归档目标已存在：reporter-20261022-093000", env.Message)
	require.Nil(t, env.Data)
}

// TestArchivedAgents_ArchiveNotFound 断言待归档 Agent 不存在映射 404 并点名。
// A missing agent maps to a 404 that names it.
func TestArchivedAgents_ArchiveNotFound(t *testing.T) {
	srv := newAgentAdminServer(t, &fakeArchivedAgentService{
		archiveFn: func(context.Context, core.Actor, string) (core.AgentArchiveRecord, error) {
			return core.AgentArchiveRecord{}, &core.AgentNotFoundError{Name: "ghost"}
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodDelete, "/api/v1/agents/ghost", ""))
	require.Equal(t, http.StatusNotFound, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Equal(t, "未找到 Agent：ghost", env.Message)
}

// TestArchivedAgents_RestoreOriginalName 断言缺省请求体等价于原名恢复（端口收到空
// 目标名），响应是恢复后的 Agent 文件视图（默认停用）。
// An absent body means the original name; the response is the restored file set.
func TestArchivedAgents_RestoreOriginalName(t *testing.T) {
	etag := agentSetETag("b")
	var gotArchiveID, gotTarget string
	srv := newAgentAdminServer(t, &fakeArchivedAgentService{
		restoreFn: func(_ context.Context, _ core.Actor, archiveID, target string) (AgentFileSet, error) {
			gotArchiveID, gotTarget = archiveID, target
			return agentFileSet("reporter", etag, "AGENT.md"), nil
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodPost,
		"/api/v1/archived-agents/reporter-20261022-093000/restore", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, "reporter-20261022-093000", gotArchiveID)
	require.Empty(t, gotTarget, "缺省请求体必须原样交给端口为空名")

	env, view := decodeAdminAgentFilesView(t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Equal(t, "reporter", view.Agent)
	require.Equal(t, etag, view.ETag)
	require.NotNil(t, view.Lifecycle.State)
	require.Equal(t, string(core.AgentDisabled), *view.Lifecycle.State, "原名恢复保留停用")
}

// TestArchivedAgents_RestoreRenamed 断言显式 `new_name` 原样交给端口，响应投影
// 的是异名恢复后的文件视图。
// An explicit new_name is forwarded verbatim and projected back.
func TestArchivedAgents_RestoreRenamed(t *testing.T) {
	etag := agentSetETag("c")
	var gotTarget string
	srv := newAgentAdminServer(t, &fakeArchivedAgentService{
		restoreFn: func(_ context.Context, _ core.Actor, _, target string) (AgentFileSet, error) {
			gotTarget = target
			return agentFileSet("beta", etag, "AGENT.md"), nil
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodPost,
		"/api/v1/archived-agents/reporter-20261022-093000/restore", `{"new_name":"beta"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "beta", gotTarget)

	_, view := decodeAdminAgentFilesView(t, rec)
	require.Equal(t, "beta", view.Agent)
	require.Equal(t, etag, view.ETag)
}

// TestArchivedAgents_RestoreMissingArchive 断言归档记录缺失映射 404，且文案点名
// 路径里的归档标识而不是 Agent 名。
// A missing archive row maps to a 404 that names the archive id from the path.
func TestArchivedAgents_RestoreMissingArchive(t *testing.T) {
	srv := newAgentAdminServer(t, &fakeArchivedAgentService{
		restoreFn: func(_ context.Context, _ core.Actor, archiveID, _ string) (AgentFileSet, error) {
			return AgentFileSet{}, fmt.Errorf("%w: 归档 %s", core.ErrResourceNotFound, archiveID)
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodPost,
		"/api/v1/archived-agents/gone-20260101-000000/restore", ""))
	require.Equal(t, http.StatusNotFound, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Equal(t, "未找到归档记录：gone-20260101-000000", env.Message)
}

// TestArchivedAgents_RestoreInvalidBody 断言可选的恢复请求体一旦给出就走严格解码：
// 未知字段 / 尾随内容 / 语法错误一律 400 固定文案，端口零调用。
// A present body is decoded strictly; every malformed shape is a fixed 400 with no
// port call.
func TestArchivedAgents_RestoreInvalidBody(t *testing.T) {
	cases := map[string]string{
		"unknown field":  `{"extra":true}`,
		"trailing":       `{"new_name":"beta"} {"new_name":"gamma"}`,
		"malformed json": `{"new_name":`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			var calls int
			srv := newAgentAdminServer(t, &fakeArchivedAgentService{
				restoreFn: func(context.Context, core.Actor, string, string) (AgentFileSet, error) {
					calls++
					return AgentFileSet{}, nil
				},
			})

			rec := serveAdmin(srv, agentAdminCall(http.MethodPost,
				"/api/v1/archived-agents/reporter-20261022-093000/restore", body))
			require.Equal(t, http.StatusBadRequest, rec.Code)

			env := decodeAuthEnvelope(t, rec)
			require.Equal(t, codeBadRequest, env.Code)
			require.Equal(t, messageArchiveRestoreBody, env.Message)
			require.Nil(t, env.Data)
			require.Zero(t, calls, "解码失败不得触达端口")
		})
	}
}

// TestArchivedAgents_PortMissing 断言三个归档端点缺端口时都是固定 500 INTERNAL
// （装配错误，不是空结果）；认证仍然先于端口检查。
// Every archive endpoint answers a fixed 500 when the port is missing.
func TestArchivedAgents_PortMissing(t *testing.T) {
	srv := newAgentAdminServer(t, nil)
	requests := []*http.Request{
		agentAdminCall(http.MethodDelete, "/api/v1/agents/reporter", ""),
		agentAdminCall(http.MethodGet, "/api/v1/archived-agents", ""),
		agentAdminCall(http.MethodPost, "/api/v1/archived-agents/reporter-20261022-093000/restore", ""),
	}
	for _, req := range requests {
		rec := serveAdmin(srv, req)
		require.Equal(t, http.StatusInternalServerError, rec.Code, "%s %s", req.Method, req.URL.Path)
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
	}
}

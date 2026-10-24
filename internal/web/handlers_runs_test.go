package web

// 本文件是运行查询与取消的 handler 验收（模块卡 §3.2.1：handlers_runs_test.go，
// 18 例）：过滤与非法参数、固定排序、详情清单、legacy 视图、取消终态 / 超时 /
// 404。假端口只记录调用并把预置结果原样返回——web 层若二次排序、补造清单字段或
// 改写取消文案，这里的断言就会红。
// Acceptance for the run-query and cancel handlers: filters and invalid parameters,
// fixed ordering, detail manifest, legacy view, and cancel terminal/running/404.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// runLedgerFake 是 RunService 的记录型替身：Run / Archive 只为满足端口方法集，
// ListRuns / GetRun / Cancel 记录入参并返回预置结果或错误。
// runLedgerFake records every ledger call; Run/Archive exist only to satisfy the port.
type runLedgerFake struct {
	mu sync.Mutex

	filters    []core.RunFilter
	listCalls  int
	listed     []core.RunDetail
	listErr    error
	detail     core.RunDetail
	getErr     error
	cancelled  []string
	cancelView core.RunDetail
	cancelErr  error
}

// var _ RunService = (*runLedgerFake)(nil) 让替身与端口方法集在编译期同步。
// The compile-time assertion keeps the fake in sync with the frozen port.
var _ RunService = (*runLedgerFake)(nil)

func (f *runLedgerFake) Run(context.Context, core.RunRequest) (core.RunResult, error) {
	return core.RunResult{}, nil
}

func (f *runLedgerFake) Archive(context.Context, string) error { return nil }

func (f *runLedgerFake) ListRuns(_ context.Context, filter core.RunFilter) ([]core.RunDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.filters = append(f.filters, filter)
	return f.listed, f.listErr
}

func (f *runLedgerFake) GetRun(context.Context, string) (core.RunDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.detail, f.getErr
}

func (f *runLedgerFake) Cancel(_ context.Context, runID string) (core.RunDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, runID)
	return f.cancelView, f.cancelErr
}

// lastFilter / listCallCount 供「参数原样传入」「非法参数零查询」断言。
func (f *runLedgerFake) lastFilter(t *testing.T) core.RunFilter {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.filters, "handler 必须调用 ListRuns")
	return f.filters[len(f.filters)-1]
}

func (f *runLedgerFake) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// canceledIDs 供取消路径的入参断言。
func (f *runLedgerFake) canceledIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cancelled)
}

// runEnvelope[T] 解出响应信封：code / message / data 三段。
type runEnvelope[T any] struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

// decodeRunEnvelope 把响应体解成指定 data 形态的信封。
func decodeRunEnvelope[T any](t *testing.T, rec *httptest.ResponseRecorder) runEnvelope[T] {
	t.Helper()
	var env runEnvelope[T]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env),
		"响应必须是合法信封：%s", rec.Body.String())
	return env
}

// newRunLedgerServer 装配一个只替身账本的管理面服务器。
func newRunLedgerServer(t *testing.T, fake *runLedgerFake) *Server {
	t.Helper()
	return newTestServer(t, func(o *Options) { o.Runs = fake })
}

// TestRuns_ListDefaultsAndFilters 覆盖缺省窗口、四个参数原样传入与未知参数忽略
// （约束 21：固定窗口、只认 session_id / agent / status / limit）。
// 逆改点：缺省 limit 写错 / 过滤值被改写 / 未知参数被当成过滤 → 对应断言红。
func TestRuns_ListDefaultsAndFilters(t *testing.T) {
	t.Run("缺省窗口 50 且无过滤", func(t *testing.T) {
		fake := &runLedgerFake{}
		rec := serveAdmin(newRunLedgerServer(t, fake), adminRequest(http.MethodGet, "/api/v1/runs", ""))
		require.Equal(t, http.StatusOK, rec.Code)

		require.Equal(t, core.RunFilter{Limit: defaultRunLimit}, fake.lastFilter(t),
			"缺省只填窗口 50，不得自行派生过滤条件")
		env := decodeRunEnvelope[[]RunView](t, rec)
		require.Equal(t, codeOK, env.Code)
		require.NotNil(t, env.Data, "空列表必须输出 [] 而不是 null")
		require.Empty(t, env.Data)
	})

	t.Run("三个过滤与窗口原样传入", func(t *testing.T) {
		fake := &runLedgerFake{}
		rec := serveAdmin(newRunLedgerServer(t, fake), adminRequest(http.MethodGet,
			"/api/v1/runs?session_id=s-1&agent=alpha&status=running&limit=7", ""))
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, core.RunFilter{
			SessionID: "s-1", Agent: "alpha", Status: "running", Limit: 7,
		}, fake.lastFilter(t), "过滤与窗口必须逐字传给仓储，不在 web 层改写")
	})

	t.Run("未知参数被忽略", func(t *testing.T) {
		fake := &runLedgerFake{}
		rec := serveAdmin(newRunLedgerServer(t, fake), adminRequest(http.MethodGet,
			"/api/v1/runs?sort=started_at&order=asc&page=2&offset=10", ""))
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, core.RunFilter{Limit: defaultRunLimit}, fake.lastFilter(t),
			"排序 / 分页参数不得影响过滤（约束 21）")
	})
}

// TestRuns_ListRejectsInvalidParameters 覆盖三类非法参数的 400 与「零查询」：
// 非法 status、非正整数 limit、超上限 limit、非整数 limit。
// 逆改点：任一非法值降级为缺省 / 触达仓储 → 对应断言红。
func TestRuns_ListRejectsInvalidParameters(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		wantPhrase string
	}{
		{name: "非法 status", query: "?status=bogus", wantPhrase: "status"},
		{name: "limit 为零", query: "?limit=0", wantPhrase: "limit"},
		{name: "limit 超上限", query: "?limit=201", wantPhrase: "limit"},
		{name: "limit 非整数", query: "?limit=abc", wantPhrase: "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &runLedgerFake{}
			rec := serveAdmin(newRunLedgerServer(t, fake), adminRequest(http.MethodGet, "/api/v1/runs"+tc.query, ""))
			require.Equal(t, http.StatusBadRequest, rec.Code)

			env := decodeRunEnvelope[any](t, rec)
			require.Equal(t, codeBadRequest, env.Code)
			require.Contains(t, env.Message, tc.wantPhrase, "400 文案必须点名非法参数")
			require.Equal(t, 0, fake.listCallCount(), "非法参数必须在触达仓储前被拒绝")
		})
	}
}

// TestRuns_ListResponse 覆盖固定排序原样保留、视图字段投影、空列表与仓储错误
// （约束 21 / 22 / 23）。
// 逆改点：二次排序 / 空列表序列化为 null / 错误文案泄漏底层原因 → 对应断言红。
func TestRuns_ListResponse(t *testing.T) {
	base := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

	t.Run("固定排序原样保留且字段投影", func(t *testing.T) {
		fake := &runLedgerFake{listed: []core.RunDetail{
			{
				RunRecord: core.RunRecord{
					RunID: "run-new", SessionID: "s-1", Agent: "alpha",
					Channel: "scheduler", Trigger: core.TriggerCron, StartedAt: base.Add(time.Hour),
				},
				Status: core.StatusFailed, Reason: "模型调用失败：inner detail",
				Iterations: 3, ToolCalls: 2, FinalText: "半截回答",
				ManifestHash: "manifest-hash-1", FinishedAt: base.Add(2 * time.Hour),
			},
			{
				RunRecord: core.RunRecord{
					RunID: "run-old", SessionID: "s-1", Agent: "alpha",
					Channel: "web", Trigger: core.Trigger("api"),
					ConfigHash: "config-1", StartedAt: base,
				},
				Status: core.StatusCompleted, FinalText: "完成",
			},
		}}
		rec := serveAdmin(newRunLedgerServer(t, fake), adminRequest(http.MethodGet, "/api/v1/runs", ""))
		require.Equal(t, http.StatusOK, rec.Code)

		env := decodeRunEnvelope[[]RunView](t, rec)
		require.Len(t, env.Data, 2)
		require.Equal(t, []string{"run-new", "run-old"},
			[]string{env.Data[0].RunID, env.Data[1].RunID}, "固定排序必须原样保留，不得二次排序")

		newest := env.Data[0]
		require.Equal(t, "cron", newest.Trigger)
		require.Equal(t, "failed", newest.Status)
		require.Equal(t, publicReasonFailed, newest.Reason, "原始原因不得出站（约束 23）")
		require.Equal(t, "manifest-hash-1", newest.ManifestHash)
		require.Equal(t, "2026-10-21T10:00:00Z", newest.StartedAt)
		require.Equal(t, "2026-10-21T11:00:00Z", newest.FinishedAt)
		require.Equal(t, "半截回答", newest.FinalText)

		oldest := env.Data[1]
		require.Equal(t, "api", oldest.Trigger)
		require.Equal(t, "config-1", oldest.ConfigHash)
		require.Empty(t, oldest.FinishedAt, "running 行没有终态时间，零值渲染空串")
	})

	t.Run("空列表输出空数组", func(t *testing.T) {
		rec := serveAdmin(newRunLedgerServer(t, &runLedgerFake{}),
			adminRequest(http.MethodGet, "/api/v1/runs", ""))
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `"data":[]`, "空列表必须是 []，不是 null")
	})

	t.Run("仓储错误返回 500 固定文案", func(t *testing.T) {
		fake := &runLedgerFake{listErr: errors.New("查询运行列表: 库文件 C:\\secret\\basalt.db 损坏")}
		rec := serveAdmin(newRunLedgerServer(t, fake), adminRequest(http.MethodGet, "/api/v1/runs", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		env := decodeRunEnvelope[any](t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
		require.NotContains(t, rec.Body.String(), "secret", "底层原因只进日志，不得出站")
	})
}

// TestRuns_DetailManifestView 覆盖准入清单的投影：kind 枚举、技能四个哈希、
// 文件扁平化排序与 legacy 视图（约束 23 / 24 / 32 / 注 17）。
// 逆改点：kind 写错 / l3 复算口径偏差 / 文件未排序 / legacy 填了 active → 红。
func TestRuns_DetailManifestView(t *testing.T) {
	base := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

	t.Run("recorded 清单逐项投影", func(t *testing.T) {
		manifest := &core.RunResourceManifest{
			FormatVersion:  1,
			Hash:           "manifest-hash-1",
			AgentLifecycle: "active",
			Provider: core.ProviderVersionRef{
				ResourceVersionRef: core.ResourceVersionRef{
					Name: "openai", VersionID: 11, VersionNo: 1, ContentHash: "provider-hash",
				},
			},
			MCPServers: []core.ResourceVersionRef{
				{Name: "search", VersionID: 21, VersionNo: 2, ContentHash: "mcp-search"},
				{Name: "zebra", VersionID: 22, VersionNo: 3, ContentHash: "mcp-zebra"},
			},
			Skills: []core.SkillVersionRef{
				{
					ResourceVersionRef: core.ResourceVersionRef{
						Name: "s1", VersionID: 31, VersionNo: 1, ContentHash: "skill-content",
					},
					// 故意乱序：投影必须按 path 升序复算与输出。
					Files: []core.FileHash{
						{Path: "SKILL.md", SHA256: "skill-md-hash", Size: 12},
						{Path: "b.md", SHA256: "hash-b", Size: 2},
						{Path: "a.md", SHA256: "hash-a", Size: 1},
					},
				},
				{
					ResourceVersionRef: core.ResourceVersionRef{
						Name: "s2", VersionID: 32, VersionNo: 1, ContentHash: "skill-content-2",
					},
					Files: []core.FileHash{
						{Path: "SKILL.md", SHA256: "skill-md-hash-2", Size: 8},
					},
				},
			},
		}
		fake := &runLedgerFake{detail: core.RunDetail{
			RunRecord: core.RunRecord{
				RunID: "run-1", SessionID: "s-1", Agent: "alpha",
				Channel: "web", Trigger: core.Trigger("api"), StartedAt: base,
			},
			Status: core.StatusCompleted, ManifestHash: "manifest-hash-1",
			FinishedAt: base.Add(time.Minute), ResourceManifest: manifest,
		}}
		rec := serveAdmin(newRunLedgerServer(t, fake), adminRequest(http.MethodGet, "/api/v1/runs/run-1", ""))
		require.Equal(t, http.StatusOK, rec.Code)

		env := decodeRunEnvelope[RunDetailView](t, rec)
		require.Equal(t, "run-1", env.Data.RunID)
		require.Equal(t, "manifest-hash-1", env.Data.ManifestHash, "运行行必须带清单哈希（契约 §1.1）")

		view := env.Data.ResourceManifest
		require.NotNil(t, view)
		require.False(t, view.LegacyUnrecorded)
		require.Equal(t, 1, view.FormatVersion)
		require.Equal(t, "active", view.AgentLifecycleState)

		require.Len(t, view.Providers, 1, "Provider 投影为单元素列表")
		require.Equal(t, "model_connection", view.Providers[0].Kind)
		require.Equal(t, "openai", view.Providers[0].Name)
		require.Equal(t, int64(11), view.Providers[0].VersionID)
		require.Equal(t, 1, view.Providers[0].VersionNo)
		require.Equal(t, "provider-hash", view.Providers[0].ContentHash)

		require.Len(t, view.MCPServers, 2)
		require.Equal(t, "mcp_server", view.MCPServers[0].Kind)
		require.Equal(t, []string{"search", "zebra"},
			[]string{view.MCPServers[0].Name, view.MCPServers[1].Name}, "MCP 顺序与持久清单一致")

		require.Len(t, view.Skills, 2)
		s1 := view.Skills[0]
		require.Equal(t, "s1", s1.Name)
		require.Equal(t, int64(31), s1.VersionID)
		require.Equal(t, 1, s1.VersionNo)
		require.Equal(t, "skill-content", s1.ContentHash)
		require.Equal(t, "skill-md-hash", s1.SkillMDHash, "skill_md_hash 取自清单里的 SKILL.md 项")
		require.Equal(t, "e8ec8c30c773448c3907f53de6a74d2443c29ef886edd68308dc52822c8827e5",
			s1.L3Hash, "l3_hash 按约束 32 对除 SKILL.md 外的清单复算")
		require.Empty(t, s1.L1Hash, "l1_hash 从未持久化，不得回读发布目录补造")
		require.Empty(t, s1.L2Hash, "l2_hash 从未持久化，不得回读发布目录补造")

		require.Equal(t, "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945",
			view.Skills[1].L3Hash, "空 L3 清单按 [] 的字节复算（约束 32）")

		require.Equal(t, []string{
			".basalt/skills/s1/SKILL.md",
			".basalt/skills/s1/a.md",
			".basalt/skills/s1/b.md",
			".basalt/skills/s2/SKILL.md",
		}, []string{
			view.Files[0].Path, view.Files[1].Path, view.Files[2].Path, view.Files[3].Path,
		}, "顶层 files 必须扁平化为逻辑工作区路径并按 path 升序")
		require.Len(t, view.Files, 4, "每个技能文件恰出现一次")
		require.Equal(t, "skill-md-hash", view.Files[0].SHA256)
		require.Equal(t, int64(12), view.Files[0].Size)
	})

	t.Run("legacy 视图省略生命周期状态", func(t *testing.T) {
		fake := &runLedgerFake{detail: core.RunDetail{
			RunRecord: core.RunRecord{
				RunID: "run-legacy", SessionID: "s-1", Agent: "alpha",
				Channel: "web", StartedAt: base,
			},
			Status:     core.StatusCompleted,
			FinishedAt: base.Add(time.Minute),
		}}
		rec := serveAdmin(newRunLedgerServer(t, fake),
			adminRequest(http.MethodGet, "/api/v1/runs/run-legacy", ""))
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotContains(t, rec.Body.String(), "agent_lifecycle_state",
			"历史 Run 的生命周期状态不可知，字段必须省略（注 17）")

		env := decodeRunEnvelope[RunDetailView](t, rec)
		require.Empty(t, env.Data.ManifestHash, "历史 Run 的 manifest_hash 为空串")
		view := env.Data.ResourceManifest
		require.NotNil(t, view)
		require.True(t, view.LegacyUnrecorded)
		require.Equal(t, legacyManifestFormatVersion, view.FormatVersion)
		require.Empty(t, view.AgentLifecycleState)
		require.NotNil(t, view.Providers, "legacy 数组必须是 []，不是 null")
		require.Empty(t, view.Providers)
		require.Empty(t, view.MCPServers)
		require.Empty(t, view.Skills)
		require.Empty(t, view.Files)
		require.Contains(t, rec.Body.String(), `"providers":[]`)
	})

	t.Run("未知运行返回 404", func(t *testing.T) {
		fake := &runLedgerFake{getErr: core.ErrRunNotFound}
		rec := serveAdmin(newRunLedgerServer(t, fake),
			adminRequest(http.MethodGet, "/api/v1/runs/run-missing", ""))
		require.Equal(t, http.StatusNotFound, rec.Code)

		env := decodeRunEnvelope[any](t, rec)
		require.Equal(t, codeNotFound, env.Code)
		require.Contains(t, env.Message, "run-missing", "404 文案回显运行标识")
	})
}

// TestRuns_DetailFieldsAndErrors 覆盖详情全字段投影与非未知仓储错误的 500。
// 逆改点：final_text 丢失 / 错误原因泄漏 → 对应断言红。
func TestRuns_DetailFieldsAndErrors(t *testing.T) {
	base := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

	t.Run("详情字段与最终文本", func(t *testing.T) {
		fake := &runLedgerFake{detail: core.RunDetail{
			RunRecord: core.RunRecord{
				RunID: "run-1", SessionID: "s-9", Agent: "ops-agent",
				Channel: "web", Trigger: core.Trigger("api"), ConfigHash: "config-9", StartedAt: base,
			},
			Status: core.StatusLimitReached, Reason: core.ReasonIterationLimit,
			Iterations: 8, ToolCalls: 5, FinalText: "达到上限前的回答",
			ManifestHash: "manifest-hash-9", FinishedAt: base.Add(2 * time.Minute),
		}}
		rec := serveAdmin(newRunLedgerServer(t, fake), adminRequest(http.MethodGet, "/api/v1/runs/run-1", ""))
		require.Equal(t, http.StatusOK, rec.Code)

		env := decodeRunEnvelope[RunDetailView](t, rec)
		require.Equal(t, "run-1", env.Data.RunID)
		require.Equal(t, "s-9", env.Data.SessionID)
		require.Equal(t, "ops-agent", env.Data.Agent)
		require.Equal(t, "web", env.Data.Channel)
		require.Equal(t, "api", env.Data.Trigger)
		require.Equal(t, "limit_reached", env.Data.Status)
		require.Equal(t, core.ReasonIterationLimit, env.Data.Reason,
			"白名单内的上限原因原样保留（约束 23 / 注 5）")
		require.Equal(t, 8, env.Data.Iterations)
		require.Equal(t, 5, env.Data.ToolCalls)
		require.Equal(t, "达到上限前的回答", env.Data.FinalText)
		require.Equal(t, "config-9", env.Data.ConfigHash)
		require.Equal(t, "manifest-hash-9", env.Data.ManifestHash)
		require.Equal(t, "2026-10-21T09:00:00Z", env.Data.StartedAt)
		require.Equal(t, "2026-10-21T09:02:00Z", env.Data.FinishedAt)
	})

	t.Run("非未知仓储错误返回 500", func(t *testing.T) {
		fake := &runLedgerFake{getErr: errors.New("读取运行详情: 磁盘 I/O 错误")}
		rec := serveAdmin(newRunLedgerServer(t, fake), adminRequest(http.MethodGet, "/api/v1/runs/run-1", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		env := decodeRunEnvelope[any](t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
		require.NotContains(t, rec.Body.String(), "磁盘")
	})
}

// TestRuns_Cancel 覆盖取消的三条出口（约束 20 / 注 15 / 图 G）：终态 → 200 +
// 无需取消；仍是 running（等待超时或信号未送达）→ 200 + 尚未结束；未知 → 404。
// 逆改点：伪造 cancelled / 超时改写成错误码 / 404 变 200 → 对应断言红。
func TestRuns_Cancel(t *testing.T) {
	base := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

	t.Run("终态返回当前状态并说明无需取消", func(t *testing.T) {
		fake := &runLedgerFake{cancelView: core.RunDetail{
			RunRecord: core.RunRecord{
				RunID: "run-done", SessionID: "s-1", Agent: "alpha",
				Channel: "web", StartedAt: base,
			},
			Status: core.StatusCompleted, FinalText: "早就跑完了",
			FinishedAt: base.Add(time.Minute),
		}}
		rec := serveAdmin(newRunLedgerServer(t, fake),
			adminRequest(http.MethodPost, "/api/v1/runs/run-done/cancel", ""))
		require.Equal(t, http.StatusOK, rec.Code)

		env := decodeRunEnvelope[RunView](t, rec)
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, cancelMessageTerminal, env.Message)
		require.Equal(t, "completed", env.Data.Status, "已终态的运行不得被取消改写")
		require.Equal(t, "早就跑完了", env.Data.FinalText)
		require.Equal(t, []string{"run-done"}, fake.canceledIDs())
	})

	t.Run("仍是运行中返回 running 与尚未结束", func(t *testing.T) {
		fake := &runLedgerFake{cancelView: core.RunDetail{
			RunRecord: core.RunRecord{
				RunID: "run-live", SessionID: "s-1", Agent: "alpha",
				Channel: "web", StartedAt: base,
			},
			Status: core.StatusRunning,
		}}
		rec := serveAdmin(newRunLedgerServer(t, fake),
			adminRequest(http.MethodPost, "/api/v1/runs/run-live/cancel", ""))
		require.Equal(t, http.StatusOK, rec.Code)

		env := decodeRunEnvelope[RunView](t, rec)
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, cancelMessageRunning, env.Message)
		require.Equal(t, "running", env.Data.Status, "超时或信号未送达时不得伪造 cancelled")
		require.Empty(t, env.Data.FinishedAt)
	})

	t.Run("未知运行返回 404", func(t *testing.T) {
		fake := &runLedgerFake{cancelErr: core.ErrRunNotFound}
		rec := serveAdmin(newRunLedgerServer(t, fake),
			adminRequest(http.MethodPost, "/api/v1/runs/run-missing/cancel", ""))
		require.Equal(t, http.StatusNotFound, rec.Code)

		env := decodeRunEnvelope[any](t, rec)
		require.Equal(t, codeNotFound, env.Code)
		require.Contains(t, env.Message, "run-missing")
		require.Equal(t, []string{"run-missing"}, fake.canceledIDs(), "404 之前必须已向服务请求取消")
	})
}

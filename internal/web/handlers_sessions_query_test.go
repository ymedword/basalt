package web

// 本文件覆盖会话列表查询端点（模块卡 §3.1.26 / §4.1 第 22 行，10 例）：过滤维度
// 逐字透传、非法 status 的失败关闭（零仓储调用）、存储层固定排序原样照搬、
// archived_at 派生状态投影与空集合 `[]`。排序**不**由 handler 决定——用例预置的
// 标识序与最后活跃序故意不一致，handler 一旦二次排序就会读出不同顺序。
// Session-list query acceptance: filter passthrough, fail-closed status validation,
// storage-fixed ordering preserved verbatim, and empty collections as [].

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// recordingSessionStore 在内存假件外记录每次 ListSessions 的过滤参数与调用次数：
// 「非法 status 零仓储调用」与「过滤维度逐字透传」都需要观测到调用本身。
// recordingSessionStore wraps the in-memory fake and records every filter argument.
type recordingSessionStore struct {
	*fakeSessionStore
	mu      sync.Mutex
	filters []core.SessionFilter
}

// newRecordingSessionStore 构造带空内存表的记录型仓储。
// newRecordingSessionStore builds a recording store over an empty in-memory table.
func newRecordingSessionStore() *recordingSessionStore {
	return &recordingSessionStore{fakeSessionStore: &fakeSessionStore{
		sessions: map[string]core.SessionRecord{},
		messages: map[string][]core.Message{},
	}}
}

// ListSessions 记录过滤器后委托内存假件。
// ListSessions records the filter and delegates to the in-memory fake.
func (r *recordingSessionStore) ListSessions(ctx context.Context, filter core.SessionFilter) ([]core.SessionRecord, error) {
	r.mu.Lock()
	r.filters = append(r.filters, filter)
	r.mu.Unlock()
	return r.fakeSessionStore.ListSessions(ctx, filter)
}

// recordedFilters 返回已记录的过滤器副本（按调用顺序）。
func (r *recordingSessionStore) recordedFilters() []core.SessionFilter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.SessionFilter(nil), r.filters...)
}

// sessionsQueryBase 是夹具时间基线（视图时间字段按 RFC3339 UTC 预测）。
// sessionsQueryBase is the fixture time base for view timestamp assertions.
var sessionsQueryBase = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// seedSessionQueryRows 预置三行：标识序（web-a, web-m, web-z）与最后活跃序
// （web-m, web-z, web-a）**故意不一致**——handler 若按 last_active_at 二次排序，
// 「固定排序照搬」用例会读出另一顺序。
// The seeded id order deliberately differs from last_active order, so any handler-side
// re-sort changes the observed order.
func seedSessionQueryRows(t *testing.T, store *recordingSessionStore) {
	t.Helper()
	store.seed(core.SessionRecord{
		ID: "web-a", ProfileName: "demo", Channel: "web", UserID: "u1",
		Status: statusActive, MessageCount: 2,
		CreatedAt: sessionsQueryBase, LastActiveAt: sessionsQueryBase,
	})
	store.seed(core.SessionRecord{
		ID: "web-m", ProfileName: "ops-agent", Channel: "web", UserID: "u2",
		Status: statusArchived, MessageCount: 5,
		CreatedAt: sessionsQueryBase, LastActiveAt: sessionsQueryBase.Add(2 * time.Hour),
		ArchivedAt: timePtr(sessionsQueryBase.Add(3 * time.Hour)),
	})
	store.seed(core.SessionRecord{
		ID: "web-z", ProfileName: "demo", Channel: "web", UserID: "u3",
		Status: statusActive, MessageCount: 1,
		CreatedAt: sessionsQueryBase, LastActiveAt: sessionsQueryBase.Add(time.Hour),
	})
}

// timePtr 取时间指针（归档时刻列是可空列）。
func timePtr(t time.Time) *time.Time { return &t }

// newSessionsQueryServer 装配带三行夹具的 handler 级服务。
func newSessionsQueryServer(t *testing.T) (*Server, *recordingSessionStore) {
	t.Helper()
	store := newRecordingSessionStore()
	seedSessionQueryRows(t, store)
	s := newTestServer(t, func(o *Options) { o.Sessions = store })
	return s, store
}

// getSessions 发起一次列表请求并返回记录器（状态码由调用方断言）。
func getSessions(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	return serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/sessions"+query, ""))
}

// decodeSessionList 解析成功响应的数据数组。
func decodeSessionList(t *testing.T, rec *httptest.ResponseRecorder) resourcesEnvelope[[]SessionSummaryView] {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code)
	return decodeResourcesEnvelope[[]SessionSummaryView](t, rec)
}

// TestSessions_QueryListUnfiltered —— 无查询参数时按命令面全量口径返回（含归档行），
// 过滤器为零值。
// TestSessions_QueryListUnfiltered pins the unfiltered full-list behavior.
func TestSessions_QueryListUnfiltered(t *testing.T) {
	s, store := newSessionsQueryServer(t)
	env := decodeSessionList(t, getSessions(t, s, ""))

	require.Equal(t, codeOK, env.Code)
	require.Len(t, env.Data, 3, "无过滤 = 全量（含归档行）")
	require.Equal(t, []core.SessionFilter{{}}, store.recordedFilters(), "无过滤时过滤器必须为零值")
}

// TestSessions_QueryListFiltersByAgent —— `agent` 逐字进入过滤器，响应只含该画像行。
// TestSessions_QueryListFiltersByAgent pins the agent filter dimension.
func TestSessions_QueryListFiltersByAgent(t *testing.T) {
	s, store := newSessionsQueryServer(t)
	env := decodeSessionList(t, getSessions(t, s, "?agent=demo"))

	require.Equal(t, []core.SessionFilter{{Agent: "demo"}}, store.recordedFilters())
	require.Len(t, env.Data, 2)
	for _, item := range env.Data {
		require.Equal(t, "demo", item.ProfileName)
	}
}

// TestSessions_QueryListFiltersByActiveStatus —— `status=active` 逐字进入过滤器，
// 响应只含未归档行。
// TestSessions_QueryListFiltersByActiveStatus pins the active status dimension.
func TestSessions_QueryListFiltersByActiveStatus(t *testing.T) {
	s, store := newSessionsQueryServer(t)
	env := decodeSessionList(t, getSessions(t, s, "?status=active"))

	require.Equal(t, []core.SessionFilter{{Status: statusActive}}, store.recordedFilters())
	require.Len(t, env.Data, 2)
	for _, item := range env.Data {
		require.Equal(t, statusActive, item.Status)
	}
}

// TestSessions_QueryListFiltersByArchivedStatus —— `status=archived` 只回归档行。
// TestSessions_QueryListFiltersByArchivedStatus pins the archived status dimension.
func TestSessions_QueryListFiltersByArchivedStatus(t *testing.T) {
	s, store := newSessionsQueryServer(t)
	env := decodeSessionList(t, getSessions(t, s, "?status=archived"))

	require.Equal(t, []core.SessionFilter{{Status: statusArchived}}, store.recordedFilters())
	require.Len(t, env.Data, 1)
	require.Equal(t, "web-m", env.Data[0].SessionID)
	require.Equal(t, statusArchived, env.Data[0].Status)
}

// TestSessions_QueryListCombinesFilters —— 两个维度同时给出时进入同一个过滤器。
// TestSessions_QueryListCombinesFilters pins both dimensions at once.
func TestSessions_QueryListCombinesFilters(t *testing.T) {
	s, store := newSessionsQueryServer(t)
	env := decodeSessionList(t, getSessions(t, s, "?agent=demo&status=active"))

	require.Equal(t, []core.SessionFilter{{Agent: "demo", Status: statusActive}}, store.recordedFilters())
	require.Len(t, env.Data, 2)
	for _, item := range env.Data {
		require.Equal(t, "demo", item.ProfileName)
		require.Equal(t, statusActive, item.Status)
	}
}

// TestSessions_QueryListRejectsInvalidStatus —— 非法 status 在触达仓储之前 400
// （失败关闭：绝不退化成"不过滤"把归档行混进结果）。
// 逆改点：校验移到仓储调用之后，或默认分支落成过滤为空 → 本用例红。
// TestSessions_QueryListRejectsInvalidStatus pins the fail-closed 400.
func TestSessions_QueryListRejectsInvalidStatus(t *testing.T) {
	s, store := newSessionsQueryServer(t)
	rec := getSessions(t, s, "?status=closed")

	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeResourcesEnvelope[map[string]any](t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Equal(t, "status 只接受 active 或 archived", env.Message)
	require.Empty(t, store.recordedFilters(), "非法 status 不得触达仓储")
}

// TestSessions_QueryListIgnoresUnknownParams —— 排序 / 窗口类参数不在本端点契约里，
// 出现时忽略而不是报错（契约只认 agent / status 两个过滤维）。
// TestSessions_QueryListIgnoresUnknownParams pins that unknown query parameters are
// ignored rather than rejected.
func TestSessions_QueryListIgnoresUnknownParams(t *testing.T) {
	s, store := newSessionsQueryServer(t)
	env := decodeSessionList(t, getSessions(t, s, "?sort=created_at&limit=1&offset=2&status="))

	require.Len(t, env.Data, 3, "未知参数不得改变结果集")
	require.Equal(t, []core.SessionFilter{{}}, store.recordedFilters(),
		"空 status 与未知参数都不得进入过滤器")
}

// TestSessions_QueryListPreservesFixedOrder —— handler 逐字照搬仓储顺序：夹具按标识
// 升序返回，若 handler 二次按 last_active_at DESC 排序会得到 web-m, web-z, web-a。
// 逆改点：在 handler 内加 sort.Slice → 顺序断言红。
// TestSessions_QueryListPreservesFixedOrder proves the handler never re-sorts.
func TestSessions_QueryListPreservesFixedOrder(t *testing.T) {
	s, _ := newSessionsQueryServer(t)
	env := decodeSessionList(t, getSessions(t, s, ""))

	ids := make([]string, 0, len(env.Data))
	for _, item := range env.Data {
		ids = append(ids, item.SessionID)
	}
	require.Equal(t, []string{"web-a", "web-m", "web-z"}, ids,
		"顺序必须与仓储返回逐字一致（last_active_at DESC 排序由 storage 固定，handler 不重排）")
}

// TestSessions_QueryListProjectsDerivedArchivedAt —— 状态与时间字段原样投影：
// 归档行的 archived 状态取自记录（由 storage 按 archived_at 派生），视图不改写；
// 时间字段按 RFC3339 UTC 出站。
// TestSessions_QueryListProjectsDerivedArchivedAt pins the status and time projection.
func TestSessions_QueryListProjectsDerivedArchivedAt(t *testing.T) {
	s, _ := newSessionsQueryServer(t)
	env := decodeSessionList(t, getSessions(t, s, "?status=archived"))

	require.Len(t, env.Data, 1)
	item := env.Data[0]
	require.Equal(t, statusArchived, item.Status, "archived_at 非空的派生状态必须原样投影")
	require.Equal(t, "2026-10-18T12:00:00Z", item.CreatedAt)
	require.Equal(t, "2026-10-18T14:00:00Z", item.LastActiveAt)
	require.Equal(t, 5, item.MessageCount)
	require.Equal(t, "web", item.Channel)
	require.Equal(t, "u2", item.UserID)
}

// TestSessions_QueryListEmptyRendersArray —— 查无结果时输出 `[]` 而不是 `null`
// （约束 34：空集合不得破坏 SDK 反序列化）。
// 逆改点：把 `make([]T, 0, n)` 改成 `var out []T` → 原始载荷里出现 `null`。
// TestSessions_QueryListEmptyRendersArray pins the empty array encoding.
func TestSessions_QueryListEmptyRendersArray(t *testing.T) {
	s, _ := newSessionsQueryServer(t)
	rec := getSessions(t, s, "?agent=ghost")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"data":[]`, "空结果必须是 []")

	var env resourcesEnvelope[[]SessionSummaryView]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.NotNil(t, env.Data)
	require.Empty(t, env.Data)
}

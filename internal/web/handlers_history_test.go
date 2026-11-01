package web

// 本文件覆盖历史回查三条端点的 HTTP 层（US-16 / T056），判据取自冻结契约
// `docs/contracts/openapi.yaml` 的 history 组与 `GET /sessions`：
//
//  1. **形状 400 在调用端口之前**（缺 q / limit 越界 / 时间非 RFC3339），零端口调用；
//  2. **跨调用方 404 不是 403**（C-28）：领域层给出 `ErrNotFound` 时响应必须是否认
//     "存在"，而不是否认"有权"；
//  3. **引用标注逐条取当次结论**：`valid` / `reason` 只来自本次 `Check`，无权判定
//     收敛成 `revoked` 而不是让整条读取失败；
//  4. **缺端口是装配错误**：固定 500，**不得**用空命中 / 空引用冒充合法空状态；
//  5. **会话列表的归属过滤是强制项**：调用方只见自己的行，管理员跨调用方。
//
// 逆改点写在每个用例头上。`evidence_refs` 是否存在用原始 JSON 键断言——结构体
// 零值区分不出"字段没出站"与"字段出了空值"。
// This file pins the three history endpoints: shape checks before the port, cross-caller
// reads as 404 (never 403), per-read validity annotation, fail-closed missing ports, and
// the enforced ownership filter on the session list.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/history"
	"github.com/ymedword/basalt/internal/knowledge"
)

// fakeHistorySearch 是可编程的检索端口：返回值、错误与入参各自独立记录。
// fakeHistorySearch is a programmable search port that records its arguments.
type fakeHistorySearch struct {
	result history.SearchResult
	err    error

	calls        int
	gotPrincipal core.Principal
	gotQuery     history.HistoryQuery
}

// Search 记录身份与查询后返回预置结果。
// Search records the principal and query, then returns the configured result.
func (f *fakeHistorySearch) Search(
	_ context.Context, p core.Principal, q history.HistoryQuery,
) (history.SearchResult, error) {
	f.calls++
	f.gotPrincipal, f.gotQuery = p, q
	if f.err != nil {
		return history.SearchResult{}, f.err
	}
	return f.result, nil
}

// fakeHistoryExactRead 是可编程的精确读取端口。
// fakeHistoryExactRead is a programmable exact-read port.
type fakeHistoryExactRead struct {
	view history.MessageView
	err  error

	calls        int
	gotPrincipal core.Principal
	gotRef       history.MessageRef
}

// Read 记录标识与身份后返回预置视图。
// Read records the ref and principal, then returns the configured view.
func (f *fakeHistoryExactRead) Read(
	_ context.Context, p core.Principal, ref history.MessageRef,
) (history.MessageView, error) {
	f.calls++
	f.gotPrincipal, f.gotRef = p, ref
	if f.err != nil {
		return history.MessageView{}, f.err
	}
	return f.view, nil
}

// fakeEvidenceValidity 是可编程的引用判定端口：按引用标识给出结论或错误。
// fakeEvidenceValidity is a programmable validity port keyed by reference id.
type fakeEvidenceValidity struct {
	byID  map[string]knowledge.ValidityResult
	errID map[string]error

	calls        int
	gotPrincipal core.Principal
	gotRefs      []knowledge.EvidenceRef
}

// Check 记录判定过哪些引用并返回按标识预置的结论。
// Check records every checked ref and returns the verdict configured for its id.
func (f *fakeEvidenceValidity) Check(
	_ context.Context, p core.Principal, ref knowledge.EvidenceRef,
) (knowledge.ValidityResult, error) {
	f.calls++
	f.gotPrincipal = p
	f.gotRefs = append(f.gotRefs, ref)
	if err, ok := f.errID[ref.ID]; ok {
		return knowledge.ValidityResult{}, err
	}
	if result, ok := f.byID[ref.ID]; ok {
		return result, nil
	}
	return knowledge.ValidityResult{Valid: true, VersionID: ref.VersionID}, nil
}

// historyFakes 打包三个端口，便于用例按需改写。
// historyFakes bundles the three history ports.
type historyFakes struct {
	search   *fakeHistorySearch
	read     *fakeHistoryExactRead
	validity *fakeEvidenceValidity
}

// newHistoryServer 装配带三个历史端口的 handler 级服务。
// newHistoryServer wires the three history ports into a handler-level server.
func newHistoryServer(t *testing.T, mutate func(*Options)) (*Server, *historyFakes) {
	t.Helper()
	fakes := &historyFakes{
		search:   &fakeHistorySearch{},
		read:     &fakeHistoryExactRead{},
		validity: &fakeEvidenceValidity{},
	}
	srv := newTestServer(t, func(o *Options) {
		o.HistorySearch = fakes.search
		o.HistoryExactRead = fakes.read
		o.EvidenceValidity = fakes.validity
		if mutate != nil {
			mutate(o)
		}
	})
	return srv, fakes
}

// historyGet 发起一次 GET 请求（管理凭据，走真实的 admin token 闸门）。
// historyGet issues one GET with the admin token.
func historyGet(s *Server, path string) *httptest.ResponseRecorder {
	return serveAdmin(s, adminRequest(http.MethodGet, path, ""))
}

// TestHistory_SearchValidatesShapeBeforePort 判四条形状 400 都在触达端口之前发生：
// 缺 q、q 全空白、limit 越界（0 / 201 / 非整数）、时间参数非 RFC3339。
// 逆改点：把任一校验挪到 Search 之后 → calls 非 0；去掉 trim → 空白 q 断言红。
func TestHistory_SearchValidatesShapeBeforePort(t *testing.T) {
	t.Parallel()
	srv, fakes := newHistoryServer(t, nil)

	cases := []struct {
		name, query, message string
	}{
		{"缺 q", "?limit=10", messageHistoryQueryRequired},
		{"q 全空白", "?q=%20%20", messageHistoryQueryRequired},
		{"limit 为 0", "?q=x&limit=0", messageHistoryLimitRange},
		{"limit 超上限", "?q=x&limit=201", messageHistoryLimitRange},
		{"limit 非整数", "?q=x&limit=abc", messageHistoryLimitRange},
		{"from 非 RFC3339", "?q=x&from=2026-10-18", "from 必须是 RFC3339 时间"},
		{"to 非 RFC3339", "?q=x&to=yesterday", "to 必须是 RFC3339 时间"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := historyGet(srv, "/api/v1/history/search"+tc.query)
			requireKnowledgeError(t, rec, http.StatusBadRequest, codeBadRequest, tc.message)
		})
	}
	require.Zero(t, fakes.search.calls, "形状非法不得触达检索端口")
}

// TestHistory_SearchPassesQueryThroughPort 判六个查询参数逐字进入领域查询：
// 关键词原样（不去空白，去空白由领域层判）、过滤维原样、时间解析成 UTC、limit 整数。
// 逆改点：handler 自己 trim 关键词 → gotQuery 断言红；丢掉某个过滤维 → 对应断言红。
func TestHistory_SearchPassesQueryThroughPort(t *testing.T) {
	t.Parallel()
	srv, fakes := newHistoryServer(t, nil)

	rec := historyGet(srv,
		"/api/v1/history/search?q=%E5%B7%A1%E6%A3%80%20%E5%91%8A%E8%AD%A6&agent=demo"+
			"&session_id=s-1&caller_id=cal_1&limit=7"+
			"&from=2026-10-18T00:00:00Z&to=2026-10-19T00:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 1, fakes.search.calls)

	got := fakes.search.gotQuery
	require.Equal(t, "巡检 告警", got.Keywords)
	require.Equal(t, "demo", got.Agent)
	require.Equal(t, "s-1", got.SessionID)
	require.Equal(t, "cal_1", got.CallerID)
	require.Equal(t, 7, got.Limit)
	require.Equal(t, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), got.From)
	require.Equal(t, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), got.To)
}

// TestHistory_SearchMapsDomainErrors 判四个领域错误的固定映射：越权 403（业务文案）、
// 形状非法 400、装配不可用 500、存储违约 500。
// 逆改点：把 ErrForbidden 映射成 404（或反过来把 ErrNotFound 映射成 403）→ 断言红。
func TestHistory_SearchMapsDomainErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"越权", history.ErrForbidden, http.StatusForbidden, codeForbidden, messageScopeDenied},
		{"倒置时间窗", history.ErrInvalidQuery, http.StatusBadRequest, codeBadRequest, "历史检索参数非法"},
		{"装配不可用", history.ErrHistoryUnavailable, http.StatusInternalServerError, codeInternal, messageInternal},
		{"存储违约", errors.New("boom"), http.StatusInternalServerError, codeInternal, messageInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fakes := newHistoryServer(t, nil)
			fakes.search.err = tc.err
			rec := historyGet(srv, "/api/v1/history/search?q=x")
			requireKnowledgeError(t, rec, tc.status, tc.code, tc.message)
			require.Equal(t, 1, fakes.search.calls)
		})
	}
}

// TestHistory_SearchMissingPortFailsClosed 判缺端口是 500 而不是 200 + 空命中：
// 空命中会被调用方读成"查过了，没有"。
// 逆改点：nil 端口时直接 `writeOK(空结果)` → 状态断言红。
func TestHistory_SearchMissingPortFailsClosed(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, func(o *Options) { o.HistorySearch = nil })
	requireKnowledgeError(t, historyGet(srv, "/api/v1/history/search?q=x"),
		http.StatusInternalServerError, codeInternal, messageInternal)
}

// TestHistory_SearchResultShape 判检索响应形状：命中恒为 `[]`（不是 `null`），
// `truncated` 恒出站且逐字透传，命中只带指针与片段。
// 逆改点：投影用 `var hits []T` → 空集合断言红；丢掉 Truncated → 截断断言红。
func TestHistory_SearchResultShape(t *testing.T) {
	t.Parallel()
	srv, fakes := newHistoryServer(t, nil)

	rec := historyGet(srv, "/api/v1/history/search?q=x")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"hits":[]`, "空命中必须是 []")
	require.Contains(t, rec.Body.String(), `"truncated":false`, "truncated 必须出站")

	fakes.search.result = history.SearchResult{
		Hits: []history.HistoryHit{{
			MessageID: "42", SessionID: "s-1", RunID: "run-1", Role: "assistant",
			Snippet: "…巡检完成…", Score: 0.75,
			CreatedAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		}},
		Truncated: true,
	}
	rec = historyGet(srv, "/api/v1/history/search?q=x")
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeKnowledge[HistorySearchResultView](t, rec)
	require.True(t, env.Data.Truncated, "截断标记必须逐字透传")
	require.Len(t, env.Data.Hits, 1)
	hit := env.Data.Hits[0]
	require.Equal(t, "42", hit.MessageID)
	require.Equal(t, "…巡检完成…", hit.Snippet)
	require.Equal(t, 0.75, hit.Score)
	require.Equal(t, "2026-10-18T12:00:00Z", hit.CreatedAt)

	fields := knowledgeDataFields(t, rec)
	hitFields := decodeRawMap(t, fields["hits"])
	require.Contains(t, hitFields[0], "snippet")
	require.NotContains(t, hitFields[0], "content", "命中不得携带正文（只有精确读取才给原文）")
}

// TestHistory_ReadMessageMapsDomainErrors 判精确读取的错误映射：不存在 404（点名
// 消息标识）、保留策略删除 410、越权 403、存储违约 500。契约该端点**没有** 400。
// 逆改点：把 ErrNotFound 归进 400 → 状态断言红；去掉标识回显 → 文案断言红。
func TestHistory_ReadMessageMapsDomainErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{"不存在", history.ErrNotFound, http.StatusNotFound, codeNotFound, "未找到消息：42"},
		{
			"保留策略删除", history.ErrDeleted, http.StatusGone, codeGone,
			errorCodeMessage(codeGone, "42"),
		},
		{"越权", history.ErrForbidden, http.StatusForbidden, codeForbidden, messageScopeDenied},
		{
			"装配不可用", history.ErrHistoryUnavailable, http.StatusInternalServerError,
			codeInternal, messageInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, fakes := newHistoryServer(t, nil)
			fakes.read.err = tc.err
			rec := historyGet(srv, "/api/v1/sessions/s-1/messages/42")
			requireKnowledgeError(t, rec, tc.status, tc.code, tc.message)
			require.Equal(t, history.MessageRef{MessageID: "42", SessionID: "s-1"},
				fakes.read.gotRef, "两个路径参数必须逐字透传")
		})
	}
}

// TestHistory_ReadMessageCrossCallerIsNotFoundNotForbidden 判跨调用方的响应是 404
// 而不是 403：403 会泄露"这条消息存在"（C-28）。
// 逆改点：把 ErrNotFound 与 ErrForbidden 的映射对调 → 本用例红。
func TestHistory_ReadMessageCrossCallerIsNotFoundNotForbidden(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"hc-secret": callerPrincipal("cal_1", core.ScopeSessionsRead),
	}}
	srv, fakes := newHistoryServer(t, func(o *Options) { o.AdminAuth = businessGuard(t, keys) })
	fakes.read.err = history.ErrNotFound

	rec := serveAdmin(srv, withBearer(
		adminRequest(http.MethodGet, "/api/v1/sessions/s-2/messages/42", ""), "hc-secret"))
	requireKnowledgeError(t, rec, http.StatusNotFound, codeNotFound, "未找到消息：42")
	require.Equal(t, core.PrincipalCaller, fakes.read.gotPrincipal.Kind,
		"调用方身份必须原样交给领域层结算归属（web 不自行过滤）")
	require.Equal(t, "cal_1", fakes.read.gotPrincipal.CallerID)
}

// TestHistory_ReadMessageRequiresSessionsReadScope 判路由行的作用域：没有
// `sessions:read` 的调用方 Key 在认证闸门就被拒绝（403），不会走到端口。
// 逆改点：路由行换成 `sessions:write` 或漏声明 scope → 状态与零调用断言红。
func TestHistory_ReadMessageRequiresSessionsReadScope(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"hs-secret": callerPrincipal("cal_1", core.ScopeSessionsWrite),
	}}
	srv, fakes := newHistoryServer(t, func(o *Options) { o.AdminAuth = businessGuard(t, keys) })

	rec := serveAdmin(srv, withBearer(
		adminRequest(http.MethodGet, "/api/v1/history/search?q=x", ""), "hs-secret"))
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Zero(t, fakes.search.calls, "缺 scope 不得触达端口")
}

// TestHistory_ReadMessageAnnotatesEvidenceValidity 判引用标注逐条取**本次**结论：
// 一条仍有效、一条已撤权，响应必须是同一条消息里两个不同结论。
// 逆改点：把结论写死成 valid / 复用第一次的结论 → 第二段断言红。
func TestHistory_ReadMessageAnnotatesEvidenceValidity(t *testing.T) {
	t.Parallel()
	srv, fakes := newHistoryServer(t, nil)
	fakes.read.view = history.MessageView{
		MessageID: "42", SessionID: "s-1", RunID: "run-1", Seq: 3,
		Role: "assistant", Content: "巡检结论见附件。",
		CreatedAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		EvidenceRefs: []core.EvidenceRef{
			{Kind: "knowledge", ID: "doc-1", VersionID: "v1", ContentHash: "h1"},
			{Kind: "knowledge", ID: "doc-2", VersionID: "v2", ContentHash: "h2"},
		},
	}
	fakes.validity.byID = map[string]knowledge.ValidityResult{
		"doc-1": {Valid: true, VersionID: "v1", ContentHash: "h1"},
		"doc-2": {Valid: false, Reason: knowledge.ReasonRevoked, VersionID: "v2"},
	}

	rec := historyGet(srv, "/api/v1/sessions/s-1/messages/42")
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeKnowledge[MessageDetailView](t, rec)
	require.Equal(t, "巡检结论见附件。", env.Data.Content)
	require.Equal(t, int64(3), env.Data.Seq)
	require.Equal(t, "2026-10-18T12:00:00Z", env.Data.CreatedAt)
	require.False(t, env.Data.Truncated)
	require.Len(t, env.Data.EvidenceRefs, 2)
	require.True(t, env.Data.EvidenceRefs[0].Validity.Valid)
	require.Empty(t, env.Data.EvidenceRefs[0].Validity.Reason)
	require.False(t, env.Data.EvidenceRefs[1].Validity.Valid)
	require.Equal(t, knowledge.ReasonRevoked, env.Data.EvidenceRefs[1].Validity.Reason)
	require.Equal(t, "doc-2", env.Data.EvidenceRefs[1].Ref.ID)
	require.Equal(t, "v2", env.Data.EvidenceRefs[1].Ref.VersionID)
	require.Len(t, fakes.validity.gotRefs, 2, "每条引用都要单独判定，不得合并")

	// 契约的 `EvidenceRef` 没有 kind 字段：结构体与原始 JSON 都必须不带它。
	fields := knowledgeDataFields(t, rec)
	refs := decodeRawMap(t, fields["evidence_refs"])
	require.NotContains(t, refs[0], "kind", "契约 EvidenceRef 无 kind 字段")
	require.Contains(t, refs[0], "validity")
}

// TestHistory_ReadMessageValidityDenialBecomesRevoked 判"能读消息但无权判定"收敛成
// 标注而不是错误：`Check` 报越权时读取照常 200，该引用标 `revoked`。
// 逆改点：把 ErrForbidden 当整条读取失败（403）→ 状态断言红。
func TestHistory_ReadMessageValidityDenialBecomesRevoked(t *testing.T) {
	t.Parallel()
	srv, fakes := newHistoryServer(t, nil)
	fakes.read.view = history.MessageView{
		MessageID: "42", SessionID: "s-1", Role: "assistant", Content: "原文",
		EvidenceRefs: []core.EvidenceRef{{ID: "doc-1", VersionID: "v1"}},
	}
	fakes.validity.errID = map[string]error{"doc-1": core.ErrForbidden}

	rec := historyGet(srv, "/api/v1/sessions/s-1/messages/42")
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeKnowledge[MessageDetailView](t, rec)
	require.Len(t, env.Data.EvidenceRefs, 1)
	require.False(t, env.Data.EvidenceRefs[0].Validity.Valid)
	require.Equal(t, knowledge.ReasonRevoked, env.Data.EvidenceRefs[0].Validity.Reason)
}

// TestHistory_ReadMessageValidityStoreViolationFailsClosed 判判定端口违约时整条读取
// 500：无法确认状态**不得**写成"有效"或"失效"。
// 逆改点：把默认分支标成 revoked / valid 后照常 200 → 状态断言红。
func TestHistory_ReadMessageValidityStoreViolationFailsClosed(t *testing.T) {
	t.Parallel()
	srv, fakes := newHistoryServer(t, nil)
	fakes.read.view = history.MessageView{
		MessageID: "42", SessionID: "s-1", Role: "assistant", Content: "原文",
		EvidenceRefs: []core.EvidenceRef{{ID: "doc-1", VersionID: "v1"}},
	}
	fakes.validity.errID = map[string]error{"doc-1": errors.New("boom")}

	requireKnowledgeError(t, historyGet(srv, "/api/v1/sessions/s-1/messages/42"),
		http.StatusInternalServerError, codeInternal, messageInternal)
}

// TestHistory_ReadMessageWithoutRefsNeedsNoValidityPort 判没有引用的消息不要求判定
// 端口：字段整体缺席（不是空数组），缺端口也不影响读取。
// 逆改点：无条件调用端口 → nil 端口 panic / 500；用 `[]` 而非省略 → 键存在断言红。
func TestHistory_ReadMessageWithoutRefsNeedsNoValidityPort(t *testing.T) {
	t.Parallel()
	srv, fakes := newHistoryServer(t, func(o *Options) { o.EvidenceValidity = nil })
	fakes.read.view = history.MessageView{
		MessageID: "42", SessionID: "s-1", Role: "user", Content: "帮我看看巡检结果",
	}

	rec := historyGet(srv, "/api/v1/sessions/s-1/messages/42")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, knowledgeDataFields(t, rec), "evidence_refs",
		"没有引用时该字段必须缺席")
}

// TestHistory_ReadMessageWithRefsNeedsValidityPort 判有引用而缺判定端口是装配错误：
// 固定 500，**不得**静默丢掉标注照常返回。
// 逆改点：缺端口时跳过标注 → 状态断言红（200 与 500 的差别就是"标注丢了没有"）。
func TestHistory_ReadMessageWithRefsNeedsValidityPort(t *testing.T) {
	t.Parallel()
	srv, fakes := newHistoryServer(t, func(o *Options) { o.EvidenceValidity = nil })
	fakes.read.view = history.MessageView{
		MessageID: "42", SessionID: "s-1", Role: "assistant", Content: "原文",
		EvidenceRefs: []core.EvidenceRef{{ID: "doc-1", VersionID: "v1"}},
	}

	requireKnowledgeError(t, historyGet(srv, "/api/v1/sessions/s-1/messages/42"),
		http.StatusInternalServerError, codeInternal, messageInternal)
}

// TestHistory_ReadMessageMissingPortFailsClosed 判缺精确读取端口是 500 而不是空视图。
// 逆改点：nil 端口时返回零值视图 → 200 断言红。
func TestHistory_ReadMessageMissingPortFailsClosed(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, func(o *Options) { o.HistoryExactRead = nil })
	requireKnowledgeError(t, historyGet(srv, "/api/v1/sessions/s-1/messages/42"),
		http.StatusInternalServerError, codeInternal, messageInternal)
}

// TestSessions_QueryListForcedCallerFilter 判会话列表的归属强制过滤：调用方 Key
// 只拿到自己的行，过滤器携带自己的 caller_id，且响应里没有他人的会话。
// 逆改点：漏填 `filter.CallerID` → 三条行全返回（越权）且过滤器断言红。
func TestSessions_QueryListForcedCallerFilter(t *testing.T) {
	t.Parallel()
	store := newRecordingSessionStore()
	store.seed(core.SessionRecord{
		ID: "s-mine", ProfileName: "demo", Channel: "web", Status: statusActive,
		CallerID: "cal_1", CreatedAt: sessionsQueryBase, LastActiveAt: sessionsQueryBase,
	})
	store.seed(core.SessionRecord{
		ID: "s-alien", ProfileName: "demo", Channel: "web", Status: statusActive,
		CallerID: "cal_2", CreatedAt: sessionsQueryBase, LastActiveAt: sessionsQueryBase,
	})
	store.seed(core.SessionRecord{
		ID: "s-legacy", ProfileName: "demo", Channel: "web", Status: statusActive,
		CreatedAt: sessionsQueryBase, LastActiveAt: sessionsQueryBase,
	})
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"hs-secret": callerPrincipal("cal_1", core.ScopeSessionsRead),
	}}
	srv := newTestServer(t, func(o *Options) {
		o.Sessions = store
		o.AdminAuth = businessGuard(t, keys)
	})

	rec := serveAdmin(srv, withBearer(
		adminRequest(http.MethodGet, "/api/v1/sessions", ""), "hs-secret"))
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourcesEnvelope[[]SessionSummaryView](t, rec)
	require.Len(t, env.Data, 1, "调用方只能看到自己的行（他人与空归属都不可见）")
	require.Equal(t, "s-mine", env.Data[0].SessionID)
	require.Equal(t, []core.SessionFilter{{CallerID: "cal_1"}}, store.recordedFilters(),
		"归属过滤必须逐字进入仓储过滤器")
}

// TestSessions_QueryListAdminSpansCallers 判内置管理员跨调用方：过滤器不带 caller_id，
// 迁移遗留的空归属行也只对管理员可见。
// 逆改点：给管理员也填 CallerID → 只剩一行且过滤器断言红。
func TestSessions_QueryListAdminSpansCallers(t *testing.T) {
	t.Parallel()
	store := newRecordingSessionStore()
	store.seed(core.SessionRecord{
		ID: "s-mine", ProfileName: "demo", Status: statusActive, CallerID: "cal_1",
		CreatedAt: sessionsQueryBase, LastActiveAt: sessionsQueryBase,
	})
	store.seed(core.SessionRecord{
		ID: "s-alien", ProfileName: "demo", Status: statusActive, CallerID: "cal_2",
		CreatedAt: sessionsQueryBase, LastActiveAt: sessionsQueryBase,
	})
	store.seed(core.SessionRecord{
		ID: "s-legacy", ProfileName: "demo", Status: statusActive,
		CreatedAt: sessionsQueryBase, LastActiveAt: sessionsQueryBase,
	})
	srv := newTestServer(t, func(o *Options) {
		o.Sessions = store
		o.AdminAuth = businessGuard(t, &fakeCallerKeys{})
	})

	rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/sessions", ""), testToken))
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourcesEnvelope[[]SessionSummaryView](t, rec)
	require.Len(t, env.Data, 3, "管理员跨调用方，空归属行同样可见")
	require.Equal(t, []core.SessionFilter{{}}, store.recordedFilters(),
		"管理员不得被加上归属过滤")
}

// TestSessions_QueryListOwnerlessCallerDenied 判没有归属的调用方身份被拒：无法收窄
// 就退化成全量，那是越权而不是"没过滤"。该分支是纵深防御（真实闸门在认证期就会
// 拒绝空 CallerID 的身份），因此直接调 handler 而不经路由。
// 逆改点：把 `CallerID == ""` 当"不过滤"放行 → 200 且仓储被调用，两条断言全红。
func TestSessions_QueryListOwnerlessCallerDenied(t *testing.T) {
	t.Parallel()
	store := newRecordingSessionStore()
	srv := newTestServer(t, func(o *Options) { o.Sessions = store })

	rec := httptest.NewRecorder()
	srv.handleListSessions(rec, adminRequest(http.MethodGet, "/api/v1/sessions", ""),
		core.Principal{
			Kind: core.PrincipalCaller, Actor: core.ActorCaller,
			Scopes: []string{core.ScopeSessionsRead},
		})

	requireKnowledgeError(t, rec, http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
	require.Empty(t, store.recordedFilters(), "无归属身份不得触达仓储")
}

// decodeRawMap 把一段原始 JSON 解码成对象数组，用于断言字段的**存在性**。
// decodeRawMap decodes a raw JSON array of objects so key presence can be asserted.
func decodeRawMap(t *testing.T, raw []byte) []map[string]json.RawMessage {
	t.Helper()
	var out []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

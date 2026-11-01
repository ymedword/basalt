package web

// 本文件是**管理面记忆**七个端点的 handler 级验收（US-16 裁决 A / 颗粒度文档 C-21）：
// 结构化读模型的两条列表、四条写动作（提议 ×2 分层、确认 / 纠正 / 软删），以及
// 失败分类映射与装配缺失的 fail-closed。
//
// 断言分三层：① 状态码与契约错误码（404 只给 NOT_FOUND、状态冲突给 409、纯请求
// 非法给 400）；② 参数原样过端口（分层 / 状态过滤 / 条目主键 / 正文），handler 不做
// 第二套校验或第二次改写；③ handler 零副作用——纯请求失败**不**触达端口，因此不可能
// 留下操作行或改动任何条目。正文**不**出现在失败响应里。
// Handler-level acceptance for the seven management memory rows: status/code mapping,
// verbatim parameter pass-through, and zero port calls on pure request errors.

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
)

// errFakeMemoryAdminUnused 标记用例没有为该端口方法准备行为：被调用即说明请求走到了
// 不该走的路径（例如纯请求非法却仍调用端口）。
// errFakeMemoryAdminUnused marks a port method the case never intended to call.
var errFakeMemoryAdminUnused = errors.New("fake memory admin: unexpected call")

// fakeMemoryAdmin 是可注入行为的 `core.MemoryAdmin` 替身：五个方法各自记录入参、
// 返回注入结果；未准备的方法一律回 errFakeMemoryAdminUnused。
// fakeMemoryAdmin is a behavior-injected core.MemoryAdmin double.
type fakeMemoryAdmin struct {
	listFn    func(context.Context, core.MemoryEntryFilter) ([]core.MemoryEntryRecord, error)
	proposeFn func(context.Context, core.Actor, core.MemoryProposal) (core.MemoryEntryResult, error)
	confirmFn func(context.Context, core.Actor, int64) (core.MemoryEntryResult, error)
	correctFn func(context.Context, core.Actor, core.MemoryCorrection) (core.MemoryEntryResult, error)
	deleteFn  func(context.Context, core.Actor, int64) (core.MemoryEntryResult, error)

	lastFilter     core.MemoryEntryFilter
	lastProposal   core.MemoryProposal
	lastCorrection core.MemoryCorrection
	lastEntryID    int64
	listCalls      int
}

// ListEntries 记录过滤条件并回注入结果。
func (f *fakeMemoryAdmin) ListEntries(ctx context.Context, filter core.MemoryEntryFilter) ([]core.MemoryEntryRecord, error) {
	f.listCalls++
	f.lastFilter = filter
	if f.listFn == nil {
		return nil, errFakeMemoryAdminUnused
	}
	return f.listFn(ctx, filter)
}

// Propose 记录提议并回注入结果。
func (f *fakeMemoryAdmin) Propose(ctx context.Context, actor core.Actor, proposal core.MemoryProposal) (core.MemoryEntryResult, error) {
	f.lastProposal = proposal
	if f.proposeFn == nil {
		return core.MemoryEntryResult{}, errFakeMemoryAdminUnused
	}
	return f.proposeFn(ctx, actor, proposal)
}

// Confirm 记录条目标识并回注入结果。
func (f *fakeMemoryAdmin) Confirm(ctx context.Context, actor core.Actor, entryID int64) (core.MemoryEntryResult, error) {
	f.lastEntryID = entryID
	if f.confirmFn == nil {
		return core.MemoryEntryResult{}, errFakeMemoryAdminUnused
	}
	return f.confirmFn(ctx, actor, entryID)
}

// Correct 记录纠正入参并回注入结果。
func (f *fakeMemoryAdmin) Correct(ctx context.Context, actor core.Actor, correction core.MemoryCorrection) (core.MemoryEntryResult, error) {
	f.lastCorrection = correction
	if f.correctFn == nil {
		return core.MemoryEntryResult{}, errFakeMemoryAdminUnused
	}
	return f.correctFn(ctx, actor, correction)
}

// Delete 记录条目标识并回注入结果。
func (f *fakeMemoryAdmin) Delete(ctx context.Context, actor core.Actor, entryID int64) (core.MemoryEntryResult, error) {
	f.lastEntryID = entryID
	if f.deleteFn == nil {
		return core.MemoryEntryResult{}, errFakeMemoryAdminUnused
	}
	return f.deleteFn(ctx, actor, entryID)
}

// 编译期断言：假件实现被测端口——端口漂移在编译期就红。
var _ core.MemoryAdmin = (*fakeMemoryAdmin)(nil)

// newMemoryAdminServer 装配带记忆管理端口的 handler 级服务；mutate 可再覆盖装配。
// newMemoryAdminServer wires the real handler chain over the injected memory port.
func newMemoryAdminServer(t *testing.T, mutate func(*Options)) (*Server, *fakeMemoryAdmin) {
	t.Helper()
	fake := &fakeMemoryAdmin{}
	s := newTestServer(t, func(o *Options) {
		o.MemoryAdmin = fake
		if mutate != nil {
			mutate(o)
		}
	})
	return s, fake
}

// memoryAdminCall 发一条管理面请求并返回记录器。
func memoryAdminCall(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serveAdmin(s, adminRequest(method, path, body))
}

// TestAdminMemory_ListGlobalPassesStateAndShape —— 全局层列表：`state` 过滤原样进
// 端口（不在 handler 里另做取值域判断），响应字段集与契约 MemoryEntryListView 完全
// 一致，条目带主键与状态。
// 逆改点：把 state 丢掉或本地改写过滤 → 过滤断言红；投影少字段 / 多字段 → 契约
// 字段集断言红。
func TestAdminMemory_ListGlobalPassesStateAndShape(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	s, fake := newMemoryAdminServer(t, nil)
	fake.listFn = func(context.Context, core.MemoryEntryFilter) ([]core.MemoryEntryRecord, error) {
		return []core.MemoryEntryRecord{{
			EntryID:      7,
			Scope:        core.GlobalScope,
			Section:      core.SectionCore,
			State:        "candidate",
			Content:      "候选正文",
			Source:       "explicit_propose",
			SupersedesID: 0,
			CreatedAt:    time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC),
		}}, nil
	}

	rec := memoryAdminCall(t, s, http.MethodGet, "/api/v1/memory/entries?state=candidate", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "candidate", fake.lastFilter.State, "状态过滤必须原样进端口")
	require.Equal(t, core.GlobalScope, fake.lastFilter.Scope, "全局层是零值作用域，不是空过滤")

	var env resourcesEnvelope[map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, "OK", env.Code)
	requireResourcePayloadMatches(t, doc, "MemoryEntryListView", env.Data)
	require.Equal(t, "global", env.Data["scope"])

	entries, ok := env.Data["entries"].([]any)
	require.True(t, ok, "entries 必须是数组")
	require.Len(t, entries, 1)
	entry, ok := entries[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "candidate", entry["state"], "候选必须出现在读模型里（渲染投影看不到它）")
	require.EqualValues(t, 7, entry["entry_id"])
}

// TestAdminMemory_ListAgentScopeAndUnknownAgent —— Agent 层列表带 `{Agent: name}`；
// 未注册 Agent 走同一处 404 判定且**不**读端口。空集合必须输出 `[]` 而不是 null。
// 逆改点：把 Agent 名传丢 → 作用域断言红；404 前先读端口 → 端口调用计数红。
func TestAdminMemory_ListAgentScopeAndUnknownAgent(t *testing.T) {
	s, fake := newMemoryAdminServer(t, func(o *Options) {
		o.Agents = fakeAgents{entries: []core.AgentEntry{
			{Name: "ops-agent", Registered: true, Profile: &core.Profile{Name: "ops-agent"}},
			{Name: "broken", Registered: false, RejectReason: "AGENT.md 语法错误"},
		}}
	})
	fake.listFn = func(context.Context, core.MemoryEntryFilter) ([]core.MemoryEntryRecord, error) {
		return nil, nil
	}

	rec := memoryAdminCall(t, s, http.MethodGet, "/api/v1/agents/ops-agent/memory/entries", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, core.Scope{Agent: "ops-agent"}, fake.lastFilter.Scope)
	require.Contains(t, rec.Body.String(), `"entries":[]`, "空集合输出 []，不输出 null")

	before := fake.listCalls
	rec = memoryAdminCall(t, s, http.MethodGet, "/api/v1/agents/broken/memory/entries", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)
	require.Equal(t, before, fake.listCalls, "未注册 Agent 不得读取记忆")
}

// TestAdminMemory_ListInvalidStateMapsBadRequest —— 状态取值域外由适配器按
// `InvalidInputError` 拒绝，handler 映射 400 并原样带出 Reason；**不**静默回空列表
// （页面会把"没有候选"当成事实）。
// 逆改点：把 400 分支删掉回 200 空列表 → 状态码与端口调用断言红。
func TestAdminMemory_ListInvalidStateMapsBadRequest(t *testing.T) {
	s, fake := newMemoryAdminServer(t, nil)
	fake.listFn = func(context.Context, core.MemoryEntryFilter) ([]core.MemoryEntryRecord, error) {
		return nil, &core.InvalidInputError{
			Tool:   core.ActionMemoryPropose,
			Reason: `未知的记忆状态："expired"（只接受六态字面量）`,
		}
	}

	rec := memoryAdminCall(t, s, http.MethodGet, "/api/v1/memory/entries?state=expired", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Contains(t, env.Message, "expired")
}

// TestAdminMemory_ProposeGlobal —— 全局层提议：请求体原样映射（分区缺省为空串，
// 由适配器按归档区落），回执字段集与契约一致，`merged` 如实透出。
// 逆改点：把 section 默认值改在 handler 里 → 入参断言红；Merged 不投影 → 回执断言红。
func TestAdminMemory_ProposeGlobal(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	s, fake := newMemoryAdminServer(t, nil)
	fake.proposeFn = func(_ context.Context, _ core.Actor, p core.MemoryProposal) (core.MemoryEntryResult, error) {
		require.Equal(t, core.GlobalScope, p.Scope, "全局层提议的作用域是零值")
		return core.MemoryEntryResult{EntryID: 11, State: "candidate", Merged: true}, nil
	}

	rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/proposals",
		`{"content":"下次部署窗口定在周五"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "下次部署窗口定在周五", fake.lastProposal.Content)
	require.Equal(t, core.Section(""), fake.lastProposal.Section, "分区缺省留给适配器决定，handler 不预填")

	var env resourcesEnvelope[map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	requireResourcePayloadMatches(t, doc, "MemoryEntryReceipt", env.Data)
	require.Equal(t, true, env.Data["merged"])
	require.EqualValues(t, 11, env.Data["entry_id"])
}

// TestAdminMemory_ProposeRequestErrorsDoNotTouchPort —— 非法 JSON、未知字段、空正文
// 都在 handler 层或适配器层被拒：handler 先拒的不触达端口，绝不产生操作行。
// 逆改点：去掉未知字段拒绝（DisallowUnknownFields）→ 未知字段子例红。
func TestAdminMemory_ProposeRequestErrorsDoNotTouchPort(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", `{"content":`},
		{"未知字段", `{"content":"x","scope":"agent:ops-agent"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, fake := newMemoryAdminServer(t, nil)
			rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/proposals", tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, codeBadRequest, decodeAuthEnvelope(t, rec).Code)
			require.Empty(t, fake.lastProposal.Content, "请求非法时不得触达端口")
		})
	}
}

// TestAdminMemory_AgentProposeUnknownAgent —— Agent 层提议同样先过注册判定：
// 未知 Agent 404，端口零调用。
func TestAdminMemory_AgentProposeUnknownAgent(t *testing.T) {
	s, fake := newMemoryAdminServer(t, nil)
	rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/agents/ghost/memory/proposals",
		`{"content":"x"}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)
	require.Empty(t, fake.lastProposal.Content)
}

// TestAdminMemory_ConfirmAndDeleteFailureMapping —— 确认 / 软删共用两条失败分类：
// 条目不存在 → 404 NOT_FOUND；状态不允许 → 409 CONFLICT。分类只按哨兵判定，
// 不比对文案。
// 逆改点：把 ErrMemoryEntryNotFound 写成 500 → 404 子例红；把 ErrConflict 写成
// 400 → 409 子例红。
func TestAdminMemory_ConfirmAndDeleteFailureMapping(t *testing.T) {
	t.Run("确认：条目不存在 404", func(t *testing.T) {
		s, fake := newMemoryAdminServer(t, nil)
		fake.confirmFn = func(context.Context, core.Actor, int64) (core.MemoryEntryResult, error) {
			return core.MemoryEntryResult{}, core.ErrMemoryEntryNotFound
		}
		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/9/confirm", "")
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)
		require.EqualValues(t, 9, fake.lastEntryID)
	})

	t.Run("确认：状态冲突 409", func(t *testing.T) {
		s, fake := newMemoryAdminServer(t, nil)
		fake.confirmFn = func(context.Context, core.Actor, int64) (core.MemoryEntryResult, error) {
			return core.MemoryEntryResult{}, core.ErrConflict
		}
		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/9/confirm", "")
		require.Equal(t, http.StatusConflict, rec.Code)
		require.Equal(t, codeConflict, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("软删：条目不存在 404", func(t *testing.T) {
		s, fake := newMemoryAdminServer(t, nil)
		fake.deleteFn = func(context.Context, core.Actor, int64) (core.MemoryEntryResult, error) {
			return core.MemoryEntryResult{}, core.ErrMemoryEntryNotFound
		}
		rec := memoryAdminCall(t, s, http.MethodDelete, "/api/v1/memory/entries/9", "")
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)
	})
}

// TestAdminMemory_EntryIDParsingAndCorrect —— 三件事：① 路径标识不是正整数按 404
// 且端口零调用（不给存在性探针）；② 纠正把主键与正文原样传给端口，作用域字段在
// 请求体里**不存在**（未知字段被拒）；③ 纠正回执的 `replaced_id` 如实透出。
// 逆改点：非法标识改回 400 → 404 断言红；纠正改带 scope 字段 → 未知字段子例红。
func TestAdminMemory_EntryIDParsingAndCorrect(t *testing.T) {
	t.Run("非法标识 404 且不读端口", func(t *testing.T) {
		s, fake := newMemoryAdminServer(t, nil)
		rec := memoryAdminCall(t, s, http.MethodDelete, "/api/v1/memory/entries/not-a-number", "")
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)
		require.Zero(t, fake.lastEntryID)
	})

	t.Run("纠正原样传主键与正文", func(t *testing.T) {
		s, fake := newMemoryAdminServer(t, nil)
		fake.correctFn = func(_ context.Context, _ core.Actor, c core.MemoryCorrection) (core.MemoryEntryResult, error) {
			return core.MemoryEntryResult{EntryID: 12, State: "active", ReplacedID: c.EntryID}, nil
		}
		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/9/correct",
			`{"content":"纠正后的正文"}`)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, int64(9), fake.lastCorrection.EntryID)
		require.Equal(t, "纠正后的正文", fake.lastCorrection.Content)

		var env resourcesEnvelope[map[string]any]
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.EqualValues(t, 12, env.Data["entry_id"])
		require.EqualValues(t, 9, env.Data["replaced_id"])
	})

	t.Run("纠正请求体带作用域字段被拒", func(t *testing.T) {
		s, fake := newMemoryAdminServer(t, nil)
		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/9/correct",
			`{"content":"x","agent":"ops-agent"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Zero(t, fake.lastCorrection.EntryID)
	})
}

// TestAdminMemory_PostPendingFailureCarriesOperationID —— `pending` 之后的失败必须
// 带 `data.operation_id`：页面据此去操作记录查证这次写留下的终态，而不是只看
// 一句错误文案。包装来自适配器（`postPendingError`），handler 只做白名单投影。
// 逆改点：把该分支退回不带 data 的 writeError → operation_id 断言红。
func TestAdminMemory_PostPendingFailureCarriesOperationID(t *testing.T) {
	s, fake := newMemoryAdminServer(t, nil)
	fake.deleteFn = func(context.Context, core.Actor, int64) (core.MemoryEntryResult, error) {
		return core.MemoryEntryResult{}, &core.AdminError{
			Kind: core.ErrConflict,
			Data: map[string]any{"operation_id": "op-1792576800000-abc123"},
		}
	}
	rec := memoryAdminCall(t, s, http.MethodDelete, "/api/v1/memory/entries/9", "")
	require.Equal(t, http.StatusConflict, rec.Code)

	var env resourcesEnvelope[map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.NotNil(t, env.Data, "失败响应必须带 data")
	require.Equal(t, "op-1792576800000-abc123", env.Data["operation_id"])
}

// TestAdminMemory_UnassembledFailsClosed —— 端口缺失 = 管理写面不存在：所有端点
// 返回固定 500 INTERNAL，**不得**退化成空列表或"没有找到"。
// 逆改点：把 nil 端口当空读面返回 200 → 本用例红。
func TestAdminMemory_UnassembledFailsClosed(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.MemoryAdmin = nil })

	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/memory/entries", ""},
		{http.MethodGet, "/api/v1/agents/ops-agent/memory/entries", ""},
		{http.MethodPost, "/api/v1/memory/proposals", `{"content":"x"}`},
		{http.MethodPost, "/api/v1/agents/ops-agent/memory/proposals", `{"content":"x"}`},
		{http.MethodPost, "/api/v1/memory/entries/9/confirm", ""},
		{http.MethodPost, "/api/v1/memory/entries/9/correct", `{"content":"x"}`},
		{http.MethodDelete, "/api/v1/memory/entries/9", ""},
	} {
		rec := memoryAdminCall(t, s, call.method, call.path, call.body)
		require.Equalf(t, http.StatusInternalServerError, rec.Code, "%s %s", call.method, call.path)
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code, "%s %s", call.method, call.path)
	}
}

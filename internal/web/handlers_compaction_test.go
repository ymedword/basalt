package web

// 本文件覆盖压缩只读视图端点的 HTTP 层与视图投影（US-16 / T089），判据逐条取自
// 冻结契约 `docs/contracts/openapi.yaml` 的 `GET /compactions` / `CompactionView`：
//
//  1. **scope 恰给一个**：两个都给或都不给 → 400，且零端口调用；
//  2. **limit 边界在端口之前判**：0 / 201 / 非整数 → 400；缺省 50 逐字传下去；
//  3. **缺端口是装配错误**：固定 500，**不得**回 200 + 空数组（那会把装配故障
//     显示成"从未压缩"）；
//  4. **状态与原因码一并透传**：failed / stale 条目带 `reason_code` 出站——失败必须
//     显示降级原因；`source_refs`（审计集）与 `included_refs`（当前依赖集）分列出站；
//  5. **管理面**：契约根 security 没有 callerKey——有效 caller key 必须 403。
//
// 逆改点写在每个用例头上。视图字段集与枚举由 `TestOpenAPI_CompactionViewMatchesContract`
// 与契约逐字对账。
// This file pins the compaction read surface: exactly-one scope, limit bounds before the
// port, fail-closed missing port, status/reason passthrough and the management face.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// fakeCompactionChain 是可编程的压缩链端口：记录入参并返回预置链。
// fakeCompactionChain is a programmable compaction port recording its arguments.
type fakeCompactionChain struct {
	entries []compaction.CompactionEntry
	err     error

	calls    int
	gotScope compaction.Scope
	gotLimit int
}

// ListCurrentChain 记录 scope 与 limit 后返回预置链。
// ListCurrentChain records its arguments, then returns the configured chain.
func (f *fakeCompactionChain) ListCurrentChain(
	_ context.Context, scope compaction.Scope, limit int,
) ([]compaction.CompactionEntry, error) {
	f.calls++
	f.gotScope, f.gotLimit = scope, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.entries, nil
}

// newCompactionServer 装配带压缩端口的 handler 级服务。
// newCompactionServer wires the compaction port into a handler-level server.
func newCompactionServer(t *testing.T, mutate func(*Options)) (*Server, *fakeCompactionChain) {
	t.Helper()
	port := &fakeCompactionChain{}
	srv := newTestServer(t, func(o *Options) {
		o.CompactionChain = port
		if mutate != nil {
			mutate(o)
		}
	})
	return srv, port
}

// compactionGet 发起一次管理凭据的 GET。
// compactionGet issues one GET with the admin token.
func compactionGet(s *Server, path string) *httptest.ResponseRecorder {
	return serveAdmin(s, adminRequest(http.MethodGet, path, ""))
}

// TestCompactions_RequiresExactlyOneScope 判"恰给一个"：都不给、都给都是 400，
// 且形状非法时**不触达端口**（`calls == 0`）。
// 逆改点：把任一分支放过 → 对应 400 断言红；先把请求交给端口 → calls 非 0。
func TestCompactions_RequiresExactlyOneScope(t *testing.T) {
	t.Parallel()
	srv, port := newCompactionServer(t, nil)

	for _, path := range []string{
		"/api/v1/compactions",
		"/api/v1/compactions?session_id=s-1&run_id=run-1",
	} {
		rec := compactionGet(srv, path)
		require.Equal(t, http.StatusBadRequest, rec.Code, path)
		require.Equal(t, codeBadRequest, decodeAuthEnvelope(t, rec).Code)
	}
	require.Zero(t, port.calls, "形状非法时不得读取仓储")
}

// TestCompactions_LimitBoundsBeforePort 判契约边界（minimum 1 / maximum 200 /
// default 50）：越界与非整数一律 400 且零端口调用；缺省值与显式边界逐字传下去。
// 逆改点：去掉任一上 / 下界 → 对应 400 变 200；缺省值写错 → gotLimit 断言红。
func TestCompactions_LimitBoundsBeforePort(t *testing.T) {
	t.Parallel()
	srv, port := newCompactionServer(t, nil)

	for _, limit := range []string{"0", "201", "-1", "abc", "1.5", ""} {
		if limit == "" {
			continue
		}
		rec := compactionGet(srv, "/api/v1/compactions?session_id=s-1&limit="+limit)
		require.Equal(t, http.StatusBadRequest, rec.Code, "limit="+limit)
		require.Equal(t, codeBadRequest, decodeAuthEnvelope(t, rec).Code)
	}
	require.Zero(t, port.calls, "越界 limit 不得读取仓储")

	require.Equal(t, http.StatusOK, compactionGet(srv, "/api/v1/compactions?session_id=s-1").Code)
	require.Equal(t, defaultCompactionChainLimit, port.gotLimit, "缺省 limit 必须是契约默认值 50")
	require.Equal(t, 50, port.gotLimit)

	require.Equal(t, http.StatusOK, compactionGet(srv, "/api/v1/compactions?run_id=run-1&limit=200").Code)
	require.Equal(t, 200, port.gotLimit, "上界 200 必须放行")
	require.Equal(t, compaction.Scope{RunID: "run-1"}, port.gotScope, "run 范围只带 run 标识")
}

// TestCompactions_MissingPortFailsClosed 判装配缺失的失败关闭：固定 500 且响应里
// **没有** `"data":[]`——空数组会把装配故障显示成"从未压缩"。
// 逆改点：nil 端口回 200 + 空数组 → 状态与 body 断言双红。
func TestCompactions_MissingPortFailsClosed(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t, func(o *Options) { o.CompactionChain = nil })

	rec := compactionGet(srv, "/api/v1/compactions?session_id=s-1")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	require.NotContains(t, rec.Body.String(), `"data":[]`, "缺端口不得冒充空链")
}

// TestCompactions_StoreErrorFailsClosed 判仓储错误 → 500 固定文案（不泄漏内部错误）。
// 逆改点：把错误吞成 200 + 空数组 → 状态断言红。
func TestCompactions_StoreErrorFailsClosed(t *testing.T) {
	t.Parallel()
	srv, port := newCompactionServer(t, nil)
	port.err = errors.New("连接池耗尽")

	rec := compactionGet(srv, "/api/v1/compactions?session_id=s-1")
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	require.NotContains(t, rec.Body.String(), "连接池耗尽", "内部错误不得回显")
}

// TestCompactions_ProjectsCurrentChain 判投影逐字段：根条目父指针为空串、未失效
// `stale_at` 为空串、失败条目带原因码、失效条目带失效时间、两个引用集合分列、
// 空引用集合恒为 `[]`；scope 两个方向（会话 / 无状态 Run）都原样透传。
// 逆改点：把 `source_refs` 与 `included_refs` 合并 → 两集合断言红；漏投影
// `reason_code` → 失败条目断言红；空集合写成 null → `[]` 断言红。
func TestCompactions_ProjectsCurrentChain(t *testing.T) {
	t.Parallel()
	srv, port := newCompactionServer(t, nil)

	created := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	staleAt := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	port.entries = []compaction.CompactionEntry{
		{
			CompactionID: "cp-1", Scope: compaction.Scope{SessionID: "s-1"}, ChainEpoch: 2,
			TriggerRunID: "run-9", AttemptID: "att-1",
			CoveredFromMessageID: "1", CutMessageID: "40", FirstKeptMessageID: "41",
			StrategyVersion: "tail-v1", PromptVersion: "summary-v1", Model: "mock-model",
			TokensBefore: 12000, TokensAfter: 3000,
			Status:     compaction.StatusCommitted,
			SourceRefs: []core.EvidenceRef{{Kind: "knowledge", ID: "doc-1", VersionID: "v1", ContentHash: "sha256:a"}},
			IncludedRefs: []core.EvidenceRef{
				{Kind: "knowledge", ID: "doc-1", VersionID: "v1", ContentHash: "sha256:a"},
				{Kind: "message", ID: "doc-2", VersionID: "v3"},
			},
			Summary:   "巡检结论摘要",
			CreatedAt: created,
		},
		{
			CompactionID: "cp-2", Scope: compaction.Scope{SessionID: "s-1"}, ChainEpoch: 2,
			ParentCompactionID:   "cp-1",
			CoveredFromMessageID: "41", CutMessageID: "60", FirstKeptMessageID: "61",
			Status: compaction.StatusFailed, ReasonCode: "compaction_timeout",
			CreatedAt: created.Add(time.Minute),
		},
		{
			CompactionID: "cp-3", Scope: compaction.Scope{SessionID: "s-1"}, ChainEpoch: 2,
			ParentCompactionID:   "cp-2",
			CoveredFromMessageID: "61", CutMessageID: "80", FirstKeptMessageID: "81",
			Status: compaction.StatusStale, ReasonCode: "revoked",
			CreatedAt: created.Add(2 * time.Minute), StaleAt: staleAt,
		},
	}

	rec := compactionGet(srv, "/api/v1/compactions?session_id=s-1")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, compaction.Scope{SessionID: "s-1"}, port.gotScope)

	env := decodeKnowledge[[]CompactionView](t, rec)
	require.Len(t, env.Data, 3, "存储层给的次序必须原样透传（本层不重排）")

	root := env.Data[0]
	require.Equal(t, "cp-1", root.CompactionID)
	require.Equal(t, "s-1", root.ScopeSessionID)
	require.Empty(t, root.ScopeRunID, "会话范围不得带上 Run 范围字段")
	require.Equal(t, 2, root.ChainEpoch)
	require.Empty(t, root.ParentCompactionID, "根条目的父指针必须是空串")
	require.Equal(t, "run-9", root.TriggerRunID, "触发 Run 只是注记，原样出站")
	require.Equal(t, "1", root.CoveredFromMessageID)
	require.Equal(t, "40", root.CutMessageID)
	require.Equal(t, "41", root.FirstKeptMessageID)
	require.Equal(t, "tail-v1", root.StrategyVersion)
	require.Equal(t, "summary-v1", root.PromptVersion)
	require.Equal(t, "mock-model", root.Model)
	require.Equal(t, 12000, root.TokensBefore)
	require.Equal(t, 3000, root.TokensAfter)
	require.Equal(t, compaction.StatusCommitted, root.Status)
	require.Empty(t, root.ReasonCode)
	require.Equal(t, "2026-10-28T09:30:00Z", root.CreatedAt)
	require.Empty(t, root.StaleAt, "未失效的条目 stale_at 必须是空串")
	require.Equal(t, CompactionSummaryView{
		CompactionID: "cp-1", Summary: "巡检结论摘要", Truncated: false,
	}, root.SummaryView)
	require.Len(t, root.SourceRefs, 1)
	require.Len(t, root.IncludedRefs, 2, "两个引用集合不得合并")
	require.Equal(t, EvidenceRefView{ID: "doc-1", VersionID: "v1", ContentHash: "sha256:a"},
		root.IncludedRefs[0])

	failed := env.Data[1]
	require.Equal(t, compaction.StatusFailed, failed.Status)
	require.Equal(t, "compaction_timeout", failed.ReasonCode, "失败必须显示降级原因")
	require.Equal(t, []EvidenceRefView{}, failed.SourceRefs, "空引用集合必须是 []")
	require.Equal(t, []EvidenceRefView{}, failed.IncludedRefs)
	require.Empty(t, failed.StaleAt)

	stale := env.Data[2]
	require.Equal(t, compaction.StatusStale, stale.Status)
	require.Equal(t, "revoked", stale.ReasonCode)
	require.Equal(t, "2026-10-28T10:00:00Z", stale.StaleAt)

	// 原始 JSON 键：空引用集合出站为 []（不是 null），且摘要子视图恒在。
	// 结构体零值区分不出"出了 null"与"出了 []"，只有原始字节能判。
	var raw struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw.Data, 3)
	require.JSONEq(t, `[]`, string(raw.Data[1]["source_refs"]), "空审计集必须是 []")
	require.JSONEq(t, `[]`, string(raw.Data[1]["included_refs"]), "空当前依赖集必须是 []")
	_, ok := raw.Data[1]["summary_view"]
	require.True(t, ok, "摘要子视图必须恒出站")
}

// TestCompactions_EmptyChainIsExplicit 判"确实没有压缩记录"是 `data: []` 而不是
// null——与缺端口的 500 是两个不同事实，形状上必须能区分。
// 逆改点：空链写成 null → `"data":[]` 断言红。
func TestCompactions_EmptyChainIsExplicit(t *testing.T) {
	t.Parallel()
	srv, _ := newCompactionServer(t, nil)

	rec := compactionGet(srv, "/api/v1/compactions?run_id=run-1")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"data":[]`, "空链必须是 []")
}

// TestCompactions_ManagementSurfaceRejectsCallerKey 判管理面：契约根 security 只有
// bearerAuth / localTrust，没有 callerKey——有效 caller key 必须被拒（403），
// 不得因为"能读自己的会话"就顺手读到压缩链。
// 逆改点：把该行改成业务面（surface + scope + ingress）→ 本用例变 200/500 而不是 403。
func TestCompactions_ManagementSurfaceRejectsCallerKey(t *testing.T) {
	t.Parallel()
	keys := &fakeCallerKeys{byCredential: map[string]core.Principal{
		"c1-secret": callerPrincipal("c1", core.ScopeSessionsRead),
	}}
	guard := businessGuard(t, keys)
	srv, port := newCompactionServer(t, func(o *Options) { o.AdminAuth = guard })

	req := withBearer(adminRequest(http.MethodGet, "/api/v1/compactions?session_id=s-1", ""), "c1-secret")
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusForbidden, rec.Code, "管理端点必须拒绝 caller key")
	require.Equal(t, codeForbidden, decodeAuthEnvelope(t, rec).Code)
	require.Zero(t, port.calls, "被拒的请求不得读取仓储")
}

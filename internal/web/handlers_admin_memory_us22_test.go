package web

// 本文件是 US-22「受控自我进化」在 web 层的四个 handler 级验收点（颗粒度文档 T015 /
// FR-22-11）：可选确认 body、统一回执追加 `superseded_id`、操作明细只追加
// `resolution` / `supersedes_id`、候选元数据随条目返回。
//
// 三条边界在这里守：
//  1. **空 body 只决定处置**：空 body 等价 keep_both，但**不**改走旧 Confirm——装了
//     解析器就必须经过它；处置字段给了而装配无法兑现时按 503 fail-closed。
//  2. **未装配 resolver 时自动候选 503 不降级**：自动候选的判定在适配器（它才知道
//     来源与元数据），web 只把 `core.ErrMemoryConfirmUnavailable` 映射成
//     `NOT_CONFIGURED`；旧 Confirm 的成功路径对显式条目逐字不变。
//  3. **操作明细是闭集**：只追加两个键，其余记忆明细（正文摘要、来源、状态前后）
//     照旧不出站。
// The four web-level acceptance points of T015: optional body, superseded receipt,
// additive operation-detail keys, and candidate metadata on the read model.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeMemoryAdminResolver 在 fakeMemoryAdmin 之上补 `core.MemoryConfirmResolver`。
// 单独成类型而不是给 fakeMemoryAdmin 加方法：旧 fake 必须保持"未装配 resolver"的
// 形态，US-16 的旧路径用例才仍然在测旧路径。
// The resolver-capable fake is a separate type so the legacy fake stays non-resolver.
type fakeMemoryAdminResolver struct {
	*fakeMemoryAdmin
	confirmWithResolutionFn func(context.Context, core.Actor, core.MemoryConfirmRequest) (core.MemoryEntryResult, error)
	lastConfirmRequest      core.MemoryConfirmRequest
	resolverCalls           int
}

var (
	_ core.MemoryAdmin           = (*fakeMemoryAdminResolver)(nil)
	_ core.MemoryConfirmResolver = (*fakeMemoryAdminResolver)(nil)
)

// ConfirmWithResolution 记录入参并回注入结果；未注入行为说明请求走到了不该走的路。
func (f *fakeMemoryAdminResolver) ConfirmWithResolution(ctx context.Context, actor core.Actor,
	request core.MemoryConfirmRequest,
) (core.MemoryEntryResult, error) {
	f.resolverCalls++
	f.lastConfirmRequest = request
	if f.confirmWithResolutionFn == nil {
		return core.MemoryEntryResult{}, errFakeMemoryAdminUnused
	}
	return f.confirmWithResolutionFn(ctx, actor, request)
}

// newMemoryResolverServer 装配带候选确认解析器的管理面服务。
func newMemoryResolverServer(t *testing.T, mutate func(*Options)) (*Server, *fakeMemoryAdminResolver) {
	t.Helper()
	fake := &fakeMemoryAdminResolver{fakeMemoryAdmin: &fakeMemoryAdmin{}}
	s := newTestServer(t, func(o *Options) {
		o.MemoryAdmin = fake
		if mutate != nil {
			mutate(o)
		}
	})
	return s, fake
}

// TestUS22_Confirm_BodyOptional —— 确认请求体可省略：空 body 走解析器且处置为零值；
// 未装配解析器时显式条目保持旧 Confirm；自动候选（适配器回
// `ErrMemoryConfirmUnavailable`）一律 503，**不得**降级；带了处置字段却无法兑现同样
// 503 且不触达端口；非法 body 400 且零端口调用。
// 逆改点：空 body 改走旧 Confirm（跳过解析器）→ 首子例红；503 分支出成 500 →
// 自动候选子例红；把处置字段静默丢掉只调旧 Confirm → 无法兑现子例红。
func TestUS22_Confirm_BodyOptional(t *testing.T) {
	t.Run("空 body 走统一解析器且处置取零值", func(t *testing.T) {
		s, fake := newMemoryResolverServer(t, nil)
		fake.confirmWithResolutionFn = func(context.Context, core.Actor,
			core.MemoryConfirmRequest,
		) (core.MemoryEntryResult, error) {
			return core.MemoryEntryResult{EntryID: 21, State: "active"}, nil
		}

		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/21/confirm", "")

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, 1, fake.resolverCalls, "空 body 也必须经过统一确认事务")
		require.Equal(t, core.MemoryConfirmRequest{EntryID: 21}, fake.lastConfirmRequest,
			"空 body 等价 keep_both：处置与替代目标都是零值")
		require.Zero(t, fake.lastEntryID, "不得同时调用旧 Confirm")
	})

	t.Run("显式条目在未装配解析器时保持旧 Confirm", func(t *testing.T) {
		s, fake := newMemoryAdminServer(t, nil)
		fake.confirmFn = func(context.Context, core.Actor, int64) (core.MemoryEntryResult, error) {
			return core.MemoryEntryResult{EntryID: 5, State: "active"}, nil
		}

		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/5/confirm", "")

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.EqualValues(t, 5, fake.lastEntryID, "显式条目仍走既有确认路径")
	})

	t.Run("未装配解析器时自动候选 503 不降级", func(t *testing.T) {
		s, fake := newMemoryAdminServer(t, nil)
		fake.confirmFn = func(context.Context, core.Actor, int64) (core.MemoryEntryResult, error) {
			return core.MemoryEntryResult{}, core.ErrMemoryConfirmUnavailable
		}

		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/9/confirm", "")

		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeNotConfigured, env.Code)
		require.Contains(t, env.Message, "未装配")
		require.EqualValues(t, 9, fake.lastEntryID,
			"自动候选的判定由适配器完成：它必须被问到，但不许晋升")
	})

	t.Run("未装配解析器时处置字段无法兑现 503 且不触达端口", func(t *testing.T) {
		s, fake := newMemoryAdminServer(t, nil)

		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/9/confirm",
			`{"resolution":"supersede","supersedes_entry_id":4}`)

		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		require.Equal(t, codeNotConfigured, decodeAuthEnvelope(t, rec).Code)
		require.Zero(t, fake.lastEntryID, "无法兑现的处置不得落到旧 Confirm 上")
	})

	t.Run("非法 body 400 且零端口调用", func(t *testing.T) {
		cases := []struct {
			name string
			body string
		}{
			{"非法 JSON", `{"resolution":`},
			{"未知字段", `{"resolution":"keep_both","entry_id":3}`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s, fake := newMemoryResolverServer(t, nil)
				rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/9/confirm", tc.body)
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				require.Equal(t, codeBadRequest, decodeAuthEnvelope(t, rec).Code)
				require.Zero(t, fake.resolverCalls, "请求体非法时不得触达端口")
			})
		}
	})
}

// TestUS22_Confirm_ResolutionReceipt —— 处置原样进统一事务，回执追加 `superseded_id`：
// 替代给被退役的旧条目主键，保留双方给 0；两种形态的字段集都与契约
// MemoryEntryReceipt 完全一致。
// 逆改点：不投影 SupersededID → 替代子例红；把缺失的 superseded_id 用 omitempty
// 省掉 → keep_both 子例的字段集断言红。
func TestUS22_Confirm_ResolutionReceipt(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	decodeReceipt := func(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var env resourcesEnvelope[map[string]any]
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		requireResourcePayloadMatches(t, doc, "MemoryEntryReceipt", env.Data)
		return env.Data
	}

	t.Run("替代处置", func(t *testing.T) {
		s, fake := newMemoryResolverServer(t, nil)
		fake.confirmWithResolutionFn = func(_ context.Context, _ core.Actor,
			request core.MemoryConfirmRequest,
		) (core.MemoryEntryResult, error) {
			require.Equal(t, "supersede", request.Resolution)
			require.EqualValues(t, 9, request.SupersedesEntryID)
			return core.MemoryEntryResult{EntryID: 21, State: "active", SupersededID: 9}, nil
		}

		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/21/confirm",
			`{"resolution":"supersede","supersedes_entry_id":9}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		data := decodeReceipt(t, rec)
		require.EqualValues(t, 21, data["entry_id"], "entry_id 指向被晋升的候选")
		require.EqualValues(t, 9, data["superseded_id"], "superseded_id 指向被退役的旧条目")
	})

	t.Run("保留双方", func(t *testing.T) {
		s, fake := newMemoryResolverServer(t, nil)
		fake.confirmWithResolutionFn = func(_ context.Context, _ core.Actor,
			request core.MemoryConfirmRequest,
		) (core.MemoryEntryResult, error) {
			require.Equal(t, "keep_both", request.Resolution)
			require.Zero(t, request.SupersedesEntryID)
			return core.MemoryEntryResult{EntryID: 21, State: "active"}, nil
		}

		rec := memoryAdminCall(t, s, http.MethodPost, "/api/v1/memory/entries/21/confirm",
			`{"resolution":"keep_both"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		data := decodeReceipt(t, rec)
		require.EqualValues(t, 0, data["superseded_id"], "没有替代时明确输出 0，不用缺键表达")
	})
}

// TestUS22_OperationDetailResolution —— 记忆确认的操作明细**只追加**两个键：
// `resolution`（闭集）与 `supersedes_id`（非负整数）。其余记忆明细键（正文摘要、
// 状态前后、来源）保持不投影；越界字面量与负数整键丢弃。
// 逆改点：把整个 detail 出站 → 泄漏断言红；白名单接受任意 resolution → 越界子例红。
func TestUS22_OperationDetailResolution(t *testing.T) {
	newConfirmServer := func(t *testing.T, detail string) *Server {
		t.Helper()
		return newOperationServer(t, &operationFake{detail: core.OperationRecord{
			OperationID: "op-confirm",
			Action:      core.ActionMemoryConfirm,
			TargetKind:  core.TargetKindMemory,
			TargetID:    "agent:ops-agent",
			Status:      core.OperationSuccess,
			DetailJSON:  []byte(detail),
		}})
	}
	getDetail := func(t *testing.T, srv *Server) map[string]any {
		t.Helper()
		rec := serveAdmin(srv, adminRequest(http.MethodGet, "/api/v1/operations/op-confirm", ""))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return decodeRunEnvelope[OperationDetailView](t, rec).Data.Detail
	}

	t.Run("替代处置的两个键照常投影", func(t *testing.T) {
		detail := getDetail(t, newConfirmServer(t, `{
			"entry_id": 21,
			"section": "archive",
			"agent": "ops-agent",
			"state_before": "candidate",
			"state_after": "active",
			"content_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"resolution": "supersede",
			"supersedes_id": 9,
			"evidence_json": "[[{\"quote\":\"季度报表在每月 5 号前提交\"}]]",
			"origin_key_id": "key-secret"
		}`))

		require.Equal(t, "supersede", detail["resolution"])
		require.Equal(t, float64(9), detail["supersedes_id"])
		require.Len(t, detail, 2, "只追加两个键：其余记忆明细照旧不投影")
		for _, key := range []string{
			"entry_id", "state_before", "state_after", "content_sha256",
			"evidence_json", "origin_key_id",
		} {
			require.NotContains(t, detail, key, "未列入白名单的键必须被丢弃")
		}
	})

	t.Run("保留双方只加 resolution", func(t *testing.T) {
		detail := getDetail(t, newConfirmServer(t,
			`{"entry_id":21,"state_before":"candidate","resolution":"keep_both"}`))
		require.Equal(t, map[string]any{"resolution": "keep_both"}, detail)
	})

	t.Run("旧操作行没有追加键时照常渲染", func(t *testing.T) {
		detail := getDetail(t, newConfirmServer(t,
			`{"entry_id":12,"state_before":"candidate","state_after":"active"}`))
		require.Empty(t, detail, "旧确认行没有 resolution / supersedes_id，投影为空对象")
	})

	t.Run("越界字面量与负数整键丢弃", func(t *testing.T) {
		detail := getDetail(t, newConfirmServer(t,
			`{"resolution":"purge","supersedes_id":-1}`))
		require.Empty(t, detail)
	})
}

// TestUS22_MemoryEntry_CandidateMeta —— 自动候选的元数据随条目返回：字段集与契约
// MemoryCandidateView / MemoryEvidenceView 完全一致，证据逐字带上消息主键、角色、
// 引文与时刻；非自动候选输出 `null`（键恒在）；空证据与空来源输出 `[]`。
// 来源调用方与 Key 不投影。
// 逆改点：漏投影 evidence → 证据断言红；把 candidate 用 omitempty 省掉 →
// 非候选子例的键集断言红；投影 origin_key_id → 泄漏断言红。
func TestUS22_MemoryEntry_CandidateMeta(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	evidenceAt := time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)
	validTo := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)

	s, fake := newMemoryAdminServer(t, nil)
	fake.listFn = func(context.Context, core.MemoryEntryFilter) ([]core.MemoryEntryRecord, error) {
		return []core.MemoryEntryRecord{
			{
				EntryID:   41,
				Scope:     core.Scope{Agent: "ops-agent"},
				Section:   core.SectionArchive,
				State:     "candidate",
				Content:   "季度报表在每月 5 号前提交",
				Source:    "auto_extract",
				CreatedAt: evidenceAt,
				Candidate: &core.MemoryCandidateMeta{
					EntryID:      41,
					ExtractionID: "ext-20261105-0001",
					Kind:         "fact",
					Certainty:    "asserted",
					Confidence:   0.82,
					Evidence: []core.LearningEvidence{{
						MessageID: 101,
						RunID:     "run-1",
						Role:      "user",
						Quote:     "季度报表在每月 5 号前提交",
						MessageAt: evidenceAt,
					}},
					SourceEntryIDs:        []int64{7, 8},
					SuggestedSupersedesID: 12,
					OriginCallerID:        "caller-local",
					OriginKeyID:           "key-1",
					EvidenceAt:            evidenceAt,
					ValidTo:               validTo,
					TimeConfidence:        "exact",
				},
			},
			{
				EntryID:   42,
				Scope:     core.Scope{Agent: "ops-agent"},
				Section:   core.SectionArchive,
				State:     "candidate",
				Content:   "显式提议的候选",
				Source:    "explicit_propose",
				CreatedAt: evidenceAt,
				Candidate: &core.MemoryCandidateMeta{
					EntryID:        42,
					ExtractionID:   "ext-empty",
					Kind:           "inference",
					Certainty:      "uncertain",
					EvidenceAt:     evidenceAt,
					TimeConfidence: "unknown",
				},
			},
			{
				EntryID:   43,
				Scope:     core.Scope{Agent: "ops-agent"},
				Section:   core.SectionCore,
				State:     "active",
				Content:   "手工条目",
				Source:    "save_memory",
				CreatedAt: evidenceAt,
			},
		}, nil
	}

	rec := memoryAdminCall(t, s, http.MethodGet,
		"/api/v1/agents/ops-agent/memory/entries?state=candidate", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var env resourcesEnvelope[map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, "OK", env.Code)
	requireResourcePayloadMatches(t, doc, "MemoryEntryListView", env.Data)

	entries, ok := env.Data["entries"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 3)

	// ① 自动候选：元数据与证据逐项落地，来源身份不投影。
	first, ok := entries[0].(map[string]any)
	require.True(t, ok)
	requireViewMatchesSchema(t, doc, "MemoryEntryView", first)
	require.Equal(t, "auto_extract", first["source"], "source 闭集已含 auto_extract")
	candidate, ok := first["candidate"].(map[string]any)
	require.True(t, ok, "自动候选必须带 candidate 对象")
	requireViewMatchesSchema(t, doc, "MemoryCandidateView", candidate)
	require.Equal(t, "ext-20261105-0001", candidate["extraction_id"])
	require.Equal(t, "fact", candidate["kind"])
	require.Equal(t, "asserted", candidate["certainty"])
	require.Equal(t, 0.82, candidate["confidence"])
	require.EqualValues(t, 12, candidate["suggested_supersedes_id"])
	require.Equal(t, "2027-02-01T00:00:00Z", candidate["valid_to"])
	require.Equal(t, "exact", candidate["time_confidence"])
	require.Equal(t, []any{float64(7), float64(8)}, candidate["source_entry_ids"])
	require.NotContains(t, candidate, "origin_caller_id", "来源调用方不对外投影")
	require.NotContains(t, candidate, "origin_key_id", "来源 Key 不对外投影")

	evidenceList, ok := candidate["evidence"].([]any)
	require.True(t, ok)
	require.Len(t, evidenceList, 1)
	evidence, ok := evidenceList[0].(map[string]any)
	require.True(t, ok)
	requireViewMatchesSchema(t, doc, "MemoryEvidenceView", evidence)
	require.EqualValues(t, 101, evidence["message_id"])
	require.Equal(t, "run-1", evidence["run_id"])
	require.Equal(t, "user", evidence["role"])
	require.Equal(t, "季度报表在每月 5 号前提交", evidence["quote"])
	require.Equal(t, "2026-11-05T09:30:00Z", evidence["message_at"])

	// ② 空证据 / 空来源输出 `[]`，不输出 null。
	second, ok := entries[1].(map[string]any)
	require.True(t, ok)
	emptyCandidate, ok := second["candidate"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []any{}, emptyCandidate["evidence"], "空证据必须输出 [] 而不是 null")
	require.Equal(t, []any{}, emptyCandidate["source_entry_ids"], "空来源必须输出 [] 而不是 null")

	// ③ 非自动候选：键恒在且为 null。
	third, ok := entries[2].(map[string]any)
	require.True(t, ok)
	requireViewMatchesSchema(t, doc, "MemoryEntryView", third)
	nullValue, present := third["candidate"]
	require.True(t, present, "candidate 键必须恒在：null 表达非自动候选")
	require.Nil(t, nullValue)

	body := rec.Body.String()
	for _, leaked := range []string{"caller-local", "key-1"} {
		require.NotContains(t, body, leaked, "来源身份不得出现在响应体")
	}
}

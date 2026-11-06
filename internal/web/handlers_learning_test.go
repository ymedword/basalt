package web

// 本文件是 US-22「受控自我进化」只读学习查询面的 handler 级验收（颗粒度文档 §3.1.10 /
// FR-22-11，任务 T026）：状态、抽取列表与详情、方法草案列表与详情共五条 GET。
//
// 四条边界在这里守：
//  1. **只读**：五条路由只有 GET；写方法落到方法无关兜底 404，且零端口调用。
//  2. **失败关闭**：端口未装配或存储失败 → 固定 500，**不得**用空列表 / 全零状态冒充
//     "没有抽取"或"学习没跑过"。
//  3. **纯请求非法不触达端口**：`state` 只接受五字面量，`limit` 只接受 1–200（缺省 50）；
//     越界 400 且零端口调用。
//  4. **投影闭集**：视图字段集与契约 schema 完全一致；空集合输出 `[]` 不输出 null；
//     候选不投影来源调用方 / Key（来源身份在抽取行上）。
// The five read-only learning endpoints: only GET, fail-closed on store errors, pure
// request errors rejected before the port, and projections pinned to the contract.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errFakeLearningAdminUnused 标记用例没有为该端口方法准备行为：被调用即说明请求走到了
// 不该走的路径（例如纯请求非法却仍调用端口，或写方法被错误注册）。
// errFakeLearningAdminUnused marks a port method the case never intended to call.
var errFakeLearningAdminUnused = errors.New("fake learning admin: unexpected call")

// fakeLearningAdmin 是可注入行为的 `core.LearningAdmin` 替身：五个只读方法各自记录
// 入参与调用次数；未准备的方法一律回 errFakeLearningAdminUnused。
// fakeLearningAdmin is a behavior-injected read-only core.LearningAdmin double.
type fakeLearningAdmin struct {
	statusFn          func(context.Context) (core.LearningStatus, error)
	listExtractionsFn func(context.Context, core.LearningExtractionFilter) ([]core.LearningExtractionView, error)
	loadExtractionFn  func(context.Context, string) (core.LearningExtractionView, error)
	listDraftsFn      func(context.Context, core.LearningDraftFilter) ([]core.LearningDraftView, error)
	loadDraftFn       func(context.Context, string) (core.LearningDraftView, error)

	lastExtractionFilter core.LearningExtractionFilter
	lastDraftFilter      core.LearningDraftFilter
	lastExtractionID     string
	lastDraftID          string

	statusCalls          int
	listExtractionsCalls int
	loadExtractionCalls  int
	listDraftsCalls      int
	loadDraftCalls       int
}

// Status 记录调用并回注入结果。
func (f *fakeLearningAdmin) Status(ctx context.Context) (core.LearningStatus, error) {
	f.statusCalls++
	if f.statusFn == nil {
		return core.LearningStatus{}, errFakeLearningAdminUnused
	}
	return f.statusFn(ctx)
}

// ListExtractions 记录过滤条件并回注入结果。
func (f *fakeLearningAdmin) ListExtractions(ctx context.Context,
	filter core.LearningExtractionFilter,
) ([]core.LearningExtractionView, error) {
	f.listExtractionsCalls++
	f.lastExtractionFilter = filter
	if f.listExtractionsFn == nil {
		return nil, errFakeLearningAdminUnused
	}
	return f.listExtractionsFn(ctx, filter)
}

// LoadExtraction 记录抽取标识并回注入结果。
func (f *fakeLearningAdmin) LoadExtraction(ctx context.Context,
	extractionID string,
) (core.LearningExtractionView, error) {
	f.loadExtractionCalls++
	f.lastExtractionID = extractionID
	if f.loadExtractionFn == nil {
		return core.LearningExtractionView{}, errFakeLearningAdminUnused
	}
	return f.loadExtractionFn(ctx, extractionID)
}

// ListDrafts 记录过滤条件并回注入结果。
func (f *fakeLearningAdmin) ListDrafts(ctx context.Context,
	filter core.LearningDraftFilter,
) ([]core.LearningDraftView, error) {
	f.listDraftsCalls++
	f.lastDraftFilter = filter
	if f.listDraftsFn == nil {
		return nil, errFakeLearningAdminUnused
	}
	return f.listDraftsFn(ctx, filter)
}

// LoadDraft 记录草案标识并回注入结果。
func (f *fakeLearningAdmin) LoadDraft(ctx context.Context, draftID string) (core.LearningDraftView, error) {
	f.loadDraftCalls++
	f.lastDraftID = draftID
	if f.loadDraftFn == nil {
		return core.LearningDraftView{}, errFakeLearningAdminUnused
	}
	return f.loadDraftFn(ctx, draftID)
}

// 编译期断言：假件实现被测端口——端口漂移在编译期就红。
var _ core.LearningAdmin = (*fakeLearningAdmin)(nil)

// learningPortCalls 汇总五个只读方法的调用次数，供"零端口调用"断言使用。
// learningPortCalls sums the five read call counters.
func learningPortCalls(f *fakeLearningAdmin) int {
	return f.statusCalls + f.listExtractionsCalls + f.loadExtractionCalls +
		f.listDraftsCalls + f.loadDraftCalls
}

// newLearningServer 装配带只读学习端口的 handler 级服务。
// newLearningServer wires the real handler chain over the injected learning port.
func newLearningServer(t *testing.T, mutate func(*Options)) (*Server, *fakeLearningAdmin) {
	t.Helper()
	fake := &fakeLearningAdmin{}
	s := newTestServer(t, func(o *Options) {
		o.LearningAdmin = fake
		if mutate != nil {
			mutate(o)
		}
	})
	return s, fake
}

// learningCall 发一条管理面请求并返回记录器。
func learningCall(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	return serveAdmin(s, adminRequest(method, path, ""))
}

// learningTestStatus 是状态端点的完整样本：每个字段都取非零值，让"漏投影"能被
// 字段集断言抓住。
// learningTestStatus is the fully populated status sample.
func learningTestStatus() core.LearningStatus {
	return core.LearningStatus{
		Enabled:            true,
		Agents:             []string{"ops-agent", "report-agent"},
		UnknownAgents:      []string{"ghost-agent"},
		RuleVersion:        "memory-rules-v1",
		PromptVersion:      "memory-extract-v1",
		AttemptsUsed:       12,
		AttemptLimit:       200,
		Exhausted:          false,
		BudgetWindow:       24 * time.Hour,
		PendingExtractions: 2,
		UnknownExtractions: 1,
		OrphanDrafts:       1,
		LastExtractionAt:   time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC),
	}
}

// learningTestEvidence 是一条逐字证据样本。
// learningTestEvidence is one verbatim-evidence sample.
func learningTestEvidence() core.LearningEvidence {
	return core.LearningEvidence{
		MessageID: 101,
		RunID:     "run-1",
		Role:      "user",
		Quote:     "季度报表在每月 5 号前提交",
		MessageAt: time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC),
	}
}

// learningTestExtractionView 是一次完整抽取读模型：账本行 + 候选 + 草案摘要。
// learningTestExtractionView builds one fully populated extraction read model.
func learningTestExtractionView() core.LearningExtractionView {
	evidence := learningTestEvidence()
	return core.LearningExtractionView{
		Extraction: core.LearningExtraction{
			ExtractionID:          "lrn_20261105a",
			Agent:                 "ops-agent",
			SessionID:             "web:local:ops-agent",
			RunID:                 "run-1",
			LastAttemptRunID:      "run-1",
			OriginCallerID:        "caller-local",
			OriginKeyID:           "key-1",
			Status:                core.LearningExtractionSuccess,
			WindowFromMessageID:   100,
			WindowToMessageID:     105,
			WindowMessages:        4,
			DroppedWindowMessages: 1,
			PriorMessages:         2,
			Attempts:              1,
			ExistingEntries:       9,
			ItemsTotal:            2,
			ItemsAccepted:         1,
			ItemsDropped:          1,
			PromptVersion:         "memory-extract-v1",
			RuleVersion:           "memory-rules-v1",
			Provider:              "local-stub",
			Model:                 "stub-model",
			ConfigHash:            "config-hash",
			CreatedAt:             time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC),
			LastAttemptAt:         time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC),
			FinishedAt:            time.Date(2026, 11, 5, 9, 30, 20, 0, time.UTC),
			VerifiedAt:            time.Date(2026, 11, 5, 9, 31, 0, 0, time.UTC),
		},
		Candidates: []core.LearningCandidateView{{
			EntryID:               41,
			State:                 "candidate",
			Content:               "季度报表在每月 5 号前提交",
			Kind:                  "fact",
			Certainty:             "asserted",
			Confidence:            0.82,
			Evidence:              []core.LearningEvidence{evidence},
			SourceEntryIDs:        []int64{7, 8},
			SuggestedSupersedesID: 12,
			OriginCallerID:        "caller-local",
			OriginKeyID:           "key-1",
			EvidenceAt:            time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC),
			ValidTo:               time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC),
			TimeConfidence:        "exact",
			CreatedAt:             time.Date(2026, 11, 5, 9, 30, 20, 0, time.UTC),
		}},
		Drafts: []core.LearningDraftView{{
			DraftID:       "draft_20261105a",
			ExtractionID:  "lrn_20261105a",
			Agent:         "ops-agent",
			Name:          "weekly-report",
			Content:       "# 周报整理\n\n先汇总再分派。",
			ContentSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Evidence:      []core.LearningEvidence{evidence},
			RelDir:        ".basalt/learning/drafts/draft_20261105a",
			CreatedAt:     time.Date(2026, 11, 5, 9, 30, 20, 0, time.UTC),
		}},
	}
}

// TestUS22_LearningStatus_ReadOnly —— 状态端点只读：GET 返回 200 且字段集与契约
// LearningStatusView 完全一致；只调用 Status，不触达其余四个只读方法；全零状态把
// 名单渲染成 `[]`、最后抽取时刻渲染成 `""`，并且字段集不变（不能用缺键表达"没有"）。
// 逆改点：漏投影任一字段 / 空名单写成 null / 零值时刻写成年份 1 / 额外调用其它端口方法
// → 对应断言红。
func TestUS22_LearningStatus_ReadOnly(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	t.Run("完整状态过契约 schema", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.statusFn = func(context.Context) (core.LearningStatus, error) {
			return learningTestStatus(), nil
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/status")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		env := decodeRunEnvelope[LearningStatusView](t, rec)
		require.Equal(t, codeOK, env.Code)
		requireViewMatchesSchema(t, doc, "LearningStatusView", env.Data)

		require.True(t, env.Data.Enabled)
		require.Equal(t, []string{"ops-agent", "report-agent"}, env.Data.Agents)
		require.Equal(t, []string{"ghost-agent"}, env.Data.UnknownAgents)
		require.Equal(t, 12, env.Data.AttemptsUsed)
		require.Equal(t, 200, env.Data.AttemptLimit)
		require.False(t, env.Data.Exhausted)
		require.EqualValues(t, 86400, env.Data.BudgetWindowSeconds)
		require.Equal(t, 2, env.Data.PendingExtractions)
		require.Equal(t, 1, env.Data.UnknownExtractions)
		require.Equal(t, 1, env.Data.OrphanDrafts)
		require.Equal(t, "2026-11-05T09:30:00Z", env.Data.LastExtractionAt)

		require.Equal(t, 1, fake.statusCalls)
		require.Equal(t, 0, fake.listExtractionsCalls+fake.loadExtractionCalls+
			fake.listDraftsCalls+fake.loadDraftCalls, "状态端点只读，不得触达其它端口方法")
	})

	t.Run("全零状态输出空集合与空时刻", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.statusFn = func(context.Context) (core.LearningStatus, error) {
			return core.LearningStatus{}, nil
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/status")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		env := decodeRunEnvelope[LearningStatusView](t, rec)
		requireViewMatchesSchema(t, doc, "LearningStatusView", env.Data)
		require.NotNil(t, env.Data.Agents, "空名单必须输出 []，不输出 null")
		require.Empty(t, env.Data.Agents)
		require.NotNil(t, env.Data.UnknownAgents)
		require.Empty(t, env.Data.UnknownAgents)
		require.Empty(t, env.Data.LastExtractionAt, "从未抽取输出空串，不输出零值时刻")
	})
}

// TestUS22_LearningExtractions_ListAndDetail —— 抽取列表与详情：
//   - 列表把 `agent` / `state` / `limit` 原样送进端口；缺省 limit 是 50；
//   - 非法 `state` 与越界 `limit` → 400 且零端口调用（过滤错误不得翻译成空页）；
//   - 空账本输出 `[]`；列表行 / 详情整对象过契约 schema；
//   - 详情展开候选与草案摘要，候选不投影来源调用方 / Key，数组恒为非 nil；
//   - 不存在 → 404 且文案点名"学习抽取记录"；未完成 / 未查证时刻渲染空串。
//
// 逆改点：丢掉过滤参数 / 缺省 limit 写成 0 / 把校验放在端口之后 / 投影 origin_key_id /
// 空数组写成 null / 404 分支出成 500 → 对应断言红。
func TestUS22_LearningExtractions_ListAndDetail(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	t.Run("列表透传过滤条件并过契约 schema", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.listExtractionsFn = func(_ context.Context,
			filter core.LearningExtractionFilter,
		) ([]core.LearningExtractionView, error) {
			require.Equal(t, "ops-agent", filter.Agent)
			require.Equal(t, "success", filter.State)
			require.Equal(t, 7, filter.Limit)
			return []core.LearningExtractionView{learningTestExtractionView()}, nil
		}

		rec := learningCall(t, s, http.MethodGet,
			"/api/v1/learning/extractions?agent=ops-agent&state=success&limit=7")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, 1, fake.listExtractionsCalls)

		env := decodeRunEnvelope[LearningExtractionListView](t, rec)
		requireViewMatchesSchema(t, doc, "LearningExtractionListView", env.Data)
		require.Len(t, env.Data.Extractions, 1)
		requireViewMatchesSchema(t, doc, "LearningExtractionView", env.Data.Extractions[0])
		require.Equal(t, "lrn_20261105a", env.Data.Extractions[0].ExtractionID)
		require.Equal(t, "success", env.Data.Extractions[0].Status)
	})

	t.Run("缺省 limit 与空过滤", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.listExtractionsFn = func(_ context.Context,
			filter core.LearningExtractionFilter,
		) ([]core.LearningExtractionView, error) {
			require.Empty(t, filter.Agent)
			require.Empty(t, filter.State)
			require.Equal(t, 50, filter.Limit, "缺省 limit 是 50")
			return []core.LearningExtractionView{}, nil
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/extractions")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		env := decodeRunEnvelope[LearningExtractionListView](t, rec)
		requireViewMatchesSchema(t, doc, "LearningExtractionListView", env.Data)
		require.NotNil(t, env.Data.Extractions, "空账本必须输出 []，不输出 null")
		require.Empty(t, env.Data.Extractions)
	})

	t.Run("非法 state 与越界 limit 400 且零端口调用", func(t *testing.T) {
		cases := []struct{ name, query string }{
			{"状态越界", "/api/v1/learning/extractions?state=bogus"},
			{"limit 为零", "/api/v1/learning/extractions?limit=0"},
			{"limit 超上限", "/api/v1/learning/extractions?limit=201"},
			{"limit 非整数", "/api/v1/learning/extractions?limit=abc"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				s, fake := newLearningServer(t, nil)
				rec := learningCall(t, s, http.MethodGet, tc.query)
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				require.Equal(t, codeBadRequest, decodeAuthEnvelope(t, rec).Code)
				require.Zero(t, learningPortCalls(fake), "请求非法时不得触达端口")
			})
		}
	})

	t.Run("详情展开候选与草案摘要", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.loadExtractionFn = func(_ context.Context, extractionID string) (core.LearningExtractionView, error) {
			require.Equal(t, "lrn_20261105a", extractionID)
			return learningTestExtractionView(), nil
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/extractions/lrn_20261105a")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		env := decodeRunEnvelope[LearningExtractionDetailView](t, rec)
		requireViewMatchesSchema(t, doc, "LearningExtractionDetailView", env.Data)
		require.Equal(t, "lrn_20261105a", env.Data.Extraction.ExtractionID)
		require.Len(t, env.Data.Candidates, 1)
		requireViewMatchesSchema(t, doc, "LearningCandidateView", env.Data.Candidates[0])
		require.Len(t, env.Data.Drafts, 1)
		requireViewMatchesSchema(t, doc, "LearningDraftMetaView", env.Data.Drafts[0])

		candidate := env.Data.Candidates[0]
		require.Equal(t, "fact", candidate.Kind)
		require.EqualValues(t, 12, candidate.SuggestedSupersedesID)
		require.Equal(t, "2027-02-01T00:00:00Z", candidate.ValidTo)
		candidateJSON, err := json.Marshal(candidate)
		require.NoError(t, err)
		require.NotContains(t, string(candidateJSON), "origin_",
			"候选不投影来源调用方 / Key——来源身份属于抽取行")
		require.Contains(t, rec.Body.String(), `"origin_caller_id":"caller-local"`,
			"来源身份属于抽取行，必须仍可复核")
	})

	t.Run("未完成与未查证时刻渲染空串", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		view := learningTestExtractionView()
		view.Extraction.Status = core.LearningExtractionPending
		view.Extraction.FinishedAt = time.Time{}
		view.Extraction.VerifiedAt = time.Time{}
		view.Candidates = []core.LearningCandidateView{}
		view.Drafts = []core.LearningDraftView{}
		fake.loadExtractionFn = func(context.Context, string) (core.LearningExtractionView, error) {
			return view, nil
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/extractions/lrn_pending")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		env := decodeRunEnvelope[LearningExtractionDetailView](t, rec)
		requireViewMatchesSchema(t, doc, "LearningExtractionDetailView", env.Data)
		require.Equal(t, "pending", env.Data.Extraction.Status)
		require.Empty(t, env.Data.Extraction.FinishedAt)
		require.Empty(t, env.Data.Extraction.VerifiedAt)
		require.NotNil(t, env.Data.Candidates, "空候选必须输出 []")
		require.NotNil(t, env.Data.Drafts, "空草案必须输出 []")
	})

	t.Run("抽取不存在 404", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.loadExtractionFn = func(context.Context, string) (core.LearningExtractionView, error) {
			return core.LearningExtractionView{}, core.ErrLearningNotFound
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/extractions/lrn_missing")
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeNotFound, env.Code)
		require.Equal(t, messageLearningExtractionNotFound, env.Message)
	})
}

// TestUS22_LearningDrafts_ListAndDetail —— 草案列表与详情：
//   - 列表只回元数据，**没有**正文键；空列表输出 `[]`；越界 limit 400 且零端口调用；
//   - 详情回元数据 + `DRAFT.md` 正文 + 证据，整对象过契约 schema；
//   - 不存在 → 404 且文案点名"学习方法草案"。
//
// 逆改点：列表漏出正文 / 空列表写成 null / 404 分支出成 500 → 对应断言红。
func TestUS22_LearningDrafts_ListAndDetail(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	draft := learningTestExtractionView().Drafts[0]

	t.Run("列表只回元数据", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.listDraftsFn = func(_ context.Context, filter core.LearningDraftFilter) ([]core.LearningDraftView, error) {
			require.Equal(t, "ops-agent", filter.Agent)
			require.Equal(t, 50, filter.Limit)
			return []core.LearningDraftView{draft}, nil
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/drafts?agent=ops-agent")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		env := decodeRunEnvelope[LearningDraftListView](t, rec)
		requireViewMatchesSchema(t, doc, "LearningDraftListView", env.Data)
		require.Len(t, env.Data.Drafts, 1)
		requireViewMatchesSchema(t, doc, "LearningDraftMetaView", env.Data.Drafts[0])
		require.NotContains(t, rec.Body.String(), "先汇总再分派", "列表不得漏出草案正文")
	})

	t.Run("空列表输出空数组", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.listDraftsFn = func(context.Context, core.LearningDraftFilter) ([]core.LearningDraftView, error) {
			return nil, nil
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/drafts")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		env := decodeRunEnvelope[LearningDraftListView](t, rec)
		requireViewMatchesSchema(t, doc, "LearningDraftListView", env.Data)
		require.NotNil(t, env.Data.Drafts, "空列表必须输出 []，不输出 null")
	})

	t.Run("越界 limit 400 且零端口调用", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/drafts?limit=0")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Zero(t, learningPortCalls(fake), "请求非法时不得触达端口")
	})

	t.Run("详情含正文与证据", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.loadDraftFn = func(_ context.Context, draftID string) (core.LearningDraftView, error) {
			require.Equal(t, "draft_20261105a", draftID)
			return draft, nil
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/drafts/draft_20261105a")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		env := decodeRunEnvelope[LearningDraftDetailView](t, rec)
		requireViewMatchesSchema(t, doc, "LearningDraftDetailView", env.Data)
		require.Equal(t, "draft_20261105a", env.Data.DraftID)
		require.Contains(t, env.Data.Content, "先汇总再分派")
		require.Len(t, env.Data.Evidence, 1)
		requireViewMatchesSchema(t, doc, "LearningEvidenceView", env.Data.Evidence[0])
		require.EqualValues(t, 101, env.Data.Evidence[0].MessageID)
	})

	t.Run("草案不存在 404", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.loadDraftFn = func(context.Context, string) (core.LearningDraftView, error) {
			return core.LearningDraftView{}, core.ErrLearningDraftNotFound
		}

		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/drafts/draft_missing")
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeNotFound, env.Code)
		require.Equal(t, messageLearningDraftNotFound, env.Message)
	})
}

// TestUS22_NoWriteEndpoints —— 学习面**没有**写端点：
//   - 五条路由在表内恰为 GET（路由表里的 learning 行集合逐字相等）；
//   - 每个 learning 路径上的 POST / PUT / PATCH / DELETE 一律非 2xx（方法无关兜底
//     404），且零端口调用；
//   - `core.LearningAdmin` 类型上只有五个只读方法——端口本身没有晋升 / 重试 / 发布。
//
// 逆改点：给学习面加写方法 / 兜底被改成自动 405 之外的 2xx / 端口长出写方法 → 红。
func TestUS22_NoWriteEndpoints(t *testing.T) {
	t.Run("路由表里的学习行恰为五条 GET", func(t *testing.T) {
		s := newTestServer(t, nil)
		got := map[string]bool{}
		for _, rt := range s.routeTable() {
			if !strings.HasPrefix(rt.pattern, "/api/v1/learning") {
				continue
			}
			require.Equal(t, routeProtected, rt.visibility, "%s 必须受保护", routeKey(rt))
			got[routeKey(rt)] = true
		}
		require.Equal(t, map[string]bool{
			"GET /api/v1/learning/status":           true,
			"GET /api/v1/learning/extractions":      true,
			"GET /api/v1/learning/extractions/{id}": true,
			"GET /api/v1/learning/drafts":           true,
			"GET /api/v1/learning/drafts/{id}":      true,
		}, got, "学习面只能有这五条 GET")
	})

	t.Run("写方法一律非成功且零端口调用", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		paths := []string{
			"/api/v1/learning/status",
			"/api/v1/learning/extractions",
			"/api/v1/learning/extractions/lrn_x",
			"/api/v1/learning/drafts",
			"/api/v1/learning/drafts/draft_x",
		}
		for _, path := range paths {
			for _, method := range []string{
				http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
			} {
				rec := learningCall(t, s, method, path)
				require.GreaterOrEqual(t, rec.Code, 400,
					"%s %s 必须非成功（没有写端点）", method, path)
				require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code,
					"%s %s 应落到 404 兜底", method, path)
			}
		}
		require.Zero(t, learningPortCalls(fake), "写方法不得触达任何学习端口方法")
	})

	t.Run("端口在类型上只有五个只读方法", func(t *testing.T) {
		require.Equal(t, 5, reflect.TypeOf((*core.LearningAdmin)(nil)).Elem().NumMethod(),
			"LearningAdmin 不得长出写方法：确认 / 删除复用记忆管理动作")
	})
}

// TestUS22_Status_StoreError —— 存储失败**显式失败**：五个端点一律 500 且不降级成
// 空状态 / 空列表；端口未装配同样固定 500（装配错误不是"没有学习记录"）。
// 逆改点：任一错误分支回 200 + 空集合 / 全零状态 → 对应子例红。
func TestUS22_Status_StoreError(t *testing.T) {
	boom := errors.New("database is gone")

	t.Run("状态读取失败 500", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.statusFn = func(context.Context) (core.LearningStatus, error) {
			return core.LearningStatus{}, boom
		}
		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/status")
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
		require.Nil(t, env.Data, "存储失败不得回显空状态冒充正常读数")
	})

	t.Run("抽取列表失败 500", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.listExtractionsFn = func(context.Context,
			core.LearningExtractionFilter,
		) ([]core.LearningExtractionView, error) {
			return nil, boom
		}
		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/extractions")
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("抽取详情失败 500", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.loadExtractionFn = func(context.Context, string) (core.LearningExtractionView, error) {
			return core.LearningExtractionView{}, boom
		}
		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/extractions/lrn_x")
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("草案列表失败 500", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.listDraftsFn = func(context.Context, core.LearningDraftFilter) ([]core.LearningDraftView, error) {
			return nil, boom
		}
		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/drafts")
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("草案详情失败 500", func(t *testing.T) {
		s, fake := newLearningServer(t, nil)
		fake.loadDraftFn = func(context.Context, string) (core.LearningDraftView, error) {
			return core.LearningDraftView{}, boom
		}
		rec := learningCall(t, s, http.MethodGet, "/api/v1/learning/drafts/draft_x")
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("端口未装配固定 500", func(t *testing.T) {
		s := newTestServer(t, nil)
		for _, path := range []string{
			"/api/v1/learning/status",
			"/api/v1/learning/extractions",
			"/api/v1/learning/extractions/lrn_x",
			"/api/v1/learning/drafts",
			"/api/v1/learning/drafts/draft_x",
		} {
			rec := learningCall(t, s, http.MethodGet, path)
			require.Equal(t, http.StatusInternalServerError, rec.Code, path)
			require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code, path)
		}
	})
}

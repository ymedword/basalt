package web

// 本文件是学习准入只读面（US-21 颗粒度文档 §3.1.11 / §4.3.15）的验收：
// 列表的三参数与固定窗口、派生展示态 `expired`（与 CLI 同判据、`state=approved`
// 同口径剔除）、详情投影（清单摘要不含正文、来源 notes 不出站、子记录缺失为 null、
// 候选缺失才是 404）、缺端口固定 500、没有任何写端点，以及
// `target_kind=skill_admission` 的操作查询。
// Acceptance for the read-only admission surface: filters, the derived expired state,
// the detail projection, the missing-port 500, the absence of write endpoints, and the
// skill_admission operation filter.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/assets"
	"github.com/ymedword/basalt/internal/core"
)

// admissionWebNow 是本文件所有候选 / 决策夹具的固定时钟；与 defaultTestOptions 的
// 缺省 Now 逐字一致，但装配时仍显式注入，避免缺省值漂移后过期断言悄悄失效。
// admissionWebNow is the fixed clock for every admission fixture here; the server gets
// it injected explicitly so an expiry assertion cannot silently depend on a default.
var admissionWebNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// admissionFake 是 AdmissionReader 的记录型替身：列表记录过滤条件，详情按候选标识
// 取预置子记录；缺失一律报告 `core.ErrAdmissionNotFound`（与仓储同一哨兵）。
// admissionFake records list filters and serves preset sub-records; any missing record
// reports the store's own not-found sentinel.
type admissionFake struct {
	mu sync.Mutex

	filters   []core.AdmissionFilter
	listCalls int
	listed    []core.AdmissionCandidate
	listErr   error

	candidates map[string]core.AdmissionCandidate
	getErr     error

	evaluations   map[string]core.AdmissionEvaluation
	evaluationErr error

	decisions   map[string]core.AdmissionDecision
	decisionErr error

	publications   map[string]core.AdmissionPublication
	publicationErr error
}

// var _ AdmissionReader 让替身与端口方法集在编译期同步。
var _ AdmissionReader = (*admissionFake)(nil)

func (f *admissionFake) ListCandidates(_ context.Context, filter core.AdmissionFilter) ([]core.AdmissionCandidate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	f.filters = append(f.filters, filter)
	return f.listed, f.listErr
}

func (f *admissionFake) GetCandidate(_ context.Context, candidateID string) (core.AdmissionCandidate, error) {
	if f.getErr != nil {
		return core.AdmissionCandidate{}, f.getErr
	}
	candidate, ok := f.candidates[candidateID]
	if !ok {
		return core.AdmissionCandidate{}, core.ErrAdmissionNotFound
	}
	return candidate, nil
}

func (f *admissionFake) LatestEvaluation(_ context.Context, candidateID string) (core.AdmissionEvaluation, error) {
	if f.evaluationErr != nil {
		return core.AdmissionEvaluation{}, f.evaluationErr
	}
	evaluation, ok := f.evaluations[candidateID]
	if !ok {
		return core.AdmissionEvaluation{}, core.ErrAdmissionNotFound
	}
	return evaluation, nil
}

func (f *admissionFake) LatestDecision(_ context.Context, candidateID string) (core.AdmissionDecision, error) {
	if f.decisionErr != nil {
		return core.AdmissionDecision{}, f.decisionErr
	}
	decision, ok := f.decisions[candidateID]
	if !ok {
		return core.AdmissionDecision{}, core.ErrAdmissionNotFound
	}
	return decision, nil
}

func (f *admissionFake) GetPublication(_ context.Context, candidateID string) (core.AdmissionPublication, error) {
	if f.publicationErr != nil {
		return core.AdmissionPublication{}, f.publicationErr
	}
	publication, ok := f.publications[candidateID]
	if !ok {
		return core.AdmissionPublication{}, core.ErrAdmissionNotFound
	}
	return publication, nil
}

// lastFilter / listCallCount 供「参数原样传入」「非法参数零查询」断言。
func (f *admissionFake) lastFilter(t *testing.T) core.AdmissionFilter {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.filters, "handler 必须调用 ListCandidates")
	return f.filters[len(f.filters)-1]
}

func (f *admissionFake) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// newAdmissionServer 装配一个注入只读准入替身与有效管理凭据的服务器。
func newAdmissionServer(t *testing.T, fake *admissionFake) *Server {
	t.Helper()
	return newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.Now = func() time.Time { return admissionWebNow }
		o.Admissions = fake
	})
}

// admissionEnvelope 是测试侧的解码形态；Data 用 RawMessage 以区分「空列表 []」与
// 「字段缺失」，也便于对失败响应断言 data 不出现。
// admissionEnvelope decodes with RawMessage so [] and an omitted data stay distinct.
type admissionEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func decodeAdmissionEnvelope(t *testing.T, rec *httptest.ResponseRecorder) admissionEnvelope {
	t.Helper()
	var env admissionEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
	return env
}

func decodeAdmissionList(t *testing.T, rec *httptest.ResponseRecorder) []AdmissionListView {
	t.Helper()
	env := decodeAdmissionEnvelope(t, rec)
	var views []AdmissionListView
	require.NoError(t, json.Unmarshal(env.Data, &views))
	return views
}

// admissionTestSourceJSON 生成严格解析可通过的来源声明；notes 是故意塞进夹具的
// 自由文本，用于断言详情不出站。
// admissionTestSourceJSON builds a declaration that passes strict parsing; notes is the
// free-form text used to prove the detail projection withholds it.
func admissionTestSourceJSON(notes string) string {
	return fmt.Sprintf(`{"kind":"manual","ref":"operator/notes/weekly-report",`+
		`"task_outcome":"completed","confirmation":"direct","authorization":"declared",`+
		`"redaction":"declared","retention_until":"2026-12-31T00:00:00Z","applicable":true,`+
		`"counterexamples":[],"evidence":["人工整理的周报模板"],"limitations":[],"notes":%q}`, notes)
}

// admissionTestManifest 是两份文件摘要（路径严格升序、64 位小写十六进制哈希）——
// 清单只含摘要，本文件的所有断言都不应看到任何文件正文。
// admissionTestManifest holds two digests in canonical order; no body ever appears.
func admissionTestManifest() string {
	return `[{"path":"SKILL.md","sha256":"` + strings.Repeat("a", 64) + `","size":42},` +
		`{"path":"references/checklist.md","sha256":"` + strings.Repeat("b", 64) + `","size":7}]`
}

// admissionTestCandidate 构造一条冻结候选行；manifest / source 由本文件的两个
// 生成器给出，其余字段固定，便于逐字段断言投影。
func admissionTestCandidate(id, skill string, state core.AdmissionState) core.AdmissionCandidate {
	return core.AdmissionCandidate{
		CandidateID:        id,
		SkillName:          skill,
		State:              state,
		Revision:           1,
		CandidateHash:      strings.Repeat("c", 64),
		SourceHash:         strings.Repeat("d", 64),
		ManifestJSON:       admissionTestManifest(),
		SourceJSON:         admissionTestSourceJSON("内部自由备注：不得整段出站"),
		ProposerSubject:    "cli:alice",
		ProposerAuthMethod: core.AuthMethodCLI,
		ProposerSource:     core.AuthMethodCLI,
		RiskTier:           core.AdmissionRiskNormal,
		CreatedAt:          admissionWebNow.Add(-time.Hour),
		UpdatedAt:          admissionWebNow.Add(-30 * time.Minute),
	}
}

// TestUS21_NoWriteEndpoints 逐字落地 §4.3.15：两个路径 × 四种写方法，带合法
// admin token 请求——证明拒绝来自「没有该端点」，而不是「没带凭据」。
// 逆改点：给任一写方法注册路由（哪怕返回 200 空实现）→ 对应用例红；把写端点挂在
// 详情路径上同样红。
func TestUS21_NoWriteEndpoints(t *testing.T) {
	fake := &admissionFake{}
	srv := newAdmissionServer(t, fake)
	paths := []string{"/api/v1/admissions", "/api/v1/admissions/adm_x"}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	for _, path := range paths {
		for _, method := range methods {
			req := withBearer(adminRequest(method, path, `{}`), testToken)
			rec := serveAdmin(srv, req)
			require.NotEqual(t, http.StatusOK, rec.Code, "%s %s 不得是写端点", method, path)
			require.NotEqual(t, http.StatusNoContent, rec.Code, "%s %s 不得是写端点", method, path)
			require.Equal(t, http.StatusNotFound, rec.Code,
				"%s %s 必须落到方法无关兜底 404 信封", method, path)
		}
	}
	// 只读端口一次都不该被写请求触达。
	require.Equal(t, 0, fake.listCallCount(), "写方法不得触达只读端口")
}

// TestUS21_OperationTargetKind 钉住 `target_kind=skill_admission`（§3.1.11 / FR-21-12）：
// 闭集接受新字面量、400 文案列出它、过滤原样传到仓储；闭集外的取值零查询即拒。
// 逆改点：validOperationTargetKind 少收 skill_admission → 第一条断言红；文案漏列 →
// Contains 红；handler 改写过滤值 → 相等断言红。
func TestUS21_OperationTargetKind(t *testing.T) {
	require.True(t, validOperationTargetKind(core.TargetKindSkillAdmission))
	require.Contains(t, badOperationKindMessage, core.TargetKindSkillAdmission)

	fake := &operationFake{listed: []core.OperationRecord{}}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.Operations = fake
	})
	rec := serveAdmin(srv, withBearer(
		adminRequest(http.MethodGet, "/api/v1/operations?target_kind=skill_admission", ""), testToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, core.TargetKindSkillAdmission, fake.lastFilter(t).TargetKind,
		"过滤值必须原样传到仓储")

	rec = serveAdmin(srv, withBearer(
		adminRequest(http.MethodGet, "/api/v1/operations?target_kind=skill_admissions", ""), testToken))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, badOperationKindMessage, decodeAdmissionEnvelope(t, rec).Message)
	require.Equal(t, 1, fake.listCallCount(), "闭集外的取值必须在触达仓储前拒绝")
}

// TestUS21_AdmissionList_FiltersAndWindow 覆盖缺省窗口、三参数原样传入、非法参数
// 400（零查询、固定文案不回显取值）、仓储错误固定 500 与空列表投影为 []。
// 逆改点：缺省 limit 写错 / 过滤被改写 / 非法参数先查库 / 错误原因进响应 /
// 空列表写成 null → 对应断言红。
func TestUS21_AdmissionList_FiltersAndWindow(t *testing.T) {
	t.Run("缺省窗口 50、无过滤且空列表为 []", func(t *testing.T) {
		fake := &admissionFake{listed: []core.AdmissionCandidate{}}
		srv := newAdmissionServer(t, fake)

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions", ""), testToken))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, core.AdmissionFilter{Limit: defaultAdmissionListLimit}, fake.lastFilter(t),
			"缺省只给固定窗口，不得预填任何过滤值")
		env := decodeAdmissionEnvelope(t, rec)
		require.Equal(t, codeOK, env.Code)
		var views []AdmissionListView
		require.NoError(t, json.Unmarshal(env.Data, &views))
		require.NotNil(t, views, "空列表必须投影为 []，不得是 null")
		require.Empty(t, views)
	})

	t.Run("三个过滤原样传入", func(t *testing.T) {
		fake := &admissionFake{listed: []core.AdmissionCandidate{}}
		srv := newAdmissionServer(t, fake)

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
			"/api/v1/admissions?state=published&skill=weekly-report&limit=7", ""), testToken))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, core.AdmissionFilter{
			State: core.AdmissionPublished,
			Skill: "weekly-report",
			Limit: 7,
		}, fake.lastFilter(t))
	})

	t.Run("非法参数 400 且零查询", func(t *testing.T) {
		cases := []struct {
			query   string
			message string
		}{
			{"state=bogus", badAdmissionStateMessage},
			{"limit=0", badAdmissionLimitMessage},
			{"limit=201", badAdmissionLimitMessage},
			{"limit=abc", badAdmissionLimitMessage},
		}
		for _, tc := range cases {
			t.Run(tc.query, func(t *testing.T) {
				fake := &admissionFake{listed: []core.AdmissionCandidate{}}
				srv := newAdmissionServer(t, fake)

				rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet,
					"/api/v1/admissions?"+tc.query, ""), testToken))

				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				env := decodeAdmissionEnvelope(t, rec)
				require.Equal(t, codeBadRequest, env.Code)
				require.Equal(t, tc.message, env.Message, "只回固定文案，不回显请求值")
				require.Equal(t, 0, fake.listCallCount(), "非法参数必须在触达仓储前拒绝")
			})
		}
	})

	t.Run("仓储错误固定 500 不泄露原因", func(t *testing.T) {
		fake := &admissionFake{listErr: errors.New("boom: relation admission_candidates")}
		srv := newAdmissionServer(t, fake)

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions", ""), testToken))

		require.Equal(t, http.StatusInternalServerError, rec.Code)
		env := decodeAdmissionEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
		require.NotContains(t, rec.Body.String(), "boom")
	})
}

// TestUS21_AdmissionList_DerivedExpired 钉住派生展示态：`approved` 且活跃批准
// 有效期已过（`now >= expires_at`，含相等）显示 `expired`，库中状态不变，`state=approved`
// 与 CLI 同口径剔除该行；被 supersede 的批准、非 approve 决策、零有效期与决策行缺失
// 都按存储态展示，不派生。
// 逆改点：把过期判据改成 `now > expires_at`（等值行红）或漏判 superseded / 非 approve /
// 零有效期（对应状态断言红）；忘记在 approved 过滤下剔除派生 expired → 条数断言红。
func TestUS21_AdmissionList_DerivedExpired(t *testing.T) {
	expired := admissionTestCandidate("adm_expired", "weekly-report", core.AdmissionApproved)
	edge := admissionTestCandidate("adm_edge", "weekly-report", core.AdmissionApproved)
	active := admissionTestCandidate("adm_active", "weekly-report", core.AdmissionApproved)
	stale := admissionTestCandidate("adm_stale", "weekly-report", core.AdmissionApproved)
	superseded := admissionTestCandidate("adm_superseded", "weekly-report", core.AdmissionApproved)
	nonApprove := admissionTestCandidate("adm_reject", "weekly-report", core.AdmissionApproved)
	zeroExpiry := admissionTestCandidate("adm_zero", "weekly-report", core.AdmissionApproved)
	fake := &admissionFake{
		listed: []core.AdmissionCandidate{expired, edge, active, stale, superseded, nonApprove, zeroExpiry},
		decisions: map[string]core.AdmissionDecision{
			"adm_expired": {
				DecisionID: "dec_1", CandidateID: "adm_expired",
				Decision: core.AdmissionApprove, ExpiresAt: admissionWebNow.Add(-time.Minute),
			},
			// 等值边界：`now == expires_at` 即过期（闭区间）。
			"adm_edge": {
				DecisionID: "dec_edge", CandidateID: "adm_edge",
				Decision: core.AdmissionApprove, ExpiresAt: admissionWebNow,
			},
			"adm_active": {
				DecisionID: "dec_2", CandidateID: "adm_active",
				Decision: core.AdmissionApprove, ExpiresAt: admissionWebNow.Add(time.Hour),
			},
			"adm_superseded": {
				DecisionID: "dec_3", CandidateID: "adm_superseded",
				Decision: core.AdmissionApprove, ExpiresAt: admissionWebNow.Add(time.Hour),
				SupersededAt: admissionWebNow.Add(-time.Minute),
			},
			"adm_reject": {
				DecisionID: "dec_4", CandidateID: "adm_reject",
				Decision: core.AdmissionReject, ExpiresAt: admissionWebNow.Add(time.Hour),
			},
			"adm_zero": {
				DecisionID: "dec_5", CandidateID: "adm_zero",
				Decision: core.AdmissionApprove,
			},
		},
	}
	srv := newAdmissionServer(t, fake)

	rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions", ""), testToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	states := map[string]string{}
	for _, view := range decodeAdmissionList(t, rec) {
		states[view.CandidateID] = view.State
	}
	require.Equal(t, string(core.AdmissionExpired), states["adm_expired"])
	require.Equal(t, string(core.AdmissionExpired), states["adm_edge"], "now == expires_at 即过期")
	require.Equal(t, string(core.AdmissionApproved), states["adm_active"])
	require.Equal(t, string(core.AdmissionApproved), states["adm_stale"],
		"决策行缺失时按存储态展示，不得派生")
	require.Equal(t, string(core.AdmissionApproved), states["adm_superseded"],
		"被 supersede 的批准不派生过期")
	require.Equal(t, string(core.AdmissionApproved), states["adm_reject"],
		"非 approve 决策不派生过期")
	require.Equal(t, string(core.AdmissionApproved), states["adm_zero"],
		"零有效期不派生过期")

	rec = serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions?state=approved", ""), testToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, core.AdmissionApproved, fake.lastFilter(t).State)
	views := decodeAdmissionList(t, rec)
	require.Len(t, views, 5, "state=approved 必须与 CLI 同口径剔除派生的 expired 行")
	for _, view := range views {
		require.NotEqual(t, string(core.AdmissionExpired), view.State)
	}
}

// TestUS21_AdmissionDetail_Projection 覆盖详情投影：清单摘要（无正文）、来源声明
// 结构化字段（notes 不出站）、七维评估、批准者与有效期、发布记录，以及派生
// expired、子记录缺失为 null、候选缺失 404、损坏行与读取失败 500。
// 逆改点：把 notes 原样出站 / 返回文件正文 / 子记录缺失变 500 / 候选缺失变 200 /
// 解析失败降级为空清单 → 对应断言红。
func TestUS21_AdmissionDetail_Projection(t *testing.T) {
	candidate := admissionTestCandidate("adm_01", "weekly-report", core.AdmissionApproved)
	evaluation := core.AdmissionEvaluation{
		EvaluationID:      "eval_01",
		CandidateID:       "adm_01",
		CandidateRevision: 1,
		CandidateHash:     candidate.CandidateHash,
		EvaluationHash:    strings.Repeat("e", 64),
		GraderVersion:     "grader-v1",
		PolicyHash:        strings.Repeat("f", 64),
		Dimensions: []core.AdmissionDimension{
			{Name: core.AdmissionDimensionContent, Status: core.AdmissionDimensionPassed, Reason: "清单与内容哈希一致"},
			{Name: core.AdmissionDimensionEffect, Status: core.AdmissionDimensionNotEvaluable, Reason: "real_model_not_authorized"},
		},
		Scripts:     []string{},
		Aggregate:   core.AdmissionAggregateIncomplete,
		RiskTier:    core.AdmissionRiskNormal,
		Referrers:   []string{"ops-agent"},
		EvaluatedAt: admissionWebNow.Add(-30 * time.Minute),
	}
	decision := core.AdmissionDecision{
		DecisionID:        "dec_01",
		CandidateID:       "adm_01",
		CandidateRevision: 1,
		CandidateHash:     candidate.CandidateHash,
		EvaluationID:      "eval_01",
		EvaluationHash:    evaluation.EvaluationHash,
		Decision:          core.AdmissionApprove,
		Reason:            "人工复核通过",
		Acknowledged:      []core.AdmissionDimensionName{core.AdmissionDimensionEffect},
		RiskTier:          core.AdmissionRiskNormal,
		ApproverSubject:   "cli:bob",
		ExpiresAt:         admissionWebNow.Add(time.Hour),
		CreatedAt:         admissionWebNow.Add(-20 * time.Minute),
	}
	publication := core.AdmissionPublication{
		CandidateID: "adm_01",
		DecisionID:  "dec_01",
		SkillName:   "weekly-report",
		VersionID:   12,
		VersionNo:   3,
		ContentHash: strings.Repeat("1", 64),
		State:       core.AdmissionPublicationActive,
		PublishedAt: admissionWebNow.Add(-10 * time.Minute),
	}

	t.Run("完整详情投影", func(t *testing.T) {
		fake := &admissionFake{
			candidates:   map[string]core.AdmissionCandidate{"adm_01": candidate},
			evaluations:  map[string]core.AdmissionEvaluation{"adm_01": evaluation},
			decisions:    map[string]core.AdmissionDecision{"adm_01": decision},
			publications: map[string]core.AdmissionPublication{"adm_01": publication},
		}
		srv := newAdmissionServer(t, fake)

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions/adm_01", ""), testToken))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		env := decodeAdmissionEnvelope(t, rec)
		var view AdmissionDetailView
		require.NoError(t, json.Unmarshal(env.Data, &view))

		require.Equal(t, "adm_01", view.CandidateID)
		require.Equal(t, "weekly-report", view.SkillName)
		require.Equal(t, string(core.AdmissionApproved), view.State)
		require.Equal(t, "cli:alice", view.ProposerSubject)

		require.Len(t, view.Files, 2)
		require.Equal(t, "SKILL.md", view.Files[0].Path)
		require.Equal(t, strings.Repeat("a", 64), view.Files[0].SHA256)
		require.Equal(t, int64(42), view.Files[0].Size)

		require.Equal(t, "manual", view.Source.Kind)
		require.Equal(t, "operator/notes/weekly-report", view.Source.Ref)
		require.Empty(t, view.Source.Notes, "来源 notes 不得整段出站")
		require.NotContains(t, rec.Body.String(), "内部自由备注")

		require.NotNil(t, view.Evaluation)
		require.Equal(t, "eval_01", view.Evaluation.EvaluationID)
		require.Equal(t, "grader-v1", view.Evaluation.GraderVersion)
		require.Len(t, view.Evaluation.Dimensions, 2)
		require.NotNil(t, view.Evaluation.Scripts, "scripts 必须是数组，不得是 null")
		require.Equal(t, []string{"ops-agent"}, view.Evaluation.Referrers)
		require.Equal(t, string(core.AdmissionAggregateIncomplete), view.Evaluation.Aggregate)

		require.NotNil(t, view.Decision)
		require.Equal(t, "cli:bob", view.Decision.ApproverSubject)
		require.Equal(t, []string{string(core.AdmissionDimensionEffect)}, view.Decision.Acknowledged)
		require.Equal(t, "2026-10-18T13:00:00Z", view.Decision.ExpiresAt)

		require.NotNil(t, view.Publication)
		require.Equal(t, int64(3), view.Publication.VersionNo)
		require.Equal(t, int64(12), view.Publication.VersionID)
		require.Equal(t, string(core.AdmissionPublicationActive), view.Publication.State)
	})

	t.Run("子记录缺失投影为 null", func(t *testing.T) {
		fake := &admissionFake{
			candidates: map[string]core.AdmissionCandidate{"adm_01": candidate},
		}
		srv := newAdmissionServer(t, fake)

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions/adm_01", ""), testToken))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var view AdmissionDetailView
		require.NoError(t, json.Unmarshal(decodeAdmissionEnvelope(t, rec).Data, &view))
		require.Nil(t, view.Evaluation)
		require.Nil(t, view.Decision)
		require.Nil(t, view.Publication)
		require.Equal(t, string(core.AdmissionApproved), view.State)
	})

	t.Run("派生 expired", func(t *testing.T) {
		expiredDecision := decision
		expiredDecision.ExpiresAt = admissionWebNow.Add(-time.Minute)
		fake := &admissionFake{
			candidates: map[string]core.AdmissionCandidate{"adm_01": candidate},
			decisions:  map[string]core.AdmissionDecision{"adm_01": expiredDecision},
		}
		srv := newAdmissionServer(t, fake)

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions/adm_01", ""), testToken))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var view AdmissionDetailView
		require.NoError(t, json.Unmarshal(decodeAdmissionEnvelope(t, rec).Data, &view))
		require.Equal(t, string(core.AdmissionExpired), view.State)
	})

	t.Run("候选不存在 404", func(t *testing.T) {
		srv := newAdmissionServer(t, &admissionFake{})

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions/adm_missing", ""), testToken))
		require.Equal(t, http.StatusNotFound, rec.Code)
		env := decodeAdmissionEnvelope(t, rec)
		require.Equal(t, codeNotFound, env.Code)
		require.Equal(t, admissionNotFoundPrefix+"adm_missing", env.Message)
		require.Empty(t, env.Data)
	})

	t.Run("清单损坏按内部错误处理", func(t *testing.T) {
		broken := candidate
		broken.ManifestJSON = `not-json`
		fake := &admissionFake{
			candidates: map[string]core.AdmissionCandidate{"adm_01": broken},
		}
		srv := newAdmissionServer(t, fake)

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions/adm_01", ""), testToken))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		env := decodeAdmissionEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
		require.NotContains(t, rec.Body.String(), "not-json")
	})

	t.Run("来源损坏按内部错误处理", func(t *testing.T) {
		broken := candidate
		broken.SourceJSON = `{"kind":"bogus"}`
		fake := &admissionFake{
			candidates: map[string]core.AdmissionCandidate{"adm_01": broken},
		}
		srv := newAdmissionServer(t, fake)

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions/adm_01", ""), testToken))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, messageInternal, decodeAdmissionEnvelope(t, rec).Message)
	})

	t.Run("评估读取失败固定 500", func(t *testing.T) {
		fake := &admissionFake{
			candidates:    map[string]core.AdmissionCandidate{"adm_01": candidate},
			evaluationErr: errors.New("boom: admission_evaluations"),
		}
		srv := newAdmissionServer(t, fake)

		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, "/api/v1/admissions/adm_01", ""), testToken))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, messageInternal, decodeAdmissionEnvelope(t, rec).Message)
		require.NotContains(t, rec.Body.String(), "boom")
	})
}

// TestUS21_Admissions_MissingPort 钉住装配缺端口的固定 500：两条路由都不得降级为
// 空列表或伪造详情——空列表会被读成「确实没有候选」。
// 逆改点：缺端口时写 200 + [] 或 404 → 对应断言红。
func TestUS21_Admissions_MissingPort(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })

	for _, path := range []string{"/api/v1/admissions", "/api/v1/admissions/adm_x"} {
		rec := serveAdmin(srv, withBearer(adminRequest(http.MethodGet, path, ""), testToken))
		require.Equal(t, http.StatusInternalServerError, rec.Code, path)
		env := decodeAdmissionEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code, path)
		require.Equal(t, messageInternal, env.Message, path)
		require.Empty(t, env.Data, "%s：缺端口不得回空列表或伪造详情", path)
	}
}

// admissionConsoleNotice 是颗粒度文档 §3.1.12 的逐字提示（页面顶部固定展示）。
// admissionConsoleNotice is the module card's literal banner text.
const admissionConsoleNotice = "批准 / 发布 / 撤回只在本机 CLI 执行"

// TestUS21_ConsoleReadOnly 钉住 C-14 的管理台只读面（§3.1.12 / §4.4）：
//  1. 准入列表与详情源码没有任何按钮控件——批准 / 发布 / 撤回只在本机 CLI；
//  2. 顶部固定提示逐字存在，详情页复用同一个常量；
//  3. `api.ts` 的 `/admissions` 调用只有 GET（管理面没有写端点）；
//  4. 随二进制发布的 dist 产物不写浏览器存储，且确实包含准入页（dist 未重建即红）。
//
// 逆改点：任一准入页渲染 `h('button'...)` → ① 红；删掉固定提示 → ② 红；把
// `getAdmissions` 改成 POST → ③ 红；产物出现 `localStorage.setItem` 或没重建 →
// ④ 红。判别力：③ 同时要求至少一处匹配，避免"没有调用"让断言空转。
func TestUS21_ConsoleReadOnly(t *testing.T) {
	files := consoleSourceFiles(t)

	for _, name := range []string{"views/admissions.ts", "views/admission-detail.ts"} {
		source, ok := files[name]
		require.True(t, ok, "缺少前端源码：%s", name)
		require.NotContains(t, source, "<button", "%s：只读页不得渲染按钮控件", name)
		require.NotContains(t, source, "h('button", "%s：只读页不得渲染按钮控件", name)
		require.NotContains(t, source, `h("button"`, "%s：只读页不得渲染按钮控件", name)
	}
	require.Contains(t, files["views/admissions.ts"], admissionConsoleNotice,
		"列表页缺少固定提示「%s」", admissionConsoleNotice)
	require.Contains(t, files["views/admission-detail.ts"], "ADMISSION_CLI_ONLY_NOTICE",
		"详情页必须复用同一个固定提示常量")

	api := files["api.ts"]
	require.NotEmpty(t, api, "缺少前端源码：api.ts")
	admissionCall := regexp.MustCompile("['\"`](GET|POST|PUT|PATCH|DELETE)['\"`][^\n]*/admissions")
	matches := admissionCall.FindAllStringSubmatch(api, -1)
	require.NotEmpty(t, matches, "api.ts 里没有 /admissions 调用：断言失去判别力")
	for _, match := range matches {
		require.Equal(t, http.MethodGet, match[1], "准入调用只能是 GET：%s", match[0])
	}

	dist := assets.Console()
	entries, err := fs.ReadDir(dist, "assets")
	require.NoError(t, err)
	require.NotEmpty(t, entries, "dist/assets 为空：前端构建产物缺失")
	noticeShipped := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".js") {
			continue
		}
		raw, readErr := fs.ReadFile(dist, "assets/"+entry.Name())
		require.NoError(t, readErr)
		text := string(raw)
		require.NotContains(t, text, "localStorage.setItem", "%s：token 不得写入浏览器存储", entry.Name())
		require.NotContains(t, text, "sessionStorage.setItem", "%s：token 不得写入浏览器存储", entry.Name())
		require.NotContains(t, text, "document.cookie", "%s：token 不得写入 cookie", entry.Name())
		if strings.Contains(text, admissionConsoleNotice) {
			noticeShipped = true
		}
	}
	require.True(t, noticeShipped, "管理台产物缺少准入页固定提示：dist 未重建？")
}

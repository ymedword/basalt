package web

// 本文件是学习准入只读面的契约对账（US-21 颗粒度文档 §3.1.13 / FR-21-12）：
// 两条 GET 路径与路由表双向对账、状态 / 风险档枚举与 Go 字面量逐字相等、
// `/operations` 的 target_kind 查询枚举与 OperationView 同集，以及三种真实投影
// （列表行、完整详情、子记录全为 null 的详情）整对象过契约 schema。
// Acceptance for the admission contract: the two read-only paths reconcile with the
// route table, the enums equal the Go literals, and the real projections validate
// against the compiled OpenAPI schemas.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// admissionContractDetail 走真实 GET 端点取详情投影：契约校验必须对着 handler 的
// 输出，而不是另写一份手搓对象——后者会漏掉投影函数与契约同时漂移的情况。
// admissionContractDetail fetches the detail through the real handler so the schema
// check sees the projection rather than a hand-written stand-in.
func admissionContractDetail(t *testing.T, candidate core.AdmissionCandidate,
	evaluation *core.AdmissionEvaluation, decision *core.AdmissionDecision,
	publication *core.AdmissionPublication,
) AdmissionDetailView {
	t.Helper()
	fake := &admissionFake{
		candidates:   map[string]core.AdmissionCandidate{candidate.CandidateID: candidate},
		evaluations:  map[string]core.AdmissionEvaluation{},
		decisions:    map[string]core.AdmissionDecision{},
		publications: map[string]core.AdmissionPublication{},
	}
	if evaluation != nil {
		fake.evaluations[candidate.CandidateID] = *evaluation
	}
	if decision != nil {
		fake.decisions[candidate.CandidateID] = *decision
	}
	if publication != nil {
		fake.publications[candidate.CandidateID] = *publication
	}
	srv := newAdmissionServer(t, fake)
	rec := serveAdmin(srv, withBearer(
		adminRequest(http.MethodGet, "/api/v1/admissions/"+candidate.CandidateID, ""), testToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view AdmissionDetailView
	require.NoError(t, json.Unmarshal(decodeAdmissionEnvelope(t, rec).Data, &view))
	return view
}

// TestUS21_OpenAPIContract 钉住学习准入只读面的契约（模块卡 §3.1.13 / FR-21-12）：
// 两条 GET 路径与路由表双向对账（写方法既没有契约路径，也没有注册行）；列表状态枚举
// 等于状态机八个字面量（含派生 expired）；风险档枚举等于 unknown / normal / high；
// `/operations` 的 target_kind 查询枚举与 OperationView 同集且含 skill_admission；
// 三种真实投影（列表行、完整详情、子记录全为 null 的详情）逐字段过契约 schema。
// 逆改点：漏写路径 / 契约多一个写方法 / 枚举少一个状态 / 视图多投影一个字段（把
// 持久结构直接序列化）/ 时间字段缺 `""` 分支 / 查询枚举漏 skill_admission → 红。
// 中文用例名：学习准入只读面的 openapi 契约对账。
func TestUS21_OpenAPIContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")

	t.Run("路径与路由表双向对账", func(t *testing.T) {
		paths, ok := mustDig(t, doc, "paths").(map[string]any)
		require.True(t, ok, "paths 不是映射")
		for _, rel := range []string{"/admissions", "/admissions/{candidate_id}"} {
			item, ok := paths[rel].(map[string]any)
			require.Truef(t, ok, "openapi.yaml 缺少准入路径 %s", rel)
			_, hasGet := item["get"]
			require.Truef(t, hasGet, "%s 必须声明 GET", rel)
			// 读面只允许 GET：路径项上的其他键只能是元数据，任何第二个方法都红。
			for key := range item {
				switch key {
				case "get", "parameters", "summary", "description", "servers", "$ref":
				default:
					require.Failf(t, "准入路径声明了多余方法", "%s 只允许 GET，发现 %q", rel, key)
				}
			}
		}

		registered := map[string]bool{}
		for _, rt := range newTestServer(t, nil).routeTable() {
			if !strings.HasPrefix(rt.pattern, "/api/v1/admissions") {
				continue
			}
			rel := strings.TrimPrefix(rt.pattern, "/api/v1")
			item, ok := paths[rel].(map[string]any)
			require.Truef(t, ok, "路由 %s %s 在契约里没有对应路径", rt.method, rt.pattern)
			_, hasMethod := item[strings.ToLower(rt.method)]
			require.Truef(t, hasMethod, "路由 %s %s 在契约里没有对应方法", rt.method, rt.pattern)
			registered[rt.method+" "+rel] = true
		}
		require.Equal(t, map[string]bool{
			"GET /admissions":                true,
			"GET /admissions/{candidate_id}": true,
		}, registered, "路由表的准入行必须恰好是契约里的两条 GET")
	})

	t.Run("状态与风险档枚举等于 Go 字面量", func(t *testing.T) {
		states := []string{
			string(core.AdmissionProposed), string(core.AdmissionEvaluated),
			string(core.AdmissionApproved), string(core.AdmissionRejected),
			string(core.AdmissionCanceled), string(core.AdmissionPublished),
			string(core.AdmissionWithdrawn), string(core.AdmissionExpired),
			"pending", "", "APPROVED",
		}
		require.Equal(t,
			acceptedBy(states, validAdmissionState),
			contractEnum(t, doc, "AdmissionListView", "state"),
			"AdmissionListView.state 必须逐字等于状态机八个字面量（含派生 expired）")

		riskTiers := []string{
			string(core.AdmissionRiskUnknown), string(core.AdmissionRiskNormal),
			string(core.AdmissionRiskHigh), "critical", "",
		}
		require.Equal(t,
			acceptedBy(riskTiers, func(value string) bool {
				switch core.AdmissionRiskTier(value) {
				case core.AdmissionRiskUnknown, core.AdmissionRiskNormal, core.AdmissionRiskHigh:
					return true
				default:
					return false
				}
			}),
			contractEnum(t, doc, "AdmissionListView", "risk_tier"),
			"AdmissionListView.risk_tier 必须等于 unknown / normal / high")
	})

	t.Run("操作查询的 target_kind 枚举含新目标类型", func(t *testing.T) {
		operationEnum := contractEnum(t, doc, "OperationView", "target_kind")
		require.Contains(t, operationEnum, core.TargetKindSkillAdmission)

		params, ok := mustDig(t, doc, "paths", "/operations", "get", "parameters").([]any)
		require.True(t, ok, "/operations 的 parameters 不是数组")
		queryKinds := make([]string, 0, len(operationEnum))
		for _, raw := range params {
			entry, ok := raw.(map[string]any)
			require.True(t, ok, "参数项不是映射")
			if entry["name"] != "target_kind" || entry["in"] != "query" {
				continue
			}
			schema, ok := entry["schema"].(map[string]any)
			require.True(t, ok, "target_kind 的 schema 不是映射")
			values, ok := schema["enum"].([]any)
			require.True(t, ok, "target_kind 的 enum 不是数组")
			for _, value := range values {
				text, ok := value.(string)
				require.True(t, ok, "target_kind 枚举值不是字符串")
				queryKinds = append(queryKinds, text)
			}
		}
		require.ElementsMatch(t, operationEnum, queryKinds,
			"/operations 的 target_kind 查询枚举必须与 OperationView 同集")
	})

	t.Run("列表行过契约 schema", func(t *testing.T) {
		candidate := admissionTestCandidate("adm_01", "weekly-report", core.AdmissionEvaluated)
		requireViewMatchesSchema(t, doc, "AdmissionListView", admissionListView(candidate))
	})

	t.Run("完整详情过契约 schema", func(t *testing.T) {
		candidate := admissionTestCandidate("adm_01", "weekly-report", core.AdmissionPublished)
		evaluation := core.AdmissionEvaluation{
			EvaluationID:      "eval_01",
			CandidateID:       candidate.CandidateID,
			CandidateRevision: 1,
			CandidateHash:     candidate.CandidateHash,
			EvaluationHash:    strings.Repeat("e", 64),
			GraderVersion:     "admission-eval-v1",
			PolicyHash:        strings.Repeat("f", 64),
			Dimensions: []core.AdmissionDimension{
				{Name: core.AdmissionDimensionContent, Status: core.AdmissionDimensionPassed},
				{
					Name:   core.AdmissionDimensionEffect,
					Status: core.AdmissionDimensionNotEvaluable,
					Reason: "real_model_not_authorized",
				},
			},
			Scripts:     []string{"scripts/report.py"},
			Aggregate:   core.AdmissionAggregateIncomplete,
			RiskTier:    core.AdmissionRiskNormal,
			Referrers:   []string{"ops-agent"},
			EvaluatedAt: admissionWebNow.Add(-30 * time.Minute),
		}
		decision := core.AdmissionDecision{
			DecisionID:        "dec_01",
			CandidateID:       candidate.CandidateID,
			CandidateRevision: 1,
			CandidateHash:     candidate.CandidateHash,
			EvaluationID:      evaluation.EvaluationID,
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
			CandidateID: candidate.CandidateID,
			DecisionID:  decision.DecisionID,
			SkillName:   candidate.SkillName,
			VersionID:   12,
			VersionNo:   3,
			ContentHash: strings.Repeat("1", 64),
			State:       core.AdmissionPublicationActive,
			PublishedAt: admissionWebNow.Add(-10 * time.Minute),
		}
		view := admissionContractDetail(t, candidate, &evaluation, &decision, &publication)
		requireViewMatchesSchema(t, doc, "AdmissionDetailView", view)
	})

	t.Run("reject 决策的空时间串过契约 schema", func(t *testing.T) {
		candidate := admissionTestCandidate("adm_02", "weekly-report", core.AdmissionRejected)
		evaluation := core.AdmissionEvaluation{
			EvaluationID:      "eval_02",
			CandidateID:       candidate.CandidateID,
			CandidateRevision: 2,
			CandidateHash:     candidate.CandidateHash,
			EvaluationHash:    strings.Repeat("e", 64),
			GraderVersion:     "admission-eval-v1",
			PolicyHash:        strings.Repeat("f", 64),
			Dimensions:        []core.AdmissionDimension{},
			Scripts:           []string{},
			Aggregate:         core.AdmissionAggregateFailed,
			RiskTier:          core.AdmissionRiskNormal,
			Referrers:         []string{},
			EvaluatedAt:       admissionWebNow.Add(-30 * time.Minute),
		}
		// reject 决策没有有效期：ExpiresAt 为零值，视图必须渲染空串并由契约的
		// `{ const: "" }` 分支接受——写死 date-time 会把合法的拒绝投影判红。
		decision := core.AdmissionDecision{
			DecisionID:        "dec_02",
			CandidateID:       candidate.CandidateID,
			CandidateRevision: 2,
			CandidateHash:     candidate.CandidateHash,
			EvaluationID:      evaluation.EvaluationID,
			EvaluationHash:    evaluation.EvaluationHash,
			Decision:          core.AdmissionReject,
			Reason:            "评估未通过",
			Acknowledged:      []core.AdmissionDimensionName{},
			RiskTier:          core.AdmissionRiskNormal,
			ApproverSubject:   "cli:bob",
			CreatedAt:         admissionWebNow.Add(-20 * time.Minute),
		}
		view := admissionContractDetail(t, candidate, &evaluation, &decision, nil)
		requireViewMatchesSchema(t, doc, "AdmissionDetailView", view)
	})

	t.Run("子记录全为 null 的详情过契约 schema", func(t *testing.T) {
		candidate := admissionTestCandidate("adm_03", "weekly-report", core.AdmissionProposed)
		view := admissionContractDetail(t, candidate, nil, nil, nil)
		require.Nil(t, view.Evaluation)
		require.Nil(t, view.Decision)
		require.Nil(t, view.Publication)
		requireViewMatchesSchema(t, doc, "AdmissionDetailView", view)
	})
}

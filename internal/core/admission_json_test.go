package core_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件覆盖 US-21 §3.1.2 的严格解析与哈希契约。逐条反例对应颗粒度文档
// §3.1.2 的两张字段约束表：未知键、体积、枚举、必填、绑定、review 与确认集合。
// Each rejection case maps to one row of the frozen field tables.

const (
	admissionTestHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	admissionTestHashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	admissionTestHashC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	admissionTestHashD = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	admissionTestHashE = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// admissionTestIDs 生成一对合法候选 / 评估标识，避免测试里手写易错的 Crockford 载荷。
// admissionTestIDs mints a candidate/evaluation id pair for fixtures.
func admissionTestIDs(t *testing.T) (string, string) {
	t.Helper()
	candidateID, err := core.NewAdmissionCandidateID()
	require.NoError(t, err)
	evaluationID, err := core.NewAdmissionEvaluationID()
	require.NoError(t, err)
	return candidateID, evaluationID
}

// admissionTestSource 返回一份通过校验的最小来源声明。
// admissionTestSource is a minimal valid source declaration.
func admissionTestSource() map[string]any {
	return map[string]any{
		"kind":            "manual",
		"ref":             "internal notes 2026-11-04",
		"task_outcome":    "completed",
		"confirmation":    "direct",
		"authorization":   "declared",
		"redaction":       "declared",
		"retention_until": "2027-01-01T00:00:00Z",
		"applicable":      true,
		"counterexamples": []string{"works only for the demo tenant"},
		"evidence":        []string{"run log id 42"},
		"limitations":     []string{"no network access"},
		"notes":           "collected by hand",
	}
}

func admissionEncode(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func TestUS21_Source_StrictJSON_Accepts(t *testing.T) {
	t.Parallel()

	source, err := core.ParseAdmissionSource(admissionEncode(t, admissionTestSource()))
	require.NoError(t, err)
	assert.Equal(t, "manual", source.Kind)
	assert.Equal(t, "internal notes 2026-11-04", source.Ref)
	assert.Equal(t, "completed", source.TaskOutcome)
	assert.Equal(t, "direct", source.Confirmation)
	assert.Equal(t, "declared", source.Authorization)
	assert.Equal(t, "declared", source.Redaction)
	assert.Equal(t, "2027-01-01T00:00:00Z", source.RetentionUntil)
	assert.True(t, source.Applicable)
	assert.Equal(t, []string{"run log id 42"}, source.Evidence)
}

// 逆改点：把任一必填枚举改回"缺省即接受"→ 对应子例红（缺字段被当合法值）。
func TestUS21_Source_StrictJSON_RejectsBoundary(t *testing.T) {
	t.Parallel()

	longItem := strings.Repeat("x", 501)
	tooMany := make([]string, 21)
	for i := range tooMany {
		tooMany[i] = "item"
	}

	cases := map[string]func(doc map[string]any){
		"unknown key":          func(doc map[string]any) { doc["approver"] = "someone" },
		"missing applicable":   func(doc map[string]any) { delete(doc, "applicable") },
		"unknown kind":         func(doc map[string]any) { doc["kind"] = "scraped" },
		"empty ref":            func(doc map[string]any) { doc["ref"] = "" },
		"ref too long":         func(doc map[string]any) { doc["ref"] = strings.Repeat("r", 501) },
		"unknown confirmation": func(doc map[string]any) { doc["confirmation"] = "assumed" },
		"bad retention":        func(doc map[string]any) { doc["retention_until"] = "2026-11-04" },
		"notes too long":       func(doc map[string]any) { doc["notes"] = strings.Repeat("n", 2001) },
		"too many evidence":    func(doc map[string]any) { doc["evidence"] = tooMany },
		"evidence item long":   func(doc map[string]any) { doc["evidence"] = []string{longItem} },
		"counterexample long":  func(doc map[string]any) { doc["counterexamples"] = []string{longItem} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := admissionTestSource()
			mutate(doc)
			_, err := core.ParseAdmissionSource(admissionEncode(t, doc))
			require.Error(t, err)
			assert.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
		})
	}
}

func TestUS21_Source_StrictJSON_RejectsMalformed(t *testing.T) {
	t.Parallel()

	t.Run("root is array", func(t *testing.T) {
		_, err := core.ParseAdmissionSource([]byte(`[]`))
		assert.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
	})
	t.Run("trailing document", func(t *testing.T) {
		raw := append(admissionEncode(t, admissionTestSource()), []byte(` {}`)...)
		_, err := core.ParseAdmissionSource(raw)
		assert.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
	})
	t.Run("oversized", func(t *testing.T) {
		doc := admissionTestSource()
		doc["notes"] = strings.Repeat("n", 70000)
		_, err := core.ParseAdmissionSource(admissionEncode(t, doc))
		assert.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
	})
	t.Run("invalid utf8", func(t *testing.T) {
		_, err := core.ParseAdmissionSource([]byte{'{', '"', 'a', '"', ':', '"', 0xff, '"', '}'})
		assert.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
	})
}

// admissionTestDecision 返回一份通过校验的 approve 决策文档。
// admissionTestDecision is a valid approve decision document.
func admissionTestDecision(candidateID, evaluationID, resourceUID string) map[string]any {
	return map[string]any{
		"candidate_id":            candidateID,
		"candidate_revision":      2,
		"candidate_hash":          admissionTestHashA,
		"evaluation_id":           evaluationID,
		"evaluation_hash":         admissionTestHashB,
		"decision":                "approve",
		"reason":                  "reviewed locally on the admin host",
		"acknowledged_dimensions": []string{"effect", "cost"},
		"approved_scope": map[string]any{
			"skill":     "daily-report",
			"risk_tier": "normal",
			"files": []map[string]any{
				{"path": "SKILL.md", "sha256": admissionTestHashC, "size": 12},
			},
			"referrers": []string{"agent-b", "agent-a"},
		},
		"script_review": []map[string]any{},
		"baseline": map[string]any{
			"skill_state":          "present",
			"resource_uid":         resourceUID,
			"published_version_id": 3,
			"version_no":           2,
			"content_hash":         admissionTestHashD,
			"pointer_revision":     5,
			"referrers":            []string{"agent-b", "agent-a"},
			"referrers_hash":       admissionTestHashE,
		},
		"expires_at": "2026-11-05T00:00:00Z",
	}
}

func admissionTestResourceUID(t *testing.T) string {
	t.Helper()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	return string(uid)
}

func TestUS21_Decision_StrictJSON_AcceptsApprove(t *testing.T) {
	t.Parallel()

	candidateID, evaluationID := admissionTestIDs(t)
	raw := admissionEncode(t, admissionTestDecision(candidateID, evaluationID, admissionTestResourceUID(t)))
	input, err := core.ParseAdmissionDecision(raw)
	require.NoError(t, err)
	assert.Equal(t, candidateID, input.CandidateID)
	assert.Equal(t, int64(2), input.CandidateRevision)
	assert.Equal(t, core.AdmissionApprove, input.Decision)
	assert.Equal(t, []core.AdmissionDimensionName{"effect", "cost"}, input.Acknowledged)
	require.NotNil(t, input.ApprovedScope)
	// 文件与引用者按固定序规范化，批准范围 JSON 才能可复算。
	assert.Equal(t, []string{"agent-a", "agent-b"}, input.ApprovedScope.Referrers)
	require.NotNil(t, input.Baseline)
	assert.NotEmpty(t, input.Baseline.CanonicalJSON)
	assert.Equal(t, []string{"agent-a", "agent-b"}, input.Baseline.Referrers)
	assert.True(t, input.ExpiresAt.Equal(time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)))
}

func TestUS21_Decision_StrictJSON_AcceptsReject(t *testing.T) {
	t.Parallel()

	candidateID, evaluationID := admissionTestIDs(t)
	doc := map[string]any{
		"candidate_id":       candidateID,
		"candidate_revision": int64(3),
		"candidate_hash":     admissionTestHashA,
		"evaluation_id":      evaluationID,
		"evaluation_hash":    admissionTestHashB,
		"decision":           "reject",
		"reason":             "source confirmation is relayed",
	}
	input, err := core.ParseAdmissionDecision(admissionEncode(t, doc))
	require.NoError(t, err)
	assert.Equal(t, core.AdmissionReject, input.Decision)
	assert.Nil(t, input.ApprovedScope)
	assert.Nil(t, input.Baseline)
	assert.True(t, input.ExpiresAt.IsZero())
}

// 逆改点：reject 分支不再拒绝 approved_scope / baseline / script_review → «reject with scope» 等子例红。
func TestUS21_Decision_StrictJSON_RejectsBoundary(t *testing.T) {
	t.Parallel()

	cases := map[string]func(doc map[string]any){
		"unknown key":           func(doc map[string]any) { doc["approver"] = "someone" },
		"missing revision":      func(doc map[string]any) { delete(doc, "candidate_revision") },
		"zero revision":         func(doc map[string]any) { doc["candidate_revision"] = int64(0) },
		"candidate id prefix":   func(doc map[string]any) { doc["candidate_id"] = "rs_00000000000000000000000000" },
		"candidate hash case":   func(doc map[string]any) { doc["candidate_hash"] = strings.ToUpper(admissionTestHashA) },
		"evaluation hash short": func(doc map[string]any) { doc["evaluation_hash"] = "abc" },
		"unknown decision":      func(doc map[string]any) { doc["decision"] = "maybe" },
		"empty reason":          func(doc map[string]any) { doc["reason"] = "" },
		"reason too long":       func(doc map[string]any) { doc["reason"] = strings.Repeat("r", 2001) },
		"unknown ack dimension": func(doc map[string]any) { doc["acknowledged_dimensions"] = []string{"quality"} },
		"duplicate ack":         func(doc map[string]any) { doc["acknowledged_dimensions"] = []string{"effect", "effect"} },
		"missing scope":         func(doc map[string]any) { delete(doc, "approved_scope") },
		"missing baseline":      func(doc map[string]any) { delete(doc, "baseline") },
		"missing expires_at":    func(doc map[string]any) { delete(doc, "expires_at") },
		"bad expires_at":        func(doc map[string]any) { doc["expires_at"] = "tomorrow" },
		"scope unknown key": func(doc map[string]any) {
			doc["approved_scope"].(map[string]any)["owner"] = "ops"
		},
		"scope risk unknown": func(doc map[string]any) {
			doc["approved_scope"].(map[string]any)["risk_tier"] = "unknown"
		},
		"scope empty skill": func(doc map[string]any) {
			doc["approved_scope"].(map[string]any)["skill"] = ""
		},
		"scope bad digest": func(doc map[string]any) {
			doc["approved_scope"].(map[string]any)["files"] = []map[string]any{
				{"path": "SKILL.md", "sha256": "xyz", "size": 1},
			}
		},
		"scope duplicate file": func(doc map[string]any) {
			doc["approved_scope"].(map[string]any)["files"] = []map[string]any{
				{"path": "SKILL.md", "sha256": admissionTestHashC, "size": 1},
				{"path": "SKILL.md", "sha256": admissionTestHashD, "size": 2},
			}
		},
		"baseline bad state": func(doc map[string]any) {
			doc["baseline"].(map[string]any)["skill_state"] = "unknown"
		},
		"baseline bad uid": func(doc map[string]any) {
			doc["baseline"].(map[string]any)["resource_uid"] = "rs_short"
		},
		"review empty evidence": func(doc map[string]any) {
			doc["script_review"] = []map[string]any{{"path": "scripts/run.sh", "evidence": ""}}
		},
		"review duplicate path": func(doc map[string]any) {
			doc["script_review"] = []map[string]any{
				{"path": "scripts/run.sh", "evidence": "checked"},
				{"path": "scripts/run.sh", "evidence": "checked again"},
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidateID, evaluationID := admissionTestIDs(t)
			doc := admissionTestDecision(candidateID, evaluationID, admissionTestResourceUID(t))
			mutate(doc)
			_, err := core.ParseAdmissionDecision(admissionEncode(t, doc))
			require.Error(t, err)
			assert.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
		})
	}
}

func TestUS21_Decision_StrictJSON_RejectHasNoApprovalFields(t *testing.T) {
	t.Parallel()

	for name, field := range map[string]string{
		"approved_scope": `{"skill":"x","risk_tier":"normal","files":[],"referrers":[]}`,
		"baseline":       `{"skill_state":"absent","resource_uid":"","published_version_id":0,"version_no":0,"content_hash":"","pointer_revision":0,"referrers":[],"referrers_hash":""}`,
		"script_review":  `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			candidateID, evaluationID := admissionTestIDs(t)
			raw := fmt.Sprintf(
				`{"candidate_id":%q,"candidate_revision":1,"candidate_hash":%q,`+
					`"evaluation_id":%q,"evaluation_hash":%q,"decision":"reject","reason":"no","%s":%s}`,
				candidateID, admissionTestHashA, evaluationID, admissionTestHashB, name, field)
			_, err := core.ParseAdmissionDecision([]byte(raw))
			require.Error(t, err)
			assert.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
		})
	}
}

func TestUS21_Manifest_RequiresCanonicalOrder(t *testing.T) {
	t.Parallel()

	valid := fmt.Sprintf(`[{"path":"SKILL.md","sha256":%q,"size":3},{"path":"docs/a.md","sha256":%q,"size":1}]`,
		admissionTestHashA, admissionTestHashB)
	digests, err := core.ParseAdmissionManifest(valid)
	require.NoError(t, err)
	require.Len(t, digests, 2)
	assert.Equal(t, "SKILL.md", digests[0].Path)

	for name, raw := range map[string]string{
		"descending": fmt.Sprintf(`[{"path":"docs/a.md","sha256":%q,"size":1},{"path":"SKILL.md","sha256":%q,"size":3}]`,
			admissionTestHashB, admissionTestHashA),
		"duplicate": fmt.Sprintf(`[{"path":"SKILL.md","sha256":%q,"size":1},{"path":"SKILL.md","sha256":%q,"size":3}]`,
			admissionTestHashB, admissionTestHashA),
		"empty path":  fmt.Sprintf(`[{"path":"","sha256":%q,"size":1}]`, admissionTestHashA),
		"bad digest":  `[{"path":"SKILL.md","sha256":"nope","size":1}]`,
		"negative":    fmt.Sprintf(`[{"path":"SKILL.md","sha256":%q,"size":-1}]`, admissionTestHashA),
		"root object": fmt.Sprintf(`{"path":"SKILL.md","sha256":%q,"size":1}`, admissionTestHashA),
		"unknown key": fmt.Sprintf(`[{"path":"SKILL.md","sha256":%q,"size":1,"mode":"755"}]`, admissionTestHashA),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := core.ParseAdmissionManifest(raw)
			require.Error(t, err)
			assert.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
		})
	}
}

// 契约固定：nil 必须序列化为 `[]`，approve 无脚本时决策行的 NOT NULL 约束才成立。
// The contract freezes nil → "[]" so the approve row satisfies its NOT NULL check.
func TestUS21_ScriptReviewJSON_NilIsEmptyArray(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "[]", core.AdmissionScriptReviewJSON(nil))
	assert.Equal(t, "[]", core.AdmissionScriptReviewJSON([]core.AdmissionScriptReview{}))

	unsorted := []core.AdmissionScriptReview{
		{Path: "scripts/z.sh", Evidence: "z"},
		{Path: "scripts/a.sh", Evidence: "a"},
	}
	assert.Equal(t, `[{"path":"scripts/a.sh","evidence":"a"},{"path":"scripts/z.sh","evidence":"z"}]`,
		core.AdmissionScriptReviewJSON(unsorted))
	assert.Equal(t, core.AdmissionScriptReviewJSON(unsorted),
		core.AdmissionScriptReviewJSON([]core.AdmissionScriptReview{unsorted[1], unsorted[0]}),
		"同一集合的不同输入顺序必须产出相同字节")
}

func TestUS21_ApprovedScopeJSON_Deterministic(t *testing.T) {
	t.Parallel()

	assert.Empty(t, core.AdmissionApprovedScopeJSON(nil))
	scope := &core.AdmissionApprovedScope{
		SkillName: "daily-report",
		RiskTier:  core.AdmissionRiskNormal,
		Files: []core.AdmissionFileDigest{
			{Path: "docs/b.md", SHA256: admissionTestHashB, Size: 2},
			{Path: "SKILL.md", SHA256: admissionTestHashA, Size: 1},
		},
		Referrers: []string{"agent-b", "agent-a"},
	}
	shuffled := &core.AdmissionApprovedScope{
		SkillName: scope.SkillName,
		RiskTier:  scope.RiskTier,
		Files:     []core.AdmissionFileDigest{scope.Files[1], scope.Files[0]},
		Referrers: []string{"agent-a", "agent-b"},
	}
	assert.Equal(t, core.AdmissionApprovedScopeJSON(scope), core.AdmissionApprovedScopeJSON(shuffled))
	assert.Contains(t, core.AdmissionApprovedScopeJSON(scope), `"skill":"daily-report"`)
}

func TestUS21_BaselineCanonical_IgnoresDerivedField(t *testing.T) {
	t.Parallel()

	baseline := core.AdmissionBaseline{
		SkillState:         core.AdmissionSkillAbsent,
		ResourceUID:        "",
		PublishedVersionID: 0,
		VersionNo:          0,
		ContentHash:        "",
		PointerRevision:    0,
		Referrers:          []string{"agent-b", "agent-a"},
		ReferrersHash:      admissionTestHashE,
	}
	canonical := core.AdmissionBaselineCanonical(baseline)
	assert.Contains(t, canonical, `"referrers":["agent-a","agent-b"]`)

	baseline.CanonicalJSON = "tampered"
	assert.Equal(t, canonical, core.AdmissionBaselineCanonical(baseline),
		"派生字段不得进入自身的哈希输入")
}

func TestUS21_EvaluationHash_BindsContentAndNormalisesTime(t *testing.T) {
	t.Parallel()

	base := core.AdmissionEvaluation{
		EvaluationID:      "eval_00000000000000000000000000",
		CandidateID:       "adm_00000000000000000000000000",
		CandidateRevision: 1,
		CandidateHash:     admissionTestHashA,
		GraderVersion:     "us21-v1",
		PolicyHash:        admissionTestHashB,
		Dimensions: []core.AdmissionDimension{
			{Name: core.AdmissionDimensionCost, Status: core.AdmissionDimensionNotEvaluable},
			{Name: core.AdmissionDimensionContent, Status: core.AdmissionDimensionPassed},
		},
		Scripts:      []string{"scripts/b.sh", "scripts/a.sh"},
		Aggregate:    core.AdmissionAggregateIncomplete,
		RiskTier:     core.AdmissionRiskHigh,
		Referrers:    []string{"agent-b", "agent-a"},
		BaselineJSON: `{"skill_state":"absent"}`,
		EvaluatedAt:  time.Date(2026, 11, 4, 10, 0, 0, 123456789, time.UTC),
		OperationID:  "op_00000000000000000000000000",
	}
	first := core.AdmissionEvaluationHash(base)
	assert.Equal(t, first, core.AdmissionEvaluationHash(base), "同一评估两次读数必须逐字相同")

	// 数据库只保留毫秒：同毫秒内的纳秒差不得改变哈希。
	shifted := base
	shifted.EvaluatedAt = base.EvaluatedAt.Add(400 * time.Nanosecond)
	assert.Equal(t, first, core.AdmissionEvaluationHash(shifted))

	reordered := base
	reordered.Dimensions = []core.AdmissionDimension{base.Dimensions[1], base.Dimensions[0]}
	reordered.Scripts = []string{"scripts/a.sh", "scripts/b.sh"}
	reordered.Referrers = []string{"agent-a", "agent-b"}
	assert.Equal(t, first, core.AdmissionEvaluationHash(reordered), "排序归一后必须同哈希")

	failed := base
	failed.Dimensions = []core.AdmissionDimension{
		{Name: core.AdmissionDimensionCost, Status: core.AdmissionDimensionNotEvaluable},
		{Name: core.AdmissionDimensionContent, Status: core.AdmissionDimensionFailed},
	}
	assert.NotEqual(t, first, core.AdmissionEvaluationHash(failed), "维度判定变化必须改哈希")
}

func TestUS21_RequestHash_SeparatesElements(t *testing.T) {
	t.Parallel()

	assert.Equal(t, core.AdmissionRequestHash("ab", "c"), core.AdmissionRequestHash("ab", "c"))
	assert.NotEqual(t, core.AdmissionRequestHash("ab", "c"), core.AdmissionRequestHash("a", "bc"),
		"拼接歧义必须被长度编码消除")
	assert.Len(t, core.AdmissionRequestHash("candidate"), 64)
	assert.Len(t, core.AdmissionSHA256Hex([]byte("x")), 64)
}

// 模板必须让"没真正填写"的决策文件无法通过解析：reason / expires_at / 审查证据留空。
// The template must not parse successfully until the blanks are filled.
func TestUS21_DecisionTemplate_PrefillsAndBlanks(t *testing.T) {
	t.Parallel()

	candidateID, evaluationID := admissionTestIDs(t)
	baseline := core.AdmissionBaseline{
		SkillState:         core.AdmissionSkillPresent,
		ResourceUID:        core.ResourceUID(admissionTestResourceUID(t)),
		PublishedVersionID: 3,
		VersionNo:          2,
		ContentHash:        admissionTestHashD,
		PointerRevision:    5,
		Referrers:          []string{"agent-a"},
		ReferrersHash:      admissionTestHashE,
	}
	baseline.CanonicalJSON = core.AdmissionBaselineCanonical(baseline)
	evaluation := &core.AdmissionEvaluation{
		EvaluationID:      evaluationID,
		CandidateID:       candidateID,
		CandidateRevision: 1,
		CandidateHash:     admissionTestHashA,
		EvaluationHash:    admissionTestHashB,
		GraderVersion:     "us21-v1",
		PolicyHash:        admissionTestHashC,
		Dimensions: []core.AdmissionDimension{
			{Name: core.AdmissionDimensionContent, Status: core.AdmissionDimensionPassed},
			{Name: core.AdmissionDimensionEffect, Status: core.AdmissionDimensionNotEvaluable},
			{Name: core.AdmissionDimensionCost, Status: core.AdmissionDimensionNotEvaluable},
		},
		Scripts:      []string{"scripts/run.sh"},
		Aggregate:    core.AdmissionAggregateIncomplete,
		RiskTier:     core.AdmissionRiskNormal,
		Referrers:    []string{"agent-a"},
		BaselineJSON: baseline.CanonicalJSON,
	}
	detail := core.AdmissionDetail{
		Candidate: core.AdmissionCandidate{
			CandidateID:   candidateID,
			SkillName:     "daily-report",
			Revision:      1,
			CandidateHash: admissionTestHashA,
			ManifestJSON: fmt.Sprintf(
				`[{"path":"SKILL.md","sha256":%q,"size":12}]`, admissionTestHashC),
		},
		Evaluation: evaluation,
	}
	raw, err := core.AdmissionDecisionTemplate(detail)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &decoded))
	assert.Equal(t, "approve", decoded["decision"])
	assert.Equal(t, "", decoded["reason"])
	assert.Equal(t, "", decoded["expires_at"])
	assert.Equal(t, []any{"effect", "cost"}, decoded["acknowledged_dimensions"])

	scope := decoded["approved_scope"].(map[string]any)
	assert.Equal(t, "daily-report", scope["skill"])
	assert.Equal(t, "normal", scope["risk_tier"])

	reviews := decoded["script_review"].([]any)
	require.Len(t, reviews, 1)
	assert.Equal(t, "scripts/run.sh", reviews[0].(map[string]any)["path"])
	assert.Equal(t, "", reviews[0].(map[string]any)["evidence"])

	_, err = core.ParseAdmissionDecision([]byte(raw))
	require.Error(t, err, "未填写的模板不得直接可用")
	assert.ErrorIs(t, err, core.ErrAdmissionInvalidInput)

	// 填写留白后必须可解析，且基线逐字回到评估的规范 JSON（管理员不手抄哈希）。
	decoded["reason"] = "reviewed on the admin host"
	decoded["expires_at"] = "2026-11-05T00:00:00Z"
	reviews[0].(map[string]any)["evidence"] = "checked the script's permission surface"
	input, err := core.ParseAdmissionDecision(admissionEncode(t, decoded))
	require.NoError(t, err)
	require.NotNil(t, input.Baseline)
	assert.Equal(t, baseline.CanonicalJSON, input.Baseline.CanonicalJSON)

	_, err = core.AdmissionDecisionTemplate(core.AdmissionDetail{Candidate: detail.Candidate})
	assert.ErrorIs(t, err, core.ErrAdmissionInvalidState)
}

func TestUS21_State_Transitions(t *testing.T) {
	t.Parallel()

	for _, state := range []core.AdmissionState{
		core.AdmissionProposed, core.AdmissionEvaluated, core.AdmissionApproved,
	} {
		assert.True(t, state.CanEvaluate(), "%s 允许评估", state)
		assert.False(t, state.Terminal(), "%s 不是终态", state)
	}
	for _, state := range []core.AdmissionState{
		core.AdmissionRejected, core.AdmissionCanceled, core.AdmissionWithdrawn,
	} {
		assert.False(t, state.CanEvaluate(), "%s 不接受评估", state)
		assert.True(t, state.Terminal(), "%s 是终态", state)
	}
	assert.False(t, core.AdmissionPublished.CanEvaluate())
	assert.False(t, core.AdmissionPublished.Terminal())
	assert.False(t, core.AdmissionExpired.CanEvaluate(), "expired 是派生展示态，不接受评估")
}

func TestUS21_AdmissionIDs_RoundTrip(t *testing.T) {
	t.Parallel()

	candidateID, err := core.NewAdmissionCandidateID()
	require.NoError(t, err)
	assert.Len(t, candidateID, len("adm_")+26)
	assert.True(t, strings.HasPrefix(candidateID, "adm_"))

	evaluationID, err := core.NewAdmissionEvaluationID()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(evaluationID, "eval_"))

	decisionID, err := core.NewAdmissionDecisionID()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(decisionID, "dec_"))

	parsed, err := core.ParseAdmissionID("adm_", candidateID)
	require.NoError(t, err)
	assert.Equal(t, candidateID, parsed)

	_, err = core.ParseAdmissionID("adm_", evaluationID)
	require.Error(t, err)
	_, err = core.ParseAdmissionID("adm_", "adm_short")
	require.Error(t, err)
}

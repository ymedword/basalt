package app

// 本文件是 US-21 七维评估与策略 / 基线契约的验收面（颗粒度文档 §3.1.6、§4.2、T010）：
//
//	R1 `TestUS21_Evaluate_Dimensions`——七维状态与固定理由（effect / cost 的固定文案）
//	R2 `TestUS21_Evaluate_Aggregate`——任一 failed → failed；否则任一 not_evaluable →
//	   incomplete；同输入同结果
//	R3 `TestUS21_Evaluate_RiskTier`——绑定中的技能更新或含脚本 → high，其余 normal；
//	   批准侧重算与评估写入逐字一致
//	R4 `TestUS21_Evaluate_RetentionExpired`——now 到达 retention_until 即失败（绝对时刻）
//	R5 `TestUS21_Evaluate_NoProviderOrNetwork`——评估器源码不引入网络 / 模型 / 子进程
//	R6 `TestUS21_Evaluate_ScriptsNotExecuted`——含脚本候选只列名不执行
//	R7 `TestUS21_Policy_DomainTableRevisionChangesHash`——域名表 revision 与内容变化改哈希、
//	   行序不影响结果
//	R8 `TestUS21_Policy_AdmissionsDirDenied`——Agent 读写 `.basalt/admissions` 均被策略拒绝
//	R9 `TestUS21_NotEvaluable_IsNotPass`——聚合 incomplete 不是 passed（在
//	   `admission_approve_test.go`，`//go:build integration`：批准面需要真实数据库）
//	R10 `TestUS21_Evaluate_TrailingDotScript`——尾点文件名（`collect.`）视同无扩展名按脚本处理
//
// 判别力：把 effect / cost 改成 passed，R1 / R9 红；把 aggregate 的 not_evaluable 当
// passed，R2 / R9 红；风险档只按脚本判定，R3 的「present + referrers」子例红；哈希
// 只吃域名内容不看 revision，R7 的第二段断言红；把 admissions 加入可读根，R8 的读
// 断言由拒绝变放行。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// admissionEvalNow 是全部评估用例的固定时钟（UTC，无单调时钟分量）。
// admissionEvalNow is the fixed clock shared by the evaluation cases.
var admissionEvalNow = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

// admissionEvalSkillMD 造一份带 name / description 的合法 SKILL.md。
// admissionEvalSkillMD builds a valid SKILL.md with frontmatter.
func admissionEvalSkillMD(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n正文\n"
}

// admissionEvalFiles 造最小合法候选文件集（根 SKILL.md）。
// admissionEvalFiles returns the minimal valid candidate file set.
func admissionEvalFiles(skillName string) []core.AuthorFile {
	return []core.AuthorFile{{Path: "SKILL.md", Content: admissionEvalSkillMD(skillName, "周报生成")}}
}

// admissionEvalCandidate 用真实校验器冻结候选，确保哈希与文件集自洽。
// admissionEvalCandidate freezes the candidate through the real validator.
func admissionEvalCandidate(t *testing.T, skillName string, files []core.AuthorFile) core.AdmissionCandidate {
	t.Helper()
	draft, err := workspace.ValidateSkillDraft(skillName, files)
	require.NoError(t, err)
	return core.AdmissionCandidate{
		CandidateID:   "adm_eval_01",
		SkillName:     skillName,
		State:         core.AdmissionProposed,
		Revision:      1,
		CandidateHash: draft.ContentHash,
		ManifestJSON:  draft.ManifestJSON,
		SourceJSON:    "{}",
		SourceHash:    core.AdmissionSHA256Hex([]byte("{}")),
	}
}

// admissionEvalPolicy 造一份固定策略快照。
// admissionEvalPolicy builds a fixed policy snapshot.
func admissionEvalPolicy() core.AdmissionPolicy {
	return BuildAdmissionPolicy(core.AdmissionPolicyConfig{
		CrossAgentRead:  []string{"b", "a"},
		AllowedCommands: []string{"git", "go"},
		Interpreters:    []string{"python3"},
		AllowRunCode:    true,
	}, core.ExecutionPolicy{Profile: core.ProfileStandard, Revision: 2},
		[]core.NetworkDomainPolicy{
			{Scope: core.PolicyScopeHTTPDomains, Revision: 3, Domains: []string{"api.example.com"}},
			{Scope: core.PolicyScopeHTTPDeniedDomains, Revision: 1},
		}, nil)
}

// admissionEvalIsolationProfile 造一个带全部执行边界的隔离档位。
// admissionEvalIsolationProfile builds an isolation profile with every boundary set.
func admissionEvalIsolationProfile(memoryMB int) core.AdmissionIsolationProfile {
	return core.AdmissionIsolationProfile{
		Name:           "standard",
		Runtime:        "runsc",
		Image:          "sha256:" + strings.Repeat("a", 64),
		CPUs:           1,
		MemoryMB:       memoryMB,
		PIDs:           64,
		TmpfsMB:        16,
		OutputTmpfsMB:  32,
		MaxOutputBytes: 1 << 20,
		TimeoutSeconds: 30,
	}
}

// admissionEvalPolicyWithProfile 在固定策略上叠加一个隔离档位（R-03 回归用）。
// admissionEvalPolicyWithProfile layers one isolation profile over the fixed policy.
func admissionEvalPolicyWithProfile(profile core.AdmissionIsolationProfile) core.AdmissionPolicy {
	return BuildAdmissionPolicy(core.AdmissionPolicyConfig{
		CrossAgentRead:    []string{"b", "a"},
		AllowedCommands:   []string{"git", "go"},
		Interpreters:      []string{"python3"},
		AllowRunCode:      true,
		IsolationProfiles: []core.AdmissionIsolationProfile{profile},
	}, core.ExecutionPolicy{Profile: core.ProfileStandard, Revision: 2},
		[]core.NetworkDomainPolicy{
			{Scope: core.PolicyScopeHTTPDomains, Revision: 3, Domains: []string{"api.example.com"}},
			{Scope: core.PolicyScopeHTTPDeniedDomains, Revision: 1},
		}, nil)
}

// admissionEvalSource 造一份通过全部来源规则的声明；until 为空表示长期有效。
// admissionEvalSource builds a fully valid source declaration.
func admissionEvalSource(until string) core.AdmissionSource {
	return core.AdmissionSource{
		Kind:           "manual",
		Ref:            "内部周会记录",
		TaskOutcome:    "completed",
		Confirmation:   "direct",
		Authorization:  "declared",
		Redaction:      "declared",
		RetentionUntil: until,
		Applicable:     true,
		Evidence:       []string{"会议纪要 2026-11-01"},
	}
}

// admissionEvalBaseline 造一份基线；present 时带 UID / 版本 / 引用名单。
// admissionEvalBaseline builds a baseline snapshot.
func admissionEvalBaseline(state string, referrers []string) core.AdmissionBaseline {
	baseline := core.AdmissionBaseline{
		SkillState:  state,
		Referrers:   referrers,
		VersionNo:   1,
		ContentHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
	baseline.ReferrersHash = core.AdmissionRequestHash(referrers...)
	baseline.CanonicalJSON = core.AdmissionBaselineCanonical(baseline)
	return baseline
}

// admissionDimensionStatus 按名字取状态，避免用例依赖切片下标。
// admissionDimensionStatus looks one dimension up by name.
func admissionDimensionStatus(t *testing.T, evaluation core.AdmissionEvaluation, name core.AdmissionDimensionName) core.AdmissionDimension {
	t.Helper()
	for _, dimension := range evaluation.Dimensions {
		if dimension.Name == name {
			return dimension
		}
	}
	t.Fatalf("评估缺少维度 %s", name)
	return core.AdmissionDimension{}
}

// TestUS21_Evaluate_Dimensions 覆盖七维固定状态与理由。
// TestUS21_Evaluate_Dimensions pins the seven-dimension verdicts.
func TestUS21_Evaluate_Dimensions(t *testing.T) {
	t.Parallel()
	skillName := "weekly-report"
	files := admissionEvalFiles(skillName)
	candidate := admissionEvalCandidate(t, skillName, files)
	evaluation, err := EvaluateAdmission(candidate, files, admissionEvalSource(""), admissionEvalPolicy(),
		admissionEvalBaseline(core.AdmissionSkillAbsent, nil), admissionEvalNow)
	require.NoError(t, err)

	require.Equal(t, admissionDimensionOrder, dimensionNamesOf(evaluation), "维度顺序固定")
	for _, name := range []core.AdmissionDimensionName{
		core.AdmissionDimensionContent, core.AdmissionDimensionSource,
		core.AdmissionDimensionPermissions, core.AdmissionDimensionSafety,
	} {
		require.Equal(t, core.AdmissionDimensionPassed,
			admissionDimensionStatus(t, evaluation, name).Status, string(name))
	}
	require.Equal(t, core.AdmissionDimensionPassed,
		admissionDimensionStatus(t, evaluation, core.AdmissionDimensionRegression).Status,
		"新技能无旧版本可回归")
	require.Equal(t, core.AdmissionDimensionNotEvaluable,
		admissionDimensionStatus(t, evaluation, core.AdmissionDimensionEffect).Status)
	require.Equal(t, "real_model_not_authorized",
		admissionDimensionStatus(t, evaluation, core.AdmissionDimensionEffect).Reason)
	require.Equal(t, core.AdmissionDimensionNotEvaluable,
		admissionDimensionStatus(t, evaluation, core.AdmissionDimensionCost).Status)
	require.Equal(t, "no_real_model_invocation",
		admissionDimensionStatus(t, evaluation, core.AdmissionDimensionCost).Reason)
	require.Equal(t, AdmissionGraderVersion, evaluation.GraderVersion)
	require.Equal(t, admissionEvalPolicy().Hash, evaluation.PolicyHash)
	require.Empty(t, evaluation.Scripts)
	require.Equal(t, admissionEvalBaseline(core.AdmissionSkillAbsent, nil).CanonicalJSON, evaluation.BaselineJSON)
}

// dimensionNamesOf 抽出维度名序列（报告顺序断言用）。
// dimensionNamesOf extracts the report order.
func dimensionNamesOf(evaluation core.AdmissionEvaluation) []core.AdmissionDimensionName {
	names := make([]core.AdmissionDimensionName, 0, len(evaluation.Dimensions))
	for _, dimension := range evaluation.Dimensions {
		names = append(names, dimension.Name)
	}
	return names
}

// TestUS21_Evaluate_Aggregate 覆盖聚合规则与同输入同结果。
// TestUS21_Evaluate_Aggregate covers rollup and determinism.
func TestUS21_Evaluate_Aggregate(t *testing.T) {
	t.Parallel()
	skillName := "weekly-report"
	files := admissionEvalFiles(skillName)
	candidate := admissionEvalCandidate(t, skillName, files)
	baseline := admissionEvalBaseline(core.AdmissionSkillAbsent, nil)
	policy := admissionEvalPolicy()

	first, err := EvaluateAdmission(candidate, files, admissionEvalSource(""), policy, baseline, admissionEvalNow)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionAggregateIncomplete, first.Aggregate,
		"effect / cost 不可评估 → incomplete，不是 passed")

	second, err := EvaluateAdmission(candidate, files, admissionEvalSource(""), policy, baseline, admissionEvalNow)
	require.NoError(t, err)
	require.Equal(t, first.Dimensions, second.Dimensions, "同一输入同一时钟逐字相同")
	require.Equal(t, first.Aggregate, second.Aggregate)
	require.Equal(t, first.RiskTier, second.RiskTier)
	require.Equal(t, first.BaselineJSON, second.BaselineJSON)

	update, err := EvaluateAdmission(candidate, files, admissionEvalSource(""), policy,
		admissionEvalBaseline(core.AdmissionSkillPresent, nil), admissionEvalNow)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionDimensionNotEvaluable,
		admissionDimensionStatus(t, update, core.AdmissionDimensionRegression).Status,
		"更新既有技能：无行为回归评估")
	require.Equal(t, core.AdmissionAggregateIncomplete, update.Aggregate,
		"已有技能的回归未知仍是 incomplete，不是 failed")

	badSource := admissionEvalSource("")
	badSource.Authorization = "unknown"
	failed, err := EvaluateAdmission(candidate, files, badSource, policy, baseline, admissionEvalNow)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionAggregateFailed, failed.Aggregate)
	require.Equal(t, core.AdmissionDimensionFailed,
		admissionDimensionStatus(t, failed, core.AdmissionDimensionSource).Status)

	tampered := candidate
	tampered.CandidateHash = strings.Repeat("0", 64)
	mismatch, err := EvaluateAdmission(tampered, files, admissionEvalSource(""), policy, baseline, admissionEvalNow)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionAggregateFailed, mismatch.Aggregate)
	require.Equal(t, core.AdmissionDimensionFailed,
		admissionDimensionStatus(t, mismatch, core.AdmissionDimensionContent).Status)
}

// TestUS21_Evaluate_RiskTier 覆盖风险档判定与批准侧重算。
// TestUS21_Evaluate_RiskTier covers the tier rule and the approval-side recompute.
func TestUS21_Evaluate_RiskTier(t *testing.T) {
	t.Parallel()
	skillName := "weekly-report"
	files := admissionEvalFiles(skillName)
	candidate := admissionEvalCandidate(t, skillName, files)
	policy := admissionEvalPolicy()
	source := admissionEvalSource("")

	cases := []struct {
		name      string
		baseline  core.AdmissionBaseline
		files     []core.AuthorFile
		wantTier  core.AdmissionRiskTier
		scriptsNo int
	}{
		{
			name: "新技能无引用", baseline: admissionEvalBaseline(core.AdmissionSkillAbsent, nil),
			wantTier: core.AdmissionRiskNormal,
		},
		{
			name: "更新绑定中的技能", baseline: admissionEvalBaseline(core.AdmissionSkillPresent, []string{"analyst"}),
			wantTier: core.AdmissionRiskHigh,
		},
		{
			name: "更新未绑定技能", baseline: admissionEvalBaseline(core.AdmissionSkillPresent, nil),
			wantTier: core.AdmissionRiskNormal,
		},
		{
			name: "新技能带脚本", files: append(admissionEvalFiles(skillName),
				core.AuthorFile{Path: "scripts/collect.sh", Content: "#!/bin/sh\necho hi\n"}),
			wantTier: core.AdmissionRiskHigh, scriptsNo: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cand := candidate
			fileSet := files
			if tc.files != nil {
				fileSet = tc.files
				cand = admissionEvalCandidate(t, skillName, fileSet)
			}
			evaluation, err := EvaluateAdmission(cand, fileSet, source, policy, tc.baseline, admissionEvalNow)
			require.NoError(t, err)
			require.Equal(t, tc.wantTier, evaluation.RiskTier)
			require.Len(t, evaluation.Scripts, tc.scriptsNo)
			require.Equal(t, evaluation.RiskTier, AdmissionRiskTier(cand, evaluation),
				"批准 / 发布侧重算必须与评估逐字一致")
		})
	}
}

// TestUS21_Evaluate_RetentionExpired 覆盖保留期绝对时刻边界（now == until 即过期）。
// TestUS21_Evaluate_RetentionExpired pins the absolute-instant boundary.
func TestUS21_Evaluate_RetentionExpired(t *testing.T) {
	t.Parallel()
	skillName := "weekly-report"
	files := admissionEvalFiles(skillName)
	candidate := admissionEvalCandidate(t, skillName, files)
	policy := admissionEvalPolicy()
	baseline := admissionEvalBaseline(core.AdmissionSkillAbsent, nil)

	expired := admissionEvalSource(admissionEvalNow.Format(time.RFC3339))
	source := admissionRetentionValid(expired, admissionEvalNow)
	require.ErrorIs(t, source, core.ErrAdmissionRetentionExpired, "相等即过期")

	evaluation, err := EvaluateAdmission(candidate, files, expired, policy, baseline, admissionEvalNow)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionDimensionFailed,
		admissionDimensionStatus(t, evaluation, core.AdmissionDimensionSource).Status)
	require.Equal(t, core.AdmissionAggregateFailed, evaluation.Aggregate)

	future := admissionEvalSource(admissionEvalNow.Add(time.Hour).Format(time.RFC3339))
	require.NoError(t, admissionRetentionValid(future, admissionEvalNow))
	require.NoError(t, admissionRetentionValid(admissionEvalSource(""), admissionEvalNow),
		"空串是长期有效")

	// 批准有效期窗口（T013 的 Approve 复用同一函数）：(now, now+24h]，恰好 24h 有效。
	require.True(t, admissionExpiryWithin24h(admissionEvalNow.Add(time.Hour), admissionEvalNow))
	require.True(t, admissionExpiryWithin24h(admissionEvalNow.Add(admissionExpiryWindow), admissionEvalNow))
	require.False(t, admissionExpiryWithin24h(admissionEvalNow, admissionEvalNow), "等于当前时刻无效")
	require.False(t, admissionExpiryWithin24h(admissionEvalNow.Add(admissionExpiryWindow+time.Second), admissionEvalNow),
		"超过 24h 无效")
	require.False(t, admissionExpiryWithin24h(admissionEvalNow.Add(-time.Hour), admissionEvalNow))
}

// TestUS21_Evaluate_NoProviderOrNetwork 用源码面证明评估器不引入网络 / 模型 / 子进程。
// TestUS21_Evaluate_NoProviderOrNetwork proves by source that the evaluator imports no
// network, model or subprocess package.
func TestUS21_Evaluate_NoProviderOrNetwork(t *testing.T) {
	t.Parallel()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(file), "admission_eval.go"))
	require.NoError(t, err)
	source := string(raw)
	for _, forbidden := range []string{"net/http", "go-openai", "os/exec", "exec.Command", "provider."} {
		require.NotContains(t, source, forbidden, "评估器不得引入 %s", forbidden)
	}
}

// TestUS21_Evaluate_ScriptsNotExecuted 确认脚本只列名、不执行、不落盘。
// TestUS21_Evaluate_ScriptsNotExecuted confirms scripts are listed but never run.
func TestUS21_Evaluate_ScriptsNotExecuted(t *testing.T) {
	t.Parallel()
	skillName := "weekly-report"
	marker := filepath.Join(t.TempDir(), "executed.marker")
	files := append(admissionEvalFiles(skillName), core.AuthorFile{
		Path:    "scripts/collect.sh",
		Content: "#!/bin/sh\ntouch " + marker + "\n",
	})
	candidate := admissionEvalCandidate(t, skillName, files)
	evaluation, err := EvaluateAdmission(candidate, files, admissionEvalSource(""), admissionEvalPolicy(),
		admissionEvalBaseline(core.AdmissionSkillAbsent, nil), admissionEvalNow)
	require.NoError(t, err)
	require.Equal(t, []string{"scripts/collect.sh"}, evaluation.Scripts)
	require.Equal(t, core.AdmissionDimensionNotEvaluable,
		admissionDimensionStatus(t, evaluation, core.AdmissionDimensionPermissions).Status)
	_, statErr := os.Stat(marker)
	require.ErrorIs(t, statErr, os.ErrNotExist, "候选脚本不得被执行")
}

// TestUS21_Evaluate_ExtensionlessScript 是 review ② 发现 2 的评估面回归：无扩展名
// 的文件无法证明不是脚本，必须计入 Scripts、判 permissions not_evaluable 并抬到
// 高风险——脚本静态审查与提出者 / 批准者分离因此不可被绕过。
// 判别力：把 admissionScripts 恢复成只看扩展名，Scripts 为空、风险档退回 normal，
// 后三条断言全红。
func TestUS21_Evaluate_ExtensionlessScript(t *testing.T) {
	t.Parallel()
	skillName := "weekly-report"
	files := append(admissionEvalFiles(skillName),
		core.AuthorFile{Path: "scripts/collect", Content: "#!/bin/sh\necho collect\n"},
		core.AuthorFile{Path: "bin/runner", Content: "#!/bin/sh\nexec true\n"})
	candidate := admissionEvalCandidate(t, skillName, files)
	evaluation, err := EvaluateAdmission(candidate, files, admissionEvalSource(""), admissionEvalPolicy(),
		admissionEvalBaseline(core.AdmissionSkillAbsent, nil), admissionEvalNow)
	require.NoError(t, err)
	require.Equal(t, []string{"bin/runner", "scripts/collect"}, evaluation.Scripts)
	require.Equal(t, core.AdmissionDimensionNotEvaluable,
		admissionDimensionStatus(t, evaluation, core.AdmissionDimensionPermissions).Status)
	require.Equal(t, core.AdmissionRiskHigh, evaluation.RiskTier)
	require.Equal(t, evaluation.RiskTier, AdmissionRiskTier(candidate, evaluation),
		"批准 / 发布侧重算与评估逐字一致")
}

// TestUS21_Evaluate_TrailingDotScript 是「改文件名不能绕过审查」的改名变体：Linux 上
// `path.Ext("collect.")` 返回 `.`（名义扩展名为空），若不按无扩展名处理，改个名字即可
// 逃过脚本静态审查与提出者 / 批准者分离。断言与无扩展名用例同口径：进入 scripts、
// permissions 不可评估、风险档 high。
// 判别力：admissionScripts 去掉 `extension == "."` 分支，Scripts 期望非空实得空。
func TestUS21_Evaluate_TrailingDotScript(t *testing.T) {
	t.Parallel()
	skillName := "weekly-report"
	files := append(admissionEvalFiles(skillName),
		core.AuthorFile{Path: "scripts/collect.", Content: "#!/bin/sh\necho collect\n"})
	candidate := admissionEvalCandidate(t, skillName, files)
	evaluation, err := EvaluateAdmission(candidate, files, admissionEvalSource(""), admissionEvalPolicy(),
		admissionEvalBaseline(core.AdmissionSkillAbsent, nil), admissionEvalNow)
	require.NoError(t, err)
	require.Equal(t, []string{"scripts/collect."}, evaluation.Scripts)
	require.Equal(t, core.AdmissionDimensionNotEvaluable,
		admissionDimensionStatus(t, evaluation, core.AdmissionDimensionPermissions).Status)
	require.Equal(t, core.AdmissionRiskHigh, evaluation.RiskTier)
}

// TestUS21_Evaluate_TrailingSpaceScript 是 review ② F-03 的分类面回归：`run.sh ` 的
// 原始扩展名是带空格的 ".sh "，不剥末尾空白就会被当成非脚本放行。路径校验已拒绝这类
// 名字（workspace 的 TestSkillImport_EdgeWhitespace），分类层再兜一次，任何来源的
// 候选都不会因尾随空白漏判。
// 判别力：去掉 admissionScripts 的 TrimRightFunc，返回集合变空、断言转红。
func TestUS21_Evaluate_TrailingSpaceScript(t *testing.T) {
	t.Parallel()
	scripts := admissionScripts([]core.AuthorFile{
		{Path: "scripts/run.sh ", Content: "#!/bin/sh\n"},
		{Path: "scripts/collect. ", Content: "#!/bin/sh\n"},
		{Path: "docs/notes.md ", Content: "text\n"},
	})
	require.Equal(t, []string{"scripts/collect. ", "scripts/run.sh "}, scripts)
}

// TestUS21_Policy_HashInputMatchesContractNames 是 review ② F-04 的回归：policy_hash
// 输入必须按契约字面量序列化——嵌套结构体用 name / memory_mb / max_output_bytes /
// scope / domains / kind / value 等 snake_case 键，Go 字段名泄漏进哈希输入即偏离
// 契约（哈希仍然稳定，但第三方无法按契约复算）。
// 判别力：去掉三个嵌套结构体的 json tag，序列化回 Go 字段名，Contains 断言转红。
func TestUS21_Policy_HashInputMatchesContractNames(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(admissionPolicyHashInput{
		IsolationProfiles: []core.AdmissionIsolationProfile{admissionEvalIsolationProfile(512)},
		DomainPolicies: []core.AdmissionDomainPolicy{{
			Scope:    "http.allowed_domains",
			Revision: 1,
			Domains:  []string{"example.com"},
		}},
		NetworkExceptions: []core.AdmissionNetworkException{{Kind: "host", Value: "example.com"}},
	})
	require.NoError(t, err)
	for _, fragment := range []string{
		`"isolation_profiles":[{"name":"standard"`,
		`"memory_mb":512`,
		`"max_output_bytes":1048576`,
		`"domain_policies":[{"scope":"http.allowed_domains"`,
		`"network_exceptions":[{"kind":"host","value":"example.com"}]`,
	} {
		require.Contains(t, string(raw), fragment)
	}
}

// TestUS21_Policy_DomainTableRevisionChangesHash 逐字复刻颗粒度文档 §4.3.3 的断言。
// TestUS21_Policy_DomainTableRevisionChangesHash mirrors the module card's assertions.
func TestUS21_Policy_DomainTableRevisionChangesHash(t *testing.T) {
	t.Parallel()
	cfg := core.AdmissionPolicyConfig{}
	dbPolicy := core.ExecutionPolicy{}
	allow := core.NetworkDomainPolicy{Scope: core.PolicyScopeHTTPDomains, Revision: 3, Domains: []string{"api.example.com"}}
	deny := core.NetworkDomainPolicy{Scope: core.PolicyScopeHTTPDeniedDomains, Revision: 1}

	first := BuildAdmissionPolicy(cfg, dbPolicy, []core.NetworkDomainPolicy{allow, deny}, nil)
	allow.Revision = 4 // 内容不变、revision 变
	second := BuildAdmissionPolicy(cfg, dbPolicy, []core.NetworkDomainPolicy{allow, deny}, nil)
	require.NotEqual(t, first.Hash, second.Hash)

	allow.Domains = []string{"api.example.com", "cdn.example.com"}
	third := BuildAdmissionPolicy(cfg, dbPolicy, []core.NetworkDomainPolicy{allow, deny}, nil)
	require.NotEqual(t, second.Hash, third.Hash)

	// 行顺序不影响哈希；scope 与内容规范化后排序。
	reversed := BuildAdmissionPolicy(cfg, dbPolicy, []core.NetworkDomainPolicy{deny, allow}, nil)
	require.Equal(t, third.Hash, reversed.Hash)
	require.Len(t, third.Hash, 64)
}

// TestUS21_Policy_IsolationProfileChangesHash 是 review ② R-03 的策略面回归：
// isolation.profiles.* 的有效配置（镜像 / 内存 / PID / 超时等执行边界）必须进入
// policy_hash；档位顺序不影响哈希，新增档位改变哈希。
// 判别力：把 IsolationProfiles 从 admissionPolicyHashInput 删掉，前四条 NotEqual
// 断言全部变红。
func TestUS21_Policy_IsolationProfileChangesHash(t *testing.T) {
	t.Parallel()
	base := admissionEvalIsolationProfile(512)
	build := func(profiles ...core.AdmissionIsolationProfile) core.AdmissionPolicy {
		return BuildAdmissionPolicy(
			core.AdmissionPolicyConfig{IsolationBackend: "container", IsolationProfiles: profiles},
			core.ExecutionPolicy{Profile: core.ProfileStandard, Revision: 1}, nil, nil)
	}
	baseline := build(base)
	require.Len(t, baseline.IsolationProfiles, 1)
	require.Equal(t, "standard", baseline.IsolationProfiles[0].Name, "策略快照保留档位名")

	changed := base
	changed.Image = "sha256:" + strings.Repeat("b", 64)
	require.NotEqual(t, baseline.Hash, build(changed).Hash, "镜像变化必须改哈希")
	changed = base
	changed.MemoryMB = 1024
	require.NotEqual(t, baseline.Hash, build(changed).Hash, "内存上限变化必须改哈希")
	changed = base
	changed.PIDs = 128
	require.NotEqual(t, baseline.Hash, build(changed).Hash, "PID 上限变化必须改哈希")
	changed = base
	changed.TimeoutSeconds = 60
	require.NotEqual(t, baseline.Hash, build(changed).Hash, "超时变化必须改哈希")

	strict := admissionEvalIsolationProfile(2048)
	strict.Name = "strict"
	require.NotEqual(t, baseline.Hash, build(base, strict).Hash, "新增档位必须改哈希")
	require.Equal(t, build(base, strict).Hash, build(strict, base).Hash,
		"档位顺序（map 迭代顺序）不影响哈希")
	require.Equal(t, baseline.Hash, build(base).Hash, "同输入同结果")
}

// TestUS21_Policy_AdmissionsDirDenied 逐字复刻颗粒度文档 §4.3.9：Agent 读、写准入
// 目录都必须被既有策略拒绝，且拒绝出自明确规则而不是"路径在工作区外"。
// TestUS21_Policy_AdmissionsDirDenied pins both read and write denial for admissions.
func TestUS21_Policy_AdmissionsDirDenied(t *testing.T) {
	t.Parallel()
	root := t.TempDir() // 即 .basalt 根（PolicyOptions.Root 的既有口径）
	p := tool.NewWhitelistPolicy(tool.PolicyOptions{Root: root})
	target := filepath.Join(root, "admissions", "candidates", "adm_x", "files", "SKILL.md")

	read := core.Action{Type: core.ActionFileRead, Target: target, Agent: "analyst", RunBound: true}
	err := p.Check(context.Background(), read)
	require.Error(t, err, "Agent 不得读取准入候选")
	require.True(t, core.IsSandboxViolation(err), "拒绝必须是 *core.SandboxViolationError（按类型分类）")
	require.Contains(t, err.Error(), "读取目标不在可读根之内")

	write := core.Action{Type: core.ActionFileWrite, Target: target, Agent: "analyst", RunBound: true}
	err = p.Check(context.Background(), write)
	require.Error(t, err, "Agent 不得改写准入证据")
	require.True(t, core.IsSandboxViolation(err))
	require.Contains(t, err.Error(), "写入目标超出当前 Agent 的可写范围")
}

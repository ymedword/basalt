//go:build integration

package app

// 本文件是 US-21 批准 / 拒绝的验收面（颗粒度文档 §2.3.4、§3.1.5、§4.3.1、§4.3.5、
// §4.3.6、§4.3.12、§4.3.13、§4.3.14、T013）：
//
//	L1 `TestUS21_StateMachine_DuplicateApprove_OneActive`——重复批准被状态门禁拒绝，
//	   活跃批准仍是第一条且只有一条
//	L2 `TestUS21_HighRisk_SelfApproval_Rejected` / `TestUS21_HighRisk_SeparationPasses`
//	   ——高风险自批无决策行，换一个本机身份后批准成功
//	L3 `TestUS21_Approve_ContentChangedAfterEvaluation_Rejected` /
//	   `TestUS21_Approve_SourceChangedAfterEvaluation_Rejected`——评估后的篡改必须在
//	   决策写入前被抓住
//	L4 `TestUS21_Approve_RetentionExpired_Rejected`——保留期用固定时钟推进，不睡真实时间
//	L5 `TestUS21_Approve_ExpiryWindow`——有效期窗口是 (now, now+24h]，恰好 24 小时有效
//	L6 `TestUS21_Approve_FailedEvaluationRejected`——聚合 failed 不得批准
//	L7 `TestUS21_Approve_AcknowledgmentMismatch_Rejected`——确认集合必须精确匹配
//	L8 `TestUS21_Approve_ScriptsRequireStaticReview`——脚本审查覆盖全部脚本才通过
//	L9 `TestUS21_Approve_BearerRejected` / `TestUS21_Approve_UnknownCLIRejected`
//	   ——身份校验先于状态读取（approve 映射为 UNKNOWN_APPROVER）
//	L10 `TestUS21_Reject_TerminalStateAndSupersede`——拒绝落终态并取代活跃批准
//
// 判据只看真实数据库行与真实磁盘：候选目录由真实冻结实现写入，决策 / 候选 / 操作行
// 都来自真实仓储；用例只做两件"外部动作"——改写冻结目录与推进固定时钟。
// Every judgement reads real rows and real files; the cases only tamper with the frozen
// tree and advance the fixed clock.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// admissionApproveSourceJSON 以真实来源结构造严格 JSON；mutate 可改确认方式、保留期
// 等字段，解析与评估仍走生产路径。
// admissionApproveSourceJSON builds a strict source declaration through the real shape.
func admissionApproveSourceJSON(t *testing.T, mutate func(*core.AdmissionSource)) []byte {
	t.Helper()
	source := admissionEvalSource("")
	if mutate != nil {
		mutate(&source)
	}
	encoded, err := json.Marshal(source)
	require.NoError(t, err)
	return encoded
}

// admissionApproveScriptFiles 造含一个脚本的候选文件集（含脚本即高风险）。
// admissionApproveScriptFiles adds one script, which makes the candidate high risk.
func admissionApproveScriptFiles(skillName, scriptPath string) []core.AuthorFile {
	return append(admissionEvalFiles(skillName), core.AuthorFile{
		Path:    scriptPath,
		Content: "#!/bin/sh\necho collect\n",
	})
}

// advance 推进固定时钟；批准 / 发布的保留期与有效期判定都读同一个时钟。
// advance moves the fixture's fixed clock.
func (f *us21AdminFixture) advance(d time.Duration) {
	f.now = f.now.Add(d)
}

// proposeAndEvaluate 走真实提议 + 评估，返回提议时的候选（修订号仍是评估前的值）。
// proposeAndEvaluate runs the real propose + evaluate pair.
func (f *us21AdminFixture) proposeAndEvaluate(t *testing.T, skillName string, files []core.AuthorFile) core.AdmissionCandidate {
	t.Helper()
	return f.proposeAndEvaluateWithSource(t, skillName, files, f.source)
}

// proposeAndEvaluateWithSource 用指定来源声明走真实提议 + 评估。
// proposeAndEvaluateWithSource uses an explicit source declaration.
func (f *us21AdminFixture) proposeAndEvaluateWithSource(t *testing.T, skillName string,
	files []core.AuthorFile, source []byte,
) core.AdmissionCandidate {
	t.Helper()
	candidate, err := f.admin.Propose(t.Context(), f.actor, core.AdmissionProposeRequest{
		SkillName:  skillName,
		Files:      files,
		SourceJSON: source,
	})
	require.NoError(t, err)
	_, err = f.admin.Evaluate(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)
	return candidate
}

// mustCandidate 读当前候选行（参与决策绑定的修订号以这里为准）。
// mustCandidate reads the current candidate row.
func (f *us21AdminFixture) mustCandidate(t *testing.T, candidateID string) core.AdmissionCandidate {
	t.Helper()
	candidate, err := f.store.GetCandidate(t.Context(), candidateID)
	require.NoError(t, err)
	return candidate
}

// mustEvaluation 读最新评估行。
// mustEvaluation reads the latest evaluation row.
func (f *us21AdminFixture) mustEvaluation(t *testing.T, candidateID string) core.AdmissionEvaluation {
	t.Helper()
	evaluation, err := f.store.LatestEvaluation(t.Context(), candidateID)
	require.NoError(t, err)
	return evaluation
}

// reviewerActor 是第二个可信本机身份（高风险候选的批准人）。
// reviewerActor is a second trusted local identity for high-risk approvals.
func (f *us21AdminFixture) reviewerActor() core.Actor {
	return core.Actor{Subject: "cli:bob", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI}
}

// countDecisions 直接数决策行（含已 superseded）。
// countDecisions counts every decision row, superseded included.
func (f *us21AdminFixture) countDecisions(t *testing.T, candidateID string) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM skill_admission_decisions WHERE candidate_id = $1`,
		candidateID).Scan(&count))
	return count
}

// countActiveApprovals 直接数活跃批准（部分唯一索引允许的至多一条）。
// countActiveApprovals counts the active approvals, of which there can be at most one.
func (f *us21AdminFixture) countActiveApprovals(t *testing.T, candidateID string) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM skill_admission_decisions
		  WHERE candidate_id = $1 AND decision = 'approve' AND superseded_at IS NULL`,
		candidateID).Scan(&count))
	return count
}

// us21DecisionValues 是决策文件里可由用例改写的字段；其余绑定值始终取自当前候选与
// 最新评估，避免用例大量重复绑定字段。
// us21DecisionValues carries the decision fields a case may override; the binding
// fields always come from the current candidate and latest evaluation.
type us21DecisionValues struct {
	Acknowledged []core.AdmissionDimensionName
	ScriptReview []core.AdmissionScriptReview
	ExpiresAt    time.Time
}

// us21DecisionWire 是决策文件的严格 JSON 形态（字段名与契约逐字一致）。
// us21DecisionWire mirrors the strict decision file contract.
type us21DecisionWire struct {
	CandidateID            string                        `json:"candidate_id"`
	CandidateRevision      int64                         `json:"candidate_revision"`
	CandidateHash          string                        `json:"candidate_hash"`
	EvaluationID           string                        `json:"evaluation_id"`
	EvaluationHash         string                        `json:"evaluation_hash"`
	Decision               string                        `json:"decision"`
	Reason                 string                        `json:"reason"`
	AcknowledgedDimensions []core.AdmissionDimensionName `json:"acknowledged_dimensions"`
	ApprovedScope          *core.AdmissionApprovedScope  `json:"approved_scope,omitempty"`
	ScriptReview           []core.AdmissionScriptReview  `json:"script_review,omitempty"`
	Baseline               json.RawMessage               `json:"baseline,omitempty"`
	ExpiresAt              string                        `json:"expires_at,omitempty"`
}

// defaultDecisionValues 由最新评估推出默认值：确认集合 = 可接受集合，脚本逐条给出
// 静态审查证据，有效期一小时。
// defaultDecisionValues derives the default acknowledgement, script evidence and expiry.
func (f *us21AdminFixture) defaultDecisionValues(t *testing.T, candidateID string) us21DecisionValues {
	t.Helper()
	evaluation := f.mustEvaluation(t, candidateID)
	values := us21DecisionValues{
		Acknowledged: admissionAcknowledgeableSet(evaluation),
		ExpiresAt:    f.now.Add(time.Hour),
	}
	for _, script := range evaluation.Scripts {
		values.ScriptReview = append(values.ScriptReview, core.AdmissionScriptReview{
			Path:     script,
			Evidence: "静态阅读：只读工作目录文件，无网络调用，无外部命令",
		})
	}
	return values
}

// decisionFile 生成默认决策文件（绑定当前候选修订与最新评估）。
// decisionFile renders a default decision file bound to the current row.
func (f *us21AdminFixture) decisionFile(t *testing.T, candidateID string, kind core.AdmissionDecisionKind) []byte {
	t.Helper()
	return f.decisionFileWith(t, candidateID, kind, nil)
}

// decisionFileWith 允许用例改写确认集合 / 脚本审查 / 有效期。
// decisionFileWith overrides the acknowledgement, script review or expiry.
func (f *us21AdminFixture) decisionFileWith(t *testing.T, candidateID string,
	kind core.AdmissionDecisionKind, mutate func(*us21DecisionValues),
) []byte {
	t.Helper()
	candidate := f.mustCandidate(t, candidateID)
	evaluation := f.mustEvaluation(t, candidateID)
	values := f.defaultDecisionValues(t, candidateID)
	if mutate != nil {
		mutate(&values)
	}
	wire := us21DecisionWire{
		CandidateID:            candidate.CandidateID,
		CandidateRevision:      candidate.Revision,
		CandidateHash:          candidate.CandidateHash,
		EvaluationID:           evaluation.EvaluationID,
		EvaluationHash:         evaluation.EvaluationHash,
		Decision:               string(kind),
		Reason:                 "来源与权限面已核对",
		AcknowledgedDimensions: values.Acknowledged,
	}
	if kind == core.AdmissionApprove {
		manifest, err := core.ParseAdmissionManifest(candidate.ManifestJSON)
		require.NoError(t, err)
		referrers := append([]string{}, evaluation.Referrers...)
		wire.ApprovedScope = &core.AdmissionApprovedScope{
			SkillName: candidate.SkillName,
			RiskTier:  evaluation.RiskTier,
			Files:     manifest,
			Referrers: referrers,
		}
		wire.ScriptReview = values.ScriptReview
		wire.Baseline = json.RawMessage(evaluation.BaselineJSON)
		wire.ExpiresAt = values.ExpiresAt.UTC().Format(time.RFC3339)
	}
	encoded, err := json.Marshal(wire)
	require.NoError(t, err)
	return encoded
}

// decisionFileWithAck 用给定的确认维度覆盖默认集合。
// decisionFileWithAck overrides the acknowledged dimensions.
func (f *us21AdminFixture) decisionFileWithAck(t *testing.T, candidateID string, names ...string) []byte {
	t.Helper()
	return f.decisionFileWith(t, candidateID, core.AdmissionApprove, func(values *us21DecisionValues) {
		values.Acknowledged = nil
		for _, name := range names {
			values.Acknowledged = append(values.Acknowledged,
				core.AdmissionDimensionName(name))
		}
	})
}

// decisionFileWithScriptReview 用给定的审查条目覆盖默认脚本审查（nil 即空集）。
// decisionFileWithScriptReview overrides the script review entries.
func (f *us21AdminFixture) decisionFileWithScriptReview(t *testing.T, candidateID string,
	reviews []core.AdmissionScriptReview,
) []byte {
	t.Helper()
	return f.decisionFileWith(t, candidateID, core.AdmissionApprove,
		func(values *us21DecisionValues) { values.ScriptReview = reviews })
}

// decisionFileWithExpiry 用给定有效期覆盖默认值。
// decisionFileWithExpiry overrides the expiry.
func (f *us21AdminFixture) decisionFileWithExpiry(t *testing.T, candidateID string,
	expires time.Time,
) []byte {
	t.Helper()
	return f.decisionFileWith(t, candidateID, core.AdmissionApprove,
		func(values *us21DecisionValues) { values.ExpiresAt = expires })
}

// mustApprove 断言批准成功并返回决策行。
// mustApprove asserts a successful approval and returns the decision.
func (f *us21AdminFixture) mustApprove(t *testing.T, actor core.Actor, candidateID string,
	decisionJSON []byte,
) core.AdmissionDecision {
	t.Helper()
	decision, err := f.admin.Approve(t.Context(), actor, candidateID, decisionJSON, admissionEvalPolicy())
	require.NoError(t, err)
	return decision
}

// tamperCandidateFile 恢复写位后改写冻结目录里的一个文件。
// tamperCandidateFile rewrites one frozen candidate file after restoring write access.
func (f *us21AdminFixture) tamperCandidateFile(t *testing.T, candidateID, path, content string) {
	t.Helper()
	dir := filepath.Join(f.root, us21CandidateDirRel(candidateID))
	makeUS21TreeWritable(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "files", filepath.FromSlash(path)),
		[]byte(content), 0o644))
}

// tamperSourceFile 改写冻结目录里的来源声明（`source.json` 是固定布局的一部分）。
// tamperSourceFile rewrites the frozen source declaration.
func (f *us21AdminFixture) tamperSourceFile(t *testing.T, candidateID string, content []byte) {
	t.Helper()
	dir := filepath.Join(f.root, us21CandidateDirRel(candidateID))
	makeUS21TreeWritable(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "source.json"), content, 0o644))
}

// L1：重复批准被状态门禁拒绝，活跃批准仍是第一条且总数为一。
func TestUS21_StateMachine_DuplicateApprove_OneActive(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))

	first := f.mustApprove(t, f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove))
	_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionInvalidState, "候选已是 approved，状态门禁先拒")
	// 存储层的 CAS 也报同一个哨兵；文案把两层区分开——这里要求失败发生在应用的
	// 状态门禁上，而不是走到 InsertDecision 才被数据库条件写挡住。
	// Both layers share the sentinel; the message pins which one fired.
	require.Contains(t, err.Error(), "不允许批准")

	active, err := f.store.ActiveApproval(t.Context(), candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, first.DecisionID, active.DecisionID)
	require.Equal(t, 1, f.countActiveApprovals(t, candidate.CandidateID))
	require.Equal(t, 1, f.countDecisions(t, candidate.CandidateID))
}

// L2a：高风险候选由提出者自己批准必须被拒，且不留下决策行、候选保持 evaluated。
func TestUS21_HighRisk_SelfApproval_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report",
		admissionApproveScriptFiles("weekly-report", "scripts/collect.sh"))
	require.Equal(t, core.AdmissionRiskHigh, f.mustEvaluation(t, candidate.CandidateID).RiskTier)

	_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionSeparationRequired)
	require.Equal(t, core.AdmissionEvaluated, f.mustCandidate(t, candidate.CandidateID).State)
	require.Zero(t, f.countDecisions(t, candidate.CandidateID))
}

// L2b：高风险候选由另一个可信本机身份批准（脚本审查齐全）成功，决策记录批准者。
func TestUS21_HighRisk_SeparationPasses(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report",
		admissionApproveScriptFiles("weekly-report", "scripts/collect.sh"))
	reviewer := f.reviewerActor()

	decision := f.mustApprove(t, reviewer, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove))
	require.Equal(t, reviewer.Subject, decision.ApproverSubject)
	require.Equal(t, core.AdmissionApprove, decision.Decision)
	require.Equal(t, core.AdmissionRiskHigh, decision.RiskTier)
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State)
	require.Equal(t, 1, f.countActiveApprovals(t, candidate.CandidateID))
}

// L3a：评估后改写候选内容，批准从磁盘复算哈希后拒绝，且零决策行。
func TestUS21_Approve_ContentChangedAfterEvaluation_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	f.tamperCandidateFile(t, candidate.CandidateID, "SKILL.md",
		admissionEvalSkillMD("weekly-report", "tampered after evaluation"))

	_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch)
	require.Equal(t, core.AdmissionEvaluated, f.mustCandidate(t, candidate.CandidateID).State)
	require.Zero(t, f.countDecisions(t, candidate.CandidateID))
}

// L3b：评估后改写来源声明，批准按来源哈希拒绝，且零决策行。
func TestUS21_Approve_SourceChangedAfterEvaluation_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	f.tamperSourceFile(t, candidate.CandidateID,
		[]byte(`{"kind":"manual","ref":"tampered-after-evaluation"}`))

	_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionSourceMismatch)
	require.Equal(t, core.AdmissionEvaluated, f.mustCandidate(t, candidate.CandidateID).State)
	require.Zero(t, f.countDecisions(t, candidate.CandidateID))
}

// L4：评估时保留期未过、批准时已过——固定时钟推进三小时，不睡真实时间。
func TestUS21_Approve_RetentionExpired_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	source := admissionApproveSourceJSON(t, func(source *core.AdmissionSource) {
		source.RetentionUntil = f.now.Add(2 * time.Hour).Format(time.RFC3339)
	})
	candidate := f.proposeAndEvaluateWithSource(t, "weekly-report",
		admissionEvalFiles("weekly-report"), source)
	f.advance(3 * time.Hour) // 评估时未过期，批准时已过

	_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionRetentionExpired)
	require.Zero(t, f.countDecisions(t, candidate.CandidateID))
}

// L5：有效期必须落在 (now, now+24h]；恰好 24 小时是允许的上限。
func TestUS21_Approve_ExpiryWindow(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))

	for _, expires := range []time.Time{
		f.now,                                 // 不晚于当前时刻
		f.now.Add(24*time.Hour + time.Second), // 超过 24 小时上限
	} {
		_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID,
			f.decisionFileWithExpiry(t, candidate.CandidateID, expires), admissionEvalPolicy())
		require.ErrorIs(t, err, core.ErrAdmissionExpiryInvalid, "expires=%s", expires)
	}
	decision := f.decisionFileWithExpiry(t, candidate.CandidateID, f.now.Add(24*time.Hour))
	_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID, decision,
		admissionEvalPolicy())
	require.NoError(t, err, "恰好 24 小时是允许的上限")
}

// L6：来源未确认（model_inferred）使评估聚合 failed，批准在任何决策写入之前拒绝。
func TestUS21_Approve_FailedEvaluationRejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	source := admissionApproveSourceJSON(t, func(source *core.AdmissionSource) {
		source.Confirmation = "model_inferred"
	})
	candidate := f.proposeAndEvaluateWithSource(t, "weekly-report",
		admissionEvalFiles("weekly-report"), source)
	require.Equal(t, core.AdmissionAggregateFailed,
		f.mustEvaluation(t, candidate.CandidateID).Aggregate)

	_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID,
		f.decisionFileWithAck(t, candidate.CandidateID, "effect", "cost"), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionEvaluationFailed)
	require.Equal(t, core.AdmissionEvaluated, f.mustCandidate(t, candidate.CandidateID).State)
	require.Zero(t, f.countDecisions(t, candidate.CandidateID))
}

// L7：确认集合必须精确等于 {effect, cost, regression} 中的不可评估维度——多认、
// 少认、勾选白名单外的维度都拒绝。
func TestUS21_Approve_AcknowledgmentMismatch_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	evaluation := f.mustEvaluation(t, candidate.CandidateID)
	require.Equal(t, []core.AdmissionDimensionName{
		core.AdmissionDimensionEffect,
		core.AdmissionDimensionCost,
	}, admissionAcknowledgeableSet(evaluation), "不可评估集合固定为 effect / cost")

	cases := []struct {
		name string
		file func() []byte
	}{
		{"勾选白名单外的 safety", func() []byte {
			return f.decisionFileWithAck(t, candidate.CandidateID, "effect", "cost", "safety")
		}},
		{"漏认 effect", func() []byte {
			return f.decisionFileWithAck(t, candidate.CandidateID, "cost")
		}},
		{"多认已经通过的 regression", func() []byte {
			return f.decisionFileWithAck(t, candidate.CandidateID, "effect", "cost", "regression")
		}},
		{"完全不确认", func() []byte {
			return f.decisionFileWith(t, candidate.CandidateID, core.AdmissionApprove,
				func(values *us21DecisionValues) { values.Acknowledged = nil })
		}},
	}
	for _, tc := range cases {
		_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID, tc.file(),
			admissionEvalPolicy())
		require.ErrorIs(t, err, core.ErrAdmissionAcknowledgmentMismatch, tc.name)
	}
	require.Equal(t, core.AdmissionEvaluated, f.mustCandidate(t, candidate.CandidateID).State)
	require.Zero(t, f.countDecisions(t, candidate.CandidateID))
}

// L11：不可评估不等于通过——先断言聚合是 incomplete，再断言缺确认的批准失败。
// 逐字对号颗粒度文档 §4.3.6（夹具名适配本仓：newUS21AdminFixture / decisionFileWith）。
// TestUS21_NotEvaluable_IsNotPass asserts the aggregate is incomplete first, then that
// an approval without acknowledgement is refused.
func TestUS21_NotEvaluable_IsNotPass(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	evaluation := f.mustEvaluation(t, candidate.CandidateID)
	require.Equal(t, core.AdmissionAggregateIncomplete, evaluation.Aggregate)
	require.NotEqual(t, core.AdmissionAggregatePassed, evaluation.Aggregate, "不可评估不等于通过")
	names := map[core.AdmissionDimensionName]core.AdmissionDimensionStatus{}
	for _, dimension := range evaluation.Dimensions {
		names[dimension.Name] = dimension.Status
	}
	require.Equal(t, core.AdmissionDimensionNotEvaluable, names[core.AdmissionDimensionEffect])
	require.Equal(t, core.AdmissionDimensionNotEvaluable, names[core.AdmissionDimensionCost])
	decision := f.decisionFileWith(t, candidate.CandidateID, core.AdmissionApprove,
		func(values *us21DecisionValues) { values.Acknowledged = nil })
	_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID, decision, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionAcknowledgmentMismatch)
}

// L8：含脚本候选必须逐条提交静态审查证据；只勾选 permissions 不算审查。
func TestUS21_Approve_ScriptsRequireStaticReview(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report",
		admissionApproveScriptFiles("weekly-report", "scripts/collect.sh"))
	evaluation := f.mustEvaluation(t, candidate.CandidateID)
	require.Equal(t, []string{"scripts/collect.sh"}, evaluation.Scripts)
	// 含脚本即高风险：批准人必须是另一个可信本机身份（提出者是 f.actor）。
	reviewer := f.reviewerActor()

	_, err := f.admin.Approve(t.Context(), reviewer, candidate.CandidateID,
		f.decisionFileWithAck(t, candidate.CandidateID,
			"permissions", "effect", "cost", "regression"), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionAcknowledgmentMismatch)

	_, err = f.admin.Approve(t.Context(), reviewer, candidate.CandidateID,
		f.decisionFileWithScriptReview(t, candidate.CandidateID, nil), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionScriptReviewRequired)

	decision := f.decisionFileWithScriptReview(t, candidate.CandidateID,
		[]core.AdmissionScriptReview{{
			Path:     "scripts/collect.sh",
			Evidence: "静态阅读：只读工作目录文件，无网络调用，无外部命令",
		}})
	_, err = f.admin.Approve(t.Context(), reviewer, candidate.CandidateID, decision,
		admissionEvalPolicy())
	require.NoError(t, err)
	require.Equal(t, 1, f.countDecisions(t, candidate.CandidateID))
}

// TestUS21_Approve_ExtensionlessScriptRequiresReview 是 review ② 发现 2 的批准面
// 回归：无扩展名文件按脚本处理，因此自批被高风险分离拒绝、空脚本审查被拒绝、
// 逐条审查齐全才能批准——"换个文件名"绕不过审查与身份分离。
// 判别力：把 admissionScripts 恢复成只看扩展名，评估里 Scripts 为空、风险档为
// normal，自批会被放行，前两段断言红。
func TestUS21_Approve_ExtensionlessScriptRequiresReview(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report",
		admissionApproveScriptFiles("weekly-report", "scripts/collect"))
	evaluation := f.mustEvaluation(t, candidate.CandidateID)
	require.Equal(t, []string{"scripts/collect"}, evaluation.Scripts)
	require.Equal(t, core.AdmissionRiskHigh, evaluation.RiskTier)
	reviewer := f.reviewerActor()

	// 提出者自批：高风险分离拒绝（不把无扩展名当"非脚本"）。
	_, err := f.admin.Approve(t.Context(), f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionSeparationRequired)
	// 另一个身份但脚本审查为空：拒绝。
	_, err = f.admin.Approve(t.Context(), reviewer, candidate.CandidateID,
		f.decisionFileWithScriptReview(t, candidate.CandidateID, nil), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionScriptReviewRequired)
	require.Zero(t, f.countDecisions(t, candidate.CandidateID))

	decision := f.decisionFileWithScriptReview(t, candidate.CandidateID,
		[]core.AdmissionScriptReview{{
			Path:     "scripts/collect",
			Evidence: "静态阅读：无 shebang 之外的执行路径，无网络与外部命令",
		}})
	_, err = f.admin.Approve(t.Context(), reviewer, candidate.CandidateID, decision, admissionEvalPolicy())
	require.NoError(t, err)
	require.Equal(t, 1, f.countDecisions(t, candidate.CandidateID))
}

// L9a：Bearer 身份（Method 不是 cli）在读取候选之前被拒，approve 映射为
// UNKNOWN_APPROVER，且零决策行。
func TestUS21_Approve_BearerRejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	actor := core.Actor{
		Subject: "cli:alice",
		Method:  "bearer",
		Source:  core.AuthMethodCLI,
	}

	_, err := f.admin.Approve(t.Context(), actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove), admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionUnknownApprover)
	require.Equal(t, core.AdmissionEvaluated, f.mustCandidate(t, candidate.CandidateID).State)
	require.Zero(t, f.countDecisions(t, candidate.CandidateID))
}

// L9b：cli:unknown 主体在读取候选之前被拒（不存在的候选也报身份错误）。
func TestUS21_Approve_UnknownCLIRejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	actor := core.Actor{
		Subject: "cli:unknown",
		Method:  core.AuthMethodCLI,
		Source:  core.AuthMethodCLI,
	}
	decision := f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove)

	_, err := f.admin.Approve(t.Context(), actor, candidate.CandidateID, decision,
		admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionUnknownApprover)
	require.NotErrorIs(t, err, core.ErrAdmissionNotFound, "身份校验必须早于状态读取")

	nonexistent, idErr := core.NewAdmissionCandidateID()
	require.NoError(t, idErr)
	_, err = f.admin.Approve(t.Context(), actor, nonexistent, decision, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionUnknownApprover,
		"不存在的候选不得先报 CANDIDATE_MISSING")
	require.Zero(t, f.countDecisions(t, candidate.CandidateID))
}

// L10：已批准的候选可以被拒绝；拒绝取代活跃批准、落终态理由并保持唯一活跃批准为零。
func TestUS21_Reject_TerminalStateAndSupersede(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	approval := f.mustApprove(t, f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove))

	rejected, err := f.admin.Reject(t.Context(), f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionReject))
	require.NoError(t, err)
	require.Equal(t, core.AdmissionReject, rejected.Decision)
	require.Equal(t, "来源与权限面已核对", rejected.Reason)

	row := f.mustCandidate(t, candidate.CandidateID)
	require.Equal(t, core.AdmissionRejected, row.State)
	require.Equal(t, rejected.Reason, row.TerminalReason)
	require.Equal(t, rejected.OperationID, row.TerminalOperationID)
	require.Zero(t, f.countActiveApprovals(t, candidate.CandidateID),
		"拒绝必须取代活跃批准")
	_, err = f.store.ActiveApproval(t.Context(), candidate.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionNotFound)

	latest, err := f.store.LatestDecision(t.Context(), candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, rejected.DecisionID, latest.DecisionID)
	superseded, err := f.store.ActiveApproval(t.Context(), candidate.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionNotFound)
	_ = superseded

	operations := f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionReject})
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationSuccess, operations[0].Status)
	require.Equal(t, rejected.OperationID, operations[0].OperationID)
	require.NotEqual(t, approval.DecisionID, rejected.DecisionID)
}

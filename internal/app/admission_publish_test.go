//go:build integration

package app

// 本文件是 US-21 受控发布的验收面（颗粒度文档 §2.3.5、§3.1.5、§4.3.2、§4.3.3、
// §4.3.4、§4.3.11、C-01 / C-08 / C-10 / C-18 / C-20、T015）：
//
//	P1 `TestUS21_Publish_HappyPath`——合法发布落版本行 / 指针 / 发布记录 / 候选状态 /
//	   操作终态，工作副本与发布内容一致
//	P2 `TestUS21_Publish_ContentChangedAfterApproval_Rejected` /
//	   `TestUS21_Publish_SourceHashMismatch_Rejected`——批准后篡改在预写前抓住
//	P3 `TestUS21_Publish_StaleBaseline_Rejected` / `TestUS21_Publish_StalePolicy_Rejected` /
//	   `TestUS21_Publish_RiskTierEscalated_Rejected`——基线 / 策略 / 风险档逐条拒绝
//	P4 `TestUS21_Publish_AtomicOnFailure`——事务内 CAS 失败整体回滚：无版本、无指针、
//	   无发布记录，候选保持 approved，快照清理、工作副本复原
//	P5 `TestUS21_Publish_UsesSharedPrimitives`——只在共享原语之上编排，无第二套写入
//	P6 `TestUS21_Publish_PointerCAS` / `TestUS21_Publish_GateLocked`——指针条件写与发布闸门
//	P7 `TestUS21_Publish_FailureKeepsApproved` / `TestUS21_Publish_RetryRechecks`
//	   ——失败不新增状态，重试重新复核
//	P8 `TestUS21_Publish_DuplicateRejected` / `TestUS21_Publish_RetentionExpired_Rejected`
//	   / `TestUS21_Approve_ExpiredPublicationRejected`——一次性发布、保留期与批准过期
//	P9 `TestUS21_ConcurrentPublish_OneWinner`——并发发布只有一个赢家
//	P10 `TestUS21_Publish_DetailReadsPublishedVersion`——发布后详情能读回已发布版本
//
// 判据只看真实数据库行与真实磁盘；用例只做三件"外部动作"——改写冻结目录、推进固定
// 时钟、经仓储直接改一行做漂移注入。
// Every judgement reads real rows and real files; the cases only tamper the frozen
// tree, move the fixed clock, or mutate one row through the real store.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// proposeAndApprove 走真实提议 + 评估 + 批准，返回批准后的候选行与决策。
// proposeAndApprove runs the real propose + evaluate + approve chain.
func (f *us21AdminFixture) proposeAndApprove(t *testing.T, skillName string,
	files []core.AuthorFile,
) (core.AdmissionCandidate, core.AdmissionDecision) {
	t.Helper()
	candidate := f.proposeAndEvaluate(t, skillName, files)
	decision := f.mustApprove(t, f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove))
	return f.mustCandidate(t, candidate.CandidateID), decision
}

// countPublications 直接数发布记录行。
// countPublications counts publication records.
func (f *us21AdminFixture) countPublications(t *testing.T, candidateID string) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM skill_admission_publications WHERE candidate_id = $1`,
		candidateID).Scan(&count))
	return count
}

// countVersions 直接数某个技能名的资源版本行。
// countVersions counts resource version rows for one skill name.
func (f *us21AdminFixture) countVersions(t *testing.T, skillName string) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM resource_versions
		  WHERE resource_kind = $1 AND resource_id = $2`,
		string(core.ResourceSkill), skillName).Scan(&count))
	return count
}

// publishOperations 按动作读发布操作行（升序创建序由仓储固定）。
// publishOperations lists skill.admission.publish operation rows.
func (f *us21AdminFixture) publishOperations(t *testing.T) []core.OperationRecord {
	t.Helper()
	return f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionPublish})
}

// readPublishedSkill 经发布指针读回当前发布版本的文件内容：指针 → 版本行 → 磁盘，
// 与生产读取面（`publishedSkillFiles`）相互独立。
// readPublishedSkill reads the published files through pointer → version row → disk.
func (f *us21AdminFixture) readPublishedSkill(t *testing.T, skillName string) map[string]string {
	t.Helper()
	pointer, err := f.resources.Pointer(t.Context(), core.ResourceSkill, skillName)
	require.NoError(t, err)
	require.Nil(t, pointer.DeletedAt)
	version, err := f.resources.VersionByID(t.Context(), pointer.PublishedVersionID)
	require.NoError(t, err)
	var hashes []core.FileHash
	require.NoError(t, json.Unmarshal([]byte(version.ManifestJSON), &hashes))
	relDir, err := workspace.PublishedVersionRelDir(version.StoragePath)
	require.NoError(t, err)
	files, err := readSkillFiles(f.root, relDir, hashes)
	require.NoError(t, err)
	out := make(map[string]string, len(files))
	for _, file := range files {
		out[file.Path] = string(file.Content)
	}
	return out
}

// P1：合法发布一次事务写齐版本行、指针、发布记录、候选状态与操作终态；工作副本
// 与发布内容逐字一致，发布记录绑定批准时的决策与基线。
// P1 pins the whole publication write and its bindings.
func TestUS21_Publish_HappyPath(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, decision := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))
	require.Equal(t, core.AdmissionApproved, candidate.State)

	publication, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)
	require.Equal(t, candidate.CandidateID, publication.CandidateID)
	require.Equal(t, decision.DecisionID, publication.DecisionID)
	require.Equal(t, "weekly-report", publication.SkillName)
	require.Equal(t, core.AdmissionPublicationActive, publication.State)
	require.Equal(t, int64(1), publication.VersionNo)
	require.Equal(t, candidate.CandidateHash, publication.ContentHash)
	require.Equal(t, decision.BaselineJSON, publication.BaselineJSON)
	require.True(t, publication.PublishedAt.Equal(f.now))
	require.NotEmpty(t, publication.OperationID)

	row := f.mustCandidate(t, candidate.CandidateID)
	require.Equal(t, core.AdmissionPublished, row.State)
	require.Equal(t, candidate.Revision+1, row.Revision)

	pointer, err := f.resources.Pointer(t.Context(), core.ResourceSkill, "weekly-report")
	require.NoError(t, err)
	require.Equal(t, publication.VersionID, pointer.PublishedVersionID)
	require.Equal(t, publication.SkillUID, pointer.ResourceUID)
	require.Nil(t, pointer.DeletedAt)

	version, err := f.resources.VersionByID(t.Context(), publication.VersionID)
	require.NoError(t, err)
	require.Equal(t, 1, version.VersionNo)
	require.Equal(t, publication.ContentHash, version.ContentHash)
	require.Equal(t, publication.OperationID, version.OperationID)

	work, err := workspace.NewSkillLibrary(f.root).HashWorkCopy("weekly-report")
	require.NoError(t, err)
	require.Len(t, work.Files, 1)
	workDir, err := workspace.SkillWorkCopyRelDir("weekly-report")
	require.NoError(t, err)
	workFiles, err := readSkillFiles(f.root, workDir, work.Files)
	require.NoError(t, err)
	publishedFiles := f.readPublishedSkill(t, "weekly-report")
	require.Equal(t, admissionEvalSkillMD("weekly-report", "周报生成"),
		publishedFiles["SKILL.md"])
	require.Equal(t, publishedFiles["SKILL.md"], string(workFiles[0].Content),
		"工作副本与发布版本必须逐字一致")

	operations := f.publishOperations(t)
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationSuccess, operations[0].Status)
	require.Equal(t, publication.OperationID, operations[0].OperationID)
	require.Equal(t, core.TargetKindSkillAdmission, operations[0].TargetKind)
	require.Equal(t, candidate.CandidateID, operations[0].TargetID)
	require.JSONEq(t, `{}`, string(operations[0].DetailJSON))
}

// P2a：批准后改写候选内容——发布从磁盘复算哈希后拒绝，候选保持 approved，零发布。
// P2a pins the content re-hash at publication time.
func TestUS21_Publish_ContentChangedAfterApproval_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))
	f.tamperCandidateFile(t, candidate.CandidateID, "SKILL.md",
		admissionEvalSkillMD("weekly-report", "批准后改写"))

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch)
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State)
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
}

// P2b：批准后改写来源声明——来源哈希不符即拒绝，且不产生版本。
// P2b pins the source re-hash at publication time.
func TestUS21_Publish_SourceHashMismatch_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))
	f.tamperSourceFile(t, candidate.CandidateID, []byte(`{"kind":"manual","ref":"tampered"}`))

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionSourceMismatch)
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State)
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
}

// P3a：批准后新增引用使基线漂移——发布拒绝，零发布、零版本。
// P3a pins the baseline comparison against the current referrers.
func TestUS21_Publish_StaleBaseline_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))
	// 批准后新增引用：发布重新计算基线，与决策绑定的基线逐字不等。
	f.referrers = []string{"analyst"}

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionStaleBaseline)
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
}

// P3b：评估行的策略哈希被外部改写——发布拒绝，零版本。
// P3b pins the policy-hash comparison against the stored evaluation.
func TestUS21_Publish_StalePolicy_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))
	evaluation := f.mustEvaluation(t, candidate.CandidateID)
	_, err := f.db.ExecContext(t.Context(),
		`UPDATE skill_admission_evaluations SET policy_hash = $1 WHERE evaluation_id = $2`,
		strings.Repeat("a", 64), evaluation.EvaluationID)
	require.NoError(t, err)

	_, err = f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionStalePolicy)
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
}

// TestUS21_Policy_IsolationProfileDriftRejectsApproval 是 review ② R-03 的端到端
// 回归：评估 / 批准后修改同名档位的执行边界（内存 512 → 1024），旧批准发布被
// 拒绝（ErrAdmissionStalePolicy），候选保持 approved、零发布；未漂移的同一策略
// 随后发布成功——证明拒绝来自档位变化而不是发布路径其他因素。
// 判别力：把 IsolationProfiles 移出 policy_hash，两次策略哈希相同，第一次发布
// 不再报错，ErrorIs 断言红。
func TestUS21_Policy_IsolationProfileDriftRejectsApproval(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	approvedPolicy := admissionEvalPolicyWithProfile(admissionEvalIsolationProfile(512))
	candidate, err := f.admin.Propose(t.Context(), f.actor, core.AdmissionProposeRequest{
		SkillName:  "weekly-report",
		Files:      admissionEvalFiles("weekly-report"),
		SourceJSON: f.source,
	})
	require.NoError(t, err)
	_, err = f.admin.Evaluate(t.Context(), f.actor, candidate.CandidateID, approvedPolicy)
	require.NoError(t, err)
	_, err = f.admin.Approve(t.Context(), f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove), approvedPolicy)
	require.NoError(t, err)

	drifted := admissionEvalPolicyWithProfile(admissionEvalIsolationProfile(1024))
	require.NotEqual(t, approvedPolicy.Hash, drifted.Hash, "改档位必须改哈希")
	_, err = f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, drifted)
	require.ErrorIs(t, err, core.ErrAdmissionStalePolicy)
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State)

	_, err = f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, approvedPolicy)
	require.NoError(t, err)
	require.Equal(t, 1, f.countPublications(t, candidate.CandidateID))
}

// TestUS21_Publish_CommitUnknown_KeepsSnapshot 是 review ② 发现 1 的回归：仓储在
// 提交阶段返回"结果未知"时事务可能已经生效——补偿不得恢复工作副本或删除快照，
// 操作记 unknown，候选保持 approved 等待恢复查证。
// 判别力：删掉 abortSkillCommit 的 ErrAdmissionCommitUnknown 分支，操作变 failed、
// 快照被删除、工作副本被复原，三条断言变红。
func TestUS21_Publish_CommitUnknown_KeepsSnapshot(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	files := admissionEvalFiles(name)
	candidate, _ := f.proposeAndApprove(t, name, files)
	f.admin.publishAdmission = func(context.Context, core.AdmissionPublishRequest, time.Time) (core.AdmissionPublication, error) {
		return core.AdmissionPublication{}, fmt.Errorf("发布准入候选 %s: 提交结果未知: %w: %v",
			candidate.CandidateID, core.ErrAdmissionCommitUnknown, errors.New("connection reset"))
	}

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.Error(t, err)
	// 返回给调用方的是带 operation_id 的失败错误（既有契约），文案必须说明"提交
	// 结果未知、未回滚磁盘"，不把 unknown 伪装成普通失败。
	require.ErrorContains(t, err, "提交结果未知")

	ops := f.publishOperations(t)
	require.Len(t, ops, 1)
	require.Equal(t, core.OperationUnknown, ops[0].Status)
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State)
	matches, globErr := filepath.Glob(filepath.Join(f.root, "versions", "skills", name, "*", "1", "SKILL.md"))
	require.NoError(t, globErr)
	require.Len(t, matches, 1, "提交结果未知时不得删除快照")
	require.Equal(t, files[0].Content, f.workCopyBody(t, name), "提交结果未知时不得恢复工作副本")
}

// TestUS21_Publish_WorkCopyUncertainRecordedUnknown 是 review ② 阻断项 F-01 的回归：
// 工作副本交换失败且回滚也失败（ErrWorkCopyUncertain）时磁盘状态不确定——操作记
// unknown、不尝试自动复原工作副本、不写版本行；恢复只查证不重放。
// 判别力：把 Publish 的 ErrWorkCopyUncertain 分支删掉（退回 finishFailed），状态断言
// 由 unknown 变 failed 转红。
func TestUS21_Publish_WorkCopyUncertainRecordedUnknown(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	files := admissionEvalFiles(name)
	candidate, _ := f.proposeAndApprove(t, name, files)
	f.admin.writeWorkCopy = func(root string, cand workspace.SkillCandidate) error {
		// 先让真实实现把新树写进工作副本，再报"回滚失败"：模拟交换后的现场，
		// 断言点因此能证明调用方没有自动复原。
		if err := workspace.WriteSkillWorkCopy(root, cand); err != nil {
			return err
		}
		return fmt.Errorf("安装技能候选目录: %w", workspace.ErrWorkCopyUncertain)
	}

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.Error(t, err)

	ops := f.publishOperations(t)
	require.Len(t, ops, 1)
	require.Equal(t, core.OperationUnknown, ops[0].Status, "工作副本不确定必须记 unknown")
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State)
	require.Equal(t, files[0].Content, f.workCopyBody(t, name), "不得自动复原工作副本")
	require.Zero(t, f.countVersions(t, name), "这一阶段不得写版本行")
}

// P3c：评估行记录的风险档被改成 high——重算结果 normal 与记录不一致即拒绝。
// P3c pins the risk-tier recomputation against the recorded tier.
func TestUS21_Publish_RiskTierEscalated_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))
	evaluation := f.mustEvaluation(t, candidate.CandidateID)
	require.Equal(t, core.AdmissionRiskNormal, evaluation.RiskTier)
	_, err := f.db.ExecContext(t.Context(),
		`UPDATE skill_admission_evaluations SET risk_tier = 'high' WHERE evaluation_id = $1`,
		evaluation.EvaluationID)
	require.NoError(t, err)

	_, err = f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionRiskEscalated)
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
}

// P4：事务内候选 CAS 失败（外部把修订号 +1）——版本行、指针、发布记录与日志一起
// 回滚；快照清理、工作副本复原，候选保持 approved。
// P4 proves the publication transaction rolls back as one unit.
func TestUS21_Publish_AtomicOnFailure(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))

	var captured core.AdmissionPublishRequest
	f.admin.publishAdmission = func(ctx context.Context, req core.AdmissionPublishRequest,
		now time.Time,
	) (core.AdmissionPublication, error) {
		captured = req
		// 外部把候选修订号推进一步：事务后面的 CAS 用旧修订号，必然 0 行。
		_, execErr := f.db.ExecContext(ctx,
			`UPDATE skill_admission_candidates SET revision = revision + 1 WHERE candidate_id = $1`,
			req.CandidateID)
		require.NoError(t, execErr)
		return f.store.PublishAdmission(ctx, req, now)
	}

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.Error(t, err)
	require.NotEmpty(t, captured.CandidateID)

	row := f.mustCandidate(t, candidate.CandidateID)
	require.Equal(t, core.AdmissionApproved, row.State, "事务失败后候选必须保持 approved")
	require.Equal(t, candidate.Revision+1, row.Revision, "只有用例自己的外部推进生效")
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
	_, err = f.resources.Pointer(t.Context(), core.ResourceSkill, "weekly-report")
	require.ErrorIs(t, err, core.ErrResourceNotFound, "指针不得残留")

	snapshotRel, err := workspace.SkillVersionRelDir(captured.Version.ResourceID,
		captured.Version.ResourceUID, captured.Version.VersionNo)
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(f.root, filepath.FromSlash(snapshotRel)))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "失败尝试的版本快照必须清理")
	_, err = workspace.NewSkillLibrary(f.root).HashWorkCopy("weekly-report")
	require.ErrorIs(t, err, fs.ErrNotExist, "工作副本必须复原成不存在")

	operations := f.publishOperations(t)
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationFailed, operations[0].Status)
	require.Contains(t, operations[0].ErrorMessage, "发布失败")
}

// P5：发布只在共享原语之上编排——校验、工作副本、快照与指针事务都复用既有实现，
// 本文件不出现自己的 SQL 写入。
// P5 pins the reuse of the shared skill-publication primitives.
func TestUS21_Publish_UsesSharedPrimitives(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("admission_admin.go")
	require.NoError(t, err)
	text := string(source)
	// writeWorkCopy 以函数值形参挂在 AdmissionAdmin 上（review ② F-01 的不确定态
	// 注入点），语法形态从调用变成赋值；判据仍是"编排面必须引用共享原语"，故去掉
	// 尾随括号而不放宽其它原语。
	// The work-copy primitive is now referenced as a function value (F-01 injection
	// seam); the pin still requires the shared primitive, only the call syntax changed.
	for _, needle := range []string{
		"workspace.ValidateSkillDraft(",
		"workspace.WriteSkillWorkCopy",
		"workspace.SnapshotSkill(",
		"deps.Store.PublishAdmission",
		"a.gate.Lock()",
		"abortSkillCommit(",
	} {
		require.Contains(t, text, needle, "发布必须复用共享原语 %s", needle)
	}
	for _, forbidden := range []string{"INSERT INTO", "UPDATE skill_admission", "DELETE FROM"} {
		require.NotContains(t, text, forbidden, "编排面不得出现自己的 SQL 写入")
	}
}

// P6a：发布期间指针被外部推进——指针条件写发现基线不符，发布记录不落库。
// P6a proves the pointer CAS rejects an external pointer move.
func TestUS21_Publish_PointerCAS(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))

	var externalVersion core.ResourceVersion
	f.admin.publishAdmission = func(ctx context.Context, req core.AdmissionPublishRequest,
		now time.Time,
	) (core.AdmissionPublication, error) {
		operationID := f.beginOperation(t, core.ActionSkillCreate, core.TargetKindSkill, "weekly-report")
		version, publishErr := f.resources.PublishWithVersion(ctx, core.ResourceVersion{
			Kind:         core.ResourceSkill,
			ResourceID:   "weekly-report",
			ResourceUID:  core.ResourceUID("rs_00000000000000000000000033"),
			ContentHash:  core.AdmissionSHA256Hex([]byte("external")),
			ManifestJSON: `{"files":[]}`,
			StoragePath:  "skills/weekly-report/1",
			OperationID:  operationID,
		}, operationID, now)
		require.NoError(t, publishErr)
		externalVersion = version
		return f.store.PublishAdmission(ctx, req, now)
	}

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.Error(t, err)
	require.Contains(t, f.logs.String(), core.ErrAdmissionStaleBaseline.Error(),
		"补偿日志必须记录指针条件写的真实原因")
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State)
	require.Zero(t, f.countPublications(t, candidate.CandidateID))

	pointer, err := f.resources.Pointer(t.Context(), core.ResourceSkill, "weekly-report")
	require.NoError(t, err)
	require.Equal(t, externalVersion.ID, pointer.PublishedVersionID,
		"外部推进的指针不得被静默覆盖")
}

// P6b：发布闸门必须在事务期间持写锁——此时任何读者（Run 准入）都取不到读锁。
// P6b proves the publication gate holds the write lock across the transaction call.
func TestUS21_Publish_GateLocked(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))

	started := make(chan struct{})
	acquired := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	f.admin.publishAdmission = func(ctx context.Context, req core.AdmissionPublishRequest,
		now time.Time,
	) (core.AdmissionPublication, error) {
		go func() {
			defer close(done)
			close(started)
			f.gate.RLock()
			close(acquired)
			<-release
			f.gate.RUnlock()
		}()
		<-started
		select {
		case <-acquired:
			t.Error("发布事务期间读锁可获取——gate 未覆盖事务")
		case <-time.After(200 * time.Millisecond):
			// 预期：写锁被持有，读锁阻塞。
		}
		return f.store.PublishAdmission(ctx, req, now)
	}

	publication, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)
	require.NotZero(t, publication.VersionID)
	close(release)
	<-done
}

// P7a：发布失败不新增候选状态——候选保持 approved，失败操作留存，可再次尝试。
// P7a proves a failed publication leaves the candidate approvable again.
func TestUS21_Publish_FailureKeepsApproved(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))

	injected := errors.New("注入：发布事务失败")
	f.admin.publishAdmission = func(context.Context, core.AdmissionPublishRequest,
		time.Time,
	) (core.AdmissionPublication, error) {
		return core.AdmissionPublication{}, injected
	}

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.Error(t, err)
	require.Contains(t, f.logs.String(), injected.Error())

	row := f.mustCandidate(t, candidate.CandidateID)
	require.Equal(t, core.AdmissionApproved, row.State)
	require.Equal(t, candidate.Revision, row.Revision)
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
	// 失败不得把候选写成任何新状态（DDL 只有固定枚举；这里直接钉住 approved）。
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State)

	operations := f.publishOperations(t)
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationFailed, operations[0].Status)
}

// P7b：失败后先篡改再重试——重试必须重新复核并拒绝；恢复原字节后才允许发布。
// P7b proves retries re-run every check instead of resuming a half-written attempt.
func TestUS21_Publish_RetryRechecks(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))

	injected := errors.New("注入：首次发布失败")
	first := true
	f.admin.publishAdmission = func(ctx context.Context, req core.AdmissionPublishRequest,
		now time.Time,
	) (core.AdmissionPublication, error) {
		if first {
			first = false
			return core.AdmissionPublication{}, injected
		}
		return f.store.PublishAdmission(ctx, req, now)
	}

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.Error(t, err)

	dir := filepath.Join(f.root, us21CandidateDirRel(candidate.CandidateID), "files")
	original, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	require.NoError(t, err)
	f.tamperCandidateFile(t, candidate.CandidateID, "SKILL.md",
		admissionEvalSkillMD("weekly-report", "失败后改写"))

	_, err = f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch, "重试必须重新复核内容哈希")
	require.Zero(t, f.countPublications(t, candidate.CandidateID))

	makeUS21TreeWritable(t, filepath.Join(f.root, us21CandidateDirRel(candidate.CandidateID)))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), original, 0o444))
	_, err = f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err, "恢复原字节后重试成功")
	require.Equal(t, 1, f.countPublications(t, candidate.CandidateID))
}

// P8a：一个候选最多一次成功发布——第二次发布被状态门禁拒绝，指针不动。
// P8a pins the one-publication-per-candidate rule.
func TestUS21_Publish_DuplicateRejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))
	first, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	_, err = f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionInvalidState)
	require.Contains(t, err.Error(), "不允许发布")
	require.Equal(t, 1, f.countPublications(t, candidate.CandidateID))
	require.Equal(t, 1, f.countVersions(t, "weekly-report"))

	pointer, err := f.resources.Pointer(t.Context(), core.ResourceSkill, "weekly-report")
	require.NoError(t, err)
	require.Equal(t, first.VersionID, pointer.PublishedVersionID)
}

// P8b：批准时未过期、发布时已过保留期——发布拒绝（用固定时钟推进，不睡真实时间）。
// P8b pins the retention re-check at publication time.
func TestUS21_Publish_RetentionExpired_Rejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	source := admissionApproveSourceJSON(t, func(source *core.AdmissionSource) {
		source.RetentionUntil = f.now.Add(2 * time.Hour).Format(time.RFC3339)
	})
	candidate := f.proposeAndEvaluateWithSource(t, "weekly-report",
		admissionEvalFiles("weekly-report"), source)
	// 批准有效期用满 24 小时，确保先触发的是保留期而不是批准过期。
	f.mustApprove(t, f.actor, candidate.CandidateID,
		f.decisionFileWithExpiry(t, candidate.CandidateID, f.now.Add(24*time.Hour)))
	f.advance(3 * time.Hour)

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionRetentionExpired)
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
}

// P8c：批准到期瞬间（`now == expires_at`）发布即拒绝——有效窗口是左闭右开。
// P8c pins the expiry boundary at publication time.
func TestUS21_Approve_ExpiredPublicationRejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	f.mustApprove(t, f.actor, candidate.CandidateID,
		f.decisionFileWithExpiry(t, candidate.CandidateID, f.now.Add(time.Hour)))
	f.advance(time.Hour)

	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionApprovalExpired)
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State,
		"过期只在展示层派生，库里的 approved 不回写")
	require.Zero(t, f.countPublications(t, candidate.CandidateID))
	require.Zero(t, f.countVersions(t, "weekly-report"))
}

// P9：并发发布只有一个赢家——恰好一次成功、一次状态拒绝、一条发布记录、一个版本，
// 发布内容仍可读（-race 单列命令覆盖共享状态）。
// P9 pins the single-winner property of concurrent publications.
func TestUS21_ConcurrentPublish_OneWinner(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	success, invalid := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, core.ErrAdmissionInvalidState):
			invalid++
		default:
			t.Fatalf("unexpected publish error: %v", err)
		}
	}
	require.Equal(t, 1, success)
	require.Equal(t, 1, invalid)
	require.Equal(t, 1, f.countPublications(t, candidate.CandidateID))
	require.Equal(t, 1, f.countVersions(t, "weekly-report"))
	require.Equal(t, admissionEvalSkillMD("weekly-report", "周报生成"),
		f.readPublishedSkill(t, "weekly-report")["SKILL.md"])
}

// P10：发布后详情仍能从磁盘读回已发布版本（`IncludeDiff` 走发布指针而不是候选
// 目录）；内容未变化时 diff 为空。
// P10 proves the detail projection reads the published version after publication.
func TestUS21_Publish_DetailReadsPublishedVersion(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate, _ := f.proposeAndApprove(t, "weekly-report", admissionEvalFiles("weekly-report"))
	publication, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	detail, err := f.admin.Get(t.Context(), candidate.CandidateID, core.AdmissionShowOptions{
		IncludeFiles: true,
		IncludeDiff:  true,
	})
	require.NoError(t, err)
	require.NotNil(t, detail.Publication)
	require.Equal(t, publication.VersionID, detail.Publication.VersionID)
	require.Len(t, detail.Files, 1)
	require.Empty(t, detail.Diff, "发布版本与候选内容一致时 diff 为空")
}

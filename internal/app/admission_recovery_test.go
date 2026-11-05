//go:build integration

package app

// 本文件是 US-21 启动恢复的验收面（颗粒度文档 §2.3.7、§3.1.11、C-10 / C-16、
// §4.3.10、T019）：
//
//	R1 `TestUS21_Recovery_PublishNotApplied_NotReplayed`——只有 pending 行、没有发布
//	   证据时，恢复判 failed 且**不重放**（零发布记录、零版本行）
//	R2 `TestUS21_Recovery_UnknownNotRewritten`——候选目录在而候选行不在（写到一半）
//	   判 unknown；再次恢复不改写旧结论、不删证据、不重做
//	R3 `TestUS21_Recovery_VerdictPropose`——提议查证三态：行在 / 都不在 / 目录在
//	R4 `TestUS21_Recovery_VerdictOperationRows`——评估、批准、拒绝、取消、发布、
//	   撤回六个动作按 `operation_id` 认领证据行，行在 → applied、不在 → not_applied，
//	   取消还要核对终态是不是**本操作**写的
//	R5 `TestUS21_Recovery_DispatchRequiresAdmissionDeps`——装配缺失与目标类型不符
//	   一律 unknown，不借用别组的证据
//	R6 `TestUS21_Recovery_DispatchCoversSevenActions`——七个准入动作都经
//	   `verifyOperation` 分派臂到达准入查证面（分派臂少列任一动作即落到 default
//   的 unknown，用例转红）
//
// 判据只看真实数据库行与真实磁盘：pending 行由真实管理操作仓储预写，证据行来自
// 真实写路径；"目录在而行不在"是真实的半写状态（冻结目录后、事务前崩溃）。
// Every verdict reads real rows and real files; the half-written state is a real
// directory frozen before its transaction, not a mocked error.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// recover 以与生产装配同口径的 RecoveryDeps 触发启动恢复（只查证、不重放）。
// recover runs startup recovery through the same recovery dependencies as production.
func (f *us21AdminFixture) recover(t *testing.T) {
	t.Helper()
	require.NoError(t, recoverPendingOperations(t.Context(), RecoveryDeps{
		Operations: f.ops,
		Resources:  f.resources,
		Lifecycle:  workspace.NewLifecycleStore(f.root, func() time.Time { return f.now }),
		Root:       f.root,
		Admission:  f.admin,
		Logger:     f.admin.logger,
		Now:        func() time.Time { return f.now },
	}))
}

// mustOperation 按操作号读回恢复后的终态。
// mustOperation reloads one operation row after recovery.
func (f *us21AdminFixture) mustOperation(t *testing.T, operationID string) core.OperationRecord {
	t.Helper()
	record, err := f.ops.GetOperation(t.Context(), operationID)
	require.NoError(t, err)
	return record
}

// admissionVerdictOp 造一条只带查证所需字段的操作记录（动作 / 目标类型 / 目标 / 操作号）。
// admissionVerdictOp builds the minimal record the verdict needs.
func admissionVerdictOp(action, operationID, candidateID string) core.OperationRecord {
	return core.OperationRecord{
		OperationID: operationID,
		Action:      action,
		TargetKind:  core.TargetKindSkillAdmission,
		TargetID:    candidateID,
	}
}

// R1：发布在提交前崩溃——pending 行还在、证据行不存在。恢复判 not_applied → failed，
// 且**不重放**：若实现重放发布，会多出发布记录与版本行，计数断言红。
// R1 pins "verify, never replay" for a publish that crashed before its transaction.
func TestUS21_Recovery_PublishNotApplied_NotReplayed(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v1"))

	operationID := f.beginOperation(t, core.ActionSkillAdmissionPublish,
		core.TargetKindSkillAdmission, candidate.CandidateID)
	f.recover(t)

	recovered := f.mustOperation(t, operationID)
	require.Equal(t, core.OperationFailed, recovered.Status)
	require.Equal(t, core.VerificationNotApplied, recovered.Verification)
	require.Equal(t, 0, f.countPublications(t, candidate.CandidateID))
	require.Equal(t, 0, f.countVersions(t, name))
	require.Equal(t, core.AdmissionApproved, f.mustCandidate(t, candidate.CandidateID).State,
		"查证不改候选：保持 approved 供重新复核后重试")
}

// R2：目录在而候选行不在 = 写到一半。恢复判 unknown；再次恢复不改写旧结论、
// 不删候选目录、不补建候选行。
// R2 pins that a half-written proposal stays unknown and is never rewritten.
func TestUS21_Recovery_UnknownNotRewritten(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidateID, err := core.NewAdmissionCandidateID()
	require.NoError(t, err)
	dir := filepath.Join(f.root, us21CandidateDirRel(candidateID))
	require.NoError(t, os.MkdirAll(dir, 0o755))

	operationID := f.beginOperation(t, core.ActionSkillAdmissionPropose,
		core.TargetKindSkillAdmission, candidateID)
	f.recover(t)

	recovered := f.mustOperation(t, operationID)
	require.Equal(t, core.OperationUnknown, recovered.Status)
	require.Equal(t, core.VerificationIndeterminate, recovered.Verification)
	require.NotZero(t, recovered.RecoveredAt, "恢复要留下查证时刻")
	require.Contains(t, f.logs.String(), "无法判定", "unknown 必须留 WARN 日志线索")

	// 第二次恢复：unknown 行不再是 pending，旧结论不得被改写。
	// A second pass must not rewrite the stored verdict.
	f.recover(t)
	again := f.mustOperation(t, operationID)
	require.Equal(t, recovered.Status, again.Status)
	require.Equal(t, recovered.Verification, again.Verification)
	require.Equal(t, recovered.RecoveredAt, again.RecoveredAt)

	_, statErr := os.Stat(dir)
	require.NoError(t, statErr, "unknown 不删证据目录")
	_, getErr := f.store.GetCandidate(t.Context(), candidateID)
	require.ErrorIs(t, getErr, core.ErrAdmissionNotFound, "unknown 不自动补建候选行")
}

// R3：提议查证三态——候选行存在 → applied；行与目录都不存在 → not_applied；
// 目录存在而行不存在 → indeterminate。
// R3 covers the three propose verdicts.
func TestUS21_Recovery_VerdictPropose(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	ctx := t.Context()

	candidate, err := f.admin.Propose(ctx, f.actor, f.proposeRequest("weekly-report"))
	require.NoError(t, err)
	require.Equal(t, core.VerificationApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionPropose, "op_unused", candidate.CandidateID)))

	missingID, err := core.NewAdmissionCandidateID()
	require.NoError(t, err)
	require.Equal(t, core.VerificationNotApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionPropose, "op_unused", missingID)))

	require.NoError(t, os.MkdirAll(filepath.Join(f.root, us21CandidateDirRel(missingID)), 0o755))
	require.Equal(t, core.VerificationIndeterminate, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionPropose, "op_unused", missingID)))
}

// R4：六个"按 operation_id 认领证据行"的动作——真实写路径留下的操作号 → applied；
// 不存在的操作号 → not_applied。取消多做一步：别的操作留下的终态 → indeterminate。
// R4 covers the six operation-keyed verdicts plus the cancel ownership check.
func TestUS21_Recovery_VerdictOperationRows(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	ctx := t.Context()
	const missingOp = "op_missing_0000000000000000000000"

	// evaluate / approve：同一候选先评估再批准；先手工发一版，撤回才有上一版可恢复。
	f.importSkill(t, "weekly-report", us21VersionFiles("weekly-report", "v0"))
	candidate := f.proposeAndEvaluate(t, "weekly-report", us21VersionFiles("weekly-report", "v1"))
	evaluation := f.mustEvaluation(t, candidate.CandidateID)
	decision := f.mustApprove(t, f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove))
	require.Equal(t, core.VerificationApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionEvaluate, evaluation.OperationID, candidate.CandidateID)))
	require.Equal(t, core.VerificationNotApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionEvaluate, missingOp, candidate.CandidateID)))
	require.Equal(t, core.VerificationApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionApprove, decision.OperationID, candidate.CandidateID)))
	require.Equal(t, core.VerificationNotApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionApprove, missingOp, candidate.CandidateID)))

	// publish / withdraw：同一条发布记录的两个操作号分别认领。
	publication, err := f.admin.Publish(ctx, f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)
	withdrawn, err := f.admin.Withdraw(ctx, f.actor, candidate.CandidateID, "恢复查证")
	require.NoError(t, err)
	require.Equal(t, core.VerificationApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionPublish, publication.OperationID, candidate.CandidateID)))
	require.Equal(t, core.VerificationNotApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionPublish, missingOp, candidate.CandidateID)))
	require.NotEmpty(t, withdrawn.WithdrawOperationID)
	require.Equal(t, core.VerificationApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionWithdraw, withdrawn.WithdrawOperationID, candidate.CandidateID)))
	require.Equal(t, core.VerificationNotApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionWithdraw, missingOp, candidate.CandidateID)))

	// reject：另一个候选走真实拒绝路径。
	const rejectName = "weekly-reject"
	rejectedCandidate := f.proposeAndEvaluate(t, rejectName, us21VersionFiles(rejectName, "v1"))
	rejected, err := f.admin.Reject(ctx, f.actor, rejectedCandidate.CandidateID,
		f.decisionFile(t, rejectedCandidate.CandidateID, core.AdmissionReject))
	require.NoError(t, err)
	require.Equal(t, core.VerificationApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionReject, rejected.OperationID, rejectedCandidate.CandidateID)))

	// cancel：本操作写的终态 → applied；别的操作写的终态 → unknown；未推进 → not_applied。
	const cancelName = "weekly-cancel"
	cancelCandidate := f.proposeAndEvaluate(t, cancelName, us21VersionFiles(cancelName, "v1"))
	canceled, err := f.admin.Cancel(ctx, f.actor, cancelCandidate.CandidateID, "恢复查证")
	require.NoError(t, err)
	require.Equal(t, core.VerificationApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionCancel, canceled.TerminalOperationID, cancelCandidate.CandidateID)))
	require.Equal(t, core.VerificationIndeterminate, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionCancel, missingOp, cancelCandidate.CandidateID)))

	const openName = "weekly-open"
	openCandidate := f.proposeAndEvaluate(t, openName, us21VersionFiles(openName, "v1"))
	require.Equal(t, core.VerificationNotApplied, f.admin.VerifyOperation(ctx,
		admissionVerdictOp(core.ActionSkillAdmissionCancel, missingOp, openCandidate.CandidateID)))
}

// R5：装配缺失与目标类型不符一律 unknown——恢复不猜、不借用别组的证据行。
// R5 keeps missing deps and spoofed target kinds indeterminate.
func TestUS21_Recovery_DispatchRequiresAdmissionDeps(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	ctx := t.Context()
	op := admissionVerdictOp(core.ActionSkillAdmissionPublish, "op_unused", "adm_missing")

	withoutDeps := RecoveryDeps{}
	require.Equal(t, core.VerificationIndeterminate, verifyOperation(ctx, withoutDeps, op),
		"准入查证面缺失只能判 unknown")

	withDeps := RecoveryDeps{Admission: f.admin}
	spoofed := admissionVerdictOp(core.ActionSkillAdmissionPublish, "op_unused", "adm_missing")
	spoofed.TargetKind = "skill"
	require.Equal(t, core.VerificationIndeterminate, verifyOperation(ctx, withDeps, spoofed),
		"别的目标类型不得继承准入证据")

	unknownAction := admissionVerdictOp("skill.admission.unknown", "op_unused", "adm_missing")
	require.Equal(t, core.VerificationIndeterminate, f.admin.VerifyOperation(ctx, unknownAction),
		"未知准入动作不得判成 not_applied")

	// 三态与终态一一对应（与既有 verifyOutcome 同一映射，不另立口径）。
	// The tri-state maps 1:1 onto the terminal statuses.
	require.Equal(t, core.OperationSuccess, verifyOutcome(core.VerificationApplied))
	require.Equal(t, core.OperationFailed, verifyOutcome(core.VerificationNotApplied))
	require.Equal(t, core.OperationUnknown, verifyOutcome(core.VerificationIndeterminate))
}

// R6：七个准入动作必须**经分派臂**到达准入查证面——R4 直接调 `VerifyOperation`
// 只钉住了查证实现本身，从 `baseline.go` 的分派 `case` 里删掉任一动作，R4 仍绿。
// 这里对七个字面量各走一次 `verifyOperation`：propose 用真实候选行取 applied，
// 其余六个用不存在的操作号取 not_applied；任一动作缺分派即落到 default 的
// indeterminate，本用例转红。
// R6 pins the dispatch arm: dropping any of the seven actions falls through to the
// default indeterminate verdict, which this test catches where R4 cannot.
func TestUS21_Recovery_DispatchCoversSevenActions(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	deps := RecoveryDeps{Admission: f.admin}
	const missingOp = "op_missing_0000000000000000000000"

	// propose：查的是候选行本身，真实候选存在 → applied。
	require.Equal(t, core.VerificationApplied, verifyOperation(t.Context(), deps,
		admissionVerdictOp(core.ActionSkillAdmissionPropose, missingOp, candidate.CandidateID)),
		"propose 必须经分派臂到达准入查证面")

	// 其余六个动作：候选停在 evaluated，操作号不存在 → not_applied。
	// 分派臂缺任一动作时会变成 indeterminate（default），与期望值不同即红。
	for _, action := range []string{
		core.ActionSkillAdmissionEvaluate,
		core.ActionSkillAdmissionApprove,
		core.ActionSkillAdmissionReject,
		core.ActionSkillAdmissionCancel,
		core.ActionSkillAdmissionPublish,
		core.ActionSkillAdmissionWithdraw,
	} {
		require.Equal(t, core.VerificationNotApplied, verifyOperation(t.Context(), deps,
			admissionVerdictOp(action, missingOp, candidate.CandidateID)),
			"动作 %s 必须经分派臂到达准入查证面", action)
	}
}

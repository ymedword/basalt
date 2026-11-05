//go:build integration

package app

// 本文件是 US-21 撤回的验收面（颗粒度文档 §2.3.6、§2.5 失败矩阵、§4.3.7、
// C-09 / C-11 / C-19、T018）：
//
//	W1 `TestUS21_Withdraw_RestoresPreviousVersion`——指针仍指向本次发布版本时，上一版
//	   内容作为**新版本**重新发布：指针前移、内容回到上一版、候选落 withdrawn
//	W2 `TestUS21_Withdraw_FirstVersionRejected`——首版撤回到空被拒绝，零操作行
//	W3 `TestUS21_Withdraw_SupersededDoesNotMovePointer`——被后续手工发布超越只记撤回，
//	   **不**把指针回退到上一版
//	W4 `TestUS21_Withdraw_DeletedSkill`——技能已删只记撤回，指针保持墓碑
//	W5 `TestUS21_Withdraw_Idempotent`——重复撤回原样返回发布记录，不新增操作行
//	W6 `TestUS21_Withdraw_KeepsEvidence`——撤回不删版本行 / 决策行 / 候选目录
//	W7 `TestUS21_Withdraw_SnapshotFailureKeepsPublished`——快照失败记 failed，候选保持
//	   published，工作副本复原，指针不动
//	W8 `TestUS21_Withdraw_RejectsUntrustedActor`——不可信身份在状态读取前拒绝
//	W9 `TestUS21_Withdraw_ReasonValidation`——空 / 超长 / 含凭证理由在预写前拒绝
//	W10 `TestUS21_Withdraw_TamperedPublishedContentRejected`——上一版发布目录被改写或
//	   多出清单外文件时拒绝撤回（重读磁盘闸门的钉住用例，两个分支都走）
//	W11 `TestUS21_Withdraw_CompensationFailureRecordedUnknown`——撤回准备失败的补偿
//	   也失败时操作记 unknown（磁盘状态不确定，不谎报 failed）
//	W12 `TestUS21_Withdraw_SupersededCommitUnknownRecorded`——被超越分支提交结果未知
//	   记 unknown（无磁盘补偿，候选与指针不动）
//
// 判据只看真实数据库行与真实磁盘；"被超越 / 已删除"两类由真实的手工技能写路径制造，
// 不直接 UPDATE 指针行。
// Every judgement reads real rows and real files; the superseded / deleted cases are
// produced through the real manual skill write paths, never by mutating pointer rows.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// us21VersionFiles 造与 `admissionEvalFiles` 同构、但正文带版本标记的技能文件集：
// 三个版本内容两两不同，撤回后能按内容判定指针回到了哪一版。
// us21VersionFiles builds a valid skill file set whose body carries a version marker.
func us21VersionFiles(skillName, marker string) []core.AuthorFile {
	return []core.AuthorFile{{
		Path: "SKILL.md",
		Content: "---\nname: " + skillName + "\ndescription: 第 " + marker + " 版周报\n---\n" +
			"正文 " + marker + "\n",
	}}
}

// us21FileMap 把文件集折成 path → 正文，便于与 readPublishedSkill 的读数逐字比较。
// us21FileMap folds a file set into path → content for literal comparison.
func us21FileMap(files []core.AuthorFile) map[string]string {
	out := make(map[string]string, len(files))
	for _, file := range files {
		out[file.Path] = file.Content
	}
	return out
}

// importSkill 经真实手工技能写路径建一只技能并发布版本 1（与用户手工导入同一编排）。
// importSkill creates and publishes version 1 through the real manual write path.
func (f *us21AdminFixture) importSkill(t *testing.T, name string, files []core.AuthorFile) SkillDetail {
	t.Helper()
	detail, err := f.resourceAdmin.ImportSkill(t.Context(), f.actor,
		SkillImportRequest{Name: name, Files: files})
	require.NoError(t, err)
	return detail
}

// mustPointer 读技能发布指针（含删除标记）。
// mustPointer loads the skill publish pointer.
func (f *us21AdminFixture) mustPointer(t *testing.T, name string) core.ResourcePointer {
	t.Helper()
	pointer, err := f.resources.Pointer(t.Context(), core.ResourceSkill, name)
	require.NoError(t, err)
	return pointer
}

// mustPublication 按候选取发布记录。
// mustPublication loads the publication record of one candidate.
func (f *us21AdminFixture) mustPublication(t *testing.T, candidateID string) core.AdmissionPublication {
	t.Helper()
	publication, err := f.store.GetPublication(t.Context(), candidateID)
	require.NoError(t, err)
	return publication
}

// workCopyBody 读回技能工作副本根卡片的正文。
// workCopyBody reads the root card body of the work copy.
func (f *us21AdminFixture) workCopyBody(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.root, "skills", name, "SKILL.md"))
	require.NoError(t, err)
	return string(raw)
}

// withdrawOperations 按动作读撤回操作行（仓储按创建序升序返回）。
// withdrawOperations lists skill.admission.withdraw operation rows.
func (f *us21AdminFixture) withdrawOperations(t *testing.T) []core.OperationRecord {
	t.Helper()
	return f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionWithdraw})
}

// versionDir 解析指定版本在磁盘上的绝对目录（版本表 → storage_path）。
// versionDir resolves one published version's absolute directory on disk.
func (f *us21AdminFixture) versionDir(t *testing.T, skillName string, versionNo int) string {
	t.Helper()
	versions, err := f.resources.Versions(t.Context(), core.ResourceSkill, skillName)
	require.NoError(t, err)
	for _, version := range versions {
		if int(version.VersionNo) == versionNo {
			relDir, relErr := workspace.PublishedVersionRelDir(version.StoragePath)
			require.NoError(t, relErr)
			return filepath.Join(f.root, filepath.FromSlash(relDir))
		}
	}
	t.Fatalf("技能 %s 没有版本 %d", skillName, versionNo)
	return ""
}

// W1：手工发布 v1 → 准入发布 v2 → 撤回。上一版内容作为 v3 重新发布，指针前移，
// 候选落 withdrawn，操作终态 success。
// W1 pins the restored_previous path end to end.
func TestUS21_Withdraw_RestoresPreviousVersion(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	v1Files := us21VersionFiles(name, "v1")
	v2Files := us21VersionFiles(name, "v2")
	f.importSkill(t, name, v1Files)

	candidate, _ := f.proposeAndApprove(t, name, v2Files)
	publication, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)
	require.Equal(t, int64(2), publication.VersionNo)
	require.Equal(t, us21FileMap(v2Files), f.readPublishedSkill(t, name))

	withdrawn, err := f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "回退到 v1")
	require.NoError(t, err)
	require.Equal(t, core.AdmissionPublicationWithdrawn, withdrawn.State)
	require.Equal(t, core.AdmissionWithdrawRestoredPrevious, withdrawn.WithdrawReason)
	require.NotZero(t, withdrawn.WithdrawVersionID)
	require.NotEqual(t, publication.VersionID, withdrawn.WithdrawVersionID)

	// 指针前移到**新版本**，不是退回到 v1 的旧版本行；内容与 v1 逐字相同。
	pointer := f.mustPointer(t, name)
	require.Nil(t, pointer.DeletedAt)
	require.Equal(t, publication.SkillUID, pointer.ResourceUID)
	require.Equal(t, withdrawn.WithdrawVersionID, pointer.PublishedVersionID)
	require.Equal(t, us21FileMap(v1Files), f.readPublishedSkill(t, name))
	require.Equal(t, v1Files[0].Content, f.workCopyBody(t, name))

	versions, err := f.resources.Versions(t.Context(), core.ResourceSkill, name)
	require.NoError(t, err)
	require.Len(t, versions, 3, "v1 手工、v2 准入、v3 撤回恢复各一行")
	require.Equal(t, 3, versions[0].VersionNo)
	require.Equal(t, versions[2].ContentHash, versions[0].ContentHash,
		"撤回版本与 v1 内容同哈希，但版本行是新的")

	stored := f.mustCandidate(t, candidate.CandidateID)
	require.Equal(t, core.AdmissionWithdrawn, stored.State)
	require.Equal(t, "回退到 v1", stored.TerminalReason)

	ops := f.withdrawOperations(t)
	require.Len(t, ops, 1)
	require.Equal(t, core.OperationSuccess, ops[0].Status)
	require.Equal(t, stored.TerminalOperationID, ops[0].OperationID)
}

// W2：首版没有更早版本，拒绝撤回到空；不预写操作行、候选与指针不变。
// W2 pins the first-version refusal before any pre-write.
func TestUS21_Withdraw_FirstVersionRejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v1"))
	publication, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "想撤回到空")
	require.ErrorIs(t, err, core.ErrAdmissionFirstVersionWithdraw)
	require.Empty(t, f.withdrawOperations(t))
	require.Equal(t, core.AdmissionPublished, f.mustCandidate(t, candidate.CandidateID).State)
	require.Equal(t, publication.VersionID, f.mustPointer(t, name).PublishedVersionID)
	require.Equal(t, core.AdmissionPublicationActive, f.mustPublication(t, candidate.CandidateID).State)
}

// W3：准入发布 v2 之后又有一次手工发布 v3——撤回只记录"已被超越"，**不**把指针回退
// 到 v2 的上一版，手工更新过的内容必须留在原地。
func TestUS21_Withdraw_SupersededDoesNotMovePointer(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	v3Files := us21VersionFiles(name, "v3")
	f.importSkill(t, name, us21VersionFiles(name, "v1"))
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v2"))
	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	etag, ok := f.resourceAdmin.CurrentETag(t.Context(), core.ResourceSkill, name)
	require.True(t, ok)
	updated, err := f.resourceAdmin.UpdateSkill(t.Context(), f.actor,
		SkillUpdateRequest{Name: name, Files: v3Files, IfMatch: etag})
	require.NoError(t, err)
	require.Equal(t, 3, updated.VersionNo)

	withdrawn, err := f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "撤回旧候选")
	require.NoError(t, err)
	require.Equal(t, core.AdmissionWithdrawSuperseded, withdrawn.WithdrawReason)
	require.Zero(t, withdrawn.WithdrawVersionID)

	pointer := f.mustPointer(t, name)
	require.Equal(t, updated.VersionID, pointer.PublishedVersionID,
		"被超越时撤回不得覆盖后续手工发布")
	require.Equal(t, us21FileMap(v3Files), f.readPublishedSkill(t, name))
	require.Equal(t, core.AdmissionWithdrawn, f.mustCandidate(t, candidate.CandidateID).State)
}

// TestUS21_Withdraw_SupersededCommitUnknownRecorded：被超越分支不碰磁盘，但撤回事务
// 提交结果未知同样只证明"可能已生效"——按契约「提交结果未知」段记 unknown，不按确定
// 失败回执；候选保持 published、指针不动，等恢复路径查证。
// 判别力：删掉 Withdraw 非恢复分支的 ErrAdmissionCommitUnknown 判定，操作变 failed。
func TestUS21_Withdraw_SupersededCommitUnknownRecorded(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	f.importSkill(t, name, us21VersionFiles(name, "v1"))
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v2"))
	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	etag, ok := f.resourceAdmin.CurrentETag(t.Context(), core.ResourceSkill, name)
	require.True(t, ok)
	updated, err := f.resourceAdmin.UpdateSkill(t.Context(), f.actor,
		SkillUpdateRequest{Name: name, Files: us21VersionFiles(name, "v3"), IfMatch: etag})
	require.NoError(t, err)

	f.admin.withdrawAdmission = func(context.Context, core.AdmissionWithdrawRequest, time.Time) (core.AdmissionPublication, error) {
		return core.AdmissionPublication{}, fmt.Errorf("撤回准入发布 %s: 提交结果未知: %w: %v",
			candidate.CandidateID, core.ErrAdmissionCommitUnknown, errors.New("connection reset"))
	}
	_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "撤回旧候选")
	require.Error(t, err)
	require.ErrorContains(t, err, "提交结果未知")

	operations := f.withdrawOperations(t)
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationUnknown, operations[0].Status)
	require.Equal(t, core.AdmissionPublished, f.mustCandidate(t, candidate.CandidateID).State,
		"提交结果未知时不得把候选改成 withdrawn")
	pointer := f.mustPointer(t, name)
	require.Equal(t, updated.VersionID, pointer.PublishedVersionID, "提交结果未知时指针不得移动")
}

// W4：技能在撤回前被删除——只记录"技能已删"，指针保持墓碑，不重建技能。
func TestUS21_Withdraw_DeletedSkill(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	f.importSkill(t, name, us21VersionFiles(name, "v1"))
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v2"))
	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	before := f.mustPointer(t, name)
	etag, ok := f.resourceAdmin.CurrentETag(t.Context(), core.ResourceSkill, name)
	require.True(t, ok)
	require.NoError(t, f.resourceAdmin.Delete(t.Context(), f.actor, core.ResourceSkill, name,
		etag, string(before.ResourceUID)))
	deleted := f.mustPointer(t, name)
	require.NotNil(t, deleted.DeletedAt)

	withdrawn, err := f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "技能已删，撤回候选")
	require.NoError(t, err)
	require.Equal(t, core.AdmissionWithdrawDeleted, withdrawn.WithdrawReason)
	require.Zero(t, withdrawn.WithdrawVersionID)

	after := f.mustPointer(t, name)
	require.NotNil(t, after.DeletedAt, "撤回不得复活已删除的技能")
	require.Equal(t, deleted.PublishedVersionID, after.PublishedVersionID)
	require.Equal(t, core.AdmissionWithdrawn, f.mustCandidate(t, candidate.CandidateID).State)
}

// W5：重复撤回原样返回已撤回的发布记录，不新增操作行、不动指针。
func TestUS21_Withdraw_Idempotent(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	f.importSkill(t, name, us21VersionFiles(name, "v1"))
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v2"))
	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)
	first, err := f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "第一次撤回")
	require.NoError(t, err)
	pointer := f.mustPointer(t, name)
	revision := f.mustCandidate(t, candidate.CandidateID).Revision

	second, err := f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "再撤一次")
	require.NoError(t, err)
	require.Equal(t, first.WithdrawOperationID, second.WithdrawOperationID)
	require.Equal(t, first.WithdrawReason, second.WithdrawReason)
	require.Equal(t, first.WithdrawVersionID, second.WithdrawVersionID)
	require.Len(t, f.withdrawOperations(t), 1, "幂等返回不新增操作行")
	require.Equal(t, pointer.PublishedVersionID, f.mustPointer(t, name).PublishedVersionID)
	require.Equal(t, revision, f.mustCandidate(t, candidate.CandidateID).Revision)
}

// W6：撤回不得删除已发布版本、决策记录或候选目录（C-19）。
func TestUS21_Withdraw_KeepsEvidence(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	f.importSkill(t, name, us21VersionFiles(name, "v1"))
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v2"))
	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)
	_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "保留证据")
	require.NoError(t, err)

	_, statErr := os.Stat(filepath.Join(f.root, us21CandidateDirRel(candidate.CandidateID)))
	require.NoError(t, statErr, "候选目录是审计证据，撤回不删除")
	require.Equal(t, 1, f.countDecisions(t, candidate.CandidateID))
	require.Equal(t, 1, f.countPublications(t, candidate.CandidateID))
	require.Equal(t, 3, f.countVersions(t, name), "v1、v2 与撤回版本全部保留")
}

// W7：快照失败 → 操作记 failed，候选保持 published，指针不动，工作副本复原。
// 注入方式：把撤回目标版本目录占成普通文件，让 SnapshotSkill 在"已存在且不是目录"
// 上失败——这是真实的文件系统冲突，不是模拟的错误值。
// W7 injects a real filesystem conflict at the target version path.
func TestUS21_Withdraw_SnapshotFailureKeepsPublished(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	v2Files := us21VersionFiles(name, "v2")
	f.importSkill(t, name, us21VersionFiles(name, "v1"))
	candidate, _ := f.proposeAndApprove(t, name, v2Files)
	publication, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	relDir, err := workspace.SkillVersionRelDir(name, publication.SkillUID, int(publication.VersionNo)+1)
	require.NoError(t, err)
	blocked := filepath.Join(f.root, filepath.FromSlash(relDir))
	require.NoError(t, os.MkdirAll(filepath.Dir(blocked), 0o755))
	require.NoError(t, os.WriteFile(blocked, []byte("占位"), 0o644))

	_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "快照会失败")
	require.Error(t, err)

	ops := f.withdrawOperations(t)
	require.Len(t, ops, 1)
	require.Equal(t, core.OperationFailed, ops[0].Status)
	require.Equal(t, core.AdmissionPublished, f.mustCandidate(t, candidate.CandidateID).State)
	require.Equal(t, publication.VersionID, f.mustPointer(t, name).PublishedVersionID)
	require.Equal(t, core.AdmissionPublicationActive, f.mustPublication(t, candidate.CandidateID).State)
	require.Equal(t, v2Files[0].Content, f.workCopyBody(t, name), "失败补偿把工作副本复原为 v2")
	_, statErr := os.Stat(blocked)
	require.ErrorIs(t, statErr, os.ErrNotExist, "占位文件被清理，不留半截快照")
}

// W8：不可信身份在状态读取之前被拒绝——用不存在的候选号证明"没走到读候选"。
// W8 proves rejection happens before any state read.
func TestUS21_Withdraw_RejectsUntrustedActor(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	actors := map[string]core.Actor{
		"empty":     {},
		"unknown":   {Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		"bearer":    {Subject: "cli:alice", Method: core.AuthMethodBearer, Source: core.AuthMethodBearer},
		"non-cli":   {Subject: "cli:alice", Method: core.AuthMethodCLI, Source: "http"},
		"no-prefix": {Subject: "alice", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
	}
	for label, actor := range actors {
		_, err := f.admin.Withdraw(t.Context(), actor, "adm_missing", "撤回")
		require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor, label)
	}
	require.Empty(t, f.withdrawOperations(t))
}

// W9：空理由、超长理由、含凭证理由都在预写前拒绝，零操作行、候选保持 published。
func TestUS21_Withdraw_ReasonValidation(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	f.importSkill(t, name, us21VersionFiles(name, "v1"))
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v2"))
	_, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "")
	require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
	_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, strings.Repeat("安", 2001))
	require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
	_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, admissionSecretSamples[0])
	require.ErrorIs(t, err, core.ErrAdmissionSecretDetected)

	require.Empty(t, f.withdrawOperations(t))
	require.Equal(t, core.AdmissionPublished, f.mustCandidate(t, candidate.CandidateID).State)
}

// W10：撤回恢复"上一版"之前必须重读磁盘——上一版发布目录被改写或多出清单外文件时,
// 重算哈希 / 清单与登记行不一致，撤回拒绝且**不**把可疑内容当上一版重新发布。
// 撤回已预写操作行（预写在重读之前），失败记 failed；候选、发布记录、指针、
// 版本行全部保持原样。发布目录是只读的（目录 0555 / 文件 0444），篡改前先去只读位
// ——模拟拿到磁盘写权限或数据库写权限的人，不绕过被撤回闸门本身。
// 逆改点：去掉 `prepareWithdrawVersion` 的 `LoadPublished` 闸门或哈希比对，三个
// 子例的 `ErrAdmissionCandidateMismatch` 断言转红（撤回成功或返回普通错误）。
// W10 pins the re-read gate: a tampered published directory or version row is refused,
// never republished.
func TestUS21_Withdraw_TamperedPublishedContentRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tamper   func(t *testing.T, dir string)
		dbTamper func(t *testing.T, f *us21AdminFixture)
	}{
		{
			name: "改写上一版正文",
			tamper: func(t *testing.T, dir string) {
				t.Helper()
				card := filepath.Join(dir, "SKILL.md")
				require.NoError(t, os.Chmod(card, 0o644))
				require.NoError(t, os.WriteFile(card,
					[]byte("---\nname: weekly-report\ndescription: 被篡改的周报\n---\n篡改正文\n"), 0o644))
			},
		},
		{
			name: "清单外多文件",
			tamper: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.Chmod(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "extra.md"),
					[]byte("清单外文件\n"), 0o644))
			},
		},
		{
			// 登记行指向别的技能目录：LoadPublished 的目录归属校验必须拒绝，
			// 且统一以 admission-mismatch 类错误失败（不是普通读取错误）。
			name: "登记目录属于其他技能",
			dbTamper: func(t *testing.T, f *us21AdminFixture) {
				t.Helper()
				_, execErr := f.db.ExecContext(t.Context(),
					`UPDATE resource_versions SET storage_path = '.basalt/versions/skills/other-skill/1'
					  WHERE resource_kind = $1 AND resource_id = 'weekly-report' AND version_no = 1`,
					string(core.ResourceSkill))
				require.NoError(t, execErr)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUS21AdminFixture(t)
			const name = "weekly-report"
			f.importSkill(t, name, us21VersionFiles(name, "v1"))
			candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v2"))
			publication, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
			require.NoError(t, err)
			versionsBefore := f.countVersions(t, name)
			pointerBefore := f.mustPointer(t, name)

			// 被撤回的目标是上一版（v1）的发布目录，不是本次发布（v2）。
			if tc.tamper != nil {
				tc.tamper(t, f.versionDir(t, name, 1))
			}
			if tc.dbTamper != nil {
				tc.dbTamper(t, f)
			}

			_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "回退到 v1")
			require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch)

			// 预写在前、查证失败在后：操作行留 failed，业务面零变化。
			ops := f.withdrawOperations(t)
			require.Len(t, ops, 1)
			require.Equal(t, core.OperationFailed, ops[0].Status)
			require.Equal(t, core.AdmissionPublished, f.mustCandidate(t, candidate.CandidateID).State)
			require.Equal(t, core.AdmissionPublicationActive, f.mustPublication(t, candidate.CandidateID).State)
			require.Equal(t, versionsBefore, f.countVersions(t, name), "不得把篡改内容发布成新版本")
			require.Equal(t, publication.VersionID, f.mustPointer(t, name).PublishedVersionID,
				"指针保持指向被撤回的发布版本")
			require.Equal(t, pointerBefore.PublishedVersionID, f.mustPointer(t, name).PublishedVersionID)
		})
	}
}

// W11 是 review ② 发现 4 的回归：撤回准备失败（快照冲突）后补偿本身也失败
// （快照清理报错）时，磁盘状态不确定，操作必须记 unknown 而不是 failed。
// 判别力：把 Withdraw 的 ErrAdmissionCompensationUncertain 分支删掉（退回
// finishFailed），状态断言由 unknown 变 failed 转红。
// W11 pins that a failed compensation is recorded as unknown, never as failed.
func TestUS21_Withdraw_CompensationFailureRecordedUnknown(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	f.importSkill(t, name, us21VersionFiles(name, "v1"))
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v2"))
	publication, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	// 快照失败：目标版本目录被普通文件占住（与 W7 同一注入方式）。
	relDir, err := workspace.SkillVersionRelDir(name, publication.SkillUID, int(publication.VersionNo)+1)
	require.NoError(t, err)
	blocked := filepath.Join(f.root, filepath.FromSlash(relDir))
	require.NoError(t, os.MkdirAll(filepath.Dir(blocked), 0o755))
	require.NoError(t, os.WriteFile(blocked, []byte("占位"), 0o644))
	// 补偿失败：只替换快照清理这一个依赖，其余走真实实现。
	f.admin.removeSnapshot = func(string, string, core.ResourceUID, int) error {
		return errors.New("模拟：快照清理失败")
	}

	_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "补偿会失败")
	require.Error(t, err)

	ops := f.withdrawOperations(t)
	require.Len(t, ops, 1)
	require.Equal(t, core.OperationUnknown, ops[0].Status, "补偿失败必须记 unknown")
	require.Equal(t, core.AdmissionPublished, f.mustCandidate(t, candidate.CandidateID).State)
	require.Equal(t, publication.VersionID, f.mustPointer(t, name).PublishedVersionID)
}

// TestUS21_Withdraw_WorkCopyUncertainRecordedUnknown 是 review ② 阻断项 F-01 在撤回
// 准备路径的回归：工作副本交换失败且回滚也失败（ErrWorkCopyUncertain）时磁盘状态
// 不确定——操作记 unknown、不尝试自动复原、不写指针；候选保持 published。
// 判别力：把 prepareWithdrawVersion 的 ErrWorkCopyUncertain 分支删掉，操作由
// unknown 变 failed，状态断言转红。
func TestUS21_Withdraw_WorkCopyUncertainRecordedUnknown(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	const name = "weekly-report"
	f.importSkill(t, name, us21VersionFiles(name, "v1"))
	candidate, _ := f.proposeAndApprove(t, name, us21VersionFiles(name, "v2"))
	publication, err := f.admin.Publish(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	f.admin.writeWorkCopy = func(root string, cand workspace.SkillCandidate) error {
		// 先把上一版内容写进工作副本（撤回准备的真实方向），再报"回滚失败"。
		if err := workspace.WriteSkillWorkCopy(root, cand); err != nil {
			return err
		}
		return fmt.Errorf("回滚目录失败: %w", workspace.ErrWorkCopyUncertain)
	}

	_, err = f.admin.Withdraw(t.Context(), f.actor, candidate.CandidateID, "回退到 v1")
	require.Error(t, err)

	ops := f.withdrawOperations(t)
	require.Len(t, ops, 1)
	require.Equal(t, core.OperationUnknown, ops[0].Status, "工作副本不确定必须记 unknown")
	require.Equal(t, core.AdmissionPublished, f.mustCandidate(t, candidate.CandidateID).State)
	require.Equal(t, publication.VersionID, f.mustPointer(t, name).PublishedVersionID,
		"指针不得移动")
	require.Equal(t, us21VersionFiles(name, "v1")[0].Content, f.workCopyBody(t, name),
		"不得自动复原工作副本，留现场人工裁决")
	require.Equal(t, 2, f.countVersions(t, name), "不得写撤回版本行（v1 + v2 之外不再新增）")
}

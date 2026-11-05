//go:build integration

package app

// 本文件是 US-21 审计与身份的验收面（颗粒度文档 §2.3.2、§4.2 FR-21-04 / FR-21-10、
// §4.3.8、§4.3.12、C-17、T013）：
//
//	AU1 `TestUS21_Audit_PreWriteBeforeMutation`——七个写动作共用一条判据：审计预写
//	    失败时动作拒绝，磁盘与数据库都不得出现该动作的任何变更
//	AU2 `TestUS21_WriteActions_RejectUntrustedActor`——五个非法身份 × 七个写动作，
//	    再对六个带候选 ID 的动作用不存在的候选复测同一批身份，证明身份校验先于状态读取
//	AU3 `TestUS21_Audit_DetailJSONEmpty`——七个动作各成功执行一次，全部
//	    `management_operations.detail_json` 恒为 `{}`，不复制候选正文或来源
//
// 判据只看真实磁盘与真实数据库行；唯一的人为注入点是"审计预写必失败"的仓储包装，
// 它不调用真实实现、只返回固定错误。
// Every judgement reads real files and real rows; the only injection is a store
// wrapper whose BeginOperation returns a fixed error without touching storage.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// us21AuditBodySentinel 是审计用例候选正文里的唯一标记：若 `detail_json` 复制了
// 候选内容，检索到这个标记即判红。来源声明另带独立标记，两条来源都不许进审计表。
// The sentinels make "audit rows never copy candidate content" observable.
const us21AuditBodySentinel = "技能正文哨兵-us21-audit"

// us21AuditFiles 造一份合法候选文件集，正文带唯一标记。
// us21AuditFiles builds a valid candidate file set carrying the sentinel.
func us21AuditFiles(skillName string) []core.AuthorFile {
	return []core.AuthorFile{{
		Path: "SKILL.md",
		Content: "---\nname: " + skillName + "\ndescription: 审计用例\n---\n" +
			us21AuditBodySentinel + "\n",
	}}
}

// us21FailingOperationStore 让审计预写固定失败，其余管理操作方法原样转发。
// 它不委托真实实现，所以失败路径不可能留下 pending 行。
// us21FailingOperationStore fails BeginOperation only and forwards everything else.
type us21FailingOperationStore struct {
	core.OperationStore
	err error
}

// BeginOperation 返回固定错误，模拟审计存储不可用。
// BeginOperation returns the configured error without touching storage.
func (s us21FailingOperationStore) BeginOperation(context.Context, core.OperationRecord) error {
	return s.err
}

// failBeginOperation 用"审计不可用"的仓储重建同一编排器：其它依赖（真实仓储、
// 资源锁、固定时钟）与生产装配逐字一致。
// failBeginOperation rebuilds the orchestrator with only Operations swapped.
func (f *us21AdminFixture) failBeginOperation(t *testing.T, err error) {
	t.Helper()
	deps := f.deps
	deps.Operations = us21FailingOperationStore{OperationStore: f.deps.Operations, err: err}
	f.admin = NewAdmissionAdmin(deps)
}

// us21AuditSnapshot 汇总磁盘与数据库两侧的可比较证据：候选目录名与逐文件哈希、
// 候选 / 评估 / 决策 / 发布 / 操作行数、技能版本数与发布指针版本号。
// us21AuditSnapshot folds the disk and database evidence into one comparable value.
type us21AuditSnapshot struct {
	CandidateDirs   []string
	CandidateFiles  []string
	Candidates      int
	Evaluations     int
	Decisions       int
	Publications    int
	Operations      int
	SkillVersions   int
	PointerVersions map[string]int64
}

// snapshot 读一份两侧快照；排序与聚合全部在 Go 侧完成，行序不影响比较。
// snapshot reads the disk and database evidence; ordering never affects comparison.
func (f *us21AdminFixture) snapshot(t *testing.T) us21AuditSnapshot {
	t.Helper()
	snap := us21AuditSnapshot{PointerVersions: map[string]int64{}}
	snap.CandidateDirs = f.candidateDirs(t)
	for _, name := range snap.CandidateDirs {
		dir := filepath.Join(f.root, us21CandidateDirRel(name))
		err := filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			snap.CandidateFiles = append(snap.CandidateFiles,
				filepath.ToSlash(rel)+"="+core.AdmissionSHA256Hex(raw))
			return nil
		})
		require.NoError(t, err)
	}
	for _, count := range []struct {
		query string
		into  *int
	}{
		{`SELECT count(*) FROM skill_admission_candidates`, &snap.Candidates},
		{`SELECT count(*) FROM skill_admission_evaluations`, &snap.Evaluations},
		{`SELECT count(*) FROM skill_admission_decisions`, &snap.Decisions},
		{`SELECT count(*) FROM skill_admission_publications`, &snap.Publications},
		{`SELECT count(*) FROM management_operations`, &snap.Operations},
		{`SELECT count(*) FROM resource_versions WHERE resource_kind = $1`, &snap.SkillVersions},
	} {
		var err error
		if count.query == `SELECT count(*) FROM resource_versions WHERE resource_kind = $1` {
			err = f.db.QueryRowContext(t.Context(), count.query,
				string(core.ResourceSkill)).Scan(count.into)
		} else {
			err = f.db.QueryRowContext(t.Context(), count.query).Scan(count.into)
		}
		require.NoError(t, err)
	}
	rows, err := f.db.QueryContext(t.Context(),
		`SELECT p.resource_id, v.version_no
		   FROM resource_pointers p
		   JOIN resource_versions v ON v.id = p.published_version_id
		  WHERE p.resource_kind = $1`, string(core.ResourceSkill))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			name      string
			versionNo int64
		)
		require.NoError(t, rows.Scan(&name, &versionNo))
		snap.PointerVersions[name] = versionNo
	}
	require.NoError(t, rows.Err())
	return snap
}

// TestUS21_Audit_PreWriteBeforeMutation：前置状态用正常审计建立，随后审计预写失败，
// 动作必须拒绝且磁盘与数据库都不得出现该动作的任何变更。
// 逆改点：把任一动作的 `BeginOperation` 移到磁盘写入或事务之后，该子用例的
// `snapshot` 前后不等而红；把预写从动作里删掉，`require.Error` 先红。
// TestUS21_Audit_PreWriteBeforeMutation pins audit-before-mutation on all seven actions.
func TestUS21_Audit_PreWriteBeforeMutation(t *testing.T) {
	type pendingAction func() error
	cases := []struct {
		name    string
		prepare func(t *testing.T, f *us21AdminFixture) pendingAction
	}{
		{"propose", func(t *testing.T, f *us21AdminFixture) pendingAction {
			req := f.proposeRequest("weekly-report")
			return func() error {
				_, err := f.admin.Propose(t.Context(), f.actor, req)
				return err
			}
		}},
		{"evaluate", func(t *testing.T, f *us21AdminFixture) pendingAction {
			candidate, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
			require.NoError(t, err)
			return func() error {
				_, err := f.admin.Evaluate(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
				return err
			}
		}},
		{"approve", func(t *testing.T, f *us21AdminFixture) pendingAction {
			c := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
			decision := f.decisionFile(t, c.CandidateID, core.AdmissionApprove)
			return func() error {
				_, err := f.admin.Approve(t.Context(), f.actor, c.CandidateID, decision, admissionEvalPolicy())
				return err
			}
		}},
		{"reject", func(t *testing.T, f *us21AdminFixture) pendingAction {
			c := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
			decision := f.decisionFile(t, c.CandidateID, core.AdmissionReject)
			return func() error {
				_, err := f.admin.Reject(t.Context(), f.actor, c.CandidateID, decision)
				return err
			}
		}},
		{"cancel", func(t *testing.T, f *us21AdminFixture) pendingAction {
			c := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
			return func() error {
				_, err := f.admin.Cancel(t.Context(), f.actor, c.CandidateID, "来源写错，重做")
				return err
			}
		}},
		{"publish", func(t *testing.T, f *us21AdminFixture) pendingAction {
			c := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
			f.mustApprove(t, f.actor, c.CandidateID,
				f.decisionFile(t, c.CandidateID, core.AdmissionApprove))
			return func() error {
				_, err := f.admin.Publish(t.Context(), f.actor, c.CandidateID, admissionEvalPolicy())
				return err
			}
		}},
		{"withdraw", func(t *testing.T, f *us21AdminFixture) pendingAction {
			// 撤回需要上一版：先手工发布 v1，再准入发布 v2，确保动作不会在"首版
			// 拒绝"处提前失败——否则用例会对错因通过。
			// Seed a previous version so the action reaches the pre-write.
			const name = "weekly-report"
			f.importSkill(t, name, us21VersionFiles(name, "v1"))
			c := f.proposeAndEvaluate(t, name, us21VersionFiles(name, "v2"))
			f.mustApprove(t, f.actor, c.CandidateID,
				f.decisionFile(t, c.CandidateID, core.AdmissionApprove))
			_, err := f.admin.Publish(t.Context(), f.actor, c.CandidateID, admissionEvalPolicy())
			require.NoError(t, err)
			return func() error {
				_, err := f.admin.Withdraw(t.Context(), f.actor, c.CandidateID, "回退")
				return err
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUS21AdminFixture(t)
			act := tc.prepare(t, f) // 前置步骤使用正常审计
			before := f.snapshot(t)
			f.failBeginOperation(t, errors.New("audit down"))
			err := act()
			require.Error(t, err, "审计预写失败必须拒绝动作")
			// 拒绝必须发生在审计预写处：其它早期失败（缺候选 / 首版撤回等）会让
			// 快照断言无判别力地通过。
			require.ErrorContains(t, err, "预写管理操作失败",
				"动作必须在审计预写处拒绝，而不是更早的原因")
			require.Equal(t, before, f.snapshot(t), "审计预写失败不得留下磁盘或数据库变更")
		})
	}
}

// TestUS21_WriteActions_RejectUntrustedActor：五个非法身份 × 七个写动作；再对六个
// 带候选 ID 的动作用不存在的候选复测同一批身份——身份校验必须发生在读候选之前。
// 逆改点：把身份校验放宽为只比较 `Subject != "cli:unknown"`，空主体与非 `cli:`
// 主体会真正写入，`snapshot` 前后不等而红；把任一写方法的 `checkTrustedActor` 移到
// `lockCandidate` 之后，missing 那一轮先报"缺候选"而红。
// TestUS21_WriteActions_RejectUntrustedActor pins identity checks before state reads.
func TestUS21_WriteActions_RejectUntrustedActor(t *testing.T) {
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	request := f.proposeRequest("other-skill")
	approve := f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove)
	reject := f.decisionFile(t, candidate.CandidateID, core.AdmissionReject)
	badActors := []core.Actor{
		{Subject: "", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: "alice", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: core.ActorSubjectCLIPrefix + "unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: f.actor.Subject, Method: "bearer", Source: core.AuthMethodCLI},
		{Subject: f.actor.Subject, Method: core.AuthMethodCLI, Source: "web"},
	}
	actions := []struct {
		name string
		want error
		call func(core.Actor) error
	}{
		{"propose", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Propose(t.Context(), a, request)
			return err
		}},
		{"evaluate", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Evaluate(t.Context(), a, candidate.CandidateID, admissionEvalPolicy())
			return err
		}},
		{"approve", core.ErrAdmissionUnknownApprover, func(a core.Actor) error {
			_, err := f.admin.Approve(t.Context(), a, candidate.CandidateID, approve, admissionEvalPolicy())
			return err
		}},
		{"reject", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Reject(t.Context(), a, candidate.CandidateID, reject)
			return err
		}},
		{"cancel", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Cancel(t.Context(), a, candidate.CandidateID, "取消")
			return err
		}},
		{"publish", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Publish(t.Context(), a, candidate.CandidateID, admissionEvalPolicy())
			return err
		}},
		{"withdraw", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Withdraw(t.Context(), a, candidate.CandidateID, "撤回")
			return err
		}},
	}
	// 不存在的候选 ID：不可信身份必须先于"读候选"被拒；若实现把 checkTrustedActor
	// 移到 lockCandidate 之后，这里会变成 CANDIDATE_MISSING。
	missingActions := []struct {
		name string
		want error
		call func(core.Actor) error
	}{
		{"evaluate", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Evaluate(t.Context(), a, "adm_missing", admissionEvalPolicy())
			return err
		}},
		{"approve", core.ErrAdmissionUnknownApprover, func(a core.Actor) error {
			_, err := f.admin.Approve(t.Context(), a, "adm_missing", approve, admissionEvalPolicy())
			return err
		}},
		{"reject", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Reject(t.Context(), a, "adm_missing", reject)
			return err
		}},
		{"cancel", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Cancel(t.Context(), a, "adm_missing", "取消")
			return err
		}},
		{"publish", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Publish(t.Context(), a, "adm_missing", admissionEvalPolicy())
			return err
		}},
		{"withdraw", core.ErrAdmissionUntrustedActor, func(a core.Actor) error {
			_, err := f.admin.Withdraw(t.Context(), a, "adm_missing", "撤回")
			return err
		}},
	}
	before := f.snapshot(t)
	for _, action := range actions {
		for _, actor := range badActors {
			err := action.call(actor)
			require.ErrorIsf(t, err, action.want, "%s with %+v", action.name, actor)
		}
	}
	for _, action := range missingActions {
		for _, actor := range badActors {
			err := action.call(actor)
			require.ErrorIsf(t, err, action.want, "%s with missing candidate and %+v", action.name, actor)
		}
	}
	require.Equal(t, before, f.snapshot(t), "被拒绝的写动作不得留下任何磁盘或数据库变更")
}

// TestUS21_Audit_DetailJSONEmpty：七个动作各真实成功执行一次，全部
// `target_kind=skill_admission` 的操作行 `detail_json` 必须逐字为 `{}`，不得出现
// 候选正文或来源正文。逆改点：把候选正文 / 决策理由塞进 `DetailJSON`，逐行断言红。
// TestUS21_Audit_DetailJSONEmpty pins the empty detail object on every action row.
func TestUS21_Audit_DetailJSONEmpty(t *testing.T) {
	f := newUS21AdminFixture(t)
	const (
		mainSkill   = "weekly-report"
		rejectSkill = "monthly-report"
		cancelSkill = "quarterly-report"
		// 来源声明的 ref 带同一标记：若 detail_json 复制来源，检索即红。
		auditSource = `{"kind":"manual","ref":"` + us21AuditBodySentinel + ` 来源",` +
			`"task_outcome":"not_applicable","confirmation":"direct",` +
			`"authorization":"declared","redaction":"declared","applicable":true,` +
			`"evidence":[],"counterexamples":[],"limitations":[]}`
	)
	// 撤回需要上一版：手工发布 v1 先占位，准入发布 v2 后撤回走 restored_previous。
	f.importSkill(t, mainSkill, us21VersionFiles(mainSkill, "v1"))
	published := f.proposeAndEvaluateWithSource(t, mainSkill, us21AuditFiles(mainSkill), []byte(auditSource))
	f.mustApprove(t, f.actor, published.CandidateID,
		f.decisionFile(t, published.CandidateID, core.AdmissionApprove))
	_, err := f.admin.Publish(t.Context(), f.actor, published.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)
	_, err = f.admin.Withdraw(t.Context(), f.actor, published.CandidateID, "回退到 v1")
	require.NoError(t, err)

	// 拒绝路径。
	rejected := f.proposeAndEvaluateWithSource(t, rejectSkill, us21AuditFiles(rejectSkill), []byte(auditSource))
	_, err = f.admin.Reject(t.Context(), f.actor, rejected.CandidateID,
		f.decisionFile(t, rejected.CandidateID, core.AdmissionReject))
	require.NoError(t, err)

	// 取消路径。
	canceled := f.proposeAndEvaluateWithSource(t, cancelSkill, us21AuditFiles(cancelSkill), []byte(auditSource))
	_, err = f.admin.Cancel(t.Context(), f.actor, canceled.CandidateID, "来源写错，重做")
	require.NoError(t, err)

	rows, err := f.db.QueryContext(t.Context(),
		`SELECT action, detail_json::text FROM management_operations
		  WHERE target_kind = $1 ORDER BY action`, string(core.TargetKindSkillAdmission))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	for rows.Next() {
		var (
			action string
			detail string
		)
		require.NoError(t, rows.Scan(&action, &detail))
		seen[action] = true
		require.Equal(t, "{}", detail, "%s 的 detail_json 必须是空对象", action)
		require.NotContains(t, detail, us21AuditBodySentinel,
			"%s 的 detail_json 不得复制候选正文或来源", action)
	}
	require.NoError(t, rows.Err())
	for _, action := range []string{
		core.ActionSkillAdmissionPropose,
		core.ActionSkillAdmissionEvaluate,
		core.ActionSkillAdmissionApprove,
		core.ActionSkillAdmissionReject,
		core.ActionSkillAdmissionCancel,
		core.ActionSkillAdmissionPublish,
		core.ActionSkillAdmissionWithdraw,
	} {
		require.True(t, seen[action], "缺少 %s 操作行", action)
	}
}

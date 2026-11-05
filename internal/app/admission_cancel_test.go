//go:build integration

package app

// 本文件是 US-21 取消与派生过期的验收面（颗粒度文档 §2.3.4 `Cancel`、§2.5 失败矩阵、
// §2.3.1 状态机、T014）：
//
//	L1 `TestUS21_Cancel_SupersedesActiveApproval`——取消把活跃批准置 superseded、
//	   落终态理由、回填操作终态，决策行与证据一条不删
//	L2 `TestUS21_Cancel_StateGate`——只允许 proposed / evaluated / approved；已终态的
//	   候选再取消必须在预写之前拒绝
//	L3 `TestUS21_Cancel_RejectsUntrustedActor`——身份校验先于状态读取
//	L4 `TestUS21_Cancel_ReasonValidation`——空 / 超限 / 含凭证的理由零写入
//	L5 `TestUS21_State_ExpiredIsDerived`——过期只是读侧派生，数据库行保持 approved
//
// Every case reads real rows and real files; the clock is the fixture's injected clock,
// so expiry is exercised without sleeping.

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// countOperations 按动作数管理操作行数。
// countOperations counts management operations for one action.
func (f *us21AdminFixture) countOperations(t *testing.T, action string) int {
	t.Helper()
	return len(f.operations(t, core.OperationFilter{Action: action}))
}

// L1：取消后活跃批准被取代、候选落 canceled 终态、操作终态回填，决策行保留。
func TestUS21_Cancel_SupersedesActiveApproval(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	approval := f.mustApprove(t, f.actor, candidate.CandidateID,
		f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove))
	revisionBefore := f.mustCandidate(t, candidate.CandidateID).Revision

	canceled, err := f.admin.Cancel(t.Context(), f.actor, candidate.CandidateID, "来源写错，重做")
	require.NoError(t, err)
	require.Equal(t, core.AdmissionCanceled, canceled.State)
	require.Equal(t, "来源写错，重做", canceled.TerminalReason)
	require.NotEmpty(t, canceled.TerminalOperationID)
	require.Equal(t, revisionBefore+1, canceled.Revision)

	// 活跃批准被取代：部分唯一索引本来就不允许第二条，这里断言"零条"。
	_, err = f.store.ActiveApproval(t.Context(), candidate.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionNotFound)
	require.Zero(t, f.countActiveApprovals(t, candidate.CandidateID))
	// 证据不删：批准决策行与候选目录都还在（C-19）。
	latest, err := f.store.LatestDecision(t.Context(), candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, approval.DecisionID, latest.DecisionID)
	require.False(t, latest.SupersededAt.IsZero(), "取消必须把批准标记为被取代")
	require.Equal(t, canceled.TerminalOperationID, latest.SupersededBy,
		"superseded_by 写取代它的操作 ID，不是决策 ID")
	require.Equal(t, 1, f.countDecisions(t, candidate.CandidateID))
	require.Len(t, f.candidateDirs(t), 1)

	// 操作终态：同一操作号 success，动作与目标类型固定。
	operations := f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionCancel})
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationSuccess, operations[0].Status)
	require.Equal(t, canceled.TerminalOperationID, operations[0].OperationID)
	require.Equal(t, core.TargetKindSkillAdmission, operations[0].TargetKind)
	require.Equal(t, candidate.CandidateID, operations[0].TargetID)
	require.Equal(t, "{}", string(operations[0].DetailJSON))
}

// L2：允许的三种起始态都能取消；已终态的候选再取消必须被状态门禁挡在预写之前。
func TestUS21_Cancel_StateGate(t *testing.T) {
	t.Parallel()
	for _, start := range []string{"proposed", "evaluated", "approved"} {
		t.Run(start, func(t *testing.T) {
			t.Parallel()
			f := newUS21AdminFixture(t)
			candidate, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
			require.NoError(t, err)
			switch start {
			case "evaluated", "approved":
				_, err = f.admin.Evaluate(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
				require.NoError(t, err)
			}
			if start == "approved" {
				f.mustApprove(t, f.actor, candidate.CandidateID,
					f.decisionFile(t, candidate.CandidateID, core.AdmissionApprove))
			}
			canceled, err := f.admin.Cancel(t.Context(), f.actor, candidate.CandidateID, "不再需要")
			require.NoError(t, err, "起始态 %s 允许取消", start)
			require.Equal(t, core.AdmissionCanceled, canceled.State)
		})
	}

	// 已终态：再取消不写新操作行、不动候选行。
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	_, err := f.admin.Cancel(t.Context(), f.actor, candidate.CandidateID, "第一次取消")
	require.NoError(t, err)
	before := f.mustCandidate(t, candidate.CandidateID)
	operationsBefore := f.countOperations(t, core.ActionSkillAdmissionCancel)

	_, err = f.admin.Cancel(t.Context(), f.actor, candidate.CandidateID, "第二次取消")
	require.ErrorIs(t, err, core.ErrAdmissionInvalidState)
	require.Contains(t, err.Error(), "不允许取消")
	require.Equal(t, before, f.mustCandidate(t, candidate.CandidateID))
	require.Equal(t, operationsBefore, f.countOperations(t, core.ActionSkillAdmissionCancel),
		"状态门禁必须先于审计预写")

	// 已拒绝的候选同样不能再取消。
	rejected := f.proposeAndEvaluate(t, "monthly-report", admissionEvalFiles("monthly-report"))
	_, err = f.admin.Reject(t.Context(), f.actor, rejected.CandidateID,
		f.decisionFile(t, rejected.CandidateID, core.AdmissionReject))
	require.NoError(t, err)
	_, err = f.admin.Cancel(t.Context(), f.actor, rejected.CandidateID, "拒绝之后")
	require.ErrorIs(t, err, core.ErrAdmissionInvalidState)
}

// L3：非可信本机主体在读取候选之前被拒；不存在的候选也先报身份错误。
func TestUS21_Cancel_RejectsUntrustedActor(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))

	for _, actor := range []core.Actor{
		{}, // 空主体
		{Subject: "admin", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: "cli:alice", Method: "bearer", Source: core.AuthMethodCLI},
		{Subject: "cli:alice", Method: core.AuthMethodCLI, Source: "web"},
		{Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
	} {
		_, err := f.admin.Cancel(t.Context(), actor, candidate.CandidateID, "不应执行")
		require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor, "主体 %+v", actor)
	}
	require.Zero(t, f.countOperations(t, core.ActionSkillAdmissionCancel))
	require.Equal(t, core.AdmissionEvaluated, f.mustCandidate(t, candidate.CandidateID).State)

	nonexistent, err := core.NewAdmissionCandidateID()
	require.NoError(t, err)
	_, err = f.admin.Cancel(t.Context(),
		core.Actor{Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		nonexistent, "不应执行")
	require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor)
	require.NotErrorIs(t, err, core.ErrAdmissionNotFound, "身份校验必须早于状态读取")
}

// L4：空理由 / 超长理由 / 含凭证的理由都在预写之前拒绝，且零写入。
func TestUS21_Cancel_ReasonValidation(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	before := f.mustCandidate(t, candidate.CandidateID)

	cases := []struct {
		name   string
		reason string
		want   error
	}{
		{"空理由", "", core.ErrAdmissionInvalidInput},
		{"超长理由", strings.Repeat("理", admissionReasonMaxRunes+1), core.ErrAdmissionInvalidInput},
		{"含凭证", "口令是 sk-live-9f8e7d6c5b4a3210", core.ErrAdmissionSecretDetected},
	}
	for _, tc := range cases {
		_, err := f.admin.Cancel(t.Context(), f.actor, candidate.CandidateID, tc.reason)
		require.ErrorIs(t, err, tc.want, tc.name)
		require.NotContains(t, err.Error(), "sk-live-9f8e7d6c5b4a3210", "错误不得回显凭证")
	}
	require.Zero(t, f.countOperations(t, core.ActionSkillAdmissionCancel))
	require.Equal(t, before, f.mustCandidate(t, candidate.CandidateID))

	// 恰好等于上限的理由合法（边界是 ≤2000 字符）。
	_, err := f.admin.Cancel(t.Context(), f.actor, candidate.CandidateID,
		strings.Repeat("理", admissionReasonMaxRunes))
	require.NoError(t, err)
}

// L5：`expired` 是读侧派生——到期后详情与列表显示 expired，数据库行仍是 approved，
// 没有任何后台作业改写状态；已过期批准也不会再出现在 `--state approved` 的结果里。
func TestUS21_State_ExpiredIsDerived(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	// `expired` 过滤由仓储按真实时钟翻译；把夹具时钟放到 48 小时前，让「批准时有效、
	// 之后过期」在两处判据上同时成立，且不依赖运行时刻。
	f.now = time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	candidate := f.proposeAndEvaluate(t, "weekly-report", admissionEvalFiles("weekly-report"))
	f.mustApprove(t, f.actor, candidate.CandidateID,
		f.decisionFileWithExpiry(t, candidate.CandidateID, f.now.Add(time.Hour)))

	// 应用时钟（夹具）下尚未过期：详情显示 approved。
	detail, err := f.admin.Get(t.Context(), candidate.CandidateID, core.AdmissionShowOptions{})
	require.NoError(t, err)
	require.Equal(t, core.AdmissionApproved, detail.Candidate.State)

	// 时钟推到有效期的**等值点**（文档口径：now >= expires_at 即过期；这里同时锁住
	// 边界是闭区间，把比较写成 now > expires_at 会让本断言红）。
	f.advance(time.Hour)
	expiredList, err := f.admin.List(t.Context(), core.AdmissionFilter{State: core.AdmissionExpired})
	require.NoError(t, err)
	require.Len(t, expiredList, 1)
	require.Equal(t, core.AdmissionExpired, expiredList[0].State)

	detail, err = f.admin.Get(t.Context(), candidate.CandidateID, core.AdmissionShowOptions{})
	require.NoError(t, err)
	require.Equal(t, core.AdmissionExpired, detail.Candidate.State)
	require.NotNil(t, detail.Decision)
	require.Equal(t, core.AdmissionApprove, detail.Decision.Decision,
		"派生只改展示态，决策行仍是 approve")

	// `--state approved` 不再返回已过期的批准。
	approvedList, err := f.admin.List(t.Context(), core.AdmissionFilter{State: core.AdmissionApproved})
	require.NoError(t, err)
	require.Empty(t, approvedList)

	// 数据库里的状态从未被改写，也没有任何后台作业留下的审计行：
	// 整个过程只有 propose / evaluate / approve 三条操作。
	row := f.mustCandidate(t, candidate.CandidateID)
	require.Equal(t, core.AdmissionApproved, row.State)
	require.Equal(t, 1, f.countDecisions(t, candidate.CandidateID))
	operations := f.operations(t, core.OperationFilter{TargetID: candidate.CandidateID})
	require.Len(t, operations, 3)
	for _, operation := range operations {
		require.Equal(t, core.OperationSuccess, operation.Status, operation.Action)
	}
}

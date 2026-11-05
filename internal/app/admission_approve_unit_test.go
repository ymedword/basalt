package app

// 本文件是 T013 的纯函数判据（颗粒度文档 §4.3.14
// `TestUS21_AdmissionBlockedDimensions_InconsistentRow`）：不连数据库，直接验证
// 「聚合与维度不一致时，阻止维度仍然不可确认」的防御分支。
//
// The unit-level case locks the defensive branch that survives an inconsistent row:
// a `failed` dimension blocks approval even when the aggregate string says otherwise.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestUS21_AdmissionBlockedDimensions_InconsistentRow：聚合写成 incomplete，但维度里
// 有一条 failed——阻止集合必须只列 failed 的那条，且确认白名单里的 effect 也救不了它。
// TestUS21_AdmissionBlockedDimensions_InconsistentRow: a failed dimension blocks
// approval even when the aggregate string disagrees.
func TestUS21_AdmissionBlockedDimensions_InconsistentRow(t *testing.T) {
	t.Parallel()
	evaluation := core.AdmissionEvaluation{
		Aggregate: core.AdmissionAggregateIncomplete,
		Dimensions: []core.AdmissionDimension{
			{Name: core.AdmissionDimensionEffect, Status: core.AdmissionDimensionNotEvaluable, Reason: "real_model_not_authorized"},
			{Name: core.AdmissionDimensionSource, Status: core.AdmissionDimensionFailed, Reason: "confirmation_unconfirmed"},
		},
	}
	require.Equal(t, []core.AdmissionDimensionName{core.AdmissionDimensionSource},
		admissionBlockedDimensions(evaluation))
	input := core.AdmissionDecisionInput{Acknowledged: []core.AdmissionDimensionName{"effect"}}
	require.ErrorIs(t, checkAdmissionApprovalBoundary(evaluation, input),
		core.ErrAdmissionDimensionBlocked)
}

// TestUS21_State_ExpiredPredicateGuards 把 `admissionEffectiveState` 的守卫分支逐条
// 钉住：非 approved、决策缺失、非 approve、被 supersede、零有效期都按存储态返回；
// 等值边界（`now == expires_at`）派生 expired，之前 approved、之后 expired。它与
// internal/web 的 `TestUS21_AdmissionList_DerivedExpired` 同口径对称——两侧各持一份
// 谓词，改单边而不改另一侧必须有用例红（契约 §状态机）。
// TestUS21_State_ExpiredPredicateGuards pins every guard branch of the app-side
// derived-state predicate, mirroring the web-side table.
func TestUS21_State_ExpiredPredicateGuards(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	approved := core.AdmissionCandidate{State: core.AdmissionApproved}
	decision := func(mod func(*core.AdmissionDecision)) *core.AdmissionDecision {
		d := core.AdmissionDecision{Decision: core.AdmissionApprove, ExpiresAt: now.Add(time.Hour)}
		mod(&d)
		return &d
	}
	cases := []struct {
		name      string
		candidate core.AdmissionCandidate
		decision  *core.AdmissionDecision
		want      core.AdmissionState
	}{
		{
			"非 approved 不派生",
			core.AdmissionCandidate{State: core.AdmissionPublished},
			decision(func(*core.AdmissionDecision) {}), core.AdmissionPublished,
		},
		{"决策缺失不派生", approved, nil, core.AdmissionApproved},
		{
			"非 approve 不派生", approved,
			decision(func(d *core.AdmissionDecision) { d.Decision = core.AdmissionReject }), core.AdmissionApproved,
		},
		{
			"被 supersede 不派生", approved,
			decision(func(d *core.AdmissionDecision) { d.SupersededAt = now.Add(-time.Minute) }), core.AdmissionApproved,
		},
		{
			"零有效期不派生", approved,
			decision(func(d *core.AdmissionDecision) { d.ExpiresAt = time.Time{} }), core.AdmissionApproved,
		},
		{
			"有效期前一瞬仍是 approved", approved,
			decision(func(d *core.AdmissionDecision) { d.ExpiresAt = now.Add(time.Nanosecond) }), core.AdmissionApproved,
		},
		{
			"等值边界即 expired", approved,
			decision(func(d *core.AdmissionDecision) { d.ExpiresAt = now }), core.AdmissionExpired,
		},
		{
			"过期后 expired", approved,
			decision(func(d *core.AdmissionDecision) { d.ExpiresAt = now.Add(-time.Hour) }), core.AdmissionExpired,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, admissionEffectiveState(tc.candidate, tc.decision, now))
		})
	}
}

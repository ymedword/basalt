package knowledge

// 本文件守 V-8 的摄取配额判定事件：每一次 `AdmitCall` 的放行与拒绝都要留一条
// 可查询记录，锚点是 `ingest_job_id`（冻结契约 §2.9 的关联表），`step_id` 标明
// 作业内的哪一步（`reserve` / `embed:<seq>`），拒绝带原因码、放行不带。
//
// 逆改点（逐个用例写在注释里）：删掉任一发事件点、把批量序号改成恒 0、把放行态
// 也盖原因码——每种都让对应用例先红。诊断端口的可空性也是判据：缺装配只少事件，
// 摄取行为逐字不变。
//
// This file pins the ingest quota-decision events: one record per AdmitCall, anchored
// on the ingest job, with step ids naming the decision point.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// recordingQuotaDiag 收集摄取配额判定事件。
// recordingQuotaDiag collects ingest quota decisions.
type recordingQuotaDiag struct {
	events []IngestQuotaDiagnostic
}

func (r *recordingQuotaDiag) RecordIngestQuota(_ context.Context, ev IngestQuotaDiagnostic) {
	r.events = append(r.events, ev)
}

// TestIngest_QuotaDecisionsAnchorOnJob 守放行路径：一次字节预留 + 每批一次 Token
// 预留，全部锚在同一个作业上，序号与批号同源。
//
// 逆改点：字节预留不发事件 → 首条事件变成 `embed:1`（长度与 step 断言红）；
// 批序号写成常量 0 → `embed:<seq>` 断言红。
func TestIngest_QuotaDecisionsAnchorOnJob(t *testing.T) {
	h := newHarness(t)
	diag := &recordingQuotaDiag{}
	h.quotaDiag = diag

	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)
	require.Equal(t, VersionActive, result.Status)

	require.Len(t, diag.events, 3, "一次字节预留 + 两批 Token 预留")
	for _, ev := range diag.events {
		require.Equal(t, result.IngestJobID, ev.IngestJobID, "所有判定必须锚在同一个作业上")
		require.Equal(t, "ingest", ev.Class)
		require.Empty(t, ev.Reason)
	}

	reserve := diag.events[0]
	require.Equal(t, "reserve", reserve.StepID)
	require.Equal(t, int64(0), reserve.Seq)
	require.Equal(t, DiagnosticDecisionAllow, reserve.Decision)

	require.Equal(t, "embed:1", diag.events[1].StepID)
	require.Equal(t, int64(1), diag.events[1].Seq)
	require.Equal(t, DiagnosticDecisionAllow, diag.events[1].Decision)
	require.Equal(t, "embed:2", diag.events[2].StepID)
	require.Equal(t, int64(2), diag.events[2].Seq)
}

// TestIngest_QuotaDenyReasonsMapToContractCodes 守拒绝路径：三种哨兵各映射一个
// 契约原因码（配额耗尽 / 速率受限 / 并发名额），且拒绝事件不带票据语义。
//
// 逆改点：三个哨兵合并成一个原因码 → 后两行子用例红（恢复条件不同，排障方向
// 不同，合并等于抹掉这个区别）。
func TestIngest_QuotaDenyReasonsMapToContractCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want DiagnosticReason
	}{
		{name: "quota_exhausted", err: ErrQuotaExhausted, want: DiagnosticReasonQuotaExhausted},
		{name: "rate_limited", err: ErrRateLimited, want: DiagnosticReasonRateLimited},
		{name: "concurrency_limited", err: ErrConcurrencyLimited, want: DiagnosticReasonConcurrencyLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			diag := &recordingQuotaDiag{}
			h.quotaDiag = diag
			denyErr := tc.err
			h.quota.deny = func(cost QuotaCost) error {
				if cost.Bytes > 0 {
					return denyErr
				}
				return nil
			}

			_, err := h.build().Ingest(t.Context(), h.request())
			require.ErrorIs(t, err, denyErr, "配额拒绝必须在准入期原样上抛")

			require.Len(t, diag.events, 1, "被拒后不得再发后续判定")
			ev := diag.events[0]
			require.Equal(t, "reserve", ev.StepID)
			require.Equal(t, DiagnosticDecisionDeny, ev.Decision)
			require.Equal(t, tc.want, ev.Reason)
		})
	}
}

// TestIngest_TokenDenyRecordsBatchStep 守批次维度的拒绝：第二批被拒时，事件带
// `embed:2` 与批序号，作业落失败终态（契约 200 的 failed 语义）。
//
// 逆改点：拒绝不发事件 → 长度断言红；step 写成常量 → `embed:2` 断言红。
func TestIngest_TokenDenyRecordsBatchStep(t *testing.T) {
	h := newHarness(t)
	diag := &recordingQuotaDiag{}
	h.quotaDiag = diag
	tokenAdmits := 0
	h.quota.deny = func(cost QuotaCost) error {
		if cost.Tokens <= 0 {
			return nil
		}
		tokenAdmits++
		if tokenAdmits == 2 {
			return ErrConcurrencyLimited
		}
		return nil
	}

	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)
	require.Equal(t, VersionFailed, result.Status)

	require.Len(t, diag.events, 3)
	denied := diag.events[2]
	require.Equal(t, "embed:2", denied.StepID)
	require.Equal(t, int64(2), denied.Seq)
	require.Equal(t, DiagnosticDecisionDeny, denied.Decision)
	require.Equal(t, DiagnosticReasonConcurrencyLimited, denied.Reason)
}

// TestIngest_NilQuotaDiagnosticsStillIngests 守端口可空：未接诊断的实例摄取结果
// 逐字不变（诊断可丢弃 ≠ 诊断影响业务）。
//
// 逆改点：把端口当必填（缺就失败）→ 状态断言红。
func TestIngest_NilQuotaDiagnosticsStillIngests(t *testing.T) {
	h := newHarness(t)
	require.Nil(t, h.quotaDiag)

	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)
	require.Equal(t, VersionActive, result.Status)
}

// TestIngestQuotaReason_UnknownStaysUnattributed 守"诊断不猜"：未知错误留空原因码。
//
// 逆改点：default 分支随便归一个码 → 断言红。
func TestIngestQuotaReason_UnknownStaysUnattributed(t *testing.T) {
	require.Equal(t, DiagnosticReason(""), ingestQuotaReason(nil))
	require.Equal(t, DiagnosticReason(""), ingestQuotaReason(ErrIngestUnavailable))
}

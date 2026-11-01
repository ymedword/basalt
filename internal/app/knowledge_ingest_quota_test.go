package app

// 本文件锁死摄取配额判定适配器的契约面（V-8）：领域字面量 → 冻结契约事件。
// 与 `knowledge_adapters_test.go` 同一纪律——`internal/knowledge` 不 import
// `internal/diagnostics`，两边字面量的一致性只能靠这组用例证明。
//
// 三条判据：
//   - 锚点**只有** `ingest_job_id`（契约 §2.9：摄取作业用它，不得伪造 run_id）；
//   - 空原因码是合法的放行态，未知字面量才丢弃（丢弃留 WARN）；
//   - 事件经真实写入器过 `Validate()`，形状非法在这里红。
//
// This file pins the ingest quota adapter: job-anchored events, allow without a
// reason, and a visible drop for unknown literals.

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/knowledge"
)

// TestKnowledgeIngestQuotaAdapter_AllowAnchorsOnJob 守放行态：种类 `quota_decision`、
// 关联只有作业、step / seq / class 原样、原因码为空。
//
// 逆改点：把锚点换成 run_id → correlation 断言红；给放行补原因码 → 空原因断言红。
func TestKnowledgeIngestQuotaAdapter_AllowAnchorsOnJob(t *testing.T) {
	recorder, store := producerRecorder(t)
	adapter := newKnowledgeIngestQuotaAdapter(recorder, nil)

	adapter.RecordIngestQuota(t.Context(), knowledge.IngestQuotaDiagnostic{
		IngestJobID: "job_7",
		StepID:      "embed:2",
		Seq:         2,
		Class:       "ingest",
		Decision:    knowledge.DiagnosticDecisionAllow,
	})

	require.Len(t, store.events, 1)
	ev := store.events[0]
	require.Equal(t, diagnostics.KindQuotaDecision, ev.Kind)
	require.Equal(t, "job_7", ev.Correlation.IngestJobID)
	require.Empty(t, ev.Correlation.RunID, "摄取事件不得携带 run_id")
	require.Equal(t, "embed:2", ev.StepID)
	require.Equal(t, int64(2), ev.Seq)
	require.Equal(t, "ingest", ev.Attrs["class"])
	require.Equal(t, diagnostics.DecisionAllow, ev.Decision)
	require.Empty(t, ev.ReasonCode)
}

// TestKnowledgeIngestQuotaAdapter_DenyMapsReasons 守拒绝态的三条原因码逐值对齐。
//
// 逆改点：任一领域字面量映射错契约码 → 对应子用例红。
func TestKnowledgeIngestQuotaAdapter_DenyMapsReasons(t *testing.T) {
	cases := []struct {
		name string
		in   knowledge.DiagnosticReason
		want diagnostics.ReasonCode
	}{
		{name: "quota", in: knowledge.DiagnosticReasonQuotaExhausted, want: diagnostics.ReasonQuotaExhausted},
		{name: "rate", in: knowledge.DiagnosticReasonRateLimited, want: diagnostics.ReasonRateLimited},
		{name: "concurrency", in: knowledge.DiagnosticReasonConcurrencyLimited, want: diagnostics.ReasonConcurrencyLimited},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder, store := producerRecorder(t)
			adapter := newKnowledgeIngestQuotaAdapter(recorder, nil)
			adapter.RecordIngestQuota(t.Context(), knowledge.IngestQuotaDiagnostic{
				IngestJobID: "job_7",
				StepID:      "reserve",
				Class:       "ingest",
				Decision:    knowledge.DiagnosticDecisionDeny,
				Reason:      tc.in,
			})
			require.Len(t, store.events, 1)
			require.Equal(t, diagnostics.DecisionDeny, store.events[0].Decision)
			require.Equal(t, tc.want, store.events[0].ReasonCode)
		})
	}
}

// TestKnowledgeIngestQuotaAdapter_DropsUnknownLiterals 守丢弃路径：未知判定 / 未知
// 原因码 / 缺锚点都不落库；未知字面量还要留一行 WARN（丢弃不得静默）。
//
// 逆改点：未知字面量兜底成某个合法值 → 空事件断言红；丢弃不记 WARN → 日志断言红。
func TestKnowledgeIngestQuotaAdapter_DropsUnknownLiterals(t *testing.T) {
	recorder, store := producerRecorder(t)
	var logs bytes.Buffer
	adapter := newKnowledgeIngestQuotaAdapter(recorder, slog.New(slog.NewTextHandler(&logs, nil)))

	adapter.RecordIngestQuota(t.Context(), knowledge.IngestQuotaDiagnostic{
		IngestJobID: "job_7",
		Decision:    knowledge.DiagnosticDecision("maybe"),
	})
	adapter.RecordIngestQuota(t.Context(), knowledge.IngestQuotaDiagnostic{
		IngestJobID: "job_7",
		Decision:    knowledge.DiagnosticDecisionDeny,
		Reason:      knowledge.DiagnosticReason("kaboom"),
	})
	adapter.RecordIngestQuota(t.Context(), knowledge.IngestQuotaDiagnostic{
		Decision: knowledge.DiagnosticDecisionAllow,
	})

	require.Empty(t, store.events)
	require.Contains(t, logs.String(), "ingest quota diagnostic dropped")
}

// TestNewKnowledgeIngestQuotaAdapter_NilWhenUnwired 守可空端口的构造：没有写入器就
// 返回 nil 接口，摄取侧据此短路（typed nil 会让非空判断失效）。
//
// 逆改点：返回装好适配器的接口 → nil 断言红。
func TestNewKnowledgeIngestQuotaAdapter_NilWhenUnwired(t *testing.T) {
	require.Nil(t, newKnowledgeIngestQuotaAdapter(nil, nil))
}

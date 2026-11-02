package observability

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObservabilityPresentationFactsKeepBudgetAndZeroDuration(t *testing.T) {
	f := sameMillisecondFacts()
	f.run.Reason = "context_budget_exceeded"
	f.run.ContextBudget = &ContextBudgetFailure{Required: 66171, Limit: 65536, Reserve: 1024}
	zero := int64(0)
	f.tools[0].DurationMS = &zero
	p := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1})
	got, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}})
	require.NoError(t, err)
	var run, tool TimelineEntry
	for _, e := range got.Entries {
		if e.Source == SourceRun {
			run = e
		}
		if e.Source == SourceTool {
			tool = e
		}
	}
	require.Equal(t, "context_budget_exceeded", run.ReasonCode, "safe classification survives read redaction")
	require.EqualValues(t, 66171, run.Attrs["context_required_tokens"])
	require.EqualValues(t, 65536, run.Attrs["context_limit_tokens"])
	require.EqualValues(t, 1024, run.Attrs["context_reserve_tokens"])
	require.Contains(t, tool.Attrs, "duration_ms")
	require.EqualValues(t, 0, tool.Attrs["duration_ms"])
}

func TestObservabilityTaskReasonIsMarkedAsAssociatedRun(t *testing.T) {
	e := redactTimelineEntry(taskEntry(TaskExecutionFact{ID: 9, RunID: "known-run", Status: "failed", ErrorKind: "context_budget_exceeded", ReasonFromRun: true}))
	require.Equal(t, "associated_run", e.Attrs["reason_source"])
	require.Equal(t, "context_budget_exceeded", e.ReasonCode)
	without := taskEntry(TaskExecutionFact{ID: 10, Status: "failed"})
	require.NotContains(t, without.Attrs, "reason_source")
	require.Empty(t, without.ReasonCode)
}

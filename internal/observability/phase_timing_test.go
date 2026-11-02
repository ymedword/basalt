package observability

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/diagnostics"
)

func TestObservability_PhaseTimingHasRecordedEndAndLegacyEventsStayInstant(t *testing.T) {
	f := sameMillisecondFacts()
	id, err := diagnostics.NewEventID(time.Now())
	require.NoError(t, err)
	start := f.run.StartedAt
	f.events = []diagnostics.DiagnosticEvent{{EventID: id, Kind: diagnostics.KindRetrieval, Correlation: diagnostics.Correlation{RunID: "run_1"}, OccurredAt: start.UnixMilli(), Attrs: map[string]any{"phase": "lexical_search", "phase_parent": "knowledge_retrieval", "phase_status": "completed", "duration_ms": 1.25, "phase_end": start.Add(1250 * time.Microsecond).Format(time.RFC3339Nano)}}}
	p := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1})
	got, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
	require.NoError(t, err)
	var phase TimelineEntry
	for _, e := range got.Entries {
		if e.Source == SourceDiagnostic {
			phase = e
		}
	}
	require.NotNil(t, phase.EndedAt)
	require.Equal(t, EndTimeRecorded, phase.EndSource)
	require.Equal(t, "completed", phase.Status)
	require.Equal(t, "lexical_search", phase.Attrs["phase"], "closed phase names must survive consumer redaction")
	f.events[0].Attrs["phase_end"] = start.Add(-time.Second).Format(time.RFC3339Nano)
	got, err = p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
	require.NoError(t, err)
	for _, e := range got.Entries {
		if e.Source == SourceDiagnostic {
			require.True(t, e.EndedAt.Before(e.OccurredAt))
			require.Equal(t, 1.25, e.Attrs["duration_ms"], "wall-clock faults must not erase independently captured elapsed time")
		}
	}
	f.events[0].Attrs = nil
	got, err = p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: "run_1"}, Limit: 100})
	require.NoError(t, err)
	for _, e := range got.Entries {
		if e.Source == SourceDiagnostic {
			require.Nil(t, e.EndedAt)
			require.Empty(t, e.Status)
		}
	}
}

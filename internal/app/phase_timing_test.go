package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

func TestPhaseTiming_ProductionAdapterWritesSafeLinkedEvidence(t *testing.T) {
	recorder, store := producerRecorder(t)
	sink := newPhaseAdapter(recorder)
	start := time.Now()
	sink.RecordPhase(t.Context(), core.PhaseTiming{RunID: "run-phase", StepID: "1", Phase: core.PhaseFirstResponse, Parent: "model_call", CallID: 42, Status: "completed", StartedAt: start, FinishedAt: start.Add(time.Millisecond), Duration: time.Millisecond})
	require.Len(t, store.events, 1)
	event := store.events[0]
	require.Equal(t, "llm_calls:42", event.AuditRef.String())
	require.Equal(t, "run-phase", event.Correlation.RunID)
	_, ok := diagnostics.ReadPhase(diagnostics.KindLLMCall, event.Attrs)
	require.True(t, ok)
	sink.RecordPhase(t.Context(), core.PhaseTiming{RunID: "run-phase", Phase: "SECRET_QUERY", Status: "completed", StartedAt: start, FinishedAt: start, Duration: 0})
	require.Len(t, store.events, 1)
}

func TestPhaseTiming_EmptyRetrievalDoesNotInventSourceReads(t *testing.T) {
	f := newRunSrcFixture(t)
	recorder, store := producerRecorder(t)
	f.source.withPhases(newPhaseAdapter(recorder))
	injection, err := f.source.RetrieveForRun(t.Context(), runSrcRequest())
	require.NoError(t, err)
	require.Nil(t, injection)
	require.Empty(t, store.events, "no hits must not produce a source-read stage")
}

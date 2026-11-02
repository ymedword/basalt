package diagnostics

import (
	"maps"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhaseTiming_AttrsClosedAndSharedAcrossRedaction(t *testing.T) {
	attrs := map[string]any{"phase": "lexical_search", "phase_parent": "knowledge_retrieval", "phase_status": "completed", "duration_ms": 0.125, "phase_end": "2026-11-02T02:00:00.123456Z", "query": "SECRET_QUERY", "api_key": "SECRET_KEY"}
	redactor := NewWhitelistRedactor()
	got := redactor.Redact(KindRetrieval, attrs)
	require.Len(t, got, 5)
	phase, ok := ReadPhase(KindRetrieval, got)
	require.True(t, ok)
	require.Equal(t, .125, phase.DurationMS)
	require.Equal(t, got, redactor.Redact(KindRetrieval, got), "read/export redaction must not strip measured fields")
	for _, change := range []map[string]any{{"phase": "SECRET_QUERY"}, {"phase_parent": "other"}, {"phase_end": "SECRET_KEY"}, {"duration_ms": math.NaN()}, {"duration_ms": -1.0}, {"phase_status": "SECRET_KEY"}} {
		bad := maps.Clone(attrs)
		maps.Copy(bad, change)
		require.Empty(t, redactor.Redact(KindRetrieval, bad))
	}
	require.Empty(t, redactor.Redact(KindToolCall, attrs), "stage attrs cannot spread to unrelated kinds")
}

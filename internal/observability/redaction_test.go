package observability

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/diagnostics"
)

func TestObservability_RedactionCanaries(t *testing.T) {
	// 真实读取投影两道闸：未知键及敏感值不可外发；第二次脱敏不重复计数。
	// Two read boundaries omit unknown keys and sensitive values without double counting.
	f := sameMillisecondFacts()
	e := diagnostics.DiagnosticEvent{EventID: "event-real", Kind: diagnostics.KindToolCall, OccurredAt: f.run.StartedAt.UnixMilli(), ReasonCode: diagnostics.ReasonLLMCallFailed, Attrs: map[string]any{
		"tool": "safe-tool", "error_kind": "password=CANARY_ERROR", "url": "https://example.invalid/private/CANARY_PATH?token=CANARY_QUERY",
		"unknown_CANARY_KEY": "CANARY_UNKNOWN", "count": 2,
	}}
	reader := &redactionFacts{testFacts: f, reads: []DiagnosticReadEvent{{Event: e.Redacted(nil), HiddenFields: 1}}}
	ports := f.ports()
	ports.Diagnostics = reader
	got, err := NewProjector(ports, ProjectorConfig{SampleRate: 1}).Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: f.run.RunID}})
	require.NoError(t, err)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(b), "CANARY")
	for _, row := range got.Entries {
		if row.Source == SourceDiagnostic {
			require.Equal(t, 2, row.RedactedFields)
			require.Equal(t, "safe-tool", row.Attrs["tool"])
			require.Equal(t, "https://example.invalid", row.Attrs["url"])
		}
	}
}

func TestObservability_RedactionRunFields(t *testing.T) {
	f := sameMillisecondFacts()
	f.run.Reason = "secret=CANARY_RUN"
	got, err := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1}).Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: f.run.RunID}})
	require.NoError(t, err)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(b), "CANARY_RUN")
	require.Equal(t, "unclassified", got.Entries[0].ReasonCode)
}

func TestObservability_NoRawErrorMessages(t *testing.T) {
	f := sameMillisecondFacts()
	f.llm[0].ErrorKind, f.tools[0].ErrorKind = "authorization=CANARY_LLM", "password=CANARY_TOOL"
	got, err := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1}).Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: f.run.RunID}})
	require.NoError(t, err)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(b), "CANARY")
}

type redactionFacts struct {
	*testFacts
	reads []DiagnosticReadEvent
}

func (f *redactionFacts) ListObservationEvents(context.Context, diagnostics.Query) ([]DiagnosticReadEvent, error) {
	return f.reads, nil
}

package observability

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/diagnostics"
)

func TestObservability_RecoveryMetadataPresence(t *testing.T) {
	f := sameMillisecondFacts()
	f.run.Rounds = 9
	f.run.ToolCalls = 2
	recovered := f.run.StartedAt.Add(time.Minute)
	f.llm[0].Status = "unknown"
	f.llm[0].RecoveredAt = &recovered
	f.tools[0].Status = "unknown"
	f.tools[0].FinishedAt = nil
	f.tools[0].RecoveredAt = &recovered
	p := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1})
	read := func() Timeline {
		got, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: f.run.RunID}, Limit: 100})
		require.NoError(t, err)
		return got
	}
	for _, entry := range read().Entries {
		switch entry.Source {
		case SourceRun:
			require.Equal(t, 9, entry.Attrs["rounds"])
			require.Equal(t, 2, entry.Attrs["tool_calls"])
		case SourceLLM, SourceTool:
			require.Equal(t, recovered.Format(time.RFC3339Nano), entry.Attrs["recovered_at"])
			require.Nil(t, entry.EndedAt)
			require.Equal(t, EndTimeUnknown, entry.EndSource)
			require.NotContains(t, entry.Attrs, "prompt_tokens")
		}
	}
	zero := int64(0)
	f.llm[0].PromptTokens = &zero
	f.llm[0].CompletionTokens = &zero
	f.llm[0].RecoveredAt = nil
	f.tools[0].RecoveredAt = nil
	for _, entry := range read().Entries {
		if entry.Source == SourceLLM {
			require.Equal(t, int64(0), entry.Attrs["prompt_tokens"])
			require.Equal(t, int64(0), entry.Attrs["completion_tokens"])
		}
		if entry.Source == SourceLLM || entry.Source == SourceTool {
			require.NotContains(t, entry.Attrs, "recovered_at")
		}
	}
}

func TestObservability_RetrievalAssemblyEvidence(t *testing.T) {
	p, f := contextProjector()
	read := func() map[string]any {
		got, err := p.Context(t.Context(), f.run.RunID)
		require.NoError(t, err)
		raw, err := json.Marshal(got)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		return fields["retrieval"].(map[string]any)["evidence"].(map[string]any)
	}
	require.Equal(t, "direct", read()["assembly"])
	f.assembly = nil
	f.events = []diagnostics.DiagnosticEvent{{EventID: "filter-only", Kind: diagnostics.KindRetrieval, ReasonCode: diagnostics.ReasonPermissionDenied, OccurredAt: f.run.StartedAt.UnixMilli(), Correlation: diagnostics.Correlation{RunID: f.run.RunID}, Attrs: map[string]any{"count": 5}}}
	evidence := read()
	require.Equal(t, "insufficient", evidence["assembly"])
	require.Equal(t, "direct", evidence["filtered_count"])
}

type localMetadataFacts struct{ *testFacts }

func (f localMetadataFacts) EmbeddingCallsByRun(context.Context, string) ([]EmbeddingCallFact, error) {
	return []EmbeddingCallFact{{ID: 2, RunID: f.run.RunID, Model: "synthetic-embedding", Status: "success", CreatedAt: f.run.StartedAt}}, nil
}

func (f localMetadataFacts) OperationFacts(context.Context, string) (ManagementOperationFact, error) {
	return ManagementOperationFact{OperationID: "op-local", Action: "agent.test", Status: "success", ResultCode: "OK", CreatedAt: f.run.StartedAt}, nil
}

func TestObservability_LocalTechnicalMetadata(t *testing.T) {
	f := localMetadataFacts{sameMillisecondFacts()}
	f.llm[0].Model = "synthetic-chat"
	ports := f.ports()
	ports.Exec, ports.Ops = f, f
	p := NewProjector(ports, ProjectorConfig{SampleRate: 1})
	got, err := p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: f.run.RunID}, Limit: 100})
	require.NoError(t, err)
	models := map[SourceKind]any{}
	for _, entry := range got.Entries {
		if entry.Source == SourceLLM || entry.Source == SourceEmbedding {
			models[entry.Source] = entry.Attrs["model"]
		}
	}
	require.Equal(t, map[SourceKind]any{SourceLLM: "synthetic-chat", SourceEmbedding: "synthetic-embedding"}, models)
	got, err = p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorOperation, Value: "op-local"}, Limit: 100})
	require.NoError(t, err)
	require.Equal(t, "OK", got.Entries[0].Attrs["result_code"])
	f.llm[0].Model = "password=CANARY_MODEL_SECRET"
	got, err = p.Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: f.run.RunID}, Limit: 100})
	require.NoError(t, err)
	for _, entry := range got.Entries {
		if entry.Source == SourceLLM {
			require.NotContains(t, entry.Attrs, "model")
		}
	}
}

func TestObservability_LegacyDiagnosticEnumsRedacted(t *testing.T) {
	f := sameMillisecondFacts()
	f.events = []diagnostics.DiagnosticEvent{{EventID: "legacy", Kind: "CANARY_OBS_KIND", ReasonCode: "CANARY_OBS_REASON", OccurredAt: f.run.StartedAt.UnixMilli(), Correlation: diagnostics.Correlation{RunID: f.run.RunID}}}
	got, err := NewProjector(f.ports(), ProjectorConfig{SampleRate: 1}).Timeline(t.Context(), TimelineQuery{Anchor: Anchor{Kind: AnchorRun, Value: f.run.RunID}, Limit: 100})
	require.NoError(t, err)
	found := false
	for _, entry := range got.Entries {
		if entry.Source == SourceDiagnostic {
			found = true
			require.Equal(t, "unknown", entry.Kind)
			require.Equal(t, "unclassified", entry.ReasonCode)
			require.Equal(t, 2, entry.RedactedFields)
		}
	}
	require.True(t, found, "legacy event remains readable without echoing arbitrary enum values")
}

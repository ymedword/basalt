package observability

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/diagnostics"
)

func mapperItem(t *testing.T, source SourceKind, id string, signal SignalKind, fact any) ExportItem {
	t.Helper()
	hash, err := CanonicalStateHash(fact)
	require.NoError(t, err)
	return ExportItem{SourceKind: source, SourceID: id, Signal: signal, StateHash: hash, Fact: fact, SourceTerminalAt: time.UnixMilli(1700000000000).UTC()}
}

func TestObservability_ExportRecoveredLLMCall(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	f := LLMCallFact{ID: 1, RunID: "real-run", Status: "unknown", CreatedAt: now.Add(-72 * time.Hour), RecoveredAt: &now}
	batch, errs := (&Mapper{}).BuildBatch([]ExportItem{mapperItem(t, SourceLLM, "1", SignalLog, f)})
	require.Empty(t, errs)
	require.Equal(t, "log_record", batch.RecordUnit)
	require.Equal(t, 1, batch.RecordCount)
	p := batch.Payload.(TelemetryPayload)
	require.Empty(t, p.Spans)
	require.Len(t, p.Logs, 1)
	require.Equal(t, f.CreatedAt, p.Logs[0].Time)
	require.Equal(t, "recovered_unknown", p.Logs[0].Attributes["end_time_source"])
	require.Equal(t, now.UnixMilli(), p.Logs[0].Attributes["recovered_at"])
	_, err := (&Mapper{}).MapLLMCallSpan(f)
	require.Error(t, err)
}

func TestObservability_ExportRecoveredToolInvocation(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	f := ToolInvocationFact{ID: 1, RunID: "real-run", ToolCallID: "actual-call", Status: "unknown", StartedAt: now.Add(-72 * time.Hour), RecoveredAt: &now}
	batch, errs := (&Mapper{}).BuildBatch([]ExportItem{mapperItem(t, SourceTool, "1", SignalLog, f)})
	require.Empty(t, errs)
	p := batch.Payload.(TelemetryPayload)
	require.Empty(t, p.Spans)
	require.Len(t, p.Logs, 1)
	require.Equal(t, f.StartedAt, p.Logs[0].Time)
	require.Equal(t, "recovered_unknown", p.Logs[0].Attributes["end_time_source"])
	require.Equal(t, "actual-call", p.Logs[0].Attributes["tool_call_id"])
	_, err := (&Mapper{}).MapToolSpan(f)
	require.Error(t, err)
}

func TestObservability_ExportLegacyRecoveredRowUnrecorded(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	f := LLMCallFact{ID: 2, RunID: "legacy", Status: "unknown", CreatedAt: now}
	batch, errs := (&Mapper{}).BuildBatch([]ExportItem{mapperItem(t, SourceLLM, "2", SignalLog, f)})
	require.Empty(t, errs)
	p := batch.Payload.(TelemetryPayload)
	require.Empty(t, p.Spans)
	require.Len(t, p.Logs, 1)
	require.Equal(t, "unrecorded", p.Logs[0].Attributes["end_time_source"])
	require.NotContains(t, p.Logs[0].Attributes, "recovered_at")
}

func TestObservability_ExportEndProvenanceAndIDs(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	duration := int64(0)
	m := &Mapper{}
	f := LLMCallFact{ID: 5, RunID: "real", Status: "success", CreatedAt: now, DurationMS: &duration}
	span, err := m.MapLLMCallSpan(f)
	require.NoError(t, err)
	require.Equal(t, now, span.End)
	require.Equal(t, "recorded_duration", span.Attributes["end_time_source"])
	again, err := m.MapLLMCallSpan(f)
	require.NoError(t, err)
	require.Equal(t, span.SpanID, again.SpanID)
	require.Equal(t, span.TraceID, again.TraceID)
	run, err := m.MapRunSpan(RunFacts{RunID: "real", Status: "completed", StartedAt: now, FinishedAt: &now})
	require.NoError(t, err)
	require.Equal(t, span.TraceID, run.TraceID)
	require.NotEqual(t, span.SpanID, run.SpanID)
	_, err = m.MapRunSpan(RunFacts{RunID: "unfinished", Status: "running", StartedAt: now})
	require.Error(t, err)
	_, err = m.MapLLMCallSpan(LLMCallFact{ID: 6, RunID: "pending", Status: "pending", CreatedAt: now})
	require.Error(t, err)
}

func TestObservability_ExportMappingErrorIsolated(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	good := mapperItem(t, SourceRun, "good", SignalTrace, RunFacts{RunID: "good", Status: "completed", StartedAt: now, FinishedAt: &now})
	bad := mapperItem(t, SourceRun, "bad", SignalTrace, RunFacts{RunID: "bad", Status: "running", StartedAt: now})
	batch, errs := (&Mapper{}).BuildBatch([]ExportItem{bad, good, good})
	require.Len(t, errs, 2, "unfinished and duplicate span are isolated")
	require.Len(t, batch.Items, 1)
	require.Equal(t, "good", batch.Items[0].SourceID)
	require.Equal(t, 1, batch.RecordCount)
	for _, err := range errs {
		require.Equal(t, "mapping_error", err.Kind)
	}
}

func TestObservability_ExportRedactionCanaries(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	m := &Mapper{}
	f := RunFacts{RunID: "safe-id", SessionID: "CANARY_SESSION", Agent: "CANARY_AGENT", Status: "failed", Reason: "password=CANARY_ERROR", StartedAt: now, FinishedAt: &now}
	batch, errs := m.BuildBatch([]ExportItem{mapperItem(t, SourceRun, "safe-id", SignalTrace, f)})
	require.Empty(t, errs)
	raw, err := json.Marshal(batch.Payload)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "CANARY")
	require.NotContains(t, strings.ToLower(string(raw)), "session")
	ev := diagnostics.DiagnosticEvent{EventID: "event-safe", SchemaVersion: 1, Kind: diagnostics.KindPermissionDecision, OccurredAt: now.UnixMilli(), Correlation: diagnostics.Correlation{RunID: "safe-id", RequestID: "CANARY_REQUEST"}, Attrs: map[string]any{"tool": "password=CANARY_TOOL", "class": "CANARY_NON_WHITELIST_CLASS", "url": "https://user:CANARY_AUTH@example.test/private/CANARY_PATH?secret=CANARY_QUERY", "count": 2, "prompt": "CANARY_BODY", "authorization": "CANARY_HEADER", "nested": map[string]any{"count": "CANARY_NESTED"}}}
	log, err := MapDiagnosticLog(ev)
	require.NoError(t, err)
	raw, err = json.Marshal(log)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "CANARY")
	require.Equal(t, "https://example.test", log.Attributes["url"])
	require.Equal(t, 2, log.Attributes["count"])
	require.NotContains(t, log.Attributes, "tool")
	require.NotContains(t, log.Attributes, "class")
}

func TestObservability_ExportUnknownWithRecordedToolEnd(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	f := ToolInvocationFact{ID: 1, RunID: "real", Status: "unknown", StartedAt: now.Add(-time.Second), FinishedAt: &now}
	batch, errs := (&Mapper{}).BuildBatch([]ExportItem{mapperItem(t, SourceTool, "1", SignalTrace, f)})
	require.Empty(t, errs)
	p := batch.Payload.(TelemetryPayload)
	require.Len(t, p.Spans, 1)
	require.Empty(t, p.Logs)
	require.Equal(t, 2, p.Spans[0].StatusCode)
	require.Equal(t, now, p.Spans[0].End)
}

func TestObservability_ExportHashUsesAllowedFields(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	f := RunFacts{RunID: "real", Status: "completed", StartedAt: now, FinishedAt: &now, Agent: "private-agent", SessionID: "private-session"}
	item := mapperItem(t, SourceRun, "real", SignalTrace, f)
	before, err := ExportStateHash(item)
	require.NoError(t, err)
	f.Agent, f.SessionID, f.RequestID = "different-agent", "different-session", "different-request"
	item.Fact = f
	item.StateHash = before
	after, err := ExportStateHash(item)
	require.NoError(t, err)
	require.Equal(t, before, after)
	f.Status = "failed"
	item.Fact = f
	changed, err := ExportStateHash(item)
	require.NoError(t, err)
	require.NotEqual(t, before, changed)
}

func TestObservability_ExportAllSourceMappings(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	for _, tc := range []struct {
		source SourceKind
		id     string
		fact   any
		status int
		name   string
	}{
		{SourceEmbedding, "1", EmbeddingCallFact{ID: 1, IngestJobID: "ingest", Status: "success", CreatedAt: now, FinishedAt: &now}, 1, "basalt.embedding"},
		{SourceTask, "2", TaskExecutionFact{ID: 2, TaskID: "task", Status: "skipped", StartedAt: now, FinishedAt: &now, Agent: "CANARY_AGENT"}, 0, "basalt.task"},
		{SourceOperation, "op", ManagementOperationFact{OperationID: "op", Action: "CANARY_ACTION", Status: "unknown", CreatedAt: now, FinishedAt: &now}, 2, "basalt.operation"},
		{SourceIngest, "ingest", IngestJobFact{IngestJobID: "ingest", Status: "cancelled", Attempt: 3, StartedAt: now, FinishedAt: &now}, 0, "basalt.ingest"},
	} {
		t.Run(string(tc.source), func(t *testing.T) {
			batch, errs := (&Mapper{}).BuildBatch([]ExportItem{mapperItem(t, tc.source, tc.id, SignalTrace, tc.fact)})
			require.Empty(t, errs)
			p := batch.Payload.(TelemetryPayload)
			require.Len(t, p.Spans, 1)
			require.Equal(t, tc.status, p.Spans[0].StatusCode)
			require.Equal(t, tc.name, p.Spans[0].Name)
			raw, err := json.Marshal(p)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "CANARY")
		})
	}
}

func TestObservability_ExportMetricHistogramAndGauge(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	c := NewMetricsCollector(nil, func() time.Time { return now })
	c.ObserveExportDuration(20*time.Millisecond, "ok")
	c.ObserveExportDuration(150*time.Millisecond, "ok")
	snapshot, err := c.Snapshot(t.Context(), 15*time.Minute)
	require.NoError(t, err)
	// 本例只选聚合与ok两条已观察序列；其他预置零序列由独立用例校验。
	// This fixture selects observed aggregate/ok series; another test checks preseeded zeros.
	selected := snapshot.Samples[:0]
	for _, sample := range snapshot.Samples {
		if sample.Type != MetricHistogram || sample.Labels["result"] == "" || sample.Labels["result"] == "ok" {
			selected = append(selected, sample)
		}
	}
	snapshot.Samples = selected
	snapshot.Samples = append(snapshot.Samples, MetricSample{Name: "obs_run_duration_ms_bucket", Type: MetricGauge, Unit: "1", Scope: ScopeDatabase, Window: 15 * time.Minute, Available: true, Labels: map[string]string{"status": "completed", "le": "100"}, Value: 3})
	batch, errs := (&Mapper{}).BuildBatch([]ExportItem{mapperItem(t, SourceMetricSnapshot, "snapshot", SignalMetric, snapshot)})
	require.Empty(t, errs)
	p := batch.Payload.(TelemetryPayload)
	hist, gauge := 0, 0
	for _, point := range p.Metrics {
		switch point.Type {
		case MetricHistogram:
			hist++
			require.Equal(t, uint64(2), point.Count)
			require.Equal(t, float64(170), point.Sum)
			require.Equal(t, "ms", point.Unit)
			require.Equal(t, []uint64{0, 1, 0, 1, 0, 0, 0, 0, 0, 0, 0}, point.BucketCounts)
			require.Equal(t, now, point.StartTime)
			require.Equal(t, durationBounds, point.BucketBounds)
		case MetricCounter:
			require.Equal(t, now, point.StartTime)
		case MetricGauge:
			gauge++
			require.True(t, point.StartTime.IsZero())
			require.Equal(t, float64(3), point.Value)
			require.Equal(t, "1", point.Unit)
			require.Equal(t, "15m0s", point.Attributes["window"])
		}
	}
	require.Equal(t, 2, hist, "aggregate and fixed result series each form one point")
	require.Equal(t, 1, gauge)
	require.Equal(t, len(p.Metrics), batch.RecordCount, "partial success unit is data points, not local fragments or snapshot items")
}

func TestObservability_ExportMetricZeroSeedsPreserved(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	c := NewMetricsCollector(nil, func() time.Time { return now })
	snapshot, err := c.Snapshot(t.Context(), time.Hour)
	require.NoError(t, err)
	points, err := mapSnapshot(snapshot)
	require.NoError(t, err)
	hist := 0
	for _, p := range points {
		if p.Type == MetricHistogram {
			hist++
			require.Zero(t, p.Count)
			require.Zero(t, p.Sum)
			require.Equal(t, make([]uint64, len(durationLE)), p.BucketCounts)
		}
	}
	require.Equal(t, 5, hist)
}

func TestObservability_ExportIngestFailureClasses(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	hashes := map[string]bool{}
	for _, reason := range []string{"path_rejected", "decode_failed", "parser_failed", "chunker_failed", "embed_failed", "verify_failed", "password=CANARY_RAW_ERROR"} {
		t.Run(reason, func(t *testing.T) {
			f := IngestJobFact{IngestJobID: "job", Status: "failed", Attempt: 1, ErrorKind: reason, StartedAt: now, FinishedAt: &now}
			item := mapperItem(t, SourceIngest, "job", SignalTrace, f)
			batch, errs := (&Mapper{}).BuildBatch([]ExportItem{item})
			require.Empty(t, errs)
			attrs := batch.Payload.(TelemetryPayload).Spans[0].Attributes
			want := reason
			if strings.Contains(reason, "CANARY") {
				want = "unclassified"
			}
			require.Equal(t, want, attrs["error_kind"])
			hash, err := ExportStateHash(item)
			require.NoError(t, err)
			require.False(t, hashes[hash], "distinct frozen failure classes must not share a state hash")
			hashes[hash] = true
			raw, err := json.Marshal(batch.Payload)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "CANARY")
		})
	}
}

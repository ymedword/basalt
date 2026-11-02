package observability

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ymedword/basalt/internal/diagnostics"
)

// SpanData 只包含允许外发的事实，不提供推测父跨度字段。
// SpanData contains allowed facts and has no inferred parent field.
type SpanData struct {
	TraceID    [16]byte
	SpanID     [8]byte
	Name       string
	Start, End time.Time
	EndSource  EndTimeSource
	StatusCode int
	Attributes map[string]any
}

// LogData 对缺失结束时刻只报告事件，不伪造跨度。
// LogData represents events without inventing a span end.
type LogData struct {
	TraceID    [16]byte
	EventID    string
	Time       time.Time
	Severity   int
	Body       string
	Attributes map[string]any
}

// MetricData 区分累计进程分布和不可相加的窗口Gauge。
// MetricData separates cumulative process distributions from non-additive window gauges.
type MetricData struct {
	Count           uint64
	Sum             float64
	Name            string
	Type            MetricType
	Unit            string
	Value           float64
	StartTime, Time time.Time
	Attributes      map[string]any
	BucketBounds    []float64
	BucketCounts    []uint64
}

// TelemetryPayload 是协议无关且已经脱敏的网络中间结构。
// TelemetryPayload is a protocol-independent, redacted network representation.
type TelemetryPayload struct {
	Spans   []SpanData
	Logs    []LogData
	Metrics []MetricData
}

// Mapper 仅做纯映射，不读写来源或发送网络。
// Mapper is a pure mapper without source I/O or network effects.
type Mapper struct{}

// BuildBatch 隔离逐项错误，坏记录不拖垮其他记录。
// BuildBatch isolates per-item failures from valid records.
func (m *Mapper) BuildBatch(items []ExportItem) (ExportBatch, []ExportError) {
	out := ExportBatch{Items: []ExportItem{}}
	bad := []ExportError{}
	payload := TelemetryPayload{}
	seen := map[[8]byte]bool{}
	if len(items) > 0 {
		out.Signal = items[0].Signal
	}
	for _, item := range items {
		p, err := m.mapItem(item)
		if len(items) > 2048 || item.Signal != out.Signal {
			err = ErrInvalidQuery
		}
		if err == nil {
			for _, span := range p.Spans {
				if seen[span.SpanID] {
					err = ErrInvalidQuery
				}
			}
		}
		if err != nil {
			bad = append(bad, ExportError{SourceKind: item.SourceKind, SourceID: item.SourceID, Kind: "mapping_error"})
			continue
		}
		for _, span := range p.Spans {
			seen[span.SpanID] = true
		}
		payload.Spans = append(payload.Spans, p.Spans...)
		payload.Logs = append(payload.Logs, p.Logs...)
		payload.Metrics = append(payload.Metrics, p.Metrics...)
		out.Items = append(out.Items, item)
	}
	out.RecordCount = len(payload.Spans) + len(payload.Logs) + len(payload.Metrics)
	switch out.Signal {
	case SignalTrace:
		out.RecordUnit = "span"
	case SignalLog:
		out.RecordUnit = "log_record"
	case SignalMetric:
		out.RecordUnit = "data_point"
	}
	if out.RecordCount > 0 {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			for _, item := range out.Items {
				bad = append(bad, ExportError{SourceKind: item.SourceKind, SourceID: item.SourceID, Kind: "mapping_error"})
			}
			return ExportBatch{}, bad
		}
		out.BatchID = hex.EncodeToString(id[:])
	}
	out.Payload = payload
	return out, bad
}

// MapRunSpan 只映射实际终态运行。
// MapRunSpan maps terminal runs only.
func (*Mapper) MapRunSpan(f RunFacts) (SpanData, error) {
	if !slices.Contains([]string{"completed", "failed", "limit_reached", "cancelled"}, f.Status) {
		return SpanData{}, ErrInvalidQuery
	}
	hash, err := CanonicalStateHash(f)
	if err != nil {
		return SpanData{}, err
	}
	span, err := mappedSpan("run", f.RunID, "run:"+f.RunID+":"+hash, "basalt.run", f.StartedAt, f.FinishedAt, f.Status, EndTimeRecorded, map[string]any{"run_id": f.RunID, "reason": safeContextReason(f.Reason), "round": f.Rounds, "tool_calls": f.ToolCalls})
	return span, err
}

// MapLLMCallSpan 的结束来自记录内时长，并显式标为派生。
// MapLLMCallSpan marks duration-derived ends explicitly.
func (*Mapper) MapLLMCallSpan(f LLMCallFact) (SpanData, error) {
	if !slices.Contains([]string{"success", "failed"}, f.Status) || f.DurationMS == nil || *f.DurationMS < 0 || *f.DurationMS > math.MaxInt64/int64(time.Millisecond) {
		return SpanData{}, ErrInvalidQuery
	}
	end := f.CreatedAt.Add(time.Duration(*f.DurationMS) * time.Millisecond)
	attrs := map[string]any{"run_id": f.RunID, "round": f.Round, "duration_ms": *f.DurationMS, "error_kind": safeContextReason(f.ErrorKind), "end_time_source": "recorded_duration"}
	if f.PromptTokens != nil {
		attrs["prompt_tokens"] = *f.PromptTokens
	}
	if f.CompletionTokens != nil {
		attrs["completion_tokens"] = *f.CompletionTokens
	}
	return mappedSpan("run", f.RunID, "llm:"+strconv.FormatInt(f.ID, 10), "basalt.llm_call", f.CreatedAt, &end, f.Status, EndTimeDerivedRecorded, attrs)
}

// MapToolSpan 不把恢复时刻当结束时刻。
// MapToolSpan never treats recovery as completion.
func (*Mapper) MapToolSpan(f ToolInvocationFact) (SpanData, error) {
	if !slices.Contains([]string{"success", "failed", "unknown"}, f.Status) {
		return SpanData{}, ErrInvalidQuery
	}
	return mappedSpan("run", f.RunID, "tool:"+strconv.FormatInt(f.ID, 10), "basalt.tool", f.StartedAt, f.FinishedAt, f.Status, EndTimeDerivedRecorded, map[string]any{"run_id": f.RunID, "tool_call_id": f.ToolCallID, "tool": f.Tool, "error_kind": safeContextReason(f.ErrorKind), "end_time_source": "derived_recorded"})
}

// MapDiagnosticLog 不推断parent或span。
// MapDiagnosticLog infers neither parent nor span.
func MapDiagnosticLog(e diagnostics.DiagnosticEvent) (LogData, error) {
	if e.EventID == "" || e.OccurredAt < 0 {
		return LogData{}, ErrInvalidQuery
	}
	e = e.Redacted(diagnostics.NewWhitelistRedactor())
	attrs := map[string]any{"event_id": e.EventID, "schema_version": e.SchemaVersion, "seq": e.Seq}
	for key, value := range map[string]string{"run_id": e.Correlation.RunID, "operation_id": e.Correlation.OperationID, "ingest_job_id": e.Correlation.IngestJobID, "task_id": e.Correlation.TaskID} {
		if value != "" {
			attrs[key] = value
		}
	}
	if e.ReasonCode.Valid() {
		attrs["reason_code"] = string(e.ReasonCode)
	}
	if e.Decision.Valid() {
		attrs["decision"] = string(e.Decision)
	}
	for _, key := range []string{"tool", "tool_call_id", "class", "count", "url"} {
		if value, ok := e.Attrs[key]; ok {
			attrs[key] = value
		}
	}
	if value, ok := attrs["class"].(string); ok && !slices.Contains([]string{"run", "chat", "compaction", "ingest", "read"}, value) {
		delete(attrs, "class")
	}
	anchorKind, anchor := "", ""
	for _, candidate := range []struct{ kind, value string }{{"run", e.Correlation.RunID}, {"request", e.Correlation.RequestID}, {"operation", e.Correlation.OperationID}, {"ingest", e.Correlation.IngestJobID}, {"task", e.Correlation.TaskID}} {
		if candidate.value != "" {
			anchorKind, anchor = candidate.kind, candidate.value
			break
		}
	}
	var trace [16]byte
	if anchor != "" {
		trace = traceID(anchorKind, anchor)
	}
	body := "basalt.diagnostic.unknown"
	if e.Kind.Valid() {
		body = "basalt.diagnostic." + string(e.Kind)
	}
	severity := 9
	if strings.Contains(string(e.ReasonCode), "failed") || e.ReasonCode == "llm_call_failed" {
		severity = 17
	} else if e.Decision == "deny" || e.Decision == "trim" {
		severity = 13
	}
	return LogData{TraceID: trace, EventID: stableEvent("diagnostic", e.EventID), Time: time.UnixMilli(e.OccurredAt).UTC(), Severity: severity, Body: body, Attributes: exportAttributes(attrs)}, nil
}

// MapMetric 仅接受目录内可用读数，数据库窗口始终映射为Gauge。
// MapMetric accepts available catalog readings and always maps database windows as gauges.
func MapMetric(s MetricSample, at time.Time) (MetricData, error) {
	if !s.Available || at.IsZero() || math.IsNaN(s.Value) || math.IsInf(s.Value, 0) {
		return MetricData{}, ErrInvalidQuery
	}
	attrs := map[string]any{"derived": true}
	for k, v := range s.Labels {
		attrs[k] = v
	}
	if s.Scope == ScopeDatabase {
		base := metricBase(s.Name)
		def, ok := databaseMetrics[base]
		if !ok || s.Type != MetricGauge || !metricWindow(s.Window) {
			return MetricData{}, ErrInvalidQuery
		}
		allowed := make(map[string][]string, len(def.labels)+2)
		for k, v := range def.labels {
			allowed[k] = v
		}
		if strings.HasSuffix(s.Name, "_bucket") && base != s.Name {
			allowed["le"] = durationLE
		}
		if base == "obs_run_duration_ms" || base == "obs_llm_call_duration_ms" {
			allowed["quantile"] = []string{"0.5", "0.95", "0.99"}
		}
		if !validMetricLabels(s.Labels, allowed) {
			return MetricData{}, ErrInvalidQuery
		}
		unit := def.unit
		if base != s.Name && (strings.HasSuffix(s.Name, "_bucket") || strings.HasSuffix(s.Name, "_count")) {
			unit = "1"
		}
		attrs["window"] = s.Window.String()
		return MetricData{Name: "basalt.obs.window." + s.Name, Type: MetricGauge, Unit: unit, Value: s.Value, Time: at, Attributes: attrs}, nil
	}
	def, ok := processMetrics[s.Name]
	if s.Scope != ScopeProcess || !ok || s.Type != MetricCounter || s.Value < 0 || len(s.Labels) != len(def.labels) || !validMetricLabels(s.Labels, def.labels) {
		return MetricData{}, ErrInvalidQuery
	}
	return MetricData{Name: "basalt.obs." + s.Name, Type: MetricCounter, Unit: def.unit, Value: s.Value, Time: at, Attributes: attrs}, nil
}

func traceID(kind, id string) [16]byte {
	sum := sha256.Sum256([]byte(kind + ":" + id))
	var out [16]byte
	copy(out[:], sum[:16])
	return out
}

func spanID(id string) [8]byte {
	sum := sha256.Sum256([]byte(id))
	var out [8]byte
	copy(out[:], sum[:8])
	return out
}

func stableEvent(kind, id string) string {
	sum := sha256.Sum256([]byte(kind + ":" + id))
	return hex.EncodeToString(sum[:])
}

func mappedSpan(anchorKind, anchor, id, name string, start time.Time, end *time.Time, status string, source EndTimeSource, attrs map[string]any) (SpanData, error) {
	if anchor == "" || start.IsZero() || start.UnixMilli() < 0 || end == nil || end.Before(start) {
		return SpanData{}, ErrInvalidQuery
	}
	attrs["status"] = status
	if _, ok := attrs["end_time_source"]; !ok {
		attrs["end_time_source"] = string(source)
	}
	code := 0
	switch status {
	case "completed", "success":
		code = 1
	case "failed", "limit_reached", "unknown":
		code = 2
	}
	return SpanData{TraceID: traceID(anchorKind, anchor), SpanID: spanID(id), Name: name, Start: start, End: *end, EndSource: source, StatusCode: code, Attributes: exportAttributes(attrs)}, nil
}

func (m *Mapper) mapItem(item ExportItem) (TelemetryPayload, error) {
	var p TelemetryPayload
	var span SpanData
	var err error
	switch f := item.Fact.(type) {
	case RunFacts:
		if item.SourceKind != SourceRun || item.Signal != SignalTrace || item.SourceID != f.RunID {
			return p, ErrInvalidQuery
		}
		span, err = m.MapRunSpan(f)
		span.SpanID = spanID("run:" + f.RunID + ":" + item.StateHash)
	case LLMCallFact:
		if item.SourceKind != SourceLLM || item.SourceID != strconv.FormatInt(f.ID, 10) {
			return p, ErrInvalidQuery
		}
		if f.Status == "unknown" {
			return recoveredLog(item, f.RunID, "llm", f.CreatedAt, f.RecoveredAt, f.ErrorKind, map[string]any{"round": f.Round})
		}
		span, err = m.MapLLMCallSpan(f)
	case ToolInvocationFact:
		if item.SourceKind != SourceTool || item.SourceID != strconv.FormatInt(f.ID, 10) {
			return p, ErrInvalidQuery
		}
		if f.Status == "unknown" && f.FinishedAt == nil {
			return recoveredLog(item, f.RunID, "tool", f.StartedAt, f.RecoveredAt, f.ErrorKind, map[string]any{"tool_call_id": f.ToolCallID, "tool": f.Tool})
		}
		span, err = m.MapToolSpan(f)
	case EmbeddingCallFact:
		if item.SourceKind != SourceEmbedding || item.SourceID != strconv.FormatInt(f.ID, 10) || !slices.Contains([]string{"success", "failed", "unknown"}, f.Status) {
			return p, ErrInvalidQuery
		}
		kind, anchor := "run", f.RunID
		if anchor == "" {
			kind, anchor = "ingest", f.IngestJobID
		}
		span, err = mappedSpan(kind, anchor, "embedding:"+item.SourceID, "basalt.embedding", f.CreatedAt, f.FinishedAt, f.Status, EndTimeRecorded, map[string]any{"run_id": f.RunID, "ingest_job_id": f.IngestJobID, "error_kind": safeContextReason(f.ErrorKind)})
	case TaskExecutionFact:
		if item.SourceKind != SourceTask || item.SourceID != strconv.FormatInt(f.ID, 10) || !slices.Contains([]string{"completed", "failed", "skipped"}, f.Status) {
			return p, ErrInvalidQuery
		}
		span, err = mappedSpan("task", f.TaskID, "task:"+item.SourceID, "basalt.task", f.StartedAt, f.FinishedAt, f.Status, EndTimeRecorded, map[string]any{"task_id": f.TaskID, "run_id": f.RunID})
	case ManagementOperationFact:
		if item.SourceKind != SourceOperation || item.SourceID != f.OperationID || !slices.Contains([]string{"success", "failed", "unknown"}, f.Status) {
			return p, ErrInvalidQuery
		}
		span, err = mappedSpan("operation", f.OperationID, "operation:"+item.SourceID, "basalt.operation", f.CreatedAt, f.FinishedAt, f.Status, EndTimeRecorded, map[string]any{"operation_id": f.OperationID})
	case IngestJobFact:
		if item.SourceKind != SourceIngest || item.SourceID != f.IngestJobID || f.Attempt < 1 || !slices.Contains([]string{"success", "failed", "cancelled"}, f.Status) {
			return p, ErrInvalidQuery
		}
		span, err = mappedSpan("ingest", f.IngestJobID, fmt.Sprintf("ingest:%s:%d", f.IngestJobID, f.Attempt), "basalt.ingest", f.StartedAt, f.FinishedAt, f.Status, EndTimeRecorded, map[string]any{"ingest_job_id": f.IngestJobID, "error_kind": exportIngestReason(f.ErrorKind), "attempt": f.Attempt})
	case diagnostics.DiagnosticEvent:
		if item.SourceKind != SourceDiagnostic || item.Signal != SignalLog || item.SourceID != f.EventID {
			return p, ErrInvalidQuery
		}
		log, err := MapDiagnosticLog(f)
		if err != nil {
			return p, err
		}
		p.Logs = []LogData{log}
		return p, nil
	case MetricsSnapshot:
		if item.SourceKind != SourceMetricSnapshot || item.Signal != SignalMetric {
			return p, ErrInvalidQuery
		}
		p.Metrics, err = mapSnapshot(f)
		return p, err
	default:
		return p, ErrInvalidQuery
	}
	if err != nil || item.Signal != SignalTrace {
		return p, ErrInvalidQuery
	}
	p.Spans = []SpanData{span}
	return p, nil
}

func recoveredLog(item ExportItem, run, kind string, start time.Time, recovered *time.Time, reason string, attrs map[string]any) (TelemetryPayload, error) {
	if item.Signal != SignalLog || run == "" || start.IsZero() || start.UnixMilli() < 0 {
		return TelemetryPayload{}, ErrInvalidQuery
	}
	attrs["run_id"], attrs["status"], attrs["error_kind"], attrs["end_time_source"] = run, "unknown", safeContextReason(reason), "unrecorded"
	if recovered != nil {
		attrs["end_time_source"], attrs["recovered_at"] = "recovered_unknown", recovered.UnixMilli()
	}
	log := LogData{TraceID: traceID("run", run), EventID: stableEvent(kind, item.SourceID), Time: start, Severity: 17, Body: "basalt." + kind + ".unknown", Attributes: exportAttributes(attrs)}
	return TelemetryPayload{Logs: []LogData{log}}, nil
}

func exportIngestReason(reason string) string {
	if reason == "" || slices.Contains([]string{"path_rejected", "decode_failed", "parser_failed", "chunker_failed", "embed_failed", "verify_failed"}, reason) {
		return reason
	}
	return "unclassified"
}

func exportAttributes(attrs map[string]any) map[string]any {
	out := map[string]any{}
	dropped := 0
	for key, value := range attrs {
		switch v := value.(type) {
		case string:
			if v == "" {
				continue
			}
			if strings.Contains(v, "://") {
				parsed, err := url.Parse(v)
				if err != nil || parsed.Host == "" || parsed.Scheme == "" {
					dropped++
					continue
				}
				v = parsed.Scheme + "://" + parsed.Host
			}
			invalid := len(v) > 256
			for _, r := range v {
				if unicode.IsControl(r) {
					invalid = true
					break
				}
			}
			for _, fragment := range []string{"key", "token", "secret", "password", "credential", "authorization", "cookie", "bearer"} {
				if strings.Contains(strings.ToLower(v), fragment) {
					invalid = true
					break
				}
			}
			if invalid {
				dropped++
				continue
			}
			out[key] = v
		case int, int32, int64, uint64, bool:
			out[key] = value
		case float64:
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				out[key] = v
			} else {
				dropped++
			}
		default:
			dropped++
		}
	}
	if dropped > 0 {
		out["redacted_fields"] = dropped
	}
	return out
}

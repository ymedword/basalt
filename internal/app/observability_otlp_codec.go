package app

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	cl "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	cm "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	ct "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	metric "go.opentelemetry.io/proto/otlp/metrics/v1"
	resource "go.opentelemetry.io/proto/otlp/resource/v1"
	trace "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ymedword/basalt/internal/observability"
)

func otlpEnvelope(batch observability.ExportBatch, version string) (proto.Message, int, error) {
	payload, ok := batch.Payload.(observability.TelemetryPayload)
	if !ok {
		return nil, 0, errors.New("invalid OTLP payload type")
	}
	resourceAttrs := map[string]any{"service.name": "basalt"}
	if version != "" {
		resourceAttrs["service.version"] = version
	}
	res := &resource.Resource{Attributes: otlpAttrs(resourceAttrs)}
	scope := &common.InstrumentationScope{Name: "basalt.observability"}
	switch batch.Signal {
	case observability.SignalTrace:
		if len(payload.Logs) > 0 || len(payload.Metrics) > 0 || batch.RecordUnit != "span" {
			return nil, 0, errors.New("mixed OTLP signal")
		}
		var spans []*trace.Span
		for _, span := range payload.Spans {
			if span.TraceID == [16]byte{} || span.SpanID == [8]byte{} || !otlpTime(span.Start) || !otlpTime(span.End) || span.End.Before(span.Start) || span.StatusCode < 0 || span.StatusCode > 2 {
				return nil, 0, errors.New("invalid OTLP span")
			}
			spans = append(spans, &trace.Span{TraceId: span.TraceID[:], SpanId: span.SpanID[:], Name: span.Name, Kind: trace.Span_SPAN_KIND_INTERNAL, StartTimeUnixNano: uint64(span.Start.UnixNano()), EndTimeUnixNano: uint64(span.End.UnixNano()), Attributes: otlpAttrs(span.Attributes), Status: &trace.Status{Code: trace.Status_StatusCode(span.StatusCode)}})
		}
		return &ct.ExportTraceServiceRequest{ResourceSpans: []*trace.ResourceSpans{{Resource: res, ScopeSpans: []*trace.ScopeSpans{{Scope: scope, Spans: spans}}}}}, len(spans), nil
	case observability.SignalLog:
		if len(payload.Spans) > 0 || len(payload.Metrics) > 0 || batch.RecordUnit != "log_record" {
			return nil, 0, errors.New("mixed OTLP signal")
		}
		var records []*logs.LogRecord
		for _, log := range payload.Logs {
			if !otlpTime(log.Time) || log.EventID == "" || log.Severity < 1 || log.Severity > 24 {
				return nil, 0, errors.New("invalid OTLP log")
			}
			attrs := maps.Clone(log.Attributes)
			if attrs == nil {
				attrs = map[string]any{}
			}
			if _, exists := attrs["event_id"]; !exists {
				attrs["event_id"] = log.EventID
			}
			entry := &logs.LogRecord{TimeUnixNano: uint64(log.Time.UnixNano()), SeverityNumber: logs.SeverityNumber(log.Severity), Body: otlpAny(log.Body), EventName: log.Body, Attributes: otlpAttrs(attrs)}
			if log.TraceID != [16]byte{} {
				entry.TraceId = log.TraceID[:]
			}
			records = append(records, entry)
		}
		return &cl.ExportLogsServiceRequest{ResourceLogs: []*logs.ResourceLogs{{Resource: res, ScopeLogs: []*logs.ScopeLogs{{Scope: scope, LogRecords: records}}}}}, len(records), nil
	case observability.SignalMetric:
		if len(payload.Spans) > 0 || len(payload.Logs) > 0 || batch.RecordUnit != "data_point" {
			return nil, 0, errors.New("mixed OTLP signal")
		}
		var metrics []*metric.Metric
		byName := map[string]*metric.Metric{}
		for _, point := range payload.Metrics {
			if !otlpTime(point.Time) || !strings.HasPrefix(point.Name, "basalt.obs.") {
				return nil, 0, errors.New("invalid OTLP metric")
			}
			value := &metric.Metric{Name: point.Name, Unit: point.Unit}
			number := &metric.NumberDataPoint{TimeUnixNano: uint64(point.Time.UnixNano()), Attributes: otlpAttrs(point.Attributes), Value: &metric.NumberDataPoint_AsDouble{AsDouble: point.Value}}
			switch point.Type {
			case observability.MetricGauge:
				if !point.StartTime.IsZero() {
					return nil, 0, errors.New("window gauge has cumulative start")
				}
				value.Data = &metric.Metric_Gauge{Gauge: &metric.Gauge{DataPoints: []*metric.NumberDataPoint{number}}}
			case observability.MetricCounter:
				if !otlpTime(point.StartTime) || point.StartTime.After(point.Time) {
					return nil, 0, errors.New("invalid cumulative start")
				}
				number.StartTimeUnixNano = uint64(point.StartTime.UnixNano())
				value.Data = &metric.Metric_Sum{Sum: &metric.Sum{AggregationTemporality: metric.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, IsMonotonic: true, DataPoints: []*metric.NumberDataPoint{number}}}
			case observability.MetricHistogram:
				if !otlpTime(point.StartTime) || point.StartTime.After(point.Time) || len(point.BucketCounts) != len(point.BucketBounds)+1 {
					return nil, 0, errors.New("invalid cumulative histogram")
				}
				sum := point.Sum
				value.Data = &metric.Metric_Histogram{Histogram: &metric.Histogram{AggregationTemporality: metric.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, DataPoints: []*metric.HistogramDataPoint{{StartTimeUnixNano: uint64(point.StartTime.UnixNano()), TimeUnixNano: uint64(point.Time.UnixNano()), Count: point.Count, Sum: &sum, ExplicitBounds: point.BucketBounds, BucketCounts: point.BucketCounts, Attributes: otlpAttrs(point.Attributes)}}}}
			default:
				return nil, 0, errors.New("invalid OTLP metric type")
			}
			if existing := byName[value.Name]; existing != nil {
				if existing.Unit != value.Unit {
					return nil, 0, errors.New("conflicting OTLP metric unit")
				}
				switch point.Type {
				case observability.MetricGauge:
					if existing.GetGauge() == nil {
						return nil, 0, errors.New("conflicting OTLP metric type")
					}
					existing.GetGauge().DataPoints = append(existing.GetGauge().DataPoints, value.GetGauge().DataPoints...)
				case observability.MetricCounter:
					if existing.GetSum() == nil {
						return nil, 0, errors.New("conflicting OTLP metric type")
					}
					existing.GetSum().DataPoints = append(existing.GetSum().DataPoints, value.GetSum().DataPoints...)
				case observability.MetricHistogram:
					if existing.GetHistogram() == nil {
						return nil, 0, errors.New("conflicting OTLP metric type")
					}
					existing.GetHistogram().DataPoints = append(existing.GetHistogram().DataPoints, value.GetHistogram().DataPoints...)
				}
			} else {
				byName[value.Name] = value
				metrics = append(metrics, value)
			}
		}
		return &cm.ExportMetricsServiceRequest{ResourceMetrics: []*metric.ResourceMetrics{{Resource: res, ScopeMetrics: []*metric.ScopeMetrics{{Scope: scope, Metrics: metrics}}}}}, len(payload.Metrics), nil
	default:
		return nil, 0, errors.New("unknown OTLP signal")
	}
}

func otlpTime(at time.Time) bool { return !at.IsZero() && at.UnixNano() >= 0 && at.Year() < 2262 }

func otlpAttrs(attrs map[string]any) []*common.KeyValue {
	out := make([]*common.KeyValue, 0, len(attrs))
	for _, key := range slices.Sorted(maps.Keys(attrs)) {
		if value := otlpAny(attrs[key]); value != nil {
			out = append(out, &common.KeyValue{Key: key, Value: value})
		}
	}
	return out
}

func otlpAny(value any) *common.AnyValue {
	out := &common.AnyValue{}
	switch v := value.(type) {
	case string:
		out.Value = &common.AnyValue_StringValue{StringValue: v}
	case bool:
		out.Value = &common.AnyValue_BoolValue{BoolValue: v}
	case int:
		out.Value = &common.AnyValue_IntValue{IntValue: int64(v)}
	case int32:
		out.Value = &common.AnyValue_IntValue{IntValue: int64(v)}
	case int64:
		out.Value = &common.AnyValue_IntValue{IntValue: v}
	case uint64:
		if v > 1<<63-1 {
			return nil
		}
		out.Value = &common.AnyValue_IntValue{IntValue: int64(v)}
	case float64:
		out.Value = &common.AnyValue_DoubleValue{DoubleValue: v}
	default:
		return nil
	}
	return out
}

// otlpJSON 实现OTLP对protojson的两处差异：整数枚举与十六进制trace/span ID。
// otlpJSON applies OTLP's integer-enum and hexadecimal-ID deviations from protojson.
func otlpJSON(message proto.Message) ([]byte, error) {
	raw, err := (protojson.MarshalOptions{UseEnumNumbers: true}).Marshal(message)
	if err != nil {
		return nil, err
	}
	var value any
	if err = json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if err = otlpJSONIDs(value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func otlpJSONIDs(value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "traceId" || key == "spanId" || key == "parentSpanId" {
				encoded, ok := child.(string)
				if !ok {
					return errors.New("invalid JSON OTLP ID")
				}
				raw, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil {
					return errors.New("invalid JSON OTLP ID")
				}
				value[key] = hex.EncodeToString(raw)
			} else if err := otlpJSONIDs(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := otlpJSONIDs(child); err != nil {
				return err
			}
		}
	}
	return nil
}

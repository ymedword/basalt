//go:build integration && obscollector

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	cl "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	cm "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	ct "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	metric "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// collectorEnvelope 只接收官方Collector转发的合成OTLP，不替换产品sender。
// collectorEnvelope captures synthetic OTLP forwarded by the official Collector.
type collectorEnvelope struct {
	path string
	raw  []byte
	err  error
}

func collectorReceive(t *testing.T, ch <-chan collectorEnvelope, path string) proto.Message {
	t.Helper()
	select {
	case got := <-ch:
		require.NoError(t, got.err)
		require.Equal(t, path, got.path)
		require.NotContains(t, string(got.raw), "CANARY")
		var message proto.Message
		switch path {
		case "/v1/traces":
			message = &ct.ExportTraceServiceRequest{}
		case "/v1/logs":
			message = &cl.ExportLogsServiceRequest{}
		case "/v1/metrics":
			message = &cm.ExportMetricsServiceRequest{}
		default:
			t.Fatalf("unexpected synthetic backend path %q", path)
		}
		require.NoError(t, proto.Unmarshal(got.raw, message))
		return message
	case <-time.After(10 * time.Second):
		t.Fatalf("official Collector did not forward %s", path)
		return nil
	}
}

func collectorAttrs(attrs []*common.KeyValue) map[string]string {
	out := map[string]string{}
	for _, attr := range attrs {
		out[attr.Key] = attr.Value.GetStringValue()
	}
	return out
}

// collectorGaugeBackend 以名称/采集时间/属性幂等覆盖；不对重叠窗口求和或差分。
// collectorGaugeBackend upserts by name/time/attributes without summing or differencing windows.
type collectorGaugeBackend map[string]float64

func (b collectorGaugeBackend) ingest(t *testing.T, request *cm.ExportMetricsServiceRequest) (uint64, float64, uint64) {
	t.Helper()
	var at, processStart uint64
	var processValue float64
	found := false
	for _, resource := range request.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, m := range scope.Metrics {
				if strings.HasPrefix(m.Name, "basalt.obs.window.") {
					require.NotNil(t, m.GetGauge(), "database windows must remain gauges after Collector")
					unit := "1"
					if strings.Contains(m.Name, "duration_ms") && !strings.HasSuffix(m.Name, "_bucket") && !strings.HasSuffix(m.Name, "_count") {
						unit = "ms"
					}
					if strings.HasSuffix(m.Name, "_age_ms") {
						unit = "ms"
					}
					require.Equal(t, unit, m.Unit)
					for _, point := range m.GetGauge().DataPoints {
						require.Zero(t, point.StartTimeUnixNano)
						require.Equal(t, "15m0s", collectorAttrs(point.Attributes)["window"])
						attrs := append([]*common.KeyValue(nil), point.Attributes...)
						slices.SortFunc(attrs, func(a, b *common.KeyValue) int { return strings.Compare(a.Key, b.Key) })
						parts := make([]string, 0, len(attrs))
						for _, attr := range attrs {
							encoded, err := protojson.Marshal(attr)
							require.NoError(t, err)
							parts = append(parts, string(encoded))
						}
						key := fmt.Sprintf("%s|%d|%s", m.Name, point.TimeUnixNano, strings.Join(parts, ","))
						b[key] = point.GetAsDouble()
						labels := collectorAttrs(point.Attributes)
						for _, forbidden := range []string{"run_id", "session_id", "request_id", "agent", "tool_call_id"} {
							require.NotContains(t, labels, forbidden)
						}
						if m.Name == "basalt.obs.window.obs_runs_total" && labels["status"] == "completed" && labels["trigger"] == "api" {
							require.Equal(t, float64(1), point.GetAsDouble(), "overlapping windows each describe the same one run")
							at, found = point.TimeUnixNano, true
						}
					}
				} else if m.Name == "basalt.obs.obs_audit_write_failures_total" {
					sum := m.GetSum()
					require.NotNil(t, sum)
					require.True(t, sum.IsMonotonic)
					require.Equal(t, metric.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, sum.AggregationTemporality)
					require.Len(t, sum.DataPoints, 1)
					processStart, processValue = sum.DataPoints[0].StartTimeUnixNano, sum.DataPoints[0].GetAsDouble()
				}
			}
		}
	}
	require.True(t, found, "real database run gauge must be present")
	require.NotZero(t, processStart)
	return at, processValue, processStart
}

// collectorHistogram 验证官方Collector保持进程累计直方图及非累计桶数。
// collectorHistogram verifies cumulative histograms and non-cumulative bucket counts after Collector.
func collectorHistogram(t *testing.T, request *cm.ExportMetricsServiceRequest, at, start, count uint64) {
	t.Helper()
	found := false
	for _, resource := range request.ResourceMetrics {
		for _, scope := range resource.ScopeMetrics {
			for _, m := range scope.Metrics {
				if m.Name != "basalt.obs.obs_export_batch_duration_ms" {
					continue
				}
				found = true
				require.Equal(t, "ms", m.Unit)
				histogram := m.GetHistogram()
				require.NotNil(t, histogram)
				require.Equal(t, metric.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, histogram.AggregationTemporality)
				require.Len(t, histogram.DataPoints, 5)
				seen := map[string]bool{}
				for _, point := range histogram.DataPoints {
					require.Equal(t, at, point.TimeUnixNano)
					require.Equal(t, start, point.StartTimeUnixNano)
					require.Equal(t, []float64{10, 50, 100, 250, 500, 1000, 5000, 15000, 30000, 60000}, point.ExplicitBounds)
					result := collectorAttrs(point.Attributes)["result"]
					require.Contains(t, []string{"", "ok", "warning", "partial", "error"}, result)
					require.False(t, seen[result])
					seen[result] = true
					want := uint64(0)
					buckets := make([]uint64, 11)
					if result == "" || result == "ok" {
						want = count
						if count > 0 {
							buckets[1], buckets[2] = 1, 1
						}
					}
					require.Equal(t, want, point.Count)
					require.Equal(t, float64(want)*50, point.GetSum())
					require.Equal(t, buckets, point.BucketCounts)
				}
			}
		}
	}
	require.True(t, found, "process histogram must survive the real Collector pipeline")
}

func TestCollector_US16AProductThreeSignals(t *testing.T) {
	binary := os.Getenv("BASALT_OBS_TEST_COLLECTOR_BINARY")
	evidence := os.Getenv("BASALT_OBS_TEST_COLLECTOR_EVIDENCE")
	require.NotEmpty(t, binary, "explicit fixed-version loopback Collector binary required")
	require.NotEmpty(t, evidence, "explicit owned evidence directory required")
	raw, err := os.ReadFile(binary)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	require.Equal(t, "20aa55b613207c37f6b65df555b16001ec3f965d7218bf619742cb8579f0daa1", hex.EncodeToString(sum[:]))
	require.NoError(t, os.MkdirAll(evidence, 0o700))
	testBinary, err := os.Executable()
	require.NoError(t, err)
	testBytes, err := os.ReadFile(testBinary)
	require.NoError(t, err)
	testSum := sha256.Sum256(testBytes)
	require.NoError(t, os.WriteFile(filepath.Join(evidence, "collector-product.test.exe"), testBytes, 0o600))
	for _, protocol := range []string{"http/protobuf", "http/json"} {
		t.Run(strings.TrimPrefix(protocol, "http/"), func(t *testing.T) {
			collectorProductProtocol(t, binary, evidence, protocol)
		})
	}
	t.Log("collector_version=0.164.0 signals=trace,log,metric layers_1_2_3_5=passed optional_external_backend=not_run")
	t.Logf("binary_source=go_test binary_sha256=%x", testSum)
}

func collectorProductProtocol(t *testing.T, binary, evidence, protocol string) {
	t.Helper()
	dir := filepath.Join(evidence, strings.TrimPrefix(protocol, "http/"))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	ch := make(chan collectorEnvelope, 16)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		ch <- collectorEnvelope{path: r.URL.Path, raw: raw, err: err}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	config := fmt.Sprintf(`receivers:
  otlp:
    protocols:
      http:
        endpoint: %s
exporters:
  debug:
    verbosity: detailed
  otlp_http:
    endpoint: %s
    compression: none
    retry_on_failure:
      enabled: false
    sending_queue:
      enabled: false
service:
  telemetry:
    logs:
      level: info
    metrics:
      level: none
  pipelines:
    traces: {receivers: [otlp], exporters: [debug, otlp_http]}
    logs: {receivers: [otlp], exporters: [debug, otlp_http]}
    metrics: {receivers: [otlp], exporters: [debug, otlp_http]}
`, address, backend.URL)
	configPath := filepath.Join(dir, "collector.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
	logPath := filepath.Join(dir, "collector-debug.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, logFile.Close()) }()
	ctx, cancel := context.WithCancel(t.Context())
	command := exec.CommandContext(ctx, binary, "--config", configPath)
	command.Stdout, command.Stderr = logFile, logFile
	require.NoError(t, command.Start())
	defer func() { cancel(); _ = command.Wait() }()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	require.Eventually(t, func() bool {
		response, readErr := client.Get("http://" + address + "/v1/traces")
		if readErr != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusMethodNotAllowed
	}, 10*time.Second, 20*time.Millisecond, "Collector receiver must start on owned loopback")

	db := pgtest.NewTestDB(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	runID := "collector-synthetic-run"
	require.NoError(t, startRunTest(postgres.NewRunStore(db), t.Context(), core.RunRecord{RunID: runID, SessionID: "CANARY_SESSION", Agent: "CANARY_AGENT", Channel: "cli", Principal: core.LocalAdminCLIPrincipal(), StartedAt: now.Add(-10 * time.Minute)}, core.RunResourceManifest{}))
	_, err = db.ExecContext(t.Context(), `UPDATE runs SET status='completed',trigger='api',finished_at=$1,reason='CANARY_RUN_BODY' WHERE run_id=$2`, now.Add(-3*time.Minute).UnixMilli(), runID)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at,error_message,recovered_at) VALUES($1,1,'CANARY_PROVIDER','CANARY_MODEL',100,'success',$2,'CANARY_LLM_BODY',NULL),($1,2,'CANARY_PROVIDER','CANARY_MODEL',0,'unknown',$3,'CANARY_LLM_ERROR',$4)`, runID, now.Add(-9*time.Minute).UnixMilli(), now.Add(-8*time.Minute).UnixMilli(), now.Add(-2*time.Minute).UnixMilli())
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `INSERT INTO tool_invocations(run_id,tool_call_id,tool_name,input_json,result_json,status,started_at,finished_at,recovered_at,error_message) VALUES($1,'tc-1','lookup','CANARY_TOOL_INPUT','CANARY_TOOL_OUTPUT','success',$2,$3,NULL,'CANARY_TOOL_ERROR'),($1,'tc-2','lookup','CANARY_TOOL_INPUT','CANARY_TOOL_OUTPUT','unknown',$2,NULL,$4,'CANARY_TOOL_ERROR')`, runID, now.Add(-7*time.Minute).UnixMilli(), now.Add(-6*time.Minute).UnixMilli(), now.Add(-2*time.Minute).UnixMilli())
	require.NoError(t, err)
	id, err := diagnostics.NewEventID(now.Add(-5 * time.Minute))
	require.NoError(t, err)
	require.NoError(t, postgres.NewDiagnosticStore(db).InsertEvent(t.Context(), diagnostics.DiagnosticEvent{EventID: id, SchemaVersion: 1, Kind: diagnostics.KindPermissionDecision, OccurredAt: now.Add(-5 * time.Minute).UnixMilli(), Correlation: diagnostics.Correlation{RunID: runID}, Attrs: map[string]any{"count": 1, "prompt": "CANARY_DIAGNOSTIC_BODY", "url": "https://user:CANARY_CREDENTIAL@example.test/CANARY_PATH?token=CANARY_QUERY"}}))
	before := scenarioBusinessFingerprint(t, db)
	cfg := cli.DefaultObservabilityConfig()
	cfg.Export.Enabled, cfg.Export.Endpoint, cfg.Export.Protocol = true, "http://"+address, protocol
	newProduct := func() *Observability {
		o, createErr := NewObservability(t.Context(), cfg, Deps{Store: postgres.NewObservabilityStore(db), Clock: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler)})
		require.NoError(t, createErr)
		t.Cleanup(func() { require.NoError(t, o.Close()) })
		return o
	}
	o := newProduct()
	o.Metrics.IncCounter("obs_audit_write_failures_total", nil)
	o.Metrics.ObserveExportDuration(25*time.Millisecond, "ok")
	o.Metrics.ObserveExportDuration(75*time.Millisecond, "ok")
	require.NoError(t, o.MetricsExport.EnqueueOnce(t.Context()))
	result, err := o.Export.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 7, result.Sent)
	require.Equal(t, 3, result.Batches)
	traces := collectorReceive(t, ch, "/v1/traces").(*ct.ExportTraceServiceRequest)
	logs := collectorReceive(t, ch, "/v1/logs").(*cl.ExportLogsServiceRequest)
	metrics := collectorReceive(t, ch, "/v1/metrics").(*cm.ExportMetricsServiceRequest)
	rows, err := db.QueryContext(t.Context(), `SELECT signal,outcome,http_status,record_count,accepted_records,rejected_records FROM observability_export_batches ORDER BY signal`)
	require.NoError(t, err)
	batchSignals := map[string]int{}
	for rows.Next() {
		var signal, outcome string
		var status, count, accepted, rejected int
		require.NoError(t, rows.Scan(&signal, &outcome, &status, &count, &accepted, &rejected))
		require.Equal(t, "sent", outcome)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, count, accepted)
		require.Zero(t, rejected)
		batchSignals[signal] = count
		t.Logf("signal=%s http_status=%d outcome=%s records=%d accepted=%d rejected=%d response_parsed=true", signal, status, outcome, count, accepted, rejected)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, 3, batchSignals["trace"])
	require.Equal(t, 3, batchSignals["log"])
	require.Positive(t, batchSignals["metric"])
	require.Len(t, batchSignals, 3)
	traceID := sha256.Sum256([]byte("run:" + runID))
	names := map[string]int{}
	spanIDs := map[string]bool{}
	for _, resource := range traces.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				require.Equal(t, traceID[:16], span.TraceId)
				require.Len(t, span.SpanId, 8)
				require.Empty(t, span.ParentSpanId)
				require.False(t, spanIDs[string(span.SpanId)])
				spanIDs[string(span.SpanId)] = true
				names[span.Name]++
				require.EqualValues(t, 1, span.Status.Code)
				attrs := collectorAttrs(span.Attributes)
				switch span.Name {
				case "basalt.run":
					require.EqualValues(t, now.Add(-10*time.Minute).UnixNano(), span.StartTimeUnixNano)
					require.EqualValues(t, now.Add(-3*time.Minute).UnixNano(), span.EndTimeUnixNano)
					require.Equal(t, "recorded", attrs["end_time_source"])
				case "basalt.llm_call":
					require.EqualValues(t, now.Add(-9*time.Minute).UnixNano(), span.StartTimeUnixNano)
					require.EqualValues(t, now.Add(-9*time.Minute+100*time.Millisecond).UnixNano(), span.EndTimeUnixNano)
					require.Equal(t, "recorded_duration", attrs["end_time_source"])
				case "basalt.tool":
					require.EqualValues(t, now.Add(-7*time.Minute).UnixNano(), span.StartTimeUnixNano)
					require.EqualValues(t, now.Add(-6*time.Minute).UnixNano(), span.EndTimeUnixNano)
					require.Equal(t, "derived_recorded", attrs["end_time_source"])
				}
			}
		}
	}
	require.Equal(t, map[string]int{"basalt.run": 1, "basalt.llm_call": 1, "basalt.tool": 1}, names)
	logNames := map[string]int{}
	var recoveredLLMID, recoveredToolID int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT id FROM llm_calls WHERE status='unknown'`).Scan(&recoveredLLMID))
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT id FROM tool_invocations WHERE status='unknown'`).Scan(&recoveredToolID))
	for _, resource := range logs.ResourceLogs {
		for _, scope := range resource.ScopeLogs {
			for _, record := range scope.LogRecords {
				require.Equal(t, traceID[:16], record.TraceId)
				require.Empty(t, record.SpanId)
				logNames[record.Body.GetStringValue()]++
				attrs := collectorAttrs(record.Attributes)
				require.Equal(t, runID, attrs["run_id"])
				switch record.Body.GetStringValue() {
				case "basalt.diagnostic.permission_decision":
					require.EqualValues(t, now.Add(-5*time.Minute).UnixNano(), record.TimeUnixNano)
					require.EqualValues(t, 9, record.SeverityNumber)
					require.Equal(t, id, attrs["event_id"])
					require.Equal(t, "https://example.test", attrs["url"])
					require.Len(t, attrs, 6)
					for _, attr := range record.Attributes {
						switch attr.Key {
						case "count":
							require.Equal(t, float64(1), attr.Value.GetDoubleValue())
						case "schema_version":
							require.EqualValues(t, 1, attr.Value.GetIntValue())
						case "seq":
							require.Zero(t, attr.Value.GetIntValue())
						}
					}
				case "basalt.llm.unknown", "basalt.tool.unknown":
					require.Contains(t, attrs, "recovered_at")
					require.Len(t, attrs, map[string]int{"basalt.llm.unknown": 6, "basalt.tool.unknown": 7}[record.Body.GetStringValue()])
					kind, rowID, offset := "llm", recoveredLLMID, -8*time.Minute
					if record.Body.GetStringValue() == "basalt.tool.unknown" {
						kind, rowID, offset = "tool", recoveredToolID, -7*time.Minute
						require.Equal(t, "tc-2", attrs["tool_call_id"])
						require.Equal(t, "lookup", attrs["tool"])
					}
					event := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", kind, rowID)))
					require.Equal(t, hex.EncodeToString(event[:]), attrs["event_id"])
					require.Equal(t, "unknown", attrs["status"])
					require.EqualValues(t, now.Add(offset).UnixNano(), record.TimeUnixNano)
					require.EqualValues(t, 17, record.SeverityNumber)
					for _, attr := range record.Attributes {
						if attr.Key == "recovered_at" {
							require.Equal(t, now.Add(-2*time.Minute).UnixMilli(), attr.Value.GetIntValue())
						}
					}
				}
				if strings.HasSuffix(record.Body.GetStringValue(), ".unknown") {
					require.Equal(t, "recovered_unknown", collectorAttrs(record.Attributes)["end_time_source"])
				}
			}
		}
	}
	require.Equal(t, map[string]int{"basalt.diagnostic.permission_decision": 1, "basalt.llm.unknown": 1, "basalt.tool.unknown": 1}, logNames)
	backendPoints := collectorGaugeBackend{}
	firstAt, firstValue, firstStart := backendPoints.ingest(t, metrics)
	collectorHistogram(t, metrics, firstAt, firstStart, 2)
	require.Equal(t, float64(1), firstValue)
	pointCount := len(backendPoints)

	// 从真实账本读取原快照，通过同一产品sender重送；后端同点幂等覆盖。
	// Replay the persisted snapshot through the product sender; the backend upserts the same point.
	var payload []byte
	var sourceID, stateHash string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT source_id,state_hash,payload FROM observability_export_items WHERE source_kind='metric_snapshot'`).Scan(&sourceID, &stateHash, &payload))
	var snapshot observability.MetricsSnapshot
	require.NoError(t, json.Unmarshal(payload, &snapshot))
	item := observability.ExportItem{SourceKind: observability.SourceMetricSnapshot, SourceID: sourceID, StateHash: stateHash, Signal: observability.SignalMetric, Fact: snapshot}
	batch, bad := (&observability.Mapper{}).BuildBatch([]observability.ExportItem{item})
	require.Empty(t, bad)
	sent, err := o.sender.Send(t.Context(), batch)
	require.NoError(t, err)
	require.Equal(t, batch.RecordCount, sent.Accepted)
	duplicate := collectorReceive(t, ch, "/v1/metrics").(*cm.ExportMetricsServiceRequest)
	duplicateAt, duplicateValue, duplicateStart := backendPoints.ingest(t, duplicate)
	collectorHistogram(t, duplicate, duplicateAt, duplicateStart, 2)
	require.Equal(t, firstAt, duplicateAt)
	require.Equal(t, firstValue, duplicateValue)
	require.Equal(t, firstStart, duplicateStart)
	require.Len(t, backendPoints, pointCount)
	require.True(t, proto.Equal(metrics, duplicate), "Collector preserves the identical original snapshot")

	// 重启收集器后DB窗口仍为1，进程累计从新起点0开始；不能伪造跨重启累计。
	// After restart the database window remains one and the process counter resets with a new start.
	require.NoError(t, o.Close())
	now = now.Add(time.Minute)
	restarted := newProduct()
	require.NoError(t, restarted.MetricsExport.EnqueueOnce(t.Context()))
	result, err = restarted.Export.ExportOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, result.Sent)
	require.Equal(t, 1, result.Batches)
	second := collectorReceive(t, ch, "/v1/metrics").(*cm.ExportMetricsServiceRequest)
	secondAt, secondValue, secondStart := backendPoints.ingest(t, second)
	collectorHistogram(t, second, secondAt, secondStart, 0)
	require.Greater(t, secondAt, firstAt)
	require.Greater(t, secondStart, firstStart)
	require.Zero(t, secondValue)
	runPoints := 0
	for key, value := range backendPoints {
		if strings.HasPrefix(key, "basalt.obs.window.obs_runs_total|") && strings.Contains(key, `"completed"`) && strings.Contains(key, `"api"`) {
			runPoints++
			require.Equal(t, float64(1), value, "each retained overlapping snapshot remains one, without addition or delta")
		}
	}
	require.Equal(t, 2, runPoints, "two distinct overlapping windows remain separately addressable")
	require.Equal(t, before, scenarioBusinessFingerprint(t, db), "export does not rewrite authoritative records")
	debug, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.NotContains(t, string(debug), "CANARY")
	require.Contains(t, string(debug), "basalt.run")
	require.Contains(t, string(debug), "basalt.diagnostic.permission_decision")
	require.Contains(t, string(debug), "basalt.obs.window.obs_runs_total")
	for name, message := range map[string]proto.Message{"traces": traces, "logs": logs, "metrics": metrics, "metrics-duplicate": duplicate, "metrics-restart": second} {
		encoded, encodeErr := (protojson.MarshalOptions{Indent: "  "}).Marshal(message)
		require.NoError(t, encodeErr)
		require.False(t, bytes.Contains(encoded, []byte("CANARY")))
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+".json"), encoded, 0o600))
	}
	t.Logf("protocol=%s trace_records=3 log_records=3 metric_points=%d duplicate_upsert=true first_at=%s second_at=%s restart_counter=1_to_0 start_advanced=true canary_absent=true", protocol, pointCount, strconv.FormatUint(firstAt, 10), strconv.FormatUint(secondAt, 10))
}

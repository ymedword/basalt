package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	cl "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	cm "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	ct "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metric "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

func otlpTestBatch(t *testing.T, signal observability.SignalKind) observability.ExportBatch {
	t.Helper()
	now := time.UnixMilli(1700000000000).UTC()
	var items []observability.ExportItem
	for _, id := range []string{"a", "b"} {
		item := observability.ExportItem{SourceID: id, Signal: signal}
		switch signal {
		case observability.SignalTrace:
			item.SourceKind = observability.SourceRun
			item.Fact = observability.RunFacts{RunID: id, SessionID: "CANARY_SESSION", Agent: "CANARY_AGENT", Status: "completed", StartedAt: now, FinishedAt: &now}
		case observability.SignalLog:
			item.SourceKind = observability.SourceDiagnostic
			item.Fact = diagnostics.DiagnosticEvent{EventID: id, Kind: diagnostics.KindPermissionDecision, OccurredAt: now.UnixMilli(), Correlation: diagnostics.Correlation{RunID: id}, Attrs: map[string]any{"url": "https://user:CANARY_AUTH@example.test/CANARY_PATH?token=CANARY_QUERY", "count": 1, "prompt": "CANARY_PROMPT"}}
		case observability.SignalMetric:
			item.SourceKind = observability.SourceMetricSnapshot
			item.Fact = observability.MetricsSnapshot{CollectedAt: now, Samples: []observability.MetricSample{{Name: "obs_tool_denials_total", Type: observability.MetricGauge, Scope: observability.ScopeDatabase, Window: time.Hour, Available: true, Value: 3}}}
		}
		hash, err := observability.ExportStateHash(item)
		require.NoError(t, err)
		item.StateHash = hash
		items = append(items, item)
	}
	batch, errs := (&observability.Mapper{}).BuildBatch(items)
	require.Empty(t, errs)
	require.Equal(t, 2, batch.RecordCount)
	return batch
}

func otlpResponse(t *testing.T, signal observability.SignalKind, rejected int64, warning string, partial bool, protocol string) []byte {
	t.Helper()
	var message proto.Message
	switch signal {
	case observability.SignalTrace:
		r := &ct.ExportTraceServiceResponse{}
		if partial {
			r.PartialSuccess = &ct.ExportTracePartialSuccess{RejectedSpans: rejected, ErrorMessage: warning}
		}
		message = r
	case observability.SignalLog:
		r := &cl.ExportLogsServiceResponse{}
		if partial {
			r.PartialSuccess = &cl.ExportLogsPartialSuccess{RejectedLogRecords: rejected, ErrorMessage: warning}
		}
		message = r
	case observability.SignalMetric:
		r := &cm.ExportMetricsServiceResponse{}
		if partial {
			r.PartialSuccess = &cm.ExportMetricsPartialSuccess{RejectedDataPoints: rejected, ErrorMessage: warning}
		}
		message = r
	}
	var raw []byte
	var err error
	if protocol == "http/json" {
		raw, err = (protojson.MarshalOptions{UseEnumNumbers: true}).Marshal(message)
	} else {
		raw, err = proto.Marshal(message)
	}
	require.NoError(t, err)
	return raw
}

func TestObservability_OTLPThreeSignalResponses(t *testing.T) {
	for _, protocol := range []string{"http/protobuf", "http/json"} {
		for _, signal := range []observability.SignalKind{observability.SignalTrace, observability.SignalLog, observability.SignalMetric} {
			for _, mode := range []string{"success", "warning", "partial", "invalid"} {
				t.Run(protocol+"/"+string(signal)+"/"+mode, func(t *testing.T) {
					batch := otlpTestBatch(t, signal)
					var requests atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						require.Equal(t, http.MethodPost, r.Method)
						require.Equal(t, "/prefix/v1/"+map[observability.SignalKind]string{observability.SignalTrace: "traces", observability.SignalLog: "logs", observability.SignalMetric: "metrics"}[signal], r.URL.Path)
						raw, err := io.ReadAll(r.Body)
						require.NoError(t, err)
						require.NotContains(t, string(raw), "CANARY")
						contentType := "application/x-protobuf"
						if protocol == "http/json" {
							contentType = "application/json"
						}
						require.Equal(t, contentType, r.Header.Get("Content-Type"))
						w.Header().Set("Content-Type", contentType)
						if protocol == "http/json" && signal == observability.SignalTrace {
							var body map[string]any
							require.NoError(t, json.Unmarshal(raw, &body))
							spans := body["resourceSpans"].([]any)[0].(map[string]any)["scopeSpans"].([]any)[0].(map[string]any)["spans"].([]any)
							for _, value := range spans {
								span := value.(map[string]any)
								require.Regexp(t, "^[0-9a-f]{32}$", span["traceId"])
								require.Regexp(t, "^[0-9a-f]{16}$", span["spanId"])
								require.NotContains(t, span, "parentSpanId")
							}
							require.NotContains(t, string(raw), "STATUS_CODE_OK")
						}
						rejected, warning, partial := int64(0), "", false
						switch mode {
						case "warning":
							warning = "password=CANARY_RECEIVER"
							partial = true
						case "partial":
							rejected = 1
							partial = true
						case "invalid":
							rejected = 3
							partial = true
						}
						_, err = w.Write(otlpResponse(t, signal, rejected, warning, partial, protocol))
						require.NoError(t, err)
					}))
					defer server.Close()
					sender, err := newOTLPSender(otlpSenderConfig{Endpoint: server.URL + "/prefix", Protocol: protocol, Timeout: time.Second})
					require.NoError(t, err)
					result, sendErr := sender.Send(t.Context(), batch)
					require.Equal(t, int32(1), requests.Load())
					require.NotContains(t, result.Warning, "CANARY")
					d := observability.ResolveSendResult(batch, result, sendErr)
					switch mode {
					case "success":
						require.NoError(t, sendErr)
						require.Equal(t, observability.ExportSent, d.State)
						require.Equal(t, 2, *d.Accepted)
					case "warning":
						require.NoError(t, sendErr)
						require.Equal(t, "sent_with_warning", d.Outcome)
						require.Equal(t, 2, *d.Accepted)
					case "partial":
						require.NoError(t, sendErr)
						require.Equal(t, observability.ExportUnconfirmed, d.State)
						require.Equal(t, 1, *d.Accepted)
						require.Equal(t, 1, *d.Rejected)
						require.False(t, result.Retryable)
					case "invalid":
						require.Equal(t, observability.ExportUnconfirmed, d.State)
						require.Equal(t, "protocol_invalid", d.ErrorKind)
						require.Nil(t, d.Accepted)
						require.Nil(t, d.Rejected)
						require.False(t, result.Retryable)
					}
				})
			}
		}
	}
}

func TestObservability_OTLPHTTPFailureClasses(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500, 501, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "9")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "password=CANARY_BODY")
			}))
			defer server.Close()
			sender, err := newOTLPSender(otlpSenderConfig{Endpoint: server.URL, Timeout: time.Second})
			require.NoError(t, err)
			result, sendErr := sender.Send(t.Context(), otlpTestBatch(t, observability.SignalTrace))
			require.Error(t, sendErr)
			require.NotContains(t, sendErr.Error(), "CANARY")
			require.Equal(t, int32(1), requests.Load())
			want := status == 429 || status == 502 || status == 503 || status == 504
			require.Equal(t, want, result.Retryable)
			require.Equal(t, status, result.HTTPStatus)
			if want {
				require.Equal(t, 9*time.Second, result.RetryAfter)
			}
		})
	}
}

func TestObservability_OTLPBoundedResponse(t *testing.T) {
	for _, mode := range []string{"oversize", "malformed", "timeout", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/x-protobuf")
				switch mode {
				case "oversize":
					_, _ = io.WriteString(w, strings.Repeat("x", (1<<20)+1))
				case "malformed":
					_, _ = w.Write([]byte{255, 255, 255})
				case "timeout":
					_, _ = io.Copy(io.Discard, r.Body)
					select {
					case <-r.Context().Done():
					case <-time.After(time.Second):
					}
				case "redirect":
					w.Header().Set("Location", "/v1/traces")
					w.WriteHeader(http.StatusTemporaryRedirect)
				}
			}))
			defer server.Close()
			sender, err := newOTLPSender(otlpSenderConfig{Endpoint: server.URL, Timeout: 100 * time.Millisecond})
			require.NoError(t, err)
			result, sendErr := sender.Send(t.Context(), otlpTestBatch(t, observability.SignalTrace))
			require.Error(t, sendErr)
			require.Equal(t, int32(1), requests.Load())
			switch mode {
			case "oversize":
				require.Equal(t, "response_too_large", result.ErrorKind)
				require.False(t, result.Retryable)
			case "malformed":
				require.Equal(t, "protocol_invalid", result.ErrorKind)
				require.True(t, result.Retryable)
			case "timeout":
				require.Equal(t, "timeout", result.ErrorKind)
				require.True(t, result.Retryable)
			case "redirect":
				require.False(t, result.Retryable)
			}
		})
	}
}

func TestObservability_OTLPConfigRejectsUnsafe(t *testing.T) {
	for _, endpoint := range []string{"file:///tmp/collector", "https://user:password@example.test", "https://example.test/?token=x", "https://example.test/#secret", "${MISSING_ENDPOINT}", "http://"} {
		_, err := newOTLPSender(otlpSenderConfig{Endpoint: endpoint})
		require.Error(t, err)
		require.NotContains(t, err.Error(), endpoint)
	}
	_, err := newOTLPSender(otlpSenderConfig{Endpoint: "https://example.test", Headers: map[string]string{"X-Test": "value\r\nCANARY"}})
	require.Error(t, err)
	_, err = newOTLPSender(otlpSenderConfig{Endpoint: "https://example.test", Headers: map[string]string{"Content-Type": "application/json"}})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sender, err := newOTLPSender(otlpSenderConfig{Endpoint: "http://127.0.0.1:1"})
	require.NoError(t, err)
	_, err = sender.Send(ctx, otlpTestBatch(t, observability.SignalTrace))
	require.ErrorIs(t, err, context.Canceled)
}

type otlpMetricStore struct{}

func (otlpMetricStore) WindowCounts(context.Context, time.Duration) ([]observability.MetricSample, error) {
	return []observability.MetricSample{
		{Name: "obs_run_duration_ms_bucket", Type: observability.MetricGauge, Available: true, Value: 2, Labels: map[string]string{"status": "completed", "le": "100"}},
		{Name: "obs_run_duration_ms_sum", Type: observability.MetricGauge, Available: true, Value: 120, Labels: map[string]string{"status": "completed"}},
	}, nil
}

func TestObservability_OTLPCumulativeAndWindowShapes(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	c := observability.NewMetricsCollector(otlpMetricStore{}, func() time.Time { return now })
	c.IncCounter("obs_audit_write_failures_total", nil)
	c.ObserveExportDuration(12*time.Millisecond, "ok")
	snapshot, err := c.Snapshot(t.Context(), 15*time.Minute)
	require.NoError(t, err)
	batch, errs := (&observability.Mapper{}).BuildBatch([]observability.ExportItem{{SourceKind: observability.SourceMetricSnapshot, SourceID: "shape", Signal: observability.SignalMetric, Fact: snapshot}})
	require.Empty(t, errs)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer CANARY_AUTHORIZED_HEADER", r.Header.Get("Authorization"))
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "CANARY")
		message := &cm.ExportMetricsServiceRequest{}
		require.NoError(t, proto.Unmarshal(raw, message))
		resources := message.ResourceMetrics
		require.Len(t, resources, 1)
		attrs := map[string]string{}
		for _, a := range resources[0].Resource.Attributes {
			attrs[a.Key] = a.Value.GetStringValue()
		}
		require.Equal(t, "basalt", attrs["service.name"])
		require.Equal(t, Version, attrs["service.version"])
		count, counters, histograms, gauges := 0, 0, 0, 0
		for _, series := range resources[0].ScopeMetrics[0].Metrics {
			if sum := series.GetSum(); sum != nil {
				counters++
				require.True(t, sum.IsMonotonic)
				require.Equal(t, metric.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, sum.AggregationTemporality)
				for _, point := range sum.DataPoints {
					count++
					require.Equal(t, uint64(now.UnixNano()), point.StartTimeUnixNano)
				}
			}
			if histogram := series.GetHistogram(); histogram != nil {
				histograms++
				require.Equal(t, metric.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE, histogram.AggregationTemporality)
				for _, point := range histogram.DataPoints {
					count++
					require.Equal(t, uint64(now.UnixNano()), point.StartTimeUnixNano)
					require.Len(t, point.BucketCounts, 11)
					require.Len(t, point.ExplicitBounds, 10)
				}
			}
			if gauge := series.GetGauge(); gauge != nil {
				gauges++
				for _, point := range gauge.DataPoints {
					count++
					require.Zero(t, point.StartTimeUnixNano)
					require.Equal(t, uint64(now.UnixNano()), point.TimeUnixNano)
				}
				if strings.HasSuffix(series.Name, "_bucket") {
					require.Equal(t, "1", series.Unit)
				} else if strings.HasSuffix(series.Name, "_sum") {
					require.Equal(t, "ms", series.Unit)
				}
			}
		}
		require.Greater(t, counters, 0)
		require.Equal(t, 1, histograms, "five result-label series are points in one metric")
		require.Equal(t, 2, gauges)
		require.Equal(t, batch.RecordCount, count)
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, err = w.Write(otlpResponse(t, observability.SignalMetric, 0, "", false, "http/protobuf"))
		require.NoError(t, err)
	}))
	defer server.Close()
	sender, err := newOTLPSender(otlpSenderConfig{Endpoint: server.URL, Headers: map[string]string{"Authorization": "Bearer CANARY_AUTHORIZED_HEADER"}})
	require.NoError(t, err)
	defer sender.close()
	result, err := sender.Send(t.Context(), batch)
	require.NoError(t, err)
	require.Equal(t, batch.RecordCount, result.Accepted)
	raw, err := otlpJSONMustEnvelope(batch, sender.version)
	require.NoError(t, err)
	require.Contains(t, string(raw), strconv.FormatInt(now.UnixNano(), 10))
	require.NotContains(t, string(raw), "AGGREGATION_TEMPORALITY_CUMULATIVE")
}

func otlpJSONMustEnvelope(batch observability.ExportBatch, version string) ([]byte, error) {
	message, _, err := otlpEnvelope(batch, version)
	if err != nil {
		return nil, err
	}
	return otlpJSON(message)
}

func TestObservability_OTLPEnvironmentProxyIgnored(t *testing.T) {
	if os.Getenv("BASALT_OBS_TEST_PROXY_CHILD") == "1" {
		sender, err := newOTLPSender(otlpSenderConfig{Endpoint: "http://collector.invalid", Timeout: time.Second})
		require.NoError(t, err)
		defer sender.close()
		transport := sender.client.Transport.(*http.Transport)
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			switch address {
			case "collector.invalid:80":
				address = os.Getenv("BASALT_OBS_TEST_TARGET_ADDR")
			case os.Getenv("BASALT_OBS_TEST_PROXY_ADDR"):
			default:
				return nil, errors.New("unexpected synthetic test address")
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}
		_, err = sender.Send(t.Context(), otlpTestBatch(t, observability.SignalTrace))
		require.NoError(t, err)
		return
	}
	var direct, proxied atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		direct.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxied.Add(1); w.WriteHeader(http.StatusBadGateway) }))
	defer proxy.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestObservability_OTLPEnvironmentProxyIgnored$")
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "REQUEST_METHOD", "BASALT_OBS_TEST_PROXY_CHILD", "BASALT_OBS_TEST_TARGET_ADDR", "BASALT_OBS_TEST_PROXY_ADDR":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL, "NO_PROXY=", "no_proxy=", "REQUEST_METHOD=", "BASALT_OBS_TEST_PROXY_CHILD=1", "BASALT_OBS_TEST_TARGET_ADDR="+target.Listener.Addr().String(), "BASALT_OBS_TEST_PROXY_ADDR="+proxy.Listener.Addr().String())
	output, sendErr := cmd.CombinedOutput()
	require.Zero(t, proxied.Load(), "ambient proxy variables must not redirect telemetry")
	require.NoError(t, sendErr, string(output))
	require.Equal(t, int32(1), direct.Load())
}

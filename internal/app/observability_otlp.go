package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	cl "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	cm "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	ct "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/ymedword/basalt/internal/observability"
)

type otlpSenderConfig struct {
	Endpoint, Protocol string
	Headers            map[string]string
	Timeout            time.Duration
}

// otlpSender 不使用SDK全局注册或后台重试，只有一次显式HTTP请求。
// otlpSender uses no SDK globals or background retries, only one explicit HTTP request.
type otlpSender struct {
	version  string
	endpoint *url.URL
	cfg      otlpSenderConfig
	client   *http.Client
}

func newOTLPSender(cfg otlpSenderConfig) (*otlpSender, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || endpoint.Hostname() == "" || endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || strings.Contains(cfg.Endpoint, "${") {
		return nil, errors.New("invalid observability export endpoint")
	}
	if cfg.Protocol == "" {
		cfg.Protocol = "http/protobuf"
	}
	if cfg.Protocol != "http/protobuf" && cfg.Protocol != "http/json" {
		return nil, errors.New("invalid observability export protocol")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Timeout < 0 || cfg.Timeout > time.Minute {
		return nil, errors.New("invalid observability export timeout")
	}
	headers := make(map[string]string, len(cfg.Headers))
	for name, value := range cfg.Headers {
		if !otlpHeaderName(name) || len(value) > 8192 || strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) }) >= 0 {
			return nil, errors.New("invalid observability export headers")
		}
		switch http.CanonicalHeaderKey(name) {
		case "Content-Type", "Content-Length", "Host", "Connection", "Transfer-Encoding", "Accept-Encoding":
			return nil, errors.New("reserved observability export header")
		}
		headers[name] = value
	}
	cfg.Headers = headers
	// 与实例出网纪律一致：不读取环境代理；只有显式endpoint决定接收端。
	// Follow instance egress policy: ambient proxies cannot redirect the explicit endpoint.
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: cfg.Timeout, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1, IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: cfg.Timeout, ResponseHeaderTimeout: cfg.Timeout, MaxResponseHeaderBytes: 64 << 10}
	client := &http.Client{Transport: transport, Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	version := Version
	if len(version) > 64 || strings.IndexFunc(version, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r))
	}) >= 0 {
		version = ""
	}
	for _, fragment := range []string{"key", "token", "secret", "password", "credential", "authorization", "cookie", "bearer"} {
		if strings.Contains(strings.ToLower(version), fragment) {
			version = ""
			break
		}
	}
	return &otlpSender{cfg: cfg, client: client, endpoint: endpoint, version: version}, nil
}

func (s *otlpSender) Send(ctx context.Context, batch observability.ExportBatch) (result observability.SendResult, sendErr error) {
	started := time.Now()
	defer func() { result.Duration = time.Since(started) }()
	if err := ctx.Err(); err != nil {
		result.ErrorKind = "cancelled"
		return result, err
	}
	message, count, err := otlpEnvelope(batch, s.version)
	if err != nil || count != batch.RecordCount || count < 1 || count > 1000000 || proto.Size(message) > 16<<20 {
		result.ErrorKind = "mapping_error"
		return result, errors.New("invalid OTLP payload")
	}
	var raw []byte
	contentType := "application/x-protobuf"
	if s.cfg.Protocol == "http/json" {
		raw, err = otlpJSON(message)
		contentType = "application/json"
	} else {
		raw, err = proto.Marshal(message)
	}
	if err != nil || len(raw) > 16<<20 {
		result.ErrorKind = "mapping_error"
		return result, errors.New("invalid OTLP encoding")
	}
	endpoint := *s.endpoint
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v1/" + map[observability.SignalKind]string{observability.SignalTrace: "traces", observability.SignalLog: "logs", observability.SignalMetric: "metrics"}[batch.Signal]
	endpoint.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(raw))
	if err != nil {
		result.ErrorKind = "http_permanent"
		return result, errors.New("invalid OTLP request")
	}
	for key, value := range s.cfg.Headers {
		req.Header.Set(key, value)
	}
	req.Header.Set("Content-Type", contentType)
	response, err := s.client.Do(req)
	if err != nil {
		result.Retryable, result.ErrorKind = true, "transport_error"
		if errors.Is(err, context.Canceled) {
			result.Retryable, result.ErrorKind = false, "cancelled"
			return result, context.Canceled
		}
		var network net.Error
		if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
			result.ErrorKind = "timeout"
			return result, context.DeadlineExceeded
		}
		return result, errors.New("OTLP transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	result.HTTPStatus = response.StatusCode
	if response.StatusCode != http.StatusOK {
		result.Retryable = response.StatusCode == 429 || response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504
		result.ErrorKind = "http_permanent"
		if result.Retryable {
			result.ErrorKind = "http_retryable"
			result.RetryAfter = otlpRetryAfter(response.Header.Get("Retry-After"), time.Now())
		}
		return result, errors.New("OTLP HTTP request failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		result.Retryable, result.ErrorKind = true, "transport_error"
		return result, errors.New("OTLP response read failed")
	}
	if len(body) > 1<<20 {
		result.ErrorKind = "response_too_large"
		return result, errors.New("OTLP response exceeded limit")
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != contentType {
		result.Retryable, result.ErrorKind = true, "protocol_invalid"
		return result, errors.New("invalid OTLP response content type")
	}
	rejected, warning, partial, err := decodeOTLPResponse(batch.Signal, s.cfg.Protocol, body)
	if err != nil {
		result.Retryable, result.ErrorKind = true, "protocol_invalid"
		return result, errors.New("invalid OTLP response")
	}
	result.PartialSuccess = partial
	if warning {
		result.Warning = "receiver_warning"
	}
	if rejected < 0 || rejected > int64(batch.RecordCount) {
		result.Accepted, result.Rejected, result.ErrorKind = -1, -1, "protocol_invalid"
		return result, errors.New("invalid OTLP rejection count")
	}
	result.Accepted, result.Rejected = batch.RecordCount-int(rejected), int(rejected)
	return result, nil
}

func (s *otlpSender) close() {
	if s != nil && s.client != nil {
		s.client.CloseIdleConnections()
	}
}

func otlpHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func otlpRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil {
		return min(max(time.Duration(seconds)*time.Second, 0), 5*time.Minute)
	}
	if at, err := http.ParseTime(value); err == nil {
		return min(max(at.Sub(now), 0), 5*time.Minute)
	}
	return 0
}

func decodeOTLPResponse(signal observability.SignalKind, protocol string, body []byte) (rejected int64, warning, partial bool, err error) {
	unmarshal := func(message proto.Message) error {
		if protocol == "http/json" {
			return (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, message)
		}
		return proto.Unmarshal(body, message)
	}
	switch signal {
	case observability.SignalTrace:
		message := &ct.ExportTraceServiceResponse{}
		if err = unmarshal(message); err != nil {
			return
		}
		if value := message.PartialSuccess; value != nil {
			rejected, warning, partial = value.RejectedSpans, value.ErrorMessage != "", true
		}
	case observability.SignalLog:
		message := &cl.ExportLogsServiceResponse{}
		if err = unmarshal(message); err != nil {
			return
		}
		if value := message.PartialSuccess; value != nil {
			rejected, warning, partial = value.RejectedLogRecords, value.ErrorMessage != "", true
		}
	case observability.SignalMetric:
		message := &cm.ExportMetricsServiceResponse{}
		if err = unmarshal(message); err != nil {
			return
		}
		if value := message.PartialSuccess; value != nil {
			rejected, warning, partial = value.RejectedDataPoints, value.ErrorMessage != "", true
		}
	default:
		err = errors.New("unknown OTLP signal")
	}
	return
}

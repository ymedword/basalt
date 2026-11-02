package cli

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ObservabilityConfig 是只在启动读取的观测段；没有规则DSL或热加载入口。
// ObservabilityConfig is startup-only and contains neither a rule DSL nor hot reload.
type ObservabilityConfig struct {
	Enabled   *bool
	Retention ObservabilityRetentionConfig
	Export    ObservabilityExportConfig
	Alerts    ObservabilityAlertConfig
}

// ObservabilityRetentionConfig 的关闭值保留，天数只提供候选默认值。
// ObservabilityRetentionConfig preserves disabled cleanup and candidate durations.
type ObservabilityRetentionConfig struct {
	CleanupEnabled                                                                 bool
	HistoryEnabled                                                                 bool
	HistoryDays                                                                    int
	DiagnosticEventsDays, AssemblyRecordsDays, AlertHistoryDays, ExportHistoryDays int
	BatchSize, MaxBatchesPerRun                                                    int
	Interval                                                                       time.Duration
}

// ObservabilityExportConfig 的端点和headers不进入任何JSON投影。
// ObservabilityExportConfig excludes endpoint and headers from JSON projections.
type ObservabilityExportConfig struct {
	Enabled                                                                                                   bool
	Endpoint                                                                                                  string `json:"-"`
	Protocol                                                                                                  string
	Headers                                                                                                   string `json:"-"`
	BatchSize, MaxAttempts, ScanPageSize                                                                      int
	Timeout, MaxItemAge, ScanOverlap, SafetyLag, ReconcileHorizon, InitialBackfill, LeaseTTL, MetricsInterval time.Duration
}

// ObservabilityAlertConfig 只承载总开关和评估频率。
// ObservabilityAlertConfig contains only the global switch and evaluation interval.
type ObservabilityAlertConfig struct {
	Enabled            *bool
	EvaluationInterval time.Duration
}

// DefaultObservabilityConfig 与已审候选值一致，网络导出和清理保持关闭。
// DefaultObservabilityConfig matches approved candidates with export and cleanup disabled.
func DefaultObservabilityConfig() ObservabilityConfig {
	enabled, alerts := true, true
	return ObservabilityConfig{Enabled: &enabled, Retention: ObservabilityRetentionConfig{DiagnosticEventsDays: 30, HistoryDays: 30, AssemblyRecordsDays: 30, AlertHistoryDays: 30, ExportHistoryDays: 30, BatchSize: 1000, MaxBatchesPerRun: 20, Interval: time.Hour}, Export: ObservabilityExportConfig{Protocol: "http/protobuf", BatchSize: 256, Timeout: 10 * time.Second, MaxAttempts: 10, MaxItemAge: 24 * time.Hour, ScanOverlap: 5 * time.Minute, SafetyLag: 2 * time.Minute, ReconcileHorizon: 24 * time.Hour, InitialBackfill: 24 * time.Hour, ScanPageSize: 500, LeaseTTL: 5 * time.Minute, MetricsInterval: time.Minute}, Alerts: ObservabilityAlertConfig{Enabled: &alerts, EvaluationInterval: time.Minute}}
}

func parseObservabilityConfig(node any, present bool) (ObservabilityConfig, error) {
	cfg := DefaultObservabilityConfig()
	if !present {
		return cfg, nil
	}
	err := obsConfigSection(node, "observability", map[string]func(any) error{
		"enabled": obsConfigBool(&cfg.Enabled, "observability.enabled"),
		"retention": func(value any) error {
			return obsConfigSection(value, "observability.retention", map[string]func(any) error{
				"history_enabled":        obsConfigFlag(&cfg.Retention.HistoryEnabled, "observability.retention.history_enabled"),
				"history_days":           obsConfigInt(&cfg.Retention.HistoryDays, 1, 365, "observability.retention.history_days"),
				"cleanup_enabled":        obsConfigFlag(&cfg.Retention.CleanupEnabled, "observability.retention.cleanup_enabled"),
				"diagnostic_events_days": obsConfigInt(&cfg.Retention.DiagnosticEventsDays, 1, 365, "observability.retention.diagnostic_events_days"),
				"assembly_records_days":  obsConfigInt(&cfg.Retention.AssemblyRecordsDays, 1, 365, "observability.retention.assembly_records_days"),
				"alert_history_days":     obsConfigInt(&cfg.Retention.AlertHistoryDays, 1, 365, "observability.retention.alert_history_days"),
				"export_history_days":    obsConfigInt(&cfg.Retention.ExportHistoryDays, 1, 365, "observability.retention.export_history_days"),
				"batch_size":             obsConfigInt(&cfg.Retention.BatchSize, 100, 10000, "observability.retention.batch_size"),
				"max_batches_per_run":    obsConfigInt(&cfg.Retention.MaxBatchesPerRun, 1, 100, "observability.retention.max_batches_per_run"),
				"interval":               obsConfigDuration(&cfg.Retention.Interval, time.Hour, 24*time.Hour, "observability.retention.interval"),
			})
		},
		"alerts": func(value any) error {
			return obsConfigSection(value, "observability.alerts", map[string]func(any) error{
				"enabled":             obsConfigBool(&cfg.Alerts.Enabled, "observability.alerts.enabled"),
				"evaluation_interval": obsConfigDuration(&cfg.Alerts.EvaluationInterval, time.Second, time.Hour, "observability.alerts.evaluation_interval"),
			})
		},
		"export": func(value any) error {
			return obsConfigSection(value, "observability.export", map[string]func(any) error{
				"enabled":           obsConfigFlag(&cfg.Export.Enabled, "observability.export.enabled"),
				"endpoint":          obsConfigString(&cfg.Export.Endpoint, "observability.export.endpoint"),
				"protocol":          obsConfigString(&cfg.Export.Protocol, "observability.export.protocol"),
				"headers":           obsConfigString(&cfg.Export.Headers, "observability.export.headers"),
				"batch_size":        obsConfigInt(&cfg.Export.BatchSize, 1, 2048, "observability.export.batch_size"),
				"max_attempts":      obsConfigInt(&cfg.Export.MaxAttempts, 1, 100, "observability.export.max_attempts"),
				"scan_page_size":    obsConfigInt(&cfg.Export.ScanPageSize, 1, 10000, "observability.export.scan_page_size"),
				"timeout":           obsConfigDuration(&cfg.Export.Timeout, time.Millisecond, time.Minute, "observability.export.timeout"),
				"max_item_age":      obsConfigDuration(&cfg.Export.MaxItemAge, time.Minute, 365*24*time.Hour, "observability.export.max_item_age"),
				"scan_overlap":      obsConfigDuration(&cfg.Export.ScanOverlap, 0, 24*time.Hour, "observability.export.scan_overlap"),
				"safety_lag":        obsConfigDuration(&cfg.Export.SafetyLag, 0, time.Hour, "observability.export.safety_lag"),
				"reconcile_horizon": obsConfigDuration(&cfg.Export.ReconcileHorizon, time.Minute, 365*24*time.Hour, "observability.export.reconcile_horizon"),
				"initial_backfill":  obsConfigDuration(&cfg.Export.InitialBackfill, time.Minute, 365*24*time.Hour, "observability.export.initial_backfill"),
				"lease_ttl":         obsConfigDuration(&cfg.Export.LeaseTTL, time.Second, time.Hour, "observability.export.lease_ttl"),
				"metrics_interval":  obsConfigDuration(&cfg.Export.MetricsInterval, time.Second, 24*time.Hour, "observability.export.metrics_interval"),
			})
		},
	})
	if err != nil {
		return ObservabilityConfig{}, err
	}
	if err := ValidateObservabilityConfig(cfg); err != nil {
		return ObservabilityConfig{}, err
	}
	return cfg, nil
}

// Effective 仅给整段未装载的零值补默认，显式零值由严格解析拒绝。
// Effective defaults an entirely absent section; explicit invalid zeros are rejected by parsing.
func (cfg ObservabilityConfig) Effective() ObservabilityConfig {
	if cfg == (ObservabilityConfig{}) {
		return DefaultObservabilityConfig()
	}
	return cfg
}

// ValidateHistoryWindow 保证启用清理时装配保留不短于既有诊断历史窗口。
// ValidateHistoryWindow protects the existing diagnostic history when cleanup is enabled.
func (cfg ObservabilityConfig) ValidateHistoryWindow(diagnosticDays int) error {
	if cfg.Retention.CleanupEnabled && cfg.Retention.AssemblyRecordsDays < diagnosticDays {
		return obsConfigError("observability.retention.assembly_records_days", "must cover the configured diagnostic history window")
	}
	return nil
}

// ValidateObservabilityConfig 复核程序装配的同一边界，不回显端点或headers。
// ValidateObservabilityConfig rechecks assembly boundaries without echoing endpoint or headers.
func ValidateObservabilityConfig(cfg ObservabilityConfig) error {
	if cfg.Retention.HistoryEnabled && (cfg.Enabled == nil || !*cfg.Enabled) {
		return obsConfigError("observability.retention.history_enabled", "requires observability.enabled")
	}

	if cfg.Enabled == nil || cfg.Alerts.Enabled == nil {
		return obsConfigError("observability", "missing resolved defaults")
	}
	for _, field := range []struct {
		name            string
		value, min, max int
	}{
		{"retention.history_days", cfg.Retention.HistoryDays, 1, 365},
		{"retention.diagnostic_events_days", cfg.Retention.DiagnosticEventsDays, 1, 365},
		{"retention.assembly_records_days", cfg.Retention.AssemblyRecordsDays, 1, 365},
		{"retention.alert_history_days", cfg.Retention.AlertHistoryDays, 1, 365},
		{"retention.export_history_days", cfg.Retention.ExportHistoryDays, 1, 365},
		{"retention.batch_size", cfg.Retention.BatchSize, 100, 10000},
		{"retention.max_batches_per_run", cfg.Retention.MaxBatchesPerRun, 1, 100},
		{"export.batch_size", cfg.Export.BatchSize, 1, 2048},
		{"export.max_attempts", cfg.Export.MaxAttempts, 1, 100},
		{"export.scan_page_size", cfg.Export.ScanPageSize, 1, 10000},
	} {
		if field.value < field.min || field.value > field.max {
			return obsConfigError("observability."+field.name, "out of range")
		}
	}
	for _, field := range []struct {
		name            string
		value, min, max time.Duration
	}{
		{"retention.interval", cfg.Retention.Interval, time.Hour, 24 * time.Hour}, {"alerts.evaluation_interval", cfg.Alerts.EvaluationInterval, time.Second, time.Hour}, {"export.timeout", cfg.Export.Timeout, time.Millisecond, time.Minute}, {"export.max_item_age", cfg.Export.MaxItemAge, time.Minute, 365 * 24 * time.Hour}, {"export.scan_overlap", cfg.Export.ScanOverlap, 0, 24 * time.Hour}, {"export.safety_lag", cfg.Export.SafetyLag, 0, time.Hour}, {"export.reconcile_horizon", cfg.Export.ReconcileHorizon, time.Minute, 365 * 24 * time.Hour}, {"export.initial_backfill", cfg.Export.InitialBackfill, time.Minute, 365 * 24 * time.Hour}, {"export.lease_ttl", cfg.Export.LeaseTTL, time.Second, time.Hour}, {"export.metrics_interval", cfg.Export.MetricsInterval, time.Second, 24 * time.Hour},
	} {
		if field.value < field.min || field.value > field.max {
			return obsConfigError("observability."+field.name, "out of range")
		}
	}
	if cfg.Export.InitialBackfill <= cfg.Export.SafetyLag {
		return obsConfigError("observability.export.initial_backfill", "must exceed safety_lag")
	}
	if cfg.Export.LeaseTTL <= cfg.Export.Timeout {
		return obsConfigError("observability.export.lease_ttl", "must exceed timeout")
	}
	if cfg.Retention.CleanupEnabled && cfg.Retention.AssemblyRecordsDays < cfg.Retention.DiagnosticEventsDays {
		return obsConfigError("observability.retention.assembly_records_days", "must cover the diagnostic history window")
	}
	if cfg.Export.Protocol != "http/protobuf" && cfg.Export.Protocol != "http/json" {
		return obsConfigError("observability.export.protocol", "unsupported protocol")
	}
	endpoint := cfg.Export.Endpoint
	if cfg.Export.Enabled && endpoint == "" {
		return obsConfigError("observability.export.endpoint", "required when export is enabled")
	}
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(endpoint, "#") || strings.Contains(endpoint, "${") {
			return obsConfigError("observability.export.endpoint", "invalid URL")
		}
	}
	_, err := cfg.Export.ParsedHeaders()
	return err
}

// ParsedHeaders 只解析内存中已从环境得到的OTLP标准键值列表。
// ParsedHeaders parses the in-memory OTLP key/value list already resolved from environment.
func (cfg ObservabilityExportConfig) ParsedHeaders() (map[string]string, error) {
	out := map[string]string{}
	if cfg.Headers == "" {
		return out, nil
	}
	if len(cfg.Headers) > 16384 {
		return nil, obsConfigError("observability.export.headers", "too large")
	}
	parts := strings.Split(cfg.Headers, ",")
	if len(parts) > 32 {
		return nil, obsConfigError("observability.export.headers", "too many entries")
	}
	for _, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || len(key) > 128 {
			return nil, obsConfigError("observability.export.headers", "invalid header list")
		}
		for _, r := range key {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
				return nil, obsConfigError("observability.export.headers", "invalid header name")
			}
		}
		key = http.CanonicalHeaderKey(key)
		if _, exists := out[key]; exists {
			return nil, obsConfigError("observability.export.headers", "duplicate header")
		}
		if slices.Contains([]string{"Content-Type", "Content-Length", "Host", "Connection", "Transfer-Encoding", "Accept-Encoding"}, key) {
			return nil, obsConfigError("observability.export.headers", "reserved header")
		}
		value, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil || len(value) > 8192 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, obsConfigError("observability.export.headers", "invalid header value")
		}
		out[key] = value
	}
	return out, nil
}

func obsConfigError(path, reason string) error { return fmt.Errorf("%s: %s", path, reason) }

func obsConfigSection(node any, path string, fields map[string]func(any) error) error {
	values, ok := node.(map[string]any)
	if !ok {
		return obsConfigError(path, "must be a mapping")
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		parse, ok := fields[key]
		if !ok {
			return obsConfigError(path, "unknown field")
		}
		if err := parse(values[key]); err != nil {
			return err
		}
	}
	return nil
}

func obsConfigBool(target **bool, path string) func(any) error {
	return func(value any) error {
		v, ok := value.(bool)
		if !ok {
			return obsConfigError(path, "must be boolean")
		}
		*target = &v
		return nil
	}
}

func obsConfigFlag(target *bool, path string) func(any) error {
	return func(value any) error {
		v, ok := value.(bool)
		if !ok {
			return obsConfigError(path, "must be boolean")
		}
		*target = v
		return nil
	}
}

func obsConfigString(target *string, path string) func(any) error {
	return func(value any) error {
		v, ok := value.(string)
		if !ok {
			return obsConfigError(path, "must be a string")
		}
		*target = v
		return nil
	}
}

func obsConfigInt(target *int, minValue, maxValue int, path string) func(any) error {
	return func(value any) error {
		var n int64
		switch v := value.(type) {
		case int:
			n = int64(v)
		case int64:
			n = v
		case uint64:
			parsed, err := strconv.ParseInt(strconv.FormatUint(v, 10), 10, 64)
			if err != nil {
				return obsConfigError(path, "out of range")
			}
			n = parsed
		default:
			return obsConfigError(path, "must be an integer")
		}
		if n < int64(minValue) || n > int64(maxValue) {
			return obsConfigError(path, "out of range")
		}
		*target = int(n)
		return nil
	}
}

func obsConfigDuration(target *time.Duration, minValue, maxValue time.Duration, path string) func(any) error {
	return func(value any) error {
		v, ok := value.(string)
		if !ok {
			return obsConfigError(path, "must be a duration string")
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < minValue || d > maxValue {
			return obsConfigError(path, "duration out of range")
		}
		*target = d
		return nil
	}
}

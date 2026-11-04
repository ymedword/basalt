package observability

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// MetricType 区分进程累计值与数据库窗口快照。
// MetricType distinguishes process cumulative values from database window snapshots.
type MetricType string

// 固定指标类型与OTLP映射一致。
// The fixed metric types match their OTLP mapping.
const (
	MetricCounter   MetricType = "counter"
	MetricGauge     MetricType = "gauge"
	MetricHistogram MetricType = "histogram"
)

// MetricScope 防止把本进程读数当成跨重启累计。
// MetricScope prevents process-local readings from implying persistence.
type MetricScope string

// 作用域只有本进程和数据库窗口两种。
// Scope is either this process or a database window.
const (
	ScopeProcess  MetricScope = "process"
	ScopeDatabase MetricScope = "database"
)

// MetricSample 显式携带可用性，缺失值不会在JSON中伪装成0。
// MetricSample makes availability explicit and serializes missing values as null.
type MetricSample struct {
	Name      string            `json:"name"`
	Type      MetricType        `json:"type"`
	Unit      string            `json:"unit"`
	Value     float64           `json:"value"`
	Window    time.Duration     `json:"window"`
	Scope     MetricScope       `json:"scope"`
	Available bool              `json:"available"`
	Reason    string            `json:"reason"`
	Labels    map[string]string `json:"labels"`
}

// MarshalJSON 保持不可用与真实零值的区别。
// MarshalJSON preserves the difference between an unavailable value and a measured zero.
func (s MetricSample) MarshalJSON() ([]byte, error) {
	type plain MetricSample
	var value *float64
	if s.Available {
		value = &s.Value
	}
	return json.Marshal(struct {
		plain
		Value *float64 `json:"value"`
	}{plain(s), value})
}

// MetricsSnapshot 是一次派生读数，进程起点跟随快照持久化。
// MetricsSnapshot is a derived reading whose process start survives snapshot replay.
type MetricsSnapshot struct {
	Samples     []MetricSample `json:"samples"`
	CollectedAt time.Time      `json:"collected_at"`
	startedAt   time.Time
}

// MarshalJSON 记录累计进程指标的真实起点，供重启后导出旧快照。
// MarshalJSON records the cumulative start for replay after restart.
func (s MetricsSnapshot) MarshalJSON() ([]byte, error) {
	type plain MetricsSnapshot
	return json.Marshal(struct {
		plain
		Start time.Time `json:"process_started_at"`
	}{plain(s), s.startedAt})
}

// UnmarshalJSON 恢复旧快照的原起点，不借用新进程时间。
// UnmarshalJSON restores the original start rather than borrowing a new process start.
func (s *MetricsSnapshot) UnmarshalJSON(raw []byte) error {
	type plain MetricsSnapshot
	var out struct {
		plain
		Start time.Time `json:"process_started_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	*s = MetricsSnapshot(out.plain)
	s.startedAt = out.Start
	return nil
}

// MetricsStore 只提供SQL聚合样本，不暴露业务写口。
// MetricsStore exposes aggregate readings without business mutation.
type MetricsStore interface {
	WindowCounts(context.Context, time.Duration) ([]MetricSample, error)
}

type metricWindowAtStore interface {
	WindowCountsAt(context.Context, time.Duration, time.Time) ([]MetricSample, error)
}

type metricDefinition struct {
	unit   string
	labels map[string][]string
}

var (
	metricRules    = []string{"audit_write_failed", "diagnostic_write_failed", "export_permanent_failure", "export_backlog_stuck", "cleanup_failed", "run_failure_burst", "llm_timeout_burst", "tool_denial_burst"}
	metricKinds    = []string{"ingest_stage", "index_switch", "retrieval", "assembly", "embedding", "llm_call", "tool_call", "permission_decision", "quota_decision", "migration"}
	metricSources  = []string{"runs", "llm_calls", "tool_invocations", "embedding_calls", "task_executions", "management_operations", "kb_ingest_jobs", "diagnostic_events", "metric_snapshot"}
	metricReasons  = []string{"none", "unclassified", "vector_unavailable", "keyword_unavailable", "rerank_timeout", "permission_denied", "budget_trimmed", "history_trimmed", "no_answer", "citation_unresolved", "chunker_failed", "embed_failed", "index_failed", "key_revoked", "quota_exhausted", "rate_limited", "migration_verify_failed", "llm_call_failed", "concurrency_limited", "chunk_invalidated"}
	durationBounds = []float64{10, 50, 100, 250, 500, 1000, 5000, 15000, 30000, 60000}
	durationLE     = []string{"10", "50", "100", "250", "500", "1000", "5000", "15000", "30000", "60000", "+Inf"}
)

var processMetrics = map[string]metricDefinition{
	"obs_diagnostic_events_total":         {"1", map[string][]string{"kind": metricKinds, "result": {"recorded", "sampled_out", "write_failed"}}},
	"obs_diagnostic_write_failures_total": {"1", map[string][]string{"kind": metricKinds}},
	"obs_audit_write_failures_total":      {"1", map[string][]string{}},
	"obs_export_items_total":              {"1", map[string][]string{"source_kind": metricSources, "result": {"sent", "unconfirmed", "retryable", "permanent", "dropped", "stale"}}},
	"obs_alert_transitions_total":         {"1", map[string][]string{"rule_id": metricRules, "to_state": {"inactive", "pending", "firing", "resolved", "unknown"}}},
	"obs_cleanup_deleted_rows_total":      {"1", map[string][]string{"table_kind": {"diagnostic_events", "context_assembly_records", "alert_history", "export_history"}}},
	"obs_cleanup_skipped_rows_total":      {"1", map[string][]string{"reason": {"scan_not_covered", "out_of_coverage", "unexported_backlog", "unconfirmed_export", "unexported_terminal", "policy_disabled"}}},
}

var databaseMetrics = map[string]metricDefinition{
	"obs_flow_tasks_total":             {"1", map[string][]string{"status": {"queued", "running", "succeeded", "failed", "stopped", "blocked", "cancelling", "cancelled", "abandoned"}}},
	"obs_flow_unknown_actions_total":   {"1", map[string][]string{}},
	"obs_export_queue_depth":           {"1", map[string][]string{"state": {"pending", "inflight", "retry"}}},
	"obs_export_unconfirmed_items":     {"1", map[string][]string{}},
	"obs_export_oldest_pending_age_ms": {"ms", map[string][]string{}},
	"obs_runs_total":                   {"1", map[string][]string{"status": {"running", "completed", "limit_reached", "cancelled", "failed"}, "trigger": {"user", "cron", "manual", "api"}}},
	"obs_run_duration_ms":              {"ms", map[string][]string{"status": {"completed", "limit_reached", "cancelled", "failed"}}},
	"obs_llm_call_duration_ms":         {"ms", map[string][]string{"status": {"success", "failed"}, "purpose": {"chat", "rerank", "summary", "embedding", "compaction"}}},
	"obs_llm_timeout_total":            {"1", map[string][]string{}},
	"obs_tool_invocations_total":       {"1", map[string][]string{"status": {"pending", "success", "failed", "unknown"}}},
	"obs_tool_denials_total":           {"1", map[string][]string{}},
	"obs_retrieval_empty_total":        {"1", map[string][]string{"reason_code": metricReasons}},
	"obs_assembly_missing_total":       {"1", map[string][]string{}},
}

type (
	metricCounter struct {
		name   string
		labels map[string]string
		value  float64
	}
	metricHistogram struct {
		counts []uint64
		sum    float64
	}
)

// MetricsCollector 所有序列来自固定目录，拒绝未知名、标签和枚举值。
// MetricsCollector accepts only a fixed catalog and bounded label values.
type MetricsCollector struct {
	store       MetricsStore
	clock       func() time.Time
	startedAt   time.Time
	mu          sync.Mutex
	counters    map[string]*metricCounter
	durations   map[string]*metricHistogram
	lastFailure map[string]time.Time
}

// NewMetricsCollector 不启动采集线程，进程起点只在构造时记录。
// NewMetricsCollector records one process start without spawning a goroutine.
func NewMetricsCollector(store MetricsStore, clock func() time.Time) *MetricsCollector {
	if clock == nil {
		clock = time.Now
	}
	c := &MetricsCollector{store: store, clock: clock, startedAt: clock().UTC(), counters: map[string]*metricCounter{}, durations: map[string]*metricHistogram{}, lastFailure: map[string]time.Time{}}
	for name, definition := range processMetrics {
		for _, labels := range labelCombinations(definition.labels) {
			c.counters[metricKey(name, labels)] = &metricCounter{name: name, labels: labels}
		}
	}
	for _, result := range []string{"", "ok", "warning", "partial", "error"} {
		c.durations[result] = &metricHistogram{counts: make([]uint64, len(durationBounds)+1)}
	}
	return c
}

func labelCombinations(labels map[string][]string) []map[string]string {
	keys := slices.Sorted(maps.Keys(labels))
	out := []map[string]string{{}}
	for _, key := range keys {
		var next []map[string]string
		for _, base := range out {
			for _, value := range labels[key] {
				copy := maps.Clone(base)
				copy[key] = value
				next = append(next, copy)
			}
		}
		out = next
	}
	return out
}

func metricKey(name string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(name)
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		b.WriteByte('|')
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(labels[key])
	}
	return b.String()
}

func validMetricLabels(labels map[string]string, allowed map[string][]string) bool {
	for key, value := range labels {
		if !slices.Contains(allowed[key], value) {
			return false
		}
	}
	return true
}

// IncCounter 不接受用户维度，非法序列在分配内存前丢弃。
// IncCounter drops invalid series before allocating memory.
func (c *MetricsCollector) IncCounter(name string, labels map[string]string) {
	definition, ok := processMetrics[name]
	if !ok || len(labels) != len(definition.labels) || !validMetricLabels(labels, definition.labels) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := metricKey(name, labels)
	series := c.counters[key]
	if series == nil {
		return
	}
	series.value++
	if name == "obs_audit_write_failures_total" || name == "obs_diagnostic_write_failures_total" {
		c.lastFailure[name] = c.clock()
	}
}

// ObserveDuration 记录所有批次的累计直方图，不推断结果类别。
// ObserveDuration records a cumulative histogram without inferring an outcome category.
func (c *MetricsCollector) ObserveDuration(name string, d time.Duration) {
	c.observeDuration(name, d, "")
}

// ObserveExportDuration 用固定结果枚举补充发送外层的分类直方图。
// ObserveExportDuration records the fixed outcome category at the send boundary.
func (c *MetricsCollector) ObserveExportDuration(d time.Duration, result string) {
	if !slices.Contains([]string{"ok", "warning", "partial", "error"}, result) {
		return
	}
	c.observeDuration("obs_export_batch_duration_ms", d, "")
	c.observeDuration("obs_export_batch_duration_ms", d, result)
}

func (c *MetricsCollector) observeDuration(name string, d time.Duration, result string) {
	if name != "obs_export_batch_duration_ms" || d < 0 || result != "" && !slices.Contains([]string{"ok", "warning", "partial", "error"}, result) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.durations[result]
	if h == nil {
		h = &metricHistogram{counts: make([]uint64, len(durationBounds)+1)}
		c.durations[result] = h
	}
	value := float64(d) / float64(time.Millisecond)
	h.sum += value
	for i, bound := range durationBounds {
		if value <= bound {
			h.counts[i]++
		}
	}
	h.counts[len(durationBounds)]++
}

// Snapshot 只接受四种固定窗口，缺失来源返回不可用样本。
// Snapshot accepts four fixed windows and keeps unavailable sources explicit.
func (c *MetricsCollector) Snapshot(ctx context.Context, window time.Duration) (MetricsSnapshot, error) {
	if !metricWindow(window) {
		return MetricsSnapshot{}, ErrInvalidQuery
	}
	out := MetricsSnapshot{CollectedAt: c.clock().UTC(), startedAt: c.startedAt, Samples: []MetricSample{}}
	c.mu.Lock()
	for _, counter := range c.counters {
		out.Samples = append(out.Samples, MetricSample{
			Name: counter.name, Type: MetricCounter,
			Unit: processMetrics[counter.name].unit, Value: counter.value, Scope: ScopeProcess, Available: true, Labels: maps.Clone(counter.labels),
		})
	}
	for result, histogram := range c.durations {
		out.Samples = append(out.Samples, histogramSamples("obs_export_batch_duration_ms", histogram, result)...)
	}
	c.mu.Unlock()
	var samples []MetricSample
	if c.store == nil {
		samples = unavailableDatabase("not_supported", window)
	} else {
		var err error
		if store, ok := c.store.(metricWindowAtStore); ok {
			samples, err = store.WindowCountsAt(ctx, window, out.CollectedAt)
		} else {
			samples, err = c.store.WindowCounts(ctx, window)
		}
		if err != nil {
			samples = unavailableDatabase("query_failed", window)
		}
	}
	seen := make(map[string]bool)
	for _, sample := range samples {
		base := metricBase(sample.Name)
		definition, ok := databaseMetrics[base]
		if !ok {
			continue
		}
		allowed := maps.Clone(definition.labels)
		if strings.HasSuffix(sample.Name, "_bucket") && base != sample.Name {
			allowed["le"] = durationLE
		}
		if base == "obs_run_duration_ms" || base == "obs_llm_call_duration_ms" {
			allowed["quantile"] = []string{"0.5", "0.95", "0.99"}
		}
		if sample.Type != MetricGauge || !validMetricLabels(sample.Labels, allowed) || math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
			sample = MetricSample{Name: base, Type: MetricGauge, Available: false, Reason: "invalid_source_sample"}
		}
		sample.Scope, sample.Window, sample.Unit = ScopeDatabase, window, definition.unit
		if base != sample.Name && (strings.HasSuffix(sample.Name, "_bucket") || strings.HasSuffix(sample.Name, "_count")) {
			sample.Unit = "1"
		}
		sample.Labels = maps.Clone(sample.Labels)
		if sample.Labels == nil {
			sample.Labels = map[string]string{}
		}
		if !sample.Available && !slices.Contains([]string{"no_data", "query_failed", "not_supported", "not_recorded", "invalid_source_sample"}, sample.Reason) {
			sample.Reason = "query_failed"
		}
		seen[base] = true
		out.Samples = append(out.Samples, sample)
	}
	for _, missing := range unavailableDatabase("not_recorded", window) {
		if !seen[missing.Name] {
			out.Samples = append(out.Samples, missing)
		}
	}
	slices.SortFunc(out.Samples, func(a, b MetricSample) int {
		return strings.Compare(metricKey(a.Name, a.Labels), metricKey(b.Name, b.Labels))
	})
	return out, nil
}

func metricBase(name string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, suffix) {
			base := strings.TrimSuffix(name, suffix)
			if base == "obs_run_duration_ms" || base == "obs_llm_call_duration_ms" {
				return base
			}
		}
	}
	return name
}

func metricWindow(window time.Duration) bool {
	return slices.Contains([]time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 24 * time.Hour}, window)
}

func unavailableDatabase(reason string, window time.Duration) []MetricSample {
	out := make([]MetricSample, 0, len(databaseMetrics))
	for name, definition := range databaseMetrics {
		out = append(out, MetricSample{Name: name, Type: MetricGauge, Unit: definition.unit, Scope: ScopeDatabase, Window: window, Available: false, Reason: reason, Labels: map[string]string{}})
	}
	return out
}

func histogramSamples(name string, h *metricHistogram, result string) []MetricSample {
	labels := map[string]string{}
	if result != "" {
		labels["result"] = result
	}
	out := make([]MetricSample, 0, len(h.counts)+2)
	for i, count := range h.counts {
		l := maps.Clone(labels)
		l["le"] = durationLE[i]
		out = append(out, MetricSample{Name: name + "_bucket", Type: MetricHistogram, Unit: "1", Value: float64(count), Scope: ScopeProcess, Available: true, Labels: l})
	}
	for suffix, value := range map[string]float64{"_sum": h.sum, "_count": float64(h.counts[len(h.counts)-1])} {
		unit := "ms"
		if suffix == "_count" {
			unit = "1"
		}
		out = append(out, MetricSample{Name: name + suffix, Type: MetricHistogram, Unit: unit, Value: value, Scope: ScopeProcess, Available: true, Labels: maps.Clone(labels)})
	}
	return out
}

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

var _ observability.MetricsStore = (*ObservabilityStore)(nil)

func obsGauge(name, unit string, value float64, labels map[string]string) observability.MetricSample {
	if labels == nil {
		labels = map[string]string{}
	}
	return observability.MetricSample{Name: name, Type: observability.MetricGauge, Unit: unit, Value: value, Available: true, Scope: observability.ScopeDatabase, Labels: labels}
}

// WindowCounts 在SQL中聚合完整时间窗，不加载原始业务行或敏感列。
// WindowCounts aggregates the whole window in SQL without loading raw business rows.
func (s *ObservabilityStore) WindowCounts(ctx context.Context, window time.Duration) ([]observability.MetricSample, error) {
	if s == nil || s.db == nil || s.db.pool == nil {
		return nil, errors.New("observability metrics database unavailable")
	}
	return s.WindowCountsAt(ctx, window, s.clock())
}

// WindowCountsAt 使用快照实际时点，避免把另一时间窗标成CollectedAt。
// WindowCountsAt uses the snapshot instant instead of labeling another window as CollectedAt.
func (s *ObservabilityStore) WindowCountsAt(ctx context.Context, window time.Duration, now time.Time) ([]observability.MetricSample, error) {
	if !slices.Contains([]time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 24 * time.Hour}, window) {
		return nil, observability.ErrInvalidQuery
	}
	if s == nil || s.db == nil || s.db.pool == nil {
		return nil, errors.New("observability metrics database unavailable")
	}
	if now.IsZero() {
		return nil, observability.ErrInvalidQuery
	}
	args := []any{now.Add(-window).UnixMilli(), now.UnixMilli()}
	out := make([]observability.MetricSample, 0, 100)
	for _, group := range []struct{ name, query, label1, label2 string }{
		{"obs_flow_tasks_total", `SELECT status,''::text,count(*) FROM flow_tasks WHERE created_at BETWEEN $1 AND $2 GROUP BY status`, "status", ""},
		{"obs_runs_total", `SELECT status,trigger,count(*) FROM runs WHERE started_at BETWEEN $1 AND $2 GROUP BY status,trigger`, "status", "trigger"},
		{"obs_tool_invocations_total", `SELECT status,''::text,count(*) FROM tool_invocations WHERE started_at BETWEEN $1 AND $2 GROUP BY status`, "status", ""},
		{"obs_retrieval_empty_total", `SELECT COALESCE(reason_codes->>0,''),''::text,count(*) FROM context_assembly_records WHERE created_at BETWEEN $1 AND $2 AND purpose='retrieval' AND jsonb_array_length(hits)=0 GROUP BY reason_codes->>0`, "reason_code", ""},
	} {
		samples, err := obsRows(ctx, s.db, group.query, args, func(rows *sql.Rows) (observability.MetricSample, error) {
			var first, second string
			var count float64
			if err := rows.Scan(&first, &second, &count); err != nil {
				return observability.MetricSample{}, err
			}
			if group.label1 == "reason_code" {
				if first == "" {
					first = "none"
				} else if !diagnostics.ReasonCode(first).Valid() {
					first = "unclassified"
				}
			}
			labels := map[string]string{group.label1: first}
			if group.label2 != "" {
				labels[group.label2] = second
			}
			return obsGauge(group.name, "1", count, labels), nil
		})
		if err != nil {
			return nil, err
		}
		if len(samples) == 0 {
			samples = append(samples, obsGauge(group.name, "1", 0, nil))
		}
		out = append(out, samples...)
	}
	for _, scalar := range []struct{ name, query, unit string }{
		{"obs_flow_unknown_actions_total", `SELECT count(*) FROM flow_action_attempts WHERE outcome='unknown' AND created_at BETWEEN $1 AND $2`, "1"},
		{"obs_llm_timeout_total", `SELECT count(*) FROM llm_calls WHERE created_at BETWEEN $1 AND $2 AND error_kind='timeout'`, "1"},
		{"obs_tool_denials_total", `SELECT (SELECT count(*) FROM tool_invocations WHERE started_at BETWEEN $1 AND $2 AND error_kind='sandbox_denied') + (SELECT count(*) FROM diagnostic_events WHERE occurred_at BETWEEN $1 AND $2 AND kind='permission_decision' AND reason_code='permission_denied')`, "1"},
		{"obs_assembly_missing_total", `SELECT count(*) FROM runs r WHERE r.started_at BETWEEN $1 AND $2 AND NOT EXISTS(SELECT 1 FROM context_assembly_records a WHERE a.run_id=r.run_id)`, "1"},
	} {
		var value float64
		if err := s.db.pool.QueryRowContext(ctx, scalar.query, args...).Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, obsGauge(scalar.name, scalar.unit, value, nil))
	}
	for _, state := range []string{"pending", "inflight", "retry"} {
		var count float64
		if err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM observability_export_items WHERE state=$1`, state).Scan(&count); err != nil {
			return nil, err
		}
		out = append(out, obsGauge("obs_export_queue_depth", "1", count, map[string]string{"state": state}))
	}
	var unconfirmed, age float64
	if err := s.db.pool.QueryRowContext(ctx, `SELECT count(*) FROM observability_export_items WHERE state='unconfirmed'`).Scan(&unconfirmed); err != nil {
		return nil, err
	}
	if err := s.db.pool.QueryRowContext(ctx, `SELECT GREATEST(COALESCE($1-MIN(created_at),0),0) FROM observability_export_items WHERE state IN ('pending','inflight','retry')`, now.UnixMilli()).Scan(&age); err != nil {
		return nil, err
	}
	out = append(out, obsGauge("obs_export_unconfirmed_items", "1", unconfirmed, nil), obsGauge("obs_export_oldest_pending_age_ms", "ms", age, nil))
	for _, hist := range []struct{ name, query string }{
		{"obs_run_duration_ms", `SELECT status,''::text AS purpose,(finished_at-started_at)::double precision AS value FROM runs WHERE started_at BETWEEN $1 AND $2 AND finished_at IS NOT NULL AND finished_at>=started_at AND status IN ('completed','failed','cancelled','limit_reached')`},
		{"obs_llm_call_duration_ms", `SELECT status,purpose,duration_ms::double precision AS value FROM llm_calls WHERE created_at BETWEEN $1 AND $2 AND status IN ('success','failed') AND duration_ms>=0`},
	} {
		samples, err := s.windowHistogram(ctx, hist.name, hist.query, args)
		if err != nil {
			return nil, err
		}
		out = append(out, samples...)
	}
	for i := range out {
		out[i].Window = window
	}
	return out, nil
}

func (s *ObservabilityStore) windowHistogram(ctx context.Context, name, source string, args []any) ([]observability.MetricSample, error) {
	// source仅来自上方两个固定SQL字面量；所有时间值均是位置参数。
	// source is one of two fixed SQL literals; all times are positional parameters.
	query := `WITH durations AS (` + source + `) SELECT status,purpose,count(*),COALESCE(sum(value),0),
		percentile_cont(0.5) WITHIN GROUP(ORDER BY value),percentile_cont(0.95) WITHIN GROUP(ORDER BY value),percentile_cont(0.99) WITHIN GROUP(ORDER BY value),
		count(*) FILTER(WHERE value<=10),count(*) FILTER(WHERE value<=50),count(*) FILTER(WHERE value<=100),
		count(*) FILTER(WHERE value<=250),count(*) FILTER(WHERE value<=500),count(*) FILTER(WHERE value<=1000),
		count(*) FILTER(WHERE value<=5000),count(*) FILTER(WHERE value<=15000),count(*) FILTER(WHERE value<=30000),count(*) FILTER(WHERE value<=60000)
		FROM durations GROUP BY status,purpose ORDER BY status,purpose`
	type group struct {
		status, purpose string
		count, sum      float64
		quantiles       [3]float64
		buckets         [10]float64
	}
	groups, err := obsRows(ctx, s.db, query, args, func(rows *sql.Rows) (group, error) {
		var g group
		err := rows.Scan(&g.status, &g.purpose, &g.count, &g.sum, &g.quantiles[0], &g.quantiles[1], &g.quantiles[2],
			&g.buckets[0], &g.buckets[1], &g.buckets[2], &g.buckets[3], &g.buckets[4], &g.buckets[5], &g.buckets[6], &g.buckets[7], &g.buckets[8], &g.buckets[9])
		return g, err
	})
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		groups = []group{{}}
	}
	out := make([]observability.MetricSample, 0, len(groups)*16)
	for _, g := range groups {
		labels := map[string]string{}
		if g.status != "" {
			labels["status"] = g.status
		}
		if g.purpose != "" {
			labels["purpose"] = g.purpose
		}
		for i, le := range []string{"10", "50", "100", "250", "500", "1000", "5000", "15000", "30000", "60000", "+Inf"} {
			value := g.count
			if i < len(g.buckets) {
				value = g.buckets[i]
			}
			attrs := maps.Clone(labels)
			attrs["le"] = le
			out = append(out, obsGauge(name+"_bucket", "1", value, attrs))
		}
		out = append(out, obsGauge(name+"_count", "1", g.count, maps.Clone(labels)), obsGauge(name+"_sum", "ms", g.sum, maps.Clone(labels)))
		for i, quantile := range []string{"0.5", "0.95", "0.99"} {
			attrs := maps.Clone(labels)
			attrs["quantile"] = quantile
			sample := obsGauge(name, "ms", g.quantiles[i], attrs)
			if g.count == 0 {
				sample.Available = false
				sample.Reason = "no_data"
			}
			out = append(out, sample)
		}
	}
	return out, nil
}

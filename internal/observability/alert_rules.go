package observability

import (
	"context"
	"errors"
	"time"

	"github.com/ymedword/basalt/internal/diagnostics"
)

var errProcessRestartGap = errors.New("process window includes unavailable pre-restart evidence")

type alertEvidenceReader interface {
	AlertSamples(context.Context, string, time.Duration, time.Time) ([]AlertSample, error)
	AlertEvents(context.Context, time.Time, time.Time) ([]diagnostics.DiagnosticEvent, error)
}

// BuiltinAlertRules 只注册八个已审规则；三条统计候选规则默认关闭。
// BuiltinAlertRules registers the eight approved rules, with statistical candidates disabled.
func BuiltinAlertRules(metrics *MetricsCollector, source alertEvidenceReader) []AlertRule {
	rules := make([]AlertRule, 0, 8)
	for _, id := range metricRules {
		window, hold, enabled := 15*time.Minute, time.Duration(0), true
		switch id {
		case "audit_write_failed", "diagnostic_write_failed":
			window = 5 * time.Minute
		case "export_backlog_stuck":
			hold = 30 * time.Minute
		case "cleanup_failed":
			window = time.Hour
		case "run_failure_burst", "llm_timeout_burst", "tool_denial_burst":
			hold = 15 * time.Minute
			enabled = false
		}
		rule := AlertRule{ID: id, Enabled: enabled, Window: window, For: hold}
		rule.Collect = func(ctx context.Context, now time.Time) ([]AlertSample, error) {
			if id == "audit_write_failed" || id == "diagnostic_write_failed" {
				if metrics == nil {
					return nil, errors.New("process metric source unavailable")
				}
				name := "obs_audit_write_failures_total"
				if id == "diagnostic_write_failed" {
					name = "obs_diagnostic_write_failures_total"
				}
				metrics.mu.Lock()
				last := metrics.lastFailure[name]
				start := metrics.startedAt
				metrics.mu.Unlock()
				var value float64
				if !last.IsZero() && !last.Before(now.Add(-window)) {
					value = 1
				} else if now.Sub(start) < window {
					return nil, errProcessRestartGap
				}
				return []AlertSample{{ScopeKey: "process", Value: value}}, nil
			}
			if source == nil {
				return nil, errors.New("alert database source unavailable")
			}
			if id == "tool_denial_burst" {
				events, err := source.AlertEvents(ctx, now.Add(-window), now)
				if err != nil {
					return nil, err
				}
				var count float64
				for _, event := range events {
					if event.Kind == diagnostics.KindPermissionDecision && event.ReasonCode == diagnostics.ReasonPermissionDenied {
						count++
					}
				}
				if count < 20 {
					if len(events) >= 10001 {
						return nil, errors.New("alert event prefix truncated")
					}
					count = 0
				}
				return []AlertSample{{ScopeKey: "global", Value: count}}, nil
			}
			return source.AlertSamples(ctx, id, window, now)
		}
		rules = append(rules, rule)
	}
	return rules
}

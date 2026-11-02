package observability

import (
	"math"
	"slices"
	"strings"
)

type mappedHistogram struct {
	seen       map[string]bool
	cumulative []uint64
	count      uint64
	sum        float64
	result     string
}

// mapSnapshot 把本地累计桶还原成一个Histogram点，不把窗口Gauge相加。
// mapSnapshot reconstructs one cumulative histogram point and never adds window gauges.
func mapSnapshot(snapshot MetricsSnapshot) ([]MetricData, error) {
	if snapshot.CollectedAt.IsZero() || len(snapshot.Samples) > 10000 {
		return nil, ErrInvalidQuery
	}
	out := []MetricData{}
	histograms := map[string]*mappedHistogram{}
	seen := map[string]bool{}
	for _, s := range snapshot.Samples {
		if !s.Available {
			continue
		}
		key := metricKey(s.Name, s.Labels)
		if seen[key] {
			return nil, ErrInvalidQuery
		}
		seen[key] = true
		if s.Scope == ScopeProcess && s.Type == MetricHistogram {
			result := s.Labels["result"]
			if result != "" && !slices.Contains([]string{"ok", "warning", "partial", "error"}, result) {
				return nil, ErrInvalidQuery
			}
			allowed := map[string][]string{"result": {"ok", "warning", "partial", "error"}}
			if s.Name == "obs_export_batch_duration_ms_bucket" {
				allowed["le"] = durationLE
			}
			if !validMetricLabels(s.Labels, allowed) || math.IsNaN(s.Value) || math.IsInf(s.Value, 0) || s.Value < 0 {
				return nil, ErrInvalidQuery
			}
			h := histograms[result]
			if h == nil {
				h = &mappedHistogram{seen: map[string]bool{}, cumulative: make([]uint64, len(durationLE)), result: result}
				histograms[result] = h
			}
			switch s.Name {
			case "obs_export_batch_duration_ms_sum":
				h.sum = s.Value
				h.seen["sum"] = true
			case "obs_export_batch_duration_ms_count":
				if math.Trunc(s.Value) != s.Value || s.Value >= math.MaxUint64 {
					return nil, ErrInvalidQuery
				}
				h.count = uint64(s.Value)
				h.seen["count"] = true
			case "obs_export_batch_duration_ms_bucket":
				index := slices.Index(durationLE, s.Labels["le"])
				if index < 0 || math.Trunc(s.Value) != s.Value || s.Value >= math.MaxUint64 {
					return nil, ErrInvalidQuery
				}
				h.cumulative[index] = uint64(s.Value)
				h.seen[s.Labels["le"]] = true
			default:
				return nil, ErrInvalidQuery
			}
			continue
		}
		point, err := MapMetric(s, snapshot.CollectedAt)
		if err != nil {
			return nil, err
		}
		if point.Type == MetricCounter {
			if snapshot.startedAt.IsZero() || snapshot.startedAt.After(snapshot.CollectedAt) {
				return nil, ErrInvalidQuery
			}
			point.StartTime = snapshot.startedAt
		}
		out = append(out, point)
	}
	for _, result := range []string{"", "ok", "warning", "partial", "error"} {
		h := histograms[result]
		if h == nil {
			continue
		}
		if len(h.seen) != len(durationLE)+2 || !h.seen["sum"] || !h.seen["count"] || h.cumulative[len(durationLE)-1] != h.count || snapshot.startedAt.IsZero() || snapshot.startedAt.After(snapshot.CollectedAt) {
			return nil, ErrInvalidQuery
		}
		counts := make([]uint64, len(h.cumulative))
		previous := uint64(0)
		for i, cumulative := range h.cumulative {
			if cumulative < previous {
				return nil, ErrInvalidQuery
			}
			counts[i] = cumulative - previous
			previous = cumulative
		}
		attrs := map[string]any{"derived": true}
		if result != "" {
			attrs["result"] = result
		}
		out = append(out, MetricData{Name: "basalt.obs.obs_export_batch_duration_ms", Type: MetricHistogram, Unit: "ms", Time: snapshot.CollectedAt, StartTime: snapshot.startedAt, Count: h.count, Sum: h.sum, Attributes: attrs, BucketBounds: slices.Clone(durationBounds), BucketCounts: counts})
	}
	if len(out) == 0 {
		return nil, ErrInvalidQuery
	}
	slices.SortFunc(out, func(a, b MetricData) int { return strings.Compare(metricPointKey(a), metricPointKey(b)) })
	return out, nil
}

func metricPointKey(point MetricData) string {
	labels := map[string]string{}
	for key, value := range point.Attributes {
		if v, ok := value.(string); ok {
			labels[key] = v
		}
	}
	return metricKey(point.Name, labels)
}

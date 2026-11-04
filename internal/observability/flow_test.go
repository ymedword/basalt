package observability

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlow_MetricsRejectHighCardinalityAndUnknownStatus(t *testing.T) {
	store := &metricTestStore{samples: []MetricSample{
		{Name: "obs_flow_tasks_total", Type: MetricGauge, Value: 1, Available: true, Labels: map[string]string{"status": "blocked"}},
		{Name: "obs_flow_tasks_total", Type: MetricGauge, Value: 2, Available: true, Labels: map[string]string{"status": "CANARY_STATUS"}},
		{Name: "obs_flow_unknown_actions_total", Type: MetricGauge, Value: 3, Available: true, Labels: map[string]string{"task_id": "CANARY_TASK"}},
	}}
	snapshot, err := NewMetricsCollector(store, nil).Snapshot(t.Context(), time.Hour)
	require.NoError(t, err)
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "CANARY")
	found := false
	for _, sample := range snapshot.Samples {
		if sample.Name == "obs_flow_tasks_total" && sample.Labels["status"] == "blocked" {
			require.True(t, sample.Available)
			require.EqualValues(t, 1, sample.Value)
			found = true
		}
	}
	require.True(t, found)
}

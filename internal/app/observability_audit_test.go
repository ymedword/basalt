//go:build integration

package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityProducer_AuditFailureMetricsFailClosed(t *testing.T) {
	h := newRunHarness(t)
	metrics := observability.NewMetricsCollector(nil, nil)
	h.db.ObserveAuditWriteFailures(func() { metrics.IncCounter("obs_audit_write_failures_total", nil) })
	_, err := h.db.ExecContext(t.Context(), `CREATE FUNCTION obs_fail_run_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit write failure'; END $$`)
	require.NoError(t, err)
	_, err = h.db.ExecContext(t.Context(), `CREATE TRIGGER obs_fail_run_audit BEFORE INSERT ON llm_calls FOR EACH ROW EXECUTE FUNCTION obs_fail_run_audit()`)
	require.NoError(t, err)
	result, err := h.invoker.Run(t.Context(), h.requestFor(runHarnessCaller))
	require.NoError(t, err)
	require.Equal(t, core.StatusFailed, result.Status)
	require.Zero(t, h.provider.calls(), "audit pre-write failure must still stop the real provider boundary")
	snapshot, err := metrics.Snapshot(t.Context(), 5*time.Minute)
	require.NoError(t, err)
	var failures float64
	for _, sample := range snapshot.Samples {
		if sample.Name == "obs_audit_write_failures_total" {
			failures = sample.Value
		}
	}
	require.Equal(t, float64(1), failures)
}

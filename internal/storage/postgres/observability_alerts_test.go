//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityStore_AlertPersistence(t *testing.T) {
	db := portTestDB(t)
	store := NewObservabilityStore(db)
	now := time.UnixMilli(1700000000000).UTC()
	value := float64(1)
	collectErr := error(nil)
	rule := observability.AlertRule{ID: "run_failure_burst", Enabled: true, For: time.Minute, Collect: func(context.Context, time.Time) ([]observability.AlertSample, error) {
		return []observability.AlertSample{{ScopeKey: "agent-a", Value: value}}, collectErr
	}}
	e := observability.NewAlertEngine(store, []observability.AlertRule{rule}, nil)
	states, err := e.Evaluate(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, observability.StatePending, states[0].State)
	states, err = e.Evaluate(t.Context(), now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, observability.StateFiring, states[0].State)
	collectErr = errors.New("CANARY_SOURCE_FAILURE")
	states, err = e.Evaluate(t.Context(), now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, observability.StateUnknown, states[0].State)
	loaded, err := NewObservabilityStore(db).LoadStates(t.Context())
	require.NoError(t, err)
	require.Equal(t, observability.StateFiring, loaded["run_failure_burst/agent-a"].PreviousState)
	collectErr = nil
	e = observability.NewAlertEngine(NewObservabilityStore(db), []observability.AlertRule{rule}, nil)
	states, err = e.Evaluate(t.Context(), now.Add(3*time.Minute))
	require.NoError(t, err)
	require.Equal(t, observability.StateFiring, states[0].State)
	require.Equal(t, now.Add(time.Minute), states[0].Since, "unknown must not reset the firing start")
	value = 0
	states, err = e.Evaluate(t.Context(), now.Add(4*time.Minute))
	require.NoError(t, err)
	require.Equal(t, "recovery_pending", states[0].Reason)
	e = observability.NewAlertEngine(NewObservabilityStore(db), []observability.AlertRule{rule}, nil)
	states, err = e.Evaluate(t.Context(), now.Add(5*time.Minute))
	require.NoError(t, err)
	require.Equal(t, observability.StateInactive, states[0].State)
	_, err = e.Evaluate(t.Context(), now.Add(6*time.Minute))
	require.NoError(t, err)
	var resolved int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_alert_transitions WHERE to_state='resolved'`).Scan(&resolved))
	require.Equal(t, 1, resolved)
	_, err = db.pool.ExecContext(t.Context(), `CREATE FUNCTION obs_reject_alert_transition() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic transition failure'; END $$`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `CREATE TRIGGER obs_reject_alert BEFORE INSERT ON observability_alert_transitions FOR EACH ROW EXECUTE FUNCTION obs_reject_alert_transition()`)
	require.NoError(t, err)
	value = 1
	states, err = e.Evaluate(t.Context(), now.Add(7*time.Minute))
	require.Error(t, err)
	require.Equal(t, observability.StateUnknown, states[0].State)
	loaded, err = store.LoadStates(t.Context())
	require.NoError(t, err)
	require.Equal(t, observability.StateInactive, loaded["run_failure_burst/agent-a"].State, "transition failure must roll back the current-state write")
}

func TestObservabilityStore_AlertToolDenialSource(t *testing.T) {
	db := portTestDB(t)
	now := time.UnixMilli(1700000000000).UTC()
	recorder := diagnostics.NewRecorder(diagnostics.RecorderOptions{Store: NewDiagnosticStore(db), Now: func() time.Time { return now }})
	for range 25 {
		recorder.Record(t.Context(), diagnostics.DiagnosticEvent{Kind: diagnostics.KindRetrieval, Correlation: diagnostics.Correlation{RunID: "synthetic"}, ReasonCode: diagnostics.ReasonPermissionDenied, Attrs: map[string]any{"count": 500}})
	}
	for range 19 {
		recorder.Record(t.Context(), diagnostics.DiagnosticEvent{Kind: diagnostics.KindPermissionDecision, Correlation: diagnostics.Correlation{RunID: "synthetic"}, ReasonCode: diagnostics.ReasonPermissionDenied})
	}
	s := NewObservabilityStore(db)
	samples, err := s.AlertSamples(t.Context(), "tool_denial_burst", 15*time.Minute, now)
	require.NoError(t, err)
	require.Zero(t, samples[0].Value)
	recorder.Record(t.Context(), diagnostics.DiagnosticEvent{Kind: diagnostics.KindPermissionDecision, Correlation: diagnostics.Correlation{RunID: "synthetic"}, ReasonCode: diagnostics.ReasonPermissionDenied})
	samples, err = s.AlertSamples(t.Context(), "tool_denial_burst", 15*time.Minute, now)
	require.NoError(t, err)
	require.Equal(t, float64(20), samples[0].Value)
	rules := observability.BuiltinAlertRules(nil, s)
	var selected observability.AlertRule
	for _, rule := range rules {
		if rule.ID == "tool_denial_burst" {
			selected = rule
		}
	}
	selected.Enabled, selected.For = true, 0
	states, err := observability.NewAlertEngine(s, []observability.AlertRule{selected}, nil).Evaluate(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, observability.StateFiring, states[0].State)
	require.Equal(t, float64(20), states[0].Value)
}

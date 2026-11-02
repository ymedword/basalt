package observability

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/diagnostics"
)

type testAlertEvidence struct{ events []diagnostics.DiagnosticEvent }

func (s *testAlertEvidence) AlertSamples(context.Context, string, time.Duration, time.Time) ([]AlertSample, error) {
	return []AlertSample{{ScopeKey: "global", Value: 0}}, nil
}

func (s *testAlertEvidence) AlertEvents(context.Context, time.Time, time.Time) ([]diagnostics.DiagnosticEvent, error) {
	return s.events, nil
}

func alertNamed(t *testing.T, states []AlertStateView, id string) AlertStateView {
	t.Helper()
	for _, state := range states {
		if state.RuleID == id {
			return state
		}
	}
	t.Fatalf("missing alert %s", id)
	return AlertStateView{}
}

func TestObservability_AlertAuditEvidence(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	clock := func() time.Time { return now }
	c := NewMetricsCollector(nil, clock)
	store := &testAlertStore{}
	e := NewAlertEngine(store, BuiltinAlertRules(c, nil), clock)
	states, err := e.Evaluate(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, StateUnknown, alertNamed(t, states, "audit_write_failed").State, "pre-restart evidence must not be invented as zero")
	c.IncCounter("obs_audit_write_failures_total", nil)
	states, err = e.Evaluate(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, StateFiring, alertNamed(t, states, "audit_write_failed").State)
	now = now.Add(time.Minute)
	c = NewMetricsCollector(nil, clock)
	e = NewAlertEngine(store, BuiltinAlertRules(c, nil), clock)
	states, err = e.Evaluate(t.Context(), now)
	require.NoError(t, err)
	audit := alertNamed(t, states, "audit_write_failed")
	require.Equal(t, StateUnknown, audit.State)
	require.Equal(t, "process_restart_gap", audit.Reason)
	store.saveErr = errors.New("CANARY_DB_UNWRITABLE")
	c.IncCounter("obs_audit_write_failures_total", nil)
	states, err = e.Evaluate(t.Context(), now)
	require.Error(t, err)
	require.Equal(t, StateUnknown, alertNamed(t, states, "audit_write_failed").State)
}

func TestObservability_AlertToolDenialExcludesRetrieval(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	source := &testAlertEvidence{}
	for range 25 {
		source.events = append(source.events, diagnostics.DiagnosticEvent{Kind: diagnostics.KindRetrieval, ReasonCode: diagnostics.ReasonPermissionDenied, Attrs: map[string]any{"count": 500}})
	}
	for range 19 {
		source.events = append(source.events, diagnostics.DiagnosticEvent{Kind: diagnostics.KindPermissionDecision, ReasonCode: diagnostics.ReasonPermissionDenied})
	}
	rules := BuiltinAlertRules(nil, source)
	var selected AlertRule
	for _, rule := range rules {
		if rule.ID == "tool_denial_burst" {
			selected = rule
		}
	}
	selected.Enabled, selected.For = true, 0
	e := NewAlertEngine(&testAlertStore{}, []AlertRule{selected}, nil)
	states, err := e.Evaluate(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, StateInactive, states[0].State)
	source.events = append(source.events, diagnostics.DiagnosticEvent{Kind: diagnostics.KindPermissionDecision, ReasonCode: diagnostics.ReasonPermissionDenied})
	states, err = e.Evaluate(t.Context(), now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, StateFiring, states[0].State)
	require.Equal(t, float64(20), states[0].Value)
}

type testAlertStore struct {
	states           map[string]AlertStateView
	history          []AlertStateView
	loadErr, saveErr error
}

func (s *testAlertStore) LoadStates(context.Context) (map[string]AlertStateView, error) {
	return maps.Clone(s.states), s.loadErr
}

func (s *testAlertStore) SaveState(_ context.Context, next AlertStateView) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.states == nil {
		s.states = map[string]AlertStateView{}
	}
	s.states[next.RuleID+"/"+next.ScopeKey] = next
	return nil
}

func (s *testAlertStore) SaveTransition(_ context.Context, _, next AlertStateView, _ time.Time) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.history = append(s.history, next)
	return nil
}

func (s *testAlertStore) ApplyAlertEvaluation(ctx context.Context, from, to AlertStateView, at time.Time) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	if alertChanged(from, to) {
		transition := to
		if alertResolved(from, to) {
			transition.State = StateResolved
		}
		s.history = append(s.history, transition)
	}
	return s.SaveState(ctx, to)
}

func TestObservability_AlertTriggerRecovery(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	value := float64(1)
	store := &testAlertStore{}
	rule := AlertRule{ID: "run_failure_burst", Enabled: true, For: 2 * time.Minute, Collect: func(context.Context, time.Time) ([]AlertSample, error) {
		return []AlertSample{{ScopeKey: "agent-a", Value: value}}, nil
	}}
	e := NewAlertEngine(store, []AlertRule{rule}, nil)
	states, err := e.Evaluate(t.Context(), now)
	require.NoError(t, err)
	require.Equal(t, StatePending, states[0].State)
	states, err = e.Evaluate(t.Context(), now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, StatePending, states[0].State)
	states, err = e.Evaluate(t.Context(), now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, StateFiring, states[0].State)
	value = 0
	states, err = e.Evaluate(t.Context(), now.Add(3*time.Minute))
	require.NoError(t, err)
	require.Equal(t, StateFiring, states[0].State)
	require.Equal(t, "recovery_pending", states[0].Reason)
	// 重建引擎模拟进程重启，恢复计时必须由存储保留。
	e = NewAlertEngine(store, []AlertRule{rule}, nil)
	states, err = e.Evaluate(t.Context(), now.Add(5*time.Minute))
	require.NoError(t, err)
	require.Equal(t, StateInactive, states[0].State)
	require.Equal(t, StateResolved, store.history[len(store.history)-1].State)
	states, err = e.Evaluate(t.Context(), now.Add(6*time.Minute))
	require.NoError(t, err)
	require.Equal(t, StateInactive, states[0].State)
	resolved := 0
	for _, transition := range store.history {
		if transition.State == StateResolved {
			resolved++
		}
	}
	require.Equal(t, 1, resolved)
}

func TestObservability_AlertUnknownOnStoreFailure(t *testing.T) {
	now := time.UnixMilli(1700000000000).UTC()
	collectErr := error(nil)
	rule := AlertRule{ID: "export_backlog_stuck", Enabled: true, For: time.Minute, Collect: func(context.Context, time.Time) ([]AlertSample, error) {
		return []AlertSample{{ScopeKey: "export", Value: 1}}, collectErr
	}}
	store := &testAlertStore{loadErr: errors.New("CANARY_STORE_ERROR")}
	e := NewAlertEngine(store, []AlertRule{rule}, nil)
	states, err := e.Evaluate(t.Context(), now)
	require.Error(t, err)
	require.Len(t, states, 1)
	require.Equal(t, StateUnknown, states[0].State)
	require.NotContains(t, states[0].Reason, "CANARY")
	store.loadErr = nil
	_, err = e.Evaluate(t.Context(), now)
	require.NoError(t, err)
	states, err = e.Evaluate(t.Context(), now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, StateFiring, states[0].State)
	collectErr = errors.New("source failed")
	states, err = e.Evaluate(t.Context(), now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, StateUnknown, states[0].State)
	collectErr = nil
	e = NewAlertEngine(store, []AlertRule{rule}, nil)
	states, err = e.Evaluate(t.Context(), now.Add(3*time.Minute))
	require.NoError(t, err)
	require.Equal(t, StateFiring, states[0].State, "unknown must preserve a previously firing alert across restart")
	store.saveErr = errors.New("write failed")
	states, err = e.Evaluate(t.Context(), now.Add(4*time.Minute))
	require.Error(t, err)
	require.Equal(t, StateUnknown, states[0].State)
}

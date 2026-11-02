package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/historyretention"
	"github.com/ymedword/basalt/internal/observability"
)

func TestObservability_APIContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	s, _ := newObservationServer(t)
	paths := doc["paths"].(map[string]any)
	for _, name := range []string{"overview", "timeline", "context", "metrics", "alerts", "export", "cleanup"} {
		p := paths["/observability/"+name].(map[string]any)
		require.Len(t, p, 1, "观测契约只允许GET")
		get := p["get"].(map[string]any)
		raw, err := json.Marshal(get["security"])
		require.NoError(t, err)
		require.NotContains(t, string(raw), "callerKey")
		require.Contains(t, string(raw), "bearerAuth")
		require.Contains(t, string(raw), "localTrust")
		found := false
		for _, r := range s.routeTable() {
			if r.pattern == "/api/v1/observability/"+name {
				found = true
				require.Equal(t, http.MethodGet, r.method)
				require.NotNil(t, r.adminHandler)
				require.Empty(t, r.scope)
				require.Empty(t, r.ingress)
			}
		}
		require.True(t, found)
	}
	now := time.UnixMilli(1700000000000).UTC()
	metrics, err := observability.NewMetricsCollector(nil, func() time.Time { return now }).Snapshot(t.Context(), 15*time.Minute)
	require.NoError(t, err)
	views := map[string]any{
		"ObsOverview": observability.Overview{Groups: []observability.TaskGroup{}},
		"ObsTimeline": observability.Timeline{Anchor: observability.Anchor{Kind: observability.AnchorRun, Value: "real"}, Entries: []observability.TimelineEntry{{DisplayStepID: "S1", Source: observability.SourceRun, SourceID: "real", Kind: "run", OccurredAt: now, EndSource: observability.EndTimeUnknown, Evidence: observability.EvidenceDirect, Attrs: map[string]any{}}}, Gaps: []observability.EvidenceGap{}, OrderScope: "anchor-local", ProjectedAt: now},
		"ObsContext":  observability.ContextDiagnosis{RunID: "real", Rounds: []observability.RoundView{}, Other: []observability.AssemblyView{}, Gaps: []observability.EvidenceGap{}, Grade: observability.EvidenceInsufficient},
		"ObsMetrics":  metrics,
		"ObsAlerts":   observability.AlertStatus{Rules: []observability.AlertRuleStatus{{RuleID: "audit_write_failed", WindowSeconds: 300, States: []observability.AlertStateView{observability.InitialAlertState("audit_write_failed", true)}}}, Transitions: []observability.AlertTransition{}},
		"ObsExport":   observability.ExportStatus{Protocol: "http/protobuf", Sources: []observability.SourceCoverage{{Source: observability.SourceDiagnostic, CoverageKind: "occurred_at"}}},
		"ObsCleanup":  observability.CleanupStatus{HistoryDays: 30, History: []historyretention.Report{}, Files: observability.FileRetentionView{Days: 30, LogFileMiB: 32, LogTotalMiB: 256, MinVerifiedBackups: 2, Status: "not_run"}, Policies: []observability.RetentionPolicyView{}, Runs: []observability.CleanupRun{}},
	}
	for name, view := range views {
		t.Run(name, func(t *testing.T) { requireViewMatchesSchema(t, doc, name, view) })
	}
}

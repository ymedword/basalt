//go:build integration

package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/observability"
)

func TestPhaseTiming_ProductionWiringThroughPostgresProjection(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			t.Error("stream must remain disabled")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"CANARY_PHASE_RESPONSE"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer srv.Close()
	f := newWireFixture(t)
	f.cfg.Providers[0].BaseURL = srv.URL
	f.cfg.Observability = cli.DefaultObservabilityConfig()
	configPath := filepath.Join(f.workDir, configFileName)
	content, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte(strings.ReplaceAll(string(content), "http://127.0.0.1:1", srv.URL)), 0o600))
	f.writeAgent(t, "phase-probe")
	rt := f.assemble(t)
	result, err := rt.runService().Run(t.Context(), core.RunRequest{Principal: core.LocalAdminPrincipal(), Agent: "phase-probe", SessionID: "phase-session", Message: "CANARY_PHASE_QUERY", Channel: "cli"})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, result.Status, "reason=%s", result.Reason)
	require.Equal(t, int32(1), calls.Load())
	view, err := rt.observability.Timeline(t.Context(), observability.TimelineQuery{Anchor: observability.Anchor{Kind: observability.AnchorRun, Value: result.RunID}, Limit: 1000})
	require.NoError(t, err)
	stages := map[string]observability.TimelineEntry{}
	for _, e := range view.Entries {
		if name, ok := e.Attrs["phase"].(string); ok {
			stages[name] = e
		}
	}
	for _, name := range []string{core.PhaseKnowledge, core.PhaseContext, core.PhaseAssembly, core.PhaseFirstResponse, core.PhaseResponseRead} {
		stage, ok := stages[name]
		require.True(t, ok, "production wiring omitted %s", name)
		require.NotNil(t, stage.EndedAt)
		require.Equal(t, observability.EndTimeRecorded, stage.EndSource)
	}
	require.True(t, strings.HasPrefix(stages[core.PhaseFirstResponse].AuditRef, "llm_calls:"))
	bytes, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(bytes), "CANARY_PHASE_QUERY")
	require.NotContains(t, string(bytes), "CANARY_PHASE_RESPONSE")
}

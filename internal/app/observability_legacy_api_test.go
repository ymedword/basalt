//go:build integration

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/web"
)

func TestObservability_APILegacyEnumsAfterV5Upgrade(t *testing.T) {
	f := newWireFixture(t)
	legacy, err := sql.Open("pgx", f.dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, legacy.Close()) })
	files, err := filepath.Glob("../storage/postgres/migrations/000[1-5]_*.sql")
	require.NoError(t, err)
	require.Len(t, files, 5)
	for i, path := range files {
		ddl, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		tx, beginErr := legacy.BeginTx(t.Context(), nil)
		require.NoError(t, beginErr)
		_, err = tx.ExecContext(t.Context(), string(ddl))
		require.NoError(t, err)
		_, err = tx.ExecContext(t.Context(), `INSERT INTO schema_version(version,applied_at,description) VALUES($1,$2,$3)`, i+1, time.Now().UnixMilli(), "migrations/"+filepath.Base(path))
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	id, err := diagnostics.NewEventID(now)
	require.NoError(t, err)
	_, err = legacy.ExecContext(t.Context(), `INSERT INTO diagnostic_events(event_id,schema_version,kind,reason_code,decision,occurred_at,run_id,attrs) VALUES($1,0,'CANARY_OBS_KIND','CANARY_OBS_REASON','CANARY_OBS_DECISION',$2,'legacy-run','{}')`, id, now.UnixMilli())
	require.NoError(t, err)
	var version int
	require.NoError(t, legacy.QueryRowContext(t.Context(), `SELECT max(version) FROM schema_version`).Scan(&version))
	require.Equal(t, 5, version)
	f.cfg.Observability = cli.DefaultObservabilityConfig()
	disabled := false
	f.cfg.Observability.Alerts.Enabled = &disabled
	rt := f.assemble(t)
	require.NoError(t, legacy.QueryRowContext(t.Context(), `SELECT max(version) FROM schema_version`).Scan(&version))
	require.Equal(t, 7, version)
	var original string
	require.NoError(t, legacy.QueryRowContext(t.Context(), `SELECT kind||reason_code||decision FROM diagnostic_events WHERE event_id=$1`, id).Scan(&original))
	require.Contains(t, original, "CANARY_OBS_KIND")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	opts := f.app().webOptions(rt)
	opts.Listener = ln
	opts.Addr = addr
	opts.Logger = slog.New(slog.DiscardHandler)
	opts.AdminAuth, err = web.NewAuthGuard(web.AdminAuthConfig{Mode: "token", Token: "synthetic-legacy-admin", AllowedHosts: []string{addr}}, opts.Logger, time.Now)
	require.NoError(t, err)
	opts.Ingress, err = newIngressLimiter(f.cfg, rt.db, time.Now)
	require.NoError(t, err)
	server, err := web.NewServer(opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("legacy API shutdown timed out")
		}
	})
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/api/v1/observability/timeline?run_id=legacy-run", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer synthetic-legacy-admin")
	response, err := client.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, 200, response.StatusCode)
	require.NotContains(t, string(body), "CANARY")
	var envelope struct {
		Data observability.Timeline `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &envelope))
	require.Len(t, envelope.Data.Entries, 1)
	entry := envelope.Data.Entries[0]
	require.Equal(t, id, entry.EventID)
	require.Equal(t, "unknown", entry.Kind)
	require.Equal(t, "unclassified", entry.ReasonCode)
	require.Empty(t, entry.Decision)
	require.Equal(t, 3, entry.RedactedFields)
	var after string
	require.NoError(t, legacy.QueryRowContext(t.Context(), `SELECT kind||reason_code||decision FROM diagnostic_events WHERE event_id=$1`, id).Scan(&after))
	require.Equal(t, original, after, "GET never rewrites the legacy authority")
}

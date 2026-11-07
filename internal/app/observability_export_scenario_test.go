//go:build integration

package app

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

func TestScenario_ExportFailureRecovery(t *testing.T) {
	for _, c := range []struct {
		name                    string
		retry, partial, loseAck bool
	}{{name: "A_503_then_success", retry: true}, {name: "B_partial_no_retry", partial: true}, {name: "C_plain_success"}, {name: "D_partial_ack_transaction_lost", partial: true, loseAck: true}, {name: "E_sent_ack_transaction_lost", loseAck: true}} {
		t.Run(c.name, func(t *testing.T) {
			var requests atomic.Int32
			var mu sync.Mutex
			var payloads [][]byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || r.URL.Path != "/v1/traces" {
					w.WriteHeader(400)
					return
				}
				mu.Lock()
				payloads = append(payloads, body)
				mu.Unlock()
				n := requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if c.retry && n == 1 {
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"message":"CANARY_RECEIVER_ERROR"}`))
					return
				}
				if c.partial {
					_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedSpans":1}}`))
					return
				}
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			h := newRunHarness(t)
			now := time.Now().UTC().Truncate(time.Millisecond)
			cfg := cli.DefaultObservabilityConfig()
			cfg.Export.Enabled = true
			cfg.Export.Endpoint = server.URL
			cfg.Export.Protocol = "http/json"
			cfg.Export.BatchSize = 3
			o, err := NewObservability(t.Context(), cfg, Deps{Store: postgres.NewObservabilityStore(h.db), Clock: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler)})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, o.Close()) })
			for _, id := range []string{"source-a", "source-b", "source-c"} {
				require.NoError(t, startRunTest(postgres.NewRunStore(h.db), t.Context(), core.RunRecord{RunID: id, Agent: "synthetic", Channel: "cli", Principal: core.LocalAdminCLIPrincipal(), StartedAt: now.Add(-10 * time.Minute)}, core.RunResourceManifest{}))
				_, err = h.db.ExecContext(t.Context(), `UPDATE runs SET status='completed',finished_at=$1 WHERE run_id=$2`, now.Add(-3*time.Minute).UnixMilli(), id)
				require.NoError(t, err)
			}
			business := scenarioBusinessFingerprint(t, h.db)
			if c.loseAck {
				_, err = h.db.ExecContext(t.Context(), `CREATE FUNCTION obs_scenario_reject_ack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.outcome <> 'unknown' THEN RAISE EXCEPTION 'synthetic confirmation transaction lost'; END IF; RETURN NEW; END $$;
				CREATE TRIGGER obs_scenario_reject_ack BEFORE UPDATE ON observability_export_batches FOR EACH ROW EXECUTE FUNCTION obs_scenario_reject_ack()`)
				require.NoError(t, err)
			}
			_, err = o.Export.ExportOnce(t.Context())
			if c.loseAck {
				require.Error(t, err, "receiver response exists but confirmation must roll back")
				status, readErr := o.ExportStatus(t.Context())
				require.NoError(t, readErr)
				require.Equal(t, int64(1), status.UnknownAttempts)
				require.Nil(t, status.LastAccepted)
				require.Nil(t, status.LastRejected)
				require.Equal(t, int64(3), status.Inflight)
				_, err = h.db.ExecContext(t.Context(), `DROP TRIGGER obs_scenario_reject_ack ON observability_export_batches`)
				require.NoError(t, err)
			} else {
				require.NoError(t, err)
			}
			if c.retry || c.loseAck {
				now = now.Add(10 * time.Minute)
				_, err = o.Export.ExportOnce(t.Context())
				require.NoError(t, err)
			}
			status, err := o.ExportStatus(t.Context())
			require.NoError(t, err)
			if c.partial {
				require.Equal(t, int64(3), status.Unconfirmed)
				require.NotNil(t, status.LastAccepted)
				require.NotNil(t, status.LastRejected)
				require.Equal(t, 2, *status.LastAccepted)
				require.Equal(t, 1, *status.LastRejected)
			} else {
				require.NotNil(t, status.LastAccepted)
				require.Equal(t, 3, *status.LastAccepted)
				require.Zero(t, *status.LastRejected)
				var sent int
				require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_items WHERE state='sent'`).Scan(&sent))
				require.Equal(t, 3, sent)
			}
			require.Zero(t, status.Pending)
			require.Zero(t, status.Retry)
			require.Zero(t, status.Inflight)
			require.Zero(t, status.FailedPermanent, "partial or lost confirmation is not a proven permanent loss")
			if c.loseAck {
				require.Equal(t, int64(1), status.UnknownAttempts)
			}
			want := 1
			if c.retry || c.loseAck {
				want = 2
			}
			require.Equal(t, int32(want), requests.Load())
			var batches, minAttempts, maxAttempts int
			require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM observability_export_batches`).Scan(&batches))
			require.Equal(t, want, batches)
			require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT min(attempts),max(attempts) FROM observability_export_items`).Scan(&minAttempts, &maxAttempts))
			require.Equal(t, want, minAttempts)
			require.Equal(t, want, maxAttempts)
			now = now.Add(10 * time.Minute)
			_, err = o.Export.ExportOnce(t.Context())
			require.NoError(t, err)
			require.Equal(t, int32(want), requests.Load(), "confirmed partial results never auto-retry")
			mu.Lock()
			received := slices.Clone(payloads)
			mu.Unlock()
			require.Len(t, received, want)
			ids := scenarioSpanIDs(t, received[0])
			require.Len(t, ids, 3)
			if want == 2 {
				require.Equal(t, ids, scenarioSpanIDs(t, received[1]), "unknown attempts may duplicate the same stable spans")
			}
			require.Equal(t, business, scenarioBusinessFingerprint(t, h.db), "export faults cannot execute, replay, recover, or rewrite business/audit facts")
			require.Zero(t, h.provider.calls())
		})
	}
}

func scenarioSpanIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var payload struct {
		Resources []struct {
			Scopes []struct {
				Spans []struct {
					TraceID string `json:"traceId"`
					SpanID  string `json:"spanId"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	ids := []string{}
	for _, r := range payload.Resources {
		for _, s := range r.Scopes {
			for _, span := range s.Spans {
				require.Len(t, span.TraceID, 32)
				require.Len(t, span.SpanID, 16)
				ids = append(ids, span.TraceID+"/"+span.SpanID)
			}
		}
	}
	slices.Sort(ids)
	return ids
}

func scenarioBusinessFingerprint(t *testing.T, db *postgres.DB) string {
	t.Helper()
	var out string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT md5(COALESCE(string_agg(v,',' ORDER BY v),'')) FROM (
	SELECT row_to_json(r)::text v FROM runs r UNION ALL SELECT row_to_json(r)::text FROM llm_calls r
	UNION ALL SELECT row_to_json(r)::text FROM tool_invocations r UNION ALL SELECT row_to_json(r)::text FROM messages r
	UNION ALL SELECT row_to_json(r)::text FROM management_operations r UNION ALL SELECT row_to_json(r)::text FROM task_executions r
	UNION ALL SELECT row_to_json(r)::text FROM kb_ingest_jobs r UNION ALL SELECT row_to_json(r)::text FROM embedding_calls r
	UNION ALL SELECT row_to_json(r)::text FROM context_assembly_records r UNION ALL SELECT row_to_json(r)::text FROM diagnostic_events r)r`).Scan(&out))
	return out
}

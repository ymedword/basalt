//go:build integration

package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/observability"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/web"
)

func TestObservability_APINoCanary(t *testing.T) {
	// 隔离PG中的真实原始列→生产Reader/Projector→真实管理认证/HTTP；不能用空替身证明脱敏。
	// Real raw columns in isolated PG flow through production reads and authenticated HTTP.
	f := newWireFixture(t)
	f.cfg.Observability = cli.DefaultObservabilityConfig()
	alerts := false
	f.cfg.Observability.Alerts.Enabled = &alerts
	f.cfg.Observability.Export.Endpoint = "http://127.0.0.1:1/CANARY_ENDPOINT"
	f.cfg.Observability.Export.Headers = "X-OBS=CANARY_HEADER"
	rt := f.assemble(t)
	now := time.Now().UTC()
	require.NoError(t, postgres.NewRunStore(rt.db).StartRun(t.Context(), core.RunRecord{RunID: "obs-api-real", RequestID: "obs-api-request", Agent: "synthetic", Channel: "cli", Principal: core.LocalAdminCLIPrincipal(), StartedAt: now}, core.RunResourceManifest{}))
	for _, stmt := range []string{
		`UPDATE runs SET status='failed',reason='password=CANARY_RUN',user_id='CANARY_USER',finished_at=started_at+1 WHERE run_id='obs-api-real'`,
		`INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at,error_kind,error_message) VALUES('obs-api-real',1,'fake','fake',12,'failed',1,'timeout','CANARY_LLM_ERROR')`,
		`INSERT INTO tool_invocations(run_id,tool_name,tool_call_id,status,started_at,finished_at,error_message) VALUES('obs-api-real','fs.read','real-call','failed',1,12,'CANARY_TOOL_ERROR')`,
		`INSERT INTO sessions(session_id,profile_name,channel,user_id,created_at,last_active_at) VALUES('obs-api-session','synthetic','cli','CANARY_SESSION_USER',1,1)`,
		`INSERT INTO messages(session_id,run_id,seq,role,content,created_at) VALUES('obs-api-session','obs-api-real',1,'user','CANARY_MESSAGE_BODY',1)`,
		`INSERT INTO kb_collections(collection_id,name,project,created_at) VALUES('obs-api-col','synthetic','',1)`,
		`INSERT INTO kb_documents(document_id,collection_id,source_path,content_hash,created_at,updated_at) VALUES('obs-api-doc','obs-api-col','CANARY_DOCUMENT_TITLE.md','hash',1,1)`,
		`INSERT INTO kb_document_versions(version_id,document_id,content_hash,status,parser_version,chunker_version,source_text,created_at,updated_at) VALUES('obs-api-ver','obs-api-doc','hash','pending','text-v1','chunk-v1','CANARY_DOCUMENT_BODY',1,1)`,
		`INSERT INTO kb_ingest_jobs(ingest_job_id,document_id,version_id,stage,attempt,status,error_kind,started_at,finished_at) VALUES('obs-api-ingest','obs-api-doc','obs-api-ver','embed',1,'failed','verify_failed',1,2)`,
		`INSERT INTO embedding_calls(ingest_job_id,model,dimensions,input_count,status,error_kind,created_at,finished_at) VALUES('obs-api-ingest','synthetic',512,1,'failed','password=CANARY_EMBEDDING_ERROR',1,2)`,
		`INSERT INTO scheduled_tasks(task_id,agent_name,cron,zone,message,definition_hash,created_at,updated_at,caller_id,authorizing_kind) VALUES('obs-api-task','synthetic','0 * * * * *','UTC','CANARY_TASK_BODY','h1',1,1,'local-admin','admin-builtin')`,
		`INSERT INTO task_executions(task_id,run_id,status,trigger,error_message,started_at,finished_at) VALUES('obs-api-task','obs-api-real','failed','cron','CANARY_TASK_ERROR',1,2)`,
		`INSERT INTO context_assembly_records(record_id,run_id,step_id,purpose,agent,prompt_version,model_version,estimator_version,budget,candidates,hits,retrieval,created_at) VALUES('obs-api-retrieval','obs-api-real','retrieval','retrieval','synthetic','v1','v1','v1',NULL,'[{"chunk_id":"allowed","acl_decision":"allow","decision":"kept","title":"CANARY_ALLOWED_TITLE","content":"CANARY_ALLOWED_BODY"},{"chunk_id":"CANARY_FILTERED_ID","acl_decision":"deny","decision":"kept"}]','["allowed","CANARY_FILTERED_ID"]','{"filtered_count":1,"filtered_reason":"CANARY_FILTER_REASON"}',1)`,
	} {
		_, err := rt.db.ExecContext(t.Context(), stmt)
		require.NoError(t, err)
	}
	require.NoError(t, postgres.NewManagementStore(rt.db).BeginOperation(t.Context(), core.OperationRecord{OperationID: "obs-api-op", Action: "system.baseline_v2", TargetKind: "system", TargetID: "system", ActorSubject: core.ActorSubjectMigration, AuthMethod: core.AuthMethodMigration, Source: "migration", CreatedAt: now}))
	_, err := rt.db.ExecContext(t.Context(), `UPDATE management_operations SET detail_json='{"password":"CANARY_DETAIL_JSON"}',error_message='CANARY_OPERATION_ERROR' WHERE operation_id='obs-api-op'`)
	require.NoError(t, err)
	eventID, err := diagnostics.NewEventID(now)
	require.NoError(t, err)
	_, err = rt.db.ExecContext(t.Context(), `INSERT INTO diagnostic_events(event_id,schema_version,kind,occurred_at,run_id,attrs) VALUES($1,0,'tool_call',$2,'obs-api-real','{"tool":"fs.read","error_kind":"bearer=CANARY_ATTR","password":"CANARY_PASSWORD","unknown":"CANARY_UNKNOWN","url":"https://example.invalid/private/CANARY_PATH?token=CANARY_QUERY"}')`, eventID, now.UnixMilli())
	require.NoError(t, err)
	var raw string
	require.NoError(t, rt.db.QueryRowContext(t.Context(), `SELECT error_message FROM llm_calls WHERE run_id='obs-api-real'`).Scan(&raw))
	require.Contains(t, raw, "CANARY_LLM_ERROR")
	_, err = rt.db.ExecContext(t.Context(), `INSERT INTO observability_export_batches(batch_id,signal,record_unit,item_count,record_count,outcome,error_kind,attempted_at) VALUES('legacy-read','trace','span',1,1,'permanent','password=CANARY_BATCH_ERROR',$1)`, now.UnixMilli())
	require.NoError(t, err)
	_, err = rt.db.ExecContext(t.Context(), `INSERT INTO observability_cleanup_runs(cleanup_id,table_kind,window_start,window_end,status,error_kind,started_at,finished_at) VALUES('legacy-read','diagnostic_events',$1,$2,'failed','bearer=CANARY_CLEANUP_ERROR',$3,$3)`, now.Add(-8*24*time.Hour).UnixMilli(), now.Add(-7*24*time.Hour).UnixMilli(), now.UnixMilli())
	require.NoError(t, err)
	before := observationReadFingerprint(t, rt.db)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	opts := f.app().webOptions(rt)
	opts.Listener = ln
	opts.Addr = addr
	opts.Logger = slog.New(slog.DiscardHandler)
	opts.Now = func() time.Time { return now.Add(time.Second) }
	opts.AdminAuth, err = web.NewAuthGuard(web.AdminAuthConfig{Mode: "token", Token: "synthetic-test-admin", AllowedHosts: []string{addr}}, opts.Logger, time.Now)
	require.NoError(t, err)
	opts.Ingress, err = newIngressLimiter(f.cfg, rt.db, time.Now)
	require.NoError(t, err)
	srv, err := web.NewServer(opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("HTTP observation test shutdown timed out")
		}
	})
	client := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	for _, path := range []string{"overview", "timeline?run_id=obs-api-real", "timeline?operation_id=obs-api-op", "timeline?ingest_job_id=obs-api-ingest", "timeline?task_id=obs-api-task", "context?run_id=obs-api-real", "metrics?window=15m", "alerts", "export", "cleanup"} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/api/v1/observability/"+path, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer synthetic-test-admin")
			res, err := client.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			require.NoError(t, res.Body.Close())
			require.Equal(t, 200, res.StatusCode, path+": "+string(body))
			require.NotContains(t, string(body), "CANARY", path)
			if strings.HasPrefix(path, "timeline?run_id") {
				var envelope struct {
					Data observability.Timeline `json:"data"`
				}
				require.NoError(t, json.Unmarshal(body, &envelope))
				found := false
				for _, e := range envelope.Data.Entries {
					if e.Source == observability.SourceDiagnostic {
						found = true
						require.Equal(t, 3, e.RedactedFields)
						require.Equal(t, "https://example.invalid", e.Attrs["url"])
					}
				}
				require.True(t, found, "必须实际读到诊断行")
			}
			if path == "alerts" {
				require.Contains(t, string(body), `"state":"unknown"`)
				require.Contains(t, string(body), `"reason":"disabled"`)
				require.Contains(t, string(body), `"last_eval_at":null`)
			}
			if path == "context?run_id=obs-api-real" {
				var envelope struct {
					Data observability.ContextDiagnosis `json:"data"`
				}
				require.NoError(t, json.Unmarshal(body, &envelope))
				require.NotNil(t, envelope.Data.Retrieval)
				require.Len(t, envelope.Data.Retrieval.Candidates, 1, "canary safety must be checked alongside a real allowed candidate")
				require.Equal(t, "allowed", envelope.Data.Retrieval.Candidates[0].ChunkID)
				require.Equal(t, []string{"allowed"}, envelope.Data.Retrieval.Hits)
			}
			if path == "export" {
				require.Contains(t, string(body), `"endpoint_configured":true`)
				require.Contains(t, string(body), `"headers_configured":true`)
			}
		})
	}
	require.Equal(t, before, observationReadFingerprint(t, rt.db), "所有GET不得评估告警、清理、入队、恢复或修改业务/证据")
}

func observationReadFingerprint(t *testing.T, db *postgres.DB) string {
	t.Helper()
	var out string
	// 固定表集合；聚合原始整行，用于捕获GET的任何隐式写入。
	// Fixed whole-row aggregation detects implicit writes by GET.
	err := db.QueryRowContext(t.Context(), `SELECT md5(COALESCE(string_agg(v,',' ORDER BY v),'')) FROM (
	SELECT row_to_json(r)::text v FROM runs r UNION ALL SELECT row_to_json(r)::text FROM llm_calls r
	UNION ALL SELECT row_to_json(r)::text FROM tool_invocations r UNION ALL SELECT row_to_json(r)::text FROM messages r
	UNION ALL SELECT row_to_json(r)::text FROM management_operations r UNION ALL SELECT row_to_json(r)::text FROM diagnostic_events r
	UNION ALL SELECT row_to_json(r)::text FROM observability_alert_states r UNION ALL SELECT row_to_json(r)::text FROM observability_alert_transitions r
	UNION ALL SELECT row_to_json(r)::text FROM observability_export_items r UNION ALL SELECT row_to_json(r)::text FROM observability_export_batches r
	UNION ALL SELECT row_to_json(r)::text FROM observability_export_scan r UNION ALL SELECT row_to_json(r)::text FROM observability_cleanup_runs r)r`).Scan(&out)
	require.NoError(t, err)
	return out
}

func TestObservability_APICleanupHistoryUsesExecutionTime(t *testing.T) {
	o, h := newObservationFixture(t, cli.DefaultObservabilityConfig())
	now := time.Now().UTC().Truncate(time.Millisecond)
	t.Run("old_data_cleaned_now", func(t *testing.T) {
		_, err := h.db.ExecContext(t.Context(), `INSERT INTO observability_cleanup_runs(cleanup_id,table_kind,window_start,window_end,deleted_count,status,started_at,finished_at) VALUES('today-old-data','diagnostic_events',$1,$2,3,'succeeded',$3,$3)`, now.Add(-8*24*time.Hour).UnixMilli(), now.Add(-7*24*time.Hour).UnixMilli(), now.UnixMilli())
		require.NoError(t, err)
		got, err := o.CleanupStatus(t.Context(), observability.ReadWindow{From: now.Add(-time.Hour), To: now.Add(time.Second), Limit: 10})
		require.NoError(t, err)
		require.Len(t, got.Runs, 1, "清理执行时间落在管理历史窗口，不能因删的是旧数据而消失")
		require.Equal(t, "today-old-data", got.Runs[0].CleanupID)
	})
	t.Run("latest_before_limit", func(t *testing.T) {
		_, err := h.db.ExecContext(t.Context(), `INSERT INTO observability_cleanup_runs(cleanup_id,table_kind,window_start,window_end,status,started_at,finished_at) SELECT 'page-'||g,'diagnostic_events',$1::bigint,$2::bigint,'succeeded',$1::bigint+g,$1::bigint+g FROM generate_series(1,10002)g`, now.UnixMilli(), now.Add(time.Minute).UnixMilli())
		require.NoError(t, err)
		got, err := o.CleanupStatus(t.Context(), observability.ReadWindow{From: now.Add(-time.Hour), To: now.Add(time.Minute), Limit: 2})
		require.NoError(t, err)
		require.Len(t, got.Runs, 2)
		require.Equal(t, "page-10002", got.Runs[0].CleanupID)
		require.True(t, got.Truncated)
	})
}

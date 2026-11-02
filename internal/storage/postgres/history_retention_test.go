//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/observability"
)

func historySeed(t *testing.T, db *DB, session, run string, at time.Time) {
	t.Helper()
	if session != "" {
		_, err := db.pool.ExecContext(t.Context(), `INSERT INTO sessions(session_id,profile_name,channel,user_id,created_at,last_active_at) VALUES($1,'agent','web','user',$2,$2)`, session, at.UnixMilli())
		require.NoError(t, err)
	}
	_, err := db.pool.ExecContext(t.Context(), `INSERT INTO runs(run_id,session_id,agent,channel,user_id,trigger,status,started_at,finished_at,caller_id,authorizing_kind,actor) VALUES($1,NULLIF($2,''),'agent','web','user','user','completed',$3,$3,'local-admin','admin-builtin','admin-cli')`, run, session, at.UnixMilli())
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `INSERT INTO llm_calls(run_id,session_id,round,provider,model,duration_ms,status,created_at) VALUES($1,NULLIF($2,''),1,'provider','model',20,'success',$3)`, run, session, at.UnixMilli())
	require.NoError(t, err)
	if session != "" {
		_, err = db.pool.ExecContext(t.Context(), `INSERT INTO messages(session_id,run_id,seq,role,content,created_at) VALUES($1,$2,1,'assistant','old message',$3)`, session, run, at.UnixMilli())
		require.NoError(t, err)
	}
}

func TestHistoryRetention_GroupProtection(t *testing.T) {
	for _, mode := range []string{"expired", "recent_session", "running", "unknown_model", "pending_tool", "pending_quota", "recent_message", "busy", "missing_gate", "cross_scope_summary", "export_enabled", "pending_export", "external_diagnostic"} {
		t.Run(mode, func(t *testing.T) {
			db := portTestDB(t)
			now := time.Now().UTC()
			old := now.Add(-40 * 24 * time.Hour)
			db.now = func() time.Time { return now }
			historySeed(t, db, "session", "run", old)
			var query string
			switch mode {
			case "pending_export":
				query = `INSERT INTO observability_export_items(item_id,source_kind,source_id,state_hash,signal,source_terminal_at,state,created_at,updated_at) VALUES('item','runs','run','hash','trace',1,'pending',1,1)`
			case "external_diagnostic":
				query = `INSERT INTO diagnostic_events(event_id,schema_version,kind,occurred_at,request_id,audit_ref) SELECT 'event',0,'llm_call',1,'other-request','llm_calls:'||id::text FROM llm_calls`
			case "recent_session":
				query = `UPDATE sessions SET last_active_at=$1`
			case "running":
				query = `UPDATE runs SET status='running',finished_at=NULL`
			case "unknown_model":
				query = `UPDATE llm_calls SET status='unknown'`
			case "pending_tool":
				query = `INSERT INTO tool_invocations(run_id,session_id,tool_name,status,started_at) VALUES('run','session','tool','pending',1)`
			case "pending_quota":
				query = `INSERT INTO quota_reservations(reservation_id,caller_id,idempotency_key,class,run_id,period_start,amount,state,created_at) VALUES('reservation','local-admin','idempotency','run','run',1,1,'pending',1)`
			case "recent_message":
				query = `UPDATE messages SET created_at=$1`
			case "cross_scope_summary":
				historySeed(t, db, "other", "other-run", now)
				query = `INSERT INTO compaction_entries(compaction_id,scope_session_id,trigger_run_id,chain_epoch,attempt_id,covered_from_message_id,cut_message_id,first_kept_message_id,strategy_version,prompt_version,model,status,created_at) VALUES('summary','other','run',1,'attempt','1','2','3','v1','v1','model','committed',1)`
			}
			if query != "" {
				var args []any
				if mode == "recent_session" || mode == "recent_message" {
					args = append(args, now.UnixMilli())
				}
				_, err := db.pool.ExecContext(t.Context(), query, args...)
				require.NoError(t, err)
			}
			gate := core.NewAgentService(core.ServiceOptions{})
			if mode == "busy" {
				release, ok := gate.TrySessionMaintenance("session")
				require.True(t, ok)
				defer release()
			}
			var rerr error
			store := NewHistoryRetentionStore(db, mode == "export_enabled")
			if mode == "missing_gate" {
				_, rerr = store.CleanupHistoryBatch(t.Context(), "sessions", now.Add(-30*24*time.Hour), 100, "", nil)
			} else {
				_, rerr = store.CleanupHistoryBatch(t.Context(), "sessions", now.Add(-30*24*time.Hour), 100, "", gate)
			}
			require.NoError(t, rerr)
			var count int
			require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM sessions WHERE session_id='session'`).Scan(&count))
			if mode == "expired" {
				require.Zero(t, count)
				for _, table := range []string{"runs", "messages", "llm_calls"} {
					require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&count))
					require.Zero(t, count)
				}
			} else {
				require.Equal(t, 1, count)
			}
			// Repeated maintenance is safe; an active session cannot disappear mid-run.
			_, err := store.CleanupHistoryBatch(t.Context(), "sessions", now.Add(-30*24*time.Hour), 100, "", gate)
			require.NoError(t, err)
		})
	}
}

func TestHistoryRetention_StatelessRollbackAndReports(t *testing.T) {
	db := portTestDB(t)
	now := time.Now().UTC()
	historySeed(t, db, "", "run", now.Add(-40*24*time.Hour))
	store := NewHistoryRetentionStore(db, false)
	_, err := db.pool.ExecContext(t.Context(), `CREATE FUNCTION reject_history_report() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='succeeded' THEN RAISE EXCEPTION 'synthetic report failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_history_report BEFORE INSERT ON history_retention_runs FOR EACH ROW EXECUTE FUNCTION reject_history_report()`)
	require.NoError(t, err)
	r, err := store.CleanupHistoryBatch(t.Context(), "runs", now.Add(-30*24*time.Hour), 100, "", nil)
	require.Error(t, err)
	require.Zero(t, r.DeletedRows)
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM runs`).Scan(&count))
	require.Equal(t, 1, count)
	_, err = db.pool.ExecContext(t.Context(), `DROP TRIGGER reject_history_report ON history_retention_runs`)
	require.NoError(t, err)
	r, err = store.CleanupHistoryBatch(t.Context(), "runs", now.Add(-30*24*time.Hour), 100, "", nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, r.DeletedGroups)
	require.EqualValues(t, 2, r.DeletedRows)
	reports, err := store.HistoryReports(t.Context(), now.Add(-time.Hour), time.Now().Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, reports, 2)
}

func TestHistoryRetention_MaintenanceAndAllowlist(t *testing.T) {
	db := portTestDB(t)
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	historySeed(t, db, "kept", "kept-run", old)
	_, err := db.pool.ExecContext(t.Context(), `INSERT INTO config_snapshots(agent_name,hash,content,manifest_json,created_at) VALUES('agent','kept','config','[]',1),('agent','orphan','config','[]',1),('agent','audit-kept','config','[]',1);UPDATE runs SET config_hash='kept'; INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at,config_hash) VALUES('orphan-audit',1,'provider','model',20,'unknown',1,'audit-kept')`)
	require.NoError(t, err)
	store := NewHistoryRetentionStore(db, false)
	_, err = store.CleanupHistoryBatch(t.Context(), "maintenance", now.Add(-30*24*time.Hour), 100, "", nil)
	require.NoError(t, err)
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM config_snapshots`).Scan(&count))
	require.Equal(t, 2, count)
	_, err = store.CleanupHistoryBatch(t.Context(), "runs;DELETE FROM runs", old, 100, "", nil)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.CleanupHistoryBatch(ctx, "runs", old, 100, "", nil)
	require.Error(t, err)
}

func TestHistoryRetention_IngestAndOperationAnchors(t *testing.T) {
	db := portTestDB(t)
	now := time.Now().UTC()
	store := NewHistoryRetentionStore(db, false)
	knSeedCollection(t, db, "collection", "collection")
	_, err := db.pool.ExecContext(t.Context(), `INSERT INTO kb_documents(document_id,collection_id,source_path,content_hash,created_at,updated_at) VALUES('doc','collection','source.md','hash',1,1);
 INSERT INTO kb_document_versions(version_id,document_id,content_hash,status,parser_version,chunker_version,created_at,updated_at) VALUES('version','doc','hash','active','v1','v1',1,1);
 UPDATE kb_documents SET latest_version_id='version';
 INSERT INTO kb_ingest_jobs(ingest_job_id,document_id,version_id,stage,attempt,status,started_at,finished_at) VALUES
 ('old-success','doc','version','done',1,'success',1,1),('needed-success','doc','version','done',2,'success',1,1),('latest-failed','doc','version','done',3,'failed',1,1);
 INSERT INTO management_operations(operation_id,action,target_kind,target_id,actor_subject,auth_method,source,status,created_at,finished_at) VALUES
 ('old-test','model_connection.test','model_connection','model','admin','cli','cli','success',1,1),
 ('latest-test','model_connection.test','model_connection','model','admin','cli','cli','success',2,2),
 ('origin','system.baseline_v2','system','system','migration','migration','migration','success',1,1),
 ('unknown-test','model_connection.test','model_connection','model','admin','cli','cli','unknown',1,1)`)
	require.NoError(t, err)
	r, err := store.CleanupHistoryBatch(t.Context(), "ingest", now.Add(-30*24*time.Hour), 100, "", nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, r.DeletedGroups)
	require.EqualValues(t, 2, r.ProtectedGroups)
	r, err = store.CleanupHistoryBatch(t.Context(), "operations", now.Add(-30*24*time.Hour), 100, "", nil)
	require.NoError(t, err)
	require.EqualValues(t, 1, r.DeletedGroups)
	require.EqualValues(t, 3, r.ProtectedGroups)
	var count int
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM kb_document_versions WHERE status='active'`).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT count(*) FROM kb_documents WHERE latest_version_id='version'`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestHistoryRetention_CompactionChainIsAtomic(t *testing.T) {
	db := portTestDB(t)
	now := time.Now().UTC()
	historySeed(t, db, "session", "run", now.Add(-40*24*time.Hour))
	_, err := db.pool.ExecContext(t.Context(), `INSERT INTO compaction_entries(compaction_id,scope_session_id,trigger_run_id,chain_epoch,attempt_id,covered_from_message_id,cut_message_id,first_kept_message_id,strategy_version,prompt_version,model,status,created_at) VALUES
 ('parent','session','run',1,'attempt1','1','2','3','v1','v1','model','committed',1),('child','session','run',1,'attempt2','1','3','4','v1','v1','model','committed',1);
 UPDATE compaction_entries SET parent_compaction_id='parent' WHERE compaction_id='child';
 INSERT INTO message_evidence_refs(message_id,kind,ref_id,version_id,created_at) SELECT id,'knowledge','source','v1',1 FROM messages`)
	require.NoError(t, err)
	r, err := NewHistoryRetentionStore(db, false).CleanupHistoryBatch(t.Context(), "sessions", now.Add(-30*24*time.Hour), 100, "", core.NewAgentService(core.ServiceOptions{}))
	require.NoError(t, err)
	require.EqualValues(t, 1, r.DeletedGroups)
	require.EqualValues(t, 7, r.DeletedRows)
}

func TestHistoryRetention_DisabledExportKeepsUnresolvedDiagnosticSources(t *testing.T) {
	db := portTestDB(t)
	now := time.Now().UTC()
	for _, state := range []string{"pending", "inflight", "retry", "unconfirmed", "sent"} {
		obsSeedCleanupEvent(t, db, state, now.Add(-40*24*time.Hour))
		_, err := db.pool.ExecContext(t.Context(), `INSERT INTO observability_export_items(item_id,source_kind,source_id,state_hash,signal,source_terminal_at,state,created_at,updated_at) VALUES($1,'diagnostic_events',$1,'hash','log',1,$1,1,1)`, state)
		require.NoError(t, err)
	}
	r, err := NewObservabilityStore(db).CleanupBatch(t.Context(), observability.CleanupRequest{Table: "diagnostic_events", Cutoff: now.Add(-30 * 24 * time.Hour), Limit: 100})
	require.NoError(t, err)
	require.EqualValues(t, 1, r.Deleted)
	require.EqualValues(t, 4, r.Skipped)
}

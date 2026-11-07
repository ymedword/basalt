//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/knowledge"
	"github.com/ymedword/basalt/internal/observability"
)

func TestObservabilityStore_FixedFailureReasons(t *testing.T) {
	db := portTestDB(t)
	job := embSeedJob(t, db, "obs-failed-job")
	s := NewObservabilityStore(db)
	for _, kind := range append(knowledge.ErrorKinds(), "CANARY_RAW_ERROR") {
		_, err := db.pool.ExecContext(t.Context(), `UPDATE kb_ingest_jobs SET status='failed',error_kind=$1 WHERE ingest_job_id=$2`, kind, job)
		require.NoError(t, err)
		fact, err := s.IngestJobFacts(t.Context(), job)
		require.NoError(t, err)
		if kind == "CANARY_RAW_ERROR" {
			require.Equal(t, "unclassified", fact.ErrorKind)
		} else {
			require.Equal(t, kind, fact.ErrorKind, "fixed ingest failure classes must survive projection")
		}
	}
	require.NoError(t, startRunTest(NewRunStore(db), t.Context(), core.RunRecord{
		RunID: "obs-known-reason", Agent: "synthetic", Channel: "web",
		StartedAt: time.UnixMilli(1000), Principal: core.LocalAdminCLIPrincipal(),
	}, core.RunResourceManifest{}))
	for reason, want := range map[string]string{core.ReasonToolBudget: "tool_call_limit", core.ReasonIterationLimit: "iteration_limit", "超过运行时限": "run_timeout", "context canceled": "cancelled", "CANARY_RAW_ERROR": "unclassified"} {
		_, err := db.pool.ExecContext(t.Context(), `UPDATE runs SET reason=$1 WHERE run_id='obs-known-reason'`, reason)
		require.NoError(t, err)
		fact, err := s.RunFacts(t.Context(), "obs-known-reason")
		require.NoError(t, err)
		require.Equal(t, want, fact.Reason)
	}
}

func TestObservabilityStore_AssemblyPresence(t *testing.T) {
	db := portTestDB(t)
	require.NoError(t, startRunTest(NewRunStore(db), t.Context(), core.RunRecord{
		RunID: "assembly-presence", Agent: "synthetic",
		Channel: "web", StartedAt: time.UnixMilli(1000), Principal: core.LocalAdminCLIPrincipal(),
	}, core.RunResourceManifest{}))
	writer := NewAssemblyStore(db, slog.New(slog.DiscardHandler))
	writer.Record(t.Context(), core.AssemblyRecord{
		RunID: "assembly-presence", Purpose: core.PurposeRetrieval,
		Budget: core.ContextBudget{ContextTokens: 99}, Retrieval: &core.RetrievalRecord{
			TopK: 2, CandidateK: 8, BudgetTokens: 0, FilteredCount: 0, FilteredReason: "not_distinguished",
		},
		Candidates: []core.AssemblyCandidate{{ChunkID: "allowed", Decision: "budget_drop", ACLDecision: "allow"}},
	})
	facts, err := NewObservabilityStore(db).AssemblyByRun(t.Context(), "assembly-presence")
	require.NoError(t, err)
	require.Len(t, facts, 1)
	require.False(t, facts[0].BudgetRecorded, "retrieval must not borrow a chat budget")
	require.Equal(t, float64(0), facts[0].Retrieval["filtered_count"], "recorded zero must survive")
	require.NotContains(t, facts[0].Retrieval, "index_generation", "missing generation must remain missing")
	require.Equal(t, "budget_drop", facts[0].Candidates[0].Decision)
	var nullBudget bool
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT budget IS NULL FROM context_assembly_records WHERE run_id='assembly-presence'`).Scan(&nullBudget))
	require.True(t, nullBudget)
}

func TestObservabilityStore_TimelineQueries(t *testing.T) {
	db := portTestDB(t)
	ctx := t.Context()
	record := core.RunRecord{
		RunID: "obs-read", RequestID: "request:read", SessionID: "session-read", Agent: "synthetic",
		Channel: "web", StartedAt: time.UnixMilli(1000), Principal: core.LocalAdminCLIPrincipal(),
	}
	require.NoError(t, startRunTest(NewRunStore(db), ctx, record, core.RunResourceManifest{}))
	_, err := db.pool.ExecContext(ctx, `UPDATE runs SET reason='CANARY_RAW_ERROR' WHERE run_id=$1`, record.RunID)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO llm_calls(run_id,round,provider,model,duration_ms,status,created_at,error_message)
		VALUES ($1,1,'fake','fake',12,'success',1000,'CANARY_LLM_ERROR'),('unrelated',1,'fake','fake',0,'pending',1000,NULL)`, record.RunID)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO tool_invocations(run_id,tool_name,tool_call_id,status,started_at,finished_at,error_message)
		VALUES ($1,'fs.read','real-call','success',1000,1012,'CANARY_TOOL_ERROR')`, record.RunID)
	require.NoError(t, err)
	s := NewObservabilityStore(db)
	run, err := s.RunFacts(ctx, record.RunID)
	require.NoError(t, err)
	require.Equal(t, "unclassified", run.Reason)
	require.Equal(t, record.RequestID, run.RequestID)
	ids, err := s.RunsByRequestID(ctx, record.RequestID)
	require.NoError(t, err)
	require.Equal(t, []string{record.RunID}, ids)
	ids, err = s.RunsBySessionID(ctx, record.SessionID)
	require.NoError(t, err)
	require.Equal(t, []string{record.RunID}, ids)
	ids, err = s.RunsByRequestID(ctx, "' OR true --")
	require.NoError(t, err)
	require.Empty(t, ids, "hostile input remains a literal parameter")
	runs, err := s.RunsByAgentWindow(ctx, record.Agent, time.UnixMilli(900), time.UnixMilli(1100))
	require.NoError(t, err)
	require.Len(t, runs, 1)
	_, err = s.RunsByAgentWindow(ctx, "", time.Time{}, time.Time{})
	require.ErrorIs(t, err, observability.ErrInvalidQuery)
	llm, err := s.LLMCallsByRun(ctx, record.RunID)
	require.NoError(t, err)
	require.Len(t, llm, 1)
	require.NotNil(t, llm[0].DurationMS)
	require.Equal(t, int64(12), *llm[0].DurationMS)
	tools, err := s.ToolInvocationsByRun(ctx, record.RunID)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	require.Equal(t, "real-call", tools[0].ToolCallID)
	require.Equal(t, int64(1012), tools[0].FinishedAt.UnixMilli())
	raw, err := json.Marshal([]any{run, llm, tools})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "CANARY")
	seedOperation(t, db, "obs-op")
	op, err := s.OperationFacts(ctx, "obs-op")
	require.NoError(t, err)
	require.Equal(t, "obs-op", op.OperationID)
	_, err = s.RunFacts(ctx, "missing")
	require.ErrorIs(t, err, core.ErrRunNotFound)
	_, err = s.OperationFacts(ctx, "missing")
	require.ErrorIs(t, err, core.ErrOperationNotFound)
	_, err = s.IngestJobFacts(ctx, "missing")
	require.ErrorIs(t, err, fs.ErrNotExist)
	_, err = s.EmbeddingCallsByRun(ctx, record.RunID)
	require.NoError(t, err)
	_, err = s.EmbeddingCallsByIngest(ctx, "missing")
	require.NoError(t, err)
	_, err = s.TaskExecutionFacts(ctx, "missing", time.Time{}, time.Time{})
	require.NoError(t, err)
	_, err = s.TaskExecutionByRun(ctx, record.RunID)
	require.NoError(t, err)
	_, err = s.CleanupRuns(ctx, time.UnixMilli(900), time.UnixMilli(1100))
	require.NoError(t, err)
	_, err = s.AssemblyByRun(ctx, "missing")
	require.NoError(t, err)
}

func TestObservabilityMigration_UpgradeFromV5(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, 5))
	_, err := conn.ExecContext(ctx, `INSERT INTO runs
		(run_id,agent,channel,user_id,trigger,status,started_at,caller_id,authorizing_kind,actor)
		VALUES ('obs-old','agent','web','synthetic','user','completed',1,'local-admin','admin-builtin','admin-cli')`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO context_assembly_records
		(record_id,run_id,step_id,purpose,agent,prompt_version,model_version,estimator_version,created_at)
		VALUES ('old-assembly','obs-old','','retrieval','agent','','','',1)`)
	require.NoError(t, err)
	require.NoError(t, ensureSchema(ctx, conn, 6), "v5 must upgrade to OBS without rewriting v1")
	var requestID, retrieval sql.NullString
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT request_id FROM runs WHERE run_id='obs-old'`).Scan(&requestID))
	require.False(t, requestID.Valid, "historical request identity must not be invented")
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT retrieval::text FROM context_assembly_records WHERE record_id='old-assembly'`).Scan(&retrieval))
	require.False(t, retrieval.Valid)
	_, err = conn.ExecContext(ctx, `UPDATE context_assembly_records SET budget=NULL WHERE record_id='old-assembly'`)
	require.NoError(t, err, "non-chat evidence must support unrecorded budget")
	require.NoError(t, ensureSchema(ctx, conn, 6), "migration runner must remain idempotent")
	var versions int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM schema_version`).Scan(&versions))
	require.Equal(t, 6, versions)
}

func TestDiagnostics_UnknownEnumReadable(t *testing.T) {
	db := portTestDB(t)
	ctx := t.Context()
	// 隔离夹具模拟升级前的未知kind；恢复当前NOT VALID约束后再走生产读路径。
	_, err := db.pool.ExecContext(ctx, `ALTER TABLE diagnostic_events DROP CONSTRAINT diagnostic_events_kind_check`)
	require.NoError(t, err)
	id, err := diagnostics.NewEventID(time.UnixMilli(1000))
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `INSERT INTO diagnostic_events(event_id,schema_version,kind,occurred_at,run_id,attrs)
		VALUES ($1,0,'future_kind',1000,'old-run','{"password":"CANARY_OLD","count":1}')`, id)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `ALTER TABLE diagnostic_events ADD CONSTRAINT diagnostic_events_kind_check CHECK
		(kind IN ('ingest_stage','index_switch','retrieval','assembly','embedding','llm_call','tool_call','permission_decision','quota_decision','migration')) NOT VALID`)
	require.NoError(t, err)
	events, err := NewObservabilityStore(db).ListEvents(ctx, diagnostics.Query{RunID: "old-run", Limit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, diagnostics.Kind("future_kind"), events[0].Kind)
	require.NotContains(t, events[0].Attrs, "password")
	_, err = db.pool.ExecContext(ctx, `INSERT INTO diagnostic_events(event_id,schema_version,kind,occurred_at,run_id,attrs)
		VALUES ('new-unknown',0,'future_kind',1001,'old-run','{}')`)
	require.Error(t, err, "new unknown writes must be refused")
}

func TestObservabilityMigration_Applies(t *testing.T) {
	conn := migrationConn(t)
	require.NoError(t, ensureSchema(context.Background(), conn, 6))
	tables, err := tableNames(context.Background(), conn)
	require.NoError(t, err)
	require.Len(t, tables, 58, "v1 52 tables plus exactly six OBS-owned tables")
	for _, name := range []string{"observability_export_items", "observability_export_batches", "observability_export_scan", "observability_cleanup_runs", "observability_alert_states", "observability_alert_transitions"} {
		require.Contains(t, tables, name)
	}
}

func TestObservabilitySchema_Enums(t *testing.T) {
	conn := migrationConn(t)
	ctx := context.Background()
	require.NoError(t, ensureSchema(ctx, conn, 6))
	var definition string
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid='observability_export_items'::regclass AND conname='observability_export_items_source_kind_check'`).Scan(&definition))
	require.Contains(t, definition, "metric_snapshot")
	require.NotContains(t, definition, "context_assembly_records", "assembly evidence is never exported")
	for _, source := range []string{"runs", "llm_calls", "tool_invocations", "embedding_calls", "task_executions", "management_operations", "kb_ingest_jobs", "diagnostic_events"} {
		require.Contains(t, definition, source)
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO observability_export_items
		(item_id,source_kind,source_id,state_hash,signal,source_terminal_at,state,created_at,updated_at)
		VALUES ('invalid','metric_snapshot','x','h','metric',1,'invented',1,1)`)
	require.Error(t, err, "unknown item states must be refused")
}

func TestObservabilityStore_RequestIDBridge(t *testing.T) {
	db := portTestDB(t)
	store := NewRunStore(db)
	record := core.RunRecord{
		RunID: "obs-request", RequestID: "request:real-1", Agent: "agent", Channel: "web",
		UserID: "synthetic", StartedAt: time.UnixMilli(1),
		Principal: core.LocalAdminCLIPrincipal(),
	}
	require.NoError(t, startRunTest(store, t.Context(), record, core.RunResourceManifest{}))
	var request sql.NullString
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT request_id FROM runs WHERE run_id=$1`, record.RunID).Scan(&request))
	require.True(t, request.Valid, "real request identity must reach the run ledger")
	require.Equal(t, record.RequestID, request.String)
	detail, err := store.GetRun(t.Context(), record.RunID)
	require.NoError(t, err)
	require.Equal(t, record.RequestID, detail.RequestID)
	record.RunID, record.RequestID, record.Channel = "obs-scheduled", "", "scheduler"
	require.NoError(t, startRunTest(store, t.Context(), record, core.RunResourceManifest{}))
	require.NoError(t, db.pool.QueryRowContext(t.Context(), `SELECT request_id FROM runs WHERE run_id=$1`, record.RunID).Scan(&request))
	require.False(t, request.Valid, "scheduler has no HTTP request identity to invent")
}

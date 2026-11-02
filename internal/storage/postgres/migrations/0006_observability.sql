-- US-16A adds read-side correlation and bounded, non-authoritative telemetry ledgers.
-- Existing business/audit rows keep their original values; no historical identities
-- or recovery times are invented. All timestamps are UTC Unix milliseconds.
ALTER TABLE runs ADD COLUMN request_id text;
ALTER TABLE runs ADD CONSTRAINT runs_request_id_shape CHECK
    (request_id IS NULL OR (length(request_id) BETWEEN 1 AND 64 AND request_id ~ '^[A-Za-z0-9._:-]+$'));
CREATE INDEX IF NOT EXISTS idx_runs_request ON runs(request_id) WHERE request_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_runs_finished ON runs(finished_at) WHERE finished_at IS NOT NULL;

ALTER TABLE llm_calls ADD COLUMN recovered_at bigint;
ALTER TABLE tool_invocations ADD COLUMN recovered_at bigint;
ALTER TABLE context_assembly_records ADD COLUMN retrieval jsonb;
ALTER TABLE context_assembly_records ALTER COLUMN budget DROP NOT NULL;
ALTER TABLE context_assembly_records ALTER COLUMN budget DROP DEFAULT;
CREATE INDEX IF NOT EXISTS idx_context_assembly_created ON context_assembly_records(created_at);
CREATE INDEX IF NOT EXISTS idx_llm_calls_terminal ON llm_calls
    ((GREATEST(created_at + duration_ms, COALESCE(recovered_at, 0)))) WHERE status <> 'pending';
CREATE INDEX IF NOT EXISTS idx_tool_invocations_terminal ON tool_invocations
    ((COALESCE(finished_at, recovered_at))) WHERE status <> 'pending';
CREATE INDEX IF NOT EXISTS idx_embedding_calls_finished ON embedding_calls(finished_at) WHERE finished_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_task_executions_finished ON task_executions(finished_at) WHERE finished_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_management_operations_terminal ON management_operations
    ((GREATEST(finished_at, COALESCE(recovered_at, 0)))) WHERE finished_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_kb_ingest_jobs_finished ON kb_ingest_jobs(finished_at) WHERE finished_at IS NOT NULL;

-- NOT VALID preserves readable historical unknown values while constraining new
-- database writes. The recorder remains the primary write-side validator.
ALTER TABLE diagnostic_events ADD CONSTRAINT diagnostic_events_kind_check CHECK
    (kind IN ('ingest_stage','index_switch','retrieval','assembly','embedding','llm_call',
              'tool_call','permission_decision','quota_decision','migration')) NOT VALID;
ALTER TABLE diagnostic_events ADD CONSTRAINT diagnostic_events_reason_code_check CHECK
    (reason_code IS NULL OR reason_code IN ('','vector_unavailable','keyword_unavailable',
     'rerank_timeout','permission_denied','budget_trimmed','history_trimmed','no_answer',
     'citation_unresolved','chunker_failed','embed_failed','index_failed','key_revoked',
     'quota_exhausted','rate_limited','migration_verify_failed','llm_call_failed',
     'concurrency_limited','chunk_invalidated')) NOT VALID;

CREATE TABLE IF NOT EXISTS observability_export_items (
    item_id text PRIMARY KEY,
    source_kind text NOT NULL CHECK (source_kind IN ('runs','llm_calls','tool_invocations',
        'embedding_calls','task_executions','management_operations','kb_ingest_jobs',
        'diagnostic_events','metric_snapshot')),
    source_id text NOT NULL,
    state_hash text NOT NULL,
    signal text NOT NULL CHECK (signal IN ('trace','log','metric')),
    source_terminal_at bigint NOT NULL,
    state text NOT NULL CHECK (state IN ('pending','inflight','sent','unconfirmed','retry',
        'failed_permanent','dropped','stale')),
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_batch_id text,
    next_attempt_at bigint,
    lease_until bigint,
    last_error_kind text,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    sent_at bigint,
    -- Only derived, redacted metric snapshots are persisted here. Execution facts
    -- are reloaded from their authoritative sources before sending.
    payload jsonb,
    CHECK (payload IS NULL OR source_kind = 'metric_snapshot'),
    UNIQUE (source_kind, source_id, state_hash)
);
CREATE INDEX IF NOT EXISTS idx_obs_export_ready ON observability_export_items(state, next_attempt_at, created_at);
CREATE INDEX IF NOT EXISTS idx_obs_export_source ON observability_export_items(source_kind, source_id, source_terminal_at);

CREATE TABLE IF NOT EXISTS observability_export_batches (
    batch_id text PRIMARY KEY,
    signal text NOT NULL CHECK (signal IN ('trace','log','metric')),
    record_unit text NOT NULL CHECK (record_unit IN ('span','log_record','data_point')),
    item_count integer NOT NULL CHECK (item_count > 0),
    record_count integer NOT NULL CHECK (record_count > 0),
    accepted_records integer CHECK (accepted_records >= 0),
    rejected_records integer CHECK (rejected_records >= 0),
    partial_success boolean NOT NULL DEFAULT false,
    warning_present boolean NOT NULL DEFAULT false,
    warning_text text,
    outcome text NOT NULL CHECK (outcome IN ('sent','sent_with_warning','partial_unconfirmed',
        'retry','permanent','timeout','unknown')),
    http_status integer,
    error_kind text,
    duration_ms bigint,
    attempted_at bigint NOT NULL,
    CHECK ((accepted_records IS NULL) = (rejected_records IS NULL)),
    CHECK (accepted_records IS NULL OR accepted_records + rejected_records = record_count)
);
CREATE INDEX IF NOT EXISTS idx_obs_batches_attempted ON observability_export_batches(attempted_at);

CREATE TABLE IF NOT EXISTS observability_export_scan (
    source_kind text PRIMARY KEY CHECK (source_kind IN ('runs','llm_calls','tool_invocations',
        'embedding_calls','task_executions','management_operations','kb_ingest_jobs','diagnostic_events')),
    coverage_kind text NOT NULL CHECK (coverage_kind IN ('terminal_time','occurred_at')),
    covered_from bigint,
    covered_through bigint,
    last_scan_started_at bigint,
    last_scan_at bigint,
    last_reconcile_at bigint,
    updated_at bigint NOT NULL,
    CHECK ((covered_from IS NULL) = (covered_through IS NULL)),
    CHECK (covered_from IS NULL OR covered_from <= covered_through)
);

CREATE TABLE IF NOT EXISTS observability_cleanup_runs (
    cleanup_id text PRIMARY KEY,
    table_kind text NOT NULL CHECK (table_kind IN
        ('diagnostic_events','context_assembly_records','alert_history','export_history')),
    window_start bigint NOT NULL,
    window_end bigint NOT NULL,
    scan_covered_from bigint,
    scan_covered_through bigint,
    require_confirmed_export boolean NOT NULL DEFAULT true,
    deleted_count bigint NOT NULL DEFAULT 0 CHECK (deleted_count >= 0),
    skipped_count bigint NOT NULL DEFAULT 0 CHECK (skipped_count >= 0),
    skip_reason text CHECK (skip_reason IN ('scan_not_covered','out_of_coverage',
        'unexported_backlog','unconfirmed_export','unexported_terminal','policy_disabled')),
    status text NOT NULL CHECK (status IN ('succeeded','failed','skipped')),
    error_kind text,
    started_at bigint NOT NULL,
    finished_at bigint
);
CREATE INDEX IF NOT EXISTS idx_obs_cleanup_window ON observability_cleanup_runs(table_kind, window_start, window_end);
CREATE INDEX IF NOT EXISTS idx_obs_cleanup_started ON observability_cleanup_runs(started_at DESC, cleanup_id DESC);

CREATE TABLE IF NOT EXISTS observability_alert_states (
    rule_id text NOT NULL,
    scope_key text NOT NULL,
    state text NOT NULL CHECK (state IN ('inactive','pending','firing','unknown')),
    since bigint NOT NULL,
    last_eval_at bigint NOT NULL,
    last_value double precision NOT NULL,
    updated_at bigint NOT NULL,
    PRIMARY KEY (rule_id, scope_key)
);
CREATE TABLE IF NOT EXISTS observability_alert_transitions (
    id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    rule_id text NOT NULL,
    scope_key text NOT NULL,
    from_state text NOT NULL CHECK (from_state IN ('inactive','pending','firing','unknown')),
    to_state text NOT NULL CHECK (to_state IN ('inactive','pending','firing','resolved','unknown')),
    value double precision NOT NULL,
    occurred_at bigint NOT NULL,
    evidence_ref text
);
CREATE INDEX IF NOT EXISTS idx_obs_alert_transitions_time ON observability_alert_transitions(occurred_at);
CREATE INDEX IF NOT EXISTS idx_obs_alert_transitions_rule ON observability_alert_transitions(rule_id, scope_key, id DESC);

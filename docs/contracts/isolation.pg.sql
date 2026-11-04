CREATE TABLE IF NOT EXISTS sandbox_executions (
    tool_invocation_id bigint PRIMARY KEY REFERENCES tool_invocations(id) ON DELETE CASCADE,
    run_id text NOT NULL,
    agent_name text NOT NULL,
    instance_id text NOT NULL,
    profile text NOT NULL,
    runtime text NOT NULL,
    image_digest text NOT NULL,
    container_name text NOT NULL,
    container_id text,
    state text NOT NULL,
    exit_code integer,
    error_reason text,
    artifact_count integer NOT NULL DEFAULT 0,
    artifact_bytes bigint NOT NULL DEFAULT 0,
    cleanup_state text NOT NULL,
    cleanup_error text,
    started_at bigint NOT NULL,
    finished_at bigint,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    CONSTRAINT sandbox_executions_state_check CHECK (
        state IN ('creating','running','finished','canceled','timeout','failed','unknown')
    ),
    CONSTRAINT sandbox_executions_cleanup_check CHECK (
        cleanup_state IN ('pending','cleaned','failed')
    )
);

CREATE INDEX IF NOT EXISTS idx_sandbox_executions_instance_open
    ON sandbox_executions(instance_id)
    WHERE state IN ('creating','running') OR cleanup_state IN ('pending','failed');
CREATE INDEX IF NOT EXISTS idx_sandbox_executions_run
    ON sandbox_executions(run_id);

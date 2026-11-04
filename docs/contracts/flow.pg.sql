-- US-19: append-only durable Flow schema; times are UTC Unix milliseconds.
-- Canonical definition/contract bytes are text identities; structured checkpoints use JSONB.
CREATE TABLE IF NOT EXISTS flow_definitions (
    definition_uid text NOT NULL,
    version bigint NOT NULL CHECK (version >= 1),
    name text NOT NULL,
    content_hash text NOT NULL,
    definition_content text NOT NULL CHECK (octet_length(definition_content) <= 1048576),
    contract_content text NOT NULL CHECK (octet_length(contract_content) <= 1048576),
    created_at bigint NOT NULL,
    operation_id text NOT NULL REFERENCES management_operations(operation_id),
    PRIMARY KEY (definition_uid, version)
);
CREATE INDEX IF NOT EXISTS idx_flow_definition_hash ON flow_definitions(definition_uid, content_hash);
CREATE INDEX IF NOT EXISTS idx_flow_definition_operation ON flow_definitions(operation_id);
CREATE TABLE IF NOT EXISTS flow_pointers (
    definition_uid text PRIMARY KEY,
    name text NOT NULL,
    current_version bigint NOT NULL,
    retired boolean NOT NULL DEFAULT false,
    updated_at bigint NOT NULL,
    operation_id text NOT NULL REFERENCES management_operations(operation_id),
    FOREIGN KEY (definition_uid, current_version) REFERENCES flow_definitions(definition_uid, version)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_flow_pointer_name ON flow_pointers(lower(name));
CREATE TABLE IF NOT EXISTS flow_tasks (
    task_id text PRIMARY KEY,
    root_task_id text NOT NULL,
    parent_task_id text,
    caller_id text NOT NULL REFERENCES callers(caller_id),
    authorizing_kind text NOT NULL CHECK (authorizing_kind IN ('caller-key', 'admin-builtin')),
    authorizing_key_id text,
    definition_uid text NOT NULL,
    definition_version bigint NOT NULL,
    contract_version bigint NOT NULL CHECK (contract_version >= 1),
    status text NOT NULL CHECK (status IN ('queued','running','succeeded','failed','stopped','blocked','cancelling','cancelled','abandoned')),
    state_version bigint NOT NULL CHECK (state_version >= 1),
    execution_epoch bigint NOT NULL CHECK (execution_epoch >= 1),
    current_revision bigint NOT NULL CHECK (current_revision BETWEEN 0 AND 2),
    cancel_requested boolean NOT NULL DEFAULT false,
    close_intent text NOT NULL CHECK (close_intent IN ('none','cancel','abandon','deadline')),
    deadline bigint NOT NULL,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    next_eligible_at bigint NOT NULL,
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    request_hash text NOT NULL,
    reason text NOT NULL DEFAULT '',
    depth integer NOT NULL CHECK (depth BETWEEN 0 AND 3),
    children_created integer NOT NULL DEFAULT 0 CHECK (children_created BETWEEN 0 AND 16),
    UNIQUE(caller_id, idempotency_key),
    UNIQUE(task_id, caller_id),
    FOREIGN KEY(definition_uid, definition_version) REFERENCES flow_definitions(definition_uid, version),
    FOREIGN KEY(root_task_id, caller_id) REFERENCES flow_tasks(task_id, caller_id) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY(parent_task_id, caller_id) REFERENCES flow_tasks(task_id, caller_id) DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT flow_close_intent_matches_cancel CHECK (cancel_requested = (close_intent IN ('cancel','abandon'))),
    CONSTRAINT flow_cancelling_has_intent CHECK (status <> 'cancelling' OR (cancel_requested AND close_intent IN ('cancel','abandon'))),
    CHECK ((authorizing_kind='admin-builtin' AND authorizing_key_id IS NULL AND caller_id='local-admin') OR (authorizing_kind='caller-key' AND authorizing_key_id IS NOT NULL AND caller_id<>'local-admin')),
    CHECK ((parent_task_id IS NULL AND root_task_id=task_id AND depth=0) OR (parent_task_id IS NOT NULL AND root_task_id<>task_id AND depth>0)),
    CHECK (deadline >= created_at)
);
CREATE INDEX IF NOT EXISTS idx_flow_tasks_owner ON flow_tasks(caller_id,status,updated_at,task_id);
CREATE INDEX IF NOT EXISTS idx_flow_tasks_ready ON flow_tasks(next_eligible_at,created_at,task_id) WHERE status='queued' AND NOT cancel_requested;
CREATE INDEX IF NOT EXISTS idx_flow_tasks_root ON flow_tasks(root_task_id,depth,task_id);
CREATE TABLE IF NOT EXISTS flow_task_contracts (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    contract_version bigint NOT NULL,
    definition_uid text NOT NULL,
    definition_version bigint NOT NULL,
    content text NOT NULL CHECK (octet_length(content)<=1048576),
    content_hash text NOT NULL,
    PRIMARY KEY(task_id,contract_version),
    FOREIGN KEY(definition_uid,definition_version) REFERENCES flow_definitions(definition_uid,version)
);
CREATE TABLE IF NOT EXISTS flow_task_revisions (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    revision bigint NOT NULL CHECK (revision BETWEEN 0 AND 2),
    contract_version bigint NOT NULL,
    checkpoint jsonb NOT NULL CHECK (COALESCE(jsonb_typeof(checkpoint)='object' AND checkpoint->>'format_version'='1', false)),
    created_at bigint NOT NULL,
    PRIMARY KEY(task_id,revision),
    FOREIGN KEY(task_id,contract_version) REFERENCES flow_task_contracts(task_id,contract_version),
    CHECK (COALESCE(checkpoint->>'task_id'=task_id AND (checkpoint->>'revision')::bigint=revision, false))
);
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='flow_tasks'::regclass AND conname='flow_task_contract_head') THEN
        ALTER TABLE flow_tasks ADD CONSTRAINT flow_task_contract_head FOREIGN KEY(task_id,contract_version) REFERENCES flow_task_contracts(task_id,contract_version) DEFERRABLE INITIALLY DEFERRED;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='flow_tasks'::regclass AND conname='flow_task_revision_head') THEN
        ALTER TABLE flow_tasks ADD CONSTRAINT flow_task_revision_head FOREIGN KEY(task_id,current_revision) REFERENCES flow_task_revisions(task_id,revision) DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS flow_transitions (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    seq bigint NOT NULL,
    operation_id text NOT NULL,
    phase text NOT NULL,
    request_hash text NOT NULL,
    old_state_version bigint NOT NULL,
    new_state_version bigint NOT NULL,
    old_revision bigint NOT NULL,
    new_revision bigint NOT NULL,
    reason text NOT NULL,
    actor_kind text NOT NULL,
    authorizing_key_id text,
    created_at bigint NOT NULL,
    PRIMARY KEY(task_id,seq),
    UNIQUE(task_id,operation_id,phase),
    CHECK (new_state_version >= old_state_version AND new_revision >= old_revision)
);
CREATE TABLE IF NOT EXISTS flow_node_attempts (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    node_attempt_id text NOT NULL UNIQUE,
    node_id text NOT NULL,
    node_invocation_id text NOT NULL,
    attempt integer NOT NULL CHECK (attempt BETWEEN 1 AND 3),
    task_revision bigint NOT NULL,
    execution_epoch bigint NOT NULL,
    run_id text UNIQUE,
    session_id text,
    state text NOT NULL CHECK (state IN ('prepared','running','succeeded','failed','cancelled','interrupted','unknown')),
    input text NOT NULL CHECK (octet_length(input)<=1048576),
    input_hash text NOT NULL,
    output text CHECK (octet_length(output)<=1048576),
    output_hash text,
    started_at bigint NOT NULL,
    finished_at bigint,
    PRIMARY KEY(task_id,node_attempt_id),
    FOREIGN KEY(task_id,task_revision) REFERENCES flow_task_revisions(task_id,revision)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_flow_attempt_active ON flow_node_attempts(task_id,node_invocation_id) WHERE state IN ('prepared','running');
CREATE INDEX IF NOT EXISTS idx_flow_attempt_task ON flow_node_attempts(task_id,started_at,node_attempt_id);
CREATE TABLE IF NOT EXISTS flow_actions (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    action_id text NOT NULL UNIQUE,
    node_invocation_id text NOT NULL,
    logical_slot text NOT NULL,
    tool_name text NOT NULL,
    adapter_version text NOT NULL,
    schema_hash text NOT NULL,
    input text NOT NULL CHECK (octet_length(input)<=1048576),
    input_hash text NOT NULL,
    recovery_class text NOT NULL CHECK (recovery_class IN ('readonly','verifiable_write','unverifiable_write')),
    outcome text NOT NULL CHECK (outcome IN ('prepared','dispatching','succeeded','failed','unknown','not_issued')),
    created_at bigint NOT NULL,
    PRIMARY KEY(task_id,action_id),
    UNIQUE(task_id,node_invocation_id,logical_slot)
);
CREATE INDEX IF NOT EXISTS idx_flow_actions_unresolved ON flow_actions(task_id,action_id) WHERE outcome IN ('dispatching','unknown');
CREATE TABLE IF NOT EXISTS flow_action_attempts (
    task_id text NOT NULL,
    action_id text NOT NULL,
    action_attempt_id text NOT NULL UNIQUE,
    node_attempt_id text NOT NULL,
    run_id text NOT NULL,
    tool_invocation_id bigint UNIQUE REFERENCES tool_invocations(id),
    llm_call_id bigint UNIQUE REFERENCES llm_calls(id),
    task_revision bigint NOT NULL,
    execution_epoch bigint NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('prepared','dispatching','succeeded','failed','unknown','not_issued')),
    output text CHECK (octet_length(output)<=1048576),
    output_hash text,
    created_at bigint NOT NULL,
    dispatched_at bigint,
    finished_at bigint,
    PRIMARY KEY(task_id,action_id,action_attempt_id),
    FOREIGN KEY(task_id,action_id) REFERENCES flow_actions(task_id,action_id),
    FOREIGN KEY(task_id,node_attempt_id) REFERENCES flow_node_attempts(task_id,node_attempt_id),
    CHECK (num_nonnulls(tool_invocation_id,llm_call_id)<=1)
);
CREATE TABLE IF NOT EXISTS flow_evidence_refs (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    evidence_id text NOT NULL,
    source_kind text NOT NULL,
    source_uid text NOT NULL,
    source_version text NOT NULL,
    location text NOT NULL,
    content_hash text NOT NULL,
    run_id text,
    node_attempt_id text NOT NULL,
    PRIMARY KEY(task_id,evidence_id),
    FOREIGN KEY(task_id,node_attempt_id) REFERENCES flow_node_attempts(task_id,node_attempt_id)
);
CREATE INDEX IF NOT EXISTS idx_flow_evidence_source ON flow_evidence_refs(source_kind,source_uid,source_version);
CREATE TABLE IF NOT EXISTS flow_claims (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    claim_id text NOT NULL,
    claim_revision bigint NOT NULL CHECK (claim_revision>=1),
    task_revision bigint NOT NULL,
    text text NOT NULL,
    state text NOT NULL CHECK (state IN ('proposed','accepted','stale','superseded','rejected')),
    PRIMARY KEY(task_id,claim_id,claim_revision),
    FOREIGN KEY(task_id,task_revision) REFERENCES flow_task_revisions(task_id,revision)
);
CREATE TABLE IF NOT EXISTS flow_claim_evidence (
    task_id text NOT NULL,
    claim_id text NOT NULL,
    claim_revision bigint NOT NULL,
    evidence_id text NOT NULL,
    PRIMARY KEY(task_id,claim_id,claim_revision,evidence_id),
    FOREIGN KEY(task_id,claim_id,claim_revision) REFERENCES flow_claims(task_id,claim_id,claim_revision),
    FOREIGN KEY(task_id,evidence_id) REFERENCES flow_evidence_refs(task_id,evidence_id)
);
CREATE TABLE IF NOT EXISTS flow_claim_dependencies (
    task_id text NOT NULL,
    claim_id text NOT NULL,
    claim_revision bigint NOT NULL,
    dependency_id text NOT NULL,
    dependency_revision bigint NOT NULL,
    PRIMARY KEY(task_id,claim_id,claim_revision,dependency_id,dependency_revision),
    FOREIGN KEY(task_id,claim_id,claim_revision) REFERENCES flow_claims(task_id,claim_id,claim_revision),
    FOREIGN KEY(task_id,dependency_id,dependency_revision) REFERENCES flow_claims(task_id,claim_id,claim_revision),
    CHECK (claim_id<>dependency_id)
);
CREATE INDEX IF NOT EXISTS idx_flow_claim_reverse ON flow_claim_dependencies(task_id,dependency_id,dependency_revision);
CREATE TABLE IF NOT EXISTS flow_artifacts (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    artifact_id text NOT NULL,
    version bigint NOT NULL CHECK (version>=1),
    task_revision bigint NOT NULL,
    format text NOT NULL CHECK (format IN ('json','text')),
    content bytea NOT NULL CHECK (octet_length(content)<=1048576),
    content_hash text NOT NULL,
    state text NOT NULL CHECK (state IN ('proposed','accepted','stale','rejected')),
    PRIMARY KEY(task_id,artifact_id,version),
    FOREIGN KEY(task_id,task_revision) REFERENCES flow_task_revisions(task_id,revision)
);
CREATE TABLE IF NOT EXISTS flow_artifact_claims (
    task_id text NOT NULL,
    artifact_id text NOT NULL,
    artifact_version bigint NOT NULL,
    claim_id text NOT NULL,
    claim_revision bigint NOT NULL,
    PRIMARY KEY(task_id,artifact_id,artifact_version,claim_id,claim_revision),
    FOREIGN KEY(task_id,artifact_id,artifact_version) REFERENCES flow_artifacts(task_id,artifact_id,version),
    FOREIGN KEY(task_id,claim_id,claim_revision) REFERENCES flow_claims(task_id,claim_id,claim_revision)
);
CREATE TABLE IF NOT EXISTS flow_verifications (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    verification_id text NOT NULL,
    task_revision bigint NOT NULL,
    node_attempt_id text NOT NULL,
    target_kind text NOT NULL CHECK (target_kind IN ('slot','claim','artifact')),
    target_id text NOT NULL,
    target_version bigint NOT NULL CHECK (target_version>=1),
    contract_version bigint NOT NULL,
    rule_id text NOT NULL,
    rule_version bigint NOT NULL CHECK (rule_version>=1),
    validator_kind text NOT NULL CHECK (validator_kind IN ('deterministic','evidence','judgment')),
    validator_id text NOT NULL,
    outcome text NOT NULL CHECK (outcome IN ('pass','fail','inconclusive')),
    reason text NOT NULL,
    detail jsonb NOT NULL CHECK (COALESCE(jsonb_typeof(detail)='object' AND detail->>'format_version'='1', false)),
    created_at bigint NOT NULL,
    PRIMARY KEY(task_id,verification_id),
    FOREIGN KEY(task_id,node_attempt_id) REFERENCES flow_node_attempts(task_id,node_attempt_id),
    FOREIGN KEY(task_id,contract_version) REFERENCES flow_task_contracts(task_id,contract_version)
);
CREATE INDEX IF NOT EXISTS idx_flow_verification_target ON flow_verifications(task_id,target_kind,target_id,target_version,rule_id,rule_version);
CREATE TABLE IF NOT EXISTS flow_budget_allocations (
    task_id text NOT NULL REFERENCES flow_tasks(task_id),
    root_task_id text NOT NULL REFERENCES flow_tasks(task_id),
    allocation_id text NOT NULL,
    ancestor_allocation_id text,
    run_id text NOT NULL,
    action_id text NOT NULL,
    reserved_tokens bigint NOT NULL CHECK (reserved_tokens>=0),
    reserved_tool_calls bigint NOT NULL CHECK (reserved_tool_calls>=0),
    actual_tokens bigint CHECK (actual_tokens>=0),
    actual_tool_calls bigint CHECK (actual_tool_calls>=0),
    state text NOT NULL CHECK (state IN ('pending','settled','unknown')),
    created_at bigint NOT NULL,
    settled_at bigint,
    PRIMARY KEY(root_task_id,allocation_id),
    CHECK ((state='settled')=(actual_tokens IS NOT NULL AND actual_tool_calls IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_flow_budget_task ON flow_budget_allocations(task_id,created_at,allocation_id);
-- Planned Run IDs are retained before Run admission. The actual Run links back only after it exists.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS flow_task_id text REFERENCES flow_tasks(task_id);
ALTER TABLE runs ADD COLUMN IF NOT EXISTS flow_node_attempt_id text REFERENCES flow_node_attempts(node_attempt_id);
CREATE INDEX IF NOT EXISTS idx_runs_flow_task ON runs(flow_task_id,started_at,run_id) WHERE flow_task_id IS NOT NULL;

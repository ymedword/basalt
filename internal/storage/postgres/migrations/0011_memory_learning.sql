-- US-22: append-only memory learning ledgers; times are UTC Unix milliseconds.
-- 1) Widen two closed sets; 2-6) extractions, cursors, candidate metadata, candidate
-- sources and method drafts. Existing tables and published rows are never rewritten.

ALTER TABLE memory_entries DROP CONSTRAINT IF EXISTS memory_entries_source_check;
ALTER TABLE memory_entries ADD CONSTRAINT memory_entries_source_check
    CHECK (source IN ('save_memory', 'explicit_propose', 'legacy_import', 'migration', 'auto_extract'));

ALTER TABLE llm_calls DROP CONSTRAINT IF EXISTS llm_calls_purpose_check;
ALTER TABLE llm_calls ADD CONSTRAINT llm_calls_purpose_check
    CHECK (purpose IN ('chat', 'rerank', 'summary', 'embedding', 'compaction', 'memory_extract'));

CREATE TABLE IF NOT EXISTS memory_extractions (
    extraction_id            text        PRIMARY KEY,
    agent_name               text        NOT NULL,
    session_id               text        NOT NULL,
    run_id                   text        NOT NULL UNIQUE,
    last_attempt_run_id      text        NOT NULL,
    origin_caller_id         text        NOT NULL,
    origin_key_id            text        NOT NULL DEFAULT '',
    status                   text        NOT NULL,
    window_from_message_id   bigint      NOT NULL,
    window_to_message_id     bigint      NOT NULL,
    window_messages          integer     NOT NULL,
    dropped_window_messages  integer     NOT NULL DEFAULT 0,
    prior_messages           integer     NOT NULL DEFAULT 0,
    attempts                 integer     NOT NULL DEFAULT 1,
    existing_entries         integer     NOT NULL DEFAULT 0,
    items_total              integer     NOT NULL DEFAULT 0,
    items_accepted           integer     NOT NULL DEFAULT 0,
    items_dropped            integer     NOT NULL DEFAULT 0,
    prompt_version           text        NOT NULL,
    rule_version             text        NOT NULL,
    provider                 text        NOT NULL,
    model                    text        NOT NULL,
    config_hash              text,
    failure_class            text,
    error_message            text,
    created_at               bigint      NOT NULL,
    last_attempt_at          bigint      NOT NULL,
    finished_at              bigint,
    verified_at              bigint,
    CHECK (status IN ('pending', 'success', 'failed', 'skipped', 'unknown')),
    CHECK (failure_class IS NULL OR failure_class IN
        ('model_error', 'invalid_output', 'timeout', 'audit_unavailable', 'extractor_unavailable', 'window_gone')),
    CHECK (error_message IS NULL OR length(error_message) <= 1024),
    CHECK (window_to_message_id >= window_from_message_id),
    CHECK (attempts >= 1 AND attempts <= 3)
);
CREATE INDEX IF NOT EXISTS idx_memory_extractions_agent_created
    ON memory_extractions(agent_name, created_at);
CREATE INDEX IF NOT EXISTS idx_memory_extractions_origin_status
    ON memory_extractions(agent_name, session_id, origin_caller_id, origin_key_id, status);
CREATE INDEX IF NOT EXISTS idx_memory_extractions_status
    ON memory_extractions(status);

CREATE TABLE IF NOT EXISTS memory_extraction_cursors (
    agent_name           text        NOT NULL,
    session_id           text        NOT NULL,
    origin_caller_id     text        NOT NULL,
    origin_key_id        text        NOT NULL DEFAULT '',
    committed_message_id bigint      NOT NULL DEFAULT 0,
    updated_at           bigint      NOT NULL,
    PRIMARY KEY (agent_name, session_id, origin_caller_id, origin_key_id)
);

CREATE TABLE IF NOT EXISTS memory_candidate_meta (
    entry_id                  bigint      PRIMARY KEY REFERENCES memory_entries(id),
    extraction_id             text        NOT NULL REFERENCES memory_extractions(extraction_id),
    kind                      text        NOT NULL,
    certainty                 text        NOT NULL,
    confidence                double precision NOT NULL DEFAULT 0,
    evidence_json             jsonb       NOT NULL,
    source_entry_ids          bigint[]    NOT NULL DEFAULT '{}',
    suggested_supersedes_id   bigint      REFERENCES memory_entries(id),
    origin_caller_id          text        NOT NULL,
    origin_key_id             text        NOT NULL DEFAULT '',
    evidence_at               bigint      NOT NULL,
    valid_to                  bigint,
    time_confidence           text        NOT NULL,
    created_at                bigint      NOT NULL,
    CHECK (kind IN ('fact', 'preference', 'constraint', 'inference', 'temporary')),
    CHECK (certainty IN ('asserted', 'uncertain')),
    CHECK (evidence_at >= 1),
    CHECK (time_confidence IN ('exact', 'relative_unknown', 'unknown'))
);
CREATE INDEX IF NOT EXISTS idx_memory_candidate_meta_extraction
    ON memory_candidate_meta(extraction_id);

CREATE TABLE IF NOT EXISTS memory_candidate_sources (
    entry_id        bigint  NOT NULL REFERENCES memory_entries(id),
    source_entry_id bigint  NOT NULL REFERENCES memory_entries(id),
    extraction_id   text    NOT NULL REFERENCES memory_extractions(extraction_id),
    created_at      bigint  NOT NULL,
    PRIMARY KEY (entry_id, source_entry_id),
    CHECK (entry_id <> source_entry_id)
);
CREATE INDEX IF NOT EXISTS idx_memory_candidate_sources_source
    ON memory_candidate_sources(source_entry_id);
CREATE INDEX IF NOT EXISTS idx_memory_candidate_sources_extraction
    ON memory_candidate_sources(extraction_id);

CREATE TABLE IF NOT EXISTS memory_method_drafts (
    draft_id       text        PRIMARY KEY,
    extraction_id  text        NOT NULL REFERENCES memory_extractions(extraction_id),
    agent_name     text        NOT NULL,
    name           text        NOT NULL,
    content_sha256 text        NOT NULL,
    evidence_json  jsonb       NOT NULL,
    rel_dir        text        NOT NULL,
    created_at     bigint      NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_method_drafts_extraction
    ON memory_method_drafts(extraction_id);

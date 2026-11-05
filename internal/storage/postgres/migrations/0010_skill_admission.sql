-- US-21: append-only skill admission schema; times are UTC Unix milliseconds.
-- Candidates freeze content and source; evaluations and decisions are append-only
-- evidence. Published versions are never deleted by this schema.

CREATE TABLE IF NOT EXISTS skill_admission_candidates (
    candidate_id           text PRIMARY KEY
        CHECK (candidate_id ~ '^adm_[0-9A-HJKMNP-TV-Z]{26}$'),
    skill_name             text NOT NULL CHECK (skill_name <> ''),
    state                  text NOT NULL
        CHECK (state IN ('proposed','evaluated','approved','rejected','canceled','published','withdrawn')),
    revision               bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
    candidate_hash         text NOT NULL CHECK (candidate_hash ~ '^[0-9a-f]{64}$'),
    source_hash            text NOT NULL CHECK (source_hash ~ '^[0-9a-f]{64}$'),
    manifest_json          text NOT NULL,
    source_json            text NOT NULL,
    baseline_json          text NOT NULL,
    proposer_subject       text NOT NULL
        CHECK (proposer_subject LIKE 'cli:_%' AND proposer_subject <> 'cli:unknown'),
    proposer_auth_method   text NOT NULL CHECK (proposer_auth_method = 'cli'),
    proposer_source        text NOT NULL CHECK (proposer_source = 'cli'),
    risk_tier              text NOT NULL DEFAULT 'unknown'
        CHECK (risk_tier IN ('unknown','normal','high')),
    terminal_reason        text,
    terminal_operation_id  text REFERENCES management_operations(operation_id),
    operation_id           text NOT NULL REFERENCES management_operations(operation_id),
    created_at             bigint NOT NULL,
    updated_at             bigint NOT NULL,
    CHECK (
        (state IN ('rejected','canceled','withdrawn')
            AND terminal_reason IS NOT NULL AND char_length(terminal_reason) <= 2000
            AND terminal_operation_id IS NOT NULL)
        OR
        (state NOT IN ('rejected','canceled','withdrawn')
            AND terminal_reason IS NULL AND terminal_operation_id IS NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS skill_admission_candidates_active_name
    ON skill_admission_candidates (skill_name)
    WHERE state IN ('proposed','evaluated','approved');

CREATE INDEX IF NOT EXISTS skill_admission_candidates_list
    ON skill_admission_candidates (updated_at DESC, candidate_id DESC);
CREATE INDEX IF NOT EXISTS skill_admission_candidates_operation
    ON skill_admission_candidates (operation_id);

CREATE TABLE IF NOT EXISTS skill_admission_evaluations (
    seq                    bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    evaluation_id          text PRIMARY KEY
        CHECK (evaluation_id ~ '^eval_[0-9A-HJKMNP-TV-Z]{26}$'),
    candidate_id           text NOT NULL REFERENCES skill_admission_candidates(candidate_id),
    candidate_revision     bigint NOT NULL,
    candidate_hash         text NOT NULL CHECK (candidate_hash ~ '^[0-9a-f]{64}$'),
    evaluation_hash        text NOT NULL CHECK (evaluation_hash ~ '^[0-9a-f]{64}$'),
    grader_version         text NOT NULL CHECK (grader_version <> ''),
    policy_hash            text NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    dimensions_json        text NOT NULL,
    scripts_json           text NOT NULL,
    aggregate              text NOT NULL CHECK (aggregate IN ('passed','failed','incomplete')),
    risk_tier              text NOT NULL CHECK (risk_tier IN ('normal','high')),
    referrers_json         text NOT NULL,
    baseline_json          text NOT NULL,
    evaluated_at           bigint NOT NULL,
    operation_id           text NOT NULL REFERENCES management_operations(operation_id)
);

CREATE INDEX IF NOT EXISTS skill_admission_evaluations_latest
    ON skill_admission_evaluations (candidate_id, seq DESC);
CREATE INDEX IF NOT EXISTS skill_admission_evaluations_operation
    ON skill_admission_evaluations (operation_id);

CREATE TABLE IF NOT EXISTS skill_admission_decisions (
    seq                    bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    decision_id            text PRIMARY KEY
        CHECK (decision_id ~ '^dec_[0-9A-HJKMNP-TV-Z]{26}$'),
    candidate_id           text NOT NULL REFERENCES skill_admission_candidates(candidate_id),
    candidate_revision     bigint NOT NULL,
    candidate_hash         text NOT NULL CHECK (candidate_hash ~ '^[0-9a-f]{64}$'),
    evaluation_id          text NOT NULL REFERENCES skill_admission_evaluations(evaluation_id),
    evaluation_hash        text NOT NULL CHECK (evaluation_hash ~ '^[0-9a-f]{64}$'),
    decision               text NOT NULL CHECK (decision IN ('approve','reject')),
    reason                 text NOT NULL CHECK (char_length(reason) <= 2000),
    acknowledged_json      text NOT NULL,
    approved_scope_json    text,
    script_review_json     text,
    baseline_json          text,
    risk_tier              text NOT NULL CHECK (risk_tier IN ('normal','high')),
    approver_subject       text NOT NULL
        CHECK (approver_subject LIKE 'cli:_%' AND approver_subject <> 'cli:unknown'),
    approver_auth_method   text NOT NULL CHECK (approver_auth_method = 'cli'),
    approver_source        text NOT NULL CHECK (approver_source = 'cli'),
    expires_at             bigint,
    superseded_at          bigint,
    superseded_by          text,
    created_at             bigint NOT NULL,
    operation_id           text NOT NULL REFERENCES management_operations(operation_id),
    CHECK (
        (decision = 'approve' AND approved_scope_json IS NOT NULL
            AND script_review_json IS NOT NULL
            AND baseline_json IS NOT NULL AND expires_at IS NOT NULL)
        OR
        (decision = 'reject' AND approved_scope_json IS NULL
            AND script_review_json IS NULL
            AND baseline_json IS NULL AND expires_at IS NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS skill_admission_decisions_active_approval
    ON skill_admission_decisions (candidate_id)
    WHERE decision = 'approve' AND superseded_at IS NULL;

CREATE INDEX IF NOT EXISTS skill_admission_decisions_latest
    ON skill_admission_decisions (candidate_id, seq DESC);
CREATE INDEX IF NOT EXISTS skill_admission_decisions_operation
    ON skill_admission_decisions (operation_id);

CREATE TABLE IF NOT EXISTS skill_admission_publications (
    candidate_id           text PRIMARY KEY REFERENCES skill_admission_candidates(candidate_id),
    decision_id            text NOT NULL UNIQUE REFERENCES skill_admission_decisions(decision_id),
    skill_name             text NOT NULL CHECK (skill_name <> ''),
    skill_uid              text NOT NULL CHECK (skill_uid ~ '^rs_[0-9A-HJKMNP-TV-Z]{26}$'),
    version_id             bigint NOT NULL REFERENCES resource_versions(id),
    version_no             bigint NOT NULL CHECK (version_no > 0),
    content_hash           text NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    baseline_json          text NOT NULL,
    state                  text NOT NULL DEFAULT 'active' CHECK (state IN ('active','withdrawn')),
    published_at           bigint NOT NULL,
    withdrawn_at           bigint,
    withdraw_operation_id  text REFERENCES management_operations(operation_id),
    withdraw_reason        text CHECK (withdraw_reason IN ('restored_previous','superseded','deleted')),
    withdraw_version_id    bigint REFERENCES resource_versions(id),
    operation_id           text NOT NULL REFERENCES management_operations(operation_id),
    CHECK (
        (state = 'active' AND withdrawn_at IS NULL AND withdraw_operation_id IS NULL
            AND withdraw_reason IS NULL AND withdraw_version_id IS NULL)
        OR
        (state = 'withdrawn' AND withdrawn_at IS NOT NULL
            AND withdraw_operation_id IS NOT NULL AND withdraw_reason IS NOT NULL
            AND (withdraw_reason <> 'restored_previous' OR withdraw_version_id IS NOT NULL))
    )
);

CREATE INDEX IF NOT EXISTS skill_admission_publications_operation
    ON skill_admission_publications (operation_id);
CREATE INDEX IF NOT EXISTS skill_admission_publications_withdraw_operation
    ON skill_admission_publications (withdraw_operation_id);

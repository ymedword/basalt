-- Historical cleanup reports use explicit row/group units, separate from OBS evidence.
CREATE TABLE history_retention_runs (
 id text PRIMARY KEY, group_kind text NOT NULL,
 cutoff bigint NOT NULL, occurred_at bigint NOT NULL,
 deleted_groups bigint NOT NULL CHECK (deleted_groups >= 0),
 deleted_rows bigint NOT NULL CHECK (deleted_rows >= 0),
 protected_groups bigint NOT NULL CHECK (protected_groups >= 0),
 status text NOT NULL CHECK (status IN ('succeeded','failed')),
 reason text NOT NULL DEFAULT ''
);
CREATE INDEX idx_history_retention_time ON history_retention_runs(occurred_at DESC);
CREATE INDEX idx_runs_retention_session ON runs(session_id, finished_at);
CREATE INDEX idx_sessions_retention ON sessions(last_active_at,session_id);
-- Minimal window evidence survives removal of old detailed maintenance reports.
-- Month buckets do not assert individual deletion or continuous coverage.
CREATE TABLE history_retention_rollups (
 group_kind text NOT NULL, month_bucket bigint NOT NULL,
 first_at bigint NOT NULL, last_at bigint NOT NULL,
 deleted_groups bigint NOT NULL, deleted_rows bigint NOT NULL,
 PRIMARY KEY(group_kind,month_bucket)
);

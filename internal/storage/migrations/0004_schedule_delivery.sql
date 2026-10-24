-- US-12 R7 增量迁移 v3 → v4：定时任务系统代投与资源连接测试最后结果。
-- US-12 R7 incremental migration v3 -> v4: scheduled system delivery and the last
-- connection-test projection for model connections and notify channels.
--
-- 本迁移只加列 / 建新表，不重建既有表、不改写已发布迁移。`scheduled_tasks.deliver_to`
-- 存渠道**资源 UID**（空串 = 不代投）；既有任务一律空串，行为与迁移前一致。

ALTER TABLE scheduled_tasks ADD COLUMN deliver_to TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS schedule_deliveries (
    run_id       TEXT PRIMARY KEY,
    task_id      TEXT    NOT NULL REFERENCES scheduled_tasks(task_id),
    agent_name   TEXT    NOT NULL,
    agent_uid    TEXT    NOT NULL DEFAULT '',
    channel_uid  TEXT    NOT NULL,
    channel_name TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL,
    http_status  INTEGER,
    receipt_code INTEGER,
    detail       TEXT    NOT NULL DEFAULT '',
    attempted_at INTEGER,
    finished_at  INTEGER,
    created_at   INTEGER NOT NULL,
    CHECK (status IN ('pending', 'sent', 'failed', 'unknown', 'skipped'))
);

CREATE INDEX IF NOT EXISTS idx_schedule_deliveries_task
    ON schedule_deliveries(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_schedule_deliveries_pending
    ON schedule_deliveries(status) WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS resource_connection_tests (
    resource_kind TEXT    NOT NULL,
    resource_uid  TEXT    NOT NULL,
    tested_at     INTEGER NOT NULL,
    status        TEXT    NOT NULL,
    detail        TEXT    NOT NULL DEFAULT '',
    operation_id  TEXT    NOT NULL,
    PRIMARY KEY (resource_kind, resource_uid),
    CHECK (resource_kind IN ('model_connection', 'notify_channel')),
    CHECK (status IN ('success', 'failed', 'unknown'))
);

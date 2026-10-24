-- US-12 R8 增量迁移 v4 → v5：网络域名白名单的唯一权威源。
-- US-12 R8 incremental migration v4 -> v5: the single source of truth for the
-- outbound network domain allowlist.
--
-- 本迁移只新建一张单行表，不重建既有表、不改写已发布迁移。既有库升级后表内无行，
-- 首次启动按 `basalt.yaml` 的 `http.allowed_domains` 写入种子；此后运行期与重启都
-- 以本表为准。

CREATE TABLE IF NOT EXISTS network_domain_policy (
    policy_scope TEXT PRIMARY KEY,            -- 固定 http.allowed_domains
    domains      TEXT    NOT NULL,            -- 规整域名，换行连接；空串 = 空列表
    revision     INTEGER NOT NULL,            -- 从 1 起，每次成功写入 +1
    updated_at   INTEGER NOT NULL,            -- 最近一次写入时间（Unix 毫秒，UTC）
    operation_id TEXT    NOT NULL DEFAULT ''  -- 产生该修订的管理操作；种子为空串
);

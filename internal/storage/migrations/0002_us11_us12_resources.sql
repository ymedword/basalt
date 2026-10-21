-- US-11 / US-12 增量迁移 v1 → v2：管理操作、资源版本/指针、运行资源清单/引用、生命周期与归档索引。
-- US-11 / US-12 incremental migration v1 -> v2: management operations, resource
-- versions/pointers, run resource manifests/refs, lifecycle and archive indexes.
-- 只新增表与索引，全部 IF NOT EXISTS；不改既有表、不删列、不重建（documented in
-- docs/contracts/schema.sql v2 section）。

-- ============================================================
-- 管理操作审计（US-11 / US-12；v2 新增）
-- ============================================================
-- 一次管理面高权限变更 = 一行。先插 status=pending 再执行；预写失败不得执行（fail-closed）。
-- 该表**不**挂 run_id：管理变更不是模型 / 工具调用，不得混写，也不得伪造一次 Run 来承载审计。
-- 恢复时不盲目重做：按 action 查证目标的实际状态，再写 success / failed / unknown（run-and-errors.md §9.4）。
-- token 只记录不可恢复的指纹（token:<sha256 前 16 位十六进制>），明文 token 不得入库。

CREATE TABLE IF NOT EXISTS management_operations (
    operation_id        TEXT PRIMARY KEY,          -- op-<unix_millis>-<6 位小写字母数字>
    action              TEXT NOT NULL,             -- agent.content.update / model_connection.test / system.baseline_v2 ...
    target_kind         TEXT NOT NULL,             -- agent / schedule / model_connection / mcp_server / skill / notify_channel / system
    target_id           TEXT NOT NULL,             -- 资源名 / Agent 名 / system
    actor_subject       TEXT NOT NULL,             -- token:<指纹> / local_trust / cli:<本机用户> / migration
    auth_method         TEXT NOT NULL,             -- bearer / local_trust / cli / migration
    source              TEXT NOT NULL,             -- <ip>:<port> / cli / migration；转发头只在可信代理后用于归因
    request_hash        TEXT,                      -- 脱敏请求摘要（sha256）；不含凭证原文
    requested_version_id INTEGER,                  -- 发布 / 撤回的目标版本（无则为空）
    expected_etag       TEXT,                      -- 条件写期望值（无则为空）
    status              TEXT NOT NULL,             -- pending / success / failed / unknown
    result_code         TEXT,                      -- OK 或错误码表中的 code
    error_message       TEXT,                      -- 失败原文（截断 4 KB；不得含凭证）
    detail_json         TEXT,                      -- 脱敏详情：变更摘要、测试结果、token 用量等
    created_at          INTEGER NOT NULL,
    finished_at         INTEGER,
    recovered_at        INTEGER,                   -- 启动恢复查证时间；正常完成为空
    verification        TEXT,                      -- applied / not_applied / indeterminate（仅恢复时写）
    CHECK (status IN ('pending', 'success', 'failed', 'unknown')),
    CHECK (auth_method IN ('bearer', 'local_trust', 'cli', 'migration')),
    CHECK (verification IS NULL OR verification IN ('applied', 'not_applied', 'indeterminate'))
);

CREATE INDEX IF NOT EXISTS idx_management_operations_pending
    ON management_operations(status, created_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_management_operations_target
    ON management_operations(target_kind, target_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_management_operations_created
    ON management_operations(created_at DESC);

-- ============================================================
-- 资源版本与发布指针（Provider / MCP / 技能；v2 新增）
-- ============================================================
-- 定义内容在文件（basalt.yaml / mcp_servers.yaml / 技能工作副本），已提交版本、当前发布指针与
-- Run 引用在 SQLite。版本表是**不可变历史**，不是第二套可编辑定义：
--   - model_connection / mcp_server：definition_yaml = 该资源块的原文（含 ${ENV_VAR} 引用，不含解析后的值）
--   - skill：manifest_json + storage_path = 完整文件清单与不可变快照目录
-- content_hash 对 model/MCP 是 definition_yaml 的 sha256；对技能是文件清单规整序列化的 sha256
-- （与 config_snapshots.hash 同一规整化规则）。版本号逐资源从 1 单调递增；撤回 = 用旧内容创建新版本。

CREATE TABLE IF NOT EXISTS resource_versions (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,  -- version_id（跨资源全局稳定）
    resource_kind   TEXT    NOT NULL,                   -- model_connection / mcp_server / skill
    resource_id     TEXT    NOT NULL,                   -- provider / server / 技能名
    version_no      INTEGER NOT NULL,                   -- 1 起，逐资源单调递增
    content_hash    TEXT    NOT NULL,                   -- sha256（十六进制小写）
    definition_yaml TEXT,                               -- model_connection / mcp_server 的资源块原文
    manifest_json   TEXT,                               -- skill 的完整清单 [{path, sha256, size}]
    storage_path    TEXT,                               -- skill 不可变快照目录（工作区相对路径）
    created_at      INTEGER NOT NULL,
    operation_id    TEXT    NOT NULL REFERENCES management_operations(operation_id),
    UNIQUE (resource_kind, resource_id, version_no),
    CHECK (resource_kind IN ('model_connection', 'mcp_server', 'skill')),
    CHECK (version_no >= 1),
    CHECK (
        (resource_kind IN ('model_connection', 'mcp_server') AND definition_yaml IS NOT NULL)
        OR (resource_kind = 'skill' AND manifest_json IS NOT NULL AND storage_path IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_resource_versions_lookup
    ON resource_versions(resource_kind, resource_id, version_no DESC);
CREATE INDEX IF NOT EXISTS idx_resource_versions_hash
    ON resource_versions(resource_kind, resource_id, content_hash);

-- 当前发布指针（每个资源一行；删除后保留行并置 deleted_at，历史版本不物理删）。
-- published_version_id = 最近一次发布的版本；applied_version_id = 运行时实际生效的版本。
-- Provider / 技能发布后对新 Run 立即生效（两者相等）；MCP 首版需重启，重启前 applied 仍指旧版本。
CREATE TABLE IF NOT EXISTS resource_pointers (
    resource_kind        TEXT    NOT NULL,
    resource_id          TEXT    NOT NULL,
    published_version_id INTEGER NOT NULL REFERENCES resource_versions(id),
    applied_version_id   INTEGER REFERENCES resource_versions(id),
    deleted_at           INTEGER,
    updated_at           INTEGER NOT NULL,
    operation_id         TEXT    NOT NULL REFERENCES management_operations(operation_id),
    PRIMARY KEY (resource_kind, resource_id),
    CHECK (resource_kind IN ('model_connection', 'mcp_server', 'skill'))
);

CREATE INDEX IF NOT EXISTS idx_resource_pointers_applied
    ON resource_pointers(resource_kind, applied_version_id);

-- ============================================================
-- Run 资源清单与引用（v2 新增）
-- ============================================================
-- RunResourceManifest 的持久形态：一次 Run 一行，snapshot_json 记录准入时固定的
-- Provider 版本、MCP 实际生效版本、每个技能版本与完整 L1/L2/L3 文件哈希、Agent 生命周期状态。
-- 历史 Run 没有本行 → API 标 legacy_unrecorded，不得用当前版本补造。
-- runs.config_hash 的原义不变（仍是 ConfigSnapshot 哈希）；本表不覆盖、不改写它。

CREATE TABLE IF NOT EXISTS run_resource_manifests (
    run_id                TEXT PRIMARY KEY REFERENCES runs(run_id),
    format_version        INTEGER NOT NULL DEFAULT 1,
    manifest_hash         TEXT    NOT NULL,          -- snapshot_json 规整序列化的 sha256
    agent_lifecycle_state TEXT    NOT NULL,          -- 准入时的生命周期状态（只可能为 active）
    snapshot_json         TEXT    NOT NULL,          -- RunResourceManifest 完整 JSON
    created_at            INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_run_resource_manifests_hash
    ON run_resource_manifests(manifest_hash);

-- 版本级引用：活动 Run 持有引用，最后一个引用释放前版本不得回收。
-- Run 进入终态时在同一事务内写 released_at；崩溃留下的未释放引用由启动恢复按终态 Run 补齐。
-- 行保留作历史证据，不因 released_at 非空而删除。
CREATE TABLE IF NOT EXISTS run_resource_refs (
    run_id         TEXT    NOT NULL REFERENCES runs(run_id),
    resource_kind  TEXT    NOT NULL,
    resource_id    TEXT    NOT NULL,
    version_id     INTEGER NOT NULL REFERENCES resource_versions(id),
    content_hash   TEXT    NOT NULL,
    released_at    INTEGER,
    PRIMARY KEY (run_id, resource_kind, resource_id),
    CHECK (resource_kind IN ('model_connection', 'mcp_server', 'skill'))
);

CREATE INDEX IF NOT EXISTS idx_run_resource_refs_version
    ON run_resource_refs(version_id, released_at);
CREATE INDEX IF NOT EXISTS idx_run_resource_refs_unreleased
    ON run_resource_refs(released_at) WHERE released_at IS NULL;

-- ============================================================
-- Agent 生命周期索引与归档索引（v2 新增）
-- ============================================================
-- 权威是工作区受保护管理目录中的状态文件（.basalt/control/agents/<name>.json），本表只是
-- 可重建索引。状态文件缺失 / 校验和损坏 → state 为 NULL、valid=0，拒绝新 Run；不得悄悄激活。
-- archived 的 Agent 不在活动注册表；恢复走 agent_archives 的受控 restore，默认 disabled。

CREATE TABLE IF NOT EXISTS agent_lifecycle (
    agent_name      TEXT PRIMARY KEY,
    state           TEXT,                         -- active / disabled / archived；NULL = 状态文件缺失或损坏
    generation      INTEGER NOT NULL DEFAULT 0,   -- 状态文件中的单调代数；0 = 无有效状态
    state_file_hash TEXT,                         -- 状态文件校验和（sha256 十六进制）
    valid           INTEGER NOT NULL DEFAULT 0,   -- 1 = 状态文件存在且校验通过
    error_message   TEXT,                         -- 无效时的可读原因（点名字段 / 相对路径，不含凭证）
    updated_at      INTEGER NOT NULL,
    operation_id    TEXT REFERENCES management_operations(operation_id),
    CHECK (state IS NULL OR state IN ('active', 'disabled', 'archived')),
    CHECK (valid IN (0, 1))
);

CREATE INDEX IF NOT EXISTS idx_agent_lifecycle_state
    ON agent_lifecycle(state, agent_name);

CREATE TABLE IF NOT EXISTS agent_archives (
    archive_id          TEXT PRIMARY KEY,          -- 归档目录名：<agent>-YYYYMMDD-HHMMSS
    agent_name          TEXT    NOT NULL,          -- 归档前的 Agent 名
    archived_at         INTEGER NOT NULL,
    content_hash        TEXT    NOT NULL,          -- 归档目录清单的 sha256
    manifest_json       TEXT    NOT NULL,          -- 归档目录清单 [{path, sha256, size}]
    operation_id        TEXT    NOT NULL REFERENCES management_operations(operation_id),
    restored_at         INTEGER,
    restored_agent_name TEXT
);

CREATE INDEX IF NOT EXISTS idx_agent_archives_agent
    ON agent_archives(agent_name, archived_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_archives_restorable
    ON agent_archives(archived_at DESC) WHERE restored_at IS NULL;

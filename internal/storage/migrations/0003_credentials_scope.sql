-- US-12 范围修订增量迁移 v2 → v3：UID 级资源身份与墓碑、使用范围、托管凭据与凭据版本、
-- Agent 资源绑定投影、通知 Run 引用与发送明细。
-- US-12 scope revision incremental migration v2 -> v3: UID-level resource identity and
-- tombstones, usage scope, managed credentials and credential versions, agent resource
-- binding projection, notify run refs and delivery details.
--
-- 顺序与纪律（docs/contracts/schema.sql「增量迁移 v3」段）：
--   - 迁移器在事务外 PRAGMA foreign_keys=OFF，单事务内完成 DDL 与表重建，
--     PRAGMA foreign_key_check 通过才提交（本文件自身不开关外键、不写 schema_version——
--     版本行由迁移机制统一记录，保持「版本记录单一写者」）；
--   - 表重建只出现在本**未发布**迁移里：建新表 → 复制（保留原 id）→ drop → rename；
--   - 资源 UID 只在这里分配一次：resource_pointers 与 notify_channels 各用 rowid 派生
--     （载荷 25 位零填充十进制 + 表别后缀 1/2，共 26 位、字符集 ⊆ Crockford Base32）；
--     迁移后同名重建生成新 UID，旧 UID 的墓碑留在 resource_identities；
--   - Agent UID 不在这里生成：身份以权威状态文件为准，由启动编排在**本迁移之后**回填
--     （agent_lifecycle / agent_archives 的列在本文件里先加好，见契约 3g → 2c 的顺序）；
--   - 同一名称在迁移前经历删除 / 重建循环时，旧版本无法可靠拆分归属：全部归入当前指针
--     UID（契约 3h），不猜测拆分、不改写历史内容。这是满足新表 NOT NULL 约束的**兼容规则**，
--     不是对历史身份连续性的证明——因此 run_resource_refs 一律不回填 UID（本文件 §7）。

-- ============================================================
-- 1. 资源 UID 级身份与墓碑（v3 新增）
-- ============================================================

CREATE TABLE IF NOT EXISTS resource_identities (
    resource_uid  TEXT PRIMARY KEY,               -- rs_ + 26 位 Crockford Base32（ULID 形态）；创建后不可变
    resource_kind TEXT NOT NULL,                  -- model_connection / notify_channel / mcp_server / skill
    name          TEXT NOT NULL,                  -- 创建时名称；删除后保留最后名称供历史检索
    state         TEXT NOT NULL,                  -- active / deleted
    created_at    INTEGER NOT NULL,
    deleted_at    INTEGER,
    operation_id  TEXT NOT NULL,                  -- 创建该身份的管理操作；迁移建立时记 system.baseline_v3 的操作
    CHECK (resource_kind IN ('model_connection', 'notify_channel', 'mcp_server', 'skill')),
    CHECK (state IN ('active', 'deleted')),
    CHECK (
        (state = 'active' AND deleted_at IS NULL)
        OR (state = 'deleted' AND deleted_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_resource_identities_kind_name
    ON resource_identities(resource_kind, name, created_at);

-- ============================================================
-- 2. Agent 资源绑定投影（v3 新增；可重建投影，权威是 AGENT.md）
-- ============================================================

CREATE TABLE IF NOT EXISTS agent_resource_bindings (
    agent_uid     TEXT NOT NULL,
    agent_name    TEXT NOT NULL,
    resource_kind TEXT NOT NULL,                  -- model_connection / notify_channel
    ref_name      TEXT NOT NULL,                  -- AGENT.md 中声明的名称（标签，可随改名变化）
    resource_uid  TEXT NOT NULL,
    content_hash  TEXT NOT NULL,                  -- 产生该绑定的 AGENT.md 内容哈希
    projected_at  INTEGER NOT NULL,
    PRIMARY KEY (agent_uid, resource_kind, ref_name),
    CHECK (resource_kind IN ('model_connection', 'notify_channel'))
);

CREATE INDEX IF NOT EXISTS idx_agent_resource_bindings_uid
    ON agent_resource_bindings(resource_uid);
CREATE INDEX IF NOT EXISTS idx_agent_resource_bindings_name
    ON agent_resource_bindings(agent_name);

-- ============================================================
-- 3. 托管凭据身份与不可变凭据版本（v3 新增）
-- ============================================================

CREATE TABLE IF NOT EXISTS credentials (
    credential_id    TEXT PRIMARY KEY,            -- cred_ + 26 位 Crockford Base32（ULID 形态）
    owner_kind       TEXT    NOT NULL,            -- model_connection / notify_channel
    owner_uid        TEXT    NOT NULL,            -- 资源 UID
    owner_name       TEXT    NOT NULL,            -- 展示用名称
    state            TEXT    NOT NULL,            -- active / revoked / retired
    current_version  INTEGER NOT NULL,            -- 当前活动凭据版本号
    revocation_epoch INTEGER NOT NULL DEFAULT 0,  -- 每次撤销 +1；参与资源 ETag
    key_id           TEXT    NOT NULL,            -- 当前版本使用的主密钥标识；主密钥本身不在库中
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    operation_id     TEXT    NOT NULL REFERENCES management_operations(operation_id),
    UNIQUE (owner_kind, owner_uid),
    CHECK (owner_kind IN ('model_connection', 'notify_channel')),
    CHECK (state IN ('active', 'revoked', 'retired')),
    CHECK (current_version >= 1)
);

CREATE TABLE IF NOT EXISTS credential_versions (
    credential_id  TEXT    NOT NULL REFERENCES credentials(credential_id),
    version_no     INTEGER NOT NULL,              -- 1 起，逐凭据身份单调递增
    state          TEXT    NOT NULL,              -- active / superseded / revoked / retired
    key_id         TEXT    NOT NULL,
    target_binding TEXT    NOT NULL,
    last4          TEXT    NOT NULL DEFAULT '',
    nonce          BLOB    NOT NULL,              -- 12 字节随机数
    ciphertext     BLOB,                          -- 解出的 AES-256-GCM 密文（尾部含 16 字节 tag）；revoked 时为 NULL
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    operation_id   TEXT    NOT NULL REFERENCES management_operations(operation_id),
    PRIMARY KEY (credential_id, version_no),
    CHECK (version_no >= 1),
    CHECK (state IN ('active', 'superseded', 'revoked', 'retired')),
    CHECK (length(nonce) = 12),
    CHECK (
        (state = 'revoked' AND ciphertext IS NULL)
        OR (state IN ('active', 'superseded', 'retired') AND ciphertext IS NOT NULL)
    )
);

CREATE TABLE IF NOT EXISTS resource_version_credentials (
    version_id       INTEGER NOT NULL REFERENCES resource_versions(id),
    credential_id    TEXT    NOT NULL,
    resolved_version INTEGER NOT NULL,
    resolved_at      INTEGER NOT NULL,
    PRIMARY KEY (version_id, credential_id),
    CHECK (resolved_version >= 1)
);

CREATE INDEX IF NOT EXISTS idx_resource_version_credentials_cred
    ON resource_version_credentials(credential_id, resolved_version);

-- ============================================================
-- 4. Run 凭据 / 通知引用与发送明细（v3 新增）
-- ============================================================

CREATE TABLE IF NOT EXISTS run_credential_refs (
    run_id             TEXT    NOT NULL REFERENCES runs(run_id),
    credential_id      TEXT    NOT NULL,
    credential_version INTEGER NOT NULL,
    resource_kind      TEXT    NOT NULL,
    resource_uid       TEXT    NOT NULL,
    resource_name      TEXT    NOT NULL,
    released_at        INTEGER,
    PRIMARY KEY (run_id, credential_id, resource_kind, resource_uid),
    CHECK (resource_kind = 'model_connection'),
    CHECK (credential_version >= 1)
);

CREATE INDEX IF NOT EXISTS idx_run_credential_refs_unreleased
    ON run_credential_refs(released_at) WHERE released_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_run_credential_refs_uid
    ON run_credential_refs(resource_uid, released_at);

CREATE TABLE IF NOT EXISTS run_notify_refs (
    run_id       TEXT    NOT NULL REFERENCES runs(run_id),
    agent_uid    TEXT    NOT NULL,
    channel_uid  TEXT    NOT NULL,
    channel_name TEXT    NOT NULL,
    released_at  INTEGER,
    PRIMARY KEY (run_id, agent_uid, channel_uid)
);

CREATE INDEX IF NOT EXISTS idx_run_notify_refs_channel
    ON run_notify_refs(channel_uid, released_at);
CREATE INDEX IF NOT EXISTS idx_run_notify_refs_unreleased
    ON run_notify_refs(released_at) WHERE released_at IS NULL;

CREATE TABLE IF NOT EXISTS notify_deliveries (
    tool_invocation_id INTEGER PRIMARY KEY REFERENCES tool_invocations(id),
    run_id             TEXT    NOT NULL REFERENCES runs(run_id),
    agent_uid          TEXT    NOT NULL,
    channel_uid        TEXT    NOT NULL,
    channel_name       TEXT    NOT NULL,
    credential_id      TEXT,
    credential_version INTEGER,
    target_host        TEXT    NOT NULL DEFAULT '',  -- 解析不出 host 时为空串
    resolved_at        INTEGER NOT NULL,
    CHECK (credential_version IS NULL OR credential_version >= 1)
);

CREATE INDEX IF NOT EXISTS idx_notify_deliveries_run ON notify_deliveries(run_id, tool_invocation_id);
CREATE INDEX IF NOT EXISTS idx_notify_deliveries_channel ON notify_deliveries(channel_uid, resolved_at DESC);

-- ============================================================
-- 5. resource_pointers：先加列（契约 3d），再分配资源 UID（3b）
-- ============================================================
-- resource_uid 可空只因 SQLite 的 ALTER ADD COLUMN 无法带 NOT NULL 约束；回填后必然有值。

ALTER TABLE resource_pointers ADD COLUMN resource_uid TEXT;
ALTER TABLE resource_pointers ADD COLUMN scope TEXT NOT NULL DEFAULT 'shared'
    CHECK (scope IN ('exclusive', 'shared'));
ALTER TABLE resource_pointers ADD COLUMN owner_agent_uid TEXT
    CHECK ((scope = 'exclusive') = (owner_agent_uid IS NOT NULL));
ALTER TABLE resource_pointers ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;

-- UID 派生：rs_ + 25 位零填充 rowid + 表别后缀，共 26 位载荷，字符集 ⊆ Crockford Base32。
UPDATE resource_pointers
   SET resource_uid = 'rs_' || printf('%025d', rowid) || '1';

-- ============================================================
-- 6. resource_versions 重建：加 resource_uid，唯一键换 (kind, uid, version_no)
-- ============================================================
-- **保留原 id**（version_id 全局稳定：run_resource_refs / resource_pointers 的引用不换号）。
-- 找不到同名指针行时 resource_uid 为 NULL → 新表 NOT NULL 约束整条中止、事务回滚并拒绝启动，
-- 不静默丢版本、不猜归属。

CREATE TABLE resource_versions_v3 (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,  -- version_id（跨资源全局稳定）
    resource_kind   TEXT    NOT NULL,                   -- model_connection / mcp_server / skill
    resource_uid    TEXT    NOT NULL,                   -- 资源实例身份（名称不参与版本键）
    resource_id     TEXT    NOT NULL,                   -- 名称快照（仅展示与检索）
    version_no      INTEGER NOT NULL,                   -- 1 起，逐资源 UID 单调递增
    content_hash    TEXT    NOT NULL,                   -- sha256（十六进制小写）
    definition_yaml TEXT,                               -- model_connection / mcp_server 的资源块原文
    manifest_json   TEXT,                               -- skill 的完整清单 [{path, sha256, size}]
    storage_path    TEXT,                               -- skill 不可变快照目录（工作区相对路径）
    created_at      INTEGER NOT NULL,
    operation_id    TEXT    NOT NULL REFERENCES management_operations(operation_id),
    UNIQUE (resource_kind, resource_uid, version_no),
    CHECK (resource_kind IN ('model_connection', 'mcp_server', 'skill')),
    CHECK (version_no >= 1),
    CHECK (
        (resource_kind IN ('model_connection', 'mcp_server') AND definition_yaml IS NOT NULL)
        OR (resource_kind = 'skill' AND manifest_json IS NOT NULL AND storage_path IS NOT NULL)
    )
);

INSERT INTO resource_versions_v3 (
    id, resource_kind, resource_uid, resource_id, version_no, content_hash,
    definition_yaml, manifest_json, storage_path, created_at, operation_id
)
SELECT v.id,
       v.resource_kind,
       (SELECT p.resource_uid FROM resource_pointers p
         WHERE p.resource_kind = v.resource_kind AND p.resource_id = v.resource_id),
       v.resource_id,
       v.version_no,
       v.content_hash,
       v.definition_yaml,
       v.manifest_json,
       v.storage_path,
       v.created_at,
       v.operation_id
  FROM resource_versions v;

DROP TABLE resource_versions;
ALTER TABLE resource_versions_v3 RENAME TO resource_versions;

CREATE INDEX IF NOT EXISTS idx_resource_versions_lookup
    ON resource_versions(resource_kind, resource_uid, version_no DESC);
CREATE INDEX IF NOT EXISTS idx_resource_versions_hash
    ON resource_versions(resource_kind, resource_uid, content_hash);
CREATE INDEX IF NOT EXISTS idx_resource_versions_name
    ON resource_versions(resource_kind, resource_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_resource_pointers_uid
    ON resource_pointers(resource_uid);

-- 资源身份（指针侧）：state 由 deleted_at 是否有值决定；created_at 取首版创建时间，
-- 无版本时退回指针 updated_at。operation_id 记 system.baseline_v3 的操作（缺失 → NOT NULL 中止）。
INSERT INTO resource_identities (resource_uid, resource_kind, name, state, created_at, deleted_at, operation_id)
SELECT p.resource_uid,
       p.resource_kind,
       p.resource_id,
       CASE WHEN p.deleted_at IS NULL THEN 'active' ELSE 'deleted' END,
       COALESCE(
           (SELECT MIN(v.created_at) FROM resource_versions v
             WHERE v.resource_kind = p.resource_kind AND v.resource_uid = p.resource_uid),
           p.updated_at
       ),
       p.deleted_at,
       (SELECT operation_id FROM management_operations
         WHERE action = 'system.baseline_v3'
         ORDER BY created_at DESC LIMIT 1)
  FROM resource_pointers p;

-- ============================================================
-- 7. run_resource_refs：加 resource_uid / resource_name（契约 3e）
-- ============================================================
-- 归属规则：v2 的 run_resource_refs 只有名称与版本 id，resource_pointers 只有当前行、
-- 没有历史。「同名指针当前活动」**不能**证明旧 Run 属于该实例——反例：旧资源执行过
-- Run → 删除 → 同名重建 → 新指针当前活动，此时当前活动指向的是新实例。因此迁移**不**
-- 按名称回填 UID：resource_uid 保持 NULL = legacy_unrecorded（逐行标记，不另加列，表结构
-- 以契约 schema.sql 为准）；resource_name 记名称快照，供按名称检索历史引用。新 Run 在准入
-- 时记录完整 UID，不沿用历史缺失例外。
--
-- 与 R4.10 的偏差（复核 L-1 / U19，已接受）：卡片允许再用「名称 + 创建时间」证明连续性，
-- 但 v2 的 resource_pointers 只有当前行、没有历史；同名重建复用同一行的前提下，创建时间
-- 也无法把旧引用与重建前的实例区分开。因此采用更保守的口径——一律 NULL，不猜。
-- Accepted deviation: v2 rows carry no continuity evidence, so every legacy ref stays
-- NULL (legacy_unrecorded) instead of a guessed uid.

ALTER TABLE run_resource_refs ADD COLUMN resource_uid TEXT;
ALTER TABLE run_resource_refs ADD COLUMN resource_name TEXT;

UPDATE run_resource_refs
   SET resource_name = resource_id;

CREATE INDEX IF NOT EXISTS idx_run_resource_refs_uid
    ON run_resource_refs(resource_uid, released_at);

-- ============================================================
-- 8. notify_channels 重建：主键换资源 UID（契约 3f）
-- ============================================================
-- 既有行按 env 模式复制（url 保留原 ${ENV_VAR} 引用）、scope='shared'、revision=1、
-- 无凭据身份、未删除。url_host 留空串：地址解析归运行侧唯一实现（迁移不写第二份 URL
-- 解析器，也绝不把整条地址塞进展示列）。

CREATE TABLE notify_channels_v3 (
    resource_uid    TEXT PRIMARY KEY,
    name            TEXT    NOT NULL,             -- 当前标签；活动名称唯一由部分唯一索引保证
    type            TEXT    NOT NULL,             -- webhook / feishu（装配层显式映射的键）
    url             TEXT    NOT NULL DEFAULT '',  -- env 模式：精确 ${ENV_VAR} 引用；managed 模式：空串
    url_host        TEXT    NOT NULL DEFAULT '',  -- 展示用 host；不含 path / query；解析不出时为空串
    credential_mode TEXT    NOT NULL DEFAULT 'env',
    scope           TEXT    NOT NULL CHECK (scope IN ('exclusive', 'shared')),
    owner_agent_uid TEXT,                         -- scope=exclusive 时的属主 Agent UID；shared 为 NULL
    credential_id   TEXT,                         -- managed 模式的当前凭据身份；env 模式为 NULL
    revision        INTEGER NOT NULL DEFAULT 1,   -- 每次地址 / 凭据 / 范围变更 +1；参与资源 ETag
    description     TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    deleted_at      INTEGER,
    operation_id    TEXT    NOT NULL,
    CHECK (credential_mode IN ('env', 'managed')),
    CHECK ((scope = 'exclusive') = (owner_agent_uid IS NOT NULL))
);

INSERT INTO notify_channels_v3 (
    resource_uid, name, type, url, url_host, credential_mode, scope, owner_agent_uid,
    credential_id, revision, description, created_at, updated_at, deleted_at, operation_id
)
SELECT 'rs_' || printf('%025d', rowid) || '2',
       name,
       type,
       url,
       '',
       'env',
       'shared',
       NULL,
       NULL,
       1,
       COALESCE(description, ''),
       created_at,
       created_at,
       NULL,
       (SELECT operation_id FROM management_operations
         WHERE action = 'system.baseline_v3'
         ORDER BY created_at DESC LIMIT 1)
  FROM notify_channels;

DROP TABLE notify_channels;
ALTER TABLE notify_channels_v3 RENAME TO notify_channels;

CREATE UNIQUE INDEX IF NOT EXISTS ux_notify_channels_active_name
    ON notify_channels(name) WHERE deleted_at IS NULL;

INSERT INTO resource_identities (resource_uid, resource_kind, name, state, created_at, deleted_at, operation_id)
SELECT c.resource_uid,
       'notify_channel',
       c.name,
       'active',
       c.created_at,
       NULL,
       (SELECT operation_id FROM management_operations
         WHERE action = 'system.baseline_v3'
         ORDER BY created_at DESC LIMIT 1)
  FROM notify_channels c;

-- ============================================================
-- 9. Agent 身份列（契约 3g）：只加列，回填由启动编排在迁移之后按状态文件执行（2c）
-- ============================================================
-- agent_lifecycle.uid_source：state_file / legacy_unrecorded；NULL = 尚未对账。
-- agent_archives.uid_source：archive / legacy_unrecorded（既有行默认后者，回填时按证据改写）。

ALTER TABLE agent_lifecycle ADD COLUMN agent_uid TEXT;
ALTER TABLE agent_lifecycle ADD COLUMN uid_source TEXT
    CHECK (uid_source IS NULL OR uid_source IN ('state_file', 'legacy_unrecorded'))
    CHECK (uid_source IS NULL OR uid_source <> 'state_file' OR agent_uid IS NOT NULL);

CREATE INDEX IF NOT EXISTS idx_agent_lifecycle_uid
    ON agent_lifecycle(agent_uid) WHERE agent_uid IS NOT NULL;

ALTER TABLE agent_archives ADD COLUMN agent_uid TEXT;
ALTER TABLE agent_archives ADD COLUMN uid_source TEXT NOT NULL DEFAULT 'legacy_unrecorded'
    CHECK (uid_source IN ('archive', 'legacy_unrecorded'))
    CHECK ((uid_source = 'archive') = (agent_uid IS NOT NULL));

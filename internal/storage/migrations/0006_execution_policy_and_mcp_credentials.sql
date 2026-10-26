-- US-14 14D 增量迁移 v5 → v6：执行档位、访问例外、实例密钥记录与 MCP 每键凭据来源。
-- US-14 14D incremental migration v5 -> v6: execution profile, network exceptions,
-- instance secret records and per-key MCP credential sources.
--
-- 顺序与纪律（docs/contracts/schema.sql「增量迁移 v6」段）：
--   - 迁移器在事务外 PRAGMA foreign_keys=OFF，单事务内完成 DDL 与表重建，
--     PRAGMA foreign_key_check 通过才提交（本文件自身不开关外键、不写 schema_version——
--     版本行由迁移机制统一记录，保持「版本记录单一写者」）；
--   - 表重建只出现在本**未发布**迁移里：建新表 → 复制（保留原主键）→ drop → rename；
--   - 本迁移**不新增任何授权**：execution_policy 一律写 strict，不看白名单内容、空白名单
--     也不放宽；MCP 凭据来源不在这里补行（MCP server 定义在 mcp_servers.yaml，不在库里，
--     SQL 无法枚举占位符），由启动编排按文件事实补记 mode=env（见 §2.5.2 与 T094）。

-- ============================================================
-- 1. credentials 重建：owner_kind 加 mcp_server、新增 binding_key、唯一键改三元组
-- ============================================================
-- 一个 MCP server 可有多个占位符，各自独立版本与撤销；binding_key 是占位符名。
-- 非 mcp_server 行 binding_key 必须为空串——三种 kind 共用一张表，但键空间不混用。
-- **保留原 credential_id**：credential_versions / resource_version_credentials /
-- run_credential_refs 的引用不换号。

CREATE TABLE credentials_v6 (
    credential_id    TEXT PRIMARY KEY,            -- cred_ + 26 位 Crockford Base32（ULID 形态）
    owner_kind       TEXT    NOT NULL,            -- model_connection / notify_channel / mcp_server
    owner_uid        TEXT    NOT NULL,            -- 资源 UID
    binding_key      TEXT    NOT NULL DEFAULT '', -- MCP 占位符名；其它 kind 固定空串
    owner_name       TEXT    NOT NULL,            -- 展示用名称
    state            TEXT    NOT NULL,            -- active / revoked / retired
    current_version  INTEGER NOT NULL,            -- 当前活动凭据版本号
    revocation_epoch INTEGER NOT NULL DEFAULT 0,  -- 每次撤销 +1；参与资源 ETag
    key_id           TEXT    NOT NULL,            -- 当前版本使用的主密钥标识；主密钥本身不在库中
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    operation_id     TEXT    NOT NULL REFERENCES management_operations(operation_id),
    UNIQUE (owner_kind, owner_uid, binding_key),
    CHECK (owner_kind IN ('model_connection', 'notify_channel', 'mcp_server')),
    CHECK (state IN ('active', 'revoked', 'retired')),
    CHECK (current_version >= 1),
    CHECK (
        (owner_kind = 'mcp_server' AND binding_key <> '')
        OR (owner_kind <> 'mcp_server' AND binding_key = '')
    )
);

INSERT INTO credentials_v6 (
    credential_id, owner_kind, owner_uid, binding_key, owner_name, state,
    current_version, revocation_epoch, key_id, created_at, updated_at, operation_id
)
SELECT c.credential_id,
       c.owner_kind,
       c.owner_uid,
       '',
       c.owner_name,
       c.state,
       c.current_version,
       c.revocation_epoch,
       c.key_id,
       c.created_at,
       c.updated_at,
       c.operation_id
  FROM credentials c;

DROP TABLE credentials;
ALTER TABLE credentials_v6 RENAME TO credentials;

-- ============================================================
-- 2. execution_policy：执行档位落库（YAML 只作种子）
-- ============================================================
-- 单行表，形态与 network_domain_policy 一致。升级一律 strict：存量实例不得因为
-- "白名单看起来够窄"被静默放宽授权；只有实例初始化流程创建的新库写 standard。

CREATE TABLE IF NOT EXISTS execution_policy (
    policy_scope TEXT PRIMARY KEY,            -- 固定 global
    profile      TEXT    NOT NULL,            -- standard / strict
    revision     INTEGER NOT NULL,            -- 从 1 起，每次成功写入 +1
    updated_at   INTEGER NOT NULL,            -- 最近一次写入时间（Unix 毫秒，UTC）
    operation_id TEXT    NOT NULL DEFAULT '', -- 产生该修订的管理操作；迁移种子记 system.migrate_v6
    CHECK (policy_scope = 'global'),
    CHECK (profile IN ('standard', 'strict')),
    CHECK (revision >= 1)
);

INSERT OR IGNORE INTO execution_policy (policy_scope, profile, revision, updated_at, operation_id)
VALUES ('global', 'strict', 1,
        CAST(strftime('%s', 'now') AS INTEGER) * 1000,
        'system.migrate_v6');

-- ============================================================
-- 3. network_exceptions：内网 / 受保护地址段的访问例外
-- ============================================================
-- 例外只管网络：文件由目录边界决定、命令由白名单 / 安装清单决定，不在这里开口子。
-- note 必填（放行必须带理由）；同一 (kind, value) 只允许一条。

CREATE TABLE IF NOT EXISTS network_exceptions (
    exception_id TEXT PRIMARY KEY,            -- exc_ + 26 位 Crockford Base32（ULID 形态）
    kind         TEXT    NOT NULL,            -- host / ip_port
    value        TEXT    NOT NULL,            -- 规范化主机名或 ip:port
    note         TEXT    NOT NULL,            -- 必填理由；同时进管理审计
    revision     INTEGER NOT NULL,            -- 从 1 起，每次修改 +1
    created_at   INTEGER NOT NULL,
    operation_id TEXT    NOT NULL,
    UNIQUE (kind, value),
    CHECK (kind IN ('host', 'ip_port')),
    CHECK (note <> ''),
    CHECK (revision >= 1)
);

-- ============================================================
-- 4. instance_secrets：实例级密钥初始化记录
-- ============================================================
-- 与 schema_version 解耦：迁移版本与密钥身份是两件事，混用会让"回滚库版本"
-- 误伤密钥判定。fingerprint 是可公开比对的密钥材料摘要（不含密钥本身），
-- 用来在不解密的前提下核对"这把密钥是不是这个实例的那把"。

CREATE TABLE IF NOT EXISTS instance_secrets (
    scope          TEXT PRIMARY KEY,          -- 固定 master_key
    key_id         TEXT    NOT NULL,          -- 主密钥标识（文件内 key_id 的权威副本）
    fingerprint    TEXT    NOT NULL,          -- 密钥材料摘要（sha256 十六进制小写）
    initialized_at INTEGER NOT NULL,
    operation_id   TEXT    NOT NULL,
    CHECK (scope = 'master_key')
);

-- ============================================================
-- 5. mcp_credential_sources：每个 MCP 占位符的凭据来源
-- ============================================================
-- 来源必须显式记录：靠"托管行是否存在"推断会让撤销变成隐式回落 env。
-- 迁移之后改 managed 再撤销，仍然不会退回 env。

CREATE TABLE IF NOT EXISTS mcp_credential_sources (
    server_uid   TEXT    NOT NULL,            -- MCP server 的资源 UID
    binding_key  TEXT    NOT NULL,            -- 占位符名（env 键或 header 名）
    mode         TEXT    NOT NULL,            -- managed / env
    revision     INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    operation_id TEXT    NOT NULL,
    PRIMARY KEY (server_uid, binding_key),
    CHECK (mode IN ('managed', 'env')),
    CHECK (revision >= 1),
    CHECK (binding_key <> '')
);

-- Basalt 数据库结构契约（SQLite）
--
-- 契约地位：本文档是 Basalt **数据结构**（表 / 列 / 状态字面量）的唯一事实源——技术方案§5、
--   需求文档§3.4、§6.6 等处出现同名字段时以本文档为准；这几份文档的其余内容（架构、行为、验收）
--   仍以 docs/TechnicalDesign.md 为最权威。字段名与状态字面量**不得**在别处另立第二份定义。
-- 文档状态：待验证设计草案（冻结条件：五链路原型验证通过 + 真实模型调用往返，见 DevelopmentLoop.md§5.2）。
-- 目标 schema_version：6（v1 已发布；v2 = US-11 / US-12 的资源版本、发布指针、Run 资源引用、
--   管理操作审计、Agent 生命周期索引与归档索引；v3 = US-12 范围修订的 UID 级资源身份与墓碑、
--   使用范围、托管凭据与凭据版本、Agent 资源绑定投影、通知 Run 引用与发送明细；v4 = US-12 R7 的
--   定时任务系统代投与资源连接测试最后结果；v5 = US-12 R8 的网络域名白名单权威表；v6 = US-14 14D 的
--   执行档位、访问例外、实例密钥初始化记录、MCP 每键凭据来源）。
--   v2 只追加新表 / 新索引；v3 追加新表与新列，并**重建** resource_versions / notify_channels 两张表
--   （SQLite 不能就地改唯一键 / 主键与 CHECK 约束）；v4 / v5 只追加新表 / 新列 / 新索引；v6 追加四张
--   新表并**重建** credentials（owner_kind 加 mcp_server、新增 binding_key、唯一键改三元组）。
-- 迁移纪律：
--   1. 建表一律用 IF NOT EXISTS（幂等）；结构变更 = 新增受控前向 SQL + 递增 schema_version，禁止 ORM 自动 DDL
--   2. 新旧库判定以 schema_version 表**是否存在**为准：不存在 = 新库（全量建表）；存在 = 旧库（按当前
--      版本逐版前向迁移）。库内版本高于二进制支持的版本 → **拒绝启动**（不得让旧二进制打开新库）
--   3. 已发布版本的迁移脚本不得改动（只能追加新版本迁移）
--   4. 时间统一存 INTEGER（Unix 毫秒，UTC）；布尔存 INTEGER（0/1）——SQLite 无原生布尔与时间类型
--   5. 连接池：写连接 max 1（SQLite 单写者），读连接若干
--   6. PRAGMA 必须**逐物理连接**设置（SQLite 官方语义：foreign_keys / busy_timeout 是**连接级**设置，
--      连接池会开多条连接，只在启动时执行一次只影响那一条）：modernc.org/sqlite 经 DSN 参数
--      `?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)` 对每条新连接生效；`journal_mode=WAL`
--      是**库级持久**属性（写一次进库文件，重启仍是 WAL），经 DSN 复用无害
--   7. 事务不得跨 LLM 调用与工具执行（外部调用耗时不可控，长事务会把写锁焊死）
--   8. 表重建（create new → copy → drop → rename）只允许出现在**未发布**的新迁移里；迁移器在事务外
--      `PRAGMA foreign_keys=OFF`，迁移事务内完成重建与 `PRAGMA foreign_key_check`，通过才提交
--   9. 新表的时间列一律 INTEGER（Unix 毫秒），与既有表一致；设计稿里的 TEXT 时间写法按本条归一

-- 连接级设置的示例（脚本自身单连接执行时有效）；装配侧逐连接设置，见迁移纪律 6
PRAGMA foreign_keys = ON;

-- 本文档声明的目标结构版本；与 storage.SupportedSchemaVersion 必须一致（契约测试判卷）。
-- 权威版本记录仍是 schema_version 表：本 PRAGMA 只是让契约文件自述，不参与迁移判定。
PRAGMA user_version = 6;

-- ============================================================
-- 版本与迁移
-- ============================================================

CREATE TABLE IF NOT EXISTS schema_version (
    version     INTEGER PRIMARY KEY,          -- 迁移版本号，逐版递增
    applied_at  INTEGER NOT NULL,             -- 应用时间（Unix 毫秒）
    description TEXT    NOT NULL              -- 该版本做了什么
);

-- ============================================================
-- 会话与消息（会话元数据 + 消息逐条追加）
-- ============================================================
-- 设计要点：消息独立成表、逐条追加，完整承载 tool_calls / tool_call_id / reasoning_content。
-- 不用单列 messages_json：工具调用轮次的完整结构一旦被序列化成一整块，就无法按 run 查询、
-- 无法增量追加，供应商要求回传的扩展字段也容易在往返中丢失。

CREATE TABLE IF NOT EXISTS sessions (
    session_id     TEXT PRIMARY KEY,          -- channel+user+profile 联合派生
    profile_name   TEXT NOT NULL,             -- 关联 Agent（Profile 名 = 目录名）
    channel        TEXT NOT NULL,             -- cli / web / scheduler
    user_id        TEXT NOT NULL,             -- 用户标识（定时器固定 scheduler）
    status         TEXT NOT NULL DEFAULT 'active',  -- active / archived
    message_count  INTEGER NOT NULL DEFAULT 0,      -- 冗余计数，列表页免 count(*)
    created_at     INTEGER NOT NULL,
    last_active_at INTEGER NOT NULL,
    archived_at    INTEGER                    -- 可空
);

CREATE INDEX IF NOT EXISTS idx_sessions_profile ON sessions(profile_name);
CREATE INDEX IF NOT EXISTS idx_sessions_status  ON sessions(status, last_active_at DESC);

CREATE TABLE IF NOT EXISTS messages (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id        TEXT    NOT NULL REFERENCES sessions(session_id),
    run_id            TEXT,                   -- 产生该消息的 Run（system 注入类可为空）
    seq               INTEGER NOT NULL,       -- 会话内序号（1 起，追加序）
    role              TEXT    NOT NULL,       -- system / user / assistant / tool
    content           TEXT,                   -- 可空：带工具调用的 assistant 消息 content 可能为空
    tool_calls        TEXT,                   -- JSON 数组：[{id,name,arguments}]（arguments 是 JSON 字符串，原样保存）
    tool_call_id      TEXT,                   -- tool 角色：回填哪次调用（必须与 assistant 里的 id 对应）
    reasoning_content TEXT,                   -- 供应商扩展：思考过程（DeepSeek 思考模式必须回传，丢了会 HTTP 400）
    provider_extra    TEXT,                   -- JSON 对象：供应商扩展字段透传位
    created_at        INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_session_seq ON messages(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_messages_run ON messages(run_id);

-- ============================================================
-- Run（执行与审计的单位）
-- ============================================================
-- 一次 Run = 一条消息触发的完整处理过程（含其全部 LLM 轮次与工具执行）。
-- 先落 running 行再执行：崩溃时靠它找回来。

CREATE TABLE IF NOT EXISTS runs (
    run_id      TEXT PRIMARY KEY,             -- <channel>-<unix_millis>-<随机6位>
    session_id  TEXT,                         -- 无状态调用（/agents/{name}/invoke）可为空
    agent       TEXT NOT NULL,                -- Agent 名
    channel     TEXT NOT NULL,                -- cli / web / scheduler / admin
    user_id     TEXT NOT NULL,
    trigger     TEXT NOT NULL,                -- user / cron / manual（管理台手动补跑）/ api
    status      TEXT NOT NULL,                -- running / completed / limit_reached / cancelled / failed
    reason      TEXT,                         -- 非正常终止的可读原因
    iterations  INTEGER NOT NULL DEFAULT 0,   -- 实际消耗的 ReAct 轮次
    tool_calls  INTEGER NOT NULL DEFAULT 0,   -- 实际执行的工具调用次数
    config_hash TEXT,                         -- 本次运行使用的配置快照版本（关联 config_snapshots.hash）
    started_at  INTEGER NOT NULL,
    finished_at INTEGER                       -- 终态时写入
);

CREATE INDEX IF NOT EXISTS idx_runs_session  ON runs(session_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_agent    ON runs(agent, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_status   ON runs(status) WHERE status = 'running';  -- 崩溃恢复扫描用

-- ============================================================
-- 配置快照（校验通过的不可变配置）
-- ============================================================
-- 热加载的正确性基础：运行中的 Run 始终使用它启动时的那份快照，
-- 磁盘上的 AGENT.md 此刻无论怎么改，都不会让一次运行中途换配置。
--
-- 冻结范围 = manifest_json 列出的文件（AGENT.md + bootstrap 文件 + 绑定技能的 SKILL.md，即 L1 注入源）；
-- 构建时"读即所记"——注入内容与清单哈希出自同一次读取，两者必然对应。
-- 清单任一文件读取失败 → 该次运行拒绝启动（失败关闭）。
-- L2/L3 技能正文与 scripts/ 脚本**不在**运行冻结范围：按需读取、读时为准（读取行为进审计）。

CREATE TABLE IF NOT EXISTS config_snapshots (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_name    TEXT    NOT NULL,
    hash          TEXT    NOT NULL,           -- manifest 规整序列化的 sha256（内容寻址：内容相同即同一版本）
    content       TEXT    NOT NULL,           -- AGENT.md 全文（含 frontmatter 与正文；人工可读的便捷副本）
    manifest_json TEXT    NOT NULL,           -- 冻结清单 JSON：[{path, sha256}]；hash 由本列内容派生，二者永不脱钩
    created_at    INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_config_snapshots_hash ON config_snapshots(agent_name, hash);

-- ============================================================
-- 审计一：LLM 调用（两阶段写）
-- ============================================================
-- 与工具执行同一条纪律：调用前先插入 status=pending（插入失败即拒绝发出该次调用 = fail-closed），
-- 调用返回后更新为终态。run_id / round / status / error_kind / config_hash 用于关联与解释执行——
-- 从"有记录"到"能关联、能解释失败"。
-- 崩溃窗口：pending 行在重启后标 unknown；终态更新失败时保留 pending 并记 ERROR（见 run-and-errors.md §7.2）。

CREATE TABLE IF NOT EXISTS llm_calls (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id            TEXT    NOT NULL,       -- 关联 Run
    session_id        TEXT,
    round             INTEGER NOT NULL,       -- 该 Run 内第几轮（1 起）
    provider          TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    duration_ms       INTEGER NOT NULL,
    status            TEXT    NOT NULL,       -- pending / success / failed / unknown
    error_message     TEXT,                   -- 失败原文（截断 4 KB）
    error_kind        TEXT,                   -- sandbox_denied/timeout/not_found/invalid_input/external_error/runtime_error/unknown
    config_hash       TEXT,                   -- 当时的配置快照
    created_at        INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_llm_calls_run ON llm_calls(run_id, round);

-- ============================================================
-- 审计二：工具执行（两阶段写）
-- ============================================================
-- 执行前插入 status=pending（插入失败即拒绝执行 = fail-closed），执行后更新为终态。
-- 崩溃时 pending 行在重启后标 unknown——工具是否已产生外部副作用无法确定，如实标记不猜测。
-- 终态更新失败时保留 pending 并记 ERROR（见 run-and-errors.md §7.2）。成败只由 status 列表达。

CREATE TABLE IF NOT EXISTS tool_invocations (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        TEXT    NOT NULL,           -- 关联 Run
    session_id    TEXT,
    tool_call_id  TEXT,                       -- 模型给的调用标识（与 assistant 消息里的对应）
    tool_name     TEXT    NOT NULL,
    input_json    TEXT,                       -- 入参（原样）
    result_json   TEXT,                       -- 结果（超 256 KB 截断并标记 truncated）
    status        TEXT    NOT NULL,           -- pending / success / failed / unknown（成败只由本列表达，不另设布尔列）
    error_message TEXT,                       -- 失败原文（截断 4 KB）
    error_kind    TEXT,
    duration_ms   INTEGER,
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER
);

CREATE INDEX IF NOT EXISTS idx_tool_invocations_run ON tool_invocations(run_id, id);
CREATE INDEX IF NOT EXISTS idx_tool_invocations_pending ON tool_invocations(status) WHERE status = 'pending';

-- ============================================================
-- 定时任务（定义源在 AGENT.md，这两张表只存状态与历史）
-- ============================================================
-- 重启对账：enabled 以库为准、定义以文件为准（文件改动不回翻停用）。
-- 下线 = 置 retired_at（**不物理删**——task_executions 的外键因此永不撞）；retired 后同 id 再出现
-- = 清除 retired_at、enabled 保持库值。

CREATE TABLE IF NOT EXISTS scheduled_tasks (
    task_id      TEXT PRIMARY KEY,            -- AGENT.md 里的 id：单项任务可缺省派生 <agent>-1；两项及以上每项必须显式声明
    agent_name   TEXT NOT NULL,
    cron         TEXT NOT NULL,               -- 6 段表达式（原样保存，便于对账）
    zone         TEXT NOT NULL,               -- IANA 时区名，缺省 UTC
    message      TEXT NOT NULL,
    deliver_to   TEXT NOT NULL DEFAULT '',    -- 系统代投目标的**渠道资源 UID**；空串 = 不代投。不接受渠道名
    enabled      INTEGER NOT NULL DEFAULT 1,
    definition_hash TEXT NOT NULL,            -- 定义指纹：与文件不一致时以文件为准并更新
    last_run_at  INTEGER,
    last_status  TEXT,                        -- 上一次执行结果状态
    retired_at   INTEGER,                     -- 下线时间（Agent 归档删除时置位）；下线 = 标记不物理删
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS task_executions (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id       TEXT    NOT NULL REFERENCES scheduled_tasks(task_id),  -- 登记行不物理删（下线置 retired_at），FK 永不撞
    run_id        TEXT,                       -- 本次触发产生的 Run
    status        TEXT    NOT NULL,           -- started / completed / failed / skipped（抢锁失败跳过）；启动恢复把遗留 started 行按 Run 终态收尾（completed/limit_reached → completed；failed/cancelled → failed）
    trigger       TEXT    NOT NULL,           -- cron / manual
    duration_ms   INTEGER,
    error_message TEXT,
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER
);

CREATE INDEX IF NOT EXISTS idx_task_executions_task ON task_executions(task_id, started_at DESC);

-- 定时任务系统代投（v4 新增）：每个成功运行至多一条，主键 = run_id。
-- 这是**权威记录**（不是 notify_deliveries 那种工具调用派生账）：发送前先写 pending，
-- 再按固定渠道 UID 发送并回填 sent / failed / unknown；运行未成功写 skipped。
-- run_id 不设外键：pending 行在 Run 准入创建 `runs` 行**之前**写入，外键会让预写失败。
-- `channel_name` 是预写时的名称快照；资格与目标只认 `channel_uid`。
CREATE TABLE IF NOT EXISTS schedule_deliveries (
    run_id       TEXT PRIMARY KEY,
    task_id      TEXT    NOT NULL REFERENCES scheduled_tasks(task_id),
    agent_name   TEXT    NOT NULL,
    agent_uid    TEXT    NOT NULL DEFAULT '',
    channel_uid  TEXT    NOT NULL,
    channel_name TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL,            -- pending / sent / failed / unknown / skipped
    http_status  INTEGER,
    receipt_code INTEGER,
    detail       TEXT    NOT NULL DEFAULT '', -- 固定短语与 scheme://host；不含路径 / 查询串 / 凭证
    attempted_at INTEGER,
    finished_at  INTEGER,
    created_at   INTEGER NOT NULL,
    CHECK (status IN ('pending', 'sent', 'failed', 'unknown', 'skipped'))
);

CREATE INDEX IF NOT EXISTS idx_schedule_deliveries_task
    ON schedule_deliveries(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_schedule_deliveries_pending
    ON schedule_deliveries(status) WHERE status = 'pending';

-- ============================================================
-- 资源 UID 级身份与墓碑（v3 新增）
-- ============================================================
-- 一个资源**实例**一行；`resource_uid` 创建后不可变，名称只是标签。
--   - 删除只把本行置 state='deleted'，**不**物理删除；同名重建生成**新 UID** 与新行
--   - `resource_pointers` / `notify_channels` 的行只回答「这个名称当前指向哪个 UID」；同名重建覆盖
--     指针行时，旧 UID 的删除事实仍在本表保留
--   - 版本、凭据、Run 引用一律按 UID 关联；**不得**按同名把旧 UID 的历史接到新 UID 上

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
-- Agent 资源绑定投影（v3 新增）
-- ============================================================
-- 权威是 AGENT.md frontmatter 的 `resource_bindings`：声明即绑定；保存期与 Run 准入判据都在文件上。
-- 本表是**可重建投影**，供列表、引用视图与删除闸门查询，**不是**写入口：
--   - 注册表扫描按 AGENT.md **全量重建**；投影与文件不一致时以文件为准并记 WARN，不反向改写文件
--   - 复制 / 归档 / 恢复 / 导出随目录携带绑定（UID 不变）；同名重建是新 Agent UID，不自动接管旧绑定
--   - 归档 Agent 有可信 UID 时同样投影；`uid_source='legacy_unrecorded'` 的旧归档没有可信身份，
--     不写本表（不得伪造 UID），删除闸门与引用视图改为直接扫描归档定义文件

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
-- 通知渠道注册表（v2 表；v3 重建：主键改资源 UID，地址两模式）
-- ============================================================
-- 地址两模式（credential_mode）：
--   - env：url 存精确 `${ENV_VAR}` 引用（发送时解析；渠道地址的路径 / 查询串就是凭证，明文不入库）
--   - managed：url 为空，完整地址以 AES-256-GCM 密文存 credential_versions；对外只回 url_host 与尾号
-- url 为空且无托管密文 = 暂不可发送。对外一律只回 scheme + host（url_host），不返回 path / query。
-- 活动名称唯一（部分唯一索引）；删除置 deleted_at 且身份行置 deleted，行保留供历史查询。

CREATE TABLE IF NOT EXISTS notify_channels (
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

CREATE UNIQUE INDEX IF NOT EXISTS ux_notify_channels_active_name
    ON notify_channels(name) WHERE deleted_at IS NULL;

-- ============================================================
-- 资源连接测试的最后一次结果（v4 新增；模型连接与通知渠道共用）
-- ============================================================
-- `availability`（本地配置就绪：env 有值 / 托管凭据 active）与"连接测试成功"是两件事：
-- 本表只记**实测**结果与时间，页面分别展示，不得用本地就绪冒充实测可用。
-- 历史测试明细仍以 management_operations 为权威；本表只保留最后一次投影。
CREATE TABLE IF NOT EXISTS resource_connection_tests (
    resource_kind TEXT    NOT NULL,             -- model_connection / notify_channel
    resource_uid  TEXT    NOT NULL,
    tested_at     INTEGER NOT NULL,
    status        TEXT    NOT NULL,             -- success / failed / unknown
    detail        TEXT    NOT NULL DEFAULT '',  -- 脱敏固定短语
    operation_id  TEXT    NOT NULL,
    PRIMARY KEY (resource_kind, resource_uid),
    CHECK (resource_kind IN ('model_connection', 'notify_channel')),
    CHECK (status IN ('success', 'failed', 'unknown'))
);

-- ============================================================
-- 网络域名白名单（v5 新增；唯一权威源）
-- ============================================================
-- 单行表：`policy_scope` 固定 `http.allowed_domains`，`domains` 是换行连接的规整域名
-- （小写、去重、升序；空串 = 空列表 = 受此策略约束的出站请求全部拒绝）。
-- `basalt.yaml` 的同名键只在表内无行时作为首次种子，此后不再读取；改 YAML 不会覆盖
-- 管理台写入的值，重启也不会回退（R8 裁决 2）。
-- `revision` 从 1 起、每次成功写入 +1，是条件写（If-Match → 412）的 CAS 依据；
-- `operation_id` 指向产生该修订的管理操作（种子行为空串），操作者与时间以审计为准。

CREATE TABLE IF NOT EXISTS network_domain_policy (
    policy_scope TEXT PRIMARY KEY,            -- 固定 http.allowed_domains
    domains      TEXT    NOT NULL,            -- 规整域名，换行连接；空串 = 空列表
    revision     INTEGER NOT NULL,            -- 从 1 起，每次成功写入 +1
    updated_at   INTEGER NOT NULL,            -- 最近一次写入时间（Unix 毫秒，UTC）
    operation_id TEXT    NOT NULL DEFAULT ''  -- 产生该修订的管理操作；种子为空串
);

-- ============================================================
-- 执行档位（v6 新增；YAML 只作种子，运行期以库为准）
-- ============================================================
-- 单行表（`policy_scope` 固定 `global`）。`standard` 放行未命中禁止域名的公网域名；
-- `strict` 只放行白名单。两档共用同一套基础防护（DNS 解析校验、连接固定、忽略代理
-- 环境变量、shell 名称集合），差异只在授权范围。
-- v5 → v6 迁移一律写 `strict`（不看白名单内容、空白名单也不放宽）；只有实例初始化
-- 流程创建的新库写 `standard` —— "存量实例静默扩大授权"在结构上不可能发生。
-- `revision` 是条件写（If-Match → 412）的 CAS 依据；`operation_id` 指向产生该修订的
-- 管理操作，迁移种子记 `system.migrate_v6`。

CREATE TABLE IF NOT EXISTS execution_policy (
    policy_scope TEXT PRIMARY KEY,            -- 固定 global
    profile      TEXT    NOT NULL,            -- standard / strict
    revision     INTEGER NOT NULL,            -- 从 1 起，每次成功写入 +1
    updated_at   INTEGER NOT NULL,            -- 最近一次写入时间（Unix 毫秒，UTC）
    operation_id TEXT    NOT NULL DEFAULT '', -- 产生该修订的管理操作
    CHECK (policy_scope = 'global'),
    CHECK (profile IN ('standard', 'strict')),
    CHECK (revision >= 1)
);

-- ============================================================
-- 网络访问例外（v6 新增；仅网络，不含文件与命令授权）
-- ============================================================
-- 例外用于放行**受保护地址段**（回环、私网、链路本地、云元数据等）：两档入口相同、
-- 语义不同——`standard` 用它放行受保护地址，`strict` 等价于一条白名单条目并另可放行
-- 受保护地址。`note` 必填：放行必须带理由，并与 `operation_id` 一起进管理审计。
-- 云元数据地址（`169.254.169.254` / `fd00:ec2::254`）**即使加例外也拒**，判定在运行时。
-- 删除后下一次判定即失效；同一 `(kind, value)` 只允许一条。

CREATE TABLE IF NOT EXISTS network_exceptions (
    exception_id TEXT PRIMARY KEY,            -- exc_ + 26 位 Crockford Base32（ULID 形态）
    kind         TEXT    NOT NULL,            -- host / ip_port
    value        TEXT    NOT NULL,            -- 规范化主机名或 ip:port
    note         TEXT    NOT NULL,            -- 必填理由
    revision     INTEGER NOT NULL,            -- 从 1 起，每次修改 +1
    created_at   INTEGER NOT NULL,
    operation_id TEXT    NOT NULL,
    UNIQUE (kind, value),
    CHECK (kind IN ('host', 'ip_port')),
    CHECK (note <> ''),
    CHECK (revision >= 1)
);

-- ============================================================
-- 实例密钥初始化记录（v6 新增；与 schema_version 解耦）
-- ============================================================
-- 单行表（`scope` 固定 `master_key`）。选独立表而不是复用 schema_version：迁移版本
-- 与密钥身份是两件事，混用会让"回滚库版本"误伤密钥判定。
-- `key_id` 是主密钥文件内标识的权威副本；`fingerprint` 是可公开比对的密钥材料摘要
-- （sha256 十六进制小写），用于在不解密的前提下核对"这把密钥是不是这个实例的那把"。
-- 记录存在但密钥文件缺失 / 损坏 / `key_id` 不符 → 进恢复态，**不得**据此生成新密钥。

CREATE TABLE IF NOT EXISTS instance_secrets (
    scope          TEXT PRIMARY KEY,          -- 固定 master_key
    key_id         TEXT    NOT NULL,
    fingerprint    TEXT    NOT NULL,
    initialized_at INTEGER NOT NULL,
    operation_id   TEXT    NOT NULL,
    CHECK (scope = 'master_key')
);

-- ============================================================
-- MCP 每键凭据来源（v6 新增；来源必须显式记录）
-- ============================================================
-- 一个 MCP server 的每个占位符（env 键或 header 名）独立记来源与修订。
-- 靠"托管凭据行是否存在"推断来源会让撤销变成隐式回落 env：显式记录后，`managed`
-- 缺失 / 已撤销 / 解密失败时**不读**进程环境，只有 `env` 才读。
-- v5 存量配置在升级后由启动编排按文件事实补记 `mode=env`（MCP server 定义不在库里，
-- SQL 无法枚举占位符）；这是"记录当前真实来源"，不是运行时回落。

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

-- ============================================================
-- 长期记忆（sqlite 档载体；markdown 档不用此表）
-- ============================================================
-- scope 语义：agent = Agent 层；global = 全局回退层（agent_name 为 NULL）。
-- 解析规则：有 Agent 上下文时优先 Agent 层，无内容回退全局层，两层不叠加。

CREATE TABLE IF NOT EXISTS memory_entries (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    scope      TEXT NOT NULL,                 -- agent / global
    agent_name TEXT,                          -- scope=global 时为 NULL
    section    TEXT NOT NULL,                 -- core / archive
    content    TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_memory_scope ON memory_entries(scope, agent_name, section, id);

-- ============================================================
-- 管理操作审计（US-11 / US-12；v2 新增）
-- ============================================================
-- 一次管理面高权限变更 = 一行。先插 status=pending 再执行；预写失败不得执行（fail-closed）。
-- 该表**不**挂 run_id：管理变更不是模型 / 工具调用，不得混写，也不得伪造一次 Run 来承载审计。
-- 恢复时不盲目重做：按 action 查证目标的实际状态，再写 success / failed / unknown（run-and-errors.md §9.4）。
-- token 只记录不可恢复的指纹（token:<sha256 前 16 位十六进制>），明文 token 不得入库。
-- v3 起凭据与范围动作的 target_id 记资源 `resource_uid`（不是名称）；detail_json 只记资源名、
--   credential_id、版本号、模式与状态，不得出现明文地址、密钥或主密钥。

CREATE TABLE IF NOT EXISTS management_operations (
    operation_id        TEXT PRIMARY KEY,          -- op-<unix_millis>-<6 位小写字母数字>
    action              TEXT NOT NULL,             -- agent.content.update / model_connection.test / system.baseline_v3 ...
    target_kind         TEXT NOT NULL,             -- agent / schedule / model_connection / mcp_server / skill / notify_channel / system
    target_id           TEXT NOT NULL,             -- 资源 UID（v3 凭据 / 范围动作）/ 资源名 / Agent 名 / system
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
-- 资源版本与发布指针（Provider / MCP / 技能；v2 新增；v3 重建版本表）
-- ============================================================
-- 定义内容在文件（basalt.yaml / mcp_servers.yaml / 技能工作副本），已提交版本、当前发布指针与
-- Run 引用在 SQLite。版本表是**不可变历史**，不是第二套可编辑定义：
--   - model_connection / mcp_server：definition_yaml = 该资源块的原文（含 ${ENV_VAR} 引用，不含解析后的值）
--   - skill：manifest_json + storage_path = 完整文件清单与不可变快照目录
-- content_hash 对 model/MCP 是 definition_yaml 的 sha256；对技能是文件清单规整序列化的 sha256
-- （与 config_snapshots.hash 同一规整化规则）。版本号逐**资源 UID** 从 1 单调递增；
-- 撤回 = 用旧内容创建新版本；同名重建是新 UID，版本号从 1 重新计。
-- resource_id 是发布时的名称标签，仅展示与历史检索；身份键是 resource_uid。

CREATE TABLE IF NOT EXISTS resource_versions (
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

CREATE INDEX IF NOT EXISTS idx_resource_versions_lookup
    ON resource_versions(resource_kind, resource_uid, version_no DESC);
CREATE INDEX IF NOT EXISTS idx_resource_versions_hash
    ON resource_versions(resource_kind, resource_uid, content_hash);
CREATE INDEX IF NOT EXISTS idx_resource_versions_name
    ON resource_versions(resource_kind, resource_id, created_at DESC);

-- 当前发布指针（每个 (资源种类, 名称) 一行；删除后保留行并置 deleted_at，历史版本不物理删）。
-- published_version_id = 最近一次发布的版本；applied_version_id = 运行时实际生效的版本。
-- Provider / 技能发布后对新 Run 立即生效（两者相等）；MCP 首版需重启，重启前 applied 仍指旧版本。
-- resource_uid = 该名称**当前**指向的资源实例；同名重建把本行更新为新 UID（旧 UID 的墓碑在
-- resource_identities）。scope / owner_agent_uid / revision 随范围转换与凭据变更更新，参与资源 ETag。
-- resource_uid 在迁移回填后必然有值；可空只因 SQLite 的 ALTER ADD COLUMN 无法带 NOT NULL 约束。

CREATE TABLE IF NOT EXISTS resource_pointers (
    resource_kind        TEXT    NOT NULL,
    resource_id          TEXT    NOT NULL,          -- 名称（既有列语义，不改名）
    resource_uid         TEXT,                      -- 当前指向的资源 UID
    published_version_id INTEGER NOT NULL REFERENCES resource_versions(id),
    applied_version_id   INTEGER REFERENCES resource_versions(id),
    scope                TEXT    NOT NULL DEFAULT 'shared',
    owner_agent_uid      TEXT,
    revision             INTEGER NOT NULL DEFAULT 1, -- 每次发布 / 凭据变更 / 范围转换 +1；参与资源 ETag
    deleted_at           INTEGER,
    updated_at           INTEGER NOT NULL,
    operation_id         TEXT    NOT NULL REFERENCES management_operations(operation_id),
    PRIMARY KEY (resource_kind, resource_id),
    CHECK (resource_kind IN ('model_connection', 'mcp_server', 'skill')),
    CHECK (scope IN ('exclusive', 'shared')),
    CHECK ((scope = 'exclusive') = (owner_agent_uid IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_resource_pointers_applied
    ON resource_pointers(resource_kind, applied_version_id);
CREATE INDEX IF NOT EXISTS idx_resource_pointers_uid
    ON resource_pointers(resource_uid);

-- ============================================================
-- 托管凭据身份与不可变凭据版本（v3 新增）
-- ============================================================
-- 一个资源 UID 一个**托管**凭据身份（env 模式没有凭据身份；env 的引用就是定义里的 `${VAR}`）。
-- v6 起 MCP server 按 `(owner_uid, binding_key)` 每占位符一条身份，各自独立版本与撤销。
-- owner_name 只用于展示，可能被同名新 UID 复用；owner_uid 才是身份。
--   - state：active / revoked / retired（资源删除或 managed→env 切换时置 retired，版本仍可解密供在途使用）
--   - current_version：当前活动凭据版本号；revocation_epoch 每次撤销 +1，参与资源 ETag
--   - 撤销按**整个身份的全部版本**执行；旧版本永不复活；重新录入生成新的 version_no
-- 主密钥在工作区**之外**，只经 key_id 引用；解密按需发生，不得常驻内存或落盘。

CREATE TABLE IF NOT EXISTS credentials (
    credential_id    TEXT PRIMARY KEY,            -- cred_ + 26 位 Crockford Base32（ULID 形态）
    owner_kind       TEXT    NOT NULL,            -- model_connection / notify_channel / mcp_server
    owner_uid        TEXT    NOT NULL,            -- 资源 UID
    binding_key      TEXT    NOT NULL DEFAULT '', -- MCP 占位符名；其它 kind 固定空串（v6 新增）
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

-- 不可变版本：目标绑定、密文与 nonce 都在版本上。
-- AAD 绑定 (owner_kind, owner_uid, credential_id, version_no, target_binding)；解密后必须复核目标一致。
-- target_binding = 规范化目标地址的 sha256（不含明文 URL）；last4 只在原值 >= 8 位时保留，否则空串。
-- 撤销后 ciphertext 置 NULL（不可再解密）；superseded / retired 保留密文供在途使用。
CREATE TABLE IF NOT EXISTS credential_versions (
    credential_id  TEXT    NOT NULL REFERENCES credentials(credential_id),
    version_no     INTEGER NOT NULL,              -- 1 起，逐凭据身份单调递增
    state          TEXT    NOT NULL,              -- active / superseded / revoked / retired
    key_id         TEXT    NOT NULL,
    target_binding TEXT    NOT NULL,
    last4          TEXT    NOT NULL DEFAULT '',
    nonce          BLOB    NOT NULL,              -- 12 字节随机数
    ciphertext     BLOB,                           -- 解出的 AES-256-GCM 密文（尾部含 16 字节 tag）；revoked 时为 NULL
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

-- 发布版本与当时活动凭据版本的快照，**仅供审计展示**。
-- 运行时授权来源不是本表：模型调用按 Run 准入固定的凭据版本（run_credential_refs），
-- 通知按发送时当前 active 版本；回滚发布只回滚定义内容，**不恢复旧 Key**。
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
-- Run 资源清单与引用（v2 新增；v3 增列）
-- ============================================================
-- RunResourceManifest 的持久形态：一次 Run 一行，snapshot_json 记录准入时固定的
-- Provider 版本、MCP 实际生效版本、每个技能版本与完整 L1/L2/L3 文件哈希、Agent 生命周期状态。
-- v2 清单（format_version=2）另含 agent_uid、Provider 凭据版本与目标绑定、通知模式与具体渠道 UID。
-- 历史 Run 没有本行 → API 标 legacy_unrecorded，不得用当前版本补造。
-- runs.config_hash 的原义不变（仍是 ConfigSnapshot 哈希）；本表不覆盖、不改写它。

CREATE TABLE IF NOT EXISTS run_resource_manifests (
    run_id                TEXT PRIMARY KEY REFERENCES runs(run_id),
    format_version        INTEGER NOT NULL DEFAULT 1, -- 1 / 2；按 snapshot_json 内声明写入
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
-- resource_uid 记录准入时固定版本所属的资源实例；迁移前无法证明归属的旧行置 NULL（legacy_unrecorded）。
-- resource_name 是 v3 追加的名称快照（与既有 resource_id 同值），供按名称检索历史引用。
CREATE TABLE IF NOT EXISTS run_resource_refs (
    run_id         TEXT    NOT NULL REFERENCES runs(run_id),
    resource_kind  TEXT    NOT NULL,
    resource_id    TEXT    NOT NULL,              -- 名称（既有列语义）
    resource_uid   TEXT,                            -- 资源实例 UID；迁移无法证明时为空
    resource_name  TEXT,                            -- 名称快照（v3 追加）
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
CREATE INDEX IF NOT EXISTS idx_run_resource_refs_uid
    ON run_resource_refs(resource_uid, released_at);

-- Provider 凭据固定（v3 新增）：Run 准入固定**凭据版本**，Run 内每次模型调用前检查该版本状态——
-- 凭据被撤销（身份 revoked / 版本 revoked / revocation_epoch 变化）即终止该 Run，
-- 不静默回退到 env 或新版本。通知不写本表：通知只固定渠道 UID 集合，凭据按每次发送解析当前 active 版本。
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

-- 通知渠道冻结（v3 新增）：Run 准入按 notify_mode 枚举出**具体渠道 UID**（没有 '*' 通配），每行一个渠道；
-- notify_mode=none 不写行，all_eligible / allowlist 都写枚举后的 UID。之后新建的渠道不会自动加入已运行任务。
-- 发送时仍按当前 scope / 状态 / 凭据逐次判定；删除闸门按本表的未释放引用判定「渠道正被运行引用」。
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

-- 通知发送明细（v3 新增）：与 tool_invocations 一对一（本表主键即其外键）；
-- 成败只由 tool_invocations.status 表达，**不设**独立成功 / 失败列。
-- 只记渠道 UID / 名称 / 目标 host（不含 path / query）与解析到的凭据版本，不记完整 URL 或密钥。
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
-- Agent 生命周期索引与归档索引（v2 新增；v3 增列）
-- ============================================================
-- 权威是工作区受保护管理目录中的状态文件（.basalt/control/agents/<name>.json），本表只是
-- 可重建索引。状态文件缺失 / 校验和损坏 → state 为 NULL、valid=0，拒绝新 Run；不得悄悄激活。
-- archived 的 Agent 不在活动注册表；恢复走 agent_archives 的受控 restore，默认 disabled。
-- v3：agent_uid 是状态文件 schema v2 的身份镜像；uid_source 说明来源（state_file / legacy_unrecorded）。
-- 文件为准、索引可重建；镜像与文件不一致时按文件修正并记 ERROR，**不得**在运行期重新生成 UID。

CREATE TABLE IF NOT EXISTS agent_lifecycle (
    agent_name      TEXT PRIMARY KEY,
    agent_uid       TEXT,                         -- 身份镜像（ag_ 前缀）；schema 1 状态文件升级前为空
    uid_source      TEXT,                         -- state_file / legacy_unrecorded；NULL = 尚未对账
    state           TEXT,                         -- active / disabled / archived；NULL = 状态文件缺失或损坏
    generation      INTEGER NOT NULL DEFAULT 0,   -- 状态文件中的单调代数；0 = 无有效状态；只作乐观锁，不是身份
    state_file_hash TEXT,                         -- 状态文件校验和（sha256 十六进制）
    valid           INTEGER NOT NULL DEFAULT 0,   -- 1 = 状态文件存在且校验通过
    error_message   TEXT,                         -- 无效时的可读原因（点名字段 / 相对路径，不含凭证）
    updated_at      INTEGER NOT NULL,
    operation_id    TEXT REFERENCES management_operations(operation_id),
    CHECK (state IS NULL OR state IN ('active', 'disabled', 'archived')),
    CHECK (valid IN (0, 1)),
    CHECK (uid_source IS NULL OR uid_source IN ('state_file', 'legacy_unrecorded')),
    CHECK (uid_source IS NULL OR uid_source <> 'state_file' OR agent_uid IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_agent_lifecycle_state
    ON agent_lifecycle(state, agent_name);
CREATE INDEX IF NOT EXISTS idx_agent_lifecycle_uid
    ON agent_lifecycle(agent_uid) WHERE agent_uid IS NOT NULL;

CREATE TABLE IF NOT EXISTS agent_archives (
    archive_id          TEXT PRIMARY KEY,          -- 归档目录名：<agent>-YYYYMMDD-HHMMSS
    agent_name          TEXT    NOT NULL,          -- 归档前的 Agent 名
    agent_uid           TEXT,                      -- 归档时的 Agent UID；无可信来源时为空（不按同名猜）
    uid_source          TEXT    NOT NULL DEFAULT 'legacy_unrecorded',  -- archive / legacy_unrecorded
    archived_at         INTEGER NOT NULL,
    content_hash        TEXT    NOT NULL,          -- 归档目录清单的 sha256
    manifest_json       TEXT    NOT NULL,          -- 归档目录清单 [{path, sha256, size}]
    operation_id        TEXT    NOT NULL REFERENCES management_operations(operation_id),
    restored_at         INTEGER,
    restored_agent_name TEXT,
    CHECK (uid_source IN ('archive', 'legacy_unrecorded')),
    CHECK ((uid_source = 'archive') = (agent_uid IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_agent_archives_agent
    ON agent_archives(agent_name, archived_at DESC);
CREATE INDEX IF NOT EXISTS idx_agent_archives_restorable
    ON agent_archives(archived_at DESC) WHERE restored_at IS NULL;

-- ============================================================
-- 增量迁移 v2 与资源基线（旧库 1 → 2）
-- ============================================================
-- 1. SQL 迁移文件：internal/storage/migrations/0002_us11_us12_resources.sql。
--    **只**复制本文件“v2 新增”的 CREATE TABLE / CREATE INDEX 段（全部 IF NOT EXISTS）；
--    不改既有表、不删列、不重建表。新库由本文件全量 DDL 直接建到最新版本（现为 v3）。
-- 2. SQL 迁移完成后、开放服务之前，由持工作区实例锁的启动进程执行一次资源基线：
--    a. 先写 management_operations(system.baseline_v2, pending)；预写失败 → 拒绝启动；
--    b. Provider / MCP / 技能逐个建立 version 1 与发布指针；技能同时落不可变快照；
--    c. 既有 Agent 逐个建立状态文件，**初始 state=disabled**（不得悄悄激活），generation=1；
--       状态文件已存在但损坏 → 基线失败、拒绝启动并点名修复；
--    d. 既有 Run 不补资源清单——保持 legacy_unrecorded；
--    e. 全部完成 → 操作置 success，detail_json 记计数。任何一步失败 → 操作置 failed，拒绝启动；
--       重跑时已完成的资源按 content_hash 识别并跳过，未完成的继续（确定性、幂等，不是盲目重做）。
-- 3. 基线是文件 + SQLite 两介质协同，不能跨介质事务；每步都按“已提交版本 / 指针 / 状态文件”
--    查证后再写，重复启动收敛到同一结果。

-- ============================================================
-- 增量迁移 v3 与身份基线（旧库 2 → 3）
-- ============================================================
-- 顺序固定：**状态文件先升级并原子落盘 → 数据库迁移 → 绑定回填**。无当前活动 Run 时执行（启动前）。
-- 迁移器在事务外 `PRAGMA foreign_keys=OFF`，单事务内完成 DDL 与重建，`PRAGMA foreign_key_check`
-- 通过才提交；任一步失败 → 整事务回滚，schema_version 保持 2，启动中止并提示按备份恢复。
--
-- 1. 操作行（fail-closed）：先写 management_operations(system.baseline_v3, auth_method='migration',
--    actor_subject='migration', pending)；预写失败 → 不写任何文件、不迁移、拒绝启动。
-- 2. Agent UID（以权威状态文件为依据，先于数据库回填）：
--    a. 读生命周期状态文件（schema 1）；缺 agent_uid 的活动 Agent 生成一次 `ag_` UID，
--       写入记录并把文件升级为 schema 2（临时文件 + fsync + rename）；已有 UID 的**原样保留**；
--    b. 归档记录若已存同一 Agent 的 UID 则沿用，否则按 legacy_unrecorded 处理，**不得**按同名猜；
--    c. 数据库回填 agent_lifecycle.agent_uid / agent_archives.agent_uid，来源写 uid_source
--       （state_file / archive / legacy_unrecorded）；数据库已有值与状态文件不符 → 迁移中止，
--       不覆盖任一侧（证据冲突）；
--    d. 中断后重跑以状态文件为准：已有 UID 直接复用，已一致的行为幂等 no-op。
-- 3. SQL 迁移（0003_credentials_scope.sql，只追加，不改已发布的 0002_us11_us12_resources.sql）：
--    a. 建新表：credentials / credential_versions / resource_version_credentials / run_credential_refs /
--       run_notify_refs / notify_deliveries / resource_identities / agent_resource_bindings；
--    b. 资源 UID：给 resource_pointers 每行（活动与已删除）分配一次 `rs_` UID 并写入，同一
--       (kind, name) 只分配一次；同时为每行写 resource_identities（state 取 deleted_at 是否有值）；
--    c. 重建 resource_versions：新表加 resource_uid、唯一键改 (resource_kind, resource_uid, version_no)，
--       按 resource_pointers.resource_uid 回填同名历史版本，**保留原 id**（version_id 全局稳定）；
--       DROP 旧表 → RENAME → 重建索引；
--    d. resource_pointers 加列 resource_uid / scope / owner_agent_uid / revision（既有行 scope='shared'、
--       owner 为空、revision=1）；
--    e. run_resource_refs 加列 resource_uid / resource_name：v2 只有名称与版本 id，resource_pointers
--       只有当前行、没有历史，无法证明旧引用属于哪个实例——resource_uid 一律置 NULL 并标
--       legacy_unrecorded（resource_name 记名称快照）。**不得**用「同名指针当前活动」推断历史归属：
--       旧资源执行过 Run → 删除 → 同名重建后，活动指针指向的是新实例；
--    f. 重建 notify_channels：新表主键改 resource_uid，复制既有行（scope='shared'、owner 为空、
--       revision=1、credential_id=NULL、credential_mode='env'、url_host 能解析则填、deleted_at=NULL），
--       活动名唯一索引；同步写 resource_identities；
--    g. agent_lifecycle 加列 agent_uid / uid_source；agent_archives 加列 agent_uid / uid_source；
--    h. 迁移限制（如实登记）：同一名称在迁移前经历删除 / 重建循环时，旧版本无法可靠拆分归属——
--       全部归入当前指针 UID，并在迁移报告逐条列出（资源名、版本、相关操作 ID）供运维核对；
--       **不**猜测拆分、**不**改写历史内容。这是满足新表 NOT NULL 约束的**兼容规则**，不是对
--       历史身份连续性的证明；run_resource_refs 因此一律不回填 UID（见 3e）；
--    i. 版本行 schema_version(3, <now>, 'migrations/0003_credentials_scope.sql') 由迁移机制在脚本之后
--       写入（description 记实际执行的迁移文件路径，与 v2 同一审计口径；版本记录单一写者）。
-- 4. 绑定回填（AGENT.md 权威，写入文件而不是只写数据库）：
--    a. 对活动 Agent 与有可信 UID 的归档 Agent，读 provider.name / notify_channels，仅在能证明
--       资源身份连续时写入 resource_bindings：当前指针 active，且该名称在「Agent 定义最后保存时间」
--       之后没有 *.delete 管理操作（无删除 / 重建循环）；
--    b. 用既有 YAML 块编辑器在 frontmatter 增加 resource_bindings，块外字节逐字保留，
--       原子写；每个文件记录写前 / 写后哈希与操作 system.baseline_v3；
--    c. 无法证明连续（删除后同名重建、操作台账缺失、资源从未发布、归档缺定义）→ **不写绑定**，
--       该引用在准入时报 AGENT_RESOURCE_BINDING_MISSING，页面显示「引用未记录（迁移前无法证明身份）」；
--    d. 定义文件结构无法安全编辑（EDIT_UNSUPPORTED）→ 保持原样、不写绑定、列入迁移报告；
--    e. **不得**按同名猜身份，**不得**把迁移时的同名新资源绑定给旧引用；
--    f. 写入完成后全量重建 agent_resource_bindings；投影与文件不一致时以文件为准并记 WARN；
--    g. 重跑幂等：已有完全相同绑定 → 跳过；已有不同绑定 → 不覆盖，记冲突并列入报告。
-- 5. 全部完成 → 操作置 success，detail_json 记计数与逐条限制；任一步失败 → 操作置 failed，拒绝启动。
-- 6. 迁移完成后（schema_version = 3）：状态文件退回 v1 或活动记录缺 agent_uid → 拒绝启动并点名，
--    **不得**在运行期静默补发 UID；新安装由本文件全量 DDL 直接建到 v3，不执行上述回填。

-- ============================================================
-- 增量迁移 v6（旧库 5 → 6）
-- ============================================================
-- 1. SQL 迁移文件：internal/storage/migrations/0006_execution_policy_and_mcp_credentials.sql。
--    迁移器在事务外 `PRAGMA foreign_keys=OFF`，单事务内完成 DDL 与重建，
--    `PRAGMA foreign_key_check` 通过才提交；任一步失败 → 整事务回滚，schema_version 保持 5。
-- 2. 内容：
--    a. 重建 credentials：`owner_kind` CHECK 加 `mcp_server`、新增 `binding_key`、唯一键改
--       `(owner_kind, owner_uid, binding_key)`，**保留原 credential_id**（版本与引用不换号）；
--       非 mcp_server 行 `binding_key` 一律空串（CHECK 强制）；
--    b. 新建 execution_policy 并**写入 `strict`** 种子行（`system.migrate_v6`）——不根据白名单
--       内容反推，空白名单也不放宽；只有实例初始化流程创建的新库写 `standard`；
--    c. 新建 network_exceptions / instance_secrets / mcp_credential_sources，均不预置行。
-- 3. 迁移之后、开放服务之前由启动编排完成两件事（不在 SQL 里做，因为事实源在文件不在库）：
--    a. MCP 凭据来源补记：`mcp_servers.yaml` 中**已存在的每个占位符**按事实写
--       `mcp_credential_sources(mode=env)` 并留迁移记录；这是"记录当前真实来源"，不是运行时
--       回落——之后若改为 `managed` 再撤销，仍然不会退回 `env`；
--    b. 存量待确认项：白名单中解析到受保护地址段的条目、命中新增 shell 名称的命令条目，
--       逐条给「加例外」/「保持拒绝」两个显式动作并写审计，未确认前一律保持拒绝。
-- 4. 本迁移不新增任何授权、不改写已发布迁移（0002–0005 只追加）。

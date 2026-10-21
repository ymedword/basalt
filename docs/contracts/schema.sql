-- Basalt 数据库结构契约（SQLite）
--
-- 契约地位：本文档是 Basalt **数据结构**（表 / 列 / 状态字面量）的唯一事实源——技术方案§5、
--   需求文档§3.4、§6.6 等处出现同名字段时以本文档为准；这几份文档的其余内容（架构、行为、验收）
--   仍以 docs/TechnicalDesign.md 为最权威。字段名与状态字面量**不得**在别处另立第二份定义。
-- 文档状态：待验证设计草案（冻结条件：五链路原型验证通过 + 真实模型调用往返，见 DevelopmentLoop.md§5.2）。
-- 目标 schema_version：2（v1 已发布；v2 = US-11 / US-12 的资源版本、发布指针、Run 资源引用、
--   管理操作审计、Agent 生命周期索引与归档索引。v2 **只追加新表 / 新索引**，不改既有表列）。
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

-- 连接级设置的示例（脚本自身单连接执行时有效）；装配侧逐连接设置，见迁移纪律 6
PRAGMA foreign_keys = ON;

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

-- ============================================================
-- 通知渠道注册表
-- ============================================================
-- url 列存的是凭证引用（${ENV_VAR}）而非明文地址：渠道地址的路径/查询串就是凭证
-- （?key=<token> / /hook/<token>），明文入库即长期有效的泄漏面。发送时解析。

CREATE TABLE IF NOT EXISTS notify_channels (
    name        TEXT PRIMARY KEY,
    type        TEXT NOT NULL,                -- webhook / feishu（装配层显式映射的键）
    url         TEXT NOT NULL,                -- ${ENV_VAR} 引用；对外一律只回 scheme + host
    description TEXT,
    created_at  INTEGER NOT NULL
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

-- ============================================================
-- 增量迁移 v2 与资源基线（旧库 1 → 2）
-- ============================================================
-- 1. SQL 迁移文件：internal/storage/migrations/0002_us11_us12_resources.sql。
--    **只**复制本文件“v2 新增”的 CREATE TABLE / CREATE INDEX 段（全部 IF NOT EXISTS）；
--    不改既有表、不删列、不重建表。新库由本文件全量 DDL 直接建到 v2。
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

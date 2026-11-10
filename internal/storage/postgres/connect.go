package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 PostgreSQL 谱系的**启动校验**（设计 §3.1.1 / 图 1）：连接、扩展能力、schema
// 版本三者任一不满足即拒绝启动——**没有**降级分支、**不**回退 SQLite、**不**重试成循环。
// This file is the PostgreSQL startup gate: connection, extension capabilities and schema
// version all have to pass; there is no degraded branch and no SQLite fallback.

// SupportedSchemaVersion 是 PostgreSQL 谱系的当前版本。它与 SQLite 谱系的
// `storage.SupportedSchemaVersion`（6）各自独立——两者**不得**互相赋值，也**不得**
// 用同一个数字表达"都最新"。
//
// 版本链：1 = `0001_postgres_v1.sql`（契约基线，`schema.pg.sql` 与它逐字节相等、
// 冻结）；2 = `0002_agent_binding_knowledge_kind.sql`（D-19：绑定投影的
// `resource_kind` 闭集加入 `knowledge_collection`）；3 =
// `0003_knowledge_source_text_repair.sql`（补齐 R59 对 v1 就地改写后既有库缺失的
// `kb_document_versions.source_text`——`ensureSchema` 只跑 `current+1..target`，
// 不会重跑 v1，缺这条脚本的库一读知识库就报列不存在）；4 =
// `0004_knowledge_version_revision.sql`（重建改用不可变版本行：给 `kb_document_versions`
// 加 `revision` 并去掉 `(document_id, content_hash)` 唯一约束）；5 =
// `0005_knowledge_collection_description.sql`（集合简介与 `description_revision`，
// 乐观并发令牌）；6 = `0006_observability.sql`（诊断关联、检索摘要与独立遥测账本）；
// 7 = `0007_history_retention.sql`（关联历史保留账本，只记录清理组与行数）；
// 8 = `0008_flow.sql`（持久流程、版本化验收与动作／祖先预算账本）；
// 9 = `0009_sandbox_execution.sql`（任务隔离生命周期与清理账本）；
// 10 = `0010_skill_admission.sql`（技能学习准入候选、评估、决策与发布记录）；
// 11 = `0011_memory_learning.sql`（记忆抽取账本、游标、候选元数据、候选来源与方法草案，
// 并扩展 source / purpose 两个闭集）；12 = `0012_distributed_execution.sql`
// （Worker 登记、执行权租约、租约状态转换、调度触发去重列与索引、跨 Worker 取消标志、
// `resource_kind` 闭集加入 `agent_bundle`，以及三类在途行的立即过期租约回填）。
// 迁移只追加，**不得**回头改建表字面量——既有库已经按 v1 建过表。
// Version chain: 1 is the frozen contract baseline, 2 widens the binding kind set,
// 3 repairs source_text; 4 gives immutable knowledge versions ordered revisions;
// 5 adds the user-managed collection description with its optimistic-concurrency token;
// 6 adds observability correlation and non-authoritative telemetry ledgers;
// 7 appends the related-history retention ledger; 8 adds durable Flow ledgers;
// 9 adds the sandbox execution ledger; 10 adds the skill admission ledgers;
// 11 adds the memory learning ledgers and widens the source/purpose closed sets;
// 12 adds worker nodes, execution leases with their transition log, DB-claimed scheduling
// columns and indexes, the cross-worker cancel flag, and the agent_bundle resource kind;
// 13 = `0013_agent_bundle_payloads.sql`（已发布 bundle 的内容寻址载荷表、
// `worker_nodes.heartbeat_interval_seconds`、`resource_pointers` 键集索引、
// `task_executions.pinned_bundle_hash` 与到期扫描的非 NULL 段索引）。
// Migrations are append-only: published scripts are never rewritten.
const SupportedSchemaVersion = 13

// undefinedTableSQLState 是 PostgreSQL 的 `undefined_table`（42P01）：它把"版本表不存在"
// 从其他查询失败里区分出来，避免把连接故障误判成"未初始化"。
// undefinedTableSQLState separates "version table missing" from other query failures.
const undefinedTableSQLState = "42P01"

// Config 是打开 PG 后端的全部参数（设计 §3.1.1）。`DSN` 是**已解析**的连接串：
// `${ENV_VAR}` 占位在装配层解析，残留占位或空值在这里拒绝。
// Config carries the fully resolved DSN; placeholder resolution belongs to the wiring
// layer, and an empty or unresolved value is rejected here.
type Config struct {
	DSN              string
	MaxConns         int
	MinConns         int
	ConnectTimeout   time.Duration
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

// DB 是打开后的 PostgreSQL 句柄。T011 只装配启动校验需要的字段与方法；`Migrate` /
// `Tx` / `VerifyTx` / `MarkFirstWrite` 由后续任务在同一类型上追加——**不得**在别处
// 另立第二个句柄类型。
// DB is the opened PostgreSQL handle. Later tasks extend this type with Migrate/Tx/
// VerifyTx/MarkFirstWrite; there is never a second handle type.
type DB struct {
	auditObserver atomic.Pointer[func()]
	pool          *sql.DB
	cfg           Config
	// now 是句柄时钟：事务起止、审计时间都取它；测试可注入固定时刻。
	// now is the handle clock; tests inject a fixed instant instead.
	now func() time.Time
	// faults 是**仅测试构建可见**的提交故障钩子（设计 §3.1.1 `DB` 行）：生产构建里
	// `newCommitFaults` 返回 nil，事务与核查路径都不进入任何注入分支。
	// faults is the integration-only commit-fault hook; it is nil in release builds.
	faults *commitFaults
}

// clock 返回当前时刻；未注入时退回系统时钟（结构体字面量构造的句柄也安全）。
// clock falls back to the system clock for handles built without one.
func (db *DB) clock() time.Time {
	if db.now != nil {
		return db.now()
	}
	return time.Now()
}

// 编译期断言：PG 句柄满足 core 的只读自省接口（接口在消费方，实现在这里）。
var _ core.SchemaIntrospector = (*DB)(nil)

// Queryer 是启动校验与迁移共用的最小查询面：`*sql.DB` / `*sql.Conn` / `*sql.Tx` 都满足。
// Queryer is the minimal query surface shared by startup checks and migrations.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ExtensionRequirement 是一条扩展要求。`MinCapability` 为空表示"只要求扩展已安装"；
// 非空时**必须**能探测到对应能力，探测不到一律拒绝——能力名字面量集中登记在
// `capabilityProbes`，未知能力按"无法验证"拒绝，**不得**当作满足。
// An empty MinCapability requires presence only; a named capability that cannot be
// probed is refused rather than assumed.
type ExtensionRequirement struct {
	Name          string
	MinCapability string
}

// requiredExtensions 是本 US 的扩展前置。两代冻结依据：
//   - `vector`：0.8.x 的 HNSW 与**过滤后迭代扫描**（`hnsw.iterative_scan`）是检索路径的
//     必需能力（D-04 实测 0.8.7；D-06 索引代际 1 走精确扫描但能力探测仍按已冻结要求保留）；
//   - `pg_trgm`：代际 1 关键词路径的三元组索引（D-07，2026-10-28 冻结）。
//
// 缺失时由部署者安装（`CREATE EXTENSION`），应用**不**自动创建——见 schema.pg.sql D7。
// The required extensions are installed by the deployer; the app never creates them.
var requiredExtensions = []ExtensionRequirement{
	{Name: "vector", MinCapability: "hnsw_iterative_scan"},
	{Name: "pg_trgm"},
}

// capabilityProbes 把能力名映射成一条**自足**的探针 SQL：同一句里先求值一次扩展类型
// 转换（触发扩展库加载并注册 GUC），再用 `current_setting(name, true)` 读值。
//
// 为什么不能只写 `SELECT current_setting('hnsw.iterative_scan', true)`：扩展库未加载的
// 新会话里该调用返回 NULL（2026-10-28 在 PG 17.11 + vector 0.8.7 实测），会把"能力存在"
// 误判成"能力缺失"。CTE 用 `MATERIALIZED` 固定求值顺序，`current_setting` 的第二个参数
// 让未注册的名字返回 NULL 而不是报错。两条独立语句不能替代：连接池会把它们派到不同
// 连接上，而 GUC 是会话级的。
//
// 类型名**必须**按扩展实际安装的 schema 限定（取自 `pg_extension.extnamespace`）：
// 未限定的 `vector` 按会话 `search_path` 解析，`pgtest` 的隔离 schema 与自定义
// `search_path` 的部署里都会 42704（2026-10-28 实测）。探针模板收 schema 入参正是
// 为了不依赖会话状态。
//
// Each probe loads the extension library and reads the setting in one statement. The
// type name is schema-qualified from pg_extension.extnamespace because an unqualified
// name resolves through the session search_path and fails under isolated schemas.
var capabilityProbes = map[string]func(extSchema string) string{
	"hnsw_iterative_scan": func(extSchema string) string {
		qualifiedType := pgx.Identifier{extSchema}.Sanitize() + ".vector"
		return "WITH load AS MATERIALIZED (SELECT '[0]'::" + qualifiedType + " AS v)\n" +
			"SELECT current_setting('hnsw.iterative_scan', true) FROM load"
	},
}

// Open 装配唯一运行后端：连接 → 扩展能力 → schema（建库 / 前向迁移），任一失败即拒绝
// 启动。**没有**降级分支：不回退 SQLite、不重试成循环、不自动修补结构。
// 这是**控制面**入口：它拥有迁移权（migrate.go 的 `DB.Migrate`）。Worker 走
// `OpenForWorker`——共享定义的迁移权只归控制面，见该函数的说明。
// Open assembles the control-plane runtime backend; every prerequisite is fail-closed
// with no degraded branch. Workers use OpenForWorker instead.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	return openWith(ctx, cfg, true)
}

// OpenForWorker 装配 **Worker** 后端：连接 → 扩展能力 → **精确版本校验**，不迁移。
//
// 为什么单列一个入口：迁移会写共享 schema（建表、加列、回填），那是控制面的职责；
// Worker 只允许读共享定义与写执行协调列。要求 `current == SupportedSchemaVersion`
// （精确相等）而不是"低于就迁"——低一版说明控制面还没升级完，Worker 必须失败退出
// 并点名先启动控制面，**不得**自行前向迁移，也不得容忍"差一点也能跑"。
// 库版本高于二进制同样是拒绝（旧二进制不得打开新库）。
// OpenForWorker opens the worker-side backend: connection, extension check and an
// exact schema-version equality check. It never migrates — schema evolution belongs to
// the control plane, and a mismatch is a fail-closed startup error.
func OpenForWorker(ctx context.Context, cfg Config) (*DB, error) {
	return openWith(ctx, cfg, false)
}

// openWith 是两个入口的共同装配序列；`migrate` 只决定最后一步是迁移还是精确版本校验。
// openWith is the shared assembly sequence; migrate only chooses the last step.
func openWith(ctx context.Context, cfg Config, migrate bool) (*DB, error) {
	pool, err := openPool(ctx, cfg)
	if err != nil {
		return nil, err
	}
	db := &DB{pool: pool, cfg: cfg, now: time.Now}
	// 生产构建里 `newCommitFaults` 返回 nil：这一行不留下任何注入面。
	db.faults = newCommitFaults(db)
	if err := db.Ping(ctx); err != nil {
		return nil, closeOnError(db, err)
	}
	// 扩展先于建表：新库的 `vector(n)` 列依赖扩展已安装（设计 图 1 的顺序不是随意的）。
	if err := CheckExtensions(ctx, pool, requiredExtensions); err != nil {
		return nil, closeOnError(db, err)
	}
	if !migrate {
		if err := verifySchemaVersionExact(ctx, pool, SupportedSchemaVersion); err != nil {
			return nil, closeOnError(db, err)
		}
		return db, nil
	}
	// 建库与前向迁移在 advisory 锁内完成（migrate.go 的 `DB.Migrate`）；失败整体回滚。
	// `db migrate` 与这里走的是**同一个**方法——测试与运维看到的行为不来自两套实现。
	// The schema work is the very same DB.Migrate the CLI calls; there is no second path.
	if err := db.Migrate(ctx); err != nil {
		return nil, closeOnError(db, err)
	}
	return db, nil
}

// OpenReadOnly 打开一个**诊断用**的只读句柄（`basalt status`、轻路径的只读命令）：
// 连接 + Ping，**不**校验扩展、**不**建库 / 迁移，会话固定为只读事务
// （`default_transaction_read_only=on`）——它不可能顺手改库。运行期后端**不得**用它：
// `serve` 只经 `Open`（D-01：缺 DSN / 扩展不足一律拒绝启动，不回退、不降级）。
// OpenReadOnly is the diagnostic read-only open: no extension check, no migration and
// a read-only session. The runtime backend is only ever opened through Open.
func OpenReadOnly(ctx context.Context, cfg Config) (*DB, error) {
	pool, err := openReadOnlyPool(ctx, cfg)
	if err != nil {
		return nil, err
	}
	db := &DB{pool: pool, cfg: cfg, now: time.Now}
	db.faults = newCommitFaults(db)
	if err := db.Ping(ctx); err != nil {
		return nil, closeOnError(db, err)
	}
	return db, nil
}

// openPool 建立连接池：`pgx/v5/stdlib`，DSN 规范化后固定 `TimeZone=UTC` 与
// `target_session_attrs=read-write`。后者保证后续事务核查**只打主库**——只读副本的
// 滞后快照会把已提交读成未提交（C-34）。
// openPool normalizes the DSN and pins the session timezone and primary-only connections.
func openPool(ctx context.Context, cfg Config) (*sql.DB, error) {
	return openPoolWith(ctx, cfg, false)
}

// openReadOnlyPool 与 openPool 同源，只多一条逐连接会话参数：默认事务只读。
// 只读是**服务端**保证的，不依赖调用方自觉。
// openReadOnlyPool adds one per-connection setting to the very same pool: every
// transaction defaults to read-only, enforced by the server.
func openReadOnlyPool(ctx context.Context, cfg Config) (*sql.DB, error) {
	return openPoolWith(ctx, cfg, true)
}

func openPoolWith(ctx context.Context, cfg Config, readOnly bool) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("装配 PostgreSQL 连接池: %w", err)
	}
	connCfg, err := parseConnConfig(cfg)
	if err != nil {
		return nil, err
	}
	if readOnly {
		connCfg.RuntimeParams["default_transaction_read_only"] = "on"
		// `target_session_attrs=read-write` 的校验函数读的正是 `SHOW transaction_read_only`，
		// 与上面这条会话参数直接冲突：新连接在启动包里就被设成只读，校验随即把它判成
		// "只读副本"并拒绝连接（实测报错 `ValidateConnect failed: read only connection`）。
		// 只读诊断句柄仍须只打主库（C-34），但判据换成 `pg_is_in_recovery() = false`——
		// "事务只读"与"是不是主库"是两个问题，不能用一个判据一起判。
		// The read-write validator reads SHOW transaction_read_only and therefore rejects
		// the very session parameter we just set; primary-only becomes the right check.
		connCfg.Config.ValidateConnect = pgconn.ValidateConnectTargetSessionAttrsPrimary
	}
	// stdlib.OpenDB 只装配驱动，不拨号（与 `import _` + sql.Open 等价，但不依赖驱动名
	// 字面量）；真正的连通性由 Open 的 Ping 判定。
	pool := stdlib.OpenDB(*connCfg)
	if cfg.MaxConns > 0 {
		pool.SetMaxOpenConns(cfg.MaxConns)
	}
	if cfg.MinConns > 0 {
		// `database/sql` 没有"最小连接数"语义：按最大空闲连接数落地，池不预热。
		// database/sql has no minimum-connection notion; this maps to the idle ceiling.
		pool.SetMaxIdleConns(cfg.MinConns)
	}
	return pool, nil
}

// parseConnConfig 把配置装配成 pgx 连接配置：DSN 规范化 → 解析 → 连接超时 → 逐连接
// 会话参数。抽成独立函数是为了让"每个新连接都会被执行哪些会话设置"可在**无库**单测里
// 逐项断言——池本身要连上库才可见，配置的漏设不该等到集成用例才暴露。
// parseConnConfig is separated from openPool so the per-connection session settings are
// assertable without a live database.
func parseConnConfig(cfg Config) (*pgx.ConnConfig, error) {
	dsn, err := normalizeDSN(cfg.DSN)
	if err != nil {
		return nil, err
	}
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// 不把 DSN 拼进文案：连接串可能含口令（凭证不回显，CLAUDE.md 硬性禁止）。
		return nil, fmt.Errorf("解析 PostgreSQL 连接串: %w", err)
	}
	if cfg.ConnectTimeout > 0 {
		connCfg.ConnectTimeout = cfg.ConnectTimeout
	}
	// 语句 / 锁等待超时是**逐连接**的会话参数（设计 §3.3 `database.statement_timeout_seconds`
	// / `lock_timeout_seconds`：「逐连接设置」）。写进 RuntimeParams 后随每条新连接的启动包
	// 下发——连接池里没有一条会漏设。配置值优先于连接串里的同名字段：两者同时出现时结果
	// 由配置决定，不取决于 DSN 长什么样（与 normalizeDSN 删重复参数的纪律同源）。
	// The two timeouts are per-connection session settings delivered in each connection's
	// startup packet; the config value wins over a same-named DSN field.
	if cfg.StatementTimeout > 0 {
		connCfg.RuntimeParams["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	}
	if cfg.LockTimeout > 0 {
		connCfg.RuntimeParams["lock_timeout"] = strconv.FormatInt(cfg.LockTimeout.Milliseconds(), 10)
	}
	return connCfg, nil
}

// normalizeDSN 把两种合法写法（`postgres://` URL / `keyword=value`）统一成带上
// `TimeZone=UTC` 与 `target_session_attrs=read-write` 的连接串。已有的同名参数（含
// 大小写变体）先删再写：重复的 runtime 参数在 PostgreSQL 侧按到达顺序生效，留着两份
// 等于把"用哪个时区"交给不确定顺序。
// normalizeDSN pins the timezone and primary-only attributes for both DSN spellings,
// removing case-variant duplicates first so the outcome never depends on arrival order.
func normalizeDSN(dsn string) (string, error) {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return "", errors.New("PostgreSQL 连接串为空——database.dsn 必须解析为实际值，残留 ${...} 占位同样按空值处理")
	}
	u, parseErr := url.Parse(trimmed)
	if parseErr == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		for key := range q {
			if strings.EqualFold(key, "TimeZone") || strings.EqualFold(key, "target_session_attrs") {
				q.Del(key)
			}
		}
		q.Set("TimeZone", "UTC")
		q.Set("target_session_attrs", "read-write")
		u.RawQuery = q.Encode()
		return u.String(), nil
	}
	if strings.Contains(trimmed, "://") {
		return "", errors.New("无法解析 PostgreSQL 连接串——须为 postgres:// URL 或 keyword=value 形式")
	}
	// keyword=value 形式：同名键后出现的覆盖先出现的（pgconn 按出现顺序赋值）。
	return trimmed + " TimeZone=UTC target_session_attrs=read-write", nil
}

// Ping 探活连接池。失败原样上抛，**不**吞错、**不**伪造可用。
// Ping checks the pool; failures are never masked as "ok".
func (db *DB) Ping(ctx context.Context) error {
	if db == nil || db.pool == nil {
		return errors.New("postgres: 句柄未装配连接池，无法探活")
	}
	if err := db.pool.PingContext(ctx); err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	return nil
}

// Close 释放连接池；未装配的句柄按已关闭处理（幂等）。
// Close releases the pool; an unwired handle counts as already closed.
func (db *DB) Close() error {
	if db == nil || db.pool == nil {
		return nil
	}
	return db.pool.Close()
}

// SchemaVersion 返回库内 schema_version；版本表不存在 = 尚未初始化，零行 = 损坏态。
// SchemaVersion reports the stored version; a missing table means "not initialized",
// zero rows means corruption.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	return schemaVersion(ctx, db.pool)
}

// TableNames 返回当前 schema 的用户表名（不含 PostgreSQL 系统 schema），按名排序。
// TableNames lists the user tables of the current schema, sorted by name.
func (db *DB) TableNames(ctx context.Context) ([]string, error) {
	return tableNames(ctx, db.pool)
}

// CheckExtensions 逐条核对扩展与能力（设计 §3.1.1）。任一缺失即返回错误，文案点名
// 扩展名与所需能力，并给出部署者要执行的动作——错误信息是运维看到的唯一线索，
// **不得**只报"扩展检查失败"。
// CheckExtensions verifies presence and capabilities; the error names the extension,
// the capability and the deployer's remedy.
func CheckExtensions(ctx context.Context, q Queryer, reqs []ExtensionRequirement) error {
	for _, req := range reqs {
		var version, extSchema string
		err := q.QueryRowContext(ctx, `
			SELECT e.extversion, n.nspname
			FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
			WHERE e.extname = $1`, req.Name).Scan(&version, &extSchema)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("扩展 %s 缺失：请部署者在目标库执行 CREATE EXTENSION %s 后重启（应用不自动创建）", req.Name, req.Name)
		case err != nil:
			return fmt.Errorf("检查扩展 %s: %w", req.Name, err)
		}
		if req.MinCapability == "" {
			continue
		}
		buildProbe, ok := capabilityProbes[req.MinCapability]
		if !ok {
			return fmt.Errorf("扩展 %s 要求能力 %s，但本构建没有该能力的探针——无法验证即拒绝启动", req.Name, req.MinCapability)
		}
		var value sql.NullString
		if err := q.QueryRowContext(ctx, buildProbe(extSchema)).Scan(&value); err != nil {
			return fmt.Errorf("探测扩展 %s 的能力 %s: %w", req.Name, req.MinCapability, err)
		}
		if !value.Valid || value.String == "" {
			return fmt.Errorf("扩展 %s（版本 %s）缺少所需能力 %s——请升级扩展后再启动", req.Name, version, req.MinCapability)
		}
	}
	return nil
}

// verifySchemaVersion 只做**拒绝判定**，不做建库 / 迁移（那是 ensureSchema 的职责）：
// 空库放行、版本更高拒绝、版本表缺失但有表拒绝、版本表零行拒绝、低版本放行交给前向迁移。
// 它被调用两次——事务外预检（尽早报错）与 advisory 锁内重断言（并发安全判据）。
// verifySchemaVersion only refuses unusable states; ensureSchema runs it both before
// the transaction (fail fast) and again inside the advisory lock (authoritative).
func verifySchemaVersion(ctx context.Context, q Queryer, target int) error {
	if target < 1 {
		return fmt.Errorf("postgres: 目标版本 %d 非法——版本从 1 起", target)
	}
	current, err := schemaVersion(ctx, q)
	if errors.Is(err, core.ErrDatabaseMissing) {
		tables, listErr := tableNames(ctx, q)
		if listErr != nil {
			return listErr
		}
		if len(tables) > 0 {
			return fmt.Errorf("%w: 库内有表但无 schema_version 表（%s）——不是可识别的空库",
				core.ErrSchemaCorrupt, strings.Join(tables, ", "))
		}
		// 空库：全量建 v1 由 ensureSchema 负责，启动校验到此通过。
		return nil
	}
	if err != nil {
		return err
	}
	if current < 1 {
		return fmt.Errorf("%w: schema_version 记录 %d 非法——版本从 1 起", core.ErrSchemaCorrupt, current)
	}
	if current > target {
		return fmt.Errorf("%w: 库版本 %d 高于二进制支持的 %d——旧二进制不得打开新库", core.ErrSchemaTooNew, current, target)
	}
	// 1 <= current < target：前向迁移由 ensureSchema 在 advisory lock 内完成。
	return nil
}

// verifySchemaVersionExact 是 **Worker 入口**的版本判定：要求 `current` 与 `target`
// **精确相等**，不迁移、不放行低版本。
//
// 为什么不复用 `verifySchemaVersion`：那个函数的语义是"能不能从这里前向迁到 target"，
// `1 ≤ current < target` 是**允许**的（随后由 ensureSchema 迁移）；Worker 恰恰不能迁移，
// 把那个判定搬过来会让"控制面还没升级完"的库被 Worker 当成可以启动。
// 失败文案点名动作：版本表缺失 / 版本更低 → 先由控制面启动完成初始化与迁移；
// 版本更高 → 旧二进制不得打开新库（与 `ErrSchemaTooNew` 同一语义）。
// verifySchemaVersionExact is the worker-side verdict: exact equality, never a migration.
func verifySchemaVersionExact(ctx context.Context, q Queryer, target int) error {
	if target < 1 {
		return fmt.Errorf("postgres: 目标版本 %d 非法——版本从 1 起", target)
	}
	current, err := schemaVersion(ctx, q)
	if errors.Is(err, core.ErrDatabaseMissing) {
		return fmt.Errorf("%w: 库尚未初始化——Worker 不执行建库 / 迁移，请先启动控制面完成初始化", core.ErrDatabaseMissing)
	}
	if err != nil {
		return err
	}
	switch {
	case current == target:
		return nil
	case current > target:
		return fmt.Errorf("%w: 库版本 %d 高于二进制支持的 %d——旧二进制不得打开新库",
			core.ErrSchemaTooNew, current, target)
	default:
		return fmt.Errorf("库版本 %d 低于二进制要求的 %d——Worker 不执行迁移，请先由控制面完成升级", current, target)
	}
}

// schemaVersion 读 `schema_version` 的最大版本。版本表不存在映射成
// `core.ErrDatabaseMissing`（"尚未初始化"，与 SQLite 侧同一语义），零行映射成
// `core.ErrSchemaCorrupt`；其他失败原样包装，**不得**当成"空库"放行。
//
// 先查 `to_regclass` 再查表，而不是直接查表等 42P01：**事务内**一条语句失败会把整个事务
// 置为 aborted（25P02），锁内重断言就再也查不动了（2026-10-28 实测暴露）。`to_regclass`
// 对不存在的表返回 NULL 而不报错，两种上下文都能用；42P01 分支保留作防御（表在两条语句
// 之间被并发删除）。
// Probe with to_regclass first: a failing statement inside a transaction aborts it
// (25P02), which broke the in-lock re-assertion. The 42P01 branch is kept as defense.
func schemaVersion(ctx context.Context, q Queryer) (int, error) {
	var present sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('schema_version')::text`).Scan(&present); err != nil {
		return 0, fmt.Errorf("探测 PostgreSQL schema_version: %w", err)
	}
	if !present.Valid {
		return 0, fmt.Errorf("%w: schema_version 表不存在", core.ErrDatabaseMissing)
	}
	var current sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT max(version) FROM schema_version`).Scan(&current)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == undefinedTableSQLState:
		return 0, fmt.Errorf("%w: schema_version 表不存在", core.ErrDatabaseMissing)
	case err != nil:
		return 0, fmt.Errorf("读取 PostgreSQL schema_version: %w", err)
	case !current.Valid:
		return 0, core.ErrSchemaCorrupt
	}
	return int(current.Int64), nil
}

// TableNames 是 tableNames 的导出面：`dbmigrate` 的窗口行数快照需要枚举当前 schema 的
// 用户表。查询实现只有一份——导出与否不改变"当前 schema、BASE TABLE、固定排序"的口径。
// TableNames exposes the same enumeration to dbmigrate for its row-count snapshot.
func TableNames(ctx context.Context, q Queryer) ([]string, error) { return tableNames(ctx, q) }

// tableNames 返回当前 schema 的用户表（`information_schema` 只列可访问的表；系统
// schema 由 `current_schema()` 排除）。排序固定，便于错误文案与测试断言逐字比对。
// tableNames lists the current schema's base tables in a stable order.
func tableNames(ctx context.Context, q Queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		return nil, fmt.Errorf("清点 PostgreSQL 用户表: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读取表名: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历表名: %w", err)
	}
	return names, nil
}

// closeOnError 把启动失败的句柄关掉再返回：带错误的连接池不留给调用方，关闭失败
// 并入错误链（`errors.Join`），不覆盖原始原因。
// closeOnError closes a failing handle and keeps the original cause in the chain.
func closeOnError(db *DB, err error) error {
	return errors.Join(err, db.Close())
}

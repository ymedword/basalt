//go:build integration

package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtemp"
)

// 本文件是启动校验的**有库**用例：每个用例在测试库里建独立 schema 并在连接上固定
// `search_path`——`pg_extension` 是库级对象、不能按 schema 隔离，故扩展用例改用
// "不可能存在的扩展 / 能力"走拒绝路径，真实声明另用正例钉住。
// The database-backed half of the startup gate. Extension negatives use impossible
// names because pg_extension is database-scoped and cannot be isolated per schema.

// testDSN 取集成测试连接串。缺环境即**失败**（D-01：缺 DSN 不跳过，也不降级成
// SQLite）——跳过会让 `go test -tags=integration` 在无库机器上"全绿"，那正是本 US
// 要消灭的假绿。排查路径写进报文，避免只看到一堆用例失败却不知道缺什么。
// testDSN fails (never skips) when the integration DSN is absent: a skip would turn a
// database-less machine green, which is exactly the false pass D-01 forbids.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BASALT_TEST_PG_DSN")
	if dsn == "" {
		t.Fatalf("未设置 BASALT_TEST_PG_DSN——集成用例需要真实 PostgreSQL（D-01：缺省即失败，不跳过）。" +
			"本地：启动 dev-env 测试库后经 dev-env/scripts/with-postgres.sh 运行，环境准备见 docs/DevelopmentLoop.md §10；" +
			"CI：使用 postgres service 容器注入的 DSN（.github/workflows/ci.yml）")
	}
	return dsn
}

// testPool 建一个独立连接池；每个调用点都拿到新池，避免前一个用例加载过的扩展库
// 影响"未加载态"的断言。
// testPool returns a fresh pool; extension-library load state is per backend.
func testPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := openPool(context.Background(), Config{DSN: testDSN(t), ConnectTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	return pool
}

// isolatedConn 在测试库里建一个随机命名的 schema，把连接钉在该 schema 上（只含该
// schema 的 search_path）。需要同时解析扩展类型的用例用 isolatedSchemaConn 拿到 schema 名
// 再自行拼 search_path。
// isolatedConn pins one connection to a fresh schema whose search_path contains only
// that schema; callers that also need extension types use isolatedSchemaConn.
func isolatedConn(t *testing.T) *sql.Conn {
	t.Helper()
	conn, _ := isolatedSchemaConn(t)
	return conn
}

// isolatedSchemaConn 是 isolatedConn 的带 schema 名版本：迁移用例要把扩展 schema 也放进
// search_path（`vector(n)` 与 `gin_trgm_ops` 都按 search_path 解析），否则 DDL 会 42704。
// 独立连接（`*sql.Conn`）而不是连接池：`SET search_path` 是会话级状态，池会把后续查询
// 派到别的连接。
// isolatedSchemaConn also returns the schema name so migration tests can append the
// extension schemas to search_path.
func isolatedSchemaConn(t *testing.T) (*sql.Conn, string) {
	t.Helper()
	ctx := context.Background()
	pool := testPool(t)
	conn, err := pool.Conn(ctx)
	require.NoError(t, err)
	// 停机迁移用例会**主动**先关这条连接（模拟停机后再由 `Open` 迁移），cleanup 因此
	// 必须容忍"已关闭"：`sql.ErrConnDone` 之外的错误照旧判失败。
	t.Cleanup(func() {
		if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			require.NoError(t, err)
		}
	})

	schema := createSchema(t, pool)
	_, err = conn.ExecContext(ctx, "SET search_path TO "+schema)
	require.NoError(t, err)
	return conn, schema
}

// createSchema 建一个随机命名的 schema 并在用例结束时删除。删除走池上的另一条连接：
// 钉住 schema 的用例连接可能还开着，DROP 由别的会话执行不影响。
// createSchema creates a randomly named schema and drops it at cleanup time.
func createSchema(t *testing.T, pool *sql.DB) string {
	t.Helper()
	schema := "basalt_startup_" + randomSuffix(t)
	_, err := pool.ExecContext(context.Background(), "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		// 回收按对象计锁：`DROP SCHEMA ... CASCADE` 在删每个表 / 索引时持锁（实测 1,597
		// 条），几十个并行用例叠加会打爆测试库锁表（SQLSTATE 53200）。因此回收与迁移一样
		// 走 `pgtemp` 的跨进程闸门；排队本身给 3 分钟上限，槽位泄漏时失败落在这一次清理上。
		// DROP goes through the shared cross-process gate for the same lock-count reason.
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 3*time.Minute)
		release, releaseErr := pgtemp.AcquireDropSlot(waitCtx)
		cancelWait()
		require.NoError(t, releaseErr)
		defer release()
		_, dropErr := pool.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, dropErr)
	})
	return schema
}

// openMigratedTestDB 在夹具迁移槽位内跑 `Open`（内部执行全量迁移并加 `schema_version`）。
// 全量迁移在一个事务里持约 1,136 个锁表条目，本包几十个并行用例各自迁移会打爆测试库锁表
// （2026-11-05 实测 SQLSTATE 53200）。槽位在迁移返回后**立刻**释放，不跨用例持有——否则
// 并行的迁移用例会互相饿死。迁移并发用例（`migrate_integration_test.go`）**不走**这里：
// 它们要验证的正是「同 schema 互斥 / 不同 schema 并行」，必须直接调用迁移路径。
// openMigratedTestDB runs Open under the shared migration slot and releases it as soon
// as the migration returns; the migration-concurrency cases deliberately bypass it.
func openMigratedTestDB(t *testing.T, cfg Config) *DB {
	t.Helper()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 3*time.Minute)
	release, err := pgtemp.AcquireMigrationSlot(waitCtx)
	cancelWait()
	require.NoError(t, err)
	defer release()
	db, err := Open(context.Background(), cfg)
	require.NoError(t, err)
	return db
}

// withSearchPath 把会话 `search_path` 钉到给定 schema 列表。`Open` 会在连接池上建库，
// 不钉住就会迁到该库的默认 schema——测试库的 `public` 因此被整套表污染。pgx 把连接串里
// 的未知键当会话运行时参数下发（pgconn/config.go 的 RuntimeParams），URL 与
// keyword=value 两种写法都支持。
// withSearchPath pins the session search_path; pgx forwards unknown DSN keys as
// session runtime parameters.
func withSearchPath(t *testing.T, dsn string, paths ...string) string {
	t.Helper()
	joined := strings.Join(paths, ",")
	if !strings.Contains(dsn, "://") {
		return dsn + " search_path=" + joined
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", joined)
	u.RawQuery = q.Encode()
	return u.String()
}

// randomSuffix 产生 16 位十六进制后缀，保证并行用例的 schema 名不冲突。
func randomSuffix(t *testing.T) string {
	t.Helper()
	var buf [8]byte
	_, err := rand.Read(buf[:])
	require.NoError(t, err)
	return hex.EncodeToString(buf[:])
}

// TestStartupVerification_AcceptsEmptySchema 判空库放行的语义边界：**放行**不等于建表，
// 建库仍归 ensureSchema；此时读取版本应得到"尚未初始化"。
// 逆改点：把"空库"分支改成报错 → 本用例红；把 42P01 映射删掉 → ErrorIs 断言红。
func TestStartupVerification_AcceptsEmptySchema(t *testing.T) {
	conn := isolatedConn(t)
	ctx := context.Background()
	require.NoError(t, verifySchemaVersion(ctx, conn, SupportedSchemaVersion), "空库必须放行到建库步骤")

	_, err := schemaVersion(ctx, conn)
	require.ErrorIs(t, err, core.ErrDatabaseMissing, "无版本表 = 尚未初始化")
}

// TestStartupVerification_RefusesUnknownTablesWithoutVersionTable 判"有表但无版本表"
// 不是可识别的空库——拒绝启动并点名遗留表，避免把别人的库当新库全量建表。
// 逆改点：删掉 UnknownTables 分支 → 本用例拿到 nil error 而红。
func TestStartupVerification_RefusesUnknownTablesWithoutVersionTable(t *testing.T) {
	conn := isolatedConn(t)
	ctx := context.Background()
	_, err := conn.ExecContext(ctx, "CREATE TABLE leftover_notes (id integer)")
	require.NoError(t, err)

	err = verifySchemaVersion(ctx, conn, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaCorrupt)
	require.Contains(t, err.Error(), "leftover_notes", "文案必须点名遗留表")
	require.Contains(t, err.Error(), "schema_version")
}

// TestStartupVerification_RefusesNewerSchema 判旧二进制不得打开新库。
// 逆改点：去掉 `current > target` 分支 → 本用例红。
func TestStartupVerification_RefusesNewerSchema(t *testing.T) {
	conn := isolatedConn(t)
	ctx := context.Background()
	createVersionTable(t, conn)
	// "更新的库"取 SupportedSchemaVersion+1 而不是字面量：D-19 把谱系前移到 v2 后，
	// 写死 2 会变成"当前版本"、把拒绝路径测成放行路径。逆改点因此仍然成立。
	// A newer library is SupportedSchemaVersion+1, so the refusal stays discriminating
	// after every forward migration.
	future := SupportedSchemaVersion + 1
	_, err := conn.ExecContext(ctx,
		"INSERT INTO schema_version (version, applied_at, description) VALUES ($1, 0, 'future')", future)
	require.NoError(t, err)

	err = verifySchemaVersion(ctx, conn, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaTooNew)
	require.Contains(t, err.Error(), "旧二进制不得打开新库")
}

// TestStartupVerification_RefusesCorruptVersionTable 判版本表零行是损坏态，不是"空库"。
// 逆改点：把 `!current.Valid` 分支改成返回 0 → 用例红（0 会被当作旧版本放行）。
func TestStartupVerification_RefusesCorruptVersionTable(t *testing.T) {
	conn := isolatedConn(t)
	createVersionTable(t, conn)

	err := verifySchemaVersion(context.Background(), conn, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaCorrupt)
}

// TestStartupVerification_AcceptsCurrentSchema 判当前版本放行，且自省接口读回同一个值。
// 逆改点：把 `max(version)` 读成 `min` / 跳过 scan → 版本断言红。
func TestStartupVerification_AcceptsCurrentSchema(t *testing.T) {
	conn := isolatedConn(t)
	ctx := context.Background()
	createVersionTable(t, conn)
	// 与 `ensureSchema` 落库的结果同形：每一版一行，末行是当前版本。写死 v1 会在
	// D-19 前移后把"当前版本"测成"旧版本"；逐版插入同时让"max 读成 min"的逆改仍会红。
	// Seed one row per migration, exactly as ensureSchema does, so the max/min mutation
	// stays discriminating after each forward migration.
	for version := 1; version <= SupportedSchemaVersion; version++ {
		_, description, err := migrationScript(version)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx,
			"INSERT INTO schema_version (version, applied_at, description) VALUES ($1, 0, $2)",
			version, description)
		require.NoError(t, err)
	}

	require.NoError(t, verifySchemaVersion(ctx, conn, SupportedSchemaVersion))
	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)

	tables, err := tableNames(ctx, conn)
	require.NoError(t, err)
	require.Contains(t, tables, "schema_version")
}

// TestStartupVerification_RefusesMissingExtension 判缺失扩展拒绝启动，且文案点名扩展与
// 部署者动作（错误信息是运维唯一线索，不许只报"检查失败"）。
// 逆改点：把 ErrNoRows 分支改成 continue → 本用例红。
func TestStartupVerification_RefusesMissingExtension(t *testing.T) {
	conn := isolatedConn(t)
	err := CheckExtensions(context.Background(), conn,
		[]ExtensionRequirement{{Name: "basalt_missing_extension"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "basalt_missing_extension")
	require.Contains(t, err.Error(), "CREATE EXTENSION")
}

// TestStartupVerification_RefusesUnknownCapability 判"声明了能力但没有探针"按无法验证
// 拒绝——**不得**默认放行（fail-closed）。
// 逆改点：把未知探针分支改成 continue → 本用例红。
func TestStartupVerification_RefusesUnknownCapability(t *testing.T) {
	conn := isolatedConn(t)
	err := CheckExtensions(context.Background(), conn,
		[]ExtensionRequirement{{Name: "vector", MinCapability: "basalt_missing_capability"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "basalt_missing_capability")
	require.Contains(t, err.Error(), "无法验证即拒绝启动")
}

// TestStartupVerification_RequiredExtensionsInstalled 是扩展前置的正例：本机测试库
// （D-04 冻结环境）必须满足 `vector` + `pg_trgm` 全部声明。
// 逆改点：把 requiredExtensions 里的 vector 能力名改成未实现的名字 → 未知探针分支
// 让本用例红。
func TestStartupVerification_RequiredExtensionsInstalled(t *testing.T) {
	conn := isolatedConn(t)
	require.NoError(t, CheckExtensions(context.Background(), conn, requiredExtensions))
}

// TestStartupVerification_CapabilityProbeLoadsExtension 是探针判别力的正面判据：
// 未加载扩展库的后端里 `current_setting('hnsw.iterative_scan', true)` 为 NULL，
// 探针必须在同一句里加载库后才读得到值——去掉 MATERIALIZED CTE 的加载步骤即红。
// 该断言依赖测试镜像不预加载 vector（pgvector 官方镜像即如此）；镜像变更时先复核，
// **不得**为此删除断言。
func TestStartupVerification_CapabilityProbeLoadsExtension(t *testing.T) {
	ctx := context.Background()
	// 隔离 schema 的连接：search_path 里没有 public，未限定的 `vector` 会 42704——
	// 探针必须自己按扩展 schema 限定类型名（2026-10-28 首轮实测暴露）。
	conn := isolatedConn(t)

	var bare sql.NullString
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT current_setting('hnsw.iterative_scan', true)`).Scan(&bare))
	require.False(t, bare.Valid, "扩展库未加载时该设置必须为 NULL——否则本用例失去判别力")

	extSchema := extensionSchema(t, conn, "vector")
	var probed sql.NullString
	require.NoError(t, conn.QueryRowContext(ctx,
		capabilityProbes["hnsw_iterative_scan"](extSchema)).Scan(&probed))
	require.True(t, probed.Valid, "探针必须自行加载扩展库后读到设置")
	require.NotEmpty(t, probed.String)
}

// extensionSchema 读扩展实际安装的 schema（探针按它限定类型名）。
func extensionSchema(t *testing.T, q Queryer, name string) string {
	t.Helper()
	var schema string
	require.NoError(t, q.QueryRowContext(context.Background(), `
		SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname = $1`, name).Scan(&schema))
	return schema
}

// TestStartupVerification_OpenPinsUTCAndPrimarySession 判 openPool 的两个强制属性：
// 会话时区固定 UTC（时间列一律 Unix 毫秒 UTC，进程本地时区不得渗进来）、连接只接受
// 可写主库（事务结局核查不能落在只读副本上）。
// 逆改点：normalizeDSN 不写 TimeZone → 时区断言红；不写 target_session_attrs →
// 副本场景才会红（本机主库上 `transaction_read_only` 断言只能证明当前会话可写）。
func TestStartupVerification_OpenPinsUTCAndPrimarySession(t *testing.T) {
	ctx := context.Background()
	// `Open` 会建库（设计 图 1）：会话 search_path 必须钉在独立 schema 上，否则迁移落到
	// 测试库默认 schema。`public` 一旦出现 Basalt 的 `schema_version`，同库其他用例的
	// search_path 会穿透读到它，迁移用例"空转成零表"（2026-10-28 全量 -race 套件实测）。
	// 扩展装在 public 里，仍需留在 search_path 上供 `vector(n)` / `gin_trgm_ops` 解析。
	// Open migrates its target schema, so the test pins search_path to an isolated one.
	pool := testPool(t)
	paths := []string{createSchema(t, pool)}
	seen := map[string]bool{paths[0]: true}
	for _, ext := range []string{"vector", "pg_trgm"} {
		extSchema := extensionSchema(t, pool, ext)
		if seen[extSchema] {
			continue
		}
		seen[extSchema] = true
		paths = append(paths, extSchema)
	}
	db := openMigratedTestDB(t, Config{DSN: withSearchPath(t, testDSN(t), paths...), ConnectTimeout: 5 * time.Second})
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	var timezone string
	require.NoError(t, db.pool.QueryRowContext(ctx, "SHOW TimeZone").Scan(&timezone))
	require.Equal(t, "UTC", timezone)

	var readOnly string
	require.NoError(t, db.pool.QueryRowContext(ctx, "SHOW transaction_read_only").Scan(&readOnly))
	require.Equal(t, "off", readOnly)
}

// createVersionTable 按契约建最小的 `schema_version` 表（列与 schema.pg.sql 一致）。
func createVersionTable(t *testing.T, conn *sql.Conn) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), `
		CREATE TABLE schema_version (
			version     integer PRIMARY KEY,
			applied_at  bigint  NOT NULL,
			description text    NOT NULL
		)`)
	require.NoError(t, err)
}

package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 合成迁移集的 v1 全量脚本：与真实契约同形（版本表 + 一张业务表），只为覆盖
// 逐版前向路径——真实 schema.sql 由 schemaFS 承载。
// The synthetic v1 script mirrors the real contract's shape.
const syntheticV1 = "CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL);\n" +
	"CREATE TABLE a (id INTEGER);\n"

// openBare 打开一个不带 PRAGMA 装配的裸库句柄——Migrate 的调用方形态（US-4 的
// 持锁重路径自己决定连接装配，机制层不替调用方补设置）。
// openBare opens a bare handle: Migrate's caller owns the connection setup.
func openBare(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// countTable 数 sqlite_master 里点名表的存在条数（0 = 不存在）。
// countTable counts occurrences of a table name in sqlite_master.
func countTable(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n))
	return n
}

// mustVersion 读库内当前版本（调用前版本表必有行）。
// mustVersion reads the stored version.
func mustVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	require.NoError(t, db.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&v))
	return v
}

// requireTableExists 断言表存在（失败信息带表名）。
// requireTableExists asserts the table exists.
func requireTableExists(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	require.Equal(t, 1, countTable(t, db, name), "表 %s 应当存在", name)
}

// tableNamesOf 读库内用户表名（排序），与 (*DB).TableNames 同口径。
// tableNamesOf lists user tables, sorted, matching (*DB).TableNames.
func tableNamesOf(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

// contractTables 是契约 schema.sql 建齐的 33 张用户表（v1 十一张 + v2 七张 +
// v3 八张 + v4 两张 + v5 一张 + v6 四张，排序后）。v4 两张 = schedule_deliveries /
// resource_connection_tests；v5 一张 = network_domain_policy（出网域名白名单的唯一权威行）；
// v6 四张 = execution_policy / network_exceptions / instance_secrets / mcp_credential_sources。
// contractTables are the thirty-three user tables of the v6 contract schema, sorted.
var contractTables = []string{
	"agent_archives",
	"agent_lifecycle",
	"agent_resource_bindings",
	"config_snapshots",
	"credential_versions",
	"credentials",
	"execution_policy",
	"instance_secrets",
	"llm_calls",
	"management_operations",
	"mcp_credential_sources",
	"memory_entries",
	"messages",
	"network_domain_policy",
	"network_exceptions",
	"notify_channels",
	"notify_deliveries",
	"resource_connection_tests",
	"resource_identities",
	"resource_pointers",
	"resource_version_credentials",
	"resource_versions",
	"run_credential_refs",
	"run_notify_refs",
	"run_resource_manifests",
	"run_resource_refs",
	"runs",
	"schedule_deliveries",
	"scheduled_tasks",
	"schema_version",
	"sessions",
	"task_executions",
	"tool_invocations",
}

// TestMigrate_NewDatabaseCreatesAllTables：新库建齐 33 表 + 版本行 = target；
// 建表与版本行同一事务（颗粒度文档 §4.2 映射）。
func TestMigrate_NewDatabaseCreatesAllTables(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))

	require.NoError(t, Migrate(context.Background(), db, schemaFS, SupportedSchemaVersion))

	require.Equal(t, contractTables, tableNamesOf(t, db))
	require.Equal(t, SupportedSchemaVersion, mustVersion(t, db))
}

// TestMigrate_Idempotent：二跑无错、版本行不变（契约 schema.sql 幂等）。
func TestMigrate_Idempotent(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	ctx := context.Background()

	require.NoError(t, Migrate(ctx, db, schemaFS, SupportedSchemaVersion))
	require.NoError(t, Migrate(ctx, db, schemaFS, SupportedSchemaVersion))

	require.Equal(t, SupportedSchemaVersion, mustVersion(t, db))
	require.Equal(t, contractTables, tableNamesOf(t, db))
}

// TestMigrate_AppliesIncrementalWithFixtures：合成迁移集覆盖「新库全量 → 旧库
// 逐版前向 1→2→3」——target 显式传参（支持版本写死的实现无法自然覆盖此路径）。
func TestMigrate_AppliesIncrementalWithFixtures(t *testing.T) {
	migrationFS := fstest.MapFS{ // 合成集：schema.sql = v1 全量；0002 / 0003 = 增量
		"schema.sql":                &fstest.MapFile{Data: []byte(syntheticV1)},
		"migrations/0002_add_b.sql": &fstest.MapFile{Data: []byte("CREATE TABLE b (id INTEGER);")},
		"migrations/0003_add_c.sql": &fstest.MapFile{Data: []byte("CREATE TABLE c (id INTEGER);")},
	}
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))

	require.NoError(t, Migrate(context.Background(), db, migrationFS, 1)) // 新库：v1 全量 + 记 1
	require.NoError(t, Migrate(context.Background(), db, migrationFS, 3)) // 旧库：1→2→3 逐版
	require.Equal(t, 3, mustVersion(t, db))                               // 版本记录到 3
	requireTableExists(t, db, "b")                                        // 0002 落地
	requireTableExists(t, db, "c")                                        // 0003 落地

	// 版本行逐版留痕：1（全量）、2（0002）、3（0003）各一行。
	rows, err := db.Query(`SELECT version FROM schema_version ORDER BY version`)
	require.NoError(t, err)
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		versions = append(versions, v)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []int{1, 2, 3}, versions)
}

// TestMigrate_RejectsNewerSchema：库内版本高于目标 → 拒绝（契约迁移纪律 2：
// 不得让旧二进制打开新库）。
func TestMigrate_RejectsNewerSchema(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))

	// 构造"更晚版本的库"：schema_version 记入高于本二进制支持的版本
	_, err := db.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO schema_version (version, applied_at, description) VALUES (?, 0, 'future')`,
		SupportedSchemaVersion+1)
	require.NoError(t, err)

	err = Migrate(context.Background(), db, schemaFS, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaTooNew)
}

// TestMigrate_NewDatabaseFailureRollsBack：首次建库失败必须整体回滚（约束 16）——
// 半途失败不得留下"已有但为空 / 只有版本表"的部分状态；完好 FS 重跑按新库成功。
// 夹具设计：① 失败点**不得**用 DDL 类型名构造——SQLite 类型名宽松，
//
//	`id INTEGER SYNTAX ERROR` 会被当作多词类型名接受、建表照样成功（已实测复现）；
//
// ② 夹具必须先建出"可观察的残留"（版本表 + a），否则"断言版本表不存在"在未回滚
//
//	时也会通过（空虚断言）。
func TestMigrate_NewDatabaseFailureRollsBack(t *testing.T) {
	broken := fstest.MapFS{
		"schema.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL);\n" +
				"CREATE TABLE a (id INTEGER);\n" +
				"INSERT INTO nonexistent_probe (x) VALUES (1);\n")}, // 确定性失败："no such table"
	}
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))

	err := Migrate(context.Background(), db, broken, 1)
	require.Error(t, err)

	// 同事务回滚：版本表与 a **均不存在**（任一残留都说明未回滚）
	require.Zero(t, countTable(t, db, "schema_version"))
	require.Zero(t, countTable(t, db, "a"))

	// 完好 FS 重跑：按新库成功（版本表未残留，仍走新库分支）
	require.NoError(t, Migrate(context.Background(), db, schemaFS, 1))
	require.Equal(t, 1, mustVersion(t, db))
}

// TestMigrate_IncrementalFailureRollsBack：增量失败同样整体回滚——0002 = [建表 +
// 确定性失败语句] → 版本仍为 1 且该表不存在；修好后重试到 2（约束 16）。
func TestMigrate_IncrementalFailureRollsBack(t *testing.T) {
	broken := fstest.MapFS{
		"schema.sql": &fstest.MapFile{Data: []byte(syntheticV1)},
		"migrations/0002_add_b.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE b (id INTEGER);\n" +
				"INSERT INTO nonexistent_probe (x) VALUES (1);\n")},
	}
	fixed := fstest.MapFS{
		"schema.sql":                &fstest.MapFile{Data: []byte(syntheticV1)},
		"migrations/0002_add_b.sql": &fstest.MapFile{Data: []byte("CREATE TABLE b (id INTEGER);")},
	}
	ctx := context.Background()
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))

	require.NoError(t, Migrate(ctx, db, broken, 1)) // 先到 v1

	err := Migrate(ctx, db, broken, 2)
	require.Error(t, err)
	require.Equal(t, 1, mustVersion(t, db), "增量失败后版本必须回到迁移前")
	require.Zero(t, countTable(t, db, "b"), "半截增量表不得残留")

	// 修好后重试到 2
	require.NoError(t, Migrate(ctx, db, fixed, 2))
	require.Equal(t, 2, mustVersion(t, db))
	requireTableExists(t, db, "b")
}

// TestMigrate_EmptyVersionTableRejected：版本表零行 = 损坏态 → 拒绝（不猜测为
// 新库或旧库，fail-closed）。
func TestMigrate_EmptyVersionTableRejected(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))

	_, err := db.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL)`)
	require.NoError(t, err)

	err = Migrate(context.Background(), db, schemaFS, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaCorrupt)
	require.Zero(t, countTable(t, db, "sessions"), "损坏态不得按新库建表")
}

// TestMigrate_MissingMigrationFileRejected：目标版本缺迁移文件 → 拒绝并点名
// 文件；版本不变（契约迁移纪律 3：不跳过版本）。
func TestMigrate_MissingMigrationFileRejected(t *testing.T) {
	// 只有 v1 全量，没有 0002——target=2 必然缺文件。
	onlyV1 := fstest.MapFS{"schema.sql": &fstest.MapFile{Data: []byte(syntheticV1)}}
	ctx := context.Background()
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))

	require.NoError(t, Migrate(ctx, db, onlyV1, 1))

	err := Migrate(ctx, db, onlyV1, 2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "migrations/0002_*.sql", "错误必须点名期望的迁移文件")
	require.Equal(t, 1, mustVersion(t, db), "缺文件时版本不得前进")
}

// TestMigrate_NewDatabaseDoesNotRunMigrations：新库只执行全量脚本——迁移目录里的
// 文件即使损坏也不得被新库路径执行（否则升级文件成了第二份建表源）。
// A new database runs the full script only; migration files are never executed.
func TestMigrate_NewDatabaseDoesNotRunMigrations(t *testing.T) {
	script, err := schemaFS.ReadFile(schemaFileName)
	require.NoError(t, err)
	brokenFS := fstest.MapFS{
		"schema.sql":                 &fstest.MapFile{Data: script},
		"migrations/0002_broken.sql": &fstest.MapFile{Data: []byte("THIS IS NOT SQL")},
	}
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))

	require.NoError(t, Migrate(context.Background(), db, brokenFS, SupportedSchemaVersion))
	require.Equal(t, SupportedSchemaVersion, mustVersion(t, db))
	requireV2Tables(t, db)
}

// TestMigrate_CurrentDatabaseSkipsMigrationFiles：库已是目标版本时无需任何迁移文件
// （只读打开 / 重复启动不得因为迁移目录缺失而失败）。
// A current database needs no migration files at all.
func TestMigrate_CurrentDatabaseSkipsMigrationFiles(t *testing.T) {
	ctx := context.Background()
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	require.NoError(t, Migrate(ctx, db, schemaFS, SupportedSchemaVersion))

	script, err := schemaFS.ReadFile(schemaFileName)
	require.NoError(t, err)
	noMigrations := fstest.MapFS{"schema.sql": &fstest.MapFile{Data: script}}

	require.NoError(t, Migrate(ctx, db, noMigrations, SupportedSchemaVersion))
	require.Equal(t, SupportedSchemaVersion, mustVersion(t, db))
}

// TestMigrate_VersionRowsRecordDescriptions：版本行的 description 指向实际执行的
// 源——新库是全量脚本名，前向迁移是具体迁移文件名（可审计）。
// Version rows name the source that was actually executed.
func TestMigrate_VersionRowsRecordDescriptions(t *testing.T) {
	ctx := context.Background()

	t.Run("new database", func(t *testing.T) {
		db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
		require.NoError(t, Migrate(ctx, db, schemaFS, SupportedSchemaVersion))

		var desc string
		require.NoError(t, db.QueryRow(
			`SELECT description FROM schema_version WHERE version = ?`, SupportedSchemaVersion).Scan(&desc))
		require.Equal(t, schemaFileName, desc)
	})

	t.Run("incremental migration", func(t *testing.T) {
		db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
		fsys := v1ToV2FS(t)
		require.NoError(t, Migrate(ctx, db, fsys, 1))
		require.NoError(t, Migrate(ctx, db, fsys, 2))

		var desc string
		require.NoError(t, db.QueryRow(
			`SELECT description FROM schema_version WHERE version = 2`).Scan(&desc))
		require.Equal(t, "migrations/0002_us11_us12_resources.sql", desc)
	})
}

// TestMigrate_SupportedVersionHasMigrationFile：支持版本与迁移文件名编号对齐——
// 常量改动而忘记追加迁移文件时在此拦下（契约 v6 基线）。
// The supported version must have its own migration file.
func TestMigrate_SupportedVersionHasMigrationFile(t *testing.T) {
	name, err := migrationFile(schemaFS, SupportedSchemaVersion)
	require.NoError(t, err)
	require.Equal(t, "migrations/0006_execution_policy_and_mcp_credentials.sql", name)
}

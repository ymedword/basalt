package storage

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// v2TestResourceUID 是 v2 段用例里的资源 UID 字面量（v3 起 resource_versions 必填）。
// v2TestResourceUID is the resource uid fixture used by the v2 section cases.
const v2TestResourceUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"

// v2Tables 是 v2 增量迁移新增的七张表（契约 schema.sql「v2 新增」段）。
// v2Tables lists the seven tables added by the v2 migration.
var v2Tables = []string{
	"agent_archives",
	"agent_lifecycle",
	"management_operations",
	"resource_pointers",
	"resource_versions",
	"run_resource_manifests",
	"run_resource_refs",
}

// v2Indexes 是 v2 段声明的全部索引。
// v2Indexes lists every index declared by the v2 section.
var v2Indexes = []string{
	"idx_agent_archives_agent",
	"idx_agent_archives_restorable",
	"idx_agent_lifecycle_state",
	"idx_management_operations_created",
	"idx_management_operations_pending",
	"idx_management_operations_target",
	"idx_resource_pointers_applied",
	"idx_resource_versions_hash",
	"idx_resource_versions_lookup",
	"idx_run_resource_manifests_hash",
	"idx_run_resource_refs_unreleased",
	"idx_run_resource_refs_version",
}

// v1ToV2FS 构造「v1 全量 + 真实 0002 迁移」的合成迁移集：旧库前向路径必须用
// 真实迁移文件覆盖，不能只测合成 DDL。
// v1ToV2FS builds a synthetic v1 schema plus the real 0002 migration file.
func v1ToV2FS(t *testing.T) fstest.MapFS {
	t.Helper()
	script, err := fs.ReadFile(schemaFS, "migrations/0002_us11_us12_resources.sql")
	require.NoError(t, err)
	return fstest.MapFS{
		"schema.sql": &fstest.MapFile{Data: []byte(syntheticV1)},
		"migrations/0002_us11_us12_resources.sql": &fstest.MapFile{
			Data: script,
		},
	}
}

// requireV2Tables 断言七张表全部存在。
// requireV2Tables asserts every v2 table exists.
func requireV2Tables(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, name := range v2Tables {
		requireTableExists(t, db, name)
	}
}

// TestSchemaV2_NewDatabaseHasV2Tables：新库由契约全量脚本直接建到 v2，七表齐备。
func TestSchemaV2_NewDatabaseHasV2Tables(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	require.NoError(t, Migrate(context.Background(), db, schemaFS, 2))
	requireV2Tables(t, db)
}

// TestSchemaV2_VersionRecorded：新库的版本行 = 当前支持版本（SupportedSchemaVersion）。
func TestSchemaV2_VersionRecorded(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	require.NoError(t, Migrate(context.Background(), db, schemaFS, SupportedSchemaVersion))
	require.Equal(t, SupportedSchemaVersion, mustVersion(t, db))
}

// TestSchemaV2_MigrateFromV1：旧库 1 → 2 执行真实迁移，七表落地且两版恰好各一行。
func TestSchemaV2_MigrateFromV1(t *testing.T) {
	ctx := context.Background()
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	fsys := v1ToV2FS(t)

	require.NoError(t, Migrate(ctx, db, fsys, 1))
	require.NoError(t, Migrate(ctx, db, fsys, 2))

	require.Equal(t, 2, mustVersion(t, db))
	requireV2Tables(t, db)

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
	require.Equal(t, []int{1, 2}, versions)
}

// TestSchemaV2_MigrationPreservesExistingRows：v1 既有数据在前向迁移后逐字保留。
func TestSchemaV2_MigrationPreservesExistingRows(t *testing.T) {
	ctx := context.Background()
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	fsys := v1ToV2FS(t)

	require.NoError(t, Migrate(ctx, db, fsys, 1))
	_, err := db.Exec(`INSERT INTO a (id) VALUES (42)`)
	require.NoError(t, err)

	require.NoError(t, Migrate(ctx, db, fsys, 2))

	var id int
	require.NoError(t, db.QueryRow(`SELECT id FROM a`).Scan(&id))
	require.Equal(t, 42, id)
}

// TestSchemaV2_MigrationIsIdempotent：已到 v2 再跑迁移是空操作，不重复写版本行。
func TestSchemaV2_MigrationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	fsys := v1ToV2FS(t)

	require.NoError(t, Migrate(ctx, db, fsys, 1))
	require.NoError(t, Migrate(ctx, db, fsys, 2))
	require.NoError(t, Migrate(ctx, db, fsys, 2))

	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM schema_version WHERE version = 2`).Scan(&n))
	require.Equal(t, 1, n)
}

// TestSchemaV2_TooNewBinaryRefused：库内版本高于二进制支持版本 → 拒绝启动。
func TestSchemaV2_TooNewBinaryRefused(t *testing.T) {
	ctx := context.Background()
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	require.NoError(t, Migrate(ctx, db, schemaFS, SupportedSchemaVersion))

	_, err := db.Exec(`INSERT INTO schema_version (version, applied_at, description) VALUES (?, 0, 'future')`,
		SupportedSchemaVersion+1)
	require.NoError(t, err)

	err = Migrate(ctx, db, schemaFS, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaTooNew)
}

// TestSchemaV2_IndexesCreated：v2 段声明的索引全部存在。
func TestSchemaV2_IndexesCreated(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	require.NoError(t, Migrate(context.Background(), db, schemaFS, 2))

	for _, name := range v2Indexes {
		var n int
		require.NoError(t, db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&n))
		require.Equal(t, 1, n, "索引 %s 应当存在", name)
	}
}

// TestSchemaV2_StatusCheckConstraint：管理操作状态只允许四个固定值。
func TestSchemaV2_StatusCheckConstraint(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	require.NoError(t, Migrate(context.Background(), db, schemaFS, 2))

	const insert = `INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES (?, 'agent.disable', 'agent', 'reporter', 'local_trust', 'local_trust', 'cli', ?, 1)`

	_, err := db.Exec(insert, "op-1-aaaaaa", "bogus")
	require.Error(t, err, "非法 status 必须被 CHECK 拒绝")

	_, err = db.Exec(insert, "op-2-bbbbbb", "pending")
	require.NoError(t, err)
}

// TestSchemaV2_ResourceKindCheckConstraint：资源类型与版本形态必须成对。
func TestSchemaV2_ResourceKindCheckConstraint(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	require.NoError(t, Migrate(context.Background(), db, schemaFS, 2))

	_, err := db.Exec(`INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES ('op-3-cccccc', 'system.baseline_v2', 'system', 'system', 'migration', 'migration', 'migration', 'pending', 1)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO resource_versions
		(resource_kind, resource_uid, resource_id, version_no, content_hash, created_at, operation_id)
		VALUES ('skill', ?, 'summary', 1, 'abc', 1, 'op-3-cccccc')`, v2TestResourceUID)
	require.Error(t, err, "skill 版本必须带 manifest_json 与 storage_path")

	_, err = db.Exec(`INSERT INTO resource_versions
		(resource_kind, resource_uid, resource_id, version_no, content_hash, manifest_json, storage_path, created_at, operation_id)
		VALUES ('skill', ?, 'summary', 1, 'abc', '[]', '.basalt/skills/summary', 1, 'op-3-cccccc')`, v2TestResourceUID)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO resource_versions
		(resource_kind, resource_uid, resource_id, version_no, content_hash, definition_yaml, created_at, operation_id)
		VALUES ('bogus', ?, 'x', 1, 'abc', 'x', 1, 'op-3-cccccc')`, v2TestResourceUID)
	require.Error(t, err, "非法 resource_kind 必须被 CHECK 拒绝")
}

// TestSchemaV2_RefPrimaryKey：同一运行的同一资源只能有一条引用。
func TestSchemaV2_RefPrimaryKey(t *testing.T) {
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	require.NoError(t, Migrate(context.Background(), db, schemaFS, 2))

	const insert = `INSERT INTO run_resource_refs
		(run_id, resource_kind, resource_id, version_id, content_hash)
		VALUES ('run-1', 'skill', 'summary', 1, 'abc')`

	_, err := db.Exec(insert)
	require.NoError(t, err)
	_, err = db.Exec(insert)
	require.Error(t, err, "复合主键必须拒绝重复引用")
}

package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// v3Tables 是 v3 增量迁移新增的八张表（契约 schema.sql「v3 新增」段）。
// v3Tables lists the eight tables added by the v3 migration.
var v3Tables = []string{
	"agent_resource_bindings",
	"credential_versions",
	"credentials",
	"notify_deliveries",
	"resource_identities",
	"resource_version_credentials",
	"run_credential_refs",
	"run_notify_refs",
}

// v1TablesTouchedByV3 是 v3 迁移会读到 / 重建的 v1 期表（真实 v1 形状）：
// 合成 v1 脚本只带一张样例表，v2 → v3 的夹具必须补齐这些表才能验证重建本身。
// v1TablesTouchedByV3 recreates the v1-era tables the v3 migration touches.
const v1TablesTouchedByV3 = `
CREATE TABLE runs (
    run_id TEXT PRIMARY KEY, session_id TEXT, agent TEXT NOT NULL, channel TEXT NOT NULL,
    user_id TEXT NOT NULL, trigger TEXT NOT NULL, status TEXT NOT NULL, reason TEXT,
    iterations INTEGER NOT NULL DEFAULT 0, tool_calls INTEGER NOT NULL DEFAULT 0,
    config_hash TEXT, started_at INTEGER NOT NULL, finished_at INTEGER
);
CREATE TABLE tool_invocations (
    id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, session_id TEXT,
    tool_call_id TEXT, tool_name TEXT NOT NULL, input_json TEXT, result_json TEXT,
    status TEXT NOT NULL, error_message TEXT, error_kind TEXT, duration_ms INTEGER,
    started_at INTEGER NOT NULL, finished_at INTEGER
);
CREATE TABLE notify_channels (
    name TEXT PRIMARY KEY, type TEXT NOT NULL, url TEXT NOT NULL, description TEXT,
    created_at INTEGER NOT NULL
);`

// v2ToV3FS 构造「v1 全量（含 v3 触及的表）+ 真实 0002 + 真实 0003」的合成迁移集：
// v2 → v3 前向路径必须用真实迁移文件覆盖（合成 DDL 证明不了重建脚本本身）。
// v2ToV3FS builds a synthetic v1 schema plus the real 0002 and 0003 migrations.
func v2ToV3FS(t *testing.T) fstest.MapFS {
	t.Helper()
	v2Script, err := fs.ReadFile(schemaFS, "migrations/0002_us11_us12_resources.sql")
	require.NoError(t, err)
	v3Script, err := fs.ReadFile(schemaFS, "migrations/0003_credentials_scope.sql")
	require.NoError(t, err)
	return fstest.MapFS{
		"schema.sql": &fstest.MapFile{Data: []byte(syntheticV1 + v1TablesTouchedByV3)},
		"migrations/0002_us11_us12_resources.sql": &fstest.MapFile{Data: v2Script},
		"migrations/0003_credentials_scope.sql":   &fstest.MapFile{Data: v3Script},
	}
}

// seedV2Database 建一个 v1 → v2 的旧库并写入迁移前的真实形状数据：资源版本 /
// 活动与已删除指针 / 通知渠道 / 生命周期与归档索引 / Run 资源引用，外加 v3 编排
// 预写的 system.baseline_v3 操作行（0003 的身份与渠道行引用它的 operation_id）。
// seedV2Database builds a v2 database seeded with pre-migration shaped rows.
func seedV2Database(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	fsys := v2ToV3FS(t)

	require.NoError(t, Migrate(ctx, db, fsys, 1))
	require.NoError(t, Migrate(ctx, db, fsys, 2))

	const insertOp = `INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES (?, ?, 'system', 'system', 'migration', 'migration', 'migration', ?, 1)`
	_, err := db.Exec(insertOp, "op-1-aaaaaa", "system.baseline_v2", "success")
	require.NoError(t, err)
	_, err = db.Exec(insertOp, "op-2-bbbbbb", "model_connection.publish", "success")
	require.NoError(t, err)
	_, err = db.Exec(insertOp, "op-3-cccccc", "system.baseline_v3", "pending")
	require.NoError(t, err)

	const insertVersion = `INSERT INTO resource_versions
		(id, resource_kind, resource_id, version_no, content_hash, definition_yaml, manifest_json, storage_path, created_at, operation_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'op-2-bbbbbb')`
	_, err = db.Exec(insertVersion, 1, "model_connection", "openai", 1, "hash-v1", "name: openai", nil, nil, 100)
	require.NoError(t, err)
	_, err = db.Exec(insertVersion, 2, "model_connection", "openai", 2, "hash-v2", "name: openai", nil, nil, 200)
	require.NoError(t, err)
	_, err = db.Exec(insertVersion, 3, "skill", "summary", 1, "hash-skill", nil, "[]", ".basalt/skills/summary", 300)
	require.NoError(t, err)

	const insertPointer = `INSERT INTO resource_pointers
		(resource_kind, resource_id, published_version_id, applied_version_id, deleted_at, updated_at, operation_id)
		VALUES (?, ?, ?, ?, ?, ?, 'op-2-bbbbbb')`
	_, err = db.Exec(insertPointer, "model_connection", "openai", 2, 2, nil, 250)
	require.NoError(t, err)
	_, err = db.Exec(insertPointer, "skill", "summary", 3, 3, nil, 350)
	require.NoError(t, err)
	_, err = db.Exec(insertPointer, "mcp_server", "legacy", 3, 3, 400, 400)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO notify_channels (name, type, url, description, created_at)
		VALUES ('alerts', 'webhook', '${BASALT_ALERT_URL}', '值班渠', 500)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO agent_lifecycle
		(agent_name, state, generation, state_file_hash, valid, updated_at, operation_id)
		VALUES ('reporter', 'active', 3, 'state-hash', 1, 600, 'op-1-aaaaaa')`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO agent_archives
		(archive_id, agent_name, archived_at, content_hash, manifest_json, operation_id)
		VALUES ('old-20260101-000000', 'old', 700, 'archive-hash', '[]', 'op-1-aaaaaa')`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO runs (run_id, agent, channel, user_id, trigger, status, started_at)
		VALUES ('run-1', 'reporter', 'web', 'admin', 'user', 'completed', 800)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO run_resource_refs
		(run_id, resource_kind, resource_id, version_id, content_hash, released_at)
		VALUES ('run-1', 'model_connection', 'openai', 1, 'hash-v1', 900)`)
	require.NoError(t, err)

	return db
}

// TestMigrationV3_ForwardFromV2：v2 旧库前向到 v3 —— 版本 id 保留、资源 UID 与
// 墓碑写入身份表、通知渠道主键换 UID、Run 引用保持 NULL / 记名称、Agent 列就位。
func TestMigrationV3_ForwardFromV2(t *testing.T) {
	ctx := context.Background()
	db := seedV2Database(t)

	require.NoError(t, Migrate(ctx, db, v2ToV3FS(t), 3))

	require.Equal(t, 3, mustVersion(t, db))
	for _, name := range v3Tables {
		requireTableExists(t, db, name)
	}

	// 资源 UID：同名历史版本共享同一 UID；版本 id 全局稳定（1/2/3 原样）。
	var openaiUID, skillUID string
	require.NoError(t, db.QueryRow(`SELECT resource_uid FROM resource_pointers
		WHERE resource_kind = 'model_connection' AND resource_id = 'openai'`).Scan(&openaiUID))
	require.NoError(t, db.QueryRow(`SELECT resource_uid FROM resource_pointers
		WHERE resource_kind = 'skill' AND resource_id = 'summary'`).Scan(&skillUID))
	_, err := core.ParseResourceUID(openaiUID)
	require.NoError(t, err, "迁移分配的 UID 必须能通过 core.ParseResourceUID")
	_, err = core.ParseResourceUID(skillUID)
	require.NoError(t, err)
	require.NotEqual(t, openaiUID, skillUID, "不同资源实例不得共享 UID")

	rows, err := db.Query(`SELECT id, resource_uid FROM resource_versions ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var id int
		var uid string
		require.NoError(t, rows.Scan(&id, &uid))
		got[id] = uid
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[int]string{1: openaiUID, 2: openaiUID, 3: skillUID}, got,
		"版本 id 必须保留、UID 按同名指针回填")

	// 身份表：活动 / 已删除指针各一行，通知渠道一行。
	var identities int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM resource_identities`).Scan(&identities))
	require.Equal(t, 4, identities)
	var deletedState string
	require.NoError(t, db.QueryRow(`SELECT state FROM resource_identities
		WHERE resource_kind = 'mcp_server' AND name = 'legacy'`).Scan(&deletedState))
	require.Equal(t, "deleted", deletedState)
	var notifyUID string
	require.NoError(t, db.QueryRow(`SELECT resource_uid FROM notify_channels WHERE name = 'alerts'`).Scan(&notifyUID))
	_, err = core.ParseResourceUID(notifyUID)
	require.NoError(t, err)
	require.NotEqual(t, openaiUID, notifyUID, "通知渠道与其它的 UID 命名空间不得相撞")

	// 通知渠道：地址引用保留、模式 env、范围 shared、历史列不变。
	var url, mode, scope string
	var revision int
	require.NoError(t, db.QueryRow(`SELECT url, credential_mode, scope, revision FROM notify_channels
		WHERE resource_uid = ?`, notifyUID).Scan(&url, &mode, &scope, &revision))
	require.Equal(t, "${BASALT_ALERT_URL}", url)
	require.Equal(t, "env", mode)
	require.Equal(t, "shared", scope)
	require.Equal(t, 1, revision)

	// Run 引用：v2 没有任何可证明归属的证据 → UID 保持 NULL（legacy_unrecorded），
	// 只记名称快照。「同名指针当前活动」不能证明旧 Run 属于该实例（见反例用例）。
	var refUID, refName sql.NullString
	require.NoError(t, db.QueryRow(`SELECT resource_uid, resource_name FROM run_resource_refs
		WHERE run_id = 'run-1'`).Scan(&refUID, &refName))
	require.False(t, refUID.Valid, "无法证明历史归属的引用必须保持 NULL")
	require.Equal(t, "openai", refName.String)

	// Agent 身份列：迁移只加列，不猜 UID（回填由启动编排按状态文件执行）。
	var agentUID sql.NullString
	var lifecycleSource sql.NullString
	require.NoError(t, db.QueryRow(`SELECT agent_uid, uid_source FROM agent_lifecycle
		WHERE agent_name = 'reporter'`).Scan(&agentUID, &lifecycleSource))
	require.False(t, agentUID.Valid)
	require.False(t, lifecycleSource.Valid, "未对账前 uid_source 必须为 NULL")
	var archiveUID sql.NullString
	var archiveSource string
	require.NoError(t, db.QueryRow(`SELECT agent_uid, uid_source FROM agent_archives
		WHERE archive_id = 'old-20260101-000000'`).Scan(&archiveUID, &archiveSource))
	require.False(t, archiveUID.Valid)
	require.Equal(t, "legacy_unrecorded", archiveSource)

	// 唯一键换到 UID 维度：同名新 UID 可以从 1 重新计版本号。
	_, err = db.Exec(`INSERT INTO resource_versions
		(resource_kind, resource_uid, resource_id, version_no, content_hash, definition_yaml, created_at, operation_id)
		VALUES ('model_connection', 'rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B', 'openai', 1, 'hash-new', 'name: openai', 1000, 'op-2-bbbbbb')`)
	require.NoError(t, err, "同名重建（新 UID）版本号必须能从 1 重新计")

	// 活动名称唯一索引就位。
	_, err = db.Exec(`INSERT INTO notify_channels
		(resource_uid, name, type, url, scope, created_at, updated_at, operation_id)
		VALUES ('rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C', 'alerts', 'webhook', '', 'shared', 1, 1, 'op-2-bbbbbb')`)
	require.Error(t, err, "活动名称唯一索引必须拒绝同名新行")
}

// TestMigrationV3_SameNameRebuildDoesNotClaimLegacyRun：反例（复核 U19）——旧资源执行过
// Run → 删除 → 同名重建 → 新指针当前活动时，「当前活动」不能证明旧 Run 属于新实例；
// 迁移必须把旧引用保持 NULL（legacy_unrecorded），不按名称猜测归属。
// 逆改点：恢复「活动指针即回填」→ UID.Valid 断言红；迁移漏记名称 → refName 断言红。
// TestMigrationV3_SameNameRebuildDoesNotClaimLegacyRun pins the delete → rebuild
// counterexample: a live pointer alone never proves which instance a legacy run used.
func TestMigrationV3_SameNameRebuildDoesNotClaimLegacyRun(t *testing.T) {
	ctx := context.Background()
	db := seedV2Database(t)

	// 旧实例：版本 1 / 2（种子数据），run-1 用的是版本 1。
	// 删除（1000）在 run-1（900）之后；同名重建的版本 3（1100）在删除之后，指针当前活动。
	_, err := db.Exec(`INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES ('op-del-1', 'model_connection.delete', 'model_connection', 'openai',
		        'admin', 'bearer', 'api', 'success', 1000)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES ('op-re-1', 'model_connection.publish', 'model_connection', 'openai',
		        'admin', 'bearer', 'api', 'success', 1100)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO resource_versions
		(id, resource_kind, resource_id, version_no, content_hash, definition_yaml, created_at, operation_id)
		VALUES (4, 'model_connection', 'openai', 3, 'hash-rebuilt', 'name: openai', 1100, 'op-re-1')`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE resource_pointers
		SET published_version_id = 4, applied_version_id = 4, updated_at = 1100, operation_id = 'op-re-1'
		WHERE resource_kind = 'model_connection' AND resource_id = 'openai'`)
	require.NoError(t, err)

	require.NoError(t, Migrate(ctx, db, v2ToV3FS(t), 3))

	var refUID, refName sql.NullString
	require.NoError(t, db.QueryRow(`SELECT resource_uid, resource_name FROM run_resource_refs
		WHERE run_id = 'run-1'`).Scan(&refUID, &refName))
	require.False(t, refUID.Valid, "删除 → 同名重建后，当前活动指针不能证明旧 Run 的历史归属")
	require.Equal(t, "openai", refName.String, "名称快照仍要保留，供按名称检索历史引用")

	// 兼容规则不等于历史连续：同名历史版本仍归并到当前指针 UID（新表 NOT NULL 要求），
	// 但这只是迁移的兼容处置，不用于回填 run_resource_refs。
	var pointerUID, versionUID string
	require.NoError(t, db.QueryRow(`SELECT resource_uid FROM resource_pointers
		WHERE resource_kind = 'model_connection' AND resource_id = 'openai'`).Scan(&pointerUID))
	require.NoError(t, db.QueryRow(`SELECT resource_uid FROM resource_versions WHERE id = 1`).Scan(&versionUID))
	require.Equal(t, pointerUID, versionUID, "兼容规则：同名历史版本归入当前指针 UID")
}

// TestMigrationV3_InterruptedRefusesStartup：迁移中途失败 → 整事务回滚、版本停在
// 2、不留半张重建表（启动方据版本号中止并提示按备份恢复）。
func TestMigrationV3_InterruptedRefusesStartup(t *testing.T) {
	ctx := context.Background()
	db := seedV2Database(t)

	// 造一条没有同名指针行的版本：重建时 resource_uid 为 NULL，新表 NOT NULL 约束
	// 使其在复制阶段失败——正是"无法证明归属就中止"的路径。
	_, err := db.Exec(`INSERT INTO resource_versions
		(id, resource_kind, resource_id, version_no, content_hash, definition_yaml, created_at, operation_id)
		VALUES (9, 'model_connection', 'orphan', 1, 'hash-orphan', 'name: orphan', 1100, 'op-2-bbbbbb')`)
	require.NoError(t, err)

	err = Migrate(ctx, db, v2ToV3FS(t), 3)
	require.Error(t, err, "无法证明归属的版本必须让迁移失败，而不是静默丢弃")

	require.Equal(t, 2, mustVersion(t, db), "失败后版本号不得前进")
	require.Equal(t, 0, countTable(t, db, "resource_versions_v3"))
	require.Equal(t, 0, countTable(t, db, "notify_channels_v3"))

	// 旧表结构原样：v2 的版本表没有 resource_uid 列。
	var columns int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM pragma_table_info('resource_versions') WHERE name = 'resource_uid'`).Scan(&columns))
	require.Equal(t, 0, columns)

	// 外键状态恢复：失败的迁移不得把 OFF 泄漏给连接的下一个使用者。
	var foreignKeys int
	require.NoError(t, db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys))
	require.Equal(t, 0, foreignKeys, "裸测试库的初始外键状态是 OFF，迁移必须还原而不是强设 ON")
}

// TestMigrationV3_PublishedFilesUnchanged：已发布的 v2 迁移文件逐字节冻结——
// 只追加新版本迁移，不改写历史（迁移纪律 3）。
func TestMigrationV3_PublishedFilesUnchanged(t *testing.T) {
	script, err := fs.ReadFile(schemaFS, "migrations/0002_us11_us12_resources.sql")
	require.NoError(t, err)
	sum := sha256.Sum256(script)
	require.Equal(t, "f305465f97dee825a8c986f2333b5e66606b063b6e308b9714c40f94449efdae",
		hex.EncodeToString(sum[:]), "已发布的 0002 迁移被改写")
}

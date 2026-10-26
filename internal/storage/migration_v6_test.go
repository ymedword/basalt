package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
)

// v6Tables 是 0006 迁移新增的四张表（契约 schema.sql「v6 新增」段）。
// v6Tables lists the four tables added by the v6 migration.
var v6Tables = []string{
	"execution_policy",
	"instance_secrets",
	"mcp_credential_sources",
	"network_exceptions",
}

// syntheticV5 是 0006 迁移会读到 / 重建的 v5 期表（真实 v5 形状）：
// 合成 v5 脚本只带迁移实际触及的表——schema_version（版本判定）、
// management_operations（credentials 的外键目标）、credentials 与
// credential_versions（重建与被重建的一方，证明重建后外键仍然指向新表）。
// syntheticV5 recreates the v5-era tables the v6 migration touches.
const syntheticV5 = `
CREATE TABLE schema_version (
    version     INTEGER PRIMARY KEY,
    applied_at  INTEGER NOT NULL,
    description TEXT    NOT NULL
);
CREATE TABLE management_operations (
    operation_id  TEXT PRIMARY KEY,
    action        TEXT NOT NULL,
    target_kind   TEXT NOT NULL,
    target_id     TEXT NOT NULL,
    actor_subject TEXT NOT NULL,
    auth_method   TEXT NOT NULL,
    source        TEXT NOT NULL,
    status        TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);
CREATE TABLE credentials (
    credential_id    TEXT PRIMARY KEY,
    owner_kind       TEXT    NOT NULL,
    owner_uid        TEXT    NOT NULL,
    owner_name       TEXT    NOT NULL,
    state            TEXT    NOT NULL,
    current_version  INTEGER NOT NULL,
    revocation_epoch INTEGER NOT NULL DEFAULT 0,
    key_id           TEXT    NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    operation_id     TEXT    NOT NULL REFERENCES management_operations(operation_id),
    UNIQUE (owner_kind, owner_uid),
    CHECK (owner_kind IN ('model_connection', 'notify_channel')),
    CHECK (state IN ('active', 'revoked', 'retired')),
    CHECK (current_version >= 1)
);
CREATE TABLE credential_versions (
    credential_id  TEXT    NOT NULL REFERENCES credentials(credential_id),
    version_no     INTEGER NOT NULL,
    state          TEXT    NOT NULL,
    key_id         TEXT    NOT NULL,
    target_binding TEXT    NOT NULL,
    last4          TEXT    NOT NULL DEFAULT '',
    nonce          BLOB    NOT NULL,
    ciphertext     BLOB,
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
CREATE INDEX idx_credential_versions_cred ON credential_versions(credential_id, version_no);
`

// v5ToV6FS 构造「合成 v5 全量 + 真实 0006」的迁移集：v5 → v6 前向路径必须用
// 真实迁移文件覆盖（合成 DDL 证明不了重建脚本本身）。
// v5ToV6FS builds a synthetic v5 schema plus the real 0006 migration.
func v5ToV6FS(t *testing.T) fstest.MapFS {
	t.Helper()
	script, err := fs.ReadFile(schemaFS, "migrations/0006_execution_policy_and_mcp_credentials.sql")
	require.NoError(t, err)
	return fstest.MapFS{
		"schema.sql": &fstest.MapFile{Data: []byte(syntheticV5)},
		"migrations/0006_execution_policy_and_mcp_credentials.sql": &fstest.MapFile{Data: script},
	}
}

// seedV5Database 建一个 v5 旧库并写入迁移前的真实形状数据：一条迁移操作行、
// 两个凭据身份（模型连接 / 通知渠道）与三个不可变凭据版本。
// seedV5Database builds a v5 database seeded with pre-migration shaped rows.
func seedV5Database(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db := openBare(t, filepath.Join(t.TempDir(), "basalt.db"))
	fsys := v5ToV6FS(t)

	require.NoError(t, Migrate(ctx, db, fsys, 5))
	require.Equal(t, 5, mustVersion(t, db))

	_, err := db.Exec(`INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES ('op-1-aaaaaa', 'model_connection.publish', 'model_connection', 'openai', 'admin', 'token', 'api', 'success', 1)`)
	require.NoError(t, err)

	const insertCred = `INSERT INTO credentials
		(credential_id, owner_kind, owner_uid, owner_name, state, current_version,
		 revocation_epoch, key_id, created_at, updated_at, operation_id)
		VALUES (?, ?, ?, ?, 'active', 2, 0, 'mk-1', 100, 200, 'op-1-aaaaaa')`
	_, err = db.Exec(insertCred, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", "model_connection", "rs_00000000000000000000000001", "openai")
	require.NoError(t, err)
	_, err = db.Exec(insertCred, "cred_bbbbbbbbbbbbbbbbbbbbbbbbbb", "notify_channel", "rs_00000000000000000000000002", "alerts")
	require.NoError(t, err)

	const insertVer = `INSERT INTO credential_versions
		(credential_id, version_no, state, key_id, target_binding, last4, nonce, ciphertext,
		 created_at, updated_at, operation_id)
		VALUES (?, ?, ?, 'mk-1', 'binding-hash', '1234', zeroblob(12), ?, 100, 200, 'op-1-aaaaaa')`
	_, err = db.Exec(insertVer, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", 1, "superseded", []byte{0x01})
	require.NoError(t, err)
	_, err = db.Exec(insertVer, "cred_aaaaaaaaaaaaaaaaaaaaaaaaaa", 2, "active", []byte{0x02})
	require.NoError(t, err)
	_, err = db.Exec(insertVer, "cred_bbbbbbbbbbbbbbbbbbbbbbbbbb", 1, "active", []byte{0x03})
	require.NoError(t, err)

	return db
}

// credentialsFingerprint 把两张凭据表的全部行规整序列化后哈希，用来证明迁移
// 只换结构、不动内容。只取 v5 / v6 共有的列：`binding_key` 是迁移新增列，
// 升级前后的可比性由「旧行一律空串」单独断言。
// credentialsFingerprint hashes every row over the columns shared by v5 and v6.
func credentialsFingerprint(t *testing.T, db *sql.DB) string {
	t.Helper()
	var b strings.Builder

	rows, err := db.Query(`SELECT credential_id, owner_kind, owner_uid, owner_name,
		state, current_version, revocation_epoch, key_id, created_at, updated_at, operation_id
		FROM credentials ORDER BY credential_id`)
	require.NoError(t, err)
	for rows.Next() {
		var (
			id, kind, uid, name, state, keyID, opID    string
			curVersion, revEpoch, createdAt, updatedAt int64
		)
		require.NoError(t, rows.Scan(&id, &kind, &uid, &name, &state,
			&curVersion, &revEpoch, &keyID, &createdAt, &updatedAt, &opID))
		fmt.Fprintf(&b, "c|%s|%s|%s|%s|%s|%d|%d|%s|%d|%d|%s\n",
			id, kind, uid, name, state, curVersion, revEpoch, keyID, createdAt, updatedAt, opID)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())

	versions, err := db.Query(`SELECT credential_id, version_no, state, key_id, target_binding,
		last4, length(nonce), hex(ciphertext), created_at, updated_at, operation_id
		FROM credential_versions ORDER BY credential_id, version_no`)
	require.NoError(t, err)
	for versions.Next() {
		var (
			id, state, keyID, binding, last4, cipherHex, opID string
			versionNo, nonceLen, createdAt, updatedAt         int64
		)
		require.NoError(t, versions.Scan(&id, &versionNo, &state, &keyID, &binding,
			&last4, &nonceLen, &cipherHex, &createdAt, &updatedAt, &opID))
		fmt.Fprintf(&b, "v|%s|%d|%s|%s|%s|%s|%d|%s|%d|%d|%s\n",
			id, versionNo, state, keyID, binding, last4, nonceLen, cipherHex, createdAt, updatedAt, opID)
	}
	require.NoError(t, versions.Err())
	require.NoError(t, versions.Close())

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// TestMigrate_V5ToV6：v5 旧库前向到 v6 —— 版本推进、四张新表建齐、credentials
// 重建出 binding_key 与三元组键、owner_kind 新 CHECK 生效、execution_policy
// 一律写 strict（不看白名单内容）。
func TestMigrate_V5ToV6(t *testing.T) {
	ctx := context.Background()
	db := seedV5Database(t)

	require.NoError(t, Migrate(ctx, db, v5ToV6FS(t), 6))

	require.Equal(t, 6, mustVersion(t, db))
	for _, name := range v6Tables {
		requireTableExists(t, db, name)
	}

	// binding_key 列存在且旧行一律空串。
	var bindingKeys []string
	rows, err := db.Query(`SELECT binding_key FROM credentials ORDER BY credential_id`)
	require.NoError(t, err)
	for rows.Next() {
		var binding string
		require.NoError(t, rows.Scan(&binding))
		bindingKeys = append(bindingKeys, binding)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.Equal(t, []string{"", ""}, bindingKeys)

	// owner_kind 新 CHECK：mcp_server 带占位符名可写入；缺名、错配、未知 kind 一律拒。
	const insertCred = `INSERT INTO credentials
		(credential_id, owner_kind, owner_uid, binding_key, owner_name, state, current_version,
		 revocation_epoch, key_id, created_at, updated_at, operation_id)
		VALUES (?, ?, ?, ?, ?, 'active', 1, 0, 'mk-1', 1, 1, 'op-1-aaaaaa')`
	_, err = db.Exec(insertCred, "cred_cccccccccccccccccccccccccc", "mcp_server",
		"rs_00000000000000000000000003", "GITHUB_PERSONAL_ACCESS_TOKEN", "github")
	require.NoError(t, err, "mcp_server + 占位符名必须可写入")
	_, err = db.Exec(insertCred, "cred_dddddddddddddddddddddddddd", "mcp_server",
		"rs_00000000000000000000000003", "", "github")
	require.Error(t, err, "mcp_server 缺占位符名必须被 CHECK 拒绝")
	_, err = db.Exec(insertCred, "cred_eeeeeeeeeeeeeeeeeeeeeeeeee", "model_connection",
		"rs_00000000000000000000000004", "EXTRA_KEY", "openai")
	require.Error(t, err, "非 mcp_server 带占位符名必须被 CHECK 拒绝")
	_, err = db.Exec(insertCred, "cred_ffffffffffffffffffffffffff", "unknown_kind",
		"rs_00000000000000000000000005", "", "x")
	require.Error(t, err, "未声明的 owner_kind 必须被 CHECK 拒绝")

	// 同一个 MCP server 的多个占位符各自独立（三元组唯一键）。
	_, err = db.Exec(insertCred, "cred_gggggggggggggggggggggggggg", "mcp_server",
		"rs_00000000000000000000000003", "BRAVE_API_KEY", "github")
	require.NoError(t, err, "同一 server 的第二个占位符必须可写入")
	_, err = db.Exec(insertCred, "cred_hhhhhhhhhhhhhhhhhhhhhhhhhh", "mcp_server",
		"rs_00000000000000000000000003", "GITHUB_PERSONAL_ACCESS_TOKEN", "github")
	require.Error(t, err, "同一 (kind, uid, binding_key) 必须唯一")

	// execution_policy：升级一律 strict，revision 从 1 起，种子记 system.migrate_v6。
	var (
		scope, profile, opID string
		revision             int64
	)
	require.NoError(t, db.QueryRow(
		`SELECT policy_scope, profile, revision, operation_id FROM execution_policy`).
		Scan(&scope, &profile, &revision, &opID))
	require.Equal(t, "global", scope)
	require.Equal(t, "strict", profile, "v5 → v6 一律 strict，不看白名单内容")
	require.Equal(t, int64(1), revision)
	require.Equal(t, "system.migrate_v6", opID)

	var policyRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM execution_policy`).Scan(&policyRows))
	require.Equal(t, 1, policyRows, "单行表只允许一行")
}

// TestMigrate_V6Idempotent：同一目标版本再跑一次是 no-op —— 版本记录不追加、
// execution_policy 不被重复插入、凭据内容不变（增量路径，与新库路径的
// TestMigrate_Idempotent 互补）。
func TestMigrate_V6Idempotent(t *testing.T) {
	ctx := context.Background()
	db := seedV5Database(t)
	fsys := v5ToV6FS(t)

	require.NoError(t, Migrate(ctx, db, fsys, 6))
	before := credentialsFingerprint(t, db)

	var versionRowsBefore int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&versionRowsBefore))

	require.NoError(t, Migrate(ctx, db, fsys, 6), "第二次迁移必须是 no-op")

	var versionRowsAfter, policyRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM schema_version`).Scan(&versionRowsAfter))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM execution_policy`).Scan(&policyRows))
	require.Equal(t, versionRowsBefore, versionRowsAfter, "no-op 不得追加版本记录")
	require.Equal(t, 1, policyRows, "no-op 不得重复插入 execution_policy")
	require.Equal(t, before, credentialsFingerprint(t, db), "no-op 不得改动凭据内容")
}

// TestMigrate_KeepsRows：重建只换结构 —— 两张凭据表的行内容逐字段不变、
// 外键校验通过、重建后的 credentials 仍是 credential_versions 的引用目标。
func TestMigrate_KeepsRows(t *testing.T) {
	ctx := context.Background()
	db := seedV5Database(t)
	before := credentialsFingerprint(t, db)

	require.NoError(t, Migrate(ctx, db, v5ToV6FS(t), 6))

	require.Equal(t, before, credentialsFingerprint(t, db),
		"重建后凭据身份与版本必须逐字段不变")

	var nonEmptyBinding int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM credentials WHERE binding_key <> ''`).Scan(&nonEmptyBinding))
	require.Zero(t, nonEmptyBinding, "v5 存量行升级后 binding_key 一律空串")

	var credCount, versionCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM credentials`).Scan(&credCount))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM credential_versions`).Scan(&versionCount))
	require.Equal(t, 2, credCount)
	require.Equal(t, 3, versionCount)

	// 外键完整性：迁移器提交前已跑过 foreign_key_check，这里再独立断言一次。
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	require.NoError(t, err)
	require.False(t, rows.Next(), "迁移后不得残留外键违例")
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())

	// 重建后的 credentials 仍是引用目标：指向不存在身份的版本行必须被拒。
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO credential_versions
		(credential_id, version_no, state, key_id, target_binding, last4, nonce, ciphertext,
		 created_at, updated_at, operation_id)
		VALUES ('cred_missing', 1, 'active', 'mk-1', 'b', '', zeroblob(12), x'01', 1, 1, 'op-1-aaaaaa')`)
	require.Error(t, err, "重建后外键必须仍然生效")
}

package storage

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// v6ComparedTables 是两条建库路径必须产出**同一结构**的表：0006 重建的 `credentials`
// 与四张新表。其余表只由 schema.sql 建（升级路径的合成 v5 不含它们），不在等价范围内。
// v6ComparedTables lists the tables both build paths must produce identically.
var v6ComparedTables = []string{
	"credentials",
	"execution_policy",
	"instance_secrets",
	"mcp_credential_sources",
	"network_exceptions",
}

// TestContracts14D_SchemaMatchesMigration 钉住「契约 schema.sql 的全量 DDL」与
// 「v5 → v6 迁移脚本」在 v6 表面上结构等价：新库与升级库的列（名 / 类型 / 非空 /
// 默认 / 主键位）、外键与索引形状必须逐表相同。任何一侧改了 DDL 而另一侧没跟，都会红。
// 逆改点：迁移里漏一列 / 改 CHECK 依赖的列序 / 少建索引 → 指纹不等；
// schema.sql 种了 execution_policy 或迁移没种 strict → 种子断言红。
// The contract's full DDL and the v5→v6 migration must converge on the same v6 shape.
func TestContracts14D_SchemaMatchesMigration(t *testing.T) {
	ctx := context.Background()

	fresh := openBare(t, filepath.Join(t.TempDir(), "fresh.db"))
	require.NoError(t, Migrate(ctx, fresh, schemaFS, SupportedSchemaVersion))
	require.Equal(t, SupportedSchemaVersion, mustVersion(t, fresh))

	upgraded := seedV5Database(t)
	require.NoError(t, Migrate(ctx, upgraded, v5ToV6FS(t), SupportedSchemaVersion))
	require.Equal(t, SupportedSchemaVersion, mustVersion(t, upgraded))

	for _, table := range v6ComparedTables {
		requireTableExists(t, fresh, table)
		requireTableExists(t, upgraded, table)
		freshShape := tableFingerprint(t, fresh, table)
		require.Equalf(t, freshShape, tableFingerprint(t, upgraded, table),
			"%s 的结构必须由 schema.sql 与 0006 迁移产出同一结果", table)
		// 空指纹会让"两侧相等"变成空转：至少要读到列，才谈得上等价。
		require.Containsf(t, freshShape, "\ncol|", "%s 的指纹必须含列定义", table)
	}

	// 唯一键形状单独钉住：`(owner_kind, owner_uid, binding_key)` 是同一个 server
	// 多个占位符各自独立的前提，指纹相等但两边都丢了唯一约束同样是回归。
	require.Contains(t, tableFingerprint(t, fresh, "credentials"),
		"idx|1|u|0|owner_kind,owner_uid,binding_key",
		"credentials 的唯一约束必须落成 origin=u 的唯一索引")

	// 档位种子是两条路径**唯一**有意不同的数据：迁移写 strict（存量不放宽），
	// 全量 DDL 不种（新库由实例初始化写 standard）。反过来做即静默扩大授权。
	require.Zero(t, countRows(t, fresh, "execution_policy"),
		"schema.sql 不得预置 execution_policy：新库档位由实例初始化写 standard")
	var profile, operationID string
	require.NoError(t, upgraded.QueryRow(
		`SELECT profile, operation_id FROM execution_policy WHERE policy_scope = 'global'`).Scan(&profile, &operationID))
	require.Equal(t, "strict", profile, "升级一律 strict，不看白名单内容")
	require.Equal(t, "system.migrate_v6", operationID)

	// 四张新表在两条路径上都**不预置行**（instance_secrets 记录由初始化流程写）。
	for _, table := range []string{"instance_secrets", "mcp_credential_sources", "network_exceptions"} {
		require.Zero(t, countRows(t, fresh, table), "schema.sql 不得预置 %s 的行", table)
		require.Zero(t, countRows(t, upgraded, table), "0006 不得预置 %s 的行", table)
	}

	// PRAGMA 看不到 CHECK 约束，行为断言补上：全量 DDL 的 credentials 同样强制
	// 「mcp_server ⇔ 非空 binding_key」，否则新库会放过升级库拒绝的行。
	requireMCPCredentialBindingCheck(t, fresh)
}

// tableFingerprint 是单表的结构指纹：列（名 / 类型 / 非空 / 默认 / 主键位）、外键与索引
// 形状。PRAGMA 的输出与建表 SQL 的书写方式无关，两条路径因此可比；SQLite 在
// `ALTER TABLE … RENAME` 后保留自动索引的旧名字，故索引只按形状（唯一性 / 来源 / 列序）
// 记账，不记名字。
// tableFingerprint records columns, foreign keys and index shapes, not DDL text.
func tableFingerprint(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var b strings.Builder

	fmt.Fprintf(&b, "table=%s\n", table)

	columns, err := db.Query(`SELECT cid, name, type, "notnull", dflt_value, pk
		FROM pragma_table_info(?) ORDER BY cid`, table)
	require.NoError(t, err)
	for columns.Next() {
		var (
			cid     int64
			name    string
			colType string
			notNull int64
			dflt    any
			pk      int64
		)
		require.NoError(t, columns.Scan(&cid, &name, &colType, &notNull, &dflt, &pk))
		fmt.Fprintf(&b, "col|%d|%s|%s|%d|%v|%d\n", cid, name, colType, notNull, dflt, pk)
	}
	require.NoError(t, columns.Err())
	require.NoError(t, columns.Close())

	foreignKeys, err := db.Query(`SELECT id, seq, "table", "from", "to", on_update, on_delete, match
		FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table)
	require.NoError(t, err)
	for foreignKeys.Next() {
		var (
			id, seq               int64
			refTable, from, to    string
			onUpdate, onDelete, m string
		)
		require.NoError(t, foreignKeys.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &m))
		fmt.Fprintf(&b, "fk|%d|%d|%s|%s|%s|%s|%s|%s\n", id, seq, refTable, from, to, onUpdate, onDelete, m)
	}
	require.NoError(t, foreignKeys.Err())
	require.NoError(t, foreignKeys.Close())

	indexes, err := db.Query(`SELECT name, "unique", origin, partial
		FROM pragma_index_list(?)`, table)
	require.NoError(t, err)
	var shapes []string
	for indexes.Next() {
		var (
			name    string
			unique  int64
			origin  string
			partial int64
		)
		require.NoError(t, indexes.Scan(&name, &unique, &origin, &partial))
		shapes = append(shapes, fmt.Sprintf("idx|%d|%s|%d|%s",
			unique, origin, partial, strings.Join(indexColumns(t, db, name), ",")))
	}
	require.NoError(t, indexes.Err())
	require.NoError(t, indexes.Close())
	slices.Sort(shapes)
	for _, shape := range shapes {
		b.WriteString(shape + "\n")
	}

	return b.String()
}

// indexColumns 取索引的列序（自动索引同样适用）。
// indexColumns lists an index's column names in order.
func indexColumns(t *testing.T, db *sql.DB, index string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
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

// countRows 数表的行数（表必存在）。
// countRows counts a table's rows.
func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n))
	return n
}

// requireMCPCredentialBindingCheck 行为断言 credentials 的绑定键 CHECK：
// mcp_server 必须带占位符名，其它 kind 必须为空串。
// requireMCPCredentialBindingCheck asserts the binding-key CHECK behaviourally.
func requireMCPCredentialBindingCheck(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES ('op-v6check', 'mcp_server.create', 'mcp_server', 'github', 'admin', 'bearer', 'api', 'success', 1)`)
	require.NoError(t, err)

	const insertCred = `INSERT INTO credentials
		(credential_id, owner_kind, owner_uid, binding_key, owner_name, state, current_version,
		 revocation_epoch, key_id, created_at, updated_at, operation_id)
		VALUES (?, ?, 'rs_00000000000000000000000001', ?, 'github', 'active', 1, 0, 'mk-1', 1, 1, 'op-v6check')`
	_, err = db.Exec(insertCred, "cred_11111111111111111111111111", "mcp_server", "GITHUB_TOKEN")
	require.NoError(t, err, "mcp_server + 占位符名必须可写入")
	_, err = db.Exec(insertCred, "cred_22222222222222222222222222", "mcp_server", "")
	require.Error(t, err, "mcp_server 缺占位符名必须被 CHECK 拒绝")
	_, err = db.Exec(insertCred, "cred_33333333333333333333333333", "model_connection", "EXTRA")
	require.Error(t, err, "非 mcp_server 带占位符名必须被 CHECK 拒绝")
}

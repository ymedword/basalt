package testutil

// 本文件判卷夹具自身：版本行、按行推断的声明类型、`id` 主键改写与样例行插入。
// 夹具错了会把整批迁移用例的结论带偏，所以它自己也要有判据。
// This file pins the fixture builder itself so a broken fixture cannot skew the
// migration suite.

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// columnShape 是 PRAGMA table_info 里本包判卷用得到的字段。
type columnShape struct {
	Decl    string
	NotNull bool
	PK      int
}

// TestSQLiteFixture_VersionRow 缺省 v6；`SchemaVersion(n)` 写指定版本；
// `SchemaVersion(0)` 不建版本表（造"没有版本表"的坏库）。
func TestSQLiteFixture_VersionRow(t *testing.T) {
	var version int

	byDefault := SQLiteFixture(t)
	require.NoError(t, byDefault.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version))
	require.Equal(t, defaultSchemaVersion, version)

	legacy := SQLiteFixture(t, SchemaVersion(3))
	require.NoError(t, legacy.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version))
	require.Equal(t, 3, version)

	bare := SQLiteFixture(t, SchemaVersion(0))
	var tables int
	require.NoError(t, bare.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_version'`).Scan(&tables))
	require.Zero(t, tables, "SchemaVersion(0) 必须不建版本表")
}

// TestSQLiteFixture_InfersDeclaredTypesAndInsertsRows 列类型按首个非 nil 值推断，
// `id` 改写为 `INTEGER PRIMARY KEY`，样例行逐行插入。
// 逆改点：把 `[]byte` 推断成 TEXT → BLOB 断言红；不打 `id` 主键 → pk 断言红。
func TestSQLiteFixture_InfersDeclaredTypesAndInsertsRows(t *testing.T) {
	db := SQLiteFixture(t, Table("widgets",
		map[string]any{"id": int64(1), "name": "alpha", "score": 1.5, "raw": []byte{0x01}, "note": nil},
		map[string]any{"id": int64(2), "name": "beta", "score": 2.25, "raw": []byte{}, "note": "x"},
	))

	shapes := tableShapes(t, db, "widgets")
	require.Equal(t, "INTEGER", shapes["id"].Decl)
	require.Equal(t, 1, shapes["id"].PK, "id 必须是主键")
	require.Equal(t, "TEXT", shapes["name"].Decl)
	require.Equal(t, "REAL", shapes["score"].Decl)
	require.Equal(t, "BLOB", shapes["raw"].Decl)
	require.Equal(t, "TEXT", shapes["note"].Decl, "全 nil 列按 TEXT 兜底")

	var (
		name  string
		score float64
		note  sql.NullString
	)
	require.NoError(t, db.QueryRow(
		`SELECT name, score, note FROM widgets WHERE id = 1`).Scan(&name, &score, &note))
	require.Equal(t, "alpha", name)
	require.Equal(t, 1.5, score)
	require.False(t, note.Valid, "nil 值必须落成 NULL")

	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM widgets`).Scan(&count))
	require.Equal(t, 2, count)
}

// TestSQLiteFixture_BoolIsInteger 布尔值按 SQLite 惯例存 INTEGER。
func TestSQLiteFixture_BoolIsInteger(t *testing.T) {
	db := SQLiteFixture(t, Table("scheduled_tasks",
		map[string]any{"id": int64(1), "enabled": true}))

	shapes := tableShapes(t, db, "scheduled_tasks")
	require.Equal(t, "INTEGER", shapes["enabled"].Decl)

	var enabled int
	require.NoError(t, db.QueryRow(`SELECT enabled FROM scheduled_tasks WHERE id = 1`).Scan(&enabled))
	require.Equal(t, 1, enabled)
}

// tableShapes 读 PRAGMA table_info 的列形态。
func tableShapes(t *testing.T, db *sql.DB, table string) map[string]columnShape {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + quoteIdent(table) + `)`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	shapes := make(map[string]columnShape)
	for rows.Next() {
		var (
			cid     int
			name    string
			decl    string
			notNull int
			dflt    any
			pk      int
		)
		require.NoError(t, rows.Scan(&cid, &name, &decl, &notNull, &dflt, &pk))
		shapes[name] = columnShape{Decl: decl, NotNull: notNull != 0, PK: pk}
	}
	require.NoError(t, rows.Err())
	return shapes
}

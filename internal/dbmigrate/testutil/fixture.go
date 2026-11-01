// Package testutil 造**真实的 SQLite 夹具**给迁移测试用（颗粒度文档 §2.11.4：
// 这些用例必须使用真实 SQLite 夹具 + 临时 PG，不用假 dump）。
//
// 夹具写进 t.TempDir()，用毕由 testing 回收；返回的句柄由 t.Cleanup 关闭。
// Package testutil builds real SQLite fixtures for the migration tests: the migration suite
// must run against a real database file, never a hand-written fake dump.
package testutil

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // 驱动经空白导入注册为 "sqlite"
)

// driverName 是 modernc.org/sqlite 在 database/sql 里的注册名。
const driverName = "sqlite"

// defaultSchemaVersion 是夹具默认的源谱系版本（v6，当前唯一可导出的版本）。
const defaultSchemaVersion = 6

// fixture 收集所有选项，最后一次性建库。
type fixture struct {
	schemaVersion int
	tables        []tableSpec
}

// tableSpec 是一张待建的夹具表。
type tableSpec struct {
	name string
	rows []map[string]any
}

// Option 是夹具的一个配置项。
type Option func(*fixture)

// SchemaVersion 指定 `schema_version` 表里记录的版本号；缺省 v6。
// 传 0 表示**不建** `schema_version` 表（造"没有版本表"的坏库）。
func SchemaVersion(version int) Option {
	return func(f *fixture) { f.schemaVersion = version }
}

// Table 建一张表并插入给定行；列与声明类型按行推断：
// `int` / `int64` / `bool` → INTEGER，`float64` → REAL，`string` → TEXT，`[]byte` → BLOB，
// 全为 nil 的列 → TEXT。列名 `id` 声明为 `INTEGER PRIMARY KEY`。
// Table creates one fixture table; column types are inferred from the first non-nil value.
func Table(name string, rows ...map[string]any) Option {
	return func(f *fixture) {
		f.tables = append(f.tables, tableSpec{name: name, rows: rows})
	}
}

// SQLiteFixture 造库并返回可写句柄（建表、插入行都在这里做完）。
func SQLiteFixture(t *testing.T, opts ...Option) *sql.DB {
	t.Helper()

	f := fixture{schemaVersion: defaultSchemaVersion}
	for _, opt := range opts {
		opt(&f)
	}

	path := filepath.Join(t.TempDir(), "fixture.db")
	db, err := sql.Open(driverName, "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("打开夹具库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if f.schemaVersion != 0 {
		mustExec(t, db, `CREATE TABLE schema_version (
			version INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL,
			description TEXT NOT NULL)`)
		mustExec(t, db,
			`INSERT INTO schema_version (version, applied_at, description) VALUES (?, 0, 'fixture')`,
			f.schemaVersion)
	}
	for _, spec := range f.tables {
		createTable(t, db, spec)
	}
	return db
}

// createTable 按行推断列并建表插入。
func createTable(t *testing.T, db *sql.DB, spec tableSpec) {
	t.Helper()
	if len(spec.rows) == 0 {
		t.Fatalf("夹具表 %s 没有样例行，无法推断列", spec.name)
	}

	columns := make([]string, 0, len(spec.rows[0]))
	for name := range spec.rows[0] {
		columns = append(columns, name)
	}
	sort.Strings(columns)

	defs := make([]string, 0, len(columns))
	for _, name := range columns {
		defs = append(defs, quoteIdent(name)+" "+inferDecl(columns, spec.rows, name))
	}
	// `id` 是主键：迁移要用它算 ID 集合摘要，也让夹具贴近真实表形态。
	for i, name := range columns {
		if name == "id" {
			defs[i] = quoteIdent(name) + " INTEGER PRIMARY KEY"
			break
		}
	}

	mustExec(t, db, fmt.Sprintf("CREATE TABLE %s (%s)", quoteIdent(spec.name), strings.Join(defs, ", ")))

	placeholders := make([]string, len(columns))
	for i := range columns {
		placeholders[i] = "?"
	}
	insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		quoteIdent(spec.name), strings.Join(quoted(columns), ", "), strings.Join(placeholders, ", "))
	for _, row := range spec.rows {
		args := make([]any, len(columns))
		for i, name := range columns {
			args[i] = row[name]
		}
		mustExec(t, db, insert, args...)
	}
}

// inferDecl 按第一个非 nil 值的 Go 类型给列挑声明类型。
func inferDecl(columns []string, rows []map[string]any, name string) string {
	for _, row := range rows {
		switch row[name].(type) {
		case int, int64, bool:
			return "INTEGER"
		case float64:
			return "REAL"
		case []byte:
			return "BLOB"
		case string:
			return "TEXT"
		}
	}
	return "TEXT"
}

// mustExec 执行一条语句，失败即终止用例。
func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("夹具执行失败（%s）: %v", query, err)
	}
}

// quoteIdent 用双引号包住标识符。
func quoteIdent(name string) string {
	return `"` + name + `"`
}

// quoted 批量加引号。
func quoted(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = quoteIdent(name)
	}
	return out
}

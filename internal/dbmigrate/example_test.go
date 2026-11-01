package dbmigrate_test

// 本文件是 `internal/dbmigrate` 的可运行 Example（颗粒度文档 §4「新包 Example」：
// Example 的 `Output` 注释必须与真实输出一致，`go test` 无 `-run` 过滤时默认执行）。
// This is the package Example required by the module card; the Output comment must match
// the real output because `go test` runs it by default.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/dbmigrate"
)

// ExampleNewExporter 导出一份最小的 SQLite v6 源库：停机快照 + 每表 JSONL + 清单。
// 只打印确定性字段（批次号、生成时间、快照摘要每次运行都不同，不打印）。
// ExampleNewExporter exports a minimal v6 source: a snapshot, one JSONL per table, and the
// manifest. Only deterministic fields are printed.
func ExampleNewExporter() {
	workDir, err := os.MkdirTemp("", "basalt-export-example")
	mustNoErr(err)
	defer func() { _ = os.RemoveAll(workDir) }()

	source, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(workDir, "source.db")))
	mustNoErr(err)
	defer func() { _ = source.Close() }()

	for _, statement := range []string{
		`CREATE TABLE schema_version (
			version     INTEGER PRIMARY KEY,
			applied_at  INTEGER NOT NULL,
			description TEXT NOT NULL)`,
		`INSERT INTO schema_version (version, applied_at, description) VALUES (6, 0, 'example')`,
		`CREATE TABLE llm_calls (
			id     INTEGER PRIMARY KEY,
			status TEXT NOT NULL)`,
		`INSERT INTO llm_calls (id, status) VALUES (1, 'unknown'), (2, 'failed')`,
	} {
		_, err = source.Exec(statement)
		mustNoErr(err)
	}

	manifest, err := dbmigrate.NewExporter(source, dbmigrate.SourceSchemaVersion).
		Export(context.Background(), filepath.Join(workDir, "dump"))
	mustNoErr(err)

	fmt.Printf("format=%d source_version=%d tables=%d\n",
		manifest.FormatVersion, manifest.SourceSchemaVersion, len(manifest.Tables))
	for _, table := range manifest.Tables {
		fmt.Printf("%s rows=%d columns=%s\n",
			table.Name, table.RowCount, strings.Join(table.ColumnNames(), ","))
	}

	// Output:
	// format=1 source_version=6 tables=1
	// llm_calls rows=2 columns=id,status
}

// mustNoErr 在 Example 里失败即 panic（Example 没有 *testing.T 可传）。
func mustNoErr(err error) {
	if err != nil {
		panic(err)
	}
}

package storage_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ymedword/basalt/internal/storage"
)

// Example 演示存储能力层的完整调用序列：引导建库 → 只读打开 → 读版本与表清点。
// Example walks the whole call sequence of the persistence layer: bootstrap,
// read-only open, then version and table inventory.
//
// 用 os.MkdirTemp 而非 t.TempDir：Example 没有 *testing.T，但同样不留产物。
// os.MkdirTemp stands in for t.TempDir; the example still cleans up after itself.
func Example() {
	dir, err := os.MkdirTemp("", "basalt-example")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	ctx := context.Background()
	path := filepath.Join(dir, "basalt.db")

	created, err := storage.Bootstrap(ctx, path, storage.SupportedSchemaVersion)
	if err != nil {
		panic(err)
	}

	db, err := storage.OpenReadOnly(ctx, path, storage.SupportedSchemaVersion)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	version, err := db.SchemaVersion(ctx)
	if err != nil {
		panic(err)
	}
	tables, err := db.TableNames(ctx)
	if err != nil {
		panic(err)
	}

	fmt.Println("created:", created)
	fmt.Println("version:", version)
	fmt.Println("tables:", len(tables))
	// Output:
	// created: true
	// version: 1
	// tables: 11
}

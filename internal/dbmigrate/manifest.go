package dbmigrate

// 本文件是数据库侧 `manifest.json` 的**清单结构**（C-30 / 颗粒度文档 §2.11.1、§3.1.2）。
// 清单是导入端**唯一**的列元数据来源：目标表的 `information_schema` **不得**用来猜类型，
// `source_schema_version` 与 `postgres.SupportedSchemaVersion` 是**两个独立字段**，
// 导入时逐项比对、互不映射。
// This file holds the database-side manifest. It is the importer's only source of column
// metadata; the target's information_schema is never consulted to guess a type, and the
// source/target schema versions are compared as two independent fields.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	// ManifestFormatVersion 是 JSONL 与清单的格式版本（§2.11.1 `format_version=1`）。
	// 不认识的版本**必须**中止，不得按"看起来像"的字段猜测。
	// ManifestFormatVersion is the wire format version; unknown versions abort.
	ManifestFormatVersion = 1

	// ManifestFileName 是清单在导出目录里的固定文件名。
	ManifestFileName = "manifest.json"

	// SourceSchemaVersion 是导出端**唯一**接受的 SQLite 谱系版本。
	// 它与 `storage.SupportedSchemaVersion` 必须相等——值漂移由
	// `TestSourceSchemaVersion_MatchesStorage` 机器判卷；这里不 import `storage`，
	// 因为导出包只该认识"源库是什么形态"，不该被运行期存储包拖进来。
	// SourceSchemaVersion is the only SQLite lineage version this exporter accepts; a test
	// pins it to storage.SupportedSchemaVersion instead of importing that package here.
	SourceSchemaVersion = 6
)

// Manifest 是数据库侧导出清单（与 `assets.json` 分开存放、分开校验，靠三元组绑定）。
// Manifest is the database-side manifest; the asset manifest stays a separate artifact.
type Manifest struct {
	FormatVersion        int             `json:"format_version"`
	MigrationBatchID     string          `json:"migration_batch_id"`
	SourceInstanceID     string          `json:"source_instance_id"`
	SourceSchemaVersion  int             `json:"source_schema_version"`
	SourceSnapshotSHA256 string          `json:"source_snapshot_sha256"`
	GeneratedAt          time.Time       `json:"generated_at"`
	Tables               []TableManifest `json:"tables"`
}

// TableManifest 是一张表的导出口径：行数、列元数据与三个摘要。
// TableManifest is one table's export record: row count, column metadata and three digests.
type TableManifest struct {
	Name        string       `json:"name"`
	RowCount    int64        `json:"row_count"`
	Columns     []ColumnMeta `json:"columns"`
	IDSetSHA256 string       `json:"id_set_sha256"`
	RowSHA256   string       `json:"row_sha256"`
	JSONLSHA256 string       `json:"jsonl_sha256"`
}

// ColumnMeta 是一列的导入口径：声明类型只决定目标类型与可空性，**不**决定实际值类型
// （SQLite 是动态类型，实际存储类在编码时按值判定）。
// ColumnMeta is one column's import contract; the declared type never decides the value's
// storage class, which is read off the value itself.
type ColumnMeta struct {
	Name       string   `json:"name"`
	SQLiteDecl string   `json:"sqlite_decl"`
	PGType     string   `json:"pg_type"`
	Nullable   bool     `json:"nullable"`
	Encoding   ValueTag `json:"encoding"`
}

// ErrUnsupportedFormat 是格式版本不认识的哨兵：**不**尝试兼容解析。
var ErrUnsupportedFormat = errors.New("dbmigrate: 格式版本不受支持")

// WriteManifest 把清单写到目录下的 `manifest.json`（先写临时文件再改名，避免半截文件）。
// 目录由调用方给定，文件操作经 `os.Root` 限定在目录内（文件名是常量，但根限定让写入
// 不可能因为目录本身是符号链接而落到别处）。
func WriteManifest(dir string, manifest Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("dbmigrate: 序列化清单: %w", err)
	}
	raw = append(raw, '\n')
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("dbmigrate: 打开导出目录: %w", err)
	}
	defer func() { _ = root.Close() }()

	temp := ManifestFileName + ".tmp"
	if err := root.WriteFile(temp, raw, 0o600); err != nil {
		return fmt.Errorf("dbmigrate: 写清单: %w", err)
	}
	if err := root.Rename(temp, ManifestFileName); err != nil {
		return fmt.Errorf("dbmigrate: 落盘清单: %w", err)
	}
	return nil
}

// ReadManifest 读目录下的 `manifest.json`（同样经 `os.Root` 限定在目录内）。
func ReadManifest(dir string) (Manifest, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Manifest{}, fmt.Errorf("dbmigrate: 打开导出目录: %w", err)
	}
	defer func() { _ = root.Close() }()

	raw, err := root.ReadFile(ManifestFileName)
	if err != nil {
		return Manifest{}, fmt.Errorf("dbmigrate: 读清单: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("dbmigrate: 解析清单: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate 判清单自身的可导入口径：格式版本认识、源版本是 v6、每张表的列元数据可用。
// 缺字段按失败处理——"看不出来"不等于"没问题"。
func (m Manifest) Validate() error {
	if m.FormatVersion != ManifestFormatVersion {
		return fmt.Errorf("%w: format_version=%d（本工具支持 %d）",
			ErrUnsupportedFormat, m.FormatVersion, ManifestFormatVersion)
	}
	if m.SourceSchemaVersion != SourceSchemaVersion {
		return fmt.Errorf("%w: source_schema_version=%d（本工具只接受 %d）",
			ErrSourceVersionUnsupported, m.SourceSchemaVersion, SourceSchemaVersion)
	}
	seen := make(map[string]struct{}, len(m.Tables))
	for _, table := range m.Tables {
		if table.Name == "" {
			return fmt.Errorf("dbmigrate: 清单里有表名为空的条目")
		}
		if _, dup := seen[table.Name]; dup {
			return fmt.Errorf("dbmigrate: 清单里表 %q 出现两次", table.Name)
		}
		seen[table.Name] = struct{}{}
		if err := table.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate 判一张表的列元数据：列集合非空、列名不重复、`encoding` 是已知标签。
func (t TableManifest) Validate() error {
	if len(t.Columns) == 0 {
		return fmt.Errorf("dbmigrate: 表 %q 的列元数据为空", t.Name)
	}
	seen := make(map[string]struct{}, len(t.Columns))
	for _, column := range t.Columns {
		if column.Name == "" {
			return fmt.Errorf("dbmigrate: 表 %q 有空列名", t.Name)
		}
		if _, dup := seen[column.Name]; dup {
			return fmt.Errorf("dbmigrate: 表 %q 的列 %q 出现两次", t.Name, column.Name)
		}
		seen[column.Name] = struct{}{}
		if !column.Encoding.Known() {
			return fmt.Errorf("%w: 表 %q 的列 %q 用了未知 encoding %q",
				ErrInvalidValue, t.Name, column.Name, column.Encoding)
		}
	}
	return nil
}

// ColumnNames 返回列元数据里列名的有序副本（清单里的顺序即导出 / 导入的列序）。
func (t TableManifest) ColumnNames() []string {
	out := make([]string, len(t.Columns))
	for i, column := range t.Columns {
		out[i] = column.Name
	}
	return out
}

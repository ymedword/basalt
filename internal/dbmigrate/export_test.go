package dbmigrate_test

// 本文件判卷 SQLite v6 只读导出（C-30 / 颗粒度文档 §2.11.2）：版本门只接受 v6、
// 快照不被覆盖、列元数据按 PRAGMA 落清单、JSONL 摘要与规范化摘要与 `DigestRows` /
// `DigestIDSet` 同源。
// This file pins the read-only SQLite v6 export: the version gate, the snapshot
// non-overwrite rule, and that the export digests come from the shared encoder.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/dbmigrate"
	"github.com/ymedword/basalt/internal/dbmigrate/testutil"
	"github.com/ymedword/basalt/internal/storage"
)

// ctx 是包级测试上下文（颗粒度文档 §4.4 片段约定：不逐用例声明）。
var ctx = context.Background()

// TestExport_RequiresSQLiteV6 导出源版本门：只接受 SQLite v6；v5 / v7 一律拒绝。
//
// 版本门有**两道**：`Export` 开头的前置判定，以及清单自检里对 `source_schema_version`
// 的复核。只断言哨兵值分不出是哪一道拦下的——所以负向子例同时断言**导出目录里没有产物**：
// 前置判定必须在建目录、落快照之前就失败。
// 逆改点：把 `Export` 的版本检查改成"不高于目标即可"或删除 → v5 子例会先落出快照与 JSONL
// （产物断言红）；改成">= 5 即通过"→ 同上。
func TestExport_RequiresSQLiteV6(t *testing.T) {
	v6 := testutil.SQLiteFixture(t, testutil.SchemaVersion(6))
	_, err := dbmigrate.NewExporter(v6, 6).Export(ctx, t.TempDir())
	require.NoError(t, err)

	v5 := testutil.SQLiteFixture(t, testutil.SchemaVersion(5))
	v5Out := t.TempDir()
	_, err = dbmigrate.NewExporter(v5, 6).Export(ctx, v5Out)
	require.ErrorIs(t, err, dbmigrate.ErrSourceVersionUnsupported,
		"v5 源必须先由当前发布版升级到 v6，导出不得自动修补")
	requireNoArtifacts(t, v5Out)

	v7 := testutil.SQLiteFixture(t, testutil.SchemaVersion(7))
	v7Out := t.TempDir()
	_, err = dbmigrate.NewExporter(v7, 6).Export(ctx, v7Out)
	require.ErrorIs(t, err, dbmigrate.ErrSourceVersionUnsupported, "高于目标谱系的版本不得导出")
	requireNoArtifacts(t, v7Out)
}

// TestExport_RejectsNewerSchemaVersion 版本门的两条负向分支各自可辨：
// 版本表存在但偏高、以及版本表根本不存在——后者**不得**被当成"最新版本"放行。
// 逆改点：`sourceVersion` 在查不到版本表时返回 0 并允许导出 → 第二个子例红。
func TestExport_RejectsNewerSchemaVersion(t *testing.T) {
	newer := testutil.SQLiteFixture(t, testutil.SchemaVersion(7))
	_, err := dbmigrate.NewExporter(newer, 6).Export(ctx, t.TempDir())
	require.ErrorIs(t, err, dbmigrate.ErrSourceVersionUnsupported)

	// 没有版本表 = 无法证明是 v6；按不受支持处理，不猜。
	missing := testutil.SQLiteFixture(t, testutil.SchemaVersion(0))
	_, err = dbmigrate.NewExporter(missing, 6).Export(ctx, t.TempDir())
	require.ErrorIs(t, err, dbmigrate.ErrSourceVersionUnsupported)
	require.Contains(t, err.Error(), "schema_version")
}

// TestExport_WritesManifestAndJSONL 一次完整导出：清单字段、列元数据、两个摘要与
// JSONL 文件的对应关系逐项对号。
// 逆改点：清单里的 `pg_type` 改成"照抄声明类型"→ 列元数据断言红；摘要换成别的编码器
// （如 JSON 序列化值）→ 与 `DigestRows` 的等值断言红。
func TestExport_WritesManifestAndJSONL(t *testing.T) {
	source := testutil.SQLiteFixture(t, testutil.SchemaVersion(dbmigrate.SourceSchemaVersion))
	execSQL(t, source, `CREATE TABLE llm_calls (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		status  TEXT NOT NULL,
		score   REAL,
		note    TEXT,
		payload BLOB)`)
	execSQL(t, source,
		`INSERT INTO llm_calls (id, status, score, note, payload) VALUES (7, 'unknown', 1.5, NULL, x'00ff')`)
	execSQL(t, source,
		`INSERT INTO llm_calls (id, status, score, note, payload) VALUES (8, 'failed', NULL, '', NULL)`)

	outDir := t.TempDir()
	manifest, err := dbmigrate.NewExporter(source, dbmigrate.SourceSchemaVersion).Export(ctx, outDir)
	require.NoError(t, err)

	require.Equal(t, dbmigrate.ManifestFormatVersion, manifest.FormatVersion)
	require.Equal(t, dbmigrate.SourceSchemaVersion, manifest.SourceSchemaVersion)
	require.Equal(t, storage.SupportedSchemaVersion, manifest.SourceSchemaVersion,
		"导出的源版本必须是运行期 SQLite 谱系版本")
	require.Empty(t, manifest.SourceInstanceID,
		"实例身份来自名册，由 db export 补齐；导出端不得猜")
	require.Len(t, manifest.MigrationBatchID, 32, "批次号是 16 字节随机数的十六进制")
	require.Len(t, manifest.SourceSnapshotSHA256, 64)
	require.False(t, manifest.GeneratedAt.IsZero())
	require.Len(t, manifest.Tables, 1)

	table := manifest.Tables[0]
	require.Equal(t, "llm_calls", table.Name)
	require.EqualValues(t, 2, table.RowCount)
	require.Equal(t, []string{"id", "status", "score", "note", "payload"}, table.ColumnNames())
	require.Equal(t, []dbmigrate.ColumnMeta{
		// `INTEGER PRIMARY KEY` 是 rowid 别名，取值不可能是 NULL；但 PRAGMA 只报告
		// **显式** NOT NULL 声明，故这里如实记 true（清单描述源库声明，不替它加约束）。
		{Name: "id", SQLiteDecl: "INTEGER", PGType: "bigint", Nullable: true, Encoding: dbmigrate.TagInteger},
		{Name: "status", SQLiteDecl: "TEXT", PGType: "text", Nullable: false, Encoding: dbmigrate.TagText},
		{Name: "score", SQLiteDecl: "REAL", PGType: "double precision", Nullable: true, Encoding: dbmigrate.TagReal},
		{Name: "note", SQLiteDecl: "TEXT", PGType: "text", Nullable: true, Encoding: dbmigrate.TagText},
		{Name: "payload", SQLiteDecl: "BLOB", PGType: "bytea", Nullable: true, Encoding: dbmigrate.TagBytes},
	}, table.Columns)

	// 摘要必须与公开的规范化编码器同源（导出端与导入端共用一套，不得各写一份）。
	rows := []dbmigrate.Row{
		{
			"id": dbmigrate.Int(7), "status": dbmigrate.Text("unknown"), "score": dbmigrate.Real(1.5),
			"note": dbmigrate.Null(), "payload": dbmigrate.Bytes([]byte{0x00, 0xff}),
		},
		{
			"id": dbmigrate.Int(8), "status": dbmigrate.Text("failed"), "score": dbmigrate.Null(),
			"note": dbmigrate.Text(""), "payload": dbmigrate.Null(),
		},
	}
	rowDigest := dbmigrate.DigestRows(rows)
	require.NoError(t, rowDigest.Err)
	require.Equal(t, rowDigest.RowSHA256, table.RowSHA256)

	idSet, err := dbmigrate.DigestIDSet([]string{"id"}, rows)
	require.NoError(t, err)
	require.Equal(t, idSet, table.IDSetSHA256)

	jsonlPath := filepath.Join(outDir, "llm_calls.jsonl")
	raw, err := os.ReadFile(jsonlPath)
	require.NoError(t, err)
	require.Equal(t, sha256HexOf(raw), table.JSONLSHA256, "清单记的必须是 JSONL 原始字节摘要")
	require.Equal(t, 2, countLines(raw))

	// 清单落盘可读回，且与返回值一致（读回不等于"另算一份"）。
	stored, err := dbmigrate.ReadManifest(outDir)
	require.NoError(t, err)
	require.Equal(t, manifest, stored)

	snapshot, err := os.Stat(filepath.Join(outDir, "source-snapshot.db"))
	require.NoError(t, err, "停机快照必须单独留档")
	require.NotZero(t, snapshot.Size())
}

// TestExport_RefusesToOverwriteSnapshot 同一目录二次导出拒绝——重试必须显式清理目录，
// 不允许"悄悄换掉上一次的快照"。
// 逆改点：快照存在时继续覆盖 → 本用例红（第二次会成功）。
func TestExport_RefusesToOverwriteSnapshot(t *testing.T) {
	source := testutil.SQLiteFixture(t, testutil.SchemaVersion(dbmigrate.SourceSchemaVersion))
	outDir := t.TempDir()

	_, err := dbmigrate.NewExporter(source, dbmigrate.SourceSchemaVersion).Export(ctx, outDir)
	require.NoError(t, err)

	_, err = dbmigrate.NewExporter(source, dbmigrate.SourceSchemaVersion).Export(ctx, outDir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "拒绝覆盖")
}

// TestExport_RejectsUnknownSourceTable 迁移顺序里没有的表是 schema 漂移，**不**按表名字典序
// 硬塞进迁移。
func TestExport_RejectsUnknownSourceTable(t *testing.T) {
	source := testutil.SQLiteFixture(t, testutil.SchemaVersion(dbmigrate.SourceSchemaVersion))
	execSQL(t, source, `CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT)`)

	_, err := dbmigrate.NewExporter(source, dbmigrate.SourceSchemaVersion).Export(ctx, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "widgets")
}

// TestExport_RejectsTableWithoutPrimaryKey 没有主键的表算不出 ID 集合摘要，导出前就失败。
func TestExport_RejectsTableWithoutPrimaryKey(t *testing.T) {
	source := testutil.SQLiteFixture(t, testutil.SchemaVersion(dbmigrate.SourceSchemaVersion))
	execSQL(t, source, `CREATE TABLE llm_calls (status TEXT, created_at INTEGER)`)

	_, err := dbmigrate.NewExporter(source, dbmigrate.SourceSchemaVersion).Export(ctx, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "主键")
}

// TestExport_RejectsStorageClassMismatch 实际存储类与声明不符即中止：**不**隐式 CAST。
// 逆改点：把不匹配的值按声明类型硬转（TEXT 塞进 INTEGER 列时转 0）→ 本用例红。
func TestExport_RejectsStorageClassMismatch(t *testing.T) {
	// 首行决定夹具的声明类型：round 声明为 INTEGER，第二行却塞入 TEXT。
	source := testutil.SQLiteFixture(t, testutil.SchemaVersion(dbmigrate.SourceSchemaVersion),
		testutil.Table("llm_calls",
			map[string]any{"id": int64(1), "round": int64(1)},
			map[string]any{"id": int64(2), "round": "第一轮"}))

	_, err := dbmigrate.NewExporter(source, dbmigrate.SourceSchemaVersion).Export(ctx, t.TempDir())
	require.ErrorIs(t, err, dbmigrate.ErrInvalidValue)
	require.Contains(t, err.Error(), "不隐式 CAST")
}

// TestExport_RejectsInvalidUTF8 源库里的非法 UTF-8 文本必须在导出时中止（PG text 存不下）。
func TestExport_RejectsInvalidUTF8(t *testing.T) {
	source := testutil.SQLiteFixture(t, testutil.SchemaVersion(dbmigrate.SourceSchemaVersion))
	execSQL(t, source, `CREATE TABLE llm_calls (id INTEGER PRIMARY KEY, status TEXT)`)
	execSQL(t, source, `INSERT INTO llm_calls (id, status) VALUES (1, CAST(x'fffe' AS TEXT))`)

	_, err := dbmigrate.NewExporter(source, dbmigrate.SourceSchemaVersion).Export(ctx, t.TempDir())
	require.ErrorIs(t, err, dbmigrate.ErrInvalidValue)
	require.Contains(t, err.Error(), "UTF-8")
}

// TestSourceSchemaVersion_MatchesStorage 导出端接受的源版本必须与运行期 SQLite 谱系同源：
// 值漂移会让"只导出 v6"变成一句空话。
func TestSourceSchemaVersion_MatchesStorage(t *testing.T) {
	require.Equal(t, storage.SupportedSchemaVersion, dbmigrate.SourceSchemaVersion)
}

// requireNoArtifacts 断言导出目录里没有任何产物：目录根本不存在也算通过
// （版本门在 MkdirAll 之前就失败，比"建了空目录"更严格地满足"没落工件"）。
func requireNoArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	require.NoError(t, err)
	require.Empty(t, entries, "版本门必须在落任何工件之前拦下")
}

// execSQL 在夹具上补建表 / 补插行。
func execSQL(t *testing.T, db *sql.DB, statement string, args ...any) {
	t.Helper()
	_, err := db.Exec(statement, args...)
	require.NoErrorf(t, err, "执行：%s", statement)
}

// sha256HexOf 算原始字节的 SHA-256（导出端摘要口径）。
func sha256HexOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// countLines 数 JSONL 的行数（每条记录一个换行）。
func countLines(data []byte) int {
	return bytes.Count(data, []byte("\n"))
}

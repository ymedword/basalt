package dbmigrate

// 本文件判卷 `manifest.json` 的读写与自检（颗粒度文档 §2.11.1、§3.1.2）：清单是导入端
// **唯一**的列元数据来源，所以"看不出来"一律按失败处理——格式版本不认识、源版本不是 v6、
// 表名重复、列元数据为空、编码标签未知，全部拒绝。
// This file pins the manifest read/write and its fail-closed self-check.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// sampleManifest 造一份合法的清单：一张表、一个整数主键列。
func sampleManifest() Manifest {
	return Manifest{
		FormatVersion:        ManifestFormatVersion,
		MigrationBatchID:     "0f0e0d0c0b0a09080706050403020100",
		SourceInstanceID:     "inst-1",
		SourceSchemaVersion:  SourceSchemaVersion,
		SourceSnapshotSHA256: "a3f1",
		GeneratedAt:          time.Date(2026, 10, 29, 1, 2, 3, 456, time.UTC),
		Tables: []TableManifest{{
			Name:     "llm_calls",
			RowCount: 2,
			Columns: []ColumnMeta{
				{Name: "id", SQLiteDecl: "INTEGER", PGType: pgTypeBigInt, Nullable: false, Encoding: TagInteger},
				{Name: "status", SQLiteDecl: "TEXT", PGType: pgTypeText, Nullable: true, Encoding: TagText},
			},
			IDSetSHA256: "id-sum",
			RowSHA256:   "row-sum",
			JSONLSHA256: "jsonl-sum",
		}},
	}
}

// TestManifest_RoundTrip 写读来回逐字段相同，且不残留临时文件。
// 逆改点：把 `os.Rename` 改成直接写目标文件 → 断电语义变了但这断言不红；把
// `MarshalIndent` 的字段名改掉 → 读回字段为零值，断言红。
func TestManifest_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := sampleManifest()
	require.NoError(t, WriteManifest(dir, want))

	got, err := ReadManifest(dir)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, []string{"id", "status"}, got.Tables[0].ColumnNames(),
		"列名副本必须保持清单里的列序")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{ManifestFileName}, names, "落盘后不得残留 .tmp")
}

// TestManifest_RejectsUnknownFormatVersion 不认识的格式版本中止，**不**按字段猜测。
func TestManifest_RejectsUnknownFormatVersion(t *testing.T) {
	dir := t.TempDir()
	raw, err := json.Marshal(sampleManifest())
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	decoded["format_version"] = ManifestFormatVersion + 1

	writeJSONFile(t, filepath.Join(dir, ManifestFileName), decoded)
	_, err = ReadManifest(dir)
	require.ErrorIs(t, err, ErrUnsupportedFormat)

	broken := sampleManifest()
	broken.FormatVersion = 0
	require.ErrorIs(t, WriteManifest(dir, broken), ErrUnsupportedFormat)
}

// TestManifest_RejectsForeignSourceVersion 源版本不是 v6 一律拒绝（v5 先升级、v7 高于本谱系）。
func TestManifest_RejectsForeignSourceVersion(t *testing.T) {
	dir := t.TempDir()
	for _, version := range []int{0, 5, 7} {
		manifest := sampleManifest()
		manifest.SourceSchemaVersion = version
		require.ErrorIs(t, WriteManifest(dir, manifest), ErrSourceVersionUnsupported,
			"source_schema_version=%d 必须拒绝", version)
	}
}

// TestManifest_RejectsBrokenTableMetadata 空列元数据 / 重复表 / 未知编码全部拒绝。
func TestManifest_RejectsBrokenTableMetadata(t *testing.T) {
	dir := t.TempDir()

	noColumns := sampleManifest()
	noColumns.Tables[0].Columns = nil
	err := WriteManifest(dir, noColumns)
	require.Error(t, err)
	require.Contains(t, err.Error(), "列元数据为空")

	duplicate := sampleManifest()
	duplicate.Tables = append(duplicate.Tables, duplicate.Tables[0])
	require.Contains(t, WriteManifest(dir, duplicate).Error(), "出现两次")

	unknownEncoding := sampleManifest()
	unknownEncoding.Tables[0].Columns[1].Encoding = ValueTag("z")
	require.ErrorIs(t, WriteManifest(dir, unknownEncoding), ErrInvalidValue)

	duplicateColumn := sampleManifest()
	duplicateColumn.Tables[0].Columns = append(duplicateColumn.Tables[0].Columns,
		duplicateColumn.Tables[0].Columns[0])
	require.Contains(t, WriteManifest(dir, duplicateColumn).Error(), "出现两次")
}

// TestManifest_ReadRejectsMissingFile 缺清单是显式错误，不是"空清单"。
func TestManifest_ReadRejectsMissingFile(t *testing.T) {
	_, err := ReadManifest(t.TempDir())
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrInvalidValue))
}

// TestManifest_SourceSchemaVersionMatchesStorageVersion 见 export_test.go 的外部判卷；
// 本处只钉住常量自身的取值，防止被"顺手"改成别的版本。
func TestManifest_SourceSchemaVersionMatchesStorageVersion(t *testing.T) {
	require.Equal(t, 6, SourceSchemaVersion)
}

// writeJSONFile 把任意结构写成 JSON 文件（造坏清单用）。
func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}

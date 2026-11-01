package dbmigrate

// 本文件判卷**离线复核**（颗粒度文档 §2.15.3 / §3.1.2 的 `VerifyDump`）：清单声明的
// 文件摘要、行摘要、主键集合摘要任一不符都必须失败，报告点名表与判据。
// 不需要 PostgreSQL：离线复核只看 dump 目录与清单，因此这些用例进默认测试集，
// 不靠 integration 标签。
// These cases pin VerifyDump's three per-table criteria; no database is involved.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVerifyDump_DetectsTamperedDump dump 被改过时离线复核必须失败，且三项判据各自
// 单独可用：只改字节（语义不变）时由文件摘要抓；改语义时行摘要与主键集合摘要都变。
// 逆改点：删掉文件摘要比对 → 子例「只改字节」通过；删掉行摘要比对 → 子例「改语义」
// 只剩 ID 集合一条判据，`RowDigestOK` 仍为真，断言红。
func TestVerifyDump_DetectsTamperedDump(t *testing.T) {
	ctx := context.Background()

	t.Run("只改字节", func(t *testing.T) {
		dir, manifest := writeSyntheticDump(t)

		report, err := VerifyDump(ctx, dir, manifest)
		require.NoError(t, err, "未被改动的 dump 必须通过")
		require.True(t, report.OK)
		require.Equal(t, int64(2), report.TargetRows["sessions"], "现场读数来自 dump 文件本身")

		tamperDumpFile(t, dir, `{"session_id":`, `{"session_id" :`)

		report, err = VerifyDump(ctx, dir, manifest)
		require.ErrorIs(t, err, ErrVerifyFailed)
		require.False(t, report.OK)
		require.Contains(t, err.Error(), "文件摘要", "字节变了必须报文件摘要不符")
		require.Contains(t, err.Error(), "sessions", "错误必须点名表")
		require.True(t, report.RowDigestOK["sessions"],
			"只多了空白，行摘要与主键集合仍应相符——这条判据由文件摘要单独兜住")
	})

	t.Run("改语义", func(t *testing.T) {
		dir, manifest := writeSyntheticDump(t)
		tamperDumpFile(t, dir, `"v":"s2"`, `"v":"s9"`)

		report, err := VerifyDump(ctx, dir, manifest)
		require.ErrorIs(t, err, ErrVerifyFailed)
		require.False(t, report.RowDigestOK["sessions"])
		require.Contains(t, err.Error(), "行摘要")
		require.Contains(t, err.Error(), "ID 集合摘要", "主键值变了，ID 集合摘要也必须不符")
		require.Equal(t, int64(2), report.TargetRows["sessions"], "行数没变——只比行数发现不了这种改动")
	})
}

// writeSyntheticDump 造一份最小 dump：一张 `sessions` 表、两行，经真实 `writeJSONL`
// 落盘并按同一套编码器算出三项摘要，清单由这些读数组装。
func writeSyntheticDump(t *testing.T) (string, Manifest) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()

	columns := []ColumnMeta{{
		Name: "session_id", SQLiteDecl: "TEXT", PGType: pgTypeText, Nullable: false, Encoding: TagText,
	}}
	names := []string{"session_id"}
	rows := []Row{
		{"session_id": Text("s1")},
		{"session_id": Text("s2")},
	}
	fileSHA, err := writeJSONL(root, "sessions.jsonl", rows, names)
	require.NoError(t, err)

	digest := DigestRows(rows)
	require.NoError(t, digest.Err)
	idSet, err := DigestIDSet(sourcePrimaryKeys["sessions"], rows)
	require.NoError(t, err)

	manifest := Manifest{
		FormatVersion:       ManifestFormatVersion,
		SourceSchemaVersion: SourceSchemaVersion,
		Tables: []TableManifest{{
			Name:        "sessions",
			RowCount:    int64(len(rows)),
			Columns:     columns,
			IDSetSHA256: idSet,
			RowSHA256:   digest.RowSHA256,
			JSONLSHA256: fileSHA,
		}},
	}
	require.NoError(t, manifest.Validate())
	return dir, manifest
}

// tamperDumpFile 把 dump 里 `sessions.jsonl` 的指定片段替换掉（片段不存在即用例前提不成立）。
func tamperDumpFile(t *testing.T, dir, old, replacement string) {
	t.Helper()
	path := filepath.Join(dir, "sessions.jsonl")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), old, "夹具前提：待篡改的片段必须存在")
	require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(data), old, replacement)), 0o600))
}

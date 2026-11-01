package dbmigrate

// 本文件判卷 JSONL 的**线上形态**（颗粒度文档 §2.11.1）：一行一条记录、每个值带类型标签、
// NULL 写成 JSON `null`，且读回后与写入值逐位相同——整数不经浮点、实数保留 IEEE754 位模式、
// 空串与 NULL 不撞、BLOB 走 base64。
// This file pins the JSONL wire shape and its round-trip fidelity.

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestJSONL_RoundTripPreservesEveryStorageClass 四类标签 + NULL 走一遍写读来回。
// 逆改点：把实数改成十进制小数输出（丢掉位模式）→ 负零 / NaN 断言红；把 NULL 编成空串 →
// 与 `Text("")` 撞车，读回不相等。
func TestJSONL_RoundTripPreservesEveryStorageClass(t *testing.T) {
	dir := t.TempDir()
	root := openRoot(t, dir)
	columns := []string{"id", "note", "status"}
	rows := []Row{
		{"id": Int(7), "note": Null(), "status": Text("unknown")},
		{"id": Int(math.MinInt64), "note": Text(""), "status": Bytes([]byte{0x00, 0xff})},
		{"id": Int(math.MaxInt64), "note": Real(math.Copysign(0, -1)), "status": Real(math.NaN())},
	}

	digest, err := writeJSONL(root, "rows.jsonl", rows, columns)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, "rows.jsonl"))
	require.NoError(t, err)
	require.Equal(t, sha256Hex(raw), digest, "清单里的摘要必须是 JSONL 原始字节的 SHA-256")
	require.Len(t, digest, 64)

	back, err := readJSONL(root, "rows.jsonl", columns)
	require.NoError(t, err)
	require.Equal(t, rows, back, "写读来回必须逐位相同")
}

// TestJSONL_WireFormatIsTheFrozenShape 钉住线上字面量形态本身。
// 逆改点：把 `{"t":"i","v":"7"}` 改成裸数字 `7` → 字面量断言红。
func TestJSONL_WireFormatIsTheFrozenShape(t *testing.T) {
	dir := t.TempDir()
	_, err := writeJSONL(openRoot(t, dir), "rows.jsonl", []Row{{
		"id":     Int(7),
		"note":   Null(),
		"status": Text("无"),
	}}, []string{"id", "note", "status"})
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(dir, "rows.jsonl"))
	require.NoError(t, err)
	require.Equal(t,
		`{"id":{"t":"i","v":"7"},"note":null,"status":{"t":"s","v":"无"}}`+"\n",
		string(raw))
}

// TestJSONL_DistinguishesNullEmptyTextAndEmptyBytes NULL / 空串 / 空 BLOB 是三种不同形态。
func TestJSONL_DistinguishesNullEmptyTextAndEmptyBytes(t *testing.T) {
	dir := t.TempDir()
	root := openRoot(t, dir)
	columns := []string{"null_col", "text_col", "bytes_col"}
	rows := []Row{{"null_col": Null(), "text_col": Text(""), "bytes_col": Bytes(nil)}}

	_, err := writeJSONL(root, "rows.jsonl", rows, columns)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, "rows.jsonl"))
	require.NoError(t, err)
	require.Contains(t, string(raw), `"null_col":null`)
	require.Contains(t, string(raw), `"text_col":{"t":"s","v":""}`)
	require.Contains(t, string(raw), `"bytes_col":{"t":"b","v":""}`)

	back, err := readJSONL(root, "rows.jsonl", columns)
	require.NoError(t, err)
	require.True(t, back[0]["null_col"].IsNull())
	require.Equal(t, TagText, back[0]["text_col"].Tag)
	require.Equal(t, TagBytes, back[0]["bytes_col"].Tag)
}

// TestJSONL_EmptyTableWritesEmptyFile 空表产出 0 字节文件，其摘要是空串的 SHA-256——
// 与"根本没写文件"区分开。
func TestJSONL_EmptyTableWritesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	digest, err := writeJSONL(openRoot(t, dir), "rows.jsonl", nil, []string{"id"})
	require.NoError(t, err)
	require.Equal(t, sha256Hex(nil), digest)

	info, err := os.Stat(filepath.Join(dir, "rows.jsonl"))
	require.NoError(t, err)
	require.Zero(t, info.Size())
}

// TestJSONL_RejectsUnknownTag 未知标签在读入端即拒绝，**不**当成某种字节放行。
func TestJSONL_RejectsUnknownTag(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rows.jsonl"),
		[]byte(`{"id":{"t":"x","v":"1"}}`+"\n"), 0o600))

	_, err := readJSONL(openRoot(t, dir), "rows.jsonl", []string{"id"})
	require.ErrorIs(t, err, ErrInvalidValue)
	require.Contains(t, err.Error(), "未知")
}

// TestJSONL_RejectsColumnSetMismatch 缺列与多列都拒绝——**不**按位置对齐、**不**补 NULL。
func TestJSONL_RejectsColumnSetMismatch(t *testing.T) {
	dir := t.TempDir()
	root := openRoot(t, dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "missing.jsonl"),
		[]byte(`{"a":{"t":"i","v":"1"}}`+"\n"), 0o600))
	_, err := readJSONL(root, "missing.jsonl", []string{"a", "b"})
	require.ErrorIs(t, err, ErrInvalidValue)
	require.Contains(t, err.Error(), "缺列")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "extra.jsonl"),
		[]byte(`{"a":{"t":"i","v":"1"},"b":{"t":"i","v":"2"}}`+"\n"), 0o600))
	_, err = readJSONL(root, "extra.jsonl", []string{"a"})
	require.ErrorIs(t, err, ErrInvalidValue)
	require.Contains(t, err.Error(), "多列")

	// 写入端同样拒绝缺列：不得把缺失当成 NULL 写出去。
	_, err = writeJSONL(root, "short.jsonl", []Row{{"a": Int(1)}}, []string{"a", "b"})
	require.ErrorIs(t, err, ErrInvalidValue)
}

// TestJSONL_RejectsNulAndInvalidUTF8 写入端在编码时就拒绝，**不**落盘坏行。
func TestJSONL_RejectsNulAndInvalidUTF8(t *testing.T) {
	root := openRoot(t, t.TempDir())
	_, err := writeJSONL(root, "nul.jsonl",
		[]Row{{"a": Text("x\x00y")}}, []string{"a"})
	require.ErrorIs(t, err, ErrInvalidValue)
	require.True(t, strings.Contains(err.Error(), "NUL"), err.Error())

	_, err = writeJSONL(root, "utf8.jsonl",
		[]Row{{"a": Text(string([]byte{0xff, 0xfe}))}}, []string{"a"})
	require.ErrorIs(t, err, ErrInvalidValue)
	require.True(t, strings.Contains(err.Error(), "UTF-8"), err.Error())
}

// openRoot 打开测试目录并以 `*os.Root` 返回（用毕由 t.Cleanup 关闭）。
// 生产代码的读写都经 `os.Root`，测试必须走同一条路径，否则测的不是线上形态。
func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	return root
}

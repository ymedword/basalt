package dbmigrate_test

// 本文件是规范化摘要与导入 / 校验报告的验收面（C-30 / 颗粒度文档 §2.11.1、§3.1.2）：
//   - `TestDigest_IDSetGoldenVectors`：testdata 里冻结的 ID 集合摘要逐条比对（复合主键
//     按声明列序、NULL 与空串区分、多字节文本按字节序）；
//   - `TestDigest_RowOrderIndependent`：行集合的先后不改变摘要——导出端按拓扑序读、
//     导入端按批次读，两边顺序本来就可能不同；
//   - `TestDigest_RejectsInvalidText` / `_RejectsNulText` / `_RejectsMissingColumn`：
//     非法 UTF-8、NUL、缺列都必须**显式失败**，不得编码成"某种字节"混过去；
//   - `TestVerifyReport_Finalize`：`OK` 只有行数、缺 / 多 ID、行摘要三项都相符才为真。
// This file pins the canonical digests against frozen golden vectors and the failure paths.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/dbmigrate"
)

// idSetVector 是 testdata 里一条 golden vector：输入（主键列序 + 行）与期望摘要。
type idSetVector struct {
	Name        string                        `json:"name"`
	KeyColumns  []string                      `json:"key_columns"`
	Rows        []map[string]idSetVectorValue `json:"rows"`
	IDSetSHA256 string                        `json:"id_set_sha256"`
}

// idSetVectorValue 是向量里的一个值：`t` 是类型标签（空 = NULL），`v` 是规范字面量。
type idSetVectorValue struct {
	Tag   string `json:"t"`
	Value string `json:"v"`
}

// decode 把向量值还原成 `EncodedValue`——直接构造，**不**经过构造函数，
// 这样向量文件本身就能表达"非法输入"这一类用例。
func decode(t *testing.T, column string, v idSetVectorValue) dbmigrate.EncodedValue {
	t.Helper()
	if v.Tag == "" {
		require.Empty(t, v.Value, "NULL 不得带字面量：列 %s", column)
		return dbmigrate.Null()
	}
	return dbmigrate.EncodedValue{Tag: dbmigrate.ValueTag(v.Tag), Raw: []byte(v.Value)}
}

// loadVectors 读 testdata/idset_vectors.json。
func loadVectors(t *testing.T) []idSetVector {
	t.Helper()
	path := filepath.Join("testdata", "idset_vectors.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "golden vector 文件必须存在：%s", path)

	var file struct {
		FormatVersion int           `json:"format_version"`
		Vectors       []idSetVector `json:"vectors"`
	}
	require.NoError(t, json.Unmarshal(raw, &file))
	require.Equal(t, 1, file.FormatVersion, "format_version 与 §2.11.1 同步")
	require.NotEmpty(t, file.Vectors, "向量集不得为空")
	return file.Vectors
}

// buildRows 把向量还原成 `[]Row`。
func buildRows(t *testing.T, vector idSetVector) []dbmigrate.Row {
	t.Helper()
	rows := make([]dbmigrate.Row, 0, len(vector.Rows))
	for i, raw := range vector.Rows {
		row := make(dbmigrate.Row, len(raw))
		for column, value := range raw {
			row[column] = decode(t, column, value)
		}
		require.NotEmpty(t, row, "第 %d 行不得为空", i)
		rows = append(rows, row)
	}
	return rows
}

// TestDigest_IDSetGoldenVectors 逐条比对冻结摘要；任一条变化都说明编码器被改动。
// 逆改点：把列间分隔去掉（直接拼接）、按 map 遍历取列、或按文本排序改整数字面量，
// 对应向量的摘要都会变。
func TestDigest_IDSetGoldenVectors(t *testing.T) {
	for _, vector := range loadVectors(t) {
		t.Run(vector.Name, func(t *testing.T) {
			require.NotEmpty(t, vector.IDSetSHA256, "向量必须带冻结摘要：%s", vector.Name)

			got, err := dbmigrate.DigestIDSet(vector.KeyColumns, buildRows(t, vector))
			require.NoError(t, err)
			require.Equal(t, vector.IDSetSHA256, got, "ID 集合摘要必须与冻结向量逐字相等")
		})
	}
}

// TestDigest_RowOrderIndependent 行集合的输入顺序不改变两个摘要。
// 逆改点：`DigestRows` / `DigestIDSet` 若直接按入参顺序拼接（不排序），本用例必红。
func TestDigest_RowOrderIndependent(t *testing.T) {
	for _, vector := range loadVectors(t) {
		t.Run(vector.Name, func(t *testing.T) {
			rows := buildRows(t, vector)
			reversed := make([]dbmigrate.Row, len(rows))
			for i := range rows {
				reversed[len(rows)-1-i] = rows[i]
			}

			forward := dbmigrate.DigestRows(rows)
			require.NoError(t, forward.Err)
			backward := dbmigrate.DigestRows(reversed)
			require.NoError(t, backward.Err)
			require.Equal(t, forward.RowSHA256, backward.RowSHA256, "行摘要与行顺序无关")
			require.Equal(t, forward.RowCount, backward.RowCount)

			idForward, err := dbmigrate.DigestIDSet(vector.KeyColumns, rows)
			require.NoError(t, err)
			idBackward, err := dbmigrate.DigestIDSet(vector.KeyColumns, reversed)
			require.NoError(t, err)
			require.Equal(t, idForward, idBackward, "ID 集合摘要与行顺序无关")
		})
	}
}

// TestDigest_CompositeKeyKeepsColumnBoundaries 复合主键必须保留列边界：
// `("a","bc")` 与 `("ab","c")` 拼成同一裸串，但它们是不同的键。
// **逆改点实测修正（2026-10-29）**：只把列间 `\x1f` 换成空串时，上一对仍因每个值都
// 带类型标签 `s:` 而不同——那一条**没有**判别力。真正的判据是第二对：
// `("", "s:x")` 与 `("s:", "x")` 在无分隔符时都拼成 `s:s:s:x`，只有列间分隔符能把
// 它们分开。两对都保留：前者守"裸串相同"，后者守"标签补齐不得顶替分隔符"。
func TestDigest_CompositeKeyKeepsColumnBoundaries(t *testing.T) {
	digestOf := func(t *testing.T, a, b string) string {
		t.Helper()
		digest, err := dbmigrate.DigestIDSet([]string{"a", "b"}, []dbmigrate.Row{{
			"a": dbmigrate.Text(a),
			"b": dbmigrate.Text(b),
		}})
		require.NoError(t, err)
		return digest
	}

	require.NotEqual(t, digestOf(t, "a", "bc"), digestOf(t, "ab", "c"),
		"复合主键不得退化成裸串拼接")

	require.NotEqual(t, digestOf(t, "", "s:x"), digestOf(t, "s:", "x"),
		"类型标签恰好补齐时也不得顶替列间分隔符")
}

// TestDigest_EmptyTableUsesEmptyDigest 空表的行摘要取空串的 SHA-256，且与"未计算"不同。
func TestDigest_EmptyTableUsesEmptyDigest(t *testing.T) {
	digest := dbmigrate.DigestRows(nil)
	require.NoError(t, digest.Err)
	require.Zero(t, digest.RowCount)
	require.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", digest.RowSHA256)
	require.Empty(t, digest.IDSetSHA256, "行摘要入口不填 ID 集合摘要")
}

// TestDigest_DistinguishesNullEmptyAndLiteralNull 三个值互不相同——NULL、空串、
// 字符串 "null" 在编码里各有字面量。
func TestDigest_DistinguishesNullEmptyAndLiteralNull(t *testing.T) {
	digests := map[string]string{}
	for name, value := range map[string]dbmigrate.EncodedValue{
		"null":         dbmigrate.Null(),
		"empty":        dbmigrate.Text(""),
		"literal-null": dbmigrate.Text("null"),
	} {
		digest := dbmigrate.DigestRows([]dbmigrate.Row{{"v": value}})
		require.NoError(t, digest.Err)
		digests[name] = digest.RowSHA256
	}
	require.NotEqual(t, digests["null"], digests["empty"])
	require.NotEqual(t, digests["empty"], digests["literal-null"])
	require.NotEqual(t, digests["null"], digests["literal-null"])
}

// TestDigest_RejectsInvalidText 非法 UTF-8 必须显式失败，不得替换成 U+FFFD 后继续。
// 逆改点：编码器改用 `strings.ToValidUTF8` 清洗 → 本用例拿不到 `ErrInvalidValue`。
func TestDigest_RejectsInvalidText(t *testing.T) {
	digest := dbmigrate.DigestRows([]dbmigrate.Row{{
		"v": dbmigrate.EncodedValue{Tag: dbmigrate.TagText, Raw: []byte{0xff, 0xfe}},
	}})
	require.ErrorIs(t, digest.Err, dbmigrate.ErrInvalidValue)
	require.Empty(t, digest.RowSHA256, "失败时不得留下看似合法的摘要")
}

// TestDigest_RejectsNulText NUL 字节必须显式失败（PG text / jsonb 不接受）。
func TestDigest_RejectsNulText(t *testing.T) {
	digest := dbmigrate.DigestRows([]dbmigrate.Row{{
		"v": dbmigrate.Text("a\x00b"),
	}})
	require.ErrorIs(t, digest.Err, dbmigrate.ErrInvalidValue)
	require.Empty(t, digest.RowSHA256)
}

// TestDigest_RejectsMissingColumn 缺列不得按 NULL 补——本用例漏了 b 列。
func TestDigest_RejectsMissingColumn(t *testing.T) {
	rows := []dbmigrate.Row{
		{"a": dbmigrate.Int(1), "b": dbmigrate.Text("x")},
		{"a": dbmigrate.Int(2)},
	}
	digest := dbmigrate.DigestRows(rows)
	require.ErrorIs(t, digest.Err, dbmigrate.ErrInvalidValue)
	require.Empty(t, digest.RowSHA256)
}

// TestDigestIDSet_RejectsMissingKeyColumn 主键列缺失同样是失败，不是"补 NULL"。
func TestDigestIDSet_RejectsMissingKeyColumn(t *testing.T) {
	_, err := dbmigrate.DigestIDSet([]string{"a", "b"}, []dbmigrate.Row{{"a": dbmigrate.Int(1)}})
	require.ErrorIs(t, err, dbmigrate.ErrInvalidValue)
}

// TestDigestIDSet_RejectsEmptyKeyColumns 主键列序为空即失败——空列序会算出"所有表相同"的
// 摘要，把"没核对"伪装成"核对通过"。
func TestDigestIDSet_RejectsEmptyKeyColumns(t *testing.T) {
	_, err := dbmigrate.DigestIDSet(nil, []dbmigrate.Row{{"a": dbmigrate.Int(1)}})
	require.ErrorIs(t, err, dbmigrate.ErrInvalidValue)
}

// TestImportReport_Failed 任一失败表即整体失败；行数合计按表累加。
func TestImportReport_Failed(t *testing.T) {
	report := dbmigrate.ImportReport{Rows: map[string]int64{"sessions": 3, "runs": 2}}
	require.False(t, report.Failed())
	require.Equal(t, int64(5), report.TotalRows())
	require.Equal(t, 2, report.TableCount())

	report.Failures = map[string]string{"messages": "digest_mismatch"}
	require.True(t, report.Failed())
}

// TestVerifyReport_Finalize `OK` 的四项判据：行数相符、无缺 / 多 ID、行摘要相符、
// 关键引用无失败；任一项不符或缺表都判失败。
func TestVerifyReport_Finalize(t *testing.T) {
	clean := dbmigrate.NewVerifyReport([]string{"sessions", "runs"})
	clean.SourceRows = map[string]int64{"sessions": 3, "runs": 2}
	clean.TargetRows = map[string]int64{"sessions": 3, "runs": 2}
	require.True(t, clean.Finalize().OK, "四项都相符时 OK 为真")

	cases := map[string]func(*dbmigrate.VerifyReport){
		"行数不符":  func(r *dbmigrate.VerifyReport) { r.TargetRows["sessions"] = 4 },
		"缺 ID":  func(r *dbmigrate.VerifyReport) { r.MissingIDs["runs"] = []string{"r-1"} },
		"多 ID":  func(r *dbmigrate.VerifyReport) { r.ExtraIDs["runs"] = []string{"r-9"} },
		"行摘要不符": func(r *dbmigrate.VerifyReport) { r.RowDigestOK["runs"] = false },
		"关键引用失败": func(r *dbmigrate.VerifyReport) {
			r.ReferenceFailures["run_resource_refs"] = []string{"content_hash 不一致"}
		},
		"目标缺表":  func(r *dbmigrate.VerifyReport) { delete(r.TargetRows, "runs") },
		"目标多出表": func(r *dbmigrate.VerifyReport) { r.TargetRows["extra"] = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			report := dbmigrate.NewVerifyReport([]string{"sessions", "runs"})
			report.SourceRows = map[string]int64{"sessions": 3, "runs": 2}
			report.TargetRows = map[string]int64{"sessions": 3, "runs": 2}
			mutate(&report)
			require.False(t, report.Finalize().OK, "%s 时 OK 必须为假", name)
		})
	}
}

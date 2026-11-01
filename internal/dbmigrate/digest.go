package dbmigrate

// 本文件是**规范化摘要**与导入 / 校验报告（C-30 / 颗粒度文档 §2.11.1 与 §3.1.2）：
// 行哈希与 `IDSetSHA256` 走同一个编码器，复合主键按声明列序编码、按字节升序排序整行后
// 再算 SHA-256——**不得**按 map 遍历或按数据库返回顺序。
// This file holds the canonical digests and the import / verify reports. Row hashes and
// IDSetSHA256 share one encoder; composite keys are encoded in declared column order and
// sorted by bytes before hashing, never by map iteration or driver order.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	// columnSeparator 是列间分隔（0x1f，单元分隔符）。
	columnSeparator byte = 0x1f
	// recordSeparator 是行间分隔（0x1e，记录分隔符）。
	recordSeparator byte = 0x1e
)

// MaxRowBytes 是单行 JSONL 的上限（64 MiB）。读取端**必须**按它判超限并显式报错，
// **不得**用默认 64 KiB 缓冲的 `bufio.Scanner` 静默截断。
// MaxRowBytes bounds one JSONL line; readers must reject oversized lines explicitly.
const MaxRowBytes = 64 << 20

// TableDigest 是一张表的规范化摘要：行数、ID 集合摘要与行摘要。
// `Err` 记录编码期拒绝（非法 UTF-8 / NUL / 缺列），序列化时不落盘——摘要为空即视为失败，
// 调用方**不得**把空摘要当成"零行表的合法摘要"。
// TableDigest carries the row count and the two canonical digests; Err records an encoding
// refusal and is never serialized.
type TableDigest struct {
	RowCount    int64  `json:"row_count"`
	IDSetSHA256 string `json:"id_set_sha256"`
	RowSHA256   string `json:"row_sha256"`
	Err         error  `json:"-"`
}

// DigestRows 计算一张表的行数与行摘要：每行按**键字典序**取列、逐值编码、列间 `\x1f`，
// 行尾追加 `\x1e`，全部记录**按字节升序**排序后拼接再算 SHA-256。
// 行集合的先后不影响结果——导出端按 MigrationOrder 读、导入端按批次读，两边的返回顺序
// 本来就可能不同。
// DigestRows hashes a table's rows canonically: columns in key order, records sorted by
// bytes, so neither side's row order can change the digest.
func DigestRows(rows []Row) TableDigest {
	digest := TableDigest{RowCount: int64(len(rows))}
	if len(rows) == 0 {
		// 空表的行摘要取空串的 SHA-256（e3b0c442…），与"未计算"区分开。
		digest.RowSHA256 = sha256Hex(nil)
		return digest
	}

	columns := sortedColumns(rows[0])
	records := make([][]byte, 0, len(rows))
	for i, row := range rows {
		record, err := encodeCanonicalRecord(row, columns)
		if err != nil {
			digest.Err = fmt.Errorf("第 %d 行: %w", i, err)
			return digest
		}
		records = append(records, append(record, recordSeparator))
	}
	digest.RowSHA256 = sha256Hex(joinSortedRecords(records))
	return digest
}

// DigestIDSet 按**声明的主键列序**计算 ID 集合摘要（§2.11.1）：每列编码成
// `类型标签:规范字面量`（NULL 为 `\N`），列间 `\x1f`，行尾 `\x1e`，整行按字节升序排序后
// 拼接再算 SHA-256。复合主键**不得**退化成"把各列拼成一个字符串"。
// DigestIDSet hashes the primary-key set in declared key order; composite keys keep their
// column boundaries.
func DigestIDSet(keyColumns []string, rows []Row) (string, error) {
	if len(keyColumns) == 0 {
		return "", fmt.Errorf("%w: 主键列序为空", ErrInvalidValue)
	}
	records := make([][]byte, 0, len(rows))
	for i, row := range rows {
		record, err := encodeCanonicalRecord(row, keyColumns)
		if err != nil {
			return "", fmt.Errorf("第 %d 行: %w", i, err)
		}
		records = append(records, append(record, recordSeparator))
	}
	return sha256Hex(joinSortedRecords(records)), nil
}

// sortedColumns 取一行的列名并按字节序排序（"键字典序"）。
func sortedColumns(row Row) []string {
	columns := make([]string, 0, len(row))
	for column := range row {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	return columns
}

// joinSortedRecords 按字节升序排序记录后拼接——顺序由内容决定，不由遍历顺序决定。
func joinSortedRecords(records [][]byte) []byte {
	sort.Slice(records, func(i, j int) bool {
		return bytes.Compare(records[i], records[j]) < 0
	})
	total := 0
	for _, record := range records {
		total += len(record)
	}
	out := make([]byte, 0, total)
	for _, record := range records {
		out = append(out, record...)
	}
	return out
}

// sha256Hex 返回小写十六进制摘要。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ErrVerifyFailed 是校验不通过的哨兵：行数、ID 集合或行摘要任一不符。
var ErrVerifyFailed = errors.New("校验不通过")

// ImportReport 是一次导入的对账结果：逐表行数、失败表与总耗时——收口证据直接引用。
// 失败表**必须**留名与原因；"跳过"与"成功"在报告里不得合并。
// ImportReport is the import reconciliation record; failed tables keep their reason.
type ImportReport struct {
	Rows     map[string]int64  `json:"rows"`
	Failures map[string]string `json:"failures"`
	Duration time.Duration     `json:"duration"`
}

// TableCount 返回成功导入的表数。
func (r ImportReport) TableCount() int {
	return len(r.Rows)
}

// TotalRows 返回导入行数合计。
func (r ImportReport) TotalRows() int64 {
	var total int64
	for _, rows := range r.Rows {
		total += rows
	}
	return total
}

// Failed 判断导入是否整体失败（有任一失败表即失败）。
func (r ImportReport) Failed() bool {
	return len(r.Failures) > 0
}

// VerifyReport 是两侧校验的差异表：行数、缺 / 多 ID、行摘要与关键引用是否逐表相符。
//
// 字段语义（`VerifyDump` 与 `VerifyTarget` 共用同一份结构）：
//   - `SourceRows` 是**声明值**（清单记录的行数），`TargetRows` 是**现场读数**（dump 文件
//     或目标库实际读到的行数）——两边同名同义，报告可以直接横着比；
//   - `RowDigestOK` 覆盖每张表的行摘要与主键集合两项（两者共用同一个规范编码器）；
//   - `ReferenceFailures` 记关键引用的逐条失败（§2.15.3 的「关键引用」维度），非空即该表失败；
//   - `Notes` 只记录"这一项为什么无法在本侧比对"（如 jsonb 列的行摘要），**不参与**
//     `OK` 判定，也不得用来掩盖差异。
//
// `MissingIDs` / `ExtraIDs` 保留在结构里供调用方补充逐 ID 明细；当前两处校验各自只持有
// 整侧行集合的**摘要**（清单不存完整 ID 集合），因此不回填这两个字段。
// `OK` 只有在**每一张表**都相符时才为真；缺表按失败处理，不按"未比较"放过。
// VerifyReport records per-table differences; OK requires every table to match, and notes
// are never part of the verdict.
type VerifyReport struct {
	OK                bool                `json:"ok"`
	SourceRows        map[string]int64    `json:"source_rows"`
	TargetRows        map[string]int64    `json:"target_rows"`
	MissingIDs        map[string][]string `json:"missing_ids"`
	ExtraIDs          map[string][]string `json:"extra_ids"`
	RowDigestOK       map[string]bool     `json:"row_digest_ok"`
	ReferenceFailures map[string][]string `json:"reference_failures,omitempty"`
	Notes             map[string]string   `json:"notes,omitempty"`
	Duration          time.Duration       `json:"duration"`
}

// NewVerifyReport 用给定的表清单建一份空白报告（各表先记相符，比较时再改）。
func NewVerifyReport(tables []string) VerifyReport {
	report := VerifyReport{
		SourceRows:        make(map[string]int64, len(tables)),
		TargetRows:        make(map[string]int64, len(tables)),
		MissingIDs:        make(map[string][]string, len(tables)),
		ExtraIDs:          make(map[string][]string, len(tables)),
		RowDigestOK:       make(map[string]bool, len(tables)),
		ReferenceFailures: make(map[string][]string),
		Notes:             make(map[string]string),
	}
	for _, table := range tables {
		report.RowDigestOK[table] = true
	}
	return report
}

// Finalize 汇总 `OK` 判据：行数、缺 / 多 ID、行摘要、关键引用四项都相符才算通过。
// 只要有一张表缺记录（`SourceRows` 或 `TargetRows` 里没有它），就判失败。
// Finalize folds the four criteria into OK; a table missing from either side fails.
func (r VerifyReport) Finalize() VerifyReport {
	ok := true
	for table, sourceRows := range r.SourceRows {
		targetRows, present := r.TargetRows[table]
		if !present || sourceRows != targetRows ||
			len(r.MissingIDs[table]) > 0 || len(r.ExtraIDs[table]) > 0 ||
			!r.RowDigestOK[table] || len(r.ReferenceFailures[table]) > 0 {
			ok = false
			break
		}
	}
	for table := range r.TargetRows {
		if _, present := r.SourceRows[table]; !present {
			ok = false
			break
		}
	}
	// 关键引用失败按表逐个记账，但**不得**因为它挂在一张两侧都没列出的表名下就被忽略：
	// 只要记了非空失败，整体就不通过。
	for _, failures := range r.ReferenceFailures {
		if len(failures) > 0 {
			ok = false
			break
		}
	}
	r.OK = ok
	return r
}

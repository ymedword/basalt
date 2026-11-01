package dbmigrate

// 本文件是**无损编码**的类型与规范字面量构造（C-30 / 颗粒度文档 §2.11.1）：每个源列值按
// 实际存储类打**固定的类型标签**，字面量在构造时定型，不经过 JSON number、`float64` 或
// 驱动的默认序列化——大于 2^53 的整数、BLOB、负零与 NaN 都必须原样过河。
// This file holds the lossless value encoding: every source column value carries a fixed
// type tag and a canonical literal fixed at construction, never routed through JSON
// numbers, float64, or a driver's default mapping.

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValueTag 是 §2.11.1 的类型标签；NULL 不用标签（JSONL 里就是 `null`），
// 因此空标签只表示 NULL，**不得**用它表示空串或空二进制。
// ValueTag is the §2.11.1 tag; NULL carries no tag at all.
type ValueTag string

const (
	// TagInteger 整数 / 布尔列（布尔按 0 / 1 存成整数）。
	TagInteger ValueTag = "i"
	// TagReal 实数，字面量是 IEEE754 位模式的 16 位十六进制。
	TagReal ValueTag = "f"
	// TagText 文本（UTF-8 原字节），含 JSON / NUMERIC 文本列。
	TagText ValueTag = "s"
	// TagBytes 二进制（base64）。
	TagBytes ValueTag = "b"
)

// ErrInvalidValue 是编码期拒绝的哨兵：非法 UTF-8、NUL 文本、未知标签或缺失字面量。
// ErrInvalidValue marks a value the canonical encoder refuses.
var ErrInvalidValue = errors.New("编码值不合法")

// Known 判断标签是否属于 §2.11.1 的四类。空标签**不**算已知——它表示 NULL，不是编码。
func (t ValueTag) Known() bool {
	switch t {
	case TagInteger, TagReal, TagText, TagBytes:
		return true
	default:
		return false
	}
}

// EncodedValue 是一个源列值的无损表示：`Tag` 决定存储类，`Raw` 是规范字面量
// （整数十进制、实数 IEEE754 十六进制、文本 UTF-8 原字节、二进制 base64）。
// NULL 是零值——标签为空且 `Raw` 为 nil。
// EncodedValue is one column value: a tag plus a canonical literal; the zero value is NULL.
type EncodedValue struct {
	Tag ValueTag
	Raw []byte
}

// Row 是一行数据：列名 → 编码值。列集合由导出端按 `columns[]` 定死，
// 导入端逐列比对，**不得**按位置对齐、**不得**补 NULL。
// Row maps column names to encoded values; the column set is fixed by the manifest.
type Row map[string]EncodedValue

// Null 构造 NULL。
func Null() EncodedValue {
	return EncodedValue{}
}

// Int 构造整数；十进制字符串保 int64 全范围，**不**经过浮点。
func Int(v int64) EncodedValue {
	return EncodedValue{Tag: TagInteger, Raw: []byte(strconv.FormatInt(v, 10))}
}

// Real 构造实数；用 `math.Float64bits` 的位模式，保留 NaN / ±Inf / 负零与位精确值。
func Real(v float64) EncodedValue {
	return EncodedValue{Tag: TagReal, Raw: []byte(fmt.Sprintf("%016x", math.Float64bits(v)))}
}

// Text 构造文本。UTF-8 与 NUL 的校验在**编码时**做（见 `encodeCanonicalValue`），
// 这样拒绝信息能带上表 / 列 / 行，而不是在构造点丢掉上下文。
// Text stores the raw UTF-8 bytes; validation happens at encode time so the rejection can
// name table, column and row.
func Text(v string) EncodedValue {
	return EncodedValue{Tag: TagText, Raw: []byte(v)}
}

// Bytes 构造二进制；字面量是 base64，与原字节一一对应。
func Bytes(v []byte) EncodedValue {
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(v)))
	base64.StdEncoding.Encode(encoded, v)
	return EncodedValue{Tag: TagBytes, Raw: encoded}
}

// IsNull 判断是否 NULL。
func (v EncodedValue) IsNull() bool {
	return v.Tag == "" && v.Raw == nil
}

// encodeCanonicalValue 把一个值编码成 `标签:规范字面量`——NULL 编码成 `\N`。
// 校验在这里统一做：未知标签、标签与字面量缺失、文本非 UTF-8 或含 NUL 一律拒绝。
// encodeCanonicalValue renders one value as `tag:literal`, with NULL as `\N`.
func encodeCanonicalValue(v EncodedValue) ([]byte, error) {
	if v.IsNull() {
		return []byte(`\N`), nil
	}
	switch v.Tag {
	case TagInteger, TagReal, TagText, TagBytes:
	default:
		return nil, fmt.Errorf("%w: 未知类型标签 %q", ErrInvalidValue, v.Tag)
	}
	if v.Raw == nil {
		return nil, fmt.Errorf("%w: 标签 %q 缺字面量", ErrInvalidValue, v.Tag)
	}
	if v.Tag == TagText {
		if !utf8.Valid(v.Raw) {
			return nil, fmt.Errorf("%w: 文本不是合法 UTF-8", ErrInvalidValue)
		}
		if idx := indexNUL(v.Raw); idx >= 0 {
			return nil, fmt.Errorf("%w: 文本含 NUL（字节偏移 %d）", ErrInvalidValue, idx)
		}
	}
	out := make([]byte, 0, len(v.Tag)+1+len(v.Raw))
	out = append(out, v.Tag...)
	out = append(out, ':')
	out = append(out, v.Raw...)
	return out, nil
}

// indexNUL 找第一个 0x00 的字节偏移；没有返回 -1。
func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// encodeCanonicalRecord 按给定列序把一行编码成一条记录（列间 `\x1f`，**不**带行尾）。
// 列缺失即拒绝——缺列不得按 NULL 补。
// encodeCanonicalRecord encodes one row in the given column order, joined by 0x1f.
func encodeCanonicalRecord(row Row, columns []string) ([]byte, error) {
	if len(columns) == 0 {
		return nil, fmt.Errorf("%w: 列序为空", ErrInvalidValue)
	}
	parts := make([][]byte, 0, len(columns))
	for _, column := range columns {
		v, ok := row[column]
		if !ok {
			return nil, fmt.Errorf("%w: 缺列 %q", ErrInvalidValue, column)
		}
		encoded, err := encodeCanonicalValue(v)
		if err != nil {
			return nil, fmt.Errorf("列 %q: %w", column, err)
		}
		parts = append(parts, encoded)
	}
	return []byte(strings.Join(byteStrings(parts), string(columnSeparator))), nil
}

// byteStrings 把 [][]byte 转成 []string，供 `strings.Join` 使用
// （`bytes.Join` 的语义相同，但这里列数很少，转换更直白）。
func byteStrings(parts [][]byte) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

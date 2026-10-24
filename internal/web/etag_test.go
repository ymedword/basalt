package web

// 本文件是条件写 ETag 判据的判卷（颗粒度文档 §3.2.1 etag_test.go，check
// `012-web-etag-routes`）：只认逐字强 ETag；`W/`、`*`、逗号列表、裸摘要、
// 大小写与长度偏差一律非法；渲染与解析共用同一形状。
// These tests pin the single If-Match rule: exact strong ETag only.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// validDigest 是判卷用的 64 位小写十六进制摘要。
const validDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestIfMatch_AcceptsStrongETagLiteral：合法强 ETag 原样返回（不做归一化）。
// TestIfMatch_AcceptsStrongETagLiteral pins the accepted shape.
func TestIfMatch_AcceptsStrongETagLiteral(t *testing.T) {
	raw := formatETag(validDigest)
	require.Equal(t, `"sha256:`+validDigest+`"`, raw)

	got, ok := parseIfMatch(raw)
	require.True(t, ok)
	require.Equal(t, raw, got)
}

// TestIfMatch_RejectsWeakAndWildcard：弱 ETag、通配 `*`、缺失与空串全部拒绝。
// TestIfMatch_RejectsWeakAndWildcard covers the four non-entity forms.
func TestIfMatch_RejectsWeakAndWildcard(t *testing.T) {
	strong := formatETag(validDigest)
	for name, raw := range map[string]string{
		"wildcard": "*",
		"weak":     "W/" + strong,
		"missing":  "",
		"blank":    " ",
	} {
		_, ok := parseIfMatch(raw)
		require.False(t, ok, name)
	}
}

// TestIfMatch_RejectsCommaList：多值列表（含首项合法）整体拒绝——条件写只接受
// 恰好一个实体标签，不解析列表语义。
// TestIfMatch_RejectsCommaList refuses any list form, even with a valid first item.
func TestIfMatch_RejectsCommaList(t *testing.T) {
	strong := formatETag(validDigest)
	other := formatETag(strings.Repeat("b", 64))
	for name, raw := range map[string]string{
		"two_values":      strong + ", " + other,
		"valid_then_junk": strong + ", *",
		"only_comma":      ",",
	} {
		_, ok := parseIfMatch(raw)
		require.False(t, ok, name)
	}
}

// TestIfMatch_RejectsBareDigest：无前缀 / 无引号的裸摘要不合法（ETag 是字面量，
// 不是哈希本身）。
// TestIfMatch_RejectsBareDigest requires the literal wrapper.
func TestIfMatch_RejectsBareDigest(t *testing.T) {
	for name, raw := range map[string]string{
		"bare_hex":       validDigest,
		"sha256_prefix":  "sha256:" + validDigest,
		"quotes_only":    `"` + validDigest + `"`,
		"wrong_scheme":   `"md5:` + validDigest + `"`,
		"double_quoted":  `""sha256:` + validDigest + `""`,
		"prefix_no_hash": `"sha256:"`,
	} {
		_, ok := parseIfMatch(raw)
		require.False(t, ok, name)
	}
}

// TestIfMatch_RejectsUppercaseHex：大写十六进制拒绝——摘要按逐字小写比较，
// 大小写归一化会让两个不同字节串通过同一个判据。
// TestIfMatch_RejectsUppercaseHex keeps the digest strictly lowercase.
func TestIfMatch_RejectsUppercaseHex(t *testing.T) {
	upper := strings.ToUpper(validDigest)
	_, ok := parseIfMatch(formatETag(upper))
	require.False(t, ok, "uppercase digest must be rejected verbatim")
}

// TestIfMatch_RejectsLengthDrift：长度偏差（少一位 / 多一位）拒绝。
// TestIfMatch_RejectsLengthDrift rejects any digest length other than 64.
func TestIfMatch_RejectsLengthDrift(t *testing.T) {
	for name, digest := range map[string]string{
		"63": validDigest[:63],
		"65": validDigest + "0",
		"32": validDigest[:32],
	} {
		_, ok := parseIfMatch(formatETag(digest))
		require.False(t, ok, name)
	}
}

// TestIfMatch_RejectsSurroundingBytes：前后空白、引号外追加字节一律拒绝。
// TestIfMatch_RejectsSurroundingBytes rejects leading/trailing bytes.
func TestIfMatch_RejectsSurroundingBytes(t *testing.T) {
	strong := formatETag(validDigest)
	for name, raw := range map[string]string{
		"leading_space":  " " + strong,
		"trailing_space": strong + " ",
		"trailing_junk":  strong + "x",
		"trailing_crlf":  strong + "\r\n",
	} {
		_, ok := parseIfMatch(raw)
		require.False(t, ok, name)
	}
}

// TestIfMatch_RejectsNonHexDigest：64 位但含非十六进制字符（g–z、符号）拒绝。
// TestIfMatch_RejectsNonHexDigest rejects non-hex bytes inside a 64-byte window.
func TestIfMatch_RejectsNonHexDigest(t *testing.T) {
	for name, digest := range map[string]string{
		"letter_g": strings.Repeat("g", 64),
		"dash":     strings.Repeat("-", 64),
		"space":    strings.Repeat(" ", 64),
		"mixed":    validDigest[:63] + "G",
	} {
		_, ok := parseIfMatch(formatETag(digest))
		require.False(t, ok, name)
	}
}

// TestIfMatch_SingleAndSetShareGrammar：单文件 ETag 与受管文件集合 ETag 用同一
// 语法（同一解析器接受两者）；"不可互换"由来源绑定保证——解析层无法也不应区分
// 两者，调用方必须与产生该 ETag 的同一读取路径比较。
// TestIfMatch_SingleAndSetShareGrammar pins that both ETag families parse identically;
// interchangeability is prevented by source binding, not by the grammar.
func TestIfMatch_SingleAndSetShareGrammar(t *testing.T) {
	contentETag := formatETag(validDigest)
	setETag := formatETag(strings.Repeat("f", 64))

	for name, raw := range map[string]string{"content": contentETag, "set": setETag} {
		got, ok := parseIfMatch(raw)
		require.True(t, ok, name)
		require.Equal(t, raw, got, name)
	}

	// 语法相同、值不同：把集合 ETag 交给单文件条件写时必须由适配器判不匹配，
	// 解析层只负责"形状合法"。这里用字符串比较钉住两者不相等这一事实。
	require.NotEqual(t, contentETag, setETag)
}

package cli_test

// 本文件是 `author:` 段解析的验收测试（颗粒度文档 §3.1.9 / §3.2.1：六例——缺段、
// 缺键、temperature 缺省与范围、未知键、类型错误）。命名族固定为
// `TestParseAuthorSection_*`（§4.4 的 `012-cli-author` 按族展开精确函数名）。
// This file pins the author-section parser: absent section, missing keys, temperature
// default and range, unknown keys and type errors. Names follow the fixed family
// TestParseAuthorSection_*.

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
)

// TestParseAuthorSection_Defaults：缺段（nil）→ 零值，作者功能关闭；有段但省略
// temperature → 缺省 0.2（缺省只作用于"段存在"这一条路径）。
// TestParseAuthorSection_Defaults pins the zero value for an absent section and the
// 0.2 default when the section exists but omits temperature.
func TestParseAuthorSection_Defaults(t *testing.T) {
	t.Parallel()

	absent, err := cli.ParseAuthorSection(nil)
	require.NoError(t, err, "缺段是合法配置：作者功能关闭")
	require.Equal(t, core.AuthorConfig{}, absent, "缺段必须返回零值，而不是填充缺省温度")

	present, err := cli.ParseAuthorSection(map[string]any{
		"provider": "main",
		"model":    "author-model",
	})
	require.NoError(t, err)
	require.Equal(t, core.AuthorConfig{
		Provider:    "main",
		Model:       "author-model",
		Temperature: 0.2,
	}, present, "省略 temperature 时必须取缺省 0.2")
}

// TestParseAuthorSection_MissingKeysDoNotFail：有段但缺 provider / model 不报错
// （字段留空）——启动不失败，端点调用时才 503；空映射等价于两个键都缺。
// TestParseAuthorSection_MissingKeysDoNotFail keeps startup alive when either key is
// omitted; the endpoint reports 503 later.
func TestParseAuthorSection_MissingKeysDoNotFail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  map[string]any
		want core.AuthorConfig
	}{
		{
			name: "只有 provider",
			raw:  map[string]any{"provider": "main"},
			want: core.AuthorConfig{Provider: "main", Temperature: 0.2},
		},
		{
			name: "只有 model",
			raw:  map[string]any{"model": "author-model"},
			want: core.AuthorConfig{Model: "author-model", Temperature: 0.2},
		},
		{
			name: "空映射",
			raw:  map[string]any{},
			want: core.AuthorConfig{Temperature: 0.2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := cli.ParseAuthorSection(tc.raw)
			require.NoError(t, err, "缺键不得阻断启动")
			require.Equal(t, tc.want, got)
		})
	}
}

// TestParseAuthorSection_ParsesAllKeys：三个键都给出时逐字段取值，temperature 是
// 显式配置值而不是缺省值。
// TestParseAuthorSection_ParsesAllKeys pins the explicit temperature override.
func TestParseAuthorSection_ParsesAllKeys(t *testing.T) {
	t.Parallel()

	got, err := cli.ParseAuthorSection(map[string]any{
		"provider":    "main",
		"model":       "author-model",
		"temperature": 0.7,
	})
	require.NoError(t, err)
	require.Equal(t, core.AuthorConfig{
		Provider:    "main",
		Model:       "author-model",
		Temperature: 0.7,
	}, got)
}

// TestParseAuthorSection_TemperatureBounds：闭区间 `[0,2]`——两端与整数形态都合法；
// 越界、NaN 与非数值一律拒绝，文案点名键与区间。
// TestParseAuthorSection_TemperatureBounds pins the closed interval [0,2], including
// integer forms, and refuses out-of-range, NaN and non-numeric values.
func TestParseAuthorSection_TemperatureBounds(t *testing.T) {
	t.Parallel()

	valid := []struct {
		name  string
		value any
		want  float64
	}{
		{name: "下界 0", value: 0.0, want: 0},
		{name: "上界 2", value: 2.0, want: 2},
		{name: "区间内", value: 1.5, want: 1.5},
		{name: "整数 0", value: int64(0), want: 0},
		{name: "整数 1", value: int64(1), want: 1},
		{name: "无符号整数 2", value: uint64(2), want: 2},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := cli.ParseAuthorSection(map[string]any{"temperature": tc.value})
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Temperature)
		})
	}

	invalid := []struct {
		name  string
		value any
	}{
		{name: "低于下界", value: -0.1},
		{name: "高于上界", value: 2.1},
		{name: "负整数", value: int64(-1)},
		{name: "大无符号整数", value: uint64(3)},
		{name: "NaN", value: math.NaN()},
		{name: "正无穷", value: math.Inf(1)},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := cli.ParseAuthorSection(map[string]any{"temperature": tc.value})
			require.Error(t, err, "temperature=%v 必须拒绝", tc.value)
			require.Contains(t, err.Error(), "author.temperature")
			require.Contains(t, err.Error(), "[0,2]")
		})
	}
}

// TestParseAuthorSection_RejectsUnknownKeys：未知键报错并点名该键——配置拼写错误
// 不得静默忽略；多个未知键时按字典序报第一个（错误文案跨运行确定）。
// TestParseAuthorSection_RejectsUnknownKeys names the offending key and reports the
// lexicographically first unknown key deterministically.
func TestParseAuthorSection_RejectsUnknownKeys(t *testing.T) {
	t.Parallel()

	_, err := cli.ParseAuthorSection(map[string]any{
		"provider": "main",
		"modle":    "typo",
	})
	require.Error(t, err, "拼写错误的键必须拒绝，而不是被静默忽略")
	require.Contains(t, err.Error(), "未知键")
	require.Contains(t, err.Error(), `"modle"`)
	require.Contains(t, err.Error(), "provider / model / temperature")

	_, err = cli.ParseAuthorSection(map[string]any{
		"zzz": "1",
		"aaa": "2",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), `"aaa"`, "多个未知键按字典序报第一个，文案跨运行确定")
}

// TestParseAuthorSection_RejectsTypeErrors：provider / model 必须是字符串，
// temperature 必须是数字（字符串 "0.2" 不代转）——类型错误点名键路径。
// TestParseAuthorSection_RejectsTypeErrors pins string/number types and names the key
// path; a quoted number is not coerced.
func TestParseAuthorSection_RejectsTypeErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  map[string]any
		key  string
	}{
		{name: "provider 是数字", raw: map[string]any{"provider": 123}, key: "author.provider"},
		{name: "model 是列表", raw: map[string]any{"model": []any{"m"}}, key: "author.model"},
		{name: "temperature 是字符串", raw: map[string]any{"temperature": "0.2"}, key: "author.temperature"},
		{name: "temperature 是布尔", raw: map[string]any{"temperature": true}, key: "author.temperature"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := cli.ParseAuthorSection(tc.raw)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.key)
		})
	}
}

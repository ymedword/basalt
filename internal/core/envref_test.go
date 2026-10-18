package core_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestEnvPlaceholderName_ExactFormTable 钉住 `${VAR}` 的唯一判定口径：整串必须是
// `${` + 标识符 + `}`，标识符以字母或下划线开头、其后可带数字。任何半截引用
// （`${A}${B}` / `${A}x` / `x${A}`）与近形写法（`$A` / `${}` / `${1A}` / `${A-B}`）
// 一律拒绝——放行它们等于把"凭证只能来自环境"的门禁退化成前缀匹配。
// The table pins the single exact-form rule for ${VAR}; every near-miss is rejected so
// the credential red line cannot degrade into prefix matching.
func TestEnvPlaceholderName_ExactFormTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input    string
		wantName string
		wantOK   bool
	}{
		// 接受档：精确整串引用。
		{"${A}", "A", true},
		{"${_A}", "_A", true},
		{"${A_1}", "A_1", true},
		{"${aBc}", "aBc", true},
		{"${BASALT_NOTIFY_OPS}", "BASALT_NOTIFY_OPS", true},

		// 拒绝档：非合法标识符或非整串。
		{"$A", "", false},
		{"${}", "", false},
		{"${1A}", "", false},
		{"${A}${B}", "", false},
		{"${A}x", "", false},
		{"x${A}", "", false},
		{"${A-B}", "", false},
		{" ${A}", "", false},
		{"", "", false},
	}

	for _, tc := range cases {
		name, ok := core.EnvPlaceholderName(tc.input)
		require.Equal(t, tc.wantOK, ok, "input=%q ok", tc.input)
		require.Equal(t, tc.wantName, name, "input=%q name", tc.input)
	}
}

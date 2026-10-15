package storage

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDSN_EncodesURIReservedCharacters：`%` / `#` / `?` 必须转义（`%` 先转，其
// 替换产物不得被二次转义）；空格、反斜杠、盘符冒号按字面保留——SQLite 的 URI
// 解析只对前三个字符赋予特殊含义，其余按字节取用（Windows 路径现状即依赖此）。
// URI-reserved characters are escaped (percent first, single pass); everything
// else passes through verbatim.
//
// 为什么较真：`#` 不转义时路径被截断到 fragment 之前，库静默落到兄弟路径上
// （TestBootstrap_PathWithURIReservedCharacters 抓端到端形态，本用例钉住字符串面）。
func TestDSN_EncodesURIReservedCharacters(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string // dsn 的前缀（参数部分另断）
	}{
		{"井号", `C:\ws\hash#dir\basalt.db`, `file:C:\ws\hash%23dir\basalt.db?`},
		{"百分号", `C:\ws\pct%dir\basalt.db`, `file:C:\ws\pct%25dir\basalt.db?`},
		{"问号", `C:\ws\q?dir\basalt.db`, `file:C:\ws\q%3Fdir\basalt.db?`},
		{"% 与 # 同现：不得二次转义", `C:\ws\a%b#c\basalt.db`, `file:C:\ws\a%25b%23c\basalt.db?`},
		{"空格与反斜杠按字面保留", `C:\ws\space dir\basalt.db`, `file:C:\ws\space dir\basalt.db?`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dsn(tc.path, false)
			require.True(t, strings.HasPrefix(got, tc.want), "dsn(%q) = %q；期望前缀 %q", tc.path, got, tc.want)
			require.Contains(t, got, "_pragma=busy_timeout(", "逐连接 PRAGMA 参数必须随 DSN 装配")
		})
	}
}

// TestDSN_ReadOnlyMode：只读打开在编码后的路径上追加 mode=ro，其余同规。
func TestDSN_ReadOnlyMode(t *testing.T) {
	got := dsn(`C:\ws\hash#dir\basalt.db`, true)
	require.True(t, strings.HasPrefix(got, `file:C:\ws\hash%23dir\basalt.db?mode=ro&`), "dsn = %q", got)
}

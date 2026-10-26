//go:build windows

package app

// 本文件是实例私有目录权限断言的 Windows 分支（14D T092 / A12）：
// 读回目录 DACL，只允许当前用户、SYSTEM 与 Administrators。
// The Windows branch asserts the directory DACL through checkTightDACL.

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireDirectoryRestricted 断言目录存在且 DACL 只允许三个受托人。
// requireDirectoryRestricted asserts the directory DACL allows only the three trustees.
func requireDirectoryRestricted(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Lstat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	allowed, err := allowedKeyTrustees()
	require.NoError(t, err)
	require.NoError(t, checkTightDACL(dir, "实例私有目录", allowed))
}

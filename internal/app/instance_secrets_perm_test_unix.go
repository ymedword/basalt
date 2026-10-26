//go:build !windows

package app

// 本文件是实例私有目录权限断言的 Unix 分支（14D T092 / A12）：0700 + 属主为当前用户。
// The Unix branch asserts the directory mode is 0700 and owned by the current user.

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireDirectoryRestricted 断言目录存在、权限 0700 且属主是当前进程用户。
// requireDirectoryRestricted asserts a 0700 directory owned by the current user.
func requireDirectoryRestricted(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Lstat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Zero(t, info.Mode().Perm()&0o077, "私有目录不得对组 / 其他用户开放")
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Equal(t, uint32(os.Getuid()), stat.Uid)
}

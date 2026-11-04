//go:build !windows

package app

// 本文件是主密钥测试夹具的 Unix 半边：显式收口目录 0700 / 文件 0600。
// testing.T.TempDir 的编号子目录由 os.Mkdir(..., 0o777) 创建（再受 umask 影响），
// 通常为 0755，不能直接通过 0700/0600 判卷；夹具必须像 Windows ACL 分支一样
// 主动收紧权限，否则生产校验会（正确地）拒绝测试凭据。
// Unix half of the master-key test fixture: explicitly tighten the mode bits.
// testing.T.TempDir creates its numbered subdirectory with os.Mkdir(..., 0o777),
// which is typically 0755 under the default umask and would (correctly) fail the
// 0700/0600 check; the fixture must tighten it like the Windows ACL branch.

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// tightenMasterKeyACLForTest 把目录收成 0700、文件收成 0600；签名与 Windows 分支保持一致。
// tightenMasterKeyACLForTest sets 0700 for directories and 0600 for files.
func tightenMasterKeyACLForTest(t *testing.T, path string, isDir bool) {
	t.Helper()
	mode := os.FileMode(0o600)
	if isDir {
		mode = 0o700
	}
	require.NoError(t, os.Chmod(path, mode))
}

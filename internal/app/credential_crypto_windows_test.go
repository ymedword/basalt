//go:build windows

package app

// 本文件判卷 Windows 分支的 DACL 校验（复核 U18）：正确 ACL 允许使用；未授权用户 / 组
// 获得读或写权限即拒绝；无法读取安全描述符即拒绝；使用前权限被放宽，下一次读取必须发现。
// These tests pin the Windows DACL check: a tight ACL loads, an untrusted trustee with
// read or write is refused, an unreadable descriptor is refused, and an ACL loosened
// after a successful load is caught by the next read.

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestCredentialCrypto_WindowsDACL 覆盖文件与父目录两处判卷。
// TestCredentialCrypto_WindowsDACL covers both the file and its parent directory.
func TestCredentialCrypto_WindowsDACL(t *testing.T) {
	key := testMasterKey()
	t.Cleanup(func() { key.Zero() })

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, ".basalt"), 0o700))

	t.Run("tight_acl_accepted", func(t *testing.T) {
		path := writeMasterKeyFile(t, filepath.Join(root, "tight"), "k_tight", key.Key)
		loaded, err := LoadMasterKey(path, workspace)
		require.NoError(t, err, "当前用户 + SYSTEM + Administrators 的 ACL 必须放行")
		require.Equal(t, "k_tight", loaded.KeyID)
		require.Equal(t, key.Key, loaded.Key)
	})

	t.Run("untrusted_user_read_on_file_rejected", func(t *testing.T) {
		path := writeMasterKeyFile(t, filepath.Join(root, "loose-file"), "k_loose", key.Key)
		grantMasterKeyACLForTest(t, path, testEveryoneSID, "R")
		loaded, err := LoadMasterKey(path, workspace)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "DACL")
		require.Contains(t, err.Error(), "未授权主体")
		require.Empty(t, loaded.Key)
	})

	t.Run("untrusted_group_write_on_dir_rejected", func(t *testing.T) {
		dir := filepath.Join(root, "loose-dir")
		path := writeMasterKeyFile(t, dir, "k_dir", key.Key)
		// 目录写权限 = 可以删除 / 替换密钥文件，只收紧文件本身不够。
		grantMasterKeyACLForTest(t, dir, testBuiltinUsersSID, "(OI)(CI)W")
		loaded, err := LoadMasterKey(path, workspace)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "主密钥目录")
		require.Contains(t, err.Error(), "未授权主体")
		require.Empty(t, loaded.Key)
	})

	t.Run("inherited_loose_ace_rejected", func(t *testing.T) {
		// 显式的 Everyone 读授权经继承落到子目录与密钥文件上——这正是「测试目录继承了
		// 宽松权限」的场景：登录用户之外的主体能读密钥，必须拒绝。
		parent := filepath.Join(root, "loose-parent")
		require.NoError(t, os.MkdirAll(parent, 0o700))
		tightenMasterKeyACLForTest(t, parent, true)
		grantMasterKeyACLForTest(t, parent, testEveryoneSID, "(OI)(CI)R")
		child := filepath.Join(parent, "secrets")
		require.NoError(t, os.MkdirAll(child, 0o700))
		path := filepath.Join(child, "master.key")
		payload := `{"version":1,"key_id":"k_inherit","key":"` +
			base64.StdEncoding.EncodeToString(key.Key) + `"}`
		require.NoError(t, os.WriteFile(path, []byte(payload), 0o600))

		loaded, err := LoadMasterKey(path, workspace)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "未授权主体")
		require.Empty(t, loaded.Key)
	})

	t.Run("loosened_before_use_rejected", func(t *testing.T) {
		path := writeMasterKeyFile(t, filepath.Join(root, "recheck"), "k_recheck", key.Key)
		loaded, err := LoadMasterKey(path, workspace)
		require.NoError(t, err, "第一次读取时 ACL 仍是最小权限")

		grantMasterKeyACLForTest(t, path, testEveryoneSID, "R")
		loaded, err = LoadMasterKey(path, workspace)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable,
			"权限在使用前被放宽，下一次读取必须发现")
		require.Contains(t, err.Error(), "未授权主体")
		require.Empty(t, loaded.Key)
	})

	t.Run("unreadable_descriptor_rejected", func(t *testing.T) {
		missingDir := filepath.Join(root, "absent")
		missingFile := filepath.Join(missingDir, "master.key")
		err := checkKeyPermissions(missingDir, missingFile)
		require.ErrorIs(t, err, core.ErrCredentialUnavailable)
		require.Contains(t, err.Error(), "安全描述符")
	})
}

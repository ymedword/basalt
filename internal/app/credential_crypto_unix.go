//go:build !windows

package app

// 本文件是主密钥平台校验的 Unix 分支（模块卡 R4.8 / T129）：硬链接数取自 inode，
// 权限按 0700 / 0600 判卷。Windows 分支见 credential_crypto_windows.go。
// This file is the Unix branch of master-key file validation.

import (
	"fmt"
	"os"
	"syscall"

	"github.com/ymedword/basalt/internal/core"
)

// hardLinkCount 读 inode 的硬链接数：同一份密钥材料挂在不同路径上就是多一条泄露面。
// hardLinkCount reads the inode link count; a second path is a second leak surface.
func hardLinkCount(_ string, info os.FileInfo) (uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(stat.Nlink), true
}

// checkKeyPermissions 按目录 0700 / 文件 0600 判卷：组与其他位必须为零。
// checkKeyPermissions requires empty group and other bits on both the file and its dir.
func checkKeyPermissions(dirPath, filePath string) error {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("%w: 读取主密钥文件状态失败", core.ErrCredentialUnavailable)
	}
	dirInfo, err := os.Stat(dirPath)
	if err != nil {
		return fmt.Errorf("%w: 读取主密钥目录状态失败", core.ErrCredentialUnavailable)
	}
	if mode := fileInfo.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%w: 主密钥文件权限为 %#o，必须为 0600", core.ErrCredentialUnavailable, mode)
	}
	if mode := dirInfo.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%w: 主密钥目录权限为 %#o，必须为 0700", core.ErrCredentialUnavailable, mode)
	}
	return nil
}

//go:build !windows

package app

// 本文件是实例私有目录的平台准备（14D T088）与硬链接退化判定（T086）的 Unix 分支。
// 顺序固定为 MkdirAll → Chmod 0700 → stat 读回：**权限校验通过之前不创建任何文件**
// （临时文件也不建），不退回"先建文件再补权限"。Windows 分支见
// instance_secrets_perm_windows.go。
// This file prepares the instance-private directory on Unix: mkdir, chmod, then a
// read-back stat before anything else is created.

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/ymedword/basalt/internal/core"
)

// prepareSecretDirectory 建目录并把权限收口到 0700，随后读回校验属主与权限位。
// 目录已存在时同样收口并校验：实例私有目录不是"谁建的谁负责"，它承载主密钥。
// prepareSecretDirectory creates chmods and re-reads the directory before any file.
func prepareSecretDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: 创建实例私有目录失败", core.ErrCredentialUnavailable)
	}
	// #nosec G302 -- 目标是**目录**：0700 已是目录的最小可用权限，G302 按文件 0600 判、不适用。
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("%w: 设置实例私有目录权限失败", core.ErrCredentialUnavailable)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w: 读取实例私有目录状态失败", core.ErrCredentialUnavailable)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: 实例私有目录路径不是目录", core.ErrCredentialUnavailable)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: 实例私有目录权限为 %04o，必须为 0700",
			core.ErrCredentialUnavailable, perm)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: 无法读取实例私有目录的属主", core.ErrCredentialUnavailable)
	}
	if int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%w: 实例私有目录属主不是当前用户（uid %d，期望 %d）",
			core.ErrCredentialUnavailable, stat.Uid, os.Getuid())
	}
	return nil
}

// isHardLinkUnsupported 判断 `os.Link` 的失败是否属于"该文件系统不支持硬链接"
// （FAT / exFAT、部分 FUSE）。此时退化为 `O_EXCL` 直写；硬链接的原子性没了，
// 但不覆盖语义仍然成立。权限错误同样归入退化路径：退化写入会立刻以同样的错误失败。
// isHardLinkUnsupported routes filesystems without hard links to the O_EXCL fallback.
func isHardLinkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.EXDEV)
}

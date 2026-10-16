//go:build windows

package workspace

// 本文件是实例锁的 Windows 落位：LockFileEx 排他 + 非阻塞。syscall 包**没有**
// LockFileEx 符号——取 golang.org/x/sys/windows（§2.3 流程 D；US-1 §6 读数）。
// 锁区 = offset 0 起的 1 字节（区段锁）；诊断区在锁区之外，另一句柄照常可读。
// This file binds the instance lock to LockFileEx on Windows.

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const (
	// lockRegionOffset / lockRegionLength 是锁区：offset 0 起 1 字节——区段越窄，
	// 对同一文件其他区域的读写干扰越小（诊断区就落在锁区之外）。
	lockRegionOffset = 0
	lockRegionLength = 1
)

// lockHandle 排他非阻塞锁住 [0,1)：拿不到立刻返回错误（LOCKFILE_FAIL_IMMEDIATELY），
// 绝不阻塞等待——写模式命令要的是"现在能不能跑"，不是"排队等"。
// lockHandle takes an exclusive, non-blocking lock on the first byte.
func lockHandle(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, lockRegionLength, 0, &windows.Overlapped{Offset: lockRegionOffset})
}

// unlockHandle 解锁同一区段；句柄关闭时内核也会释放，本函数是显式的提前释放路径。
// unlockHandle releases the same region.
func unlockHandle(file *os.File) error {
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, lockRegionLength, 0,
		&windows.Overlapped{Offset: lockRegionOffset})
}

// isLockContended 判"被别的句柄持有"（ERROR_LOCK_VIOLATION = 33）；其余错误（磁盘故障 /
// 句柄无效）不是竞争，必须原样上抛。
// isLockContended reports whether the failure means "held by another handle".
func isLockContended(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

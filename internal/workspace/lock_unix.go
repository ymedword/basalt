//go:build unix

package workspace

// 本文件是实例锁的类 Unix 落位：flock(LOCK_EX|LOCK_NB)。flock 是**全文件**锁——与
// Windows 的 [0,1) 区段锁在可观测语义上等价：两者都只挡住"同一把锁的竞争者"，不挡普通
// read()，故锁定区之外的诊断记录在两端都读得到（§2.3 流程 D）。CI 只在 ubuntu-latest 上
// **运行**本分支；Windows 分支的运行读数由本机门禁登记（§3.3 / §4.1）。
// This file binds the instance lock to flock on Unix-like systems.

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockHandle 排他非阻塞取锁：拿不到返回 EWOULDBLOCK，绝不阻塞等待——写模式命令要的是
// "现在能不能跑"，不是"排队等"。
// lockHandle takes an exclusive, non-blocking flock.
func lockHandle(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

// unlockHandle 解锁；句柄关闭时内核也会释放，本函数是显式的提前释放路径。
// unlockHandle releases the flock.
func unlockHandle(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

// isLockContended 判"被别的句柄持有"（EWOULDBLOCK 与 EAGAIN 是同一 errno 的两个名字，
// 两个都判以覆盖平台差异）；其余错误（如 EBADF）不是竞争，必须原样上抛。
// isLockContended reports whether the failure means "held by another handle".
func isLockContended(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

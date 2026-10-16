//go:build darwin

package channel

import (
	"os"

	"golang.org/x/sys/unix"
)

// 本文件是渠道层的 Darwin 半边。**独立成文件而不是与 Linux 合并**：两边取终端属性的
// 请求码不同——Darwin 是 `TIOCGETA`（0x40487413），Linux 是 `TCGETS`（0x5401）——
// 合并写一个常量会让另一侧的 `IoctlGetTermios` 恒失败，"是不是终端"于是永远答"不是"，
// 而这个错误只在真终端上才看得出来（管道侧一切正常）。
// The Darwin half is a separate file on purpose: the terminal-attribute request code
// differs from Linux's, and a shared constant would make the query always fail here.
//
// H4 复核（2026-10-16，`golang.org/x/sys@v0.48.0` 于模块缓存）：`unix.IoctlGetTermios`
// 在 darwin/linux 上取 `(fd int, req uint)` 形参；`TIOCGETA = 0x40487413`（darwin/amd64）
// 与 `TCGETS = 0x5401`（linux/amd64）就地读到。
// Verified in the module cache: the ioctl signature and both platform constants.

// isTerminal 判定文件句柄是不是真终端：Darwin 的终端属性请求是 `TIOCGETA`。
// isTerminal queries the Darwin terminal attributes request TIOCGETA.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}

// newConsole 造控制台读写面（真终端与管道共用字节流实现，差别只在行尾）。
// newConsole builds the console surface; both branches share the byte-stream path.
func newConsole(in, out *os.File) console {
	return newFileConsole(in, out, isTerminal(in))
}

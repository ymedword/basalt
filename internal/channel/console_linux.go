//go:build linux

package channel

import (
	"os"

	"golang.org/x/sys/unix"
)

// 本文件是渠道层的 Linux 半边。与 Windows 的差别在**协议**上而不在编码上：类 Unix
// 的终端本来就是 UTF-8 字节流，真终端与管道的读写路径是同一条，所以这里没有两条
// 实现；分流判据仍然要问内核（`TCGETS`），因为"是不是终端"决定行尾语义（见
// `newFileConsole`），而且 FR-040 要求这个判定来自终端属性查询而非环境变量。
// The Linux half: a Unix terminal is already a UTF-8 byte stream, so both branches
// share one implementation; the kernel query still decides the line-ending semantics.

// isTerminal 判定文件句柄是不是真终端：Linux 的终端属性请求是 `TCGETS`。
// isTerminal queries the Linux terminal attributes request TCGETS.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// newConsole 造控制台读写面（真终端与管道共用字节流实现，差别只在行尾）。
// newConsole builds the console surface; both branches share the byte-stream path.
func newConsole(in, out *os.File) console {
	return newFileConsole(in, out, isTerminal(in))
}

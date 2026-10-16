//go:build windows

package channel

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// 本文件是渠道层的 Windows 半边：**真控制台**与**管道 / 重定向**走两条不同的
// 读写路径，分流判据由内核回答（`GetConsoleMode` 成功即控制台），**不**看任何环境
// 变量（FR-040）。
// The Windows half: a real console goes through the console APIs while pipes and
// redirects stay byte streams, and the split is decided by the kernel, not by env vars.
//
// 分流是**逐句柄**做的：`chat > out.txt` 时输入仍是控制台、输出是文件，两侧各走各的
// 路径——按"整体是不是终端"一刀切会让这种组合下的输出经控制台 API 写进被重定向的
// 句柄，直接失败。
// The decision is per handle: input and output can be redirected independently.
//
// H3 复核（2026-10-16，`golang.org/x/sys@v0.48.0` 于模块缓存）：用到的四个函数
// `GetConsoleMode` / `ReadConsole` / `WriteConsole` / `UTF16ToString` 均在位，且
// `ReadConsole` / `WriteConsole` 的实现体调用的是 `ReadConsoleW` / `WriteConsoleW`
// 入口（`zsyscall_windows.go` 的 `procReadConsoleW` / `procWriteConsoleW`）。
// 同批复核在位的还有 `GetStdHandle` / `SetConsoleMode` / `GetConsoleOutputCP` /
// `SetConsoleOutputCP` 四个——**本文件不用它们**：句柄已由注入的 `*os.File` 给出
// （不必再向进程要标准句柄）；W 路径不经过代码页（这正是它不乱码的原因），因此输出
// 代码页无需读写；输入模式（`ENABLE_PROCESSED_INPUT` 等）保持控制台缺省——读出
// 循环对"一次只回来半行"本就鲁棒，而缺省模式下 Ctrl-C 才会变成信号而不是字符。
// Four more functions were verified present as well but are deliberately unused; the
// reasons are recorded above so a later reader does not re-derive them.

// consoleChunk 是单次控制台读写调用的 UTF-16 单元宽度。
// consoleChunk is the per-call UTF-16 width of console reads and writes.
const consoleChunk = 4096

// isTerminal 判定文件句柄是不是真控制台。
// isTerminal asks the kernel whether the handle is a real console.
//
// 实现就是向内核要控制台模式：拿得到即控制台，拿不到（管道的
// `ERROR_INVALID_HANDLE`）即不是——环境变量在这条路径上没有任何参与。
// The whole implementation is one kernel query; environment variables play no part.
func isTerminal(f *os.File) bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

// newConsole 逐句柄分流后造出控制台读写面。
// newConsole picks the path per handle.
func newConsole(in, out *os.File) console {
	c := newFileConsole(in, out, false)
	if isTerminal(in) {
		c.reader = &consoleReader{h: windows.Handle(in.Fd())}
	}
	if isTerminal(out) {
		c.writer = &consoleWriter{h: windows.Handle(out.Fd())}
	}
	return c
}

// consoleReader 经控制台 API 读入：`ReadConsoleW` 收 UTF-16，再转回 Go 字符串——
// 中文是否乱码与代码页无关，因为这条路径根本不经过代码页。
// consoleReader reads through the console API; UTF-16 in, Go string out.
type consoleReader struct {
	h windows.Handle
}

// readLine 读一行（控制台行输入模式下 `ReadConsole` 会一直等到回车，但这里仍按
// "攒够一行才返回"写：模式被外部改过时也只是多循环几次，不会把半行当整行）。
// readLine returns one line, accumulating across calls if the console hands back less.
func (r *consoleReader) readLine() (string, error) {
	buf := make([]uint16, consoleChunk)
	var units []uint16
	for {
		var read uint32
		// inputControl 只对"控制台输入缓冲"句柄有意义；控制台句柄传 nil。
		// 要读的宽度直接写**未类型常量 consoleChunk**（buf 就是按它开的）：它是编译期常量，
		// 隐式转 uint32 不可能溢出，无需 `uint32(len(buf))` 这类运行期窄化转换。
		// The control parameter only matters for input-buffer handles; nil here. The count
		// is the untyped constant the buffer was sized with, so no runtime narrowing occurs.
		err := windows.ReadConsole(r.h, &buf[0], consoleChunk, &read, nil)
		if err != nil {
			if len(units) == 0 {
				return "", fmt.Errorf("读控制台输入: %w", err)
			}
			// 已攒到的内容先交出去（与字节流路径同一取向：数据先到就先给，
			// 错误留给下一次调用——bufio 也是这么做的）。
			// Hand over what arrived; the error resurfaces on the next call.
			break
		}
		if read == 0 {
			if len(units) == 0 {
				// 控制台输入结束（用户在行首按 Ctrl-Z 回车）：与管道 EOF 同一语义。
				// End of console input, the same semantics as a pipe's EOF.
				return "", io.EOF
			}
			break
		}
		units = append(units, buf[:read]...)
		if idx := slices.Index(units, '\n'); idx >= 0 {
			units = units[:idx]
			break
		}
	}
	return strings.TrimSuffix(windows.UTF16ToString(units), "\r"), nil
}

// consoleWriter 经控制台 API 写：`WriteConsoleW` 收 UTF-16。
// consoleWriter writes through the console API, in UTF-16.
type consoleWriter struct {
	h windows.Handle
}

// write 写出一段文本（自带行尾换行），单次写不完就分段——一个写调用能吃多长不是
// 调用方能假定的（写满即返回，剩下的自己接着写）。
// write emits the text and loops until every unit has been accepted.
func (w *consoleWriter) write(text string) error {
	units := utf16.Encode([]rune(text + "\n"))
	for len(units) > 0 {
		n := min(len(units), consoleChunk)
		var written uint32
		if err := windows.WriteConsole(w.h, &units[0], uint32(n), &written, nil); err != nil {
			return fmt.Errorf("写控制台输出: %w", err)
		}
		if written == 0 {
			// 没报错却一个字符也没写进去：如实报错，不假装写完了。
			// No error but nothing accepted: report it rather than pretend success.
			return errors.New("写控制台输出: 写入 0 个字符")
		}
		units = units[written:]
	}
	return nil
}

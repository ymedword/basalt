package channel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// 本文件是渠道层的**平台中立半边**：接口形态 + 控制台读写面的抽象。平台半边在
// `console_<goos>.go`（各带 build tag）——真控制台与管道 / 重定向在那里分流，
// 判定由终端属性查询给出，**不**猜环境变量（FR-040 / 技术方案 §8.4）。
// The platform-neutral half of the channel layer: the interface shapes plus the
// console abstraction; the per-OS files decide console-API versus byte stream.

// Channel 是渠道的消费方接口：入站消息进、出站回复出。
// Channel is the consumer-declared interface of a channel adapter.
type Channel interface {
	// Name 是渠道名（会话标识派生的第一元，§6.2 固定口径 1）。
	// Name is the channel's name, the first element of session-id derivation.
	Name() string

	// Send 把一条回复写出去。
	// Send writes one reply out.
	Send(ctx context.Context, r Reply) error

	// Receive 读一条入站消息；输入结束返回 io.EOF。
	// Receive reads one inbound message; io.EOF marks the end of input.
	Receive(ctx context.Context) (Message, error)
}

// Message 是一条入站消息：正文 + 用户标识。
// Message is one inbound message: its text plus who sent it.
//
// 只带这两项是有意的：会话标识的派生需要 (渠道, 用户, 画像) 三元组，渠道名由
// `Name()` 给出、画像名由命令面装载后给出，只有**用户标识**属于进程环境（当前 OS
// 用户名），必须由渠道层解析——其余两项在这里多带一遍就多一个不一致的来源。
// Only the text and user id travel here: the channel name and profile name are
// resolved by the caller, so carrying them again would add a second source of truth.
type Message struct {
	Text   string
	UserID string
}

// Reply 是一条出站回复：正文已是**渲染完成的最终文本**（渲染口径在命令面
// §6.2 固定口径 4），渠道只负责写到对的介质上，不再加工。
// Reply carries the already-rendered text; the channel only transports it.
type Reply struct {
	Text string
}

// console 是平台控制台读写面，由 newConsole（各平台一份）构造。
// console is the per-platform read/write surface built by newConsole.
//
// 读与写**分开**构造：标准输入与标准输出可以被分别重定向（`chat > out.txt` 的输入
// 仍是真控制台），任何一侧都不该跟着另一侧走。
// Reader and writer are built independently because the two standard streams can be
// redirected separately.
type console struct {
	reader lineReader
	writer lineWriter
}

// lineReader 逐行读入；行尾换行符不属于内容。
// lineReader reads one line at a time; the trailing newline is not content.
type lineReader interface {
	// readLine 读一行；输入结束返回 io.EOF（末行没有换行符时先交出末行，下一次
	// 调用才返回 io.EOF）。
	// readLine returns io.EOF at end of input, after yielding a final unterminated line.
	readLine() (string, error)
}

// lineWriter 写出一段文本（实现自带行尾换行）。
// lineWriter writes a piece of text, adding the line terminator itself.
type lineWriter interface {
	write(text string) error
}

// newFileConsole 按**字节流**读写一个文件句柄对：管道 / 重定向两侧一律显式 UTF-8
// （Go 字符串本身就是 UTF-8 字节序列，这里不做任何转码、也不经过代码页）。
// newFileConsole reads and writes byte streams as explicit UTF-8.
//
// terminal 为假（管道 / 重定向）时吃掉行尾 CRLF 的 `\r`：它是传输介质的产物，不是
// 用户输入的内容——不吃进去，每条消息尾部都会多一个看不见的字符。terminal 为真
// （Unix 真终端）时不吃：终端驱动已经把 CR 归一成 NL，此时还剩的 `\r` 是用户真要的
// 字节。（Windows 的真终端不走这条路——`console_windows.go` 的控制台 API 自己处理
// 行尾。）
// When terminal is false a trailing carriage return is dropped as a transport
// artifact; on a Unix terminal the driver already normalized CR, so it is kept.
func newFileConsole(in io.Reader, out io.Writer, terminal bool) console {
	return console{
		reader: &fileReader{r: bufio.NewReader(in), keepCR: terminal},
		writer: &fileWriter{w: out},
	}
}

// fileReader 从字节流按行读。
// fileReader reads lines off a byte stream.
//
// 用 `bufio.Reader` 而不是 `bufio.Scanner`：Scanner 的默认行长上限是 64 KiB，超长
// 的一行会被静默截断成两行（`ReadString` 没有这个上限）——消息长度由用户决定，读的
// 这一层不该替它设限。
// A bufio.Reader rather than a bufio.Scanner: the scanner's 64 KiB default line cap
// would silently split a long line into two messages.
type fileReader struct {
	r      *bufio.Reader
	keepCR bool
}

// readLine 读一行。
// readLine reads one line.
func (f *fileReader) readLine() (string, error) {
	line, err := f.r.ReadString('\n')
	if err != nil && line == "" {
		// 没读到字节就只有一种终止态可交：如实上抛（io.EOF 就是"输入结束"）。
		// Nothing was read, so the terminating condition is all there is to report.
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	if !f.keepCR {
		line = strings.TrimSuffix(line, "\r")
	}
	return line, nil
}

// fileWriter 把一段文本写到字节流（正文 + 行尾换行，两次写）。
// fileWriter writes text plus its line terminator.
type fileWriter struct {
	w io.Writer
}

// write 写出一段文本（自带行尾换行——每条回复是一整块，写完即结束了这一轮）。
// write emits the text and terminates the line.
func (f *fileWriter) write(text string) error {
	if _, err := f.w.Write([]byte(text)); err != nil {
		return fmt.Errorf("写标准输出: %w", err)
	}
	if _, err := f.w.Write([]byte("\n")); err != nil {
		return fmt.Errorf("写标准输出: %w", err)
	}
	return nil
}

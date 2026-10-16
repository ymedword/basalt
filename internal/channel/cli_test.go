package channel

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本文件对应颗粒度文档 §3.2 的 channel/cli 八条（Q1~Q8）。自动化这一侧全部建在
// **管道**上：管道就是"非终端"分支的天然夹具；真控制台的肯定读数（Windows 控制台
// 编码、Linux / Darwin 真终端）与三平台编译属人工项（颗粒度文档 §6.1 第 4 项），
// 此处不作判据。
// The eight cases here run on pipes, the natural fixture for the non-terminal branch;
// real-console readings and cross-platform builds stay manual acceptance items.

// pipePair 造一对管道，调用方负责用 closeFile 关闭。
// pipePair builds a pipe pair; the caller closes both ends with closeFile.
func pipePair(t *testing.T) (readEnd, writeEnd *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err, "造管道")
	return r, w
}

// closeFile 关闭测试用文件并如实报出失败；"已经关过"不算失败（清理路径会碰见）。
// closeFile closes a test file and reports failures; closing twice is not one.
func closeFile(t *testing.T, f *os.File) {
	t.Helper()
	if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("关闭 %s 失败: %v", f.Name(), err)
	}
}

// Q1 编解码往返：同一条消息（中文 + emoji）经 Loop 逐字进、逐字回。
// Q1 The loop round-trips a UTF-8 message, emoji included, without mangling it.
func TestCliChannel_LoopRoundTripsUTF8(t *testing.T) {
	t.Run("单条即退", func(t *testing.T) {
		inR, inW := pipePair(t)
		outR, outW := pipePair(t)
		defer closeFile(t, inR)
		defer closeFile(t, inW)
		defer closeFile(t, outR)

		// 场景 1 的那句话：模型不认识这个文件，渠道只负责把它原样送到命令面。
		const prompt = "读一下工作区里的 notes.txt 并总结 🚀"
		ch := NewCliChannel(CliOptions{In: inR, Out: outW, Message: prompt, UserID: "tester"})
		require.Equal(t, "cli", ch.Name(), "渠道名")

		var got []Message
		handle := func(_ context.Context, msg Message) (Reply, error) {
			got = append(got, msg)
			return Reply{Text: "收到：" + msg.Text}, nil
		}
		require.NoError(t, ch.Loop(context.Background(), handle))
		closeFile(t, outW)

		require.Len(t, got, 1, "单条消息只该触发一轮运行")
		assert.Equal(t, Message{Text: prompt, UserID: "tester"}, got[0], "入站消息")
		body, err := io.ReadAll(outR)
		require.NoError(t, err, "读回复")
		assert.Equal(t, "收到："+prompt+"\n", string(body), "出站回复")
	})

	t.Run("交互式多轮", func(t *testing.T) {
		inR, inW := pipePair(t)
		outR, outW := pipePair(t)
		defer closeFile(t, inR)
		defer closeFile(t, outR)

		_, err := io.WriteString(inW, "你好，世界 👋\n")
		require.NoError(t, err, "预置输入")
		closeFile(t, inW) // 输入结束 → 循环收场

		ch := NewCliChannel(CliOptions{In: inR, Out: outW, UserID: "tester"})
		var got []string
		require.NoError(t, ch.Loop(context.Background(), func(_ context.Context, msg Message) (Reply, error) {
			got = append(got, msg.Text)
			return Reply{Text: "echo: " + msg.Text}, nil
		}))
		closeFile(t, outW)

		assert.Equal(t, []string{"你好，世界 👋"}, got, "入站消息逐字往返")
		body, err := io.ReadAll(outR)
		require.NoError(t, err, "读回复")
		assert.Equal(t, "echo: 你好，世界 👋\n", string(body), "出站回复逐字往返")
	})
}

// Q2 管道输入按 UTF-8 读、行尾归一、末行无换行时先交末行。
// Q2 Piped input is read as UTF-8, CRLF is normalized, and a final unterminated
// line is yielded before io.EOF.
func TestCliChannel_ReadsPipeAsUTF8(t *testing.T) {
	t.Run("多字节不拆散与 CRLF 归一", func(t *testing.T) {
		inR, inW := pipePair(t)
		outR, outW := pipePair(t)
		defer closeFile(t, inR)
		defer closeFile(t, outR)
		defer closeFile(t, outW)

		// 三条一次写进去：多字节行（不被拆成两条）、CRLF 行（`\r` 是介质的产物）、
		// 末行没有换行符（先交内容，下一次调用才是 io.EOF）。
		_, err := io.WriteString(inW, "第一行 🚀\ncrlf\r\n末行无换行")
		require.NoError(t, err, "预置输入")
		closeFile(t, inW)

		ch := NewCliChannel(CliOptions{In: inR, Out: outW, UserID: "tester"})
		ctx := context.Background()
		for i, want := range []string{"第一行 🚀", "crlf", "末行无换行"} {
			msg, err := ch.Receive(ctx)
			require.NoError(t, err, "第 %d 条", i+1)
			assert.Equal(t, want, msg.Text, "第 %d 条", i+1)
		}
		_, err = ch.Receive(ctx)
		assert.ErrorIs(t, err, io.EOF, "第 4 次调用")
	})

	t.Run("真终端语义保留 CR", func(t *testing.T) {
		inR, inW := pipePair(t)
		defer closeFile(t, inR)

		_, err := io.WriteString(inW, "crlf\r\n")
		require.NoError(t, err, "预置输入")
		closeFile(t, inW)

		// terminal=true（Unix 真终端）时终端驱动已把 CR 归一成 NL，剩下还能读到的
		// CR 就是用户真要的字节——不吃。真终端本身只能人工验，这里判的是这条分界。
		c := newFileConsole(inR, io.Discard, true)
		line, err := c.reader.readLine()
		require.NoError(t, err, "读一行")
		assert.Equal(t, "crlf\r", line, "terminal=true 保留 CR")
	})
}

// Q3 管道输出按 UTF-8 写：写出的字节逐字等于"正文 + 换行"。
// Q3 Piped output is written as UTF-8: exactly the text plus the line terminator.
func TestCliChannel_WritesPipeAsUTF8(t *testing.T) {
	inR, inW := pipePair(t)
	outR, outW := pipePair(t)
	defer closeFile(t, inR)
	defer closeFile(t, inW)
	defer closeFile(t, outR)

	ch := NewCliChannel(CliOptions{In: inR, Out: outW, UserID: "tester"})
	const text = "notes.txt 写了三行，给你看正文 🔤"
	require.NoError(t, ch.Send(context.Background(), Reply{Text: text}), "写回复")
	closeFile(t, outW)

	body, err := io.ReadAll(outR)
	require.NoError(t, err, "读回复字节")
	assert.Equal(t, text+"\n", string(body), "出站字节")
	assert.True(t, utf8.ValidString(string(body)), "出站字节是合法 UTF-8")
}

// Q4 两条非交互终止语义：标准输入结束 → 正常收场（零轮运行）；--message → 恰一轮。
// Q4 Two non-interactive terminations: EOF ends the loop quietly, --message runs once.
func TestCliChannel_TerminatesOnEOFAndSingleMessage(t *testing.T) {
	t.Run("标准输入结束", func(t *testing.T) {
		inR, inW := pipePair(t)
		outR, outW := pipePair(t)
		defer closeFile(t, inR)
		defer closeFile(t, outR)
		defer closeFile(t, outW)

		closeFile(t, inW) // 空输入：立刻 EOF
		ch := NewCliChannel(CliOptions{In: inR, Out: outW, UserID: "tester"})

		_, err := ch.Receive(context.Background())
		assert.ErrorIs(t, err, io.EOF, "Receive 的终止态")

		calls := 0
		require.NoError(t, ch.Loop(context.Background(), func(context.Context, Message) (Reply, error) {
			calls++
			return Reply{}, nil
		}))
		assert.Zero(t, calls, "输入结束不该触发任何一轮运行")
	})

	t.Run("单条即退", func(t *testing.T) {
		inR, inW := pipePair(t)
		outR, outW := pipePair(t)
		defer closeFile(t, inR)
		defer closeFile(t, inW)
		defer closeFile(t, outR)

		ch := NewCliChannel(CliOptions{In: inR, Out: outW, Message: "只跑这一次", UserID: "tester"})
		calls := 0
		require.NoError(t, ch.Loop(context.Background(), func(_ context.Context, msg Message) (Reply, error) {
			calls++
			assert.Equal(t, "只跑这一次", msg.Text, "轮内消息")
			return Reply{Text: "done"}, nil
		}))
		closeFile(t, outW)

		assert.Equal(t, 1, calls, "单条模式恰一轮")
		body, err := io.ReadAll(outR)
		require.NoError(t, err, "读回复字节")
		assert.Equal(t, "done\n", string(body), "出站回复")
	})
}

// Q5 Ctrl-C 只取消当前运行：该次运行的上下文被取消、回复照发、循环收场、整场会话
// 的上下文不受影响（进程退出码 0 由"Loop 返回 nil"承载）。
// Q5 Ctrl-C cancels the run in flight only: the run's context is cancelled, the reply
// is still written, the loop ends, and the session context is untouched.
func TestCliChannel_CtrlCCancelsCurrentRun(t *testing.T) {
	inR, inW := pipePair(t)
	outR, outW := pipePair(t)
	defer closeFile(t, inR)
	defer closeFile(t, outR)
	defer closeFile(t, inW)

	// 预置两行：第一轮跑起来后触发中断；循环若没被打断，第二行会变成第二轮运行。
	_, err := io.WriteString(inW, "第一行\n第二行\n")
	require.NoError(t, err, "预置输入")

	// 信号在真控制台上按不出来，可判卷的入口是注入的通道（CliOptions.Signals）。
	signals := make(chan os.Signal, 1)
	ch := NewCliChannel(CliOptions{In: inR, Out: outW, UserID: "tester", Signals: signals})

	ctx := context.Background()
	calls := 0
	handle := func(runCtx context.Context, _ Message) (Reply, error) {
		calls++
		signals <- os.Interrupt
		select {
		case <-runCtx.Done():
			assert.ErrorIs(t, runCtx.Err(), context.Canceled, "取消原因")
		case <-time.After(5 * time.Second):
			t.Error("限时内未收到取消：中断没有送达当前运行")
		}
		return Reply{Text: "已取消"}, nil
	}
	require.NoError(t, ch.Loop(ctx, handle))
	closeFile(t, outW)

	assert.Equal(t, 1, calls, "中断之后不该再读下一行")
	assert.NoError(t, ctx.Err(), "Ctrl-C 只取消当前运行，不取消整场会话")
	body, err := io.ReadAll(outR)
	require.NoError(t, err, "读回复字节")
	assert.Equal(t, "已取消\n", string(body), "取消后的回复仍要写出去")
}

// Q6 "是不是终端"由内核回答、不猜环境变量（FR-040）。
// Q6 Terminal detection asks the kernel and never guesses at environment variables.
func TestCliChannel_IsTerminalDoesNotGuessEnvironment(t *testing.T) {
	r, w := pipePair(t)
	defer closeFile(t, r)
	defer closeFile(t, w)

	require.False(t, isTerminal(r), "管道读端不是终端")
	require.False(t, isTerminal(w), "管道写端不是终端")

	for _, key := range []string{"TERM", "CI", "WT_SESSION", "TERM_PROGRAM"} {
		t.Setenv(key, "1")
	}
	assert.False(t, isTerminal(r), "环境变量不参与判定")
	assert.False(t, isTerminal(w), "环境变量不参与判定")
}

// Q7 非终端路径就是字节流：完整走一遍读与写（选了哪条路 + 这条路通）。
// Q7 The non-terminal path is the byte-stream one; both directions are exercised.
func TestCliChannel_NonTerminalPathUsesByteStream(t *testing.T) {
	inR, inW := pipePair(t)
	outR, outW := pipePair(t)
	defer closeFile(t, inR)
	defer closeFile(t, outR)

	c := newConsole(inR, outW)
	_, isReader := c.reader.(*fileReader)
	_, isWriter := c.writer.(*fileWriter)
	assert.True(t, isReader, "管道输入应走字节流实现")
	assert.True(t, isWriter, "管道输出应走字节流实现")

	_, err := io.WriteString(inW, "你好\n")
	require.NoError(t, err, "预置输入")
	closeFile(t, inW)
	line, err := c.reader.readLine()
	require.NoError(t, err, "读一行")
	assert.Equal(t, "你好", line, "读回的正文")

	require.NoError(t, c.writer.write("回复"), "写一行")
	closeFile(t, outW)
	body, err := io.ReadAll(outR)
	require.NoError(t, err, "读回复字节")
	assert.Equal(t, "回复\n", string(body), "出站字节")
}

// Q8 超长行不截断：`bufio.Scanner` 的 64 KiB 默认上限会把超长行静默切成两条消息
// （本包因此用 `bufio.Reader`）——超长输入必须仍是一条完整消息。
// Q8 A long line stays one message: the scanner's 64 KiB cap would silently split it.
func TestCliChannel_LongLineIsNotTruncated(t *testing.T) {
	inR, inW := pipePair(t)
	outR, outW := pipePair(t)
	defer closeFile(t, inR)
	defer closeFile(t, outR)
	defer closeFile(t, outW)

	const size = 120000 // > 64 KiB：跨过 Scanner 的上限才判得动
	long := strings.Repeat("汉", size/len("汉"))

	// 写端放协程：管道缓冲有限，先写完再读会互相等死。
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(inW, long+"\n")
		written <- err
	}()

	ch := NewCliChannel(CliOptions{In: inR, Out: outW, UserID: "tester"})
	msg, err := ch.Receive(context.Background())
	require.NoError(t, err, "读超长行")
	assert.Len(t, msg.Text, len(long), "正文长度（字节）")
	assert.Equal(t, long[:9], msg.Text[:9], "正文开头")
	assert.Equal(t, long[len(long)-9:], msg.Text[len(msg.Text)-9:], "正文结尾")

	select {
	case werr := <-written:
		require.NoError(t, werr, "写端")
	case <-time.After(5 * time.Second):
		t.Fatal("写端限时内未写完")
	}
}

package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"sync"
)

// cliName 是命令行渠道名（会话标识派生的第一元，§6.2 固定口径 1）。
// cliName is the command-line channel's name.
const cliName = "cli"

// unknownUser 是取不到当前 OS 用户名时的回落值（§6.2 固定口径 1）。
// unknownUser is the fallback when the OS cannot report a user name.
const unknownUser = "unknown"

// CliOptions 是命令行渠道的构造参数。
// CliOptions carries the construction parameters of the CLI channel.
//
// In / Out 为空时分别回落 `os.Stdin` / `os.Stdout`；测试注入管道对（管道即"非终端"
// 分支的天然夹具，真控制台读数只能人工采——§6.1 第 4 项）。
// Nil In/Out fall back to the process standard streams.
type CliOptions struct {
	// In / Out 是输入 / 输出介质（真控制台或管道，由 `newConsole` 逐句柄分流）。
	In  *os.File
	Out *os.File

	// Message 是 `--message` 的单条输入：非空即"单条即退"，不碰标准输入。
	// Message is the one-shot input of `--message`; non-empty means run once and exit.
	Message string

	// UserID 是操作用户标识；为空时解析当前 OS 用户名，取不到回落 `unknown`。
	// UserID is resolved from the OS user when left empty.
	UserID string

	// Signals 是中断（Ctrl-C）来源；为空时由 `Loop` 自建一条并登记 `os.Interrupt`。
	// 注入是为了可判卷：信号在真控制台上按不出来，测试拿得住的只有一条通道。
	// Signals is the interrupt source; injections make the Ctrl-C path testable.
	Signals <-chan os.Signal

	// Logger 是结构化日志器；为空即不记（中断与回落都是诊断信息，不属审计面）。
	// Logger is optional diagnostic logging; nothing here is part of the audit trail.
	Logger *slog.Logger
}

// Handler 处理一轮输入（= 一次运行）并给出回复。
// Handler runs one turn of the loop, i.e. one agent run, and returns its reply.
//
// ctx 是**这一次运行**的上下文：Ctrl-C 取消的正是它，不是整场会话、更不是进程。
// 返回 error 表示循环没法继续（如运行入口报了调用方问题里的致命项），`Loop` 据此
// 收场并把错误交给调用方；运行本身的失败已经在回复正文里了（运行状态归
// `RunResult`），不该走这条出口。
// The ctx belongs to this one run; a non-nil error stops the loop.
type Handler func(ctx context.Context, msg Message) (Reply, error)

// CliChannel 是命令行渠道（核心阶段唯一的渠道实现）。
// CliChannel is the command-line channel, the only channel in the core stage.
//
// 它承担的**只有 I/O 与中断**：会话标识派生、预算与时限、渲染口径都在命令面
// （`internal/cli`）——渠道不做业务判断，命令面也不碰终端差异。
// It owns I/O and interrupts only; ids, budgets and rendering live in the command
// surface.
type CliChannel struct {
	message string
	userID  string
	signals <-chan os.Signal
	logger  *slog.Logger
	con     console

	// mu 护住"当前运行"的取消函数：控制台读在别的协程里等中断，取消槽位只有在
	// 锁下交接才不会有"取消了一个已经结束的运行"或"漏掉正在跑的运行"的窗口。
	mu sync.Mutex
	// cancel 是当前运行的取消函数（不是 context 值——context 本身不落字段，
	// 见工程规范 §1.6）；运行结束后清空。
	cancel context.CancelFunc
	// interrupted 记录"刚收到过中断"：读被中断打断、运行被取消都据此收场。
	interrupted bool
}

// 编译期断言：命令行渠道满足消费方（本包）声明的渠道接口。
// Compile-time assertion that CliChannel satisfies the Channel interface.
var _ Channel = (*CliChannel)(nil)

// NewCliChannel 构造命令行渠道。
// NewCliChannel builds the command-line channel.
func NewCliChannel(opts CliOptions) *CliChannel {
	in, out := opts.In, opts.Out
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	return &CliChannel{
		message: opts.Message,
		userID:  resolveUserID(opts.UserID, opts.Logger),
		signals: opts.Signals,
		logger:  opts.Logger,
		con:     newConsole(in, out),
	}
}

// Name 是渠道名。
// Name reports the channel's name.
func (c *CliChannel) Name() string { return cliName }

// Send 把一条回复写出去（正文已是最终文本，这里只加行尾）。
// Send writes one reply out, adding the line terminator.
func (c *CliChannel) Send(ctx context.Context, r Reply) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("写回复: %w", err)
	}
	return c.con.writer.write(r.Text)
}

// Receive 读一条入站消息；输入结束返回 io.EOF。
// Receive reads one inbound message; io.EOF marks the end of input.
//
// 取消的送达方式由平台面决定：Windows 的真控制台读会被 Ctrl-C 打断（以错误收场），
// 管道 / Unix 侧的阻塞读不会——那时中断只记在渠道里，`Loop` 在下一句话收场时据此
// 退出。两种情形都不丢数据：读到的内容照常交出。
// How a cancellation reaches a blocked read is platform business; either way the
// data already read is still handed over.
func (c *CliChannel) Receive(ctx context.Context) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	text, err := c.con.reader.readLine()
	if err != nil {
		return Message{}, err
	}
	return Message{Text: text, UserID: c.userID}, nil
}

// Loop 驱动交互循环：取一条输入 → 交 `handle` 处理（一次运行）→ 把回复写出去，
// 直到输入结束。三条终止语义（§6.2 固定口径 3）。
// Loop drives the interactive loop until one of the three terminations applies.
//
//   - 标准输入 EOF → 正常返回（进程退出码 0）；
//   - `--message` → 单条即退，不读标准输入；
//   - Ctrl-C → 取消**当前运行**（运行入口据此把该次运行标成已取消、渲染出取消
//     提示），Loop 在把这一轮回复写完之后返回——已落库的历史不属于"当前运行"，
//     取消碰不到它。调用方据此以退出码 0 收场：取消是预期动作，不是错误。
//
// 返回 error 只有一种情形：`handle` 说循环没法继续（或回复写不出去）。
// Ctrl-C at rest (no run in flight) is recorded and honoured at the end of the turn.
func (c *CliChannel) Loop(ctx context.Context, handle Handler) error {
	signals, stopSignals := c.interrupts()
	defer stopSignals()

	done := make(chan struct{})
	stopped := make(chan struct{})
	// 中断看护：什么时候退出是可说清的——`Loop` 返回时关掉 done，本协程随即退出，
	// 且 `Loop` 会等它退完（不留孤儿协程，测试因此可判卷）。
	go func() {
		defer close(stopped)
		c.watch(signals, done)
	}()
	defer func() {
		close(done)
		<-stopped
	}()

	for {
		if ctx.Err() != nil {
			// 循环级取消（调用方收摊）：与 Ctrl-C 同一收场语义——正常返回。
			return nil
		}
		msg, err := c.next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if c.wasInterrupted() {
				// 控制台读被 Ctrl-C 打断：中断已经记下，这一轮到此为止。
				return nil
			}
			return err
		}
		reply, err := c.runOnce(ctx, msg, handle)
		if err != nil {
			return err
		}
		if err := c.Send(ctx, reply); err != nil {
			return err
		}
		if c.wasInterrupted() || c.message != "" {
			return nil
		}
	}
}

// next 取下一轮输入：`--message` 模式直接给那一句，交互模式读一行。
// next yields the next turn's input, either the one-shot message or a read line.
func (c *CliChannel) next(ctx context.Context) (Message, error) {
	if c.message != "" {
		return Message{Text: c.message, UserID: c.userID}, nil
	}
	return c.Receive(ctx)
}

// runOnce 跑一轮：为这一次运行单独建取消函数（Ctrl-C 取消的就是它），运行结束后
// 立刻把槽位清空并释放——收场之后才到的中断不该再取消一个已经结束的上下文。
// runOnce gives this run its own cancel func and clears the slot once it is over.
func (c *CliChannel) runOnce(ctx context.Context, msg Message, handle Handler) (Reply, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.mu.Lock()
	c.cancel = cancel
	c.interrupted = false
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
	}()

	return handle(runCtx, msg)
}

// watch 是中断源与"当前运行"之间的桥：收到中断就取消当时在跑的那一次运行。
// watch bridges the interrupt source to whatever run is in flight.
func (c *CliChannel) watch(signals <-chan os.Signal, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case sig, ok := <-signals:
			if !ok {
				return
			}
			c.cancelCurrent(sig)
		}
	}
}

// cancelCurrent 处理一次中断：记下"刚被中断"，并取消当前运行（没有运行在飞时——
// 比如停在提示符上——只记下，`Loop` 收场时据此退出）。
// cancelCurrent records the interrupt and cancels the run in flight, if any.
func (c *CliChannel) cancelCurrent(sig os.Signal) {
	c.mu.Lock()
	c.interrupted = true
	cancel := c.cancel
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if c.logger != nil {
		c.logger.Debug("cli 渠道收到中断，取消当前运行", "signal", sig.String())
	}
}

// wasInterrupted 报告"刚收到过中断"。
// wasInterrupted reports whether an interrupt has just arrived.
func (c *CliChannel) wasInterrupted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interrupted
}

// interrupts 返回中断源与它的注销函数：注入了通道就用注入的（测试据此可控），
// 没注入就自建一条并登记 `os.Interrupt`——登记是**进程级**状态，所以注销一并交回，
// 由 `Loop` 退出时执行（谁登记谁注销）。
// interrupts returns the signal source plus its unregister function, so a
// process-level registration is always undone by the same owner.
func (c *CliChannel) interrupts() (<-chan os.Signal, func()) {
	if c.signals != nil {
		return c.signals, func() {}
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	return ch, func() { signal.Stop(ch) }
}

// resolveUserID 解析操作用户标识：显式给了就用显式的，否则取当前 OS 用户名；取不到
// （容器里没有该用户、系统答不出）回落 `unknown`（§6.2 固定口径 1）——会话标识的
// 派生需要一个稳定值，这里宁可用一个明确的兜底，也不让整个派生失败。
// resolveUserID resolves the operator identity, falling back to a fixed value that
// keeps session-id derivation working.
func resolveUserID(explicit string, logger *slog.Logger) string {
	if explicit != "" {
		return explicit
	}
	u, err := user.Current()
	if err == nil && u.Username != "" {
		return u.Username
	}
	if logger != nil {
		// 回落不是错误，但原因要留痕——否则"为什么所有会话都叫 unknown"无从查起。
		reason := "系统未返回用户名"
		if err != nil {
			reason = err.Error()
		}
		logger.Debug("取不到当前 OS 用户名，会话标识回落固定值", "fallback", unknownUser, "reason", reason)
	}
	return unknownUser
}

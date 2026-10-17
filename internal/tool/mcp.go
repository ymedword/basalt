package tool

// 本文件是 MCP 客户端池（US-7）：单 `mcp.Client` 承载多会话，逐 server 串行「起子进程 →
// 初始化 → 列工具」。预算与进程生命周期分离：每 server 一个握手预算（只罩 initialize 与
// tools/list），dial 不接收 ctx——**不得**用 `exec.CommandContext` 把子进程绑到预算上
// （约束 27；颗粒度文档 §2.2 约束 28）。
// This file holds the MCP client pool: one client, many sessions, serial per-server
// connect. The handshake budget covers initialize and tools/list only; dialing never
// binds the child process to a context.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// defaultMCPConnectTimeout 是每 server 的握手预算缺省值（30 秒，代码常量、零新增配置键）：
	// 只覆盖 initialize 与 tools/list 两步；释放后会话与子进程继续存活到 Close（R11 钉住）。
	defaultMCPConnectTimeout = 30 * time.Second
	// mcpClientName 是 SDK 客户端的实现名（服务端可见的客户端标识）。
	mcpClientName = "basalt"
)

// MCPConnection 是一条 server 的连接结果：server 名 + 已适配为 `core.Tool` 的工具列表
// （顺序 = server 返回的工具列表序）。
// MCPConnection is one connected server with its adapted tools.
type MCPConnection struct {
	Server string
	Tools  []core.Tool
}

// MCPPoolOptions 是连接池的构造参数；`ConnectTimeout` 为零值取 30 秒缺省，
// `Version` 进客户端实现标识，`Root` 是子进程工作目录（工作区根，约束 18）。
// MCPPoolOptions configures the pool; a zero ConnectTimeout means the 30s default.
type MCPPoolOptions struct {
	Logger         *slog.Logger
	EnvPassthrough []string
	ConnectTimeout time.Duration
	Version        string
	Root           string
}

// MCPPool 是 MCP 客户端池。`dial` 是非导出注入点：包内白盒测试替换它即可不起真实子进程，
// **不新增导出测试 API**。
// MCPPool is the client pool; dial is the package-internal seam replacing the real
// subprocess in white-box tests.
type MCPPool struct {
	logger         *slog.Logger
	envPassthrough []string
	connectTimeout time.Duration
	root           string
	client         *mcp.Client
	dial           func(core.MCPServerConfig) (mcp.Transport, error)

	mu       sync.Mutex
	sessions []*mcp.ClientSession
}

// NewMCPPool 装配连接池：缺省 logger 丢弃、缺省预算 30 秒；`dial` 默认真实命令传输。
// NewMCPPool wires the pool with a discard logger and the 30s budget by default.
func NewMCPPool(opts MCPPoolOptions) *MCPPool {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	timeout := opts.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultMCPConnectTimeout
	}
	pool := &MCPPool{
		logger:         logger,
		envPassthrough: slices.Clone(opts.EnvPassthrough),
		connectTimeout: timeout,
		root:           opts.Root,
		client: mcp.NewClient(&mcp.Implementation{
			Name:    mcpClientName,
			Version: opts.Version,
		}, nil),
	}
	pool.dial = pool.dialCommand
	return pool
}

// Connect 逐 server 串行连接并列出工具，按 specs 顺序返回连接结果。失败吸收（约束 28）：
// dial / initialize / 列工具任一步失败只影响当前 server——WARN 固定键点名 + 关闭半截会话 +
// 继续下一个；**只有整体 ctx 取消 / 截止**才作为池级错误返回（启动不中断）。
// Connect dials, initializes and lists tools per spec; per-server failures are absorbed
// into a fixed-key warning and the loop continues. Only outer cancellation/deadline is an
// error.
func (p *MCPPool) Connect(ctx context.Context, specs []core.MCPServerConfig) ([]MCPConnection, error) {
	conns := make([]MCPConnection, 0, len(specs))
	for _, spec := range specs {
		// 取消 / 截止是池级信号：后面不必再看（也已无预算可言）。
		if err := ctx.Err(); err != nil {
			return conns, err
		}
		transport, err := p.dial(spec)
		if err != nil {
			p.logger.Warn("MCP server 未连接——已跳过", "server", spec.Name,
				"reason", "启动失败", "detail", safeErrorSummary(err))
			// 取消 / 截止可能发生在拨号期间：失败已吸收，但池级信号不得跟着被吞。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return conns, ctxErr
			}
			continue
		}
		// 预算只罩下面两步；到点之前会话已建立、子进程已存活。
		cctx, cancel := context.WithTimeout(ctx, p.connectTimeout)
		session, err := p.client.Connect(cctx, transport, nil)
		if err != nil {
			cancel()
			// SDK 在 Connect 失败路径已关闭连接（传输失败时根本没有进程），这里不必再关。
			p.logger.Warn("MCP server 未连接——已跳过", "server", spec.Name,
				"reason", "初始化失败", "detail", safeErrorSummary(err))
			// 父 ctx 已取消 / 截止 → 池级错误；只有预算单独到点（父 ctx 未结束）才跳过。
			if ctxErr := ctx.Err(); ctxErr != nil {
				return conns, ctxErr
			}
			continue
		}
		tools, err := listAllTools(cctx, session)
		if err != nil {
			_ = session.Close() // 收起半截会话（连带子进程）
			cancel()
			p.logger.Warn("MCP server 未连接——已跳过", "server", spec.Name,
				"reason", "列工具失败", "detail", safeErrorSummary(err))
			if ctxErr := ctx.Err(); ctxErr != nil {
				return conns, ctxErr
			}
			continue
		}
		cancel()
		adapters := make([]core.Tool, 0, len(tools))
		for _, mcpTool := range tools {
			adapters = append(adapters, newMCPToolAdapter(session, spec.Name, mcpTool))
		}
		p.mu.Lock()
		p.sessions = append(p.sessions, session)
		p.mu.Unlock()
		conns = append(conns, MCPConnection{Server: spec.Name, Tools: adapters})
		p.logger.Info("MCP server 已连接", "server", spec.Name, "tools", len(adapters))
	}
	// 最后一次成功连接的收尾也可能与父 ctx 取消同刻发生：失败分支都查过，成功路径
	// 仍要复核一次，否则整体取消会被"最后一个 server 连接成功"掩盖。
	if err := ctx.Err(); err != nil {
		return conns, err
	}
	return conns, nil
}

// safeErrorSummary 把连接期错误映射到**固定短语集**（约束 28）：命令名 / server 消息 /
// 路径等原文一律不出现——它们可能含凭证（R12 前半）。单行化与截断都不是脱敏。
// safeErrorSummary maps connect-time errors onto a fixed phrase set; raw error text
// never leaves this function.
func safeErrorSummary(err error) string {
	switch {
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist):
		return "可执行文件不存在"
	case errors.Is(err, fs.ErrPermission):
		return "权限不足"
	case errors.Is(err, context.DeadlineExceeded):
		return "超时"
	case errors.Is(err, context.Canceled):
		return "已取消"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("子进程退出码 %d", exit.ExitCode())
	}
	return "外部交互失败"
}

// Close 关闭池内全部会话（幂等：第二次取到的会话集为空）；逐个错误合并返回、不静默。
// Close shuts down every pooled session; repeated calls are no-ops.
func (p *MCPPool) Close() error {
	p.mu.Lock()
	sessions := p.sessions
	p.sessions = nil
	p.mu.Unlock()
	var errs []error
	for _, session := range sessions {
		errs = append(errs, session.Close())
	}
	return errors.Join(errs...)
}

// dialCommand 构造真实子进程传输：command 只按空白切分（无 shell 引号语义，约束 19），
// `exec.Command` 而不是 `exec.CommandContext`——进程生命周期不绑握手预算（约束 27）；
// 工作目录 = 工作区根，环境 = 白名单透传 + 配置声明（约束 17 / 18）。
// dialCommand builds the real subprocess transport without binding the process to any
// context; splitting is whitespace-only and never involves a shell.
func (p *MCPPool) dialCommand(spec core.MCPServerConfig) (mcp.Transport, error) {
	argv := strings.Fields(spec.Command)
	if len(argv) == 0 {
		return nil, errors.New("command 为空")
	}
	// #nosec G204 -- 豁免理由：① argv 只来自管理员登记的 mcp_servers.yaml（受信配置面，加载期已拒绝式校验），command 仅按空白切分、逐项直执不经 shell，配置字面量不能变成可注入命令行；② G204 只接受常量实参，配置驱动 argv 静态不可见，无法用代码消除（陷阱 #56）；③ 消除它只能改走 os.StartProcess 或手工拼 exec.Cmd（掩盖审计点、偏离颗粒度文档 §2.3 的 exec.Command 决定），二者都更差（用户 2026-10-17 裁决）。
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = p.root
	cmd.Env = mergeChildEnv(p.envPassthrough, spec.Env)
	return &mcp.CommandTransport{Command: cmd}, nil
}

// toolLister 是 `listAllTools` 的最小依赖面：`*mcp.ClientSession` 天然满足，测试可注入
// 分页脚本（`fakeToolLister`）。
// toolLister is the minimal lister surface; a session satisfies it naturally.
type toolLister interface {
	ListTools(ctx context.Context, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error)
}

// listAllTools 以游标驱动分页直到空游标；同一游标第二次出现 → 失败，**不得**无限循环。
// listAllTools paginates to an empty cursor and fails on a repeated cursor instead of
// looping forever.
func listAllTools(ctx context.Context, lister toolLister) ([]*mcp.Tool, error) {
	var all []*mcp.Tool
	cursor := ""
	seen := make(map[string]bool)
	for {
		page, err := lister.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		all = append(all, page.Tools...)
		cursor = page.NextCursor
		if cursor == "" {
			return all, nil
		}
		if seen[cursor] {
			return nil, errors.New("tools/list 游标重复——终止分页")
		}
		seen[cursor] = true
	}
}

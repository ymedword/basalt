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
	"net/http"
	"net/url"
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
	// mcpTransportStdio / mcpTransportHTTP 与配置解析层的字面量逐字一致：空串只对既有
	// 文件按 stdio 解释，候选写入必须显式给出二者之一（约束 8）。
	// The transport literals mirror the parser; only legacy files may omit them.
	mcpTransportStdio = "stdio"
	mcpTransportHTTP  = "http"
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
	// httpClient 是 http 传输共用的出站客户端：**不跟随重定向**——白名单按初始 URL
	// 判定，跟随 3xx 会让一次重定向把请求带到未授权域名（约束 20 的硬边界）。
	// 其余超时由各调用 ctx 承载（握手预算 / 工具执行期限），客户端自身不设总超时。
	// httpClient refuses redirects so a 3xx cannot carry the request to a domain that
	// never passed the allowlist check.
	httpClient *http.Client
	dial       func(core.MCPServerConfig) (mcp.Transport, error)

	// sessions 与 cmds 同序：cmds[i] 是第 i 个会话的本地子进程（http 或测试注入传输为 nil）。
	// 进程树回收要拿得到 *exec.Cmd，而 SDK 在 Connect 内部才 Start——故在拨号返回的
	// `*mcp.CommandTransport` 上取，成功连接后与会话一并登记。
	// sessions and cmds are parallel: cmds[i] is the local child of session i, or nil.
	mu       sync.Mutex
	sessions []*mcp.ClientSession
	cmds     []*exec.Cmd
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
		httpClient: &http.Client{
			// 3xx 原样返回给协议层，由它按失败处理；不得跟随后再判白名单。
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	pool.dial = pool.dialTransport
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
		cmd := commandOfTransport(transport)
		if err != nil {
			// 清理失败不改变「当前 server 失败已吸收、继续下一个」的判定，但必须留痕：
			// 残留进程树是运维要看得见的事实，不得静默丢弃。
			// A failed cleanup never changes the absorbed-failure decision, but it is
			// still reported so leftover process trees stay visible to operators.
			if _, killErr := killTreeAfterFailure(cmd); killErr != nil {
				p.logger.Warn("MCP server 进程树清理失败", "server", spec.Name,
					"detail", safeErrorSummary(killErr))
			}
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
			// SDK 在 Connect 失败路径已关闭连接（传输失败时根本没有进程），这里只兜底
			// 清理可能已派生的进程树——它同样属于"stdio 由 supervisor 持有"的边界。
			// 清理失败照上面同口径 WARN 留痕，不影响「跳过并继续」的判定。
			if _, killErr := killTreeAfterFailure(cmd); killErr != nil {
				p.logger.Warn("MCP server 进程树清理失败", "server", spec.Name,
					"detail", safeErrorSummary(killErr))
			}
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
			// 先整树、再关会话：Windows 上父进程一死，子进程关系即消失（见 closePooledSession）。
			closeErr := closePooledSession(session, cmd)
			cancel()
			if closeErr != nil {
				p.logger.Warn("MCP server 收尾失败", "server", spec.Name,
					"detail", safeErrorSummary(closeErr))
			}
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
		p.cmds = append(p.cmds, cmd)
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

// Close 关闭池内全部会话并清理每个 stdio 子进程的**整棵树**（幂等：第二次取到的会话集为空）；
// 逐个错误合并返回、不静默。
// Close shuts down every pooled session, kills each stdio child's whole process tree, and
// is a no-op when called again.
func (p *MCPPool) Close() error {
	p.mu.Lock()
	sessions := p.sessions
	cmds := p.cmds
	p.sessions = nil
	p.cmds = nil
	p.mu.Unlock()
	var errs []error
	for i, session := range sessions {
		var cmd *exec.Cmd
		if i < len(cmds) {
			cmd = cmds[i]
		}
		errs = append(errs, closePooledSession(session, cmd))
	}
	return errors.Join(errs...)
}

// closePooledSession 收尾一条已建立会话：**先整树终止、再关会话**。顺序不可反转——
// Windows 上父进程一死，`taskkill /T` 就再也走不到孙进程，先关会话等于放走整棵子树。
// 主动终止是预期路径：若整树已由本函数杀掉，SDK Close 等到的非零退出码 / 管道错误是
// 终止本身的后果，不再重复上报；清理本身失败（killErr）照常单列。
// closePooledSession kills the tree before closing the session: once the parent dies,
// Windows can no longer walk to grandchildren. Exit errors caused by our own kill are
// expected and are not re-reported; a failed cleanup is.
func closePooledSession(session *mcp.ClientSession, cmd *exec.Cmd) error {
	killed, killErr := killTreeAfterFailure(cmd)
	closeErr := session.Close()
	if killed {
		closeErr = nil
	}
	return errors.Join(killErr, closeErr)
}

// killTreeAfterFailure 在失败 / 停止路径上尽力清理 stdio 子进程树：返回是否真的杀掉
// （用于判定会话关闭错误是否为我们的终止所致）与清理错误。没有本地进程（http / 测试
// 注入传输）或进程已被收尸时是空操作。
// killTreeAfterFailure best-effort kills the stdio child tree and reports whether it did.
func killTreeAfterFailure(cmd *exec.Cmd) (bool, error) {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return false, nil
	}
	if err := killMCPProcessTree(cmd); err != nil {
		return false, err
	}
	return true, nil
}

// commandOfTransport 从拨号结果里取出本地子进程句柄；非命令传输（http / 测试注入）
// 返回 nil——回收逻辑据此把"零进程"表达为 nil 而不是特判传输类型。
// commandOfTransport returns the local child behind a dial result, nil for non-command
// transports such as http.
func commandOfTransport(transport mcp.Transport) *exec.Cmd {
	commandTransport, ok := transport.(*mcp.CommandTransport)
	if !ok {
		return nil
	}
	return commandTransport.Command
}

// dialCommand 构造真实子进程传输：`Command` 是**单个可执行名**，参数只从 `Args` 逐项
// 原样进入 argv（约束 8——命令与参数已在解析层分开，本层**不得**再按空白拆分，
// 否则解析层的严格校验会被运行时悄悄绕过）；`exec.Command` 而不是
// `exec.CommandContext`——进程生命周期不绑握手预算（约束 27）；工作目录 = 工作区根，
// 环境 = 白名单透传 + 配置声明（约束 17 / 18）。
// dialCommand builds the real subprocess transport. Command is one executable name and
// Args enter argv one-for-one; this layer never splits on whitespace. The process is
// not bound to any context.
func (p *MCPPool) dialCommand(spec core.MCPServerConfig) (mcp.Transport, error) {
	command := strings.TrimSpace(spec.Command)
	if command == "" {
		return nil, errors.New("command 为空")
	}
	// #nosec G204 -- 豁免理由：① argv 只来自管理员登记的 mcp_servers.yaml（受信配置面，加载期已拒绝式校验；command = 单可执行名、args 逐项直执），不经 shell，配置字面量不能变成可注入命令行；② G204 只接受常量实参，配置驱动 argv 静态不可见，无法用代码消除（陷阱 #56）；③ 消除它只能改走 os.StartProcess 或手工拼 exec.Cmd（掩盖审计点、偏离颗粒度文档 §2.3 的 exec.Command 决定），二者都更差（用户 2026-10-17 裁决）。
	cmd := exec.Command(command, spec.Args...)
	cmd.Dir = p.root
	cmd.Env = mergeChildEnv(p.envPassthrough, spec.Env)
	// Unix 上先把子进程放进独立进程组，停止时才能按组回收整棵树（约束 19 / 22）。
	configureMCPChildProcess(cmd)
	return &mcp.CommandTransport{Command: cmd}, nil
}

// dialTransport 按配置声明的传输类型选择拨号方式（约束 8 / 19）：空串只对既有文件
// 按 stdio 解释；`stdio` 起子进程、`http` 建 streamable HTTP 客户端。解析层已拒绝
// 表外取值，这里再兜底一次——池也可能被直接构造。
// dialTransport dispatches on the declared transport; an empty value means legacy
// stdio, and anything outside the two literals is rejected.
func (p *MCPPool) dialTransport(spec core.MCPServerConfig) (mcp.Transport, error) {
	switch spec.Transport {
	case "", mcpTransportStdio:
		return p.dialCommand(spec)
	case mcpTransportHTTP:
		return p.dialHTTP(spec)
	default:
		return nil, errors.New("未知的 MCP 传输类型")
	}
}

// dialHTTP 构造 streamable HTTP 传输：**零子进程**，只持有客户端连接（约束 19）。
// 拨号前做最低限度的形状校验——绝对 URL、`http` / `https`、无 userinfo；域名白名单
// 与端口档由 `Policy.Check(ActionHTTPRequest, url)` 在 supervisor 连接前判定
// （约束 20），本层不重复实现、也不放宽。重定向由池级客户端拒绝（见 `httpClient`）。
// dialHTTP builds the streamable HTTP transport: zero child processes. It checks only
// the URL shape; the allowlist and port rules stay in the policy layer.
func (p *MCPPool) dialHTTP(spec core.MCPServerConfig) (mcp.Transport, error) {
	raw := strings.TrimSpace(spec.URL)
	if raw == "" {
		return nil, errors.New("url 为空")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("url 无法解析")
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return nil, errors.New("url 必须是绝对地址")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("url 只接受 http / https")
	}
	if parsed.User != nil {
		return nil, errors.New("url 不得包含 userinfo")
	}
	return &mcp.StreamableClientTransport{
		Endpoint:   parsed.String(),
		HTTPClient: p.httpClient,
		// 连接失败不重试：握手预算（测试端点 15 秒 / 启动 30 秒）已界定一次尝试，
		// 后台退避重连会把失败判定拖到预算之外。负值按 SDK 口径关闭重试。
		MaxRetries: -1,
		// 本仓只用请求-响应（tools/list、tools/call），不消费服务端主动通知：
		// 不建立常驻 GET SSE，避免为一条永远不用的流维持长连接与重连。
		DisableStandaloneSSE: true,
	}, nil
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

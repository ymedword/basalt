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
	"os"
	"os/exec"
	"path/filepath"
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

// MCPConnectFailure 是一条 server 的**结构化**连接失败（契约 D-05）：`Stage` 说明
// 失败发生在哪一步，`Class` 说明属于哪一类可证明的失败。`Err` 只供日志与内部分类，
// 视图**不得**暴露原文（可能含路径与凭据）。
// MCPConnectFailure is one structured connect failure; Err is for logs only.
type MCPConnectFailure struct {
	Server string
	Stage  string
	Class  string
	Err    error
}

// 阶段与分类的固定字面量逐字对齐契约 D-05（`complete` 与 `env_missing` 由装配层判定，
// 连接池不产生）。
// Stage and class literals mirror contract D-05; complete and env_missing are decided
// by the assembly layer, never by the pool.
const (
	mcpStagePreflight  = "preflight"
	mcpStageSpawn      = "spawn"
	mcpStageInitialize = "initialize"
	mcpStageToolsList  = "tools_list"
)

const (
	mcpClassCommandNotFound       = "command_not_found"
	mcpClassPermissionDenied      = "permission_denied"
	mcpClassPolicyDenied          = "policy_denied"
	mcpClassTimeout               = "timeout"
	mcpClassHTTPUnauthorized      = "http_unauthorized"
	mcpClassHTTPUnreachable       = "http_unreachable"
	mcpClassHandshakeFailed       = "handshake_failed"
	mcpClassToolsListFailed       = "tools_list_failed"
	mcpClassDependencyUnconfirmed = "dependency_unconfirmed"
)

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
			// 非 2xx 在传输层变成可判别错误：SDK 对 401 / 403 只回字符串，
			// 按文案分类会让改一句话就漂移（契约 D-05 禁止解析错误文案）。
			Transport: mcpStatusTransport{base: http.DefaultTransport},
		},
	}
	pool.dial = pool.dialTransport
	return pool
}

// Connect 逐 server 串行连接并列出工具，按 specs 顺序返回连接结果。失败吸收（约束 28）：
// dial / initialize / 列工具任一步失败只影响当前 server——WARN 固定键点名 + 关闭半截会话 +
// 继续下一个；**只有整体 ctx 取消 / 截止**才作为池级错误返回（启动不中断）。内部委托
// `ConnectDetailed`，只丢弃结构化失败清单——启动语义与旧实现逐条一致。
// Connect dials, initializes and lists tools per spec; per-server failures are absorbed
// into a fixed-key warning and the loop continues. Only outer cancellation/deadline is an
// error. It delegates to ConnectDetailed and drops the structured failures.
func (p *MCPPool) Connect(ctx context.Context, specs []core.MCPServerConfig) ([]MCPConnection, error) {
	conns, _, err := p.ConnectDetailed(ctx, specs)
	return conns, err
}

// ConnectDetailed 与 `Connect` 同一套拨号 / 初始化 / 列工具流程，另把每条失败的
// server 记成 `MCPConnectFailure`（stage + class，契约 D-05）。分类只来自**结构化**
// 失败类型与进程状态，**不**解析 stderr / 日志文案。池级错误（外层取消 / 截止）仍是
// 第三个返回值：此刻尚未归因的 server 不写失败项——否则取消会被伪装成一条 server 失败。
// ConnectDetailed runs the same pipeline as Connect and also reports each failed server
// as a structured failure. Classification reads only typed errors and process state;
// outer cancellation returns a pool-level error without inventing a server failure.
func (p *MCPPool) ConnectDetailed(ctx context.Context, specs []core.MCPServerConfig) ([]MCPConnection, []MCPConnectFailure, error) {
	conns := make([]MCPConnection, 0, len(specs))
	failures := make([]MCPConnectFailure, 0, len(specs))
	for _, spec := range specs {
		// 取消 / 截止是池级信号：后面不必再看（也已无预算可言）。
		if err := ctx.Err(); err != nil {
			return conns, failures, err
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
				return conns, failures, ctxErr
			}
			stage, class := classifyDialFailure(err)
			failures = append(failures, MCPConnectFailure{Server: spec.Name, Stage: stage, Class: class, Err: err})
			continue
		}
		// 预算只罩下面两步；到点之前会话已建立、子进程已存活。
		cctx, cancel := context.WithTimeout(ctx, p.connectTimeout)
		session, err := p.client.Connect(cctx, transport, nil)
		// 预算先取后放：cancel 之后 cctx.Err() 恒为非 nil，会把真实分类抹平成超时。
		budgetErr := cctx.Err()
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
				return conns, failures, ctxErr
			}
			stage, class := classifyInitializeFailure(err, cmd, budgetErr)
			failures = append(failures, MCPConnectFailure{Server: spec.Name, Stage: stage, Class: class, Err: err})
			continue
		}
		tools, err := listAllTools(cctx, session)
		budgetErr = cctx.Err()
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
				return conns, failures, ctxErr
			}
			class := mcpClassToolsListFailed
			if errors.Is(budgetErr, context.DeadlineExceeded) {
				class = mcpClassTimeout
			}
			failures = append(failures, MCPConnectFailure{
				Server: spec.Name, Stage: mcpStageToolsList, Class: class, Err: err,
			})
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
		return conns, failures, err
	}
	return conns, failures, nil
}

// classifyDialFailure 归类拨号阶段（preflight / spawn）的失败：策略拒绝来自预检，
// 其余只看错误类型——**不**看文案。`fs.ErrNotExist` 刻意不归为"命令不存在"：
// 带分隔符的相对入口也可能只是工作目录不在了，无法证明的东西一律 unconfirmed
// （契约 C-06 的"不得把没有成功连接统一推断为未安装"）。
// classifyDialFailure maps dial-stage errors by type only. fs.ErrNotExist stays
// unconfirmed because a missing cwd is indistinguishable from a missing binary here.
func classifyDialFailure(err error) (string, string) {
	switch {
	case errors.Is(err, core.ErrPolicyDenied):
		return mcpStagePreflight, mcpClassPolicyDenied
	case errors.Is(err, exec.ErrNotFound):
		return mcpStageSpawn, mcpClassCommandNotFound
	case errors.Is(err, fs.ErrPermission):
		return mcpStageSpawn, mcpClassPermissionDenied
	case errors.Is(err, context.DeadlineExceeded):
		return mcpStageSpawn, mcpClassTimeout
	default:
		return mcpStageSpawn, mcpClassDependencyUnconfirmed
	}
}

// classifyInitializeFailure 归类初始化阶段（spawn / initialize）的失败。判别顺序即
// 判据强度：先认策略拒绝与 HTTP 状态（类型化），再认网络不可达（`*url.Error`），
// 再认超时；进程始终没起来 → 启动阶段未归因；起来了但握手没过 → 握手失败。
// `budgetErr` 是握手预算的终态（必须在 cancel 之前取值）。
// classifyInitializeFailure orders checks by evidence strength; budgetErr must be
// captured before the budget is canceled.
func classifyInitializeFailure(err error, cmd *exec.Cmd, budgetErr error) (string, string) {
	if errors.Is(err, core.ErrPolicyDenied) {
		return mcpStagePreflight, mcpClassPolicyDenied
	}
	var statusErr *mcpHTTPStatusError
	if errors.As(err, &statusErr) &&
		(statusErr.Status == http.StatusUnauthorized || statusErr.Status == http.StatusForbidden) {
		return mcpStageInitialize, mcpClassHTTPUnauthorized
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return mcpStageInitialize, mcpClassHTTPUnreachable
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(budgetErr, context.DeadlineExceeded) {
		// 子进程根本没起来时的超时属于启动阶段；否则是握手预算用完。
		if cmd != nil && cmd.Process == nil {
			return mcpStageSpawn, mcpClassTimeout
		}
		return mcpStageInitialize, mcpClassTimeout
	}
	if cmd != nil && cmd.Process == nil {
		// 命令传输存在、进程却没起来：按**结构化**证据归因（颗粒度文档 §2.4.3 第
		// 668–670 行）。LookPath 失败（`exec.ErrNotFound`）确定是命令不存在；`ENOENT`
		// 还要亲眼看住命令文件缺失——同一错误码也可能是工作目录或 shebang 解释器
		// 缺失（Linux 上同码）；权限错误按权限不足。仍无法归因才落 unconfirmed，
		// 不猜"未安装"（契约 C-06）。
		// Child never started: attribute from typed evidence only; ENOENT additionally
		// requires observing the command file missing (cwd / shebang interpreter can
		// produce the same errno), otherwise stay unconfirmed.
		switch {
		case errors.Is(err, exec.ErrNotFound):
			return mcpStageSpawn, mcpClassCommandNotFound
		case errors.Is(err, fs.ErrNotExist) && commandFileMissing(cmd):
			return mcpStageSpawn, mcpClassCommandNotFound
		case errors.Is(err, fs.ErrPermission):
			return mcpStageSpawn, mcpClassPermissionDenied
		}
		return mcpStageSpawn, mcpClassDependencyUnconfirmed
	}
	return mcpStageInitialize, mcpClassHandshakeFailed
}

// commandFileMissing 判断启动失败是否可归因到「命令文件本身不存在」：`fs.ErrNotExist`
// 在子进程未起来时并不专属于命令文件——工作目录不存在（Linux 上 chdir 与 execve 同报
// ENOENT）、shebang 解释器缺失、文件在 LookPath 之后被删都可能给出同一错误码。只有
// 命令文件经 `os.Stat` 确认不存在、且工作目录确实存在时才敢报 `command_not_found`；
// 其余一律交回调用方落 `dependency_unconfirmed`。相对入口按子进程的工作目录（`cmd.Dir`）
// 解析——父进程的 cwd 不是它的 cwd。
// commandFileMissing reports whether the missing path is provably the command file:
// ENOENT alone is ambiguous, so the file must stat as non-existent while the child's
// working directory exists. Relative entries resolve against cmd.Dir, not the parent cwd.
func commandFileMissing(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Path == "" {
		return false
	}
	if cmd.Dir != "" {
		if _, err := os.Stat(cmd.Dir); err != nil {
			// 工作目录本身不可用（缺失 / 无权限）：错误无法归因到命令文件。
			return false
		}
	}
	path := cmd.Path
	if !filepath.IsAbs(path) && cmd.Dir != "" {
		path = filepath.Join(cmd.Dir, path)
	}
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// mcpHTTPStatusError 把 streamable HTTP 的非 2xx 响应变成**可判别**类型：SDK 对
// 401 / 403 只返回字符串，类型化后才满足"分类不解析错误文案"的硬边界。
// mcpHTTPStatusError makes non-2xx responses typeable so classification never parses
// error strings.
type mcpHTTPStatusError struct {
	Status int
}

// Error 只写状态码与标准短语；不携带响应体（可能含服务端细节）。
// Error reports the status code and standard phrase only.
func (e *mcpHTTPStatusError) Error() string {
	return fmt.Sprintf("MCP HTTP 响应状态异常: %d %s", e.Status, http.StatusText(e.Status))
}

// mcpStatusTransport 在传输层拦非 2xx：响应体读完关闭（不参与协议解析，也不泄漏
// 连接），再返回类型化错误。`base` 为空时用 `http.DefaultTransport`。
// mcpStatusTransport turns non-2xx responses into typed errors and closes the body.
type mcpStatusTransport struct {
	base http.RoundTripper
}

// RoundTrip 透传 2xx，非 2xx 返回 `*mcpHTTPStatusError`。
// RoundTrip passes 2xx through and returns a typed error otherwise.
func (t mcpStatusTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return resp, nil
	}
	// 非 2xx 的正文对协议层没有价值，按 RoundTripper 契约关闭；关闭本身失败时
	// 与状态错误合并上报，不静默丢弃。
	statusErr := &mcpHTTPStatusError{Status: resp.StatusCode}
	if closeErr := resp.Body.Close(); closeErr != nil {
		return nil, errors.Join(statusErr, closeErr)
	}
	return nil, statusErr
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

// NewMCPCommand 构造一次 stdio 启动的 `*exec.Cmd`，是**启动与诊断共用的唯一构造器**
// （契约 C-04）：诊断只读 `cmd.Err` / `cmd.Path` / `os.Stat`，绝不 `Start`；`dialCommand`
// 也经它构造，两处字段永远同源。`Command` 是**单个可执行名**，参数只从 `Args` 逐项
// 原样进入 argv（约束 8——命令与参数已在解析层分开，本层**不得**再按空白拆分，
// 否则解析层的严格校验会被运行时悄悄绕过）；`exec.Command` 而不是
// `exec.CommandContext`——进程生命周期不绑握手预算（约束 27）；工作目录 = 工作区根，
// 环境 = 白名单透传 + 配置声明（约束 17 / 18）。
// NewMCPCommand builds the stdio *exec.Cmd shared by launch and diagnosis. Command is
// one executable name and Args enter argv one-for-one; nothing here starts a process.
func NewMCPCommand(spec core.MCPServerConfig, root string, envPassthrough []string) (*exec.Cmd, error) {
	command := strings.TrimSpace(spec.Command)
	if command == "" {
		return nil, errors.New("command 为空")
	}
	// #nosec G204 -- 豁免理由：① argv 只来自管理员登记的 mcp_servers.yaml（受信配置面，加载期已拒绝式校验；command = 单可执行名、args 逐项直执），不经 shell，配置字面量不能变成可注入命令行；② G204 只接受常量实参，配置驱动 argv 静态不可见，无法用代码消除（陷阱 #56）；③ 消除它只能改走 os.StartProcess 或手工拼 exec.Cmd（掩盖审计点、偏离颗粒度文档 §2.3 的 exec.Command 决定），二者都更差（用户 2026-10-17 裁决）。
	cmd := exec.Command(command, spec.Args...)
	cmd.Dir = root
	cmd.Env = mergeChildEnv(envPassthrough, spec.Env)
	// Unix 上先把子进程放进独立进程组，停止时才能按组回收整棵树（约束 19 / 22）。
	configureMCPChildProcess(cmd)
	return cmd, nil
}

// dialCommand 用共享构造器建出命令后包成命令传输——**不复制**构造逻辑（契约 C-04）。
// dialCommand wraps the shared constructor; it never duplicates the construction.
func (p *MCPPool) dialCommand(spec core.MCPServerConfig) (mcp.Transport, error) {
	cmd, err := NewMCPCommand(spec, p.root, p.envPassthrough)
	if err != nil {
		return nil, err
	}
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

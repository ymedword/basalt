package tool

// 本文件是 US-7 MCP 池与适配器的白盒用例（颗粒度文档 §3.2 的 tool/mcp_test.go 十八例，
// 至此全部落地：成功路径六例 + 故障隔离三例 + 子进程环境与协议错误两例 + US4 七例）。
// `package tool` 可直接替换非导出 `dial` 注入点、调用非导出 `newMCPToolAdapter`——
// **不新增导出测试 API**。
// White-box tests for the MCP pool and adapter; the package-internal dial seam and the
// unexported adapter constructor are used directly instead of adding exported seams.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/testutil"
)

// logBuffer 是并发安全的日志缓冲（slog.TextHandler 的写端）。
// logBuffer is a concurrency-safe sink for the text handler.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write 追加一段日志字节。
// Write appends one log record.
func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String 取当前全部日志文本。
// String returns everything captured so far.
func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogger 造一只写入内存缓冲的 logger 与取文句柄：WARN / INFO 两轨都经它断言
// （R2 的 "WARN 半轨"判据）。
// captureLogger returns a logger backed by an in-memory buffer plus the buffer.
func captureLogger(t *testing.T) (*slog.Logger, *logBuffer) {
	t.Helper()
	buffer := &logBuffer{}
	return slog.New(slog.NewTextHandler(buffer, nil)), buffer
}

// connectTestSession 以工具包同一套客户端形态连上给定传输并登记清理（白盒用例共用）。
// connectTestSession connects a client to the given transport and registers cleanup.
func connectTestSession(t *testing.T, transport mcp.Transport) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "basalt-test"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	return session
}

// testAdapter 用最小 `*mcp.Tool` 构造适配器——测的是适配器对会话的调用与渲染，
// 不是 server 侧的 schema 校验。
// testAdapter builds an adapter around a minimal tool descriptor.
func testAdapter(t *testing.T, session *mcp.ClientSession, server, name string) *mcpToolAdapter {
	t.Helper()
	return newMCPToolAdapter(session, server, &mcp.Tool{
		Name:        name,
		InputSchema: map[string]any{"type": "object"},
	})
}

// TestMCPPool_ConnectReturnsConnectionsInSpecOrder：连接结果按 specs 顺序返回（连接序 =
// 文件序 = 注册序的池级判据），每条连接的工具名原样来自 server。
// Connections come back in spec order, one entry per server.
func TestMCPPool_ConnectReturnsConnectionsInSpecOrder(t *testing.T) {
	transports := make(map[string]mcp.Transport, 2)
	for _, name := range []string{"alpha", "beta"} {
		toolName := name + "_tool"
		transports[name] = testutil.NewInMemoryMCPPair(t, 0, testutil.MCPTestTool{
			Name:   toolName,
			Schema: map[string]any{"type": "object"},
			Handler: func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: toolName}}}, nil
			},
		})
	}
	logger, _ := captureLogger(t)
	pool := NewMCPPool(MCPPoolOptions{Logger: logger, Root: t.TempDir(), Version: "test"})
	pool.dial = func(spec core.MCPServerConfig) (mcp.Transport, error) {
		return transports[spec.Name], nil
	}
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{
		{Name: "beta", Command: "beta-mcp"},
		{Name: "alpha", Command: "alpha-mcp"},
	})
	require.NoError(t, err)
	require.Len(t, conns, 2)
	require.Equal(t, "beta", conns[0].Server)
	require.Equal(t, "alpha", conns[1].Server)
	require.Len(t, conns[0].Tools, 1)
	require.Equal(t, "beta_tool", conns[0].Tools[0].Name())
	require.Len(t, conns[1].Tools, 1)
	require.Equal(t, "alpha_tool", conns[1].Tools[0].Name())
}

// TestMCPPool_ListToolsPaginatesToEmptyCursor（**R7 前半**）：PageSize = 1 + 3 工具——
// 三页必须全部列出；逆改点：只取第一页 → 工具数断言红。
// R7 front half: with a page size of one, all three tools must still be listed.
func TestMCPPool_ListToolsPaginatesToEmptyCursor(t *testing.T) {
	transport := testutil.NewInMemoryMCPPair(t, 1, testutil.StandardMCPTools()...) // 每页 1 件
	logger, _ := captureLogger(t)
	pool := NewMCPPool(MCPPoolOptions{Logger: logger, Root: t.TempDir()})
	pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
		return transport, nil
	}
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{Name: "paged", Command: "unused"}})
	require.NoError(t, err)
	require.Len(t, conns, 1)
	require.Len(t, conns[0].Tools, 3, "三页必须全部列出")
	names := make([]string, 0, len(conns[0].Tools))
	for _, adapter := range conns[0].Tools {
		names = append(names, adapter.Name())
	}
	sort.Strings(names)
	require.Equal(t, []string{"blob", "boom", "echo"}, names)
}

// TestMCPToolAdapter_RendersTextBlocksJoined：多个文本块按序以换行连接（§6.2 口径 11）。
// Text blocks are rendered in order, joined with a newline.
func TestMCPToolAdapter_RendersTextBlocksJoined(t *testing.T) {
	transport := testutil.NewInMemoryMCPPair(t, 0, testutil.MCPTestTool{
		Name:   "multi",
		Schema: map[string]any{"type": "object"},
		Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "first"},
				&mcp.TextContent{Text: "second"},
			}}, nil
		},
	})
	session := connectTestSession(t, transport)
	adapter := testAdapter(t, session, "probe", "multi")

	result, err := adapter.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, "first\nsecond", result.Content)
	require.False(t, result.Retryable)
}

// TestMCPToolAdapter_RendersNonTextAsPlaceholderNoBase64：非文本块渲染为类型化占位符，
// base64 原文不得进入模型上下文（注 3 / 约束 12）。
// Non-text blocks become typed placeholders; the base64 payload never reaches the model.
func TestMCPToolAdapter_RendersNonTextAsPlaceholderNoBase64(t *testing.T) {
	transport := testutil.NewInMemoryMCPPair(t, 0, testutil.MCPTestTool{
		Name:   "blob",
		Schema: map[string]any{"type": "object"},
		Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{
				MIMEType: "image/png",
				Data:     []byte("aGVsbG8="),
			}}}, nil
		},
	})
	session := connectTestSession(t, transport)
	adapter := testAdapter(t, session, "probe", "blob")

	result, err := adapter.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, "[非文本内容：image image/png]", result.Content)
	require.NotContains(t, result.Content, "aGVsbG8=", "base64 原文不得进入上下文")
}

// TestMCPToolAdapter_FallsBackToStructuredContentAndNoOutput：无文本块时用 StructuredContent
// 的紧凑 JSON；连结构化内容也没有时回落固定兜底文案（§6.2 口径 11）。
// Falls back to compact structured-content JSON, then to the fixed no-output wording.
func TestMCPToolAdapter_FallsBackToStructuredContentAndNoOutput(t *testing.T) {
	transport := testutil.NewInMemoryMCPPair(t, 0,
		testutil.MCPTestTool{
			Name:   "structured",
			Schema: map[string]any{"type": "object"},
			Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{StructuredContent: map[string]any{"ok": true}}, nil
			},
		},
		testutil.MCPTestTool{
			Name:   "empty",
			Schema: map[string]any{"type": "object"},
			Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			},
		},
	)
	session := connectTestSession(t, transport)

	structured, err := testAdapter(t, session, "probe", "structured").Execute(
		context.Background(), core.ToolEnv{}, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, `{"ok":true}`, structured.Content)

	empty, err := testAdapter(t, session, "probe", "empty").Execute(
		context.Background(), core.ToolEnv{}, json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, "(MCP 工具无输出)", empty.Content)
}

// TestMCPToolAdapter_InputObjectAndEmptyInput：合法 JSON 对象原样字节透传；空串与 `null`
// 一律折算为空对象 `{}`（**不得**把 null 交给 server）。
// Object input passes through verbatim; empty and null both become the empty object.
func TestMCPToolAdapter_InputObjectAndEmptyInput(t *testing.T) {
	echoArgs := testutil.MCPTestTool{
		Name:   "args",
		Schema: map[string]any{"type": "object"},
		Handler: func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: string(req.Params.Arguments)},
			}}, nil
		},
	}
	transport := testutil.NewInMemoryMCPPair(t, 0, echoArgs)
	session := connectTestSession(t, transport)
	adapter := testAdapter(t, session, "probe", "args")

	object, err := adapter.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{"text":"hi"}`))
	require.NoError(t, err)
	require.Equal(t, `{"text":"hi"}`, object.Content)

	empty, err := adapter.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(``))
	require.NoError(t, err)
	require.Equal(t, `{}`, empty.Content, "空入参必须折算为空对象")

	nullInput, err := adapter.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`null`))
	require.NoError(t, err)
	require.Equal(t, `{}`, nullInput.Content, "null 不得原样交给 server")
}

// TestMCPPool_ConnectSkipsFailedServerAndKeepsOthers（**R2**）：dial 失败只影响自己——
// WARN 固定键点名 + 其余连接照常保留 + `Connect` 无错返回（启动不中断）。
// R2: one failed dial is skipped with a fixed-key warning; the other connections survive.
func TestMCPPool_ConnectSkipsFailedServerAndKeepsOthers(t *testing.T) {
	transport := testutil.NewInMemoryMCPPair(t, 0, testutil.MCPTestTool{
		Name:   "good_tool",
		Schema: map[string]any{"type": "object"},
		Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		},
	})
	logger, logs := captureLogger(t)
	pool := NewMCPPool(MCPPoolOptions{Logger: logger, Root: t.TempDir(), Version: "test"})
	pool.dial = func(spec core.MCPServerConfig) (mcp.Transport, error) {
		if spec.Name == "broken" {
			return nil, &exec.Error{Name: "ghost-mcp", Err: exec.ErrNotFound}
		}
		return transport, nil
	}
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{
		{Name: "broken", Command: "ghost-mcp"},
		{Name: "good", Command: "good-mcp"},
	})
	require.NoError(t, err, "单 server 失败不得让 Connect 返回错误")
	require.Len(t, conns, 1, "失败的 server 跳过，其余连接照常返回")
	require.Equal(t, "good", conns[0].Server)
	require.Len(t, conns[0].Tools, 1)
	require.Equal(t, "good_tool", conns[0].Tools[0].Name())

	output := logs.String()
	require.Contains(t, output, "MCP server 未连接——已跳过")
	require.Contains(t, output, "server=broken")
	require.Contains(t, output, "reason=启动失败")
	require.Contains(t, output, "detail=可执行文件不存在")
}

// initializeFailingTransport 是「dial 成功、初始化失败」的传输：连接可建立，但首个读
// 立刻失败，SDK 据此走 Connect 失败路径并关闭连接。
// initializeFailingTransport yields a connection whose first read fails, so the SDK
// handshake fails after a successful dial.
type initializeFailingTransport struct {
	conn *initializeFailingConn
}

// Connect 返回记录用的假连接（dial 本身成功）。
// Connect returns the recording connection: the dial step succeeds.
func (t *initializeFailingTransport) Connect(context.Context) (mcp.Connection, error) {
	return t.conn, nil
}

// initializeFailingConn 记录 Close 是否被调用，并提供必然失败的读。
// initializeFailingConn records Close and always fails on read.
type initializeFailingConn struct {
	closed atomic.Bool
}

// Read 以固定错误结束握手。
// Read ends the handshake with a fixed error.
func (c *initializeFailingConn) Read(context.Context) (jsonrpc.Message, error) {
	return nil, errors.New("initialize boom")
}

// Write 丢弃消息（握手请求写不进去也无妨）。
// Write discards messages.
func (c *initializeFailingConn) Write(context.Context, jsonrpc.Message) error { return nil }

// Close 记录关闭。
// Close records the closure.
func (c *initializeFailingConn) Close() error {
	c.closed.Store(true)
	return nil
}

// SessionID 返回空串（无会话标识）。
// SessionID returns an empty id.
func (c *initializeFailingConn) SessionID() string { return "" }

// TestMCPPool_ConnectFailureClosesPartialSession：dial 成功但 initialize 失败 → 半截会话
// 必须被关闭（子进程随之收尾），该 server 跳过而不是让 Connect 返回错误。
// A failed initialize closes the half-open session and skips the server.
func TestMCPPool_ConnectFailureClosesPartialSession(t *testing.T) {
	conn := &initializeFailingConn{}
	logger, logs := captureLogger(t)
	pool := NewMCPPool(MCPPoolOptions{Logger: logger, Root: t.TempDir(), Version: "test"})
	pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
		return &initializeFailingTransport{conn: conn}, nil
	}
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{
		{Name: "half", Command: "half-mcp"},
	})
	require.NoError(t, err)
	require.Empty(t, conns)
	require.True(t, conn.closed.Load(), "初始化失败必须关闭半截会话")
	require.Contains(t, logs.String(), "reason=初始化失败")
}

// TestMCPPool_ConnectFailureLogOmitsSensitiveDetail（**R12 前半**）：dial 错误原文含哨兵时，
// 日志只出现 `safeErrorSummary` 固定短语——`err.Error()` 单行化 / 截断都不是脱敏。
// R12 front half: a sentinel embedded in the dial error must never reach the log; only
// the fixed safeErrorSummary phrase is emitted.
func TestMCPPool_ConnectFailureLogOmitsSensitiveDetail(t *testing.T) {
	const secret = "ghp_SENTINEL_9f2c7d"
	logger, logs := captureLogger(t)
	pool := NewMCPPool(MCPPoolOptions{Logger: logger, Root: t.TempDir(), Version: "test"})
	pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
		return nil, fmt.Errorf("exec %s: %w", secret, exec.ErrNotFound)
	}
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{
		{Name: "leaky", Command: "leaky-mcp"},
	})
	require.NoError(t, err)
	require.Empty(t, conns)
	require.NotContains(t, logs.String(), secret, "原始错误正文不得进入日志")
	require.Contains(t, logs.String(), "detail=可执行文件不存在")
}

// TestMCPPool_DialCommandEnvAndDir（约束 17 / 18 / 19 / 27）：检查**构造出的** `exec.Cmd`，
// 不起真实子进程——command 与 args 分开进入 argv、工作目录 = 工作区根、环境 = 白名单透传
// + 声明覆盖（其余清空、永不为 nil）、由 `exec.Command` 创建（`Cancel` 为 nil，非
// `CommandContext`）。
// 逆改点：换回 `exec.CommandContext` → `Cancel` 断言红；拼完整父进程环境 → Env 等值断言红。
// Inspects the constructed exec.Cmd without starting a process: command plus args, the
// workspace root as Dir, whitelist-plus-declared environment, and a nil Cancel.
func TestMCPPool_DialCommandEnvAndDir(t *testing.T) {
	t.Setenv("MCP_PROBE_PASS", "process-pass")
	t.Setenv("MCP_PROBE_OVERRIDE", "process-override")
	root := t.TempDir()
	pool := NewMCPPool(MCPPoolOptions{
		Root:           root,
		EnvPassthrough: []string{"MCP_PROBE_PASS", "MCP_PROBE_OVERRIDE", "MCP_PROBE_MISSING"},
	})
	spec := core.MCPServerConfig{
		Name:    "gh",
		Command: "gh-mcp",
		Args:    []string{"--stdio", "extra"},
		Env: map[string]string{
			"MCP_PROBE_OVERRIDE": "declared-override",
			"GITHUB_TOKEN":       "tok-secret-value",
		},
	}

	transport, err := pool.dialCommand(spec)
	require.NoError(t, err, "command 与 args 分开传入，构造阶段不得失败")
	commandTransport, ok := transport.(*mcp.CommandTransport)
	require.True(t, ok, "dialCommand 必须返回 *mcp.CommandTransport")
	require.Equal(t, []string{"gh-mcp", "--stdio", "extra"}, commandTransport.Command.Args,
		"参数逐项进入 argv、无 shell 引号语义、不得包裹 cmd /c")
	require.Equal(t, root, commandTransport.Command.Dir, "子进程工作目录 = 工作区根")
	require.Equal(t, []string{
		"MCP_PROBE_PASS=process-pass", // 透传且未被声明覆盖
		"GITHUB_TOKEN=tok-secret-value",
		"MCP_PROBE_OVERRIDE=declared-override", // 声明覆盖同名透传键
	}, commandTransport.Command.Env, "透传白名单跳过覆盖键与未设置键；声明键排序后追加")
	require.Nil(t, commandTransport.Command.Cancel,
		"exec.Command 创建——Cancel 只在 CommandContext 下非 nil（进程不绑握手预算）")

	// 无透传、无声明：环境为**空而非 nil**（子进程不继承完整父进程环境，约束 17）。
	bare := NewMCPPool(MCPPoolOptions{Root: root})
	bareTransport, err := bare.dialCommand(core.MCPServerConfig{Name: "bare", Command: "bare-mcp"})
	require.NoError(t, err)
	bareCommand := bareTransport.(*mcp.CommandTransport).Command
	require.NotNil(t, bareCommand.Env, "结果永不为 nil（nil 会退化成继承父进程环境）")
	require.Empty(t, bareCommand.Env)
}

// TestMCPToolAdapter_ProtocolErrorOmitsSensitiveDetail（**R12 后半**）：server handler 以
// JSON-RPC 错误答复时 → 外部错误分类、Reason 固定自产文案；server 错误消息里的哨兵不得
// 出现在 `err.Error()`，原始错误只留在 `Cause`（只经 Unwrap 供分类与进程内排障）。
// 逆改点：把 err.Error() 拼进 Reason → NotContains(secret) 红；丢掉 Cause → Error(t, ee.Cause) 红。
// R12 back half: a protocol error maps to a fixed Reason; the raw server text survives only
// in Cause.
func TestMCPToolAdapter_ProtocolErrorOmitsSensitiveDetail(t *testing.T) {
	const secret = "ghp_protocol_must_not_leak"
	failing := testutil.MCPTestTool{
		Name:        "leak",
		Description: "handler 返回错误",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("server exploded: token=" + secret)
		},
	}
	transport := testutil.NewInMemoryMCPPair(t, 0, failing)
	adapter := testAdapter(t, connectTestSession(t, transport), "probe", "leak")

	_, err := adapter.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{}`))
	require.Error(t, err)
	require.True(t, core.IsExternalError(err))
	require.Equal(t, "MCP 调用失败：probe/leak", err.Error(), "Reason 必须是自产固定文案")
	require.NotContains(t, err.Error(), secret, "协议错误原文不得回显")
	var external *core.ExternalError
	require.ErrorAs(t, err, &external)
	require.Error(t, external.Cause, "原始错误保留在 Cause（只经 Unwrap，不进文案）")
}

// fakeToolLister 用脚本化的分页结果驱动 listAllTools——判的是游标推进逻辑本身，
// 不经过传输层。
// fakeToolLister drives listAllTools with scripted pages.
type fakeToolLister struct {
	pages []*mcp.ListToolsResult
	calls int
}

// ListTools 依次返回脚本页；脚本耗尽后返回空游标页。
// ListTools returns the scripted pages in order.
func (f *fakeToolLister) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	if f.calls >= len(f.pages) {
		return &mcp.ListToolsResult{}, nil
	}
	page := f.pages[f.calls]
	f.calls++
	return page, nil
}

// recordingAudit 是两阶段审计的记忆实现（与 core/tool_exec_test.go 的 execAudit 同型）：
// `BeginInvocation` 追加 pending 行并返回行号，`FinishInvocation` 原地回填终态。
// recordingAudit mirrors execAudit: begin appends a pending row, finish updates it in place.
type recordingAudit struct {
	mu   sync.Mutex
	rows []core.LLMCallStatus
}

// BeginInvocation 写一行 pending 并返回行号。
// BeginInvocation appends a pending row and returns its id.
func (a *recordingAudit) BeginInvocation(_ context.Context, _ core.ToolInvocation) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, core.LLMCallPending)
	return int64(len(a.rows)), nil
}

// FinishInvocation 把同一行回填为终态。
// FinishInvocation updates the same row to its terminal status.
func (a *recordingAudit) FinishInvocation(_ context.Context, id int64, res core.ToolInvocationResult) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id < 1 || int(id) > len(a.rows) {
		return fmt.Errorf("tool invocation row %d not found", id)
	}
	a.rows[id-1] = res.Status
	return nil
}

// count 是审计行数（0 行 = 执行之前的拒绝）。
// count returns the number of audit rows.
func (a *recordingAudit) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.rows)
}

// lastStatus 是最后一行当前状态。
// lastStatus returns the current status of the last row.
func (a *recordingAudit) lastStatus() core.LLMCallStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.rows) == 0 {
		return ""
	}
	return a.rows[len(a.rows)-1]
}

// TestMCPToolAdapter_MapsIsErrorToExternalError（**R5**）：MCP 侧报错不是成功；且 server
// 文本是外部正文，不得进 Reason / Cause / 日志。
// 逆改点：忽略 res.IsError 直接渲染内容 → Error 断言红（调用"成功"了）；
//
//	把 server 文本放进 Reason → NotContains(secret) 红；
//	把文本挂进 Cause → Cause == nil 断言红。
//
// R5: an IsError result is an external error; the server text never leaves the server.
func TestMCPToolAdapter_MapsIsErrorToExternalError(t *testing.T) {
	const secret = "ghp_iserror_must_not_leak"
	leaky := testutil.MCPTestTool{
		Name:        "boom",
		Description: "IsError + 敏感值",
		Schema:      map[string]any{"type": "object"},
		Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{
				&mcp.TextContent{Text: "server-side failure token=" + secret},
			}}, nil
		},
	}
	transport := testutil.NewInMemoryMCPPair(t, 0, leaky)
	session := connectTestSession(t, transport)
	adapter := testAdapter(t, session, "probe", "boom")

	_, err := adapter.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{}`))
	require.Error(t, err)
	require.True(t, core.IsExternalError(err))
	require.Equal(t, "MCP 工具返回错误：probe/boom", err.Error(), "Reason 必须是自产固定文案")
	require.NotContains(t, err.Error(), secret, "server 文本不得回显")
	require.NotContains(t, err.Error(), "server-side failure", "整段外部正文都不回显")
	var external *core.ExternalError
	require.ErrorAs(t, err, &external)
	require.Nil(t, external.Cause, "IsError 的 server 文本连 Cause 也不放")
	require.False(t, core.IsInvalidInput(err))
}

// TestMCPToolAdapter_ContextIdentityWinsOverExternalError（**R6**）：ctx 取消 → 返回 ctx
// 原错误（→ timeout），不得包成 ExternalError。用「进入适配器前已取消」的 ctx：结果确定
// （SDK 在发送前就查 ctx.Done），不赌 echo 的耗时。
// 逆改点：把所有 err 统一包成 &ExternalError{Cause: err} → errors.Is 仍真（Unwrap 穿透），
// 但 !IsExternalError 断言红——这个断言守的正是「取消的身份优先于错误来自哪个系统」。
// R6: cancellation identity wins over the external-error classification.
func TestMCPToolAdapter_ContextIdentityWinsOverExternalError(t *testing.T) {
	transport := testutil.NewInMemoryMCPPair(t, 0, testutil.StandardMCPTools()...)
	session := connectTestSession(t, transport)
	adapter := testAdapter(t, session, "probe", "echo")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := adapter.Execute(ctx, core.ToolEnv{}, json.RawMessage(`{"text":"late"}`))
	require.Error(t, err)
	require.True(t, errors.Is(err, context.Canceled))
	require.False(t, core.IsExternalError(err), "ctx 身份必须保留为裸 sentinel")
}

// TestMCPToolAdapter_CallIsAuditedBeforeExecute（**R9**）：宪法 V 在 MCP 工具上不打折——
// 执行前 pending 已落审计存储（生产 = SQLite 仓储），执行后同一行回填终态。**所有等待都有
// 界**：断言失败时用例给出可读原因，不挂死。
// 逆改点：适配器自己执行、绕过 ToolExecutor → 阻塞期间的 pending 查询红（没有行）；
// 执行后再插入第二行 → 终态行数断言红（一次调用两行）。
// R9: the two-phase audit is visible while the call is still blocked.
func TestMCPToolAdapter_CallIsAuditedBeforeExecute(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // 失败路径也放行 handler：会话关闭不被阻塞的调用拖住

	blocking := testutil.MCPTestTool{
		Name:        "slow",
		Description: "阻塞到放行",
		Schema:      map[string]any{"type": "object"},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done(): // 调用被取消 / 截止时同样退出（否则 handler 永远挂着）
				return nil, ctx.Err()
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "done"}}}, nil
		},
	}
	transport := testutil.NewInMemoryMCPPair(t, 0, blocking)
	adapter := testAdapter(t, connectTestSession(t, transport), "probe", "slow")

	audit := &recordingAudit{} // 与 core/tool_exec_test.go 的 execAudit 同型
	reg := NewRegistry(slog.New(slog.DiscardHandler))
	require.NoError(t, reg.Register(adapter))
	exec := core.NewToolExecutor(reg, audit, slog.New(slog.DiscardHandler))
	done := make(chan core.Result, 1) // 有缓冲：主 goroutine 提前失败时发送方不阻塞
	go func() {
		done <- exec.Execute(context.Background(),
			core.ToolEnv{RunID: "run-1", SessionID: "ses-1", Timeout: 5 * time.Second},
			[]string{"slow"}, core.ToolCall{Name: "slow", Arguments: `{}`})
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("工具调用未在预算内进入 server 处理函数")
	}
	require.Equal(t, 1, audit.count(), "执行期间审计行已存在")
	require.Equal(t, core.LLMCallPending, audit.lastStatus(),
		"工具执行期间审计行必须是 pending（fail-closed 的可见形态）")

	unblock()
	select {
	case res := <-done:
		require.Equal(t, "done", res.Content)
	case <-time.After(5 * time.Second):
		t.Fatal("工具调用未在预算内返回")
	}
	require.Equal(t, 1, audit.count(), "一次调用一行")
	require.Equal(t, core.LLMCallSuccess, audit.lastStatus(), "同一行回填终态")
}

// TestListAllTools_RepeatedCursorFails（**R7 后半**）：游标不前进 → 判失败，**不得**无限循环。
// 逆改点：删掉 seen 守卫 → 用例超时判红。
// R7 back half: a repeated cursor fails instead of looping forever.
func TestListAllTools_RepeatedCursorFails(t *testing.T) {
	lister := &fakeToolLister{pages: []*mcp.ListToolsResult{
		{NextCursor: "c1", Tools: []*mcp.Tool{{Name: "a"}}},
		{NextCursor: "c1", Tools: []*mcp.Tool{{Name: "b"}}}, // 游标不前进
	}}
	_, err := listAllTools(context.Background(), lister)
	require.Error(t, err)
	require.Contains(t, err.Error(), "游标重复")
}

// TestMCPPool_CloseIsIdempotent（**R10 前半**）：Close 幂等，且**确实关闭了会话**。
// 逆改点：Close 完全不关闭会话（直接 return nil）→ 限定时间内 Wait() 不返回，第三条断言红。
// 注：SDK 自身的 ClientSession.Close 是幂等的，因此"去掉会话清空 → 第二次 Close 红"不成立；
// 幂等断言保留，但判别力由"关闭可观察"承担。
// R10 front half: Close is idempotent and observably closes the sessions.
func TestMCPPool_CloseIsIdempotent(t *testing.T) {
	transport := testutil.NewInMemoryMCPPair(t, 0, testutil.StandardMCPTools()...)
	pool := NewMCPPool(MCPPoolOptions{Logger: slog.New(slog.DiscardHandler)})
	pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
		return transport, nil
	}
	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{Name: "s", Command: "unused"}})
	require.NoError(t, err)
	require.Len(t, conns, 1, "连接成功是本用例的前提")

	pool.mu.Lock()
	sessions := slices.Clone(pool.sessions)
	pool.mu.Unlock()
	require.Len(t, sessions, 1, "池必须持有已连接会话")

	require.NoError(t, pool.Close())
	require.NoError(t, pool.Close(), "重复关闭必须无错")

	closed := make(chan error, 1)
	go func() { closed <- sessions[0].Wait() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 后会话未在限定时间内结束——Close 没有真正关闭会话")
	}
}

// TestMCPCallArguments_PreservesLargeIntegers（**R13**）：大于 2^53 的整数不得经 float64 舍入。
// 逆改点：mcpCallArguments 改为 json.Unmarshal 到 map[string]any
// → 序列化输出 9007199254740992，NotContains 与 Contains 双双红。
// R13: raw bytes pass through so integers beyond 2^53 survive serialisation.
func TestMCPCallArguments_PreservesLargeIntegers(t *testing.T) {
	args, err := mcpCallArguments(json.RawMessage(`{"n":9007199254740993}`))
	require.NoError(t, err)
	wire, err := json.Marshal(&mcp.CallToolParams{Name: "probe", Arguments: args})
	require.NoError(t, err)
	require.Contains(t, string(wire), "9007199254740993", "原始字节必须透传")
	require.NotContains(t, string(wire), "9007199254740992", "不得出现 float64 舍入后的值")

	empty, err := mcpCallArguments(json.RawMessage(`null`))
	require.NoError(t, err)
	require.Equal(t, map[string]any{}, empty, "null → 空对象，不传 nil")
}

// TestMCPPool_ConnectReturnsErrorOnlyOnContextCancellation：只有整体取消 / 截止才是池级
// 错误——普通失败一律吸收为跳过（对照 R2）。取消的 ctx 在拨号前就被看见：dial 一次都不该
// 被调用。
// Only outer cancellation or deadline makes Connect return an error; dialing is short-
// circuited before it can run.
func TestMCPPool_ConnectReturnsErrorOnlyOnContextCancellation(t *testing.T) {
	dialed := 0
	pool := NewMCPPool(MCPPoolOptions{Logger: slog.New(slog.DiscardHandler), Root: t.TempDir()})
	pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
		dialed++
		return nil, errors.New("dial must not run under a canceled context")
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	conns, err := pool.Connect(canceled, []core.MCPServerConfig{{Name: "s", Command: "unused"}})
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, conns)
	require.Zero(t, dialed, "整体取消必须在拨号之前短路")

	expired, cancelDeadline := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelDeadline()
	_, err = pool.Connect(expired, []core.MCPServerConfig{{Name: "s", Command: "unused"}})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// handshakeBlockingTransport 在 Connect 时先执行 onConnect，再阻塞到 ctx 结束——
// 用于在「握手期间」触发取消 / 截止（既有用例只覆盖拨号前已取消）。
// handshakeBlockingTransport runs onConnect and then blocks until the context ends, so
// cancellation happens during the handshake rather than before dialing.
type handshakeBlockingTransport struct {
	onConnect func()
}

// Connect 触发回调后等待 ctx 结束，并按 ctx 错误返回。
// Connect fires the callback, waits for the context, and returns its error.
func (t *handshakeBlockingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	t.onConnect()
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestMCPPool_ConnectCancellationDuringHandshake：整体取消 / 截止发生在握手期间时仍是
// **池级错误**——最后一个 server 的失败不得把取消吞成 nil；单 server 的握手预算超时
// （父 ctx 未结束）仍按跳过处理；拨号失败分支同样先判父 ctx。
// 逆改点：去掉失败分支与返回前的 ctx.Err() 检查 → 取消 / 截止两条返回 nil，ErrorIs 红。
// Outer cancellation during the handshake is a pool-level error; a per-server budget
// timeout with a live parent context remains a skip.
func TestMCPPool_ConnectCancellationDuringHandshake(t *testing.T) {
	newPool := func(t *testing.T, timeout time.Duration) *MCPPool {
		t.Helper()
		pool := NewMCPPool(MCPPoolOptions{
			Logger:         slog.New(slog.DiscardHandler),
			Root:           t.TempDir(),
			ConnectTimeout: timeout,
		})
		t.Cleanup(func() { require.NoError(t, pool.Close()) })
		return pool
	}

	t.Run("取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pool := newPool(t, time.Minute)
		pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
			return &handshakeBlockingTransport{onConnect: cancel}, nil
		}

		conns, err := pool.Connect(ctx, []core.MCPServerConfig{{Name: "last", Command: "unused"}})
		require.ErrorIs(t, err, context.Canceled, "最后一个 server 握手期间取消，不得吞成 nil")
		require.Empty(t, conns)
	})

	t.Run("截止", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		pool := newPool(t, time.Minute)
		pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
			return &handshakeBlockingTransport{onConnect: func() {}}, nil
		}

		conns, err := pool.Connect(ctx, []core.MCPServerConfig{{Name: "last", Command: "unused"}})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Empty(t, conns)
	})

	t.Run("拨号期间取消", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pool := newPool(t, time.Minute)
		pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
			cancel() // 取消发生在拨号内部：失败分支必须复核父 ctx
			return nil, errors.New("dial interrupted")
		}

		conns, err := pool.Connect(ctx, []core.MCPServerConfig{{Name: "last", Command: "unused"}})
		require.ErrorIs(t, err, context.Canceled)
		require.Empty(t, conns)
	})

	t.Run("握手预算超时仍跳过", func(t *testing.T) {
		pool := newPool(t, 30*time.Millisecond)
		pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) {
			return &handshakeBlockingTransport{onConnect: func() {}}, nil
		}

		conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{Name: "slow", Command: "unused"}})
		require.NoError(t, err, "父 ctx 未取消时，单 server 预算超时只跳过")
		require.Empty(t, conns)
	})
}

// TestMCPPool_ConnectPreservesToolDescriptionAndSchema（**SC-001**）：server 的描述与
// InputSchema 经「SDK 列工具 → 适配器 → 注册池」后必须保真——工具自身读数与注册池交给
// 模型的 `Specs`（Description / Parameters）都与 server 声明逐字一致，且不是兜底 schema。
// 区分度：清空 Description 或把 schema 固定为 fallback 时，本用例的等值断言红。
// SC-001: descriptions and input schemas survive the SDK list → adapter → registry path.
func TestMCPPool_ConnectPreservesToolDescriptionAndSchema(t *testing.T) {
	const (
		toolName    = "fidelity_probe"
		description = "查询仓库提交历史（SC-001 保真探针）"
	)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":  map[string]any{"type": "string", "description": "仓库名称"},
			"limit": map[string]any{"type": "integer", "minimum": 1},
		},
		"required": []any{"repo"},
	}
	transport := testutil.NewInMemoryMCPPair(t, 0, testutil.MCPTestTool{
		Name:        toolName,
		Description: description,
		Schema:      schema,
		Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		},
	})
	pool := NewMCPPool(MCPPoolOptions{Logger: slog.New(slog.DiscardHandler), Root: t.TempDir()})
	pool.dial = func(core.MCPServerConfig) (mcp.Transport, error) { return transport, nil }
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{Name: "docs", Command: "unused"}})
	require.NoError(t, err)
	require.Len(t, conns, 1)
	require.Len(t, conns[0].Tools, 1)

	registry := NewRegistry(slog.New(slog.DiscardHandler))
	require.NoError(t, registry.Register(conns[0].Tools[0]))
	adapted, ok := registry.Resolve(toolName)
	require.True(t, ok)

	wantSchema, err := json.Marshal(schema)
	require.NoError(t, err)
	require.Equal(t, description, adapted.Description(), "描述必须原样透传")
	require.NotEqual(t, mcpFallbackInputSchema, string(adapted.InputSchema()), "不得退化成兜底 schema")
	require.JSONEq(t, string(wantSchema), string(adapted.InputSchema()), "输入 schema 必须保真")

	specs := registry.Specs([]string{toolName})
	require.Len(t, specs, 1)
	require.Equal(t, description, specs[0].Description, "注册池交给模型的描述必须保真")
	require.JSONEq(t, string(wantSchema), string(specs[0].Parameters), "注册池交给模型的 schema 必须保真")
}

// TestMCPPool_DialCommandArgsVerbatim（约束 8）：`Args` 逐项原样进入 argv——含空白的
// 参数保持为**单个** argv 元素，不重新切分、不加引号、不经 shell。
// 逆改点：恢复 `strings.Fields(spec.Command)` 或把 Args 拼成一串 → 等值断言红。
// Args enter argv one-for-one; arguments containing spaces stay single elements.
func TestMCPPool_DialCommandArgsVerbatim(t *testing.T) {
	pool := NewMCPPool(MCPPoolOptions{Root: t.TempDir()})
	spec := core.MCPServerConfig{
		Name:    "verbatim",
		Command: "mcp-server",
		Args: []string{
			"--config=dir with space/config.yaml",
			"hello world",
			"--token=${MCP_TOKEN}",
			"",
		},
	}

	transport, err := pool.dialCommand(spec)
	require.NoError(t, err)
	command := transport.(*mcp.CommandTransport).Command
	require.Equal(t, []string{
		"mcp-server",
		"--config=dir with space/config.yaml",
		"hello world",
		"--token=${MCP_TOKEN}",
		"",
	}, command.Args, "argv[0] = command，其余逐项等于 Args（空串也保留）")
	require.Nil(t, command.Cancel, "exec.Command 创建——进程不绑握手预算")
}

// TestMCPPool_DialCommandNoWhitespaceSplitting（约束 8 / 47）：工具层**不再**对 command
// 做空白拆分——多词字符串到达本层时原样作为单个 argv[0]（拆分只在解析层的旧文件兼容
// 路径发生，且已带 WARN）。
// 逆改点：恢复 `strings.Fields` → argv 长度断言红（一个元素变成三个）。
// The tool layer performs no whitespace splitting: a multi-word command is one argv[0].
func TestMCPPool_DialCommandNoWhitespaceSplitting(t *testing.T) {
	pool := NewMCPPool(MCPPoolOptions{Root: t.TempDir()})
	const legacy = "npx -y @scope/server"

	transport, err := pool.dialCommand(core.MCPServerConfig{Name: "legacy", Command: legacy})
	require.NoError(t, err)
	command := transport.(*mcp.CommandTransport).Command
	require.Equal(t, []string{legacy}, command.Args, "工具层不得按空白拆分 command")
	require.Len(t, command.Args, 1, "argv 只有一个元素")
}

// TestMCPPool_DialCommandEmptyCommand：空白 command 在构造期 fail-closed（空 argv[0]
// 会让 exec 报出与配置无关的系统错误，失去可诊断性）。
// An empty or blank command fails at construction instead of reaching exec.
func TestMCPPool_DialCommandEmptyCommand(t *testing.T) {
	pool := NewMCPPool(MCPPoolOptions{Root: t.TempDir()})
	for _, command := range []string{"", "   ", "\t"} {
		_, err := pool.dialCommand(core.MCPServerConfig{Name: "empty", Command: command})
		require.Error(t, err, "空白 command 必须拒绝：%q", command)
		require.Contains(t, err.Error(), "command 为空")
	}
}

// TestMCPPool_DialCommandCapturesSnapshotAtCallTime：argv 是调用时刻对配置快照的**拷贝**——
// supervisor 之后修改快照里的 Args 不得改变已构造的命令（工具来源 = supervisor 快照，
// 不是进程级全局，M8）。
// 逆改点：把 Args 直接赋给 Cmd.Args → 快照后修改会穿透到已构造命令，断言红。
// argv is a copy of the snapshot at call time; later snapshot mutations do not leak in.
func TestMCPPool_DialCommandCapturesSnapshotAtCallTime(t *testing.T) {
	pool := NewMCPPool(MCPPoolOptions{Root: t.TempDir()})
	args := []string{"--stdio"}

	transport, err := pool.dialCommand(core.MCPServerConfig{Name: "snap", Command: "mcp", Args: args})
	require.NoError(t, err)
	args[0] = "--mutated-after-dial"

	command := transport.(*mcp.CommandTransport).Command
	require.Equal(t, []string{"mcp", "--stdio"}, command.Args,
		"已构造命令必须保留调用时刻的取参快照")
}

// TestMCPPool_ConnectToolsFollowSnapshot：工具集合只来自本次快照建立的连接——换一份快照
// 再连接时，返回的工具随之替换，旧工具不残留（否证「进程级全局」；M8）。
// The tool set follows the snapshot of each Connect call; no process-global residue.
func TestMCPPool_ConnectToolsFollowSnapshot(t *testing.T) {
	newToolPair := func(name string) mcp.Transport {
		return testutil.NewInMemoryMCPPair(t, 0, testutil.MCPTestTool{
			Name:        name,
			Description: "快照探针 " + name,
			Schema:      map[string]any{"type": "object"},
			Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name}}}, nil
			},
		})
	}
	transports := map[string]mcp.Transport{
		"old": newToolPair("old_tool"),
		"new": newToolPair("new_tool"),
	}
	pool := NewMCPPool(MCPPoolOptions{Logger: slog.New(slog.DiscardHandler), Root: t.TempDir()})
	pool.dial = func(spec core.MCPServerConfig) (mcp.Transport, error) {
		return transports[spec.Name], nil
	}
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	first, err := pool.Connect(context.Background(),
		[]core.MCPServerConfig{{Name: "old", Command: "old-mcp"}})
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Len(t, first[0].Tools, 1)
	require.Equal(t, "old_tool", first[0].Tools[0].Name())

	second, err := pool.Connect(context.Background(),
		[]core.MCPServerConfig{{Name: "new", Command: "new-mcp"}})
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, "new", second[0].Server)
	require.Len(t, second[0].Tools, 1)
	require.Equal(t, "new_tool", second[0].Tools[0].Name(), "工具集合随快照替换")
}

// —— US-12：进程树回收（约束 19 / 22）——
// Process-tree reaping added by US-12.

// 进程树用例的辅助角色：同一测试二进制按环境变量重入（父 → 孙两层），不依赖 shell
// 或平台工具，Windows 与 Unix 走同一套断言。
// The tree test re-enters the same test binary as parent and grandchild so one portable
// assertion covers both platforms.
const (
	treeRoleEnv        = "BASALT_MCP_TREE_ROLE"
	treeBeatEnv        = "BASALT_MCP_TREE_BEAT"
	treeRoleParent     = "parent"
	treeRoleGrandchild = "grandchild"
)

// TestMCPPool_KillProcessTree：整树终止必须连**孙进程**一起收掉——只杀直接子进程会
// 留下继续写心跳的孤儿。逆改点：把 killMCPProcessTree 改成只 `Process.Kill()` →
// 父进程死后孙进程继续写，末尾等值断言红。
// Killing the tree must also reap the grandchild; a direct-child kill leaves a live
// orphan writing heartbeats, which turns the size-equality assertion red.
func TestMCPPool_KillProcessTree(t *testing.T) {
	switch os.Getenv(treeRoleEnv) {
	case treeRoleParent:
		runTreeParent()
		return
	case treeRoleGrandchild:
		runTreeGrandchild()
		return
	}

	beat := filepath.Join(t.TempDir(), "beat.txt")
	pidFile := beat + ".pid"
	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPPool_KillProcessTree$")
	cmd.Env = append(os.Environ(), treeRoleEnv+"="+treeRoleParent, treeBeatEnv+"="+beat)
	// 与生产路径同一配置：Unix 独立进程组，Windows 空操作（整树由 taskkill 遍历）。
	configureMCPChildProcess(cmd)
	require.NoError(t, cmd.Start())
	defer func() {
		// 兜底清理：断言失败时也不得留下孤儿（先读孙进程 PID，再收父进程）。
		if pid := readTreePID(pidFile); pid > 0 {
			if process, err := os.FindProcess(pid); err == nil {
				_ = process.Kill()
			}
		}
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	require.Eventually(t, func() bool {
		info, err := os.Stat(beat)
		return err == nil && info.Size() > 2
	}, 10*time.Second, 20*time.Millisecond, "辅助进程未能开始心跳")

	require.NoError(t, killMCPProcessTree(cmd))
	// 收尸放在 defer：若整树回收失效，父进程仍在等孙进程，这里调用 Wait 会挂死，
	// 心跳断言就永远轮不到执行（红态必须是逐字断言，不是超时）。
	time.Sleep(400 * time.Millisecond)
	sizeAfterKill := treeBeatSize(t, beat)
	time.Sleep(400 * time.Millisecond)
	require.Equal(t, sizeAfterKill, treeBeatSize(t, beat),
		"孙进程仍在写心跳——进程树未被整棵回收")
}

// runTreeParent 是父角色：再拉起一个孙角色进程并等它退出（等不到就说明整树没被收）。
// runTreeParent spawns the grandchild role and waits; it never returns while the grandchild lives.
func runTreeParent() {
	beat := os.Getenv(treeBeatEnv)
	child := exec.Command(os.Args[0], "-test.run=^TestMCPPool_KillProcessTree$")
	child.Env = append(os.Environ(), treeRoleEnv+"="+treeRoleGrandchild, treeBeatEnv+"="+beat)
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	_ = child.Wait()
	os.Exit(0)
}

// runTreeGrandchild 是孙角色：写一次 PID，然后持续追加心跳（10ms 一次）。
// runTreeGrandchild records its pid and appends a heartbeat every 10ms.
func runTreeGrandchild() {
	beat := os.Getenv(treeBeatEnv)
	if err := os.WriteFile(beat+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		os.Exit(3)
	}
	for {
		file, err := os.OpenFile(beat, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(4)
		}
		_, _ = file.WriteString("x")
		_ = file.Close()
		time.Sleep(10 * time.Millisecond)
	}
}

// readTreePID 读孙进程 PID；文件未就绪返回 0。
// readTreePID returns the grandchild pid, or 0 before the file exists.
func readTreePID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return pid
}

// treeBeatSize 读心跳文件大小（继续增长 = 孙进程还活着）。
// treeBeatSize reads the heartbeat size; a growing size means the grandchild lives.
func treeBeatSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

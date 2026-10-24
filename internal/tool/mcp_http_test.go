package tool

// 本文件是 US-12 的真实 HTTP 传输层（约束 19 / 20 / 22）：用 `httptest` + SDK 的
// `NewStreamableHTTPHandler` 起一个**进程内**的真实 streamable HTTP MCP server，
// 验证连接、初始化、列工具、调用与"零子进程"；不是注入假连接器的白盒替身。
// 另含地址形状与重定向用例：白名单判定在 Policy 层，本层只保证不会把未授权地址
// 通过重定向带出去。
// This file exercises the real streamable HTTP transport against an in-process
// httptest server, including the zero-process guarantee and redirect refusal.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/testutil"
)

// newHTTPMCPTestServer 起一个真实的 HTTP MCP server（标准工具集），返回地址与关闭函数。
// 中文验收点：连接 → 初始化 → 列工具 → 调用 echo 全链路走 HTTP；服务端不启动任何子进程。
// newHTTPMCPTestServer starts a real HTTP MCP server serving the standard tool set.
func newHTTPMCPTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "basalt-http-test-server", Version: "v1"}, nil)
	for _, tool := range testutil.StandardMCPTools() {
		server.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Schema,
		}, tool.Handler)
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// newHTTPPool 造一只默认预算的连接池；日志进内存，供失败用例断言固定键。
// newHTTPPool builds a pool whose logs stay in memory for failure assertions.
func newHTTPPool(logs *bytes.Buffer) *MCPPool {
	return NewMCPPool(MCPPoolOptions{
		Logger:         slog.New(slog.NewTextHandler(logs, nil)),
		Root:           "",
		ConnectTimeout: 5 * time.Second,
		Version:        "us12-test",
	})
}

// TestMCPHTTP_ConnectListAndCall：真实 HTTP 传输上的最小闭环；连接后池内**恰好**登记
// 一条会话且对应的本地进程句柄为 nil——http 传输零子进程（约束 19）。
// The minimal loop over real HTTP, plus the zero-process guarantee.
func TestMCPHTTP_ConnectListAndCall(t *testing.T) {
	var logs bytes.Buffer
	pool := newHTTPPool(&logs)
	srv := newHTTPMCPTestServer(t)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{
		Name: "remote", Transport: "http", URL: srv.URL,
	}})
	require.NoError(t, err)
	require.Len(t, conns, 1, "HTTP server 必须连上：%s", logs.String())
	require.Len(t, conns[0].Tools, 3, "标准工具集三件全部列出")

	pool.mu.Lock()
	cmds := len(pool.cmds)
	var firstCmdNil bool
	if cmds == 1 {
		firstCmdNil = pool.cmds[0] == nil
	}
	pool.mu.Unlock()
	require.Equal(t, 1, cmds, "每个已建立会话登记一条进程句柄槽位")
	require.True(t, firstCmdNil, "http 传输不得登记本地子进程")

	var echo core.Tool
	for _, adapter := range conns[0].Tools {
		if adapter.Name() == "echo" {
			echo = adapter
		}
	}
	require.NotNil(t, echo, "echo 必须出现在工具列表里")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := echo.Execute(ctx, core.ToolEnv{Timeout: 5 * time.Second}, json.RawMessage(`{"text":"hello-http"}`))
	require.NoError(t, err)
	require.Equal(t, "hello-http", res.Content)
}

// TestMCPHTTP_URLShapeRejected：地址形状不合法 → 该 server 被吸收为 WARN、零连接、
// 零会话；白名单 / 端口档不在这里判定（Policy 层负责），但形状错误不得进入拨号。
// Malformed URLs are absorbed into a warning with zero connections.
func TestMCPHTTP_URLShapeRejected(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "空地址", url: ""},
		{name: "相对地址", url: "mcp.example.com/mcp"},
		{name: "非 http 协议", url: "ftp://mcp.example.com/mcp"},
		{name: "带 userinfo", url: "http://user:pass@mcp.example.com/mcp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			pool := newHTTPPool(&logs)
			t.Cleanup(func() { require.NoError(t, pool.Close()) })

			conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{
				Name: "remote", Transport: "http", URL: tc.url,
			}})
			require.NoError(t, err, "单 server 失败不得中断启动")
			require.Empty(t, conns)
			require.Contains(t, logs.String(), "MCP server 未连接——已跳过")

			pool.mu.Lock()
			defer pool.mu.Unlock()
			require.Empty(t, pool.sessions)
			require.Empty(t, pool.cmds)
		})
	}
}

// TestMCPHTTP_UnknownTransportRejected：表外传输类型在拨号层被拒绝（解析层已先拒一次，
// 这里兜底直接构造连接池的情况）。
// An unknown transport is rejected at the dial layer as a fallback.
func TestMCPHTTP_UnknownTransportRejected(t *testing.T) {
	var logs bytes.Buffer
	pool := newHTTPPool(&logs)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{
		Name: "remote", Transport: "websocket", URL: "http://mcp.example.com/mcp",
	}})
	require.NoError(t, err)
	require.Empty(t, conns)
	require.Contains(t, logs.String(), "MCP server 未连接——已跳过")
}

// TestMCPHTTP_RedirectNotFollowed：指向合法 MCP server 的 3xx 不得被跟随——否则一次
// 重定向就能把请求带到未通过白名单判定的地址（约束 20 的硬边界）。
// Redirects are never followed, so a 3xx cannot bypass the allowlist.
func TestMCPHTTP_RedirectNotFollowed(t *testing.T) {
	var reached atomic.Int32
	target := newHTTPMCPTestServer(t)
	targetHandler := target.Config.Handler
	target.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		targetHandler.ServeHTTP(w, r)
	})

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	var logs bytes.Buffer
	pool := newHTTPPool(&logs)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })

	conns, err := pool.Connect(context.Background(), []core.MCPServerConfig{{
		Name: "remote", Transport: "http", URL: redirector.URL,
	}})
	require.NoError(t, err)
	require.Empty(t, conns, "重定向后的地址不得被当作已授权连接")
	require.Zero(t, reached.Load(), "目标 server 不得收到任何请求")
}

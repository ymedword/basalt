package testutil

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 本文件是 MCP 测试基建（**非测试文件**，供跨包测试 import）：进程内 in-memory
// server/client 传输对、标准工具集、集成层子进程入口。SDK 在测试基建里出现是
// 约束 25 的**测试面例外**——它不参与运行时装配（判据见 tasks.md Notes ③）。
// MCP test infrastructure (non-test file so other packages can import it): in-memory
// transport pairs, the standard tool set, and the integration-layer subprocess entry.

const (
	// mcpHelperEnv 标记"本进程是集成层的 MCP helper 子进程"。
	mcpHelperEnv = "BASALT_MCP_HELPER"
	// mcpPageSizeEnv 让集成层把分页压到每页 1 件（不改代码即可验证游标链路）。
	mcpPageSizeEnv = "BASALT_MCP_PAGE_SIZE"
)

// MCPTestTool 是一件可编程的测试工具：名 / 描述 / schema / 处理函数原样进 server。
// MCPTestTool is one programmable test tool: name, description, schema and handler
// go to the server verbatim.
type MCPTestTool struct {
	Name        string
	Description string
	Schema      any
	Handler     mcp.ToolHandler
}

// NewInMemoryMCPPair 建一对进程内传输：server 先连、client 由调用方后连——
// SDK 的 NewInMemoryTransports 要求两侧各自 Connect，先起 server 才能接住初始化。
// 返回 **client 侧**传输；server 会话登记在 t.Cleanup（LIFO：调用方的 client
// 清理先跑，server 清理后跑）。
// NewInMemoryMCPPair builds an in-memory pair; the server connects first and the
// caller connects the client afterwards. The client-side transport is returned.
func NewInMemoryMCPPair(t *testing.T, pageSize int, tools ...MCPTestTool) mcp.Transport {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(
		&mcp.Implementation{Name: "basalt-test-server", Version: "v1"},
		&mcp.ServerOptions{PageSize: pageSize},
	)
	for _, tool := range tools {
		server.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Schema,
		}, tool.Handler)
	}
	session, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("连接 in-memory MCP server 失败: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("关闭 in-memory MCP server 会话失败: %v", err)
		}
	})
	return clientTransport
}

// StandardMCPTools 是标准测试工具集：`echo`（回显 text 参数）、`boom`（IsError +
// 固定文本；在集成层 helper 子进程里让 server 直接退出，供"调用中途进程死亡"用例）、
// `blob`（图片内容块，判非文本渲染与 base64 不回显）。
// StandardMCPTools is the standard tool set: echo, boom and blob.
func StandardMCPTools() []MCPTestTool {
	return []MCPTestTool{
		{
			Name:        "echo",
			Description: "回显 text 参数",
			Schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"text": map[string]any{"type": "string"}},
			},
			Handler: func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args struct {
					Text string `json:"text"`
				}
				if len(req.Params.Arguments) > 0 {
					if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
						return nil, err
					}
				}
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: args.Text}},
				}, nil
			},
		},
		{
			Name:        "boom",
			Description: "IsError + 固定文本；集成层 helper 进程里退出 server",
			Schema:      map[string]any{"type": "object"},
			Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if os.Getenv(mcpHelperEnv) != "" {
					// 集成层专供：让子进程在此调用中死亡（模拟 server 崩溃）。
					// 进程内用例不会带这个环境变量，因此不会误杀测试进程。
					os.Exit(1)
				}
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "boom: tool failed"}},
				}, nil
			},
		},
		{
			Name:        "blob",
			Description: "图片内容块（判非文本渲染）",
			Schema:      map[string]any{"type": "object"},
			Handler: func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.ImageContent{
						MIMEType: "image/png",
						Data:     []byte("aGVsbG8="),
					}},
				}, nil
			},
		},
	}
}

// RunMCPHelper 是集成层的子进程入口：父进程用测试二进制自举（`-test.run=
// ^TestMCPHelperProcess$`）拉起自己并设 `BASALT_MCP_HELPER=1`，子进程在本函数里
// 以 stdio 传输服务标准工具集，直到父进程关闭 stdin。未设置该环境变量时（默认层
// 顶层运行）**跳过**——它是入口，不是用例。
// RunMCPHelper serves the standard tool set over stdio in the helper subprocess; it
// skips when the helper environment variable is absent.
func RunMCPHelper(t *testing.T) {
	t.Helper()
	if os.Getenv(mcpHelperEnv) == "" {
		t.Skip("仅作为 MCP stdio 子进程入口运行（未设置 BASALT_MCP_HELPER）")
	}
	pageSize := 0
	if raw := os.Getenv(mcpPageSizeEnv); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("BASALT_MCP_PAGE_SIZE 不是整数: %q", raw)
		}
		pageSize = n
	}
	server := mcp.NewServer(
		&mcp.Implementation{Name: "basalt-mcp-helper", Version: "v1"},
		&mcp.ServerOptions{PageSize: pageSize},
	)
	for _, tool := range StandardMCPTools() {
		server.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Schema,
		}, tool.Handler)
	}
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		t.Fatalf("MCP helper server 退出异常: %v", err)
	}
}

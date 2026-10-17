package tool

// 本文件把一条 MCP 工具适配成 `core.Tool`（US-7）：名称 / 描述 / schema 原样，入参原样
// 字节透传（大整数不经 float64），结果按固定口径渲染。适配器**不落审计、不写消息、
// 不记日志**——两阶段审计的唯一归属是既有 `ToolExecutor`（宪法 II / V）。
// This file adapts one MCP tool to core.Tool. Input bytes pass through untouched, the
// result is rendered by fixed rules, and the adapter never audits, writes messages or
// logs — the executor owns the two-phase audit.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ymedword/basalt/internal/core"
)

// mcpFallbackInputSchema 是 server 未给 schema（或序列化失败）时的固定回落（§6.2 口径 6）：
// 模型侧仍然看到"可传空对象"的合法 schema。
const mcpFallbackInputSchema = `{"type":"object","properties":{}}`

// mcpToolAdapter 是 `core.Tool` 的 MCP 实现；`server` 只用于错误文案的点名，
// 循环与执行器不感知"来源"（宪法 II）。
// mcpToolAdapter implements core.Tool; server only appears in self-authored error text.
type mcpToolAdapter struct {
	session     *mcp.ClientSession
	server      string
	name        string
	description string
	inputSchema json.RawMessage
}

// 编译期断言：适配器与内置工具同形。
// Compile-time assertion that the adapter is shaped like any other tool.
var _ core.Tool = (*mcpToolAdapter)(nil)

// newMCPToolAdapter 构造适配器：schema 由 `map[string]any` 原样序列化；nil 或序列化失败
// 回落固定 schema（§6.2 口径 6）。
// newMCPToolAdapter serialises the server-provided schema, falling back to a fixed
// object schema when it is absent or unserialisable.
func newMCPToolAdapter(session *mcp.ClientSession, server string, tool *mcp.Tool) *mcpToolAdapter {
	schema := json.RawMessage(mcpFallbackInputSchema)
	if tool.InputSchema != nil {
		if raw, err := json.Marshal(tool.InputSchema); err == nil {
			schema = raw
		}
	}
	return &mcpToolAdapter{
		session:     session,
		server:      server,
		name:        tool.Name,
		description: tool.Description,
		inputSchema: schema,
	}
}

// Name 返回工具名（原样，不加前缀、不做命名空间）。
// Name returns the tool name verbatim.
func (a *mcpToolAdapter) Name() string { return a.name }

// Description 返回 server 给的描述（原样透传）。
// Description returns the server-provided description verbatim.
func (a *mcpToolAdapter) Description() string { return a.description }

// InputSchema 返回 server 给的 JSON Schema（原样序列化）。
// InputSchema returns the serialised server schema.
func (a *mcpToolAdapter) InputSchema() json.RawMessage { return a.inputSchema }

// Execute 调用一次 MCP 工具并按固定规则渲染结果，错误边界按四个判序收口（§2.3）：
// 入参非法 → `*core.InvalidInputError`；调用失败先经 `ctxPreferred` 让**取消 / 截止的
// 身份优先**（与 HTTP 工具同一判序函数，→ timeout），其余映射为 `*core.ExternalError`
// （Reason 自产固定文案，原始错误只进 `Cause`）；`IsError` 是工具级失败，server 文本
// **整段丢弃**（连 Cause 也不放）。SDK 原文可能含 server stderr / 路径 / 凭证，
// **不得**进入任何可观测面（约束 13 / 14 / 15 / 28）。
// Execute applies the four-step error boundary: invalid input, cancellation identity,
// protocol error, then IsError — with server text dropped wholesale.
func (a *mcpToolAdapter) Execute(ctx context.Context, _ core.ToolEnv, input json.RawMessage) (core.Result, error) {
	args, err := mcpCallArguments(input)
	if err != nil {
		return core.Result{}, &core.InvalidInputError{
			Tool:   a.name,
			Reason: "MCP 工具入参必须是 JSON 对象",
		}
	}
	result, err := a.session.CallTool(ctx, &mcp.CallToolParams{Name: a.name, Arguments: args})
	if err != nil {
		if ctxErr := ctxPreferred(ctx, err); ctxErr != nil {
			return core.Result{}, ctxErr
		}
		return core.Result{}, &core.ExternalError{
			Target: "mcp:" + a.server + "/" + a.name,
			Reason: "MCP 调用失败：" + a.server + "/" + a.name,
			Cause:  err,
		}
	}
	if result.IsError {
		// server 文本是纯外部正文：不进 Reason / Cause / 日志 / 审计 / 模型上下文。
		return core.Result{}, &core.ExternalError{
			Target: "mcp:" + a.server + "/" + a.name,
			Reason: "MCP 工具返回错误：" + a.server + "/" + a.name,
		}
	}
	return core.Result{Content: renderCallToolResult(result)}, nil
}

// mcpCallArguments 把执行器给的入参转成协议层的 `Arguments`：空串与 `null` → 空对象
// （null 在 server 侧可能被当非法）；其余**原样字节**交给 SDK 序列化——先解成
// `map[string]any` 会让 >2^53 的大整数经 float64 无报错失真（约束 11 / R13）。
// mcpCallArguments passes non-empty input through as raw bytes; empty and null become
// the empty object.
func mcpCallArguments(input json.RawMessage) (any, error) {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || trimmed == "null" {
		return map[string]any{}, nil
	}
	// 只做形态校验（必须是 JSON 对象）：值仍是 RawMessage，不经过任何数值类型。
	var object map[string]json.RawMessage
	if err := json.Unmarshal(input, &object); err != nil {
		return nil, err
	}
	return json.RawMessage(input), nil
}

// renderCallToolResult 按固定口径渲染：文本块按序 `\n` 连接；无文本时用 StructuredContent
// 的紧凑 JSON；再不行用非文本占位；全空回落固定文案。（§6.2 口径 11）
// renderCallToolResult applies the fixed rendering order: text blocks, structured
// content, typed placeholders, then the no-output wording.
func renderCallToolResult(result *mcp.CallToolResult) string {
	if result == nil {
		return "(MCP 工具无输出)"
	}
	texts := make([]string, 0, len(result.Content))
	placeholders := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
			continue
		}
		placeholders = append(placeholders, mcpContentPlaceholder(content))
	}
	if len(texts) > 0 {
		return strings.Join(texts, "\n")
	}
	if result.StructuredContent != nil {
		if raw, err := json.Marshal(result.StructuredContent); err == nil {
			return string(raw)
		}
	}
	if len(placeholders) > 0 {
		return strings.Join(placeholders, "\n")
	}
	return "(MCP 工具无输出)"
}

// mcpContentPlaceholder 把非文本内容块渲染为类型化占位符 `[非文本内容：<type> <mime 或 uri>]`；
// **不得**把 base64 原文放进模型上下文（注 3 / 约束 12）。
// mcpContentPlaceholder renders a non-text block as a typed placeholder and never
// exposes the base64 payload.
func mcpContentPlaceholder(content mcp.Content) string {
	switch typed := content.(type) {
	case *mcp.TextContent:
		return typed.Text
	case *mcp.ImageContent:
		return placeholderWithDetail("image", typed.MIMEType)
	case *mcp.AudioContent:
		return placeholderWithDetail("audio", typed.MIMEType)
	case *mcp.ResourceLink:
		return placeholderWithDetail("resource_link", typed.URI)
	case *mcp.EmbeddedResource:
		uri := ""
		if typed.Resource != nil {
			uri = typed.Resource.URI
		}
		return placeholderWithDetail("resource", uri)
	default:
		return "[非文本内容：其他]"
	}
}

// placeholderWithDetail 拼占位符；detail 为空时不留尾随空格。
// placeholderWithDetail omits the detail segment when it is empty.
func placeholderWithDetail(kind, detail string) string {
	if detail == "" {
		return fmt.Sprintf("[非文本内容：%s]", kind)
	}
	return fmt.Sprintf("[非文本内容：%s %s]", kind, detail)
}

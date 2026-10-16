package core

import (
	"context"
	"encoding/json"
)

// 本文件是工具面声明（技术方案 §6.1 逐字）：内置工具、MCP 适配与代码内扩展
// 都实现同一个 Tool 接口——ReAct 循环不感知工具来源。
// This file declares the tool surface (Technical Solution §6.1): builtin tools, MCP
// adapters and in-code extensions all implement the same Tool interface.

// 消息角色常量：`messages.role` 与出站消息模型的取值。
// Message role literals shared by storage and the provider payload.
const (
	// RoleUser 是用户输入消息。
	RoleUser = "user"
	// RoleAssistant 是模型响应消息；带工具调用时 content 可为空——**必须容忍**（契约事实）。
	RoleAssistant = "assistant"
	// RoleTool 是工具结果回填消息：ToolCallID 必须与被回填的那次调用对应。
	RoleTool = "tool"
)

// Tool 是所有工具的统一抽象（技术方案 §6.1 逐字）。
// `env` 是执行环境的**显式入口**（值传递的必填参数，不是可选装饰）：工具**不得**
// 从 context、全局状态或进程环境里猜"当前是哪个 Agent / 哪次运行"。
// Tool is the single abstraction over all tools. env is the explicit, value-passed
// entry point of the execution environment; a tool must never guess its context.
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage // JSON Schema（显式给出，测试钉住）
	Execute(ctx context.Context, env ToolEnv, input json.RawMessage) (Result, error)
}

// Result 是一次工具执行的产出——**只有两个字段**（§6.1 冻结），故截断信息不进结构：
// 截断标记追加在 Content 尾部（`TruncationMarker`），审计侧从标记判定 `truncated`。
// Result has exactly two fields by design; truncation travels in the Content marker.
type Result struct {
	Content   string // 结果内容（回填给模型；失败信息也经这里回填——模型看不到结果就不会收敛）
	Retryable bool   // 失败是否可重试（供模型参考）
}

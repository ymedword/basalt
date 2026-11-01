package core

import (
	"context"
	"encoding/json"
)

// 本文件是工具面声明（技术方案 §4.1.1 逐字）：内置工具、MCP 适配与代码内扩展
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

// Tool 是所有工具的统一抽象（技术方案 §4.1.1 逐字）。
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

// Result 是一次工具执行的产出。§6.1 冻结的是 `Content` / `Retryable` 两个字段的语义，
// 故截断信息仍不进结构：截断标记追加在 Content 尾部（`TruncationMarker`），审计侧从
// 标记判定 `truncated`。US-16 在此之上追加**一个**来源注记 `EvidenceRefs`（检索 / 来源
// 视图随内容返回的引用），它随 tool 消息落 `message_evidence_refs`，不改变前两个字段的
// 任何语义。
// Result carries the frozen Content/Retryable pair plus one US-16 addition: EvidenceRefs,
// the source references returned alongside the content; truncation still travels in the
// Content marker, and the first two fields keep their exact semantics.
type Result struct {
	Content   string // 结果内容（回填给模型；失败信息也经这里回填——模型看不到结果就不会收敛）
	Retryable bool   // 失败是否可重试（供模型参考）
	// EvidenceRefs 是本次结果依赖的来源引用；无来源的工具留空（不是错误）。
	// 与 `Message.EvidenceRefs` 同为**读面注记**：`json:"-"` 保证它不进任何协议 /
	// 审计载荷，只经 `message_evidence_refs` 随消息同事务落库（§3.4.1）。
	// EvidenceRefs lists the sources this result depends on; tools without sources
	// leave it empty, which is not an error. It is a read-side annotation like its
	// message counterpart: never serialized, only persisted via message_evidence_refs.
	EvidenceRefs []EvidenceRef `json:"-"`
}

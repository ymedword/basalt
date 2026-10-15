package core

import (
	"context"
	"time"
)

// 本文件是 Provider 能力的契约面：消息模型、调用接口、审计数据类型与仓储接口。
// 全部为**声明**——零可执行语句（实现进 internal/provider 与 internal/storage）。
// This file is the contract surface for the provider capability: message model, call
// interface, audit data types and the audit store interface. Declarations only; the
// implementations live in internal/provider and internal/storage.

// Message 是一条对话消息。字段与 JSON 名逐字取自技术方案 §4.4：出站发什么、
// 入站存什么，两端同形。
// Message is one conversation message. Fields and JSON names mirror Technical Solution
// §4.4 verbatim so the outbound request and the stored history share one shape.
type Message struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ToolCalls        []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ProviderExtra    map[string]any `json:"provider_extra,omitempty"`
}

// ToolCall 是模型给出的工具调用请求：本 US 只透传，执行权归 US-4 的 ToolExecutor。
// ToolCall is a tool invocation requested by the model. This US only passes it through;
// execution belongs to the US-4 ToolExecutor.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Usage 是 token 计量的载体——**可空**：供应商未返回时为零值，落库为三列 0（不判失败）。
// Usage carries token accounting and may be absent: a provider that returns no usage
// yields the zero value and three zero columns in the database, which is not a failure.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ChatRequest 是一次调用的完整入参（技术方案 §3.1 + §4.1 的单参数调用形状）。
// 两组字段的读者不同：路由与审计关联组由 ProviderService 消费；协议内容组由 ProviderClient 消费。
// ChatRequest is the complete input of one call. Two groups of fields with different
// readers: ProviderService consumes routing and audit bindings, ProviderClient
// consumes the protocol payload.
type ChatRequest struct {
	// —— 路由与审计关联（ProviderService 消费）——
	// Routing and audit bindings, consumed by ProviderService.

	Provider   string // 显式映射表的键；查表不中 → 点名错误（不遍历、不推断——宪法 III）
	RunID      string // 审计关联单位（契约 §7.1）；空 = 拒绝发出（NOT NULL 列的语义位）
	SessionID  string // 可空（列允许 NULL）
	Round      int    // 该 Run 内第几轮（1 起）
	ConfigHash string // 配置快照 hash；本 US 恒空（§2.4 裁决注 3）

	// —— 协议内容（ProviderClient 消费）——
	// Protocol payload, consumed by ProviderClient.

	Model    string    // 空 = 用供应商缺省模型（models 映射）
	Messages []Message // 完整消息模型（含 tool_calls / reasoning_content，原样发出）
}

// ChatResponse 是一次调用的返回（usage 可空；tool_calls 只透传，执行权归 US-4 的 ToolExecutor）。
// ChatResponse is the result of one call. Usage may be absent; tool calls are passed
// through untouched and executed later by the US-4 ToolExecutor.
type ChatResponse struct {
	Content          string
	ToolCalls        []ToolCall
	ReasoningContent string         // 供应商扩展：思考过程——**必须在会话历史里回传**（US-4）
	ProviderExtra    map[string]any // 协议未知字段的透传位（出站平铺 / 入站收集，经**两跳**断言——§3.1）
	Usage            Usage
}

// ProviderClient 抽象单个供应商的调用能力（技术方案 §3.1 逐字）。
// ProviderClient abstracts a single provider's call capability. The loop itself is
// ours (constitution I/II): this interface never schedules tools or owns history.
type ProviderClient interface {
	// Chat 发出一次调用；返回的响应与错误都未经二次包装（错误链保留）。
	// Chat performs one call and returns the response and error unwrapped.
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// LLMCallStatus 是 llm_calls.status 的字面量（契约 schema.sql；pending/success/failed/unknown）。
// LLMCallStatus is the literal set of llm_calls.status.
type LLMCallStatus string

const (
	// LLMCallPending 是预写态；插入失败即拒绝发出（fail-closed）。
	LLMCallPending LLMCallStatus = "pending"
	// LLMCallSuccess 是调用成功。
	LLMCallSuccess LLMCallStatus = "success"
	// LLMCallFailed 是调用失败——失败原因经 ErrorKind 表达。
	LLMCallFailed LLMCallStatus = "failed"
	// LLMCallUnknown 由恢复扫描写入（US-4）；本 US 不产生。
	LLMCallUnknown LLMCallStatus = "unknown"
)

// ErrorKind 是 llm_calls.error_kind 的七类字面量（契约 §7.3）。
// ErrorKind is the seven-value literal set of llm_calls.error_kind (contract §7.3).
type ErrorKind string

const (
	ErrorKindSandboxDenied ErrorKind = "sandbox_denied"
	ErrorKindTimeout       ErrorKind = "timeout"
	ErrorKindNotFound      ErrorKind = "not_found"
	ErrorKindInvalidInput  ErrorKind = "invalid_input"
	ErrorKindExternalError ErrorKind = "external_error"
	ErrorKindRuntimeError  ErrorKind = "runtime_error"
	ErrorKindUnknown       ErrorKind = "unknown"
)

// LLMCall 是审计行的**预写形态**：只含调用前可知的字段（关联键 + 供应商 + 模型 + 快照）。
// **不带状态字段**——预写态恒为 pending，由仓储的 SQL 固定写（不由调用方给，
// 也就没有"传错状态"的可能；终态才由 LLMCallResult 表达）。
// LLMCall is the pre-write shape of an audit row: only fields known before the call.
// It carries no status field on purpose — the repo writes pending in SQL, so a caller
// cannot get it wrong.
type LLMCall struct {
	RunID      string
	SessionID  string
	Round      int
	Provider   string
	Model      string    // 解析后的**生效**模型（请求模型 > 供应商缺省模型）
	ConfigHash string    // 空 → 列写 NULL
	CreatedAt  time.Time // 列存 Unix 毫秒 UTC（契约迁移纪律 4）
}

// LLMCallResult 是审计行的**终态回填形态**：成败与计量都在这——成败只由 Status 表达，不设布尔列。
// 三 token 列**显式平铺**（不嵌 core.Usage）：审计表的列是平的，回填结构与列同构；
// 可空兜底（供应商未返回 usage → 三列 0）由调用方在填之前完成——嵌套一层会让
// "谁负责兜底"变含糊。
// LLMCallResult is the terminal shape of an audit row: success or failure is expressed
// by Status alone. The three token columns are flat here, matching the flat table, and
// the caller resolves an absent usage to zeros before filling them in.
type LLMCallResult struct {
	Status           LLMCallStatus
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	Duration         time.Duration
	ErrorMessage     string    // 截断 4 KB 的责任在 storage（§3.4）
	ErrorKind        ErrorKind // 失败必填（契约 §7.3）；成功留空 → 列写 NULL
}

// LLMCallStore 是 ProviderService 消费的审计仓储接口（接口定义在消费方 core，实现放 storage）。
// LLMCallStore is the audit store interface consumed by ProviderService; declared here,
// implemented in storage.
type LLMCallStore interface {
	// BeginCall 插入 status=pending 的审计行并返回行 id；失败 = 调用方**拒绝发出**该次调用。
	// BeginCall inserts the pending audit row and returns its id. A failure here means the
	// caller must refuse to make the call.
	BeginCall(ctx context.Context, call LLMCall) (int64, error)

	// FinishCall 把该行更新为终态；调用方以独立短超时 + WithoutCancel 调用（契约 §4.2）。
	// FinishCall writes the terminal state. Callers use an independent short timeout with
	// the cancellation detached (contract §4.2).
	FinishCall(ctx context.Context, id int64, result LLMCallResult) error
}

package core

import (
	"context"
	"encoding/json"
)

// FlowRunContext 显式承载一次叶子的固定任务身份与消费端口；不得放入 context。
// FlowRunContext explicitly carries pinned task identity and consumer-owned ports.
type FlowRunContext struct {
	TaskID, RootTaskID, ParentTaskID, ParentRunID, NodeInvocationID, NodeAttemptID string
	TaskRevision                                                                   int64
	AgentUID, AgentConfigHash, ResourceManifestHash                                string
	Authority                                                                      FlowAuthority
	Budget                                                                         AncestorBudget
	Actions                                                                        FlowActionLedger
	Progress                                                                       ProgressGuard
	Tools                                                                          FlowToolBinder
	Models                                                                         FlowModelLedger
	State                                                                          *StructuredState
	InputEvidenceRefs                                                              []EvidenceRef
	Retrieval                                                                      FlowRetrievalGuard
}

// FlowRetrievalGuard rechecks original authority and the frozen collection
// ceiling before any query embedding or source retrieval, without framework types.
type FlowRetrievalGuard interface {
	BeforeRetrieve(ctx context.Context, agent string) ([]string, error)
	BeforeEmbedding(ctx context.Context, auditID string, budget BudgetTicket) error
	BeforeSourceRead(ctx context.Context) (Principal, error)
}

// FlowAuthority 是受理时冻结的权限上限；当前授权仍须逐调用复议。
// FlowAuthority freezes the acceptance ceiling, never replacing current authorization.
type FlowAuthority struct {
	CallerID        string          `json:"caller_id"`
	AuthorizingKind AuthorizingKind `json:"authorizing_kind"`
	KeyID           string          `json:"key_id,omitempty"`
	Scopes          []string        `json:"scopes"`
	AgentUIDs       []string        `json:"agent_uids"`
	Tools           []string        `json:"tools"`
	CollectionIDs   []string        `json:"collection_ids"`
}

// UsageEstimate 是动作发出前的保守祖先额度，不与 caller 配额混为同一账本。
// UsageEstimate is a conservative ancestor allocation, separate from caller quota.
type UsageEstimate struct {
	Tokens    int64 `json:"tokens"`
	ToolCalls int64 `json:"tool_calls"`
}

// UsageActual 表达已知消耗；未知部分必须保留原预留。
// UsageActual expresses known usage; unknown portions retain their reservation.
type UsageActual struct {
	Tokens    int64 `json:"tokens"`
	ToolCalls int64 `json:"tool_calls"`
	Known     bool  `json:"known"`
}

// BudgetTicket 是持久分配票据，重试相同身份不得重复预留。
// BudgetTicket identifies a durable idempotent allocation.
type BudgetTicket struct {
	ID, TaskID, RunID, ActionID string
	Reserved                    UsageEstimate
}

// AncestorBudget 是 core 实际调用消费的祖先预算端口。
// AncestorBudget is consumed by core at real call boundaries.
type AncestorBudget interface {
	Reserve(ctx context.Context, runID, actionID string, estimate UsageEstimate) (BudgetTicket, error)
	Settle(ctx context.Context, ticket BudgetTicket, actual UsageActual) error
	KeepUnknown(ctx context.Context, ticket BudgetTicket) error
}

// ActionRequest 固定逻辑槽、可信适配器与规范输入；尝试身份不参与逻辑键。
// ActionRequest pins the logical slot, trusted adapter and canonical input.
type ActionRequest struct {
	RootTaskID, TaskID, NodeInvocationID, LogicalSlot string
	ToolName, AdapterVersion, SchemaHash, InputHash   string
	RecoveryClass                                     string
	Input                                             json.RawMessage
	RunID                                             string
	TaskRevision, ExecutionEpoch                      int64
}

// ActionTicket 分离稳定动作与独立发送尝试。
// ActionTicket separates stable intent identity from a dispatch attempt.
type ActionTicket struct {
	ActionID, AttemptID, TaskID, RunID, InputHash string
	TaskRevision, ExecutionEpoch                  int64
}

// ActionOutcome 保存真实调用及审计确认程度，不能由自然语言结果猜测。
// ActionOutcome records actual dispatch and audit certainty rather than guessing from text.
type ActionOutcome struct {
	Outcome, OutputHash        string
	AuditID                    int64
	Dispatched, AuditConfirmed bool
	Output                     json.RawMessage
}

// FlowActionLedger 在实际执行之前持久授权，未知结果不得换身份重放。
// FlowActionLedger durably authorizes dispatch and preserves unresolved action identity.
type FlowActionLedger interface {
	Prepare(ctx context.Context, req ActionRequest) (ActionTicket, error)
	BeforeDispatch(ctx context.Context, ticket ActionTicket, auditID int64) error
	RecordOutcome(ctx context.Context, ticket ActionTicket, outcome ActionOutcome) error
}

// ProgressGuard 在发出前阻断循环，在完成观察后记录可信进展。
// ProgressGuard stops loops before dispatch and records verified progress afterwards.
type ProgressGuard interface {
	BeforeCall(ctx context.Context, call ToolCall) error
	AfterCall(ctx context.Context, call ToolCall, result Result, outcome ActionOutcome) error
}

// FlowToolExecution comes only from the trusted Run adapter, never a message DTO.
// Its hashes bind the original tool schema and the separately compiled strict schema.
type FlowToolExecution struct {
	Flow               *FlowRunContext
	Request            ActionRequest
	OriginalSchemaHash string
	Validator          SchemaValidator
	ReadOnlyOnly       bool
}

// FlowToolBinder derives a pinned adapter/schema classification for an untrusted
// requested call; it cannot grant tools omitted from the frozen ceiling.
type FlowToolBinder interface {
	Bind(ctx context.Context, env ToolEnv, call ToolCall) (FlowToolExecution, error)
}

// FlowReadOnlyToolFilter classifies actual pinned instances before their specs
// enter an Agent prompt. A model cannot opt into a writing adapter by name.
type FlowReadOnlyToolFilter interface {
	ReadOnlyTools(ctx context.Context, pinned map[string]Tool, declared []string) ([]string, error)
}

// FlowModelLedger prepares the ancestor reservation before the original model
// audit. ProviderService alone binds that audit and issues the actual request.
type FlowModelLedger interface {
	PrepareModel(ctx context.Context, request ChatRequest, estimatedInput int) (FlowModelPermit, error)
}

// FlowModelPermit belongs to one frozen model request and never writes llm_calls.
type FlowModelPermit interface {
	BeforeDispatch(ctx context.Context, auditID int64, request ChatRequest) error
	RecordOutcome(ctx context.Context, outcome FlowModelOutcome) error
}

// FlowModelOutcome preserves dispatch/audit/usage certainty independently.
type FlowModelOutcome struct {
	AuditID                    int64
	Dispatched, AuditConfirmed bool
	Status                     LLMCallStatus
	Usage                      Usage
}

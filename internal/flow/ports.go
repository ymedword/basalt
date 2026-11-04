package flow

import (
	"context"
	"encoding/json"

	"github.com/ymedword/basalt/internal/core"
)

// StartRequest 只携调用参数与收紧预算，控制契约由服务端固定。
// StartRequest carries parameters and a tighter budget, not mutable controls.
type StartRequest struct {
	Principal                                core.Principal
	RequestID, IdempotencyKey, DefinitionUID string
	DefinitionVersion                        int64
	Input                                    json.RawMessage
	BudgetCap                                *BudgetCap
}

// AdvanceRequest 以完整条件版本推进一项任务。
// AdvanceRequest advances one task using its complete version tuple.
type AdvanceRequest struct {
	Principal core.Principal
	TaskID    string
	Expected  TaskVersion
}

// CancelRequest 保存有归属的幂等关闭操作。
// CancelRequest identifies an owned idempotent cancellation.
type CancelRequest struct {
	Principal                              core.Principal
	TaskID, RequestID, OperationID, Reason string
	Expected                               TaskVersion
}

// ResumeRequest 只要求可信核对，不授权重放未知动作。
// ResumeRequest requests reconciliation, never blind replay.
type ResumeRequest struct {
	Principal                              core.Principal
	TaskID, RequestID, OperationID, Reason string
	Expected                               TaskVersion
}

// AbandonRequest 是带管理认证及理由的救援操作。
// AbandonRequest is an authenticated administrative rescue.
type AbandonRequest struct {
	Principal                              core.Principal
	Actor                                  core.Actor
	TaskID, RequestID, OperationID, Reason string
	Expected                               TaskVersion
}

// RevisionRequest 只提交既定修订协议内的提议。
// RevisionRequest carries a bounded proposal rather than an arbitrary patch.
type RevisionRequest struct {
	Principal           core.Principal
	TaskID, OperationID string
	Expected            TaskVersion
	Proposal            json.RawMessage
}

// PublishRequest 带管理身份、条件版本和已校验内容。
// PublishRequest binds management identity, version precondition and content.
type PublishRequest struct {
	Principal                             core.Principal
	Actor                                 core.Actor
	DefinitionUID, OperationID, RequestID string
	ExpectedVersion                       int64
	Definition                            Definition
}

// RetireRequest 只退役当前指针，不删除历史内容。
// RetireRequest retires a current pointer without deleting history.
type RetireRequest struct {
	Principal                                     core.Principal
	Actor                                         core.Actor
	DefinitionUID, OperationID, RequestID, Reason string
	Version, ExpectedVersion                      int64
}

// CreateTask 是受理事务的完整服务端输入。
// CreateTask is the complete server-resolved acceptance transaction input.
type CreateTask struct {
	Request                                                  StartRequest
	TaskID, RootTaskID, ParentTaskID, RequestHash, InputHash string
	Definition                                               DefinitionVersion
	Contract                                                 TaskContract
	Input                                                    json.RawMessage
	CreatedAt, Deadline                                      int64
	Depth                                                    int
	Config                                                   Config
}

// NodeAttempt 保存每次尝试，预分配RunID不伪称Run已经准入。
// NodeAttempt records an attempt; a planned RunID is not proof of admission.
type NodeAttempt struct {
	ID               string          `json:"node_attempt_id"`
	TaskID           string          `json:"task_id"`
	NodeID           string          `json:"node_id"`
	NodeInvocationID string          `json:"node_invocation_id"`
	Attempt          int             `json:"attempt"`
	TaskRevision     int64           `json:"task_revision"`
	ExecutionEpoch   int64           `json:"execution_epoch"`
	RunID            string          `json:"run_id,omitempty"`
	SessionID        string          `json:"session_id,omitempty"`
	State            string          `json:"state"`
	Input            json.RawMessage `json:"-"`
	InputHash        string          `json:"input_hash"`
	Output           json.RawMessage `json:"-"`
	OutputHash       string          `json:"output_hash,omitempty"`
	StartedAt        int64           `json:"started_at"`
	FinishedAt       int64           `json:"finished_at,omitempty"`
}

// ClaimNode 原子领取一次顺序节点，不持事务执行业务。
// ClaimNode atomically claims one sequential node without holding a call transaction.
type ClaimNode struct {
	Principal core.Principal
	TaskID    string
	Expected  TaskVersion
	Attempt   NodeAttempt
	Now       int64
}

// CommitNode 一次提交输出、关系、验收和转移。
// CommitNode atomically accepts output, dependencies, verification and transition.
type CommitNode struct {
	Recovered                      bool // Internal: reuse a completed original Run, never re-dispatch it.
	Principal                      core.Principal
	TaskID, AttemptID, OperationID string
	Expected                       TaskVersion
	Revision                       TaskRevision
	Outcome                        TaskStatus
	Reason                         string
	AttemptState                   string
	Output                         json.RawMessage
	OutputHash                     string
	Evidence                       []EvidenceRef
	Claims                         []ClaimRevision
	Artifacts                      []ArtifactVersion
	Verifications                  []Verification
	Now                            int64
}

// CancelTask 携真实join证明，不能用等待超时替代。
// CancelTask carries actual join proof rather than a wait timeout.
type CancelTask struct {
	Request CancelRequest
	Joined  bool
	Now     int64
}

// AbandonTask 的救援标志只由可信授权检查产生。
// AbandonTask rescue flags come only from trusted authorization checks.
type AbandonTask struct {
	Request                      AbandonRequest
	Joined, NoAvailableCallerKey bool
	Now                          int64
}

// ReconcileTask 保存核对事实，不允许清除取消位。
// ReconcileTask supplies reconciliation facts without clearing cancellation.
type ReconcileTask struct {
	Request ResumeRequest
	Status  TaskStatus
	Reason  string
	Joined  bool
	Now     int64
}

// AcceptRevision 固定旧头、契约和不增预算的完整新头。
// AcceptRevision pins the old head, contract and a non-expanding new checkpoint.
type AcceptRevision struct {
	Request          RevisionRequest
	Revision         TaskRevision
	InvalidClaims    []ClaimRef
	InvalidArtifacts []ArtifactRef
	Now              int64
}

// Repository 是任务状态的唯一持久写端口。
// Repository is the single durable task-state mutation port.
type Repository interface {
	FindAccepted(ctx context.Context, principal core.Principal, idempotencyKey string) (Task, error)
	Create(ctx context.Context, req CreateTask) (Task, error)
	Claim(ctx context.Context, req ClaimNode) (NodeAttempt, error)
	Commit(ctx context.Context, req CommitNode) (Task, error)
	Cancel(ctx context.Context, req CancelTask) (Task, error)
	Abandon(ctx context.Context, req AbandonTask) (Task, error)
	Recover(ctx context.Context, req ReconcileTask) (Task, error)
	Revise(ctx context.Context, req AcceptRevision) (Task, error)
	Read(ctx context.Context, principal core.Principal, taskID string) (Task, error)
}

// DefinitionRepository 维护不可变发布版本和条件指针。
// DefinitionRepository maintains immutable versions and conditional pointers.
type DefinitionRepository interface {
	CurrentPointer(ctx context.Context, uid string) (DefinitionVersion, error)
	FindDefinition(ctx context.Context, name string) (DefinitionVersion, error)
	Publish(ctx context.Context, req PublishRequest) (DefinitionVersion, error)
	Retire(ctx context.Context, req RetireRequest) (DefinitionVersion, error)
	ResolveCurrent(ctx context.Context, uid string, requestedVersion int64) (DefinitionVersion, error)
	ReadDefinition(ctx context.Context, ref DefinitionRef) (DefinitionVersion, error)
	ListDefinitions(ctx context.Context, includeRetired bool, cursor string, limit int) ([]DefinitionVersion, string, error)
}

// TaskQuery 是有界只读查询，不触发推进或恢复。
// TaskQuery is bounded and read-only, never advancing or recovering tasks.
type TaskQuery interface {
	ListTasks(ctx context.Context, principal core.Principal, cursor string, limit int, administrative bool, definitionName string) ([]Task, string, error)
	ReadGraph(ctx context.Context, principal core.Principal, taskID string) (EvidenceGraph, error)
	ListAttempts(ctx context.Context, principal core.Principal, taskID, cursor string, limit int) ([]NodeAttempt, string, error)
}

// WorkerRepository 只供服务内部领取／核对，不通过公共GET暴露。
// WorkerRepository exposes internal scheduling facts, never public GET side effects.
type WorkerRepository interface {
	ReadForWorker(ctx context.Context, taskID string) (Task, error)
	ReadyRoots(ctx context.Context, now int64, limit int) ([]Task, error)
	OpenRoots(ctx context.Context, limit int) ([]Task, error)
	Defer(ctx context.Context, taskID string, expected TaskVersion, nextEligibleAt int64) error
	HasUnknownActions(ctx context.Context, taskID string) (bool, error)
}

// NodeRunRequest 由受控节点显式传递身份、固定输入及执行上下文。
// NodeRunRequest explicitly supplies pinned identity, input and execution context.
type NodeRunRequest struct {
	Principal core.Principal
	Task      Task
	Node      Node
	Attempt   NodeAttempt
	Flow      *core.FlowRunContext
}

// NodeRunResult 保留真实Run结局与观察，不由completed推导任务成功。
// NodeRunResult preserves actual Run outcomes separately from task acceptance.
type NodeRunResult struct {
	Reused    bool // Set only by the trusted completed-Run observer.
	Run       core.RunResult
	Output    json.RawMessage
	Evidence  []EvidenceRef
	Usage     core.UsageActual
	Unknown   bool
	Reason    string
	Retryable bool // Trusted adapter: only a proven read-only leaf may be retried.
}

// NodeRunner 是唯一叶子适配面，实现在app并复用共享runAdmission。
// NodeRunner is implemented by app through the shared runAdmission path.
type NodeRunner interface {
	Run(ctx context.Context, req NodeRunRequest) (NodeRunResult, error)
}

// AuthorityResolver 每次复议原执行Key和固定Agent资格，不切换管理员。
// AuthorityResolver rechecks original authorization without switching principals.
type AuthorityResolver interface {
	Current(ctx context.Context, task Task) (core.Principal, error)
	CheckStart(ctx context.Context, principal core.Principal, def DefinitionVersion) error
	NoAvailableCallerKey(ctx context.Context, callerID string) (bool, error)
}

// AuthorityFreezer derives the acceptance ceiling from current Agent/resources
// and the original principal; author JSON cannot grant itself tool/source access.
type AuthorityFreezer interface {
	Freeze(ctx context.Context, principal core.Principal, definition DefinitionVersion) (core.FlowAuthority, error)
}

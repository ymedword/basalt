package observability

import (
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// RunFacts 只保留解释执行所需的事实，不包含消息或用户身份。
// RunFacts retains execution facts without message bodies or user identities.
type RunFacts struct {
	RunID, RequestID, SessionID, Agent, Status, Reason, Trigger string
	StartedAt                                                   time.Time
	FinishedAt                                                  *time.Time
	Rounds, ToolCalls                                           int
	ContextBudget                                               *ContextBudgetFailure `json:",omitempty"`
}

// ContextBudgetFailure contains only numeric facts from the core budget error,
// never arbitrary error text, prompts, identities or tool results.
type ContextBudgetFailure struct {
	Required, Limit, Reserve int64
}

// LLMCallFact 保留真实时长与恢复时刻，不能把恢复时刻当结束时间。
// LLMCallFact keeps duration and recovery time distinct.
type LLMCallFact struct {
	ID                                       int64
	RunID, Status, Purpose, ErrorKind, Model string
	Round                                    int
	CreatedAt                                time.Time
	DurationMS                               *int64
	RecoveredAt                              *time.Time
	PromptTokens, CompletionTokens           *int64
}

// ToolInvocationFact 只含审计关联与状态，不包含参数和结果。
// ToolInvocationFact excludes tool inputs and results.
type ToolInvocationFact struct {
	ID                                         int64
	RunID, ToolCallID, Tool, Status, ErrorKind string
	Round                                      int
	StartedAt                                  time.Time
	FinishedAt, RecoveredAt                    *time.Time
	DurationMS                                 *int64 `json:",omitempty"`
}

// EmbeddingCallFact 只读向量调用审计的允许字段。
// EmbeddingCallFact projects safe embedding audit fields.
type EmbeddingCallFact struct {
	ID                                           int64
	RunID, IngestJobID, Status, ErrorKind, Model string
	CreatedAt                                    time.Time
	FinishedAt                                   *time.Time
}

// TaskExecutionFact 的 RunID 为空时不补造运行关联。
// TaskExecutionFact never invents a run for an empty association.
type TaskExecutionFact struct {
	ID                                      int64
	TaskID, RunID, Agent, Status, ErrorKind string
	StartedAt                               time.Time
	FinishedAt                              *time.Time
	ReasonFromRun                           bool `json:",omitempty"`
}

// ManagementOperationFact 不读取操作详情或资源配置。
// ManagementOperationFact excludes operation details and configuration.
type ManagementOperationFact struct {
	OperationID, Action, Status, Verification, ResultCode string
	CreatedAt                                             time.Time
	FinishedAt, RecoveredAt                               *time.Time
}

// IngestJobFact 不包含文档标题或正文。
// IngestJobFact excludes document titles and content.
type IngestJobFact struct {
	IngestJobID, Status, Stage, ErrorKind string
	Attempt                               int
	StartedAt                             time.Time
	FinishedAt                            *time.Time
}

// AssemblyRecordFact 保留字段是否记录，不能把历史缺失解释为零。
// AssemblyRecordFact preserves presence instead of inventing zero values.
type AssemblyRecordFact struct {
	core.AssemblyRecord
	BudgetRecorded bool
	Retrieval      map[string]any
}

package web

import (
	"context"
	"encoding/json"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// FlowBackend is the HTTP consumer port. It returns explicit safe projections;
// raw checkpoints, inputs, prompts, action arguments and owner keys never cross it.
type FlowBackend interface {
	Enabled() bool
	Definitions(context.Context, core.Principal, bool, string, int) (FlowDefinitionPage, error)
	Definition(context.Context, core.Principal, string) (FlowDefinitionView, error)
	Validate(context.Context, core.Principal, core.Actor, flow.Definition) error
	Publish(context.Context, flow.PublishRequest) (FlowDefinitionView, error)
	Retire(context.Context, flow.RetireRequest) (FlowDefinitionView, error)
	Start(context.Context, flow.StartRequest) (FlowTaskView, bool, error)
	Tasks(context.Context, core.Principal, string, int, string) (FlowTaskPage, error)
	Task(context.Context, core.Principal, string, string, int) (FlowTaskView, error)
	AdminTasks(context.Context, core.Actor, string, int, string) (FlowTaskPage, error)
	AdminTask(context.Context, core.Actor, string, string, int) (FlowTaskView, error)
	Artifacts(context.Context, core.Principal, string, string, int) (FlowArtifactPage, error)
	Verifications(context.Context, core.Principal, string, string, int) (FlowVerificationPage, error)
	Cancel(context.Context, flow.CancelRequest) (FlowTaskView, error)
	Resume(context.Context, flow.ResumeRequest) (FlowTaskView, error)
	Abandon(context.Context, flow.AbandonRequest) (FlowTaskView, error)
}

// FlowAuthoringBackend 提供独立的管理只读编排面，不扩张业务调用方的可读投影。
// FlowAuthoringBackend keeps complete author content outside the business read port.
type FlowAuthoringBackend interface {
	AuthoringAgents(context.Context, core.Actor, string, int) (FlowAgentPage, error)
	AuthoringDefinition(context.Context, core.Actor, flow.DefinitionRef) (FlowAuthoringVersion, error)
}

// FlowAgentView 只展示当前引用与能力配置提示，不返回提示词或凭据。
// FlowAgentView exposes current pins and configuration hints without prompts or secrets.
type FlowAgentView struct {
	Name                 string         `json:"name"`
	DisplayName          string         `json:"display_name"`
	Description          string         `json:"description"`
	Eligible             bool           `json:"eligible"`
	Reason               string         `json:"reason"`
	Agent                *flow.AgentRef `json:"agent,omitempty"`
	KnowledgeCollections []string       `json:"knowledge_collections"`
	MemoryScope          string         `json:"memory_scope"`
	ReadonlyTools        []string       `json:"readonly_tools"`
}

// FlowAgentPage 将当前快照的编排候选按名称有界分页。
// FlowAgentPage bounds the authoring catalog by its last observed name.
type FlowAgentPage struct {
	Items []FlowAgentView `json:"items"`
	Next  string          `json:"next_cursor,omitempty"`
}

// FlowAuthoringVersion 用字符串承载正文，避免浏览器解析信封时损失精确数字。
// FlowAuthoringVersion carries canonical author JSON as a string to preserve exact numbers.
type FlowAuthoringVersion struct {
	UID         string `json:"definition_uid"`
	Version     int64  `json:"version"`
	Hash        string `json:"content_hash"`
	Retired     bool   `json:"retired"`
	PublishedAt int64  `json:"published_at"`
	BundleJSON  string `json:"bundle_json"`
}

type FlowNodeView struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Agent string `json:"agent,omitempty"`
	Tool  string `json:"tool,omitempty"`
	Next  string `json:"next,omitempty"`
}
type FlowDefinitionView struct {
	UID          string          `json:"definition_uid"`
	Version      int64           `json:"version"`
	Name         string          `json:"name"`
	Hash         string          `json:"content_hash"`
	Retired      bool            `json:"retired"`
	Goal         string          `json:"goal"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Nodes        []FlowNodeView  `json:"nodes"`
}
type FlowDefinitionPage struct {
	Items []FlowDefinitionView `json:"items"`
	Next  string               `json:"next_cursor,omitempty"`
}
type FlowTaskPage struct {
	Items []FlowTaskView `json:"items"`
	Next  string         `json:"next_cursor,omitempty"`
}
type FlowSlotView struct {
	ID              string   `json:"id"`
	State           string   `json:"state"`
	EvidenceIDs     []string `json:"evidence_ids"`
	VerificationIDs []string `json:"verification_ids"`
}
type FlowClaimView struct {
	ID       string `json:"claim_id"`
	Revision int64  `json:"claim_revision"`
	State    string `json:"state"`
	Text     string `json:"text,omitempty"`
	Readable bool   `json:"readable"`
}

// FlowAttemptView only carries execution metadata, never the domain attempt's
// raw input/output. RunAdmitted distinguishes a planned ID from an actual Run row.
type FlowAttemptView struct {
	ID               string `json:"node_attempt_id"`
	TaskID           string `json:"task_id"`
	NodeID           string `json:"node_id"`
	NodeInvocationID string `json:"node_invocation_id"`
	Attempt          int    `json:"attempt"`
	TaskRevision     int64  `json:"task_revision"`
	ExecutionEpoch   int64  `json:"execution_epoch"`
	RunID            string `json:"run_id,omitempty"`
	SessionID        string `json:"session_id,omitempty"`
	RunAdmitted      bool   `json:"run_admitted"`
	State            string `json:"state"`
	InputHash        string `json:"input_hash"`
	OutputHash       string `json:"output_hash,omitempty"`
	StartedAt        int64  `json:"started_at"`
	FinishedAt       int64  `json:"finished_at,omitempty"`
}
type FlowTaskView struct {
	ID                string            `json:"task_id"`
	RootID            string            `json:"root_task_id"`
	ParentID          string            `json:"parent_task_id,omitempty"`
	DefinitionUID     string            `json:"definition_uid"`
	DefinitionName    string            `json:"definition_name"`
	DefinitionVersion int64             `json:"definition_version"`
	Status            flow.TaskStatus   `json:"status"`
	Version           flow.TaskVersion  `json:"version"`
	ETag              string            `json:"etag"`
	Goal              string            `json:"goal"`
	Constraints       []string          `json:"constraints"`
	CurrentNode       string            `json:"current_node"`
	CloseIntent       flow.CloseIntent  `json:"close_intent"`
	CancelRequested   bool              `json:"cancel_requested"`
	Unknown           bool              `json:"unknown_actions"`
	Reason            string            `json:"reason,omitempty"`
	Deadline          int64             `json:"deadline"`
	CreatedAt         int64             `json:"created_at"`
	UpdatedAt         int64             `json:"updated_at"`
	Budget            *flow.BudgetCap   `json:"budget,omitempty"`
	RemainingTokens   *int64            `json:"remaining_tokens,omitempty"`
	RemainingTools    *int64            `json:"remaining_tool_calls,omitempty"`
	BudgetExhausted   bool              `json:"budget_exhausted"`
	Slots             []FlowSlotView    `json:"slots"`
	Claims            []FlowClaimView   `json:"claims"`
	Attempts          []FlowAttemptView `json:"attempts"`
	NextAttempt       string            `json:"next_attempt_cursor,omitempty"`
}
type FlowArtifactView struct {
	ID           string `json:"artifact_id"`
	Version      int64  `json:"version"`
	TaskRevision int64  `json:"task_revision"`
	State        string `json:"state"`
	Format       string `json:"format"`
	Hash         string `json:"content_hash"`
	Readable     bool   `json:"readable"`
	Content      string `json:"content,omitempty"`
	Reason       string `json:"reason,omitempty"`
}
type FlowArtifactPage struct {
	Items []FlowArtifactView `json:"items"`
	Next  string             `json:"next_cursor,omitempty"`
}
type FlowVerificationPage struct {
	Items []flow.Verification `json:"items"`
	Next  string              `json:"next_cursor,omitempty"`
}

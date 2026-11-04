package core

import (
	"time"
)

// IsolationSpec 标识必须隔离的一次调用，nil 表示本机执行。
// IsolationSpec marks a call that must run in a container.
type IsolationSpec struct {
	ToolInvocationID int64
	ContainerName    string
	OutputDir        string
}

// SandboxState 是执行事实的状态，不代替调用审计。
// SandboxState records execution facts independently of audit conclusions.
type SandboxState string

// 执行状态闭集；结果未知不得重放。 Execution states never authorize replay.
const (
	SandboxCreating SandboxState = "creating"
	SandboxRunning  SandboxState = "running"
	SandboxFinished SandboxState = "finished"
	SandboxCanceled SandboxState = "canceled"
	SandboxTimeout  SandboxState = "timeout"
	SandboxFailed   SandboxState = "failed"
	SandboxUnknown  SandboxState = "unknown"
)

// CleanupState 表示资源是否确认全部释放。
// CleanupState records whether all execution resources have been released.
type CleanupState string

// pending 仅由预写产生；finish 不制造新的 pending。
// Only the initial ledger write produces pending.
const (
	CleanupPending CleanupState = "pending"
	CleanupCleaned CleanupState = "cleaned"
	CleanupFailed  CleanupState = "failed"
)

// CommandExecution 是容器运行结果；本机结果保持 nil。
// CommandExecution describes a container run, never a local process.
type CommandExecution struct {
	Backend       string
	Profile       string
	Runtime       string
	ImageDigest   string
	ContainerID   string
	ContainerName string
	ExitCode      int
	State         SandboxState
	CleanupState  CleanupState
	CleanupError  string
}

// SandboxExecution 是创建前的账本记录及恢复读取形态。
// SandboxExecution contains the facts known before container creation.
type SandboxExecution struct {
	ToolInvocationID int64
	RunID            string
	AgentName        string
	InstanceID       string
	Profile          string
	Runtime          string
	ImageDigest      string
	ContainerName    string
	State            SandboxState
	CleanupState     CleanupState
	StartedAt        time.Time
}

// SandboxExecutionResult 是执行终态与清理事实。
// SandboxExecutionResult records execution and cleanup without duplicating audit.
type SandboxExecutionResult struct {
	ToolInvocationID int64
	ContainerID      string
	State            SandboxState
	ExitCode         int
	ErrorReason      string
	ArtifactCount    int
	ArtifactBytes    int64
	CleanupState     CleanupState
	CleanupError     string
	FinishedAt       time.Time
}

// SandboxExecutionView 是管理面只读投影。
// SandboxExecutionView is the read-only management projection.
type SandboxExecutionView struct {
	ToolInvocationID int64
	ToolCallID       string
	ToolName         string
	Profile          string
	Runtime          string
	State            SandboxState
	ExitCode         int
	CleanupState     CleanupState
	ErrorReason      string
	ArtifactCount    int
	ArtifactBytes    int64
	StartedAt        time.Time
	FinishedAt       time.Time
}

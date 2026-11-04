package web

import (
	"maps"
	"slices"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

func normalizedIsolationView(v IsolationView) IsolationView {
	v.Profiles = slices.Clone(v.Profiles)
	if v.Profiles == nil {
		v.Profiles = []IsolationProfileView{}
	}
	v.Tools = maps.Clone(v.Tools)
	if v.Tools == nil {
		v.Tools = map[string]string{"shell": "local"}
	}
	return v
}

func isolationExecutionView(row core.SandboxExecutionView) IsolationExecutionView {
	v := IsolationExecutionView{ToolInvocationID: row.ToolInvocationID, ToolCallID: row.ToolCallID, ToolName: row.ToolName, Profile: row.Profile, Runtime: row.Runtime, State: string(row.State), ExitCode: row.ExitCode, CleanupState: string(row.CleanupState), ErrorReason: row.ErrorReason, ArtifactCount: row.ArtifactCount, ArtifactBytes: row.ArtifactBytes}
	if !row.StartedAt.IsZero() {
		v.StartedAt = row.StartedAt.UTC().Format(time.RFC3339)
	}
	if !row.FinishedAt.IsZero() {
		v.FinishedAt = row.FinishedAt.UTC().Format(time.RFC3339)
	}
	return v
}

// IsolationView 是静态隔离配置与启动探针的只读事实投影。
// IsolationView is a read-only projection of static isolation config and startup probes.
type IsolationView struct {
	Enabled               bool                   `json:"enabled"`
	Backend               string                 `json:"backend"`
	Profiles              []IsolationProfileView `json:"profiles"`
	Tools                 map[string]string      `json:"tools"`
	MaxConcurrent         int                    `json:"max_concurrent"`
	MaxTotalCPUs          float64                `json:"max_total_cpus"`
	MaxTotalMemoryMB      int                    `json:"max_total_memory_mb"`
	MaxTotalTempBytes     int64                  `json:"max_total_temp_bytes"`
	MaxTotalArtifactBytes int64                  `json:"max_total_artifact_bytes"`
	CheckedAt             string                 `json:"checked_at"`
	UnavailableReason     string                 `json:"unavailable_reason"`
}

// IsolationProfileView 区分已配置、被绑定和探针可用性，不把未探测写成通过。
// IsolationProfileView distinguishes configuration, binding and verified availability.
type IsolationProfileView struct {
	Name           string  `json:"name"`
	Runtime        string  `json:"runtime"`
	ImageDigest    string  `json:"image_digest"`
	Bound          bool    `json:"bound"`
	Available      bool    `json:"available"`
	Reason         string  `json:"reason"`
	CPUs           float64 `json:"cpus"`
	MemoryMB       int     `json:"memory_mb"`
	PIDs           int     `json:"pids"`
	TmpfsMB        int     `json:"tmpfs_mb"`
	TimeoutSeconds int     `json:"timeout_seconds"`
}

// IsolationExecutionView 只读展示一次工具执行与清理的事实。
// IsolationExecutionView displays execution and cleanup facts for one tool invocation.
type IsolationExecutionView struct {
	ToolInvocationID int64  `json:"tool_invocation_id"`
	ToolCallID       string `json:"tool_call_id"`
	ToolName         string `json:"tool_name"`
	Profile          string `json:"profile"`
	Runtime          string `json:"runtime"`
	State            string `json:"state"`
	ExitCode         int    `json:"exit_code"`
	CleanupState     string `json:"cleanup_state"`
	ErrorReason      string `json:"error_reason"`
	ArtifactCount    int    `json:"artifact_count"`
	ArtifactBytes    int64  `json:"artifact_bytes"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at"`
}

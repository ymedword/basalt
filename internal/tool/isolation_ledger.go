package tool

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// SandboxAdmission 合并进程内活跃计数与仅启动时读取的遗留基线。
// SandboxAdmission combines live in-process occupancy with a startup-only orphan baseline.
type SandboxAdmission struct {
	mu               sync.Mutex
	active, baseline int
	initialized      bool
}

// NewSandboxAdmission 构造未初始化、默认拒绝的准入器。
// NewSandboxAdmission constructs an uninitialized, fail-closed gate.
func NewSandboxAdmission() *SandboxAdmission { return &SandboxAdmission{} }

// RefreshBaseline 只在启动恢复后调用一次；查询失败不能开启准入。
// RefreshBaseline runs once after startup recovery; query failures cannot enable admission.
func (g *SandboxAdmission) RefreshBaseline(ctx context.Context, store core.SandboxExecutionStore, instanceID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.initialized || g.active != 0 {
		return errors.New("准入基线不得在运行中刷新")
	}
	if store == nil || instanceID == "" {
		return errors.New("准入基线缺少账本或实例身份")
	}
	count, err := store.CountActiveSandboxExecutions(ctx, instanceID)
	if err != nil {
		return err
	}
	if count < 0 {
		return errors.New("准入基线计数无效")
	}
	g.baseline = count
	g.initialized = true
	return nil
}

func (g *SandboxAdmission) acquire(limit int) (string, error) {
	if g == nil {
		return "admission_unavailable", isolationDenied("admission_unavailable", "准入未装配")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.initialized {
		return "admission_unavailable", isolationDenied("admission_unavailable", "准入基线未初始化")
	}
	if limit <= 0 || g.baseline >= limit || g.active >= limit-g.baseline {
		return "admission_rejected", isolationDenied("admission_rejected", "隔离并发容量已满")
	}
	g.active++
	return "", nil
}

func (g *SandboxAdmission) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active > 0 {
		g.active--
	}
}
func (g *SandboxAdmission) pin() { g.mu.Lock(); defer g.mu.Unlock(); g.baseline++ }

// SandboxLedger 共用执行生命周期记账，调用审计仍归 ToolExecutor。
// SandboxLedger shares lifecycle persistence while ToolExecutor retains invocation audit ownership.
type SandboxLedger struct {
	store                               core.SandboxExecutionStore
	instanceID, profile, runtime, image string
	logger                              *slog.Logger
}

// NewSandboxLedger 构造账本辅助，不写库。
// NewSandboxLedger constructs a helper without database writes.
func NewSandboxLedger(store core.SandboxExecutionStore, instanceID, profile, runtime, image string, logger *slog.Logger) *SandboxLedger {
	if logger == nil {
		logger = slog.Default()
	}
	return &SandboxLedger{store: store, instanceID: instanceID, profile: profile, runtime: runtime, image: image, logger: logger}
}

func (l *SandboxLedger) begin(ctx context.Context, env core.ToolEnv, name string) error {
	if l == nil || l.store == nil {
		return isolationDenied("not_ready", "执行账本未装配")
	}
	return l.store.BeginSandboxExecution(ctx, core.SandboxExecution{ToolInvocationID: env.ToolInvocationID, RunID: env.RunID, AgentName: env.AgentName, InstanceID: l.instanceID, Profile: l.profile, Runtime: l.runtime, ImageDigest: l.image, ContainerName: name, State: core.SandboxCreating, CleanupState: core.CleanupPending, StartedAt: time.Now().UTC()})
}

func (l *SandboxLedger) markRunning(ctx context.Context, env core.ToolEnv) error {
	err := l.store.MarkSandboxRunning(ctx, env.ToolInvocationID, time.Now().UTC())
	if err != nil {
		l.logger.Warn("执行账本 running 更新失败", "tool_invocation_id", env.ToolInvocationID, "error", err)
	}
	return err
}

func (l *SandboxLedger) finish(ctx context.Context, env core.ToolEnv, res core.CommandResult, runErr error, reason string, cleanup core.CleanupState, artifacts []artifact) core.SandboxExecutionResult {
	r := core.SandboxExecutionResult{ToolInvocationID: env.ToolInvocationID, State: core.SandboxUnknown, ExitCode: res.ExitCode, ErrorReason: reason, CleanupState: cleanup, FinishedAt: time.Now().UTC(), ArtifactCount: len(artifacts)}
	if res.Execution != nil {
		r.ContainerID = res.Execution.ContainerID
		r.CleanupError = res.Execution.CleanupError
		switch res.Execution.State {
		case core.SandboxFinished, core.SandboxCanceled, core.SandboxTimeout, core.SandboxFailed, core.SandboxUnknown:
			r.State = res.Execution.State
		}
	}
	if reason == "" {
		r.ErrorReason = isolationReason(runErr)
	}
	if cleanup != core.CleanupCleaned {
		r.CleanupState = core.CleanupFailed
		r.ErrorReason = "cleanup_failed"
	}
	for _, item := range artifacts {
		r.ArtifactBytes += item.Size
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.store.FinishSandboxExecution(finish, r); err != nil {
		l.logger.Warn("执行账本终态更新失败", "tool_invocation_id", env.ToolInvocationID, "error", err)
	}
	return r
}

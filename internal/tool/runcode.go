package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// RunCodeOptions 显式承载授权、执行、账本与实例级预算，不从上下文推断。
// RunCodeOptions carries authority, execution, persistence and instance budgets explicitly.
type RunCodeOptions struct {
	Policy                                                      core.Policy
	Runner                                                      core.CommandRunner
	Store                                                       core.SandboxExecutionStore
	Admission                                                   *SandboxAdmission
	Root, OutputRoot, InstanceID, Profile, Runtime, ImageDigest string
	MaxConcurrent                                               int
	MaxTimeout                                                  time.Duration
	MaxCodeBytes, MaxInputFiles                                 int
	MaxInputBytes                                               int64
	MaxArtifactFiles                                            int
	MaxArtifactBytes, MaxTotalArtifactBytes                     int64
	PublishMu                                                   *sync.Mutex
	Logger                                                      *slog.Logger
}

// RunCodeTool 只在显式 gVisor 绑定下执行生成代码。
// RunCodeTool executes generated code only under an explicit gVisor binding.
type RunCodeTool struct {
	opts   RunCodeOptions
	ledger *SandboxLedger
}

var _ core.Tool = (*RunCodeTool)(nil)

// NewRunCodeTool 总是可注册；未装配执行后端时调用必拒绝。
// NewRunCodeTool is always registrable; calls without an execution backend are refused.
func NewRunCodeTool(opts RunCodeOptions) *RunCodeTool {
	if opts.PublishMu == nil {
		opts.PublishMu = &sync.Mutex{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &RunCodeTool{opts: opts, ledger: NewSandboxLedger(opts.Store, opts.InstanceID, opts.Profile, opts.Runtime, opts.ImageDigest, opts.Logger)}
}

// Name 返回固定工具名。
// Name returns the fixed tool name.
func (t *RunCodeTool) Name() string { return "run_code" }

// Description 明示隔离前置与禁止降级。
// Description states the isolation prerequisite and prohibits fallback.
func (t *RunCodeTool) Description() string {
	return "在显式配置的 gVisor 隔离中执行 POSIX shell 代码；未配置时拒绝，不退回本机。无网络；输入位于 /task/input，产物写 /output。"
}

const runCodeInputSchema = `{"type":"object","additionalProperties":false,"required":["code"],"properties":{"code":{"type":"string","minLength":1,"maxLength":65536},"inputs":{"type":"array","maxItems":16,"items":{"type":"string","maxLength":512}},"timeout_seconds":{"type":"integer","minimum":1,"maximum":300}}}`

// InputSchema 返回固定输入约束。
// InputSchema returns the fixed input constraints.
func (t *RunCodeTool) InputSchema() json.RawMessage { return json.RawMessage(runCodeInputSchema) }

type runCodeInput struct {
	Code           string   `json:"code"`
	Inputs         []string `json:"inputs"`
	TimeoutSeconds *int     `json:"timeout_seconds"`
}

func runCodeInputError(detail string) error {
	return isolationError("input_rejected", &core.InvalidInputError{Tool: "run_code", Reason: detail})
}

func decodeRunCodeInput(raw json.RawMessage) (runCodeInput, error) {
	var in runCodeInput
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, runCodeInputError("输入格式无效")
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return in, runCodeInputError("输入包含尾随内容")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return in, runCodeInputError("输入格式无效")
	}
	for _, name := range []string{"inputs", "timeout_seconds"} {
		if bytes.Equal(bytes.TrimSpace(fields[name]), []byte("null")) {
			return in, runCodeInputError(name + " 不接受 null")
		}
	}
	if len(in.Code) == 0 || len(in.Code) > 65536 || strings.ContainsRune(in.Code, 0) || len(in.Inputs) > 16 {
		return in, runCodeInputError("代码或输入数量越界")
	}
	if in.TimeoutSeconds != nil && (*in.TimeoutSeconds < 1 || *in.TimeoutSeconds > 300) {
		return in, runCodeInputError("timeout_seconds 必须在 1–300")
	}
	for _, path := range in.Inputs {
		if path == "" || len(path) > 512 || strings.ContainsRune(path, 0) {
			return in, runCodeInputError("输入路径形态无效")
		}
	}
	return in, nil
}

func (t *RunCodeTool) ready(env core.ToolEnv) error {
	if t.opts.Runner == nil {
		return isolationDenied("config_missing", "run_code 未配置 gVisor 执行档")
	}
	if t.opts.Store == nil || t.opts.Policy == nil || t.opts.Admission == nil || t.opts.Profile != "gvisor" || t.opts.Runtime != "runsc" || !env.RunBound || env.ToolInvocationID <= 0 || !safeExecutionSegment(env.RunID) || !safeExecutionSegment(env.AgentName) || !filepath.IsAbs(env.AgentDir) {
		return isolationDenied("not_ready", "run_code 缺少可信 Run 身份或完整执行装配")
	}
	if t.opts.MaxTimeout <= 0 || env.Timeout <= 0 || t.opts.MaxCodeBytes <= 0 || t.opts.MaxInputFiles <= 0 || t.opts.MaxInputBytes <= 0 || t.opts.MaxArtifactFiles <= 0 || t.opts.MaxArtifactBytes <= 0 || t.opts.MaxTotalArtifactBytes <= 0 {
		return isolationDenied("not_ready", "run_code 预算未初始化")
	}
	if foldCase(filepath.Clean(env.AgentDir)) != foldCase(filepath.Join(t.opts.Root, "agents", env.AgentName)) {
		return isolationDenied("not_ready", "Agent 目录与本实例身份不一致")
	}
	return nil
}

func safeExecutionSegment(s string) bool {
	return s != "" && s != "." && s != ".." && len(s) <= 200 && !strings.ContainsAny(s, "/\\:\x00\r\n") && filepath.IsLocal(s)
}

// Execute 依次授权、准入、暂存、预写、执行、发布与清理，不新建调用审计路径。
// Execute authorizes, admits, stages, prewrites, executes, publishes and cleans up without another audit path.
func (t *RunCodeTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	if err := t.ready(env); err != nil {
		return core.Result{}, err
	}
	if err := t.opts.Policy.Check(ctx, core.Action{Type: core.ActionRunCode, Target: "run_code", Agent: env.AgentName, RunBound: env.RunBound}); err != nil {
		return core.Result{}, isolationError("policy_denied", err)
	}
	if _, err := t.opts.Admission.acquire(t.opts.MaxConcurrent); err != nil {
		return core.Result{}, err
	}
	defer t.opts.Admission.release()
	in, err := decodeRunCodeInput(input)
	if err != nil {
		return core.Result{}, err
	}
	stage, err := t.stageRunCode(ctx, env, in)
	if err != nil {
		if cleanup := releaseStage(env, t.opts.Runner, stage, &core.CommandExecution{CleanupState: core.CleanupCleaned}); cleanup != core.CleanupCleaned {
			t.opts.Admission.pin()
		}
		return core.Result{}, err
	}
	budget := min(t.opts.MaxTimeout, env.Timeout)
	if in.TimeoutSeconds != nil {
		budget = min(budget, time.Duration(*in.TimeoutSeconds)*time.Second)
	}
	run, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	name := sandboxContainerName(t.opts.InstanceID, env.ToolInvocationID)
	if err := t.ledger.begin(run, env, name); err != nil {
		if releaseStage(env, t.opts.Runner, stage, &core.CommandExecution{CleanupState: core.CleanupCleaned}) != core.CleanupCleaned {
			t.opts.Admission.pin()
		}
		return core.Result{}, err
	}
	_ = t.ledger.markRunning(run, env)
	res, runErr := t.opts.Runner.Run(run, core.Command{Executable: "/bin/sh", Args: []string{"/task/main.sh"}, Dir: stage.TaskDir, MaxOutputBytes: env.MaxOutputBytes, Isolation: &core.IsolationSpec{ToolInvocationID: env.ToolInvocationID, ContainerName: name, OutputDir: stage.OutputDir}})
	var artifacts []artifact
	note := isolationReason(runErr)
	if runErr == nil && res.Execution != nil && res.Execution.State == core.SandboxFinished && res.Execution.CleanupState == core.CleanupCleaned {
		artifacts, note, runErr = t.collectAndPublishRunCode(run, env, stage, res)
		if note != "" {
			runErr = isolationError(note, runErr)
		}
	} else if runErr == nil && res.Execution == nil {
		runErr = isolationError("state_unknown", errors.New("执行器未返回隔离事实"))
		note = "state_unknown"
	}
	cleanup := releaseStage(env, t.opts.Runner, stage, res.Execution)
	if cleanup != core.CleanupCleaned {
		t.opts.Admission.pin()
	}
	t.ledger.finish(ctx, env, res, runErr, note, cleanup, artifacts)
	return core.Result{Content: runCodeContent(res, artifacts, note)}, runErr
}

func runCodeContent(res core.CommandResult, artifacts []artifact, note string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "退出码：%d\n%s", res.ExitCode, res.Output)
	for _, a := range artifacts {
		fmt.Fprintf(&out, "\n产物：%s（%d 字节，SHA-256 %s）", a.Name, a.Size, a.SHA256)
	}
	if note != "" {
		fmt.Fprintf(&out, "\n隔离结果：%s", note)
	}
	return out.String()
}

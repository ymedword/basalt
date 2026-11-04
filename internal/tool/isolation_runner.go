package tool

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// ContainerRunnerOptions 固定单个执行档的资源、镜像和宿主路径边界。
// ContainerRunnerOptions freezes a profile's resources, image and host path boundaries.
type ContainerRunnerOptions struct {
	Docker                                                                     ContainerCLI
	Root, OutputRoot, InstanceID, Profile, Runtime, Image, ImageDigest         string
	ApprovedDigests, Interpreters                                              []string
	CPUs                                                                       float64
	MemoryMB, PIDs, TmpfsMB, OutputTmpfsMB, MaxOutputBytes, RunAsUID, RunAsGID int
	PassEnv                                                                    bool
	EnvPassthrough                                                             []string
	Logger                                                                     *slog.Logger
	Now                                                                        func() time.Time
	RemoveAll                                                                  func(string) error
}

// ContainerRunner 执行受控 OCI 或 gVisor 命令，不做授权与工具调度。
// ContainerRunner executes controlled OCI or gVisor commands without authorization or tool scheduling.
type ContainerRunner struct{ opts ContainerRunnerOptions }

var (
	_               core.CommandRunner = (*ContainerRunner)(nil)
	isolationDigest                    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type isolationFailure struct {
	reason string
	cause  error
}

func (e *isolationFailure) Error() string { return e.reason + ": " + e.cause.Error() }
func (e *isolationFailure) Unwrap() error { return e.cause }
func isolationError(reason string, err error) error {
	return &isolationFailure{reason: reason, cause: err}
}

func isolationDenied(reason, detail string) error {
	return isolationError(reason, deny(detail, reason, "检查隔离配置与运行时；不允许降级为本机执行。"))
}

func isolationReason(err error) string {
	var failure *isolationFailure
	if errors.As(err, &failure) {
		return failure.reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return ""
}

// NewContainerRunner 验证固定选项并复制可变集合，不创建容器。
// NewContainerRunner validates and copies immutable options without creating containers.
func NewContainerRunner(opts ContainerRunnerOptions) (*ContainerRunner, error) {
	if !isolationDigest.MatchString(opts.ImageDigest) || !slices.Contains(opts.ApprovedDigests, opts.ImageDigest) {
		return nil, isolationDenied("image_untrusted", "镜像 digest 不在已验签批准集合中")
	}
	if opts.Image != opts.ImageDigest && !strings.HasSuffix(opts.Image, "@"+opts.ImageDigest) {
		return nil, isolationDenied("image_untrusted", "镜像引用与批准 digest 不一致")
	}
	if strings.HasPrefix(opts.Image, "-") || strings.ContainsAny(opts.Image, "\r\n\x00 ") {
		return nil, isolationDenied("image_untrusted", "镜像引用无效")
	}
	if !((opts.Profile == "oci" && opts.Runtime == "runc") || (opts.Profile == "gvisor" && opts.Runtime == "runsc")) {
		return nil, isolationDenied("not_ready", "执行档与 runtime 不匹配")
	}
	if opts.InstanceID == "" || !filepath.IsAbs(opts.Root) || !filepath.IsAbs(opts.OutputRoot) || opts.CPUs <= 0 || math.IsNaN(opts.CPUs) || math.IsInf(opts.CPUs, 0) || opts.MemoryMB <= 0 || opts.PIDs <= 0 || opts.TmpfsMB <= 0 || opts.OutputTmpfsMB <= 0 || opts.MaxOutputBytes <= 0 || opts.RunAsUID <= 0 || opts.RunAsGID <= 0 {
		return nil, isolationDenied("not_ready", "隔离路径、身份或资源限额无效")
	}
	if strings.ContainsAny(opts.InstanceID, "\r\n,=\x00") || opts.Profile == "gvisor" && opts.PassEnv {
		return nil, isolationDenied("not_ready", "隔离身份或环境透传无效")
	}
	if opts.Docker == nil {
		opts.Docker = NewDockerCLI("docker")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RemoveAll == nil {
		opts.RemoveAll = os.RemoveAll
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	opts.ApprovedDigests = slices.Clone(opts.ApprovedDigests)
	opts.Interpreters = slices.Clone(opts.Interpreters)
	opts.EnvPassthrough = slices.Clone(opts.EnvPassthrough)
	return &ContainerRunner{opts: opts}, nil
}

func sandboxContainerName(instanceID string, id int64) string {
	sum := sha256.Sum256([]byte(instanceID))
	return fmt.Sprintf("basalt-exec-%x-%d", sum[:6], id)
}

func cleanupContainerName(instanceID string, id int64) string {
	sum := sha256.Sum256([]byte(instanceID))
	return fmt.Sprintf("basalt-clean-%x-%d", sum[:6], id)
}

// SandboxContainerName 供装配恢复与执行账本共用确定性名称。
// SandboxContainerName shares deterministic naming between recovery and the execution ledger.
func SandboxContainerName(instanceID string, id int64) string {
	return sandboxContainerName(instanceID, id)
}

// CleanupContainerName 供启动恢复定位独立清理容器。
// CleanupContainerName identifies cleanup containers during startup recovery.
func CleanupContainerName(instanceID string, id int64) string {
	return cleanupContainerName(instanceID, id)
}

func (r *ContainerRunner) execution() *core.CommandExecution {
	return &core.CommandExecution{Backend: "docker", Profile: r.opts.Profile, Runtime: r.opts.Runtime, ImageDigest: r.opts.ImageDigest, State: core.SandboxFailed, CleanupState: core.CleanupCleaned}
}

func (r *ContainerRunner) validateCommand(cmd core.Command) (core.Command, error) {
	if cmd.Isolation == nil || cmd.Isolation.ToolInvocationID <= 0 || cmd.Isolation.ContainerName != sandboxContainerName(r.opts.InstanceID, cmd.Isolation.ToolInvocationID) {
		return cmd, isolationDenied("not_ready", "容器执行要求有效的隔离身份")
	}
	if len(cmd.Env) != 0 || cmd.Executable == "" || strings.ContainsAny(cmd.Executable, "\x00\r\n") {
		return cmd, isolationDenied("not_ready", "命令环境或入口无效")
	}
	if err := validateMountPath(r.opts.Root, cmd.Dir, false); err != nil {
		return cmd, err
	}
	if cmd.Isolation.OutputDir != "" {
		if err := validateMountPath(r.opts.OutputRoot, cmd.Isolation.OutputDir, true); err != nil {
			return cmd, err
		}
	}
	if r.opts.Profile == "oci" {
		if core.IsShellName(cmd.Executable) {
			return cmd, deny(ruleShellName, cmd.Executable, guidanceShellName)
		}
		interpreters := interpreterSet(r.opts.Interpreters)
		if err := validateControlledCommand(cmd, interpreters, []string{filepath.Join(cmd.Dir, scriptsDirName)}); err != nil {
			return cmd, err
		}
		if interpreters[interpreterBase(cmd.Executable)] {
			if filepath.IsAbs(cmd.Args[0]) || strings.Contains(cmd.Args[0], "\\") || slices.Contains(strings.Split(cmd.Args[0], "/"), "..") {
				return cmd, deny(ruleScriptOutside, cmd.Args[0], guidanceScriptOutside)
			}
			translated, err := translateScriptArg(cmd.Dir, cmd.Args[0], cmd.Dir)
			if err != nil {
				return cmd, err
			}
			cmd.Args = slices.Clone(cmd.Args)
			cmd.Args[0] = translated
		}
	} else if cmd.Executable != "/bin/sh" || !slices.Equal(cmd.Args, []string{"/task/main.sh"}) {
		return cmd, isolationDenied("not_ready", "生成代码仅允许固定脚本入口")
	}
	return cmd, nil
}

func validateMountPath(root, dir string, strict bool) error {
	if !filepath.IsAbs(dir) || strings.ContainsAny(dir, ",\"\r\n\x00") {
		return isolationDenied("not_ready", "挂载路径形态无效")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return isolationDenied("not_ready", "挂载源必须是现有非链接目录")
	}
	real, err := filepath.EvalSymlinks(dir)
	base, baseErr := filepath.EvalSymlinks(root)
	if err != nil || baseErr != nil || !withinDir(real, base) || strict && foldCase(real) == foldCase(base) {
		return isolationDenied("not_ready", "挂载源越出本实例目录")
	}
	return nil
}

func (r *ContainerRunner) inspect(ctx context.Context) error {
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	digests, id, err := r.opts.Docker.ImageInspect(probe, r.opts.Image)
	if err != nil {
		return isolationDenied("image_untrusted", "镜像 inspect 不可用")
	}
	// 纯 sha256 引用允许内容寻址的本地镜像 ID；仓库引用必须命中 RepoDigests。
	// Bare content-addressed IDs are checked as IDs; repository references require RepoDigests.
	if r.opts.Image == r.opts.ImageDigest && id == r.opts.ImageDigest {
		return nil
	}
	for _, ref := range digests {
		if strings.HasSuffix(ref, "@"+r.opts.ImageDigest) {
			return nil
		}
	}
	return isolationDenied("image_untrusted", "实际镜像与批准 digest 不符")
}

// Run 使用创建、启动、等待、读取、删除单一路径，不回退或重放未知执行。
// Run follows one create/start/wait/log/remove path without fallback or replay of unknown outcomes.
func (r *ContainerRunner) Run(ctx context.Context, cmd core.Command) (core.CommandResult, error) {
	e := r.execution()
	result := core.CommandResult{Execution: e}
	checked, err := r.validateCommand(cmd)
	if err != nil {
		return result, err
	}
	cmd = checked
	e.ContainerName = cmd.Isolation.ContainerName
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := r.inspect(ctx); err != nil {
		return result, err
	}
	task, cancel := context.WithTimeout(ctx, 600*time.Second)
	defer cancel()
	create, createCancel := context.WithTimeout(task, 30*time.Second)
	id, err := r.opts.Docker.Create(create, buildCreateArgs(r.opts, cmd))
	createCancel()
	if err != nil || id == "" {
		e.State, e.CleanupState = core.SandboxUnknown, core.CleanupFailed
		// 创建响应丢失时只查证与回收一次；查不到并不证明创建从未发生。
		// A lost create response permits one reconciliation, never a second create.
		removed, cleanupErr := r.RemoveByName(context.Background(), e.ContainerName)
		if removed && cleanupErr == nil {
			e.CleanupState = core.CleanupCleaned
		}
		if cleanupErr != nil {
			e.CleanupError = cleanupErr.Error()
		}
		if err == nil {
			err = errors.New("创建返回空容器 ID")
		}
		return result, isolationError("create_unknown", err)
	}
	e.ContainerID = id
	e.CleanupState = core.CleanupFailed
	defer func() {
		_, err := r.RemoveByName(context.Background(), e.ContainerName)
		if err != nil {
			e.CleanupError = err.Error()
			r.opts.Logger.Warn("容器清理失败", "container", e.ContainerName, "error", err)
			return
		}
		e.CleanupState = core.CleanupCleaned
	}()
	start, startCancel := context.WithTimeout(task, 10*time.Second)
	err = r.opts.Docker.Start(start, id)
	startCancel()
	if err != nil {
		e.State = executionState(err, core.SandboxFailed)
		return result, isolationError(reasonForContext(err, "start_failed"), err)
	}
	e.State = core.SandboxRunning
	code, err := r.opts.Docker.Wait(task, id)
	if err != nil {
		e.State = executionState(err, core.SandboxUnknown)
		return result, isolationError(reasonForContext(err, "wait_failed"), err)
	}
	e.State, e.ExitCode = core.SandboxFinished, code
	result.ExitCode = code
	logs, logsCancel := context.WithTimeout(task, 10*time.Second)
	defer logsCancel()
	limit := r.opts.MaxOutputBytes
	if cmd.MaxOutputBytes > 0 {
		limit = min(limit, cmd.MaxOutputBytes)
	}
	result.Output, result.Truncated, err = r.opts.Docker.Logs(logs, id, limit)
	if err != nil {
		return result, isolationError("wait_failed", err)
	}
	if result.Truncated {
		result.Output += core.TruncationMarker(limit)
	}
	return result, nil
}

func executionState(err error, fallback core.SandboxState) core.SandboxState {
	if errors.Is(err, context.DeadlineExceeded) {
		return core.SandboxTimeout
	}
	if errors.Is(err, context.Canceled) {
		return core.SandboxCanceled
	}
	return fallback
}

func reasonForContext(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return fallback
}

// ListByLabel 有界读取容器名，用于启动恢复。
// ListByLabel reads container names under a bounded recovery call.
func (r *ContainerRunner) ListByLabel(ctx context.Context, label string) ([]string, error) {
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.opts.Docker.ListByLabel(probe, label)
}

// RemoveByName 仅删除本实例标签匹配的确定性容器，删除后再次确认。
// RemoveByName removes only deterministic, instance-labelled containers and verifies absence.
func (r *ContainerRunner) RemoveByName(ctx context.Context, name string) (bool, error) {
	if !strings.HasPrefix(name, strings.TrimSuffix(sandboxContainerName(r.opts.InstanceID, 0), "0")) && !strings.HasPrefix(name, strings.TrimSuffix(cleanupContainerName(r.opts.InstanceID, 0), "0")) {
		return false, errors.New("拒绝删除非本实例容器名")
	}
	names, err := r.ListByLabel(ctx, "basalt.instance_id="+r.opts.InstanceID)
	if err != nil {
		return false, err
	}
	if !slices.Contains(names, name) {
		return false, nil
	}
	remove, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = r.opts.Docker.Remove(remove, name)
	cancel()
	if err != nil {
		return true, err
	}
	names, err = r.ListByLabel(ctx, "basalt.instance_id="+r.opts.InstanceID)
	if err != nil {
		return true, err
	}
	if slices.Contains(names, name) {
		return true, errors.New("容器删除后仍存在")
	}
	return true, nil
}

func containerBaseArgs(opts ContainerRunnerOptions, name string, id int64) []string {
	memory := strconv.Itoa(opts.MemoryMB) + "m"
	return []string{
		"--name", name, "--label", "basalt.managed=true", "--label", "basalt.instance_id=" + opts.InstanceID, "--label", "basalt.tool_invocation_id=" + strconv.FormatInt(id, 10), "--label", "basalt.profile=" + opts.Profile,
		"--runtime", opts.Runtime, "--pull=never", "--ipc=none", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", strconv.Itoa(opts.PIDs), "--memory", memory, "--memory-swap", memory, "--cpus", strconv.FormatFloat(opts.CPUs, 'f', -1, 64), "--ulimit", "nofile=256:256", "--user", fmt.Sprintf("%d:%d", opts.RunAsUID, opts.RunAsGID), "--init", "--workdir", "/task", "--tmpfs", fmt.Sprintf("/tmp:rw,noexec,nosuid,nodev,size=%dm", opts.TmpfsMB), "--log-driver", "local", "--log-opt", "max-size=" + strconv.Itoa(opts.MaxOutputBytes), "--log-opt", "max-file=1", "--log-opt", "compress=false", "--env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "--env", "HOME=/tmp", "--env", "LANG=C.UTF-8", "--env", "TZ=UTC",
	}
}

func buildCreateArgs(opts ContainerRunnerOptions, cmd core.Command) []string {
	args := containerBaseArgs(opts, cmd.Isolation.ContainerName, cmd.Isolation.ToolInvocationID)
	args = append(args, "--mount", "type=bind,src="+cmd.Dir+",dst=/task,readonly")
	if cmd.Isolation.OutputDir != "" {
		args = append(args, "--mount", "type=bind,src="+cmd.Isolation.OutputDir+",dst=/output", "--env", "BASALT_OUTPUT_DIR=/output")
	}
	if opts.PassEnv {
		for _, name := range opts.EnvPassthrough {
			if name == "PATH" || name == "HOME" || name == "LANG" || name == "TZ" || name == "BASALT_OUTPUT_DIR" || strings.ContainsAny(name, "=\x00\r\n") {
				continue
			}
			if value, ok := os.LookupEnv(name); ok {
				args = append(args, "--env", name+"="+value)
			}
		}
	}
	args = append(args, "--entrypoint", cmd.Executable, opts.Image)
	return append(args, cmd.Args...)
}

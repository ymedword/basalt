//go:build e2e

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtemp"
	"github.com/ymedword/basalt/internal/tool"
)

// 同包 e2e 调用真实启动恢复，无生产导出测试接缝；子进程被杀后留真实容器。
// Same-package e2e exercises real startup recovery after killing the runner process.
type us20RecoveryHarness struct {
	deps     isolationDeps
	store    *postgres.DB
	sql      *sql.DB
	profile  string
	children []*exec.Cmd
}

func newUS20RecoveryHarness(t *testing.T, profile string) *us20RecoveryHarness {
	t.Helper()
	require.Equal(t, "linux", runtime.GOOS)
	require.NotZero(t, os.Geteuid())
	require.Equal(t, "user-2026-11-04-us20-wsl", os.Getenv("BASALT_US20_AUTH"))
	output := os.Getenv("BASALT_US20_APP_OUTPUT_TMPFS")
	require.True(t, filepath.IsAbs(output))
	entries, err := os.ReadDir(output)
	require.NoError(t, err)
	require.Empty(t, entries, "恢复专用挂载不能与其他测试/实例共用")
	root := t.TempDir()
	docker := tool.NewDockerCLI("docker")
	_, image, err := docker.ImageInspect(t.Context(), "basalt:us20-test")
	require.NoError(t, err)
	schema, err := pgtemp.New(t.Context(), "basalt_us20_recovery_")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, schema.Drop(context.Background())) })
	store, err := postgres.Open(t.Context(), postgres.Config{DSN: schema.DSN, ConnectTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	db, err := sql.Open("pgx", schema.DSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.Exec(`INSERT INTO runs(run_id,agent,channel,user_id,trigger,status,started_at,caller_id,authorizing_kind,actor) VALUES('run','demo','web','test','user','running',1,'local-admin','admin-builtin','admin-cli')`)
	require.NoError(t, err)
	runtimeName := "runc"
	toolName := "shell"
	if profile == "gvisor" {
		runtimeName = "runsc"
		toolName = "run_code"
	}
	cfg := cli.IsolationConfig{Backend: "docker", OutputRoot: output, ReleaseManifest: filepath.Join(root, "unused-signed-fixture"), RunAsUID: 65534, RunAsGID: 65534, MaxConcurrent: 2, MaxTotalCPUs: 1, MaxTotalMemoryMB: 512, MaxTotalTempBytes: 128 << 20, MaxInputBytes: 1 << 20, MaxTotalArtifactBytes: 2 << 20, Tools: map[string]string{toolName: profile}, Profiles: map[string]cli.IsolationProfileConfig{profile: {Runtime: runtimeName, Image: image, CPUs: 0.5, MemoryMB: 256, PIDs: 64, TmpfsMB: 8, OutputTmpfsMB: 8, MaxOutputBytes: 4096, TimeoutSeconds: 20}}}
	h := &us20RecoveryHarness{store: store, sql: db, profile: profile, deps: isolationDeps{Config: cfg, ShellConfig: cli.ShellConfig{AllowedCommands: []string{"true", "sleep"}}, Policy: tool.NewWhitelistPolicy(tool.PolicyOptions{Root: root, AllowRunCode: profile == "gvisor", AllowedCommands: []string{"true", "sleep"}}), Root: root, InstanceID: fmt.Sprintf("inst_%016x", time.Now().UnixNano()), Store: store, Reader: store, Docker: docker, ApprovedDigests: []string{image}}}
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents", "demo"), 0o755))
	t.Cleanup(func() {
		for _, child := range h.children {
			if child.ProcessState == nil {
				_ = child.Process.Kill()
				_ = child.Wait()
			}
		}
		names, err := docker.ListByLabel(context.Background(), "basalt.instance_id="+h.deps.InstanceID)
		require.NoError(t, err)
		for _, name := range names {
			require.NoError(t, docker.Remove(context.Background(), name))
		}
		require.NoError(t, sweepIsolationDirectories(context.Background(), output, nil))
	})
	return h
}

type us20CrashConfig struct {
	Root, Output, Instance, Profile, Image string
	ID                                     int64
}

func TestUS20_IsolationCrashChild(t *testing.T) {
	body := os.Getenv("BASALT_US20_CRASH_CHILD")
	if body == "" {
		return
	}
	var c us20CrashConfig
	require.NoError(t, json.Unmarshal([]byte(body), &c))
	runtimeName := "runc"
	if c.Profile == "gvisor" {
		runtimeName = "runsc"
	}
	r, err := tool.NewContainerRunner(tool.ContainerRunnerOptions{Docker: tool.NewDockerCLI("docker"), Root: c.Root, OutputRoot: c.Output, InstanceID: c.Instance, Profile: c.Profile, Runtime: runtimeName, Image: c.Image, ImageDigest: c.Image, ApprovedDigests: []string{c.Image}, CPUs: 0.5, MemoryMB: 256, PIDs: 64, TmpfsMB: 8, OutputTmpfsMB: 8, MaxOutputBytes: 4096, RunAsUID: 65534, RunAsGID: 65534})
	require.NoError(t, err)
	cmd := core.Command{Executable: "sleep", Args: []string{"30"}, Dir: filepath.Join(c.Root, "sandbox", "run", strconv.FormatInt(c.ID, 10), "task"), Isolation: &core.IsolationSpec{ToolInvocationID: c.ID, ContainerName: tool.SandboxContainerName(c.Instance, c.ID)}}
	if c.Profile == "gvisor" {
		cmd.Executable = "/bin/sh"
		cmd.Args = []string{"/task/main.sh"}
		cmd.Isolation.OutputDir = filepath.Join(c.Output, "run", strconv.FormatInt(c.ID, 10))
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	_, err = r.Run(ctx, cmd)
	require.NoError(t, err)
}

func (h *us20RecoveryHarness) crash(t *testing.T, tracked bool) int64 {
	t.Helper()
	name := "shell"
	if h.profile == "gvisor" {
		name = "run_code"
	}
	id, err := postgres.NewToolInvocationRepo(h.store).BeginInvocation(t.Context(), core.ToolInvocation{RunID: "run", ToolCallID: fmt.Sprint("crash-", time.Now().UnixNano()), ToolName: name, InputJSON: json.RawMessage(`{}`)})
	require.NoError(t, err)
	p := h.deps.Config.Profiles[h.profile]
	if tracked {
		require.NoError(t, h.store.BeginSandboxExecution(t.Context(), core.SandboxExecution{ToolInvocationID: id, RunID: "run", AgentName: "demo", InstanceID: h.deps.InstanceID, Profile: h.profile, Runtime: p.Runtime, ImageDigest: p.Image, ContainerName: tool.SandboxContainerName(h.deps.InstanceID, id), StartedAt: time.Now()}))
	}
	stage := filepath.Join(h.deps.Root, "sandbox", "run", fmt.Sprint(id), "task")
	require.NoError(t, os.MkdirAll(stage, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stage, "main.sh"), []byte("echo crash > /output/marker; sleep 30"), 0o644))
	output := filepath.Join(h.deps.Config.OutputRoot, "run", fmt.Sprint(id))
	require.NoError(t, os.MkdirAll(output, 0o777))
	require.NoError(t, os.Chmod(output, 0o777))
	body, err := json.Marshal(us20CrashConfig{Root: h.deps.Root, Output: h.deps.Config.OutputRoot, Instance: h.deps.InstanceID, Profile: h.profile, Image: p.Image, ID: id})
	require.NoError(t, err)
	child := exec.Command(os.Args[0], "-test.run=^TestUS20_IsolationCrashChild$")
	child.Env = append(os.Environ(), "BASALT_US20_CRASH_CHILD="+string(body))
	require.NoError(t, child.Start())
	h.children = append(h.children, child)
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", tool.SandboxContainerName(h.deps.InstanceID, id)).Output()
		return err == nil && string(out) == "true\n"
	}, 10*time.Second, 20*time.Millisecond)
	if h.profile == "gvisor" {
		require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(output, "marker")); return err == nil }, 5*time.Second, 20*time.Millisecond)
	}
	require.NoError(t, child.Process.Kill())
	require.Error(t, child.Wait(), "必须真实杀死执行宿主而非自然结束")
	return id
}

func (h *us20RecoveryHarness) recover(t *testing.T) *isolationRuntime {
	t.Helper()
	rt, err := buildIsolation(t.Context(), h.deps)
	require.NoError(t, err)
	names, err := h.deps.Docker.ListByLabel(t.Context(), "basalt.instance_id="+h.deps.InstanceID)
	require.NoError(t, err)
	require.Empty(t, names)
	return rt
}

func TestUS20_OCI_CrashResidue(t *testing.T) {
	h := newUS20RecoveryHarness(t, "oci")
	id := h.crash(t, true)
	h.recover(t)
	var state, cleanup string
	require.NoError(t, h.sql.QueryRow(`SELECT state,cleanup_state FROM sandbox_executions WHERE tool_invocation_id=$1`, id).Scan(&state, &cleanup))
	require.Equal(t, "unknown", state)
	require.Equal(t, "cleaned", cleanup)
}

func TestUS20_GVisor_CrashResidue(t *testing.T) {
	h := newUS20RecoveryHarness(t, "gvisor")
	id := h.crash(t, true)
	h.recover(t)
	require.NoDirExists(t, filepath.Join(h.deps.Config.OutputRoot, "run", fmt.Sprint(id)))
	var state, cleanup string
	require.NoError(t, h.sql.QueryRow(`SELECT state,cleanup_state FROM sandbox_executions WHERE tool_invocation_id=$1`, id).Scan(&state, &cleanup))
	require.Equal(t, "unknown", state)
	require.Equal(t, "cleaned", cleanup)
}

func TestUS20_OCI_OrphanCleanup(t *testing.T) {
	h := newUS20RecoveryHarness(t, "oci")
	h.crash(t, false)
	h.recover(t)
}

func TestUS20_OCI_OrphanDirSweep(t *testing.T) {
	h := newUS20RecoveryHarness(t, "oci")
	for _, root := range []string{filepath.Join(h.deps.Root, "sandbox"), h.deps.Config.OutputRoot} {
		dir := filepath.Join(root, "before-ledger", "1")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "data"), []byte("before pending"), 0o600))
	}
	h.recover(t)
	require.NoDirExists(t, filepath.Join(h.deps.Config.OutputRoot, "before-ledger"))
}

func TestUS20_OCI_RecoveryReleasesCapacity(t *testing.T) {
	h := newUS20RecoveryHarness(t, "oci")
	h.crash(t, true)
	h.crash(t, true)
	count, err := h.store.CountActiveSandboxExecutions(t.Context(), h.deps.InstanceID)
	require.NoError(t, err)
	require.Equal(t, 2, count)
	rt := h.recover(t)
	count, err = h.store.CountActiveSandboxExecutions(t.Context(), h.deps.InstanceID)
	require.NoError(t, err)
	require.Zero(t, count)
	// 恢复后必须再次同时占 N 槽，N+1 拒绝；不能用顺序 N+1 调用假装验证容量。
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		id, err := postgres.NewToolInvocationRepo(h.store).BeginInvocation(t.Context(), core.ToolInvocation{RunID: "run", ToolName: "shell", ToolCallID: fmt.Sprint("hold-", i), InputJSON: json.RawMessage(`{}`)})
		require.NoError(t, err)
		go func() {
			_, err := rt.Shell.Execute(ctx, core.ToolEnv{RunBound: true, RunID: "run", AgentName: "demo", AgentDir: filepath.Join(h.deps.Root, "agents", "demo"), ToolInvocationID: id, Timeout: 30 * time.Second}, json.RawMessage(`{"executable":"sleep","arguments":["15"]}`))
			done <- err
		}()
	}
	require.Eventually(t, func() bool {
		n, e := h.store.CountActiveSandboxExecutions(t.Context(), h.deps.InstanceID)
		return e == nil && n == 2
	}, 10*time.Second, 20*time.Millisecond)
	_, err = rt.Shell.Execute(t.Context(), core.ToolEnv{RunBound: true, RunID: "run", AgentName: "demo", AgentDir: filepath.Join(h.deps.Root, "agents", "demo"), ToolInvocationID: 99999, Timeout: time.Second}, json.RawMessage(`{"executable":"true","arguments":[]}`))
	require.ErrorContains(t, err, "admission_rejected")
	cancel()
	require.Error(t, <-done)
	require.Error(t, <-done)
}

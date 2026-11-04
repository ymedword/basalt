//go:build e2e

package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/isolation"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtemp"
	"github.com/ymedword/basalt/internal/tool"
)

// 本夹具直接贯通生产 ToolExecutor/Policy/Runner/PG，既有 ReAct 消息路径另由 core 回归覆盖。
// The fixture runs the production executor, policy, runner and PG with no model endpoint.
type us20Harness struct {
	root, output, agent, instance, image, profile string
	uid                                           int
	docker                                        tool.ContainerCLI
	runner                                        *tool.ContainerRunner
	opts                                          tool.ContainerRunnerOptions
	codeOpts                                      tool.RunCodeOptions
	policy                                        core.Policy
	gate                                          *tool.SandboxAdmission
	store                                         *postgres.DB
	sql                                           *sql.DB
	executor                                      *core.ToolExecutor
	seq                                           atomic.Int64
	tools                                         us20ToolPool
}

type us20ToolPool map[string]core.Tool

type us20ObservedCLI struct {
	tool.ContainerCLI
	t *testing.T
}

func (d us20ObservedCLI) Wait(ctx context.Context, id string) (int, error) {
	code, err := d.ContainerCLI.Wait(ctx, id)
	if code != 0 && err == nil {
		logs, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		body, _, logErr := d.ContainerCLI.Logs(logs, id, 4096)
		d.t.Logf("nonzero container=%s exit=%d logs=%q read_error=%v", id, code, body, logErr)
	}
	return code, err
}

func (p us20ToolPool) Resolve(name string) (core.Tool, bool) { v, ok := p[name]; return v, ok }
func (p us20ToolPool) Specs(names []string) []core.ToolSpec  { return nil }

func newUS20OCIHarness(t *testing.T, uid ...int) *us20Harness {
	return newUS20IsolationHarness(t, "oci", uid...)
}

func newUS20GVisorHarness(t *testing.T, uid ...int) *us20Harness {
	return newUS20IsolationHarness(t, "gvisor", uid...)
}

func newUS20IsolationHarness(t *testing.T, profile string, uid ...int) *us20Harness {
	t.Helper()
	require.Equal(t, "linux", runtime.GOOS, "真实 Linux 必验，不得 skip")
	require.NotZero(t, os.Geteuid(), "服务必须以非 root UID 验证")
	require.Equal(t, "user-2026-11-04-us20-wsl", os.Getenv("BASALT_US20_AUTH"), "缺本地隔离验收授权登记")
	outputBase := os.Getenv("BASALT_US20_OUTPUT_TMPFS")
	require.True(t, filepath.IsAbs(outputBase), "需要专用 tmpfs 验收挂载")
	h := &us20Harness{root: t.TempDir(), profile: profile, uid: 65534, tools: us20ToolPool{}}
	if len(uid) > 0 {
		h.uid = uid[0]
	}
	h.instance = "us20_" + filepath.Base(h.root) + fmt.Sprint(time.Now().UnixNano())
	h.agent = filepath.Join(h.root, "agents", "demo")
	require.NoError(t, os.MkdirAll(h.agent, 0o755))
	var err error
	h.output, err = os.MkdirTemp(outputBase, "instance-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(h.output)) })
	h.docker = us20ObservedCLI{ContainerCLI: tool.NewDockerCLI("docker"), t: t}
	_, runtimes, err := h.docker.Version(t.Context())
	require.NoError(t, err)
	runtimeName := "runc"
	if profile == "gvisor" {
		runtimeName = "runsc"
	}
	require.True(t, runtimes[runtimeName], "必验运行时缺失")
	_, h.image, err = h.docker.ImageInspect(t.Context(), "basalt:us20-test")
	require.NoError(t, err)
	approved := us20SignedImages(t, h.image)
	h.opts = tool.ContainerRunnerOptions{Docker: h.docker, Root: h.root, OutputRoot: h.output, InstanceID: h.instance, Profile: profile, Runtime: runtimeName, Image: h.image, ImageDigest: h.image, ApprovedDigests: approved, CPUs: 0.5, MemoryMB: 64, PIDs: 32, TmpfsMB: 8, OutputTmpfsMB: 8, MaxOutputBytes: 4096, RunAsUID: h.uid, RunAsGID: h.uid, Interpreters: []string{"awk"}}
	if profile == "gvisor" {
		h.opts.MemoryMB = 256
		h.opts.PIDs = 64
	}
	h.runner, err = tool.NewContainerRunner(h.opts)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		names, err := h.docker.ListByLabel(ctx, "basalt.instance_id="+h.instance)
		require.NoError(t, err)
		for _, name := range names {
			_, err := h.runner.RemoveByName(ctx, name)
			require.NoError(t, err)
		}
	})
	require.NoError(t, h.runner.Probe(t.Context()), "真实跨 UID 探针")
	schema, err := pgtemp.New(t.Context(), "basalt_us20_")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, schema.Drop(context.Background())) })
	h.store, err = postgres.Open(t.Context(), postgres.Config{DSN: schema.DSN, ConnectTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.store.Close()) })
	h.sql, err = sql.Open("pgx", schema.DSN)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.sql.Close()) })
	_, err = h.sql.Exec(`INSERT INTO runs(run_id,agent,channel,user_id,trigger,status,started_at,caller_id,authorizing_kind,actor) VALUES('run','demo','web','test','user','running',1,'local-admin','admin-builtin','admin-cli')`)
	require.NoError(t, err)
	h.policy = tool.NewWhitelistPolicy(tool.PolicyOptions{Root: h.root, AllowRunCode: profile == "gvisor", AllowedCommands: []string{"wget", "touch", "env", "ls", "sleep", "true", "false", "id", "stat", "cat", "dd"}, Interpreters: []string{"awk"}})
	h.gate = tool.NewSandboxAdmission()
	require.NoError(t, h.gate.RefreshBaseline(t.Context(), h.store, h.instance))
	if profile == "oci" {
		h.tools["shell"] = tool.NewShellTool(h.policy, h.runner).WithIsolation(tool.ShellIsolationOptions{Ledger: tool.NewSandboxLedger(h.store, h.instance, profile, runtimeName, h.image, nil), Admission: h.gate, Root: h.root, Interpreters: []string{"awk"}, MaxConcurrent: 2, MaxTimeout: 20 * time.Second})
	} else {
		h.codeOpts = tool.RunCodeOptions{Policy: h.policy, Runner: h.runner, Store: h.store, Admission: h.gate, Root: h.root, OutputRoot: h.output, InstanceID: h.instance, Profile: profile, Runtime: runtimeName, ImageDigest: h.image, MaxConcurrent: 2, MaxTimeout: 20 * time.Second, MaxCodeBytes: 65536, MaxInputFiles: 16, MaxInputBytes: 1 << 20, MaxArtifactFiles: 16, MaxArtifactBytes: 1 << 20, MaxTotalArtifactBytes: 2 << 20, PublishMu: &sync.Mutex{}}
		h.tools["run_code"] = tool.NewRunCodeTool(h.codeOpts)
	}
	h.executor = core.NewToolExecutor(h.tools, postgres.NewToolInvocationRepo(h.store), nil)
	t.Logf("service_uid=%d task_uid=%d runtime=%s image=%s instance=%s", os.Geteuid(), h.uid, runtimeName, h.image, h.instance)
	return h
}

func us20SignedImages(t *testing.T, image string) []string {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ref := "basalt:us20-test@" + image
	body, err := json.Marshal(isolation.ImageManifest{Schema: 2, AppBase: ref, Postgres: ref, Sandbox: isolation.SandboxImages{OCI: ref, GVisor: ref}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "image-lock.json")
	require.NoError(t, os.WriteFile(path, body, 0o600))
	require.NoError(t, os.WriteFile(path+".sig", ed25519.Sign(private, body), 0o600))
	trusted, err := isolation.LoadApprovedImages(path, pub)
	require.NoError(t, err)
	require.True(t, trusted.Approved(image))
	return []string{image}
}

type us20RunResult struct {
	Content                        string
	ID                             int64
	State                          core.SandboxState
	CleanupState                   core.CleanupState
	ExitCode                       int
	Reason, ErrorKind, AuditStatus string
	Artifacts                      int
}

func (h *us20Harness) invoke(t *testing.T, ctx context.Context, name string, input any) us20RunResult {
	t.Helper()
	callID := fmt.Sprint("call-", h.seq.Add(1))
	body, err := json.Marshal(input)
	require.NoError(t, err)
	env := core.ToolEnv{RunBound: true, RunID: "run", AgentName: "demo", AgentDir: h.agent, Timeout: 30 * time.Second, MaxOutputBytes: 8192}
	result := h.executor.Execute(ctx, env, []string{name}, core.ToolCall{ID: callID, Name: name, Arguments: string(body)})
	out := us20RunResult{Content: result.Content}
	require.NoError(t, h.sql.QueryRow(`SELECT id,status,COALESCE(error_kind,'') FROM tool_invocations WHERE tool_call_id=$1`, callID).Scan(&out.ID, &out.AuditStatus, &out.ErrorKind))
	err = h.sql.QueryRow(`SELECT state,cleanup_state,COALESCE(exit_code,0),COALESCE(error_reason,''),artifact_count FROM sandbox_executions WHERE tool_invocation_id=$1`, out.ID).Scan(&out.State, &out.CleanupState, &out.ExitCode, &out.Reason, &out.Artifacts)
	if err != sql.ErrNoRows {
		require.NoError(t, err)
	}
	return out
}

func (h *us20Harness) Shell(t *testing.T, executable string, args ...string) us20RunResult {
	t.Helper()
	if args == nil {
		args = []string{}
	}
	return h.invoke(t, t.Context(), "shell", map[string]any{"executable": executable, "arguments": args})
}

func (h *us20Harness) waitRunning(t *testing.T, count int) []string {
	t.Helper()
	var names []string
	require.Eventually(t, func() bool {
		out, err := us20Docker(t, "ps", "--filter", "label=basalt.instance_id="+h.instance, "--format", "{{.Names}}")
		if err != nil {
			return false
		}
		names = strings.Fields(out)
		return len(names) == count
	}, 10*time.Second, 20*time.Millisecond, "真实运行容器未到达目标数量")
	return names
}

func (h *us20Harness) RunCode(t *testing.T, code string) us20RunResult {
	t.Helper()
	return h.invoke(t, t.Context(), "run_code", map[string]any{"code": code})
}

func (h *us20Harness) assertClean(t *testing.T) {
	t.Helper()
	names, err := h.docker.ListByLabel(t.Context(), "basalt.instance_id="+h.instance)
	require.NoError(t, err)
	require.Empty(t, names)
	count, err := h.store.CountActiveSandboxExecutions(t.Context(), h.instance)
	require.NoError(t, err)
	require.Zero(t, count)
}

func us20Docker(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}

func (h *us20Harness) control(t *testing.T, entry string, args ...string) (string, error) {
	t.Helper()
	name := fmt.Sprint("us20-control-", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = us20Docker(t, "rm", "-f", name) })
	a := []string{"run", "--rm", "--name", name, "--label", "basalt.us20.control=true", "--cpus", "0.5", "--memory", "64m", "--pids-limit", "32", "--user", fmt.Sprint(h.uid), "--entrypoint", entry, h.image}
	return us20Docker(t, append(a, args...)...)
}

func (h *us20Harness) listener(t *testing.T) string {
	t.Helper()
	ip, err := us20Docker(t, "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
	require.NoError(t, err)
	ln, err := net.Listen("tcp", net.JoinHostPort(strings.TrimSpace(ip), "0"))
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("us20-local-control")) }), ReadHeaderTimeout: time.Second}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Serve(ln) }()
	body, err := h.control(t, "wget", "-q", "-T", "2", "-O-", "http://"+ln.Addr().String()+"/")
	require.NoError(t, err, "可达控制组: %s", body)
	require.Contains(t, body, "us20-local-control")
	return ln.Addr().String()
}

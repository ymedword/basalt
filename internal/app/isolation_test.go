package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

func TestUS20_RecoveryManyEmptyRuns(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 1025; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprint("run-", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := sweepIsolationDirectories(context.Background(), root, nil); err != nil {
		t.Fatalf("completed run parents blocked startup: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty run parents remain: %d %v", len(entries), err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := os.Mkdir(filepath.Join(root, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sweepIsolationDirectories(ctx, root, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("recovery deadline ignored: %v", err)
	}
}

type isolationTestCLI struct {
	names        []string
	removed      []string
	removeErrors map[string]error
	listErr      error
	runtimes     map[string]bool
}

func (c *isolationTestCLI) Version(context.Context) (string, map[string]bool, error) {
	return "test", c.runtimes, nil
}

func (c *isolationTestCLI) ImageInspect(context.Context, string) ([]string, string, error) {
	return nil, "", errors.New("unexpected inspect")
}

func (c *isolationTestCLI) Create(context.Context, []string) (string, error) {
	return "", errors.New("unexpected create")
}

func (c *isolationTestCLI) Start(context.Context, string) error {
	return errors.New("unexpected start")
}

func (c *isolationTestCLI) Wait(context.Context, string) (int, error) {
	return 0, errors.New("unexpected wait")
}

func (c *isolationTestCLI) Logs(context.Context, string, int) (string, bool, error) {
	return "", false, nil
}

func (c *isolationTestCLI) Remove(_ context.Context, name string) error {
	c.removed = append(c.removed, name)
	if err := c.removeErrors[name]; err != nil {
		return err
	}
	c.names = slices.DeleteFunc(c.names, func(n string) bool { return n == name })
	return nil
}

func (c *isolationTestCLI) ListByLabel(context.Context, string) ([]string, error) {
	return slices.Clone(c.names), c.listErr
}

type isolationTestStore struct {
	rows                       []core.SandboxExecution
	marked                     []core.SandboxExecutionResult
	listErr, countErr, markErr error
}

func (s *isolationTestStore) BeginSandboxExecution(context.Context, core.SandboxExecution) error {
	return nil
}

func (s *isolationTestStore) MarkSandboxRunning(context.Context, int64, time.Time) error { return nil }

func (s *isolationTestStore) FinishSandboxExecution(context.Context, core.SandboxExecutionResult) error {
	return nil
}

func (s *isolationTestStore) CountActiveSandboxExecutions(context.Context, string) (int, error) {
	n := 0
	for _, r := range s.rows {
		if r.CleanupState != core.CleanupCleaned {
			n++
		}
	}
	return n, s.countErr
}

func (s *isolationTestStore) MarkSandboxCleaned(_ context.Context, id int64, state core.SandboxState, cleanup core.CleanupState, reason string, _ time.Time) error {
	if s.markErr != nil {
		return s.markErr
	}
	s.marked = append(s.marked, core.SandboxExecutionResult{ToolInvocationID: id, State: state, CleanupState: cleanup, ErrorReason: reason})
	for i := range s.rows {
		if s.rows[i].ToolInvocationID == id {
			s.rows[i].State = state
			s.rows[i].CleanupState = cleanup
		}
	}
	return nil
}

func (s *isolationTestStore) PendingSandboxExecutions(context.Context, string) ([]core.SandboxExecution, error) {
	return slices.Clone(s.rows), s.listErr
}

func (s *isolationTestStore) SandboxExecutionsByRun(context.Context, string) ([]core.SandboxExecutionView, error) {
	return []core.SandboxExecutionView{}, nil
}

func isolationTestDeps(t *testing.T) (isolationDeps, *isolationTestCLI, *isolationTestStore) {
	t.Helper()
	root := t.TempDir()
	digest := "sha256:" + strings.Repeat("1", 64)
	docker := &isolationTestCLI{runtimes: map[string]bool{"runsc": true}, removeErrors: map[string]error{}}
	store := &isolationTestStore{}
	cfg := cli.IsolationConfig{Backend: "docker", OutputRoot: t.TempDir(), ReleaseManifest: filepath.Join(root, "manifest.json"), RunAsUID: 65534, RunAsGID: 65534, MaxConcurrent: 1, MaxTotalCPUs: 1, MaxTotalMemoryMB: 64, MaxTotalTempBytes: 64 << 20, MaxInputBytes: 1 << 20, MaxTotalArtifactBytes: 1 << 20, Tools: map[string]string{"run_code": "gvisor"}, Profiles: map[string]cli.IsolationProfileConfig{"gvisor": {Runtime: "runsc", Image: "basalt:test@" + digest, CPUs: 1, MemoryMB: 64, PIDs: 32, TmpfsMB: 8, OutputTmpfsMB: 8, MaxOutputBytes: 1024, TimeoutSeconds: 10}}}
	return isolationDeps{Config: cfg, Policy: tool.NewWhitelistPolicy(tool.PolicyOptions{Root: root, AllowRunCode: true}), Root: root, InstanceID: "inst_0000000000000001", Store: store, Reader: store, Docker: docker, ApprovedDigests: []string{digest}, TmpfsSize: func(string) (int64, error) { return 16 << 20, nil }, Probe: func(context.Context, *tool.ContainerRunner) error { return nil }}, docker, store
}

func TestUS20_Probe_RuntimeMissingRefusesStartup(t *testing.T) {
	deps, docker, _ := isolationTestDeps(t)
	deps.Probe = nil
	docker.runtimes = map[string]bool{"runc": true}
	if _, err := buildIsolation(context.Background(), deps); err == nil {
		t.Fatal("missing gVisor did not refuse startup")
	}
}

func seedIsolationRecovery(t *testing.T, deps isolationDeps, docker *isolationTestCLI, store *isolationTestStore) {
	t.Helper()
	row := core.SandboxExecution{ToolInvocationID: 1, RunID: "run", InstanceID: deps.InstanceID, Profile: "gvisor", Runtime: "runsc", ImageDigest: deps.ApprovedDigests[0], ContainerName: tool.SandboxContainerName(deps.InstanceID, 1), State: core.SandboxRunning, CleanupState: core.CleanupPending}
	store.rows = []core.SandboxExecution{row}
	docker.names = []string{row.ContainerName, tool.CleanupContainerName(deps.InstanceID, 1)}
	for _, p := range []string{filepath.Join(deps.Root, "sandbox", "run", "1"), filepath.Join(deps.Config.OutputRoot, "run", "1"), filepath.Join(deps.Root, "sandbox", "before-ledger", "2"), filepath.Join(deps.Config.OutputRoot, "before-ledger", "2")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "data"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUS20_CleanupOrphans(t *testing.T) {
	deps, docker, store := isolationTestDeps(t)
	seedIsolationRecovery(t, deps, docker, store)
	docker.names = append(docker.names, tool.SandboxContainerName(deps.InstanceID, 2))
	rt, err := buildIsolation(context.Background(), deps)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Admission == nil || rt.RunCode == nil || len(docker.names) != 0 || len(store.marked) != 1 || store.marked[0].State != core.SandboxUnknown || store.marked[0].CleanupState != core.CleanupCleaned {
		t.Fatalf("recovery incomplete %+v %+v", docker, store.marked)
	}
	for _, p := range []string{filepath.Join(deps.Root, "sandbox", "run", "1"), filepath.Join(deps.Config.OutputRoot, "before-ledger", "2")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan survived %s: %v", p, err)
		}
	}
}

func TestUS20_CleanupOrphansSkipsHandled(t *testing.T) {
	deps, docker, store := isolationTestDeps(t)
	seedIsolationRecovery(t, deps, docker, store)
	for _, name := range docker.names {
		docker.removeErrors[name] = errors.New("cannot remove")
	}
	var logs bytes.Buffer
	deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := buildIsolation(context.Background(), deps); err != nil {
		t.Fatalf("tracked failure became fatal: %v", err)
	}
	if len(docker.removed) != 2 || len(store.marked) != 1 || store.marked[0].CleanupState != core.CleanupFailed || !strings.Contains(logs.String(), "WARN") {
		t.Fatalf("handled deleted twice or not retained: %v %+v %s", docker.removed, store.marked, logs.String())
	}
	if _, err := os.Stat(filepath.Join(deps.Config.OutputRoot, "run", "1", "data")); err != nil {
		t.Fatal("deleted output of unknown container")
	}
}

func TestUS20_CleanupOrphansFailClosed(t *testing.T) {
	deps, docker, _ := isolationTestDeps(t)
	name := tool.SandboxContainerName(deps.InstanceID, 99)
	docker.names = []string{name}
	docker.removeErrors[name] = errors.New("cannot remove")
	if _, err := buildIsolation(context.Background(), deps); err == nil {
		t.Fatal("untracked resource admitted startup")
	}
}

func TestUS20_CleanupOrphansListFailure(t *testing.T) {
	deps, docker, _ := isolationTestDeps(t)
	docker.listErr = errors.New("daemon unavailable")
	if _, err := buildIsolation(context.Background(), deps); err == nil {
		t.Fatal("failed enumeration admitted startup")
	}
}

func TestUS20_IsolationBaselineFailure(t *testing.T) {
	deps, _, store := isolationTestDeps(t)
	store.countErr = errors.New("db unavailable")
	if _, err := buildIsolation(context.Background(), deps); err == nil {
		t.Fatal("missing baseline admitted startup")
	}
}

func TestUS20_IsolationDisabled(t *testing.T) {
	rt, err := buildIsolation(context.Background(), isolationDeps{})
	if err != nil || rt != nil {
		t.Fatalf("legacy config: %+v %v", rt, err)
	}
}

func TestUS20_DeclaredToolsMatchRegistry(t *testing.T) {
	want := []string{"http_get", "http_post", "list_dir", "manage_schedule", "notify", "read_file", "recall_memory", "run_code", "save_memory", "shell", "web_search", "write_file"}
	if !slices.Equal(declaredToolNames, want) {
		t.Fatalf("builtin declaration drift: %v", declaredToolNames)
	}
	registry := tool.NewRegistry(nil)
	code := tool.NewRunCodeTool(tool.RunCodeOptions{})
	if err := registry.Register(code); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(registry.Names(), []string{"run_code"}) {
		t.Fatal("run_code missing actual registry")
	}
}

func TestUS20_BuiltinCatalogIncludesRunCode(t *testing.T) {
	registry := tool.NewRegistry(nil)
	if err := registry.Register(tool.NewRunCodeTool(tool.RunCodeOptions{})); err != nil {
		t.Fatal(err)
	}
	port := newToolCatalogPort(registry, nil)
	if port.source("run_code") != "builtin" {
		t.Fatal("run_code not a builtin")
	}
}

func TestUS20_MCPBoundaryUnchanged(t *testing.T) {
	remote := &isolationMCPTool{}
	runtime := newMCPRuntime([]core.MCPServerConfig{{Name: "alpha", Transport: "stdio", Command: "allowed-mcp"}}, []tool.MCPConnection{{Server: "alpha", Tools: []core.Tool{remote}}})
	before := runtime.Tools("alpha")
	deps, _, _ := isolationTestDeps(t)
	if _, err := buildIsolation(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, []string{"search_docs"}) || !slices.Equal(runtime.Tools("alpha"), before) {
		t.Fatal("isolation changed MCP binding")
	}
	result, err := remote.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{}`))
	if err != nil || result.Content != "低信任 MCP 返回" || remote.calls != 1 {
		t.Fatal("MCP execution rerouted")
	}
	if _, err := tool.FlowAdapterFor(context.Background(), tool.NewRunCodeTool(tool.RunCodeOptions{}), tool.NewSchemaCompiler()); !errors.Is(err, tool.ErrFlowToolUnavailable) {
		t.Fatal("run_code gained implicit Flow execution authority")
	}
}

type isolationMCPTool struct{ calls int }

func (t *isolationMCPTool) Name() string                 { return "search_docs" }
func (t *isolationMCPTool) Description() string          { return "MCP fixture" }
func (t *isolationMCPTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (t *isolationMCPTool) Execute(context.Context, core.ToolEnv, json.RawMessage) (core.Result, error) {
	t.calls++
	return core.Result{Content: "低信任 MCP 返回"}, nil
}

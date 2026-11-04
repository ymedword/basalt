package tool

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

type us20CodeRunner struct {
	calls int
	run   func(context.Context, core.Command) (core.CommandResult, error)
}

func (r *us20CodeRunner) Run(ctx context.Context, cmd core.Command) (core.CommandResult, error) {
	r.calls++
	if r.run != nil {
		return r.run(ctx, cmd)
	}
	return core.CommandResult{Output: "ok", Execution: &core.CommandExecution{State: core.SandboxFinished, CleanupState: core.CleanupCleaned}}, nil
}

func us20CodeTool(t *testing.T, allow bool) (*RunCodeTool, core.ToolEnv, *us20Store, *us20CodeRunner) {
	t.Helper()
	root := t.TempDir()
	agent := filepath.Join(root, "agents", "demo")
	if err := os.MkdirAll(agent, 0o755); err != nil {
		t.Fatal(err)
	}
	store := &us20Store{}
	runner := &us20CodeRunner{}
	opts := RunCodeOptions{Policy: NewWhitelistPolicy(PolicyOptions{Root: root, AllowRunCode: allow}), Runner: runner, Store: store, Admission: us20Admission(t, store), Root: root, OutputRoot: t.TempDir(), InstanceID: "inst_test", Profile: "gvisor", Runtime: "runsc", ImageDigest: "sha256:" + strings.Repeat("1", 64), MaxConcurrent: 2, MaxTimeout: time.Minute, MaxCodeBytes: 65536, MaxInputFiles: 16, MaxInputBytes: 1024, MaxArtifactFiles: 16, MaxArtifactBytes: 1024, MaxTotalArtifactBytes: 2048, PublishMu: &sync.Mutex{}}
	return NewRunCodeTool(opts), core.ToolEnv{RunBound: true, RunID: "run-1", AgentName: "demo", AgentDir: agent, ToolInvocationID: 1, Timeout: time.Minute}, store, runner
}

func TestUS20_RunCode_PolicyDenied(t *testing.T) {
	tool, env, s, r := us20CodeTool(t, false)
	_, err := tool.Execute(context.Background(), env, json.RawMessage(`{"code":"echo hi"}`))
	var v *core.SandboxViolationError
	if !errors.As(err, &v) || v.Reason != "" || r.calls != 0 || len(s.records) != 0 {
		t.Fatalf("denial bypassed: %v calls=%d rows=%d", err, r.calls, len(s.records))
	}
}

func TestUS20_RunCode_PolicyAllowed(t *testing.T) {
	tool, env, s, r := us20CodeTool(t, true)
	r.run = func(_ context.Context, cmd core.Command) (core.CommandResult, error) {
		body, err := os.ReadFile(filepath.Join(cmd.Dir, "main.sh"))
		if err != nil || string(body) != "echo hi" {
			t.Fatalf("script missing %q %v", body, err)
		}
		if cmd.Executable != "/bin/sh" || cmd.Isolation == nil || cmd.Env != nil {
			t.Fatal("wrong generated-code command")
		}
		return core.CommandResult{Output: "hi", Execution: &core.CommandExecution{State: core.SandboxFinished, CleanupState: core.CleanupCleaned}}, nil
	}
	res, err := tool.Execute(context.Background(), env, json.RawMessage(`{"code":"echo hi"}`))
	if err != nil || !strings.Contains(res.Content, "hi") || r.calls != 1 || len(s.results) != 1 || s.results[0].CleanupState != core.CleanupCleaned {
		t.Fatalf("allowed: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(tool.opts.Root, "sandbox", env.RunID, "1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staging survived")
	}
}

func TestUS20_NoFallback_Unconfigured(t *testing.T) {
	tool, env, _, r := us20CodeTool(t, true)
	tool.opts.Runner = nil
	_, err := tool.Execute(context.Background(), env, json.RawMessage(`{"code":"echo hi"}`))
	if isolationReason(err) != "config_missing" || r.calls != 0 {
		t.Fatalf("unconfigured fallback: %v", err)
	}
}

func TestUS20_RunCodeInputLimits(t *testing.T) {
	for _, input := range []string{`{}`, `{"code":""}`, `{"code":"x","extra":1}`, `{"code":"x","timeout_seconds":0}`, `{"code":"x","timeout_seconds":301}`, `{"code":"x","inputs":[""]}`, `{"code":"x"}{}`, `{"code":null}`} {
		if _, err := decodeRunCodeInput([]byte(input)); err == nil {
			t.Fatalf("invalid input accepted %s", input)
		}
	}
	tool, env, s, r := us20CodeTool(t, true)
	tool.opts.MaxInputBytes = 5
	if _, err := tool.Execute(context.Background(), env, json.RawMessage(`{"code":"123456"}`)); err == nil || len(s.records) != 0 || r.calls != 0 {
		t.Fatalf("script omitted from staging budget: %v", err)
	}
}

func TestUS20_RunCodePrewriteFailure(t *testing.T) {
	tool, env, s, r := us20CodeTool(t, true)
	s.beginErr = errors.New("DB unavailable")
	if _, err := tool.Execute(context.Background(), env, json.RawMessage(`{"code":"true"}`)); err == nil || r.calls != 0 {
		t.Fatalf("prewrite failure executed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tool.opts.Root, "sandbox", env.RunID, "1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("prewrite failure leaked stage")
	}
}

func TestUS20_RunCodeOptionalNullRejected(t *testing.T) {
	for _, input := range []string{`{"code":"true","inputs":null}`, `{"code":"true","timeout_seconds":null}`} {
		t.Run(input, func(t *testing.T) {
			tool, env, store, runner := us20CodeTool(t, true)
			_, err := tool.Execute(context.Background(), env, json.RawMessage(input))
			var invalid *core.InvalidInputError
			if !errors.As(err, &invalid) || runner.calls != 0 || len(store.records) != 0 {
				t.Fatalf("optional null executed: err=%v calls=%d records=%d", err, runner.calls, len(store.records))
			}
		})
	}
	for _, input := range []string{`{"code":"true"}`, `{"code":"true","inputs":[],"timeout_seconds":1}`} {
		if _, err := decodeRunCodeInput(json.RawMessage(input)); err != nil {
			t.Fatalf("valid optional input rejected: %v", err)
		}
	}
}

func TestUS20_RunCodeTimeoutNarrows(t *testing.T) {
	tool, env, _, r := us20CodeTool(t, true)
	env.Timeout = 40 * time.Millisecond
	r.run = func(ctx context.Context, _ core.Command) (core.CommandResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > env.Timeout {
			t.Fatal("deadline enlarged")
		}
		return core.CommandResult{Execution: &core.CommandExecution{State: core.SandboxTimeout, CleanupState: core.CleanupCleaned}}, context.DeadlineExceeded
	}
	_, err := tool.Execute(context.Background(), env, json.RawMessage(`{"code":"sleep 1","timeout_seconds":300}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout lost %v", err)
	}
}

func TestUS20_PublishMuPrivateWhenNil(t *testing.T) {
	a := NewRunCodeTool(RunCodeOptions{})
	b := NewRunCodeTool(RunCodeOptions{})
	if a.opts.PublishMu == nil || b.opts.PublishMu == nil || a.opts.PublishMu == b.opts.PublishMu {
		t.Fatal("nil publish lock not private")
	}
}

func TestUS20_RunCodeArtifactCancellation(t *testing.T) {
	tool, env, store, runner := us20CodeTool(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.run = func(_ context.Context, cmd core.Command) (core.CommandResult, error) {
		if err := os.WriteFile(filepath.Join(cmd.Isolation.OutputDir, "out"), []byte("cancel me"), 0o644); err != nil {
			t.Fatal(err)
		}
		cancel()
		return core.CommandResult{Execution: &core.CommandExecution{State: core.SandboxFinished, CleanupState: core.CleanupCleaned}}, nil
	}
	_, err := tool.Execute(ctx, env, json.RawMessage(`{"code":"true"}`))
	if !errors.Is(err, context.Canceled) || isolationReason(err) != "canceled" {
		t.Fatalf("cancellation identity lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.AgentDir, "artifacts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled artifacts published")
	}
	if len(store.results) != 1 || store.results[0].CleanupState != core.CleanupCleaned {
		t.Fatal("cancellation cleanup not recorded")
	}
}

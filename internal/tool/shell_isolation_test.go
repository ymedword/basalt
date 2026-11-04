package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ymedword/basalt/internal/core"
)

func TestUS20_ShellLocalUnchanged(t *testing.T) {
	r := &us20CodeRunner{run: func(_ context.Context, cmd core.Command) (core.CommandResult, error) {
		if cmd.Isolation != nil {
			t.Fatal("local request gained isolation")
		}
		return core.CommandResult{Output: "local", ExitCode: 3}, nil
	}}
	p := NewWhitelistPolicy(PolicyOptions{Root: t.TempDir(), AllowedCommands: []string{"echo"}})
	res, err := NewShellTool(p, r).Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{"executable":"echo"}`))
	if err != nil || res.Content != "退出码：3\nlocal" || r.calls != 1 {
		t.Fatalf("local drift: %+v %v", res, err)
	}
}

func TestUS20_ShellIsolationLedger(t *testing.T) {
	for _, fail := range []bool{false, true} {
		tool, env, s, r := us20CodeTool(t, true)
		if fail {
			s.beginErr = errors.New("db down")
		}
		p := NewWhitelistPolicy(PolicyOptions{Root: tool.opts.Root, AllowedCommands: []string{"echo"}})
		shell := NewShellTool(p, r).WithIsolation(ShellIsolationOptions{Ledger: NewSandboxLedger(s, "inst_test", "oci", "runc", tool.opts.ImageDigest, nil), Admission: tool.opts.Admission, Root: tool.opts.Root, MaxConcurrent: 2})
		r.run = func(_ context.Context, cmd core.Command) (core.CommandResult, error) {
			if cmd.Isolation == nil || cmd.Isolation.OutputDir != "" {
				t.Fatal("shell isolation shape")
			}
			return core.CommandResult{Execution: &core.CommandExecution{State: core.SandboxFinished, CleanupState: core.CleanupCleaned}}, nil
		}
		_, err := shell.Execute(context.Background(), env, json.RawMessage(`{"executable":"echo"}`))
		if fail {
			if err == nil || r.calls != 0 {
				t.Fatal("failed prewrite executed")
			}
		} else if err != nil || r.calls != 1 || len(s.results) != 1 || s.results[0].CleanupState != core.CleanupCleaned {
			t.Fatalf("ledger chain: %v", err)
		}
	}
}

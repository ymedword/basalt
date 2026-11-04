package core_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

type isolationAuditLedger struct {
	audit            *execAudit
	t                *testing.T
	begins, finishes int
}

func (s *isolationAuditLedger) BeginSandboxExecution(_ context.Context, r core.SandboxExecution) error {
	if s.audit.count() != 1 || s.audit.row(0).terminal || r.ToolInvocationID != 1 {
		s.t.Fatal("sandbox ledger preceded audit pending")
	}
	s.begins++
	return nil
}

func (s *isolationAuditLedger) MarkSandboxRunning(context.Context, int64, time.Time) error {
	return nil
}

func (s *isolationAuditLedger) FinishSandboxExecution(context.Context, core.SandboxExecutionResult) error {
	s.finishes++
	return nil
}

func (s *isolationAuditLedger) CountActiveSandboxExecutions(context.Context, string) (int, error) {
	return 0, nil
}

func (s *isolationAuditLedger) MarkSandboxCleaned(context.Context, int64, core.SandboxState, core.CleanupState, string, time.Time) error {
	return nil
}

type isolationAuditRunner struct {
	t      *testing.T
	ledger *isolationAuditLedger
	calls  int
}

func (r *isolationAuditRunner) Run(_ context.Context, cmd core.Command) (core.CommandResult, error) {
	r.calls++
	if r.ledger.begins != 1 || cmd.Isolation == nil || cmd.Isolation.ToolInvocationID != 1 {
		r.t.Fatal("execution preceded lifecycle prewrite")
	}
	return core.CommandResult{Output: "ok", Execution: &core.CommandExecution{State: core.SandboxFinished, CleanupState: core.CleanupCleaned}}, nil
}

func TestUS20_AuditSingleRow(t *testing.T) {
	for _, mode := range []string{"success", "denied", "audit_failed"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			agent := filepath.Join(root, "agents", "demo")
			if err := os.MkdirAll(agent, 0o755); err != nil {
				t.Fatal(err)
			}
			audit := &execAudit{}
			if mode == "audit_failed" {
				audit.failStart = errors.New("unavailable")
			}
			ledger := &isolationAuditLedger{audit: audit, t: t}
			runner := &isolationAuditRunner{t: t, ledger: ledger}
			gate := tool.NewSandboxAdmission()
			if err := gate.RefreshBaseline(context.Background(), ledger, "inst_test"); err != nil {
				t.Fatal(err)
			}
			code := tool.NewRunCodeTool(tool.RunCodeOptions{Policy: tool.NewWhitelistPolicy(tool.PolicyOptions{Root: root, AllowRunCode: mode != "denied"}), Runner: runner, Store: ledger, Admission: gate, Root: root, OutputRoot: t.TempDir(), InstanceID: "inst_test", Profile: "gvisor", Runtime: "runsc", MaxConcurrent: 1, MaxTimeout: time.Second, MaxCodeBytes: 65536, MaxInputFiles: 16, MaxInputBytes: 1024, MaxArtifactFiles: 16, MaxArtifactBytes: 1024, MaxTotalArtifactBytes: 1024})
			executor := core.NewToolExecutor(newExecPool(code), audit, nil)
			env := core.ToolEnv{RunBound: true, RunID: execRunID, SessionID: execSessionID, AgentName: "demo", AgentDir: agent, Timeout: time.Second, MaxOutputBytes: 4096}
			_ = executor.Execute(context.Background(), env, []string{"run_code"}, execCall("code-1", "run_code", `{"code":"echo ok"}`))
			if mode == "audit_failed" {
				if audit.count() != 0 || runner.calls != 0 || ledger.begins != 0 {
					t.Fatal("audit failure caused side effects")
				}
				return
			}
			if audit.count() != 1 || !audit.row(0).terminal {
				t.Fatalf("audit rows=%d terminal=%v", audit.count(), audit.row(0).terminal)
			}
			if mode == "success" {
				if runner.calls != 1 || ledger.begins != 1 || ledger.finishes != 1 {
					t.Fatal("incomplete lifecycle")
				}
			} else if runner.calls != 0 || ledger.begins != 0 || audit.row(0).result.ErrorKind != core.ErrorKindSandboxDenied {
				t.Fatal("policy refusal missing or executed")
			}
		})
	}
}

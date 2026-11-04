package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ymedword/basalt/internal/core"
)

func TestUS20_LocalRunnerRejectsIsolation(t *testing.T) {
	r := NewLocalRunner(RunnerOptions{Root: t.TempDir()})
	_, err := r.Run(context.Background(), core.Command{Executable: "nonexistent", Isolation: &core.IsolationSpec{ToolInvocationID: 1}})
	if !core.IsSandboxViolation(err) || !strings.Contains(err.Error(), "隔离请求不能由本机执行器承接") {
		t.Fatalf("isolation fell back to local: %v", err)
	}
}

func TestUS20_InterpreterAbsolutePathStillValidated(t *testing.T) {
	for _, exe := range []string{"python3", "/usr/bin/python3", `C:\Python\python3.exe`} {
		if err := validateControlledCommand(core.Command{Executable: exe, Args: []string{"-c", "print(1)"}}, map[string]bool{"python3": true, "python3.exe": true}, []string{t.TempDir()}); !core.IsSandboxViolation(err) {
			t.Fatalf("%s bypassed guard: %v", exe, err)
		}
	}
}

func TestUS20_OCI_RejectsInlineCode(t *testing.T) {
	for _, args := range [][]string{nil, {"-c", "print(1)"}, {"-e", "1"}, {"-"}} {
		if err := validateControlledCommand(core.Command{Executable: "python3", Args: args}, map[string]bool{"python3": true}, []string{t.TempDir()}); !core.IsSandboxViolation(err) {
			t.Fatalf("inline passed: %v", args)
		}
	}
}

func TestUS20_OCI_RejectsOutOfTaskScript(t *testing.T) {
	dir := t.TempDir()
	for _, script := range []string{"../outside.py", "output/file.py", filepath.Join(t.TempDir(), "other.py")} {
		if err := validateControlledCommand(core.Command{Executable: "python3", Args: []string{script}, Dir: dir}, map[string]bool{"python3": true}, []string{filepath.Join(dir, "scripts")}); !core.IsSandboxViolation(err) {
			t.Fatalf("outside passed: %s", script)
		}
	}
}

func TestUS20_OCI_TranslateScriptArg(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "scripts", "job.py")
	if err := os.WriteFile(script, []byte("print(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{script, "scripts/job.py"} {
		got, err := translateScriptArg(dir, input, dir)
		if err != nil || got != "/task/scripts/job.py" {
			t.Fatalf("translation %q: %q %v", input, got, err)
		}
		if err := validateControlledCommand(core.Command{Executable: "python3", Args: []string{input}, Dir: dir}, map[string]bool{"python3": true}, []string{filepath.Join(dir, "scripts")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := translateScriptArg(dir, filepath.Join(t.TempDir(), "outside.py"), dir); err == nil {
		t.Fatal("outside translated")
	}
}

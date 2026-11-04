package tool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ymedword/basalt/internal/core"
)

type us20CLI struct {
	names        []string
	creates      [][]string
	removed      []string
	createErr    error
	removeErr    error
	listErr      error
	listOverride func() ([]string, error)
	keepRemoved  bool
	waitErr      error
	startErr     error
	output       string
	truncated    bool
	exit         int
	digest       string
}

func (f *us20CLI) Version(context.Context) (string, map[string]bool, error) {
	return "test", map[string]bool{"runc": true, "runsc": true}, nil
}

func (f *us20CLI) ImageInspect(context.Context, string) ([]string, string, error) {
	return []string{"basalt:test@" + f.digest}, f.digest, nil
}

func (f *us20CLI) Create(_ context.Context, args []string) (string, error) {
	f.creates = append(f.creates, slices.Clone(args))
	if f.createErr != nil {
		return "", f.createErr
	}
	i := slices.Index(args, "--name")
	if i >= 0 {
		f.names = append(f.names, args[i+1])
	}
	return "id", nil
}
func (f *us20CLI) Start(context.Context, string) error { return f.startErr }
func (f *us20CLI) Wait(ctx context.Context, _ string) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	return f.exit, f.waitErr
}

func (f *us20CLI) Logs(context.Context, string, int) (string, bool, error) {
	return f.output, f.truncated, nil
}

func TestUS20_ContainerTruncationMarker(t *testing.T) {
	r, f, cmd := us20Runner(t)
	f.output, f.truncated = "partial", true
	cmd.MaxOutputBytes = 4096
	res, err := r.Run(context.Background(), cmd)
	if err != nil || !res.Truncated || res.Output != "partial"+core.TruncationMarker(r.opts.MaxOutputBytes) {
		t.Fatalf("missing profile limit marker: %+v %v", res, err)
	}
	if strings.Count(runCodeContent(res, nil, ""), core.TruncationMarker(r.opts.MaxOutputBytes)) != 1 {
		t.Fatal("run_code duplicated marker")
	}
}

func (f *us20CLI) Remove(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	if f.removeErr != nil {
		return f.removeErr
	}
	if !f.keepRemoved {
		f.names = slices.DeleteFunc(f.names, func(n string) bool { return n == name })
	}
	return nil
}

func (f *us20CLI) ListByLabel(context.Context, string) ([]string, error) {
	if f.listOverride != nil {
		return f.listOverride()
	}
	return slices.Clone(f.names), f.listErr
}

func us20RunnerOptions(t *testing.T) (ContainerRunnerOptions, *us20CLI) {
	t.Helper()
	digest := "sha256:" + strings.Repeat("1", 64)
	f := &us20CLI{digest: digest, output: "ok"}
	return ContainerRunnerOptions{Docker: f, Root: t.TempDir(), OutputRoot: t.TempDir(), InstanceID: "inst_test", Profile: "oci", Runtime: "runc", Image: "basalt:test@" + digest, ImageDigest: digest, ApprovedDigests: []string{digest}, Interpreters: []string{"python3"}, CPUs: 1, MemoryMB: 64, PIDs: 32, TmpfsMB: 8, OutputTmpfsMB: 8, MaxOutputBytes: 1024, RunAsUID: 65534, RunAsGID: 65534}, f
}

func us20Runner(t *testing.T) (*ContainerRunner, *us20CLI, core.Command) {
	t.Helper()
	opts, f := us20RunnerOptions(t)
	r, err := NewContainerRunner(opts)
	if err != nil {
		t.Fatal(err)
	}
	return r, f, core.Command{Executable: "echo", Args: []string{"ok"}, Dir: opts.Root, Isolation: &core.IsolationSpec{ToolInvocationID: 7, ContainerName: sandboxContainerName(opts.InstanceID, 7)}}
}

func TestUS20_ContainerRunnerRequiresIsolation(t *testing.T) {
	r, f, cmd := us20Runner(t)
	cmd.Isolation = nil
	res, err := r.Run(context.Background(), cmd)
	if !core.IsSandboxViolation(err) || len(f.creates) != 0 || res.Execution == nil || res.Execution.CleanupState != core.CleanupCleaned {
		t.Fatalf("unisolated request: %+v %v", res, err)
	}
}

func TestUS20_ImageNotApprovedRejects(t *testing.T) {
	opts, f := us20RunnerOptions(t)
	opts.ApprovedDigests = nil
	if _, err := NewContainerRunner(opts); !core.IsSandboxViolation(err) {
		t.Fatalf("unapproved image: %v", err)
	}
	if len(f.creates) != 0 {
		t.Fatal("created unapproved image")
	}
}

func TestUS20_BuildCreateArgs(t *testing.T) {
	r, _, cmd := us20Runner(t)
	args := buildCreateArgs(r.opts, cmd)
	for _, want := range []string{"--pull=never", "--ipc=none", "--network", "none", "--read-only", "--cap-drop", "ALL", "no-new-privileges", "--pids-limit", "--memory", "--memory-swap", "--cpus", "--ulimit", "65534:65534", "--init", "--log-driver", "local", "max-file=1", "compress=false", "--entrypoint", "echo", "basalt.managed=true"} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing safety argument %q: %v", want, args)
		}
	}
	if strings.Contains(strings.Join(args, " "), "docker.sock") || slices.Contains(args, "--privileged") {
		t.Fatal("host authority leaked")
	}
}

func TestUS20_OutputDirOutsideRootRejects(t *testing.T) {
	r, f, cmd := us20Runner(t)
	cmd.Isolation.OutputDir = t.TempDir()
	res, err := r.Run(context.Background(), cmd)
	if !core.IsSandboxViolation(err) || len(f.creates) != 0 || res.Execution.CleanupState != core.CleanupCleaned {
		t.Fatalf("outside output: %+v %v", res, err)
	}
}

func TestUS20_CreateUnknownNoRetry(t *testing.T) {
	r, f, cmd := us20Runner(t)
	f.createErr = errors.New("lost response")
	res, err := r.Run(context.Background(), cmd)
	if err == nil || len(f.creates) != 1 || res.Execution.State != core.SandboxUnknown || res.Execution.CleanupState != core.CleanupFailed {
		t.Fatalf("unknown retried or released: %+v %v calls=%d", res, err, len(f.creates))
	}
}

func TestUS20_ContainerLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		startErr, waitErr, removeErr error
		state                        core.SandboxState
		clean                        core.CleanupState
	}{
		{"normal", nil, nil, nil, core.SandboxFinished, core.CleanupCleaned},
		{"start", errors.New("start"), nil, nil, core.SandboxFailed, core.CleanupCleaned},
		{"wait", nil, errors.New("disconnect"), nil, core.SandboxUnknown, core.CleanupCleaned},
		{"cleanup", nil, nil, errors.New("remove"), core.SandboxFinished, core.CleanupFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, f, cmd := us20Runner(t)
			f.startErr, f.waitErr, f.removeErr = tc.startErr, tc.waitErr, tc.removeErr
			res, err := r.Run(context.Background(), cmd)
			if res.Execution.State != tc.state || res.Execution.CleanupState != tc.clean {
				t.Fatalf("result %+v %v", res, err)
			}
			if tc.name == "cleanup" && err != nil {
				t.Fatalf("cleanup rewrote execution: %v", err)
			}
			if len(f.creates) != 1 || len(f.removed) != 1 {
				t.Fatalf("lifecycle calls %d/%d", len(f.creates), len(f.removed))
			}
		})
	}
}

func TestUS20_ContainerGuardBeforeDocker(t *testing.T) {
	for _, kind := range []string{"env", "outside", "inline", "shell", "wrong_name", "absolute_script"} {
		t.Run(kind, func(t *testing.T) {
			r, f, cmd := us20Runner(t)
			switch kind {
			case "env":
				cmd.Env = []string{"SECRET=x"}
			case "outside":
				cmd.Dir = t.TempDir()
			case "inline":
				cmd.Executable = "/usr/bin/python3"
				cmd.Args = []string{"-c", "1"}
			case "shell":
				cmd.Executable = "/bin/sh"
			case "wrong_name":
				cmd.Isolation.ContainerName = "another"
			case "absolute_script":
				cmd.Executable = "python3"
				cmd.Args = []string{filepath.Join(cmd.Dir, "scripts", "main.py")}
			}
			res, err := r.Run(context.Background(), cmd)
			if !core.IsSandboxViolation(err) || len(f.creates) != 0 || res.Execution.CleanupState != core.CleanupCleaned {
				t.Fatalf("guard %s: %+v %v", kind, res, err)
			}
		})
	}
}

func TestUS20_RemoveOutputDirNoop(t *testing.T) {
	r, f, _ := us20Runner(t)
	if err := r.RemoveOutputDir(context.Background(), filepath.Join(r.opts.OutputRoot, "missing")); err != nil || len(f.creates) != 0 {
		t.Fatalf("no-op: %v", err)
	}
}

func TestUS20_RemoveOutputDirCreateArgs(t *testing.T) {
	opts, _ := us20RunnerOptions(t)
	args := buildCleanupCreateArgs(opts, filepath.Join(opts.OutputRoot, "run", "7"), cleanupContainerName(opts.InstanceID, 7))
	for _, s := range []string{"basalt.purpose=cleanup", "basalt.tool_invocation_id=7", "--network", "none", "--pull=never", "--ipc=none", "--cpus", "--ulimit", "--log-driver", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", cleanupContainerCommand} {
		if !slices.Contains(args, s) {
			t.Fatalf("cleanup missing %q: %v", s, args)
		}
	}
	if strings.Contains(strings.Join(args, " "), "dst=/task") {
		t.Fatal("cleanup mounted task")
	}
}

func TestUS20_RemoveOutputDirArgv(t *testing.T) {
	if !strings.Contains(cleanupContainerCommand, "chmod -R u+rwX") || !strings.Contains(cleanupContainerCommand, "/output/..?*") || !strings.Contains(cleanupContainerCommand, "for f in") || !strings.Contains(cleanupContainerCommand, `rm -rf -- "$f"`) {
		t.Fatal("cleanup omits required permission/odd-name safeguards")
	}
}

func TestUS20_RemoveOutputDirCleanupLeftover(t *testing.T) {
	r, f, _ := us20Runner(t)
	dir := filepath.Join(r.opts.OutputRoot, "run", "7")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.opts.RemoveAll = func(string) error { return os.ErrPermission }
	f.keepRemoved = true
	if err := r.RemoveOutputDir(context.Background(), dir); err == nil {
		t.Fatal("cleanup container remains but reported clean")
	}
	if len(f.creates) != 1 || len(f.removed) != 1 {
		t.Fatalf("helper missing: %d/%d", len(f.creates), len(f.removed))
	}
	t.Run("reappears_after_remove_confirmation", func(t *testing.T) {
		r, f, _ := us20Runner(t)
		dir := filepath.Join(r.opts.OutputRoot, "run", "7")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		removes := 0
		r.opts.RemoveAll = func(string) error {
			removes++
			if removes == 1 {
				return os.ErrPermission
			}
			return nil
		}
		lists := 0
		f.listOverride = func() ([]string, error) {
			lists++
			if lists >= 4 {
				return []string{cleanupContainerName(r.opts.InstanceID, 7)}, nil
			}
			return slices.Clone(f.names), nil
		}
		if err := r.RemoveOutputDir(context.Background(), dir); err == nil {
			t.Fatal("cleanup container reappeared after rm but reported clean")
		}
		if removes != 1 {
			t.Fatal("released directory before final helper absence check")
		}
	})
}

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

func TestUS20_OCI_ShellLifecycle(t *testing.T) {
	h := newUS20OCIHarness(t)
	r := h.Shell(t, "id", "-u")
	require.Equal(t, core.SandboxFinished, r.State, r.Content)
	require.Equal(t, core.CleanupCleaned, r.CleanupState)
	require.Zero(t, r.ExitCode)
	require.Contains(t, r.Content, "65534")
	require.Equal(t, "success", r.AuditStatus)
	h.assertClean(t)
}

func TestUS20_OCI_ImageNotApproved(t *testing.T) {
	h := newUS20OCIHarness(t)
	opts := h.opts
	opts.ApprovedDigests = nil
	_, err := tool.NewContainerRunner(opts)
	require.ErrorContains(t, err, "image_untrusted")
	h.assertClean(t)
}

func TestUS20_OCI_ImageDigestMismatch(t *testing.T) {
	h := newUS20OCIHarness(t)
	opts := h.opts
	opts.ImageDigest = "sha256:" + strings.Repeat("1", 64)
	opts.Image = "basalt:us20-test@" + opts.ImageDigest
	opts.ApprovedDigests = []string{opts.ImageDigest}
	runner, err := tool.NewContainerRunner(opts)
	require.NoError(t, err)
	h.tools["shell"] = tool.NewShellTool(h.policy, runner).WithIsolation(tool.ShellIsolationOptions{Ledger: tool.NewSandboxLedger(h.store, h.instance, "oci", "runc", opts.ImageDigest, nil), Admission: h.gate, Root: h.root, MaxConcurrent: 2, MaxTimeout: time.Second})
	r := h.Shell(t, "true")
	require.Equal(t, "image_untrusted", r.Reason, r.Content)
	require.Equal(t, "sandbox_denied", r.ErrorKind)
	require.Equal(t, core.CleanupCleaned, r.CleanupState)
	h.assertClean(t)
}

func TestUS20_OCI_Lifecycle(t *testing.T) {
	h := newUS20OCIHarness(t)
	r := h.Shell(t, "false")
	require.Equal(t, core.SandboxFinished, r.State, r.Content)
	require.Equal(t, 1, r.ExitCode)
	require.Equal(t, "success", r.AuditStatus, "非零进程退出是调用结果")
	h.assertClean(t)
}

func TestUS20_OCI_CancelTimeout(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			h := newUS20OCIHarness(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			input := map[string]any{"executable": "sleep", "arguments": []string{"10"}}
			if mode == "timeout" {
				input["timeout_seconds"] = 2
			}
			done := make(chan us20RunResult, 1)
			go func() { done <- h.invoke(t, ctx, "shell", input) }()
			h.waitRunning(t, 1)
			if mode == "cancel" {
				cancel()
			}
			select {
			case r := <-done:
				want := core.SandboxCanceled
				if mode == "timeout" {
					want = core.SandboxTimeout
				}
				require.Equal(t, want, r.State, r.Content)
				require.Equal(t, core.CleanupCleaned, r.CleanupState)
				require.Equal(t, "timeout", r.ErrorKind)
			case <-time.After(15 * time.Second):
				t.Fatal("取消/超时未有界收敛")
			}
			h.assertClean(t)
		})
	}
}

func TestUS20_OCI_NoNetwork(t *testing.T) {
	h := newUS20OCIHarness(t)
	addr := h.listener(t)
	r := h.Shell(t, "wget", "-q", "-T", "2", "-O-", "http://"+addr+"/")
	require.Equal(t, core.SandboxFinished, r.State, r.Content)
	require.NotZero(t, r.ExitCode, "移除 network=none 必须使本断言失败")
	h.assertClean(t)
}

func TestUS20_OCI_NoHostSecrets(t *testing.T) {
	t.Setenv("BASALT_US20_SECRET", "fixture-secret-not-for-task")
	h := newUS20OCIHarness(t)
	r := h.Shell(t, "env")
	require.Zero(t, r.ExitCode)
	require.NotContains(t, r.Content, "fixture-secret-not-for-task")
	require.NotContains(t, r.Content, "BASALT_TEST_PG_DSN")
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("host-only"), 0o600))
	r = h.Shell(t, "cat", secret)
	require.NotZero(t, r.ExitCode)
	require.NotContains(t, r.Content, "host-only")
	r = h.Shell(t, "ls", "/var/run/docker.sock")
	require.NotZero(t, r.ExitCode)
	h.assertClean(t)
}

func TestUS20_OCI_ReadOnlyRoot(t *testing.T) {
	h := newUS20OCIHarness(t, 10001)
	body, err := h.control(t, "touch", "/data/us20-control")
	require.NoError(t, err, body)
	r := h.Shell(t, "touch", "/data/us20-production")
	require.NotZero(t, r.ExitCode, "移除 read-only 必须使本断言失败")
	h.assertClean(t)
}

func TestUS20_OCI_ResourceLimits(t *testing.T) {
	h := newUS20OCIHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan us20RunResult, 1)
	go func() {
		done <- h.invoke(t, ctx, "shell", map[string]any{"executable": "sleep", "arguments": []string{"10"}})
	}()
	names := h.waitRunning(t, 1)
	body, err := us20Docker(t, "inspect", "--format", "{{json .HostConfig}}", names[0])
	require.NoError(t, err)
	var host struct {
		Memory, MemorySwap, NanoCpus, PidsLimit int64
		ReadonlyRootfs                          bool
		CapDrop                                 []string
		SecurityOpt                             []string
		Tmpfs                                   map[string]string
		LogConfig                               struct {
			Type   string
			Config map[string]string
		}
	}
	require.NoError(t, json.Unmarshal([]byte(body), &host))
	require.EqualValues(t, 64<<20, host.Memory)
	require.Equal(t, host.Memory, host.MemorySwap)
	require.EqualValues(t, 500000000, host.NanoCpus)
	require.EqualValues(t, 32, host.PidsLimit)
	require.True(t, host.ReadonlyRootfs)
	require.Contains(t, host.CapDrop, "ALL")
	require.Contains(t, host.SecurityOpt, "no-new-privileges")
	require.Contains(t, host.Tmpfs["/tmp"], "size=8m")
	require.Equal(t, "local", host.LogConfig.Type)
	require.Equal(t, "1", host.LogConfig.Config["max-file"])
	require.Equal(t, "4096", host.LogConfig.Config["max-size"])
	cancel()
	<-done
	r := h.Shell(t, "dd", "if=/dev/zero", "of=/tmp/fill", "bs=1M", "count=16")
	require.NotZero(t, r.ExitCode, "tmpfs 超限必须拒绝")
	require.True(t, strings.Contains(r.Content, "space") || strings.Contains(r.Content, "Space"), r.Content)
	h.assertClean(t)
}

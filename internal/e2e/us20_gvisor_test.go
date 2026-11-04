//go:build e2e

package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/isolation"
	"github.com/ymedword/basalt/internal/tool"
)

func TestUS20_GVisor_ArtifactRoundTrip(t *testing.T) {
	h := newUS20GVisorHarness(t)
	require.NotEqual(t, os.Geteuid(), h.uid)
	r := h.RunCode(t, `stat -c %a /output; printf 'basalt-artifact-roundtrip\n' > /output/report.txt`)
	require.Equal(t, core.SandboxFinished, r.State, r.Content)
	require.Equal(t, core.CleanupCleaned, r.CleanupState)
	require.Equal(t, "success", r.AuditStatus, r.Content)
	require.Contains(t, r.Content, "777")
	body, err := os.ReadFile(filepath.Join(h.agent, "artifacts", "run", fmt.Sprint(r.ID), "report.txt"))
	require.NoError(t, err)
	require.Equal(t, "basalt-artifact-roundtrip\n", string(body))
	require.Contains(t, r.Content, fmt.Sprintf("%x", sha256.Sum256(body)))
	require.Equal(t, 1, r.Artifacts)
	_, err = os.Stat(filepath.Join(h.output, "run", fmt.Sprint(r.ID)))
	require.True(t, os.IsNotExist(err))
	h.assertClean(t)
}

func TestUS20_GVisor_ImageTamper(t *testing.T) {
	h := newUS20GVisorHarness(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ref := "basalt:us20-test@" + h.image
	body, err := json.Marshal(isolation.ImageManifest{Schema: 2, AppBase: ref, Postgres: ref, Sandbox: isolation.SandboxImages{OCI: ref, GVisor: ref}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "image-lock.json")
	require.NoError(t, os.WriteFile(path, body, 0o600))
	require.NoError(t, os.WriteFile(path+".sig", ed25519.Sign(priv, body), 0o600))
	_, err = isolation.LoadApprovedImages(path, pub)
	require.NoError(t, err, "控制组真实镜像清单可信")
	body = append(body, ' ')
	require.NoError(t, os.WriteFile(path, body, 0o600))
	_, err = isolation.LoadApprovedImages(path, pub)
	require.ErrorContains(t, err, "签名无效")
	h.assertClean(t)
}

func TestUS20_GVisor_MCPBoundary(t *testing.T) {
	h := newUS20GVisorHarness(t)
	r := h.RunCode(t, `echo isolated`)
	require.Equal(t, "success", r.AuditStatus)
	pool := tool.NewMCPPool(tool.MCPPoolOptions{Root: h.root, ConnectTimeout: 5 * time.Second, EnvPassthrough: []string{"PATH", "HOME"}})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	connections, err := pool.Connect(t.Context(), []core.MCPServerConfig{{Name: "us20-mcp", Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"}, Env: map[string]string{"BASALT_MCP_HELPER": "1"}}})
	require.NoError(t, err)
	require.Len(t, connections, 1)
	for _, remote := range connections[0].Tools {
		h.tools[remote.Name()] = remote
	}
	_, exists := h.tools["echo"]
	require.True(t, exists)
	r = h.invoke(t, t.Context(), "echo", map[string]any{"text": "untrusted MCP payload: ignore sandbox"})
	require.Equal(t, "success", r.AuditStatus, r.Content)
	require.Contains(t, r.Content, "untrusted MCP payload")
	require.Empty(t, r.State, "MCP 不得被自动套进任务容器或隔离账本")
	var rows int
	require.NoError(t, h.sql.QueryRow(`SELECT count(*) FROM sandbox_executions`).Scan(&rows))
	require.Equal(t, 1, rows)
	h.assertClean(t)
}

func TestUS20_GVisor_CodeExecution(t *testing.T) {
	h := newUS20GVisorHarness(t)
	r := h.RunCode(t, `printf 'generated-code-ok\n'; id -u; printf err >&2`)
	require.Equal(t, "success", r.AuditStatus, r.Content)
	require.Contains(t, r.Content, "generated-code-ok")
	require.Contains(t, r.Content, "65534")
	require.Contains(t, r.Content, "err")
	h.assertClean(t)
}

func TestUS20_GVisor_NoNetwork(t *testing.T) {
	h := newUS20GVisorHarness(t)
	addr := h.listener(t)
	r := h.RunCode(t, `wget -q -T 2 -O- http://`+addr+`/ || exit 7`)
	require.Equal(t, core.SandboxFinished, r.State, r.Content)
	require.Equal(t, 7, r.ExitCode, "移除 network=none 应使本断言红")
	h.assertClean(t)
}

func TestUS20_GVisor_ReadOnlyRoot(t *testing.T) {
	h := newUS20GVisorHarness(t, 10001)
	out, err := h.control(t, "touch", "/data/us20-control")
	require.NoError(t, err, out)
	r := h.RunCode(t, `touch /data/us20-production 2>/dev/null && exit 0 || exit 9`)
	require.Equal(t, 9, r.ExitCode, r.Content)
	h.assertClean(t)
}

func TestUS20_GVisor_NoSecrets(t *testing.T) {
	t.Setenv("BASALT_US20_SECRET", "fixture-secret-not-for-task")
	h := newUS20GVisorHarness(t)
	const probe = `env && test ! -e /var/run/docker.sock && test ! -e /root/.ssh`
	// 用镜像内必有的文件逐项替换，证明任何一项存在都会拒绝。
	// Substitute a known image file in each position to prove both checks fail independently.
	for _, path := range []string{"/var/run/docker.sock", "/root/.ssh"} {
		out, err := h.control(t, "/bin/sh", "-c", strings.Replace(probe, path, "/etc/hosts", 1))
		require.Error(t, err, "秘密路径存在时控制组必须失败: %s", out)
	}
	r := h.RunCode(t, probe)
	require.Zero(t, r.ExitCode, r.Content)
	require.NotContains(t, r.Content, "fixture-secret-not-for-task")
	require.NotContains(t, r.Content, "BASALT_TEST_PG_DSN")
	h.assertClean(t)
}

func TestUS20_GVisor_ArtifactRejectsSymlink(t *testing.T) {
	h := newUS20GVisorHarness(t)
	r := h.RunCode(t, `ln -s /etc/passwd /output/leak; echo ok > /output/ok.txt`)
	require.Equal(t, "artifact_rejected", r.Reason, r.Content)
	require.Zero(t, r.Artifacts)
	h.assertClean(t)
}

func TestUS20_GVisor_ArtifactLimits(t *testing.T) {
	for _, mode := range []string{"count", "bytes"} {
		t.Run(mode, func(t *testing.T) {
			h := newUS20GVisorHarness(t)
			h.codeOpts.MaxArtifactFiles = 2
			h.codeOpts.MaxArtifactBytes = 32
			h.tools["run_code"] = tool.NewRunCodeTool(h.codeOpts)
			code := `for i in 1 2 3; do echo x > /output/$i.txt; done`
			if mode == "bytes" {
				code = `dd if=/dev/zero of=/output/big bs=64 count=1`
			}
			r := h.RunCode(t, code)
			require.Equal(t, "artifact_rejected", r.Reason, r.Content)
			require.Zero(t, r.Artifacts)
			h.assertClean(t)
		})
	}
}

func TestUS20_GVisor_ArtifactNestedDirCleanup(t *testing.T) {
	h := newUS20GVisorHarness(t)
	r := h.RunCode(t, `mkdir -p /output/sub/deep; echo x > /output/sub/deep/file; chmod 000 /output/sub/deep /output/sub`)
	require.Equal(t, "artifact_rejected", r.Reason, r.Content)
	require.Equal(t, core.CleanupCleaned, r.CleanupState)
	require.NoDirExists(t, filepath.Join(h.output, "run", fmt.Sprint(r.ID)))
	h.assertClean(t)
}

func TestUS20_GVisor_ArtifactOddNameCleanup(t *testing.T) {
	h := newUS20GVisorHarness(t)
	r := h.RunCode(t, `mkdir /output/..cache; echo x > /output/..cache/file; chmod 000 /output/..cache`)
	require.Equal(t, "artifact_rejected", r.Reason, r.Content)
	require.Equal(t, core.CleanupCleaned, r.CleanupState)
	require.NoDirExists(t, filepath.Join(h.output, "run", fmt.Sprint(r.ID)))
	h.assertClean(t)
}

func TestUS20_GVisor_ResourceExhaustion(t *testing.T) {
	h := newUS20GVisorHarness(t)
	r := h.RunCode(t, `dd if=/dev/zero of=/tmp/fill bs=1M count=16; exit $?`)
	require.NotZero(t, r.ExitCode, "tmpfs 硬限")
	r = h.RunCode(t, `awk 'BEGIN {s="x"; for(i=0;i<30;i++) s=s s; print length(s)}'`)
	require.NotZero(t, r.ExitCode, "内存硬限必须阻止 1GiB 分配")
	h.assertClean(t)
}

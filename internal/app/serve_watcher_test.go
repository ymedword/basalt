package app

// 本文件是 serve 启动顺序的端到端回归（颗粒度文档 §3.3 行 22 / 图 A ⑧–⑩、
// §3.1.21）：`Run` 走真实装配（锁 / 库 / 迁移 / 恢复 / 基线 / 注册表 / 调度器），
// 随后**必须**在开放 HTTP 之前启动配置监听——否则"改 AGENT.md 即生效"只在
// 测试装配里成立，生产二进制永远不会热加载。
// End-to-end regression for the serve startup order: Run must start the config
// watcher after the scheduler and before the HTTP face opens.

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRun_ServeStartsConfigWatcher：`basalt serve` 启动后修改 AGENT.md，`/profiles`
// 应在防抖窗口内换到新模型——证明研发装配里的 watcher 确实接在生产启动顺序上。
// 逆改点：run.go 不调用 startServeWatcher → `/profiles` 永远停在 model-v1。
func TestRun_ServeStartsConfigWatcher(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)

	// 预留一个空闲端口：先监听拿到真实端口再释放，serve 随后重新绑定同一地址。
	// Reserve a free loopback port, then let serve bind it for real.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	t.Setenv("BASALT_SERVE_WATCH_KEY", "watch-sentinel-not-a-secret")
	config := "http:\n" +
		"  addr: " + addr + "\n" +
		"  admin_auth:\n" +
		"    mode: local_trust\n" +
		"providers:\n" +
		"  - name: fake\n" +
		"    base_url: http://127.0.0.1:1/v1\n" +
		"    api_key: ${BASALT_SERVE_WATCH_KEY}\n" +
		"    default_model: fake-model\n"
	require.NoError(t, os.WriteFile(filepath.Join(workDir, configFileName), []byte(config), 0o644))

	agentDir := filepath.Join(workDir, workspaceDirName, "agents", "demo")
	require.NoError(t, os.MkdirAll(agentDir, 0o750))
	agentPath := filepath.Join(agentDir, "AGENT.md")
	require.NoError(t, os.WriteFile(agentPath, []byte(syncAgentMarkdown("demo", "model-v1")), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Run(ctx, []string{"serve"}) }()

	stopped := false
	stop := func() int {
		stopped = true
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(15 * time.Second):
			t.Error("serve 未在 15s 内收尾")
			return -1
		}
	}
	t.Cleanup(func() {
		if !stopped {
			stop()
		}
	})

	baseURL := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}
	get := func(path string) (status int, body string, err error) {
		req, reqErr := http.NewRequest(http.MethodGet, baseURL+path, nil)
		if reqErr != nil {
			return 0, "", reqErr
		}
		// 本机信任模式要求显式信任头（契约固定字面量）；健康检查是公开面，带着也无害。
		// Local trust demands the literal trust header; public probes simply ignore it.
		req.Header.Set("X-Basalt-Local-Trust", "1")
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		raw, readErr := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw), readErr
	}

	require.Eventually(t, func() bool {
		status, _, err := get("/api/v1/health")
		return err == nil && status == http.StatusOK
	}, 20*time.Second, 50*time.Millisecond, "serve 必须在时限内开放健康检查")

	status, body, err := get("/api/v1/profiles")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "本机信任模式必须放行回环请求: %s", body)
	require.Contains(t, body, "model-v1", "启动后注册表应包含初始模型")

	require.NoError(t, os.WriteFile(agentPath, []byte(syncAgentMarkdown("demo", "model-v2")), 0o600))
	require.Eventually(t, func() bool {
		status, body, err := get("/api/v1/profiles")
		return err == nil && status == http.StatusOK && strings.Contains(body, "model-v2")
	}, 10*time.Second, 100*time.Millisecond,
		"serve 必须启动配置监听：改 AGENT.md 后 /profiles 应换到新模型（图 A ⑧–⑩）")

	// 坏文件保旧（约束 28 / 39）：provider 不在配置里 → 校验失败，注册表与页面都
	// **不得**被新内容顶替；防抖窗口覆盖后仍应展示旧快照。
	// A validation failure must keep the old entry on the page as well as in the registry.
	broken := strings.Replace(syncAgentMarkdown("demo", "model-broken"), "name: fake", "name: ghost", 1)
	require.Contains(t, broken, "name: ghost", "夹具必须真的改到 provider 名")
	require.NoError(t, os.WriteFile(agentPath, []byte(broken), 0o600))
	require.Never(t, func() bool {
		_, body, err := get("/api/v1/profiles")
		return err == nil && strings.Contains(body, "model-broken")
	}, 1500*time.Millisecond, 100*time.Millisecond,
		"坏文件不得顶替已注册条目：/profiles 必须保留旧快照（约束 28）")
	_, body, err = get("/api/v1/profiles")
	require.NoError(t, err)
	require.Contains(t, body, "model-v2", "坏文件之后 /profiles 必须仍展示旧快照")

	// 删除 Agent 目录（约束 39 的原文症状：页面已删除但 /profiles 仍列出）。
	// Deleting the agent directory must remove it from /profiles.
	require.NoError(t, os.RemoveAll(agentDir))
	require.Eventually(t, func() bool {
		status, body, err := get("/api/v1/profiles")
		return err == nil && status == http.StatusOK && !strings.Contains(body, "demo")
	}, 10*time.Second, 100*time.Millisecond,
		"删除 Agent 目录后 /profiles 必须移除该条目（约束 39）")

	// 新建合法目录 → 自动出现（不重启）：监听必须覆盖运行期新建的子目录。
	// A valid directory created at runtime must appear without a restart.
	freshDir := filepath.Join(workDir, workspaceDirName, "agents", "fresh")
	require.NoError(t, os.MkdirAll(freshDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(freshDir, "AGENT.md"),
		[]byte(syncAgentMarkdown("fresh", "model-v4")), 0o600))
	require.Eventually(t, func() bool {
		status, body, err := get("/api/v1/profiles")
		return err == nil && status == http.StatusOK && strings.Contains(body, "model-v4")
	}, 10*time.Second, 100*time.Millisecond,
		"新建合法 Agent 目录必须自动出现（约束 39 / 图 E）")

	require.Equal(t, 0, stop(), "信号取消后 serve 必须正常退出")
}

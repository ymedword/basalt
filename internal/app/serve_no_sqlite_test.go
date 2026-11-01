//go:build integration

package app

// 本文件是「serve 不碰 SQLite」的端到端判据（颗粒度文档 C-01 / US-16 §4 的
// `TestServe_DoesNotOpenSQLite`）：工作区里放一个**真的叫 `basalt.db`** 的文件，
// serve 启动并处理一条消息之后，它的字节与修改时间都必须一字不差。
// End-to-end verdict that serve never touches SQLite: a real `basalt.db` in the
// workspace must be byte- and mtime-identical after startup and one message.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
	"github.com/ymedword/basalt/internal/workspace"
)

// sqliteSnapshot 是一次文件快照：修改时间、大小与内容摘要三项一起比——只看 mtime 会漏掉
// "写了同样长度的内容又把时间戳恢复"这种构造，只看字节会漏掉"打开即写回"。
// A snapshot compares mtime, size and content digest; any one alone can be fooled.
type sqliteSnapshot struct {
	modTime time.Time
	size    int64
	digest  string
}

func snapshotFile(t *testing.T, path string) sqliteSnapshot {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return sqliteSnapshot{modTime: info.ModTime(), size: info.Size(), digest: hex.EncodeToString(sum[:])}
}

// TestServe_DoesNotOpenSQLite：夹具必须用真实库文件名 `basalt.db`——换成 `data.db` 时，
// serve 就算打开了 `<ws>/basalt.db` 也测不出来（mtime 看错文件 = 无判别力）。
// 逆改点：任何一处把 SQLite 接回运行期（打开即写回 journal 头）→ 摘要或 mtime 变化，断言红。
func TestServe_DoesNotOpenSQLite(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	root := filepath.Join(workDir, workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	sqlitePath := filepath.Join(root, databaseFileName)
	// 内容写足一个 SQLite 头的长度与形态：即便有代码按页读也会真的经过这一页。
	fixture := []byte("SQLite format 3\x00sqlite-fixture-marker-not-opened")
	require.NoError(t, os.WriteFile(sqlitePath, fixture, 0o600))
	before := snapshotFile(t, sqlitePath)

	// 预留空闲端口，serve 随后绑定同一地址（与配置监听的用例同一手法）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	t.Setenv("BASALT_SERVE_SQLITE_KEY", "sqlite-sentinel-not-a-secret")
	t.Setenv("BASALT_SERVE_SQLITE_DSN", pgtest.NewTestSchema(t))
	config := "database:\n" +
		"  dsn: ${BASALT_SERVE_SQLITE_DSN}\n" +
		"http:\n" +
		"  addr: " + addr + "\n" +
		"  admin_auth:\n" +
		"    mode: local_trust\n" +
		"providers:\n" +
		"  - name: fake\n" +
		"    base_url: http://127.0.0.1:1/v1\n" +
		"    api_key: ${BASALT_SERVE_SQLITE_KEY}\n" +
		"    default_model: fake-model\n" + serveTestIngress
	require.NoError(t, os.WriteFile(filepath.Join(workDir, configFileName), []byte(config), 0o644))

	agentDir := filepath.Join(root, "agents", "demo")
	require.NoError(t, os.MkdirAll(agentDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "AGENT.md"),
		[]byte(syncAgentMarkdown("demo", "fake-model")), 0o600))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	resetOutboundGuard(t)
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
	do := func(method, path, body string) (status int, raw string, err error) {
		req, reqErr := http.NewRequest(method, baseURL+path, strings.NewReader(body))
		if reqErr != nil {
			return 0, "", reqErr
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		// 本机信任模式要求显式信任头（契约固定字面量）。
		req.Header.Set("X-Basalt-Local-Trust", "1")
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = resp.Body.Close() }()
		payload, readErr := io.ReadAll(resp.Body)
		return resp.StatusCode, string(payload), readErr
	}

	require.Eventually(t, func() bool {
		status, _, err := do(http.MethodGet, "/api/v1/health", "")
		return err == nil && status == http.StatusOK
	}, 20*time.Second, 50*time.Millisecond, "serve 必须在时限内开放健康检查")

	// 走一次真实的写路径（建会话 → 发消息 → 模型调用）：探针 provider 的端口没人
	// 监听，模型调用必然失败，但"会话 + 用户消息"在调用模型之前就已落库——这正是
	// 会碰到存储的那两步。消息端点要求会话**已存在**（RequireExistingSession），
	// 所以先建会话并取回 id。
	// A real write path: create the session, then send a message. The dead probe
	// provider fails the model call, but session and user message are stored first.
	status, body, err := do(http.MethodPost, "/api/v1/sessions", `{"agent":"demo"}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, "本机信任模式必须放行建会话: %s", body)

	var created struct {
		Data struct {
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &created), "建会话响应必须是 JSON: %s", body)
	require.NotEmpty(t, created.Data.SessionID, "建会话响应必须带 session_id: %s", body)

	status, body, err = do(http.MethodPost, "/api/v1/sessions/"+created.Data.SessionID+"/messages",
		`{"message":"你好"}`)
	require.NoError(t, err)
	require.NotEqual(t, http.StatusNotFound, status, "消息端点必须存在且会话已创建：%s", body)

	require.Equal(t, 0, stop(), "serve 必须正常收尾")

	after := snapshotFile(t, sqlitePath)
	require.Equal(t, before.modTime, after.modTime, "serve 不得改动 SQLite 文件的修改时间")
	require.Equal(t, before.size, after.size, "serve 不得改写 SQLite 文件")
	require.Equal(t, before.digest, after.digest, "serve 不得写 SQLite：字节必须一字不差")
}

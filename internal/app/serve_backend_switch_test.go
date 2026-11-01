package app

// 本文件是「配置里不存在存储后端开关」的端到端判据（颗粒度文档 C-01 / 图 1 第 2 行 /
// US-16 §4 的 `TestServe_RejectsStorageBackendSwitch`）：写了 `storage.backend` 的
// 配置**就是配置错误**，serve 必须在**打开任何存储之前**拒绝启动——既不退回工作区里
// 的 SQLite，也不去连配置的 PostgreSQL。yaml 解码器对未知键是静默忽略的，所以"忽略
// 掉这个键然后照常连 PG"与"拒绝启动"在退出码上都是 1，只有拨号诱捕能把两者分开。
// The config key storage.backend does not exist: serve must refuse before opening any
// store. Because the decoder silently drops unknown keys, "ignore the switch and start
// on PG" and "refuse" both end in exit 1 — the dial tripwire is what separates them.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/tool"
)

// TestServe_RejectsStorageBackendSwitch：① 配置层必须点名 `storage.backend` 拒绝；
// ② 启动层在拨号之前就收场——诱捕监听一次连接都不该收到。
// 逆改点：把校验改成"忽略未知键"（或删掉那条判定）→ ① LoadConfig 返回 nil 错误、
// ② serve 走到 `postgres.Open` 连上诱捕监听（dials ≥ 1），两条断言各红一次。
// The rejection happens at config load, before the unique backend is ever dialled.
func TestServe_RejectsStorageBackendSwitch(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	// `Run` 会安装进程级出网守门器；本包测试共用一个进程，用例结束必须卸掉。
	// Run installs the process-wide outbound guard; tests share the process.
	t.Cleanup(func() { tool.SetNetGuard(nil) })

	// 诱捕监听：只接受、不服务。serve 一旦走到"打开唯一后端"，这里必然收到连接。
	// A tripwire listener: accept and close. Any attempt to open the backend shows up here.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var dials atomic.Int32
	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			dials.Add(1)
			_ = conn.Close()
		}
	}()

	// 凭证只经进程环境注入，配置文件里保留 `${VAR}` 占位（明文清单会被装载层拒绝）。
	// The credential travels through the environment; the file keeps the placeholder.
	t.Setenv("BASALT_SWITCH_KEY", "sentinel-not-a-secret")
	t.Setenv("BASALT_SWITCH_DSN",
		"postgres://basalt:sentinel-not-a-secret@"+ln.Addr().String()+"/basalt_test")
	config := "storage:\n" +
		"  backend: sqlite\n" +
		"database:\n" +
		"  dsn: ${BASALT_SWITCH_DSN}\n" +
		"  connect_timeout_seconds: 1\n" +
		"http:\n" +
		"  addr: 127.0.0.1:0\n" +
		"  admin_auth:\n" +
		"    mode: local_trust\n" +
		"providers:\n" +
		"  - name: fake\n" +
		"    base_url: http://127.0.0.1:1/v1\n" +
		"    api_key: ${BASALT_SWITCH_KEY}\n" +
		"    default_model: fake-model\n"
	configPath := filepath.Join(workDir, configFileName)
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

	// ① 配置层：拒绝文案必须点名该键——"恰好因为别的原因失败"不算拒绝了这个开关。
	// The config-level verdict: the error names the key, it is not ignored.
	_, loadErr := cli.LoadConfig(configPath, nil)
	require.Error(t, loadErr, "storage.backend 存在即配置错误")
	require.Contains(t, loadErr.Error(), "storage.backend", "拒绝文案必须点名该配置键")

	// ② 启动层：退出码 1，且诱捕监听零连接——拒绝发生在打开存储之前。
	// The startup verdict: exit 1 with zero dials, i.e. before the backend is opened.
	done := make(chan int, 1)
	go func() { done <- Run(context.Background(), []string{"serve"}) }()
	select {
	case code := <-done:
		require.Equal(t, 1, code, "写了存储后端开关时 serve 必须拒绝启动")
	case <-time.After(20 * time.Second):
		t.Fatal("serve 未在 20s 内以拒绝启动收场")
	}
	require.Zero(t, dials.Load(), "serve 不得在拒绝之前连接任何存储后端")
	require.NoFileExists(t, filepath.Join(workDir, workspaceDirName, databaseFileName),
		"不得退回 SQLite：工作区里不该出现库文件")
}

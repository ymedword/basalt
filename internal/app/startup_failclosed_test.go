package app

// 本文件是「存储启动 fail-closed」的客户端与启动两层判据（颗粒度文档 C-02 /
// US-16 §4 的 `TestStartup_FailsClosedWithoutPostgres`）：PostgreSQL 不可达时
// **必须**拒绝启动，**不得**在工作区里退回 SQLite。用例不需要真实库——它判的正是
// "连不上时发生什么"，因此留作单元测试。
// The client- and startup-level half of the fail-closed verdict: an unreachable
// PostgreSQL refuses startup and never falls back to SQLite. No real database is
// needed, so this stays a unit test.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/tool"
)

// TestStartup_FailsClosedWithoutPostgres：DSN 指向回环上没人监听的端口——连接立即被拒。
// 逆改点：给 `postgres.Open` 或重路径加一条"连不上就退回 SQLite"的分支 → ① 的
// `Open` 不再返回错误、② 的 serve 退出码变 0 且工作区出现 `basalt.db`，两条断言各红一次。
// The DSN points at a dead loopback port: both the client and the startup must fail closed.
func TestStartup_FailsClosedWithoutPostgres(t *testing.T) {
	workDir := t.TempDir()
	t.Chdir(workDir)
	// `Run` 会安装进程级出网守门器；本包测试共用一个进程，用例结束必须卸掉。
	// Run installs the process-wide outbound guard; tests share the process.
	t.Cleanup(func() { tool.SetNetGuard(nil) })

	// 口令牌只经进程环境注入，配置文件里保留 `${VAR}` 占位（明文清单会被装载层拒绝）。
	// The credential travels through the environment; the file keeps the placeholder.
	t.Setenv("BASALT_ABSENT_PG_KEY", "sentinel-not-a-secret")
	t.Setenv("BASALT_ABSENT_PG_DSN", "postgres://basalt:sentinel-not-a-secret@127.0.0.1:1/basalt_test")
	config := "database:\n" +
		"  dsn: ${BASALT_ABSENT_PG_DSN}\n" +
		"  connect_timeout_seconds: 1\n" +
		"http:\n" +
		"  addr: 127.0.0.1:0\n" +
		"  admin_auth:\n" +
		"    mode: local_trust\n" +
		"providers:\n" +
		"  - name: fake\n" +
		"    base_url: http://127.0.0.1:1/v1\n" +
		"    api_key: ${BASALT_ABSENT_PG_KEY}\n" +
		"    default_model: fake-model\n" + serveTestIngress
	require.NoError(t, os.WriteFile(filepath.Join(workDir, configFileName), []byte(config), 0o644))

	// ① 客户端层：错误文案必须点名"连接 PostgreSQL 失败"——回退分支会让 Open 成功返回。
	// The client-level verdict: the error names the failed PostgreSQL connection.
	_, openErr := postgres.Open(context.Background(), postgres.Config{
		DSN:            os.Getenv("BASALT_ABSENT_PG_DSN"),
		ConnectTimeout: time.Second,
	})
	require.ErrorContains(t, openErr, "连接 PostgreSQL 失败")

	// ② 启动层：serve 以退出码 1 收场，且工作区里**没有**出现 SQLite 库文件。
	// The startup-level verdict: exit code 1 and no SQLite file in the workspace.
	code := Run(context.Background(), []string{"serve"})
	require.Equal(t, 1, code, "PostgreSQL 不可达时 serve 必须拒绝启动（D-01）")
	require.NoFileExists(t, filepath.Join(workDir, workspaceDirName, databaseFileName),
		"不得回退 SQLite：工作区里不该出现库文件")
}

//go:build integration

package app

// 本文件守C-37/G2-17的双谱系注入。US-16A批准0006后PG与SQLite的数字恰好同为6，
// 仍须各自版本化、按后端分别注入；数字不等已不能作为独立性的判据。
// 实际init/db migrate/status链路、两后端表身份及VersionLineageOrigins的包来源检查共同防串用。
// The independent lineages happen to share version six after the approved OBS migration.
// Real backend operations, schema identity and origin checks continue to prevent cross-injection.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
	"github.com/ymedword/basalt/internal/tool"
)

// TestApp_SupportedVersionInjectedPerBackend：整条链路上只出现一次版本交叉的机会
// （装配层），所以每个断言都锚在"哪个后端拿到了哪个版本"上。
// 逆改点：交换任一注入符号，VersionLineageOrigins即使在数字同6时也会红；
// 将迁移/源库建错后端或版本，下面真实数据库往返与status仍会红。
// Every assertion anchors on which backend received which version.
func TestApp_SupportedVersionInjectedPerBackend(t *testing.T) {
	// ① 分别检查已批准版本；来源独立性由包符号与真实后端身份判定。
	// Check each approved version; origin and schema identity prove backend separation.
	// 运行期谱系自 v1 起；D-19（AGENT.md `knowledge:` 绑定）以**前向迁移**
	// `0002_agent_binding_knowledge_kind.sql` 加宽 `resource_kind` 闭集并前移到 2；
	// R59 的 `source_text` 补列修复以 `0003_knowledge_source_text_repair.sql` 前移到 3
	// ——已发布的 0001 现场字面量保持逐字节不变（D-19 裁决 2026-10-30 / R59 2026-10-31）。
	require.GreaterOrEqual(t, postgres.SupportedSchemaVersion, 11,
		"运行期PostgreSQL至少包含已批准的 0009 隔离、0010 学习准入与 0011 记忆学习提取迁移（US-24 追加 0012），SQLite 源谱系独立")
	require.Equal(t, 6, storage.SupportedSchemaVersion, "SQLite 源谱系保持 v6（导出 / 旧库升级来源）")

	workDir := t.TempDir()
	t.Chdir(workDir)
	// `Run` 会安装进程级出网守门器；本包测试共用一个进程，用例结束必须卸掉。
	// Run installs the process-wide outbound guard; tests share the process.
	t.Cleanup(func() { tool.SetNetGuard(nil) })

	dsn := pgtest.NewTestSchema(t)
	// 凭证只经进程环境注入，配置文件里保留 `${VAR}` 占位（明文清单会被装载层拒绝）。
	// The credential travels through the environment; the file keeps the placeholder.
	t.Setenv("BASALT_LINEAGE_KEY", "sentinel-not-a-secret")
	t.Setenv("BASALT_LINEAGE_DSN", dsn)
	config := "database:\n" +
		"  dsn: ${BASALT_LINEAGE_DSN}\n" +
		"  connect_timeout_seconds: 5\n" +
		"http:\n" +
		"  addr: 127.0.0.1:0\n" +
		"  admin_auth:\n" +
		"    mode: local_trust\n" +
		"providers:\n" +
		"  - name: fake\n" +
		"    base_url: http://127.0.0.1:1/v1\n" +
		"    api_key: ${BASALT_LINEAGE_KEY}\n" +
		"    default_model: fake-model\n"
	require.NoError(t, os.WriteFile(filepath.Join(workDir, configFileName), []byte(config), 0o600))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// ② `init` 走 SQLite 源谱系：建出来的库必须是 v6。用 v6 当 target 打开是第二道——
	// 库真被写成 v1 时，这里的只读校验会先报"库内 v1 < 目标 v6"。
	// init provisions the SQLite source lineage at v6; opening with target=6 re-checks it.
	require.Equal(t, 0, Run(ctx, []string{"init"}), "init 必须成功建出工作区与源库")
	sqliteDB, err := storage.OpenReadOnly(ctx,
		filepath.Join(workDir, workspaceDirName, databaseFileName), storage.SupportedSchemaVersion)
	require.NoError(t, err, "init 的源库必须按 SQLite 谱系（v6）建出")
	sqliteVersion, err := sqliteDB.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, storage.SupportedSchemaVersion, sqliteVersion,
		"源库版本是 SQLite 谱系的 6")
	sqliteTables, err := sqliteDB.TableNames(ctx)
	require.NoError(t, err)
	for _, name := range []string{"observability_alert_states", "observability_alert_transitions", "observability_cleanup_runs", "observability_export_batches", "observability_export_items", "observability_export_scan"} {
		require.NotContains(t, sqliteTables, name, "SQLite源谱系不包含PG观测表")
	}
	require.NoError(t, sqliteDB.Close())

	// ③ db migrate只迁移PG运行期谱系，OBS表不得进入SQLite源库。
	// db migrate provisions PostgreSQL without adding OBS tables to the source lineage.
	require.Equal(t, 0, Run(ctx, []string{"db", "migrate"}), "db migrate 必须建出运行期库")
	pgDB, err := postgres.OpenReadOnly(ctx, postgres.Config{DSN: dsn})
	require.NoError(t, err)
	pgVersion, err := pgDB.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, postgres.SupportedSchemaVersion, pgVersion, "PG谱系应迁到二进制声明的当前版本")
	pgTables, err := pgDB.TableNames(ctx)
	require.NoError(t, err)
	for _, name := range []string{"observability_alert_states", "observability_alert_transitions", "observability_cleanup_runs", "observability_export_batches", "observability_export_items", "observability_export_scan"} {
		require.Contains(t, pgTables, name, "PG运行期按自身迁移拥有OBS表")
	}
	require.NoError(t, pgDB.Close())

	// ④ 注入值直达命令面：`status` 拿 `Deps.SupportedSchemaVersion` 与库版本对照。
	// 版本比较仍必须使用运行期后端的注入值。
	// The injected version reaches the command face: status compares it with the library.
	require.Equal(t, 0, Run(ctx, []string{"status"}),
		"status必须把PG库判为与二进制同版本（使用PG自身谱系）")
}

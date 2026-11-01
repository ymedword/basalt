//go:build integration

package app

// 本文件守 C-37 / G2-17 的**双谱系注入**（US-16 §4 的
// `TestApp_SupportedVersionInjectedPerBackend`）：`storage.SupportedSchemaVersion`（SQLite
// 源谱系，6）与 `postgres.SupportedSchemaVersion`（运行期谱系，D-19 前向迁移 0002 +
// R59 补列修复 0003 后为 3）各自版本化，装配层**按后端分别注入**——`init` / 旧库升级
// 拿 6，运行期命令面拿 3。
// 三个可判事实：
//
//  1. 两个常量**互不相等**（"统一成一个版本"会直接红）；
//  2. `init` 建出的 SQLite 源库是 v6（不是 3）；
//  3. `db migrate` 建出的 PG 库是 v3（不是 6），且同一二进制随后 `status` 把该库判为
//     **与自身同版本**——`status` 比的正是装配注入的 `Deps.SupportedSchemaVersion`，
//     注入成 6 就会得到"库 v3 < 目标 v6"并以 1 退出（C-37 症状"6 与 v3 直接比较"）。
//
// 版本数字随迁移链前移（v2 → v3 是 R59 补列修复，2026-10-31）；本文件判的是"按后端
// 分别注入"，不是某个具体数字——数字断言只是让"有人偷偷统一"可判。
// The two lineages are versioned separately and injected per backend: init/legacy upgrade
// get 6, the runtime command face gets 3 (after the D-19 and R59 forward migrations).

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
// 逆改点：把 `cli.Deps.SupportedSchemaVersion` 换成 `storage.SupportedSchemaVersion`
// （6）→ ③ 的 `status` 报"库版本旧于二进制（v3 < v6）"、退出码 1，断言红；
// 把 `SourceSchemaVersion` 换成 PG 的 1 → `init` 建出的库是 v1，② 的 `OpenReadOnly`
// 以 target=6 打开即报 `ErrSchemaOutdated`，断言红。
// Every assertion anchors on which backend received which version.
func TestApp_SupportedVersionInjectedPerBackend(t *testing.T) {
	// ① 两条谱系的数值本身：相等就说明有人把一条"统一"到了另一条上。
	// The lineage constants themselves; equality would mean one was unified onto the other.
	// 运行期谱系自 v1 起；D-19（AGENT.md `knowledge:` 绑定）以**前向迁移**
	// `0002_agent_binding_knowledge_kind.sql` 加宽 `resource_kind` 闭集并前移到 2；
	// R59 的 `source_text` 补列修复以 `0003_knowledge_source_text_repair.sql` 前移到 3
	// ——已发布的 0001 现场字面量保持逐字节不变（D-19 裁决 2026-10-30 / R59 2026-10-31）。
	require.Equal(t, 5, postgres.SupportedSchemaVersion, "运行期 PostgreSQL 包含版本修订和集合简介两次前向迁移")
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
	require.NotEqual(t, postgres.SupportedSchemaVersion, sqliteVersion,
		"不得把运行期谱系的 3 传给旧库升级 / 导出")
	require.NoError(t, sqliteDB.Close())

	// ③ `db migrate` 走运行期谱系：PG 库必须落 v3，而不是沿用 SQLite 的 6。
	// db migrate provisions the runtime lineage: PG lands on v3, never the SQLite 6.
	require.Equal(t, 0, Run(ctx, []string{"db", "migrate"}), "db migrate 必须建出运行期库")
	pgDB, err := postgres.OpenReadOnly(ctx, postgres.Config{DSN: dsn})
	require.NoError(t, err)
	pgVersion, err := pgDB.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, postgres.SupportedSchemaVersion, pgVersion, "PG 谱系应迁到当前版本 v5")
	require.NotEqual(t, storage.SupportedSchemaVersion, pgVersion,
		"不得让 PG 迁移编号沿用 SQLite 的 v6")
	require.NoError(t, pgDB.Close())

	// ④ 注入值直达命令面：`status` 拿 `Deps.SupportedSchemaVersion` 与库版本对照。
	// 注入成 6 时这里必然是"库版本旧于二进制（v3 < v6）"，退出码 1。
	// The injected version reaches the command face: status compares it with the library.
	require.Equal(t, 0, Run(ctx, []string{"status"}),
		"status 必须把 PG 库判为与二进制同版本（注入的是 PG 谱系的 3）")
}

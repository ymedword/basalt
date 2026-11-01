package postgres

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件是迁移的**无库**用例：内嵌脚本的定位纪律（一个版本恰好一个文件、缺文件即拒绝）
// 与 v1 基线脚本的内容锚点。需要真实 PostgreSQL 的建库 / 锁语义在
// migrate_integration_test.go（`integration` 构建标签）。
// The no-database half of the migration gate: one file per version, and the v1 baseline
// script really is the frozen contract.

// TestPostgres_MigrationScriptRejectsMissingVersion 判"缺文件即拒绝、不得跳版"：没有
// 对应文件的版本必须报错，而不是静默跳过把库标成已迁移。
// 逆改点：把 `len(matches) == 0` 分支改成返回空脚本 + nil → 本用例红。
func TestPostgres_MigrationScriptRejectsMissingVersion(t *testing.T) {
	t.Parallel()

	// 版本探针取"当前支持版本 + 1"而不是写死数字：新增迁移时探针自动前移，不会因为
	// 新脚本落地就把本用例误判成红（2026-10-30 D-19 加 0002 时踩到的形态）。
	missing := SupportedSchemaVersion + 1
	_, _, err := migrationScript(missing)
	require.Error(t, err, "缺少迁移文件的版本必须拒绝而不是跳过")
	require.Contains(t, err.Error(), fmt.Sprintf("缺少 v%d 迁移文件", missing))
	require.Contains(t, err.Error(), "不得跳版")

	_, _, err = migrationScript(0)
	require.Error(t, err, "版本从 1 起，0 是编程错误")
	require.Contains(t, err.Error(), "版本从 1 起")
}

// TestPostgres_MigrationScriptV1IsBaseline 钉住 v1 基线的身份：描述串与 `schema_version`
// 记录里的字面量一致（`Open` 建库后写的就是它），脚本内容确实是契约的建表脚本。
// 与契约文件的**逐字节**比对归 T105a 的 `TestPostgresSchemaFile_MatchesContract`，
// 本用例只防"拿错文件当 v1"。
// 逆改点：把 0001 文件名改成别的描述 → 描述断言红；换成空文件 → 内容断言红。
func TestPostgres_MigrationScriptV1IsBaseline(t *testing.T) {
	t.Parallel()

	script, description, err := migrationScript(1)
	require.NoError(t, err)
	require.Equal(t, "migrations/0001_postgres_v1.sql", description)
	require.Contains(t, script, "CREATE TABLE IF NOT EXISTS schema_version")
	require.Contains(t, script, "CREATE TABLE IF NOT EXISTS callers")
	require.NotContains(t, script, "CREATE EXTENSION",
		"扩展由部署者预装，迁移脚本不得自行创建（契约 D7 / ADR-02）")
}

// TestPostgres_MigrationScriptV2WidensBindingKinds 钉住 v2 迁移的身份与边界：它把
// `agent_resource_bindings.resource_kind` 的闭集加宽到含 `knowledge_collection`
// （D-19），且**只加宽闭集**——不新增表、不动 v1 基线、不碰扩展。
// 逆改点：把新值从 CHECK 里删掉 → 闭集断言红；改成 CREATE TABLE → 建表断言红。
func TestPostgres_MigrationScriptV2WidensBindingKinds(t *testing.T) {
	t.Parallel()

	script, description, err := migrationScript(2)
	require.NoError(t, err)
	require.Equal(t, "migrations/0002_agent_binding_knowledge_kind.sql", description)

	require.Contains(t, script, "DROP CONSTRAINT IF EXISTS agent_resource_bindings_resource_kind_check",
		"重跑安全靠 IF EXISTS；缺它时第二次迁移直接报错")
	require.Contains(t, script,
		"CHECK (resource_kind IN ('model_connection', 'notify_channel', 'knowledge_collection'))",
		"闭集必须含 D-19 的 knowledge_collection，且保留既有两类")
	require.NotContains(t, script, "CREATE TABLE",
		"v2 只加宽闭集——契约 v1 的 52 张表集合不得变化")
	require.NotContains(t, script, "CREATE EXTENSION")
}

// TestPostgres_MigrationScriptV3RepairsSourceText 钉住 v3 迁移的身份与边界：它给
// `kb_document_versions` 补回 R59 就地改写后既有库缺失的 `source_text` 列，且**只补列**
// ——不新增表、不改列类型、不碰扩展。幂等靠 `IF NOT EXISTS`：新建库里 0001 已建过该列，
// 0003 必须空转而不是报错。
// 逆改点：删掉 `IF NOT EXISTS` → 新建库迁移（0001 已建列）在 0003 报 column already
// exists；把 `text` 改成别的类型 → 类型断言红；改成 CREATE TABLE → 建表断言红。
func TestPostgres_MigrationScriptV3RepairsSourceText(t *testing.T) {
	t.Parallel()

	script, description, err := migrationScript(3)
	require.NoError(t, err)
	require.Equal(t, "migrations/0003_knowledge_source_text_repair.sql", description)

	require.Contains(t, script, "ALTER TABLE kb_document_versions")
	require.Contains(t, script, "ADD COLUMN IF NOT EXISTS source_text text",
		"补列必须幂等且与契约同型——缺 IF NOT EXISTS 时新建库的迁移会撞列已存在")
	require.NotContains(t, script, "CREATE TABLE",
		"v3 只补列——契约 v1 的 52 张表集合不得变化")
	require.NotContains(t, script, "CREATE EXTENSION")
	require.NotContains(t, script, "DROP COLUMN",
		"补列脚本不得带破坏性动作：已发布的迁移只追加，不回退既有数据")
}

//go:build integration

package dbmigrate

// 本文件守 D-22：存量工作区的 `.basalt/memory/MEMORY.md` 在迁移链路上**不搬迁、不删除、
// 不改写**——它是一次性只读导入的来源，导入后原样留作回退层（技术方案 §3.5.2）。
// The workspace memory file must survive the migration chain byte for byte: it is the
// read-only source of the one-shot import and stays behind as the rollback layer (D-22).

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite" // 夹具要按真实路径在工作区里建源库，驱动经空白导入注册

	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// TestMigration_KeepsGlobalMemoryFileUnchanged 守 D-22：源库就放在工作区里（真实现场
// 是 `<WorkDir>/.basalt/basalt.db`），整条导出 → 导入跑完之后工作区的
// `memory/MEMORY.md` 必须**仍然存在、逐字节不变、权限不变**。
//
// 逆改点：在迁移链路里加入"导入后清理旧记忆载体"（或把 MEMORY.md 当资产重写 / 重排）→
// 文件被删、内容变化或模式变化，三条断言红。本用例钉的是**迁移不得触碰工作区文件**这个
// 行为边界，不涉及 `ImportLegacy` 的逐条对账（那是 `internal/memory` 的验收面）。
func TestMigration_KeepsGlobalMemoryFileUnchanged(t *testing.T) {
	workspace := t.TempDir()
	memoryPath := filepath.Join(workspace, "memory", "MEMORY.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(memoryPath), 0o755))
	original := []byte("# 全局记忆\n\n- 迁移前就存在的一条\n")
	require.NoError(t, os.WriteFile(memoryPath, original, 0o644))
	before, err := os.Stat(memoryPath)
	require.NoError(t, err)

	source := openWorkspaceV6SQLite(t, filepath.Join(workspace, "basalt.db"))
	execFixture(t, source, `INSERT INTO sessions
		(session_id, profile_name, channel, user_id, status, message_count, created_at, last_active_at)
		VALUES ('s1', 'demo', 'cli', 'u1', 'active', 1, 1000, 1000)`)

	dir, manifest := exportFixture(t, source)
	db := pgtest.NewTestDB(t)
	_, err = NewImporter(db, dir, manifest).Import(importCtx)
	require.NoError(t, err)

	after, err := os.ReadFile(memoryPath)
	require.NoError(t, err, "导入后 memory/MEMORY.md 必须仍然存在（D-22：不删除）")
	require.Equal(t, original, after, "导入后 memory/MEMORY.md 必须逐字节不变（D-22：不改写）")

	info, err := os.Stat(memoryPath)
	require.NoError(t, err)
	// 跨平台判据：迁移前后模式逐位一致（Windows 上也能抓住"被改成只读"这类改写）。
	require.Equal(t, before.Mode(), info.Mode(), "文件模式不得被迁移改写")
	// Unix 绝对判据：夹具按 0o644 建立，迁移后仍必须是 0o644。Windows 不保存 Unix 权限位
	// （可写文件恒读作 0o666，见 `asset.go` 的同一守卫），该绝对判据只在非 Windows 生效。
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "Unix 上文件模式必须是 0o644")
	}
}

// openWorkspaceV6SQLite 在**指定路径**建一份真实的 SQLite v6 源库（契约脚本整脚本执行）。
// 与 `testutil.SQLiteFixture` 的差别只有位置：本用例要求源库落在工作区里。
func openWorkspaceV6SQLite(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// 单连接：契约脚本的 `PRAGMA foreign_keys = ON` 是连接级设置（同 migrationFixture）。
	db.SetMaxOpenConns(1)

	execFixture(t, db, `CREATE TABLE schema_version (
		version     INTEGER PRIMARY KEY,
		applied_at  INTEGER NOT NULL,
		description TEXT    NOT NULL)`)
	execFixture(t, db,
		`INSERT INTO schema_version (version, applied_at, description) VALUES (6, 0, 'workspace fixture')`)

	script, err := os.ReadFile(filepath.Join("..", "storage", "schema.sql"))
	require.NoError(t, err, "必须读得到随二进制嵌入的 v6 契约脚本")
	_, err = db.Exec(string(script))
	require.NoError(t, err, "契约脚本必须在工作区源库上整脚本执行成功")
	return db
}

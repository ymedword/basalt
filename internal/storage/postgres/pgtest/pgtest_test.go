//go:build integration

package pgtest

// 本文件覆盖 T018b 的三条守卫（设计 §4.4 片段约定 / D-01）：测试库守卫拒绝把既有
// Basalt 实例库当一次性目标、并行用例落在互不可见的 schema 里、句柄不落任何本地文件
// （SQLite 旁路的反证）。
// These tests pin the pgtest guards: the DSN name check, parallel schema isolation,
// and the absence of any on-disk (SQLite) side effect.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPGTest_RejectsNonTestDatabase 判守卫本身：非 `basalt_test` 库名一律拒绝，只有
// 显式 `BASALT_TEST_PG_ALLOW` 才放行；URL 与 keyword 两种写法都要认库名。
// 逆改点：去掉库名校验 → 断言红；把放行条件写成"默认允许" → 前两条红。
func TestPGTest_RejectsNonTestDatabase(t *testing.T) {
	urlDSN := "postgres://basalt:secret@127.0.0.1:5432/basalt?sslmode=disable"
	require.Error(t, checkTestDSN(urlDSN, ""), "既有实例库不得当测试库")

	err := checkTestDSN(urlDSN, "1")
	require.NoError(t, err, "显式放行必须生效")

	require.NoError(t, checkTestDSN("postgres://u:p@h:5432/basalt_test?sslmode=disable", ""))
	require.NoError(t, checkTestDSN("host=h dbname=basalt_test user=u", ""))
	require.Error(t, checkTestDSN("host=h dbname=basalt user=u", ""), "keyword 写法同样要判库名")
	require.Error(t, checkTestDSN("postgres://u:p@h:5432/", ""), "缺库名必须拒绝")
}

// TestPGTest_IsolationUnderParallel 判隔离：两个并行子用例用同一主键写入 `callers`，
// 共享 schema 时第二个必然撞主键；各自只看得见自己那一行。
// 逆改点：去掉随机 schema（共用一个）→ 第二个子用例的 INSERT 报 23505。
func TestPGTest_IsolationUnderParallel(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			db := NewTestDB(t)

			_, err := db.ExecContext(ctx, `INSERT INTO callers
				(caller_id, kind, name, project, status, created_at)
				VALUES ('iso', 'service', 'iso', '', 'active', $1)`,
				time.Now().UTC().UnixMilli())
			require.NoError(t, err, "隔离 schema 下主键不得与其他并行用例冲突")

			var count int
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT count(*) FROM callers WHERE caller_id = 'iso'`).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}

// TestPGTest_NoSQLiteOpened 判句柄不落本地文件：切到空目录后建库，目录必须仍然为空
// （测试库路径上不存在 SQLite 旁路）。
// 逆改点：句柄顺带打开 SQLite 文件 → 目录出现 `basalt.db`，断言红。
func TestPGTest_NoSQLiteOpened(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	db := NewTestDB(t)
	require.NoError(t, db.Ping(context.Background()))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "测试库句柄不得在工作目录落任何文件")
}

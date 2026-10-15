package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// writablePath 给每个用例一个独立的库文件（测试隔离纪律：不得共享样例库）。
// writablePath hands every test its own database file.
func writablePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "basalt.db")
}

// TestOpenWritable_CreatesNewDatabase：新库（无版本表）→ 全量建表 + 版本行
// = target，同一事务（能力层）。
func TestOpenWritable_CreatesNewDatabase(t *testing.T) {
	path := writablePath(t)

	db, err := OpenWritable(context.Background(), path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	version, err := db.SchemaVersion(context.Background())
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)

	names, err := db.TableNames(context.Background())
	require.NoError(t, err)
	require.Equal(t, contractTables, names)
}

// TestOpenWritable_OpensExistingCurrent：当前版本库二次打开正常，版本不变。
func TestOpenWritable_OpensExistingCurrent(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	first, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	// Close 幂等：显式关一次以重开，另注册一次兜底（中途失败也不留开着句柄的文件）。
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	require.NoError(t, first.Close())

	second, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })

	version, err := second.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)
}

// TestOpenWritable_OldVersionOpensWithoutMigrating：旧库打开**成功**且版本不变
// ——迁移是显式动作（评审修订三稿 P1：写入口分层）。
func TestOpenWritable_OldVersionOpensWithoutMigrating(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	created, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, created.Close()) })
	require.NoError(t, created.Close())

	// 目标版本提升（模拟"二进制升级了、库还是旧的"）
	db, err := OpenWritable(ctx, path, SupportedSchemaVersion+1)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	version, err := db.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version, "打开旧库不得顺手迁移")
}

// TestOpenWritable_RejectsTooNew：库内版本高于目标 → ErrSchemaTooNew。
func TestOpenWritable_RejectsTooNew(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	newer, err := OpenWritable(ctx, path, SupportedSchemaVersion+1)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, newer.Close()) })
	require.NoError(t, newer.Close())

	_, err = OpenWritable(ctx, path, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaTooNew)
}

// TestOpenWritable_RejectsCorrupt：版本表存在但零行 → ErrSchemaCorrupt。
func TestOpenWritable_RejectsCorrupt(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)
	bare := openBare(t, path)
	_, err := bare.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL)`)
	require.NoError(t, err)

	_, err = OpenWritable(ctx, path, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaCorrupt)
}

// TestOpenWritable_PragmasOnEveryConnection：两条独立物理连接各自
// foreign_keys=1、busy_timeout=5000——反「只设第一条连接」经典坑（契约迁移
// 纪律 6：逐物理连接）。连接级设置若只在启动执行一次，池子里后续新开的连接
// 会带着默认值（foreign_keys 默认关），本用例判红。
func TestOpenWritable_PragmasOnEveryConnection(t *testing.T) {
	path := writablePath(t)

	db, err := OpenWritable(context.Background(), path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	ctx := context.Background()
	pools := map[string]*sql.DB{"写池": db.write, "读池": db.read}
	for name, pool := range pools {
		conn, err := pool.Conn(ctx)
		require.NoError(t, err, "%s 取连接", name)

		var foreignKeys int
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys), "%s foreign_keys", name)
		require.Equal(t, 1, foreignKeys, "%s 的 foreign_keys 应当为 1", name)

		var busyTimeout int
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout), "%s busy_timeout", name)
		require.Equal(t, busyTimeoutMillis, busyTimeout, "%s 的 busy_timeout 应当为 %d", name, busyTimeoutMillis)

		require.NoError(t, conn.Close())
	}

	// 写池恒为一条物理连接：SQLite 单写者，多写连接只会互相等锁。
	require.Equal(t, 1, db.write.Stats().MaxOpenConnections)
}

// TestOpenWritable_JournalModeWALPersists：创建后回读 wal；重开仍是 WAL（库级
// 持久属性，写在库头里）。
func TestOpenWritable_JournalModeWALPersists(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	first, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	require.Equal(t, journalModeWAL, journalMode(t, first))
	require.NoError(t, first.Close())

	second, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })

	require.Equal(t, journalModeWAL, journalMode(t, second), "WAL 是库级持久属性，重开必须仍是 wal")
}

// TestOpenWritable_ExistingDatabaseStillSwitchesToWAL：OpenWritable 对**已有**
// 当前版本库仍确保 WAL——Bootstrap 的 Kept 零写承诺只约束引导路径（连库头都不
// 碰），OpenWritable 是长期服务入口，多读并发依赖 WAL，其装配策略另行规定。
// OpenWritable keeps ensuring WAL for an existing database; the zero-write
// promise is scoped to the Bootstrap path only.
//
// 夹具刻意用 DELETE 模式的完整库：库若已是 WAL，切换是空操作，夹具自己把本用例
// 要抓的回归遮掉。
func TestOpenWritable_ExistingDatabaseStillSwitchesToWAL(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)
	makeFullDeleteModeDatabase(t, path)

	db, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	require.Equal(t, journalModeWAL, journalMode(t, db), "OpenWritable 必须把已有库带入 WAL")
}

// journalMode 回读库的 journal_mode。
// journalMode reads back the journal mode of the database.
func journalMode(t *testing.T, db *DB) string {
	t.Helper()
	var mode string
	require.NoError(t, db.read.QueryRowContext(context.Background(), "PRAGMA journal_mode").Scan(&mode))
	return mode
}

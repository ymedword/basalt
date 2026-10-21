package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// requireLibraryVersion 用一条**独立于被测句柄**的裸连接回读库内版本——「拒绝后
// 版本不变」这类断言的判据不该建立在被测对象自己的读数上。
// requireLibraryVersion reads the stored version back through an independent bare
// connection, so "the version did not move" does not rest on the subject's own
// reporting.
func requireLibraryVersion(t *testing.T, path string, want int) {
	t.Helper()
	require.Equal(t, want, mustVersion(t, openBare(t, path)))
}

// TestOpenReadOnly_ReadsCurrent：当前版本可读——版本与表名可清点（status 节 3 的
// 数据源），且句柄真是只读的：没有写池、SQL 层的写入被库拒绝。
func TestOpenReadOnly_ReadsCurrent(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	created, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	require.NoError(t, created.Close())

	db, err := OpenReadOnly(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	version, err := db.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)

	names, err := db.TableNames(ctx)
	require.NoError(t, err)
	require.Equal(t, contractTables, names)

	// 只读句柄不装配写池——边界由句柄承载，不靠调用方自觉（见 DB 类型注释）。
	require.Nil(t, db.write, "只读句柄不得持有写池")

	// 真写一次：连接串的 mode=ro 让写入在库层面失败，而不是"约定不写"。
	_, err = db.read.ExecContext(ctx, "CREATE TABLE readonly_probe (id INTEGER)")
	require.Error(t, err, "只读句柄上的写入必须被拒绝")
}

// TestOpenReadOnly_RejectsTooNew：库内版本高于目标 → ErrSchemaTooNew；只校验、
// 不迁移（版本保持原样）。
func TestOpenReadOnly_RejectsTooNew(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	newer, err := OpenWritable(ctx, path, SupportedSchemaVersion+1)
	require.NoError(t, err)
	require.NoError(t, newer.Close())

	_, err = OpenReadOnly(ctx, path, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaTooNew)

	requireLibraryVersion(t, path, SupportedSchemaVersion+1)
}

// TestOpenReadOnly_RejectsOutdated：库内版本低于目标 → ErrSchemaOutdated；只校验、
// 不迁移——"打开旧库顺手升级"是被禁止的（升级归持锁重命令的显式动作）。
func TestOpenReadOnly_RejectsOutdated(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	older, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	require.NoError(t, older.Close())

	_, err = OpenReadOnly(ctx, path, SupportedSchemaVersion+1)
	require.ErrorIs(t, err, core.ErrSchemaOutdated)

	requireLibraryVersion(t, path, SupportedSchemaVersion)
}

// TestOpenReadOnly_RejectsCorrupt：版本表存在但零行 → ErrSchemaCorrupt（既非新库
// 也非任何已知版本，fail-closed 不猜）。
func TestOpenReadOnly_RejectsCorrupt(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	bare := openBare(t, path)
	_, err := bare.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL)`)
	require.NoError(t, err)

	_, err = OpenReadOnly(ctx, path, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaCorrupt)
}

// TestOpenReadOnly_MissingDatabase：库缺失 → ErrDatabaseMissing，**不代建**——
// status 是纯诊断，缺库要报"未就绪"而不是凭空造一个。
func TestOpenReadOnly_MissingDatabase(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	_, err := OpenReadOnly(ctx, path, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrDatabaseMissing)
	require.NoFileExists(t, path, "只读打开不得创建库文件")
}

// TestPing_ReportsReadPoolHealth：Ping 只探**读池**——只读句柄也能回答（系统概览的
// database.status）；未装配读池 / 已关闭的句柄如实报错，不得把失败伪装成 ok。
// Ping checks the read pool only: a read-only handle answers, a closed one reports an
// error instead of a fake ok.
func TestPing_ReportsReadPoolHealth(t *testing.T) {
	ctx := context.Background()
	path := writablePath(t)

	created, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	require.NoError(t, created.Close())

	db, err := OpenReadOnly(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	require.NoError(t, db.Ping(ctx), "只读句柄不得因缺少写池而报错")

	require.Error(t, (&DB{}).Ping(ctx), "未装配读池的句柄拒绝探活")

	require.NoError(t, db.Close())
	require.Error(t, db.Ping(ctx), "关闭后的句柄不得报 ok")
}

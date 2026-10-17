package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestBootstrap_CreatesNewDatabase：init 缺库 → created=true、11 表 + version
// = target（需求文档 §3.1 的 basalt.db）。
func TestBootstrap_CreatesNewDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")

	created, err := Bootstrap(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	require.True(t, created, "缺库引导必须报告实际创建")

	// 引导不留长期写句柄，终态经只读打开复核。
	db, err := OpenReadOnly(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	version, err := db.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version)

	names, err := db.TableNames(ctx)
	require.NoError(t, err)
	require.Equal(t, contractTables, names)
}

// TestBootstrap_KeepsExistingCurrent：当前版本 → created=false、零写（版本行
// 不变——技术方案 §3.1.1 幂等；契约 §4.3.1 已存在库零写）。
func TestBootstrap_KeepsExistingCurrent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")

	first, err := Bootstrap(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	require.True(t, first)

	second, err := Bootstrap(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	require.False(t, second, "二跑必须报告 Kept")
}

// TestBootstrap_RefusesExistingOutdated：旧版本（target 注入合成值构造）→
// ErrSchemaOutdated 且**不迁移**（版本不变）。
func TestBootstrap_RefusesExistingOutdated(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")

	// 先按 v1 建库，再以 target=2 引导——模拟"二进制升级、库还是旧的"。
	created, err := Bootstrap(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	require.True(t, created)

	_, err = Bootstrap(ctx, path, SupportedSchemaVersion+1)
	require.ErrorIs(t, err, core.ErrSchemaOutdated)

	db, err := OpenReadOnly(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	version, err := db.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, version, "引导不得迁移旧库")
}

// TestBootstrap_RefusesTooNew：过高 / 零行 → 各自哨兵错误（fail-closed）。
func TestBootstrap_RefusesTooNew(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")

	newer, err := OpenWritable(ctx, path, SupportedSchemaVersion+1)
	require.NoError(t, err)
	require.NoError(t, newer.Close())

	_, err = Bootstrap(ctx, path, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaTooNew)
}

// TestBootstrap_RefusesCorrupt：版本表零行 → ErrSchemaCorrupt。
func TestBootstrap_RefusesCorrupt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")
	bare := openBare(t, path)
	_, err := bare.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL)`)
	require.NoError(t, err)

	_, err = Bootstrap(ctx, path, SupportedSchemaVersion)
	require.ErrorIs(t, err, core.ErrSchemaCorrupt)
}

// TestBootstrap_RefusalLeavesDatabaseUntouched：被拒绝的库必须**逐字节不变**——
// 判态先于持久化修改（WAL 切换会改写库头，是库级持久属性）。
// Refused databases must be left byte-identical: the state judgement precedes
// the persistent WAL switch.
//
// 夹具刻意用 DELETE 模式的库：库若已是 WAL，切换是空操作，夹具自己把缺陷遮掉
// （旧版实现此用例判红：过高 / 旧版库在被拒绝的 init 里被切成 WAL，SHA256 改变）。
// The fixture is deliberately in DELETE mode — a WAL fixture would mask the bug.
func TestBootstrap_RefusalLeavesDatabaseUntouched(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		version int // 库内版本
		target  int // 引导目标版本
		wantErr error
	}{
		{"过高版本：库 v2、二进制 v1", 2, 1, core.ErrSchemaTooNew},
		{"旧版本：库 v1、二进制 v2（Bootstrap 拒绝）", 1, 2, core.ErrSchemaOutdated},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "basalt.db")
			makeDeleteModeDatabase(t, path, tc.version)

			before := sha256File(t, path)

			_, err := Bootstrap(ctx, path, tc.target)
			require.ErrorIs(t, err, tc.wantErr)

			require.Equal(t, before, sha256File(t, path),
				"被拒绝的库不得被改动一个字节（判态必须先于 WAL 切换）")
			require.NoFileExists(t, path+"-wal", "拒绝路径不得把库带入 WAL")
			require.NoFileExists(t, path+"-shm", "拒绝路径不得物化 WAL 索引文件")
		})
	}
}

// makeFullDeleteModeDatabase 造一个**完整 11 表 + 版本行**、journal_mode 停留在
// delete 的库——Kept 零写类用例的夹具（用户复现口径：完整 v1、DELETE 模式的库）。
// 建表直接走 createNewDatabase 本体而非先 Bootstrap 再切回：夹具不得依赖待测
// 代码，也保证 WAL 从未开启过。
// A full-schema fixture in plain DELETE mode; the tables are created directly so
// no WAL switch ever happened and the fixture never routes through Bootstrap.
func makeFullDeleteModeDatabase(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()

	db, err := sql.Open(driverName, "file:"+filepath.ToSlash(path))
	require.NoError(t, err)
	require.NoError(t, withWriteLock(ctx, db, func(ctx context.Context, conn *sql.Conn) error {
		return createNewDatabase(ctx, conn, schemaFS, SupportedSchemaVersion)
	}))

	var mode string
	require.NoError(t, db.QueryRow("PRAGMA journal_mode").Scan(&mode))
	require.Equal(t, "delete", mode, "夹具必须停留在 DELETE 模式——WAL 夹具会遮掉本缺陷")
	require.NoError(t, db.Close())
}

// TestBootstrap_ExistingCurrentStaysUntouched：当前版本的**完整库**在 Kept 路径上
// 必须零写——「Kept」的承诺是对库字节零改动，连 WAL 切换（库级持久属性、改写
// 库头）都不得发生。
// Kept must be byte-for-byte zero-write: even the persistent WAL switch is
// skipped for an existing current-version database.
//
// 夹具刻意用 DELETE 模式的完整库：库若已是 WAL，切换是空操作，夹具自己把缺陷
// 遮掉（P2 回归：旧版实现此用例判红——init 报"零写"、exit 0，库头却变成 WAL、
// SHA256 改变）。
func TestBootstrap_ExistingCurrentStaysUntouched(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")
	makeFullDeleteModeDatabase(t, path)

	before := sha256File(t, path)

	created, err := Bootstrap(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	require.False(t, created, "已有当前版本库必须报告 Kept")

	require.Equal(t, before, sha256File(t, path), "Kept 路径不得改动库一个字节")
	require.NoFileExists(t, path+"-wal", "Kept 路径不得把库带入 WAL")
	require.NoFileExists(t, path+"-shm", "Kept 路径不得物化 WAL 索引文件")

	// 库头回读：journal_mode 仍是 delete——文件摘要之外的第二个独立观察面
	// （裸连接只读 PRAGMA，不写入）。
	bare, err := sql.Open(driverName, "file:"+filepath.ToSlash(path))
	require.NoError(t, err)
	var mode string
	require.NoError(t, bare.QueryRow("PRAGMA journal_mode").Scan(&mode))
	require.Equal(t, "delete", mode, "Kept 路径不得切换 journal_mode")
	require.NoError(t, bare.Close())
}

// makeDeleteModeDatabase 造一个 journal_mode 停留在默认 delete、schema_version
// 为指定值的库，**建完即关闭连接**——连接不关，WAL 切换会被"别的连接持有该库"
// 静默拒绝，夹具自己把本用例要抓的缺陷遮掉。
// The fixture closes its connection before returning: a connection left open
// would silently block the WAL switch and mask the very bug under test.
func makeDeleteModeDatabase(t *testing.T, path string, version int) {
	t.Helper()

	db, err := sql.Open(driverName, "file:"+filepath.ToSlash(path))
	require.NoError(t, err)

	_, err = db.Exec(`CREATE TABLE schema_version (` +
		`version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = db.Exec(
		`INSERT INTO schema_version (version, applied_at, description) VALUES (?, 0, 'fixture')`, version)
	require.NoError(t, err)

	var mode string
	require.NoError(t, db.QueryRow("PRAGMA journal_mode").Scan(&mode))
	require.Equal(t, "delete", mode, "夹具必须停留在 DELETE 模式——WAL 夹具会遮掉本缺陷")
	require.NoError(t, db.Close())
}

// sha256File 计算文件摘要——经 os.Root 读（与实现同一口径，避开裸
// os.ReadFile(变量) 在静态安全扫描里的判定）。
//
// 摘要取 sha256.Sum256(raw)：**不得**写成 sha256.New().Sum(raw)——那不是文件的
// SHA256，而是"空消息摘要"再拼接原文（两种写法都随字节变化而变化，但零写类
// 证据面要用真摘要，名实不符的 helper 会把证据本身变可疑）。
// The digest must be the real SHA-256 of the bytes: sha256.New().Sum(raw) appends
// the raw bytes to the digest of the empty message and is not a file digest.
func sha256File(t *testing.T, path string) string {
	t.Helper()

	root, err := os.OpenRoot(filepath.Dir(path))
	require.NoError(t, err)
	defer func() { require.NoError(t, root.Close()) }()

	raw, err := root.ReadFile(filepath.Base(path))
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// TestBootstrap_PathWithURIReservedCharacters：路径含 URI 保留字符（# / % / 空格）
// 时库里必须落在**原路径**上——dsn 不转义时 `#` 被当 fragment 起始，库被写到截断
// 后的兄弟路径（P2 回归：实测 init 报"已创建"、退出码 0，库实际落在父目录里名为
// `hash` 的文件上，随后 status 判"库不存在"）。
// Reserved characters must reach SQLite escaped, or the database silently lands
// beside the intended path.
func TestBootstrap_PathWithURIReservedCharacters(t *testing.T) {
	ctx := context.Background()

	cases := []struct{ name, dir string }{
		{"井号 #", "hash#workspace"},
		{"百分号 %", "pct%workspace"},
		{"空格", "space workspace"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			dir := filepath.Join(base, tc.dir)
			require.NoError(t, os.MkdirAll(dir, 0o750))
			path := filepath.Join(dir, "basalt.db")

			created, err := Bootstrap(ctx, path, SupportedSchemaVersion)
			require.NoError(t, err)
			require.True(t, created)
			require.FileExists(t, path, "库必须落在原路径上")

			// 父目录不得出现任何截断路径的残骸（缺陷形态：多出一个名为 hash 的文件）。
			entries, err := os.ReadDir(base)
			require.NoError(t, err)
			require.Len(t, entries, 1, "不得在截断路径上另建文件或目录")
			require.Equal(t, tc.dir, entries[0].Name())

			// 只读打开复核库真的可用（读池 DSN 走同一套编码）。
			db, err := OpenReadOnly(ctx, path, SupportedSchemaVersion)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })

			version, err := db.SchemaVersion(ctx)
			require.NoError(t, err)
			require.Equal(t, SupportedSchemaVersion, version)
		})
	}
}

// TestBootstrap_ConcurrentInitRace：并发引导的序列化必须由用例兜底——
// 语义：恰好一个调用实际创建（created=true），其余在写锁释放后重判态 → Kept
// （created=false）且不报错；终态一致（11 表 + version=target）。
// 缺「写权后重判」的实现在此判红：不用 IF NOT EXISTS 则竞争方报
// "table already exists"；只用 IF NOT EXISTS 而多个调用都报 created=true，
// 则 created 计数为 2。
func TestBootstrap_ConcurrentInitRace(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "basalt.db")

	const n = 4
	results := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() { // 各 goroutine 经各自连接竞争同一文件（SQLite 文件写锁真串行）
			defer wg.Done()
			results[i], errs[i] = Bootstrap(context.Background(), dbPath, SupportedSchemaVersion)
		}()
	}
	wg.Wait()

	created := 0
	for i := range n {
		require.NoError(t, errs[i]) // busy_timeout(5000) 兜底；超时应如实报错而非被吞
		if results[i] {
			created++
		}
	}
	require.Equal(t, 1, created) // 恰好一个实际创建者

	db, err := OpenReadOnly(context.Background(), dbPath, SupportedSchemaVersion)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	v, err := db.SchemaVersion(context.Background())
	require.NoError(t, err)
	require.Equal(t, SupportedSchemaVersion, v) // 终态：版本行就位

	names, err := db.TableNames(context.Background())
	require.NoError(t, err)
	require.Equal(t, contractTables, names) // 终态：11 表齐备
}

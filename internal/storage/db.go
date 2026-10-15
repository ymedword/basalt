package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ymedword/basalt/internal/core"
)

// DB 是打开后的数据库句柄：读写两条池指向同一个库文件，写池恒为一条连接
// （SQLite 单写者），读池若干。
// DB is an opened database handle: a read pool and a write pool over the same
// file. The write pool is capped at one connection (SQLite has a single writer).
//
// 只读打开只装配读池（write 为 nil）——只读句柄没有写路径可走，句柄本身就是
// 边界的载体，不靠调用方自觉。
// A read-only handle has no write pool at all: the boundary is carried by the
// handle rather than by caller discipline.
type DB struct {
	write *sql.DB
	read  *sql.DB
	path  string
}

// 编译期断言：*DB 满足 core.SchemaIntrospector（依赖倒置的首个样例——接口声明
// 在消费方 core，实现落在本包；cli 的 status 经该接口消费）。
// Compile-time assertion of the dependency-inversion sample.
var _ core.SchemaIntrospector = (*DB)(nil)

// OpenWritable 以写模式打开库文件（缺失则创建）。
// OpenWritable opens the database file for writing, creating it when missing.
//
// 与状态矩阵（颗粒度文档 §2.3）逐格对齐：
//   - 新库（无 schema_version 表）→ 全量建表 + 写 version=target，**同一事务**；
//   - 当前版本 → 空事务提交（Kept 的可写形态）；
//   - 旧版本 → 打开成功但**不迁移**——升级是持锁重命令的显式动作（Migrate）；
//   - 过高 / 损坏 → 拒绝（ErrSchemaTooNew / ErrSchemaCorrupt）。
//
// 判定不在打开之前、也不在打开之后，而是**取得写权之后**（BEGIN IMMEDIATE 内）：
// 先判态后写入会被并发初始化穿透（约束 16）。
// The state is re-judged under the write lock, never before it.
func OpenWritable(ctx context.Context, path string, target int) (*DB, error) {
	db, _, err := openWritable(ctx, path, target, false)
	return db, err
}

// OpenReadOnly 以只读模式打开库文件并做状态校验——status 用，只校验、不迁移。
// OpenReadOnly opens the file read-only and verifies the state; it never migrates.
//
// 三重只读保证：库文件缺失返回 ErrDatabaseMissing（不代建——建库是 init / 重路径
// 的事）、连接串带 mode=ro（不创建库文件、库内容零写——WAL 索引侧文件由 SQLite
// 物化，见 dsn 注释）、开放状态矩阵只放行"当前版本"，旧 / 过高 / 损坏各自点名拒绝。
// Missing file, outdated, too-new and corrupt states are each rejected with
// their own sentinel so the status report can name them.
func OpenReadOnly(ctx context.Context, path string, target int) (*DB, error) {
	if target < 1 {
		return nil, fmt.Errorf("storage: 目标版本 %d 非法——版本从 1 起", target)
	}

	exists, err := databaseExists(path)
	if err != nil {
		return nil, fmt.Errorf("检查库文件 %s: %w", path, err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: %s", core.ErrDatabaseMissing, path)
	}

	db := &DB{path: path}
	if err := db.openReadPool(ctx, true); err != nil {
		return nil, err
	}
	if err := db.verifyReadOnly(ctx, target); err != nil {
		// 校验不过即关闭：带错误的句柄不交给调用方；关闭失败并入错误链。
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// SchemaVersion 返回库内 schema_version 值；版本表存在但零行是损坏态。
// SchemaVersion returns the stored schema version; zero rows is corruption.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var current sql.NullInt64
	if err := db.read.QueryRowContext(ctx, "SELECT max(version) FROM schema_version").Scan(&current); err != nil {
		return 0, fmt.Errorf("读取 schema_version: %w", err)
	}
	if !current.Valid {
		return 0, core.ErrSchemaCorrupt
	}
	return int(current.Int64), nil
}

// TableNames 返回用户表表名（不含 sqlite_ 内部表），按名排序。
// TableNames returns the user table names, sorted, excluding internals.
func (db *DB) TableNames(ctx context.Context) ([]string, error) {
	rows, err := db.read.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("清点用户表: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("读取表名: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历表名: %w", err)
	}
	return names, nil
}

// Close 关闭读写两条池（只读句柄的写池为空，跳过）。
// Close shuts down both pools, skipping the absent write pool of a read-only
// handle.
func (db *DB) Close() error {
	var errs []error
	if db.write != nil {
		errs = append(errs, db.write.Close())
	}
	if db.read != nil {
		errs = append(errs, db.read.Close())
	}
	return errors.Join(errs...)
}

// openWritePool 装配写池：恒 1 条连接（SQLite 单写者），逐连接 PRAGMA 经 DSN
// 生效。装配即 Ping，路径非法 / 权限不足在这里即刻暴露。
// openWritePool wires the single-connection write pool (SQLite has a single
// writer); the ping surfaces bad paths at assembly time.
//
// **调用时机是顺序约束的一部分**：写模式下「判态 → WAL → 读池」的次序由
// openWritable 钉死（WAL 是库级持久属性，早于判态做会把该被拒绝的库改掉；
// 晚于读池做会被 SQLite 静默拒绝）——见那里的注释。
// The order in openWritable is a correctness constraint, not a style choice.
func (db *DB) openWritePool(ctx context.Context) error {
	pool, err := openPool(ctx, fmt.Sprintf("写池（%s）", db.path), dsn(db.path, false), 1)
	if err != nil {
		return err
	}
	db.write = pool
	return nil
}

// openReadPool 装配读池：若干条连接；只读打开额外带 mode=ro（经 dsn）。
// openReadPool wires the read pool, adding mode=ro for read-only opens.
func (db *DB) openReadPool(ctx context.Context, readOnly bool) error {
	pool, err := openPool(ctx, fmt.Sprintf("读池（%s）", db.path), dsn(db.path, readOnly), readPoolSize)
	if err != nil {
		return err
	}
	db.read = pool
	return nil
}

// openPool 打开一条连接池并真开一条物理连接（Ping）：路径非法 / 权限不足 /
// 只读侧文件不可用在这里即刻暴露，而不是等到第一次查询。
// openPool opens a pool and pings it so that bad paths and permissions surface
// at assembly time rather than at the first query.
func openPool(ctx context.Context, name, dsn string, maxConns int) (*sql.DB, error) {
	pool, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 %s: %w", name, err)
	}
	pool.SetMaxOpenConns(maxConns)
	if err := pool.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("连接 %s: %w", name, err), pool.Close())
	}
	return pool, nil
}

// verifyReadOnly 按状态矩阵校验只读打开的库；任何非"当前版本"都点名拒绝。
// verifyReadOnly enforces the read-only state matrix.
func (db *DB) verifyReadOnly(ctx context.Context, target int) error {
	state, current, err := inspectSchema(ctx, db.read, target)
	if err != nil {
		return err
	}
	switch state {
	case stateCurrent:
		return nil
	case stateNew:
		// 库文件在、schema_version 表不在 = 尚未 init（含空文件 / 残留库）。
		return fmt.Errorf("%w: %s（库文件存在但无 schema_version 表）", core.ErrDatabaseMissing, db.path)
	case stateOutdated:
		return fmt.Errorf("%w（库内 v%d < 目标 v%d）——需由持锁重命令升级",
			core.ErrSchemaOutdated, current, target)
	case stateTooNew:
		return fmt.Errorf("%w（库内 v%d > 目标 v%d）——请升级二进制",
			core.ErrSchemaTooNew, current, target)
	case stateCorrupt:
		return core.ErrSchemaCorrupt
	}
	return fmt.Errorf("storage: 未处理的 schema 状态 %d", state)
}

// schemaState 是库状态判定的五种结果（判定以 schema_version 表为准——契约迁移
// 纪律 2）；命令 × 状态 × 允许副作用见颗粒度文档 §2.3。
// schemaState enumerates the five outcomes of state judgement.
type schemaState int

const (
	// stateNew：无 schema_version 表（库文件缺失 / 空文件 / 残留）。
	stateNew schemaState = iota

	// stateCurrent：max(version) == target。
	stateCurrent

	// stateOutdated：max(version) < target。
	stateOutdated

	// stateTooNew：max(version) > target（旧二进制不得打开新库）。
	stateTooNew

	// stateCorrupt：表存在但零行（既非新库也非任何已知版本，fail-closed）。
	stateCorrupt
)

// queryer 是判态所需的最小数据库面——*sql.Conn 与 *sql.DB 都满足：写路径在
// 事务内判态（拿写权的那条连接），只读路径在读池上判态。
// queryer is the minimal surface state judgement needs.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// inspectSchema 判态并返回库内当前版本（新库 / 损坏态时版本无意义，返回 0）。
// inspectSchema judges the state and returns the current version.
func inspectSchema(ctx context.Context, q queryer, target int) (schemaState, int, error) {
	var tables int
	err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_version'`).Scan(&tables)
	if err != nil {
		return stateNew, 0, fmt.Errorf("查询 schema_version 表: %w", err)
	}
	if tables == 0 {
		return stateNew, 0, nil
	}

	var current sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT max(version) FROM schema_version`).Scan(&current); err != nil {
		return stateNew, 0, fmt.Errorf("读取 schema_version: %w", err)
	}
	if !current.Valid {
		return stateCorrupt, 0, nil
	}

	switch {
	case current.Int64 > int64(target):
		return stateTooNew, int(current.Int64), nil
	case current.Int64 < int64(target):
		return stateOutdated, int(current.Int64), nil
	default:
		return stateCurrent, int(current.Int64), nil
	}
}

// withWriteLock 在专用连接上取写权（BEGIN IMMEDIATE）后执行 fn，按 fn 的结果
// 提交或回滚。
// withWriteLock takes the write lock on a dedicated connection, runs fn, then
// commits or rolls back according to fn's outcome.
//
// 两点必须是同一连接：BEGIN IMMEDIATE 与后续语句——连接池会把语句派发到别的
// 物理连接，那样写权与事务都不成立；故先 Conn(ctx) 钉住一条连接，事务全程只
// 用它。写权（IMMEDIATE）而非普通 BEGIN（DEFERRED）：判定与写入都必须在事务
// 开始时就独占写者，否则"判态 → 写入"之间仍有并发窗口（约束 16）。
// The lock and every statement of the transaction share one pinned connection;
// IMMEDIATE (not DEFERRED) closes the window between judging and writing.
func withWriteLock(ctx context.Context, db *sql.DB, fn func(context.Context, *sql.Conn) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("获取写连接: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("取得写权（BEGIN IMMEDIATE）: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// 手工 BEGIN 对 database/sql 不可见（它只懂 BeginTx 开的 Tx），连接归还
		// 池前必须自己回滚；回滚失败无补救余地，但不得静默——并入返回错误链。
		if _, rollbackErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("回滚事务: %w", rollbackErr))
		}
	}()

	if fnErr := fn(ctx, conn); fnErr != nil {
		return fnErr
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	committed = true
	return nil
}

// databaseExists 判定库文件是否存在——检查范围经 os.Root 钉在库文件所在目录。
// databaseExists reports whether the database file exists, scoping the check
// through os.Root to the directory the file lives in.
//
// 与 cli.readConfigFile 同一口径：路径来自调用方参数，裸 os.Stat 在静态安全
// 扫描里是文件包含判定的近亲，而抑制手段默认禁用；以父目录为 root 的检查既
// 消除判定面，又把"跳到目录之外的软连接"挡在门外（fail-closed）。父目录不存在
// 等同于库文件不存在——正是 status 需要的判读（未 init）。
// A missing parent directory reads as a missing file, which is exactly what the
// status report needs.
func databaseExists(path string) (bool, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer root.Close()

	if _, err := root.Stat(filepath.Base(path)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

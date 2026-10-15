package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	// 驱动经空白导入注册：modernc.org/sqlite 以 "sqlite" 为驱动名注册到 database/sql
	// （纯 Go，无 CGO——单二进制纪律）。本包是唯一的驱动装配点。
	// The driver registers itself under "sqlite" via its blank import; this
	// package is the only place that wires it up.
	_ "modernc.org/sqlite"
)

// driverName 是 database/sql 使用的驱动名；与空白导入注册的名字一致。
// driverName is the database/sql driver name registered by the blank import.
const driverName = "sqlite"

// busyTimeoutMillis 是写锁等待上限（毫秒）：并发引导时竞争方在此窗口内自动重试，
// 超时如实报错而不是被吞（契约迁移纪律 6 的并发口径）。
// busyTimeoutMillis caps the wait for the write lock, in milliseconds.
const busyTimeoutMillis = 5000

// readPoolSize 是读池的物理连接上限——读池若干条，写池恒为 1。
// readPoolSize caps the physical connections of the read pool; the write pool
// is always one connection.
const readPoolSize = 4

// pragmaParams 是逐物理连接的 PRAGMA 查询参数（不含前导 ? / &）。
// pragmaParams renders the per-connection PRAGMA query parameters.
//
// 为什么走 DSN 而不是"打开后执行一次 PRAGMA"：foreign_keys / busy_timeout 都是
// **连接级**设置，启动时执行一次只影响当时那一条物理连接——池子里后续新开的
// 连接会带着默认值（foreign_keys 默认关）。连接级设置随 DSN 装配，每条新连接
// 自动生效（契约迁移纪律 6；TestOpenWritable_PragmasOnEveryConnection 反此坑）。
// Connection-scoped PRAGMAs must travel in the DSN, or later connections in the
// pool silently run with defaults (foreign keys off).
func pragmaParams() string {
	return "_pragma=busy_timeout(" + strconv.Itoa(busyTimeoutMillis) + ")&_pragma=foreign_keys(1)"
}

// dsn 装配连接串：读写在同一个库文件上，只读打开额外带 mode=ro。
// dsn assembles the connection string; read-only opening adds mode=ro.
//
// mode=ro 的意义：只读打开不会创建**库文件**（status 不得代建），库内容也一字
// 不改——写语句在库层面被拒（TestOpenReadOnly_ReadsCurrent 反此坑）。
// mode=ro keeps the connection from creating or modifying the database file
// itself.
//
// 但它**拦不住** WAL 索引文件：SQLite 读一个 WAL 库时要在同目录物化 -shm 与
// -wal（实测：-shm 32KB / -wal 0B），只读连接同样如此，且与 _pragma 参数无关
// （裸 mode=ro 连接一样命中）。所以"零写"指的是**不改库**，不是目录级零字节；
// 契约 §4.3.1 禁碰的"锁文件"是 `.basalt/basalt.lock`，与这两个 SQLite 侧文件
// 无关。
// The WAL index files are materialized by SQLite on any read of a WAL database,
// read-only connections included: zero-write means the database is untouched,
// not that the directory gains no bytes.
func dsn(path string, readOnly bool) string {
	params := pragmaParams()
	if readOnly {
		params = "mode=ro&" + params
	}
	return "file:" + encodeURIPath(path) + "?" + params
}

// encodeURIPath 转义路径里对 SQLite 的 URI 解析有特殊含义的字符。
// encodeURIPath escapes the characters special to SQLite's URI parser.
//
// `#` 起 fragment、`?` 起 query——不转义时库文件会被解析到**截断后的路径**上
// （实测：`hash#workspace/` 下 init 报"已创建"、退出码 0，库实际落在兄弟路径
// `hash`，随后 status 判"库不存在"）；裸 `%` 会被当作百分之转义序列的开头。
// SQLite 把这些序列解码回原字符（实测：`%23` / `%25` 的落点都是原路径），
// 所以按 `%` 先、`#` / `?` 后的次序转义即可（NewReplacer 单遍扫描，替换产物
// 不会被二次转义）。空格不需要转义——URI 路径里的空格按字面处理（实测通过）。
// An unescaped `#` silently truncates the path: the database lands beside the
// intended one. `%` is escaped first so real percent signs survive.
func encodeURIPath(path string) string {
	return strings.NewReplacer("%", "%25", "#", "%23", "?", "%3F").Replace(path)
}

// walStatement 是库级持久属性 journal_mode 的设置语句——执行一次即写入库头，
// 此后重开仍是 WAL。必须在事务**外**执行（事务内该 PRAGMA 是空操作）。
// walStatement sets the persistent, database-scoped journal mode. It must run
// outside a transaction; inside one it is silently a no-op.
const walStatement = "PRAGMA journal_mode=WAL"

// journalModeWAL 是 journal_mode 的回读期望值（小写——SQLite 回读的形态）。
// journalModeWAL is the expected read-back value of journal_mode.
const journalModeWAL = "wal"

// walRetryDelay 是 WAL 切换失败后的重试间隔——并发打开同一新库时，切换会撞上
// 创建者的写锁，SQLITE_BUSY 立即返回、不等 busy_timeout 窗口。
// walRetryDelay is the pause between WAL switch attempts under contention.
//
// 口径收窄（本场景观察，不得推广）：SQLITE_BUSY 不等 busy_timeout **只覆盖
// journal_mode 的切换**（此处与 walRetryDelay 所服务的循环）；一般锁竞争由
// busy_timeout 兜底，"所有 SQLITE_BUSY 都立即返回"是错误概括。
// The observation is scoped to the journal_mode switch only — general lock
// contention does honour busy_timeout.
const walRetryDelay = 25 * time.Millisecond

// applyWAL 把库切到 WAL 并**回读判卷**，在 busy_timeout 窗口内重试。
// applyWAL switches the database to WAL, verifies the read-back and retries
// within the busy window.
//
// 两处必须小心（都踩过）：
//  1. 切不动时 SQLite **不报错**，只把当前模式返回——不回读就会静默留在 DELETE
//     模式；触发条件很隐蔽：库里已有**别的连接**打开着（实测：先开读池再设 WAL，
//     回读仍是 delete，切换被静默拒绝）。故 openWritable 里 WAL 必须先于读池
//     装配（且晚于判态——拒绝路径不得改库），而这里用回读把该隐式前提变成显式判据。
//  2. 并发打开同一新库时切换可能拿到 SQLITE_BUSY（创建者正持写锁），在
//     busy_timeout 窗口内重试；窗口用尽如实报错，不静默降级。
func applyWAL(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(time.Duration(busyTimeoutMillis) * time.Millisecond)
	ticker := time.NewTicker(walRetryDelay)
	defer ticker.Stop()

	for {
		mode, err := setJournalModeWAL(ctx, db)
		if err == nil && mode == journalModeWAL {
			return nil
		}
		if err == nil {
			// 切不动不报错——把"回读了别的模式"翻译成错误，与真错误统一处置。
			err = fmt.Errorf("journal_mode 回读为 %q", mode)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("设置 WAL 失败（%s）: %w", walStatement, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// setJournalModeWAL 执行一次切换并回读结果模式。
// setJournalModeWAL performs one switch and reads back the resulting mode.
func setJournalModeWAL(ctx context.Context, db *sql.DB) (string, error) {
	var mode string
	if err := db.QueryRowContext(ctx, walStatement).Scan(&mode); err != nil {
		return "", err
	}
	return mode, nil
}

// 并发安全不靠 IF NOT EXISTS：建表脚本是契约的逐字节副本（不在实现侧改写），
// 重复初始化由「取写权（BEGIN IMMEDIATE）后重判态」序列化（约束 16）。
// Concurrency safety comes from re-judging the state under the write lock, never
// from IF NOT EXISTS: the schema script is a byte-exact contract copy.

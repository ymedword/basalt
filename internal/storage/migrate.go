package storage

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// SupportedSchemaVersion 是本二进制支持的目标 schema 版本，也是首个版本记录
// 锚点：生产装配把它作为 target 传给 OpenWritable / Bootstrap / OpenReadOnly /
// Migrate；测试传合成值以覆盖单版本期不可达的分支（旧版本 / 损坏态）。
// v3 = US-12 范围修订：资源 UID 身份与墓碑、使用范围、托管凭据、绑定投影、
// 通知 Run 引用与发送明细（含 resource_versions / notify_channels 重建）。
// v4 = US-12 R7：定时任务系统代投（deliver_to / schedule_deliveries）与资源连接
// 测试最后结果（resource_connection_tests）。
// v5 = US-12 R8：网络域名白名单的唯一权威源（network_domain_policy 单行表）。
// v6 = US-14 14D：执行档位（execution_policy）、访问例外（network_exceptions）、实例
// 密钥初始化记录（instance_secrets）、MCP 每键凭据来源（mcp_credential_sources），
// 并**重建** credentials（owner_kind 加 mcp_server、新增 binding_key、唯一键改三元组）。
// SupportedSchemaVersion is the schema version this binary targets; it is the
// first version-record anchor and the production value of the target parameter
// threaded through OpenWritable, Bootstrap, OpenReadOnly and Migrate.
const SupportedSchemaVersion = 6

// schemaFileName 是建表脚本在 fs.FS 里的路径——契约文件的嵌入副本。
// schemaFileName is the path of the schema script inside the injected fs.FS.
const schemaFileName = "schema.sql"

// migrationsDir 是增量迁移文件所在目录；文件命名 = <4 位零填充版本>_<描述>.sql
// （如 0002_add_b.sql），一个版本恰好一个文件，缺文件即拒绝、不跳过版本。
// Migration files are named <zero-padded version>_<slug>.sql, exactly one per
// version; a missing file is a hard refusal, never a skipped version.
const migrationsDir = "migrations"

// schemaFS 是随二进制嵌入的建表脚本与迁移目录。
// schemaFS is the embedded schema script plus the migrations directory.
//
// 嵌入清单里列 migrations 目录本身（US-11 起含 0002 增量文件；此前只有 .gitkeep
// 占位）。前缀 all: 是必需的——go:embed 默认忽略以 . / _ 开头的文件，不加前缀时
// "目录里没有可嵌入文件"会直接编译报错（实测：pattern migrations: cannot embed
// directory migrations: contains no embeddable files）。有了这一行，后续 US 把
// 0003_*.sql 放进目录即被发现，无需再改本声明。
// The all: prefix is required: the placeholder is a dotfile, which plain
// embedding skips — and a directory with no embeddable file fails to compile.
//
//go:embed schema.sql all:migrations
var schemaFS embed.FS

// Migrate 把库受控地前向迁移到 target 版本——机制层，迁移是**显式动作**。
// Migrate advances the database to the target version under its own write lock.
//
// 唯一的生产调用方是持锁重路径（US-4 起）：本函数自己不取实例锁，但整段迁移
// 在一个 BEGIN IMMEDIATE 事务内完成——判定、逐版执行、写入版本记录同一事务，
// 中途任何失败（含缺迁移文件）整体回滚，库回到运行前状态；重跑按原状态重试。
// The whole run is one IMMEDIATE transaction: judgement, every incremental
// script and every version row commit together or not at all.
//
// 逐版语义：v < target 时从 v+1 起逐版执行「迁移文件 + 版本记录」。颗粒度文档
// 写作「每版同一事务」，本实现取更强的一档（整个迁移趟次一个事务）——两句在
// 「版本记录必与对应 SQL 同事务」上一致，而"中途失败库回到运行前状态"只有更强
// 的一档才成立（§2.2 约束 16）。
// Per-version scripts run in order; the stronger all-or-nothing reading of
// constraint 16 is implemented here.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS, target int) error {
	if db == nil {
		return errors.New("storage: Migrate 需要非空的 *sql.DB")
	}
	if fsys == nil {
		return errors.New("storage: Migrate 需要非空的 fs.FS")
	}
	if target < 1 {
		return fmt.Errorf("storage: 目标版本 %d 非法——版本从 1 起", target)
	}

	return withWriteLockOptions(ctx, db, writeLockOptions{
		// 表重建（create new → copy → drop → rename）只允许在未发布迁移里，且必须在
		// 外键关闭时进行：resource_versions / notify_channels 被其它表引用，DROP 在
		// 外键开启时会因引用行失败（迁移纪律 8）。
		foreignKeysOff: true,
		preCommit:      checkForeignKeys,
	}, func(ctx context.Context, conn *sql.Conn) error {
		state, current, err := inspectSchema(ctx, conn, target)
		if err != nil {
			return err
		}
		switch state {
		case stateNew:
			return createNewDatabase(ctx, conn, fsys, target)
		case stateCurrent:
			return nil // 无操作
		case stateOutdated:
			return applyIncremental(ctx, conn, fsys, current, target)
		case stateTooNew:
			return fmt.Errorf("%w（库内 v%d > 目标 v%d）——请升级二进制",
				core.ErrSchemaTooNew, current, target)
		case stateCorrupt:
			return core.ErrSchemaCorrupt
		}
		// 五种状态在上方穷举——保留兜底，防新增状态漏改（不可达）。
		return fmt.Errorf("storage: 未处理的 schema 状态 %d", state)
	})
}

// checkForeignKeys 在提交前跑 PRAGMA foreign_key_check：任何违例都让整个迁移
// 回滚（迁移纪律 8 —— 重建后的外键必须逐条复核通过才提交）。
// checkForeignKeys runs PRAGMA foreign_key_check before COMMIT; any violation
// aborts the transaction.
func checkForeignKeys(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("外键校验: %w", err)
	}
	defer func() { _ = rows.Close() }()

	const maxReported = 5
	var violations []string
	for rows.Next() {
		var (
			table  string
			rowid  sql.NullInt64
			parent string
			fkid   int64
		)
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("外键校验结果: %w", err)
		}
		violations = append(violations,
			fmt.Sprintf("%s(rowid=%d)→%s(fk#%d)", table, rowid.Int64, parent, fkid))
		if len(violations) >= maxReported {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("外键校验: %w", err)
	}
	if len(violations) > 0 {
		return fmt.Errorf("迁移后外键校验失败（%s）——事务已回滚，请按备份恢复", strings.Join(violations, ", "))
	}
	return nil
}

// Migrate 是机制层 Migrate 的 *DB 形态（**装配面**）：持锁重路径（US-4 起）手里只有
// 从 OpenWritable 拿到的句柄，写池 `DB.write` 与嵌入源 `schemaFS` 都不出包。
// DB.Migrate is the assembly-facing form of the machinery function; the write pool and
// the embedded schema source stay inside this package.
//
// 只读句柄（write 为 nil）在这里**显式拒绝**，而不是让它崩在一句 nil 解引用上。
// A read-only handle is refused explicitly instead of panicking on a nil pool.
func (db *DB) Migrate(ctx context.Context, target int) error {
	if db == nil || db.write == nil {
		return errors.New("storage: 只读句柄没有写池，不得迁移（先经 OpenWritable 取得写模式句柄）")
	}
	return Migrate(ctx, db.write, schemaFS, target)
}

// createNewDatabase 走新库分支：建表脚本全量执行 + 写入版本行。
// createNewDatabase runs the full schema script and records the version row.
//
// 两步都在调用方的事务内（同一 BEGIN IMMEDIATE），任一步失败由 withWriteLock
// 整体回滚——不留"有表无版本行"或"只有半张表"的中间态（约束 16）。
// Both steps share the caller's transaction, so a failure leaves no partial
// schema behind.
//
// 建表脚本经**整脚本一次 Exec** 下发给驱动（多语句执行成立，2026-10-15 实测）：
// 不在实现侧切分 SQL——切分器是第二份 SQL 解析实现，而契约脚本只有一份。
// The script is handed to the driver in one Exec; splitting it would be a second
// SQL parser, and there is only one schema script.
func createNewDatabase(ctx context.Context, conn *sql.Conn, fsys fs.FS, target int) error {
	script, err := fs.ReadFile(fsys, schemaFileName)
	if err != nil {
		return fmt.Errorf("读取建表脚本 %s: %w", schemaFileName, err)
	}
	if _, err := conn.ExecContext(ctx, string(script)); err != nil {
		return fmt.Errorf("执行建表脚本 %s: %w", schemaFileName, err)
	}
	if err := recordVersion(ctx, conn, target, schemaFileName); err != nil {
		return err
	}
	return nil
}

// applyIncremental 从 from+1 逐版前进到 target：每版「迁移文件 + 版本记录」，
// 全程在调用方事务内（见 Migrate 的事务口径）。
// applyIncremental walks the versions one by one inside the caller's transaction.
func applyIncremental(ctx context.Context, conn *sql.Conn, fsys fs.FS, from, target int) error {
	for version := from + 1; version <= target; version++ {
		name, err := migrationFile(fsys, version)
		if err != nil {
			return err
		}
		script, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("读取迁移文件 %s: %w", name, err)
		}
		if _, err := conn.ExecContext(ctx, string(script)); err != nil {
			return fmt.Errorf("执行迁移 %s（v%d）: %w", name, version, err)
		}
		if err := recordVersion(ctx, conn, version, name); err != nil {
			return err
		}
	}
	return nil
}

// migrationFile 定位某一版的迁移文件：migrations/<4 位版本>_*.sql 恰好一个。
// migrationFile locates the single migration file of a version.
//
// 零命中与多命中都是拒绝而不是猜测：缺文件时点名期望的路径（不跳过版本、不
// 静默），多文件时说明该版本歧义——迁移集是契约性资产，宁停不错。
// Zero or several matches is a refusal, never a guess.
func migrationFile(fsys fs.FS, version int) (string, error) {
	pattern := fmt.Sprintf("%s/%04d_*.sql", migrationsDir, version)
	matches, err := fs.Glob(fsys, pattern)
	if err != nil {
		return "", fmt.Errorf("查找迁移文件 %s: %w", pattern, err)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("缺少版本 v%d 的迁移文件（期望 %s）——不得跳过版本", version, pattern)
	default:
		return "", fmt.Errorf("版本 v%d 的迁移文件不唯一（%s）——一个版本只允许一个文件", version, pattern)
	}
}

// recordVersion 写入一行版本记录；applied_at 用 UTC 毫秒（时区纪律：不依赖
// 进程时区，见 EngineeringStandards §1.6）。
// recordVersion writes one version row; applied_at is UTC milliseconds.
func recordVersion(ctx context.Context, conn *sql.Conn, version int, description string) error {
	_, err := conn.ExecContext(ctx,
		`INSERT INTO schema_version (version, applied_at, description) VALUES (?, ?, ?)`,
		version, time.Now().UnixMilli(), description)
	if err != nil {
		return fmt.Errorf("写入版本记录 v%d（%s）: %w", version, description, err)
	}
	return nil
}

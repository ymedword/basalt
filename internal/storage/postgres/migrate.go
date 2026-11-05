package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 PostgreSQL 谱系的**受控前向迁移**（设计 §3.1.1 / C-15 / 契约头部「迁移纪律」）：
// 单事务 + `pg_advisory_xact_lock` 串行，已发布迁移只追加不改写，失败整体回滚、不自动修补。
// 锁键按**目标 schema** 派生：同一 schema 严格互斥，不同 schema 可并行。
// This file is the PostgreSQL lineage's controlled forward migration: one transaction,
// serialized by a per-schema advisory lock, append-only scripts, all-or-nothing failure.

// migrationsDir 是内嵌迁移目录；文件名 = `<4 位零填充版本>_<描述>.sql`，一个版本恰好一个
// 文件——缺文件即拒绝、不跳过版本（与 SQLite 谱系同一命名纪律）。
// Migration files are named <zero-padded version>_<slug>.sql, exactly one per version;
// a missing file is a hard refusal, never a skipped version.
const migrationsDir = "migrations"

// migrationLockSeed 是迁移 advisory lock 的种子的前半段。完整种子 = 本常量 + "/" + 目标
// schema——键**按 schema 派生**而不是全库一把固定键：同一 schema 的两个迁移进程仍然互斥，
// 不同 schema 可以并行。全库固定键会把每个隔离 schema 的迁移串成一条队列（2026-11-05
// 实测：8 个 fixture 并发只比串行快 1.59×，全仓上千个测试 fixture 因此排队）。
// 前缀与压缩链的 `basalt/compaction-append` 不同——两类锁不得撞键。
//
// **兼容边界**：旧二进制取的是固定键 `advisoryKey(migrationLockSeed)`，与新版按 schema
// 派生的键**不互斥**——混合版本并发迁移同一库不在保护范围内。升级必须停旧启新，**不得**
// 让两个版本同时连同一库迁移（单二进制、单实例的既有部署口径）。同一版本内，同一 schema
// 严格互斥；不同 schema 互不影响。已发布迁移脚本不因本改动改写。
// The key is seed + "/" + target schema: the same schema stays mutually exclusive while
// different schemas migrate in parallel. Mixed-version concurrency is NOT covered (the old
// binary holds the old fixed key): stop the old instance before starting the new one.
const migrationLockSeed = "basalt/schema-migrate"

// migrationAdvisoryKeyFor 按目标 schema 派生迁移 advisory 锁键（int64）。
// migrationAdvisoryKeyFor derives the migration advisory key for one target schema.
func migrationAdvisoryKeyFor(schema string) int64 {
	return advisoryKey(migrationLockSeed + "/" + schema)
}

// advisoryKey 把种子串派生为 int64：sha256 前 8 字节按大端读取。与设计 §3.1.11 的
// `scopeAdvisoryKey` 同一派生方式，键只用于 `pg_advisory_xact_lock`，**不落库**。
//
// 分两段 32 位拼接而不是 `int64(binary.BigEndian.Uint64(...))`：后者是 uint64→int64 的
// 溢出转换（gosec G115，门禁零命中），而这里想要的正是"任意 8 字节的二进制解释"——
// 拼接得到的位型与整体转换完全一致，同时不用抑制注释。
// Two 32-bit halves keep the exact bit pattern without a wrapping uint64->int64
// conversion (gosec G115) and without a suppression comment.
func advisoryKey(seed string) int64 {
	sum := sha256.Sum256([]byte(seed))
	hi := int64(binary.BigEndian.Uint32(sum[:4]))
	lo := int64(binary.BigEndian.Uint32(sum[4:8]))
	return hi<<32 | lo
}

// migrationFS 是随二进制嵌入的迁移脚本目录。`all:` 前缀让以 `.` / `_` 开头的占位文件
// 也被收进来——go:embed 默认忽略点文件，空目录会直接编译报错（SQLite 侧同因加了该前缀）。
// migrationFS holds the embedded migration scripts; the all: prefix keeps placeholder
// files and stops go:embed from failing on a directory with no embeddable file.
//
//go:embed all:migrations
var migrationFS embed.FS

// migrationTargetSchema 解析迁移的**目标 schema**，并拒绝 search_path 回退造成的跨 schema
// 错位：版本表若经 search_path 解析到别的 schema，迁移会在本 schema 建表、却按别处的版本
// 记录判"已是最新"直接 no-op（或反之）。解析结果同时用于派生 advisory 锁键——**锁与 DDL
// 必须落在同一个 schema 上**，这是"同 schema 互斥 / 不同 schema 并行"成立的前提。
//
// 拒绝条件：① `current_schema()` 为 NULL（search_path 里没有可用 schema）；② `schema_version`
// 已存在但不属于该 schema。空 schema（首次迁移）没有版本表，只做第一项判定。
// migrationTargetSchema resolves the schema migrations will write into and refuses
// search_path fallback: a version table resolved from another schema would make the
// migration compare against someone else's version. The same value keys the advisory lock.
func migrationTargetSchema(ctx context.Context, q Queryer) (string, error) {
	var schema sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return "", fmt.Errorf("解析迁移目标 schema: %w", err)
	}
	if !schema.Valid || schema.String == "" {
		return "", errors.New("postgres: search_path 没有可用的 schema——无法确定迁移目标，拒绝执行")
	}
	var owner sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT n.nspname
		  FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.oid = to_regclass('schema_version')`).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("核对 schema_version 归属: %w", err)
	}
	if owner.Valid && owner.String != schema.String {
		return "", fmt.Errorf("postgres: schema_version 解析到 %q，迁移目标却是 %q——search_path 回退会让迁移跨 schema，拒绝执行（目标 schema 必须是 search_path 的第一项）",
			owner.String, schema.String)
	}
	return schema.String, nil
}

// ensureSchema 把库带到 target 版本（设计 §3.1.1 的签名逐字）：空库全量建 v1；版本表缺失
// 但有未知表则拒绝；低版本前向迁移；高版本拒绝。事务外先预检（尽早报错），随后在**同一
// 事务内**取 advisory 锁并**重新断言**——并发安全判据只在锁内成立。
// ensureSchema brings the database to the target version. The pre-check outside the
// transaction only fails fast; the authoritative assertions run again inside the lock.
func ensureSchema(ctx context.Context, q Queryer, target int) error {
	if err := verifySchemaVersion(ctx, q, target); err != nil {
		return err
	}
	beginner, ok := q.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return errors.New("postgres: ensureSchema 需要能开启事务的查询方（*sql.DB / *sql.Conn）")
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启迁移事务: %w", err)
	}
	// 提交后的 Rollback 返回 sql.ErrTxDone；只做释放，不覆盖已返回的结论。
	defer func() { _ = tx.Rollback() }()

	// 事务内先解析**目标 schema**（同一会话、同一事务，解析结果与随后 DDL 落在同一
	// schema 上），再按它取 advisory 事务锁：同一 schema 的并发启动在此等待（或按会话
	// `lock_timeout` 失败），不同 schema 各持一把键、互不排队。**不得**两进程在同一
	// schema 上同时建表（C-15）。
	// Resolve the target schema in this same transaction, then take that schema's advisory
	// lock: same-schema starters wait here; different schemas never queue behind each other.
	schema, err := migrationTargetSchema(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationAdvisoryKeyFor(schema)); err != nil {
		return fmt.Errorf("获取迁移 advisory 锁: %w", err)
	}

	// 锁内重新断言：锁外的预检可能已过时（另一个进程刚建完库 / 刚迁完版本）。
	// Re-assert inside the lock: the outside pre-check may be stale.
	if err := verifySchemaVersion(ctx, tx, target); err != nil {
		return err
	}
	current, err := schemaVersion(ctx, tx)
	if errors.Is(err, core.ErrDatabaseMissing) {
		current = 0 // 空库：从 v1 全量建起
	} else if err != nil {
		return err
	}
	if current >= target {
		// 并发进程已把库带到目标版本；本事务只补种子行并释放锁。
		// The concurrent peer already migrated; this transaction only seeds.
		if err := seedBuiltinCaller(ctx, tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	for version := current + 1; version <= target; version++ {
		script, description, err := migrationScript(version)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, script); err != nil {
			return fmt.Errorf("应用迁移 %s: %w", description, err)
		}
		// 版本记录与对应 SQL 同事务：中途失败整体回滚，库回到迁移前状态。
		// The version row commits with its script or not at all.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO schema_version (version, applied_at, description)
			VALUES ($1, $2, $3)
			ON CONFLICT (version) DO NOTHING`,
			version, time.Now().UTC().UnixMilli(), description); err != nil {
			return fmt.Errorf("记录迁移 %s: %w", description, err)
		}
	}
	if err := seedBuiltinCaller(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Migrate 把库带到当前支持版本（幂等）：`Open` 在连接与扩展校验之后经它建库 / 前向迁移，
// `basalt db migrate` 也落在同一条路径上——**不得**出现第二套迁移入口（设计 §3.1.1 的
// `DB` 行点名了本方法；T124 冻结其公开形态为无版本参数，目标版本只取
// `SupportedSchemaVersion`，命令面因此无从要求"迁到某个别的版本"）。
//
// 前置：连接可用（`Open` 已 Ping）。本方法只做版本判定与迁移；失败整体回滚、不重试、
// 不修补——重试成循环与自动修补都是"看起来能起来"的假象。
// Migrate brings the schema to the supported version. Open and the CLI funnel through
// this single entry point, so no caller can pick a different target version.
func (db *DB) Migrate(ctx context.Context) error {
	if db == nil || db.pool == nil {
		return errors.New("postgres: 句柄未装配连接池，无法迁移")
	}
	return ensureSchema(ctx, db.pool, SupportedSchemaVersion)
}

// seedBuiltinCaller 补齐内置 `local-admin` 调用方行（契约 `schema.pg.sql` 头部与设计
// §3.4.2）：`sessions` / `runs` / `scheduled_tasks` 的 `caller_id` 外键指向 `callers`，
// 缺行时内置管理员的每一次运行都会在写入层被外键拒绝——这是建库的一部分，不是业务数据。
//
// **只补缺失行，不覆盖既有行**：`status='disabled'` 是管理决定，不得被每次启动
// 悄悄复活（契约写的是 upsert，语义取"保证存在"这一半）。
// seedBuiltinCaller guarantees the built-in local-admin caller row exists; an existing
// row is never overwritten because disabling it is an administrative decision.
func seedBuiltinCaller(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'builtin', $1, '', 'active', $2)
		ON CONFLICT (caller_id) DO NOTHING`,
		core.LocalAdminCallerID, time.Now().UTC().UnixMilli()); err != nil {
		return fmt.Errorf("补齐内置调用方行（caller_id=%s）: %w", core.LocalAdminCallerID, err)
	}
	return nil
}

// migrationScript 定位并读取某一版的迁移脚本：`migrations/<4 位版本>_*.sql` 恰好一个。
// 缺文件或同名重复都返回错误——版本只追加、不得跳版，也不得一次给出版本两个脚本。
// migrationScript finds exactly one script per version; missing or duplicated files are
// both hard errors.
func migrationScript(version int) (script, description string, err error) {
	if version < 1 {
		return "", "", fmt.Errorf("postgres: 迁移版本 %d 非法——版本从 1 起", version)
	}
	entries, err := fs.ReadDir(migrationFS, migrationsDir)
	if err != nil {
		return "", "", fmt.Errorf("读取内嵌迁移目录: %w", err)
	}
	prefix := fmt.Sprintf("%04d_", version)
	var matches []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") || !strings.HasPrefix(name, prefix) {
			continue
		}
		matches = append(matches, name)
	}
	switch len(matches) {
	case 1:
	case 0:
		return "", "", fmt.Errorf("缺少 v%d 迁移文件（%s/%s*.sql）——版本只追加、不得跳版", version, migrationsDir, prefix)
	default:
		return "", "", fmt.Errorf("v%d 迁移文件不唯一（%s）——一个版本恰好一个文件", version, strings.Join(matches, ", "))
	}
	description = path.Join(migrationsDir, matches[0])
	content, err := migrationFS.ReadFile(description)
	if err != nil {
		return "", "", fmt.Errorf("读取迁移 %s: %w", description, err)
	}
	return string(content), description, nil
}

//go:build integration

// Package pgtest 为集成用例提供**一次性**的 PostgreSQL 句柄（设计 §3.1.1 `pgtest` 行 /
// §4.4 片段约定）：每次调用建一个随机命名的 schema，把会话 `search_path` 钉在它（加上
// 扩展所在 schema）上，`postgres.Open` 的建库与迁移因此全部落在隔离 schema 里；用例结束
// 由 `t.Cleanup` 关闭句柄并 `DROP SCHEMA … CASCADE`。独立子包才能被其他包的测试导入，
// 而不把 `testing` 拖进任何生产包。
//
// 建 / 删 schema 与"不得指向既有实例库"的守卫在 `pgtemp` 包（评测运行器共用同一份）；
// 本包只负责把它们包成 `*testing.T` 形态（失败即 `t.Fatalf`、回收挂 `t.Cleanup`）。
// Package pgtest hands out throwaway PostgreSQL handles. Schema provisioning and the
// instance-database guard live in pgtemp so the evals runner shares the very same code;
// this package only wraps them in *testing.T form.
package pgtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtemp"
)

const (
	// schemaPrefix 让并行用例的 schema 名互不冲突，也便于人工核对残留。
	// schemaPrefix keeps parallel schemas distinct and any leftovers recognisable.
	schemaPrefix = "basalt_pgtest_"
)

// Fixture 是一次性测试库的两个面：`DSN` 供实例配置注入（让被测装配打开**同一个**
// 隔离 schema），`DB` 供夹具自己读写。两者共享同一 schema，随 `t.Cleanup` 一起回收。
// Fixture is one throwaway database seen two ways: DSN for the configuration under
// test, DB for the fixture's own reads and writes. Both share one schema and are
// reclaimed together in t.Cleanup.
type Fixture struct {
	// DSN 已把会话 `search_path` 钉在隔离 schema 上——它**含测试库口令**，只能经
	// 内存配置或进程环境传递，**不得**写进任何文件。
	// The DSN pins search_path to the isolated schema and carries the test password:
	// hand it over in memory or via the environment, never through a file.
	DSN string

	// DB 是与 DSN 同一 schema 的已迁移句柄。
	// DB is the migrated handle on the same schema as DSN.
	DB *postgres.DB
}

// NewTestDB 建一个隔离 schema 并返回已迁移的句柄。调用方**必须**在测试用例内使用；
// 句柄与 schema 都随 `t.Cleanup` 回收。支持 `t.Parallel()`：schema 名随机、彼此不可见。
// NewTestDB creates an isolated schema and returns a migrated handle; both are
// reclaimed in t.Cleanup and parallel subtests never see each other's rows.
func NewTestDB(t *testing.T) *postgres.DB {
	t.Helper()
	return NewFixture(t).DB
}

// NewTestSchema 只建隔离 schema 并返回钉住 search_path 的 DSN——**不**打开句柄、**不**
// 迁移建表。给"把 DSN 交给被测装配、由装配自己决定建不建库"的用例用（如验证轻路径
// 不写库的断言：schema 存在但表集合为空）。schema 随 `t.Cleanup` 回收。
// NewTestSchema creates only the isolated schema and returns its pinned DSN, without
// opening a handle or migrating: the assembly under test owns that decision.
func NewTestSchema(t *testing.T) string {
	t.Helper()
	return newPinnedDSN(t)
}

// NewFixture 建一个隔离 schema，返回它的 DSN 与已迁移句柄（同一 schema）。
// NewFixture creates one isolated schema and returns its DSN and migrated handle.
func NewFixture(t *testing.T) Fixture {
	t.Helper()
	pinned := newPinnedDSN(t)
	db, err := openMigrated(t.Context(), pinned)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭隔离句柄: %v", err)
		}
	})
	return Fixture{DSN: pinned, DB: db}
}

// OpenMigrated 在给定 DSN 上打开一个**已迁移**句柄，准备过程同样受全局夹具槽位约束。
// 给"先 NewTestSchema 拿 DSN、再自己 postgres.Open"的用例用：直接调 postgres.Open 会让
// 全量迁移绕过槽位，几十个 `t.Parallel()` 用例并联时一样打爆测试库锁表
// （2026-11-05 实测：cover 轮 20 条用例因此报 SQLSTATE 53200）。
// OpenMigrated opens a migrated handle under the shared slot, for cases that
// provision their own schema instead of using NewFixture.
func OpenMigrated(t *testing.T, dsn string) *postgres.DB {
	t.Helper()
	db, err := openMigrated(t.Context(), dsn)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return db
}

// openMigrated 在迁移槽位内跑 postgres.Open（内部会执行全量迁移）。迁移在一个事务里
// 持有约 1,136 个锁表条目，因此必须排队（见 pgtemp 的闸门说明）。
// openMigrated runs postgres.Open (which migrates) inside the migration slot.
func openMigrated(ctx context.Context, dsn string) (*postgres.DB, error) {
	release, err := pgtemp.AcquireMigrationSlot(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取夹具迁移槽位: %w", err)
	}
	defer release()
	db, err := postgres.Open(ctx, postgres.Config{
		DSN:            dsn,
		ConnectTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("在隔离 schema 上打开句柄: %w", err)
	}
	return db, nil
}

// newPinnedDSN 建隔离 schema 并把回收挂到 `t.Cleanup`；准备失败一律 `t.Fatalf`——
// 集成用例没有"没有数据库也能跑"的退路（D-01）。
// newPinnedDSN provisions the schema and registers its cleanup; any preparation
// failure is fatal because integration cases have no database-less fallback.
func newPinnedDSN(t *testing.T) string {
	t.Helper()
	schema, err := pgtemp.New(t.Context(), schemaPrefix)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(func() {
		// t.Context() 在 cleanup 前已取消，回收一律用独立上下文。
		// t.Context() is already canceled here, so cleanup uses its own context.
		// 回收同样按对象计锁：`DROP SCHEMA ... CASCADE` 在删每个表 / 索引时持锁（实测
		// 1,597 条），并发回收与并发迁移叠加会一起触发 53200（本次实测里就有 16 条回收
		// 因此失败）。因此回收走自己的闸门（1 路），与迁移闸门分开。
		// 先排队取槽位、再起 10s 的 DROP 时限——否则排队时间会算进 DROP 预算，
		// 回收被误判成超时（2026-11-05 实测：6 个用例因此假红）。排队本身给 3 分钟
		// 上限：槽位若被泄漏，失败要落在这一次清理上，而不是把整包挂到全局超时。
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 3*time.Minute)
		release, err := pgtemp.AcquireDropSlot(waitCtx)
		cancelWait()
		if err != nil {
			t.Errorf("获取回收槽位 %s: %v", schema.Name, err)
			return
		}
		defer release()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := schema.Drop(cleanupCtx); err != nil {
			t.Errorf("删除隔离 schema %s: %v", schema.Name, err)
		}
	})
	return schema.DSN
}

// checkTestDSN 是 `pgtemp.CheckTestDSN` 的本包别名：守卫用例 `TestPGTest_RejectsNonTestDatabase`
// 在 T018b 就已按这个函数名登记，改名会让已登记的判据找不到被测面。
// checkTestDSN aliases pgtemp.CheckTestDSN for the guard case registered as
// TestPGTest_RejectsNonTestDatabase.
func checkTestDSN(dsn, allow string) error { return pgtemp.CheckTestDSN(dsn, allow) }

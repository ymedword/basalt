package app

import (
	"context"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// 本文件把 PostgreSQL 谱系接到命令面的迁移端口（`cli.DatabaseMigrator`）。适配器只能
// 放在装配层：端口的方法签名用 cli 自己的配置类型，实现包无法命名它（那会成为反向依赖）。
// The adapter from the PostgreSQL lineage onto the command surface's migration port.
// It can only live here: the port's signature names a cli type that the implementation
// package cannot reference without depending back on cli.

// postgresMigratorPort 是 `db migrate` 的生产适配器。无状态：每次调用自建短命连接池
// 并释放——迁移是一次性运维动作，不参与运行期的重路径装配（那条路径由 serve 自己打开）。
// The production adapter behind `db migrate`, stateless: the pool lives for one call
// and never becomes a second runtime handle.
type postgresMigratorPort struct{}

// Migrate 打开配置的 PostgreSQL 并把它带到当前 schema 版本。复用 `postgres.Open`：
// 「连接 → 扩展能力 → 版本化迁移」的次序只有一处实现，`db migrate` 与 `serve` 的启动
// 校验因此不会各自长出"能不能启动"的第二套判断。
// Migrate reuses postgres.Open so `db migrate` and serve's startup gate share one
// implementation of connect → extensions → versioned migration.
func (postgresMigratorPort) Migrate(ctx context.Context, database cli.DatabaseConfig) (schemaVersion int, err error) {
	db, err := postgres.Open(ctx, postgresConfig(database))
	if err != nil {
		return 0, err
	}
	defer func() {
		// 关闭失败只在成功路径上改判（失败的错误更有价值，不覆盖）。
		if closeErr := db.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("释放 PostgreSQL 连接池: %w", closeErr)
		}
	}()
	return db.SchemaVersion(ctx)
}

// databaseConfig 取实例配置里的数据库段；配置缺失（`basalt.yaml` 不存在）时给零值——
// 空 DSN 会让写路径与运行期装配各自 fail-closed，**不**回退 SQLite（D-01）。
// databaseConfig projects the database section; a missing config yields the zero value
// so every write path fails closed instead of falling back to SQLite.
func databaseConfig(cfg *cli.Config) cli.DatabaseConfig {
	if cfg == nil {
		return cli.DatabaseConfig{}
	}
	return cfg.Database
}

// postgresConfig 把配置段投影成后端参数：秒换算成 time.Duration（0 交后端取缺省）。
// 单独成函数是为了能在**无库**用例里逐项断言——超时漏乘 time.Second 会把
// `statement_timeout` 变成 0 毫秒（等于不设限），而它只在真实慢查询下才暴露。
// The projection is split out so the unit test can assert it without a database: a
// missing time.Second would turn statement_timeout into 0ms, i.e. no limit at all.
func postgresConfig(database cli.DatabaseConfig) postgres.Config {
	return postgres.Config{
		DSN:              database.DSN,
		MaxConns:         database.MaxConns,
		MinConns:         database.MinConns,
		ConnectTimeout:   secondsDuration(database.ConnectTimeoutSeconds),
		StatementTimeout: secondsDuration(database.StatementTimeoutSeconds),
		LockTimeout:      secondsDuration(database.LockTimeoutSeconds),
	}
}

// runtimePostgresConfig 为启用有界 Flow 的共享池补齐有限上限与连接复用。
// runtimePostgresConfig bounds and reuses the existing pool for opted-in Flows.
func runtimePostgresConfig(cfg *cli.Config) postgres.Config {
	out := postgresConfig(databaseConfig(cfg))
	if cfg == nil || !cfg.Flows.Enabled {
		return out
	}
	if out.MaxConns == 0 {
		out.MaxConns = 32
	}
	if out.MinConns == 0 {
		// database/sql 的默认2条空闲连接会在多Worker边界反复建连／关闭。
		// Reuse the bounded peak rather than exhausting ephemeral ports with churn.
		out.MinConns = out.MaxConns
	}
	return out
}

// secondsDuration 把秒数换算成时长；非正值一律归零——装载层已拒绝负数，这里再收一次
// 口：负的会话参数没有意义，而 0 的语义是"取后端缺省"。
// Non-positive seconds collapse to zero, which means "use the backend default".
func secondsDuration(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

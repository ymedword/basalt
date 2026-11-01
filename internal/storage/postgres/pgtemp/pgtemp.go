// Package pgtemp 在一次性测试库上建 / 删**隔离 schema**，并给出把会话 `search_path`
// 钉在该 schema 上的连接串。两个调用方共用它：集成用例的 `pgtest`（t.Cleanup 回收）
// 与评测运行器（逐用例建、收尾删）。抽成非测试包是为了让评测运行器也用**同一份**
// "不得指向既有实例库"守卫——复制一份守卫就会漂移。
// Package pgtemp provisions throwaway PostgreSQL schemas and returns a DSN whose
// session search_path is pinned to one. Both the integration-test helper pgtest and
// the evals runner share it so the "never target an existing instance database"
// guard exists exactly once.
package pgtemp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	// pgx 的 stdlib 模式在这里注册 `pgx` 驱动：本包自己开连接池，不经 postgres 包的
	// 生产连接代码（那一条会跑锁与迁移，隔离 schema 的准备阶段不需要、也不该触发）。
	// The pgx driver registers itself through this blank import; provisioning opens its
	// own pool instead of the production connection path.
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	// TestDSNEnv 是测试库连接串的**唯一**来源；缺省即失败（D-01），不跳过。
	// TestDSNEnv is the only source of the test DSN; its absence fails the caller.
	TestDSNEnv = "BASALT_TEST_PG_DSN"

	// allowEnv 显式放行非 `basalt_test` 数据库；只有人明确设置时才生效。
	// allowEnv explicitly permits a non-basalt_test database.
	allowEnv = "BASALT_TEST_PG_ALLOW"

	// requiredDatabase 是测试库的固定名（守卫：不得把既有 Basalt 实例库当测试库）。
	// requiredDatabase is the fixed test database name.
	requiredDatabase = "basalt_test"
)

// identifierPattern 限定 schema 前缀的合法形态：生成出来的 schema 名会直接进
// `CREATE SCHEMA` / `DROP SCHEMA` 语句，前缀由调用方给，必须先把注入面关掉。
// identifierPattern constrains the caller-supplied prefix, which ends up in DDL.
var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Schema 是一次性隔离 schema 的句柄：`Name` 供残留识别与回收，`DSN` 交给被测装配。
// Schema is one throwaway schema: the name for cleanup, the DSN for the assembly
// under test.
type Schema struct {
	// Name 是 schema 名（调用方前缀 + 16 位随机十六进制后缀）。
	// Name is the prefix plus 16 random hex characters.
	Name string

	// DSN 已把会话 `search_path` 钉在隔离 schema（加扩展所在 schema）上——它**含
	// 测试库口令**，只能经内存配置或进程环境传递，**不得**写进任何文件。
	// The DSN pins search_path to the isolated schema and carries the test password:
	// hand it over in memory or through the environment, never through a file.
	DSN string
}

// New 在测试库上建一个隔离 schema 并返回钉住 `search_path` 的 DSN。前缀用于并行 /
// 残留识别（如 `basalt_pgtest_` / `basalt_evals_`）；回收由调用方负责（`Drop`）。
// New creates one isolated schema and returns its pinned DSN; the caller owns cleanup.
func New(ctx context.Context, prefix string) (Schema, error) {
	if !identifierPattern.MatchString(prefix) {
		return Schema{}, fmt.Errorf("schema 前缀 %q 不合法：只允许小写字母开头的 [a-z0-9_]", prefix)
	}
	base := os.Getenv(TestDSNEnv)
	if base == "" {
		return Schema{}, fmt.Errorf("未设置 %s——需要真实 PostgreSQL（D-01：缺省即失败，不跳过）。"+
			"本地：启动 dev-env 测试库并经 dev-env/scripts/Enter-DevEnv.ps1 注入；"+
			"CI：使用 postgres service 容器注入的 DSN（.github/workflows/ci.yml）", TestDSNEnv)
	}
	if err := CheckTestDSN(base, os.Getenv(allowEnv)); err != nil {
		return Schema{}, err
	}

	pool, err := sql.Open("pgx", base)
	if err != nil {
		return Schema{}, fmt.Errorf("打开测试连接池: %w", err)
	}
	if err := pool.PingContext(ctx); err != nil {
		return Schema{}, errors.Join(fmt.Errorf("测试库不可达: %w", err), pool.Close())
	}

	suffix, err := randomSuffix()
	if err != nil {
		return Schema{}, errors.Join(err, pool.Close())
	}
	name := prefix + suffix
	if _, err := pool.ExecContext(ctx, `CREATE SCHEMA "`+name+`"`); err != nil {
		return Schema{}, errors.Join(fmt.Errorf("建隔离 schema %s: %w", name, err), pool.Close())
	}

	paths := []string{name}
	seen := map[string]bool{name: true}
	for _, ext := range []string{"vector", "pg_trgm"} {
		extSchema, err := extensionSchema(ctx, pool, ext)
		if err != nil {
			// 扩展读不出来时把已建的 schema 一并收掉——准备阶段失败不留残留。
			// Drop the just-created schema when preparation fails, so nothing leaks.
			cleanupErr := dropSchema(ctx, pool, name)
			return Schema{}, errors.Join(fmt.Errorf("读取扩展 %s 所在 schema: %w", ext, err),
				cleanupErr, pool.Close())
		}
		if seen[extSchema] {
			continue
		}
		seen[extSchema] = true
		paths = append(paths, extSchema)
	}
	if err := pool.Close(); err != nil {
		return Schema{}, fmt.Errorf("关闭准备连接池: %w", err)
	}

	return Schema{Name: name, DSN: withSearchPath(base, paths...)}, nil
}

// Drop 删除该 schema（连带其中全部对象）；连接池按调用传进 ctx 的期限收敛。
// Drop removes the schema and everything in it.
func (s Schema) Drop(ctx context.Context) error {
	if s.Name == "" {
		return errors.New("Schema.Drop: 空 schema 名（未被 New 赋值的零值不可回收）")
	}
	base := os.Getenv(TestDSNEnv)
	if base == "" {
		return fmt.Errorf("未设置 %s——回收隔离 schema %s 需要同一个测试库", TestDSNEnv, s.Name)
	}
	pool, err := sql.Open("pgx", base)
	if err != nil {
		return fmt.Errorf("打开回收连接池: %w", err)
	}
	return errors.Join(dropSchema(ctx, pool, s.Name), pool.Close())
}

// dropSchema 执行 `DROP SCHEMA ... CASCADE`；名字由本包生成（受 identifierPattern
// 约束的前缀 + 十六进制后缀），这里仍加引号，避免日后前缀放宽时留下注入面。
// dropSchema runs the DROP; the name is generated here and still quoted.
func dropSchema(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, name string,
) error {
	if _, err := q.ExecContext(ctx, `DROP SCHEMA "`+name+`" CASCADE`); err != nil {
		return fmt.Errorf("删除隔离 schema %s: %w", name, err)
	}
	return nil
}

// CheckTestDSN 是"不得指向既有 Basalt 实例库"的守卫：库名必须是 `basalt_test` 或以
// `basalt_test_` 开头，否则拒绝；只有显式设置 `BASALT_TEST_PG_ALLOW` 才放行其他库名。
// CheckTestDSN refuses any database that is not the disposable test database; an
// explicit BASALT_TEST_PG_ALLOW is the only way past it.
func CheckTestDSN(dsn, allow string) error {
	if allow != "" {
		return nil
	}
	name, err := databaseName(dsn)
	if err != nil {
		return err
	}
	if name == requiredDatabase || strings.HasPrefix(name, requiredDatabase+"_") {
		return nil
	}
	return fmt.Errorf("DSN 指向的数据库 %q 不是一次性测试库（必须是 %q 或以 %q 开头）；"+
		"确实要用其他库请显式设置 %s", name, requiredDatabase, requiredDatabase+"_", allowEnv)
}

// databaseName 同时支持 URL 与 keyword=value 两种 DSN 写法。
// databaseName accepts both the URL and the keyword=value DSN forms.
func databaseName(dsn string) (string, error) {
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("解析 %s: %w", TestDSNEnv, err)
		}
		name := strings.TrimPrefix(u.Path, "/")
		if name == "" {
			return "", fmt.Errorf("%s 缺少数据库名", TestDSNEnv)
		}
		return name, nil
	}
	for _, field := range strings.Fields(dsn) {
		if value, ok := strings.CutPrefix(field, "dbname="); ok {
			return value, nil
		}
	}
	return "", fmt.Errorf("%s 缺少 dbname", TestDSNEnv)
}

// extensionSchema 读取扩展实际安装的 schema：`vector` 与 `pg_trgm` 的类型与操作符
// 都按 `search_path` 解析，隔离 schema 必须把它们显式带进 search_path。
// extensionSchema returns where an extension is installed so the isolated search_path
// can still resolve its types and operators.
func extensionSchema(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, name string,
) (string, error) {
	var schema string
	err := q.QueryRowContext(ctx, `
		SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname = $1`, name).Scan(&schema)
	if err != nil {
		return "", err
	}
	return schema, nil
}

// withSearchPath 把会话 `search_path` 钉到给定 schema 列表；pgx 把连接串里的未知键当
// 会话运行时参数下发。
// withSearchPath pins the session search_path through a runtime parameter.
func withSearchPath(dsn string, paths ...string) string {
	joined := strings.Join(paths, ",")
	if !strings.Contains(dsn, "://") {
		return dsn + " search_path=" + joined
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	q := u.Query()
	q.Set("search_path", joined)
	u.RawQuery = q.Encode()
	return u.String()
}

// randomSuffix 产生 16 位十六进制后缀；`crypto/rand` 保证并行调用不撞名。失败向上
// 返回而不是 panic：准备阶段没有"继续跑"的合理默认动作。
// randomSuffix yields 16 hex characters so parallel schemas never collide; a failure
// is returned rather than panicked.
func randomSuffix() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成 schema 随机后缀: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

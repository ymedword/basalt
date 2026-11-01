//go:build integration

package pgtemp

// 本文件是 `pgtemp` 的包级判卷（T138：有可执行语句但无测试文件的包计 0%）。
// 它判四件事：
//
//   - `New` 真的建出隔离 schema，并从**另一个**连接看回去：表只在该 schema 里；
//   - `Drop` 回收后 schema 不复存在，重复回收报错而不是静默成功；
//   - `CheckTestDSN` 的守卫：只有 `basalt_test` / `basalt_test_*` 放行，显式
//     `BASALT_TEST_PG_ALLOW` 是唯一例外；两种 DSN 写法都解析；
//   - 前缀合法性与缺 DSN 的失败路径：准备阶段失败要报错，不得 panic 或继续。
//
// 全部用例都要真实 PostgreSQL（D-01：缺 DSN 即失败，不跳过），因此带
// `//go:build integration` —— 与 `make cover` 的判卷口径一致。
// The cases exercise the real test database, so they carry the integration tag.

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	// pgx 的 stdlib 模式在测试里也自己注册驱动：回收句柄直接用包外连接，
	// 不走被测的 New / Drop 内部的打开逻辑（避免"自己验证自己"）。
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// openBase 打开测试库的基连接（不经 `New`），供"从外部看 schema 是否存在"使用。
// openBase opens the base test database without going through New.
func openBase(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv(TestDSNEnv)
	require.NotEmpty(t, dsn, "集成用例需要 %s（D-01：缺省即失败，不跳过）", TestDSNEnv)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	require.NoError(t, db.Ping())
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// schemaExists 从基连接读 information_schema：判 schema 是否真的存在。
// schemaExists reads information_schema through the base connection.
func schemaExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var exists bool
	require.NoError(t, db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		name).Scan(&exists))
	return exists
}

// TestPgtemp_NewAndDropIsolatesSchema 判建与删的闭环：新 schema 从基连接可见、
// 钉住的 DSN 让 `current_schema()` 指向它、表建在其中而基连接看不到；Drop 后消失。
//
// 逆改点：把 `withSearchPath` 改成不钉 search_path → `current_schema()` 断言红；
// 把 `Drop` 改成只关连接不删 schema → 回收后的存在性断言红；把 Drop 改成静默
// 幂等 → 第二次回收的报错断言红。
// Reverting the search_path pin or the DROP turns exactly one assertion red.
func TestPgtemp_NewAndDropIsolatesSchema(t *testing.T) {
	ctx := context.Background()
	base := openBase(t)

	schema, err := New(ctx, "basalt_pgtemp_ut")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(schema.Name, "basalt_pgtemp_ut"),
		"schema 名必须带调用方前缀（并行与残留识别依赖它）")
	require.Len(t, schema.Name, len("basalt_pgtemp_ut")+16, "后缀固定 16 位十六进制")
	require.NotEmpty(t, schema.DSN)
	dropped := false
	t.Cleanup(func() {
		// 用例中途失败时也不留残留；已经显式回收过就不再删第二次。
		if dropped {
			return
		}
		if dropErr := schema.Drop(context.Background()); dropErr != nil {
			t.Errorf("回收隔离 schema: %v", dropErr)
		}
	})
	require.True(t, schemaExists(t, base, schema.Name), "New 必须真的建出 schema")

	// 钉住的 DSN 走自己的一条连接：session 的 current_schema 必须就是隔离 schema。
	isolated, err := sql.Open("pgx", schema.DSN)
	require.NoError(t, err)
	require.NoError(t, isolated.PingContext(ctx))
	var current string
	require.NoError(t, isolated.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&current))
	require.Equal(t, schema.Name, current, "DSN 必须把 search_path 钉在隔离 schema 上")

	// 隔离性：表建在隔离 schema 里；基连接以缺省 search_path 看不见它。
	_, err = isolated.ExecContext(ctx, `CREATE TABLE isolation_probe (x integer)`)
	require.NoError(t, err)
	_, err = base.ExecContext(ctx, `SELECT 1 FROM isolation_probe`)
	require.Error(t, err, "隔离 schema 里的表不得出现在基连接的缺省 search_path 上")
	require.NoError(t, isolated.Close())

	require.NoError(t, schema.Drop(ctx))
	dropped = true
	require.False(t, schemaExists(t, base, schema.Name), "Drop 必须真的删除 schema")
	require.Error(t, schema.Drop(ctx), "重复回收必须报错，不得静默成功")
}

// TestPgtemp_RejectsInvalidPrefix 判前缀合法性：前缀会直接拼进 DDL，非法形态在
// 触库之前就必须被拒绝（不建 schema、不打开连接）。
// Invalid prefixes are rejected before any DDL runs.
func TestPgtemp_RejectsInvalidPrefix(t *testing.T) {
	ctx := context.Background()

	for _, prefix := range []string{"", "Upper", "9lead", "has-dash", "has space", "with.dot"} {
		prefix := prefix
		t.Run("prefix="+prefix, func(t *testing.T) {
			_, err := New(ctx, prefix)
			require.Error(t, err, "非法前缀 %q 必须在准备阶段被拒绝", prefix)
		})
	}
}

// TestPgtemp_RequiresDSN 判 D-01：缺 `BASALT_TEST_PG_DSN` 时 New 与 Drop 都报错，
// 不跳过、不回退到缺省连接。
// A missing DSN fails both entry points instead of skipping.
func TestPgtemp_RequiresDSN(t *testing.T) {
	ctx := context.Background()
	t.Setenv(TestDSNEnv, "")

	_, err := New(ctx, "basalt_pgtemp_ut")
	require.Error(t, err)
	require.Contains(t, err.Error(), TestDSNEnv, "错误文案必须点名缺失的环境变量")

	require.Error(t, Schema{Name: "basalt_pgtemp_ut_x"}.Drop(ctx))
}

// TestPgtemp_DropZeroValue 判零值不可回收：未经 New 赋名的 Schema 删不到任何东西，
// 必须报错而不是把空串拼进 DROP。
// The zero value has no schema to drop and must fail loudly.
func TestPgtemp_DropZeroValue(t *testing.T) {
	t.Setenv(TestDSNEnv, "host=127.0.0.1 dbname=basalt_test")
	require.Error(t, Schema{}.Drop(context.Background()))
}

// TestPgtemp_CheckTestDSN 判"不得指向既有实例库"的守卫：只放行 basalt_test 与
// basalt_test_*，显式 allow 是唯一例外；URL 与 keyword=value 两种写法都要解析。
// The guard admits only the disposable test database unless explicitly allowed.
func TestPgtemp_CheckTestDSN(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		allow   string
		wantErr bool
	}{
		{"url exact", "postgres://u:p@127.0.0.1:5432/basalt_test?sslmode=disable", "", false},
		{"url prefix", "postgres://u:p@127.0.0.1:5432/basalt_test_alpha", "", false},
		{"url other db", "postgres://u:p@127.0.0.1:5432/basalt", "", true},
		{"url other db allowed", "postgres://u:p@127.0.0.1:5432/basalt", "1", false},
		{"url missing db", "postgres://u:p@127.0.0.1:5432/", "", true},
		{"url malformed", "postgres://%zz@127.0.0.1/basalt_test", "", true},
		{"keyword exact", "host=127.0.0.1 dbname=basalt_test user=u", "", false},
		{"keyword other", "host=127.0.0.1 dbname=basalt user=u", "", true},
		{"keyword missing dbname", "host=127.0.0.1 user=u", "", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := CheckTestDSN(tc.dsn, tc.allow)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestPgtemp_NewRefusesForeignDatabase 判 New 在触库前先过守卫：DSN 指向非测试库
// 且没有显式 allow 时，错误必须来自守卫，而不是连接失败——否则守卫可以被"连不上"
// 掩盖成"拒绝"。
// New must fail on the guard itself, not on a connection error.
func TestPgtemp_NewRefusesForeignDatabase(t *testing.T) {
	t.Setenv(TestDSNEnv, "postgres://u:p@127.0.0.1:1/definitely_not_test")
	t.Setenv(allowEnv, "")

	_, err := New(context.Background(), "basalt_pgtemp_ut")
	require.Error(t, err)
	require.Contains(t, err.Error(), "不是一次性测试库", "拒绝必须来自守卫")
}

// TestPgtemp_WithSearchPath 判两种 DSN 写法都被钉上 search_path：URL 走查询参数，
// keyword 走追加的运行时参数。
// Both DSN forms get the search_path pinned.
func TestPgtemp_WithSearchPath(t *testing.T) {
	pinned := withSearchPath("host=127.0.0.1 dbname=basalt_test", "s1", "public")
	require.Contains(t, pinned, "search_path=s1,public")

	urlDSN := withSearchPath("postgres://h:5432/basalt_test?sslmode=disable", "s1", "public")
	parsed, err := url.Parse(urlDSN)
	require.NoError(t, err)
	require.Equal(t, "s1,public", parsed.Query().Get("search_path"))
	require.Equal(t, "disable", parsed.Query().Get("sslmode"), "原有查询参数不得丢失")
}

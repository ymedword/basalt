package app

// 本文件是 PostgreSQL 迁移端口投影的**无库**验收（T124）：秒 → 时长的换算与
// "空 DSN 就地拒绝"两条都不需要真实数据库——有库路径归 `internal/storage/postgres`
// 的集成用例（`us16-pg` / `us16-pg-migrate`）。
// The database-free half of the migration port: the seconds projection and the
// empty-DSN refusal. Everything that needs a live server belongs to the postgres
// package's integration checks.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

// TestPostgresConfig_MapsDatabaseSection 验收点：database 段逐字段投影到后端参数，
// 三个超时**乘上 time.Second**（漏乘会变成毫秒级 0 = 不设限，只在真实慢查询下暴露）。
// 逆改点：任一字段漏传或超时按毫秒传递 → 断言红。
func TestPostgresConfig_MapsDatabaseSection(t *testing.T) {
	got := postgresConfig(cli.DatabaseConfig{
		DSN:                     "postgres://basalt@db.example.com:5432/basalt",
		MaxConns:                8,
		MinConns:                2,
		ConnectTimeoutSeconds:   5,
		StatementTimeoutSeconds: 30,
		LockTimeoutSeconds:      3,
	})

	require.Equal(t, "postgres://basalt@db.example.com:5432/basalt", got.DSN)
	require.Equal(t, 8, got.MaxConns)
	require.Equal(t, 2, got.MinConns)
	require.Equal(t, 5*time.Second, got.ConnectTimeout)
	require.Equal(t, 30*time.Second, got.StatementTimeout)
	require.Equal(t, 3*time.Second, got.LockTimeout)

	// 0 的语义是"交后端取缺省"，负值同样不得变成负的会话参数（装载层已拒绝，这里再收口）。
	zero := postgresConfig(cli.DatabaseConfig{})
	require.Zero(t, zero.ConnectTimeout)
	require.Zero(t, zero.StatementTimeout)
	require.Zero(t, zero.LockTimeout)
	require.Zero(t, secondsDuration(-1), "负秒数必须归零而不是变成负时长")
}

func TestFlow_RuntimePostgresPoolIsBoundedAndReusesConnections(t *testing.T) {
	cfg := &cli.Config{}
	ordinary := runtimePostgresConfig(cfg)
	require.Zero(t, ordinary.MaxConns)
	require.Zero(t, ordinary.MinConns)
	cfg.Flows.Enabled = true
	defaults := runtimePostgresConfig(cfg)
	require.Equal(t, 32, defaults.MaxConns)
	require.Equal(t, 32, defaults.MinConns)
	cfg.Database.MaxConns = 8
	bounded := runtimePostgresConfig(cfg)
	require.Equal(t, 8, bounded.MaxConns)
	require.Equal(t, 8, bounded.MinConns, "idle ceiling must respect the explicit open ceiling")
	cfg.Database.MinConns = 2
	explicit := runtimePostgresConfig(cfg)
	require.Equal(t, 8, explicit.MaxConns)
	require.Equal(t, 2, explicit.MinConns, "explicit deployment choices remain authoritative")
}

// TestPostgresMigratorPort_EmptyDSNFailsClosed 验收点：空 DSN 在**拨号之前**就被拒绝
// （含残留占位的空值形态），不会退化成一个连向本机缺省端口的尝试。
// 逆改点：去掉空值判定或先拨号 → 用例要么拿到连接错误而不是"连接串为空"，要么超时。
func TestPostgresMigratorPort_EmptyDSNFailsClosed(t *testing.T) {
	_, err := postgresMigratorPort{}.Migrate(context.Background(), cli.DatabaseConfig{})
	require.Error(t, err, "空 DSN 必须 fail-closed")
	require.Contains(t, err.Error(), "连接串为空")
}
